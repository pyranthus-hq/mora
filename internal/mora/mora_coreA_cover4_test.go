package mora

// mora_coreA_cover4_test.go — coreA coverage worker, part 4. The last residual
// error branches: loadSources failures in the source-mutation helpers, and the
// init/search argument-validation guards.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCoreA_SourceHelpersLoadError corrupts sources.json so loadSources() fails,
// then confirms every helper that reads it surfaces the error instead of writing
// over a file it could not parse.
func TestCoreA_SourceHelpersLoadError(t *testing.T) {
	cfg := coreADirsCfg(t)
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Invalid JSON => loadSources returns an unmarshal error.
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "sources.json"), []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := setSourceEnabledByName(cfg, "gmail", true); err == nil {
		t.Error("setSourceEnabledByName must surface the loadSources error")
	}
	if err := setSourceSinceDaysByName(cfg, "gmail", 30); err == nil {
		t.Error("setSourceSinceDaysByName must surface the loadSources error")
	}
	if err := setSourceSinceDays(cfg, "gmail", 30); err == nil {
		t.Error("setSourceSinceDays must surface the loadSources error")
	}
	if err := setSourceEnabled(cfg, "gmail", true); err == nil {
		t.Error("setSourceEnabled must surface the loadSources error")
	}
	if err := setSourceEmailByAccount(cfg, "", "a@b.com"); err == nil {
		t.Error("setSourceEmailByAccount must surface the loadSources error")
	}
	if err := setIMessageDenyList(cfg, nil, nil); err == nil {
		t.Error("setIMessageDenyList must surface the loadSources error")
	}

	var out bytes.Buffer
	// enableConnector for a no-auth type goes straight to setSourceEnabled, which
	// hits the corrupt file.
	if err := enableConnector(context.Background(), cfg, "filesystem", &out, testStderr, strings.NewReader("")); err == nil {
		t.Error("enableConnector must surface the setSourceEnabled/loadSources error")
	}
	if err := disableConnector(cfg, "gmail", &out); err == nil {
		t.Error("disableConnector must surface the setSourceEnabled/loadSources error")
	}
}

// TestCoreA_CmdInitErrors covers cmdInit's flag-parse and loadConfig guards.
func TestCoreA_CmdInitErrors(t *testing.T) {
	// Flag parse error.
	withTempHome(t)
	if _, err := runErr(t, "init", "--bogus"); err == nil {
		t.Fatal("init with an unknown flag must error")
	}

	// loadConfig error: config.toml is a directory.
	withTempHome(t)
	dir := configDirFor(t)
	if err := os.MkdirAll(filepath.Join(dir, "config.toml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runErr(t, "init"); err == nil {
		t.Fatal("init must surface the loadConfig error")
	}
}

// TestCoreA_CmdSearchArgErrors covers the parseSearchArgs error branch in cmdSearch.
func TestCoreA_CmdSearchArgErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	for _, args := range [][]string{
		{"search", "q", "--limit", "not-a-number"},
		{"search", "q", "--limit"},
		{"search", "q", "--scope"},
	} {
		if _, err := runErr(t, args...); err == nil {
			t.Errorf("%v should surface a search-args error", args)
		}
	}
}
