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
)

// stepClock is the clock of every engine these tests build: each start and end they read is
// stamped with it.
var stepClock = time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)

// stepEngine is the engine the step record tests drive: no design gate, a fixed clock, and
// agent as the daemon's read of a step's agent (nil reads none).
func stepEngine(agent func(claim.Token) StepAgent) *Engine {
	return New(record.NewStore(), Config{Project: "LEGION", Steps: config.DesignGateOff.Steps(), ReviewRoundCap: 3, Linger: time.Hour,
		ProductionCheckRole: claim.RoleTester, ReviewAppLogin: testReviewApp, Repositories: testRepositories,
		Clock: func() time.Time { return stepClock }, StepAgent: agent}, nil)
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

// move runs one transition of key on trigger (into target, for a backward move) through engine,
// on the record as the store holds it, with handoff as the completion's row.
func move(t *testing.T, pool *pgxpool.Pool, engine *Engine, key string, trigger TriggerKind, target phase.Phase, handoff record.PhaseRow) {
	t.Helper()
	applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := engine.store.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		pr, err := engine.store.PullRequest(ctx, tx, key)
		if err != nil {
			return err
		}
		// A move no completion records (a conflict, a backward move) says why, as the engine
		// requires of its notice; the other moves carry the reason unread.
		return engine.transition(ctx, tx, *issue, trigger, target, handoff, pr, "the step test's move")
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

func token(t *testing.T, issue string, role claim.Role) string {
	t.Helper()
	got, err := claim.NewToken("legion", issue, role)
	if err != nil {
		t.Fatalf("token of %s %s: %v", issue, role, err)
	}
	return string(got)
}

func isStart(w record.StepRecord) bool { return w.StartedAt != nil && w.EndedAt == nil }
func isEnd(w record.StepRecord) bool   { return w.EndedAt != nil && w.Result != nil }

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
		// The restart: a new engine on a new store over the same database.
		move(t, pool, stepEngine(nil), "LEGION-208", TriggerTesterPassed, "", record.PhaseRow{})
		for _, w := range stepWrites(t, pool, "LEGION-208") {
			if w.Kind == "step" && w.Step == 3 && isStart(w) {
				start = w
			}
			if w.Kind == "step" && w.Step == 3 && isEnd(w) {
				end = w
			}
		}
		return start, end, pool
	}
	start, end, pool := run(t, func(*pgxpool.Pool) {})
	if want := StepID("LEGION-208", 1, "default", "step", 3, 1); start.ID != want {
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
	if end.Role != "reviewer" || end.Type != "review" || end.Agent != token(t, "LEGION-208", claim.RoleReviewer) {
		t.Fatalf("the review's end = %+v, want the reviewer's", end)
	}
	if !isStart(start) || start.Step != 2 || start.Role != "implementer" || start.ID != StepID("LEGION-208", 1, "default", "step", 2, 2) {
		t.Fatalf("the rejection's start = %+v, want the implementer's second start of step 2", start)
	}
	if first := writes[1]; !isStart(first) || first.Step != 2 || first.ID == start.ID {
		t.Fatalf("the first start of step 2 = %+v; want a start of another id than the second's %s", first, start.ID)
	}
}

// A head that conflicts with its base sends the work back with no report asking for it: the step
// it interrupts ends stopped, naming the step the work returns to.
func TestAConflictEndsTheStepStopped(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Testing)
	move(t, pool, stepEngine(nil), "LEGION-208", TriggerConflict, "", record.PhaseRow{})
	writes := stepWrites(t, pool, "LEGION-208")
	if len(writes) != 2 {
		t.Fatalf("step rows = %+v, want the end of testing and the start of implementing", writes)
	}
	assertEnd(t, writes[0], "step", 3, 1, "stopped", new(2))
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
	inStore := func(key string, write func(ctx context.Context, tx pgx.Tx, issue record.Issue) error) {
		t.Helper()
		applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			issue, err := engine.store.Issue(ctx, tx, key)
			if err != nil {
				return err
			}
			return write(ctx, tx, *issue)
		})
	}
	endTwice := func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		if err := engine.EndRunningStep(ctx, tx, issue); err != nil {
			return err
		}
		return engine.EndRunningStep(ctx, tx, issue)
	}
	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		inStore(key, endTwice)
		inStore(key, endTwice)
	}
	testingWrites := func(key string) (starts, ends []record.StepRecord) {
		for _, w := range stepWrites(t, pool, key) {
			switch {
			case w.Step != 3:
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
	if starts, ends := testingWrites("LEGION-209"); len(starts) != 0 || len(ends) != 1 || ends[0].ID != StepID("LEGION-209", 3, "default", "step", 3, 0) {
		t.Fatalf("four ends of a run no start counted wrote starts %+v and ends %+v, want one end at the count 0", starts, ends)
	}
	startTwice := func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		if err := engine.StartStepRecord(ctx, tx, issue); err != nil {
			return err
		}
		return engine.StartStepRecord(ctx, tx, issue)
	}
	inStore("LEGION-208", startTwice)
	inStore("LEGION-208", endTwice)
	starts, ends := testingWrites("LEGION-208")
	if len(starts) != 2 || len(ends) != 2 || starts[1].ID != StepID("LEGION-208", 1, "default", "step", 3, 2) || ends[1].ID != starts[1].ID {
		t.Fatalf("a start after the end, a second start and two ends wrote starts %+v and ends %+v, want the second run's one start and one end", starts, ends)
	}
}

// The end of a step carries the session and the model the daemon reads of its agent at that end,
// and the handoff record the ending role's completion named; the start carries the session alone.
// A daemon that reads no agent writes neither, which leaves them for a later write to fill.
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
	if start.Session == nil || *start.Session != "s1" || start.Model != nil || !start.StartedAt.Equal(stepClock) {
		t.Fatalf("the start = %+v, want session s1, no model, started at %s", start, stepClock)
	}
	if len(asked) != 2 || string(asked[0]) != token(t, "LEGION-208", claim.RoleTester) || string(asked[1]) != token(t, "LEGION-208", claim.RoleReviewer) {
		t.Fatalf("the agents asked = %v, want the tester's at the end, then the reviewer's at the start", asked)
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

// Two starts of a step raise no phase entry: accepting, whose role is the tree's architect, and
// merging on a pull request GitHub already merged, where no merger starts. Each still writes its
// start, and the merge's close ends it passed, under the agent its start named, none.
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
		if !isStart(start) || start.Step != 5 || start.Role != "acceptance" || start.Type != "review" || start.Agent != token(t, "LEGION-208", claim.RoleArchitect) {
			t.Fatalf("the start of accepting = %+v, want step 5, acceptance, review, the tree architect", start)
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
		if got := issueAt(t, pool, "LEGION-208"); got != phase.Done {
			t.Fatalf("LEGION-208 stands at %s, want done", got)
		}
	})
}

// A root's close lingers its tree and stops each member's step there, but not the merge of a member
// that awaits it: that merge lands later, and its close in the lingering tree ends the open record
// passed, the one end it gets.
func TestALingeringTreesMergeEndsTheMergeStep(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Reviewing)
	stepChild(t, pool, "LEGION-209", phase.Retro, nil)
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-209", HeadSHA: "head-43", Failing: []string{}})
	engine := stepEngine(nil)
	move(t, pool, engine, "LEGION-209", TriggerRetroCompleted, "", record.PhaseRow{})
	var merge string
	for _, w := range stepWrites(t, pool, "LEGION-209") {
		if w.Kind == "merge" && isStart(w) {
			merge = w.ID
		}
	}
	if merge == "" {
		t.Fatalf("LEGION-209's merge wrote no start: %+v", stepWrites(t, pool, "LEGION-209"))
	}
	applyFacts(t, pool, engine)("root-done", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
		Status: "done", Title: "root", Rank: "U"})
	for _, w := range stepWrites(t, pool, "LEGION-209") {
		if w.ID == merge && isEnd(w) {
			t.Fatalf("the linger ended LEGION-209's merge %+v; a merge that awaits its landing stays open", w)
		}
	}
	seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-209", HeadSHA: "head-43", MergeSHA: "merge-43", Failing: []string{}})
	applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		member, err := engine.store.Issue(ctx, tx, "LEGION-209")
		if err != nil {
			return err
		}
		pr, err := engine.store.PullRequest(ctx, tx, "LEGION-209")
		if err != nil {
			return err
		}
		return engine.closeMergedTelling(ctx, tx, *member, pr)
	})
	var ends []record.StepRecord
	for _, w := range stepWrites(t, pool, "LEGION-209") {
		if w.ID == merge && isEnd(w) {
			ends = append(ends, w)
		}
	}
	if len(ends) != 1 {
		t.Fatalf("ends of LEGION-209's merge = %+v, want exactly one", ends)
	}
	assertEnd(t, ends[0], "merge", 0, 3, "passed", nil)
	if !ends[0].EndedAt.Equal(stepClock) || ends[0].Agent != token(t, "LEGION-209", claim.RoleMerger) {
		t.Fatalf("the merge's end = %+v, want it ended at the merge's %s by the merger", ends[0], stepClock)
	}
	if got := issueAt(t, pool, "LEGION-209"); got != phase.Done {
		t.Fatalf("LEGION-209 stands at %s, want done", got)
	}
}

// A root's close stops every member's running step, each keyed on the phase that member stands in,
// and the root's own.
func TestALingeringRootEndsEachMembersStep(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Reviewing)
	stepChild(t, pool, "LEGION-209", phase.Testing, nil)
	stepChild(t, pool, "LEGION-210", phase.Held, &record.Hold{From: phase.Reviewing})
	applyFacts(t, pool, stepEngine(nil))("root-done", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
		Status: "done", Title: "root", Rank: "U"})
	for _, want := range []struct {
		issue      string
		step       int
		generation uint64
	}{{"LEGION-208", 4, 1}, {"LEGION-209", 3, 3}, {"LEGION-210", 4, 3}} {
		writes := stepWrites(t, pool, want.issue)
		if len(writes) != 1 {
			t.Fatalf("step rows of %s = %+v, want one end", want.issue, writes)
		}
		assertEnd(t, writes[0], "step", want.step, want.generation, "stopped", nil)
	}
}

// A held issue's step runs through its hold, so the issue's leave ends the step of the phase the
// hold is from.
func TestAHeldIssueThatLeavesEndsItsHeldStep(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Held)
	root.Hold = &record.Hold{From: phase.Testing}
	seedIssue(t, pool, root)
	applyFacts(t, pool, stepEngine(nil))("backlog", intake.DispatchIssue{Key: "LEGION-208", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
		Status: "backlog", Title: "root", Rank: "U"})
	writes := stepWrites(t, pool, "LEGION-208")
	if len(writes) != 1 {
		t.Fatalf("step rows = %+v, want one end", writes)
	}
	assertEnd(t, writes[0], "step", 3, 1, "stopped", nil)
}

// A person's move of a running issue out of the workflow stops its step, once.
func TestAPersonsMoveToDoneEndsTheRunningStep(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Reviewing)
	stepChild(t, pool, "LEGION-209", phase.Reviewing, nil)
	applyFacts(t, pool, stepEngine(nil))("child-done", intake.DispatchIssue{Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
		Status: "done", Title: "LEGION-209", Parent: "LEGION-208", Rank: "V"})
	writes := stepWrites(t, pool, "LEGION-209")
	if len(writes) != 1 {
		t.Fatalf("step rows = %+v, want one end", writes)
	}
	assertEnd(t, writes[0], "step", 4, 3, "stopped", nil)
}

// A child set back to todo opens its next run; the step of the run it leaves is stopped, and the
// new run, at admitted, runs none yet.
func TestAReenteredChildEndsItsOldRun(t *testing.T) {
	pool := migratedPool(t)
	stepRoot(t, pool, phase.Implementing)
	child := stepChild(t, pool, "LEGION-209", phase.Implementing, nil)
	child.Status = "todo"
	engine := stepEngine(nil)
	applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		return engine.ReenterChild(ctx, tx, child, RecordedFact(child))
	})
	writes := stepWrites(t, pool, "LEGION-209")
	if len(writes) != 1 {
		t.Fatalf("step rows = %+v, want one end of the old run", writes)
	}
	assertEnd(t, writes[0], "step", 2, 3, "stopped", nil)
	if got := issueRecord(t, pool, "LEGION-209"); got == nil || got.Phase != phase.Admitted || got.Generation != 4 {
		t.Fatalf("LEGION-209 = %+v, want admitted at generation 4", got)
	}
}

// An adopted member takes its next generation in the tree it joins; the step of the run it leaves
// is stopped.
func TestAnAdoptedMemberEndsItsOldRun(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Implementing)
	member := record.Issue{Key: "LEGION-300", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-300", Project: "LEGION",
		Title: "queued", Phase: phase.Implementing, Generation: 2, Status: "in_progress", Rank: "W", LastDispatchSeq: 1}
	seedIssue(t, pool, member)
	engine := stepEngine(nil)
	applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		return engine.moveIntoTree(ctx, tx, member, root, intake.AdoptedChild{})
	})
	writes := stepWrites(t, pool, "LEGION-300")
	if len(writes) != 1 {
		t.Fatalf("step rows = %+v, want one end of the old run", writes)
	}
	assertEnd(t, writes[0], "step", 2, 2, "stopped", nil)
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
	if end.ID != StepID("LEGION-208", 1, "default", "step", 3, 0) || end.StartedAt == nil || !end.StartedAt.Equal(t0) ||
		end.Role != "tester" || end.Type != "testing" || end.Agent != token(t, "LEGION-208", claim.RoleTester) {
		t.Fatalf("the end = %+v, want count 0, the tester's identity and its start at %s", end, t0)
	}
}

// An issue at admitted runs no step: the architect works that phase (phase.RoleFor), and the
// task's first step is planning. So an end and a start of the step an issue at admitted stands in
// write nothing, and the move out of admitted writes the start of planning and no end, the
// architect's own completion beside it or not. It is what the three readers of phase.RoleFor in
// steps.go answer for admitted (internal/textcheck's roleForReaders): startAgent and upgradedStart
// are never asked there, and endOfTransition's answer is read by no end.
func TestAnIssueAtAdmittedRunsNoStep(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Admitted)
	engine := stepEngine(nil)
	applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := engine.EndRunningStep(ctx, tx, root); err != nil {
			return err
		}
		return engine.StartStepRecord(ctx, tx, root)
	})
	if writes := stepWrites(t, pool, root.Key); len(writes) != 0 {
		t.Fatalf("an end and a start at admitted wrote %+v, want nothing", writes)
	}
	move(t, pool, engine, root.Key, TriggerGateOpened, "", record.PhaseRow{Issue: root.Key, Role: claim.RoleArchitect, HandoffRecord: "architect-handoff"})
	writes := stepWrites(t, pool, root.Key)
	if len(writes) != 1 || !isStart(writes[0]) || writes[0].Kind != "step" || writes[0].Step != 1 || writes[0].Role != "planner" {
		t.Fatalf("the move out of admitted wrote %+v, want the start of step 1 alone", writes)
	}
}
