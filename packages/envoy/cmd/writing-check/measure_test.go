package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck/prose"
)

// The sentences of the measurement fixture, written for these tests. The code stage keeps
// sentences A, C, D, F, G, H and K as candidates (each a subject, a verb and an object with no
// value), and not B, E, I, J, L or M (each states a value in the verb's clause); the baseline
// matches A, C, E, F and G.
const (
	sentA = "The new section names the risk."               // vacuous, names; candidate; baseline
	sentB = "The tide table lists 14 tides for May."        // a fact; no candidate
	sentC = "The fix shows the gap."                        // vacuous, shows; candidate; baseline
	sentD = "The proposal frames the risk."                 // vacuous, other verb; candidate
	sentE = "The note shows that the cache holds 3 rows."   // a fact; baseline only
	sentF = "The draft names the gap."                      // planted fault, names; candidate; baseline
	sentG = "The branch pays off."                          // vacuous, pays; candidate; baseline
	sentH = "The patch lists the steps."                    // a fact the stub calls vacuous; candidate
	sentI = "The new wording will name two issues."         // vacuous, names; a count, so no candidate
	sentJ = "The rewrite will shape 3 parts of the work."   // vacuous, other verb; a count, so no candidate
	sentK = "The plan carries the weight."                  // planted fault, other verb; candidate
	sentL = "The config file names us-west1 as the region." // planted control; no candidate
	sentM = "The kettle boils in 40 seconds."               // a fact; no candidate
)

// fixtureSentence is one labelled sentence: its text, the labeller's verdict and shape.
type fixtureSentence struct{ text, verdict, shape string }

// fixtureLabels is a labels file of six real writes and three planted copies, and plant's manifest
// for the copies, written into dir. Real writes: w1 (A, B, I) and w2 (C, D, J) hold failures; w3
// (H, E) and w4 (M) pass; w5 holds no sentence; w6 (G) has a labeller error. Planted copies of w1:
// p1 plants the fault F (names), p2 the control L, p3 the fault K (other verb).
func fixtureLabels(t *testing.T, dir string) (labels, manifest string) {
	t.Helper()
	vac, not := verdictVacuous, verdictNotVacuous
	type write struct {
		event     int64
		source    string
		copy      string
		sentences []fixtureSentence
		err       string
	}
	writes := []write{
		{101, "w1", "", []fixtureSentence{{sentA, vac, shapeNames}, {sentB, not, "none"}, {sentI, vac, shapeNames}}, ""},
		{102, "w2", "", []fixtureSentence{{sentC, vac, shapeShows}, {sentD, vac, shapeOther}, {sentJ, vac, shapeOther}}, ""},
		{103, "w3", "", []fixtureSentence{{sentH, not, "none"}, {sentE, not, shapeShows}}, ""},
		{104, "w4", "", []fixtureSentence{{sentM, not, "none"}}, ""},
		{105, "w5", "", nil, ""},
		{106, "w6", "", []fixtureSentence{{sentG, vac, shapePays}}, "the labeller failed"},
		// The labeller's shape and verdict on a planted sentence are overridden by the manifest.
		{101, "w1", "p1", []fixtureSentence{{sentF, vac, "none"}, {sentB, not, "none"}}, ""},
		{101, "w1", "p2", []fixtureSentence{{sentL, vac, shapeNames}, {sentB, not, "none"}}, ""},
		{101, "w1", "p3", []fixtureSentence{{sentK, vac, shapeOther}, {sentB, not, "none"}}, ""},
	}
	var lines []labelRecord
	for _, w := range writes {
		r := labelRecord{EventID: w.event, SourceID: w.source, Copy: w.copy, Family: "comment", Error: w.err}
		for i, s := range w.sentences {
			r.Sentences = append(r.Sentences, sentenceLabel{ID: "s" + string(rune('1'+i)), Field: "body", Kind: "paragraph", Text: s.text, Verdict: s.verdict, Shape: s.shape})
		}
		r.WriteLabel = writeLabelOf(r.Sentences)
		if w.err != "" {
			r.WriteLabel = writeError
		}
		lines = append(lines, r)
	}
	labels = filepath.Join(dir, "labels.jsonl")
	if _, err := writeJSONL(labels, lines); err != nil {
		t.Fatal(err)
	}
	planted := []plantedCase{
		{Copy: "p1", Pair: "pair-1", Tag: plantedTag, Kind: kindFault, Shape: shapeNames, Verb: "name", Sentence: sentF, SentenceProse: sentF, EventID: 101, SourceID: "w1", Family: "comment", Field: "body", Seed: 7},
		{Copy: "p2", Pair: "pair-1", Tag: plantedTag, Kind: kindControl, Shape: shapeNames, Verb: "name", Sentence: sentL, SentenceProse: sentL, EventID: 101, SourceID: "w1", Family: "comment", Field: "body", Seed: 7},
		{Copy: "p3", Pair: "pair-2", Tag: plantedTag, Kind: kindFault, Shape: shapeOther, Verb: "carry", Sentence: sentK, SentenceProse: sentK, EventID: 101, SourceID: "w1", Family: "comment", Field: "body", Seed: 7},
	}
	manifest = filepath.Join(dir, "manifest.jsonl")
	if _, err := writeJSONL(manifest, planted); err != nil {
		t.Fatal(err)
	}
	return labels, manifest
}

func TestClopperPearsonBoundsMatchKnownValues(t *testing.T) {
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"0 of 59 at 95%", upperBound(0, 59, 0.95), 0.0495},
		{"0 of 183 at 97.5%", upperBound(0, 183, 0.975), 0.0199},
		{"0 of 45 at 90%", upperBound(0, 45, 0.90), 0.0499},
		{"0 of 121 at 97.5%", upperBound(0, 121, 0.975), 0.0300},
		// 1 − 0.05^(1/59): every one of 59 a hit.
		{"59 of 59 lower at 95%", lowerBound(59, 59, 0.95), 0.9505},
		// 27 of 30: the Beta(27, 4) quantile at 0.05.
		{"27 of 30 lower at 95%", lowerBound(27, 30, 0.95), 0.7614},
		{"1 of 10 upper at 95%", upperBound(1, 10, 0.95), 0.3942},
	} {
		if math.Abs(c.got-c.want) > 0.0001 {
			t.Errorf("%s: got %.5f, want %.4f", c.name, c.got, c.want)
		}
	}
	// The lower bound on hits is one less the upper bound on misses.
	for _, xn := range [][2]int{{3, 10}, {17, 40}, {90, 100}} {
		x, n := xn[0], xn[1]
		if d := lowerBound(x, n, 0.95) - (1 - upperBound(n-x, n, 0.95)); math.Abs(d) > 1e-9 {
			t.Errorf("%d of %d: lower bound and 1 − the upper bound on misses differ by %g", x, n, d)
		}
	}
	if upperBound(0, 0, 0.95) != 1 || lowerBound(0, 0, 0.95) != 0 || upperBound(5, 5, 0.95) != 1 || lowerBound(0, 5, 0.95) != 0 {
		t.Error("the edge bounds are not 1 and 0")
	}
	for _, c := range []struct {
		e, conf float64
		want    int
	}{{0.05, 0.95, 59}, {0.05, 0.90, 45}, {0.02, 0.975, 183}, {0.03, 0.975, 122}} {
		if got := requiredDraws(c.e, c.conf); got != c.want {
			t.Errorf("requiredDraws(%v, %v) = %d, want %d", c.e, c.conf, got, c.want)
		}
	}
}

func TestRecallCountsEachPopulationWithAndWithoutLists(t *testing.T) {
	dir := t.TempDir()
	labels, manifest := fixtureLabels(t, dir)
	out := filepath.Join(dir, "recall")
	code, stdout, stderr := runCmd(t, "recall", "--labels", labels, "--planted", manifest, "--heldout-seed", "3231", "--out", out)
	if code != 0 {
		t.Fatalf("recall exited %d: %s", code, stderr)
	}
	var report recallReport
	data, err := os.ReadFile(filepath.Join(out, "recall.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	rows := map[string]recallRow{}
	for _, r := range report.Rows {
		rows[r.Population] = r
	}
	// Reference-vacuous sentences of usable writes: A, I (names), C (shows), D, J (other) of the
	// real writes; F (names) and K (other) planted. G sits in w6, whose label failed; the control
	// L is not vacuous whatever the labeller said.
	want := map[string][2]int{
		shapeNames:              {2, 3}, // A, F found; I not
		shapeShows:              {1, 1},
		shapePays:               {0, 0},
		"no-list (all)":         {2, 3}, // D, K found; J not
		"all reference-vacuous": {5, 7},
	}
	for pop, w := range want {
		r := rows[pop]
		if r.Lists.Hits != w[0] || r.Lists.N != w[1] {
			t.Errorf("%s: %d of %d, want %d of %d", pop, r.Lists.Hits, r.Lists.N, w[0], w[1])
		}
		if r.NoLists.Hits < r.Lists.Hits {
			t.Errorf("%s: deleting the lists lowered the recall", pop)
		}
	}
	tuning, held := rows["no-list (tuning half)"], rows["no-list (held-out half)"]
	if tuning.Lists.N+held.Lists.N != 3 || report.TuningSentences+report.HeldSentences != 3 {
		t.Errorf("the halves hold %d and %d no-list sentences, not 3 in all", tuning.Lists.N, held.Lists.N)
	}
	if got := len(readJSONL[noListLine](t, filepath.Join(out, "nolist-heldout.jsonl"))); got != held.Lists.N {
		t.Errorf("nolist-heldout.jsonl holds %d lines, the row %d", got, held.Lists.N)
	}
	// Writes: 9; sentences: 16; candidates A, C, D, F, H, K, G.
	if s := report.Share.Sentences; s.Hits != 7 || s.N != 16 {
		t.Errorf("candidate sentences %d of %d, want 7 of 16", s.Hits, s.N)
	}
	if s := report.Share.Writes; s.Hits != 6 || s.N != 9 {
		t.Errorf("writes with a candidate %d of %d, want 6 of 9", s.Hits, s.N)
	}
	if !report.R13Holds || !strings.Contains(stdout, "R13") {
		t.Errorf("R13 not reported as holding:\n%s", stdout)
	}
	// The split is by unit and seed alone: the planted fault K's unit is its verb.
	split := heldout{Seed: 3231, Share: 0.5}
	if split.held("verb:carry") != containsUnit(t, filepath.Join(out, "nolist-heldout.jsonl"), "verb:carry") {
		t.Error("the planted fault's half is not the one its verb hashes to")
	}
}

func containsUnit(t *testing.T, path, unit string) bool {
	for _, l := range readJSONL[noListLine](t, path) {
		if l.Unit == unit {
			return true
		}
	}
	return false
}

// Find answers its candidates strongest first, so candidateSentences maps each one to its
// sentence in any order; a sentence that appears twice is taken once per candidate, and a
// candidate with no sentence left is an error.
func TestCandidateSentencesMatchInAnyOrder(t *testing.T) {
	ps := []prose.Sentence{
		{Field: "body", Text: "The plan pays off."},
		{Field: "body", Text: "This names the problem."},
		{Field: "title", Text: "This names the problem."},
		{Field: "body", Text: "This names the problem."},
	}
	cands := []candidateRef{
		{ID: "c1", Field: "body", Text: "This names the problem."},
		{ID: "c2", Field: "title", Text: "This names the problem."},
		{ID: "c3", Field: "body", Text: "The plan pays off."},
		{ID: "c4", Field: "body", Text: "This names the problem."},
	}
	got, err := candidateSentences(ps, cands)
	if err != nil || !reflect.DeepEqual(got, []int{1, 2, 0, 3}) {
		t.Fatalf("candidateSentences = %v, %v; want [1 2 0 3]", got, err)
	}
	if _, err := candidateSentences(ps, append(cands, candidateRef{ID: "c5", Field: "body", Text: "The plan pays off."})); err == nil {
		t.Fatal("a candidate with no sentence left gave no error")
	}
}

// scoreFixture scores the fixture's stratum against the stub and answers the run's directory.
func scoreFixture(t *testing.T, dir, stratum string, extra ...string) (string, *stubModel) {
	t.Helper()
	labels, manifest := fixtureLabels(t, dir)
	stub := newStubModel(t, containsAny("names the", "shows the", "frames", "lists the steps", "carries"), containsAny("pays off"))
	out := filepath.Join(dir, "score-"+stratum)
	args := append([]string{"score", "--labels", labels, "--planted", manifest, "--stratum", stratum, "--out", out,
		"--root", "../..", "--labeller-bound", "0.02", "--heldout-seed", "3231"}, stub.args(t, dir)...)
	code, _, stderr := runCmd(t, append(args, extra...)...)
	if code != 0 {
		t.Fatalf("score exited %d: %s", code, stderr)
	}
	return out, stub
}

func readFigures(t *testing.T, path string) figuresFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f figuresFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestScoreRunsTheDetectorPerWriteAndComputesTheFigures(t *testing.T) {
	dir := t.TempDir()
	out, stub := scoreFixture(t, dir, stratumReal)
	f := readFigures(t, filepath.Join(out, "figures.json"))
	g := f.Figures
	// One call per write with a candidate: w1, w2, w3 and w6 (whose call the stub fails).
	if got := stub.calls.Load(); got != 4 {
		t.Errorf("the stub got %d calls, want 4", got)
	}
	wantVerdicts := map[string]int{scanFailure: 3, scanNoFailure: 1, scanNotApplicable: 1, scanError: 1}
	for v, n := range wantVerdicts {
		if g.ScannerVerdicts[v] != n {
			t.Errorf("verdict %s: %d writes, want %d (%v)", v, g.ScannerVerdicts[v], n, g.ScannerVerdicts)
		}
	}
	check := func(name string, got rate, hits, n int) {
		t.Helper()
		if got.Hits != hits || got.N != n {
			t.Errorf("%s: %d of %d, want %d of %d", name, got.Hits, got.N, hits, n)
		}
	}
	s, b := g.Scanner, g.Baseline
	check("write precision", s.WritePrecision, 2, 3)
	check("write recall", s.WriteRecall, 2, 2)
	check("sentence precision", s.SentencePrecision, 3, 4)
	check("recall names", s.RecallByShape[shapeNames], 1, 2)
	check("recall shows", s.RecallByShape[shapeShows], 1, 1)
	check("recall pays", s.RecallByShape[shapePays], 0, 0)
	check("recall no-list", s.RecallNoList, 1, 2)
	check("recall all", s.RecallAll, 3, 5)
	check("baseline write precision", b.WritePrecision, 2, 3)
	check("baseline sentence precision", b.SentencePrecision, 2, 3)
	check("baseline recall all", b.RecallAll, 2, 5)
	check("baseline recall no-list", b.RecallNoList, 0, 2)
	if fd := s.WriteFalseDiscovery; fd.Errors != 1 || fd.N != 3 || math.Abs(fd.Upper95-upperBound(1, 3, 0.95)) > 1e-12 {
		t.Errorf("1 − write precision: %+v", fd)
	}
	if c := g.Composed; c == nil || math.Abs(c.Composed-(0.02+upperBound(1, 3, 0.975))) > 1e-12 || c.Certified {
		t.Errorf("composed bound: %+v", c)
	}
	if math.Abs(g.ErrorShare-1.0/6) > 1e-12 || math.Abs(g.NotApplicableShare-1.0/6) > 1e-12 {
		t.Errorf("shares: error %v, not_applicable %v", g.ErrorShare, g.NotApplicableShare)
	}
	if g.Latency.Calls != 4 || g.ServedModels[stubModelID] != 3 || g.Latency.TokensP95 <= 0 || g.Latency.CeilingTokens != 2*g.Latency.TokensP95 {
		t.Errorf("latency and models: %+v %v", g.Latency, g.ServedModels)
	}
	if g.SentenceBootstrap == nil || g.SentenceBootstrap[0] > g.SentenceBootstrap[1] {
		t.Errorf("bootstrap interval: %v", g.SentenceBootstrap)
	}
	if len(f.Run.BundleDigest) != 64 {
		t.Errorf("the run names no bundle digest: %q", f.Run.BundleDigest)
	}
	outputs := readJSONL[caseOutput](t, filepath.Join(out, "outputs.jsonl"))
	if len(outputs) != 6 {
		t.Fatalf("outputs.jsonl holds %d writes, want 6", len(outputs))
	}
	for _, o := range outputs {
		if o.Write == "106/w6" && (o.Outcome != "reader_error" || o.Verdict != scanError || o.Error == "") {
			t.Errorf("the failed call's output: %+v", o)
		}
		if o.Write == "101/w1" && (len(o.Candidates) != 1 || o.Candidates[0].SentenceID != "s1" || o.Model != stubModelID || len(o.Verdicts) != 1 || o.Verdicts[0].Rewrite == "") {
			t.Errorf("w1's output: %+v", o)
		}
	}
}

func TestScorePlantedStratumReadsThePlantedFaults(t *testing.T) {
	out, _ := scoreFixture(t, t.TempDir(), stratumPlanted)
	g := readFigures(t, filepath.Join(out, "figures.json")).Figures
	if g.Writes != 3 || g.ScannerVerdicts[scanFailure] != 2 || g.ScannerVerdicts[scanNoFailure] != 1 {
		t.Errorf("planted verdicts: %v of %d", g.ScannerVerdicts, g.Writes)
	}
	if r := g.Scanner.RecallByShape[shapeNames]; r.Hits != 1 || r.N != 1 {
		t.Errorf("planted names recall %d of %d, want 1 of 1 (the control is not vacuous)", r.Hits, r.N)
	}
	if r := g.Scanner.RecallNoList; r.Hits != 1 || r.N != 1 {
		t.Errorf("planted no-list recall %d of %d, want 1 of 1", r.Hits, r.N)
	}
	if p := g.Scanner.WritePrecision; p.Hits != 2 || p.N != 2 {
		t.Errorf("planted write precision %d of %d", p.Hits, p.N)
	}
}

func TestReproduceMatchesTheStoredRunAndFailsOnAChange(t *testing.T) {
	dir := t.TempDir()
	out, stub := scoreFixture(t, dir, stratumReal)
	calls := stub.calls.Load()
	labels, manifest := filepath.Join(dir, "labels.jsonl"), filepath.Join(dir, "manifest.jsonl")
	outputs, figs := filepath.Join(out, "outputs.jsonl"), filepath.Join(out, "figures.json")
	code, stdout, stderr := runCmd(t, "reproduce", "--outputs", outputs, "--labels", labels, "--planted", manifest, "--figures", figs)
	if code != 0 {
		t.Fatalf("reproduce exited %d: %s%s", code, stdout, stderr)
	}
	if stub.calls.Load() != calls {
		t.Error("reproduce called the model")
	}
	// A stored verdict turned from vacuous to not vacuous: the outputs no longer give the figures,
	// and the stored write verdict no longer follows from them.
	data, err := os.ReadFile(outputs)
	if err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(dir, "outputs-edited.jsonl")
	if err := os.WriteFile(edited, []byte(strings.Replace(string(data), `"vacuous":true`, `"vacuous":false`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = runCmd(t, "reproduce", "--outputs", edited, "--labels", labels, "--planted", manifest, "--figures", figs)
	for _, want := range []string{"outputs.jsonl has sha256", "is not the verdict its outcome and verdicts give", "figures.scanner.write_precision.hits"} {
		if code != 1 || !strings.Contains(stdout, want) {
			t.Errorf("an edited output: exit %d, want %q in\n%s", code, want, stdout)
		}
	}
	// A figure changed in figures.json.
	f := readFigures(t, figs)
	f.Figures.Scanner.WritePrecision.Hits = 3
	if err := writeJSON(figs, f); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = runCmd(t, "reproduce", "--outputs", outputs, "--labels", labels, "--planted", manifest, "--figures", figs)
	if code != 1 || !strings.Contains(stdout, "figures.scanner.write_precision.hits") {
		t.Errorf("an edited figure: exit %d\n%s", code, stdout)
	}
}

func TestAFinalTestIsReadOnce(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "exposure.jsonl")
	set := strings.Repeat("ab", 32)
	scoreFixture(t, dir, stratumReal, "--final-test", "--set-digest", set, "--seed", "42", "--exposure-log", log)
	runs := readJSONL[exposure](t, log)
	if len(runs) != 1 || runs[0].Kind != exposureFinalTest || runs[0].SetDigest != set || runs[0].Seed != 42 || len(runs[0].BundleDigest) != 64 || runs[0].ModelID != stubModelID {
		t.Fatalf("the exposure log: %+v", runs)
	}
	labels, manifest := filepath.Join(dir, "labels.jsonl"), filepath.Join(dir, "manifest.jsonl")
	stub := newStubModel(t, containsAny("names the"), nil)
	args := append([]string{"score", "--labels", labels, "--planted", manifest, "--stratum", stratumReal, "--out", filepath.Join(dir, "again"),
		"--root", "../..", "--final-test", "--set-digest", set, "--seed", "42", "--exposure-log", log}, stub.args(t, dir)...)
	code, _, stderr := runCmd(t, args...)
	if code != 1 || !strings.Contains(stderr, "already holds final-test run") {
		t.Errorf("a second final-test run: exit %d: %s", code, stderr)
	}
	if stub.calls.Load() != 0 {
		t.Error("the refused run called the model")
	}
	if got := len(readJSONL[exposure](t, log)); got != 1 {
		t.Errorf("the exposure log holds %d runs after the refusal", got)
	}
}

func TestDigestHashesTheBundleFilesInOrder(t *testing.T) {
	code, stdout, stderr := runCmd(t, "digest", "--root", "../..")
	if code != 0 {
		t.Fatalf("digest exited %d: %s", code, stderr)
	}
	digest, files, err := bundleDigest("../..")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "bundle digest: "+digest) {
		t.Errorf("stdout does not print the digest:\n%s", stdout)
	}
	var listing strings.Builder
	var paths []string
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join("../..", f.Path))
		if err != nil {
			t.Fatal(err)
		}
		listing.WriteString(sha256Hex(data) + "  " + f.Path + "\n")
		paths = append(paths, f.Path)
	}
	if got := sha256Hex([]byte(listing.String())); got != digest {
		t.Errorf("the digest is not the sha256 of the sha256sum listing: %s vs %s", got, digest)
	}
	for _, want := range []string{"internal/dispatch/writingcheck/prose/prose.go", "internal/dispatch/writingcheck/reader/prompt.md",
		"internal/dispatch/writingcheck/bundle.go", "internal/dispatch/writingcheck/baseline/baseline.go"} {
		found := false
		for _, p := range paths {
			found = found || p == want
		}
		if !found {
			t.Errorf("the bundle leaves out %s", want)
		}
	}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") || strings.Contains(p, "/eval/") {
			t.Errorf("the bundle holds %s", p)
		}
	}
	if code, _, _ := runCmd(t, "digest", "--root", t.TempDir()); code != 1 {
		t.Error("a root with no bundle did not fail")
	}
}
