package mora

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const sourceProcessCleanupGrace = 5 * time.Second

type sourceProcessRunner struct {
	command func(string, ...string) *exec.Cmd
	grace   time.Duration
	after   func(time.Duration) <-chan time.Time
}

type sourceProcessError struct {
	Cause  error
	Forced bool
	Err    error
}

func (e *sourceProcessError) Error() string {
	if e.Forced {
		return fmt.Sprintf("source process forced cleanup after cancellation: %v", e.Cause)
	}
	return fmt.Sprintf("source process cancelled: %v", e.Cause)
}

func (e *sourceProcessError) Unwrap() error { return e.Cause }

func defaultSourceProcessRunner() sourceProcessRunner {
	return sourceProcessRunner{
		command: exec.Command,
		grace:   sourceProcessCleanupGrace,
		after:   time.After,
	}
}

// runSourceProcess owns the complete child lifecycle. Cancellation first sends
// an interrupt, then kills after a bounded grace. Both paths wait for Wait to
// acknowledge exit before returning, so no protected-source work survives its
// receipt.
func runSourceProcess(ctx context.Context, runner sourceProcessRunner, name string, args ...string) error {
	if runner.command == nil {
		runner.command = exec.Command
	}
	if runner.after == nil {
		runner.after = time.After
	}
	if runner.grace <= 0 {
		runner.grace = sourceProcessCleanupGrace
	}

	cmd := runner.command(name, args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return err
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	select {
	case err := <-waited:
		return sourceProcessExitError(name, args, output.String(), err)
	case <-ctx.Done():
	}

	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case err := <-waited:
		return &sourceProcessError{Cause: ctx.Err(), Err: sourceProcessExitError(name, args, output.String(), err)}
	case <-runner.after(runner.grace):
		_ = cmd.Process.Kill()
		err := <-waited
		return &sourceProcessError{Cause: ctx.Err(), Forced: true, Err: sourceProcessExitError(name, args, output.String(), err)}
	}
}

func sourceProcessExitError(name string, args []string, output string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w\n%s", name, redactCredentials(strings.Join(args, " ")), err, redactCredentials(output))
}

var _ error = (*sourceProcessError)(nil)
