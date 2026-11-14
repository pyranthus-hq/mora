package mora

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/pyranthus-hq/mora/internal/memory"
)

const projectRuleApplicability = "always"

type projectRuleRevision struct {
	RuleID, SourceMemoryID, Scope, Applicability, Text string
	RevisionSHA256, Provenance                         string
}

type projectRuleKey struct{ RuleID, RevisionSHA256 string }

// loadProjectRuleRevision is deliberately local-only: a shared or unconfirmed
// record can remain useful context, but cannot become a project instruction.
func loadProjectRuleRevision(cfg Config, binding projectBinding, id string) (projectRuleRevision, error) {
	if binding.Scope == "" || id == "" {
		return projectRuleRevision{}, errors.New("invalid project rule identity")
	}
	m, err := findMemory(cfg, id)
	if err != nil {
		return projectRuleRevision{}, err
	}
	governance, err := loadGovernance(cfg)
	if err != nil || m.ID != id || m.Scope != binding.Scope || m.DeletedAt != "" || !governance.memoryVisible(m.ID) || (m.Type != memory.TypeDecision && m.Type != memory.TypeCorrection) || memory.DeriveProvenance(m) != memory.ProvenanceAuthored || !memory.IsOwnerConfirmed(m) || strings.Contains(m.Text, projectRuleBegin) || strings.Contains(m.Text, projectRuleEnd) {
		return projectRuleRevision{}, fmt.Errorf("project rule is not a current confirmed local authored record")
	}
	return projectRuleRevision{RuleID: id, SourceMemoryID: id, Scope: m.Scope, Applicability: projectRuleApplicability, Text: m.Text, RevisionSHA256: projectRuleRevisionSHA256(m, projectRuleApplicability), Provenance: memory.DeriveProvenance(m)}, nil
}

func projectRuleRevisionSHA256(m Memory, applicability string) string {
	h := sha256.New()
	h.Write([]byte("mora-project-rule-revision-v1\x00"))
	h.Write([]byte(memory.OwnerConfirmedDigest(m)))
	h.Write([]byte("\x00" + applicability))
	return hex.EncodeToString(h.Sum(nil))
}
