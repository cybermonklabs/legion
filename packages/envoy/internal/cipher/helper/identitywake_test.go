// packages/envoy/internal/cipher/helper/identitywake_test.go
//go:build linux

// What a 401 LAUNCHER_INVALID leaves a helper that proves an identity of its own.
//
// In machine-login mode a dropped credential is handed to the refresh loop, which settles it at
// once. In identity mode there is no refresh loop: the trade loop (cipherclient.Client.Run)
// holds the credential and sleeps until the token in hand is nearly spent. A drop that does not
// wake it leaves the helper answering NO_CREDENTIAL to every register and sign for the rest of
// that sleep — ten minutes at a 900 s token, fifty-five at an hour's.
//
// Like enrolauthority_test.go's, these drive Broker's launcher calls against an httptest server
// directly: no unix socket and no pidfd.

package helper

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipherclient"
)

// serveTokenExchange adds the broker's token endpoint to f, minting a launcher credential of
// its own for each trade, for the machine key the helper proves. The credential of an earlier
// trade stays in f.credentials, so a proof signed with it still verifies and only the broker's
// scripted refusal refuses it. It answers how many trades have been made.
func serveTokenExchange(t *testing.T, f *enrolBroker, mux *http.ServeMux, thumbprint string) func() int {
	t.Helper()
	answered := serveTimedTokenExchange(t, f, mux, thumbprint)
	return func() int { return len(answered()) }
}

// serveTimedTokenExchange is serveTokenExchange answering when each trade was answered, oldest
// first, for a test whose failure says how long the helper's start waited for its first one.
func serveTimedTokenExchange(t *testing.T, f *enrolBroker, mux *http.ServeMux, thumbprint string) func() []time.Time {
	t.Helper()
	var mu sync.Mutex
	var answered []time.Time
	mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		id := "credential-of-trade-" + strconv.Itoa(len(answered)+1)
		answered = append(answered, time.Now())
		mu.Unlock()
		f.mu.Lock()
		f.credentials[id] = thumbprint
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"acc" + "ess_token": "access-token", "token_type": "DPoP", "expires_in": 3600,
			"credential_id": id, "credential_expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	return func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(answered)
	}
}

// TestADroppedCredentialInIdentityModeIsTradedAgainAtOnce: the broker refuses the enrolment's
// launcher proof, the helper throws that credential away, and the trade loop — which would
// otherwise sleep out the hour its token lives — trades again straight away, so the next
// register finds a credential rather than NO_CREDENTIAL.
func TestADroppedCredentialInIdentityModeIsTradedAgainAtOnce(t *testing.T) {
	t.Parallel()
	f := newEnrolBroker(t)
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	trades := serveTimedTokenExchange(t, f, mux, thumbprint)
	// The enrolment routes stay f's own; this mux only adds the token endpoint in front of them.
	mux.Handle("/", f.srv.Config.Handler)
	f.srv.Config.Handler = mux

	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("projected.service.account.token"), 0o600); err != nil {
		t.Fatal(err)
	}
	// machine, not identity: identity is the type one reading of a session's broker identity
	// has (Session.identity), which this test's enrol below names.
	machine, err := cipherclient.New(cipherclient.Config{
		BrokerURL: cipheraddr.New(f.srv.URL), Key: key, HTTP: f.srv.Client(),
		Subject: cipherclient.NewProjectedToken(tokenFile),
	})
	if err != nil {
		t.Fatal(err)
	}
	b := &Broker{
		URL: cipheraddr.New(f.srv.URL), Identity: machine, HTTP: f.srv.Client(),
		OperatorFile: filepath.Join(t.TempDir(), "no-operator-here"),
	}
	t.Cleanup(b.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	began := time.Now()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	started := time.Since(began)
	if !b.HasCredential() {
		t.Fatalf("the helper listens with no credential, so this test proves nothing about dropping one: Start returned %v after it began, against the %v it waits for the first trade (defaultStartRefreshBound); the trades the token endpoint answered, counted from Start's beginning: %s",
			started.Round(time.Millisecond), defaultStartRefreshBound, sinceBegan(began, trades()))
	}
	first := len(trades())

	f.mu.Lock()
	f.enrolRefusals, f.enrolStatus, f.enrolCode = 1, http.StatusUnauthorized, "LAUNCHER_INVALID"
	f.mu.Unlock()
	// A Session's identity fields are its own to replace (Session.rename), so a caller that
	// holds no session enrols one reading of an identity instead (Broker.EnrollIdentity).
	if _, _, err := b.EnrollIdentity(ctx, identity{RuntimeID: "this-box", Thumbprint: sessionThumbprint(t)}); err == nil {
		t.Fatal("the refused enrolment answered no error")
	}

	waitForAccounted(t, "the helper holding a credential again after the drop", b.HasCredential, func() string {
		return fmt.Sprintf("it traded %d times, want the drop to wake the trade loop rather than leave every register answering NO_CREDENTIAL until the token's own renewal", len(trades())-first)
	})
	if again := len(trades()) - first; again != 1 {
		t.Fatalf("the trade loop traded %d times after the drop, want exactly one", again)
	}
}

// sessionThumbprint is the thumbprint of a key a session would hold, which an enrolment names.
func sessionThumbprint(t *testing.T) string {
	t.Helper()
	key, err := proof.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return thumbprint
}
