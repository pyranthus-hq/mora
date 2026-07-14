package exam

import (
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

func ruleError(rule, format string, args ...any) error {
	return fmt.Errorf("ERR_%s [%s]: %s", strings.ToUpper(rule), rule, fmt.Sprintf(format, args...))
}

func Validate(l Ledger) error {
	ids, err := validateIdentity(l)
	if err != nil {
		return err
	}
	if err := validateTimestamps(l); err != nil {
		return err
	}
	if err := validateTransitions(l); err != nil {
		return err
	}
	if err := validateDirections(l); err != nil {
		return err
	}
	if err := validateSpansAndClosure(l); err != nil {
		return err
	}
	if err := validateRequiredShapes(l); err != nil {
		return err
	}
	if err := validateSelfAttendee(l); err != nil {
		return err
	}
	if err := validateDefectIsolation(l); err != nil {
		return err
	}
	if err := validateBalance(l); err != nil {
		return err
	}
	if err := validatePersona(ids); err != nil {
		return err
	}
	return validateChannelGrain(l)
}

func validateIdentity(l Ledger) (map[string]Identity, error) {
	if l.Version != 1 || strings.TrimSpace(l.Self.ID) == "" {
		return nil, ruleError(RuleIdentity, "version 1 and a declared self identity are required")
	}
	ids := map[string]Identity{l.Self.ID: l.Self}
	for _, p := range l.People {
		if p.ID == "" || p.ID == l.Self.ID {
			return nil, ruleError(RuleIdentity, "self is duplicated or an identity id is empty")
		}
		if _, exists := ids[p.ID]; exists {
			return nil, ruleError(RuleIdentity, "duplicate identity %q", p.ID)
		}
		ids[p.ID] = p
	}
	addresses := map[string]string{}
	for id, p := range ids {
		for _, raw := range append(append([]string(nil), p.Emails...), p.Handles...) {
			key := strings.ToLower(strings.TrimSpace(raw))
			if key == "" {
				return nil, ruleError(RuleIdentity, "identity %q has an empty address", id)
			}
			if prior, exists := addresses[key]; exists && prior != id {
				return nil, ruleError(RuleIdentity, "address %q belongs to both %q and %q", raw, prior, id)
			}
			addresses[key] = id
		}
	}
	resolve := func(kind, ref string) error {
		if _, ok := ids[ref]; !ok {
			return ruleError(RuleIdentity, "%s references unknown identity %q", kind, ref)
		}
		return nil
	}
	for _, a := range l.Artifacts {
		for _, p := range a.Participants {
			if err := resolve("participant", p); err != nil {
				return nil, err
			}
		}
		for _, m := range a.Messages {
			if err := resolve("message sender", m.From); err != nil {
				return nil, err
			}
			for _, ref := range append(append([]string(nil), m.To...), m.Cc...) {
				if err := resolve("message recipient", ref); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, c := range l.Commitments {
		if err := resolve("commitment owner", c.Owner); err != nil {
			return nil, err
		}
		if err := resolve("commitment counterparty", c.Counterparty); err != nil {
			return nil, err
		}
		if c.RequiresMerge != "" {
			parts := strings.Split(c.RequiresMerge, "|")
			if len(parts) != 2 || addresses[strings.ToLower(parts[0])] == "" || addresses[strings.ToLower(parts[0])] != addresses[strings.ToLower(parts[1])] {
				return nil, ruleError(RuleIdentity, "commitment %q requires unresolved identity pair %q", c.ID, c.RequiresMerge)
			}
		}
	}
	return ids, nil
}

func parseAt(rule, label, raw string, asOf time.Time) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, ruleError(rule, "%s is not RFC3339: %q", label, raw)
	}
	if t.After(asOf) {
		return time.Time{}, ruleError(rule, "%s is after as_of: %q", label, raw)
	}
	return t, nil
}

func validateTimestamps(l Ledger) error {
	asOf, err := time.Parse(time.RFC3339, l.AsOf)
	if err != nil {
		return ruleError(RuleTimestamp, "as_of is not RFC3339: %q", l.AsOf)
	}
	for _, a := range l.Artifacts {
		if _, err := parseAt(RuleTimestamp, "artifact occurred_at", a.OccurredAt, asOf); err != nil {
			return err
		}
		var prior time.Time
		for _, m := range a.Messages {
			at, err := parseAt(RuleTimestamp, "message at", m.At, asOf)
			if err != nil {
				return err
			}
			if !prior.IsZero() && at.Before(prior) {
				return ruleError(RuleTimestamp, "messages in artifact %q are not monotonic", a.ID)
			}
			prior = at
		}
	}
	for _, c := range l.Commitments {
		if c.DueAt != "" {
			if _, err := parseAt(RuleTimestamp, "due_at", c.DueAt, asOf); err != nil {
				return err
			}
		}
		for _, tr := range c.Transitions {
			if _, err := parseAt(RuleTimestamp, "transition at", tr.At, asOf); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateTransitions(l Ledger) error {
	for _, c := range l.Commitments {
		if c.State != "open" && c.State != "closed" && c.State != "superseded" {
			return ruleError(RuleTransition, "commitment %q has illegal state %q", c.ID, c.State)
		}
		opened, ok := spanMessageTime(l, c.OpenedBy)
		if !ok {
			continue
		}
		prior := opened
		for _, tr := range c.Transitions {
			at, err := time.Parse(time.RFC3339, tr.At)
			if err != nil {
				continue
			}
			if at.Before(prior) || tr.To == "open" || (tr.To != "closed" && tr.To != "superseded") {
				return ruleError(RuleTransition, "commitment %q has illegal or unordered transition", c.ID)
			}
			prior = at
		}
		if c.State == "open" && len(c.Transitions) != 0 {
			return ruleError(RuleTransition, "open commitment %q has terminal transitions", c.ID)
		}
		if c.State != "open" {
			if len(c.Transitions) == 0 || c.Transitions[len(c.Transitions)-1].To != c.State {
				return ruleError(RuleTransition, "commitment %q state does not match terminal transition", c.ID)
			}
		}
		if c.State == "closed" && len(c.Transitions) == 0 {
			return ruleError(RuleTransition, "closed commitment %q lacks evidence transition", c.ID)
		}
	}
	return nil
}

func validateDirections(l Ledger) error {
	for _, c := range l.Commitments {
		want := "owed_by_counterparty"
		if c.Owner == l.Self.ID {
			want = "owed_by_self"
		}
		if c.Direction != want {
			return ruleError(RuleDirection, "commitment %q direction %q disagrees with owner %q", c.ID, c.Direction, c.Owner)
		}
	}
	return nil
}

func validateSpansAndClosure(l Ledger) error {
	artifactIDs, memoryIDs, commitmentIDs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, a := range l.Artifacts {
		if a.ID == "" || artifactIDs[a.ID] || a.MemoryID == "" || memoryIDs[a.MemoryID] {
			return ruleError(RuleEvidenceSpan, "duplicate or empty artifact/memory id at %q", a.ID)
		}
		artifactIDs[a.ID], memoryIDs[a.MemoryID] = true, true
		messages := map[string]bool{}
		for _, m := range a.Messages {
			if m.ID == "" || messages[m.ID] {
				return ruleError(RuleEvidenceSpan, "duplicate or empty message id %q in %q", m.ID, a.ID)
			}
			messages[m.ID] = true
			blocks := map[string]bool{}
			for _, b := range m.Body {
				if b.ID == "" || blocks[b.ID] {
					return ruleError(RuleEvidenceSpan, "duplicate or empty block id %q in %q/%q", b.ID, a.ID, m.ID)
				}
				blocks[b.ID] = true
			}
		}
	}
	crossChannel := false
	for _, c := range l.Commitments {
		if c.ID == "" || commitmentIDs[c.ID] {
			return ruleError(RuleEvidenceSpan, "duplicate or empty commitment id %q", c.ID)
		}
		commitmentIDs[c.ID] = true
		openingChannel, err := resolveSpan(l, c.OpenedBy)
		if err != nil {
			return err
		}
		for _, tr := range c.Transitions {
			closingChannel, err := resolveSpan(l, tr.Evidence)
			if err != nil {
				return ruleError(RuleClosure, "%v", err)
			}
			if openingChannel != closingChannel {
				crossChannel = true
			}
		}
		if c.DuplicateOf != "" && c.DuplicateOf == c.ID {
			return ruleError(RuleEvidenceSpan, "commitment %q duplicates itself", c.ID)
		}
	}
	for _, n := range l.NonObligations {
		if _, err := resolveSpan(l, n.Span); err != nil {
			return err
		}
	}
	for _, c := range l.Commitments {
		if c.DuplicateOf != "" {
			if !commitmentIDs[c.DuplicateOf] {
				return ruleError(RuleEvidenceSpan, "commitment %q duplicates unknown %q", c.ID, c.DuplicateOf)
			}
			if artifactOfSpan(l, c.OpenedBy) == artifactOfCommitment(l, c.DuplicateOf) {
				return ruleError(RuleEvidenceSpan, "duplicate pair %q and %q share one artifact", c.ID, c.DuplicateOf)
			}
		}
	}
	if !crossChannel {
		return ruleError(RuleClosure, "ledger contains no cross-channel closure")
	}
	return nil
}

func resolveSpan(l Ledger, s Span) (string, error) {
	for _, a := range l.Artifacts {
		if a.ID != s.ArtifactID {
			continue
		}
		if s.MessageID == "" {
			if s.BlockID != "" || s.Quote == "" || !strings.Contains(a.Subject, s.Quote) {
				return "", ruleError(RuleEvidenceSpan, "subject span in %q does not resolve", a.ID)
			}
			return a.Channel, nil
		}
		for _, m := range a.Messages {
			if m.ID != s.MessageID {
				continue
			}
			for _, b := range m.Body {
				if b.ID == s.BlockID && s.Quote != "" && strings.Contains(b.Text, s.Quote) {
					return a.Channel, nil
				}
			}
		}
		return "", ruleError(RuleEvidenceSpan, "span in %q does not resolve", a.ID)
	}
	return "", ruleError(RuleEvidenceSpan, "span references unknown artifact %q", s.ArtifactID)
}

func spanMessageTime(l Ledger, s Span) (time.Time, bool) {
	for _, a := range l.Artifacts {
		if a.ID != s.ArtifactID {
			continue
		}
		if s.MessageID == "" {
			t, err := time.Parse(time.RFC3339, a.OccurredAt)
			return t, err == nil
		}
		for _, m := range a.Messages {
			if m.ID == s.MessageID {
				t, err := time.Parse(time.RFC3339, m.At)
				return t, err == nil
			}
		}
	}
	return time.Time{}, false
}

func artifactOfSpan(l Ledger, s Span) string { return s.ArtifactID }

func artifactOfCommitment(l Ledger, id string) string {
	for _, c := range l.Commitments {
		if c.ID == id {
			return c.OpenedBy.ArtifactID
		}
	}
	return ""
}

func validateRequiredShapes(l Ledger) error {
	hasReply, hasQuoted, hasForwarded, hasFooter, hasIMessage := false, false, false, false, false
	for _, a := range l.Artifacts {
		if a.Channel == "gmail" {
			hasReply = hasReply || len(a.Messages) > 1
			for _, m := range a.Messages {
				for _, b := range m.Body {
					hasQuoted = hasQuoted || b.Kind == "quoted_reply"
					hasForwarded = hasForwarded || b.Kind == "forwarded"
					hasFooter = hasFooter || b.Kind == "footer"
				}
			}
		}
		if a.Channel == "imessage" {
			from := map[string]bool{}
			for _, m := range a.Messages {
				from[m.From] = true
			}
			hasIMessage = hasIMessage || len(from) >= 2 && from[l.Self.ID]
		}
	}
	if !hasReply || !hasQuoted || !hasForwarded || !hasFooter || !hasIMessage {
		return ruleError(RuleReplyChainQuotes, "required reply/quoted/forwarded/footer/multi-speaker shape is missing")
	}
	return nil
}

func validateSelfAttendee(l Ledger) error {
	for _, c := range l.Commitments {
		for _, surface := range c.ExpectedIn {
			if !strings.HasPrefix(surface, "meeting:") {
				continue
			}
			memoryID := strings.TrimPrefix(surface, "meeting:")
			found, self := false, false
			for _, a := range l.Artifacts {
				if a.MemoryID != memoryID || a.Channel != "calendar" {
					continue
				}
				found = true
				for _, to := range a.Messages[0].To {
					self = self || to == l.Self.ID
				}
			}
			if !found || !self {
				return ruleError(RuleSelfAttendee, "surface %q lacks a calendar event containing self", surface)
			}
		}
	}
	return nil
}

func validateDefectIsolation(l Ledger) error {
	byArtifact, commitments := map[string]int{}, map[string]bool{}
	for _, n := range l.NonObligations {
		byArtifact[n.Span.ArtifactID]++
	}
	for _, c := range l.Commitments {
		commitments[c.OpenedBy.ArtifactID] = true
	}
	for artifact, count := range byArtifact {
		if count > 1 || commitments[artifact] {
			return ruleError(RuleOneDefectArtifact, "artifact %q has %d defects or mixes labels", artifact, count)
		}
	}
	return nil
}

func validateBalance(l Ledger) error {
	open, selfOwned, surfaced := 0, 0, 0
	for _, c := range l.Commitments {
		if c.State != "open" || c.DuplicateOf != "" {
			continue
		}
		open++
		if c.Owner == l.Self.ID {
			selfOwned++
		}
		if len(c.ExpectedIn) > 0 {
			surfaced++
		}
	}
	if open < 2 || selfOwned*10 < open*4 || selfOwned*10 > open*6 || surfaced*10 < open*4 || surfaced*10 > open*6 {
		return ruleError(RuleClassBalance, "open class counts are unbalanced: open=%d self=%d surfaced=%d", open, selfOwned, surfaced)
	}
	return nil
}

func validatePersona(ids map[string]Identity) error {
	for id, p := range ids {
		for _, raw := range p.Emails {
			a, err := mail.ParseAddress(raw)
			if err != nil || !reservedDomain(strings.ToLower(strings.TrimSpace(strings.Split(a.Address, "@")[1]))) {
				return ruleError(RulePersonaHygiene, "identity %q has non-reserved email %q", id, raw)
			}
		}
		for _, handle := range p.Handles {
			if !fictionalHandle(handle) {
				return ruleError(RulePersonaHygiene, "identity %q has non-fictional handle %q", id, handle)
			}
		}
	}
	return nil
}

func reservedDomain(domain string) bool {
	switch domain {
	case "example.com", "example.org", "example.net":
		return true
	}
	return strings.HasSuffix(domain, ".invalid") || domain == "invalid" || strings.HasSuffix(domain, ".test") || domain == "test" || strings.HasSuffix(domain, ".example") || domain == "example"
}

func fictionalHandle(raw string) bool {
	if len(raw) != len("+15550100100") || !strings.HasPrefix(raw, "+1555010") {
		return false
	}
	line, err := strconv.Atoi(raw[len(raw)-4:])
	return err == nil && line >= 100 && line <= 199
}

func validateChannelGrain(l Ledger) error {
	for _, a := range l.Artifacts {
		switch a.Channel {
		case "gmail":
			if len(a.Participants) != 0 || len(a.Messages) == 0 {
				return ruleError(RuleChannelGrain, "gmail artifact %q has invalid grain", a.ID)
			}
			for _, m := range a.Messages {
				if len(m.To) == 0 {
					return ruleError(RuleChannelGrain, "gmail message %q has no To", m.ID)
				}
				for _, b := range m.Body {
					for _, line := range strings.Split(b.Text, "\n") {
						trim := strings.TrimSpace(line)
						if strings.HasPrefix(trim, ">") || (strings.HasPrefix(trim, "On ") && strings.HasSuffix(trim, "wrote:")) {
							return ruleError(RuleChannelGrain, "gmail block %q contains connector-stripped quote syntax", b.ID)
						}
					}
				}
			}
		case "imessage":
			if len(a.Participants) == 0 || len(a.Messages) == 0 {
				return ruleError(RuleChannelGrain, "imessage artifact %q lacks participants/messages", a.ID)
			}
			for _, m := range a.Messages {
				if len(m.To) != 0 || len(m.Cc) != 0 {
					return ruleError(RuleChannelGrain, "imessage message %q has To/Cc", m.ID)
				}
			}
		case "calendar", "notes":
			if len(a.Participants) != 0 || len(a.Messages) != 1 {
				return ruleError(RuleChannelGrain, "%s artifact %q must have exactly one message", a.Channel, a.ID)
			}
		default:
			return ruleError(RuleChannelGrain, "unknown channel %q", a.Channel)
		}
	}
	return nil
}
