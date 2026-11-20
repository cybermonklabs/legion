package requests

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// retagDuringRead is a secrets.Reader that runs during once, after Values has checked its policy
// and before the store read it wraps answers, then reads the store as it stands after during.
type retagDuringRead struct {
	secrets.Reader
	once   *sync.Once
	during func(ctx context.Context) error
	err    *error
}

func (r retagDuringRead) Read(ctx context.Context, source string) (string, error) {
	r.once.Do(func() { *r.err = r.during(ctx) })
	return r.Reader.Read(ctx, source)
}

// TestAPlainGrantRacingATypeRetagNeverReleasesWhatTheTypeStores: a grant made while its secret was
// plain releases the secret's stored value. When a retag to a type is reread while a release is
// reading the store, and the type's long-lived credential is stored once the broker serves the
// type, that release returns nothing the store holds: its values are discarded and the grant ends
// for good, as a type change found before the read ends it. Each route to a grant is held to it: a
// grant made at once, the same grant a later request reuses, and one a person approved.
func TestAPlainGrantRacingATypeRetagNeverReleasesWhatTheTypeStores(t *testing.T) {
	const canary = "refresh-token-canary-9f3c"
	for _, route := range []string{"automatic", "reused", "approved"} {
		t.Run(route, func(t *testing.T) {
			machine, enrollmentID, key, approver := newFixture(t)
			tier := policy.TierAgent
			if route == "approved" {
				tier = policy.TierHuman
			}
			// The stored value is one the google-login decoder reads, so the retag below is
			// served as that type at once: the policy refuses a type secret whose value it cannot
			// decode (policy.ReasonValueMalformed), and the race needs it served.
			ordinary := policytest.TypeValue(policy.TypeGoogleLogin)
			retag(t, machine, policytest.Secret("RACED_LOGIN", fixtureOperator, tier, ordinary))
			minted := 0
			machine.Minters = map[string]Minter{
				policy.TypeGoogleLogin: func(context.Context, secrets.MintRequest) (secrets.Minted, error) {
					minted++
					return secrets.Minted{Value: "access-token"}, nil
				},
			}
			request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read", "RACED_LOGIN"), "", nil)
			if err != nil {
				t.Fatalf("Create = %v", err)
			}
			var grantID string
			switch route {
			case "automatic":
				if request.GrantID == nil {
					t.Fatalf("Create = %+v; want an automatic grant", request)
				}
				grantID = *request.GrantID
			case "reused":
				reused, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read again", "RACED_LOGIN"), "", nil)
				if err != nil || reused.GrantID == nil || request.GrantID == nil || *reused.GrantID != *request.GrantID {
					t.Fatalf("second Create = %+v, %v; want the first request's grant reused", reused, err)
				}
				grantID = *reused.GrantID
			case "approved":
				if request.RecordID == nil {
					t.Fatalf("Create = %+v; want a pending request", request)
				}
				decision, err := machine.ApplyDecision(t.Context(), *request.RecordID, true, approver)
				if err != nil || decision.GrantID == "" {
					t.Fatalf("ApplyDecision = %+v, %v", decision, err)
				}
				grantID = decision.GrantID
			}
			store := fixtureStore(machine)
			var duringErr error
			machine.Secrets = retagDuringRead{Reader: machine.Secrets, once: &sync.Once{}, err: &duringErr, during: func(ctx context.Context) error {
				store.Put(policytest.TypeSecret("RACED_LOGIN", fixtureOperator, tier, policy.TypeGoogleLogin, ordinary))
				if err := machine.Policy.Refresh(ctx); err != nil {
					return err
				}
				store.Put(policytest.TypeSecret("RACED_LOGIN", fixtureOperator, tier, policy.TypeGoogleLogin, canary))
				return nil
			}}
			release, err := machine.Values(t.Context(), grantID, enrollmentID, nil, ReleaseCall{})
			if duringErr != nil {
				t.Fatalf("the retag during the read: %v", duringErr)
			}
			if _, served := machine.Policy.Get().Secrets["RACED_LOGIN"]; !served {
				t.Fatal("the retag during the read left RACED_LOGIN unserved; the race never ran")
			}
			for _, value := range release.Values {
				if strings.Contains(value, canary) {
					t.Fatal("Values released the credential the secret stores once it has a type")
				}
			}
			if err != nil && strings.Contains(err.Error(), canary) {
				t.Fatal("Values' error holds the credential the secret stores once it has a type")
			}
			if !errors.Is(err, ErrGrantNotLive) || release.Values != nil || minted != 0 {
				t.Fatalf("Values across a retag to a type = %d values, %v after %d mints; want ErrGrantNotLive and none", len(release.Values), err, minted)
			}
			if by, audits := endedForGood(t, machine, grantID); by != GrantEndedActor || audits != 1 {
				t.Fatalf("grant revoked_by %q with %d grant.revoked rows by it; want %s and 1", by, audits, GrantEndedActor)
			}
		})
	}
}

// TestAPauseFoundAfterTheReadsHidesNoMovedSource: a grant of AUTO_TOKEN and RACED_LOGIN, both
// plain secrets its session got at once. While a release reads the store, AUTO_TOKEN, which sorts
// first, is retagged with a tag the loader refuses, so the policy no longer serves it, which only
// pauses the grant; and RACED_LOGIN is retagged to a type, so it is served from another source,
// which ends the grant for good. The check after the reads reads both names, so the release ends
// the grant, and once both retags are taken back the grant still releases nothing.
func TestAPauseFoundAfterTheReadsHidesNoMovedSource(t *testing.T) {
	machine, enrollmentID, key, _ := newFixture(t)
	ordinary := policytest.TypeValue(policy.TypeGoogleLogin)
	plain := policytest.Secret("RACED_LOGIN", fixtureOperator, policy.TierAgent, ordinary)
	retag(t, machine, plain)
	request, err := machine.Create(t.Context(), enrollmentID, signRequest(t, machine, key, "read both", "AUTO_TOKEN", "RACED_LOGIN"), "", nil)
	if err != nil || request.GrantID == nil {
		t.Fatalf("Create = %+v, %v; want an automatic grant", request, err)
	}
	grantID := *request.GrantID
	frozen := machine.Policy.Get().Secrets["RACED_LOGIN"].Source

	refused := policytest.Secret("AUTO_TOKEN", fixtureOperator, policy.TierAgent, "auto-v1")
	refused.Tags[policy.TagAdmin] = "yes"
	store, reader := fixtureStore(machine), machine.Secrets
	var duringErr error
	machine.Secrets = retagDuringRead{Reader: reader, once: &sync.Once{}, err: &duringErr, during: func(ctx context.Context) error {
		store.Put(refused)
		store.Put(policytest.TypeSecret("RACED_LOGIN", fixtureOperator, policy.TierAgent, policy.TypeGoogleLogin, ordinary))
		return machine.Policy.Refresh(ctx)
	}}
	release, err := machine.Values(t.Context(), grantID, enrollmentID, nil, ReleaseCall{})
	machine.Secrets = reader
	if duringErr != nil {
		t.Fatalf("the retags during the read: %v", duringErr)
	}
	now := machine.Policy.Get()
	if _, served := now.Secrets["AUTO_TOKEN"]; served {
		t.Fatal("the policy still serves AUTO_TOKEN under a malformed admin tag; the pause never ran")
	}
	if raced, served := now.Secrets["RACED_LOGIN"]; !served || raced.Source == frozen {
		t.Fatalf("RACED_LOGIN served %v from %q; want it served from another source than %q", served, raced.Source, frozen)
	}
	if !errors.Is(err, ErrGrantNotLive) || release.Values != nil {
		t.Fatalf("Values across the two retags = %v, %v; want ErrGrantNotLive and no value", release.Values, err)
	}
	if by, audits := endedForGood(t, machine, grantID); by != GrantEndedActor || audits != 1 {
		t.Errorf("grant revoked_by %q with %d grant.revoked rows by it; want %s and 1: RACED_LOGIN's moved source ends it whatever AUTO_TOKEN answers", by, audits, GrantEndedActor)
	}

	retag(t, machine, policytest.Secret("AUTO_TOKEN", fixtureOperator, policy.TierAgent, "auto-v1"), plain)
	if release, err := machine.Values(t.Context(), grantID, enrollmentID, nil, ReleaseCall{}); !errors.Is(err, ErrGrantNotLive) || release.Values != nil {
		t.Errorf("Values once both retags are taken back = %v, %v; want ErrGrantNotLive and no value", release.Values, err)
	}
}
