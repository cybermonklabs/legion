package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// abandoningCall is a secrets.Local behind which the caller of one kind of call goes away while
// it is in flight: once cut names it ("GetSecretValue" or "DescribeSecret"), that call ends its
// caller's context through cancel and fails as the AWS SDK does once a context has ended. Every
// other call is Local's.
type abandoningCall struct {
	*secrets.Local
	cut    string
	cancel context.CancelFunc
}

func (s *abandoningCall) GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	if s.cut == "GetSecretValue" {
		s.cancel()
		return nil, fmt.Errorf("operation error Secrets Manager: GetSecretValue, %w", ctx.Err())
	}
	return s.Local.GetSecretValue(ctx, in, opts...)
}

func (s *abandoningCall) DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, opts ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	if s.cut == "DescribeSecret" {
		s.cancel()
		return nil, fmt.Errorf("operation error Secrets Manager: DescribeSecret, %w", ctx.Err())
	}
	return s.Local.DescribeSecret(ctx, in, opts...)
}

// TestAPeersRereadThatMergedTheTagsItReadRan: the reread listener's Refresh (peerReread) answers
// a reread whose context ended during its value read alone (policy.ErrValueUnchecked) as one that
// ran, since it put the tags it read in force on this pod, so the listener writes no "did not run"
// line for it; a reread whose context ended during its metadata read merged nothing and is that
// context's error, which the listener logs. The first merged a new version of the App's value, so
// it drops the name from the airlock proxy's cache; the second merged no version, so it drops
// nothing (policy.Lookup.Moved).
//
// The named revert: peerReread returns RefreshOne's error whole; the value-read row then answers
// ErrValueUnchecked and this test fails.
func TestAPeersRereadThatMergedTheTagsItReadRan(t *testing.T) {
	driven := 0
	for _, row := range []struct {
		name string
		// cut is the call whose caller goes away.
		cut string
		// ran is whether peerReread answers the reread as one that ran (nil), with the tags it
		// read in force, and so with the new version of the value it lists.
		ran bool
		// dropped is what the reread drops from the airlock proxy's cache.
		dropped []string
	}{
		{"its value read", "GetSecretValue", true, []string{"APP_KEY"}},
		{"its metadata read", "DescribeSecret", false, nil},
	} {
		t.Run("ended during "+row.name, func(t *testing.T) {
			driven++
			local := secrets.NewLocal(policytest.TypeSecret("APP_KEY", "ada@example.com", policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)))
			store := &abandoningCall{Local: local}
			loader := policytest.Loader(local)
			loader.Store = policytest.StoreThrough(local, store)
			loader.Profiles = policytest.Profiles(t, policytest.ProfilesFile(policytest.AppProfile("APP_KEY")))
			current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			// A retag to human tier, with a new version of the value (a trailing space, which the
			// decoder still reads), so the reread both merges new tags and reads the value.
			local.Put(policytest.TypeSecret("APP_KEY", "ada@example.com", policy.TierHuman, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)+" "))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store.cut, store.cancel = row.cut, cancel
			var dropped []string
			err = peerReread(current, func(name string) { dropped = append(dropped, name) })(ctx, "APP_KEY")

			if !slices.Equal(dropped, row.dropped) {
				t.Errorf("dropped %q from the airlock proxy's cache, want %q", dropped, row.dropped)
			}
			tier := current.Get().Secrets["APP_KEY"].Tier
			switch {
			case row.ran && (err != nil || tier != policy.TierHuman):
				t.Fatalf("a peer's reread whose context ended during %s = %v, serving tier %q; want nil, a reread that ran, serving tier %q", row.name, err, tier, policy.TierHuman)
			case !row.ran && (!errors.Is(err, context.Canceled) || errors.Is(err, policy.ErrValueUnchecked) || tier != policy.TierAgent):
				t.Fatalf("a peer's reread whose context ended during %s = %v, serving tier %q; want its context's error alone, a reread that did not run, serving tier %q", row.name, err, tier, policy.TierAgent)
			}
		})
	}
	if driven != 2 {
		t.Fatalf("the table drove %d rows, want 2", driven)
	}
}

// TestAPeersRereadDropsEveryNameServedFromTheValueItRead: the airlock's proxy holds a grant's
// values by the name the grant was released under, a profile's own name for a profile, so a
// peer's reread of a source drops that source's profiles from the proxy's cache too, and a reread
// of a profile drops its source and its sibling profiles, which are served from the one stored
// value the reread read (policy.Current.RereadNames). Dropping the reread name alone left the
// source's old value in every request of a profile of another name until its grant ended. The
// source is behind a kind (policytest.Proxied), so its profiles are behind it too, as the names the
// proxy holds values of are.
func TestAPeersRereadDropsEveryNameServedFromTheValueItRead(t *testing.T) {
	local := secrets.NewLocal(policytest.Proxied(policytest.Secret("STORED_TOKEN", "ada@example.com", policy.TierAgent, "stored-v1"), "example"))
	loader := policytest.Loader(local)
	loader.Profiles = policytest.Profiles(t, policytest.KindsFile(
		`{"name": "STORED_TOKEN_READ", "source": "STORED_TOKEN"}`,
		`{"name": "STORED_TOKEN_SECOND", "source": "STORED_TOKEN"}`,
	))
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	local.Put(policytest.Proxied(policytest.Secret("STORED_TOKEN", "ada@example.com", policy.TierAgent, "stored-v2"), "example"))
	want := []string{"STORED_TOKEN", "STORED_TOKEN_READ", "STORED_TOKEN_SECOND"}
	for _, name := range []string{"STORED_TOKEN", "STORED_TOKEN_READ"} {
		var dropped []string
		if err := peerReread(current, func(name string) { dropped = append(dropped, name) })(t.Context(), name); err != nil {
			t.Fatalf("a peer's reread of %s = %v", name, err)
		}
		if got := slices.Sorted(slices.Values(dropped)); !slices.Equal(got, want) {
			t.Errorf("a peer's reread of %s dropped %q from the airlock proxy's cache; want %q", name, got, want)
		}
	}
}

// TestAPeersRereadDropsAMintedSourcesNamesOnlyWhenItMovedTheValueVersion: a minted name's held
// value is a token the proxy minted from the stored value, so a reread drops the names of a type
// source (policy.Lookup.Moved) only when it moved that source's value version
// (policy.Set.ValueVersion): a reread of the source or of a profile of it that finds the version
// the live policy held drops nothing, and the proxy goes on spending the token it holds, rather
// than minting again for every held grant of every profile at one public call. A reread that
// moved it drops every profile of it. A stored value has no version the policy tracks, so its
// reread is the only signal of a new value, and drops every name served from it each time. The
// stored value is behind a kind (policytest.Proxied), as the names the proxy holds values of are.
//
// The named revert: peerReread drops current.RereadNames(name) whatever the version did; the two
// rows of an unmoved github-app source then drop its names and this test fails.
func TestAPeersRereadDropsAMintedSourcesNamesOnlyWhenItMovedTheValueVersion(t *testing.T) {
	local := secrets.NewLocal(
		policytest.TypeSecret("APP_KEY", "ada@example.com", policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)),
		policytest.Proxied(policytest.Secret("STORED_TOKEN", "ada@example.com", policy.TierAgent, "stored-v1"), "example"))
	loader := policytest.Loader(local)
	loader.Profiles = policytest.Profiles(t, policytest.KindsFile(
		policytest.AppProfile("APP_KEY"),
		`{"name": "APP_READ", "source": "APP_KEY", "github": {"accounts": ["acme"], "permissions": {"contents": "read", "metadata": "read"}}}`,
		`{"name": "STORED_TOKEN_READ", "source": "STORED_TOKEN"}`,
	))
	current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reread := func(name string) []string {
		t.Helper()
		var dropped []string
		if err := peerReread(current, func(name string) { dropped = append(dropped, name) })(t.Context(), name); err != nil {
			t.Fatalf("a peer's reread of %s = %v", name, err)
		}
		return slices.Sorted(slices.Values(dropped))
	}
	app := []string{"APP_KEY", "APP_READ"}
	stored := []string{"STORED_TOKEN", "STORED_TOKEN_READ"}

	for _, name := range app {
		if dropped := reread(name); len(dropped) != 0 {
			t.Errorf("a peer's reread of %s, whose source's value version did not move, dropped %q from the airlock proxy's cache; want nothing", name, dropped)
		}
	}
	for _, name := range stored {
		if dropped := reread(name); !slices.Equal(dropped, stored) {
			t.Errorf("a peer's reread of %s, a stored value's, dropped %q; want %q: its reread is the only signal of a new value", name, dropped, stored)
		}
	}
	// A new version of the App's value (a trailing space, which the decoder still reads).
	local.Put(policytest.TypeSecret("APP_KEY", "ada@example.com", policy.TierAgent, policy.TypeGitHubApp, policytest.TypeValue(policy.TypeGitHubApp)+" "))
	if dropped := reread("APP_READ"); !slices.Equal(dropped, app) {
		t.Errorf("a peer's reread of APP_READ that moved its source's value version dropped %q; want %q", dropped, app)
	}
}
