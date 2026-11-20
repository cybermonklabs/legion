package brokertoken_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
)

// outsideOrigin is DISPATCH_OUTSIDE_ORIGIN for the tests of a verifier of two origins: the
// second origin Dispatch answers at, the one a caller outside signs its proof's 'htu' against
// and sends its request to.
const outsideOrigin = "https://outside.test"

// twoOrigins makes a rig's verifier one of two origins, the canonical one first, as Dispatch
// builds it where DISPATCH_OUTSIDE_ORIGIN is set.
func twoOrigins(o *brokertoken.Options) { o.Origins = []string{dispatchOrigin, outsideOrigin} }

// reaching is signed as the server sees it when it arrives under host, its proof unchanged: the
// request sent to another origin than the one its proof names, or through a proxy.
func reaching(signed *http.Request, host string) *http.Request {
	out := signed.Clone(signed.Context())
	out.URL.Host, out.Host = host, host
	return out
}

// TestAProofNamingTheOutsideOriginIsAcceptedThereAndNowhereElse: a verifier of two origins checks
// a proof against the one whose host is the request's Host. An agent outside signs its proof for
// the outside origin and sends the request there, and it is accepted, under a Host of any case, as
// Dispatch answers that host as the outside origin. The same proof sent to the canonical origin is
// refused for its target URI, which spends nothing, so the outside origin still accepts it after;
// once accepted it is refused there as a replay, which only the store can have refused, since the
// broker is asked once in all, and it is still refused at the canonical origin.
func TestAProofNamingTheOutsideOriginIsAcceptedThereAndNowhereElse(t *testing.T) {
	rig := newRig(t, twoOrigins)
	caller := newSigner(t)
	const token = "a-token-of-an-agent-outside"
	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))
	signed := caller.request(outsideOrigin+resourcePath, token)

	if _, err := rig.verifier.Verify(t.Context(), reaching(signed, "dispatch.test")); !errors.Is(err, brokertoken.ErrProofInvalid) {
		t.Fatalf("a proof naming the outside origin, sent to the canonical one, answered %v, want ErrProofInvalid", err)
	}
	if _, err := rig.verifier.Verify(t.Context(), reaching(signed, "outside.test")); err != nil {
		t.Fatalf("a proof naming the outside origin, sent there: %v", err)
	}
	if _, err := rig.verifier.Verify(t.Context(), reaching(signed, "outside.test")); !errors.Is(err, brokertoken.ErrProofInvalid) {
		t.Fatalf("the same proof sent to the outside origin again answered %v, want ErrProofInvalid", err)
	}
	if _, err := rig.verifier.Verify(t.Context(), reaching(signed, "dispatch.test")); !errors.Is(err, brokertoken.ErrProofInvalid) {
		t.Fatalf("the spent proof sent to the canonical origin answered %v, want ErrProofInvalid", err)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("four presentations of one token cost %d introspections, want 1: every refusal was the proof check's", rig.broker.Count())
	}

	capitals := caller.request(outsideOrigin+resourcePath, token)
	if _, err := rig.verifier.Verify(t.Context(), reaching(capitals, "OUTSIDE.test")); err != nil {
		t.Fatalf("a proof naming the outside origin, sent there under a Host in capitals: %v", err)
	}
}

// TestAVerifierOfTwoOriginsChecksEveryOtherHostAgainstTheCanonicalOrigin: a request whose Host is
// not the outside origin's is checked against the canonical origin, the first, as a verifier of one
// origin checks every request. A proof naming the canonical origin is accepted there, and behind a
// proxy under a Host that is neither origin's, whose forwarded headers name the outside origin and
// choose nothing; it is refused at the outside origin. A proof naming the outside origin is refused
// behind that proxy.
func TestAVerifierOfTwoOriginsChecksEveryOtherHostAgainstTheCanonicalOrigin(t *testing.T) {
	rig := newRig(t, twoOrigins)
	caller := newSigner(t)
	const token = "a-token-of-an-agent-inside"
	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))
	behindProxy := func(signed *http.Request) *http.Request {
		out := reaching(signed, "dispatch.internal:8080")
		out.Header.Set("X-Forwarded-Proto", "https")
		out.Header.Set("X-Forwarded-Host", "outside.test")
		return out
	}

	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err != nil {
		t.Fatalf("a proof naming the canonical origin, sent there: %v", err)
	}
	if _, err := rig.verifier.Verify(t.Context(), behindProxy(caller.at(resourcePath, token))); err != nil {
		t.Fatalf("a proof naming the canonical origin, sent behind a proxy that names the outside host: %v", err)
	}
	if _, err := rig.verifier.Verify(t.Context(), reaching(caller.at(resourcePath, token), "outside.test")); !errors.Is(err, brokertoken.ErrProofInvalid) {
		t.Fatalf("a proof naming the canonical origin, sent to the outside one, answered %v, want ErrProofInvalid", err)
	}
	if _, err := rig.verifier.Verify(t.Context(), behindProxy(caller.request(outsideOrigin+resourcePath, token))); !errors.Is(err, brokertoken.ErrProofInvalid) {
		t.Fatalf("a proof naming the outside origin, sent behind a proxy that names the outside host, answered %v, want ErrProofInvalid", err)
	}
}
