package mora

import "errors"

// errOwnerVerificationUnavailable is deliberately stable for public routes.
// A terminal, environment marker, typed confirmation, or caller-supplied JSON
// cannot establish owner authority.
var errOwnerVerificationUnavailable = errors.New("ERR_OWNER_VERIFICATION_UNAVAILABLE: no trusted native owner verifier is available")

// ownerAction names the exact authority transition a future trusted adapter
// must bind. Production has no such adapter yet, so public authority routes
// fail closed before they can mutate state.
type ownerAction struct {
	Kind     string
	TargetID string
	Revision string
	Digest   string
}

var ownerActionVerifier = func(ownerAction) error { return errOwnerVerificationUnavailable }

func requireVerifiedOwnerAction(action ownerAction) error { return ownerActionVerifier(action) }
