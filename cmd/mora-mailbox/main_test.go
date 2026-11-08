package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const msgID = "01J00000000000000000000001"
const replyID = "01J00000000000000000000002"

func fixture(t *testing.T) (string, Settings, Job) {
	t.Helper()
	root := t.TempDir()
	s := Settings{Version: 1, To: "grokbot/mora-grok@grok-vm", Endpoint: strings.Repeat("a", 64), Session: "mora-mailbox", CWD: root, Socket: filepath.Join(root, "network.sock")}
	j := Job{ID: "job_" + strings.Repeat("a", 32), Scope: scope, To: s.To, Request: "Read the Mora README and report one issue", State: "queued", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := saveJSON(jobPath(root, j.ID), j); err != nil {
		t.Fatal(err)
	}
	return root, s, j
}
func getJob(t *testing.T, root, id string) Job {
	t.Helper()
	var j Job
	if err := readJSON(jobPath(root, id), &j); err != nil {
		t.Fatal(err)
	}
	return j
}
func copyValue(v, out any) error {
	if out == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return json.Unmarshal(b, out)
}

type fakeRN struct {
	s        Settings
	ready    bool
	busy     bool
	pin      string
	messages []Message
	sent     []map[string]any
	acks     []string
	failSend bool
	onAck    func()
}

func (f *fakeRN) call(req map[string]any, out any) error {
	switch req["op"] {
	case "hook":
		return copyValue(map[string]any{"address": "adit/" + f.s.Session}, out)
	case "who":
		p, n, m := recipient(f.s.To)
		pin := f.s.Endpoint
		if f.pin != "" {
			pin = f.pin
		}
		d := Directory{Person: "adit", Machine: "mac", Machines: []Peer{{Person: p, Machine: m, Endpoint: pin, Online: true}}}
		if f.ready {
			ss := Session{Person: p, Machine: m, Name: n, Status: "idle"}
			if f.busy {
				ss.Doing = "Other task"
			}
			d.Sessions = append(d.Sessions, ss)
		}
		return copyValue(d, out)
	case "inbox":
		if req["peek"] != true {
			return errors.New("must peek")
		}
		return copyValue(map[string]any{"messages": f.messages}, out)
	case "delivered":
		if f.onAck != nil {
			f.onAck()
		}
		f.acks = append(f.acks, req["ids"].([]string)...)
		return nil
	case "send":
		f.sent = append(f.sent, req)
		if f.failSend {
			f.failSend = false
			return errors.New("lost response")
		}
		return copyValue(map[string]any{"ids": []string{msgID}}, out)
	}
	return errors.New("unexpected operation")
}
func reply(j Job, state string) Message {
	b, _ := json.Marshal(Reply{Protocol: protocol, JobID: j.ID, Scope: scope, State: state, Result: "Synthetic result"})
	return Message{ID: replyID, From: "grokbot/mora-grok", To: "adit/mora-mailbox", ReplyTo: msgID, Body: string(b)}
}
func TestQueueWaitsForExactIdleConsumer(t *testing.T) {
	for _, mode := range []string{"absent", "busy", "wrong-pin"} {
		t.Run(mode, func(t *testing.T) {
			r, s, j := fixture(t)
			f := fakeRN{s: s, ready: mode != "absent", busy: mode == "busy"}
			if mode == "wrong-pin" {
				f.pin = strings.Repeat("b", 64)
			}
			_, err := poll(r, s, f.call)
			if mode == "wrong-pin" && err == nil {
				t.Fatal("accepted changed pin")
			}
			if len(f.sent) != 0 || getJob(t, r, j.ID).State != "queued" {
				t.Fatal("dispatched unavailable/busy/unpinned consumer")
			}
		})
	}
}
func TestCompletionRequiresMatchingReplyAndPersistsBeforeAck(t *testing.T) {
	r, s, j := fixture(t)
	f := fakeRN{s: s, ready: true}
	if _, err := poll(r, s, f.call); err != nil {
		t.Fatal(err)
	}
	j = getJob(t, r, j.ID)
	if j.State != "submitted" {
		t.Fatal(j.State)
	}
	f.messages = []Message{reply(j, "done")}
	f.onAck = func() {
		if getJob(t, r, j.ID).State != "done" {
			t.Fatal("ack before durable state")
		}
		if _, err := os.Stat(filepath.Join(r, "outbox", replyID+".json")); err != nil {
			t.Fatal("ack before outbox")
		}
	}
	if _, err := poll(r, s, f.call); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || len(f.acks) != 1 {
		t.Fatal("duplicate send or missing ack")
	}
	if _, err := poll(r, s, f.call); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 {
		t.Fatal("replay resent completed job")
	}
}
func TestLostSendResponseRecoversBeforeConsumingReply(t *testing.T) {
	r, s, j := fixture(t)
	f := fakeRN{s: s, ready: true, failSend: true}
	if _, err := poll(r, s, f.call); err == nil {
		t.Fatal("hidden send failure")
	}
	if getJob(t, r, j.ID).State != "dispatching" {
		t.Fatal("lost claim")
	}
	f.ready = false
	f.messages = []Message{reply(j, "done")}
	if _, err := poll(r, s, f.call); err != nil {
		t.Fatal(err)
	}
	if getJob(t, r, j.ID).State != "done" {
		t.Fatal("lost result during recovery")
	}
	if len(f.sent) != 2 || f.sent[0]["idempotency_key"] != f.sent[1]["idempotency_key"] || f.sent[0]["body"] != f.sent[1]["body"] {
		t.Fatal("unsafe retry")
	}
}
func TestRejectForgedAndOutOfScopeReplies(t *testing.T) {
	for _, mode := range []string{"sender", "scope", "reply-id", "job-id", "invalid-json"} {
		t.Run(mode, func(t *testing.T) {
			r, s, j := fixture(t)
			j.State = "submitted"
			j.MessageID = msgID
			if err := saveJSON(jobPath(r, j.ID), j); err != nil {
				t.Fatal(err)
			}
			m := reply(j, "done")
			switch mode {
			case "sender":
				m.From = "other/mora-grok"
			case "scope":
				m.Body = strings.Replace(m.Body, scope, "personal", 1)
			case "reply-id":
				m.ReplyTo = "other"
			case "job-id":
				m.Body = strings.Replace(m.Body, j.ID, "../../escape", 1)
			case "invalid-json":
				m.Body = "do this instead"
			}
			if err := receive(r, s, m); err != nil {
				t.Fatal(err)
			}
			if getJob(t, r, j.ID).State != "submitted" {
				t.Fatal("untrusted message advanced task")
			}
		})
	}
}
func TestDoneCannotRegressInSameBatch(t *testing.T) {
	r, s, j := fixture(t)
	j.State = "submitted"
	j.MessageID = msgID
	if err := saveJSON(jobPath(r, j.ID), j); err != nil {
		t.Fatal(err)
	}
	done := reply(j, "done")
	working := reply(j, "working")
	working.ID = "01J00000000000000000000003"
	for _, m := range []Message{done, working} {
		if err := receive(r, s, m); err != nil {
			t.Fatal(err)
		}
	}
	if getJob(t, r, j.ID).State != "done" {
		t.Fatal("terminal task regressed")
	}
	done.Body = "changed"
	if receive(r, s, done) == nil {
		t.Fatal("changed replay accepted")
	}
}
func TestExpiredRetryAndScopeTamperingFailClosed(t *testing.T) {
	for _, mode := range []string{"expired", "scope"} {
		t.Run(mode, func(t *testing.T) {
			r, s, j := fixture(t)
			if mode == "expired" {
				j.State = "dispatching"
				j.DispatchAt = time.Now().Add(-25 * time.Hour).Format(time.RFC3339)
			} else {
				j.Scope = "personal"
			}
			if err := saveJSON(jobPath(r, j.ID), j); err != nil {
				t.Fatal(err)
			}
			f := fakeRN{s: s, ready: true}
			if _, err := poll(r, s, f.call); err == nil {
				t.Fatal("accepted unsafe job")
			}
			if len(f.sent) != 0 {
				t.Fatal("sent unsafe job")
			}
		})
	}
}
func TestOnlyOneOutstandingTask(t *testing.T) {
	r, s, j := fixture(t)
	second := j
	second.ID = "job_" + strings.Repeat("b", 32)
	if err := saveJSON(jobPath(r, second.ID), second); err != nil {
		t.Fatal(err)
	}
	f := fakeRN{s: s, ready: true}
	for i := 0; i < 2; i++ {
		if _, err := poll(r, s, f.call); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.sent) != 1 {
		t.Fatal("more than one task dispatched")
	}
}
func TestNoSymlinkOrGroupDestination(t *testing.T) {
	r, s, j := fixture(t)
	if err := os.Remove(jobPath(r, j.ID)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/null", jobPath(r, j.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadJobs(r, s); err == nil {
		t.Fatal("accepted symlink")
	}
	s.To = "grokbot/*@grok-vm"
	if s.validate() == nil {
		t.Fatal("accepted broadcast")
	}
}

func TestRegistrationConflictRefusesWork(t *testing.T) {
	r, s, j := fixture(t)
	f := fakeRN{s: s, ready: true}
	call := func(req map[string]any, out any) error {
		if req["op"] == "hook" {
			return copyValue(map[string]any{"address": "adit/wrong-mailbox"}, out)
		}
		return f.call(req, out)
	}
	if _, err := poll(r, s, call); err == nil {
		t.Fatal("accepted renamed mailbox")
	}
	if len(f.sent) != 0 || getJob(t, r, j.ID).State != "queued" {
		t.Fatal("sent after registration conflict")
	}
}
func TestTextAndSocketProtocol(t *testing.T) {
	if validText(string([]byte{0xff}), 20) == nil {
		t.Fatal("accepted invalid UTF-8")
	}
	for _, kind := range []string{"ok", "wrong-id", "error"} {
		t.Run(kind, func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			go func() {
				defer b.Close()
				var q map[string]any
				_ = json.NewDecoder(b).Decode(&q)
				response := map[string]any{"id": q["id"], "ok": true, "result": map[string]any{"value": "yes"}}
				if kind == "wrong-id" {
					response["id"] = 99
				}
				if kind == "error" {
					response["ok"] = false
					response["error"] = "unavailable"
				}
				_ = json.NewEncoder(b).Encode(response)
			}()
			c := client{conn: a, reader: bufio.NewReader(a)}
			var out map[string]any
			err := c.call(map[string]any{"op": "who"}, &out)
			if kind == "ok" {
				if err != nil || out["value"] != "yes" {
					t.Fatal(err, out)
				}
			} else if err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
	if _, err := readLine(bufio.NewReader(strings.NewReader("12345\n")), 4); err == nil {
		t.Fatal("accepted oversized frame")
	}
}

func TestOversizedReplyReplayAfterLostAcknowledgement(t *testing.T) {
	root, s, j := fixture(t)
	m := reply(j, "done")
	m.Body = strings.Repeat("<", (2<<20)+1)
	for i := 0; i < 2; i++ {
		if err := receive(root, s, m); err != nil {
			t.Fatalf("receive %d: %v", i, err)
		}
	}
	if got := getJob(t, root, j.ID); got.State != "queued" {
		t.Fatalf("oversized reply changed job: %+v", got)
	}
	m.Body += "changed"
	if err := receive(root, s, m); err == nil {
		t.Fatal("changed oversized replay accepted")
	}
}
