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

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// storeStepWait bounds how long a run waits for the write under test to queue behind a hold, and
// how long a write that was let go is given to commit. Both are far above what the work takes; a
// run that passes either is the write waiting on something it should not, not a slow machine.
const storeStepWait = 20 * time.Second

// TestEveryApiStoreWriterTakesTheStoreLockOrder is this package's half of dispatch://INFRA-1149's
// c-lock-order. graphstore's TestEveryStoreWriteCallSiteIsListed lists every call site of the node
// and tuple store's writers and leaves the proof of each site's lock steps to the package the site
// lives in. Twelve of them live here, on ten write paths: the comment path (createCommentFor, with
// the node its writeNode writes and the tuples its caused appends, then graphstore.Authored), run
// on an issue and on a project document; a session's reply to a mention (replyComment, through
// the same three); the settle of a comment mention's delivery (completeCommentDelivery) and of
// a targeted message's (completeMessageDelivery), each of which records the session it reached
// through graphstore.Reached inside its own transaction; a run of the command bar, its record
// (recordGoingRequest) and one frame of it (appendGoingEvent), which takes no owner row; an
// issue's own field write made for a run (patchIssue), which writes the issue's node; an edit of
// an ask's words and of a comment's words made for a run (editAsk, editComment), which write the
// ask's and the comment's node; and the removal of an untouched document (removeArtifact), which
// states the document's node gone.
//
// The order they are held to is the one written on api/inbox_read.go's markInboxReadThreads, which
// graphstore's TestEveryStoreWriteTakesOneLockOrder reads: the owner row comes first; a node's
// graph_nodes row is written before any projection row whose foreign key names it; the event is
// appended before the tuples whose cause it is; and a write of several nodes takes them in
// ascending (kind, id). Each path lists the steps it takes in the order it takes them, and that
// list is checked against the order (requireTheWrittenOrderAdmits). Then each step is held, with
// one run of the path per step: another transaction holds what the step takes, and once the
// path's own backend has queued behind it the test reads the statement that backend waits at
// (pg_stat_activity), which has to be the statement that takes the step, and the table locks its
// transaction holds (pg_locks), which have to include every earlier step's and none of a later
// step's (requireEachStepIsWaitedAtInTurn). Postgres keeps every table lock until its transaction
// ends, so a step the write has already run shows as its lock, and a step it has not run does not.
//
// Every event these paths append is an issue's or a project document's, so its owner row is the
// row the owner step already holds (events.Broker's lockEventOwner), and its project_key is null.
// None of them takes the event's project row, and every run is held to taking no lock on projects
// (eventProjectLocks).
func TestEveryApiStoreWriterTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	rig := newStoreWriterRig(t)
	for _, writer := range []storeWriter{
		rig.commentCreate("createCommentFor on an issue", "/api/v1/issues/"+rig.issue+"/comments",
			rig.issueOwnerStep(issueOwnerStatement)),
		rig.commentCreate("createCommentFor on a project document", "/api/v1/artifacts/"+rig.document+"/comments",
			ownerRowStep(documentOwnerStatement, "artifacts", `select 1 from artifacts where id = $1::uuid for no key update`, rig.document)),
		rig.mentionReply(), rig.commentDeliverySettle(), rig.messageDeliverySettle(),
		rig.goingRecord(), rig.goingFrame(), rig.issuePatchForRun(), rig.issuePatchRelations(), rig.issuePatchFiledFrom(), rig.artifactRemoval(),
		rig.askEditForRun(), rig.commentEditForRun(),
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

// tableLock is one table lock a transaction holds, as pg_locks names it.
type tableLock struct {
	table, mode string
}

func (l tableLock) String() string { return l.mode + " on " + l.table }

// eventProjectLocks are the locks the written order's last step, the project of the event, would
// show: its row locked or written. None of the paths here takes them.
var eventProjectLocks = []tableLock{{"projects", rowShareMode}, {"projects", rowExclusiveMode}}

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
	// locksOwnProject says the path takes a lock on its project's row of its own - an issue
	// create numbers the issue under it, and a document's row's foreign key into projects takes
	// it in key share mode - so a lock on projects is not the project of its event
	// (eventProjectLocks).
	locksOwnProject bool
}

// storeWriterRun is one run of a write path.
type storeWriterRun struct {
	// made is the node the run's last store write makes: the author's, or the session the
	// delivery reached. No write before the run has recorded it, so its row is new, and a new
	// row is what an upsert waits on another transaction for.
	made graphstore.Node
	// answered is the comment a reply answers, and empty for every other path.
	answered string
	// write runs the path on the rig's one connection, to its commit, and answers how it ended.
	write func() error
}

// Each statement head a step is read at. pg_stat_activity holds the statement as the writer sent
// it, and normalizedStatement lowers it and folds its whitespace before it is compared.
const (
	// issueOwnerStatement is ownerStatus's lock of an issue (issueStatus, ownerRowLock), which the
	// comment path takes through lockCommentOwner and a reply through lockOwnerOfComment.
	issueOwnerStatement = "select status, closed_at is null from issues where key = $1 for no key update"
	// patchOwnerStatement is lockIssueRows' lock of each issue row a PATCH names, which the issue's
	// own field write takes first (lockPatchRows), and patchRowStatement that write's one update of
	// the issue's row.
	patchOwnerStatement = "select parent_key from issues where key = $1 for no key update"
	patchRowStatement   = "update issues set "
	// documentOwnerStatement is ownerStatus's lock of a project document, which the comment path
	// takes through lockCommentOwner on a comment the document owns.
	documentOwnerStatement = "select true from artifacts where id = $1 and issue_key is null for no key update"
	// eventOwnerStatement is events.Broker's lock of an issue event's owner, which a delivery's
	// settle takes through LockOwners before anything else (settleDeliveryAttempt).
	eventOwnerStatement = "select last_seq, project_key, pg_current_xact_id() from issues where key = $1 for no key update"
	// nodeWriteStatement is graphstore's node write (writeNodes), which writeNode runs through
	// graphstore.WriteUncaused.
	nodeWriteStatement = "with incoming as ("
	// armsStatement is graphstore.WriteArms spliced into a statement of its own, as Authored and
	// Reached run it: the node arm and the tuple arms after it, in one statement.
	armsStatement = "with graph_nodes_written as ("
	// commentInsertStatement writes a comments row, a comment's or a reply's.
	commentInsertStatement = "insert into comments ("
	// eventAppendStatement is events.Broker's append (AppendAll), after its owner lock.
	eventAppendStatement = "with inserted as ( insert into events ("
	// tupleAppendStatement is graphstore's tuple append, which caused runs through graphstore.Write.
	tupleAppendStatement = "insert into graph_tuples ("
	// goingRequestInsertStatement writes a run's going_requests row (recordGoingRequest), and
	// goingEventInsertStatement a frame's going_events row (goingFrameRow).
	goingRequestInsertStatement = "insert into going_requests ("
	goingEventInsertStatement   = "insert into going_events ("
)

// answeredRowStatement is loadCommentForUpdate's lock of the comment a reply answers.
var answeredRowStatement = "select " + commentColumns + " from comments where id = $1 for update"

// --- the paths ----------------------------------------------------------------------------

// commentCreate is the comment path, through route. It takes its owner row, the issue's or the
// project document's (lockCommentOwner), writes the comment's node (writeNode) before the comments
// row that names it, appends comment.created, then appends the tuples that event caused (caused)
// and, in one statement, the author's node and the authored_by tuple under that event
// (graphstore.Authored). The comment's id is drawn inside its own transaction, so no other
// transaction can hold its node, its row or its tuples before they exist: those steps are held as
// the tables they are written to. Each run's author is a person no write has recorded, so the
// author's node is new and is held as its row.
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
					return createdOrError(dispatchRequest(t, r.call, http.MethodPost, route,
						map[string]any{"body": "A remark the lock steps hold."}, login))
				},
			}
		},
	}
}

// mentionReply is a session's reply to a mention, through its route. It takes the owner row and
// then the comment it answers `for update` (lockedCommentForDelivery), and from there takes the
// comment path's steps for the reply: its node, its row, the event, the tuples and its author.
// The answered comment's row belongs to a projected table, and the written order lets it come
// before the reply's node: what it orders is a node against the rows that name that node, and
// this row names the answered comment's node, which the reply never writes.
func (r *storeWriterRig) mentionReply() storeWriter {
	return storeWriter{
		name: "replyComment",
		steps: []storeStep{
			r.issueOwnerStep(issueOwnerStatement),
			{
				name: "the answered comment's row", order: projectionStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
					holdRow(t, ctx, holder, `select 1 from comments where id = $1::uuid for update`, run.answered)
				},
				at:    answeredRowStatement,
				takes: []tableLock{{"comments", rowShareMode}},
			},
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
			// session's first write, and its author's node is new (graphstore.Authored).
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
					return createdOrError(bearerRequest(t, r.call, http.MethodPost, "/api/v1/comments/"+answered.ID+"/reply", map[string]any{
						"actor": map[string]any{"kind": "session", "id": session}, "attempt": 1, "body": "Yes, it ships.",
					}))
				},
			}
		},
	}
}

// commentDeliverySettle is the settle of a comment mention's delivery, called as the mention
// sender calls it once its send has landed. It takes the receipt's owner row (LockOwners), records
// the send on the attempt row, writes the reached session's node and that node's two base
// attributes in one statement (graphstore.Reached), and appends the receipt. Each run's attempt is
// pending under the claim the run settles it with, as a sender leaves it at its send, and pinned to
// a session no write has recorded, so the session's node is new and is held as its row.
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
// the receipt's owner row is that issue's.
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

// goingRecord is the record of a run of the command bar (recordGoingRequest), called as the going
// POST calls it. A run belongs to no issue and no document, so it takes no owner row: it writes
// the run's interaction node and its author's node in one statement (graphstore.Write), appends
// their tuples, and then writes the going_requests row that projects the node. It appends no
// event. The run's uuid is drawn inside its own transaction, so no other transaction can hold its
// node, its tuples or its row before they exist: each step is held as the table it is written to.
// Each run's asker is a person no write has recorded, so both nodes are new.
func (r *storeWriterRig) goingRecord() storeWriter {
	return storeWriter{
		name: "recordGoingRequest",
		steps: []storeStep{
			{
				name: "the run's interaction node", order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			{
				name: "the run's tuples", order: tupleStep,
				hold: holdTableWrites("graph_tuples"), at: tupleAppendStatement,
				takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
			},
			{
				name: "the run's row", order: projectionStep, names: "the run's interaction node",
				hold: holdTableWrites("going_requests"), at: goingRequestInsertStatement,
				takes: []tableLock{{"going_requests", rowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			login := r.fresh("lockasker")
			asker := model.Actor{Kind: "user", ID: login}
			return storeWriterRun{
				made: graphstore.PersonNode(login, asker, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					_, err := r.direct.recordGoingRequest(ctx, asker, "LOCK", "What do the lock steps hold?")
					return err
				},
			}
		},
	}
}

// goingFrame is one frame of a run (appendGoingEvent), the path the answer, a reader at the
// deadline and the sweep all write a frame through. Its first lock is the run's interaction node
// row `for no key update`, in place of the owner row a run does not have; then the run's
// going_requests row `for key share`, in the statement that reads the next sequence; then the
// frame's value tuple (graphstore.Write) and last the going_events row. The node and the run's row
// exist before the run starts, so each is held as its row: the node at the level the frame takes
// it, and the run's row `for update`, the one level `for key share` waits on. The frame writes no
// node, so the run's made names the interaction node the first step holds.
func (r *storeWriterRig) goingFrame() storeWriter {
	return storeWriter{
		name: "appendGoingEvent",
		steps: []storeStep{
			{
				name: "the run's interaction node", order: nodeStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
					holdRow(t, ctx, holder, `select 1 from graph_nodes where kind = 'interaction' and id = $1 for no key update`, run.made.ID)
				},
				at:    graphstore.NodeRowLockSQL,
				takes: []tableLock{{"graph_nodes", rowShareMode}},
			},
			{
				name: "the run's row", order: projectionStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
					holdRow(t, ctx, holder, `select 1 from going_requests where id = $1::uuid for update`, run.made.ID)
				},
				at:    goingNextFrame,
				takes: []tableLock{{"going_requests", rowShareMode}},
			},
			{
				name: "the frame's value", order: tupleStep,
				hold: holdTableWrites("graph_tuples"), at: tupleAppendStatement,
				takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
			},
			{
				name: "the frame's row", order: projectionStep, names: "the run's interaction node",
				hold: holdTableWrites("going_events"), at: goingEventInsertStatement,
				takes: []tableLock{{"going_events", rowExclusiveMode}},
			},
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			run, err := r.direct.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "LOCK", "Which frame do the lock steps hold?")
			if err != nil {
				t.Fatalf("record the run the frame is written of: %v", err)
			}
			return storeWriterRun{
				made: graphstore.Node{Kind: graphstore.KindInteraction, ID: run.ID},
				write: func() error {
					_, err := appendGoingEvent(ctx, r.callStore.Pool, run.ID, going.Text("A frame the lock steps hold."))
					return err
				},
			}
		},
	}
}

// issuePatchForRun is an issue's own field write made for a run (patchIssue), through its route,
// with the run named in its header: a PATCH of the issue's labels by the person whose run it is. It
// takes the issue's row as its owner row (lockPatchRows), updates that row, writes the issue's
// node (graphstore.WriteUncaused) after it, appends issue.updated, and then appends what the node
// states under that event with the node's changed_by_run to the run (graphstore.Caused). The
// issue's node exists from its create, so the node step is held as the table it is written to,
// and the row's update as its table too, since the write holds the row itself from its owner step.
// Each run is a run of its own, its window moved an hour ahead, by a person no write has recorded,
// and sets labels no run has set, so every run changes the issue.
//
// The event's append advances the issue's last_seq on the row this write has already updated
// (events.Broker's queueOwnerSeq), and Postgres checks the foreign key of a row its own
// transaction wrote whenever that transaction updates it again, so that second update takes the
// issue's project row `for key share`. That is a lock on the project of the issue's own row, taken
// at the event's step, which every PATCH that changes a field takes with or without a run, and not
// the project of its event, which an issue's event does not name (locksOwnProject).
func (r *storeWriterRig) issuePatchForRun() storeWriter {
	event := eventAppendStep()
	event.takes = append(event.takes, tableLock{"projects", rowShareMode})
	return storeWriter{
		name: "patchIssue under a run",
		steps: []storeStep{
			r.issueOwnerStep(patchOwnerStatement),
			{
				name: "the issue's row", order: projectionStep,
				hold: holdTableWrites("issues"), at: patchRowStatement,
				takes: []tableLock{{"issues", rowExclusiveMode}},
			},
			{
				name: "the issue's node", order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			event,
			tupleAppendStep(),
		},
		locksOwnProject: true,
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			login := r.fresh("lockpatcher")
			run, err := r.direct.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: login}, "LOCK", "Which labels do the lock steps hold?")
			if err != nil {
				t.Fatalf("record the run the PATCH is made for: %v", err)
			}
			runWindowAhead(t, r.database, run.ID)
			labels := []string{r.fresh("lock-label")}
			return storeWriterRun{
				made: graphstore.Node{Kind: graphstore.KindIssue, ID: r.issue},
				write: func() error {
					response := requestForRun(t, r.call, http.MethodPatch, "/api/v1/issues/"+r.issue,
						map[string]any{"labels": labels}, login, run.ID)
					if response.Code != http.StatusOK {
						return fmt.Errorf("status=%d body=%s", response.Code, response.Body.String())
					}
					return nil
				},
			}
		},
	}
}

// issuePatchRelations is an issue's PATCH that states a relation of the issue (patchIssue,
// dispatch://INFRA-1253 R10), through its route: a PATCH of the issue's needs. It takes the issue's
// row as its owner row (lockPatchRows), updates that row, writes the issue_links rows the new set
// projects (writeIssueNeeds), writes the issue's node (graphstore.WriteUncaused), so that
// graph_edges answers the relation out of a node the store holds, appends issue.updated, and then
// states the needs that event causes in one statement of the store's arms (stateIssueRelations).
// The issue's node exists from its create, so the node step is held as the table it is written to,
// and so are the rows, which the write holds itself from its owner step. Each run names a target
// no run has named, an issue it creates first, so every run retracts one wait and states another.
// The event's step takes the issue's project row `for key share`, as issuePatchForRun says.
func (r *storeWriterRig) issuePatchRelations() storeWriter {
	event := eventAppendStep()
	event.takes = append(event.takes, tableLock{"projects", rowShareMode})
	return storeWriter{
		name: "patchIssue stating its needs",
		steps: []storeStep{
			r.issueOwnerStep(patchOwnerStatement),
			{
				name: "the issue's row", order: projectionStep,
				hold: holdTableWrites("issues"), at: patchRowStatement,
				takes: []tableLock{{"issues", rowExclusiveMode}},
			},
			{
				name: "the issue's issue_links rows", order: projectionStep,
				hold: holdTableWrites("issue_links"), at: "delete from issue_links where issue_key = $1",
				takes: []tableLock{{"issue_links", rowExclusiveMode}},
			},
			{
				name: "the issue's node", order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			event,
			{
				name: "the relations the event causes", order: tupleStep, caused: true,
				hold: holdTableWrites("graph_tuples"), at: "with graph_relations_appended as (",
				takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
			},
		},
		locksOwnProject: true,
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			created := dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "LOCK", "title": r.fresh("A wait the lock steps hold"), "force": true,
			}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("create the run's target: status=%d body=%s", created.Code, created.Body.String())
			}
			target := decodeBody[model.Issue](t, created).Key
			return storeWriterRun{
				made: graphstore.Node{Kind: graphstore.KindIssue, ID: r.issue},
				write: func() error {
					response := dispatchRequest(t, r.call, http.MethodPatch, "/api/v1/issues/"+r.issue,
						map[string]any{"needs": []string{target}}, "alice")
					if response.Code != http.StatusOK {
						return fmt.Errorf("status=%d body=%s", response.Code, response.Body.String())
					}
					return nil
				},
			}
		},
	}
}

// issuePatchFiledFrom is an issue's PATCH that states its filed_from at a comment the release
// before wrote, which carries no node (patchIssue, dispatch://INFRA-1253 R9), through its route.
// It takes the issue's row as its owner row (lockPatchRows) and updates that row; then it states
// the comment's node as the backfill of the comments states it (stateFiledFromNode): the node rows,
// the comment's and its author's, in one node write, the facts they hold at the sentinel, which no
// event causes, and then the comments row naming the comment's node; then it writes the issue's
// node (graphstore.WriteUncaused), appends issue.updated, and claims the filed_from that event
// causes (stateIssueRelations). The comment is a row no run has seen, so its node is new, and the
// first node write, the first tuple append and the comments row are held as their tables. The
// issue's node exists from its create, and the node write locks it, so it is held as its row. The
// claim is appended under the event the write draws in its own transaction, so it is held by the
// assertion it appends under each id the next events can draw (holdNextEventsClaim). The event's
// step takes the issue's project row `for key share`, as issuePatchForRun says.
func (r *storeWriterRig) issuePatchFiledFrom() storeWriter {
	event := eventAppendStep()
	event.takes = append(event.takes, tableLock{"projects", rowShareMode})
	var comment string
	return storeWriter{
		name: "patchIssue stating its filed_from at a comment with no node",
		steps: []storeStep{
			r.issueOwnerStep(patchOwnerStatement),
			{
				name: "the issue's row", order: projectionStep,
				hold: holdTableWrites("issues"), at: patchRowStatement,
				takes: []tableLock{{"issues", rowExclusiveMode}},
			},
			{
				name: "the comment's node", order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			{
				name: "the comment's facts at the sentinel", order: tupleStep,
				hold: holdTableWrites("graph_tuples"), at: tupleAppendStatement,
				takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
			},
			{
				name: "the comment's row", order: projectionStep, names: "the comment's node",
				hold: holdTableWrites("comments"), at: "update comments set node_kind",
				takes: []tableLock{{"comments", rowExclusiveMode}},
			},
			{
				name: "the issue's node", order: nodeStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
					holdRow(t, ctx, holder, `select 1 from graph_nodes where kind = 'issue' and id = $1 for update`, r.issue)
				},
				at:    nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			event,
			{
				name: "the filed_from the event causes", order: tupleStep, caused: true,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
					holdNextEventsClaim(t, ctx, holder, r.issue, graphstore.TupleFiledFrom, graphstore.KindComment, comment)
				},
				at:    "with graph_claims as (",
				takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
			},
		},
		locksOwnProject: true,
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			if err := r.database.Pool.QueryRow(ctx, `
				insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text
			`, r.issue, r.fresh("Words the release before wrote")).Scan(&comment); err != nil {
				t.Fatalf("write the comment the run files the issue from: %v", err)
			}
			ref := "dispatch://" + r.issue + "/comment/" + comment
			return storeWriterRun{
				made: graphstore.Node{Kind: graphstore.KindComment, ID: comment},
				write: func() error {
					response := dispatchRequest(t, r.call, http.MethodPatch, "/api/v1/issues/"+r.issue,
						map[string]any{"filed_from": ref}, "alice")
					if response.Code != http.StatusOK {
						return fmt.Errorf("status=%d body=%s", response.Code, response.Body.String())
					}
					return nil
				},
			}
		},
	}
}

// claimHeldEvents is how many ids past events_id_seq's last holdNextEventsClaim holds a claim
// under: far more events than the rig's other work appends while one run waits.
const claimHeldEvents = 1024

// holdNextEventsClaim holds the assertion a write's claim appends out of issue under kind to the
// node toKind/toID, caused by the event the write appends, whose id it draws in its own
// transaction after the hold is taken. The hold writes, uncommitted, that assertion under each id
// the next claimHeldEvents events can draw (events_id_seq caches one value, so they come in order):
// the write's insert of its own then waits on the repeat key (graph_tuples_once) to learn whether
// the hold's row commits, and appends its own once the hold rolls back.
func holdNextEventsClaim(t *testing.T, ctx context.Context, holder pgx.Tx, issue, kind, toKind, toID string) {
	t.Helper()
	if _, err := holder.Exec(ctx, `
		insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
		select 'issue', $1, $2, $3, $4, now(), '{}'::jsonb, next.id, true
		from events_id_seq seq
		cross join lateral generate_series(seq.last_value + 1, seq.last_value + $5) as next (id)
	`, issue, kind, toKind, toID, claimHeldEvents); err != nil {
		t.Fatalf("hold the %s claim of %s under the next events: %v", kind, issue, err)
	}
}

// askRowLockStatement is lockAskForTransition's lock of the ask an edit writes, and
// askWordsStatement the edit's update of that row (editAsk).
var askRowLockStatement = "select " + askRowColumns + " " + askRowFrom + " where a.id = $1 for no key update of a"

const askWordsStatement = "update asks set question = $2"

// askEditForRun is an edit of an ask's words made for a run (editAsk), through its route, with
// the run named in its header: a PATCH of the question of an ask a session opened on the rig's
// issue, by the person whose run it is. It takes the issue's row as its owner row
// (requireOpenOwner), then the ask's row `for no key update` (lockAskForTransition), updates that
// row, writes the ask's node (graphstore.WriteUncaused) after it, appends ask.edited, and then
// appends what the node states under that event with the node's changed_by_run to the run
// (graphstore.Caused). The ask's node exists from its create, so the node step is held as the
// table it is written to, and the row's update as its table too, since the write holds the row
// itself from its lock. Each run edits an ask of its own, opened for it, to words no run has
// written, under a run of its own, its window moved an hour ahead, by a person no write has
// recorded.
func (r *storeWriterRig) askEditForRun() storeWriter {
	return storeWriter{
		name: "editAsk under a run",
		steps: []storeStep{
			r.issueOwnerStep(issueOwnerStatement),
			{
				name: "the ask's row", order: projectionStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
					holdRow(t, ctx, holder, `select 1 from asks where id = $1::uuid for no key update`, run.made.ID)
				},
				at:    askRowLockStatement,
				takes: []tableLock{{"asks", rowShareMode}},
			},
			{
				name: "the ask's words", order: projectionStep,
				hold: holdTableWrites("asks"), at: askWordsStatement,
				takes: []tableLock{{"asks", rowExclusiveMode}},
			},
			{
				name: "the ask's node", order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			eventAppendStep(),
			tupleAppendStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			ask := createIssueAsk(t, r.handler, r.issue, r.fresh("Which words do the lock steps hold"))
			login := r.fresh("lockasker")
			run, err := r.direct.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: login}, "LOCK", "Which words of the ask do the lock steps hold?")
			if err != nil {
				t.Fatalf("record the run the ask's edit is made for: %v", err)
			}
			runWindowAhead(t, r.database, run.ID)
			question := r.fresh("Which words does the edit hold")
			return storeWriterRun{
				made: graphstore.Node{Kind: graphstore.KindAsk, ID: ask},
				write: func() error {
					response := requestForRun(t, r.call, http.MethodPatch, "/api/v1/asks/"+ask,
						map[string]any{"question": question}, login, run.ID)
					if response.Code != http.StatusOK {
						return fmt.Errorf("status=%d body=%s", response.Code, response.Body.String())
					}
					return nil
				},
			}
		},
	}
}

// commentWordsStatement is an edit's update of the comment's row (editComment).
const commentWordsStatement = "update comments set body = $2"

// commentEditForRun is an edit of a comment's words made for a run (editComment), through its
// route, with the run named in its header: a PATCH of the body of a comment on the rig's issue by
// its author, the person whose run it is. It takes the issue's row as its owner row
// (lockOwnerOfComment), then the comment's row `for update` (loadCommentForUpdate), updates that
// row, writes the comment's node (graphstore.WriteUncaused) after it, appends comment.edited,
// and then appends what the node states under that event with the node's changed_by_run to the
// run (graphstore.Caused). The comment's node exists from its create, so the node step is held
// as the table it is written to, and the row's update as its table too, since the write holds
// the row itself from its lock. Each run edits a comment of its own, written for it, to words no
// run has written, under a run of its own, its window moved an hour ahead, by a person no write
// has recorded.
func (r *storeWriterRig) commentEditForRun() storeWriter {
	return storeWriter{
		name: "editComment under a run",
		steps: []storeStep{
			r.issueOwnerStep(issueOwnerStatement),
			{
				name: "the comment's row", order: projectionStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
					holdRow(t, ctx, holder, `select 1 from comments where id = $1::uuid for update`, run.made.ID)
				},
				at:    answeredRowStatement,
				takes: []tableLock{{"comments", rowShareMode}},
			},
			{
				name: "the comment's words", order: projectionStep,
				hold: holdTableWrites("comments"), at: commentWordsStatement,
				takes: []tableLock{{"comments", rowExclusiveMode}},
			},
			{
				name: "the comment's node", order: nodeStep,
				hold: holdTableWrites("graph_nodes"), at: nodeWriteStatement,
				takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
			},
			eventAppendStep(),
			tupleAppendStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			login := r.fresh("lockcommenter")
			written := dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/issues/"+r.issue+"/comments",
				map[string]any{"body": "A remark whose words the lock steps edit."}, login)
			if written.Code != http.StatusCreated {
				t.Fatalf("write the comment the edit is made of: status=%d body=%s", written.Code, written.Body.String())
			}
			comment := decodeBody[writtenRecord](t, written).ID
			run, err := r.direct.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: login}, "LOCK", "Which words of the comment do the lock steps hold?")
			if err != nil {
				t.Fatalf("record the run the comment's edit is made for: %v", err)
			}
			runWindowAhead(t, r.database, run.ID)
			body := r.fresh("The words the edit holds")
			return storeWriterRun{
				made: graphstore.Node{Kind: graphstore.KindComment, ID: comment},
				write: func() error {
					response := requestForRun(t, r.call, http.MethodPatch, "/api/v1/comments/"+comment,
						map[string]any{"body": body}, login, run.ID)
					if response.Code != http.StatusOK {
						return fmt.Errorf("status=%d body=%s", response.Code, response.Body.String())
					}
					return nil
				},
			}
		},
	}
}

// settleSteps are the steps of a delivery's settle: the owner row, the reached session's node,
// the receipt, and the node's two base attributes appended under it. The node is made at the
// sentinel, before the receipt it is recorded beside exists, so its attributes wait for that
// receipt and are appended after it (graphstore.Reached, graphstore.Caused), which is what
// keeps the node row and its base tuples naming one event.
func (r *storeWriterRig) settleSteps() []storeStep {
	return []storeStep{
		r.issueOwnerStep(eventOwnerStatement),
		{
			name: "the session's node", order: nodeStep,
			hold: holdNewNode, at: armsStatement,
			takes: []tableLock{{"graph_nodes", rowExclusiveMode}},
		},
		eventAppendStep(),
		tupleAppendStep(),
	}
}

// issueOwnerStep is the issue's owner row, taken by statement.
func (r *storeWriterRig) issueOwnerStep(statement string) storeStep {
	return ownerRowStep(statement, "issues", `select 1 from issues where key = $1 for no key update`, r.issue)
}

// ownerRowStep is the owner row of table, taken by statement and held by lock on key. lock takes
// the row `for no key update`, the level every writer takes an owner row at (ownerRowLock,
// events.Broker's lockEventOwner), which conflicts with itself, rather than the `for update` this
// module keeps off its owner tables (TestNoForUpdateOnOwnerTables).
func ownerRowStep(statement, table, lock, key string) storeStep {
	return storeStep{
		name: "the owner row", order: ownerStep,
		hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
			holdRow(t, ctx, holder, lock, key)
		},
		at:    statement,
		takes: []tableLock{{table, rowShareMode}},
	}
}

// eventAppendStep is the event's append. Its owner row is the one the owner step holds, so what
// the append waits at is its own insert, whose row nobody can hold before it exists: the events
// table is held instead.
func eventAppendStep() storeStep {
	return storeStep{
		name: "the event", order: eventStep,
		hold: holdTableWrites("events"), at: eventAppendStatement,
		takes: []tableLock{{"events", rowExclusiveMode}},
	}
}

// tupleAppendStep is caused's append of the tuples the event caused: the node's base attributes
// and any relation it holds, about a node whose id the path drew, so the table is held. Before it,
// caused moves the node onto the event (graphstore.Caused), an update of the node row the path has
// held since its node step, which takes no lock the path does not already hold.
func tupleAppendStep() storeStep {
	return storeStep{
		name: "the tuple append", order: tupleStep, caused: true,
		hold: holdTableWrites("graph_tuples"), at: tupleAppendStatement,
		takes: []tableLock{{"graph_tuples", rowExclusiveMode}},
	}
}

// authorNodeStep is graphstore.Authored: the author's node and the authored_by tuple under the
// event, in one statement.
func authorNodeStep() storeStep {
	return storeStep{
		name: "the author's node", order: nodeStep, caused: true,
		hold: holdNewNode, at: armsStatement,
		takes: []tableLock{{"graph_nodes", rowExclusiveMode}, {"graph_tuples", rowExclusiveMode}},
	}
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
// the invariants on markInboxReadThreads: the owner row first, a node before the projection row
// naming it, and the event before every step appending tuples that carry it. The fourth, several
// nodes of one write in ascending (kind, id), is graphstore's own (writeNodes, WriteArms), and
// every node step here writes one node.
func requireTheWrittenOrderAdmits(t *testing.T, writer storeWriter) {
	t.Helper()
	position := make(map[string]int, len(writer.steps))
	event := -1
	for index, step := range writer.steps {
		position[step.name] = index
		if step.order == eventStep {
			event = index
		}
	}
	for index, step := range writer.steps {
		if step.order == ownerStep && index != 0 {
			t.Errorf("%s takes the owner row as its step %d: the owner row comes first", writer.name, index+1)
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
	}
}

// requireEachStepIsWaitedAtInTurn runs the path once per step with that step held, and holds the
// stopped write to the statement that takes the step and to the locks of exactly the steps before
// it. A lock several steps show is read for the first of them, the one that takes it: the second
// node write of a path shows graph_nodes again and says nothing about where the write is, which
// the statement the write waits at says.
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
				switch has := slices.Contains(held, lock); {
				case position < index && !has:
					t.Errorf("%s held at %s holds no %s, which %s takes, an earlier step: it reached %s before it took %s (it holds %v)",
						writer.name, step.name, lock, other.name, step.name, other.name, held)
				case position > index && has:
					t.Errorf("%s held at %s already holds %s, which %s takes, a later step: it took %s before %s (it holds %v)",
						writer.name, step.name, lock, other.name, other.name, step.name, held)
				}
			}
		}
		for _, lock := range eventProjectLocks {
			if !writer.locksOwnProject && slices.Contains(held, lock) {
				t.Errorf("%s held at %s holds %s: it took the project of its event, which an issue's or a document's event does not name (it holds %v)",
					writer.name, step.name, lock, held)
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
	}
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

// storeWriterRig is the database the paths write: an issue and a project document on one
// project, a comment on the issue mentioning the session s1 and a message to s1, each delivered.
// Every write under test runs on a second server over the same dependencies whose store is one
// connection of its own, so the backend a write runs on is known before it starts and any other
// session queued behind the same hold is never taken for it.
type storeWriterRig struct {
	handler  http.Handler
	database *store.Store
	// call serves the routes, and direct calls the delivery steps, on callStore's one connection.
	call      http.Handler
	direct    *server
	callStore *store.Store
	issue     string
	document  string
	// mentioned is the comment whose mention attempt the comment delivery's settle settles, and
	// message the message whose attempt the message delivery's settle settles.
	mentioned string
	message   model.Message
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
	rig := &storeWriterRig{handler: handler, database: database, issue: issue.Key, document: document.ID}

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

// createdOrError answers nil for a 201 and the response as an error for anything else.
func createdOrError(response *httptest.ResponseRecorder) error {
	if response.Code != http.StatusCreated {
		return fmt.Errorf("status=%d body=%s", response.Code, response.Body.String())
	}
	return nil
}
