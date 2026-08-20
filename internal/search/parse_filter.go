package search

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Catalog supplies connector-family normalization and support facts without coupling search to composition wiring.
type Catalog struct {
	Normalize   func(string) string
	Known       func(string) bool
	Types       func() []string
	Unsupported func(string) (string, bool)
}

const maxSinceHours = math.MaxInt64 / int64(time.Hour)

func ParseFilter(args map[string]any, now time.Time, catalog Catalog) (Filter, error) {
	f := Filter{Now: now}
	if raw, ok := args["source"]; ok {
		s, ok := raw.(string)
		if !ok {
			return Filter{}, fmt.Errorf("source must be a string, got %T", raw)
		}
		// Parse/validate on the TRIMMED value (surrounding whitespace is not
		// part of the grammar), but keep f.Source as the ORIGINAL, untrimmed
		// string — the "filters" receipt promises to echo back exactly what
		// the caller sent (docs/architecture/06-mcp-server.md's "Response
		// receipt" bullet), and silently trimming it before storage would
		// break that promise for a caller who (deliberately or not) sent
		// leading/trailing whitespace.
		family, instance, err := ParseSource(strings.TrimSpace(s), catalog)
		if err != nil {
			return Filter{}, err
		}
		f.Source = s
		f.SourceFamily, f.SourceInstance = family, instance
	}
	if raw, ok := args["since_hours"]; ok {
		n, ok := raw.(float64)
		if !ok {
			return Filter{}, fmt.Errorf("since_hours must be a positive integer, got %T", raw)
		}
		if n != math.Trunc(n) || n <= 0 {
			return Filter{}, fmt.Errorf("since_hours must be a positive integer, got %v", raw)
		}
		// Fail-closed overflow guard, checked on the RAW float64 BEFORE any int
		// conversion or duration arithmetic: since_hours ultimately feeds
		// time.Duration(hours) * time.Hour (sqlPredicate/passes), and
		// time.Duration is int64 nanoseconds. A since_hours large enough to
		// overflow that multiplication would wrap to an unpredictable —
		// possibly negative — duration, silently disabling or INVERTING the
		// filter (the worst failure mode: a caller who asked to narrow the
		// result set instead gets an unfiltered or backwards one with no
		// error). maxSinceHours is NOT an invented product ceiling — issue
		// #241 specifies since_hours as any positive integer, so a large-but-
		// representable value (e.g. 100000 hours, ~11.4 years) must stay
		// valid. It is derived directly from the arithmetic's own bound (see
		// maxSinceHours' doc comment): anything at or beyond it is an
		// explicit tool error, never a silently saturated/clamped value.
		if n > float64(maxSinceHours) {
			return Filter{}, fmt.Errorf("since_hours=%v exceeds the maximum representable value of %d hours (the int64-nanosecond time.Duration bound) — omit the filter for no time bound", raw, maxSinceHours)
		}
		f.SinceHours = int(n)
	}
	return f, nil
}

func ParseSource(s string, catalog Catalog) (family, instance string, err error) {
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		family = parts[0]
	case 2:
		family, instance = parts[0], parts[1]
		if instance == "" {
			return "", "", fmt.Errorf("source %q has an empty account instance after ':' — expected type or type:account", s)
		}
	default:
		return "", "", fmt.Errorf("source %q is malformed — expected type or type:account (a single colon), got %d colon-separated parts", s, len(parts)-1)
	}
	if family == "" {
		return "", "", fmt.Errorf("source must not be empty")
	}
	family = catalog.Normalize(family)
	if !catalog.Known(family) {
		return "", "", fmt.Errorf("unknown source %q — expected a connector type (%s) or type:account",
			s, strings.Join(catalog.Types(), ", "))
	}
	// #241 integrator decision (filesystem family audit): a family that is a
	// real catalog connector but whose memories carry no per-item Provider
	// identity would otherwise be ACCEPTED yet NEVER MATCH ANYTHING — a
	// silent-wrong-answer trap indistinguishable from "no matches this call"
	// (see unsupportedSourceFamilies' own doc comment for the full
	// rationale). Reject it explicitly, fail closed, for BOTH the bare
	// family and any family:instance under it.
	if reason, unsupported := catalog.Unsupported(family); unsupported {
		return "", "", fmt.Errorf("source %q is unsupported: %s", s, reason)
	}
	return family, instance, nil
}
