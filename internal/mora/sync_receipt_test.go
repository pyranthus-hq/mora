package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/imessage"
	"github.com/pyranthus-hq/mora/internal/memory"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestSyncSourceReceiptCarriesCodedError(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Apple Calendar live-open failure is macOS-only")
	}
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	// The temporary home has no Calendar database: exercise the real open failure.
	if err := saveSources(cfg, []Source{{Name: "applecalendar", Type: "applecalendar"}}); err != nil {
		t.Fatal(err)
	}
	out, _, err := runSplit(t, "sync", "applecalendar", "--json")
	if err == nil {
		t.Fatal("expected failure")
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err, out)
	}
	e, ok := r["error"].(map[string]any)
	if !ok || e["code"] != "connector_unavailable" || !strings.Contains(fmt.Sprint(e["message"]), "Full Disk Access") {
		t.Fatalf("receipt error = %+v", r["error"])
	}
	// Normalize only volatile time and the OS-specific open diagnostic.
	for _, key := range []string{"observed_at", "last_attempt_at", "next_scheduled_at"} {
		if r[key] == "" {
			t.Fatalf("missing %s", key)
		}
		r[key] = "<timestamp>"
	}
	if r["correlation_id"] == "" {
		t.Fatal("missing correlation id")
	}
	r["correlation_id"] = "<id>"
	r["duration_ms"] = float64(0)
	e["message"] = "cannot read your Calendar database (Full Disk Access not granted?) — run `mora doctor`: open failed"
	assertK17Golden(t, "variants/mora.sync.applecalendar.error.json", r)
}

func TestCodeOfWrappedErrors(t *testing.T) {
	e := newCodedError(errCodeConnectorUnavailable, nil, "open failed")
	for _, err := range []error{e, &e, fmt.Errorf("outer: %w", e), errors.Join(errors.New("other"), &e)} {
		code, msg, ok := codeOf(err)
		if !ok || code != errCodeConnectorUnavailable || msg != "open failed" {
			t.Fatalf("codeOf(%v) = %q, %q, %v", err, code, msg, ok)
		}
	}
	for _, err := range []error{nil, errors.New("plain")} {
		if _, _, ok := codeOf(err); ok {
			t.Fatal("unclassified error matched")
		}
	}
}

func assertK17Golden(t *testing.T, name string, got map[string]any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/contracts", name))
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("golden %s: got %+v, want %+v", name, got, want)
	}
}

func TestReceiptErrorSuccessAndUnclassified(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	for _, failure := range []error{nil, errors.New("partial read failed")} {
		var out bytes.Buffer
		if err := emitSyncSourceResult(mustConfig(t), &out, "filesystem", true, 3, "", failure); err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if failure == nil {
			if _, ok := doc["error"]; ok {
				t.Fatal("success acquired an error field")
			}
		} else {
			e, ok := doc["error"].(map[string]any)
			if !ok || e["code"] != "unclassified" || e["message"] != failure.Error() || doc["items"] != float64(3) {
				t.Fatal(doc)
			}
		}
	}
}

func TestConnectReceiptCarriesCodedError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	defer stubIMessageReadiness(t, true)()
	original := newIMessageFetcher
	defer func() { newIMessageFetcher = original }()
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		return connectTestFetcher{imessage.NewSyntheticFetcher(2, 1), func(context.Context, memory.ItemKind, memory.FetchWindow, string) (memory.Page, error) {
			return memory.Page{}, newCodedError(errCodeConnectorUnavailable, nil, "read failed")
		}}, nil
	}
	out, _, err := runSplit(t, "connect", "imessage", "--json", "--progress")
	if err == nil {
		t.Fatal("expected failure")
	}
	docs := decodeLines(t, out)
	r := docs[len(docs)-1]
	e, ok := r["error"].(map[string]any)
	if r["schema"] != "mora.connect.imessage" || !ok || e["code"] != "connector_unavailable" || e["message"] != "read failed" {
		t.Fatal(r)
	}
}
