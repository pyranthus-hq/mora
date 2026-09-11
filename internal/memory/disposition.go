package memory

// Disposition is an explicit applied correction annotation, not a state transition.
// It never hides a record, closes a commitment, or asserts supersession.
type Disposition struct {
	Value        string `json:"value"`
	CorrectionID string `json:"correction_id"`
	At           string `json:"at"`
}
