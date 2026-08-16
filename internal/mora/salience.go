package mora

import graphpkg "github.com/pyranthus-hq/mora/internal/graph"

func aggregatePersonSalience(m []Memory) map[string]int64 { return graphpkg.AggregatePersonSalience(m) }
