package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func statusSeedTentative(t *testing.T, cfg Config, id, scope, title string) {
	t.Helper()
	pbSeed(t, cfg, id, scope, "decision", title, "Body of "+title, 3, func(m *Memory) { m.Tags = []string{memory.TentativeTag} })
}

func TestStatusTextUnboundEmptyVault(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	t.Chdir(t.TempDir())
	out := run(t, "status")
	for _, want := range []string{"Folder:  not bound to a project", "Vault:", "Health:", "Review:  nothing waits for review", "Given:   nothing handed to an agent yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
}

func TestStatusShowsBindingReviewQueueAndLastGiven(t *testing.T) {
	cfg := projectBriefSetup(t)
	dir := pbBind(t)
	t.Chdir(dir)
	statusSeedTentative(t, cfg, "mem_t1", pbScope, "Package manager is pnpm")
	pbSeed(t, cfg, "mem_g1", pbScope, "decision", "Deploy through staging", "Deploys go through staging.", 4)
	if err := appendGiven(cfg, givenLine{Surface: "hook-recall", Kind: "recall", Caller: "antigravity", Dir: dir}, []string{"mem_g1", "mem_gone"}, 2); err != nil {
		t.Fatal(err)
	}
	out := run(t, "status")
	for _, want := range []string{"bound to " + pbScope, "1 saved (tentative), 0 held (proposals)", "hook-recall to antigravity, 2 notes", "mem_g1  Deploy through staging", "mem_gone"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}

	var rep struct {
		Folder statusFolder `json:"folder"`
		Review statusReview `json:"review"`
		Given  *statusGiven `json:"last_given"`
	}
	if err := json.Unmarshal([]byte(run(t, "status", "--json")), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Folder.Bound || rep.Folder.Scope != pbScope || rep.Review.Saved != 1 || rep.Given == nil || len(rep.Given.Notes) != 2 || rep.Given.Notes[0].Title != "Deploy through staging" {
		t.Fatalf("json report = %+v", rep)
	}
}

func TestStatusFlagValidation(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	for _, args := range [][]string{{"status", "--web", "--json"}, {"status", "--port", "9000"}, {"status", "extra"}, {"status", "--web", "--port", "70000"}} {
		if _, err := runErr(t, args...); err == nil {
			t.Errorf("%v must be a usage error", args)
		}
	}
}

func TestStatusHandlerIsReadOnlyEscapedAndLoopbackOnly(t *testing.T) {
	cfg := projectBriefSetup(t)
	dir := pbBind(t)
	statusSeedTentative(t, cfg, "mem_x", pbScope, `<script>alert("x")</script>`)
	const tok = "0123456789abcdef"
	srv := httptest.NewServer(newStatusHandler(cfg, dir, time.Now, tok))
	defer srv.Close()

	get := func(path, host string) (int, string, http.Header) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/"+tok+path, nil)
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}

	code, body, hdr := get("/", "")
	if code != 200 || !strings.Contains(body, "Mora status") || !strings.Contains(body, pbScope) {
		t.Fatalf("page = %d\n%s", code, body)
	}
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("note title was not escaped:\n%s", body)
	}
	for _, ext := range []string{"http://", "https://", "<script", "<link", "<img", "src="} {
		if strings.Contains(body, ext) {
			t.Errorf("page references %q: no external assets or scripts are allowed", ext)
		}
	}
	if csp := hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v", hdr)
	}

	if code, _, _ := get("/", "evil.example.com"); code != http.StatusForbidden {
		t.Fatalf("a non-loopback Host must be refused, got %d", code)
	}
	if code, _, _ := get("/", "localhost:7781"); code != 200 {
		t.Fatalf("localhost Host must work, got %d", code)
	}
	if code, _, _ := get("/nope", ""); code != http.StatusNotFound {
		t.Fatalf("unknown path = %d", code)
	}
	// Without the per-run token the page does not exist, whatever the path.
	for _, bare := range []string{"/", "/status.json", "/wrong/", "/" + tok + "x/"} {
		resp, err := http.Get(srv.URL + bare)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || strings.Contains(string(b), "scope") {
			t.Fatalf("GET %s without the token = %d, want 404 and no content", bare, resp.StatusCode)
		}
	}
	code, body, _ = get("/status.json", "")
	var rep statusReport
	if code != 200 || json.Unmarshal([]byte(body), &rep) != nil || rep.Review.Saved != 1 {
		t.Fatalf("/status.json = %d %s", code, body)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(method, srv.URL+"/"+tok+"/", strings.NewReader("x=1"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405: the page must have no write path", method, resp.StatusCode)
		}
	}
	// Serving the page changed nothing: the tentative note is still there.
	if notes, err := listReviewNotes(cfg, io.Discard); err != nil || len(notes) != 1 {
		t.Fatalf("review queue after serving = %d, %v", len(notes), err)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestServeStatusWebBindsLoopbackAndStops(t *testing.T) {
	cfg := projectBriefSetup(t)
	ctx, cancel := context.WithCancel(testCtx(t))
	out, errOut := &syncBuffer{}, &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- serveStatusWeb(ctx, cfg, t.TempDir(), 0, out, errOut) }()
	var addr string
	for i := 0; i < 200 && addr == ""; i++ {
		if _, rest, ok := strings.Cut(out.String(), "http://"); ok {
			addr, _, _ = strings.Cut(rest, "\n")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		cancel()
		t.Fatalf("server must listen on 127.0.0.1 only, announced %q", addr)
	}
	host, path, _ := strings.Cut(addr, "/")
	if len(strings.Trim(path, "/")) != 64 {
		cancel()
		t.Fatalf("the announced address must carry the 64-hex-character run token, got %q", addr)
	}
	resp, err := http.Get("http://" + addr)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if bare, err := http.Get("http://" + host + "/"); err != nil || bare.StatusCode != http.StatusNotFound {
		t.Fatalf("the bare address must be 404, got %v %v", bare, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestStatusHostOK(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:7781": true, "localhost:7781": true, "[::1]:7781": true, "127.0.0.1": true,
		"evil.example:7781": false, "127.0.0.1.evil.example": false, "": false, "0.0.0.0:7781": false,
	} {
		if got := statusHostOK(host); got != want {
			t.Errorf("statusHostOK(%q) = %v, want %v", host, got, want)
		}
	}
}
