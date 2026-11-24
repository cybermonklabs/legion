package githubapp

import (
	"context"
	"testing"
	"time"
)

// RepositoryTokenWithStatuses answers whether the token it hands out holds Commit statuses from
// the permissions GitHub's mint named for that very token, and never from the installation
// lookup: the two differ for as long as a token minted before a person changed the grant lives.
// The answer is kept with the token, so a second call mints nothing and answers the same, and
// RepositoryToken hands out the same token.
func TestRepositoryTokenWithStatusesReadsTheTokensOwnPermissions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		installation string
		token        map[string]string
		want         bool
	}{
		{"the token holds statuses at read", "read", map[string]string{"contents": "read", "metadata": "read", "statuses": "read"}, true},
		{"the token holds statuses at write", "write", map[string]string{"contents": "read", "metadata": "read", "statuses": "write"}, true},
		{"the installation holds statuses and the token does not", "read", map[string]string{"contents": "read", "metadata": "read"}, false},
		{"the token holds statuses and the installation lookup says none", "", map[string]string{"contents": "read", "metadata": "read", "statuses": "read"}, true},
		{"the token names statuses at none", "read", map[string]string{"contents": "read", "metadata": "read", "statuses": "none"}, false},
		{"the token answer names no permissions", "read", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeGitHub{
				t:                    t,
				installations:        map[string]string{"acme/web": "read"},
				installationID:       4,
				tokenExpiresAt:       time.Now().Add(time.Hour),
				installationStatuses: tc.installation,
				tokenPermissions:     tc.token,
			}
			client := newTestClient(t, fake)
			ctx := context.Background()

			token, statuses, err := client.RepositoryTokenWithStatuses(ctx, "acme", "web")
			if err != nil || token == "" || statuses != tc.want {
				t.Fatalf("RepositoryTokenWithStatuses(acme/web) = (%q, %v, %v), want a token and statuses %v", token, statuses, err, tc.want)
			}
			again, statusesAgain, err := client.RepositoryTokenWithStatuses(ctx, "acme", "web")
			if err != nil || again != token || statusesAgain != statuses {
				t.Fatalf("the second call = (%q, %v, %v), want the cached (%q, %v)", again, statusesAgain, err, token, statuses)
			}
			plain, err := client.RepositoryToken(ctx, "acme", "web")
			if err != nil || plain != token {
				t.Fatalf("RepositoryToken(acme/web) = (%q, %v), want the same token %q", plain, err, token)
			}
			if fake.tokenMints != 1 {
				t.Fatalf("token mints: got %d, want 1: every call is answered from the one cached token", fake.tokenMints)
			}
		})
	}

	// A person grants the App Commit statuses while a token minted before it lives: that token
	// still holds none, and the token minted after it does.
	t.Run("a token minted after the grant holds it", func(t *testing.T) {
		fake := &fakeGitHub{
			t:                    t,
			installations:        map[string]string{"acme/web": "read"},
			installationID:       4,
			tokenExpiresAt:       time.Now().Add(time.Hour),
			installationStatuses: "read",
			tokenPermissions:     map[string]string{"contents": "read", "metadata": "read"},
		}
		client := newTestClient(t, fake)
		ctx := context.Background()
		if _, statuses, err := client.RepositoryTokenWithStatuses(ctx, "acme", "web"); err != nil || statuses {
			t.Fatalf("before the grant: statuses %v, err %v; want none", statuses, err)
		}
		fake.tokenPermissions = map[string]string{"contents": "read", "metadata": "read", "statuses": "read"}
		if _, statuses, err := client.RepositoryTokenWithStatuses(ctx, "acme", "web"); err != nil || statuses {
			t.Fatalf("the token minted before the grant: statuses %v, err %v; want none, as its own mint said", statuses, err)
		}
		// Age the cached entry rather than the clock, as TestTokenCachesUntilNearExpiry does.
		client.mu.Lock()
		aged := client.tokens[4]
		aged.expires = client.now().Add(4 * time.Minute)
		client.tokens[4] = aged
		client.mu.Unlock()
		if _, statuses, err := client.RepositoryTokenWithStatuses(ctx, "acme", "web"); err != nil || !statuses {
			t.Fatalf("the token minted after the grant: statuses %v, err %v; want Commit statuses", statuses, err)
		}
		if fake.tokenMints != 2 {
			t.Fatalf("token mints: got %d, want 2", fake.tokenMints)
		}
	})
}
