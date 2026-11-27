package docs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheDecisionsStepsWritersTakeTheStoreLockOrder drives the decisions step's sites of the node
// and tuple store along the passes that reach them, with the machinery of
// TestEveryDocsStoreWriterTakesTheStoreLockOrder: each path lists its steps in the order it takes
// them, the list is checked against the order written on api/inbox_read.go's
// markInboxReadThreads (requireTheDocsOrderAdmits), and each step is held in turn while the
// path's backend is read at the statement it waits at and at the table locks it holds
// (requireEachDocsStepIsWaitedAtInTurn). The paths are:
//
//   - the catch-up of a follow a process of the release before recorded (asks.CatchUpFollower,
//     run by catchUpAsk): the issue's owner lock, the ask's row, the follower row marked, and the
//     session's node and the ask node's followed_by under that build's ask.follower_added, an event
//     the log held before the pass began, so no event of this path orders them.
//
// Each path takes its issue's owner lock first, the transaction advisory lock every writer of the
// issue takes (lockArtifactOwner, store.LockOwner), which locks no table and no row; the issue's
// row is the event append's, in its write phase.
func TestTheDecisionsStepsWritersTakeTheStoreLockOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, path := range []func(t *testing.T) (*docsWriterRig, docsWriter){followCatchUpPath} {
		rig, writer := path(t)
		t.Run(writer.name, func(t *testing.T) {
			requireTheDocsOrderAdmits(t, writer)
			requireEachDocsStepIsWaitedAtInTurn(t, ctx, rig, writer)
		})
	}
}

// issueOwnerStep is the owner lock of *issue, the run's issue, which the step's passes take first
// (lockArtifactOwner): a transaction advisory lock, held as every writer of the issue takes it.
func issueOwnerStep(issue *string) docsStep {
	return docsStep{
		name: "the owner lock", order: docsOwnerStep,
		hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ docsRun) {
			t.Helper()
			if err := store.LockOwner(ctx, holder, store.IssueOwner(*issue)); err != nil {
				t.Fatalf("hold issue %s's owner lock: %v", *issue, err)
			}
		},
		at: docsOwnerStatement,
	}
}

// decisionStepRig is a database of its own and a document service over it that settles an hour
// on, so no timer settles a document under a hold, and no write of another path is a write the
// step reads.
func decisionStepRig(t *testing.T) *docsWriterRig {
	t.Helper()
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	service.settle = time.Hour
	return &docsWriterRig{database: database, service: service}
}

// followCatchUpPath is the catch-up of a follow the release before recorded on an ask this build
// marked (catchUpAsk, asks.CatchUpFollower), run as one pass of the step runs it, over an issue of
// its own for each run: that build's follow wrote the follower row unmarked and its
// ask.follower_added, and stated nothing on the store. The pass takes the ask's owner row, then
// the ask's row, marks the follower row, which names the ask's node, and then writes the session's
// node and the ask node's followed_by under that event. The issue's and the ask's rows are held as
// rows, and the follower row, the session's node and the followed_by, each the pass's first write
// of its table, as their tables. The follow's event was in the log before the pass, so no event
// step orders the followed_by.
func followCatchUpPath(t *testing.T) (*docsWriterRig, docsWriter) {
	r := decisionStepRig(t)
	// The run under way's issue and ask, which the holds name.
	var issue, ask string
	return r, docsWriter{
		name: "CatchUpFollower",
		steps: []docsStep{
			issueOwnerStep(&issue),
			{
				name: "the ask's row", order: docsProjectionStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ docsRun) {
					holdDocsRow(t, ctx, holder, `select 1 from asks where id = $1::uuid for update`, ask)
				},
				at:    "select node_kind is not null, coalesce(issue_key, ''), coalesce(artifact_id::text, '') from asks",
				takes: []docsTableLock{{"asks", docsRowShareMode}},
			},
			{
				name: "the follower row", order: docsProjectionStep,
				hold: holdDocsTableWrites("ask_followers"), at: "update ask_followers f set node_kind",
				takes: []docsTableLock{{"ask_followers", docsRowExclusiveMode}},
			},
			{
				name: "the session's node", order: docsNodeStep,
				hold: holdDocsTableWrites("graph_nodes"), at: docsNodeStatement,
				takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
			},
			{
				name: "the followed_by", order: docsTupleStep,
				hold: holdDocsTableWrites("graph_tuples"), at: docsTupleStatement,
				takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			r.named++
			ask, issue = seedCatchUpAsk(t, r.database, fmt.Sprintf("CATCH%d", r.named))
			session := r.fresh("lock-steps-follower")
			followed := asThePreviousBuild(t, r.database, issue, func(ctx context.Context, tx pgx.Tx) (model.Event, error) {
				if _, err := tx.Exec(ctx, `insert into ask_followers (ask_id, session_id) values ($1::uuid, $2)`, ask, session); err != nil {
					return model.Event{}, err
				}
				return model.Event{Type: "ask.follower_added", Actor: catchUpAlice, Payload: model.AskFollowerEventPayload{
					AskID: ask, SessionID: session, By: catchUpAlice,
				}}, nil
			})
			caughtUp := ask
			return docsRun{write: func() error {
				failed, err := NewDecisionStep(r.database, catchUpCommit, nil).catchUp(ctx)
				if err != nil || failed {
					return fmt.Errorf("the catch-up answered failed=%v: %v", failed, err)
				}
				var marked bool
				var cause int64
				if err := r.database.Pool.QueryRow(ctx, `
					select f.node_kind is not null, coalesce((
						select t.source_event from graph_tuples t
						where t.from_kind = $3 and t.from_id = $1::uuid::text and t.kind = $4 and t.to_id = $2 and t.asserted
						order by t.seq desc limit 1), 0)
					from ask_followers f where f.ask_id = $1::uuid and f.session_id = $2
				`, caughtUp, session, graphstore.KindAsk, graphstore.TupleFollowedBy).Scan(&marked, &cause); err != nil {
					return fmt.Errorf("read the caught-up follow of %s: %w", session, err)
				}
				if !marked || cause != followed {
					return fmt.Errorf("the follow of %s reads marked %v, its followed_by caused by %d, want it marked and caused by the follow's event %d",
						session, marked, cause, followed)
				}
				return nil
			}}
		},
	}
}
