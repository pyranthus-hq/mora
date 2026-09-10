package mora

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestActivitySearchFlagsFreeTextRepeatExclusionsAndErrors(t *testing.T) {
	rest, args, err := extractActivitySearchFlags([]string{"offer", "--source=gmail:work", "--event-since-hours", "24", "status", "--dispositions=exclude:not-context", "--dispositions", "exclude:done", "--dispositions", "exclude:not-context", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rest, []string{"offer", "status", "--json"}) {
		t.Fatal(rest)
	}
	f, err := parseSearchFilters(args, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if f.EventSinceHours != 24 || f.Source != "gmail:work" || !reflect.DeepEqual(f.ExcludeDispositions, []string{"done", "not-context"}) {
		t.Fatalf("wrong flags %+v", f)
	}
	for _, args := range [][]string{{"--event-since-hours"}, {"--event-since-hours", "0"}, {"--event-since-hours", "-1"}, {"--event-since-hours", "8785"}, {"--event-since-hours", "1.5"}, {"--event-since-hours", "1", "--event-since-hours", "2"}, {"--dispositions"}, {"--dispositions", "include:keep"}, {"--dispositions", "exclude:"}} {
		if _, _, err := extractActivitySearchFlags(args); err == nil {
			t.Fatalf("invalid flags accepted %v", args)
		}
	}
}

func TestActivitySearchEmptyCLIReceiptIsArray(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	raw := run(t, "search", "definitelyabsent", "--source", "gmail", "--event-since-hours", "24", "--dispositions", "exclude:not-context", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	rows, ok := got["memories"].([]any)
	if !ok || len(rows) != 0 {
		t.Fatalf("empty filtered receipt must be array: %s", raw)
	}
	if got["excluded_by_disposition"] != float64(0) {
		t.Fatal("zero count missing", got)
	}
}
