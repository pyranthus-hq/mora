package memory

// DecisionValidity is the persisted validity contract for a decision memory.
type DecisionValidity struct {
	AsOf           string   `json:"as_of"`
	Durability     string   `json:"durability"`
	FlipConditions []string `json:"flip_conditions"`
	ReviewBy       string   `json:"review_by,omitempty"`
	Complete       bool     `json:"complete"`
}
