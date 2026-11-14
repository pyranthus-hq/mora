package mora

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

var (
	errMCPProjectRootURI        = errors.New("MCP workspace root is not a supported local file URI")
	errMCPProjectRootsUnbound   = errors.New("MCP workspace roots do not identify a bound project")
	errMCPProjectRootsAmbiguous = errors.New("MCP workspace roots identify different project bindings")
	errMCPProjectRootBindings   = errors.New("MCP workspace project bindings are unavailable")
)

// resolveMCPProjectRoots uses client-asserted roots to choose workspace context.
// Roots confer no authority. A caller must prefer an explicit --project-dir,
// restrict discovery to an authorized local client, collapse errors before
// returning them to the client, and resolve again when the roots change.
//
// Every root must agree. Explicit network paths and paths through symlinks or
// Windows reparse points (including junctions) are rejected. Filesystem case
// variants must match by identity. Other aliases, including differing Unicode
// normalization, may fail closed. The checks do not establish POSIX mount
// locality or protect against concurrent remounts. WSL drive conversion does
// not verify the underlying Windows drive type or apply Windows device-name
// rules to the mounted POSIX path.
func resolveMCPProjectRoots(cfg Config, roots []mcppkg.Root) (mcpProject, error) {
	if len(roots) == 0 {
		return mcpProject{}, errMCPProjectRootsUnbound
	}
	dirs := make([]string, 0, len(roots))
	for _, root := range roots {
		dir, err := mcpProjectRootPath(root.URI, wslHostOS)
		if err != nil {
			return mcpProject{}, err
		}
		dirs = append(dirs, dir)
	}

	for _, dir := range dirs {
		if !mcpRootDirectoryChain(dir) {
			return mcpProject{}, errMCPProjectRootsUnbound
		}
	}
	bindings, err := loadProjectBindings(cfg)
	if err != nil {
		return mcpProject{}, errMCPProjectRootBindings
	}
	var selected mcpProject
	for _, dir := range dirs {
		project, err := mcpProjectForLocalRoot(bindings, dir)
		if err != nil {
			return mcpProject{}, err
		}
		if selected != (mcpProject{}) && selected != project {
			return mcpProject{}, errMCPProjectRootsAmbiguous
		}
		selected = project
	}
	return selected, nil
}

func mcpProjectRootPath(raw, hostOS string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" || !strings.HasPrefix(raw[len("file:"):], "//") || u.User != nil ||
		(u.Host != "" && !strings.EqualFold(u.Host, "localhost")) || strings.ContainsAny(raw, "?#") {
		return "", errMCPProjectRootURI
	}
	p := u.Path
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") ||
		strings.ContainsRune(p, '\\') || !utf8.ValidString(p) || strings.ContainsFunc(p, unicode.IsControl) {
		return "", errMCPProjectRootURI
	}
	drive := len(p) >= 4 && isASCIILetter(p[1]) && p[2:4] == ":/"
	if hostOS == "windows" && !drive {
		return "", errMCPProjectRootURI
	}
	for i, component := range strings.Split(strings.TrimSuffix(p[1:], "/"), "/") {
		if component == "." || component == ".." {
			return "", errMCPProjectRootURI
		}
		if drive && i > 0 && (!filepath.IsLocal(component) || strings.ContainsAny(component, `:<>"|?*`) ||
			strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ")) {
			return "", errMCPProjectRootURI
		}
	}

	switch {
	case hostOS == "windows":
		return filepath.FromSlash(p[1:]), nil
	case hostOS == "linux" && drive && wslDistro() != "":
		mount := filepath.Join(wslMountRoot, strings.ToLower(p[1:2]))
		if !mcpRootDirectoryChain(mount) {
			return "", errMCPProjectRootsUnbound
		}
		dir := normalizeWSLWindowsPath(p[1:])
		if !filepath.IsAbs(dir) {
			return "", errMCPProjectRootsUnbound
		}
		return dir, nil
	case (hostOS == "darwin" || hostOS == "linux") && !drive:
		return p, nil
	default:
		return "", errMCPProjectRootURI
	}
}

func mcpRootDirectoryChain(dir string) bool {
	if !filepath.IsAbs(dir) || !mcpRootLocalVolume(dir) {
		return false
	}
	dir = filepath.Clean(dir)
	root := filepath.VolumeName(dir) + string(filepath.Separator)
	rest := strings.TrimPrefix(dir, root)
	if !mcpRootDirectory(root) {
		return false
	}
	if rest == "" {
		return true
	}
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		root = filepath.Join(root, part)
		if !mcpRootDirectory(root) {
			return false
		}
	}
	return true
}

// The nearest binding wins. Only compare candidate paths that equal this
// ancestor or differ by case, so an unrelated configured share isn't probed.
func mcpProjectForLocalRoot(bindings []projectBinding, dir string) (mcpProject, error) {
	for cur := filepath.Clean(dir); ; cur = filepath.Dir(cur) {
		var selected mcpProject
		for _, binding := range bindings {
			if !strings.EqualFold(cur, binding.Dir) ||
				(cur != binding.Dir && (!mcpRootDirectoryChain(binding.Dir) || !sameProjectDir(cur, binding.Dir))) {
				continue
			}
			candidate := mcpProject{scope: binding.Scope, root: binding.Dir, ownerOnly: binding.OwnerOnly}
			if selected != (mcpProject{}) && selected != candidate {
				return mcpProject{}, errMCPProjectRootsAmbiguous
			}
			selected = candidate
		}
		if selected != (mcpProject{}) {
			return selected, nil
		}
		if filepath.Dir(cur) == cur {
			return mcpProject{}, errMCPProjectRootsUnbound
		}
	}
}
