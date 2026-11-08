package rules

import "crypto/sha256"

const (
	GroupWarned = "warned"
	GroupSilent = "silent"
)

// Group is the Steward hash split. The unit is "<session>:<agent>", with
// "main" for the main thread. Steward read the first 8 hex digits of the
// sha256 as an integer and warned when it was even; that is the low bit of
// byte 3.
func Group(sessionID, agentID string) string {
	if agentID == "" {
		agentID = "main"
	}
	s := sha256.Sum256([]byte(sessionID + ":" + agentID))
	if s[3]&1 == 0 {
		return GroupWarned
	}
	return GroupSilent
}
