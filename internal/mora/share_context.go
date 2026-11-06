package mora

import (
	"context"
	"fmt"
	"io"
)

// Subscribed shares on the session-start surfaces (pilot F5/F9).
//
// The query-shaped surfaces (search, think, and the generic query path of
// context) union every subscription's corpus into the ranked list through
// unionSharedResults, owner-attributed. The query-less surfaces (brief, and
// context with no query or a structured intent) have nothing to rank a shared
// corpus against, so they do not summarize shares. They say the shares exist,
// how many memories they hold, and which tools read them. Without that pointer
// a machine that only subscribes looks empty to an agent that starts with
// brief or context. With zero subscriptions every pointer is nil and every
// surface is byte-identical to its pre-share output.

// contextSearchWithShares is the generic context query: local hybrid
// retrieval, then the same owner-attributed share union that search and think
// use. With no subscription (or no shared hit) it returns the local rows
// unchanged.
func contextSearchWithShares(ctx context.Context, cfg Config, query, scope string, limit int, filters searchFilters) ([]Memory, error) {
	local, err := hybridSearch(ctx, cfg, query, scope, limit, filters)
	if err != nil {
		return nil, err
	}
	return unionSharedResults(ctx, cfg, local, query, scope, limit, filters)
}

// sharedCorpusSummary is one subscription as the pointer reports it.
type sharedCorpusSummary struct {
	Name     string `json:"name"`
	Memories int    `json:"memories"`
	Pulled   bool   `json:"pulled"`
}

// sharedCorporaPointer tells a caller that subscribed shares exist but were
// not read by this surface, and names the tools that do read them.
type sharedCorporaPointer struct {
	Subscriptions int                   `json:"subscriptions"`
	Memories      int                   `json:"memories"`
	Shares        []sharedCorpusSummary `json:"shares,omitempty"`
	Error         string                `json:"error,omitempty"`
	Note          string                `json:"note"`
}

// sharedCorporaPointerFor returns nil when there are no subscriptions. An
// unreadable share registry is reported inside the pointer rather than
// failing the brief: search and think already fail loudly on it, and doctor
// marks it critical, but a session-start brief should still show the user's
// own vault.
func sharedCorporaPointerFor(cfg Config) *sharedCorporaPointer {
	sf, err := loadShares(cfg)
	if err != nil {
		return &sharedCorporaPointer{
			Error: err.Error(),
			Note:  "The share registry could not be read, so subscribed shares may be missing from every surface. Run `mora doctor`.",
		}
	}
	if len(sf.Subscriptions) == 0 {
		return nil
	}
	p := &sharedCorporaPointer{Subscriptions: len(sf.Subscriptions)}
	for _, sub := range sf.Subscriptions {
		row := sharedCorpusSummary{Name: sub.Name}
		if commit, ok, _ := resolvePublishedCommit(cfg, sub.Name); ok {
			row.Memories = commit.Count
			row.Pulled = true
		}
		p.Memories += row.Memories
		p.Shares = append(p.Shares, row)
	}
	p.Note = fmt.Sprintf("This covers your own vault only. %s not summarized here. "+
		"Call search_memory, think, or context_memory with a query to read them; "+
		"each result is attributed to its share.", shareCountPhrase(p.Subscriptions, p.Memories))
	return p
}

// shareCountPhrase renders "1 subscribed share (1 memory) is" or
// "8 subscribed shares (1039 memories) are".
func shareCountPhrase(shares, memories int) string {
	noun, verb := "shares", "are"
	if shares == 1 {
		noun, verb = "share", "is"
	}
	mem := "memories"
	if memories == 1 {
		mem = "memory"
	}
	return fmt.Sprintf("%d subscribed %s (%d %s) %s", shares, noun, memories, mem, verb)
}

// printSharedCorporaPointer is the human-CLI form of the pointer. surface
// names what was not summarized ("this brief"); it prints nothing without
// subscriptions.
func printSharedCorporaPointer(w io.Writer, p *sharedCorporaPointer, surface string) {
	if p == nil {
		return
	}
	if p.Error != "" {
		fmt.Fprintf(w, "\nShared: the share registry could not be read (%s). Run `mora doctor`.\n", p.Error)
		return
	}
	pronoun := "them"
	if p.Subscriptions == 1 {
		pronoun = "it"
	}
	fmt.Fprintf(w, "\nShared: %s not summarized in %s. Read %s with `mora search`, `mora think`, or `mora context --query`.\n",
		shareCountPhrase(p.Subscriptions, p.Memories), surface, pronoun)
}
