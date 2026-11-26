// Command broker is the secrets broker: it enrolls agent sessions and pods, decides their secret
// requests by policy or, for a request that needs a person, on its own approval page, where the
// approver signs in to the broker itself (BROKER_SIGNIN_*), over a signed credential-request
// record, and releases granted values. Its HTTP API is documented at
// https://cybermonklabs.github.io/legion/broker/reference/api/. The broker opens no Dispatch ask
// and issues nothing in Dispatch: Dispatch's server lists pending requests and links each to the
// broker's page, and relays a machine login's decision with its typed code. The broker reads
// Dispatch in two places, each with its own projected ServiceAccount token: its model judge,
// whether an issue is open (BROKER_JUDGE_DISPATCH_URL), and its project reads, whether a person
// may see the projects a token names and which repositories a project's issues resolve to, which
// it relays to an edge listener (BROKER_PROJECT_ACCESS_URL).
package main

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/approval"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/groups"
	"github.com/sjawhar/envoy/internal/cipher/judge"
	"github.com/sjawhar/envoy/internal/cipher/leasetime"
	"github.com/sjawhar/envoy/internal/cipher/machine"
	"github.com/sjawhar/envoy/internal/cipher/natscallout"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/cipher/projectrepos"
	"github.com/sjawhar/envoy/internal/cipher/proof"
	"github.com/sjawhar/envoy/internal/cipher/proxy"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/requests"
	"github.com/sjawhar/envoy/internal/cipher/reread"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
	"github.com/sjawhar/envoy/internal/cipher/store"
	"github.com/sjawhar/envoy/internal/cipher/wake"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/redact"
)

// machineLoginPendingTTL bounds how long a typed-code machine login waits for a human to decide
// it before the Sweeper expires it. Not a BROKER_* config knob (the shared broker contract's
// Configuration deltas name none for it): 15 minutes comfortably covers the "look at the
// terminal, type the code" UX the confirmation-code flow is built around.
const machineLoginPendingTTL = 15 * time.Minute

// agentSecretPendingTTL bounds how long an agent_secret request waits for approval before the
// Sweeper expires it. Unchanged from the pre-v9 poller's own hardcoded value.
const agentSecretPendingTTL = 12 * time.Hour

// policyRefresh is how often the broker rereads the namespace: every agent secret's owner and tier
// tags, its key, and whether it has a current value.
const policyRefresh = 5 * time.Minute

// rereadListenWait bounds how long the bind waits for the reread listener's first successful
// LISTEN. What it waits on is pglisten.Listener's OnConnected, which runs only once the LISTEN
// statement itself has returned, so while this waits the pod either becomes one the fan-out can
// reach or the bound runs out: a pod whose dial or whose LISTEN failed waits the whole of it,
// logs one warning and serves anyway, which is the other half of what the bound is for - it is
// what keeps a database that takes the connection and then says nothing from holding the boot.
//
// Five seconds is insurance, not a figure a boot spends: in twenty measured boots the LISTEN was
// in place before the wait was reached at all. What a boot that did reach it loses is the
// notifications sent in that window, which costs that one pod its own five-minute reload
// (policyRefresh) and nothing else.
const rereadListenWait = 5 * time.Second

// oauthSecretTimeout bounds oauthstore.Store.Secret, one of the boot calls that run after the
// bind: an insert ... on conflict do nothing and a select on a one-row table. It needs a bound
// of its own because it runs on serving, which no signal cancels - a pod the load balancer may
// already be sending requests to has to drain rather than die mid-request - and a hang there is
// a pod holding a bound listening socket with no srv.Serve behind it until the kubelet's SIGKILL
// at the end of the grace period. Ten seconds is far above any contention two statements against
// a one-row table ever meet, and far below the 60-second shutdownTimeout the grace period
// already has to cover, so it can only fire on a database that has stopped answering.
//
// The headroom is measured, not assumed. Two sessions running these two statements at the same
// moment against a fresh database - the contention a cold two-pod boot makes on
// oauth_provider_secret, where both pods insert the row and one of them loses - take 2.7-6.3 ms
// for the session that waits and 0.8-1.5 ms for the one that does not, over five runs: three
// orders of magnitude inside this bound. The caveat belongs with the figure. That was measured
// with psql against a one-row table of the same shape rather than through the broker's own pgx
// call on the migrated schema, so it is the order of magnitude of the contention and not the
// whole cost of the call. oauth_secret_timeout_test.go measures the call itself, and asserts a
// margin rather than a duration.
const oauthSecretTimeout = 10 * time.Second

// clientCheckInterval is how often the broker asks its sign-in provider whether that provider
// still accepts the broker's own client, after the pass it takes at boot. An hour is what the
// answer is worth: a client is deleted or its secret rotated by a person doing it deliberately,
// and the cost of hearing an hour late is an hour of a status page that is out of date, against
// one token request per pod per hour at the provider.
const clientCheckInterval = time.Hour

func main() {
	// The broker's logging (setUpLogging): every line of slog and the log package, masked.
	setUpLogging(os.Stderr)
	// A subcommand is read before flag.Parse, and serves nothing: it is the whole of what this
	// process does. Reading it here is what makes an older image refuse `broker end-sessions`
	// rather than fall through to serving - and so to migrating the database - when a
	// deployment asks it for a subcommand it does not have (cmd/dispatch/main.go's own
	// precedent, runSubcommand there).
	if len(os.Args) > 1 {
		os.Exit(runSubcommand(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
	// The broker takes no flags; parsing refuses any flag given rather than ignoring it.
	flag.Parse()

	ctx, stopNotify := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopNotify()
	// again carries this process's signals, registered here before any boot work rather than
	// inside the drain where it used to be, and held for the life of the process: so "stop it
	// now" works throughout. signal.NotifyContext's own relay goroutine returns as soon as it has
	// cancelled ctx, while its channel stays registered, so without this registration every
	// signal after the first was swallowed instead of taking SIGTERM's default disposition -
	// kill -TERM twice did nothing at all until the boot had finished. Room for two, because
	// "stop it now" is two signals in quick succession and signal.Notify drops what a full
	// channel cannot take.
	again := make(chan os.Signal, 2)
	signal.Notify(again, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(again)
	// serveStarted is closed once the socket is bound and srv.Serve is about to take it, and
	// drainNow is how the watchdog below hands a second signal to the drain. The watchdog is
	// again's only reader, and it decides once, from serveStarted's state, who a second signal
	// belongs to (secondSignalFindsServing): the drain, through drainNow, or the watchdog's own
	// exit. Room for one, so that handing it over never blocks on a drain that is not reading
	// yet - there is never a second hand-over to make room for.
	serveStarted := make(chan struct{})
	drainNow := make(chan struct{}, 1)
	go func() {
		if secondSignalFindsServing(again, serveStarted) {
			// Serving: ending the drain rather than the process is right once there are
			// in-flight requests for srv.Shutdown to finish.
			drainNow <- struct{}{}
			return
		}
		// A second signal read before serveStarted had closed. Some of the boot runs on a
		// context no signal cancels, and all of it runs with no health endpoint answering, so
		// an operator who has asked twice is owed an exit rather than a process that runs on to
		// bind a socket and drain a server that never served. The lines logged above this one
		// say how far the boot had got; the status is non-zero because this is a boot that did
		// not finish, not a clean stop.
		//
		// The line says what the check established and not what the exit finds: main may close
		// serveStarted and start srv.Serve in the two statements between the two, so "had not
		// begun serving when the second signal was read" is true where "has served nothing"
		// would be a claim about an instant this goroutine never observes.
		slog.Error("broker: a second signal arrived while the broker was still starting up, so it is stopping without a drain: it had not begun serving when that signal was read")
		os.Exit(1)
	}()

	// The two settings config.Load reads that no Config field holds, documented here because
	// broker-refgen takes a setting's description from a Config field's own comment or from a
	// comment in this command, and these two are locals of Load: the ExchangeGoogleAccounts
	// entry each one pins is the only thing that reads them.

	// BROKER_VM_PROJECTS: the Google Cloud project each pinned account's machines run in,
	// whitespace-separated <service account email>=<project id> entries, at most one per account
	// BROKER_VM_SERVICES lists and none for any other account. An account named here is named in
	// BROKER_VM_INSTANCES too, or the boot is refused: half a pin says nothing clear about which
	// machines an entry means. Keyed by account. Needs BROKER_GOOGLE_OIDC_AUDIENCE. Unset, it is
	// empty, so every listed account takes a machine wherever it runs — including one whose token
	// carries no Compute Engine claims at all, which is what a token minted through a workload
	// identity pool and a token from a Kubernetes pod's metadata server both carry.

	// BROKER_VM_INSTANCES: the names each pinned account's machines may have,
	// whitespace-separated <service account email>=<instance-name glob> entries, at most one per
	// account BROKER_VM_SERVICES lists and none for any other account. The glob is Go's path.Match
	// syntax (*, ?, and [...] classes, negated with ^) over the characters of an instance name, such
	// as example-vm-*. An account named here is named in BROKER_VM_PROJECTS too, or the boot is
	// refused. Keyed by account. Needs BROKER_GOOGLE_OIDC_AUDIENCE. Unset, it is empty, so no
	// account is pinned.

	cfg, err := config.Load(os.Getenv)
	fatal(err)
	// BROKER_FAKE_SECRETS_FILE: for local development only, a JSON file standing in for the secret
	// store, in Secrets Manager's shape whatever BROKER_SECRETS_STORE says,
	// {"secrets": [{"name", "kms_key_id", "tags", "value", "deleted_at"}]}: the broker lists
	// the agent secrets and reads their values from it instead of from the store, reading the file again
	// on each of those calls, so an edit to it is a write as Secrets Manager would see one. A secret
	// with no "value" is one created without a value, which the broker refuses as no-current-value;
	// one with a "deleted_at" (RFC 3339) is scheduled for deletion, which the broker no longer lists.
	// Its "kms_key_id" names the agent-secrets key as the selected store configures it:
	// BROKER_SECRETS_KMS_KEY_ARN, or BROKER_SECRETS_KMS_KEY_NAME under gcp-secret-manager.
	fakeSecrets := os.Getenv("BROKER_FAKE_SECRETS_FILE")
	fatal(refusePortZeroPublicURLInProduction(cfg.PublicURL, fakeSecrets))
	fatal(refuseHTTPEdgeURLsInProduction(cfg.EdgeURL, cfg.EdgeMachineLoginURL, fakeSecrets))
	// And, in the same breath, the airlock's test-only settings: every one of them is a local
	// run's, so a production start that names any of them is refused here rather than running
	// a broker that mints against a stand-in or dials an address no production proxy may.
	fatal(refuseAirlockTestSettingsInProduction(cfg, fakeSecrets))
	// And the NATS writer hold's switch, in the same breath: BROKER_NATS_HOLD_READ on without the
	// operator who writes the hold row, or without the keys it is signed by, would have the auth
	// service refuse every client of the hub for good, so it refuses this start instead, before
	// anything is migrated. A start with the switch on and no row is no refusal.
	fatal(refuseHoldReadWithoutOperator(cfg))
	// The profiles, read here beside the port-0 refusal: before the database is migrated, before
	// the socket is bound and before anything reaches for AWS, so a pod given a file it cannot
	// read or cannot serve refuses to start instead of running without the profiles its operator
	// believes are in force. The file is read once; a change to it rolls the pods. The airlock's
	// rules are the profiles' proxy blocks, which the proxy port, every release and the judge
	// read alike.
	profiles, err := readProfiles(cfg.ProfilesFile, serviceNames(cfg))
	fatal(err)
	proxyRules := profiles.ProxyRules()
	// The airlock runs only for a profile with a proxy block, so its settings are required only
	// then, and refused the same way: a deployment whose profiles hold none, such as one that
	// serves GitHub Apps through profiles alone, needs no proxy, no certificate authority and no
	// cluster ranges, and binds no proxy port.
	proxied := make([]string, len(proxyRules.Secrets))
	for i, rule := range proxyRules.Secrets {
		proxied[i] = rule.Name
	}
	fatal(cfg.RequireAirlock(proxied))
	// The proxy blocks of kind dpop, checked on every broker whatever capabilities it grants
	// (config.ResultsEntryRules, config.ProxyMintedRules): every broker with an airlock port serves
	// every minted grant of the database it shares, so a pod that grants none still sends a
	// grant's token, and a proof of its own key, to every host a dpop block binds. The results
	// profile DISPATCH_RESULTS_TOKEN is of kind dpop, and result entries need it; each dpop block
	// binds one exact host, a private destination's, with an allow list, in Authorization alone,
	// and no block of another kind binds that host; each destination needs a dpop block to bind
	// its host.
	fatal(config.ResultsEntryRules(proxyRules, len(cfg.ExchangeResultWorkflows) > 0))
	fatal(config.ProxyMintedRules(proxyRules, cfg.ProxyPrivateUpstreams))
	// And the results entry this broker grants and serves result grants under (resultEntryDigest):
	// the profile and the destination the checks above passed, hashed once, which the token
	// endpoint records in each grant it makes and the results kind compares each grant with.
	entryDigest := resultEntryDigest(profiles, cfg.ProxyPrivateUpstreams)
	// And the proxy's certificate authority, built in the same breath and refused the same way:
	// the key beside the certificate, which the proxy port signs each bound host's certificate
	// with, and the certificate itself, which every release of a name a proxy block holds back
	// hands the command as the one thing it trusts the proxy by. It is the last thing about the
	// airlock that can refuse a boot - a file that is not a certificate, a key that is not that
	// certificate's, or a certificate that is no authority would leave every bound host's
	// handshake failing for every command, found out one command at a time - so it is read
	// before the database is migrated, before the socket is bound and before anything reaches
	// for AWS. nil for a deployment with no airlock, which binds no proxy port.
	var airlockCA *proxy.Authority
	if len(proxied) > 0 {
		if airlockCA, err = proxy.NewAuthority(cfg.AirlockCACertFile, cfg.AirlockCAKeyFile); err != nil {
			// Named by the two variables an operator sets, since what the refusal under this
			// is about is the files they name.
			fatal(fmt.Errorf("BROKER_AIRLOCK_CA_CERT_FILE and BROKER_AIRLOCK_CA_KEY_FILE: %w", err))
		}
	}
	// What a release hands the command: the authority's own certificate, encoded from the
	// certificate this process signs with rather than read out of the file again. A file is
	// bytes an operator put there - a combined PEM holding the certificate and its key is one
	// the authority above accepts - and a second reader of it would hand every command whatever
	// else the file holds, the key with which any host's certificate could be signed among it.
	airlockCACertificate := airlockCACertificatePEM(airlockCA)
	// And the extra roots the proxy verifies a service's certificate against on its own calls
	// upstream, which a local stack points at the certificate of its stand-in services. It is
	// read here with the files above and refused the same way: a file that is not a
	// certificate would leave every bound host's call failing.
	airlockTestUpstreamRoots, err := readAirlockTestUpstreamRoots(cfg.AirlockTestUpstreamCAFile)
	fatal(err)
	// The proxy port's own server, built far below once the boot has bound its socket, and none
	// for a deployment with no airlock. It is declared here, and held in a pointer read and
	// written atomically, because the two reread paths below — this pod's own, and a peer's
	// notification — are wired before it is built: the listener's goroutine is already running
	// when it is stored, so the store and those reads are on different goroutines.
	var airlockProxy atomic.Pointer[proxy.Server]
	// dropAirlockName is what one reread of a secret's name does to the proxy, for each name the
	// reread moved the value of (policy.Lookup.Moved): it drops that name's value from every
	// grant the proxy holds values for, so the next request of such a grant reads the value again
	// and mints again from it. The reread of the name is the signal for a stored value, not the
	// policy version: a new version of a secret's value moves neither the policy's version nor
	// the set of names it serves, so nothing else this process sees says that the value the proxy
	// is holding is the old one. Nothing to do when there is no proxy, which is both a deployment
	// with no airlock and the window between the listener starting and the proxy being built.
	dropAirlockName := func(name string) {
		if proxy := airlockProxy.Load(); proxy != nil {
			proxy.DropName(name)
		}
	}
	// The whole stop budget, logged as soon as the configuration naming its first half is loaded,
	// because a deployment's terminationGracePeriodSeconds has to cover it and nothing in this
	// repository can check that it does: the guard this repository would otherwise write — a test
	// in the owning module reading the dependent's rendered manifest — cannot cross repositories,
	// since the Deployment and its grace period live in the deployment's own repository. This line
	// and the operator documentation are that guard's replacement, so total is the figure an
	// operator transcribes: the grace period must cover it in addition to any preStop pause the
	// deployment adds, because the pause and this drain are both paid out of the one grace period.
	drain := time.Duration(cfg.DrainSeconds) * time.Second
	slog.Info("broker stop budget", "drain", wholeSeconds(drain), "shutdown", wholeSeconds(shutdownTimeout),
		"total", wholeSeconds(drain+shutdownTimeout))
	// How the broker signs in to its database, logged before anything reaches for AWS or the
	// database, so an operator reads it from the log rather than the task's environment: rds-iam
	// when each connection signs in with an RDS IAM token the broker mints, and password when it
	// signs in as BROKER_DATABASE_URL and libpq's defaults say (a password in the URL, PGPASSWORD or
	// a passfile, or none for a server that trusts the client).
	signIn := "password"
	if cfg.DatabaseIAM {
		signIn = "rds-iam"
	}
	slog.Info("database sign-in", "method", signIn)
	// Who a person signs in to the approval page as, logged once at boot, or that nobody does
	// (BROKER_SIGNIN=off), so an operator reads from the log which of the two brokers this is.
	// The client's secret is never logged.
	if cfg.SignInOff {
		slog.Info("approval sign-in off", "detail", "BROKER_SIGNIN=off: no approval page is served, and a request a person would approve is denied at once")
	} else {
		slog.Info("approval sign-in on", "issuer", cfg.SignIn.Issuer, "client_id", cfg.SignIn.ClientID, "domain", cfg.SignIn.Domain)
	}
	// The database, and with it the AWS config loaded once when anything needs it: an IAM-form
	// BROKER_DATABASE_URL, whose connections sign in with RDS IAM tokens minted from it, and the
	// Secrets Manager and KMS clients of the AWS store, unless the fake secrets file stands in.
	// The GCP store needs none.
	st, awsCfg, err := openStore(ctx, cfg, fakeSecrets == "" && cfg.SecretsStore == secrets.StoreAWS)
	fatal(err)
	fatal(st.Migrate(ctx))
	// The schema those migrations created their tables in, by which the NATS writer hold's routes
	// and the auth service's read of the hold row name the hold's tables.
	holdSchema, err := migratedSchema(ctx, st)
	fatal(err)
	// This process's one floor of the hold row, which the hold's routes and the auth service's
	// hold view share (api.NATSHold.RowFloor, natscallout.NewHoldView), so neither half trusts a row
	// the other has moved past.
	holdFloor := &signedrecord.Floor{}
	// The secret store, logged with how it signs in so an operator reads it from the log: the
	// fake file (an AWS-shaped store whatever BROKER_SECRETS_STORE says, since the file holds
	// Secrets Manager's fields), Secrets Manager under the AWS SDK's credential chain, or Secret
	// Manager under Application Default Credentials.
	var secretStore secrets.Store
	var credentials string
	switch {
	case fakeSecrets != "":
		local, err := secrets.LocalFromFile(fakeSecrets)
		fatal(err)
		// The file's kms_key_id names the agent-secrets key as the selected store configures it.
		// Under gcp-secret-manager this is still the AWS store over the file, holding the Cloud KMS
		// key name where it would hold an ARN: names follow Secrets Manager's rules and a source
		// is a local ARN, which only a local run sees.
		secretStore, credentials = secrets.AWS{Client: local, KMS: local, KeyARN: cfg.SecretsKMSKey}, "fake secrets file"
	case cfg.SecretsStore == secrets.StoreGCP:
		client, err := secrets.NewGCPClient(ctx)
		fatal(err)
		defer client.Close()
		secretStore, credentials = secrets.NewGCP(client, cfg.SecretsKMSKey), "application-default"
	default:
		sm := secretsmanager.NewFromConfig(awsCfg)
		secretStore, credentials = secrets.AWS{Client: sm, KMS: kms.NewFromConfig(awsCfg), KeyARN: cfg.SecretsKMSKey}, "aws-sdk"
	}
	slog.Info("secret store", "store", cfg.SecretsStore, "credentials", credentials)
	// The one registry every metric this process serves is built on, and the one the metrics
	// port serves (BROKER_METRICS_LISTEN_ADDR). The policy's store-read counter is built on it
	// here, before the loader that counts through it takes the first load, so the boot's own
	// reads are counted with every read after them; broker_unservable_secrets, which reads the
	// live policy, is registered on it once that first load has answered (LoadPolicy).
	registry := metrics.New()
	// The broker's two clients of the Dispatch at BROKER_PROJECT_ACCESS_URL, both nil when it is
	// unset. Project access is read by the token exchange, every enrollment and secret request, an
	// approval, a release and every airlock connection, so one cache of Dispatch's answers serves
	// them all; the repository answer is what GET /v1/project-repositories relays to an edge
	// listener, and a broker that reads none answers every edge listener 503 REPOSITORIES_UNKNOWN.
	// Both present the broker's own projected ServiceAccount token for Dispatch's audience through
	// one reader, read at call time, so a rotation needs no restart and the broker has one Dispatch
	// address and one account there (projectAccessClients). config.Load holds the two settings to
	// both or neither. Built before the loader, which serves a tagged secret, and each profile of
	// one, only when this broker can ask whether its project is private
	// (policy.Loader.ProjectChecks).
	projectAccess, projectRepositories := projectAccessClients(cfg.ProjectAccessURL, cfg.ProjectAccessTokenFile)
	// The broker's one reader of Google group members, which every decision site asks who is a
	// member of a group: the policy's sets (the loader stamps it on each), the request machine,
	// its chain check and the approval page. A full policy load names its groups to it; it reads
	// each once an hour (Start, below, at the first load) with the reader account's token
	// (BROKER_DIRECTORY_ACCOUNT), minted with this pod's own credential.
	groupReader := newGroupReader(cfg, "", &secrets.OwnGoogleCredential{})
	loader := policyLoader(cfg, secretStore, profiles, registry, groupReader)
	loader.ProjectChecks = projectAccess != nil
	// One key source per issuer, read by everything that needs that issuer's keys: the verifiers
	// discoverVerifiers builds and, for the cluster's issuer and Google's, the RFC 8693 exchange
	// below, which resolves a subject token's own key through the same *oidc.Keys. So the boot
	// makes one discovery read per issuer rather than two, one set is held in memory, and a
	// rotation becomes visible to both at the same moment.
	//
	// Discovery is bounded: an issuer that accepts the connection and never answers refuses the
	// boot instead of hanging it with no health endpoint serving.
	discovered, err := discoverVerifiers(ctx, cfg, issuerKeys)
	fatal(err)
	pod := discovered.Pod
	// The verifiers of the NATS auth service's services' platform tokens, read at boot so an entry
	// it could never admit refuses the start rather than every connection. config.Load parsed the
	// list (cfg.NATS.Services); a broker that runs no auth service still answers its edge
	// listeners' entries at GET /v1/project-repositories.
	natsTokens, err := natsCalloutBoot(ctx, cfg, discovered, issuerKeys)
	fatal(err)
	edgeListeners := cfg.NATS.EdgeListeners()

	// This process's own sender id, minted once at boot. Postgres delivers a notification to every
	// session listening on the channel, this pod's own included, so without an id stamped on what
	// it sends a pod would reread every secret it just reread on behalf of a caller. Both halves of
	// the fan-out take it: the notifier stamps it, and the listener drops what carries it.
	senderID := uuid.NewString()
	started, err := boot(ctx, brokerBoot{
		// The policy and its five-minute reload, on serving rather than on ctx so the backstop
		// the fan-out leans on goes on reloading while the pod drains: stopped at the signal it
		// would leave the one pod the load balancer is still choosing with neither the fan-out
		// nor the reload to narrow the tags it serves. One thing about boot changes with it: the
		// first load no longer ends early on a signal that arrives while it is running, so a pod
		// signalled mid-boot finishes that load and then returns without draining at all, there
		// being nothing bound to drain. NewCurrent's own contract is untouched — its
		// loop logs no failed-load line once its context has ended, which is now the end of the
		// shutdown rather than the signal, so the failed-load line still reports only loads that
		// failed while the broker was meant to be serving.
		//
		// Written down so nobody rediscovers it as a hang: one signal does not interrupt this
		// call. A store that never answers the first load leaves it running - it runs on serving,
		// which no signal cancels, and the boot's ctx.Err() check sits after it returns - so a
		// single SIGTERM is noted and nothing more until the load ends. That is safe, and it
		// stays as it is. A pod here has bound nothing, is in no EndpointSlice and serves no
		// caller, so there is no request to answer and nothing to drain by ending it sooner; what
		// does end it is a second signal, which the watchdog goroutine at the top of main reads
		// from `again` and answers with os.Exit(1) (measured: gone within 30 ms of that signal),
		// or the kubelet's SIGKILL at the end of the grace period.
		LoadPolicy: func(serving context.Context) (*policy.Current, error) {
			return loadFirstPolicy(serving, loader, groupReader, registry, firstGroupReadsWait)
		},
		// The receiving half of the reread fan-out, started after the first policy load (it
		// rereads into that policy) and before the bind, so a pod that cannot hold a LISTEN
		// connection still serves: Run redials forever on its own and never refuses the boot.
		// What such a pod loses while it has no connection is the notifications sent in that
		// window — it goes on serving the tags it last read and catches up at its own five-minute
		// reload (policyRefresh), which is the fan-out's documented backstop. It runs on serving,
		// not on the signal's own context: a draining pod is still being sent requests, so it is
		// still a pod a peer's reread has to reach, and a listener stopped at the signal would
		// leave that pod serving the tags it read before the retag for the whole of its drain.
		// The shutdown ends it instead.
		StartListener: func(serving context.Context, current *policy.Current, onConnected func()) {
			go (&reread.Listener{
				Sender:      senderID,
				Dial:        st.Listener,
				OnConnected: onConnected,
				Refresh:     peerReread(current, dropAirlockName),
			}).Run(serving)
		},
		Listen: listenTCP(cfg.ListenAddr),
		// The proxy port, bound by the same boot and only for a deployment that has an
		// airlock: without a proxy block among its profiles no name is ever held back, so there
		// is nothing for a proxy to carry and no port to bind.
		ListenAirlock: airlockListen(cfg, proxyRules),
		// The edge, bound by the same boot and only for a deployment that names one
		// (BROKER_EDGE_LISTEN_ADDR): with none, the broker binds nothing for it.
		ListenEdge: edgeListen(cfg),
		// The metrics port, bound by the same boot after the others, on every deployment.
		ListenMetrics: listenTCP(cfg.MetricsListenAddr),
		ListenWait:    rereadListenWait,
		Catchup:       func(serving context.Context, current *policy.Current) error { return current.Refresh(serving) },
		Log:           slog.Default(),
	})
	fatal(err)
	// A boot signalled before it had bound anything answers no serving context, having logged the
	// stage it had reached: nothing is bound, nothing is serving and no caller is waiting, so
	// returning here is the whole of such a process's shutdown. The boot has already ended its
	// own serving context on that path, so there is nothing to end here either.
	if started.Serving == nil {
		return
	}
	serving, current, listener := started.Serving, started.Policy, started.Listener
	// A configured BROKER_PUBLIC_URL with port 0 is never itself dialable, so it can only be
	// dev-broker.sh's own "derive my public URL from whatever address I actually bind"
	// convention (its BROKER_PUBLIC_URL mirrors BROKER_LISTEN_ADDR=127.0.0.1:0): resolve it from
	// the real bound address before anything checks a request's audience against it. Every
	// audience-consuming construct below (enr.Chain included) is built after this point, so none
	// ever sees the stale placeholder. refusePortZeroPublicURLInProduction (the dev-vs-production
	// gate) already refused this above, right after config.Load — before st.Migrate, any AWS
	// call, and the boot's bind — so reaching here means it's safe to apply.
	if publicURLPortZero(cfg.PublicURL) {
		cfg.PublicURL = "http://" + listener.Addr().String()
	}
	if fakeSecrets != "" && started.AirlockListener != nil {
		if u, parseErr := url.Parse(cfg.AirlockProxyURL); parseErr == nil && u.Port() == "0" {
			cfg.AirlockProxyURL = "http://" + started.AirlockListener.Addr().String()
		}
	}
	slog.Info("broker listening", "addr", listener.Addr().String())
	if started.AirlockListener != nil {
		slog.Info("airlock proxy listening", "addr", started.AirlockListener.Addr().String())
	}
	if started.EdgeListener != nil {
		slog.Info("broker edge listening", "addr", started.EdgeListener.Addr().String(), "url", cfg.EdgeURL,
			"verification_uri", cfg.EdgeMachineLoginURL, "workspace_domains", cfg.WorkspaceDomains())
	}
	// Logged as the API's line is, once its bind has succeeded, so a rig that starts many brokers
	// on port 0 reads each one's own metrics address from its own log.
	slog.Info("broker metrics listening", "addr", started.MetricsListener.Addr().String())

	lease, sweep := time.Duration(cfg.LeaseSeconds)*time.Second, time.Duration(cfg.SweepSeconds)*time.Second
	enr := &enroll.Service{
		Store: st, Lease: lease, Carry: time.Duration(cfg.SessionCarrySeconds) * time.Second, Pod: pod,
		ServiceLogins: cfg.ServiceLogins, ServicesByAccount: cfg.ServicesByAccount,
		VMServicesByAccount: cfg.VMServicesByAccount, PodOperators: cfg.PodOperators, Projects: projectAccess,
	}
	enr.Chain = enroll.NewChainVerifier(st, cfg.PublicURL, time.Duration(cfg.ProofSkewSeconds)*time.Second)
	// A machine login the edge opened is signed for BROKER_EDGE_URL, and the launcher credential
	// its approval mints authenticates through this chain as any other does.
	enr.Chain.EdgeAudience = cfg.EdgeURL
	// The exchange's listed identities, read here, before the request machine, which reads the
	// project an operator's entry maps a pod's or a machine's account to from the same entries
	// the exchange mints from (exchange.Set.MappedProjects), so a token and a release cannot
	// disagree on a credential's project. Job entries share their kinds' entry lists, while result
	// entries remain a distinct workflow kind.
	exchangePolicy, err := exchange.NewSet(slices.Concat(cfg.ExchangeAccounts, cfg.ExchangeJobAccounts), cfg.ExchangeReadAccounts,
		slices.Concat(cfg.ExchangeWorkflows, cfg.ExchangeJobWorkflows), cfg.ExchangeResultWorkflows, cfg.ExchangeGoogleAccounts, cfg.SignIn.Domain)
	fatal(err)
	// The broker's one read of Dispatch, where BROKER_JUDGE_DISPATCH_URL names one: the request
	// machine and the model judge share it, its token cache and its client.
	dispatch := judgeDispatch(cfg)
	reqMachine := newRequestMachine(cfg, st, current, secretStore, enr.Replay, proxyRules, airlockCACertificate, secrets.NewRetry(registry), dispatch, groupReader)
	reqMachine.Projects, reqMachine.Exchange = projectAccess, exchangePolicy
	// The model judge, on ctx rather than on serving: a run writes a grant, so an in-flight one
	// has to end at the first SIGTERM, before the drain, leaving its request pending for the
	// person who decides it today rather than approving it while the pod is going away.
	wireJudge(ctx, cfg, proxyRules, reqMachine, dispatch)
	credentialLifetime := time.Duration(cfg.LauncherCredentialSeconds) * time.Second
	oauthStore := oauthstore.New(st.Pool)
	oauthStore.Reused = enroll.RevokeReusedCredential
	// The one table that says which principal holds a session name is written by both of the
	// things that name a session: the token exchange, at every token it mints, and an
	// enrollment, at every session it enrols (enroll.Service.Sessions). So a name a session
	// writes its Dispatch tokens under and the name its grant follows have one holder, and a
	// name another principal holds is refused either way, unless that principal is an account
	// BROKER_SESSION_ACCOUNT_PAIRS pairs with the claimant's, which takes it either way but for a
	// name the configuration fixes (below).
	enr.Sessions = oauthStore
	// On serving, not ctx, for the same reason the policy's own loads are: this runs after the
	// bind above, so a SIGTERM arriving here finds a pod the load balancer may already be sending
	// requests to, and such a pod has to drain rather than die with "context canceled" where its
	// callers see a reset connection. The signal is still read, by <-ctx.Done() below, which the
	// moment this boot finishes begins the drain it asked for. What no signal cancels still has
	// to be bounded, though, because nothing is serving the socket this pod has already bound:
	// oauthSecretTimeout is that bound, and its own comment says what it is sized against.
	secretCtx, cancelSecret := context.WithTimeout(serving, oauthSecretTimeout)
	oauthSecret, err := oauthStore.Secret(secretCtx)
	cancelSecret()
	fatal(err)
	// The exchange's issuers whose tokens it accepts as a subject token, beside the listed
	// identities read above. Both halves are optional and the grant is composed either way: a
	// broker told about no issuer accepts no subject token at all, which the line below says once
	// at boot rather than leaving an operator to read it out of a refusal.
	exchangeConfig := oauth.ExchangeConfig{Policy: exchangePolicy}
	if cfg.ExchangeK8sAudience != "" {
		// The key source the account verifier already reads: a subject token is the same
		// cluster's projected token, for an audience of its own (BROKER_EXCHANGE_K8S_AUDIENCE),
		// and BROKER_EXCHANGE_K8S_AUDIENCE needs BROKER_K8S_OIDC_ISSUER, so this is never nil.
		exchangeConfig.Kubernetes = oauth.IssuerConfig{Issuer: cfg.K8sOIDCIssuer, Audience: cfg.ExchangeK8sAudience, Keys: discovered.K8sKeys}
	}
	if cfg.GoogleOIDCAudience != "" {
		// The key set discoverVerifiers read for Google, for the same reason the cluster's is
		// shared: BROKER_GOOGLE_OIDC_AUDIENCE is what made it read one, so this is never nil.
		exchangeConfig.Google = oauth.IssuerConfig{Issuer: cfg.GoogleOIDCIssuer, Audience: cfg.GoogleOIDCAudience, Keys: discovered.GoogleKeys}
		exchangeConfig.MachineCredentials = machineCredentials(enr)
	}
	if cfg.SignInDeviceAudience != "" {
		// A person signs in through Google, which is also the issuer of the machine's actor
		// token, so BROKER_SIGNIN_DEVICE_AUDIENCE needs BROKER_GOOGLE_OIDC_AUDIENCE and the two
		// share the key set read above. What a person may be is BROKER_SIGNIN_DOMAIN's to say,
		// which the exchange's policy already holds.
		exchangeConfig.PersonAudience = cfg.SignInDeviceAudience
		exchangeConfig.PersonSignIns = personSignIns()
		slog.Info("google person sign-in", "issuer", cfg.GoogleOIDCIssuer, "audience", cfg.SignInDeviceAudience, "domain", cfg.SignIn.Domain)
	}
	if cfg.ExchangeGitHubIssuer != "" {
		// The GitHub issuer has no second reader, so it keeps a key source of its own, read here
		// rather than at the boot's start: nothing before the bind needs it.
		keys, err := issuerKeys(serving, cfg.ExchangeGitHubIssuer)
		fatal(err)
		exchangeConfig.GitHub = oauth.IssuerConfig{Issuer: cfg.ExchangeGitHubIssuer, Audience: cfg.ExchangeGitHubAudience, Keys: keys}
	}
	if projectAccess != nil {
		exchangeConfig.Projects = projectAccess
		slog.Info("project access check", "state", "on", "url", redact.URL(cfg.ProjectAccessURL))
	} else {
		slog.Info("project access check", "state", "off",
			"detail", "BROKER_PROJECT_ACCESS_URL is unset, so a token or a session naming no project is minted and enrolled as before, a person's trade or session naming one is refused, a secret tagged with a project is refused project-unchecked, and no enrollment or secret request asks what kind of member its person is")
	}
	if cfg.ExchangeK8sAudience == "" && cfg.ExchangeGitHubIssuer == "" && cfg.GoogleOIDCAudience == "" {
		slog.Info("token exchange", "state", "unconfigured", "detail", "no subject token issuer is set, so no pod, workflow run or machine exchanges")
	} else {
		slog.Info("token exchange", "state", "on", "kubernetes", cfg.ExchangeK8sAudience != "", "github", cfg.ExchangeGitHubIssuer != "",
			"accounts", len(cfg.ExchangeAccounts), "read_accounts", len(cfg.ExchangeReadAccounts), "job_accounts", len(cfg.ExchangeJobAccounts),
			"workflows", len(cfg.ExchangeWorkflows), "job_workflows", len(cfg.ExchangeJobWorkflows), "result_workflows", len(cfg.ExchangeResultWorkflows),
			"machines", len(cfg.ExchangeGoogleAccounts), "job_machines", jobMachines(cfg.ExchangeGoogleAccounts))
	}
	logPodRegistrars(cfg)
	// Every claim on a session name, the exchange's and an enrollment's alike, is BindSession's,
	// and it reads two things off the store: the pairs of BROKER_SESSION_ACCOUNT_PAIRS, either of
	// whose accounts takes a name the other holds, set here, and the names the exchange's entries
	// fix, which no pair takes, set by oauth.New below from the policy it is composed with. Both
	// are set before srv.Serve below hands the store its first request, and nothing writes them
	// after, so the handlers read them with no lock.
	paired := make([][2]string, len(cfg.SessionAccountPairs))
	for i, pair := range cfg.SessionAccountPairs {
		paired[i] = [2]string{exchange.AccountPrincipal(pair[0]), exchange.AccountPrincipal(pair[1])}
	}
	oauthStore.SetSessionPairs(paired)
	if len(cfg.SessionAccountPairs) == 0 {
		slog.Info("session account pairs", "state", "off",
			"detail", "BROKER_SESSION_ACCOUNT_PAIRS is unset, so an account is refused a session name another account holds")
	} else {
		written := make([]string, len(cfg.SessionAccountPairs))
		for i, pair := range cfg.SessionAccountPairs {
			written[i] = pair[0] + "=" + pair[1]
		}
		slog.Info("session account pairs", "state", "on", "pairs", strings.Join(written, " "))
	}
	// verification_uri is Dispatch's machine-login page, where the person types the code. With
	// no page (BROKER_MACHINE_LOGIN_URL unset) the provider composes no device grant at all:
	// the broker mounts no device authorization route and its metadata document names no device
	// code, while the token endpoint, the exchange, introspection and revocation are served as
	// ever. The line says which of the two this broker is, so an operator reads it off the boot
	// rather than out of a 404.
	provider, err := oauth.New(oauth.Config{
		Secret: oauthSecret, DeviceLifetime: machineLoginPendingTTL,
		AccessLifetime:  time.Duration(cfg.AccessTokenSeconds) * time.Second,
		RefreshLifetime: credentialLifetime, VerificationURL: cfg.MachineLoginURL,
		ProofSkew: time.Duration(cfg.ProofSkewSeconds) * time.Second,
		Exchange:  exchangeConfig,
		Airlock:   resultAirlock(cfg, proxyRules, entryDigest, airlockCACertificate),
	}, oauthStore)
	fatal(err)
	if provider.DeviceGrant() {
		slog.Info("machine login device grant", "state", "on", "verification_uri", cfg.MachineLoginURL)
	} else {
		slog.Info("machine login device grant", "state", "off", "detail", "BROKER_MACHINE_LOGIN_URL is unset, so no person types a code and POST /v1/oauth/device_authorization is not served")
	}
	mach := &machine.Service{
		Store: st, Enroll: enr, Policy: current,
		Audience: cfg.PublicURL, EdgeAudience: cfg.EdgeURL, Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second,
		PendingTTL: machineLoginPendingTTL, CredentialLifetime: credentialLifetime,
		OAuth: provider, Replay: enr.Replay,
	}
	// The approval page signs a person in with the broker's own client and returns them to
	// BROKER_PUBLIC_URL as resolved above. A broker that signs nobody in (BROKER_SIGNIN=off)
	// builds none, so api.Register mounts no route of it and every one is a 404, and the
	// request machine denies at once what the page would have put to a person.
	var page *approval.Page
	if !cfg.SignInOff {
		page, err = approval.New(approval.Config{
			SignIn: cfg.SignIn, PublicURL: cfg.PublicURL, MachineLoginURL: cfg.MachineLoginURL,
			Store: st, Machine: reqMachine,
		})
		fatal(err)
	}

	var waker func(context.Context, string, string, string)
	if cfg.EnvoyURL != "" {
		w := wake.Envoy{URL: cfg.EnvoyURL, Account: ciphertoken.NewAccountToken(cfg.EnvoyAccountTokenFile), HTTP: &http.Client{Timeout: 10 * time.Second}}
		waker = func(ctx context.Context, enrollmentID, requestID, state string) {
			sid, _ := reqMachine.SessionID(ctx, requestID) // requests.session_id, else the enrollment's
			if sid == "" {
				sid, _ = enr.SessionID(ctx, enrollmentID)
			}
			if sid != "" {
				w.Notify(ctx, sid, requestID, state)
			}
		}
	}
	sweeper := &requests.Sweeper{
		Enrollments: enr, Machine: reqMachine, MachineLogins: mach, OAuth: oauthStore,
		Interval: sweep, Wake: waker,
	}
	go sweeper.Run(ctx)
	// The client check, beside the sweeper and on the same context: at boot and every hour
	// after, one token request asking the sign-in provider whether it still accepts the
	// broker's own client, written into the row GET /signin/status answers. It never refuses a
	// boot and never stops the process - a provider that is down stops nobody signing in later,
	// since a sign-in reads the discovery document on its own path - so it is a goroutine with
	// no error to return. It stops at the signal, as the sweeper does: a draining pod is
	// answering its last requests, not reporting on a provider. A broker that signs nobody in
	// has no client to check.
	if page != nil {
		go runClientChecks(ctx, cfg.SignIn, st)
	}

	mux := http.NewServeMux()
	// Under the GCP store a reread spends Secret Manager's shared metadata quota, so the fleet
	// budget this process divides is the GCP one. api.RereadRate divides it among the pods the
	// serving clock counts below, and moves this process's reread buckets whenever that count
	// changes.
	rereadBudget := api.DefaultRereadFleetBudget
	if cfg.SecretsStore == secrets.StoreGCP {
		rereadBudget = api.GCPRereadFleetBudget
	}
	rereadRate := api.NewRereadRate(rereadBudget, slog.Default())
	// draining is set once, by the signal handler below, and read by every handler goroutine
	// GET /healthz runs on, which is why it is atomic rather than a plain bool.
	var draining atomic.Bool
	// The sending half of the reread fan-out. reread.Notifier returns the error rather than
	// logging it, because what a failed fan-out means is this program's decision, and the decision
	// is one WARN and nothing else: the reread this pod was asked for has already happened and the
	// answer it gives its caller stands, and the peers that did not hear reread this secret at
	// their own next five-minute reload (policyRefresh) anyway. Neither the caller's answer nor the
	// process is worth failing over a notification Postgres would not take.
	notify := reread.Notifier(st.Pool, senderID)
	// A Google caller's verifier is built only where the broker was told about Google, and it
	// is an interface field, so it is assigned through a variable rather than inline: a nil
	// *oidc.Verifier put straight into the field would be an interface that is not nil, and
	// every Google caller would fault instead of being refused ACCOUNT_TOKENS_OFF.
	var googleCallers api.GoogleVerifier
	if discovered.GoogleIdentities != nil {
		googleCallers = api.GoogleTokens{Verifier: discovered.GoogleIdentities}
	}
	// detached counts the rereads the API runs after answering 202, which srv.Shutdown does not
	// wait for; the stop sequence waits for them (brokerStop.WaitDetached).
	detached := &api.Detached{}
	deps := api.Deps{PublicURL: cfg.PublicURL, RereadRate: rereadRate, Detached: detached,
		APIAccounts: cfg.APIAccounts, AccountTokens: pod,
		PodRegistrars: cfg.PodRegistrars, PodRegistrationTokens: discovered.PodRegistration,
		GoogleAPIAccounts: cfg.GoogleAPIAccounts, GoogleAccountTokens: googleCallers,
		EdgeListeners: edgeListeners, ProjectRepositories: projectRepositories,
		Enroll: enr, Machine: reqMachine, MachineLogin: mach,
		OAuth: provider, DeviceLogin: mach.LoginDevice, TokenHooks: mach.TokenHooks(),
		Proof:  &proof.Verifier{Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second, Lookup: enr.Lookup, LookupLauncher: enr.AuthenticateLauncher, Replay: enr.Replay},
		Policy: current, SecretsStore: cfg.SecretsStore, SecretsPrefix: cfg.SecretsPrefix,
		SecretsKMSKey:          cfg.SecretsKMSKey,
		GoogleExchangeAudience: googleExchangeAudience(cfg),
		TrustedProxyHeader:     cfg.TrustedProxyHeader,
		Approval:               page,
		Draining:               draining.Load,
		NotifyReread: func(ctx context.Context, name string, moved []string) {
			// This pod reread the name itself and merged what it read (the reread route's
			// notifyPeers), so the value behind the name may be a new version: what the reread
			// moved of it is dropped from the airlock proxy's own cache before the peers are
			// told.
			for _, affected := range moved {
				dropAirlockName(affected)
			}
			if err := notify(ctx, name); err != nil {
				slog.Warn("broker: the other pods were not told to reread a secret", "secret", name, "error", err)
			}
		},
		// The NATS writer hold's routes, which admit the operator entry alone, with the auth
		// service's own verifier of its token, and store only what one of its keys signed.
		NATSHold: holdRoutes(cfg, st, holdSchema, cfg.NATS.Services, natsTokens, holdFloor),
	}
	api.Register(mux, deps)
	// The answer recorder wraps the mux itself, inside the request deadline, which writes no
	// answer, so it sees every answer a handler of this port writes, whichever writer wrote it:
	// the API's routes, the OAuth provider's library, the approval page and the mux's own 404
	// and 405 (answer.Middleware), and reads each one's route from the request it hands the mux.
	// It sees none of the answers net/http writes itself before any handler runs, such as its
	// 400 for a malformed request, 431 and 417. Its counter is built on the registry the metrics
	// port serves, and the airlock's proxy port counts its answers on it too.
	answers := answer.NewAnswers(registry)
	srv := &http.Server{
		Handler:           withRequestDeadline(answer.Middleware(mux, answers), requestDeadline),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       2 * time.Minute,
	}
	// The airlock's proxy port, for a deployment that has one: the same proxy rules every
	// release reads, the same request machine the API decides with - the proxy's two reads are
	// its AirlockConnection and AirlockValues - and the authority built at the top of main.
	var proxyServer *proxy.Server
	if started.AirlockListener != nil {
		// The pod's own DPoP key: made here, held in this process alone, and the key every token
		// this pod mints for a minted capability's grant is bound to, so such a token is of no use
		// without this pod. Its thumbprint is logged once, never the key, so every minted token's
		// binding can be read back to a pod of the broker.
		podKey, err := proof.NewKey()
		fatal(err)
		podThumbprint, err := proof.Thumbprint(&podKey.PublicKey)
		fatal(err)
		slog.Info("broker: the proxy's DPoP key", "thumbprint", podThumbprint)
		reqMachine.MintedTokens, reqMachine.MintedKey = provider, podThumbprint
		// The capabilities this pod mints and holds: the results capability, its grants read from
		// result_grants, served only under the results entry the token endpoint records in each
		// grant (requests.MintedKind.EntryDigest).
		reqMachine.Minted = []requests.MintedKind{{
			Name: oauthwire.AirlockResultsName, Word: oauthwire.AirlockResultsWord,
			Grants: oauthStore.ResultGrants(), EntryDigest: entryDigest,
		}}
		proxyServer = proxy.New(proxy.Options{
			Rules:   proxyRules,
			Grants:  reqMachine,
			DPoPKey: podKey,
			CA:      airlockCA,
			Idle:    time.Duration(cfg.AirlockIdleSeconds) * time.Second,
			Log:     slog.Default(),
			// broker_answers_total, which counts the proxy's answers under caller class
			// airlock beside the API port's.
			Answers: answers,
			// The broker cluster's own ranges (BROKER_AIRLOCK_CLUSTER_RANGES), which the proxy
			// refuses to dial beside every private address. config.Load has already refused a
			// start in a cluster whose ranges miss its Service range.
			ClusterRanges: cfg.AirlockClusterRanges,
			// The bound hosts whose allowed requests go to an address inside the cluster
			// (BROKER_PROXY_PRIVATE_UPSTREAM), each dialled only where ClusterRanges or another
			// rule refuses every other request; the start above has checked the dpop blocks that
			// bind them.
			PrivateUpstreams: proxyDestinations(cfg.ProxyPrivateUpstreams),
			// The local stack's settings for the proxy, every one of them refused on a
			// production start (refuseAirlockTestSettingsInProduction): where a name is, the
			// addresses it may dial anyway, the roots it trusts upstream, the two negative
			// controls, and the stand-in token authority the azure exchange is held to.
			Resolve:        airlockTestResolver(cfg.AirlockTestResolve),
			Dial:           proxyTestDial(cfg.ProxyTestDialPorts),
			UpstreamRoots:  airlockTestUpstreamRoots,
			DialAllow:      cfg.AirlockTestDialAllow,
			KeepValues:     cfg.AirlockTestKeepValues,
			NoKind:         cfg.AirlockTestNoKind,
			AzureAuthority: cfg.AirlockTestAzureAuthority,
		})
		airlockProxy.Store(proxyServer)
	}
	// The edge's own server, for a deployment that has one, on the socket the boot bound: the
	// device login's two routes and the health check (api.RegisterEdge), on the same request
	// deadline and read and write timeouts as the API, since what it serves is the API's own
	// handlers on a server of its own, and on an idle timeout above its load balancer's 600-second
	// keepalive (newEdgeServer). Its answers are recorded by answer.EdgeMiddleware: their refusal
	// lines name port=edge and their samples port="edge", and the mux's own 404 and 405 write no
	// refusal line. Every request it answers writes its access-log line (edgeHandler).
	var edgeServer *http.Server
	if started.EdgeListener != nil {
		edgeMux := http.NewServeMux()
		api.RegisterEdge(edgeMux, deps, api.Edge{URL: cfg.EdgeURL, VerificationURL: cfg.EdgeMachineLoginURL, WorkspaceDomains: cfg.WorkspaceDomains()})
		edgeServer = newEdgeServer(edgeMux, answers, slog.Default(), srv)
	}
	// The metrics port's own server, on the socket the boot bound: the scrape of the registry
	// every metric above is built on, and the NATS auth service's liveness (metricsHandler). Its
	// timeouts are the API's, since a scraper is a caller like any other to the server.
	var callout atomic.Pointer[natscallout.Service]
	metricsServer := &http.Server{
		Handler:           metricsHandler(registry, &callout),
		ReadHeaderTimeout: srv.ReadHeaderTimeout,
		ReadTimeout:       srv.ReadTimeout,
		WriteTimeout:      srv.WriteTimeout,
		IdleTimeout:       srv.IdleTimeout,
	}
	// From here a second signal belongs to the drain below rather than to the watchdog's exit:
	// the socket is bound and srv.Serve is about to take it, so there are requests to finish and
	// a drain whose job is finishing them. Closing serveStarted before srv.Serve starts is what
	// makes the watchdog's exit line true: a decision that finds it open is one taken before
	// srv.Serve could have answered a single request.
	close(serveStarted)
	// The serving clock starts only now, beside srv.Serve: a beat says a broker is serving, and a
	// boot that fails above (the OAuth secret, the exchange's issuers, the provider) answers
	// nothing, so a fleet crash-looping there must read as no broker serving rather than have its
	// leases that run out swept. It beats on serving, not ctx: this pod still answers renewals
	// through its drain, so it is still a broker serving, and a gap in the beats there would read
	// as an outage.
	clock := &enroll.ServingClock{Enrollments: enr, Interval: sweep, NotServingAfter: leasetime.NotServingSweeps * sweep,
		OnReplicas: rereadRate.SetReplicas}
	go clock.Run(serving)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("broker: serve", "error", err)
			stopNotify()
		}
	}()
	if proxyServer != nil {
		go func() {
			if err := proxyServer.Serve(started.AirlockListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("broker: airlock proxy serve", "error", err)
				stopNotify()
			}
		}()
	}
	if edgeServer != nil {
		go func() {
			if err := edgeServer.Serve(started.EdgeListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("broker: edge serve", "error", err)
				stopNotify()
			}
		}()
	}
	go func() {
		if err := metricsServer.Serve(started.MetricsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("broker: metrics serve", "error", err)
			stopNotify()
		}
	}()
	// The NATS auth service starts once the broker serves, so a connection it admits can trade
	// and introspect against a broker that answers; it answers in the replicas' queue group until
	// the drain begins. BROKER_NATS_SERVICES alone starts none (config.Load), and the record says
	// which, with the edge listeners GET /v1/project-repositories answers either way, and the hub
	// account it places each client in (BROKER_NATS_ACCOUNT).
	if cfg.NATS.Enabled() {
		slog.Info("NATS auth service", "state", "on", "services", len(cfg.NATS.Services), "edge_listeners", len(edgeListeners), "account", cfg.NATS.Account)
		deps := natscallout.Deps{
			Introspect: provider.Introspect, ServiceToken: natsTokens.Verify,
			Log: slog.Default(), Metrics: natscallout.NewMetrics(registry),
		}
		// The writer hold's rule, only with its switch on (BROKER_NATS_HOLD_READ): the hold row is
		// read at every decision, on a pool of its own, and verified against the operator's keys,
		// through one view for this replica, whose floor every decision shares. With the switch off
		// Hold is nil, nothing reads the row and every decision is answered as before.
		if cfg.NATS.HoldRead {
			holdPool, err := openHoldPool(st, cfg.NATS.HoldPoolSize)
			fatal(err)
			defer holdPool.Close()
			deps.Hold = natscallout.NewHoldView(holdReader(holdPool, holdSchema), cfg.NATS.HoldSigningKeys, holdFloor, deps.Metrics)
		}
		service, err := natscallout.Start(ctx, natscallout.Settings{
			URLs: cfg.NATS.URLs, SeedFile: cfg.NATS.SeedFile, IssuerSeedFile: cfg.NATS.IssuerSeedFile,
			XKeySeedFile: cfg.NATS.XKeySeedFile, Services: cfg.NATS.Services, Account: cfg.NATS.Account,
		}, deps)
		fatal(err)
		callout.Store(service)
		go func() {
			if err := service.Ready(ctx); err == nil {
				slog.Info("broker: the NATS auth service is answering", "servers", strings.Join(cfg.NATS.URLs, ","), "services", len(cfg.NATS.Services))
			}
		}()
	} else {
		slog.Info("NATS auth service", "state", "off", "edge_listeners", len(edgeListeners),
			"detail", "BROKER_NATS_URLS is unset, so the broker answers no hub; GET /v1/project-repositories reads the edge-listener entries of BROKER_NATS_SERVICES")
	}

	<-ctx.Done()
	// The auth service leaves the queue group first, so the hub sends the next authorization to
	// a replica that is not draining.
	if service := callout.Load(); service != nil {
		service.Close()
	}
	// nil when this deployment has no airlock, which is what brokerStop reads as "there is no
	// proxy port to drain", and the same for the edge.
	var airlockShutdown, edgeShutdown func(ctx context.Context) error
	if proxyServer != nil {
		airlockShutdown = proxyServer.Shutdown
	}
	if edgeServer != nil {
		edgeShutdown = edgeServer.Shutdown
	}
	stop(context.Background(), brokerStop{
		Drain: drain, DrainNow: drainNow, Draining: draining.Store,
		Shutdown: srv.Shutdown, WaitDetached: detached.Wait, ShutdownAirlock: airlockShutdown, ShutdownEdge: edgeShutdown, ShutdownMetrics: metricsServer.Shutdown,
		EndServing: started.EndServing, CloseStore: st.Close,
		Log: slog.Default(),
	})
}

// secondSignalFindsServing takes the first two signals off again and then answers whether
// serveStarted was closed: true when the second signal belongs to the drain, false when it
// belongs to the watchdog's exit. It is the watchdog's whole decision, kept out of main so a test
// drives it with no process to kill.
//
// The order is the point. It takes both signals before it looks at serveStarted, and then looks
// once without waiting. Waiting on serveStarted and the second signal in one select instead would
// be a select over two ready channels whenever a second signal reached a broker already serving,
// and Go picks among ready cases uniformly at random: some of those stops would os.Exit(1) a
// serving broker, skipping the drain and srv.Shutdown and logging that it had served nothing.
// Read this way, serveStarted's state is the only input, and it only ever moves one way, so once
// it is closed every second signal answers drain.
//
// The first signal is the one signal.NotifyContext has already turned into ctx's cancellation;
// again is sent it too, and taking it off here is what leaves the next one to mean "now". Nothing
// else reads again, so no other reader can take either signal first.
//
// What this establishes is the state at the look and not at what the caller does next: found
// open, the exit two statements later runs while main may already have closed serveStarted and
// started srv.Serve, so what is true of that exit is that the broker had not begun serving when
// the second signal was read. The ERROR line says that and no more. Nothing orders the two, and
// nothing should: this window is between a function returning and the next two statements, where
// the defect it replaces was a choice between two ready cases measured at 39 of 100 on a signal
// pair the kernel cannot merge - orders of magnitude apart, and the exit is what the operator
// asked for twice. A lock or a second check here would be a fix to a window that does not need
// one.
//
// Either answer stops the process. Found open, srv.Serve has not started at the look, since main
// closes serveStarted first, so no request was ever accepted to drop; found closed, a server
// exists or is one statement from existing, and the drain is the right owner for it - it ends at
// once on the hand-over and runs srv.Shutdown.
func secondSignalFindsServing(again <-chan os.Signal, serveStarted <-chan struct{}) bool {
	<-again
	<-again
	select {
	case <-serveStarted:
		return true
	default:
		return false
	}
}

// requestDeadline bounds every request's own context, so no handler outlives its caller by more
// than a Dispatch call's worth of time; writeTimeout sits past it so a handler that honours its
// context always gets to write its answer.
const requestDeadline = 45 * time.Second

// writeTimeout is the http.Server's own WriteTimeout: the request deadline plus the fifteen
// seconds a handler that honours its context needs to write the answer it already has. Go resets
// the write deadline once a request's headers are read, so what it bounds is the exchange after
// the headers and not the whole accepted connection, whose header read has its own
// ReadHeaderTimeout above.
const writeTimeout = requestDeadline + 15*time.Second

// shutdownTimeout is how long the broker waits for its in-flight requests once the drain has run
// out, and it is the write timeout rather than a figure of its own because that is the longest
// exchange the server itself allows a request already past its headers: one admitted in the last
// instant of the drain still gets its whole 45-second handler deadline, and WriteTimeout bounds
// the exchange around it at 60 seconds. A connection still sending headers when the drain ends
// is not what this budget is for: Shutdown closes an idle connection at once and treats one that
// has gone five seconds without its headers as idle (net/http closeIdleConns), so the longest it
// waits for a connection that never sends a request is those five seconds. It is only safe to be
// this long because the deployment's terminationGracePeriodSeconds has to cover it — past that
// the kubelet SIGKILLs the process whatever it believes it is still allowed, and the in-flight
// requests this budget exists to finish are dropped anyway. The stop-budget line main logs at
// boot is how a deployment reads the number it has to cover.
const shutdownTimeout = writeTimeout

// wholeSeconds renders a duration as a whole number of seconds ("65s") rather than as
// time.Duration's own "1m5s", because the stop budget's reader is a deployment setting
// terminationGracePeriodSeconds, which is counted in seconds and nothing else. Every duration it
// is given is a whole number of them.
func wholeSeconds(d time.Duration) string { return strconv.Itoa(int(d/time.Second)) + "s" }

// withRequestDeadline hands next the request with deadline on its context. It writes no answer,
// so it sits outside the answer recorder, which wraps the mux itself.
func withRequestDeadline(next http.Handler, deadline time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), deadline)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func fatal(err error) {
	if err != nil {
		slog.Error("broker: fatal", "error", err)
		os.Exit(1)
	}
}

// openStore opens the broker's database as cfg says, and answers the AWS configuration it loaded
// for it. An IAM-form BROKER_DATABASE_URL signs each new connection in with an RDS IAM token
// minted from that configuration, so it is loaded before the database is opened; awsAlso asks for
// it where the database does not, for the Secrets Manager and KMS clients of the AWS store, and
// with neither the answer is the zero aws.Config. It migrates nothing: the server migrates after
// this, and the end-sessions and versions subcommands must not. All three open the store here, so
// each subcommand reaches the same database and signs in to it the same way as the server.
func openStore(ctx context.Context, cfg config.Config, awsAlso bool) (*store.Store, aws.Config, error) {
	var awsCfg aws.Config
	var err error
	if cfg.DatabaseIAM || awsAlso {
		if awsCfg, err = loadAWSConfig(ctx); err != nil {
			return nil, aws.Config{}, fmt.Errorf("load the AWS configuration: %w", err)
		}
	}
	var opts []store.Option
	if cfg.DatabaseIAM {
		mint, err := rdsAuthTokens(awsCfg)
		if err != nil {
			return nil, aws.Config{}, err
		}
		opts = append(opts, store.WithTokenMinter(mint))
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, opts...)
	if err != nil {
		return nil, aws.Config{}, fmt.Errorf("open the database: %w", err)
	}
	return st, awsCfg, nil
}

// refusePortZeroPublicURLInProduction refuses a BROKER_PUBLIC_URL whose port is 0
// (publicURLPortZero) unless BROKER_FAKE_SECRETS_FILE makes this a local run: port 0 is never
// dialable, so it can only be dev-broker.sh's own "derive my public URL from whatever address I
// actually bind" convention (BROKER_PUBLIC_URL mirrors BROKER_LISTEN_ADDR=127.0.0.1:0). A stray
// ":0" reaching a real deployment, however its zero is written, must fail loudly at boot, never
// silently reinterpret the broker's own public identity as its internal bind address. Extracted
// from main so a test can drive it directly instead of through fatal, which calls os.Exit.
func refusePortZeroPublicURLInProduction(publicURL, fakeSecretsFile string) error {
	if !publicURLPortZero(publicURL) || fakeSecretsFile != "" {
		return nil
	}
	return fmt.Errorf("BROKER_PUBLIC_URL %q: port 0 is never dialable in production (BROKER_FAKE_SECRETS_FILE is unset); only a local run may use it as dev-broker.sh's derive-from-bind convention", redact.URL(publicURL))
}

// refuseHTTPEdgeURLsInProduction refuses a BROKER_EDGE_URL or BROKER_EDGE_MACHINE_LOGIN_URL whose
// scheme is not https unless BROKER_FAKE_SECRETS_FILE makes this a local run, as
// refusePortZeroPublicURLInProduction does. Both are on the path of a person on the internet. Over
// plain http, anyone on the path could rewrite the edge's device authorization, its
// verification_uri and user_code among them, or serve a page of their own where
// BROKER_EDGE_MACHINE_LOGIN_URL sends the person to type the user code while signed in to Dispatch.
// A setting left empty (a broker with no edge) is refused nothing. Extracted from main so a test
// can drive it directly instead of through fatal, which calls os.Exit.
func refuseHTTPEdgeURLsInProduction(edgeURL, machineLoginURL, fakeSecretsFile string) error {
	if fakeSecretsFile != "" {
		return nil
	}
	for _, setting := range []struct{ name, value, what string }{
		{"BROKER_EDGE_URL", edgeURL, "the edge answers callers on the internet"},
		{"BROKER_EDGE_MACHINE_LOGIN_URL", machineLoginURL, "the edge sends a person on the internet to this page to type a user code"},
	} {
		if setting.value == "" {
			continue
		}
		if u, err := url.Parse(setting.value); err == nil && strings.EqualFold(u.Scheme, "https") {
			continue
		}
		return fmt.Errorf("%s %q: %s, so it must be https in production (BROKER_FAKE_SECRETS_FILE is unset); only a local run may use http", setting.name, redact.URL(setting.value), setting.what)
	}
	return nil
}

// publicURLPortZero reports whether publicURL names port 0, its port read as a number the way
// the bind reads BROKER_LISTEN_ADDR's (net.LookupPort), so ":00" is port 0 as ":0" is. The
// production refusal above and main's derive-from-bind rewrite both read it, so the URL one of
// them takes as port 0 is the one the other does. A URL that does not parse, or names no port
// (an empty one, as in "http://host:", included), names no port 0.
func publicURLPortZero(publicURL string) bool {
	u, err := url.Parse(publicURL)
	if err != nil || u.Port() == "" {
		return false
	}
	port, err := net.LookupPort("tcp", u.Port())
	return err == nil && port == 0
}

// refuseAirlockTestSettingsInProduction refuses every one of the local stack's test-only
// settings, the airlock's eight, the result grant's one and the judge's one, unless
// BROKER_FAKE_SECRETS_FILE makes this a local run, as the port-0 refusal above does and for the
// same reason: each of them points the broker at a stand-in, lets it dial an address no
// production proxy may dial, shortens a grant below what production gives it, or turns off a
// check the airlock or the judge is made of, so one reaching a real deployment must fail
// loudly at boot rather than quietly serve a broker that is not the one the operator
// configured. The refusal names the variable it found, since an operator reads which of its
// settings it has to take out. Extracted from main so a test can drive it directly instead of
// through fatal, which calls os.Exit.
func refuseAirlockTestSettingsInProduction(cfg config.Config, fakeSecretsFile string) error {
	if fakeSecretsFile != "" {
		return nil
	}
	for _, setting := range []struct {
		name string
		set  bool
	}{
		{"BROKER_AIRLOCK_TEST_GITHUB_API_BASE", cfg.AirlockTestGitHubAPIBase != ""},
		{"BROKER_AIRLOCK_TEST_GOOGLE_TOKEN_URL", cfg.AirlockTestGoogleTokenURL != ""},
		{"BROKER_AIRLOCK_TEST_DIAL_ALLOW", len(cfg.AirlockTestDialAllow) > 0},
		{"BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE", cfg.AirlockTestUpstreamCAFile != ""},
		{"BROKER_AIRLOCK_TEST_RESOLVE", len(cfg.AirlockTestResolve) > 0},
		{"BROKER_AIRLOCK_TEST_KEEP_VALUES", cfg.AirlockTestKeepValues},
		{"BROKER_AIRLOCK_TEST_NO_KIND", cfg.AirlockTestNoKind},
		{"BROKER_AIRLOCK_TEST_AZURE_AUTHORITY", cfg.AirlockTestAzureAuthority != ""},
		{"BROKER_RESULT_GRANT_TEST_SECONDS", cfg.ResultGrantTestSeconds != 0},
		{"BROKER_JUDGE_TEST_SKIP_CHECKS", cfg.JudgeTestSkipChecks},
	} {
		if setting.set {
			return fmt.Errorf("%s is for the broker's local stack alone and is set on a production start (BROKER_FAKE_SECRETS_FILE is unset); take it out", setting.name)
		}
	}
	return nil
}

// resultAirlock is what the token endpoint hands a result entry's run beside its placeholder
// (oauth.AirlockSettings): the proxy's URL, the authority's certificate, the hosts the proxy block
// of the profile DISPATCH_RESULTS_TOKEN binds, the local stack's BROKER_RESULT_GRANT_TEST_SECONDS,
// and the results entry each grant records (resultEntryDigest). nil when no profile of that name
// has a proxy block, so the provider grants no run a placeholder. Every proxy block binds at least
// one host (proxyrules.Validate), so Lookup answers the bound entry.
func resultAirlock(cfg config.Config, rules proxyrules.Rules, entryDigest, caCertificate string) *oauth.AirlockSettings {
	entry := rules.Lookup(oauthwire.AirlockResultsName)
	if entry == nil {
		return nil
	}
	return &oauth.AirlockSettings{
		ProxyURL: cfg.AirlockProxyURL, CACertificatePEM: caCertificate,
		ResultHosts:   slices.Clone(entry.Hosts),
		EntryDigest:   entryDigest,
		GrantLifetime: time.Duration(cfg.ResultGrantTestSeconds) * time.Second,
	}
}

// readAirlockTestUpstreamRoots is the trust store the proxy verifies a service's certificate
// against on its own calls upstream: the host's own roots with the certificates of
// BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE added, so a local stack's stand-in services verify while
// every real host still verifies as it did. An empty path answers nil, which is the host's
// roots alone. A file that cannot be read or holds no certificate refuses the boot: a run that
// quietly trusted nothing extra would fail one call at a time instead.
func readAirlockTestUpstreamRoots(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// os.ReadFile's own error names the path, so this adds the variable alone.
		return nil, fmt.Errorf("BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE: the host's own roots: %w", err)
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE %s: holds no PEM certificate", path)
	}
	return pool, nil
}

// airlockTestResolver is BROKER_AIRLOCK_TEST_RESOLVE as the proxy's Resolve: the address the
// setting gives a host, and the proxy's own lookup (proxy.ResolveHost) for every host it
// does not name, so a host this setting says nothing about resolves exactly as it would with
// the setting unset. It answers nil for a setting that names none, which leaves the proxy its
// own resolver.
func airlockTestResolver(hosts map[string]netip.Addr) func(context.Context, string) ([]netip.Addr, error) {
	if len(hosts) == 0 {
		return nil
	}
	return func(ctx context.Context, host string) ([]netip.Addr, error) {
		if addr, named := hosts[host]; named {
			return []netip.Addr{addr}, nil
		}
		return proxy.ResolveHost(ctx, host)
	}
}

// proxyDestinations is BROKER_PROXY_PRIVATE_UPSTREAM's entries as the proxy takes them.
func proxyDestinations(upstreams []config.PrivateUpstream) []proxy.PrivateUpstream {
	destinations := make([]proxy.PrivateUpstream, len(upstreams))
	for i, upstream := range upstreams {
		destinations[i] = proxy.PrivateUpstream{Host: upstream.Host, URL: upstream.URL}
	}
	return destinations
}

// proxyTestDial is the port half of BROKER_AIRLOCK_TEST_RESOLVE as the proxy's Dial: a dial to
// an address whose entry named a port goes to that port, whatever port the CONNECT named, so a
// stand-in service on loopback needs no privileged port 443; every other dial is a net.Dialer's,
// as with the setting unset. The proxy hands its Dial an address it resolved and checked, never a
// name, so the port follows the address the check read. It answers nil for a setting that names
// no port, which leaves the proxy its own dialer.
func proxyTestDial(ports map[netip.Addr]uint16) func(context.Context, string, string) (net.Conn, error) {
	if len(ports) == 0 {
		return nil
	}
	var dialer net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if at, err := netip.ParseAddrPort(addr); err == nil {
			if port, named := ports[at.Addr()]; named {
				addr = netip.AddrPortFrom(at.Addr(), port).String()
			}
		}
		return dialer.DialContext(ctx, network, addr)
	}
}

// readProfilesFile reads one profiles file through parse: policy.ParseProfiles for a file about to
// be served, and policy.DecodeProfiles for the file in force that `profiles check` compares a
// change against. A path that cannot be read is os.ReadFile's own error, which names the path,
// and a file parse refuses is that refusal under the file's name, since the refusal names the
// profile and the field, or the parse position, and not the path.
func readProfilesFile(path string, parse func([]byte) (policy.Profiles, error)) (policy.Profiles, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return policy.Profiles{}, err
	}
	profiles, err := parse(data)
	if err != nil {
		return policy.Profiles{}, fmt.Errorf("%s: %w", path, err)
	}
	return profiles, nil
}

// readProfiles reads the profiles file, the path BROKER_PROFILES_FILE names, as the boot serves
// it: policy.ParseProfiles, which holds each profile to the file's shape in the words
// `profiles check` refuses it in too, and then Profiles.CheckRequesters, which reads each
// requester service against the deployment's registered services, a setting `profiles check`
// has no reader of. An empty path is a deployment with no profiles, and answers no profile, no
// proxy rule and a Digest of "" for every name. Anything else refuses the boot: a path that
// cannot be read, a file that is not JSON (whose refusal names the parse position), and a profile
// either reader refuses (whose refusal names the profile and the field). Each refusal names the
// setting beside what the reader said, since the policy package names a profiles file no way
// but by its contents.
func readProfiles(path string, services []string) (policy.Profiles, error) {
	if path == "" {
		return policy.Profiles{}, nil
	}
	profiles, err := readProfilesFile(path, policy.ParseProfiles)
	if err != nil {
		return policy.Profiles{}, fmt.Errorf("BROKER_PROFILES_FILE: %w", err)
	}
	if err := profiles.CheckRequesters(services); err != nil {
		return policy.Profiles{}, fmt.Errorf("BROKER_PROFILES_FILE: %s: %w", path, err)
	}
	return profiles, nil
}

// airlockListen is brokerBoot.ListenAirlock in production: the bind on
// BROKER_AIRLOCK_LISTEN_ADDR, and nil for a deployment whose profiles hold no proxy block, rules
// being their proxy blocks (policy.Profiles.ProxyRules). Nil rather than a bind of the default
// address, because a broker with no airlock holds back no name, so a proxy port there would be a
// port that authenticates nobody and carries nothing.
func airlockListen(cfg config.Config, rules proxyrules.Rules) func() (net.Listener, error) {
	if len(rules.Secrets) == 0 {
		return nil
	}
	return listenTCP(cfg.AirlockListenAddr)
}

// edgeListen is brokerBoot.ListenEdge in production: the bind on BROKER_EDGE_LISTEN_ADDR, and nil
// for a deployment that names no edge, which binds nothing for one.
func edgeListen(cfg config.Config) func() (net.Listener, error) {
	if cfg.EdgeListenAddr == "" {
		return nil
	}
	return listenTCP(cfg.EdgeListenAddr)
}

// peerReread is the reread listener's Refresh (reread.Listener): the reread of name another pod
// told this one to make, current's RefreshOne. A peer served the reread, so this pod's airlock
// proxy may be holding the old value of that name: it drops each name the reread moved
// (policy.Lookup.Moved), by the rule the reread route's own drop follows. The proxy holds a
// grant's values by the name the grant was released under, which for a profile of another name
// is the profile's own, so a reread of a source drops its profiles, and a reread of a profile
// drops its source and its sibling profiles; dropping the reread name alone left the old value in
// every request of the others until their grants ended, and a stored value has no expiry. A type
// secret's held values are tokens minted from its value, which a reread that left that value's
// version alone gives no reason to mint again, and anyone may ask for a reread. A reread whose
// context ended during its value read alone (policy.ErrValueUnchecked) has merged the tags it
// read, which this pod now serves, so it is a reread that ran: it answers nil, and the next load
// reads the value again. Any other error is a reread that did not run, which the listener logs:
// it moved no type secret's value version, so it drops none of a type secret's names, and it
// drops a stored value's as every reread of one does.
func peerReread(current *policy.Current, dropAirlockName func(string)) func(context.Context, string) error {
	return func(ctx context.Context, name string) error {
		lk, err := current.RefreshOne(ctx, name)
		for _, affected := range lk.Moved {
			dropAirlockName(affected)
		}
		if errors.Is(err, policy.ErrValueUnchecked) {
			return nil
		}
		return err
	}
}

// metricsHandler is what the metrics port serves: registry's scrape at GET /metrics in the
// Prometheus text format (the pattern takes HEAD too, as every GET pattern of a ServeMux does),
// and GET /livez, the NATS auth service's liveness, and nothing else - every other path is 404 and
// every method there but GET and HEAD is 405 - so a scraper or a probe reaches no route of the API
// through it, and no caller of the API reaches either (the API's mux registers neither).
//
// GET /livez is natscallout.Livez on the auth service this broker started, which answers the whole
// of it, the broker that runs no auth service included.
func metricsHandler(registry *metrics.Registry, callout *atomic.Pointer[natscallout.Service]) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", registry.Handler())
	mux.Handle("GET /livez", natscallout.Livez(callout.Load))
	return mux
}

// airlockCACertificatePEM is the certificate every airlock release hands a command: the
// authority's own certificate, encoded here from the certificate this process signs with. A
// deployment with no airlock has no authority and answers "".
//
// It is encoded rather than read from BROKER_AIRLOCK_CA_CERT_FILE because that file is whatever
// an operator mounted: a PEM holding the certificate and its private key together is a file the
// authority accepts - tls.LoadX509KeyPair takes the certificate block and leaves the rest - and
// a reader that answered the file's bytes would put that key in every release, with which any
// host's certificate could be signed and would be trusted by every command the broker released
// to. What a command needs is the one certificate it verifies the proxy by, and that is this.
func airlockCACertificatePEM(ca *proxy.Authority) string {
	if ca == nil {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate().Raw}))
}

// newRequestMachine builds the requests.Machine the broker decides every secret request with, from
// its configuration: the audience and skew every request object is checked against, and, for each
// service account, the registered service a pod running as it proves, for each Google
// service account, the one a machine of that fleet proves and the person its exchange entry
// names, for each service account BROKER_POD_OPERATORS maps, the person a pod enrolled under its
// login acts for, and, for each google-service-account secret, the one account the deployment
// binds it to and so the only one its release mints a token for. A grant lasts the lifetime its
// request signed, or until its session ends. rules are the profiles' proxy blocks
// (policy.Profiles.ProxyRules) and caCertificate the airlock's authority, both read at boot: a
// name a proxy block holds is released as placeholders, and every such release carries the
// proxy's URL and this certificate. retry is the one retry policy and process-wide retry budget
// of the three minters that call an upstream over HTTP (main's is secrets.NewRetry on the
// process's registry, which broker_mint_attempts_total is counted on), so a GitHub brownout that
// spends the budget leaves a Google mint's failure unretried too: each holds the one pointer,
// which typeMinters' copies keep. dispatch is the broker's one read of Dispatch (judgeDispatch),
// nil where BROKER_JUDGE_DISPATCH_URL is unset; where it is set it also names a Legion pod's
// issue's assignee, whose Inbox lists that pod's request for a shared secret. groupReader is the
// broker's one reader of group members (newGroupReader), which the machine's decisions, revokes
// and lists and its chain check ask who is a member of a group.
func newRequestMachine(cfg config.Config, st *store.Store, current *policy.Current, secretStore secrets.Store, replay func(context.Context, string, time.Time) (bool, error), rules proxyrules.Rules, caCertificate string, retry *secrets.Retry, dispatch *judge.Dispatch, groupReader record.Directory) *requests.Machine {
	githubApp := secrets.GitHubApp{Base: cfg.AirlockTestGitHubAPIBase, Retry: retry}
	m := &requests.Machine{
		Store: st, Policy: current, Secrets: secretStore, PendingTTL: agentSecretPendingTTL,
		Audience: cfg.PublicURL, EdgeAudience: cfg.EdgeURL, Skew: time.Duration(cfg.ProofSkewSeconds) * time.Second, Replay: replay,
		ServicesByAccount:   cfg.ServicesByAccount,
		VMServicesByAccount: cfg.VMServicesByAccount,
		PodOperators:        cfg.PodOperators,
		VMPersonsByAccount:  vmPersonsByAccount(cfg.ExchangeGoogleAccounts),
		// The two types whose minter calls an endpoint of its own carry the local stack's
		// stand-in endpoints when it named them, and the real ones otherwise: a minter left
		// zero mints against GitHub and Google themselves.
		Minters: typeMinters(
			githubApp,
			secrets.GoogleLogin{TokenURL: cfg.AirlockTestGoogleTokenURL, Retry: retry},
			secrets.AzureLogin{TokenFile: cfg.AzureTokenFile, SecretName: cfg.AzureLoginSecret},
			// The google-service-account minter takes the deployment's binding of each such
			// secret to the one account it may mint for (BROKER_SERVICE_ACCOUNT_SECRETS): the
			// broker holds Token Creator on every bound account, so the binding and not the
			// secret's stored value is what chooses the account, and a secret the deployment
			// binds to none mints nothing. It still impersonates with the broker pod's own
			// Workload Identity credentials, which it finds on its first mint and keeps, and
			// asks Google's own IAM endpoint. It is passed by pointer because that is where it
			// keeps those credentials.
			&secrets.GoogleServiceAccount{Accounts: cfg.ServiceAccountSecrets, Retry: retry},
			// The anthropic-federation minter takes the same binding: the broker may mint the
			// identity token of every bound account, so the binding chooses the account. Each
			// exchange presents an identity token read for it alone (no cache), which it sends
			// once, and counts that one attempt on the same Retry.
			&secrets.AnthropicFederation{
				Accounts:      cfg.ServiceAccountSecrets,
				IdentityToken: (&ciphertoken.GoogleAccounts{Audience: secrets.AnthropicAudience}).Read,
				Retry:         retry,
			}),
		// A GitHub App token a release minted and hands to nobody - a release that failed after
		// the mint, or a name behind the airlock - goes back to GitHub through the same minter's
		// endpoint and retry. A github-installations token has no give-back: it is the token the
		// secret stores, which every other release of it hands over too.
		GiveBacks:            map[string]requests.GiveBack{policy.TypeGitHubApp: githubApp.GiveBack},
		Airlock:              rules,
		AirlockProxyURL:      cfg.AirlockProxyURL,
		AirlockCACertificate: caCertificate,
		// BROKER_SIGNIN=off: nobody signs in to approve, so a name that would wait for a
		// person is denied at once.
		SignInOff: cfg.SignInOff,
		// BROKER_GITHUB_APP_FIELD=off: the github_app field an agent-secrets of a release before
		// --target sends is refused 400 GITHUB_APP_FIELD_REMOVED rather than answered. Unset, it
		// is answered for the requesters and the secrets below, and for nothing else.
		GitHubAppFieldOff: cfg.GitHubAppFieldOff,
		// BROKER_GITHUB_APP_FIELD_REQUESTERS: the registered services and people that field is
		// answered for, separated by commas or spaces. Unset, the broker answers no service at
		// all, and a person asking for their own App is answered whatever it names, so a
		// deployment's own value is what turns the answer on for its pods.
		GitHubAppFieldRequesters: cfg.GitHubAppFieldRequesters,
		// BROKER_GITHUB_APP_FIELD_SECRETS: the stored github-app secrets that field hands over
		// outside the airlock, separated by commas or spaces. Unset, the broker names none, so
		// it answers the field for no secret at all.
		GitHubAppFieldSecrets: cfg.GitHubAppFieldSecrets,
		Groups:                groupReader,
	}
	m.Chain = requests.NewChainVerifier(st, cfg.PublicURL, time.Duration(cfg.ProofSkewSeconds)*time.Second, m.Airlock)
	m.Chain.Groups = groupReader
	if dispatch != nil {
		m.Assignees = dispatch
	}
	return m
}

// projectAccessHTTPClient is the HTTP client the broker reads Dispatch's project access with: Go's
// default transport, but keeping requests.MaySeeAtOnce idle connections to Dispatch rather than
// the default two, since one row, and one list, asks Dispatch that many questions at once. With
// two kept, each round of a row's questions would close all but two of the connections the round
// before it opened, and open as many again.
func projectAccessHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = requests.MaySeeAtOnce
	return &http.Client{Transport: transport}
}

// projectAccessClients is the broker's two clients of the Dispatch at baseURL, both reading the
// broker's own projected ServiceAccount token at tokenFile, read at call time: its project access,
// over projectAccessHTTPClient, and the repository answer GET /v1/project-repositories relays. Both
// are nil, as interfaces, when baseURL is empty, so a broker with no BROKER_PROJECT_ACCESS_URL
// holds none. main wires the broker with these, and the test of the client's connections reads its
// project access through this same call, so a client built here on another transport fails it.
func projectAccessClients(baseURL, tokenFile string) (projectaccess.Access, api.ProjectRepositoriesReader) {
	if baseURL == "" {
		return nil, nil
	}
	account := ciphertoken.NewAccountToken(tokenFile)
	return projectaccess.New(baseURL, account, projectAccessHTTPClient()), projectrepos.New(baseURL, account, nil)
}

// The broker's group reader answers every question a decision site asks of group members.
var _ record.Directory = (*groups.Reader)(nil)

// newGroupReader is the broker's one reader of Google group members: it reads the Directory API
// at BROKER_DIRECTORY_URL (Google's when unset) with the token of the reader account
// BROKER_DIRECTORY_ACCOUNT names, which it mints at IAM's generateAccessToken (iamURL, Google's
// when empty, where a test names its stand-in) with own, this pod's own Google credential. With no
// reader account it reads no group, and every group admits nobody. The first policy load starts it
// on the serving context (loadFirstPolicy).
func newGroupReader(cfg config.Config, iamURL string, own oauth2.TokenSource) *groups.Reader {
	return &groups.Reader{
		BaseURL: cfg.DirectoryURL,
		Source:  groups.NewTokenSource(groups.TokenOptions{Account: cfg.DirectoryAccount, IAMCredentialsURL: iamURL, Default: own}),
		Log:     slog.Default(),
	}
}

// policyLoader is the loader of the broker's policy: the namespace secretStore holds under
// BROKER_SECRETS_PREFIX, the registered services, the service-account bindings, the profiles file,
// the store-read counter on registry, and groupReader, which the loader's Current stamps on every
// set it makes live and names each full load's groups to.
func policyLoader(cfg config.Config, secretStore secrets.Store, profiles policy.Profiles, registry *metrics.Registry, groupReader record.Directory) policy.Loader {
	// The policy serves each profile by its name, checked against its source at every publish,
	// and folds each profile's digest into its version, so an edit of a profile's entry ends that
	// name's grants. With no profiles file it serves no profile and refuses every github-app
	// secret, whose tokens a profile alone scopes.
	return policy.Loader{Store: secretStore, Prefix: cfg.SecretsPrefix, Services: serviceNames(cfg), ServiceAccounts: cfg.ServiceAccountSecrets, Profiles: profiles,
		StoreReads: policy.NewStoreReads(registry), Groups: groupReader}
}

// firstGroupReadsWait bounds how long the first policy load waits for the group reads it started
// before the pod serves: a read is a token mint and a page or two of members, well inside it, and
// a Directory that does not answer holds the boot no longer than this.
const firstGroupReadsWait = 15 * time.Second

// loadFirstPolicy is the broker's first policy load (the boot's LoadPolicy): it starts the group
// reader on serving, as the reload does, so a read a load starts and the hourly read end when the
// broker stops serving rather than at the signal, and a draining pod still admits the members it
// read; it loads the policy, which names its groups to the reader and starts their reads; and it
// waits up to wait for those reads, so the pod does not serve its first requests with no group
// read yet. A read still under way after the wait goes on, and its group admits nobody until it
// ends; a failed one is read again at the next load (groups.Reader.Want).
func loadFirstPolicy(serving context.Context, loader policy.Loader, groupReader *groups.Reader, registry *metrics.Registry, wait time.Duration) (*policy.Current, error) {
	groupReader.Start(serving)
	current, err := policy.NewCurrent(serving, loader, policyRefresh)
	if err != nil {
		return nil, err
	}
	firstReads, cancel := context.WithTimeout(serving, wait)
	groupReader.WaitReads(firstReads)
	cancel()
	policy.NewUnservableSecrets(registry, current)
	return current, nil
}

// vmPersonsByAccount is the person each BROKER_VM_EXCHANGE_ACCOUNTS entry names, keyed by its
// account, an entry that names none left out: whose Inbox lists a request one of that fleet's
// machines makes for a shared human-tier secret. The entry's person reaches no secret.
func vmPersonsByAccount(entries []exchange.GoogleEntry) map[string]string {
	persons := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Person != "" {
			persons[e.Account] = e.Person
		}
	}
	return persons
}

// jobMachines counts the BROKER_VM_EXCHANGE_JOB_ACCOUNTS entries among a set's machine entries:
// the fleets whose machines write as a job, naming no person.
func jobMachines(entries []exchange.GoogleEntry) int {
	n := 0
	for _, e := range entries {
		if e.Job.Listed() {
			n++
		}
	}
	return n
}

// judgeDispatch is the broker's one read of Dispatch, at BROKER_JUDGE_DISPATCH_URL under the token
// BROKER_JUDGE_DISPATCH_TOKEN_FILE holds, and nil where that URL is unset: the model judge reads a
// request's issue with it, and the request machine a Legion pod's issue's assignee. main builds it
// once and hands both the same one, so they share its token cache and its client.
func judgeDispatch(cfg config.Config) *judge.Dispatch {
	if cfg.JudgeDispatchURL == "" {
		return nil
	}
	return &judge.Dispatch{
		BaseURL: cfg.JudgeDispatchURL,
		Account: ciphertoken.NewAccountToken(cfg.JudgeDispatchTokenFile),
		HTTP:    &http.Client{Timeout: judgeDispatchTimeout},
	}
}

// The request machine answers every question the model judge asks of the broker. The assertion
// lives here, where the two already meet, and never in internal/cipher/requests: that package
// must not import the judge, since the judge imports it. Breaking either of the three methods
// fails this build rather than a deployment's first judged request.
var _ judge.Requests = (*requests.Machine)(nil)

// judgeDispatchTimeout bounds each of the judge's Dispatch GETs (the issue, its spec document and
// the ask the reason cites). A run's own 30-second
// budget already ends a read whose server never answers, but that budget is shared with the
// 20-second model call, so a client with no timeout of its own would let one stalled read eat
// the whole run. Ten seconds is wake.Envoy's own bound for the same kind of call, an authorized
// GET against a service inside the cluster. The request machine's assignee read holds itself to
// a shorter bound of its own (requests' assigneeBound).
const judgeDispatchTimeout = 10 * time.Second

// wireJudge gives m the broker's model judge, when this deployment configures one, and logs in
// one line which of the two brokers this is. The five settings are all-or-none (config.Load
// refuses a start that sets some of them), so the model URL alone says whether there is a judge,
// and a judge always has dispatch, the read of Dispatch main built once and gave m as well.
// The line of a broker with a judge names the identity whose token it presents to its model:
// metadata for the metadata server's identity token, or the service account
// BROKER_JUDGE_MODEL_ACCOUNT names, whose token is minted at generateIdToken. It is read off the
// reader the model holds, so the line shows what was wired rather than what was configured.
//
// ctx is main's own signal.NotifyContext, which the first SIGTERM cancels, and not boot's
// `serving`, which outlives the drain: every run hangs off it, so one in flight when the broker
// is asked to stop ends there, before the drain, and leaves its request pending for the person who
// decides it today. A run still before its verdict writes nothing at all; one whose approval the
// stop cuts writes its request.judged row on a context of its own, which lasts until the run's own
// deadline, and at least a second (judge's rowContext), so the write may go on into the drain,
// saying the approval was not applied, unless the approval's own row shows it applied. That is
// the right way round for a judge where it is not for the policy reload — a half-finished reload
// costs a pod stale tags for its drain, while a half-finished run would approve a credential as
// the pod goes away.
func wireJudge(ctx context.Context, cfg config.Config, rules proxyrules.Rules, m *requests.Machine, dispatch *judge.Dispatch) {
	if cfg.JudgeModelURL == "" {
		// config.Load refuses a judge beside BROKER_SIGNIN=off, so an off broker always lands here,
		// and its line says what such a broker does with a request a person would decide.
		detail := "none of " + strings.Join(config.JudgeSettings, ", ") + " is set, so every human-tier request waits for the person who decides it today"
		if cfg.SignInOff {
			detail = "BROKER_SIGNIN=off: no request waits for a person, since every one a person would decide is denied at once, so there is nothing for a judge to decide"
		}
		slog.Info("model judge", "state", "off", "detail", detail)
		return
	}
	account := ciphertoken.NewGoogleAccount(cfg.JudgeModelAudience)
	account.ServiceAccount = cfg.JudgeModelAccount
	runner := judge.New(ctx, judge.Options{
		Rules:      rules,
		Dispatch:   *dispatch,
		Model:      judge.NewModel(cfg.JudgeModelURL, cfg.JudgeModel, cfg.JudgeModelProtocol, account),
		Requests:   m,
		SkipChecks: cfg.JudgeTestSkipChecks,
	})
	// The hook the state machine calls for each request it writes pending: the judge takes the
	// machine's own type, so this is the method and nothing else. Consider returns at once, so
	// nothing of a run is on the requester's path.
	m.Judge = runner.Consider
	identity := cmp.Or(account.ServiceAccount, "metadata")
	slog.Info("model judge", "state", "on", "model", cfg.JudgeModel, "protocol", cfg.JudgeModelProtocol,
		"dispatch", cfg.JudgeDispatchURL, "identity", identity, "skip_checks", cfg.JudgeTestSkipChecks)
}

// logPodRegistrars logs, once at boot, what POST /v1/oauth/exchange/pod-sessions admits: how
// many registrar and pod account pairs BROKER_POD_REGISTRARS holds, none being a route that admits
// no registration, and whether BROKER_POD_REGISTRATION_AUDIENCE gives the route a second audience.
// A registrar BROKER_SERVICE_LOGINS does not list cannot call the route, so it registers nothing.
// The boot is not refused over one, since the list may be derived from another source than the
// logins and a refusal would turn a mismatch into an outage; each such registrar gets a WARN line
// naming it instead.
func logPodRegistrars(cfg config.Config) {
	slog.Info("pod-session registrars", "entries", len(cfg.PodRegistrars), "registration_audience", cfg.PodRegistrationAudience != "")
	for _, registrar := range cfg.PodRegistrars.Registrars() {
		if _, login := cfg.ServiceLogins[registrar]; !login {
			slog.Warn("pod-session registrar cannot log in", "registrar", registrar,
				"detail", "BROKER_POD_REGISTRARS names this account and BROKER_SERVICE_LOGINS does not list it, so it registers no pod")
		}
	}
}

// googleExchangeAudience is the Google audience GET /v1/settings publishes: BROKER_GOOGLE_OIDC_AUDIENCE
// where BROKER_VM_SERVICES lists a machine the exchange may mint a launcher credential for, and ""
// where the audience is set for an API caller alone (a google: entry in BROKER_API_ACCOUNTS). The
// exchange refuses every machine then, and a client that read the audience would mint a token, be
// refused and stop on every command, where with none published it skips the machine source.
func googleExchangeAudience(cfg config.Config) string {
	if len(cfg.ExchangeGoogleAccounts) == 0 {
		return ""
	}
	return cfg.GoogleOIDCAudience
}

// serviceNames is every registered service, sorted and each once: BROKER_SERVICES' and
// BROKER_VM_SERVICES'. The policy loader serves a secret whose owner tag names one of them as that
// service's, and refuses any other owner that is not a person.
func serviceNames(cfg config.Config) []string {
	names := slices.AppendSeq(slices.Collect(maps.Values(cfg.ServicesByAccount)), maps.Values(cfg.VMServicesByAccount))
	slices.Sort(names)
	return slices.Compact(names)
}

// keysFunc is issuerKeys' shape, a seam for a test that must not reach a real issuer.
type keysFunc func(ctx context.Context, issuer string) (*oidc.Keys, error)

// verifiers are what one boot's discovery of the token issuers the broker trusts produced: a
// verifier per issuer and the key set behind each, since the RFC 8693 exchange resolves a subject
// token's own key itself and must read the very set its verifier reads.
type verifiers struct {
	// Pod verifies a pod's enrollment token and a service's login token, nil on a broker told
	// about no cluster.
	Pod enroll.PodVerifier
	// K8sKeys is the cluster issuer's published keys, nil where Pod is.
	K8sKeys *oidc.Keys
	// PodRegistration verifies a registration's pod token at BROKER_POD_REGISTRATION_AUDIENCE, on
	// the cluster's key set Pod verifies with, nil on a broker with no such audience. Only the
	// registration route reads it (api.Deps.PodRegistrationTokens).
	PodRegistration enroll.PodVerifier
	// GoogleKeys is the Google issuer's published keys, nil on a broker with no Google audience,
	// which registers no Google subject token type.
	GoogleKeys *oidc.Keys
	// GoogleIdentities verifies a listed caller's own Google identity token, against the same
	// issuer and audience as those keys, nil where GoogleKeys is. It is how a caller on a VM —
	// the Envoy listener — authenticates on the routes BROKER_API_ACCOUNTS names it for,
	// holding no Kubernetes token at all.
	GoogleIdentities *oidc.Verifier
}

// discoverVerifiers reads the key sets of the token issuers the broker trusts (keysFor, issuerKeys
// in production) and builds a verifier on each: the cluster's (BROKER_K8S_OIDC_ISSUER and
// BROKER_K8S_OIDC_AUDIENCE), one verifier for both a pod's enrollment token and a service's login
// token, which are the same cluster's projected tokens for the same audience, whose key set it
// also answers for the RFC 8693 exchange, and on that key set a second where
// BROKER_POD_REGISTRATION_AUDIENCE is set, for a registration's pod token of that audience; and
// Google's (BROKER_GOOGLE_OIDC_ISSUER), for a machine's identity token, only when
// BROKER_GOOGLE_OIDC_AUDIENCE is set, whose key set the exchange resolves a machine's subject
// token against, logging one line that names the issuer, the audience, the machine services and
// how many machine entries the exchange holds. With no Google audience it reads and logs nothing
// about Google, and answers no Google keys, so the broker registers no Google subject token type.
func discoverVerifiers(ctx context.Context, cfg config.Config, keysFor keysFunc) (verifiers, error) {
	var found verifiers
	if cfg.K8sOIDCIssuer != "" {
		keys, err := keysFor(ctx, cfg.K8sOIDCIssuer)
		if err != nil {
			return verifiers{}, err
		}
		found.K8sKeys = keys
		found.Pod = enroll.K8sPodVerifier{Verifier: oidc.NewWithKeys(cfg.K8sOIDCIssuer, cfg.K8sOIDCAudience, keys)}
		if cfg.PodRegistrationAudience != "" {
			found.PodRegistration = enroll.K8sPodVerifier{Verifier: oidc.NewWithKeys(cfg.K8sOIDCIssuer, cfg.PodRegistrationAudience, keys)}
		}
	}
	if cfg.GoogleOIDCAudience == "" {
		return found, nil
	}
	googleKeys, err := keysFor(ctx, cfg.GoogleOIDCIssuer)
	if err != nil {
		return verifiers{}, err
	}
	services := slices.Sorted(maps.Values(cfg.VMServicesByAccount))
	slog.Info("google machine identity", "issuer", cfg.GoogleOIDCIssuer, "audience", cfg.GoogleOIDCAudience,
		"services", strings.Join(slices.Compact(services), ","), "fleets", len(cfg.ExchangeGoogleAccounts))
	found.GoogleKeys = googleKeys
	// One verifier on that key set, for a listed caller that holds a Google identity rather
	// than a Kubernetes one: the audience is BROKER_GOOGLE_OIDC_AUDIENCE, the one every machine
	// of the deployment already holds a token for, so such a caller presents the token its
	// metadata server already hands it.
	found.GoogleIdentities = oidc.NewWithKeys(cfg.GoogleOIDCIssuer, cfg.GoogleOIDCAudience, googleKeys)
	return found, nil
}

// machineCredentials is oauth.ExchangeConfig.MachineCredentials over the enrollment service: the
// launcher credential a trade mints or moves, written in the exchange's own transaction
// (enroll.Service.IssueTokenLoginTx), so the credential and the access token that names it land
// together. Both trades that mint one come through here — a machine's own, which carries its
// fleet's service and no person, and a person's sign-in on such a machine, which carries that
// person as its operator — so there is one way a credential is minted and not two. A key
// another live credential holds is oauth.ErrMachineKeyTaken, and a key a live read token is bound
// to (enroll.ErrKeyBindsReadToken) is oauth.ErrMachineKeyBindsReadToken; the exchange answers
// each invalid_grant with a hint of its own.
func machineCredentials(enr *enroll.Service) oauth.MachineCredentials {
	return func(ctx context.Context, cred oauth.MachineCredential) (string, error) {
		tx, ok := oauthstore.TxFrom(ctx)
		if !ok {
			return "", errors.New("broker: a machine's launcher credential is minted in the exchange's transaction")
		}
		id, err := enr.IssueTokenLoginTx(ctx, tx, enroll.TokenLogin{
			Subject:      cred.Subject,
			Service:      cred.Service,
			Host:         cred.Host,
			Operator:     cred.Operator,
			TokenExpires: cred.Expires,
			Thumbprint:   cred.Thumbprint,
			JWK:          cred.JWK,
			Expires:      cred.Expires,
		})
		if enroll.KeyHeld(err) {
			return "", fmt.Errorf("%w: %v", oauth.ErrMachineKeyTaken, err)
		}
		if errors.Is(err, enroll.ErrKeyBindsReadToken) {
			return "", fmt.Errorf("%w: %v", oauth.ErrMachineKeyBindsReadToken, err)
		}
		if err != nil {
			return "", err
		}
		return id.String(), nil
	}
}

// personSignIns is oauth.ExchangeConfig.PersonSignIns over the broker's store: the sign-in a
// person's trade belongs to, written in the exchange's own transaction
// (store.RecordPersonSignIn), so the sign-in and the access token minted under it land
// together. A box key that person has revoked is store.ErrSignInRevoked, and one whose sign-in
// was made on another machine is store.ErrSignInMachineChanged; the exchange answers both
// invalid_grant naming the sign-in command.
func personSignIns() oauth.PersonSignIns {
	return func(ctx context.Context, in oauth.PersonSignIn) error {
		tx, ok := oauthstore.TxFrom(ctx)
		if !ok {
			return errors.New("broker: a person's sign-in is recorded in the exchange's transaction")
		}
		err := store.RecordPersonSignIn(ctx, tx, in.Person, in.Thumbprint, in.Machine)
		switch {
		case errors.Is(err, store.ErrSignInRevoked):
			return fmt.Errorf("%w: %v", oauth.ErrSignInRevoked, err)
		case errors.Is(err, store.ErrSignInMachineChanged):
			return fmt.Errorf("%w: %v", oauth.ErrSignInMachineChanged, err)
		}
		return err
	}
}

// issuerKeys reads issuer's published signing keys: the source a token's own key is resolved
// against, both for the verifier that answers whether a token verifies (a projected
// service-account token) and for the RFC 8693 exchange, which
// needs the key itself. It takes the same boot deadline
// discovery does: an issuer that accepts the connection and never answers refuses the boot rather
// than hanging it with no health endpoint serving. No key is fetched here; the first token read
// reads the set.
func issuerKeys(ctx context.Context, issuer string) (*oidc.Keys, error) {
	discovery, cancel := context.WithTimeout(ctx, oidc.DiscoveryTimeout)
	defer cancel()
	return oidc.NewKeys(discovery, issuer)
}

// typeMinters is the broker's minter for each type. Its arguments are the production minters in
// newRequestMachine; a test passes ones aimed at its own endpoints and projected token file, and
// its own binding of each google-service-account and anthropic-federation secret to the account
// that minter may mint for, which in production is BROKER_SERVICE_ACCOUNT_SECRETS; a minter given
// no binding at all mints no secret of that type. The google-service-account and
// anthropic-federation minters are pointers: a method value would otherwise copy the credentials
// each caches away from the minter every later mint reads. The other two are copied, and each
// Retry they hold is a pointer, so minters given one Retry still draw from its one budget. The
// github-installations minter takes no setting and calls no endpoint: it reads the tokens the
// secret stores, so it is built here.
func typeMinters(githubApp secrets.GitHubApp, googleLogin secrets.GoogleLogin, azureLogin secrets.AzureLogin, googleServiceAccount *secrets.GoogleServiceAccount, anthropicFederation *secrets.AnthropicFederation) map[string]requests.Minter {
	return map[string]requests.Minter{
		policy.TypeGitHubApp:            githubApp.Mint,
		policy.TypeGitHubInstallations:  secrets.GitHubInstallations{}.Mint,
		policy.TypeGoogleLogin:          googleLogin.Mint,
		policy.TypeAzureLogin:           azureLogin.Mint,
		policy.TypeGoogleServiceAccount: googleServiceAccount.Mint,
		policy.TypeAnthropicFederation:  anthropicFederation.Mint,
	}
}

// The sign-in client check's lines (checkSignInClient). Each says that a pass found other than
// what the line before it said, keyed on the check's state and on whether its write failed, never
// on an error's text, as enroll.ServingClock.Run logs its beats: a provider that stays away for a
// day writes one line, not one each clientCheckInterval.
const (
	// clientCheckFailedMessage is logged, at WARN, by a pass that finds a state other than ready
	// and other than the state this message last named since the client was last ready, followed
	// by state=, issuer= and error=.
	clientCheckFailedMessage = "broker: the sign-in client check"
	// clientCheckReadyMessage is logged, at INFO, by a pass that finds the client ready after a
	// clientCheckFailedMessage line, followed by issuer= and was=<the state that line named>.
	clientCheckReadyMessage = "broker: the sign-in client check finds the client ready again"
	// clientCheckUnwrittenMessage is logged, at WARN, by a pass whose write fails when the pass
	// before it wrote, followed by state= and error=.
	clientCheckUnwrittenMessage = "broker: the sign-in client check was not written"
	// clientCheckWrittenMessage is logged, at INFO, by a pass that writes after a pass that logged
	// clientCheckUnwrittenMessage, followed by state=.
	clientCheckWrittenMessage = "broker: the sign-in client check is written again"
)

// clientCheckLog is what the client check last logged, which runClientChecks holds from one pass
// to the next so that a pass logs a change and nothing while nothing changes. Its zero value is a
// check that has logged nothing, which is the same as one that last found the client ready and
// wrote it: a pass that finds that logs nothing.
type clientCheckLog struct {
	// notReady is the state clientCheckFailedMessage last named, and empty when it has named none
	// since the client was last ready.
	notReady oidc.ClientCheck
	// unwritten is whether the last pass's write failed.
	unwritten bool
}

// runClientChecks checks the sign-in client at once and every clientCheckInterval after, until
// ctx ends, holding what the passes logged.
func runClientChecks(ctx context.Context, signIn approval.SignIn, st *store.Store) {
	var logged clientCheckLog
	for {
		checkSignInClient(ctx, signIn, st, &logged)
		select {
		case <-ctx.Done():
			return
		case <-time.After(clientCheckInterval):
		}
	}
}

// checkSignInClient takes one pass of the client check and writes what it found, with the time,
// into the row GET /signin/status answers. A test drives this rather than waiting an hour for
// the loop above to take its second pass.
//
// Neither half can stop the broker. A provider that cannot be asked is the answer
// issuer_unreachable and a line in the log, not a boot that fails: people sign in through a
// path that reads the discovery document again on its own, so a check that fails says nothing
// about whether the next sign-in works. A write that fails loses this pass's answer and the
// next pass writes again.
//
// Each half logs only a change from what logged holds, which it then updates: the check
// logs clientCheckFailedMessage when it finds a state that is not ready and not the one logged
// last, and clientCheckReadyMessage when it finds ready after one that was not; the write logs
// clientCheckUnwrittenMessage when it fails after one that did not, and clientCheckWrittenMessage
// when it succeeds after one that failed.
func checkSignInClient(ctx context.Context, signIn approval.SignIn, st *store.Store, logged *clientCheckLog) {
	check, err := oidc.CheckClient(ctx, signIn.Issuer, signIn.ClientID, signIn.ClientSecret)
	if ctx.Err() != nil {
		// The process is stopping: this answer is the end of the context, not the provider's,
		// so it is neither logged nor written, and what logged holds stands.
		return
	}
	switch {
	case check == oidc.ClientCheckReady && logged.notReady != "":
		slog.Info(clientCheckReadyMessage, "issuer", signIn.Issuer, "was", logged.notReady)
		logged.notReady = ""
	case check != oidc.ClientCheckReady && check != logged.notReady:
		slog.Warn(clientCheckFailedMessage, "state", check, "issuer", signIn.Issuer, "error", err)
		logged.notReady = check
	}
	err = st.RecordClientCheck(ctx, string(check), time.Now())
	switch {
	case ctx.Err() != nil:
	case err != nil && !logged.unwritten:
		slog.Warn(clientCheckUnwrittenMessage, "state", check, "error", err)
		logged.unwritten = true
	case err == nil && logged.unwritten:
		slog.Info(clientCheckWrittenMessage, "state", check)
		logged.unwritten = false
	}
}

// endSessionsCommand and versionsCommand are two of the three subcommands the broker takes in
// place of serving; the third is `profiles check` (profilesCommand, profilescheck.go).
const (
	endSessionsCommand = "end-sessions"
	versionsCommand    = "versions"
)

// runSubcommand runs the subcommand args name and answers its exit code. It is every argument
// this binary takes: an unknown name, a missing or malformed argument to a name it knows, is
// refused with 2 and a usage line, never a fall-through to serving. A removed subcommand is
// refused the same way, naming the one that replaced it. There are four names, so a switch still
// reads as one thing (cmd/dispatch/subcommands.go keeps a table for its eleven).
func runSubcommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	switch args[0] {
	case endSessionsCommand:
		if len(args) != 2 || strings.TrimSpace(args[1]) == "" {
			fmt.Fprintf(stderr, "broker: usage: broker %s <login>, one login and nothing else\n", endSessionsCommand)
			return 2
		}
		return endSessions(ctx, strings.TrimSpace(args[1]), stdout, stderr)
	case versionsCommand:
		if len(args) != 1 {
			fmt.Fprintf(stderr, "broker: usage: broker %s, with no argument\n", versionsCommand)
			return 2
		}
		return versions(ctx, stdout, stderr)
	case profilesCommand:
		if len(args) < 2 || args[1] != profilesCheckCommand {
			fmt.Fprintln(stderr, profilesCheckUsage)
			return 2
		}
		return runProfilesCheck(args[2:], stdout, stderr)
	case removedAirlockCheckCommand:
		fmt.Fprintf(stderr, "broker: %s is removed: the airlock's rules are the proxy blocks of the profiles file now; check that file with broker %s\n",
			removedAirlockCheckCommand, profilesCheckForm)
		return 2
	}
	fmt.Fprintf(stderr, "broker: unknown subcommand %q; the subcommands are %s <login>, %s and %s, and broker with no argument serves\n",
		args[0], endSessionsCommand, versionsCommand, profilesCheckForm)
	return 2
}

// versions prints what broker_schema_migrations records of each version against the migrations
// this image embeds, and what this image's next boot would do with each row (store.CheckVersions):
// the check to take, read-only, before an image with a different set rolls out. It reads the same
// configuration the server does and signs in to the database the same way, the RDS IAM token
// minter included (openStore), binds no listener and migrates nothing. It exits 0 when every
// recorded version has a file of this image and the next boot refuses none, 1 when one has no file,
// a row would be refused or the table records nothing, and 2 when the check could not be taken.
func versions(ctx context.Context, stdout, stderr io.Writer) int {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", versionsCommand, err)
		return 2
	}
	st, _, err := openStore(ctx, cfg, false)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", versionsCommand, err)
		return 2
	}
	defer st.Close()
	check, err := st.CheckVersions(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", versionsCommand, err)
		return 2
	}
	check.Write(stdout)
	if !check.OK() {
		return 1
	}
	return 0
}

// endSessions deletes every session login is signed in to the approval page with, wherever and
// whenever it was made, and prints how many it deleted: the operator's way to end one person's
// sign-ins everywhere at once. It runs in any one-off process of the image that has the serving
// broker's settings and a way to its database: an exec into a broker pod where the deployment
// admits one, or a one-off pod, job or container of the image, with BROKER_SIGNIN=off in place of
// the sign-in settings (docs/site/src/content/docs/broker/operate.md, "Running a subcommand").
//
// It reads the same configuration the server does and opens the database the same way, the RDS
// IAM token minter included (openStore), and it binds no listener. It does not migrate: an image
// older than this subcommand cannot be allowed to boot and migrate the database because a
// deployment asked it to end some sessions, so the schema is whatever the servers have already
// applied (cmd/dispatch/main.go's subcommands take the opposite view, and say why: each of them
// exists to work on a schema it brings up to date first).
func endSessions(ctx context.Context, login string, stdout, stderr io.Writer) int {
	// The form every login comparison in the module is made in (record.CanonicalLogin), and the
	// form oidc.WorkspaceLogin stores a session under: a login typed in another case ends the
	// person's sessions here as surely as the one they signed in with.
	login = record.CanonicalLogin(login)
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", endSessionsCommand, err)
		return 1
	}
	st, _, err := openStore(ctx, cfg, false)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", endSessionsCommand, err)
		return 1
	}
	defer st.Close()
	ended, err := st.DeleteBrowserSessionsForLogin(ctx, login)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", endSessionsCommand, err)
		return 1
	}
	sessions := "sessions"
	if ended == 1 {
		sessions = "session"
	}
	fmt.Fprintf(stdout, "%s: ended %d %s of %s\n", endSessionsCommand, ended, sessions, login)
	return 0
}
