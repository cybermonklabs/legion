package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
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
// context's error, which the listener logs. Both drop the name from the airlock proxy's cache,
// since the reread of the name is the signal.
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
		// read in force.
		ran bool
	}{
		{"its value read", "GetSecretValue", true},
		{"its metadata read", "DescribeSecret", false},
	} {
		t.Run("ended during "+row.name, func(t *testing.T) {
			driven++
			local := secrets.NewLocal(policytest.KindSecret("APP_KEY", "ada@example.com", policy.TierAgent, policy.KindGitHubApp, policytest.KindValue(policy.KindGitHubApp)))
			store := &abandoningCall{Local: local}
			loader := policytest.Loader(local)
			loader.Store = policytest.StoreThrough(local, store)
			current, err := policy.NewCurrent(t.Context(), loader, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			// A retag to human tier, with a new version of the value (a trailing space, which the
			// decoder still reads), so the reread both merges new tags and reads the value.
			local.Put(policytest.KindSecret("APP_KEY", "ada@example.com", policy.TierHuman, policy.KindGitHubApp, policytest.KindValue(policy.KindGitHubApp)+" "))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store.cut, store.cancel = row.cut, cancel
			var dropped []string
			err = peerReread(current, func(name string) { dropped = append(dropped, name) })(ctx, "APP_KEY")

			if !slices.Equal(dropped, []string{"APP_KEY"}) {
				t.Errorf("dropped %q from the airlock proxy's cache, want APP_KEY", dropped)
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
