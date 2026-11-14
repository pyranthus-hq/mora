//go:build windows

package mora

// nativeOwnerWindowsHelperSHA256 is written by apps/mora-review/windows/pin-helper.ps1
// after the final (optionally Authenticode-signed) PE is built. An empty value is
// intentionally read-only for developer builds.
const nativeOwnerWindowsHelperSHA256 = ""
const nativeOwnerWindowsHelperFilename = "MoraReview.exe"
