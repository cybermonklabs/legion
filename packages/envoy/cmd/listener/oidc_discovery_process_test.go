package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// discoveryFailedLine is the line the listener logs for an issuer whose discovery failed, at most
// once a minute for each issuer however often it tries again (oidc.LazyVerifier.Run).
const discoveryFailedLine = "oidc: discover the issuer; retrying in the background"

// The listener reads neither issuer's discovery document at its start: started while the cluster's
// issuer and Google's both cannot be read, it serves /healthz, and a caller on the shared token, at
// once. A projected token and a Google identity token are each answered 503 with Retry-After, never
// 401, while their issuer cannot be read, since the listener could not check them. Once the
// cluster's issuer answers again, the projected token is admitted, with no restart. Each issuer's
// failed discovery is logged once in this test's minute, though the listener tries it again every
// few seconds. Google's issuer is the real accounts.google.com, which the listener's own
// HTTPS_PROXY makes unreachable here; the cluster's stand-in is on loopback, which no proxy takes.
func TestTheListenerServesWhileItsIssuersCannotBeDiscovered(t *testing.T) {
	cluster := oidctest.New(t)
	key := cluster.PublishKey(t, "listener-discovery-key")
	cluster.FailDiscovery(http.StatusServiceUnavailable)
	projected := cluster.Mint(t, key, cluster.Claims(listenerListedSubject, listenerTestAudience))
	googleClaims := cluster.Claims(listenerGoogleAccount, listenerGoogleAudience)
	googleClaims["iss"] = oidc.GoogleIssuer
	google := cluster.Mint(t, key, googleClaims)

	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), testnats.URL(t), "oidc-discovery-down",
		"ENVOY_OIDC_ISSUER="+cluster.URL(), "ENVOY_OIDC_AUDIENCE="+listenerTestAudience, "ENVOY_OIDC_ACCOUNTS="+listenerListedSubject,
		"ENVOY_OIDC_GOOGLE_AUDIENCE="+listenerGoogleAudience, "ENVOY_OIDC_GOOGLE_ACCOUNTS="+listenerGoogleAccount,
		"HTTPS_PROXY=http://127.0.0.1:1")
	listener.waitHealthy(t)
	base := "http://127.0.0.1:" + strconv.Itoa(listener.port)
	call := func(bearer string) *http.Response {
		t.Helper()
		request := mustRequest(t, http.MethodGet, base+"/v1/sessions")
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET /v1/sessions: %v", err)
		}
		_ = response.Body.Close()
		return response
	}

	if response := call(listenerTestToken); response.StatusCode != http.StatusOK {
		t.Fatalf("the shared token while neither issuer can be read: status %d, want 200\n%s", response.StatusCode, listener.Output.String())
	}
	for name, token := range map[string]string{"a projected token": projected, "a Google identity token": google} {
		if response := call(token); response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") == "" {
			t.Fatalf("%s while its issuer cannot be read: status %d, Retry-After %q; want 503 with Retry-After, never 401\n%s",
				name, response.StatusCode, response.Header.Get("Retry-After"), listener.Output.String())
		}
	}

	cluster.FailDiscovery(0)
	deadline := time.Now().Add(45 * time.Second)
	for {
		status := call(projected).StatusCode
		if status == http.StatusOK {
			break
		}
		if status != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("a projected token once its issuer answers: status %d, want 200 within 45 s with no restart\n%s", status, listener.Output.String())
		}
		time.Sleep(250 * time.Millisecond)
	}
	if response := call(google); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a Google identity token while Google's issuer still cannot be read: status %d, want 503", response.StatusCode)
	}

	output := listener.Output.String()
	for _, issuer := range []string{cluster.URL(), oidc.GoogleIssuer} {
		if lines := countLines(output, discoveryFailedLine, "issuer="+issuer); lines != 1 {
			t.Fatalf("the listener logged %d failed discoveries of %s in this test's minute, want 1:\n%s", lines, issuer, output)
		}
	}
}

// countLines is how many of output's lines carry every one of words.
func countLines(output string, words ...string) int {
	count := 0
	for line := range strings.SplitSeq(output, "\n") {
		holds := true
		for _, word := range words {
			holds = holds && strings.Contains(line, word)
		}
		if holds {
			count++
		}
	}
	return count
}
