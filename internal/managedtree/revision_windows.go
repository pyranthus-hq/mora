//go:build windows

package managedtree

// Windows cannot observe POSIX executable bits. It accepts an imported revision
// when the path and bytes still match, retaining the manifest's executable
// metadata for consumers that understand it.
func sourceMatchesRevision(expected, actual Revision) bool {
	if len(expected.Files) != len(actual.Files) {
		return false
	}
	for i := range expected.Files {
		if expected.Files[i].Path != actual.Files[i].Path || expected.Files[i].SHA256 != actual.Files[i].SHA256 {
			return false
		}
	}
	return true
}
