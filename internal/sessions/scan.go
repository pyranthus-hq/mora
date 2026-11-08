package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// lockedVisitor serializes Visitor calls and applies OnlySession.
type lockedVisitor struct {
	mu   sync.Mutex
	v    Visitor
	free bool // the Visitor locks for itself
	only string
}

func (l *lockedVisitor) lock() func() {
	if l.free {
		return func() {}
	}
	l.mu.Lock()
	return l.mu.Unlock
}

func (l *lockedVisitor) Call(c Call) {
	if l.only == "" || c.SessionID == l.only {
		defer l.lock()()
		l.v.Call(c)
	}
}

func (l *lockedVisitor) Denial(d Denial) {
	if l.only == "" || d.SessionID == l.only {
		defer l.lock()()
		l.v.Denial(d)
	}
}

type lineReader interface {
	line(raw []byte)
	finish() []string
}

// Scan reads every transcript under Home with a fixed worker pool. Order across
// files is not defined. No error it returns holds a path. A file that cannot be
// read is a count in Stats, and the scan goes on.
func Scan(ctx context.Context, o Options, v Visitor) (Stats, error) {
	if !filepath.IsAbs(o.Home) {
		return Stats{}, errors.New("sessions: home is not an absolute path")
	}
	var st Stats
	files := discover(o, &st)
	workers := o.Workers
	if workers <= 0 {
		workers = min(runtime.GOMAXPROCS(0), 8)
	}
	_, free := v.(LockingVisitor)
	visitor := &lockedVisitor{v: v, free: free, only: o.OnlySession}
	jobs := make(chan transcript)
	var mu sync.Mutex
	claude, codex, agents := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for t := range jobs {
				part, ids := readFile(t, visitor)
				mu.Lock()
				st.add(part)
				for _, id := range ids {
					if t.harness == HarnessCodex {
						codex[id] = true
					} else {
						claude[id] = true
					}
				}
				if t.subagent && len(ids) > 0 {
					agents[t.session+"\x00"+t.agent] = true
				}
				mu.Unlock()
			}
		})
	}
	for _, t := range files {
		if ctx.Err() != nil {
			break
		}
		jobs <- t
	}
	close(jobs)
	wg.Wait()
	st.ClaudeSessions, st.CodexSessions, st.ClaudeSubagentFiles = len(claude), len(codex), len(agents)
	return st, ctx.Err()
}

// readFile reads one transcript with os.Open only, and closes it before
// returning so t.TempDir cleanup works on Windows.
func readFile(t transcript, v Visitor) (Stats, []string) {
	var st Stats
	f, err := os.Open(t.path)
	if err != nil {
		st.UnreadableFiles++
		return st, nil
	}
	defer f.Close()
	if !stillRegular(f, t.path) {
		st.SymlinksSkipped++
		return st, nil
	}
	var r lineReader = newClaudeReader(t, &st, v)
	if t.harness == HarnessCodex {
		r = newCodexReader(t, &st, v)
	}
	read := eachLine
	if t.compressed {
		read = eachZstLine
	}
	if err := read(f, &st, r.line); err != nil {
		st.UnreadableFiles++
	} else {
		st.FilesRead++
	}
	return st, r.finish()
}

// stillRegular repeats discovery's file check on the open file, so a file
// swapped for a symlink after discovery is not read.
func stillRegular(f *os.File, path string) bool {
	opened, err := f.Stat()
	if err != nil {
		return false
	}
	now, err := os.Lstat(path)
	return err == nil && now.Mode().IsRegular() && os.SameFile(opened, now)
}
