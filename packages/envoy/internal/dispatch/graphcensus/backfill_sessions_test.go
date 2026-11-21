package graphcensus

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// The sessions a database written before the store existed names and no write of the store
// recorded: the session that opened an ask, a second session that follows it, and a session an
// ask.follower_added event named and that no longer follows.
const (
	preStoreAsker    = "ses_backfill_asker"
	preStoreFollower = "ses_backfill_follower"
	preStoreLeft     = "ses_backfill_left"
)

// seedPreStoreFollowers writes, as a build of the release before the store writes them, an ask a
// session opened on CENS-1, its two follower rows and the event of a follower since removed, and
// answers that event.
func seedPreStoreFollowers(t *testing.T, writes addressingWrites) model.Event {
	t.Helper()
	ctx := context.Background()
	alice := model.Actor{Kind: "user", ID: "alice"}
	writes.createIssue(t, "CENS-1", 1, "U", alice)
	var ask string
	if err := writes.database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question) values ('CENS-1', $1, 'Who follows?') returning id::text
	`, `{"kind":"session","id":"`+preStoreAsker+`"}`).Scan(&ask); err != nil {
		t.Fatalf("write the ask: %v", err)
	}
	for _, follower := range []string{preStoreAsker, preStoreFollower} {
		if _, err := writes.database.Pool.Exec(ctx,
			`insert into ask_followers (ask_id, session_id) values ($1, $2)`, ask, follower); err != nil {
			t.Fatalf("write the follower %s: %v", follower, err)
		}
	}
	return writes.write(t, func(context.Context, pgx.Tx) (model.Event, error) {
		return model.Event{
			IssueKey: new("CENS-1"), Type: "ask.follower_added", Actor: alice,
			Payload: model.AskFollowerEventPayload{AskID: ask, SessionID: preStoreLeft, By: alice},
		}, nil
	})
}

// TestTheBackfillGivesEverySessionTheCensusNamesItsNode is R14's backfill of the sessions an
// existing ask names. The store census holds every session a row of the store names, or an
// ask.follower_added event ever named, to a node (recordedSessions), and a session that only
// asked or followed before the store existed was recorded by no write of the store, so the census
// reports each of them before the backfill. The backfill's session pass reads the same definition
// and writes each one's node, so the census reads zero after it, the node of a session an event
// named carries that event as its cause, and a second run writes nothing.
func TestTheBackfillGivesEverySessionTheCensusNamesItsNode(t *testing.T) {
	writes := newAddressingWrites(t)
	left := seedPreStoreFollowers(t, writes)

	before := censusKind(t, storeCensusOf(t, writes.database), graphstore.KindSession)
	reported := map[string]bool{}
	for _, difference := range before.ReportedRows {
		reported[difference.At] = true
	}
	for _, session := range []string{preStoreAsker, preStoreFollower, preStoreLeft} {
		if !reported["session "+session] {
			t.Errorf("before the backfill the session census reports %+v, which leaves out %s", before.ReportedRows, session)
		}
	}

	report, err := Backfill(context.Background(), writes.database, io.Discard)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if done := reportedKind(t, report, graphstore.KindSession); done.Rows != 3 || done.Nodes != 3 || !done.Complete {
		t.Errorf("the backfill reports %#v for sessions, want 3 rows, 3 nodes and every session complete", done)
	}
	after := censusKind(t, storeCensusOf(t, writes.database), graphstore.KindSession)
	if after.RowDifferences != 0 || after.NodeDifferences != 0 || after.Rows != 3 {
		t.Errorf("after the backfill the session census reads %d row and %d node difference(s) over %d row(s), want none over 3: %+v %+v",
			after.RowDifferences, after.NodeDifferences, after.Rows, after.ReportedRows, after.ReportedNodes)
	}
	var cause int64
	if err := writes.database.Pool.QueryRow(context.Background(),
		`select source_event from graph_nodes where kind = $1 and id = $2`, graphstore.KindSession, preStoreLeft).Scan(&cause); err != nil {
		t.Fatalf("read the node of %s: %v", preStoreLeft, err)
	}
	if cause != left.ID {
		t.Errorf("the node of %s is caused by %d, want %d, the ask.follower_added that named it", preStoreLeft, cause, left.ID)
	}

	nodes, tuples := storeSize(t, writes.database)
	again, err := Backfill(context.Background(), writes.database, io.Discard)
	if err != nil {
		t.Fatalf("backfill again: %v", err)
	}
	if done := reportedKind(t, again, graphstore.KindSession); done.Rows != 0 || !done.Complete {
		t.Errorf("a second backfill reports %#v for sessions, want no row", done)
	}
	if nodesAgain, tuplesAgain := storeSize(t, writes.database); nodesAgain != nodes || tuplesAgain != tuples {
		t.Errorf("a second backfill moved the store from %d nodes and %d tuples to %d and %d", nodes, tuples, nodesAgain, tuplesAgain)
	}
}

// requireTheSessionBatchLockOrder holds writeSessionBatch to its steps. The batch writes the node
// of each session the census names that the store holds none of, as one statement on a pooled
// connection: a session has no projection row, and the batch takes no owner lock and appends no
// event. The two sessions' nodes are held as rows other transactions inserted and have not
// committed, and the batch must queue behind them in ascending id, whatever order it read them in.
// The issue's owner lock is held throughout, and the batch never queues behind it. The test
// follows the session pass alone (backfillSessions): the asks and follower passes Backfill runs
// after it take that lock, as the owner of the ask and the follower rows they state
// (TestTheAskBatchesTakeEveryOwnerRowBeforeTheirRows), and following them would read their lock as
// the session batch's.
func requireTheSessionBatchLockOrder(t *testing.T) {
	writes := newAddressingWrites(t)
	database := writes.database
	seedPreStoreFollowers(t, writes)
	owner := lockstep.HoldOwner(t, database, &lockstep.Hold{Name: "issue CENS-1's owner lock", Step: lockstep.StepOwner},
		store.IssueOwner("CENS-1"))
	holds := []*lockstep.Hold{owner}
	for _, session := range []string{preStoreFollower, preStoreAsker, preStoreLeft} {
		holds = append(holds, holdWrite(t, database, &lockstep.Hold{
			Name: "the node row of " + session, Step: lockstep.StepNode, Taken: true,
			Kind: graphstore.KindSession, ID: session, Statement: "with graph_nodes_written as (",
		}, []graphstore.Node{graphstore.SessionNode(session, backfillActor, time.Now(), graphstore.NoEvent)}, nil))
	}
	report := followTheRun(t, database, "writeSessionBatch", func(ctx context.Context, database *store.Store) (BackfillReport, error) {
		done, err := backfillSessions(ctx, database)
		return BackfillReport{Kinds: []KindBackfill{done}}, err
	}, holds)
	if done := reportedKind(t, report, graphstore.KindSession); done.Rows != 3 || !done.Complete {
		t.Errorf("the backfill reports %#v for sessions, want 3 rows and every session complete", done)
	}
}
