package mora

import (
	"bytes"
	"strings"
	"testing"
)

func TestCmdPrepRemoved(t *testing.T) {
	var out bytes.Buffer
	err := Run(testCtx(t), []string{"prep"}, &out, &out, strings.NewReader(""))
	if err == nil || err.Error() != "usage: mora brief --event-id <id>" {
		t.Fatalf("Run(prep) error = %v, want replacement usage error", err)
	}
	const want = "mora prep was removed (#137): use 'mora brief --event-id <id>' — same engine as MCP meeting_prep\n"
	if out.String() != want {
		t.Fatalf("Run(prep) output = %q, want %q", out.String(), want)
	}

	out.Reset()
	if err := Run(testCtx(t), []string{"help"}, &out, &out, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "mora prep") {
		t.Fatalf("help still advertises removed prep command:\n%s", out.String())
	}
}

func eventMemFull(id, title, occurredAt string, names map[string]string, attendees ...string) Memory {
	m := eventMem(id, title, occurredAt, attendees...)
	if names != nil {
		m.Meta["names"] = names
	}
	return m
}

func eventMem(id, title, occurredAt string, attendees ...string) Memory {
	meta := map[string]any{"occurred_at": occurredAt}
	if len(attendees) > 0 {
		anyAtt := make([]any, len(attendees))
		for i, a := range attendees {
			anyAtt[i] = a
		}
		meta["attendees"] = anyAtt
	}
	return Memory{
		ID: id, Type: "event", Title: title, Provider: "google",
		ProviderID: "calendar_event/" + id, Source: "calendar_event/" + id,
		CreatedAt: occurredAt, Meta: meta,
	}
}
