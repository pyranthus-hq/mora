//go:build windows

package main

import "os/exec"

// The sync runner is installed on macOS and Linux/WSL only.
func cancelProcessTree(cmd *exec.Cmd) {}
