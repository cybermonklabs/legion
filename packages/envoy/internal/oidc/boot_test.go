package oidc

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// TestDiscoverRefusesAnIssuerThatNeverAnswers is the case a 404 issuer does not
// cover: a partition or a black-holing proxy accepts the connection and then
// says nothing. Without a deadline the boot hangs there forever instead of
// refusing, and a hung boot never binds a port, so nothing reports it.
func TestDiscoverRefusesAnIssuerThatNeverAnswers(t *testing.T) {
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
	// the instant before Close and still mid-send, and Discover issues exactly
	// one GET, buffered long before the deadline; net/http does not redial
	// after a cancellation.
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
	start := time.Now()
	verifier, err := Discover(context.Background(), issuer, "dispatch", 300*time.Millisecond, 0)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Discover against a silent issuer returned verifier %#v and no error", verifier)
	}
	if !strings.Contains(err.Error(), issuer) {
		t.Errorf("Discover error %q does not name the issuer %s", err, issuer)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Discover took %s to give up, want the deadline to bound it", elapsed)
	}
}

// TestDiscoverIsNilWhenNoIssuerIsConfigured pins the "unset means no verifier"
// half of the contract, which is what keeps both binaries byte-identical to
// their pre-OIDC behaviour.
func TestDiscoverIsNilWhenNoIssuerIsConfigured(t *testing.T) {
	verifier, err := Discover(context.Background(), "", "", time.Second, 0)
	if err != nil || verifier != nil {
		t.Fatalf("Discover(\"\") = %#v, %v; want nil, nil", verifier, err)
	}
}

// Discover is how the Envoy listener builds its /v1 verifier at boot (cmd/listener), so the
// key-set wait it is given bounds every Verify that waits on a read: a key set slower than the
// wait is answered ErrIssuerUnavailable once the wait has passed, the read going on behind the
// answer for the next caller, where a verifier with no wait would sit the read out.
func TestDiscoverBoundsAVerifysKeySetWaitByItsArgument(t *testing.T) {
	issuer := oidctest.New(t)
	signing := issuer.PublishKey(t, "boot-key")
	const keySetWait, slowRead = 200 * time.Millisecond, 1500 * time.Millisecond
	verifier, err := Discover(context.Background(), issuer.URL(), "dispatch", time.Second, keySetWait)
	if err != nil {
		t.Fatalf("Discover(%q): %v", issuer.URL(), err)
	}
	token := issuer.Mint(t, signing, issuer.Claims("system:serviceaccount:example-ns:example-app", "dispatch"))
	issuer.DelayKeys(slowRead)
	start := time.Now()
	_, err = verifier.Verify(context.Background(), token)
	if elapsed := time.Since(start); !errors.Is(err, ErrIssuerUnavailable) || elapsed >= slowRead/2 {
		t.Fatalf("Verify against a key set that answers in %s = %v after %s, want ErrIssuerUnavailable once the %s wait Discover was given has passed",
			slowRead, err, elapsed.Round(time.Millisecond), keySetWait)
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
