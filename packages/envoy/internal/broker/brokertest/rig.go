// packages/envoy/internal/broker/brokertest/rig.go

// Package brokertest mounts the real broker (api.Register) — real Postgres store, real
// enroll/machine-login/request services — on an httptest.Server, for any package that needs to
// drive a genuine broker end to end rather than a hand-rolled fake. It is the extraction
// api_test.go's own newTestServer names as its reference pattern. Every consumer of a full
// real-broker rig imports this package instead of duplicating the wiring.
package brokertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/hold"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/oauth"
	"github.com/sjawhar/envoy/internal/broker/oauthstore"
	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
	"github.com/sjawhar/envoy/internal/broker/projectaccess"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/broker/store"
	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// podAudience is the fixed audience every Rig's pod verifier is constructed for, and the value
// MintPodToken and MintAccountToken sign tokens against — arbitrary, since a Rig both mints and
// verifies them itself; chosen only to read clearly in a failure message.
const podAudience = "legion-broker-pod"

// ExchangeAudience is the audience every Rig's token exchange takes a pod's subject token for,
// the one MintSubjectToken signs: a second audience of the same issuer, as a deployment gives an
// exchanging pod a token beside the one it enrols with. Tests read it to mint a subject token of
// their own for a provider NewOAuthProvider builds.
const ExchangeAudience = "legion-broker-exchange"

// githubAudience is the audience every Rig's token exchange takes a workflow run's token for.
// The Rig's own issuer stands in for GitHub's, as it stands in for the cluster's: a test mints
// both, and nothing here reaches the real one.
const githubAudience = "legion-broker-github"

// APIAccount is the ServiceAccount every Rig's BROKER_API_ACCOUNTS names, as the caller
// "dispatch": the identity its UI helper presents on the routes Dispatch's server calls.
const APIAccount = "system:serviceaccount:example-ns:example-dispatch"

// APICaller is the name that account calls as, the one the broker logs when it acts.
const APICaller = "dispatch"

// ListenerAccount is the second ServiceAccount every Rig's BROKER_API_ACCOUNTS names, as the
// caller "listener": the Envoy listener, which reads one of the broker's access tokens back
// through introspection and relays no person's decision. A Rig lists it so a test can drive a
// listed caller reaching past its own routes, which is 403 ACCOUNT_LACKS_POWER.
const ListenerAccount = "system:serviceaccount:example-ns:example-listener"

// ListenerCaller is the name that second account calls as.
const ListenerCaller = "listener"

// The one account every Rig's BROKER_EXCHANGE_ACCOUNTS lists and what a pod running as it may
// write: ExchangeSession is the session name the entry fixes, so a token minted for that pod
// carries it and a request naming any other is refused.
const (
	ExchangeAccount = "system:serviceaccount:example-ns:example-worker"
	ExchangeProject = "EXAMPLE"
	ExchangePerson  = "ada@example.com"
	ExchangeSession = "example-worker"
)

// The second account every Rig's BROKER_EXCHANGE_ACCOUNTS lists: its entry fixes no session, as
// a Legion issue pod's does, so each pod of it trades only for a session a registrar registered for
// that pod (POST /v1/oauth/exchange/pod-sessions), with the key the registration names. Every
// Rig's BROKER_POD_REGISTRARS lists DaemonAccount as a registrar of its pods.
const ExchangeRolesAccount = "system:serviceaccount:example-ns:example-roles"

// The one workflow every Rig's BROKER_EXCHANGE_WORKFLOWS lists and what a run of it may write. It
// fixes no session, so a run names its own.
const (
	ExchangeRepository = "example-org/example-repo"
	ExchangeWorkflow   = ".github/workflows/release.yaml"
	ExchangeRef        = "refs/heads/main"
)

// BrokerAccount is the broker's own Kubernetes ServiceAccount, which a Rig built WithProjectAccess
// lists in its read entries (BROKER_EXCHANGE_READ_ACCOUNTS) as the subject and as the service, the
// one Dispatch's DISPATCH_ACCESS_ACCOUNTS names, with BrokerSession fixed as its session: the read
// token the broker issues itself to ask Dispatch's access routes with.
const (
	BrokerAccount = "system:serviceaccount:dispatch:envoy-broker"
	BrokerSession = "broker-access"
	// brokerPodUID is the pod the broker's projected token is bound to, as a read entry requires.
	brokerPodUID = "broker-pod-uid"
)

// Rig is a live broker HTTP server (real handlers, real Postgres) plus a direct handle to its
// store — enough to drive a machine login, a session enrollment, and an agent-secret request
// end to end over genuine HTTP, and to reach into the store directly for what the API itself has
// no route to do (e.g. forcing a launcher credential's expiry). A machine login is decided on one
// of the routes Dispatch's server calls, with UIToken as the caller's credential and Operator
// named in the body as the approver; a secret request on the broker's own approval page, by
// Operator signed in at the Rig's provider (Decide).
type Rig struct {
	// URL is the broker's own httptest.Server base URL: every proof's htu and every request
	// object's aud must check against this one value.
	URL string
	// Operator is the approver, named by email as Dispatch names a signed-in person, also the
	// owner of the fixture secrets below and the operator of the sessions a test enrolls.
	Operator string
	// OperatorFile already holds Operator, trimmed — ready to hand to a helper.Broker as its
	// OperatorFile field.
	OperatorFile string
	// UIToken is the ServiceAccount token this Rig's APIAccount presents on the routes Dispatch's
	// server calls (UI). It is minted once, for an hour, so a test may send it from a goroutine
	// of its own where failing t is not allowed.
	UIToken string
	// ListenerToken is ListenerAccount's own token, minted the same way: a listed caller that may
	// introspect and nothing else, for a test driving what a caller's powers refuse.
	ListenerToken string
	Store         *store.Store
	// Secrets is the secrets.Local standing in for Secrets Manager, which a test writes to as a
	// person would, before asking the broker to reread a secret.
	Secrets *secrets.Local
	// Registry holds the policy's two metrics as cmd/broker/main.go builds them on its own
	// registry: broker_policy_store_reads_total, counted from the first load on, and
	// broker_unservable_secrets, registered once that load answered; and, for a Rig built
	// WithProjectAccess, the hold gate's broker_hold_unchecked_total. A test reads them through
	// Registry.Handler(), the scrape main serves.
	Registry *metrics.Registry
	// ReadToken, ProjectAccess and Hold are, for a Rig built WithProjectAccess, the broker's own
	// read token at Dispatch, the Dispatch client that presents it, and the hold gate on that
	// client, as cmd/broker builds the three; nil otherwise.
	ReadToken     *projectaccess.ReadToken
	ProjectAccess *projectaccess.Client
	Hold          *hold.Gate
	// Policy is the broker's live policy, for a test that reloads it as the broker's five-minute
	// reload does (Refresh): the rig's own reload runs once an hour, so never within a test.
	Policy *policy.Current
	// podIssuer and podKey back every token this Rig mints — a pod's enrolment token, a service
	// account's login token, and a subject token for the exchange: a local OIDC issuer this Rig's
	// own verifier (wired into its enroll.Service and its api.Deps by NewRig) trusts, and the
	// signing key they are minted under.
	podIssuer *oidctest.Issuer
	podKey    *oidctest.Key
	// serviceLogins is the broker's live BROKER_SERVICE_LOGINS: DaemonAccount, as legion-daemon.
	serviceLogins map[string]string
}

// Option changes how NewRig wires its broker.
type Option func(*rigConfig)

type rigConfig struct {
	lease         time.Duration
	maxConns      int
	minters       map[string]requests.Minter
	projectAccess *ProjectAccess
	// holdRequiresDeadline is WithHoldRequiresDeadline: the gate's RequireDeadline.
	holdRequiresDeadline bool
	personSignIns        *personSignInConfig
}

// ProviderOption changes the OAuth provider NewOAuthProvider constructs. It stays variadic so
// existing rigs retain the default issuer and exchange entries unchanged.
type ProviderOption func(*providerConfig)

type providerConfig struct {
	projectAccess       projectaccess.Access
	hold                *hold.Gate
	google              *oidctest.Issuer
	machineAccount      string
	machineCredentials  oauth.MachineCredentials
	personSignInRecords oauth.PersonSignIns
	// exchange is the token exchange's entries NewRig built and gives the API too; nil has
	// NewOAuthProvider build its own (newExchangeSet).
	exchange *exchange.Set
}

type personSignInConfig struct {
	google         *oidctest.Issuer
	machineAccount string
}

// WithLease sets the enrollment lease the broker grants, an hour by default, for a test whose
// launcher must renew a session within it.
func WithLease(lease time.Duration) Option {
	return func(c *rigConfig) { c.lease = lease }
}

// WithPoolMaxConns caps the broker's database pool at n connections (pgx's pool_max_conns), for a
// test that starves it; pgx's default otherwise.
func WithPoolMaxConns(n int) Option {
	return func(c *rigConfig) { c.maxConns = n }
}

// WithMinters gives the broker a minter for each type minters names, as cmd/broker/main.go's
// typeMinters does, so a test releases a type secret through a minter aimed at its own stub
// upstream; none by default.
func WithMinters(minters map[string]requests.Minter) Option {
	return func(c *rigConfig) { c.minters = minters }
}

// ProjectAccess is where a Rig built WithProjectAccess asks Dispatch, as cmd/broker's settings
// say it.
type ProjectAccess struct {
	// URL is Dispatch's canonical URL (BROKER_PROJECT_ACCESS_URL), which every proof names.
	URL string
	// DialURL is the address the client connects to instead (BROKER_PROJECT_ACCESS_DIAL_URL);
	// empty connects to URL.
	DialURL string
	// TradeDialURL is where the read token's trades connect, empty for the rig's own server, as
	// cmd/broker's connect to the broker's own listener: a test that delays them puts a proxy of
	// its own there. Each trade's proof names the rig's own URL whatever this says.
	TradeDialURL string
	// Now is the clock the client's 30-second cache of an answer (projectaccess.CacheTTL), the
	// gate's memories and the read token's life are timed by, so a test can move it past an
	// answer it would otherwise wait out; nil is the wall clock, as main runs.
	Now func() time.Time
}

// WithProjectAccess wires the rig's broker to Dispatch as cmd/broker wires one whose
// BROKER_PROJECT_ACCESS_URL is set: the broker's own read token, traded at the rig's token endpoint
// for a projected token of BrokerAccount under its read entry, the token exchange's Dispatch client
// that presents it, and the hold gate built on that client and the rig's store, given to every field
// of the broker that takes it. The read token is traded at the first call that needs it; the rig
// runs no renewal loop of its own.
func WithProjectAccess(access ProjectAccess) Option {
	return func(c *rigConfig) { c.projectAccess = &access }
}

// WithHoldRequiresDeadline sets the hold gate's deadline rule (hold.Config.RequireDeadline), as
// cmd/broker sets it where BROKER_HOLD_REQUIRES_DEADLINE is true, which an IAP deployment's broker
// sets: Dispatch's answer that a person is not held then serves only with the deadline such a
// Dispatch puts on it (not_held_until), a person of whom the broker keeps no answer is not served
// while Dispatch cannot be asked, and every refusal of a held person says the Workspace Directory
// ends the hold (hold.Gate.HeldWords). NewRig fails t when it is given without WithProjectAccess,
// as the broker's boot refuses the setting without BROKER_PROJECT_ACCESS_URL: that broker builds
// no gate, so the rule would rule nothing and hold nobody.
func WithHoldRequiresDeadline() Option {
	return func(c *rigConfig) { c.holdRequiresDeadline = true }
}

// WithPersonSignIns lets the rig accept Google person trades from machineAccount. NewRig binds
// the option to its real enrollment and store functions once those exist, so a successful trade
// creates the same launcher credential and person-sign-in rows as the broker command.
func WithPersonSignIns(google *oidctest.Issuer, machineAccount string) Option {
	return func(c *rigConfig) {
		c.personSignIns = &personSignInConfig{google: google, machineAccount: machineAccount}
	}
}

// NewRig holds two secrets the person ada@example.com owns: TEST_SECRET, human-tier, which a
// request from any session makes an approval request to her, and TEST_AUTO_SECRET, agent-tier,
// which her own sessions get at once. It wires a real pod verifier (a local OIDC issuer trusted by
// an enroll.K8sPodVerifier, mirroring cmd/broker/main.go's own wiring), which is also what
// authenticates the listed caller of the routes Dispatch's server calls, and mounts api.Register
// on an httptest.Server as cmd/broker/main.go and api_test.go's newTestServer mount it, but with
// no answer recorder in front (answer.Middleware): its answers carry no answer id, and its
// Registry counts none of them. It skips t when BROKER_TEST_DATABASE_URL is unset
// (storetest.Open's own contract).
func NewRig(t *testing.T, options ...Option) *Rig {
	t.Helper()
	config := rigConfig{lease: time.Hour}
	for _, option := range options {
		option(&config)
	}
	if config.holdRequiresDeadline && config.projectAccess == nil {
		t.Fatal("brokertest: WithHoldRequiresDeadline needs WithProjectAccess: the deadline rules the hold of refused people, which a broker builds only when it has a Dispatch to ask, as the broker's boot refuses BROKER_HOLD_REQUIRES_DEADLINE without BROKER_PROJECT_ACCESS_URL")
	}
	var params []string
	if config.maxConns > 0 {
		params = append(params, "pool_max_conns="+strconv.Itoa(config.maxConns))
	}
	st := storetest.Open(t, params...)
	operator := "ada@example.com"
	fixtures := []secrets.LocalSecret{
		policytest.Secret("TEST_SECRET", operator, policy.TierHuman, "test-secret-v1"),
		policytest.Secret("TEST_AUTO_SECRET", operator, policy.TierAgent, "test-auto-secret-v1"),
	}
	local := secrets.NewLocal(fixtures...)
	// The policy's metrics as main wires them: the store-read counter before the first load,
	// which NewCurrent takes, and the gauge of unservable secrets once NewCurrent has answered.
	registry := metrics.New()
	cur := policytest.MeteredCurrent(t, local, registry)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	podVerifier, err := oidc.New(context.Background(), issuer.URL(), podAudience)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}

	serviceLogins := map[string]string{DaemonAccount: "legion-daemon"}
	// BROKER_SERVICES: the account every pod MintPodToken mints a token for runs as, bound to the
	// service DaemonAccount logs in as, which is what lets that login enrol those pods
	// (enroll.Service.podUnderTokenLogin).
	servicesByAccount := map[string]string{WorkerAccount: "legion-daemon"}
	// BROKER_POD_REGISTRARS: DaemonAccount, the one registrar, listed for WorkerAccount and both
	// accounts BROKER_EXCHANGE_ACCOUNTS lists, so a registration a test makes as that daemon reaches
	// the step it is about rather than 403 POD_REGISTRAR_NOT_LISTED: the exchange policy's refusal
	// of a WorkerAccount pod (POD_ACCOUNT_NOT_LISTED) or of an ExchangeAccount pod, whose entry
	// fixes its session (SESSION_FIXED), and the write itself for an ExchangeRolesAccount pod.
	podRegistrars := api.PodRegistrars{}
	for _, pod := range []string{WorkerAccount, ExchangeRolesAccount, ExchangeAccount} {
		podRegistrars[api.PodRegistrar{Registrar: DaemonAccount, Pod: pod}] = struct{}{}
	}
	enr := &enroll.Service{Store: st, Lease: config.lease, Pod: enroll.K8sPodVerifier{Verifier: podVerifier}, ServiceLogins: serviceLogins, ServicesByAccount: servicesByAccount}
	enr.Chain = enroll.NewChainVerifier(st, srv.URL, time.Minute)
	// A session name is held here as the real broker holds one with no account paired
	// (oauthstore.Store.SetSessionPairs never called, BROKER_SESSION_ACCOUNT_PAIRS unset), so it
	// belongs to one principal: the binder is a store on the same pool the OAuth provider's own
	// store uses (NewOAuthProvider, built below), so a name the token exchange binds and a name an
	// enrollment binds are the one table's.
	enr.Sessions = oauthstore.New(st.Pool)

	reqMachine := &requests.Machine{
		Store: st, Policy: cur, Secrets: policytest.Store(local), PendingTTL: 12 * time.Hour,
		Audience: srv.URL, Skew: time.Minute, Replay: enr.Replay, Minters: config.minters,
	}
	reqMachine.Chain = requests.NewChainVerifier(st, srv.URL, time.Minute, reqMachine.Airlock)
	// The token exchange's entries, built once and given to both the OAuth provider and the API
	// (Deps.Exchange, which GET /v1/person-access counts), as main gives both the one set.
	google, machineAccount := config.personSignIns != nil, ""
	if google {
		machineAccount = config.personSignIns.machineAccount
		// BROKER_VM_SERVICES, the one map main gives the enrollment service and the request
		// machine: the service newExchangeSet's Google entry names. Without it no launcher
		// credential a trade mints for this machine, a person's sign-in's among them,
		// authenticates at a launcher route (enroll.Service.AuthenticateLauncher).
		vmServices := map[string]string{machineAccount: "leaver-machine"}
		enr.VMServicesByAccount, reqMachine.VMServicesByAccount = vmServices, vmServices
	}
	entries := newExchangeSet(t, google, machineAccount, config.projectAccess != nil)
	providerOptions := []ProviderOption{func(c *providerConfig) { c.exchange = entries }}
	// The broker's own read token and its Dispatch client, and the hold gate on that client, as
	// main builds them where BROKER_PROJECT_ACCESS_URL is set: this rig's store and its registry,
	// with the deadline rule where WithHoldRequiresDeadline sets it, as
	// BROKER_HOLD_REQUIRES_DEADLINE does. Without WithProjectAccess there is none, and every field
	// that takes one is nil, which holds nobody.
	var (
		gate      *hold.Gate
		readToken *projectaccess.ReadToken
		access    *projectaccess.Client
	)
	if pa := config.projectAccess; pa != nil {
		readToken, access = newProjectAccess(t, *pa, srv.URL, issuer, podKey)
		gate = hold.New(hold.Config{Access: access, Store: st, Log: slog.Default(), Metrics: registry, Now: pa.Now, RequireDeadline: config.holdRequiresDeadline})
		providerOptions = append(providerOptions, func(c *providerConfig) {
			c.projectAccess = access
			c.hold = gate
		})
	}
	if signIns := config.personSignIns; signIns != nil {
		providerOptions = append(providerOptions, func(c *providerConfig) {
			c.google = signIns.google
			c.machineAccount = signIns.machineAccount
			c.machineCredentials = tokenMachineCredentials(enr)
			c.personSignInRecords = brokerPersonSignIns()
		})
	}
	provider := NewOAuthProvider(t, st, issuer, providerOptions...)
	mach := &machine.Service{
		Store: st, Enroll: enr, Policy: cur,
		Audience: srv.URL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		OAuth: provider, Replay: enr.Replay,
		ApprovalsWithoutGeneration: machine.NewApprovalsWithoutGeneration(registry),
	}

	api.Register(mux, api.Deps{
		PublicURL: srv.URL,
		APIAccounts: map[string]api.Account{
			APIAccount:      Caller(t, APICaller),
			ListenerAccount: Caller(t, ListenerCaller),
		},
		// The one verifier serves both, as the broker's own wiring does: a pod enrolling and a
		// listed caller present tokens of the same issuer for the same audience.
		AccountTokens: enroll.K8sPodVerifier{Verifier: podVerifier},
		Enroll:        enr, Machine: reqMachine, MachineLogin: mach, PodRegistrars: podRegistrars,
		OAuth: provider, DeviceLogin: mach.LoginDevice, TokenHooks: mach.TokenHooks(),
		Proof:  &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: cur, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN,
		Approval: NewApprovalPage(t, SignIn(issuer, podKey), srv.URL, st, reqMachine, WithApprovalHold(gate)),
		Hold:     gate,
		Exchange: entries,
	})

	operatorFile := filepath.Join(t.TempDir(), "operator")
	if err := os.WriteFile(operatorFile, []byte(operator), 0o600); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{
		URL: srv.URL, Operator: operator, OperatorFile: operatorFile,
		Store:     st,
		Secrets:   local,
		Registry:  registry,
		ReadToken: readToken, ProjectAccess: access, Hold: gate,
		Policy:    cur,
		podIssuer: issuer, podKey: podKey,
		serviceLogins: serviceLogins,
	}
	rig.UIToken = rig.MintAccountToken(t, APIAccount, time.Hour)
	rig.ListenerToken = rig.MintAccountToken(t, ListenerAccount, time.Hour)
	return rig
}

// newProjectAccess is the broker's own read token, traded at brokerURL, the rig's token endpoint,
// for a projected token of BrokerAccount bound to brokerPodUID for ExchangeAudience, which issuer
// signs with key, written to a file of t's own as the kubelet mounts one; and the Dispatch client
// presenting it at access.URL, connecting to access.DialURL where it is set. A trade connects to
// access.TradeDialURL where that is set, and signs brokerURL either way.
func newProjectAccess(t *testing.T, access ProjectAccess, brokerURL string, issuer *oidctest.Issuer, key *oidctest.Key) (*projectaccess.ReadToken, *projectaccess.Client) {
	t.Helper()
	claims := issuer.Claims(BrokerAccount, ExchangeAudience)
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": brokerPodUID}}
	subjectFile := filepath.Join(t.TempDir(), "broker-exchange-token")
	if err := os.WriteFile(subjectFile, []byte(issuer.Mint(t, key, claims)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := func(address string) *http.Client {
		if address == "" {
			return nil
		}
		parsed, err := url.Parse(address)
		if err != nil {
			t.Fatalf("brokertest: the address %q: %v", address, err)
		}
		return &http.Client{Transport: brokerclient.AtAddress(parsed, http.DefaultTransport)}
	}
	readToken, err := projectaccess.NewReadToken(projectaccess.ReadTokenConfig{
		BrokerURL: brokerURL, HTTP: at(access.TradeDialURL), SubjectFile: subjectFile, Log: slog.Default(), Now: access.Now,
	})
	if err != nil {
		t.Fatalf("brokertest: the broker's read token: %v", err)
	}
	client := projectaccess.New(access.URL, readToken, at(access.DialURL))
	if access.Now != nil {
		client.UseClock(access.Now)
	}
	return readToken, client
}

// Caller is the account a caller name carries, for an api.Deps a test wires itself as well as for
// the Rig's own: the broker's boot reads BROKER_API_ACCOUNTS this way, and a name its table does
// not know fails t here exactly as it would refuse that boot.
func Caller(t *testing.T, name string) api.Account {
	t.Helper()
	account, known := api.AccountFor(name)
	if !known {
		t.Fatalf("api.AccountFor(%q): the broker knows no caller of that name", name)
	}
	return account
}

// MachineLoginURL is the rig broker's machine-login page, the device grant's verification_uri.
const MachineLoginURL = "https://dispatch.example/credentials/machine"

const (
	// GoogleMachineAudience is the audience a machine's metadata identity token carries when the
	// rig accepts a Google machine or a person signing in on that machine.
	GoogleMachineAudience = "legion-broker-google-machine"
	// PersonSignInAudience is the audience of the person's Google device-flow identity token.
	PersonSignInAudience = "legion-broker-person-sign-in"
)

const personSignInDomain = "example.com"

// newExchangeSet is the token exchange's entries a rig's broker trades on, the one set cmd/broker
// builds from its exchange settings: the two accounts and the one workflow the Rig's constants
// name; for a rig that accepts Google person trades (google), the Google entry for machineAccount
// on the sign-in domain; and for a rig that asks Dispatch (reads), the broker's own read entry,
// BrokerAccount as subject and service with BrokerSession fixed.
func newExchangeSet(t *testing.T, google bool, machineAccount string, reads bool) *exchange.Set {
	t.Helper()
	var machines []exchange.GoogleEntry
	signInDomain := ""
	if google {
		machines = []exchange.GoogleEntry{{Account: machineAccount, Service: "leaver-machine"}}
		signInDomain = personSignInDomain
	}
	var readEntries []exchange.ReadEntry
	if reads {
		readEntries = []exchange.ReadEntry{{Account: BrokerAccount, Service: BrokerAccount, Session: BrokerSession}}
	}
	set, err := exchange.NewSet(
		[]exchange.AccountEntry{
			{Account: ExchangeAccount, Project: ExchangeProject, Person: ExchangePerson, Session: ExchangeSession},
			{Account: ExchangeRolesAccount, Project: ExchangeProject, Person: ExchangePerson},
		},
		readEntries,
		[]exchange.WorkflowEntry{{Repository: ExchangeRepository, Workflow: ExchangeWorkflow, Ref: ExchangeRef, Project: ExchangeProject, Person: ExchangePerson}},
		machines,
		signInDomain,
	)
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	return set
}

// NewOAuthProvider builds the machine login's OAuth provider on st's database as
// cmd/broker/main.go does: device codes live as long as a pending machine login, access tokens
// fifteen minutes, refresh tokens as long as a launcher credential, and verification_uri is
// MachineLoginURL. Its token exchange reads issuer for both kinds of subject token — a pod's, for
// ExchangeAudience, and a workflow run's, for githubAudience — and lists the two accounts and the
// one workflow the Rig's constants name, so an exchange test has an identity to be, a registered
// one, and an unlisted one to be refused as.
func NewOAuthProvider(t *testing.T, st *store.Store, issuer *oidctest.Issuer, opts ...ProviderOption) *oauth.Provider {
	t.Helper()
	var config providerConfig
	for _, opt := range opts {
		opt(&config)
	}
	oauthStore := oauthstore.New(st.Pool)
	oauthStore.Reused = enroll.RevokeReusedCredential
	secret, err := oauthStore.Secret(context.Background())
	if err != nil {
		t.Fatalf("OAuth secret: %v", err)
	}
	policy := config.exchange
	if policy == nil {
		policy = newExchangeSet(t, config.google != nil, config.machineAccount, false)
	}
	keys, err := oidc.NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("oidc.NewKeys: %v", err)
	}
	exchangeConfig := oauth.ExchangeConfig{
		Policy:     policy,
		Kubernetes: oauth.IssuerConfig{Issuer: issuer.URL(), Audience: ExchangeAudience, Keys: keys},
		GitHub:     oauth.IssuerConfig{Issuer: issuer.URL(), Audience: githubAudience, Keys: keys},
		Projects:   config.projectAccess,
		Hold:       config.hold,
	}
	if config.google != nil {
		googleKeys, err := oidc.NewKeys(context.Background(), config.google.URL())
		if err != nil {
			t.Fatalf("Google keys: %v", err)
		}
		exchangeConfig.Google = oauth.IssuerConfig{Issuer: config.google.URL(), Audience: GoogleMachineAudience, Keys: googleKeys}
		exchangeConfig.MachineCredentials = config.machineCredentials
		exchangeConfig.PersonAudience = PersonSignInAudience
		exchangeConfig.PersonSignIns = config.personSignInRecords
	}
	provider, err := oauth.New(oauth.Config{
		Secret: secret, DeviceLifetime: 15 * time.Minute, AccessLifetime: 15 * time.Minute,
		RefreshLifetime: 7 * 24 * time.Hour, VerificationURL: MachineLoginURL, ProofSkew: time.Minute,
		Exchange: exchangeConfig,
	}, oauthStore)
	if err != nil {
		t.Fatalf("oauth.New: %v", err)
	}
	return provider
}

// tokenMachineCredentials is the real broker-store writer cmd/broker gives Google token trades.
func tokenMachineCredentials(enr *enroll.Service) oauth.MachineCredentials {
	return func(ctx context.Context, cred oauth.MachineCredential) (string, error) {
		tx, ok := oauthstore.TxFrom(ctx)
		if !ok {
			return "", errors.New("broker test rig: a machine credential is minted in the exchange transaction")
		}
		id, err := enr.IssueTokenLoginTx(ctx, tx, enroll.TokenLogin{
			Subject: cred.Subject, Service: cred.Service, Host: cred.Host, Operator: cred.Operator,
			TokenExpires: cred.Expires, Thumbprint: cred.Thumbprint, JWK: cred.JWK, Expires: cred.Expires,
		})
		if store.IsUniqueViolation(err) {
			return "", fmt.Errorf("%w: %v", oauth.ErrMachineKeyTaken, err)
		}
		if err != nil {
			return "", err
		}
		return id.String(), nil
	}
}

// brokerPersonSignIns is the real broker-store writer cmd/broker gives person trades.
func brokerPersonSignIns() oauth.PersonSignIns {
	return func(ctx context.Context, in oauth.PersonSignIn) error {
		tx, ok := oauthstore.TxFrom(ctx)
		if !ok {
			return errors.New("broker test rig: a person sign-in is recorded in the exchange transaction")
		}
		err := enroll.RecordPersonSignIn(ctx, tx, store.SignInTrade{
			Person: in.Person, Thumbprint: in.Thumbprint, MachineAccount: in.Machine,
			Account: store.IssuerSubject{Issuer: in.Issuer, Subject: in.Subject},
		})
		switch {
		case errors.Is(err, store.ErrSignInSubjectReplaced):
			return fmt.Errorf("%w: %v", oauth.ErrSignInSubjectReplaced, err)
		case errors.Is(err, store.ErrSignInRevoked):
			return fmt.Errorf("%w: %v", oauth.ErrSignInRevoked, err)
		case errors.Is(err, store.ErrSignInMachineChanged):
			return fmt.Errorf("%w: %v", oauth.ErrSignInMachineChanged, err)
		case errors.Is(err, store.ErrSignInSubjectChanged):
			return fmt.Errorf("%w: %v", oauth.ErrSignInSubjectChanged, err)
		}
		return err
	}
}

// request issues method against baseURL+path through client, with an optional body (marshaled to
// JSON) and headers, and answers the status and the body bytes. It asserts no status itself: a
// caller does that, so a mismatch names the actual body.
//
// It is the one request body in this package: Rig.Req and Client.Req differ in the broker they
// name, the client they go out on, and at — the process a failure is about, " (at host:port)"
// for a Client pinned to one of two processes serving one database, and empty for a Rig, which
// has one broker and so nothing to tell apart.
func request(
	t *testing.T,
	client *http.Client,
	baseURL, method, path string,
	headers map[string]string,
	body any,
	at string,
) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s%s: %v", method, path, at, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body of %s %s%s: %v", method, path, at, err)
	}
	return resp.StatusCode, respBody
}

// Req issues method against path on the real broker with an optional body (marshaled to JSON)
// and headers, returning the decoded status and body bytes. It never asserts a status itself: a
// caller does that so a mismatch names the actual body.
func (r *Rig) Req(t *testing.T, method, path string, headers map[string]string, body any) (int, []byte) {
	t.Helper()
	return request(t, http.DefaultClient, r.URL, method, path, headers, body, "")
}

// UI is Req as the listed caller of the routes Dispatch's server calls: it presents UIToken, the
// ServiceAccount token this Rig minted for APIAccount, which is the credential Dispatch's own
// server presents in production.
func (r *Rig) UI(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	return r.Req(t, method, path, map[string]string{"Authorization": "Bearer " + r.UIToken}, body)
}

// uiCall is the UI-route caller a machine-login decision goes out through: Rig.UI or Client.UI,
// which differ in the broker they reach and the bearer they carry.
type uiCall func(t *testing.T, method, path string, body any) (int, []byte)

// decideMachineLogin decides the pending machine login whose confirmation code is code, through
// ui, as the operator does on Dispatch's machine-login page: it looks the login up by that code,
// then approves or denies it naming approver as the approver. It returns the launcher credential
// an approval minted, "" for a denial, and fails t on any other answer. at names the process a
// failure is about, as request's does.
func decideMachineLogin(t *testing.T, ui uiCall, approver, code string, approve bool, at string) (credentialID string) {
	t.Helper()
	status, body := ui(t, http.MethodPost, "/v1/machine-logins/lookup", map[string]any{"code": code})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/machine-logins/lookup%s = %d: %s", at, status, body)
	}
	var looked struct {
		RecordID string `json:"record_id"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(body, &looked); err != nil || looked.State != "pending" || looked.RecordID == "" {
		t.Fatalf("POST /v1/machine-logins/lookup%s answered %s (%v), want a pending record", at, body, err)
	}
	action, want := "deny", "denied"
	if approve {
		action, want = "approve", "approved"
	}
	status, body = ui(t, http.MethodPost, "/v1/credential-requests/"+looked.RecordID+"/"+action,
		map[string]any{"approver": approver, "code": code})
	if status != http.StatusOK {
		t.Fatalf("%s %s's machine login%s = %d: %s", action, approver, at, status, body)
	}
	var decided struct {
		State        string  `json:"state"`
		CredentialID *string `json:"credential_id"`
	}
	if err := json.Unmarshal(body, &decided); err != nil || decided.State != want ||
		approve != (decided.CredentialID != nil && *decided.CredentialID != "") {
		t.Fatalf("%s %s's machine login%s answered %s (%v), want state %s and a credential id only for an approval",
			action, approver, at, body, err, want)
	}
	if decided.CredentialID == nil {
		return ""
	}
	return *decided.CredentialID
}

// Decide decides the pending secret request recordID as Operator on the broker's own approval
// page, as the operator does: signed in at the Rig's provider, from the record's page
// (brokertest.Decide).
func (r *Rig) Decide(t *testing.T, recordID string, approve bool) {
	t.Helper()
	Decide(t, r.URL, r.podIssuer, r.Operator, recordID, approve)
}

// DecideMachineLogin decides the pending machine login whose confirmation code is code, naming
// this Rig's Operator as the approver.
func (r *Rig) DecideMachineLogin(t *testing.T, code string, approve bool) (credentialID string) {
	t.Helper()
	return decideMachineLogin(t, r.UI, r.Operator, code, approve, "")
}

// MintPodToken mints a projected service-account token bound to podUID — the shape
// enroll.K8sPodVerifier.Verify reads — signed by this Rig's own local OIDC issuer, which the
// Rig's enroll.Service trusts as its PodVerifier (wired in NewRig). Mirrors
// internal/broker/enroll/enroll_test.go's own mintPodToken helper.
func (r *Rig) MintPodToken(t *testing.T, podUID string) string {
	t.Helper()
	claims := r.podIssuer.Claims(WorkerAccount, podAudience)
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(raw, &merged); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	merged["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	return r.podIssuer.Mint(t, r.podKey, merged)
}

// DaemonAccount is the ServiceAccount every Rig's BROKER_SERVICE_LOGINS lets log in as
// legion-daemon with its token (POST /v1/launcher-credentials/token), as a Legion daemon's own
// account does, and the one registrar every Rig's BROKER_POD_REGISTRARS lists: for WorkerAccount,
// ExchangeRolesAccount and ExchangeAccount, so its token registers a pod of any of the three at
// POST /v1/oauth/exchange/pod-sessions and no other account's does.
const DaemonAccount = "system:serviceaccount:legion-system:legion-daemon-test"

// WorkerAccount is the ServiceAccount the pods MintPodToken mints tokens for run as, which every
// Rig's BROKER_SERVICES binds to the service DaemonAccount logs in as, as a Legion worker pod's
// account is bound to legion-daemon. BROKER_POD_REGISTRARS lists DaemonAccount for it and no
// BROKER_EXCHANGE_ACCOUNTS entry lists it, so a registration of one of its pods is refused by the
// exchange policy (403 POD_ACCOUNT_NOT_LISTED), past the route's own checks.
const WorkerAccount = "system:serviceaccount:legion:worker"

// MintAccountToken mints a projected ServiceAccount token for account, expiring lifetime from now
// (a negative lifetime mints one already expired), signed by this Rig's own local OIDC issuer for
// its audience: what a daemon's token file holds. Bound to no pod, as `kubectl create token` mints.
func (r *Rig) MintAccountToken(t *testing.T, account string, lifetime time.Duration) string {
	t.Helper()
	claims := r.podIssuer.Claims(account, podAudience)
	now := time.Now()
	claims["iat"] = now.Add(min(lifetime, 0) - 5*time.Second).Unix()
	claims["exp"] = now.Add(lifetime).Unix()
	return r.podIssuer.Mint(t, r.podKey, claims)
}

// MintAccountPodToken is MintAccountToken bound to podUID: the projected ServiceAccount token a
// pod's own kubelet mounts, which names both the account it runs as and the pod it is. A service
// that logs in with one of these logs in as that pod rather than as the account at large, which
// is what lets several pods of one account hold logins at the same time.
func (r *Rig) MintAccountPodToken(t *testing.T, account, podUID string, lifetime time.Duration) string {
	t.Helper()
	claims := r.podIssuer.Claims(account, podAudience)
	now := time.Now()
	claims["iat"] = now.Add(min(lifetime, 0) - 5*time.Second).Unix()
	claims["exp"] = now.Add(lifetime).Unix()
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	return r.podIssuer.Mint(t, r.podKey, claims)
}

// MintSubjectToken mints a pod's projected ServiceAccount token for account, bound to podUID and
// carrying this Rig's exchange audience: the subject token POST /v1/oauth/token takes for the
// Kubernetes token type. A podUID of "" mints one bound to no pod, which the exchange refuses.
func (r *Rig) MintSubjectToken(t *testing.T, account, podUID string) string {
	t.Helper()
	claims := r.podIssuer.Claims(account, ExchangeAudience)
	if podUID != "" {
		claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": podUID}}
	}
	return r.podIssuer.Mint(t, r.podKey, claims)
}

// MintWorkflowToken mints a continuous-integration workflow run's token as GitHub mints one —
// the repository, the workflow file's ref, the git ref and the run id the exchange decides by —
// signed by this Rig's own issuer for its GitHub audience. The claims name the listed workflow
// unless a caller gives another repository, workflow or ref.
func (r *Rig) MintWorkflowToken(t *testing.T, repository, workflow, ref, runID string) string {
	t.Helper()
	claims := r.podIssuer.Claims("repo:"+repository+":ref:"+ref, githubAudience)
	workflowRef := repository + "/" + workflow + "@" + ref
	claims["repository"] = repository
	claims["workflow_ref"] = workflowRef
	claims["job_workflow_ref"] = workflowRef
	claims["ref"] = ref
	claims["run_id"] = runID
	return r.podIssuer.Mint(t, r.podKey, claims)
}

// DropServiceLogin takes account off this Rig's BROKER_SERVICE_LOGINS, as an operator removing its
// line and redeploying the broker does.
func (r *Rig) DropServiceLogin(account string) {
	delete(r.serviceLogins, account)
}
