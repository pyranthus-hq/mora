//go:build !windows

package managedtree

func sourceMatchesRevision(expected, actual Revision) bool { return sameRevision(expected, actual) }
