// brokertoken_address_test.go drives the source address's introspection bucket
// (DefaultAddressLimit) of the broker-token check: that it bounds a flood of tokens the broker
// never issued before the broker, that its refusal is its own outcome, that a burst on one cold
// token takes one request of it, and that only the broker's answer that a token is live gives the
// request back, stamped with the verifier's own clock (R11, Verifier.known).
package ciphertoken_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// TestAFloodOfDistinctUnknownTokensFromOneAddressIsBoundedBeforeTheBroker: the address bucket is
// what stops a stranger spending the budget a person's approval needs (INFRA-228), so K requests
// bearing K tokens the broker has never issued cost fewer than K introspections. With the bound
// lifted and nothing else changed they cost K, which is what says the bucket did it.
func TestAFloodOfDistinctUnknownTokensFromOneAddressIsBoundedBeforeTheBroker(t *testing.T) {
	const flood = 40
	spend := func(t *testing.T, rig *rig) int64 {
		caller := newSigner(t)
		for request := range flood {
			token := fmt.Sprintf("a-token-the-broker-never-issued-%d", request)
			if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err == nil {
				t.Fatalf("request %d was accepted; want a refusal", request)
			}
		}
		return rig.broker.Count()
	}

	bounded := spend(t, newRig(t))
	if bounded >= flood || bounded < int64(ciphertoken.DefaultAddressLimit.Burst) {
		t.Fatalf("%d requests from one address cost %d introspections, want the bucket's burst of %d and fewer than %d",
			flood, bounded, ciphertoken.DefaultAddressLimit.Burst, flood)
	}
	// The negative control: the same flood with the bucket wide enough to admit all of it.
	unbounded := spend(t, newRig(t, func(o *ciphertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Nanosecond, Burst: flood}
	}))
	if unbounded != flood {
		t.Fatalf("with the bound lifted, %d requests cost %d introspections, want %d", flood, unbounded, flood)
	}
}

// TestARefusedAddressIsNeverTheBrokersVerdict: the refusal a spent bucket gives is its own
// outcome, so a caller is told to come back rather than told its token is bad. The bucket is
// spent by a token the broker calls not live, whose request stays taken: a live token gives its
// request back (R11), so it would leave the bucket as it found it.
func TestARefusedAddressIsNeverTheBrokersVerdict(t *testing.T) {
	rig := newRig(t, func(o *ciphertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Minute, Burst: 1}
	})
	caller := newSigner(t)
	rig.broker.Says("the-first-token", ciphertoken.Introspection{Active: false})
	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "the-first-token")); !errors.Is(err, ciphertoken.ErrNotActive) {
		t.Fatalf("the request that spends the bucket answered %v, want ErrNotActive", err)
	}

	_, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "a-second-token"))
	if !errors.Is(err, ciphertoken.ErrRateLimited) {
		t.Fatalf("the request with no token left in the bucket answered %v, want ErrRateLimited", err)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("the refused request reached the broker: %d introspections, want 1", rig.broker.Count())
	}
	if rig.verifier.RetryAfter() != time.Minute {
		t.Fatalf("RetryAfter = %v, want the bucket's minute", rig.verifier.RetryAfter())
	}
}

// heldAnswer is a transport that holds each introspection back until every request counted in
// arrived is on its way to the verifier, and then for pause more, ample time for each to reach
// it, before it sends the introspection on: how a test has all its requests arrive while the
// first introspection is still in flight.
type heldAnswer struct {
	arrived *sync.WaitGroup
	pause   time.Duration
}

func (h heldAnswer) RoundTrip(r *http.Request) (*http.Response, error) {
	h.arrived.Wait()
	time.Sleep(h.pause)
	return http.DefaultTransport.RoundTrip(r)
}

// TestRequestsArrivingAtOnceWithOneTokenCostOneIntrospection: the single flight is what makes a
// burst on a cold cache cost one introspection rather than one a request, and the address bucket
// is taken from inside that flight, so the burst takes one request of it rather than one each,
// and the live answer gives that one back (R11). The bucket here holds exactly one, so charging
// per request would refuse every request that arrives while the first one's introspection is in
// flight — which is what an honest caller making a burst of calls on a token it has just been
// given would meet.
//
// The broker's answer is held back (heldAnswer) until all eight requests are on their way to the
// verifier and 200 ms more have passed, so every request arrives while the first introspection is
// in flight, before its live answer gives the bucket's one request back. Without the hold the
// answer could come back, and return the request, before the others arrive, and a charge per
// request could then refuse none of them; with it, such a charge refuses all seven others
// ErrRateLimited.
func TestRequestsArrivingAtOnceWithOneTokenCostOneIntrospection(t *testing.T) {
	const requests = 8
	var arrived sync.WaitGroup
	arrived.Add(requests)
	rig := newRig(t, func(o *ciphertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Hour, Burst: 1}
		o.HTTP = &http.Client{Transport: heldAnswer{arrived: &arrived, pause: 200 * time.Millisecond}}
	})
	caller := newSigner(t)
	const token = "a-token-many-requests-arrive-with-at-once"
	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))

	// Each request needs its own proof: a replayed one is refused, which is a different check.
	signed := make([]*http.Request, requests)
	for i := range signed {
		signed[i] = caller.at(fmt.Sprintf("%s/%d", resourcePath, i), token)
	}
	var wait sync.WaitGroup
	failures := make([]error, requests)
	for i := range signed {
		wait.Add(1)
		go func() {
			defer wait.Done()
			arrived.Done()
			_, failures[i] = rig.verifier.Verify(t.Context(), signed[i])
		}()
	}
	wait.Wait()
	for i, err := range failures {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("%d requests arriving at once cost %d introspections, want 1", requests, rig.broker.Count())
	}
}

// The three tests below hold R11 of INFRA-4425, the give-back rule Verifier.known states. Each
// runs on newRig's default bucket (DefaultAddressLimit) and its held clock, which none of them
// advances, so the bucket refills nothing and every answer falls inside one AffirmativeCacheTTL.
// Every request comes from one source address, presents a token no request before it presented,
// and is sent only once the one before it has its answer.

// TestOnlyALiveAnswerGivesItsRequestBack (R11): each row presents twice the bucket's burst of
// distinct tokens the broker answers one way. The first burst all reach the broker and are each
// answered the row's error. Where the row's answer gives its request back, so are the second
// burst, and every token reaches the broker; where it keeps it, every one of the second burst is
// refused ErrRateLimited without reaching the broker.
//
// A live answer gives its request back whatever Verify decides about the token after it: a
// session's live token is admitted, and a machine login's own, live and no session's credential,
// is refused ErrNotASession, and neither is ever refused ErrRateLimited. A not-live answer keeps
// its request, and so does an introspection that gets no answer: a body that does not decode, an
// answer cut off mid-body even where it began live, is ErrBrokerUnreachable.
//
// The not-live row is the flood
// TestAFloodOfDistinctUnknownTokensFromOneAddressIsBoundedBeforeTheBroker sends as well, since the
// stub broker answers a token it was told nothing about not live. Both stay: that test holds
// INFRA-228's bound, against its negative control of the same flood with the bucket lifted, and
// this row holds R11's split between the answers that keep their request and those that give it
// back.
func TestOnlyALiveAnswerGivesItsRequestBack(t *testing.T) {
	burst := ciphertoken.DefaultAddressLimit.Burst
	for _, row := range []struct {
		name string
		// answer makes the broker answer token this row's way, bound to jkt where it is live.
		answer func(broker *ciphertokentest.StubBroker, token, jkt string)
		// first is what each token of the first burst is answered: nil for one admitted.
		first error
		// refused is each token of the second burst being refused ErrRateLimited without
		// reaching the broker; where it is false, each is answered first, as the first burst was.
		refused bool
	}{
		{
			name: "live tokens",
			answer: func(broker *ciphertokentest.StubBroker, token, jkt string) {
				broker.Says(token, liveToken(jkt, time.Hour))
			},
		},
		{
			name: "live tokens that are no session's credential",
			answer: func(broker *ciphertokentest.StubBroker, token, jkt string) {
				broker.Says(token, machineLoginToken(jkt, time.Hour))
			},
			first: ciphertoken.ErrNotASession,
		},
		{
			name: "tokens the broker calls not live",
			answer: func(broker *ciphertokentest.StubBroker, token, _ string) {
				broker.Says(token, ciphertoken.Introspection{Active: false})
			},
			first:   ciphertoken.ErrNotActive,
			refused: true,
		},
		{
			name: "introspections that get no answer",
			answer: func(broker *ciphertokentest.StubBroker, token, _ string) {
				broker.SaysRaw(token, `{"active":true,"session":"ses_a1b2c3"`)
			},
			first:   ciphertoken.ErrBrokerUnreachable,
			refused: true,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newRig(t)
			caller := newSigner(t)
			for i := range 2 * burst {
				token := fmt.Sprintf("a-token-of-%s-%d", strings.ReplaceAll(row.name, " ", "-"), i)
				row.answer(rig.broker, token, caller.jkt)
				want := row.first
				if row.refused && i >= burst {
					want = ciphertoken.ErrRateLimited
				}
				if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); !errors.Is(err, want) {
					t.Fatalf("token %d of %d answered %v, want %v", i+1, 2*burst, err, want)
				}
			}
			introspections := 2 * burst
			if row.refused {
				introspections = burst
			}
			if rig.broker.Count() != int64(introspections) {
				t.Fatalf("%d tokens cost %d introspections, want %d", 2*burst, rig.broker.Count(), introspections)
			}
		})
	}
}

// TestOnlyTokensTheBrokerCallsNotLiveSpendTheBucket (R11): a burst of live tokens leaves the
// bucket whole for what follows, so after the bucket's burst of distinct live tokens, all
// admitted, the next burst of distinct not-live tokens are each refused ErrNotActive by the
// broker, and only the request after them, the first the bucket cannot take, is refused
// ErrRateLimited.
func TestOnlyTokensTheBrokerCallsNotLiveSpendTheBucket(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	burst := ciphertoken.DefaultAddressLimit.Burst
	request := 0
	for i := range burst {
		request++
		token := fmt.Sprintf("a-live-token-before-the-refusals-%d", i)
		rig.broker.Says(token, liveToken(caller.jkt, time.Hour))
		if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err != nil {
			t.Fatalf("request %d, live token %d of %d, answered %v, want it admitted", request, i+1, burst, err)
		}
	}
	for i := range burst + 1 {
		request++
		token := fmt.Sprintf("a-not-live-token-after-the-live-ones-%d", i)
		rig.broker.Says(token, ciphertoken.Introspection{Active: false})
		want := ciphertoken.ErrNotActive
		if i == burst {
			want = ciphertoken.ErrRateLimited
		}
		if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); !errors.Is(err, want) {
			t.Fatalf("request %d, not-live token %d of %d, answered %v, want %v", request, i+1, burst+1, err, want)
		}
	}
	if rig.broker.Count() != int64(2*burst) {
		t.Fatalf("%d requests cost %d introspections, want %d: every one but the refused last", request, rig.broker.Count(), 2*burst)
	}
}

// TestTheGiveBackIsStampedWithTheVerifiersClock (R11): a live token's give-back is stamped with
// the verifier's own clock (Options.Now), the one its take read, so it returns the one request it
// took and no refill besides. After the bucket's burst less one of distinct not-live tokens, which
// keep their requests, one live token takes the last request and gives it back, so of the two
// not-live tokens after it the first reaches the broker and is refused ErrNotActive, and the
// second is refused ErrRateLimited without reaching it.
//
// The held clock stands still while the real one runs on, and the test waits two of the bucket's
// refills of real time before the live token, so a give-back stamped with the real clock would
// also refill the bucket for that wait, and the second not-live token would reach the broker. The
// wait is what tells the two clocks apart: without it they part only by the time the requests
// before the live token take, less than one refill on a quick machine, and a give-back stamped
// with the real clock then returns the one request alone, as the held clock's does.
func TestTheGiveBackIsStampedWithTheVerifiersClock(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	limit := ciphertoken.DefaultAddressLimit
	request := 0
	notLive := func(i int, want error) {
		t.Helper()
		request++
		token := fmt.Sprintf("a-not-live-token-around-a-give-back-%d", i)
		rig.broker.Says(token, ciphertoken.Introspection{Active: false})
		if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); !errors.Is(err, want) {
			t.Fatalf("request %d, not-live token %d, answered %v, want %v", request, i+1, err, want)
		}
	}
	for i := range limit.Burst - 1 {
		notLive(i, ciphertoken.ErrNotActive)
	}

	time.Sleep(2 * limit.Every)
	request++
	const live = "a-live-token-whose-request-comes-back"
	rig.broker.Says(live, liveToken(caller.jkt, time.Hour))
	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, live)); err != nil {
		t.Fatalf("request %d, the live token, answered %v, want it admitted", request, err)
	}

	notLive(limit.Burst-1, ciphertoken.ErrNotActive)
	notLive(limit.Burst, ciphertoken.ErrRateLimited)
	if rig.broker.Count() != int64(limit.Burst+1) {
		t.Fatalf("%d requests cost %d introspections, want %d: every one but the refused last", request, rig.broker.Count(), limit.Burst+1)
	}
}
