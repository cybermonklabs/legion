package auth_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The rows' request comes from a proxy DISPATCH_TRUSTED_PROXY_RANGES names (127.0.0.1/32), which
// appends to X-Forwarded-For as Google's internal Application Load Balancer does: what the caller
// wrote (198.51.100.66), the client it saw (203.0.113.7), and its forwarding rule's address
// (10.0.0.9), the same for every caller.
const (
	trustedProxyPeer = "127.0.0.1:40000"
	proxiedFor       = "198.51.100.66, 203.0.113.7, 10.0.0.9"
	proxiedClient    = "203.0.113.7"
)

// TestEveryConsumerOfTheClientAddressReadsTheResolvedOne: the client address the middleware around
// the router resolves under the configured ranges (ResolveForwardedAddress) and hands on in the
// request's context (WithForwardedAddress) is the one every consumer of the client address keys
// on: the access log's address, the previous-token line's address and the broker token limit's
// key, auth.ForwardedAddress, the function cmd/dispatch's brokerTokenVerifier hands the verifier.
// A consumer that read the header for itself would key every caller behind the proxy on the
// forwarding rule's address, the rightmost entry.
func TestEveryConsumerOfTheClientAddressReadsTheResolvedOne(t *testing.T) {
	var logged logtest.Buffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logged, nil))
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	// resolved is a request for path from the trusted proxy as the middleware hands it on.
	resolved := func(t *testing.T, path string) *http.Request {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = trustedProxyPeer
		request.Header.Set("X-Forwarded-For", proxiedFor)
		address, err := auth.ResolveForwardedAddress(request, trusted)
		if err != nil {
			t.Fatalf("resolve the client of a request from %s with X-Forwarded-For %q: %v", trustedProxyPeer, proxiedFor, err)
		}
		return request.WithContext(auth.WithForwardedAddress(request.Context(), address))
	}
	// loggedAddress is the address of the line message names that the log holds past before lines.
	loggedAddress := func(t *testing.T, before int, message string) any {
		t.Helper()
		line, found := logtest.LineWhere(logtest.JSONLines(t, logged.String())[before:], func(line map[string]any) bool { return line["msg"] == message })
		if !found {
			t.Fatalf("no %q line in the log:\n%s", message, logged.String())
		}
		return line["address"]
	}

	for _, consumer := range []struct {
		name    string
		address func(t *testing.T) any
	}{
		{"the access log's address", func(t *testing.T) any {
			before := len(logtest.JSONLines(t, logged.String()))
			accesslog.HandlerFunc("/api/v1/whoami", func(http.ResponseWriter, *http.Request) {})(httptest.NewRecorder(), resolved(t, "/api/v1/whoami"))
			return loggedAddress(t, before, accesslog.Message)
		}},
		{"the previous-token line's address", func(t *testing.T) any {
			tokens, err := auth.ParseSharedAgentTokens("new-token old-token")
			if err != nil {
				t.Fatal(err)
			}
			before := len(logtest.JSONLines(t, logged.String()))
			if !auth.MatchesSharedAgentToken(resolved(t, "/api/v1/whoami"), "old-token", tokens) {
				t.Fatal("the previous shared token was refused")
			}
			return loggedAddress(t, before, "dispatch: request authenticated with a previous shared agent token")
		}},
		{"the broker token limit's key", func(t *testing.T) any {
			return auth.ForwardedAddress(resolved(t, "/api/v1/whoami"))
		}},
	} {
		t.Run(consumer.name, func(t *testing.T) {
			if got := consumer.address(t); got != proxiedClient {
				t.Errorf("%s for a request from %s with X-Forwarded-For %q = %v, want %s, the client the trusted proxy appended", consumer.name, trustedProxyPeer, proxiedFor, got, proxiedClient)
			}
		})
	}
}
