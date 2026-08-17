package previewfilter

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/graph"
	"github.com/pyranthus-hq/mora/internal/graphstore"
	"sort"
	"strings"
)

type Resolution struct {
	Canonical string
	IDSet     map[string]bool
	OK        bool
	Ambiguous []string
}
type candidate struct {
	id, display, aliasesJSON string
	aliases                  []string
}

func ResolveEntity(ctx context.Context, db *sql.DB, name string) (Resolution, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Resolution{}, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT id, display_name, aliases, mention_count FROM entities WHERE id NOT LIKE 'memory:%'`)
	if err != nil {
		return Resolution{}, err
	}
	isAddr := strings.Contains(name, "@") || strings.HasPrefix(name, "+")
	wantID := graph.PersonID(name)
	var aliasHit *candidate
	var byName []candidate
	for rows.Next() {
		var c candidate
		var mention int
		if err := rows.Scan(&c.id, &c.display, &c.aliasesJSON, &mention); err != nil {
			rows.Close()
			return Resolution{}, err
		}
		if c.aliasesJSON != "" {
			_ = json.Unmarshal([]byte(c.aliasesJSON), &c.aliases)
		}
		if isAddr && AliasIDSet(c.id, c.aliases)[wantID] {
			cc := c
			aliasHit = &cc
		}
		if strings.EqualFold(c.display, name) || graphstore.AliasMatches(c.aliasesJSON, name) {
			byName = append(byName, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Resolution{}, err
	}
	if aliasHit != nil {
		return Resolution{Canonical: aliasHit.id, IDSet: AliasIDSet(aliasHit.id, aliasHit.aliases), OK: true}, nil
	}
	uniq := map[string]candidate{}
	for _, c := range byName {
		uniq[c.id] = c
	}
	switch len(uniq) {
	case 0:
		return Resolution{}, nil
	case 1:
		for _, c := range uniq {
			return Resolution{Canonical: c.id, IDSet: AliasIDSet(c.id, c.aliases), OK: true}, nil
		}
	}
	ids := make([]string, 0, len(uniq))
	for id := range uniq {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ambiguous := make([]string, 0, len(ids))
	for _, id := range ids {
		display := uniq[id].display
		if display == "" {
			display = strings.TrimPrefix(id, "person:")
		}
		ambiguous = append(ambiguous, display+" <"+strings.TrimPrefix(id, "person:")+">")
	}
	return Resolution{Ambiguous: ambiguous}, nil
}
