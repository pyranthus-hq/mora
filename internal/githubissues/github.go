// Package githubissues implements Mora's read-only GitHub issue source.
package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

const (
	KindIssue memory.ItemKind = "github_issue"
	pageSize                  = 100
)

var DefaultRepositories = []string{"pyranthus-hq/mora", "pyranthus-hq/productivity"}

func init() { memory.RegisterKind(KindIssue, "issue", "github") }

type label struct {
	Name string `json:"name"`
}

type assignee struct {
	Login string `json:"login"`
}

type apiIssue struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	HTMLURL     string     `json:"html_url"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Labels      []label    `json:"labels"`
	Assignees   []assignee `json:"assignees"`
	PullRequest *struct{}  `json:"pull_request"`
}

// Snapshot is the immutable source record carried to Mora's append-only snapshot
// store before the searchable projection is reconciled.
type Snapshot struct {
	Repository   string   `json:"repository"`
	Number       int      `json:"issue_number"`
	Title        string   `json:"title"`
	Body         string   `json:"body"`
	State        string   `json:"state"`
	Labels       []string `json:"labels"`
	Assignees    []string `json:"assignees"`
	CanonicalURL string   `json:"canonical_url"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	RetrievedAt  string   `json:"retrieved_at"`
}

// Payload is connector-private data passed opaquely through memory.Ingest.
type Payload struct {
	Snapshot Snapshot
	Bytes    []byte
}

type Fetcher struct {
	client  *http.Client
	baseURL string
	repos   []string
	token   string
	now     func() time.Time
}

func NewLiveFetcher(repos []string, token string) (*Fetcher, error) {
	return NewFetcher(http.DefaultClient, "https://api.github.com", repos, token, time.Now)
}

// NewFetcher is the deterministic test seam; production uses NewLiveFetcher.
func NewFetcher(client *http.Client, baseURL string, repos []string, token string, now func() time.Time) (*Fetcher, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if now == nil {
		now = time.Now
	}
	clean, err := ValidateRepositories(repos)
	if err != nil {
		return nil, err
	}
	return &Fetcher{client: client, baseURL: strings.TrimRight(baseURL, "/"), repos: clean, token: token, now: now}, nil
}

func ValidateRepositories(repos []string) ([]string, error) {
	seen := map[string]bool{}
	clean := make([]string, 0, len(repos))
	for _, raw := range repos {
		repo := strings.ToLower(strings.TrimSpace(raw))
		parts := strings.Split(repo, "/")
		if len(parts) != 2 || !safeRepoPart(parts[0]) || !safeRepoPart(parts[1]) {
			return nil, fmt.Errorf("invalid GitHub repository %q (want owner/repo)", raw)
		}
		if !seen[repo] {
			seen[repo] = true
			clean = append(clean, repo)
		}
	}
	if len(clean) == 0 {
		return nil, errors.New("at least one GitHub repository is required")
	}
	sort.Strings(clean)
	return clean, nil
}

func safeRepoPart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

func (f *Fetcher) FetchPage(kind memory.ItemKind, _ memory.FetchWindow, cursor string) (memory.Page, error) {
	if kind != KindIssue {
		return memory.Page{}, fmt.Errorf("github issues: unsupported kind %q", kind)
	}
	repoIndex, page, err := parseCursor(cursor)
	if err != nil {
		return memory.Page{}, err
	}
	if repoIndex >= len(f.repos) {
		return memory.Page{}, nil
	}
	repo := f.repos[repoIndex]
	parts := strings.Split(repo, "/")
	u, err := url.Parse(f.baseURL + "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/issues")
	if err != nil {
		return memory.Page{}, err
	}
	q := u.Query()
	q.Set("state", "all")
	q.Set("sort", "created")
	q.Set("direction", "asc")
	q.Set("per_page", strconv.Itoa(pageSize))
	q.Set("page", strconv.Itoa(page))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
	if err != nil {
		return memory.Page{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return memory.Page{}, fmt.Errorf("github issues: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return memory.Page{}, errors.New("github issues: authentication failed (check MORA_GITHUB_TOKEN)")
		case http.StatusForbidden:
			if resp.Header.Get("X-RateLimit-Remaining") != "0" {
				return memory.Page{}, errors.New("github issues: access forbidden (check MORA_GITHUB_TOKEN and repository access)")
			}
			return memory.Page{}, fmt.Errorf("github issues: rate limited (reset=%s, retry_after=%s)", resp.Header.Get("X-RateLimit-Reset"), resp.Header.Get("Retry-After"))
		case http.StatusTooManyRequests:
			return memory.Page{}, fmt.Errorf("github issues: rate limited (reset=%s, retry_after=%s)", resp.Header.Get("X-RateLimit-Reset"), resp.Header.Get("Retry-After"))
		default:
			return memory.Page{}, fmt.Errorf("github issues: API returned HTTP %d", resp.StatusCode)
		}
	}
	var rows []apiIssue
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&rows); err != nil {
		return memory.Page{}, fmt.Errorf("github issues: decoding page: %w", err)
	}
	retrieved := f.now().UTC().Format(time.RFC3339)
	items := make([]memory.Item, 0, len(rows))
	for _, row := range rows {
		if row.PullRequest != nil {
			continue
		}
		items = append(items, issueItem(repo, row, retrieved))
	}
	next := ""
	if len(rows) == pageSize {
		next = formatCursor(repoIndex, page+1)
	} else if repoIndex+1 < len(f.repos) {
		next = formatCursor(repoIndex+1, 1)
	}
	return memory.Page{Items: items, NextCursor: next}, nil
}

func issueItem(repo string, row apiIssue, retrieved string) memory.Item {
	labels := make([]string, 0, len(row.Labels))
	for _, l := range row.Labels {
		labels = append(labels, l.Name)
	}
	assignees := make([]string, 0, len(row.Assignees))
	for _, a := range row.Assignees {
		assignees = append(assignees, a.Login)
	}
	sort.Strings(labels)
	sort.Strings(assignees)
	snap := Snapshot{
		Repository: repo, Number: row.Number, Title: row.Title, Body: row.Body, State: row.State,
		Labels: labels, Assignees: assignees, CanonicalURL: row.HTMLURL,
		CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: row.UpdatedAt.UTC().Format(time.RFC3339), RetrievedAt: retrieved,
	}
	b, _ := json.Marshal(snap)
	body := fmt.Sprintf("Repository: %s\nIssue: #%d\nState: %s\nURL: %s\nLabels: %s\nAssignees: %s\n\n%s",
		repo, row.Number, row.State, row.HTMLURL, strings.Join(labels, ", "), strings.Join(assignees, ", "), row.Body)
	return memory.Item{
		Kind: KindIssue, ProviderID: fmt.Sprintf("%s#%d", repo, row.Number), Title: row.Title,
		Body: body, OccurredAt: row.CreatedAt, Tags: append([]string{"github", repo}, labels...),
		Meta: map[string]any{
			"repository": repo, "issue_number": row.Number, "state": row.State, "labels": labels,
			"assignees": assignees, "canonical_url": row.HTMLURL, "created_at": snap.CreatedAt,
			"updated_at": snap.UpdatedAt, "retrieved_at": retrieved,
		},
		Payload: Payload{Snapshot: snap, Bytes: b},
	}
}

func MapIssue(it memory.Item, scope string, budget int) memory.MappedMemory {
	mm := memory.MapItem(it, scope, budget)
	mm.StableID = "github:" + it.ProviderID
	mm.Type = "issue"
	mm.Provider = "github"
	return mm
}

func parseCursor(cursor string) (int, int, error) {
	if cursor == "" {
		return 0, 1, nil
	}
	parts := strings.Split(cursor, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("github issues: invalid checkpoint %q", cursor)
	}
	ri, err1 := strconv.Atoi(parts[0])
	page, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || ri < 0 || page < 1 {
		return 0, 0, fmt.Errorf("github issues: invalid checkpoint %q", cursor)
	}
	return ri, page, nil
}

func formatCursor(repoIndex, page int) string { return fmt.Sprintf("%d:%d", repoIndex, page) }
