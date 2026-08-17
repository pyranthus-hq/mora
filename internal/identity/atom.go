package identity

// Atom is a stable provider identity used by governance and derived projections.
type Atom struct {
	Provider string `json:"provider,omitempty"`
	Kind     string `json:"kind"`
	Value    string `json:"value"`
}
