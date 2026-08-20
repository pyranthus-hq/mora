package mora

import (
	"context"
	"database/sql"
	searchpkg "github.com/pyranthus-hq/mora/internal/search"
)

type mmrParams struct {
	lambda float64
	force  bool
}

const defaultLambda = searchpkg.DefaultMMRLambda

func mmrRerank(ids []string, rel map[string]float64, vectors map[string][]float32, p mmrParams) []string {
	return searchpkg.MMRRerank(ids, rel, vectors, p.lambda)
}
func mmrActive(useVec bool, p *mmrParams) bool { return searchpkg.MMRActive(useVec, p.force) }
func loadVectorsByID(ctx context.Context, db *sql.DB, model string, ids []string) (map[string][]float32, error) {
	return searchpkg.LoadVectorsByID(ctx, db, model, ids)
}
