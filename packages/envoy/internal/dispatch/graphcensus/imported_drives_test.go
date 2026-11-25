package graphcensus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// This file is the package's external test: its drives reach the API and the document service,
// which no test of the package's own imports, through the routes and the calls cmd/dispatch
// serves them by. It hands its drives to TestTheSizingCountsEveryTupleTheWritersAppend as the
// test binary starts.
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
	// comment whose held send the settle settles, and the ask the follower follows.
	issue, spec, notes, notesSlug, mentioned, ask string
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
	broker := events.NewBroker()
	rig.documents = docs.New(docs.Deps{Store: rig.database, Events: broker, ServerURL: writersServerURL, Settle: time.Hour})
	t.Cleanup(func() {
		if err := rig.documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut down the document service: %v", err)
		}
	})
	people := store.NewPgPeopleStore(rig.database.Pool, auth.SingleSigningKey("signing-key"), nil)
	if err := people.Record(ctx, "alice"); err != nil {
		t.Fatalf("record alice: %v", err)
	}
	tokens, err := auth.ParseSharedAgentTokens("agent-token")
	if err != nil {
		t.Fatalf("parse the agent token: %v", err)
	}
	deps, err := api.NewDeps(api.DepsInput{
		Store:       rig.database,
		Identity:    identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
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
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s: status=%d body=%s, want %d", method, target, response.Code, response.Body.String(), want)
	}
	return response.Body.Bytes()
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
// docs and asks, each the real site reached as its own lock-step driver reaches it: the API's
// through its HTTP routes, the settle of a delivery through the send the stand-in listener
// answers, and the settlement and the block-index tick through the document service. They run in
// this order over one database, each needing what the ones before it wrote: an issue with its
// spec, an upload and an edit of a document, a comment on the issue and one anchored to the
// document mentioning a session whose send is held, that send's settle, a reply to a mention whose
// send was refused, a targeted message's delivery, an ask a session opens, a follow of it, the
// spec's approval request renewed on a retry and handed back, a settlement opening an ask a
// session wrote as a block, and the tick indexing a version a build without the block index
// wrote. The comment path's drives name the sites the path runs in turn (comment): the path
// itself, which writes the comment's node before its row (queueCommentRow) and its tuples under its
// event (queueCommentCreated), and who wrote it.
func importedDrives(t *testing.T) []graphcensus.StoreDrive {
	t.Helper()
	r := newWritersRig(t)
	ctx := context.Background()
	comment := func(name string, sites ...string) []string {
		return append([]string{name, "graphstore/people.go#QueueAuthored"}, sites...)
	}
	return []graphcensus.StoreDrive{{
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
		Counts: newBlockCounted,
	}, {
		Name: "api/artifacts.go#storeArtifact", Database: r.database,
		Sites: []string{"api/artifacts.go#storeArtifact", "refs/blocks.go#Queue"},
		Drive: func(t *testing.T) {
			notes := decoded[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{
				"name": "notes.md", "content": "# Notes\n\nA paragraph the upload indexes.\n",
			}, "alice", http.StatusCreated)).Artifact
			r.notes, r.notesSlug = notes.ID, notes.Slug
		},
		Counts: newBlockCounted,
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
		Sites: comment("api/comment_create.go#queueCommentRow", "api/comment_create.go#queueCommentCreated"),
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/comments",
				map[string]any{"body": "A remark on the issue."}, "alice", http.StatusCreated)
		},
		// The comment, the first alice writes, and alice: her node is made by the write that first
		// records her.
		Counts: map[graphcensus.StoredKind][]string{
			{graphstore.KindComment, graphstore.AttributeVisibility}: {"comment"},
			{graphstore.KindComment, graphstore.AttributeState}:      {"comment"},
			{graphstore.KindComment, graphstore.TupleAuthoredBy}:     {graphstore.TupleAuthoredBy},
			{graphstore.KindPerson, graphstore.AttributeVisibility}:  {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:       {graphstore.KindPerson},
		},
	}, {
		// A comment anchored to the notes, whose mention resolves to the held session: the
		// listener holds that send, so what the comment's create appends is read before its settle
		// runs.
		Name: "api/comment_create.go#createCommentFor, anchored to a document, mentioning a session", Database: r.database,
		Sites: comment("api/comment_create.go#queueCommentRow", "api/comment_create.go#queueCommentCreated"),
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
			{graphstore.KindComment, graphstore.TupleAuthoredBy}:     {graphstore.TupleAuthoredBy},
			{graphstore.KindComment, graphstore.TupleDeliveredTo}:    {"delivery"},
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
		},
	}, {
		Name: "api/message_delivery.go#completeMessageDelivery", Database: r.database,
		Sites: []string{"api/message_delivery.go#completeMessageDelivery", "graphstore/people.go#QueueReached",
			"graphstore/people.go#QueueSessionRecorded"},
		Drive: func(t *testing.T) {
			message := decoded[model.Message](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/messages", map[string]any{
				"body": "Over to you.", "target": "session:" + messageSession, "delivery": "steer",
			}, "alice", http.StatusCreated)).ID
			r.awaitTrue(t, "message "+message+"'s attempt settling sent", `
				select not exists (select 1 from message_deliveries where message_id = $1::uuid and state = 'pending')
				   and exists (select 1 from message_deliveries where message_id = $1::uuid and state = 'sent')
			`, message)
		},
		Counts: sessionNodeCounted,
	}, {
		Name: "api/asks.go#createAskFor", Database: r.database,
		Sites: []string{"api/asks.go#createAskFor", "asks/followers.go#queueFollow"},
		Drive: func(t *testing.T) {
			r.ask = decoded[model.Ask](t, r.request(t, http.MethodPost, "/api/v1/issues/"+r.issue+"/asks", map[string]any{
				"question": "Which store do the writers take?", "actor": sessionActor("ses_asker"),
			}, "", http.StatusCreated)).ID
		},
		Counts: sessionNodeCounted,
	}, {
		Name: "api/server.go#appendCausingEvent", Database: r.database,
		Sites: []string{"api/server.go#appendCausingEvent", "asks/followers.go#queueFollow"},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPut, "/api/v1/asks/"+r.ask+"/followers/ses_follower",
				map[string]any{"actor": sessionActor("ses_follower")}, "", http.StatusNoContent)
		},
		Counts: sessionNodeCounted,
	}, {
		// The spec's approval request, which a session opens, renewed by another while it waits on
		// the person: a retry, which appends no event.
		Name: "api/reviews.go#renewApprovalAsk, on a retry", Database: r.database,
		Sites: []string{"api/reviews.go#renewApprovalAsk", "asks/followers.go#queueFollow"},
		Ready: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+r.spec+"/approval-requests",
				map[string]any{"actor": sessionActor("ses_requester")}, "", http.StatusCreated)
		},
		Drive: func(t *testing.T) {
			r.request(t, http.MethodPost, "/api/v1/artifacts/"+r.spec+"/approval-requests",
				map[string]any{"actor": sessionActor("ses_retry")}, "", http.StatusOK)
		},
		Counts: sessionNodeCounted,
	}, {
		// A new version of the spec moves the request to its agent, and a session asking for the
		// approval again hands it back to the person.
		Name: "api/reviews.go#renewApprovalAsk, handing the request back", Database: r.database,
		Sites: []string{"api/reviews.go#renewApprovalAsk", "asks/followers.go#queueFollow"},
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
		Counts: sessionNodeCounted,
	}, {
		// A session writes an ask as a block of the notes through the edits route, which writes
		// the version holding it, and the settlement opens the ask and follows it for that session.
		Name: "docs/settlement.go#appendEvents", Database: r.database,
		Sites: []string{"docs/settlement.go#appendEvents", "asks/followers.go#queueFollow"},
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
		Counts: sessionNodeCounted,
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
	}}
}

// The terms that count what several drives here append: a block a version states afresh, with its
// two base attributes, its in_document and its part_of, which the term block counts; and a
// session's node with its two base attributes, made by the first write that records the session,
// which the term session counts.
var (
	newBlockCounted = map[graphcensus.StoredKind][]string{
		{refs.BlockNodeKind, graphstore.AttributeVisibility}: {"block"},
		{refs.BlockNodeKind, graphstore.AttributeState}:      {"block"},
		{refs.BlockNodeKind, refs.TupleInDocument}:           {"block"},
		{refs.BlockNodeKind, graphstore.TuplePartOf}:         {"block"},
	}
	sessionNodeCounted = map[graphcensus.StoredKind][]string{
		{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
		{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
	}
)
