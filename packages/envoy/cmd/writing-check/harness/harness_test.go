package harness

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// Every text below is written for these tests; none comes from Dispatch.
const (
	fixtureWriter    = "01a10000-0000-7000-8000-000000000001"
	fixtureOwner     = "owner@example.com"
	fixtureSessionFx = "SESSIONFACT-7: the zork train is the queue of pull requests"
	fixtureThinking  = "a thought the export holds and no grader reads"
	plainText        = "Merge pull request 12?\n- yes: merge it\n- no: wait for the review"
	answerText       = "Work is done on part one.\n\nThe blocker is the release of engine one."
)

// fixtureCases are four development cases, one of each group, and one held-out case.
func fixtureCases() []Case {
	record := CaseRecord{
		Project:   "TEST",
		Issue:     &CaseIssue{Key: "TEST-1", Title: "Build the zork", Root: "TEST-1"},
		Documents: []CaseDocument{{ArtifactID: "art-1", Name: "spec.md", Version: 1}},
		Thread:    []CaseText{{Key: "comment:t1", Kind: "comment", ActorKind: "user", Actor: fixtureOwner, Text: "Start the zork work.", Time: "2026-11-01T00:00:00Z"}},
	}
	agent := func(key string, id int64, text string) CaseText {
		return CaseText{Key: key, EventID: id, Kind: "comment", ActorKind: "session", Actor: fixtureWriter, Session: fixtureWriter, Text: text}
	}
	owner := func(text, verdict string) *CaseText {
		return &CaseText{Key: "comment:o", ActorKind: "user", Actor: fixtureOwner, Text: text, Verdict: &CaseVerdict{Verdict: verdict}}
	}
	return []Case{
		{ID: "question/comment:q1@11", Group: GroupQuestion, Part: PartDevelopment, SplitGroup: "TREE-1", Kind: "comment", WriterSession: fixtureWriter,
			Agent: agent("comment:a1", 10, "The zork train is ready.\n\nIt merges at noon."), OwnerReply: owner("What is the zork train?", "asks_word_meaning"), Record: record},
		{ID: "answer/comment:a2@20", Group: GroupAnswer, Part: PartDevelopment, SplitGroup: "TREE-2", Kind: "comment", Conversation: true, WriterSession: fixtureWriter,
			Agent: agent("comment:a2", 20, answerText), Question: owner("What is the blocker?", "asks_agent_question"), Record: record},
		{ID: "plain/ask:p3@30", Group: GroupPlain, Part: PartDevelopment, SplitGroup: "TREE-3", Kind: "ask", WriterSession: "01a10000-0000-7000-8000-000000000002",
			Agent: agent("ask:p3", 30, plainText), OwnerReply: owner("yes", "none"), Record: record},
		{ID: "phrase/comment:f4@40", Group: GroupPhrase, Part: PartDevelopment, SplitGroup: "TREE-4", Kind: "comment",
			Agent: agent("comment:f4", 40, "This names the problem."), Record: record},
		{ID: "question/comment:h5@51", Group: GroupQuestion, Part: PartHeldOut, SplitGroup: "TREE-5", Kind: "comment", WriterSession: fixtureWriter,
			Agent: agent("comment:a5", 50, "The flim gate is shut."), OwnerReply: owner("What is the flim gate?", "asks_word_meaning"), Record: record},
	}
}

// writeGzipLines writes each of lines as one JSON line of a gzipped file.
func writeGzipLines(t *testing.T, path string, lines ...any) {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	enc := json.NewEncoder(gz)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeFixture writes a dataset build and its session export under a temporary directory.
func writeFixture(t *testing.T) (build, sessions string) {
	t.Helper()
	root := t.TempDir()
	build, sessions = filepath.Join(root, "build"), filepath.Join(root, "sessions")
	for _, d := range []string{build, sessions} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var cases []any
	for _, c := range fixtureCases() {
		cases = append(cases, c)
	}
	writeGzipLines(t, filepath.Join(build, casesFile), cases...)
	writeGzipLines(t, filepath.Join(build, documentsFile), map[string]any{"artifact_id": "art-1", "version": 1, "markdown": "# Spec\n\nThe work builds the zork."})
	writeJSONFile(t, filepath.Join(build, "counts.json"), map[string]any{"owner": fixtureOwner})

	row := func(index int, host, role, kind, text string) map[string]any {
		return map[string]any{"session_id": fixtureWriter, "index": index, "row": map[string]any{
			"session_id": fixtureWriter, "host": host, "seq": index, "ts": "2026-11-01 00:00:0" + string(rune('0'+index)), "role": role, "kind": kind, "text": text}}
	}
	writeGzipLines(t, filepath.Join(sessions, "session-rows.jsonl.gz"),
		row(0, "a", "user", "user", "Please build the zork train."),
		row(1, "a", "assistant", "thinking", fixtureThinking),
		row(2, "a", "toolResult", "tool_result", fixtureSessionFx),
		row(3, "a", "assistant", "assistant", "The zork train is built."),
		row(4, "a", "assistant", "assistant", "A row after every case's place."),
	)
	places := []map[string]any{
		{"case_id": "question/comment:q1@11", "session_id": fixtureWriter, "status": "found", "rows": 3},
		{"case_id": "answer/comment:a2@20", "session_id": fixtureWriter, "status": "found", "rows": 4},
		{"case_id": "plain/ask:p3@30", "session_id": "01a10000-0000-7000-8000-000000000002", "status": "session_missing"},
		{"case_id": "phrase/comment:f4@40", "status": "no_writer_session"},
		{"case_id": "question/comment:h5@51", "session_id": fixtureWriter, "status": "found", "rows": 2},
	}
	var b strings.Builder
	for _, p := range places {
		line, _ := json.Marshal(p)
		b.Write(append(line, '\n'))
	}
	if err := os.WriteFile(filepath.Join(sessions, "places.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(sessions, "sessions-counts.json"), map[string]any{"sha256": map[string]string{"session-rows.jsonl.gz": "rows-1", "places.jsonl": "places-1"}})
	return build, sessions
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The markers of the prompts' own text, by which a stand-in's rule tells the calls apart.
const (
	markReader     = "You are a fresh reader."
	markAnswered   = "Decide whether the answer of the fresh reader answers the question of the reader."
	markPhrases    = "Find each phrase that states no fact"
	markUndefined  = "Find each word or name of the text that the reader would have to ask about"
	markStart      = "Find the first sentence of the text that answers the question."
	markHarder     = "Decide whether the new text is harder to read"
	markDefinition = "Decide whether the definition answers the question of the reader."
)

// graderRules answer every call of change 1 the way a grader that finds one phrase in the phrase
// case and nothing else would.
func graderRules(t *testing.T) []modeltest.Rule {
	return []modeltest.Rule{
		{Contains: markReader, Reply: modeltest.Reply{Text: "The text says what the reader asked."}},
		{Contains: markAnswered, Reply: modeltest.Reply{Text: asJSON(t, map[string]any{
			"1_asked": "The meaning of the words.", "2_given": "The meaning.", "3_restates_question": false, "4_points_elsewhere": false,
			"5_other_question": false, "6_contradicts_record": false, "7_not_plain": false, "8_reason": "It answers.", "9_answers": true})}},
		{Match: func(r modeltest.Request) bool {
			return strings.Contains(r.Text, markPhrases) && strings.Contains(r.Text, "This names the problem.")
		}, Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"candidates": []map[string]string{{"1_id": "s1", "2_reason": "It names no problem.", "3_rewrite": "This is a problem [the problem is not in the text].", "4_verdict": "vacuous"}}})}},
		{Contains: markPhrases, Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"candidates": []any{}})}},
		{Contains: markUndefined, Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"undefined": []any{}})}},
		{Contains: markStart, Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"2_found": true, "1_quote": "The blocker is the release of engine one."})}},
		{Contains: markHarder, Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"1_reason": "The texts are the same.", "2_harder": false})}},
	}
}

// keyFile is a gateway key file for the stand-in, which reads none.
func keyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("stand-in\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runHarness runs `harness` with args and answers its status and its output.
func runHarness(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func readFigures(t *testing.T, out string) Figures {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, "scores.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f Figures
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestScoreChangeOneOverTheDevelopmentPart(t *testing.T) {
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	out := t.TempDir()
	code, stdout, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", out,
		"--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "200", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log"))
	if code != 0 {
		t.Fatalf("score = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	results, err := readResults(filepath.Join(out, "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d, want the 4 development cases", len(results))
	}
	if _, ok := results["question/comment:h5@51"]; ok {
		t.Error("a development run scored a held-out case")
	}
	q := results["question/comment:q1@11"]
	if q.Reader == nil || q.Grades[JAnswered] == nil || q.Grades[JAnswered].Verdict == nil || !*q.Grades[JAnswered].Verdict {
		t.Errorf("the question case's answered judgement: %+v", q.Grades[JAnswered])
	}
	if !q.Session || q.Grades[JAnswered].Session != (FinalInputs[JAnswered] == InputsFull) {
		t.Errorf("the question case has a session, and its answered judgement read it: %v, under final inputs %s", q.Grades[JAnswered].Session, FinalInputs.String())
	}
	for _, j := range []Judgement{JLost, JAdded} {
		if g := q.Grades[j]; g == nil || !g.ByConstruction || g.Call != nil {
			t.Errorf("change 1's %s judgement is by construction, with no call: %+v", j, g)
		}
	}
	a := results["answer/comment:a2@20"]
	if g := a.Grades[JAnswerStart]; g == nil || g.WordsBefore == nil || *g.WordsBefore != 6 {
		t.Errorf("the answer case's words before the answer: %+v, want 6", g)
	}
	if a.Words != 14 || a.FirstParagraphReader == nil || a.Grades[JFirstParagraph] == nil {
		t.Errorf("the answer case: words %d, first paragraph reader %+v", a.Words, a.FirstParagraphReader)
	}
	if p := results["plain/ask:p3@30"]; p.Session || p.Grades[JHarder] == nil || p.Reader != nil {
		t.Errorf("the plain case reads no session and no reader, and takes harder: %+v", p)
	}
	if f := results["phrase/comment:f4@40"]; f.Grades[JPhrases] == nil || f.Grades[JPhrases].Count != 1 {
		t.Errorf("the phrase case's phrases: %+v", f.Grades[JPhrases])
	}

	// Session text reaches the grader alone, and only the part before the case's place.
	for _, r := range stand.Requests() {
		holds := strings.Contains(r.Text, "<writer_session>")
		if holds && r.Model != "claude-opus-5" {
			t.Errorf("model %s got session text", r.Model)
		}
		if strings.Contains(r.Text, fixtureThinking) || strings.Contains(r.Text, "A row after every case's place.") {
			t.Errorf("a call read a session row it must not: %s", r.Model)
		}
		if strings.Contains(r.Text, markReader) && (holds || strings.Contains(r.Text, fixtureSessionFx)) {
			t.Error("the fresh reader read session text")
		}
	}

	f := readFigures(t, out)
	whole := f.Populations[0]
	if whole.Name != PopulationWhole || whole.All.Cases != 4 {
		t.Fatalf("the whole part: %+v", whole)
	}
	ans := whole.Groups[GroupQuestion].Answered
	if ans == nil || ans.Hits != 1 || ans.CI95 == nil || ans.CI95.Method != MethodExact {
		t.Errorf("the question group's answered share: %+v", ans)
	}
	if lost := whole.All.Lost; lost == nil || lost.WithAny.CI95.Method != MethodByConstruction {
		t.Errorf("facts lost under change 1: %+v", lost)
	}
	if w := whole.All.Wait; w == nil || *w.Mean != 0 {
		t.Errorf("change 1's wait: %+v", w)
	}
	if ws := f.Populations[1]; ws.Name != PopulationWithSession || ws.All.Cases != 2 {
		t.Errorf("the cases with a session: %+v", ws.All)
	}
	if len(f.Models) == 0 || f.Models[0].CostUSD <= 0 {
		t.Errorf("the model use is priced: %+v", f.Models)
	}
}

func TestScoreContinuesTheCasesWithNoResult(t *testing.T) {
	build, sessions := writeFixture(t)
	out := t.TempDir()
	key := keyFile(t)
	exposure := filepath.Join(t.TempDir(), "exposure.log")
	// The first run's grader answers the plain case's harder judgement with no JSON object.
	broken := append([]modeltest.Rule{{Match: func(r modeltest.Request) bool {
		return strings.Contains(r.Text, markHarder)
	}, Reply: modeltest.Reply{Text: "not an object"}}}, graderRules(t)...)
	first := modeltest.New(t, broken...)
	code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", out, "--gateway", first.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure)
	if code != 1 || !strings.Contains(stderr, "1 of 4 cases have no result") {
		t.Fatalf("the first run = %d, want 1 with one case unscored:\n%s", code, stderr)
	}
	second := modeltest.New(t, graderRules(t)...)
	code, _, stderr = runHarness("score", "--cases", build, "--sessions", sessions, "--out", out, "--gateway", second.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure)
	if code != 0 {
		t.Fatalf("the second run = %d:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "4 cases, 3 with a result, 1 to score") {
		t.Errorf("the second run did not continue from the first's results:\n%s", stderr)
	}
	for _, r := range second.Requests() {
		if !strings.Contains(r.Text, "Merge pull request 12?") {
			t.Errorf("the second run called a model for a case the first run scored: %.200s", r.Text)
		}
	}
	if n := readFigures(t, out).Populations[0].Groups[GroupPlain].Cases; n != 1 {
		t.Errorf("the plain case has no result after the second run: %d", n)
	}
	// A run of other parameters in the same directory is refused.
	code, _, stderr = runHarness("score", "--cases", build, "--sessions", sessions, "--out", out, "--gateway", second.URL, "--gateway-key-file", key, "--groups", "plain", "--exposure-log", exposure)
	if code != 1 || !strings.Contains(stderr, "holds a run of other parameters") {
		t.Errorf("a run of other parameters in the same directory = %d:\n%s", code, stderr)
	}
}

// A paced run records its pace in run.json, and scores.md gives the uncached input of the run's
// own calls and its busiest minute. A run in the same directory at another pace is refused.
func TestAPacedRunRecordsItsPaceAndItsUncachedInput(t *testing.T) {
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	out := t.TempDir()
	key := keyFile(t)
	exposure := filepath.Join(t.TempDir(), "exposure.log")
	code, stdout, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", out, "--uncached-input-per-minute", "2000000",
		"--gateway", stand.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure)
	if code != 0 {
		t.Fatalf("a paced score = %d:\n%s", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(out, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var params RunParams
	if err := json.Unmarshal(data, &params); err != nil || params.UncachedInputPerMinute != 2000000 {
		t.Errorf("run.json records the pace %d (%v), want 2000000", params.UncachedInputPerMinute, err)
	}
	f := readFigures(t, out)
	// The stand-in reports no cache tokens, so each call's uncached input is its input.
	input := 0
	for _, u := range f.Models {
		input += u.Input + u.CacheWrite
	}
	if u := f.Run.UncachedInput; input == 0 || u.Tokens != input || u.BusiestMinute <= 0 || u.BusiestMinute > u.Tokens {
		t.Errorf("the run's uncached input is %+v, want the %d tokens its calls reported, and a busiest minute of at most that", u, input)
	}
	if want := "the busiest minute held " + strconv.Itoa(f.Run.UncachedInput.BusiestMinute) + "; paced to at most 2000000 a minute."; !strings.Contains(stdout, want) {
		t.Errorf("scores.md does not say %q:\n%s", want, stdout)
	}
	code, _, stderr = runHarness("score", "--cases", build, "--sessions", sessions, "--out", out,
		"--gateway", stand.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure)
	if code != 1 || !strings.Contains(stderr, "holds a run of other parameters") {
		t.Errorf("a run at no pace in the paced run's directory = %d:\n%s", code, stderr)
	}
}

func TestAHeldOutRunWritesTheExposureLogAndIsRefusedAtTheSameCommit(t *testing.T) {
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	key := keyFile(t)
	exposure := filepath.Join(t.TempDir(), "exposure.log")
	score := func(out string, extra ...string) (int, string) {
		args := append([]string{"score", "--part", "held_out", "--cases", build, "--sessions", sessions, "--out", out,
			"--gateway", stand.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure}, extra...)
		code, _, stderr := runHarness(args...)
		return code, stderr
	}
	// A held-out run that one of its own refusals stops before any model call, here no gateway,
	// read no text of the part, so it writes no line and the next run at its commit is no second.
	if code, stderr := score(t.TempDir(), "--commit", "c0ffee", "--gateway", ""); code != 2 || !strings.Contains(stderr, "no model gateway") || len(stand.Requests()) != 0 {
		t.Fatalf("a held-out run with no gateway = %d after %d calls:\n%s", code, len(stand.Requests()), stderr)
	}
	if logged, err := ReadExposures(exposure); err != nil || len(logged) != 0 {
		t.Fatalf("the held-out run refused before any call wrote the exposure log: %+v (%v)", logged, err)
	}
	if code, stderr := score(t.TempDir(), "--commit", "c0ffee"); code != 0 {
		t.Fatalf("the first held-out run = %d:\n%s", code, stderr)
	}
	calls := len(stand.Requests())
	code, stderr := score(t.TempDir(), "--commit", "c0ffee")
	if code != 1 || !strings.Contains(stderr, "already holds a run of the held-out part at commit c0ffee") {
		t.Fatalf("a second held-out run at one commit = %d, want the refusal:\n%s", code, stderr)
	}
	if len(stand.Requests()) != calls {
		t.Error("the refused run called a model")
	}
	if code, stderr := score(t.TempDir(), "--commit", "c0ffee", "--new-run", "the prompts changed after a forward merge"); code != 0 {
		t.Fatalf("a second held-out run with a reason = %d:\n%s", code, stderr)
	}
	logged, err := ReadExposures(exposure)
	if err != nil {
		t.Fatal(err)
	}
	if len(logged) != 2 || logged[0].Commit != "c0ffee" || logged[1].Reason == "" || logged[0].Changes[0] != "1" || logged[0].Models[RoleGrader] != "claude-opus-5" {
		t.Errorf("the exposure log: %+v", logged)
	}
	// A development run writes no line.
	code, _, stderr = runHarness("score", "--cases", build, "--sessions", sessions, "--out", t.TempDir(), "--gateway", stand.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure)
	if code != 0 {
		t.Fatalf("a development run = %d:\n%s", code, stderr)
	}
	if logged, _ := ReadExposures(exposure); len(logged) != 2 {
		t.Errorf("a development run wrote the exposure log: %d lines", len(logged))
	}
}

// The log holds one run of each cell at a commit: a second run of a cell needs a reason, a cell
// of its own at the same commit does not, and a run in the directory of the run of its very cells
// continues that run while it has a case left.
func TestClaimHeldOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness", "exposure.log")
	claim := func(e Exposure, finished bool) (Exposure, error) { return ClaimHeldOut(path, e, finished) }
	if _, err := claim(Exposure{}, false); err == nil {
		t.Error("a held-out run with no commit was let through")
	}
	one := Exposure{Commit: "a", Time: "t1", Changes: []string{"1"}, Out: "/runs/1"}
	if _, err := claim(one, false); err != nil {
		t.Fatal(err)
	}
	three := Exposure{Commit: "a", Time: "t2", Changes: []string{"3"}, Models: map[string]string{RoleCandidate: "m1,m2"}, Out: "/runs/3"}
	if _, err := claim(three, false); err != nil {
		t.Errorf("change 3 at the commit of change 1's run, a cell of its own: %v", err)
	}
	again := one
	again.Out = "/runs/1b"
	if _, err := claim(again, false); err == nil {
		t.Error("a second run of change 1 at commit a was let through")
	}
	other := three
	other.Models, other.Out = map[string]string{RoleCandidate: "m2"}, "/runs/3b"
	if _, err := claim(other, false); err == nil {
		t.Error("a second run of change 3 with m2 at commit a was let through")
	}
	if _, err := claim(Exposure{Commit: "b", Changes: []string{"1"}}, false); err != nil {
		t.Errorf("a run at another commit: %v", err)
	}
	if _, err := claim(Exposure{Commit: "a", Changes: []string{"1"}, Reason: "a new candidate answered"}, false); err != nil {
		t.Errorf("a second run at commit a with a reason: %v", err)
	}
	more := three
	more.Time = "t3"
	got, err := claim(more, false)
	if err != nil || got.Continues != "t2" {
		t.Errorf("a run in the directory of change 3's run, of its cells, with cases left = %+v, %v; want a continuation of the run begun t2", got, err)
	}
	more.Time = "t4"
	if got, err := claim(more, false); err != nil || got.Continues != "t2" {
		t.Errorf("a continuation of a continuation = %+v, %v; want the run begun t2", got, err)
	}
	if _, err := claim(more, true); err == nil || !strings.Contains(err.Error(), "nothing to continue") {
		t.Errorf("a continuation of a run with no case left = %v, want a refusal", err)
	}
	fewer := three
	fewer.Models = map[string]string{RoleCandidate: "m1"}
	if _, err := claim(fewer, false); err == nil {
		t.Error("a run in the directory of change 3's run, of other cells, was let through")
	}
	logged, err := ReadExposures(path)
	if err != nil || len(logged) != 6 {
		t.Errorf("the log holds %d lines (%v), want 6", len(logged), err)
	}
}

func TestAScoreOfASampleRecordsTheSample(t *testing.T) {
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	out := t.TempDir()
	code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", out, "--sample", "2", "--sample-seed", "5",
		"--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log"))
	if code != 0 {
		t.Fatalf("score --sample = %d:\n%s", code, stderr)
	}
	f := readFigures(t, out)
	s := f.Run.Params.Sample
	// Each of the fixture's four strata holds one case, and a sample takes at least one case of
	// each stratum.
	if s == nil || s.Size != 2 || s.Seed != 5 || s.Cases != f.Run.Selected || len(s.Strata) != 4 || s.Strata[0].Drawn != 1 {
		t.Fatalf("the sample the run records: %+v (selected %d)", s, f.Run.Selected)
	}
	// Another sample in the same directory is another run.
	code, _, stderr = runHarness("score", "--cases", build, "--sessions", sessions, "--out", out, "--sample", "3",
		"--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log"))
	if code != 1 || !strings.Contains(stderr, "holds a run of other parameters") {
		t.Errorf("another sample in the same directory = %d:\n%s", code, stderr)
	}
}

func TestAnOutputInsideARepositoryIsRefused(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".jj"), 0o755); err != nil {
		t.Fatal(err)
	}
	build, sessions := writeFixture(t)
	inside := filepath.Join(repo, "packages", "grader-test-5")
	code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", inside)
	if code != 2 || !strings.Contains(stderr, "is inside the repository") {
		t.Errorf("score --out inside a repository = %d:\n%s", code, stderr)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Errorf("the refused run made its directory: %v", err)
	}
	if err := OutsideRepository("--out", filepath.Join(t.TempDir(), "run")); err != nil {
		t.Errorf("a directory outside a repository: %v", err)
	}
}
