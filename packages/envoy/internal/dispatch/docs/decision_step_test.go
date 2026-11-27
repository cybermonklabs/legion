package docs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// catchUpCommit is the commit the step of these tests serves. No process is registered, so
// processes.Live lists none and the fleet reads as one build: a step's first tick is the final pass
// its catch-up owes.
const catchUpCommit = "1150115011501150115011501150115011501150"

// catchUpFixCommit is another commit of this release, as a fix rolled forward after a rollback
// serves; the release before serves previousReleaseCommit (graph_previous_release_version_test.go).
const catchUpFixCommit = "5570557055705570557055705570557055705570"

// catchUpAlice is the person who answers, resolves and follows in these tests.
var catchUpAlice = model.Actor{Kind: "user", ID: "alice"}

// catchUpSession is the session the release before puts on an ask as a follower.
const catchUpSession = "session-catch-up-follower"

// seedUnmarkedAsk writes project, its issue <project>-1 and an open ask on that issue as the
// release before writes them: no node for any of them, and no marker on the ask's row, so phase 1
// owes them (graphcensus.AsksOwed). It answers the ask's id and its issue.
func seedUnmarkedAsk(t *testing.T, database *store.Store, project string) (string, string) {
	t.Helper()
	ctx := context.Background()
	issue := project + "-1"
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ($1, $1, 'alice')
	`, project); err != nil {
		t.Fatalf("seed project %s: %v", project, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'One', '{"kind":"user","id":"alice"}', 'U')
	`, issue, project); err != nil {
		t.Fatalf("seed issue %s: %v", issue, err)
	}
	var ask string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question) values ($1, '{"kind":"user","id":"alice"}', 'Ship it?')
		returning id::text
	`, issue).Scan(&ask); err != nil {
		t.Fatalf("seed the ask of %s: %v", issue, err)
	}
	return ask, issue
}

// askMarked is whether ask's row names its node, as phase 1 marks it.
func askMarked(t *testing.T, database *store.Store, ask string) bool {
	t.Helper()
	var marked bool
	if err := database.Pool.QueryRow(context.Background(), `select node_kind is not null from asks where id = $1::uuid`, ask).Scan(&marked); err != nil {
		t.Fatalf("read whether ask %s names its node: %v", ask, err)
	}
	return marked
}

// seedCatchUpAsk writes project, its issue <project>-1 and an open ask on that issue as the release
// before writes them (seedUnmarkedAsk), then runs the boot step's backfill
// (graphcensus.BackfillAsks), which writes the ask's node and marks its row, as it marks every row
// that release wrote. It answers the ask's id and its issue.
func seedCatchUpAsk(t *testing.T, database *store.Store, project string) (string, string) {
	t.Helper()
	ask, issue := seedUnmarkedAsk(t, database, project)
	if _, err := graphcensus.BackfillAsks(context.Background(), database); err != nil {
		t.Fatalf("state the ask of %s on its node: %v", issue, err)
	}
	if !askMarked(t, database, ask) {
		t.Fatalf("the ask of %s names no node, want the backfill to have marked it", issue)
	}
	return ask, issue
}

// asThePreviousBuild runs write as a write of the release before on issue's ask: under the
// issue's owner lock, with the event write answers appended through the broker, which takes the
// issue's row in that append, and nothing stated on the store, which that release's writers do not
// know. It answers the event's id.
func asThePreviousBuild(t *testing.T, database *store.Store, issue string, write func(ctx context.Context, tx pgx.Tx) (model.Event, error)) int64 {
	t.Helper()
	return commitAsThePreviousBuild(t, beginAsThePreviousBuild(t, database, issue), issue, write)
}

// beginAsThePreviousBuild opens a write of the release before on issue and takes the issue's
// owner lock, as that release's writers of an ask take it first (store.LockOwner: the release
// before serialises an owner's writers on that lock and takes the issue's row only in its event's
// append); commitAsThePreviousBuild writes in it and commits.
func beginAsThePreviousBuild(t *testing.T, database *store.Store, issue string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the previous build's write: %v", err)
	}
	testcleanup.Add(t, func() { _ = tx.Rollback(context.Background()) })
	if err := store.LockOwner(ctx, tx, store.IssueOwner(issue)); err != nil {
		t.Fatalf("take issue %s's owner lock: %v", issue, err)
	}
	return tx
}

// commitAsThePreviousBuild runs write in tx, a write beginAsThePreviousBuild opened on issue,
// appends the event write answers through the broker and commits, and answers the event's id.
func commitAsThePreviousBuild(t *testing.T, tx pgx.Tx, issue string, write func(ctx context.Context, tx pgx.Tx) (model.Event, error)) int64 {
	t.Helper()
	ctx := context.Background()
	event, err := write(ctx, tx)
	if err != nil {
		t.Fatalf("write as the previous build: %v", err)
	}
	event.IssueKey = &issue
	appended, err := events.NewBroker().Append(ctx, tx, event)
	if err != nil {
		t.Fatalf("append the previous build's %s: %v", event.Type, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the previous build's write: %v", err)
	}
	return appended.ID
}

// answerAsThePreviousBuild answers ask on issue as the answer route of the release before does:
// the asks row and its ask.answered, and nothing on the node.
func answerAsThePreviousBuild(t *testing.T, database *store.Store, issue, ask, selected string) int64 {
	t.Helper()
	return asThePreviousBuild(t, database, issue, previousBuildAnswer(ask, selected))
}

// previousBuildAnswer is the write of the release before's answer route that answers ask with
// selected, a first answer or a change to the one it holds.
func previousBuildAnswer(ask, selected string) func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
	return func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		answer, err := json.Marshal(model.AskAnswer{User: catchUpAlice.ID, Selected: []string{selected}, At: time.Now().UTC()})
		if err != nil {
			return model.Event{}, err
		}
		if _, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1::uuid`, ask, answer); err != nil {
			return model.Event{}, err
		}
		answered, err := ScanAsk(tx.QueryRow(ctx, `select `+AskColumns+` from asks a where a.id = $1::uuid`, ask))
		if err != nil {
			return model.Event{}, err
		}
		return model.Event{Type: "ask.answered", Actor: catchUpAlice, Payload: model.AskAnsweredEventPayload{
			AskEventPayload: model.NewAskEventPayload(answered, model.ReferenceChanges{}),
		}}, nil
	}
}

// followAsThePreviousBuild puts catchUpSession on ask as the follow route of the release before
// does: the follower row and its ask.follower_added, and nothing on the node.
func followAsThePreviousBuild(t *testing.T, database *store.Store, issue, ask string) int64 {
	t.Helper()
	return asThePreviousBuild(t, database, issue, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
		if _, err := tx.Exec(ctx, `insert into ask_followers (ask_id, session_id) values ($1::uuid, $2)`, ask, catchUpSession); err != nil {
			return model.Event{}, err
		}
		return model.Event{Type: "ask.follower_added", Actor: catchUpAlice, Payload: model.AskFollowerEventPayload{
			AskID: ask, SessionID: catchUpSession, By: catchUpAlice,
		}}, nil
	})
}

// retractAsThisBuild retracts ask on issue as this build's settlement retracts an ask whose block
// left its document: through the one writer of an ask's answer state (asks.Settle), resolved with
// no answer, its facts stated under the ask.resolved that records it. It answers that event's id.
func retractAsThisBuild(t *testing.T, database *store.Store, issue, ask string) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin this build's retraction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select 1 from issues where key = $1 for no key update`, issue); err != nil {
		t.Fatalf("take issue %s's row: %v", issue, err)
	}
	resolution := model.AskResolution{Kind: "retracted", Reason: SettlementRetractionReason + " 2", Actor: SettlementActor, At: time.Now().UTC()}
	var held graphstore.Uncaused
	change := asks.Recorded(asks.StateResolved, nil, &resolution)
	change.Who, change.At, change.Held = SettlementActor, resolution.At, &held
	if _, err := asks.Settle(ctx, tx, ask, change); err != nil {
		t.Fatalf("retract ask %s: %v", ask, err)
	}
	retracted, err := ScanAsk(tx.QueryRow(ctx, `select `+AskColumns+` from asks a where a.id = $1::uuid`, ask))
	if err != nil {
		t.Fatalf("read the retracted ask: %v", err)
	}
	appended, err := events.NewBroker().Append(ctx, tx, model.Event{
		IssueKey: &issue, Type: "ask.resolved", Actor: SettlementActor,
		Payload: model.NewAskEventPayload(retracted, model.ReferenceChanges{}),
	})
	if err != nil {
		t.Fatalf("append the ask.resolved: %v", err)
	}
	if err := graphstore.Caused(ctx, tx, held, appended.ID); err != nil {
		t.Fatalf("state the retraction under its event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit this build's retraction: %v", err)
	}
	return appended.ID
}

// newestAnswerState is the newest answer_state value ask's node holds and the event it is stated
// under, empty and 0 where it holds none.
func newestAnswerState(t *testing.T, database *store.Store, ask string) (string, int64) {
	t.Helper()
	var state string
	var event int64
	err := database.Pool.QueryRow(context.Background(), `
		select to_id, source_event from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4
		order by seq desc limit 1
	`, graphstore.KindAsk, ask, graphstore.TupleAnswerState, graphstore.KindValue).Scan(&state, &event)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0
	}
	if err != nil {
		t.Fatalf("read the answer state of ask %s's node: %v", ask, err)
	}
	return state, event
}

// backendOf is the backend process id of a one-connection store's connection (storetest.
// SingleConnection), which every statement of a run on it goes through.
func backendOf(t *testing.T, runner *store.Store) uint32 {
	t.Helper()
	var pid uint32
	if err := runner.Pool.QueryRow(context.Background(), `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the run's backend: %v", err)
	}
	return pid
}

// startCatchUp runs one pass of step's catch-up off the test's goroutine and answers the channel
// its answer arrives on: its error, or one naming the ask that failed.
func startCatchUp(step *DecisionStep) <-chan error {
	done := make(chan error, 1)
	go func() {
		failed, err := step.catchUp(context.Background())
		if err == nil && failed {
			err = errors.New("the catch-up failed on an ask")
		}
		done <- err
	}()
	return done
}

// The catch-up takes the store's lock order (api/inbox_read.go's markInboxReadThreads) over the ask
// it states an event on: the ask's owner lock, its issue's, before the ask's row, as every writer
// of an answer takes them, and then the nodes and tuples asks.Settle writes. A catch-up that locked
// the ask's row first could hold it while it waited for a node a settlement of the issue held, and
// that settlement, holding the issue, waits for the ask's row.
//
// The previous build answers an ask this build marked, and the issue's owner lock and the ask's row
// are each held in a transaction of its own while one pass of the real catch-up runs on a
// connection of its own (lockstep.Follow): the pass must queue behind the issue before the ask, and
// never behind the project's row, since it appends no event. The ask's node then holds the answer
// under that build's ask.answered, so the run followed is the one that committed.
func TestTheCatchUpTakesTheAsksOwnerRowBeforeItsRow(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ask, issue := seedCatchUpAsk(t, database, "CATCH")
	answered := answerAsThePreviousBuild(t, database, issue, ask, "Ship")
	runner := storetest.SingleConnection(t, database)
	step := NewDecisionStep(runner, catchUpCommit, nil)
	pid := backendOf(t, runner)

	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{
		Name: "issue " + issue + "'s owner lock", Step: lockstep.StepOwner, Taken: true, Kind: "issue", ID: issue,
		Statement: lockstep.OwnerLockStatement,
		Unlocked:  []string{"asks", "graph_nodes", "graph_tuples"},
	}, store.IssueOwner(issue))
	row := lockstep.HoldRow(t, database, &lockstep.Hold{
		Name: "the ask's row", Step: lockstep.StepHeld, Taken: true,
		Statement: "select node_kind is not null, coalesce(issue_key, ''), coalesce(artifact_id::text, '') from asks",
		Unlocked:  []string{"graph_nodes", "graph_tuples"},
	}, `select 1 from asks where id = $1::uuid for update`, ask)
	project := lockstep.HoldRow(t, database, &lockstep.Hold{Name: "project CATCH's row", Step: lockstep.StepEvent},
		`select 1 from projects where key = $1 for no key update`, "CATCH")

	lockstep.Follow(t, database, "the catch-up", pid, startCatchUp(step), []*lockstep.Hold{owner, row, project})

	if state, event := newestAnswerState(t, database, ask); state != asks.StateAnswered || event != answered {
		t.Errorf("the ask's node holds answer_state %q under event %d, want answered under the previous build's ask.answered %d: the run followed is not one that committed",
			state, event, answered)
	}
}

// A catch-up pass reads an ask's events before it takes any lock, so the previous build can commit
// a newer write on the ask in between: that write holds the ask's issue, which the pass waits for.
// The pass then states the newer write too, read under its locks, so the row it writes and the
// node it states are the newer write's, and never the older event's written over it.
//
// The previous build answers an ask this build marked, Ship. A transaction of the test's own then
// opens the previous build's change of that answer and takes the ask's issue, and one pass of the
// real catch-up runs on a connection of its own: it reads the window, Ship among it, and queues
// behind the issue. The previous build changes the answer to Hold under its own ask.answered and
// commits, and the pass goes on. The ask's row and its node then both hold Hold, the node's
// answer_state under that newer ask.answered.
func TestTheCatchUpStatesAWriteCommittedBetweenItsReadAndItsLock(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	ask, issue := seedCatchUpAsk(t, database, "GAP")
	answerAsThePreviousBuild(t, database, issue, ask, "Ship")
	runner := storetest.SingleConnection(t, database)
	step := NewDecisionStep(runner, catchUpCommit, nil)
	pid := backendOf(t, runner)

	change := beginAsThePreviousBuild(t, database, issue)
	done := startCatchUp(step)
	if failed, answered := storetest.WaitForQueuedBehind(t, change, pid, done); answered {
		t.Fatalf("the pass answered (%v) without waiting on the issue the previous build's change holds", failed)
	}
	changed := commitAsThePreviousBuild(t, change, issue, previousBuildAnswer(ask, "Hold"))
	if err := <-done; err != nil {
		t.Fatalf("run the pass: %v", err)
	}

	if state, event := newestAnswerState(t, database, ask); state != asks.StateAnswered || event != changed {
		t.Errorf("the ask's node holds answer_state %q under event %d, want answered under the previous build's newer ask.answered %d",
			state, event, changed)
	}
	var row, node string
	if err := database.Pool.QueryRow(ctx, `
		select k.answer->>'selected',
		       coalesce((select t.to_id from graph_tuples t
		                 where t.from_kind = $2 and t.from_id = k.id::text and t.kind = $3 and t.to_kind = $4
		                 order by t.seq desc limit 1), '')
		from asks k where k.id = $1::uuid
	`, ask, graphstore.KindAsk, graphstore.TupleSelected, graphstore.KindValue).Scan(&row, &node); err != nil {
		t.Fatalf("read the ask's selection on its row and its node: %v", err)
	}
	if want := asks.SelectedValue([]string{"Hold"}); row != want || node != want {
		t.Errorf("the ask's row selects %s and its node %s, want both the newer write's %s", row, node, want)
	}
}

// failFirstAskWrite is a trigger of the test's own that fails the first write of an asks row once
// it is installed and lets every later one through: a sequence moves whatever the transaction that
// draws from it does, so the failure does not come back when that transaction rolls back.
const failFirstAskWrite = `
	create sequence fail_first_ask_write;
	create function fail_first_ask_write() returns trigger language plpgsql as $$
	begin
		if nextval('fail_first_ask_write') = 1 then
			raise exception 'the first write of an asks row fails';
		end if;
		return new;
	end $$;
	create trigger fail_first_ask_write before update on asks
		for each row execute function fail_first_ask_write();`

// A catch-up whose apply fails on the final pass - the first tick that finds the fleet one build,
// after which no other build's write is left to catch up - is applied on the next tick: the failure
// keeps the pass owed, as every failure of a pass does, so the previous build's answer reaches the
// ask's node once the write that failed goes through.
//
// The previous build answers an ask this build marked. No process is registered, so the step's
// first tick is its final pass, and its one write of the asks row (asks.Settle) fails once. The
// node holds no answer state under that build's ask.answered after that tick, and holds the answer
// under it after the next.
func TestACatchUpWhoseApplyFailsOnTheFinalPassIsAppliedOnTheNextTick(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	ask, issue := seedCatchUpAsk(t, database, "FINAL")
	answered := answerAsThePreviousBuild(t, database, issue, ask, "Ship")
	if _, err := database.Pool.Exec(ctx, failFirstAskWrite); err != nil {
		t.Fatalf("install the trigger that fails the first write of an asks row: %v", err)
	}
	step := NewDecisionStep(database, catchUpCommit, nil)

	if err := step.Tick(ctx); err != nil {
		t.Fatalf("run the final pass: %v", err)
	}
	if state, event := newestAnswerState(t, database, ask); event == answered {
		t.Fatalf("the ask's node holds answer_state %q under the previous build's ask.answered %d after the tick whose write failed, want the failure to have rolled it back",
			state, answered)
	}
	if err := step.Tick(ctx); err != nil {
		t.Fatalf("run the tick after the final pass: %v", err)
	}
	if state, event := newestAnswerState(t, database, ask); state != asks.StateAnswered || event != answered {
		t.Errorf("the ask's node holds answer_state %q under event %d after the next tick, want answered under the previous build's ask.answered %d",
			state, event, answered)
	}
}

// A catch-up pass never reads again an event a pass before it read, where no ask failed on that
// pass: an event that needs nothing states nothing, so no tuple ever names it, and a pass that read
// the window from where it opens would take its ask's row again on every tick. The step's
// high-water mark is what it read instead, and a later event is read all the same.
//
// Two events of the previous build need nothing: its answer of an ask this build's settlement
// retracted after it, which is older than the node's newest answer_state, and its follow of another
// ask, whose row the backfill already stated. The first pass reads both and states nothing. Each
// ask's row is then held in a transaction of its own, and the second pass must answer without
// queueing behind either. The previous build then answers the second ask, and the third pass
// states that answer.
func TestACatchUpPassNeverReadsAgainAnEventAPassBeforeItRead(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	retracted, retractedIssue := seedCatchUpAsk(t, database, "SKIP")
	answerAsThePreviousBuild(t, database, retractedIssue, retracted, "Ship")
	retraction := retractAsThisBuild(t, database, retractedIssue, retracted)
	followed, followedIssue := seedCatchUpAsk(t, database, "FOLLOW")
	followAsThePreviousBuild(t, database, followedIssue, followed)
	if _, err := graphcensus.BackfillAsks(ctx, database); err != nil {
		t.Fatalf("state the previous build's follower row: %v", err)
	}
	runner := storetest.SingleConnection(t, database)
	step := NewDecisionStep(runner, catchUpCommit, nil)
	pid := backendOf(t, runner)

	if err := <-startCatchUp(step); err != nil {
		t.Fatalf("run the first pass: %v", err)
	}
	if state, event := newestAnswerState(t, database, retracted); state != asks.StateResolved || event != retraction {
		t.Fatalf("the retracted ask's node holds answer_state %q under event %d after the first pass, want resolved under this build's ask.resolved %d: the older answer was put back",
			state, event, retraction)
	}

	holds := map[string]*lockstep.Hold{}
	for name, ask := range map[string]string{"the retracted ask's row": retracted, "the followed ask's row": followed} {
		holds[name] = lockstep.HoldRow(t, database, &lockstep.Hold{Name: name, Step: lockstep.StepHeld},
			`select 1 from asks where id = $1::uuid for update`, ask)
	}
	done := startCatchUp(step)
	deadline := time.Now().Add(lockstep.Wait)
	for answered := false; !answered; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("run the second pass: %v", err)
			}
			answered = true
			continue
		default:
		}
		for name, hold := range holds {
			queued, err := storetest.QueuedBehind(ctx, hold.Tx, pid)
			if err != nil {
				t.Fatalf("read what the second pass waits on: %v", err)
			}
			if queued {
				for _, release := range holds {
					release.Release()
				}
				<-done
				t.Fatalf("the second pass queued behind %s: it read again an event the first pass had read", name)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second pass did not answer within %s", lockstep.Wait)
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, hold := range holds {
		hold.Release()
	}

	late := answerAsThePreviousBuild(t, database, followedIssue, followed, "Ship")
	if err := <-startCatchUp(step); err != nil {
		t.Fatalf("run the third pass: %v", err)
	}
	if state, event := newestAnswerState(t, database, followed); state != asks.StateAnswered || event != late {
		t.Errorf("the followed ask's node holds answer_state %q under event %d after the third pass, want answered under the previous build's ask.answered %d, appended after the mark",
			state, event, late)
	}
}

// The catch-up's reads reach every table through an index keyed on what they bind. The read of a
// pass (caughtUpRead) reads events through their primary key from the high-water mark on, each
// event's owner by its key, and each event's tuples through an index of graph_tuples keyed on the
// ask's node - its kind and its id. The read an ask's catch-up makes under its locks
// (caughtUpSince) reads its owner's events through the owner's unique (key, seq) index from the
// sequence the pass's read saw, and each one's tuples the same way. None is a read that no index
// condition narrows, nor one of every ask's tuples, which on a store of millions of events and
// tuples is all of them on every tick.
func TestTheCatchUpReadsItsWindowThroughIndexes(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	keys := explaintest.NewIndexKeys(database.Pool)
	const ask = "00000000-0000-4000-8000-0000000000c1"
	for _, read := range []struct {
		name, index string
		plan        func() json.RawMessage
	}{
		{"the catch-up's read of the window", "events_pkey", func() json.RawMessage {
			return explainPlan(t, database, caughtUpRead, int64(1000), []int64{1001})
		}},
		{"the catch-up's read of an issue's ask under its locks", "events_issue_key_seq_key", func() json.RawMessage {
			return explainPlan(t, database, caughtUpSinceOnIssue, "CORE-1", int64(3), ask)
		}},
		{"the catch-up's read of a document's ask under its locks", "events_artifact_id_seq_key", func() json.RawMessage {
			return explainPlan(t, database, caughtUpSinceOnDocument, "00000000-0000-4000-8000-0000000000d1", int64(3), ask)
		}},
	} {
		plan := read.plan()
		if unkeyed := explaintest.ScansWithoutIndexCond(t, plan); len(unkeyed) > 0 {
			t.Errorf("%s reads %v, which no index condition keys:\n%s", read.name, unkeyed, plan)
		}
		keys.RequireIndexKeyed(t, plan, read.index, read.name, explaintest.CondRequired)
		keys.RequireIndexKeyed(t, plan, explaintest.AnyIndex, read.name, explaintest.CondRequired)
		probed := false
		for _, scan := range explaintest.IndexScans(t, plan) {
			if strings.HasPrefix(scan.Index, "graph_tuples") {
				if !strings.Contains(scan.Cond, "from_id") {
					t.Errorf("%s reads %s on `%s`, which does not name the ask's node:\n%s", read.name, scan, scan.Cond, plan)
				}
				probed = true
			}
		}
		if !probed {
			t.Errorf("%s reads no graph_tuples index, want each event's tuples probed through one:\n%s", read.name, plan)
		}
	}
}

// storeGatedProject stores project key with a list of gated steps that holds `decisions`, as the
// project route of a build that admits the step stores it: the row and its project.created event.
func storeGatedProject(t *testing.T, database *store.Store, key string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the project's write: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into projects (key, name, default_assignee, gated_steps) values ($1, $1, 'alice', '{decisions,spec}')
	`, key); err != nil {
		t.Fatalf("store a list of gated steps holding decisions: %v", err)
	}
	if _, err := events.NewBroker().Append(ctx, tx, model.Event{
		ProjectKey: &key, Type: "project.created", Actor: catchUpAlice, Payload: map[string]string{"key": key},
	}); err != nil {
		t.Fatalf("append the project.created of %s: %v", key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the project's write: %v", err)
	}
}

// The step's tick never stops for want of work: a process of another build can go live after a
// tick found nothing to do - a rollback, or a process of the release before that starts late - and
// only a tick that still runs catches up what it writes. /healthz goes on answering the last
// tick's count (Owed).
//
// Run ticks on an empty database until its catch-up's final pass is behind it and its ticks find
// nothing to do. It is still running after many such ticks, and once a project stores a list of
// gated steps that holds `decisions`, with the event its write appends, a later tick counts it.
func TestTheDecisionsStepKeepsTickingAfterATickWithNoWork(t *testing.T) {
	database := storetest.Open(t)
	step := NewDecisionStep(database, catchUpCommit, nil)
	step.interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		step.Run(ctx)
		close(stopped)
	}()
	testcleanup.Add(t, func() {
		cancel()
		<-stopped
	})
	deadline := time.Now().Add(10 * time.Second)
	for step.Owed() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the step's first tick counted nothing within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-stopped:
		t.Fatal("Run returned once its ticks found nothing to do, want it to keep ticking")
	case <-time.After(50 * step.interval):
	}
	storeGatedProject(t, database, "GATE")
	for deadline = time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if owed := step.Owed(); owed.GatedStepsLists == 1 && owed.Total == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the step's ticks count %+v 10s after a list of gated steps holding decisions was stored, want gated_steps_lists 1 and total 1",
				*step.Owed())
		}
	}
}

// wholeTableRead matches a statement that reads one of the tables a probe of the step reads to the
// end when nothing is owed: the object kinds and the asks graphcensus.AsksOwed reads, and the
// documents, asks and projects the count of what the restore and the conversion owe reads.
var wholeTableRead = regexp.MustCompile(`\b(from|join)\s+(issues|artifacts|asks|ask_followers|messages|comments|projects)\b`)

// A tick with nothing owed and no other build live reads no table whole: it reads the live
// processes, the newest event and whether a document's block index is owed, each one probe of an
// index, and leaves the last count standing - every 30 s on every process, for as long as each
// runs. graphcensus.AsksOwed and the count each read whole tables when nothing is owed, so neither
// runs on such a tick.
//
// The step boots on an empty database, runs the tick that takes its catch-up's final pass and the
// one that counts after it, and then three more ticks, every statement of which a tracer on the
// step's pool records. None of those three reads a table of the probes', and the step's count still
// stands at nothing owed. A project then stores a list of gated steps holding `decisions`, with
// the event its write appends, and the next tick counts it.
func TestATickWithNothingOwedReadsNoTableWhole(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	recorder := &storetest.StatementRecorder{}
	traced := storetest.Reopened(t, database, func(config *pgxpool.Config) { config.ConnConfig.Tracer = recorder })
	step := NewDecisionStep(traced, catchUpCommit, nil)
	if err := step.Boot(ctx); err != nil {
		t.Fatalf("boot the step: %v", err)
	}
	for range 2 {
		if err := step.Tick(ctx); err != nil {
			t.Fatalf("run a tick of the step: %v", err)
		}
	}
	before := len(recorder.Statements())
	for range 3 {
		if err := step.Tick(ctx); err != nil {
			t.Fatalf("run a tick with nothing owed: %v", err)
		}
	}
	idle := recorder.Statements()[before:]
	if len(idle) == 0 {
		t.Fatal("three ticks with nothing owed ran no statement the tracer saw, want the probes of each")
	}
	for _, statement := range idle {
		if read := wholeTableRead.FindString(strings.ToLower(statement)); read != "" {
			t.Errorf("a tick with nothing owed runs a statement that reads %q to the end:\n%s", read, statement)
		}
	}
	if owed := step.Owed(); owed == nil || owed.Total != 0 {
		t.Errorf("the step's count reads %+v after the ticks with nothing owed, want it standing at 0", owed)
	}

	storeGatedProject(t, database, "IDLE")
	if err := step.Tick(ctx); err != nil {
		t.Fatalf("run the tick after a project's write: %v", err)
	}
	if owed := step.Owed(); owed == nil || owed.GatedStepsLists != 1 || owed.Total != 1 {
		t.Errorf("the step's count reads %+v after a project stored a list of gated steps holding decisions, want gated_steps_lists 1 and total 1", owed)
	}
}

// specVersions is how many versions document holds.
func specVersions(t *testing.T, database *store.Store, document string) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, document).Scan(&count); err != nil {
		t.Fatalf("count the versions of %s: %v", document, err)
	}
	return count
}

// A tick runs no pass of phase 3 after a catch-up that failed:
// the restore's finder reads an ask's answer from its node, which an answer of another build that
// the catch-up did not state there leaves unanswered, so the restore waits for a tick whose
// catch-up went through, as RunByHand refuses to run it after one that failed.
//
// An answered ask's block left issue DOC-1's spec, which phase 3 owes, and the previous build
// answers an ask of another issue that this build marked, whose catch-up's one write of the asks
// row fails once. The first tick's catch-up fails, and the tick writes no version of the spec; the
// next tick's catch-up goes through, and its phase 3 restores the ask.
func TestATickRunsNoRestoreAfterACatchUpThatFailed(t *testing.T) {
	ctx := context.Background()
	issue := newRestoreIssue(t, "# Spec\n\n:::ask{#q-tick urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n")
	issue.answer(issue.spec, "q-tick", "The base release.")
	issue.replace(issue.spec, "# Spec\n\nThe corpus.\n")
	caught, other := seedCatchUpAsk(t, issue.database, "CATCH")
	answered := answerAsThePreviousBuild(t, issue.database, other, caught, "Ship")
	if _, err := issue.database.Pool.Exec(ctx, failFirstAskWrite); err != nil {
		t.Fatalf("install the trigger that fails the first write of an asks row: %v", err)
	}
	before := specVersions(t, issue.database, issue.spec)
	step := NewDecisionStep(issue.database, catchUpCommit, nil)
	step.UseDocuments(issue.service)

	if err := step.Tick(ctx); err != nil {
		t.Fatalf("run the tick whose catch-up fails: %v", err)
	}
	if _, event := newestAnswerState(t, issue.database, caught); event == answered {
		t.Fatalf("the first tick's catch-up stated the previous build's ask.answered %d, want its write to have failed", answered)
	}
	if owed := step.Owed(); owed == nil || owed.AnswersLeftSpec != 1 {
		t.Fatalf("the step's count reads %+v, want the spec's answer that left it owed", owed)
	}
	if got := specVersions(t, issue.database, issue.spec); got != before {
		t.Fatalf("the tick whose catch-up failed wrote the spec %d versions, want none: no restore after a failed catch-up", got-before)
	}
	if err := step.Tick(ctx); err != nil {
		t.Fatalf("run the tick after the failed catch-up: %v", err)
	}
	if state, event := newestAnswerState(t, issue.database, caught); state != asks.StateAnswered || event != answered {
		t.Fatalf("the next tick's catch-up left answer_state %q under event %d, want answered under the previous build's ask.answered %d", state, event, answered)
	}
	if spec := issue.latest(issue.spec); !strings.Contains(spec, "#q-tick urgency=") {
		t.Fatalf("the tick after a catch-up that went through restored no ask:\n%s", spec)
	}
}

// A booting process waits for the step's lock while phase 1 owes a row, so every process has
// phase 1 behind it before it serves, and for nothing else of the step's (cmd/dispatch's boot):
// a pass of phase 3 - every issue the restore and the conversion
// owe, one transaction each - holds a lock of its own (decisionRestoreLock) and not the step's,
// which a tick lets go of once phases 1 and 2 are behind it.
//
// newRestoreIssue writes issue DOC-1 and its spec as the release before left them, naming no
// node, and phase 1's pass over documents takes each one's issue row (graphcensus'
// lockObjectOwners), so phase 1 states them before the test's own transaction takes that row: a
// tick would otherwise wait on it in phase 1, under the step's lock. A tick's pass of phase 3 then
// queues behind the row. An ask of another issue then stands as the release before writes it, so
// a second process's boot owes phase 1 and takes the step's lock. That boot answers within ten
// seconds while the pass still waits, having marked the ask, and once the row is let go the pass
// restores the issue's answered ask.
func TestABootWaitsForNoPassOfTheThirdPhase(t *testing.T) {
	ctx := context.Background()
	issue := newRestoreIssue(t, "# Spec\n\n:::ask{#q-boot urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n")
	issue.answer(issue.spec, "q-boot", "The base release.")
	issue.replace(issue.spec, "# Spec\n\nThe corpus.\n")
	if _, err := graphcensus.BackfillAsks(ctx, issue.database); err != nil {
		t.Fatalf("state the issue and its spec on their nodes: %v", err)
	}
	if owed, err := graphcensus.AsksOwed(ctx, issue.database); err != nil || owed {
		t.Fatalf("phase 1 owes a row %t (%v) after it ran, want none, so the pass the tick waits in on the issue is phase 3's", owed, err)
	}
	holder, err := issue.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the issue: %v", err)
	}
	testcleanup.Add(t, func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `select 1 from issues where key = 'DOC-1' for no key update`); err != nil {
		t.Fatalf("take the issue's row: %v", err)
	}
	step := NewDecisionStep(issue.database, catchUpCommit, nil)
	step.UseDocuments(issue.service)
	ticked := make(chan error, 1)
	go func() { ticked <- step.Tick(ctx) }()
	storetest.WaitForLockWaiters(t, holder, 1)
	owedAsk, _ := seedUnmarkedAsk(t, issue.database, "OWED")
	if owed, err := graphcensus.AsksOwed(ctx, issue.database); err != nil || !owed {
		t.Fatalf("phase 1 owes a row %t (%v) once the release before wrote an ask, want one owed, so the boot takes the step's lock", owed, err)
	}

	booting := NewDecisionStep(issue.database, catchUpCommit, nil)
	bootCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	started := time.Now()
	if err := booting.Boot(bootCtx); err != nil {
		t.Fatalf("a boot while a pass of phase 3 waits answered %v after %s, want it to wait for no part of that pass", err, time.Since(started))
	}
	t.Logf("the boot answered in %s while the pass of phase 3 waited on the issue", time.Since(started))
	if !askMarked(t, issue.database, owedAsk) {
		t.Fatal("the boot answered with the release before's ask unmarked, want its phase 1 to have marked it")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let the issue go: %v", err)
	}
	select {
	case err := <-ticked:
		if err != nil {
			t.Fatalf("the tick answered %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the tick answered nothing 60s after the issue was let go")
	}
	if spec := issue.latest(issue.spec); !strings.Contains(spec, "#q-boot urgency=") {
		t.Fatalf("the pass restored no ask once the issue was let go:\n%s", spec)
	}
}

// A booting process waits for the step's lock only while phase 1 owes a row (cmd/dispatch's
// boot). A tick holds that lock while it runs phases 1 and 2, and a
// process stopped or stalled while its tick holds it keeps it for as long as its session stays
// open. Where phase 1 owes nothing, such a holder keeps no boot waiting. Where phase 1 owes a row,
// the boot waits for the lock, so every process has phase 1 behind it before it serves.
//
// A second connection takes the step's lock and keeps it, as a stalled tick would. With nothing
// owed, a boot answers nil within ten seconds. An ask then stands as the release before writes it,
// naming no node. A boot has not answered two seconds later, and waits behind the holder. Once the
// lock is let go, the boot answers nil, the ask names its node, and phase 1 owes nothing.
func TestABootWaitsForTheStepsLockOnlyWhilePhaseOneOwesARow(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	conn, err := database.Pool.Dedicated(ctx, "test-stalled-decision-step")
	if err != nil {
		t.Fatalf("open the connection that holds the step's lock: %v", err)
	}
	testcleanup.Add(t, func() { _ = conn.Close(context.Background()) })
	holder, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the step's lock: %v", err)
	}
	if _, err := holder.Exec(ctx, `select pg_advisory_lock($1)`, decisionStepLock); err != nil {
		t.Fatalf("take the step's lock: %v", err)
	}
	if owed, err := graphcensus.AsksOwed(ctx, database); err != nil || owed {
		t.Fatalf("phase 1 owes a row %t (%v) on an empty database, want nothing owed", owed, err)
	}
	bootCtx, cancelBoot := context.WithTimeout(ctx, 10*time.Second)
	defer cancelBoot()
	started := time.Now()
	if err := NewDecisionStep(database, catchUpCommit, nil).Boot(bootCtx); err != nil {
		t.Fatalf("a boot with nothing owed answered %v after %s while another session held the step's lock, want nil: it waits for no lock", err, time.Since(started))
	}
	t.Logf("the boot with nothing owed answered in %s while another session held the step's lock", time.Since(started))

	ask, issue := seedUnmarkedAsk(t, database, "OWED")
	if owed, err := graphcensus.AsksOwed(ctx, database); err != nil || !owed {
		t.Fatalf("phase 1 owes a row %t (%v) once the release before wrote an ask, want one owed", owed, err)
	}
	owedCtx, cancelOwed := context.WithTimeout(ctx, time.Minute)
	defer cancelOwed()
	booted := make(chan error, 1)
	go func() { booted <- NewDecisionStep(database, catchUpCommit, nil).Boot(owedCtx) }()
	select {
	case err := <-booted:
		t.Fatalf("a boot with phase 1 owed answered %v while another session held the step's lock, want it to wait for the lock", err)
	case <-time.After(2 * time.Second):
	}
	if waiting := storetest.WaitForLockWaiters(t, holder, 1); waiting != 1 {
		t.Fatalf("%d backends wait behind the session that holds the step's lock, want the boot's", waiting)
	}
	var released bool
	if err := holder.QueryRow(ctx, `select pg_advisory_unlock($1)`, decisionStepLock).Scan(&released); err != nil || !released {
		t.Fatalf("let the step's lock go: released %t (%v)", released, err)
	}
	select {
	case err := <-booted:
		if err != nil {
			t.Fatalf("the boot answered %v once the step's lock was let go, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the boot answered nothing 30s after the step's lock was let go")
	}
	if !askMarked(t, database, ask) {
		t.Fatalf("the ask of %s names no node after the boot, want its phase 1 to have marked it", issue)
	}
	if owed, err := graphcensus.AsksOwed(ctx, database); err != nil || owed {
		t.Fatalf("phase 1 owes a row %t (%v) after the boot, want nothing owed", owed, err)
	}
}

// registerTheReleaseBefore registers in server_processes, the registry processes.Live reads, a
// live process of the release before, with the schema numbers a process of it registers
// (serveTwoBuilds), and answers its instance.
func registerTheReleaseBefore(t *testing.T, database *store.Store) string {
	t.Helper()
	var instance string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into server_processes (instance, commit, schema_version, schema_creatable, heartbeat_at, owner_lock_protocol)
		values (gen_random_uuid(), 'the-release-before', $1, $2, now() + interval '1 hour', $3)
		returning instance::text
	`, baseSchemaVersion, baseSchemaCreatable, store.OwnerLockProtocol).Scan(&instance); err != nil {
		t.Fatalf("register a live process of the release before: %v", err)
	}
	return instance
}

// leaveTheFleet deletes the process instance from server_processes, as a process deregisters once
// its drain has ended.
func leaveTheFleet(t *testing.T, database *store.Store, instance string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `delete from server_processes where instance = $1::uuid`, instance); err != nil {
		t.Fatalf("take process %s out of the fleet: %v", instance, err)
	}
}

// joiningBuild is a log handler of the decisions step that registers a live process of the release
// before (registerTheReleaseBefore) the moment the step logs that it restored and converted issue:
// a process of another build that joins the fleet between two issues of one pass of the third
// phase, after the first committed.
type joiningBuild struct {
	t        *testing.T
	database *store.Store
	issue    string
	joined   bool
}

func (j *joiningBuild) Enabled(context.Context, slog.Level) bool { return true }

func (j *joiningBuild) Handle(_ context.Context, record slog.Record) error {
	if j.joined || record.Message != "dispatch: decisions step restored and converted an issue" {
		return nil
	}
	record.Attrs(func(attr slog.Attr) bool {
		j.joined = attr.Key == "issue" && attr.Value.String() == j.issue
		return !j.joined
	})
	if j.joined {
		registerTheReleaseBefore(j.t, j.database)
	}
	return nil
}

func (j *joiningBuild) WithAttrs([]slog.Attr) slog.Handler { return j }

func (j *joiningBuild) WithGroup(string) slog.Handler { return j }

// readRetired reports whether document is retired (documentRetired).
func readRetired(t *testing.T, database *store.Store, document string) bool {
	t.Helper()
	retired, err := documentRetired(context.Background(), database.Pool, document)
	if err != nil {
		t.Fatalf("read whether %s is retired: %v", document, err)
	}
	return retired
}

// A pass of the third phase converts nothing once a process of another build goes live during it
// (the spec's Design): that build reads the decisions documents the pass retires, and would meet
// them half moved. Each issue's transaction reads the fleet again (restoreIssue), so the pass stops
// at the issue after the process joined, with that issue's transaction rolled back, and answers
// that it stopped (ErrAnotherBuildLive); a later pass, on a fleet of one build again, converts the
// rest.
//
// DOC-1 and DOC-2 each hold a decisions document of one section that cites no ask. A process of
// the release before registers the moment the step logs DOC-1 converted. The pass answers
// ErrAnotherBuildLive with DOC-1 alone touched and its decisions document retired, while DOC-2's
// decisions document stands at the one version it was written at and its spec gains no version.
// Once that process leaves the fleet, the next pass converts DOC-2 and retires its document.
func TestAPassOfTheThirdPhaseStopsWhereAProcessOfAnotherBuildGoesLiveDuringIt(t *testing.T) {
	ctx := context.Background()
	const section = "# Decisions\n\n## Files stay out of the corpus\n\nWe write no file beside the database.\n"
	first := newRestoreIssue(t, "# Spec\n\nThe corpus.\n")
	firstDecisions := first.document("decisions-md", "decisions.md", false, "decisions", section)
	second := first.another(2, "# Spec\n\nThe corpus.\n")
	secondDecisions := second.document("decisions-md", "decisions.md", false, "decisions", section)
	database := first.database
	specBefore := specVersions(t, database, second.spec)
	joining := &joiningBuild{t: t, database: database, issue: "DOC-1"}
	step := NewDecisionStep(database, catchUpCommit, slog.New(joining))
	step.UseDocuments(first.service)

	done, failed, err := step.RestoreAndConvert(ctx, "DOC", true, false)
	if !joining.joined {
		t.Fatalf("the pass answered %v having touched %+v, and never logged DOC-1 converted, so no process of another build joined it", err, done)
	}
	if !errors.Is(err, ErrAnotherBuildLive) || len(failed) != 0 || len(done) != 1 || done[0].Issue != "DOC-1" || !done[0].Retired {
		t.Fatalf("the pass a process of another build joined after DOC-1 answered %v, touched %+v and failed %+v, want ErrAnotherBuildLive with DOC-1 alone converted and retired",
			err, done, failed)
	}
	if !readRetired(t, database, firstDecisions) {
		t.Errorf("DOC-1's decisions document stands after the pass converted it, want it retired")
	}
	if readRetired(t, database, secondDecisions) {
		t.Fatalf("DOC-2's decisions document was retired after a process of another build joined the pass, want it standing")
	}
	if versions := specVersions(t, database, secondDecisions); versions != 1 {
		t.Errorf("DOC-2's decisions document holds %d versions after the pass stopped, want the one it was written at", versions)
	}
	if versions := specVersions(t, database, second.spec); versions != specBefore {
		t.Errorf("DOC-2's spec holds %d versions after the pass stopped, want the %d it held before", versions, specBefore)
	}

	if _, err := database.Pool.Exec(ctx, `delete from server_processes where commit = 'the-release-before'`); err != nil {
		t.Fatalf("take the release before out of the fleet: %v", err)
	}
	done, failed, err = step.RestoreAndConvert(ctx, "DOC", true, false)
	if err != nil || len(failed) != 0 || len(done) != 1 || done[0].Issue != "DOC-2" || done[0].Decisions != 1 || !done[0].Retired {
		t.Fatalf("the pass on a fleet of one build again answered %v, touched %+v and failed %+v, want DOC-2's section a decision and its document retired",
			err, done, failed)
	}
	if !readRetired(t, database, secondDecisions) {
		t.Errorf("DOC-2's decisions document stands after the later pass converted it, want it retired")
	}
}

// announceBuild records an announcement of commit in server_build_announcements, as the announcer
// records each build the whole fleet serves (startup's announcer), dated by the database now.
func announceBuild(t *testing.T, database *store.Store, commit string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `insert into server_build_announcements (commit) values ($1)`, commit); err != nil {
		t.Fatalf("announce %s: %v", commit, err)
	}
}

// The release before's answer on a row this build marked reaches the ask's node after a rollback
// and a second roll forward (R3, the roll window), and so does the restore it owes, which leaves
// what the step owes at 0 (decisions_step_owed). A rollback announces the release before again
// under a new seq, after that answer, so the catch-up's window opens where the fleet last stood
// whole on a build that marks no row before the asks rows' marker migration was applied
// (catchUpWindowOpens), and not at the newest announcement of all: a window opened at the
// rollback's announcement would open after the answer, and no process of this build would ever
// state it on the node, which the Decisions read and the restore's finder read.
//
// The release before is announced, then this build: DOC-1's spec holds an open ask this build
// marked, and a process of this build ticks on a fleet of its one build. A process of the release
// before goes live and that process of this build ticks once more, its last. The release before
// answers the ask on its row alone and its agent takes the ask's block out of the spec, and it is
// announced whole; its process leaves, and this build is announced again. A new process of this
// build ticks until its count reads nothing owed: the ask's node holds the answer under the
// release before's ask.answered, the spec holds the ask's block again, and the count reads 0.
func TestTheReleaseBeforesAnswerReachesTheNodeAfterARollBackAndAnotherRollForward(t *testing.T) {
	ctx := context.Background()
	const head = "# Spec\n\n:::requirement{#r1}\nThe corpus is written once.\n:::\n"
	issue := newRestoreIssue(t, head+"\n:::ask{#q-roll urgency=\"med\" multiple=\"false\"}\nWhich build writes the corpus?\n:::\n")
	database := issue.database
	ask := blockAskOn(t, issue.service, issue.spec, "q-roll").ID
	if !askMarked(t, database, ask) {
		t.Fatalf("ask %s names no node, want this build to have marked it as it opened it", ask)
	}
	announceBuild(t, database, "the-release-before")
	announceBuild(t, database, catchUpCommit)
	first := NewDecisionStep(database, catchUpCommit, nil)
	first.UseDocuments(issue.service)
	if err := first.Tick(ctx); err != nil {
		t.Fatalf("tick on a fleet of this build: %v", err)
	}
	instance := registerTheReleaseBefore(t, database)
	if err := first.Tick(ctx); err != nil {
		t.Fatalf("tick once the release before is live: %v", err)
	}
	answered := answerAsThePreviousBuild(t, database, issue.key, ask, "The base release")
	issue.replace(issue.spec, head)
	announceBuild(t, database, "the-release-before")
	leaveTheFleet(t, database, instance)
	announceBuild(t, database, catchUpCommit)

	second := NewDecisionStep(database, catchUpCommit, nil)
	second.UseDocuments(issue.service)
	for ticks := 1; ; ticks++ {
		if err := second.Tick(ctx); err != nil {
			t.Fatalf("tick %d of the process of the roll forward: %v", ticks, err)
		}
		if owed := second.Owed(); owed != nil && owed.Total == 0 && strings.Contains(issue.latest(issue.spec), "#q-roll urgency=") {
			t.Logf("the process of the roll forward owed nothing after %d ticks", ticks)
			break
		}
		if ticks == 5 {
			state, event := newestAnswerState(t, database, ask)
			t.Fatalf("after %d ticks the step owes %+v, the ask's node holds answer_state %q under event %d, and the spec reads:\n%s\nwant the release before's ask.answered %d stated, the ask restored and nothing owed",
				ticks, second.Owed(), state, event, issue.latest(issue.spec), answered)
		}
	}
	if state, event := newestAnswerState(t, database, ask); state != asks.StateAnswered || event != answered {
		t.Errorf("the ask's node holds answer_state %q under event %d, want answered under the release before's ask.answered %d", state, event, answered)
	}
	if total := second.OwedTotal(); total == nil || *total != 0 {
		t.Errorf("decisions_step_owed reads %v, want 0", total)
	}
}

// appendIssueClaim stores project key and its issue <key>-1 as rows alone, appending nothing, and
// then appends the issue's claim, issue.claimed, through the broker: an event of a write that
// moves no part of what the restore and the conversion owe (decisionStepOwedMovers).
func appendIssueClaim(t *testing.T, database *store.Store, key string) {
	t.Helper()
	ctx := context.Background()
	issue := key + "-1"
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ($1, $1, 'alice');
	`, key); err != nil {
		t.Fatalf("store project %s: %v", key, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'One', '{"kind":"user","id":"alice"}', 'U')
	`, issue, key); err != nil {
		t.Fatalf("store issue %s: %v", issue, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the claim of %s: %v", issue, err)
	}
	defer tx.Rollback(ctx)
	if _, err := events.NewBroker().Append(ctx, tx, model.Event{
		IssueKey: &issue, Type: "issue.claimed", Actor: catchUpAlice, Payload: map[string]string{"key": issue},
	}); err != nil {
		t.Fatalf("append the issue.claimed of %s: %v", issue, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the claim of %s: %v", issue, err)
	}
}

// A tick after an event of a write that cannot move what the restore and the conversion owe reads
// no table whole (recountOwed): a fleet appends such events all the time, and a count read again
// after each would read every question ask, document and project every 30 s on every process. The
// tick reads the events appended since its last read, finds none of a write that can move the
// count (decisionStepOwedMovers), and leaves the count standing; an event of such a write is
// counted at the next tick all the same.
//
// The step boots on an empty database and runs the tick that takes its catch-up's final pass and
// the one that counts after it. An issue is then claimed, with the issue.claimed its write appends,
// and the next tick, every statement of which a tracer on the step's pool records, reads the
// events since and none of the tables the count reads, and the count stands at nothing owed. A
// project then stores a list of gated steps holding `decisions`, with the project.created its
// write appends, and the next tick counts it.
func TestATickAfterAnEventThatCannotMoveTheCountReadsNoRestoreWork(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	recorder := &storetest.StatementRecorder{}
	traced := storetest.Reopened(t, database, func(config *pgxpool.Config) { config.ConnConfig.Tracer = recorder })
	step := NewDecisionStep(traced, catchUpCommit, nil)
	if err := step.Boot(ctx); err != nil {
		t.Fatalf("boot the step: %v", err)
	}
	for range 2 {
		if err := step.Tick(ctx); err != nil {
			t.Fatalf("run a tick of the step: %v", err)
		}
	}
	appendIssueClaim(t, database, "QUIET")
	before := len(recorder.Statements())
	if err := step.Tick(ctx); err != nil {
		t.Fatalf("run the tick after the claim: %v", err)
	}
	readEvents := false
	for _, statement := range recorder.Statements()[before:] {
		lowered := strings.ToLower(statement)
		if read := wholeTableRead.FindString(lowered); read != "" {
			t.Errorf("the tick after an issue's claim runs a statement that reads %q to the end:\n%s", read, statement)
		}
		readEvents = readEvents || strings.Contains(lowered, "from events e") && strings.Contains(lowered, "e.id > $1")
	}
	if !readEvents {
		t.Errorf("the tick after an issue's claim read no event of a write that can move the count, want the events since its last read read")
	}
	if owed := step.Owed(); owed == nil || owed.Total != 0 {
		t.Errorf("the step's count reads %+v after the claim, want it standing at 0", owed)
	}

	storeGatedProject(t, database, "LOUD")
	if err := step.Tick(ctx); err != nil {
		t.Fatalf("run the tick after a project's write: %v", err)
	}
	if owed := step.Owed(); owed == nil || owed.GatedStepsLists != 1 || owed.Total != 1 {
		t.Errorf("the step's count reads %+v after a project stored a list of gated steps holding decisions, want gated_steps_lists 1 and total 1", owed)
	}
}

// A row the backfill's object pass cannot write, because the store holds its node in another
// project (graphstore.ErrProjectMoved), is one no run cures, and it stops no boot: phase 1 runs
// the asks and follower passes past it, as the backfill command runs its passes past such a row,
// logs it, and owes nothing for it after, so the boot after it runs no phase 1 at all. Were the
// row owed, or its refusal a failure, every process of this build would exit at boot
// (cmd/dispatch) until a person repaired the store.
//
// The release before writes an ask on issue MOVED-1, a session's follow of it and a document of
// the issue, none of them naming a node, and the store holds the document's address gone in
// project ELSEWHERE. A boot answers nil, the ask and its follower row then name their node, phase
// 1 owes nothing, and a second boot answers nil and leaves it so.
func TestABootRunsTheAskPassesPastARowWhoseNodeStandsInAnotherProject(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	ask, issue := seedUnmarkedAsk(t, database, "MOVED")
	at := time.Now().UTC().Add(-time.Hour)
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into projects (key, name, default_assignee) values ('ELSEWHERE', 'Elsewhere', 'alice')`); err != nil {
			return err
		}
		moved := graphstore.ArtifactNode(issue+"/moved-md", "ELSEWHERE", "", catchUpAlice, at)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{moved}, nil); err != nil {
			return err
		}
		moved.State, moved.MadeAt = graphstore.StateGone, at.Add(time.Minute)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{moved}, nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ($1, 'MOVED', 'moved-md', 'Moved', 'doc', '{"kind":"user","id":"alice"}')
		`, issue); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into ask_followers (ask_id, session_id) values ($1::uuid, $2)`, ask, catchUpSession)
		return err
	}); err != nil {
		t.Fatalf("seed the document whose node stands in another project and the ask's follower: %v", err)
	}
	for boot := 1; boot <= 2; boot++ {
		if err := NewDecisionStep(database, catchUpCommit, nil).Boot(ctx); err != nil {
			t.Fatalf("boot %d answered %v, want nil: a row no run cures stops no boot", boot, err)
		}
		if !askMarked(t, database, ask) {
			t.Fatalf("the ask of %s names no node after boot %d, want the asks pass to have marked it", issue, boot)
		}
		var followed bool
		if err := database.Pool.QueryRow(ctx, `
			select node_kind is not null from ask_followers where ask_id = $1::uuid and session_id = $2
		`, ask, catchUpSession).Scan(&followed); err != nil || !followed {
			t.Fatalf("the follower row of %s names its node %t (%v) after boot %d, want the follower pass to have marked it",
				catchUpSession, followed, err, boot)
		}
		if owed, err := graphcensus.AsksOwed(ctx, database); err != nil || owed {
			t.Fatalf("phase 1 owes a row %t (%v) after boot %d, want nothing owed: the one row left is one no run cures", owed, err, boot)
		}
	}
}

// The catch-up's window opens where the fleet last stood whole on a build that marks no row
// before the migration that gives the asks rows their marker was applied, so a rollback to the
// release before and a second roll forward pass over no event that release appended on a row a
// build of this release marked. The release before announces its build again once the fleet
// stands whole on it after the rollback, later than an event it appended after this build's last
// pass, and the step's high-water mark is a process's own: a window opening at that announcement
// would leave the event out of every pass after it, and the backfill, which reads no marked row,
// would not state it either.
//
// The release before announces its build before the migration. This build marks an ask, runs its
// final catch-up pass, which finds nothing to state, and announces its build. The release before
// then serves alone: it answers the ask, which states nothing on the ask's node, and announces its
// build again. A process of this release then rolls forward again, of this commit and of another,
// and its first tick states the answer on the ask's node under the release before's ask.answered.
func TestTheCatchUpOfARollForwardAfterARollbackStatesWhatTheReleaseBeforeWroteInBetween(t *testing.T) {
	t.Parallel()
	for name, commit := range map[string]string{"this commit": catchUpCommit, "another commit of this release": catchUpFixCommit} {
		t.Run(name, func(t *testing.T) {
			database := storetest.Open(t)
			ctx := context.Background()
			announce := func(commit string) {
				t.Helper()
				if _, err := database.Pool.Exec(ctx, `insert into server_build_announcements (commit) values ($1)`, commit); err != nil {
					t.Fatalf("announce %s: %v", commit, err)
				}
			}
			before, err := database.Pool.Exec(ctx, `
				insert into server_build_announcements (commit, announced_at)
				select $1, m.applied_at - interval '1 hour' from schema_migrations m where m.version = $2
			`, previousReleaseCommit, askNodeMigration)
			if err != nil || before.RowsAffected() != 1 {
				t.Fatalf("announce the release before ahead of migration %d: %d row(s) (%v), want one", askNodeMigration, before.RowsAffected(), err)
			}
			ask, issue := seedCatchUpAsk(t, database, "ROLL")
			if err := NewDecisionStep(database, catchUpCommit, nil).Tick(ctx); err != nil {
				t.Fatalf("run this build's final pass before the rollback: %v", err)
			}
			announce(catchUpCommit)

			answered := answerAsThePreviousBuild(t, database, issue, ask, "Ship")
			announce(previousReleaseCommit)
			if _, event := newestAnswerState(t, database, ask); event == answered {
				t.Fatalf("the ask's node holds the release before's ask.answered %d before the roll forward, want it stated by none", answered)
			}

			if err := NewDecisionStep(database, commit, nil).Tick(ctx); err != nil {
				t.Fatalf("run the first tick of the roll forward: %v", err)
			}
			if state, event := newestAnswerState(t, database, ask); state != asks.StateAnswered || event != answered {
				t.Errorf("the ask's node holds answer_state %q under event %d after the roll forward's first tick, want answered under the release before's ask.answered %d",
					state, event, answered)
			}
		})
	}
}
