package health

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	SuccessRing          = 10
	StaleMultiplier      = 2
	AdoptMinSuccesses    = 3
	AdoptMinGap          = time.Hour
	AdoptMinDistinctDays = 3
	IntervalFloor        = time.Hour
	IntervalCeil         = 7 * 24 * time.Hour
)

// ProducerStatus is one producer evidence row in status.json.
type ProducerStatus struct {
	Name            string   `json:"name"`
	LastAttemptAt   string   `json:"last_attempt_at,omitempty"`
	LastSuccessAt   string   `json:"last_success_at,omitempty"`
	LastError       string   `json:"last_error,omitempty"`
	SuccessTimes    []string `json:"success_times"`
	IntervalSeconds int      `json:"interval_seconds"`
	Source          string   `json:"source"`
}

// ExpectedProducer is one durable liveness expectation in expected.json.
type ExpectedProducer struct {
	Name            string `json:"name"`
	IntervalSeconds int    `json:"interval_seconds"`
	Source          string `json:"source"`
	AdoptedAt       string `json:"adopted_at,omitempty"`
}

// AppendSuccessTime adds a UTC success stamp and retains the bounded raw ring.
func AppendSuccessTime(times []string, now time.Time) []string {
	times = append(times, now.UTC().Format(time.RFC3339))
	if len(times) > SuccessRing {
		times = times[len(times)-SuccessRing:]
	}
	return times
}

// AdoptInterval derives a safe cadence from non-burst, multi-day successes.
func AdoptInterval(raw []string) (int, bool) {
	ts := make([]time.Time, 0, len(raw))
	for _, s := range raw {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			ts = append(ts, t.UTC())
		}
	}
	if len(ts) < AdoptMinSuccesses {
		return 0, false
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	days := map[string]struct{}{}
	gaps := make([]time.Duration, 0, len(ts)-1)
	for i, t := range ts {
		days[t.Format("2006-01-02")] = struct{}{}
		if i > 0 {
			g := t.Sub(ts[i-1])
			if g < AdoptMinGap {
				return 0, false
			}
			gaps = append(gaps, g)
		}
	}
	if len(days) < AdoptMinDistinctDays {
		return 0, false
	}
	med := medianDuration(gaps)
	if med > IntervalCeil {
		med = IntervalCeil
	}
	return int(med.Seconds()), true
}
func medianDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// ClassifyProducers deterministically projects expected producers against evidence.
func ClassifyProducers(expected map[string]ExpectedProducer, status map[string]ProducerStatus, now time.Time, defaultInterval func(string) int) []Producer {
	names := make([]string, 0, len(expected))
	for n := range expected {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Producer, 0, len(names))
	for _, name := range names {
		exp := expected[name]
		interval := exp.IntervalSeconds
		if interval <= 0 {
			interval = 86400
			if defaultInterval != nil {
				interval = defaultInterval(name)
			}
		}
		ph := Producer{Name: name, IntervalSeconds: interval, Source: exp.Source, Subject: ProducerSubjectProducer}
		st, ok := status[name]
		ph.LastSuccessAt = st.LastSuccessAt
		ph.LastAttemptAt = st.LastAttemptAt
		ph.LastError = st.LastError
		last, err := time.Parse(time.RFC3339, st.LastSuccessAt)
		if !ok || st.LastSuccessAt == "" || err != nil {
			ph.State = ProducerNever
			out = append(out, ph)
			continue
		}
		age := now.UTC().Sub(last.UTC())
		if age < 0 {
			age = 0
		}
		ph.AgeHours = int(age.Hours())
		switch {
		case st.LastError != "" && AttemptAfterSuccess(st):
			ph.State = ProducerFailed
		case age >= time.Duration(interval)*time.Second*StaleMultiplier:
			ph.State = ProducerStale
		default:
			ph.State = ProducerFresh
		}
		out = append(out, ph)
	}
	return out
}

// ProducerLedgerFailure returns the typed fail-closed ledger record.
func ProducerLedgerFailure(err error) []Producer {
	return []Producer{{State: ProducerFailed, LastError: err.Error(), Subject: ProducerSubjectLedger}}
}

// AttemptAfterSuccess reports whether the latest recorded attempt postdates success.
func AttemptAfterSuccess(st ProducerStatus) bool {
	a, aerr := time.Parse(time.RFC3339, st.LastAttemptAt)
	s, serr := time.Parse(time.RFC3339, st.LastSuccessAt)
	if aerr != nil {
		return st.LastError != ""
	}
	if serr != nil {
		return true
	}
	return a.After(s)
}

// BriefArtifactFresh checks the dated daily-brief artifact rail using an injected clock.
func BriefArtifactFresh(vaultDir string, now time.Time) (bool, bool) {
	matches, _ := filepath.Glob(filepath.Join(vaultDir, "briefs", "*-brief.md"))
	newest := time.Time{}
	present := false
	for _, m := range matches {
		datePart := strings.TrimSuffix(filepath.Base(m), "-brief.md")
		d, err := time.Parse("2006-01-02", datePart)
		if err != nil {
			continue
		}
		present = true
		if d.After(newest) {
			newest = d
		}
	}
	if !present {
		return true, false
	}
	stale := now.UTC().Sub(newest.UTC()) >= StaleMultiplier*24*time.Hour
	return !stale, true
}
