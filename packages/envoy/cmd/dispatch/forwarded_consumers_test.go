package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The rows' requests come from a proxy DISPATCH_TRUSTED_PROXY_RANGES names (127.0.0.1/32), which
// appends to X-Forwarded-For as Google's internal Application Load Balancer does: what the caller
// wrote (198.51.100.66), the client it saw, and its forwarding rule's address (10.0.0.9), the same
// for every caller.
const (
	trustedProxyPeer = "127.0.0.1:40000"
	proxiedClient    = "203.0.113.7"
)

// proxiedFor is the X-Forwarded-For line the trusted proxy sends for client.
func proxiedFor(client string) string {
	return "198.51.100.66, " + client + ", 10.0.0.9"
}

// TestEveryConsumerOfTheClientAddressReadsTheResolvedOne: on the server main builds with
// DISPATCH_TRUSTED_PROXY_RANGES=127.0.0.1/32, every consumer of the client address keys a request
// through that proxy on the client the proxy appended, second from the right, the one address the
// middleware around the router resolves: the request's dispatch: request line, the line of a
// request authenticated with a previous shared agent token, and the broker token check's
// per-address bucket (brokerTokenVerifier's Address). A consumer that read the header for itself
// would key every caller behind the proxy on its forwarding rule's address, the rightmost entry.
func TestEveryConsumerOfTheClientAddressReadsTheResolvedOne(t *testing.T) {
	// The server's request goroutines write the log while the test reads it.
	var logged cmdtest.LockedBuffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logged, nil))
	broker := ciphertokentest.NewStubBroker(t)
	boot := resolveWith(t, map[string]string{"DISPATCH_AGENT_TOKEN": "new-token\nold-token", "DISPATCH_TRUSTED_PROXY_RANGES": "127.0.0.1/32"})
	boot.AgentSecretsURL = broker.Server.URL
	boot.AgentSecretsTokenFile = writeFile(t, "account-token", "dispatch-account-token\n")
	database := storetest.Open(t)
	verifier, err := brokerTokenVerifier(boot, database, brokerOrigin)
	if err != nil {
		t.Fatalf("build the broker-token check: %v", err)
	}
	handler := routerFor(t, boot, routes.AppContextOptions{Store: database, ServerURL: brokerOrigin, BrokerTokens: verifier})
	// whoamiThrough is GET /api/v1/whoami from client through the trusted proxy, presenting
	// authorization.
	whoamiThrough := func(client, authorization string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, brokerOrigin+"/api/v1/whoami", nil)
		request.RemoteAddr = trustedProxyPeer
		request.Header.Set("X-Forwarded-For", proxiedFor(client))
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// loggedAddress is the address of the first line message names that the log holds past before
	// lines.
	loggedAddress := func(t *testing.T, before int, message string) any {
		t.Helper()
		line, found := logtest.LineWhere(logtest.JSONLines(t, logged.String())[before:], func(line map[string]any) bool { return line["msg"] == message })
		if !found {
			t.Fatalf("no %q line in the log:\n%s", message, logged.String())
		}
		return line["address"]
	}

	t.Run("the access log's address", func(t *testing.T) {
		before := len(logtest.JSONLines(t, logged.String()))
		if response := whoamiThrough(proxiedClient, "Bearer new-token"); !answersSharedToken(response) {
			t.Fatalf("GET /api/v1/whoami with the shared token answered %d %s, want 200 naming it", response.Code, response.Body.String())
		}
		if got := loggedAddress(t, before, accesslog.Message); got != proxiedClient {
			t.Errorf("the dispatch: request line of a request through the proxy for %s names the address %v, want %s", proxiedFor(proxiedClient), got, proxiedClient)
		}
	})

	t.Run("the previous-token line's address", func(t *testing.T) {
		before := len(logtest.JSONLines(t, logged.String()))
		if response := whoamiThrough(proxiedClient, "Bearer old-token"); !answersSharedToken(response) {
			t.Fatalf("GET /api/v1/whoami with the previous shared token answered %d %s, want 200 naming the shared token", response.Code, response.Body.String())
		}
		if got := loggedAddress(t, before, "dispatch: request authenticated with a previous shared agent token"); got != proxiedClient {
			t.Errorf("the previous-token line of a request through the proxy for %s names the address %v, want %s", proxiedFor(proxiedClient), got, proxiedClient)
		}
	})

	// The broker token check spends a bucket per address on each token it asks the broker about
	// (ciphertoken.DefaultAddressLimit). The client runs its bucket out with tokens the broker does
	// not hold; then another client behind the same proxy, whose line differs from the first's in
	// its client entry alone, still has a bucket of its own.
	t.Run("the broker token limit's key", func(t *testing.T) {
		limited := false
		for attempt := 0; attempt < 200 && !limited; attempt++ {
			response := whoamiThrough(proxiedClient, fmt.Sprintf("DPoP a-token-the-broker-does-not-hold-%d", attempt))
			switch code := errorCode(response); {
			case response.Code == http.StatusTooManyRequests && code == "BROKER_TOKEN_RATE_LIMITED":
				limited = true
			case response.Code != http.StatusUnauthorized || code != "BROKER_TOKEN_NOT_ACTIVE":
				t.Fatalf("token %d from %s answered %d %s, want 401 BROKER_TOKEN_NOT_ACTIVE until its bucket runs out", attempt, proxiedClient, response.Code, response.Body.String())
			}
		}
		if !limited {
			t.Fatalf("200 tokens from %s never ran out its bucket", proxiedClient)
		}
		const otherClient = "203.0.113.8"
		if response := whoamiThrough(otherClient, "DPoP a-token-of-another-client"); response.Code != http.StatusUnauthorized || errorCode(response) != "BROKER_TOKEN_NOT_ACTIVE" {
			t.Errorf("a token from %s through the proxy (X-Forwarded-For %q), once %s ran out its bucket, answered %d %s; "+
				"want 401 BROKER_TOKEN_NOT_ACTIVE from a bucket of its own, keyed on the client the proxy appended and not on an entry the two share",
				otherClient, proxiedFor(otherClient), proxiedClient, response.Code, response.Body.String())
		}
	})
}
