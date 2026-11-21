package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/nats-io/jwt/v2"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// relayReplyControlVar names the guards TestASessionCannotAckAnotherSessionsFrameThroughTheCallout
// takes away, for a control run by hand. Unset, the test runs this tree as it is. "counter" takes
// away the reply token and the grant together, as they were before a frame's reply subject named
// its session and before a session's grant held its replies to its own id, and the test must fail
// naming session B's stream sequence. "counter-relay" takes away the token alone and "wide-grant"
// the grant alone; each of those must leave B's message pending and fail on the other guard's
// assertion.
const relayReplyControlVar = "ENVOY_RELAY_REPLY_CONTROL"

// No session can acknowledge a frame the listener relayed to another session. Two sessions, A and
// B, are each admitted through the real auth service (natscallout.Start) by a broker access token
// of their own and a proof of their own key (natscred.Session), so each holds the grant the
// service answers it with, and the real listener binary, a static user of the hub, relays one
// stored message to each.
//
// A asks for B's relay subject and the server refuses it by name. A then publishes done to every
// reply subject it can form from its own frame's: its own part of the reply subject (everything
// before the token) and B's part (the listener's inbox and B's id), each with the 32 tokens of a
// shared counter nearest A's own (counterNeighbours) and 1,000 random tokens as long as the
// listener's. Under its own part the server serves each one and the relay moves nothing, since no
// frame awaits that token under A's id; under B's part the server refuses each one by name. B's
// message is still pending 300 ms later, and B's own done on its own reply subject acknowledges
// it.
//
// Three guards close this together. The grant subscribes a session to its own relay subject
// alone, so A never sees B's frame or its token. The listener gives each frame a reply subject of
// the listener's inbox, the frame's session and 128 random bits, and moves a frame only on a reply
// naming that session. The grant lets a session publish replies under its own id alone. The
// control is relayReplyControlVar=counter, which builds the listener through a go build overlay
// with one shared counter for every frame's token (counterRelayOverlay) and grants A the shared
// session user's _INBOX.> publish (startWideningAuthService): A's done on a neighbouring counter
// then acknowledges B's message, and the test fails naming B's stream sequence.
func TestASessionCannotAckAnotherSessionsFrameThroughTheCallout(t *testing.T) {
	counterRelay, wideGrant := relayReplyControl(t)
	keys := testnats.NewCalloutKeys(t)
	listenerSeed, listenerKey := testnats.User(t)
	url := testnats.StartWithCallout(t, keys, testnats.StaticUser{NKey: listenerKey, Publish: []string{">"}, Subscribe: []string{">"}})
	a, b := "ses_a_"+id.New(), "ses_b_"+id.New()
	broker := startTokenBroker(t)
	if wideGrant {
		startWideningAuthService(t, url, keys, broker.introspect, a)
	} else {
		startAuthService(t, url, keys, broker.introspect)
	}
	if counterRelay {
		t.Setenv("ENVOY_TEST_GO_OVERLAY", counterRelayOverlay(t))
	}
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), url, "relay-reply-callout", bus.NkeySeedEnvVar+"="+listenerSeed)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the listener's output:\n%s", listener.Output.String())
		}
	})
	listener.waitHealthy(t)
	asListener := bus.WithEnvironment(func(name string) (string, bool) {
		if name == bus.NkeySeedEnvVar {
			return listenerSeed, true
		}
		return "", false
	})
	consumers, err := bus.ConnectSessionConsumers([]string{url}, 0, nil, asListener)
	if err != nil {
		t.Fatalf("connect the test's view of the session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	publisher, err := bus.Connect([]string{url}, bus.WithName("relay-reply-callout-publisher"), asListener)
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)

	refusedA, refusedB := &serverRefusals{}, &serverRefusals{}
	connA := connectThroughTheService(t, url, broker.server.URL, a, refusedA)
	connB := connectThroughTheService(t, url, broker.server.URL, b, refusedB)

	// Step 1: A asks for B's relay subject, which A's grant does not name.
	spy, err := connA.SubscribeSync(bus.DeliverSubject(b))
	if err != nil {
		t.Fatalf("subscribe A to B's relay subject: %v", err)
	}
	if err := connA.Flush(); err != nil {
		t.Fatalf("flush A's subscription: %v", err)
	}
	if !testwait.Holds(testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll, func() bool {
		return slices.Contains(refusedA.subscriptions(), bus.DeliverSubject(b))
	}) {
		t.Fatalf("the server never refused A's subscription to B's relay subject %s; it refused %v", bus.DeliverSubject(b), refusedA.subscriptions())
	}

	// Each session receives its own frame and answers received, as a plugin does before it writes
	// a frame: A's message and B's stay pending, and neither relay pauses for a missing receipt.
	receipt := func(relayedFrame) []string { return []string{relayReplyReceived} }
	standInA := startSessionStandInOn(t, connA, a, receipt)
	standInB := startSessionStandInOn(t, connB, b, receipt)
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(a, "epoch-1"))
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(b, "epoch-1"))
	forA, forB := newEnvelope("agent", contracts.LegacyAgentSubject(a), "for A"), newEnvelope("agent", contracts.LegacyAgentSubject(b), "for B")
	publish(t, publisher, forA)
	publish(t, publisher, forB)
	frameA, frameB := standInA.collect(t, 1, 10*time.Second)[0], standInB.collect(t, 1, 10*time.Second)[0]
	requireFrames(t, []relayedFrame{frameA}, forA)
	requireFrames(t, []relayedFrame{frameB}, forB)
	if spied, err := spy.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("A received %q on B's relay subject", spied.Data)
	}

	// Step 2: A forms every reply subject it can from its own frame's, and publishes done to each.
	inbox, ownPart, ownToken := replyParts(t, frameA.msg.Reply)
	inboxB, partB, _ := replyParts(t, frameB.msg.Reply)
	wantOwn, wantB := inbox+"."+a, inbox+"."+b
	if counterRelay {
		wantOwn, wantB = inbox, inbox
	}
	if inboxB != inbox || ownPart != wantOwn || partB != wantB || isCounterToken(ownToken) != counterRelay {
		t.Fatalf("A's frame is answered on %s and B's on %s, want %s.<token> and %s.<token> under one inbox: the listener relaying is not the one this run asks for",
			frameA.msg.Reply, frameB.msg.Reply, wantOwn, wantB)
	}
	guesses := append(counterNeighbours(ownToken), randomTokens(1000)...)
	underOwn, underB := make([]string, len(guesses)), make([]string, len(guesses))
	for i, guess := range guesses {
		underOwn[i], underB[i] = ownPart+"."+guess, inbox+"."+b+"."+guess
	}
	for _, subject := range append(slices.Clone(underOwn), underB...) {
		if err := connA.Publish(subject, []byte(relayReplyDone)); err != nil {
			t.Fatalf("publish A's done on %s: %v", subject, err)
		}
	}
	if err := connA.Flush(); err != nil {
		t.Fatalf("flush A's dones: %v", err)
	}
	// ackedB reports whether B's consumer has let go of B's message, and what it holds.
	ackedB := func() (bool, string) {
		info, err := consumers.Info(t.Context(), b)
		if err != nil {
			t.Fatalf("read the consumer of B: %v", err)
		}
		return info.NumAckPending != 1 || info.AckFloor.Stream >= frameB.seq,
			fmt.Sprintf("%d pending ack, ack floor %d", info.NumAckPending, info.AckFloor.Stream)
	}
	var held string
	if testwait.Holds(300*time.Millisecond, testwait.Poll, func() (gone bool) { gone, held = ackedB(); return gone }) {
		t.Fatalf("A's done acknowledged B's message at stream sequence %d away: B's consumer holds %s", frameB.seq, held)
	}
	refusedUnderB := func() bool { return len(missingFrom(refusedA.publishes(), underB)) == 0 }
	if !testwait.Holds(testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll, refusedUnderB) {
		missing := missingFrom(refusedA.publishes(), underB)
		t.Fatalf("the server served %d of A's %d dones under B's part of the reply subject, %s among them, which A's grant should refuse",
			len(missing), len(underB), missing[0])
	}
	if refused := missingFrom(underB, refusedA.publishes()); len(refused) > 0 {
		t.Fatalf("the server refused %d of A's dones outside B's part of the reply subject, %s among them: A's grant refused its own replies, so its dones never reached the relay",
			len(refused), refused[0])
	}
	if gone, held := ackedB(); gone {
		t.Fatalf("B's message at stream sequence %d was acknowledged before B answered done: B's consumer holds %s", frameB.seq, held)
	}

	// Step 3: B's own done, on its own reply subject, acknowledges its message.
	standInB.reply(frameB, relayReplyDone)
	awaitAcked(t, consumers, b, frameB.seq)
	if refused := refusedB.publishes(); len(refused) != 0 {
		t.Fatalf("the server refused B's publishes to %v, want none: B answers under its own reply prefix", refused)
	}
	t.Logf("A's frame was answered on %s; A published done to %d subjects, and the server refused the %d under B's part; B's message (stream sequence %d) waited on %s until B's own done",
		frameA.msg.Reply, len(underOwn)+len(underB), len(underB), frameB.seq, frameB.msg.Reply)
}

// relayReplyControl reads relayReplyControlVar: whether this run builds the listener with the
// shared counter's token (counterRelayOverlay), and whether session A is granted the shared
// session user's reply publish (startWideningAuthService).
func relayReplyControl(t *testing.T) (counterRelay, wideGrant bool) {
	t.Helper()
	value := os.Getenv(relayReplyControlVar)
	switch value {
	case "":
		return false, false
	case "counter":
		counterRelay, wideGrant = true, true
	case "counter-relay":
		counterRelay = true
	case "wide-grant":
		wideGrant = true
	default:
		t.Fatalf("%s=%q: want counter, counter-relay or wide-grant, or unset", relayReplyControlVar, value)
	}
	t.Logf("%s=%s: a control run, with the shared counter's relay %t and the shared session user's reply publish for A %t",
		relayReplyControlVar, value, counterRelay, wideGrant)
	return counterRelay, wideGrant
}

// replyParts splits a frame's reply subject: the listener's inbox (_INBOX.<id>), the part before
// the token, and the token. Under this tree's relay the part before the token is the inbox and the
// frame's session (bus.SessionReplySubject); under the shared counter it is the inbox alone.
func replyParts(t *testing.T, reply string) (inbox, part, token string) {
	t.Helper()
	fields := strings.Split(reply, ".")
	if len(fields) < 3 || fields[0]+"." != natsgo.InboxPrefix {
		t.Fatalf("a frame carries reply subject %q, which is no reply subject under a listener's inbox", reply)
	}
	dot := strings.LastIndexByte(reply, '.')
	return fields[0] + "." + fields[1], reply[:dot], reply[dot+1:]
}

// isCounterToken reports whether token is one the shared counter makes: a number in base 36, as
// strconv.FormatUint writes it.
func isCounterToken(token string) bool {
	n, err := strconv.ParseUint(token, 36, 64)
	return err == nil && strconv.FormatUint(n, 36) == token
}

// counterNeighbours are the 32 tokens of the shared counter nearest own, own aside: the 16 either
// side of it where own is a counter token far enough from the start, else the first 32 past the
// start. A token that is no counter's, as this tree's relay makes, stands at the start, so the
// guesses are the first tokens such a counter hands out.
func counterNeighbours(own string) []string {
	var n uint64
	if isCounterToken(own) {
		n, _ = strconv.ParseUint(own, 36, 64)
	}
	next := uint64(1)
	if n > 17 {
		next = n - 16
	}
	tokens := make([]string, 0, 32)
	for ; len(tokens) < 32; next++ {
		if next != n {
			tokens = append(tokens, strconv.FormatUint(next, 36))
		}
	}
	return tokens
}

// randomTokens are n random tokens, each as long as a token of this tree's relay
// (crypto/rand.Text).
func randomTokens(n int) []string {
	tokens := make([]string, n)
	for i := range tokens {
		tokens[i] = rand.Text()
	}
	return tokens
}

// missingFrom is every subject of want that got does not hold, in want's order.
func missingFrom(got, want []string) []string {
	held := make(map[string]bool, len(got))
	for _, subject := range got {
		held[subject] = true
	}
	var missing []string
	for _, subject := range want {
		if !held[subject] {
			missing = append(missing, subject)
		}
	}
	return missing
}

// serverRefusals is every subject the server refused one connection a publish to or a
// subscription to: a refusal reaches the client only as an asynchronous error, which record reads.
type serverRefusals struct {
	mu                 sync.Mutex
	publish, subscribe []string
}

func (r *serverRefusals) record(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
	text := err.Error()
	for _, form := range []struct {
		prefix string
		into   *[]string
	}{
		{`Permissions Violation for Publish to "`, &r.publish},
		{`Permissions Violation for Subscription to "`, &r.subscribe},
	} {
		at := strings.Index(text, form.prefix)
		if at < 0 {
			continue
		}
		subject, _, _ := strings.Cut(text[at+len(form.prefix):], `"`)
		r.mu.Lock()
		*form.into = append(*form.into, subject)
		r.mu.Unlock()
		return
	}
}

func (r *serverRefusals) publishes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.publish)
}

func (r *serverRefusals) subscriptions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.subscribe)
}

// tokenBroker is a stub of the broker's token endpoint: each trade is answered with a fresh access
// token for the session the trade names, bound to the key its DPoP proof carries, and introspect
// reads that binding back, as the broker's own introspection answers the auth service.
type tokenBroker struct {
	server *httptest.Server
	mu     sync.Mutex
	bound  map[string]oauthwire.Introspection
}

func startTokenBroker(t *testing.T) *tokenBroker {
	t.Helper()
	b := &tokenBroker{bound: map[string]oauthwire.Introspection{}}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cipherclient.TokenPath {
			http.NotFound(w, r)
			return
		}
		header, _, _ := strings.Cut(r.Header.Get("DPoP"), ".")
		raw, err := base64.RawURLEncoding.DecodeString(header)
		if err != nil {
			http.Error(w, "no proof", http.StatusBadRequest)
			return
		}
		var fields struct {
			JWK jose.JSONWebKey `json:"jwk"`
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			http.Error(w, "no key", http.StatusBadRequest)
			return
		}
		sum, err := fields.JWK.Thumbprint(crypto.SHA256)
		if err != nil {
			http.Error(w, "no thumbprint", http.StatusBadRequest)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "no form", http.StatusBadRequest)
			return
		}
		token := "tok-" + id.New()
		b.mu.Lock()
		b.bound[token] = oauthwire.Introspection{
			Active: true, Session: r.PostForm.Get(oauthwire.FormSession), Person: "person@example.com",
			ClientID: ciphertoken.ExchangeClientID, ProjectScope: ciphertoken.ProjectScopePerson,
			Thumbprint: base64.RawURLEncoding.EncodeToString(sum), ExpiresAt: time.Now().Add(15 * time.Minute).Unix(),
		}
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "DPoP", "expires_in": 900})
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (b *tokenBroker) introspect(_ context.Context, token string) (oauthwire.Introspection, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bound[token], nil
}

// writeCalloutSeeds writes the auth service's three seeds into files of their own, as the broker
// reads them.
func writeCalloutSeeds(t *testing.T, keys testnats.CalloutKeys) natscallout.Settings {
	t.Helper()
	dir := t.TempDir()
	write := func(name, seed string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	return natscallout.Settings{
		SeedFile:       write("service.seed", keys.ServiceSeed),
		IssuerSeedFile: write("issuer.seed", keys.IssuerSeed),
		XKeySeedFile:   write("xkey.seed", keys.XKeySeed),
	}
}

// startAuthService runs the real auth service in front of the hub at url, deciding each session
// by introspect, and returns once the hub sends it each connection.
func startAuthService(t *testing.T, url string, keys testnats.CalloutKeys, introspect func(context.Context, string) (oauthwire.Introspection, error)) {
	t.Helper()
	settings := writeCalloutSeeds(t, keys)
	settings.URLs = []string{url}
	service, err := natscallout.Start(t.Context(), settings, natscallout.Deps{Introspect: introspect, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("start the auth service: %v", err)
	}
	t.Cleanup(service.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := service.Ready(ctx); err != nil {
		t.Fatalf("the auth service's subscription never reached the hub: %v", err)
	}
}

// startWideningAuthService is the control's auth service. It answers the hub's authorization
// requests as the real one does, each decided by natscallout.Decide over the connection's own
// token and proof, but grants session widened the shared session user's reply publish, the
// _INBOX. entry of natsgrant.SessionPattern, in place of the one natsgrant.Session gives it. Every
// other entry of every user is Decide's.
func startWideningAuthService(t *testing.T, url string, keys testnats.CalloutKeys, introspect func(context.Context, string) (oauthwire.Introspection, error), widened string) {
	t.Helper()
	issuer, err := nkeys.FromSeed([]byte(keys.IssuerSeed))
	if err != nil {
		t.Fatalf("read the issuer seed: %v", err)
	}
	curve, err := nkeys.FromCurveSeed([]byte(keys.XKeySeed))
	if err != nil {
		t.Fatalf("read the curve seed: %v", err)
	}
	var wide string
	for _, entry := range natsgrant.SessionPattern().Publish {
		if strings.HasPrefix(entry, natsgo.InboxPrefix) {
			wide = entry
		}
	}
	if wide == "" {
		t.Fatalf("natsgrant.SessionPattern publishes %v, no entry under %s: the control has no wide reply publish to grant", natsgrant.SessionPattern().Publish, natsgo.InboxPrefix)
	}
	conn, err := testnats.ConnectSeed(t, url, keys.ServiceSeed)
	if err != nil {
		t.Fatalf("connect the control's auth service: %v", err)
	}
	t.Cleanup(conn.Close)
	deps := natscallout.Deps{Introspect: introspect}
	if _, err := conn.Subscribe("$SYS.REQ.USER.AUTH", func(msg *natsgo.Msg) {
		serverKey := msg.Header.Get("Nats-Server-Xkey")
		opened, err := curve.Open(msg.Data, serverKey)
		if err != nil {
			t.Errorf("open an authorization request: %v", err)
			return
		}
		claims, err := jwt.DecodeAuthorizationRequestClaims(string(opened))
		if err != nil {
			t.Errorf("decode an authorization request: %v", err)
			return
		}
		decision, err := natscallout.Decide(context.Background(), deps, nil, natscallout.Request{
			UserNkey: claims.UserNkey, ServerID: claims.Server.ID, Nonce: claims.ClientInformation.Nonce,
			Token: claims.ConnectOptions.Token, Sig: claims.ConnectOptions.SignedNonce,
		})
		response := jwt.NewAuthorizationResponseClaims(claims.UserNkey)
		response.Audience = claims.Server.ID
		if err != nil {
			response.Error = err.Error()
		} else {
			publish := decision.Grant.Publish
			if decision.Name == "session/"+widened {
				publish = slices.Clone(publish)
				for i, entry := range publish {
					if strings.HasPrefix(entry, natsgo.InboxPrefix) {
						publish[i] = wide
					}
				}
			}
			user := jwt.NewUserClaims(claims.UserNkey)
			user.Audience, user.Name, user.Expires = "$G", decision.Name, decision.Expires.Unix()
			user.Pub.Allow.Add(publish...)
			user.Sub.Allow.Add(decision.Grant.Subscribe...)
			if response.Jwt, err = user.Encode(issuer); err != nil {
				t.Errorf("sign the user of %s: %v", decision.Name, err)
				return
			}
		}
		token, err := response.Encode(issuer)
		if err != nil {
			t.Errorf("sign an authorization response: %v", err)
			return
		}
		sealed, err := curve.Seal([]byte(token), serverKey)
		if err != nil {
			t.Errorf("seal an authorization response: %v", err)
			return
		}
		if err := msg.Respond(sealed); err != nil {
			t.Errorf("answer an authorization request: %v", err)
		}
	}); err != nil {
		t.Fatalf("subscribe the control's auth service: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the control's auth service: %v", err)
	}
}

// connectThroughTheService connects session sessionID to the hub at url through the auth
// service, with the credential a Go session client presents (natscred.Session): an access token
// traded at the broker at brokerURL under a key of the session's own, and a proof of that key over
// the server's nonce. Every refusal the server sends the connection goes to refusals.
func connectThroughTheService(t *testing.T, url, brokerURL, sessionID string, refusals *serverRefusals) *natsgo.Conn {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatalf("make the key of %s: %v", sessionID, err)
	}
	identity := filepath.Join(t.TempDir(), "pod-token")
	if err := os.WriteFile(identity, []byte("pod-token-of-"+sessionID), 0o600); err != nil {
		t.Fatalf("write the identity of %s: %v", sessionID, err)
	}
	client, err := cipherclient.New(cipherclient.Config{
		BrokerURL: cipheraddr.New(brokerURL), Key: key, Subject: cipherclient.NewProjectedToken(identity), Session: sessionID,
	})
	if err != nil {
		t.Fatalf("make the broker client of %s: %v", sessionID, err)
	}
	option, _ := natscred.Session(client)
	conn, err := natsgo.Connect(url, option, natsgo.Timeout(10*time.Second), natsgo.NoReconnect(), natsgo.ErrorHandler(refusals.record))
	if err != nil {
		t.Fatalf("the auth service did not admit session %s: %v", sessionID, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// counterRelayOverlay writes a go build -overlay file, and returns its path, that builds the
// listener with the relay as it was before a frame's reply subject named its session, from this
// tree's own session_relay.go: each frame's reply subject is the listener's inbox and one counter
// every session shares, in base 36 (<inbox>.1, <inbox>.2, and on), and a reply on a frame's token
// moves that frame whoever sent it, since onReply reads a reply on <inbox>.<token> as one from the
// frame's own session before it parses anything. Each change must find its text exactly once, so
// a relay rewritten since fails the control instead of building the relay it was meant to
// replace.
func counterRelayOverlay(t *testing.T) string {
	t.Helper()
	source, err := filepath.Abs("session_relay.go")
	if err != nil {
		t.Fatalf("find session_relay.go: %v", err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read session_relay.go: %v", err)
	}
	text := string(raw)
	const onReply = "func (m *sessionRelays) onReply(msg *nats.Msg) {\n"
	for _, change := range []struct{ from, to string }{
		{"reply := bus.SessionReplySubject(m.inbox, run.sessionID)", `reply := m.inbox + "." + strconv.FormatUint(relayReplyControlCounter.Add(1), 36)`},
		{onReply, onReply + `	if token, ok := strings.CutPrefix(msg.Subject, m.replyPrefix); ok && !strings.Contains(token, ".") {
		m.framesMu.Lock()
		frame := m.frames[token]
		m.framesMu.Unlock()
		if frame != nil {
			msg = &nats.Msg{Subject: m.replyPrefix + frame.run.sessionID + "." + token, Data: msg.Data}
		}
	}
`},
	} {
		if n := strings.Count(text, change.from); n != 1 {
			t.Fatalf("session_relay.go holds %q %d times, want once: the control cannot build the shared counter's relay from this tree", change.from, n)
		}
		text = strings.Replace(text, change.from, change.to, 1)
	}
	text += "\n// relayReplyControlCounter is the one counter every frame's token comes from.\nvar relayReplyControlCounter atomic.Uint64\n"
	dir := t.TempDir()
	relay := filepath.Join(dir, "session_relay.go")
	if err := os.WriteFile(relay, []byte(text), 0o600); err != nil {
		t.Fatalf("write the overlay's session_relay.go: %v", err)
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {source: relay}})
	if err != nil {
		t.Fatalf("encode the overlay: %v", err)
	}
	path := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(path, overlay, 0o600); err != nil {
		t.Fatalf("write the overlay: %v", err)
	}
	return path
}
