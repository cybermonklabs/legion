package requests

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/secrets"
)

func TestAzureLoginReleaseIsDisabledWithoutConfiguredSecret(t *testing.T) {
	file := filepath.Join(t.TempDir(), "azure-token")
	const token = "ey" + "JhbGciOiJSUzI1NiJ9.ey" + "JleHAiOjQxMDI0NDQ4MDB9.c2lnbmF0dXJl"
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	stored := policytest.TypeSecret("AZURE_LOGIN", fixtureOperator, policy.TierHuman, policy.TypeAzureLogin, `{"tenant_id":"tenant","client_id":"client"}`)
	local := secrets.NewLocal(stored)
	set, err := policytest.Loader(local).Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	machine := &Machine{
		Policy:  policytest.Current(t, local),
		Secrets: policytest.Store(local),
		Minters: map[string]Minter{policy.TypeAzureLogin: (secrets.AzureLogin{TokenFile: file}).Mint},
	}
	secret := set.Secrets["AZURE_LOGIN"]
	value, err := machine.release(t.Context(), requestedSecret{name: secret.Name, source: secret.Source, decision: policy.Approval}, secret, nil)
	if value.Value != "" || !errors.Is(err, ErrMintFailed) || secrets.MintErrorClass(err) != "refused" {
		t.Fatalf("release returned class %s, value present %t; want refused mint and no token", secrets.MintErrorClass(err), value.Value != "")
	}
}

func TestAzureLoginReleaseUsesThePolicySecretNameAndTier(t *testing.T) {
	file := filepath.Join(t.TempDir(), "azure-token")
	const token = "ey" + "JhbGciOiJSUzI1NiJ9.ey" + "JleHAiOjQxMDI0NDQ4MDB9.c2lnbmF0dXJl"
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, secret, tier, owner, decision string
		retagServiceGrant, allowed          bool
	}{
		{"another human secret", "OTHER_LOGIN", policy.TierHuman, policy.OwnerShared, policy.Approval, false, false},
		{"shared agent secret", "OTHER_LOGIN", policy.TierAgent, policy.OwnerShared, policy.Automatic, false, false},
		{"configured agent secret", "AZURE_LOGIN", policy.TierAgent, otherPerson, policy.Approval, false, false},
		{"automatic service grant after retag", "AZURE_LOGIN", policy.TierHuman, policy.OwnerShared, policy.Automatic, true, false},
		{"configured human secret", "AZURE_LOGIN", policy.TierHuman, policy.OwnerShared, policy.Approval, false, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			stored := policytest.TypeSecret(row.secret, row.owner, row.tier, policy.TypeAzureLogin,
				`{"tenant_id":"tenant","client_id":"client","name":"AZURE_LOGIN","human_tier":true}`)
			if row.retagServiceGrant {
				stored.Tags[policy.TagOwner] = fixtureService
				stored.Tags[policy.TagTier] = policy.TierAgent
			}
			local := secrets.NewLocal(stored)
			set, err := policytest.Loader(local, fixtureService).Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			secret, ok := set.Secrets[row.secret]
			if !ok {
				t.Fatal("policy did not serve the Azure login fixture")
			}
			decision, err := set.Evaluate(row.secret, policy.Requester{Operator: fixtureOperator, Service: fixtureService})
			if err != nil || decision.Outcome != row.decision {
				t.Fatalf("policy decision = %+v, %v, want %s", decision, err, row.decision)
			}
			granted := requestedSecret{name: secret.Name, source: secret.Source, decision: decision.Outcome}
			if row.retagServiceGrant {
				stored.Tags[policy.TagOwner] = row.owner
				stored.Tags[policy.TagTier] = row.tier
				local.Put(stored)
				current, err := policytest.Loader(local, fixtureService).Load(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				secret, ok = current.Secrets[row.secret]
				if !ok || secret.Tier != policy.TierHuman {
					t.Fatal("policy did not serve the retagged secret as human tier")
				}
			}
			machine := &Machine{
				Policy:  policytest.Current(t, local, fixtureService),
				Secrets: policytest.Store(local),
				Minters: map[string]Minter{policy.TypeAzureLogin: (secrets.AzureLogin{TokenFile: file, SecretName: "AZURE_LOGIN"}).Mint},
			}
			value, err := machine.release(t.Context(), granted, secret, nil)
			if row.allowed {
				var got map[string]string
				if err != nil || json.Unmarshal([]byte(value.Value), &got) != nil || got["federated_token"] != token {
					t.Fatal("release of the configured human-tier secret did not return its token")
				}
			} else if value.Value != "" || !errors.Is(err, ErrMintFailed) || secrets.MintErrorClass(err) != "refused" {
				t.Fatalf("release returned class %s, value present %t; want refused mint and no token", secrets.MintErrorClass(err), value.Value != "")
			}
		})
	}
}

// TestAnAzureLoginSplitCarriesTheApplicationAndTenantItsDocumentNames: the proxy holds every
// exchange of an azure-login's federated token to the application and tenant the name's own
// document names, so the split the proxy is answered from carries those two beside the token's
// slot - read out of what the real minter writes, so the ids are the stored document's and not
// a shape this test made up - while the session's document keeps both in the clear, as az
// needs them. A value of another kind carries none.
func TestAnAzureLoginSplitCarriesTheApplicationAndTenantItsDocumentNames(t *testing.T) {
	file := filepath.Join(t.TempDir(), "azure-token")
	const token = "ey" + "JhbGciOiJSUzI1NiJ9.ey" + "JleHAiOjQxMDI0NDQ4MDB9.c2lnbmF0dXJl"
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	minted, err := (secrets.AzureLogin{TokenFile: file, SecretName: "AZURE_LOGIN"}).Mint(t.Context(), secrets.MintRequest{
		Name: "AZURE_LOGIN", HumanTier: true, Value: `{"tenant_id":"the-tenant","client_id":"the-application"}`,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	split, err := airlockSplitOf(airlock.KindAzure, policy.TypeAzureLogin, minted.Value)
	if err != nil {
		t.Fatalf("airlockSplitOf: %v", err)
	}
	if want := (secrets.AzureLoginValue{TenantID: "the-tenant", ClientID: "the-application"}); split.azure != want {
		t.Fatalf("the split carries %+v; want the document's %+v", split.azure, want)
	}
	if !maps.Equal(split.values, map[string]string{SlotAzureFederated: token}) {
		t.Fatalf("the split's slots are %v; want the federated token's alone, holding the minted token", split.slots())
	}
	shape, err := split.fill(map[string]string{SlotAzureFederated: "the-placeholder"})
	if err != nil {
		t.Fatalf("fill: %v", err)
	}
	var released map[string]string
	if err := json.Unmarshal([]byte(shape), &released); err != nil {
		t.Fatalf("the released login is no JSON object: %v", err)
	}
	if released["tenant_id"] != "the-tenant" || released["client_id"] != "the-application" || released["federated_token"] != "the-placeholder" {
		t.Errorf("the session's login holds tenant_id %q, client_id %q and the placeholder %t; want both ids as minted and the placeholder in the token's place",
			released["tenant_id"], released["client_id"], released["federated_token"] == "the-placeholder")
	}

	for _, other := range []struct {
		name      string
		entryKind string
		kind      string
		value     string
	}{
		{"a stored value", "", "", "a-stored-value"},
		{"a stored AWS key pair", airlock.KindAWS, "", `{"access_key_id":"AKIAEXAMPLE","secret_access_key":"not-a-secret"}`},
	} {
		split, err := airlockSplitOf(other.entryKind, other.kind, other.value)
		if err != nil || split.azure != (secrets.AzureLoginValue{}) {
			t.Errorf("%s split as %+v, %v; want no application or tenant", other.name, split.azure, err)
		}
	}
}

// TestAnAzureLoginThatNamesNoApplicationOrTenantIsReleasedNowhere: the proxy could hold an
// exchange to no empty id, so a document whose tenant or application is missing, empty or no
// string is a shape refusal, released neither to the session nor to the proxy. The refusal
// names the field and carries nothing the document holds.
func TestAnAzureLoginThatNamesNoApplicationOrTenantIsReleasedNowhere(t *testing.T) {
	for _, row := range []struct{ name, value, field string }{
		{"no tenant_id", `{"client_id":"the-application","federated_token":"the-federated-token"}`, "tenant_id"},
		{"an empty tenant_id", `{"tenant_id":"","client_id":"the-application","federated_token":"the-federated-token"}`, "tenant_id"},
		{"a tenant_id that is no string", `{"tenant_id":7,"client_id":"the-application","federated_token":"the-federated-token"}`, "tenant_id"},
		{"no client_id", `{"tenant_id":"the-tenant","federated_token":"the-federated-token"}`, "client_id"},
		{"an empty client_id", `{"tenant_id":"the-tenant","client_id":"","federated_token":"the-federated-token"}`, "client_id"},
		{"a client_id that is no string", `{"tenant_id":"the-tenant","client_id":["the-application"],"federated_token":"the-federated-token"}`, "client_id"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := airlockSplitOf(airlock.KindAzure, policy.TypeAzureLogin, row.value)
			if !errors.Is(err, ErrAirlockValueShape) {
				t.Fatalf("airlockSplitOf = %v; want ErrAirlockValueShape", err)
			}
			words := err.Error()
			if !strings.Contains(words, row.field) {
				t.Errorf("the refusal %q does not name %s", words, row.field)
			}
			for _, held := range []string{"the-tenant", "the-application", "the-federated-token"} {
				if strings.Contains(words, held) {
					t.Errorf("the refusal %q carries %q, which the document holds", words, held)
				}
			}
		})
	}
}
