package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Layout names the disposable directories for one runner root.
type Layout struct {
	Root        string
	Repo        string
	Config      string
	Vault       string
	Index       string
	HarnessHome string
	Contender   string // contender-visible workspace only
	OracleRef   string // hash-ref staging; never mounted into Contender
	Artifacts   string // NOT reset between trials (receipts/outputs accumulate)
	PrivateAdm  string // admitted private package staging (outside public fixtures)
}

// mutableDirs are restored from the preserved snapshot on every Reset.
func (l Layout) mutableDirs() []string {
	return []string{l.Repo, l.Config, l.Vault, l.Index, l.HarnessHome, l.Contender, l.OracleRef}
}

// InitialHashes records clean-state digests verified after reset.
type InitialHashes struct {
	Repo        string `json:"repo_sha256"`
	Config      string `json:"config_sha256"`
	Vault       string `json:"vault_sha256"`
	Index       string `json:"index_sha256"`
	HarnessHome string `json:"harness_home_sha256"`
	Contender   string `json:"contender_sha256"`
}

// Workspace is a resettable disposable trial environment.
type Workspace struct {
	Layout Layout
	// preserved holds pristine copies of mutable dirs used to reset between trials.
	preserved string
	baseline  InitialHashes
}

// PrepareWorkspace creates a disposable layout under parentDir and seeds
// pristine copies for reset. parentDir should be a test temp dir or an
// explicit opt-in path — never a production vault/home.
func PrepareWorkspace(parentDir string) (*Workspace, error) {
	if parentDir == "" {
		return nil, errf(CodeWorkspace, "parent", "parent directory is required")
	}
	if looksLikeProductionPath(parentDir) {
		return nil, errf(CodePathRejected, "parent", "refusing production-looking path %q", parentDir)
	}
	root := filepath.Join(parentDir, "pilotreplay-workspace")
	preserved := filepath.Join(parentDir, "pilotreplay-preserved")
	for _, d := range []string{root, preserved} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, errf(CodeWorkspace, "mkdir", "%v", err)
		}
	}
	layout := Layout{
		Root:        root,
		Repo:        filepath.Join(root, "repo"),
		Config:      filepath.Join(root, "config"),
		Vault:       filepath.Join(root, "vault"),
		Index:       filepath.Join(root, "index"),
		HarnessHome: filepath.Join(root, "harness-home"),
		Contender:   filepath.Join(root, "contender"),
		OracleRef:   filepath.Join(root, "oracle-ref"),
		Artifacts:   filepath.Join(root, "artifacts"),
		PrivateAdm:  filepath.Join(root, "private-admit"),
	}
	for _, d := range []string{
		layout.Repo, layout.Config, layout.Vault, layout.Index,
		layout.HarnessHome, layout.Contender, layout.OracleRef,
		layout.Artifacts, layout.PrivateAdm,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, errf(CodeWorkspace, "mkdir", "%v", err)
		}
	}
	seeds := map[string]string{
		filepath.Join(layout.Repo, "COMMIT"):             "SEED\n",
		filepath.Join(layout.Config, "runner.json"):      "{}\n",
		filepath.Join(layout.Vault, "README.synth"):      "synthetic disposable vault — not production\n",
		filepath.Join(layout.Index, "index.marker"):      "empty\n",
		filepath.Join(layout.HarnessHome, "home.marker"): "synthetic harness home\n",
		filepath.Join(layout.Contender, ".keep"):         "",
	}
	for path, body := range seeds {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return nil, errf(CodeWorkspace, "seed", "%v", err)
		}
	}
	ws := &Workspace{Layout: layout, preserved: preserved}
	if err := ws.snapshotPreserved(); err != nil {
		return nil, err
	}
	hashes, err := ws.ComputeHashes()
	if err != nil {
		return nil, err
	}
	ws.baseline = hashes
	return ws, nil
}

func looksLikeProductionPath(p string) bool {
	// Treat both separator styles conservatively on every host. ToSlash alone
	// does not normalize Windows input on Unix. Add boundaries so a relative path or parent
	// ending in .mora is rejected even after Clean removes its trailing slash.
	clean := filepath.Clean(strings.ReplaceAll(p, `\`, "/"))
	lower := "/" + strings.ToLower(filepath.ToSlash(clean)) + "/"
	denySubstrings := []string{
		"/library/application support/mora",
		"/.mora/",
		"/mora/vault",
		"/credentials.json",
		"/client_secret",
	}
	for _, s := range denySubstrings {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

func (w *Workspace) snapshotPreserved() error {
	if err := os.RemoveAll(w.preserved); err != nil {
		return errf(CodeWorkspace, "preserved", "%v", err)
	}
	if err := os.MkdirAll(w.preserved, 0o755); err != nil {
		return errf(CodeWorkspace, "preserved", "%v", err)
	}
	for _, dir := range w.Layout.mutableDirs() {
		name := filepath.Base(dir)
		if err := copyDir(dir, filepath.Join(w.preserved, name)); err != nil {
			return err
		}
	}
	return nil
}

// Baseline returns the clean-state hashes recorded at PrepareWorkspace.
func (w *Workspace) Baseline() InitialHashes { return w.baseline }

// ComputeHashes digests the mutable trial directories.
func (w *Workspace) ComputeHashes() (InitialHashes, error) {
	h := InitialHashes{}
	var err error
	if h.Repo, err = dirDigest(w.Layout.Repo); err != nil {
		return h, err
	}
	if h.Config, err = dirDigest(w.Layout.Config); err != nil {
		return h, err
	}
	if h.Vault, err = dirDigest(w.Layout.Vault); err != nil {
		return h, err
	}
	if h.Index, err = dirDigest(w.Layout.Index); err != nil {
		return h, err
	}
	if h.HarnessHome, err = dirDigest(w.Layout.HarnessHome); err != nil {
		return h, err
	}
	if h.Contender, err = dirDigest(w.Layout.Contender); err != nil {
		return h, err
	}
	return h, nil
}

// Reset restores mutable disposable dirs from the preserved snapshot and
// verifies hashes match the baseline (no bleed from the previous trial).
// Artifacts and private-admit staging are retained across resets.
func (w *Workspace) Reset() (InitialHashes, error) {
	for _, dir := range w.Layout.mutableDirs() {
		name := filepath.Base(dir)
		src := filepath.Join(w.preserved, name)
		if err := os.RemoveAll(dir); err != nil {
			return InitialHashes{}, errf(CodeWorkspace, "reset", "%v", err)
		}
		if err := copyDir(src, dir); err != nil {
			return InitialHashes{}, err
		}
	}
	got, err := w.ComputeHashes()
	if err != nil {
		return InitialHashes{}, err
	}
	if got != w.baseline {
		return got, errf(CodeResetBleed, "hashes", "post-reset hashes diverge from baseline (repo=%s→%s vault=%s→%s contender=%s→%s)",
			w.baseline.Repo, got.Repo, w.baseline.Vault, got.Vault, w.baseline.Contender, got.Contender)
	}
	return got, nil
}

// VerifyCleanInitial asserts current hashes equal baseline without resetting.
func (w *Workspace) VerifyCleanInitial() error {
	got, err := w.ComputeHashes()
	if err != nil {
		return err
	}
	if got != w.baseline {
		return errf(CodeHashMismatch, "hashes", "workspace is not at clean initial state")
	}
	return nil
}

// ContenderHasOracleMaterial returns true if any oracle / gold / post-cutoff
// marker leaked into the contender workspace.
func (w *Workspace) ContenderHasOracleMaterial() (bool, []string, error) {
	var leaks []string
	err := filepath.WalkDir(w.Layout.Contender, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(w.Layout.Contender, path)
		lower := strings.ToLower(rel)
		for _, bad := range []string{"oracle", "gold", "expected_answer", "reference_patch", "post_cutoff", "hidden_test"} {
			if strings.Contains(lower, bad) {
				leaks = append(leaks, rel)
				break
			}
		}
		return nil
	})
	if err != nil {
		return false, nil, errf(CodeWorkspace, "walk", "%v", err)
	}
	return len(leaks) > 0, leaks, nil
}

// ListContenderWrites returns relative paths under contender (for containment checks).
func (w *Workspace) ListContenderWrites() ([]string, error) {
	var out []string
	err := filepath.WalkDir(w.Layout.Contender, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(w.Layout.Contender, path)
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, errf(CodeWorkspace, "walk", "%v", err)
	}
	sort.Strings(out)
	return out, nil
}

func dirDigest(root string) (string, error) {
	type entry struct {
		Rel  string
		Mode string
		Sum  string
	}
	var entries []entry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			entries = append(entries, entry{Rel: rel + "/", Mode: info.Mode().String(), Sum: ""})
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		entries = append(entries, entry{Rel: rel, Mode: info.Mode().String(), Sum: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return "", errf(CodeWorkspace, "digest", "%v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Rel < entries[j].Rel })
	payload, err := json.Marshal(entries)
	if err != nil {
		return "", errf(CodeWorkspace, "digest", "%v", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
