package harness

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// rewriteResults writes a run's cases.jsonl again with change applied to each case's result.
func rewriteResults(t *testing.T, out string, change func(*CaseResult)) {
	t.Helper()
	path := filepath.Join(out, "cases.jsonl")
	results, err := readResults(path)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, id := range slices.Sorted(maps.Keys(results)) {
		r := results[id]
		change(&r)
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(append(line, '\n'))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// measureCost is the cost of the calls a run's figures count as its own.
func measureCost(t *testing.T, out string) float64 {
	t.Helper()
	var cost float64
	for _, u := range readFigures(t, out).Models {
		cost += u.CostUSD
	}
	return cost
}

// A run takes another run's answers and grades for a case whose text is the same and whose
// reader and grader read the same inputs: it calls no model for them and counts no cost of them.
// A case whose text the other run graded differently, and a judgement whose inputs changed, are
// graded by this run.
func TestARunTakesAnotherRunsGradesOfTheSameText(t *testing.T) {
	const question, phrase = "question/comment:q1@11", "phrase/comment:f4@40"
	build, sessions := writeFixture(t)
	key := keyFile(t)
	score := func(extra ...string) (*modeltest.Model, string, map[string]CaseResult) {
		t.Helper()
		stand := modeltest.New(t, graderRules(t)...)
		out := t.TempDir()
		args := append([]string{"score", "--cases", build, "--sessions", sessions, "--out", out,
			"--gateway", stand.URL, "--gateway-key-file", key, "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log")}, extra...)
		if code, _, stderr := runHarness(args...); code != 0 {
			t.Fatalf("score %v = %d: %s", extra, code, stderr)
		}
		results, err := readResults(filepath.Join(out, "cases.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return stand, out, results
	}

	first, outA, a := score()
	if n := len(first.Requests()); n == 0 || measureCost(t, outA) <= 0 {
		t.Fatalf("the first run made %d calls at a cost of %v", n, measureCost(t, outA))
	}
	for id, r := range a {
		for j, g := range r.Grades {
			if g.Reused || (g.InputSHA256 == "") != g.ByConstruction {
				t.Errorf("the first run's %s grade of %s: reused %v, inputs %q, by construction %v", j, id, g.Reused, g.InputSHA256, g.ByConstruction)
			}
		}
	}
	// The first run graded another text of the phrase case than the one a later run sees.
	rewriteResults(t, outA, func(r *CaseResult) {
		if r.CaseID == phrase {
			r.Text = "This names another problem."
			r.TextSHA256 = sha256Hex([]byte(r.Text))
		}
	})

	second, outB, b := score("--grades-from", outA)
	requests := second.Requests()
	if len(requests) != 2 {
		t.Errorf("the second run made %d calls, want the phrase case's phrases and undefined calls", len(requests))
	}
	for _, r := range requests {
		if !strings.Contains(r.Text, "This names the problem.") {
			t.Errorf("the second run called a model for a text the first run graded: %s", r.Text)
		}
	}
	for id, r := range b {
		_, measure := r.calls()
		if id == phrase {
			for _, j := range []Judgement{JPhrases, JUndefined} {
				if g := r.Grades[j]; g == nil || g.Reused || g.Call == nil {
					t.Errorf("the changed text's %s grade is not this run's: %+v", j, g)
				}
			}
			if len(measure) != 2 {
				t.Errorf("the changed text counts %d calls, want its 2", len(measure))
			}
			continue
		}
		if len(measure) != 0 {
			t.Errorf("%s: the run counts %d calls of a text whose grades it took", id, len(measure))
		}
		for j, g := range r.Grades {
			was := a[id].Grades[j]
			if g.ByConstruction {
				continue
			}
			if !g.Reused || g.InputSHA256 != was.InputSHA256 || g.Count != was.Count || (g.Call == nil) != (was.Call == nil) {
				t.Errorf("%s: the %s grade %+v is not the first run's %+v, reused", id, j, g, was)
			}
		}
		for name, pair := range map[string][2]*Answer{"reader": {r.Reader, a[id].Reader}, "first paragraph reader": {r.FirstParagraphReader, a[id].FirstParagraphReader}} {
			if got, was := pair[0], pair[1]; (got == nil) != (was == nil) || (got != nil && (!got.Reused || got.Text != was.Text)) {
				t.Errorf("%s: the %s %+v is not the first run's %+v, reused", id, name, got, was)
			}
		}
	}
	// The run's cost is the cost of the calls it made: the changed text's two.
	var own float64
	changed := b[phrase]
	_, measure := changed.calls()
	for _, c := range measure {
		own += c.CostUSD
	}
	if got := measureCost(t, outB); own <= 0 || !near(got, own) || got >= measureCost(t, outA) {
		t.Errorf("the second run's cost = %v, want the %v of its own calls, below the first run's %v", got, own, measureCost(t, outA))
	}
	// The figures say how many answers and grades the run took: the three other cases' three
	// readers' answers and eleven grades.
	if run := readFigures(t, outB).Run; run.ReusedAnswers != 3 || run.ReusedGrades != 11 {
		t.Errorf("the second run's figures count %d answers and %d grades of the first run, want 3 and 11", run.ReusedAnswers, run.ReusedGrades)
	}
	if md, err := os.ReadFile(filepath.Join(outB, "scores.md")); err != nil ||
		!strings.Contains(string(md), "Taken from other runs, with no call and no cost in this run: 3 readers' answers and 11 grades.") {
		t.Errorf("scores.md does not say what the run took (%v):\n%s", err, md)
	}
	if run := readFigures(t, outA).Run; run.ReusedAnswers != 0 || run.ReusedGrades != 0 {
		t.Errorf("the first run's figures count %d answers and %d grades of other runs", run.ReusedAnswers, run.ReusedGrades)
	}
	data, err := os.ReadFile(filepath.Join(outB, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var params RunParams
	if err := json.Unmarshal(data, &params); err != nil || !slices.Equal(params.GradesFrom, []string{outA}) {
		t.Errorf("run.json records the runs %v (%v), want %s", params.GradesFrom, err, outA)
	}

	// Other grader inputs: a judgement that reads other context is graded again, on the answer
	// the first run's reader gave.
	third, _, c := score("--grades-from", outA, "--grader-inputs", "full")
	q := c[question]
	if g := q.Grades[JAnswered]; g == nil || g.Reused || g.Inputs != InputsFull || g.InputSHA256 == a[question].Grades[JAnswered].InputSHA256 {
		t.Errorf("with other grader inputs, the answered grade: %+v", g)
	}
	if q.Reader == nil || !q.Reader.Reused {
		t.Errorf("with other grader inputs, the reader's answer: %+v", q.Reader)
	}
	if n := len(requestsOf(third, markReader)); n != 0 || len(requestsOf(third, markAnswered)) == 0 {
		t.Errorf("with other grader inputs, the run called the reader %d times and graded %d answers", n, len(requestsOf(third, markAnswered)))
	}

	// Several runs: each case's grades come from the first run named that holds them, and a
	// grade a run took from another can be taken again.
	fourth, outD, d := score("--grades-from", outA+","+outB)
	if n := len(fourth.Requests()); n != 0 || measureCost(t, outD) != 0 {
		t.Errorf("a run over both runs made %d calls at a cost of %v, want none", n, measureCost(t, outD))
	}
	if g := d[phrase].Grades[JPhrases]; g == nil || !g.Reused || g.Count != 1 {
		t.Errorf("the phrase case's grade from the second run: %+v", g)
	}

	// A run directory with no results is refused.
	if code, _, stderr := runHarness("score", "--cases", build, "--sessions", sessions, "--out", t.TempDir(), "--grades-from", outA+","+t.TempDir(),
		"--gateway", first.URL, "--gateway-key-file", key); code != 2 || !strings.Contains(stderr, "holds no results") {
		t.Errorf("--grades-from a directory with no results = %d: %s", code, stderr)
	}
}

// A text that change 6 left as the agent wrote it is the text change 1 graded. Each judgement of
// it but define reads no writer's answer, so a run takes change 1's grade with no call; define
// reads the answer and is graded. A line that keeps no answer for such a text lost none that a
// judgement reads. A changed text of the same case reads the writer's answer.
func TestAnUnchangedTextTakesTheBaselinesGrades(t *testing.T) {
	const question = "question/comment:q1@11"
	build, sessions := writeFixture(t)
	key := keyFile(t)
	exposure := filepath.Join(t.TempDir(), "exposure.log")
	score := func(out string, extra ...string) *modeltest.Model {
		t.Helper()
		stand := modeltest.New(t, allJudgementRules(t)...)
		args := append([]string{"score", "--cases", build, "--sessions", sessions, "--out", out, "--gateway", stand.URL, "--gateway-key-file", key,
			"--draws", "50", "--exposure-log", exposure}, extra...)
		if code, _, stderr := runHarness(args...); code != 0 {
			t.Fatalf("score %v = %d:\n%s", extra, code, stderr)
		}
		return stand
	}
	baseline := t.TempDir()
	score(baseline)
	was := readLines(t, baseline)[question]
	// texts is the directory of a change 6 run made of the baseline's lines, whose line of the
	// question case holds text, answer and definition.
	texts := func(text string, answer *string, definition string) string {
		t.Helper()
		dir := t.TempDir()
		for _, name := range []string{"run.json", "cases.jsonl"} {
			data, err := os.ReadFile(filepath.Join(baseline, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		rewriteResults(t, dir, func(r *CaseResult) {
			if r.CaseID == question {
				r.Text, r.TextSHA256, r.WriterAnswer, r.Definition = text, sha256Hex([]byte(text)), answer, definition
			}
			r.Change = changeDefine
		})
		p := readRunParams(t, dir)
		p.Change, p.Model, p.Models[RoleCandidate], p.Groups = changeDefine, laneA, laneA, []string{GroupQuestion}
		if err := writeJSON(filepath.Join(dir, "run.json"), p); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	answer := writerAnswer

	// The text as the agent wrote it, with the writer's answer and a definition.
	out := t.TempDir()
	stand := score(out, "--texts-from", texts(was.Text, &answer, fixtureDefinition), "--grades-from", baseline)
	if requests := stand.Requests(); len(requests) != 1 || !strings.Contains(requests[0].Text, markDefinition) || !strings.Contains(requests[0].Text, tag("writer_answer", writerAnswer)) {
		t.Errorf("the run made %d calls, want define's alone, with the writer's answer", len(requests))
	}
	line := readLines(t, out)[question]
	if line.Error != "" || line.Reader == nil || !line.Reader.Reused || line.Grades[JUndefined] == nil {
		t.Errorf("the unchanged text's line: error %q, reader %+v, undefined grade %+v", line.Error, line.Reader, line.Grades[JUndefined])
	}
	for j, g := range line.Grades {
		switch {
		case j == JDefine:
			if g.Reused || g.Call == nil || g.Error != "" {
				t.Errorf("the define grade %+v is not this run's", g)
			}
		case j == JLost || j == JAdded:
			if !g.ByConstruction {
				t.Errorf("the unchanged text's %s grade %+v is not by construction", j, g)
			}
		case !g.Reused || g.InputSHA256 != was.Grades[j].InputSHA256:
			t.Errorf("the unchanged text's %s grade %+v is not the baseline's %+v", j, g, was.Grades[j])
		}
	}
	if line.Grades[JDefine] == nil {
		t.Error("the unchanged text has no define grade")
	}

	// The same text, on a line that keeps no writer's answer and no definition.
	out = t.TempDir()
	stand = score(out, "--texts-from", texts(was.Text, nil, ""), "--grades-from", baseline)
	if line := readLines(t, out)[question]; len(stand.Requests()) != 0 || line.WriterAnswerNotKept || line.Error != "" {
		t.Errorf("an unchanged text whose line keeps no answer: %d calls, writer_answer_not_kept %t, error %q", len(stand.Requests()), line.WriterAnswerNotKept, line.Error)
	}

	// A text the change changed: each judgement that reads the writer's answer in its input mode
	// reads it.
	stand = score(t.TempDir(), "--texts-from", texts(fixtureNewText, &answer, fixtureDefinition), "--grades-from", baseline)
	for _, marker := range []string{markLost, markAdded, markUndefined, markDefinition} {
		if got := requestsOf(stand, marker); len(got) != 1 || !strings.Contains(got[0], tag("writer_answer", writerAnswer)) {
			t.Errorf("the changed text's %d calls of %q do not read the writer's answer", len(got), marker)
		}
	}
}
