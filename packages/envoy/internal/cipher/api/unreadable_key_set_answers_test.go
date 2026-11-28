package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/api/unreadablekeysettest"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
)

// unreadableKeySetAnswersComment is the fixture's _comment: what it holds, how it is made and
// what of the broker's answers it leaves out.
const unreadableKeySetAnswersComment = "Each member but this one is the secrets broker's answer to a token check that reached no verdict because the key set of the token's issuer could not be read, one of each shape a client reads: json_route a JSON route's (POST /v1/oauth/introspect with the caller's own token), token_endpoint the token endpoint's (POST /v1/oauth/token, a pod's token exchange), hold_route a route of the NATS writer hold's (GET /v1/nats-hold with the operator's token). Each holds the request, the status, the Retry-After and Content-Type headers, and the body, at the default spacing of a key set's failed reads (5 s). internal/cipher/api's TestTheUnreadableKeySetAnswersAreTheFixture writes this file from the broker's real HTTP server when UPDATE_UNREADABLE_KEY_SET_ANSWERS=1 is set, and fails in every other run where the server answers otherwise. A client's test replays the answer of its shape (internal/cipher/api/unreadablekeysettest), setting its own Retry-After where its row is about another wait. Each body is the broker's bytes as JSON: the white space between its tokens is free, and the newline the broker's encoder ends it with is left out. No body carries a value of its own request, so nothing in one is replaced: the answer id is in the X-Broker-Answer-Id header, which this file does not hold, and the broker adds answer_id to a 4xx body only, never to a 503's."

// TestTheUnreadableKeySetAnswersAreTheFixture: the broker's real server, while the key set of the
// token's issuer cannot be read, answers each shape a client reads exactly as
// testdata/unreadable-key-set-answers.json holds it, the file every client's test replays
// (unreadablekeysettest.Read): a JSON route's (POST /v1/oauth/introspect, newTestServer), the
// token endpoint's (POST /v1/oauth/token, a pod's token exchange, mounted by api.Register on the
// same server) and a hold route's (GET /v1/nats-hold, newHoldRig). Each is compared whole: the
// status, Retry-After, Content-Type and the body, read as JSON with the white space between its
// tokens taken out (json.Compact), which takes out the newline its encoder ends it with. Nothing
// else is normalised: none of the three bodies carries a value of its own request, since the
// answer id rides in the X-Broker-Answer-Id header, which the file does not hold, and
// answer.Middleware adds answer_id to a 4xx body only. With UPDATE_UNREADABLE_KEY_SET_ANSWERS=1
// it writes the file from the server's answers instead, each first held to its shape's assertion
// (503, Retry-After 5, its code), so no capture records an answer of another kind.
func TestTheUnreadableKeySetAnswersAreTheFixture(t *testing.T) {
	ts := newTestServer(t)
	ts.podIssuer.FailKeys(http.StatusServiceUnavailable)
	hold := newHoldRig(t)
	hold.issuer.FailKeys(http.StatusServiceUnavailable)

	introspection := introspectCall(t, ts.URL, ts.uiToken)
	answeredIssuerUnavailable(t, "introspection with the listed caller's token", introspection)
	exchange := ts.exchangeCall(t, ts.audienceToken(t, ciphertest.ExchangeAccount, "pod-uid-exchange", ciphertest.ExchangeAudience, time.Hour))
	answeredTemporarilyUnavailable(t, "a pod's token exchange", exchange)
	holdRead := bearerCall(t, hold.url, http.MethodGet, "/v1/nats-hold", hold.token(holdOperatorAccount))
	answeredHoldIssuerUnavailable(t, "GET /v1/nats-hold with the operator's token", holdRead)
	got := unreadablekeysettest.Answers{
		Comment:       unreadableKeySetAnswersComment,
		JSONRoute:     capturedAnswer(t, "POST /v1/oauth/introspect", introspection),
		TokenEndpoint: capturedAnswer(t, "POST /v1/oauth/token", exchange),
		HoldRoute:     capturedAnswer(t, "GET /v1/nats-hold", holdRead),
	}

	path := filepath.Join("testdata", "unreadable-key-set-answers.json")
	if os.Getenv("UPDATE_UNREADABLE_KEY_SET_ANSWERS") == "1" {
		encoded, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := unreadablekeysettest.Read(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s is not what this broker answers while a key set cannot be read; run UPDATE_UNREADABLE_KEY_SET_ANSWERS=1 with BROKER_TEST_DATABASE_URL set, go test -run %s ./internal/cipher/api, and check every client test that replays it.\nthe server answers:\n%s\nthe file holds:\n%s",
			path, t.Name(), indented(t, got), indented(t, want))
	}
}

// capturedAnswer is got, the answer to request, as the fixture holds it
// (unreadablekeysettest.Captured).
func capturedAnswer(t *testing.T, request string, got answered) unreadablekeysettest.Answer {
	t.Helper()
	return unreadablekeysettest.Captured(t, request, got.status, got.header, got.body)
}

// indented is answers as the fixture writes them.
func indented(t *testing.T, answers unreadablekeysettest.Answers) string {
	t.Helper()
	encoded, err := json.MarshalIndent(answers, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
