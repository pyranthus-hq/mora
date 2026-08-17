// Package previewfilter owns deterministic per-memory preview filtering.
package previewfilter

import (
	"github.com/pyranthus-hq/mora/internal/graph"
	"github.com/pyranthus-hq/mora/internal/memory"
	"strings"
	"time"
)

type Options struct {
	EntityIDs map[string]bool
	Scope     string
	SinceDays int
}

func (o Options) Active() bool { return len(o.EntityIDs) > 0 || o.Scope != "" || o.SinceDays > 0 }
func FilterByInstance(byInstance map[string][]memory.Memory, opts Options, now time.Time) map[string][]memory.Memory {
	if !opts.Active() {
		return byInstance
	}
	out := make(map[string][]memory.Memory, len(byInstance))
	for key, mems := range byInstance {
		var kept []memory.Memory
		for _, m := range mems {
			if Matches(m, opts, now) {
				kept = append(kept, m)
			}
		}
		out[key] = kept
	}
	return out
}
func Matches(m memory.Memory, opts Options, now time.Time) bool {
	if len(opts.EntityIDs) > 0 && !MentionsEntity(m, opts.EntityIDs) {
		return false
	}
	if opts.Scope != "" && m.Scope != opts.Scope {
		return false
	}
	if opts.SinceDays > 0 {
		cutoff := now.Add(-time.Duration(opts.SinceDays) * 24 * time.Hour)
		ts, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil || ts.Before(cutoff) {
			return false
		}
	}
	return true
}
func MentionsEntity(m memory.Memory, idSet map[string]bool) bool {
	if len(idSet) == 0 {
		return false
	}
	parts, _, _, _ := graph.PersonRefs(m)
	for _, p := range parts {
		if idSet[p.ID] {
			return true
		}
	}
	return false
}
func ClampSinceDays(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
func AliasIDSet(canonical string, aliases []string) map[string]bool {
	set := map[string]bool{canonical: true}
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if strings.Contains(alias, "@") || strings.HasPrefix(alias, "+") {
			set[graph.PersonID(alias)] = true
		}
	}
	return set
}
