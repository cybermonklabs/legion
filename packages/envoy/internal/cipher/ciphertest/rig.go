// packages/envoy/internal/cipher/ciphertest/rig.go

// Package ciphertest mounts the real broker (api.Register) — real Postgres store, real
// enroll/machine-login/request services — on an httptest.Server, for any package that needs to
// drive a genuine broker end to end rather than a hand-rolled fake. It is the extraction
// api_test.go's own newTestServer names as its reference pattern. Every consumer of a full
// real-broker rig imports this package instead of duplicating the wiring.
package ciphertest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/policy/policytest"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/ratelimit"
	"github.com/sjawhar/envoy/internal/testwait"
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

// The third account every Rig's BROKER_EXCHANGE_ACCOUNTS lists: a Legion daemon's own pod, whose
// entry fixes DaemonExchangeSession, the session a daemon of ExchangeProject writes Dispatch
// under, and names the Rig's approver (Operator) as its person, since Dispatch refuses every
// write of a token that names none (SIGN_IN_REQUIRED). It is an account of its own rather than
// DaemonAccount, which a Rig lists for no exchange: the registration route refuses a pod token of
// an account the exchange does not list (POD_ACCOUNT_NOT_LISTED), and a test of that refusal
// presents the daemon's own login token.
const (
	DaemonExchangeAccount = "system:serviceaccount:legion-system:legion-daemon-exchange"
	DaemonExchangeSession = "legion-daemon:" + ExchangeProject
)

// The fourth account every Rig's BROKER_EXCHANGE_ACCOUNTS lists: a pod of a second project,
// OtherProject, whose entry fixes OtherProjectSession and names the Rig's approver as its person,
// so a test holds a token that Dispatch serves for its person and that names a project other
// than ExchangeProject.
const (
	OtherProjectAccount = "system:serviceaccount:other-ns:other-project-worker"
	OtherProject        = "OTHER"
	OtherProjectSession = "other-project-worker"
)

// The one workflow every Rig's BROKER_EXCHANGE_WORKFLOWS lists and what a run of it may write. It
// fixes no session, so a run names its own.
const (
	ExchangeRepository = "example-org/example-repo"
	ExchangeWorkflow   = ".github/workflows/release.yaml"
	ExchangeRef        = "refs/heads/main"
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
	// EdgeURL is the base URL of the broker's edge, the second httptest.Server WithEdge mounts
	// api.RegisterEdge on: every request object and proof made there names it in place of URL.
	// It is "" for a Rig with no edge.
	EdgeURL string
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
	// Registry holds the policy's two metrics as cmd/cipherd/main.go builds them on its own
	// registry: broker_policy_store_reads_total, counted from the first load on, and
	// broker_unservable_secrets, registered once that load answered. A test reads them through
	// Registry.Handler(), the scrape main serves.
	Registry *metrics.Registry
	// Policy is the broker's live policy, for a test that reloads it as the broker's five-minute
	// reload does (Refresh): the rig's own reload runs once an hour, so never within a test.
	Policy *policy.Current
	// podIssuer and podKey back every token this Rig mints — a pod's enrolment token, a service
	// account's login token, and a subject token for the exchange: a local OIDC issuer this Rig's
	// own verifier (wired into its enroll.Service and its api.Deps by NewRig) trusts, and the
	// signing key they are minted under.
	podIssuer *oidctest.Issuer
	podKey    *oidctest.Key
	// serviceLogins is the broker's live BROKER_SERVICE_LOGINS: DaemonAccount, as legion-daemon,
	// and each account WithPersonPod names, as the service it names, each from the one issuer the
	// Rig trusts.
	serviceLogins map[string]enroll.ServiceLogin
	// podOperators is the broker's live BROKER_POD_OPERATORS: each account WithPersonPod names,
	// answering its person, and no account by default. It is one map, which the enroll.Service
	// and the requests.Machine share, and which the machine.Service reads through that
	// enroll.Service. So a change SetPodOperator makes reaches all three.
	podOperators map[string]string
}

// Option changes how NewRig wires its broker.
type Option func(*rigConfig)

type rigConfig struct {
	lease          time.Duration
	maxConns       int
	minters        map[string]requests.Minter
	accessLifetime time.Duration
	projectAccess  projectaccess.Access
	personPods     []personPod
	profiles       string
	edge           bool
	// edgeDeviceLimit is the edge's bucket every caller of a device authorization shares
	// (api.Edge.DeviceLimit); nil is api.DefaultEdgeDeviceLimit.
	edgeDeviceLimit *ratelimit.Limit
	// edgeDeviceLogin wraps the edge's device login (api.Edge.DeviceLogin); nil is the login as
	// it is.
	edgeDeviceLogin func(api.DeviceLoginFunc) api.DeviceLoginFunc
}

// EdgeMachineLoginURL is the page a device authorization made at a Rig's edge sends its person
// to (BROKER_EDGE_MACHINE_LOGIN_URL), and EdgeWorkspaceDomain the one domain a Rig's edge counts
// as the Workspace's beside SignInDomain (BROKER_SIGNIN_WORKSPACE_DOMAINS).
const (
	EdgeMachineLoginURL = "https://dispatch.edge.example/credentials/machine"
	EdgeWorkspaceDomain = "second.example"
)

// WithEdge gives the broker its edge, as BROKER_EDGE_LISTEN_ADDR does: api.RegisterEdge on an
// httptest.Server of its own at Rig.EdgeURL, over the same Deps the Rig's own address serves,
// sending a person to EdgeMachineLoginURL and refusing a login on SignInDomain or
// EdgeWorkspaceDomain. The Rig's machine-login service, request machine and launcher credentials'
// chain each take EdgeURL as their EdgeAudience, as cmd/cipherd wires them.
func WithEdge() Option {
	return func(c *rigConfig) { c.edge = true }
}

// WithEdgeDeviceLimit is WithEdge with limit as the edge's bucket every caller of a device
// authorization shares, for a test of that bucket that cannot wait for its refill.
func WithEdgeDeviceLimit(limit ratelimit.Limit) Option {
	return func(c *rigConfig) { c.edge, c.edgeDeviceLimit = true, &limit }
}

// WithEdgeDeviceLogin is WithEdge with wrap around the edge's device login (api.Edge.DeviceLogin),
// for a test of what the edge does with a login its pre-check refused and its login opened.
func WithEdgeDeviceLogin(wrap func(api.DeviceLoginFunc) api.DeviceLoginFunc) Option {
	return func(c *rigConfig) { c.edge, c.edgeDeviceLogin = true, wrap }
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

// WithMinters gives the broker a minter for each type minters names, as cmd/cipherd/main.go's
// typeMinters does, so a test releases a type secret through a minter aimed at its own stub
// upstream; none by default.
func WithMinters(minters map[string]requests.Minter) Option {
	return func(c *rigConfig) { c.minters = minters }
}

// WithAccessLifetime makes every access token the broker issues live d, a machine login's and an
// exchanged one's alike, as BROKER_ACCESS_TOKEN_SECONDS sets it; fifteen minutes by default.
func WithAccessLifetime(d time.Duration) Option {
	return func(c *rigConfig) { c.accessLifetime = d }
}

// WithProjectAccess gives the broker access, a client of Dispatch's project access, as
// BROKER_PROJECT_ACCESS_URL wires the broker to Dispatch in cmd/cipherd/main.go: every token
// exchange is held to what access answers about the projects its token names, and the enrollment
// and the request machine ask it, the machine login reading it through that enrollment. By
// default the broker checks no project, and so refuses every trade that names one, as on a broker
// with that setting unset. The rig's secrets carry no project tag, so its policy is the same
// either way.
func WithProjectAccess(access projectaccess.Access) Option {
	return func(c *rigConfig) { c.projectAccess = access }
}

// WithProfiles serves file as the deployment's profiles file, the JSON BROKER_PROFILES_FILE
// names (policytest.ProfilesFile builds one), so a test asks for a profile by its name as a session
// of a deployment that lists it does. Every github-app secret the policy serves is a profile's
// (policy.ReasonGitHubAppNoProfile), so a test that releases one names its profile here. With no
// such option, or with "", the rig has no profiles file, as a deployment that leaves the setting
// unset has none. The rig wires no proxy rules (requests.Machine.Airlock), so a proxy block the
// file holds keeps no value back here.
func WithProfiles(file string) Option {
	return func(c *rigConfig) { c.profiles = file }
}

// personPod is one WithPersonPod: an account that BROKER_POD_OPERATORS maps to person, and the
// service the account logs in as.
type personPod struct {
	account, service, person string
}

// WithPersonPod maps account to person, as a BROKER_POD_OPERATORS entry does, and lets account
// log in as service with its own token, as the BROKER_SERVICE_LOGINS entry does that the setting
// needs for each account it maps. Without the second entry, a token login of account is refused
// ACCOUNT_NOT_LISTED before any rule of the mapping applies. Give the option once for each account
// to map. config.Load refuses a mapped account that BROKER_SERVICES binds, or whose service it
// names, so give an account other than WorkerAccount and a service other than legion-daemon.
func WithPersonPod(account, service, person string) Option {
	return func(c *rigConfig) { c.personPods = append(c.personPods, personPod{account, service, person}) }
}

// NewRig holds two secrets the person ada@example.com owns: TEST_SECRET, human-tier, which a
// request from any session makes an approval request to her, and TEST_AUTO_SECRET, agent-tier,
// which her own sessions get at once. It wires a real pod verifier (a local OIDC issuer trusted by
// an enroll.K8sPodVerifier, mirroring cmd/cipherd/main.go's own wiring), which is also what
// authenticates the listed caller of the routes Dispatch's server calls, and mounts api.Register
// on an httptest.Server as cmd/cipherd/main.go and api_test.go's newTestServer mount it, but with
// no answer recorder in front (answer.Middleware): its answers carry no answer id, and its
// Registry counts none of them. It skips t when BROKER_TEST_DATABASE_URL is unset, and fails it
// when the calling package's TestMain does not run its tests through storetest.Main or
// storetest.Run (storetest.Open's own contract).
func NewRig(t *testing.T, options ...Option) *Rig {
	t.Helper()
	config := rigConfig{lease: time.Hour, accessLifetime: 15 * time.Minute}
	for _, option := range options {
		option(&config)
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
	// Its profiles are WithProfiles's file, or none.
	registry := metrics.New()
	cur := policytest.MeteredProfiledCurrent(t, local, registry, config.profiles)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// The edge's server, made before anything is built on its URL, as srv is for the Rig's own.
	var edgeMux *http.ServeMux
	var edgeURL string
	if config.edge {
		edgeMux = http.NewServeMux()
		edgeSrv := httptest.NewServer(edgeMux)
		t.Cleanup(edgeSrv.Close)
		edgeURL = edgeSrv.URL
	}

	issuer := oidctest.New(t)
	podKey := issuer.PublishKey(t, "signing-key")
	// The one issuer the Rig trusts, as a deployment's BROKER_K8S_OIDC_ISSUER names one cluster: a
	// bare URL, so the issuer has no name.
	podIssuers := []oidc.NamedIssuer{{URL: issuer.URL()}}
	podVerifier := enroll.K8sPodVerifier{Issuers: oidc.NewIssuerSet(podIssuers, podAudience, 0)}

	serviceLogins := map[string]enroll.ServiceLogin{DaemonAccount: {Service: "legion-daemon", Issuers: []string{""}}}
	// BROKER_POD_OPERATORS: one map, empty unless WithPersonPod names an account. An empty map
	// maps no account, as nil does, so a Rig without the option decides as a broker with the
	// setting unset does. Each mapped account logs in with its own token as its service, as
	// config.Load requires, and from the Rig's one issuer, as DaemonAccount does.
	podOperators := map[string]string{}
	for _, pod := range config.personPods {
		serviceLogins[pod.account] = enroll.ServiceLogin{Service: pod.service, Issuers: []string{""}}
		podOperators[pod.account] = pod.person
	}
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
	enr := &enroll.Service{
		Store: st, Lease: config.lease, Pod: podVerifier, Issuers: podIssuers,
		ServiceLogins: serviceLogins, ServicesByAccount: servicesByAccount, PodOperators: podOperators,
		Projects: config.projectAccess,
	}
	enr.Chain = enroll.NewChainVerifier(st, srv.URL, time.Minute)
	enr.Chain.EdgeAudience = edgeURL
	// A session name is held here as the real broker holds one with no account paired
	// (oauthstore.Store.SetSessionPairs never called, BROKER_SESSION_ACCOUNT_PAIRS unset), so it
	// belongs to one principal: the binder is a store on the same pool the OAuth provider's own
	// store uses (NewOAuthProvider, built below), so a name the token exchange binds and a name an
	// enrollment binds are the one table's.
	enr.Sessions = oauthstore.New(st.Pool)

	reqMachine := &requests.Machine{
		Store: st, Policy: cur, Secrets: policytest.Store(local), PendingTTL: 12 * time.Hour,
		Audience: srv.URL, EdgeAudience: edgeURL, Skew: time.Minute, Replay: enr.Replay, Minters: config.minters,
		PodOperators: podOperators, Projects: config.projectAccess,
	}
	reqMachine.Chain = requests.NewChainVerifier(st, srv.URL, time.Minute, reqMachine.Airlock)
	provider := newOAuthProvider(t, st, issuer, config.accessLifetime, config.projectAccess)
	mach := &machine.Service{
		Store: st, Enroll: enr, Policy: cur,
		Audience: srv.URL, EdgeAudience: edgeURL, Skew: time.Minute, PendingTTL: 15 * time.Minute, CredentialLifetime: 7 * 24 * time.Hour,
		OAuth: provider, Replay: enr.Replay,
	}

	deps := api.Deps{
		PublicURL: srv.URL,
		APIAccounts: map[string]api.Account{
			APIAccount:      Caller(t, APICaller),
			ListenerAccount: Caller(t, ListenerCaller),
		},
		// The one verifier serves both, as the broker's own wiring does: a pod enrolling and a
		// listed caller present tokens of the same issuer for the same audience.
		AccountTokens: podVerifier,
		Enroll:        enr, Machine: reqMachine, MachineLogin: mach, PodRegistrars: podRegistrars,
		OAuth: provider, DeviceLogin: mach.LoginDevice, TokenHooks: mach.TokenHooks(),
		Proof:  &proof.Verifier{Skew: time.Minute, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: cur, SecretsStore: secrets.StoreAWS, SecretsPrefix: policytest.Prefix, SecretsKMSKey: policytest.KeyARN,
		Approval: NewApprovalPage(t, SignIn(issuer, podKey), srv.URL, st, reqMachine),
	}
	api.Register(mux, deps)
	if config.edge {
		api.RegisterEdge(edgeMux, deps, api.Edge{URL: edgeURL, VerificationURL: EdgeMachineLoginURL, WorkspaceDomains: []string{SignInDomain, EdgeWorkspaceDomain},
			DeviceLimit: config.edgeDeviceLimit, DeviceLogin: config.edgeDeviceLogin})
	}

	operatorFile := filepath.Join(t.TempDir(), "operator")
	if err := os.WriteFile(operatorFile, []byte(operator), 0o600); err != nil {
		t.Fatal(err)
	}

	rig := &Rig{
		URL: srv.URL, EdgeURL: edgeURL, Operator: operator, OperatorFile: operatorFile,
		Store:     st,
		Secrets:   local,
		Registry:  registry,
		Policy:    cur,
		podIssuer: issuer, podKey: podKey,
		serviceLogins: serviceLogins, podOperators: podOperators,
	}
	rig.UIToken = rig.MintAccountToken(t, APIAccount, time.Hour)
	rig.ListenerToken = rig.MintAccountToken(t, ListenerAccount, time.Hour)
	return rig
}

// Caller is the account a caller name carries, for an api.Deps a test wires itself as well as for
// the Rig's own: the broker's boot reads BROKER_API_ACCOUNTS this way, and a name its table does
// not know fails t here exactly as it would refuse that boot. Its issuers are the ones the boot
// gives a Kubernetes account's entry that names none under one bare BROKER_K8S_OIDC_ISSUER URL,
// as every such rig is wired: that one issuer, which has no name. A Google account's entry names
// no issuer, and nothing reads its issuers.
func Caller(t *testing.T, name string) api.Account {
	t.Helper()
	account, known := api.AccountFor(name)
	if !known {
		t.Fatalf("api.AccountFor(%q): the broker knows no caller of that name", name)
	}
	account.Issuers = []string{""}
	return account
}

// MachineLoginURL is the rig broker's machine-login page, the device grant's verification_uri.
const MachineLoginURL = "https://dispatch.example/credentials/machine"

// DeviceLifetime is how long a device code and its user code the rig broker issues wait for a
// person's decision (oauth.Config.DeviceLifetime), as long as cmd/cipherd's pending machine login.
const DeviceLifetime = 15 * time.Minute

// NewOAuthProvider builds the machine login's OAuth provider on st's database as
// cmd/cipherd/main.go does: device codes live as long as a pending machine login, access tokens
// fifteen minutes, refresh tokens as long as a launcher credential, and verification_uri is
// MachineLoginURL. Its token exchange reads issuer for both kinds of subject token — a pod's, for
// ExchangeAudience, and a workflow run's, for githubAudience — and lists the four accounts and the
// one workflow the Rig's constants name, so an exchange test has an identity to be, a registered
// one, a daemon's fixed session, a second project's, and an unlisted one to be refused as.
func NewOAuthProvider(t *testing.T, st *store.Store, issuer *oidctest.Issuer) *oauth.Provider {
	t.Helper()
	return newOAuthProvider(t, st, issuer, 15*time.Minute, nil)
}

// newOAuthProvider is NewOAuthProvider whose access tokens live accessLifetime and whose trades
// are held to what access answers, nil for no project check.
func newOAuthProvider(t *testing.T, st *store.Store, issuer *oidctest.Issuer, accessLifetime time.Duration, access projectaccess.Access) *oauth.Provider {
	t.Helper()
	oauthStore := oauthstore.New(st.Pool)
	oauthStore.Reused = enroll.RevokeReusedCredential
	secret, err := oauthStore.Secret(context.Background())
	if err != nil {
		t.Fatalf("OAuth secret: %v", err)
	}
	policy, err := exchange.NewSet(
		// Each entry names the rig's one cluster issuer, which, written as a bare URL, has the
		// empty name.
		[]exchange.AccountEntry{
			{Account: ExchangeAccount, Project: ExchangeProject, Person: ExchangePerson, Session: ExchangeSession, Issuers: []string{""}},
			{Account: ExchangeRolesAccount, Project: ExchangeProject, Person: ExchangePerson, Issuers: []string{""}},
			{Account: DaemonExchangeAccount, Project: ExchangeProject, Person: ExchangePerson, Session: DaemonExchangeSession, Issuers: []string{""}},
			{Account: OtherProjectAccount, Project: OtherProject, Person: ExchangePerson, Session: OtherProjectSession, Issuers: []string{""}},
		},
		nil,
		[]exchange.WorkflowEntry{{Repository: ExchangeRepository, Workflow: ExchangeWorkflow, Ref: ExchangeRef, Project: ExchangeProject, Person: ExchangePerson}},
		nil,
		// The rig lists no machine, so there is no box a person could have signed in on and no
		// domain to hold one to.
		"",
	)
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	keys, err := oidc.NewKeys(context.Background(), issuer.URL())
	if err != nil {
		t.Fatalf("oidc.NewKeys: %v", err)
	}
	provider, err := oauth.New(oauth.Config{
		Secret: secret, DeviceLifetime: DeviceLifetime, AccessLifetime: accessLifetime,
		RefreshLifetime: 7 * 24 * time.Hour, VerificationURL: MachineLoginURL, ProofSkew: time.Minute,
		Exchange: oauth.ExchangeConfig{
			Policy:     policy,
			Kubernetes: []oauth.IssuerConfig{{Issuer: issuer.URL(), Audience: ExchangeAudience, Keys: keys}},
			GitHub:     oauth.IssuerConfig{Issuer: issuer.URL(), Audience: githubAudience, Keys: keys},
			Projects:   access,
		},
	}, oauthStore)
	if err != nil {
		t.Fatalf("oauth.New: %v", err)
	}
	return provider
}

// request issues method against baseURL+path through client on ctx, with an optional body
// (marshaled to JSON) and headers, and answers the status and the body bytes. It asserts no
// status itself: a caller does that, so a mismatch names the actual body.
//
// ctx carries the request's one bound, the test binary's deadline less testwait's margin
// (testwait.Context): a broker a loaded machine slows is waited for, and one that never answers
// fails the test there.
//
// It is the one request body in this package: Rig.Req and Client.Req differ in the broker they
// name, the client they go out on, and at — the process a failure is about, " (at host:port)"
// for a Client pinned to one of two processes serving one database, and empty for a Rig, which
// has one broker and so nothing to tell apart.
func request(
	t *testing.T,
	ctx context.Context,
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
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
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
	return request(t, testwait.Context(t), http.DefaultClient, r.URL, method, path, headers, body, "")
}

// UI is Req as the listed caller of the routes Dispatch's server calls: it presents UIToken, the
// ServiceAccount token this Rig minted for APIAccount, which is the credential Dispatch's own
// server presents in production.
func (r *Rig) UI(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	return r.Req(t, method, path, map[string]string{"Authorization": "Bearer " + r.UIToken}, body)
}

// Introspect is what the broker's introspection reads token back as, asked as the Envoy listener
// asks it: POST /v1/oauth/introspect with the token in a form, presenting ListenerToken. It fails
// t on any answer but 200.
func (r *Rig) Introspect(t *testing.T, token string) oauthwire.Introspection {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.URL+"/v1/oauth/introspect", strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		t.Fatalf("build the introspection request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+r.ListenerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/oauth/introspect: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer of POST /v1/oauth/introspect: %v", err)
	}
	var read oauthwire.Introspection
	if err := json.Unmarshal(body, &read); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/oauth/introspect = %d %s, want 200 and an introspection (%v)", resp.StatusCode, body, err)
	}
	return read
}

// NewSession inserts a live session enrollment of operator's, under a launcher credential of its
// own, straight into the Rig's store (InsertSession), and answers it with its key: a session for a
// row that needs a session's proof and not the enrollment route that makes one.
func (r *Rig) NewSession(t *testing.T, operator string) (enrollmentID string, key *ecdsa.PrivateKey) {
	t.Helper()
	return InsertSession(t, r.Store.Pool, operator)
}

// Execer is what InsertSession writes through: a *pgxpool.Pool or a *pgx.Conn on a broker's own
// database.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// InsertSession writes a live session enrollment of operator's, under a launcher credential of its
// own, through db, and answers it with its key: the one session insert for a row that needs a
// session's proof, on a Rig (NewSession) or on a real broker binary's database, and not the
// enrollment route that makes one.
func InsertSession(t *testing.T, db Execer, operator string) (enrollmentID string, key *ecdsa.PrivateKey) {
	t.Helper()
	key = NewSigningKey(t)
	thumbprint, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, credentialID := context.Background(), uuid.New()
	enrollmentID = uuid.NewString()
	if _, err := db.Exec(ctx, `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1,$2,'test-host',$3,'{}'::jsonb, now() + interval '30 days')`, credentialID, operator, thumbprint); err != nil {
		t.Fatalf("insert launcher_credentials: %v", err)
	}
	if _, err := db.Exec(ctx, `insert into enrollments (id, kind, runtime_id, operator, thumbprint, launcher_credential_id, lease_expires_at)
		values ($1,'session',$2,$3,$4,$5, now() + interval '1 hour')`, enrollmentID, "session-"+enrollmentID, operator, thumbprint, credentialID); err != nil {
		t.Fatalf("insert enrollments: %v", err)
	}
	return enrollmentID, key
}

// RereaderOperator is the operator of a session a row makes only to read what a reread answers a
// session: a login of its own, so that no row reading an approver's or an operator's sessions meets
// one of them.
const RereaderOperator = "rereader@example.com"

// Reread asks the broker to reread name as a session of its own (NewSession), the caller the
// reread route answers once the reread is done: whether the broker now serves the name, why not,
// and as which type. A caller with no proof is answered 202 at once, before the reread reads the
// store, so a row that reads what the broker serves, or goes on to ask for the secret, rereads
// this way.
func (r *Rig) Reread(t *testing.T, name string) (int, []byte) {
	t.Helper()
	enrollmentID, key := r.NewSession(t, RereaderOperator)
	path := "/v1/secrets/" + name + "/reread"
	signed, err := proof.Sign(key, enrollmentID, http.MethodPost, r.URL+path, time.Now())
	if err != nil {
		t.Fatalf("proof.Sign: %v", err)
	}
	return r.Req(t, http.MethodPost, path, map[string]string{"Proof": signed}, nil)
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
// (ciphertest.Decide).
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

// DecideMachineLoginAs decides the pending machine login whose confirmation code is code, naming
// approver as the approver, as a person other than the Rig's Operator does on Dispatch's page.
func (r *Rig) DecideMachineLoginAs(t *testing.T, approver, code string, approve bool) (credentialID string) {
	t.Helper()
	return decideMachineLogin(t, r.UI, approver, code, approve, "")
}

// MintPodToken mints a projected service-account token bound to podUID — the shape
// enroll.K8sPodVerifier.Verify reads — signed by this Rig's own local OIDC issuer, which the
// Rig's enroll.Service trusts as its PodVerifier (wired in NewRig). Mirrors
// internal/cipher/enroll/enroll_test.go's own mintPodToken helper.
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
// account is bound to legion-daemon. BROKER_POD_REGISTRARS lists DaemonAccount for it, and no
// entry of BROKER_EXCHANGE_ACCOUNTS, BROKER_EXCHANGE_READ_ACCOUNTS or BROKER_EXCHANGE_JOB_ACCOUNTS
// lists it, so a registration of one of its pods is refused by the exchange policy (403
// POD_ACCOUNT_NOT_LISTED), past the route's own checks.
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

// SetPodOperator changes account's entry in this Rig's BROKER_POD_OPERATORS, as an operator
// editing that entry and redeploying the broker does: a person maps account to that person, and
// "" drops the entry. The broker reads the setting at each decision, so the next call that reads
// it sees the change. A login that a pod made before the change keeps its operator until the pod
// logs in again. account stays in BROKER_SERVICE_LOGINS.
func (r *Rig) SetPodOperator(account, person string) {
	if person == "" {
		delete(r.podOperators, account)
		return
	}
	r.podOperators[account] = person
}
