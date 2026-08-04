package githubissues

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

var fixedNow = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func TestFetcherPaginatesAllowlistedRepositories(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization header missing")
		}
		if strings.Contains(r.URL.Path, "/mora/") {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 255, "title": "GitHub source", "body": "body", "state": "open",
				"html_url":   "https://github.com/pyranthus-hq/mora/issues/255",
				"created_at": "2026-07-31T00:00:00Z", "updated_at": "2026-08-01T00:00:00Z",
				"labels": []map[string]string{{"name": "product"}}, "assignees": []map[string]string{{"login": "octo"}},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"number": 9, "title": "Downstream", "body": "body", "state": "open",
			"html_url":   "https://github.com/pyranthus-hq/productivity/issues/9",
			"created_at": "2026-07-31T00:00:00Z", "updated_at": "2026-08-01T00:00:00Z",
		}})
	}))
	defer srv.Close()
	f, err := NewFetcher(srv.Client(), srv.URL,
		[]string{"pyranthus-hq/productivity", "pyranthus-hq/mora"}, "secret", func() time.Time { return fixedNow })
	if err != nil {
		t.Fatal(err)
	}
	p1, err := f.FetchPage(KindIssue, memory.FetchWindow{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Items) != 1 || p1.Items[0].ProviderID != "pyranthus-hq/mora#255" || p1.NextCursor != "1:1" {
		t.Fatalf("first page = %+v", p1)
	}
	p2, err := f.FetchPage(KindIssue, memory.FetchWindow{}, p1.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Items) != 1 || p2.Items[0].ProviderID != "pyranthus-hq/productivity#9" || p2.NextCursor != "" {
		t.Fatalf("second page = %+v", p2)
	}
	if len(paths) != 2 || !strings.Contains(paths[0], "state=all") || !strings.Contains(paths[0], "per_page=100") {
		t.Fatalf("requests = %v", paths)
	}
}

func TestFetcherFiltersPullRequestsAndMapsProvenance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
 {"number":1,"title":"Issue","body":"Details","state":"closed","html_url":"https://github.com/o/r/issues/1","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-02-01T00:00:00Z","labels":[{"name":"bug"}],"assignees":[{"login":"sam"}]},
 {"number":2,"title":"PR","body":"","state":"open","html_url":"https://github.com/o/r/pull/2","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-02-01T00:00:00Z","pull_request":{}}
]`))
	}))
	defer srv.Close()
	f, _ := NewFetcher(srv.Client(), srv.URL, []string{"o/r"}, "", func() time.Time { return fixedNow })
	p, err := f.FetchPage(KindIssue, memory.FetchWindow{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 {
		t.Fatalf("items = %d, want issue only", len(p.Items))
	}
	it := p.Items[0]
	mm := MapIssue(it, "personal", 64*1024)
	if mm.StableID != "github:o/r#1" || mm.Provider != "github" || mm.Type != "issue" {
		t.Fatalf("mapped identity = %+v", mm)
	}
	if mm.Meta["state"] != "closed" || mm.Meta["canonical_url"] != "https://github.com/o/r/issues/1" {
		t.Fatalf("meta = %+v", mm.Meta)
	}
	payload := it.Payload.(Payload)
	if payload.Snapshot.RetrievedAt != fixedNow.Format(time.RFC3339) || !json.Valid(payload.Bytes) {
		t.Fatalf("snapshot = %+v bytes=%s", payload.Snapshot, payload.Bytes)
	}
}

func TestLifecycleReconcilesStableIdentity(t *testing.T) {
	base := apiIssue{
		Number: 7, Title: "Lifecycle", Body: "original", State: "open",
		HTMLURL: "https://github.com/o/r/issues/7", CreatedAt: fixedNow.Add(-time.Hour), UpdatedAt: fixedNow,
	}
	a := MapIssue(issueItem("o/r", base, fixedNow.Format(time.RFC3339)), "personal", 1024)
	base.State = "closed"
	base.Body = "edited"
	base.UpdatedAt = fixedNow.Add(time.Hour)
	b := MapIssue(issueItem("o/r", base, fixedNow.Add(time.Hour).Format(time.RFC3339)), "personal", 1024)
	if a.StableID != b.StableID || a.ContentHash == b.ContentHash {
		t.Fatalf("lifecycle must overwrite one stable projection with changed content: a=%+v b=%+v", a, b)
	}
}

func TestRateLimitAndAuthenticationErrorsAreExplicit(t *testing.T) {
	for _, tc := range []struct {
		status    int
		remaining string
		want      string
	}{
		{http.StatusUnauthorized, "", "authentication failed"},
		{http.StatusForbidden, "0", "rate limited"},
		{http.StatusForbidden, "42", "access forbidden"},
		{http.StatusTooManyRequests, "", "rate limited"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Reset", "123")
			w.Header().Set("X-RateLimit-Remaining", tc.remaining)
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte("secret issue content must not enter errors"))
		}))
		f, _ := NewFetcher(srv.Client(), srv.URL, []string{"o/r"}, "token", func() time.Time { return fixedNow })
		_, err := f.FetchPage(KindIssue, memory.FetchWindow{}, "")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret issue content") {
			t.Fatalf("status %d error = %v", tc.status, err)
		}
	}
}

func TestRepositoryAndCursorValidation(t *testing.T) {
	for _, bad := range [][]string{{}, {"owner"}, {"../repo"}, {"owner/repo/extra"}} {
		if _, err := ValidateRepositories(bad); err == nil {
			t.Fatalf("repositories %v should fail", bad)
		}
	}
	f, _ := NewFetcher(http.DefaultClient, "https://api.github.com", []string{"o/r"}, "", func() time.Time { return fixedNow })
	if _, err := f.FetchPage(KindIssue, memory.FetchWindow{}, "broken"); err == nil {
		t.Fatal("malformed checkpoint should fail")
	}
}
