package main

//ci:reads packages/envoy/internal/dispatch/store/migrations/* TestHealthzReportsAnUnstampedCommitAsNull

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/config"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/fixtureenv"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// signInEnvironment is a cookie-identity boot environment with all four sign-in settings.
func signInEnvironment(overrides map[string]string) map[string]string {
	values := map[string]string{
		"DATABASE_URL":                  "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":          "agent-token",
		"DISPATCH_SIGNIN_ISSUER":        "https://issuer.example/pool",
		"DISPATCH_SIGNIN_CLIENT_ID":     "dispatch-client",
		"DISPATCH_SIGNIN_CLIENT_SECRET": "client-secret",
		"DISPATCH_SIGNIN_GROUP":         "dispatch-members",
	}
	for name, value := range overrides {
		values[name] = value
	}
	return values
}

func headerIdentityEnvironment(overrides map[string]string) map[string]string {
	values := map[string]string{
		"DATABASE_URL":                     "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":             "agent-token",
		"DISPATCH_IDENTITY":                "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
	}
	for name, value := range overrides {
		values[name] = value
	}
	return values
}

// testIAPAudience is the audience IAP signs for the test's load balancer backend service.
const testIAPAudience = "/projects/123456789012/global/backendServices/9876543210"

// iapIdentityEnvironment is an IAP-identity boot environment admitting the people of example.com:
// the identity, its audience and the domain, and no other sign-in setting.
func iapIdentityEnvironment(overrides map[string]string) map[string]string {
	values := map[string]string{
		"DATABASE_URL":           "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":   "agent-token",
		"DISPATCH_IDENTITY":      "iap",
		"DISPATCH_IAP_AUDIENCE":  testIAPAudience,
		"DISPATCH_SIGNIN_DOMAIN": "example.com",
	}
	for name, value := range overrides {
		values[name] = value
	}
	return values
}

// iapKeySet is a key set served on this machine in IAP's place, so no test calls Google, with the
// ES256 key it publishes; iapBoot is boot pointed at it, as no setting can point a server.
func iapKeySet(t *testing.T) (*oidctest.Issuer, *oidctest.Key) {
	t.Helper()
	keys := oidctest.New(t)
	signing := oidctest.NewES256Key(t, "iap-key")
	keys.Publish(signing)
	return keys, signing
}

func iapBoot(t *testing.T, keys *oidctest.Issuer, overrides map[string]string) bootConfig {
	t.Helper()
	boot, err := resolveBootConfig(envGetter(iapIdentityEnvironment(overrides)))
	if err != nil {
		t.Fatalf("resolve an IAP boot: %v", err)
	}
	boot.IAPKeySetURL = keys.URL() + "/keys"
	return boot
}

// iapAssertion is the assertion IAP signs with key for a Google account of hd, A.Person@<hd>,
// to audience.
func iapAssertion(t *testing.T, keys *oidctest.Issuer, key *oidctest.Key, audience, hd string) string {
	t.Helper()
	now := time.Now()
	return keys.Mint(t, key, map[string]any{
		"iss": oidc.IAPIssuer, "aud": audience, "sub": "accounts.google.com:118000000000000000001",
		"email": "A.Person@" + hd, "hd": hd, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(),
	})
}

// iapWhoami answers GET /auth/whoami from handler carrying assertion, when it is not empty, and
// headers.
func iapWhoami(handler http.Handler, assertion string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/auth/whoami", nil)
	if assertion != "" {
		request.Header.Set("X-Goog-IAP-JWT-Assertion", assertion)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestResolveBootConfigReadsIAPIdentity(t *testing.T) {
	t.Parallel()
	boot, err := resolveBootConfig(envGetter(iapIdentityEnvironment(map[string]string{"DISPATCH_IDENTITY": " iap "})))
	if err != nil {
		t.Fatalf("resolve boot config: %v", err)
	}
	if boot.IAPAudience != testIAPAudience || boot.IAPKeySetURL != oidc.IAPKeySetURL || boot.SignInDomain != "example.com" ||
		boot.IdentityHeader != "" || boot.SignInIssuer != "" {
		t.Fatalf("boot = %#v, want IAP identity for %s over IAP's own key set, admitting example.com", boot, testIAPAudience)
	}
}

// IAP signs people in, so an IAP server takes the domain it admits and no other sign-in setting,
// an audience of the shape IAP signs for, and no dev sign-in; the audience is refused under any
// other identity, which would leave it unread.
func TestResolveBootConfigFencesIAPIdentity(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{
		"DISPATCH_SIGNIN_ISSUER":        "https://accounts.google.com",
		"DISPATCH_SIGNIN_CLIENT_ID":     "dispatch-client",
		"DISPATCH_SIGNIN_CLIENT_SECRET": "client-secret",
		"DISPATCH_SIGNIN_GROUP":         "dispatch-members",
	} {
		if _, err := resolveBootConfig(envGetter(iapIdentityEnvironment(map[string]string{name: value}))); err == nil ||
			!strings.Contains(err.Error(), name+" must be unset under DISPATCH_IDENTITY=iap") {
			t.Errorf("IAP identity with %s: err = %v, want a refusal naming it", name, err)
		}
	}
	for _, tc := range []struct {
		overrides map[string]string
		naming    string
	}{
		{map[string]string{"DISPATCH_SIGNIN_DOMAIN": ""}, "DISPATCH_IDENTITY=iap needs DISPATCH_SIGNIN_DOMAIN"},
		{map[string]string{"DISPATCH_SIGNIN_DOMAIN": "Example.com"}, "DISPATCH_SIGNIN_DOMAIN must be a lowercase domain name"},
		{map[string]string{"DISPATCH_IAP_AUDIENCE": ""}, "DISPATCH_IAP_AUDIENCE required"},
		{map[string]string{"DISPATCH_IAP_AUDIENCE": "123456789012-client.apps.googleusercontent.com"}, "DISPATCH_IAP_AUDIENCE=\"123456789012-client.apps.googleusercontent.com\" must be the audience IAP signs"},
		{map[string]string{"DISPATCH_IAP_AUDIENCE": "/projects/example-project/global/backendServices/1"}, "begins /projects/<project number>/"},
	} {
		if _, err := resolveBootConfig(envGetter(iapIdentityEnvironment(tc.overrides))); err == nil || !strings.Contains(err.Error(), tc.naming) {
			t.Errorf("IAP identity with %v: err = %v, want a refusal naming %q", tc.overrides, err, tc.naming)
		}
	}
	for name, environment := range map[string]map[string]string{
		"cookie identity": signInEnvironment(map[string]string{"DISPATCH_IAP_AUDIENCE": testIAPAudience}),
		"header identity": headerIdentityEnvironment(map[string]string{"DISPATCH_IAP_AUDIENCE": testIAPAudience}),
	} {
		if _, err := resolveBootConfig(envGetter(environment)); err == nil || !strings.Contains(err.Error(), "DISPATCH_IAP_AUDIENCE is read only with DISPATCH_IDENTITY=iap") {
			t.Errorf("%s with DISPATCH_IAP_AUDIENCE: err = %v, want a refusal naming it", name, err)
		}
	}
	_, err := resolveBootConfig(devSignInEnvironment(map[string]string{
		"DISPATCH_IDENTITY": "iap", "DISPATCH_IAP_AUDIENCE": testIAPAudience, "DISPATCH_SIGNIN_DOMAIN": "example.com",
	}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_IDENTITY must be cookie") {
		t.Errorf("dev sign-in with IAP identity: err = %v, want a refusal naming DISPATCH_IDENTITY", err)
	}
}

// noSignInEnvironment is a cookie-identity boot environment with no sign-in setting: the sandbox
// deployment, which has no sign-in client and asks nobody to sign in. What is left is what every
// boot takes, the database and the agent token.
func noSignInEnvironment(overrides map[string]string) map[string]string {
	values := map[string]string{
		"DATABASE_URL":         "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN": "agent-token",
	}
	for name, value := range overrides {
		values[name] = value
	}
	return values
}

func TestResolveBootConfigReadsTheFourSignInSettings(t *testing.T) {
	t.Parallel()
	boot, err := resolveBootConfig(envGetter(signInEnvironment(nil)))
	if err != nil {
		t.Fatalf("resolve boot config: %v", err)
	}
	if boot.IdentityHeader != "" || boot.SignInIssuer != "https://issuer.example/pool" || boot.SignInClientID != "dispatch-client" ||
		boot.SignInClientSecret != "client-secret" || boot.SignInGroup != "dispatch-members" {
		t.Fatalf("boot = %#v, want cookie identity with the four sign-in settings", boot)
	}
}

// googleSignInEnvironment is a cookie-identity boot environment that signs people in with Google
// for the Workspace domain example.com: Google's issuer, the client, and the domain in place of a
// group.
func googleSignInEnvironment(overrides map[string]string) map[string]string {
	values := signInEnvironment(map[string]string{
		"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com",
		"DISPATCH_SIGNIN_GROUP":  "",
		"DISPATCH_SIGNIN_DOMAIN": "example.com",
	})
	for name, value := range overrides {
		values[name] = value
	}
	return values
}

func TestResolveBootConfigReadsTheDomainSignIn(t *testing.T) {
	t.Parallel()
	boot, err := resolveBootConfig(envGetter(googleSignInEnvironment(map[string]string{"DISPATCH_SIGNIN_DOMAIN": " example.com "})))
	if err != nil {
		t.Fatalf("resolve boot config: %v", err)
	}
	if boot.SignInIssuer != "https://accounts.google.com" || boot.SignInClientID != "dispatch-client" ||
		boot.SignInClientSecret != "client-secret" || boot.SignInDomain != "example.com" || boot.SignInGroup != "" {
		t.Fatalf("boot = %#v, want Google's issuer, the client and the domain example.com with no group", boot)
	}
}

// Sign-in set in part refuses the boot and names the whole shortfall in one run: the settings it
// was given, and the ones it still misses. The case below is the issuer, the client and the
// secret with neither a group nor a domain, which admits nobody.
func TestResolveBootConfigRefusesAPartialSignIn(t *testing.T) {
	t.Parallel()
	_, err := resolveBootConfig(envGetter(signInEnvironment(map[string]string{"DISPATCH_SIGNIN_GROUP": ""})))
	if want := "Google sign-in needs DISPATCH_SIGNIN_ISSUER, DISPATCH_SIGNIN_CLIENT_ID, DISPATCH_SIGNIN_CLIENT_SECRET and one of DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN: DISPATCH_SIGNIN_ISSUER, DISPATCH_SIGNIN_CLIENT_ID, DISPATCH_SIGNIN_CLIENT_SECRET set, DISPATCH_SIGNIN_GROUP and DISPATCH_SIGNIN_DOMAIN missing"; err == nil || err.Error() != want {
		t.Fatalf("sign-in with neither a group nor a domain: err = %v, want %q", err, want)
	}
}

// Each sign-in setting the server cannot use refuses the boot with its own text, and an input that
// trips two checks gets the first: both set, a partial set, a domain under another issuer, a
// malformed domain, a group under Google's issuer.
func TestResolveBootConfigRefusesSignInSettingsItCannotUse(t *testing.T) {
	t.Parallel()
	const lead = "Google sign-in needs DISPATCH_SIGNIN_ISSUER, DISPATCH_SIGNIN_CLIENT_ID, DISPATCH_SIGNIN_CLIENT_SECRET and one of DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN: "
	const both = "DISPATCH_SIGNIN_GROUP and DISPATCH_SIGNIN_DOMAIN are both set; set one: a Cognito pool's group, or a Google Workspace domain"
	noSignIn := map[string]string{"DATABASE_URL": "postgres://dispatch", "DISPATCH_AGENT_TOKEN": "agent-token"}
	with := func(base, overrides map[string]string) map[string]string {
		values := map[string]string{}
		for name, value := range base {
			values[name] = value
		}
		for name, value := range overrides {
			values[name] = value
		}
		return values
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"both a group and a domain", signInEnvironment(map[string]string{"DISPATCH_SIGNIN_DOMAIN": "example.com"}), both},
		{"both, with the secret missing too", signInEnvironment(map[string]string{"DISPATCH_SIGNIN_DOMAIN": "example.com", "DISPATCH_SIGNIN_CLIENT_SECRET": ""}), both},
		{"a domain alone", with(noSignIn, map[string]string{"DISPATCH_SIGNIN_DOMAIN": "example.com"}),
			lead + "DISPATCH_SIGNIN_DOMAIN set, DISPATCH_SIGNIN_ISSUER, DISPATCH_SIGNIN_CLIENT_ID, DISPATCH_SIGNIN_CLIENT_SECRET missing"},
		{"a domain without its secret", googleSignInEnvironment(map[string]string{"DISPATCH_SIGNIN_CLIENT_SECRET": ""}),
			lead + "DISPATCH_SIGNIN_ISSUER, DISPATCH_SIGNIN_CLIENT_ID, DISPATCH_SIGNIN_DOMAIN set, DISPATCH_SIGNIN_CLIENT_SECRET missing"},
		{"an issuer and a client with no secret, group or domain", with(noSignIn, map[string]string{"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com", "DISPATCH_SIGNIN_CLIENT_ID": "dispatch-client"}),
			lead + "DISPATCH_SIGNIN_ISSUER, DISPATCH_SIGNIN_CLIENT_ID set, DISPATCH_SIGNIN_CLIENT_SECRET, DISPATCH_SIGNIN_GROUP or DISPATCH_SIGNIN_DOMAIN missing"},
		{"a domain under a Cognito pool", googleSignInEnvironment(map[string]string{"DISPATCH_SIGNIN_ISSUER": "https://issuer.example/pool"}),
			"DISPATCH_SIGNIN_DOMAIN needs DISPATCH_SIGNIN_ISSUER https://accounts.google.com, since only Google's tokens carry hd; DISPATCH_SIGNIN_ISSUER is https://issuer.example/pool"},
		{"a domain under Google's schemeless issuer", googleSignInEnvironment(map[string]string{"DISPATCH_SIGNIN_ISSUER": "accounts.google.com"}),
			"DISPATCH_SIGNIN_DOMAIN needs DISPATCH_SIGNIN_ISSUER https://accounts.google.com, since only Google's tokens carry hd; DISPATCH_SIGNIN_ISSUER is accounts.google.com"},
		{"a malformed domain under another issuer", googleSignInEnvironment(map[string]string{"DISPATCH_SIGNIN_ISSUER": "https://issuer.example/pool", "DISPATCH_SIGNIN_DOMAIN": "Example.com"}),
			"DISPATCH_SIGNIN_DOMAIN needs DISPATCH_SIGNIN_ISSUER https://accounts.google.com, since only Google's tokens carry hd; DISPATCH_SIGNIN_ISSUER is https://issuer.example/pool"},
		{"a group under Google's issuer", signInEnvironment(map[string]string{"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com"}),
			"DISPATCH_SIGNIN_GROUP is a Cognito pool's group, and a token from https://accounts.google.com carries none; set DISPATCH_SIGNIN_DOMAIN instead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveBootConfig(envGetter(tc.env)); err == nil || err.Error() != tc.want {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	for _, domain := range []string{"Example.com", "example", "exa_mple.com", "https://example.com", "alice@example.com", "-example.com", "example-.com", "example..com", ".example.com", "example.com.", "ex ample.com", strings.Repeat("a", 64) + ".com", strings.Repeat(strings.Repeat("a", 63)+".", 4) + "com"} {
		_, err := resolveBootConfig(envGetter(googleSignInEnvironment(map[string]string{"DISPATCH_SIGNIN_DOMAIN": domain})))
		if want := "DISPATCH_SIGNIN_DOMAIN must be a lowercase domain name such as example.com; got a value of " + strconv.Itoa(len(domain)) + " characters"; err == nil || err.Error() != want {
			t.Errorf("DISPATCH_SIGNIN_DOMAIN=%q: err = %v, want %q", domain, err, want)
		}
	}
	for _, domain := range []string{"example.com", "d.example", "eng.example.co.uk", "x-1.example", strings.Repeat("a", 63) + ".com"} {
		if _, err := resolveBootConfig(envGetter(googleSignInEnvironment(map[string]string{"DISPATCH_SIGNIN_DOMAIN": domain}))); err != nil {
			t.Errorf("DISPATCH_SIGNIN_DOMAIN=%q: %v, want accepted", domain, err)
		}
	}
}

// A setting a release removed is refused, never silently ignored.
func TestResolveBootConfigRefusesRemovedSettings(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"DISPATCH_ALLOWED_LOGINS", "DISPATCH_APP_CLIENT_SECRET", "COHERE_API_KEY", "DISPATCH_AGENT_SECRETS_TOKEN", "DISPATCH_AGENT_SECRETS_TOKEN_FILE", "DISPATCH_ASSET_STORE_BUCKET", "DISPATCH_REPO_PROJECTS", "DISPATCH_DEFAULT_PROJECT"} {
		_, err := resolveBootConfig(envGetter(signInEnvironment(map[string]string{name: "value"})))
		if err == nil || !strings.Contains(err.Error(), name+" is no longer read") {
			t.Errorf("%s set: err = %v, want a refusal naming it", name, err)
		}
	}
	// Each refusal says what replaced the setting.
	for name, wants := range map[string][]string{
		// Meaning search reaches Cohere through either place, and the refusal names both.
		"COHERE_API_KEY": {"AWS Bedrock", "Azure AI Foundry", "DISPATCH_EMBEDDER"},
		// A project's default repository replaced the mapping the boot seeded.
		"DISPATCH_REPO_PROJECTS": {"PUT /api/v1/projects/{key}/default-repository", "Settings → Projects"},
		// An import names its own project: no project is found from a repository.
		"DISPATCH_DEFAULT_PROJECT": {"an import names its project in the create's `project`"},
	} {
		_, err := resolveBootConfig(envGetter(signInEnvironment(map[string]string{name: "value"})))
		for _, want := range wants {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s set: err = %v, want it to name %q", name, err, want)
			}
		}
	}
}

func TestResolveBootConfigFencesHeaderIdentityToTrustedLocalHarnesses(t *testing.T) {
	t.Parallel()
	withoutAcknowledgement := headerIdentityEnvironment(map[string]string{
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "",
	})
	if _, err := resolveBootConfig(envGetter(withoutAcknowledgement)); err == nil ||
		!strings.Contains(err.Error(), "DISPATCH_IDENTITY_HEADER_TRUSTED=1") {
		t.Fatalf("header identity without acknowledgement: err = %v, want a refusal naming DISPATCH_IDENTITY_HEADER_TRUSTED", err)
	}
	if _, err := resolveBootConfig(envGetter(headerIdentityEnvironment(nil))); err != nil {
		t.Fatalf("acknowledged header identity: %v", err)
	}
	for name, value := range map[string]string{
		"DISPATCH_SIGNIN_ISSUER":        "https://issuer.example/pool",
		"DISPATCH_SIGNIN_CLIENT_ID":     "dispatch-client",
		"DISPATCH_SIGNIN_CLIENT_SECRET": "client-secret",
		"DISPATCH_SIGNIN_GROUP":         "dispatch-members",
		"DISPATCH_SIGNIN_DOMAIN":        "example.com",
	} {
		if _, err := resolveBootConfig(envGetter(headerIdentityEnvironment(map[string]string{name: value}))); err == nil ||
			!strings.Contains(err.Error(), name) ||
			!strings.Contains(err.Error(), "header identity and Google sign-in cannot share a deployment") {
			t.Errorf("header identity with %s: err = %v, want a refusal naming the setting and why it cannot share a deployment", name, err)
		}
	}
}

func TestDevSignInRefusesTheSignInSettings(t *testing.T) {
	t.Parallel()
	_, err := resolveBootConfig(devSignInEnvironment(map[string]string{
		"DISPATCH_SIGNIN_ISSUER": "https://issuer.example/pool", "DISPATCH_SIGNIN_CLIENT_ID": "c",
		"DISPATCH_SIGNIN_CLIENT_SECRET": "s", "DISPATCH_SIGNIN_GROUP": "g",
	}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_SIGNIN_* must be unset") {
		t.Fatalf("dev sign-in with the sign-in settings: err = %v, want a refusal", err)
	}
	// A complete domain set passes every sign-in check, and the fence refuses it.
	_, err = resolveBootConfig(devSignInEnvironment(map[string]string{
		"DISPATCH_SIGNIN_ISSUER": "https://accounts.google.com", "DISPATCH_SIGNIN_CLIENT_ID": "c",
		"DISPATCH_SIGNIN_CLIENT_SECRET": "s", "DISPATCH_SIGNIN_DOMAIN": "example.com",
	}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_SIGNIN_* must be unset") {
		t.Fatalf("dev sign-in with the domain sign-in settings: err = %v, want the fence's refusal", err)
	}
	// A domain on its own is a partial set, refused before the fence.
	_, err = resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_SIGNIN_DOMAIN": "example.com"}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_SIGNIN_DOMAIN set") {
		t.Fatalf("dev sign-in with DISPATCH_SIGNIN_DOMAIN: err = %v, want a refusal naming it", err)
	}
}

func TestResolveBootConfigDisablesNATS(t *testing.T) {
	t.Parallel()
	boot, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":                     "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":             "agent-token",
		"DISPATCH_IDENTITY":                "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
		"DISPATCH_NATS_DISABLED":           "1",
	}))
	if err != nil {
		t.Fatalf("resolve boot config: %v", err)
	}
	if !boot.NATSDisabled {
		t.Fatal("DISPATCH_NATS_DISABLED=1 did not disable NATS")
	}
}

func TestResolveBootConfigDefaultsAndValidatesEnvoyURL(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"DATABASE_URL":                     "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":             "t",
		"DISPATCH_IDENTITY":                "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
	}

	boot, err := resolveBootConfig(envGetter(base))
	if err != nil || boot.EnvoyURL != "http://127.0.0.1:9020" {
		t.Fatalf("default: boot=%#v err=%v", boot, err)
	}

	withURL := map[string]string{}
	for key, value := range base {
		withURL[key] = value
	}
	withURL["ENVOY_URL"] = "http://envoy.internal:9020/"
	if boot, err := resolveBootConfig(envGetter(withURL)); err != nil || boot.EnvoyURL != "http://envoy.internal:9020" {
		t.Fatalf("explicit: boot=%#v err=%v", boot, err)
	}

	withURL["ENVOY_URL"] = "envoy.internal:9020"
	if _, err := resolveBootConfig(envGetter(withURL)); err == nil || !strings.Contains(err.Error(), "ENVOY_URL") {
		t.Fatalf("invalid: err = %v", err)
	}
}

// The credential-request UI feature is off by default (no broker URL configured), and
// resolveBootConfig must not require a token when the URL is unset.
func TestResolveBootConfigLeavesAgentSecretsOffByDefault(t *testing.T) {
	t.Parallel()
	boot, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":                     "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":             "agent-token",
		"DISPATCH_IDENTITY":                "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
	}))
	if err != nil || boot.AgentSecretsURL != "" || boot.AgentSecretsTokenFile != "" {
		t.Fatalf("boot=%#v err=%v, want the feature off", boot, err)
	}
}

func TestResolveBootConfigAcceptsAgentSecretsURLAndTokenFile(t *testing.T) {
	t.Parallel()
	boot, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":                              "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":                      "agent-token",
		"DISPATCH_IDENTITY":                         "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED":          "1",
		"DISPATCH_AGENT_SECRETS_URL":                "https://broker.internal/",
		"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": " /var/run/secrets/broker/token ",
	}))
	if err != nil {
		t.Fatalf("resolve boot config: %v", err)
	}
	if boot.AgentSecretsURL != "https://broker.internal" {
		t.Fatalf("AgentSecretsURL = %q, want the trailing slash trimmed", boot.AgentSecretsURL)
	}
	// The path is not read at boot: the client reads it on each call, since a projected token
	// rotates, and in the cluster the file is there before the first call either way.
	if boot.AgentSecretsTokenFile != "/var/run/secrets/broker/token" {
		t.Fatalf("AgentSecretsTokenFile = %q, want the trimmed path", boot.AgentSecretsTokenFile)
	}
}

// DISPATCH_AGENT_SECRETS_URL must be an absolute http(s) URL with no path, matching the
// ENVOY_URL check but additionally refusing a path component: the broker's client builds every
// call by appending a fixed path to this base.
func TestResolveBootConfigRejectsAgentSecretsURLWithPathOrBadScheme(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"DATABASE_URL":                              "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":                      "agent-token",
		"DISPATCH_IDENTITY":                         "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED":          "1",
		"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": "/var/run/secrets/broker/token",
	}
	for _, badURL := range []string{"broker.internal:9090", "https://broker.internal/v1"} {
		env := map[string]string{}
		for key, value := range base {
			env[key] = value
		}
		env["DISPATCH_AGENT_SECRETS_URL"] = badURL
		if _, err := resolveBootConfig(envGetter(env)); err == nil || !strings.Contains(err.Error(), "DISPATCH_AGENT_SECRETS_URL") {
			t.Fatalf("URL=%q: err = %v, want a DISPATCH_AGENT_SECRETS_URL rejection", badURL, err)
		}
	}
}

// Both DISPATCH_AGENT_SECRETS_URL refusals - a shape refusal (a path, or a value that does not
// parse) and the dev sign-in's refusal of a remote broker - name a clean URL as it is and any
// other value as redact.NotPrinted: user info, a query, escapes, a control byte.
func TestResolveBootConfigRefusesAgentSecretsURLWithoutNamingItsCredential(t *testing.T) {
	t.Parallel()
	shape := func(brokerURL string) settingValues {
		return envGetter(map[string]string{
			"DATABASE_URL":                              "postgres://dispatch",
			"DISPATCH_AGENT_TOKEN":                      "agent-token",
			"DISPATCH_IDENTITY":                         "header:X-Dispatch-User",
			"DISPATCH_IDENTITY_HEADER_TRUSTED":          "1",
			"DISPATCH_AGENT_SECRETS_URL":                brokerURL,
			"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": "/var/run/secrets/broker/token",
		})
	}
	remote := func(brokerURL string) settingValues {
		return devSignInEnvironment(map[string]string{
			"DISPATCH_AGENT_SECRETS_URL":                brokerURL,
			"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": "/var/run/secrets/broker/token",
		})
	}
	for name, row := range map[string]struct {
		env  settingValues
		want string
	}{
		"a clean path":                     {shape("https://broker.example.com/v1"), `DISPATCH_AGENT_SECRETS_URL="https://broker.example.com/v1"`},
		"a clean remote broker":            {remote("https://broker.example.com"), `DISPATCH_AGENT_SECRETS_URL="https://broker.example.com"`},
		"a path":                           {shape("https://tokuser9f3:hunter2pw@broker.example.com/v1?token=qtok77"), `DISPATCH_AGENT_SECRETS_URL="` + redact.NotPrinted + `"`},
		"a remote broker":                  {remote("https://tokuser9f3:hunter2pw@broker.example.com?token=qtok77"), `DISPATCH_AGENT_SECRETS_URL="` + redact.NotPrinted + `"`},
		"an '@' in a query value":          {shape("https://broker.example.com/?password=hunter@2pwSECRET"), `DISPATCH_AGENT_SECRETS_URL="` + redact.NotPrinted + `"`},
		"a remote broker, '@' in a query":  {remote("https://broker.example.com?a=1&token=qtok@77SECRET&b=2"), `DISPATCH_AGENT_SECRETS_URL="` + redact.NotPrinted + `"`},
		"a literal '@' and an escaped one": {shape("https://ada@corp.example:hunter2pw%40127.0.0.1:1"), `DISPATCH_AGENT_SECRETS_URL="` + redact.NotPrinted + `"`},
		"a double-escaped '@'":             {shape("https://tokuser9f3:hunter2pw%2540broker.example.com"), `DISPATCH_AGENT_SECRETS_URL="` + redact.NotPrinted + `"`},
		"a newline forging a line":         {shape("https://broker.example.com\nFORGED"), `DISPATCH_AGENT_SECRETS_URL="` + redact.NotPrinted + `"`},
	} {
		_, err := resolveBootConfig(row.env)
		if err == nil || !strings.Contains(err.Error(), row.want) {
			t.Errorf("%s: err = %v, want a refusal naming %s", name, err, row.want)
			continue
		}
		for _, secret := range []string{"tokuser9f3", "hunter2pw", "qtok", "2pwSECRET", "77SECRET", "ada@corp", "2540", "FORGED"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: refusal %q names %q", name, err, secret)
			}
		}
	}
}

// A broker URL with no token file must refuse to boot rather than construct a client that
// authenticates with an empty bearer.
func TestResolveBootConfigRequiresTheAccountTokenFileWhenAgentSecretsURLSet(t *testing.T) {
	t.Parallel()
	_, err := resolveBootConfig(envGetter(map[string]string{
		"DATABASE_URL":                     "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":             "agent-token",
		"DISPATCH_IDENTITY":                "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
		"DISPATCH_AGENT_SECRETS_URL":       "https://broker.internal",
	}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE") {
		t.Fatalf("err = %v, want a refusal naming DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", err)
	}
}

// TestDispatchReadsItsProofOrigins: DISPATCH_PROOF_ORIGINS adds origins a broker token's proof may
// name to DISPATCH_SERVER_URL's, and they reach the verifier main builds: brokerTokenVerifier, from
// resolveBootConfig and the server URL loadEnvoyConfig reads. A value of production's shape, which
// lists DISPATCH_SERVER_URL's origin again beside the in-cluster one, starts Dispatch with the two:
// a proof signed for either is accepted, and one for an origin neither setting names is refused.
// Unset, DISPATCH_SERVER_URL's origin is the only one. An entry with a path, and one with another
// scheme, each refuse the start naming it.
func TestDispatchReadsItsProofOrigins(t *testing.T) {
	const (
		public    = "https://dispatch.example"
		inCluster = "http://dispatch.internal.example:8766"
		elsewhere = "https://dispatch.elsewhere.example"
		route     = "/api/v1/whoami"
		token     = "a-session-token-the-broker-issued"
	)
	broker := ciphertokentest.NewStubBroker(t)
	caller := ciphertokentest.NewCaller(t)
	broker.Says(token, ciphertoken.Introspection{
		Active:       true,
		Session:      "ses_proof_origins",
		Person:       "alice",
		ProjectScope: ciphertoken.ProjectScopeNamed,
		Projects:     []string{"INFRA"},
		Scope:        "dispatch.write",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   caller.JKT(),
	})
	accountToken := writeFile(t, "account-token", "dispatchs-own-account-token")
	// start is what main does with values: the boot config, the server URL, the verifier.
	start := func(t *testing.T, values map[string]string) ([]string, *ciphertoken.Verifier) {
		t.Helper()
		values["DISPATCH_SERVER_URL"] = public
		values["DISPATCH_AGENT_SECRETS_URL"] = broker.Server.URL
		values["DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"] = accountToken
		env := bootEnvironment(values)
		boot, err := resolveBootConfig(env)
		if err != nil {
			t.Fatalf("resolve boot config: %v", err)
		}
		envoyConfig, err := loadEnvoyConfig(env, config.LoadOptions{CWD: t.TempDir(), HomeDir: envoyJSONHome(t)})
		if err != nil {
			t.Fatalf("load envoy config: %v", err)
		}
		serverURL := envoyConfig.Dispatch.ServerURL
		verifier, err := brokerTokenVerifier(boot, storetest.Open(t), serverURL)
		if err != nil {
			t.Fatalf("build the broker token verifier: %v", err)
		}
		return proofOrigins(serverURL, boot.ProofOrigins), verifier
	}
	verify := func(t *testing.T, verifier *ciphertoken.Verifier, origin string) error {
		t.Helper()
		_, err := verifier.Verify(t.Context(), caller.Signed(t, http.MethodGet, origin+route, token))
		return err
	}

	t.Run("production's shape", func(t *testing.T) {
		origins, verifier := start(t, map[string]string{"DISPATCH_PROOF_ORIGINS": " " + public + " , " + inCluster + " "})
		if !slices.Equal(origins, []string{public, inCluster}) {
			t.Fatalf("the verifier's origins are %q, want %q and %q, each once", origins, public, inCluster)
		}
		for _, origin := range []string{public, inCluster} {
			if err := verify(t, verifier, origin); err != nil {
				t.Errorf("a proof for %s was refused: %v", origin, err)
			}
		}
		if err := verify(t, verifier, elsewhere); !errors.Is(err, ciphertoken.ErrProofInvalid) {
			t.Errorf("a proof for %s, which no setting names, answered %v, want ErrProofInvalid", elsewhere, err)
		}
	})

	t.Run("unset", func(t *testing.T) {
		origins, verifier := start(t, map[string]string{})
		if !slices.Equal(origins, []string{public}) {
			t.Fatalf("the verifier's origins are %q, want DISPATCH_SERVER_URL's %q alone", origins, public)
		}
		if err := verify(t, verifier, public); err != nil {
			t.Errorf("a proof for DISPATCH_SERVER_URL's origin was refused: %v", err)
		}
		if err := verify(t, verifier, inCluster); !errors.Is(err, ciphertoken.ErrProofInvalid) {
			t.Errorf("unset, a proof for %s answered %v, want ErrProofInvalid", inCluster, err)
		}
	})

	t.Run("an entry that is no origin", func(t *testing.T) {
		for entry, naming := range map[string]string{
			inCluster + "/dispatch":           `DISPATCH_PROOF_ORIGINS entry 2 of 2 is "` + inCluster + `/dispatch"`,
			"ftp://dispatch.internal.example": `DISPATCH_PROOF_ORIGINS entry 2 of 2 is "` + redact.NotPrinted + `"`,
		} {
			refusedWith(t, map[string]string{"DISPATCH_PROOF_ORIGINS": public + "," + entry}, naming)
		}
	})
}

// The issuer and the audience are one setting in two variables: half of it is a
// misconfiguration a deployment must not boot with, and neither means the server
// verifies no service-account token at all.
func TestResolveBootConfigRequiresBothOIDCVariables(t *testing.T) {
	t.Parallel()
	base := map[string]string{
		"DATABASE_URL":                     "postgres://dispatch",
		"DISPATCH_AGENT_TOKEN":             "agent-token",
		"DISPATCH_IDENTITY":                "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
	}
	env := func(overrides map[string]string) settingValues {
		values := map[string]string{}
		for key, value := range base {
			values[key] = value
		}
		for key, value := range overrides {
			values[key] = value
		}
		return envGetter(values)
	}

	boot, err := resolveBootConfig(env(nil))
	if err != nil || boot.OIDCIssuer != "" || boot.OIDCAudience != "" {
		t.Fatalf("unset: boot=%#v err=%v", boot, err)
	}

	_, err = resolveBootConfig(env(map[string]string{"DISPATCH_OIDC_ISSUER": "https://oidc.example"}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_OIDC_AUDIENCE") {
		t.Fatalf("issuer alone: err = %v, want one naming DISPATCH_OIDC_AUDIENCE", err)
	}

	_, err = resolveBootConfig(env(map[string]string{"DISPATCH_OIDC_AUDIENCE": "dispatch"}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_OIDC_ISSUER") {
		t.Fatalf("audience alone: err = %v, want one naming DISPATCH_OIDC_ISSUER", err)
	}

	boot, err = resolveBootConfig(env(map[string]string{
		"DISPATCH_OIDC_ISSUER":   "https://oidc.example",
		"DISPATCH_OIDC_AUDIENCE": "dispatch",
	}))
	if err != nil || boot.OIDCIssuer != "https://oidc.example" || boot.OIDCAudience != "dispatch" {
		t.Fatalf("both: boot=%#v err=%v", boot, err)
	}
}

func TestDispatchHandlerReportsDisabledNATS(t *testing.T) {
	t.Parallel()
	handler := dispatchHandler(http.NewServeMux(), metrics.New(), nil, nil, nil, nil, "", nil, issuers{}, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if nats, found := health["nats"]; !found || nats != nil {
		t.Fatalf("healthz nats = %#v, want null", nats)
	}
}

// A process whose NATS is not connected serves every request that does not need the bus, so the
// probe answers 200 with `ok` true and reports the bus in `nats` alone: a readiness probe that
// failed with the bus would take every server process out of service at once.
func TestDispatchHandlerReportsDisconnectedNATS(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	handler := dispatchHandler(http.NewServeMux(), metrics.New(), database, &bus.Client{}, nil, nil, "", nil, issuers{}, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if response.Code != http.StatusOK || health["ok"] != true {
		t.Fatalf("healthz with NATS disconnected = %d %v, want 200 with ok true", response.Code, health)
	}
	if nats, ok := health["nats"].(bool); !ok || nats {
		t.Fatalf("healthz nats = %#v, want false", health["nats"])
	}
}

// untouchedFiles is a file store any call on which panics: its methods are the nil interface's.
type untouchedFiles struct{ files.Store }

// /livez answers 204 with no body and asks no dependency anything: with no database, no NATS
// client and a file store any call on which panics, it still answers, and the router behind it is
// never reached. It is what the liveness and startup probes ask, which must not wait on or fail
// with a dependency.
func TestDispatchHandlerLivezAnswersWithoutDependencies(t *testing.T) {
	t.Parallel()
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the router served %s %s", r.Method, r.URL.Path)
	})
	response := httptest.NewRecorder()
	dispatchHandler(router, metrics.New(), nil, nil, nil, untouchedFiles{}, "", nil, issuers{}, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("GET /livez status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("GET /livez body = %q, want none", response.Body.String())
	}
}

// A scrape of /metrics reads the process's registry, not the router: the router answers every
// path it does not know with the dashboard's HTML shell, so a scraper would otherwise collect a
// page instead of the outbox's publish failures.
func TestDispatchHandlerServesMetrics(t *testing.T) {
	t.Parallel()
	router := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>dispatch</title>")
	})
	registry := metrics.New()
	failures := registry.NewCounter("envoy_dispatch_outbox_publish_failures_total", "Destinations of an outbox publish attempt that did not reach NATS")
	failures.Inc([2]string{"reason", "denied"})

	response := httptest.NewRecorder()
	dispatchHandler(router, registry, nil, nil, nil, untouchedFiles{}, "", nil, issuers{}, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", response.Code, http.StatusOK)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("GET /metrics content type = %q, want the Prometheus exposition format's", contentType)
	}
	if body := response.Body.String(); !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="denied"} 1`) {
		t.Fatalf("GET /metrics body = %q, want the outbox's publish failures in it", body)
	}
}

// /healthz reports the file store beside the database and NATS: null where no bucket is
// configured, true where the bucket answers, and false where it does not, so a wrong grant shows
// on the health page and not only as failed uploads. The bucket never decides `ok`: the load
// balancer would replace the one task, taking documents and asks down with the files.
func TestDispatchHandlerReportsTheFileStore(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	probe := func(store files.Store) (int, map[string]any) {
		t.Helper()
		response := httptest.NewRecorder()
		dispatchHandler(http.NewServeMux(), metrics.New(), database, nil, nil, store, "", nil, issuers{}, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var health map[string]any
		if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
			t.Fatalf("decode health response: %v", err)
		}
		return response.Code, health
	}
	if status, health := probe(nil); health["files"] != nil || status != http.StatusOK {
		t.Fatalf("healthz with no store: status %d, files %#v, want 200 and null", status, health["files"])
	}
	reachable := filestest.NewMemory()
	if status, health := probe(reachable); health["files"] != true || status != http.StatusOK {
		t.Fatalf("healthz with a reachable store: status %d, files %#v, want 200 and true", status, health["files"])
	}
	unreachable := filestest.NewMemory()
	unreachable.SetFailure(errors.New("bucket unreachable"))
	status, health := probe(unreachable)
	if health["files"] != false || health["ok"] != true || status != http.StatusOK {
		t.Fatalf("healthz with an unreachable store: status %d, ok %#v, files %#v, want 200 ok with files false", status, health["ok"], health["files"])
	}
}

// /healthz reports each OIDC issuer the process discovers in the background: null where none is
// configured, false until its discovery document has been read, true after. Neither decides `ok`:
// a process whose issuer is down still serves agents and the people whose confirmation stands.
func TestDispatchHandlerReportsEachIssuersDiscovery(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	probe := func(configured issuers) (int, map[string]any) {
		t.Helper()
		response := httptest.NewRecorder()
		dispatchHandler(http.NewServeMux(), metrics.New(), database, nil, nil, nil, "", nil, configured, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var health map[string]any
		if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
			t.Fatalf("decode health response: %v", err)
		}
		return response.Code, health
	}
	if status, health := probe(issuers{}); status != http.StatusOK || health["signin"] != nil || health["service_tokens"] != nil {
		t.Fatalf("healthz with no issuer configured: %d %v, want 200 with signin and service_tokens null", status, health)
	}
	signInIssuer, serviceTokenIssuer := oidctest.New(t), oidctest.New(t)
	signInIssuer.EnableCodeFlow(signInIssuer.PublishKey(t, "signing-key"), "dispatch-client", "client-secret")
	serviceTokenIssuer.PublishKey(t, "signing-key")
	configured := issuers{
		signIn:        oidc.NewCodeFlow(signInIssuer.URL(), "dispatch-client", "client-secret", ""),
		serviceTokens: oidc.NewLazyVerifier(serviceTokenIssuer.URL(), "dispatch"),
	}
	if status, health := probe(configured); status != http.StatusOK || health["ok"] != true || health["signin"] != false || health["service_tokens"] != false {
		t.Fatalf("healthz before either issuer is discovered: %d %v, want 200 ok with signin and service_tokens false", status, health)
	}
	if err := configured.signIn.Discover(context.Background()); err != nil {
		t.Fatalf("discover the sign-in issuer: %v", err)
	}
	if err := configured.serviceTokens.Discover(context.Background()); err != nil {
		t.Fatalf("discover the service-account token issuer: %v", err)
	}
	if status, health := probe(configured); status != http.StatusOK || health["signin"] != true || health["service_tokens"] != true {
		t.Fatalf("healthz once both issuers are discovered: %d %v, want 200 with signin and service_tokens true", status, health)
	}
}

// /healthz names what is deployed, for a deploy check to compare with what was meant to be:
// the commit the image build stamped, and the highest migration the database has applied. The
// schema version is the database's, read per probe, never the binary's own list: a row a later
// image applied shows up at once, which is the only reading under which comparing it with a
// commit's migrations tests anything.
func TestHealthzReportsTheBuildCommitAndTheAppliedSchemaVersion(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	const commit = "0123456789abcdef0123456789abcdef01234567"
	handler := dispatchHandler(http.NewServeMux(), metrics.New(), database, nil, nil, nil, commit, nil, issuers{}, nil)
	probe := func() map[string]any {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var health map[string]any
		if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
			t.Fatalf("decode health response: %v", err)
		}
		if response.Code != http.StatusOK || health["ok"] != true || health["db"] != true {
			t.Fatalf("healthz = %d %#v, want 200 with ok and db true", response.Code, health)
		}
		return health
	}

	highest := highestMigrationFile(t)
	health := probe()
	if health["commit"] != commit {
		t.Fatalf("healthz commit = %#v, want %q", health["commit"], commit)
	}
	if health["schema_version"] != float64(highest) {
		t.Fatalf("healthz schema_version = %#v, want %d, the highest migration file", health["schema_version"], highest)
	}

	if _, err := database.Pool.Exec(context.Background(), "insert into schema_migrations (version) values ($1)", highest+1); err != nil {
		t.Fatalf("record a later migration: %v", err)
	}
	if got := probe()["schema_version"]; got != float64(highest+1) {
		t.Fatalf("healthz schema_version after the database recorded %d = %#v, want the database's", highest+1, got)
	}
}

// A binary the image build did not stamp says so: null, never an empty string or a guess. So does
// a process with no announcement of the fleet's build to report. Every field is always present,
// the ones nothing filled as null, outbox_publisher false - a process with NATS disabled publishes
// nothing - and idempotency_keys true, the binary's own fact.
func TestHealthzReportsAnUnstampedCommitAsNull(t *testing.T) {
	t.Parallel()
	response := httptest.NewRecorder()
	dispatchHandler(http.NewServeMux(), metrics.New(), nil, nil, nil, nil, "", nil, issuers{}, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	want := map[string]any{
		"ok": false, "db": false, "nats": nil, "files": nil, "signin": nil, "service_tokens": nil, "github": nil,
		"commit": nil, "fleet_commit": nil, "schema_version": nil, "outbox_publisher": false,
		"idempotency_keys": true,
	}
	for field, value := range want {
		if got, found := health[field]; !found || got != value {
			t.Fatalf("healthz %s = %#v (present %t), want %#v", field, got, found, value)
		}
	}
	if len(health) != len(want) {
		t.Fatalf("healthz = %#v, want exactly the fields %v", health, want)
	}
}

// requireOnlyDegraded fails unless body, a /healthz answer, carries degraded as want, compared as
// the literal a probe reads, and is otherwise without, the body of a handler with no read, field
// for field: no key and no count beside the fixed reasons, so nothing in it changes with how many
// projects are private, or which (R2).
func requireOnlyDegraded(t *testing.T, body, without, want string) {
	t.Helper()
	var fields, withoutFields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatalf("decode /healthz %s: %v", body, err)
	}
	if err := json.Unmarshal([]byte(without), &withoutFields); err != nil {
		t.Fatalf("decode /healthz %s: %v", without, err)
	}
	if got := string(fields["degraded"]); got != want {
		t.Fatalf("/healthz = %s: degraded %s, want %s", body, got, want)
	}
	delete(fields, "degraded")
	if !maps.EqualFunc(fields, withoutFields, func(a, b json.RawMessage) bool { return string(a) == string(b) }) {
		t.Errorf("/healthz = %s, want the body without the read plus degraded alone, %s", body, without)
	}
}

// /healthz names each GitHub read from the value main hands it alone (gitHubReads): off for a read
// this process does not run, whatever the record holds of it; unknown before the copy of the
// record has loaded; no_attempt for a read the record holds no attempt of; and succeeded or failed
// from the attempt it holds, with that attempt's time in UTC, and the cause of a failed one. `at`
// and `cause` are present and null wherever the state carries neither, and the four keys come in
// githubreads.Reads' order. A failed read leaves the status 200 and `ok` true, and a database that
// does not answer leaves the entries as the copy has them beside its 503.
func TestHealthzNamesEachStateOfAGitHubReadFromItsCopy(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	succeededAt := time.Date(2026, 11, 18, 3, 4, 5, 678901000, time.FixedZone("UTC+2", 2*60*60))
	failedAt := succeededAt.Add(90 * time.Second)
	cause := "GET https://api.github.com/repos/acme/deploy/installation: status 503: unavailable"
	recorded := loadedGitHubReadCopy(t, []githubreads.Recorded{
		{Read: githubreads.ArchitectureImport, At: succeededAt, Succeeded: true},
		{Read: githubreads.DeliveryReconcile, At: failedAt, Cause: cause},
		{Read: githubreads.CIReconcile, At: failedAt, Cause: "a read this process does not run"},
	})
	reads := &gitHubReads{recordCopy: recorded, runs: map[githubreads.Read]bool{
		githubreads.WebhookRedelivery:  true,
		githubreads.ArchitectureImport: true,
		githubreads.DeliveryReconcile:  true,
		githubreads.CIReconcile:        false,
	}}
	// entry is an entry as written: at in RFC 3339 UTC, and at and cause null where nil.
	entry := func(state string, at *time.Time, failure *string) map[string]any {
		answer := map[string]any{"state": state, "at": nil, "cause": nil}
		if at != nil {
			answer["at"] = at.UTC().Format(time.RFC3339Nano)
		}
		if failure != nil {
			answer["cause"] = *failure
		}
		return answer
	}
	fromTheCopy := map[string]map[string]any{
		"webhook_redelivery":  entry("no_attempt", nil, nil),
		"architecture_import": entry("succeeded", &succeededAt, nil),
		"delivery_reconcile":  entry("failed", &failedAt, &cause),
		"ci_reconcile":        entry("off", nil, nil),
	}

	status, health := probeGitHubReads(t, database, reads)
	if status != http.StatusOK || !health.OK || !health.DB {
		t.Fatalf("healthz with a failed GitHub read = %d, ok %t, db %t; want 200 with ok and db true", status, health.OK, health.DB)
	}
	requireGitHubReads(t, "with the copy loaded", health.GitHub, fromTheCopy)

	status, health = probeGitHubReads(t, nil, reads)
	if status != http.StatusServiceUnavailable || health.OK || health.DB {
		t.Fatalf("healthz with no database = %d, ok %t, db %t; want 503 with ok and db false", status, health.OK, health.DB)
	}
	requireGitHubReads(t, "with no database", health.GitHub, fromTheCopy)

	unloaded := &gitHubReads{recordCopy: &githubreads.Copy{}, runs: reads.runs}
	_, health = probeGitHubReads(t, database, unloaded)
	requireGitHubReads(t, "before the copy has loaded", health.GitHub, map[string]map[string]any{
		"webhook_redelivery":  entry("unknown", nil, nil),
		"architecture_import": entry("unknown", nil, nil),
		"delivery_reconcile":  entry("unknown", nil, nil),
		"ci_reconcile":        entry("off", nil, nil),
	})
}

// loadedGitHubReadCopy is a copy of the record of GitHub reads whose first load read rows, as
// githubreads.Copy.Run leaves it at start. It runs until the test ends.
func loadedGitHubReadCopy(t *testing.T, rows []githubreads.Recorded) *githubreads.Copy {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	recorded := &githubreads.Copy{}
	go recorded.Run(ctx, func(context.Context) ([]githubreads.Recorded, error) { return rows, nil }, nil, nil)
	testwait.Eventually(t, "the copy's first load", func() bool {
		_, loaded := recorded.Rows()
		return loaded
	})
	return recorded
}

// gitHubReadsHealth is the part of /healthz the GitHub read tests read: `github` as written.
type gitHubReadsHealth struct {
	OK     bool            `json:"ok"`
	DB     bool            `json:"db"`
	GitHub json.RawMessage `json:"github"`
}

// probeGitHubReads answers one /healthz from database and reads, returning its status and body.
func probeGitHubReads(t *testing.T, database *store.Store, reads *gitHubReads) (int, gitHubReadsHealth) {
	t.Helper()
	response := httptest.NewRecorder()
	dispatchHandler(http.NewServeMux(), metrics.New(), database, nil, nil, nil, "", nil, issuers{}, reads).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var health gitHubReadsHealth
	if err := json.Unmarshal(response.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health response %s: %v", response.Body.String(), err)
	}
	return response.Code, health
}

// requireGitHubReads requires github to hold the four reads in githubreads.Reads' order, each
// entry exactly the want of its key: its state, and its at and cause, present when null.
func requireGitHubReads(t *testing.T, when string, github json.RawMessage, want map[string]map[string]any) {
	t.Helper()
	var order []string
	for _, read := range githubreads.Reads() {
		order = append(order, string(read))
	}
	if keys := jsonObjectKeys(t, github); !slices.Equal(keys, order) {
		t.Fatalf("healthz %s: github keys %q, want %q", when, keys, order)
	}
	var entries map[string]map[string]any
	if err := json.Unmarshal(github, &entries); err != nil {
		t.Fatalf("healthz %s: decode github %s: %v", when, github, err)
	}
	for read, wantEntry := range want {
		if !reflect.DeepEqual(entries[read], wantEntry) {
			t.Errorf("healthz %s: github %s = %#v, want %#v", when, read, entries[read], wantEntry)
		}
	}
}

// jsonObjectKeys is the keys of the JSON object raw, in the order it writes them.
func jsonObjectKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("%s is not a JSON object (%v, %v)", raw, token, err)
	}
	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			t.Fatalf("read a key of %s: %v", raw, err)
		}
		keys = append(keys, token.(string))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("read the value of %v in %s: %v", token, raw, err)
		}
	}
	return keys
}

// /healthz says whether this process publishes the events outbox: true while its outbox holds the
// publisher hold, false while it stands by for the other process's, and false again once it has
// stopped and released the hold. It never decides ok.
func TestHealthzReportsWhetherThisProcessPublishesTheOutbox(t *testing.T) {
	t.Parallel()
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	type process struct {
		outbox  *outbox.Outbox
		handler http.Handler
		stop    func()
	}
	start := func(database *store.Store) *process {
		t.Helper()
		client, err := bus.ConnectOwningStream([]string{url})
		if err != nil {
			t.Fatalf("connect NATS: %v", err)
		}
		t.Cleanup(client.Close)
		pool, err := database.Pool.Outbox()
		if err != nil {
			t.Fatalf("open the outbox's pool: %v", err)
		}
		publisher := outbox.New(outbox.Deps{
			Store: &store.Store{Pool: pool}, Publisher: client, Broker: events.NewBroker(), Listen: database.Pool.Listener,
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { publisher.Run(ctx); close(done) }()
		stop := func() { cancel(); <-done }
		t.Cleanup(stop)
		return &process{outbox: publisher, handler: dispatchHandler(http.NewServeMux(), metrics.New(), database, client, publisher, nil, "", nil, issuers{}, nil), stop: stop}
	}
	probe := func(p *process) bool {
		t.Helper()
		response := httptest.NewRecorder()
		p.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var health map[string]any
		if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
			t.Fatalf("decode health response: %v", err)
		}
		if response.Code != http.StatusOK || health["ok"] != true {
			t.Fatalf("healthz = %d %#v, want 200 and ok whether or not this process publishes", response.Code, health)
		}
		publishing, isBool := health["outbox_publisher"].(bool)
		if !isBool {
			t.Fatalf("healthz outbox_publisher = %#v, want a boolean", health["outbox_publisher"])
		}
		return publishing
	}

	first := start(database)
	deadline := time.Now().Add(10 * time.Second)
	for !probe(first) {
		if time.Now().After(deadline) {
			t.Fatal("healthz never reported the only outbox publishing")
		}
		time.Sleep(20 * time.Millisecond)
	}
	second := start(storetest.SecondProcess(t, database))
	time.Sleep(time.Second)
	if probe(second) || !probe(first) {
		t.Fatalf("outbox_publisher: first %t, second %t, want only the first, which holds the publisher hold", probe(first), probe(second))
	}

	first.stop()
	if probe(first) {
		t.Fatal("healthz reported a stopped outbox publishing")
	}
	deadline = time.Now().Add(10 * time.Second)
	for !probe(second) {
		if time.Now().After(deadline) {
			t.Fatal("healthz never reported the standby publishing once the holder stopped")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// /healthz says this build judges a write's Idempotency-Key (INFRA-258), which the deploy check
// reads (scripts/e2e/lib/dispatch-keyed-comment.ts): an earlier Dispatch ignores the header and
// would run a retried write again. It is the binary's fact, so a probe that answers 503 - its
// database down, closed or never configured - carries it in its body as a 200 does, and the check
// reads the body whatever the status.
func TestHealthzReportsThatWritesTakeIdempotencyKeys(t *testing.T) {
	t.Parallel()
	closed := storetest.Open(t)
	closed.Pool.Close()
	for name, probe := range map[string]struct {
		database *store.Store
		status   int
	}{
		"a database":        {storetest.Open(t), http.StatusOK},
		"a closed database": {closed, http.StatusServiceUnavailable},
		"no database":       {nil, http.StatusServiceUnavailable},
	} {
		response := httptest.NewRecorder()
		dispatchHandler(http.NewServeMux(), metrics.New(), probe.database, nil, nil, nil, "", nil, issuers{}, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if response.Code != probe.status {
			t.Errorf("healthz with %s: status %d, want %d", name, response.Code, probe.status)
		}
		var health map[string]any
		if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
			t.Fatalf("decode health response: %v", err)
		}
		if health["idempotency_keys"] != true {
			t.Errorf("healthz with %s: idempotency_keys = %#v, want true", name, health["idempotency_keys"])
		}
	}
}

// highestMigrationFile is the number of the last up migration in the source tree, read the way
// store.Migrate names versions: the digits before the first underscore.
func highestMigrationFile(t *testing.T) int {
	t.Helper()
	names, err := filepath.Glob("../../internal/dispatch/store/migrations/*.up.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(names))
	}
	highest := 0
	for _, name := range names {
		prefix, _, _ := strings.Cut(filepath.Base(name), "_")
		version, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatalf("migration %s: %v", name, err)
		}
		highest = max(highest, version)
	}
	return highest
}

// /healthz answers while every connection of the shared pool is held. A busy period
// legitimately empties that pool - writers queued on one issue's row lock hold theirs until
// they commit - and a probe that queues behind them is read as a dead process: the ALB fails
// it at five seconds, ECS replaces the task, and every in-flight request of every other client
// is cancelled. The probe therefore takes its connection from a pool of its own, and the short
// client timeout here is what tells a queued probe from an answered one. The shared pool here
// holds four connections: the property holds at any size, and holding all of production's
// sixty-four while the other packages' tests share the server would push a default Postgres
// past its hundred.
func TestHealthzAnswersWhileEveryPooledConnectionIsHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	config := storetest.Open(t).Pool.Config().Copy()
	config.MaxConns = 4
	shared, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a four-connection shared pool: %v", err)
	}
	database := &store.Store{Pool: store.NewPool(shared)}
	t.Cleanup(database.Pool.Close)
	for held := int32(0); held < database.Pool.Config().MaxConns; held++ {
		connection, err := database.Pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold pooled connection %d: %v", held, err)
		}
		defer connection.Release()
	}

	server := httptest.NewServer(dispatchHandler(http.NewServeMux(), metrics.New(), database, nil, nil, nil, "", nil, issuers{}, nil))
	defer server.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	started := time.Now()
	response, err := client.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("health probe with every pooled connection held: %v (after %s)", err, time.Since(started))
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if response.StatusCode != http.StatusOK || health["ok"] != true || health["db"] != true {
		t.Fatalf("health probe = %d %#v, want 200 with ok and db true", response.StatusCode, health)
	}
}

// /healthz answers inside the load balancer's timeout when Postgres stops answering at all:
// packets dropped, nothing refused and nothing reset, which is what a security-group change, an
// availability-zone partition, or an endpoint that accepts and then stalls looks like to the
// client. A stopped container cannot produce it - its port refuses instantly - so the probe's
// own wait only shows here. Without store.healthProbeTimeout that wait is two minutes on a
// cold dial, which pgxpool floors it at, and unbounded once the connection is up, because an
// http.Server request context carries no deadline: either way the probe does not answer, and
// three polls that never answer replace the task and cancel every in-flight request of every
// other client - the outcome the health pool exists to prevent. 503 is the right answer here;
// silence is not.
//
// The health pool opens on the first probe, so black-holing the link before that probe leaves
// nothing to race: the connection this test hangs on is dialled after the outage begins. That
// makes this the cold case; the warm one, which production actually runs, is the unbounded
// one and is measured against the real binary rather than here.
func TestHealthzAnswersWhilePostgresStopsAnswering(t *testing.T) {
	// Not parallel: it holds a step to a wall-clock bound load could break (the client's 5 s timeout
	// around a 2 s unanswered probe).
	migrated := storetest.Open(t)
	dsn, err := url.Parse(migrated.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	var blackholed atomic.Bool
	dsn.Host = blackholePostgres(t, dsn.Host, &blackholed)

	database, err := store.Open(context.Background(), dsn.String())
	if err != nil {
		t.Fatalf("open the store through the proxy: %v", err)
	}
	t.Cleanup(func() {
		blackholed.Store(false)
		database.Pool.Close()
	})

	server := httptest.NewServer(dispatchHandler(http.NewServeMux(), metrics.New(), database, nil, nil, nil, "", nil, issuers{}, nil))
	defer server.Close()
	// The shared pool's own round trip is the control: the link works right up to the outage.
	if _, err := database.Pool.Exec(context.Background(), "select 1"); err != nil {
		t.Fatalf("query through the proxy before the outage: %v", err)
	}

	// The load balancer's own timeout: a poll it has not heard back from in five seconds is a
	// failure, and three consecutive failures replace the task.
	client := &http.Client{Timeout: 5 * time.Second}

	blackholed.Store(true)
	started := time.Now()
	response, err := client.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("health probe with Postgres unreachable: %v (after %s)", err, time.Since(started))
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable || health["db"] != false || health["schema_version"] != nil {
		t.Fatalf("health probe with Postgres unreachable = %d %#v, want 503 with db false and no schema version",
			response.StatusCode, health)
	}
}

// unansweringBucket is a file store whose bucket never answers its probe: Healthy waits out the
// two seconds files.S3 bounds its HeadBucket by, as a hung HeadBucket does.
type unansweringBucket struct{ *filestest.Memory }

func (unansweringBucket) Healthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	<-ctx.Done()
	return ctx.Err()
}

// /healthz still answers inside the tightest prober's three seconds when Postgres and the
// file store's bucket both stop answering. Each probe is bounded at two seconds on its own, so
// run one after the other they took four, and the compose healthcheck and the deploy script
// read that silence as a dead process; run side by side they take two.
func TestHealthzAnswersWhilePostgresAndTheBucketBothStopAnswering(t *testing.T) {
	// Not parallel: it holds a step to a wall-clock bound load could break (the client's 3 s timeout
	// around a 2 s probe and a 2 s bucket).
	migrated := storetest.Open(t)
	dsn, err := url.Parse(migrated.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	var blackholed atomic.Bool
	dsn.Host = blackholePostgres(t, dsn.Host, &blackholed)
	database, err := store.Open(context.Background(), dsn.String())
	if err != nil {
		t.Fatalf("open the store through the proxy: %v", err)
	}
	t.Cleanup(func() {
		blackholed.Store(false)
		database.Pool.Close()
	})
	bucket := unansweringBucket{filestest.NewMemory()}
	server := httptest.NewServer(dispatchHandler(http.NewServeMux(), metrics.New(), database, nil, nil, bucket, "", nil, issuers{}, nil))
	defer server.Close()

	client := &http.Client{Timeout: 3 * time.Second}
	blackholed.Store(true)
	started := time.Now()
	response, err := client.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("health probe with Postgres and the bucket unreachable: %v (after %s)", err, time.Since(started))
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable || health["db"] != false || health["files"] != false {
		t.Fatalf("health probe with Postgres and the bucket unreachable = %d %#v, want 503 with db and files false",
			response.StatusCode, health)
	}
}

// blackholePostgres puts a proxy in front of upstream and returns its address. While blackholed
// is set, bytes stop moving in both directions and nothing is refused, reset or closed, so a
// client waiting on a reply waits for as long as its own deadline allows.
func blackholePostgres(t *testing.T, upstream string, blackholed *atomic.Bool) string {
	t.Helper()
	return proxyPostgres(t, upstream, blackholed, 0)
}

// slowPostgres puts a proxy in front of upstream and returns its address. Every connection
// through it reaches upstream dialDelay late and is then forwarded whole, and nothing is ever
// black-holed: the only thing the proxy adds is that wait. It is how a test makes a process pay
// for a cold dial what a loaded database charges for one, which a caller already holding a
// connection pays nothing of.
func slowPostgres(t *testing.T, upstream string, dialDelay time.Duration) string {
	t.Helper()
	return proxyPostgres(t, upstream, &atomic.Bool{}, dialDelay)
}

// proxyPostgres is the listener both proxies above are: it accepts for as long as the test runs
// and hands each connection to proxyPostgresConnection on their terms.
func proxyPostgres(t *testing.T, upstream string, blackholed *atomic.Bool, dialDelay time.Duration) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the Postgres proxy: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go proxyPostgresConnection(client, upstream, blackholed, dialDelay)
		}
	}()
	return listener.Addr().String()
}

// proxyPostgresConnection completes the client's handshake even while the link is black-holed,
// and waits dialDelay out, before it dials upstream, which is what a dropped SYN-ACK path and a
// slow database both leave a client holding: a connected socket nobody is answering on. The
// kernel completes that handshake on the listening socket, so neither wait is a connection the
// client sees refused; each is the answer taking its time.
func proxyPostgresConnection(client net.Conn, upstream string, blackholed *atomic.Bool, dialDelay time.Duration) {
	defer client.Close()
	for blackholed.Load() {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(dialDelay)
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		return
	}
	defer server.Close()
	done := make(chan struct{}, 2)
	go pumpUntilBlackholed(server, client, blackholed, done)
	go pumpUntilBlackholed(client, server, blackholed, done)
	<-done
}

// pumpUntilBlackholed copies src to dst, freezing while the link is black-holed rather than
// tearing the connection down: a dropped packet does not close a socket.
func pumpUntilBlackholed(dst, src net.Conn, blackholed *atomic.Bool, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	buffer := make([]byte, 32*1024)
	for {
		if blackholed.Load() {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if err := src.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
			return
		}
		read, err := src.Read(buffer)
		if read > 0 {
			if _, err := dst.Write(buffer[:read]); err != nil {
				return
			}
		}
		if err != nil {
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				continue
			}
			return
		}
	}
}

// envGetter is what the settings table reads from an environment holding exactly values: a key
// present is set, even when its value is empty.
func envGetter(values map[string]string) settingValues {
	return readSettings(func(key string) (string, bool) {
		value, set := values[key]
		return value, set
	})
}

// memoryTestServeEnv makes this test binary run main, so the memory tests (memory_test.go, built
// under the `memory` tag) drive a real Dispatch process and read its own resident memory.
const memoryTestServeEnv = "DISPATCH_MEMORY_TEST_SERVE"

// dispatchTestParallelism is how many of this package's parallel tests run at once when the
// command line names no -parallel (TestMain). Most of them wait - on a Dispatch process they
// started, on Postgres, on a product timer - rather than compute: serially the test binary and its
// processes kept about a tenth of a core busy (41 CPU-s over 376 s, 4 CPUs, build pod). go test's
// default, GOMAXPROCS, is four on CI's runner; eight lets the tests that start no process run
// beside the ones that hold the four process slots (dispatchSlots).
//
// The memory build sets it to zero, which keeps go test's default (parallelism_memory_test.go):
// its tests measure processes' memory against a runner's 16 GB and were sized at that default.
var dispatchTestParallelism = 8

// TestMain runs the package's tests, or, run by a memory test with memoryTestServeEnv set, is that
// test's Dispatch server. It sets the package's default -parallel (dispatchTestParallelism); go
// test parses its flags inside m.Run, so a -parallel on the command line still wins. Only TestMain
// makes a server of a copy a test starts: in a build of these tests without it, such a copy runs
// the tests, each of which starts copies that run them again. dispatchCopy refuses to start a copy
// in such a process (dispatchCopyRefusal), and TestACopyStartedAsTheServerRunsMain fails in such a
// build by name.
func TestMain(m *testing.M) {
	if os.Getenv(memoryTestServeEnv) == "1" {
		main()
		os.Exit(0)
	}
	if dispatchTestParallelism > 0 {
		if err := flag.Set("test.parallel", strconv.Itoa(dispatchTestParallelism)); err != nil {
			panic("set this package's default -parallel: " + err.Error())
		}
	}
	os.Exit(cmdtest.Run(func() int { return storetest.Main(m) }))
}

// dispatchCopy is the path of this test binary, which a memory test runs as its Dispatch server
// with memoryTestServeEnv=1 in the copy's environment. It fails t where dispatchCopyRefusal
// refuses.
func dispatchCopy(t *testing.T) string {
	t.Helper()
	if err := dispatchCopyRefusal(); err != nil {
		t.Fatal(err)
	}
	return os.Args[0]
}

// dispatchCopyRefusal is why this process may start no copy of this test binary, nil when it may.
// A process that has memoryTestServeEnv=1 was started as a Dispatch server, and TestMain runs such
// a process as the server, so one that runs the tests runs in a build without TestMain, and a copy
// started there would run the tests again.
func dispatchCopyRefusal() error {
	if os.Getenv(memoryTestServeEnv) == "1" {
		return fmt.Errorf("this process has %s=1, so a test started it as a Dispatch server, but it runs the tests: this build of the tests has no TestMain that runs the server, and a copy started here would run the tests again", memoryTestServeEnv)
	}
	return nil
}

// dispatchCopyRefusal, which dispatchCopy asks before any copy starts, refuses a process that has
// memoryTestServeEnv=1 and no other. While TestMain is in the build no copy reaches it, so this test
// is what holds the refusal.
func TestAProcessStartedAsTheServerStartsNoCopy(t *testing.T) {
	fixtureenv.Unset(t, memoryTestServeEnv)
	if err := dispatchCopyRefusal(); err != nil {
		t.Fatalf("with %s unset: %v, want no refusal", memoryTestServeEnv, err)
	}
	t.Setenv(memoryTestServeEnv, "1")
	if err := dispatchCopyRefusal(); err == nil || !strings.Contains(err.Error(), memoryTestServeEnv+"=1") {
		t.Fatalf("with %s=1: %v, want a refusal that names it", memoryTestServeEnv, err)
	}
}

// A copy started with memoryTestServeEnv=1 runs main: given -h, main reads it as a subcommand it
// does not know and refuses it, exit 2. In a build without TestMain, the testing package reads -h
// before any test runs, prints its own flag usage and exits 0, so this test fails there by name and
// its copy starts no copy of its own.
func TestACopyStartedAsTheServerRunsMain(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(dispatchCopy(t), "-h")
	cmd.Env = append(os.Environ(), memoryTestServeEnv+"=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if want := `envoy-dispatch: unknown subcommand "-h"`; !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.HasPrefix(string(out), want) {
		t.Fatalf("a copy started as the Dispatch server with -h: %v, want exit 2 and a refusal that starts %q\n%s", err, want, out)
	}
}

func TestWriteBlockIDBackfillReportLabelsSkippedDocuments(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	writeBlockIDBackfillReport(&output, docs.BlockIDBackfill{
		ArtifactID: "artifact-1",
		Skipped:    "service stopping",
	})
	const want = "artifact-1 skipped (service stopping)\n"
	if got := output.String(); got != want {
		t.Fatalf("backfill skipped output = %q, want %q", got, want)
	}
}

func TestWriteBlockIDBackfillReportLabelsDocumentErrors(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if writeBlockIDBackfillReport(&output, docs.BlockIDBackfill{
		ArtifactID: "artifact-1",
		Err:        errors.New("persist update"),
	}) {
		t.Fatal("error report succeeded")
	}
	const want = "artifact-1 error (persist update)\n"
	if got := output.String(); got != want {
		t.Fatalf("backfill error output = %q, want %q", got, want)
	}
}

func TestWriteAnchorBlockBackfillReport(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	writeAnchorBlockBackfillReport(&output, docs.AnchorBlockBackfill{Asks: 2, Comments: 3, Skipped: 1})
	const want = "backfill-anchor-blocks: asks=2 comments=3 skipped=1\n"
	if got := output.String(); got != want {
		t.Fatalf("anchor block backfill output = %q, want %q", got, want)
	}
}

// An empty dashboard origin would make every URL-form mention unrecognisable and the rebuild
// would delete them all, so the subcommand refuses before it opens the database.
func TestRebuildRefsRefusesWithoutServerURL(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if code := rebuildRefs(context.Background(), "postgres://unused", "  ", &output); code != 1 {
		t.Fatalf("rebuild-refs without server URL exited %d, want 1", code)
	}
	if !strings.Contains(output.String(), "dispatch.server_url is required") {
		t.Fatalf("rebuild-refs refusal = %q", output.String())
	}
	output.Reset()
	if code := rebuildRefs(context.Background(), "", "https://dispatch.example", &output); code != 1 || !strings.Contains(output.String(), "DATABASE_URL is required") {
		t.Fatalf("rebuild-refs without DATABASE_URL: code=%d output=%q", code, output.String())
	}
}

// An argument envoy-dispatch does not know is refused, never served: the server migrates the
// database at boot, so `envoy-dispatch census` on an image that predates the subcommand would
// otherwise apply the very migrations the census was to inspect. backfill-embeddings is one such
// argument now: the server's embedding queue keeps coverage itself.
func TestRunSubcommandRefusesAnUnknownSubcommand(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"cenus", "backfill-embeddings"} {
		var stdout, stderr bytes.Buffer
		code := runSubcommand(context.Background(), []string{name}, envGetter(nil), &stdout, &stderr)
		if code != 2 {
			t.Fatalf("%s: exit %d, want 2", name, code)
		}
		for _, want := range []string{fmt.Sprintf("unknown subcommand %q", name), "census", "redeliver-webhooks"} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("%s: stderr %q lacks %q", name, stderr.String(), want)
			}
		}
		if strings.Contains(stderr.String(), "DATABASE_URL") || strings.Contains(stderr.String(), "backfill-embeddings,") {
			t.Errorf("%s: stderr %q reads like a boot, or still lists backfill-embeddings", name, stderr.String())
		}
	}
}

// The census exits 2 when it could not be taken and 0 when nothing refuses, and prints the report
// to stdout.
func TestCensusSubcommandExitCodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var out, errOut bytes.Buffer
	if code := census(ctx, nil, "", &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "DATABASE_URL is required") {
		t.Errorf("no URL: exit %d, stderr %q", code, errOut.String())
	}
	errOut.Reset()
	if code := census(ctx, nil, "postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "census: connect") {
		t.Errorf("unreachable: exit %d, stderr %q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("a census that could not be taken printed a report: %q", out.String())
	}
	database := storetest.Open(t)
	out.Reset()
	errOut.Reset()
	if code := census(ctx, nil, database.Pool.Config().ConnString(), &out, &errOut); code != 0 {
		t.Fatalf("migrated database: exit %d, stderr %q, stdout %q", code, errOut.String(), out.String())
	}
	if !strings.Contains(out.String(), "no pending migration") || !strings.HasSuffix(strings.TrimSpace(out.String()), "census: ok") {
		t.Errorf("stdout:\n%s", out.String())
	}
}

// pgmigrate.Census already prefixes its errors with "census:", so the command prints that
// prefix once when a census cannot be taken.
func TestCensusSubcommandPrintsACensusErrorWithOnePrefix(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	var out, errOut bytes.Buffer
	if code := census(context.Background(), nil, database.Pool.Config().ConnString()+"&search_path=nosuch", &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2; stderr %q", code, errOut.String())
	}
	const want = "census: the connection's search_path names no schema that exists, so current_schema() is null: the census would find neither schema_migrations nor any table, and the runner can create neither; name an existing schema in the search_path\n"
	if got := errOut.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// A database at 52 holding a row 0053's check refuses is refused by the embedded 0053 census,
// exit 1, and the report names the migration, the count and the census file.
func TestCensusSubcommandExitsOneWhenAPendingMigrationsCensusCountsRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		alter table asks drop constraint asks_approval_kind_check;
		delete from schema_migrations where version >= 53;
		-- The template already applied 0068, which dropped users, and 0111, which dropped
		-- delivery_settings.production_job_name; a database at 52 still has both.
		create table users (login text primary key);
		alter table delivery_settings add column production_job_name text;
		-- CORE's default assignee has signed in, so 20261116015736's census counts no project
		-- the owner fill would leave without an owner, and 0053's is the one refusal.
		insert into people (email) values ('alice');
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Spec', '{"kind":"session","id":"s"}', 'U');
		insert into asks (issue_key, author, question, options, kind, approval)
			values ('CORE-1', '{"kind":"session","id":"s"}', 'A question?', '[]', 'question', '{}');
	`); err != nil {
		t.Fatalf("seed a database at 52 with a row 0053 refuses: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := census(ctx, nil, database.Pool.Config().ConnString(), &out, &errOut); code != 1 {
		t.Fatalf("exit %d, want 1; stderr %q, stdout:\n%s", code, errOut.String(), out.String())
	}
	for _, want := range []string{
		"census: schema version 52 (schema_migrations); pending: 0053_asks_approval_kind_check.up.sql, 0054_message_delivery_acceptance.up.sql, 0055_broadcast_idempotency_keys.up.sql",
		"census: REFUSED 0053_asks_approval_kind_check.up.sql: its census counts 1 (0053_asks_approval_kind_check.census.sql)",
		"census: 0055_broadcast_idempotency_keys.up.sql touches broadcasts: 0 rows, ",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
	if last := strings.TrimSpace(out.String()); !strings.HasSuffix(last, "census: REFUSED (1 reason)") {
		t.Errorf("stdout does not end with the verdict:\n%s", out.String())
	}
}

// `envoy-dispatch versions` exits 2 when it cannot be taken, 0 over a database this image
// migrated, and 1 when a recorded version names another file, which it prints. The control is the
// migrated database itself, read before the row is changed.
func TestVersionsSubcommandExitCodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var out, errOut bytes.Buffer
	if code := versions(ctx, "", &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "DATABASE_URL is required") {
		t.Errorf("no URL: exit %d, stderr %q", code, errOut.String())
	}
	database := storetest.Open(t)
	url := database.Pool.Config().ConnString()
	out.Reset()
	errOut.Reset()
	if code := versions(ctx, url, &out, &errOut); code != 0 || !strings.HasSuffix(strings.TrimSpace(out.String()), "versions: ok") {
		t.Fatalf("control, a migrated database: exit %d, stderr %q, stdout:\n%s", code, errOut.String(), out.String())
	}
	if _, err := database.Pool.Exec(ctx, "update schema_migrations set name = '0001_another_branch.up.sql' where version = 1"); err != nil {
		t.Fatalf("record another file under version 1: %v", err)
	}
	out.Reset()
	if code := versions(ctx, url, &out, &errOut); code != 1 || !strings.Contains(out.String(), "versions: REFUSED version 1 is recorded as 0001_another_branch.up.sql") {
		t.Errorf("another file under version 1: exit %d, stdout:\n%s", code, out.String())
	}
}

func TestWriteRebuildRefsReport(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	writeRebuildRefsReport(&output, refs.Rebuild{Documents: 4, Asks: 3, Comments: 2, Messages: 1, Orphans: 5, Edges: 9, BlocksOwed: 4, ItemsOwed: 6})
	const want = "rebuild-refs: documents=4 asks=3 comments=2 messages=1 orphans=5 edges=9 blocks_owed=4 items_owed=6\n"
	if got := output.String(); got != want {
		t.Fatalf("rebuild-refs output = %q, want %q", got, want)
	}
}

// devSignInEnvironment is a boot environment the dev sign-in fence accepts, with overrides
// applied; an override of "" leaves that variable empty, which resolveBootConfig reads as unset.
// With the flag overridden off it is a production cookie server, so it carries the sign-in.
func devSignInEnvironment(overrides map[string]string) settingValues {
	values := map[string]string{
		"DATABASE_URL":         "postgres://postgres:x@127.0.0.1:5432/dispatch_e2e?sslmode=disable",
		"DISPATCH_AGENT_TOKEN": "x",
		"DISPATCH_LISTEN_HOST": "127.0.0.1",
		"DISPATCH_DEV_SIGNIN":  "1",
	}
	for key, value := range overrides {
		values[key] = value
	}
	if values["DISPATCH_DEV_SIGNIN"] == "" && values["DISPATCH_SIGNIN_ISSUER"] == "" {
		values["DISPATCH_SIGNIN_ISSUER"] = "https://issuer.example/pool"
		values["DISPATCH_SIGNIN_CLIENT_ID"] = "dispatch-client"
		values["DISPATCH_SIGNIN_CLIENT_SECRET"] = "client-secret"
		values["DISPATCH_SIGNIN_GROUP"] = "dispatch-members"
	}
	return envGetter(values)
}

func TestResolveBootConfigDevSignInFlagValue(t *testing.T) {
	t.Parallel()
	// Off (getenv answers "" for an unset variable as for an empty one), the fence asks nothing of
	// the rest of the environment.
	boot, err := resolveBootConfig(devSignInEnvironment(map[string]string{
		"DISPATCH_DEV_SIGNIN":  "",
		"DISPATCH_LISTEN_HOST": "0.0.0.0",
		"DATABASE_URL":         "postgres://u@db.example.com/d",
		"DISPATCH_SIGNING_KEY": "abc",
	}))
	if err != nil || boot.DevSignIn {
		t.Fatalf("DISPATCH_DEV_SIGNIN unset: DevSignIn=%t err=%v, want off", boot.DevSignIn, err)
	}

	boot, err = resolveBootConfig(devSignInEnvironment(nil))
	if err != nil || !boot.DevSignIn {
		t.Fatalf("DISPATCH_DEV_SIGNIN=1: DevSignIn=%t err=%v, want on", boot.DevSignIn, err)
	}

	for _, flag := range []string{"true", "yes", "0", " 1"} {
		_, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_DEV_SIGNIN": flag}))
		if err == nil || !strings.Contains(err.Error(), "DISPATCH_DEV_SIGNIN") || !strings.Contains(err.Error(), strconv.Quote(flag)) {
			t.Errorf("DISPATCH_DEV_SIGNIN=%q: err = %v, want a refusal naming the variable and the value", flag, err)
		}
	}
}

func TestResolveBootConfigDevSignInRequiresCookieIdentity(t *testing.T) {
	t.Parallel()
	_, err := resolveBootConfig(devSignInEnvironment(map[string]string{
		"DISPATCH_IDENTITY":                "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
	}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_IDENTITY") {
		t.Fatalf("header identity: err = %v, want a refusal naming DISPATCH_IDENTITY", err)
	}
}

// The fence checks the address the server binds, not a second reading of the variable: every
// spelling it accepts yields boot.ListenAddr, listenAddress's one value, which main binds.
func TestResolveBootConfigDevSignInChecksTheAddressItBinds(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"", "0.0.0.0", "::", "10.0.0.5", "localhost", "::ffff:127.0.0.1", "[::1%lo]", "]]::1[[", "[[::1]]", "127.0.0.1:9000", "[::1]:9000"} {
		boot, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_LISTEN_HOST": host}))
		if err == nil || !strings.Contains(err.Error(), "DISPATCH_LISTEN_HOST") {
			t.Errorf("DISPATCH_LISTEN_HOST=%q: listen address %q, err = %v; want a refusal naming DISPATCH_LISTEN_HOST", host, boot.ListenAddr, err)
		}
	}
	for host, want := range map[string]string{
		"127.0.0.1":       "127.0.0.1:8799",
		" 127.0.0.1 ":     "127.0.0.1:8799",
		"[127.0.0.1]":     "127.0.0.1:8799",
		"127.255.255.254": "127.255.255.254:8799",
		"::1":             "[::1]:8799",
		"[::1]":           "[::1]:8799",
		"0:0:0:0:0:0:0:1": "[0:0:0:0:0:0:0:1]:8799",
	} {
		environment := devSignInEnvironment(map[string]string{"DISPATCH_LISTEN_HOST": host, "DISPATCH_PORT": "8799"})
		bound, err := listenAddress(environment)
		if err != nil || bound != want {
			t.Fatalf("DISPATCH_LISTEN_HOST=%q: listenAddress = %q, %v; want %q", host, bound, err, want)
		}
		boot, err := resolveBootConfig(environment)
		if err != nil || boot.ListenAddr != bound {
			t.Errorf("DISPATCH_LISTEN_HOST=%q: ListenAddr = %q, %v; want the bound %q accepted", host, boot.ListenAddr, err, bound)
		}
	}

	// Without the flag the same field carries the address, so main binds one value either way, and
	// a bad port is refused before anything connects.
	boot, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_DEV_SIGNIN": "", "DISPATCH_LISTEN_HOST": "", "DISPATCH_PORT": "8799"}))
	if err != nil || boot.ListenAddr != ":8799" {
		t.Errorf("without the flag: ListenAddr = %q, %v; want \":8799\"", boot.ListenAddr, err)
	}
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_DEV_SIGNIN": "", "DISPATCH_PORT": "70000"})); err == nil || !strings.Contains(err.Error(), "DISPATCH_PORT") {
		t.Errorf("DISPATCH_PORT=70000: err = %v, want a refusal naming DISPATCH_PORT", err)
	}
}

// With DISPATCH_LISTEN_FD the fence checks the inherited socket's own address, which is what
// serves: a socket on every interface is refused, naming the variable, though DISPATCH_LISTEN_HOST
// names that socket's address correctly, and a loopback one is accepted.
func TestResolveBootConfigDevSignInChecksTheListenSocket(t *testing.T) {
	t.Parallel()
	wildcard, address := listeningFD(t, "tcp4", "0.0.0.0:0")
	_, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_LISTEN_FD": wildcard, "DISPATCH_LISTEN_HOST": "0.0.0.0"}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_DEV_SIGNIN=1 is for a loopback server only") ||
		!strings.Contains(err.Error(), "DISPATCH_LISTEN_FD="+wildcard) || !strings.Contains(err.Error(), address.String()) {
		t.Errorf("dev sign-in on a socket on %s: err = %v, want the fence's refusal naming DISPATCH_LISTEN_FD and the address", address, err)
	}
	loopback, address := listeningFD(t, "tcp4", "127.0.0.1:0")
	boot, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_LISTEN_FD": loopback}))
	if err != nil || !boot.DevSignIn || boot.ListenAddr != address.String() {
		t.Errorf("dev sign-in on a socket on %s: ListenAddr = %q, DevSignIn %t, %v; want accepted", address, boot.ListenAddr, boot.DevSignIn, err)
	}
}

// listeningFD listens on address and returns the number of a descriptor of the listening socket's
// own, as DISPATCH_LISTEN_FD names an inherited one, with the socket's address. Both close when the
// test ends; resolveBootConfig only inspects the descriptor.
func listeningFD(t *testing.T, network, address string) (string, *net.TCPAddr) {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		t.Fatalf("listen on %s: %v", address, err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	file, err := listener.(*net.TCPListener).File()
	if err != nil {
		t.Fatalf("take the listening socket's descriptor: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return strconv.Itoa(int(file.Fd())), listener.Addr().(*net.TCPAddr)
}

func TestResolveBootConfigDevSignInRequiresALoopbackDatabase(t *testing.T) {
	// Not parallel: it sets environment variables for the whole process.
	for _, tc := range []struct{ databaseURL, host string }{
		{databaseURL: "postgres://postgres:x@db.example.com:5432/d", host: "db.example.com"},
		{databaseURL: "postgres://u@10.0.0.5/d", host: "10.0.0.5"},
		{databaseURL: "postgres://u@127.0.0.1:5432,db.example.com:5432/d", host: "db.example.com"},
		{databaseURL: "host=db.example.com dbname=d", host: "db.example.com"},
		// pgx dials the host parameter, not the URL's authority.
		{databaseURL: "postgres://u@127.0.0.1:5432/d?host=db.example.com", host: "db.example.com"},
	} {
		_, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DATABASE_URL": tc.databaseURL}))
		if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") || !strings.Contains(err.Error(), tc.host) {
			t.Errorf("DATABASE_URL=%q: err = %v, want a refusal naming DATABASE_URL and %s", tc.databaseURL, err, tc.host)
		}
	}
	for _, databaseURL := range []string{
		"postgres://u@127.0.0.1:5432/d",
		"postgres://u@localhost:5432/d",
		"postgres://u@[::1]:5432/d",
		"postgres:///d?host=/var/run/postgresql",
		"host=/tmp dbname=d",
	} {
		if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DATABASE_URL": databaseURL})); err != nil {
			t.Errorf("DATABASE_URL=%q: %v, want a loopback database accepted", databaseURL, err)
		}
	}

	// A URL naming no host is dialled at libpq's PGHOST default, which the check reads too.
	t.Setenv("PGHOST", "db.example.com")
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DATABASE_URL": "postgres:///d"})); err == nil || !strings.Contains(err.Error(), "db.example.com") {
		t.Errorf("DATABASE_URL=postgres:///d with PGHOST=db.example.com: err = %v, want a refusal naming db.example.com", err)
	}
}

func TestResolveBootConfigDevSignInRefusesAConfiguredSigningKey(t *testing.T) {
	t.Parallel()
	_, err := resolveBootConfig(devSignInEnvironment(map[string]string{"DISPATCH_SIGNING_KEY": "abc"}))
	if err == nil || !strings.Contains(err.Error(), "DISPATCH_SIGNING_KEY") {
		t.Fatalf("configured signing key: err = %v, want a refusal naming DISPATCH_SIGNING_KEY", err)
	}
}

// With the flag a loopback client acts as any person it names, so the process may not publish
// into another machine's NATS.
func TestResolveBootConfigDevSignInRefusesRemoteNATS(t *testing.T) {
	t.Parallel()
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_ALLOW_REMOTE_NATS": "1"})); err == nil || !strings.Contains(err.Error(), "ENVOY_ALLOW_REMOTE_NATS") {
		t.Fatalf("ENVOY_ALLOW_REMOTE_NATS=1 with NATS on: err = %v, want a refusal naming ENVOY_ALLOW_REMOTE_NATS", err)
	}
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_ALLOW_REMOTE_NATS": "1", "DISPATCH_NATS_DISABLED": "1"})); err != nil {
		t.Fatalf("ENVOY_ALLOW_REMOTE_NATS=1 with NATS off: %v, want accepted", err)
	}
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_ALLOW_REMOTE_NATS": "1", "DISPATCH_DEV_SIGNIN": ""})); err != nil {
		t.Fatalf("ENVOY_ALLOW_REMOTE_NATS=1 without the flag: %v, want accepted", err)
	}
}

// With the flag a loopback client acts as any person it names, so the process may not decide
// another machine's secrets broker requests as one.
func TestResolveBootConfigDevSignInRefusesARemoteAgentSecretsBroker(t *testing.T) {
	t.Parallel()
	broker := func(brokerURL string) map[string]string {
		return map[string]string{
			"DISPATCH_AGENT_SECRETS_URL":                brokerURL,
			"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": "/var/run/secrets/broker/token",
		}
	}
	for _, brokerURL := range []string{"https://broker.example.com", "http://10.0.0.5:13380"} {
		if _, err := resolveBootConfig(devSignInEnvironment(broker(brokerURL))); err == nil || !strings.Contains(err.Error(), "DISPATCH_AGENT_SECRETS_URL") {
			t.Errorf("DISPATCH_AGENT_SECRETS_URL=%s: err = %v, want a refusal naming DISPATCH_AGENT_SECRETS_URL", brokerURL, err)
		}
	}
	for _, brokerURL := range []string{"http://127.0.0.1:13380", "http://localhost:13380", "http://[::1]:13380"} {
		if _, err := resolveBootConfig(devSignInEnvironment(broker(brokerURL))); err != nil {
			t.Errorf("DISPATCH_AGENT_SECRETS_URL=%s: %v, want a loopback broker accepted", brokerURL, err)
		}
	}
	withoutFlag := broker("https://broker.example.com")
	withoutFlag["DISPATCH_DEV_SIGNIN"] = ""
	if _, err := resolveBootConfig(devSignInEnvironment(withoutFlag)); err != nil {
		t.Errorf("a remote broker without the flag: %v, want accepted", err)
	}
}

// With the flag a loopback client acts as any person it names, so the process may not deliver
// that human's mentions through another machine's Envoy listener.
func TestResolveBootConfigDevSignInRefusesARemoteEnvoyListener(t *testing.T) {
	t.Parallel()
	for _, listenerURL := range []string{"https://listener.example.com", "http://10.0.0.5:9020"} {
		if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_URL": listenerURL})); err == nil || !strings.Contains(err.Error(), "ENVOY_URL") {
			t.Errorf("ENVOY_URL=%s: err = %v, want a refusal naming ENVOY_URL", listenerURL, err)
		}
	}
	// Unset, ENVOY_URL defaults to this machine's listener.
	for _, listenerURL := range []string{"", "http://127.0.0.1:9020", "http://localhost:9020", "http://[::1]:9020"} {
		if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_URL": listenerURL})); err != nil {
			t.Errorf("ENVOY_URL=%q: %v, want a loopback listener accepted", listenerURL, err)
		}
	}
	if _, err := resolveBootConfig(devSignInEnvironment(map[string]string{"ENVOY_URL": "https://listener.example.com", "DISPATCH_DEV_SIGNIN": ""})); err != nil {
		t.Errorf("a remote listener without the flag: %v, want accepted", err)
	}
}

// devSignInOrigin is a dashboard origin the dev sign-in fence accepts.
const devSignInOrigin = "http://127.0.0.1:8799"

// With the flag a loopback client acts as any person it names, and a person can have the App probe
// and import any repository it is installed on, so an App private key from the environment may
// sign calls only to a loopback host.
func TestDevSignInRefusesAnAppKeyThatReachesGitHub(t *testing.T) {
	t.Parallel()
	withKey := &auth.AppConfig{ClientID: "Iv1.app", PEM: "app private key"}
	fromEnv := appCredentialSource{}
	// Unset, the App calls https://api.github.com.
	for _, base := range []string{"", "https://api.github.com", "http://10.0.0.5:9022", "http://127.0.0.1@api.github.com"} {
		err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_GITHUB_API_BASE": base}), devSignInOrigin, withKey, fromEnv)
		if err == nil || !strings.Contains(err.Error(), "DISPATCH_GITHUB_API_BASE="+strconv.Quote(base)) || !strings.Contains(err.Error(), "DISPATCH_APP_PEM_B64") {
			t.Errorf("DISPATCH_GITHUB_API_BASE=%q: err = %v, want a refusal naming DISPATCH_GITHUB_API_BASE and DISPATCH_APP_PEM_B64", base, err)
		}
	}
	// The e2e harness points the App at its fake GitHub on 127.0.0.1 (packages/dispatch/e2e/run-server.sh).
	for _, base := range []string{"http://127.0.0.1:9022", "http://localhost:9022", "http://[::1]:9022"} {
		if err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_GITHUB_API_BASE": base}), devSignInOrigin, withKey, fromEnv); err != nil {
			t.Errorf("DISPATCH_GITHUB_API_BASE=%s: %v, want a loopback host accepted", base, err)
		}
	}
	// No App, or an App without its private key, signs no App call.
	for _, app := range []*auth.AppConfig{nil, {ClientID: "Iv1.app"}} {
		if err := devSignInLoadedFence(devSignInBoot(t, nil), devSignInOrigin, app, fromEnv); err != nil {
			t.Errorf("App %+v: %v, want accepted", app, err)
		}
	}
	if err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_DEV_SIGNIN": ""}), devSignInOrigin, withKey, fromEnv); err != nil {
		t.Errorf("an App key reaching GitHub without the flag: %v, want accepted", err)
	}
}

// The dashboard origin comes from envoy.json, which main reads after the environment, so the fence
// over loaded values checks it, before anything connects.
func TestDevSignInLoadedFenceRefusesAPublicOrigin(t *testing.T) {
	t.Parallel()
	const public = "https://dispatch.example.com"
	if err := devSignInLoadedFence(devSignInBoot(t, nil), public, nil, appCredentialSource{}); err == nil || !strings.Contains(err.Error(), "DISPATCH_SERVER_URL") {
		t.Errorf("origin %s with the flag: err = %v, want a refusal naming DISPATCH_SERVER_URL", public, err)
	}
	if err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_DEV_SIGNIN": ""}), public, nil, appCredentialSource{}); err != nil {
		t.Errorf("origin %s without the flag: %v, want accepted", public, err)
	}
}

// app.json is where a developer keeps the real App's key, and a loopback base proves only where the
// port is, not that a fake owns it. So with the flag a key from app.json is refused whatever the
// base, naming the file and the environment key that replaces it, while a throwaway key in the
// environment with a loopback fake boots, as packages/dispatch/e2e/run-server.sh's does.
func TestDevSignInRefusesAnAppJSONKeyWhateverTheBase(t *testing.T) {
	t.Parallel()
	const fake = "http://127.0.0.1:9022"
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "app.json")
	if err := os.WriteFile(path, []byte(`{"clientId":"Iv1.file","pem":"app private key"}`), 0o600); err != nil {
		t.Fatalf("write app.json: %v", err)
	}
	app, source, err := loadAppCredentials(envGetter(nil), dataDir)
	if err != nil || app == nil {
		t.Fatalf("loadAppCredentials from app.json: %+v, %v", app, err)
	}
	for _, base := range []string{fake, "http://localhost:9022", "http://[::1]:9022", "", "https://api.github.com"} {
		err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_GITHUB_API_BASE": base}), devSignInOrigin, app, source)
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "DISPATCH_APP_PEM_B64") {
			t.Errorf("a key in app.json, DISPATCH_GITHUB_API_BASE=%q: err = %v, want a refusal naming %s and DISPATCH_APP_PEM_B64", base, err, path)
		}
	}
	if err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_DEV_SIGNIN": ""}), devSignInOrigin, app, source); err != nil {
		t.Errorf("a key in app.json without the flag: %v, want accepted", err)
	}

	_, throwaway := githubapptest.Key(t)
	app, source, err = loadAppCredentials(envGetter(map[string]string{
		"DISPATCH_APP_CLIENT_ID": "Iv1.env",
		"DISPATCH_APP_PEM_B64":   base64.StdEncoding.EncodeToString([]byte(throwaway)),
	}), dataDir)
	if err != nil || app == nil {
		t.Fatalf("loadAppCredentials from the environment: %+v, %v", app, err)
	}
	if err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_GITHUB_API_BASE": fake}), devSignInOrigin, app, source); err != nil {
		t.Errorf("a key in DISPATCH_APP_PEM_B64 with a loopback fake: %v, want accepted", err)
	}
	if err := devSignInLoadedFence(devSignInBoot(t, nil), devSignInOrigin, app, source); err == nil || !strings.Contains(err.Error(), "DISPATCH_APP_PEM_B64") || strings.Contains(err.Error(), path) {
		t.Errorf("a key in DISPATCH_APP_PEM_B64, DISPATCH_GITHUB_API_BASE unset: err = %v, want a refusal naming DISPATCH_APP_PEM_B64 and not %s", err, path)
	}
}

// devSignInBoot resolves devSignInEnvironment(overrides), which the boot fence accepts.
func devSignInBoot(t *testing.T, overrides map[string]string) bootConfig {
	t.Helper()
	boot, err := resolveBootConfig(devSignInEnvironment(overrides))
	if err != nil {
		t.Fatalf("resolveBootConfig(%v): %v", overrides, err)
	}
	return boot
}

func TestListenAddressJoinsAnIPv6LoopbackHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ host, port, want string }{
		{host: "::1", port: "", want: "[::1]:8766"},
		{host: "::1", port: "8799", want: "[::1]:8799"},
		{host: "[::1]", port: "", want: "[::1]:8766"},
		{host: "[::1]", port: "8799", want: "[::1]:8799"},
		{host: "127.0.0.1", port: "8799", want: "127.0.0.1:8799"},
		{host: "127.0.0.1", port: "", want: "127.0.0.1:8766"},
		{host: "", port: "", want: ":8766"},
		{host: "", port: "8799", want: ":8799"},
	} {
		got, err := listenAddress(envGetter(map[string]string{"DISPATCH_LISTEN_HOST": tc.host, "DISPATCH_PORT": tc.port}))
		if err != nil || got != tc.want {
			t.Errorf("host %q port %q: listenAddress = %q, %v; want %q", tc.host, tc.port, got, err, tc.want)
		}
	}
	// 2^64+1000 overflows int: a check that ignores overflow accepts it, and only the bind, after
	// NATS and the migrations, refuses it. It must be refused here, before anything connects.
	for _, port := range []string{"0", "70000", "18446744073709552616"} {
		if got, err := listenAddress(envGetter(map[string]string{"DISPATCH_LISTEN_HOST": "127.0.0.1", "DISPATCH_PORT": port})); err == nil || !strings.Contains(err.Error(), "DISPATCH_PORT") {
			t.Errorf("port %q: listenAddress = %q, %v; want a refusal naming DISPATCH_PORT", port, got, err)
		}
	}
}

// seedBackfillFile writes an issue holding one file artifact, notes-bin, with version 1 holding
// its bytes in its row, and returns those bytes.
func seedBackfillFile(t *testing.T, database *store.Store) []byte {
	t.Helper()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('FILES', 'Files', 'alice')`,
		`insert into issues (key, project_key, number, title, status, created_by, rank) values ('FILES-1', 'FILES', 1, 'Issue', 'todo', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into artifacts (issue_key, project_key, slug, name, kind, created_by) values ('FILES-1', 'FILES', 'notes-bin', 'notes.bin', 'file', '{"kind":"user","id":"alice"}')`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	body := []byte("an uploaded file")
	addBackfillVersion(t, database, 1, body, files.SHA256(body))
	return body
}

// addBackfillVersion adds version number of notes-bin, holding content in its row under sha.
func addBackfillVersion(t *testing.T, database *store.Store, number int, content []byte, sha string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256)
		select id, $1, $2, 'application/octet-stream', $3, $4 from artifacts where slug = 'notes-bin'
	`, number, content, len(content), sha); err != nil {
		t.Fatalf("seed version %d: %v", number, err)
	}
}

// rowHoldsBytes reports whether version number of the file holds its bytes in its row.
func rowHoldsBytes(t *testing.T, database *store.Store, number int) bool {
	t.Helper()
	var held bool
	if err := database.Pool.QueryRow(context.Background(), `select content is not null from artifact_versions where number = $1`, number).Scan(&held); err != nil {
		t.Fatalf("read version %d: %v", number, err)
	}
	return held
}

// runBackfillFiles runs envoy-dispatch backfill-files with args against database and bucket, and
// returns its exit code and what it wrote to stdout and stderr.
func runBackfillFiles(t *testing.T, database *store.Store, bucket string, args ...string) (int, string) {
	t.Helper()
	env := envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString(), "DISPATCH_FILE_STORE_BUCKET": bucket})
	var stdout, stderr bytes.Buffer
	code := runSubcommand(context.Background(), append([]string{"backfill-files"}, args...), env, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// envoy-dispatch backfill-files moves every file row's bytes into the bucket and exits 0, and
// exits 1 when it stops short; --verify-only exits 0 while every moved file reads back and 1 once
// one does not. Those exit codes are what an operator's script reads before it trusts that the
// rows' bytes are safe to have cleared.
func TestBackfillFilesExitsNonZeroUntilEveryFileReadsBackFromTheBucket(t *testing.T) {
	// Not parallel: it sets environment variables for the whole process.
	database := storetest.Open(t)
	const bucket = "example-files-bucket"
	fake := filestest.ServeS3(t, bucket)
	body := seedBackfillFile(t, database)
	key := files.Key(files.SHA256(body))

	if code, out := runBackfillFiles(t, database, bucket, "--verify-everything"); code != 2 {
		t.Fatalf("backfill-files --verify-everything: exit %d, want 2:\n%s", code, out)
	}
	// A bucket that does not answer stops the run before any row is touched.
	if code, out := runBackfillFiles(t, database, "example-missing-bucket"); code != 1 || !strings.Contains(out, "not reachable") || !rowHoldsBytes(t, database, 1) {
		t.Fatalf("backfill-files against a missing bucket: exit %d, row holds bytes %t, want exit 1 with the row untouched:\n%s", code, rowHoldsBytes(t, database, 1), out)
	}

	if code, out := runBackfillFiles(t, database, bucket); code != 0 || rowHoldsBytes(t, database, 1) {
		t.Fatalf("backfill-files: exit %d, row holds bytes %t, want exit 0 with the row cleared:\n%s", code, rowHoldsBytes(t, database, 1), out)
	}
	if _, stored, held := fake.Object(key); !held || !bytes.Equal(stored, body) {
		t.Fatalf("the bucket holds %d bytes under %s (held %t), want the file", len(stored), key, held)
	}
	if code, out := runBackfillFiles(t, database, bucket, "--verify-only"); code != 0 || !strings.Contains(out, "verified=1 bytes=0 failed=0") {
		t.Fatalf("backfill-files --verify-only: exit %d, want 0 with the one file verified:\n%s", code, out)
	}
	if code, out := runBackfillFiles(t, database, bucket, "--verify-only", "--restore"); code != 2 {
		t.Fatalf("backfill-files --verify-only --restore: exit %d, want 2:\n%s", code, out)
	}

	// The rollback writes the bytes back from the bucket; a backfill moves them out again.
	if code, out := runBackfillFiles(t, database, bucket, "--restore"); code != 0 || !strings.Contains(out, "restored=1") || !rowHoldsBytes(t, database, 1) {
		t.Fatalf("backfill-files --restore: exit %d, row holds bytes %t, want exit 0 with the row filled:\n%s", code, rowHoldsBytes(t, database, 1), out)
	}
	if code, out := runBackfillFiles(t, database, bucket); code != 0 || rowHoldsBytes(t, database, 1) {
		t.Fatalf("backfill-files after a restore: exit %d, row holds bytes %t, want exit 0 with the row cleared again:\n%s", code, rowHoldsBytes(t, database, 1), out)
	}

	fake.DeleteObject(key)
	if code, out := runBackfillFiles(t, database, bucket, "--verify-only"); code != 1 || !strings.Contains(out, "failed=1") {
		t.Fatalf("backfill-files --verify-only with the object gone: exit %d, want 1 naming the failure:\n%s", code, out)
	}
	if code, out := runBackfillFiles(t, database, bucket, "--restore"); code != 1 || !strings.Contains(out, "FAILED version") || rowHoldsBytes(t, database, 1) {
		t.Fatalf("backfill-files --restore with the object gone: exit %d, row holds bytes %t, want exit 1 with the row left empty:\n%s", code, rowHoldsBytes(t, database, 1), out)
	}

	// A row whose bytes do not match its hash is named and passed over: exit 1, the row untouched,
	// and every other row still moved.
	addBackfillVersion(t, database, 2, []byte("other bytes"), files.SHA256(body))
	addBackfillVersion(t, database, 3, []byte("sound bytes"), files.SHA256([]byte("sound bytes")))
	if code, out := runBackfillFiles(t, database, bucket); code != 1 || !strings.Contains(out, "FAILED version") || !rowHoldsBytes(t, database, 2) || rowHoldsBytes(t, database, 3) {
		t.Fatalf("backfill-files over a row it cannot move: exit %d, corrupt row holds bytes %t, sound row holds bytes %t, want exit 1 with the corrupt row untouched and the sound row moved:\n%s", code, rowHoldsBytes(t, database, 2), rowHoldsBytes(t, database, 3), out)
	}
}

// envoy-dispatch backfill-files moves no row while a live server process stores no file in a
// bucket, since that process serves a file only from its row: it exits 1 naming each such
// process, one started without DISPATCH_FILE_STORE_BUCKET and one of a build that predates the
// fleet columns, which registers no file_store. --verify-only and --restore, which take no bytes
// out of a row, still run. Once those rows lapse, the backfill moves the row.
func TestBackfillFilesRefusesWhileAProcessWithoutABucketIsLive(t *testing.T) {
	// Not parallel: it sets environment variables for the whole process.
	database := storetest.Open(t)
	const bucket = "example-files-bucket"
	filestest.ServeS3(t, bucket)
	ctx := context.Background()
	seedBackfillFile(t, database)
	var bucketless, older string
	// The bucketless process is this build's, started without DISPATCH_FILE_STORE_BUCKET, so its
	// row names this build's owner lock protocol; the older one's, like its build, names none.
	if err := database.Pool.QueryRow(ctx, `
		insert into server_processes (instance, commit, signing_keys, schema_version, schema_creatable, file_store, owner_lock_protocol)
		values (gen_random_uuid(), 'bucketless0commit', '{fingerprint}', 1, 1, false, $1) returning instance::text`, store.OwnerLockProtocol).Scan(&bucketless); err != nil {
		t.Fatalf("add a bucketless server process: %v", err)
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into server_processes (instance, commit, signing_keys) values (gen_random_uuid(), 'older0commit', '{fingerprint}')
		returning instance::text`).Scan(&older); err != nil {
		t.Fatalf("add a server process of a build that predates the fleet columns: %v", err)
	}

	code, out := runBackfillFiles(t, database, bucket)
	if code != 1 || !rowHoldsBytes(t, database, 1) || !strings.Contains(out, bucketless) || !strings.Contains(out, older) {
		t.Fatalf("backfill-files beside live processes without a bucket: exit %d, row holds bytes %t, want exit 1 naming %s and %s with the row untouched:\n%s",
			code, rowHoldsBytes(t, database, 1), bucketless, older, out)
	}
	if code, out := runBackfillFiles(t, database, bucket, "--verify-only"); code != 0 {
		t.Fatalf("backfill-files --verify-only beside them: exit %d, want 0:\n%s", code, out)
	}
	if code, out := runBackfillFiles(t, database, bucket, "--restore"); code != 0 {
		t.Fatalf("backfill-files --restore beside them: exit %d, want 0:\n%s", code, out)
	}

	if _, err := database.Pool.Exec(ctx, `update server_processes set heartbeat_at = now() - interval '1 hour'`); err != nil {
		t.Fatalf("let the processes lapse: %v", err)
	}
	if code, out := runBackfillFiles(t, database, bucket); code != 0 || rowHoldsBytes(t, database, 1) {
		t.Fatalf("backfill-files once they lapsed: exit %d, row holds bytes %t, want exit 0 with the row cleared:\n%s", code, rowHoldsBytes(t, database, 1), out)
	}
}

func TestExplainMigrateErrorNamesTheDeploymentBootstrapOnAVectorPermissionDenial(t *testing.T) {
	t.Parallel()
	pgErr := &pgconn.PgError{Code: "42501", Message: `permission denied to create extension "vector"`}
	original := fmt.Errorf("apply migration 72: execute migration: migration 0072_embeddings_core.up.sql: %w", pgErr)
	explained := explainMigrateError(original)
	if !errors.Is(explained, original) {
		t.Fatalf("explainMigrateError does not wrap the original error (errors.Is fails)")
	}
	if !strings.Contains(explained.Error(), "deployment's bootstrap") {
		t.Errorf("explainMigrateError(%v) = %v, want it to name the deployment bootstrap", original, explained)
	}
}

func TestExplainMigrateErrorPassesThroughAnUnrelatedMigrationFailure(t *testing.T) {
	t.Parallel()
	cases := map[string]error{
		"a different SQLSTATE entirely": fmt.Errorf(
			"apply migration 42: execute migration: migration 0042_something.up.sql: %w",
			&pgconn.PgError{Code: "42P01", Message: `relation "widgets" does not exist`},
		),
		"42501 but not about the vector extension": fmt.Errorf(
			"apply migration 43: execute migration: migration 0043_other.up.sql: %w",
			&pgconn.PgError{Code: "42501", Message: `permission denied for table widgets`},
		),
		"no PgError in the chain at all": errors.New(
			`apply migration 72: execute migration: migration 0072_embeddings_core.up.sql: ERROR: permission denied to create extension "vector"`,
		),
	}
	for name, original := range cases {
		t.Run(name, func(t *testing.T) {
			if got := explainMigrateError(original); got != original {
				t.Errorf("explainMigrateError(%v) = %v, want the original error unchanged", original, got)
			}
		})
	}
	if explainMigrateError(nil) != nil {
		t.Error("explainMigrateError(nil) is not nil")
	}
}

// ───── the keyword fill ─────────────────────────────────────────────────────

// The lines the keyword fill (textversions.Run) logs as it takes its hold and as it stands by for
// the hold of another process, and the line a process logs at boot when DISPATCH_SEARCH_FILL=off
// keeps it from starting the fill. This file and the multiprocess rows (multiprocess_test.go) read
// them. The deferral is written out here rather than taken from main.go's searchFillDeferredLine,
// so the test holds the text an operator reads in the boot log.
const (
	keywordFillTookLine     = `msg="dispatch: the keyword fill took its hold; this process fills the keyword table"`
	keywordFillStandbyLine  = `msg="dispatch: another process holds the keyword fill's hold; standing by"`
	keywordFillDeferredLine = `msg="dispatch: the keyword fill is deferred; this process fills no keyword table" setting=DISPATCH_SEARCH_FILL value=off`
)

// A process that the registry of live server processes refuses writes no keyword version. A live
// peer does not share the signing keys of the process, so the process waits for admission
// (processes.Register: until a live window and a heartbeat after its first refusal), serves
// nothing and exits 1. Its keyword fill starts only after the registry admits it: the pause of the
// fill reads the answers reads that this process serves, and a process that is not admitted serves
// none. So for the whole wait, and after the exit, the fill has no hold, no row and no cursor,
// although 200 events wait for their rows.
func TestAProcessTheRegistryRefusesWritesNoKeywordVersion(t *testing.T) {
	// Not parallel: it holds a step to a wall-clock bound load could break (the boot's wait of
	// LiveWindow + HeartbeatInterval + 30 s past the refusing row).
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	writeKeywordFillComments(t, database, 200)
	// The heartbeat of the peer is an hour ahead of the database's clock, so the peer stays live
	// (processes.Live) for the whole test with no heartbeat of its own. The peer has the owner
	// lock protocol of this build, as a peer of this build registers it (processes.admit), so the
	// process does not find an earlier build in the fleet.
	if _, err := database.Pool.Exec(context.Background(), `
		insert into server_processes (instance, commit, signing_keys, heartbeat_at, owner_lock_protocol)
		values (gen_random_uuid(), 'peer0commit', '{a-fingerprint-this-process-does-not-hold}', now() + interval '1 hour', $1)`,
		store.OwnerLockProtocol); err != nil {
		t.Fatalf("add a live peer that does not share the keys of the process: %v", err)
	}

	process := startDispatchProcessWith(t, databaseURLOf(t, database), dispatchOptions{SigningKey: "a-key-the-peer-does-not-share"})
	process.waitForLog(t, 30*time.Second, "a live server process refuses this one's signing keys; waiting for its heartbeat to lapse")
	refused := time.Now()
	// The test reads the database every 250 ms until the process exits, so it sees each moment of
	// the wait in which a fill could take the hold and write.
	stillRunning := refused.Add(processes.LiveWindow + processes.HeartbeatInterval + 30*time.Second)
	for waiting := true; waiting; {
		requireNoKeywordFillWrite(t, database, fmt.Sprintf("%v into the wait for admission", time.Since(refused).Round(time.Millisecond)))
		select {
		case <-process.Exited:
			waiting = false
		case <-time.After(250 * time.Millisecond):
			if time.Now().After(stillRunning) {
				t.Fatalf("the refused process still runs %v after its first refusal:\n%s", time.Since(refused).Round(time.Millisecond), process.Output.String())
			}
		}
	}
	if waited := time.Since(refused); waited < processes.LiveWindow {
		t.Fatalf("the refused process exited %v after its first refusal; the test needs the whole wait of %v:\n%s",
			waited.Round(time.Millisecond), processes.LiveWindow+processes.HeartbeatInterval, process.Output.String())
	}

	if code := process.Cmd.ProcessState.ExitCode(); code != 1 ||
		!process.logged(`msg="dispatch: register this server process"`, "signing key set does not match a live server process") {
		t.Fatalf("the refused process exited %d; want 1, with the refusal of the registry logged:\n%s", code, process.Output.String())
	}
	requireNoKeywordFillWrite(t, database, "after the refused process exited")
	for _, line := range []string{keywordFillTookLine, keywordFillStandbyLine} {
		if process.logged(line) {
			t.Fatalf("the refused process logged %s; want a keyword fill that never started:\n%s", line, process.Output.String())
		}
	}
}

// requireNoKeywordFillWrite fails the test when the keyword fill took its hold or wrote: a
// duty_holds row of the hold of the fill, a row of the keyword table, or a cursor row, whatever
// event the cursor row names (a row at event 0 is a write as much as one at event 50). when says
// at which moment the test reads them.
func requireNoKeywordFillWrite(t *testing.T, database *store.Store, when string) {
	t.Helper()
	if writes := readKeywordFillWrites(t, database); writes != (keywordFillWrites{}) {
		t.Fatalf("%s, the keyword fill has %d holds, %d rows and %d cursor rows; want none of each", when, writes.holds, writes.rows, writes.cursors)
	}
}

// keywordFillWrites counts what the keyword fill takes and writes: the duty_holds rows of its hold
// (search-text-versions-fill), the rows of search_text_versions, and the rows of
// search_text_versions_fill, the table of its cursor.
type keywordFillWrites struct{ holds, rows, cursors int }

// readKeywordFillWrites reads keywordFillWrites in one statement.
func readKeywordFillWrites(t *testing.T, database *store.Store) keywordFillWrites {
	t.Helper()
	var writes keywordFillWrites
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from duty_holds where name = 'search-text-versions-fill'),
		       (select count(*) from search_text_versions),
		       (select count(*) from search_text_versions_fill)`).Scan(&writes.holds, &writes.rows, &writes.cursors); err != nil {
		t.Fatalf("read the hold, the rows and the cursor row of the keyword fill: %v", err)
	}
	return writes
}

// writeKeywordFillComments commits count comment.created events of textversionstest.Issue, each a
// new comment and so one version, appended through the broker as the routes append them, at most
// 500 to a transaction of their own (textversionstest.CommitEvents), and answers their ids in
// order.
func writeKeywordFillComments(t *testing.T, database *store.Store, count int) []int64 {
	t.Helper()
	broker := events.NewBroker()
	issue := textversionstest.Issue
	actor := model.Actor{Kind: "user", ID: "alice"}
	ids := make([]int64, 0, count)
	for written := 0; written < count; {
		inThisOne := min(count-written, 500)
		comments := make([]model.Event, 0, inThisOne)
		for n := range inThisOne {
			body := fmt.Sprintf("The signing keys of region %d rotate on Monday.", written+n)
			comments = append(comments, model.Event{IssueKey: &issue, Type: "comment.created", Actor: actor,
				Payload: map[string]any{"id": uuid.NewString(), "issue_key": issue, "body": body}})
		}
		ids = append(ids, textversionstest.CommitEvents(t, database, broker, comments...)...)
		written += inThisOne
	}
	return ids
}

// keywordFillDeferredWatch is how long a process booted with DISPATCH_SEARCH_FILL=off is watched,
// from its admission to the registry of live server processes, for a keyword fill that started
// anyway: longer than the 10 s a standby waits between its tries for the hold, where a fill that
// started takes the hold the moment its process serves and writes the waiting events' rows.
const keywordFillDeferredWatch = 15 * time.Second

// keywordFillTablesEmpty is the end of the deferral line of a process booted with
// DISPATCH_SEARCH_FILL=off on a database where neither table the keyword fill writes holds a row
// (searchFillTableRows).
const keywordFillTablesEmpty = "search_text_versions_rows=none search_text_versions_fill_rows=none"

// DISPATCH_SEARCH_FILL decides whether a serving process starts the keyword fill. Booted with it
// off, a process that the registry of live server processes admitted, and so serves, logs once
// that the fill is deferred, saying that neither table the fill writes holds a row, and for
// keywordFillDeferredWatch takes no hold of the fill's and writes no row and no cursor row, though
// 50 events wait for their rows and 50 more are written while it serves. Booted with it on, on a
// database of its own holding the same events, a process takes the hold, writes every event's row
// and the cursor row, and logs no deferral; the same read that finds none of them in the off row
// counts each of them there.
func TestDispatchSearchFillDecidesWhetherTheKeywordFillStarts(t *testing.T) {
	t.Parallel()
	t.Run("off", func(t *testing.T) {
		database := storetest.Open(t)
		textversionstest.SeedIssue(t, database)
		writeKeywordFillComments(t, database, 50)
		process := startDispatchProcessWith(t, databaseURLOf(t, database), dispatchOptions{Env: map[string]string{"DISPATCH_SEARCH_FILL": "off"}})
		process.waitHealthy(t)
		awaitRegistryAdmission(t, database, process)
		writeKeywordFillComments(t, database, 50)
		admitted := time.Now()
		for time.Since(admitted) < keywordFillDeferredWatch {
			requireNoKeywordFillWrite(t, database, fmt.Sprintf("%v after the process was admitted", time.Since(admitted).Round(time.Millisecond)))
			time.Sleep(250 * time.Millisecond)
		}
		requireNoKeywordFillWrite(t, database, "at the end of the watch")
		if lines := process.logLines(keywordFillDeferredLine); len(lines) != 1 {
			t.Fatalf("the process logged the deferral %d times; want once:\n%s", len(lines), process.Output.String())
		}
		if !process.logged(keywordFillDeferredLine + " " + keywordFillTablesEmpty) {
			t.Fatalf("the deferral does not end with %s, though neither table holds a row:\n%s", keywordFillTablesEmpty, process.Output.String())
		}
		for _, line := range []string{keywordFillTookLine, keywordFillStandbyLine} {
			if process.logged(line) {
				t.Fatalf("the process logged %s; want a keyword fill that never started:\n%s", line, process.Output.String())
			}
		}
	})
	t.Run("on", func(t *testing.T) {
		database := storetest.Open(t)
		textversionstest.SeedIssue(t, database)
		before := writeKeywordFillComments(t, database, 50)
		process := startDispatchProcessWith(t, databaseURLOf(t, database), dispatchOptions{Env: map[string]string{"DISPATCH_SEARCH_FILL": "on"}})
		process.waitHealthy(t)
		process.waitForLog(t, 30*time.Second, keywordFillTookLine)
		after := writeKeywordFillComments(t, database, 50)
		textversionstest.AwaitRowsUntil(t, database, append(before, after...), nil)
		if writes := readKeywordFillWrites(t, database); writes.holds == 0 || writes.rows == 0 || writes.cursors == 0 {
			t.Fatalf("the fill wrote every event's row, and the read counts %d holds, %d rows and %d cursor rows; want each above 0", writes.holds, writes.rows, writes.cursors)
		}
		if process.logged(keywordFillDeferredLine) {
			t.Fatalf("the process logged the deferral with DISPATCH_SEARCH_FILL=on:\n%s", process.Output.String())
		}
	})
}

// searchFillTableRows answers the deferral line's fields, one read per table the keyword fill
// writes. On a migrated database that no fill has written, both say none. A row of the keyword
// table makes its field some and leaves the cursor's none, and the cursor's row alone does the
// reverse. A table the read cannot read, here the keyword table dropped, makes its field unknown
// with the read's error, and the cursor's field still reads. The steps run in order on one
// database, each on what the one before left.
func TestTheKeywordFillDeferralSaysWhetherEachTableHoldsARow(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	for _, step := range []struct {
		name       string
		statements []string
		want       string
	}{
		{"no row in either table", nil, keywordFillTablesEmpty},
		{"a row of the keyword table", []string{`
			insert into search_text_versions (kind, id, project_key, event_id, event_seq, started_at, body, body_from, search)
			values ('comment', 'a-comment', 'PROJ', 1, 1, now(), 'the signing keys rotate', 'event', to_tsvector('simple', 'the signing keys rotate'))`},
			"search_text_versions_rows=some search_text_versions_fill_rows=none"},
		{"the cursor's row alone", []string{
			`delete from search_text_versions`,
			`insert into search_text_versions_fill (id, event_id, rules) values (1, 0, 1)`},
			"search_text_versions_rows=none search_text_versions_fill_rows=some"},
		{"the keyword table dropped", []string{`drop table search_text_versions`},
			`search_text_versions_rows=unknown search_text_versions_rows_error=ERROR: relation "public.search_text_versions" does not exist (SQLSTATE 42P01) search_text_versions_fill_rows=some`},
	} {
		for _, statement := range step.statements {
			if _, err := database.Pool.Exec(ctx, statement); err != nil {
				t.Fatalf("%s: %s: %v", step.name, statement, err)
			}
		}
		if got := deferralFields(searchFillTableRows(ctx, database.Pool)); got != step.want {
			t.Errorf("%s: the deferral's fields are\n  %s\nwant\n  %s", step.name, got, step.want)
		}
	}
}

// A failed read of a table the keyword fill writes does not stop the boot. Booted with
// DISPATCH_SEARCH_FILL=off on a database whose keyword table was dropped and whose cursor holds its
// row, a process logs the deferral once, saying the keyword table is unknown with the read's error
// and the cursor holds a row, and then serves: it answers its health check and the registry of
// live server processes admits it.
func TestATableTheKeywordFillDeferralCannotReadDoesNotStopTheBoot(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	for _, statement := range []string{
		`insert into search_text_versions_fill (id, event_id, rules) values (1, 0, 1)`,
		`drop table search_text_versions`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	process := startDispatchProcessWith(t, databaseURLOf(t, database), dispatchOptions{Env: map[string]string{"DISPATCH_SEARCH_FILL": "off"}})
	process.waitHealthy(t)
	awaitRegistryAdmission(t, database, process)
	want := keywordFillDeferredLine + ` search_text_versions_rows=unknown search_text_versions_rows_error="ERROR: relation \"public.search_text_versions\" does not exist (SQLSTATE 42P01)" search_text_versions_fill_rows=some`
	if lines := process.logLines(keywordFillDeferredLine); len(lines) != 1 || !strings.Contains(lines[0], want) {
		t.Fatalf("the process logged the deferral %d times; want once, holding\n  %s\nin:\n%s", len(lines), want, process.Output.String())
	}
}

// A lock on a table the keyword fill writes ends the read at the server, and the boot goes on.
// Another connection holds ACCESS EXCLUSIVE on search_text_versions_fill in a transaction it keeps
// open for the whole test. searchFillTableRows then gives the cursor's table unknown with
// Postgres's own lock timeout (55P03) and the keyword table still none, and a read of the cursor's
// table alone ends with 55P03 before the client's deadline could end it; once it returned, no
// backend of the database waits for a lock on the cursor's table, though the lock is still held.
// A process booted with DISPATCH_SEARCH_FILL=off under the same lock logs the same fields once,
// then serves: it answers its health check and the registry of live server processes admits it,
// and still no backend waits for the lock.
func TestALockedKeywordFillTableEndsItsReadAtTheServerAndTheBootGoesOn(t *testing.T) {
	// Not parallel: it holds a step to a wall-clock bound load could break
	// (requireTheServerEndsTheRead's 10 s over a 5 s lock timeout).
	database := storetest.Open(t)
	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the transaction that holds the lock: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `lock table public.search_text_versions_fill in access exclusive mode`); err != nil {
		t.Fatalf("lock search_text_versions_fill: %v", err)
	}
	const fields = `search_text_versions_rows=none search_text_versions_fill_rows=unknown search_text_versions_fill_rows_error=ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)`

	if got := deferralFields(searchFillTableRows(ctx, database.Pool)); got != fields {
		t.Errorf("under the lock, the deferral's fields are\n  %s\nwant\n  %s", got, fields)
	}
	requireTheServerEndsTheRead(t, database, "search_text_versions_fill", "55P03")
	requireNoKeywordFillCursorLockWait(t, database, "after the reads returned")

	process := startDispatchProcessWith(t, databaseURLOf(t, database), dispatchOptions{Env: map[string]string{"DISPATCH_SEARCH_FILL": "off"}})
	process.waitHealthy(t)
	awaitRegistryAdmission(t, database, process)
	want := keywordFillDeferredLine + ` search_text_versions_rows=none search_text_versions_fill_rows=unknown search_text_versions_fill_rows_error="ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)"`
	if lines := process.logLines(keywordFillDeferredLine); len(lines) != 1 || !strings.Contains(lines[0], want) {
		t.Fatalf("the process logged the deferral %d times; want once, holding\n  %s\nin:\n%s", len(lines), want, process.Output.String())
	}
	requireNoKeywordFillCursorLockWait(t, database, "after the booted process logged the deferral")
}

// A read of a table the keyword fill writes that runs on with no lock to wait for is ended by
// Postgres's statement timeout, and the boot goes on. search_text_versions_fill is replaced with a
// view whose select sleeps 30 s, past the client's deadline. searchFillTableRows then gives that
// table unknown with Postgres's own statement timeout (57014) and the keyword table still none,
// and a read of that table alone ends with 57014 before the client's deadline could end it. A
// process booted with DISPATCH_SEARCH_FILL=off on that database logs the same fields once, then
// serves: it answers its health check and the registry of live server processes admits it.
func TestASlowKeywordFillTableReadEndsAtTheServerAndTheBootGoesOn(t *testing.T) {
	// Not parallel: it holds a step to a wall-clock bound load could break
	// (requireTheServerEndsTheRead's 10 s over a 6 s statement timeout).
	database := storetest.Open(t)
	ctx := context.Background()
	for _, statement := range []string{
		`drop table public.search_text_versions_fill`,
		`create view public.search_text_versions_fill as select 1 as id from pg_sleep(30)`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	const fields = `search_text_versions_rows=none search_text_versions_fill_rows=unknown search_text_versions_fill_rows_error=ERROR: canceling statement due to statement timeout (SQLSTATE 57014)`
	if got := deferralFields(searchFillTableRows(ctx, database.Pool)); got != fields {
		t.Errorf("with a read that runs on, the deferral's fields are\n  %s\nwant\n  %s", got, fields)
	}
	requireTheServerEndsTheRead(t, database, "search_text_versions_fill", "57014")

	process := startDispatchProcessWith(t, databaseURLOf(t, database), dispatchOptions{Env: map[string]string{"DISPATCH_SEARCH_FILL": "off"}})
	process.waitHealthy(t)
	awaitRegistryAdmission(t, database, process)
	want := keywordFillDeferredLine + ` search_text_versions_rows=none search_text_versions_fill_rows=unknown search_text_versions_fill_rows_error="ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"`
	if lines := process.logLines(keywordFillDeferredLine); len(lines) != 1 || !strings.Contains(lines[0], want) {
		t.Fatalf("the process logged the deferral %d times; want once, holding\n  %s\nin:\n%s", len(lines), want, process.Output.String())
	}
}

// requireTheServerEndsTheRead reads table alone, as searchFillTableRows reads each table, and fails
// the test unless Postgres itself ended the read: an error of the SQLSTATE code, before
// searchFillTableReadTimeout, the client's own deadline, could end it. It judges the server's end
// of the read by its error, not by a time that a slow box or a slow dial would stretch.
func requireTheServerEndsTheRead(t *testing.T, database *store.Store, table, code string) {
	t.Helper()
	started := time.Now()
	_, err := searchFillTableHoldsRow(context.Background(), database.Pool, table)
	took := time.Since(started)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("the read of %s ended after %v with %v; want Postgres's own error %s", table, took.Round(time.Millisecond), err, code)
	}
	if took >= searchFillTableReadTimeout {
		t.Errorf("the read of %s took %v; want less than the client's deadline, %v", table, took.Round(time.Millisecond), searchFillTableReadTimeout)
	}
}

// requireNoKeywordFillCursorLockWait fails the test when a backend of the database waits for a
// lock on search_text_versions_fill. when says at which moment the test reads it.
func requireNoKeywordFillCursorLockWait(t *testing.T, database *store.Store, when string) {
	t.Helper()
	var waiting int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from pg_locks
		 where database = (select oid from pg_database where datname = current_database())
		   and relation = 'public.search_text_versions_fill'::regclass
		   and not granted`).Scan(&waiting); err != nil {
		t.Fatalf("read the lock waits on search_text_versions_fill: %v", err)
	}
	if waiting != 0 {
		t.Fatalf("%s, %d backends still wait for a lock on search_text_versions_fill; want none", when, waiting)
	}
}

// deferralFields writes the fields searchFillTableRows answers as `key=value` pairs, one space
// between, in their order.
func deferralFields(fields []any) string {
	pairs := make([]string, 0, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		pairs = append(pairs, fmt.Sprintf("%v=%v", fields[i], fields[i+1]))
	}
	return strings.Join(pairs, " ")
}

// awaitRegistryAdmission waits up to 30 s for the registry of live server processes to hold a row,
// which on a database no other process serves is the process's own: once it is admitted, the
// process serves, and a keyword fill it started takes its hold.
func awaitRegistryAdmission(t *testing.T, database *store.Store, process *dispatchProcess) {
	t.Helper()
	if !testwait.Holds(30*time.Second, 50*time.Millisecond, func() bool {
		var rows int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from server_processes`).Scan(&rows); err != nil {
			t.Fatalf("read the registry of live server processes: %v", err)
		}
		return rows > 0
	}) {
		t.Fatalf("the registry of live server processes admitted no process within 30 s:\n%s", process.Output.String())
	}
}
