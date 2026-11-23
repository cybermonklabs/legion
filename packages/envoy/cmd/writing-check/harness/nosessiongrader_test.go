package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/claude"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// foreignGrader is the Foundry deployment of prices.yaml that the tests grade with.
const foreignGrader = "grok-4.7"

// stubFoundryTokens signs the run's Foundry clients in with a stand-in token, which reaches no
// Entra tenant.
func stubFoundryTokens(t *testing.T) {
	t.Helper()
	saved := foundryTokens
	foundryTokens = func(llm.FoundryConfig) (func(context.Context) (string, error), error) {
		return func(context.Context) (string, error) { return "stand-in-token", nil }, nil
	}
	t.Cleanup(func() { foundryTokens = saved })
}

// sessionMarks are words of the fixture's session part: its tag and the text of its rows.
var sessionMarks = []string{"<" + claude.SessionTag + ">", "Please build the zork train.", fixtureSessionFx, "The zork train is built."}

// chatTurn is the system prompt and the user message of an OpenAI Chat Completions request.
func chatTurn(t *testing.T, r modeltest.Request) (system, user string) {
	t.Helper()
	var wire struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(r.Body, &wire); err != nil {
		t.Fatalf("the request is no Chat Completions request: %v\n%s", err, r.Body)
	}
	text := func(raw json.RawMessage) string {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &parts); err != nil || len(parts) != 1 {
			t.Fatalf("a message's content is %s, want one text", raw)
		}
		return parts[0].Text
	}
	for _, m := range wire.Messages {
		switch m.Role {
		case "system", "developer":
			system = text(m.Content)
		case "user":
			user = text(m.Content)
		}
	}
	return system, user
}

// `score --grader <Foundry deployment> --grader-no-session --readers-from <run>` grades the cases
// of that run again with the deployment alone: no reader and no Claude call, and no request holds
// any of the session part, which the Claude grader of the run it grades again read. Each request
// is the Claude run's with the session part cut; the grades read no session, their digests are
// not the Claude grades', and run.json and scores.md record grader_family foundry and
// session_part none.
func TestAFoundryGraderWithReadersFromSendsNoSessionPart(t *testing.T) {
	stubFoundryTokens(t)
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	key, exposure := keyFile(t), filepath.Join(t.TempDir(), "exposure.log")
	claudeRun := t.TempDir()
	code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", claudeRun, "--grader-inputs", "full",
		"--gateway", stand.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure)
	if code != 0 {
		t.Fatalf("the Claude run = %d:\n%s", code, stderr)
	}
	before := stand.Requests()
	claudeAnswered := request(t, stand, markAnswered, goldenQuestion)
	if !bytes.Contains(claudeAnswered.Body, []byte(fixtureSessionFx)) {
		t.Fatal("the Claude grader's answered call read no session part, so the fixture tests nothing")
	}

	out := filepath.Join(t.TempDir(), "grok")
	code, _, stderr = runHarness("score", "--cases", build, "--out", out, "--grader-inputs", "full", "--readers-from", claudeRun,
		"--grader", foreignGrader, "--grader-no-session", "--foundry-endpoint", stand.URL+"/models", "--azure-tenant", "tenant",
		"--azure-client-id", "client", "--gateway", stand.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure)
	if code != 0 {
		t.Fatalf("the Foundry grader's run = %d:\n%s", code, stderr)
	}
	after := stand.Requests()[len(before):]
	if len(after) == 0 {
		t.Fatal("the Foundry grader's run made no call")
	}
	var answered *modeltest.Request
	for i, r := range after {
		if r.Protocol != modeltest.Chat || r.Model != foreignGrader || strings.Contains(r.Text, markReader) {
			t.Errorf("a call of the run went to %s over %s (the reader's: %t), want grading calls of %s alone", r.Model, r.Protocol, strings.Contains(r.Text, markReader), foreignGrader)
		}
		for _, m := range sessionMarks {
			if bytes.Contains(r.Body, []byte(m)) {
				t.Errorf("a request to %s holds %q of the session part:\n%s", r.Model, m, r.Body)
			}
		}
		if strings.Contains(r.Text, markAnswered) && strings.Contains(r.Text, goldenQuestion) {
			answered = &after[i]
		}
	}
	if answered == nil {
		t.Fatal("the Foundry grader made no answered call of the question case")
	}
	system, user := chatTurn(t, *answered)
	claudeTurn := decodeTurn(t, claudeAnswered)
	if want := strings.TrimRight(user, "\n") + "\n\n<" + claude.SessionTag + ">\n"; !strings.HasPrefix(claudeTurn.user(t), want) {
		t.Errorf("the answered user message is\n%s\nwant the Claude run's\n%s\nwith its session part cut", user, claudeTurn.user(t))
	}
	if !strings.Contains(system, markAnswered) || strings.Contains(system, "the session") {
		t.Errorf("the answered system prompt is\n%s\nwant grade-answered's for a case with no session part", system)
	}

	params := readRunParams(t, out)
	if params.GraderFamily != llm.FamilyFoundry || params.SessionPart != sessionPartNone || params.Models[RoleGrader] != foreignGrader ||
		params.Models[RoleGraderFallback] != "" || params.ReadersFrom != claudeRun || params.SessionsSHA != "" || len(params.SkippedJudgements) != 0 {
		t.Errorf("run.json records %+v", params)
	}
	md, err := os.ReadFile(filepath.Join(out, "scores.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "grader_family: foundry, session_part: none") {
		t.Errorf("scores.md records no grader_family and session_part:\n%s", md)
	}

	claudeResults := readLines(t, claudeRun)
	results := readLines(t, out)
	if len(results) != len(claudeResults) {
		t.Fatalf("the run graded %d cases, want the %d of --readers-from", len(results), len(claudeResults))
	}
	graded := 0
	for id, r := range results {
		if r.Error != "" || r.Session || r.SessionStatus != "not_read" {
			t.Errorf("%s: error %q, session %t, session status %q, want no error and no session read", id, r.Error, r.Session, r.SessionStatus)
		}
		if r.Reader != nil && !r.Reader.Reused {
			t.Errorf("%s: the reader's answer is not the one of --readers-from", id)
		}
		for j, g := range r.Grades {
			if g.ByConstruction || (j == JPhrases && g.Candidates == 0) {
				continue
			}
			graded++
			if g.Session || g.Call == nil || g.Call.Model != foreignGrader {
				t.Errorf("%s %s: %+v, want a grade of %s that read no session part", id, j, g, foreignGrader)
			}
			if c := claudeResults[id].Grades[j]; c != nil && c.InputSHA256 == g.InputSHA256 {
				t.Errorf("%s %s: the grade's input_sha256 is the Claude grade's", id, j)
			}
		}
	}
	if graded == 0 {
		t.Error("the run gave no grade")
	}

	// harness compare reads the agreement of the two graders.
	code, stdout, stderr := runHarness("compare", "--a", claudeRun, "--b", out, "--out", filepath.Join(t.TempDir(), "agreement.json"), "--draws", "50")
	if code != 0 || !strings.Contains(stdout, "| answered |") {
		t.Errorf("compare = %d:\n%s\n%s", code, stdout, stderr)
	}
}

// A grader of another family grades the cases of the run --readers-from names, and --sample draws
// its sample from them, so a sample of a sample run is graded again: the cases are all of that
// run, and compare reads their agreement.
func TestAFoundryGraderSamplesTheCasesOfReadersFrom(t *testing.T) {
	stubFoundryTokens(t)
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	key, exposure := keyFile(t), filepath.Join(t.TempDir(), "exposure.log")
	claudeRun := t.TempDir()
	code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", claudeRun, "--groups", "question,answer",
		"--gateway", stand.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure)
	if code != 0 {
		t.Fatalf("the Claude run = %d:\n%s", code, stderr)
	}
	out := filepath.Join(t.TempDir(), "grok")
	code, _, stderr = runHarness("score", "--cases", build, "--sessions", sessions, "--out", out, "--readers-from", claudeRun,
		"--sample", "1", "--sample-seed", "4232", "--grader", foreignGrader, "--grader-no-session", "--foundry-endpoint", stand.URL+"/models",
		"--azure-tenant", "tenant", "--azure-client-id", "client", "--gateway", stand.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure)
	if code != 0 {
		t.Fatalf("the Foundry grader's run = %d:\n%s", code, stderr)
	}
	results := readLines(t, out)
	claudeResults := readLines(t, claudeRun)
	if len(results) == 0 || len(results) > len(claudeResults) {
		t.Fatalf("the sample holds %d cases", len(results))
	}
	for id := range results {
		if _, ok := claudeResults[id]; !ok {
			t.Errorf("the sample holds %s, which --readers-from does not", id)
		}
	}
	f := readFigures(t, out)
	if s := f.Run.Params.Sample; s == nil || s.Seed != 4232 || s.Cases != len(results) {
		t.Errorf("run.json records the sample %+v", s)
	}
	code, stdout, stderr := runHarness("compare", "--a", claudeRun, "--b", out, "--out", filepath.Join(t.TempDir(), "agreement.json"), "--draws", "50")
	if code != 0 || !strings.Contains(stdout, "# Agreement of two runs on "+strconv.Itoa(len(results))+" cases") {
		t.Errorf("compare = %d:\n%s\n%s", code, stdout, stderr)
	}
}

// Under --texts-from, a grader of another family does not grade added for the texts of a change
// that plays the writer, whose session part is an input of the new text: run.json and scores.md
// say so, and no added call is made. It grades added for a change that plays no writer (4, the
// reviewer's rewrite from the record alone). It refuses the texts of a change whose candidate is
// the grader (R2).
func TestAFoundryGraderSkipsAddedOfAChangeThatPlaysTheWriter(t *testing.T) {
	const question = "question/comment:q1@11"
	stubFoundryTokens(t)
	build, sessions := writeFixture(t)
	key, exposure := keyFile(t), filepath.Join(t.TempDir(), "exposure.log")
	stand := modeltest.New(t, allJudgementRules(t)...)
	// A run of change 1 gives the lines; the test makes them a run of change, by candidate, that
	// wrote the question case's text again.
	textsOf := func(change, candidate string) string {
		t.Helper()
		src := t.TempDir()
		if code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", src, "--gateway", stand.URL, "--gateway-key-file", key,
			"--draws", "50", "--exposure-log", exposure); code != 0 {
			t.Fatalf("change 1 = %d:\n%s", code, stderr)
		}
		rewriteResults(t, src, func(r *CaseResult) {
			if r.CaseID == question {
				r.Text = "The zork train, the queue of pull requests, is ready."
				r.TextSHA256 = sha256Hex([]byte(r.Text))
			}
			r.Change = change
		})
		p := readRunParams(t, src)
		p.Change, p.Model, p.Models[RoleCandidate] = change, candidate, candidate
		if err := writeJSON(filepath.Join(src, "run.json"), p); err != nil {
			t.Fatal(err)
		}
		return src
	}
	regrade := func(src string) (string, int, string, []modeltest.Request) {
		t.Helper()
		before := len(stand.Requests())
		out := filepath.Join(t.TempDir(), "grok")
		code, _, stderr := runHarness("score", "--cases", build, "--out", out, "--texts-from", src, "--readers-from", src,
			"--grader", foreignGrader, "--grader-no-session", "--foundry-endpoint", stand.URL+"/models", "--azure-tenant", "tenant",
			"--azure-client-id", "client", "--gateway", stand.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure)
		return out, code, stderr, stand.Requests()[before:]
	}

	out, code, stderr, calls := regrade(textsOf(changeRewrite, laneA))
	if code != 0 {
		t.Fatalf("the regrade of change 5 = %d:\n%s", code, stderr)
	}
	if p := readRunParams(t, out); p.SkippedJudgements[JAdded] == "" || len(p.SkippedJudgements) != 1 {
		t.Errorf("run.json records the skipped judgements %v, want added alone", p.SkippedJudgements)
	}
	if md, err := os.ReadFile(filepath.Join(out, "scores.md")); err != nil || !strings.Contains(string(md), "Not graded: added, since change 5 plays the writer") {
		t.Errorf("scores.md does not say added is not graded (%v):\n%s", err, md)
	}
	if r := readLines(t, out)[question]; r.Error != "" || r.Grades[JAdded] != nil || r.Grades[JLost] == nil || r.Grades[JLost].Call == nil {
		t.Errorf("the rewritten case's line: error %q, added %+v, lost %+v; want lost graded and no added grade", r.Error, r.Grades[JAdded], r.Grades[JLost])
	}
	for _, r := range calls {
		if strings.Contains(r.Text, markAdded) {
			t.Error("the run made an added call")
		}
	}

	out, code, stderr, calls = regrade(textsOf(changeReviewRewrite, laneA))
	if code != 0 {
		t.Fatalf("the regrade of change 4 = %d:\n%s", code, stderr)
	}
	if p := readRunParams(t, out); len(p.SkippedJudgements) != 0 {
		t.Errorf("run.json records the skipped judgements %v for change 4, want none", p.SkippedJudgements)
	}
	if g := readLines(t, out)[question].Grades[JAdded]; g == nil || g.Call == nil || g.Call.Model != foreignGrader || g.Session {
		t.Errorf("change 4's added grade of the rewritten case: %+v", g)
	}
	if !slices.ContainsFunc(calls, func(r modeltest.Request) bool { return strings.Contains(r.Text, markAdded) }) {
		t.Error("the run of change 4 made no added call")
	}

	_, code, stderr, calls = regrade(textsOf(changeReviewRewrite, foreignGrader))
	if code != 2 || !strings.Contains(stderr, "a grader grades no output it wrote") || len(calls) != 0 {
		t.Errorf("the regrade of texts %s wrote = %d with %d calls, want 2 saying it wrote them:\n%s", foreignGrader, code, len(calls), stderr)
	}
}

// A grader of another family is refused before any model call and before the run makes its
// directory: without --grader-no-session, without --readers-from, for a change that the run
// would make, for the held-out part, below its least time bound; and --grader-no-session with a
// Claude grader.
func TestAFoundryGraderIsRefusedWhereItsRunWouldCallAnotherModel(t *testing.T) {
	stubFoundryTokens(t)
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, graderRules(t)...)
	readers := t.TempDir()
	if err := os.WriteFile(filepath.Join(readers, "cases.jsonl"), []byte(`{"case_id":"question/comment:q1@11"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	foundry := []string{"--foundry-endpoint", stand.URL + "/models", "--azure-tenant", "tenant", "--azure-client-id", "client"}
	for _, c := range []struct {
		name string
		args []string
		says string
	}{
		{"no --grader-no-session", []string{"--grader", foreignGrader, "--readers-from", readers}, "give --grader-no-session"},
		{"no --readers-from", []string{"--grader", foreignGrader, "--grader-no-session"}, "give --readers-from"},
		{"a change it would make", []string{"--grader", foreignGrader, "--grader-no-session", "--readers-from", readers, "--change", "2", "--local", "x", "--loaded", "y"}, "give --texts-from"},
		{"the held-out part", []string{"--grader", foreignGrader, "--grader-no-session", "--readers-from", readers, "--part", PartHeldOut}, "development part alone"},
		{"a short time bound", []string{"--grader", foreignGrader, "--grader-no-session", "--readers-from", readers, "--grader-timeout", "120s"}, "at least 10m"},
		{"a Claude grader", []string{"--grader", "claude-fable-5-2", "--grader-no-session", "--readers-from", readers}, "a Claude grader reads the session part"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "run")
			args := append([]string{"score", "--cases", build, "--sessions", sessions, "--out", out, "--gateway", stand.URL,
				"--gateway-key-file", keyFile(t), "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log")}, foundry...)
			code, _, stderr := runHarness(append(args, c.args...)...)
			if code != 2 || !strings.Contains(stderr, c.says) {
				t.Errorf("score %v = %d, want 2 saying %q:\n%s", c.args, code, c.says, stderr)
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

// A grader of another family refuses a request that carries a session part, and calls no model.
// No request it builds holds one (context), so the refusal is R2's guard against code that
// would build one: session text never reaches a Foundry deployment. The same request with no
// session part reaches the deployment, so the stand-in would have seen a call.
func TestAFoundryGraderRefusesARequestWithASessionPart(t *testing.T) {
	stand := modeltest.New(t, modeltest.Rule{Model: foreignGrader, Reply: modeltest.Reply{Text: "graded"}})
	tokens := func(context.Context) (string, error) { return "stand-in-token", nil }
	g := NewNoSessionGrader(llm.NewFoundry(llm.FoundryConfig{Endpoint: stand.URL + "/models"}, tokens, foreignGrader, "", agentloop.Price{}, &llm.Runner{}), FinalInputs)
	session := changeSessions(t)["question/ask:k1@11"]
	r := gradeRequest{req: llm.Request{Purpose: "grade:" + string(JAnswered), System: "Grade the answer.", User: tag("question", goldenQuestion)}, session: session}
	if _, _, err := g.call(t.Context(), r); err == nil || !strings.Contains(err.Error(), "a grader of another family reads no session part") {
		t.Errorf("the call with a session part answered %v, want the refusal", err)
	}
	if n := len(stand.Requests()); n != 0 {
		t.Fatalf("the refused call reached the deployment with %d requests, want none", n)
	}
	r.session = claude.SessionText{}
	if _, _, err := g.call(t.Context(), r); err != nil {
		t.Fatalf("the call with no session part: %v", err)
	}
	if n := len(stand.Requests()); n != 1 {
		t.Errorf("the call with no session part made %d requests, want 1", n)
	}
}
