package harness

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

func TestPlaceQuote(t *testing.T) {
	sentences := []LabelledSentence{
		{ID: "s1", Text: "The run is green."},
		{ID: "s2", Text: "This lands the change in CODE today."},
		{ID: "s3", Text: "Refuse a launch and state what it leaves out."},
	}
	for quote, want := range map[string]string{
		"lands the change":                      "s2",
		"This lands the change in `main` today": "s2", // code the label text writes as CODE
		"lands the change in main":              "s2", // a run of four of its five words
		"The run":                               "s1",
		"each unblocks the work and the launch": "", // common words alone place nothing
		"nothing of the kind":                   "",
	} {
		if got := placeQuote(quote, sentences); got != want {
			t.Errorf("placeQuote(%q) = %q, want %q", quote, got, want)
		}
	}
}

func TestPhrasesAgreement(t *testing.T) {
	writes := []labelledWrite{
		{EventID: 1, SourceID: "a", Group: "g1", Sentences: []LabelledSentence{
			{ID: "s1", Verdict: "vacuous"}, {ID: "s2", Verdict: "not_vacuous"}, {ID: "s3", Verdict: "unresolved"}}},
		{EventID: 2, SourceID: "b", Group: "g2", Sentences: []LabelledSentence{
			{ID: "s1", Verdict: "vacuous"}, {ID: "s2", Verdict: "not_vacuous"}}},
	}
	results := map[string]LabelResult{
		"1/a": {Key: "1/a", Predicted: []string{"s1", "s2", "s3"}, Unplaced: []string{"q"}},
		"2/b": {Key: "2/b"},
	}
	a := phrasesAgreement("labels", writes, results, 1, 200)
	// Found: s1 (vacuous) and s2 (not) of write 1; s3 has no verdict. Vacuous: two, one found.
	if a.Writes != 2 || a.Sentences != 4 || a.Vacuous != 2 || *a.Precision.Share != 0.5 || *a.Recall.Share != 0.5 ||
		a.Unplaced != 1 || a.PrecisionUnplaced.Cases != 3 || a.PrecisionUnplaced.Hits != 1 {
		t.Errorf("the agreement: %+v", a)
	}
}

func TestThePhrasesOfLabelledWrites(t *testing.T) {
	dir := t.TempDir()
	writeLines := func(name string, lines ...any) {
		var b strings.Builder
		for _, l := range lines {
			b.WriteString(asJSON(t, l) + "\n")
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeLines("cases.jsonl",
		map[string]any{"event_id": 1, "source_id": "a", "field": "body", "group": "g1", "text": "This names the problem. The run is green."},
		map[string]any{"event_id": 2, "source_id": "b", "field": "body", "group": "g2", "text": "The run is red."})
	writeLines("labels.jsonl",
		map[string]any{"event_id": 1, "source_id": "a", "family": "comment", "sentences": []map[string]string{
			{"id": "s1", "field": "body", "text": "This names the problem.", "verdict": "vacuous"},
			{"id": "s2", "field": "body", "text": "The run is green.", "verdict": "not_vacuous"}}},
		map[string]any{"event_id": 2, "source_id": "b", "family": "comment", "sentences": []map[string]string{
			{"id": "s1", "field": "body", "text": "The run is red.", "verdict": "not_vacuous"}}})
	stand := modeltest.New(t, graderRules(t)...)
	out := t.TempDir()
	code, stdout, stderr := runHarness("phrases-labels", "--labels", dir, "--out", out, "--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50")
	if code != 0 {
		t.Fatalf("phrases-labels = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "1 of 1 = 100.0%") {
		t.Errorf("the agreement:\n%s", stdout)
	}
	calls := len(stand.Requests())
	if code, _, stderr := runHarness("phrases-labels", "--labels", dir, "--out", out, "--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50"); code != 0 || len(stand.Requests()) != calls {
		t.Errorf("a second run graded again: %d, %d calls after %d: %s", code, len(stand.Requests()), calls, stderr)
	}
	// The results of --out are another grading's for other labels: a run is refused there.
	other := t.TempDir()
	for _, name := range []string{"cases.jsonl", "labels.jsonl"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || os.WriteFile(filepath.Join(other, name), data, 0o600) != nil {
			t.Fatalf("copying %s: %v", name, err)
		}
	}
	if code, _, stderr := runHarness("phrases-labels", "--labels", other, "--out", out, "--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50"); code != 1 || !strings.Contains(stderr, "holds a run of other parameters") {
		t.Errorf("a run of other labels in the first run's --out = %d: %s", code, stderr)
	}
	// A run with a write left ungraded writes no agreement, which would read as one of every write.
	refused := modeltest.Rule{Contains: "This names the problem.", Reply: modeltest.Reply{Status: http.StatusBadRequest, Text: "the stand-in refuses to grade this write"}}
	failing := modeltest.New(t, append([]modeltest.Rule{refused}, graderRules(t)...)...)
	partial := t.TempDir()
	code, stdout, stderr = runHarness("phrases-labels", "--labels", dir, "--out", partial, "--gateway", failing.URL, "--gateway-key-file", keyFile(t), "--draws", "50")
	if code != 1 || !strings.Contains(stderr, "1 of 2 writes have no result, so no agreement is written") || stdout != "" {
		t.Errorf("a run with a write ungraded = %d:\n%s\n%s", code, stdout, stderr)
	}
	for _, name := range []string{"agreement.json", "agreement.md"} {
		if _, err := os.Stat(filepath.Join(partial, name)); !os.IsNotExist(err) {
			t.Errorf("the run with a write ungraded wrote %s (%v)", name, err)
		}
	}
}

func TestAskedWordsRecall(t *testing.T) {
	words := func(ws ...string) *CaseText { return &CaseText{Verdict: &CaseVerdict{Verdict: "words", Words: ws}} }
	ds := &Dataset{Cases: []*Case{
		{ID: "q1", Group: GroupQuestion, SplitGroup: "g1", Agent: CaseText{Text: "The zork train is parked at the door."}, OwnerReply: words("zork train", "parked", "absent word")},
		{ID: "q2", Group: GroupQuestion, SplitGroup: "g2", Agent: CaseText{Text: "The lane session stopped."}, OwnerReply: words("lane session")},
		{ID: "a1", Group: GroupAnswer, SplitGroup: "g3", Agent: CaseText{Text: "zork"}, OwnerReply: words("zork")},
	}}
	results := map[string]CaseResult{
		"q1": {CaseID: "q1", Grades: map[Judgement]*Grade{JUndefined: {Items: []Item{{Quote: "zork"}}}}},
		"q2": {CaseID: "q2", Grades: map[Judgement]*Grade{JUndefined: {Items: []Item{{Quote: "the lane session"}}}}},
		"a1": {CaseID: "a1", Grades: map[Judgement]*Grade{JUndefined: {Items: []Item{{Quote: "zork"}}}}},
	}
	r := askedWordsRecall("run", ds, results, 1, 200)
	// q1: "zork train" holds the finding "zork"; "parked" is not listed; "absent word" is not
	// in the text. q2: the finding holds "lane session". The answer case is not counted.
	if r.Cases != 2 || r.Asked != 3 || r.NotInText != 1 || r.Recall.Hits != 2 || r.Recall.Cases != 3 {
		t.Errorf("the recall: %+v", r)
	}
}
