package mora

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func emit(w io.Writer, v any, jsonOut bool) error {
	if jsonOut {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(b))
		return nil
	}
	sty := newStyler(w, jsonOut)
	switch x := v.(type) {
	case Memory:
		fmt.Fprintf(w, "%s\t%s\t%s\n", sty.dim(x.ID), sty.dim(x.Scope), ownedTitle(x))
	case []Memory:
		for _, m := range x {
			fmt.Fprintf(w, "%s\t%s\t%s\n", sty.dim(m.ID), sty.dim(m.Scope), ownedTitle(m))
		}
	case []catalogRow:
		for _, r := range x {
			// Off-path stays byte-identical ("enabled"/"disabled"); glyph + color
			// only appear on a real TTY.
			state := "disabled"
			if r.Enabled {
				state = "enabled"
			}
			if sty.on {
				if r.Enabled {
					state = sty.ok("● enabled")
				} else {
					state = sty.dim("○ disabled")
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", r.Type, r.Name, state)
		}
	default:
		fmt.Fprintf(w, "%v\n", v)
	}
	return nil
}

// refuseDashLedPositional closes the bug class Plans 01-05 and 01-07 each found
// a fresh instance of: a dash-led argument landing in a POSITIONAL slot.
// `tasks add --json` created a task named "--json", `loop begin --json` started
// a run for a loop of that name, and `sources add --json` registered a live
// source typed and named "--json" — three independent discoveries of one
// defect, each found only because somebody executed that exact path.
//
// The call sites parse their arguments very differently (flag.FlagSet with
// flagsFirst, hand-rolled loops, positional-then-flags splits), so a shared
// PARSER would not fit. The CHECK is identical everywhere, so this is the
// shared piece. Phase 01-10's matrix should assert the CLASS is closed by
// driving every positional-taking path, not that N instances were patched.
func refuseDashLedPositional(command, slot, value string) error {
	if !strings.HasPrefix(value, "-") {
		return nil
	}
	return newCodedError(errCodeUsageMissingArgument, nil,
		"%s: %q is a flag, not the %s — pass the %s first, or use `--` to end flag interpretation",
		command, value, slot, slot)
}
