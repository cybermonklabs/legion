package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// statePath is the route path, under /api/v1/issues/{key}, of a node's state route or its settle,
// the node's address one escaped segment (nodePath).
func statePath(issue, address, route string) string {
	return "/api/v1/issues/" + issue + nodePath(address, route)
}

// writeStateAs is POST .../nodes/{id}/state of state on the node address names, as login's cookie.
func writeStateAs(t *testing.T, handler http.Handler, issue, address, state, login string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, statePath(issue, address, "state"), map[string]any{"state": state}, login)
}

// writeState writes state on the node address names as login, failing on anything but 200.
func (r reasoningRig) writeState(address, state, login string) nodeStateWritten {
	r.t.Helper()
	response := writeStateAs(r.t, r.handler, r.issue, address, state, login)
	if response.Code != http.StatusOK {
		r.t.Fatalf("write %s on %s as %s: status=%d body=%s", state, address, login, response.Code, response.Body.String())
	}
	return decodeBody[nodeStateWritten](r.t, response)
}

// nodeStateRead is the node state GET /api/v1/references answers for the node address names, as
// login: the state every read of the node answers.
func nodeStateRead(t *testing.T, handler http.Handler, address, login string) string {
	t.Helper()
	response := dispatchReferences(t, handler, address, login)
	if response.Code != http.StatusOK {
		t.Fatalf("read %s as %s: status=%d body=%s", address, login, response.Code, response.Body.String())
	}
	return decodeBody[model.GraphReferences](t, response).Node.State
}

// requireNodeState fails unless every read of the node address names answers want.
func requireNodeState(t *testing.T, handler http.Handler, address, login, want string) {
	t.Helper()
	if got := nodeStateRead(t, handler, address, login); got != want {
		t.Fatalf("%s reads node state %q, want %q", address, got, want)
	}
}

// proposedRead is GET /api/v1/issues/{key}/proposed as login, failing on anything but 200.
func proposedRead(t *testing.T, handler http.Handler, issue, login string) issueProposed {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/proposed", nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("read the proposed nodes of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	return decodeBody[issueProposed](t, response)
}

// proposedRefs is the address of each node a proposed read answers.
func proposedRefs(read issueProposed) []string {
	addresses := make([]string, len(read.Nodes))
	for index, node := range read.Nodes {
		addresses[index] = node.Node.Ref
	}
	return addresses
}

// newestStateTuple is the newest state tuple the store holds of the node it keys by kind and id.
func newestStateTuple(t *testing.T, database *store.Store, kind, id string) graphstore.Tuple {
	t.Helper()
	history, err := graphstore.History(context.Background(), database.Pool, kind, id, graphstore.AttributeState)
	if err != nil || len(history) == 0 {
		t.Fatalf("read the state history of %s %s: %d tuples, %v", kind, id, len(history), err)
	}
	return history[len(history)-1]
}

// stateTupleCount is how many state tuples the store holds, of every node.
func stateTupleCount(t *testing.T, database *store.Store) int64 {
	t.Helper()
	return countRows(t, database, `select count(*) from graph_tuples where kind = $1`, graphstore.AttributeState)
}

// TestTheStateRouteTakesTheFiveValuesWhereTheyApplyAndRefusesTheRest drives
// POST /api/v1/issues/{key}/nodes/{id}/state as a person's cookie, the Go twin of c-state-closed
// of the verification document. A lens moves through proposed, overtaken,
// live and gone, each read back from the node and from its newest state tuple, written by her;
// after gone it takes no value, live, proposed or overtaken: each is 409 NODE_GONE naming the
// lens, and appends no state tuple. A requirement moves through proposed, overtaken and back to
// live, and gone on it is refused and leaves it live. settled through this route is refused, and a
// word that is none of the five, `propsed`, is 400 INVALID_ATTRIBUTE_VALUE naming the attribute
// and the word, with no tuple carrying it stored and the node at the state it held. A node of the
// issue another person may not see answers that person as an id nobody wrote.
func TestTheStateRouteTakesTheFiveValuesWhereTheyApplyAndRefusesTheRest(t *testing.T) {
	rig := newReasoningRig(t)
	ctx := context.Background()
	lens := rig.invoke(rig.spec, "trade-off", "alice", nil)

	for _, state := range []string{graphstore.StateProposed, graphstore.StateOvertaken, graphstore.StateLive, graphstore.StateGone} {
		before := nodeStateRead(t, rig.handler, lens.Address, "alice")
		written := rig.writeState(lens.Address, state, "alice")
		if written.Node.State != state || written.Previous != before || written.Node.Kind != graphstore.KindLens {
			t.Fatalf("the write of %s on the lens answers %+v, want the lens at %s from %s", state, written, state, before)
		}
		newest := newestStateTuple(t, rig.database, graphstore.KindLens, lens.ID)
		if newest.ToID != state || newest.Who.Kind != "user" || newest.Who.ID != "alice" || newest.SourceEvent != graphstore.NoEvent {
			t.Fatalf("the lens's newest state tuple is %+v, want %s written by alice at no event", newest, state)
		}
		if state != graphstore.StateGone {
			requireNodeState(t, rig.handler, lens.Address, "alice", state)
		}
	}
	if node, held, err := graphstore.ReadNode(ctx, rig.database.Pool, graphstore.KindLens, lens.ID); err != nil || !held || node.State != graphstore.StateGone {
		t.Fatalf("the lens's row is %+v (held %t, %v), want it at gone", node, held, err)
	}
	held := stateTupleCount(t, rig.database)
	for _, asked := range []string{graphstore.StateLive, graphstore.StateProposed, graphstore.StateOvertaken} {
		requireGoneRefused(t, asked+" on the gone lens", writeStateAs(t, rig.handler, rig.issue, lens.Address, asked, "alice"), lens.ID)
	}
	if after := stateTupleCount(t, rig.database); after != held {
		t.Fatalf("the refused writes on the gone lens moved the state tuples from %d to %d", held, after)
	}

	for _, state := range []string{graphstore.StateProposed, graphstore.StateOvertaken, graphstore.StateLive} {
		rig.writeState(rig.requirement, state, "alice")
		requireNodeState(t, rig.handler, rig.requirement, "alice", state)
	}
	requireRefused(t, "gone on the requirement", writeStateAs(t, rig.handler, rig.issue, rig.requirement, graphstore.StateGone, "alice"),
		http.StatusConflict, codeStateNotWritable, "the block "+rig.requirement+" is live and takes no gone")
	requireNodeState(t, rig.handler, rig.requirement, "alice", graphstore.StateLive)
	requireRefused(t, "settled through the state route", writeStateAs(t, rig.handler, rig.issue, rig.requirement, graphstore.StateSettled, "alice"),
		http.StatusConflict, codeStateNotWritable, "is live and takes no settled here: a proposed node is settled by POST")

	before := stateTupleCount(t, rig.database)
	requireRefused(t, "a word that is no value", writeStateAs(t, rig.handler, rig.issue, rig.requirement, "propsed", "alice"),
		http.StatusBadRequest, "INVALID_ATTRIBUTE_VALUE", `the node state "propsed" is none of live, gone, proposed, settled, overtaken`)
	misspelled := countRows(t, rig.database, `select count(*) from graph_tuples where to_id = 'propsed'`)
	if after := stateTupleCount(t, rig.database); misspelled != 0 || after != before {
		t.Fatalf("the refused write stored %d tuples at propsed and moved the state tuples from %d to %d", misspelled, before, after)
	}
	requireNodeState(t, rig.handler, rig.requirement, "alice", graphstore.StateLive)

	hidden := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	nobody := graphstore.KindLens + "/" + uuid.NewString()
	requireNobodysAnswer(t, "bob's write on alice's hidden lens",
		writeStateAs(t, rig.handler, rig.issue, graphstore.KindLens+"/"+hidden.ID, graphstore.StateProposed, "bob"),
		writeStateAs(t, rig.handler, rig.issue, nobody, graphstore.StateProposed, "bob"),
		graphstore.KindLens+"/"+hidden.ID, nobody)
}

// requireGoneRefused fails unless response is 409 NODE_GONE naming the node whose id is id.
func requireGoneRefused(t *testing.T, name string, response *httptest.ResponseRecorder, id string) {
	t.Helper()
	requireRefused(t, name, response, http.StatusConflict, codeNodeGone, id)
}

// TestALensTakenGoneTakesItsFindingsAndRisksGone holds the gone of a lens (spec requirement
// r-proposed), the Go twin of c-state-closed's lens. alice's pre-mortem lens holds a finding and a
// risk, which she proposes and settles. A move into gone, of a lens as of a finding, a risk or a
// constraint, is a person's alone, since it takes the node's whole part_of subtree with it (spec
// v59 R5): the shared bearer's gone on the lens is 403 STATE_NOT_PERMITTED naming the lens and
// stores nothing, and so is the gone of bob's personal token, without the person scope, on a lens
// bob wrote that holds only his own finding, in the same words once the lens is substituted; that
// token marks its lens overtaken instead. A person's gone takes what stands under the lens to gone
// whether or not she may see it: bob's cookie takes gone a lens of alice's holding a risk alice
// reserved for herself, and the risk with it. alice's gone takes her lens, its finding and its
// risk gone in one write, each with a state tuple written by her, and each then reads as an
// address nobody holds and as a reference target that is gone and names nothing. gone is final:
// live, proposed and overtaken on the lens, proposed and the settle on the risk, a finding added
// to the lens, a risk written under it, a pointer out of it and a lens invoked on the risk are each
// 409 NODE_GONE naming the node written on, and a lens triggered by the gone lens, a far end, is
// still 404 NOT_FOUND. A gone lens reserved for alice answers bob as a lens nobody wrote.
func TestALensTakenGoneTakesItsFindingsAndRisksGone(t *testing.T) {
	rig := newReasoningRig(t)
	ctx := context.Background()
	const bobSession = "ses_bob_agent_00001"
	bobToken := personalTokenOwnedBy(t, rig.database, "bob")
	asToken := func(token, session, path string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body["actor"] = map[string]any{"kind": "session", "id": session}
		return agentRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+path, body, token)
	}
	premortem := func(name string, response *httptest.ResponseRecorder) lensInvoked {
		t.Helper()
		if response.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", name, response.Code, response.Body.String())
		}
		lens := decodeBody[lensInvoked](t, response)
		if len(lens.Findings) != 1 {
			t.Fatalf("%s holds %d findings, want 1", name, len(lens.Findings))
		}
		return lens
	}
	risk := func(lens string) riskWritten {
		t.Helper()
		response := rig.post("/risks", map[string]any{
			"lens": lens, "heading": "The cache fills", "early_warning": "Evictions climb", "prevention": "A size bound",
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("alice's risk under %s: status=%d body=%s", lens, response.Code, response.Body.String())
		}
		return decodeBody[riskWritten](t, response)
	}
	settle := func(address string) {
		t.Helper()
		rig.writeState(address, graphstore.StateProposed, "alice")
		if response := dispatchRequest(t, rig.handler, http.MethodPost, statePath(rig.issue, address, "settle"), nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("alice settles %s: status=%d body=%s", address, response.Code, response.Body.String())
		}
	}
	// stored is the state the store's row holds of the node it keys by kind and id.
	stored := func(kind, id string) string {
		t.Helper()
		node, held, err := graphstore.ReadNode(ctx, rig.database.Pool, kind, id)
		if err != nil || !held {
			t.Fatalf("read %s %s: held %t, %v", kind, id, held, err)
		}
		return node.State
	}

	lens := premortem("alice's pre-mortem", rig.post("/lenses", map[string]any{
		"on": rig.spec, "kind": graphstore.LensKindPreMortem, "findings": []string{"The cache holds stale reads."},
	}, "alice"))
	finding := lens.Findings[0]
	held := risk(lens.Address)
	settle(held.Address)
	requireNodeState(t, rig.handler, held.Address, "alice", graphstore.StateSettled)

	// lensGoneWords is the one refusal of a gone on the lens at address by a caller who is no
	// person, whatever the lens holds (goneNotPermitted).
	lensGoneWords := func(address string) string {
		return "this credential may not move the lens " + address +
			" to gone: a person alone takes a lens, a finding, a risk or a constraint to gone, with everything that stands under it, and its author marks it overtaken instead"
	}
	before := stateTupleCount(t, rig.database)
	shared := asToken("agent-token", "ses_shared_bearer_01", nodePath(lens.Address, "state"), map[string]any{"state": graphstore.StateGone})
	if refusal := requireRefused(t, "the shared bearer's gone on alice's lens", shared, http.StatusForbidden, codeStateNotPermitted, lens.ID); refusal.Error != lensGoneWords(lens.Address) {
		t.Fatalf("the shared bearer's gone on alice's lens is refused %q, want %q", refusal.Error, lensGoneWords(lens.Address))
	}
	if after := stateTupleCount(t, rig.database); after != before {
		t.Fatalf("the shared bearer's refused gone moved the state tuples from %d to %d", before, after)
	}
	requireNodeState(t, rig.handler, lens.Address, "alice", graphstore.StateLive)
	requireNodeState(t, rig.handler, finding.Address, "alice", graphstore.StateLive)
	requireNodeState(t, rig.handler, held.Address, "alice", graphstore.StateSettled)

	own := premortem("bob's own pre-mortem", asToken(bobToken, bobSession, "/lenses", map[string]any{
		"on": rig.spec, "kind": graphstore.LensKindPreMortem, "findings": []string{"The bill doubles."},
	}))
	before = stateTupleCount(t, rig.database)
	refused := asToken(bobToken, bobSession, nodePath(own.Address, "state"), map[string]any{"state": graphstore.StateGone})
	if refusal := requireRefused(t, "bob's gone on his own lens, holding only his own finding", refused, http.StatusForbidden, codeStateNotPermitted, own.ID); refusal.Error != lensGoneWords(own.Address) {
		t.Fatalf("bob's gone on his own lens is refused %q, want %q", refusal.Error, lensGoneWords(own.Address))
	}
	if after := stateTupleCount(t, rig.database); after != before {
		t.Fatalf("bob's refused gone moved the state tuples from %d to %d", before, after)
	}
	for _, node := range [][2]string{{graphstore.KindLens, own.ID}, {refs.BlockNodeKind, own.Findings[0].ID}} {
		if state := stored(node[0], node[1]); state != graphstore.StateLive {
			t.Fatalf("bob's refused gone on his own lens left %s %s at %s, want live", node[0], node[1], state)
		}
	}
	if moved := asToken(bobToken, bobSession, nodePath(own.Address, "state"), map[string]any{"state": graphstore.StateOvertaken}); moved.Code != http.StatusOK {
		t.Fatalf("bob marks his own lens overtaken: status=%d body=%s, want 200", moved.Code, moved.Body.String())
	}
	if state := stored(graphstore.KindLens, own.ID); state != graphstore.StateOvertaken {
		t.Fatalf("bob's own lens reads %s after he marked it overtaken", state)
	}

	hiding := premortem("alice's pre-mortem holding a risk she reserves for herself", rig.post("/lenses", map[string]any{
		"on": rig.spec, "kind": graphstore.LensKindPreMortem, "findings": []string{"The index drifts."},
	}, "alice"))
	reserved := rig.post("/risks", map[string]any{
		"lens": hiding.Address, "heading": "A hidden risk", "early_warning": "Hidden", "prevention": "Hidden",
		"visibility": graphstore.VisibilityAuthor,
	}, "alice")
	if reserved.Code != http.StatusCreated {
		t.Fatalf("alice's risk reserved for her: status=%d body=%s", reserved.Code, reserved.Body.String())
	}
	aliceOwn := decodeBody[riskWritten](t, reserved)
	if moved := writeStateAs(t, rig.handler, rig.issue, hiding.Address, graphstore.StateGone, "bob"); moved.Code != http.StatusOK {
		t.Fatalf("bob, a person, takes alice's lens gone: status=%d body=%s, want 200", moved.Code, moved.Body.String())
	}
	for _, node := range [][2]string{{graphstore.KindLens, hiding.ID}, {refs.BlockNodeKind, hiding.Findings[0].ID}, {graphstore.KindRisk, aliceOwn.ID}} {
		if state := stored(node[0], node[1]); state != graphstore.StateGone {
			t.Fatalf("bob's gone on alice's lens left %s %s at %s, want gone: the cascade takes a risk he may not see too", node[0], node[1], state)
		}
	}

	written := rig.writeState(lens.Address, graphstore.StateGone, "alice")
	if written.Previous != graphstore.StateLive || written.Node.State != graphstore.StateGone {
		t.Fatalf("alice's gone on her lens answers %+v, want it moved from live to gone", written)
	}
	for _, node := range []struct{ name, kind, id, address string }{
		{"the lens", graphstore.KindLens, lens.ID, lens.Address},
		{"its finding", refs.BlockNodeKind, finding.ID, finding.Address},
		{"its risk", graphstore.KindRisk, held.ID, held.Address},
	} {
		if state := stored(node.kind, node.id); state != graphstore.StateGone {
			t.Fatalf("after alice's gone on the lens %s reads %s by its row, want gone", node.name, state)
		}
		if newest := newestStateTuple(t, rig.database, node.kind, node.id); newest.ToID != graphstore.StateGone || newest.Who.ID != "alice" || newest.SourceEvent != graphstore.NoEvent {
			t.Fatalf("%s's newest state tuple is %+v, want gone written by alice at no event", node.name, newest)
		}
		if read := dispatchReferences(t, rig.handler, node.address, "alice"); read.Code != http.StatusNotFound {
			t.Fatalf("%s read by its own address after its lens is gone: status=%d body=%s, want 404", node.name, read.Code, read.Body.String())
		}
	}
	targets := dispatchRequest(t, rig.handler, http.MethodGet, referenceTargetsOf(finding.Address, held.Address), nil, "alice")
	if targets.Code != http.StatusOK {
		t.Fatalf("the reference targets of the finding and the risk: status=%d body=%s", targets.Code, targets.Body.String())
	}
	for index, target := range decodeBody[model.ReferenceTargets](t, targets).Targets {
		if !target.Gone || target.Name != "" {
			t.Fatalf("reference target %d after its lens is gone answers gone=%t name=%q, want gone and naming nothing", index, target.Gone, target.Name)
		}
	}
	if strings.Contains(targets.Body.String(), "stale reads") || strings.Contains(targets.Body.String(), "The cache fills") {
		t.Fatalf("the reference targets of the gone finding and risk answer their words: %s", targets.Body.String())
	}

	before = stateTupleCount(t, rig.database)
	for _, asked := range []string{graphstore.StateLive, graphstore.StateProposed, graphstore.StateOvertaken} {
		requireGoneRefused(t, asked+" on the gone lens", writeStateAs(t, rig.handler, rig.issue, lens.Address, asked, "alice"), lens.ID)
	}
	requireGoneRefused(t, "proposed on the gone risk", writeStateAs(t, rig.handler, rig.issue, held.Address, graphstore.StateProposed, "alice"), held.ID)
	requireGoneRefused(t, "the settle of the gone risk",
		dispatchRequest(t, rig.handler, http.MethodPost, statePath(rig.issue, held.Address, "settle"), nil, "alice"), held.ID)
	if after := stateTupleCount(t, rig.database); after != before {
		t.Fatalf("the refused writes on the gone nodes moved the state tuples from %d to %d", before, after)
	}
	lenses, lensTuples := rig.counts(graphstore.KindLens, graphstore.TuplePartOf)
	requireGoneRefused(t, "a finding added to the gone lens",
		rig.post(nodePath(lens.Address, "findings"), map[string]any{"text": "One more."}, "alice"), lens.ID)
	requireGoneRefused(t, "a risk written under the gone lens", rig.post("/risks", map[string]any{
		"lens": lens.Address, "heading": "Late", "early_warning": "Late", "prevention": "Late",
	}, "alice"), lens.ID)
	requireGoneRefused(t, "a pointer out of the gone lens", rig.post(nodePath(lens.Address, "pointers"), map[string]any{
		"kind": graphstore.TupleTriggeredBy, "action": "append", "to": own.Address,
	}, "alice"), lens.ID)
	requireGoneRefused(t, "a lens invoked on the gone risk",
		rig.post("/lenses", map[string]any{"on": held.Address, "kind": "trade-off"}, "alice"), held.ID)
	requireRefused(t, "a lens triggered by the gone lens", rig.post("/lenses", map[string]any{
		"on": rig.spec, "kind": "trade-off", "triggered_by": lens.Address,
	}, "alice"), http.StatusNotFound, "NOT_FOUND", "")
	if nodes, tuples := rig.counts(graphstore.KindLens, graphstore.TuplePartOf); nodes != lenses || tuples != lensTuples {
		t.Fatalf("the refused writes on the gone nodes moved the lenses from %d to %d and the part_of tuples from %d to %d",
			lenses, nodes, lensTuples, tuples)
	}
	if state := stored(graphstore.KindLens, lens.ID); state != graphstore.StateGone {
		t.Fatalf("the lens reads %s after the refused writes, want gone", state)
	}

	hidden := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	rig.writeState(hidden.Address, graphstore.StateGone, "alice")
	nobody := graphstore.KindLens + "/" + uuid.NewString()
	requireNobodysAnswer(t, "bob's write on alice's gone hidden lens",
		writeStateAs(t, rig.handler, rig.issue, graphstore.KindLens+"/"+hidden.ID, graphstore.StateLive, "bob"),
		writeStateAs(t, rig.handler, rig.issue, nobody, graphstore.StateLive, "bob"),
		graphstore.KindLens+"/"+hidden.ID, nobody)
}

// threeRequirements is a spec marking three requirements, r1, r2 and r3.
const threeRequirements = ":::requirement{#r1}\nThe cache holds every read for a minute.\n:::\n\n" +
	":::requirement{#r2}\nA write evicts what it changed.\n:::\n\n" +
	":::requirement{#r3}\nThe cache is shared by every pod.\n:::\n"

// TestTheReadsAnswerEachNodeStateAndCountNoProposedNodeAsSettled is c-proposed-page
// of the verification document: a spec of three requirements and a plan,
// nothing serving any of them. With her cookie a person writes r1 proposed, r2 proposed and then
// settles it, and r3 overtaken. Each node reads the state she wrote; the proposed read answers r1
// alone; the coverage read and the approval request's answer raise no gap for r1 and both for r2.
// She settles r1, and with no restart the same reads answer it settled, out of the proposed read
// and with both gaps. Then a check of the Verification document and a task of the plan that serve
// r2 are written and marked proposed: each is a question still open and serves nothing yet, so r2
// keeps both gaps and counts 0 of each, and the approval request's answer still names its missing
// check. She settles the check and the task, and r2 raises neither gap and counts 1 of each. Last,
// a new version of the spec changes r3's text, and r3 still reads overtaken, since the index pass
// keeps every node state that is not gone.
func TestTheReadsAnswerEachNodeStateAndCountNoProposedNodeAsSettled(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "PRP", "Proposed requirements", threeRequirements)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", "The plan holds no task yet.\n")
	spec := "dispatch://" + issue.Key + "/spec"
	r1, r2, r3 := refs.BlockRef(spec, "r1"), refs.BlockRef(spec, "r2"), refs.BlockRef(spec, "r3")

	write := func(address, state string) {
		t.Helper()
		if response := writeStateAs(t, handler, issue.Key, address, state, "alice"); response.Code != http.StatusOK {
			t.Fatalf("write %s on %s: status=%d body=%s", state, address, response.Code, response.Body.String())
		}
	}
	settle := func(address string) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, statePath(issue.Key, address, "settle"), nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("settle %s: status=%d body=%s", address, response.Code, response.Body.String())
		}
		if settled := decodeBody[nodeStateWritten](t, response); settled.Node.State != graphstore.StateSettled || settled.Previous != graphstore.StateProposed {
			t.Fatalf("the settle of %s answers %+v, want settled from proposed", address, settled)
		}
	}
	write(r1, graphstore.StateProposed)
	write(r2, graphstore.StateProposed)
	settle(r2)
	write(r3, graphstore.StateOvertaken)

	for address, want := range map[string]string{r1: graphstore.StateProposed, r2: graphstore.StateSettled, r3: graphstore.StateOvertaken} {
		requireNodeState(t, handler, address, "alice", want)
	}

	// requireCounted holds the coverage read and the approval request's answer to counting
	// counted and not proposed: both gaps for each counted requirement, none for the proposed one.
	requireCounted := func(when string, counted []string, proposed string) {
		t.Helper()
		coverage := readCoverage(t, handler, issue.Key)
		if !coverage.HasPlan {
			t.Fatalf("%s: the coverage read holds no plan, so the no-plan-task gap is never raised: %+v", when, coverage)
		}
		if got := requirementOf(t, coverage, proposed); len(got.Gaps) != 0 || got.State != graphstore.StateProposed {
			t.Errorf("%s: the coverage read raises %q for the proposed requirement %s (state %q), want no gap", when, got.Gaps, proposed, got.State)
		}
		for _, gap := range coverage.Gaps {
			if strings.HasPrefix(gap, proposed+" ") {
				t.Errorf("%s: the coverage read's gaps hold %q for the proposed requirement %s", when, gap, proposed)
			}
		}
		for _, blockID := range counted {
			got := requirementOf(t, coverage, blockID)
			if want := []string{noCheckReason, noPlanTaskReason}; !slices.Equal(got.Gaps, want) {
				t.Errorf("%s: the coverage read raises %q for %s (state %q), want %q", when, got.Gaps, blockID, got.State, want)
			}
			for _, reason := range []string{noCheckReason, noPlanTaskReason} {
				if !slices.Contains(coverage.Gaps, blockID+" "+reason) {
					t.Errorf("%s: the coverage read's gaps %q lack %q", when, coverage.Gaps, blockID+" "+reason)
				}
			}
		}
		answer := requestSpecApproval(t, handler, issue.PrimaryArtifactID)
		for _, gap := range answer.Coverage {
			if strings.HasPrefix(gap, proposed+" ") {
				t.Errorf("%s: the approval request's answer lists %q for the proposed requirement %s", when, gap, proposed)
			}
		}
		for _, blockID := range counted {
			if !slices.Contains(answer.Coverage, noCheckGap(blockID)) {
				t.Errorf("%s: the approval request's answer %q lacks %q", when, answer.Coverage, noCheckGap(blockID))
			}
		}
	}

	if got := proposedRefs(proposedRead(t, handler, issue.Key, "alice")); !slices.Equal(got, []string{r1}) {
		t.Fatalf("the proposed read answers %v, want %s alone", got, r1)
	}
	requireCounted("before the settle", []string{"r2"}, "r1")

	settle(r1)
	requireNodeState(t, handler, r1, "alice", graphstore.StateSettled)
	if got := proposedRead(t, handler, issue.Key, "alice"); len(got.Nodes) != 0 {
		t.Fatalf("the proposed read answers %v after the settle, want none", proposedRefs(got))
	}
	coverage := readCoverage(t, handler, issue.Key)
	for _, blockID := range []string{"r1", "r2"} {
		if got := requirementOf(t, coverage, blockID); !slices.Equal(got.Gaps, []string{noCheckReason, noPlanTaskReason}) || got.State != graphstore.StateSettled {
			t.Errorf("after the settle the coverage read answers %s at %q with %q, want settled with both gaps", blockID, got.State, got.Gaps)
		}
	}
	answer := requestSpecApproval(t, handler, issue.PrimaryArtifactID)
	for _, blockID := range []string{"r1", "r2"} {
		if !slices.Contains(answer.Coverage, noCheckGap(blockID)) {
			t.Errorf("after the settle the approval request's answer %q lacks %q", answer.Coverage, noCheckGap(blockID))
		}
	}

	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", checkServing("k2", r2))
	planned := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", planTaskServing("t2", r2))
	check := refs.BlockRef("dispatch://"+issue.Key+"/artifact/"+verification.Artifact.Slug, "k2")
	task := refs.BlockRef("dispatch://"+issue.Key+"/artifact/"+planned.Artifact.Slug, "t2")
	// requireServed holds the coverage read of r2 to served checks, served plan tasks and gaps.
	requireServed := func(when string, served int, gaps []string) {
		t.Helper()
		got := requirementOf(t, readCoverage(t, handler, issue.Key), "r2")
		if got.Checks != served || got.PlanTasks != served || !slices.Equal(got.Gaps, gaps) {
			t.Errorf("%s the coverage read answers r2 with %d checks, %d plan tasks and the gaps %q, want %d of each and %q",
				when, got.Checks, got.PlanTasks, got.Gaps, served, gaps)
		}
	}
	write(check, graphstore.StateProposed)
	write(task, graphstore.StateProposed)
	requireServed("with its check and its plan task proposed,", 0, []string{noCheckReason, noPlanTaskReason})
	if answer := requestSpecApproval(t, handler, issue.PrimaryArtifactID); !slices.Contains(answer.Coverage, noCheckGap("r2")) {
		t.Errorf("with r2's check proposed the approval request's answer %q lacks %q", answer.Coverage, noCheckGap("r2"))
	}
	settle(check)
	settle(task)
	requireServed("with its check and its plan task settled,", 1, []string{})
	if answer := requestSpecApproval(t, handler, issue.PrimaryArtifactID); slices.Contains(answer.Coverage, noCheckGap("r2")) {
		t.Errorf("with r2's check settled the approval request's answer %q still names %q", answer.Coverage, noCheckGap("r2"))
	}

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "replace", "find": "The cache is shared by every pod.", "with": "Each pod keeps a cache of its own."}},
		"summary": "Rewrite the overtaken requirement",
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("rewrite r3: status=%d body=%s", edited.Code, edited.Body.String())
	}
	reread := decodeBody[model.GraphReferences](t, dispatchReferences(t, handler, r3, "alice"))
	if !strings.Contains(reread.Node.Text, "Each pod keeps a cache of its own.") {
		t.Fatalf("r3 reads %q after the new version, want its new text", reread.Node.Text)
	}
	if reread.Node.State != graphstore.StateOvertaken {
		t.Fatalf("r3 reads %q after a new version changed its text, want overtaken", reread.Node.State)
	}
}

// sender is one caller of a test: the request it sends of method to path with body.
type sender = func(method, path string, body any) *httptest.ResponseRecorder

// The two agents of c-settle's tests, each a broker session that acts for a person of its own.
const (
	settleFirstSession  = "ses_first_agent_0001"
	settleSecondSession = "ses_second_agent_002"
	settleFirstPerson   = "bob"
	settleSecondPerson  = "carol"
)

// settleRigIssue is the issue a settleRig holds, as its create answers it.
type settleRigIssue struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
}

// settleRig is the server c-settle's tests drive: the broker-token rig, on a server that also
// verifies the pod tokens of issuer, holding one issue of the project brokerTestProject that
// alice, the person, made, whose spec, at spec, opens with a paragraph. alice signs in with her
// cookie (asAlice), and each of the two agents holds a broker-issued token bound to a session of
// its own (agent), the first acting for bob and the second for carol, so each proves an author and
// holds no person scope.
type settleRig struct {
	*brokerTokenRig
	issuer                     *serviceTokenIssuer
	issue                      settleRigIssue
	spec                       string
	asAlice, asFirst, asSecond sender
}

func newSettleRig(t *testing.T) settleRig {
	t.Helper()
	issuer := newServiceTokenIssuer(t)
	rig := settleRig{brokerTokenRig: newBrokerTokenRigOn(t, testServerOptions{oidc: issuer.verifier(t)}), issuer: issuer}
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": brokerTestProject, "name": "Broker"}); created.Code != http.StatusCreated {
		t.Fatalf("create the project: status=%d body=%s", created.Code, created.Body.String())
	}
	created := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": brokerTestProject, "title": "Settled by whom", "spec": "An opening paragraph.\n",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue: status=%d body=%s", created.Code, created.Body.String())
	}
	rig.issue = decodeBody[settleRigIssue](t, created)
	rig.spec = "dispatch://" + rig.issue.Key + "/spec"
	rig.asAlice = func(method, path string, body any) *httptest.ResponseRecorder {
		return dispatchRequest(t, rig.handler, method, path, body, "alice")
	}
	rig.asFirst = rig.agent(t, settleFirstSession, settleFirstPerson, "the-first-agent's-token")
	rig.asSecond = rig.agent(t, settleSecondSession, settleSecondPerson, "the-second-agent's-token")
	return rig
}

// agent is a broker-issued token bound to a session of its own that acts for person, and the
// requests it makes.
func (r settleRig) agent(t *testing.T, session, person, token string) sender {
	t.Helper()
	caller := newBrokerCaller(t)
	answer := liveNamedToken(caller.jkt)
	answer.Session, answer.Person = session, person
	r.broker.Says(token, answer)
	return func(method, path string, body any) *httptest.ResponseRecorder {
		return r.serve(caller.presenting(t, method, path, token, body))
	}
}

// posted is what as answers for body posted to path under the rig's issue, failing the test,
// naming the post name, on anything but 201.
func (r settleRig) posted(t *testing.T, name, path string, as sender, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	response := as(http.MethodPost, "/api/v1/issues/"+r.issue.Key+path, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("%s: status=%d body=%s", name, response.Code, response.Body.String())
	}
	return response
}

// invoke is the lens of body that as invokes under the rig's issue (posted).
func (r settleRig) invoke(t *testing.T, name string, as sender, body map[string]any) lensInvoked {
	t.Helper()
	return decodeBody[lensInvoked](t, r.posted(t, name, "/lenses", as, body))
}

// risk is the risk headed name that as writes under lens (posted).
func (r settleRig) risk(t *testing.T, name string, as sender, lens string) riskWritten {
	t.Helper()
	return decodeBody[riskWritten](t, r.posted(t, name, "/risks", as, map[string]any{
		"lens": lens, "heading": name, "early_warning": "It shows early.", "prevention": "It is kept from happening.",
	}))
}

// storedNodeOf is the row the store holds of the node address names, failing where it holds none.
func storedNodeOf(t *testing.T, database *store.Store, address string) graphstore.Node {
	t.Helper()
	kind, id, ok := decodeNodeAddress(address)
	if !ok {
		t.Fatalf("decode the node address %s", address)
	}
	node, held, err := graphstore.ReadNode(context.Background(), database.Pool, refs.StoredKindOf(kind), id)
	if err != nil || !held {
		t.Fatalf("read the row of %s: held %t, %v", address, held, err)
	}
	return node
}

// TestSettlingAProposedNodeIsOneWriteByAPersonOrItsAuthor is c-settle
// of the verification document (plan V18). alice signs in with her cookie and
// holds a personal token with the person scope and two without it; two agents each hold a
// broker-issued token bound to a session of its own, one for bob and one for carol, so each
// proves an author - the person a broker session that names one proves - and holds no person
// scope. The spec holds four requirements, each the block a version of its own first held: one
// the first agent wrote, two the second wrote, one written on the shared bearer, beside a fifth
// the shared bearer wrote that stays live. Each agent moves its own requirements into proposed
// itself, as their author, and alice the shared bearer's, since that bearer proves no author;
// she marks her own lens, reserved for her, proposed too. She settles the second agent's first
// proposal, and the first agent its own, each read back settled with its writer as the tuple's
// writer. The first agent is refused the second agent's other proposal, which alice's token then
// settles as her. alice's personal token without the person scope writes one more proposal, a pod
// token of another workload is refused its settle, and a second personal token of hers without
// the person scope settles it, since both of her tokens prove her (graphstore.SameAuthor). alice
// is refused a node already settled, and the shared bearer its own proposal. The first agent's
// settle of alice's hidden lens answers, byte for byte, what a lens nobody wrote answers. The
// second agent writes a third proposal of its own and settles it, and a broker session acting for
// alice settles a proposal another session of hers wrote, both sessions proving her. alice writes
// a Verification document whose one check serves the live requirement, and marks the check
// proposed. Through the state route the shared bearer is refused 403 STATE_NOT_PERMITTED, naming
// the node, its move of the requirement alice settled to live, its move of the live requirement to
// proposed and its move of alice's proposed check to live, and the second agent the same refusal
// for its move of the first agent's settled proposal to overtaken, each node reading as before and
// holding no new state tuple. After the refused move of the check the coverage read still counts
// no check serving the live requirement, and once alice, a person, moves the check to live it
// counts that one; the first agent then moves its own settled proposal to live. Last, the first
// agent writes two pre-mortems of its own, one holding only its own finding and one under which
// alice reserves a risk for herself: its gone on each is 403 STATE_NOT_PERMITTED naming the lens,
// the two answers alike byte for byte once the lens is substituted, and both lenses stay live; it
// marks the first overtaken, and alice's gone on the second takes her risk gone with it. The tree
// a gone takes whole, on the same rig (settleRig), is TestANodeTakenGoneTakesItsWholePartOfSubtree's
// (gone_subtree_test.go), and the lenses a document's removal takes gone are
// TestRemovingADocumentTakesEveryLensOnItGone's.
func TestSettlingAProposedNodeIsOneWriteByAPersonOrItsAuthor(t *testing.T) {
	settle := newSettleRig(t)
	rig, issuer, issue, spec := settle.brokerTokenRig, settle.issuer, settle.issue, settle.spec
	asFirst, asSecond := settle.asFirst, settle.asSecond
	ctx := context.Background()
	refKey := issue.Key + "/spec"

	const firstSession, secondSession = settleFirstSession, settleSecondSession
	firstPerson, secondPerson := settleFirstPerson, settleSecondPerson
	asBearer := func(method, path string, body map[string]any) *httptest.ResponseRecorder {
		body["actor"] = sessionActor()
		return rig.serve(brokerRequest(t, method, path, body, "Bearer agent-token", ""))
	}
	personal := personalTokenOwnedBy(t, rig.database, "alice", model.ScopePerson)
	asPersonalToken := func(method, path string) *httptest.ResponseRecorder {
		return rig.serve(brokerRequest(t, method, path, map[string]any{}, "Bearer "+personal, ""))
	}

	// Each requirement is the block one version first held, written by its agent.
	edits := "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/edits"
	insert := func(blockIDs ...string) map[string]any {
		var markdown strings.Builder
		for _, id := range blockIDs {
			markdown.WriteString(":::requirement{#" + id + "}\nThe requirement " + id + " holds.\n:::\n\n")
		}
		return map[string]any{
			"ops":     []map[string]string{{"op": "insert", "after": "end", "markdown": markdown.String()}},
			"summary": "Add " + strings.Join(blockIDs, " and "),
		}
	}
	for name, response := range map[string]func() *httptest.ResponseRecorder{
		"the first agent's": func() *httptest.ResponseRecorder { return asFirst(http.MethodPost, edits, insert("r-first")) },
		"the second agent's": func() *httptest.ResponseRecorder {
			return asSecond(http.MethodPost, edits, insert("r-second-a", "r-second-b"))
		},
		"the shared bearer's": func() *httptest.ResponseRecorder {
			return asBearer(http.MethodPost, edits, insert("r-shared", "r-live"))
		},
	} {
		if written := response(); written.Code != http.StatusOK {
			t.Fatalf("write %s requirement: status=%d body=%s", name, written.Code, written.Body.String())
		}
	}
	// requireMadeBy holds the requirement blockID to the author who wrote it: the test's premise.
	requireMadeBy := func(blockID string, author model.Actor) {
		t.Helper()
		node, held, err := graphstore.ReadNode(ctx, rig.database.Pool, refs.BlockNodeKind, refKey+"#"+blockID)
		if err != nil || !held || !node.MadeBy.SameCredential(author) || node.MadeBy.Proven != author.Proven {
			t.Fatalf("%s is made by %+v (held %t, %v), want %+v: the test's premise is who wrote each requirement", blockID, node.MadeBy, held, err, author)
		}
	}
	secondAgent := model.Actor{Kind: "session", ID: secondSession, Owner: &secondPerson, Proven: true}
	sharedBearer := model.Actor{Kind: "session", ID: sessionActor()["id"].(string)}
	for blockID, author := range map[string]model.Actor{
		"r-first":    {Kind: "session", ID: firstSession, Owner: &firstPerson, Proven: true},
		"r-second-a": secondAgent,
		"r-second-b": secondAgent,
		"r-shared":   sharedBearer,
		"r-live":     sharedBearer,
	} {
		requireMadeBy(blockID, author)
	}

	requirement := func(blockID string) string { return refs.BlockRef(spec, blockID) }
	stateRoute := func(blockID string) string { return statePath(issue.Key, requirement(blockID), "state") }
	toState := func(state string) map[string]any { return map[string]any{"state": state} }
	// Each agent moves its own requirements into proposed, as their author; alice, a person, moves
	// the shared bearer's, since that bearer proves no author. r-live stays live.
	for blockID, as := range map[string]func(method, path string, body any) *httptest.ResponseRecorder{
		"r-first": asFirst, "r-second-a": asSecond, "r-second-b": asSecond,
	} {
		if response := as(http.MethodPost, stateRoute(blockID), toState(graphstore.StateProposed)); response.Code != http.StatusOK {
			t.Fatalf("its author marks %s proposed: status=%d body=%s", blockID, response.Code, response.Body.String())
		}
	}
	if response := writeStateAs(t, rig.handler, issue.Key, requirement("r-shared"), graphstore.StateProposed, "alice"); response.Code != http.StatusOK {
		t.Fatalf("alice marks r-shared proposed: status=%d body=%s", response.Code, response.Body.String())
	}
	lensWritten := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/lenses", map[string]any{
		"on": spec, "kind": "trade-off", "visibility": graphstore.VisibilityAuthor,
	}, "alice")
	if lensWritten.Code != http.StatusCreated {
		t.Fatalf("write alice's hidden lens: status=%d body=%s", lensWritten.Code, lensWritten.Body.String())
	}
	hidden := decodeBody[lensInvoked](t, lensWritten)
	if response := writeStateAs(t, rig.handler, issue.Key, hidden.Address, graphstore.StateProposed, "alice"); response.Code != http.StatusOK {
		t.Fatalf("mark alice's hidden lens proposed: status=%d body=%s", response.Code, response.Body.String())
	}

	settlePath := func(address string) string { return statePath(issue.Key, address, "settle") }
	// requireSettled holds a settle to 200 and the node to settled, with writer the newest state
	// tuple's writer.
	requireSettled := func(name, blockID string, response *httptest.ResponseRecorder, writer model.Actor) {
		t.Helper()
		if response.Code != http.StatusOK {
			t.Fatalf("%s settles %s: status=%d body=%s, want 200", name, blockID, response.Code, response.Body.String())
		}
		requireNodeState(t, rig.handler, requirement(blockID), "alice", graphstore.StateSettled)
		if newest := newestStateTuple(t, rig.database, refs.BlockNodeKind, refKey+"#"+blockID); newest.ToID != graphstore.StateSettled || !newest.Who.SameAs(writer) {
			t.Fatalf("%s's newest state tuple is %s written by %+v, want settled written by %+v", blockID, newest.ToID, newest.Who, writer)
		}
	}
	// requireRefusedSettle holds a settle to its refusal, naming the node, and the node to proposed.
	requireRefusedSettle := func(name, blockID string, response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		refusal := requireRefused(t, name, response, status, code, "")
		if !strings.Contains(refusal.Error, requirement(blockID)) {
			t.Fatalf("%s is refused %q, which does not name the node %s", name, refusal.Error, requirement(blockID))
		}
	}

	alice := model.Actor{Kind: "user", ID: "alice"}
	requireSettled("alice's cookie", "r-second-a",
		dispatchRequest(t, rig.handler, http.MethodPost, settlePath(requirement("r-second-a")), nil, "alice"), alice)
	requireSettled("the first agent", "r-first",
		asFirst(http.MethodPost, settlePath(requirement("r-first")), map[string]any{}), model.Actor{Kind: "session", ID: firstSession})

	if response := asFirst(http.MethodPost, settlePath(requirement("r-second-b")), map[string]any{}); response.Code != http.StatusForbidden {
		t.Fatalf("the first agent's settle of the second agent's proposal %s answers %d %s, want 403 %s naming the node id",
			requirement("r-second-b"), response.Code, response.Body.String(), codeSettleNotPermitted)
	} else {
		requireRefusedSettle("the first agent's settle of the second agent's proposal", "r-second-b", response, http.StatusForbidden, codeSettleNotPermitted)
	}
	requireNodeState(t, rig.handler, requirement("r-second-b"), "alice", graphstore.StateProposed)

	requireSettled("alice's personal token", "r-second-b", asPersonalToken(http.MethodPost, settlePath(requirement("r-second-b"))), alice)

	// alice's personal token without the person scope writes one more proposal, which a second
	// such token of hers settles, both proving her; a pod token of another workload is refused.
	const firstTokenSession, secondTokenSession = "ses_alice_first_token", "ses_alice_second_token"
	asAliceToken := func(token, session, method, path string, body map[string]any) *httptest.ResponseRecorder {
		body["actor"] = map[string]any{"kind": "session", "id": session}
		return rig.serve(brokerRequest(t, method, path, body, "Bearer "+token, ""))
	}
	firstToken, secondToken := personalTokenOwnedBy(t, rig.database, "alice"), personalTokenOwnedBy(t, rig.database, "alice")
	if written := asAliceToken(firstToken, firstTokenSession, http.MethodPost, edits, insert("r-token")); written.Code != http.StatusOK {
		t.Fatalf("write the requirement of alice's first token: status=%d body=%s", written.Code, written.Body.String())
	}
	aliceLogin := "alice"
	if node, held, err := graphstore.ReadNode(ctx, rig.database.Pool, refs.BlockNodeKind, refKey+"#r-token"); err != nil || !held ||
		!node.MadeBy.SameCredential(model.Actor{Kind: "session", ID: firstTokenSession, Owner: &aliceLogin}) {
		t.Fatalf("r-token is made by %+v (held %t, %v), want the session alice's first token named, hers", node.MadeBy, held, err)
	}
	if response := writeStateAs(t, rig.handler, issue.Key, requirement("r-token"), graphstore.StateProposed, "alice"); response.Code != http.StatusOK {
		t.Fatalf("mark r-token proposed: status=%d body=%s", response.Code, response.Body.String())
	}
	otherWorkload := issuer.Mint(t, issuer.signing, issuer.Claims("system:serviceaccount:legion:another-workload", serviceTokenAudience))
	requireRefusedSettle("another workload's pod token's settle of alice's token's proposal", "r-token",
		asAliceToken(otherWorkload, "ses_another_workload", http.MethodPost, settlePath(requirement("r-token")), map[string]any{}),
		http.StatusForbidden, codeSettleNotPermitted)
	requireNodeState(t, rig.handler, requirement("r-token"), "alice", graphstore.StateProposed)
	requireSettled("alice's second personal token without the person scope", "r-token",
		asAliceToken(secondToken, secondTokenSession, http.MethodPost, settlePath(requirement("r-token")), map[string]any{}),
		model.Actor{Kind: "session", ID: secondTokenSession})

	requireRefusedSettle("alice's settle of a node already settled", "r-second-a",
		dispatchRequest(t, rig.handler, http.MethodPost, settlePath(requirement("r-second-a")), nil, "alice"),
		http.StatusConflict, codeNotProposed)

	requireRefusedSettle("the shared bearer's settle of its own proposal", "r-shared",
		asBearer(http.MethodPost, settlePath(requirement("r-shared")), map[string]any{}), http.StatusForbidden, codeSettleNotPermitted)
	requireNodeState(t, rig.handler, requirement("r-shared"), "alice", graphstore.StateProposed)

	nobody := graphstore.KindLens + "/" + uuid.NewString()
	hiddenID := graphstore.KindLens + "/" + hidden.ID
	seen := asFirst(http.MethodPost, settlePath(hiddenID), map[string]any{})
	unwritten := asFirst(http.MethodPost, settlePath(nobody), map[string]any{})
	t.Logf("the first agent's settle of alice's hidden lens: status=%d body=%s", seen.Code, seen.Body.String())
	t.Logf("the first agent's settle of a lens nobody wrote: status=%d body=%s", unwritten.Code, unwritten.Body.String())
	if seen.Code != unwritten.Code || unwritten.Code != http.StatusNotFound ||
		!bytes.Equal(seen.Body.Bytes(), bytes.ReplaceAll(unwritten.Body.Bytes(), []byte(nobody), []byte(hiddenID))) {
		t.Fatalf("the hidden lens answers %d %s and a lens nobody wrote %d %s, want one 404 once the id is substituted",
			seen.Code, seen.Body.String(), unwritten.Code, unwritten.Body.String())
	}
	requireNodeState(t, rig.handler, hidden.Address, "alice", graphstore.StateProposed)

	// The second agent writes a third proposal of its own, moves it into proposed as its author,
	// and settles it.
	if written := asSecond(http.MethodPost, edits, insert("r-second-c")); written.Code != http.StatusOK {
		t.Fatalf("write the second agent's third requirement: status=%d body=%s", written.Code, written.Body.String())
	}
	requireMadeBy("r-second-c", secondAgent)
	if response := asSecond(http.MethodPost, stateRoute("r-second-c"), toState(graphstore.StateProposed)); response.Code != http.StatusOK {
		t.Fatalf("mark r-second-c proposed: status=%d body=%s", response.Code, response.Body.String())
	}
	requireSettled("the second agent", "r-second-c",
		asSecond(http.MethodPost, settlePath(requirement("r-second-c")), map[string]any{}), model.Actor{Kind: "session", ID: secondSession})

	// A broker session acting for alice writes a proposal and moves it into proposed, and another
	// session of hers settles it: neither holds the person scope, and the one author rule names
	// alice for both.
	const aliceFirstSession, aliceSecondSession = "ses_alice_agent_0001", "ses_alice_agent_0002"
	asAliceFirst := settle.agent(t, aliceFirstSession, aliceLogin, "alice's-first-session-token")
	asAliceSecond := settle.agent(t, aliceSecondSession, aliceLogin, "alice's-second-session-token")
	if written := asAliceFirst(http.MethodPost, edits, insert("r-alice")); written.Code != http.StatusOK {
		t.Fatalf("write the requirement of alice's first broker session: status=%d body=%s", written.Code, written.Body.String())
	}
	requireMadeBy("r-alice", model.Actor{Kind: "session", ID: aliceFirstSession, Owner: &aliceLogin, Proven: true})
	if response := asAliceFirst(http.MethodPost, stateRoute("r-alice"), toState(graphstore.StateProposed)); response.Code != http.StatusOK {
		t.Fatalf("mark r-alice proposed: status=%d body=%s", response.Code, response.Body.String())
	}
	requireSettled("alice's second broker session", "r-alice",
		asAliceSecond(http.MethodPost, settlePath(requirement("r-alice")), map[string]any{}), model.Actor{Kind: "session", ID: aliceSecondSession})

	// alice writes a Verification document whose one check serves r-live, and marks the check
	// proposed: a question still open, which serves no requirement yet.
	verification := uploadDocument(t, rig.handler, "/api/v1/issues/"+issue.Key, "verification.md", checkServing("k-live", requirement("r-live")))
	check := refs.BlockRef("dispatch://"+issue.Key+"/artifact/"+verification.Artifact.Slug, "k-live")
	checkNode := verification.Artifact.RefKey + "#k-live"
	if response := writeStateAs(t, rig.handler, issue.Key, check, graphstore.StateProposed, "alice"); response.Code != http.StatusOK {
		t.Fatalf("alice marks her check %s proposed: status=%d body=%s", check, response.Code, response.Body.String())
	}
	checksServingLive := func() int {
		t.Helper()
		return requirementOf(t, readCoverage(t, rig.handler, issue.Key), "r-live").Checks
	}

	// Through the state route a move into or out of proposed or settled takes the settle's caller
	// rule: the shared bearer, which proves no author, makes none, and an agent moves another
	// agent's settled proposal nowhere. Each refusal names the node, which reads the state it held
	// and holds no new state tuple. The second agent's move runs while the first agent's proposal
	// is still settled, before the first agent's own move of it. A move the route takes is an
	// error and not the end of the test, so a run where the route takes them all names each node.
	stateTuples := func(storeID string) int {
		t.Helper()
		history, err := graphstore.History(ctx, rig.database.Pool, refs.BlockNodeKind, storeID, graphstore.AttributeState)
		if err != nil {
			t.Fatalf("read the state history of %s: %v", storeID, err)
		}
		return len(history)
	}
	// requireRefusedMove holds move, a write of the state of the block at address, whose store id
	// is storeID, to its refusal naming the block, and the block to held and to its state tuples.
	requireRefusedMove := func(name, address, storeID, held string, move func() *httptest.ResponseRecorder) {
		t.Helper()
		before := stateTuples(storeID)
		if before == 0 {
			t.Fatalf("the store holds no state tuple of %s at the store id %s: the test's premise is the id its tuples are keyed by", address, storeID)
		}
		response := move()
		t.Logf("%s: status=%d body=%s", name, response.Code, response.Body.String())
		refused := response.Code == http.StatusForbidden
		if refused {
			refusal := decodeBody[refusalAnswer](t, response)
			refused = refusal.Code == codeStateNotPermitted && strings.Contains(refusal.Error, address)
		}
		if !refused {
			t.Errorf("%s answers %d %s, want 403 %s naming the node %s", name, response.Code, response.Body.String(), codeStateNotPermitted, address)
		}
		if state := nodeStateRead(t, rig.handler, address, "alice"); state != held {
			t.Errorf("%s left the node %s at %s, want the %s it held", name, address, state, held)
		}
		if after := stateTuples(storeID); after != before {
			t.Errorf("%s left the node %s with %d state tuples, want the %d it held", name, address, after, before)
		}
	}
	requireRefusedMove("the shared bearer's move of "+requirement("r-second-a")+", which alice settled, to live",
		requirement("r-second-a"), refKey+"#r-second-a", graphstore.StateSettled,
		func() *httptest.ResponseRecorder {
			return asBearer(http.MethodPost, stateRoute("r-second-a"), toState(graphstore.StateLive))
		})
	requireRefusedMove("the shared bearer's move of the live "+requirement("r-live")+" to proposed",
		requirement("r-live"), refKey+"#r-live", graphstore.StateLive,
		func() *httptest.ResponseRecorder {
			return asBearer(http.MethodPost, stateRoute("r-live"), toState(graphstore.StateProposed))
		})
	requireRefusedMove("the shared bearer's move of alice's proposed check "+check+" to live",
		check, checkNode, graphstore.StateProposed,
		func() *httptest.ResponseRecorder {
			return asBearer(http.MethodPost, statePath(issue.Key, check, "state"), toState(graphstore.StateLive))
		})
	if checks := checksServingLive(); checks != 0 {
		t.Errorf("after the shared bearer's move of the proposed check %s the coverage read counts %d checks serving %s, want 0",
			check, checks, requirement("r-live"))
	}
	// alice, a person, ends her own proposal, and the check then serves r-live: the count of 0
	// above is of this check, which a move out of proposed would count.
	if response := writeStateAs(t, rig.handler, issue.Key, check, graphstore.StateLive, "alice"); response.Code != http.StatusOK {
		t.Fatalf("alice moves her proposed check %s to live: status=%d body=%s", check, response.Code, response.Body.String())
	}
	if checks := checksServingLive(); checks != 1 {
		t.Fatalf("with alice's check %s live the coverage read counts %d checks serving %s, want 1", check, checks, requirement("r-live"))
	}
	requireRefusedMove("the second agent's move of the first agent's settled "+requirement("r-first")+" to overtaken",
		requirement("r-first"), refKey+"#r-first", graphstore.StateSettled,
		func() *httptest.ResponseRecorder {
			return asSecond(http.MethodPost, stateRoute("r-first"), toState(graphstore.StateOvertaken))
		})
	moved := asFirst(http.MethodPost, stateRoute("r-first"), toState(graphstore.StateLive))
	if moved.Code != http.StatusOK {
		t.Fatalf("the first agent's move of its own settled %s to live: status=%d body=%s, want 200",
			requirement("r-first"), moved.Code, moved.Body.String())
	}
	if written := decodeBody[nodeStateWritten](t, moved); written.Previous != graphstore.StateSettled || written.Node.State != graphstore.StateLive {
		t.Fatalf("the first agent's move of its own settled proposal answers %+v, want it moved from settled to live", written)
	}
	requireNodeState(t, rig.handler, requirement("r-first"), "alice", graphstore.StateLive)
	if newest := newestStateTuple(t, rig.database, refs.BlockNodeKind, refKey+"#r-first"); newest.ToID != graphstore.StateLive ||
		!newest.Who.SameAs(model.Actor{Kind: "session", ID: firstSession}) {
		t.Fatalf("r-first's newest state tuple is %s written by %+v, want live written by the first agent", newest.ToID, newest.Who)
	}

	// A lens's move into gone is a person's alone, whatever the lens holds (goneNotPermitted):
	// the first agent's gone on each of two pre-mortems of its own, one holding only its own
	// finding and one under which alice reserves a risk for herself, is refused, the two answers
	// equal byte for byte once the lens is substituted and each lens still at live. The agent marks
	// the first overtaken instead, and alice's gone on the second takes her risk gone with it.
	agentLens := func(body map[string]any) lensInvoked {
		t.Helper()
		body["on"], body["kind"] = spec, graphstore.LensKindPreMortem
		written := asFirst(http.MethodPost, "/api/v1/issues/"+issue.Key+"/lenses", body)
		if written.Code != http.StatusCreated {
			t.Fatalf("the first agent's pre-mortem: status=%d body=%s", written.Code, written.Body.String())
		}
		return decodeBody[lensInvoked](t, written)
	}
	ownOnly := agentLens(map[string]any{"findings": []string{"The first agent's own finding."}})
	holding := agentLens(map[string]any{})
	reserved := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/risks", map[string]any{
		"lens": holding.Address, "heading": "alice's own risk", "early_warning": "She sees it coming",
		"prevention": "She keeps it from happening", "visibility": graphstore.VisibilityAuthor,
	}, "alice")
	if reserved.Code != http.StatusCreated {
		t.Fatalf("alice's risk reserved for her under the first agent's lens: status=%d body=%s", reserved.Code, reserved.Body.String())
	}
	aliceRisk := decodeBody[riskWritten](t, reserved)
	lensStateTuples := func() int64 {
		return countRows(t, rig.database, `select count(*) from graph_tuples where kind = $1 and from_kind = $2`,
			graphstore.AttributeState, graphstore.KindLens)
	}
	before := lensStateTuples()
	answered := map[string]string{}
	for _, lens := range []lensInvoked{ownOnly, holding} {
		response := asFirst(http.MethodPost, statePath(issue.Key, lens.Address, "state"), toState(graphstore.StateGone))
		requireRefused(t, "the first agent's gone on its own lens "+lens.Address, response, http.StatusForbidden, codeStateNotPermitted, lens.Address)
		answered[lens.ID] = strings.ReplaceAll(response.Body.String(), lens.ID, "<lens>")
		if state := storedNodeOf(t, rig.database, lens.Address).State; state != graphstore.StateLive {
			t.Errorf("the first agent's refused gone left its lens %s at %s, want live", lens.Address, state)
		}
	}
	if answered[ownOnly.ID] != answered[holding.ID] {
		t.Errorf("the first agent's gone on its lens holding only its own finding answers %s, and on its lens holding alice's hidden risk %s: want the two alike",
			answered[ownOnly.ID], answered[holding.ID])
	}
	if after := lensStateTuples(); after != before {
		t.Errorf("the first agent's refused gones moved the lenses' state tuples from %d to %d", before, after)
	}
	if marked := asFirst(http.MethodPost, statePath(issue.Key, ownOnly.Address, "state"), toState(graphstore.StateOvertaken)); marked.Code != http.StatusOK {
		t.Fatalf("the first agent marks its own lens %s overtaken: status=%d body=%s, want 200", ownOnly.Address, marked.Code, marked.Body.String())
	}
	if state := storedNodeOf(t, rig.database, ownOnly.Address).State; state != graphstore.StateOvertaken {
		t.Fatalf("the first agent's lens %s reads %s after it marked it overtaken", ownOnly.Address, state)
	}
	if taken := writeStateAs(t, rig.handler, issue.Key, holding.Address, graphstore.StateGone, "alice"); taken.Code != http.StatusOK {
		t.Fatalf("alice takes the first agent's lens %s gone: status=%d body=%s, want 200", holding.Address, taken.Code, taken.Body.String())
	}
	if node, held, err := graphstore.ReadNode(ctx, rig.database.Pool, graphstore.KindRisk, aliceRisk.ID); err != nil || !held || node.State != graphstore.StateGone {
		t.Fatalf("alice's gone on %s left her risk %s at %q (held %t, %v), want gone with its lens", holding.Address, aliceRisk.Address, node.State, held, err)
	}
}

// TestATokenThatProvesNoIDOfItsOwnSettlesWhatItWrote holds the settle, and the read of what a
// token reserved for itself, to the root's one author rule (graphstore.SameAuthor, spec v43 R5)
// for the two credentials that prove no id of their own: a personal token without the person
// scope, which proves its owner, and a pod's service-account token, which proves its workload. A
// lens either writes project-visible is made by the session its body names, with that owner or
// that workload beside it, and one it reserves for its author by the credential itself
// (writeReasoningNodes); the rule reads both as the owner's or the workload's. Each settles both
// of its own proposals, and is refused 403 SETTLE_NOT_PERMITTED, naming the node, on a proposal a
// token of another owner, another workload or the other kind of credential wrote. A pod reads the
// lens it reserved for itself, and a pod of another workload reads it, and is refused its settle,
// as a lens nobody wrote. The shared bearer settles nothing, not even the proposal it wrote, and
// nothing a caller is refused moves a node. Each writer marks its own lens proposed, as its
// author, but for the shared bearer, which proves no author: alice, a person, marks its lens.
func TestATokenThatProvesNoIDOfItsOwnSettlesWhatItWrote(t *testing.T) {
	ctx := context.Background()
	issuer := newServiceTokenIssuer(t)
	handler, database, _ := newTestServer(t, testServerOptions{oidc: issuer.verifier(t)})
	issue := createInteractionIssue(t, handler, "SET", "Settled by agents", "A paragraph to look at.\n")
	spec := "dispatch://" + issue.Key + "/spec"
	pod := func(subject string) string {
		return issuer.Mint(t, issuer.signing, issuer.Claims(subject, serviceTokenAudience))
	}
	type writer struct{ name, token, session string }
	var (
		bob     = writer{"bob's personal token", personalTokenOwnedBy(t, database, "bob"), "ses_bob_agent_00001"}
		carol   = writer{"carol's personal token", personalTokenOwnedBy(t, database, "carol"), "ses_carol_agent_001"}
		workerA = writer{"worker a's pod token", pod("system:serviceaccount:legion:worker-a"), "ses_worker_a_000001"}
		workerB = writer{"worker b's pod token", pod("system:serviceaccount:legion:worker-b"), "ses_worker_b_000001"}
		shared  = writer{"the shared bearer", "agent-token", "ses_shared_bearer_01"}
	)
	post := func(as writer, path string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body["actor"] = map[string]any{"kind": "session", "id": as.session}
		return agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+path, body, as.token)
	}
	// propose writes one lens on the spec as by at visibility, marks it proposed as by - or as
	// alice's cookie for the shared bearer, which proves no author and so moves no node into
	// proposed (maySettle) - and answers its store address, after holding its made_by to madeBy:
	// the test's premise.
	propose := func(by writer, visibility string, madeBy model.Actor) string {
		t.Helper()
		written := post(by, "/lenses", map[string]any{"on": spec, "kind": "trade-off", "visibility": visibility})
		if written.Code != http.StatusCreated {
			t.Fatalf("%s's %s lens: status=%d body=%s", by.name, visibility, written.Code, written.Body.String())
		}
		lens := decodeBody[lensInvoked](t, written)
		node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindLens, lens.ID)
		if err != nil || !held || !node.MadeBy.SameCredential(madeBy) || node.MadeBy.Proven {
			t.Fatalf("%s's %s lens is made by %+v (held %t, %v), want %+v", by.name, visibility, node.MadeBy, held, err, madeBy)
		}
		address := graphstore.KindLens + "/" + lens.ID
		var marked *httptest.ResponseRecorder
		if by == shared {
			marked = writeStateAs(t, handler, issue.Key, address, graphstore.StateProposed, "alice")
		} else {
			marked = post(by, nodePath(address, "state"), map[string]any{"state": graphstore.StateProposed})
		}
		if marked.Code != http.StatusOK {
			t.Fatalf("the proposed mark on %s's %s lens: status=%d body=%s", by.name, visibility, marked.Code, marked.Body.String())
		}
		return address
	}
	refused := func(as writer, address, whose string) {
		t.Helper()
		response := post(as, nodePath(address, "settle"), map[string]any{})
		refusal := requireRefused(t, as.name+"'s settle of "+whose, response, http.StatusForbidden, codeSettleNotPermitted, "")
		if !strings.Contains(refusal.Error, address) {
			t.Fatalf("%s's settle of %s is refused %q, which does not name the node %s", as.name, whose, refusal.Error, address)
		}
		if state := storedNodeOf(t, database, address).State; state != graphstore.StateProposed {
			t.Fatalf("%s's refused settle left %s at %s, want proposed", as.name, whose, state)
		}
	}
	settled := func(as writer, address, whose string) {
		t.Helper()
		response := post(as, nodePath(address, "settle"), map[string]any{})
		t.Logf("%s's settle of %s: status=%d body=%s", as.name, whose, response.Code, response.Body.String())
		if response.Code != http.StatusOK {
			t.Fatalf("%s's settle of %s: status=%d body=%s, want 200", as.name, whose, response.Code, response.Body.String())
		}
		if state := storedNodeOf(t, database, address).State; state != graphstore.StateSettled {
			t.Fatalf("%s's settle left %s at %s, want settled", as.name, whose, state)
		}
	}
	// unseen holds as's answer to target, the read or the settle of the node address, to what
	// the same request answers for a lens nobody wrote, once the address is substituted.
	unseen := func(as writer, address, whose string, request func(as writer, address string) *httptest.ResponseRecorder) {
		t.Helper()
		requireAnswersAsALensNobodyWrote(t, as.name+" on "+whose, address, func(address string) *httptest.ResponseRecorder {
			return request(as, address)
		})
		if state := storedNodeOf(t, database, address).State; state != graphstore.StateProposed {
			t.Fatalf("%s's request left %s at %s, want proposed", as.name, whose, state)
		}
	}
	settle := func(as writer, address string) *httptest.ResponseRecorder {
		return post(as, nodePath(address, "settle"), map[string]any{})
	}
	read := func(as writer, address string) *httptest.ResponseRecorder {
		return agentRequest(t, handler, http.MethodGet, "/api/v1/lenses?on="+url.QueryEscape(address), nil, as.token)
	}

	owner, subject := "bob", "system:serviceaccount:legion:worker-a"
	bobsProject := propose(bob, graphstore.VisibilityProject, model.Actor{Kind: "session", ID: bob.session, Owner: &owner})
	bobsOwn := propose(bob, graphstore.VisibilityAuthor, model.Actor{Kind: "session", Owner: &owner})
	podsProject := propose(workerA, graphstore.VisibilityProject, model.Actor{Kind: "session", ID: workerA.session, Service: &subject})
	podsOwn := propose(workerA, graphstore.VisibilityAuthor, model.Actor{Service: &subject})
	sharedProject := propose(shared, graphstore.VisibilityProject, model.Actor{Kind: "session", ID: shared.session})

	// A body's proven mark proves nothing: the session a pod's body names, claiming its credential
	// proved it, is recorded with no mark, so it is no session's own write but the workload's.
	claimed := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/lenses", map[string]any{
		"on": spec, "kind": "trade-off",
		"actor": map[string]any{"kind": "session", "id": workerA.session, "proven": true},
	}, workerA.token)
	if claimed.Code != http.StatusCreated {
		t.Fatalf("worker a's lens with a body claiming proven: status=%d body=%s", claimed.Code, claimed.Body.String())
	}
	if node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindLens, decodeBody[lensInvoked](t, claimed).ID); err != nil || !held ||
		node.MadeBy.Proven || !node.MadeBy.SameCredential(model.Actor{Kind: "session", ID: workerA.session, Service: &subject}) {
		t.Fatalf("the lens whose body claimed proven is made by %+v (held %t, %v), want the body's session with no mark", node.MadeBy, held, err)
	}
	for address, whose := range map[string]string{podsOwn: "the lens it reserved for itself", podsProject: "its project lens"} {
		if own := read(workerA, address); own.Code != http.StatusOK {
			t.Fatalf("worker a's read of %s: status=%d body=%s", whose, own.Code, own.Body.String())
		} else if lens := decodeBody[nodeLenses](t, own).Lens; lens == nil || lens.Address != "dispatch://"+address {
			t.Fatalf("worker a reads %s as %+v, want %s", whose, lens, address)
		}
	}
	if lens := decodeBody[nodeLenses](t, read(workerA, podsOwn)).Lens; lens.Visibility != graphstore.VisibilityAuthor {
		t.Fatalf("worker a reads the lens it reserved for itself at visibility %q, want author", lens.Visibility)
	}
	unseen(workerB, podsOwn, "the read of worker a's own lens", read)
	for _, other := range []writer{carol, workerA, shared} {
		refused(other, bobsProject, "bob's project lens")
	}
	for _, other := range []writer{workerB, bob, shared} {
		refused(other, podsProject, "worker a's project lens")
	}
	unseen(workerB, podsOwn, "the settle of worker a's own lens", settle)
	for _, anyone := range []writer{shared, bob, workerA} {
		refused(anyone, sharedProject, "the shared bearer's project lens")
	}
	settled(bob, bobsProject, "its own project lens")
	settled(bob, bobsOwn, "the lens it reserved for itself")
	settled(workerA, podsProject, "its own project lens")
	settled(workerA, podsOwn, "the lens it reserved for itself")
}

// TestABrokerSessionOfAWorkloadIsNotThatWorkload holds the one author rule where a workload's
// credentials meet: a pod token of one workload, and two sessions the broker verified for that
// same workload and no person. The pod's write records the workload (graphstore.SameAuthor reads
// a session that is not proven beside a service as that service), so the pod reads and settles
// the lens it reserved for itself. Each broker session's credential is Proven (brokerTokenActor),
// so its author is its own session and not the workload: it reads that lens
// as a lens nobody wrote, byte for byte once the address is substituted. Such a session writes
// nothing at all, since no person stands behind it (writeNeedsASignedInPerson), so its settle is
// refused 403 SIGN_IN_REQUIRED and the lens stays proposed; two such sessions therefore author no
// proposal to be told apart by, and what tells them from the workload is the read.
func TestABrokerSessionOfAWorkloadIsNotThatWorkload(t *testing.T) {
	ctx := context.Background()
	issuer := newServiceTokenIssuer(t)
	rig := newBrokerTokenRigOn(t, testServerOptions{oidc: issuer.verifier(t)})
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": brokerTestProject, "name": "Broker"}); created.Code != http.StatusCreated {
		t.Fatalf("create the project: status=%d body=%s", created.Code, created.Body.String())
	}
	created := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": brokerTestProject, "title": "One workload, its pod and its sessions", "spec": "An opening paragraph.\n",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
	spec := "dispatch://" + issue + "/spec"

	workload := brokerTestService
	type caller struct {
		name string
		send func(method, path string, body map[string]any) *httptest.ResponseRecorder
	}
	session := func(name, id, token string) caller {
		broker := newBrokerCaller(t)
		answer := serviceOnlyToken(broker.jkt)
		answer.Session = id
		rig.broker.Says(token, answer)
		return caller{name, func(method, path string, body map[string]any) *httptest.ResponseRecorder {
			if body == nil {
				return rig.serve(broker.presenting(t, method, path, token, nil))
			}
			return rig.serve(broker.presenting(t, method, path, token, body))
		}}
	}
	podToken := issuer.Mint(t, issuer.signing, issuer.Claims(workload, serviceTokenAudience))
	pod := caller{"the workload's pod token", func(method, path string, body map[string]any) *httptest.ResponseRecorder {
		if body == nil {
			return rig.serve(brokerRequest(t, method, path, nil, "Bearer "+podToken, ""))
		}
		body["actor"] = map[string]any{"kind": "session", "id": "ses_workload_pod"}
		return rig.serve(brokerRequest(t, method, path, body, "Bearer "+podToken, ""))
	}}
	sessions := []caller{
		session("the workload's first broker session", "ses_workload_first", "the-workload's-first-token"),
		session("the workload's second broker session", "ses_workload_second", "the-workload's-second-token"),
	}

	written := pod.send(http.MethodPost, "/api/v1/issues/"+issue+"/lenses", map[string]any{
		"on": spec, "kind": "trade-off", "visibility": graphstore.VisibilityAuthor,
	})
	if written.Code != http.StatusCreated {
		t.Fatalf("the pod's own lens: status=%d body=%s", written.Code, written.Body.String())
	}
	lensID := decodeBody[lensInvoked](t, written).ID
	lens := graphstore.KindLens + "/" + lensID
	if node, held, err := graphstore.ReadNode(ctx, rig.database.Pool, graphstore.KindLens, lensID); err != nil || !held ||
		!node.MadeBy.SameCredential(model.Actor{Service: &workload}) || node.MadeBy.Proven {
		t.Fatalf("the pod's lens is made by %+v (held %t, %v), want the workload with no proven mark", node.MadeBy, held, err)
	}
	if marked := pod.send(http.MethodPost, statePath(issue, lens, "state"), map[string]any{"state": graphstore.StateProposed}); marked.Code != http.StatusOK {
		t.Fatalf("the pod marks its lens proposed: status=%d body=%s", marked.Code, marked.Body.String())
	}
	read := func(as caller, address string) *httptest.ResponseRecorder {
		return as.send(http.MethodGet, "/api/v1/lenses?on="+url.QueryEscape(address), nil)
	}

	if own := read(pod, lens); own.Code != http.StatusOK {
		t.Fatalf("the pod's read of its own lens: status=%d body=%s", own.Code, own.Body.String())
	} else if answered := decodeBody[nodeLenses](t, own).Lens; answered == nil || answered.Address != "dispatch://"+lens ||
		answered.Visibility != graphstore.VisibilityAuthor {
		t.Fatalf("the pod reads its own lens as %+v, want %s at visibility author", answered, lens)
	}
	for _, as := range sessions {
		requireAnswersAsALensNobodyWrote(t, as.name+"'s read of the pod's lens", lens, func(address string) *httptest.ResponseRecorder {
			return read(as, address)
		})
		requireRefused(t, as.name+"'s settle of the pod's lens", as.send(http.MethodPost, statePath(issue, lens, "settle"), map[string]any{}),
			http.StatusForbidden, contracts.DispatchSignInRequired, "no person")
		if state := storedNodeOf(t, rig.database, lens).State; state != graphstore.StateProposed {
			t.Fatalf("%s's refused settle left the pod's lens at %s, want proposed", as.name, state)
		}
	}
	if settled := pod.send(http.MethodPost, statePath(issue, lens, "settle"), map[string]any{}); settled.Code != http.StatusOK {
		t.Fatalf("the pod's settle of its own lens: status=%d body=%s, want 200", settled.Code, settled.Body.String())
	}
	if state := storedNodeOf(t, rig.database, lens).State; state != graphstore.StateSettled {
		t.Fatalf("the pod's settle left its own lens at %s, want settled", state)
	}
}

// requireAnswersAsALensNobodyWrote holds what request answers for the lens address to the 404 the
// same request answers for a lens nobody wrote, byte for byte once that lens's address is
// substituted: a node the caller may not see is told by nothing from one that does not exist.
func requireAnswersAsALensNobodyWrote(t *testing.T, what, address string, request func(address string) *httptest.ResponseRecorder) {
	t.Helper()
	nobody := graphstore.KindLens + "/" + uuid.NewString()
	seen, unwritten := request(address), request(nobody)
	t.Logf("%s: status=%d body=%s", what, seen.Code, seen.Body.String())
	if seen.Code != http.StatusNotFound || unwritten.Code != http.StatusNotFound ||
		!bytes.Equal(seen.Body.Bytes(), bytes.ReplaceAll(unwritten.Body.Bytes(), []byte(nobody), []byte(address))) {
		t.Fatalf("%s answers %d %s and on a lens nobody wrote %d %s, want one 404 once the id is substituted",
			what, seen.Code, seen.Body.String(), unwritten.Code, unwritten.Body.String())
	}
}

// TestAStateWriteKeepsTheCauseOfTheBlockItMoves holds the state route to moving a block's state
// and nothing else of its row (graphstore.Node.KeepsCause). The requirement's row keeps the event
// that caused it across proposed, live and proposed again, and each move is one state tuple at no
// event, written by her, three in all, since no repeat key drops a tuple at the sentinel. A new
// version of the spec that rewrites another block then leaves the requirement's cause as it was:
// its row was never at the sentinel, which that version's index pass moves its own event onto.
func TestAStateWriteKeepsTheCauseOfTheBlockItMoves(t *testing.T) {
	rig := newReasoningRig(t)
	ctx := context.Background()
	cause := func() int64 {
		t.Helper()
		var event int64
		if err := rig.database.Pool.QueryRow(ctx, `select source_event from graph_nodes where kind = $1 and id = $2`,
			refs.BlockNodeKind, rig.requirementNode).Scan(&event); err != nil {
			t.Fatalf("read the cause of the requirement's row: %v", err)
		}
		return event
	}
	caused := cause()
	if caused == graphstore.NoEvent {
		t.Fatalf("the requirement's row is at the sentinel before any write of its state, so a cause kept reads as one lost")
	}

	moves := []string{graphstore.StateProposed, graphstore.StateLive, graphstore.StateProposed}
	for _, state := range moves {
		rig.writeState(rig.requirement, state, "alice")
		if got := cause(); got != caused {
			t.Fatalf("after the write of %s the requirement's row is caused by event %d, want %d, the event that caused it", state, got, caused)
		}
	}
	history, err := graphstore.History(ctx, rig.database.Pool, refs.BlockNodeKind, rig.requirementNode, graphstore.AttributeState)
	if err != nil {
		t.Fatalf("read the requirement's state tuples: %v", err)
	}
	if len(history) != 1+len(moves) {
		t.Fatalf("the requirement holds %d state tuples %+v, want the one its first write stated and one for each of %v", len(history), history, moves)
	}
	for index, state := range moves {
		if tuple := history[1+index]; tuple.ToID != state || tuple.SourceEvent != graphstore.NoEvent || tuple.Who.ID != "alice" {
			t.Errorf("state tuple %d of the requirement is %+v, want %s written by alice at no event", 1+index, tuple, state)
		}
	}

	var specID string
	if err := rig.database.Pool.QueryRow(ctx, `select id::text from artifacts where issue_key = $1 and is_primary`, rig.issue).Scan(&specID); err != nil {
		t.Fatalf("read the spec's id: %v", err)
	}
	edited := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/artifacts/"+specID+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "replace", "find": "An opening paragraph.", "with": "A rewritten opening paragraph."}},
		"summary": "Rewrite the opening paragraph",
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("rewrite the opening paragraph: status=%d body=%s", edited.Code, edited.Body.String())
	}
	var indexed bool
	if err := rig.database.Pool.QueryRow(ctx, `
		select exists (select 1 from document_blocks where artifact_id = $1::uuid and excerpt like '%rewritten opening%')
	`, specID).Scan(&indexed); err != nil || !indexed {
		t.Fatalf("the block index holds no block of the rewritten paragraph (%v), so no index pass of the new version ran", err)
	}
	if got := cause(); got != caused {
		t.Fatalf("after a version that rewrote another block the requirement's row is caused by event %d, want %d", got, caused)
	}
	requireNodeState(t, rig.handler, rig.requirement, "alice", graphstore.StateProposed)
}

// TestTheStateRouteRefusesABlockOfACommentInWordsThatAreTrue: the store keeps no node of a block
// of an ask, a comment or a message, so the state route refuses proposed, overtaken and live on a
// comment's block with 409 NODE_STATE_NOT_WRITABLE saying that, naming the block, and stores no
// tuple and no row; the block still reads live, and the settle refuses it as a node that is not
// proposed.
func TestTheStateRouteRefusesABlockOfACommentInWordsThatAreTrue(t *testing.T) {
	rig := newReasoningRig(t)
	ctx := context.Background()
	comment := createThreadComment(t, rig.handler, rig.issue, map[string]any{"body": "The first remark.\n\nThe second remark."}, "alice")
	node := "comment/" + comment.ID + "#b1"
	block := "dispatch://" + rig.issue + "/comment/" + comment.ID + "#b-b1"
	stored := func() (rows, tuples int) {
		t.Helper()
		if err := rig.database.Pool.QueryRow(ctx, `
			select (select count(*) from graph_nodes where id = $1), (select count(*) from graph_tuples where from_id = $1)
		`, node).Scan(&rows, &tuples); err != nil {
			t.Fatalf("count the rows and the tuples of the comment's block: %v", err)
		}
		return rows, tuples
	}
	rows, tuples := stored()
	if rows != 0 {
		t.Fatalf("the store holds %d rows of the comment's block %s, so the route has a node to write and this refusal is no longer true", rows, node)
	}

	for _, state := range []string{graphstore.StateProposed, graphstore.StateOvertaken, graphstore.StateLive} {
		refusal := requireRefused(t, state+" on the comment's block", writeStateAs(t, rig.handler, rig.issue, block, state, "alice"),
			http.StatusConflict, codeStateNotWritable, "the store keeps no node of a block of an ask, a comment or a message")
		if !strings.Contains(refusal.Error, block) || !strings.Contains(refusal.Error, "takes no "+state) {
			t.Errorf("the refusal of %s on the comment's block names neither it nor the value: %q", state, refusal.Error)
		}
	}
	if afterRows, afterTuples := stored(); afterRows != rows || afterTuples != tuples {
		t.Fatalf("the refused writes moved the comment's block from %d rows and %d tuples to %d and %d", rows, tuples, afterRows, afterTuples)
	}
	requireNodeState(t, rig.handler, block, "alice", graphstore.StateLive)
	requireRefused(t, "the settle of the comment's block",
		dispatchRequest(t, rig.handler, http.MethodPost, statePath(rig.issue, block, "settle"), nil, "alice"),
		http.StatusConflict, codeNotProposed, "the block "+block+" is live, and only a proposed node is settled")
}

// TestTheProposedReadTakesTheProposedIndex holds the proposed read (issueProposedQuery) to the
// partial index graph_nodes_proposed: its plan reads graph_nodes through that index and scans the
// table nowhere, so the read costs the few rows at proposed and not every node of the store.
func TestTheProposedReadTakesTheProposedIndex(t *testing.T) {
	_, database := newTestHandlerWithStore(t)
	plan := planWithoutSeqScans(t, database, issueProposedQuery, "PRP-1", refs.VisibilityArg(model.Actor{Kind: "user", ID: "alice"}))
	if explaintest.SeqScansRelation(t, plan, "graph_nodes") {
		t.Fatalf("the proposed read scans graph_nodes:\n%s", plan)
	}
	if !planReadsIndex(t, plan, "graph_nodes_proposed") {
		t.Fatalf("the proposed read does not read graph_nodes_proposed:\n%s", plan)
	}
}
