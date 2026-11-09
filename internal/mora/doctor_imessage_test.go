package mora

import (
	"bytes"
	"strings"
	"testing"
)

func TestDoctorIMessageSourceGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources []Source
		want    bool
	}{
		{"AC-001.1 no sources", nil, false},
		{"AC-001.2 disabled", []Source{{Name: "imessage", Type: "imessage", Enabled: boolPtrDoctor(false)}}, false},
		{"AC-001.3 enabled", []Source{{Name: "imessage", Type: "imessage", Enabled: boolPtrDoctor(true)}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			orig := runtimeGOOS
			t.Cleanup(func() { runtimeGOOS = orig })
			runtimeGOOS = func() string { return "darwin" }
			coreAMkChatDB(t, []byte("not a sqlite file"))
			cfg := mustConfig(t)
			if err := saveSources(cfg, tc.sources); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := cmdDoctor(testCtx(t), nil, &out, testStderr); err != nil {
				t.Fatal(err)
			}
			for _, phrase := range []string{"iMessage", "chat.db", "Full Disk Access"} {
				if strings.Contains(out.String(), phrase) != tc.want {
					t.Fatalf("presence of %q should be %v:\n%s", phrase, tc.want, out.String())
				}
			}
			if tc.want {
				var block bytes.Buffer
				printIMessageReadiness(cfg, &block, false)
				if !strings.HasSuffix(out.String(), block.String()) {
					t.Fatalf("readiness block changed:\n%s", out.String())
				}
			}
			// AC-001.4 WhatsApp retains its existing block with and without iMessage.
			sources := append(tc.sources, Source{Name: "whatsapp", Type: "whatsapp"})
			if err := saveSources(cfg, sources); err != nil {
				t.Fatal(err)
			}
			var withWhatsApp, block bytes.Buffer
			if err := cmdDoctor(testCtx(t), nil, &withWhatsApp, testStderr); err != nil {
				t.Fatal(err)
			}
			printWhatsAppReadiness(cfg, &block)
			if !strings.HasSuffix(withWhatsApp.String(), block.String()) {
				t.Fatalf("WhatsApp block changed:\n%s", withWhatsApp.String())
			}
			// Existing check lines remain present after adding a connector.
			for _, line := range strings.Split(out.String(), "\n") {
				if (strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "warn ")) && !strings.Contains(line, "storage ") {
					if !strings.Contains(withWhatsApp.String(), line+"\n") {
						t.Fatalf("lost doctor check %q", line)
					}
				}
			}
		})
	}
}

func boolPtrDoctor(v bool) *bool { return &v }
