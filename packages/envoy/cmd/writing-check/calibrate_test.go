package main

import (
	"bytes"
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The port draws what CPython draws: r = random.Random(3231), then [r.randrange(79) for _ in
// range(12)], the same with 21 from a fresh Random(3231), and [r.getrandbits(32) for _ in
// range(4)] from a third.
func TestMTMatchesPythonsRandom(t *testing.T) {
	for n, want := range map[int][]int{
		79: {61, 77, 26, 0, 6, 41, 18, 78, 75, 65, 60, 26},
		21: {15, 19, 6, 0, 1, 10, 4, 19, 18, 16, 15, 6},
	} {
		r := newPyRandom(3231)
		got := make([]int, len(want))
		for i := range got {
			got[i] = r.randrange(n)
		}
		if !slices.Equal(got, want) {
			t.Errorf("randrange(%d) drew %v, want %v", n, got, want)
		}
	}
	r := newPyRandom(3231)
	want := []uint32{2061968191, 3347766188, 4097650309, 2596364306}
	got := make([]uint32, len(want))
	for i := range got {
		got[i] = r.getrandbits(32)
	}
	if !slices.Equal(got, want) {
		t.Errorf("getrandbits(32) drew %v, want %v", got, want)
	}
}

// confirmedDraw is the draw of the confirmation read's seed on the real input's 729 question
// cases, which takes sample's set branch, as the pod's python3 (3.13.5) printed it:
//
//	python3 -c 'import random; print(random.Random(3231051).sample(range(729), 50))'
var confirmedDraw = []int{482, 307, 46, 102, 177, 2, 17, 53, 693, 455, 487, 726, 632, 606, 446, 170, 205, 50, 534, 302, 640, 656, 52, 184, 375,
	24, 581, 37, 643, 526, 208, 718, 672, 376, 401, 148, 42, 725, 587, 112, 723, 257, 92, 276, 211, 337, 109, 593, 520, 336}

// The port's sample draws what CPython's draws, in the same order: random.Random(seed).sample(
// range(n), k) as the pod's python3 (3.13.5) printed it with the command above each row, in the
// set branch (n above setsize), in the pool branch (n at most setsize), and from a seed of two
// 32-bit words.
func TestSampleMatchesPythonsSample(t *testing.T) {
	for _, c := range []struct {
		seed uint64
		n, k int
		want []int
	}{
		{3231051, 729, 50, confirmedDraw},
		// python3 -c 'import random; print(random.Random(20261115).sample(range(729), 50))'
		{20261115, 729, 50, []int{308, 25, 487, 719, 676, 619, 165, 360, 181, 714, 718, 463, 643, 196, 97, 47, 245, 528, 698, 191, 153, 80, 556, 506, 162,
			525, 728, 344, 348, 141, 373, 472, 375, 179, 557, 44, 625, 434, 8, 368, 568, 306, 332, 608, 361, 388, 333, 89, 214, 98}},
		// python3 -c 'import random; print(random.Random(77).sample(range(100), 50))'
		{77, 100, 50, []int{32, 41, 25, 30, 24, 14, 37, 60, 71, 78, 96, 18, 91, 80, 84, 64, 0, 35, 3, 63, 22, 95, 85, 49, 23,
			42, 98, 15, 11, 70, 33, 68, 92, 26, 28, 90, 9, 5, 54, 12, 44, 82, 57, 65, 38, 45, 34, 4, 6, 61}},
		// python3 -c 'import random; print(random.Random(1099514858827).sample(range(729), 10))', the seed 2**40 + 3231051
		{1099514858827, 729, 10, []int{306, 199, 548, 194, 20, 56, 107, 24, 349, 522}},
	} {
		if got := newPyRandom(c.seed).sample(c.n, c.k); !slices.Equal(got, c.want) {
			t.Errorf("Random(%d).sample(range(%d), %d) drew %v, want %v", c.seed, c.n, c.k, got, c.want)
		}
	}
}

// calFixtureRows is a made-up tuning part: question cases q01..q24 in split groups named for
// letters, what the owner did after each explanation under labels v1 and v2, the score of the
// explanation (yes, no, or error: a grade that failed), and the question check's categories.
// q22 is held out, q23's label is neither kept label, and india is in neither half.
var calFixtureRows = []struct {
	id, group, v1, part, score, v2 string
	categories                     []string
}{
	{"q01", "alpha", calUnderstood, dsDevelopment, "yes", calUnderstood, []string{"asks_word_meaning", "asks_word_meaning"}},
	{"q02", "alpha", calUnderstood, dsDevelopment, "yes", calUnderstood, []string{"asks_word_meaning", "order"}},
	{"q03", "alpha", calNotUnderstood, dsDevelopment, "no", calNotUnderstood, []string{"cannot_follow_wording", "cannot_follow_wording"}},
	{"q04", "bravo", calUnderstood, dsDevelopment, "no", calUnderstood, []string{"cannot_follow_wording", "asks_word_meaning"}},
	{"q05", "bravo", calNotUnderstood, dsDevelopment, "no", calNotUnderstood, []string{"asks_word_meaning", "cannot_follow_wording", "cannot_follow_wording"}},
	{"q06", "charlie", calUnderstood, dsDevelopment, "yes", calUnderstood, []string{"asks_word_meaning", "asks_word_meaning"}},
	{"q07", "charlie", calNotUnderstood, dsDevelopment, "yes", calUnderstood, []string{"cannot_follow_wording", "cannot_follow_wording"}},
	{"q08", "charlie", calUnderstood, dsDevelopment, "yes", calUnderstood, []string{"asks_word_meaning", "asks_word_meaning"}},
	{"q09", "delta", calUnderstood, dsDevelopment, "yes", calUnderstood, nil},
	{"q10", "echo", calUnderstood, dsDevelopment, "yes", calUnderstood, []string{"asks_word_meaning", "asks_word_meaning"}},
	{"q11", "echo", calUnderstood, dsDevelopment, "yes", calUnderstood, []string{"cannot_follow_wording", "cannot_follow_wording"}},
	{"q12", "echo", calNotUnderstood, dsDevelopment, "no", calNotUnderstood, []string{"asks_word_meaning", "asks_word_meaning"}},
	{"q13", "echo", calNotUnderstood, dsDevelopment, "yes", calUnderstood, []string{"cannot_follow_wording", "cannot_follow_wording"}},
	{"q14", "foxtrot", calUnderstood, dsDevelopment, "no", calUnderstood, []string{"cannot_follow_wording", "cannot_follow_wording"}},
	{"q15", "foxtrot", calUnderstood, dsDevelopment, "yes", calUnderstood, []string{"asks_word_meaning", "asks_word_meaning"}},
	{"q16", "golf", calNotUnderstood, dsDevelopment, "no", calNotUnderstood, []string{"cannot_follow_wording", "cannot_follow_wording"}},
	{"q17", "golf", calUnderstood, dsDevelopment, "yes", calUnderstood, []string{"asks_word_meaning", "asks_word_meaning"}},
	{"q18", "golf", calUnderstood, dsDevelopment, "error", calUnderstood, []string{"asks_word_meaning", "asks_word_meaning"}},
	{"q19", "hotel", calNotUnderstood, dsDevelopment, "no", calNotUnderstood, []string{"cannot_follow_wording", "cannot_follow_wording"}},
	{"q20", "hotel", calUnderstood, dsDevelopment, "no", calUnderstood, []string{"asks_word_meaning", "asks_word_meaning"}},
	{"q21", "hotel", calUnderstood, dsDevelopment, "yes", calUnderstood, []string{"cannot_follow_wording", "cannot_follow_wording"}},
	{"q22", "alpha", calUnderstood, dsHeldOut, "yes", calUnderstood, []string{"asks_word_meaning", "asks_word_meaning"}},
	{"q23", "echo", "unsure", dsDevelopment, "yes", "unsure", []string{"asks_word_meaning", "asks_word_meaning"}},
	{"q24", "india", calNotUnderstood, dsDevelopment, "no", calNotUnderstood, []string{"cannot_follow_wording", "cannot_follow_wording"}},
}

// calFixturePairs are pairs of the fixture's explanations: followed, not followed, group, half,
// part. The score prefers the first, ties the second, reverses the third, and prefers the
// fourth and fifth.
var calFixturePairs = [][5]string{
	{"e01", "e03", "alpha", calHalfFit, dsDevelopment},
	{"e04", "e05", "bravo", calHalfFit, dsDevelopment},
	{"e12", "e13", "echo", calHalfMeasure, dsDevelopment},
	{"e17", "e16", "golf", calHalfMeasure, dsDevelopment},
	{"e21", "e19", "hotel", dsHeldOut, dsHeldOut},
}

// writeJSONLines writes each value as one JSON line of path, making its directory first.
func writeJSONLines(t *testing.T, path string, values ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := writeJSONL(path, values); err != nil {
		t.Fatal(err)
	}
}

// writeCalFixture writes the fixture's files in a temporary directory and makes it the working
// directory: both labels files, two runs that score q01..q13 and q10..q24 (agreeing on
// q10..q13), the verdicts, the split with hotel read before it, and the pairs.
func writeCalFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	var v1, v2, runA, runB, verdicts []any
	for i, r := range calFixtureRows {
		label := func(l string) calLabel {
			return calLabel{CaseID: "question/" + r.id, Label: l, Part: r.part, SplitGroup: r.group, ExplanationTID: "e" + r.id[1:]}
		}
		v1, v2 = append(v1, label(r.v1)), append(v2, label(r.v2))
		grade := map[string]any{"verdict": r.score == "yes"}
		if r.score == "error" {
			grade = map[string]any{"error": "the grader stopped"}
		}
		c := map[string]any{"case_id": "answer/e" + r.id[1:], "grades": map[string]any{"answered": grade}}
		if i < 13 {
			runA = append(runA, c)
		}
		if i >= 9 {
			runB = append(runB, c)
		}
		verdicts = append(verdicts, map[string]any{"id": "question/" + r.id, "categories": r.categories})
	}
	writeJSONLines(t, filepath.Join(dir, "labels.jsonl"), v1...)
	writeJSONLines(t, filepath.Join(dir, "labels-v2.jsonl"), v2...)
	writeJSONLines(t, filepath.Join(dir, "run-a", "cases.jsonl"), runA...)
	writeJSONLines(t, filepath.Join(dir, "run-b", "cases.jsonl"), runB...)
	writeJSONLines(t, filepath.Join(dir, "verdicts.jsonl"), verdicts...)
	writeJSONLines(t, filepath.Join(dir, "split.json"), map[string]any{
		"seed": 1, "fit_groups": []string{"alpha", "bravo", "charlie", "delta"}, "measure_groups": []string{"echo", "foxtrot", "golf", "hotel"}})
	writeJSONLines(t, filepath.Join(dir, "read.json"), map[string]any{"their_measure_groups": []string{"hotel"}})
	var pairs []any
	for _, p := range calFixturePairs {
		pairs = append(pairs, calPair{NotFollowed: "question/x" + p[1], Followed: "question/x" + p[0],
			ExplNotFollowed: "answer/" + p[1], ExplFollowed: "answer/" + p[0], Group: p[2], Half: p[3], Part: p[4]})
	}
	writeJSONLines(t, filepath.Join(dir, "pairs.jsonl"), pairs...)
}

// calibrateArgs runs calibrate with args.
func calibrateArgs(args ...string) (code int, stdout, stderr string) {
	var out, errs bytes.Buffer
	code = run(context.Background(), append([]string{"calibrate"}, args...), &out, &errs)
	return code, out.String(), errs.String()
}

// figureLines is the lines of calibrate's output after its scores line and its bar.
func figureLines(t *testing.T, stdout string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "calibrate ") || !strings.HasPrefix(lines[1], "bar: ") {
		t.Fatalf("calibrate printed no scores line and bar:\n%s", stdout)
	}
	return lines[2:]
}

// The expected lines are agree.py's: its cluster_ci, agreement and catch, run with python3 on
// this fixture. Labels v2 over every tuning row passes; v1 fails on agreement; v2 on the fit
// half fails on its interval's lower end alone. q18's grade failed, so it is unscored; q22
// (held out) and q23 (no kept label) are not counted. q04's categories tie, and the first
// listed is its kind; q09 has no category.
func TestCalibrateReactionCheckMatchesAgreePy(t *testing.T) {
	writeCalFixture(t)
	base := []string{"--name", "fixture", "--run", "run-a,run-b", "--labels-v1", "labels.jsonl", "--labels-v2", "labels-v2.jsonl",
		"--split", "split.json", "--read-before-split", "read.json"}
	for _, tc := range []struct {
		half  string
		extra []string
		want  []string
	}{
		{calHalfAll, []string{"--verdicts", "verdicts.jsonl"}, []string{
			"labels v1, every tuning row: n 21 (9 groups, 1 unscored), agreement 76.2% [65.0%, 88.9%], caught 6 of 8 = 75.0% [50.0%, 100.0%]; understood & answered 10, understood & not answered 3, not understood & answered 2, not understood & not answered 6 -> FAIL",
			"labels v1, every tuning row, kind asks_word_meaning: 9 cases, 8 understood, agreement 88.9%, caught 1 of 1 = 100.0%",
			"labels v1, every tuning row, kind cannot_follow_wording: 11 cases, 4 understood, agreement 63.6%, caught 5 of 7 = 71.4%",
			"labels v1, every tuning row, kind (none): 1 cases, 1 understood, agreement 100.0%, caught 0 of 0 = -",
			"labels v2, every tuning row: n 21 (9 groups, 1 unscored), agreement 85.7% [70.6%, 100.0%], caught 6 of 6 = 100.0% [100.0%, 100.0%]; understood & answered 12, understood & not answered 3, not understood & answered 0, not understood & not answered 6 -> PASS",
			"labels v2, every tuning row, kind asks_word_meaning: 9 cases, 8 understood, agreement 88.9%, caught 1 of 1 = 100.0%",
			"labels v2, every tuning row, kind cannot_follow_wording: 11 cases, 6 understood, agreement 81.8%, caught 5 of 5 = 100.0%",
			"labels v2, every tuning row, kind (none): 1 cases, 1 understood, agreement 100.0%, caught 0 of 0 = -",
		}},
		{calHalfFit, nil, []string{
			"labels v1, fit half: n 9 (4 groups, 0 unscored), agreement 77.8% [57.1%, 100.0%], caught 2 of 3 = 66.7% [0.0%, 100.0%]; understood & answered 5, understood & not answered 1, not understood & answered 1, not understood & not answered 2 -> FAIL",
			"labels v2, fit half: n 9 (4 groups, 0 unscored), agreement 88.9% [66.7%, 100.0%], caught 2 of 2 = 100.0% [100.0%, 100.0%]; understood & answered 6, understood & not answered 1, not understood & answered 0, not understood & not answered 2 -> FAIL",
		}},
		{calHalfMeasure, nil, []string{
			"labels v1, measure half: n 11 (4 groups, 1 unscored), agreement 72.7% [60.0%, 88.9%], caught 3 of 4 = 75.0% [50.0%, 100.0%]; understood & answered 5, understood & not answered 2, not understood & answered 1, not understood & not answered 3 -> FAIL",
			"labels v1, measure half without the 1 groups read before the split: n 8 (3 groups, 1 unscored), agreement 75.0% [50.0%, 100.0%], caught 2 of 3 = 66.7% [50.0%, 100.0%]; understood & answered 4, understood & not answered 1, not understood & answered 1, not understood & not answered 2 -> FAIL",
			"labels v2, measure half: n 11 (4 groups, 1 unscored), agreement 81.8% [60.0%, 100.0%], caught 3 of 3 = 100.0% [100.0%, 100.0%]; understood & answered 6, understood & not answered 2, not understood & answered 0, not understood & not answered 3 -> FAIL",
			"labels v2, measure half without the 1 groups read before the split: n 8 (3 groups, 1 unscored), agreement 87.5% [50.0%, 100.0%], caught 2 of 2 = 100.0% [100.0%, 100.0%]; understood & answered 5, understood & not answered 1, not understood & answered 0, not understood & not answered 2 -> FAIL",
		}},
	} {
		code, stdout, stderr := calibrateArgs(append(slices.Clone(base), append([]string{"--half", tc.half}, tc.extra...)...)...)
		if code != 0 {
			t.Fatalf("--half %s: exit %d: %s", tc.half, code, stderr)
		}
		if got := figureLines(t, stdout); !slices.Equal(got, tc.want) {
			t.Errorf("--half %s printed\n%s\nwant\n%s", tc.half, strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
		}
	}

	// A rows file scores only the explanations it holds: the fit half's, so every tuning row
	// has the fit half's figures and the rest unscored.
	var rows []any
	for _, r := range calFixtureRows[:9] {
		rows = append(rows, map[string]any{"expl": "answer/e" + r.id[1:], "score": r.score == "yes"})
	}
	writeJSONLines(t, "rows.jsonl", rows...)
	code, stdout, stderr := calibrateArgs("--name", "judge", "--rows", "rows.jsonl", "--half", calHalfAll, "--labels-v1", "labels.jsonl")
	if code != 0 {
		t.Fatalf("--rows: exit %d: %s", code, stderr)
	}
	want := "labels v1, every tuning row: n 9 (4 groups, 13 unscored), agreement 77.8% [57.1%, 100.0%], caught 2 of 3 = 66.7% [0.0%, 100.0%]; understood & answered 5, understood & not answered 1, not understood & answered 1, not understood & not answered 2 -> FAIL"
	if got := figureLines(t, stdout); !slices.Equal(got, []string{want}) {
		t.Errorf("--rows printed %q, want %q", got, want)
	}
}

// The bar holds at its edges: 80% agreement with a lower end of 70% and a 70% catch pass, and
// each one a step below fails.
func TestCalibrateBarIsMetAtItsEdges(t *testing.T) {
	figures := func(agreed int, low float64, caught int) calReactionFigures {
		return calReactionFigures{
			agreement: calShare{hits: agreed, n: 100, ci: &[2]float64{low, 0.9}},
			caught:    calShare{hits: caught, n: 10},
		}
	}
	for _, tc := range []struct {
		f    calReactionFigures
		pass bool
	}{
		{figures(80, 0.70, 7), true},
		{figures(79, 0.70, 7), false},
		{figures(80, 0.69, 7), false},
		{figures(80, 0.70, 6), false},
		{calReactionFigures{agreement: calShare{hits: 9, n: 10, ci: &[2]float64{0.8, 1}}}, false},
	} {
		if got := tc.f.pass(); got != tc.pass {
			t.Errorf("%s: pass = %v, want %v", tc.f.text(), got, tc.pass)
		}
	}
}

// The pair check counts a prefer, a tie and a reversed pair, by half and by part, with
// agree.py's interval over the pairs' groups (python3 on this fixture). A pair whose
// explanation has no score fails the check, and the line gives the count of such pairs; pairs
// that all prefer pass. The pair check reads no labels and no split.
func TestCalibratePairCheckCountsPrefersTiesAndReversed(t *testing.T) {
	writeCalFixture(t)
	code, stdout, stderr := calibrateArgs("--name", "pairs", "--run", "run-a,run-b", "--pairs", "pairs.jsonl")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := []string{
		"pairs, all: n 5 (5 groups, 0 unscored), prefers 3 of 5 = 60.0% [20.0%, 100.0%], ties 1, reversed 1 -> FAIL",
		"pairs, half fit: n 2 (2 groups, 0 unscored), prefers 1 of 2 = 50.0% [0.0%, 100.0%], ties 1, reversed 0",
		"pairs, half held_out: n 1 (1 groups, 0 unscored), prefers 1 of 1 = 100.0% [100.0%, 100.0%], ties 0, reversed 0",
		"pairs, half measure: n 2 (2 groups, 0 unscored), prefers 1 of 2 = 50.0% [0.0%, 100.0%], ties 0, reversed 1",
		"pairs, part development: n 4 (4 groups, 0 unscored), prefers 2 of 4 = 50.0% [0.0%, 100.0%], ties 1, reversed 1",
		"pairs, part held_out: n 1 (1 groups, 0 unscored), prefers 1 of 1 = 100.0% [100.0%, 100.0%], ties 0, reversed 0",
	}
	if got := figureLines(t, stdout); !slices.Equal(got, want) {
		t.Errorf("printed\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	pair := func(followed, notFollowed, group string) calPair {
		return calPair{ExplFollowed: "answer/" + followed, ExplNotFollowed: "answer/" + notFollowed, Group: group, Half: calHalfMeasure, Part: dsDevelopment}
	}
	scores, err := calRunScores([]string{"run-a", "run-b"})
	if err != nil {
		t.Fatal(err)
	}
	prefer := []calPair{pair("e01", "e03", "alpha"), pair("e17", "e16", "golf"), pair("e21", "e19", "hotel")}
	if got, want := newCalPairFigures(prefer, scores).text(true), "n 3 (3 groups, 0 unscored), prefers 3 of 3 = 100.0% [100.0%, 100.0%], ties 0, reversed 0 -> PASS"; got != want {
		t.Errorf("pairs that all prefer: %q, want %q", got, want)
	}
	unscored := append(slices.Clone(prefer), pair("e18", "e16", "golf"))
	if got, want := newCalPairFigures(unscored, scores).text(true), "n 3 (3 groups, 1 unscored), prefers 3 of 3 = 100.0% [100.0%, 100.0%], ties 0, reversed 0 -> FAIL (pairs unscored: 1)"; got != want {
		t.Errorf("a pair unscored: %q, want %q", got, want)
	}
}

// --measured appends one record of every printed figure, by key, and refuses a second
// measurement of the name, a record of the earlier script's (its variant) included, writing
// nothing.
func TestCalibrateRefusesASecondMeasuredRecordOfOneName(t *testing.T) {
	writeCalFixture(t)
	const measured = "measured.jsonl"
	writeJSONLines(t, measured, map[string]any{"variant": "earlier", "run": "x", "figures": map[string]string{}})
	args := func(name string) []string {
		return []string{"--name", name, "--run", "run-a", "--half", calHalfFit, "--labels-v2", "labels-v2.jsonl", "--split", "split.json",
			"--pairs", "pairs.jsonl", "--measured", measured}
	}
	code, stdout, stderr := calibrateArgs(args("v6")...)
	if code != 0 {
		t.Fatalf("first measurement: exit %d: %s", code, stderr)
	}
	records := readJSONL[calRecord](t, measured)
	if len(records) != 2 {
		t.Fatalf("measured.jsonl holds %d records, want 2", len(records))
	}
	r := records[1]
	if r.Name != "v6" || !slices.Equal(r.Runs, []string{"run-a"}) || r.Rows != "" || r.Half != calHalfFit {
		t.Errorf("record = %+v", r)
	}
	if at, err := time.Parse(time.RFC3339, r.At); err != nil || at.Format(time.RFC3339) != r.At || !strings.HasSuffix(r.At, "Z") {
		t.Errorf("at = %q, want a UTC RFC 3339 time to the second (%v)", r.At, err)
	}
	printed := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if last := printed[len(printed)-1]; !strings.HasPrefix(last, "recorded v6 in ") {
		t.Errorf("the last line is %q", last)
	}
	printed = printed[:len(printed)-1]
	recorded := slices.Sorted(maps.Values(r.Figures))
	if !slices.Equal(recorded, slices.Sorted(slices.Values(printed))) {
		t.Errorf("the record's figures\n%s\nare not the printed lines\n%s", strings.Join(recorded, "\n"), strings.Join(printed, "\n"))
	}
	for key, want := range map[string]string{"v2/fit": "labels v2, fit half: ", "pairs/all": "pairs, all: ", "pairs/half/fit": "pairs, half fit: "} {
		if !strings.HasPrefix(r.Figures[key], want) {
			t.Errorf("figures[%q] = %q", key, r.Figures[key])
		}
	}

	before, err := os.ReadFile(measured)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"v6", "earlier"} {
		code, stdout, stderr = calibrateArgs(args(name)...)
		if code == 0 || stdout != "" || !strings.Contains(stderr, "recorded already") {
			t.Errorf("a second %s: exit %d, stdout %q, stderr %q", name, code, stdout, stderr)
		}
	}
	if after, err := os.ReadFile(measured); err != nil || !bytes.Equal(after, before) {
		t.Errorf("a refused measurement changed measured.jsonl (%v)", err)
	}
}

// Two runs that score one explanation both ways are refused. (run-a and run-b, which agree on
// q10..q13, are read together above.)
func TestCalibrateRefusesRunsThatScoreACaseBothWays(t *testing.T) {
	writeCalFixture(t)
	writeJSONLines(t, filepath.Join("run-c", "cases.jsonl"),
		map[string]any{"case_id": "answer/e10", "grades": map[string]any{"answered": map[string]any{"verdict": false}}})
	code, _, stderr := calibrateArgs("--name", "x", "--run", "run-a,run-c", "--half", calHalfAll, "--labels-v1", "labels.jsonl")
	if code != 1 || !strings.Contains(stderr, "answer/e10") || !strings.Contains(stderr, "ambiguous") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}
