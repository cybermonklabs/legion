package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestAKeySetLogsEachChangeItsReadsFindOnce: a key set the broker logs (logKeySet) writes one WARN
// line when a read of it fails, however many reads fail after it, naming the issuer and the key
// set's address; one INFO line when a read succeeds after that; nothing while reads keep
// succeeding; and a WARN line again when a read fails after the recovery. A key the cached set
// holds, answered while every read behind it fails, writes the WARN line and no INFO line: the key
// set could not be read, whatever the cache answered.
func TestAKeySetLogsEachChangeItsReadsFindOnce(t *testing.T) {
	logged := captureLog(t)
	issuer := oidctest.New(t)
	issuer.PublishKey(t, "issuer-key")
	keys, err := oidc.NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatal(err)
	}
	// Every Key reads the set: no lifetime, no spacing of a miss and none of a failure.
	keys.Lifetime, keys.MinRefresh, keys.FailureSpacing = 0, 0, 0
	logKeySet(keys, issuer.URL())
	lines := func(message string) []string { return logLines(logged, message) }
	read := func(what string) {
		t.Helper()
		if _, err := keys.Key(context.Background(), "issuer-key", "RS256"); err != nil {
			t.Fatalf("%s: Key of the key the set holds = %v, want the key", what, err)
		}
	}

	read("the first read")
	if got := len(lines(keySetUnreadableMessage)) + len(lines(keySetReadableMessage)); got != 0 {
		t.Fatalf("a first read that succeeded wrote %d lines, want none:\n%s", got, logged)
	}

	issuer.FailKeys(http.StatusServiceUnavailable)
	for range 5 {
		read("a read that fails, the cached set holding the key")
	}
	found := lines(keySetUnreadableMessage)
	if len(found) != 1 {
		t.Fatalf("five failed reads wrote %d %q lines, want one:\n%s", len(found), keySetUnreadableMessage, logged)
	}
	for _, want := range []string{"level=WARN", "url=" + issuer.URL(), "keys=" + issuer.URL() + "/keys", "key set could not be read"} {
		if !strings.Contains(found[0], want) {
			t.Fatalf("the line %q does not carry %q", found[0], want)
		}
	}
	if found := lines(keySetReadableMessage); len(found) != 0 {
		t.Fatalf("the cached key answered while the reads failed wrote %v, want no %q line", found, keySetReadableMessage)
	}

	issuer.FailKeys(0)
	for range 3 {
		read("a read once the key set answers")
	}
	found = lines(keySetReadableMessage)
	if len(found) != 1 {
		t.Fatalf("three good reads after the failures wrote %d %q lines, want one:\n%s", len(found), keySetReadableMessage, logged)
	}
	for _, want := range []string{"level=INFO", "url=" + issuer.URL(), "keys=" + issuer.URL() + "/keys"} {
		if !strings.Contains(found[0], want) {
			t.Fatalf("the line %q does not carry %q", found[0], want)
		}
	}

	issuer.FailKeys(http.StatusServiceUnavailable)
	read("a read that fails after the recovery")
	if got := len(lines(keySetUnreadableMessage)); got != 2 {
		t.Fatalf("a failed read after the recovery left %d %q lines, want a second:\n%s", got, keySetUnreadableMessage, logged)
	}
}

// TestTheGoogleVerifierDiscoveryBuildsReadsTheLoggedKeySet: the listed caller's Google verifier
// that discoverVerifiers builds reads Google's key set through the set it logs (logKeySet), so a
// Google token checked while that key set cannot be read is answered oidc.ErrIssuerUnavailable
// and writes the WARN line naming Google's issuer.
func TestTheGoogleVerifierDiscoveryBuildsReadsTheLoggedKeySet(t *testing.T) {
	logged := captureLog(t)
	google := oidctest.New(t)
	key := google.PublishKey(t, "google-key")
	cfg := vmConfig(t, localCluster(t).URL(), "example-vm-audience")
	cfg.GoogleOIDCIssuer = google.URL()
	found, err := discoverVerifiers(context.Background(), cfg, issuerKeys)
	if err != nil {
		t.Fatal(err)
	}
	google.FailKeys(http.StatusServiceUnavailable)
	claims := google.Claims(exampleVMAccount, "example-vm-audience")
	if _, err := found.GoogleIdentities.Verify(context.Background(), google.Mint(t, key, claims)); !errors.Is(err, oidc.ErrIssuerUnavailable) {
		t.Fatalf("Verify while Google's key set answers 503 = %v, want oidc.ErrIssuerUnavailable", err)
	}
	if found := logLines(logged, keySetUnreadableMessage); len(found) != 1 || !strings.Contains(found[0], "url="+google.URL()) {
		t.Fatalf("log = %q, want one %q line naming Google's issuer", logged, keySetUnreadableMessage)
	}
}

// TestTheExchangesGitHubIssuerReadsALoggedKeySet: the GitHub issuer the broker hands the token
// exchange (githubExchangeIssuer) names BROKER_EXCHANGE_GITHUB_ISSUER and its audience and reads
// that issuer's key set through the set it logs (logKeySet), so a workflow token's key read while
// the key set cannot be read writes the WARN line naming the GitHub issuer.
func TestTheExchangesGitHubIssuerReadsALoggedKeySet(t *testing.T) {
	logged := captureLog(t)
	github := oidctest.New(t)
	github.PublishKey(t, "github-key")
	cfg := config.Config{ExchangeGitHubIssuer: github.URL(), ExchangeGitHubAudience: "example-workflow-audience"}
	issuer, err := githubExchangeIssuer(context.Background(), cfg, issuerKeys)
	if err != nil {
		t.Fatal(err)
	}
	if issuer.Issuer != github.URL() || issuer.Audience != "example-workflow-audience" {
		t.Fatalf("the exchange's GitHub issuer = %q for %q, want %q for example-workflow-audience", issuer.Issuer, issuer.Audience, github.URL())
	}
	github.FailKeys(http.StatusServiceUnavailable)
	for range 3 {
		if _, err := issuer.Keys.Key(context.Background(), "github-key", "RS256"); !errors.Is(err, oidc.ErrKeySet) {
			t.Fatalf("Key while the GitHub key set answers 503 = %v, want oidc.ErrKeySet", err)
		}
	}
	if found := logLines(logged, keySetUnreadableMessage); len(found) != 1 || !strings.Contains(found[0], "url="+github.URL()) {
		t.Fatalf("log = %q, want one %q line naming the GitHub issuer", logged, keySetUnreadableMessage)
	}
}
