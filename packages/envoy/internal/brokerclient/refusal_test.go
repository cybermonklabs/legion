// packages/envoy/internal/brokerclient/refusal_test.go
//
// What a person running a deployment is told when the broker refuses the trade. Every caller of
// this package prints a broker failure through internal/redact, whose allow-list prints an
// error's own text only when it can prove that text carries nothing of the URL the deployment
// configured. A refusal that did not read as the answered HTTP refusal it is printed as "the
// request failed", and the broker's own words — the ones that say the request names no session,
// or that no entry lists this account — never reached the person, who had nothing to act on.

package brokerclient

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
)

// A refusal vouches that its sentence is fixed words and the broker's answer alone.
var _ redact.Printable = (*RefusedError)(nil)

// tradeID is a caller's typed id, the kind of value a caller wraps around a failure.
type tradeID string

// TestARefusalPrintsItsSentenceAndNoneOfACallersWrapper: redact prints a refusal as its own
// sentence beside a clean broker URL, alone, in the *url.Error a call makes, and under a
// caller's fmt.Errorf that names a typed id, and in none of them the id. The sentence is the one
// Error answers, built from the broker's answer.
func TestARefusalPrintsItsSentenceAndNoneOfACallersWrapper(t *testing.T) {
	const (
		base = "http://127.0.0.1:1"
		id   = tradeID("trade-5d1e8b-demo-not-a-secret")
	)
	refused := newRefusedError(400, "invalid_request", refusalBody)
	sentence := "the secrets broker refused this machine's identity: HTTP 400 invalid_request: " + refusalBody
	if refused.Error() != sentence || refused.PrintableText() != sentence {
		t.Fatalf("the refusal reads %q and %q, want %q", refused.Error(), refused.PrintableText(), sentence)
	}
	inCall := &url.Error{Op: "Post", URL: base + "/v1/oauth/token", Err: refused}
	for name, row := range map[string]struct {
		err  error
		want string
	}{
		"alone":                        {refused, sentence},
		"in the call":                  {inCall, `Post "` + base + `/v1/oauth/token": ` + sentence},
		"under a caller's id":          {fmt.Errorf("trade %s: %w", id, refused), sentence},
		"the call under a caller's id": {fmt.Errorf("trade %s: %w", id, inCall), `Post "` + base + `/v1/oauth/token": ` + sentence},
	} {
		t.Run(name, func(t *testing.T) {
			printed := redact.Error(row.err, base)
			if printed.Error() != row.want {
				t.Errorf("redact.Error = %q, want %q", printed, row.want)
			}
			if cause := redact.Cause(row.err, base).Error(); cause != sentence {
				t.Errorf("redact.Cause = %q, want %q", cause, sentence)
			}
			if strings.Contains(printed.Error(), string(id)) {
				t.Errorf("redact.Error = %q, which holds the caller's id", printed)
			}
			var reached *RefusedError
			if !errors.As(printed, &reached) || reached != refused {
				t.Errorf("errors.As(%q) does not reach the refusal", printed)
			}
		})
	}
}

// TestARefusalWhoseDescriptionHoldsALineBreakForgesNoLine: whoever answers at a clean broker
// address writes the description, and a line break in it would start a line of its own where the
// refusal prints, such as `legion controller start`'s stderr. redact prints such a refusal as
// x/oauth2's own text of the answer, which quotes the code and the description, so the words are
// all there on one line and the sentence is not.
func TestARefusalWhoseDescriptionHoldsALineBreakForgesNoLine(t *testing.T) {
	const base = "http://127.0.0.1:1"
	for _, description := range []string{
		"no entry lists this account\nagent-secrets: FORGED from the broker",
		"no entry lists this account\r\nagent-secrets: FORGED",
		"no entry lists this account\x1b[2Kagent-secrets: FORGED",
	} {
		refused := newRefusedError(400, "invalid_grant", description)
		want := fmt.Sprintf("oauth2: %q %q", "invalid_grant", description)
		inCall := &url.Error{Op: "Post", URL: base + "/v1/oauth/token", Err: refused}
		for what, got := range map[string]string{
			"redact.Error":           redact.Error(refused, base).Error(),
			"redact.Cause":           redact.Cause(refused, base).Error(),
			"redact.Error in a call": redact.Error(inCall, base).Error(),
			"redact.Cause of a call": redact.Cause(inCall, base).Error(),
		} {
			if strings.ContainsAny(got, "\r\n\x1b") {
				t.Errorf("%s of a description %q = %q, which holds a control character", what, description, got)
			}
			if !strings.HasSuffix(got, want) {
				t.Errorf("%s of a description %q = %q, want it to end with the answer's own text %q", what, description, got, want)
			}
		}
	}
}

// refusalBody is the broker's own words for the refusal that costs the most to diagnose: the
// deployment's entry fixes no session, so a trade that names none mints nothing.
const refusalBody = "the fleet example-fleet is listed with no session of its own, so the request names one"

// TestARefusalCarriesTheBrokersWordsThroughTheRedaction: the refusal reaches the caller as the
// answered HTTP refusal it is, so redact prints it whole beside a broker URL that carries no
// credential — a caller reads why its trade was refused without attaching a debugger.
func TestARefusalCarriesTheBrokersWordsThroughTheRedaction(t *testing.T) {
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 400, map[string]any{"error": "invalid_request", "error_description": refusalBody}
	})
	err := refusalClient(t, broker).Run(context.Background(), func(context.Context, *Token) error {
		t.Error("the loop handed its caller a token the broker refused to mint")
		return nil
	})
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Run = %v, want the broker's refusal", err)
	}
	printed := redact.Error(err, broker.URL()).Error()
	if !strings.Contains(printed, refusalBody) || !strings.Contains(printed, "invalid_request") {
		t.Errorf("the redacted refusal is %q, want the broker's own words and its OAuth code in it", printed)
	}
}

// TestARefusalBesideABrokerURLThatCarriesACredentialNamesTheStatus: the same refusal where the
// configured URL holds a credential, which redact never prints any part of. The caller is told
// the broker answered and with what status — not merely that the request failed — and no byte of
// that URL, of the identity traded, or of the proof is in the message.
func TestARefusalBesideABrokerURLThatCarriesACredentialNamesTheStatus(t *testing.T) {
	broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
		return 403, map[string]any{"error": "access_denied", "error_description": refusalBody}
	})
	err := refusalClient(t, broker).Run(context.Background(), func(context.Context, *Token) error {
		t.Error("the loop handed its caller a token the broker refused to mint")
		return nil
	})
	const carriesACredential = "https://machine:hunter2pw@secrets.example"
	printed := redact.Error(err, carriesACredential).Error()
	if printed != "the broker answered HTTP 403" {
		t.Errorf("the redacted refusal is %q, want the status alone: no text of a URL this one may not print", printed)
	}
	if strings.Contains(printed, "hunter2pw") || strings.Contains(printed, "projected.service.account.token") {
		t.Errorf("the redacted refusal is %q, which carries a credential", printed)
	}
}
