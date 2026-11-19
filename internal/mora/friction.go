package mora

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/pyranthus-hq/mora/internal/leasefile"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"github.com/pyranthus-hq/mora/internal/memoryfile"
)

const frictionArg = "friction_record"

type frictionRecord struct {
	Kind         string `json:"kind,omitempty"`
	SourceID     string `json:"source_id,omitempty"`
	ReportID     string `json:"report_id,omitempty"`
	At           string `json:"at"`
	Author       string `json:"author"`
	Source       string `json:"source"`
	Evidence     string `json:"evidence,omitempty"`
	Symptom      string `json:"symptom,omitempty"`
	Expected     string `json:"expected,omitempty"`
	Summary      string `json:"summary,omitempty"`
	Outcome      string `json:"outcome,omitempty"`
	AppliesWhen  string `json:"applies_when,omitempty"`
	RunUnblocked *bool  `json:"run_unblocked,omitempty"`
	DefectOpen   *bool  `json:"defect_open,omitempty"`
}

type frictionPacket struct {
	Scope      string           `json:"scope"`
	Report     frictionRecord   `json:"report"`
	Recoveries []frictionRecord `json:"recoveries"`
}

var frictionRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,159}$`)
var frictionReportID = regexp.MustCompile(`^mem_friction_report_[0-9a-f]{32}$`)
var frictionFeedbackID = regexp.MustCompile(`^feedback-[0-9a-f]{16}$`)
var frictionRecoveryID = regexp.MustCompile(`^recovery-[0-9a-f]{16,64}$`)

func decodeFriction(r io.Reader, target any) error {
	const maxBytes = 64 << 10
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxBytes {
		return errors.New("friction JSON exceeds 64 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected a single bounded JSON value")
	}
	return nil
}

func validFrictionScope(scope string) bool {
	return strings.HasPrefix(scope, "project:") && len(scope) > len("project:") && memoryfile.ValidateWriteScope(scope) == nil
}

func frictionMemory(args map[string]any, raw string) (Memory, error) {
	scope := strArg(args, "scope", "")
	if !validFrictionScope(scope) {
		return Memory{}, errors.New("friction requires a project scope")
	}
	var r frictionRecord
	if err := decodeFriction(strings.NewReader(raw), &r); err != nil {
		return Memory{}, err
	}
	m, err := r.memory(scope)
	if err != nil {
		return Memory{}, err
	}
	if strArg(args, "title", "") != m.Title || strArg(args, "text", "") != m.Text ||
		strArg(args, "type", "") != m.Type || strArg(args, "source", "") != m.Source {
		return Memory{}, errors.New("friction proposal fields differ from the record")
	}
	return m, nil
}

func (r frictionRecord) memory(scope string) (Memory, error) {
	if _, err := time.Parse(time.RFC3339, r.At); err != nil {
		return Memory{}, errors.New("friction requires an RFC3339 observation date")
	}
	if !frictionRef.MatchString(r.Author) || !frictionRef.MatchString(r.Source) ||
		(r.Evidence != "" && !frictionRef.MatchString(r.Evidence)) {
		return Memory{}, errors.New("author, source and evidence must be short opaque references")
	}
	for _, s := range []string{r.Symptom, r.Expected, r.Summary, r.AppliesWhen} {
		if len(s) > 900 || strings.ContainsFunc(s, func(c rune) bool {
			return unicode.IsControl(c) || unicode.Is(unicode.Cf, c)
		}) {
			return Memory{}, errors.New("friction text must be a short single line")
		}
	}
	var title, body string
	switch r.Kind {
	case "report":
		if strings.TrimSpace(r.Symptom) == "" || strings.TrimSpace(r.Expected) == "" ||
			r.ReportID != "" || r.Summary != "" || r.Outcome != "" || r.AppliesWhen != "" ||
			r.RunUnblocked != nil || r.DefectOpen != nil ||
			(r.SourceID != "" && !frictionFeedbackID.MatchString(r.SourceID)) {
			return Memory{}, errors.New("report requires symptom and expected behavior")
		}
		title = r.Symptom
		body = fmt.Sprintf("Symptom: %s\nExpected: %s", r.Symptom, r.Expected)
	case "recovery":
		if !frictionReportID.MatchString(r.ReportID) ||
			strings.TrimSpace(r.Summary) == "" || strings.TrimSpace(r.AppliesWhen) == "" || r.RunUnblocked == nil ||
			r.Evidence == "" || r.Symptom != "" || r.Expected != "" ||
			(r.SourceID != "" && !frictionRecoveryID.MatchString(r.SourceID)) ||
			(r.Outcome != "worked" && r.Outcome != "failed" && r.Outcome != "counterexample") {
			return Memory{}, errors.New("recovery requires a report ID, summary, applicability, evidence, outcome and run_unblocked")
		}
		title = "Friction recovery: " + r.Outcome
		body = fmt.Sprintf("Summary: %s\nApplies when: %s\nRun unblocked: %t", r.Summary, r.AppliesWhen, *r.RunUnblocked)
		if r.DefectOpen != nil {
			body += fmt.Sprintf("\nDefect still open: %t", *r.DefectOpen)
		}
	default:
		return Memory{}, errors.New("invalid friction record kind")
	}
	if r.SourceID == "" && r.Evidence == "" {
		return Memory{}, errors.New("native friction requires an evidence reference")
	}
	body += fmt.Sprintf("\nAuthor: %s\nObserved: %s\nEvidence: %s", r.Author, r.At, r.Evidence)
	if r.SourceID != "" {
		body += "\nDrona source ID: " + r.SourceID
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return Memory{}, err
	}
	if _, redacted := redactProposalText(title + "\n" + body + "\n" + r.Source + "\n" + string(raw)); redacted {
		return Memory{}, errors.New("redact credential-like values before recording friction")
	}
	return Memory{Scope: scope, Type: "fact", Title: title, Text: body, Source: r.Source,
		CreatedAt: r.At, Meta: map[string]any{frictionArg: string(raw)}}, nil
}

func frictionID(scope, kind, identity string) string {
	hash := sha256.Sum256([]byte(scope + "\x00" + kind + "\x00" + identity))
	return fmt.Sprintf("mem_friction_%s_%x", kind, hash[:16])
}

func (r frictionRecord) args(scope string) (map[string]any, error) {
	m, err := r.memory(scope)
	if err != nil {
		return nil, err
	}
	return map[string]any{"scope": scope, "type": m.Type, "title": m.Title, "text": m.Text,
		"source": m.Source, frictionArg: m.Meta[frictionArg]}, nil
}

func frictionFromMemory(m Memory) (frictionRecord, error) {
	raw, ok := m.Meta[frictionArg].(string)
	if !ok {
		return frictionRecord{}, errors.New("missing friction metadata")
	}
	var r frictionRecord
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return frictionRecord{}, err
	}
	return r, nil
}

func frictionAt(cfg Config, scope, id string) (Memory, bool, error) {
	m := Memory{Scope: scope, ID: id}
	path, err := memoryPath(cfg, m)
	if err != nil {
		return m, false, err
	}
	m, err = parseMemory(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if m.Scope != scope || m.ID != id {
		return m, false, errors.New("friction record does not match its project or ID")
	}
	return m, true, nil
}

func validateFrictionParent(cfg Config, m Memory) error {
	if m.Meta[frictionArg] == nil {
		return nil
	}
	r, err := frictionFromMemory(m)
	if err != nil || r.Kind != "recovery" {
		return err
	}
	parent, found, err := frictionAt(cfg, m.Scope, r.ReportID)
	if err != nil {
		return err
	}
	if found {
		record, err := frictionFromMemory(parent)
		if err == nil && record.Kind == "report" {
			return nil
		}
	}
	return errors.New("approve or save the original friction report in this project first")
}

func cmdFriction(ctx context.Context, argv []string, stdout, stderr io.Writer, stdin io.Reader) error {
	const usage = "usage: mora friction log|recover|import [--scope project:name] < record.json\n       mora friction recall [--scope project:name] QUERY"
	if len(argv) == 0 || argv[0] == "--help" || argv[0] == "help" {
		_, err := fmt.Fprintln(stdout, usage)
		return err
	}
	cmd := argv[0]
	flags := flag.NewFlagSet("friction "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	explicit := flags.String("scope", "", "project scope (required outside a bound project)")
	if err := flags.Parse(argv[1:]); err != nil {
		return err
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	scope, _, bound, err := lookupProject(cfg, dir)
	if err != nil {
		return err
	}
	if !bound {
		scope = *explicit
	}
	if !validFrictionScope(scope) || (*explicit != "" && *explicit != scope) {
		return errors.New("friction requires the current bound project or an explicit project from an unbound folder")
	}
	if cmd == "recall" && flags.NArg() > 0 {
		result, err := recallFriction(cfg, scope, strings.Join(flags.Args(), " "))
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(result)
	}
	if flags.NArg() != 0 || (cmd != "log" && cmd != "recover" && cmd != "import") {
		return errors.New(usage)
	}
	policy := configMCPWritePolicy(cfg)
	if policy == mcpWritePolicyReadonly {
		return errors.New("friction mutation refused by read-only write policy")
	}
	defer drainReconcileOrWarn(stderr)
	if cmd == "import" {
		var packet frictionPacket
		if err := decodeFriction(stdin, &packet); err != nil {
			return err
		}
		if packet.Scope != scope {
			return errors.New("Drona packet scope must match the current project")
		}
		result, err := stageFrictionImport(cfg, packet)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(result)
	}
	var record frictionRecord
	if err := decodeFriction(stdin, &record); err != nil {
		return err
	}
	if record.SourceID != "" {
		return errors.New("use reviewed import for Drona records")
	}
	record.Kind = "report"
	if cmd == "recover" {
		record.Kind = "recovery"
	}
	if record.At == "" {
		record.At = time.Now().UTC().Format(time.RFC3339)
	}
	args, err := record.args(scope)
	if err != nil {
		return err
	}
	m, err := record.memory(scope)
	if err != nil {
		return err
	}
	if err := validateFrictionParent(cfg, m); err != nil {
		return err
	}
	var result any
	ctx = withAgentCaller(ctx, "friction-cli")
	switch policy {
	case mcpWritePolicyOpen:
		result, err = mcpWriteMemory(ctx, cfg, args)
	case mcppkg.WritePolicyTentative:
		result, err = mcpWriteTentative(ctx, cfg, args)
	default:
		result, err = stageMCPWriteProposalHeld(cfg, args, record.Author, nil)
	}
	if err != nil {
		return err
	}
	if id := proposalIDOf(result); id != "" {
		result.(map[string]any)["memory_id"] = frictionID(scope, record.Kind, id)
	}
	return json.NewEncoder(stdout).Encode(result)
}

func stageFrictionImport(cfg Config, packet frictionPacket) (any, error) {
	report := packet.Report
	report.Kind = "report"
	parent := frictionID(packet.Scope, report.Kind, report.SourceID)
	if !frictionFeedbackID.MatchString(report.SourceID) || len(packet.Recoveries) > 32 {
		return nil, errors.New("import requires a Drona feedback ID and at most 32 recoveries")
	}
	records := []frictionRecord{report}
	for _, r := range packet.Recoveries {
		if !frictionRecoveryID.MatchString(r.SourceID) {
			return nil, errors.New("import recovery requires its original Drona recovery ID")
		}
		r.Kind, r.ReportID = "recovery", parent
		records = append(records, r)
	}
	var args []map[string]any
	seen := map[string]bool{}
	for _, r := range records {
		if seen[r.SourceID] {
			return nil, errors.New("duplicate source ID in Drona packet")
		}
		seen[r.SourceID] = true
		a, err := r.args(packet.Scope)
		if err != nil {
			return nil, err
		}
		args = append(args, a)
	}
	results := make([]map[string]any, 0, len(records))
	err := leasefile.WithGuard(filepath.Join(cfg.StateDir, "friction-import"), func() error {
		pending, err := listMCPWriteProposals(cfg)
		if err != nil {
			return err
		}
		for i, r := range records {
			id := frictionID(packet.Scope, r.Kind, r.SourceID)
			found, exists, err := frictionAt(cfg, packet.Scope, id)
			if err != nil {
				return err
			}
			result := map[string]any{"source_id": r.SourceID, "memory_id": id}
			if exists {
				existing, err := frictionFromMemory(found)
				raw, _ := json.Marshal(existing)
				if err != nil || string(raw) != args[i][frictionArg] {
					return fmt.Errorf("Drona source %s conflicts with its saved record", r.SourceID)
				}
				result["status"] = "already_saved"
			} else {
				for _, p := range pending {
					if p.Arguments["scope"] != packet.Scope {
						continue
					}
					var staged frictionRecord
					if json.Unmarshal([]byte(strArg(p.Arguments, frictionArg, "")), &staged) != nil || staged.SourceID != r.SourceID {
						continue
					}
					if p.Arguments[frictionArg] != args[i][frictionArg] {
						return fmt.Errorf("Drona source %s conflicts with its pending proposal", r.SourceID)
					}
					result["status"], result["proposal_id"] = "pending", p.ID
					break
				}
			}
			results = append(results, result)
		}
		for i, result := range results {
			if result["status"] != nil {
				continue
			}
			staged, err := stageMCPWriteProposalHeld(cfg, args[i], records[i].Author, []string{"review Drona import"})
			if err != nil {
				return fmt.Errorf("stage Drona source %s; rerun import to reconcile pending records: %w", records[i].SourceID, err)
			}
			result["status"], result["proposal_id"] = "pending", proposalIDOf(staged)
		}
		return nil
	})
	return map[string]any{"scope": packet.Scope, "records": results}, err
}

type frictionRecallRecovery struct {
	ID string `json:"id"`
	frictionRecord
}

type frictionRecallRow struct {
	ID         string                   `json:"id"`
	Report     frictionRecord           `json:"report"`
	Recoveries []frictionRecallRecovery `json:"recoveries"`
	DefectOpen bool                     `json:"defect_open"`
}

func recallFriction(cfg Config, scope, query string) (any, error) {
	stub, err := memoryPath(cfg, Memory{Scope: scope, ID: "mem_friction"})
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Dir(stub))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	type scored struct {
		row   frictionRecallRow
		score int
	}
	var reports []scored
	recoveries := map[string][]frictionRecallRecovery{}
	terms := frictionTerms(query)
	if len(terms) == 0 {
		return nil, errors.New("provide a symptom or report ID")
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), "mem_friction_") || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".md")
		m, found, err := frictionAt(cfg, scope, id)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		r, err := frictionFromMemory(m)
		if err != nil {
			return nil, err
		}
		if r.Kind == "recovery" {
			recoveries[r.ReportID] = append(recoveries[r.ReportID], frictionRecallRecovery{id, r})
			continue
		}
		haystack := frictionTerms(r.Symptom + " " + r.Expected)
		score := 0
		for _, term := range terms {
			if slices.Contains(haystack, term) {
				score++
			}
		}
		if query == id {
			score += len(terms) + 1
		}
		if score > 0 {
			reports = append(reports, scored{frictionRecallRow{ID: id, Report: r, DefectOpen: true}, score})
		}
	}
	sort.SliceStable(reports, func(i, j int) bool { return reports[i].score > reports[j].score })
	rows := make([]frictionRecallRow, 0, min(len(reports), 10))
	for _, item := range reports[:min(len(reports), 10)] {
		row := item.row
		row.Recoveries = append([]frictionRecallRecovery{}, recoveries[row.ID]...)
		sort.SliceStable(row.Recoveries, func(i, j int) bool {
			a, _ := time.Parse(time.RFC3339, row.Recoveries[i].At)
			b, _ := time.Parse(time.RFC3339, row.Recoveries[j].At)
			return a.Before(b)
		})
		for _, recovery := range row.Recoveries {
			if recovery.DefectOpen != nil {
				row.DefectOpen = *recovery.DefectOpen
			}
		}
		rows = append(rows, row)
	}
	return map[string]any{"scope": scope, "results": rows, "health": compactHealthOf(cfg, cfg.OperationClock())}, nil
}

func frictionTerms(s string) []string {
	var terms []string
	for _, term := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		if len(term) > 2 && !slices.Contains(terms, term) {
			terms = append(terms, term)
		}
	}
	return terms
}
