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
// and stored under the copy's own message id, and saying that the copy's forward may have reached
// a holder exactly when forwardUnconfirmed is set.
func assertDeliveryNoticeOf(t *testing.T, notices []storedNotice, role string, held laneCopy, forwardUnconfirmed bool) {
	t.Helper()
	if len(notices) != 1 {
		t.Fatalf("the stream holds %d expiry notices for role %s, want the delivery's one: %+v", len(notices), role, notices)
	}
	got := notices[0].notice
	if got.EventID != held.item.EventID || got.SourceEventID != bus.DispatchPostEventID(deliveredPost) || got.Role != role ||
		got.DedupeKey != held.item.DedupeKey || got.Sequence != held.sequence || got.KeptAt != held.stored.UnixMilli() {
		t.Fatalf("the expiry notice is %+v, want the copy %s at %d naming the post %q", got, held.item.EventID, held.sequence, bus.DispatchPostEventID(deliveredPost))
	}
	if got.ForwardUnconfirmed != forwardUnconfirmed {
		t.Fatalf("the expiry notice says forward_unconfirmed %t, want %t: %+v", got.ForwardUnconfirmed, forwardUnconfirmed, got)
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

		assertDeliveryNoticeOf(t, rig.expiryNotices(role), role, held, false)
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

		assertDeliveryNoticeOf(t, rig.expiryNotices(role), role, held, false)
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

		assertDeliveryNoticeOf(t, rig.expiryNotices(role), role, held, false)
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

// writeForwardClaim writes onto role's drain cursor the claim a walk leaves when it ends after it
// claimed the copy stored at seq to forward it and before it confirmed the forward: the stream may
// hold that forward, so the copy may have reached the session that held the role then.
func (r *hourRig) writeForwardClaim(role string, seq uint64) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	current, err := r.client().ReadKeptRoleCursor(ctx, role)
	if err != nil {
		r.t.Fatalf("read role %s's drain cursor: %v", role, err)
	}
	body, err := json.Marshal(bus.KeptRoleCursor{Kept: current.Cursor.Kept, Claiming: seq})
	if err != nil {
		r.t.Fatalf("encode the forward's claim: %v", err)
	}
	if _, err := r.client().PublishCursorAt(ctx, bus.KeptRoleCursorSubject(role), body, current.Sequence); err != nil {
		r.t.Fatalf("write the forward's claim of the copy at %d: %v", seq, err)
	}
}

// The notice of a copy of Dispatch's delivery says whether the copy's forward may have reached a
// holder (bus.KeptCopyExpiry.ForwardUnconfirmed), however many walks took the claim over before
// the one whose confirm landed, and whichever way the copy ended: a walk claimed it to forward it
// and no walk confirmed the forward, so the stream may hold that forward, and Dispatch must not
// tell a person who retries the delivery that nothing took it. The first walk's publish of the
// notice fails with an error that can pass, so a second walk takes the claim over; the claim both
// find names the copy's end and, for a copy whose forward was claimed, the qualifier
// (keptRoleDrop.claimReason). A delivery mode refusal's claim is read back as one, so the walk
// that takes it over reports no expired copy: nothing is counted on envoy_role_kept_expired_total
// and no drop line names the keep window, though the copy's hour had not ended when it was refused.
func TestADeliverysNoticeKeepsWhatItsClaimSaidWhenAnotherWalkTakesTheClaimOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		// refused is whether the role's live holder, which takes btw alone, refuses the copy's
		// steer, which ends the copy inside its hour; otherwise nobody holds the role and the
		// copy's hour ends.
		refused bool
		// forward is whether a walk claimed the copy to forward it and no walk confirmed that.
		forward bool
		// claim is the dead_letter value of the claim the notice's publish finds on the cursor.
		claim string
	}{
		{"the hour ends on a copy no walk claimed", false, false, "expired"},
		{"the hour ends on a copy whose forward was claimed", false, true, "expired_unconfirmed"},
		{"the holder refuses the mode of a copy no walk claimed", true, false, "delivery_mode"},
		{"the holder refuses the mode of a copy whose forward was claimed", true, true, "delivery_mode_unconfirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHourRig(t)
			const (
				role   = "hour-delivery-claimed"
				holder = "ses_hour_delivery_claimed_holder"
			)
			mode := "btw"
			if tc.refused {
				mode = "steer"
			}
			held := rig.keptForDispatch(rig.listeners[0], role, mode)
			if tc.forward {
				rig.writeForwardClaim(role, held.sequence)
			}
			seam := failingOnce(role, nil)
			seam.installExpiryNotices(t, rig)
			// Moving the clock makes every claim read older than bus.KeptRoleClaimWindow to the
			// listeners the moment it is written, which lets a walk take the claims over. It would
			// let the first listener's sweep take over the notice's claim while the second
			// listener's walk publishes, so the first listener's guard is held for the whole row:
			// both walks are the second listener's.
			release, taken := rig.listeners[0].cfg.roleKeepDrains.hold(context.Background(), role)
			if !taken {
				t.Fatalf("take role %s's handover guard on the first listener", role)
			}
			defer release()
			if tc.refused {
				rig.moveClock(30 * time.Minute)
			} else {
				rig.moveClock(time.Hour + time.Minute)
			}
			rig.startSweeps()
			if tc.refused {
				rig.claim(rig.listeners[1], holder, role,
					session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}})
			}
			rig.waitPassed(role, held.sequence)
			rig.waitReported(role, expiryNoticeLine, 1)
			rig.letSweepsRun()

			assertDeliveryNoticeOf(t, rig.expiryNotices(role), role, held, tc.forward)
			for i, call := range seam.assertTakenOver(t, "expiry notice", held, 0) {
				if call.cursor.DeadLetter != tc.claim {
					t.Fatalf("call %d of the notice's publish found the claim %+v, want its dead_letter to be %q", i+1, call.cursor, tc.claim)
				}
			}
			expired, stale := rig.count(hourExpired), rig.count(hourExpiredStale)
			drops := rig.lines(hourDropLine, map[string]any{"role": role})
			if tc.refused && (expired != 0 || stale != 0 || len(drops) != 0) {
				t.Fatalf("a delivery mode refusal taken over counts %d expired and %d stale copies and logs %d drop lines %v, want none: the copy ended inside its hour",
					expired, stale, len(drops), drops)
			}
			if !tc.refused && (expired != 1 || stale != 0 || len(drops) != 1) {
				t.Fatalf("an expired copy counts %d expired and %d stale copies and logs %d drop lines %v, want one expired copy and one line",
					expired, stale, len(drops), drops)
			}
		})
	}
}

// The claim a walk writes for the notice of a copy of Dispatch's delivery (keptRoleDrop.claimReason)
// is read back by the walk that takes it over (keptCopyClaimOf) as the end it was made for, and
// that walk judges the copy as the first did, whatever its age on its own clock and whoever holds
// the role now: the notice's reason is the delivery mode or the hour, a forward that may have
// reached a holder stays a notice that says so (bus.KeptCopyExpiry.ForwardUnconfirmed), and a
// delivery mode refusal is reported as no expired copy by the walk that takes its claim over.
func TestADeliverysNoticeClaimIsTakenOverForTheReasonItWasMade(t *testing.T) {
	t.Parallel()
	const role = "delivery-claimed-role"
	stored := bus.StoredMessage{Sequence: 7, Time: time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)}
	data, err := json.Marshal(contracts.Envelope{
		EventID: "evt_delivery", Source: "dispatch", SourceEventID: bus.DispatchPostEventID(deliveredPost),
		DedupeKey: bus.PublishDedupePrefix + "claimed", Payload: `{"delivery":{"mode":"steer"}}`,
	})
	if err != nil {
		t.Fatalf("encode the copy: %v", err)
	}
	stored.Data = data
	forward := bus.KeptRoleCursor{Kept: 6, Claiming: 7}
	const refuser = "ses_delivery_claim_holder"
	// judged is the judgement of a walk for holder - refuser, a live holder that takes btw alone,
	// or none - of the copy under cursor's claim, by a clock at after the keep.
	judged := func(holder string, cursor bus.KeptRoleCursor, at time.Duration) keptCopyVerdict {
		return judgeKeptRoleCopy(listenerDeliveryHandlerConfig{}, role, holder, session.SessionEntry{Capabilities: []string{"btw"}}, stored,
			keptCopyClaimOf(cursor, stored.Sequence), func() time.Time { return stored.Time.Add(at) }, func(string, string) bool { return false })
	}
	for _, tc := range []struct {
		name      string
		holder    string
		cursor    bus.KeptRoleCursor
		at        time.Duration
		reason    deadLetterReason
		forwarded bool
		// reported is whether the walk's own judgement reports the copy: an expired copy or the
		// refusal of a holder, and not a delivery mode claim taken over.
		reported bool
		// claim is the dead_letter value the claim of that notice carries.
		claim string
	}{
		{"a copy no walk claimed, read after its hour", "", bus.KeptRoleCursor{Kept: 6}, time.Hour + time.Minute, deadLetterExpired, false, true, "expired"},
		{"a forward's claim taken over after the hour", "", forward, time.Hour + time.Minute, deadLetterExpired, true, true, "expired_unconfirmed"},
		{"a delivery mode claim taken over after the hour", "", bus.KeptRoleCursor{Kept: 6, Claiming: 7, DeadLetter: "delivery_mode"}, 3 * time.Hour, deadLetterDeliveryMode, false, false, "delivery_mode"},
		{"a delivery mode claim of a forwarded copy taken over after the hour", "", bus.KeptRoleCursor{Kept: 6, Claiming: 7, DeadLetter: "delivery_mode_unconfirmed"}, 3 * time.Hour, deadLetterDeliveryMode, true, false, "delivery_mode_unconfirmed"},
		{"a copy no walk claimed, read inside its hour for a holder that does not take its delivery mode", refuser, bus.KeptRoleCursor{Kept: 6}, 30 * time.Minute, deadLetterDeliveryMode, false, true, "delivery_mode"},
		{"a forward's claim taken over inside the hour for a holder that does not take its delivery mode", refuser, forward, 30 * time.Minute, deadLetterDeliveryMode, true, true, "delivery_mode_unconfirmed"},
	} {
		first := judged(tc.holder, tc.cursor, tc.at)
		if first.step != keptCopyExpiryNotice || first.drop.reason != tc.reason || first.drop.forwarded != tc.forwarded || (first.report != nil) != tc.reported {
			t.Fatalf("%s: judged %+v, want a notice for %s with forwarded %t and a report: %t", tc.name, first, tc.reason, tc.forwarded, tc.reported)
		}
		if notice := keptCopyExpiry(role, stored, first.item, first.at, first.drop.forwarded); notice.ForwardUnconfirmed != tc.forwarded {
			t.Fatalf("%s: the notice says forward_unconfirmed %t, want %t", tc.name, notice.ForwardUnconfirmed, tc.forwarded)
		}
		claimed := bus.KeptRoleCursor{Kept: 6, Claiming: 7, DeadLetter: first.drop.claimReason()}
		if claimed.DeadLetter != tc.claim {
			t.Fatalf("%s: the notice's claim names %q, want %q", tc.name, claimed.DeadLetter, tc.claim)
		}
		for _, at := range []time.Duration{59 * time.Minute, 3 * time.Hour} {
			again := judged("", claimed, at)
			if again.step != keptCopyExpiryNotice || again.drop.reason != tc.reason || again.drop.forwarded != tc.forwarded ||
				again.drop.claimReason() != tc.claim || (again.report != nil) != (tc.reason == deadLetterExpired) {
				t.Fatalf("%s: its claim taken over by a clock %s after the keep is judged %+v, want a notice for %s with forwarded %t under the claim %q, reported only for an expired copy",
					tc.name, at, again, tc.reason, tc.forwarded, tc.claim)
			}
		}
	}
}
