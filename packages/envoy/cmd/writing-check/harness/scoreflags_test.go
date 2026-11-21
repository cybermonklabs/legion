package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// The case ids of the fixture that the tests below name.
const (
	questionCase = "question/comment:q1@11"
	answerCase   = "answer/comment:a2@20"
)

// The input_sha256 of the fresh reader's answers on the fixture with no --reader and no
// --reader-vocabulary, as the harness sent them before those flags: the question case's, and the
// answer case's reader and first-paragraph reader. The same inputs give the same digests, so a run
// of --grades-from still takes the answers of a run made before the flags.
const (
	questionReaderSHA256       = "1830ff7eb2f6ef532b8c154a0b3b82e0dfecf10be72e8d12cd2a0a89f43ea7a5"
	answerReaderSHA256         = "ad936d2a1475d240d6308df2eb1d64a5193a26c17e3b3ffa8bf2e6138f6ab366"
	answerFirstParagraphSHA256 = "4fa449f4bfd63e3640f87bc2d891ce490053b6a8b63abbbfb787eee681004d33"
)

// writeCaseList writes a --case-list file of content and answers its path.
func writeCaseList(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cases.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// scoreArgs are a score run's arguments on the fixture with the stand-in as the gateway.
func scoreArgs(t *testing.T, build, sessions, out string, stand *modeltest.Model, extra ...string) []string {
	t.Helper()
	return append([]string{"score", "--cases", build, "--sessions", sessions, "--out", out, "--gateway", stand.URL,
		"--gateway-key-file", keyFile(t), "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log")}, extra...)
}

// `score --reader claude-fable-5-2 --case-list <file>` scores the one case the file lists, with
// claude-fable-5-2 as the fresh reader: every reader's call goes to it, its answer records it, and
// no call reads another case. run.json records the reader and the list's SHA-256, and a run in the
// same directory with another list is refused.
func TestScoreWithAReaderAndACaseListScoresTheListedCaseWithThatReader(t *testing.T) {
	const reader = "claude-fable-5-2"
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	out := t.TempDir()
	list := writeCaseList(t, "# the question case alone\n\n"+questionCase+"\n")
	code, _, stderr := runHarness(scoreArgs(t, build, sessions, out, stand, "--reader", reader, "--case-list", list)...)
	if code != 0 {
		t.Fatalf("score = %d:\n%s", code, stderr)
	}
	results, err := readResults(filepath.Join(out, "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	q, ok := results[questionCase]
	if len(results) != 1 || !ok {
		t.Fatalf("the run scored %d cases, want the listed %s alone", len(results), questionCase)
	}
	if q.Reader == nil || q.Reader.Call.Model != reader || q.Reader.InputSHA256 == questionReaderSHA256 {
		t.Errorf("the question case's reader answer is %+v, want %s's, whose inputs name it", q.Reader, reader)
	}
	readers := 0
	for _, r := range stand.Requests() {
		if strings.Contains(r.Text, markReader) {
			readers++
			if r.Model != reader {
				t.Errorf("a reader's call went to %s, want %s", r.Model, reader)
			}
		}
		for _, other := range []string{"The blocker is the release of engine one.", "Merge pull request 12?", "This names the problem.", "The flim gate is shut."} {
			if strings.Contains(r.Text, other) {
				t.Errorf("a call to %s read a case the list does not name: %q", r.Model, other)
			}
		}
	}
	if readers != 1 {
		t.Errorf("%d reader's calls, want 1", readers)
	}
	sum, err := fileSHA256(list)
	if err != nil {
		t.Fatal(err)
	}
	params := readRunParams(t, out)
	if params.CaseListSHA256 != sum || params.Models[RoleReader] != reader {
		t.Errorf("run.json records case_list_sha256 %q and the models %v, want %s and the reader %s", params.CaseListSHA256, params.Models, sum, reader)
	}
	if f := readFigures(t, out); f.Run.Selected != 1 {
		t.Errorf("the figures select %d cases, want 1", f.Run.Selected)
	}

	code, _, stderr = runHarness(scoreArgs(t, build, sessions, out, stand, "--reader", reader, "--case-list", list)...)
	if code != 0 || !strings.Contains(stderr, "1 cases, 1 with a result, 0 to score") {
		t.Errorf("the same list in the same directory = %d, want it continued with nothing to score:\n%s", code, stderr)
	}
	other := writeCaseList(t, questionCase+"\n"+answerCase+"\n")
	code, _, stderr = runHarness(scoreArgs(t, build, sessions, out, stand, "--reader", reader, "--case-list", other)...)
	if code != 1 || !strings.Contains(stderr, "holds a run of other parameters") {
		t.Errorf("another list in the same directory = %d, want 1:\n%s", code, stderr)
	}
}

// --case-list refuses, before any model call and before run.json, an id the run would not score,
// naming it: one the build does not hold, a held-out case in a run of the development part, and a
// case of a group --groups leaves out. It refuses a list that names one case twice or none, and
// --sample beside it.
func TestScoreCaseListRefusesACaseTheRunDoesNotScore(t *testing.T) {
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	for _, c := range []struct {
		name, list string
		extra      []string
		says       []string
	}{
		{"a typo", questionCase + "\nquestion/comment:q1@12\n", nil, []string{"question/comment:q1@12 (--cases holds no case of that id in the development part)"}},
		{"a held-out case", "question/comment:h5@51\n", nil, []string{"question/comment:h5@51", "development part"}},
		{"a group left out", questionCase + "\nplain/ask:p3@30\n", []string{"--groups", "question"}, []string{"plain/ask:p3@30 (its group plain is not one of --groups)"}},
		{"one case twice", questionCase + "\n" + questionCase + "\n", nil, []string{"names case " + questionCase + " twice"}},
		{"no case", "# none\n\n", nil, []string{"names no case"}},
		{"with --sample", questionCase + "\n", []string{"--sample", "2"}, []string{"give --case-list or --sample, not both"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "run")
			args := scoreArgs(t, build, sessions, out, stand, append([]string{"--case-list", writeCaseList(t, c.list)}, c.extra...)...)
			code, _, stderr := runHarness(args...)
			if code != 2 {
				t.Errorf("score = %d, want 2:\n%s", code, stderr)
			}
			for _, s := range c.says {
				if !strings.Contains(stderr, s) {
					t.Errorf("the refusal does not say %q:\n%s", s, stderr)
				}
			}
			if _, err := os.Stat(filepath.Join(out, "run.json")); !os.IsNotExist(err) {
				t.Errorf("a refused run wrote run.json (%v)", err)
			}
		})
	}
	if n := len(stand.Requests()); n != 0 {
		t.Errorf("the refused runs made %d model calls, want none", n)
	}
}

// `score --reader` refuses the grader and a fallback grader (R2), a model of another family and
// a model prices.yaml has no price for, and --readers-from beside it, whose answers no reader of
// this run gives. It calls no model.
func TestScoreReaderFlagRefusesAModelThatMayNotRead(t *testing.T) {
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	for _, c := range []struct {
		name  string
		extra []string
		says  string
	}{
		{"the grader", []string{"--reader", cfg.Models.Roles.Grader.ID}, "is the grader"},
		{"a fallback grader", []string{"--reader", cfg.Models.Roles.GraderFallbacks[0].ID}, "is a fallback grader"},
		{"a Foundry deployment", []string{"--reader", foreignGrader}, "has no Claude price"},
		{"a model with no price", []string{"--reader", "claude-unpriced-1"}, "has no Claude price"},
		{"the reader beside --readers-from", []string{"--reader", "claude-fable-5-2", "--readers-from", t.TempDir()}, "--readers-from takes the fresh reader's answers of another run"},
		{"own words beside --readers-from", []string{"--reader-vocabulary", filepath.Join(t.TempDir(), "owner-texts.jsonl.gz"), "--readers-from", t.TempDir()}, "--readers-from takes the fresh reader's answers of another run"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "run")
			code, _, stderr := runHarness(scoreArgs(t, build, sessions, out, stand, c.extra...)...)
			if code != 2 || !strings.Contains(stderr, c.says) {
				t.Errorf("score %v = %d, want 2 saying %q:\n%s", c.extra, code, c.says, stderr)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("a refused run made its output directory (%v)", err)
			}
		})
	}
	if n := len(stand.Requests()); n != 0 {
		t.Errorf("the refused runs made %d model calls, want none", n)
	}
}

// With --reader-vocabulary the fresh reader also reads, for each case, the words of the text it
// reads that the owner wrote himself before the case, in reader_own_words after the question, and
// its system prompt ends with prompts/reader-own-words.md's part. A word he first wrote at the
// case's time or after it is not among them. No grading call holds them. run.json records the
// file's SHA-256 and the prompt part's. Without the flag each reader's request is the one the
// harness sent before the flag: its input_sha256 is the pinned one.
func TestTheReaderReadsTheOwnersOwnWordsWithReaderVocabulary(t *testing.T) {
	build, sessions := writeFixture(t)
	var cases []any
	for _, c := range fixtureCases() {
		c.WriteTime = "2026-11-02T00:00:00Z"
		cases = append(cases, c)
	}
	writeGzipLines(t, filepath.Join(build, casesFile), cases...)
	owner := writeOwnerTexts(t,
		ownerText("2026-11-01T10:00:00Z", "The Zork train is late."),
		ownerText("2026-11-02T00:00:00Z", "What does \"merges\" mean?"),
		ownerText("2026-11-03T10:00:00Z", "Is it ready at noon?"),
	)
	score := func(extra ...string) (*modeltest.Model, string) {
		t.Helper()
		stand := modeltest.New(t, graderRules(t)...)
		out := t.TempDir()
		if code, _, stderr := runHarness(scoreArgs(t, build, sessions, out, stand, extra...)...); code != 0 {
			t.Fatalf("score %v = %d:\n%s", extra, code, stderr)
		}
		return stand, out
	}

	stand, out := score("--reader-vocabulary", owner)
	// "merges" he wrote at the case's own time, and "it", "ready", "at" and "noon" after it.
	question := decodeTurn(t, request(t, stand, markReader, "The zork train is ready."))
	if user := question.user(t); user != tag("text", goldenText)+tag("question", goldenQuestion)+"<reader_own_words>\n- the\n- zork\n- train\n- is\n</reader_own_words>\n" {
		t.Errorf("the question case's reader reads\n%s\nwant the text, the question, then the words he wrote before the case", user)
	}
	system := question.system(t)
	if !strings.Contains(system, "## The reader's own words\n\nThe user message also holds \"reader_own_words\": words of the text that the reader used himself") ||
		!strings.HasSuffix(system, "only when it says nothing about the thing that he asks about.\n") {
		t.Errorf("the question case's reader's system prompt does not end with the own-words part:\n%s", system)
	}
	first := decodeTurn(t, request(t, stand, markReader, "You see only this paragraph."))
	if user := first.user(t); !strings.HasSuffix(user, "</question>\n<reader_own_words>\n- is\n</reader_own_words>\n") {
		t.Errorf("the first-paragraph reader reads\n%s\nwant the words of the first paragraph he wrote before the case", user)
	}
	if system := first.system(t); !strings.Contains(system, "words of the paragraph that the reader used himself") {
		t.Errorf("the first-paragraph reader's own-words part names no paragraph:\n%s", system)
	}
	graded := 0
	for _, r := range stand.Requests() {
		if strings.Contains(r.Text, markReader) {
			continue
		}
		graded++
		if strings.Contains(r.Text, "reader_own_words") {
			t.Errorf("a grading call of %s holds the reader's own words:\n%s", r.Model, r.Text)
		}
	}
	if graded == 0 {
		t.Error("the run made no grading call")
	}
	sum, err := fileSHA256(owner)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := promptFiles.ReadFile(readerOwnWordsPrompt)
	if err != nil {
		t.Fatal(err)
	}
	params := readRunParams(t, out)
	if params.ReaderVocabularySHA256 != sum || params.ReaderOwnWordsPromptSHA256 != sha256Hex(prompt) || params.VocabularySHA256 != "" {
		t.Errorf("run.json records reader_vocabulary_sha256 %q, reader_own_words_prompt_sha256 %q and vocabulary_sha256 %q; want %s, %s and none",
			params.ReaderVocabularySHA256, params.ReaderOwnWordsPromptSHA256, params.VocabularySHA256, sum, sha256Hex(prompt))
	}

	stand, out = score()
	for _, r := range stand.Requests() {
		if strings.Contains(r.Text, "reader_own_words") || strings.Contains(r.Text, "The reader's own words") {
			t.Errorf("without --reader-vocabulary a call to %s holds the reader's own words:\n%s", r.Model, r.Text)
		}
	}
	results, err := readResults(filepath.Join(out, "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	q, a := results[questionCase], results[answerCase]
	for _, got := range []struct {
		name   string
		answer *Answer
		want   string
	}{
		{"the question case's reader", q.Reader, questionReaderSHA256},
		{"the answer case's reader", a.Reader, answerReaderSHA256},
		{"the answer case's first-paragraph reader", a.FirstParagraphReader, answerFirstParagraphSHA256},
	} {
		if got.answer == nil || got.answer.InputSHA256 != got.want {
			t.Errorf("without --reader-vocabulary %s's answer is %+v, want the input_sha256 of before the flag, %s", got.name, got.answer, got.want)
		}
	}
	if p := readRunParams(t, out); p.ReaderVocabularySHA256 != "" || p.ReaderOwnWordsPromptSHA256 != "" {
		t.Errorf("run.json of a run with no reader vocabulary records %+v", p)
	}
}

// `score --dry-run` checks the flags, selects the cases and writes run.json as the run does, prints
// the cases of each group and the model of each role, and calls no model. A run with the same
// flags then continues that run.json. A dry run of a Foundry grader signs in nowhere, and a dry
// run of the held-out part writes no line to the exposure log.
func TestScoreDryRunCallsNoModelAndARealRunContinuesIt(t *testing.T) {
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	out := t.TempDir()
	list := writeCaseList(t, questionCase+"\n"+answerCase+"\n")
	args := scoreArgs(t, build, sessions, out, stand, "--reader", "claude-fable-5-2", "--case-list", list)
	code, stdout, stderr := runHarness(append(args, "--dry-run")...)
	if code != 0 {
		t.Fatalf("score --dry-run = %d:\n%s", code, stderr)
	}
	if n := len(stand.Requests()); n != 0 {
		t.Errorf("the dry run made %d model calls, want none", n)
	}
	for _, want := range []string{"dry run, no model called: 2 cases (question 1, answer 1, plain 0, phrase 0)\n", "reader: claude-fable-5-2\n",
		"grader: claude-opus-5\n", "grader_fallback: claude-opus-4-7,claude-opus-4-6,claude-sonnet-4-5\n", "run.json: " + filepath.Join(out, "run.json") + "\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the dry run printed\n%s\nwant %q", stdout, want)
		}
	}
	for _, name := range []string{"cases.jsonl", "session-excerpts.jsonl.gz", "scores.json"} {
		if _, err := os.Stat(filepath.Join(out, name)); !os.IsNotExist(err) {
			t.Errorf("the dry run wrote %s (%v)", name, err)
		}
	}
	dry, err := os.ReadFile(filepath.Join(out, "run.json"))
	if err != nil {
		t.Fatal(err)
	}

	code, _, stderr = runHarness(args...)
	if code != 0 || !strings.Contains(stderr, "2 cases, 0 with a result, 2 to score") {
		t.Fatalf("the run after the dry run = %d, want it to score both cases:\n%s", code, stderr)
	}
	if after, err := os.ReadFile(filepath.Join(out, "run.json")); err != nil || string(after) != string(dry) {
		t.Errorf("the run changed the dry run's run.json (%v):\n%s\nwant\n%s", err, after, dry)
	}
	if results, err := readResults(filepath.Join(out, "cases.jsonl")); err != nil || len(results) != 2 {
		t.Errorf("the run scored %d cases (%v), want 2", len(results), err)
	}

	saved := foundryTokens
	foundryTokens = func(llm.FoundryConfig) (func(context.Context) (string, error), error) {
		t.Error("a dry run signed in to the Foundry account")
		return nil, errors.New("no sign-in in a dry run")
	}
	t.Cleanup(func() { foundryTokens = saved })
	calls := len(stand.Requests())
	foreign := filepath.Join(t.TempDir(), "grok")
	code, stdout, stderr = runHarness("score", "--cases", build, "--out", foreign, "--readers-from", out, "--grader", foreignGrader, "--grader-no-session",
		"--foundry-endpoint", stand.URL+"/models", "--azure-tenant", "tenant", "--azure-client-id", "client", "--gateway", stand.URL,
		"--gateway-key-file", keyFile(t), "--exposure-log", filepath.Join(t.TempDir(), "exposure.log"), "--dry-run")
	if code != 0 || !strings.Contains(stdout, "2 cases") || !strings.Contains(stdout, "grader: "+foreignGrader+"\n") {
		t.Errorf("the dry run of a Foundry grader = %d:\n%s\n%s", code, stdout, stderr)
	}
	if n := len(stand.Requests()) - calls; n != 0 {
		t.Errorf("the dry run of a Foundry grader made %d model calls, want none", n)
	}

	log := filepath.Join(t.TempDir(), "exposure.log")
	code, _, stderr = runHarness("score", "--cases", build, "--sessions", sessions, "--out", t.TempDir(), "--part", PartHeldOut,
		"--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--exposure-log", log, "--dry-run")
	if code != 0 || !strings.Contains(stderr, "part held_out: 1 cases") {
		t.Errorf("the dry run of the held-out part = %d:\n%s", code, stderr)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Errorf("the dry run of the held-out part wrote the exposure log (%v)", err)
	}
}
