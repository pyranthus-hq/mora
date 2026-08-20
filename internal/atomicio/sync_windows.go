//go:build windows

package atomicio

// SyncDir is a no-op on Windows. Go's File.Sync there is FlushFileBuffers, which
// needs a write-access handle the read-only directory handle os.Open yields cannot
// provide. This is not a durability hole: NTFS renames go through MoveFileEx, whose
// metadata is journaled, so the directory entry is filesystem-ordered without an
// explicit parent-dir sync. Mirrors rename_windows.go's build-tag no-op half.
func SyncDir(string) error { return nil }
