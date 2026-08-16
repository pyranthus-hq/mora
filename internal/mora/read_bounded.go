package mora

import mcppkg "github.com/pyranthus-hq/mora/internal/mcp"

type boundedReadReceipt = mcppkg.BoundedReadReceipt

func boundedReadRequested(args map[string]any) bool { return mcppkg.BoundedReadRequested(args) }
func applyBoundedRead(m Memory, args map[string]any) (Memory, boundedReadReceipt) {
	return mcppkg.ApplyBoundedRead(m, args)
}
