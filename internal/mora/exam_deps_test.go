package mora

import (
	"os/exec"
	"strings"
	"testing"
)

// The safety boundary, asserted rather than promised: no exam framework enters a
// production import, no test needs the network, and nothing is ever sent to an LLM
// judge. The first of those is the one a person can break by accident, so it is the
// one that gets a test.
//
// The CI binary-size job is a backstop. `go list -deps ./cmd/mora` is the contract.
func TestExamTestOnlyDepsAreNotLinked(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "../../cmd/mora").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	forbidden := []string{
		"pgregory.net/rapid",
		"github.com/rogpeppe/go-internal",
		"github.com/go-gremlins/gremlins",
		"github.com/pyranthus-hq/mora/internal/mora/exam",
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, banned := range forbidden {
			if strings.TrimSpace(dep) == banned {
				t.Errorf("%s is linked into cmd/mora. The exam and its test-only tooling must never ship in the product binary.", banned)
			}
		}
	}
}
