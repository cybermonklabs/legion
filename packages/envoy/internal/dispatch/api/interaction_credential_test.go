package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
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
// (dispatch://INFRA-1950/spec#b-r-interaction). It holds the kind's declarations and its privacy in
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
// loader (refs.VisibilityKinds, refs.ResolvesNodeKind), arms of refs.backlinkArms (refs.BacklinkKinds)
// so refs.BacklinkCounts answers a count of it where it refuses a kind with none, and changed_by_run
// and authored_by among the relation kinds the view's tuple arm answers (refs.TupleKinds).
func requireInteractionDeclared(t *testing.T, database *store.Store) {
	ctx := context.Background()
	if !slices.Contains(refs.VisibilityKinds(), graphstore.KindInteraction) || !refs.ResolvesNodeKind(graphstore.KindInteraction) {
		t.Errorf("refs.nodeRegistry holds no entry with a loader for %s: it holds %v", graphstore.KindInteraction, refs.VisibilityKinds())
	}
	if !slices.Contains(refs.BacklinkKinds(), graphstore.KindInteraction) {
		t.Errorf("refs.backlinkArms declares no arms for %s: it declares %v", graphstore.KindInteraction, refs.BacklinkKinds())
	}
	counted, err := refs.BacklinkCounts(ctx, database.Pool, model.Actor{}, graphstore.KindInteraction, []string{uuid.NewString()}, nil)
	t.Logf("a count of an interaction answers %v, %v", counted, err)
	if err != nil {
		t.Errorf("a count of an interaction is refused: %v", err)
	}
	// The refusal a kind with no arms meets, so the answer above is read as the count's own.
	const undeclared = "no_such_node_kind"
	if _, err := refs.BacklinkCounts(ctx, database.Pool, model.Actor{}, undeclared, []string{"x"}, nil); err == nil ||
		!strings.Contains(err.Error(), "no arms declared") {
		t.Errorf("a count of %s, which declares no arms, answers %v, want the refusal backlinkTargetArms makes", undeclared, err)
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
		excerpted, err := refs.ExcerptedNodes(ctx, pool, reader.actor, true, []refs.Target{target})
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

// installInteractionWriteProbes installs, in the test's own database (storetest.Open cloned it for
// this test), two triggers. interaction_write_logged records, after each row written to graph_nodes,
// graph_tuples, going_requests and going_events, the run the row names, what it is, the id of its
// transaction through txid_current() and the statement that wrote it. interaction_write_probe runs
// before and after each row inserted into going_requests and going_events and does what
// interaction_write_plan says for that table and moment: `fail` raises an error that names the run
// and what the transaction holds of it at that moment - the node, its authored_by, the frame value
// of the row's sequence and the row itself - and `hold` waits on the advisory lock
// interactionWriteHold, which a test holds to keep the write's transaction open.
func installInteractionWriteProbes(t *testing.T, database *store.Store) {
	t.Helper()
	for _, statement := range []string{
		`create table interaction_write_plan (tbl text not null, moment text not null, act text not null, primary key (tbl, moment))`,
		`create table interaction_write_statements (
			seq bigserial primary key, tbl text not null, run text, what text not null, xid bigint not null, statement text not null)`,
		`create function interaction_write_logged() returns trigger language plpgsql as $$
		begin
			if tg_table_name = 'graph_nodes' then
				insert into interaction_write_statements (tbl, run, what, xid, statement) values (tg_table_name,
					case when new.kind = 'interaction' then new.id end, new.kind || '/' || new.id, txid_current(), current_query());
			elsif tg_table_name = 'graph_tuples' then
				insert into interaction_write_statements (tbl, run, what, xid, statement) values (tg_table_name,
					case when new.from_kind = 'interaction' then new.from_id when new.to_kind = 'interaction' then new.to_id end,
					new.from_kind || '/' || new.from_id || ' ' || new.kind || ' ' || new.to_kind, txid_current(), current_query());
			elsif tg_table_name = 'going_requests' then
				insert into interaction_write_statements (tbl, run, what, xid, statement)
				values (tg_table_name, new.id::text, 'going_requests', txid_current(), current_query());
			else
				insert into interaction_write_statements (tbl, run, what, xid, statement)
				values (tg_table_name, new.request_id::text, 'going_events ' || new.sequence, txid_current(), current_query());
			end if;
			return null;
		end $$`,
		`create function interaction_write_probe() returns trigger language plpgsql as $$
		declare
			plan text;
			run text;
			frame_sequence integer;
			projected boolean;
		begin
			select act into plan from interaction_write_plan where tbl = tg_table_name and moment = lower(tg_when);
			if plan is null then
				return new;
			end if;
			if tg_table_name = 'going_requests' then
				run := new.id::text;
				projected := exists (select 1 from going_requests where id = new.id);
			else
				run := new.request_id::text;
				frame_sequence := new.sequence;
				projected := exists (select 1 from going_events where request_id = new.request_id and going_events.sequence = new.sequence);
			end if;
			if plan = 'hold' then
				perform pg_advisory_xact_lock(` + interactionWriteHold + `);
				return new;
			end if;
			raise exception 'the probe fails the write % its % row of run %: node=% authored_by=% frame=% row=%',
				lower(tg_when), tg_table_name, run,
				exists (select 1 from graph_nodes where kind = 'interaction' and id = run),
				exists (select 1 from graph_tuples where from_kind = 'interaction' and from_id = run and kind = 'authored_by' and asserted),
				exists (select 1 from graph_tuples where from_kind = 'interaction' and from_id = run and kind = 'frame'
					and to_kind = 'value' and (to_id::jsonb ->> 'sequence')::integer = frame_sequence),
				projected;
		end $$`,
		`create trigger interaction_write_logged after insert or update on graph_nodes for each row execute function interaction_write_logged()`,
		`create trigger interaction_write_logged after insert or update on graph_tuples for each row execute function interaction_write_logged()`,
		`create trigger interaction_write_logged after insert or update on going_requests for each row execute function interaction_write_logged()`,
		`create trigger interaction_write_logged after insert or update on going_events for each row execute function interaction_write_logged()`,
		`create trigger interaction_write_probe_before before insert on going_requests for each row execute function interaction_write_probe()`,
		`create trigger interaction_write_probe_after after insert on going_requests for each row execute function interaction_write_probe()`,
		`create trigger interaction_write_probe_before before insert on going_events for each row execute function interaction_write_probe()`,
		`create trigger interaction_write_probe_after after insert on going_events for each row execute function interaction_write_probe()`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("install the probes of a run's writes: %v\n%s", err, statement)
		}
	}
}

// interactionWriteHold is the advisory lock a `hold` of interaction_write_probe waits on.
const interactionWriteHold = "19500601"

// planInteractionWrite sets what interaction_write_probe does at the moment of the table's insert:
// `fail`, `hold`, or nothing for an empty act.
func planInteractionWrite(t *testing.T, database *store.Store, table, moment, act string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `delete from interaction_write_plan`); err != nil {
		t.Fatalf("clear the probe's plan: %v", err)
	}
	if act == "" {
		return
	}
	if _, err := database.Pool.Exec(ctx, `insert into interaction_write_plan (tbl, moment, act) values ($1, $2, $3)`, table, moment, act); err != nil {
		t.Fatalf("plan %s %s the %s insert: %v", act, moment, table, err)
	}
}

// probeFailure is what the probe's raised error names: the run, and whether the node, its
// authored_by, the frame value and the projected row stood in the transaction as it was failed.
var probeFailure = regexp.MustCompile(`the probe fails the write (before|after) its (going_requests|going_events) row of run ([0-9a-f-]{36}): ` +
	`node=(t|f) authored_by=(t|f) frame=(t|f) row=(t|f)`)

// runHeld is what a select on one connection finds of a run: its interaction node, its authored_by
// tuples, its frame values, its going_requests row and its going_events rows.
type runHeld struct {
	node                                bool
	authoredBy, frames, request, events int
}

func (held runHeld) empty() bool {
	return !held.node && held.authoredBy == 0 && held.frames == 0 && held.request == 0 && held.events == 0
}

// readRunHeld reads, in one select on q, what the store and the two tables hold of run.
func readRunHeld(t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, run string) runHeld {
	t.Helper()
	var held runHeld
	if err := q.QueryRow(context.Background(), `
		select exists (select 1 from graph_nodes where kind = $1 and id = $2),
		       (select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $3),
		       (select count(*) from graph_tuples where from_kind = $1 and from_id = $2 and kind = $4 and to_kind = $5),
		       (select count(*) from going_requests where id = $2::uuid),
		       (select count(*) from going_events where request_id = $2::uuid)
	`, graphstore.KindInteraction, run, graphstore.TupleAuthoredBy, graphstore.AttributeFrame, graphstore.KindValue).Scan(
		&held.node, &held.authoredBy, &held.frames, &held.request, &held.events); err != nil {
		t.Fatalf("read what the store holds of run %s: %v", run, err)
	}
	return held
}

// loggedStatement is one row interaction_write_logged recorded.
type loggedStatement struct {
	table, what, statement string
	xid                    int64
}

// named is the statement as requireOneTransaction names it: its table, then what it wrote.
func (logged loggedStatement) named() string { return logged.table + " " + logged.what }

// statementsOfRun is every row interaction_write_logged recorded of run after the log's seq after,
// in the order they were written, and the log's last seq.
func statementsOfRun(t *testing.T, database *store.Store, run string, after int64) ([]loggedStatement, int64) {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select tbl, what, statement, xid from interaction_write_statements where run = $1 and seq > $2 order by seq`, run, after)
	if err != nil {
		t.Fatalf("read the statements of run %s: %v", run, err)
	}
	statements, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (loggedStatement, error) {
		var logged loggedStatement
		return logged, row.Scan(&logged.table, &logged.what, &logged.statement, &logged.xid)
	})
	if err != nil {
		t.Fatalf("read the statements of run %s: %v", run, err)
	}
	var last int64
	if err := database.Pool.QueryRow(ctx, `select coalesce(max(seq), 0) from interaction_write_statements`).Scan(&last); err != nil {
		t.Fatalf("read the end of the statement log: %v", err)
	}
	return statements, last
}

// requireOneTransaction fails on each statement of written whose transaction id is not the one of
// the statement that wrote the run's node side, nodeWrite, and unless the statements hold each of
// want. Each statement is named `<table> <what>` (loggedStatement.named), and nodeWrite and want
// are prefixes of such a name.
func requireOneTransaction(t *testing.T, what string, written []loggedStatement, nodeWrite string, want ...string) {
	t.Helper()
	xid := int64(-1)
	for _, logged := range written {
		t.Logf("%s: %s in transaction %d: %s", what, logged.named(), logged.xid, strings.Join(strings.Fields(logged.statement), " "))
		if xid < 0 && strings.HasPrefix(logged.named(), nodeWrite) {
			xid = logged.xid
		}
	}
	if xid < 0 {
		t.Fatalf("%s: no statement wrote %s", what, nodeWrite)
	}
	for _, logged := range written {
		if logged.xid != xid {
			t.Errorf("%s: %s was written in transaction %d, and %s in transaction %d: one transaction writes them\n%s",
				what, logged.named(), logged.xid, nodeWrite, xid, logged.statement)
		}
	}
	for _, wanted := range want {
		if !slices.ContainsFunc(written, func(logged loggedStatement) bool { return strings.HasPrefix(logged.named(), wanted) }) {
			t.Errorf("%s: no statement of the transaction that wrote %s wrote %s", what, nodeWrite, wanted)
		}
	}
}

// failedByProbe runs write with the probe planned to fail at the moment of the table's insert, and
// answers the run its error names and what that error says the transaction held of it.
func failedByProbe(t *testing.T, database *store.Store, table, moment string, write func() error) (string, map[string]bool) {
	t.Helper()
	planInteractionWrite(t, database, table, moment, "fail")
	err := write()
	planInteractionWrite(t, database, table, moment, "")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("the write failed %s its %s row answers %v, want the probe's error", moment, table, err)
	}
	match := probeFailure.FindStringSubmatch(pgErr.Message)
	if match == nil || match[1] != moment || match[2] != table {
		t.Fatalf("the write failed %s its %s row answers %q, want the probe's error at that moment", moment, table, pgErr.Message)
	}
	t.Logf("the write failed %s its %s row: %s", moment, table, pgErr.Message)
	return match[3], map[string]bool{"node": match[4] == "t", "authored_by": match[5] == "t", "frame": match[6] == "t", "row": match[7] == "t"}
}

// heldOpen runs write with the probe planned to hold its transaction open after the table's insert,
// and, once it waits there, calls read; then it lets the write commit and answers its error.
func heldOpen(t *testing.T, database *store.Store, table string, write func() error, read func()) error {
	t.Helper()
	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the write open: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock(`+interactionWriteHold+`)`); err != nil {
		t.Fatalf("take the lock that holds the write open: %v", err)
	}
	planInteractionWrite(t, database, table, "after", "hold")
	done := make(chan error, 1)
	go func() { done <- write() }()
	if waiting := storetest.WaitForLockWaiters(t, holder, 1, 5*time.Second); waiting < 1 {
		t.Fatalf("the write never reached its %s row: no backend waits on the lock that holds it open", table)
	}
	read()
	planInteractionWrite(t, database, table, "after", "")
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("let the write commit: %v", err)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the write held open never committed")
		return nil
	}
}

// requireAProjectedRowIsWrittenInItsNodesTransaction proves the one transaction by failing it
// rather than by reading the source. With the probes installed it fails the record of a run before
// and after its going_requests row, and a frame before and after its going_events row, and finds
// neither side of the write afterwards, though the failure found the node, its authored_by and the
// frame value standing in the transaction before the row. It records a run and a frame and holds
// every statement naming the run to the transaction of the node write. It fails, then holds open, a
// frame of a run a pod of the release before recorded with no node, which writes the node first in
// the frame's transaction: a second connection reads neither the node nor the frame value while
// that transaction is open, and both in one select after it commits. It holds open a record too,
// which a second connection reads no part of until it commits.
func requireAProjectedRowIsWrittenInItsNodesTransaction(t *testing.T, server *server, database *store.Store) {
	ctx := context.Background()
	pool := database.Pool
	installInteractionWriteProbes(t, database)
	asker := model.Actor{Kind: "user", ID: "alice"}
	record := func(words string) (goingRequest, error) { return server.recordGoingRequest(ctx, asker, "TEST", words) }

	// The record, failed before its row and after it.
	for _, moment := range []string{"before", "after"} {
		run, stood := failedByProbe(t, database, "going_requests", moment, func() error {
			_, err := record("Which run did the probe fail " + moment + " its row?")
			return err
		})
		if !stood["node"] || !stood["authored_by"] || stood["row"] != (moment == "after") {
			t.Errorf("the record failed %s its row stood with %v: the node and its authored_by come before the row, in its transaction", moment, stood)
		}
		if held := readRunHeld(t, pool, run); !held.empty() {
			t.Errorf("the record of run %s failed %s its row left %+v, want neither side", run, moment, held)
		}
	}

	// A record and a frame, each one transaction.
	_, mark := statementsOfRun(t, database, "", 0)
	run, err := record("Which transaction wrote this run?")
	if err != nil {
		t.Fatalf("record a run: %v", err)
	}
	written, mark := statementsOfRun(t, database, run.ID, mark)
	requireOneTransaction(t, "the record of run "+run.ID, written, "graph_nodes "+graphstore.KindInteraction+"/"+run.ID,
		"going_requests", "graph_tuples "+graphstore.KindInteraction+"/"+run.ID+" "+graphstore.TupleAuthoredBy)
	for _, moment := range []string{"before", "after"} {
		failed, stood := failedByProbe(t, database, "going_events", moment, func() error {
			_, err := appendGoingEvent(ctx, pool, run.ID, going.Text("A frame the probe fails "+moment+" its row."))
			return err
		})
		if failed != run.ID || !stood["frame"] || stood["row"] != (moment == "after") {
			t.Errorf("the frame of run %s failed %s its row stood with %v in run %s: its value comes before the row, in its transaction",
				run.ID, moment, stood, failed)
		}
		if held := readRunHeld(t, pool, run.ID); held.frames != 0 || held.events != 0 {
			t.Errorf("the frame of run %s failed %s its row left %+v, want neither its value nor its row", run.ID, moment, held)
		}
	}
	if sequence, err := appendGoingEvent(ctx, pool, run.ID, going.Text("The frame one transaction writes.")); err != nil || sequence != 1 {
		t.Fatalf("append a frame of run %s: sequence %d, %v", run.ID, sequence, err)
	}
	frame, _ := statementsOfRun(t, database, run.ID, mark)
	requireOneTransaction(t, "the frame of run "+run.ID, frame, "graph_tuples "+graphstore.KindInteraction+"/"+run.ID+" "+graphstore.AttributeFrame,
		"going_events going_events 1")
	if len(written) > 0 && len(frame) > 0 && frame[0].xid == written[0].xid {
		t.Errorf("the frame of run %s was written in transaction %d, the run's own: each frame is a transaction of its own", run.ID, frame[0].xid)
	}

	// A run a pod of the release before recorded has no node: its frame writes the node first, in
	// the frame's one transaction.
	older := func() string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `
			insert into going_requests (author, project, words, ends_at)
			values ('{"kind":"user","id":"alice"}'::jsonb, 'TEST', 'What did the older pod record?', now() + make_interval(secs => $1))
			returning id::text
		`, contracts.GoingRunWindow.Seconds()).Scan(&id); err != nil {
			t.Fatalf("record a run as a pod of the release before: %v", err)
		}
		return id
	}
	failedOlder := older()
	if _, stood := failedByProbe(t, database, "going_events", "before", func() error {
		_, err := appendGoingEvent(ctx, pool, failedOlder, going.Text("A frame of the older pod's run."))
		return err
	}); !stood["node"] || !stood["frame"] {
		t.Errorf("the frame of the older pod's run %s failed before its row stood with %v: the node and the value come first", failedOlder, stood)
	}
	if held := readRunHeld(t, pool, failedOlder); held.node || held.frames != 0 || held.events != 0 {
		t.Errorf("the failed frame of the older pod's run %s left %+v, want no node, no value and no row", failedOlder, held)
	}
	heldOlder := older()
	_, mark = statementsOfRun(t, database, "", 0)
	var sequence int
	err = heldOpen(t, database, "going_events", func() error {
		var err error
		sequence, err = appendGoingEvent(ctx, pool, heldOlder, going.Text("A frame held open."))
		return err
	}, func() {
		second, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire a second connection: %v", err)
		}
		defer second.Release()
		held := readRunHeld(t, second, heldOlder)
		t.Logf("a second connection reads run %s while its frame's transaction is open: %+v", heldOlder, held)
		if held.node || held.frames != 0 || held.events != 0 {
			t.Errorf("a second connection reads %+v of run %s while its frame's transaction is open, want neither its node nor its frame value", held, heldOlder)
		}
	})
	if err != nil || sequence != 1 {
		t.Fatalf("append the held frame of run %s: sequence %d, %v", heldOlder, sequence, err)
	}
	after := readRunHeld(t, pool, heldOlder)
	t.Logf("one select reads run %s after its frame committed: %+v", heldOlder, after)
	if !after.node || after.frames != 1 || after.events != 1 {
		t.Errorf("one select reads %+v of run %s after its frame committed, want its node, its frame value and its row", after, heldOlder)
	}
	statements, _ := statementsOfRun(t, database, heldOlder, mark)
	requireOneTransaction(t, "the frame of the older pod's run "+heldOlder, statements, "graph_nodes "+graphstore.KindInteraction+"/"+heldOlder,
		"graph_tuples "+graphstore.KindInteraction+"/"+heldOlder+" "+graphstore.TupleAuthoredBy,
		"graph_tuples "+graphstore.KindInteraction+"/"+heldOlder+" "+graphstore.AttributeFrame, "going_events going_events 1")

	// A record held open: a second connection reads no run more than it read before.
	count := func(q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}) [3]int {
		t.Helper()
		var counts [3]int
		if err := q.QueryRow(ctx, `
			select (select count(*) from graph_nodes where kind = $1),
			       (select count(*) from graph_tuples where from_kind = $1 and kind = $2),
			       (select count(*) from going_requests)
		`, graphstore.KindInteraction, graphstore.TupleAuthoredBy).Scan(&counts[0], &counts[1], &counts[2]); err != nil {
			t.Fatalf("count the runs: %v", err)
		}
		return counts
	}
	before := count(pool)
	var recorded goingRequest
	err = heldOpen(t, database, "going_requests", func() error {
		var err error
		recorded, err = record("Which run is held open?")
		return err
	}, func() {
		second, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire a second connection: %v", err)
		}
		defer second.Release()
		during := count(second)
		t.Logf("a second connection counts interaction nodes, their authored_by and going_requests rows %v before the record and %v while it is open",
			before, during)
		if during != before {
			t.Errorf("a second connection counts %v while the record is open, and %v before it: want no part of the run", during, before)
		}
	})
	if err != nil {
		t.Fatalf("record the run held open: %v", err)
	}
	if held := readRunHeld(t, pool, recorded.ID); !held.node || held.authoredBy != 1 || held.request != 1 {
		t.Errorf("one select reads %+v of run %s after its record committed, want its node, its authored_by and its row", held, recorded.ID)
	}
}

// requireTwoFrameWritersQueueAtTheRunsNode appends two frames of one run from two goroutines at
// once: the first is held open after its row, and the second, started while it is, waits on the
// run's interaction row, which every frame's transaction locks first (graphstore.NodeRowLockSQL).
// Once the first commits the second takes the next sequence, so the two read 1 and 2, one after
// the other, with no gap, each held as a frame value.
func requireTwoFrameWritersQueueAtTheRunsNode(t *testing.T, server *server, database *store.Store) {
	ctx := context.Background()
	run, err := server.recordGoingRequest(ctx, model.Actor{Kind: "user", ID: "alice"}, "TEST", "Which writer takes which frame?")
	if err != nil {
		t.Fatalf("record the run two writers append to: %v", err)
	}
	var first, second int
	secondDone := make(chan error, 1)
	err = heldOpen(t, database, "going_events", func() error {
		var err error
		first, err = appendGoingEvent(ctx, database.Pool, run.ID, going.Text("The first writer's frame. "))
		return err
	}, func() {
		go func() {
			var err error
			second, err = appendGoingEvent(ctx, database.Pool, run.ID, going.Text("The second writer's frame."))
			secondDone <- err
		}()
		waitUntil(t, 5*time.Second, "the second writer waiting on the run's interaction row while the first held it", func() bool {
			waiting, err := storetest.CountLockWaits(ctx, database, graphstore.NodeRowLockSQL)
			if err != nil {
				t.Fatalf("count the writers waiting on the run's node: %v", err)
			}
			return waiting == 1
		})
		t.Logf("the second writer waits on the run's interaction row: %s", graphstore.NodeRowLockSQL)
	})
	if err != nil {
		t.Fatalf("append the first writer's frame: %v", err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("append the second writer's frame: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second writer never appended its frame")
	}
	t.Logf("the two writers took sequences %d and %d", first, second)
	if first != 1 || second != 2 {
		t.Fatalf("the two writers took sequences %d and %d, want 1 and then 2, with no gap", first, second)
	}
	requireFramesHeldAsValues(t, database, run.ID, 2)
}
