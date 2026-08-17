package evidence

import (
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/memory"
	"testing"
)

func TestCitationJSONAndValidation(t *testing.T) {
	c, err := NewCitation("gmail_thread/t1", "gmail", "gmail:me@example.com", "2026-07-10T13:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"memory_id":"gmail_thread/t1","channel":"gmail","source":"gmail:me@example.com","date":"2026-07-10T13:00:00Z"}`
	if string(body) != want {
		t.Fatalf("json=%s", body)
	}
	var round Citation
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatal(err)
	}
	if round.MemoryID() != "gmail_thread/t1" || round.Channel() != "gmail" || round.Source() != "gmail:me@example.com" || round.Date() != "2026-07-10T13:00:00Z" {
		t.Fatalf("round=%+v", round)
	}
	for _, raw := range []string{`{"channel":"x","source":"x","date":"2026-01-01T00:00:00Z"}`, `{"memory_id":"x","source":"x","date":"2026-01-01T00:00:00Z"}`, `{"memory_id":"x","channel":"x","date":"2026-01-01T00:00:00Z"}`, `{"memory_id":"x","channel":"x","source":"x"}`, `{"memory_id":"x","channel":"x","source":"x","date":"bad"}`, `{`} {
		var got Citation
		if json.Unmarshal([]byte(raw), &got) == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	var malformed Citation
	if malformed.UnmarshalJSON([]byte("{")) == nil {
		t.Fatal("malformed JSON accepted")
	}
}

func TestCitationForMemoryFallbacksAndUTC(t *testing.T) {
	tests := []struct {
		name                              string
		m                                 memory.Memory
		source, date                      string
		wantChannel, wantSource, wantDate string
	}{{"provider", memory.Memory{ID: "m1", Provider: "gmail", Type: "email", Source: "gmail:me"}, "", "2026-01-02T04:04:05+01:00", "gmail", "gmail:me", "2026-01-02T03:04:05Z"}, {"type", memory.Memory{ID: "m2", Type: "note"}, "", "2026-01-02T03:04:05Z", "note", "note", "2026-01-02T03:04:05Z"},
		{"provider fallback", memory.Memory{ID: "m4", Provider: "gmail", Type: "email"}, "", "2026-01-02T03:04:05Z", "gmail", "gmail", "2026-01-02T03:04:05Z"}, {"explicit", memory.Memory{ID: "m3"}, " manual ", "bad", "manual", "manual", "bad"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ForMemory(tt.m, tt.source, tt.date)
			if tt.wantDate == "bad" {
				if err == nil {
					t.Fatal("bad date accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Channel() != tt.wantChannel || got.Source() != tt.wantSource || got.Date() != tt.wantDate {
				t.Fatalf("got=%s,%s,%s", got.Channel(), got.Source(), got.Date())
			}
		})
	}
}
