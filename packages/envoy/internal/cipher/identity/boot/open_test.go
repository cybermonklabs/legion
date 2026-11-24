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

	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/boot"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
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

// settingsOf is every provider's settings read from env, as the broker reads them from its
// environment (Load); a variable env does not name is unset.
func settingsOf(t *testing.T, env map[string]string) boot.Settings {
	t.Helper()
	settings, err := boot.Load(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("boot.Load: %v", err)
	}
	return settings
}

// everyIssuer is the environment naming every issuer and every audience the providers read.
func everyIssuer(i issuers) map[string]string {
	return map[string]string{
		"BROKER_K8S_OIDC_ISSUER": i.cluster.URL(), "BROKER_K8S_OIDC_AUDIENCE": "broker",
		"BROKER_EXCHANGE_K8S_AUDIENCE": "broker-exchange",
		"BROKER_GOOGLE_OIDC_ISSUER":    i.google.URL(), "BROKER_GOOGLE_OIDC_AUDIENCE": "broker-machines",
		"BROKER_SIGNIN_DEVICE_AUDIENCE":   "broker-signin",
		"BROKER_VM_SERVICES":              "example-fleet=example-fleet@example-project.iam.gserviceaccount.com",
		"BROKER_EXCHANGE_GITHUB_ISSUER":   i.github.URL(),
		"BROKER_EXCHANGE_GITHUB_AUDIENCE": "broker-ci",
	}
}

// TestOpenBuildsEachProviderFromItsSettings: Open reads each issuer's keys once and builds every
// provider of the build from the broker's settings: the four subject token types and the machine
// actor type, the API's callers in rank order, and the pod verifier.
func TestOpenBuildsEachProviderFromItsSettings(t *testing.T) {
	i := newIssuers(t)
	reads := &countingKeys{reads: map[string]int{}}
	b, err := boot.Open(context.Background(), settingsOf(t, everyIssuer(i)), reads.keys)
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
	b, err := boot.Open(context.Background(), settingsOf(t, nil), reads.keys)
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
	// An API caller's audience alone: the same environment with no VM service listed.
	callerOnly := withEnv(everyIssuer(i), map[string]string{"BROKER_VM_SERVICES": "", "BROKER_SIGNIN_DEVICE_AUDIENCE": ""})
	for _, c := range []struct {
		name string
		env  map[string]string
		want map[string]string
	}{
		{name: "a listed machine", env: listed, want: map[string]string{"google_exchange_audience": "broker-machines"}},
		{name: "an API caller's audience alone", env: callerOnly, want: map[string]string{}},
		{name: "no Google issuer", env: nil, want: map[string]string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := boot.Open(context.Background(), settingsOf(t, c.env), (&countingKeys{reads: map[string]int{}}).keys)
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

// vmEnv is the broker's environment with the cluster's issuer and, when audience is set, one VM
// service: example-fleet, run by exampleVMAccount in example-project as example-vm-*, with
// Google's own issuer, which the google provider reads for BROKER_GOOGLE_OIDC_ISSUER unset.
func vmEnv(audience string) map[string]string {
	env := map[string]string{
		"BROKER_K8S_OIDC_ISSUER": "https://cluster.example", "BROKER_K8S_OIDC_AUDIENCE": "example-broker-pod",
		"BROKER_EXCHANGE_K8S_AUDIENCE": "example-broker-exchange",
	}
	if audience != "" {
		env["BROKER_GOOGLE_OIDC_AUDIENCE"] = audience
		env["BROKER_VM_SERVICES"] = "example-fleet=" + exampleVMAccount
		env["BROKER_VM_PROJECTS"] = exampleVMAccount + "=example-project"
		env["BROKER_VM_INSTANCES"] = exampleVMAccount + "=example-vm-*"
	}
	return env
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
	b, err := boot.Open(context.Background(), settingsOf(t, vmEnv("")), d.keysFor)
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
	b, err := boot.Open(context.Background(), settingsOf(t, vmEnv("example-vm-audience")), d.keysFor)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://cluster.example", google.DefaultIssuer}; !slices.Equal(d.asked, want) {
		t.Fatalf("read the key sets of %v, want %v", d.asked, want)
	}
	if keys := keysOf(b, oauthwire.SubjectTokenTypeGoogle); keys == nil || keys != identity.KeySource(d.keys[google.DefaultIssuer]) {
		t.Fatalf("the exchange's Google key source = %v, want Google's own, %p", keys, d.keys[google.DefaultIssuer])
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
	const standIn = "http://127.0.0.1:18080"
	env := withEnv(vmEnv("example-vm-audience"), map[string]string{"BROKER_GOOGLE_OIDC_ISSUER": standIn})
	b, err := boot.Open(context.Background(), settingsOf(t, env), d.keysFor)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://cluster.example", standIn}; !slices.Equal(d.asked, want) {
		t.Fatalf("read the key sets of %v, want %v", d.asked, want)
	}
	if keys := keysOf(b, oauthwire.SubjectTokenTypeGoogle); keys == nil || keys != identity.KeySource(d.keys[standIn]) {
		t.Fatalf("the exchange's Google key source = %v, want the stand-in issuer's own, %p", keys, d.keys[standIn])
	}
	if !strings.Contains(logged.String(), "issuer=http://127.0.0.1:18080") {
		t.Fatalf("log = %q, want the stand-in issuer named", logged)
	}
}

// TestOpenWiresEachNATSAudienceToItsKind: the NATS auth service's callers are each kind's verifier
// at its NATS audience, and the pod registration verifier is the cluster's at its own audience.
func TestOpenWiresEachNATSAudienceToItsKind(t *testing.T) {
	i := newIssuers(t)
	env := withEnv(everyIssuer(i), map[string]string{
		"BROKER_NATS_K8S_AUDIENCE": "nats-cluster", "BROKER_NATS_GOOGLE_AUDIENCE": "nats-google",
		"BROKER_POD_REGISTRATION_AUDIENCE": "broker-registration",
	})
	b, err := boot.Open(context.Background(), settingsOf(t, env), (&countingKeys{reads: map[string]int{}}).keys)
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
	b, err := boot.Open(context.Background(), settingsOf(t, map[string]string{
		"BROKER_GOOGLE_OIDC_ISSUER": i.google.URL(), "BROKER_NATS_GOOGLE_AUDIENCE": "nats-google",
	}), reads.keys)
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
	env := withEnv(everyIssuer(i), map[string]string{
		"BROKER_EXCHANGE_ACCOUNTS":  "system:serviceaccount:example-ns:example-worker=ACME=ada@example.com",
		"BROKER_EXCHANGE_WORKFLOWS": "example-org/example-repo=.github/workflows/release.yaml=refs/heads/main=ACME=ada@example.com",
	})
	b, err := boot.Open(context.Background(), settingsOf(t, env), (&countingKeys{reads: map[string]int{}}).keys)
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
	const (
		worker     = "system:serviceaccount:example-ns:example-worker"
		jobAccount = "system:serviceaccount:example-ns:example-job"
		jobFleet   = "example-job@example-project.iam.gserviceaccount.com"
		repo       = "example-org/example-repo"
	)
	env := withEnv(everyIssuer(i), map[string]string{
		"BROKER_EXCHANGE_ACCOUNTS":        worker + "=ACME=ada@example.com",
		"BROKER_EXCHANGE_JOB_ACCOUNTS":    jobAccount + "=ACME",
		"BROKER_EXCHANGE_WORKFLOWS":       repo + "=.github/workflows/release.yaml=refs/heads/main=ACME=ada@example.com",
		"BROKER_EXCHANGE_JOB_WORKFLOWS":   repo + "=.github/workflows/sweep.yaml=refs/heads/main=public,PRIVB,PRIVA",
		"BROKER_VM_SERVICES":              "example-jobs=" + jobFleet,
		"BROKER_VM_EXCHANGE_JOB_ACCOUNTS": jobFleet + "=PRIVA",
	})
	b, err := boot.Open(context.Background(), settingsOf(t, env), (&countingKeys{reads: map[string]int{}}).keys)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, c := range []struct {
		principal string
		projects  []string
	}{
		{"account:" + jobAccount, []string{"ACME"}},
		{"workflow:" + repo + "/.github/workflows/sweep.yaml@refs/heads/main", []string{"PRIVA", "PRIVB"}},
		{"google:" + jobFleet, []string{"PRIVA"}},
	} {
		person, projects, ok := b.Registry.Policy().MappedProjects(c.principal)
		if !ok || person != "" || !slices.Equal(projects, c.projects) {
			t.Errorf("Policy().MappedProjects(%s) = %q, %v, %t; want no person and the job list %v", c.principal, person, projects, ok, c.projects)
		}
	}
	// An account both settings name is refused where the provider reads its settings, before any
	// issuer is read, and the refusal names both settings and the account.
	twice := withEnv(env, map[string]string{"BROKER_EXCHANGE_JOB_ACCOUNTS": worker + "=ACME"})
	_, err = boot.Load(func(name string) string { return twice[name] })
	if err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_JOB_ACCOUNTS and BROKER_EXCHANGE_ACCOUNTS both name the service account "+worker) {
		t.Fatalf("Load with %s in BROKER_EXCHANGE_ACCOUNTS and BROKER_EXCHANGE_JOB_ACCOUNTS = %v; want a refusal naming both settings and the account", worker, err)
	}
}
