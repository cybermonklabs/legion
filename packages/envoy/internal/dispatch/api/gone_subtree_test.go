package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A node taken to gone takes its whole part_of subtree with it, transitively and in one write,
// whatever the kind of each node under it and whoever may see it (the architect's ruling of
// 2026-11-20 on the spec's R5): a lens invoked on a lens and one invoked on a finding hang under
// that lens or finding through part_of (R6), so a person's gone on the first lens takes its
// finding, the lens on it with its finding, the lens on its finding with that lens's finding, and a
// lens another person keeps for themselves under it. A risk and a constraint taken to gone take the
// lens invoked on each. A move of a finding, a risk or a constraint into gone is a person's alone,
// as a lens's is (the ruling of 2026-11-20 07:15Z): the node's own author, proved by a token
// without the person scope, is refused 403 STATE_NOT_PERMITTED naming the node and marks it
// overtaken instead.
//
// Then a tree on the rig of c-settle's other tests (settleRig), whose two agents each hold a
// broker-issued token bound to a session of its own, so each proves an author and holds no person
// scope. alice's trade-off analysis L1 on the spec holds one finding F1, the first agent invokes a
// lens L3 on L1, and the second agent invokes a lens L4 on F1 with one finding F4 and a lens L5 on
// F4, each reserved for itself, so alice may see none of L4, F4 and L5. Her gone on L1 answers L1
// alone and takes F1, L3, L4, F4 and L5 to gone in the same transaction, each with one new state
// tuple written by her, and the first agent's lens read and reference read of L3, and the second
// agent's of L4 and of L5, answer what a lens nobody wrote answers. alice writes a pre-mortem with
// one risk R, and the first agent invokes a lens L6 on R: her gone on R takes L6 to gone with it.
// Her gone on a constraint of hers takes the lens the first agent invoked on it to gone in the same
// way. The first agent's gone on each of two risks under a pre-mortem of its own, one with nothing
// under it and one under which alice reserves a lens for herself, is 403 STATE_NOT_PERMITTED naming
// the risk, the two answers alike byte for byte once the risk is substituted and both risks at the
// value they held. Its gone on a constraint of its own is refused the same way, and its overtaken
// on the first risk is taken. Its gone on the finding F7 of a trade-off analysis of its own, on
// which the second agent reserves a lens L7 for itself, is refused the same way, F7 and L7 keeping
// their values, and alice's gone on F7 takes L7, which she may not see, gone with it. Each of these
// checks is an error and not the end of the test, so a run whose route leaves a node standing, or
// takes a move it refuses, names each node.
func TestANodeTakenGoneTakesItsWholePartOfSubtree(t *testing.T) {
	t.Parallel()
	rig := newReasoningRig(t)
	first := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{"findings": []string{"The first lens's finding."}})
	finding := first.Findings[0].Address
	onLens := rig.invoke(first.Address, "trade-off", "alice", map[string]any{"findings": []string{"Said of the first lens."}})
	onFinding := rig.invoke(finding, "trade-off", "alice", map[string]any{"findings": []string{"Said of the first lens's finding."}})
	hidden := rig.invoke(first.Address, "trade-off", "bob", map[string]any{
		"visibility": graphstore.VisibilityAuthor, "findings": []string{"Bob keeps this one."}})

	rig.writeState(first.Address, graphstore.StateGone)
	under := map[string]string{
		"the first lens's finding":                    finding,
		"the lens on the first lens":                  onLens.Address,
		"the finding of the lens on the first lens":   onLens.Findings[0].Address,
		"the lens on the first lens's finding":        onFinding.Address,
		"that lens's own finding":                     onFinding.Findings[0].Address,
		"bob's author-only lens on the first lens":    hidden.Address,
		"the finding of bob's author-only lens on it": hidden.Findings[0].Address,
	}
	for name, address := range under {
		if state := storedNodeOf(t, rig.database, address).State; state != graphstore.StateGone {
			t.Errorf("after alice took the lens %s to gone, %s %s is %s, want gone with it", first.Address, name, address, state)
		}
	}

	premortem := rig.invoke(rig.spec, graphstore.LensKindPreMortem, "alice", nil)
	riskWrite := rig.post("/risks", map[string]any{
		"lens": premortem.Address, "heading": "The cache goes cold.", "early_warning": "Misses climb.", "prevention": "Warm it first.",
	}, "alice")
	if riskWrite.Code != http.StatusCreated {
		t.Fatalf("write the risk: status=%d body=%s", riskWrite.Code, riskWrite.Body.String())
	}
	risk := decodeBody[riskWritten](t, riskWrite)
	onRisk := rig.invoke(risk.Address, "trade-off", "alice", map[string]any{"findings": []string{"Said of the risk."}})
	constraintWrite := rig.post("/constraints", map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"}, "alice")
	if constraintWrite.Code != http.StatusCreated {
		t.Fatalf("write the constraint: status=%d body=%s", constraintWrite.Code, constraintWrite.Body.String())
	}
	constraint := decodeBody[riskWritten](t, constraintWrite).Address
	onConstraint := rig.invoke(constraint, "trade-off", "alice", nil)

	rig.writeState(risk.Address, graphstore.StateGone)
	rig.writeState(constraint, graphstore.StateGone)
	for name, address := range map[string]string{
		"the lens on the risk": onRisk.Address, "that lens's finding": onRisk.Findings[0].Address,
		"the lens on the constraint": onConstraint.Address,
	} {
		if state := storedNodeOf(t, rig.database, address).State; state != graphstore.StateGone {
			t.Errorf("after alice took its parent to gone, %s %s is %s, want gone with it", name, address, state)
		}
	}

	// alice's token without the person scope proves her, the author of what it writes, and is no person.
	token := personalTokenOwnedBy(t, rig.database, "alice")
	asAgent := func(path string, body map[string]any) *httptest.ResponseRecorder {
		body["actor"] = map[string]any{"kind": "session", "id": "ses_alice_gone_agent"}
		response := httptest.NewRecorder()
		rig.handler.ServeHTTP(response, brokerRequest(t, http.MethodPost, path, body, "Bearer "+token, ""))
		return response
	}
	issuePath := "/api/v1/issues/" + rig.issue
	ownRisk := asAgent(issuePath+"/risks", map[string]any{
		"lens": premortem.Address, "heading": "A job runs twice.", "early_warning": "Two drainers.", "prevention": "One drainer.",
	})
	ownConstraint := asAgent(issuePath+"/constraints", map[string]any{"standard": "RFC 9111", "reference": "section 4"})
	ownLens := asAgent(issuePath+"/lenses", map[string]any{"on": rig.spec, "kind": "trade-off", "findings": []string{"The agent's own finding."}})
	if ownLens.Code != http.StatusCreated {
		t.Fatalf("the agent invokes its own lens: status=%d body=%s", ownLens.Code, ownLens.Body.String())
	}
	ownFinding := decodeBody[lensInvoked](t, ownLens).Findings[0].Address
	written := map[string]*httptest.ResponseRecorder{"risk": ownRisk, "constraint": ownConstraint}
	for name, response := range written {
		if response.Code != http.StatusCreated {
			t.Fatalf("the agent writes its own %s: status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
	for name, address := range map[string]string{
		"risk": decodeBody[riskWritten](t, ownRisk).Address, "constraint": decodeBody[riskWritten](t, ownConstraint).Address,
		"finding": ownFinding,
	} {
		refused := asAgent(statePath(rig.issue, address, "state"), map[string]any{"state": graphstore.StateGone})
		if refused.Code != http.StatusForbidden || !strings.Contains(refused.Body.String(), codeStateNotPermitted) ||
			!strings.Contains(refused.Body.String(), address) {
			t.Errorf("the agent's gone on its own %s %s answers %d %s, want 403 %s naming it: a person alone takes a %s to gone",
				name, address, refused.Code, refused.Body.String(), codeStateNotPermitted, name)
		}
		if state := storedNodeOf(t, rig.database, address).State; state != graphstore.StateLive {
			t.Errorf("after the agent's gone on its own %s %s, it is %s, want the live it held", name, address, state)
		}
		if overtaken := asAgent(statePath(rig.issue, address, "state"), map[string]any{"state": graphstore.StateOvertaken}); overtaken.Code != http.StatusOK {
			t.Errorf("the agent marks its own %s %s overtaken: status=%d body=%s, want 200", name, address, overtaken.Code, overtaken.Body.String())
		}
	}

	settle := newSettleRig(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	// takeGone is alice's gone on the node name names at address, an error unless it is taken.
	takeGone := func(name, address string) *httptest.ResponseRecorder {
		t.Helper()
		response := writeStateAs(t, settle.handler, settle.issue.Key, address, graphstore.StateGone, "alice")
		if response.Code != http.StatusOK {
			t.Errorf("alice's gone on %s %s: status=%d body=%s, want 200: a person takes a lens, a finding, a risk or a constraint to gone",
				name, address, response.Code, response.Body.String())
		}
		return response
	}
	// hiddenFromAlice fails unless alice's read of the node at address is 404 NOT_FOUND: the test's
	// premise that she may not see it.
	hiddenFromAlice := func(name, address string) {
		t.Helper()
		if read := dispatchReferences(t, settle.handler, address, "alice"); read.Code != http.StatusNotFound {
			t.Fatalf("alice reads %s %s: status=%d body=%s, want 404: the test's premise is that she may not see it",
				name, address, read.Code, read.Body.String())
		}
	}

	// The tree: alice's trade-off analysis L1 on the spec, with one finding F1; the first agent's
	// lens L3 on L1; and the second agent's lens L4 on F1, reserved for itself, with one finding
	// F4, and its lens L5 on F4, reserved too. L5 is a lens on a finding of a lens on the finding
	// of L1, and alice may see none of L4, F4 and L5.
	l1 := settle.invoke(t, "alice's trade-off analysis L1 on the spec", settle.asAlice, map[string]any{
		"on": settle.spec, "kind": "trade-off", "findings": []string{"F1, the one finding of L1."},
	})
	f1 := l1.Findings[0]
	l3 := settle.invoke(t, "the first agent's lens L3 on L1", settle.asFirst, map[string]any{"on": l1.Address, "kind": "trade-off"})
	l4 := settle.invoke(t, "the second agent's lens L4 on F1", settle.asSecond, map[string]any{
		"on": f1.Address, "kind": "trade-off", "visibility": graphstore.VisibilityAuthor, "findings": []string{"F4, the one finding of L4."},
	})
	f4 := l4.Findings[0]
	l5 := settle.invoke(t, "the second agent's lens L5 on F4", settle.asSecond, map[string]any{
		"on": f4.Address, "kind": "trade-off", "visibility": graphstore.VisibilityAuthor,
	})
	tree := []struct {
		name, address, id string
		hidden            bool
	}{
		{"F1", f1.Address, f1.BlockID, false}, {"L3", l3.Address, l3.ID, false}, {"L4", l4.Address, l4.ID, true},
		{"F4", f4.Address, f4.BlockID, true}, {"L5", l5.Address, l5.ID, true},
	}
	treeTuples := map[string]int{}
	for _, node := range tree {
		if node.hidden {
			hiddenFromAlice(node.name, node.address)
		}
		treeTuples[node.name] = len(stateHistoryOf(t, settle.database, node.address))
	}
	taken := takeGone("L1", l1.Address)
	wantTaken := nodeStateWritten{
		Node:     reasoningEnd{Address: l1.Address, Kind: graphstore.KindLens, State: graphstore.StateGone, Visibility: graphstore.VisibilityProject},
		Previous: graphstore.StateLive,
	}
	if answer := decodeBody[nodeStateWritten](t, taken); answer != wantTaken {
		t.Errorf("alice's gone on L1 answers %+v, want %+v: L1 alone, moved from live to gone", answer, wantTaken)
	}
	l1History := stateHistoryOf(t, settle.database, l1.Address)
	takenAt := l1History[len(l1History)-1].At
	for _, node := range tree {
		if strings.Contains(taken.Body.String(), node.address) || strings.Contains(taken.Body.String(), node.id) {
			t.Errorf("alice's gone on L1 answers %s, which names %s %s: its answer names L1 alone", taken.Body.String(), node.name, node.address)
		}
		if state := storedNodeOf(t, settle.database, node.address).State; state != graphstore.StateGone {
			t.Errorf("alice's gone on L1 %s left %s %s at %s, want gone with it: a node's gone takes its whole part_of subtree, at every depth and whoever may see each node",
				l1.Address, node.name, node.address, state)
			continue
		}
		// One state tuple more, written by alice at the time of L1's own, which is the time of the
		// transaction that wrote it.
		history := stateHistoryOf(t, settle.database, node.address)
		newest := history[len(history)-1]
		if len(history) != treeTuples[node.name]+1 || newest.ToID != graphstore.StateGone || !newest.Who.SameAs(alice) || !newest.At.Equal(takenAt) {
			t.Errorf("%s %s holds %d state tuples, the newest %s written by %+v at %s; want %d, the newest gone written by alice at %s, in L1's own write",
				node.name, node.address, len(history), newest.ToID, newest.Who, newest.At, treeTuples[node.name]+1, takenAt)
		}
	}
	// No read answers a lens of the tree live with an on or a parent naming nothing: the first
	// agent's GET /api/v1/lenses?on= and GET /api/v1/references?to= of its L3, and the second
	// agent's of its L4 and L5, by the lens's dispatch:// address, answer what the same read of a
	// lens nobody wrote answers.
	requireReadsAsALensNobodyWrote(t, func(read, status, login string) string {
		return fmt.Sprintf("%s's %s once L1 is gone answers %s, want 404 NOT_FOUND as for a lens nobody wrote", login, read, status)
	}, lensReader{"the first agent", settle.asFirst, l3}, lensReader{"the second agent", settle.asSecond, l4},
		lensReader{"the second agent", settle.asSecond, l5})

	// requireTakenWith fails, naming the node, unless alice's gone on parent, called parentName,
	// left the node name names at address gone.
	requireTakenWith := func(parentName, parent, name, address string) {
		t.Helper()
		if state := storedNodeOf(t, settle.database, address).State; state != graphstore.StateGone {
			t.Errorf("alice's gone on %s %s left %s %s at %s, want gone with it: a node's gone takes its whole part_of subtree, at every depth and whoever invoked each node",
				parentName, parent, name, address, state)
		}
	}
	// A risk's gone takes the lens on it, and so does a constraint's, whoever invoked that lens:
	// alice's pre-mortem holds one risk R, on which the first agent invokes a lens L6, and the
	// first agent invokes another lens on a constraint of alice's.
	alicePremortem := settle.invoke(t, "alice's pre-mortem", settle.asAlice, map[string]any{"on": settle.spec, "kind": graphstore.LensKindPreMortem})
	r := settle.risk(t, "R, alice's risk", settle.asAlice, alicePremortem.Address)
	l6 := settle.invoke(t, "the first agent's lens L6 on R", settle.asFirst, map[string]any{"on": r.Address, "kind": "trade-off"})
	aliceConstraint := decodeBody[riskWritten](t, settle.posted(t, "alice's constraint", "/constraints", settle.asAlice,
		map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"})).Address
	onAliceConstraint := settle.invoke(t, "the first agent's lens on alice's constraint", settle.asFirst, map[string]any{"on": aliceConstraint, "kind": "trade-off"})
	takeGone("R", r.Address)
	requireTakenWith("R", r.Address, "L6", l6.Address)
	takeGone("her constraint", aliceConstraint)
	requireTakenWith("her constraint", aliceConstraint, "the first agent's lens on it", onAliceConstraint.Address)

	// refusedGone is the first agent's gone on the node at address, one it wrote, held to 403
	// STATE_NOT_PERMITTED naming the node, and the node to the state and the state tuples it held;
	// it answers the refusal with id, the node's own, written <node>.
	refusedGone := func(name, address, id string) string {
		t.Helper()
		held, tuples := storedNodeOf(t, settle.database, address).State, len(stateHistoryOf(t, settle.database, address))
		response := settle.asFirst(http.MethodPost, statePath(settle.issue.Key, address, "state"), map[string]any{"state": graphstore.StateGone})
		t.Logf("%s: status=%d body=%s", name, response.Code, response.Body.String())
		refused := response.Code == http.StatusForbidden
		if refused {
			refusal := decodeBody[refusalAnswer](t, response)
			refused = refusal.Code == codeStateNotPermitted && strings.Contains(refusal.Error, address)
		}
		if !refused {
			t.Errorf("%s answers %d %s, want 403 %s naming %s: a person alone takes a lens, a finding, a risk or a constraint to gone, its author included",
				name, response.Code, response.Body.String(), codeStateNotPermitted, address)
		}
		if state := storedNodeOf(t, settle.database, address).State; state != held {
			t.Errorf("%s left %s at %s, want the %s it held", name, address, state, held)
		}
		if after := len(stateHistoryOf(t, settle.database, address)); after != tuples {
			t.Errorf("%s left %s with %d state tuples, want the %d it held", name, address, after, tuples)
		}
		return strings.ReplaceAll(response.Body.String(), id, "<node>")
	}
	// The first agent's two risks under a pre-mortem of its own: nothing stands under the first,
	// and alice reserves a lens for herself on the second, which the agent may not see.
	agentPremortem := settle.invoke(t, "the first agent's pre-mortem", settle.asFirst, map[string]any{"on": settle.spec, "kind": graphstore.LensKindPreMortem})
	bare := settle.risk(t, "The first agent's risk with nothing under it", settle.asFirst, agentPremortem.Address)
	covered := settle.risk(t, "The first agent's risk alice looks at", settle.asFirst, agentPremortem.Address)
	settle.invoke(t, "alice's lens on the first agent's second risk, reserved for her", settle.asAlice, map[string]any{
		"on": covered.Address, "kind": "trade-off", "visibility": graphstore.VisibilityAuthor,
	})
	bareRefusal := refusedGone("the first agent's gone on its own risk "+bare.Address+", with nothing under it", bare.Address, bare.ID)
	coveredRefusal := refusedGone("the first agent's gone on its own risk "+covered.Address+", under which alice keeps a lens", covered.Address, covered.ID)
	if bareRefusal != coveredRefusal {
		t.Errorf("the first agent's gone on its risk %s with nothing under it answers %s, and on its risk %s under which alice keeps a lens %s: want the two alike",
			bare.Address, bareRefusal, covered.Address, coveredRefusal)
	}
	// Its gone on a constraint it wrote is refused the same way, and it marks its first risk
	// overtaken.
	agentConstraint := decodeBody[riskWritten](t, settle.posted(t, "the first agent's constraint", "/constraints", settle.asFirst,
		map[string]any{"standard": "RFC 9111", "reference": "section 4"}))
	refusedGone("the first agent's gone on its own constraint "+agentConstraint.Address, agentConstraint.Address, agentConstraint.ID)
	if marked := settle.asFirst(http.MethodPost, statePath(settle.issue.Key, bare.Address, "state"),
		map[string]any{"state": graphstore.StateOvertaken}); marked.Code != http.StatusOK {
		t.Errorf("the first agent marks its own risk %s overtaken: status=%d body=%s, want 200", bare.Address, marked.Code, marked.Body.String())
	}

	// A finding's gone is a person's alone too, and takes the lens on it: the first agent's
	// trade-off analysis holds one finding F7, on which the second agent reserves a lens L7 for
	// itself, which alice may not see.
	analysis := settle.invoke(t, "the first agent's trade-off analysis", settle.asFirst, map[string]any{
		"on": settle.spec, "kind": "trade-off", "findings": []string{"F7, the first agent's own finding."},
	})
	f7 := analysis.Findings[0]
	l7 := settle.invoke(t, "the second agent's lens L7 on F7", settle.asSecond, map[string]any{
		"on": f7.Address, "kind": "trade-off", "visibility": graphstore.VisibilityAuthor,
	})
	hiddenFromAlice("L7", l7.Address)
	l7Held := storedNodeOf(t, settle.database, l7.Address).State
	refusedGone("the first agent's gone on its own finding F7 "+f7.Address, f7.Address, f7.BlockID)
	if state := storedNodeOf(t, settle.database, l7.Address).State; state != l7Held {
		t.Errorf("the first agent's gone on F7 %s left L7 %s at %s, want the %s it held", f7.Address, l7.Address, state, l7Held)
	}
	takeGone("F7", f7.Address)
	requireTakenWith("F7", f7.Address, "L7", l7.Address)
}

// A write on a lens on a removed document, or on a node under that lens, is refused as a write on
// a gone node is, 409 NODE_GONE naming the node as the request names it, whichever build removed
// the document (spec v59 R5): gone is final, and a stale node and everything under it are answered
// as gone ones. The removal deleted the document's row, through which no read finds the issue the
// lens stands under any more, so the gone check reads the issue the document's address spells
// (refs.GoneStandingIssueSQL). On reasoningRig's issue alice makes a document D by typing a title,
// invokes a pre-mortem LD on it, adds a finding FD to LD and writes a risk RD under it, and invokes
// a trade-off analysis LA on D reserved for herself, which bob may not see; a second document E
// holds the same nodes, LE, FE, RE and LB. Her DELETE /api/v1/artifacts/{id} of D takes LD, FD, RD
// and LA gone with it, and E is removed as the release before this one removes it
// (removeAsTheReleaseBefore): its row deleted and no node written, so LE, FE, RE and LB read live
// in the store, stale. Then on each document her overtaken through the state route on the lens,
// the finding, the risk and her own analysis, a finding she adds to the lens and a risk she writes
// under it each answer 409 NODE_GONE naming the node written on, and a lens she invokes on D
// answers the same naming D, whose node the removal took gone too. Bob's overtaken on her own
// analysis, and a finding he adds to it, answer byte for byte once the id is substituted what the
// same write on a lens nobody wrote answers, 404 NOT_FOUND, on either document. None of the
// refused writes stores a lens, a part_of tuple or a state tuple.
func TestAWriteOnALensOnARemovedDocumentIsRefusedAsGone(t *testing.T) {
	t.Parallel()
	rig := newReasoningRig(t)
	// lensed is a document of the rig's issue that alice made by typing a title, at address, with
	// her pre-mortem lens on it holding a finding she added and a risk she wrote, and her trade-off
	// analysis hidden on it, reserved for her.
	type lensed struct {
		document     model.Artifact
		address      string
		lens, hidden lensInvoked
		finding      lensFinding
		risk         riskWritten
	}
	riskUnder := func(lens string) map[string]any {
		return map[string]any{"lens": lens, "heading": "The import stalls.", "early_warning": "Rows stop.", "prevention": "Batch it."}
	}
	makeLensed := func(name string) lensed {
		t.Helper()
		document := emptyDocument(t, rig.handler, rig.issue, name)
		made := lensed{document: document, address: "dispatch://" + rig.issue + "/artifact/" + document.Slug}
		made.lens = rig.invoke(made.address, graphstore.LensKindPreMortem, "alice", nil)
		made.finding = decodeBody[lensFinding](t, created(t, "alice's finding under "+made.lens.Address,
			rig.post(nodePath(made.lens.Address, "findings"), map[string]any{"text": "The import holds a lock."}, "alice")))
		made.risk = decodeBody[riskWritten](t, created(t, "alice's risk under "+made.lens.Address,
			rig.post("/risks", riskUnder(made.lens.Address), "alice")))
		made.hidden = rig.invoke(made.address, "trade-off", "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
		return made
	}
	// requireHeld fails unless each node standing on d reads want in the store: the test's premise.
	requireHeld := func(when string, d lensed, want string) {
		t.Helper()
		for _, address := range []string{d.lens.Address, d.finding.Address, d.risk.Address, d.hidden.Address} {
			if state := storedNodeOf(t, rig.database, address).State; state != want {
				t.Fatalf("%s, %s reads %s in the store; the test's premise is %s", when, address, state, want)
			}
		}
	}
	// requireGoneNamed fails unless response is 409 NODE_GONE whose message names address, the node
	// as the request named it, as one of its words (graphstoretest.NamesAsAWord).
	requireGoneNamed := func(name string, response *httptest.ResponseRecorder, address string) {
		t.Helper()
		refusal := decodeBody[refusalAnswer](t, response)
		if response.Code != http.StatusConflict || refusal.Code != codeNodeGone || !graphstoretest.NamesAsAWord(refusal.Error, address) {
			t.Errorf("%s answers %d %s, want 409 %s naming %s", name, response.Code, response.Body.String(), codeNodeGone, address)
		}
	}
	nobodysID := uuid.NewString()
	nobodys := refs.StoreRef(graphstore.KindLens, nobodysID)
	// requireRefusedAsGone holds every write on the nodes standing on d, the document removed when
	// says, to 409 NODE_GONE naming the node written on, and bob's to the 404 of a lens nobody wrote.
	requireRefusedAsGone := func(when string, d lensed) {
		t.Helper()
		for _, node := range []struct{ name, address string }{
			{"the pre-mortem", d.lens.Address}, {"its finding", d.finding.Address}, {"its risk", d.risk.Address},
			{"her own analysis", d.hidden.Address},
		} {
			requireGoneNamed(fmt.Sprintf("%s, alice's overtaken on %s %s", when, node.name, node.address),
				writeStateAs(t, rig.handler, rig.issue, node.address, graphstore.StateOvertaken, "alice"), node.address)
		}
		requireGoneNamed(when+", a finding alice adds to the pre-mortem "+d.lens.Address,
			rig.post(nodePath(d.lens.Address, "findings"), map[string]any{"text": "One more."}, "alice"), d.lens.Address)
		requireGoneNamed(when+", a risk alice writes under the pre-mortem "+d.lens.Address,
			rig.post("/risks", riskUnder(d.lens.Address), "alice"), d.lens.Address)
		requireNobodysAnswer(t, when+", bob's overtaken on alice's own analysis "+d.hidden.Address,
			writeStateAs(t, rig.handler, rig.issue, d.hidden.Address, graphstore.StateOvertaken, "bob"),
			writeStateAs(t, rig.handler, rig.issue, nobodys, graphstore.StateOvertaken, "bob"), d.hidden.ID, nobodysID)
		requireNobodysAnswer(t, when+", a finding bob adds to alice's own analysis "+d.hidden.Address,
			rig.post(nodePath(d.hidden.Address, "findings"), map[string]any{"text": "Bob's."}, "bob"),
			rig.post(nodePath(nobodys, "findings"), map[string]any{"text": "Bob's."}, "bob"), d.hidden.ID, nobodysID)
	}

	d, e := makeLensed("Notes D.md"), makeLensed("Notes E.md")
	removedDocument(t, rig.handler, d.document.ID)
	requireHeld("after alice's removal of D", d, graphstore.StateGone)
	removeAsTheReleaseBefore(t, rig.database, e.document)
	requireHeld("after E's removal as the release before this one removes it", e, graphstore.StateLive)

	lenses, partOfs := rig.counts(graphstore.KindLens, graphstore.TuplePartOf)
	states := stateTupleCount(t, rig.database)
	requireRefusedAsGone("after alice's removal of D", d)
	requireGoneNamed("after alice's removal of D, a lens alice invokes on D",
		rig.post("/lenses", map[string]any{"on": d.address, "kind": "trade-off"}, "alice"), d.address)
	requireRefusedAsGone("after E's removal as the release before this one removes it", e)
	if nodes, tuples := rig.counts(graphstore.KindLens, graphstore.TuplePartOf); nodes != lenses || tuples != partOfs {
		t.Errorf("the refused writes moved the lenses from %d to %d and the part_of tuples from %d to %d", lenses, nodes, partOfs, tuples)
	}
	if after := stateTupleCount(t, rig.database); after != states {
		t.Errorf("the refused writes moved the state tuples from %d to %d", states, after)
	}
}

// A lens on a block that a version removes, or on a document that is retired, or on a block of
// that document, stays live, and neither read answers it a null `on` or parent (spec v59 R5): GET
// /api/v1/lenses answers its `on` as the block's address at the last version that held it,
// `dispatch://KEY/artifact/<slug>#b-<id>@v<N>`, or as the retired document's at its last version,
// `dispatch://KEY/artifact/<slug>@v<N>`, and GET /api/v1/references answers the node's `parent`
// naming that block or document with `parent_version` N.
func TestALensOnARemovedBlockOrARetiredDocumentNamesItsParent(t *testing.T) {
	t.Parallel()
	rig := newReasoningRig(t)
	onBlock := rig.invoke(rig.secondCheck, "trade-off", "alice", nil)
	uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "verification.md", checkServing("k1", rig.requirement))
	if removed := dispatchReferences(t, rig.handler, rig.secondCheck, "alice"); removed.Code != http.StatusNotFound {
		t.Fatalf("version 2 of the verification document drops k2, yet %s answers %d %s", rig.secondCheck, removed.Code, removed.Body.String())
	}
	if state := storedNodeOf(t, rig.database, onBlock.Address).State; state != graphstore.StateLive {
		t.Errorf("the lens %s on the removed block is %s, want live", onBlock.Address, state)
	}
	pinned := rig.secondCheck + "@v1"
	if read := rig.lenses(onBlock.Address, "alice"); read.Lens == nil || read.Lens.On == nil || *read.Lens.On != pinned {
		t.Errorf("the lens read of %s answers on=%v, want %s, the block at the last version that held it", onBlock.Address, lensOn(read), pinned)
	}
	requireNamedParent(t, rig, onBlock.Address, rig.secondCheck)

	notes := uploadDocument(t, rig.handler, "/api/v1/issues/"+rig.issue, "notes.md", "# Notes\n\nOne paragraph.\n")
	document := "dispatch://" + rig.issue + "/artifact/" + notes.Artifact.Slug
	block := refs.BlockRef(document, firstBlockOf(t, rig.handler, notes.Artifact.ID))
	onDocument := rig.invoke(document, "trade-off", "alice", nil)
	onNotesBlock := rig.invoke(block, "trade-off", "alice", nil)
	ctx := context.Background()
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the retirement: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := docs.RetireDocument(ctx, tx, notes.Artifact.ID, model.Actor{Kind: "user", ID: "alice"}, "The test retires the notes."); err != nil {
		t.Fatalf("retire %s: %v", document, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}
	for address, on := range map[string]string{onDocument.Address: document, onNotesBlock.Address: block} {
		if state := storedNodeOf(t, rig.database, address).State; state != graphstore.StateLive {
			t.Errorf("the lens %s on %s of the retired document is %s, want live", address, on, state)
		}
		if read := rig.lenses(address, "alice"); read.Lens == nil || read.Lens.On == nil || *read.Lens.On != on+"@v1" {
			t.Errorf("the lens read of %s answers on=%v, want %s@v1, at the last version of the retired document", address, lensOn(read), on)
		}
		requireNamedParent(t, rig, address, on)
	}
}

// lensOn is the `on` a lens read answers, "<nil>" where it answers none.
func lensOn(read nodeLenses) string {
	if read.Lens == nil || read.Lens.On == nil {
		return "<nil>"
	}
	return *read.Lens.On
}

// requireNamedParent fails unless GET /api/v1/references of address answers its node's parent
// naming want, bare or pinned at version 1, and parent_version 1.
func requireNamedParent(t *testing.T, rig reasoningRig, address, want string) {
	t.Helper()
	response := dispatchReferences(t, rig.handler, address, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", address, response.Code, response.Body.String())
	}
	node := decodeBody[model.GraphReferences](t, response).Node
	if node.Parent == nil || (node.Parent.Ref != want && node.Parent.Ref != want+"@v1") {
		t.Errorf("GET /api/v1/references of %s answers parent %+v, want one naming %s", address, node.Parent, want)
	}
	if node.ParentVersion == nil || *node.ParentVersion != 1 {
		t.Errorf("GET /api/v1/references of %s answers parent_version %v, want 1, the last version that held its parent", address, node.ParentVersion)
	}
}

// A finding asked for `project` under a lens its author keeps for themselves is refused 400
// VISIBILITY_WIDER_THAN_PARENT naming the finding the write drew and then the lens as the node it
// cannot stand under, and nothing is stored, as a risk or a lens asked for `project` under or on
// such a node is (the ruling of 2026-11-20 07:15Z on the spec's R1): no write is narrowed in
// silence. A finding asked for no visibility takes its lens's, `author`.
func TestAFindingAskedForProjectUnderAnAuthorOnlyLensIsRefused(t *testing.T) {
	t.Parallel()
	rig := newReasoningRig(t)
	lens := rig.invoke(rig.spec, "trade-off", "alice", map[string]any{"visibility": graphstore.VisibilityAuthor})
	nodes, tuples := rig.counts(refs.BlockNodeKind, graphstore.TuplePartOf)
	// The refusal names both nodes, as TestAChildIsNeverWiderThanItsParent holds a risk's and a
	// lens's: the finding first, whose address is the lens's own with the finding's block id,
	// `<lens address>#b-<id>`, and the lens as a word of its own (graphstoretest.NamesAsAWord),
	// which words that name the finding alone never hold.
	asked := "a finding asked for project under alice's author-only lens " + lens.Address
	answer := requireRefused(t, asked, rig.post(nodePath(lens.Address, "findings"), map[string]any{
		"text": "Every reader would see this.", "visibility": graphstore.VisibilityProject}, "alice"),
		http.StatusBadRequest, codeVisibilityWiderThanParent, lens.Address)
	if !strings.HasPrefix(answer.Error, lens.Address+"#b-") {
		t.Errorf("%s is refused %q, want it to name the finding it drew, %s#b-<id>, first", asked, answer.Error, lens.Address)
	}
	if afterNodes, afterTuples := rig.counts(refs.BlockNodeKind, graphstore.TuplePartOf); afterNodes != nodes || afterTuples != tuples {
		t.Errorf("the refused finding left %d block nodes and %d part_of tuples, want the %d and %d before it", afterNodes, afterTuples, nodes, tuples)
	}
	unasked := rig.post(nodePath(lens.Address, "findings"), map[string]any{"text": "Alice keeps this."}, "alice")
	if unasked.Code != http.StatusCreated {
		t.Fatalf("a finding asked for no visibility under %s: status=%d body=%s", lens.Address, unasked.Code, unasked.Body.String())
	}
	if written := decodeBody[lensFinding](t, unasked); written.Visibility != graphstore.VisibilityAuthor {
		t.Errorf("a finding asked for no visibility under the author-only lens %s is %q, want author, its lens's", lens.Address, written.Visibility)
	}
}

// stateHistoryOf is every state tuple the store holds of the node at address, oldest first.
func stateHistoryOf(t *testing.T, database *store.Store, address string) []graphstore.Tuple {
	t.Helper()
	node := storedNodeOf(t, database, address)
	history, err := graphstore.History(context.Background(), database.Pool, node.Kind, node.ID, graphstore.AttributeState)
	if err != nil {
		t.Fatalf("read the state history of %s: %v", address, err)
	}
	return history
}
