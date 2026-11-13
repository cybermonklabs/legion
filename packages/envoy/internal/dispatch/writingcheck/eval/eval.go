// Package eval holds the writing check's description file, scanner.yaml, and its schema,
// scanner.schema.json: the record the eval method (INFRA-3189) asks of a scanner, with the
// definition, the error analysis, the data sets, the final test and the bar it is held to.
//
// A field whose figure waits for the final test (or for the freeze) carries `pending: true` and
// a reason, and its other fields may be absent. Parse reads a description strictly: a field the
// schema does not name is an error. Check lists what a description lacks.
package eval

import (
	"bytes"
	"cmp"
	_ "embed"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// Schema is scanner.schema.json, the JSON schema of a description file.
//
//go:embed scanner.schema.json
var Schema []byte

// WritingCheck is scanner.yaml, the writing check's description file.
//
//go:embed scanner.yaml
var WritingCheck []byte

// The uses a scanner can be put to.
const (
	UseMonitor = "monitor"
	UseTriage  = "triage"
	UseGate    = "gate"
)

// Pending marks a field whose figures wait for the final test or the freeze, with the reason.
type Pending struct {
	Pending bool   `yaml:"pending,omitempty"`
	Reason  string `yaml:"reason,omitempty"`
}

// Description is a description file.
type Description struct {
	SchemaVersion      int                 `yaml:"schema_version"`
	Name               string              `yaml:"name"`
	Use                *Use                `yaml:"use"`
	Owner              *Owner              `yaml:"owner"`
	Definition         *Definition         `yaml:"definition"`
	ErrorAnalysis      *ErrorAnalysis      `yaml:"error_analysis"`
	EligiblePopulation *EligiblePopulation `yaml:"eligible_population"`
	BootstrapSeed      *BootstrapSeed      `yaml:"bootstrap_seed"`
	ServesTop          *ServesTop          `yaml:"serves_top"`
	BundleDigest       *BundleDigest       `yaml:"bundle_digest"`
	Manifests          *Manifests          `yaml:"manifests"`
	FinalTest          *FinalTest          `yaml:"final_test"`
	Baseline           *Baseline           `yaml:"baseline"`
	Repeatability      *Repeatability      `yaml:"repeatability"`
	ComparabilityBars  *ComparabilityBars  `yaml:"comparability_bars"`
	AuditSampleSize    *AuditSampleSize    `yaml:"audit_sample_size"`
	Ceilings           *Ceilings           `yaml:"ceilings"`
	AcceptanceBar      *AcceptanceBar      `yaml:"acceptance_bar"`
	ReaderModel        *ReaderModel        `yaml:"reader_model"`
	Summarizer         *Summarizer         `yaml:"summarizer"`
	Labellers          *Labellers          `yaml:"labellers"`
	ValidateTask       string              `yaml:"validate_task"`
	ReproduceTask      string              `yaml:"reproduce_task"`
	KPI                *KPI                `yaml:"kpi"`
	Schedule           *Schedule           `yaml:"schedule"`
	AlertRoute         *AlertRoute         `yaml:"alert_route"`
	RuleCosts          *RuleCosts          `yaml:"rule_costs,omitempty"`
}

// Use is what the scanner is used for (monitor, triage or gate), and how.
type Use struct {
	Purpose string `yaml:"purpose"`
	Note    string `yaml:"note,omitempty"`
}

// Owner is who answers for the scanner.
type Owner struct {
	Pending `yaml:",inline"`
	Name    string `yaml:"name,omitempty"`
}

// Definition is the operational definition: the outcome, then the proxy a scanner reads, the
// opportunity and when it is final, the decision rule, the evidence cutoff, the exclusions and
// the boundary cases.
type Definition struct {
	Version        int            `yaml:"version"`
	Frozen         bool           `yaml:"frozen"`
	Outcome        string         `yaml:"outcome"`
	Proxy          string         `yaml:"proxy"`
	Opportunity    string         `yaml:"opportunity"`
	FinalWhen      string         `yaml:"final_when"`
	DecisionRule   []string       `yaml:"decision_rule"`
	EvidenceCutoff string         `yaml:"evidence_cutoff"`
	Exclusions     []string       `yaml:"exclusions"`
	BoundaryCases  []BoundaryCase `yaml:"boundary_cases"`
}

// BoundaryCase is a case two readers could judge differently, with its ruling and reason.
type BoundaryCase struct {
	Case   string `yaml:"case"`
	Ruling string `yaml:"ruling"`
	Reason string `yaml:"reason"`
}

// ErrorAnalysis is the record of the error analysis: the sample's digest, size and session or
// issue count, the failure kinds seen with their counts, and the reads since the last new kind.
type ErrorAnalysis struct {
	Pending               `yaml:",inline"`
	SampleSHA256          string        `yaml:"sample_sha256,omitempty"`
	File                  string        `yaml:"file,omitempty"`
	Unit                  string        `yaml:"unit,omitempty"`
	Size                  int           `yaml:"size,omitempty"`
	SessionsOrIssues      int           `yaml:"sessions_or_issues,omitempty"`
	CountUnit             string        `yaml:"count_unit,omitempty"`
	FailureKinds          []FailureKind `yaml:"failure_kinds,omitempty"`
	ReadsSinceLastNewKind int           `yaml:"reads_since_last_new_kind,omitempty"`
}

// FailureKind is one failure kind the error analysis saw, with its count.
type FailureKind struct {
	Code  string `yaml:"code"`
	Kind  string `yaml:"kind"`
	Count int    `yaml:"count"`
}

// EligiblePopulation is the finite population the final test is drawn from, and the part of it
// a reference source can label.
type EligiblePopulation struct {
	Pending     `yaml:",inline"`
	Description string `yaml:"description,omitempty"`
	Labelable   string `yaml:"labelable,omitempty"`
	PoolSHA256  string `yaml:"pool_sha256,omitempty"`
	PoolWrites  int    `yaml:"pool_writes,omitempty"`
	PoolGroups  int    `yaml:"pool_groups,omitempty"`
}

// BootstrapSeed is the seed of the figures' bootstrap.
type BootstrapSeed struct {
	Pending `yaml:",inline"`
	Seed    *int64 `yaml:"seed,omitempty"`
}

// ServesTop says how the eval serves the top measure.
type ServesTop struct {
	Pending   `yaml:",inline"`
	Statement string `yaml:"statement,omitempty"`
}

// BundleDigest is the frozen bundle's digest and the command that computes it.
type BundleDigest struct {
	Pending `yaml:",inline"`
	SHA256  string `yaml:"sha256,omitempty"`
	Command string `yaml:"command,omitempty"`
}

// Manifests are the three sets.
type Manifests struct {
	Development *Manifest `yaml:"development"`
	Calibration *Manifest `yaml:"calibration"`
	FinalTest   *Manifest `yaml:"final_test"`
}

// Manifest is one set: its file, digest and dataset version, its counts and group key, and the
// source and revision of its labels.
type Manifest struct {
	Pending        `yaml:",inline"`
	File           string `yaml:"file,omitempty"`
	SHA256         string `yaml:"sha256,omitempty"`
	DatasetVersion string `yaml:"dataset_version,omitempty"`
	Cases          int    `yaml:"cases,omitempty"`
	Writes         int    `yaml:"writes,omitempty"`
	Groups         int    `yaml:"groups,omitempty"`
	GroupKey       string `yaml:"group_key,omitempty"`
	LabelSource    string `yaml:"label_source,omitempty"`
	LabelRevision  string `yaml:"label_revision,omitempty"`
}

// FinalTest is the one scorer run on the final test: its id and time, the set it read, its
// outputs' digest, and the scanner's and the baseline's figures.
type FinalTest struct {
	Pending         `yaml:",inline"`
	RunID           string       `yaml:"run_id,omitempty"`
	RunTime         string       `yaml:"run_time,omitempty"`
	SetSHA256       string       `yaml:"set_sha256,omitempty"`
	OutputsSHA256   string       `yaml:"outputs_sha256,omitempty"`
	Seed            *int64       `yaml:"seed,omitempty"`
	Figures         *TestFigures `yaml:"figures,omitempty"`
	BaselineFigures *TestFigures `yaml:"baseline_figures,omitempty"`
}

// TestFigures are the counts a final test's two errors are bounded from.
type TestFigures struct {
	Precision PrecisionCount `yaml:"precision"`
	Recall    RecallCount    `yaml:"recall"`
}

// PrecisionCount is the cases the scanner called failure, and those the reference does not.
type PrecisionCount struct {
	Refused        int `yaml:"refused"`
	FalsePositives int `yaml:"false_positives"`
}

// RecallCount is the reference failures, and those the scanner missed.
type RecallCount struct {
	Failures int `yaml:"failures"`
	Missed   int `yaml:"missed"`
}

// Baseline is the simple baseline scored on the same final test.
type Baseline struct {
	Description string `yaml:"description"`
	Code        string `yaml:"code"`
}

// Repeatability is the model reader's per-class disagreement over reruns, and its bound.
type Repeatability struct {
	Pending      `yaml:",inline"`
	Runs         int                `yaml:"runs,omitempty"`
	Disagreement map[string]float64 `yaml:"disagreement,omitempty"`
	Bound        float64            `yaml:"bound,omitempty"`
}

// ComparabilityBars are the largest insufficient-evidence and not-applicable shares, and the
// lowest extraction and execution coverage, at which a figure still counts.
type ComparabilityBars struct {
	Pending                 `yaml:",inline"`
	InsufficientEvidenceMax float64  `yaml:"insufficient_evidence_max,omitempty"`
	NotApplicableMax        float64  `yaml:"not_applicable_max,omitempty"`
	ExtractionCoverageMin   *float64 `yaml:"extraction_coverage_min,omitempty"`
	ExecutionCoverageMin    float64  `yaml:"execution_coverage_min,omitempty"`
	Note                    string   `yaml:"note,omitempty"`
}

// AuditSampleSize is the size of each live audit sample.
type AuditSampleSize struct {
	Pending `yaml:",inline"`
	Size    int `yaml:"size,omitempty"`
}

// Ceilings are the latency and tokens per scan above which a live run stops.
type Ceilings struct {
	Pending        `yaml:",inline"`
	LatencySeconds float64 `yaml:"latency_seconds,omitempty"`
	Tokens         int     `yaml:"tokens,omitempty"`
}

// AcceptanceBar is the precision and recall floors the scanner's use needs, at a confidence. A
// floor f bounds its error at e = 1 − f. BarCommittedAt, when set, is when the bar was committed
// (RFC 3339); otherwise the commit time of the bar's line is read from git.
type AcceptanceBar struct {
	Pending        `yaml:",inline"`
	PrecisionFloor *float64 `yaml:"precision_floor,omitempty"`
	RecallFloor    *float64 `yaml:"recall_floor,omitempty"`
	Confidence     float64  `yaml:"confidence,omitempty"`
	Note           string   `yaml:"note,omitempty"`
	BarCommittedAt string   `yaml:"bar_committed_at,omitempty"`
}

// ReaderModel is the model reader: the model by its id and its generation settings.
type ReaderModel struct {
	Pending         `yaml:",inline"`
	ID              string   `yaml:"id,omitempty"`
	Kind            string   `yaml:"kind,omitempty"`
	Deadline        string   `yaml:"deadline,omitempty"`
	MaxOutputTokens int      `yaml:"max_output_tokens,omitempty"`
	Effort          string   `yaml:"effort,omitempty"`
	Temperature     *float64 `yaml:"temperature,omitempty"`
	CandidateCap    int      `yaml:"candidate_cap,omitempty"`
}

// Summarizer says whether a summarizer stands between the evidence and the reader.
type Summarizer struct {
	Used bool   `yaml:"used"`
	Note string `yaml:"note,omitempty"`
}

// Labellers are the model labellers whose labels the figures rest on.
type Labellers struct {
	Pending `yaml:",inline"`
	Entries []Labeller `yaml:"entries,omitempty"`
}

// Labeller is one model labeller: its model, the digests of its prompt and rubric, its
// validation run and its agreement there, and the case types the validation covers. Bound is
// its disagreement bound at Confidence.
type Labeller struct {
	Model         string   `yaml:"model"`
	PromptSHA256  string   `yaml:"prompt_sha256"`
	RubricSHA256  string   `yaml:"rubric_sha256"`
	ValidationRun string   `yaml:"validation_run"`
	Agree         int      `yaml:"agree"`
	Disagree      int      `yaml:"disagree"`
	Bound         float64  `yaml:"bound"`
	Confidence    float64  `yaml:"confidence"`
	CaseTypes     []string `yaml:"case_types"`
}

// KPI is the registered KPI the description file belongs to.
type KPI struct {
	Pending `yaml:",inline"`
	Name    string `yaml:"name,omitempty"`
}

// Schedule is when the scanner runs live.
type Schedule struct {
	Live bool   `yaml:"live"`
	Note string `yaml:"note"`
}

// AlertRoute is where a failed or missed run alerts.
type AlertRoute struct {
	Pending `yaml:",inline"`
	Route   string `yaml:"route,omitempty"`
}

// RuleCosts is the measurement that chose the structure rules: the drawn writes' manifest, its
// seed and size, the ceiling a rule must stay under, and each rule's share.
type RuleCosts struct {
	ManifestSHA256 string     `yaml:"manifest_sha256"`
	Seed           int64      `yaml:"seed"`
	Writes         int        `yaml:"writes"`
	Ceiling        float64    `yaml:"ceiling"`
	Rules          []RuleCost `yaml:"rules"`
}

// RuleCost is one rule's share of the drawn writes it refuses, and whether the bundle enables it.
type RuleCost struct {
	ID      string  `yaml:"id"`
	Share   float64 `yaml:"share"`
	Enabled bool    `yaml:"enabled"`
}

// Parse reads a description file. A field the schema does not name is an error.
func Parse(data []byte) (*Description, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var d Description
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Done is whether a field is present and not pending.
func Done(p *Pending) bool { return p != nil && !p.Pending }

// Check lists what the description lacks: a required field that is absent, a pending field with
// no reason, and a field that is not pending but lacks the figures it must hold.
func (d *Description) Check() []string {
	var problems []string
	need := func(ok bool, format string, args ...any) {
		if !ok {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	pending := func(name string, p *Pending, present bool, done func()) {
		if !present {
			problems = append(problems, name+": missing")
			return
		}
		if p.Pending {
			need(p.Reason != "", "%s: pending with no reason", name)
			return
		}
		if done != nil {
			done()
		}
	}
	need(d.SchemaVersion == 1, "schema_version: must be 1")
	need(d.Name != "", "name: missing")
	need(d.Use != nil && slices.Contains([]string{UseMonitor, UseTriage, UseGate}, d.Use.Purpose), "use.purpose: must be monitor, triage or gate")
	need(d.Definition != nil, "definition: missing")
	if d.Definition != nil {
		def := d.Definition
		need(def.Outcome != "" && def.Proxy != "" && def.Opportunity != "" && def.FinalWhen != "" && def.EvidenceCutoff != "",
			"definition: needs outcome, proxy, opportunity, final_when and evidence_cutoff")
		need(len(def.DecisionRule) > 0, "definition.decision_rule: missing")
		need(len(def.BoundaryCases) >= 5, "definition.boundary_cases: at least five are needed, %d given", len(def.BoundaryCases))
	}
	if d.Owner != nil {
		pending("owner", &d.Owner.Pending, true, func() { need(d.Owner.Name != "", "owner.name: missing") })
	} else {
		pending("owner", nil, false, nil)
	}
	if d.ErrorAnalysis != nil {
		ea := d.ErrorAnalysis
		pending("error_analysis", &ea.Pending, true, func() {
			need(ea.SampleSHA256 != "" && ea.Size > 0 && ea.SessionsOrIssues > 0 && len(ea.FailureKinds) > 0,
				"error_analysis: needs sample_sha256, size, sessions_or_issues and failure_kinds")
			need(ea.CountUnit == "sessions" || ea.CountUnit == "issues", "error_analysis.count_unit: must be sessions or issues")
		})
	} else {
		pending("error_analysis", nil, false, nil)
	}
	type part struct {
		name    string
		p       *Pending
		present bool
		done    func()
	}
	parts := []part{}
	if d.EligiblePopulation != nil {
		e := d.EligiblePopulation
		parts = append(parts, part{"eligible_population", &e.Pending, true, func() {
			need(e.Description != "" && e.Labelable != "", "eligible_population: needs description and labelable")
		}})
	} else {
		parts = append(parts, part{"eligible_population", nil, false, nil})
	}
	if d.BootstrapSeed != nil {
		parts = append(parts, part{"bootstrap_seed", &d.BootstrapSeed.Pending, true, func() {
			need(d.BootstrapSeed.Seed != nil, "bootstrap_seed.seed: missing")
		}})
	} else {
		parts = append(parts, part{"bootstrap_seed", nil, false, nil})
	}
	if d.ServesTop != nil {
		parts = append(parts, part{"serves_top", &d.ServesTop.Pending, true, func() {
			need(d.ServesTop.Statement != "", "serves_top.statement: missing")
		}})
	} else {
		parts = append(parts, part{"serves_top", nil, false, nil})
	}
	if d.BundleDigest != nil {
		parts = append(parts, part{"bundle_digest", &d.BundleDigest.Pending, true, func() {
			need(len(d.BundleDigest.SHA256) == 64, "bundle_digest.sha256: must be a SHA-256")
		}})
	} else {
		parts = append(parts, part{"bundle_digest", nil, false, nil})
	}
	if d.Manifests == nil {
		problems = append(problems, "manifests: missing")
	} else {
		for _, m := range []struct {
			name string
			m    *Manifest
		}{{"manifests.development", d.Manifests.Development}, {"manifests.calibration", d.Manifests.Calibration}, {"manifests.final_test", d.Manifests.FinalTest}} {
			if m.m == nil {
				parts = append(parts, part{m.name, nil, false, nil})
				continue
			}
			mm, name := m.m, m.name
			parts = append(parts, part{name, &mm.Pending, true, func() {
				need(mm.File != "" && len(mm.SHA256) == 64 && mm.LabelSource != "", "%s: needs file, sha256 and label_source", name)
			}})
		}
	}
	if d.FinalTest != nil {
		ft := d.FinalTest
		parts = append(parts, part{"final_test", &ft.Pending, true, func() {
			need(ft.RunID != "" && ft.SetSHA256 != "" && ft.Figures != nil, "final_test: needs run_id, set_sha256 and figures")
		}})
	} else {
		parts = append(parts, part{"final_test", nil, false, nil})
	}
	if d.Baseline == nil || d.Baseline.Description == "" {
		problems = append(problems, "baseline: missing: a simple baseline is scored on the same final test")
	}
	if d.Repeatability != nil {
		parts = append(parts, part{"repeatability", &d.Repeatability.Pending, true, func() {
			need(d.Repeatability.Runs > 1 && len(d.Repeatability.Disagreement) > 0, "repeatability: needs runs and disagreement")
		}})
	} else {
		parts = append(parts, part{"repeatability", nil, false, nil})
	}
	if d.ComparabilityBars != nil {
		c := d.ComparabilityBars
		parts = append(parts, part{"comparability_bars", &c.Pending, true, func() {
			need(c.InsufficientEvidenceMax > 0 && c.NotApplicableMax > 0 && c.ExecutionCoverageMin > 0,
				"comparability_bars: needs insufficient_evidence_max, not_applicable_max and execution_coverage_min")
		}})
	} else {
		parts = append(parts, part{"comparability_bars", nil, false, nil})
	}
	if d.AuditSampleSize != nil {
		parts = append(parts, part{"audit_sample_size", &d.AuditSampleSize.Pending, true, func() {
			need(d.AuditSampleSize.Size > 0, "audit_sample_size.size: missing")
		}})
	} else {
		parts = append(parts, part{"audit_sample_size", nil, false, nil})
	}
	if d.Ceilings != nil {
		parts = append(parts, part{"ceilings", &d.Ceilings.Pending, true, func() {
			need(d.Ceilings.LatencySeconds > 0 && d.Ceilings.Tokens > 0, "ceilings: needs latency_seconds and tokens")
		}})
	} else {
		parts = append(parts, part{"ceilings", nil, false, nil})
	}
	if d.AcceptanceBar != nil {
		b := d.AcceptanceBar
		parts = append(parts, part{"acceptance_bar", &b.Pending, true, func() {
			need(b.PrecisionFloor != nil && b.RecallFloor != nil && b.Confidence > 0 && b.Confidence < 1,
				"acceptance_bar: needs precision_floor, recall_floor and a confidence between 0 and 1")
		}})
	} else {
		parts = append(parts, part{"acceptance_bar", nil, false, nil})
	}
	if d.ReaderModel != nil {
		parts = append(parts, part{"reader_model", &d.ReaderModel.Pending, true, func() {
			need(d.ReaderModel.ID != "", "reader_model.id: missing")
		}})
	} else {
		parts = append(parts, part{"reader_model", nil, false, nil})
	}
	need(d.Summarizer != nil, "summarizer: missing")
	if d.Labellers != nil {
		parts = append(parts, part{"labellers", &d.Labellers.Pending, true, func() {
			need(len(d.Labellers.Entries) > 0, "labellers.entries: missing")
		}})
	} else {
		parts = append(parts, part{"labellers", nil, false, nil})
	}
	need(d.ValidateTask != "", "validate_task: missing")
	need(d.ReproduceTask != "", "reproduce_task: missing")
	if d.KPI != nil {
		parts = append(parts, part{"kpi", &d.KPI.Pending, true, func() { need(d.KPI.Name != "", "kpi.name: missing") }})
	} else {
		parts = append(parts, part{"kpi", nil, false, nil})
	}
	need(d.Schedule != nil && d.Schedule.Note != "", "schedule: missing")
	if d.AlertRoute != nil {
		parts = append(parts, part{"alert_route", &d.AlertRoute.Pending, true, func() {
			need(d.AlertRoute.Route != "", "alert_route.route: missing")
		}})
	} else {
		parts = append(parts, part{"alert_route", nil, false, nil})
	}
	for _, p := range parts {
		pending(p.name, p.p, p.present, p.done)
	}
	return problems
}

// PendingFields is the name and reason of every field that waits.
func (d *Description) PendingFields() [][2]string {
	var out [][2]string
	add := func(name string, p *Pending) {
		if p != nil && p.Pending {
			out = append(out, [2]string{name, p.Reason})
		}
	}
	if d.Owner != nil {
		add("owner", &d.Owner.Pending)
	}
	if d.ErrorAnalysis != nil {
		add("error_analysis", &d.ErrorAnalysis.Pending)
	}
	if d.EligiblePopulation != nil {
		add("eligible_population", &d.EligiblePopulation.Pending)
	}
	if d.BootstrapSeed != nil {
		add("bootstrap_seed", &d.BootstrapSeed.Pending)
	}
	if d.ServesTop != nil {
		add("serves_top", &d.ServesTop.Pending)
	}
	if d.BundleDigest != nil {
		add("bundle_digest", &d.BundleDigest.Pending)
	}
	if d.Manifests != nil {
		for name, m := range map[string]*Manifest{"manifests.development": d.Manifests.Development, "manifests.calibration": d.Manifests.Calibration, "manifests.final_test": d.Manifests.FinalTest} {
			if m != nil {
				add(name, &m.Pending)
			}
		}
	}
	if d.FinalTest != nil {
		add("final_test", &d.FinalTest.Pending)
	}
	if d.Repeatability != nil {
		add("repeatability", &d.Repeatability.Pending)
	}
	if d.ComparabilityBars != nil {
		add("comparability_bars", &d.ComparabilityBars.Pending)
	}
	if d.AuditSampleSize != nil {
		add("audit_sample_size", &d.AuditSampleSize.Pending)
	}
	if d.Ceilings != nil {
		add("ceilings", &d.Ceilings.Pending)
	}
	if d.AcceptanceBar != nil {
		add("acceptance_bar", &d.AcceptanceBar.Pending)
	}
	if d.ReaderModel != nil {
		add("reader_model", &d.ReaderModel.Pending)
	}
	if d.Labellers != nil {
		add("labellers", &d.Labellers.Pending)
	}
	if d.KPI != nil {
		add("kpi", &d.KPI.Pending)
	}
	if d.AlertRoute != nil {
		add("alert_route", &d.AlertRoute.Pending)
	}
	slices.SortFunc(out, func(a, b [2]string) int { return cmp.Compare(a[0], b[0]) })
	return out
}
