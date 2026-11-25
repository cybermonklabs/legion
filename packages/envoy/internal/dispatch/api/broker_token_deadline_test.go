package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// brokerUnavailableText is Dispatch's 503 BROKER_UNAVAILABLE for a broker token whose
// introspection answers its person not held only until a deadline that has passed.
const brokerUnavailableText = "whether this token's person may still act could not be read; try again in a minute"

// TestABrokerTokenPastItsDeadlineIsUnavailable (K11, c-iap-deadline): a broker token whose
// introspection answers its person not held until a deadline (not_held_until) is served until the
// verifier's clock reaches that deadline, a millisecond before it from the kept answer; at the
// deadline the verifier asks the broker again, and an answer whose deadline is not after the
// verifier's clock is 503 BROKER_UNAVAILABLE with Retry-After 60, saying whether the token's person
// may still act could not be read and to try again in a minute, whatever rule signs people in. A
// token the introspection answers held is 401 MEMBERSHIP_HELD before and after the deadline it
// carries. A live socket a not-held token opened closes at its next heartbeat once its deadline
// has passed. A broker token asks Standing nothing.
//
// Controls: brokerTokenActor without its deadline check (carol's token is served at her deadline,
// and the test fails naming it); errBrokerTokenLapsed with no Retry-After (the 503 carries none,
// and the test fails naming it); callerStillResolves without the deadline check (the socket stays
// open, and the test fails naming it).
func TestABrokerTokenPastItsDeadlineIsUnavailable(t *testing.T) {
	t.Parallel()
	clock := &heldClock{at: time.Now()}
	rig := newBrokerTokenRig(t, func(o *ciphertoken.Options) { o.Now = clock.now })
	seedPeople(t, rig.database, accessCarol, accessDave)
	caller := newBrokerCaller(t)
	deadline := clock.now().Truncate(time.Second).Add(3 * time.Second)
	const servedToken, heldToken = "carols-token-served-until-a-deadline", "daves-held-token-carrying-a-deadline"
	served := heldPersonToken(caller.jkt, accessCarol, false)
	served.NotHeldUntil = deadline.Unix()
	rig.broker.Says(servedToken, served)
	held := heldPersonToken(caller.jkt, accessDave, true)
	held.NotHeldUntil = deadline.Unix()
	rig.broker.Says(heldToken, held)
	standing := &standingFake{directory: true}
	iap := rig.deps
	iap.Standing = standing
	servers := []struct {
		name, heldText string
		handler        http.Handler
	}{{"under IAP identity", membershipHeldDirectoryText, servedWith(t, iap)}, {"with no Google rule", brokerHeldText(accessDave), rig.handler}}
	whoami := func(handler http.Handler, token string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil))
		return response
	}
	expect := func(when string, servedStatus int) {
		t.Helper()
		for _, server := range servers {
			response := whoami(server.handler, servedToken)
			switch servedStatus {
			case http.StatusOK:
				if response.Code != http.StatusOK {
					t.Fatalf("carol's broker token %s, %s = %d %s, want 200", when, server.name, response.Code, response.Body.String())
				}
			default:
				var refusal refusalAnswer
				_ = json.Unmarshal(response.Body.Bytes(), &refusal)
				if response.Code != http.StatusServiceUnavailable || refusal.Code != "BROKER_UNAVAILABLE" || refusal.Error != brokerUnavailableText {
					t.Fatalf("carol's broker token %s, %s = %d %s, want 503 BROKER_UNAVAILABLE saying %q",
						when, server.name, response.Code, strings.TrimSpace(response.Body.String()), brokerUnavailableText)
				}
				if got := response.Header().Get("Retry-After"); got != "60" {
					t.Fatalf("carol's broker token %s, %s answered Retry-After %q, want 60: its text says to try again in a minute", when, server.name, got)
				}
			}
			wantHeldRefusal(t, whoami(server.handler, heldToken), server.heldText, "dave's held broker token "+when+", "+server.name)
		}
	}
	expect("before her deadline", http.StatusOK)
	clock.advance(deadline.Sub(clock.now()) - time.Millisecond)
	expect("a millisecond before her deadline, from the kept answer", http.StatusOK)
	clock.advance(time.Millisecond)
	expect("at her deadline", http.StatusServiceUnavailable)
	clock.advance(time.Second)
	expect("a second past her deadline", http.StatusServiceUnavailable)
	if asked := standing.askedSoFar(); len(asked) != 0 {
		t.Fatalf("broker tokens asked Standing about %v, want nothing", asked)
	}

	t.Run("a live socket", func(t *testing.T) {
		broker := ciphertokentest.NewStubBroker(t)
		socketClock := &heldClock{at: time.Now()}
		// An answer past its deadline is never kept, so each heartbeat after it asks the broker
		// again: the source address's bucket is one no heartbeat of this test can empty, so the
		// socket can close on nothing but the deadline, and the token lives two days past it.
		_, database, deps := newTestServer(t, testServerOptions{brokerTokens: stubBrokerTokens(t, broker, func(o *ciphertoken.Options) {
			o.Now, o.AddressLimit = socketClock.now, ratelimit.Limit{Every: time.Microsecond, Burst: 1 << 20}
		})})
		seedPeople(t, database, accessCarol)
		socketCaller := newBrokerCaller(t)
		const token = "carols-broker-token-holding-a-socket-until-a-deadline"
		answer := heldPersonToken(socketCaller.jkt, accessCarol, false)
		answer.NotHeldUntil, answer.ExpiresAt = socketClock.now().Add(time.Hour).Unix(), socketClock.now().Add(49*time.Hour).Unix()
		broker.Says(token, answer)
		deps.StreamHeartbeat = 50 * time.Millisecond
		ended := openHeldSocket(t, directServer(deps), socketCaller.presenting(t, http.MethodGet, "/ws/live", token, nil))
		socketClock.advance(time.Hour)
		if !<-ended {
			t.Fatal("the socket carol's broker token opened was still open 10 s after the deadline of the broker's answer that she is not held: the heartbeat read no deadline")
		}
	})
}
