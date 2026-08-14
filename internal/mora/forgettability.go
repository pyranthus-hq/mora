package mora

import (
	saliencepkg "github.com/pyranthus-hq/mora/internal/salience"
	"math"
	"sort"
	"time"
)

const forgettabilityMicrosScale = 1e6

type forgettabilityOptions struct {
	RecallHalfLifeDays    float64
	DormancyHalfLifeDays  float64
	RarityScale           float64
	WeightAge             float64
	WeightDormancy        float64
	WeightRarity          float64
	RelFloor              float64
	CommitLift            float64
	ShadowStrength        float64
	ShadowHardGateOverlap float64
	HapaxCap              float64
	BodyMinChars          int
	ThinCoverageK         int
	PerAttendeeCap        int
	EvidenceCap           int
}

type forgettabilityCandidate struct {
	StableID             string
	Title                string
	Text                 string
	CreatedAt            string
	OccurredAt           string
	DeletedAt            string
	PersonID             string
	PersonDisplay        string
	PersonKind           string
	PersonLastSeen       string
	AttendeeKnown        bool
	IdentityCorroborated bool
	Self                 bool
	BulkAuthored         bool
	HumanAuthored        bool
	ContentCorroborated  bool
	Commit               bool
	MessageCount         int
	MentionCount         int
}

type forgettabilityGates struct {
	Human         bool
	IdentityKnown bool
	NotSelf       bool
	NotBulk       bool
	NotDeleted    bool
	Valid         bool
}

type forgettabilityResult struct {
	StableID    string
	PersonID    string
	Value       float64
	ValueMicros int64
	Forget      float64
	Relevance   float64
	Freshness   float64
	Dated       bool
	Gates       forgettabilityGates
}

type forgettabilityRanking struct {
	All      []forgettabilityResult
	Selected []forgettabilityResult
	Gaps     MeetingGaps
}

func (r forgettabilityRanking) ByID(id string) forgettabilityResult {
	for _, item := range r.All {
		if item.StableID == id {
			return item
		}
	}
	return forgettabilityResult{}
}

func defaultForgettabilityOptions(in forgettabilityOptions) forgettabilityOptions {
	out := in
	if out.RecallHalfLifeDays <= 0 {
		out.RecallHalfLifeDays = 90
	}
	if out.DormancyHalfLifeDays <= 0 {
		out.DormancyHalfLifeDays = 60
	}
	if out.RarityScale <= 0 {
		out.RarityScale = 40
	}
	if out.WeightAge+out.WeightDormancy+out.WeightRarity != 1 {
		out.WeightAge = 0.55
		out.WeightDormancy = 0.30
		out.WeightRarity = 0.15
	}
	if out.RelFloor < 0 || out.RelFloor > 1 {
		out.RelFloor = 0.50
	}
	if out.RelFloor == 0 {
		out.RelFloor = 0.50
	}
	if out.CommitLift < 0 || out.CommitLift > 1 {
		out.CommitLift = 0.15
	}
	if out.CommitLift == 0 {
		out.CommitLift = 0.15
	}
	if out.ShadowStrength < 0 || out.ShadowStrength > 1 {
		out.ShadowStrength = 0.60
	}
	if out.ShadowStrength == 0 {
		out.ShadowStrength = 0.60
	}
	if out.ShadowHardGateOverlap <= 0 || out.ShadowHardGateOverlap > 1 {
		out.ShadowHardGateOverlap = 0.80
	}
	if out.HapaxCap < 0 || out.HapaxCap > 1 {
		out.HapaxCap = 0.50
	}
	if out.HapaxCap == 0 {
		out.HapaxCap = 0.50
	}
	if out.BodyMinChars <= 0 {
		out.BodyMinChars = 40
	}
	if out.ThinCoverageK <= 0 {
		out.ThinCoverageK = thinkThinK
	}
	if out.PerAttendeeCap <= 0 {
		out.PerAttendeeCap = 3
	}
	if out.EvidenceCap <= 0 {
		out.EvidenceCap = meetingPrepEvidenceCap
	}
	return out
}

func rankForgettability(now time.Time, eventTitle string, attendeeNames []string, candidates []forgettabilityCandidate, opts forgettabilityOptions) forgettabilityRanking {
	opts = defaultForgettabilityOptions(opts)
	eventTokens := forgettabilityDistinctiveTokens(eventTitle, attendeeNames)
	tokenSets := make(map[string]map[string]bool, len(candidates))
	for _, c := range candidates {
		tokenSets[c.StableID] = forgettabilityDistinctiveTokens(c.Title+" "+c.Text, attendeeNames)
	}

	ranking := forgettabilityRanking{}
	thinSeen := map[string]bool{}
	for _, c := range candidates {
		result := scoreForgettabilityCandidate(now, eventTokens, tokenSets, candidates, c, opts)
		ranking.All = append(ranking.All, result)
		if c.MentionCount > 0 && c.MentionCount < opts.ThinCoverageK && !thinSeen[c.PersonID] {
			name := c.PersonDisplay
			if name == "" {
				name = c.PersonID
			}
			ranking.Gaps.ThinAttendees = append(ranking.Gaps.ThinAttendees, "Only 1 memory about "+name+" - coverage is thin.")
			thinSeen[c.PersonID] = true
		}
	}
	sortForgettabilityResults(ranking.All)

	perPerson := map[string]int{}
	for _, item := range ranking.All {
		if len(ranking.Selected) >= opts.EvidenceCap {
			break
		}
		if item.ValueMicros <= 0 || !item.Gates.all() {
			continue
		}
		if perPerson[item.PersonID] >= opts.PerAttendeeCap {
			continue
		}
		ranking.Selected = append(ranking.Selected, item)
		perPerson[item.PersonID]++
	}
	return ranking
}

func scoreForgettabilityCandidate(now time.Time, eventTokens map[string]bool, tokenSets map[string]map[string]bool, all []forgettabilityCandidate, c forgettabilityCandidate, opts forgettabilityOptions) forgettabilityResult {
	tFact, dated := parseForgettabilityTime(c.OccurredAt, c.CreatedAt)
	ageDays := 0.0
	if dated {
		ageDays = math.Max(0, now.Sub(tFact).Hours()/24)
	}
	dormantDays := 0.0
	if lastSeen, ok := parseRFC3339(c.PersonLastSeen); ok {
		dormantDays = math.Max(0, now.Sub(lastSeen).Hours()/24)
	}
	mc := c.MessageCount
	if mc < 1 {
		mc = 1
	}

	a := 1 - math.Exp2(-ageDays/opts.RecallHalfLifeDays)
	b := 1 - math.Exp2(-dormantDays/opts.DormancyHalfLifeDays)
	corroboration := opts.HapaxCap
	if c.ContentCorroborated || (c.HumanAuthored && len(c.Text) >= opts.BodyMinChars) {
		corroboration = 1
	}
	rarity := 1 - saliencepkg.Saturate(float64(mc-1), opts.RarityScale)
	forget := clamp01(opts.WeightAge*a + opts.WeightDormancy*b + opts.WeightRarity*corroboration*rarity)

	rel := 0.0
	if len(eventTokens) > 0 {
		rel = float64(intersectionSize(eventTokens, tokenSets[c.StableID])) / float64(len(eventTokens))
	}
	relPrime := opts.RelFloor + (1-opts.RelFloor)*clamp01(rel)
	maxOverlap := maxNewerOverlap(c, all, tokenSets)
	fresh := clamp01(1 - opts.ShadowStrength*maxOverlap)
	valid := maxOverlap < opts.ShadowHardGateOverlap
	identityKnown := c.AttendeeKnown && (mc > 1 || c.IdentityCorroborated)
	gates := forgettabilityGates{
		Human:         c.PersonKind == "person",
		IdentityKnown: identityKnown,
		NotSelf:       !c.Self,
		NotBulk:       !c.BulkAuthored,
		NotDeleted:    c.DeletedAt == "",
		Valid:         valid,
	}

	value := 0.0
	if gates.all() {
		commit := 0.0
		if c.Commit {
			commit = 1
		}
		value = clamp01(fresh * relPrime * clamp01(forget+opts.CommitLift*commit))
	}
	return forgettabilityResult{
		StableID:    c.StableID,
		PersonID:    c.PersonID,
		Value:       value,
		ValueMicros: int64(math.Round(value * forgettabilityMicrosScale)),
		Forget:      forget,
		Relevance:   rel,
		Freshness:   fresh,
		Dated:       dated,
		Gates:       gates,
	}
}

func sortForgettabilityResults(results []forgettabilityResult) {
	sort.Slice(results, func(i, j int) bool {
		if results[i].ValueMicros != results[j].ValueMicros {
			return results[i].ValueMicros > results[j].ValueMicros
		}
		if results[i].Dated != results[j].Dated {
			return results[i].Dated
		}
		return results[i].StableID < results[j].StableID
	})
}

func maxNewerOverlap(c forgettabilityCandidate, all []forgettabilityCandidate, tokenSets map[string]map[string]bool) float64 {
	tFact, dated := parseForgettabilityTime(c.OccurredAt, c.CreatedAt)
	if !dated {
		return 0
	}
	denom := len(tokenSets[c.StableID])
	if denom == 0 {
		return 0
	}
	maxOverlap := 0.0
	for _, newer := range all {
		if newer.StableID == c.StableID || newer.PersonID != c.PersonID {
			continue
		}
		nt, nd := parseForgettabilityTime(newer.OccurredAt, newer.CreatedAt)
		if !nd || !nt.After(tFact) {
			continue
		}
		overlap := float64(intersectionSize(tokenSets[c.StableID], tokenSets[newer.StableID])) / float64(denom)
		if overlap > maxOverlap {
			maxOverlap = overlap
		}
	}
	return maxOverlap
}

func parseForgettabilityTime(occurredAt, createdAt string) (time.Time, bool) {
	if t, ok := parseRFC3339(occurredAt); ok {
		return t, true
	}
	return parseRFC3339(createdAt)
}

func parseRFC3339(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

func forgettabilityDistinctiveTokens(s string, attendeeNames []string) map[string]bool {
	exclude := map[string]bool{}
	for _, name := range attendeeNames {
		for _, tok := range tokenizeWords(name) {
			exclude[tok] = true
		}
	}
	out := map[string]bool{}
	for _, tok := range tokenizeWords(s) {
		if len(tok) < 3 || ftsStopwords[tok] || exclude[tok] {
			continue
		}
		out[tok] = true
	}
	return out
}

func intersectionSize(a, b map[string]bool) int {
	n := 0
	for tok := range a {
		if b[tok] {
			n++
		}
	}
	return n
}

func clamp01(x float64) float64 {
	if math.IsNaN(x) || x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func (g forgettabilityGates) all() bool {
	return g.Human && g.IdentityKnown && g.NotSelf && g.NotBulk && g.NotDeleted && g.Valid
}
