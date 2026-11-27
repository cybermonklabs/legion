package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/contracts"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// Dispatch's message or mention for a role no session holds, whose Legion agent registry holds no
// entry for the role while its project answers, is published on the role's topic for the role's
// first holder (publishForRole in internal/dispatch/api). It names no source session, and names
// the Dispatch post it delivers (listenerwire.PublishRequest.DispatchPost), which the listener
// takes from Dispatch's own principal alone: such a copy is kept, wakes the role, reaches the
// role's first holder, and ends with an expiry notice to Dispatch rather than a dead letter.

// deliveredPost is the Dispatch post these tests' publishes deliver.
var deliveredPost = listenerwire.DispatchPost{Kind: listenerwire.DispatchPostMessage, ID: "6f1c2b9a-4d3e-4f5a-8b7c-9d0e1f2a3b4c"}

// bodyRecorder is a stand-in listener that keeps the body of every request it is sent and answers
// each as a kept role publish, so a test holds the exact body a client wrote.
type bodyRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (b *bodyRecorder) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	b.bodies = append(b.bodies, body)
	b.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"event_id":"recorded","kept":true,"kept_until":1}`))
}

func (b *bodyRecorder) only(t *testing.T) []byte {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.bodies) != 1 {
		t.Fatalf("the client sent %d requests, want its one publish", len(b.bodies))
	}
	return b.bodies[0]
}

// publishAs is one POST /v1/messages/publish of body to handler, as a client on this machine sends
// it, presenting authorize as its Authorization header when it is not empty.
func publishAs(handler http.Handler, body []byte, authorize string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", bytes.NewReader(body))
	request.Host = "127.0.0.1:9020"
	request.Header.Set("Content-Type", "application/json")
	if authorize != "" {
		request.Header.Set("Authorization", authorize)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// refusedForDispatchPost fails the test unless response is the 403 naming the field dispatch_post.
func refusedForDispatchPost(t *testing.T, what string, response *httptest.ResponseRecorder) {
	t.Helper()
	var refusal listenerwire.APIError
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusForbidden || len(refusal.Expected) != 1 || refusal.Expected[0] != dispatchPostField {
		t.Fatalf("%s: status %d body %s, want 403 naming the field %s", what, response.Code, response.Body.String(), dispatchPostField)
	}
}

// Dispatch's own client publishes a delivery to a Legion role no session holds and the registry
// holds no entry for, while its project answers, on Dispatch's projected token to the real
// publish route behind its real API auth: answered 202 kept, the kept copy naming no source
// session and naming the post as its source_event_id, the role's wake reported, and the session
// that claims the role first handed the copy once. The same body, Dispatch's client's own bytes,
// from any other caller cannot name the post: the shared token, a session's broker token, another
// service's identity token, and Dispatch's own token at a listener that names no Dispatch
// principal are each answered 403 naming dispatch_post, and nothing is kept for any of them. The
// control is #5412's rule for everyone else: the same publish from Dispatch's principal with no
// post, naming no source session, is answered 404 and nothing is kept.
func TestADispatchDeliveryToARolesFirstHolderIsKeptOnDispatchsPrincipalAlone(t *testing.T) {
	d, client := keptRoleDeps(t)
	useDispatchPrincipal(d, rigDispatchPrincipal)
	withLegionAgent(t, d, legionAgentEntry("ses_post_other_role", "INFRA-300", "reviewer"))
	wakes := reportWakes(t, d)
	role := legionagents.RoleToken("infra", "INFRA-301", "planner")
	control := legionagents.RoleToken("infra", "INFRA-302", "planner")

	broker := ciphertokentest.NewStubBroker(t)
	accountFile := filepath.Join(t.TempDir(), "listener-account-token")
	if err := os.WriteFile(accountFile, []byte("listener-account-token\n"), 0o600); err != nil {
		t.Fatalf("write the listener's own account token: %v", err)
	}
	tokens := newBrokerTokens(listenerBrokerConfig{BrokerURL: broker.Server.URL, AccountTokenFile: accountFile, Origin: listenerBrokerOrigin})
	if err := tokens.open(t.Context(), d.client.Conn, 1); err != nil {
		t.Fatalf("open the spent-proof bucket: %v", err)
	}
	issuer := newClusterIssuer(t)
	routes := http.NewServeMux()
	routes.Handle("/v1/messages/publish", publishHandler(d, listenerRigLogger(t, "listener")))
	listener := apiAuth(listenerAPIAuth{
		token:    rigSharedToken,
		verifier: issuer.verifier,
		accounts: []string{rigDispatchPrincipal, listenerListedSubject},
		brokers:  tokens,
		logger:   listenerRigLogger(t, "listener auth"),
	}, routes)
	dispatchToken := issuer.token(t, rigDispatchPrincipal)
	tokenFile := filepath.Join(t.TempDir(), "dispatch-account-token")
	rotateAccountToken(t, tokenFile, dispatchToken)
	publish := func(url string) (dispatchenvoy.RolePublishResult, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return dispatchenvoy.New(url, dispatchenvoy.WithAccountTokenFile(tokenFile)).PublishToRole(ctx, dispatchenvoy.RolePublishInput{
			Role: role, Post: deliveredPost, Message: "For the role's first holder",
			Payload: json.RawMessage(`{"delivery":{"attempt":1,"mode":"btw"}}`), IdempotencyKey: deliveredPost.ID + ":btw",
			ExpectsReply: contracts.ExpectsReplyOptional,
		})
	}

	// Dispatch's client's own body, as it writes it (deliveryBody), read off a stand-in.
	recorder := &bodyRecorder{}
	stand := httptest.NewServer(http.HandlerFunc(recorder.serve))
	t.Cleanup(stand.Close)
	if _, err := publish(stand.URL); err != nil {
		t.Fatalf("Dispatch's publish to the stand-in: %v", err)
	}
	body := recorder.only(t)

	const brokerSession = "ses_post_broker_caller"
	caller := ciphertokentest.NewCaller(t)
	broker.Says("session-broker-token", sessionAnswer(brokerSession, "ada@example.com", caller.JKT()))
	fromSession := httptest.NewRecorder()
	listener.ServeHTTP(fromSession, brokerTokenRequest(t, caller, http.MethodPost, "/v1/messages/publish", "session-broker-token", string(body)))
	refusedForDispatchPost(t, "the publish from a session's broker token", fromSession)
	refusedForDispatchPost(t, "the publish on the shared token", publishAs(listener, body, "Bearer "+rigSharedToken))
	refusedForDispatchPost(t, "the publish from another service's identity token",
		publishAs(listener, body, "Bearer "+issuer.token(t, listenerListedSubject)))
	unconfigured := *d
	useDispatchPrincipal(&unconfigured, "")
	unconfiguredRoutes := http.NewServeMux()
	unconfiguredRoutes.Handle("/v1/messages/publish", publishHandler(&unconfigured, listenerRigLogger(t, "unconfigured listener")))
	refusedForDispatchPost(t, "the publish on Dispatch's token to a listener that names no Dispatch principal",
		publishAs(apiAuth(listenerAPIAuth{verifier: issuer.verifier, logger: listenerRigLogger(t, "unconfigured listener auth")}, unconfiguredRoutes),
			body, "Bearer "+dispatchToken))
	assertNothingKept(t, client, role)

	// Control: #5412's rule stands for a publish that names no post: Dispatch's principal, no
	// source session, no post, and nothing is kept.
	var unnamed map[string]any
	if err := json.Unmarshal(body, &unnamed); err != nil {
		t.Fatalf("decode Dispatch's publish body %s: %v", body, err)
	}
	if _, named := unnamed[dispatchPostField]; !named {
		t.Fatalf("Dispatch's publish body %s names no %s", body, dispatchPostField)
	}
	delete(unnamed, dispatchPostField)
	unnamed["topic"] = contracts.RoleTopicPrefix + control
	plain, err := json.Marshal(unnamed)
	if err != nil {
		t.Fatal(err)
	}
	if refused := publishAs(listener, plain, "Bearer "+dispatchToken); refused.Code != http.StatusNotFound {
		t.Fatalf("control: Dispatch's publish naming no post and no source session: status %d body %s, want 404", refused.Code, refused.Body.String())
	}
	assertNothingKept(t, client, control)

	server := httptest.NewServer(listener)
	t.Cleanup(server.Close)
	result, err := publish(server.URL)
	if err != nil || !result.Kept || result.KeptUntil == 0 || result.EnvelopeID == "" {
		t.Fatalf("Dispatch's publish for the role's first holder = %+v, %v; want 202 kept, naming the copy and kept_until", result, err)
	}
	kept := waitForKeptRoleMessage(t, client, role, result.EnvelopeID)
	if kept.Source != "dispatch" || kept.SourceSession != "" || kept.SourceEventID != bus.DispatchPostEventID(deliveredPost) {
		t.Fatalf("the kept copy is from %q, session %q, source event %q; want Dispatch's, naming no session and the post %q",
			kept.Source, kept.SourceSession, kept.SourceEventID, bus.DispatchPostEventID(deliveredPost))
	}
	awaitWake(t, wakes, wakeKindRole, role)

	const holder = "ses_post_first_holder"
	frames := startStandIn(t, testnats.URL(t), holder)
	if err := d.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"aside", "btw", "steer"}}); err != nil {
		t.Fatalf("register the role's first holder: %v", err)
	}
	claim := func(what string) {
		t.Helper()
		if claimed := postRoleClaim(roleSetHandler(d, "test-machine", logging.New("test")), holder, role, "", false); claimed.Code != http.StatusOK {
			t.Fatalf("%s claim of %s: status %d, body %s", what, role, claimed.Code, claimed.Body.String())
		}
	}
	claim("first")
	forwarded := expectFrames(t, frames, 1, 500*time.Millisecond)[0]
	if forwarded.EventID != result.EnvelopeID || forwarded.SourceEventID != kept.SourceEventID {
		t.Fatalf("the first holder received event %s (source event %s), want the kept %s", forwarded.EventID, forwarded.SourceEventID, result.EnvelopeID)
	}
	claim("second")
	select {
	case again := <-frames:
		t.Fatalf("a second claim of %s forwarded event %s again", role, again.EventID)
	case <-time.After(time.Second):
	}
}

// keptForDispatch publishes, through l's publish route on Dispatch's own principal, Dispatch's
// delivery of post to role in the delivery mode the payload names, and fails unless the route
// answered 202 kept. It returns the copy the role's kept lane holds for it.
func (r *hourRig) keptForDispatch(l *hourListener, role, mode string) laneCopy {
	r.t.Helper()
	useDispatchPrincipal(l.route, rigDispatchPrincipal)
	body, err := json.Marshal(listenerwire.PublishRequest{
		MessageBody: listenerwire.MessageBody{
			Source: "dispatch", Message: "A person's message for the role's first holder",
			Payload: new(`{"delivery":{"attempt":1,"mode":"` + mode + `"}}`), IdempotencyKey: deliveredPost.ID + ":" + mode,
		},
		Topic:        contracts.RoleTopicPrefix + role,
		Keep:         new(true),
		DispatchPost: &deliveredPost,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/publish", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(withVerifiedPrincipal(request.Context(), rigDispatchPrincipal))
	recorder := httptest.NewRecorder()
	publishHandler(l.route, logging.New("test")).ServeHTTP(recorder, request)
	var answer hourAnswer
	if recorder.Code != http.StatusAccepted || json.Unmarshal(recorder.Body.Bytes(), &answer) != nil || !answer.Kept {
		r.t.Fatalf("Dispatch's delivery to role %s: status %d, body %s; want 202 kept", role, recorder.Code, recorder.Body.String())
	}
	return r.copyOf(role, answer.EventID)
}

// assertDeliveryNoticeOf fails unless notices is the one expiry notice of held, the copy of
// Dispatch's delivery of deliveredPost on role's kept lane, naming the post as its source event
// and stored under the copy's own message id.
func assertDeliveryNoticeOf(t *testing.T, notices []storedNotice, role string, held laneCopy) {
	t.Helper()
	if len(notices) != 1 {
		t.Fatalf("the stream holds %d expiry notices for role %s, want the delivery's one: %+v", len(notices), role, notices)
	}
	got := notices[0].notice
	if got.EventID != held.item.EventID || got.SourceEventID != bus.DispatchPostEventID(deliveredPost) || got.Role != role ||
		got.DedupeKey != held.item.DedupeKey || got.Sequence != held.sequence || got.KeptAt != held.stored.UnixMilli() {
		t.Fatalf("the expiry notice is %+v, want the copy %s at %d naming the post %q", got, held.item.EventID, held.sequence, bus.DispatchPostEventID(deliveredPost))
	}
	if post, ok := bus.ParseDispatchPostEventID(got.SourceEventID); !ok || post != deliveredPost {
		t.Fatalf("the expiry notice's source event %q names %+v (%t), want the post %+v", got.SourceEventID, post, ok, deliveredPost)
	}
	if want := bus.KeptExpiryMsgID(role, held.sequence); notices[0].stored.MsgID != want {
		t.Fatalf("the expiry notice is stored under message id %q, want %q", notices[0].stored.MsgID, want)
	}
}

// A copy of Dispatch's delivery of a post that the publish route kept names no source session, so
// the walk that ends it sends no dead letter: it tells Dispatch with one expiry notice on the
// role's expiry subject naming the copy and the post, under the copy's own message id, which
// Dispatch settles the delivery's attempt by. It does so at the end of the copy's hour, for a copy
// no walk reached for hours as for any, and inside the hour when the role's live holder does not
// take the copy's delivery mode, so the attempt does not wait queued for good. The copy is the
// listener's own (kept_by listener). At this change's base the route answers such a publish 404,
// keeping nothing, and a walk tells nobody of a listener copy that names no session.
func TestADispatchDeliverysKeptCopyEndsWithANoticeToDispatch(t *testing.T) {
	t.Run("a the copy's hour ends: one notice naming the post, and no dead letter", func(t *testing.T) {
		rig := newHourRig(t)
		const role = "hour-delivery-told"
		held := rig.keptForDispatch(rig.listeners[0], role, "btw")
		if held.item.SourceSession != "" {
			t.Fatalf("the kept copy names source session %q, want none", held.item.SourceSession)
		}
		rig.startSweeps()
		rig.letSweepsRun()
		if notices := rig.expiryNotices(role); len(notices) != 0 {
			t.Fatalf("inside the copy's hour the stream holds expiry notices %+v, want none", notices)
		}

		rig.moveClock(time.Hour + time.Minute)
		rig.waitPassed(role, held.sequence)
		rig.waitReported(role, expiryNoticeLine, 1)
		rig.letSweepsRun()

		assertDeliveryNoticeOf(t, rig.expiryNotices(role), role, held)
		if letters := rig.envoyLetters(); len(letters) != 0 {
			t.Fatalf("the stream holds dead letters %+v, want none", letters)
		}
		if stored := rig.count(expiryNoticesStored); stored != 1 {
			t.Fatalf("envoy_role_kept_expiry_notices_total counts %d stored, want one", stored)
		}
		if drops := rig.count(hourExpired); drops != 1 {
			t.Fatalf("%s is %d, want the one copy, kept by the listener", hourExpired, drops)
		}
		if lines := rig.lines(expiryNoticeLine, map[string]any{"role": role, "event_id": held.item.EventID}); len(lines) != 1 || lines[0]["kept_by"] != keptByListener {
			t.Fatalf("the notice lines are %v, want one naming kept_by %s", lines, keptByListener)
		}
	})

	t.Run("b a copy no walk reached until its hour had ended more than an hour before is told to Dispatch all the same", func(t *testing.T) {
		rig := newHourRig(t)
		const role = "hour-delivery-stale"
		held := rig.keptForDispatch(rig.listeners[0], role, "btw")
		// Every claim reads older than bus.KeptRoleClaimWindow under the clock moved 3 h on, so the
		// second listener's guard is held for the row and one walk judges the copy, as row (b) of
		// TestAnOutboxCopyWhoseHourEndsIsToldToDispatch holds it.
		release, taken := rig.listeners[1].cfg.roleKeepDrains.hold(context.Background(), role)
		if !taken {
			t.Fatalf("take role %s's handover guard on the second listener", role)
		}
		defer release()
		rig.moveClock(3 * time.Hour)
		rig.startSweeps()
		rig.waitPassed(role, held.sequence)
		rig.waitReported(role, expiryNoticeLine, 1)
		rig.letSweepsRun()

		assertDeliveryNoticeOf(t, rig.expiryNotices(role), role, held)
		if letters := rig.envoyLetters(); len(letters) != 0 {
			t.Fatalf("the stream holds dead letters %+v, want none", letters)
		}
		if drops := rig.count(hourExpiredStale); drops != 1 {
			t.Fatalf("%s is %d, want the stale copy counted once", hourExpiredStale, drops)
		}
	})

	t.Run("c a live holder that does not take the copy's delivery mode: one notice, nothing forwarded, no dead letter", func(t *testing.T) {
		rig := newHourRig(t)
		const (
			role   = "hour-delivery-mode"
			holder = "ses_hour_delivery_mode_holder"
		)
		held := rig.keptForDispatch(rig.listeners[0], role, "steer")
		rig.startSweeps()
		rig.claim(rig.listeners[1], holder, role,
			session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}})
		rig.waitPassed(role, held.sequence)
		rig.waitReported(role, expiryNoticeLine, 1)
		rig.letSweepsRun()

		assertDeliveryNoticeOf(t, rig.expiryNotices(role), role, held)
		if lines := rig.lines(hourModeLine, map[string]any{"role": role, "event_id": held.item.EventID}); len(lines) != 1 {
			t.Fatalf("the copy's refusal for its delivery mode is logged %d times, want once: %v", len(lines), lines)
		}
		if stored := rig.envelopesOn(contracts.LegacyAgentSubject(holder)); len(stored) != 0 {
			t.Fatalf("the holder's agent subject holds %+v, want nothing", stored)
		}
		if letters := rig.envoyLetters(); len(letters) != 0 {
			t.Fatalf("the stream holds dead letters %+v, want none", letters)
		}
		if drops := rig.count(hourExpired); drops != 0 {
			t.Fatalf("%s is %d, want no expired copy: the copy ended inside its hour", hourExpired, drops)
		}
	})
}
