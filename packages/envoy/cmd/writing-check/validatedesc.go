package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/eval"
)

// The least error-analysis sample the method accepts: 50 opportunities from 20 sessions or issues.
const (
	minErrorAnalysisSize   = 50
	minErrorAnalysisGroups = 20
)

func runValidate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("writing-check validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	descPath := flags.String("description", "", "the description file, scanner.yaml (required)")
	logPath := flags.String("exposure-log", "", "the exposure log score --final-test appends to (required; a log that does not exist holds no run)")
	manifestsDir := flags.String("manifests", "", "the directory the manifests' files are in (default: the description file's directory)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *descPath == "" || *logPath == "" || flags.NArg() > 0 {
		fmt.Fprintln(stderr, "writing-check validate: --description and --exposure-log are required, and nothing else")
		return 2
	}
	dir := *manifestsDir
	if dir == "" {
		dir = filepath.Dir(*descPath)
	}
	data, err := os.ReadFile(*descPath)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check validate: %v\n", err)
		return 1
	}
	d, err := eval.Parse(data)
	if err != nil {
		fmt.Fprintf(stdout, "validate: FAIL\n- the description file does not parse against the schema: %v\n", err)
		return 1
	}
	logged, err := readExposures(*logPath)
	if err != nil {
		fmt.Fprintf(stderr, "writing-check validate: %v\n", err)
		return 1
	}
	failures := validateDescription(d, *descPath, dir, logged, gitLineTime)
	pending := d.PendingFields()
	if len(failures) > 0 {
		fmt.Fprintf(stdout, "validate: FAIL (%d)\n", len(failures))
		for _, f := range failures {
			fmt.Fprintf(stdout, "- %s\n", f)
		}
	} else {
		fmt.Fprintln(stdout, "validate: PASS")
	}
	if len(pending) > 0 {
		fmt.Fprintf(stdout, "pending (%d):\n", len(pending))
		for _, p := range pending {
			fmt.Fprintf(stdout, "- %s: %s\n", p[0], p[1])
		}
	}
	if len(failures) > 0 {
		return 1
	}
	return 0
}

// lineTime answers when the line of a file that starts with prefix was committed, and false when
// it is not committed or cannot be read.
type lineTime func(path, prefix string) (time.Time, bool)

// validateDescription answers every rule of the method the description breaks, for a gate:
//
//   - a field the description lacks (eval.Check), a missing baseline among them;
//   - for triage or a gate, no error-analysis record, or a sample under 50 opportunities or
//     under 20 sessions or issues;
//   - a manifest whose file does not match its digest, a group in two sets, a case whose evidence
//     comes after its cutoff (evidence_event_id above cutoff_event_id, each the case's event_id
//     where absent);
//   - before the claim, any final-test run of the final-test set in the exposure log; after it,
//     no such run, more than one, one naming another bundle digest or another run id;
//   - an acceptance bar committed after the final-test run;
//   - for a gate, a class below its required draws, an exact upper bound on either error above
//     its e (e = 1 − floor), the composed bound (the labeller's bound plus the scanner's, each at
//     1 − α/2) above e, and a final test with no baseline figures.
func validateDescription(d *eval.Description, descPath, manifestsDir string, logged []exposure, committed lineTime) []string {
	failures := d.Check()
	fail := func(format string, args ...any) { failures = append(failures, fmt.Sprintf(format, args...)) }
	use := ""
	if d.Use != nil {
		use = d.Use.Purpose
	}
	gated := use == eval.UseGate || use == eval.UseTriage
	if gated {
		switch ea := d.ErrorAnalysis; {
		case !eval.Done(pendingOf(ea)):
			fail("error_analysis: a %s needs an error-analysis record", use)
		default:
			if ea.Size < minErrorAnalysisSize {
				fail("error_analysis: the sample holds %d opportunities, under %d", ea.Size, minErrorAnalysisSize)
			}
			if ea.SessionsOrIssues < minErrorAnalysisGroups {
				fail("error_analysis: the sample comes from %d %s, under %d", ea.SessionsOrIssues, ea.CountUnit, minErrorAnalysisGroups)
			}
		}
	}

	finalSet := ""
	if d.Manifests != nil {
		groupSet := map[string]string{}
		for _, m := range []struct {
			name string
			m    *eval.Manifest
		}{{"development", d.Manifests.Development}, {"calibration", d.Manifests.Calibration}, {"final_test", d.Manifests.FinalTest}} {
			if m.m == nil || m.m.Pending.Pending || m.m.File == "" {
				continue
			}
			if m.name == "final_test" {
				finalSet = m.m.SHA256
			}
			path := filepath.Join(manifestsDir, m.m.File)
			data, err := os.ReadFile(path)
			if err != nil {
				fail("manifests.%s: cannot read %s: %v", m.name, path, err)
				continue
			}
			if sum := sha256Hex(data); sum != m.m.SHA256 {
				fail("manifests.%s: %s has sha256 %s, not the digest %s the description names", m.name, m.m.File, sum, m.m.SHA256)
			}
			for _, problem := range checkManifestRows(m.name, data, groupSet) {
				fail("%s", problem)
			}
		}
	}

	ft := d.FinalTest
	claimed := eval.Done(pendingOf(ft))
	if claimed && ft.SetSHA256 != "" {
		if finalSet != "" && finalSet != ft.SetSHA256 {
			fail("final_test: set_sha256 %s is not the final-test manifest's digest %s", ft.SetSHA256, finalSet)
		}
		finalSet = ft.SetSHA256
	}
	var runs []exposure
	for _, e := range logged {
		if e.Kind == exposureFinalTest && finalSet != "" && e.SetDigest == finalSet {
			runs = append(runs, e)
		}
	}
	bundle := ""
	if eval.Done(pendingOf(d.BundleDigest)) {
		bundle = d.BundleDigest.SHA256
	}
	switch {
	case !claimed:
		for _, r := range runs {
			fail("exposure log: final-test run %s read the final test (%s) before the description claims it", r.RunID, finalSet)
		}
	case len(runs) == 0:
		fail("exposure log: no final-test run of set %s, which final_test claims", finalSet)
	case len(runs) > 1:
		ids := make([]string, len(runs))
		for i, r := range runs {
			ids[i] = r.RunID
		}
		fail("exposure log: %d final-test runs of set %s (%s): a final test is read once", len(runs), finalSet, strings.Join(ids, ", "))
	}
	if claimed {
		for _, r := range runs {
			if r.BundleDigest != bundle {
				fail("exposure log: final-test run %s names bundle digest %q, not the description's %q", r.RunID, r.BundleDigest, bundle)
			}
			if r.RunID != ft.RunID {
				fail("exposure log: final-test run %s is not the run final_test names (%s)", r.RunID, ft.RunID)
			}
		}
		if ft.BaselineFigures == nil {
			fail("final_test: no baseline figures: the baseline is scored on the same final test")
		}
		checkBar(d, descPath, runs, committed, fail)
	}
	if use == eval.UseGate && claimed && ft.Figures != nil {
		checkBounds(d, fail)
	}
	return failures
}

// pendingOf is the Pending of a pending-able field, nil for an absent one.
func pendingOf[T any](v *T) *eval.Pending {
	if v == nil {
		return nil
	}
	switch f := any(v).(type) {
	case *eval.ErrorAnalysis:
		return &f.Pending
	case *eval.FinalTest:
		return &f.Pending
	case *eval.BundleDigest:
		return &f.Pending
	case *eval.AcceptanceBar:
		return &f.Pending
	case *eval.Labellers:
		return &f.Pending
	}
	return nil
}

// checkManifestRows checks one set's rows: each names a group that no other set holds, and no
// case's evidence comes after its cutoff. groupSet maps each group seen to its set.
func checkManifestRows(set string, data []byte, groupSet map[string]string) []string {
	var problems []string
	crossed := map[string]bool{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 1<<20), 256<<20)
	for n := 1; scanner.Scan(); n++ {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var row struct {
			ID       string `json:"id"`
			EventID  int64  `json:"event_id"`
			Group    string `json:"group"`
			Evidence *int64 `json:"evidence_event_id"`
			Cutoff   *int64 `json:"cutoff_event_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			problems = append(problems, fmt.Sprintf("manifests.%s line %d: %v", set, n, err))
			continue
		}
		if row.Group == "" {
			problems = append(problems, fmt.Sprintf("manifests.%s line %d: no group", set, n))
		} else if other, ok := groupSet[row.Group]; ok && other != set && !crossed[row.Group] {
			crossed[row.Group] = true
			problems = append(problems, fmt.Sprintf("group %s is in two sets: %s and %s", row.Group, other, set))
		} else if !ok {
			groupSet[row.Group] = set
		}
		evidence, cutoff := row.EventID, row.EventID
		if row.Evidence != nil {
			evidence = *row.Evidence
		}
		if row.Cutoff != nil {
			cutoff = *row.Cutoff
		}
		if evidence > cutoff {
			problems = append(problems, fmt.Sprintf("manifests.%s line %d (%s): evidence at event %d comes after the case's cutoff at event %d", set, n, row.ID, evidence, cutoff))
		}
	}
	if err := scanner.Err(); err != nil {
		problems = append(problems, fmt.Sprintf("manifests.%s: %v", set, err))
	}
	return problems
}

// checkBar fails a bar committed after the final-test run: its bar_committed_at, or else the
// commit time of the acceptance_bar line, against the run's time in the exposure log (or
// final_test.run_time).
func checkBar(d *eval.Description, descPath string, runs []exposure, committed lineTime, fail func(string, ...any)) {
	runAt := d.FinalTest.RunTime
	if len(runs) > 0 {
		runAt = runs[0].Time
	}
	ran, err := time.Parse(time.RFC3339, runAt)
	if err != nil {
		fail("final_test: the run's time %q is not an RFC 3339 time", runAt)
		return
	}
	bar := d.AcceptanceBar
	if bar == nil {
		return
	}
	var at time.Time
	if bar.BarCommittedAt != "" {
		if at, err = time.Parse(time.RFC3339, bar.BarCommittedAt); err != nil {
			fail("acceptance_bar.bar_committed_at: %q is not an RFC 3339 time", bar.BarCommittedAt)
			return
		}
	} else {
		var ok bool
		if at, ok = committed(descPath, "acceptance_bar:"); !ok {
			fail("acceptance_bar: not committed, so it cannot predate the final-test run")
			return
		}
	}
	if at.After(ran) {
		fail("acceptance_bar: committed at %s, after the final-test run at %s", at.UTC().Format(time.RFC3339), ran.UTC().Format(time.RFC3339))
	}
}

// checkBounds fails a gate whose final test does not certify its bar: for each error, a class
// below the draws that bound it at e with no error, an exact upper bound above e at the bar's
// confidence, and, with a labeller, the composed bound above e.
func checkBounds(d *eval.Description, fail func(string, ...any)) {
	bar := d.AcceptanceBar
	if bar == nil || bar.Confidence <= 0 || bar.Confidence >= 1 {
		return
	}
	conf := bar.Confidence
	half := 1 - (1-conf)/2
	labeller := 0.0
	hasLabeller := false
	if eval.Done(pendingOf(d.Labellers)) {
		for _, l := range d.Labellers.Entries {
			hasLabeller = true
			labeller = max(labeller, l.Bound)
		}
	}
	f := d.FinalTest.Figures
	for _, c := range []struct {
		name   string
		floor  *float64
		errors int
		n      int
	}{
		{"1 - precision", bar.PrecisionFloor, f.Precision.FalsePositives, f.Precision.Refused},
		{"1 - recall", bar.RecallFloor, f.Recall.Missed, f.Recall.Failures},
	} {
		if c.floor == nil {
			continue
		}
		e := 1 - *c.floor
		if need := requiredDraws(e, conf); c.n < need {
			fail("final_test: %s rests on %d draws, under the %d that bound it at %s with %s confidence", c.name, c.n, need, pct(e), pct(conf))
		}
		if ub := upperBound(c.errors, c.n, conf); ub > e {
			fail("final_test: the exact upper bound on %s is %s (%d of %d) at %s, above e = %s", c.name, pct(ub), c.errors, c.n, pct(conf), pct(e))
		}
		if hasLabeller {
			if composed := labeller + upperBound(c.errors, c.n, half); composed > e {
				fail("final_test: the composed bound on %s is %s (labeller %s + scanner %s at %s), above the bar's e = %s",
					c.name, pct(composed), pct(labeller), pct(upperBound(c.errors, c.n, half)), pct(half), pct(e))
			}
		}
	}
}

// gitLineTime is the committer time of the commit that last changed the line of path starting
// with prefix, by git blame; false when git cannot read it or the line is not committed.
func gitLineTime(path, prefix string) (time.Time, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return time.Time{}, false
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return time.Time{}, false
	}
	line := 0
	for i, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, prefix) {
			line = i + 1
			break
		}
	}
	if line == 0 {
		return time.Time{}, false
	}
	out, err := exec.Command("git", "-C", filepath.Dir(abs), "blame", "--porcelain", "-L", fmt.Sprintf("%d,%d", line, line), "--", filepath.Base(abs)).Output()
	if err != nil {
		return time.Time{}, false
	}
	if strings.HasPrefix(string(out), strings.Repeat("0", 40)) {
		return time.Time{}, false
	}
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "committer-time "); ok {
			secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(secs, 0), true
		}
	}
	return time.Time{}, false
}
