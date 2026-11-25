// packages/envoy/internal/cipherclient/tokentransport_url_test.go

package cipherclient

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADroppedTokensLineNamesTheURLOnlyAsTheRuleReadsIt: a caller's URL carries a credential in its
// query, and the server answers its request 401 BROKER_TOKEN_NOT_ACTIVE, so the transport drops
// the token and logs the request's URL. The line names the URL as the URL rule reads it
// (redact.URL) and holds no run of the credential (captures cipherclient.token-transport/url); the
// twin shows URL.Redacted, which hides a password alone, keeps it.
func TestADroppedTokensLineNamesTheURLOnlyAsTheRuleReadsIt(t *testing.T) {
	broker := newStubBroker(t, func(n int, _ url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": "token-" + strconv.Itoa(n), "token_type": "DPoP", "expires_in": 3600}
	})
	server := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"BROKER_TOKEN_NOT_ACTIVE","error":"the secrets broker does not hold this access token as live"}`)
	})
	var logged bytes.Buffer
	client, err := New(Config{BrokerURL: broker.Address(), Key: testKey(t), Subject: projectedStub(), Log: slog.New(slog.NewTextHandler(&logged, nil))})
	if err != nil {
		t.Fatal(err)
	}
	caller := &http.Client{Transport: NewTokenTransport(client, http.DefaultTransport)}
	address := server + "/api/v1/whoami?access_token=" + redacttest.Token

	t.Run("twin", func(t *testing.T) {
		u, err := url.Parse(address)
		if err != nil {
			t.Fatal(err)
		}
		if redacttest.Piece(u.Redacted(), redacttest.Token) == "" {
			t.Fatalf("URL.Redacted of %s holds no run of the credential, so this row proves nothing", u.Redacted())
		}
	})

	if status, _ := presentTo(t, caller, address); status != http.StatusUnauthorized {
		t.Fatalf("the request was answered %d, want the server's 401", status)
	}
	if !strings.Contains(logged.String(), "the next request trades again") {
		t.Fatalf("the transport logged %q, want the dropped token's line", logged.String())
	}
	redacttest.RequireNoPiece(t, "cipherclient.token-transport/url", logged.String(), redacttest.Token)
}
