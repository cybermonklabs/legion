package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// brokerOrigin is the server URL the trusted proxy tests build the router and the broker token
// check with.
const brokerOrigin = "http://dispatch.test"

// TestTheTrustedProxyRangesSettingRefusals: DISPATCH_TRUSTED_PROXY_RANGES refuses startup on an
// empty entry, an entry that is not a CIDR, a CIDR not written as the network it names and a range
// of length 0, IPv4 or IPv6, which would trust every peer's X-Forwarded-For: each alone and each
// beside a good entry, naming the setting and the entry's position. Unset or blank, it trusts none.
func TestTheTrustedProxyRangesSettingRefusals(t *testing.T) {
	for _, row := range []struct{ value, naming string }{
		{"not-a-cidr", "DISPATCH_TRUSTED_PROXY_RANGES entry 1 of 1 must be a CIDR"},
		{"192.0.2.0/24,not-a-cidr", "DISPATCH_TRUSTED_PROXY_RANGES entry 2 of 2 must be a CIDR"},
		{"192.0.2.0/24,,2001:db8::/64", "DISPATCH_TRUSTED_PROXY_RANGES entry 2 of 3 is empty"},
		{"192.0.2.0/24,", "DISPATCH_TRUSTED_PROXY_RANGES entry 2 of 2 is empty"},
		{"10.0.0.1/8", "DISPATCH_TRUSTED_PROXY_RANGES entry 1 of 1 must be written as the network it names, 10.0.0.0/8"},
		{"192.0.2.0/24,2001:db8::1/64", "DISPATCH_TRUSTED_PROXY_RANGES entry 2 of 2 must be written as the network it names, 2001:db8::/64"},
		{"0.0.0.0/0", "DISPATCH_TRUSTED_PROXY_RANGES entry 1 of 1 trusts every peer's X-Forwarded-For"},
		{"::/0", "DISPATCH_TRUSTED_PROXY_RANGES entry 1 of 1 trusts every peer's X-Forwarded-For"},
		{"192.0.2.0/24,::/0", "DISPATCH_TRUSTED_PROXY_RANGES entry 2 of 2 trusts every peer's X-Forwarded-For"},
	} {
		t.Run(row.value, func(t *testing.T) {
			_, err := resolveBootConfig(bootEnvironment(map[string]string{"DISPATCH_TRUSTED_PROXY_RANGES": row.value}))
			if err == nil || !strings.Contains(err.Error(), row.naming) {
				t.Errorf("DISPATCH_TRUSTED_PROXY_RANGES=%q: boot answered %v, want a refusal naming %q", row.value, err, row.naming)
			}
		})
	}
	for _, value := range []string{"", "  "} {
		if boot := resolveWith(t, map[string]string{"DISPATCH_TRUSTED_PROXY_RANGES": value}); boot.TrustedProxyRanges != nil {
			t.Errorf("DISPATCH_TRUSTED_PROXY_RANGES=%q: TrustedProxyRanges = %s, want none", value, boot.TrustedProxyRanges)
		}
	}
}

// TestAConfiguredPeerIsReadSecondFromTheRight: on the server main builds with
// DISPATCH_TRUSTED_PROXY_RANGES=127.0.0.1/32, a request through that proxy, which appends to
// X-Forwarded-For as Google's internal Application Load Balancer does (the client it saw, then its
// forwarding rule's address, 10.0.0.9), is logged and keyed by the entry second from the right of
// its one line (netaddr.ForwardedClient): an IPv4 client as it is, an IPv6 client as its /64, and
// never an entry to its left, which the caller wrote. A header that proxy could not have written
// (two lines, one entry, none, or a second-to-last entry that is no address) is answered 400
// FORWARDED_FOR_INVALID before any credential is read: its one dispatch: request line names the
// proxy's own address, the broker is asked nothing, and no bucket is spent, so the address each
// such header would have keyed on under the rule for a peer in no range, its rightmost entry or
// the connection's own, still has its whole bucket.
//
// Controls: resolve a configured peer's header as a peer's in no range (the rightmost entry), and
// the accepted rows log 10.0.0.9 and the refusals reach the broker; read every refusal as the
// connection's address instead of answering 400, and the refusals answer 401 after asking the
// broker.
func TestAConfiguredPeerIsReadSecondFromTheRight(t *testing.T) {
	// The server's request goroutines write the log while the test reads it.
	var logged cmdtest.LockedBuffer
	logtest.SetDefault(t, slog.NewJSONHandler(&logged, nil))
	broker := ciphertokentest.NewStubBroker(t)
	boot := resolveWith(t, map[string]string{"DISPATCH_TRUSTED_PROXY_RANGES": "127.0.0.1/32"})
	boot.AgentSecretsURL = broker.Server.URL
	boot.AgentSecretsTokenFile = writeFile(t, "account-token", "dispatch-account-token\n")
	database := storetest.Open(t)
	verifier, err := brokerTokenVerifier(boot, database, brokerOrigin)
	if err != nil {
		t.Fatalf("build the broker-token check: %v", err)
	}
	handler := routerFor(t, boot, routes.AppContextOptions{Store: database, ServerURL: brokerOrigin, BrokerTokens: verifier})
	// through is GET /api/v1/whoami from the trusted proxy, presenting authorization and carrying
	// the X-Forwarded-For lines forwarded.
	through := func(authorization string, forwarded ...string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, brokerOrigin+"/api/v1/whoami", nil)
		request.RemoteAddr = trustedProxyPeer
		for _, line := range forwarded {
			request.Header.Add("X-Forwarded-For", line)
		}
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// requestLines are the dispatch: request lines the log holds past before lines.
	requestLines := func(t *testing.T, before int) []map[string]any {
		t.Helper()
		var lines []map[string]any
		for _, line := range logtest.JSONLines(t, logged.String())[before:] {
			if line["msg"] == accesslog.Message {
				lines = append(lines, line)
			}
		}
		return lines
	}

	for _, row := range []struct{ name, forwarded, want string }{
		{"an IPv4 client", "203.0.113.7, 10.0.0.9", "203.0.113.7"},
		{"an IPv6 client, by its /64", "2001:db8:7:1::9, 10.0.0.9", "2001:db8:7:1::/64"},
		{"a client entry a caller wrote to the left", "198.51.100.66, 203.0.113.7, 10.0.0.9", "203.0.113.7"},
	} {
		t.Run(row.name, func(t *testing.T) {
			before := len(logtest.JSONLines(t, logged.String()))
			if response := through("Bearer agent-token", row.forwarded); !answersSharedToken(response) {
				t.Fatalf("GET /api/v1/whoami with the shared token through the proxy (X-Forwarded-For %q) answered %d %s, want 200 naming it", row.forwarded, response.Code, response.Body.String())
			}
			lines := requestLines(t, before)
			if len(lines) != 1 || lines[0]["address"] != row.want {
				t.Errorf("a request through the proxy with X-Forwarded-For %q logged %v, want one dispatch: request line whose address is %s, second from the right", row.forwarded, lines, row.want)
			}
		})
	}

	// Each refusal is sent once more than a bucket holds, each with a token the broker does not
	// hold: had one reached the broker token check, it would have asked the broker, and the
	// bucket of the address the request would key on as a peer in no range (spent) would be run
	// out, so the request through the proxy that keys on that address right after would be 429.
	for _, row := range []struct {
		name      string
		forwarded []string
		spent     string
	}{
		{"two lines", []string{"198.51.100.8", "198.51.100.66"}, "198.51.100.66"},
		{"one entry", []string{"198.51.100.8"}, "198.51.100.8"},
		{"no header", nil, "127.0.0.1"},
		{"a second-to-last entry that is no address", []string{"not-an-address, 10.0.0.9"}, "10.0.0.9"},
	} {
		t.Run(row.name, func(t *testing.T) {
			before, asked := len(logtest.JSONLines(t, logged.String())), broker.Count()
			refusals := ciphertoken.DefaultAddressLimit.Burst + 1
			for attempt := range refusals {
				response := through(fmt.Sprintf("DPoP a-token-the-broker-does-not-hold-%d", attempt), row.forwarded...)
				if response.Code != http.StatusBadRequest || errorCode(response) != "FORWARDED_FOR_INVALID" {
					t.Fatalf("request %d through the proxy with X-Forwarded-For %q answered %d %s, want 400 FORWARDED_FOR_INVALID", attempt, row.forwarded, response.Code, response.Body.String())
				}
			}
			if got := broker.Count() - asked; got != 0 {
				t.Errorf("%d refused requests asked the broker %d times, want none", refusals, got)
			}
			lines := requestLines(t, before)
			if len(lines) != refusals {
				t.Fatalf("%d refused requests wrote %d dispatch: request lines, want one each", refusals, len(lines))
			}
			for _, line := range lines {
				if line["route"] != accesslog.RefusedRoute || line["address"] != "127.0.0.1" || line["status"] != float64(http.StatusBadRequest) {
					t.Fatalf("a refused request's line = %v, want route %s, address 127.0.0.1, the proxy's own, and status 400", line, accesslog.RefusedRoute)
				}
			}
			forSpent := row.spent + ", 10.0.0.9"
			if response := through("DPoP a-token-of-a-caller-at-"+row.spent, forSpent); response.Code != http.StatusUnauthorized || errorCode(response) != "BROKER_TOKEN_NOT_ACTIVE" {
				t.Errorf("a token through the proxy keyed on %s (X-Forwarded-For %q), right after the refusals, answered %d %s; want 401 BROKER_TOKEN_NOT_ACTIVE from a bucket no refusal spent",
					row.spent, forSpent, response.Code, response.Body.String())
			}
		})
	}
}
