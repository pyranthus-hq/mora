package mora

// companion_readonly.go carries the read-only signal the companion listener
// puts on every kernel call (graph node N12).
//
// # Why a kernel signal rather than a check at the boundary
//
// Mora's read paths repair themselves. An absent index is rebuilt, a
// schema-stale one is rebuilt, a corrupt subscribed-share index is re-cut into
// a fresh repair generation and published. Every one of those is right for a
// human at a terminal who asked a question and would rather wait than be told
// no — and every one is minutes of disk and CPU over the whole vault, reachable
// from one authenticated HTTP request the moment a listener exists.
//
// The previous shape asked "is the index usable?" at the companion boundary and
// called the kernel only when the answer was yes. That is a guess about which
// paths repair, and it was wrong twice: `meeting_prep` reached ensureIndexDB
// through the commitment inventory, and `think` reached healShareIndex through
// a subscribed corpus. It also raced — the index could be deleted between the
// check and the call.
//
// So the property moves to where it is decided. A context carrying this marker
// means "answer from what exists; never repair", the repair sites honor it by
// returning ErrReadOnlyRepairNeeded instead of writing, and the caller decides
// what a refusal means. A boundary cannot be wrong about a path it does not
// know exists, because the path itself refuses.
//
// # Scope
//
// Only the companion listener sets it. Every other caller — the CLI, MCP, the
// generic loopback API — leaves it unset and reaches exactly the code it did
// before, which is what TestReadOnlyIsOffForEveryOtherCaller pins.

import (
	"context"
	"errors"
)

// ErrReadOnlyRepairNeeded reports that a read could only have been served by
// repairing durable state, and the caller asked for a read.
//
// It is a sentinel rather than a message because callers branch on it: the
// companion listener turns it into an honest "the vault cannot answer this
// right now" projection, and nothing else should turn it into a 500.
var ErrReadOnlyRepairNeeded = errors.New("mora: this read needs a durable repair, and the caller asked for a read-only answer")

type readOnlyKey struct{}

// withReadOnly marks ctx as a read-only call.
//
// It is unexported and set in exactly one place (the companion Reader), so the
// set of callers that can turn repair off is a grep rather than a policy.
func withReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyKey{}, true)
}

// readOnlyCall reports whether ctx forbids a durable repair.
//
// A nil context reads as NOT read-only. That is the fail-safe direction here:
// this function guards a repair, and the callers that pass a nil or background
// context are the CLI and the test harness, for whom repairing is the correct
// and long-standing behavior. The companion listener always threads a real
// context, so its side of the guard cannot be lost to a nil.
func readOnlyCall(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	on, _ := ctx.Value(readOnlyKey{}).(bool)
	return on
}
