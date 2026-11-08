// mora-tentacles is a local operator companion, not a new vault or sync protocol.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

type Channel struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Detail      string `json:"detail"`
	LastSuccess string `json:"last_success,omitempty"`
}
type Device struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Connection string    `json:"connection"`
	Detail     string    `json:"detail"`
	Channels   []Channel `json:"channels"`
}
type Snapshot struct {
	GeneratedAt  string         `json:"generated_at"`
	Devices      []Device       `json:"devices"`
	Sources      []SourceHealth `json:"sources"`
	SourcesError string         `json:"sources_error,omitempty"`
}
type SourceHealth struct {
	Key           string `json:"key"`
	State         string `json:"state"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	Detail        string `json:"detail,omitempty"`
}
type Peer struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	SSH          string `json:"ssh"`
	HostName     string `json:"host_name,omitempty"`
	HostKeyAlias string `json:"host_key_alias,omitempty"`
	Profile      string `json:"profile"`
}
type Config struct {
	Peers []Peer `json:"peers"`
}
type Receipt struct {
	Schema      int    `json:"schema"`
	Job         string `json:"job"`
	State       string `json:"state"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	LastSuccess string `json:"last_success,omitempty"`
	Operations  int    `json:"operations"`
	Detail      string `json:"detail"`
}
type operation struct{ verb, name string }

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var safeHost = regexp.MustCompile(`^[A-Za-z0-9\[\]][A-Za-z0-9_.:@\[\]-]*$`)
var clock = func() time.Time { return time.Now().UTC() }

func stamp() string { return clock().Format(time.RFC3339) }
func home() string  { h, _ := os.UserHomeDir(); return h }
func stateRoot() string {
	if s := os.Getenv("MORA_TENTACLES_STATE_DIR"); s != "" {
		return s
	}
	return filepath.Join(home(), ".local/state/mora/tentacles")
}
func emit(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) > 0 && args[0] == "sync" {
		f := flag.NewFlagSet("sync", flag.ContinueOnError)
		j := f.String("job", "", "")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		return syncShares(*j)
	}
	if len(args) > 0 && args[0] == "probe" {
		f := flag.NewFlagSet("probe", flag.ContinueOnError)
		p := f.String("profile", "", "")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		if *p != "windows" && *p != "devbox" {
			return errors.New("invalid probe profile")
		}
		return emit(probe(*p))
	}
	f := flag.NewFlagSet("status", flag.ContinueOnError)
	p := f.String("config", filepath.Join(home(), ".config/mora-tentacles/config.json"), "")
	if e := f.Parse(args); e != nil {
		return e
	}
	b, e := os.ReadFile(*p)
	if e != nil {
		return errors.New("Tentacle configuration unavailable")
	}
	var cfg Config
	if e = json.Unmarshal(b, &cfg); e != nil {
		return errors.New("Invalid tentacle configuration")
	}
	if e = validateConfig(cfg); e != nil {
		return e
	}
	s := Snapshot{GeneratedAt: stamp(), Devices: make([]Device, len(cfg.Peers)+1)}
	s.Sources, s.SourcesError = sourceHealth()
	s.Devices[0] = probe("mac")
	var wg sync.WaitGroup
	for i, p := range cfg.Peers {
		wg.Add(1)
		go func(i int, p Peer) { defer wg.Done(); s.Devices[i+1] = remote(p) }(i, p)
	}
	wg.Wait()
	return emit(s)
}

// sourceHealth reads Mora's machine-readable doctor report without running its
// --pulse path, which advances producer state and can notify on unhealthy data.
func sourceHealth() ([]SourceHealth, string) {
	bin := os.Getenv("MORA_BIN")
	if bin == "" {
		bin = filepath.Join(home(), ".local/bin/mora")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	b, err := command(ctx, bin, "doctor", "--json")
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			if ctx.Err() != nil {
				return []SourceHealth{}, "Mora doctor source health timed out."
			}
			return []SourceHealth{}, "Mora doctor source health is unavailable."
		}
	}
	var report struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		Sources       *[]struct {
			Key           string `json:"key"`
			State         string `json:"state"`
			LastSuccessAt string `json:"last_success_at"`
			ErrorCode     string `json:"error_code"`
		} `json:"sources"`
	}
	if json.Unmarshal(b, &report) != nil || report.Schema != "mora.doctor.report" || report.SchemaVersion != 1 || report.Sources == nil {
		return []SourceHealth{}, "Mora doctor returned an invalid source health report."
	}
	sources := make([]SourceHealth, 0, len(*report.Sources))
	for _, source := range *report.Sources {
		if source.Key == "" || source.State == "" {
			return []SourceHealth{}, "Mora doctor returned an invalid source health report."
		}
		detail := source.ErrorCode
		if source.State == "failed" && detail == "" {
			detail = "Source sync reported an error."
		}
		sources = append(sources, SourceHealth{Key: source.Key, State: source.State, LastSuccessAt: source.LastSuccessAt, Detail: detail})
	}
	return sources, ""
}
func validateConfig(c Config) error {
	if len(c.Peers) > 8 {
		return errors.New("at most eight tentacles supported")
	}
	seen := map[string]bool{"mac": true}
	for _, p := range c.Peers {
		if !safeName.MatchString(p.ID) || seen[p.ID] || p.Name == "" || len(p.Name) > 80 || !safeHost.MatchString(p.SSH) || (p.HostName != "" && !safeHost.MatchString(p.HostName)) || (p.HostKeyAlias != "" && !safeHost.MatchString(p.HostKeyAlias)) || (p.Profile != "devbox" && p.Profile != "windows") {
			return errors.New("invalid or duplicate tentacle configuration")
		}
		seen[p.ID] = true
	}
	return nil
}

// Limit subprocess output, including accidental memory/log dumps, without blocking pipes.
type bounded struct {
	bytes.Buffer
	overflow bool
}

func (b *bounded) Write(p []byte) (int, error) {
	n := len(p)
	left := 1024*1024 - b.Len()
	if left < len(p) {
		b.overflow = true
		p = p[:max(0, left)]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
func command(ctx context.Context, bin string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, bin, args...)
	cancelProcessTree(c)
	c.WaitDelay = time.Second
	var out bounded
	c.Stdout = &out
	c.Stderr = io.Discard
	e := c.Run()
	if out.overflow {
		return nil, errors.New("command output exceeded limit")
	}
	return out.Bytes(), e
}
func remote(p Peer) Device {
	d := Device{ID: p.ID, Name: p.Name, Connection: "unknown", Channels: []Channel{}}
	args := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=6", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=1"}
	if p.HostName != "" {
		args = append(args, "-o", "HostName="+p.HostName)
	}
	if p.HostKeyAlias != "" {
		args = append(args, "-o", "HostKeyAlias="+p.HostKeyAlias)
	}
	args = append(args, p.SSH, "~/.local/bin/mora-tentacles probe --profile "+p.Profile)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	b, e := command(ctx, "/usr/bin/ssh", args...)
	if e != nil {
		d.Detail = "Mora status endpoint unavailable. Check the device, SSH connection and status helper."
		var x *exec.ExitError
		if ctx.Err() == nil && errors.As(e, &x) && x.ExitCode() > 0 && x.ExitCode() != 255 {
			d.Connection = "connected"
			d.Detail = "SSH answered, but the Mora status helper did not complete."
		} else {
			d.Connection = "offline"
		}
		d.Channels = []Channel{{Name: "Sync", State: "unknown", Detail: "No current sync evidence received."}}
		return d
	}
	var got Device
	if json.Unmarshal(b, &got) != nil || len(got.Channels) == 0 {
		d.Detail = "Remote status response is invalid."
		d.Channels = []Channel{{Name: "Sync", State: "unknown", Detail: "No valid sync evidence received."}}
		return d
	}
	got.ID = p.ID
	got.Name = p.Name
	got.Connection = "connected"
	got.Channels = aggregateRemoteChannels(p.Profile, got.Channels)
	return got
}
func aggregateRemoteChannels(profile string, channels []Channel) []Channel {
	if profile != "windows" {
		return channels
	}
	filtered := channels[:0]
	for _, channel := range channels {
		if channel.Name != "Windows → Mac" {
			filtered = append(filtered, channel)
		}
	}
	return filtered
}
func readReceipt(path, name string, maxAge time.Duration) Channel {
	c := Channel{Name: name, State: "unknown", Detail: "No sync receipt yet."}
	b, e := os.ReadFile(path)
	if e != nil {
		return c
	}
	var r Receipt
	if json.Unmarshal(b, &r) != nil || r.Schema != 1 {
		c.Detail = "Sync receipt is unreadable."
		return c
	}
	c.LastSuccess = r.LastSuccess
	if r.State == "running" {
		c.State = "running"
		c.Detail = "Sync started; completion has not been confirmed."
		if t, e := time.Parse(time.RFC3339, r.StartedAt); e != nil || clock().Sub(t) > 30*time.Minute || t.After(clock().Add(5*time.Minute)) {
			c.State = "failed"
			c.Detail = "Sync has not recorded completion; inspect the job before retrying."
		}
		return c
	}
	if r.State == "failed" {
		c.State = "failed"
		c.Detail = r.Detail
		return c
	}
	if r.State != "succeeded" || r.Operations < 1 || r.LastSuccess != r.FinishedAt {
		c.Detail = "Receipt does not confirm completed sync operations."
		return c
	}
	return dated(name, r.LastSuccess, maxAge, "Last scheduled push/pull succeeded. Retrieval on the other device is not verified.")
}
func dated(name, value string, maxAge time.Duration, detail string) Channel {
	c := Channel{Name: name, State: "unknown", Detail: "No valid success time."}
	t, e := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if e != nil || t.After(clock().Add(5*time.Minute)) {
		return c
	}
	c.LastSuccess = t.Format(time.RFC3339)
	c.State = "healthy"
	c.Detail = detail
	if clock().Sub(t) > maxAge {
		c.State = "stale"
		c.Detail = "Last success is overdue. " + detail
	}
	return c
}
func probe(profile string) Device {
	d := Device{ID: profile, Name: profile, Connection: "connected", Channels: []Channel{}}
	root := stateRoot()
	switch profile {
	case "mac":
		d.ID = "mac"
		d.Name = "This Mac"
		d.Detail = "Local status · checks do not trigger sync"
		d.Channels = append(d.Channels, readReceipt(filepath.Join(root, "mac-backup.json"), "Vault backup", time.Hour))
		d.Channels = append(d.Channels, botReturnChannel())
		for _, j := range []string{"mac-devbox", "mac-gateway"} {
			name := "Devbox work shares"
			if j == "mac-gateway" {
				name = "Gateway work shares"
			}
			d.Channels = append(d.Channels, readReceipt(filepath.Join(root, j+".json"), name, 90*time.Minute))
		}
	case "devbox":
		d.Detail = "Work-memory tentacle"
		d.Channels = append(d.Channels, readReceipt(filepath.Join(root, "devbox.json"), "Work shares", 45*time.Minute))
		d.Channels = append(d.Channels, service("Mora gateway", "mora-mcp-gateway.service"))
	case "windows":
		d.Detail = "WSL backend · archive and gateway are separate"
		d.Channels = append(d.Channels, replicaChannel())
		d.Channels = append(d.Channels, readReceipt(filepath.Join(root, "windows-gateway.json"), "Gateway work shares", 90*time.Minute))
		d.Channels = append(d.Channels, service("Mora gateway", "mora-gateway.service"))
		d.Channels = append(d.Channels, Channel{Name: "Windows → Mac", State: "unknown", Detail: "Authored-memory return path is not configured. Archive pulls are one-way."})
	}
	return d
}
func botReturnChannel() Channel {
	path := filepath.Join(stateRoot(), "bot-return.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return Channel{Name: "Grok → Mac proposals", State: "unknown", Detail: "No proposal return receipt yet."}
	}
	var receipt Receipt
	if json.Unmarshal(b, &receipt) != nil || receipt.Schema != 1 || receipt.Job != "bot-return" {
		return Channel{Name: "Grok → Mac proposals", State: "unknown", Detail: "Proposal return receipt is unreadable."}
	}
	c := readReceipt(path, "Grok → Mac proposals", 15*time.Minute)
	if c.State == "healthy" || c.State == "stale" {
		c.Detail = "Proposal intake completed. Notes await owner approval; this is not vault publication."
		if c.State == "stale" {
			c.Detail = "Last proposal intake is overdue. " + c.Detail
		}
	}
	return c
}
func service(name, unit string) Channel {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, e := command(ctx, "systemctl", "--user", "is-active", unit)
	if e == nil && strings.TrimSpace(string(b)) == "active" {
		return Channel{Name: name, State: "healthy", Detail: "Service is running. This is not proof of successful sync."}
	}
	return Channel{Name: name, State: "failed", Detail: "Service did not report active."}
}
func replicaChannel() Channel {
	c := Channel{Name: "Personal archive", State: "unknown", Detail: "No managed replica receipt."}
	base := filepath.Join(home(), "vault/mora-replica/current")
	resolved, e := filepath.EvalSymlinks(base)
	if e != nil {
		return c
	}
	b, e := os.ReadFile(filepath.Join(resolved, "config/config.toml"))
	if e != nil || len(bytes.TrimSpace(b)) == 0 {
		c.State = "failed"
		c.Detail = "Replica configuration is missing or empty."
		return c
	}
	// A success stamp cannot excuse a broken active bundle. This is a lightweight check, not a full content audit.
	for _, key := range []string{"vault_dir", "data_dir"} {
		sub := "vault"
		if key == "data_dir" {
			sub = "data"
		}
		want := fmt.Sprintf(`%s = "%s"`, key, filepath.Join(resolved, sub))
		if !strings.Contains(string(b), want) {
			c.State = "failed"
			c.Detail = "Replica configuration does not match its active generation."
			return c
		}
	}
	if !strings.Contains(string(b), `mcp_write_policy = "readonly"`) {
		c.State = "failed"
		c.Detail = "Replica is not configured read-only."
		return c
	}
	for _, p := range []string{"vault/.git", "data/index.db"} {
		st, e := os.Stat(filepath.Join(resolved, p))
		if e != nil || st.Size() == 0 {
			c.State = "failed"
			c.Detail = "Replica metadata or search index is missing or empty."
			return c
		}
	}
	b, e = os.ReadFile(filepath.Join(home(), ".local/state/mora-vault-replica/last-run"))
	if e != nil {
		return c
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "at=") || !strings.HasPrefix(fields[1], "status=") {
		c.Detail = "Replica receipt is malformed."
		return c
	}
	if fields[1] != "status=0" {
		c.State = "failed"
		c.Detail = "Latest archive pull failed. The previous copy may still be available."
		return c
	}
	return dated(c.Name, strings.TrimPrefix(fields[0], "at="), 2*time.Hour, "Last archive pull succeeded; active configuration and index are present. Source completeness is not verified.")
}
func atomicJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(path, append(b, '\n'), 0600)
}
func planOperations(job string, b []byte) ([]operation, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return nil, errors.New("share list did not return JSON")
	}
	if p, ok := raw["payload"]; ok {
		if json.Unmarshal(p, &raw) != nil {
			return nil, errors.New("invalid share list payload")
		}
	}
	type row struct {
		Name string `json:"name"`
	}
	var pubs, subs []row
	if json.Unmarshal(raw["publishes"], &pubs) != nil && len(raw["publishes"]) > 0 {
		return nil, errors.New("invalid publishes")
	}
	if json.Unmarshal(raw["subscriptions"], &subs) != nil && len(raw["subscriptions"]) > 0 {
		return nil, errors.New("invalid subscriptions")
	}
	push, pull := "", ""
	switch job {
	case "mac-devbox":
		push = "devbox-"
		pull = "from-devbox-"
	case "mac-gateway":
		push = "gw-"
	case "devbox":
		push = "from-devbox-"
		pull = "devbox-"
	case "windows-gateway":
		pull = "gw-"
	default:
		return nil, errors.New("unknown sync job")
	}
	out := []operation{}
	seen := map[string]bool{}
	add := func(rows []row, prefix, verb string) error {
		if prefix == "" {
			return nil
		}
		count := 0
		for _, r := range rows {
			if !strings.HasPrefix(r.Name, prefix) {
				continue
			}
			if !safeName.MatchString(r.Name) || seen[verb+":"+r.Name] {
				return errors.New("invalid or duplicate share name")
			}
			seen[verb+":"+r.Name] = true
			out = append(out, operation{verb, r.Name})
			count++
		}
		if count == 0 {
			return fmt.Errorf("no configured %s shares for %s", verb, job)
		}
		return nil
	}
	if e := add(subs, pull, "pull"); e != nil {
		return nil, e
	}
	if e := add(pubs, push, "push"); e != nil {
		return nil, e
	}
	return out, nil
}
func syncShares(job string) (ret error) {
	switch job {
	case "mac-backup", "mac-devbox", "mac-gateway", "devbox", "windows-gateway":
	default:
		return errors.New("unknown sync job")
	}
	root := stateRoot()
	if e := os.MkdirAll(root, 0700); e != nil {
		return e
	}
	release, err := acquireJobLock(filepath.Join(root, job+".lock"))
	if err != nil {
		return err
	}
	defer release()

	path := filepath.Join(root, job+".json")
	r := Receipt{Schema: 1, Job: job, State: "running", StartedAt: stamp()}
	if b, e := os.ReadFile(path); e == nil {
		var prev Receipt
		if json.Unmarshal(b, &prev) == nil {
			r.LastSuccess = prev.LastSuccess
		}
	}
	if e := atomicJSON(path, r); e != nil {
		return e
	}
	completed := false
	defer func() {
		if !completed && ret == nil {
			ret = errors.New("sync did not complete normally")
		}
		r.FinishedAt = stamp()
		if ret != nil {
			r.State = "failed"
			r.Detail = ret.Error()
		} else {
			r.State = "succeeded"
			r.LastSuccess = r.FinishedAt
			r.Detail = "All configured share operations completed."
		}
		if e := atomicJSON(path, r); e != nil {
			ret = fmt.Errorf("sync receipt could not be committed: %w", e)
		}
	}()
	bin := os.Getenv("MORA_BIN")
	if bin == "" {
		bin = filepath.Join(home(), ".local/bin/mora")
		if job == "windows-gateway" {
			bin = filepath.Join(home(), "opt/mora/bin/mora")
		}
	}
	// Preserve the gateway's isolated config; never point remote agents at the personal replica.
	if job == "windows-gateway" {
		if e := os.Setenv("MORA_CONFIG_DIR", filepath.Join(home(), ".config/mora-gateway")); e != nil {
			return e
		}
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, 20*time.Minute)
	defer cancel()
	if job == "mac-backup" {
		_, err := command(ctx, filepath.Join(home(), ".local/bin/mora-sync-git-locked.sh"))
		if err != nil {
			return fmt.Errorf("vault backup failed (%s); inspect the backup job", commandReason(ctx, err))
		}
		r.Operations = 1
		completed = true
		return nil
	}
	b, e := command(ctx, bin, "share", "list", "--json")
	if e != nil {
		return errors.New("could not list configured shares")
	}
	ops, e := planOperations(job, b)
	if e != nil {
		return e
	}
	var failed []string
	for _, op := range ops {
		args := []string{"share", op.verb, op.name}
		if op.verb == "push" {
			args = append(args, "--yes")
		}
		_, e := command(ctx, bin, args...)
		if e != nil {
			failed = append(failed, op.verb+" "+op.name+" ("+commandReason(ctx, e)+")")
			fmt.Printf("%s %s %s failed\n", stamp(), op.verb, op.name)
		} else {
			r.Operations++
			fmt.Printf("%s %s %s succeeded\n", stamp(), op.verb, op.name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("share operations failed: %s", strings.Join(failed, ", "))
	}
	completed = true
	return nil
}

func acquireJobLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = leasefile.TryLock(f); err != nil {
		_ = f.Close()
		return nil, errors.New("sync is already running")
	}
	return func() { _ = leasefile.Unlock(f); _ = f.Close() }, nil
}
func commandReason(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "cancelled or timed out"
	}
	var x *exec.ExitError
	if errors.As(err, &x) {
		return fmt.Sprintf("exit %d", x.ExitCode())
	}
	return "could not complete command"
}
