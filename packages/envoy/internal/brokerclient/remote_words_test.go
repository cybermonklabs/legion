package brokerclient

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// remoteWordsRows are a broker's descriptions that quote a URL carrying a credential, each with
// what the rule for remote text prints of it: the URL with no user information and no query or
// fragment value where it can read the description, and redact.WordsNotReadable where it cannot.
var remoteWordsRows = []struct{ name, description, shown string }{
	{"user information and a query value", redacttest.Words, redacttest.Shown},
	{"a fragment's value", redacttest.FragmentWords, redacttest.FragmentShown},
	{"words the rule cannot read", redacttest.Unreadable, redact.WordsNotReadable},
}

// TestARefusalPrintsTheBrokersWordsAsTheRuleReadsThem: the broker refuses the trade with a code or
// a description that quotes a URL carrying a credential. The refusal Run returns prints the
// status, the code and the description as the rule reads them - in its own text, through
// redact.Error and redact.Cause beside a clean broker URL, and under every verb and in a value
// that holds it - and none of the credentials; errors.As still reaches the refusal and its OAuth
// answer through the redaction.
func TestARefusalPrintsTheBrokersWordsAsTheRuleReadsThem(t *testing.T) {
	type answer struct{ code, description, want string }
	const prefix = "the secrets broker refused this machine's identity: HTTP 400 "
	rows := map[string]answer{
		"the code, with a description":  {redacttest.Code, "revoked", prefix + redacttest.CodeShown + ": revoked"},
		"the code, with no description": {redacttest.Code, "", prefix + redacttest.CodeShown},
	}
	for _, row := range remoteWordsRows {
		rows["the description: "+row.name] = answer{"invalid_grant", row.description, prefix + "invalid_grant: " + row.shown}
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			broker := newStubBroker(t, func(int, url.Values) (int, map[string]any) {
				return 400, map[string]any{"error": row.code, "error_description": row.description}
			})
			err := refusalClient(t, broker).Run(context.Background(), func(context.Context, *Token) error {
				t.Error("the loop handed its caller a token the broker refused to mint")
				return nil
			})
			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Run = %v, want the broker's refusal", err)
			}
			redacttest.RequireNone(t, "brokerclient.refusal-text", err.Error())
			if err.Error() != row.want {
				t.Errorf("the refusal's text is %q, want %q", err, row.want)
			}
			for capture, printed := range map[string]string{
				"brokerclient.redact-Error": redact.Error(err, broker.URL()).Error(),
				"brokerclient.redact-Cause": redact.Cause(err, broker.URL()).Error(),
			} {
				redacttest.RequireNone(t, capture, printed)
			}
			redacttest.RequireNoneFormatted(t, "brokerclient.RefusedError", refused)
			var answer *oauth2.RetrieveError
			redacted := redact.Error(err, broker.URL())
			if !errors.As(redacted, &refused) || !errors.As(redacted, &answer) || !refused.HasCode(row.code) {
				t.Errorf("redact.Error(%v) does not reach the refusal and its OAuth answer through errors.As", err)
			}
		})
	}
}

// TestTheRenewalLoopLogsTheBrokersWordsAsTheRuleReadsThem: a refusal a later trade may cure (the
// broker's 429) is logged before the loop trades again, and the logged line holds the status, the
// code and the words as the rule reads them, and none of the credentials. The line prints the
// refusal through redact.Error, which reads its words again (redact.sanitized), so this row is
// the end-to-end line and the rows of the sentence and of sanitized each hold one of the two.
func TestTheRenewalLoopLogsTheBrokersWordsAsTheRuleReadsThem(t *testing.T) {
	for _, row := range remoteWordsRows {
		t.Run(row.name, func(t *testing.T) {
			broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
				if n == 0 {
					return 429, map[string]any{"error": "invalid_request", "error_description": row.description}
				}
				return 400, map[string]any{"error": "invalid_grant", "error_description": "no entry lists this account"}
			})
			var logged bytes.Buffer
			client, err := New(Config{
				BrokerURL: broker.Address(), Key: testKey(t), Subject: projectedStub(),
				Log: slog.New(slog.NewTextHandler(&logged, nil)),
				After: func(time.Duration) <-chan time.Time {
					now := make(chan time.Time, 1)
					now <- time.Now()
					return now
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := client.Run(context.Background(), func(context.Context, *Token) error { return nil }); err == nil {
				t.Fatal("Run ended with no error, want the broker's second refusal")
			}
			redacttest.RequireNone(t, "brokerclient.renewal-log", logged.String())
			if want := "HTTP 429 invalid_request: " + row.shown; !strings.Contains(logged.String(), want) {
				t.Errorf("the loop logged %q, want the refusal's status, code and words in it as %q", logged.String(), want)
			}
		})
	}
}

// TestARefusedSignInLogsTheBrokersCodeAsTheRuleReadsIt: the broker refuses a person's sign-in for
// good with a code that quotes a URL carrying a credential. The sign-in falls back to this
// machine, and the line that says so names the code as the rule reads it, and none of the
// credentials.
func TestARefusedSignInLogsTheBrokersCodeAsTheRuleReadsIt(t *testing.T) {
	base := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"` + redacttest.Code + `","error_description":"this sign-in was revoked"}`))
	})
	rig := newSignInRig(t, nil)
	rig.signIn(t, "ada@example.com")
	var logged bytes.Buffer
	rig.authority.BrokerURL = brokeraddr.New(base)
	rig.authority.Log = slog.New(slog.NewTextHandler(&logged, nil))
	if authority, err := rig.authority.Current(context.Background()); authority != nil || err != nil {
		t.Fatalf("a refused sign-in answered %+v, %v; want nil and no error", authority, err)
	}
	redacttest.RequireNone(t, "brokerclient.signin-log", logged.String())
	if !strings.Contains(logged.String(), `code="`+redacttest.CodeShown+`"`) {
		t.Errorf("the sign-in's log does not name the code as the rule reads it:\n%s", logged.String())
	}
}
