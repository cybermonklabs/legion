package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/refs/refstest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// interactionCredential is one credential a run is written or read with: how a failure names it,
// a bearer's plaintext (a person's personal token, or the deployment's shared token) or the login
// a cookie proves, and the actor the server resolves it to (server.optionalActor).
type interactionCredential struct {
	name  string
	token string
	login string
	actor model.Actor
}

// newRequest is one request under the credential, naming run in contracts.GoingRunHeader where
// run is not empty, as a write made for that run sends it (GoingCaller.Do).
func (c interactionCredential) newRequest(t *testing.T, method, target string, body any, run string) *http.Request {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the request body: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	} else {
		request.Header.Set("X-Dispatch-User", c.login)
	}
	if run != "" {
		request.Header.Set(contracts.GoingRunHeader, run)
	}
	return request
}

// request carries one request under the credential (newRequest) through handler.
func (c interactionCredential) request(t *testing.T, handler http.Handler, method, target string, body any, run string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, c.newRequest(t, method, target, body, run))
	return response
}

// references reads one address through GET /api/v1/references under the credential, in one
// direction: `to`, the edges into it, or `from`, the edges out of it.
func (c interactionCredential) references(t *testing.T, handler http.Handler, direction, address string) *httptest.ResponseRecorder {
	t.Helper()
	return c.request(t, handler, http.MethodGet, "/api/v1/references?"+direction+"="+url.QueryEscape(address), nil, "")
}

// TestInteractionNodesAreTuplesReadByTheirOwnCredential is c-interaction-declared-and-private
// (spec requirement r-interaction). It holds the kind's declarations and its privacy in
// one place:
//
//   - interaction has a registry entry and backlink arms, so a count of it is answered and not
//     refused; changed_by_run and authored_by are tuple kinds the graph_edges view answers from the
//     tuple table alone, as its `store` class, and no arm of the view states either over a column
//     of a source table; the committed OpenAPI document and the dashboard's NODE_LABELS name it.
//   - A run alice's personal token records, and a comment it writes under that run, are read by
//     bob's personal token as what an address nobody holds answers, with no changed_by_run and
//     an unmoved count on alice's person node, although SameAs calls the two tokens one writer.
//     The run's going_requests.author is its node's made_by. Bob's comment under alice's run is
//     refused as the stream refuses a run nobody holds, and stores nothing; a second write of
//     alice's comment under the run appends no second tuple; alice's cookie reads nothing of the
//     run her token recorded, at its address, in the comment's references or in the count of her
//     own person node; a run of model.Actor{} is read by nobody. A comment alice's token writes
//     under her live run with an Idempotency-Key, sent again under the same key and run header
//     once the run has ended, is answered its stored 201 with the same comment, and the store
//     holds the one comment and its one changed_by_run; the same key with no run header is
//     422 KEY_REUSED.
//   - The node, its authored_by and each frame value are written before either projected row, in
//     the one transaction of their writer: triggers in the test's own database fail the write
//     before the projected row and after it and find neither side afterwards, record the
//     transaction id of each statement through txid_current, and hold a write open while a
//     second connection reads; two goroutines appending to one run take two sequences with no
//     gap, the second queued at the run's interaction row.
func TestInteractionNodesAreTuplesReadByTheirOwnCredential(t *testing.T) {
	t.Parallel()
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	server := directServer(deps)
	requireInteractionDeclared(t, database)
	run, comment := requireARunIsReadByTheCredentialThatRecordedIt(t, handler, server, database)
	requireTheViewAnswersTheRunsTuplesFromTheStoreAlone(t, database, run, comment)
	requireTheRepositoryNamesTheKind(t)
	requireAProjectedRowIsWrittenInItsNodesTransaction(t, server, database)
	requireTwoFrameWritersQueueAtTheRunsNode(t, server, database)
}

// requireInteractionDeclared holds the kind's declarations: an entry of refs.nodeRegistry with a
// loader (refs.VisibilityKinds, refs.ResolvesNodeKind), arms of refs.backlinkArms (refstest.BacklinkKinds)
// so refs.BacklinkCounts answers a count of it where it refuses a kind with none, and changed_by_run
// and authored_by among the relation kinds the view's tuple arm answers (refs.TupleKinds).
func requireInteractionDeclared(t *testing.T, database *store.Store) {
	ctx := context.Background()
	if !slices.Contains(refs.VisibilityKinds(), graphstore.KindInteraction) || !refs.ResolvesNodeKind(graphstore.KindInteraction) {
		t.Errorf("refs.nodeRegistry holds no entry with a loader for %s: it holds %v", graphstore.KindInteraction, refs.VisibilityKinds())
	}
	if !slices.Contains(refstest.BacklinkKinds(), graphstore.KindInteraction) {
		t.Errorf("refs.backlinkArms declares no arms for %s: it declares %v", graphstore.KindInteraction, refstest.BacklinkKinds())
	}
	counted, err := refs.BacklinkCounts(ctx, database.Pool, model.Actor{}, graphstore.KindInteraction, []string{uuid.NewString()}, nil)
	t.Logf("a count of an interaction answers %v, %v", counted, err)
	if err != nil {
		t.Errorf("a count of an interaction is refused: %v", err)
	}
	// The refusal a kind with no arms meets, so the answer above is read as the count's own. It
	// carries no sentinel, so it is read by the kind it names.
	const undeclared = "no_such_node_kind"
	if _, err := refs.BacklinkCounts(ctx, database.Pool, model.Actor{}, undeclared, []string{"x"}, nil); err == nil ||
		!graphstoretest.NamesAsAWord(err.Error(), undeclared) {
		t.Errorf("a count of %s, which declares no arms, answers %v, want the refusal backlinkTargetArms makes, naming %s", undeclared, err, undeclared)
	}
	for _, kind := range []string{graphstore.TupleChangedByRun, graphstore.TupleAuthoredBy} {
		if !slices.Contains(refs.TupleKinds(), kind) {
			t.Errorf("%s is no tuple kind the graph_edges view answers from the tuple table: refs.TupleKinds is %v", kind, refs.TupleKinds())
		}
	}
}

// requireARunIsReadByTheCredentialThatRecordedIt records a run with alice's personal token through
// the going POST and writes one comment on an issue under it with the same token, naming the run in
// the request header, and reads both as each credential, keeping alice's token's reads beside the
// others so each absence stands beside the same read finding the run. Each credential is the actor
// the server resolves it to. It answers the run's id and the comment's.
func requireARunIsReadByTheCredentialThatRecordedIt(t *testing.T, handler http.Handler, server *server, database *store.Store) (string, string) {
	ctx := context.Background()
	pool := database.Pool
	const words = "Which credential reads the run that wrote this comment?"
	issue := createTestIssue(t, handler, "ICR", "The issue a token's run writes on")
	resolved := func(credential interactionCredential) interactionCredential {
		t.Helper()
		actor, _, err := server.optionalActor(credential.newRequest(t, http.MethodGet, "/api/v1/references", nil, ""))
		if err != nil {
			t.Fatalf("resolve %s: %v", credential.name, err)
		}
		credential.actor = actor
		return credential
	}
	alice := resolved(interactionCredential{name: "alice's personal token", token: personalTokenOwnedBy(t, database, "alice", model.ScopePerson)})
	bob := resolved(interactionCredential{name: "bob's personal token", token: personalTokenOwnedBy(t, database, "bob", model.ScopePerson)})
	cookie := resolved(interactionCredential{name: "alice's cookie", login: "alice"})
	shared := resolved(interactionCredential{name: "the shared bearer", token: "agent-token"})
	if cookie.actor.Kind != "user" || cookie.actor.ID != "alice" || shared.actor.Kind != "" || shared.actor.ID != "" || shared.actor.Owner != nil {
		t.Fatalf("alice's cookie resolves as %+v and the shared bearer as %+v: want alice, and nobody", cookie.actor, shared.actor)
	}
	// The premise: SameAs, which compares kind and id alone, calls the two tokens one writer, and
	// only the whole credential tells them apart.
	if !alice.actor.SameAs(bob.actor) || alice.actor.SameCredential(bob.actor) {
		t.Fatalf("alice's token resolves as %+v and bob's as %+v: want two credentials SameAs calls one writer and SameCredential two",
			alice.actor, bob.actor)
	}

	// What each token reads of alice's person node, where the run's authored_by points: the count,
	// and the tuples into it behind that count.
	personNode := func(who interactionCredential) (int, []string) {
		t.Helper()
		counts, err := refs.BacklinkCounts(ctx, pool, who.actor, graphstore.KindPerson, []string{"alice"}, nil)
		if err != nil {
			t.Fatalf("count the backlinks of alice's person node as %s: %v", who.name, err)
		}
		edges, err := refs.Edges(ctx, pool, who.actor, refs.Query{Direction: "in", Kind: graphstore.KindPerson, ID: "alice"})
		if err != nil {
			t.Fatalf("read the tuples into alice's person node as %s: %v", who.name, err)
		}
		t.Logf("%s reads alice's person node: count %d, tuples %v", who.name, counts["alice"], edgeEnds(edges))
		return counts["alice"], edgeEnds(edges)
	}
	countBefore := map[string]int{}
	for _, who := range []interactionCredential{alice, bob, cookie} {
		countBefore[who.name], _ = personNode(who)
	}

	asked := alice.request(t, handler, http.MethodPost, "/api/v1/going/requests", map[string]any{"words": words}, "")
	if asked.Code != http.StatusCreated {
		t.Fatalf("ask under alice's personal token: status=%d body=%s", asked.Code, asked.Body.String())
	}
	run := decodeBody[askedRun](t, asked).ID
	goingRoutesRig{handler: handler, server: server, database: database}.awaitTerminal(t, run)
	runWindowAhead(t, database, run)
	requireTheRowProjectsTheNode(t, database, "the run alice's token recorded", run, alice.actor)
	// A personal token proves no session, so a bearer's write names the one it acts for in its body
	// (bearerSessionActor); the run is read off the header by the whole credential all the same.
	written := alice.request(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"body": "The comment alice's token wrote for its run.", "actor": map[string]any{"kind": "session", "id": "alice-agent"},
	}, run)
	if written.Code != http.StatusCreated {
		t.Fatalf("write the comment under the run with alice's personal token: status=%d body=%s", written.Code, written.Body.String())
	}
	comment := decodeBody[writtenRecord](t, written).ID
	interactionRef := "dispatch://" + graphstore.KindInteraction + "/" + run
	commentRef := "dispatch://" + issue + "/comment/" + comment
	if held := changedByRunCount(t, database, graphstore.KindComment, comment, run); held != 1 {
		t.Fatalf("comment %s holds %d changed_by_run tuples to run %s, want the one its write appended", comment, held, run)
	}

	// Alice's token reads the run at its address and the comment's changed_by_run to it.
	own := alice.references(t, handler, "from", interactionRef)
	t.Logf("%s's ?from=%s: status=%d body=%s", alice.name, interactionRef, own.Code, own.Body.String())
	if own.Code != http.StatusOK || decodeBody[model.GraphReferences](t, own).Node.Ref != interactionRef {
		t.Fatalf("%s reads its own run at %s: status=%d body=%s, want the interaction node", alice.name, interactionRef, own.Code, own.Body.String())
	}
	byRun := func(who interactionCredential) ([]model.GraphEdge, string) {
		t.Helper()
		response := who.references(t, handler, "from", commentRef)
		t.Logf("%s's ?from=%s: status=%d body=%s", who.name, commentRef, response.Code, response.Body.String())
		if response.Code != http.StatusOK {
			t.Fatalf("%s reads the comment's references: status=%d body=%s", who.name, response.Code, response.Body.String())
		}
		edges := slices.DeleteFunc(decodeBody[model.GraphReferences](t, response).Edges, func(edge model.GraphEdge) bool {
			return edge.Kind != graphstore.TupleChangedByRun && edge.Node.Kind != graphstore.KindInteraction
		})
		return edges, response.Body.String()
	}
	aliceEdges, _ := byRun(alice)
	if len(aliceEdges) != 1 || aliceEdges[0].Kind != graphstore.TupleChangedByRun || aliceEdges[0].Node.Ref != interactionRef ||
		aliceEdges[0].Excerpt == nil || aliceEdges[0].Excerpt.Text != words {
		t.Fatalf("%s reads the comment's run edges %+v, want its one changed_by_run to %s with the run's words", alice.name, aliceEdges, interactionRef)
	}

	// Bob's token, and alice's cookie, read the run at its address as an address nobody holds, in
	// both directions, and the comment with no changed_by_run; the words of the run are in nothing
	// either read.
	var bodies []string
	for _, reader := range []interactionCredential{bob, cookie} {
		edges, body := byRun(reader)
		if len(edges) != 0 {
			t.Errorf("%s reads the comment's run edges %+v, want no changed_by_run and no interaction", reader.name, edges)
		}
		bodies = append(bodies, body)
	}
	nobodysID := uuid.NewString()
	nobodys := "dispatch://" + graphstore.KindInteraction + "/" + nobodysID
	for _, reader := range []interactionCredential{bob, cookie} {
		for _, direction := range []string{"to", "from"} {
			hidden := reader.references(t, handler, direction, interactionRef)
			requireNobodysAnswer(t, reader.name+"'s ?"+direction+"= of alice's token's run", hidden,
				reader.references(t, handler, direction, nobodys), run, nobodysID)
			bodies = append(bodies, hidden.Body.String())
		}
	}
	for _, body := range bodies {
		if strings.Contains(body, words) {
			t.Errorf("a credential other than alice's personal token read the run's words %q in %s", words, body)
		}
	}
	for _, reader := range []interactionCredential{bob, cookie, shared} {
		if node, found, err := refs.Node(ctx, pool, reader.actor, graphstore.KindInteraction, run, true); err != nil || found {
			t.Errorf("%s reads the run alice's token recorded: found %t (%+v), %v; want what an address nobody holds answers", reader.name, found, node, err)
		}
	}
	// The read that answers a node with its words, cut as an edge at it shows them
	// (refs.ExcerptedNodes): alice's token reads the run's words in it, and no other credential
	// reads the run there at all.
	target := refs.Target{Kind: graphstore.KindInteraction, ID: run}
	for _, reader := range []interactionCredential{alice, bob, cookie, shared} {
		excerpted, err := refs.ExcerptedNodes(ctx, pool, reader.actor, []refs.Target{target})
		if err != nil {
			t.Fatalf("read the run's words as %s: %v", reader.name, err)
		}
		node, found := excerpted[target]
		var read string
		if found && node.Excerpt != nil {
			read = node.Excerpt.Text
		}
		t.Logf("%s reads the run through refs.ExcerptedNodes: found %t, its words %q", reader.name, found, read)
		switch own := reader.name == alice.name; {
		case own && read != words:
			t.Fatalf("%s reads its own run's words as %q (found %t), want %q", reader.name, read, found, words)
		case !own && (found || read != ""):
			t.Errorf("%s reads the run's words %q through refs.ExcerptedNodes (found %t): the run is alice's token's alone", reader.name, read, found)
		}
	}
	// A count of the run is answered, not refused: the comment's changed_by_run points at it.
	if counts, err := refs.BacklinkCounts(ctx, pool, alice.actor, graphstore.KindInteraction, []string{run}, nil); err != nil || counts[run] != 1 {
		t.Errorf("%s counts the backlinks of its run as %v (%v), want the comment's one changed_by_run", alice.name, counts, err)
	}

	// The run's authored_by raises the count of alice's person node for the credential that
	// recorded it, and for bob's token and alice's own cookie the count is unmoved and is the
	// tuples each is given.
	if count, tuples := personNode(alice); count != countBefore[alice.name]+1 || count != len(tuples) {
		t.Errorf("%s reads a backlink count of %d on alice's person node, %d before the run, over the tuples %v: want one more, the run's authored_by",
			alice.name, count, countBefore[alice.name], tuples)
	}
	for _, reader := range []interactionCredential{bob, cookie} {
		count, tuples := personNode(reader)
		if count != len(tuples) {
			t.Errorf("%s reads a backlink count of %d on alice's person node, %d higher than the %d tuples into it the same credential is given (%v)",
				reader.name, count, count-len(tuples), len(tuples), tuples)
		}
		if count != countBefore[reader.name] {
			t.Errorf("%s reads a backlink count of %d on alice's person node, %d before alice's token recorded its run: want it unmoved",
				reader.name, count, countBefore[reader.name])
		}
	}

	// Bob's comment under alice's run is answered what the stream answers for a run nobody holds,
	// and stores nothing.
	comments := func() int {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, `select count(*) from comments where issue_key = $1`, issue).Scan(&count); err != nil {
			t.Fatalf("count the issue's comments: %v", err)
		}
		return count
	}
	heldComments, heldTuples := comments(), tuplesIntoRun(t, database, run)
	refused := bob.request(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"body": "Bob's comment for alice's run.", "actor": map[string]any{"kind": "session", "id": "bob-agent"},
	}, run)
	for name, stream := range map[string]*httptest.ResponseRecorder{
		"a run nobody holds": bob.request(t, handler, http.MethodGet, "/api/v1/going/requests/"+nobodysID+"/stream", nil, ""),
		"alice's run":        bob.request(t, handler, http.MethodGet, "/api/v1/going/requests/"+run+"/stream", nil, ""),
	} {
		t.Logf("%s's comment under alice's run: status=%d body=%s; the stream of %s to him: status=%d body=%s",
			bob.name, refused.Code, refused.Body.String(), name, stream.Code, stream.Body.String())
		if refused.Code != stream.Code || refused.Body.String() != stream.Body.String() ||
			refused.Header().Get("Content-Type") != stream.Header().Get("Content-Type") {
			t.Errorf("%s's comment under alice's run answers %d %s, and the stream answers him %d %s for %s",
				bob.name, refused.Code, refused.Body.String(), stream.Code, stream.Body.String(), name)
		}
	}
	if refused.Code != http.StatusNotFound || decodeBody[errorBody](t, refused).Code != codeGoingRequestNotFound {
		t.Errorf("%s's comment under alice's run answers %d %s, want 404 %s", bob.name, refused.Code, refused.Body.String(), codeGoingRequestNotFound)
	}
	if count := comments(); count != heldComments {
		t.Errorf("%s's refused comment left the issue with %d comments, want the %d it held", bob.name, count, heldComments)
	}
	if tuples := tuplesIntoRun(t, database, run); tuples != heldTuples {
		t.Errorf("%s's refused comment left %d tuples into alice's run, want the %d it held", bob.name, tuples, heldTuples)
	}

	// A second write that changes alice's comment under the same run, its state moved to proposed,
	// marks the comment changed again and appends no second tuple.
	changed := alice.request(t, handler, http.MethodPost, statePath(issue, graphstore.KindComment+"/"+comment, "state"), map[string]any{
		"state": graphstore.StateProposed, "actor": map[string]any{"kind": "session", "id": "alice-agent"},
	}, run)
	t.Logf("%s's second write of the comment under the run: status=%d body=%s", alice.name, changed.Code, changed.Body.String())
	if changed.Code != http.StatusOK {
		t.Fatalf("write the comment's state under the run with alice's personal token: status=%d body=%s", changed.Code, changed.Body.String())
	}
	if moved := decodeBody[nodeStateWritten](t, changed); moved.Previous != graphstore.StateLive || moved.Node.State != graphstore.StateProposed {
		t.Fatalf("the second write of the comment answers %+v, want it moved from live to proposed", moved)
	}
	if held := changedByRunCount(t, database, graphstore.KindComment, comment, run); held != 1 {
		t.Errorf("comment %s holds %d changed_by_run tuples to run %s after a second write under it, want the one", comment, held, run)
	}
	requireARunOfNobodyIsReadByNobody(t, handler, server, database, []interactionCredential{alice, bob, cookie, shared})
	requireARepeatAfterItsRunEndedReadsTheWrite(t, handler, database, alice, issue, run)
	return run, comment
}

// requireARepeatAfterItsRunEndedReadsTheWrite writes a comment under alice's live run with an
// Idempotency-Key, ends the run, and sends the same request again under the same key and the same
// run header. The key layer answers it the comment the first request made, its 201 replayed,
// before the run is checked (withinGoingRun, answerRefusedRun), and the issue holds that one
// comment with its one changed_by_run to the run. The same key sent with no run header is another
// request, since the key's digest holds the header (writeRequestFields): 422 KEY_REUSED, and
// nothing more is stored. Ending the run moves its going_requests.ends_at into the past, as
// runWindowAhead moved it ahead: that row is what the run check reads (readLiveGoingRun). A fresh
// comment under the ended run is refused first, so the replay is read off a run the check refuses.
func requireARepeatAfterItsRunEndedReadsTheWrite(t *testing.T, handler http.Handler, database *store.Store, alice interactionCredential, issue, run string) {
	ctx := context.Background()
	const key = "a-comment-repeated-after-its-run-ended"
	const words = "The comment alice's token wrote under a key for its run."
	body := map[string]any{"body": words, "actor": map[string]any{"kind": "session", "id": "alice-agent"}}
	keyed := func(run string) *httptest.ResponseRecorder {
		t.Helper()
		request := alice.newRequest(t, http.MethodPost, "/api/v1/issues/"+issue+"/comments", body, run)
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	held := func() int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from comments where issue_key = $1 and body = $2`, issue, words).Scan(&count); err != nil {
			t.Fatalf("count the issue's comments of the keyed words: %v", err)
		}
		return count
	}

	first := keyed(run)
	t.Logf("%s's keyed comment under its live run: status=%d body=%s", alice.name, first.Code, first.Body.String())
	if first.Code != http.StatusCreated {
		t.Fatalf("write the keyed comment under the run with %s: status=%d body=%s", alice.name, first.Code, first.Body.String())
	}
	comment := decodeBody[writtenRecord](t, first).ID
	if _, err := database.Pool.Exec(ctx, `update going_requests set ends_at = now() - interval '1 second' where id = $1::uuid`, run); err != nil {
		t.Fatalf("end run %s: %v", run, err)
	}
	fresh := alice.request(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"body": "A comment written for the run once it ended.", "actor": map[string]any{"kind": "session", "id": "alice-agent"},
	}, run)
	if fresh.Code != http.StatusNotFound || decodeBody[errorBody](t, fresh).Code != codeGoingRequestNotFound {
		t.Fatalf("a fresh comment under the ended run answers %d %s, want 404 %s: the run check must refuse the run before its replay is read",
			fresh.Code, fresh.Body.String(), codeGoingRequestNotFound)
	}

	repeated := keyed(run)
	t.Logf("the same keyed comment under the ended run: status=%d replayed=%q body=%s",
		repeated.Code, repeated.Header().Get(contracts.IdempotencyReplayedHeader), repeated.Body.String())
	if repeated.Code != http.StatusCreated || repeated.Header().Get(contracts.IdempotencyReplayedHeader) != "?1" ||
		decodeBody[writtenRecord](t, repeated).ID != comment {
		t.Errorf("the same keyed comment sent again under %s's run once it ended answers %d (Idempotency-Replayed %q) %s, want the stored 201 replayed with comment %s",
			alice.name, repeated.Code, repeated.Header().Get(contracts.IdempotencyReplayedHeader), repeated.Body.String(), comment)
	}
	if count := held(); count != 1 {
		t.Errorf("after the repeat the issue holds %d comments of the keyed words, want the one its first request wrote", count)
	}
	if tuples := changedByRunCount(t, database, graphstore.KindComment, comment, run); tuples != 1 {
		t.Errorf("after the repeat comment %s holds %d changed_by_run tuples to run %s, want the one its first request appended", comment, tuples, run)
	}

	unnamed := keyed("")
	t.Logf("the same keyed comment with no run header: status=%d body=%s", unnamed.Code, unnamed.Body.String())
	if unnamed.Code != http.StatusUnprocessableEntity || decodeBody[errorBody](t, unnamed).Code != contracts.IdempotencyKeyReused {
		t.Errorf("the same key sent with no run header answers %d %s, want 422 %s: the key's comparison holds the run header",
			unnamed.Code, unnamed.Body.String(), contracts.IdempotencyKeyReused)
	}
	if count := held(); count != 1 {
		t.Errorf("after the key was sent with no run header the issue holds %d comments of the keyed words, want the one", count)
	}
}

// requireARunOfNobodyIsReadByNobody records a run for model.Actor{}, which only a direct call writes,
// and reads it as each credential: the store holds its node, and no credential reads it.
func requireARunOfNobodyIsReadByNobody(t *testing.T, handler http.Handler, server *server, database *store.Store, readers []interactionCredential) {
	ctx := context.Background()
	nobodys, err := server.recordGoingRequest(ctx, model.Actor{}, "", "Whose run is this one?")
	if err != nil {
		t.Fatalf("record a run for model.Actor{}: %v", err)
	}
	if node, held, err := graphstore.ReadNode(ctx, database.Pool, graphstore.KindInteraction, nobodys.ID); err != nil || !held {
		t.Fatalf("the store holds the node of the run of model.Actor{}: %t (%v), want it written like any run", held, err)
	} else {
		t.Logf("the store holds the run of model.Actor{} as %s/%s at %s", node.Kind, node.ID, node.Visibility)
	}
	requireTheRowProjectsTheNode(t, database, "the run of model.Actor{}", nobodys.ID, model.Actor{})
	address := "dispatch://" + graphstore.KindInteraction + "/" + nobodys.ID
	nobodysID := uuid.NewString()
	for _, reader := range readers {
		if node, found, err := refs.Node(ctx, database.Pool, reader.actor, graphstore.KindInteraction, nobodys.ID, true); err != nil || found {
			t.Errorf("%s reads the run of model.Actor{}: found %t (%+v), %v; want nobody to", reader.name, found, node, err)
		}
		requireNobodysAnswer(t, reader.name+"'s ?from= of the run of model.Actor{}", reader.references(t, handler, "from", address),
			reader.references(t, handler, "from", "dispatch://"+graphstore.KindInteraction+"/"+nobodysID), nobodys.ID, nobodysID)
	}
}

// requireTheRowProjectsTheNode holds a run's going_requests row to its interaction node: the
// row's author is the node's made_by, compared as jsonb in the one select that reads both, so
// the column is a projection of the node and not a second record of the asker; and that made_by
// is the credential that recorded the run, compared as model.Actor.SameCredential compares one.
func requireTheRowProjectsTheNode(t *testing.T, database *store.Store, name, run string, recorder model.Actor) {
	t.Helper()
	var madeBy, author []byte
	var same bool
	if err := database.Pool.QueryRow(context.Background(), `
		select held.made_by, projected.author, held.made_by = projected.author
		from graph_nodes held join going_requests projected on projected.id::text = held.id
		where held.kind = $1 and held.id = $2`, graphstore.KindInteraction, run).Scan(&madeBy, &author, &same); err != nil {
		t.Fatalf("read the node and the going_requests row of %s: %v", name, err)
	}
	t.Logf("%s: its node's made_by %s, its going_requests.author %s", name, madeBy, author)
	var recorded model.Actor
	if err := json.Unmarshal(madeBy, &recorded); err != nil {
		t.Fatalf("decode the made_by of %s: %v", name, err)
	}
	if !same || !recorded.SameCredential(recorder) {
		t.Errorf("%s: going_requests.author is %s and its node's made_by %s, want the one recorded actor of the credential %+v, the row a projection of the node",
			name, author, madeBy, recorder)
	}
}

// literalArmKind is the kind an arm of graph_edges states as a literal, as the catalogue prints the
// view back: the arm of a source table names its edge kind so, where the arms over the tuple table
// and over refs and the block tables read it from a column.
var literalArmKind = regexp.MustCompile(`'([a-z_]+)'::text AS kind`)

// requireTheViewAnswersTheRunsTuplesFromTheStoreAlone reads graph_edges out of the catalogue. No arm
// of it states changed_by_run or authored_by as the literal kind of a source table's rows - the
// same pattern finds the arms that do state a literal kind, so its zero is read off a search that
// finds - and every arm that names either kind reads graph_tuples. The tuple arm's class `store`
// takes changed_by_run out of any node and authored_by out of an interaction, and consults no
// projection row. The view answers the comment's changed_by_run and the run's authored_by.
func requireTheViewAnswersTheRunsTuplesFromTheStoreAlone(t *testing.T, database *store.Store, run, comment string) {
	ctx := context.Background()
	var definition string
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges'::regclass, true)`).Scan(&definition); err != nil {
		t.Fatalf("read the graph_edges definition: %v", err)
	}
	var literal []string
	for _, match := range literalArmKind.FindAllStringSubmatch(definition, -1) {
		literal = append(literal, match[1])
	}
	t.Logf("the arms of graph_edges that state their kind as a literal state %v", literal)
	if !slices.Contains(literal, "child_of") {
		t.Fatalf("the search for an arm's literal kind finds no child_of arm in the view, so it finds nothing:\n%s", definition)
	}
	arms := strings.Split(definition, "UNION ALL")
	for _, kind := range []string{graphstore.TupleChangedByRun, graphstore.TupleAuthoredBy} {
		if slices.Contains(literal, kind) {
			t.Errorf("an arm of graph_edges states %s as the literal kind of a source table's rows", kind)
		}
		for index, arm := range arms {
			if strings.Contains(arm, "'"+kind+"'::text") && !strings.Contains(arm, "FROM graph_tuples") {
				t.Errorf("arm %d of graph_edges names %s and reads no graph_tuples:\n%s", index+1, kind, arm)
			}
		}
	}
	class := strings.Index(definition, "THEN 'store'::text")
	if class < 0 {
		t.Fatalf("the tuple arm of graph_edges holds no class store:\n%s", definition)
	}
	condition := definition[strings.LastIndex(definition[:class], "WHEN ")+len("WHEN ") : class]
	t.Logf("the tuple arm classes a tuple store when %s", condition)
	for _, named := range []string{graphstore.TupleChangedByRun, graphstore.TupleAuthoredBy, graphstore.KindInteraction} {
		if !strings.Contains(condition, "'"+named+"'::text") {
			t.Errorf("the tuple arm's class store does not name %s: it is %s", named, condition)
		}
	}
	if !strings.Contains(definition, "WHEN 'store'::text THEN true") {
		t.Errorf("the tuple arm consults a projection row for its class store:\n%s", definition)
	}
	for _, edge := range []struct {
		what                                 string
		fromKind, fromID, kind, toKind, toID string
	}{
		{"the comment's changed_by_run", graphstore.KindComment, comment, graphstore.TupleChangedByRun, graphstore.KindInteraction, run},
		{"the run's authored_by", graphstore.KindInteraction, run, graphstore.TupleAuthoredBy, graphstore.KindPerson, "alice"},
	} {
		var rows int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from graph_edges
			where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 and to_id = $5
		`, edge.fromKind, edge.fromID, edge.kind, edge.toKind, edge.toID).Scan(&rows); err != nil {
			t.Fatalf("read %s out of graph_edges: %v", edge.what, err)
		}
		t.Logf("graph_edges answers %s %d time(s)", edge.what, rows)
		if rows != 1 {
			t.Errorf("graph_edges answers %s %d times, want once, from its tuple", edge.what, rows)
		}
	}
}

// requireTheRepositoryNamesTheKind reads the committed OpenAPI document, whose GraphNode.kind enum
// holds interaction, and the dashboard's ReferencedBy.tsx, whose NODE_LABELS holds the key. The
// type check that refuses a missing key runs in CI.
func requireTheRepositoryNamesTheKind(t *testing.T) {
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(committedOpenAPI(t), &document); err != nil {
		t.Fatalf("decode %s: %v", OpenAPIDocumentPath, err)
	}
	kinds := document.Components.Schemas["GraphNode"].Properties["kind"].Enum
	t.Logf("%s declares GraphNode.kind as one of %v", OpenAPIDocumentPath, kinds)
	if !slices.Contains(kinds, graphstore.KindInteraction) {
		t.Errorf("%s declares GraphNode.kind as one of %v, which holds no %s", OpenAPIDocumentPath, kinds, graphstore.KindInteraction)
	}
	const labels = "packages/dispatch/web/src/features/refs/ReferencedBy.tsx"
	_, table, found := strings.Cut(readRepositoryFile(t, repositoryRoot(t), labels), "const NODE_LABELS")
	table, _, closed := strings.Cut(table, "\n};")
	if !found || !closed {
		t.Fatalf("%s holds no NODE_LABELS table", labels)
	}
	keys := regexp.MustCompile(`(?m)^\s*([a-z_]+):`).FindAllStringSubmatch(table, -1)
	var named []string
	for _, key := range keys {
		named = append(named, key[1])
	}
	t.Logf("%s's NODE_LABELS holds the keys %v", labels, named)
	if !slices.Contains(named, graphstore.KindInteraction) {
		t.Errorf("%s's NODE_LABELS holds the keys %v, and no %s", labels, named, graphstore.KindInteraction)
	}
}
