package mora

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeReranker scores by an explicit table so a test can state the ranking it
// wants without reasoning about a model.
type fakeReranker struct {
	scores map[string]float64
	err    error
	sawQ   string
	sawIDs []string
}

func (f *fakeReranker) Name() string { return "fake" }

func (f *fakeReranker) Score(_ context.Context, query string, docs []rerankDoc) (map[string]float64, error) {
	f.sawQ = query
	f.sawIDs = nil
	for _, d := range docs {
		f.sawIDs = append(f.sawIDs, d.ID)
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.scores, nil
}

func rerankFixture(t *testing.T, n int) (Config, []string) {
	t.Helper()
	mems := make([]Memory, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := "synth/doc-" + string(rune('a'+i))
		mems = append(mems, coreBIdxmem(id, "global", "note", "Doc "+string(rune('A'+i)), "body of document "+id))
		ids = append(ids, id)
	}
	return gate2Vault(t, mems...), ids
}

func rerankIDs(t *testing.T, cfg Config, r reranker, ids []string) []string {
	t.Helper()
	ctx := context.Background()
	db, err := openIndexRO(ctx, cfg)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()
	return applyRerank(ctx, db, r, "any query", ids)
}

// The stage may reorder the window. It may never change which documents are in
// it -- that is the property that bounds a wrong judgment to a slot change
// rather than a disappeared search result.
func TestRerankIsAPurePermutationOfTheWindow(t *testing.T) {
	cfg, ids := rerankFixture(t, 4)
	r := &fakeReranker{scores: map[string]float64{
		ids[0]: 0.1, ids[1]: 0.2, ids[2]: 0.3, ids[3]: 0.9,
	}}
	got := rerankIDs(t, cfg, r, ids)

	want := []string{ids[3], ids[2], ids[1], ids[0]}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reranked order = %v, want descending by score %v", got, want)
	}
	if !isPermutation(ids, got) {
		t.Fatalf("rerank changed the candidate set: %v -> %v", ids, got)
	}
}

// A reranker outage must be indistinguishable from the reranker being off.
func TestRerankFailsOpenWhenTheRerankerErrors(t *testing.T) {
	cfg, ids := rerankFixture(t, 3)
	r := &fakeReranker{err: errors.New("upstream is down")}
	got := rerankIDs(t, cfg, r, ids)
	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Fatalf("errored rerank changed the order: %v, want the fused order %v", got, ids)
	}
}

// An empty answer set is an outage by another name.
func TestRerankFailsOpenOnAnEmptyScoreSet(t *testing.T) {
	cfg, ids := rerankFixture(t, 3)
	got := rerankIDs(t, cfg, &fakeReranker{scores: map[string]float64{}}, ids)
	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Fatalf("empty-score rerank changed the order: %v", got)
	}
}

// A partial answer must leave the unscored candidates where fusion put them,
// rather than sweeping them to one end of the window.
func TestRerankKeepsTheFusedOrderForUnscoredCandidates(t *testing.T) {
	cfg, ids := rerankFixture(t, 4)
	// Only the last candidate is scored; the other three go unmentioned.
	r := &fakeReranker{scores: map[string]float64{ids[3]: 0.9}}
	got := rerankIDs(t, cfg, r, ids)

	want := []string{ids[3], ids[0], ids[1], ids[2]}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("partial rerank = %v, want the scored doc promoted and the rest in fused order %v", got, want)
	}
}

// Everything past rerankWindow keeps its fused position: the stage is bounded
// in cost AND in blast radius.
func TestRerankLeavesTheTailBelowTheWindowUntouched(t *testing.T) {
	n := rerankWindow + 5
	cfg, ids := rerankFixture(t, n)
	// Score the tail highest. It is outside the window, so it must not move.
	scores := map[string]float64{}
	for i, id := range ids {
		scores[id] = float64(i)
	}
	got := rerankIDs(t, cfg, &fakeReranker{scores: scores}, ids)

	if len(got) != n {
		t.Fatalf("rerank changed length: %d, want %d", len(got), n)
	}
	for i := rerankWindow; i < n; i++ {
		if got[i] != ids[i] {
			t.Fatalf("tail moved at %d: %q, want %q", i, got[i], ids[i])
		}
	}
	if got[0] != ids[rerankWindow-1] {
		t.Fatalf("head not reranked: got[0]=%q, want the top-scored in-window id %q", got[0], ids[rerankWindow-1])
	}
}

// The reranker only ever sees the window, never the whole fused pool.
func TestRerankSendsOnlyTheWindowToTheReranker(t *testing.T) {
	n := rerankWindow + 3
	cfg, ids := rerankFixture(t, n)
	r := &fakeReranker{scores: map[string]float64{ids[0]: 1}}
	rerankIDs(t, cfg, r, ids)
	if len(r.sawIDs) != rerankWindow {
		t.Fatalf("reranker saw %d docs, want the %d-doc window", len(r.sawIDs), rerankWindow)
	}
}

func TestIsPermutationRejectsSubstitutionAndLengthChange(t *testing.T) {
	base := []string{"a", "b", "c"}
	cases := []struct {
		name string
		got  []string
		want bool
	}{
		{"same set reordered", []string{"c", "a", "b"}, true},
		{"substituted member", []string{"a", "b", "d"}, false},
		{"dropped member", []string{"a", "b"}, false},
		{"duplicated member", []string{"a", "a", "b"}, false},
	}
	for _, tt := range cases {
		if got := isPermutation(base, tt.got); got != tt.want {
			t.Fatalf("%s: isPermutation = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// Reranking is off unless something explicitly turns it on, so the default
// retrieval path stays deterministic and offline.
func TestConfigRerankIsOffByDefault(t *testing.T) {
	t.Setenv("MORA_RERANK", "")
	if r := configRerank(defaultConfig()); r != nil {
		t.Fatalf("configRerank = %v, want nil when nothing selects a reranker", r)
	}
}

// ---- Jev client ----

func jevTestServer(t *testing.T, h http.HandlerFunc) *jevReranker {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &jevReranker{apiKey: "test-key", client: srv.Client(), model: jevModel, endpoint: srv.URL}
}

func TestJevScoreSendsOneNoulPerDocumentAndMapsAnswersBackToIDs(t *testing.T) {
	var req jevRequest
	j := jevTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		answers := map[string]jevAnswer{}
		for key := range req.Questions {
			// D1 -> 0.9, everything else -> 0.1
			v := 0.1
			if key == "D1" {
				v = 0.9
			}
			answers[key] = jevAnswer{Type: "noul", Noul: v}
		}
		json.NewEncoder(w).Encode(jevResponse{Model: jevModel, Answers: answers})
	})

	docs := []rerankDoc{
		{ID: "id-one", Title: "One", Text: "first"},
		{ID: "id-two", Title: "Two", Text: "second"},
	}
	got, err := j.Score(context.Background(), "the query", docs)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if len(req.Questions) != 2 {
		t.Fatalf("sent %d questions, want one per document", len(req.Questions))
	}
	for key, q := range req.Questions {
		if q.Type != "noul" {
			t.Fatalf("question %s type = %q, want noul", key, q.Type)
		}
		if !strings.Contains(q.Instructions, "["+key+"]") {
			t.Fatalf("question %s does not name its own label: %q", key, q.Instructions)
		}
	}
	if !strings.Contains(req.State, "the query") {
		t.Fatalf("state omits the query: %q", req.State)
	}
	if req.Model != jevModel {
		t.Fatalf("model = %q, want the pinned %q", req.Model, jevModel)
	}
	if got["id-one"] != 0.9 || got["id-two"] != 0.1 {
		t.Fatalf("scores = %v, want the D1 answer mapped to id-one", got)
	}
}

// The response is keyed by label, so the mapping from label back to memory id
// is ours to get right. Shuffling the input must not shuffle the verdict: if
// this fails, the stage is ranking by position rather than by content.
func TestJevScoreIsOrderIndependentForTheSameDocuments(t *testing.T) {
	// The fake scores by CONTENT, not by label, so any order dependence the test
	// detects comes from our label bookkeeping.
	j := jevTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req jevRequest
		json.NewDecoder(r.Body).Decode(&req)
		answers := map[string]jevAnswer{}
		for key := range req.Questions {
			// Find this label's block in the state and score on what is in it.
			v := 0.2
			if idx := strings.Index(req.State, "["+key+"]"); idx >= 0 {
				block := req.State[idx:]
				if end := strings.Index(block[1:], "\n["); end >= 0 {
					block = block[:end+1]
				}
				if strings.Contains(block, "relevant") {
					v = 0.95
				}
			}
			answers[key] = jevAnswer{Type: "noul", Noul: v}
		}
		json.NewEncoder(w).Encode(jevResponse{Model: jevModel, Answers: answers})
	})

	forward := []rerankDoc{
		{ID: "a", Title: "A", Text: "unrelated filler"},
		{ID: "b", Title: "B", Text: "the relevant one"},
		{ID: "c", Title: "C", Text: "more filler"},
	}
	reversed := []rerankDoc{forward[2], forward[1], forward[0]}

	fwd, err := j.Score(context.Background(), "q", forward)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	rev, err := j.Score(context.Background(), "q", reversed)
	if err != nil {
		t.Fatalf("reversed: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if fwd[id] != rev[id] {
			t.Fatalf("id %q scored %v forward and %v reversed: ranking depends on input position", id, fwd[id], rev[id])
		}
	}
	if fwd["b"] <= fwd["a"] {
		t.Fatalf("content scoring did not take effect: %v", fwd)
	}
}

// A non-200 body can echo the state we submitted, which is vault content. It
// must not end up in a Mora error string, a log line, or a bug report.
func TestJevScoreErrorOmitsTheResponseBody(t *testing.T) {
	j := jevTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("rate limited; submitted state was: PRIVATE_VAULT_SENTINEL"))
	})
	_, err := j.Score(context.Background(), "q", []rerankDoc{{ID: "a", Text: "x"}})
	if err == nil {
		t.Fatal("want an error on 429")
	}
	if strings.Contains(err.Error(), "PRIVATE_VAULT_SENTINEL") {
		t.Fatalf("error leaked the response body: %v", err)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("error should name the status: %v", err)
	}
}

// An unpinned model would let the vendor's release schedule silently re-rank a
// user's search results and invalidate any eval number recorded against it.
func TestJevRerankerPinsAnExactModelVersion(t *testing.T) {
	if jevModel == "jev-latest" || jevModel == "jev-preview" {
		t.Fatalf("jevModel = %q, want an exact pinned version", jevModel)
	}
	if !strings.HasPrefix(jevModel, "jev-") {
		t.Fatalf("jevModel = %q", jevModel)
	}
}

// Malformed or foreign answer types are dropped rather than coerced, and a
// response with nothing usable is an error (which applyRerank turns into the
// fused order).
func TestJevScoreRejectsAnswersOfTheWrongType(t *testing.T) {
	j := jevTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req jevRequest
		json.NewDecoder(r.Body).Decode(&req)
		answers := map[string]jevAnswer{}
		for key := range req.Questions {
			answers[key] = jevAnswer{Type: "choice", Noul: 0.99}
		}
		json.NewEncoder(w).Encode(jevResponse{Model: jevModel, Answers: answers})
	})
	if _, err := j.Score(context.Background(), "q", []rerankDoc{{ID: "a", Text: "x"}}); err == nil {
		t.Fatal("want an error when no answer is a noul")
	}
}
