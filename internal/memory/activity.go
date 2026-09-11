package memory

import "time"

// Participation is an evidence-backed conversation fact. It is a read
// projection: absence means no retained validated message evidence. IsGroup is
// nil when connector metadata did not explicitly state whether it is a group.
type Participation struct {
	OwnShare             float64    `json:"own_share"`
	LastOwnAt            *time.Time `json:"last_own_at"`
	IsGroup              *bool      `json:"is_group"`
	LatestSender         string     `json:"latest_sender"`
	MessageEvidenceCount int        `json:"message_evidence_count"`
}
