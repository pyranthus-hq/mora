package mora

import (
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
