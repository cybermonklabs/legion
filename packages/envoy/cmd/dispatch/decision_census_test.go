package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// decisionCensusKinds are the kinds of dispatch://INFRA-1150 `census --store` prints a line for,
// each of which the database this file's test seeds holds rows and nodes of.
var decisionCensusKinds = []string{
	refs.TupleShaped, refs.TupleFromAsk, graphstore.TupleDecidedBy, graphstore.TupleFollowedBy,
	"reading_parent", "ask_parent", "ask_anchor", "ask_answer",
}

// decisionCensusSession is the session the shared agent bearer writes this file's decision as,
// decisionCensusFollower the session that follows the ask, and decisionCensusRole the role the
// issue is routed to.
const (
	decisionCensusSession  = "ses_decision_census_agent"
	decisionCensusFollower = "ses_decision_census_follower"
	decisionCensusRole     = "decision-census-reader"
)

// agentBearer is the header the shared agent bearer's request carries; the body names the session
// it writes as.
func agentBearer() http.Header {
	return http.Header{"Authorization": {"Bearer " + dispatchTestAgentToken}}
}

// censusBody encodes body for a request, failing the test on a value that does not encode.
func censusBody(t *testing.T, body any) string {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode a request body: %v", err)
	}
	return string(encoded)
}

// storeCensusOf runs `envoy-dispatch census --store` over databaseURL, in this process, and
// answers its exit code and what it printed, every line of it logged.
func storeCensusOf(t *testing.T, databaseURL, when string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := census(context.Background(), []string{"--store"}, databaseURL, &out, &errOut)
	t.Logf("census --store %s exited %d:\n%s%s", when, code, out.String(), errOut.String())
	return code, out.String()
}

// The store census over a database the server wrote holds every kind of dispatch://INFRA-1150 to
// its projection with zero differences, and reads a difference once one shaped tuple reaches the
// wrong node. The server is this build, serving a database of its own and publishing to a NATS of
// the test's own, and every row is written through its API, as a deployment's are: a spec of three
// requirements; an ask a person asks in a document of the issue, shaping the first requirement,
// which another person answers and a session follows; a reading inside that answered ask; a copy
// of that ask on another document; an agent's decision shaping the first two requirements,
// settling the ask (from_ask) and holding a reading of its own; and a note citing the issue. The
// issue is routed to a role and commented on, as dispatch://INFRA-1149's census run addresses one
// (addressTheRole), so every kind of that child holds rows too - the note's mention the block_ref
// kind's, which holds no decision's shaped - and the census has no empty kind to refuse. `census --store` then
// prints one line for each of the eight kinds, each with rows and nodes or tuples and no difference
// in either direction, the store-only lines for copied_from and moved_to, and exits 0. One shaped
// tuple of the decision is then moved by SQL from the second requirement to the third, which no
// row of block_refs names: with both sides still counting three, the census names the row of the
// second that no tuple states and the tuple into the third that no row projects, and nothing of
// the first, and exits 1.
func TestTheStoreCensusHoldsThisChildsKindsToAServerSeededDatabase(t *testing.T) {
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	ctx := context.Background()
	_, natsURL := testnats.Start(t)
	process := startDispatchProcessWith(t, databaseURL, dispatchOptions{NATSURL: natsURL, SettleDelay: 200 * time.Millisecond})
	process.waitHealthy(t)

	process.call(t, http.MethodPost, "/api/v1/projects", `{"key":"CENS","name":"Census"}`, http.StatusCreated)
	var created struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(process.call(t, http.MethodPost, "/api/v1/issues", censusBody(t, map[string]any{
		"project": "CENS", "title": "The decisions census",
		"spec": ":::requirement{#r1}\nOne.\n:::\n\n:::requirement{#r2}\nTwo.\n:::\n\n:::requirement{#r3}\nThree.\n:::\n",
	}), http.StatusCreated), &created); err != nil || created.Key == "" {
		t.Fatalf("read the issue: %v", err)
	}
	issue := created.Key
	pointer := func(block string) string { return "dispatch://" + issue + "/spec#b-" + block }
	upload := func(header http.Header, name, content string, actor any) {
		t.Helper()
		body := map[string]any{"name": name, "content": content}
		if actor != nil {
			body["actor"] = actor
		}
		if answer := process.send(t, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", censusBody(t, body), header); answer.status != http.StatusCreated {
			t.Fatalf("upload %s: %s", name, answer)
		}
	}

	const question = "Which transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n"
	askBlock := "::::ask{#a1 urgency=\"med\" multiple=\"false\" shaped=\"" + pointer("r1") + "\"}\n" + question
	upload(asUser(dispatchTestLogin), "asks.md", askBlock+"::::\n", nil)
	askID := awaitBlockAsk(t, process, issue, "a1")
	if answer := process.send(t, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		`{"selected":["REST"],"text":"Plain HTTP for now.","expected_edited_at":null}`, asUser("bob")); answer.status != http.StatusOK {
		t.Fatalf("bob answers the ask: %s", answer)
	}
	if answer := process.send(t, http.MethodPut, "/api/v1/asks/"+askID+"/followers/"+decisionCensusFollower,
		censusBody(t, map[string]any{"actor": map[string]any{"kind": "session", "id": decisionCensusFollower}}), agentBearer()); answer.status != http.StatusNoContent {
		t.Fatalf("a session follows the ask: %s", answer)
	}
	upload(asUser(dispatchTestLogin), "asks.md", askBlock+"\n:::reading{#g1}\nThe agent reads it as REST for every service.\n:::\n::::\n", nil)
	upload(asUser(dispatchTestLogin), "copy.md", "::::ask{#a1 urgency=\"med\" multiple=\"false\"}\n"+question+
		"\n:::reading{#g2}\nThe agent reads the copied answer as REST.\n:::\n::::\n", nil)
	upload(agentBearer(), "choices.md", "::::decision{#d1 shaped=\""+pointer("r1")+" "+pointer("r2")+"\" from_ask=\"dispatch://"+issue+"/ask/"+askID+"\"}\n"+
		"REST for every service this quarter.\n\n:::reading{#g3}\nThe agent reads it so.\n:::\n::::\n",
		map[string]any{"kind": "session", "id": decisionCensusSession})
	// A note citing the issue, so INFRA-1149's block_ref kind holds a block's mention: a decision's
	// shaped is held by the shaped kind alone, and no other block of the fixture cites anything.
	upload(asUser(dispatchTestLogin), "notes.md", "The notes cite dispatch://"+issue+".\n", nil)
	process.call(t, http.MethodPatch, "/api/v1/issues/"+issue, `{"route":"role:`+decisionCensusRole+`"}`, http.StatusOK)
	process.mention(t, issue, "A comment on the routed issue, so the role is a destination.")
	awaitSettledCopy(t, database, askID)
	awaitRolePublished(t, database, decisionCensusRole)

	code, printed := storeCensusOf(t, databaseURL, "over the seeded database")
	if code != 0 {
		t.Fatalf("census --store over the seeded database exited %d, want 0", code)
	}
	requireCensusLine(t, printed, "block_ref", true)
	for _, name := range decisionCensusKinds {
		requireCensusLine(t, printed, name, true)
	}
	for _, line := range []string{
		"census: store-only kind=copied_from tuples=1 unresolved=0 left_out=0\n",
		"census: store-only kind=moved_to tuples=0 unresolved=0 left_out=0\n",
		"census: store kind=ask_parent unmarked_rows=0 ",
		"census: store kind=followed_by unmarked_rows=0 ",
		"census: store kind=shaped tuples=3 rows=3 table=block_refs rows_without_a_tuple=0 tuples_without_a_row=0 ",
	} {
		if !strings.Contains(printed, line) {
			t.Errorf("census --store over the seeded database printed no line %q", line)
		}
	}

	// The decision's tuple into r2 moves to r3: the rows still name r1 and r2 and the store now
	// states r1 and r3, so both sides count three, and the census names the row of r2 that no
	// tuple states and the tuple into r3 that no row projects, and nothing of r1, whose row and
	// tuple agree (dispatch://INFRA-1150 c-census: every reference, triple by triple).
	decision := issue + "/choices-md#d1"
	tag, err := database.Pool.Exec(ctx, `
		update graph_tuples set to_id = $3
		where from_kind = 'block' and from_id = $1 and kind = 'shaped' and to_kind = 'block' and to_id = $2
	`, decision, issue+"/spec#r2", issue+"/spec#r3")
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("move the decision's shaped tuple to the third requirement: %d rows, %v", tag.RowsAffected(), err)
	}
	code, printed = storeCensusOf(t, databaseURL, "once a shaped tuple reaches the wrong node")
	if code != 1 {
		t.Fatalf("census --store once a shaped tuple reaches the wrong node exited %d, want 1", code)
	}
	requireCensusLine(t, printed, refs.TupleShaped, false)
	for _, want := range []string{
		"census: store kind=shaped tuples=3 rows=3 table=block_refs rows_without_a_tuple=1 tuples_without_a_row=1 ",
		"census: store kind=shaped difference at block_ref " + decision + " shaped -> block " + issue + "/spec#r2: a row no tuple states\n",
		"census: store kind=shaped difference at block:" + decision + " shaped -> block:" + issue + "/spec#r3: a tuple no row projects\n",
		"census: store REFUSED (2 difference(s))\n",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("census --store once a shaped tuple reaches the wrong node printed no line holding %q", want)
		}
	}
	if unchanged := " difference at block_ref " + decision + " shaped -> block " + issue + "/spec#r1"; strings.Contains(printed, unchanged) {
		t.Errorf("census --store once a shaped tuple reaches the wrong node printed a line holding %q, whose row and tuple agree", unchanged)
	}
}

// awaitRolePublished waits for the publisher to record the role an issue was routed to: the role's
// node and its page, which the publisher writes from the events that address the role
// (outbox.addressedRoles), so the census's role and page kinds hold their rows' nodes.
func awaitRolePublished(t *testing.T, database *store.Store, role string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var held int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from graph_nodes where (kind = $1 and id = $2) or (kind = $3 and id = $4)
		`, graphstore.KindRole, role, graphstore.KindPage, graphstore.RolePageID(role)).Scan(&held); err != nil {
			t.Fatalf("read the role's node and page: %v", err)
		}
		if held == 2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the publisher wrote no node and page of the role %s", role)
}

// requireCensusLine fails unless the census printed a kind line for name holding rows and nodes or
// tuples, and, where agrees, no difference in either direction; where it does not, at least one.
// A kind held triple by triple prints tuples= and its differences as rows without a tuple and
// tuples without a row, and any other nodes= and rows without a node and nodes without a row.
func requireCensusLine(t *testing.T, printed, name string, agrees bool) {
	t.Helper()
	for _, line := range strings.Split(printed, "\n") {
		if !strings.HasPrefix(line, "census: store kind="+name+" nodes=") && !strings.HasPrefix(line, "census: store kind="+name+" tuples=") {
			continue
		}
		fields := map[string]string{}
		for _, field := range strings.Fields(strings.TrimPrefix(line, "census: store ")) {
			if key, value, found := strings.Cut(field, "="); found {
				fields[key] = value
			}
		}
		side, rowsMissing, sideMissing := "nodes", "rows_without_a_node", "nodes_without_a_row"
		if _, triples := fields["tuples"]; triples {
			side, rowsMissing, sideMissing = "tuples", "rows_without_a_tuple", "tuples_without_a_row"
		}
		if fields["rows"] == "0" || fields[side] == "0" {
			t.Errorf("the census line of %s reads %q, which compared nothing", name, line)
		}
		differs := fields[rowsMissing] != "0" || fields[sideMissing] != "0"
		if differs == agrees {
			t.Errorf("the census line of %s reads %q, want differences %t", name, line, !agrees)
		}
		return
	}
	t.Errorf("the census printed no line for kind %s", name)
}

// awaitBlockAsk waits for the settlement of the upload that holds blockID to open its ask, and
// answers the ask's id.
func awaitBlockAsk(t *testing.T, process *dispatchProcess, issue, blockID string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var asks []struct {
			ID      string  `json:"id"`
			BlockID *string `json:"block_id"`
		}
		if err := json.Unmarshal(process.call(t, http.MethodGet, "/api/v1/issues/"+issue+"/asks?state=all", "", http.StatusOK), &asks); err != nil {
			t.Fatalf("read the asks of %s: %v", issue, err)
		}
		for _, ask := range asks {
			if ask.BlockID != nil && *ask.BlockID == blockID {
				return ask.ID
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no settlement opened the ask of block %s on %s:\n%s", blockID, issue, process.Output.String())
	return ""
}

// awaitSettledCopy waits for the settlement of the copy's document to state its block's
// copied_from tuple to the ask it copies, and for every document's block index to stand, so the
// census reads a database with nothing owed.
func awaitSettledCopy(t *testing.T, database *store.Store, askID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var copied, owed int
		if err := database.Pool.QueryRow(context.Background(), `
			select (select count(*) from graph_tuples where kind = $1 and to_kind = $2 and to_id = $3 and asserted),
			       (select count(*) from document_block_versions where version is null)
		`, graphstore.TupleCopiedFrom, graphstore.KindAsk, askID).Scan(&copied, &owed); err != nil {
			t.Fatalf("read the copy's tuple and the owed block indexes: %v", err)
		}
		if copied == 1 && owed == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no settlement stated the copy's copied_from to ask %s, or a block index is still owed", askID)
}
