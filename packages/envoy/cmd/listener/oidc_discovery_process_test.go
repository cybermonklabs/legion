package main

import (
	"encoding/json"
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
// cluster's issuer answers again, the projected token is admitted, with no restart.
// envoy_listener_oidc_issuer_discovered reads 0 for both issuers while neither can be read, and
// then 1 for the cluster's and still 0 for Google's, and /healthz's "oidc" and "google" fields read
// false and false, then true and false. Each issuer's failed discovery is logged once a minute at
// most, though the listener tries it again every few seconds. Google's issuer is the real
// accounts.google.com, which the listener's own HTTPS_PROXY makes unreachable here; the cluster's
// stand-in is on loopback, which no proxy takes.
func TestTheListenerServesWhileItsIssuersCannotBeDiscovered(t *testing.T) {
	cluster := oidctest.New(t)
	key := cluster.PublishKey(t, "listener-discovery-key")
	cluster.FailDiscovery(http.StatusServiceUnavailable)
	projected := cluster.Mint(t, key, cluster.Claims(listenerListedSubject, listenerTestAudience))
	googleClaims := cluster.Claims(listenerGoogleAccount, listenerGoogleAudience)
	googleClaims["iss"] = oidc.GoogleIssuer
	google := cluster.Mint(t, key, googleClaims)

	started := time.Now()
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
	wantIssuerGauge(t, base, map[string]int{issuerCluster: 0, issuerGoogle: 0})
	wantIssuerHealth(t, base, false, false)

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
	wantIssuerGauge(t, base, map[string]int{issuerCluster: 1, issuerGoogle: 0})
	wantIssuerHealth(t, base, true, false)

	// Google's issuer never answers here, so its failure is logged again each minute the test
	// runs; a listener that logged every failed round would write several lines within seconds.
	output := listener.Output.String()
	most := 1 + int(time.Since(started)/time.Minute)
	for _, issuer := range []string{cluster.URL(), oidc.GoogleIssuer} {
		if lines := countLines(output, discoveryFailedLine, "issuer="+issuer); lines < 1 || lines > most {
			t.Fatalf("the listener logged %d failed discoveries of %s in %s, want 1 to %d:\n%s", lines, issuer, time.Since(started).Round(time.Second), most, output)
		}
	}
}

// wantIssuerHealth fails t unless the listener at base's /healthz answers "oidc" and "google" as
// the booleans named.
func wantIssuerHealth(t *testing.T, base string, oidcRead, googleRead bool) {
	t.Helper()
	_, answered := do(t, mustRequest(t, http.MethodGet, base+"/healthz"))
	var body map[string]any
	if err := json.Unmarshal([]byte(answered), &body); err != nil {
		t.Fatalf("/healthz answered %q: %v", answered, err)
	}
	if body["oidc"] != oidcRead || body["google"] != googleRead {
		t.Fatalf("/healthz answers oidc %v and google %v, want %t and %t: %s", body["oidc"], body["google"], oidcRead, googleRead, answered)
	}
}

// wantIssuerGauge fails t unless the listener at base's /metrics carries
// envoy_listener_oidc_issuer_discovered with exactly the value want names for each issuer.
func wantIssuerGauge(t *testing.T, base string, want map[string]int) {
	t.Helper()
	_, scraped := do(t, mustRequest(t, http.MethodGet, base+"/metrics"))
	for issuer, value := range want {
		line := `envoy_listener_oidc_issuer_discovered{issuer="` + issuer + `"} ` + strconv.Itoa(value)
		if !strings.Contains(scraped, line+"\n") {
			t.Fatalf("/metrics does not carry %s:\n%s", line, scraped)
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
