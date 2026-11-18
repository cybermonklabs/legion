package secrets_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestGiveBackRevokesEveryTokenOfAReleasedValue: GiveBack is what the broker calls for a github-app
// value a release minted and then hands to nobody, because a later name of the grant failed. It
// sends GitHub the revoke of each installation's token in the value, under a bound of its own that
// the caller's context does not end, and logs at WARN each revoke GitHub does not confirm with the
// secret's name and never the token. Nobody waits on a give-back, so each revoke is made once and
// never again after a failure: a retry would draw on the budget the live mints retry from. A value
// that is no such object holds nothing it can name.
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
			minted, err := app.Mint(t.Context(), secrets.MintRequest{Value: githubAppValue(t, "123456", key), GitHubOwners: []string{"acme-org", "ada"}})
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
			app.GiveBack(ended, "APP_KEY", minted.Value)
			app.GiveBack(ended, "APP_KEY", "not a value Mint answers")

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
