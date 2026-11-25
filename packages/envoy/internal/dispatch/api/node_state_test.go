package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

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

// writeState writes state on the node address names as alice, failing on anything but 200.
func (r reasoningRig) writeState(address, state string) nodeStateWritten {
	r.t.Helper()
	response := writeStateAs(r.t, r.handler, r.issue, address, state, "alice")
	if response.Code != http.StatusOK {
		r.t.Fatalf("write %s on %s as alice: status=%d body=%s", state, address, response.Code, response.Body.String())
	}
	return decodeBody[nodeStateWritten](r.t, response)
}

// requireNodeState fails unless every read of the node address names answers want, as alice reads it.
func requireNodeState(t *testing.T, handler http.Handler, address, want string) {
	t.Helper()
	if got := graphEdges(t, handler, url.Values{"to": {address}}).Node.State; got != want {
		t.Fatalf("%s reads node state %q, want %q", address, got, want)
	}
}

// proposedRead is GET /api/v1/issues/{key}/proposed as alice, failing on anything but 200.
func proposedRead(t *testing.T, handler http.Handler, issue string) issueProposed {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/proposed", nil, "alice")
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
	history := stateHistoryOfNode(t, database, kind, id)
	if len(history) == 0 {
		t.Fatalf("the store holds no state tuple of %s %s", kind, id)
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
// live; gone on it, and settled through this route, are each 409 NODE_STATE_NOT_WRITABLE naming
// the block, its state and the value asked, and leave it live. A word that is none of the five,
// `propsed`, is 400 INVALID_ATTRIBUTE_VALUE naming the attribute, the word and the five values,
// with no tuple carrying it stored and the node at the state it held. A node of the issue another
// person may not see answers that person as an id nobody wrote.
func TestTheStateRouteTakesTheFiveValuesWhereTheyApplyAndRefusesTheRest(t *testing.T) {
	t.Parallel()
	rig := newReasoningRig(t)
	ctx := context.Background()
	lens := rig.invoke(rig.spec, "trade-off", "alice", nil)

	for _, state := range []string{graphstore.StateProposed, graphstore.StateOvertaken, graphstore.StateLive, graphstore.StateGone} {
		before := graphEdges(t, rig.handler, url.Values{"to": {lens.Address}}).Node.State
		written := rig.writeState(lens.Address, state)
		if written.Node.State != state || written.Previous != before || written.Node.Kind != graphstore.KindLens {
			t.Fatalf("the write of %s on the lens answers %+v, want the lens at %s from %s", state, written, state, before)
		}
		newest := newestStateTuple(t, rig.database, graphstore.KindLens, lens.ID)
		if newest.ToID != state || newest.Who.Kind != "user" || newest.Who.ID != "alice" || newest.SourceEvent != graphstore.NoEvent {
			t.Fatalf("the lens's newest state tuple is %+v, want %s written by alice at no event", newest, state)
		}
		if state != graphstore.StateGone {
			requireNodeState(t, rig.handler, lens.Address, state)
		}
	}
	if node, held, err := graphstore.ReadNode(ctx, rig.database.Pool, graphstore.KindLens, lens.ID); err != nil || !held || node.State != graphstore.StateGone {
		t.Fatalf("the lens's row is %+v (held %t, %v), want it at gone", node, held, err)
	}
	held := stateTupleCount(t, rig.database)
	for _, asked := range []string{graphstore.StateLive, graphstore.StateProposed, graphstore.StateOvertaken} {
		requireGoneRefused(t, asked+" on the gone lens", writeStateAs(t, rig.handler, rig.issue, lens.Address, asked, "alice"), lens.Address)
	}
	if after := stateTupleCount(t, rig.database); after != held {
		t.Fatalf("the refused writes on the gone lens moved the state tuples from %d to %d", held, after)
	}

	for _, state := range []string{graphstore.StateProposed, graphstore.StateOvertaken, graphstore.StateLive} {
		rig.writeState(rig.requirement, state)
		requireNodeState(t, rig.handler, rig.requirement, state)
	}
	requireRefused(t, "gone on the requirement", writeStateAs(t, rig.handler, rig.issue, rig.requirement, graphstore.StateGone, "alice"),
		http.StatusConflict, codeStateNotWritable, refs.BlockNodeKind, rig.requirement, graphstore.StateLive, graphstore.StateGone)
	requireNodeState(t, rig.handler, rig.requirement, graphstore.StateLive)
	requireRefused(t, "settled through the state route", writeStateAs(t, rig.handler, rig.issue, rig.requirement, graphstore.StateSettled, "alice"),
		http.StatusConflict, codeStateNotWritable, refs.BlockNodeKind, rig.requirement, graphstore.StateLive, graphstore.StateSettled)

	before := stateTupleCount(t, rig.database)
	requireRefused(t, "a word that is no value", writeStateAs(t, rig.handler, rig.issue, rig.requirement, "propsed", "alice"),
		http.StatusBadRequest, "INVALID_ATTRIBUTE_VALUE", append([]string{graphstore.AttributeState, "propsed"}, graphstore.NodeStates()...)...)
	misspelled := countRows(t, rig.database, `select count(*) from graph_tuples where to_id = 'propsed'`)
	if after := stateTupleCount(t, rig.database); misspelled != 0 || after != before {
		t.Fatalf("the refused write stored %d tuples at propsed and moved the state tuples from %d to %d", misspelled, before, after)
	}
	requireNodeState(t, rig.handler, rig.requirement, graphstore.StateLive)

	hidden := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	nobody := graphstore.KindLens + "/" + uuid.NewString()
	requireNobodysAnswer(t, "bob's write on alice's hidden lens",
		writeStateAs(t, rig.handler, rig.issue, graphstore.KindLens+"/"+hidden.ID, graphstore.StateProposed, "bob"),
		writeStateAs(t, rig.handler, rig.issue, nobody, graphstore.StateProposed, "bob"),
		graphstore.KindLens+"/"+hidden.ID, nobody)
}

// requireGoneRefused fails unless response is 409 NODE_GONE naming the node at address, as the
// write named it, as one of its words.
func requireGoneRefused(t *testing.T, name string, response *httptest.ResponseRecorder, address string) {
	t.Helper()
	requireRefused(t, name, response, http.StatusConflict, codeNodeGone, address)
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
	t.Parallel()
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
		rig.writeState(address, graphstore.StateProposed)
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
	requireNodeState(t, rig.handler, held.Address, graphstore.StateSettled)

	// A gone on the lens by a caller who is no person is refused naming the lens, in one body
	// whatever the lens holds (goneNotPermitted): bob's refusal below reads as this one once each
	// lens's address is set aside.
	before := stateTupleCount(t, rig.database)
	shared := asToken("agent-token", "ses_shared_bearer_01", nodePath(lens.Address, "state"), map[string]any{"state": graphstore.StateGone})
	sharedRefusal := requireRefused(t, "the shared bearer's gone on alice's lens", shared, http.StatusForbidden, codeStateNotPermitted, lens.Address)
	if after := stateTupleCount(t, rig.database); after != before {
		t.Fatalf("the shared bearer's refused gone moved the state tuples from %d to %d", before, after)
	}
	requireNodeState(t, rig.handler, lens.Address, graphstore.StateLive)
	requireNodeState(t, rig.handler, finding.Address, graphstore.StateLive)
	requireNodeState(t, rig.handler, held.Address, graphstore.StateSettled)

	own := premortem("bob's own pre-mortem", asToken(bobToken, bobSession, "/lenses", map[string]any{
		"on": rig.spec, "kind": graphstore.LensKindPreMortem, "findings": []string{"The bill doubles."},
	}))
	before = stateTupleCount(t, rig.database)
	refused := asToken(bobToken, bobSession, nodePath(own.Address, "state"), map[string]any{"state": graphstore.StateGone})
	bobsRefusal := requireRefused(t, "bob's gone on his own lens, holding only his own finding", refused, http.StatusForbidden, codeStateNotPermitted, own.Address)
	if bobs, sharedBearers := strings.ReplaceAll(bobsRefusal.Error, own.Address, "<lens>"), strings.ReplaceAll(sharedRefusal.Error, lens.Address, "<lens>"); bobs != sharedBearers {
		t.Fatalf("bob's gone on his own lens is refused %q, and the shared bearer's on alice's %q: want one body once the lens is substituted", bobs, sharedBearers)
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

	written := rig.writeState(lens.Address, graphstore.StateGone)
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
		requireGoneRefused(t, asked+" on the gone lens", writeStateAs(t, rig.handler, rig.issue, lens.Address, asked, "alice"), lens.Address)
	}
	requireGoneRefused(t, "proposed on the gone risk", writeStateAs(t, rig.handler, rig.issue, held.Address, graphstore.StateProposed, "alice"), held.Address)
	requireGoneRefused(t, "the settle of the gone risk",
		dispatchRequest(t, rig.handler, http.MethodPost, statePath(rig.issue, held.Address, "settle"), nil, "alice"), held.Address)
	if after := stateTupleCount(t, rig.database); after != before {
		t.Fatalf("the refused writes on the gone nodes moved the state tuples from %d to %d", before, after)
	}
	lenses, lensTuples := rig.counts(graphstore.KindLens, graphstore.TuplePartOf)
	requireGoneRefused(t, "a finding added to the gone lens",
		rig.post(nodePath(lens.Address, "findings"), map[string]any{"text": "One more."}, "alice"), lens.Address)
	requireGoneRefused(t, "a risk written under the gone lens", rig.post("/risks", map[string]any{
		"lens": lens.Address, "heading": "Late", "early_warning": "Late", "prevention": "Late",
	}, "alice"), lens.Address)
	requireGoneRefused(t, "a pointer out of the gone lens", rig.post(nodePath(lens.Address, "pointers"), map[string]any{
		"kind": graphstore.TupleTriggeredBy, "action": "append", "to": own.Address,
	}, "alice"), lens.Address)
	requireGoneRefused(t, "a lens invoked on the gone risk",
		rig.post("/lenses", map[string]any{"on": held.Address, "kind": "trade-off"}, "alice"), held.Address)
	requireRefused(t, "a lens triggered by the gone lens", rig.post("/lenses", map[string]any{
		"on": rig.spec, "kind": "trade-off", "triggered_by": lens.Address,
	}, "alice"), http.StatusNotFound, "NOT_FOUND")
	if nodes, tuples := rig.counts(graphstore.KindLens, graphstore.TuplePartOf); nodes != lenses || tuples != lensTuples {
		t.Fatalf("the refused writes on the gone nodes moved the lenses from %d to %d and the part_of tuples from %d to %d",
			lenses, nodes, lensTuples, tuples)
	}
	if state := stored(graphstore.KindLens, lens.ID); state != graphstore.StateGone {
		t.Fatalf("the lens reads %s after the refused writes, want gone", state)
	}

	hidden := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	rig.writeState(hidden.Address, graphstore.StateGone)
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
	t.Parallel()
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
		requireNodeState(t, handler, address, want)
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

	if got := proposedRefs(proposedRead(t, handler, issue.Key)); !slices.Equal(got, []string{r1}) {
		t.Fatalf("the proposed read answers %v, want %s alone", got, r1)
	}
	requireCounted("before the settle", []string{"r2"}, "r1")

	settle(r1)
	requireNodeState(t, handler, r1, graphstore.StateSettled)
	if got := proposedRead(t, handler, issue.Key); len(got.Nodes) != 0 {
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

// TestAStateWriteKeepsTheCauseOfTheBlockItMoves holds the state route to moving a block's state
// and nothing else of its row (graphstore.Node.KeepsCause). The requirement's row keeps the event
// that caused it across proposed, live and proposed again, and each move is one state tuple at no
// event, written by her, three in all, since no repeat key drops a tuple at the sentinel. A new
// version of the spec that rewrites another block then leaves the requirement's cause as it was:
// its row was never at the sentinel, which that version's index pass moves its own event onto.
func TestAStateWriteKeepsTheCauseOfTheBlockItMoves(t *testing.T) {
	t.Parallel()
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
		rig.writeState(rig.requirement, state)
		if got := cause(); got != caused {
			t.Fatalf("after the write of %s the requirement's row is caused by event %d, want %d, the event that caused it", state, got, caused)
		}
	}
	history := stateHistoryOfNode(t, rig.database, refs.BlockNodeKind, rig.requirementNode)
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
	// The spec's blocks are read where the block index keeps them, in its issue's room
	// (refs.DocumentRoomBlocksSQL).
	var indexed bool
	if err := rig.database.Pool.QueryRow(ctx, `
		select exists (select 1 from document_blocks d where `+refs.DocumentRoomBlocksSQL("d", "$1::uuid")+` and d.excerpt like '%rewritten opening%')
	`, specID).Scan(&indexed); err != nil || !indexed {
		t.Fatalf("the block index holds no block of the rewritten paragraph (%v), so no index pass of the new version ran", err)
	}
	if got := cause(); got != caused {
		t.Fatalf("after a version that rewrote another block the requirement's row is caused by event %d, want %d", got, caused)
	}
	requireNodeState(t, rig.handler, rig.requirement, graphstore.StateProposed)
}

// TestTheStateRouteRefusesABlockOfACommentInWordsThatAreTrue: the store keeps no node of a block
// of an ask, a comment or a message, so the state route refuses proposed, overtaken and live on a
// comment's block with 409 NODE_STATE_NOT_WRITABLE saying that - naming the block, the value asked
// and the comment among the items whose blocks the store keeps no node of, which the refusal of a
// node the backfill has not reached, of the same code, does not name - and stores no tuple and no
// row; the block still reads live, and the settle refuses it as a node that is not proposed.
func TestTheStateRouteRefusesABlockOfACommentInWordsThatAreTrue(t *testing.T) {
	t.Parallel()
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
		requireRefused(t, state+" on the comment's block", writeStateAs(t, rig.handler, rig.issue, block, state, "alice"),
			http.StatusConflict, codeStateNotWritable, refs.BlockNodeKind, block, graphstore.StateLive, state, graphstore.KindComment)
	}
	if afterRows, afterTuples := stored(); afterRows != rows || afterTuples != tuples {
		t.Fatalf("the refused writes moved the comment's block from %d rows and %d tuples to %d and %d", rows, tuples, afterRows, afterTuples)
	}
	requireNodeState(t, rig.handler, block, graphstore.StateLive)
	requireRefused(t, "the settle of the comment's block",
		dispatchRequest(t, rig.handler, http.MethodPost, statePath(rig.issue, block, "settle"), nil, "alice"),
		http.StatusConflict, codeNotProposed, refs.BlockNodeKind, block, graphstore.StateLive)
}

// TestTheProposedReadTakesTheProposedIndex holds the proposed read (issueProposedQuery) to the
// partial index graph_nodes_proposed: its plan reads graph_nodes through that index and scans the
// table nowhere, so the read costs the few rows at proposed and not every node of the store.
func TestTheProposedReadTakesTheProposedIndex(t *testing.T) {
	t.Parallel()
	_, database := newTestHandlerWithStore(t)
	plan := planWithoutSeqScans(t, database, issueProposedQuery, "PRP-1", refs.VisibilityArg(model.Actor{Kind: "user", ID: "alice"}))
	if explaintest.SeqScansRelation(t, plan, "graph_nodes") {
		t.Fatalf("the proposed read scans graph_nodes:\n%s", plan)
	}
	if !planReadsIndex(t, plan, "graph_nodes_proposed") {
		t.Fatalf("the proposed read does not read graph_nodes_proposed:\n%s", plan)
	}
}
