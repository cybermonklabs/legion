package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	dispatchapi "github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	dispatchstore "github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/googleidentity"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// This file is the listener's half of the check c-dispatch-role-route: Dispatch's sends for a
// route to a role, through the real Dispatch API and the real listener behind its real API auth,
// over one NATS and one Postgres (the cross-service rig of dispatch_resolution_test.go). Dispatch
// presents its own projected service-account token, which a local issuer mints and the listener
// verifies; the listener takes a send's role mark from that principal alone. The reply routes'
// rows are TestAReplyToARoleRouteAttemptComesFromTheRolesHolder in internal/dispatch/api.

const (
	// rigDispatchPrincipal is Dispatch's own service account, the principal the listener takes a
	// role mark from (ENVOY_DISPATCH_PRINCIPAL).
	rigDispatchPrincipal = "system:serviceaccount:dispatch:example-dispatch"
	// rigSharedToken is the listener's shared bearer, ENVOY_API_TOKEN.
	rigSharedToken = "listener-shared-token"
)

// roleRouteRig is the dispatchRig with the listener Dispatch calls behind apiAuth: the shared
// token, a projected-token verifier over a local issuer admitting Dispatch's account and one
// other service's, and broker tokens over a stub broker. Dispatch presents its account token from
// tokenFile. Every send that reaches the listener passes sends first.
type roleRouteRig struct {
	*dispatchRig
	issuer clusterIssuer
	// listener is the cold listener's /v1 behind apiAuth, as Dispatch reaches it, and url where
	// it is served.
	listener http.Handler
	url      string
	sends    *sendWatch
	broker   *ciphertokentest.StubBroker
	caller   *ciphertokentest.Caller
	// nats is the rig's own NATS account: testnats gives each subtest an account of its own, so
	// a session a row registers stands in on this one, where the rig's listeners are.
	nats string
}

// sendWatch stands in front of the listener's /v1 and records the answer of every POST
// /v1/messages/send, and runs, once, the step a row sets to happen after Dispatch resolved a
// send's target and before the send arrives.
type sendWatch struct {
	mu      sync.Mutex
	before  func()
	answers []watchedSend
}

type watchedSend struct {
	status int
	body   string
}

func (s *sendWatch) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/send" {
			next.ServeHTTP(w, r)
			return
		}
		s.mu.Lock()
		before := s.before
		s.before = nil
		s.mu.Unlock()
		if before != nil {
			before()
		}
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)
		s.mu.Lock()
		s.answers = append(s.answers, watchedSend{status: recorder.Code, body: recorder.Body.String()})
		s.mu.Unlock()
		for key, values := range recorder.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	})
}

// beforeNextSend sets the step the next send runs before it reaches the listener.
func (s *sendWatch) beforeNextSend(step func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.before = step
}

// answered is every send answer recorded so far.
func (s *sendWatch) answered() []watchedSend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]watchedSend(nil), s.answers...)
}

func newRoleRouteRig(t *testing.T) *roleRouteRig {
	t.Helper()
	warm, cold := trailingListeners(t)
	deliveryLog := startListenerDelivery(t, warm)
	useDispatchPrincipal(cold, rigDispatchPrincipal)

	broker := ciphertokentest.NewStubBroker(t)
	accountFile := filepath.Join(t.TempDir(), "listener-account-token")
	if err := os.WriteFile(accountFile, []byte("listener-account-token\n"), 0o600); err != nil {
		t.Fatalf("write the listener's own account token: %v", err)
	}
	tokens := newBrokerTokens(listenerBrokerConfig{BrokerURL: broker.Server.URL, AccountTokenFile: accountFile, Origin: listenerBrokerOrigin})
	if err := tokens.open(t.Context(), cold.client.Conn, 1); err != nil {
		t.Fatalf("open the spent-proof bucket: %v", err)
	}
	issuer := newClusterIssuer(t)
	routes := http.NewServeMux()
	registerV1Routes(routes, cold, rigMachineID, listenerRigLogger(t, "cold listener"))
	listener := apiAuth(listenerAPIAuth{
		token:    rigSharedToken,
		verifier: issuer.verifier,
		accounts: []string{rigDispatchPrincipal, listenerListedSubject},
		brokers:  tokens,
		logger:   listenerRigLogger(t, "cold listener auth"),
	}, routes)
	sends := &sendWatch{}
	server := httptest.NewServer(sends.wrap(listener))
	t.Cleanup(server.Close)

	rig := &roleRouteRig{
		dispatchRig: &dispatchRig{warm: warm, cold: cold, database: migratedDispatchDatabase(t), deliveryLog: deliveryLog},
		issuer:      issuer, listener: listener, url: server.URL, sends: sends,
		broker: broker, caller: ciphertokentest.NewCaller(t), nats: testnats.URL(t),
	}
	tokenFile := rig.accountTokenFile(t, issuer.token(t, rigDispatchPrincipal))
	rig.dispatch = roleRouteDispatchMux(t, rig.database, server.URL, warm.client, func(input *dispatchapi.DepsInput) {
		presentAccountToken(input, tokenFile)
	})
	return rig
}

// register registers sessionID through the warm listener on consumer delivery, with a stand-in of
// its own on the rig's NATS account taking its frames, as registerSession does; a registration of
// a session that ran before, the same id coming back, is one too.
func (rig *roleRouteRig) register(t *testing.T, sessionID, title string) *rigSession {
	t.Helper()
	standIn := startSessionStandIn(t, rig.nats, sessionID, answerDone)
	body, err := json.Marshal(map[string]any{
		"session_id": sessionID, "self_subscribed": true, "topics": []string{}, "title": title,
		"dir": "/test/" + sessionID, "capabilities": []string{"aside", "btw", "steer"},
		"delivery": map[string]any{"consumer": true, "epoch": "rig-" + id.New()},
	})
	if err != nil {
		t.Fatalf("marshal %s's registration: %v", sessionID, err)
	}
	recorder := httptest.NewRecorder()
	subscribeHandler(rig.warm, rigMachineID, listenerRigLogger(t, "warm listener")).
		ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"mode":"consumer"`) {
		t.Fatalf("register %s on consumer delivery: status %d %s", sessionID, recorder.Code, recorder.Body.String())
	}
	return &rigSession{id: sessionID, title: title, standIn: standIn}
}

// accountTokenFile writes token into a file of its own, as the kubelet projects Dispatch's
// service-account token, and returns its path.
func (rig *roleRouteRig) accountTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dispatch-account-token")
	rotateAccountToken(t, path, token)
	return path
}

// rotateAccountToken replaces the token in path at once, as the kubelet rewrites a projected
// token before it expires.
func rotateAccountToken(t *testing.T, path, token string) {
	t.Helper()
	next := path + ".next"
	if err := os.WriteFile(next, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the account token: %v", err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatalf("replace the account token: %v", err)
	}
}

// roleRouteDispatchMux is the real Dispatch API over database, built as cmd/dispatch builds it,
// calling the listener at envoyURL with the credential configure gives it, and queueing a
// person's targeted message for a session no store of the listener's holds through publisher.
func roleRouteDispatchMux(
	t *testing.T, database *dispatchstore.Store, envoyURL string, publisher *bus.Client, configure func(*dispatchapi.DepsInput),
) http.Handler {
	t.Helper()
	tokens, err := auth.ParseSharedAgentTokens(rigAgentToken)
	if err != nil {
		t.Fatalf("parse the shared agent token: %v", err)
	}
	people := dispatchstore.NewPgPeopleStore(database.Pool, auth.SingleSigningKey("signing-key"), nil)
	broker := events.NewBroker()
	caller := identity.HeaderIdentity{Header: rigUserHeader, People: people}
	documents := docs.New(docs.Deps{Store: database, Events: broker, Identity: caller, ServerURL: "https://dispatch.example", Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the document service down: %v", err)
		}
	})
	input := dispatchapi.DepsInput{
		Store: database, Identity: caller, AgentTokens: tokens, ServerURL: "https://dispatch.example",
		EnvoyURL: envoyURL, Docs: documents, Events: broker, AgentPublisher: publisher,
	}
	configure(&input)
	deps, err := dispatchapi.NewDeps(input)
	if err != nil {
		t.Fatalf("new Dispatch API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(dispatchapi.Register(mux, t.Context(), deps).Wait)
	return mux
}

// dispatchCall makes one request of the Dispatch mux handler, carrying header.
func dispatchCall(t *testing.T, handler http.Handler, method, target string, body any, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the %s %s body: %v", method, target, err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(header, value)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// rigMessage is a targeted message as Dispatch's create answers it, with its attempts.
type rigMessage struct {
	ID         string `json:"id"`
	Deliveries []struct {
		SessionID string  `json:"session_id"`
		State     string  `json:"state"`
		Error     *string `json:"error"`
	} `json:"deliveries"`
}

// roleMessage has the person send body to role through handler, as a message on issue key, and
// returns it once its first attempt has settled (a targeted message's create sends before it
// answers).
func dispatchRoleMessage(t *testing.T, handler http.Handler, key, role, body string) rigMessage {
	t.Helper()
	created := dispatchCall(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/messages",
		map[string]any{"body": body, "target": "role:" + role, "delivery": rigDeliveryMode}, rigUserHeader, rigHuman)
	if created.Code != http.StatusCreated {
		t.Fatalf("the message to role:%s: status=%d body=%s", role, created.Code, created.Body.String())
	}
	message := decodeRigBody[rigMessage](t, created)
	if len(message.Deliveries) != 1 {
		t.Fatalf("the message to role:%s has attempts %+v, want one", role, message.Deliveries)
	}
	return message
}

// roleMention has the person post body on issue key mentioning role, and returns the comment and
// its mention's first attempt once that attempt has settled.
func (rig *roleRouteRig) roleMention(t *testing.T, key, role, body string) (string, rigDelivery) {
	t.Helper()
	created := rig.asHuman(t, http.MethodPost, "/api/v1/issues/"+key+"/comments", map[string]any{
		"body": body, "delivery": rigDeliveryMode, "mentions": []map[string]any{{"target": "role:" + role}},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("the comment mentioning role:%s: status=%d body=%s", role, created.Code, created.Body.String())
	}
	comment := decodeRigBody[rigComment](t, created)
	deadline := time.Now().Add(20 * time.Second)
	for {
		read := rig.asHuman(t, http.MethodGet, "/api/v1/comments/"+comment.ID, nil)
		stored := decodeRigBody[struct {
			Comment rigComment `json:"comment"`
		}](t, read).Comment
		if len(stored.Deliveries) == 1 && stored.Deliveries[0].State != "pending" {
			return comment.ID, stored.Deliveries[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mention of role:%s has not settled: %#v", role, stored.Deliveries)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// claimRoleAs has sessionID claim role outright through the warm listener's claim route, as a
// session's plugin does, and answers what went wrong, "" when the claim landed. It returns rather
// than failing the test, since a row also runs it from the listener's own goroutine.
func (rig *roleRouteRig) claimRoleAs(sessionID, role string) string {
	if recorder := postRoleClaim(roleSetHandler(rig.warm, rigMachineID, rig.warm.delivery.logger), sessionID, role, "", false); recorder.Code != http.StatusOK {
		return fmt.Sprintf("%s claims %s: status %d %s", sessionID, role, recorder.Code, recorder.Body.String())
	}
	return ""
}

func (rig *roleRouteRig) claim(t *testing.T, sessionID, role string) {
	t.Helper()
	if failure := rig.claimRoleAs(sessionID, role); failure != "" {
		t.Fatal(failure)
	}
}

// crash stops a session as a process that died does: its relay subscription goes with its
// connection, and its registration ages past the live window, with nothing else of it changed,
// so the listener still holds its consumer and the role's claim still names it.
func (rig *roleRouteRig) crash(t *testing.T, s *rigSession) {
	t.Helper()
	s.standIn.close()
	sessions, err := rig.warm.client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	stored, err := sessions.Get(s.id)
	if err != nil {
		t.Fatalf("read %s's registration: %v", s.id, err)
	}
	var entry session.SessionEntry
	if err := json.Unmarshal(stored.Value(), &entry); err != nil {
		t.Fatalf("decode %s's registration: %v", s.id, err)
	}
	entry.UpdatedAt = time.Now().Add(-(session.LiveWindow + time.Minute)).UnixMilli()
	value, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode %s's registration: %v", s.id, err)
	}
	if _, err := sessions.Put(s.id, value); err != nil {
		t.Fatalf("age %s's registration: %v", s.id, err)
	}
}

// received collects the frames that reach s for within, and answers those carrying each of texts:
// it stops two seconds after every text has arrived once, the while a second copy would take.
func received(s *rigSession, within time.Duration, texts ...string) map[string][]relayedFrame {
	frames := map[string][]relayedFrame{}
	deadline := time.After(within)
	var settle <-chan time.Time
	for {
		select {
		case frame := <-s.standIn.frames:
			for _, text := range texts {
				if strings.Contains(string(frame.msg.Data), text) {
					frames[text] = append(frames[text], frame)
				}
			}
			if settle == nil && len(frames) == len(texts) {
				settle = time.After(2 * time.Second)
			}
		case <-deadline:
			return frames
		case <-settle:
			return frames
		}
	}
}

// receivedOnce reports, with Errorf so the row's replies still run, each of texts that did not
// reach s exactly once, and each copy that does not carry role as its mark when role is set.
func receivedOnce(t *testing.T, s *rigSession, role string, frames map[string][]relayedFrame, texts ...string) {
	t.Helper()
	for _, text := range texts {
		got := frames[text]
		if len(got) != 1 {
			t.Errorf("%s received %d copies of %q, want it once", s.id, len(got), text)
			continue
		}
		if role != "" && got[0].item.Role != role {
			t.Errorf("%s received %q marked with role %q, want %q", s.id, text, got[0].item.Role, role)
		}
	}
}

// reply posts sessionID's reply callback, a body or an error, on attempt 1 of a message or a
// comment ("messages" or "comments", id), through Dispatch.
func (rig *roleRouteRig) reply(t *testing.T, kind, id, sessionID string, reply map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	reply["actor"] = map[string]any{"kind": "session", "id": sessionID}
	reply["attempt"] = 1
	return rig.asSession(t, http.MethodPost, "/api/v1/"+kind+"/"+id+"/reply", reply)
}

// listenerSend is one POST /v1/messages/send made straight to the listener Dispatch calls, as a
// Dispatch send with role set, presenting authorize; connectedOnly asks for a running session.
func listenerSend(handler http.Handler, target, role, key, authorize string, connectedOnly bool) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"target_session":%q,"source":"dispatch","message":"from %s","payload":"{}","idempotency_key":%q,"role":%q,"connected_only":%t}`, target, key, key, role, connectedOnly)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/send", strings.NewReader(body))
	request.Host = "127.0.0.1:9020"
	request.Header.Set("Content-Type", "application/json")
	if authorize != "" {
		request.Header.Set("Authorization", authorize)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// refusedForRole fails the test unless response is the 403 naming the field role.
func refusedForRole(t *testing.T, what string, response *httptest.ResponseRecorder) {
	t.Helper()
	var refusal listenerwire.APIError
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusForbidden || len(refusal.Expected) != 1 || refusal.Expected[0] != "role" {
		t.Fatalf("%s: status %d body %s, want 403 naming the field role", what, response.Code, response.Body.String())
	}
}

// Dispatch sends a mention and a message for a route to role R whose holder is past the live
// window; each goes to the holder's own session, marked with R. The rows are c-dispatch-role-route's
// (1), (2), (5) and (6) and one more, (7); (3) and (4) are the reply routes' alone, in
// internal/dispatch/api.
func TestADispatchMessageForARoleReachesTheSessionThatTakesTheRole(t *testing.T) {
	rig := newRoleRouteRig(t)
	key := rig.issueKey(t, "Role-route sends")

	t.Run("(1) a different session takes R: it receives each once, and its reply and its error callback are accepted", func(t *testing.T) {
		const role = "route-takeover"
		holder := rig.register(t, "ses_route_takeover_a", "Holder A")
		rig.claim(t, holder.id, role)
		rig.crash(t, holder)
		message := dispatchRoleMessage(t, rig.dispatch, key, role, "Takeover message one")
		failing := dispatchRoleMessage(t, rig.dispatch, key, role, "Takeover message two")
		comment, attempt := rig.roleMention(t, key, role, "Takeover mention one")
		failingComment, _ := rig.roleMention(t, key, role, "Takeover mention two")
		for _, delivery := range append(message.Deliveries, failing.Deliveries...) {
			if delivery.SessionID != holder.id || delivery.Error != nil {
				t.Fatalf("a message to role:%s went %+v, want to %s", role, delivery, holder.id)
			}
		}
		if attempt.SessionID == nil || *attempt.SessionID != holder.id || attempt.Error != nil {
			t.Fatalf("the mention of role:%s went %+v, want to %s", role, attempt, holder.id)
		}

		next := rig.register(t, "ses_route_takeover_b", "Holder B")
		rig.claim(t, next.id, role)
		texts := []string{"Takeover message one", "Takeover message two", "Takeover mention one", "Takeover mention two"}
		receivedOnce(t, next, role, received(next, 20*time.Second, texts...), texts...)

		if answered := rig.reply(t, "messages", message.ID, next.id, map[string]any{"body": "B answers message one."}); answered.Code != http.StatusCreated {
			t.Errorf("B's reply to the message: status=%d body=%s, want 201", answered.Code, answered.Body.String())
		}
		if failed := rig.reply(t, "messages", failing.ID, next.id, map[string]any{"error": "B cannot run message two"}); failed.Code != http.StatusOK {
			t.Errorf("B's error callback on the message: status=%d body=%s, want 200", failed.Code, failed.Body.String())
		}
		if answered := rig.reply(t, "comments", comment, next.id, map[string]any{"body": "B answers mention one."}); answered.Code != http.StatusCreated {
			t.Errorf("B's reply to the mention: status=%d body=%s, want 201", answered.Code, answered.Body.String())
		}
		if failed := rig.reply(t, "comments", failingComment, next.id, map[string]any{"error": "B cannot run mention two"}); failed.Code != http.StatusOK {
			t.Errorf("B's error callback on the mention: status=%d body=%s, want 200", failed.Code, failed.Body.String())
		}
	})

	t.Run("(2) no takeover, the holder continues: it receives each once, and its reply is accepted", func(t *testing.T) {
		const role = "route-continues"
		holder := rig.register(t, "ses_route_continues", "Holder")
		rig.claim(t, holder.id, role)
		rig.crash(t, holder)
		message := dispatchRoleMessage(t, rig.dispatch, key, role, "Continues message")
		comment, _ := rig.roleMention(t, key, role, "Continues mention")

		resumed := rig.register(t, holder.id, "Holder")
		texts := []string{"Continues message", "Continues mention"}
		receivedOnce(t, resumed, "", received(resumed, 20*time.Second, texts...), texts...)
		if answered := rig.reply(t, "messages", message.ID, holder.id, map[string]any{"body": "Back, and answering."}); answered.Code != http.StatusCreated {
			t.Errorf("the holder's reply to the message: status=%d body=%s, want 201", answered.Code, answered.Body.String())
		}
		if answered := rig.reply(t, "comments", comment, holder.id, map[string]any{"body": "Back, and answering the mention."}); answered.Code != http.StatusCreated {
			t.Errorf("the holder's reply to the mention: status=%d body=%s, want 201", answered.Code, answered.Body.String())
		}
	})

	t.Run("(5) only Dispatch's own principal may mark a send with a role, and its refreshed token is taken", func(t *testing.T) {
		const role = "route-principal"
		holder := rig.register(t, "ses_route_principal", "Holder")
		rig.claim(t, holder.id, role)

		const session = "ses_route_principal_caller"
		rig.broker.Says("session-broker-token", sessionAnswer(session, "ada@example.com", rig.caller.JKT()))
		dpop := brokerTokenRequest(t, rig.caller, http.MethodPost, "/v1/messages/send", "session-broker-token",
			fmt.Sprintf(`{"target_session":%q,"source":"dispatch","message":"from a session","payload":"{}","idempotency_key":"principal-broker","role":%q}`, holder.id, role))
		fromSession := httptest.NewRecorder()
		rig.listener.ServeHTTP(fromSession, dpop)
		refusedForRole(t, "a send with role from a session's broker token", fromSession)
		refusedForRole(t, "a send with role on the shared token", listenerSend(rig.listener, holder.id, role, "principal-shared", "Bearer "+rigSharedToken, false))
		refusedForRole(t, "a send with role from another service's principal",
			listenerSend(rig.listener, holder.id, role, "principal-other", "Bearer "+rig.issuer.token(t, listenerListedSubject), false))
		open := apiAuth(listenerAPIAuth{logger: listenerRigLogger(t, "open listener")}, v1Routes(t, rig.cold))
		refusedForRole(t, "a send with role and no credential to an open listener", listenerSend(open, holder.id, role, "principal-none", "", false))
		unconfigured := *rig.cold
		useDispatchPrincipal(&unconfigured, "")
		refusedForRole(t, "a send with role from Dispatch's principal to a listener that names no Dispatch principal",
			listenerSend(apiAuth(listenerAPIAuth{verifier: rig.issuer.verifier, logger: listenerRigLogger(t, "unconfigured listener auth")}, v1Routes(t, &unconfigured)),
				holder.id, role, "principal-unconfigured", "Bearer "+rig.issuer.token(t, rigDispatchPrincipal), false))
		if accepted := listenerSend(rig.listener, holder.id, role, "principal-dispatch", "Bearer "+rig.issuer.token(t, rigDispatchPrincipal), false); accepted.Code != http.StatusOK {
			t.Fatalf("a send with role from Dispatch's principal: status %d body %s, want 200", accepted.Code, accepted.Body.String())
		}

		// Dispatch's own client and token source: the first token is minted to expire in 15 s,
		// and the file is rewritten with a new one after the first send, as the kubelet rewrites a
		// projected token before it expires. The next send, made once the first token expired,
		// is accepted on the new one.
		claims := rig.issuer.issuer.Claims(rigDispatchPrincipal, listenerTestAudience)
		expiry := time.Now().Add(15 * time.Second)
		claims["exp"] = expiry.Unix()
		tokenFile := rig.accountTokenFile(t, rig.issuer.issuer.Mint(t, rig.issuer.key, claims))
		refreshing := roleRouteDispatchMux(t, rig.database, rig.url, rig.warm.client, func(input *dispatchapi.DepsInput) {
			presentAccountToken(input, tokenFile)
		})
		first := dispatchRoleMessage(t, refreshing, key, role, "Principal message before the rotation")
		if got := first.Deliveries[0]; got.State != "sent" || got.SessionID != holder.id || got.Error != nil {
			t.Fatalf("the send on the first token = %+v, want sent to %s", got, holder.id)
		}
		rotateAccountToken(t, tokenFile, rig.issuer.token(t, rigDispatchPrincipal))
		time.Sleep(time.Until(expiry.Add(time.Second)))
		second := dispatchRoleMessage(t, refreshing, key, role, "Principal message after the rotation")
		if got := second.Deliveries[0]; got.State != "sent" || got.SessionID != holder.id || got.Error != nil {
			t.Fatalf("the send after the first token expired = %+v, want sent to %s on the rotated token", got, holder.id)
		}
		texts := []string{"Principal message before the rotation", "Principal message after the rotation"}
		receivedOnce(t, holder, role, received(holder, 20*time.Second, texts...), texts...)
	})

	t.Run("(6) Dispatch resolved holder A and B takes R before the send arrives: it is kept for R, answered 202 kept, and B receives it once", func(t *testing.T) {
		const role = "route-moved"
		holder := rig.register(t, "ses_route_moved_a", "Holder A")
		rig.claim(t, holder.id, role)
		next := rig.register(t, "ses_route_moved_b", "Holder B")
		claimed := make(chan string, 1)
		rig.sends.beforeNextSend(func() { claimed <- rig.claimRoleAs(next.id, role) })
		before := len(rig.sends.answered())

		message := dispatchRoleMessage(t, rig.dispatch, key, role, "Moved message")
		if failure := <-claimed; failure != "" {
			t.Fatal(failure)
		}
		answers := rig.sends.answered()[before:]
		var answer struct {
			Recipient string `json:"recipient"`
			Kept      bool   `json:"kept"`
			KeptUntil int64  `json:"kept_until"`
		}
		if len(answers) != 1 || json.Unmarshal([]byte(answers[0].body), &answer) != nil ||
			answers[0].status != http.StatusAccepted || !answer.Kept || answer.KeptUntil == 0 || answer.Recipient != holder.id {
			t.Errorf("the listener answered the send %+v, want one 202 kept for role %s naming %s and kept_until", answers, role, holder.id)
		}
		if got := message.Deliveries[0]; got.State != "sent" || got.SessionID != holder.id || got.Error != nil {
			t.Errorf("the attempt = %+v, want sent, naming %s, the holder Dispatch resolved", got, holder.id)
		}
		receivedOnce(t, next, role, received(next, 20*time.Second, "Moved message"), "Moved message")
		if got := received(holder, 2*time.Second, "Moved message")["Moved message"]; len(got) != 0 {
			t.Errorf("A, which no longer holds %s, received the message %d times, want none", role, len(got))
		}
	})

	t.Run("(7) Dispatch with no identity refuses a send for R, naming the setting, and sends nothing unmarked; with its identity the same send is made", func(t *testing.T) {
		const role = "route-no-identity"
		holder := rig.register(t, "ses_route_no_identity", "Holder")
		rig.claim(t, holder.id, role)
		shared := roleRouteDispatchMux(t, rig.database, rig.url, rig.warm.client, func(input *dispatchapi.DepsInput) {
			input.EnvoyToken = rigSharedToken
		})
		refused := dispatchRoleMessage(t, shared, key, role, "No-identity message")
		got := refused.Deliveries[0]
		if got.State != "failed" || got.Error == nil || !strings.Contains(*got.Error, "ENVOY_ACCOUNT_TOKEN_FILE") ||
			!strings.Contains(*got.Error, "Dispatch's identity at the Envoy listener") {
			t.Fatalf("the send for %s from a Dispatch with no identity = %+v, want failed, its reason naming ENVOY_ACCOUNT_TOKEN_FILE and the README section", role, got)
		}
		if frames := received(holder, 3*time.Second, "No-identity message")["No-identity message"]; len(frames) != 0 {
			t.Fatalf("the holder received %d copies of a send Dispatch refused, the first marked %q; want none", len(frames), roleOf(frames))
		}
		sent := dispatchRoleMessage(t, rig.dispatch, key, role, "Identity message")
		if got := sent.Deliveries[0]; got.State != "sent" || got.SessionID != holder.id || got.Error != nil {
			t.Fatalf("control: the send for %s from a Dispatch with its identity = %+v, want sent to %s", role, got, holder.id)
		}
		receivedOnce(t, holder, role, received(holder, 20*time.Second, "Identity message"), "Identity message")
	})

	t.Run("(8) Dispatch resolved holder A and R's claim is gone before the send arrives: it is kept for R, answered 202 kept, and R's wake is reported", func(t *testing.T) {
		const role = "route-released"
		holder := rig.register(t, "ses_route_released_a", "Holder A")
		rig.claim(t, holder.id, role)
		wakesCtx, stopWakes := context.WithCancel(context.Background())
		t.Cleanup(stopWakes)
		rig.cold.wakes = newWakeReporter(wakesCtx, rig.cold.client)
		t.Cleanup(func() { rig.cold.wakes = nil })
		wakes, err := rig.cold.client.Conn.SubscribeSync(bus.LegionWakeTopic)
		if err != nil {
			t.Fatalf("subscribe to the wake reports: %v", err)
		}
		t.Cleanup(func() { _ = wakes.Unsubscribe() })
		claims, err := rig.warm.client.JS().KeyValue(bus.RoleClaimBucket)
		if err != nil {
			t.Fatalf("open the role bucket: %v", err)
		}
		// The reaper's delete of a released pod's claim, between Dispatch's lookup and its send.
		released := make(chan error, 1)
		rig.sends.beforeNextSend(func() { released <- claims.Delete(role) })
		before := len(rig.sends.answered())

		message := dispatchRoleMessage(t, rig.dispatch, key, role, "Released message")
		if err := <-released; err != nil {
			t.Fatalf("delete role %s's claim: %v", role, err)
		}
		answers := rig.sends.answered()[before:]
		var answer struct {
			Kept      bool  `json:"kept"`
			KeptUntil int64 `json:"kept_until"`
		}
		if len(answers) != 1 || json.Unmarshal([]byte(answers[0].body), &answer) != nil ||
			answers[0].status != http.StatusAccepted || !answer.Kept || answer.KeptUntil == 0 {
			t.Fatalf("the listener answered the send %+v, want one 202 kept for role %s", answers, role)
		}
		if got := message.Deliveries[0]; got.State != "sent" || got.SessionID != holder.id {
			t.Fatalf("the attempt = %+v, want sent, naming %s, the holder Dispatch resolved", got, holder.id)
		}
		awaitWake(t, wakes, wakeKindRole, role)
	})

	t.Run("(9) a send for R that asks connected_only, whose claim names another session, is refused 409 and keeps nothing", func(t *testing.T) {
		const role = "route-connected-only"
		holder := rig.register(t, "ses_route_connected_only_a", "Holder A")
		rig.claim(t, holder.id, role)
		next := rig.register(t, "ses_route_connected_only_b", "Holder B")
		rig.claim(t, next.id, role)
		kept := func() int {
			t.Helper()
			return len(storedEnvelopes(t, rig.warm.client, bus.KeptRoleSubject(role)))
		}
		send := func(key string, connectedOnly bool) *httptest.ResponseRecorder {
			return listenerSend(rig.listener, holder.id, role, key, "Bearer "+rig.issuer.token(t, rigDispatchPrincipal), connectedOnly)
		}

		refused := send("connected-only-moved", true)
		if refused.Code != http.StatusConflict {
			t.Fatalf("a connected_only send to %s for %s, whose claim names %s: status %d body %s, want 409", holder.id, role, next.id, refused.Code, refused.Body.String())
		}
		if got := kept(); got != 0 {
			t.Fatalf("role %s's kept lane holds %d copies after the refused connected_only send; want none", role, got)
		}
		if accepted := send("connected-only-control", false); accepted.Code != http.StatusAccepted {
			t.Fatalf("control: the same send without connected_only: status %d body %s, want 202 kept", accepted.Code, accepted.Body.String())
		}
		if got := kept(); got != 1 {
			t.Fatalf("control: role %s's kept lane holds %d copies after the send without connected_only; want 1", role, got)
		}
	})

	t.Run("(10) Dispatch's token file holds no token when a send for R starts and a live one moments later: the attempt is tried again and sent, marked", func(t *testing.T) {
		const role, text = "route-token-late", "Message sent once the token is there"
		holder := rig.register(t, "ses_route_token_late", "Holder")
		rig.claim(t, holder.id, role)
		path := filepath.Join(t.TempDir(), "dispatch-account-token")
		late := roleRouteDispatchMux(t, rig.database, rig.url, rig.warm.client, func(input *dispatchapi.DepsInput) {
			presentAccountToken(input, path)
		})
		token := rig.issuer.token(t, rigDispatchPrincipal)
		written := make(chan error, 1)
		// The kubelet's first write of the projected token, after the send began: the file is
		// written whole and renamed into place, as rotateAccountToken does.
		go func() {
			time.Sleep(600 * time.Millisecond)
			next := path + ".next"
			if err := os.WriteFile(next, []byte(token+"\n"), 0o600); err != nil {
				written <- err
				return
			}
			written <- os.Rename(next, path)
		}()
		message := dispatchRoleMessage(t, late, key, role, text)
		if err := <-written; err != nil {
			t.Fatalf("write Dispatch's token: %v", err)
		}
		if got := message.Deliveries[0]; got.State != "sent" || got.SessionID != holder.id || got.Error != nil {
			t.Fatalf("the attempt begun before the token file held a token = %+v, want sent to %s once the file held one", got, holder.id)
		}
		frames := received(holder, 20*time.Second, text)[text]
		if len(frames) != 1 || frames[0].item.Role != role {
			t.Fatalf("the holder received %d copies, the first marked %q; want one copy marked %s", len(frames), roleOf(frames), role)
		}
	})

	t.Run("(11) Dispatch on its Google identity whose metadata server does not answer for a moment: the attempt is tried again and sent, marked", func(t *testing.T) {
		const role, text = "route-metadata-outage", "Message sent once the metadata server answers"
		holder := rig.register(t, "ses_route_metadata_outage", "Holder")
		rig.claim(t, holder.id, role)
		// The metadata server drops its first four connections, as one that restarts does, and
		// then mints the identity token the rig's listener verifies as Dispatch's principal.
		token := rig.issuer.token(t, rigDispatchPrincipal)
		var asked atomic.Int32
		metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if asked.Add(1) <= 4 {
				if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			w.Header().Set("Metadata-Flavor", "Google")
			switch r.URL.Path {
			case "/computeMetadata/v1/instance/service-accounts/default/identity":
				_, _ = w.Write([]byte(token))
			case "/computeMetadata/v1/instance/service-accounts/default/email":
				_, _ = w.Write([]byte("dispatch@example-project.iam.gserviceaccount.com"))
			default:
				http.NotFound(w, r)
			}
		}))
		defer metadata.Close()
		t.Setenv(googleidentity.MetadataHostVar, strings.TrimPrefix(metadata.URL, "http://"))
		google := roleRouteDispatchMux(t, rig.database, rig.url, rig.warm.client, func(input *dispatchapi.DepsInput) {
			input.EnvoyAccountTokenFile = ""
			input.EnvoyGoogleAudience = "envoy-listener"
		})
		message := dispatchRoleMessage(t, google, key, role, text)
		if got := message.Deliveries[0]; got.State != "sent" || got.SessionID != holder.id || got.Error != nil {
			t.Fatalf("the attempt begun while the metadata server did not answer = %+v, want sent to %s once it answered", got, holder.id)
		}
		if n := asked.Load(); n <= 4 {
			t.Fatalf("the metadata server was asked %d times, want more than the 4 it dropped", n)
		}
		frames := received(holder, 20*time.Second, text)[text]
		if len(frames) != 1 || frames[0].item.Role != role {
			t.Fatalf("the holder received %d copies, the first marked %q; want one copy marked %s", len(frames), roleOf(frames), role)
		}
	})
}

// roleOf is the role mark of the first of frames, "" when there is none.
func roleOf(frames []relayedFrame) string {
	if len(frames) == 0 {
		return ""
	}
	return frames[0].item.Role
}

// v1Routes is d's /v1 mux, with no API auth in front of it.
func v1Routes(t *testing.T, d *listenerDeps) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	registerV1Routes(mux, d, rigMachineID, listenerRigLogger(t, "listener routes"))
	return mux
}
