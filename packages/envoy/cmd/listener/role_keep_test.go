package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// keptPublishAnswer is the publish route's 202 for a role message it kept: the envelope as it was
// stored, with the kept fields the answer carries only then.
type keptPublishAnswer struct {
	contracts.Envelope
	Holder      string `json:"holder"`
	HolderState string `json:"holder_state"`
	LastSeen    *int64 `json:"last_seen"`
	Kept        bool   `json:"kept"`
	KeptReason  string `json:"kept_reason"`
	KeptUntil   int64  `json:"kept_until"`
}

// readKeptRoleMessage reads the copy the stream holds on role's kept subject, and reports false
// when it holds none.
func readKeptRoleMessage(t *testing.T, client *bus.Client, role string) (contracts.Envelope, bool) {
	t.Helper()
	item, _, ok := readKeptRoleCopy(t, client, role)
	return item, ok
}

// readKeptRoleCopy reads the newest copy the stream holds on role's kept subject with the time
// the stream stored it, and reports false when it holds none.
func readKeptRoleCopy(t *testing.T, client *bus.Client, role string) (contracts.Envelope, time.Time, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, err := client.LastStoredMessage(ctx, bus.KeptRoleSubject(role))
	if errors.Is(err, natsgo.ErrMsgNotFound) {
		return contracts.Envelope{}, time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read role %s's kept lane: %v", role, err)
	}
	var item contracts.Envelope
	if err := json.Unmarshal(stored.Data, &item); err != nil {
		t.Fatalf("decode the kept copy of role %s: %v", role, err)
	}
	return item, stored.Time, true
}

// waitForKeptRoleMessage waits until the stream holds the copy of eventID on role's kept subject,
// which the role lane writes after its own handler has answered, and returns it.
func waitForKeptRoleMessage(t *testing.T, client *bus.Client, role, eventID string) contracts.Envelope {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		item, ok := readKeptRoleMessage(t, client, role)
		if ok && item.EventID == eventID {
			return item
		}
		if time.Now().After(deadline) {
			t.Fatalf("role %s's kept lane does not hold event %s after 5s (last read: %+v, held: %t)", role, eventID, item, ok)
		}
	}
}

// assertNothingKept fails when the stream holds any copy on role's kept subject.
func assertNothingKept(t *testing.T, client *bus.Client, role string) {
	t.Helper()
	if item, ok := readKeptRoleMessage(t, client, role); ok {
		t.Fatalf("role %s's kept lane holds event %s, want nothing kept", role, item.EventID)
	}
}

// claimRoleWith registers sessionID with entry and gives it role, which is what a handover of
// role's kept lane resolves its holder from: the handover reads the role's claim and the session
// the claim names itself (drainKeptRole), so a test that drains a lane for a holder puts that
// holder where the listener reads it from rather than naming it.
func claimRoleWith(t *testing.T, harness listenerDeliveryHarness, sessionID, role string, entry session.SessionEntry) {
	t.Helper()
	if err := harness.sessions.Put(sessionID, entry); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	if _, err := harness.registry.SetRole(sessionID, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, sessionID, err)
	}
}

// keptRoleDeps is the publish route and the role-claim route over one listener's stores, as main
// wires them.
func keptRoleDeps(t *testing.T) (*listenerDeps, *bus.Client) {
	t.Helper()
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	return withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions}), client
}

// publishToRole sends message to role's topic through the publish route and returns its answer.
func publishToRole(t *testing.T, d *listenerDeps, role, body string) (*httptest.ResponseRecorder, keptPublishAnswer) {
	t.Helper()
	recorder, _ := publishThrough(d, body)
	var answer keptPublishAnswer
	if recorder.Code == http.StatusOK || recorder.Code == http.StatusAccepted {
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the publish answer for role %s: %v", role, err)
		}
	}
	return recorder, answer
}

// A role nobody holds is no longer a refusal: the listener keeps the message on that role's kept
// lane for whoever claims the role next, and answers its sender 202 saying so. The stored copy
// keeps its own role topic, so the next holder is handed the message as its sender sent it.
func TestAPublishToAnUnheldRoleIsKeptAndAnsweredWithItsReason(t *testing.T) {
	d, client := keptRoleDeps(t)
	const role = "kept-unclaimed"

	recorder, answer := publishToRole(t, d, role,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"nobody holds this yet"}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("publish to an unheld role: status %d, body %s; want 202", recorder.Code, recorder.Body.String())
	}
	if !answer.Kept || answer.KeptReason != "unclaimed" || answer.Holder != "" || answer.HolderState != "" || answer.LastSeen != nil {
		t.Fatalf("the answer is %+v, want kept with reason unclaimed and no holder", answer)
	}

	kept := waitForKeptRoleMessage(t, client, role, answer.EventID)
	if kept.Topic != contracts.RoleTopicPrefix+role {
		t.Fatalf("the kept copy's topic is %q, want the role topic it was published on", kept.Topic)
	}
	if kept.DedupeKey != answer.DedupeKey || kept.PayloadSummary != "nobody holds this yet" {
		t.Fatalf("the kept copy is %+v, want the envelope the answer named", kept)
	}
	if kept.KeptAt != nil {
		t.Fatalf("the stored copy carries kept_at %d: the time is stamped on the forward, from the stream's own store time", *kept.KeptAt)
	}
	if _, stored, _ := readKeptRoleCopy(t, client, role); answer.KeptUntil != stored.Add(roleKeepWindow).UnixMilli() {
		t.Fatalf("the answer's kept_until is %d, want %d: the copy's store time %s plus the keep window %s",
			answer.KeptUntil, stored.Add(roleKeepWindow).UnixMilli(), stored, roleKeepWindow)
	}
}

// A claim whose holder is not live is kept for as a role nobody claims is (roleKeepable): the
// publish is answered 202 kept with reason holder_lapsed, naming the holder, the state its record
// puts it in and, where the listener can date it, when it last registered, as the 404
// holder_lapsed names them; kept_until is the end of the copy's hour, the time the stream stored
// it plus roleKeepWindow. The copy waits on the role's kept lane for the next session that holds
// the role live, and the claim still stands: nothing but the role reaper releases one
// (dispatch://INFRA-392).
func TestAPublishToALapsedClaimIsKeptAndTheClaimStands(t *testing.T) {
	const role = "kept-lapsed"
	for _, tc := range []struct {
		name   string
		holder string
		// register leaves the holder's record as the row needs it, and returns when the holder
		// last registered, 0 when nothing can date it.
		register func(t *testing.T, d *listenerDeps, client *bus.Client, holder string) int64
		state    session.State
	}{
		{
			name:     "a holder that never registered",
			holder:   "ses_kept_lapsed_unregistered",
			register: func(*testing.T, *listenerDeps, *bus.Client, string) int64 { return 0 },
			state:    session.Gone,
		},
		{
			name:   "a holder whose registration is gone",
			holder: "ses_kept_lapsed_departed",
			register: func(t *testing.T, d *listenerDeps, _ *bus.Client, holder string) int64 {
				t.Helper()
				if err := d.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
					t.Fatalf("register %s: %v", holder, err)
				}
				entry, err := d.sessions.Get(holder)
				if err != nil {
					t.Fatalf("read %s's registration: %v", holder, err)
				}
				if err := d.sessions.Delete(context.Background(), holder); err != nil {
					t.Fatalf("deregister %s: %v", holder, err)
				}
				return entry.UpdatedAt
			},
			state: session.Gone,
		},
		{
			name:   "a holder past the live window",
			holder: "ses_kept_lapsed_unresponsive",
			register: func(t *testing.T, _ *listenerDeps, client *bus.Client, holder string) int64 {
				t.Helper()
				heartbeat := time.Now().Add(-(session.LiveWindow + time.Minute))
				storeSessionHeartbeat(t, client, holder, heartbeat)
				return heartbeat.UnixMilli()
			},
			state: session.Unresponsive,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, client := keptRoleDeps(t)
			lastSeen := tc.register(t, d, client, tc.holder)
			if _, err := d.registry.SetRole(tc.holder, "test-machine", role, false); err != nil {
				t.Fatalf("claim %s for %s: %v", role, tc.holder, err)
			}

			recorder, answer := publishToRole(t, d, role,
				`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"is the holder still there?"}`)
			if recorder.Code != http.StatusAccepted {
				t.Fatalf("publish to a lapsed claim: status %d, body %s; want 202 with the message kept", recorder.Code, recorder.Body.String())
			}
			if !answer.Kept || answer.KeptReason != string(roles.Lapsed) || answer.Holder != tc.holder || answer.HolderState != string(tc.state) {
				t.Fatalf("the answer is %s, want kept with reason holder_lapsed naming %s as %s", recorder.Body.String(), tc.holder, tc.state)
			}
			if lastSeen == 0 && answer.LastSeen != nil {
				t.Fatalf("the answer names last_seen %d for a holder nothing can date, want none: %s", *answer.LastSeen, recorder.Body.String())
			}
			if lastSeen != 0 && (answer.LastSeen == nil || *answer.LastSeen != lastSeen) {
				t.Fatalf("the answer is %s, want last_seen %d, when %s last registered", recorder.Body.String(), lastSeen, tc.holder)
			}

			kept := waitForKeptRoleMessage(t, client, role, answer.EventID)
			if kept.Topic != contracts.RoleTopicPrefix+role || kept.DedupeKey != answer.DedupeKey {
				t.Fatalf("the kept copy is %+v, want the envelope the answer named on the role topic", kept)
			}
			if _, stored, _ := readKeptRoleCopy(t, client, role); answer.KeptUntil != stored.Add(roleKeepWindow).UnixMilli() {
				t.Fatalf("the answer's kept_until is %d, want %d: the copy's store time %s plus the keep window %s",
					answer.KeptUntil, stored.Add(roleKeepWindow).UnixMilli(), stored, roleKeepWindow)
			}
			if held, err := d.registry.RoleHolder(role); err != nil || held != tc.holder {
				t.Fatalf("the lapsed claim after the publish = %q, %v; want %s's, left for the role reaper", held, err, tc.holder)
			}
		})
	}
}

// A sender that holds a queue of its own asks for keep: false, and to a claim whose holder is not
// live it gets the 404 holder_lapsed naming the holder, with the claim retained, the refusal's
// exception on the role topic's exception lane, and nothing stored for a later holder.
func TestAPublishWithKeepFalseToALapsedClaimIsRefusedAndKeepsNothing(t *testing.T) {
	d, client := keptRoleDeps(t)
	const (
		role   = "kept-lapsed-opted-out"
		holder = "ses_kept_lapsed_opted_out"
	)
	if _, err := d.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for a session the store does not hold: %v", role, err)
	}
	probe, err := client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + role)
	if err != nil {
		t.Fatalf("subscribe to the role topic's exception lane: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the probe: %v", err)
	}

	recorder, _ := publishThrough(d,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"the sender queues this itself","keep":false}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("publish with keep: false to a lapsed claim: status %d, body %s; want 404 with nothing kept", recorder.Code, recorder.Body.String())
	}
	var refusal listenerwire.RoleRefusal
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	if refusal.Reason != roles.Lapsed || refusal.Holder != holder || refusal.ClaimReleased == nil || *refusal.ClaimReleased {
		t.Fatalf("the refusal is %s, want reason holder_lapsed naming %s with its claim retained", recorder.Body.String(), holder)
	}
	message, err := probe.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("the refusal raised no exception: %v", err)
	}
	var exception contracts.Envelope
	if err := json.Unmarshal(message.Data, &exception); err != nil {
		t.Fatalf("decode the exception envelope: %v", err)
	}
	if payload := exceptionPayloadOf(t, exception); payload.Reason != string(roles.Lapsed) || payload.RecipientSession != holder {
		t.Fatalf("the exception payload = %+v, want reason holder_lapsed naming %s", payload, holder)
	}
	assertNothingKept(t, client, role)
	if held, err := d.registry.RoleHolder(role); err != nil || held != holder {
		t.Fatalf("the lapsed claim after the publish = %q, %v; want %s's, left for the role reaper", held, err, holder)
	}
}

// A sender that holds a queue of its own asks for keep: false on its request and gets today's
// answer: the 404 naming the state, with nothing stored for a later holder. The flag is this
// route's own request field, read by its give-up alone (roleSend.noHolder).
func TestAPublishWithKeepFalseToAnUnheldRoleIsRefusedAndKeepsNothing(t *testing.T) {
	d, client := keptRoleDeps(t)
	const role = "kept-opted-out"

	recorder, _ := publishThrough(d,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"the sender queues this itself","keep":false}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("publish with keep: false to an unheld role: status %d, body %s; want 404", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "{\"error\":\"no holder for role "+role+"\",\"reason\":\"unclaimed\",\"claim_revision\":0}\n" {
		t.Fatalf("body = %q", body)
	}
	assertNothingKept(t, client, role)
}

// A clustered stream refuses a publish whose message id another publish still has in process
// (bus.ErrDuplicateInProcess, nats-server's 10158), with no sequence to read a copy at. The keep
// publishes again under the same id roleKeepInProcessPause later, until an answer names the copy,
// all inside its one window (storeKeptRoleCopy): a stream that refuses twice and then stores the
// copy is a keep that kept it, under KeptMsgID each time, with its hour counted from the copy's
// store time. A stream that refuses until the window has run out is a keep that failed, and the
// lane holds nothing. No cluster gives the refusal on demand, so the keep is passed a publish that
// answers it, which the keep takes in the stream's place.
func TestAKeepPublishesAgainWhileItsMessageIDIsStillInProcess(t *testing.T) {
	const role = "kept-in-process"
	inProcess := fmt.Errorf("%w: %w", bus.ErrDuplicateInProcess,
		&natsgo.APIError{Code: 409, ErrorCode: 10158, Description: "duplicate message id is in process"})

	t.Run("refused twice, then stored", func(t *testing.T) {
		harness := newListenerDeliveryHarness(t)
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-in-process-stored")
		var ids []string
		publish := func(ctx context.Context, subject string, envelope contracts.Envelope, msgID string) (bus.StoredAck, error) {
			ids = append(ids, msgID)
			if len(ids) <= 2 {
				return bus.StoredAck{}, inProcess
			}
			return harness.client.PublishStoredAck(ctx, subject, envelope, msgID)
		}
		ctx, cancel := context.WithTimeout(context.Background(), roleKeepPublishWindow)
		defer cancel()
		started := time.Now()
		kept, err := storeKeptRoleCopy(ctx, harness.config, role, item, publish)
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("a keep whose first two publishes were refused as in process: %v; want it kept by the third", err)
		}
		msgID := bus.KeptMsgID(item.DedupeKey, role)
		if !slices.Equal(ids, []string{msgID, msgID, msgID}) {
			t.Fatalf("the keep published under %v, want %s three times: the in-process refusal is retried under the id it refused", ids, msgID)
		}
		if elapsed < 2*roleKeepInProcessPause {
			t.Fatalf("the keep took %s over two refusals, want at least two pauses of %s between its publishes", elapsed, roleKeepInProcessPause)
		}
		stored, at, ok := readKeptRoleCopy(t, harness.client, role)
		if !ok || stored.EventID != item.EventID || kept.item.EventID != item.EventID {
			t.Fatalf("the lane holds %+v (held: %t) and the keep answered %+v, want both to be event %s", stored, ok, kept.item, item.EventID)
		}
		if want := at.Add(roleKeepWindow).UnixMilli(); kept.until != want {
			t.Fatalf("the keep's kept_until is %d, want %d: the copy's store time %s plus the keep window", kept.until, want, at)
		}
	})

	t.Run("refused until the window ends", func(t *testing.T) {
		harness := newListenerDeliveryHarness(t)
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-in-process-refused")
		publish := func(context.Context, string, contracts.Envelope, string) (bus.StoredAck, error) {
			return bus.StoredAck{}, inProcess
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*roleKeepInProcessPause)
		defer cancel()
		if _, err := storeKeptRoleCopy(ctx, harness.config, role, item, publish); !errors.Is(err, bus.ErrDuplicateInProcess) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a keep refused as in process until its window ended = %v, want a failed keep naming the refusal and the window", err)
		}
		assertNothingKept(t, harness.client, role)
	})
}

// startRawKeptStandIn is a session process that keeps every frame it is sent as the bytes it was
// sent, so a test can judge a forwarded copy by the keys it carries rather than by the fields this
// build decodes. It answers nothing: a forward is stored for the holder's consumer, and no sender
// waits on the agent subject for an answer of the session's.
func startRawKeptStandIn(t *testing.T, uri, sessionID string) <-chan []byte {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	frames := make(chan []byte, 16)
	subscription, err := conn.Subscribe(contracts.LegacyAgentSubject(sessionID), func(message *natsgo.Msg) {
		frames <- slices.Clone(message.Data)
	})
	if err != nil {
		t.Fatalf("stand-in %s subscribe: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("stand-in %s flush: %v", sessionID, err)
	}
	return frames
}

// assertNoKeepKey fails when body, the JSON of an envelope the listener wrote, carries a keep key
// at all.
func assertNoKeepKey(t *testing.T, what string, body []byte) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode %s: %v (%s)", what, err, body)
	}
	if keep, ok := fields["keep"]; ok {
		t.Fatalf("%s carries keep %s, want no such key: the flag is the publish route's request field, not the envelope's", what, keep)
	}
}

// The keep flag is the publish route's own request field. Nothing the listener writes carries it:
// not the copy it keeps, not the copy it forwards to the role's next holder, and not the answer
// it gives the sender - which would otherwise tell the holder of every daemon notice and every
// phase reminder "keep": false about a message it has just been handed.
func TestNoKeptRoleCopyOrPublishAnswerCarriesAKeepKey(t *testing.T) {
	d, client := keptRoleDeps(t)
	uri := testnats.URL(t)
	const (
		role   = "kept-no-keep-key"
		holder = "ses_kept_no_keep_key"
	)

	recorder, answer := publishToRole(t, d, role,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"nobody holds this yet","keep":true}`)
	if recorder.Code != http.StatusAccepted || !answer.Kept {
		t.Fatalf("publish to an unheld role: status %d, body %s; want 202 kept", recorder.Code, recorder.Body.String())
	}
	assertNoKeepKey(t, "the route's 202 kept answer", recorder.Body.Bytes())
	waitForKeptRoleMessage(t, client, role, answer.EventID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, err := client.LastStoredMessage(ctx, bus.KeptRoleSubject(role))
	if err != nil {
		t.Fatalf("read role %s's kept lane: %v", role, err)
	}
	assertNoKeepKey(t, "the kept copy", stored.Data)

	frames := startRawKeptStandIn(t, uri, holder)
	if err := d.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the claimant: %v", err)
	}
	claimRecorder := httptest.NewRecorder()
	roleSetHandler(d, "test-machine", logging.New("test")).ServeHTTP(claimRecorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(`{"session_id":"`+holder+`","role":"`+role+`"}`)))
	if claimRecorder.Code != http.StatusOK {
		t.Fatalf("claim of %s: status %d, body %s", role, claimRecorder.Code, claimRecorder.Body.String())
	}
	select {
	case frame := <-frames:
		assertNoKeepKey(t, "the copy handed to the role's next holder", frame)
	case <-time.After(30 * time.Second):
		t.Fatalf("the claimant of %s was handed no kept copy in 30 seconds", role)
	}

	// The live send's own forward and its 200, which carry the envelope the same way.
	liveRecorder, _ := publishToRole(t, d, role,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"the holder is live now"}`)
	if liveRecorder.Code != http.StatusOK {
		t.Fatalf("publish to the live holder of %s: status %d, body %s; want 200", role, liveRecorder.Code, liveRecorder.Body.String())
	}
	assertNoKeepKey(t, "the route's 200 answer", liveRecorder.Body.Bytes())
	select {
	case frame := <-frames:
		assertNoKeepKey(t, "the copy forwarded to the live holder", frame)
	case <-time.After(30 * time.Second):
		t.Fatalf("the live holder of %s was forwarded nothing in 30 seconds", role)
	}
}

// The claim is where a kept message is handed over: the session that takes the role is forwarded
// the copy once, under the role forward's dedupe key and carrying the time it was kept, and the
// cursor the drain leaves means a second claim of the same role forwards nothing again.
func TestAClaimForwardsEachKeptRoleMessageToItsNewHolderExactlyOnce(t *testing.T) {
	d, client := keptRoleDeps(t)
	uri := testnats.URL(t)
	const (
		role   = "kept-drained"
		holder = "ses_kept_claimant"
	)

	recorder, answer := publishToRole(t, d, role,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"wait for the next holder"}`)
	if recorder.Code != http.StatusAccepted || !answer.Kept {
		t.Fatalf("publish to an unheld role: status %d, body %s; want 202 kept", recorder.Code, recorder.Body.String())
	}
	waitForKeptRoleMessage(t, client, role, answer.EventID)

	frames := startStandIn(t, uri, holder)
	if err := d.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the claimant: %v", err)
	}
	claim := func(what string) {
		t.Helper()
		claimRecorder := httptest.NewRecorder()
		roleSetHandler(d, "test-machine", logging.New("test")).ServeHTTP(claimRecorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
			strings.NewReader(`{"session_id":"`+holder+`","role":"`+role+`"}`)))
		if claimRecorder.Code != http.StatusOK {
			t.Fatalf("%s claim of %s: status %d, body %s", what, role, claimRecorder.Code, claimRecorder.Body.String())
		}
	}

	claim("first")
	forwarded := expectFrames(t, frames, 1, 500*time.Millisecond)[0]
	if forwarded.EventID != answer.EventID {
		t.Fatalf("the claimant received event %s, want the kept %s", forwarded.EventID, answer.EventID)
	}
	if forwarded.DedupeKey != roleForwardDedupePrefix+answer.DedupeKey {
		t.Fatalf("the forwarded copy's dedupe key is %q, want %s", forwarded.DedupeKey, roleForwardDedupePrefix+answer.DedupeKey)
	}
	if forwarded.Topic != contracts.RoleTopicPrefix+role {
		t.Fatalf("the forwarded copy's topic is %q, want the role topic", forwarded.Topic)
	}
	if forwarded.KeptAt == nil || *forwarded.KeptAt <= 0 {
		t.Fatalf("the forwarded copy carries kept_at %v, want the time the stream stored it", forwarded.KeptAt)
	}

	claim("second")
	select {
	case again := <-frames:
		t.Fatalf("a second claim of %s forwarded event %s again", role, again.EventID)
	case <-time.After(time.Second):
	}
}

// A kept copy the stream has held longer than roleKeepWindow is not handed to a later holder: a
// role token names a tree role rather than one run, so a holder generations later is not the
// addressee its sender had. It is dropped, counted and logged once, and the cursor moves past it,
// so the role's next claim does not see it again. Its publish named no sender session, so no dead
// letter is sent for it: the count and the line say the listener kept it, and that it was not
// stale, its hour having ended a minute before the drop.
//
// The age is measured against the stream's own store time, which no caller may choose, so the test
// moves the drain's clock rather than holding a copy for an hour.
func TestAKeptRoleMessageOlderThanTheKeepWindowIsDroppedAndTheCursorMovesPastIt(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "kept-expired"
		holder = "ses_kept_expired_claimant"
	)
	var forwards, deadLetters atomic.Int32
	cfg := harness.config
	cfg.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
		forwards.Add(1)
		return nil
	})
	cfg.publishDeadLetter = func(context.Context, string, contracts.Envelope, string) error {
		deadLetters.Add(1)
		return nil
	}
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-expired-1")
	kept, err := keepRoleMessage(context.Background(), cfg, role, item)
	if err != nil {
		t.Fatalf("keep the role message: %v; the listener log says why:\n%s", err, harness.logs.String())
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	drain := func(now func() time.Time) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := drainKeptRole(ctx, cfg, role, now); err != nil {
			t.Fatalf("drain role %s's kept lane: %v", role, err)
		}
	}

	drain(func() time.Time { return time.Now().Add(roleKeepWindow + time.Minute) })
	if forwards.Load() != 0 {
		t.Fatalf("a kept copy older than %s was forwarded %d times, want none", roleKeepWindow, forwards.Load())
	}
	if deadLetters.Load() != 0 {
		t.Fatalf("a kept copy whose publish named no sender session was sent %d dead letters, want none", deadLetters.Load())
	}
	const dropLine = "listener dropped a kept role message older than the keep window"
	if dropped := strings.Count(harness.logs.String(), dropLine); dropped != 1 {
		t.Fatalf("the drop is logged %d times, want once:\n%s", dropped, harness.logs.String())
	}
	lines := logRecordsNamed(harness.logs.String(), dropLine, role)
	if len(lines) != 1 || lines[0]["event_id"] != item.EventID || lines[0]["source_session"] != "" ||
		lines[0]["kept_by"] != "listener" || lines[0]["stale"] != false || lines[0]["sequence"] != float64(kept.sequence) {
		t.Fatalf("the drop lines are %v, want one naming event %s, no source_session, kept_by listener, stale false and sequence %d:\n%s",
			lines, item.EventID, kept.sequence, harness.logs.String())
	}
	scrape := httptest.NewRecorder()
	harness.metrics.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if want := `test_role_kept_expired{kept_by="listener",stale="false"} 1`; !strings.Contains(scrape.Body.String(), want) {
		t.Fatalf("metrics do not hold %s:\n%s", want, scrape.Body.String())
	}
	if strings.Contains(scrape.Body.String(), "test_role_dead_letters{") {
		t.Fatalf("metrics count a dead letter for a copy that names no sender session:\n%s", scrape.Body.String())
	}

	// The copy is still on the lane and is now well inside the window; a cursor that had not
	// moved past it would forward it here.
	drain(time.Now)
	if forwards.Load() != 0 {
		t.Fatalf("a later claim forwarded the dropped copy %d times: the cursor did not move past it", forwards.Load())
	}
	if dropped := strings.Count(harness.logs.String(), dropLine); dropped != 1 {
		t.Fatalf("the drop is logged %d times after a second drain, want once:\n%s", dropped, harness.logs.String())
	}
}

// The role lane's keepable give-ups - a role nobody claims, and a claim whose holder is not live -
// keep the message instead of reporting it lost: no delivery exception goes out, and the lane's
// kept line names the reason, and for a lapsed holder that holder, whose claim stands. The kept
// copy lies on a subject no consumer filter carries (bus.StreamSubjects), so the durable never
// delivers it back and it is never counted received a second time.
func TestTheRoleLaneKeepsAMessageNoLiveHolderTakesWithNoException(t *testing.T) {
	cases := []struct {
		name string
		role string
		// holder is the session the role's claim names, registered nowhere; empty leaves the
		// role unclaimed.
		holder string
		reason roleKeepReason
	}{
		{name: "unclaimed role", role: "lane-kept-unclaimed", reason: roleKeptUnclaimed},
		{name: "lapsed holder", role: "lane-kept-lapsed", holder: "ses_lane_kept_lapsed", reason: roleKeptLapsed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t)
			topic := contracts.RoleTopicPrefix + tc.role
			if tc.holder != "" {
				if _, err := harness.registry.SetRole(tc.holder, "test-machine", tc.role, false); err != nil {
					t.Fatalf("claim %s for a session the store does not hold: %v", tc.role, err)
				}
			}
			probe, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + ">")
			if err != nil {
				t.Fatalf("subscribe to the exception lanes: %v", err)
			}
			t.Cleanup(func() { _ = probe.Unsubscribe() })
			delivered := make(chan contracts.Envelope, 8)
			durable, err := startListenerSubscription(harness.client, "listener-kept-"+id.New(), func(message *natsgo.Msg) {
				var item contracts.Envelope
				if err := json.Unmarshal(message.Data, &item); err == nil {
					delivered <- item
				}
				_ = message.Ack()
			})
			if err != nil {
				t.Fatalf("start the durable subscription: %v", err)
			}
			t.Cleanup(func() { _ = durable.Unsubscribe() })
			if err := harness.client.Conn.Flush(); err != nil {
				t.Fatalf("flush the probes: %v", err)
			}

			item := listenerTestEnvelope(topic, "lane-kept-"+tc.role)
			harness.coreHandler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})

			kept := waitForKeptRoleMessage(t, harness.client, tc.role, item.EventID)
			if kept.Topic != topic {
				t.Fatalf("the kept copy's topic is %q, want the role topic %s", kept.Topic, topic)
			}
			noException(t, probe)
			lines := logRecordsNamed(harness.logs.String(), "listener kept role message for the role's next holder", tc.role)
			if len(lines) != 1 || lines[0]["reason"] != string(tc.reason) || lines[0]["session_id"] != tc.holder || lines[0]["event_id"] != item.EventID {
				t.Fatalf("the lane's kept lines are %v, want one naming reason %s, session_id %q and event %s:\n%s",
					lines, tc.reason, tc.holder, item.EventID, harness.logs.String())
			}
			if tc.holder != "" {
				if held, err := harness.registry.RoleHolder(tc.role); err != nil || held != tc.holder {
					t.Fatalf("the lapsed claim after the delivery = %q, %v; want %s still holding it", held, err, tc.holder)
				}
			}
			select {
			case back := <-delivered:
				t.Fatalf("the durable delivered event %s on %s: a kept copy is on no consumer's filter", back.EventID, back.Topic)
			case <-time.After(time.Second):
			}
			scrape := httptest.NewRecorder()
			harness.metrics.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if want := `test_messages_received{source="agent",topic_prefix="notifications.role"} 1`; !strings.Contains(scrape.Body.String(), want) {
				t.Fatalf("metrics do not hold %s, so the kept copy was counted received:\n%s", want, scrape.Body.String())
			}
		})
	}
}

// Keeping is only for a role the listener cannot deliver to. A live holder's send is delivered and
// keeps nothing, and a holder that answers nothing at all is delivered too and keeps nothing
// either: the stream stored its forward on its agent subject, which is the whole of a forward's
// outcome (startRoleForward).
func TestADeliveredSendAndADeafHoldersSendBothKeepNothing(t *testing.T) {
	d, client := keptRoleDeps(t)
	uri := testnats.URL(t)

	t.Run("a live holder is delivered", func(t *testing.T) {
		const (
			role   = "kept-live-holder"
			holder = "ses_kept_live_holder"
		)
		claimRoleFor(t, d.registry, d.sessions, holder, role, nil)
		startStandIn(t, uri, holder)

		recorder, answer := publishToRole(t, d, role,
			`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"the holder is here"}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("publish to a live holder: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
		}
		if answer.Kept || answer.KeptReason != "" || answer.Holder != holder {
			t.Fatalf("the answer is %+v, want a delivered send to %s with no kept fields", answer, holder)
		}
		assertNothingKept(t, client, role)
	})

	t.Run("a deaf holder is delivered the stored forward", func(t *testing.T) {
		const (
			role   = "kept-deaf-holder"
			holder = "ses_kept_deaf_holder"
		)
		claimRoleFor(t, d.registry, d.sessions, holder, role, nil)
		startStandIn(t, uri, holder)

		recorder, _ := publishThrough(d,
			`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"the holder answers nothing"}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("publish to a deaf holder: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
		}
		assertNothingKept(t, client, role)
	})
}

// The drain's budget counts its forwards, not its reads. A role unheld for long enough gathers
// copies past the keep window, and a walk that spent one of roleKeepDrainMax on each of those
// handed the role's new holder nothing while every sender had been answered 202 kept. The walk
// is bounded by roleKeepScanMax instead, so a claim forwards every copy still inside the window.
func TestADrainSpendsItsBudgetOnForwardsRatherThanOnExpiredCopies(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "kept-drain-budget"
		holder = "ses_kept_budget_claimant"
		copies = 30
	)
	// expired reports whether the copy the drain walks n-th, counting from 1, is read as past the
	// keep window: the first 21 are, and every second one after them, so every copy that must be
	// forwarded lies past roleKeepDrainMax reads of the lane.
	expired := func(n int) bool { return n <= 21 || n%2 == 0 }

	cfg := harness.config
	var forwarded []string
	cfg.forwardRole = inlineForward(func(_ string, item contracts.Envelope, _ string, _ time.Duration) error {
		forwarded = append(forwarded, item.DedupeKey)
		return nil
	})
	var fresh []string
	for i := range copies {
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("kept-budget-%02d", i))
		if _, err := keepRoleMessage(context.Background(), cfg, role, item); err != nil {
			t.Fatalf("keep copy %d: %v; the listener log says why:\n%s", i, err, harness.logs.String())
		}
		if !expired(i + 1) {
			fresh = append(fresh, roleForwardDedupePrefix+item.DedupeKey)
		}
	}
	walked := 0
	now := func() time.Time {
		walked++
		if expired(walked) {
			return time.Now().Add(roleKeepWindow + time.Minute)
		}
		return time.Now()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	if err := drainKeptRole(ctx, cfg, role, now); err != nil {
		t.Fatalf("drain role %s's kept lane: %v", role, err)
	}
	if !slices.Equal(forwarded, fresh) {
		t.Fatalf("the drain forwarded %v, want every copy still inside the keep window: %v", forwarded, fresh)
	}
}

// Two listeners draining one role must not hand one copy over twice. Each drain claims a copy -
// advancing the role's cursor to that copy alone - before it forwards it, so a drain that lost
// the claim stops there; a cursor written once at the end of the walk let both of them forward
// everything they had read.
func TestTwoDrainsOfOneRoleHandEachKeptCopyToExactlyOneHolder(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "kept-two-drains"
		holder = "ses_kept_drain_holder"
		copies = 6
	)
	var keys []string
	for i := range copies {
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("kept-two-drains-%02d", i))
		if _, err := keepRoleMessage(context.Background(), harness.config, role, item); err != nil {
			t.Fatalf("keep copy %d: %v; the listener log says why:\n%s", i, err, harness.logs.String())
		}
		keys = append(keys, roleForwardDedupePrefix+item.DedupeKey)
	}

	var mu sync.Mutex
	handed := map[string][]string{}
	hand := func(listener string) func(string, contracts.Envelope, string, time.Duration) error {
		return func(_ string, item contracts.Envelope, _ string, _ time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			handed[item.DedupeKey] = append(handed[item.DedupeKey], listener)
			return nil
		}
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	toFirst := hand("first")
	first := harness.config
	first.forwardRole = inlineForward(func(subject string, item contracts.Envelope, msgID string, window time.Duration) error {
		// The first drain holds its first forward until the second has walked the lane, which
		// is the moment two listeners drain one role at once.
		once.Do(func() {
			close(started)
			<-release
		})
		return toFirst(subject, item, msgID, window)
	})
	// The second drain stands in for a second listener: it has guards of its own, where one
	// listener's guard for the role would hold it until the first drain had finished. One guard
	// per role bounds the handovers inside one listener (roleKeepGuards); across listeners the
	// cursor's compare-and-swap is the guard, which is what this test exercises.
	second := harness.config
	second.roleKeepDrains = newRoleKeepGuards()
	second.forwardRole = inlineForward(hand("second"))

	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		done <- drainKeptRole(ctx, first, role, time.Now)
	}()
	<-started
	// The second listener's handover reads the claim the first is holding and leaves the lane to
	// it, which it would wait bus.KeptRoleClaimWindow out to walk again; this context ends it
	// there, since what the test is about has happened by then.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := drainKeptRole(ctx, second, role, time.Now); err != nil {
		t.Fatalf("the second listener's drain of role %s's kept lane: %v", role, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the first listener's drain of role %s's kept lane: %v", role, err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, key := range keys {
		switch listeners := handed[key]; len(listeners) {
		case 1:
		case 0:
			t.Errorf("the kept copy under %s was forwarded by neither listener: %v", key, handed)
		default:
			t.Errorf("the kept copy under %s was forwarded by %v; a kept copy is handed over once", key, listeners)
		}
	}
}

// The drain is a forward path beside the role lane and the publish route, and it judges a copy by
// the rule they both apply (beforeForward): a copy whose own sender has since claimed the role is
// not forwarded back to it, and one whose payload the new holder's delivery modes do not take is
// not forwarded either. Each is passed over with the cursor already past it, so the role's next
// claim is not handed it again.
func TestTheDrainJudgesAKeptCopyByTheRulesEveryForwardPathApplies(t *testing.T) {
	for _, tc := range []struct {
		name string
		role string
		// prepare stamps the rule under test onto the copy that is kept.
		prepare func(item *contracts.Envelope, holder string)
		// entry is the new holder's session entry, which the rule judges the copy against.
		entry session.SessionEntry
		// line is the drain's line for the copy it passed over.
		line string
	}{
		{
			name:    "a copy whose own sender now holds the role",
			role:    "kept-echo",
			prepare: func(item *contracts.Envelope, holder string) { item.SourceSession = holder },
			entry:   session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true},
			line:    "listener passed over a kept role message its own sender has claimed the role for",
		},
		{
			name:    "a copy the new holder's delivery modes do not take",
			role:    "kept-unadvertised",
			prepare: func(item *contracts.Envelope, _ string) { item.Payload = `{"delivery":{"mode":"steer"}}` },
			entry:   session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}},
			line:    "listener kept role message's new holder does not advertise delivery mode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t)
			holder := "ses_" + strings.ReplaceAll(tc.role, "-", "_") + "_claimant"
			cfg := harness.config
			var forwards atomic.Int32
			cfg.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
				forwards.Add(1)
				return nil
			})
			item := listenerTestEnvelope(contracts.RoleTopicPrefix+tc.role, tc.role+"-1")
			tc.prepare(&item, holder)
			if _, err := keepRoleMessage(context.Background(), cfg, tc.role, item); err != nil {
				t.Fatalf("keep the role message: %v; the listener log says why:\n%s", err, harness.logs.String())
			}
			claimRoleWith(t, harness, holder, tc.role, tc.entry)
			drain := func(what string) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := drainKeptRole(ctx, cfg, tc.role, time.Now); err != nil {
					t.Fatalf("%s drain of role %s's kept lane: %v", what, tc.role, err)
				}
			}

			drain("the claim's")
			if forwards.Load() != 0 {
				t.Fatalf("the drain forwarded the copy %d times, want none:\n%s", forwards.Load(), harness.logs.String())
			}
			if passed := strings.Count(harness.logs.String(), tc.line); passed != 1 {
				t.Fatalf("the drain logged %q %d times, want once:\n%s", tc.line, passed, harness.logs.String())
			}

			// The cursor is past it, so a later claim's drain is not handed it again.
			drain("a later claim's")
			if forwards.Load() != 0 {
				t.Fatalf("a later drain forwarded the copy %d times: the cursor did not move past it", forwards.Load())
			}
			if passed := strings.Count(harness.logs.String(), tc.line); passed != 1 {
				t.Fatalf("the drain logged %q %d times after a second drain, want once:\n%s", tc.line, passed, harness.logs.String())
			}
		})
	}
}

// A kept copy carries its sender: the publish route stamps the sender onto the envelope before it
// is kept, so the holder handed the copy later reads the same stamp a live delivery carries.
func TestAKeptRoleMessageCarriesTheSenderThePublishStamped(t *testing.T) {
	d, client := keptRoleDeps(t)
	uri := testnats.URL(t)
	const (
		role   = "kept-sender-stamp"
		holder = "ses_kept_sender_claimant"
		sender = "ses_kept_sender"
		dir    = "/work/kept-sender"
	)
	if _, err := d.registry.Upsert(store.Interest{SessionID: sender, MachineID: "test-machine", Dir: dir}, []string{"notifications.test.kept"}); err != nil {
		t.Fatalf("register the sender's interest record: %v", err)
	}

	recorder, answer := publishToRole(t, d, role,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","source_session":"`+sender+`","message":"nobody holds this yet"}`)
	if recorder.Code != http.StatusAccepted || !answer.Kept {
		t.Fatalf("publish to an unheld role: status %d, body %s; want 202 kept", recorder.Code, recorder.Body.String())
	}
	kept := waitForKeptRoleMessage(t, client, role, answer.EventID)
	if kept.Sender == nil || kept.Sender.SessionID != sender || kept.Sender.Machine != "test-machine" || kept.Sender.Cwd != dir {
		t.Fatalf("the kept copy's sender is %+v, want the stamp the publish made for %s", kept.Sender, sender)
	}

	frames := startStandIn(t, uri, holder)
	if err := d.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the claimant: %v", err)
	}
	claimRecorder := httptest.NewRecorder()
	roleSetHandler(d, "test-machine", logging.New("test")).ServeHTTP(claimRecorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(`{"session_id":"`+holder+`","role":"`+role+`"}`)))
	if claimRecorder.Code != http.StatusOK {
		t.Fatalf("claim of %s: status %d, body %s", role, claimRecorder.Code, claimRecorder.Body.String())
	}
	forwarded := expectFrames(t, frames, 1, 500*time.Millisecond)[0]
	if forwarded.Sender == nil || forwarded.Sender.SessionID != sender || forwarded.Sender.Cwd != dir {
		t.Fatalf("the forwarded copy's sender is %+v, want the stamp the publish made for %s", forwarded.Sender, sender)
	}
}

// A role token POST /v1/roles/set refuses (roles.PatternString) is a role no session can ever
// claim, so a copy kept under it would expire unread. Keeping is off for it, and its sender gets
// the 404 it can act on, with nothing stored.
func TestAPublishToARoleNoClaimCanNameIsRefusedAndKeepsNothing(t *testing.T) {
	d, client := keptRoleDeps(t)
	const role = "legion-infra-INFRA-839-architect"

	recorder, _ := publishThrough(d,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"a token no claim can name"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("publish to a role no claim can name: status %d, body %s; want 404", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "{\"error\":\"no holder for role "+role+"\",\"reason\":\"unclaimed\",\"claim_revision\":0}\n" {
		t.Fatalf("body = %q", body)
	}
	assertNothingKept(t, client, role)

	// The claim route refuses the same token, which is why nothing may be kept under it.
	claimRecorder := httptest.NewRecorder()
	roleSetHandler(d, "test-machine", logging.New("test")).ServeHTTP(claimRecorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(`{"session_id":"ses_kept_invalid_role","role":"`+role+`"}`)))
	if claimRecorder.Code != http.StatusBadRequest {
		t.Fatalf("claim of %s: status %d, body %s; want the 400 that refuses the token", role, claimRecorder.Code, claimRecorder.Body.String())
	}
}

// A keep that lands after a concurrent claim's drain has read the lane still reaches that holder:
// the keep hands the lane over again and that handover resolves whoever holds the role now
// (keepForNextHolder, runKeptRoleDrain). The claim and its drain are made here before the keep,
// which is the order that race leaves behind, and no second claim follows.
func TestAKeptCopyReachesTheHolderThatClaimedTheRoleUnderTheKeep(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "kept-claimed-under-the-keep"
		holder = "ses_kept_under_claim"
	)
	cfg := harness.config
	forwards := make(chan contracts.Envelope, 4)
	cfg.forwardRole = inlineForward(func(_ string, item contracts.Envelope, _ string, _ time.Duration) error {
		forwards <- item
		return nil
	})
	claimRoleFor(t, harness.registry, harness.sessions, holder, role, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := drainKeptRole(ctx, cfg, role, time.Now); err != nil {
		t.Fatalf("the claim's own drain of role %s's still empty kept lane: %v", role, err)
	}

	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-under-claim-1")
	if err := keepRoleLaneMessage(context.Background(), cfg, role, item, roleKeptUnclaimed, ""); err != nil {
		t.Fatalf("keep the role message: %v; the listener log says why:\n%s", err, harness.logs.String())
	}
	select {
	case forwarded := <-forwards:
		if forwarded.EventID != item.EventID {
			t.Fatalf("the holder was handed event %s, want the kept %s", forwarded.EventID, item.EventID)
		}
		if forwarded.KeptAt == nil || *forwarded.KeptAt <= 0 {
			t.Fatalf("the forwarded copy carries kept_at %v, want the time the stream stored it", forwarded.KeptAt)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the copy kept for %s, which held the role already, reached it in no 20 seconds and no second claim comes:\n%s",
			holder, harness.logs.String())
	}
}

// A raw role publish is kept for the role's next holder whatever its sender is: the lane honours
// no opt-out of its own, because the one kind of sender that keeps a queue of its own and
// redelivers from it - the Legion daemon's notice queue, and a phase worker's stall reminder -
// publishes through the route and opts out on its request instead
// (TestAPublishWithKeepFalseToAnUnheldRoleIsRefusedAndKeepsNothing). So an ordinary raw role
// publish is kept and raises no exception.
func TestTheRoleLaneKeepsARawRolePublishForTheRolesNextHolder(t *testing.T) {
	const role = "lane-keep-raw"
	harness := newListenerDeliveryHarness(t)
	topic := contracts.RoleTopicPrefix + role
	probe, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + topic)
	if err != nil {
		t.Fatalf("subscribe to the role topic's exception lane: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the probe: %v", err)
	}

	item := listenerTestEnvelope(topic, "lane-keep-"+role)
	harness.coreHandler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})

	waitForKeptRoleMessage(t, harness.client, role, item.EventID)
	noException(t, probe)
}

// A claim the restart grace retained names a holder that is not live: its session registration is
// gone, though the claim stands for one session TTL (store.ExpiredRoleClaimRetained). The lane keeps
// the message for the role's next live holder, as it keeps one for any lapsed claim: the holder
// that registers again inside that grace, or a successor that claims the role, is handed it. No
// delivery exception goes out, the kept line names the holder, and the claim is left as it is.
func TestTheRoleLaneKeepsAMessageForAClaimTheRestartGraceRetained(t *testing.T) {
	const (
		role   = "kept-retained-claim"
		holder = "ses_kept_retained_holder"
	)
	client := setupPublishTestClient(t)
	// The claim is written before the listener's own registry opens, which is what a restart
	// leaves behind: that registry restores the claim and gives it one session TTL of grace
	// (store.ExpiredRoleClaimRetained) even while its holder's session registration is gone.
	writer, err := store.Open(t.Context(), client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the registry that writes the claim: %v", err)
	}
	if _, err := writer.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for a session the store does not hold: %v", role, err)
	}
	harness := newListenerDeliveryHarnessOn(t, client)

	topic := contracts.RoleTopicPrefix + role
	probe, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + topic)
	if err != nil {
		t.Fatalf("subscribe to the role topic's exception lane: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the probe: %v", err)
	}

	item := listenerTestEnvelope(topic, "lane-retained-claim")
	harness.coreHandler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})

	waitForKeptRoleMessage(t, client, role, item.EventID)
	noException(t, probe)
	lines := logRecordsNamed(harness.logs.String(), "listener kept role message for the role's next holder", role)
	if len(lines) != 1 || lines[0]["reason"] != string(roleKeptLapsed) || lines[0]["session_id"] != holder {
		t.Fatalf("the lane's kept lines are %v, want one naming reason %s and session_id %s:\n%s", lines, roleKeptLapsed, holder, harness.logs.String())
	}
	if held, err := harness.registry.RoleHolder(role); err != nil || held != holder {
		t.Fatalf("the retained claim after the delivery = %q, %v; want %s still holding it", held, err, holder)
	}
}

// The kept lane is the listener's own: its copies and the cursors of its drains. A publish to it
// through the route would let any bearer holder rewind a role's drain or put a copy of its own on
// the lane, so the route refuses that prefix as it refuses agent topics, and stores nothing.
func TestAPublishToTheListenersKeptLaneIsRefused(t *testing.T) {
	d, client := keptRoleDeps(t)
	const role = "kept-lane-guard"
	for _, subject := range []string{bus.KeptRoleSubject(role), bus.KeptRoleCursorSubject(role)} {
		t.Run(subject, func(t *testing.T) {
			recorder, _ := publishThrough(d,
				`{"topic":"`+subject+`","source":"agent","message":"a copy of my own"}`)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("publish to %s: status %d, body %s; want 400", subject, recorder.Code, recorder.Body.String())
			}
			if body := recorder.Body.String(); !strings.Contains(body, bus.KeptTopicPrefix) {
				t.Fatalf("the refusal of %s is %q, want it to name %s", subject, body, bus.KeptTopicPrefix)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := client.LastStoredMessage(ctx, subject); !errors.Is(err, natsgo.ErrMsgNotFound) {
				t.Fatalf("the stream holds a message on %s (%v); the refused publish stored nothing", subject, err)
			}
		})
	}
}
