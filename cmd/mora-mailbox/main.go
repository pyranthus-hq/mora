// mora-mailbox keeps Mora-project tasks separate from durable memory.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

const scope = "project:mora"
const protocol = "mora-mailbox/v1"

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,63}$`)
var jobRE = regexp.MustCompile(`^job_[a-f0-9]{32}$`)
var messageRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

type Settings struct {
	Version  int    `json:"version"`
	To       string `json:"to"`
	Endpoint string `json:"endpoint"`
	Session  string `json:"session"`
	CWD      string `json:"cwd"`
	Socket   string `json:"socket"`
}
type Job struct {
	ID         string `json:"id"`
	Scope      string `json:"scope"`
	To         string `json:"to"`
	Request    string `json:"request"`
	State      string `json:"state"`
	CreatedAt  string `json:"created_at"`
	DispatchAt string `json:"dispatch_at,omitempty"`
	MessageID  string `json:"message_id,omitempty"`
	Result     string `json:"result,omitempty"`
}
type Reply struct {
	Protocol string `json:"protocol"`
	JobID    string `json:"job_id"`
	Scope    string `json:"scope"`
	State    string `json:"state"`
	Result   string `json:"result"`
}
type Message struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	ReplyTo string `json:"reply_to"`
	Body    string `json:"body"`
}
type Peer struct {
	Person   string `json:"person"`
	Machine  string `json:"machine"`
	Endpoint string `json:"endpoint"`
	Online   bool   `json:"online"`
}
type Session struct {
	Person  string `json:"person"`
	Machine string `json:"machine"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Doing   string `json:"doing"`
}
type Directory struct {
	Person   string    `json:"person"`
	Machine  string    `json:"machine"`
	Machines []Peer    `json:"machines"`
	Sessions []Session `json:"sessions"`
}
type callFn func(map[string]any, any) error

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mora-mailbox:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("mora-mailbox", flag.ContinueOnError)
	root := flags.String("root", filepath.Join(cfg.StateDir, "tentacle-mailbox", "grok"), "private mailbox state directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	args = flags.Args()
	if len(args) == 0 {
		return errors.New("usage: mora-mailbox [--root DIR] init|enqueue|poll|status|show")
	}
	if !filepath.IsAbs(*root) {
		return errors.New("root must be absolute")
	}
	if args[0] == "init" {
		return initialize(*root, args[1:])
	}
	var s Settings
	if err := readJSON(filepath.Join(*root, "config.json"), &s); err != nil {
		return err
	}
	if err := s.validate(); err != nil {
		return err
	}
	switch args[0] {
	case "enqueue":
		if len(args) != 2 {
			return errors.New("enqueue requires one UTF-8 task file")
		}
		body, err := readRegular(args[1], 16384)
		if err != nil {
			return err
		}
		if err := validText(string(body), 16384); err != nil {
			return err
		}
		return leasefile.WithGuard(filepath.Join(*root, "queue"), func() error {
			id := make([]byte, 16)
			if _, err := rand.Read(id); err != nil {
				return err
			}
			j := Job{ID: "job_" + hex.EncodeToString(id), Scope: scope, To: s.To, Request: string(body), State: "queued", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
			if err := saveJSON(jobPath(*root, j.ID), j); err != nil {
				return err
			}
			fmt.Println(j.ID)
			return nil
		})
	case "poll":
		if len(args) != 1 {
			return errors.New("poll takes no arguments")
		}
		return leasefile.WithGuard(filepath.Join(*root, "queue"), func() error {
			client, err := connect(s.Socket)
			status := map[string]any{"checked_at": time.Now().UTC().Format(time.RFC3339), "state": "failed"}
			var detail string
			if err == nil {
				defer client.conn.Close()
				detail, err = poll(*root, s, client.call)
			}
			if err == nil {
				status["state"] = "ok"
				status["detail"] = detail
			} else {
				status["detail"] = err.Error()
			}
			return errors.Join(err, saveJSON(filepath.Join(*root, "status.json"), status))
		})
	case "status":
		jobs, err := loadJobs(*root, s)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			fmt.Printf("%s  %-12s %s\n", j.ID, j.State, j.MessageID)
		}
		var st map[string]any
		if err := readJSON(filepath.Join(*root, "status.json"), &st); err == nil {
			return json.NewEncoder(os.Stdout).Encode(st)
		}
		return nil
	case "show":
		if len(args) != 2 || !jobRE.MatchString(args[1]) {
			return errors.New("show requires a job ID")
		}
		var j Job
		if err := readJSON(jobPath(*root, args[1]), &j); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(j)
	default:
		return errors.New("unknown command")
	}
}
func (s Settings) validate() error {
	parts := strings.Split(s.To, "@")
	if len(parts) != 2 {
		return errors.New("recipient must be person/session@machine")
	}
	who := strings.Split(parts[0], "/")
	if len(who) != 2 || !nameRE.MatchString(who[0]) || !nameRE.MatchString(who[1]) || !nameRE.MatchString(parts[1]) {
		return errors.New("invalid exact recipient")
	}
	if s.Version != 1 || !nameRE.MatchString(s.Session) || !filepath.IsAbs(s.CWD) || !filepath.IsAbs(s.Socket) {
		return errors.New("invalid mailbox configuration")
	}
	b, err := hex.DecodeString(s.Endpoint)
	if err != nil || len(b) != 32 {
		return errors.New("recipient endpoint must be pinned")
	}
	return nil
}
func initialize(root string, args []string) error {
	f := flag.NewFlagSet("init", flag.ContinueOnError)
	to := f.String("to", "", "exact Reason recipient")
	session := f.String("session", "mora-mailbox", "local mailbox name")
	cwd := f.String("cwd", "", "Mora project checkout")
	h, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sock := f.String("socket", filepath.Join(h, ".reason/network/network.sock"), "existing local Reason socket")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected init arguments")
	}
	if _, err := os.Lstat(filepath.Join(root, "config.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("mailbox configuration already exists or cannot be inspected")
	}
	s := Settings{Version: 1, To: *to, Session: *session, CWD: *cwd, Socket: *sock, Endpoint: strings.Repeat("0", 64)}
	if err := s.validate(); err != nil {
		return err
	}
	c, err := connect(s.Socket)
	if err != nil {
		return err
	}
	defer c.conn.Close()
	var d Directory
	if err := c.call(map[string]any{"op": "who", "all": true}, &d); err != nil {
		return err
	}
	person, _, machine := recipient(s.To)
	found := false
	for _, p := range d.Machines {
		if p.Person == person && p.Machine == machine {
			s.Endpoint = p.Endpoint
			found = true
		}
	}
	if !found {
		return errors.New("recipient machine is not in the existing Reason team")
	}
	if err := s.validate(); err != nil {
		return err
	}
	for _, dir := range []string{root, filepath.Join(root, "inbox"), filepath.Join(root, "outbox")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if err := atomicio.SyncDirFn(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	if err := saveJSON(filepath.Join(root, "config.json"), s); err != nil {
		return err
	}
	fmt.Println("Mailbox initialized for", s.To, "in", scope)
	return nil
}
func recipient(to string) (string, string, string) {
	a := strings.Split(to, "@")
	b := strings.Split(a[0], "/")
	return b[0], b[1], a[1]
}
func jobPath(root, id string) string { return filepath.Join(root, "inbox", id+".json") }
func validText(s string, max int) error {
	if !utf8.ValidString(s) || strings.TrimSpace(s) == "" || len(s) > max {
		return errors.New("text empty or oversized")
	}
	for _, r := range s {
		if r < 32 && r != '\n' && r != '\t' {
			return errors.New("control characters in text")
		}
	}
	return nil
}
func saveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(path, append(b, '\n'), 0600)
}
func readRegular(path string, max int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > max {
		return nil, errors.New("expected bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if len(b) > int(max) {
		return nil, errors.New("file exceeds limit")
	}
	return b, err
}
func readJSON(path string, v any) error {
	return readJSONLimit(path, v, 1<<20)
}
func readJSONLimit(path string, v any, max int64) error {
	b, err := readRegular(path, max)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func loadJobs(root string, s Settings) ([]Job, error) {
	entries, err := os.ReadDir(filepath.Join(root, "inbox"))
	if errors.Is(err, os.ErrNotExist) {
		return []Job{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > 1024 {
		return nil, errors.New("pilot queue limit exceeded")
	}
	jobs := []Job{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var j Job
		if err := readJSON(filepath.Join(root, "inbox", e.Name()), &j); err != nil {
			return nil, err
		}
		if !jobRE.MatchString(j.ID) || e.Name() != j.ID+".json" || j.Scope != scope || j.To != s.To {
			return nil, errors.New("job scope/identity conflict")
		}
		if err := validText(j.Request, 16384); err != nil {
			return nil, err
		}
		switch j.State {
		case "queued", "dispatching", "submitted", "working", "done", "failed":
		default:
			return nil, errors.New("invalid job state")
		}
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt == jobs[j].CreatedAt {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].CreatedAt < jobs[j].CreatedAt
	})
	return jobs, nil
}

// shortcut: pinned to the inspected Reason Network 0.12.17 local socket API; revalidate before upgrading it.
type client struct {
	conn   net.Conn
	reader *bufio.Reader
	id     int
}

func connect(path string) (*client, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("Reason socket is not a socket")
	}
	c, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, err
	}
	return &client{conn: c, reader: bufio.NewReader(c)}, nil
}
func (c *client) call(req map[string]any, out any) error {
	c.id++
	req["id"] = c.id
	if err := c.conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return err
	}
	if err := json.NewEncoder(c.conn).Encode(req); err != nil {
		return err
	}
	line, err := readLine(c.reader, 4<<20)
	if err != nil {
		return err
	}
	var r struct {
		ID     int             `json:"id"`
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return err
	}
	if r.ID != c.id {
		return errors.New("unexpected Reason reply")
	}
	if !r.OK {
		return fmt.Errorf("Reason Network: %s", r.Error)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		p, more, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(out)+len(p) > max {
			return nil, errors.New("Reason response too large")
		}
		out = append(out, p...)
		if !more {
			return out, nil
		}
	}
}

func poll(root string, s Settings, call callFn) (string, error) {
	key := "cli:" + s.Session
	var registered struct {
		Address string `json:"address"`
	}
	if err := call(map[string]any{"op": "hook", "agent": "cli", "event": "stop", "session_id": s.Session, "cwd": s.CWD, "mail": false, "keep": true, "name": s.Session}, &registered); err != nil {
		return "", err
	}
	var d Directory
	if err := call(map[string]any{"op": "who", "all": true}, &d); err != nil {
		return "", err
	}
	if registered.Address != d.Person+"/"+s.Session {
		return "", errors.New("local mailbox registration address conflict")
	}
	person, name, machine := recipient(s.To)
	pinned := false
	ready := false
	for _, p := range d.Machines {
		if p.Person == person && p.Machine == machine && p.Endpoint == s.Endpoint {
			pinned = true
		}
	}
	if !pinned {
		return "", errors.New("recipient machine pin changed or missing")
	}
	for _, sess := range d.Sessions {
		if sess.Person == person && sess.Machine == machine && sess.Name == name && (sess.Status == "idle" || sess.Status == "live") && sess.Doing == "" {
			ready = true
		}
	}
	jobs, err := loadJobs(root, s)
	if err != nil {
		return "", err
	}
	// Recover a lost send receipt before consuming replies that reference it.
	for _, j := range jobs {
		if j.State == "dispatching" {
			if err := dispatch(root, s, d, j, call); err != nil {
				return "", err
			}
		}
	}
	var inbox struct {
		Messages []Message `json:"messages"`
	}
	if err := call(map[string]any{"op": "inbox", "key": key, "peek": true, "wait_secs": 0}, &inbox); err != nil {
		return "", err
	}
	for _, m := range inbox.Messages {
		if err := receive(root, s, m); err != nil {
			return "", err
		}
		if err := call(map[string]any{"op": "delivered", "key": key, "ids": []string{m.ID}}, nil); err != nil {
			return "", err
		}
	}
	jobs, err = loadJobs(root, s)
	if err != nil {
		return "", err
	}
	for _, j := range jobs {
		if j.State == "submitted" || j.State == "working" {
			return "Waiting for Grok result; delivery is not completion.", nil
		}
	}
	for _, j := range jobs {
		if j.State != "queued" && j.State != "dispatching" {
			continue
		}
		if !ready {
			return "Queued; waiting for the pinned Grok listener to be idle.", nil
		}
		if j.State == "queued" {
			j.State = "dispatching"
			j.DispatchAt = time.Now().UTC().Format(time.RFC3339)
			if err := saveJSON(jobPath(root, j.ID), j); err != nil {
				return "", err
			}
		}
		if err := dispatch(root, s, d, j, call); err != nil {
			return "", err
		}
		return "Submitted one Mora task; awaiting Grok acknowledgement/result.", nil
	}
	return "No queued work.", nil
}
func dispatch(root string, s Settings, d Directory, j Job, call callFn) error {
	at, err := time.Parse(time.RFC3339, j.DispatchAt)
	if err != nil || time.Since(at) > 24*time.Hour || at.After(time.Now().Add(time.Minute)) {
		return errors.New("ambiguous send is older than retry window; inspect manually")
	}
	body, _ := json.Marshal(map[string]any{"protocol": protocol, "kind": "task", "job_id": j.ID, "scope": scope, "request": j.Request, "reply_to": d.Person + "/" + s.Session + "@" + d.Machine, "instructions": "Mora project only. Read/research only; do not run unapproved side effects. Reply through Reason Network with reply_to set to this transport message ID and JSON {protocol:mora-mailbox/v1,job_id,scope:project:mora,state:working|done|failed,result}. Keep useful memory proposals separate; never auto-approve them. Deduplicate this job_id before work."})
	var sent struct {
		IDs []string `json:"ids"`
	}
	if err := call(map[string]any{"op": "send", "from": "cli:" + s.Session, "to": s.To, "body": string(body), "idempotency_key": "mora-mailbox:" + j.ID, "wake": true, "notify": false}, &sent); err != nil {
		return err
	}
	if len(sent.IDs) != 1 || !messageRE.MatchString(sent.IDs[0]) {
		return errors.New("missing single message receipt; retry keeps the same idempotency key")
	}
	j.MessageID = sent.IDs[0]
	j.State = "submitted"
	if err := saveJSON(jobPath(root, j.ID), j); err != nil {
		return err
	}
	return nil
}
func receive(root string, s Settings, m Message) error {
	if !messageRE.MatchString(m.ID) {
		return errors.New("invalid transport message ID")
	}
	// Persist before acknowledging transport. Unknown/malformed replies stay quarantined, never instructions.
	archive := filepath.Join(root, "outbox", m.ID+".json")
	var old Message
	// The socket accepts up to 4 MiB frames. Invalid replies must remain
	// replayable after a lost acknowledgement, even when larger than a job.
	if err := readJSONLimit(archive, &old, 8<<20); err == nil {
		a, _ := json.Marshal(old)
		b, _ := json.Marshal(m)
		if string(a) != string(b) {
			return errors.New("message replay changed")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := saveJSON(archive, m); err != nil {
			return err
		}
	} else {
		return err
	}
	expected := strings.Split(s.To, "@")[0]
	if m.From != expected && m.From != s.To {
		return nil
	}
	var r Reply
	if len(m.Body) > 32768 || json.Unmarshal([]byte(m.Body), &r) != nil || r.Protocol != protocol || r.Scope != scope || !jobRE.MatchString(r.JobID) {
		return nil
	}
	if r.State != "working" && r.State != "done" && r.State != "failed" {
		return nil
	}
	if validText(r.Result, 30000) != nil {
		return nil
	}
	var j Job
	if err := readJSON(jobPath(root, r.JobID), &j); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if j.ID != r.JobID || j.Scope != scope || j.To != s.To {
		return errors.New("reply job identity conflict")
	}
	if j.MessageID == "" || m.ReplyTo != j.MessageID {
		return nil
	}
	if j.State == "done" || j.State == "failed" {
		return nil
	}
	if j.State != "submitted" && j.State != "working" {
		return nil
	}
	j.State = r.State
	j.Result = r.Result
	return saveJSON(jobPath(root, j.ID), j)
}
