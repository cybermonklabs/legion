package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// TestEverySettlementWriterTakesTheStoreLockOrder drives the settlement's sites of the node and
// tuple store that TestEveryDocsStoreWriterTakesTheStoreLockOrder does not, each along the
// settlement that reaches it, with that test's machinery: each path lists its steps in the order
// it takes them, the list is checked against the order written on api/inbox_read.go's
// markInboxReadThreads (requireTheDocsOrderAdmits), and each step is held in turn while the path's
// backend is read at the statement it waits at and at the table locks it holds
// (requireEachDocsStepIsWaitedAtInTurn). Every settlement takes its owner row and then its
// document's revision fence row before anything it writes (lockSettlementFence), so every write of
// each path is held to come after that row. The paths are:
//
//   - a settlement that retracts the ask of a block a person deleted, and writes the version
//     without it: the ask's row (asks.Settle, held for its event), the version's block index, the
//     artifact.version and what the index stated under it, then the ask.resolved and the
//     retraction's facts under it (causedByEvent);
//   - a settlement that versions an ask block whose question a person emptied: the block index, the
//     artifact.version and what the index stated, the block.invalid that flags the block, and the
//     reason on the ask's node under the version's event (stateInvalidReasons);
//   - a settlement whose version holds again the text a person approved, while an approval request
//     of the document is open: the block index, the request moved to the new version and its
//     ask.edited, then the carry's answer of the request (answerApprovalAsk) - the approver's
//     node, the request's row, its ask.answered and the answer's facts under it - and the
//     artifact.approved and the artifact.version after it.
//
// A step whose table the path wrote at an earlier step is held by a gate (gatedDocsWrites): a
// trigger on that table that makes the one row the step writes wait on an advisory key the holder
// takes, so the path stops at the statement that takes that step and nowhere before it.
func TestEverySettlementWriterTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	database, service := blockIndexService(t)
	rig := &docsWriterRig{database: database, service: service}
	for _, writer := range []docsWriter{rig.settledRetraction(), rig.settledInvalidAsk(), rig.settledCarry()} {
		t.Run(writer.name, func(t *testing.T) {
			requireTheDocsOrderAdmits(t, writer)
			requireEachDocsStepIsWaitedAtInTurn(t, ctx, rig, writer)
		})
	}
}

// coreIssue is the issue that owns every document of the settlement paths, whose row each path
// takes first (ownerStep).
var coreIssue = "CORE-1"

// settledRetraction is a settlement that retracts the ask of a block a person deleted from the
// live document, and writes the version without the block. The retraction goes through the one
// writer of an ask's answer state (asks.Settle), its facts held for the ask.resolved, and writes
// the ask's row before the version; the block index then states the block's node gone and deletes
// its row, the artifact.version is appended with what the index stated under it, and then the
// ask.resolved and the retraction's facts under it (causedByEvent). The ask's row, the block's
// node and row, the version's event and its tuples are the first writes of their tables, so each
// is held as its table; the ask.resolved and the retraction's facts come after a write of theirs
// and are held by their gates.
func (r *docsWriterRig) settledRetraction() docsWriter {
	// ask is the ask the run under way retracts, which its ready opens and its gate names.
	var ask string
	return docsWriter{
		name: "the settled answer's facts under its event",
		steps: []docsStep{
			r.ownerStep(), settlementFenceStep(),
			docsAskRowWriteStep("the retracted ask's row", holdDocsTableWrites("asks")),
			docsBlockNodesStep(),
			docsBlockRowsAt("delete from document_blocks"),
			docsVersionEventStep(holdDocsTableWrites("events")),
			docsCausedTuplesStep("the block index's tuple append", docsVersionEvent, holdDocsTableWrites("graph_tuples")),
			docsGatedEventStep(r, "the retraction's event", "ask.resolved"),
			// The retraction writes no node, so its facts are graphstore's plain tuple append.
			docsCausedPlainTuplesStep("the retraction's facts", "the retraction's event", gatedDocsWrites(r, "insert", "graph_tuples", func() string {
				return docsAnswerStateTuples(ask)
			})),
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			block := r.fresh("lock-retract")
			document := r.settledDocument(t, ctx, r.fresh("lock-steps-retract"), "# Retract\n\nContext.\n\n"+askBlockMarkdown(block))
			ask = blockAskOn(t, r.service, document, block).ID
			editLiveTree(t, r.service, document, func(tree *pmdoc.Node) *pmdoc.Node {
				kept := tree.Children[:0]
				for _, child := range tree.Children {
					if id, _ := child.Attrs[pmdoc.BlockIDAttr].(string); id != block {
						kept = append(kept, child)
					}
				}
				tree.Children = kept
				return tree
			})
			r.awaitDurable(t, document)
			retracted := ask
			return docsRun{document: document, write: func() error {
				if err := r.settle(ctx, document); err != nil {
					return err
				}
				return r.answerStateCaused(ctx, retracted, asks.StateResolved, "ask.resolved")
			}}
		},
	}
}

// settledInvalidAsk is a settlement of a document whose ask block, opened by an earlier
// settlement, a person left with an empty question: the block fails the strict read, the
// settlement writes its `invalid` attribute into the live tree (block.invalid) and versions the
// document, and states the reason on the ask's node under the version's event
// (stateInvalidReasons). The block index's node and row, the version's event and its tuples are
// the first writes of their tables, so each is held as its table; the block.invalid and the
// reason come after a write of theirs and are held by their gates.
func (r *docsWriterRig) settledInvalidAsk() docsWriter {
	// ask is the ask whose block the run under way empties, which its gate names.
	var ask string
	return docsWriter{
		name: "stateInvalidReasons",
		steps: []docsStep{
			r.ownerStep(), settlementFenceStep(), docsBlockNodesStep(),
			docsBlockRowsAt("delete from document_blocks"),
			docsVersionEventStep(holdDocsTableWrites("events")),
			docsCausedTuplesStep("the block index's tuple append", docsVersionEvent, holdDocsTableWrites("graph_tuples")),
			docsGatedEventStep(r, "the invalid block's event", "block.invalid"),
			docsCausedPlainTuplesStep("the ask's invalid value", docsVersionEvent, gatedDocsWrites(r, "insert", "graph_tuples", func() string {
				return "new.from_kind = 'ask' and new.from_id = " + docsLiteral(ask) + " and new.kind = 'invalid'"
			})),
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			block := r.fresh("lock-invalid")
			document := r.settledDocument(t, ctx, r.fresh("lock-steps-invalid"), "# Invalid\n\n"+askBlockMarkdown(block))
			ask = blockAskOn(t, r.service, document, block).ID
			editLiveTree(t, r.service, document, withAskQuestion(t, block, ""))
			r.awaitDurable(t, document)
			flagged := ask
			return docsRun{document: document, write: func() error {
				if err := r.settle(ctx, document); err != nil {
					return err
				}
				if _, held, err := graphstore.CurrentValue(ctx, r.database.Pool, graphstore.KindAsk, flagged, "invalid"); err != nil || !held {
					return fmt.Errorf("the node of ask %s holds no invalid value (%v): stateInvalidReasons stated none", flagged, err)
				}
				return nil
			}}
		},
	}
}

// settledCarry is a settlement whose version holds again, byte for byte, the text a person
// approved, while the approval request an agent opened over the version that changed it is open.
// The settlement indexes the version, moves the request to it (MoveApprovalAsk: the request's row
// and its ask.edited), and carries the approval (carryApproval), whose answer of the request goes
// through the one writer of an ask's answer state (answerApprovalAsk): the approver's node, then
// the request's row, then the ask.answered and the answer's facts under it. The approval's
// artifact.approved and the version's artifact.version follow. The approver is new to each run, so
// the approver's node is held as a row another transaction inserted and has not committed; the
// request's answered row, the ask.answered, the answer's facts and the two events after them come
// after a write of their tables and are held by their gates.
func (r *docsWriterRig) settledCarry() docsWriter {
	// request is the approval request the run under way answers, which its gates name.
	var request string
	return docsWriter{
		name: "answerApprovalAsk",
		steps: []docsStep{
			r.ownerStep(), settlementFenceStep(), docsBlockNodesStep(),
			docsBlockRowsAt("insert into document_blocks ("),
			docsAskRowWriteStep("the request's row moved to the version", holdDocsTableWrites("asks")),
			{
				name: "the move's event", order: docsEventStep,
				hold: holdDocsTableWrites("events"), at: docsEventStatement,
				takes: []docsTableLock{{"events", docsRowExclusiveMode}},
			},
			{
				name: "the approver's node", order: docsNodeStep, fenced: true,
				hold: holdDocsNewNode, at: docsNodeStatement,
				takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
			},
			docsAskRowWriteStep("the request's row answered", gatedDocsWrites(r, "update", "asks", func() string {
				return "new.id = " + docsLiteral(request) + "::uuid and new.state = 'answered'"
			})),
			docsGatedEventStep(r, "the answer's event", "ask.answered"),
			docsCausedTuplesStep("the answer's facts", "the answer's event", gatedDocsWrites(r, "insert", "graph_tuples", func() string {
				return docsAnswerStateTuples(request)
			})),
			docsGatedEventStep(r, "the approval's event", "artifact.approved"),
			docsGatedEventStep(r, "the version's event", "artifact.version"),
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			slug := r.fresh("lock-steps-carry")
			document := r.settledDocument(t, ctx, slug, "# Carry\n\nContext.\n")
			approver := r.fresh("lock-approver")
			approveStoredVersion(t, r.database, document, 1, approver)
			agent := model.Actor{Kind: "session", ID: r.fresh("lock-steps-agent")}
			if err := joinedWrite(r.service, func(ctx context.Context) error {
				if _, err := r.service.ApplyOps(ctx, document, []model.EditOp{{Op: "replace", Find: "Context.", With: "Context, edited."}}, agent, nil); err != nil {
					return err
				}
				_, err := r.service.SnapshotVersion(ctx, document, agent)
				return err
			}); err != nil {
				t.Fatalf("version the agent's edit of %s: %v", slug, err)
			}
			request = r.openMarkedApprovalRequest(t, ctx, document, slug+".md", agent, 2)
			alice := model.Actor{Kind: "user", ID: "alice"}
			if _, err := joinedApplyOps(r.service, document, []model.EditOp{{Op: "replace", Find: "Context, edited.", With: "Context."}}, alice, nil); err != nil {
				t.Fatalf("undo the agent's edit of %s: %v", slug, err)
			}
			r.awaitDurable(t, document)
			answered := request
			return docsRun{
				document: document,
				made:     graphstore.PersonNode(approver, alice, time.Now().UTC(), graphstore.NoEvent),
				write: func() error {
					if err := r.settle(ctx, document); err != nil {
						return err
					}
					return r.answerStateCaused(ctx, answered, asks.StateAnswered, "ask.answered")
				},
			}
		},
	}
}

// openMarkedApprovalRequest stores an open approval request by agent over version of document,
// waiting on the person, as the approval route writes one, and states it on its node as the boot
// step's backfill states a row written before the store (graphcensus.BackfillAsks), so the
// request's row names its node and the carry's answer is stated there. It answers the request.
func (r *docsWriterRig) openMarkedApprovalRequest(t *testing.T, ctx context.Context, document, name string, agent model.Actor, version int) string {
	t.Helper()
	documents := []model.AskApprovalDocument{{ArtifactID: document, Name: name, Version: version, RequestedVersion: version}}
	approval, err := EncodeApproval(&model.AskApproval{Documents: documents})
	if err != nil {
		t.Fatal(err)
	}
	var request string
	if err := r.database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, options, urgency, kind, approval)
		values ($1, jsonb_build_object('kind', 'session', 'id', $2::text), $3,
			'[{"label":"Approve"},{"label":"Request changes"}]', 'high', 'approval', $4)
		returning id::text
	`, coreIssue, agent.ID, ApprovalQuestion(documents, "Ships it."), approval).Scan(&request); err != nil {
		t.Fatalf("store the approval request of %s: %v", name, err)
	}
	if _, err := graphcensus.BackfillAsks(ctx, r.database); err != nil {
		t.Fatalf("state the approval request of %s on its node: %v", name, err)
	}
	return request
}

// answerStateCaused answers nil where the newest answer_state of ask's node is state, caused by an
// event of type eventType: the run the test followed is the one that committed.
func (r *docsWriterRig) answerStateCaused(ctx context.Context, ask, state, eventType string) error {
	var held, caused string
	err := r.database.Pool.QueryRow(ctx, `
		select t.to_id, e.type from graph_tuples t join events e on e.id = t.source_event
		where t.from_kind = $1 and t.from_id = $2 and t.kind = $3 and t.to_kind = $4
		order by t.seq desc limit 1
	`, graphstore.KindAsk, ask, graphstore.TupleAnswerState, graphstore.KindValue).Scan(&held, &caused)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("the node of ask %s holds no answer_state caused by an event", ask)
	}
	if err != nil {
		return fmt.Errorf("read the answer state of ask %s: %w", ask, err)
	}
	if held != state || caused != eventType {
		return fmt.Errorf("the node of ask %s holds answer_state %q caused by %s, want %q caused by %s", ask, held, caused, state, eventType)
	}
	return nil
}

// settledDocument writes a document of CORE-1 holding markdown through the request path and runs
// the settlement its seed leaves owed, which opens the ask of each ask block it holds.
func (r *docsWriterRig) settledDocument(t *testing.T, ctx context.Context, slug, markdown string) string {
	t.Helper()
	document := writeDocumentThroughTheRequestPath(t, r.service, r.database, slug, markdown)
	r.awaitDurable(t, document)
	if err := r.settle(ctx, document); err != nil {
		t.Fatalf("settle the seed of %s: %v", slug, err)
	}
	return document
}

// --- the steps the settlement paths share -------------------------------------------------

// docsVersionEvent is the name of a path's version event step (docsVersionEventStep), which the
// steps it causes name.
const docsVersionEvent = "the version's event"

// docsVersionEventStep is the append of the artifact.version that records a settlement's version,
// held by hold.
func docsVersionEventStep(hold func(t *testing.T, ctx context.Context, holder pgx.Tx, run docsRun)) docsStep {
	return docsStep{
		name: docsVersionEvent, order: docsEventStep,
		hold: hold, at: docsEventStatement,
		takes: []docsTableLock{{"events", docsRowExclusiveMode}},
	}
}

// docsGatedEventStep is the append of the path's event of type eventType, after an event the path
// appended before it, so it is held by its gate.
func docsGatedEventStep(r *docsWriterRig, name, eventType string) docsStep {
	return docsStep{
		name: name, order: docsEventStep,
		hold:  gatedDocsWrites(r, "insert", "events", func() string { return "new.type = " + docsLiteral(eventType) }),
		at:    docsEventStatement,
		takes: []docsTableLock{{"events", docsRowExclusiveMode}},
	}
}

// docsCausedTuplesStep is an append of tuples under the event of the step cause names, or the
// path's one event where it names none, held by hold.
func docsCausedTuplesStep(name, cause string, hold func(t *testing.T, ctx context.Context, holder pgx.Tx, run docsRun)) docsStep {
	return docsStep{
		name: name, order: docsTupleStep, caused: true, cause: cause, fenced: true,
		hold: hold, at: docsTupleStatement,
		takes: []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
	}
}

// docsCausedPlainTuplesStep is docsCausedTuplesStep for an append that carries no node of its
// write, which graphstore queues as a plain insert (docsPlainTupleStatement).
func docsCausedPlainTuplesStep(name, cause string, hold func(t *testing.T, ctx context.Context, holder pgx.Tx, run docsRun)) docsStep {
	step := docsCausedTuplesStep(name, cause, hold)
	step.at = docsPlainTupleStatement
	return step
}

// docsBlockRowsAt is the block index's document_blocks rows, written first by the statement at:
// the delete of a block the version no longer holds, or the insert of one it adds.
func docsBlockRowsAt(at string) docsStep {
	step := docsBlockRowsStep("the block nodes")
	step.at = at
	return step
}

// docsAskRowWriteStep is a write of an ask's row by asks.Settle or a move of an approval request:
// the row names the ask's node, which the path does not write, so no node step orders it. The
// settlement read the row `for no key update` before, a row share lock no hold here conflicts
// with.
func docsAskRowWriteStep(name string, hold func(t *testing.T, ctx context.Context, holder pgx.Tx, run docsRun)) docsStep {
	return docsStep{
		name: name, order: docsProjectionStep, fenced: true,
		hold: hold, at: "update asks set ",
		takes: []docsTableLock{{"asks", docsRowExclusiveMode}},
	}
}

// docsAnswerStateTuples is the gate's condition on the answer_state tuple of ask's node.
func docsAnswerStateTuples(ask string) string {
	return "new.from_kind = 'ask' and new.from_id = " + docsLiteral(ask) + " and new.kind = " + docsLiteral(graphstore.TupleAnswerState)
}

// --- the gate -----------------------------------------------------------------------------

// docsGateFunction is the trigger function every gate runs: it takes, in share mode, the advisory
// key its trigger names, which waits while a holder takes that key alone.
const docsGateFunction = `
	create or replace function lock_steps_gate() returns trigger language plpgsql as $$
	begin
		perform pg_advisory_xact_lock_shared(tg_argv[0]::bigint);
		return new;
	end $$`

// docsGateKeys hands out the advisory key of each gate, one no other gate of the test binary
// takes.
var docsGateKeys atomic.Int64

// gatedDocsWrites holds a step whose table the path wrote at an earlier step, so no hold of the
// table stops it there: a trigger before operation on table, on the rows when picks out, waits on
// an advisory key the holder takes. The trigger is made outside the holder, committed before the
// run starts, and when is read then, so it names what the run's ready wrote. It stays when the run
// is over and takes its key at each row it picks out, which nothing holds again: each gate has a
// key of its own. The write waits in the trigger, so the statement it waits at is the statement
// that writes the row, and it holds the table's row exclusive lock by then, which an earlier step
// of the same table already showed.
func gatedDocsWrites(r *docsWriterRig, operation, table string, when func() string) func(t *testing.T, ctx context.Context, holder pgx.Tx, run docsRun) {
	return func(t *testing.T, ctx context.Context, holder pgx.Tx, _ docsRun) {
		t.Helper()
		key := 1_150_000_000 + docsGateKeys.Add(1)
		trigger := fmt.Sprintf(`create trigger lock_steps_gate_%d before %s on %s for each row when (%s) execute function lock_steps_gate('%d')`,
			key, operation, table, when(), key)
		for _, statement := range []string{docsGateFunction, trigger} {
			if _, err := r.database.Pool.Exec(ctx, statement); err != nil {
				t.Fatalf("make the gate of %s: %v", table, err)
			}
		}
		if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock($1)`, key); err != nil {
			t.Fatalf("hold the gate of %s: %v", table, err)
		}
	}
}

// docsLiteral is value as a SQL string literal, for a gate's condition.
func docsLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
