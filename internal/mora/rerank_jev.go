package mora

// A reranker backed by TypeSafe's Jev (a "System One" model returning typed
// probabilistic decisions rather than text). One request scores the whole
// window: the candidates go in the shared state, and each candidate gets its
// own Noul question -- a yes/no whose answer is the probability that the
// document answers the query. That probability is the sort key.
//
// Why Noul rather than Score: Noul yields a continuous 0..1, which orders a
// window far more finely than Score's 2..10 ordinal levels, and ordering is the
// only thing this stage does.
//
// Known hazards, both handled by the caller rather than trusted away:
//
//   - A typed answer is guaranteed well-formed, never correct. Jev can return a
//     confident probability for the wrong document, so applyRerank confines the
//     damage to reordering a bounded window.
//   - Independent testing reports position bias (accuracy varying with where an
//     item sits in the input). Documents are labelled and questioned by label
//     rather than by position, and TestJevRerankIsStableUnderInputShuffle holds
//     the line on it.
//
// stdlib only: no SDK, no new module dependency.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	jevEndpointDefault = "https://api.typesafe.ai/v1/systemone"

	// jevModel is pinned, not "jev-latest". Cross-version probability drift is
	// undocumented, so an unpinned model would silently re-rank a user's search
	// results on the vendor's release schedule and invalidate any eval number
	// recorded against it.
	jevModel = "jev-1.13.0"

	// jevTimeout is short on purpose. The stage is an optional improvement to
	// ordering; if it cannot answer quickly the fused order ships instead.
	jevTimeout = 5 * time.Second
)

type jevReranker struct {
	apiKey   string
	client   *http.Client
	model    string
	endpoint string

	mu    sync.Mutex
	usage jevUsage
}

// jevUsage accumulates what this reranker actually spent, so an eval run can
// report measured cost instead of a projection.
type jevUsage struct {
	Calls        int
	InputTokens  int
	OutputTokens int
}

// Usage returns a snapshot of the accumulated spend.
func (j *jevReranker) Usage() jevUsage {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.usage
}

func newJevReranker() (*jevReranker, error) {
	key, err := typesafeAPIKey()
	if err != nil {
		return nil, err
	}
	return &jevReranker{
		apiKey:   key,
		client:   &http.Client{Timeout: jevTimeout},
		model:    jevModel,
		endpoint: jevEndpointDefault,
	}, nil
}

func (j *jevReranker) Name() string { return "jev/" + j.model }

// typesafeAPIKey resolves the key from the environment, falling back to the
// key file. The value is never logged, echoed, or attached to an error.
func typesafeAPIKey() (string, error) {
	if k := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")); k != "" {
		return k, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(home, ".typesafe", "api-key.env"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "TYPESAFE_API_KEY" {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if v != "" {
			return v, nil
		}
	}
	return "", errors.New("no TYPESAFE_API_KEY in environment or key file")
}

type jevQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

type jevRequest struct {
	State     string                 `json:"state"`
	Model     string                 `json:"model"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevAnswer struct {
	Type string  `json:"type"`
	Noul float64 `json:"noul"`
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (j *jevReranker) Score(ctx context.Context, query string, docs []rerankDoc) (map[string]float64, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	// Labels are positional only as identifiers; every question names its label
	// explicitly so the model is never asked to count or to infer order.
	labels := make([]string, len(docs))
	var state strings.Builder
	state.WriteString("SEARCH QUERY: ")
	state.WriteString(query)
	state.WriteString("\n\nCANDIDATE DOCUMENTS:\n")
	questions := make(map[string]jevQuestion, len(docs))
	for i, d := range docs {
		label := fmt.Sprintf("D%d", i+1)
		labels[i] = label
		fmt.Fprintf(&state, "\n[%s]\nTitle: %s\n%s\n", label, d.Title, d.Text)
		questions[label] = jevQuestion{
			Type: "noul",
			Instructions: fmt.Sprintf(
				"Document [%s] contains information that answers the SEARCH QUERY.", label),
			Criteria: map[string]string{
				"true":  "the document states, or directly evidences, what the query asks for",
				"false": "the document merely shares vocabulary with the query, or is about a different subject, person, or event",
			},
		}
	}

	body, err := json.Marshal(jevRequest{State: state.String(), Model: j.model, Questions: questions})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+j.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := j.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Status only. The body can echo submitted state, which does not belong
		// in a Mora error string.
		return nil, fmt.Errorf("jev rerank: unexpected status %d", resp.StatusCode)
	}
	var parsed jevResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	j.mu.Lock()
	j.usage.Calls++
	j.usage.InputTokens += parsed.Usage.InputTokens
	j.usage.OutputTokens += parsed.Usage.OutputTokens
	j.mu.Unlock()

	out := make(map[string]float64, len(docs))
	for i, d := range docs {
		a, ok := parsed.Answers[labels[i]]
		if !ok || a.Type != "noul" {
			continue // unscored: applyRerank keeps its fused position
		}
		out[d.ID] = a.Noul
	}
	if len(out) == 0 {
		return nil, errors.New("jev rerank: no usable answers")
	}
	return out, nil
}
