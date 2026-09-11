package mora

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	doctorpkg "github.com/pyranthus-hq/mora/internal/doctor"
)

type fakeFileInfo struct{ os.FileInfo }

func checkSeams(goos string, present bool, readable bool, probeErr error) doctorpkg.IMessageSeams {
	return doctorpkg.IMessageSeams{
		GOOS:       func() string { return goos },
		ChatDBPath: func() string { return "/synthetic/Library/Messages/chat.db" },
		Stat: func(string) (os.FileInfo, error) {
			if present {
				return fakeFileInfo{}, nil
			}
			return nil, os.ErrNotExist
		},
		ProbeReadable: func(string) (bool, error) { return readable, probeErr },
	}
}

func TestIMessageAccessCheckGranted(t *testing.T) {
	rep := imessageAccessCheck(checkSeams("darwin", true, true, nil), time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC))
	if !rep.OK || !rep.PlatformSupported || !rep.ChatDBPresent || !rep.Readable || rep.Reason != "" {
		t.Fatalf("granted report wrong: %+v", rep)
	}
	if rep.ObservedAt != "2026-09-10T00:00:00Z" {
		t.Fatalf("observed_at = %q", rep.ObservedAt)
	}
}

func TestIMessageAccessCheckDenied(t *testing.T) {
	rep := imessageAccessCheck(checkSeams("darwin", true, false, errors.New("unable to open database file: operation not permitted")), time.Now())
	if rep.OK || !rep.ChatDBPresent || rep.Readable || rep.Reason != "not_readable" {
		t.Fatalf("denied report wrong: %+v", rep)
	}
	if !strings.Contains(rep.Detail, "operation not permitted") {
		t.Fatalf("detail must quote the probe error, got %q", rep.Detail)
	}
}

func TestIMessageAccessCheckMissingAndUnsupported(t *testing.T) {
	missing := imessageAccessCheck(checkSeams("darwin", false, false, nil), time.Now())
	if missing.Reason != "chat_db_missing" || missing.ChatDBPresent {
		t.Fatalf("missing report wrong: %+v", missing)
	}
	linux := imessageAccessCheck(checkSeams("linux", true, true, nil), time.Now())
	if linux.Reason != "unsupported_platform" || linux.PlatformSupported || linux.OK {
		t.Fatalf("linux report wrong: %+v", linux)
	}
}

func TestDoctorCheckCLIEmitsVersionedDocument(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	stdout, stderr, err := runSplit(t, "doctor", "check", "imessage-access", "--json")
	if err != nil {
		t.Fatalf("doctor check: %v (stderr %q)", err, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if doc["schema"] != "mora.doctor.check" || doc["schema_version"] != float64(1) || doc["check"] != "imessage-access" {
		t.Fatalf("envelope wrong: %v", doc)
	}
	if _, ok := doc["ok"].(bool); !ok {
		t.Fatalf("ok must be a bool: %v", doc)
	}
}

func TestDoctorCheckRefusesUnknownNameAndProse(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	if _, err := runErr(t, "doctor", "check", "gmail-access", "--json"); err == nil {
		t.Fatal("unknown check name must fail")
	}
	if _, err := runErr(t, "doctor", "check", "imessage-access"); err == nil {
		t.Fatal("doctor check without --json must fail; the human form is " + "`mora doctor`")
	}
}
