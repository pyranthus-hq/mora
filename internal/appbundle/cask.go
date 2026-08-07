package appbundle

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// CaskAutoUpdatesReady is deliberately false until issue #291 lands. Keeping
// this gate in the generator prevents a release or CI edit from advertising an
// updater that Mora does not yet schedule or notify through.
const (
	CaskAutoUpdatesReady = false
	maxCaskChecksumBytes = int64(1 << 20)
)

var (
	caskTagPattern    = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	caskSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// GenerateCask renders the repository-owned Homebrew Cask for the signed,
// post-staple Mora.app assets. It is byte-stable for identical inputs.
//
// autoUpdates is explicit because Homebrew's stanza is a product promise, not
// formatting: true is refused until CaskAutoUpdatesReady is enabled by #291.
func GenerateCask(tag string, checksums io.Reader, autoUpdates bool) ([]byte, error) {
	match := caskTagPattern.FindStringSubmatch(tag)
	if match == nil {
		return nil, fmt.Errorf("release tag %q must be canonical vMAJOR.MINOR.PATCH", tag)
	}
	if autoUpdates && !CaskAutoUpdatesReady {
		return nil, fmt.Errorf("auto_updates requires Mora's scheduled updater (#291); refusing to advertise it")
	}
	version := strings.TrimPrefix(tag, "v")
	want := map[string]string{
		"amd64": fmt.Sprintf("mora_%s_darwin_amd64_app.zip", version),
		"arm64": fmt.Sprintf("mora_%s_darwin_arm64_app.zip", version),
	}
	found := make(map[string]string, len(want))
	limited := &io.LimitedReader{R: checksums, N: maxCaskChecksumBytes + 1}
	scanner := bufio.NewScanner(limited)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("checksums-app.txt contains malformed line %q", line)
		}
		sha, name := fields[0], strings.TrimPrefix(fields[1], "*")
		if !caskSHA256Pattern.MatchString(sha) {
			return nil, fmt.Errorf("checksums-app.txt contains non-canonical SHA-256 for %q", name)
		}
		arch := ""
		for candidate, expected := range want {
			if name == expected {
				arch = candidate
				break
			}
		}
		if arch == "" {
			return nil, fmt.Errorf("checksums-app.txt contains unexpected asset %q", name)
		}
		if _, duplicate := found[arch]; duplicate {
			return nil, fmt.Errorf("checksums-app.txt contains duplicate asset %q", name)
		}
		found[arch] = sha
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading checksums-app.txt: %w", err)
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("checksums-app.txt exceeds %d bytes", maxCaskChecksumBytes)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if found[arch] == "" {
			return nil, fmt.Errorf("checksums-app.txt is missing %q", want[arch])
		}
	}

	var out bytes.Buffer
	fmt.Fprintln(&out, `# typed: strict`)
	fmt.Fprintln(&out, `# frozen_string_literal: true`)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, `cask "mora" do`)
	fmt.Fprintln(&out, `  arch arm: "arm64", intel: "amd64"`)
	fmt.Fprintln(&out)
	fmt.Fprintf(&out, "  version %q\n", version)
	fmt.Fprintf(&out, "  sha256 arm:   %q,\n", found["arm64"])
	fmt.Fprintf(&out, "         intel: %q\n", found["amd64"])
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, `  url "https://github.com/pyranthus-hq/mora/releases/download/v#{version}/mora_#{version}_darwin_#{arch}_app.zip"`)
	fmt.Fprintln(&out, `  name "Mora"`)
	fmt.Fprintln(&out, `  desc "Local-first, agent-agnostic memory CLI"`)
	fmt.Fprintln(&out, `  homepage "https://github.com/pyranthus-hq/mora"`)
	if autoUpdates {
		fmt.Fprintln(&out, `  auto_updates true`)
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, `  app "Mora.app"`)
	fmt.Fprintln(&out, `  binary "#{appdir}/Mora.app/Contents/MacOS/mora", target: "mora"`)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, `  preflight do`)
	fmt.Fprintln(&out, `    user_app = Pathname(Dir.home)/"Applications/Mora.app"`)
	fmt.Fprintln(&out, `    if user_app.exist?`)
	fmt.Fprintln(&out, `      odie "Mora.app already exists at #{user_app}. " \`)
	fmt.Fprintln(&out, `           "Remove it with Mora's signed-app uninstaller before installing the Homebrew Cask."`)
	fmt.Fprintln(&out, `    end`)
	fmt.Fprintln(&out, `  end`)
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, `  caveats <<~EOS`)
	fmt.Fprintln(&out, `    Mora preserves its vault, configuration, state, connector tokens, and backups on uninstall.`)
	fmt.Fprintln(&out, `    If another Mora.app, standalone mora binary, symlink, formula, or legacy Cask is installed,`)
	fmt.Fprintln(&out, `    remove that installation explicitly before retrying. This Cask never uses --adopt.`)
	fmt.Fprintln(&out, `  EOS`)
	fmt.Fprintln(&out, `end`)
	return out.Bytes(), nil
}
