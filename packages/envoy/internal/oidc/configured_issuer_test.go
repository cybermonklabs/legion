package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The rows of what the code flow, the client check and the verifier print of an issuer someone
// configured: it prints only as redact.URL prints it, in every error and log line its discovery,
// its client check and a refusal of another issuer's token write. Each row captures the error or
// the log line the production code returns or writes. The rows of the issuer's own words are in
// issuer_words_test.go, which shares the helpers below.

// probeParam is the parameter OAuth carries an access token under, in two parts as redacttest
// writes it, so no line of this file reads as a credential to a scanner.
const probeParam = "access" + "_token"

// credentialed is base, the http URL a test server answers at, with the probe's user information
// in it: an issuer someone configured with a credential whose discovery the server still answers,
// since net/http sends the user information as a Basic authorization and the document can name
// the issuer exactly as configured.
func credentialed(base string) string {
	return strings.Replace(base, "://", "://"+redacttest.User+":"+redacttest.Password+"@", 1)
}

// probed is credentialed(base) with a path, and the probe's access token in its query and its
// fragment: the whole probe, for an issuer whose discovery fails whatever it names.
func probed(base string) string {
	return credentialed(base) + "/path?" + probeParam + "=" + redacttest.Token + "#" + probeParam + "=" + redacttest.Token
}

// refusingServer answers every request 500, an issuer whose discovery document cannot be read.
func refusingServer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "discovery unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// silentServer answers no request until the test ends: an issuer whose discovery read is still in
// flight when its caller stops waiting.
func silentServer(t *testing.T) string {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	// Cleanups run last first, so the held requests end before Close waits for them.
	t.Cleanup(func() { close(release) })
	return server.URL
}

// documentServer serves, whatever the path, the discovery document document builds from the
// server's own URL: one no oidctest issuer serves, naming an issuer with user information, a
// revocation_endpoint that does not parse, or a member of a shape discovery refuses. The URL is
// read from the request's Host, so the handler shares no variable with the test.
func documentServer(t *testing.T, document func(base string) map[string]any) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(document("http://" + r.Host))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// signInDocument is a discovery document for issuer whose endpoints are under base: what
// discoverCodeFlow and NewKeys read, before a row changes one member.
func signInDocument(issuer, base string) map[string]any {
	return map[string]any{
		"issuer":                                issuer,
		"authorization_endpoint":                base + "/authorize",
		"token_endpoint":                        base + "/token",
		"jwks_uri":                              base + "/keys",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
}

// signIn runs one sign-in through flow as personClaims names a member, and returns what its
// exchange returns.
func signIn(t *testing.T, issuer *oidctest.Issuer, flow *CodeFlow) (Session, error) {
	t.Helper()
	issuer.SignInAs(personClaims("dispatch-members"))
	code, _ := issuer.Authorize(t, authURL(t, flow, "state-1", "nonce-1"))
	return flow.Exchange(context.Background(), flowRedirect, code, "nonce-1")
}

// requireRefused fails t unless err is an error that says says, and captures its text and its %+v
// under capture, each of which must hold none of the probe's credentials.
func requireRefused(t *testing.T, capture string, err error, says string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), says) {
		t.Fatalf("the error = %v, want one that says %q", err, says)
	}
	redacttest.RequireNone(t, capture+"/error", err.Error())
	redacttest.RequireNone(t, capture+"/plus-v", fmt.Sprintf("%+v", err))
}

// TestADiscoveryErrorPrintsNoCredentialOfTheConfiguredIssuer: a code flow configured with an issuer
// that carries a credential names the issuer, in each error its discovery returns, only as
// redact.URL prints it: a document that cannot be read (unread, with the whole probe), one that
// names no authorization or token endpoint (no-sign-in), and one whose revocation_endpoint is null
// (metadata). Captures oidc.configured-issuer-discovery/<row>.
func TestADiscoveryErrorPrintsNoCredentialOfTheConfiguredIssuer(t *testing.T) {
	t.Run("unread", func(t *testing.T) {
		err := NewCodeFlow(probed(refusingServer(t)), flowClientID, flowClientSecret, "").Discover(context.Background())
		requireRefused(t, "oidc.configured-issuer-discovery/unread", err, "discovering issuer")
	})
	t.Run("no-sign-in", func(t *testing.T) {
		issuer := oidctest.New(t)
		configured := credentialed(issuer.URL())
		issuer.AdvertiseIssuer(configured)
		issuer.OmitCodeFlowEndpoints()
		err := NewCodeFlow(configured, flowClientID, flowClientSecret, "").Discover(context.Background())
		requireRefused(t, "oidc.configured-issuer-discovery/no-sign-in", err, "so it offers no sign-in")
	})
	t.Run("metadata", func(t *testing.T) {
		base := documentServer(t, func(base string) map[string]any {
			document := signInDocument(credentialed(base), base)
			document["revocation_endpoint"] = nil
			return document
		})
		err := NewCodeFlow(credentialed(base), flowClientID, flowClientSecret, "").Discover(context.Background())
		requireRefused(t, "oidc.configured-issuer-discovery/metadata", err, "'s revocation_endpoint and token_endpoint_auth_methods_supported")
	})
}

// TestACheckClientsErrorPrintsNoCredentialOfTheConfiguredIssuer: a client check of an issuer
// configured with a credential names the issuer only as redact.URL prints it, in each of its
// errors: a token endpoint that answers nothing (unanswered), one that refuses the client
// (refused), one that cannot answer (outage), and one that answers an error with no refusal's
// status (no-refusal). Captures oidc.configured-issuer-client-check/<row>.
func TestACheckClientsErrorPrintsNoCredentialOfTheConfiguredIssuer(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), checkClientID, checkClientSecret)
	configured := credentialed(issuer.URL())
	issuer.AdvertiseIssuer(configured)
	for _, row := range []struct {
		name string
		fail func()
		want ClientCheck
		says string
	}{
		{"unanswered", issuer.DropToken, ClientCheckIssuerUnreachable, "check the client at"},
		{"refused", func() { issuer.FailToken(http.StatusBadRequest, "invalid_client") }, ClientCheckRejected, "refused the client"},
		{"outage", func() { issuer.FailToken(http.StatusServiceUnavailable, "") }, ClientCheckIssuerUnreachable, "could not answer the client check"},
		{"no-refusal", func() { issuer.FailToken(http.StatusOK, "invalid_grant") }, ClientCheckIssuerUnreachable, "and no refusal"},
	} {
		t.Run(row.name, func(t *testing.T) {
			row.fail()
			t.Cleanup(func() { issuer.FailToken(0, "") })
			check, err := CheckClient(context.Background(), configured, checkClientID, checkClientSecret)
			if check != row.want {
				t.Fatalf("CheckClient = %q, want %q", check, row.want)
			}
			requireRefused(t, "oidc.configured-issuer-client-check/"+row.name, err, row.says)
		})
	}
}

// TestAnIssuerRefusalPrintsNoCredentialOfTheConfiguredIssuer: a token from another issuer is
// refused naming the issuer the verifier was configured with only as redact.URL prints it, for a
// verifier over a key set (verify) and for the code flow's exchange (exchange). Captures
// oidc.configured-issuer-verifier/<row>.
func TestAnIssuerRefusalPrintsNoCredentialOfTheConfiguredIssuer(t *testing.T) {
	const other = "https://issuer.example/other"
	t.Run("verify", func(t *testing.T) {
		issuer := oidctest.New(t)
		signing := issuer.PublishKey(t, "signing-key")
		verifier := NewWithKeys(credentialed(issuer.URL()), testAudience, NewKeysAt(issuer.URL()+"/keys"))
		claims := issuer.Claims(testSubject, testAudience)
		claims["iss"] = other
		_, err := verifier.Verify(context.Background(), issuer.Mint(t, signing, claims))
		if !errors.Is(err, ErrIssuer) {
			t.Fatalf("Verify = %v, want %v", err, ErrIssuer)
		}
		requireRefused(t, "oidc.configured-issuer-verifier/verify", err, other)
	})
	t.Run("exchange", func(t *testing.T) {
		issuer := oidctest.New(t)
		issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), flowClientID, flowClientSecret, flowRedirect)
		configured := credentialed(issuer.URL())
		issuer.AdvertiseIssuer(configured)
		issuer.SpoilIDTokens(func(claims map[string]any) { claims["iss"] = other })
		_, err := signIn(t, issuer, NewCodeFlow(configured, flowClientID, flowClientSecret, ""))
		if !errors.Is(err, ErrIssuer) {
			t.Fatalf("Exchange = %v, want %v", err, ErrIssuer)
		}
		requireRefused(t, "oidc.configured-issuer-verifier/exchange", err, other)
	})
}

// TestAKeySetDiscoveryErrorPrintsNoCredentialOfTheConfiguredIssuer: a verifier configured with an
// issuer that carries a credential names the issuer, in each error its discovery returns (New,
// NewKeys), only as redact.URL prints it: a document that cannot be read (unread, with the whole
// probe) and one that names no jwks_uri (no-jwks-uri). Captures
// oidc.configured-issuer-keys/<row>.
func TestAKeySetDiscoveryErrorPrintsNoCredentialOfTheConfiguredIssuer(t *testing.T) {
	t.Run("unread", func(t *testing.T) {
		_, err := New(context.Background(), probed(refusingServer(t)), testAudience)
		requireRefused(t, "oidc.configured-issuer-keys/unread", err, "discovering issuer")
	})
	t.Run("no-jwks-uri", func(t *testing.T) {
		base := documentServer(t, func(base string) map[string]any {
			document := signInDocument(credentialed(base), base)
			delete(document, "jwks_uri")
			return document
		})
		_, err := New(context.Background(), credentialed(base), testAudience)
		requireRefused(t, "oidc.configured-issuer-keys/no-jwks-uri", err, "names no jwks_uri")
	})
}

// stopAtFailure is a log handler that ends the context of a discovery's background loop once it
// has logged the loop's failure line, so Run returns right after the line a row reads.
type stopAtFailure struct {
	slog.Handler
	stop context.CancelFunc
}

func (h stopAtFailure) Handle(ctx context.Context, record slog.Record) error {
	err := h.Handler.Handle(ctx, record)
	if strings.HasPrefix(record.Message, "oidc: discover the issuer;") {
		h.stop()
	}
	return err
}

// TestTheCodeFlowsDiscoveryPrintsNoCredentialOfTheConfiguredIssuer: a code flow configured with an
// issuer that carries a credential names the issuer only as redact.URL prints it in Discover's
// error for a caller that stopped waiting (waiting), and in Run's log lines, the one for a
// discovered issuer (discovered) and the one for a failed discovery (failed). Captures
// oidc.configured-issuer-discovery-wait and oidc.configured-issuer-discovery-log/<row>.
func TestTheCodeFlowsDiscoveryPrintsNoCredentialOfTheConfiguredIssuer(t *testing.T) {
	t.Run("waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := NewCodeFlow(probed(silentServer(t)), flowClientID, flowClientSecret, "").Discover(ctx)
		requireRefused(t, "oidc.configured-issuer-discovery-wait", err, "waiting for issuer")
	})
	t.Run("discovered", func(t *testing.T) {
		issuer := oidctest.New(t)
		configured := credentialed(issuer.URL())
		issuer.AdvertiseIssuer(configured)
		var logged logtest.Buffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		NewCodeFlow(configured, flowClientID, flowClientSecret, "").Run(context.Background())
		line := logged.Lines("oidc: discovered the issuer")
		if line == "" {
			t.Fatalf("Run logged no discovered line: %q", logged.String())
		}
		redacttest.RequireNone(t, "oidc.configured-issuer-discovery-log/discovered", line)
	})
	t.Run("failed", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var logged logtest.Buffer
		logtest.SetDefault(t, stopAtFailure{slog.NewTextHandler(&logged, nil), cancel})
		NewCodeFlow(probed(refusingServer(t)), flowClientID, flowClientSecret, "").Run(ctx)
		line := logged.Lines("oidc: discover the issuer;")
		if line == "" {
			t.Fatalf("Run logged no failure line: %q", logged.String())
		}
		redacttest.RequireNone(t, "oidc.configured-issuer-discovery-log/failed", line)
	})
}
