package ingest

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestOperationSourceKeyAndGoogleTokenPath(t *testing.T) {
	if got := OperationSourceKey(memory.Source{Type: "gmail", Account: "work"}); got != "gmail@work" {
		t.Fatalf("gmail=%q", got)
	}
	if got := OperationSourceKey(memory.Source{Type: "filesystem", Name: "docs", Account: "ignored"}); got != "filesystem@docs" {
		t.Fatalf("filesystem=%q", got)
	}
	cfg := config.Config{ConfigDir: "/config"}
	if got := GoogleTokenPath(cfg, ""); got != filepath.Join("/config", "tokens", "google.json") {
		t.Fatalf("default=%q", got)
	}
	if got := GoogleTokenPath(cfg, "work"); got != filepath.Join("/config", "tokens", "google-work.json") {
		t.Fatalf("work=%q", got)
	}
}
func TestGoogleWindows(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.Local)
	gmail := GoogleWindow(memory.Source{SinceDays: 30, LabelIDs: []string{"INBOX"}}, google.KindGmailThread, now)
	if !gmail.Since.Equal(now.AddDate(0, 0, -30)) || !reflect.DeepEqual(gmail.Labels, []string{"INBOX"}) {
		t.Fatalf("gmail=%+v", gmail)
	}
	def := GoogleWindow(memory.Source{}, google.KindGmailThread, now)
	if !def.Since.Equal(now.AddDate(0, 0, -90)) {
		t.Fatalf("default=%+v", def)
	}
	cal := GoogleWindow(memory.Source{Calendar: "primary"}, google.KindCalEvent, now)
	if !cal.Since.Equal(now.AddDate(0, -6, 0)) || !cal.Until.Equal(now.AddDate(0, 3, 0)) || cal.CalendarID != "primary" {
		t.Fatalf("cal=%+v", cal)
	}
}
func TestIMessageAndAppleCalendarWindows(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	if got := IMessageLookbackDays(memory.Source{}); got != 365 {
		t.Fatalf("default=%d", got)
	}
	im := IMessageWindow(memory.Source{SinceDays: 7}, now)
	if !im.Since.Equal(now.AddDate(0, 0, -7)) {
		t.Fatalf("im=%+v", im)
	}
	if im := IMessageWindow(memory.Source{SinceDays: -1}, now); !im.Since.IsZero() {
		t.Fatalf("alltime=%+v", im)
	}
	def := AppleCalendarWindow(memory.Source{}, now)
	if !def.Since.Equal(now.AddDate(0, 0, -90)) || !def.Until.Equal(now.AddDate(0, 0, 180)) {
		t.Fatalf("default=%+v", def)
	}
	all := AppleCalendarWindow(memory.Source{SinceDays: -1}, now)
	if !all.Since.IsZero() || !all.Until.Equal(now.AddDate(0, 0, 180)) {
		t.Fatalf("all=%+v", all)
	}
}
func TestFilesystemNamingAndCuratedClassification(t *testing.T) {
	if got := DefaultFilesystemSourceName(filepath.Join("a", "docs")); got != "docs" {
		t.Fatalf("name=%q", got)
	}
	if got := DefaultFilesystemSourceName(string(filepath.Separator)); got != "filesystem" {
		t.Fatalf("root=%q", got)
	}
	for _, ext := range []string{".MD", ".json", ".YML"} {
		if !CuratedAllowedExt(ext) {
			t.Errorf("allowed %q rejected", ext)
		}
	}
	if CuratedAllowedExt(".exe") {
		t.Fatal("exe allowed")
	}
	for _, ext := range []string{".DOCX", ".pdf"} {
		if !CuratedExtractExt(ext) {
			t.Errorf("extract %q rejected", ext)
		}
	}
	if CuratedExtractExt(".txt") {
		t.Fatal("txt extract")
	}
	for _, name := range []string{"go.mod", "README", "AGENTS.md"} {
		if !CuratedMetadataFile(name) {
			t.Errorf("metadata %q rejected", name)
		}
	}
	if CuratedMetadataFile("readme.md") {
		t.Fatal("case drift accepted")
	}
}
