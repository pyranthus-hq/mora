package mora

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

func mcpProjectTestURI(dir string) string {
	slash := filepath.ToSlash(dir)
	if !strings.HasPrefix(slash, "/") {
		slash = "/" + slash
	}
	u := url.URL{Scheme: "file", Path: slash}
	return u.String()
}

func TestMCPProjectRootsResolveOneExistingWorkspace(t *testing.T) {
	cfg := mcpProjectSetup(t)
	parent := t.TempDir()
	a := filepath.Join(parent, "Project A")
	child := filepath.Join(a, "child")
	b := filepath.Join(parent, "Project B")
	other := filepath.Join(parent, "Unbound")
	for _, dir := range []string{child, b, other} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	aRoot, _, err := bindProjectDir(testCtx(t), a, "project:a")
	if err != nil {
		t.Fatal(err)
	}
	bRoot, _, err := bindProjectDir(testCtx(t), b, "project:b")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(a, "note.txt")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		uris []string
		want mcpProject
		err  error
	}{
		{"spaces", []string{mcpProjectTestURI(a)}, mcpProject{scope: "project:a", root: aRoot}, nil},
		{"child and duplicate", []string{mcpProjectTestURI(child), mcpProjectTestURI(a), mcpProjectTestURI(child)}, mcpProject{scope: "project:a", root: aRoot}, nil},
		{"other session", []string{mcpProjectTestURI(b)}, mcpProject{scope: "project:b", root: bRoot}, nil},
		{"different projects", []string{mcpProjectTestURI(a), mcpProjectTestURI(b)}, mcpProject{}, errMCPProjectRootsAmbiguous},
		{"empty", nil, mcpProject{}, errMCPProjectRootsUnbound},
		{"unknown", []string{mcpProjectTestURI(filepath.Join(a, "missing"))}, mcpProject{}, errMCPProjectRootsUnbound},
		{"file", []string{mcpProjectTestURI(file)}, mcpProject{}, errMCPProjectRootsUnbound},
		{"mixed known and unknown", []string{mcpProjectTestURI(a), mcpProjectTestURI(other)}, mcpProject{}, errMCPProjectRootsUnbound},
		{"remote", []string{"file://server/share/fixture"}, mcpProject{}, errMCPProjectRootURI},
		{"escaped UNC", []string{"file:///%2Fserver/share/fixture"}, mcpProject{}, errMCPProjectRootURI},
		{"escaped backslash", []string{"file:///%5Cserver/share/fixture"}, mcpProject{}, errMCPProjectRootURI},
		{"malformed escape", []string{"file:///some%zz"}, mcpProject{}, errMCPProjectRootURI},
		{"relative", []string{"file:workspace"}, mcpProject{}, errMCPProjectRootURI},
		{"encoded traversal", []string{"file:///one/%2e%2e/two"}, mcpProject{}, errMCPProjectRootURI},
		{"query", []string{"file:///one?value=x"}, mcpProject{}, errMCPProjectRootURI},
		{"fragment", []string{"file:///one#x"}, mcpProject{}, errMCPProjectRootURI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var roots []mcppkg.Root
			for _, uri := range tc.uris {
				roots = append(roots, mcppkg.Root{URI: uri, Name: "untrusted name"})
			}
			got, err := resolveMCPProjectRoots(cfg, roots)
			if got != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("resolve = %+v, %v; want %+v, %v", got, err, tc.want, tc.err)
			}
		})
	}
	if _, _, err := bindProjectDir(testCtx(t), b, "project:a"); err != nil {
		t.Fatal(err)
	}
	got, err := resolveMCPProjectRoots(cfg, []mcppkg.Root{{URI: mcpProjectTestURI(a)}, {URI: mcpProjectTestURI(b)}})
	if got != (mcpProject{}) || !errors.Is(err, errMCPProjectRootsAmbiguous) {
		t.Fatalf("same scope on different roots = %+v, %v", got, err)
	}
}

func TestMCPProjectRootsRejectCorruptBindingStore(t *testing.T) {
	cfg := mcpProjectSetup(t)
	if err := os.WriteFile(projectBindingsPath(cfg), []byte(`{"bindings": ["private config"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveMCPProjectRoots(cfg, []mcppkg.Root{{URI: mcpProjectTestURI(t.TempDir())}})
	if got != (mcpProject{}) || err != errMCPProjectRootBindings {
		t.Fatalf("invalid store = %+v, %v", got, err)
	}
}

func TestMCPProjectRootsDoNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires Windows privileges not assumed by this test")
	}
	cfg := mcpProjectSetup(t)
	parent := t.TempDir()
	bound := filepath.Join(parent, "bound")
	if err := os.Mkdir(bound, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bindProjectDir(testCtx(t), bound, "project:bound"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(bound, link); err != nil {
		t.Fatal(err)
	}
	got, err := resolveMCPProjectRoots(cfg, []mcppkg.Root{{URI: mcpProjectTestURI(link)}})
	if got != (mcpProject{}) || err != errMCPProjectRootsUnbound {
		t.Fatalf("workspace through symlink = %+v, %v", got, err)
	}
}

func TestMCPProjectRootsNativeWindowsURI(t *testing.T) {
	got, err := mcpProjectRootPath("file://localhost/C:/Fixture%20Project", "windows")
	if err != nil || got != filepath.FromSlash("C:/Fixture Project") {
		t.Fatalf("Windows file URI = %q, %v", got, err)
	}
	for _, uri := range []string{"file:///C:relative", "file:///C:/workspace:stream", "file:///C:/one./two"} {
		if _, err := mcpProjectRootPath(uri, "windows"); err != errMCPProjectRootURI {
			t.Fatalf("Windows file URI %q should be refused, got %v", uri, err)
		}
	}
	if runtime.GOOS == "windows" {
		if _, err := mcpProjectRootPath("file:///C:/nul", "windows"); err != errMCPProjectRootURI {
			t.Fatalf("Windows device name should be refused, got %v", err)
		}
		cfg := mcpProjectSetup(t)
		work := t.TempDir()
		bound, _, err := bindProjectDir(testCtx(t), work, "project:win")
		if err != nil {
			t.Fatal(err)
		}
		got, err := resolveMCPProjectRoots(cfg, []mcppkg.Root{{URI: mcpProjectTestURI(work)}})
		if err != nil || got != (mcpProject{scope: "project:win", root: bound}) {
			t.Fatalf("Windows workspace = %+v, %v", got, err)
		}
	}
}

func TestMCPProjectRootsWSLDriveURI(t *testing.T) {
	skipWSLSimulationOnWindows(t)
	cfg := mcpProjectSetup(t)
	mount := t.TempDir()
	withWSLEnv(t, "linux", mount)
	withWSLDistro(t, "Ubuntu")
	work := filepath.Join(mount, "c", "Fixture Project")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	bound, _, err := bindProjectDir(testCtx(t), work, "project:wsl")
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveMCPProjectRoots(cfg, []mcppkg.Root{{URI: "file:///C:/Fixture%20Project"}})
	if err != nil || got != (mcpProject{scope: "project:wsl", root: bound}) {
		t.Fatalf("WSL workspace = %+v, %v", got, err)
	}
	if got, err := resolveMCPProjectRoots(cfg, []mcppkg.Root{{URI: "file:///D:/Not%20Mounted"}}); got != (mcpProject{}) || err != errMCPProjectRootsUnbound {
		t.Fatalf("unmounted WSL drive = %+v, %v", got, err)
	}
}
