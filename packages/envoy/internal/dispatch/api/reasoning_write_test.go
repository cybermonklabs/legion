package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestTheReasoningWriteStatesTheWriterItsRecordNames holds the one write path of a lens, a
// finding, a risk and a constraint (writeReasoningNodes) to what it states of who wrote each node
// (dispatch://INFRA-1950/spec#b-r-author, #b-r-node-visibility): a project node is made by the
// recorded actor and holds its authored_by tuple to the writer graphstore.AuthorOf names of that
// actor, whose own node the write makes, on a cookie, a personal token and the shared bearer
// alike; an author-only node is made by the credential that proved the caller; `author` on the
// shared bearer is refused 403 AUTHOR_NAMES_NOBODY with nothing stored; and a project node on the
// shared bearer holds its authored_by to the session its body names, whose node the write makes,
// while its made_by proves nobody (spec v46 R12: attribution is no proof). Every node is live, in
// the project of the issue the write names, and no write appends an event. A pointer the request
// states is held to its type rule by the store, so a lens of a kind that is no lens_kind is 400
// INVALID_POINTER and a lens of two kinds 409 POINTER_ALREADY_ASSERTED, each with nothing stored.
// Each attribute a node's row carries - a lens's verdict, a risk's heading, early warning and
// prevention, a constraint's standard and reference or measurement, value and where it was taken
// - is also one value tuple of the node, its whole history, as a finding's text is; and an append
// of a pointer the node holds already writes nothing, under a run as under none.
func TestTheReasoningWriteStatesTheWriterItsRecordNames(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	ctx := context.Background()
	issue := createInteractionIssue(t, handler, "RSN", "Reasoned work", "# Spec\n\nA passage.")
	var project string
	if err := database.Pool.QueryRow(ctx, `select project_key from issues where key = $1`, issue.Key).Scan(&project); err != nil {
		t.Fatalf("read the issue's project: %v", err)
	}
	bobsToken := personalTokenOwnedBy(t, database, "bob")
	eventsBefore := countRows(t, database, `select count(*) from events`)

	// The two actors a route resolves from one request: the credential that proved the caller,
	// and the actor the write records, which on a bearer is the session its body names.
	actorsOf := func(t *testing.T, header, value, session string) (proving, recorded model.Actor) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issue.Key+"/lenses", nil)
		request.Header.Set(header, value)
		proving, _, err := served.optionalActor(request)
		if err != nil {
			t.Fatalf("resolve the caller: %v", err)
		}
		var supplied *model.Actor
		if session != "" {
			supplied = &model.Actor{Kind: "session", ID: session}
		}
		recorded, err = served.actorFrom(request, supplied)
		if err != nil {
			t.Fatalf("resolve the recorded actor: %v", err)
		}
		return proving, recorded
	}
	write := func(t *testing.T, proving, recorded model.Actor, nodes ...reasoningNode) ([]graphstore.Node, error) {
		t.Helper()
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the write: %v", err)
		}
		defer tx.Rollback(ctx)
		written, err := writeReasoningNodes(ctx, tx, reasoningWrite{
			reasoningCaller: reasoningCaller{proving: proving, recorded: recorded}, issue: issue.Key, at: time.Now().UTC(), nodes: nodes,
		})
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the write: %v", err)
		}
		return written, nil
	}
	lens := func(visibility string, kinds ...string) reasoningNode {
		id := uuid.NewString()
		node := reasoningNode{
			kind: graphstore.KindLens, id: id, visibility: visibility,
			parentKind: graphstore.KindIssue, parentID: issue.Key,
		}
		for _, kind := range kinds {
			node.tuples = append(node.tuples, graphstore.Tuple{
				FromKind: graphstore.KindLens, FromID: id, Kind: graphstore.TupleHasKind, ToKind: graphstore.KindLensKind, ToID: kind,
			})
		}
		return node
	}
	requireNode := func(t *testing.T, kind, id, visibility string, madeBy model.Actor) graphstore.Node {
		t.Helper()
		node, found, err := graphstore.ReadNode(ctx, database.Pool, kind, id)
		if err != nil || !found {
			t.Fatalf("read %s/%s: found %t, %v", kind, id, found, err)
		}
		if node.Project != project || node.Parties != nil || node.State != graphstore.StateLive || node.Visibility != visibility {
			t.Errorf("%s/%s is %s, %s, in project %q between %v; want live, %s, in %q between none",
				kind, id, node.State, node.Visibility, node.Project, node.Parties, visibility, project)
		}
		if !node.MadeBy.SameCredential(madeBy) {
			t.Errorf("%s/%s is made by %+v, want %+v", kind, id, node.MadeBy, madeBy)
		}
		return node
	}
	requireNothingStored := func(t *testing.T, kind, id string) {
		t.Helper()
		if stored := countRows(t, database, `
			select (select count(*) from graph_nodes where kind = $1 and id = $2)
			     + (select count(*) from graph_tuples where (from_kind = $1 and from_id = $2) or (to_kind = $1 and to_id = $2))`,
			kind, id); stored != 0 {
			t.Errorf("the store holds %d rows and tuples of the refused %s/%s, want none", stored, kind, id)
		}
	}
	requireRefusal := func(t *testing.T, err error, status int, code, words string) {
		t.Helper()
		if err == nil {
			t.Fatalf("the write was taken, want %d %s", status, code)
		}
		recorder := httptest.NewRecorder()
		served.writeHandlerError(recorder, err)
		body := decodeBody[errorBody](t, recorder)
		if recorder.Code != status || body.Code != code || !strings.Contains(body.Error, words) {
			t.Errorf("the refusal answers %d %s %q, want %d %s holding %q", recorder.Code, body.Code, body.Error, status, code, words)
		}
	}

	t.Run("a project lens and its finding on a cookie", func(t *testing.T) {
		proving, recorded := actorsOf(t, "X-Dispatch-User", "alice", "")
		node := lens("", "trade-off")
		// A finding's node id is its lens's address and its block id, `lens/<uuid>#<block id>`,
		// which the address `dispatch://lens/<uuid>#b-<block id>` names.
		findingID := graphstore.KindLens + "/" + node.id + "#first"
		finding := reasoningNode{
			kind: "block", id: findingID, parentKind: graphstore.KindLens, parentID: node.id,
			tuples: []graphstore.Tuple{{FromKind: "block", FromID: findingID,
				Kind: "text", ToKind: graphstore.KindValue, ToID: "The first finding."}},
		}
		written, err := write(t, proving, recorded, node, finding)
		if err != nil {
			t.Fatalf("write the lens: %v", err)
		}
		if len(written) != 2 || written[0].ID != node.id || written[1].ID != finding.id {
			t.Fatalf("the write answers %+v, want the lens and its finding", written)
		}
		requireNode(t, graphstore.KindLens, node.id, graphstore.VisibilityProject, recorded)
		requireNode(t, "block", finding.id, graphstore.VisibilityProject, recorded)
		requireFarEnds(t, database, graphstore.KindLens, node.id, graphstore.TuplePartOf, "issue/"+issue.Key)
		requireFarEnds(t, database, graphstore.KindLens, node.id, graphstore.TupleHasKind, "lens_kind/trade-off")
		requireFarEnds(t, database, "block", finding.id, graphstore.TuplePartOf, "lens/"+node.id)
		requireFarEnds(t, database, graphstore.KindLens, node.id, graphstore.TupleAuthoredBy, "person/alice")
		requireFarEnds(t, database, "block", finding.id, graphstore.TupleAuthoredBy, "person/alice")
		if !nodeHeld(t, database, graphstore.KindPerson, "alice") {
			t.Error("the write states alice's authored_by and holds no node of her")
		}
	})

	t.Run("a project lens on a personal token", func(t *testing.T) {
		const session = "session-bob-0001"
		proving, recorded := actorsOf(t, "Authorization", "Bearer "+bobsToken, session)
		if nodeHeld(t, database, graphstore.KindSession, session) {
			t.Fatalf("the store holds the session %s before any write of it", session)
		}
		node := lens("", "trade-off")
		if _, err := write(t, proving, recorded, node); err != nil {
			t.Fatalf("write the lens: %v", err)
		}
		// A project node is made by the recorded actor - the session the body names, and the
		// person the token belongs to - and authored by whom AuthorOf names of it.
		made := requireNode(t, graphstore.KindLens, node.id, graphstore.VisibilityProject, recorded)
		if made.MadeBy.ID != session || made.MadeBy.Owner == nil || *made.MadeBy.Owner != "bob" {
			t.Errorf("the lens is made by %+v, want session %s of bob's token", made.MadeBy, session)
		}
		requireFarEnds(t, database, graphstore.KindLens, node.id, graphstore.TupleAuthoredBy, "session/"+session)
		if !nodeHeld(t, database, graphstore.KindSession, session) {
			t.Errorf("the write states the lens's authored_by to %s and holds no node of it", session)
		}
	})

	t.Run("an author-only lens on a personal token", func(t *testing.T) {
		proving, recorded := actorsOf(t, "Authorization", "Bearer "+bobsToken, "session-bob-0002")
		if nodeHeld(t, database, graphstore.KindPerson, "bob") {
			t.Fatal("the store holds bob's node before any write of his")
		}
		node := lens(graphstore.VisibilityAuthor, "trade-off")
		if _, err := write(t, proving, recorded, node); err != nil {
			t.Fatalf("write the lens: %v", err)
		}
		// Made by the credential that proved the caller, the token itself: no session, its owner.
		made := requireNode(t, graphstore.KindLens, node.id, graphstore.VisibilityAuthor, proving)
		if made.MadeBy.ID != "" || made.MadeBy.Owner == nil || *made.MadeBy.Owner != "bob" {
			t.Errorf("the author-only lens is made by %+v, want bob's token", made.MadeBy)
		}
		requireFarEnds(t, database, graphstore.KindLens, node.id, graphstore.TupleAuthoredBy, "person/bob")
		if !nodeHeld(t, database, graphstore.KindPerson, "bob") {
			t.Error("the write states the lens's authored_by to bob and holds no node of him")
		}
	})

	t.Run("author on the shared bearer", func(t *testing.T) {
		proving, recorded := actorsOf(t, "Authorization", "Bearer agent-token", "session-shared-0001")
		node := lens(graphstore.VisibilityAuthor, "trade-off")
		_, err := write(t, proving, recorded, node)
		requireRefusal(t, err, http.StatusForbidden, "AUTHOR_NAMES_NOBODY", "names nobody")
		requireNothingStored(t, graphstore.KindLens, node.id)
	})

	t.Run("a project lens on the shared bearer", func(t *testing.T) {
		const session = "session-shared-0002"
		proving, recorded := actorsOf(t, "Authorization", "Bearer agent-token", session)
		if nodeHeld(t, database, graphstore.KindSession, session) {
			t.Fatalf("the store holds the session %s before any write of it", session)
		}
		node := lens("", "trade-off")
		if _, err := write(t, proving, recorded, node); err != nil {
			t.Fatalf("write the lens: %v", err)
		}
		made := requireNode(t, graphstore.KindLens, node.id, graphstore.VisibilityProject, recorded)
		if made.MadeBy.Kind != "session" || made.MadeBy.ID != session {
			t.Errorf("the lens is made by %+v, want the session %s its body names", made.MadeBy, session)
		}
		// Attribution: the lens names the session its body names as its writer, as a comment
		// written on the shared bearer does, and the write states that session's node.
		requireFarEnds(t, database, graphstore.KindLens, node.id, graphstore.TupleAuthoredBy, "session/"+session)
		if !nodeHeld(t, database, graphstore.KindSession, session) {
			t.Errorf("the write states the lens's authored_by to %s and holds no node of it", session)
		}
		// Proof: the shared bearer proves no author, so the lens's made_by proves nobody.
		if graphstore.ProvesAuthor(made.MadeBy) {
			t.Errorf("the shared bearer's lens, made by %+v, proves an author", made.MadeBy)
		}
	})

	t.Run("the pointers a request states", func(t *testing.T) {
		proving, recorded := actorsOf(t, "X-Dispatch-User", "alice", "")
		strayKind := lens("")
		strayKind.tuples = []graphstore.Tuple{{
			FromKind: graphstore.KindLens, FromID: strayKind.id, Kind: graphstore.TupleHasKind,
			ToKind: graphstore.KindConstraint, ToID: uuid.NewString(),
		}}
		_, err := write(t, proving, recorded, strayKind)
		requireRefusal(t, err, http.StatusBadRequest, "INVALID_POINTER",
			"the has_kind tuple from a lens at a constraint breaks its type rule: has_kind points from a lens at a lens_kind")
		requireNothingStored(t, graphstore.KindLens, strayKind.id)

		twoKinds := lens("", "trade-off", "inversion")
		_, err = write(t, proving, recorded, twoKinds)
		requireRefusal(t, err, http.StatusConflict, "POINTER_ALREADY_ASSERTED", graphstore.ErrLensKindAsserted.Reason)
		requireNothingStored(t, graphstore.KindLens, twoKinds.id)
	})

	t.Run("each attribute a row carries is a value tuple of its node", func(t *testing.T) {
		proving, recorded := actorsOf(t, "X-Dispatch-User", "alice", "")
		premortem := lens("", graphstore.LensKindPreMortem)
		premortem.attributes = map[string]any{graphstore.AttributeVerdict: "Ship it with the index built first."}
		risk := reasoningNode{
			kind: graphstore.KindRisk, id: uuid.NewString(), parentKind: graphstore.KindLens, parentID: premortem.id,
			attributes: map[string]any{
				graphstore.AttributeHeading:      "The migration locks the table.",
				graphstore.AttributeEarlyWarning: "Writes queue behind the build.",
				graphstore.AttributePrevention:   "Build the index concurrently.",
			},
		}
		standard := reasoningNode{
			kind: graphstore.KindConstraint, id: uuid.NewString(), parentKind: graphstore.KindIssue, parentID: issue.Key,
			attributes: map[string]any{graphstore.AttributeStandard: "RFC 9110", graphstore.AttributeReference: "section 15.5.20"},
		}
		measured := reasoningNode{
			kind: graphstore.KindConstraint, id: uuid.NewString(), parentKind: graphstore.KindIssue, parentID: issue.Key,
			attributes: map[string]any{
				graphstore.AttributeMeasurement: "p95 read latency", graphstore.AttributeValue: "120 ms",
				graphstore.AttributeTakenWhere: "the staging pod",
			},
		}
		findingID := graphstore.KindLens + "/" + premortem.id + "#held"
		finding := reasoningNode{
			kind: "block", id: findingID, parentKind: graphstore.KindLens, parentID: premortem.id,
			tuples: []graphstore.Tuple{{FromKind: "block", FromID: findingID,
				Kind: graphstore.AttributeText, ToKind: graphstore.KindValue, ToID: "The index is the risk."}},
		}
		if _, err := write(t, proving, recorded, premortem, risk, standard, measured, finding); err != nil {
			t.Fatalf("write the reasoning nodes: %v", err)
		}
		finding.attributes = map[string]any{graphstore.AttributeText: "The index is the risk."}
		for _, node := range []reasoningNode{premortem, risk, standard, measured, finding} {
			stored, found, err := graphstore.ReadNode(ctx, database.Pool, node.kind, node.id)
			if err != nil || !found {
				t.Fatalf("read %s/%s: found %t, %v", node.kind, node.id, found, err)
			}
			for attribute, value := range node.attributes {
				if node.kind != "block" && stored.Attributes[attribute] != value {
					t.Errorf("%s/%s's row carries %s %v, want %q", node.kind, node.id, attribute, stored.Attributes[attribute], value)
				}
				history, err := graphstore.History(ctx, database.Pool, node.kind, node.id, attribute)
				if err != nil {
					t.Fatalf("read the history of %s on %s/%s: %v", attribute, node.kind, node.id, err)
				}
				if len(history) != 1 || history[0].ToKind != graphstore.KindValue || history[0].ToID != value ||
					!history[0].Asserted || !history[0].Who.SameCredential(recorded) || history[0].SourceEvent != graphstore.NoEvent {
					t.Errorf("the history of %s on %s/%s is %+v, want one asserted value %q by the node's writer at the sentinel",
						attribute, node.kind, node.id, history, value)
				}
			}
		}
	})

	t.Run("an append of a pointer the node holds writes nothing", func(t *testing.T) {
		proving, recorded := actorsOf(t, "X-Dispatch-User", "alice", "")
		trigger, triggered := lens("", "trade-off"), lens("", "inversion")
		if _, err := write(t, proving, recorded, trigger, triggered); err != nil {
			t.Fatalf("write the two lenses: %v", err)
		}
		// Each append is made for a run of its own, so a second append that wrote anything would
		// also name the lens changed by a second run.
		pointer := graphstore.Tuple{
			FromKind: graphstore.KindLens, FromID: triggered.id, Kind: graphstore.TupleTriggeredBy,
			ToKind: graphstore.KindLens, ToID: trigger.id, At: time.Now().UTC(), Who: recorded, SourceEvent: graphstore.NoEvent, Asserted: true,
		}
		for attempt := range 2 {
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin append %d: %v", attempt+1, err)
			}
			if err := writeReasoningPointer(graphstore.WithRun(ctx, uuid.NewString()), tx, reasoningPointer{issue: issue.Key, action: pointerAppend, pointer: pointer}); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("append %d of the triggered_by: %v", attempt+1, err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit append %d: %v", attempt+1, err)
			}
		}
		history, err := graphstore.History(ctx, database.Pool, graphstore.KindLens, triggered.id, graphstore.TupleTriggeredBy)
		if err != nil {
			t.Fatalf("read the triggered_by history: %v", err)
		}
		changed, err := graphstore.History(ctx, database.Pool, graphstore.KindLens, triggered.id, graphstore.TupleChangedByRun)
		if err != nil {
			t.Fatalf("read the changed_by_run history: %v", err)
		}
		if len(history) != 1 || len(changed) != 1 {
			t.Errorf("two appends of one triggered_by left %d triggered_by and %d changed_by_run tuples, want one of each: %+v %+v",
				len(history), len(changed), history, changed)
		}
		requireFarEnds(t, database, graphstore.KindLens, triggered.id, graphstore.TupleTriggeredBy, "lens/"+trigger.id)
	})

	if eventsAfter := countRows(t, database, `select count(*) from events`); eventsAfter != eventsBefore {
		t.Errorf("the reasoning writes appended %d events, want none", eventsAfter-eventsBefore)
	}
}

// farEnds is the far end of each current tupleKind tuple out of the node kind/id, in the order
// graphstore.CurrentOut reads them, each written kind/id.
func farEnds(t *testing.T, database *store.Store, kind, id, tupleKind string) []string {
	t.Helper()
	held, err := graphstore.CurrentOut(context.Background(), database.Pool, kind, id, []string{tupleKind})
	if err != nil {
		t.Fatalf("read the %s of %s/%s: %v", tupleKind, kind, id, err)
	}
	ends := make([]string, len(held))
	for index, tuple := range held {
		ends[index] = tuple.ToKind + "/" + tuple.ToID
	}
	return ends
}

// requireFarEnds holds the node kind/id to the far ends of its current tupleKind tuples
// (farEnds): want, in that order, and none where want is empty.
func requireFarEnds(t *testing.T, database *store.Store, kind, id, tupleKind string, want ...string) {
	t.Helper()
	if ends := farEnds(t, database, kind, id, tupleKind); strings.Join(ends, " ") != strings.Join(want, " ") {
		t.Errorf("%s/%s holds %s %v, want %v", kind, id, tupleKind, ends, want)
	}
}

// nodeHeld is whether the store holds the node kind/id, at any state.
func nodeHeld(t *testing.T, database *store.Store, kind, id string) bool {
	t.Helper()
	_, found, err := graphstore.ReadNode(context.Background(), database.Pool, kind, id)
	if err != nil {
		t.Fatalf("read %s/%s: %v", kind, id, err)
	}
	return found
}
