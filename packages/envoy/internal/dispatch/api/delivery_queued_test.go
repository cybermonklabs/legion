package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// INFRA-626: a session the Envoy listener knows and that is not running is a recipient, not a
// refusal. The listener stores the message for it and hands it over when it runs again, and
// Dispatch records that attempt `queued`. A session the listener knows nothing about is refused
// with "no live session <id>".

// fakeSession is one session the queueing listener knows. stopped is a session the listener knows
// by its known-session record alone, with no registry entry: GET /v1/registry/<id> answers it 200
// with state "stopped" only when the include list names stopped, and 404 otherwise, as the real
// route answers. crashed is a session whose registry entry still stands while its plugin has gone:
// the registry reads it live, and the send's probe finds nobody, so the send answers it stopped.
type fakeSession struct {
	title        string
	capabilities []string
	stopped      bool
	crashed      bool
}

// queueingListener is a fake Envoy listener that knows some sessions as running and some as
// stopped, answers the registry's include list as the real one does, and records every registry
// read and every send so a test can say what Dispatch asked for.
type queueingListener struct {
	url      string
	sessions map[string]fakeSession
	mu       sync.Mutex
	sends    []map[string]any
	// refused is every send it answered 409 "the session is stopped": each one carried
	// connected_only and met a stopped target, and nothing of it was stored, so it is not in sends.
	refused []map[string]any
	// stoppedReads counts the registry reads that asked for stopped sessions too.
	stoppedReads int
	// registryOnlyReads counts the registry reads that did not.
	registryOnlyReads int
}

const stoppedLastSeen = 1762000000000

func newQueueingListener(t *testing.T, sessions map[string]fakeSession) *queueingListener {
	t.Helper()
	listener := &queueingListener{sessions: sessions}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			listener.writeSessions(w)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			listener.writeRegistry(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			listener.writeSend(t, w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(server.Close)
	listener.url = server.URL
	return listener
}

// liveRow is a session the registry holds an entry for, as GET /v1/sessions and
// GET /v1/registry/<id> answer it.
func liveRow(id string, session fakeSession) map[string]any {
	return map[string]any{
		"session_id": id, "title": session.title, "machine_id": "m1", "host": "m1",
		"capabilities": session.capabilities, "topics": []string{}, "roles": []string{},
		"last_seen": stoppedLastSeen, "state": envoy.StateLive,
	}
}

// writeSessions answers the registry's list, which never holds a stopped session.
func (l *queueingListener) writeSessions(w http.ResponseWriter) {
	rows := []map[string]any{}
	for id, session := range l.sessions {
		if !session.stopped {
			rows = append(rows, liveRow(id, session))
		}
	}
	_ = json.NewEncoder(w).Encode(rows)
}

func (l *queueingListener) writeRegistry(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
	withStopped := slices.Contains(strings.Split(r.URL.Query().Get("include"), ","), envoy.StateStopped)
	l.mu.Lock()
	if withStopped {
		l.stoppedReads++
	} else {
		l.registryOnlyReads++
	}
	l.mu.Unlock()
	session, known := l.sessions[sessionID]
	if !known || (session.stopped && !withStopped) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no live session ` + sessionID + `","state":"gone"}`))
		return
	}
	if !session.stopped {
		_ = json.NewEncoder(w).Encode(liveRow(sessionID, session))
		return
	}
	// The stopped shape carries no port, host, machine, dir or roles, as the record holds none.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session_id": sessionID, "state": envoy.StateStopped, "last_seen": stoppedLastSeen,
		"title": session.title, "capabilities": session.capabilities, "topics": []string{},
	})
}

func (l *queueingListener) writeSend(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	var request map[string]any
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode a send: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	target, _ := request["target_session"].(string)
	session, known := l.sessions[target]
	if !known {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no live session ` + target + `","state":"gone"}`))
		return
	}
	stopped := session.stopped || session.crashed
	if stopped && request["connected_only"] == true {
		// The listener stores nothing for a connected_only send to a stopped target, so this
		// send is not among the stored ones; it is recorded as refused instead.
		l.mu.Lock()
		l.refused = append(l.refused, request)
		l.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"the session is stopped"}`))
		return
	}
	l.mu.Lock()
	l.sends = append(l.sends, request)
	l.mu.Unlock()
	// recipient_state is the registry's own age reading, which a session known only by its record
	// reads as gone; state is the probe's, which wins.
	answer := map[string]any{
		"event_id": "envelope-" + target, "recipient": target,
		"recipient_state": envoy.StateLive, "state": envoy.SendStateConnected,
	}
	if stopped {
		answer["state"], answer["last_seen"] = envoy.SendStateStopped, stoppedLastSeen
	}
	if session.stopped {
		answer["recipient_state"] = envoy.StateGone
	}
	_ = json.NewEncoder(w).Encode(answer)
}

// sentTo is every send the listener took for target, in order.
func (l *queueingListener) sentTo(target string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return sendsFor(l.sends, target)
}

// refusedFor is every connected_only send the listener refused for target, in order.
func (l *queueingListener) refusedFor(target string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return sendsFor(l.refused, target)
}

// sendsFor is the sends of record addressed to target, in order. The caller holds the
// listener's lock.
func sendsFor(record []map[string]any, target string) []map[string]any {
	took := []map[string]any{}
	for _, send := range record {
		if send["target_session"] == target {
			took = append(took, send)
		}
	}
	return took
}

// reads is how many registry reads asked for stopped sessions, and how many did not.
func (l *queueingListener) reads() (stopped, registryOnly int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stoppedReads, l.registryOnlyReads
}

// stoppedAndLive is the listener every test below runs against: s1 stopped but known, s2
// running, and crashed, whose registry entry is still in place while its process has gone.
func stoppedAndLive(t *testing.T) *queueingListener {
	t.Helper()
	modes := []string{"aside", "btw", "steer"}
	return newQueueingListener(t, map[string]fakeSession{
		"s1":      {title: "planner", capabilities: modes, stopped: true},
		"s2":      {title: "tester", capabilities: modes},
		"crashed": {title: "crashed", capabilities: modes, crashed: true},
	})
}

// A person's direct message to a stopped but known session reaches the listener, which stores it
// for that session, and the attempt is recorded queued - never failed "no live session", which
// would tell the person their message was thrown away. The receipt the conversation renders
// carries the same word.
func TestDirectMessageToAStoppedSessionIsQueued(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "Status when you are back?", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to a stopped session: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if len(message.Deliveries) != 1 || message.Deliveries[0].State != "queued" ||
		message.Deliveries[0].Error != nil || message.Deliveries[0].SessionID != "s1" {
		t.Fatalf("the answer's deliveries = %#v, want one queued attempt pinned to s1", message.Deliveries)
	}
	if attempts := messageAttempts(t, database, message.ID); len(attempts) != 1 ||
		attempts[0].State != "queued" || attempts[0].EnvelopeID == nil {
		t.Fatalf("the stored attempts = %#v, want one queued attempt carrying its envelope", attempts)
	}
	if sends := listener.sentTo("s1"); len(sends) != 1 {
		t.Fatalf("the listener took %d sends for s1, want exactly one", len(sends))
	} else if _, asked := sends[0]["connected_only"]; asked {
		t.Fatalf("an ordinary direct message asked for a connected session: %#v", sends[0])
	}
	if receipts := messageDeliveryReceiptRows(t, database, message.ID); len(receipts) != 1 ||
		receipts[0].State != "queued" {
		t.Fatalf("the delivery receipts = %#v, want one carrying queued", receipts)
	}
	// The registry-only read is what an issue claim's holder liveness, the route roster and role
	// resolution ask for; a delivery asks for the stopped sessions too, or it would never find
	// this session at all.
	if stopped, _ := listener.reads(); stopped == 0 {
		t.Fatalf("the delivery made no registry read that asks for stopped sessions")
	}
}

// A mention of a stopped but known session is queued for it too, on one send: the mention's own.
// The create answers with the attempt pending, pinned to the session, and the mention sender
// settles it behind the answer.
func TestMentionOfAStoppedSessionIsQueuedOnOneSend(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)
	issue := createInteractionIssue(t, handler, "QUE", "A mention of a stopped session", "before")

	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body":     "@s1 pick this up when you are back",
		"mentions": []map[string]string{{"target": "session:s1"}},
	}))
	if len(comment.Deliveries) != 1 || comment.Deliveries[0].SessionID == nil || *comment.Deliveries[0].SessionID != "s1" {
		t.Fatalf("the mention's deliveries = %#v, want one attempt pinned to s1", comment.Deliveries)
	}
	attempts := settledCommentDeliveries(t, database, comment.ID)
	if len(attempts) != 1 || attempts[0].State != "queued" || attempts[0].Error != nil ||
		attempts[0].SessionID == nil || *attempts[0].SessionID != "s1" || attempts[0].EnvelopeID == nil {
		t.Fatalf("the mention's settled attempts = %#v, want one queued attempt pinned to s1 carrying its envelope", attempts)
	}
	if sends := listener.sentTo("s1"); len(sends) != 1 {
		t.Fatalf("the listener took %d sends for s1, want exactly one", len(sends))
	}
}

// A broadcast that names a stopped but known session beside a running one reaches both: the
// stopped one is a recipient with a message and a queued attempt of its own, and it is in no
// exclusion list.
func TestBroadcastQueuesForAStoppedRecipientAndExcludesItFromNothing(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, _ := newTargetedMessageHandler(t, listener.url)

	response := postBroadcast(t, handler, map[string]any{
		"body": "Stand down and report.", "delivery": "steer", "session_ids": []string{"s1", "s2"},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("broadcast: status=%d body=%s", response.Code, response.Body.String())
	}
	created := decodeBody[broadcastResponse](t, response)
	if len(created.Excluded) != 0 {
		t.Fatalf("excluded = %#v, want nothing excluded: both sessions are known", created.Excluded)
	}
	// A broadcast answers once it is committed, each recipient's delivery behind the answer.
	settled := awaitBroadcastDeliveries(t, handler, created.ID)
	states := map[string]string{}
	for _, recipient := range settled.Recipients {
		if len(recipient.Message.Deliveries) != 1 {
			t.Fatalf("%s's attempts = %#v, want one", recipient.SessionID, recipient.Message.Deliveries)
		}
		states[recipient.SessionID] = recipient.Message.Deliveries[0].State
	}
	if len(states) != 2 || states["s1"] != "queued" || states["s2"] != "sent" {
		t.Fatalf("recipient states = %#v, want s1 queued and s2 sent", states)
	}
	if sends := listener.sentTo("s1"); len(sends) != 1 {
		t.Fatalf("the listener took %d sends for the stopped recipient, want one", len(sends))
	}
}

// The send's own answer decides, not the registry read before it: a session whose entry was still
// in place when Dispatch resolved it, and whose process had gone by the send, is queued too.
func TestASendAnsweredStoppedIsQueuedThoughTheRegistryReadFoundItLive(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "crashed", map[string]any{
		"body": "Are you still there?", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to a crashed session: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if attempts := messageAttempts(t, database, message.ID); len(attempts) != 1 || attempts[0].State != "queued" {
		t.Fatalf("the stored attempts = %#v, want one queued attempt", attempts)
	}
}

// A session Envoy knows nothing about is refused by the text retryableDeliveryFailure and the
// dashboard both key on, and is excluded from a broadcast with the reason they key on there.
func TestASessionTheListenerDoesNotKnowIsStillRefusedByTheSameText(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "ghost", map[string]any{
		"body": "Anyone there?", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to an unknown session: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	attempts := messageAttempts(t, database, message.ID)
	if len(attempts) != 1 || attempts[0].State != "failed" || attempts[0].Error == nil ||
		*attempts[0].Error != "no live session ghost" {
		t.Fatalf("the stored attempts = %#v, want one failed attempt reading \"no live session ghost\"", attempts)
	}
	if sends := listener.sentTo("ghost"); len(sends) != 0 {
		t.Fatalf("the listener took %d sends for a session it does not know, want none", len(sends))
	}

	response := postBroadcast(t, handler, map[string]any{
		"body": "Stand down.", "delivery": "steer", "session_ids": []string{"s1", "ghost"},
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("broadcast: status=%d body=%s", response.Code, response.Body.String())
	}
	broadcast := decodeBody[broadcastResponse](t, response)
	if len(broadcast.Excluded) != 1 || broadcast.Excluded[0].SessionID != "ghost" ||
		broadcast.Excluded[0].Reason != "no live session" {
		t.Fatalf("excluded = %#v, want ghost alone, for no live session", broadcast.Excluded)
	}
	if len(broadcast.Recipients) != 1 || broadcast.Recipients[0].SessionID != "s1" {
		t.Fatalf("recipients = %#v, want the stopped but known session alone", broadcast.Recipients)
	}
}

// A queued attempt is the session's turn to take: the listener handed it the message, so the
// accept route must not answer ACCEPT_FAILED, which it answers for an attempt the person was told
// had failed.
func TestASessionAcceptsAQueuedAttemptAsItsTurn(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "Take this as your turn.", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message to a stopped session: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if attempts := messageAttempts(t, database, message.ID); len(attempts) != 1 || attempts[0].State != "queued" {
		t.Fatalf("the stored attempts = %#v, want one queued attempt to accept", attempts)
	}

	accepted := acceptDelivery(t, handler, message.ID, 1, "s1")
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept a queued attempt: status=%d code=%s body=%s",
			accepted.Code, responseCode(t, accepted), accepted.Body.String())
	}
	took := decodeBody[acceptedAttempt](t, accepted)
	if took.State != "queued" || took.AcceptedAs == nil || *took.AcceptedAs != acceptedAsUserTurn ||
		took.Body != "Take this as your turn." {
		t.Fatalf("the accepted attempt = %#v, want the queued attempt taken as a user turn", took)
	}
}

// The delivery sweep resumes and settles the attempts a dead sender left pending. A queued
// attempt is not one of them - the listener took its message - so a pass leaves it as it is.
func TestTheSweepLeavesAQueuedAttemptAlone(t *testing.T) {
	listener := stoppedAndLive(t)
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.url})
	ctx := context.Background()
	var messageID string
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (author, body, target)
		values ('{"kind":"user","id":"alice"}', 'Queued and left alone.', 'session:s1')
		returning id::text
	`).Scan(&messageID); err != nil {
		t.Fatalf("seed the message: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, envelope_id,
		                                claimed_at, created_at, requested_by)
		values ($1::uuid, 1, 'steer', 's1', 'queued', 'envelope-s1',
		        now() - interval '10 minutes', now() - interval '10 minutes', '{"kind":"user","id":"alice"}')
	`, messageID); err != nil {
		t.Fatalf("seed the queued attempt: %v", err)
	}

	if err := served.resumeOwedDeliveries(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	attempts := messageAttempts(t, database, messageID)
	if len(attempts) != 1 || attempts[0].State != "queued" || attempts[0].Error != nil {
		t.Fatalf("after the sweep the attempts = %#v, want the one queued attempt untouched", attempts)
	}
	if sends := listener.sentTo("s1"); len(sends) != 0 {
		t.Fatalf("the sweep sent %d frames for a queued attempt, want none", len(sends))
	}
}

// An attempt whose sender died between the commit and the settle is resumed by the delivery
// sweep, which asks for a connected session if the request that stored the message did, or if the
// resumed send is a model or thinking switch the session runs as its own turn (isSessionSwitch: a
// person's own Send or Aside, not one an agent wrote for them, on no issue, no broadcast's copy,
// whose body is one switch line). So the switch's own attempt, resumed against a stopped target,
// is settled failed and nothing is stored for the session - the attempt a request that asked
// connected_only opened, which carries the flag on its row (message_deliveries.connected_only),
// because the sweep never saw the request, and the attempt an earlier release opened, whose row
// carries none. An ordinary send's attempt without the flag is queued.
func TestTheSweepResumesAStrandedSwitchAsConnectedOnly(t *testing.T) {
	for _, test := range []struct {
		name          string
		body          string
		connectedOnly bool
		wantState     string
		wantSends     int
	}{
		{name: "a stranded switch", body: "/model claude-opus", connectedOnly: true, wantState: "failed", wantSends: 0},
		{
			name: "a stranded switch an earlier release wrote", body: "/model claude-opus", connectedOnly: false,
			wantState: "failed", wantSends: 0,
		},
		{name: "a stranded ordinary send", body: "Carry on.", connectedOnly: false, wantState: "queued", wantSends: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener := stoppedAndLive(t)
			_, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.url})
			ctx := context.Background()
			var messageID string
			if err := database.Pool.QueryRow(ctx, `
				insert into messages (author, body, target)
				values ('{"kind":"user","id":"alice"}', $1, 'session:s1')
				returning id::text
			`, test.body).Scan(&messageID); err != nil {
				t.Fatalf("seed the message: %v", err)
			}
			// The attempt its own request committed and never settled: pending, claimed ten
			// minutes ago, which is what the sweep takes.
			if _, err := database.Pool.Exec(ctx, `
				insert into message_deliveries (message_id, attempt, delivery, session_id, state,
				                                claimed_at, created_at, requested_by, connected_only)
				values ($1::uuid, 1, 'steer', 's1', 'pending',
				        now() - interval '10 minutes', now() - interval '10 minutes',
				        '{"kind":"user","id":"alice"}', $2)
			`, messageID, model.ConnectedOnlyColumn(test.connectedOnly)); err != nil {
				t.Fatalf("strand the attempt: %v", err)
			}

			if err := served.resumeOwedDeliveries(ctx); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			attempts := messageAttempts(t, database, messageID)
			flags := connectedOnlyFlags(t, database, messageID)
			if len(attempts) != 1 || attempts[0].State != test.wantState ||
				!slices.Equal(flags, []bool{test.connectedOnly}) {
				t.Fatalf("the resumed attempt = %#v carrying connected_only %v, want one %s attempt carrying connected_only=%v",
					attempts, flags, test.wantState, test.connectedOnly)
			}
			if test.wantState == "failed" {
				if attempts[0].Error == nil || *attempts[0].Error != "the session is stopped" {
					t.Fatalf("the refused switch's error = %v, want \"the session is stopped\"", attempts[0].Error)
				}
				if refused := listener.refusedFor("s1"); len(refused) != 1 || refused[0]["connected_only"] != true {
					t.Fatalf("the listener refused %#v, want the one resumed send, carrying connected_only", refused)
				}
			}
			sends := listener.sentTo("s1")
			if len(sends) != test.wantSends {
				t.Fatalf("the listener stored %d sends, want %d", len(sends), test.wantSends)
			}
			if test.wantSends == 1 && sends[0]["connected_only"] == true {
				t.Fatalf("an ordinary resumed send asked for a connected session: %#v", sends[0])
			}
		})
	}
}

// The live view's model and thinking switch keeps its refusal: it asks the listener for a session
// that is running now (connected_only), and the 409 it meets settles the attempt failed with the
// listener's own reason. Nothing is stored for the session, so nothing applies at an unknown
// later time.
func TestTheModelSwitchToAStoppedSessionIsRefusedAndQueuesNothing(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "/model claude-opus", "delivery": "steer", "connected_only": true,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("the switch: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	attempts := messageAttempts(t, database, message.ID)
	if len(attempts) != 1 || attempts[0].State != "failed" || attempts[0].Error == nil ||
		*attempts[0].Error != "the session is stopped" {
		t.Fatalf("the switch's attempts = %#v, want one failed attempt reading \"the session is stopped\"", attempts)
	}
	if sends := listener.sentTo("s1"); len(sends) != 0 {
		t.Fatalf("the listener stored %d sends for the refused switch, want none", len(sends))
	}

	// The same switch to a running session is sent, under the same flag, and recorded sent.
	live := postAgentMessage(t, handler, "s2", map[string]any{
		"body": "/model claude-opus", "delivery": "steer", "connected_only": true,
	}, "alice")
	if live.Code != http.StatusCreated {
		t.Fatalf("the switch to a running session: status=%d body=%s", live.Code, live.Body.String())
	}
	running := decodeBody[model.Message](t, live)
	if attempts := messageAttempts(t, database, running.ID); len(attempts) != 1 || attempts[0].State != "sent" {
		t.Fatalf("the running session's attempts = %#v, want one sent attempt", attempts)
	}
	sends := listener.sentTo("s2")
	if len(sends) != 1 || sends[0]["connected_only"] != true {
		t.Fatalf("the switch's send = %#v, want one carrying connected_only", sends)
	}
}

// A refused switch keeps its refusal on every later attempt of it, whichever button the person
// presses on the agent card: Retry sends it in the mode it was written in, Use BTW instead sends
// it in another, and both ask the listener for a session that is running now, because the
// message asked for one. Each attempt is settled failed with the listener's own reason and the
// listener stores nothing, so no `/model` line waits for the session to run again.
func TestARetryOfARefusedSwitchIsRefusedAgainAndQueuesNothing(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "s1", map[string]any{
		"body": "/model claude-opus", "delivery": "steer", "connected_only": true,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("the switch: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)

	for _, press := range []struct {
		name     string
		delivery string
	}{
		{name: "Retry", delivery: "steer"},
		{name: "Use BTW instead", delivery: "btw"},
	} {
		again := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/deliveries",
			map[string]any{"delivery": press.delivery}, "alice")
		if again.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", press.name, again.Code, again.Body.String())
		}
		attempt := decodeBody[model.MessageDelivery](t, again)
		if attempt.State != "failed" || attempt.Error == nil || *attempt.Error != "the session is stopped" {
			t.Fatalf("%s answered %#v, want a failed attempt reading \"the session is stopped\"", press.name, attempt)
		}
	}

	attempts := messageAttempts(t, database, message.ID)
	flags := connectedOnlyFlags(t, database, message.ID)
	if len(attempts) != 3 || len(flags) != 3 {
		t.Fatalf("the switch's attempts = %#v carrying connected_only %v, want three", attempts, flags)
	}
	for index, attempt := range attempts {
		if attempt.State != "failed" || !flags[index] ||
			attempt.Error == nil || *attempt.Error != "the session is stopped" {
			t.Fatalf("attempt %d = %#v, want it failed, carrying connected_only, reading \"the session is stopped\"",
				attempt.Attempt, attempt)
		}
	}
	if sends := listener.sentTo("s1"); len(sends) != 0 {
		t.Fatalf("the listener stored %d sends for the refused switch and its retries, want none", len(sends))
	}
}

// TestTheSwitchRuleCaseByCase is plan D9's switch rule, one subtest per row of its table ("The
// switch rule, case by case", dispatch://INFRA-626/artifact/plan-md), every row sent to s1, a
// session the listener knows and that is not running. A switch is a person's own Send or Aside
// (not one an agent wrote for them) on no issue, no broadcast's copy, whose whole body is one
// /model or /effort line: the one shape a session runs as its own turn (isUserTurnCandidate in
// packages/pi-shared/src/accept-user-turn.ts, isSessionSwitch here). Every attempt of one, and
// every send, is refused 409 "the session is stopped" with nothing stored, whatever its request
// said and whatever its row holds. The same line in any other shape reaches the session as a card
// and changes no model, so it is stored for the session and recorded queued like any other message
// (spec R6). The rule is judged at each send, so an attempt's connected_only carries only what its
// request asked, which only the agents route takes; a request that asks it holds any message to a
// running session, a switch or not.
func TestTheSwitchRuleCaseByCase(t *testing.T) {
	const line = "/model anthropic/claude-x"
	const person, session = `{"kind":"user","id":"alice"}`, `{"kind":"session","id":"s2"}`
	for _, row := range []struct {
		name string
		// refused is the row's answer: every attempt it judges refused 409 "the session is
		// stopped" with nothing stored, or else stored for the session and recorded queued.
		refused bool
		// judge sends the row's cases to s1 and answers the attempts the row judges.
		judge func(t *testing.T, rig switchRuleRig) []judgedAttempt
	}{
		{
			name: "a switch, attempt 1", refused: true,
			judge: func(t *testing.T, rig switchRuleRig) []judgedAttempt {
				var judged []judgedAttempt
				for _, sent := range []struct {
					what  string
					input map[string]any
				}{
					{what: "a model switch from a tab older than connected_only", input: map[string]any{"body": line, "delivery": "steer"}},
					{what: "a thinking switch from a tab older than connected_only", input: map[string]any{"body": "/effort high", "delivery": "steer"}},
					{what: "a model switch sent as an Aside", input: map[string]any{"body": line, "delivery": "aside"}},
					{what: "the live view's switch", input: map[string]any{"body": line, "delivery": "steer", "connected_only": true}},
				} {
					message := rig.send(t, sent.what, sent.input)
					asked, _ := sent.input["connected_only"].(bool)
					judged = append(judged, rig.attempt(t, sent.what, message.ID, 1, asked))
				}
				return judged
			},
		},
		{
			name: "a switch, any later attempt", refused: true,
			judge: func(t *testing.T, rig switchRuleRig) []judgedAttempt {
				sent := rig.send(t, "the switch", map[string]any{"body": line, "delivery": "steer"})
				aside := rig.send(t, "the switch sent as an Aside", map[string]any{"body": line, "delivery": "aside"})
				// An earlier release wrote connected_only on no row: the live view's switch, its
				// attempt 1 left as that release would have left it.
				earlier := rig.send(t, "an earlier release's switch",
					map[string]any{"body": line, "delivery": "steer", "connected_only": true})
				if _, err := rig.database.Pool.Exec(context.Background(),
					`update message_deliveries set connected_only = null where message_id = $1`, earlier.ID); err != nil {
					t.Fatalf("write the switch's attempt 1 as an earlier release wrote it: %v", err)
				}
				judged := []judgedAttempt{
					rig.press(t, "Retry", sent.ID, "steer", 2, false),
					rig.press(t, "Use Aside instead", sent.ID, "aside", 3, false),
					rig.press(t, "Use Send instead of the Aside", aside.ID, "steer", 2, false),
					rig.press(t, "Retry of an earlier release's switch", earlier.ID, "steer", 2, false),
				}
				// The sweep resumes the pending attempt a sender that died left: one an earlier
				// release wrote with no flag, and one the live view's switch wrote with it.
				unflagged := rig.strand(t, person, line, "steer", false)
				flagged := rig.strand(t, person, line, "steer", true)
				rig.sweep(t)
				return append(judged,
					rig.attempt(t, "the sweep's resume of an earlier release's switch", unflagged, 1, false),
					rig.attempt(t, "the sweep's resume of the live view's switch", flagged, 1, true),
				)
			},
		},
		{
			name: "the line in an issue message or a mention",
			judge: func(t *testing.T, rig switchRuleRig) []judgedAttempt {
				issue := createInteractionIssue(t, rig.handler, "QUE", "A switch line on an issue", "before")
				message := createIssueMessage(t, rig.handler, issue.Key, map[string]any{
					"body": line, "target": "session:s1", "delivery": "steer",
				}, "alice")
				comment := decodeMentionedComment(t, postMentionedComment(t, rig.handler, issue.Key, map[string]any{
					"body": line, "mentions": []map[string]string{{"target": "session:s1"}},
				}))
				settled := settledCommentDeliveries(t, rig.database, comment.ID)
				if len(settled) != 1 {
					t.Fatalf("the mention's attempts = %#v, want one", settled)
				}
				return []judgedAttempt{
					rig.attempt(t, "an issue message", message.ID, 1, false),
					{what: "a mention", state: settled[0].State, failure: settled[0].Error},
				}
			},
		},
		{
			name: "the line in a broadcast copy",
			judge: func(t *testing.T, rig switchRuleRig) []judgedAttempt {
				var judged []judgedAttempt
				for _, delivery := range []string{"steer", "aside"} {
					what := "a broadcast copy sent in " + delivery
					response := postBroadcast(t, rig.handler, map[string]any{
						"body": line, "delivery": delivery, "session_ids": []string{"s1"},
					}, "alice")
					if response.Code != http.StatusCreated {
						t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
					}
					settled := awaitBroadcastDeliveries(t, rig.handler, decodeBody[broadcastResponse](t, response).ID)
					if len(settled.Recipients) != 1 || settled.Recipients[0].SessionID != "s1" {
						t.Fatalf("%s: recipients = %#v, want s1 alone", what, settled.Recipients)
					}
					judged = append(judged, rig.attempt(t, what, settled.Recipients[0].Message.ID, 1, false))
				}
				return judged
			},
		},
		{
			name: "the line as BTW",
			judge: func(t *testing.T, rig switchRuleRig) []judgedAttempt {
				btw := rig.send(t, "a BTW", map[string]any{"body": line, "delivery": "btw"})
				refused := rig.send(t, "a switch", map[string]any{"body": line, "delivery": "steer"})
				return []judgedAttempt{
					rig.attempt(t, "a BTW", btw.ID, 1, false),
					rig.press(t, "Use BTW instead of a refused switch", refused.ID, "btw", 2, false),
				}
			},
		},
		{
			// No route sends a session's targeted message on no issue, so its rows are written
			// here: the author alone keeps it from being a switch.
			name: "the line in a message a session sent",
			judge: func(t *testing.T, rig switchRuleRig) []judgedAttempt {
				retried := rig.store(t, session, line)
				judged := []judgedAttempt{rig.press(t, "a person's Retry of a session's message", retried, "steer", 1, false)}
				stranded := rig.strand(t, session, line, "steer", false)
				rig.sweep(t)
				return append(judged, rig.attempt(t, "the sweep's resume of a session's message", stranded, 1, false))
			},
		},
		{
			// The flag alone holds a message that is no switch: attempt 1's own send, and the
			// sweep's resume of an attempt a sender that died left with the flag on its row.
			name: "a message whose request asked connected_only", refused: true,
			judge: func(t *testing.T, rig switchRuleRig) []judgedAttempt {
				message := rig.send(t, "a plain message asking connected_only",
					map[string]any{"body": "hello", "delivery": "steer", "connected_only": true})
				stranded := rig.strand(t, person, "hello", "steer", true)
				rig.sweep(t)
				return []judgedAttempt{
					rig.attempt(t, "a plain message asking connected_only", message.ID, 1, true),
					rig.attempt(t, "the sweep's resume of a plain message that asked connected_only", stranded, 1, true),
				}
			},
		},
		{
			name: "any other message",
			judge: func(t *testing.T, rig switchRuleRig) []judgedAttempt {
				var judged []judgedAttempt
				for _, sent := range []struct{ what, body string }{
					{what: "a plain message", body: "hello"},
					{what: "a message that mentions /model", body: "Run " + line + " when you are back."},
					{what: "a /model line with more words", body: line + " and then carry on"},
				} {
					message := rig.send(t, sent.what, map[string]any{"body": sent.body, "delivery": "steer"})
					judged = append(judged, rig.attempt(t, sent.what, message.ID, 1, false))
				}
				return judged
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			listener := stoppedAndLive(t)
			handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.url})
			judged := row.judge(t, switchRuleRig{handler: handler, database: database, served: served})
			for _, attempt := range judged {
				switch {
				case row.refused && (attempt.state != "failed" || deref(attempt.failure) != "the session is stopped"):
					t.Errorf("%s: the attempt is %s (%q), want it refused, failed reading \"the session is stopped\"",
						attempt.what, attempt.state, deref(attempt.failure))
				case !row.refused && (attempt.state != "queued" || attempt.failure != nil):
					t.Errorf("%s: the attempt is %s (%q), want it queued", attempt.what, attempt.state, deref(attempt.failure))
				}
				if attempt.connectedOnly != attempt.asked {
					t.Errorf("%s: the attempt carries connected_only=%v, want %v, what its request asked",
						attempt.what, attempt.connectedOnly, attempt.asked)
				}
			}
			// The listener's record says the same: a refused send asked for a running session
			// and is stored nowhere, and a stored one asked nothing.
			for _, send := range listener.refusedFor("s1") {
				if send["connected_only"] != true {
					t.Errorf("a refused send = %#v, want it carrying connected_only", send)
				}
			}
			stored := listener.sentTo("s1")
			for _, send := range stored {
				if _, asked := send["connected_only"]; asked {
					t.Errorf("a stored send asked for a running session: %#v", send)
				}
			}
			want := len(judged)
			if row.refused {
				want = 0
			}
			if len(stored) != want {
				t.Errorf("the listener stored %d sends for s1, want %d", len(stored), want)
			}
		})
	}
}

// switchRuleRig is what a row of TestTheSwitchRuleCaseByCase sends through: the API, its store,
// and the server whose delivery sweep a row runs.
type switchRuleRig struct {
	handler  http.Handler
	database *store.Store
	served   *server
}

// judgedAttempt is one attempt a row of TestTheSwitchRuleCaseByCase judges: what sent it, how it
// settled, the connected_only its row carries and whether its request asked for one.
type judgedAttempt struct {
	what          string
	state         string
	failure       *string
	connectedOnly bool
	asked         bool
}

// send is a person's send to s1 on the agents route.
func (rig switchRuleRig) send(t *testing.T, what string, input map[string]any) model.Message {
	t.Helper()
	created := postAgentMessage(t, rig.handler, "s1", input, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("%s: status=%d body=%s", what, created.Code, created.Body.String())
	}
	return decodeBody[model.Message](t, created)
}

// press is a person's press on the agent card - a Retry in delivery, or a Use ... instead - which
// opens attempt n of messageID, judged with asked as its request's connected_only.
func (rig switchRuleRig) press(t *testing.T, what, messageID, delivery string, n int, asked bool) judgedAttempt {
	t.Helper()
	pressed := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/messages/"+messageID+"/deliveries",
		map[string]any{"delivery": delivery}, "alice")
	if pressed.Code != http.StatusCreated {
		t.Fatalf("%s: status=%d body=%s", what, pressed.Code, pressed.Body.String())
	}
	return rig.attempt(t, what, messageID, n, asked)
}

// attempt is attempt n of messageID, which must be its latest, judged with asked as its request's
// connected_only.
func (rig switchRuleRig) attempt(t *testing.T, what, messageID string, n int, asked bool) judgedAttempt {
	t.Helper()
	attempts := messageAttempts(t, rig.database, messageID)
	flags := connectedOnlyFlags(t, rig.database, messageID)
	if len(attempts) != n || len(flags) != n {
		t.Fatalf("%s: the message has %d attempts, want %d", what, len(attempts), n)
	}
	latest := attempts[n-1]
	return judgedAttempt{
		what: what, state: latest.State, failure: latest.Error, connectedOnly: flags[n-1], asked: asked,
	}
}

// store writes a message to s1 by author, with no attempt.
func (rig switchRuleRig) store(t *testing.T, author, body string) string {
	t.Helper()
	var messageID string
	if err := rig.database.Pool.QueryRow(context.Background(), `
		insert into messages (author, body, target) values ($1::text::jsonb, $2, 'session:s1')
		returning id::text
	`, author, body).Scan(&messageID); err != nil {
		t.Fatalf("store the message: %v", err)
	}
	return messageID
}

// strand writes a message to s1 by author whose attempt 1 its sender left pending in delivery,
// claimed ten minutes ago, which is what the delivery sweep takes.
func (rig switchRuleRig) strand(t *testing.T, author, body, delivery string, connectedOnly bool) string {
	t.Helper()
	messageID := rig.store(t, author, body)
	if _, err := rig.database.Pool.Exec(context.Background(), `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state,
		                                claimed_at, created_at, requested_by, connected_only)
		values ($1::uuid, 1, $2, 's1', 'pending', now() - interval '10 minutes', now() - interval '10 minutes',
		        $3::text::jsonb, $4)
	`, messageID, delivery, author, model.ConnectedOnlyColumn(connectedOnly)); err != nil {
		t.Fatalf("strand the message's attempt: %v", err)
	}
	return messageID
}

// sweep runs one pass of the delivery sweep.
func (rig switchRuleRig) sweep(t *testing.T) {
	t.Helper()
	if err := rig.served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

// A switch whose attempt 1 an earlier release wrote carries no connected_only: that release had
// no column to write it in, and during the rolling update or after it the person's Retry, or a
// "Use ... instead", of that switch reaches a server of this release. The switch rule
// (isSessionSwitch) decides a switch whatever its request says, judged at each send in the mode it
// is sent in, so the attempt a Retry in Send or a Use Aside instead opens asks the listener for a
// session that is running now, the stopped session refuses it, and nothing is stored for it - as
// it is for the same switch sent afresh.
func TestARetryOfASwitchAnEarlierReleaseWroteIsRefusedForAStoppedSession(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)

	created := postAgentMessage(t, handler, "s1", map[string]any{"body": "/model acme/model-b", "delivery": "steer"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("the switch: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	// The row as an earlier release left it: the column it never wrote reads null.
	if _, err := database.Pool.Exec(context.Background(),
		`update message_deliveries set connected_only = null where message_id = $1 and attempt = 1`, message.ID); err != nil {
		t.Fatalf("write attempt 1 as an earlier release wrote it: %v", err)
	}
	storedBefore := len(listener.sentTo("s1"))

	for _, press := range []struct{ name, delivery string }{
		{name: "Retry", delivery: "steer"},
		{name: "Use Aside instead", delivery: "aside"},
	} {
		again := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/deliveries",
			map[string]any{"delivery": press.delivery}, "alice")
		if again.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", press.name, again.Code, again.Body.String())
		}
		attempt := decodeBody[model.MessageDelivery](t, again)
		if attempt.State != "failed" || attempt.Error == nil || *attempt.Error != "the session is stopped" {
			t.Fatalf("%s of an earlier release's switch answered %#v, want a failed attempt reading \"the session is stopped\"",
				press.name, attempt)
		}
	}
	if stored := listener.sentTo("s1"); len(stored) != storedBefore {
		t.Fatalf("the listener stored %d sends of the earlier release's switch for the stopped session, want none",
			len(stored)-storedBefore)
	}
}

// A /model line an agent sends for its person, with the person's own agent token, is recorded as
// the person with the agent's act, and the session's accept refuses it (ACCEPT_WRITTEN_BY_AGENT):
// it reaches the session as a card and is never run as a turn, so it changes no model. It is "a
// message a session sent", which plan v5's switch table queues for a stopped session (R6), and
// no switch: the switch rule holds only the shape a session runs as its turn.
func TestASwitchLineAnAgentSentForItsPersonIsQueuedForAStoppedSession(t *testing.T) {
	listener := stoppedAndLive(t)
	handler, database := newTargetedMessageHandler(t, listener.url)
	token := mintPersonalToken(t, handler, "alice", actingTokenName, nil)

	created := agentRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages?session="+actingSession, map[string]any{
		"body": "/model acme/model-b", "delivery": "steer",
	}, token.Token)
	if created.Code != http.StatusCreated {
		t.Fatalf("send the line with a personal token: status=%d body=%s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if message.Author.Kind != "user" || message.Author.Act == nil {
		t.Fatalf("the message's author = %#v, want the person with the agent's act", message.Author)
	}
	if attempts := messageAttempts(t, database, message.ID); len(attempts) != 1 || attempts[0].State != "queued" {
		t.Fatalf("the agent's line for its person to the stopped session = %#v, want one queued attempt", attempts)
	}
	if refused := listener.refusedFor("s1"); len(refused) != 0 {
		t.Fatalf("the listener refused %d sends of the agent's line, want none: %#v", len(refused), refused)
	}
}

// The claim holder's liveness read stays the registry's own: a stopped session holds no claim, so
// it must read as no holder at all and the read must not ask for stopped sessions. A delivery's
// read of the same session, beside it, finds it.
func TestTheClaimHoldersLivenessReadNeverAsksForStoppedSessions(t *testing.T) {
	listener := stoppedAndLive(t)
	_, _, deps := newTestServer(t, testServerOptions{envoyURL: listener.url})
	direct := directServer(deps)
	ctx := context.Background()

	liveness, err := direct.fetchHolderLiveness(ctx, model.IssueClaim{Actor: model.Actor{Kind: "session", ID: "s1"}})
	if err != nil {
		t.Fatalf("read the holder's liveness: %v", err)
	}
	if !liveness.fetched || liveness.holder != nil {
		t.Fatalf("liveness of a stopped holder = %#v, want fetched with no holder", liveness)
	}
	stopped, registryOnly := listener.reads()
	if stopped != 0 {
		t.Fatalf("the holder's liveness made %d registry reads asking for stopped sessions, want none", stopped)
	}
	if registryOnly != 1 {
		t.Fatalf("the holder's liveness made %d registry-only reads, want one", registryOnly)
	}

	// The same session, resolved for a delivery, is found: the two reads differ only in what
	// they ask for.
	sessionID, _, resolveError := direct.resolveDeliveryTarget(ctx, model.Route{Kind: "session", ID: "s1"}, "steer")
	if sessionID != "s1" || resolveError != "" {
		t.Fatalf("a delivery resolved s1 as (%q, %q), want it resolved with no error", sessionID, resolveError)
	}
}

// connectedOnlyFlags reads message_deliveries.connected_only of every attempt of messageID,
// oldest first, as the claim reads it (model.ConnectedOnlySQL).
func connectedOnlyFlags(t *testing.T, database *store.Store, messageID string) []bool {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select `+model.ConnectedOnlySQL+` from message_deliveries where message_id = $1 order by attempt
	`, messageID)
	if err != nil {
		t.Fatalf("read the connected_only flags of %s: %v", messageID, err)
	}
	flags, err := pgx.CollectRows(rows, pgx.RowTo[bool])
	if err != nil {
		t.Fatalf("read the connected_only flags of %s: %v", messageID, err)
	}
	return flags
}
