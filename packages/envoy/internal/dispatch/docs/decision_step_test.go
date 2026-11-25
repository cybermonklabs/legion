package docs

import (
	"context"
	"encoding/json"
	"errors"
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

// wholeTableRead matches a statement that reads one of the tables a probe of the step reads to the
// end when nothing is owed: the object kinds and the asks graphcensus.AsksOwed reads.
var wholeTableRead = regexp.MustCompile(`\b(from|join)\s+(issues|artifacts|asks|ask_followers|messages|comments|projects)\b`)

// A tick with nothing owed and no other build live reads no table whole: it reads the live
// processes, the newest event and whether a document's block index is owed, each one probe of an
// index - every 30 s on every process, for as long as each runs. graphcensus.AsksOwed reads whole
// tables when nothing is owed, so it does not run on such a tick.
//
// The step boots on an empty database, runs the tick that takes its catch-up's final pass and the
// one after it, and then three more ticks, every statement of which a tracer on the step's pool
// records. None of those three reads a table of the probes'.
func TestATickWithNothingOwedReadsNoTableWhole(t *testing.T) {
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
	if owed, err := graphcensus.AsksOwed(ctx, database.Pool); err != nil || owed {
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
	if owed, err := graphcensus.AsksOwed(ctx, database.Pool); err != nil || !owed {
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
	if owed, err := graphcensus.AsksOwed(ctx, database.Pool); err != nil || owed {
		t.Fatalf("phase 1 owes a row %t (%v) after the boot, want nothing owed", owed, err)
	}
}
