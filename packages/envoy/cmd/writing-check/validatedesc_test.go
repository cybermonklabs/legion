package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/eval"
)

// setRow is one case of a fixture manifest, with the evidence and cutoff events a case may
// name.
type setRow struct {
	ID       string `json:"id"`
	EventID  int64  `json:"event_id"`
	Group    string `json:"group"`
	Evidence *int64 `json:"evidence_event_id,omitempty"`
	Cutoff   *int64 `json:"cutoff_event_id,omitempty"`
}

// validFixture is a description file that passes every rule, with its manifests and exposure log
// in dir: the repository's scanner.yaml with every pending field filled with made-up figures.
type validFixture struct {
	dir, desc, log string
	d              *eval.Description
	runs           []exposure
	rows           map[string][]setRow
}

const (
	fixtureBundle = "1111111111111111111111111111111111111111111111111111111111111111"
	fixtureRunAt  = "2026-11-20T12:00:00Z"
)

func newValidFixture(t *testing.T) *validFixture {
	t.Helper()
	d, err := eval.Parse(eval.WritingCheck)
	if err != nil {
		t.Fatal(err)
	}
	f := &validFixture{dir: t.TempDir(), d: d, rows: map[string][]setRow{
		"development": {{ID: "1/a/body", EventID: 1, Group: "g1"}, {ID: "2/b/body", EventID: 2, Group: "g2"}},
		"calibration": {{ID: "3/c/body", EventID: 3, Group: "g3"}},
		"final_test":  {{ID: "4/d/body", EventID: 4, Group: "g4"}},
	}}
	f.desc, f.log = filepath.Join(f.dir, "scanner.yaml"), filepath.Join(f.dir, "exposure.jsonl")
	seed := int64(5)
	floorP, floorR := 0.95, 0.90
	d.Owner = &eval.Owner{Name: "the check's owner"}
	d.EligiblePopulation = &eval.EligiblePopulation{Description: "agent writes after the cut", Labelable: "writes with prose"}
	d.BootstrapSeed = &eval.BootstrapSeed{Seed: &seed}
	d.ServesTop = &eval.ServesTop{Statement: "fewer questions a person must ask"}
	d.BundleDigest = &eval.BundleDigest{SHA256: fixtureBundle, Command: "writing-check digest"}
	d.FinalTest = &eval.FinalTest{RunID: "run-1", RunTime: fixtureRunAt, Seed: &seed,
		Figures:         &eval.TestFigures{Precision: eval.PrecisionCount{Refused: 200}, Recall: eval.RecallCount{Failures: 100}},
		BaselineFigures: &eval.TestFigures{Precision: eval.PrecisionCount{Refused: 50, FalsePositives: 20}, Recall: eval.RecallCount{Failures: 100, Missed: 60}}}
	d.Repeatability = &eval.Repeatability{Runs: 3, Disagreement: map[string]float64{"vacuous": 0.01}, Bound: 0.03}
	d.AuditSampleSize = &eval.AuditSampleSize{Size: 60}
	d.Ceilings = &eval.Ceilings{LatencySeconds: 3, Tokens: 4000}
	d.AcceptanceBar = &eval.AcceptanceBar{PrecisionFloor: &floorP, RecallFloor: &floorR, Confidence: 0.95, BarCommittedAt: "2026-11-19T12:00:00Z"}
	d.ReaderModel = &eval.ReaderModel{ID: "model-2026", Kind: "openai-chat"}
	d.Labellers = &eval.Labellers{Entries: []eval.Labeller{{Model: "labeller", PromptSHA256: fixtureBundle, RubricSHA256: fixtureBundle,
		ValidationRun: "v1", Agree: 183, Bound: 0.02, Confidence: 0.975, CaseTypes: []string{"comment"}}}}
	d.KPI = &eval.KPI{Name: "vacuous verb phrase share"}
	d.AlertRoute = &eval.AlertRoute{Route: "the owner's queue"}
	d.Manifests.Development.File, d.Manifests.Development.SHA256 = "development.jsonl", ""
	d.Manifests.Calibration.File, d.Manifests.Calibration.SHA256 = "calibration.jsonl", ""
	d.Manifests.FinalTest = &eval.Manifest{File: "final_test.jsonl", LabelSource: "labeller"}
	f.runs = []exposure{{RunID: "run-1", Kind: exposureFinalTest, Time: fixtureRunAt, BundleDigest: fixtureBundle, Seed: seed, ModelID: "model-2026"}}
	return f
}

// write writes the manifests, the description (each manifest's digest filled where empty, the
// final test's set digest and its runs' set digest likewise) and the exposure log.
func (f *validFixture) write(t *testing.T) {
	t.Helper()
	for name, m := range map[string]*eval.Manifest{"development": f.d.Manifests.Development, "calibration": f.d.Manifests.Calibration, "final_test": f.d.Manifests.FinalTest} {
		sum, err := writeJSONL(filepath.Join(f.dir, name+".jsonl"), f.rows[name])
		if err != nil {
			t.Fatal(err)
		}
		if m.SHA256 == "" {
			m.SHA256 = sum
		}
	}
	if !f.d.FinalTest.Pending.Pending {
		f.d.FinalTest.SetSHA256 = f.d.Manifests.FinalTest.SHA256
	}
	for i := range f.runs {
		if f.runs[i].SetDigest == "" {
			f.runs[i].SetDigest = f.d.Manifests.FinalTest.SHA256
		}
	}
	data, err := yaml.Marshal(f.d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.desc, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeJSONL(f.log, f.runs); err != nil {
		t.Fatal(err)
	}
}

func (f *validFixture) validate(t *testing.T) (int, string) {
	t.Helper()
	f.write(t)
	code, stdout, stderr := runCmd(t, "validate", "--description", f.desc, "--exposure-log", f.log)
	return code, stdout + stderr
}

func TestValidatePassesADescriptionThatMeetsEveryRule(t *testing.T) {
	f := newValidFixture(t)
	if code, out := f.validate(t); code != 0 {
		t.Fatalf("a valid description: exit %d\n%s", code, out)
	}
}

// TestValidateRefusesAGroupInTwoSetsAndNoBaseline is the negative control: a copy of a valid
// description with one group in two sets and the baseline taken out is refused, for both.
func TestValidateRefusesAGroupInTwoSetsAndNoBaseline(t *testing.T) {
	f := newValidFixture(t)
	f.rows["calibration"] = append(f.rows["calibration"], setRow{ID: "5/e/body", EventID: 5, Group: "g1"})
	f.d.Baseline = nil
	code, out := f.validate(t)
	if code != 1 {
		t.Fatalf("the negative control passed:\n%s", out)
	}
	for _, want := range []string{"group g1 is in two sets: development and calibration", "baseline: missing"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
}

func TestValidateRefusesEachBrokenRule(t *testing.T) {
	for _, c := range []struct {
		name    string
		breakIt func(f *validFixture)
		want    string
	}{
		{"a small error analysis", func(f *validFixture) { f.d.ErrorAnalysis.Size, f.d.ErrorAnalysis.SessionsOrIssues = 40, 12 }, "under 50"},
		{"no error analysis", func(f *validFixture) {
			f.d.ErrorAnalysis = &eval.ErrorAnalysis{Pending: eval.Pending{Pending: true, Reason: "later"}}
		}, "needs an error-analysis record"},
		{"a digest that does not match", func(f *validFixture) { f.d.Manifests.Development.SHA256 = strings.Repeat("0", 64) }, "not the digest"},
		{"evidence after the cutoff", func(f *validFixture) {
			evidence, cutoff := int64(9), int64(3)
			f.rows["calibration"][0].Evidence, f.rows["calibration"][0].Cutoff = &evidence, &cutoff
		}, "comes after the case's cutoff"},
		{"two final-test runs", func(f *validFixture) {
			f.runs = append(f.runs, exposure{RunID: "run-2", Kind: exposureFinalTest, Time: fixtureRunAt, BundleDigest: fixtureBundle})
		}, "2 final-test runs"},
		{"a run naming another bundle", func(f *validFixture) { f.runs[0].BundleDigest = strings.Repeat("2", 64) }, "names bundle digest"},
		{"no final-test run", func(f *validFixture) { f.runs = nil }, "no final-test run of set"},
		{"a run before the claim", func(f *validFixture) {
			f.d.FinalTest = &eval.FinalTest{Pending: eval.Pending{Pending: true, Reason: "later"}}
		}, "before the description claims it"},
		{"a bar committed after the run", func(f *validFixture) { f.d.AcceptanceBar.BarCommittedAt = "2026-11-21T00:00:00Z" }, "after the final-test run"},
		{"an upper bound above e", func(f *validFixture) { f.d.FinalTest.Figures.Precision.FalsePositives = 8 }, "the exact upper bound on 1 - precision"},
		{"too few draws", func(f *validFixture) { f.d.FinalTest.Figures.Recall.Failures = 20 }, "under the 29"},
		{"a composed bound above e", func(f *validFixture) { f.d.Labellers.Entries[0].Bound = 0.04 }, "the composed bound on 1 - precision"},
		{"no baseline figures", func(f *validFixture) { f.d.FinalTest.BaselineFigures = nil }, "no baseline figures"},
		{"a pending field with no reason", func(f *validFixture) { f.d.KPI = &eval.KPI{Pending: eval.Pending{Pending: true}} }, "kpi: pending with no reason"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newValidFixture(t)
			c.breakIt(f)
			code, out := f.validate(t)
			if code != 1 || !strings.Contains(out, c.want) {
				t.Errorf("exit %d, want %q in\n%s", code, c.want, out)
			}
		})
	}
}

func TestValidateReadsTheBarsCommitTimeFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "scanner.yaml")
	if err := os.WriteFile(path, []byte("name: x\nacceptance_bar:\n  confidence: 0.95\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := gitLineTime(path, "acceptance_bar:"); ok {
		t.Error("a file outside git has a commit time")
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE=2026-11-19T12:00:00Z", "GIT_AUTHOR_DATE=2026-11-19T12:00:00Z")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", "scanner.yaml")
	git("commit", "-q", "-m", "bar")
	at, ok := gitLineTime(path, "acceptance_bar:")
	if want, _ := time.Parse(time.RFC3339, "2026-11-19T12:00:00Z"); !ok || !at.Equal(want) {
		t.Errorf("commit time %v, %t; want %v", at, ok, want)
	}
	if err := os.WriteFile(path, []byte("name: x\nacceptance_bar: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := gitLineTime(path, "acceptance_bar:"); ok {
		t.Error("an uncommitted bar line has a commit time")
	}
}
