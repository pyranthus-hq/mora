//go:build windows

package operation

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

const processHelperEnv = "MORA_OPERATION_TEST_SLEEP"

func TestMain(m *testing.M) {
	switch os.Getenv(processHelperEnv) {
	case "1":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "259":
		os.Exit(259) // STILL_ACTIVE as an exit code
	}
	os.Exit(m.Run())
}

// A killed process whose handle is still open elsewhere (here the parent's)
// must read as dead: opening the pid succeeds, the exit code says it ended.
func TestProcessAliveSeesAKilledProcessWhileItsHandleIsOpen(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), processHelperEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if !processAlive(pid) {
		t.Fatal("a running process read as dead")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// cmd.Wait has not run, so the parent still holds a handle to the process.
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatal("a killed process still reads as alive")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An exit code of 259 equals STILL_ACTIVE, so the exit code alone cannot tell
// a finished process from a running one.
func TestProcessAliveSeesAProcessThatExitedWithCode259(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), processHelperEnv+"=259")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatal("a process that exited with code 259 still reads as alive")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
