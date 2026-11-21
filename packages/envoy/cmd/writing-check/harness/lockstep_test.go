package harness

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
)

// The two candidates of the lockstep tests, in the order --models names them.
const (
	laneA = "claude-haiku-5-5"
	laneB = "claude-sonnet-5-5"
)

// The phrase case's sentence, which laneB's detector calls vacuous and laneA's does not, and the
// text the writer writes in its place.
const (
	vacuousSentence = "This names the problem."
	writtenAgain    = "The problem is the build."
)

// lockstepRules answer change 7a on the fixture: laneB's detector calls the phrase case's
// sentence vacuous, laneA's calls nothing vacuous; the writer writes the text again; and the grader
// answers every judgement. writer, where it is set, answers the writer in place of that rule.
func lockstepRules(t *testing.T, writer *modeltest.Reply) []modeltest.Rule {
	t.Helper()
	again := modeltest.Reply{Text: asJSON(t, map[string]string{"text": writtenAgain})}
	if writer != nil {
		again = *writer
	}
	return append([]modeltest.Rule{
		{Model: laneB, Contains: markDetector, Match: func(r modeltest.Request) bool { return strings.Contains(r.Text, vacuousSentence) },
			Reply: modeltest.Reply{Text: `{"vacuous":[{"id":"c1","rewrite":"This is the problem."}]}`}},
		{Model: laneB, Contains: markDetector, Reply: modeltest.Reply{Text: `{"vacuous":[]}`}},
		{Model: laneA, Contains: markDetector, Reply: modeltest.Reply{Text: `{"vacuous":[]}`}},
		{Model: "claude-opus-5-5", Contains: markWriterRewrite, Reply: again},
	}, allJudgementRules(t)...)
}

// scoreSevenA runs `harness score --change 7a` on the fixture against stand with extra flags and
// answers its status and its stderr.
func scoreSevenA(t *testing.T, stand *modeltest.Model, build, sessions, out string, extra ...string) (int, string) {
	t.Helper()
	args := append([]string{"score", "--change", "7a", "--cases", build, "--sessions", sessions, "--out", out,
		"--gateway", stand.URL, "--gateway-key-file", keyFile(t), "--draws", "50", "--exposure-log", filepath.Join(t.TempDir(), "exposure.log")}, extra...)
	code, _, stderr := runHarness(args...)
	return code, stderr
}

// scoresOf are the figures that two runs of the same results share: every score and each model's
// calls, without the times a run measures (the wait).
func scoresOf(f Figures) Figures {
	out := Figures{Run: FigureRun{Params: f.Run.Params, Selected: f.Run.Selected, Results: f.Run.Results}}
	for _, p := range f.Populations {
		q := Population{Name: p.Name, Groups: map[string]*GroupFigures{}}
		strip := func(g *GroupFigures) *GroupFigures {
			c := *g
			c.Wait = nil
			return &c
		}
		for name, g := range p.Groups {
			q.Groups[name] = strip(g)
		}
		q.All = strip(p.All)
		out.Populations = append(out.Populations, q)
	}
	out.Models = f.Models
	return out
}

// A lockstep run of two models writes one run directory for each, under --out: the run.json a run
// of --model with that model writes, and the figures that run computes. Run alone on the same
// stand-in grades, each model's run gives the same scores, and --figures-only of --model on a
// lockstep's directory computes its figures again, byte for byte the same scores.
func TestALockstepRunWritesTheRunOfOneModelForEachModel(t *testing.T) {
	build, sessions := writeFixture(t)
	stand := modeltest.New(t, lockstepRules(t, nil)...)
	lockstep := t.TempDir()
	if code, stderr := scoreSevenA(t, stand, build, sessions, lockstep, "--models", laneA+","+laneB); code != 0 {
		t.Fatalf("the lockstep run = %d:\n%s", code, stderr)
	}
	for _, m := range []string{laneA, laneB} {
		dir := filepath.Join(lockstep, m)
		for _, file := range []string{"cases.jsonl", "run.json", "scores.json", "scores.md"} {
			if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
				t.Errorf("%s: %v", m, err)
			}
		}
		alone := t.TempDir()
		if code, stderr := scoreSevenA(t, stand, build, sessions, alone, "--model", m); code != 0 {
			t.Fatalf("the run of %s alone = %d:\n%s", m, code, stderr)
		}
		together, err := os.ReadFile(filepath.Join(dir, "run.json"))
		if err != nil {
			t.Fatal(err)
		}
		single, err := os.ReadFile(filepath.Join(alone, "run.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(together, single) {
			t.Errorf("%s: the lockstep's run.json\n%s\nis not the run.json of the run alone\n%s", m, together, single)
		}
		lanes, ones := readFigures(t, dir), readFigures(t, alone)
		if !reflect.DeepEqual(scoresOf(lanes), scoresOf(ones)) {
			t.Errorf("%s: the lockstep's figures\n%s\nare not those of the run alone\n%s", m, asJSON(t, scoresOf(lanes)), asJSON(t, scoresOf(ones)))
		}
		if !slices.Equal(lanes.Run.Lockstep, []string{laneA, laneB}) || ones.Run.Lockstep != nil || lanes.Run.ScoredThisRun != 4 {
			t.Errorf("%s: the lockstep's run names %v and scored %d; the run alone names %v", m, lanes.Run.Lockstep, lanes.Run.ScoredThisRun, ones.Run.Lockstep)
		}
		if code, stderr := scoreSevenA(t, stand, build, sessions, dir, "--model", m, "--figures-only"); code != 0 {
			t.Fatalf("--figures-only of %s on the lockstep's directory = %d:\n%s", m, code, stderr)
		}
		if again := readFigures(t, dir); !reflect.DeepEqual(again.Populations, lanes.Populations) || !reflect.DeepEqual(again.Models, lanes.Models) {
			t.Errorf("%s: a run of --model computes other figures from the lockstep's results", m)
		}
	}
	// The lanes differ where their candidates do: laneB's detector found the phrase case's sentence,
	// so its writer wrote the text again, and laneA's text is the agent's.
	a, err := readResults(filepath.Join(lockstep, laneA, "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := readResults(filepath.Join(lockstep, laneB, "cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if a["phrase/comment:f4@40"].Text != vacuousSentence || b["phrase/comment:f4@40"].Text != writtenAgain {
		t.Errorf("the phrase case's texts are %q (laneA) and %q (laneB)", a["phrase/comment:f4@40"].Text, b["phrase/comment:f4@40"].Text)
	}
}

// Change 5's side question does not depend on the candidate: Dispatch writes it from the ask, and
// each lane's ask differs only by its id. So in a lockstep run the writer answers it once for a
// case, and the next lane takes that answer, calls no writer, gives it to its own ask's question,
// and records the call with the lane that made it, which its cost per text and its wait hold and
// its model calls do not. Change 6's side question is shared the same way.
func TestALockstepRunSharesTheWritersAnswerToASideQuestion(t *testing.T) {
	for _, step := range []string{changeRewrite, changeDefine} {
		t.Run("change "+step, func(t *testing.T) {
			fake := newFakeDispatch(t)
			run, stand := changeRig(t, []string{step}, fake)
			changes, err := run.withCandidates(&modelFlags{gateway: stand.URL, keyFile: keyFile(t)}, []string{candidateID, laneB}, true)
			if err != nil {
				t.Fatal(err)
			}
			answers := func() int {
				n := 0
				for _, r := range stand.Requests() {
					if strings.Contains(r.Text, markWriterAnswer) {
						n++
					}
				}
				return n
			}
			// The ask goes through the local Dispatch's route, the comment through the component.
			for _, id := range []string{"question/ask:k1@11", "question/comment:c2@20"} {
				made := answers()
				first, second := apply(t, changes[0].(*changeRun), id), apply(t, changes[1].(*changeRun), id)
				if n := answers() - made; n != 1 {
					t.Errorf("%s: the writer answered %d times for the two lanes, want once", id, n)
				}
				if first.WriterAnswer != writerAnswer || second.WriterAnswer != writerAnswer {
					t.Errorf("%s: the lanes' answers are %q and %q", id, first.WriterAnswer, second.WriterAnswer)
				}
				writer := func(a Applied) Call {
					for _, c := range a.Calls {
						if c.Model == "claude-opus-5-5" {
							return c
						}
					}
					t.Fatalf("%s: the change records no writer call: %+v", id, a.Calls)
					return Call{}
				}
				mine, taken := writer(first), writer(second)
				if mine.SharedFrom != "" || taken.SharedFrom != candidateID || taken.Input != mine.Input || taken.CostUSD != mine.CostUSD || taken.CostUSD <= 0 {
					t.Errorf("%s: the writer's calls are %+v (made) and %+v (taken), want the one call, the second naming the lane that made it", id, mine, taken)
				}
				if second.WaitSeconds < taken.Seconds {
					t.Errorf("%s: the second lane's wait %v does not hold the writer's %v", id, second.WaitSeconds, taken.Seconds)
				}
				// The second lane's figures: the shared call is in its cost per text and not in its model calls.
				result := CaseResult{CaseID: id, Group: GroupQuestion, SplitGroup: "T1", ChangeCalls: second.Calls, Grades: map[Judgement]*Grade{}}
				f := computeFigures(RunParams{Groups: []string{GroupQuestion}}, []CaseResult{result}, 1, 1, 10, run.cfg)
				cost := 0.0
				for _, c := range second.Calls {
					cost += c.CostUSD
				}
				if f.Run.SharedAnswers != 1 || f.Populations[0].All.ChangeCost == nil || *f.Populations[0].All.ChangeCost.Mean != cost {
					t.Errorf("%s: the second lane's figures share %d answers at a cost of %+v, want 1 and %v", id, f.Run.SharedAnswers, f.Populations[0].All.ChangeCost, cost)
				}
				if slices.ContainsFunc(f.Models, func(u ModelUse) bool { return u.Model == "claude-opus-5-5" }) {
					t.Errorf("%s: the second lane's model calls count the writer's call it took: %+v", id, f.Models)
				}
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if step == changeRewrite && (fake.replies["question-1"] != writerAnswer || fake.replies["question-2"] != writerAnswer) {
				t.Errorf("each lane's ask got the answer: %v", fake.replies)
			}
			if step == changeDefine && (fake.replies["definition-1"] != writerAnswer || fake.replies["definition-2"] != writerAnswer) {
				t.Errorf("each lane's definition got the answer: %v", fake.replies)
			}
		})
	}
}

// A lockstep run continues each model's run from the cases it holds no result for: laneB's
// writer failed on the phrase case, so the next run scores that case for laneB alone and calls
// nothing for laneA.
func TestALockstepRunContinuesEachModelsCasesWithNoResult(t *testing.T) {
	build, sessions := writeFixture(t)
	broken := modeltest.Reply{Text: "not an object"}
	first := modeltest.New(t, lockstepRules(t, &broken)...)
	out := t.TempDir()
	code, stderr := scoreSevenA(t, first, build, sessions, out, "--models", laneA+","+laneB)
	if code != 1 || !strings.Contains(stderr, "1 of 4 cases of "+laneB+" have no result") {
		t.Fatalf("the first run = %d, want laneB's phrase case unscored:\n%s", code, stderr)
	}
	second := modeltest.New(t, lockstepRules(t, nil)...)
	code, stderr = scoreSevenA(t, second, build, sessions, out, "--models", laneA+","+laneB)
	if code != 0 {
		t.Fatalf("the second run = %d:\n%s", code, stderr)
	}
	for _, want := range []string{
		"change 7a with " + laneA + ", part development: 4 cases, 4 with a result, 0 to score",
		"change 7a with " + laneB + ", part development: 4 cases, 3 with a result, 1 to score",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the second run does not say %q:\n%s", want, stderr)
		}
	}
	for _, r := range second.Requests() {
		if r.Model == laneA {
			t.Error("the second run called laneA, whose run holds every case")
		}
		if !strings.Contains(r.Text, vacuousSentence) && !strings.Contains(r.Text, writtenAgain) {
			t.Errorf("the second run called a model for another case than the phrase case: %.200s", r.Text)
		}
	}
	if n := readFigures(t, filepath.Join(out, laneB)).Run.Results; n != 4 {
		t.Errorf("laneB's run holds %d results, want 4", n)
	}
}

// A held-out run that stopped part way continues in its own directory at the same commit with no
// --new-run: its line names the run it continues, and counts the results it retries. Once every
// case has a result, the run is refused, and so is a second run of its cells elsewhere.
func TestAHeldOutRunThatStoppedContinuesWithNoNewRun(t *testing.T) {
	build, sessions := writeFixture(t)
	exposure := filepath.Join(t.TempDir(), "exposure.log")
	out := t.TempDir()
	key := keyFile(t)
	score := func(ctx context.Context, stand *modeltest.Model, dir string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := Run(ctx, []string{"score", "--change", "7a", "--models", laneA + "," + laneB, "--part", "held_out", "--commit", "c0ffee",
			"--cases", build, "--sessions", sessions, "--out", dir, "--gateway", stand.URL, "--gateway-key-file", key,
			"--draws", "50", "--exposure-log", exposure}, &stdout, &stderr)
		return code, stderr.String()
	}
	held := func(r modeltest.Request) bool { return strings.Contains(r.Text, "The flim gate is shut.") }

	// The first run stops while the grader reads the held-out case: no lane holds a line for it.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	stopping := modeltest.New(t, append([]modeltest.Rule{{Contains: markUndefined, Match: func(r modeltest.Request) bool {
		if held(r) {
			stop()
		}
		return held(r)
	}, Reply: modeltest.Reply{Delay: time.Minute}}}, lockstepRules(t, nil)...)...)
	if code, stderr := score(ctx, stopping, out); code != 1 || !strings.Contains(stderr, "stopped") {
		t.Fatalf("the stopped run = %d:\n%s", code, stderr)
	}
	// The second goes on with it, and its grader fails the case for both lanes.
	failing := modeltest.New(t, append([]modeltest.Rule{{Contains: markUndefined, Match: held, Reply: modeltest.Reply{Text: "not an object"}}},
		lockstepRules(t, nil)...)...)
	code, stderr := score(context.Background(), failing, out)
	if code != 1 || !strings.Contains(stderr, "records this run as a continuation of the held-out run begun") {
		t.Fatalf("the continuation = %d, want a continuation whose case fails:\n%s", code, stderr)
	}
	// The third retries the two failed results.
	stand := modeltest.New(t, lockstepRules(t, nil)...)
	if code, stderr := score(context.Background(), stand, out); code != 0 {
		t.Fatalf("the continuation that retries = %d:\n%s", code, stderr)
	}
	logged, err := ReadExposures(exposure)
	if err != nil {
		t.Fatal(err)
	}
	if len(logged) != 3 || logged[0].Continues != "" || logged[1].Continues != logged[0].Time || logged[2].Continues != logged[0].Time ||
		logged[1].Retried != 0 || logged[2].Retried != 2 || logged[0].Models[RoleCandidate] != laneA+","+laneB || logged[2].Reason != "" {
		t.Errorf("the exposure log: %+v", logged)
	}
	calls := len(stand.Requests())
	if code, stderr := score(context.Background(), stand, out); code != 1 || !strings.Contains(stderr, "nothing to continue") {
		t.Errorf("a continuation of a run with every result = %d, want the refusal:\n%s", code, stderr)
	}
	if code, stderr := score(context.Background(), stand, t.TempDir()); code != 1 || !strings.Contains(stderr, "already holds a run of the held-out part at commit c0ffee") {
		t.Errorf("a second run of the cells in another directory = %d, want the refusal:\n%s", code, stderr)
	}
	if len(stand.Requests()) != calls {
		t.Error("a refused run called a model")
	}
	if logged, _ := ReadExposures(exposure); len(logged) != 3 {
		t.Errorf("a refused run wrote the exposure log: %d lines", len(logged))
	}
}
