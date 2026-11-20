//go:build linux

package helper

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// helperWordsRows are the broker's words that quote a URL carrying a credential, each with what
// the rule for remote text prints of them.
var helperWordsRows = []struct{ name, words, shown string }{
	{"user information and a query value", redacttest.Words, redacttest.Shown},
	{"a fragment's value", redacttest.FragmentWords, redacttest.FragmentShown},
	{"words the rule cannot read", redacttest.Unreadable, redact.WordsNotReadable},
}

// TestTheStoppedTradeLogsTheBrokersWordsAsTheRuleReadsThem: the broker refuses this machine's
// identity with words that quote a URL carrying a credential. The helper's ERROR line, which goes
// to stderr and wherever the helper's log goes, prints the status, the code and the words as the
// rule reads them, and none of the credentials.
func TestTheStoppedTradeLogsTheBrokersWordsAsTheRuleReadsThem(t *testing.T) {
	for _, row := range helperWordsRows {
		t.Run(row.name, func(t *testing.T) {
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": row.words})
			}))
			t.Cleanup(broker.Close)
			key, err := proof.NewKey()
			if err != nil {
				t.Fatal(err)
			}
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("projected.service.account.token"), 0o600); err != nil {
				t.Fatal(err)
			}
			machine, err := brokerclient.New(brokerclient.Config{
				BrokerURL: brokeraddr.New(broker.URL), Key: key, HTTP: broker.Client(), Subject: brokerclient.NewProjectedToken(tokenFile),
			})
			if err != nil {
				t.Fatal(err)
			}
			var logged syncBuffer
			b := &Broker{
				URL: brokeraddr.New(broker.URL), Identity: machine, HTTP: broker.Client(),
				Log: slog.New(slog.NewTextHandler(&logged, nil)), OperatorFile: filepath.Join(t.TempDir(), "no-operator-here"),
			}
			t.Cleanup(b.Close)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			if err := b.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			waitFor(t, func() bool {
				return strings.Contains(logged.String(), "the helper stopped trading this machine's identity")
			})
			line := logged.String()
			redacttest.RequireNone(t, "helper.stopped-trade", line)
			if want := "HTTP 400 invalid_grant: " + row.shown; !strings.Contains(line, want) {
				t.Errorf("the helper logged %q, want the broker's status, code and words in it as %q", line, want)
			}
		})
	}
}

// TestABrokerErrorPrintsTheBrokersWordsAsTheRuleReadsThem: the broker refuses a renew in its error
// envelope with words that quote a URL carrying a credential. The BrokerError the renew returns
// prints the status, the code and the words as the rule reads them, under every verb and in a
// value that holds it, and none of the credentials; the helper still reads its code.
func TestABrokerErrorPrintsTheBrokersWordsAsTheRuleReadsThem(t *testing.T) {
	type answer struct{ code, words, want string }
	rows := map[string]answer{"the code": {redacttest.Code, "no such slot", "broker 400 " + redacttest.CodeShown + ": no such slot"}}
	for _, row := range helperWordsRows {
		rows["the words: "+row.name] = answer{"INVALID_SLOT", row.words, "broker 400 INVALID_SLOT: " + row.shown}
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": row.code, "error": row.words})
			}))
			t.Cleanup(srv.Close)
			key, err := proof.NewKey()
			if err != nil {
				t.Fatal(err)
			}
			signIns := &stubSignIns{authority: &brokerclient.Authority{Key: key, CredentialID: "credential-1", Operator: "ada@example.com"}}
			b := &Broker{URL: brokeraddr.New(srv.URL), HTTP: srv.Client(), SignIns: signIns}
			t.Cleanup(b.Close)
			_, err = b.renewIdentity(context.Background(), identity{EnrollmentID: "enrollment-1", Key: key})
			var be *BrokerError
			if !errors.As(err, &be) || be.Status != http.StatusBadRequest || !be.HasCode(row.code) {
				t.Fatalf("the renew answered %v, want the broker's 400 as a BrokerError", err)
			}
			redacttest.RequireNoneFormatted(t, "helper.BrokerError", be)
			if be.Error() != row.want {
				t.Errorf("BrokerError = %q, want %q", be, row.want)
			}
		})
	}
}
