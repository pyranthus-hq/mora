package mora

import ()

// senderEmail builds an email memory FROM `from` (self-presented `name`) to `to`.
func senderEmail(id, from, name string, to ...string) Memory {
	meta := map[string]any{"from": []string{from}, "to": toAnySlice(to)}
	if name != "" {
		meta["names"] = map[string]string{from: name}
	}
	return Memory{ID: id, Scope: "personal", Type: "email", Title: id, CreatedAt: "2026-05-01T00:00:00Z", Text: "x", Meta: meta}
}

// TestA3MergeGmailInbox proves RULE 1: gmail dot/plus variants are the same inbox
// and collapse to ONE person entity carrying all address aliases, with a unioned
// (not summed) mention_count.

// TestA3MergeSharedNameWithEcho proves RULE 2: two DIFFERENT addresses that both
// self-present the same distinctive multi-token name AND whose addresses echo a
// name token merge into one person.

// TestA3PrecisionTwoDifferentPeopleSameName proves the precision guard: two
// DIFFERENT people sharing a full name whose addresses do NOT echo the name are
// NOT merged (the catastrophic-merge case codex warned about).

// TestA3PrecisionSharedFirstNameOnly proves the echo guard is NOT satisfied by a
// shared FIRST name: two different "Alex Morgan"s whose addresses echo only "alex"
// (no address spells the full name) must stay separate. (Regression for the review
// finding: first-name-only corroboration was fusing distinct humans.)

// TestA3PrecisionThreeWaySharedName proves three unrelated "Maria Garcia"s, each
// echoing only one name token (no address spells the full name), do NOT collapse.

// TestA3PrecisionSplitNameTokens proves two people sharing a full name where each
// address echoes a DIFFERENT single token (sam@ / jones@ for "Sam Jones") do not
// merge — there is no full-name anchor. (codex-found case.)

// TestA3MergeFullNameAnchored proves a cluster whose address spells the FULL name
// anchors the merge: riya.sharma@gmail (full) pulls in riya@acmeconsulting (first-name
// echo, same display name).

// entCarrying returns the id of the person entity whose aliases include addr.

// TestA3DoesNotMergeServiceByName proves RULE 2 only unions PERSON identities, so a
// service (bot) is never merged into a person by a coincidental shared name.

// TestA3MergeRewritesEdgesAndResolves proves merged entities keep all their edges
// (rewritten to the canonical id) so get_entity / co-occurrence resolve via ANY of
// the merged addresses, and that the graph stays deterministic across builds.
