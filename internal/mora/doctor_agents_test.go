package mora

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAgentRemoteDefaultRelaxations(t *testing.T) {
	cases := []struct {
		name string
		p    agentProfile
		want []string
	}{
		{
			name: "remote defaults",
			p:    agentProfile{Name: "muse", ReadScopes: []string{"global"}, Write: mcpWritePolicyPropose, Tools: append([]string(nil), agentDefaultTools...)},
			want: nil,
		},
		{
			name: "readonly is stricter not a relaxation",
			p:    agentProfile{Name: "r", ReadScopes: []string{"global"}, Write: mcpWritePolicyReadonly, Tools: []string{"search_memory", "read_memory", "list_memory"}},
			want: nil,
		},
		{
			name: "raw sources on",
			p:    agentProfile{Name: "raw", ReadScopes: []string{"global"}, Write: mcpWritePolicyPropose, RawSources: true, Tools: append([]string(nil), agentDefaultTools...)},
			want: []string{"raw_sources=on"},
		},
		{
			name: "write open",
			p:    agentProfile{Name: "open", ReadScopes: []string{"global"}, Write: mcpWritePolicyOpen, Tools: append([]string(nil), agentDefaultTools...)},
			want: []string{"write=open"},
		},
		{
			name: "all three",
			p: agentProfile{
				Name: "wide", ReadScopes: []string{agentScopesAll}, Write: mcpWritePolicyOpen, RawSources: true,
				Tools: []string{"search_memory", "read_memory", "list_memory", "write_memory", "delete_memory"},
			},
			want: []string{"raw_sources=on", "write=open", "delete_memory=allowed"},
		},
	}
	for _, tc := range cases {
		got := tc.p.remoteDefaultRelaxations()
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestDoctorFlagsAgentRemoteDefaultRelaxations(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	var outBuf strings.Builder
	// Compliant profile: remote defaults. Must not appear.
	if err := cmdAgents(testCtx(t), []string{"add", "muse", "--read-scopes", "project:widget"}, &outBuf, &outBuf); err != nil {
		t.Fatalf("add muse: %v", err)
	}
	// Raw sources on.
	if err := cmdAgents(testCtx(t), []string{"add", "rawer", "--read-scopes", "project:widget", "--raw-sources"}, &outBuf, &outBuf); err != nil {
		t.Fatalf("add rawer: %v", err)
	}
	// Write open (still scoped tools; no delete).
	if err := cmdAgents(testCtx(t), []string{"add", "opener", "--read-scopes", "project:widget", "--write", "open"}, &outBuf, &outBuf); err != nil {
		t.Fatalf("add opener: %v", err)
	}
	// Full relaxation: all scopes + raw + open + delete_memory.
	if err := cmdAgents(testCtx(t), []string{
		"add", "wide", "--read-scopes", "all", "--raw-sources", "--write", "open",
		"--tools", "search_memory,read_memory,list_memory,write_memory,delete_memory",
	}, &outBuf, &outBuf); err != nil {
		t.Fatalf("add wide: %v", err)
	}
	// Revoked profile that would have relaxed must be ignored.
	if err := cmdAgents(testCtx(t), []string{"add", "gone", "--read-scopes", "project:widget", "--raw-sources"}, &outBuf, &outBuf); err != nil {
		t.Fatalf("add gone: %v", err)
	}
	if err := cmdAgents(testCtx(t), []string{"revoke", "gone"}, &outBuf, &outBuf); err != nil {
		t.Fatalf("revoke gone: %v", err)
	}
	_ = cfg

	human := run(t, "doctor")
	for _, want := range []string{
		`agent profile "rawer" relaxes remote defaults: raw_sources=on`,
		`agent profile "opener" relaxes remote defaults: write=open`,
		`agent profile "wide" relaxes remote defaults: raw_sources=on, write=open, delete_memory=allowed`,
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("doctor text missing %q:\n%s", want, human)
		}
	}
	for _, leak := range []string{`agent profile "muse"`, `agent profile "gone"`} {
		if strings.Contains(human, leak) {
			t.Fatalf("doctor text unexpectedly mentions %q:\n%s", leak, human)
		}
	}

	var rep doctorReport
	if err := json.Unmarshal([]byte(run(t, "doctor", "--json")), &rep); err != nil {
		t.Fatalf("doctor --json: %v", err)
	}
	if rep.AgentRelaxations == nil {
		t.Fatal("agent_relaxations must be a non-null array")
	}
	byName := map[string]agentRemoteRelaxation{}
	for _, r := range rep.AgentRelaxations {
		byName[r.Name] = r
	}
	if _, ok := byName["muse"]; ok {
		t.Fatalf("compliant muse listed in agent_relaxations: %+v", rep.AgentRelaxations)
	}
	if _, ok := byName["gone"]; ok {
		t.Fatalf("revoked gone listed in agent_relaxations: %+v", rep.AgentRelaxations)
	}
	if got := byName["rawer"].Relaxations; strings.Join(got, ",") != "raw_sources=on" {
		t.Fatalf("rawer = %v", got)
	}
	if got := byName["opener"].Relaxations; strings.Join(got, ",") != "write=open" {
		t.Fatalf("opener = %v", got)
	}
	if got := byName["wide"].Relaxations; strings.Join(got, ",") != "raw_sources=on,write=open,delete_memory=allowed" {
		t.Fatalf("wide = %v", got)
	}
	for _, name := range []string{"rawer", "opener", "wide"} {
		found := false
		for _, c := range rep.Checks {
			if c.Name == "agent_remote_defaults:"+name {
				found = true
				if c.OK || c.Critical {
					t.Fatalf("check %s = %+v; want ok=false critical=false", name, c)
				}
			}
		}
		if !found {
			t.Fatalf("missing check agent_remote_defaults:%s in %+v", name, rep.Checks)
		}
	}
	// Advisory: relaxing profiles must not flip .healthy by themselves.
	// (Other checks may still fail in this fixture; assert only that the
	// agent checks are non-critical, which we already did above.)
}

func TestDoctorAgentRelaxationsEmptyWithoutProfiles(t *testing.T) {
	coreBIngestInitCfg(t)
	var rep doctorReport
	if err := json.Unmarshal([]byte(run(t, "doctor", "--json")), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.AgentRelaxations == nil {
		t.Fatal("agent_relaxations must be [] not null")
	}
	if len(rep.AgentRelaxations) != 0 {
		t.Fatalf("want empty agent_relaxations, got %+v", rep.AgentRelaxations)
	}
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.Name, "agent_remote_defaults:") || c.Name == "agents_store_readable" {
			t.Fatalf("unexpected agent check without profiles: %+v", c)
		}
	}
}

func TestDoctorSurfacesCorruptAgentsStore(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentsStorePath(cfg), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	human := run(t, "doctor")
	if !strings.Contains(human, "agents store unreadable") {
		t.Fatalf("doctor text missing corrupt-store line:\n%s", human)
	}
	var rep doctorReport
	if err := json.Unmarshal([]byte(run(t, "doctor", "--json")), &rep); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range rep.Checks {
		if c.Name == "agents_store_readable" {
			found = true
			if c.OK || c.Critical {
				t.Fatalf("agents_store_readable = %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("missing agents_store_readable check")
	}
}
