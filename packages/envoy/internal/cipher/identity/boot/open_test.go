package boot_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/boot"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// countingKeys reads each issuer's keys through oidc.NewKeys and counts the reads per issuer.
type countingKeys struct {
	mu    sync.Mutex
	reads map[string]int
}

func (k *countingKeys) keys(ctx context.Context, issuer string) (*oidc.Keys, error) {
	k.mu.Lock()
	k.reads[issuer]++
	k.mu.Unlock()
	return oidc.NewKeys(ctx, issuer)
}

// issuers is one stand-in issuer for each provider's tokens.
type issuers struct{ cluster, google, github *oidctest.Issuer }

func newIssuers(t *testing.T) issuers {
	t.Helper()
	i := issuers{cluster: oidctest.New(t), google: oidctest.New(t), github: oidctest.New(t)}
	i.cluster.PublishKey(t, "cluster-key")
	i.google.PublishKey(t, "google-key")
	i.github.PublishKey(t, "github-key")
	return i
}

// everyIssuer is a configuration naming every issuer and every audience the providers read.
func everyIssuer(i issuers) config.Config {
	return config.Config{
		K8sOIDCIssuer: i.cluster.URL(), K8sOIDCAudience: "broker", ExchangeK8sAudience: "broker-exchange",
		GoogleOIDCIssuer: i.google.URL(), GoogleOIDCAudience: "broker-machines", SignInDeviceAudience: "broker-signin",
		ExchangeGitHubIssuer: i.github.URL(), ExchangeGitHubAudience: "broker-ci",
	}
}

// TestOpenBuildsEachProviderFromItsSettings: Open reads each issuer's keys once and builds every
// provider of the build from the broker's settings: the four subject token types and the machine
// actor type, the API's callers in rank order, and the pod verifier.
func TestOpenBuildsEachProviderFromItsSettings(t *testing.T) {
	i := newIssuers(t)
	reads := &countingKeys{reads: map[string]int{}}
	b, err := boot.Open(context.Background(), everyIssuer(i), reads.keys)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var names []string
	for _, tt := range b.Registry.TokenTypes() {
		names = append(names, tt.Name)
	}
	want := []string{
		oauthwire.SubjectTokenTypeKubernetes,
		oauthwire.SubjectTokenTypeGoogle, oauthwire.SubjectTokenTypePerson, oauthwire.ActorTokenTypeMachine,
		oauthwire.SubjectTokenTypeGitHub,
	}
	if !slices.Equal(names, want) {
		t.Errorf("TokenTypes() = %v, want %v", names, want)
	}
	var callers []identity.Kind
	for _, caller := range b.Registry.Callers(identity.UseAPI) {
		callers = append(callers, caller.Kind())
	}
	if !slices.Equal(callers, []identity.Kind{"kubernetes", "google"}) {
		t.Errorf("Callers(UseAPI) = %v, want kubernetes then google", callers)
	}
	if b.Pods == nil {
		t.Error("Pods = nil, want the cluster's pod verifier")
	}
	if b.PodRegistration != nil {
		t.Error("PodRegistration is set with no BROKER_POD_REGISTRATION_AUDIENCE")
	}
	for _, issuer := range []string{i.cluster.URL(), i.google.URL(), i.github.URL()} {
		if reads.reads[issuer] != 1 {
			t.Errorf("Open read the keys of %s %d times, want once", issuer, reads.reads[issuer])
		}
	}
}

// TestOpenReadsNoIssuerItIsNotTold: a broker told about no issuer reads no key set, registers
// every type unconfigured, has no pod verifier and authenticates no API or NATS caller.
func TestOpenReadsNoIssuerItIsNotTold(t *testing.T) {
	reads := &countingKeys{reads: map[string]int{}}
	b, err := boot.Open(context.Background(), config.Config{}, reads.keys)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(reads.reads) != 0 || len(b.Registry.TokenTypes()) != 0 || b.Pods != nil ||
		len(b.Registry.Callers(identity.UseAPI)) != 0 || len(b.Registry.Callers(identity.UseNATS)) != 0 {
		t.Errorf("Open(no issuer) read %v, registered %v, pods %v, callers %v/%v; want nothing",
			reads.reads, b.Registry.TokenTypes(), b.Pods, b.Registry.Callers(identity.UseAPI), b.Registry.Callers(identity.UseNATS))
	}
	subject, actor := b.Registry.TokenTypeNames()
	if len(subject) != 4 || len(actor) != 1 {
		t.Errorf("TokenTypeNames() = %v, %v; want every registered type, configured or not", subject, actor)
	}
}

// TestOpenWiresEachNATSAudienceToItsKind: the NATS auth service's callers are each kind's verifier
// at its NATS audience, and the pod registration verifier is the cluster's at its own audience.
func TestOpenWiresEachNATSAudienceToItsKind(t *testing.T) {
	i := newIssuers(t)
	cfg := everyIssuer(i)
	cfg.NATS.K8sAudience, cfg.NATS.GoogleAudience, cfg.PodRegistrationAudience = "nats-cluster", "nats-google", "broker-registration"
	b, err := boot.Open(context.Background(), cfg, (&countingKeys{reads: map[string]int{}}).keys)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var issuersOf []string
	for _, caller := range b.Registry.Callers(identity.UseNATS) {
		issuersOf = append(issuersOf, string(caller.Kind())+"="+caller.Issuer())
	}
	if want := []string{"kubernetes=" + i.cluster.URL(), "google=" + i.google.URL()}; !slices.Equal(issuersOf, want) {
		t.Errorf("Callers(UseNATS) = %v, want %v", issuersOf, want)
	}
	if b.PodRegistration == nil {
		t.Error("PodRegistration = nil with BROKER_POD_REGISTRATION_AUDIENCE set")
	}
}

// TestOpenReadsGooglesKeysForANATSAudienceAlone: a broker whose exchange takes no machine's Google
// token still reads Google's keys for the NATS auth service's Google audience, and registers the
// machine type unconfigured.
func TestOpenReadsGooglesKeysForANATSAudienceAlone(t *testing.T) {
	i := newIssuers(t)
	reads := &countingKeys{reads: map[string]int{}}
	b, err := boot.Open(context.Background(), config.Config{GoogleOIDCIssuer: i.google.URL(), NATS: config.NATSCallout{GoogleAudience: "nats-google"}}, reads.keys)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reads.reads[i.google.URL()] != 1 || len(b.Registry.TokenTypes()) != 0 || len(b.Registry.Callers(identity.UseNATS)) != 1 {
		t.Errorf("Open(a NATS Google audience alone) read %v, registered %v, NATS callers %v; want Google's keys once, no type, one caller",
			reads.reads, b.Registry.TokenTypes(), b.Registry.Callers(identity.UseNATS))
	}
}

// TestOpenListsEveryProvidersEntries: the policy holds every provider's entries, so a reader of
// the listed identities reads one set.
func TestOpenListsEveryProvidersEntries(t *testing.T) {
	i := newIssuers(t)
	cfg := everyIssuer(i)
	cfg.ExchangeAccounts = []exchange.AccountEntry{{Account: "system:serviceaccount:example-ns:example-worker", Project: "ACME", Person: "ada@example.com"}}
	cfg.ExchangeWorkflows = []exchange.WorkflowEntry{{Repository: "example-org/example-repo", Workflow: ".github/workflows/release.yaml", Ref: "refs/heads/main", Project: "ACME", Person: "ada@example.com"}}
	cfg.ExchangeGoogleAccounts = []exchange.GoogleEntry{{Account: "example-fleet@example-project.iam.gserviceaccount.com", Service: "example-fleet"}}
	b, err := boot.Open(context.Background(), cfg, (&countingKeys{reads: map[string]int{}}).keys)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, principal := range []string{
		"account:system:serviceaccount:example-ns:example-worker",
		"workflow:example-org/example-repo/.github/workflows/release.yaml@refs/heads/main",
		"google:example-fleet@example-project.iam.gserviceaccount.com",
	} {
		if _, ok := b.Registry.Policy().Listed(principal); !ok {
			t.Errorf("Policy().Listed(%s) = false, want the provider's entry", principal)
		}
	}
	if got := b.Registry.PodPrincipal("system:serviceaccount:a:b"); got != "account:system:serviceaccount:a:b" {
		t.Errorf("PodPrincipal = %q, want the kubernetes provider's spelling", got)
	}
}
