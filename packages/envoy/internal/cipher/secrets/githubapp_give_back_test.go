package secrets_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestGiveBackRevokesEveryTokenOfAReleasedValue: GiveBack is what the broker calls for a github-app
// value a release minted and then hands to nobody, because a later name of the grant failed. It
// sends GitHub the revoke of each installation's token in the value, under a bound of its own that
// the caller's context does not end, and logs at WARN each revoke GitHub does not confirm with the
// secret's name and never the token. Nobody waits on a give-back, so each revoke is made once and
// never again after a failure: a retry would draw on the budget the live mints retry from. A value
// handed as the installations form that is no such object holds nothing it can name.
func TestGiveBackRevokesEveryTokenOfAReleasedValue(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprintf("GitHub refusing the revokes: %t", refused), func(t *testing.T) {
			var logged bytes.Buffer
			logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
			github := newBreakingGitHub(t, false)
			retry, _, _ := testRetry(atTheBottom)
			app := secrets.GitHubApp{Base: github.service.URL(), HTTP: github.client, Retry: retry}
			minted, err := app.Mint(t.Context(), secrets.MintRequest{Value: githubAppValue(t, "123456", key), GitHub: readScope("acme-org", "ada")})
			if err != nil {
				t.Fatalf("the release: %v", err)
			}
			tokens := github.tokens()
			if live := github.live(t, tokens); len(tokens) != 2 || len(live) != 2 {
				t.Fatalf("the release minted %d tokens, %d live; want two live ones, one per account", len(tokens), len(live))
			}
			github.mu.Lock()
			github.refusing = refused
			github.mu.Unlock()
			before := github.revoked()

			ended, cancel := context.WithCancel(t.Context())
			cancel()
			app.GiveBack(ended, "APP_KEY", minted.Value, githubname.ValueInstallations)
			app.GiveBack(ended, "APP_KEY", "not a value Mint answers", githubname.ValueInstallations)

			if sent := github.revoked() - before; sent != len(tokens) {
				t.Fatalf("GiveBack sent GitHub %d revokes for %d tokens; want one each, since a give-back is not retried", sent, len(tokens))
			}
			want := 0
			if refused {
				want = 2
			}
			if live := github.live(t, tokens); len(live) != want {
				t.Fatalf("%d of the release's tokens are live after GiveBack under an ended context, want %d", len(live), want)
			}
			if lines, warnings := warned(logged.String(), saysNotRevokedUnreleased); lines != want || warnings != want || (want > 0 && !strings.Contains(logged.String(), "name=APP_KEY")) {
				t.Fatalf("log = %q; want %d lines saying %q at WARN, naming APP_KEY: %d lines say it, %d of them at WARN", logged.String(), want, saysNotRevokedUnreleased, lines, warnings)
			}
			for _, token := range tokens {
				if strings.Contains(logged.String(), token) {
					t.Fatalf("the log holds a token: %q", logged.String())
				}
			}
		})
	}
}

// TestGiveBackRevokesTheBareTokenOfAValueTokenProfile: a value-token profile's release is the one
// account's token itself, not an object of installations, and GiveBack revokes it as that token.
// It reads the value as the form its caller hands it says (the profile's github.value), never as
// the value's text looks: the same bare token handed as the installations form names no
// installation, so nothing is revoked, and handed as the token form it is revoked.
func TestGiveBackRevokesTheBareTokenOfAValueTokenProfile(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	github := newBreakingGitHub(t, false)
	retry, _, _ := testRetry(atTheBottom)
	app := secrets.GitHubApp{Base: github.service.URL(), HTTP: github.client, Retry: retry}
	scope := readScope("ada")
	scope.Value = githubname.ValueToken
	minted, err := app.Mint(t.Context(), secrets.MintRequest{Value: githubAppValue(t, "123456", key), GitHub: scope})
	if err != nil {
		t.Fatalf("the release: %v", err)
	}
	tokens := github.tokens()
	if len(tokens) != 1 || minted.Value != tokens[0] {
		t.Fatalf("the release handed over %d bytes and minted %d tokens; want the one token itself", len(minted.Value), len(tokens))
	}
	before := github.revoked()
	app.GiveBack(t.Context(), "APP_TOKEN", minted.Value, githubname.ValueInstallations)
	if sent := github.revoked() - before; sent != 0 {
		t.Fatalf("GiveBack of a bare token handed as the installations form sent GitHub %d revokes, want none: the form says the value is JSON of installations, and it is not", sent)
	}
	if live := github.live(t, tokens); len(live) != 1 {
		t.Fatalf("the bare token is not live after a give-back that named no installation")
	}
	app.GiveBack(t.Context(), "APP_TOKEN", minted.Value, githubname.ValueToken)
	if sent := github.revoked() - before; sent != 1 {
		t.Fatalf("GiveBack sent GitHub %d revokes for a bare token handed as the token form, want one", sent)
	}
	if live := github.live(t, tokens); len(live) != 0 {
		t.Fatalf("the bare token is live after GiveBack")
	}
}
