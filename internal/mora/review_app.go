package mora

import (
	"context"
	"errors"
	"fmt"
	"io"
)

const reviewAppUsage = "usage: mora review app-keep <saved|held> <id> <owner-digest> (Mora Status only)"

// verifyStatusAppCaller reports whether this process was started by the
// collector inside the signed Mora Status app, while that app is running. A
// variable so tests can stand in for the platform check.
var verifyStatusAppCaller = statusAppCaller

// cmdReviewAppKeep is the one entry point that lets Mora Status make an agent
// note owner-confirmed. The app shows every field the digest binds, then asks
// macOS for Touch ID or the login password before it calls this. Mora cannot
// see that prompt, so it checks the next best thing: the calling processes are
// the app's own signed collector and the signed app itself (checked by pid, so
// a copied path or a renamed binary does not pass). A shell or an agent that
// runs the collector directly fails the check, because the collector's parent
// is then not the app. The digest must equal the note's current digest, and
// the keep writes from the same read, so a note that changed after the app
// showed it is refused.
func cmdReviewAppKeep(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) != 3 || (args[0] != reviewKindSaved && args[0] != reviewKindHeld) || args[2] == "" {
		return errors.New(reviewAppUsage)
	}
	kind, id, digest := args[0], args[1], args[2]
	if err := refuseInAgentSession("mora review app-keep"); err != nil {
		return err
	}
	if err := refuseDashLedPositional("review app-keep", "note id", id); err != nil {
		return err
	}
	if err := verifyStatusAppCaller(); err != nil {
		return fmt.Errorf("refusing mora review app-keep: %v. Only the signed Mora Status app can use it; in a terminal, run mora review", err)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	n, err := loadReviewNote(cfg, kind, id)
	if err != nil {
		return err
	}
	if n.Confirm != digest {
		return fmt.Errorf("note %s changed after it was shown; nothing was done", rulesShow(id))
	}
	if kind == reviewKindHeld {
		defer drainReconcileOrWarn(stderr)
	}
	return reviewAct(ctx, cfg, n, true, stdout, stderr)
}
