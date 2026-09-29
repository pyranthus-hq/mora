package cases

import (
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

func TestPositiveEligibilityManifestsValidate(t *testing.T) {
	t.Parallel()
	for _, c := range AllPositiveEligibilityManifests() {
		c := c
		t.Run(c.CaseID, func(t *testing.T) {
			t.Parallel()
			if err := ValidateEligibilityManifest(c); err != nil {
				t.Fatalf("ValidateEligibilityManifest: %v", err)
			}
			if err := c.Validate(); err != nil {
				t.Fatalf("contract.Validate: %v", err)
			}
			if c.Eligibility.EligibleForHistoricalAttribution {
				t.Fatal("synthetic fixtures must not claim historical attribution")
			}
			if !c.MemorySnapshot.Synthetic || !c.DeliveredContext.Synthetic {
				t.Fatal("synthetic fixtures must mark snapshot and exposure synthetic")
			}
		})
	}
}

func TestSnapshotDistinctFromDeliveredContext(t *testing.T) {
	t.Parallel()
	c := ExamplePositiveFailureManifest()
	if c.MemorySnapshot.BytesSHA256 == "" || len(c.DeliveredContext.Parts) == 0 {
		t.Fatal("fixture incomplete")
	}
	if c.MemorySnapshot.SnapshotID == c.DeliveredContext.ManifestID {
		t.Fatal("snapshot_id must differ from manifest_id")
	}
	for _, p := range c.DeliveredContext.Parts {
		if p.BytesSHA256 == c.MemorySnapshot.BytesSHA256 {
			t.Fatalf("part %s hash equals snapshot hash", p.PartID)
		}
	}

	// Mutate: force equality → eligibility reject.
	c.DeliveredContext.Parts[0].BytesSHA256 = c.MemorySnapshot.BytesSHA256
	err := ValidateEligibilityManifest(c)
	if err == nil {
		t.Fatal("expected snapshot=exposure rejection")
	}
	if e, ok := err.(*Error); !ok || e.Code != CodeSnapshotEqualsExposure {
		t.Fatalf("want %s, got %v", CodeSnapshotEqualsExposure, err)
	}
}

func TestRequiredHashesCutoffAndReconstruction(t *testing.T) {
	t.Parallel()

	t.Run("missing_snapshot_hash", func(t *testing.T) {
		t.Parallel()
		c := ExamplePositiveFailureManifest()
		c.MemorySnapshot.BytesSHA256 = ""
		err := ValidateEligibilityManifest(c)
		if err == nil {
			t.Fatal("expected reject")
		}
		if e, ok := err.(*Error); !ok || e.Code != CodeMissingRequiredHash {
			t.Fatalf("want %s, got %v", CodeMissingRequiredHash, err)
		}
	})

	t.Run("missing_cutoff", func(t *testing.T) {
		t.Parallel()
		c := ExamplePositiveFailureManifest()
		c.Cutoff.CutoffAt = ""
		err := ValidateEligibilityManifest(c)
		if err == nil {
			t.Fatal("expected reject")
		}
		if e, ok := err.(*Error); !ok || e.Code != CodeMissingCutoff {
			t.Fatalf("want %s, got %v", CodeMissingCutoff, err)
		}
	})

	t.Run("reconstruction_without_note", func(t *testing.T) {
		t.Parallel()
		c := ExamplePositiveFailureManifest()
		c.Repository.ReconstructionNote = ""
		c.Notes = ""
		err := ValidateEligibilityManifest(c)
		if err == nil {
			t.Fatal("expected reject")
		}
		if e, ok := err.(*Error); !ok || e.Code != CodeRepoReconstruction {
			t.Fatalf("want %s, got %v", CodeRepoReconstruction, err)
		}
	})
}

func TestRejectionFixtures(t *testing.T) {
	t.Parallel()
	for _, fx := range AllRejectionFixtures() {
		fx := fx
		t.Run(fx.Name, func(t *testing.T) {
			t.Parallel()
			err := ValidateEligibilityManifest(fx.Case)
			if err == nil {
				t.Fatal("expected rejection")
			}
			e, ok := err.(*Error)
			if !ok {
				t.Fatalf("want *cases.Error, got %T %v", err, err)
			}
			if e.Code != fx.Code {
				t.Fatalf("want code %s, got %s (%v)", fx.Code, e.Code, err)
			}
		})
	}
}

func TestRolesCoverFailureAndTwoControls(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range AllPositiveEligibilityManifests() {
		seen[c.Role] = true
	}
	for _, want := range []string{
		contract.CaseRoleFailure,
		contract.CaseRoleValidMemoryControl,
		contract.CaseRoleNoRelevantMemoryCtrl,
	} {
		if !seen[want] {
			t.Fatalf("missing role %s", want)
		}
	}
}

func TestPublicProtocolSummary(t *testing.T) {
	t.Parallel()
	s := NewPublicProtocolSummary(time.Date(2026, 9, 29, 7, 0, 0, 0, time.UTC))
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if s.ClaimsMemoryCausedFailure {
		t.Fatal("must not claim causation")
	}
	if s.Decision != "public_protocol_only" {
		t.Fatalf("want public_protocol_only, got %s", s.Decision)
	}
	md := s.RenderMarkdown()
	for _, needle := range []string{"#542", "outside_public_repo", "Handoff", "Synthetic only"} {
		if !strings.Contains(md, needle) {
			t.Fatalf("markdown missing %q", needle)
		}
	}

	bad := s
	bad.ClaimsMemoryCausedFailure = true
	if err := bad.Validate(); err == nil {
		t.Fatal("expected causation claim reject")
	}
}

func TestNoPrivateHostPathsInFixtures(t *testing.T) {
	t.Parallel()
	all := append(AllPositiveEligibilityManifests(),
		ExampleRejectMissingDeliveredContext(),
		ExampleRejectMissingDirtyFiles(),
		ExampleRejectUnknownPermission(),
		ExampleRejectPostCutoffRepairEvidence(),
	)
	for _, c := range all {
		for _, p := range c.RunnerPackage.ContenderVisiblePaths {
			if strings.HasPrefix(p, "/Users/") || strings.HasPrefix(p, "/home/") || strings.Contains(p, "Library/") {
				t.Fatalf("%s embeds private host path %q", c.CaseID, p)
			}
		}
		for _, d := range c.Repository.DirtyFiles {
			if strings.HasPrefix(d.Path, "/Users/") || strings.HasPrefix(d.Path, "/home/") {
				t.Fatalf("%s dirty path %q looks private", c.CaseID, d.Path)
			}
		}
	}
}
