package mora

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	agentPluginSchema    = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	agentPluginMCPSchema = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
)

type agentPluginAuthor struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

type agentPluginManifest struct {
	Schema      string                    `json:"$schema"`
	Name        string                    `json:"name"`
	Version     string                    `json:"version,omitempty"`
	Description string                    `json:"description,omitempty"`
	Author      *agentPluginAuthor        `json:"author,omitempty"`
	Homepage    string                    `json:"homepage,omitempty"`
	Repository  string                    `json:"repository,omitempty"`
	License     string                    `json:"license,omitempty"`
	Keywords    []string                  `json:"keywords,omitempty"`
	Extensions  map[string]map[string]any `json:"extensions,omitempty"`
}

type claudePluginManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

type agentPluginMCPDocument struct {
	Schema     string                     `json:"$schema"`
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

type agentPluginStdioServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	CWD     string            `json:"cwd,omitempty"`
}

type agentSkillFrontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty"`
	AllowedTools  string            `yaml:"allowed-tools,omitempty"`
}

func agentPluginRoot() string {
	if root := os.Getenv("MORA_PLUGIN_CONTRACT_ROOT"); root != "" {
		return root
	}
	return filepath.Join("..", "..", "plugins", "mora")
}

func TestAgentPluginPackageContract(t *testing.T) {
	pluginRoot := agentPluginRoot()
	assertNoPluginSymlinks(t, pluginRoot)
	assertAgentPluginManifest(t, filepath.Join(pluginRoot, "plugin.json"))
	assertAgentPluginMCP(t, filepath.Join(pluginRoot, "mcp.json"))
	assertClaudePluginManifest(t, filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"))

	repositoryLicense, err := os.ReadFile(filepath.Join("..", "..", "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	pluginLicense, err := os.ReadFile(filepath.Join(pluginRoot, "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pluginLicense, repositoryLicense) {
		t.Fatal("plugins/mora/LICENSE must be a real copy of the repository Apache-2.0 license")
	}
}

func assertNoPluginSymlinks(t *testing.T, pluginRoot string) {
	t.Helper()
	if err := filepath.WalkDir(pluginRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("plugin package contains symlink %s; distributable paths must stay inside the package", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertAgentPluginManifest(t *testing.T, path string) {
	t.Helper()
	var manifest agentPluginManifest
	readStrictJSONFile(t, path, &manifest)
	if manifest.Schema != agentPluginSchema {
		t.Fatalf("plugin.json $schema = %q, want %q", manifest.Schema, agentPluginSchema)
	}
	if manifest.Name != "mora" {
		t.Fatalf("plugin.json name = %q, want mora", manifest.Name)
	}
	nameRE := regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,62}[a-z0-9])?$`)
	if !nameRE.MatchString(manifest.Name) || strings.Contains(manifest.Name, "--") || strings.Contains(manifest.Name, "..") {
		t.Fatalf("plugin.json name %q violates Agent Plugins 1.0 constraints", manifest.Name)
	}
	if manifest.License != "Apache-2.0" {
		t.Fatalf("plugin.json license = %q, want Apache-2.0", manifest.License)
	}
	if manifest.Description == "" || manifest.Repository == "" || manifest.Author == nil || manifest.Author.Name == "" || len(manifest.Keywords) == 0 {
		t.Fatal("plugin.json must carry description, repository, author, and keyword metadata")
	}
	for namespace, value := range manifest.Extensions {
		if value == nil {
			t.Fatalf("plugin.json extension %q must be an object", namespace)
		}
	}
}

func assertClaudePluginManifest(t *testing.T, path string) {
	t.Helper()
	var manifest claudePluginManifest
	readStrictJSONFile(t, path, &manifest)
	if manifest.Name != "mora" || manifest.Description == "" {
		t.Fatalf("Claude plugin manifest must identify and describe mora: %#v", manifest)
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(manifest.Version) {
		t.Fatalf("Claude plugin version %q must be stable semver", manifest.Version)
	}
}

func assertAgentPluginMCP(t *testing.T, path string) {
	t.Helper()
	var cfg agentPluginMCPDocument
	readStrictJSONFile(t, path, &cfg)
	if cfg.Schema != agentPluginMCPSchema {
		t.Fatalf("mcp.json $schema = %q, want %q", cfg.Schema, agentPluginMCPSchema)
	}
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("mcp.json has %d servers, want exactly the mora stdio server", len(cfg.MCPServers))
	}
	raw, ok := cfg.MCPServers["mora"]
	if !ok {
		t.Fatal("mcp.json is missing the mora server")
	}
	var server agentPluginStdioServer
	readStrictJSON(t, "mcp.json mora server", raw, &server)
	if server.Type != "stdio" || server.Command != "mora" {
		t.Fatalf("mcp.json mora server = %#v, want stdio command token mora", server)
	}
	if strings.ContainsAny(server.Command, " \t\r\n") {
		t.Fatalf("mcp.json command %q must be one executable token", server.Command)
	}
	if len(server.Args) != 2 || server.Args[0] != "mcp" || server.Args[1] != "serve" {
		t.Fatalf("mcp.json mora args = %#v, want [mcp serve]", server.Args)
	}
	if len(server.Env) != 0 || server.CWD != "" {
		t.Fatal("portable Mora MCP declaration must not carry env, secrets, or a client-specific working directory")
	}
}

func readStrictJSONFile(t *testing.T, path string, dst any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	readStrictJSON(t, path, b, dst)
}

func readStrictJSON(t *testing.T, label string, b []byte, dst any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("parse %s: %v", label, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("parse %s: trailing JSON value", label)
	}
}

func TestAgentPluginSkillsContract(t *testing.T) {
	pluginRoot := agentPluginRoot()
	skillsRoot := filepath.Join(pluginRoot, "skills")
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		t.Fatal(err)
	}
	readmeBytes, err := os.ReadFile(filepath.Join(pluginRoot, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(readmeBytes)
	knownTools := map[string]bool{}
	for _, name := range mcpToolNames() {
		knownTools[name] = true
	}
	nameRE := regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	toolTokenRE := regexp.MustCompile(`\b[a-z][a-z0-9_]*\b`)
	forbidden := []string{"mcp__", "claude mcp add", "briefs/<today", "/Users/", `C:\Users\`, "~/."}
	reservedToolWords := map[string]bool{"brief": true, "digest": true, "think": true, "meeting_prep": true, "get_entity": true, "list_entities": true}

	var skillNames []string
	triggerOwners := map[string]string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirName := entry.Name()
		skillPath := filepath.Join(skillsRoot, dirName, "SKILL.md")
		body, err := os.ReadFile(skillPath)
		if err != nil {
			t.Errorf("skill directory %s has no readable SKILL.md: %v", dirName, err)
			continue
		}
		text := string(body)
		frontmatter := decodeSkillFrontmatter(t, skillPath, text)
		if frontmatter.Name != dirName {
			t.Errorf("%s name = %q, want directory name %q", skillPath, frontmatter.Name, dirName)
		}
		if !nameRE.MatchString(frontmatter.Name) || len(frontmatter.Name) > 64 {
			t.Errorf("%s name %q violates Agent Skills naming rules", skillPath, frontmatter.Name)
		}
		if frontmatter.Description == "" || utf8.RuneCountInString(frontmatter.Description) > 1024 {
			t.Errorf("%s description length = %d, want 1..1024", skillPath, utf8.RuneCountInString(frontmatter.Description))
		}
		if utf8.RuneCountInString(frontmatter.Compatibility) > 500 {
			t.Errorf("%s compatibility length = %d, want <=500", skillPath, utf8.RuneCountInString(frontmatter.Compatibility))
		}
		if frontmatter.License != "Apache-2.0" {
			t.Errorf("%s license = %q, want Apache-2.0", skillPath, frontmatter.License)
		}
		rawTriggers := frontmatter.Metadata["trigger_phrases"]
		if rawTriggers == "" {
			t.Errorf("%s metadata.trigger_phrases is required", skillPath)
		}
		for _, raw := range strings.Split(rawTriggers, ",") {
			phrase := strings.ToLower(strings.Join(strings.Fields(raw), " "))
			if phrase == "" {
				t.Errorf("%s has an empty trigger phrase", skillPath)
				continue
			}
			if owner, exists := triggerOwners[phrase]; exists {
				t.Errorf("trigger phrase %q overlaps skills %q and %q", phrase, owner, frontmatter.Name)
			} else {
				triggerOwners[phrase] = frontmatter.Name
			}
		}
		if lines := strings.Count(text, "\n") + 1; lines > 500 {
			t.Errorf("%s has %d lines, Agent Skills recommends <=500", skillPath, lines)
		}
		if len(body) > 25_000 {
			t.Errorf("%s has %d bytes; move detail to references to stay near the 5k-token guidance", skillPath, len(body))
		}
		for _, bad := range forbidden {
			if strings.Contains(text, bad) {
				t.Errorf("%s contains non-portable or private path token %q", skillPath, bad)
			}
		}
		for _, token := range toolTokenRE.FindAllString(text, -1) {
			if strings.HasSuffix(token, "_memory") || reservedToolWords[token] {
				if !knownTools[token] {
					t.Errorf("%s mentions MCP-like tool %q absent from mcpToolRegistry", skillPath, token)
				}
			}
		}
		if !strings.Contains(readme, "`"+frontmatter.Name+"`") {
			t.Errorf("plugins/mora/README.md does not list skill %q", frontmatter.Name)
		}
		if frontmatter.Name == "daily-brief-loop" {
			for _, broad := range []string{"catch me up", "what changed", "morning brief", "daily pulse"} {
				if strings.Contains(strings.ToLower(frontmatter.Description), broad) {
					t.Errorf("daily-brief-loop description contains broad read-only trigger %q", broad)
				}
			}
		}
		skillNames = append(skillNames, frontmatter.Name)
	}
	if len(skillNames) == 0 {
		t.Fatal("agent plugin contains no skills")
	}
	sort.Strings(skillNames)
	for i := 1; i < len(skillNames); i++ {
		if skillNames[i] == skillNames[i-1] {
			t.Fatalf("duplicate skill name %q", skillNames[i])
		}
	}
}

func decodeSkillFrontmatter(t *testing.T, path, text string) agentSkillFrontmatter {
	t.Helper()
	// Git may materialize text files with CRLF on Windows runners. YAML accepts
	// either newline form, so normalize only for delimiter extraction.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		t.Fatalf("%s is missing leading YAML frontmatter", path)
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		t.Fatalf("%s is missing closing YAML frontmatter delimiter", path)
	}
	frontmatter := text[4 : 4+end]
	dec := yaml.NewDecoder(strings.NewReader(frontmatter))
	dec.KnownFields(true)
	var decoded agentSkillFrontmatter
	if err := dec.Decode(&decoded); err != nil {
		t.Fatalf("parse %s frontmatter: %v", path, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("parse %s frontmatter: trailing YAML document", path)
	}
	return decoded
}
