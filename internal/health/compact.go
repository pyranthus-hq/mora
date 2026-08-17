package health

import (
	"encoding/json"
	"sort"
)

// CompactSourceCap is the maximum number of source states in a compact envelope.
const CompactSourceCap = 3

// CompactSourceBytesCap is the hard JSON byte cap for Compact.PerSource.
const CompactSourceBytesCap = 80

// Compact is the bounded health envelope carried by hot CLI, MCP, and HTTP surfaces.
type Compact struct {
	State          string            `json:"state"`
	Sources        string            `json:"sources"`
	PerSource      map[string]string `json:"per_source,omitempty"`
	SourcesOmitted int               `json:"sources_omitted,omitempty"`
	Index          string            `json:"index"`
	Banner         string            `json:"banner,omitempty"`
}

// ProjectCompact projects a canonical snapshot without consulting time or storage.
func ProjectCompact(h Health) Compact {
	sources := Fresh
	if w := Worst(h.Sources); w != nil {
		sources = w.State
	}
	var perSource map[string]string
	var omitted int
	if len(h.Sources) > 0 {
		srcs := append([]Source(nil), h.Sources...)
		sort.Slice(srcs, func(i, j int) bool {
			ri, rj := StateRank(srcs[i].State), StateRank(srcs[j].State)
			if ri != rj {
				return ri < rj
			}
			if srcs[i].AgeHours != srcs[j].AgeHours {
				return srcs[i].AgeHours > srcs[j].AgeHours
			}
			return srcs[i].Key < srcs[j].Key
		})
		candidate := make(map[string]string)
		for _, src := range srcs {
			if len(candidate) >= CompactSourceCap {
				omitted++
				continue
			}
			candidate[src.Key] = src.State
			body, err := json.Marshal(candidate)
			if err != nil || len(body) > CompactSourceBytesCap {
				delete(candidate, src.Key)
				omitted++
				continue
			}
		}
		if len(candidate) > 0 {
			perSource = candidate
		}
	}
	return Compact{State: h.State, Sources: sources, PerSource: perSource, SourcesOmitted: omitted, Index: h.Index.State, Banner: BannerAll(h)}
}

// FromParts assembles and aggregates a canonical snapshot from already-pinned arms.
func FromParts(sources []Source, index Index, producers []Producer) Health {
	h := Health{Sources: sources, Index: index, Producers: producers}
	h.State = AggregateState(h)
	return h
}
