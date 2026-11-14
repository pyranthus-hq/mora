package resource

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pyranthus-hq/mora/internal/managedtree"
)

const (
	maxFiles      = 1000
	maxFileBytes  = 1 << 20
	maxTotalBytes = 20 << 20
	maxPathBytes  = 240
)

func prepareCandidate(c Candidate) (Candidate, string, managedtree.Revision, error) {
	if !validKind(c.Kind) || len(c.CanonicalSource) > 1024 || len(c.Purpose) > 1024 || len(c.EffectiveScope) > 256 || len(c.UntrustedScopeSuggestion) > 256 {
		return Candidate{}, "", managedtree.Revision{}, fmt.Errorf("%w: invalid kind or metadata bounds", ErrInvalidCandidate)
	}
	c.Applicability = normalizedStrings(c.Applicability, 32, 256)
	c.KnownLimits = normalizedStrings(c.KnownLimits, 32, 256)
	if len(c.Applicability) == 0 && c.Purpose == "" {
		return Candidate{}, "", managedtree.Revision{}, fmt.Errorf("%w: purpose or applicability required", ErrInvalidCandidate)
	}
	if !validScope(c.EffectiveScope) {
		return Candidate{}, "", managedtree.Revision{}, fmt.Errorf("%w: effective scope", ErrInvalidCandidate)
	}
	if c.Source == "" {
		switch {
		case len(c.Pasted) > 0:
			c.Source = SourcePasted
		case c.Folder != "":
			c.Source = SourceFolder
		case c.Reference.URL != "":
			c.Source = SourceReference
		}
	}
	var tree managedtree.Revision
	switch c.Source {
	case SourcePasted:
		if len(c.Pasted) == 0 || c.Folder != "" || c.Reference.URL != "" || len(c.Pasted) > maxFileBytes {
			return Candidate{}, "", tree, fmt.Errorf("%w: pasted source", ErrInvalidCandidate)
		}
		if c.Kind != KindSkill {
			return Candidate{}, "", tree, fmt.Errorf("%w: pasted source is a skill only", ErrInvalidCandidate)
		}
		identity := c.CanonicalSource
		if c.Kind == KindRepository && c.Reference.URL != "" {
			identity = "repository:" + c.Reference.URL + "#" + c.Reference.Subdirectory
		}
		if identity == "" {
			identity = "pasted:" + digestBytes(c.Pasted)
		}
		return c, identity, tree, nil
	case SourceFolder:
		if c.Folder == "" || len(c.Pasted) != 0 {
			return Candidate{}, "", tree, fmt.Errorf("%w: folder source", ErrInvalidCandidate)
		}
		if c.Reference.URL != "" {
			var err error
			c.Reference, err = normalizeReference(c.Reference, c.Kind)
			if err != nil {
				return Candidate{}, "", tree, err
			}
		}
		if err := validateTree(c.Folder); err != nil {
			return Candidate{}, "", tree, err
		}
		var err error
		tree, err = managedtree.DigestTree(c.Folder)
		if err != nil {
			return Candidate{}, "", tree, fmt.Errorf("%w: digesting folder: %v", ErrUnsafeSource, err)
		}
		if c.Kind == KindSkill && !hasSkillFile(tree.Files) {
			return Candidate{}, "", tree, fmt.Errorf("%w: skill folder lacks exact SKILL.md", ErrInvalidCandidate)
		}
		identity := c.CanonicalSource
		if c.Kind == KindRepository {
			identity = "repository:" + c.Reference.URL + "#" + c.Reference.Subdirectory
		}
		if identity == "" {
			if c.Reference.URL != "" {
				identity = "reference:" + c.Reference.URL + "#" + c.Reference.Subdirectory
			} else {
				identity = "folder:" + tree.Digest
			}
		}
		return c, identity, tree, nil
	case SourceReference:
		if len(c.Pasted) != 0 || c.Folder != "" {
			return Candidate{}, "", tree, fmt.Errorf("%w: reference source", ErrInvalidCandidate)
		}
		ref, err := normalizeReference(c.Reference, c.Kind)
		if err != nil {
			return Candidate{}, "", tree, err
		}
		c.Reference = ref
		identity := c.CanonicalSource
		if c.Kind == KindRepository {
			identity = "repository:" + ref.URL + "#" + ref.Subdirectory
		}
		if identity == "" {
			identity = "reference:" + ref.URL + "#" + ref.Subdirectory
		}
		return c, identity, tree, nil
	default:
		return Candidate{}, "", tree, fmt.Errorf("%w: exactly one source is required", ErrInvalidCandidate)
	}
}

func validateTree(root string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: folder is not a plain directory", ErrUnsafeSource)
	}
	var files, total int64
	caseNames := make(map[string]string)
	return filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: %v", ErrUnsafeSource, walkErr)
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return fmt.Errorf("%w: relative path", ErrUnsafeSource)
		}
		if rel == "." {
			return nil
		}
		if err := safePath(filepath.ToSlash(rel)); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: links are refused", ErrUnsafeSource)
		}
		if entry.IsDir() {
			if administrative(filepath.Base(rel)) {
				return fmt.Errorf("%w: administrative tree", ErrUnsafeSource)
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: only regular files", ErrUnsafeSource)
		}
		if secretLike(filepath.Base(rel)) {
			return fmt.Errorf("%w: secret-like filename", ErrUnsafeSource)
		}
		files++
		total += info.Size()
		if files > maxFiles || info.Size() > maxFileBytes || total > maxTotalBytes {
			return fmt.Errorf("%w: resource size cap", ErrUnsafeSource)
		}
		key := strings.ToLower(filepath.ToSlash(rel))
		if prior, ok := caseNames[key]; ok && prior != rel {
			return fmt.Errorf("%w: case alias", ErrUnsafeSource)
		}
		caseNames[key] = rel
		return nil
	})
}

func safePath(rel string) error {
	if rel == "" || rel != path.Clean(rel) || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") || strings.HasPrefix(rel, "../") || rel == ".." || len(rel) > maxPathBytes {
		return fmt.Errorf("%w: unsafe path", ErrUnsafeSource)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." || administrative(part) {
			return fmt.Errorf("%w: unsafe path", ErrUnsafeSource)
		}
	}
	return nil
}

func normalizeReference(ref Reference, kind Kind) (Reference, error) {
	u, err := url.Parse(ref.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Reference{}, fmt.Errorf("%w: canonical HTTPS reference required", ErrInvalidCandidate)
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = path.Clean("/" + u.Path)
	if u.Path == "/." {
		u.Path = "/"
	}
	ref.URL = strings.TrimSuffix(u.String(), "/")
	ref.Subdirectory = strings.Trim(strings.ReplaceAll(ref.Subdirectory, "\\", "/"), "/")
	if ref.Subdirectory != "" && safePath(ref.Subdirectory) != nil {
		return Reference{}, fmt.Errorf("%w: reference subdirectory", ErrInvalidCandidate)
	}
	if kind == KindRepository && ref.Version == "" {
		return Reference{}, fmt.Errorf("%w: repository pin required", ErrInvalidCandidate)
	}
	return ref, nil
}

func validKind(k Kind) bool {
	return k == KindSkill || k == KindBestPractice || k == KindRepository || k == KindTool
}
func validScope(s string) bool {
	return s == "" || s == "global" || (strings.HasPrefix(s, "project:") && len(s) > len("project:"))
}
func administrative(base string) bool { return base == ".git" || base == ".hg" || base == ".svn" }
func secretLike(base string) bool {
	b := strings.ToLower(base)
	return b == ".env" || strings.HasPrefix(b, ".env.") || strings.Contains(b, "token") || strings.HasSuffix(b, ".pem") || strings.HasSuffix(b, ".key") || strings.HasPrefix(b, "id_rsa") || b == "credentials.json" || b == "auth.json"
}
func hasSkillFile(files []managedtree.File) bool {
	for _, f := range files {
		if f.Path == "SKILL.md" {
			return true
		}
	}
	return false
}
func normalizedStrings(in []string, max, maxLen int) []string {
	set := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && len(s) <= maxLen {
			set[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) > max {
		return out[:max]
	}
	return out
}
func digestBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
