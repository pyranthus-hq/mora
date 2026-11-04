package corrections

import (
	"cmp"
	"crypto/sha256"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/pyranthus-hq/mora/internal/sessions"
)

// Report is the mora.check payload. It holds counts, dates and table text only.
type Report struct {
	Window      Window     `json:"window"`
	Harnesses   []Harness  `json:"harnesses"`
	Corrections Counts     `json:"corrections"`
	Repeat      Repeat     `json:"repeat"`
	Top         []Pattern  `json:"top_patterns"`
	NotCounted  NotCounted `json:"not_counted"`
	Read        ReadNotes  `json:"read"`
}

// Window is the time the report covers. Dates are UTC.
type Window struct {
	Since string `json:"since"`
	First string `json:"first"`
	Last  string `json:"last"`
}

// Harness is one agent tool. Signal says whether it records rejections at all.
type Harness struct {
	Name          string `json:"name"`
	Sessions      int    `json:"sessions"`
	SubagentFiles int    `json:"subagent_files"`
	Calls         int    `json:"calls"`
	Signal        string `json:"signal"`
}

// Counts holds the Tier A corrections.
type Counts struct {
	BashRejected     int `json:"bash_rejected"`
	BashInterrupted  int `json:"bash_interrupted"`
	OtherRejected    int `json:"other_rejected"`
	OtherInterrupted int `json:"other_interrupted"`
	UserActions      int `json:"user_actions"`
}

// Repeat holds sections 3.8 to 3.12 of the spec. Rates are null with no data.
type Repeat struct {
	Scored           int      `json:"scored"`
	Eligible         int      `json:"eligible"`
	SameCommand      int      `json:"same_command"`
	SameShape        int      `json:"same_shape"`
	Rate             *float64 `json:"rate"`
	ShapeRate        *float64 `json:"shape_rate"`
	BaselinePairs    int      `json:"baseline_pairs"`
	BaselineRepeated int      `json:"baseline_repeated"`
	BaselineRate     *float64 `json:"baseline_rate"`
}

// Pattern is one shape that came back.
type Pattern struct {
	Shape       string `json:"shape"`
	SameCommand int    `json:"same_command"`
	SameShape   int    `json:"same_shape"`
}

// NotCounted holds the denials that are not corrections.
type NotCounted struct {
	BlockedByRule           int `json:"blocked_by_rule"`
	BlockedByAutoMode       int `json:"blocked_by_auto_mode"`
	CancelledSiblings       int `json:"cancelled_siblings"`
	InterruptedInSubagent   int `json:"interrupted_in_subagent"`
	InterruptedNotHuman     int `json:"interrupted_not_human"`
	InterruptedBetweenCalls int `json:"interrupted_between_calls"`
	UntypedRejections       int `json:"untyped_rejections"`
	UnknownDenialKinds      int `json:"unknown_denial_kinds"`
}

// ReadNotes is every Stats field plus the copies the Corpus removed. Counts of
// events are the Corpus's own, so copies count once and Since applies.
type ReadNotes struct {
	sessions.Stats
	CopiedCalls int `json:"copied_calls"`
}

type sessionRef struct{ harness, id string }

// callID is a hash of (harness, tool_use_id). It is half the size of the two
// strings, and it leaves the call map with no pointer for the collector to scan.
type callID [16]byte

func newCallID(harness, id string) callID {
	sum := sha256.Sum256([]byte(harness + "\x00" + id))
	return callID(sum[:16])
}

// segment is what a row keeps of a simple command, in 20 bytes with no pointer
// because a call can hold several. Raw text is dropped. shape numbers c.shapes.
type segment struct {
	key   Key
	shape int32
}

// row is one surviving call, in 32 bytes because there can be millions. Times
// are Unix milliseconds. Its segments are segs[list : list+count].
type row struct {
	ts, first int64
	session   int32
	copies    int32 // other sightings of this id
	list      int32
	count     int8
	primary   int8
	old       bool // before since: not counted, but it still links sessions, dates a Start and names a correction
}

// eventRef is one event that names no command. id is what its copies share.
type eventRef struct{ kind, id string }

// Corpus collects Calls and Denials. It gives the same Report for any order.
// Call and Denial may run at once, and Report runs after they end.
type Corpus struct {
	mu       sync.Mutex
	since    time.Time
	index    map[sessionRef]int32
	refs     []sessionRef
	parent   []int32 // union-find over linked sessions
	calls    map[callID]row
	segs     []segment // one run per row
	shapes   []string  // each shape once, with "" first
	shapeIDs map[string]int32
	denials  map[string]sessions.Denial
	events   map[eventRef]bool
}

// NewCorpus reports nothing older than since. A zero since keeps everything.
// Older records in the files Scan opens are still held: they say when a session
// began and which commands were corrected.
func NewCorpus(since time.Time) *Corpus {
	return &Corpus{since: since, index: map[sessionRef]int32{}, calls: map[callID]row{}, shapes: []string{""}, shapeIDs: map[string]int32{"": 0},
		denials: map[string]sessions.Denial{}, events: map[eventRef]bool{}}
}

// LocksItself lets Scan run Parse on every worker, outside the lock.
func (c *Corpus) LocksItself() {}

func (c *Corpus) session(harness, id string) int32 {
	ref := sessionRef{harness, id}
	s, ok := c.index[ref]
	if !ok {
		s = int32(len(c.refs))
		c.index[ref] = s
		c.refs = append(c.refs, ref)
		c.parent = append(c.parent, s)
	}
	return s
}

func (c *Corpus) find(s int32) int32 {
	for c.parent[s] != s {
		c.parent[s] = c.parent[c.parent[s]]
		s = c.parent[s]
	}
	return s
}

func (c *Corpus) link(a, b int32) {
	c.parent[c.find(a)] = c.find(b)
}

// lower reports whether the first copy wins over the second, by the lowest
// (FileFirst, TS, SessionID). Equal records keep the one already held.
func lower(aFirst, aTS int64, aSession string, bFirst, bTS int64, bSession string) bool {
	return cmp.Or(cmp.Compare(aFirst, bFirst), cmp.Compare(aTS, bTS), cmp.Compare(aSession, bSession)) < 0
}

// Call stores one compact row per (harness, tool_use_id). The raw Command is
// parsed here and not kept.
func (c *Corpus) Call(call sessions.Call) {
	p := Parse(call.Command)
	id := newCallID(call.Harness, call.ToolUseID)
	first, ts := call.FileFirst.UnixMilli(), call.TS.UnixMilli()
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.session(call.Harness, call.SessionID)
	r := row{session: s, ts: ts, first: first, old: call.TS.Before(c.since)}
	if held, seen := c.calls[id]; seen {
		c.link(held.session, s)
		held.copies++
		if !lower(first, ts, call.SessionID, held.first, held.ts, c.refs[held.session].id) {
			c.calls[id] = held
			return
		}
		r.copies = held.copies
	}
	r.primary, r.list, r.count = int8(p.Primary), int32(len(c.segs)), int8(len(p.Segments))
	for _, seg := range p.Segments {
		c.segs = append(c.segs, segment{seg.Key, c.shapeID(seg.Shape)})
	}
	c.calls[id] = r
}

func (c *Corpus) shapeID(shape string) int32 {
	n, ok := c.shapeIDs[shape]
	if !ok {
		n = int32(len(c.shapes))
		c.shapeIDs[shape] = n
		c.shapes = append(c.shapes, shape)
	}
	return n
}

func (c *Corpus) primary(r row) (segment, bool) {
	if r.primary < 0 {
		return segment{}, false
	}
	return c.segs[r.list+int32(r.primary)], true
}

// Denial dedupes like Call. A copied denial is not added to copied_calls, which
// counts calls only.
func (c *Corpus) Denial(d sessions.Denial) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch d.Kind {
	case sessions.KindBetweenCalls, sessions.KindCodexTurnInterrupt, sessions.KindCodexPatchDeclined:
		if !d.TS.Before(c.since) {
			c.events[eventRef{d.Kind, cmp.Or(d.ToolUseID, d.TS.UTC().Format(time.RFC3339Nano))}] = true
		}
		return
	}
	s := c.session(sessions.HarnessClaude, d.SessionID)
	if held, seen := c.denials[d.ToolUseID]; seen {
		c.link(c.session(sessions.HarnessClaude, held.SessionID), s)
		if !lower(d.FileFirst.UnixMilli(), d.TS.UnixMilli(), d.SessionID, held.FileFirst.UnixMilli(), held.TS.UnixMilli(), held.SessionID) {
			return
		}
	}
	c.denials[d.ToolUseID] = d
}

type lastStart struct {
	group int32
	at    int64
}

// latest holds the two newest linked groups among some sessions, newest first.
// Section 3.9 of the spec only asks whether a group other than one's own started
// after a time, and one of two groups is always another. So memory stays fixed
// however many sessions hold a Key.
type latest [2]lastStart

var noLatest = latest{{-1, math.MinInt64}, {-1, math.MinInt64}}

func (l *latest) add(group int32, at int64) {
	switch {
	case group == l[0].group:
		l[0].at = max(l[0].at, at)
	case at > l[0].at:
		l[0], l[1] = lastStart{group, at}, l[0]
	case group == l[1].group:
		l[1].at = max(l[1].at, at)
	case at > l[1].at:
		l[1] = lastStart{group, at}
	}
}

func (l latest) later(own int32, at int64) bool {
	if l[0].group != own {
		return l[0].at > at
	}
	return l[1].at > at
}

// timeline answers section 3.9 of the spec: which sessions count as later.
type timeline struct {
	c       *Corpus
	all     latest
	byKey   map[Key]latest
	byShape map[int32]latest
}

func (c *Corpus) timeline() *timeline {
	t := &timeline{c: c, all: noLatest, byKey: map[Key]latest{}, byShape: map[int32]latest{}}
	start := slices.Repeat([]int64{math.MinInt64}, len(c.refs))
	for _, r := range c.calls {
		if start[r.session] == math.MinInt64 || r.ts < start[r.session] {
			start[r.session] = r.ts
		}
		if seg, named := c.primary(r); named {
			t.byKey[seg.key] = noLatest // only a primary's Key is ever asked about
		}
	}
	for s, at := range start {
		if at != math.MinInt64 {
			t.all.add(c.find(int32(s)), at)
		}
	}
	for _, r := range c.calls {
		if r.old {
			continue // its session began before since, so it cannot be later
		}
		group, at := c.find(r.session), start[r.session]
		for _, seg := range c.segs[r.list : r.list+int32(r.count)] {
			if l, asked := t.byKey[seg.key]; asked {
				l.add(group, at)
				t.byKey[seg.key] = l
			}
			if seg.shape != 0 {
				l, ok := t.byShape[seg.shape]
				if !ok {
					l = noLatest
				}
				l.add(group, at)
				t.byShape[seg.shape] = l
			}
		}
	}
	return t
}

// hasLater reports whether any session is later than at, for session s.
func (t *timeline) hasLater(s int32, at int64) bool {
	return t.all.later(t.c.find(s), at)
}

// anyLater reports whether a session holding k is later than at, for session s.
func anyLater[K comparable](t *timeline, among map[K]latest, k K, s int32, at int64) bool {
	l, ok := among[k]
	return ok && l.later(t.c.find(s), at)
}

type scoredRef struct {
	session int32
	key     Key
}

// scoredRow is section 3.8 of the spec: one per (session, Key of the primary).
type scoredRow struct {
	at    int64
	shape int32
}

// span tracks the first and last timestamp seen.
type span struct {
	first, last int64
	seen        bool
}

func (w *span) add(ts int64) {
	if !w.seen || ts < w.first {
		w.first = ts
	}
	if !w.seen || ts > w.last {
		w.last = ts
	}
	w.seen = true
}

func (w span) window(since time.Time) Window {
	out := Window{}
	if !since.IsZero() {
		out.Since = since.UTC().Format(time.DateOnly)
	}
	if w.seen {
		out.First = time.UnixMilli(w.first).UTC().Format(time.DateOnly)
		out.Last = time.UnixMilli(w.last).UTC().Format(time.DateOnly)
	}
	return out
}

// Report applies sections 3.8 to 3.12 of the spec.
func (c *Corpus) Report(st sessions.Stats) Report {
	t := c.timeline()
	var when span
	calls := map[string]int{}
	copied := 0
	for _, r := range c.calls {
		if !r.old {
			when.add(r.ts)
			calls[c.refs[r.session].harness]++
			copied += int(r.copies)
		}
	}
	counts, notCounted, scored, corrected := c.tally(&when)
	events := map[string]int{}
	for e := range c.events {
		events[e.kind]++
	}
	st.UntypedRejections, st.InterruptedBetweenCalls = notCounted[UntypedRejections], events[sessions.KindBetweenCalls]
	st.CodexTurnInterrupts, st.CodexPatchesDeclined = events[sessions.KindCodexTurnInterrupt], events[sessions.KindCodexPatchDeclined]
	r := Report{
		Window: when.window(c.since),
		Harnesses: []Harness{
			{sessions.HarnessClaude, st.ClaudeSessions, st.ClaudeSubagentFiles, calls[sessions.HarnessClaude], "typed"},
			{sessions.HarnessCodex, st.CodexSessions, 0, calls[sessions.HarnessCodex], "no_rejection_signal"},
		},
		Corrections: counts,
		Top:         []Pattern{},
		NotCounted: NotCounted{
			BlockedByRule: notCounted[BlockedByRule], BlockedByAutoMode: notCounted[BlockedByAutoMode],
			CancelledSiblings: notCounted[CancelledSiblings], InterruptedInSubagent: notCounted[InterruptedInSubagent],
			InterruptedNotHuman: notCounted[InterruptedNotHuman], InterruptedBetweenCalls: st.InterruptedBetweenCalls,
			UntypedRejections: st.UntypedRejections, UnknownDenialKinds: notCounted[UnknownDenialKinds],
		},
		Read: ReadNotes{Stats: st, CopiedCalls: copied},
	}
	r.Repeat, r.Top = t.repeats(scored)
	c.baseline(t, corrected, &r.Repeat)
	return r
}

// tally classifies each surviving denial and finds the commands they name. A
// correction before since is not counted, but its command is still corrected. A
// denial inside the window is scored even when its call came before since.
func (c *Corpus) tally(when *span) (Counts, map[string]int, map[scoredRef]scoredRow, map[Key]bool) {
	var counts Counts
	notCounted := map[string]int{}
	scored := map[scoredRef]scoredRow{}
	corrected := map[Key]bool{}
	actions := map[string]bool{}
	for id, d := range c.denials {
		kind, reason := Classify(d)
		bash := d.Tool == "Bash"
		seg, named := segment{}, false
		if call, ok := c.calls[newCallID(sessions.HarnessClaude, id)]; ok && bash {
			seg, named = c.primary(call)
		}
		if named && reason == "" {
			corrected[seg.key] = true
		}
		if d.TS.Before(c.since) {
			continue
		}
		when.add(d.TS.UnixMilli())
		if reason != "" {
			notCounted[reason]++
			continue
		}
		actions[cmp.Or(d.MessageID, id)] = true
		switch {
		case bash && kind == KindRejected:
			counts.BashRejected++
		case bash:
			counts.BashInterrupted++
		case kind == KindRejected:
			counts.OtherRejected++
		default:
			counts.OtherInterrupted++
		}
		if !named {
			continue
		}
		ref := scoredRef{c.session(sessions.HarnessClaude, d.SessionID), seg.key}
		at := d.TS.UnixMilli()
		if held, ok := scored[ref]; !ok || at < held.at || (at == held.at && c.shapes[seg.shape] < c.shapes[held.shape]) {
			scored[ref] = scoredRow{at, seg.shape}
		}
	}
	counts.UserActions = len(actions)
	return counts, notCounted, scored, corrected
}

func (t *timeline) repeats(scored map[scoredRef]scoredRow) (Repeat, []Pattern) {
	rep := Repeat{Scored: len(scored)}
	patterns := map[string]*Pattern{}
	for ref, row := range scored {
		if !t.hasLater(ref.session, row.at) {
			continue
		}
		rep.Eligible++
		shape := cmp.Or(t.c.shapes[row.shape], OtherShape)
		if patterns[shape] == nil {
			patterns[shape] = &Pattern{Shape: shape}
		}
		if anyLater(t, t.byKey, ref.key, ref.session, row.at) {
			rep.SameCommand++
			patterns[shape].SameCommand++
		}
		if row.shape != 0 && anyLater(t, t.byShape, row.shape, ref.session, row.at) {
			rep.SameShape++
			patterns[shape].SameShape++
		}
	}
	rep.Rate = rate(rep.SameCommand, rep.Eligible)
	rep.ShapeRate = rate(rep.SameShape, rep.Eligible)
	top := []Pattern{}
	for _, p := range patterns {
		if p.SameShape > 0 || (p.Shape == OtherShape && p.SameCommand > 0) {
			top = append(top, *p)
		}
	}
	slices.SortFunc(top, func(a, b Pattern) int {
		return cmp.Or(cmp.Compare(b.SameCommand, a.SameCommand), cmp.Compare(b.SameShape, a.SameShape), cmp.Compare(a.Shape, b.Shape))
	})
	return rep, top[:min(len(top), 10)]
}

// baseline is section 3.12 of the spec: the same question for commands nobody
// rejected or interrupted.
func (c *Corpus) baseline(t *timeline, corrected map[Key]bool, rep *Repeat) {
	pairs := map[scoredRef]int64{}
	for _, r := range c.calls {
		seg, named := c.primary(r)
		if r.old || !named || corrected[seg.key] {
			continue
		}
		ref := scoredRef{r.session, seg.key}
		if at, ok := pairs[ref]; !ok || r.ts < at {
			pairs[ref] = r.ts
		}
	}
	for ref, at := range pairs {
		if !t.hasLater(ref.session, at) {
			continue
		}
		rep.BaselinePairs++
		if anyLater(t, t.byKey, ref.key, ref.session, at) {
			rep.BaselineRepeated++
		}
	}
	rep.BaselineRate = rate(rep.BaselineRepeated, rep.BaselinePairs)
}

func rate(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	r := math.Round(float64(n)/float64(d)*10000) / 10000
	return &r
}
