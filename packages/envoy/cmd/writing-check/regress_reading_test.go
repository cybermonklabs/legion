package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegressBoundsEachClassDisagreement(t *testing.T) {
	dir := t.TempDir()
	cases := []regressCase{
		{ID: "r1", Body: sentA + " " + sentB, Expect: []regressVerdict{{sentA, true}, {sentB, false}}},
		{ID: "r2", Body: sentH, Expect: []regressVerdict{{sentH, false}}},
		{ID: "r3", Body: sentC + "\n\n" + sentM, Expect: []regressVerdict{{sentC, true}, {sentM, false}}},
	}
	path := filepath.Join(dir, "cases.jsonl")
	if _, err := writeJSONL(path, cases); err != nil {
		t.Fatal(err)
	}
	// The stub calls H vacuous: one of the three not-vacuous sentences disagrees.
	stub := newStubModel(t, containsAny("names the", "shows the", "lists the steps"), nil)
	out := filepath.Join(dir, "regress.json")
	code, stdout, stderr := runCmd(t, append([]string{"regress", "--cases", path, "--repeat-bound", "0.34", "--out", out}, stub.args(t, dir)...)...)
	if code != 0 {
		t.Fatalf("regress at a bound of 34%%: exit %d: %s%s", code, stdout, stderr)
	}
	var report regressReport
	data, _ := os.ReadFile(out)
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if c := report.Classes; c[0].N != 2 || c[0].Disagree != 0 || c[1].N != 3 || c[1].Disagree != 1 {
		t.Errorf("classes: %+v", c)
	}
	code, stdout, _ = runCmd(t, append([]string{"regress", "--cases", path, "--repeat-bound", "0.30"}, stub.args(t, dir)...)...)
	if code != 1 || !strings.Contains(stdout, "Result: FAIL") {
		t.Errorf("regress at a bound of 30%%: exit %d\n%s", code, stdout)
	}
	// A reader fault disagrees on every expected sentence of its case.
	failing := newStubModel(t, containsAny("names the"), containsAny("shows the"))
	code, stdout, _ = runCmd(t, append([]string{"regress", "--cases", path, "--repeat-bound", "0.34"}, failing.args(t, dir)...)...)
	if code != 1 || !strings.Contains(stdout, "reader_error") {
		t.Errorf("a reader fault: exit %d\n%s", code, stdout)
	}
	// An expected sentence that is not a sentence of its body is refused.
	bad := filepath.Join(dir, "bad.jsonl")
	if _, err := writeJSONL(bad, []regressCase{{ID: "x", Body: sentA, Expect: []regressVerdict{{sentB, false}}}}); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCmd(t, append([]string{"regress", "--cases", bad, "--repeat-bound", "0.1"}, stub.args(t, dir)...)...); code != 2 || !strings.Contains(stderr, "no prose sentence") {
		t.Errorf("a bad case: exit %d: %s", code, stderr)
	}
}

func TestRegressReadsTheReadersDevelopmentCases(t *testing.T) {
	cases, err := readRegressCases(filepath.Join("..", "..", "internal", "dispatch", "writingcheck", "eval", "regress-cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 60 {
		t.Errorf("%d development cases, want at least 60", len(cases))
	}
}

func TestQuotedBytesCountsBlockQuotesAndFences(t *testing.T) {
	quote := "> The fix shows the gap.\nand its lazy line.\n"
	fence := "```sh\necho hi\n\n> not a quote\n```\n"
	text := "The new section names the risk.\n\n" + quote + "\n" + fence + "\nA last paragraph.\n"
	q, f := quotedBytes(text)
	if q != len(quote) || f != len(fence) {
		t.Errorf("quotedBytes = %d, %d; want %d, %d", q, f, len(quote), len(fence))
	}
	if q, f := quotedBytes("    > indented code, not a quote\n"); q != 0 || f != 0 {
		t.Errorf("an indented line: %d, %d", q, f)
	}
	if _, f := quotedBytes("~~~\nnever closed\n"); f != len("~~~\nnever closed\n") {
		t.Errorf("an unclosed fence runs to the end: %d", f)
	}
}

func TestReadingCoversTheWindowLessTheExcludedManifests(t *testing.T) {
	dir := t.TempDir()
	quote := "> " + sentC + "\n"
	fence := "```\ncode\n```\n"
	body := sentA + "\n\n" + quote + "\n" + fence
	cases := []Case{
		{EventID: 150, SourceID: "before", Surface: surfaceComment, Field: "body", Text: sentA, Project: "ALPHA"},
		{EventID: 210, SourceID: "r1", Surface: surfaceComment, Field: "body", Text: body, Project: "ALPHA"},
		{EventID: 215, SourceID: "r1", Surface: surfaceComment, Field: "body", Text: sentA, Project: "ALPHA", Copy: "p9", Tag: plantedTag},
		{EventID: 220, SourceID: "r2", Surface: surfaceComment, Field: "body", Text: sentM, Project: "BETA"},
		{EventID: 230, SourceID: "r3", Surface: surfaceComment, Field: "body", Text: sentC, Project: "BETA"},
		{EventID: 300, SourceID: "r4", Surface: surfaceMessage, Field: "body", Text: sentH, Project: "BETA"},
		{EventID: 301, SourceID: "after", Surface: surfaceComment, Field: "body", Text: sentA, Project: "ALPHA"},
	}
	in := writeCases(t, dir, "cases.jsonl", cases)
	excluded := filepath.Join(dir, "development.jsonl")
	if _, err := writeJSONL(excluded, []splitRow{{ID: "230/r3/body", EventID: 230, SourceID: "r3", Field: "body", Group: "g1"}}); err != nil {
		t.Fatal(err)
	}
	stub := newStubModel(t, containsAny("names the", "shows the"), nil)
	out := filepath.Join(dir, "reading")
	code, stdout, stderr := runCmd(t, append([]string{"reading", "--in", in, "--after", "200", "--through", "300", "--exclude", excluded,
		"--out", out, "--root", "../.."}, stub.args(t, dir)...)...)
	if code != 0 {
		t.Fatalf("reading exited %d: %s%s", code, stdout, stderr)
	}
	var r readingReport
	data, _ := os.ReadFile(filepath.Join(out, "report.json"))
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	// r1, r2 and r4 are read: r3 is excluded, the copy and the writes outside the window are not.
	if r.Status != "reading" || r.Writes != 3 || r.ExcludedWrites != 1 || r.Projects["ALPHA"] != 1 || r.Projects["BETA"] != 2 {
		t.Fatalf("report: %+v", r)
	}
	// r1 (A) is refused by the detector; r4 (H) is a candidate the stub calls not vacuous.
	if r.DetectorRefused.Hits != 1 || r.CandidateWrites.Hits != 2 || r.CandidateSents.Hits != 2 || r.CandidateSents.N != 3 {
		t.Errorf("detector %+v, candidate writes %+v, candidate sentences %+v", r.DetectorRefused, r.CandidateWrites, r.CandidateSents)
	}
	bodyBytes := len(body) + len(sentM) + len(sentH)
	if r.BodyBytes != bodyBytes || *r.QuoteShare != share(len(quote), bodyBytes) || *r.FencedShare != share(len(fence), bodyBytes) {
		t.Errorf("bytes %d (want %d), quote %v, fenced %v", r.BodyBytes, bodyBytes, *r.QuoteShare, *r.FencedShare)
	}
	manifest, err := os.ReadFile(filepath.Join(out, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ManifestSHA256 != sha256Hex(manifest) || len(r.BundleDigest) != 64 || len(r.Excluded) != 1 {
		t.Errorf("manifest digest %s, bundle %q, excluded %+v", r.ManifestSHA256, r.BundleDigest, r.Excluded)
	}

	// A window with no agent write is "no reading", never zero.
	empty := filepath.Join(dir, "empty")
	code, stdout, _ = runCmd(t, append([]string{"reading", "--in", in, "--after", "400", "--through", "500", "--out", empty, "--root", "../.."}, stub.args(t, dir)...)...)
	if code != 0 || !strings.Contains(stdout, "no reading") {
		t.Fatalf("an empty window: exit %d\n%s", code, stdout)
	}
	data, _ = os.ReadFile(filepath.Join(empty, "report.json"))
	var none map[string]any
	if err := json.Unmarshal(data, &none); err != nil {
		t.Fatal(err)
	}
	if none["status"] != noReading || none["detector_failure_share"] != nil || none["quote_or_fenced_byte_share"] != nil {
		t.Errorf("an empty window's report: %v", none)
	}
}

func TestBootstrapIsSeeded(t *testing.T) {
	var writes []refWrite
	var flagged [][]bool
	for i := range 40 {
		w := refWrite{Label: writePass}
		var f []bool
		for k := range 1 + i%3 {
			verdict := verdictVacuous
			if (i+k)%4 == 0 {
				verdict = verdictNotVacuous
			}
			w.Sentences = append(w.Sentences, refSentence{Verdict: verdict})
			f = append(f, true)
		}
		writes, flagged = append(writes, w), append(flagged, f)
	}
	a := bootstrapSentencePrecision(writes, flagged, 1, 500)
	b := bootstrapSentencePrecision(writes, flagged, 1, 500)
	c := bootstrapSentencePrecision(writes, flagged, 2, 500)
	if *a != *b {
		t.Errorf("one seed gave %v and %v", *a, *b)
	}
	if *a == *c {
		t.Errorf("seeds 1 and 2 gave the same interval %v", *a)
	}
	if bootstrapSentencePrecision(writes, flagged, 1, 0) != nil {
		t.Error("no resample gave an interval")
	}
}
