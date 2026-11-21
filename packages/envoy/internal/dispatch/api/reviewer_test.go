package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/modeltest"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The reviewer of INFRA-3231's R5 on the server: a stand-in model that flags `This names the
// problem.`, and the word `store` for a reader whose records hold a question about it, a fake
// listener that keeps every frame it is sent, and an issue assigned to alice, the person who reads
// what an agent writes on it.

const (
	heldWriter    = "s-writer"
	reviewDaemon  = "legion-daemon:RVW"
	reviewMarker  = "You review a text for its reader."
	vacuousPhrase = "This names the problem."
)

// noFindings is a review's reply that found nothing as strict mode writes it on openai-chat, the
// reviewer's protocol here: every property of the schema, the rewrite null.
const noFindings = `{"findings":[],"rewrite":null}`

// reviewFinding is the stand-in's finding of the vacuous phrase or of `store`.
func standInReview(t *testing.T, quote string, rewrite map[string]any) string {
	t.Helper()
	reply := map[string]any{
		"findings": []map[string]string{{"rule_set": "phrases", "quote": quote, "why": "It states no fact.", "fix": "Say what fails."}},
		"rewrite":  rewrite,
	}
	encoded, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// reviewedText is the text part of a review request's user message.
func reviewedText(request modeltest.Request) string {
	start := strings.Index(request.Text, "<text kind=")
	end := strings.Index(request.Text, "</text>")
	if start < 0 || end < start {
		return ""
	}
	return request.Text[start:end]
}

// reviewStandIn is the stand-in reviewer: findings for the vacuous phrase, findings for `store`
// where the reader's words hold it, and none for anything else. rewrite is the rewrite each
// finding comes with, by the text's kind.
func reviewStandIn(t *testing.T) *modeltest.Model {
	t.Helper()
	isReview := func(request modeltest.Request) bool { return strings.Contains(request.Text, reviewMarker) }
	return modeltest.New(t,
		modeltest.Rule{
			Match: func(request modeltest.Request) bool {
				text := reviewedText(request)
				return isReview(request) && strings.Contains(text, vacuousPhrase) &&
					(strings.Contains(text, `kind="ask"`) || strings.Contains(text, `kind="decision_block"`))
			},
			Reply: modeltest.Reply{Text: standInReview(t, vacuousPhrase, map[string]any{
				"body": "Ship the reviewer?\nThe test of the held ask fails on its first call.",
				"options": []map[string]string{
					{"label": "Ship it", "description": "Release it today, with the failing test."},
					{"label": "Do not file", "description": "Wait until the test passes."},
				},
				"multiple": false,
			})},
		},
		modeltest.Rule{
			Match: func(request modeltest.Request) bool {
				return isReview(request) && strings.Contains(reviewedText(request), vacuousPhrase) && strings.Contains(reviewedText(request), `kind="issue"`)
			},
			Reply: modeltest.Reply{Text: standInReview(t, vacuousPhrase, map[string]any{"title": "Fix the held issue", "body": "The held issue's test fails."})},
		},
		modeltest.Rule{
			Match: func(request modeltest.Request) bool {
				return isReview(request) && strings.Contains(reviewedText(request), vacuousPhrase)
			},
			Reply: modeltest.Reply{Text: standInReview(t, vacuousPhrase, map[string]any{"body": "Status\nThe test fails on its first call."})},
		},
		modeltest.Rule{
			Match: func(request modeltest.Request) bool {
				return isReview(request) && strings.Contains(reviewedText(request), "store") &&
					strings.Contains(request.Text, "<reader_words>\n- store\n</reader_words>")
			},
			Reply: modeltest.Reply{Text: standInReview(t, "the store", map[string]any{"body": "Rows wait in the table until the sweep."})},
		},
		modeltest.Rule{Match: isReview, Reply: modeltest.Reply{Text: noFindings}},
	)
}

// fakeReviewListener is the Envoy listener the reviewer's tests run against: each session in live
// is running, any other is gone, roles names the session each role's claim names, which a role
// lookup answers while that session runs, and every send is kept.
type fakeReviewListener struct {
	mu    sync.Mutex
	live  map[string]bool
	roles map[string]string
	sends []map[string]any
}

func newFakeReviewListener(t *testing.T, live ...string) (string, *fakeReviewListener) {
	t.Helper()
	listener := &fakeReviewListener{live: map[string]bool{}, roles: map[string]string{}}
	for _, session := range live {
		listener.live[session] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/registry/", func(w http.ResponseWriter, r *http.Request) {
		session := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
		listener.mu.Lock()
		running := listener.live[session]
		listener.mu.Unlock()
		if !running {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"session_id": session, "state": "live", "machine_id": "m1", "host": "m1",
			"capabilities": []string{"btw", "aside", "steer"}, "roles": []string{},
		})
	})
	mux.HandleFunc("/v1/messages/send", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		listener.mu.Lock()
		listener.sends = append(listener.sends, body)
		count := len(listener.sends)
		listener.mu.Unlock()
		WriteJSON(w, http.StatusOK, map[string]any{"event_id": "env-" + string(rune('a'+count)), "recipient": body["target_session"]})
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, _ *http.Request) { WriteJSON(w, http.StatusOK, []any{}) })
	mux.HandleFunc("/v1/roles/", func(w http.ResponseWriter, r *http.Request) {
		role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
		listener.mu.Lock()
		holder, claimed := listener.roles[role]
		running := listener.live[holder]
		listener.mu.Unlock()
		if !claimed || !running {
			WriteJSON(w, http.StatusNotFound, map[string]any{"error": "no holder for role " + role, "reason": "unclaimed"})
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"role": role, "holder": holder, "title": holder, "capabilities": []string{"btw", "aside", "steer"}, "holder_state": "live",
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL, listener
}

func (l *fakeReviewListener) stop(session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.live, session)
}

// start makes session one of the listener's running sessions.
func (l *fakeReviewListener) start(session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live[session] = true
}

// claim makes session the holder of role.
func (l *fakeReviewListener) claim(role, session string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roles[role] = session
}

// deliveredTo reports whether the listener was sent the message id for session: a send to it whose
// frame carries that id.
func (l *fakeReviewListener) deliveredTo(session, id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, send := range l.sends {
		payload, _ := send["payload"].(string)
		var frame struct {
			Event struct {
				Payload map[string]any `json:"payload"`
			} `json:"event"`
		}
		if send["target_session"] == session && json.Unmarshal([]byte(payload), &frame) == nil && frame.Event.Payload["id"] == id {
			return true
		}
	}
	return false
}

// frames are the findings frames the listener was sent: the delivered event of each send whose
// actor is Dispatch's reviewer.
func (l *fakeReviewListener) frames() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var frames []map[string]any
	for _, send := range l.sends {
		payload, _ := send["payload"].(string)
		var frame struct {
			Event struct {
				Type    string         `json:"type"`
				Actor   model.Actor    `json:"actor"`
				Payload map[string]any `json:"payload"`
			} `json:"event"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil || frame.Event.Actor != reviewerActor {
			continue
		}
		frames = append(frames, map[string]any{
			"target": send["target_session"], "type": frame.Event.Type, "id": frame.Event.Payload["id"], "body": frame.Event.Payload["body"],
		})
	}
	return frames
}

// reviewRig is a server with the reviewer on, its stand-in model, its listener and an issue alice
// holds, whose reader asked about `store` before.
type reviewRig struct {
	t        *testing.T
	handler  http.Handler
	database *store.Store
	served   *server
	stand    *modeltest.Model
	listener *fakeReviewListener
	issue    string
}

// reviewerModel is a reviewer's model group at the stand-in's url.
func reviewerModel(url string) reviewer.Model {
	return reviewer.Model{
		Model: agentloop.Model{Endpoint: agentloop.Endpoint{
			Kind: agentloop.OpenAIChat, BaseURL: url,
			Token: func(context.Context) (string, error) { return "stand-in", nil },
		}, ID: "reviewer", Effort: "off"},
		Timeout: 20 * time.Second,
	}
}

// testReviewer is NewReviewer of cfg with each field a test leaves zero set as cmd/dispatch sets
// its setting unset: every kind off, reviewer.DefaultTimeout, DefaultWriterWindow, UneditedRewrite
// and reviewer.DefaultRules, and the model's time bound the review's.
func testReviewer(cfg ReviewerConfig) *Reviewer {
	for _, choice := range []*string{&cfg.Asks, &cfg.Issues, &cfg.Comments, &cfg.Messages, &cfg.Documents, &cfg.DecisionBlocks} {
		*choice = cmp.Or(*choice, ReviewOff)
	}
	cfg.Timeout = cmp.Or(cfg.Timeout, reviewer.DefaultTimeout)
	cfg.WriterWindow = cmp.Or(cfg.WriterWindow, DefaultWriterWindow)
	cfg.Unedited = cmp.Or(cfg.Unedited, UneditedRewrite)
	if cfg.Rules == nil {
		cfg.Rules = reviewer.DefaultRules
	}
	cfg.Model.Timeout = cmp.Or(cfg.Model.Timeout, cfg.Timeout)
	return NewReviewer(cfg)
}

// newReviewRig builds the rig with cfg, its model pointed at stand (reviewStandIn where nil).
func newReviewRig(t *testing.T, cfg ReviewerConfig, stand *modeltest.Model) *reviewRig {
	t.Helper()
	if stand == nil {
		stand = reviewStandIn(t)
	}
	cfg.Model = reviewerModel(stand.URL)
	listenerURL, listener := newFakeReviewListener(t, heldWriter)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		reviewer: testReviewer(cfg), envoyURL: listenerURL, envoyTimeout: 2 * time.Second,
	})
	rig := &reviewRig{t: t, handler: handler, database: database, served: served, stand: stand, listener: listener}
	rig.issue = createInteractionIssue(t, handler, "RVW", "Reviewer issue", "Seed paragraph here.\n").Key
	// alice asked what a store is before: her records hold the word.
	if _, err := database.Pool.Exec(context.Background(), `
		insert into word_questions (source, text_key, event_id, person, written_at, day, verdict, words, state, model)
		values ('event', 'event:9999001', 9999001, 'alice', now() - interval '1 day', current_date - 1,
			'asks_word_meaning', '{store}', 'done', 'stand-in')`); err != nil {
		t.Fatalf("record alice's question about a store: %v", err)
	}
	return rig
}

// asWriter writes as the agent session that writes the rig's texts.
func (rig *reviewRig) asWriter(method, target string, body map[string]any) *httptest.ResponseRecorder {
	rig.t.Helper()
	return rig.asSession(heldWriter, method, target, body)
}

func (rig *reviewRig) asSession(session, method, target string, body map[string]any) *httptest.ResponseRecorder {
	rig.t.Helper()
	withActor := map[string]any{"actor": map[string]any{"kind": "session", "id": session}}
	for key, value := range body {
		withActor[key] = value
	}
	return agentRequest(rig.t, rig.handler, method, target, withActor, "agent-token")
}

// heldAnswer is the part of a held write's answer the tests read.
type heldWriteAnswer struct {
	ID   string     `json:"id"`
	Key  string     `json:"key"`
	Held *heldState `json:"held"`
}

func (rig *reviewRig) held(response *httptest.ResponseRecorder, what string) heldWriteAnswer {
	rig.t.Helper()
	if response.Code != http.StatusCreated && response.Code != http.StatusAccepted {
		rig.t.Fatalf("%s: %d %s, want it held", what, response.Code, response.Body.String())
	}
	answer := decodeBody[heldWriteAnswer](rig.t, response)
	if answer.Held == nil {
		rig.t.Fatalf("%s: %s carries no held object", what, response.Body.String())
	}
	if answer.ID == "" {
		answer.ID = answer.Key
	}
	return answer
}

// heldRow reads the held text id as the reviewer keeps it.
func (rig *reviewRig) heldRow(id string) heldRow {
	rig.t.Helper()
	row, err := rig.served.loadHeld(context.Background(), rig.database.Pool, id, false)
	if err != nil {
		rig.t.Fatalf("read held text %s: %v", id, err)
	}
	return row
}

// waitHeld waits for the held text id to reach state.
func (rig *reviewRig) waitHeld(id, state string) heldRow {
	rig.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		row := rig.heldRow(id)
		if row.state == state {
			return row
		}
		if time.Now().After(deadline) {
			rig.t.Fatalf("held text %s is %s (outcome %q, detail %q), want %s", id, row.state, row.outcome, row.detail, state)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// outcomes are the reviewer's outcomes recorded for the rig's writes, in order.
func (rig *reviewRig) outcomes() []string {
	rig.t.Helper()
	rows, err := rig.database.Pool.Query(context.Background(), `
		select outcome from writing_check_events
		where outcome in ('held', 'passed', 'edited', 'posted_as_written', 'rewrite_posted', 'posted_unreviewed', 'reviewed_after')
		order by id`)
	if err != nil {
		rig.t.Fatal(err)
	}
	defer rows.Close()
	var outcomes []string
	for rows.Next() {
		var outcome string
		if err := rows.Scan(&outcome); err != nil {
			rig.t.Fatal(err)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

// eventOf is the reviewer's row of outcome: its detail, reader and project.
func (rig *reviewRig) eventOf(outcome string) (detail, reader, project string) {
	rig.t.Helper()
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select coalesce(detail, ''), coalesce(reader, ''), coalesce(project, '')
		from writing_check_events where outcome = $1 order by id desc limit 1`, outcome).Scan(&detail, &reader, &project); err != nil {
		rig.t.Fatalf("read the %s row: %v", outcome, err)
	}
	return detail, reader, project
}

// commentIDs are the ids of the issue's comments as alice reads them.
func (rig *reviewRig) commentIDs() []string {
	rig.t.Helper()
	response := dispatchRequest(rig.t, rig.handler, http.MethodGet, "/api/v1/issues/"+rig.issue+"/comments", nil, "alice")
	if response.Code != http.StatusOK {
		rig.t.Fatalf("read the comments: %d %s", response.Code, response.Body.String())
	}
	var ids []string
	for _, comment := range decodeBody[[]model.Comment](rig.t, response) {
		ids = append(ids, comment.ID)
	}
	return ids
}

func (rig *reviewRig) commentBody(id string) string {
	rig.t.Helper()
	var body string
	if err := rig.database.Pool.QueryRow(context.Background(), `select body from comments where id = $1`, id).Scan(&body); err != nil {
		rig.t.Fatalf("read comment %s: %v", id, err)
	}
	return body
}

func holdComments() ReviewerConfig {
	return ReviewerConfig{Comments: ReviewHold, WriterWindow: time.Minute}
}

// An agent's comment on an issue a person holds, with the reviewer set to hold comments, answers
// at once with the comment's id and is held: the person's read of the issue's comments does not
// show it, the writer session gets the findings as a frame from Dispatch whose message is the held
// text, and the writer's post with a reason posts it, recorded posted_as_written with the reason.
func TestTheReviewerHoldsAnAgentsCommentUntilItsWriterPostsIt(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	written := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase,
	})
	answer := rig.held(written, "the agent's comment")
	if answer.ID == "" || answer.Held.State != heldReviewing {
		t.Fatalf("the held comment answered %s, want its id and held reviewing", written.Body.String())
	}
	row := rig.waitHeld(answer.ID, heldAwaitingWriter)
	if row.deadline == nil || len(row.findings) != 1 || row.findings[0].Quote != vacuousPhrase {
		t.Fatalf("the held comment's review: deadline %v findings %+v", row.deadline, row.findings)
	}
	if slices.Contains(rig.commentIDs(), answer.ID) {
		t.Fatal("alice reads the held comment before it posts")
	}
	frames := rig.listener.frames()
	if len(frames) != 1 || frames[0]["target"] != heldWriter || frames[0]["type"] != "message.created" || frames[0]["id"] != answer.ID {
		t.Fatalf("the writer's frames = %+v, want one message.created naming the held comment", frames)
	}
	if body, _ := frames[0]["body"].(string); !strings.Contains(body, "dispatch held-post "+answer.ID) || !strings.Contains(body, vacuousPhrase) {
		t.Fatalf("the frame's body = %q, want the findings and the post command", frames[0]["body"])
	}
	if count := countRows(t, rig.database, `select count(*) from messages where id::text = $1`, answer.ID); count != 0 {
		t.Fatalf("the frame stored %d message rows, want none", count)
	}
	// A person reads no held text, and another session posts none.
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/held/"+answer.ID, nil, "alice"); response.Code != http.StatusForbidden {
		t.Fatalf("alice's read of the held text: %d, want 403", response.Code)
	}
	if response := rig.asSession("s-other", http.MethodPut, "/api/v1/held/"+answer.ID+"/state", map[string]any{"state": "posted", "reason": "mine"}); response.Code != http.StatusForbidden {
		t.Fatalf("another session's post: %d %s, want 403", response.Code, response.Body.String())
	}
	if response := rig.asWriter(http.MethodPut, "/api/v1/held/"+answer.ID+"/state", map[string]any{"state": "posted", "reason": " "}); response.Code != http.StatusBadRequest {
		t.Fatalf("a post with no reason: %d, want 400", response.Code)
	}
	reason := "The reader asked for the status line as written."
	posted := rig.asWriter(http.MethodPut, "/api/v1/held/"+answer.ID+"/state", map[string]any{"state": "posted", "reason": reason})
	if posted.Code != http.StatusOK || decodeBody[heldRead](t, posted).State != heldPosted {
		t.Fatalf("the writer's post: %d %s", posted.Code, posted.Body.String())
	}
	// The same PUT again writes nothing and answers the held text as it stands; a PUT with another
	// reason is a second decision, which the first already made.
	again := rig.asWriter(http.MethodPut, "/api/v1/held/"+answer.ID+"/state", map[string]any{"state": "posted", "reason": reason})
	if again.Code != http.StatusOK || decodeBody[heldRead](t, again).State != heldPosted {
		t.Fatalf("the writer's post sent again: %d %s, want 200 and the posted text", again.Code, again.Body.String())
	}
	if other := rig.asWriter(http.MethodPut, "/api/v1/held/"+answer.ID+"/state", map[string]any{"state": "posted", "reason": "Another reason."}); other.Code != http.StatusConflict ||
		!strings.Contains(other.Body.String(), "HELD_TEXT_DECIDED") {
		t.Fatalf("a post with another reason once it posted: %d %s, want 409 HELD_TEXT_DECIDED", other.Code, other.Body.String())
	}
	if !slices.Contains(rig.commentIDs(), answer.ID) {
		t.Fatal("alice does not read the comment once its writer posted it")
	}
	if body := rig.commentBody(answer.ID); !strings.Contains(body, vacuousPhrase) {
		t.Fatalf("the posted comment reads %q, want it as written", body)
	}
	if detail, reader, project := rig.eventOf(reviewPostedAsWritten); detail != reason || reader != "alice" || project != "RVW" {
		t.Fatalf("the posted_as_written row: detail %q reader %q project %q", detail, reader, project)
	}
	if outcomes := rig.outcomes(); !slices.Equal(outcomes, []string{reviewHeld, reviewPostedAsWritten}) {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

// PUT /api/v1/held/{id}/state moves a held text to posted alone: any other state, or none, is 400
// HELD_STATE_INPUT naming posted, and the text stays held for its writer.
func TestTheHeldStateRouteRefusesAnotherState(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	answer := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase,
	}), "the agent's comment")
	rig.waitHeld(answer.ID, heldAwaitingWriter)
	for _, body := range []map[string]any{
		{"state": "reviewing", "reason": "As written."},
		{"state": "failed", "reason": "As written."},
		{"state": "Posted", "reason": "As written."},
		{"reason": "As written."},
	} {
		response := rig.asWriter(http.MethodPut, "/api/v1/held/"+answer.ID+"/state", body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"HELD_STATE_INPUT"`) ||
			!strings.Contains(response.Body.String(), "state must be "+heldStatePosted) {
			t.Errorf("a move to %v: %d %s, want 400 HELD_STATE_INPUT naming %s", body["state"], response.Code, response.Body.String(), heldStatePosted)
		}
	}
	rig.waitHeld(answer.ID, heldAwaitingWriter)
	if outcomes := rig.outcomes(); !slices.Equal(outcomes, []string{reviewHeld}) {
		t.Fatalf("outcomes = %v, want the hold alone", outcomes)
	}
}

// A second comment that uses `store` for a reader who asked about it is held, and its writer's
// edit through PATCH /api/v1/held/{id} is reviewed again and posts plainly, recorded passed.
func TestTheReviewerPostsAHeldCommentItsWriterEdited(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	answer := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Rows wait in the store until the sweep.",
	}), "the comment that uses store")
	row := rig.waitHeld(answer.ID, heldAwaitingWriter)
	if len(row.findings) != 1 || row.findings[0].Quote != "the store" {
		t.Fatalf("findings = %+v, want the store", row.findings)
	}
	edited := rig.asWriter(http.MethodPatch, "/api/v1/held/"+answer.ID, map[string]any{"body": "Rows wait in the table until the sweep."})
	if edited.Code != http.StatusOK {
		t.Fatalf("the writer's edit: %d %s", edited.Code, edited.Body.String())
	}
	if response := rig.asWriter(http.MethodPatch, "/api/v1/held/"+answer.ID, map[string]any{"title": "No"}); response.Code != http.StatusBadRequest && response.Code != http.StatusConflict {
		t.Fatalf("an edit of a field a comment does not hold: %d, want 400 (or 409 once posted)", response.Code)
	}
	posted := rig.waitHeld(answer.ID, heldPosted)
	if posted.outcome != reviewPassed {
		t.Fatalf("the edited comment posted %s, want passed", posted.outcome)
	}
	if body := rig.commentBody(answer.ID); body != "Rows wait in the table until the sweep." {
		t.Fatalf("the posted comment reads %q, want the edit", body)
	}
	if outcomes := rig.outcomes(); !slices.Equal(outcomes, []string{reviewHeld, reviewEdited, reviewPassed}) {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

// A held message on an issue a person holds is edited through the held text's own route as a
// message is: an edit of a field a message does not hold is refused 400 HELD_EDIT_INPUT, and the
// writer's edit of its body is reviewed again and posts, recorded passed. The reader reads nothing
// of the message while it is held, and then the edited text.
func TestTheReviewerPostsAHeldMessageItsWriterEdited(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	const edit = "Status\nThe test fails on its first call."
	answer := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/messages", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase,
	}), "the agent's message")
	rig.waitHeld(answer.ID, heldAwaitingWriter)
	if read := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/messages/"+answer.ID, nil, "alice"); read.Code != http.StatusNotFound {
		t.Fatalf("alice's read of the held message: %d %s, want 404", read.Code, read.Body.String())
	}
	if refused := rig.asWriter(http.MethodPatch, "/api/v1/held/"+answer.ID, map[string]any{"title": "No"}); refused.Code != http.StatusBadRequest ||
		!strings.Contains(refused.Body.String(), `"code":"HELD_EDIT_INPUT"`) {
		t.Fatalf("an edit of a field a message does not hold: %d %s, want 400 HELD_EDIT_INPUT", refused.Code, refused.Body.String())
	}
	edited := rig.asWriter(http.MethodPatch, "/api/v1/held/"+answer.ID, map[string]any{"body": edit})
	if edited.Code != http.StatusOK {
		t.Fatalf("the writer's edit: %d %s", edited.Code, edited.Body.String())
	}
	if posted := rig.waitHeld(answer.ID, heldPosted); posted.outcome != reviewPassed {
		t.Fatalf("the edited message posted %s (%s), want passed", posted.outcome, posted.detail)
	}
	read := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/messages/"+answer.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("alice's read of the posted message: %d %s", read.Code, read.Body.String())
	}
	if body := decodeBody[messageRead](t, read).Message.Body; body != edit {
		t.Fatalf("alice reads the posted message as %q, want the edit %q", body, edit)
	}
	if outcomes := rig.outcomes(); !slices.Equal(outcomes, []string{reviewHeld, reviewEdited, reviewPassed}) {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

// A held ask its writer leaves past the window posts the reviewer's rewrite, which keeps the
// option labels, their order and the first line.
func TestTheReviewerPostsTheRewriteOfAHeldAskLeftToTheWindow(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, WriterWindow: 300 * time.Millisecond, Unedited: UneditedRewrite}, nil)
	answer := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Ship the reviewer?\n" + vacuousPhrase,
		"options":  []map[string]string{{"label": "Ship it", "description": "Release it today."}, {"label": "Do not file", "description": "Wait."}},
	}), "the agent's ask")
	rig.waitHeld(answer.ID, heldAwaitingWriter)
	time.Sleep(400 * time.Millisecond)
	rig.served.sweepHeld(t.Context())
	posted := rig.waitHeld(answer.ID, heldPosted)
	if posted.outcome != reviewRewritePosted {
		t.Fatalf("the ask posted %s (%s), want rewrite_posted", posted.outcome, posted.detail)
	}
	var question string
	var options []model.AskOption
	if err := rig.database.Pool.QueryRow(context.Background(), `select question, options from asks where id = $1`, answer.ID).Scan(&question, &options); err != nil {
		t.Fatalf("read the posted ask: %v", err)
	}
	if first, _, _ := strings.Cut(question, "\n"); first != "Ship the reviewer?" || strings.Contains(question, vacuousPhrase) {
		t.Fatalf("the posted ask asks %q, want the rewrite with the first line kept", question)
	}
	if len(options) != 2 || options[0].Label != "Ship it" || options[1].Label != "Do not file" {
		t.Fatalf("the posted ask's options = %+v, want Ship it then Do not file", options)
	}
	if posted.original.Body != "Ship the reviewer?\n"+vacuousPhrase {
		t.Fatalf("the held text keeps %q as the original", posted.original.Body)
	}
}

// writtenOptions are the options the writer's ask of an option case has.
var writtenOptions = []map[string]string{{"label": "Ship it", "description": "Release it today."}, {"label": "Do not file", "description": "Wait."}}

// optionCase is the stand-in reviewer's rewrite of the options of the ask whose question holds
// its marker, and the reason the rewrite is not applied, "" for one that keeps them.
type optionCase struct {
	marker, notApplied string
	options            []map[string]string
}

// optionCases rewrite writtenOptions: one keeps them, and the others change a label, the order
// and the count. Each writes its options as the reply's schema has a model write them, with no id.
func optionCases() []optionCase {
	ship := map[string]string{"label": "Ship it", "description": "Release it today, with the failing test."}
	hold := map[string]string{"label": "Do not file", "description": "Wait until the test passes."}
	return []optionCase{
		{"case-kept", "", []map[string]string{ship, hold}},
		{"case-labels", `option 2 has the label "Hold" in the rewrite, and "Do not file" in the ask`,
			[]map[string]string{ship, {"label": "Hold", "description": "Wait until the test passes."}}},
		{"case-order", `the rewrite puts the options in the order "Do not file", "Ship it", and the ask has them in the order "Ship it", "Do not file"`,
			[]map[string]string{hold, ship}},
		{"case-count", "the rewrite has 1 option, and the ask has 2", []map[string]string{ship}},
	}
}

// optionCaseQuestion is the question of the ask of the case with marker.
func optionCaseQuestion(marker string) string {
	return "Ship the reviewer? " + marker + "\n" + vacuousPhrase
}

// optionCaseStandIn is the stand-in reviewer of cases: for the ask whose question holds a case's
// marker, it finds the vacuous phrase and rewrites the ask with the case's options, keeping the
// first line.
func optionCaseStandIn(t *testing.T, cases []optionCase) *modeltest.Model {
	t.Helper()
	isReview := func(request modeltest.Request) bool { return strings.Contains(request.Text, reviewMarker) }
	var rules []modeltest.Rule
	for _, c := range cases {
		marker := c.marker
		rules = append(rules, modeltest.Rule{
			Match: func(request modeltest.Request) bool {
				return isReview(request) && strings.Contains(reviewedText(request), marker)
			},
			Reply: modeltest.Reply{Text: standInReview(t, vacuousPhrase, map[string]any{
				"body": "Ship the reviewer? " + marker + "\nThe test of the held ask fails on its first call.", "options": c.options, "multiple": false,
			})},
		})
	}
	return modeltest.New(t, append(rules, modeltest.Rule{Match: isReview, Reply: modeltest.Reply{Text: noFindings}})...)
}

// A held ask whose reviewer's rewrite changes an option's label, the options' order or their
// count keeps the rewrite unapplied with its reason, and its writer leaving it past the window
// posts it as written, recorded posted_as_written with that reason.
func TestAHeldAsksRewriteThatChangesItsOptionsIsNotApplied(t *testing.T) {
	cases := optionCases()[1:]
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, WriterWindow: 300 * time.Millisecond, Unedited: UneditedRewrite}, optionCaseStandIn(t, cases))
	ids := make([]string, len(cases))
	for i, c := range cases {
		ids[i] = rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
			"question": optionCaseQuestion(c.marker), "options": writtenOptions,
		}), "the ask of "+c.marker).ID
	}
	for i, c := range cases {
		if held := rig.waitHeld(ids[i], heldAwaitingWriter); held.rewrite == nil || held.notApplied != c.notApplied {
			t.Fatalf("%s: the review gave rewrite %+v not applied %q, want not applied %q", c.marker, held.rewrite, held.notApplied, c.notApplied)
		}
	}
	time.Sleep(400 * time.Millisecond)
	rig.served.sweepHeld(t.Context())
	for i, c := range cases {
		posted := rig.waitHeld(ids[i], heldPosted)
		if posted.outcome != reviewPostedAsWritten || !strings.Contains(posted.detail, "the reviewer's rewrite is not applied: "+c.notApplied) {
			t.Fatalf("%s: the ask posted %s (%s), want posted_as_written with the reason", c.marker, posted.outcome, posted.detail)
		}
		var question string
		if err := rig.database.Pool.QueryRow(context.Background(), `select question from asks where id = $1`, ids[i]).Scan(&question); err != nil {
			t.Fatalf("%s: read the posted ask: %v", c.marker, err)
		}
		if question != optionCaseQuestion(c.marker) {
			t.Fatalf("%s: the posted ask asks %q, want its writer's question", c.marker, question)
		}
	}
}

// A writer session that has gone gets no time: the setting's choice posts at once.
func TestTheReviewerPostsAtOnceWhenTheWriterHasGone(t *testing.T) {
	for _, unedited := range []string{UneditedRewrite, UneditedAsWritten} {
		t.Run(unedited, func(t *testing.T) {
			rig := newReviewRig(t, ReviewerConfig{Comments: ReviewHold, WriterWindow: time.Hour, Unedited: unedited}, nil)
			rig.listener.stop(heldWriter)
			answer := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
				"body": "Status\nWe spin up the node. " + vacuousPhrase,
			}), "the gone writer's comment")
			posted := rig.waitHeld(answer.ID, heldPosted)
			want, body := reviewRewritePosted, "Status\nThe test fails on its first call."
			if unedited == UneditedAsWritten {
				want, body = reviewPostedAsWritten, "Status\nWe spin up the node. "+vacuousPhrase
			}
			if posted.outcome != want || rig.commentBody(answer.ID) != body {
				t.Fatalf("posted %s %q, want %s %q", posted.outcome, rig.commentBody(answer.ID), want, body)
			}
			if frames := rig.listener.frames(); len(frames) != 0 {
				t.Fatalf("a gone writer was sent %d frames", len(frames))
			}
		})
	}
}

// A model that fails posts the text as written, recorded posted_unreviewed.
func TestTheReviewerPostsAsWrittenWhenTheModelFails(t *testing.T) {
	failing := modeltest.New(t, modeltest.Rule{Reply: modeltest.Reply{Status: http.StatusInternalServerError, Text: "down"}})
	rig := newReviewRig(t, holdComments(), failing)
	answer := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Status\nWe spin up the node. " + vacuousPhrase,
	}), "the comment")
	posted := rig.waitHeld(answer.ID, heldPosted)
	if posted.outcome != reviewPostedUnreviewed || !strings.Contains(rig.commentBody(answer.ID), vacuousPhrase) {
		t.Fatalf("posted %s %q, want posted_unreviewed as written", posted.outcome, rig.commentBody(answer.ID))
	}
	if outcomes := rig.outcomes(); !slices.Equal(outcomes, []string{reviewHeld, reviewPostedUnreviewed}) {
		t.Fatalf("outcomes = %v", outcomes)
	}
}

// A second write naming a held issue as its parent succeeds at once and runs after the parent
// posts, as do a claim, an update and a document of the held issue, from a client that reads a
// kept write's answer (asKeptWriter).
func TestAWriteNamingAHeldIssueRunsAfterItPosts(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	parent := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Held parent", "spec": vacuousPhrase,
	}), "the held issue")
	if !strings.HasPrefix(parent.ID, "RVW-") {
		t.Fatalf("the held issue answered key %q, want one of RVW's", parent.ID)
	}
	rig.waitHeld(parent.ID, heldAwaitingWriter)
	child := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Held child", "parent": parent.ID,
	}), "the child")
	if child.Held.WaitsOn != parent.ID {
		t.Fatalf("the child waits on %q, want %s", child.Held.WaitsOn, parent.ID)
	}
	claim := rig.held(rig.asKeptWriter(http.MethodPost, "/api/v1/issues/"+parent.ID+"/claim", map[string]any{}), "the claim")
	update := rig.held(rig.asKeptWriter(http.MethodPatch, "/api/v1/issues/"+parent.ID, map[string]any{"labels": []string{"held"}}), "the update")
	document := rig.held(rig.asKeptWriter(http.MethodPost, "/api/v1/issues/"+parent.ID+"/artifacts", map[string]any{
		"name": "notes", "content": "The notes.\n",
	}), "the document")
	for _, waiting := range []heldWriteAnswer{claim, update, document} {
		if waiting.Held.WaitsOn != parent.ID {
			t.Fatalf("a write of the held issue waits on %q", waiting.Held.WaitsOn)
		}
	}
	if count := countRows(t, rig.database, `select count(*) from issues where key = $1 or parent_key = $1`, parent.ID); count != 0 {
		t.Fatalf("%d issues exist before the parent posts", count)
	}
	if response := rig.asWriter(http.MethodPut, "/api/v1/held/"+parent.ID+"/state", map[string]any{"state": "posted", "reason": "As written."}); response.Code != http.StatusOK {
		t.Fatalf("the parent's post: %d %s", response.Code, response.Body.String())
	}
	childPosted := rig.waitHeld(child.ID, heldPosted)
	rig.waitHeld(claim.ID, heldPosted)
	rig.waitHeld(update.ID, heldPosted)
	rig.waitHeld(document.ID, heldPosted)
	parentPosted := rig.heldRow(parent.ID)
	if childPosted.postedAt.Before(*parentPosted.postedAt) {
		t.Fatalf("the child posted at %v, before its parent at %v", childPosted.postedAt, parentPosted.postedAt)
	}
	var childParent string
	if err := rig.database.Pool.QueryRow(context.Background(), `select parent_key from issues where key = $1`, child.ID).Scan(&childParent); err != nil || childParent != parent.ID {
		t.Fatalf("the child's parent is %q (%v), want %s", childParent, err, parent.ID)
	}
	issue := decodeBody[model.Issue](t, dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+parent.ID, nil, "alice"))
	if issue.Claim == nil || issue.Claim.Actor.ID != heldWriter || !slices.Contains(issue.Labels, "held") {
		t.Fatalf("the posted issue: claim %+v labels %v, want claimed by the writer and labelled", issue.Claim, issue.Labels)
	}
	if count := countRows(t, rig.database, `select count(*) from artifacts where issue_key = $1 and slug = 'notes'`, parent.ID); count != 1 {
		t.Fatalf("the held issue's document: %d rows", count)
	}
}

// A held issue's create leaves its near-copy check's row to its post, which writes it under the
// issue's key: a row written at the hold would name an issue that may never be filed. The row keeps
// the title the gate read, though the writer's edit changed the issue's.
func TestAHeldIssuesPostRecordsItsNearCopyCheck(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	const title = "Held astrolabe drifts after a reseat"
	held := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": title, "spec": vacuousPhrase,
	}), "the held issue")
	rig.waitHeld(held.ID, heldAwaitingWriter)
	if count := countRows(t, rig.database, `select count(*) from near_copy_checks where title = $1 or issue_key = $2`, title, held.ID); count != 0 {
		t.Fatalf("%d near_copy_checks rows of the held issue before it posts, want none", count)
	}
	edited := rig.asWriter(http.MethodPatch, "/api/v1/held/"+held.ID, map[string]any{
		"title": "Held sextant drifts after a reseat", "spec": "The sextant reads two degrees high.",
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("the writer's edit: %d %s", edited.Code, edited.Body.String())
	}
	rig.waitHeld(held.ID, heldPosted)
	// The row is written after the post's commit, which marked the text posted.
	deadline := time.Now().Add(10 * time.Second)
	for countRows(t, rig.database, `select count(*) from near_copy_checks where issue_key = $1`, held.ID) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no near_copy_checks row names %s within 10s of its post", held.ID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if count := countRows(t, rig.database, `select count(*) from near_copy_checks where issue_key = $1`, held.ID); count != 1 {
		t.Fatalf("%d near_copy_checks rows name %s, want one", count, held.ID)
	}
	var outcome, recorded string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select outcome, title from near_copy_checks where issue_key = $1`, held.ID).Scan(&outcome, &recorded); err != nil {
		t.Fatalf("read the held issue's row: %v", err)
	}
	if outcome == nearCopyRefused || recorded != title {
		t.Fatalf("the held issue's row: outcome %s, title %q; want a create's outcome and the title the gate read, %q", outcome, recorded, title)
	}
}

// The near-copy answer a held issue stores comes back as the gate gave it: the row (built as the
// gate builds it, newNearCopyCheckRow, with the spec's hash and the candidates' encoding), the
// answer with its candidates, and whether the filing writes it in its transaction.
func TestAHeldIssueKeepsItsNearCopyAnswerWhole(t *testing.T) {
	check := &nearCopyCheck{
		outcome: nearCopyNotMade, reason: nearCopyNotMadeUnrecorded, force: true,
		candidates: []scoredDuplicate{{DuplicateCandidate: model.DuplicateCandidate{Key: "RVW-2", Title: "Close"}, Score: 0.91}},
	}
	want := issueCreate{Project: "RVW", Title: "Edited since", Labels: []string{}, NearCopy: &issueNearCopy{
		row: newNearCopyCheckRow(
			"6f1e2d3c-4b5a-4978-8a1b-2c3d4e5f6a7b", "RVW", model.Actor{Kind: "session", ID: heldWriter},
			"Read by the gate", "Its spec.", "RVW-1", check,
		),
		check: check,
		inTx:  true,
	}}
	stored, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("store the held issue: %v", err)
	}
	var got issueCreate
	if err := json.Unmarshal(stored, &got); err != nil {
		t.Fatalf("read the held issue back: %v", err)
	}
	if got.NearCopy == nil {
		t.Fatalf("the held issue read back with no near-copy answer: %s", stored)
	}
	if !reflect.DeepEqual(got.NearCopy, want.NearCopy) {
		t.Fatalf("near-copy answer read back = %+v (check %+v), want %+v (check %+v)", got.NearCopy, got.NearCopy.check, want.NearCopy, want.NearCopy.check)
	}
}

// adviceJSON is the advice object of a write's answer as it went on the wire.
func adviceJSON(t *testing.T, what string, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Advice json.RawMessage `json:"advice"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Advice) == 0 {
		t.Fatalf("%s carries no advice (%v): %s", what, err, response.Body.String())
	}
	return string(body.Advice)
}

// settleTermStats refreshes project's term statistics and waits for it, joining a refresh a check
// started in the background (ensureSearchTermStats) where one runs: with the statistics in place no
// later check starts another, so two checks made after it pick their words from the same ones.
func (rig *reviewRig) settleTermStats(project string) {
	rig.t.Helper()
	if result := <-rig.served.ensureSearchTermStats(project); result.Err != nil {
		rig.t.Fatalf("refresh the term statistics of %s: %v", project, result.Err)
	}
}

// requireSameAdviceAsTyped fails t unless held, a held write's answer, carries the advice that
// typed, the writer's own typed write of the same text, carries: that write posts at once, as with
// the reviewer off, and the held text, waiting on its writer, is in no search and no count.
func requireSameAdviceAsTyped(t *testing.T, what string, held, typed *httptest.ResponseRecorder) string {
	t.Helper()
	if typed.Code != http.StatusCreated || strings.Contains(typed.Body.String(), `"held"`) {
		t.Fatalf("the writer's typed %s: %d %s, want it posted at once", what, typed.Code, typed.Body.String())
	}
	heldAdvice, typedAdvice := adviceJSON(t, "the held "+what, held), adviceJSON(t, "the typed "+what, typed)
	if heldAdvice != typedAdvice {
		t.Fatalf("the held %s's advice differs from the posted one's:\nheld   %s\nposted %s", what, heldAdvice, typedAdvice)
	}
	return heldAdvice
}

// A held issue's answer carries the advice the same create gets with the reviewer off: the
// writer's write advice, the spec's blocks, the related items and what the near-copy gate found.
func TestAHeldIssuesAnswerCarriesTheAdviceOfAnUnheldOne(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	near := requireCreatedIssue(t, createIssueRequest(t, rig.handler, map[string]any{
		"project": "RVW", "title": "Astrolabe calibration drifts after a reseat", "spec": duplicateSpecAstrolabe,
	}))
	rig.settleTermStats("RVW")
	create := map[string]any{
		"project": "RVW", "title": "Astrolabe gnomon reseat offsets",
		"spec": "The astrolabe reads a constant offset after each gnomon reseat.\n\n" + vacuousPhrase,
	}
	response := rig.asWriter(http.MethodPost, "/api/v1/issues", create)
	held := rig.held(response, "the held issue")
	rig.waitHeld(held.ID, heldAwaitingWriter)
	advice := requireSameAdviceAsTyped(t, "issue", response, rig.asWriterTyped(http.MethodPost, "/api/v1/issues", create))
	var read struct {
		IssueStatus    string            `json:"issue_status"`
		DecisionBlocks *int              `json:"decision_blocks"`
		Suggestions    model.Suggestions `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(advice), &read); err != nil {
		t.Fatalf("decode the held issue's advice: %v %s", err, advice)
	}
	if read.IssueStatus == "" || read.DecisionBlocks == nil {
		t.Fatalf("the held issue's advice has no issue status or no spec blocks: %s", advice)
	}
	if read.Suggestions.Outcome != relatedFound || !slices.ContainsFunc(read.Suggestions.Related, func(item model.WriteSuggestion) bool {
		return item.Owner.Key == near.Key
	}) {
		t.Fatalf("the held issue's check did not offer %s: %s", near.Key, advice)
	}
}

// A held ask's answer carries the advice the same ask gets with the reviewer off: the ask of the
// issue a person declined, the writer's open asks and the related items.
func TestAHeldAsksAnswerCarriesItsAdvice(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	near := requireCreatedIssue(t, createIssueRequest(t, rig.handler, map[string]any{
		"project": "RVW", "title": "Billing exporter transport",
		"spec": "The billing exporter sends its batches over a transport we have not chosen.",
	}))
	first := rig.asWriterTyped(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Which transport should the billing exporter use?",
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("the writer's first ask: %d %s", first.Code, first.Body.String())
	}
	declineTheAsk(t, rig.handler, decodeBody[heldWriteAnswer](t, first).ID, "Pick what the team can run.")
	rig.settleTermStats("RVW")
	ask := map[string]any{
		"question": "Which transport should the billing exporter use for its batches?\n" + vacuousPhrase,
		"options":  writtenOptions,
	}
	response := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", ask)
	held := rig.held(response, "the held ask")
	rig.waitHeld(held.ID, heldAwaitingWriter)
	advice := requireSameAdviceAsTyped(t, "ask", response, rig.asWriterTyped(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", ask))
	var read struct {
		DeclinedAsks []adviceDeclinedAsk `json:"declined_asks"`
		Suggestions  model.Suggestions   `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(advice), &read); err != nil {
		t.Fatalf("decode the held ask's advice: %v %s", err, advice)
	}
	if len(read.DeclinedAsks) != 1 || read.DeclinedAsks[0].Note != "Pick what the team can run." {
		t.Fatalf("the held ask's advice names declined asks %+v, want the one declined: %s", read.DeclinedAsks, advice)
	}
	if read.Suggestions.Outcome != relatedFound || !slices.ContainsFunc(read.Suggestions.Related, func(item model.WriteSuggestion) bool {
		return item.Owner.Key == near.Key
	}) {
		t.Fatalf("the held ask's check did not offer %s: %s", near.Key, advice)
	}
}

// A held comment's answer carries the write advice the same comment gets with the reviewer off.
func TestAHeldCommentsAnswerCarriesItsAdvice(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	comment := map[string]any{"body": "Status\nThe exporter still drops its last batch. " + vacuousPhrase}
	response := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", comment)
	held := rig.held(response, "the held comment")
	rig.waitHeld(held.ID, heldAwaitingWriter)
	requireSameAdviceAsTyped(t, "comment", response, rig.asWriterTyped(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", comment))
}

// A near copy the gate refuses is refused at once under hold, as with the reviewer off: the write
// answers 409 POSSIBLE_DUPLICATE, nothing is held, and no number is taken.
func TestANearCopyIsRefusedAtOnceUnderHold(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	first := requireCreatedIssue(t, createIssueRequest(t, rig.handler, map[string]any{
		"project": "RVW", "title": "Astrolabe calibration drifts after a reseat", "spec": duplicateSpecAstrolabe,
	}))
	refusal := requireDuplicateRefusal(t, rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe calibration drifts after a reseat", "spec": duplicateSpecAstrolabe,
	}))
	if len(refusal.Candidates) != 1 || refusal.Candidates[0].Key != first.Key {
		t.Fatalf("the refusal names %+v, want %s", refusal.Candidates, first.Key)
	}
	if count := countRows(t, rig.database, `select count(*) from held_texts`); count != 0 {
		t.Fatalf("%d held texts after a refused near copy, want none", count)
	}
	next := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Orchard pickers cannot log a crate", "spec": duplicateSpecHarvest,
	}), "the next issue")
	if want := fmt.Sprintf("RVW-%d", first.Number+1); next.ID != want {
		t.Fatalf("the issue held after the refusal is %s, want %s: the refusal took a number", next.ID, want)
	}
}

// A held issue whose near-copy read and related-item check do not answer is told so, as an unheld
// one is: the gate's note that no near-copy check was made, and the check's that it searched
// nothing.
func TestAHeldIssuesAnswerSaysWhenItCouldNotSearch(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	rig.settleTermStats("RVW")
	// Both reads start from the project's term statistics, so a lock on that table holds them
	// past their bounds; the create is held all the same.
	rig.served.duplicateBound = 200 * time.Millisecond
	rig.served.suggestionTimeout = 200 * time.Millisecond
	lockTx, err := rig.database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the lock transaction: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), "lock table search_term_stats in access exclusive mode"); err != nil {
		t.Fatalf("lock search_term_stats: %v", err)
	}
	response := rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Astrolabe calibration drifts after a reseat", "spec": duplicateSpecAstrolabe,
	})
	rig.held(response, "the held issue")
	var read struct {
		Suggestions model.Suggestions `json:"suggestions"`
	}
	advice := adviceJSON(t, "the held issue", response)
	if err := json.Unmarshal([]byte(advice), &read); err != nil {
		t.Fatalf("decode the held issue's advice: %v %s", err, advice)
	}
	if read.Suggestions.NearCopyCheck == "" {
		t.Fatalf("the held issue is not told that no near-copy check was made: %s", advice)
	}
	if read.Suggestions.Outcome != relatedNotMade || read.Suggestions.Missing == "" {
		t.Fatalf("the held issue is not told that its related-item check was not made: %s", advice)
	}
}

// framesAbout are the frames the writer was sent about the held text id whose body holds marker.
func (rig *reviewRig) framesAbout(id, marker string) []string {
	var bodies []string
	for _, frame := range rig.listener.frames() {
		if body, _ := frame["body"].(string); frame["id"] == id && strings.Contains(body, marker) {
			bodies = append(bodies, body)
		}
	}
	return bodies
}

// waitOneFrameAbout waits for the frame about the held text id whose body holds marker, and
// answers its body once no second one has come.
func (rig *reviewRig) waitOneFrameAbout(id, marker string) string {
	rig.t.Helper()
	pollUntil(rig.t, 20*time.Second, "the frame about "+id, func() bool { return len(rig.framesAbout(id, marker)) > 0 })
	time.Sleep(200 * time.Millisecond)
	bodies := rig.framesAbout(id, marker)
	if len(bodies) != 1 {
		rig.t.Fatalf("%d frames about %s, want one: %q", len(bodies), id, bodies)
	}
	return bodies[0]
}

// closeRigIssue closes the rig's issue as alice.
func (rig *reviewRig) closeRigIssue() {
	rig.t.Helper()
	if closed := dispatchRequest(rig.t, rig.handler, http.MethodPatch, "/api/v1/issues/"+rig.issue, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		rig.t.Fatalf("close the issue: %d %s", closed.Code, closed.Body.String())
	}
}

// A held ask whose issue is closed before it posts is not posted, and its writer is told once,
// with the refusal; the comment held beside it fails with it, and its writer is told which held
// text it waited on. Each held text's detail records that its writer was told.
func TestAHeldTextNotPostedIsToldToItsWriter(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	ask := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Ship the reviewer?\n" + vacuousPhrase, "options": writtenOptions,
	}), "the held ask")
	rig.waitHeld(ask.ID, heldAwaitingWriter)
	comment := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "The exporter test passes now.", "ask_id": ask.ID,
	}), "the comment on the held ask")
	if comment.Held.WaitsOn != ask.ID {
		t.Fatalf("the comment waits on %q, want %s", comment.Held.WaitsOn, ask.ID)
	}
	rig.closeRigIssue()
	if posted := rig.asWriter(http.MethodPut, "/api/v1/held/"+ask.ID+"/state", map[string]any{"state": "posted", "reason": "As written."}); posted.Code != http.StatusOK {
		t.Fatalf("the writer's post: %d %s", posted.Code, posted.Body.String())
	}
	if failed := rig.waitHeld(ask.ID, heldFailed); !strings.Contains(failed.detail, "ISSUE_CLOSED") || !strings.HasSuffix(failed.detail, "; its writer was told") {
		t.Fatalf("the failed ask's detail reads %q", failed.detail)
	}
	body := rig.waitOneFrameAbout(ask.ID, "did not post")
	if !strings.Contains(body, "your held ask "+ask.ID) || !strings.Contains(body, "Its post was refused: ISSUE_CLOSED: issue is closed") {
		t.Fatalf("the failed ask's frame reads %q", body)
	}
	if failed := rig.waitHeld(comment.ID, heldFailed); !strings.Contains(failed.detail, ask.ID) || !strings.HasSuffix(failed.detail, "; its writer was told") {
		t.Fatalf("the failed comment's detail reads %q", failed.detail)
	}
	body = rig.waitOneFrameAbout(comment.ID, "did not post")
	if !strings.Contains(body, "your held comment "+comment.ID) || !strings.Contains(body, "It waited on the held text "+ask.ID) {
		t.Fatalf("the failed comment's frame reads %q", body)
	}
}

// A held text not posted whose writer session has gone sends no frame, and its detail says why.
func TestAHeldTextNotPostedTellsNoWriterThatHasGone(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Asks: ReviewHold, WriterWindow: time.Minute}, nil)
	ask := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/asks", map[string]any{
		"question": "Ship the reviewer?\n" + vacuousPhrase, "options": writtenOptions,
	}), "the held ask")
	rig.waitHeld(ask.ID, heldAwaitingWriter)
	rig.closeRigIssue()
	rig.listener.stop(heldWriter)
	// The sweep posts a text whose writer has gone, and the post is refused.
	rig.served.sweepHeld(t.Context())
	if failed := rig.waitHeld(ask.ID, heldFailed); !strings.HasSuffix(failed.detail, "; its writer was not told: its writer session has gone") {
		t.Fatalf("the failed ask's detail reads %q", failed.detail)
	}
	if bodies := rig.framesAbout(ask.ID, "did not post"); len(bodies) != 0 {
		t.Fatalf("a writer that has gone was sent %q", bodies)
	}
}

// A held issue a near copy of which is filed while it is held posts all the same, marked as forced
// past that copy, with its near_copy_checks row saying so, and its writer is told once, with the
// copy's key and title; a held issue with no near copy sends no such frame.
func TestAHeldIssueThatBecameANearCopyPostsAndTellsItsWriter(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Issues: ReviewHold, WriterWindow: time.Minute}, nil)
	const title = "Astrolabe calibration drifts after a reseat"
	spec := duplicateSpecAstrolabe + "\n\n" + vacuousPhrase
	held := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": title, "spec": spec,
	}), "the held issue")
	other := rig.held(rig.asWriter(http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "RVW", "title": "Orchard pickers cannot log a crate", "spec": duplicateSpecHarvest + "\n\n" + vacuousPhrase,
	}), "the other held issue")
	rig.waitHeld(held.ID, heldAwaitingWriter)
	rig.waitHeld(other.ID, heldAwaitingWriter)
	copied := requireCreatedIssue(t, createIssueRequest(t, rig.handler, map[string]any{"project": "RVW", "title": title, "spec": spec}))
	for _, id := range []string{held.ID, other.ID} {
		if posted := rig.asWriter(http.MethodPut, "/api/v1/held/"+id+"/state", map[string]any{"state": "posted", "reason": "As written."}); posted.Code != http.StatusOK {
			t.Fatalf("the writer's post of %s: %d %s", id, posted.Code, posted.Body.String())
		}
		rig.waitHeld(id, heldPosted)
	}
	if forced := createdEventPayload(t, rig.handler, held.ID).Forced; forced == nil || !slices.Equal(forced.Past, []string{copied.Key}) {
		t.Fatalf("%s's issue.created records forced %+v, want past %s", held.ID, forced, copied.Key)
	}
	var outcomes []string
	pollUntil(t, 10*time.Second, "the near_copy_checks row of "+held.ID, func() bool {
		outcomes = nearCopyOutcomesOf(t, rig.database, held.ID)
		return len(outcomes) > 0
	})
	if !slices.Equal(outcomes, []string{nearCopyForced}) {
		t.Fatalf("the near_copy_checks rows of %s = %v, want one, forced", held.ID, outcomes)
	}
	body := rig.waitOneFrameAbout(held.ID, "near copy")
	if !strings.Contains(body, copied.Key+": "+title) {
		t.Fatalf("the near-copy frame reads %q, want %s and its title", body, copied.Key)
	}
	if detail := rig.heldRow(held.ID).detail; !strings.HasSuffix(detail, "posted as a near copy of "+copied.Key+"; its writer was told") {
		t.Fatalf("%s's detail reads %q", held.ID, detail)
	}
	if bodies := rig.framesAbout(other.ID, "near copy"); len(bodies) != 0 {
		t.Fatalf("%s, no near copy, sent %q", other.ID, bodies)
	}
}

// The draft route answers the reviewer's findings, its rewrite and each call's deployment, and
// writes nothing.
func TestTheReviewerDraftRouteAnswersAndWritesNothing(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{}, nil)
	counts := func() int64 {
		return countRows(t, rig.database, `select (select count(*) from held_texts) + (select count(*) from writing_check_events) + (select count(*) from comments)`)
	}
	before := counts()
	response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/reviewer-drafts", map[string]any{
		"kind": "comment", "text": "Status\n" + vacuousPhrase, "issue": rig.issue,
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("the draft route: %d %s", response.Code, response.Body.String())
	}
	answer := decodeBody[reviewerDraftAnswer](t, response)
	if answer.Reader != "alice" || len(answer.Findings) != 1 || answer.Rewrite == nil || len(answer.Calls) != 1 ||
		answer.Calls[0].Deployment != "reviewer" || answer.Calls[0].Served == "" {
		t.Fatalf("the draft's answer = %+v", answer)
	}
	if after := counts(); after != before {
		t.Fatalf("the draft route wrote %d rows", after-before)
	}
	off := newRegisteredTestHandlerWithoutReviewer(t)
	if response := dispatchRequest(t, off, http.MethodPost, "/api/v1/reviewer-drafts", map[string]any{"kind": "comment", "text": "x"}, "alice"); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("the draft route with no reviewer's model: %d, want 503", response.Code)
	}
}

// A person at a browser learns nothing of another person's recorded words from a draft's review,
// as GET /api/v1/readers/{login}/words serves them none (Decision 14 of INFRA-3231's plan): bob's
// draft on alice's issue, and bob's draft naming alice as its reader, are reviewed for alice
// without the words she asked about, while alice's own draft and an agent token's are reviewed
// with them.
func TestTheDraftRouteShowsAPersonAtABrowserNoOtherPersonsWords(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{}, nil)
	const draft = "Status\nThe rows wait in the store."
	review := func(who string, response *httptest.ResponseRecorder) reviewerDraftAnswer {
		t.Helper()
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", who, response.Code, response.Body.String())
		}
		answer := decodeBody[reviewerDraftAnswer](t, response)
		if answer.Reader != "alice" {
			t.Fatalf("%s: reader %q, want alice", who, answer.Reader)
		}
		return answer
	}
	onIssue := map[string]any{"kind": "comment", "text": draft, "issue": rig.issue}
	for who, response := range map[string]*httptest.ResponseRecorder{
		"alice at a browser": dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/reviewer-drafts", onIssue, "alice"),
		"an agent's token":   agentRequest(t, rig.handler, http.MethodPost, "/api/v1/reviewer-drafts", onIssue, "agent-token"),
	} {
		if answer := review(who, response); len(answer.Findings) != 1 || answer.Findings[0].Quote != "the store" {
			t.Errorf("%s: findings %+v, want the one on alice's word store", who, answer.Findings)
		}
	}
	for what, body := range map[string]map[string]any{
		"on alice's issue": onIssue,
		"naming alice":     {"kind": "comment", "text": draft, "reader": "alice"},
	} {
		answer := review("bob's draft "+what, dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/reviewer-drafts", body, "bob"))
		if len(answer.Findings) != 0 {
			t.Errorf("bob's draft %s: findings %+v, want none: alice's words are not his to read", what, answer.Findings)
		}
	}
}

// The draft route answers each finding the review dropped because its quote is not in the text,
// with its rule set and its quote as the model wrote it.
func TestTheDraftRouteAnswersEachDroppedFindingWithItsQuote(t *testing.T) {
	const notInText = "This names … the problem."
	isReview := func(request modeltest.Request) bool { return strings.Contains(request.Text, reviewMarker) }
	reply, err := json.Marshal(map[string]any{
		"findings": []map[string]string{
			{"rule_set": "phrases", "quote": vacuousPhrase, "why": "It states no fact.", "fix": "Say what fails."},
			{"rule_set": "phrases", "quote": notInText, "why": "It states no fact.", "fix": "Say what fails."},
		},
		"rewrite": map[string]any{"body": "Status\nThe test fails on its first call."},
	})
	if err != nil {
		t.Fatal(err)
	}
	rig := newReviewRig(t, ReviewerConfig{}, modeltest.New(t, modeltest.Rule{Match: isReview, Reply: modeltest.Reply{Text: string(reply)}}))
	response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/reviewer-drafts", map[string]any{
		"kind": "comment", "text": "Status\n" + vacuousPhrase, "issue": rig.issue,
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("the draft route: %d %s", response.Code, response.Body.String())
	}
	answer := decodeBody[reviewerDraftAnswer](t, response)
	if len(answer.Findings) != 1 || answer.Findings[0].Quote != vacuousPhrase ||
		!slices.Equal(answer.Dropped, []droppedFinding{{RuleSet: "phrases", Quote: notInText}}) {
		t.Fatalf("the draft's findings %+v, dropped %+v; want the text's finding kept and the other dropped with its quote", answer.Findings, answer.Dropped)
	}
}

// The draft route weighs the reviewer's rewrite of an ask's options as every rewrite is weighed:
// one that keeps their labels, their number and their order would post, and one that changes a
// label, the order or the count would not, saying why. No option of a rewrite carries an id.
func TestTheDraftRouteWeighsTheRewriteOfAnAsksOptions(t *testing.T) {
	cases := optionCases()
	rig := newReviewRig(t, ReviewerConfig{}, optionCaseStandIn(t, cases))
	for _, c := range cases {
		t.Run(c.marker, func(t *testing.T) {
			response := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/reviewer-drafts", map[string]any{
				"kind": "ask", "text": optionCaseQuestion(c.marker), "options": writtenOptions, "issue": rig.issue,
			}, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("the draft route: %d %s", response.Code, response.Body.String())
			}
			answer := decodeBody[reviewerDraftAnswer](t, response)
			if answer.Rewrite == nil || len(answer.Rewrite.Options) != len(c.options) || answer.NotApplied != c.notApplied {
				t.Fatalf("the draft's rewrite = %+v, not applied %q; want %d options, not applied %q", answer.Rewrite, answer.NotApplied, len(c.options), c.notApplied)
			}
			for _, option := range answer.Rewrite.Options {
				if option.ID != "" {
					t.Errorf("the rewrite's option %q carries the id %q", option.Label, option.ID)
				}
			}
		})
	}
}

func newRegisteredTestHandlerWithoutReviewer(t *testing.T) http.Handler {
	t.Helper()
	handler, _, _ := newTestServer(t, testServerOptions{})
	return handler
}

// With the reviewer off, an agent's comment posts at once: the negative control.
func TestAnAgentsCommentPostsAtOnceWithTheReviewerOff(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Comments: ReviewOff, Asks: ReviewHold}, nil)
	response := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{"body": "Status\n" + vacuousPhrase})
	if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), `"held"`) {
		t.Fatalf("the comment with the reviewer off: %d %s, want posted", response.Code, response.Body.String())
	}
	if id := decodeBody[model.Comment](t, response).ID; !slices.Contains(rig.commentIDs(), id) {
		t.Fatal("alice does not read the comment the reviewer did not hold")
	}
	if count := countRows(t, rig.database, `select count(*) from held_texts`); count != 0 {
		t.Fatalf("%d held texts", count)
	}
}

// A person's own comment and the Legion daemon's post at once, with the reviewer holding comments.
func TestThePersonsAndTheDaemonsWritesPostAtOnce(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	for name, response := range map[string]*httptest.ResponseRecorder{
		"the person's": dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{"body": vacuousPhrase}, "alice"),
		"the daemon's": rig.asSession(reviewDaemon, http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{"body": vacuousPhrase}),
	} {
		if response.Code != http.StatusCreated || strings.Contains(response.Body.String(), `"held"`) {
			t.Errorf("%s comment: %d %s, want it posted at once", name, response.Code, response.Body.String())
			continue
		}
		if id := decodeBody[model.Comment](t, response).ID; !slices.Contains(rig.commentIDs(), id) {
			t.Errorf("alice does not read %s comment", name)
		}
	}
	if count := countRows(t, rig.database, `select count(*) from held_texts`); count != 0 {
		t.Fatalf("%d held texts", count)
	}
}

// A held text's write is checked as the write is checked with the reviewer off: a comment whose
// turn its create path refuses is refused at once, not held.
func TestAHeldWriteIsRefusedAtOnceForWhatItsPathRefuses(t *testing.T) {
	rig := newReviewRig(t, holdComments(), nil)
	response := rig.asWriter(http.MethodPost, "/api/v1/issues/"+rig.issue+"/comments", map[string]any{
		"body": "Status\n" + vacuousPhrase, "turn": "agent",
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "TURN_REQUIRES_ASK") {
		t.Fatalf("a held comment with a turn on no ask: %d %s, want 400 TURN_REQUIRES_ASK", response.Code, response.Body.String())
	}
	if count := countRows(t, rig.database, `select count(*) from held_texts`); count != 0 {
		t.Fatalf("%d held texts", count)
	}
}

// An agent's reply to a person's message that a plugin posts is held, and posts through the reply
// route under the id the hold reserved. One marked as generated posts at once
// (TestAGeneratedReplyPostsAtOnceUnderHold).
func TestTheReviewerHoldsAnAgentsReplyToAPersonsMessage(t *testing.T) {
	rig := newReviewRig(t, ReviewerConfig{Messages: ReviewHold, WriterWindow: time.Minute}, nil)
	send := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/agents/"+heldWriter+"/messages", map[string]any{"body": "Is it done?", "delivery": "btw"}, "alice")
	if send.Code != http.StatusCreated {
		t.Fatalf("alice's message: %d %s", send.Code, send.Body.String())
	}
	message := decodeBody[model.Message](t, send).ID
	reply := rig.held(rig.asWriter(http.MethodPost, "/api/v1/messages/"+message+"/reply", map[string]any{
		"attempt": 1, "body": "Status\n" + vacuousPhrase,
	}), "the agent's reply")
	rig.waitHeld(reply.ID, heldAwaitingWriter)
	if count := countRows(t, rig.database, `select count(*) from messages where in_reply_to = $1`, message); count != 0 {
		t.Fatalf("%d replies exist while the reply is held", count)
	}
	if response := rig.asWriter(http.MethodPut, "/api/v1/held/"+reply.ID+"/state", map[string]any{"state": "posted", "reason": "As written."}); response.Code != http.StatusOK {
		t.Fatalf("the writer's post: %d %s", response.Code, response.Body.String())
	}
	rig.waitHeld(reply.ID, heldPosted)
	var posted string
	if err := rig.database.Pool.QueryRow(context.Background(), `select id::text from messages where in_reply_to = $1`, message).Scan(&posted); err != nil || posted != reply.ID {
		t.Fatalf("the posted reply is %q (%v), want the reserved id %s", posted, err, reply.ID)
	}
}
