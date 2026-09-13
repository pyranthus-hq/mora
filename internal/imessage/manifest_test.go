package imessage

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestNeedsRenderRules(t *testing.T) {
	m := Manifest{Version: 1, WindowStart: 100, Chats: map[string]ChatMark{"g1": {MaxDate: 500, MinDate: 150}}}
	if !m.NeedsRender("unknown", 1, 100) {
		t.Fatal("unknown chat must render")
	}
	if m.NeedsRender("g1", 500, 100) {
		t.Fatal("unchanged chat must not render")
	}
	if !m.NeedsRender("g1", 501, 100) {
		t.Fatal("newer message must render")
	}
	if !m.NeedsRender("g1", 500, 160) {
		t.Fatal("window moved past MinDate must render")
	}
	m.Version = 2
	if !m.NeedsRender("g1", 500, 100) {
		t.Fatal("unknown version must render")
	}
}

func TestManifestRoundTripAndMissing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sync", "imessage-imessage.chats.json")
	got, err := LoadManifest(p)
	if err != nil || len(got.Chats) != 0 {
		t.Fatalf("missing manifest: %v %+v", err, got)
	}
	want := Manifest{Version: 1, WindowStart: 7, Chats: map[string]ChatMark{"g": {MaxDate: 1, MinDate: 1, Hash: "h"}}}
	if err := SaveManifest(p, want); err != nil {
		t.Fatal(err)
	}
	got, err = LoadManifest(p)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	want.Version = 2
	if err := SaveManifest(p, want); err != nil {
		t.Fatal(err)
	}
	got, err = LoadManifest(p)
	if err != nil || len(got.Chats) != 0 {
		t.Fatalf("unsupported version: %+v %v", got, err)
	}
}
