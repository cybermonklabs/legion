package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// storeStepWait bounds how long a run waits for the write under test to queue behind a hold, and
// how long a write that was let go is given to commit. Both are far above what the work takes; a
// run that passes either is the write waiting on something it should not, not a slow machine.
const storeStepWait = 20 * time.Second

// TestEveryApiStoreWriterTakesTheStoreLockOrder is this package's half of the check
// c-lock-order. graphstore's TestEveryStoreWriteCallSiteIsListed lists every call site of the node
// and tuple store's writers, beside the test that drives each site's lock steps. Thirteen of them
// live here, and this test drives them all:
//
//   - the comment path (createCommentFor, with the node it queues through graphstore.QueueNodes
//     and the tuples through graphstore.QueueCaused, then graphstore.QueueAuthored), run on an
//     issue and on a project document;
//   - a session's reply to a mention (replyComment, through the same three, and then
//     queueAnsweredDelivery, which records the session the answered attempt reached: that session
//     is the reply's author, whose node row the reply already holds, and its delivered_to tuple is
//     appended under the reply's own event, so it takes no lock the reply's earlier steps do not);
//   - the settle of a comment mention's delivery (completeCommentDelivery) and of a targeted
//     message's (completeMessageDelivery), each of which records the session it reached under
//     its receipt, in the batch that appends the receipt (graphstore.QueueReached);
//   - the follower route (changeAskFollower, through appendCausingEvent), which follows the ask
//     for a session before the event it appends and moves the node that follow made onto that
//     event (graphstore.QueueCaused), and an ask's create (createAskFor), whose node write states
//     the asker's node beside the ask's (graphstore.AskFacts) and which moves both onto the event
//     it appends;
//   - an approval request renewed (renewApprovalAsk), once on a retry by a session that did not
//     follow it, which follows it before the ask.follower_added it appends and moves the node
//     onto that event, as the follower route does, once on a retry by a session that follows it
//     already, which appends no event and states nothing of its follow, and once handing the
//     request back, which follows after its event and states the node under it in the follow's
//     own statement;
//   - three writes of a document version, each of which runs the block index (refs'
//     BlockIndexing.Queue) under the revision fence and appends what it stated under the event
//     that recorded the version: an upload creating an issue's document (storeArtifact), an edit
//     of a project document (editArtifact, through commitArtifactVersionEvent), and an issue's
//     create with its spec (createIssue). The upload and the create also write the node of what
//     they create - the document's, and the issue's and its spec's - before the row that names
//     it, and so before the version whose insert writes the fence row, which names that row.
//
// The order they are held to is the one written on api/inbox_read.go's markInboxReadThreads, which
// graphstore's TestEveryStoreWriteTakesOneLockOrder reads: the owner's lock comes first; a node's
// graph_nodes row is written before any projection row whose foreign key names it; the event is
// appended before the tuples whose cause it is; a write of several nodes takes them in ascending
// (kind, id); and a writer under the revision fence takes the document's document_block_versions
// row before it writes any of the document's block statement. Each path lists the steps it takes
// in the order it takes them, and that list is checked against the order
// (requireTheWrittenOrderAdmits). A path that takes fewer steps than the order lists declares only
// the steps it takes. Then each step is held, with one run of the path per step: another
// transaction holds what the step takes, and once the path's own backend has queued behind it the
// test reads the statement that backend waits at (pg_stat_activity), which has to be the statement
// that takes the step, the table locks its transaction holds (pg_locks), which have to include
// every earlier step's, and the rows it has locked, none of which may be a later step's
// (requireEachStepIsWaitedAtInTurn). Every run is then held to leaving no node or tuple row with a
// negative cause (storetest.RequireNoMarkedCause).
//
// Every event these paths append is an issue's or a project document's, so its owner's row is the
// one its owner lock stands for (events.Broker's lockEventOwner), and its project_key is null.
// None of them locks the event's project row, and every run is held to locking no projects row,
// with two exceptions. The issue's create writes an issue that does not exist before it, so its
// owner is its project, whose row it numbers the issue with (projectOwner). A new document's row
// names its project, whose foreign key takes the project row `for key share`, and that conflicts
// with no lock an event's project is taken with (projectKeyShare).
func TestEveryApiStoreWriterTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	rig := newStoreWriterRig(t)
	for _, writer := range []storeWriter{
		rig.commentCreate("createCommentFor on an issue", "/api/v1/issues/"+rig.issue+"/comments",
			ownerLockStep(store.IssueOwner(rig.issue))),
		rig.commentCreate("createCommentFor on a project document", "/api/v1/artifacts/"+rig.document+"/comments",
			ownerLockStep(store.ArtifactOwner(rig.document))),
		rig.mentionReply(), rig.commentDeliverySettle(), rig.messageDeliverySettle(),
		rig.askFollow(), rig.askCreate(), rig.approvalRetry(), rig.approvalFollowerRetry(), rig.approvalHandBack(),
		rig.documentUpload(), rig.documentEdit(), rig.issueCreate(),
	} {
		t.Run(writer.name, func(t *testing.T) {
			requireTheWrittenOrderAdmits(t, writer)
			requireEachStepIsWaitedAtInTurn(t, ctx, rig, writer)
		})
	}
}

// --- the steps ----------------------------------------------------------------------------

// orderStep is the step of the written order a lock step takes.
type orderStep int

const (
	ownerStep orderStep = iota
	fenceStep
	nodeStep
	projectionStep
	eventStep
	tupleStep
)

// The lock modes a step shows in pg_locks: `for update`, `for no key update` and a foreign key's
// `for key share` take the row share mode on their table, and an insert, an update or a delete
// takes the row exclusive mode. A plain read takes the access share mode, which no step is named
// by, so it is left out of what a run reads.
const (
	rowShareMode     = "RowShareLock"
	rowExclusiveMode = "RowExclusiveLock"
)

// parseTimeMode says a table lock of mode tells nothing of the order a write ran its statements
// in: the row share mode and the row exclusive mode. Each write phase is one pipelined batch
// (store.WriteBatch), and pgx prepares every statement its connection has not cached in one round
// trip before the first of them runs (conn.go, sendBatchExtendedWithDescription); Postgres takes a
// statement's table locks when it parses it and keeps them to the end of the transaction. So a
// write stopped at one step may already hold those two modes on the tables of its later statements
// without having run them. No writer's lock conflicts with either mode - only a `lock table` or a
// schema change does - so neither can make a writer wait, and the order that can is the rows'
// (rowsLocked). A stronger mode is taken only by a statement that asks for it, and held before
// its step it still fails.
func parseTimeMode(mode string) bool {
	return mode == rowShareMode || mode == rowExclusiveMode
}

// tableLock is one table lock a transaction holds, as pg_locks names it.
type tableLock struct {
	table, mode string
}

func (l tableLock) String() string { return l.mode + " on " + l.table }

// storeStep is one lock step of a write path.
type storeStep struct {
	name string
	// order is the step of the written order this one takes.
	order orderStep
	// names is, for a projection row, the node step whose node the row's foreign key names, and
	// empty for a row naming a node this path does not write.
	names string
	// caused says the step appends tuples that carry the event the path appends.
	caused bool
	// fenced says the step writes part of what a version's block index states about the
	// document - its block nodes, its block rows and the tuples the pass states - which a writer
	// under the revision fence (R23) writes only once it holds the fence's row.
	fenced bool
	// hold takes, in holder, what the step takes, so the write waits for holder at this step.
	hold func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun)
	// at is the head of the statement that takes the step, the one the write waits at.
	at string
	// takes are the table locks the step shows once it has run.
	takes []tableLock
}

// storeWriter is one write path under test: the steps it takes, in the order it takes them, and
// how one run of it is readied.
type storeWriter struct {
	name  string
	steps []storeStep
	// ready writes what one run needs through the rig's own server and answers the run, not yet
	// started.
	ready func(t *testing.T, ctx context.Context) storeWriterRun
	// projectKeyShare says the path writes a row whose foreign key names a project - a new
	// document's artifacts row names the project it is in, and an issue's row written a second time
	// in one transaction has its foreign key into projects checked again - and so holds that
	// project's row `for key share`, which conflicts with no lock an event's project is taken with.
	projectKeyShare bool
	// projectOwner says the path's owner is its project, whose row it writes: the issue's create,
	// which numbers the issue on it.
	projectOwner bool
}

// storeWriterRun is one run of a write path.
type storeWriterRun struct {
	// made is the one session or person node the run writes: the author's, the session the
	// delivery reached, or the session a follow records. No write before the run has recorded it,
	// so its row is new, and a new row is what an upsert waits on another transaction for. It is
	// the zero node for a path whose nodes are a document's blocks, which are held by their table.
	made graphstore.Node
	// answered is the comment a reply answers, and empty for every other path.
	answered string
	// write runs the path on the rig's one connection, to its commit, and answers how it ended.
	write func() error
}

// Each statement head a step is read at. pg_stat_activity holds the statement as the writer sent
// it, and normalizedStatement lowers it and folds its whitespace before it is compared.
const (
	// nodeWriteStatement is graphstore's node write (queueNodes), which the comment path queues
	// through graphstore.QueueNodes and the block index through BlockIndexing.Queue.
	nodeWriteStatement = "insert into graph_nodes ("
	// armsStatement is graphstore.WriteArms spliced into a statement of its own, as QueueAuthored
	// and QueueSessionRecorded queue it: the node arm and the tuple arms after it, in one statement.
	armsStatement = "with graph_nodes_written as ("
	// commentInsertStatement writes a comments row, a comment's or a reply's.
	commentInsertStatement = "insert into comments ("
	// eventAppendStatement is events.Broker's append (QueueAppend), after its owner row's lock.
	eventAppendStatement = "with appended as ( insert into events ("
	// tupleAppendStatement is graphstore's tuple append for a write that wrote nodes, which states
	// their base attributes and moves them onto the cause in the same statement (queueAppend).
	tupleAppendStatement = "with written as ("
	// plainTupleAppendStatement is graphstore's tuple append for a write that wrote no nodes, a
	// plain insert with nothing to move onto the cause (queueAppend).
	plainTupleAppendStatement = "insert into graph_tuples ("
	// followStatement is asks' follow statement: the follower row, with the store's node arm
	// spliced in after it (graphstore.WriteArms, WriteArmsUncaused).
	followStatement = "with followed as ("
	// versionStatement is a document version's insert (docs.RenderStampHead), whose trigger
	// (document_block_versions_owe, migration 0122) writes the document's document_block_versions
	// row: the revision fence's row, which the block index's queueStand takes again later in the
	// same transaction.
	versionStatement = "with head as (select "
	// blockRowStatement is the block index's write of the document_blocks rows of the blocks a
	// version changed, each naming its block's node.
	blockRowStatement = "insert into document_blocks ("
)

// --- the paths ----------------------------------------------------------------------------

// commentCreate is the comment path, through route. It takes its owner's lock, the issue's or the
// project document's (lockCommentOwner), and in its write phase writes the comment's node
// (graphstore.QueueNodes) before the comments row that names it, appends comment.created, then
// appends the tuples that event caused (graphstore.QueueCaused) and, in one statement, the
// author's node and the authored_by tuple under that event (graphstore.QueueAuthored). The
// comment's id is drawn inside its own transaction, so no other transaction can hold its node, its
// row or its tuples before they exist: those steps are held as the tables they are written to.
// Each run's author is a person no write has recorded, so the author's node is new and is held as
// its row.
func (r *storeWriterRig) commentCreate(name, route string, owner storeStep) storeWriter {
	return storeWriter{
		name: name,
		steps: []storeStep{
			owner,
			{
				name: "the comment's node", order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			{
				name: "the comment's row", order: projectionStep, names: "the comment's node",
				hold: holdTableWrites("comments"), at: commentInsertStatement,
				takes: []tableLock{{"comments", rowExclusiveMode}},
			},
			eventAppendStep(),
			tupleAppendStep(),
			authorNodeStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			login := r.fresh("lockwriter")
			author := model.Actor{Kind: "user", ID: login}
			return storeWriterRun{
				made: graphstore.PersonNode(login, author, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					return statusOrError(dispatchRequest(t, r.call, http.MethodPost, route,
						map[string]any{"body": "A remark the lock steps hold."}, login), http.StatusCreated)
				},
			}
		},
	}
}

// mentionReply is a session's reply to a mention, through its route. It takes its owner's lock
// (lockedCommentForDelivery), and from there takes the comment path's steps for the reply: its
// node, its row, the event, the tuples and its author. The comment it answers is read under that
// lock and locked by nothing but the foreign key of the reply's row.
func (r *storeWriterRig) mentionReply() storeWriter {
	return storeWriter{
		name: "replyComment",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			{
				name: "the reply's node", order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			{
				name: "the reply's row", order: projectionStep, names: "the reply's node",
				hold: holdTableWrites("comments"), at: commentInsertStatement,
				takes: []tableLock{{"comments", rowExclusiveMode}},
			},
			eventAppendStep(),
			tupleAppendStep(),
			authorNodeStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			answered := decodeMentionedComment(t, postMentionedComment(t, r.handler, r.issue, map[string]any{
				"body": "Does this ship, @s1?", "mentions": []map[string]any{{"target": "session:s1"}},
			}))
			settledCommentDeliveries(t, r.database, answered.ID)
			// The attempt's settle made the node of s1, the session it reached. The attempt is
			// pinned here to a session no write has recorded, and failed, as a send to a session
			// its resolution found leaves it when the send does not land: the reply is then that
			// session's first write, and its author's node is new (graphstore.QueueAuthored).
			session := r.fresh("lock-steps-session")
			if _, err := r.database.Pool.Exec(ctx, `
				update comment_deliveries
				set state = 'failed', error = 'the send did not land', envelope_id = null, session_id = $2
				where comment_id = $1 and target = 'session:s1' and attempt = 1
			`, answered.ID, session); err != nil {
				t.Fatalf("leave the mention's attempt failed at session %s: %v", session, err)
			}
			author := model.Actor{Kind: "session", ID: session}
			return storeWriterRun{
				made:     graphstore.SessionNode(session, author, time.Now().UTC(), graphstore.NoEvent),
				answered: answered.ID,
				write: func() error {
					return statusOrError(bearerRequest(t, r.call, http.MethodPost, "/api/v1/comments/"+answered.ID+"/reply", map[string]any{
						"actor": map[string]any{"kind": "session", "id": session}, "attempt": 1, "body": "Yes, it ships.",
					}), http.StatusCreated)
				},
			}
		},
	}
}

// commentDeliverySettle is the settle of a comment mention's delivery, called as the mention
// sender calls it once its send has landed. It takes the receipt's owner lock (LockOwners),
// records the send on the attempt row, and in one batch appends the receipt and then the reached
// session's node with its two base attributes under it, in one statement (graphstore.QueueReached
// over graphstore.LatestEvent). Each run's attempt is pending under the claim the run settles it
// with, as a sender leaves it at its send, and pinned to a session no write has recorded, so the
// session's node is new and is held as its row.
func (r *storeWriterRig) commentDeliverySettle() storeWriter {
	return storeWriter{
		name:  "completeCommentDelivery",
		steps: r.settleSteps(),
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			session := r.fresh("lock-steps-session")
			var claimed time.Time
			var mode string
			if err := r.database.Pool.QueryRow(ctx, `
				update comment_deliveries
				set state = 'pending', envelope_id = null, error = null, session_id = $2, claimed_at = now()
				where comment_id = $1 and target = 'session:s1' and attempt = 1
				returning claimed_at, delivery
			`, r.mentioned, session).Scan(&claimed, &mode); err != nil {
				t.Fatalf("leave the mention's attempt pending at session %s: %v", session, err)
			}
			comment, err := r.direct.loadComment(ctx, r.database.Pool, r.mentioned)
			if err != nil {
				t.Fatalf("load the mentioned comment: %v", err)
			}
			actor := model.Actor{Kind: "user", ID: "alice"}
			envelope := "lock-steps-envelope"
			return storeWriterRun{
				made: graphstore.SessionNode(session, actor, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					settled, err := r.direct.completeCommentDelivery(ctx, comment, model.CommentDelivery{
						CommentID: comment.ID, Target: "session:s1", Attempt: 1, Delivery: mode, SessionID: &session,
					}, actor, attemptClaim{attempt: 1, claimedAt: claimed}, sendOutcome{envelopeID: &envelope})
					if err != nil {
						return err
					}
					if settled.State != model.DeliveryStateSent {
						return fmt.Errorf("the attempt settled %s, want %s", settled.State, model.DeliveryStateSent)
					}
					return nil
				},
			}
		},
	}
}

// messageDeliverySettle is the settle of a targeted message's delivery, called as the message's
// sender calls it once its send has landed. Its steps are the comment mention's: the settle
// transaction is one function for both (settleDeliveryAttempt), and the message is an issue's, so
// the receipt's owner is that issue.
func (r *storeWriterRig) messageDeliverySettle() storeWriter {
	return storeWriter{
		name:  "completeMessageDelivery",
		steps: r.settleSteps(),
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			session := r.fresh("lock-steps-session")
			var claimed time.Time
			var mode string
			if err := r.database.Pool.QueryRow(ctx, `
				update message_deliveries
				set state = 'pending', envelope_id = null, error = null, session_id = $2, claimed_at = now()
				where message_id = $1 and attempt = 1
				returning claimed_at, delivery
			`, r.message.ID, session).Scan(&claimed, &mode); err != nil {
				t.Fatalf("leave the message's attempt pending at session %s: %v", session, err)
			}
			actor := model.Actor{Kind: "user", ID: "alice"}
			envelope := "lock-steps-envelope"
			return storeWriterRun{
				made: graphstore.SessionNode(session, actor, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					settled, err := r.direct.completeMessageDelivery(ctx, r.message, actor, pendingMessageDelivery{
						attemptClaim: attemptClaim{attempt: 1, claimedAt: claimed},
						resolved: ResolvedMention{
							Target: "session:s1", Delivery: mode, SessionID: &session,
							attemptSessionID: session, title: "planner",
						},
					}, sendOutcome{envelopeID: &envelope})
					if err != nil {
						return err
					}
					if settled.State != model.DeliveryStateSent {
						return fmt.Errorf("the attempt settled %s, want %s", settled.State, model.DeliveryStateSent)
					}
					return nil
				},
			}
		},
	}
}

// askFollow is the follower route following the rig's ask for a session no write has recorded
// (changeAskFollower). It takes the event's owner lock (LockOwners), follows the ask - the
// follower row and the session's node in one statement, the node marked for its cause - and in
// one batch appends ask.follower_added and then moves the node onto that event and appends its
// two base attributes under it (appendCausingEvent, graphstore.QueueCaused).
func (r *storeWriterRig) askFollow() storeWriter {
	return storeWriter{
		name: "changeAskFollower",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			followNodeStep("the follower's node"),
			eventAppendStep(),
			tupleAppendStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			session := r.fresh("lock-steps-follower")
			actor := model.Actor{Kind: "session", ID: session}
			return storeWriterRun{
				made: graphstore.SessionNode(session, actor, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					return statusOrError(sessionRequest(t, r.call, http.MethodPut, "/api/v1/asks/"+r.ask+"/followers/"+session,
						map[string]any{"actor": map[string]any{"kind": "session", "id": session}}), http.StatusNoContent)
				},
			}
		},
	}
}

// askCreate is an ask's create by a session no write has recorded (createAskFor). It takes its
// owner's lock, and in its write phase writes the ask's node and its asker's in one write, at the
// sentinel and before the asks row (graphstore.AskFacts, which states the ask's author beside it
// through AuthorFacts, queued through graphstore.QueueNodes), writes the ask, follows it for its
// asker, whose node that follow finds there (asks.PlanFollowAuthor), appends ask.opened, and
// then moves both nodes onto that event and appends what their write stated under it
// (graphstore.QueueCaused). The ask's id is drawn inside its own transaction, so its node and its
// row are not held; the session is new, so its node is held as a row another transaction inserted
// and has not committed, which the node write reaches after the ask's in ascending (kind, id).
func (r *storeWriterRig) askCreate() storeWriter {
	return storeWriter{
		name: "createAskFor",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			{
				name: "the asker's node", order: nodeStep,
				hold: holdNewNode, at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			eventAppendStep(),
			tupleAppendStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			session := r.fresh("lock-steps-asker")
			actor := model.Actor{Kind: "session", ID: session}
			return storeWriterRun{
				made: graphstore.SessionNode(session, actor, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					return statusOrError(sessionRequest(t, r.call, http.MethodPost, "/api/v1/issues/"+r.issue+"/asks",
						map[string]any{"question": "Which way do the lock steps run?", "actor": map[string]any{"kind": "session", "id": session}}),
						http.StatusCreated)
				},
			}
		},
	}
}

// approvalRetry is the spec's approval request renewed while it waits on the person
// (renewApprovalAsk), a retry that changes the request in nothing: a session no write has recorded
// asks for the same approval again, which makes it a follower, so the renewal appends
// ask.follower_added as the follower route does. The renewal takes its owner's lock
// (requireOpenOwner), follows the request for that session with the node marked for its cause,
// since the follow is queued before its event, appends the event, and then moves the node onto it
// and appends the node's two base attributes and the followed_by under it (graphstore.QueueCaused).
func (r *storeWriterRig) approvalRetry() storeWriter {
	return storeWriter{
		name: "renewApprovalAsk on a retry",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			followNodeStep("the requester's node"),
			eventAppendStep(),
			tupleAppendStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			session := r.fresh("lock-steps-requester")
			actor := model.Actor{Kind: "session", ID: session}
			return storeWriterRun{
				made: graphstore.SessionNode(session, actor, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					// 200 is the request already standing as asked, its follower_added aside: a 201
					// would be a renewal that handed it back, which is the other path.
					return statusOrError(sessionRequest(t, r.call, http.MethodPost, "/api/v1/artifacts/"+r.spec+"/approval-requests",
						map[string]any{"actor": map[string]any{"kind": "session", "id": session}}), http.StatusOK)
				},
			}
		},
	}
}

// approvalFollowerRetry is the spec's approval request renewed unchanged while it waits on the
// person by a session that follows it already (renewApprovalAsk), the retry that appends no event
// and adds no follower: each run first renews the request as a session no write has recorded,
// through the rig's own server, which makes that session a follower and its node, and then renews
// it again as that session. The renewal takes its owner's lock (requireOpenOwner), and its follow
// finds the follower row marked and the session's node there, so it states nothing of either; the
// facts it holds for the node are appended at the sentinel (graphstore.QueueCaused), for a node
// this follow made, which is none. The path takes no event step and no node step, and its tuple
// append carries no event.
func (r *storeWriterRig) approvalFollowerRetry() storeWriter {
	return storeWriter{
		name: "renewApprovalAsk on a retry by a follower",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			{
				name: "the tuple append", order: tupleStep,
				hold: holdTableWrites("graph_tuples"), at: tupleAppendStatement,
				takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			session := r.fresh("lock-steps-follower-requester")
			actor := model.Actor{Kind: "session", ID: session}
			renew := func(handler http.Handler) error {
				return statusOrError(sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+r.spec+"/approval-requests",
					map[string]any{"actor": map[string]any{"kind": "session", "id": session}}), http.StatusOK)
			}
			if err := renew(r.handler); err != nil {
				t.Fatalf("make %s a follower of the approval request: %v", session, err)
			}
			return storeWriterRun{
				made:  graphstore.SessionNode(session, actor, time.Now().UTC(), graphstore.NoEvent),
				write: func() error { return renew(r.call) },
			}
		},
	}
}

// approvalHandBack is the spec's approval request renewed while it waits on its agent
// (renewApprovalAsk), which hands it back to the person. Each run first writes a new version of
// the spec through the rig's own server (storeArtifact), which moves the open request to its agent
// (docs.MoveApprovalAsk). A session no write has recorded then asks for the approval again: the
// renewal takes its owner's lock, appends ask.handed_back (handBackApprovalAsk), and follows the
// request for that session caused by that event, which states the node and its two base
// attributes under the event in the follow's own statement (asks.Follow over graphstore.WriteArms).
// Its node step is therefore also a step whose tuples carry the event, and it comes after it.
func (r *storeWriterRig) approvalHandBack() storeWriter {
	return storeWriter{
		name: "renewApprovalAsk handing the request back",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			eventAppendStep(),
			{
				name: "the requester's node", order: nodeStep, caused: true,
				hold: holdNewNode, at: followStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}, {"graph_tuples", rowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			session := r.fresh("lock-steps-requester")
			created(t, "write the version that moves the approval request to its agent", dispatchRequest(t, r.handler, http.MethodPost,
				"/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
					"name": "spec.md", "content": "# Spec\n\nWhat the writers write, as " + session + " reads it.\n",
					"precondition": uploadBase(t, r.handler, r.spec),
				}, "alice"))
			actor := model.Actor{Kind: "session", ID: session}
			return storeWriterRun{
				made: graphstore.SessionNode(session, actor, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					return statusOrError(sessionRequest(t, r.call, http.MethodPost, "/api/v1/artifacts/"+r.spec+"/approval-requests",
						map[string]any{"actor": map[string]any{"kind": "session", "id": session}}), http.StatusCreated)
				},
			}
		},
	}
}

// documentUpload is an upload creating a document of the issue (storeArtifact). It takes the
// issue's owner lock, writes the document's node and then the document's row, which names it, the
// document's text, then its version, whose insert writes the document's revision fence row
// (document_block_versions_owe), and indexes that version: the fence row again (refs'
// queueStand), the blocks' nodes in ascending (kind, id), then their document_blocks rows, which
// name those nodes (refs' BlockIndexing.Queue). It then appends artifact.created and appends what
// the node write and the index stated under it (graphstore.QueueCaused). The document's node comes
// before the fence: the fence row's foreign key names the document's row, whose node comes before
// it, and the fence orders the document's block statement alone. The document is new, so no
// transaction can hold its node, its row, its fence row or its block rows before the upload writes
// them, and each of those steps is held as the table it writes. The block nodes are the second
// write of graph_nodes, so they are held by the node of the block the run's document names
// (heldBlockNodesStep). Its artifacts row names the issue's project, so the upload holds that
// project's row `for key share` (projectKeyShare). Its controls are the document's node written
// after its row, and after the fence and the block index: held at the document's node, the upload
// then already holds the locks of the later steps it took first, and held at its row or the fence,
// it holds no lock of its node.
func (r *storeWriterRig) documentUpload() storeWriter {
	return storeWriter{
		name: "storeArtifact creating a document", projectKeyShare: true,
		steps: append(append([]storeStep{ownerLockStep(store.IssueOwner(r.issue))},
			objectNodeSteps("the document", "artifacts", "insert into artifacts (")[:2]...),
			newFenceStep(),
			heldBlockNodesStep(),
			blockRowsStep(),
			eventAppendStep(),
			blockTuplesStep(),
		),
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			name := r.fresh("Lock steps upload")
			return storeWriterRun{
				made: r.heldBlockNode(graphstore.ArtifactRefKey(r.issue, "", "", artifactSlug(name))),
				write: func() error {
					return statusOrError(dispatchRequest(t, r.call, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
						"name": name, "content": "# " + name + "\n\n" + heldBlockMarkdown("What the upload indexes."),
					}, "alice"), http.StatusCreated)
				},
			}
		},
	}
}

// documentEdit is an edit of the project document that adds a paragraph to it (editArtifact),
// whose version's event commitArtifactVersionEvent appends, and then appends what the version's
// block index stated under that event (graphstore.QueueCaused). It takes the document's owner lock,
// applies the edit to the live document, writes the version, whose insert takes the document's
// fence row, and indexes the version as an upload does. The fence row is there from the
// document's first version, so it is held as a row; the new paragraph's node and row are held as
// their tables.
func (r *storeWriterRig) documentEdit() storeWriter {
	return storeWriter{
		name: "commitArtifactVersionEvent after an edit",
		steps: []storeStep{
			ownerLockStep(store.ArtifactOwner(r.document)),
			{
				name: "the revision fence", order: fenceStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
					holdRow(t, ctx, holder, `select 1 from document_block_versions where artifact_id = $1::uuid for no key update`, r.document)
				},
				at:    versionStatement,
				takes: []tableLock{{"document_block_versions", rowExclusiveMode}},
			},
			blockNodesStep(),
			blockRowsStep(),
			eventAppendStep(),
			blockTuplesStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			paragraph := r.fresh("A paragraph the lock steps edit adds, number")
			return storeWriterRun{write: func() error {
				return statusOrError(dispatchRequest(t, r.call, http.MethodPost, "/api/v1/artifacts/"+r.document+"/edits", map[string]any{
					"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": paragraph + "."}},
				}, "alice"), http.StatusOK)
			}}
		},
	}
}

// issueCreate is an issue's create with a spec (createIssue). The issue does not exist before it,
// so the owner it takes first is its project, whose row numbers the issue. It then writes the
// issue's node and its spec's in one write, then the issue's row and the spec's row, which name
// them, and the spec's version 1, whose insert writes the spec's fence row. It indexes that
// version as an upload does, appends issue.created, and appends what the index and the node write
// stated under it. The nodes come before the fence for the reason an upload's does. The spec's
// block nodes are held by the node of the block the spec names, whose address is the spec's: the
// issue's key is its project's next number, read before the run. Each run's create is forced past
// the near-copy gate, which would otherwise take each run's issue for a copy of the one before it.
// Its control is the two nodes written after the issue's row, which fails as the upload's controls
// do.
func (r *storeWriterRig) issueCreate() storeWriter {
	const nodes = "the issue's and its spec's nodes"
	return storeWriter{
		name: "createIssue", projectOwner: true,
		steps: []storeStep{
			ownerLockStep(store.ProjectOwner(r.project)),
			{
				name: nodes, order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			{
				name: "the issue's row", order: projectionStep, names: nodes,
				hold: holdTableWrites("issues"), at: "insert into issues (",
				takes: []tableLock{{"issues", rowExclusiveMode}},
			},
			{
				name: "the spec's row", order: projectionStep, names: nodes,
				hold: holdTableWrites("artifacts"), at: "insert into artifacts (",
				takes: []tableLock{{"artifacts", rowExclusiveMode}},
			},
			newFenceStep(),
			heldBlockNodesStep(),
			blockRowsStep(),
			eventAppendStep(),
			blockTuplesStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			title := r.fresh("The lock steps of a created issue")
			var next int
			if err := r.database.Pool.QueryRow(ctx, `select next_number from projects where key = $1`, r.project).Scan(&next); err != nil {
				t.Fatalf("read the number project %s gives its next issue: %v", r.project, err)
			}
			key := fmt.Sprintf("%s-%d", r.project, next)
			return storeWriterRun{
				made: r.heldBlockNode(graphstore.ArtifactRefKey(key, r.project, "", "spec")),
				write: func() error {
					return statusOrError(dispatchRequest(t, r.call, http.MethodPost, "/api/v1/issues", map[string]any{
						"project": r.project, "title": title, "spec": "# Spec\n\n" + heldBlockMarkdown("What "+title+" holds."), "force": true,
					}, "alice"), http.StatusCreated)
				},
			}
		},
	}
}

// settleSteps are the steps of a delivery's settle: the owner's lock, then the receipt, and the
// reached session's node with its two base attributes under it, in one statement right after the
// receipt in the same batch (graphstore.QueueReached over graphstore.LatestEvent), which is what
// keeps the node row and its base tuples naming one event. The node step is therefore also a step
// whose tuples carry the event, and it comes after it.
func (r *storeWriterRig) settleSteps() []storeStep {
	return []storeStep{
		ownerLockStep(store.IssueOwner(r.issue)),
		eventAppendStep(),
		{
			name: "the session's node", order: nodeStep, caused: true,
			hold: holdNewNode, at: armsStatement,
			takes: []tableLock{{"graph_nodes", rowExclusiveMode}, {"graph_tuples", rowExclusiveMode}},
		},
	}
}

// ownerLockStep is owner's lock, the transaction advisory lock every writer of the owner takes
// before it reads what it writes (store.LockOwner, through ownerStatus, lockCommentOwner,
// requireOpenOwner or events.Broker's LockOwners), held here as those writers take it. It locks no
// table and no row.
func ownerLockStep(owner store.Owner) storeStep {
	return storeStep{
		name: "the owner lock", order: ownerStep,
		hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
			t.Helper()
			if err := store.LockOwner(ctx, holder, owner); err != nil {
				t.Fatalf("hold %s's owner lock: %v", owner.LockKey(), err)
			}
		},
		at: store.OwnerLockStatement,
	}
}

// eventAppendStep is the event's append: the lock of its owner's row, which the write phase takes
// for the event, then its own insert, whose row nobody can hold before it exists: the events table
// is held instead.
func eventAppendStep() storeStep {
	return storeStep{
		name: "the event", order: eventStep,
		hold: holdTableWrites("events"), at: eventAppendStatement,
		takes: []tableLock{{"events", rowExclusiveMode}},
	}
}

// tupleAppendStep is caused's append of the tuples the event caused: the node's base attributes
// and any relation it holds, about a node whose id the path drew, so the table is held. Before it,
// caused moves the node onto the event (graphstore.QueueCaused), an update of the node row the path has
// held since its node step, which takes no lock the path does not already hold.
func tupleAppendStep() storeStep {
	return storeStep{
		name: "the tuple append", order: tupleStep, caused: true,
		hold: holdTableWrites("graph_tuples"), at: tupleAppendStatement,
		takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
	}
}

// authorNodeStep is graphstore.QueueAuthored: the author's node and the authored_by tuple under
// the event, in one statement.
func authorNodeStep() storeStep {
	return storeStep{
		name: "the author's node", order: nodeStep, caused: true,
		hold: holdNewNode, at: armsStatement,
		takes: []tableLock{{"graph_nodes", rowExclusiveMode}, {"graph_tuples", rowExclusiveMode}},
	}
}

// followNodeStep is asks.Follow for a session no write has recorded, with its cause to come: the
// follower row and the session's node in one statement, the node made at the sentinel with no
// base attribute of its own (graphstore.WriteArmsUncaused), so the statement writes no tuple.
func followNodeStep(name string) storeStep {
	return storeStep{
		name: name, order: nodeStep,
		hold: holdNewNode, at: followStatement,
		takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
	}
}

// newFenceStep is the revision fence's row of a document the write creates. The version's insert
// writes it (document_block_versions_owe), so no transaction can hold the row before the write
// makes it: the table's writes are held instead.
func newFenceStep() storeStep {
	return storeStep{
		name: "the revision fence", order: fenceStep,
		hold: holdTableWrites("document_block_versions"), at: versionStatement,
		takes: []tableLock{{"document_block_versions", rowExclusiveMode}},
	}
}

// blockNodesStep is the block index's node write: the nodes of the blocks the version changed, in
// one statement and in ascending (kind, id) (graphstore.QueueNodes). Their ids are the document's,
// and the write is the first to name them, so the table is held.
func blockNodesStep() storeStep {
	return storeStep{
		name: "the block nodes", order: nodeStep, fenced: true,
		hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
		takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
	}
}

// heldBlockNodesStep is the block index's node write in a path that wrote the node of what it
// creates before it: holding graph_nodes would stop the path at that earlier write, so the step is
// held by one node of the statement, the run's held block (heldBlockNode), whose upsert waits for
// the holder's uncommitted row of the same key.
func heldBlockNodesStep() storeStep {
	step := blockNodesStep()
	step.hold = holdNewNode
	return step
}

// heldBlockID is the id the run's document gives one of its blocks, a callout, so the address of
// that block's node is known before the write makes it: the document's ref_key, '#' and this id
// (refs' blockNodeAddress). Every other block's id is drawn when its document is written.
const heldBlockID = "held-by-the-lock-steps"

// heldBlockMarkdown is the callout of id heldBlockID, holding text.
func heldBlockMarkdown(text string) string {
	return ":::callout{#" + heldBlockID + ` kind="note" title=""}` + "\n" + text + "\n:::\n"
}

// heldBlockNode is the node of the held callout of the document at refKey, in the rig's project,
// as refs.IndexBlocks writes a block's node.
func (r *storeWriterRig) heldBlockNode(refKey string) graphstore.Node {
	return graphstore.Node{
		Kind: refs.BlockNodeKind, ID: refKey + "#" + heldBlockID, Supertag: refs.BlockSupertag,
		Visibility: graphstore.VisibilityProject, State: graphstore.StateLive, Project: r.project,
		MadeAt: time.Now().UTC(), MadeBy: model.Actor{Kind: "user", ID: "alice"}, SourceEvent: graphstore.NoEvent,
	}
}

// blockRowsStep is the block index's document_blocks rows, each naming its block's node.
func blockRowsStep() storeStep {
	return storeStep{
		name: "the block rows", order: projectionStep, names: "the block nodes", fenced: true,
		hold: holdTableWrites("document_blocks"), at: blockRowStatement,
		takes: []tableLock{{"document_blocks", rowExclusiveMode}},
	}
}

// blockTuplesStep is the append of what the block index stated, under the event that recorded
// the version (graphstore.QueueCaused): the block nodes moved onto that event, an update of rows the
// path holds since its node step, then the tuples.
func blockTuplesStep() storeStep {
	step := tupleAppendStep()
	step.name = "the block index's tuple append"
	step.fenced = true
	return step
}

// --- what a step is held by ---------------------------------------------------------------

// holdRow takes the lock statement names on one row, which has to be there.
func holdRow(t *testing.T, ctx context.Context, holder pgx.Tx, statement string, key string) {
	t.Helper()
	tag, err := holder.Exec(ctx, statement, key)
	if err != nil {
		t.Fatalf("hold the row %s with %q: %v", key, statement, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("hold the row %s with %q: it locked %d rows, want one", key, statement, tag.RowsAffected())
	}
}

// holdTableWrites holds a table in share mode. That mode conflicts with the row exclusive lock
// every insert, update and delete takes on the table and with no read and no row lock, so the
// write stops at its first write of the table and nowhere before it. It is how a step is held
// whose row the write makes under a key it draws in its own transaction.
func holdTableWrites(table string) func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
	return func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
		t.Helper()
		if _, err := holder.Exec(ctx, "lock table "+table+" in share mode"); err != nil {
			t.Fatalf("hold the writes of %s: %v", table, err)
		}
	}
}

// holdNewNode writes the run's new node in holder, through the store's own writer, and leaves it
// uncommitted: the write's upsert of the same key waits to learn whether this row commits.
func holdNewNode(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
	t.Helper()
	if err := graphstore.Write(ctx, holder, []graphstore.Node{run.made}, nil); err != nil {
		t.Fatalf("hold the row of node %s/%s: %v", run.made.Kind, run.made.ID, err)
	}
}

// --- the written order --------------------------------------------------------------------

// requireTheWrittenOrderAdmits checks a path's steps, in the order the path takes them, against
// the invariants on markInboxReadThreads: the owner's lock first, a node before the projection row
// naming it, the event before every step appending tuples that carry it, and the revision fence's
// row before every step of the block statement it guards. A path skips the steps it does not take,
// and is checked on the order of those it does. The fourth invariant, several nodes of one write
// in ascending (kind, id), is graphstore's own (queueNodes, WriteArms): every node step here is
// one statement, which writes one node, or a version's block nodes in that order.
func requireTheWrittenOrderAdmits(t *testing.T, writer storeWriter) {
	t.Helper()
	position := make(map[string]int, len(writer.steps))
	event, fence := -1, -1
	for index, step := range writer.steps {
		position[step.name] = index
		switch step.order {
		case eventStep:
			event = index
		case fenceStep:
			fence = index
		}
	}
	for index, step := range writer.steps {
		if step.order == ownerStep && index != 0 {
			t.Errorf("%s takes the owner lock as its step %d: the owner lock comes first", writer.name, index+1)
		}
		if step.order == projectionStep && step.names != "" {
			node, listed := position[step.names]
			switch {
			case !listed || writer.steps[node].order != nodeStep:
				t.Errorf("%s's %s names %q, which is no node step of it", writer.name, step.name, step.names)
			case node > index:
				t.Errorf("%s takes %s before %s, the node its foreign key names", writer.name, step.name, step.names)
			}
		}
		if step.caused && (event < 0 || event > index) {
			t.Errorf("%s takes %s, whose tuples carry its event, before it appends that event", writer.name, step.name)
		}
		if step.fenced && (fence < 0 || fence > index) {
			t.Errorf("%s takes %s, which the revision fence guards, before it takes the fence's document_block_versions row", writer.name, step.name)
		}
	}
}

// requireEachStepIsWaitedAtInTurn runs the path once per step with that step held, and holds the
// stopped write to the statement that takes the step, to the locks of the steps before it, and
// to no lock of a later step. A lock several steps show is read for the first of them, the one
// that takes it: the second node write of a path shows graph_nodes again and says nothing about
// where the write is, which the statement the write waits at says. A later step is read by its
// rows (rowsLocked) and by its table locks of every mode but the two a batch's preparation takes
// (parseTimeMode).
func requireEachStepIsWaitedAtInTurn(t *testing.T, ctx context.Context, rig *storeWriterRig, writer storeWriter) {
	t.Helper()
	first := firstTakers(writer.steps)
	for index, step := range writer.steps {
		run := writer.ready(t, ctx)
		holder, release := rig.beginHolder(t, ctx)
		step.hold(t, ctx, holder, run)
		backend := rig.callBackend(t, ctx)
		done := make(chan error, 1)
		go func() { done <- run.write() }()
		rig.waitQueued(t, ctx, holder, backend, done, writer.name+" held at "+step.name)

		running := rig.runningStatement(t, ctx, backend)
		held := rig.locksHeld(t, ctx, backend)
		t.Logf("held at %s, %s waits at: %s", step.name, writer.name, running)
		t.Logf("held at %s, %s holds: %v", step.name, writer.name, held)
		if !strings.HasPrefix(running, normalizedStatement(step.at)) {
			t.Errorf("%s held at %s waits at %q, which is not the statement that takes %s (%q)",
				writer.name, step.name, running, step.name, step.at)
		}
		for position, other := range writer.steps {
			for _, lock := range other.takes {
				if first[lock] != position || position == index {
					continue
				}
				switch {
				case position < index && !slices.Contains(held, lock):
					t.Errorf("%s held at %s holds no %s, which %s takes, an earlier step: it reached %s before it took %s (it holds %v)",
						writer.name, step.name, lock, other.name, step.name, other.name, held)
				case position > index:
					if !parseTimeMode(lock.mode) && slices.Contains(held, lock) {
						t.Errorf("%s held at %s already holds %s, which %s takes, a later step: it took %s before %s (it holds %v)",
							writer.name, step.name, lock, other.name, other.name, step.name, held)
					}
					if locked := rig.rowsLocked(t, ctx, backend, lock.table); len(locked) > 0 {
						t.Errorf("%s held at %s has locked rows %v of %s, which %s takes, a later step: it took %s before %s",
							writer.name, step.name, locked, lock.table, other.name, other.name, step.name)
					}
				}
			}
		}
		// An event's append moves its owner's last_seq in one statement whose arms update issues,
		// artifacts and projects, so every append holds the row exclusive lock of projects with
		// no row of it. What a write must not take is a projects row, but for the project that
		// owns an issue's create and a project a new document's foreign key reads.
		if !writer.projectOwner && !writer.projectKeyShare {
			if locked := rig.rowsLocked(t, ctx, backend, "projects"); len(locked) > 0 {
				t.Errorf("%s held at %s has locked projects rows %v: it took the project of its event, which an issue's or a document's event does not name",
					writer.name, step.name, locked)
			}
		}
		release()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s, once let go at %s: %v", writer.name, step.name, err)
			}
		case <-time.After(storeStepWait):
			t.Fatalf("%s did not commit within %s of being let go at %s", writer.name, storeStepWait, step.name)
		}
		storetest.RequireNoMarkedCause(t, rig.database, writer.name+" let go at "+step.name)
	}
}

// rowsLocked names, by ctid, the rows of table that backend's transaction has locked or written
// over: those whose xmax is its transaction id. A row is locked only by a statement that ran, so a
// row here is a step the write has taken, whatever table locks its batch's preparation took. A
// transaction that has locked no row and written nothing has no id, and answers none.
func (r *storeWriterRig) rowsLocked(t *testing.T, ctx context.Context, backend uint32, table string) []string {
	t.Helper()
	rows, err := r.database.Pool.Query(ctx, `select locked.ctid::text from `+pgx.Identifier{table}.Sanitize()+` locked
		where locked.xmax::text = (select backend_xid::text from pg_stat_activity where pid = $1)
		order by 1`, backend)
	if err != nil {
		t.Fatalf("read the rows of %s backend %d has locked: %v", table, backend, err)
	}
	locked, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the rows of %s backend %d has locked: %v", table, backend, err)
	}
	return locked
}

// firstTakers answers, for each lock the steps show, the position of the first step that shows
// it, which is the step that takes it.
func firstTakers(steps []storeStep) map[tableLock]int {
	first := map[tableLock]int{}
	for position, step := range steps {
		for _, lock := range step.takes {
			if _, taken := first[lock]; !taken {
				first[lock] = position
			}
		}
	}
	return first
}

// --- the rig ------------------------------------------------------------------------------

// storeWriterRig is the database the paths write: an issue with its spec and a project document
// on one project, a comment on the issue mentioning the session s1 and a message to s1, each
// delivered, an ask alice opened on the issue, and an approval request of the spec, waiting on
// the person. Every write under test runs on a second server over the same dependencies whose
// store is one connection of its own, so the backend a write runs on is known before it starts
// and any other session queued behind the same hold is never taken for it.
type storeWriterRig struct {
	handler  http.Handler
	database *store.Store
	// call serves the routes, and direct calls the delivery steps, on callStore's one connection.
	call      http.Handler
	direct    *server
	callStore *store.Store
	project   string
	issue     string
	spec      string
	document  string
	// mentioned is the comment whose mention attempt the comment delivery's settle settles, and
	// message the message whose attempt the message delivery's settle settles.
	mentioned string
	message   model.Message
	// ask is the ask the follower route follows.
	ask string
	// named counts the fresh names the rig has handed out.
	named int
}

func newStoreWriterRig(t *testing.T) *storeWriterRig {
	t.Helper()
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["btw","steer"]}]`
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"lock-steps-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	// The issue's spec and the project document settle an hour on, never inside the test: a
	// settlement locks its owner's row and writes its blocks' nodes, and one that ran under a hold
	// would stand in front of the write under test.
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL, settle: time.Hour})
	issue := createInteractionIssue(t, handler, "LOCK", "The store writers' lock steps", "# Spec\n\nWhat the writers write.\n")
	document := createProjectDocument(t, handler, "LOCK", "Lock steps", "# Lock steps\n\nWhat the writers write.\n")
	rig := &storeWriterRig{
		handler: handler, database: database,
		project: "LOCK", issue: issue.Key, spec: issue.PrimaryArtifactID, document: document.ID,
	}

	rig.callStore = storetest.SingleConnection(t, database)
	callDeps := deps
	callDeps.Store = rig.callStore
	call := http.NewServeMux()
	// Registered after the one-connection store, so this server's wait runs before that store
	// closes.
	t.Cleanup(Register(call, t.Context(), callDeps).Wait)
	rig.call = call
	rig.direct = directServer(callDeps)

	mentioned := decodeMentionedComment(t, postMentionedComment(t, handler, rig.issue, map[string]any{
		"body": "Can this ship, @s1?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	settledCommentDeliveries(t, database, mentioned.ID)
	rig.mentioned = mentioned.ID
	rig.message = createIssueMessage(t, handler, rig.issue, map[string]any{
		"body": "Can this ship?", "target": "session:s1", "delivery": "steer",
	}, "alice")
	rig.ask = decodeBody[model.Ask](t, created(t, "open the ask the follows follow", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+rig.issue+"/asks", map[string]any{"question": "Who follows the lock steps?"}, "alice"))).ID
	created(t, "request the approval of the spec the renewals renew", sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+rig.spec+"/approval-requests", map[string]any{"actor": sessionActor()}))
	return rig
}

// fresh is a name no run has used.
func (r *storeWriterRig) fresh(prefix string) string {
	r.named++
	return fmt.Sprintf("%s-%d", prefix, r.named)
}

// callBackend is the backend of callStore's one connection, which the next write under test runs
// on. It is read before each run, so a connection the pool replaced in between is never watched.
func (r *storeWriterRig) callBackend(t *testing.T, ctx context.Context) uint32 {
	t.Helper()
	conn, err := r.callStore.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the write's connection: %v", err)
	}
	defer conn.Release()
	return conn.Conn().PgConn().PID()
}

// beginHolder opens a transaction to hold a step in, and the call that lets it go. The release
// is registered as a cleanup too, so a failed assertion never leaves a write queued on a lock
// nothing will drop.
func (r *storeWriterRig) beginHolder(t *testing.T, ctx context.Context) (pgx.Tx, func()) {
	t.Helper()
	holder, err := r.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a holder: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_ = holder.Rollback(context.Background())
	}
	t.Cleanup(release)
	return holder, release
}

// waitQueued waits for the write's backend to queue behind holder, polling through holder's own
// connection (storetest.QueuedBehind). A write that answers first never waited at the step, and
// one that never queues is waiting somewhere the step is not; each fails naming the step.
func (r *storeWriterRig) waitQueued(t *testing.T, ctx context.Context, holder pgx.Tx, backend uint32, done <-chan error, what string) {
	t.Helper()
	deadline := time.Now().Add(storeStepWait)
	for {
		select {
		case err := <-done:
			t.Fatalf("%s finished without waiting there: %v", what, err)
		default:
		}
		queued, err := storetest.QueuedBehind(ctx, holder, backend)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if queued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never queued there within %s; it runs %q", what, storeStepWait, r.runningStatement(t, ctx, backend))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// statementSpace is a run of whitespace in a statement, which normalizedStatement folds.
var statementSpace = regexp.MustCompile(`\s+`)

// normalizedStatement is statement lower-cased with every run of whitespace one space, so a
// statement is compared by its words rather than by how its source lays them out.
func normalizedStatement(statement string) string {
	return strings.TrimSpace(statementSpace.ReplaceAllString(strings.ToLower(statement), " "))
}

// runningStatement is the statement backend runs, which for a backend queued on a lock is the
// statement that asked for it. It reads pg_stat_activity through the pool, a fresh view each time.
func (r *storeWriterRig) runningStatement(t *testing.T, ctx context.Context, backend uint32) string {
	t.Helper()
	var query string
	if err := r.database.Pool.QueryRow(ctx,
		`select query from pg_stat_activity where pid = $1`, backend).Scan(&query); err != nil {
		t.Fatalf("read the statement backend %d runs: %v", backend, err)
	}
	return normalizedStatement(query)
}

// locksHeld is every table lock backend's transaction holds above a plain read's, in table and
// mode order. It reads pg_locks through the pool, and leaves out indexes and sequences, since a
// step is named by the table it locks or writes, and the tuple lock a backend takes while it
// queues on a row, which names the row's table but is no table lock.
func (r *storeWriterRig) locksHeld(t *testing.T, ctx context.Context, backend uint32) []tableLock {
	t.Helper()
	rows, err := r.database.Pool.Query(ctx, `
		select distinct c.relname::text, l.mode from pg_locks l
		join pg_class c on c.oid = l.relation
		where l.pid = $1 and l.granted and l.locktype = 'relation' and c.relkind in ('r', 'p')
			and l.mode <> 'AccessShareLock'
		order by 1, 2`, backend)
	if err != nil {
		t.Fatalf("read the locks backend %d holds: %v", backend, err)
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (tableLock, error) {
		var lock tableLock
		err := row.Scan(&lock.table, &lock.mode)
		return lock, err
	})
	if err != nil {
		t.Fatalf("read the locks backend %d holds: %v", backend, err)
	}
	return held
}

// statusOrError answers nil for a response of the status want and the response as an error for
// anything else.
func statusOrError(response *httptest.ResponseRecorder, want int) error {
	if response.Code != want {
		return fmt.Errorf("status=%d, want %d; body=%s", response.Code, want, response.Body.String())
	}
	return nil
}
