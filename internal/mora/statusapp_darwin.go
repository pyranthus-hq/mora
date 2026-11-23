//go:build darwin

package mora

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Code requirements for the two calling processes. Release builds keep these
// team-anchored values. A local dogfood build may replace them at link time
// (-ldflags -X) with cdhash requirements for one ad-hoc signed app; release
// tooling never does.
var (
	statusAppRequirement       = `anchor apple generic and certificate leaf[subject.OU] = "VS8M5VJBZ5" and identifier "com.pyranthus.mora-status"`
	statusCollectorRequirement = `anchor apple generic and certificate leaf[subject.OU] = "VS8M5VJBZ5" and identifier "com.pyranthus.mora-status.tentacles"`
)

func statusAppCaller() error {
	collector := os.Getppid()
	if collector <= 1 {
		return errors.New("no calling process")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "ppid=", "-p", strconv.Itoa(collector)).Output()
	if err != nil {
		return errors.New("cannot read the calling process")
	}
	app, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || app <= 1 {
		return errors.New("the caller was not started by Mora Status")
	}
	// codesign checks a running process by pid against the code it runs, not
	// a file on disk that could be swapped after launch.
	for _, c := range []struct {
		pid  int
		req  string
		name string
	}{{collector, statusCollectorRequirement, "collector"}, {app, statusAppRequirement, "app"}} {
		if err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "-R="+c.req, strconv.Itoa(c.pid)).Run(); err != nil {
			return fmt.Errorf("the calling %s is not the signed Mora Status %s", c.name, c.name)
		}
	}
	return nil
}
