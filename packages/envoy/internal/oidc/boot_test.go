package oidc

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// TestALazyVerifierAnswersPastAnIssuerThatNeverAnswers is the case a 404 issuer does not cover: a
// partition or a black-holing proxy accepts the connection and then says nothing. A request that
// needs the issuer must not wait on it past its own deadline, so a verifier asked under a short
// deadline answers ErrIssuerUnavailable, naming the issuer, once that deadline ends; the discovery
// attempt itself runs on under its own bound (discoveryAttemptTimeout).
func TestALazyVerifierAnswersPastAnIssuerThatNeverAnswers(t *testing.T) {
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Cleanups run LIFO, so registering the listener's Close second runs it
	// first, which is the order that lets the accept goroutine finish: Close
	// makes the blocked Accept return an error, and that path never touches the
	// channel. Nothing here synchronizes the goroutine's exit — there is no
	// WaitGroup — so this narrows the window for a send on a closed channel
	// rather than closing it. Reaching it would need a connection accepted in
	// the instant before Close and still mid-send. The discovery attempt outlives
	// this test's 300 ms wait, since it runs detached from its caller under
	// discoveryAttemptTimeout, but it dials once: its one GET is accepted and
	// then waits, and net/http does not redial a connection it holds.
	accepted := make(chan net.Conn, 4)
	t.Cleanup(func() {
		close(accepted)
		for conn := range accepted {
			_ = conn.Close()
		}
	})
	t.Cleanup(func() { _ = silent.Close() })
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			accepted <- conn // held open, never written to
		}
	}()

	issuer := "http://" + silent.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	claims, err := NewLazyVerifier(issuer, "dispatch").Verify(ctx, "any-token")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrIssuerUnavailable) {
		t.Fatalf("Verify against a silent issuer = %#v, %v; want an error matching ErrIssuerUnavailable", claims, err)
	}
	if !strings.Contains(err.Error(), issuer) {
		t.Errorf("Verify's error %q does not name the issuer %s", err, issuer)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Verify took %s to give up, want the caller's deadline to bound it", elapsed)
	}
}

// TestConfigFromEnvHoldsBothOrNeither: the one rule every binary shares. The refusal names the
// variable the operator has to set, because the one they did set is not the one that is missing.
func TestConfigFromEnvHoldsBothOrNeither(t *testing.T) {
	const issuerVar, audienceVar = "X_OIDC_ISSUER", "X_OIDC_AUDIENCE"
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}

	issuer, audience, err := ConfigFromEnv(env(nil), issuerVar, audienceVar)
	if err != nil || issuer != "" || audience != "" {
		t.Fatalf("neither set: %q, %q, %v; want empty, empty, nil", issuer, audience, err)
	}

	_, _, err = ConfigFromEnv(env(map[string]string{issuerVar: "https://oidc.example"}), issuerVar, audienceVar)
	if err == nil || !strings.Contains(err.Error(), audienceVar) {
		t.Fatalf("issuer alone: err = %v, want one naming %s", err, audienceVar)
	}

	_, _, err = ConfigFromEnv(env(map[string]string{audienceVar: "dispatch"}), issuerVar, audienceVar)
	if err == nil || !strings.Contains(err.Error(), issuerVar) {
		t.Fatalf("audience alone: err = %v, want one naming %s", err, issuerVar)
	}

	issuer, audience, err = ConfigFromEnv(env(map[string]string{
		issuerVar: "  https://oidc.example  ", audienceVar: " dispatch ",
	}), issuerVar, audienceVar)
	if err != nil || issuer != "https://oidc.example" || audience != "dispatch" {
		t.Fatalf("both set: %q, %q, %v; want the trimmed pair", issuer, audience, err)
	}
}

// An issuer setting whose value holds a credential is refused naming the setting alone: a user
// name and a password, a token holding a "/" or a "?" written as the user (its "@" past the host),
// a value that does not parse, and a token in the query or the fragment, an empty one included,
// which no issuer identifier has (OpenID Connect Discovery §3). ConfigFromEnv refuses each the
// same way. The credentials are made up.
func TestAnIssuerSettingHoldingACredentialIsRefusedByItsName(t *testing.T) {
	const issuerVar, audienceVar = "X_OIDC_ISSUER", "X_OIDC_AUDIENCE"
	const user, secret = "madeUpIssuerUser0123", "madeUpIssuerSecret0123"
	userinfo := user + ":" + secret + "@"
	queryOrFragment := issuerVar + " cannot be read: it holds a query or a fragment, which no issuer identifier has"
	for _, tc := range []struct{ name, issuer, want string }{
		{"a user name and a password", "https://" + userinfo + "oidc.example", issuerVar + " cannot be read: it holds a user name or a password, which no issuer URL has"},
		{"a token holding a / as the user", "https://" + user + "/" + secret + "@oidc.example", issuerVar + ` cannot be read: an "@" stands after its host, as it does where a credential in its userinfo holds a "/", "?" or "#"; write such a credential percent-encoded`},
		{"a token holding a ? as the user", "https://" + user + "?" + secret + "@oidc.example", issuerVar + ` cannot be read: an "@" stands after its host, as it does where a credential in its userinfo holds a "/", "?" or "#"; write such a credential percent-encoded`},
		{"a value that does not parse", "https://" + user + ":" + secret + "/x@oidc.example", issuerVar + " does not parse as a URL"},
		{"a token in the query", "https://oidc.example/?token=" + secret, queryOrFragment},
		{"a token in the fragment", "https://oidc.example/#" + secret, queryOrFragment},
		{"an empty query", "https://oidc.example/?", queryOrFragment},
		{"an empty fragment", "https://oidc.example/#", queryOrFragment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := IssuerRefusal(issuerVar, tc.issuer); err == nil || err.Error() != tc.want {
				t.Fatalf("IssuerRefusal = %v, want %q", err, tc.want)
			}
			_, _, err := ConfigFromEnv(func(key string) string {
				return map[string]string{issuerVar: tc.issuer, audienceVar: "dispatch"}[key]
			}, issuerVar, audienceVar)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("ConfigFromEnv = %v, want %q", err, tc.want)
			}
		})
	}
	if err := IssuerRefusal(issuerVar, "https://oidc.example/id/CLUSTER"); err != nil {
		t.Fatalf("IssuerRefusal(a plain issuer) = %v, want nil", err)
	}
}
