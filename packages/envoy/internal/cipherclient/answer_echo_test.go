package cipherclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestATradeAnswerPrintsNoSubjectTokenItEchoes: something in the broker's place answers the trade
// and echoes the subject token its form carried, in a status's words that are no OAuth refusal (a
// 302 and a 502, each printed as the broker's words beside its status) and in the values of a 200
// that is not this grant's (its token_type and its credential_expires_at). The trade's error, and
// the log line the renewal loop writes of it (retriedTrade), hold none of it: every answer the
// trade reads prints through the record of the request it sent (redact.Carried), whatever its
// status, and a 200's values print nowhere, as in the TypeScript client. Captures
// brokerclient.answer-echo/<case> and brokerclient.answer-echo/<case>/log.
func TestATradeAnswerPrintsNoSubjectTokenItEchoes(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   map[string]any
	}{
		{"302-words", 302, map[string]any{"error": "moved; the form was subject_token={subject}"}},
		{"502-words", 502, map[string]any{"error": "upstream refused subject_token={subject}"}},
		{"200-token-type", 200, map[string]any{"access_token": "a", "token_type": "Bearer {subject}", "expires_in": 60}},
		{"200-credential-expires-at", 200, map[string]any{"access_token": "a", "token_type": "DPoP", "expires_in": 60, "credential_expires_at": "{subject}"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			broker := newStubBroker(t, func(_ int, form url.Values) (int, map[string]any) {
				echoed := map[string]any{}
				for k, v := range c.body {
					if s, ok := v.(string); ok {
						v = strings.ReplaceAll(s, "{subject}", form.Get("subject_token"))
					}
					echoed[k] = v
				}
				return c.status, echoed
			})
			client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t),
				Subject: &scriptedSource{token: redacttest.Token, tokenType: "urn:envoy:params:oauth:token-type:kubernetes-service-account", describe: "probe subject"}})
			if err != nil {
				t.Fatal(err)
			}
			_, tradeErr := client.trade(context.Background())
			if tradeErr == nil {
				t.Fatal("the trade answered no error")
			}
			if len(broker.Requests()) != 1 {
				t.Fatalf("the broker saw %d trades, want the one", len(broker.Requests()))
			}
			redacttest.RequireNone(t, "brokerclient.answer-echo/"+c.name, tradeErr.Error())
			msg, args := client.retriedTrade(tradeErr)
			var line bytes.Buffer
			slog.New(slog.NewTextHandler(&line, nil)).Warn(msg, args...)
			redacttest.RequireNone(t, "brokerclient.answer-echo/"+c.name+"/log", line.String())
		})
	}
}

// TestARefusalBesideACredentialedAddressPrintsNoneOfItsWords: the configured broker address
// carries a credential in its fragment, which no request sends and so no request record names,
// and whatever answers at that address refuses the trade with an OAuth error whose description
// repeats it. The trade's error prints the refusal's status and none of its words: post wraps the
// refusal in the address's Error (cipheraddr.Address.Error), which beside an address that is not
// clean withholds every word of the answer (R4). Captures brokerclient.refusal-beside-credential.
func TestARefusalBesideACredentialedAddressPrintsNoneOfItsWords(t *testing.T) {
	base := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"the address ` + redacttest.Token + ` names no broker"}`))
	})
	client, err := New(Config{BrokerURL: cipheraddr.New(base + "#" + redacttest.Token), Key: testKey(t),
		Subject: &scriptedSource{token: "probe-subject", tokenType: "urn:envoy:params:oauth:token-type:kubernetes-service-account", describe: "probe subject"}})
	if err != nil {
		t.Fatal(err)
	}
	_, tradeErr := client.trade(context.Background())
	if tradeErr == nil {
		t.Fatal("the trade answered no error")
	}
	var refused *RefusedError
	if !errors.As(tradeErr, &refused) || !refused.HasCode("invalid_grant") {
		t.Errorf("the trade's error is %v, want the refusal with the code the answer gave", tradeErr)
	}
	redacttest.RequireNone(t, "brokerclient.refusal-beside-credential", tradeErr.Error())
	if !strings.Contains(tradeErr.Error(), "400") {
		t.Errorf("the trade's error is %q, want its status", tradeErr)
	}
}

// TestARetriedRefusalLogsItsCodeAsTheAddressLetsItPrint: whatever answers at the configured broker
// address refuses two trades with a 429, which the client retries, and its code and description
// repeat a credential. Each line the client logs of a retried trade - Token's while the token in
// hand is live, and Run's before it asks again - prints the refusal's status, and its code as the
// address lets the answer's text print (cipheraddr.Address.WordsOf), as the trade's error prints
// it: beside an address that carries a credential in its user information, its query or its
// fragment, cipheraddr.WordsNotPrinted, since whoever answers there can repeat that credential in
// any text of its answer, and a fragment, which no request sends, is a credential the request's
// record cannot redact; beside a clean address, the code as the rule reads it. Captures
// brokerclient.retried-trade/<case>.
func TestARetriedRefusalLogsItsCodeAsTheAddressLetsItPrint(t *testing.T) {
	for _, c := range []struct {
		name, suffix, code, want string
		credentialed             bool
	}{
		{"user-information", "", redacttest.Password, cipheraddr.WordsNotPrinted, true},
		{"query", "/?access_token=" + redacttest.Token, redacttest.Token, cipheraddr.WordsNotPrinted, true},
		{"fragment", "#access_token=" + redacttest.Token, redacttest.Token, cipheraddr.WordsNotPrinted, true},
		{"clean", "", redacttest.Code, redacttest.CodeShown, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var trades atomic.Int64
			base := newRawServer(t, func(w http.ResponseWriter, _ *http.Request) {
				status, body := http.StatusTooManyRequests, map[string]any{"error": c.code, "error_description": "the address " + c.code + " is busy"}
				switch trades.Add(1) {
				case 1:
					status, body = http.StatusOK, mintedToken(1)
				case 4:
					status, body = http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "no entry lists this account"}
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(body)
			})
			address := base + c.suffix
			if c.name == "user-information" {
				address = strings.Replace(base, "://", "://"+redacttest.User+":"+redacttest.Password+"@", 1)
			}
			var logged bytes.Buffer
			client, err := New(Config{BrokerURL: cipheraddr.New(address), Key: testKey(t), Subject: projectedStub(), RenewBefore: time.Minute,
				Log: slog.New(slog.NewJSONHandler(&logged, nil)),
				After: func(time.Duration) <-chan time.Time {
					now := make(chan time.Time, 1)
					now <- time.Now()
					return now
				}})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if cipheraddr.New(address).Clean() == c.credentialed {
				t.Fatalf("the address of case %s reads as clean %t, want %t", c.name, c.credentialed, !c.credentialed)
			}
			ctx := context.Background()
			if _, err := client.Token(ctx); err != nil {
				t.Fatalf("the first trade: %v", err)
			}
			client.held.Load().Expiry = time.Now().Add(30 * time.Second)
			if _, err := client.Token(ctx); err != nil {
				t.Fatalf("Token = %v; want the token in hand, which is still live", err)
			}
			if err := client.Run(ctx, func(context.Context, *Token) error { return nil }); err == nil {
				t.Fatal("Run ended with no error, want the broker's last refusal")
			}
			redacttest.RequireNone(t, "brokerclient.retried-trade/"+c.name, logged.String())
			var lines int
			for line := range strings.Lines(logged.String()) {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatalf("the client logged %q, which is no JSON line: %v", line, err)
				}
				if msg, _ := record["msg"].(string); !strings.HasPrefix(msg, retriedRefusalLine) {
					continue
				}
				lines++
				if record["code"] != c.want || record["status"] != float64(http.StatusTooManyRequests) {
					t.Errorf("the line %q names the code %v and the status %v; want %q and 429", record["msg"], record["code"], record["status"], c.want)
				}
			}
			if lines != 2 {
				t.Errorf("the client logged %d lines of a retried refusal, want Token's and Run's: %s", lines, logged.String())
			}
		})
	}
}
