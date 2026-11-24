package boot_test

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/boot"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/logtest"
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

// TestOpenAnswersTheGoogleExchangeAudienceOnlyWhereAMachineIsListed: the google provider adds
// google_exchange_audience to GET /v1/settings, BROKER_GOOGLE_OIDC_AUDIENCE where an entry lists a
// machine the exchange may mint a credential for, and nothing where the audience is set for an API
// caller alone, as cipherd's googleExchangeAudience answered before.
func TestOpenAnswersTheGoogleExchangeAudienceOnlyWhereAMachineIsListed(t *testing.T) {
	i := newIssuers(t)
	listed := everyIssuer(i)
	listed.ExchangeGoogleAccounts = []exchange.GoogleEntry{{Account: "example-fleet@example-project.iam.gserviceaccount.com", Service: "example-fleet"}}
	for _, c := range []struct {
		name string
		cfg  config.Config
		want map[string]string
	}{
		{name: "a listed machine", cfg: listed, want: map[string]string{"google_exchange_audience": "broker-machines"}},
		{name: "an API caller's audience alone", cfg: everyIssuer(i), want: map[string]string{}},
		{name: "no Google issuer", cfg: config.Config{}, want: map[string]string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := boot.Open(context.Background(), c.cfg, (&countingKeys{reads: map[string]int{}}).keys)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !maps.Equal(b.SettingsFields, c.want) || b.SettingsFields == nil {
				t.Errorf("SettingsFields = %v, want %v (and a map, never nil)", b.SettingsFields, c.want)
			}
		})
	}
}

// exampleVMAccount is the one VM service account the discovery rows bind, a placeholder: no real
// account, project or instance name appears in a test.
const exampleVMAccount = "example-fleet@example-project.iam.gserviceaccount.com"

// vmConfig is the broker's configuration with the cluster's issuer and, when audience is set,
// one VM service, as config.Load reads it from BROKER_VM_SERVICES, BROKER_VM_PROJECTS and
// BROKER_VM_INSTANCES: example-fleet, run by exampleVMAccount in example-project as example-vm-*,
// with Google's own issuer, which Load answers for BROKER_GOOGLE_OIDC_ISSUER unset.
func vmConfig(audience string) config.Config {
	cfg := config.Config{
		K8sOIDCIssuer: "https://cluster.example", K8sOIDCAudience: "example-broker-pod", ExchangeK8sAudience: "example-broker-exchange",
		GoogleOIDCIssuer: config.DefaultGoogleOIDCIssuer,
	}
	if audience != "" {
		cfg.GoogleOIDCAudience = audience
		cfg.VMServicesByAccount = map[string]string{exampleVMAccount: "example-fleet"}
		cfg.ExchangeGoogleAccounts = []exchange.GoogleEntry{{Account: exampleVMAccount, Service: "example-fleet", CloudProject: "example-project", Instance: "example-vm-*"}}
	}
	return cfg
}

// discovery is a KeysFunc that records each issuer whose keys are asked for and answers a key
// source of its own for each, reading nothing.
type discovery struct {
	asked []string
	keys  map[string]*oidc.Keys
}

func (d *discovery) keysFor(_ context.Context, issuer string) (*oidc.Keys, error) {
	d.asked = append(d.asked, issuer)
	if d.keys == nil {
		d.keys = map[string]*oidc.Keys{}
	}
	d.keys[issuer] = &oidc.Keys{}
	return d.keys[issuer], nil
}

// captureLog sends slog's default logger to a buffer until t ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
	return &logged
}

// keysOf is the key source the exchange resolves a subject token of tokenType against, nil for a
// type registered unconfigured.
func keysOf(b boot.Boot, tokenType string) identity.KeySource {
	tt, _, _ := b.Registry.TokenType(tokenType)
	return tt.Keys
}

// TestWithNoGoogleAudienceTheBrokerDiscoversOneIssuer: with no Google audience the broker reads
// the cluster's key set alone, builds the pod verifier on it, answers that same key set for the
// token exchange, holds no Google key set, and logs nothing about Google.
func TestWithNoGoogleAudienceTheBrokerDiscoversOneIssuer(t *testing.T) {
	logged := captureLog(t)
	d := &discovery{}
	b, err := boot.Open(context.Background(), vmConfig(""), d.keysFor)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://cluster.example"}; !slices.Equal(d.asked, want) {
		t.Fatalf("read the key sets of %v, want the cluster's issuer alone, %v", d.asked, want)
	}
	if b.Pods == nil {
		t.Fatal("Pods = nil; want a pod verifier for the cluster's issuer")
	}
	if keys := keysOf(b, oauthwire.SubjectTokenTypeKubernetes); keys == nil || keys != identity.KeySource(d.keys["https://cluster.example"]) {
		t.Fatalf("the exchange's key source = %v, want the cluster's own, %p", keys, d.keys["https://cluster.example"])
	}
	if keys := keysOf(b, oauthwire.SubjectTokenTypeGoogle); keys != nil {
		t.Fatalf("the exchange was handed a Google key set (%v) with no Google audience; it must then register no Google subject token type", keys)
	}
	if strings.Contains(strings.ToLower(logged.String()), "google") {
		t.Fatalf("log with no Google audience names Google:\n%s", logged)
	}
}

// TestAGoogleAudienceDiscoversGooglesIssuer: with BROKER_GOOGLE_OIDC_AUDIENCE set the broker also
// reads the Google issuer's key set and hands it to the token exchange, which is what lets the
// exchange register the Google subject token type and resolve a machine's token against Google's
// own keys, and logs one line naming the issuer, the audience, the machine services and how many
// machine entries the exchange holds.
func TestAGoogleAudienceDiscoversGooglesIssuer(t *testing.T) {
	logged := captureLog(t)
	d := &discovery{}
	b, err := boot.Open(context.Background(), vmConfig("example-vm-audience"), d.keysFor)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://cluster.example", config.DefaultGoogleOIDCIssuer}; !slices.Equal(d.asked, want) {
		t.Fatalf("read the key sets of %v, want %v", d.asked, want)
	}
	if keys := keysOf(b, oauthwire.SubjectTokenTypeGoogle); keys == nil || keys != identity.KeySource(d.keys[config.DefaultGoogleOIDCIssuer]) {
		t.Fatalf("the exchange's Google key source = %v, want Google's own, %p", keys, d.keys[config.DefaultGoogleOIDCIssuer])
	}
	lines := strings.Count(logged.String(), "google machine identity")
	wantLine := `msg="google machine identity" issuer=https://accounts.google.com audience=example-vm-audience services=example-fleet fleets=1`
	if lines != 1 || !strings.Contains(logged.String(), wantLine) {
		t.Fatalf("log = %q, want one line holding %q", logged, wantLine)
	}
}

// TestAStandInIssuerIsDiscoveredInGooglesPlace: BROKER_GOOGLE_OIDC_ISSUER names the issuer whose
// keys the broker reads and whose tokens the exchange takes, so a dev stack's stand-in
// (cmd/dev-oidc-issuer) serves in Google's place with nothing else changed.
func TestAStandInIssuerIsDiscoveredInGooglesPlace(t *testing.T) {
	logged := captureLog(t)
	d := &discovery{}
	cfg := vmConfig("example-vm-audience")
	cfg.GoogleOIDCIssuer = "http://127.0.0.1:18080"
	b, err := boot.Open(context.Background(), cfg, d.keysFor)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://cluster.example", cfg.GoogleOIDCIssuer}; !slices.Equal(d.asked, want) {
		t.Fatalf("read the key sets of %v, want %v", d.asked, want)
	}
	if keys := keysOf(b, oauthwire.SubjectTokenTypeGoogle); keys == nil || keys != identity.KeySource(d.keys[cfg.GoogleOIDCIssuer]) {
		t.Fatalf("the exchange's Google key source = %v, want the stand-in issuer's own, %p", keys, d.keys[cfg.GoogleOIDCIssuer])
	}
	if !strings.Contains(logged.String(), "issuer=http://127.0.0.1:18080") {
		t.Fatalf("log = %q, want the stand-in issuer named", logged)
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

// TestOpenListsTheJobEntries: the job settings reach their providers beside the settings that
// name a person (BROKER_EXCHANGE_JOB_ACCOUNTS after BROKER_EXCHANGE_ACCOUNTS,
// BROKER_EXCHANGE_JOB_WORKFLOWS after BROKER_EXCHANGE_WORKFLOWS, and
// BROKER_VM_EXCHANGE_JOB_ACCOUNTS among the Google entries), so the policy answers each job
// entry's principal with no person and its whole list, and an account both a person's setting
// and a job setting list is refused as one identity listed twice, naming the job entry after the
// person's.
func TestOpenListsTheJobEntries(t *testing.T) {
	i := newIssuers(t)
	job := func(text string) exchange.JobProjects {
		list, err := exchange.ParseJobProjects(text)
		if err != nil {
			t.Fatalf("ParseJobProjects(%q): %v", text, err)
		}
		return list
	}
	const (
		worker     = "system:serviceaccount:example-ns:example-worker"
		jobAccount = "system:serviceaccount:example-ns:example-job"
	)
	cfg := everyIssuer(i)
	cfg.ExchangeAccounts = []exchange.AccountEntry{{Account: worker, Project: "ACME", Person: "ada@example.com"}}
	cfg.ExchangeJobAccounts = []exchange.AccountEntry{{Account: jobAccount, Job: job("ACME")}}
	cfg.ExchangeWorkflows = []exchange.WorkflowEntry{{Repository: "example-org/example-repo", Workflow: ".github/workflows/release.yaml", Ref: "refs/heads/main", Project: "ACME", Person: "ada@example.com"}}
	cfg.ExchangeJobWorkflows = []exchange.WorkflowEntry{{Repository: "example-org/example-repo", Workflow: ".github/workflows/sweep.yaml", Ref: "refs/heads/main", Job: job("public,PRIVB,PRIVA")}}
	cfg.ExchangeGoogleAccounts = []exchange.GoogleEntry{{Account: "example-job@example-project.iam.gserviceaccount.com", Service: "example-jobs", Job: job("PRIVA")}}
	b, err := boot.Open(context.Background(), cfg, (&countingKeys{reads: map[string]int{}}).keys)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, c := range []struct {
		principal string
		projects  []string
	}{
		{"account:" + jobAccount, []string{"ACME"}},
		{"workflow:example-org/example-repo/.github/workflows/sweep.yaml@refs/heads/main", []string{"PRIVA", "PRIVB"}},
		{"google:example-job@example-project.iam.gserviceaccount.com", []string{"PRIVA"}},
	} {
		person, projects, ok := b.Registry.Policy().MappedProjects(c.principal)
		if !ok || person != "" || !slices.Equal(projects, c.projects) {
			t.Errorf("Policy().MappedProjects(%s) = %q, %v, %t; want no person and the job list %v", c.principal, person, projects, ok, c.projects)
		}
	}
	cfg.ExchangeJobAccounts = []exchange.AccountEntry{{Account: worker, Job: job("ACME")}}
	if _, err := boot.Open(context.Background(), cfg, (&countingKeys{reads: map[string]int{}}).keys); err == nil || !strings.Contains(err.Error(), "account entry 2") {
		t.Fatalf("Open with %s in BROKER_EXCHANGE_ACCOUNTS and BROKER_EXCHANGE_JOB_ACCOUNTS = %v; want a refusal naming account entry 2", worker, err)
	}
}
