package google_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
	"github.com/sjawhar/envoy/internal/cipher/identity/identitytest"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

const contractAccount = "fleet@example-project.iam.gserviceaccount.com"

func TestGoogleRunsTheIdentityContract(t *testing.T) {
	machine := map[string]any{
		"email": contractAccount, "email_verified": true,
		"google": map[string]any{"compute_engine": map[string]any{
			"project_id": "example-project", "zone": "example-zone-a", "instance_name": "example-vm-1", "instance_id": "1234567890",
		}},
	}
	identitytest.Contract(t, identitytest.Fixture{
		New: func(t *testing.T, issuer string, keys *oidc.Keys, audience string) identity.Provider {
			p, err := google.New(google.Config{
				Issuer: issuer, Keys: keys, Audience: audience, PersonAudience: audience, SignInDomain: "example.com",
				Machines: []google.GoogleEntry{{Account: contractAccount, Service: "example-fleet"}},
			})
			if err != nil {
				t.Fatalf("google.New: %v", err)
			}
			return p
		},
		Audience: "https://broker.example",
		Subject:  "1234567890",
		Claims:   machine,
		Want:     identity.Account{Kind: google.Kind, Address: contractAccount},
		Trades: []identitytest.TradeFixture{
			{TokenType: google.SubjectTokenType, Claims: machine, WantPrincipal: "google:" + contractAccount},
			{
				TokenType:     google.PersonTokenType,
				Claims:        map[string]any{"sub": "109ada", "email": "ada@example.com", "email_verified": true, "hd": "example.com"},
				Actor:         machine,
				WantPrincipal: "person:ada@example.com",
			},
		},
	})
}

func TestPrincipalSpellsTodaysGooglePrincipal(t *testing.T) {
	if got := google.Principal(contractAccount); got != "google:"+contractAccount {
		t.Fatalf("Principal = %q, want google:%s", got, contractAccount)
	}
}

// TestThePersonTypesAreConfiguredOnlyWithAPersonAudience: a broker that accepts no person's
// sign-in registers the person subject type and the machine actor type unconfigured, so the
// library refuses either before any handler runs, as the exchange registered them before.
func TestThePersonTypesAreConfiguredOnlyWithAPersonAudience(t *testing.T) {
	configured := func(c google.Config) map[string]bool {
		t.Helper()
		p, err := google.New(c)
		if err != nil {
			t.Fatalf("google.New: %v", err)
		}
		got := map[string]bool{}
		for _, tt := range p.TokenTypes() {
			got[tt.Name] = tt.Configured()
		}
		return got
	}
	machines := google.Config{Issuer: "https://accounts.example", Audience: "broker-machines"}
	if got := configured(machines); !got[google.SubjectTokenType] || got[google.PersonTokenType] || got[google.ActorTokenTypeMachine] {
		t.Errorf("types with a machine audience alone = %v, want the machine type configured alone", got)
	}
	signIns := machines
	signIns.PersonAudience = "broker-signin"
	if got := configured(signIns); !got[google.SubjectTokenType] || !got[google.PersonTokenType] || !got[google.ActorTokenTypeMachine] {
		t.Errorf("types with a person audience = %v, want all three configured", got)
	}
	if _, err := google.New(google.Config{Issuer: "https://accounts.example", PersonAudience: "broker-signin"}); err == nil ||
		err.Error() != "oauth: a broker that accepts a person's Google identity token needs the Google issuer too: the machine the person signs in on presents its own identity token as the actor, and the entries that list that machine are the Google ones" {
		t.Errorf("google.New(a person audience and no machine audience) = %v, want today's refusal", err)
	}
}

// TestACallerTokenNamingNoVerifiedAccountNamesNoCaller: a sound Google identity token is a
// caller only by its verified email address; one naming none, or an unverified one, is refused
// with the sentence the API answered before, and a token of another issuer is oidc.ErrIssuer,
// which a reader of several verifiers takes as "not mine".
func TestACallerTokenNamingNoVerifiedAccountNamesNoCaller(t *testing.T) {
	ctx := context.Background()
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "google-key")
	keys, err := oidc.NewKeys(ctx, issuer.URL())
	if err != nil {
		t.Fatal(err)
	}
	p, err := google.New(google.Config{Issuer: issuer.URL(), Keys: keys, Audience: "broker-machines"})
	if err != nil {
		t.Fatal(err)
	}
	verifier := p.CallerVerifier("broker-machines")
	if verifier.Kind() != google.Kind || verifier.Issuer() != issuer.URL() {
		t.Fatalf("CallerVerifier = kind %q, issuer %q; want google and the issuer", verifier.Kind(), verifier.Issuer())
	}
	for name, change := range map[string]func(map[string]any){
		"no email":            func(c map[string]any) { delete(c, "email") },
		"an unverified email": func(c map[string]any) { c["email_verified"] = false },
	} {
		claims := issuer.Claims("1234567890", "broker-machines")
		claims["email"], claims["email_verified"] = contractAccount, true
		change(claims)
		_, err := verifier.VerifyCaller(ctx, issuer.Mint(t, key, claims))
		var none *identity.NoAccountError
		if !errors.As(err, &none) || none.Reason != "the Google identity token names no verified account, so it names no caller (ask for it with format=full)" {
			t.Errorf("VerifyCaller(a token with %s) = %v, want the no-verified-account refusal", name, err)
		}
	}
	other := oidctest.New(t)
	otherKey := other.PublishKey(t, "google-key")
	if _, err := verifier.VerifyCaller(ctx, other.Mint(t, otherKey, other.Claims("1234567890", "broker-machines"))); !errors.Is(err, oidc.ErrIssuer) {
		t.Errorf("VerifyCaller(another issuer's token) = %v, want oidc.ErrIssuer", err)
	}
}

// TestTheSettingsNameTheExchangeAudienceOnlyWhereAMachineIsListed: GET /v1/settings names the
// machine audience only where an entry lists a machine the exchange may mint a credential for, as
// it did before.
func TestTheSettingsNameTheExchangeAudienceOnlyWhereAMachineIsListed(t *testing.T) {
	for _, c := range []struct {
		name     string
		machines []google.GoogleEntry
		want     string
	}{
		{name: "a listed machine", machines: []google.GoogleEntry{{Account: contractAccount, Service: "example-fleet"}}, want: "broker-machines"},
		{name: "an API caller alone", want: ""},
	} {
		p, err := google.New(google.Config{Audience: "broker-machines", Machines: c.machines})
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Settings().ExchangeAudience; got != c.want {
			t.Errorf("Settings() with %s = %q, want %q", c.name, got, c.want)
		}
	}
}
