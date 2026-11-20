package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestEveryMoveOfWhoAnAskWaitsOnTakesTheStoreLockOrder holds the writes that change who an ask
// waits on (inboxwrite.QueueMoveWaiting, a site of graphstore.ToPersonArms) to the lock order
// written on api/inbox_read.go's markInboxReadThreads, with the machinery of
// TestEveryApiStoreWriterTakesTheStoreLockOrder: each lists its steps in the order it takes them,
// the list is checked against the written order (requireTheWrittenOrderAdmits), and each step is
// held in turn while the write's own backend is read at the statement it waits at and the table
// locks it holds (requireEachStepIsWaitedAtInTurn).
//
// Each takes its owner's lock first: a hand-on and a change of an issue's assignee the issue's, a
// move of a project's default the project's. A change of an issue's assignee to nobody then takes
// the project's owner lock shared before it reads the default (inboxwrite.HoldProjectDefault); and
// each move, queued in its event's batch right after the event, locks the asks rows it moves
// before the person nodes and tuples it appends in one statement, which is the step graph_tuples
// is held at.
func TestEveryMoveOfWhoAnAskWaitsOnTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	rig := newStoreWriterRig(t)
	for _, writer := range []storeWriter{
		rig.askHandOn(),
		rig.issueReassignment(false),
		rig.issueReassignment(true),
		rig.projectDefaultMove(),
	} {
		t.Run(writer.name, func(t *testing.T) {
			requireTheWrittenOrderAdmits(t, writer)
			requireEachStepIsWaitedAtInTurn(t, ctx, rig, writer)
		})
	}
}

// Each statement head a step of a move is read at.
const (
	// moveLockStatement is inboxwrite.QueueMoveWaiting's lock of the asks rows a move may move.
	moveLockStatement = "select a.id::text from asks a where a.id in ("
	// moveTuplesStatement is the head of the move's statements after its lock, the read of the
	// nodes reserved for their author and the person nodes and tuples it appends.
	moveTuplesStatement = "with held as ( select a.id::text as ask"
	// askedOfWriteStatement is putAskAskedOf's write of the ask's asked_of.
	askedOfWriteStatement = "update asks set asked_of = $2"
	// issueWriteStatement is patchIssue's one update of the issue's row.
	issueWriteStatement = "update issues set"
	// defaultWriteStatement is putProjectDefaultAssignee's write of the default.
	defaultWriteStatement = "update projects set default_assignee = $2 where key = $1"
)

// projectDefaultStep is the project's owner lock, taken shared, under which a write reads the
// default assignee an ask falls through to (inboxwrite.HoldProjectDefault), held by that lock
// taken exclusively, as a move of the default takes it.
func (r *storeWriterRig) projectDefaultStep() storeStep {
	return storeStep{
		name: "the project's owner lock, shared", order: projectStep,
		hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
			t.Helper()
			if err := store.LockOwner(ctx, holder, store.ProjectOwner(r.project)); err != nil {
				t.Fatalf("hold project %s's owner lock: %v", r.project, err)
			}
		},
		at: store.OwnersLockShared,
	}
}

// movedAsksStep is inboxwrite.QueueMoveWaiting's lock of the asks rows the move may move, after
// the write's event, held by the row of the run's ask, one of them.
func movedAsksStep() storeStep {
	return storeStep{
		name: "the asks rows the move locks", order: askStep,
		hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
			holdRow(t, ctx, holder, `select 1 from asks where id = $1::uuid for no key update`, run.ask)
		},
		at:    moveLockStatement,
		takes: []tableLock{{"asks", rowShareMode}},
	}
}

// personTuplesStep is the move's one person statement: the person nodes, `do nothing` where the
// store holds them, then the tuples, under the write's event (graphstore.ToPersonArms). A person
// node may be the store's already, so graph_tuples is held: the statement takes graph_nodes
// first, then waits at graph_tuples.
func personTuplesStep() storeStep {
	return storeStep{
		name: "the person tuples", order: tupleStep, caused: true,
		hold: holdTableWrites("graph_tuples"), at: moveTuplesStatement,
		takes: []tableLock{{"graph_nodes", rowExclusiveMode}, {"graph_tuples", rowExclusiveMode}},
	}
}

// writeStep is a write of a row the write's owner step holds, held by its table's writes.
func writeStep(name, table, statement string) storeStep {
	return storeStep{
		name: name, order: projectionStep,
		hold: holdTableWrites(table), at: statement,
		takes: []tableLock{{table, rowExclusiveMode}},
	}
}

// signedIn is a person no write has recorded, signed in, so a write may hand them an ask, an issue
// or a project's default.
func (r *storeWriterRig) signedIn(t *testing.T, prefix string) string {
	t.Helper()
	login := r.fresh(prefix)
	seedPeople(t, r.database, login)
	return login
}

// askHandOn is putAskAskedOf: the issue's owner lock, the ask's asked_of, whose update takes the
// ask's row, then the ask.asked_of event and, in its batch, the move, whose lock of the ask
// re-takes the row the write already holds, and its person tuples. Each run hands a fresh ask on
// the rig's issue, which waits on the issue's assignee, to a person no write has recorded.
func (r *storeWriterRig) askHandOn() storeWriter {
	return storeWriter{
		name: "putAskAskedOf",
		steps: []storeStep{
			ownerLockStep(store.IssueOwner(r.issue)),
			{
				name: "the ask's row, its asked_of written", order: askStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, run storeWriterRun) {
					holdRow(t, ctx, holder, `select 1 from asks where id = $1::uuid for no key update`, run.ask)
				},
				at:    askedOfWriteStatement,
				takes: []tableLock{{"asks", rowExclusiveMode}},
			},
			eventAppendStep(),
			personTuplesStep(),
		},
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			ask := createIssueAsk(t, r.handler, r.issue, "Who takes it, "+r.fresh("hand-on")+"?")
			to := r.signedIn(t, "lock-hand-on")
			return storeWriterRun{ask: ask, write: func() error {
				return okOrError(dispatchRequest(t, r.call, http.MethodPut, "/api/v1/asks/"+ask+"/asked-of",
					map[string]any{"asked_of": to}, "alice"))
			}}
		},
	}
}

// issueReassignment is patchIssue's change of an issue's assignee: the issue's owner lock, its
// write, then the issue.updated event and, right after it, the move of its asks and its person
// tuples. One left to nobody takes its project's owner lock shared right after the issue's and
// before it reads the default, since its asks go to the project's default. Each run moves the one
// open ask of an issue of its own: from a person no write has recorded to another (a change), or
// from such a person to nobody (cleared). Either takes its project's row in key share mode
// besides: the event's update of the issue's last_seq writes the row this transaction already
// updated, and Postgres then checks the row's foreign key into projects again, whatever columns
// moved.
func (r *storeWriterRig) issueReassignment(cleared bool) storeWriter {
	name := "patchIssue changing the assignee"
	var issue, ask string
	// The owner step holds the issue this writer made, which exists only once the first run is
	// readied.
	steps := []storeStep{{
		name: "the owner lock", order: ownerStep,
		hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ storeWriterRun) {
			t.Helper()
			if err := store.LockOwner(ctx, holder, store.IssueOwner(issue)); err != nil {
				t.Fatalf("hold issue %s's owner lock: %v", issue, err)
			}
		},
		at: store.OwnerLockStatement,
	}}
	if cleared {
		name = "patchIssue clearing the assignee"
		steps = append(steps, r.projectDefaultStep())
	}
	steps = append(steps, writeStep("the issue's row", "issues", issueWriteStatement),
		eventAppendStep(), movedAsksStep(), personTuplesStep())
	writer := storeWriter{
		name:            name,
		steps:           steps,
		projectKeyShare: true,
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			if issue == "" {
				created := dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/issues", map[string]any{
					"project": "LOCK", "title": "Who holds it, " + r.fresh("reassigned") + "?", "spec": r.fresh("spec") + "\n",
				}, "alice")
				if created.Code != http.StatusCreated {
					t.Fatalf("create the reassigned issue: status=%d body=%s", created.Code, created.Body.String())
				}
				issue = decodeBody[model.Issue](t, created).Key
				ask = createIssueAsk(t, r.handler, issue, "Who answers it?")
			}
			assignIssue(t, r.handler, issue, r.signedIn(t, "lock-assignee"))
			var to any
			if !cleared {
				to = r.signedIn(t, "lock-assignee")
			}
			return storeWriterRun{ask: ask, write: func() error {
				return okOrError(dispatchRequest(t, r.call, http.MethodPatch, "/api/v1/issues/"+issue,
					map[string]any{"assignee": to}, "alice"))
			}}
		},
	}
	return writer
}

// projectDefaultMove is putProjectDefaultAssignee: the project's owner lock, its row's write, then
// the project's settings event and, in its batch, the move of every ask that falls through to the
// default and its person tuples. Each run moves the default to a person no write has recorded,
// which moves an ask on the rig's project document, made by the first run.
func (r *storeWriterRig) projectDefaultMove() storeWriter {
	var ask string
	return storeWriter{
		name: "putProjectDefaultAssignee",
		steps: []storeStep{
			ownerLockStep(store.ProjectOwner(r.project)),
			writeStep("the project's row", "projects", defaultWriteStatement),
			eventAppendStep(),
			movedAsksStep(),
			personTuplesStep(),
		},
		projectOwner: true,
		ready: func(t *testing.T, ctx context.Context) storeWriterRun {
			if ask == "" {
				response := sessionRequest(t, r.handler, http.MethodPost, "/api/v1/artifacts/"+r.document+"/asks", map[string]any{
					"question": "Who does the document's question go to?",
					"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
					"actor":    sessionActor(),
				})
				if response.Code != http.StatusCreated {
					t.Fatalf("open a question on the rig's document: status=%d body=%s", response.Code, response.Body.String())
				}
				ask = decodeBody[model.Ask](t, response).ID
			}
			to := r.signedIn(t, "lock-default")
			return storeWriterRun{ask: ask, write: func() error {
				return okOrError(dispatchRequest(t, r.call, http.MethodPut, "/api/v1/projects/LOCK/default-assignee",
					map[string]any{"assignee": to}, "alice"))
			}}
		},
	}
}

// okOrError answers nil for a 200 and the response as an error for anything else.
func okOrError(response *httptest.ResponseRecorder) error {
	if response.Code != http.StatusOK {
		return fmt.Errorf("status=%d body=%s", response.Code, response.Body.String())
	}
	return nil
}
