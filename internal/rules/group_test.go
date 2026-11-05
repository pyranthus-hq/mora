package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"testing"
)

// Vectors from venture lanes/steward/tests/test_groups.py.
func TestGroupVectors(t *testing.T) {
	cases := []struct{ session, agent, want string }{
		{"abc", "", GroupSilent},
		{"s1", "", GroupWarned},
		{"abc", "agent-1", GroupSilent},
		{"11111111-2222-3333-4444-555555555555", "", GroupWarned},
		{"", "", GroupWarned},
	}
	for _, tc := range cases {
		if got := Group(tc.session, tc.agent); got != tc.want {
			t.Errorf("Group(%q, %q) = %s, want %s", tc.session, tc.agent, got, tc.want)
		}
	}
}

// Group must equal Steward's written rule: the first 8 hex digits of the
// sha256 of "<session>:<agent or main>", read as an integer, even is warned.
func TestGroupMatchesWrittenRule(t *testing.T) {
	warned := 0
	for k := 0; k < 2000; k++ {
		sid, agent := fmt.Sprintf("session-%d", k), ""
		if k%3 == 0 {
			agent = fmt.Sprintf("agent-%d", k)
		}
		key := sid + ":main"
		if agent != "" {
			key = sid + ":" + agent
		}
		sum := sha256.Sum256([]byte(key))
		n, _ := strconv.ParseUint(hex.EncodeToString(sum[:])[:8], 16, 32)
		want := GroupSilent
		if n%2 == 0 {
			want = GroupWarned
		}
		got := Group(sid, agent)
		if got != want {
			t.Fatalf("Group(%q, %q) = %s, want %s", sid, agent, got, want)
		}
		if got == GroupWarned {
			warned++
		}
	}
	if warned < 900 || warned > 1100 {
		t.Fatalf("split is uneven: %d of 2000 warned", warned)
	}
}
