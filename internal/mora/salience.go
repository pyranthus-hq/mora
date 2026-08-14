package mora

import (
	saliencepkg "github.com/pyranthus-hq/mora/internal/salience"
	"math"
	"strings"
)

func aggregatePersonSalience(mems []Memory) map[string]int64 {
	// Pass 1: vault-relative recency anchor.
	vaultMax := ""
	for _, m := range mems {
		if m.DeletedAt != "" {
			continue
		}
		if vf := validFromOf(m); vf > vaultMax {
			vaultMax = vf
		}
	}

	// Pass 2: accumulate per-person evidence.
	inputs := map[string]*saliencepkg.Input{}
	for _, m := range mems {
		if m.DeletedAt != "" {
			continue
		}
		parts, _, _, _ := personRefs(m)
		n := len(parts)
		if n < 1 {
			continue
		}
		fanout := 1 / math.Sqrt(float64(n))
		weighted := float64(saliencepkg.MessageCount(m)) * fanout
		vf := validFromOf(m)
		for _, p := range parts {
			in, ok := inputs[p.id]
			if !ok {
				in = &saliencepkg.Input{
					Kind:             classifyIdentity(strings.TrimPrefix(p.id, "person:"), ""),
					PerChannelVolume: map[string]float64{},
					Channels:         map[string]bool{},
				}
				inputs[p.id] = in
			}
			in.PerChannelVolume[m.Type] += weighted
			in.Channels[m.Type] = true
			if vf > in.LastSeen {
				in.LastSeen = vf
			}
		}
	}

	out := make(map[string]int64, len(inputs))
	for id, in := range inputs {
		out[id] = saliencepkg.Score(*in, vaultMax)
	}
	return out
}
