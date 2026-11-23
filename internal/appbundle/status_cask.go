package appbundle

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var statusTagPattern = regexp.MustCompile(`^mora-status-v((0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*))$`)

// GenerateStatusCask renders the Homebrew Cask for the signed, post-staple
// Mora Status menu bar app (Apple silicon only). It is byte-stable for
// identical inputs and refuses any manifest that is not exactly one
// mora-status_<version>_arm64.zip line.
func GenerateStatusCask(tag string, checksums io.Reader) ([]byte, error) {
	match := statusTagPattern.FindStringSubmatch(tag)
	if match == nil {
		return nil, fmt.Errorf("release tag %q must be canonical mora-status-vMAJOR.MINOR.PATCH", tag)
	}
	version := match[1]
	want := fmt.Sprintf("mora-status_%s_arm64.zip", version)
	sha := ""
	limited := &io.LimitedReader{R: checksums, N: maxCaskChecksumBytes + 1}
	scanner := bufio.NewScanner(limited)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("checksums-status.txt contains malformed line %q", line)
		}
		name := strings.TrimPrefix(fields[1], "*")
		if !caskSHA256Pattern.MatchString(fields[0]) {
			return nil, fmt.Errorf("checksums-status.txt contains non-canonical SHA-256 for %q", name)
		}
		if name != want {
			return nil, fmt.Errorf("checksums-status.txt contains unexpected asset %q", name)
		}
		if sha != "" {
			return nil, fmt.Errorf("checksums-status.txt contains duplicate asset %q", name)
		}
		sha = fields[0]
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading checksums-status.txt: %w", err)
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("checksums-status.txt exceeds %d bytes", maxCaskChecksumBytes)
	}
	if sha == "" {
		return nil, fmt.Errorf("checksums-status.txt is missing %q", want)
	}

	var out bytes.Buffer
	fmt.Fprintln(&out, `# typed: strict`)
	fmt.Fprintln(&out, `# frozen_string_literal: true`)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, `cask "mora-status" do`)
	fmt.Fprintf(&out, "  version %q\n", version)
	fmt.Fprintf(&out, "  sha256 %q\n", sha)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, `  url "https://github.com/pyranthus-hq/mora/releases/download/mora-status-v#{version}/mora-status_#{version}_arm64.zip"`)
	fmt.Fprintln(&out, `  name "Mora Status"`)
	fmt.Fprintln(&out, `  desc "Menu bar status and agent-note review for Mora"`)
	fmt.Fprintln(&out, `  homepage "https://github.com/pyranthus-hq/mora"`)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, `  depends_on arch: :arm64`)
	fmt.Fprintln(&out, `  depends_on macos: :ventura`)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, `  app "Mora Status.app"`)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, `  caveats <<~EOS`)
	fmt.Fprintln(&out, `    Mora Status needs the mora command. It looks in ~/.local/bin, Homebrew, and Mora.app.`)
	fmt.Fprintln(&out, `    To use another vault: defaults write com.pyranthus.mora-status vault /absolute/path`)
	fmt.Fprintln(&out, `    Uninstalling removes only the app; your vault and Mora settings stay.`)
	fmt.Fprintln(&out, `  EOS`)
	fmt.Fprintln(&out, `end`)
	return out.Bytes(), nil
}
