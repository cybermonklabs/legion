package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// stepClock is the clock of every engine these tests build: each start and end they read is
// stamped with it.
var stepClock = time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)

// stepEngine is the engine the step record tests drive: testEngine with no design gate, the fixed
// clock stepClock, and agent as the daemon's read of a step's agent (nil reads none).
func stepEngine(agent func(claim.Token) StepAgent) *Engine {
	return gatedStepEngine(config.DesignGateOff, agent)
}

// gatedStepEngine is stepEngine on a deployment whose design gate is policy, as the daemon builds
// its engine at boot.
func gatedStepEngine(policy config.DesignGate, agent func(claim.Token) StepAgent) *Engine {
	engine := testEngine(policy, nil)
	engine.cfg.Clock = func() time.Time { return stepClock }
	if agent != nil {
		engine.cfg.StepAgent = func(_ context.Context, _ pgx.Tx, token claim.Token) StepAgent { return agent(token) }
	}
	return engine
}

// placeOf is the place of kind at step in the task default, the key an expected id is computed
// from (StepID).
func placeOf(kind string, step int) record.StepPlace {
	return record.StepPlace{Task: "default", Kind: kind, Place: step}
}

// stepRoot is the root LEGION-208 standing at at, generation 1, with its open pull request #42.
func stepRoot(t *testing.T, pool *pgxpool.Pool, at phase.Phase) record.Issue {
	t.Helper()
	root := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208",
		Project: "LEGION", Title: "root", Phase: at, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
	seedIssue(t, pool, root)
	seedTreeLifecycle(t, pool, root.Key)
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root.Key, Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})
	return root
}

// stepChild is the child key of LEGION-208 standing at at, generation 3.
func stepChild(t *testing.T, pool *pgxpool.Pool, key string, at phase.Phase, hold *record.Hold) record.Issue {
	t.Helper()
	parent := "LEGION-208"
	child := record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: parent, Project: "LEGION",
		Title: key, Parent: &parent, Phase: at, Hold: hold, Generation: 3, Status: "in_progress", Rank: "V", LastDispatchSeq: 1}
	seedIssue(t, pool, child)
	return child
}

// seedChildPR gives LEGION-209 its pull request #43 in state, with its merge commit where merged.
func seedChildPR(t *testing.T, pool *pgxpool.Pool, state record.PullRequestState) {
	t.Helper()
	pr := record.PullRequest{State: state, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}}
	if state == record.PullRequestMerged {
		pr.MergeSHA = "merge-43"
	}
	seedPR(t, pool, pr)
}

// mergingChild seeds LEGION-209 at the retro under LEGION-208, with its open pull request, and
// moves it into merging through engine: the start of its merge step, which the test then ends.
func mergingChild(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	stepChild(t, pool, "LEGION-209", phase.Retro, nil)
	seedChildPR(t, pool, record.PullRequestOpen)
	move(t, pool, engine, "LEGION-209", TriggerRetroCompleted, "", record.PhaseRow{})
}

// holdFrom holds key from the phase from, where the death of that phase's worker's claim leaves it.
func holdFrom(t *testing.T, pool *pgxpool.Pool, key string, from phase.Phase) {
	t.Helper()
	held := issueRecord(t, pool, key)
	held.Phase, held.Hold = phase.Held, &record.Hold{From: from}
	seedIssue(t, pool, *held)
}

// onStored runs write in one transaction on key's record as the store holds it, as a fact applies
// it (applyInTx).
func onStored(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, write func(ctx context.Context, tx pgx.Tx, issue record.Issue) error) {
	t.Helper()
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		return write(ctx, tx, *issue)
	})
}

// move runs one transition of key on trigger (into target, for a backward move) through engine,
// on the record as the store holds it, with handoff as the completion's row.
func move(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, trigger TriggerKind, target phase.Phase, handoff record.PhaseRow) {
	t.Helper()
	onStored(t, pool, engine, key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		pr, err := engine.store.PullRequest(ctx, tx, key)
		if err != nil {
			return err
		}
		// A move no completion records (a backward move) says why, as the engine
		// requires of its notice; the other moves carry the reason unread.
		return engine.transition(ctx, tx, issue, trigger, target, handoff, pr, "the step test's move")
	})
}

// stepWrites is every step record row issue's outbox holds, in the order they were written.
func stepWrites(t *testing.T, pool *pgxpool.Pool, issue string) []record.StepRecord {
	t.Helper()
	var got []record.StepRecord
	for _, row := range issueOutboxOf(t, pool, issue, string(record.OutboxKindStepRecord)) {
		var write record.StepRecord
		if err := json.Unmarshal(row.payload, &write); err != nil {
			t.Fatalf("decode a step record row of %s: %v", issue, err)
		}
		got = append(got, write)
	}
	return got
}

func isStart(w record.StepRecord) bool { return w.StartedAt != nil && w.EndedAt == nil }
func isEnd(w record.StepRecord) bool   { return w.EndedAt != nil && w.Result != nil }

// writesAt is the writes of one place among writes, those of kind at step, in their order.
func writesAt(writes []record.StepRecord, kind string, step int) []record.StepRecord {
	var at []record.StepRecord
	for _, w := range writes {
		if w.Kind == kind && w.Step == step {
			at = append(at, w)
		}
	}
	return at
}

// runOf is the latest start and the latest end among writes of kind at step, the zero record for
// none.
func runOf(writes []record.StepRecord, kind string, step int) (start, end record.StepRecord) {
	for _, w := range writesAt(writes, kind, step) {
		if isStart(w) {
			start = w
		}
		if isEnd(w) {
			end = w
		}
	}
	return start, end
}

// resultOf is the result an end write carries, "" for a start.
func resultOf(w record.StepRecord) string {
	if w.Result == nil {
		return ""
	}
	return *w.Result
}

// assertEnd fails unless w is the end of kind/step of issue's generation with result, returning
// to returned (nil: returning nowhere).
func assertEnd(t *testing.T, w record.StepRecord, kind string, step int, generation uint64, result string, returned *int) {
	t.Helper()
	if !isEnd(w) || w.Kind != kind || w.Step != step || w.Generation != generation || resultOf(w) != result || w.Task != "default" {
		t.Fatalf("write = %+v (result %q), want the end of %s %d of generation %d, %s", w, resultOf(w), kind, step, generation, result)
	}
	if (returned == nil) != (w.ReturnedTo == nil) || returned != nil && *returned != *w.ReturnedTo {
		t.Fatalf("the end of %s %d returns to %v, want %v", kind, step, w.ReturnedTo, returned)
	}
}

// A record's start and its end are two writes, maybe made by two daemons with a restart between
// them, and they meet in one record only when both compute one id. The count of starts is what
// carries it: a row of the store, raised by the start and read unchanged by the end.
func TestTheStartAndTheEndOfARunShareAnIDAcrossARestart(t *testing.T) {
	run := func(t *testing.T, between func(pool *pgxpool.Pool)) (start, end record.StepRecord, pool *pgxpool.Pool) {
		pool = migratedPool(t)
		stepRoot(t, pool, phase.Implementing)
		move(t, pool, stepEngine(nil), "LEGION-208", TriggerImplementationReady, "", record.PhaseRow{})
		between(pool)
		// The restart: a new engine on a new store over the same database, and its boot.
		rebooted(t, pool)
		move(t, pool, stepEngine(nil), "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{})
		start, end = runOf(stepWrites(t, pool, "LEGION-208"), "step", 3)
		return start, end, pool
	}
	start, end, pool := run(t, func(*pgxpool.Pool) {})
	if want := StepID("LEGION-208", 1, placeOf("step", 3), 1); start.ID != want {
		t.Fatalf("the start of testing has id %s, want %s (the first start of default/step/3)", start.ID, want)
	}
	if end.ID != start.ID {
		t.Fatalf("the end of testing has id %s after the restart, want the start's %s", end.ID, start.ID)
	}
	var starts int64
	if err := pool.QueryRow(t.Context(), `select starts from step_starts where issue = 'LEGION-208' and task = 'default' and kind = 'step' and place = 3`).Scan(&starts); err != nil || starts != 1 {
		t.Fatalf("starts of default/step/3 = %d, %v; want the one raise of the start", starts, err)
	}
	t.Run("control", func(t *testing.T) {
		start, end, _ := run(t, func(pool *pgxpool.Pool) {
			if _, err := pool.Exec(t.Context(), `delete from step_starts where issue = 'LEGION-208'`); err != nil {
				t.Fatalf("delete the count: %v", err)
			}
		})
		if start.ID == "" || end.ID == start.ID {
			t.Fatalf("with the count deleted between the engines the end's id is %s and the start's %s; the count is what carries the id, so they must differ", end.ID, start.ID)
		}
	})
}

// The end of a run writes again the moment its start wrote, so the record's start is one moment
// whichever of the two writes reaches Dispatch first: the start is stamped to the microsecond,
// the precision the count of starts keeps it at, and the end, written by another engine after a
// restart, carries that moment and its own later one. The two engines' clocks differ and read
// nanoseconds, so an end that wrote its own moment as the start, or a start finer than what its
// end reads back, shows here.
func TestAnEndWritesTheMomentItsStartWrote(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Implementing)
	began := time.Date(2026, 11, 17, 1, 2, 3, 456789123, time.UTC)
	ended := began.Add(90*time.Minute + 987654321*time.Nanosecond)
	starting, ending := stepEngine(nil), stepEngine(nil)
	starting.cfg.Clock = func() time.Time { return began }
	ending.cfg.Clock = func() time.Time { return ended }
	move(t, pool, starting, "LEGION-208", TriggerImplementationReady, "", record.PhaseRow{})
	move(t, pool, ending, "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{})
	start, end := runOf(stepWrites(t, pool, "LEGION-208"), "step", 3)
	if want := began.Truncate(time.Microsecond); start.StartedAt == nil || !start.StartedAt.Equal(want) {
		t.Fatalf("the start of testing began at %v, want %s, the moment its engine read to the microsecond", start.StartedAt, want)
	}
	if end.ID != start.ID || end.Agent != start.Agent || end.StartedAt == nil || !end.StartedAt.Equal(*start.StartedAt) || !end.EndedAt.Equal(ended) {
		t.Fatalf("the end of testing = %+v, want the start's id %s, agent %s and moment %s, ended at %s", end, start.ID, start.Agent, start.StartedAt, ended)
	}
}

// A review's request for changes ends the review failed, naming the implementation step it sends
// the work back to, and starts the implementer again: a second start of step 2, which is another
// record than the first.
func TestAFailedReviewEndsFailedAndStartsTheImplementerAgain(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Planning)
	engine := stepEngine(nil)
	move(t, pool, engine, "LEGION-208", TriggerPlannerCompleted, "", record.PhaseRow{})
	move(t, pool, engine, "LEGION-208", TriggerImplementationReady, "", record.PhaseRow{})
	move(t, pool, engine, "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{})
	before := len(stepWrites(t, pool, "LEGION-208"))
	move(t, pool, engine, "LEGION-208", TriggerReviewRejected, "", record.PhaseRow{})
	writes := stepWrites(t, pool, "LEGION-208")
	if len(writes) != before+2 {
		t.Fatalf("the rejection wrote %d step rows, want its end and one start: %+v", len(writes)-before, writes[before:])
	}
	end, start := writes[before], writes[before+1]
	assertEnd(t, end, "step", 4, 1, "failed", new(2))
	if end.Role != "reviewer" || end.Type != "review" || end.Agent != treeRoleToken(t, "LEGION-208", claim.RoleReviewer) {
		t.Fatalf("the review's end = %+v, want the reviewer's", end)
	}
	if !isStart(start) || start.Step != 2 || start.Role != "implementer" || start.ID != StepID("LEGION-208", 1, placeOf("step", 2), 2) {
		t.Fatalf("the rejection's start = %+v, want the implementer's second start of step 2", start)
	}
	if first := writes[1]; !isStart(first) || first.Step != 2 || first.ID == start.ID {
		t.Fatalf("the first start of step 2 = %+v; want a start of another id than the second's %s", first, start.ID)
	}
}

// A move back to the retro returns the work to place 0, the retro's: the end of the merge it
// interrupts is failed, its merger having asked for the move, and names returned_to 0, present in
// the row the outbox holds, where the forward end of the retro before it names none. A 0 left out
// would read as a return to nowhere.
func TestAMoveBackToTheRetroReturnsToPlaceZero(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Retro)
	engine := stepEngine(nil)
	move(t, pool, engine, "LEGION-208", TriggerRetroCompleted, "", record.PhaseRow{})
	applyFacts(t, pool, engine)("merger-moves-back", intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleMerger, To: phase.Retro,
		Reason: "the retro missed a finding"})
	if got := issueAt(t, pool, "LEGION-208"); got != phase.Retro {
		t.Fatalf("LEGION-208 stands at %s after the merger's move back, want retro", got)
	}
	writes := stepWrites(t, pool, "LEGION-208")
	if len(writes) != 4 || !isStart(writes[1]) || writes[1].Kind != "merge" || !isStart(writes[3]) || writes[3].Kind != "retro" || writes[3].Step != 0 {
		t.Fatalf("step rows = %+v, want the retro's end, the merge's start and end, and the retro's start again", writes)
	}
	assertEnd(t, writes[0], "retro", 0, 1, "passed", nil)
	assertEnd(t, writes[2], "merge", 0, 1, "failed", new(0))
	rows := issueOutboxOf(t, pool, "LEGION-208", string(record.OutboxKindStepRecord))
	for i, want := range map[int]bool{0: false, 2: true} {
		var payload map[string]any
		if err := json.Unmarshal(rows[i].payload, &payload); err != nil {
			t.Fatalf("decode step row %d: %v", i, err)
		}
		returned, present := payload["returned_to"]
		if present != want || present && returned != float64(0) {
			t.Fatalf("step row %d carries returned_to %v (present %t), want present %t and 0: %s", i, returned, present, want, rows[i].payload)
		}
	}
}

// A head that conflicts with its base moves no issue (dispatch://INFRA-4374): the merge forward
// keeps the phase, so the step the issue stands in neither ends nor starts again, and its run goes
// on through the merge.
func TestAConflictEndsNoStep(t *testing.T) {
	pool := seedConflictIssue(t, phase.Testing, "testing", record.PullRequest{})
	confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
	if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Phase != phase.Testing {
		t.Fatalf("the merge forward = %+v, want one asked with the issue in testing", asked)
	}
	if writes := stepWrites(t, pool, "LEGION-208"); len(writes) != 0 {
		t.Fatalf("step rows = %+v, want none: a confirmed conflict ends no step and starts none", writes)
	}
}

// One run of a step gets one end, however many writes end it: a second end of the same run, in
// the same transaction or a later one, finds the first end's mark in the store and queues nothing,
// and so does a second end of a step that started under the release before. The promotion's start
// of a step whose run is open writes nothing either (StartStepRecord), since that run goes on; a
// start once the run ended opens the next run, whose end is queued again.
func TestTwoEndsOfOneRunQueueOneRow(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Implementing)
	stepChild(t, pool, "LEGION-209", phase.Testing, nil)
	engine := stepEngine(nil)
	move(t, pool, engine, "LEGION-208", TriggerImplementationReady, "", record.PhaseRow{})
	endTwice := func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		w, err := engine.walk(ctx, tx, issue)
		if err != nil {
			return err
		}
		if err := engine.endStep(ctx, tx, issue, w, stepStopped); err != nil {
			return err
		}
		return engine.endStep(ctx, tx, issue, w, stepStopped)
	}
	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		onStored(t, pool, engine, key, endTwice)
		onStored(t, pool, engine, key, endTwice)
	}
	testingWrites := func(key string) (starts, ends []record.StepRecord) {
		for _, w := range writesAt(stepWrites(t, pool, key), "step", 3) {
			switch {
			case isStart(w):
				starts = append(starts, w)
			case isEnd(w):
				ends = append(ends, w)
			}
		}
		return starts, ends
	}
	if starts, ends := testingWrites("LEGION-208"); len(starts) != 1 || len(ends) != 1 || ends[0].ID != starts[0].ID {
		t.Fatalf("four ends of one run of testing wrote starts %+v and ends %+v, want one start and its one end", starts, ends)
	}
	if starts, ends := testingWrites("LEGION-209"); len(starts) != 0 || len(ends) != 1 || ends[0].ID != StepID("LEGION-209", 3, placeOf("step", 3), 0) {
		t.Fatalf("four ends of a run no start counted wrote starts %+v and ends %+v, want one end at the count 0", starts, ends)
	}
	startTwice := func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		if err := engine.StartStepRecord(ctx, tx, issue); err != nil {
			return err
		}
		return engine.StartStepRecord(ctx, tx, issue)
	}
	onStored(t, pool, engine, "LEGION-208", startTwice)
	onStored(t, pool, engine, "LEGION-208", endTwice)
	starts, ends := testingWrites("LEGION-208")
	if len(starts) != 2 || len(ends) != 2 || starts[1].ID != StepID("LEGION-208", 1, placeOf("step", 3), 2) || ends[1].ID != starts[1].ID {
		t.Fatalf("a start after the end, a second start and two ends wrote starts %+v and ends %+v, want the second run's one start and one end", starts, ends)
	}
}

// The end of a step carries the session and the model the daemon reads of its agent at that end,
// one read's pair, and the handoff record the ending role's completion named; the start carries
// neither, since Dispatch keeps the first value it is given of each field and only the end names
// the model. A daemon that reads no agent writes neither, so the record names no session and no
// model.
func TestTheEndNamesTheModelThatAnswered(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Testing)
	raisedAt := stepClock.Add(-time.Hour)
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Entry: 1, EntryRaisedAt: raisedAt})
	var asked []claim.Token
	engine := stepEngine(func(agent claim.Token) StepAgent {
		asked = append(asked, agent)
		return StepAgent{Session: "s1", Model: "second/second-model"}
	})
	report := "0b0e4c3e-5d0a-4a35-9c55-1f2e3d4c5b6a"
	move(t, pool, engine, "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{Role: claim.RoleTester, HandoffRecord: report})
	writes := stepWrites(t, pool, "LEGION-208")
	if len(writes) != 2 {
		t.Fatalf("step rows = %+v, want the end of testing and the start of reviewing", writes)
	}
	end, start := writes[0], writes[1]
	assertEnd(t, end, "step", 3, 1, "passed", nil)
	if end.Session == nil || *end.Session != "s1" || end.Model == nil || *end.Model != "second/second-model" {
		t.Fatalf("the end's session %v and model %v, want s1 and second/second-model", end.Session, end.Model)
	}
	if end.Report == nil || *end.Report != report || end.StartedAt == nil || !end.StartedAt.Equal(raisedAt) || !end.EndedAt.Equal(stepClock) {
		t.Fatalf("the end = %+v, want report %s, started at the tester's entry %s and ended at %s", end, report, raisedAt, stepClock)
	}
	if start.Session != nil || start.Model != nil || !start.StartedAt.Equal(stepClock) {
		t.Fatalf("the start = %+v, want no session, no model, started at %s", start, stepClock)
	}
	if len(asked) != 1 || string(asked[0]) != treeRoleToken(t, "LEGION-208", claim.RoleTester) {
		t.Fatalf("the agents asked = %v, want the tester's at the end alone", asked)
	}

	t.Run("no agent read", func(t *testing.T) {
		pool := migratedPool(t)
		stepRoot(t, pool, phase.Testing)
		move(t, pool, stepEngine(nil), "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{})
		for _, w := range stepWrites(t, pool, "LEGION-208") {
			if w.Session != nil || w.Model != nil {
				t.Fatalf("write = %+v, want no session and no model with no StepAgent", w)
			}
		}
	})
}

// The report of a step is the handoff its role's completion named, whichever of that completion
// and the review event lands first: an event that decides the round first ends the review before
// the reviewer completes, and the completion then names its handoff on that ended run's record,
// so one write of the run carries it. That write is the run's: its id, its role, its type, its
// agent and its generation, for a review this release started and counted (count 1) as for one
// that ran at the upgrade (count 0), whose end the store gave the count. The review's handoff is
// the review's alone: the sign-off that ends accepting passes the reviewer's row, and the record
// of acceptance names no report.
func TestAReviewsReportIsItsCompletionsWhicheverLandsFirst(t *testing.T) {
	const report = "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	reviewer := treeRoleToken(t, "LEGION-208", claim.RoleReviewer)
	for _, tc := range []struct {
		name          string
		decision      string
		completeFirst bool
		want          phase.Phase
	}{
		{name: "changes requested, then the reviewer completes", decision: "changes_requested", want: phase.Implementing},
		{name: "the reviewer completes, then changes are requested", decision: "changes_requested", completeFirst: true, want: phase.Implementing},
		{name: "approved on a clean head, then the reviewer completes", decision: "approved", want: phase.Accepting},
		{name: "the reviewer completes, then an approval on a clean head", decision: "approved", completeFirst: true, want: phase.Accepting},
	} {
		for _, counted := range []bool{true, false} {
			name := tc.name + ", a review this release counted"
			if !counted {
				name = tc.name + ", a review running at the upgrade"
			}
			t.Run(name, func(t *testing.T) {
				pool := migratedPool(t)
				seedReview(t, pool, "green")
				engine := testEngine(config.DesignGateRootIssues, nil)
				var starts int64
				if counted {
					// The start this release counted is in the store before the daemon boots, so
					// it is written with no boot pass before it (factTx); the boot then finds
					// the run open and opens nothing.
					factTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
						issue, err := engine.store.Issue(ctx, tx, "LEGION-208")
						if err != nil {
							return err
						}
						return engine.StartStepRecord(ctx, tx, *issue)
					})
					starts = 1
				}
				apply := applyFacts(t, pool, engine)
				review := func() {
					apply("review", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: tc.decision, CommitID: headOne, Author: testReviewApp,
						Body: reviewBody("the review", headOne)})
				}
				complete := func() {
					if result := apply("reviewer-handoff", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer,
						Claim: reviewClaim, Summary: "reviewed", HandoffRecord: report, HandoffVersion: 1}); result.Refusal != nil {
						t.Fatalf("the reviewer's completion was refused: %+v", result.Refusal)
					}
				}
				if tc.completeFirst {
					complete()
					review()
				} else {
					review()
					complete()
				}
				if got := issueAt(t, pool, "LEGION-208"); got != tc.want {
					t.Fatalf("LEGION-208 stands at %s, want %s", got, tc.want)
				}
				run := StepID("LEGION-208", 1, placeOf("step", 4), starts)
				var reviewWrites, ends, late []record.StepRecord
				reports := map[string]int{}
				for _, w := range writesAt(stepWrites(t, pool, "LEGION-208"), "step", 4) {
					if w.ID != run || w.Task != "default" || w.Role != "reviewer" || w.Type != "review" || w.Agent != reviewer || w.Generation != 1 {
						t.Fatalf("a write of the review = %+v, want the identity of its run: id %s, role reviewer, type review, agent %s, generation 1",
							w, run, reviewer)
					}
					reviewWrites = append(reviewWrites, w)
					switch {
					case isEnd(w):
						ends = append(ends, w)
					case !isStart(w):
						late = append(late, w)
					}
					if w.Report != nil {
						reports[*w.Report]++
					}
				}
				if counted && (len(reviewWrites) == 0 || !isStart(reviewWrites[0])) {
					t.Fatalf("the writes of the review = %+v, want its counted start first", reviewWrites)
				}
				if len(ends) != 1 || len(reports) != 1 || reports[report] != 1 {
					t.Fatalf("the writes of the review = %+v with reports %v, want one end and one write naming %s", reviewWrites, reports, report)
				}
				// A completion that lands first names its handoff on the end; one that lands after
				// the end writes the report alone, the one write that is neither start nor end.
				if tc.completeFirst && (ends[0].Report == nil || len(late) != 0) || !tc.completeFirst && (ends[0].Report != nil || len(late) != 1) {
					t.Fatalf("the review's end = %+v and its later writes %+v, want the report on the end when the completion landed first, else on one write after it",
						ends[0], late)
				}
				if tc.want != phase.Accepting {
					return
				}
				apply("sign-off", rootsSignOff(t, pool, engine, "LEGION-208"))
				var ended bool
				for _, w := range writesAt(stepWrites(t, pool, "LEGION-208"), "step", 5) {
					if w.Report != nil {
						t.Fatalf("a write of the acceptance names report %s, want none: the reviewer's handoff is the review's", *w.Report)
					}
					ended = ended || isEnd(w)
				}
				if !ended {
					t.Fatal("the sign-off wrote no end of the acceptance")
				}
			})
		}
	}
}

// Two starts of a step raise no phase entry: accepting, whose role is the architect that owns the
// issue (here the tree's: the root is its own architect's), and merging on a pull request GitHub
// already merged, where no merger starts. Each still writes its start, and the merge's close ends
// it passed, under the agent its start named, none.
func TestAcceptingAndAMergeGitHubMadeGetAStart(t *testing.T) {
	t.Run("accepting", func(t *testing.T) {
		pool := migratedPool(t)
		stepRoot(t, pool, phase.Reviewing)
		move(t, pool, stepEngine(nil), "LEGION-208", TriggerReviewApproved, "", record.PhaseRow{})
		writes := stepWrites(t, pool, "LEGION-208")
		if len(writes) != 2 {
			t.Fatalf("step rows = %+v, want the end of reviewing and the start of accepting", writes)
		}
		start := writes[1]
		if !isStart(start) || start.Step != 5 || start.Role != "acceptance" || start.Type != "review" || start.Agent != treeRoleToken(t, "LEGION-208", claim.RoleArchitect) {
			t.Fatalf("the start of accepting = %+v, want step 5, acceptance, review, the root's architect", start)
		}
	})
	t.Run("a merge GitHub made", func(t *testing.T) {
		pool := migratedPool(t)
		stepRoot(t, pool, phase.Retro)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
			Branch: "legion/LEGION-208", HeadSHA: "head", MergeSHA: "merge", Failing: []string{}})
		seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Entry: 4, EntryRaisedAt: stepClock.Add(-48 * time.Hour)})
		move(t, pool, stepEngine(nil), "LEGION-208", TriggerRetroCompleted, "", record.PhaseRow{})
		writes := stepWrites(t, pool, "LEGION-208")
		if len(writes) != 3 {
			t.Fatalf("step rows = %+v, want the end of the retro, the start of the merge and its end", writes)
		}
		assertEnd(t, writes[0], "retro", 0, 1, "passed", nil)
		start, end := writes[1], writes[2]
		if !isStart(start) || start.Kind != "merge" || start.Agent != "" || start.Role != "merger" {
			t.Fatalf("the merge's start = %+v, want a merge with no agent", start)
		}
		assertEnd(t, end, "merge", 0, 1, "passed", nil)
		// The end writes again the moment its start wrote, never the merger's entry of an earlier
		// merge (stepClock less 48 hours), since no merger started for this one.
		if end.ID != start.ID || end.Agent != "" || end.StartedAt == nil || !end.StartedAt.Equal(*start.StartedAt) {
			t.Fatalf("the merge's end = %+v, want the start's id %s, no agent and the start's moment %v", end, start.ID, start.StartedAt)
		}
		if got := issueAt(t, pool, "LEGION-208"); got != phase.Verifying {
			t.Fatalf("LEGION-208 stands at %s, want verifying: its close waits for its checks", got)
		}
	})
}

// The acceptance of a child is the work of the architect that owns the child when the step starts
// (record.OwnerWalk.Architect): the child's own architect while its claim holds its role, or while
// a start of it the outbox will act on is queued (the move into accepting starts a failed one
// again, reviveArchitect), and the tree's architect otherwise. The record of step 5 names that
// architect's token at the start the review's approval writes and at the end the sign-off writes,
// and the end names the session and the model the daemon reads of that token (the start names
// neither: startStep); so does the end of a run of accepting the release before started, which no
// start counted. Each architect answers with a session and a model of its own, so a record that
// named the other one would carry the other's.
//
// A failed child architect is the tree's at a run the release before started: nothing starts it
// again before the sign-off, and the start the sign-off's own move into the retro queues is the
// retro's, not the acceptance's. The controls are a child architect with no claim, and one the
// operator suspended, which no start wakes before the operator's resume: neither holds its role,
// and the tree's architect owns the child.
func TestAChildsAcceptanceIsItsOwningArchitects(t *testing.T) {
	tree, own := treeRoleToken(t, "LEGION-208", claim.RoleArchitect), treeRoleToken(t, "LEGION-209", claim.RoleArchitect)
	reads := map[string]StepAgent{
		tree: {Session: "ses-root-architect", Model: "anthropic/root-model"},
		own:  {Session: "ses-child-architect", Model: "anthropic/child-model"},
	}
	agent := func(token claim.Token) StepAgent { return reads[string(token)] }
	for _, tc := range []struct {
		name string
		// state is the child architect's claim, "" for none, suspended by by.
		state supervise.ClaimState
		by    supervise.SuspendSource
		// approval is the owner of a run the approval starts, before of one the release before
		// started.
		approval, before string
	}{
		{name: "the child's own architect holds its role", state: supervise.StateIdle, approval: own, before: own},
		{name: "the child's own architect failed, which the move into accepting starts again", state: supervise.StateFailed, approval: own, before: tree},
		{name: "control: the child's own architect has no claim", approval: tree, before: tree},
		{name: "control: the operator suspended the child's own architect", state: supervise.StateSuspended, by: supervise.SuspendedByOperator, approval: tree, before: tree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(t *testing.T, at phase.Phase, enter func(pool *pgxpool.Pool, engine *Engine)) []record.StepRecord {
				t.Helper()
				pool := migratedPool(t)
				stepRoot(t, pool, phase.Implementing)
				stepChild(t, pool, "LEGION-209", at, nil)
				seedChildPR(t, pool, record.PullRequestOpen)
				if tc.state != "" {
					seedArchitectClaimIn(t, pool, "LEGION-1", "LEGION-209", 1, tc.state, tc.by)
				}
				engine := stepEngine(agent)
				enter(pool, engine)
				applyFacts(t, pool, engine)("sign-off", rootsSignOff(t, pool, engine, "LEGION-209"))
				if got := issueAt(t, pool, "LEGION-209"); got != phase.Retro {
					t.Fatalf("LEGION-209 stands at %s after the sign-off, want retro", got)
				}
				return writesAt(stepWrites(t, pool, "LEGION-209"), "step", 5)
			}
			owners := func(t *testing.T, w record.StepRecord, owner string) {
				t.Helper()
				if w.Agent != owner || w.Role != "acceptance" || w.Generation != 3 {
					t.Fatalf("the record of LEGION-209's acceptance = %+v, want agent %s, the architect that owns the child", w, owner)
				}
				if !isEnd(w) {
					if w.Session != nil || w.Model != nil {
						t.Fatalf("the start of LEGION-209's acceptance names session %v and model %v, want neither: the end names the pair", w.Session, w.Model)
					}
					return
				}
				if want := reads[owner]; w.Session == nil || *w.Session != want.Session || w.Model == nil || *w.Model != want.Model {
					t.Fatalf("the end of LEGION-209's acceptance names session %v and model %v, want the owner's %s and %s", w.Session, w.Model, want.Session, want.Model)
				}
			}

			t.Run("the approval starts it", func(t *testing.T) {
				writes := run(t, phase.Reviewing, func(pool *pgxpool.Pool, engine *Engine) {
					move(t, pool, engine, "LEGION-209", TriggerReviewApproved, "", record.PhaseRow{})
				})
				if len(writes) != 2 || !isStart(writes[0]) || !isEnd(writes[1]) || writes[1].ID != writes[0].ID {
					t.Fatalf("the writes of LEGION-209's step 5 = %+v, want the start of accepting and its end", writes)
				}
				owners(t, writes[0], tc.approval)
				owners(t, writes[1], tc.approval)
				assertEnd(t, writes[1], "step", 5, 3, "passed", nil)
			})
			t.Run("a run the release before started", func(t *testing.T) {
				writes := run(t, phase.Accepting, func(*pgxpool.Pool, *Engine) {})
				if len(writes) != 1 || writes[0].ID != StepID("LEGION-209", 3, placeOf("step", 5), 0) {
					t.Fatalf("the writes of LEGION-209's step 5 = %+v, want one end at the count 0", writes)
				}
				owners(t, writes[0], tc.before)
				assertEnd(t, writes[0], "step", 5, 3, "passed", nil)
			})
		})
	}
}

// A step running when this release deploys has no count of starts and no record in Dispatch. Its
// end still writes one record: the count 0, every identity field, and its start from the role's
// entry, so the route makes the node.
func TestAnIssueRunningAtTheUpgradeEndsWithOneRecord(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Testing)
	t0 := stepClock.Add(-3 * time.Hour)
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Entry: 2, EntryRaisedAt: t0})
	move(t, pool, stepEngine(nil), "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{})
	writes := stepWrites(t, pool, "LEGION-208")
	if len(writes) != 2 {
		t.Fatalf("step rows = %+v, want the end of testing and the start of reviewing", writes)
	}
	end := writes[0]
	assertEnd(t, end, "step", 3, 1, "passed", nil)
	if end.ID != StepID("LEGION-208", 1, placeOf("step", 3), 0) || end.StartedAt == nil || !end.StartedAt.Equal(t0) ||
		end.Role != "tester" || end.Type != "testing" || end.Agent != treeRoleToken(t, "LEGION-208", claim.RoleTester) {
		t.Fatalf("the end = %+v, want count 0, the tester's identity and its start at %s", end, t0)
	}
}

// A merge that ran under the release before ends with the agent and the moment its start would
// have named, and its merger's entry names them only when this visit to merging raised it. A merge
// GitHub had made when the retro's completion brought the issue to merging started no merger, so
// the entry its merger still holds is an earlier visit's, raised before that completion: that
// merge ends with no agent and no start. One whose merger this visit started ends under that
// merger, at its entry, which the move into merging raised (enter) after that completion, or at
// its very moment where the clock gave the two one reading. The release before left each root
// waiting in merging on a child, and the boot's sweep takes the merge (sweepMerged): the root
// waits in verifying, and the move ends the merge.
func TestAnUpgradedMergeNamesOnlyAMergerOfItsOwnVisit(t *testing.T) {
	retroCompleted := stepClock.Add(-2 * time.Hour)
	for _, tc := range []struct {
		name   string
		entry  time.Time
		merger bool
	}{
		{name: "the merger of an earlier visit", entry: stepClock.Add(-48 * time.Hour)},
		{name: "the merger of this visit, entered at the completion's moment", entry: retroCompleted, merger: true},
		{name: "the merger of this visit, entered after the completion", entry: retroCompleted.Add(time.Minute), merger: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			stepRoot(t, pool, phase.Merging)
			seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
				Branch: "legion/LEGION-208", HeadSHA: "head", MergeSHA: "merge", Failing: []string{}})
			stepChild(t, pool, "LEGION-209", phase.Testing, nil)
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Entry: 5, EntryRaisedAt: retroCompleted.Add(-time.Hour),
				CompletedAt: retroCompleted})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Entry: 2, EntryRaisedAt: tc.entry})
			applyFacts(t, pool, stepEngine(nil))("boot", intake.DaemonBooted{Boot: 1})
			if got := issueAt(t, pool, "LEGION-208"); got != phase.Verifying {
				t.Fatalf("LEGION-208 stands at %s after the boot took its merge, want verifying", got)
			}
			writes := stepWrites(t, pool, "LEGION-208")
			if len(writes) != 1 || writes[0].ID != StepID("LEGION-208", 1, placeOf("merge", 0), 0) {
				t.Fatalf("step rows = %+v, want the merge's one end at the count 0", writes)
			}
			end := writes[0]
			assertEnd(t, end, "merge", 0, 1, "passed", nil)
			if tc.merger && (end.Agent != treeRoleToken(t, "LEGION-208", claim.RoleMerger) || end.StartedAt == nil || !end.StartedAt.Equal(tc.entry)) {
				t.Fatalf("the merge's end = %+v, want the merger of this visit, started at its entry %s", end, tc.entry)
			}
			if !tc.merger && (end.Agent != "" || end.StartedAt != nil) {
				t.Fatalf("the merge's end = %+v, want no agent and no start: the merger's entry %s is an earlier visit's", end, tc.entry)
			}
		})
	}
}

// An issue at admitted runs no step: the architect works that phase (phase.RoleFor), and the
// task's first step is planning. So an end and a start of the step an issue at admitted stands in
// write nothing, and the opening of its gate (StartFirstPhase) writes the start of planning and no
// end. It is what the three readers of phase.RoleFor in steps.go answer for admitted
// (internal/textcheck's roleForReaders): startAgent and upgradedStart are never asked there, and
// endOfTransition's answer is read by no end.
func TestAnIssueAtAdmittedRunsNoStep(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Admitted)
	engine := stepEngine(nil)
	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		w, err := engine.walk(ctx, tx, root)
		if err != nil {
			return err
		}
		if err := engine.endStep(ctx, tx, root, w, stepStopped); err != nil {
			return err
		}
		return engine.StartStepRecord(ctx, tx, root)
	})
	if writes := stepWrites(t, pool, root.Key); len(writes) != 0 {
		t.Fatalf("an end and a start at admitted wrote %+v, want nothing", writes)
	}
	firstStep(t, pool, engine, root.Key)
	writes := stepWrites(t, pool, root.Key)
	if len(writes) != 1 || !isStart(writes[0]) || writes[0].Kind != "step" || writes[0].Step != 1 || writes[0].Role != "planner" {
		t.Fatalf("the move out of admitted wrote %+v, want the start of step 1 alone", writes)
	}
}
