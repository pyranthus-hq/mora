//go:build windows

package mora

// rulesSignersProtected is always false on Windows, where Mora does not check
// ownership. Importing a grant then needs a person at a terminal.
func rulesSignersProtected(string) bool { return false }
