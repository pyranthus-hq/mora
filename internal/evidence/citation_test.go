package evidence

import (
	"encoding/json"
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
