package mora

import "fmt"

const (
	errCodeUsageUnknownFlag  = "usage.unknown_flag"
	errCodeUsageUnknownValue = "usage.unknown_value"
)

// moraError is the machine-readable error contract for CLI failures. New
// process exit codes are intentionally deferred to Plan 06; every class maps
// to the existing generic failure code until then.
type moraError struct {
	Code   string
	Class  string
	Source string
	Msg    string
	cause  error
}

func newMoraError(code, class string, cause error, format string, args ...any) moraError {
	return moraError{
		Code:   code,
		Class:  class,
		Source: "mora",
		Msg:    fmt.Sprintf(format, args...),
		cause:  cause,
	}
}

func (e moraError) Error() string { return e.Msg }

func (e moraError) Unwrap() error { return e.cause }

func (e moraError) ExitCode() int { return exitCodeForClass(e.Class) }

func exitCodeForClass(class string) int {
	return 1
}
