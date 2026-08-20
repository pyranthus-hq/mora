package mora

import identitypkg "github.com/pyranthus-hq/mora/internal/identity"

func classifyIdentity(identity, displayName string) string {
	return identitypkg.Classify(identity, displayName)
}
func isPhoneNumber(handle string) bool     { return identitypkg.IsPhoneNumber(handle) }
func isStructuralNoise(handle string) bool { return identitypkg.IsStructuralNoise(handle) }
