package graphcensus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/grapherase"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// This file is the package's external test because one site it drives lives in a package that
// imports graphcensus - grapherase, whose erase reads the census's ProvedSessionComments - so the
// package's own tests cannot import it, and the API's and the document service's drives run on the
// same rig ahead of the erase's, which takes a comment they wrote. It hands its drives to
// TestTheSizingCountsEveryTupleTheWritersAppend as the test binary starts.
func init() { graphcensus.ImportedDrives = importedDrives }

// writersServerURL is the public origin the writers' server serves under.
const writersServerURL = "https://dispatch.sizing.invalid"

// The sessions the stand-in listener knows, each live: the mention whose send it holds until the
// test lets it go, the mention whose send it refuses, and the message's target, whose send it
// takes at once.
const (
	heldSession    = "ses_mentioned"
	refusedSession = "ses_refused"
	messageSession = "ses_messaged"
)

// writersRig is the Dispatch API as cmd/dispatch builds it (api.NewDeps, api.Register) over a
// database of its own and the document service beside it (docs.New), with a stand-in Envoy
// listener, and the rows its drives leave for the drives after them. The document service settles
// nothing on a timer of its own (Settle is an hour): the one settlement a drive runs, it asks for
// (docs.Service.SettleNow).
type writersRig struct {
	database  *store.Store
	documents *docs.Service
	handler   http.Handler
	// arrived is signalled once the held mention's send reaches the listener, and release lets it
	// answer.
	arrived chan struct{}
	release func()
	// The issue and its spec, the issue's notes document and the slug an anchor names it by, the
	// comment on the issue the erase takes, the comment whose held send the settle settles, the
	// ask the follower follows, the targeted message the session's reply answers, and the
	// untouched document the removal removes.
	issue, spec, notes, notesSlug, comment, mentioned, ask, message, removable string
}

func newWritersRig(t *testing.T) *writersRig {
	t.Helper()
	ctx := context.Background()
	rig := &writersRig{arrived: make(chan struct{}, 1)}
	released := make(chan struct{})
	var once sync.Once
	rig.release = func() { once.Do(func() { close(released) }) }
	record := func(session string) map[string]any {
		return map[string]any{
			"session_id": session, "title": session, "capabilities": []string{"btw", "steer"},
			"state": envoy.StateLive, "last_seen": time.Now().UnixMilli(),
		}
	}
	known := []string{heldSession, refusedSession, messageSession}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			var records []map[string]any
			for _, session := range known {
				records = append(records, record(session))
			}
			_ = json.NewEncoder(w).Encode(records)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			session := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
			for _, live := range known {
				if live == session {
					_ = json.NewEncoder(w).Encode(record(session))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var send struct {
				TargetSession string `json:"target_session"`
			}
			if err := json.NewDecoder(r.Body).Decode(&send); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			switch send.TargetSession {
			case refusedSession:
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"the session's pane is gone"}`))
				return
			case heldSession:
				select {
				case rig.arrived <- struct{}{}:
				default:
				}
				select {
				case <-released:
				case <-r.Context().Done():
					return
				}
			}
			_, _ = fmt.Fprintf(w, `{"event_id":"envelope-%s","recipient":%q,"duplicate":false}`, send.TargetSession, send.TargetSession)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no live session"}`))
		}
	}))
	t.Cleanup(listener.Close)

	rig.database = storetest.Open(t)
	people := store.NewPgPeopleStore(rig.database.Pool, auth.SingleSigningKey("signing-key"), nil)
	if err := people.Record(ctx, "alice"); err != nil {
		t.Fatalf("record alice: %v", err)
	}
	// One identity for the API and the document service, so a person's browser that connects to a
	// document's room is admitted by the same header the API reads (emptyAskQuestion).
	caller := identity.HeaderIdentity{Header: "X-Dispatch-User", People: people}
	broker := events.NewBroker()
	rig.documents = docs.New(docs.Deps{Store: rig.database, Events: broker, Identity: caller, ServerURL: writersServerURL, Settle: time.Hour})
	t.Cleanup(func() {
		if err := rig.documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	tokens, err := auth.ParseSharedAgentTokens("agent-token")
	if err != nil {
		t.Fatalf("parse the agent token: %v", err)
	}
	deps, err := api.NewDeps(api.DepsInput{
		Store:       rig.database,
		Identity:    caller,
		AgentTokens: tokens,
		ServerURL:   writersServerURL,
		EnvoyURL:    listener.URL,
		// The held send waits on the test longer than the client's own window would let it.
		EnvoyTimeout: time.Minute,
		// A refused send settles failed once the take-over window has passed, which a test
		// need not wait the production twelve seconds for.
		ListenerTakeoverWindow: 100 * time.Millisecond,
		Docs:                   rig.documents,
		Events:                 broker,
	})
	if err != nil {
		t.Fatalf("build the API's dependencies: %v", err)
	}
	mux := http.NewServeMux()
	// t.Context, the server's lifetime, ends before the cleanups run, so Wait returns once the
	// deliveries and handlers it started have ended, before the database closes under them; the
	// held send is let go first, so none of them waits on the test.
	t.Cleanup(api.Register(mux, t.Context(), deps).Wait)
	t.Cleanup(rig.release)
	rig.handler = mux
	rig.request(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": "SIZE", "name": "Sizing"}, "alice", http.StatusCreated)
	return rig
}

// request sends one request to the server: as the person login names, or, with no login, under
// the shared agent bearer, whose body names the session it acts as. It fails the test unless the
// answer is want, and answers the body.
func (r *writersRig) request(t *testing.T, method, target string, body any, login string, want int) []byte {
	t.Helper()
	return r.requestForRun(t, "", method, target, body, login, want)
}

// requestForRun is request made for run, which it names in contracts.GoingRunHeader as a write
// made for that run sends it (api's GoingCaller.Do); with no run it names none.
func (r *writersRig) requestForRun(t *testing.T, run, method, target string, body any, login string, want int) []byte {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode %s %s: %v", method, target, err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	} else {
		request.Header.Set("Authorization", "Bearer agent-token")
	}
	if run != "" {
		request.Header.Set(contracts.GoingRunHeader, run)
	}
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s: status=%d body=%s, want %d", method, target, response.Code, response.Body.String(), want)
	}
	return response.Body.Bytes()
}

// askRun asks a question of the command bar as alice under the issue's project and answers the
// run's id once the run's one frame has landed: the rig holds no model, so the answer the POST
// starts stores the no-model error as the run's terminal frame, after the POST has answered. The
// run is live for contracts.GoingRunWindow from its POST, the window a write made for it has.
func (r *writersRig) askRun(t *testing.T, words string) string {
	t.Helper()
	run := decoded[struct {
		ID string `json:"id"`
	}](t, r.request(t, http.MethodPost, "/api/v1/going/requests", map[string]any{"words": words, "project": "SIZE"},
		"alice", http.StatusCreated)).ID
	r.awaitTrue(t, "run "+run+"'s answer storing its frame", `
		select exists (select 1 from going_events where request_id = $1::uuid)
	`, run)
	return run
}

// uploadBase is the token an upload replacing the document artifact names, the one it carries
// now: Dispatch refuses a replacement that names none (docs.ErrPreconditionRequired).
func (r *writersRig) uploadBase(t *testing.T, artifact string) map[string]string {
	t.Helper()
	document := decoded[struct {
		Token string `json:"token"`
	}](t, r.request(t, http.MethodGet, "/api/v1/artifacts/"+artifact+"/text", nil, "alice", http.StatusOK))
	if document.Token == "" {
		t.Fatalf("the document %s answers no token", artifact)
	}
	return map[string]string{"document": document.Token}
}

// decoded decodes body into a T, failing the test where it cannot.
func decoded[T any](t *testing.T, body []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return value
}

// sessionActor is a body's actor for the session id.
func sessionActor(id string) map[string]any { return map[string]any{"kind": "session", "id": id} }

// awaitTrue waits until query, a select of one boolean over args, answers true, and fails the test
// naming what once it has waited longer than any of the drives' work takes.
func (r *writersRig) awaitTrue(t *testing.T, what, query string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var done bool
		if err := r.database.Pool.QueryRow(context.Background(), query, args...).Scan(&done); err != nil {
			t.Fatalf("read whether %s: %v", what, err)
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within 30s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settledAttempts waits until no attempt of the comment is pending and one of them is in state.
func (r *writersRig) settledAttempts(t *testing.T, comment, state string) {
	t.Helper()
	r.awaitTrue(t, "comment "+comment+"'s attempt settling "+state, `
		select not exists (select 1 from comment_deliveries where comment_id = $1::uuid and state = 'pending')
		   and exists (select 1 from comment_deliveries where comment_id = $1::uuid and state = $2)
	`, comment, state)
}

// importedDrives are the drives of every site the register of the store's writers lists in api,
// docs, asks and grapherase, each the real site reached as its own lock-step driver reaches it:
// the API's through its HTTP routes, the settle of a delivery through the send the stand-in
// listener answers, the settlement and the block-index tick through the document service, and the
// erase through grapherase.Erase. They run in this order over one database, each needing what the
// ones before it wrote: an issue with its spec, an upload and an edit of a document, a comment on
// the issue and one anchored to the document mentioning a session whose send is held, that send's
// settle, a reply to a mention whose send was refused, a targeted message's delivery, an ask a
// session opens, a follow of it, the spec's approval request renewed on a retry and handed back, a
// settlement opening an ask a session wrote as a block, the tick indexing a version a build
// without the block index wrote, a run of the command bar alice asks, a run an older pod recorded
// that the boot pass ends, the issue's title, the question of an ask alice opens and a comment of
// alice's each written for a run she asked, a lens with a finding, a finding, a risk and a
// constraint, a pointer appended and one retargeted, the lens's state, and the erase of the
// issue's comment, which appends nothing. The comment path's drives name the sites the path runs
// in turn (comment): the path itself, which writes the comment's node and its tuples under its
// event, and who wrote it.
func importedDrives(t *testing.T) []graphcensus.StoreDrive {
	t.Helper()
	r := newWritersRig(t)
	ctx := context.Background()
	comment := func(name string, sites ...string) []string {
		return append([]string{name, "graphstore/people.go#QueueAuthored"}, sites...)
	}
	// What the drives of a run's writes and of the reasoning kinds leave for the drives after them:
	// the run a write is made for, the ask and the comment the run rewords, the trade-off lens on the
	// spec, the pre-mortem a risk stands under, the issue whose spec holds a requirement, and the
	// constraint under that issue.
	var run, reworded, edited, lens, premortem, requiring, constraint string
	nodeRoute := func(issue, address, route string) string {
		return "/api/v1/issues/" + issue + "/nodes/" + url.PathEscape(address) + "/" + route
	}
	type addressed struct {
		Address string `json:"address"`
	}
	return append([]graphcensus.StoreDrive{{
		Name: "api/issue_create.go#createIssue", Database: r.database,
		Sites: []string{"api/issue_create.go#createIssue", "refs/blocks.go#Queue"},
		Drive: func(t *testing.T) {
			created := decoded[struct {
				Key     string `json:"key"`
				Primary string `json:"primary_artifact_id"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "SIZE", "title": "What the writers write", "spec": "# Spec\n\nWhat the writers write.\n",
			}, "alice", http.StatusCreated))
			r.issue, r.spec = created.Key, created.Primary
		},
		Counts: merged(newBlockCounted, objectNodeCounted(graphstore.KindIssue), objectNodeCounted(graphstore.KindArtifact)),
	}, {
		Name: "api/artifacts.go#storeArtifact", Database: r.database,
		Sites: []string{"api/artifacts.go#storeArtifact", "api/artifacts.go#queueNewArtifact", "refs/blocks.go#Queue"},
		Drive: func(t *testing.T) {
			notes := decoded[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "notes.md", "content": "# Notes\n\nA paragraph the upload indexes.\n",
			}, "alice", http.StatusCreated)).Artifact
			r.notes, r.notesSlug = notes.ID, notes.Slug
		},
		Counts: merged(newBlockCounted, objectNodeCounted(graphstore.KindArtifact)),
	}, {
		Name: "api/artifacts.go#commitArtifactVersionEvent", Database: r.database,
		Sites: []string{"api/artifacts.go#commitArtifactVersionEvent", "refs/blocks.go#Queue"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+r.notes+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": "A paragraph the edit adds."}},
			}, "alice", http.StatusOK)
		},
		Counts: newBlockCounted,
	}, {
		Name: "api/comment_create.go#createCommentFor, on an issue", Database: r.database,
		Sites: comment("api/comment_create.go#createCommentFor"),
		Drive: func(t *testing.T) {
			r.comment = decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/comments",
				map[string]any{"body": "A remark on the issue."}, "alice", http.StatusCreated)).ID
		},
		// The comment, the first alice writes, and alice: her node is made by the write that first
		// records her.
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindComment, graphstore.AttributeVisibility}: {"comment"},
			{graphstore.KindComment, graphstore.AttributeState}:      {"comment"},
			{graphstore.KindComment, graphstore.AttributeProject}:    {"comment"},
			{graphstore.KindComment, graphstore.TupleAuthoredBy}:     {graphstore.TupleAuthoredBy},
			{graphstore.KindPerson, graphstore.AttributeVisibility}:  {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:       {graphstore.KindPerson},
		},
	}, {
		// A comment anchored to the notes, whose mention resolves to the held session: the
		// listener holds that send, so what the comment's create appends is read before its settle
		// runs.
		Name: "api/comment_create.go#createCommentFor, anchored to a document, mentioning a session", Database: r.database,
		Sites: comment("api/comment_create.go#createCommentFor"),
		Drive: func(t *testing.T) {
			r.mentioned = decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/comments", map[string]any{
				"body": "Does this ship, @" + heldSession + "?", "mentions": []map[string]any{{"target": "session:" + heldSession}},
				"anchor": map[string]any{"artifact": r.notesSlug, "quote": "the upload indexes"},
			}, "alice", http.StatusCreated)).ID
			select {
			case <-r.arrived:
			case <-time.After(30 * time.Second):
				t.Fatalf("the mention of %s sent nothing to the listener within 30s", heldSession)
			}
		},
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindComment, graphstore.AttributeVisibility}: {"comment"},
			{graphstore.KindComment, graphstore.AttributeState}:      {"comment"},
			{graphstore.KindComment, graphstore.AttributeProject}:    {"comment"},
			{graphstore.KindComment, graphstore.TupleAuthoredBy}:     {graphstore.TupleAuthoredBy},
			{graphstore.KindComment, graphstore.TupleAnchoredTo}:     {graphstore.TupleAnchoredTo},
		},
	}, {
		Name: "api/comment_delivery.go#completeCommentDelivery", Database: r.database,
		Sites: []string{"api/comment_delivery.go#completeCommentDelivery", "graphstore/people.go#QueueReached",
			"graphstore/people.go#QueueSessionRecorded"},
		Drive: func(t *testing.T) {
			r.release()
			r.settledAttempts(t, r.mentioned, model.DeliveryStateSent)
		},
		Counts: sessionNodeCounted,
	}, {
		// The reply answers an attempt the listener refused, which the answer flips to sent: a
		// delivery its session is owed a node for, and the one write that states it delivered.
		Name: "api/comment_delivery.go#replyComment", Database: r.database,
		Sites: comment("api/comment_delivery.go#replyComment",
			"api/comment_delivery.go#queueAnsweredDelivery", "graphstore/people.go#QueueSessionRecorded"),
		Ready: func(t *testing.T) {
			refused := decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/comments", map[string]any{
				"body": "Can you take this, @" + refusedSession + "?", "mentions": []map[string]any{{"target": "session:" + refusedSession}},
			}, "alice", http.StatusCreated)).ID
			r.settledAttempts(t, refused, "failed")
			r.mentioned = refused
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/comments/"+r.mentioned+"/reply", map[string]any{
				"actor": sessionActor(refusedSession), "attempt": 1, "body": "Yes, I take it.",
			}, "", http.StatusCreated)
		},
		// The reply, its author's node, the first write that records the session, and the
		// delivered_to of the attempt the reply makes a delivery, which the term delivery counts
		// once for each attempt the listener took or an answer flipped to sent.
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindComment, graphstore.AttributeVisibility}: {"comment"},
			{graphstore.KindComment, graphstore.AttributeState}:      {"comment"},
			{graphstore.KindComment, graphstore.AttributeProject}:    {"comment"},
			{graphstore.KindComment, graphstore.TupleAuthoredBy}:     {graphstore.TupleAuthoredBy},
			{graphstore.KindComment, graphstore.TupleDeliveredTo}:    {"delivery"},
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
		},
	}, {
		// A message the request stores on the issue, targeted at a session the listener takes the
		// send of at once: the message's node, in the issue's project, and the session's node the
		// settle of its attempt records.
		Name: "api/message_delivery.go#completeMessageDelivery", Database: r.database,
		Sites: []string{"api/message_delivery.go#completeMessageDelivery", "api/messages.go#createStoredMessage",
			"graphstore/people.go#QueueReached", "graphstore/people.go#QueueSessionRecorded"},
		Drive: func(t *testing.T) {
			r.message = decoded[model.Message](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/messages", map[string]any{
				"body": "Over to you.", "target": "session:" + messageSession, "delivery": "steer",
			}, "alice", http.StatusCreated)).ID
			r.awaitTrue(t, "message "+r.message+"'s attempt settling sent", `
				select not exists (select 1 from message_deliveries where message_id = $1::uuid and state = 'pending')
				   and exists (select 1 from message_deliveries where message_id = $1::uuid and state = 'sent')
			`, r.message)
		},
		Counts: merged(sessionNodeCounted, objectNodeCounted(graphstore.KindMessage)),
	}, {
		// The session's reply to that message, a message of the same issue's project.
		Name: "api/message_delivery.go#insertSessionReply", Database: r.database,
		Sites: []string{"api/message_delivery.go#insertSessionReply", "api/server.go#appendCausingEvent"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/messages/"+r.message+"/reply", map[string]any{
				"actor": sessionActor(messageSession), "attempt": 1, "body": "On it.",
			}, "", http.StatusCreated)
		},
		Counts: objectNodeCounted(graphstore.KindMessage),
	}, {
		// A broadcast to the message's session: the copy it stores for that session, a message on
		// no issue, between alice and the session, whose node names those parties.
		Name: "api/broadcasts.go#writeBroadcast", Database: r.database,
		Sites: []string{"api/broadcasts.go#writeBroadcast", "api/server.go#appendCausingEvent"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/broadcasts", map[string]any{
				"body": "To everyone.", "delivery": "btw", "session_ids": []string{messageSession},
			}, "alice", http.StatusCreated)
		},
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindMessage, graphstore.AttributeVisibility}: {graphstore.KindMessage},
			{graphstore.KindMessage, graphstore.AttributeState}:      {graphstore.KindMessage},
			{graphstore.KindMessage, graphstore.AttributeParties}:    {graphstore.KindMessage},
		},
	}, {
		// A session's ask on the issue: the ask's node, in the issue's project, who asked it, and
		// the asker's follow, planned in the create's read phase (asks.PlanFollowNewAsk).
		Name: "api/asks.go#createAskFor", Database: r.database,
		Sites: []string{"api/asks.go#createAskFor", "asks/followers.go#Queue", "asks/followers.go#queueFollow"},
		Drive: func(t *testing.T) {
			r.ask = decoded[model.Ask](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/asks", map[string]any{
				"question": "Which store do the writers take?", "actor": sessionActor("ses_asker"),
			}, "", http.StatusCreated)).ID
		},
		Counts: merged(sessionNodeCounted, objectNodeCounted(graphstore.KindAsk), askAuthorCounted, followCounted),
	}, {
		// The removal of an untouched document of the issue, made just before by typing a title,
		// with alice's trade-off lens on it: its node stated gone, which the term artifact_removed
		// counts, and the lens, stale once the row it recorded is deleted, stated gone through the
		// sweep's statement (refs.StateStaleLensesGone), whose state tuple the term lens counts.
		Name: "api/artifact_removal.go#removeArtifact", Database: r.database,
		Sites: []string{"api/artifact_removal.go#removeArtifact", "api/server.go#appendCausingEvent", "refs/stale_lenses.go#StateStaleLensesGone"},
		Ready: func(t *testing.T) {
			removable := decoded[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts",
				map[string]any{"name": "scratch.md"}, "alice", http.StatusCreated)).Artifact
			r.removable = removable.ID
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/lenses", map[string]any{
				"on": "dispatch://" + r.issue + "/artifact/" + removable.Slug, "kind": "trade-off",
			}, "alice", http.StatusCreated)
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodDelete, "/api/v1/artifacts/"+r.removable, nil, "alice", http.StatusNoContent)
		},
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindArtifact, graphstore.AttributeState}: {"artifact_removed"},
			{graphstore.KindLens, graphstore.AttributeState}:     {graphstore.KindLens},
		},
	}, {
		// The follower route's follow of that ask (asks.Follow), whose node it moves onto the
		// event it appends after it.
		Name: "api/server.go#appendCausingEvent", Database: r.database,
		Sites: []string{"api/server.go#appendCausingEvent", "asks/followers.go#Follow", "asks/followers.go#queueFollow"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPut, "/api/v1/asks/"+r.ask+"/followers/ses_follower",
				map[string]any{"actor": sessionActor("ses_follower")}, "", http.StatusNoContent)
		},
		Counts: merged(sessionNodeCounted, followCounted),
	}, {
		// The spec's approval request, which a session opens: the request's node, in the spec's
		// project, and the requester's follow.
		Name: "api/reviews.go#openApprovalAsk", Database: r.database,
		Sites: []string{"api/reviews.go#openApprovalAsk", "asks/followers.go#Queue", "asks/followers.go#queueFollow"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+r.spec+"/approval-requests",
				map[string]any{"actor": sessionActor("ses_requester")}, "", http.StatusCreated)
		},
		Counts: merged(sessionNodeCounted, objectNodeCounted(graphstore.KindAsk), askAuthorCounted, followCounted),
	}, {
		// That request renewed by another session while it waits on the person: a retry, which
		// appends no event.
		Name: "api/reviews.go#renewApprovalAsk, on a retry", Database: r.database,
		Sites: []string{"api/reviews.go#renewApprovalAsk", "asks/followers.go#Queue", "asks/followers.go#queueFollow"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+r.spec+"/approval-requests",
				map[string]any{"actor": sessionActor("ses_retry")}, "", http.StatusOK)
		},
		Counts: merged(sessionNodeCounted, followCounted),
	}, {
		// A new version of the spec moves the request to its agent, and a session asking for the
		// approval again hands it back to the person.
		Name: "api/reviews.go#renewApprovalAsk, handing the request back", Database: r.database,
		Sites: []string{"api/reviews.go#renewApprovalAsk", "asks/followers.go#Queue", "asks/followers.go#queueFollow"},
		Ready: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "spec.md", "content": "# Spec\n\nWhat the writers write, read once more.\n",
				"precondition": r.uploadBase(t, r.spec),
			}, "alice", http.StatusCreated)
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+r.spec+"/approval-requests",
				map[string]any{"actor": sessionActor("ses_handback")}, "", http.StatusCreated)
		},
		Counts: merged(sessionNodeCounted, followCounted),
	}, {
		// A session writes an ask as a block of the notes through the edits route, which writes
		// the version holding it, and the settlement opens the ask, in the notes and part of them,
		// and follows it for that session.
		Name: "docs/settlement.go#appendEvents", Database: r.database,
		Sites: []string{"docs/settlement.go#appendEvents", "docs/ask_blocks.go#createAskBlock", "asks/followers.go#Queue",
			"asks/followers.go#queueFollow"},
		Ready: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+r.notes+"/edits", map[string]any{
				"actor": sessionActor("ses_editor"),
				"ops":   []map[string]any{{"op": "insert", "after": "end", "markdown": ":::ask{#sizing-ask}\nWhich store do the writers take?\n:::"}},
			}, "", http.StatusOK)
		},
		Drive: func(t *testing.T) {
			r.documents.SettleNow(r.notes)
			r.awaitTrue(t, "the settlement opening the notes' ask block", `
				select exists (select 1 from asks where block_artifact_id = $1::uuid and block_id = 'sizing-ask')
			`, r.notes)
		},
		Counts: merged(sessionNodeCounted, objectNodeCounted(graphstore.KindAsk), askAuthorCounted, followCounted,
			map[graphcensus.StoredKind][]string{
				{graphstore.KindAsk, graphstore.TupleAnchoredTo}: {"ask_opened"},
				{graphstore.KindAsk, graphstore.TuplePartOf}:     {"ask_opened"},
			}),
	}, {
		// A person's live edit of the notes, which a build without the block index then wrote as a
		// version of its own: the version's insert owes the document its index (migration 0122's
		// trigger), and the tick's pass indexes it. A tick first runs through whatever the drives
		// before left owed, so the one that is read indexes this document alone.
		Name: "docs/block_index.go#indexOwedDocument", Database: r.database,
		Sites: []string{"docs/block_index.go#indexOwedDocument", "refs/blocks.go#Queue"},
		Ready: func(t *testing.T) {
			if _, err := r.documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil); err != nil {
				t.Fatalf("run the block-index tick over what the drives before left owed: %v", err)
			}
			tx, err := r.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the live edit: %v", err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := r.documents.Join(ctx, tx)
			defer ledger.Discard()
			if edit, err := r.documents.ApplyOps(joined, r.notes, []model.EditOp{{
				Op: "insert", After: "end", Markdown: "A paragraph a live edit adds.",
			}}, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil || !edit.Changed {
				t.Fatalf("edit the notes live (changed %v): %v", edit.Changed, err)
			}
			if err := ledger.Commit(ctx); err != nil {
				t.Fatalf("commit the live edit: %v", err)
			}
			live, _, err := r.documents.TextWithBlocks(ctx, r.notes)
			if err != nil {
				t.Fatalf("read the notes' live text: %v", err)
			}
			if _, err := r.database.Pool.Exec(ctx, `
				insert into artifact_versions (artifact_id, number, markdown, authors)
				select $1::uuid, max(number) + 1, $2, '[{"kind":"user","id":"alice"}]' from artifact_versions where artifact_id = $1::uuid
			`, r.notes, live); err != nil {
				t.Fatalf("write the live edit as a version a build without the block index writes: %v", err)
			}
		},
		Drive: func(t *testing.T) {
			tick, err := r.documents.ReconcileBlockIndex(ctx, docs.BlockIndexCursors{}, nil)
			if err != nil {
				t.Fatalf("run the block-index tick: %v", err)
			}
			if tick.Documents != 1 {
				t.Fatalf("the tick indexed %d document(s) (%d live ahead), want the notes alone", tick.Documents, tick.LiveAhead)
			}
		},
		Counts: newBlockCounted,
	}, {
		// A run of the command bar alice asks under the issue's project: its interaction node,
		// written before its going_requests row, and its one frame, the no-model error the answer
		// stores once the POST has answered.
		Name: "api/going.go#recordGoingRequest", Database: r.database,
		Sites:  []string{"api/going.go#recordGoingRequest", "api/going.go#appendGoingEvent"},
		Drive:  func(t *testing.T) { r.askRun(t, "What do the writers write?") },
		Counts: runNodeCounted,
	}, {
		// A run a process of the release before recorded with no node and left past its window
		// with no terminal frame: the boot pass ends it with the deadline's error, and that frame's
		// write gives the run its node from its row first (lockGoingRun, graphstore.WriteRun).
		Name: "api/going.go#lockGoingRun, a run an older pod recorded", Database: r.database,
		Sites: []string{"api/going.go#lockGoingRun", "graphstore/interaction.go#WriteRun", "api/going.go#appendGoingEvent"},
		Ready: func(t *testing.T) {
			if _, err := r.database.Pool.Exec(ctx, `
				insert into going_requests (author, project, words, created_at, ends_at)
				values ('{"kind":"user","id":"alice"}', 'SIZE', 'Who answered this?', now() - interval '1 minute', now() - interval '30 seconds')
			`); err != nil {
				t.Fatalf("record a run as an older pod does, with no node: %v", err)
			}
		},
		Drive: func(t *testing.T) {
			api.EndGoingRunsOverdueAtBoot(ctx, r.database.Pool)
			r.awaitTrue(t, "the boot pass ending the older pod's run", `
				select exists (select 1 from going_events e join going_requests q on q.id = e.request_id
				               where q.words = 'Who answered this?')
			`)
		},
		Counts: runNodeCounted,
	}, {
		// An issue's title written for a run alice asked: the issue's node named changed by it.
		Name: "api/issue_patch.go#patchIssue, made for a run", Database: r.database,
		Sites: []string{"api/issue_patch.go#patchIssue"},
		Ready: func(t *testing.T) { run = r.askRun(t, "Retitle the issue.") },
		Drive: func(t *testing.T) {
			r.requestForRun(t, run, http.MethodPatch, "/api/v1/issues/"+r.issue,
				map[string]any{"title": "What the writers write, run by run"}, "alice", http.StatusOK)
		},
		Counts: changedByRunCounted(graphstore.KindIssue),
	}, {
		// An ask alice opens on the issue, its question reworded for a run she asked: the ask's node
		// named changed by it. The ask is one of its own, so the ask ses_asker opened keeps the
		// question alice's answer of it confirms (askDrives).
		Name: "api/asks.go#editAsk, made for a run", Database: r.database,
		Sites: []string{"api/asks.go#editAsk"},
		Ready: func(t *testing.T) {
			reworded = decoded[model.Ask](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/asks",
				map[string]any{"question": "Which store does a run reword?"}, "alice", http.StatusCreated)).ID
			run = r.askRun(t, "Reword the ask.")
		},
		Drive: func(t *testing.T) {
			r.requestForRun(t, run, http.MethodPatch, "/api/v1/asks/"+reworded,
				map[string]any{"question": "Which store does a run reword, run by run?"}, "alice", http.StatusOK)
		},
		Counts: changedByRunCounted(graphstore.KindAsk),
	}, {
		// A comment of alice's reworded for a run she asked: the comment's node named changed by it.
		Name: "api/comment_actions.go#editComment, made for a run", Database: r.database,
		Sites: []string{"api/comment_actions.go#editComment"},
		Ready: func(t *testing.T) {
			edited = decoded[model.Comment](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/comments",
				map[string]any{"body": "A remark a run rewords."}, "alice", http.StatusCreated)).ID
			run = r.askRun(t, "Reword the remark.")
		},
		Drive: func(t *testing.T) {
			r.requestForRun(t, run, http.MethodPatch, "/api/v1/comments/"+edited,
				map[string]any{"body": "A remark a run reworded."}, "alice", http.StatusOK)
		},
		Counts: changedByRunCounted(graphstore.KindComment),
	}, {
		// A trade-off lens on the spec with its verdict and one finding, as alice invokes it.
		Name: "api/reasoning_write.go#writeReasoningNodes, a lens and its finding", Database: r.database,
		Sites: []string{"api/reasoning_write.go#writeReasoningNodes"},
		Drive: func(t *testing.T) {
			lens = decoded[addressed](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/lenses", map[string]any{
				"on": "dispatch://" + r.issue + "/spec", "kind": "trade-off", "verdict": "Keep one store.",
				"findings": []string{"One store halves the writes."},
			}, "alice", http.StatusCreated)).Address
		},
		Counts: merged(lensCounted, findingCounted),
	}, {
		// A finding added to that lens on its own.
		Name: "api/reasoning_write.go#writeReasoningNodes, a finding", Database: r.database,
		Sites: []string{"api/reasoning_write.go#writeReasoningNodes"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, nodeRoute(r.issue, lens, "findings"),
				map[string]any{"text": "Two stores double the reads."}, "alice", http.StatusCreated)
		},
		Counts: findingCounted,
	}, {
		// A risk under a pre-mortem lens of the spec.
		Name: "api/reasoning_write.go#writeReasoningNodes, a risk", Database: r.database,
		Sites: []string{"api/reasoning_write.go#writeReasoningNodes"},
		Ready: func(t *testing.T) {
			premortem = decoded[addressed](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/lenses", map[string]any{
				"on": "dispatch://" + r.issue + "/spec", "kind": graphstore.LensKindPreMortem,
			}, "alice", http.StatusCreated)).Address
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/risks", map[string]any{
				"lens": premortem, "heading": "The store outgrows its disk.", "early_warning": "The disk alarm fires.",
				"prevention": "Size the store before it ships.",
			}, "alice", http.StatusCreated)
		},
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindRisk, graphstore.AttributeVisibility}:   {graphstore.KindRisk},
			{graphstore.KindRisk, graphstore.AttributeState}:        {graphstore.KindRisk},
			{graphstore.KindRisk, graphstore.AttributeProject}:      {graphstore.KindRisk},
			{graphstore.KindRisk, graphstore.TuplePartOf}:           {graphstore.KindRisk},
			{graphstore.KindRisk, graphstore.AttributeHeading}:      {graphstore.KindRisk},
			{graphstore.KindRisk, graphstore.AttributeEarlyWarning}: {graphstore.KindRisk},
			{graphstore.KindRisk, graphstore.AttributePrevention}:   {graphstore.KindRisk},
			{graphstore.KindRisk, graphstore.TupleAuthoredBy}:       {graphstore.KindRisk},
		},
	}, {
		// A constraint under an issue whose spec holds a requirement, which the drive after this
		// one points at it.
		Name: "api/reasoning_write.go#writeReasoningNodes, a constraint", Database: r.database,
		Sites: []string{"api/reasoning_write.go#writeReasoningNodes"},
		Ready: func(t *testing.T) {
			requiring = decoded[struct {
				Key string `json:"key"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "SIZE", "title": "What forces the writers",
				"spec": "An opening paragraph.\n\n:::requirement{#r1}\nEvery write is idempotent.\n:::\n",
			}, "alice", http.StatusCreated)).Key
		},
		Drive: func(t *testing.T) {
			constraint = decoded[addressed](t, r.request(t, http.MethodPost, "/api/v1/issues/"+requiring+"/constraints",
				map[string]any{"standard": "RFC 9110", "reference": "section 9.2.2"}, "alice", http.StatusCreated)).Address
		},
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindConstraint, graphstore.AttributeVisibility}: {graphstore.KindConstraint},
			{graphstore.KindConstraint, graphstore.AttributeState}:      {graphstore.KindConstraint},
			{graphstore.KindConstraint, graphstore.AttributeProject}:    {graphstore.KindConstraint},
			{graphstore.KindConstraint, graphstore.TuplePartOf}:         {graphstore.KindConstraint},
			{graphstore.KindConstraint, graphstore.AttributeStandard}:   {graphstore.KindConstraint},
			{graphstore.KindConstraint, graphstore.AttributeReference}:  {graphstore.KindConstraint},
			{graphstore.KindConstraint, graphstore.TupleAuthoredBy}:     {graphstore.KindConstraint},
		},
	}, {
		// The requirement's constrained_by appended to that constraint.
		Name: "api/reasoning_write.go#writeReasoningPointer, an append", Database: r.database,
		Sites: []string{"api/reasoning_write.go#writeReasoningPointer"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, nodeRoute(requiring, refs.BlockRef("dispatch://"+requiring+"/spec", "r1"), "pointers"), map[string]any{
				"kind": graphstore.TupleConstrainedBy, "action": "append", "to": constraint,
			}, "alice", http.StatusOK)
		},
		Counts: map[graphcensus.StoredKind][]string{{refs.BlockNodeKind, graphstore.TupleConstrainedBy}: {graphstore.KindConstraint}},
	}, {
		// The trade-off lens's has_kind moved to another kind of the catalogue (graphstore.Retarget).
		Name: "api/reasoning_write.go#writeReasoningPointer, a retarget", Database: r.database,
		Sites: []string{"api/reasoning_write.go#writeReasoningPointer", "graphstore/reasoning.go#Retarget"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, nodeRoute(r.issue, lens, "pointers"), map[string]any{
				"kind": graphstore.TupleHasKind, "action": "retarget", "from": "trade-off", "to": "botec",
			}, "alice", http.StatusOK)
		},
		Counts: map[graphcensus.StoredKind][]string{{graphstore.KindLens, graphstore.TupleHasKind}: {graphstore.KindLens}},
	}, {
		// The trade-off lens taken to overtaken.
		Name: "api/reasoning_write.go#writeNodeState", Database: r.database,
		Sites: []string{"api/reasoning_write.go#writeNodeState"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, nodeRoute(r.issue, lens, "state"),
				map[string]any{"state": graphstore.StateOvertaken}, "alice", http.StatusOK)
		},
		Counts: map[graphcensus.StoredKind][]string{{graphstore.KindLens, graphstore.AttributeState}: {graphstore.KindLens}},
	}, {
		// The erase of the issue's comment, which deletes its tuples and appends none.
		Name: "grapherase/erase.go#apply", Database: r.database,
		Sites: []string{"grapherase/erase.go#apply"},
		Drive: func(t *testing.T) {
			url := r.database.Pool.Config().ConnString()
			listing, err := grapherase.Erase(ctx, url, "comment", r.comment, "", 10*time.Millisecond)
			if err != nil {
				t.Fatalf("list the erase of comment %s: %v", r.comment, err)
			}
			if len(listing.Blockers) > 0 {
				t.Fatalf("the erase of comment %s is blocked by %v", r.comment, listing.Blockers)
			}
			erased, err := grapherase.Erase(ctx, url, "comment", r.comment, listing.Digest, 10*time.Millisecond)
			if err != nil || !erased.Applied {
				t.Fatalf("erase comment %s (applied %v): %v", r.comment, erased != nil && erased.Applied, err)
			}
		},
		Counts: map[graphcensus.StoredKind][]string{},
	}}, askDrives(t, r)...)
}

// The terms that count what several drives here append: a block a version states afresh, with its
// two base attributes, its in_document and its part_of, which the term block counts; a session's
// node with its two base attributes, made by the first write that records the session, which the
// term session counts; a run's interaction node, with its base attributes, its words and ends_at
// values and its authored_by, which the term interaction counts, and the one frame value of the
// run's terminal frame, which the term frame counts; a lens of alice's with its verdict and a
// finding under it, which the terms lens and lens_finding count; who asked an ask, which the term
// ask_opened counts beside the ask's other facts its create states; and a session's follow of an
// ask, which the term ask_follower counts.
var (
	newBlockCounted = map[graphcensus.StoredKind][]string{
		{refs.BlockNodeKind, graphstore.AttributeVisibility}: {"block"},
		{refs.BlockNodeKind, graphstore.AttributeState}:      {"block"},
		{refs.BlockNodeKind, graphstore.AttributeProject}:    {"block"},
		{refs.BlockNodeKind, refs.TupleInDocument}:           {"block"},
		{refs.BlockNodeKind, graphstore.TuplePartOf}:         {"block"},
	}
	sessionNodeCounted = map[graphcensus.StoredKind][]string{
		{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
		{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
	}
	runNodeCounted = map[graphcensus.StoredKind][]string{
		{graphstore.KindInteraction, graphstore.AttributeVisibility}: {graphstore.KindInteraction},
		{graphstore.KindInteraction, graphstore.AttributeState}:      {graphstore.KindInteraction},
		{graphstore.KindInteraction, graphstore.AttributeProject}:    {graphstore.KindInteraction},
		{graphstore.KindInteraction, graphstore.AttributeWords}:      {graphstore.KindInteraction},
		{graphstore.KindInteraction, graphstore.AttributeEndsAt}:     {graphstore.KindInteraction},
		{graphstore.KindInteraction, graphstore.TupleAuthoredBy}:     {graphstore.KindInteraction},
		{graphstore.KindInteraction, graphstore.AttributeFrame}:      {graphstore.AttributeFrame},
	}
	lensCounted = map[graphcensus.StoredKind][]string{
		{graphstore.KindLens, graphstore.AttributeVisibility}: {graphstore.KindLens},
		{graphstore.KindLens, graphstore.AttributeState}:      {graphstore.KindLens},
		{graphstore.KindLens, graphstore.AttributeProject}:    {graphstore.KindLens},
		{graphstore.KindLens, graphstore.TuplePartOf}:         {graphstore.KindLens},
		{graphstore.KindLens, graphstore.TupleHasKind}:        {graphstore.KindLens},
		{graphstore.KindLens, graphstore.AttributeVerdict}:    {graphstore.KindLens},
		{graphstore.KindLens, graphstore.AttributeArtifactID}: {graphstore.KindLens},
		{graphstore.KindLens, graphstore.TupleAuthoredBy}:     {graphstore.KindLens},
	}
	findingCounted = map[graphcensus.StoredKind][]string{
		{refs.BlockNodeKind, graphstore.AttributeVisibility}: {"lens_finding"},
		{refs.BlockNodeKind, graphstore.AttributeState}:      {"lens_finding"},
		{refs.BlockNodeKind, graphstore.AttributeProject}:    {"lens_finding"},
		{refs.BlockNodeKind, graphstore.TuplePartOf}:         {"lens_finding"},
		{refs.BlockNodeKind, graphstore.AttributeText}:       {"lens_finding"},
		{refs.BlockNodeKind, graphstore.TupleAuthoredBy}:     {"lens_finding"},
	}
	askAuthorCounted = map[graphcensus.StoredKind][]string{
		{graphstore.KindAsk, graphstore.TupleAuthoredBy}: {"ask_opened"},
	}
	followCounted = map[graphcensus.StoredKind][]string{
		{graphstore.KindAsk, graphstore.TupleFollowedBy}: {"ask_follower"},
	}
	// What a person's answer of an ask states on its node, which the term answer counts, and what
	// a resolution states, which the term ask_resolved counts.
	answerCounted = map[graphcensus.StoredKind][]string{
		{graphstore.KindAsk, graphstore.TupleAnswerState}: {"answer"},
		{graphstore.KindAsk, graphstore.TupleAnswer}:      {"answer"},
		{graphstore.KindAsk, graphstore.TupleSelected}:    {"answer"},
		{graphstore.KindAsk, graphstore.TupleAnsweredAt}:  {"answer"},
		{graphstore.KindAsk, graphstore.TupleAnsweredBy}:  {"answer"},
		{graphstore.KindAsk, graphstore.TupleDecidedBy}:   {"answer"},
	}
	resolutionCounted = map[graphcensus.StoredKind][]string{
		{graphstore.KindAsk, graphstore.TupleAnswerState}: {"ask_resolved"},
		{graphstore.KindAsk, graphstore.TupleResolution}:  {"ask_resolved"},
	}
)

// changedByRunCounted is what a write made for a run appends of the node of kind it changed and
// moved nothing else of: the changed_by_run into the run's interaction, which the term interaction
// counts.
func changedByRunCounted(kind string) map[graphcensus.StoredKind][]string {
	return map[graphcensus.StoredKind][]string{{kind, graphstore.TupleChangedByRun}: {graphstore.KindInteraction}}
}

// objectNodeCounted is what a create of an issue, a document, an ask or a message of a project
// appends of the object's node: its visibility, its state and its project, which the term of its
// kind counts.
func objectNodeCounted(kind string) map[graphcensus.StoredKind][]string {
	return map[graphcensus.StoredKind][]string{
		{kind, graphstore.AttributeVisibility}: {kind},
		{kind, graphstore.AttributeState}:      {kind},
		{kind, graphstore.AttributeProject}:    {kind},
	}
}

// merged is every kind each of counts holds, its terms joined where two hold one kind.
func merged(counts ...map[graphcensus.StoredKind][]string) map[graphcensus.StoredKind][]string {
	joined := map[graphcensus.StoredKind][]string{}
	for _, count := range counts {
		for kind, terms := range count {
			for _, term := range terms {
				if !slices.Contains(joined[kind], term) {
					joined[kind] = append(joined[kind], term)
				}
			}
		}
	}
	return joined
}
