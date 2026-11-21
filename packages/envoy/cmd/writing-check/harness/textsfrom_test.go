package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// readRunParams is the run.json of the run in dir.
func readRunParams(t *testing.T, dir string) RunParams {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p RunParams
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// readLines is the cases.jsonl of the run in dir.
func readLines(t *testing.T, dir string) map[string]CaseResult {
	t.Helper()
	results, err := readResults(filepath.Join(dir, "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return results
}

// score --texts-from grades the texts of an earlier run again and makes no call of the change: a
// lockstep's directory gives one run for each of its models, in <out>/<model>, and one model's
// directory a run in --out. Each line holds the earlier line's text, trace, wait and change calls;
// run.json names the change, the candidate, the writer and the run the texts are from, and scores.md
// names the run the texts are from and the added grader, old by default. With
// --grades-from on the same run, the old grader takes every grade and calls no model; verify takes
// the old added grade as its first call and makes the check alone; inputs-first calls lost and
// added alone, for the one text the change changed; and inputs-first-verify, with inputs-first's
// run among --grades-from, takes its lost grade whole and its added grade as its first call, and
// makes the check alone. A flag that decides the texts is refused.
func TestTextsFromGradesAnEarlierRunsTextsWithNoChangeCall(t *testing.T) {
	const phrase = "phrase/comment:f4@40"
	build, sessions := writeFixture(t)
	// The earlier run: change 7a in lockstep. laneB writes the phrase case's text again, and its
	// added grade lists one fact.
	listed := modeltest.Rule{Contains: markAdded, Match: func(r modeltest.Request) bool { return strings.Contains(r.Text, tag("new_text", writtenAgain)) },
		Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"added": []map[string]string{{"1_quote": writtenAgain, "2_fact": "The problem is the build."}}})}}
	earlier := modeltest.New(t, append([]modeltest.Rule{listed}, lockstepRules(t, nil)...)...)
	lockstep := t.TempDir()
	if code, stderr := scoreSevenA(t, earlier, build, sessions, lockstep, "--models", laneA+","+laneB); code != 0 {
		t.Fatalf("the earlier run = %d:\n%s", code, stderr)
	}
	lanes := []string{laneA, laneB}
	gradesFrom := filepath.Join(lockstep, laneA) + "," + filepath.Join(lockstep, laneB)
	score := func(stand *modeltest.Model, out string, extra ...string) (int, string) {
		t.Helper()
		args := append([]string{"score", "--cases", build, "--sessions", sessions, "--out", out, "--gateway", stand.URL, "--gateway-key-file", keyFile(t),
			"--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log")}, extra...)
		code, _, stderr := runHarness(args...)
		return code, stderr
	}

	stand := modeltest.New(t, allJudgementRules(t)...)
	out := t.TempDir()
	code, stderr := score(stand, out, "--texts-from", lockstep, "--grades-from", gradesFrom)
	if code != 0 {
		t.Fatalf("score --texts-from = %d:\n%s", code, stderr)
	}
	if n := len(stand.Requests()); n != 0 {
		t.Errorf("the run made %d model calls, want none", n)
	}
	for _, m := range lanes {
		src, dir := filepath.Join(lockstep, m), filepath.Join(out, m)
		if want := "change 7a with " + m + ", the texts of " + src + ", part development: 4 cases, 0 with a result, 4 to score"; !strings.Contains(stderr, want) {
			t.Errorf("the run does not say %q:\n%s", want, stderr)
		}
		p, was := readRunParams(t, dir), readRunParams(t, src)
		if p.TextsFrom != src || p.Change != "7a" || p.Model != m || p.Models[RoleCandidate] != m || p.AddedGrader != AddedOld ||
			p.WriterSHA256 != was.WriterSHA256 || p.WriterOrder != was.WriterOrder || p.Models[RoleWriter] != was.Models[RoleWriter] {
			t.Errorf("%s: run.json is %+v; the earlier run's %+v", m, p, was)
		}
		lines, before := readLines(t, dir), readLines(t, src)
		if len(lines) != 4 {
			t.Errorf("%s: %d lines, want 4", m, len(lines))
		}
		for id, r := range lines {
			w := before[id]
			if r.Text != w.Text || r.TextsFrom != src || r.WaitSeconds != w.WaitSeconds || !reflect.DeepEqual(r.ChangeCalls, w.ChangeCalls) || !reflect.DeepEqual(r.ChangeTrace, w.ChangeTrace) {
				t.Errorf("%s %s: the line %+v is not the earlier line's text, trace, wait and calls %+v", m, id, r, w)
			}
			if change, measure := r.calls(); len(change)+len(measure) != 0 {
				t.Errorf("%s %s: the line counts %d change calls and %d grading calls as this run's", m, id, len(change), len(measure))
			}
			for j, g := range r.Grades {
				if !g.Reused && !g.ByConstruction {
					t.Errorf("%s %s: the %s grade is not the earlier run's: %+v", m, id, j, g)
				}
			}
		}
		f, fw := readFigures(t, dir), readFigures(t, src)
		all, allWas := f.Populations[0].All, fw.Populations[0].All
		if slices.ContainsFunc(f.Models, func(u ModelUse) bool { return u.Use == "change" }) || f.Run.ReusedGrades == 0 || !slices.Equal(f.Run.Lockstep, lanes) ||
			!reflect.DeepEqual(all.ChangeCost, allWas.ChangeCost) || !reflect.DeepEqual(all.Wait, allWas.Wait) || !reflect.DeepEqual(all.Added, allWas.Added) {
			t.Errorf("%s: the figures %+v, want no change call, the grades taken, and the earlier change cost, wait and scores", m, f.Run)
		}
		if md, err := os.ReadFile(filepath.Join(dir, "scores.md")); err != nil || !strings.Contains(string(md), "The texts are those of the run in "+src+" (--texts-from)") ||
			!strings.Contains(string(md), "Added grader: old (--added-grader).") {
			t.Errorf("%s: scores.md does not name the run of the texts and the added grader old (%v):\n%s", m, err, md)
		}
	}

	// verify: laneB's phrase case alone has a listed fact; the old added grade of the earlier run
	// is the first call, and the run makes the check.
	stand = modeltest.New(t, append([]modeltest.Rule{{Contains: markCheck,
		Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"facts": []map[string]any{{"1_number": "1", "2_input_words": "", "3_held": false}}})}}}, allJudgementRules(t)...)...)
	out = t.TempDir()
	if code, stderr := score(stand, out, "--added-grader", AddedVerify, "--texts-from", lockstep, "--grades-from", gradesFrom); code != 0 {
		t.Fatalf("score --added-grader verify --texts-from = %d:\n%s", code, stderr)
	}
	if requests := stand.Requests(); len(requests) != 1 || !strings.Contains(requests[0].Text, markCheck) || !strings.Contains(requests[0].Text, writtenAgain) {
		t.Fatalf("the verify run made %d calls, want laneB's check of the phrase case alone", len(requests))
	}
	b := readLines(t, filepath.Join(out, laneB))[phrase]
	if g := b.Grades[JAdded]; g == nil || g.AddedGrader != AddedVerify || g.Verify == nil || !g.Verify.FirstReused || len(g.Verify.Listed) != 1 ||
		g.Count != 1 || g.Verify.Call == nil || !b.Grades[JLost].Reused {
		t.Errorf("laneB's phrase case: the added grade %+v, the lost grade %+v", b.Grades[JAdded], b.Grades[JLost])
	}
	if f, p := readFigures(t, filepath.Join(out, laneB)), readRunParams(t, filepath.Join(out, laneB)); f.Run.ReusedFirstCalls != 1 || p.AddedGrader != AddedVerify {
		t.Errorf("laneB: %d first calls taken, run.json's added grader %q", f.Run.ReusedFirstCalls, p.AddedGrader)
	}
	if md, err := os.ReadFile(filepath.Join(out, laneB, "scores.md")); err != nil || !strings.Contains(string(md), "Added grader: verify (--added-grader).") ||
		!strings.Contains(string(md), ", and the first call of 1 added grades that this run checked.") {
		t.Errorf("laneB's scores.md (%v):\n%s", err, md)
	}

	// inputs-first on one model's directory: lost and added of the one text laneB changed, with the
	// new text last; the run is in --out. Its added grade lists one fact.
	stand = modeltest.New(t, append([]modeltest.Rule{listed}, allJudgementRules(t)...)...)
	out = t.TempDir()
	src := filepath.Join(lockstep, laneB)
	if code, stderr := score(stand, out, "--added-grader", AddedInputsFirst, "--texts-from", src, "--grades-from", src); code != 0 {
		t.Fatalf("score --added-grader inputs-first --texts-from = %d:\n%s", code, stderr)
	}
	requests := stand.Requests()
	if len(requests) != 2 {
		t.Errorf("the inputs-first run made %d calls, want lost and added of the phrase case", len(requests))
	}
	for _, r := range requests {
		if user := decodeTurn(t, r).user(t); !strings.HasSuffix(user, tag("new_text", writtenAgain)) || !(strings.Contains(r.Text, markLost) || strings.Contains(r.Text, markAdded)) {
			t.Errorf("a call of the inputs-first run sends\n%s", user)
		}
	}
	if p := readRunParams(t, out); p.TextsFrom != src || p.Model != laneB || p.AddedGrader != AddedInputsFirst {
		t.Errorf("the inputs-first run's run.json is %+v", p)
	}

	// inputs-first-verify over the same texts, with inputs-first's run among --grades-from: the
	// check of the phrase case's added fact is its one call.
	inputsFirst := out
	stand = modeltest.New(t, append([]modeltest.Rule{{Contains: markCheck,
		Reply: modeltest.Reply{Text: asJSON(t, map[string]any{"facts": []map[string]any{{"1_number": "1", "2_input_words": "", "3_held": false}}})}}}, allJudgementRules(t)...)...)
	out = t.TempDir()
	if code, stderr := score(stand, out, "--added-grader", AddedInputsFirstVerify, "--texts-from", src, "--grades-from", src+","+inputsFirst); code != 0 {
		t.Fatalf("score --added-grader inputs-first-verify --texts-from = %d:\n%s", code, stderr)
	}
	if requests := stand.Requests(); len(requests) != 1 || !strings.Contains(requests[0].Text, markCheck) || !strings.Contains(requests[0].Text, writtenAgain) {
		t.Fatalf("the inputs-first-verify run made %d calls, want the check of the phrase case alone", len(requests))
	}
	b = readLines(t, out)[phrase]
	if g, lost := b.Grades[JAdded], b.Grades[JLost]; g == nil || g.AddedGrader != AddedInputsFirstVerify || g.Verify == nil || !g.Verify.FirstReused ||
		g.Verify.FirstSHA256 != readLines(t, inputsFirst)[phrase].Grades[JAdded].InputSHA256 || g.Count != 1 || g.Verify.Call == nil ||
		lost == nil || !lost.Reused || lost.AddedGrader != AddedInputsFirst {
		t.Errorf("the phrase case: the added grade %+v, the lost grade %+v", g, lost)
	}
	if f, p := readFigures(t, out), readRunParams(t, out); f.Run.ReusedFirstCalls != 1 || p.AddedGrader != AddedInputsFirstVerify {
		t.Errorf("inputs-first-verify: %d first calls taken, run.json's added grader %q", f.Run.ReusedFirstCalls, p.AddedGrader)
	}

	before := len(stand.Requests())
	for _, c := range []struct {
		args []string
		says string
	}{
		{[]string{"--texts-from", lockstep, "--change", "7a", "--sample", "3"}, "give no --change, --sample"},
		{[]string{"--texts-from", lockstep, "--models", laneA}, "give no --models"},
		{[]string{"--texts-from", t.TempDir()}, "holds no run.json"},
		{[]string{"--added-grader", "newest"}, "--added-grader is old, inputs-first, verify, inputs-first-verify"},
	} {
		if code, stderr := score(stand, filepath.Join(t.TempDir(), "run"), c.args...); code != 2 || !strings.Contains(stderr, c.says) {
			t.Errorf("score %v = %d, want 2 saying %q:\n%s", c.args, code, c.says, stderr)
		}
	}
	if n := len(stand.Requests()) - before; n != 0 {
		t.Errorf("the refused runs made %d model calls", n)
	}
}

// A line of --texts-from that keeps the writer's answer gives it to the judgements. A line of a
// change that asks the writer a side question, for a case with a session part, that keeps none (a
// run made before cases.jsonl kept it) gives none: the regrade's line says so, and the figures and
// scores.md count it.
func TestTextsFromGivesTheWritersAnswerALineKeeps(t *testing.T) {
	const question, answer = "question/comment:q1@11", "answer/comment:a2@20"
	const kept = "ANSWERFACT: engine one ships in May."
	build, sessions := writeFixture(t)
	key := keyFile(t)
	exposure := filepath.Join(t.TempDir(), "exposure.log")
	stand := modeltest.New(t, allJudgementRules(t)...)
	// A run of change 1 gives the lines. The test makes them a change 5 run's that rewrote two
	// texts, of which only the answer case's line keeps the writer's answer.
	src := t.TempDir()
	if code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", src, "--gateway", stand.URL, "--gateway-key-file", key,
		"--draws", "50", "--exposure-log", exposure); code != 0 {
		t.Fatalf("change 1 = %d:\n%s", code, stderr)
	}
	rewriteResults(t, src, func(r *CaseResult) {
		switch r.CaseID {
		case question:
			r.Text = "The zork train, the queue of pull requests, is ready."
		case answer:
			r.Text = "The blocker is the release of engine one, which ships in May."
			a := kept
			r.WriterAnswer = &a
		}
		r.Change, r.TextSHA256 = changeRewrite, sha256Hex([]byte(r.Text))
	})
	p := readRunParams(t, src)
	p.Change, p.Model, p.Models[RoleCandidate] = changeRewrite, laneA, laneA
	if err := writeJSON(filepath.Join(src, "run.json"), p); err != nil {
		t.Fatal(err)
	}

	regrade := modeltest.New(t, allJudgementRules(t)...)
	out := t.TempDir()
	if code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", out, "--texts-from", src, "--grader-inputs", InputsFull,
		"--gateway", regrade.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", exposure); code != 0 {
		t.Fatalf("score --texts-from = %d:\n%s", code, stderr)
	}
	lines := readLines(t, out)
	for id, r := range lines {
		switch {
		case id == question && (!r.WriterAnswerNotKept || r.WriterAnswer != nil):
			t.Errorf("the question case's line keeps no answer: %+v", r)
		case id == answer && (r.WriterAnswerNotKept || r.WriterAnswer == nil || *r.WriterAnswer != kept):
			t.Errorf("the answer case's line keeps the answer: %+v", r)
		case id != question && r.WriterAnswerNotKept:
			t.Errorf("%s: the line says it lost an answer: %+v", id, r)
		}
		if r.Change != changeRewrite || r.Error != "" {
			t.Errorf("%s: change %q, error %q", id, r.Change, r.Error)
		}
	}
	var withAnswer, without int
	for _, text := range requestsOf(regrade, markAdded) {
		switch {
		case strings.Contains(text, lines[answer].Text) && strings.Contains(text, tag("writer_answer", kept)):
			withAnswer++
		case strings.Contains(text, lines[question].Text) && !strings.Contains(text, "<writer_answer>"):
			without++
		}
	}
	if withAnswer != 1 || without != 1 {
		t.Errorf("added calls: %d of the answer case with its answer, %d of the question case with none; want 1 and 1", withAnswer, without)
	}
	if f := readFigures(t, out); f.Run.WriterAnswersNotKept != 1 {
		t.Errorf("the figures count %d lines that kept no answer, want 1", f.Run.WriterAnswersNotKept)
	}
	if md, err := os.ReadFile(filepath.Join(out, "scores.md")); err != nil || !strings.Contains(string(md), "(a run made before cases.jsonl kept the answer): 1.") {
		t.Errorf("scores.md does not count the line that kept no answer (%v):\n%s", err, md)
	}
	if p := readRunParams(t, out); p.Change != changeRewrite || p.Model != laneA || p.TextsFrom != src {
		t.Errorf("run.json is %+v", p)
	}
}
