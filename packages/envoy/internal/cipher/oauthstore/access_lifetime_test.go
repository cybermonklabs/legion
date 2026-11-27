package oauthstore_test

import (
	"testing"
	"time"

	"authelia.com/provider/oauth2"

	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
)

// TestARestrictedTokensExpiryIsHeldToItsDeadline: an exchange session that decided a lifetime
// (AccessLifetime, a restricted member's machine login's) holds its access token's expiry to the
// deadline StartAccessLifetime set from the project check's return, whatever later expiry the
// library asks, and keeps an earlier one it asks. One that decided a lifetime and never started it
// expires at once, so a trade that skipped StartAccessLifetime mints a token that lives no time,
// which every row that reads a restricted token's expires_in catches, rather than one living the
// lifetime from its mint. A session that decided no lifetime takes the expiry asked, and no
// refresh token's expiry is touched.
//
// Control: SetExpiresAt counting a lifetime with no deadline from now, as the head before this row
// did, and the never-started row fails naming the expiry past now.
func TestARestrictedTokensExpiryIsHeldToItsDeadline(t *testing.T) {
	const lifetime = 270 * time.Second
	checked := time.Now().UTC().Add(-10 * time.Second)
	asked := time.Now().UTC().Add(time.Hour)

	started := oauthstore.NewExchangeSession()
	started.AccessLifetime = lifetime
	started.StartAccessLifetime(checked)
	started.SetExpiresAt(oauth2.AccessToken, asked)
	started.SetExpiresAt(oauth2.RefreshToken, asked)
	if got, want := started.GetExpiresAt(oauth2.AccessToken), checked.Add(lifetime); !got.Equal(want) {
		t.Errorf("a started lifetime's access token expires %s, want %s, the lifetime from the check", got, want)
	}
	if got := started.GetExpiresAt(oauth2.RefreshToken); !got.Equal(asked) {
		t.Errorf("a started lifetime's refresh token expires %s, want %s, as asked", got, asked)
	}
	earlier := time.Now().UTC().Add(30 * time.Second)
	started.SetExpiresAt(oauth2.AccessToken, earlier)
	if got := started.GetExpiresAt(oauth2.AccessToken); !got.Equal(earlier) {
		t.Errorf("a started lifetime asked an earlier expiry %s sets %s, want the earlier one", earlier, got)
	}

	unstarted := oauthstore.NewExchangeSession()
	unstarted.AccessLifetime = lifetime
	unstarted.SetExpiresAt(oauth2.AccessToken, asked)
	if got, now := unstarted.GetExpiresAt(oauth2.AccessToken), time.Now().UTC(); got.After(now) {
		t.Errorf("a lifetime decided and never started expires %s, %s after now, want at once", got, got.Sub(now).Round(time.Second))
	}

	plain := oauthstore.NewExchangeSession()
	plain.SetExpiresAt(oauth2.AccessToken, asked)
	if got := plain.GetExpiresAt(oauth2.AccessToken); !got.Equal(asked) {
		t.Errorf("a session with no lifetime expires %s, want %s, as asked", got, asked)
	}
}
