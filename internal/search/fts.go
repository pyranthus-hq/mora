package search

import (
	"context"
	"database/sql"
	"strings"

	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// FTSResult is the parent-grain FTS query result and the widened pool limit used.
type FTSResult struct {
	Memories  []memory.Memory
	ParentIDs []string
	PoolLimit int
}

// ExecuteFTS queries the canonical memories FTS join with pre-rank scope/source/time filtering.
func ExecuteFTS(ctx context.Context, db *sql.DB, query, scope string, limit int, filter Filter) (FTSResult, error) {
	match := FTSQuery(query)
	if strings.TrimSpace(match) == "" {
		return FTSResult{}, nil
	}
	sqlLimit := limit
	if limit > 0 {
		sqlLimit = limit * 5
		if sqlLimit < 50 {
			sqlLimit = 50
		}
	}
	sqlq := `SELECT m.id, m.scope, m.type, m.title, m.tags, m.source, m.created_at, m.path, m.text, bm25(memories_fts) AS score FROM memories_fts JOIN memories m ON m.id = memories_fts.id WHERE memories_fts MATCH ?`
	args := []any{match}
	if scope != "" {
		sqlq += ` AND m.scope = ?`
		args = append(args, scope)
	}
	if clause, fargs := filter.SQLPredicate(); clause != "" {
		sqlq += clause
		args = append(args, fargs...)
	}
	sqlq += ` ORDER BY score, m.id LIMIT ?`
	args = append(args, sqlLimit)
	rows, err := db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return FTSResult{}, err
	}
	defer rows.Close()
	var out []memory.Memory
	for rows.Next() {
		var m memory.Memory
		var tags string
		if err := rows.Scan(&m.ID, &m.Scope, &m.Type, &m.Title, &tags, &m.Source, &m.CreatedAt, &m.Path, &m.Text, &m.Score); err != nil {
			return FTSResult{}, err
		}
		m.Tags = genericutil.SplitCSV(tags)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return FTSResult{}, err
	}
	ids := make([]string, len(out))
	for i, m := range out {
		ids[i] = m.ID
	}
	return FTSResult{Memories: out, ParentIDs: ids, PoolLimit: sqlLimit}, nil
}
