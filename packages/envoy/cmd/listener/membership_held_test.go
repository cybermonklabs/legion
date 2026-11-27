package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/listenerwire"
)

// heldTestTokens is the listener's broker-token credential over broker, its verifier reading clock,
// as apiAuth reads it once the spent-proof bucket is open.
func heldTestTokens(t *testing.T, broker *ciphertokentest.StubBroker, clock *listenerClock) *brokerTokens {
	t.Helper()
	tokens := newBrokerTokens(listenerBrokerConfig{
		BrokerURL:        broker.Server.URL,
		AccountTokenFile: "/var/run/secrets/broker/token",
		Origin:           listenerBrokerOrigin,
	})
	verifier, err := ciphertoken.New(ciphertoken.Options{
		BrokerURL: broker.Server.URL,
		Account:   listenerBrokerAccount("listener-account-token"),
		Origins:   []string{listenerBrokerOrigin},
		Replay:    &spentProofsInMemory{},
		Address:   listenerSourceAddress,
		Now:       clock.Now,
	})
	if err != nil {
		t.Fatalf("build the broker-token verifier: %v", err)
	}
	tokens.verifier.Store(verifier)
	return tokens
}

// listenerHeldText is the listener's 401 MEMBERSHIP_HELD for a broker token whose broker answers
// person held: true under every rule that signs people in to Dispatch, since the answer carries
// none and the listener knows none.
func listenerHeldText(person string) string {
	return "Dispatch holds " + person + "'s credentials; they work again once Dispatch confirms them"
}

// TestAPIAuthRefusesAHeldBrokerToken (K2, K3): a broker token whose introspection answers its
// person held is refused 401 MEMBERSHIP_HELD, naming the person in words true under any rule that
// signs people in to Dispatch (a sign-in under a Google rule, the Workspace Directory under IAP),
// and never reaches the route; only after the listener's own checks, so the same token under a
// stranger's proof is the 401 every refused credential gets here, with no code. Once the broker
// answers the person not held, the same token is served at the first request after the answer the
// check kept lapses (ciphertoken.AffirmativeCacheTTL).
//
// Control: the Verifier leaving Caller.Held false whatever the answer says; the held token is
// served 200 and the test fails naming it.
func TestAPIAuthRefusesAHeldBrokerToken(t *testing.T) {
	broker := ciphertokentest.NewStubBroker(t)
	caller, stranger := ciphertokentest.NewCaller(t), ciphertokentest.NewCaller(t)
	clock := &listenerClock{now: time.Now()}
	tokens := heldTestTokens(t, broker, clock)

	const person, token = "ada@example.com", "access-token-of-a-held-person"
	held := sessionAnswer("ses_held", person, caller.JKT())
	held.Held = true
	broker.Says(token, held)
	var served int
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	})
	serve := func(request *http.Request) (*httptest.ResponseRecorder, string) {
		t.Helper()
		var logged bytes.Buffer
		response := httptest.NewRecorder()
		apiAuth(listenerAPIAuth{brokers: tokens, logger: logging.NewWithWriter("test-machine", &logged)}, next).ServeHTTP(response, request)
		return response, logged.String()
	}

	stolen := brokerTokenRequest(t, stranger, http.MethodPost, "/v1/messages/publish", token, `{}`)
	response, logged := serve(stolen)
	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "MEMBERSHIP_HELD") || served != 0 {
		t.Fatalf("the held token under a stranger's proof = %d %s, served %d times; want the 401 every refusal gets, with no code", response.Code, response.Body.String(), served)
	}
	assertBrokerRejectionLog(t, logged, "key_mismatch", token, stolen.Header.Get("DPoP"))

	request := brokerTokenRequest(t, caller, http.MethodPost, "/v1/messages/publish", token, `{}`)
	response, logged = serve(request)
	var refusal listenerwire.APIError
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	want := listenerHeldText(person)
	if response.Code != http.StatusUnauthorized || refusal.Code != "MEMBERSHIP_HELD" || refusal.Error != want || served != 0 {
		t.Fatalf("%s's broker token, which the broker answers held, = %d %s, served %d times; want 401 MEMBERSHIP_HELD saying %q", person, response.Code, response.Body.String(), served, want)
	}
	assertBrokerRejectionLog(t, logged, "membership_held", token, request.Header.Get("DPoP"))

	released := held
	released.Held = false
	broker.Says(token, released)
	clock.Advance(ciphertoken.AffirmativeCacheTTL)
	response, logged = serve(brokerTokenRequest(t, caller, http.MethodPost, "/v1/messages/publish", token, `{}`))
	if response.Code != http.StatusOK || served != 1 {
		t.Fatalf("%s's broker token once the broker answers her not held = %d %s, served %d times; want 200", person, response.Code, response.Body.String(), served)
	}
	assertBrokerRejectionLog(t, logged, "", token, "")
}

// brokerUnavailableText is the listener's 503 BROKER_UNAVAILABLE for a broker token whose broker
// answers its person not held only until a deadline that has passed.
const brokerUnavailableText = "whether this token's person may still act could not be read; try again in a minute"

// TestAPIAuthRefusesAnExpiredStanding (K11, c-iap-deadline): a broker token whose introspection
// answers its person not held until a deadline (not_held_until) is served until the verifier's
// clock reaches it, a millisecond before it from the answer the check kept. At the deadline the
// check asks the broker again, and an answer whose deadline is not after the verifier's clock is
// 503 BROKER_UNAVAILABLE with Retry-After 60, saying whether the token's person may still act
// could not be read and to try again in a minute, and never reaches the route; the rejection line
// names the class deadline_passed and neither the token nor the proof. A held token carrying the
// same deadline is 401 MEMBERSHIP_HELD before and after it.
//
// Controls: apiAuth without its deadline check (ada's token is served at her deadline, and the
// test fails naming it); refuseLapsedBrokerToken with no Retry-After (the 503 carries none, and the
// test fails naming it).
func TestAPIAuthRefusesAnExpiredStanding(t *testing.T) {
	broker := ciphertokentest.NewStubBroker(t)
	caller := ciphertokentest.NewCaller(t)
	clock := &listenerClock{now: time.Now()}
	tokens := heldTestTokens(t, broker, clock)
	deadline := clock.Now().Truncate(time.Second).Add(3 * time.Second)
	const person, heldPerson = "ada@example.com", "bob@example.com"
	const token, heldToken = "access-token-served-until-a-deadline", "held-access-token-carrying-a-deadline"
	served := sessionAnswer("ses_served", person, caller.JKT())
	served.NotHeldUntil = deadline.Unix()
	broker.Says(token, served)
	held := sessionAnswer("ses_held", heldPerson, caller.JKT())
	held.Held, held.NotHeldUntil = true, deadline.Unix()
	broker.Says(heldToken, held)
	var reached int
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})
	serve := func(token string) (*httptest.ResponseRecorder, string, string) {
		t.Helper()
		var logged bytes.Buffer
		request := brokerTokenRequest(t, caller, http.MethodPost, "/v1/messages/publish", token, `{}`)
		response := httptest.NewRecorder()
		apiAuth(listenerAPIAuth{brokers: tokens, logger: logging.NewWithWriter("test-machine", &logged)}, next).ServeHTTP(response, request)
		return response, logged.String(), request.Header.Get("DPoP")
	}
	expect := func(when string, wantServed bool) {
		t.Helper()
		before := reached
		response, logged, proof := serve(token)
		var refusal listenerwire.APIError
		_ = json.Unmarshal(response.Body.Bytes(), &refusal)
		switch {
		case wantServed && (response.Code != http.StatusOK || reached != before+1):
			t.Fatalf("%s's broker token %s = %d %s, reaching the route %d times; want 200", person, when, response.Code, response.Body.String(), reached-before)
		case !wantServed && (response.Code != http.StatusServiceUnavailable || refusal.Code != "BROKER_UNAVAILABLE" || refusal.Error != brokerUnavailableText || reached != before):
			t.Fatalf("%s's broker token %s = %d %s, reaching the route %d times; want 503 BROKER_UNAVAILABLE saying %q, and no route",
				person, when, response.Code, response.Body.String(), reached-before, brokerUnavailableText)
		case !wantServed && response.Header().Get("Retry-After") != "60":
			t.Fatalf("%s's broker token %s answered Retry-After %q, want 60: its text says to try again in a minute", person, when, response.Header().Get("Retry-After"))
		case !wantServed:
			assertBrokerRejectionLog(t, logged, "deadline_passed", token, proof)
		}
		response, _, _ = serve(heldToken)
		_ = json.Unmarshal(response.Body.Bytes(), &refusal)
		if response.Code != http.StatusUnauthorized || refusal.Code != "MEMBERSHIP_HELD" || refusal.Error != listenerHeldText(heldPerson) || reached != before+map[bool]int{true: 1}[wantServed] {
			t.Fatalf("%s's held broker token carrying a deadline, %s = %d %s; want 401 MEMBERSHIP_HELD saying %q", heldPerson, when, response.Code, response.Body.String(), listenerHeldText(heldPerson))
		}
	}
	expect("before her deadline", true)
	clock.Advance(deadline.Sub(clock.Now()) - time.Millisecond)
	expect("a millisecond before her deadline, from the kept answer", true)
	clock.Advance(time.Millisecond)
	expect("at her deadline", false)
	clock.Advance(time.Second)
	expect("a second past her deadline", false)
}
