package githubapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// A fetch of the client's caches runs on a context its callers' cancel does not reach (ttlCache.get),
// so a caller that leaves before it answers leaves it running, and a rate limit GitHub answers it
// with is still recorded. Fetching answers true from before the call joins the fetch until the
// fetch has answered, whether or not its caller stayed, which the cache's stored answer shows. Each
// of the client's three caches has a row: the repositories' installations (RepositoryToken), the
// App's installations (ListInstallations) and each installation's repositories
// (ListInstallationRepositoriesByID).
func TestFetchingCountsAFetchWhoseCallerLeft(t *testing.T) {
	for _, cache := range []struct {
		name string
		// get gets the row's key from the row's cache of client, through a fetch that closes
		// started, waits for gate to close and answers.
		get func(ctx context.Context, client *Client, started, gate chan struct{}) error
		// stored answers whether the row's cache of client holds the fetch's answer, fresh.
		stored func(client *Client) bool
	}{{
		name: "repositories",
		get: func(ctx context.Context, client *Client, started, gate chan struct{}) error {
			_, err := client.repositories.get(ctx, "owner/repo", func(context.Context) (int64, error) {
				close(started)
				<-gate
				return 7, nil
			})
			return err
		},
		stored: func(client *Client) bool {
			value, fresh := client.repositories.fresh("owner/repo")
			return fresh && value == 7
		},
	}, {
		name: "installations",
		get: func(ctx context.Context, client *Client, started, gate chan struct{}) error {
			_, err := client.installations.get(ctx, installationsCacheKey, func(context.Context) ([]Installation, error) {
				close(started)
				<-gate
				return []Installation{{ID: 7}}, nil
			})
			return err
		},
		stored: func(client *Client) bool {
			value, fresh := client.installations.fresh(installationsCacheKey)
			return fresh && len(value) == 1 && value[0].ID == 7
		},
	}, {
		name: "installation repositories",
		get: func(ctx context.Context, client *Client, started, gate chan struct{}) error {
			_, err := client.installationRepos.get(ctx, 7, func(context.Context) ([]string, error) {
				close(started)
				<-gate
				return []string{"owner/repo"}, nil
			})
			return err
		},
		stored: func(client *Client) bool {
			value, fresh := client.installationRepos.fresh(7)
			return fresh && len(value) == 1 && value[0] == "owner/repo"
		},
	}} {
		t.Run(cache.name, func(t *testing.T) {
			client := &Client{
				repositories:      newTTLCache[string, int64](time.Minute, time.Now),
				installations:     newTTLCache[string, []Installation](time.Minute, time.Now),
				installationRepos: newTTLCache[int64, []string](time.Minute, time.Now),
			}
			gate := make(chan struct{})
			started := make(chan struct{})
			ctx, leave := context.WithCancel(context.Background())
			defer leave()
			answered := make(chan error, 1)
			go func() { answered <- cache.get(ctx, client, started, gate) }()
			<-started
			leave()
			if err := <-answered; !errors.Is(err, context.Canceled) {
				t.Fatalf("the caller that left was answered %v, want its own context's cancel", err)
			}
			if !client.Fetching() {
				t.Fatalf("Fetching answered false while the fetch of the %s cache whose caller left still ran", cache.name)
			}
			close(gate)
			testwait.Eventually(t, "the fetch to answer and Fetching to fall", func() bool { return !client.Fetching() })
			if !cache.stored(client) {
				t.Fatalf("the %s cache does not hold the fetch's answer once Fetching fell: Fetching fell before the fetch answered", cache.name)
			}
		})
	}
}
