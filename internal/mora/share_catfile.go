package mora

// share_catfile.go — one `git cat-file --batch` process reads every pinned share
// blob. execFunc returns combined output and has no stdin, so this is a separate,
// narrow seam: gitCatFileBatchFn is swapped by tests that inject behavior.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// gitBatchReader asks one git process for objects, in order, and calls each
// with the i-th object's bytes. Every object must be a blob of at most maxSize
// bytes. An error from each stops the read and is returned unchanged.
type gitBatchReader func(ctx context.Context, dir string, objects []string, maxSize int64, each func(i int, body []byte) error) error

// gitCatFileBatchFn is the production batch reader. Tests replace it.
var gitCatFileBatchFn gitBatchReader = realGitCatFileBatch

// errGitBatchOversize reports a blob larger than the caller's maxSize. The
// reader stops before it allocates or reads the blob.
type errGitBatchOversize struct {
	index int
	size  int64
}

func (e *errGitBatchOversize) Error() string {
	return fmt.Sprintf("object %d is %d bytes", e.index, e.size)
}

func realGitCatFileBatch(ctx context.Context, dir string, objects []string, maxSize int64, each func(i int, body []byte) error) error {
	if len(objects) == 0 {
		return nil
	}
	for _, o := range objects {
		// --batch reads one object name per line.
		if o == "" || strings.ContainsAny(o, "\n\r") {
			return fmt.Errorf("git cat-file --batch: object name %q cannot be sent on one line", o)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git cat-file --batch: %w", err)
	}
	// git answers each name as it reads it. Write from a goroutine so a full
	// stdout pipe cannot block the writer while the reader waits for it.
	written := make(chan error, 1)
	go func() {
		w := bufio.NewWriter(stdin)
		var werr error
		for _, o := range objects {
			if _, werr = w.WriteString(o + "\n"); werr != nil {
				break
			}
		}
		if werr == nil {
			werr = w.Flush()
		}
		if cerr := stdin.Close(); werr == nil {
			werr = cerr
		}
		written <- werr
	}()

	callerErr := false
	readErr := func() error {
		r := bufio.NewReader(stdout)
		for i, o := range objects {
			header, err := r.ReadString('\n')
			if err != nil {
				return fmt.Errorf("git cat-file --batch: no answer for %s: %w", o, err)
			}
			header = strings.TrimSuffix(header, "\n")
			// "<oid> <type> <size>", or "<name> missing" / "<name> ambiguous".
			fields := strings.Fields(header)
			if len(fields) != 3 || strings.HasSuffix(header, " missing") || strings.HasSuffix(header, " ambiguous") {
				return fmt.Errorf("git cat-file --batch: %s is not readable: %s", o, header)
			}
			if fields[1] != "blob" {
				return fmt.Errorf("git cat-file --batch: %s is a %s, not a blob", o, fields[1])
			}
			size, perr := strconv.ParseInt(fields[2], 10, 64)
			if perr != nil || size < 0 {
				return fmt.Errorf("git cat-file --batch: %s has a malformed size: %s", o, header)
			}
			if size > maxSize {
				callerErr = true
				return &errGitBatchOversize{index: i, size: size}
			}
			body := make([]byte, size+1) // the content, then one LF
			if _, err := io.ReadFull(r, body); err != nil {
				return fmt.Errorf("git cat-file --batch: short read for %s: %w", o, err)
			}
			if body[size] != '\n' {
				return fmt.Errorf("git cat-file --batch: %s is not followed by a newline", o)
			}
			if err := each(i, body[:size]); err != nil {
				callerErr = true
				return err
			}
		}
		return nil
	}()
	if readErr != nil {
		// Stop git early: the remaining answers are no longer wanted.
		cancel()
		<-written
		_ = cmd.Wait()
		if callerErr {
			return readErr
		}
		return fmt.Errorf("%w\n%s", readErr, redactCredentials(strings.TrimSpace(stderr.String())))
	}
	werr := <-written
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git cat-file --batch: %w\n%s", err, redactCredentials(strings.TrimSpace(stderr.String())))
	}
	if werr != nil {
		return fmt.Errorf("git cat-file --batch: sending object names: %w", werr)
	}
	return nil
}

// asGitBatchOversize unwraps an oversize report.
func asGitBatchOversize(err error) (*errGitBatchOversize, bool) {
	var o *errGitBatchOversize
	ok := errors.As(err, &o)
	return o, ok
}
