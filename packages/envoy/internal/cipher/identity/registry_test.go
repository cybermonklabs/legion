package identity_test

import (
	"context"
	"crypto"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
)

// stub is a provider of the kind it is told, registering the token types and listings it is
// told, and claiming the subjects its subjects map names.
type stub struct {
	kind     identity.Kind
	types    []identity.TokenType
	listings []exchange.Listing
	subjects map[string]identity.Subject
}

func (s stub) Kind() identity.Kind                           { return s.kind }
func (s stub) TokenTypes() []identity.TokenType              { return s.types }
func (s stub) Listings() []exchange.Listing                  { return s.listings }
func (s stub) Principal(address string) string               { return string(s.kind) + ":" + address }
func (s stub) CallerVerifier(string) identity.CallerVerifier { return verifier{kind: s.kind} }
func (s stub) Decide(identity.Trade) (exchange.Claims, error) {
	return exchange.Claims{}, nil
}
func (s stub) LauncherSubject(identity.Trade, exchange.Claims, string) (string, string) {
	return "", ""
}
func (s stub) ReadSubject(subject string) (identity.Subject, bool) {
	read, ok := s.subjects[subject]
	return read, ok
}

// stubPods is a stub that serves pods too.
type stubPods struct{ stub }

func (stubPods) PodVerifier(string) identity.PodVerifier            { return nil }
func (stubPods) Registration(account, podUID, session string) error { return nil }

// verifier is a caller verifier of one kind, told apart by its issuer.
type verifier struct {
	kind   identity.Kind
	issuer string
}

func (v verifier) Kind() identity.Kind { return v.kind }
func (v verifier) Issuer() string      { return v.issuer }
func (v verifier) VerifyCaller(context.Context, string) (identity.Caller, error) {
	return identity.Caller{}, nil
}

// configured is a token type of name whose issuer, audience and keys are all set.
func configured(name string) identity.TokenType {
	return identity.TokenType{Name: name, Issuer: "https://issuer.example", Audience: "broker", Keys: keys{}, Tolerance: time.Hour}
}

type keys struct{}

func (keys) Key(context.Context, string, string) (crypto.PublicKey, error) { return nil, nil }

func TestNewRegistryRefusesAKindTwice(t *testing.T) {
	_, err := identity.NewRegistry([]identity.Provider{stub{kind: "stub"}, stub{kind: "stub"}}, nil)
	if err == nil || !strings.Contains(err.Error(), `"stub"`) {
		t.Fatalf("NewRegistry(one kind twice) = %v, want a refusal naming \"stub\"", err)
	}
}

func TestNewRegistryRefusesATokenTypeTwoProvidersRegister(t *testing.T) {
	const urn = "urn:example:params:oauth:token-type:shared"
	_, err := identity.NewRegistry([]identity.Provider{
		stub{kind: "one", types: []identity.TokenType{configured(urn)}},
		stub{kind: "two", types: []identity.TokenType{{Name: urn}}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), urn) {
		t.Fatalf("NewRegistry(a type two providers register) = %v, want a refusal naming %s", err, urn)
	}
}

func TestNewRegistryRefusesTwoPodProviders(t *testing.T) {
	_, err := identity.NewRegistry([]identity.Provider{stubPods{stub{kind: "one"}}, stubPods{stub{kind: "two"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), `"one"`) || !strings.Contains(err.Error(), `"two"`) {
		t.Fatalf("NewRegistry(two pod providers) = %v, want a refusal naming both", err)
	}
}

func TestTokenTypesAreTheConfiguredOnes(t *testing.T) {
	registry, err := identity.NewRegistry([]identity.Provider{stub{kind: "stub", types: []identity.TokenType{
		configured("urn:example:configured"),
		{Name: "urn:example:unconfigured"},
		func() identity.TokenType {
			actor := configured("urn:example:actor")
			actor.ActorOnly = true
			return actor
		}(),
	}}}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	var names []string
	for _, tt := range registry.TokenTypes() {
		names = append(names, tt.Name)
	}
	if want := []string{"urn:example:configured", "urn:example:actor"}; !slices.Equal(names, want) {
		t.Errorf("TokenTypes() = %v, want %v: the configured ones alone", names, want)
	}
	subject, actor := registry.TokenTypeNames()
	if want := []string{"urn:example:configured", "urn:example:unconfigured"}; !slices.Equal(subject, want) {
		t.Errorf("TokenTypeNames() subject = %v, want %v: every registered subject type", subject, want)
	}
	if want := []string{"urn:example:actor"}; !slices.Equal(actor, want) {
		t.Errorf("TokenTypeNames() actor = %v, want %v", actor, want)
	}
	if tt, provider, ok := registry.TokenType("urn:example:unconfigured"); !ok || tt.Configured() || provider.Kind() != "stub" {
		t.Errorf("TokenType(unconfigured) = %+v, %v, %t; want the unconfigured type of stub", tt, provider, ok)
	}

	half := identity.TokenType{Name: "urn:example:half", Issuer: "https://issuer.example"}
	_, err = identity.NewRegistry([]identity.Provider{stub{kind: "stub", types: []identity.TokenType{half}}}, nil)
	if err == nil || err.Error() != half.Validate().Error() || !strings.Contains(err.Error(), "urn:example:half") {
		t.Fatalf("NewRegistry(a type with an issuer alone) = %v, want Validate's refusal naming the type", err)
	}
}

func TestReadSubjectAsksEachProviderInRankOrderAndAnswersOtherForNone(t *testing.T) {
	first := identity.Subject{Kind: identity.SubjectMachine, Account: identity.Account{Kind: "first", Address: "a"}, Principal: "first:a"}
	second := identity.Subject{Kind: identity.SubjectSignIn, Account: identity.Account{Kind: "second", Address: "a"}, Principal: "second:a"}
	registry, err := identity.NewRegistry([]identity.Provider{
		stub{kind: "first", subjects: map[string]identity.Subject{"both": first}},
		stub{kind: "second", subjects: map[string]identity.Subject{"both": second, "second only": second}},
	}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := registry.ReadSubject("both"); got != first {
		t.Errorf("ReadSubject(a subject both claim) = %+v, want the first provider's %+v", got, first)
	}
	if got := registry.ReadSubject("second only"); got != second {
		t.Errorf("ReadSubject(a subject the second claims) = %+v, want %+v", got, second)
	}
	if got := registry.ReadSubject("nobody's"); got != (identity.Subject{Kind: identity.SubjectOther}) {
		t.Errorf("ReadSubject(a subject no provider claims) = %+v, want SubjectOther with nothing filled", got)
	}
}

func TestCallersKeepTheOrderGiven(t *testing.T) {
	callers := []identity.CallerVerifier{verifier{kind: "b", issuer: "https://b.example"}, verifier{kind: "a", issuer: "https://a.example"}}
	registry, err := identity.NewRegistry([]identity.Provider{stub{kind: "a"}, stub{kind: "b"}}, map[identity.Use][]identity.CallerVerifier{identity.UseAPI: callers})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := registry.Callers(identity.UseAPI); !slices.Equal(got, callers) {
		t.Errorf("Callers(UseAPI) = %v, want %v in the order given", got, callers)
	}
	if got := registry.Callers(identity.UseNATS); len(got) != 0 {
		t.Errorf("Callers(UseNATS) = %v, want none", got)
	}
}

func TestPolicyListsEveryProvidersListings(t *testing.T) {
	listing := exchange.Listing{Where: "stub entry 1", Principal: "stub:listed", Person: "ada@example.com", Project: "ACME", Service: "fleet"}
	registry, err := identity.NewRegistry([]identity.Provider{
		stub{kind: "other", listings: []exchange.Listing{{Where: "other entry 1", Principal: "other:listed"}}},
		stub{kind: "stub", listings: []exchange.Listing{listing}},
	}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got, ok := registry.Policy().Listed("stub:listed"); !ok || got != listing {
		t.Errorf("Policy().Listed(stub:listed) = %+v, %t; want %+v", got, ok, listing)
	}
	if _, ok := registry.Policy().Listed("other:listed"); !ok {
		t.Error("Policy().Listed(other:listed) = false, want the other provider's listing")
	}
	if person, projects, ok := registry.Policy().MappedProjects("stub:listed"); !ok || person != "ada@example.com" || !slices.Equal(projects, []string{"ACME"}) {
		t.Errorf("Policy().MappedProjects(stub:listed) = %q, %v, %t; want ada@example.com, [ACME]", person, projects, ok)
	}
}
