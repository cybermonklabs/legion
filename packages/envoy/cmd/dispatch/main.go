// Command dispatch serves the Dispatch dashboard and API, signs people in through a Cognito
// sign-in pool or with Google directly, and reads GitHub for the web app as the GitHub App.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
	"github.com/sjawhar/envoy/internal/dispatch/config"
	"github.com/sjawhar/envoy/internal/dispatch/dashboard"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/embedqueue"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/texttreefill"
	"github.com/sjawhar/envoy/internal/dispatch/textversions"
	"github.com/sjawhar/envoy/internal/envconf"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/principal"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/listenfd"
)

const (
	// defaultListenPort is the port when DISPATCH_PORT is unset; listenAddress joins it to the host.
	defaultListenPort = "8766"
	// defaultEnvoyURL is the Envoy listener when ENVOY_URL is unset: this machine's.
	defaultEnvoyURL   = "http://127.0.0.1:9020"
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
)

// buildCommit is the legion commit this binary was built from. Only the image build stamps it
// (packages/envoy/docker/Dockerfile passes its LEGION_COMMIT build argument through -ldflags -X,
// and refuses anything but a full commit sha); every other build leaves it empty, and /healthz
// then reports the commit as null rather than naming one it cannot vouch for.
var buildCommit string

type bootConfig struct {
	DatabaseURL string
	// AgentTokens is DISPATCH_AGENT_TOKEN parsed: every value the API accepts as the shared agent
	// token, the first the current one.
	AgentTokens *auth.SharedAgentTokens
	// ProjectOwnerFallback is DISPATCH_PROJECT_OWNER_FALLBACK, a lowercase email: the full member
	// the boot makes the owner of every project with no owner (store.EnsureProjectOwners); empty
	// gives none, and the boot then refuses to serve while a project has no owner.
	ProjectOwnerFallback string
	// ParentStatusRule is DISPATCH_PARENT_STATUS_RULE=on: main runs R25's one-time fix at boot
	// (api.RunParentStatusFixAtBoot), and the server it builds refuses a write that leaves a
	// parent's status out of line with its children's. Unset or off is false.
	ParentStatusRule bool
	// NewIssueLabel and NewIssueSession are DISPATCH_NEW_ISSUE_LABEL and
	// DISPATCH_NEW_ISSUE_SESSION: the label an issue the search bar files from a person's typed
	// words carries, and the session that issue is routed to and its first message is sent to.
	// Empty is a deployment that configured neither: the issue carries no label, and no
	// conversation is started on it.
	NewIssueLabel   string
	NewIssueSession string
	EnvoyURL        string
	GitHubAPIBase   string
	// ReviewerAppID is DISPATCH_REVIEWER_APP_ID, the id of the App whose `tester` and `architect`
	// check runs are Legion's verdicts, as the text of the number GitHub's own objects carry it
	// as. Empty means the CI reconcile pass records no verdict check at all.
	ReviewerAppID string
	// VerdictAppID is DISPATCH_VERDICT_APP_ID, the id of the App that writes `legion-verdict`, as
	// the text of the number GitHub's own objects carry it as. The CI reconcile pass leaves that
	// App's check suites out of the record, as the listener does (ENVOY_VERDICT_APP_ID). Empty
	// means the pass records every App's completed suite.
	VerdictAppID   string
	IdentityHeader string
	// IAPAudience is DISPATCH_IAP_AUDIENCE under DISPATCH_IDENTITY=iap: the audience
	// Identity-Aware Proxy signs each request's assertion for, which the assertion's aud must
	// name; empty under any other identity. IAPKeySetURL is where IAP's signing keys are read,
	// oidc.IAPKeySetURL, which no setting changes: a test points it at a key set it serves.
	IAPAudience  string
	IAPKeySetURL string
	// SignInIssuer, SignInClientID and SignInClientSecret, with exactly one of SignInGroup and
	// SignInDomain, configure Google sign-in (DISPATCH_SIGNIN_*): through a Cognito sign-in pool
	// for a group, or with Google directly for a Workspace domain; empty means no sign-in.
	SignInIssuer       string
	SignInClientID     string
	SignInClientSecret string
	SignInGroup        string
	SignInDomain       string
	NATSDisabled       bool
	// NATSReplicas is ENVOY_NATS_REPLICAS, the deployment's replica count for the stream and the
	// webhook redelivery bucket, and StreamMaxBytes is ENVOY_STREAM_MAX_BYTES, the stream's byte
	// limit or 0 for none set (bus.ParseNATSReplicas, bus.ParseStreamMaxBytes).
	NATSReplicas     int
	StreamMaxBytes   int64
	TestHooksEnabled bool
	// SettleDelay is how long a document waits after a live change before it settles, zero for the
	// document service's own default; only DISPATCH_TEST_SETTLE_DELAY, under the test hooks, sets it.
	SettleDelay time.Duration
	// MarkWait is how long a mark the browser says it wrote is waited for before the server
	// answers that the document does not hold it, zero for the document service's own 1 s; only
	// DISPATCH_TEST_MARK_WAIT, under the test hooks, sets it.
	MarkWait time.Duration
	// UnrecordedMarkTTL is how long a mark no row records is kept before the settlement sweep
	// expires it, zero for the document service's own minute; only
	// DISPATCH_TEST_UNRECORDED_MARK_TTL, under the test hooks, sets it.
	UnrecordedMarkTTL time.Duration
	// RouteRosterTTL is how long a stored listener answer serves issue reads' route_status, zero
	// for the server's own five seconds; only DISPATCH_TEST_ROUTE_ROSTER_TTL, under the test hooks,
	// sets it.
	RouteRosterTTL time.Duration
	// ListenerTakeoverWindow is how long a sender keeps trying a listener that is being replaced,
	// zero for the server's own twelve seconds; only DISPATCH_TEST_LISTENER_TAKEOVER_WINDOW, under
	// the test hooks, sets it.
	ListenerTakeoverWindow time.Duration
	// HorizonStallAfter is how long a pending event-horizon observation waits before the process
	// logs its stall warning, and HorizonWarnEvery how often every event-horizon warning repeats
	// - that stall warning, the failed-observation warning and the failed-persist warning - both
	// zero for the horizon's own five and thirty seconds; only DISPATCH_TEST_HORIZON_STALL_AFTER
	// and DISPATCH_TEST_HORIZON_WARN_EVERY, under the test hooks, set them, since a test whose
	// wait keys on the stall warning cannot sit out either.
	HorizonStallAfter time.Duration
	HorizonWarnEvery  time.Duration
	// EditNoticeQuiet is how long a document stays quiet after a person's last settled version
	// before its edit notice goes, zero for the document service's own 30 s; only
	// DISPATCH_TEST_EDIT_NOTICE_QUIET, under the test hooks, sets it.
	EditNoticeQuiet time.Duration
	// CIReconcileInterval is how often the CI reconcile pass runs, zero for cireconcile.Interval's
	// five minutes; only DISPATCH_TEST_CI_RECONCILE_INTERVAL, under the test hooks, sets it, since
	// no process-level test can wait five minutes for a pass.
	CIReconcileInterval time.Duration
	// RedeliveryInterval is how often the webhook redelivery sweep runs, zero for
	// redeliver.Interval's two minutes; only DISPATCH_TEST_REDELIVERY_INTERVAL, under the test
	// hooks, sets it, since no process-level test can wait two minutes for a sweep.
	RedeliveryInterval time.Duration
	// DeliveryReconcileInterval is how often the delivery reconcile pass runs, zero for
	// delivery.ReconcileInterval's five minutes; only DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL,
	// under the test hooks, sets it, since no end-to-end test can wait five minutes for a pass.
	DeliveryReconcileInterval time.Duration
	// MaintenanceSweepInterval is how often the maintenance sweep runs - the Idempotency-Keys and
	// spent DPoP proofs past their windows, and the deliveries a process that died left owed -
	// zero for api.MaintenanceSweepInterval's minute; only
	// DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL, under the test hooks, sets it, since no
	// process-level test can wait a minute for a pass.
	MaintenanceSweepInterval time.Duration
	// ProcessHeartbeat is how often this process stamps its row in the registry of live server
	// processes, zero for processes.HeartbeatInterval's five seconds; only
	// DISPATCH_TEST_PROCESS_HEARTBEAT, under the test hooks, sets it, and processes.LiveWindow
	// keeps its ratio of three beats to it, since no process-level test can wait a live window
	// and a heartbeat for a refusal to stand.
	ProcessHeartbeat time.Duration
	// ListenerRosterUncached makes every read of the listener's session list ask the listener
	// rather than share the process's five-second copy; only DISPATCH_TEST_ROSTER_UNCACHED=1, under
	// the test hooks, sets it.
	ListenerRosterUncached bool
	// EmbedderOff builds no embedder, whatever DISPATCH_EMBEDDER and the AWS SDK's own variables
	// would have built: meaning search answers keyword-only and the embedding queue does nothing.
	// Only DISPATCH_TEST_EMBEDDER_OFF=1, under the test hooks, sets it.
	EmbedderOff bool
	// ReconcileAtStart makes the delivery reconcile run one pass as it starts rather than only after
	// its jittered first tick; only DISPATCH_TEST_RECONCILE_AT_START=1, under the test hooks, sets
	// it.
	ReconcileAtStart bool
	// KeyRemakeInterval is how often the key remake loop checks the reading of the stored keys,
	// zero for delivery.KeyRemakeEvery's 30 s; only DISPATCH_TEST_KEY_REMAKE_INTERVAL, under the
	// test hooks, sets it.
	KeyRemakeInterval time.Duration
	// KeyRemakeOff starts no key remake loop, for a test that must see stored keys stay on another
	// reading; only DISPATCH_TEST_KEY_REMAKE_OFF=1, under the test hooks, sets it.
	KeyRemakeOff bool
	// ArchitectureImportAtStart makes the architecture import run one pass as it starts rather than
	// only after its jittered first tick; only DISPATCH_TEST_ARCHITECTURE_IMPORT_AT_START=1, under
	// the test hooks, sets it.
	ArchitectureImportAtStart bool
	// OIDCIssuer and OIDCAudience configure verification of projected
	// service-account tokens. Both set or neither; empty means no verifier.
	OIDCIssuer   string
	OIDCAudience string
	// AgentSecretsURL is DISPATCH_AGENT_SECRETS_URL, the secrets broker's API; empty means the
	// credential-request feature is off. AgentSecretsTokenFile is
	// DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE, the file holding Dispatch's own projected
	// Kubernetes ServiceAccount token (required when AgentSecretsURL is set). The file is read
	// by the broker client on each call, never here: a projected token rotates.
	AgentSecretsURL       string
	AgentSecretsTokenFile string
	// LegionStatusScope is DISPATCH_LEGION_STATUS_SCOPE=on. It turns on the refusal of three
	// writes of a Legion issue from a credential that holds neither the person scope nor
	// legion-status: a write of its status, the removal of its legion label, and a move that
	// leaves it under no labelled issue, but for the status write of the architect of an issue
	// above (api's legion_status_scope.go). Unset or off, it is false (api.Deps.LegionStatusScope).
	LegionStatusScope bool
	// ProofOrigins is DISPATCH_PROOF_ORIGINS read (proofOriginEntries): the origins Dispatch is
	// reached at beside DISPATCH_SERVER_URL's, each written scheme://host, which a broker token's
	// proof may name in its 'htu' (proofOrigins). Empty, unset, adds none.
	ProofOrigins []string
	// FileStoreBucket is DISPATCH_FILE_STORE_BUCKET: the bucket uploaded files are stored in,
	// gs://<bucket> for Cloud Storage or an S3 bucket's name (files.Open). Empty keeps them in
	// Postgres.
	FileStoreBucket string
	// ListenAddr is the address the server serves on: the inherited socket's own address with
	// DISPATCH_LISTEN_FD, else the one main binds, from DISPATCH_LISTEN_HOST and DISPATCH_PORT
	// (listenAddress). The dev sign-in fence checks this value, so what it checks is what serves.
	ListenAddr string
	// ListenSocket is the listening socket DISPATCH_LISTEN_FD names, inspected but not yet
	// adopted: main serves on it in place of binding ListenAddr. Nil when the variable is unset.
	ListenSocket *listenfd.Socket
	// PprofAddr is where the profiling listener binds, 127.0.0.1 and DISPATCH_PPROF_PORT
	// (pprofAddress); empty, nothing listens.
	PprofAddr string
	// MaxLiveRooms and MaxDocConnections are DISPATCH_MAX_LIVE_ROOMS and
	// DISPATCH_MAX_DOC_CONNECTIONS, the document service's room and connection caps; zero, unset,
	// is the service's own default.
	MaxLiveRooms      int
	MaxDocConnections int
	// DevSignIn (DISPATCH_DEV_SIGNIN=1) mounts GET /auth/_dev/signin behind the dev sign-in
	// fence; the signing key is then per process, or derived from DevSignInSharedKey.
	DevSignIn bool
	// DevSignInSharedKey is DISPATCH_DEV_SIGNIN_SHARED_KEY: under dev sign-in, the secret every
	// process of one local harness derives its cookie key from (sessionSigningKey), so they
	// accept each other's cookies. resolveBootConfig refuses it without dev sign-in.
	DevSignInSharedKey string
	// WebDist is DISPATCH_WEB_DIST: the dashboard directory to serve, or empty to find it from
	// the binary (defaultWebDistDir).
	WebDist string
	// SigningKey is DISPATCH_SIGNING_KEY: the session cookie keys, the current one first, or the
	// empty set to keep one key in the data dir (sessionSigningKey).
	SigningKey auth.SigningKeys
	// InsecureCookie (DISPATCH_INSECURE_COOKIE set to anything) drops Secure from every cookie.
	InsecureCookie bool
	// EnvoyToken is ENVOY_TOKEN, the bearer every Envoy listener call sends.
	EnvoyToken string
	// EnvoyAccountTokenFile is ENVOY_ACCOUNT_TOKEN_FILE, the file holding Dispatch's own projected
	// Kubernetes service-account token for the listener's audience, which every listener call
	// reads and presents in EnvoyToken's place. The file is read by the listener client on each
	// call, never here: a projected token rotates.
	EnvoyAccountTokenFile string
	// EnvoyGoogleAudience is ENVOY_ACCOUNT_GOOGLE_AUDIENCE: on Google Cloud, the audience of the
	// identity token the metadata server mints for Dispatch's machine's service account, which
	// every listener call presents in EnvoyToken's place (envoy.WithGoogleIdentity); the listener's
	// ENVOY_OIDC_GOOGLE_AUDIENCE. At most one of it and EnvoyAccountTokenFile is set.
	EnvoyGoogleAudience string
	// RoleHolderWindows are the session windows a role route's holder is judged by
	// (ENVOY_SESSION_LIVE_WINDOW and ENVOY_SESSION_GONE_HORIZON, roleHolderWindows): what the
	// outbox's reader of the role and session buckets carries (roles.NewBuckets).
	RoleHolderWindows roles.Windows
	// SearchFillOff is DISPATCH_SEARCH_FILL=off: this process does not start the keyword fill
	// (textversions.Run), so it takes no hold and writes no row of search_text_versions or its
	// cursor. Unset or on, it starts the fill.
	SearchFillOff bool
	// Embedder is where meaning search embeds text (DISPATCH_EMBEDDER and DISPATCH_AZURE_*),
	// checked whole; newEmbedder builds its client.
	Embedder embedderConfig
	// Answerer is where the server generates text (DISPATCH_ANSWERER, and the DISPATCH_AZURE_CHAT_*
	// or DISPATCH_CHAT_* settings of the place it names), checked whole; an empty Place is no
	// model configured, which the server says at boot rather than refusing to start.
	Answerer answererConfig
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	env := processSettings()
	if len(os.Args) > 1 {
		os.Exit(runSubcommand(context.Background(), os.Args[1:], env, os.Stdout, os.Stderr))
	}
	boot, err := resolveBootConfig(env)
	if err != nil {
		slog.Error("dispatch: resolve boot config", "error", err)
		os.Exit(1)
	}
	// Dispatch's Google Cloud identity at the listener needs a metadata server, decided once here:
	// a machine with none refuses to start, naming the setting, and a metadata server that does
	// not answer later fails only the call it was asked for, which its delivery tries again.
	if boot.EnvoyGoogleAudience != "" {
		if err := dispatchenvoy.CheckGoogleIdentity(context.Background()); err != nil {
			slog.Error("dispatch: Envoy listener identity", "error", err)
			os.Exit(1)
		}
	}
	// The registry's pace, before anything registers or reads what is live. The live window stays
	// three beats, as processes.LiveWindow is of processes.HeartbeatInterval, so a run cannot give
	// itself a window shorter than its own heartbeat, and the wait a refused process makes
	// (Register: one live window and one heartbeat) shortens with it.
	if boot.ProcessHeartbeat > 0 {
		processes.HeartbeatInterval = boot.ProcessHeartbeat
		processes.LiveWindow = 3 * boot.ProcessHeartbeat
	}

	envoyConfig, err := loadEnvoyConfig(env, config.LoadOptions{})
	if err != nil {
		slog.Error("dispatch: load envoy config", "error", err)
		os.Exit(1)
	}
	serverURL := ""
	if envoyConfig.Dispatch != nil {
		serverURL = envoyConfig.Dispatch.ServerURL
	}
	// The App credentials are read before anything connects, so the dev sign-in fence refuses a
	// key it will not sign with before NATS or Postgres is dialled.
	dataDir, err := defaultDataDir()
	if err != nil {
		slog.Error("dispatch: resolve data dir", "error", err)
		os.Exit(1)
	}
	appCfg, appSource, err := loadAppCredentials(env, dataDir)
	if err != nil {
		slog.Error("dispatch: load app credentials", "error", err)
		os.Exit(1)
	}
	if err := devSignInLoadedFence(boot, serverURL, appCfg, appSource); err != nil {
		slog.Error("dispatch: dev sign-in", "error", err)
		os.Exit(1)
	}
	if appCfg == nil {
		slog.Info("dispatch: no app credentials yet — dashboard will respond 503 until configured")
	} else {
		slog.Info("dispatch: loaded github app", "slug", appCfg.Slug, "client_id", appCfg.ClientID, "source", appSource.String())
	}
	if boot.DevSignIn {
		cookies := "cookies are valid on this process only"
		if boot.DevSignInSharedKey != "" {
			cookies = "cookies are valid on every process given this DISPATCH_DEV_SIGNIN_SHARED_KEY"
		}
		slog.Warn("dispatch: dev sign-in mounted: any email signs in at /auth/_dev/signin without the sign-in issuer; "+cookies, "origin", serverURL)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// The profiler binds first, before NATS, migrations and the rest of boot, so a slow or hung
	// boot can be profiled and a port clash refuses the boot before anything connects.
	if _, err := startPprof(ctx, boot.PprofAddr); err != nil {
		slog.Error("dispatch: pprof", "error", err)
		os.Exit(1)
	}

	// INFRA-555: every NATS client of the server connects in the background (serverBus), so a bus
	// that is down or refuses the credential at boot leaves the process serving everything that
	// does not need the bus, and connected once the bus answers; only a configuration error (an
	// unusable seed or proxy setting, a server that is not this machine's) ends the boot here.
	var natsClient *bus.Client
	if boot.NATSDisabled {
		slog.Info("dispatch: NATS publisher disabled")
	} else {
		natsClient, err = bus.ConnectOwningStream(envoyConfig.Envoy.NatsURLs, serverClient(dispatchStreamOptions(boot, env)...)...)
		if err != nil {
			slog.Error("dispatch: connect NATS", "error", err)
			os.Exit(1)
		}
		defer natsClient.Close()
	}

	// The live agent conversation relay takes a connection of its own: a viewer's core
	// subscription must not be lost to the publisher client replacing a shared one, and this
	// connection never touches JetStream. With test hooks and no NATS it is an in-process
	// source the e2e harness publishes into instead.
	var agentStream agentstream.Source
	if natsClient == nil {
		if boot.TestHooksEnabled {
			agentStream = agentstream.NewMemory()
			slog.Info("dispatch: agent conversation relay served from the test hook")
		} else {
			slog.Info("dispatch: agent conversation relay off: it needs NATS")
		}
	} else {
		streamConn, err := bus.Dial("dispatch-agent-stream", envoyConfig.Envoy.NatsURLs, env.lookup, serverBus()...)
		if err != nil {
			slog.Error("dispatch: connect the agent conversation relay", "error", err)
			os.Exit(1)
		}
		defer streamConn.Close()
		agentStream = agentstream.NewNATS(streamConn)
	}

	// A person's targeted message to a session that neither the listener's registry nor its
	// known-session store holds is queued on that session's own subject, published through the
	// NATS client. With test hooks and no NATS it goes to an in-process recorder the e2e harness
	// reads (GET /api/v1/messages/_test/queued) instead, and with neither such a message settles
	// failed. Only a value that is there is put in the interface: a nil *bus.Client in it would
	// read as a publisher.
	var agentPublisher api.AgentPublisher
	switch {
	case natsClient != nil:
		agentPublisher = natsClient
	case boot.TestHooksEnabled:
		agentPublisher = api.NewAgentPublishRecorder()
		slog.Info("dispatch: targeted messages for a session no store holds go to the test hook's recorder")
	default:
		slog.Info("dispatch: a targeted message to a session no store holds fails: queueing it needs NATS")
	}

	database, err := store.Open(ctx, boot.DatabaseURL)
	if err != nil {
		slog.Error("dispatch: open database", "error", err)
		os.Exit(1)
	}
	if err := database.Migrate(ctx); err != nil {
		slog.Error("dispatch: migrate database", "error", explainMigrateError(err))
		os.Exit(1)
	}
	// Every reader of a pull request's issue runs one statement, built here once for the schema
	// the migrations wrote. It asks the migrations' guard first: a database a logical restore put
	// in an encoding or a collation the reading cannot use brings its schema_migrations with it,
	// so no migration refuses it, and this stops the process before it listens.
	if err := delivery.PrepareAttribution(ctx, database.Pool); err != nil {
		slog.Error("dispatch: prepare the statement of a pull request's issue", "error", err)
		os.Exit(1)
	}
	// Each process remakes the stored keys of another reading from its start, before it listens:
	// a change of the reading by a migration, a version of Postgres or any other cause reaches
	// every row within one interval and one remake, with no App key, no lease and no restart.
	if !boot.KeyRemakeOff {
		go delivery.KeyRemakeLoop(ctx, database.Pool, boot.KeyRemakeInterval)
	}

	// A type is a node of the store, and the schema this process validates writes against and
	// serves to an editor is built from those nodes (migration 20261109051232). The first
	// install is here, beside the migration run: a process serving on a schema that is not the
	// store's would hold documents to types the deployment has moved on from, so a failure
	// stops the boot as a failed migration does. The reinstall on each change is a loop of its
	// own, started beside the other woken loops below.
	if err := schemanodes.InstallFromStore(ctx, database.Pool); err != nil {
		slog.Error("dispatch: install the block schema from the store", "error", err)
		os.Exit(1)
	}

	// The rename pairs are read once, here, before any part that reads them is built: the API,
	// the delivery reconcile and intakes and the CI reconcile are each given this one value
	// (refs.Renames). The delivery reconcile and the intakes share the one Attributor built of it
	// here (delivery.NewAttributor), and the API builds its own of the same pairs (api.NewDeps).
	// A read that fails does not stop the boot: it is logged, it is one failure in the rename
	// pass's summary below (renameRepositories), and the process serves with no pairs, so an old
	// name reads as a repository of its own until a later boot reads the pairs. A pair whose new
	// name GitHub cannot hold is dropped, not applied (readRenames).
	renames, renamesErr := readRenames(ctx, database.Pool, repoRenamesReadTimeout)
	attributor := delivery.NewAttributor(renames)

	// document_search (migration 20261101182447) holds each document's latest search vector,
	// 20261101182454's trigger keeps it in step from the deploy on, and this fills it for the
	// documents stored before that. It runs here, in committed batches, rather than as migration
	// 20261101182501: one transaction over artifact_versions measured 35.5 s on a
	// production-shaped database, and 20261101182454's trigger makes every concurrent version
	// write to a document the pass has already written wait that whole transaction out
	// (20261101182501's comment holds the measurement).
	//
	// A failure stops the boot, unlike the count below: this release's search reads
	// document_search alone, so a process that served with the table unfilled would answer a
	// search with documents missing. The rolling deploy keeps that correct - the release
	// before this one serves every request until this process has filled the table and begins
	// serving.
	if filled, err := database.FillDocumentSearch(ctx); err != nil {
		slog.Error("dispatch: fill document_search for the documents stored before this release", "error", err)
		os.Exit(1)
	} else if filled > 0 {
		slog.Info("dispatch: filled document_search for the documents stored before this release", "rows", filled)
	}

	// session_titles (migration 20261117100008) holds each session's newest stamped title,
	// 20261117100015's trigger keeps it in step from the deploy on, and this fills it for the
	// events stored before that, in committed batches, for document_search's reason above
	// (20261117100016's comment). A failure stops the boot: delivery.ResolveSessionTitles reads
	// session_titles alone, so a process that served with the table unfilled would name every
	// session stored before this release by its id. Production runs one replica under a
	// Kubernetes Deployment's `strategy: Recreate` (README.md, "Database migrations"), so no
	// process serves while this one migrates and fills: the old process has stopped, and this
	// one listens only once the fill has ended.
	if filled, err := database.FillSessionTitles(ctx); err != nil {
		slog.Error("dispatch: fill session_titles for the events stored before this release", "error", err)
		os.Exit(1)
	} else if filled > 0 {
		slog.Info("dispatch: filled session_titles for the events stored before this release", "rows", filled)
	}

	// asks_anchor_artifact_id (migration 0148) indexes the anchors whose artifact_id is a
	// lower-case document id, the form every server-written anchor carries. A row an earlier
	// release stored with another spelling is left out, and settlement records no mark for it:
	// the migration refuses no such row, so the log is where its count is visible. A count that
	// fails is a log line too, never a boot that stops: the server serves without it.
	if outside, err := database.AsksOutsideAnchorArtifactIndex(ctx); err != nil {
		slog.Warn("dispatch: count the anchored asks left out of asks_anchor_artifact_id", "error", err)
	} else if outside > 0 {
		slog.Warn("dispatch: anchored asks left out of asks_anchor_artifact_id: their anchor's artifact_id is not a lower-case document id, so settlement records no mark for them", "count", outside)
	}

	webDistDir, err := defaultWebDistDir(boot.WebDist)
	if err != nil {
		slog.Error("dispatch: resolve web dist dir", "error", err)
		os.Exit(1)
	}
	// The build's assets are in the database before the process serves, before it binds its port
	// or adopts the socket DISPATCH_LISTEN_FD names, so a page of this build that another process
	// serves, during a rolling update or a rollback, finds its chunks here, and this process
	// answers another build's from there (dashboard.Store below). A process that cannot publish
	// exits before it is ready, and a rolling update keeps the processes it would have replaced.
	// The build is what the process registers below as the build it serves, and its entry chunk
	// what GET /api/v1/dashboard/builds answers as this process's.
	dashboardBuild, err := publishDashboard(store.WithTransactionTracking(ctx), database.Pool, webDistDir, dashboardPublishTimeout)
	if err != nil {
		slog.Error("dispatch: publish dashboard assets", "error", err)
		os.Exit(1)
	}

	// The store's construction reads no network: credentials come with the first request.
	fileStore, err := openFileStore(ctx, boot)
	if err != nil {
		slog.Error("dispatch: configure the file store", "error", err)
		os.Exit(1)
	}
	if fileStore != nil {
		slog.Info("dispatch: storing uploaded files in a bucket", "bucket", boot.FileStoreBucket)
	}

	// INFRA-555: neither issuer is read at boot. The sign-in issuer's code flow and the
	// service-account token verifier each discover their issuer in the background from here, and
	// until then what needs one answers 503 (SIGNIN_UNAVAILABLE, OIDC_ISSUER_UNAVAILABLE) while
	// everything else serves; /healthz reports each as `signin` and `service_tokens`.
	signIn := signInFlow(boot)
	if signIn != nil {
		signInRule(boot).BootLog(boot.SignInIssuer, boot.SignInClientID)
		go signIn.Run(ctx)
	}

	people, signingKey, err := openPeople(boot, dataDir, database.Pool, signIn)
	if err != nil {
		slog.Error("dispatch: load signing key", "error", err)
		os.Exit(1)
	}
	retirePlainRefreshTokens(ctx, people, plainRefreshTokenRetireTimeout)
	sessions := store.NewPgSessionStore(database.Pool)

	if boot.IdentityHeader != "" {
		slog.Warn("dispatch: using test/local header identity", "header", boot.IdentityHeader)
	}
	if boot.IAPAudience != "" {
		slog.Info("dispatch: naming people from Identity-Aware Proxy's signed assertion", "audience", boot.IAPAudience, "keys", boot.IAPKeySetURL, "domain", boot.SignInDomain)
	}
	requestIdentity := requestIdentityFor(boot, signingKey, people, sessions, signIn)

	broker := events.NewBroker()
	documentService := newDocumentService(boot, docs.Deps{
		Store:       database,
		Events:      broker,
		Identity:    requestIdentity,
		ServerURL:   serverURL,
		BuildCommit: buildCommit,
	})
	publishRoomFigures(documentService)
	restoreDecidedWordings(ctx, documentService, decidedWordingRestoreTimeout)
	// The asks a close or a reopen by a release older than the withdrawal left: at boot, then on an
	// interval below. It appends events and takes no copy lock, so it runs after
	// restoreDecidedWordings, which takes the copy lock (see below).
	issueAskRepairDeps := api.Deps{Store: database, Docs: documentService, Events: broker}
	repairIssueAsks(ctx, issueAskRepairDeps, issueAskRepairBootTimeout)
	fillReviewContentHashes(ctx, database.Pool, reviewContentHashFillTimeout)
	renameRepositories(ctx, database.Pool, renames.Pairs(), renamesErr, repoRenamePassTimeout)

	serviceTokens := serviceTokenVerifier(boot)
	if serviceTokens != nil {
		slog.Info("dispatch: verifying service-account tokens", "issuer", boot.OIDCIssuer, "audience", boot.OIDCAudience)
		go serviceTokens.Run(ctx)
	}

	// embedder is nil (meaning search off, search answers keyword-only and says so) when Bedrock,
	// the default place, finds no AWS region/credentials at boot - a CI job or a devbox with no
	// AWS_REGION set - which newEmbedder logs as a warning rather than a reason to refuse to boot.
	// DISPATCH_TEST_EMBEDDER_OFF=1 is the same nil without that check, for a test process that
	// does have the variables and no model service to spend them on.
	var embedder embed.Embedder
	if boot.EmbedderOff {
		slog.Warn("dispatch: meaning search unavailable", "error", "DISPATCH_TEST_EMBEDDER_OFF=1")
	} else {
		client, err := newEmbedder(ctx, boot.Embedder)
		if err != nil {
			slog.Error("dispatch: build the embedder", "error", err)
			os.Exit(1)
		}
		embedder = client
	}

	// answerer is nil where the deployment configured no model, which newAnswerer logs: the
	// server boots and says it cannot answer. A shape it could not build from is a refusal to
	// boot, as the embedder's is, since the deployment asked for a model and got none.
	answerer, err := newAnswerer(boot.Answerer)
	if err != nil {
		slog.Error("dispatch: build the answerer", "error", err)
		os.Exit(1)
	}

	// The answers reads this process serves, each timed from the moment it enters the route's
	// handler chain, as its access-log line times it: the API records them, and the keyword fill
	// (textversions.Run, below) pauses on them, so the two share this one Latency.
	answersLatency := sessionanswers.NewLatency(time.Now)
	built := routes.AppContextOptions{
		SigningKey:     signingKey,
		WebDistDir:     webDistDir,
		AssetStore:     dashboard.NewStore(database.Pool),
		DashboardEntry: dashboardBuild.Entry,
		BuildCommit:    buildCommit,
		People:         people,
		Sessions:       sessions,
		Identity:       requestIdentity,
		SignIn:         signIn,
		Store:          database,
		ServerURL:      serverURL,
		Docs:           documentService,
		Embedder:       embedder,
		Events:         broker,
		Renames:        renames,
		App:            appCfg,
		AgentStream:    agentStream,
		AgentPublisher: agentPublisher,
		Files:          fileStore,
		Lifetime:       ctx,
		AnswersLatency: answersLatency,
	}
	// Set only when configured: a nil *oidc.LazyVerifier in the interface field would read as a
	// configured verifier, and every JWT-shaped bearer would be sent to it.
	if serviceTokens != nil {
		built.OIDC = serviceTokens
	}
	// Set only when configured, for the reason above: a nil *going.Answerer in the interface
	// field would read as a model configured, and the capability route would say so.
	if answerer != nil {
		built.Answerer = answerer
	}
	brokerTokens, err := brokerTokenVerifier(boot, database, serverURL)
	if err != nil {
		slog.Error("dispatch: build the broker access-token verifier", "error", err)
		os.Exit(1)
	}
	built.BrokerTokens = brokerTokens
	// The registry /metrics serves (dispatchHandler), built whether or not NATS is configured:
	// the probe answers from the moment the server does, and a process with no bus simply leaves
	// the outbox's counter at zero. The API adds the gauges of the mention sends it holds behind
	// an earlier event to it (api.Deps.Metrics), and the outbox those of its fallback copies.
	met := metrics.New()
	built.Metrics = met
	if brokerTokens != nil {
		slog.Info("dispatch: deciding access tokens the secrets broker issued", "broker", redact.URL(boot.AgentSecretsURL))
	}

	// Two or more envoy-dispatch processes serve one Postgres and one NATS, so each background
	// loop below either runs once overall or is deliberately per-process. The ones that must run
	// once overall take a named lease on this pool for the length of their pass (duty.Exclusive):
	// a lease is a session-level advisory lock on a connection of its own, so it needs a pool the
	// shared one's request connections are not spent on. A boot that cannot open it is a boot
	// whose loops would each run everywhere, so it ends here.
	//
	// The loops come in these shapes, so that a database nobody writes costs them nothing except
	// where GitHub gives them nothing to wake on, and the seven timers of the last shape:
	//   - woken loops, which run a pass at start and then only when a Postgres notification on the
	//     shared listener below says their work may have changed, or when a due time their last
	//     pass asked for comes round: the embedding queue (embedqueue.WakeChannel), the reference
	//     excerpts (docs.ExcerptsOwedChannel), the edit notices (docs.EditNoticesChannel), the
	//     suggestion outcomes (api.EventsChannel) and the delivery settings both intakes route by
	//     (delivery.SettingsChannel). The two leased ones, reference-excerpts and edit-notices,
	//     take their lease with no period (duty.RunWoken): a process that finds it held retries
	//     until a pass of its own runs, since the other process's pass may have read before the
	//     change that woke this one. The suggestion-outcome loop takes none: each of its pages
	//     runs in a transaction holding the suggestion_outcome_cursor row for update, which
	//     serialises two processes woken by one commit, the first pass on a database included
	//     (api/suggestion_outcomes.go). The keyed writes' own sweep - the Idempotency-Keys past
	//     their window, and the deliveries a process that died left owed - keeps a timer of its
	//     own below, because no wake announces either: a death commits nothing.
	//   - boot passes, which run once at start: the embedding coverage pass (embedqueue.Run starts
	//     it, and repeats its identity steps and its record of issues' inputs every 10 minutes for
	//     as long as the process runs) and the task progress reconciliation (retrying, briefly,
	//     rows another writer held).
	//   - timers that stay, because GitHub has no feed of what they would miss - a webhook that
	//     never arrived, a delivery that failed: the delivery reconcile and the architecture
	//     import every 5 minutes, each starting after a random delay within one interval
	//     (duty.RunJittered), and the webhook redelivery sweep every 2, sweeping at start
	//     (redeliver.Sweeper.Run). A push of a source's branch imports that source at once
	//     (delivery.PushImports, architecture.Importer.Pushed), so the architecture import's
	//     timer repairs a push that never arrived. Each then
	//     waits its interval less up to a tenth of it, drawn afresh every time
	//     (duty.JitteredWithin), so no wait is longer than
	//     the interval and two processes' ticks drift apart rather than staying in step. Under
	//     the lease each of these passes is recorded with its loop's own period, 0.9 of its
	//     interval (duty.LeasePeriod, duty_passes, migration 0115), which every wait is longer
	//     than: that is what makes it run once per period rather than once per process, and
	//     never refuses a lone process's tick. Two tickers started minutes apart almost never
	//     meet inside a pass, so the lease alone would exclude nothing.
	//   - timers that poll the database: the block-index reconciliation (every 30 s, 2 s while a
	//     batch is full), the reference forms drain (docs.Service.RunRefsFormsReconciliation, at
	//     the same pace under a lease of its own), the repair of closed and reopened issues' asks
	//     (api.RunIssueAskRepair, every 10 minutes on a plain ticker, after the boot's own
	//     unleased pass), the prune of the server process registry (processes.RunPrune, every
	//     minute, which joinRegistry starts below), the keyed writes' maintenance sweep
	//     (api.RunMaintenanceSweep, every minute), the document compaction sweep
	//     (docs.Service.RunCompactionSweep, every hour from a tick at start, with or without NATS)
	//     and the display tree fill (texttreefill.Run, every hour from a tick at start, whose
	//     first pass of a display parser version runs until every stored text has a tree at it),
	//     each recording its passes in duty_passes like the timers above; each tick sends its
	//     statements whether or not anything is owed.
	//
	// The shared listener is one pglisten.Hub per process, on one held connection of the store's
	// listener pool, carrying every woken loop's channel and the document rooms' room-sync
	// channels alike. Every wake and handler is registered before it runs (a channel added later
	// would never be LISTENed on), and no loop waits for it: each runs its boot pass whether or
	// not the hub has connected. Postgres does not replay a notification sent while nothing
	// listened, so the hub signals every wake after each successful LISTEN, and a change made
	// while it was redialing (backing off up to 30 s) is caught up then.
	//
	// The events outbox publishes under a hold of its own rather than such a lease
	// (outbox.Outbox): it renews duty.Hold "dispatch-outbox" on its own pool every second, so
	// one process publishes at a time and the other takes over within seconds of a holder that
	// stops, dies or loses NATS.
	//
	// The keyword fill writes search_text_versions under a hold of its own as well
	// (textversions.Run): one process fills at a time, and the others stand by for its hold.
	//
	// Deliberately NOT leased, in three classes:
	//   - already claims its own work, so two processes never do one unit of it twice: the
	//     embedding queue (embedqueue, `for update skip locked`), the task progress
	//     reconciliation (`for no key update ... skip locked`) and the delivery intakes (each
	//     delivers from its durable only while it holds that durable's duty.Hold, which
	//     delivery.StartIntakes takes).
	//   - per-process by design, because each serves this process's own connections or memory:
	//     the delivery live hub and its listener (migration 0107_events_notify.up.sql states that
	//     purpose), the event stream relay, the document rooms' timers, the document compaction
	//     queue (docs.New starts it; it compacts the documents this process's own appends and
	//     loads noted, and the hourly compaction sweep below is the leased pass over the
	//     documents doc_compaction_owed names, those with rows to fold), the process's shared
	//     listener hub (its connection is this process's membership and every loop above waits on
	//     a wake of this process's own, so it has to run in every process), the room sync
	//     registered on it (it serves the rooms resident in this process: it catches them up with
	//     the other processes' writes and answers their room queries) and the document sockets'
	//     schema re-check (it closes this process's own sockets).
	//   - a boot one-shot already serialised by an advisory lock or idempotent: the migrations,
	//     retirePlainRefreshTokens, restoreDecidedWordings, repairIssueAsks and
	//     renameRepositories above, and the embedding coverage pass (two processes' passes at
	//     once leave what one leaves).
	leaseCallers, err := database.Pool.Leases()
	if err != nil {
		slog.Error("dispatch: open the lease pool for the maintenance loops", "error", err)
		os.Exit(1)
	}
	// reserve takes the lease pool's connection for one caller, by a name that says which caller
	// it is, and ends the process if that name has reserved one already. Every caller reserves
	// just below, before any loop starts and whether or not this configuration runs its loop, so
	// every configuration reserves the same callers; leaseCallers.Complete then checks them
	// against the pool's size while nothing of this process is yet at work.
	//
	// The check sees only callers that reserve. For hands every name the same pool, so a
	// goroutine added inside a caller that has reserved already, or one reservation variable
	// passed to a second consumer, takes a connection nothing counted, and the count still
	// matches. Each caller that can hold a connection while another does needs a reserve of its
	// own here.
	reserve := func(name string) duty.Leases {
		leases, err := leaseCallers.For(name)
		if err != nil {
			slog.Error("dispatch: reserve a connection of the lease pool", "caller", name, "error", err)
			os.Exit(1)
		}
		return leases
	}

	// Every caller's connection, taken here, before the first loop below starts. Each
	// reservation goes to the one consumer named after it.
	settlementResumptionLeases := reserve("settlement resumption")
	searchTermStatsLeases := reserve("search term statistics")
	referenceExcerptLeases := reserve("reference excerpts")
	documentCompactionLeases := reserve("document compaction")
	editNoticesLeases := reserve("edit notices")
	blockIndexLeases := reserve("block index")
	// The reference forms drain's, a caller of its own beside the block index's: the two passes
	// run on their own goroutines under their own leases, so each can hold a connection while the
	// other does.
	refsFormsLeases := reserve("refs forms")
	issueAskRepairLeases := reserve("issue ask repair")
	// The webhook redelivery sweep's and the CI reconcile pass's, reserved whether or not this
	// configuration runs them, as every caller's here is: the count the pool is sized for is the
	// same in every configuration.
	redeliverLeases := reserve("webhook redelivery")
	ciReconcileLeases := reserve("CI reconcile")
	// The CI verify loop's (cireconcile.Reconciler.Verify), apart from the reconcile pass's: a
	// reconcile pass holds its lease connection for up to five minutes while a verify pass that has
	// requests to answer holds another under its own lease.
	ciVerifyLeases := reserve("CI verify")
	architectureImportLeases := reserve("architecture import")
	deliveryReconcileLeases := reserve("delivery reconcile")
	maintenanceSweepLeases := reserve("maintenance sweep")
	// The registry's prune (processes.RunPrune), which the registration below starts: reserved
	// here so that this process's count is the same whether or not the registry admits it.
	registryPruneLeases := reserve("process registry prune")
	// The embeddings project_key fill (api.RunEmbeddingProjectKeyFill): it is leased like any
	// loop, and it holds its connection for the one pass of one to two hours it makes, so it
	// reserves a caller of its own here as the loops do.
	embeddingProjectKeyFillLeases := reserve("embeddings project key fill")
	// The display tree fill (texttreefill.Run): a leased loop like the rest, reserved here so
	// that this process's count is the same whether or not it has work to do.
	textTreeFillLeases := reserve("text tree fill")
	// The Legion sampler (legion.RunSampler): a leased loop like the rest, reserved here so that
	// this process's count is the same whether or not any project names a daemon.
	legionSamplerLeases := reserve("Legion sampler")
	// R25's one-time fix of parents' statuses at boot (api.RunParentStatusFixAtBoot, below): it
	// takes its lease only with DISPATCH_PARENT_STATUS_RULE on, and is reserved here so that this
	// process's count is the same with the setting off.
	parentStatusFixLeases := reserve("parent status fix")
	// The intakes' six callers - a consumer lease, a hold and the hold's renewal per durable -
	// whether or not this configuration starts them (delivery.ReserveIntakeLeases).
	intakeLeases, err := delivery.ReserveIntakeLeases(leaseCallers)
	if err != nil {
		slog.Error("dispatch delivery: reserve the lease pool for the intakes", "error", err)
		os.Exit(1)
	}
	// The keyword fill's two callers - its own goroutine's hold statements, batches and reads, and
	// its hold's renewal (textversions.ReserveLeases).
	textVersionsLeases, err := textversions.ReserveLeases(leaseCallers)
	if err != nil {
		slog.Error("dispatch: reserve the lease pool for the keyword fill", "error", err)
		os.Exit(1)
	}
	// Every caller of the lease pool has reserved its connection now, so the count is this
	// process's real one: a caller short of a connection would wait for one for as long as the
	// longest pass - minutes, for the compaction - with an intake's hold reported lost
	// meanwhile, and a connection short of a caller is one the process opens and nothing uses.
	// Either is a wiring mistake, so the process refuses to serve rather than run short. Nothing
	// has started yet - no loop, no intake, no listener - so this exit has nothing to stop.
	if err := leaseCallers.Complete(); err != nil {
		slog.Error("dispatch: the lease pool's callers are not its connections", "error", err)
		os.Exit(1)
	}

	// R25's one-time fix, with DISPATCH_PARENT_STATUS_RULE on (api.RunParentStatusFixAtBoot): one
	// process unlinks each open issue from its done parent and moves each not-started ancestor of an
	// issue in work to in_progress, under the lease parent-status-fix, and then records that it ran.
	// A process that finds the lease held waits for that record. The fix runs here, before the
	// outbox, every loop and the listen, so the server takes no write and no loop writes until it is
	// over. With the setting off it reads and writes nothing. A stop during the wait is a clean stop,
	// as one during the registry's admission wait below is; a step that fails ends the boot with no
	// record, and the next boot plans again from what stands.
	parentStatusRule, err := api.RunParentStatusFixAtBoot(store.WithTransactionTracking(ctx),
		api.Deps{Store: database, Docs: documentService, Events: broker}, parentStatusFixLeases, boot.ParentStatusRule)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			slog.Info("dispatch: stopped while waiting for the parent status fix")
			return
		}
		slog.Error("dispatch: run the parent status fix", "error", err)
		os.Exit(1)
	}
	// Every project has an owner: a project the access migration left with none is given to
	// DISPATCH_PROJECT_OWNER_FALLBACK's full member, recorded as an operator's grant is
	// (giveOwnerlessProject), and while one still has none the boot refuses, naming each and both
	// repairs. The grant appends events, so it runs after every step of the boot that takes the copy
	// lock (restoreDecidedWordings above, the parent status fix just before it), and before the
	// outbox, every loop and the listen, so the server serves no project without an owner.
	if err := store.EnsureProjectOwners(store.WithTransactionTracking(ctx), database.Pool, boot.ProjectOwnerFallback, giveOwnerlessProject); err != nil {
		slog.Error("dispatch: give every project an owner", "error", err)
		os.Exit(1)
	}
	// The app context is built only now, so the rule the fix answered reaches the server
	// (api.DepsInput through appContextOptions). Nothing above reads it.
	built.ParentStatusRule = parentStatusRule
	appCtx, err := routes.BuildAppContext(appContextOptions(boot, built))
	if err != nil {
		slog.Error("dispatch: build app context", "error", err)
		os.Exit(1)
	}

	// The outbox runs only with NATS, so a process with NATS disabled never publishes
	// (outboxPublisher stays nil, and /healthz reports outbox_publisher false). outboxStopped is
	// closed once its Run has returned - its last batch recorded and its hold released - which
	// shutdown waits for before it closes the pool; it stays nil with no outbox.
	var outboxPublisher *outbox.Outbox
	var outboxStopped chan struct{}
	outboxPublishFailures := met.NewCounter(
		"envoy_dispatch_outbox_publish_failures_total",
		"Destinations of an outbox publish attempt that were not reached, by reason: denied (the server refused the send under the connection's own publish permissions), refused (NATS refuses it however often it is retried), unanswered (NATS took the forward and the stream did not acknowledge the copy it stored, which is the whole of a forward's answer) or failed (every other failure)",
	)
	// The holds beside the failures: a role-routed event whose holder is not answering is waiting
	// for that session, with nothing sent and nothing failed, so it is counted here rather than
	// under the publish failures an operator alerts on (outbox.Deps.RoleForwardsHeld).
	outboxRoleForwardsHeld := met.NewCounter(
		"envoy_dispatch_outbox_role_forwards_held_total",
		"Role forwards an event's holder resolution held pending with nothing sent, by the holder's state: unresponsive (the holder is past the live window), undated (nothing dates the holder) or unresolved (the role claim or the holder's session could not be read)",
	)
	if natsClient != nil {
		outboxPool, err := database.Pool.Outbox()
		if err != nil {
			slog.Error("dispatch: open the events outbox's pool", "error", err)
			os.Exit(1)
		}
		// One reader of the role and session buckets serves both: a role route's holder is read
		// straight from the buckets on the bus client's connection, with no watcher, and judged
		// by the windows this deployment gave its listeners (roleHolderWindows).
		holders := roles.NewBuckets(natsClient, boot.RoleHolderWindows)
		outboxPublisher = outbox.New(outbox.Deps{
			Store:            &store.Store{Pool: outboxPool},
			Publisher:        natsClient,
			Broker:           broker,
			Listen:           database.Pool.Listener,
			RoleClaims:       holders,
			Sessions:         holders,
			KeptLanes:        natsClient,
			PublishFailures:  outboxPublishFailures,
			RoleForwardsHeld: outboxRoleForwardsHeld,
			Metrics:          met,
		})
		outboxStopped = make(chan struct{})
		go func() {
			defer close(outboxStopped)
			outboxPublisher.Run(ctx)
		}()
	}
	// embedqueue.Run no-ops when its own Deps.Embedder is nil, so this always starts: the server's
	// embedding queue keeps coverage itself, so a later deploy that gives meaning search a place
	// to embed needs no other wiring change and no person-run step to cover existing content.
	// It gets a rate-limited wrapper around the same client api.Deps holds unwrapped above
	// (embedder, BuildAppContext's Embedder option): the queue is the only embedqueue caller, and
	// pacing only its calls - never a live search request's own query embedding, which always
	// goes straight through the unwrapped client - is what keeps saturating background work from
	// taking live search down with it (LEGION-549 round 5; embed.RateLimitedEmbedder's own doc
	// comment has the AIMD mechanism and the headroom this leaves). A nil embedder (meaning
	// search off) stays nil rather than becoming a non-nil wrapper around nothing, which
	// embedqueue.Run's own nil check depends on. The queue starts before this process registers
	// below; it embeds and drops only while its embedder's identity is the active one, which every
	// process decides from the live registry rows (embedqueue, active.go), so a process configured
	// with another embedder than the fleet's leaves the vectors as they are.
	var backgroundEmbedder embed.Embedder
	if embedder != nil {
		backgroundEmbedder = embed.NewRateLimitedEmbedder(embedder)
	}

	// This process's one shared LISTEN connection. Its name is what makes this process a member:
	// the other envoy-dispatch processes read it in pg_stat_activity (docs/room_sync.go), and only
	// one connection per process can answer for that, so the woken loops' wakes and the document
	// rooms' room sync are registered on the one hub. The delivery hub and the event stream relay
	// still hold listener connections of their own (store.Pool.Listener).
	//
	// A wake is registered only for a loop that will read it: with no embedder embedqueue.Run
	// returns at once, without NATS or the GitHub App no intake starts to route by the settings,
	// and without the GitHub App this process keeps no copy of the GitHub reads' record (below),
	// so none of those channels is LISTENed on.
	listenerHub := &pglisten.Hub{
		Owner: "dispatch",
		Dial: func(ctx context.Context) (*pgx.Conn, error) {
			return database.Pool.Dedicated(ctx, docs.ListenerApplicationPrefix+documentService.ProcessID())
		},
	}
	var embeddingsWake, settingsWake, gitHubReadsWake <-chan struct{}
	if backgroundEmbedder != nil {
		embeddingsWake = listenerHub.Wake(embedqueue.WakeChannel)
	}
	excerptsWake := listenerHub.Wake(docs.ExcerptsOwedChannel)
	editNoticesWake := listenerHub.Wake(docs.EditNoticesChannel)
	eventsWake := listenerHub.Wake(api.EventsChannel)
	// The block schema's own wake. It is a Wake and not a Handle because the reinstall reads
	// the store, which a handler on Run's goroutine must not do (schemanodes.Installer).
	schemaWake := schemanodes.RegisterWake(listenerHub)
	if natsClient != nil && appCtx.GitHub() != nil {
		settingsWake = listenerHub.Wake(delivery.SettingsChannel)
	}
	if appCfg != nil {
		gitHubReadsWake = listenerHub.Wake(githubreads.Channel)
	}
	// The keyword fill's own wakes (textversions.Run): a commit that wrote an event wakes its
	// holder's next pass, and a hold's release its standby's next try for the hold. A Wake is a
	// channel of its own with one buffered signal, which one reader takes, so the fill registers
	// its own on api.EventsChannel rather than reading the suggestion sweep's eventsWake.
	textVersionsEventsWake := listenerHub.Wake(api.EventsChannel)
	textVersionsReleasedWake := listenerHub.Wake(duty.HoldReleasedChannel)

	go embedqueue.Run(ctx, embedqueue.Deps{Store: database, Embedder: backgroundEmbedder, Wake: embeddingsWake})
	// The block schema the store holds, read again each time a supertag write announces itself.
	// A read that fails, and a value the install refuses, leaves the installed schema in place
	// and is read again; a store a newer build migrated is installed at this build's own
	// version (pmdoc.Install). A read that moved the schema this process serves first writes the
	// numbers it serves now into its registry row, which every process's socket admission reads,
	// and then closes its document sockets, so each editor connects again and its admission
	// decides against the schema installed now, rather than writing against types this process
	// no longer holds. The registration is made further down, once this process holds its
	// listener, so the installer reaches it through registered.
	var registered atomic.Pointer[processes.Registration]
	go (&schemanodes.Installer{
		Store: database.Pool,
		Wake:  schemaWake,
		OnSwitch: func() {
			if registration := registered.Load(); registration != nil {
				refreshRegistrySchema(ctx, registration)
			}
			documentService.CloseDocumentSockets()
		},
	}).Run(ctx)
	// project_key (migration 20261109120000) is the project each embeddings row belongs to, the
	// predicate every arm of a project-scoped meaning search reads (api/search.go). Both write
	// functions set it from the deploy on, and this labels the rows stored before it, in
	// committed batches of its own (20261109120100 holds the cursor it records its place in).
	//
	// In the background, where the document_search fill above blocks the boot: a label costs
	// about 253 bytes of heap and 4.9 KiB of growth in the vector index, and the pass is one to
	// two hours on a deployment of a million rows (INFRA-988, store.EmbeddingProjectKeyBatch), so
	// a boot that waited for it would outlast any rollout. What waiting would buy is nothing a
	// search needs: an unlabelled row is invisible to the column's own predicate, which is worse
	// than the release before this one, so a project-scoped meaning search is answered by the
	// owner each arm joins, as that release answered it, until this pass records itself done
	// (api/search.go, meaningProjectScope), and every batch moves rows onto the faster predicate
	// the pass exists for. A failure is logged and the process serves on, for the same reason.
	//
	// Here, among the loops, and not beside the document_search fill above, because it needs the
	// lease pool, which is opened below that: one process of a roll fills and the other skips,
	// since two of them would each pay the same hours for one result. The process that skipped
	// tries again every five minutes until the labelling is over, so a holder killed mid-pass
	// costs the fill that wait rather than lasting until something next boots
	// (api.RunEmbeddingProjectKeyFill, which says why the retry is written there and not taken
	// from duty's loops). It is the one leased caller here that stops of its own accord, so its
	// goroutine and its connection of the lease pool go back for good once the rows are labelled.
	go api.RunEmbeddingProjectKeyFill(ctx, database, embeddingProjectKeyFillLeases)
	// A settlement a shutdown cut short, here or in the task this one replaces, runs without
	// anyone opening its document. One process per period arms them (docs.RunSettlementResumption).
	go documentService.RunSettlementResumption(ctx, settlementResumptionLeases)
	// The document rooms' room sync, registered on the hub above and run beside it: a document
	// written by another process is announced over Postgres, and the room this process holds for
	// it catches up from the stored updates within that round trip, so its browsers see the write
	// without a reconnect. The hub takes no registration once it runs, so every wake above and
	// every room-sync handler is registered before the line that starts it. Both stop with ctx,
	// like the loops beside them; the service's NOTIFY sender stays open until
	// documentService.Shutdown has drained the rooms.
	runRoomSync := documentService.RegisterRoomSync(listenerHub)
	go runRoomSync(ctx)
	go listenerHub.Run(ctx)
	// Every live window each document socket this process serves that is writable is
	// checked again against the live server processes' schema numbers (registered below), and one
	// the fleet no longer admits closes with contracts.DocumentAdmissionRevokedCloseCode, once the
	// message in flight on it is out, on which the provider reconnects and is admitted again
	// (docs.Service.authorize).
	go documentService.RunSchemaAdmission(ctx)
	// The related-item check's keyword statistics (each project's df and N), rebuilt at
	// boot and every ten minutes.
	go api.RunSearchTermStatsRefresh(ctx, database.Pool, api.SearchTermStatsInterval, searchTermStatsLeases)
	// Issues whose spec's task count is not their latest version's - every row the migration that
	// added the count columns found, and any the task this one replaces versions while both run -
	// are counted again once at start, so their progress shows without anyone editing them
	// (LEGION-542). Every version write records its count in its own transaction, so no timer
	// follows: only rows another writer held during the pass are retried, briefly.
	go documentService.RunTaskProgressReconciliation(ctx)
	// The references route serves each document mention's stored excerpt; this fills the ones no
	// version write stored: every mention indexed before migration 0092, and each rebuild-refs
	// inserts (LEGION-531), woken by the refs trigger that announces each mention left owed.
	go documentService.RunReferenceExcerptReconciliation(ctx, referenceExcerptLeases, excerptsWake)
	// Documents the compaction queues let go - every room a restart, a Shutdown or a Quiesce
	// dropped from one - fold within an hour of coming due, whether or not NATS is configured
	// (INFRA-593).
	go documentService.RunCompactionSweep(ctx, documentCompactionLeases)
	// Every stored text written before INFRA-635 stored display trees, and every stored text at
	// all after a deploy raises pmdoc.DisplayTreeVersion, gets its tree without waiting for a
	// reader to open it; the same pass drops the trees of every version before the two newest,
	// which is what lets a rolling deploy and a rollback read each other's rows.
	go texttreefill.Run(ctx, database.Pool, textTreeFillLeases)
	// Each document's edit notice goes once its quiet window after a person's last
	// settled version ends, as one artifact.human_edited event; the window is a row, so a notice
	// still pending when this process stops is sent by the next sweep of either process. The sweep
	// is woken by the trigger that announces each notice given a due time, and waits for the
	// earliest one.
	go documentService.RunEditNotices(ctx, editNoticesLeases, editNoticesWake)
	// The block index: each document version no block-aware write indexed - every document at
	// migration 0121, each version a build without the block index writes during a roll, and each
	// rebuild-refs marks - and each comment, ask and issue message citing a block (`#b-`) that such
	// a build indexed without the block grammar.
	go documentService.RunBlockIndexReconciliation(ctx, blockIndexLeases)
	// The reference forms drain: each comment, ask, issue message and document a build that
	// predates an issue's tab addresses and a document's dashboard URLs indexed without them
	// (refs_forms_owed). A loop of its own under a lease of its own, on the block index's
	// interval: a period a process of the previous release served under the block index's lease
	// would otherwise drain none of it, and a document's row is settled only by this build's own
	// index of the document.
	go documentService.RunRefsFormsReconciliation(ctx, refsFormsLeases)
	// The asks a process older than the withdrawal leaves on an issue it closes or
	// reopens during a roll are withdrawn or restored every interval after the boot's own pass.
	go api.RunIssueAskRepair(ctx, issueAskRepairDeps, issueAskRepairLeases)

	// Each of the four loops below that reads GitHub under a lease records its newest attempt in
	// the record of GitHub reads (githubreads, store.GitHubReadAttempts) when its pass ends, and
	// this process keeps a copy of the record, loaded at start and again at every write any
	// process makes (the wake above), which GET /healthz answers from (gitHubReads), so the probe
	// sends the database nothing. Without the GitHub App no loop reads GitHub, and there is
	// nothing to record or keep.
	var (
		recordGitHubRead githubreads.Recorder
		gitHubReadCopy   *githubreads.Copy
	)
	if appCfg != nil {
		attempts := store.NewGitHubReadAttempts(database.Pool)
		recordGitHubRead = attempts.Record
		gitHubReadCopy = &githubreads.Copy{}
		go gitHubReadCopy.Run(ctx, attempts.All, gitHubReadsWake, slog.Default())
	}

	sweeper, err := webhookSweeper(natsClient, database, appCfg, boot.GitHubAPIBase, boot.NATSReplicas)
	if err != nil {
		slog.Error("dispatch: open webhook redelivery", "error", err)
		os.Exit(1)
	}
	redeliveryInterval := redeliver.Interval
	if boot.RedeliveryInterval > 0 {
		redeliveryInterval = boot.RedeliveryInterval
	}
	if sweeper != nil {
		sweeper.GitHubReads = recordGitHubRead
		slog.Info("dispatch: redelivering the GitHub App webhook's failed deliveries", "client_id", appCfg.ClientID, "interval", redeliveryInterval)
		go sweeper.Run(ctx, redeliveryInterval, redeliverLeases)
	} else {
		slog.Info("dispatch: webhook redelivery off: it needs NATS and the GitHub App private key")
	}

	// INFRA-1211: a webhook that never arrived leaves a head short of a check, and a head short
	// of a check never settles, so every interval one process reads each open pull request's head
	// from GitHub and records the completed checks Envoy's record of that head lacks. It is a
	// timer that stays, as the webhook redelivery sweep and the delivery reconcile beside it are:
	// GitHub has no feed of the webhook it never delivered, so there is nothing to wake on.
	ciReconcileInterval := cireconcile.Interval
	if boot.CIReconcileInterval > 0 {
		ciReconcileInterval = boot.CIReconcileInterval
	}
	// The interval is the pass's own bound as well as the loop's period: a pass reads from the
	// cursor the pass before it left and ends on that bound, so the loop's lease is free when
	// the next pass is due and the one line each pass logs says how far round the cycle it got.
	// ciInstance is filled in once this process is registered, below, and each pass records its
	// counts in ci_reconcile_passes under the id it then holds.
	ciInstance := &registeredInstance{}
	reconciler, err := ciReconciler(natsClient, database, appCfg, boot.GitHubAPIBase, boot.ReviewerAppID, boot.VerdictAppID, ciReconcileInterval, ciInstance, renames)
	if err != nil {
		slog.Error("dispatch: open the CI reconcile pass", "error", err)
		os.Exit(1)
	}
	if reconciler != nil {
		slog.Info("dispatch: reconciling the CI records GitHub's webhook never delivered", "client_id", appCfg.ClientID, "interval", ciReconcileInterval)
		if boot.ReviewerAppID == "" {
			slog.Info("dispatch: the CI reconcile pass records no tester or architect check run: DISPATCH_REVIEWER_APP_ID is unset, so no App's verdict checks are trusted")
		}
		if boot.VerdictAppID == "" {
			slog.Info("dispatch: the CI reconcile pass leaves no App's check suites out: DISPATCH_VERDICT_APP_ID is unset; give it the listener's ENVOY_VERDICT_APP_ID")
		}
		reconciler.GitHubReads = recordGitHubRead
		// The period argument is what makes one process serve each period (duty_passes): the
		// lease alone only keeps two passes from overlapping, and the App's budget pays for every
		// pass that runs.
		go duty.RunLeased(ctx, ciReconcileLeases, cireconcile.Lease, ciReconcileInterval, duty.TickAfterInterval, reconciler.Pass)
		// A listener with ENVOY_CI_VERIFY on publishes a head's settlement only after this loop has
		// read the head from GitHub after it asked. It lists the requests every VerifyInterval with
		// no lease, and answers any it finds under a lease of its own (VerifyLeases), so a pass with
		// nothing to answer takes no lease connection and logs nothing, and a reconcile pass that
		// runs for minutes holds none back.
		reconciler.VerifyLeases = ciVerifyLeases
		go duty.RunWoken(ctx, duty.WokenLoop{
			MinGap: cireconcile.VerifyInterval,
			Pass:   reconciler.Verify,
		})
	} else {
		slog.Info("dispatch: CI reconcile off: it needs NATS and the GitHub App private key")
	}

	appCtx.Architecture().GitHubReads = recordGitHubRead
	go architecture.Run(ctx, appCtx.Architecture(), architectureImportLeases, boot.ArchitectureImportAtStart)

	// dispatch://INFRA-1077: each project whose legion_settings row names a Legion daemon has its
	// `GET /legion/v1/state` read once a minute from one replica, under the `legion-state` lease,
	// and every value that moved recorded. It is a timer and not a woken loop for the reason the
	// delivery reconcile below is: a remote daemon announces nothing Dispatch can listen for.
	go legion.RunSampler(ctx, database.Pool, legionSamplerLeases)

	// LEGION-567: the delivery timeline's 5-minute GitHub-App reconcile runs regardless of NATS
	// (it never touches it); the NATS consumer that catches events between reconcile passes needs
	// a client, exactly like the outbox above. Both take the event broker: each writes
	// issue.deployed on a pull request's issue once its shipping run is known (LEGION-781).
	reconcile := delivery.NewReconcile(database.Pool, appCtx.GitHub(), broker, attributor).RecordGitHubReads(recordGitHubRead)
	if boot.ReconcileAtStart {
		reconcile.FirstPassAtStart()
	}
	if boot.DeliveryReconcileInterval > 0 {
		reconcile.TickEvery(boot.DeliveryReconcileInterval)
	}
	go reconcile.Run(ctx, deliveryReconcileLeases)
	// waitIntakes waits for both intakes to stop once ctx is cancelled; StartIntakes returns it as
	// a sync.OnceFunc, so a second call returns once the first has. The shutdown runs it before it
	// closes the database pool: it closes the intakes' NATS clients after their Runs return, then
	// waits for any hand-back finalizer to release its duty.Hold through that pool. A hold left in
	// place for its TTL names the stopped process's deliver subject, which a process of the build
	// before holds, binding by lookup, goes on delivering at, so the next holder would read that
	// binding as a dead holder's and take it over. Every exit after the intakes start runs it too
	// (exitAfterIntakes, stopIntakesOnExit, and the stop during the registry's admission wait), since
	// each intake can hold its durable before this process serves anything. With no NATS client there
	// is nothing to wait for.
	waitIntakes := func() {}
	if natsClient != nil {
		// The intake and the job intake (LEGION-690) each bind a durable, so each takes a client of
		// its own (delivery.StartIntakes). Their Runs begin stopping at the signal beside the HTTP
		// drain and document shutdown, and return once a message in a handler has been acknowledged or
		// given up at its stop budget and handed back. StartIntakes then closes both clients, waits for
		// each pending hand-back's finalizer to release the hold through the pool, and returns. Each
		// connects in the background as the publisher does, and its poll takes the durable's hold once
		// it has, whether or not delivery settings are saved. The intake hands each push of a branch
		// to the architecture importer the ticker and the routes share, which imports the enabled
		// sources on that repository and branch at once; a push it hands over once the importer's
		// Close has begun starts nothing (architecture.Importer.Pushed).
		connectIntake := func(name string) (*bus.Client, error) {
			return bus.Connect(envoyConfig.Envoy.NatsURLs, serverClient(bus.WithEnvironment(env.lookup), bus.WithName(name))...)
		}
		stopIntakes, err := delivery.StartIntakes(ctx, connectIntake, database.Pool, intakeLeases, appCtx.GitHub(), broker, settingsWake, appCtx.Architecture(), attributor)
		if err != nil {
			slog.Error("dispatch delivery: connect NATS", "error", err)
			os.Exit(1)
		}
		waitIntakes = stopIntakes
		// A stop before serving (the admission wait, below) returns without shutdown, and a panic
		// before serving unwinds past it, so the intakes are stopped here too; after shutdown has
		// waited for them, this returns at once.
		defer stopIntakesOnExit(cancel, waitIntakes)
	} else {
		slog.Info("dispatch delivery: no NATS client — intake is idle; the reconcile alone still runs")
	}

	// LEGION-781, INFRA-555: the announcer publishes server.started once each time every live
	// server process comes to serve one build (startup.Announcer), and /healthz reports the latest
	// announcement's commit as fleet_commit. It runs once this process is registered, below
	// (joinRegistry, registry.go). A process without NATS announces into the database and leaves
	// the publish to a process whose bus answers. It announces only the commit it registers under,
	// with the relation kinds this build writes as tuples, the list graph_edges' tuple arm renders.
	var announcements startup.Publisher
	if natsClient != nil {
		announcements = natsClient
	}
	announcer := startup.New(startup.Deps{
		Pool: database.Pool, Publisher: announcements, Commit: buildCommit, TupleKinds: refs.TupleKinds,
	})

	routesHandler, background := routes.New(appCtx)
	// The keyword fill (textversions.Run) writes search_text_versions, the keyword table of each
	// past version of the searchable text, from the event log. It is a held duty, not a loop woken
	// per pass: one process holds its duty.Hold (search-text-versions-fill) for as long as it
	// serves and fills, its passes woken by every commit that wrote an event (its own wake on
	// api.EventsChannel, above), and every other process stands by for the hold, woken by a
	// hold's release and a 10 s retry. A lease taken per pass (duty.RunWoken) would move the fill
	// between processes from pass to pass, and the pause each batch asks first reads the answers
	// reads of the process that writes (answersLatency). Its goroutine starts here, once all it
	// runs with exists, but the fill takes nothing and writes nothing until serving closes, after
	// the server listens and the registry of live server processes admits this process
	// (joinRegistry, below). So it never runs inside a migration, and never in a process that the
	// registry refuses or is still waiting to admit: its pause reads the answers reads this process
	// serves, and a process the registry has not admitted serves none. Every way out from here
	// waits for the fill (waitFill) only once ctx is cancelled, or exits without waiting for it:
	// once ctx is cancelled, a fill still waiting for serving returns at once, having taken
	// nothing, and a serving fill commits or rolls back its batch in flight and releases its hold
	// through the pool, before the pool closes. DISPATCH_SEARCH_FILL=off starts no fill at all:
	// the process says so once, with whether each table the fill writes holds a row
	// (searchFillTableRows), and fillStopped is closed from the start, so nothing waits for it.
	serving := make(chan struct{})
	fillStopped := make(chan struct{})
	if boot.SearchFillOff {
		close(fillStopped)
		slog.Info(searchFillDeferredLine, append([]any{"setting", "DISPATCH_SEARCH_FILL", "value", "off"},
			searchFillTableRows(ctx, database.Pool)...)...)
	} else {
		go func() {
			defer close(fillStopped)
			if err := textversions.Run(ctx, textversions.Config{
				Leases:   textVersionsLeases,
				Horizon:  background.EventHorizon(),
				Latency:  answersLatency,
				Serving:  serving,
				Events:   textVersionsEventsWake,
				Released: textVersionsReleasedWake,
			}); err != nil {
				slog.Error("dispatch: start the keyword fill", "error", err)
			}
		}()
	}
	// waitFill waits for the keyword fill to stop; only a cancelled ctx stops it.
	waitFill := func() { <-fillStopped }
	// exitAfterIntakes ends a process that started its intakes and its keyword fill and will not
	// serve: it cancels ctx, waits for both to stop, which releases the intakes' holds, and exits
	// 1. The deferred stop (stopIntakesOnExit) does not run on an os.Exit, so every exit here goes
	// through this.
	exitAfterIntakes := func() {
		cancel()
		waitIntakes()
		waitFill()
		os.Exit(1)
	}
	// LEGION-550: resolves write_suggestions outcomes off the write path, from the events written
	// since its last pass, woken by every event commit whichever process made it. It reads the log
	// only up to the safe horizon of the API server's one events.Horizon, as the live event feed
	// does, since event ids are not commit order; it takes no lease: each of its pages holds the
	// suggestion_outcome_cursor row for update, which is what keeps two processes from judging one
	// page twice.
	go api.RunSuggestionOutcomeSweep(ctx, database.Pool, background.EventHorizon(), eventsWake)
	// The Idempotency-Keys past their 24 hours and the deliveries a process that died
	// left owed, once a minute across the processes under the suggestion-outcomes lease. It is a
	// timer and not a woken loop because no commit announces either, and it starts here because
	// it resumes those deliveries through the API server built just above.
	maintenanceInterval := api.MaintenanceSweepInterval
	if boot.MaintenanceSweepInterval > 0 {
		maintenanceInterval = boot.MaintenanceSweepInterval
	}
	go api.RunMaintenanceSweep(ctx, database.Pool, maintenanceInterval, maintenanceSweepLeases, background.ResumeDeliveries)
	// /healthz names each GitHub read this process runs, from the copy of the record kept above. A
	// loop this process did not start, for want of NATS or of the App's private key, is a read it
	// does not run.
	var reads *gitHubReads
	if appCfg != nil {
		reads = &gitHubReads{recordCopy: gitHubReadCopy, runs: map[githubreads.Read]bool{
			githubreads.WebhookRedelivery:  sweeper != nil,
			githubreads.ArchitectureImport: appCtx.Architecture().HasApp(),
			githubreads.DeliveryReconcile:  reconcile.HasApp(),
			githubreads.CIReconcile:        reconciler != nil,
		}}
	}
	handler := dispatchHandler(routesHandler, met, database, natsClient, outboxPublisher, fileStore, buildCommit, announcer, issuers{signIn: signIn, serviceTokens: serviceTokens}, reads)
	server := newHTTPServer(handler, time.Now)
	// The server serves on the socket it inherited (DISPATCH_LISTEN_FD) or binds its address, and
	// does either before serving, so a socket it cannot serve on or an address it cannot take ends
	// the process with 1: a supervisor must not read a port clash as a clean stop.
	var listener net.Listener
	if boot.ListenSocket != nil {
		listener, err = listenfd.Adopt(*boot.ListenSocket)
	} else {
		listener, err = net.Listen("tcp", boot.ListenAddr)
	}
	if err != nil {
		slog.Error("dispatch: listen", "addr", boot.ListenAddr, "error", err)
		exitAfterIntakes()
	}
	// The registry row carries the document schema numbers this build reads and creates,
	// which every process's document socket admission reads; the identity of its embedder, "" with
	// none, the same string the embedding queue compares stored vectors with, from which every
	// process's queue takes the one identity that embeds and drops (embedqueue, active.go); and
	// whether it stores files in a bucket, which keeps an upload's bytes in its row as well while
	// any live process has none, and refuses backfill-files meanwhile.
	embedderIdentity := ""
	if embedder != nil {
		embedderIdentity = embedder.Identity()
	}
	registration, err := joinRegistry(ctx, database, processes.Instance{
		Commit:           buildCommit,
		DashboardBuild:   dashboardBuild.Build,
		SigningKeys:      signingKey.Fingerprints(),
		SchemaVersion:    pmdoc.SchemaVersion(),
		SchemaCreatable:  pmdoc.SchemaCreatable(),
		EmbedderIdentity: embedderIdentity,
		FileStore:        fileStore != nil,
	}, registryPruneLeases, announcer)
	if err != nil {
		// A deploy that stops this process while the registration waits for a refusing peer's
		// heartbeat to lapse (processes.Register's wait, up to a live window and a heartbeat) stops
		// a process that has served nothing: a clean stop, not a failed start.
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			waitIntakes()
			waitFill()
			slog.Info("dispatch: stopped while waiting for admission to the registry of live server processes")
			return
		}
		slog.Error("dispatch: register this server process", "error", err)
		exitAfterIntakes()
	}
	// From here a reinstall that moves the schema refreshes this row before it closes the
	// document sockets; the refresh after it covers a reinstall that came after the registration
	// read the numbers and before the installer could reach the registration.
	registered.Store(registration)
	refreshRegistrySchema(ctx, registration)
	// The CI reconcile loop started before this, so it has been reading the holder and finding
	// nothing; from here each of its passes records its counts under this process's row.
	ciInstance.set(registration.ID)
	// The registry admitted this process, so it serves: the keyword fill starts (above).
	close(serving)
	// The importer's flights outlive the ticker's context, so they are closed inside the
	// background wait: shutdown runs it before waitUntilIdle and pool.Close, and a sync still
	// fetching then would write into a closing pool. The keyword fill is waited for here too, so
	// its hold is released before the pool closes.
	waitBackground := func() {
		background.Wait()
		appCtx.Architecture().Close()
		waitFill()
	}
	if err := serveUntilStopped(ctx, cancel, server, listener, documentService, database.Pool, waitBackground, waitIntakes,
		deregisterAfterDrain(registration), registration.Lost(), outboxStopped); err != nil {
		slog.Error("dispatch: serve", "error", err)
		os.Exit(1)
	}
}

func defaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "dispatch"), nil
}

// stopIntakesOnExit is main's deferred stop of the intakes: it cancels the process context, then
// waits for them (waitIntakes). The intakes' Runs return only once that context ends, and main's own
// deferred cancel, made before the intakes start, runs after this one; so a panic between the
// intakes' start and serveUntilStopped -- in routes.New, newHTTPServer or joinRegistry -- would
// otherwise wait here forever, with the panic unprinted and the intake still consuming. On every
// other way out the context has already ended, and the cancel does nothing.
func stopIntakesOnExit(cancel context.CancelFunc, waitIntakes func()) {
	cancel()
	waitIntakes()
}

// defaultWebDistDir resolves the SPA build directory: configured (DISPATCH_WEB_DIST) when it is
// not empty, otherwise relative to the running binary. The binary lives at packages/envoy/dispatch
// (when built locally) or is installed elsewhere; we walk up to find packages/dispatch/web/dist.
func defaultWebDistDir(configured string) (string, error) {
	// The configured directory is made absolute and clean like every path below, so a value
	// spelled through `..` names the same directory the static handler joins requests under.
	if configured != "" {
		return filepath.Abs(configured)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// Resolve symlinks so we get the real on-disk binary path.
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	// Walk up looking for packages/dispatch/web/dist.
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "packages", "dispatch", "web", "dist")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Fall back to a sibling layout: ../../dispatch/web/dist relative to binary.
	candidate := filepath.Join(filepath.Dir(exe), "..", "..", "dispatch", "web", "dist")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		abs, _ := filepath.Abs(candidate)
		return abs, nil
	}
	// Last resort: cwd-relative.
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, "packages", "dispatch", "web", "dist"), nil
}

// appCredentialSource is where loadAppCredentials found the App: the environment, or the data
// dir's app.json at Path. The dev sign-in fence decides on it; String is the form the boot log
// prints.
type appCredentialSource struct {
	Path string
}

func (s appCredentialSource) String() string {
	if s.Path != "" {
		return "file:" + s.Path
	}
	return "env"
}

// loadAppCredentials returns the App and where it came from. The environment (the DISPATCH_APP_*
// rows of the settings table) wins over the file; either may be absent, which returns a nil App.
func loadAppCredentials(env settingValues, dataDir string) (*auth.AppConfig, appCredentialSource, error) {
	if cfg, err := auth.LoadAppFromEnv(env.get); err != nil {
		return nil, appCredentialSource{}, fmt.Errorf("load app from env: %w", err)
	} else if cfg != nil {
		return cfg, appCredentialSource{}, nil
	}
	path := filepath.Join(dataDir, "app.json")
	cfg, err := auth.ReadApp(path)
	if err != nil {
		return nil, appCredentialSource{}, fmt.Errorf("read %s: %w", path, err)
	}
	if cfg == nil {
		return nil, appCredentialSource{}, nil
	}
	return cfg, appCredentialSource{Path: path}, nil
}

// dispatchStreamOptions are the options the Dispatch server's publisher connects with, the
// stream's other owner (bus.ConnectOwningStream): its name, the settings table's lookup for NATS's
// reach and nkey, the deployment's replica count and byte limit from boot, and the webhook
// redelivery bucket, the one bucket Dispatch opens at that count (webhookSweeper) and its NATS
// grant covers, which the client's keeper raises back to it when a concurrent start or an
// operator lowered it (bus.WithKeptBuckets). A server with no App key opens no such bucket, and
// the keeper skips one that does not exist. main and the tests that start Dispatch's stream both
// take them from here.
func dispatchStreamOptions(boot bootConfig, env settingValues) []bus.ConnectOption {
	return []bus.ConnectOption{
		bus.WithEnvironment(env.lookup),
		bus.WithName("dispatch"),
		bus.WithReplicas(boot.NATSReplicas),
		bus.WithStreamMaxBytes(boot.StreamMaxBytes),
		bus.WithKeptBuckets(bus.WebhookRedeliveryBucket),
	}
}

// resolveBootConfig is the server's configuration from what the settings table read (env),
// checked before anything connects. It holds the settings main hands on as values; three groups
// reach their readers from env instead, through the table's lookup: the GitHub App credentials
// (loadAppCredentials), the envoy.json overrides (loadEnvoyConfig) and NATS's reach and nkey (the
// bus connects). A new setting goes wherever its reader takes it, and always into the table.
// A variable a release removed (removedSettings) refuses startup before anything else is read.
func resolveBootConfig(env settingValues) (bootConfig, error) {
	if err := refuseRemovedSettings(env); err != nil {
		return bootConfig{}, err
	}
	boot := bootConfig{
		DatabaseURL:           strings.TrimSpace(env.get("DATABASE_URL")),
		ProjectOwnerFallback:  strings.TrimSpace(env.get("DISPATCH_PROJECT_OWNER_FALLBACK")),
		NewIssueLabel:         strings.TrimSpace(env.get("DISPATCH_NEW_ISSUE_LABEL")),
		NewIssueSession:       strings.TrimSpace(env.get("DISPATCH_NEW_ISSUE_SESSION")),
		GitHubAPIBase:         strings.TrimSpace(env.get("DISPATCH_GITHUB_API_BASE")),
		SignInIssuer:          strings.TrimSpace(env.get("DISPATCH_SIGNIN_ISSUER")),
		SignInClientID:        strings.TrimSpace(env.get("DISPATCH_SIGNIN_CLIENT_ID")),
		SignInClientSecret:    strings.TrimSpace(env.get("DISPATCH_SIGNIN_CLIENT_SECRET")),
		SignInGroup:           strings.TrimSpace(env.get("DISPATCH_SIGNIN_GROUP")),
		SignInDomain:          strings.TrimSpace(env.get("DISPATCH_SIGNIN_DOMAIN")),
		NATSDisabled:          env.get("DISPATCH_NATS_DISABLED") == "1",
		TestHooksEnabled:      env.get("DISPATCH_TEST_HOOKS") == "1",
		WebDist:               env.get("DISPATCH_WEB_DIST"),
		FileStoreBucket:       strings.TrimSpace(env.get("DISPATCH_FILE_STORE_BUCKET")),
		DevSignInSharedKey:    env.get("DISPATCH_DEV_SIGNIN_SHARED_KEY"),
		InsecureCookie:        env.get("DISPATCH_INSECURE_COOKIE") != "",
		EnvoyToken:            env.get("ENVOY_TOKEN"),
		EnvoyAccountTokenFile: strings.TrimSpace(env.get("ENVOY_ACCOUNT_TOKEN_FILE")),
		EnvoyGoogleAudience:   strings.TrimSpace(env.get("ENVOY_ACCOUNT_GOOGLE_AUDIENCE")),
	}
	if boot.DatabaseURL == "" {
		return bootConfig{}, errors.New("DATABASE_URL required")
	}
	if fallback := boot.ProjectOwnerFallback; fallback != "" {
		if err := principal.CheckPersonLogin(fallback); err != nil {
			return bootConfig{}, fmt.Errorf("DISPATCH_PROJECT_OWNER_FALLBACK must be a lowercase email, the one a full member signed in with; got a value of %d characters", len(fallback))
		}
	}
	if boot.EnvoyAccountTokenFile != "" && boot.EnvoyGoogleAudience != "" {
		return bootConfig{}, errors.New("ENVOY_ACCOUNT_TOKEN_FILE and ENVOY_ACCOUNT_GOOGLE_AUDIENCE each name Dispatch's identity at the Envoy listener; set one of them")
	}
	agentTokens := env.get("DISPATCH_AGENT_TOKEN")
	if strings.TrimSpace(agentTokens) == "" {
		return bootConfig{}, errors.New("DISPATCH_AGENT_TOKEN required")
	}
	tokens, err := auth.ParseSharedAgentTokens(agentTokens)
	if err != nil {
		return bootConfig{}, fmt.Errorf("DISPATCH_AGENT_TOKEN %w", err)
	}
	boot.AgentTokens = tokens
	if setting := env.get("DISPATCH_SIGNING_KEY"); strings.TrimSpace(setting) != "" {
		if boot.SigningKey, err = auth.ParseSigningKeys(setting); err != nil {
			return bootConfig{}, fmt.Errorf("DISPATCH_SIGNING_KEY %w", err)
		}
	}
	boot.ListenAddr, boot.ListenSocket, err = resolveListen(env)
	if err != nil {
		return bootConfig{}, err
	}
	pprofAddr, err := pprofAddress(env)
	if err != nil {
		return bootConfig{}, err
	}
	boot.PprofAddr = pprofAddr
	if boot.MaxLiveRooms, err = positiveSetting(env, "DISPATCH_MAX_LIVE_ROOMS"); err != nil {
		return bootConfig{}, err
	}
	if boot.MaxDocConnections, err = positiveSetting(env, "DISPATCH_MAX_DOC_CONNECTIONS"); err != nil {
		return bootConfig{}, err
	}
	if boot.NATSReplicas, err = bus.ParseNATSReplicas(env.get(bus.ReplicasEnvVar)); err != nil {
		return bootConfig{}, err
	}
	if boot.StreamMaxBytes, err = bus.ParseStreamMaxBytes(env.get(bus.StreamMaxBytesEnvVar)); err != nil {
		return bootConfig{}, err
	}
	if boot.RoleHolderWindows, err = roleHolderWindows(env); err != nil {
		return bootConfig{}, err
	}
	if boot.SearchFillOff, err = searchFillOff(env); err != nil {
		return bootConfig{}, err
	}
	for _, setting := range []struct {
		name   string
		target *time.Duration
	}{
		{"DISPATCH_TEST_SETTLE_DELAY", &boot.SettleDelay},
		{"DISPATCH_TEST_MARK_WAIT", &boot.MarkWait},
		{"DISPATCH_TEST_UNRECORDED_MARK_TTL", &boot.UnrecordedMarkTTL},
		{"DISPATCH_TEST_ROUTE_ROSTER_TTL", &boot.RouteRosterTTL},
		{"DISPATCH_TEST_LISTENER_TAKEOVER_WINDOW", &boot.ListenerTakeoverWindow},
		{"DISPATCH_TEST_HORIZON_STALL_AFTER", &boot.HorizonStallAfter},
		{"DISPATCH_TEST_HORIZON_WARN_EVERY", &boot.HorizonWarnEvery},
		{"DISPATCH_TEST_EDIT_NOTICE_QUIET", &boot.EditNoticeQuiet},
		{"DISPATCH_TEST_CI_RECONCILE_INTERVAL", &boot.CIReconcileInterval},
		{"DISPATCH_TEST_REDELIVERY_INTERVAL", &boot.RedeliveryInterval},
		{"DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL", &boot.DeliveryReconcileInterval},
		{"DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL", &boot.MaintenanceSweepInterval},
		{"DISPATCH_TEST_PROCESS_HEARTBEAT", &boot.ProcessHeartbeat},
		{"DISPATCH_TEST_KEY_REMAKE_INTERVAL", &boot.KeyRemakeInterval},
	} {
		if err := testHookDuration(env, setting.name, boot.TestHooksEnabled, setting.target); err != nil {
			return bootConfig{}, err
		}
	}
	if boot.ReviewerAppID, err = reviewerAppID(env); err != nil {
		return bootConfig{}, err
	}
	if boot.VerdictAppID, err = verdictAppID(env); err != nil {
		return bootConfig{}, err
	}
	for _, setting := range []struct {
		name   string
		target *bool
	}{
		{"DISPATCH_TEST_ROSTER_UNCACHED", &boot.ListenerRosterUncached},
		{"DISPATCH_TEST_EMBEDDER_OFF", &boot.EmbedderOff},
		{"DISPATCH_TEST_RECONCILE_AT_START", &boot.ReconcileAtStart},
		{"DISPATCH_TEST_KEY_REMAKE_OFF", &boot.KeyRemakeOff},
		{"DISPATCH_TEST_ARCHITECTURE_IMPORT_AT_START", &boot.ArchitectureImportAtStart},
	} {
		if err := testHookBool(env, setting.name, boot.TestHooksEnabled, setting.target); err != nil {
			return bootConfig{}, err
		}
	}
	switch rule := env.get("DISPATCH_PARENT_STATUS_RULE"); rule {
	case "", "off":
	case "on":
		boot.ParentStatusRule = true
	default:
		return bootConfig{}, fmt.Errorf("DISPATCH_PARENT_STATUS_RULE=%q (expected on, off or unset)", rule)
	}
	identityMode := strings.TrimSpace(env.get("DISPATCH_IDENTITY"))
	if err := checkSignInSettings(boot, identityMode); err != nil {
		return bootConfig{}, err
	}
	var devSignIn bool
	switch flag := env.get("DISPATCH_DEV_SIGNIN"); flag {
	case "":
	case "1":
		devSignIn = true
	default:
		return bootConfig{}, fmt.Errorf("DISPATCH_DEV_SIGNIN=%q (expected 1 or unset)", flag)
	}
	// The shared key only ever replaces dev sign-in's per-process key; a server without the flag
	// would ignore it, and a value an operator sets expecting it to sign their deployment's cookies
	// would sign nothing.
	if !devSignIn && boot.DevSignInSharedKey != "" {
		return bootConfig{}, errors.New("DISPATCH_DEV_SIGNIN_SHARED_KEY is read only with DISPATCH_DEV_SIGNIN=1, by the processes of one local harness; unset it, and set DISPATCH_SIGNING_KEY to give a deployment's processes one key")
	}

	iapAudience := strings.TrimSpace(env.get("DISPATCH_IAP_AUDIENCE"))
	switch mode := identityMode; {
	case mode == "" || mode == "cookie":
		// Cookie identity takes no sign-in setting: with none set, and no DISPATCH_DEV_SIGNIN,
		// Dispatch asks nobody to sign in, refuses every cookie an earlier sign-in minted (requestIdentityFor), and serves
		// its agent-token API alone.
	case mode == "iap":
		if iapAudience == "" {
			return bootConfig{}, errors.New("DISPATCH_IAP_AUDIENCE required: DISPATCH_IDENTITY=iap admits a request only on an assertion Identity-Aware Proxy signed for that audience, /projects/<project number>/global/backendServices/<backend service id> behind a load balancer")
		}
		if !iapAudienceShape.MatchString(iapAudience) {
			return bootConfig{}, fmt.Errorf("DISPATCH_IAP_AUDIENCE=%q must be the audience IAP signs its assertions for, which begins /projects/<project number>/, such as /projects/<project number>/global/backendServices/<backend service id>", iapAudience)
		}
		boot.IAPAudience, boot.IAPKeySetURL = iapAudience, oidc.IAPKeySetURL
	case strings.HasPrefix(mode, "header:"):
		boot.IdentityHeader = strings.TrimSpace(strings.TrimPrefix(mode, "header:"))
		if boot.IdentityHeader == "" {
			return bootConfig{}, errors.New("DISPATCH_IDENTITY header name required")
		}
		if env.get("DISPATCH_IDENTITY_HEADER_TRUSTED") != "1" {
			return bootConfig{}, errors.New("DISPATCH_IDENTITY_HEADER_TRUSTED=1 required: header identity is only for tests and local harnesses")
		}
	default:
		return bootConfig{}, fmt.Errorf("DISPATCH_IDENTITY=%q (expected cookie, iap or header:<Header-Name>)", mode)
	}
	if iapAudience != "" && boot.IAPAudience == "" {
		return bootConfig{}, errors.New("DISPATCH_IAP_AUDIENCE is read only with DISPATCH_IDENTITY=iap; unset it, or set DISPATCH_IDENTITY=iap")
	}
	envoyURL := strings.TrimSpace(env.get("ENVOY_URL"))
	if envoyURL == "" {
		envoyURL = defaultEnvoyURL
	}
	parsed, err := url.Parse(envoyURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return bootConfig{}, fmt.Errorf("ENVOY_URL=%q (expected an absolute http(s) URL)", envoyURL)
	}
	boot.EnvoyURL = strings.TrimSuffix(envoyURL, "/")

	// The pair's both-or-neither rule lives in internal/oidc so the listener
	// applies the same one; oidc.New stays out of this function, which reads
	// the environment and returns errors and nothing else.
	boot.OIDCIssuer, boot.OIDCAudience, err = oidc.ConfigFromEnv(env.get,
		"DISPATCH_OIDC_ISSUER", "DISPATCH_OIDC_AUDIENCE")
	if err != nil {
		return bootConfig{}, err
	}

	agentSecretsURL := strings.TrimSuffix(strings.TrimSpace(env.get("DISPATCH_AGENT_SECRETS_URL")), "/")
	if agentSecretsURL != "" {
		parsed, err := url.Parse(agentSecretsURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" {
			return bootConfig{}, fmt.Errorf("DISPATCH_AGENT_SECRETS_URL=%q (expected an absolute http(s) URL with no path)", redact.URL(agentSecretsURL))
		}
		boot.AgentSecretsURL = agentSecretsURL
		boot.AgentSecretsTokenFile = strings.TrimSpace(env.get("DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"))
		if boot.AgentSecretsTokenFile == "" {
			return bootConfig{}, errors.New("DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE is required when DISPATCH_AGENT_SECRETS_URL is set: Dispatch calls the broker with its own projected Kubernetes ServiceAccount token")
		}
	}
	// The value is strict, as DISPATCH_DEV_SIGNIN's is: a misspelt `on` must refuse the boot, not
	// leave a deployment that meant to refuse these writes taking them.
	switch value := env.get("DISPATCH_LEGION_STATUS_SCOPE"); value {
	case "", "off":
	case "on":
		boot.LegionStatusScope = true
	default:
		return bootConfig{}, fmt.Errorf("DISPATCH_LEGION_STATUS_SCOPE=%q (expected on, off or unset)", value)
	}
	boot.ProofOrigins, err = proofOriginEntries(env.get("DISPATCH_PROOF_ORIGINS"))
	if err != nil {
		return bootConfig{}, err
	}
	boot.Embedder, err = resolveEmbedder(env)
	if err != nil {
		return bootConfig{}, err
	}
	boot.Answerer, err = resolveAnswerer(env, boot.Embedder.Place)
	if err != nil {
		return bootConfig{}, err
	}
	// The dev embedder's vectors mean nothing beyond the words a text and a query share, so a
	// deployment that set it would answer meaning search with them and never know.
	if !devSignIn && boot.Embedder.Place == embed.PlaceDev {
		return bootConfig{}, errors.New("DISPATCH_DEV_EMBEDDER is read only with DISPATCH_DEV_SIGNIN=1, by the processes of one local harness; unset it, and set DISPATCH_EMBEDDER to embed a deployment's text at a model service")
	}

	if devSignIn {
		if err := devSignInFence(boot, env); err != nil {
			return bootConfig{}, err
		}
		boot.DevSignIn = true
	}
	return boot, nil
}

// devSignInFence is the dev sign-in fence over the environment; devSignInLoadedFence covers what
// main loads after it. With the flag any loopback client signs in as any person it names, so the
// process must listen, keep its data and reach the services that act on a human's word (NATS, the
// secrets broker, the Envoy listener that delivers mentions and messages, GitHub as the App) on
// this machine alone, and sign its cookies with a key no production server holds: one of its own,
// or one derived from DISPATCH_DEV_SIGNIN_SHARED_KEY under a dev-only label (sessionSigningKey),
// which the other processes of its harness derive too and a production server never signs with.
func devSignInFence(boot bootConfig, env settingValues) error {
	if boot.IdentityHeader != "" || boot.IAPAudience != "" {
		return errors.New("DISPATCH_DEV_SIGNIN=1 mints session cookies, so DISPATCH_IDENTITY must be cookie")
	}
	if boot.SignInIssuer != "" {
		return errors.New("DISPATCH_DEV_SIGNIN=1 signs people in without the sign-in issuer, so DISPATCH_SIGNIN_* must be unset: a loopback server holds no sign-in client secret")
	}
	if !routes.LoopbackHostPort(boot.ListenAddr) {
		if boot.ListenSocket != nil {
			return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 is for a loopback server only: %s, must be on 127.0.0.1 or [::1]", boot.ListenSocket)
		}
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 is for a loopback server only: DISPATCH_LISTEN_HOST=%q (listen address %q) must be 127.0.0.1 or [::1]", env.get("DISPATCH_LISTEN_HOST"), boot.ListenAddr)
	}
	if err := loopbackDatabase(boot.DatabaseURL); err != nil {
		return err
	}
	if !boot.SigningKey.Empty() {
		return errors.New("DISPATCH_DEV_SIGNIN=1 signs cookies with a key generated for this process, or derived from DISPATCH_DEV_SIGNIN_SHARED_KEY when the processes of one local harness share their cookies; unset DISPATCH_SIGNING_KEY")
	}
	if !boot.NATSDisabled && env.get(bus.AllowRemoteEnvVar) == "1" {
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 publishes to this machine's NATS only: unset %s, or set DISPATCH_NATS_DISABLED=1", bus.AllowRemoteEnvVar)
	}
	if boot.AgentSecretsURL != "" {
		if !routes.LoopbackURL(boot.AgentSecretsURL) {
			return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 decides credential requests on this machine's secrets broker only: DISPATCH_AGENT_SECRETS_URL=%q must name 127.0.0.1, [::1] or localhost", redact.URL(boot.AgentSecretsURL))
		}
	}
	if !routes.LoopbackURL(boot.EnvoyURL) {
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 delivers through this machine's Envoy listener only: ENVOY_URL=%q must name 127.0.0.1, [::1] or localhost", boot.EnvoyURL)
	}
	return nil
}

// devSignInLoadedFence is the dev sign-in fence over what main loads after the environment: the
// dashboard origin from envoy.json, and the GitHub App from the environment or the data dir's
// app.json. With the flag a loopback client acts as any person it names, and a person can make the
// App act: saving an architecture source has the App probe and import the repository the caller
// names. The private key is the credential that acts (githubapp.New builds no client without it,
// the App JWT names the client ID, and nothing sends the numeric App ID), so:
//   - a key from app.json, where a developer keeps the real App's key, is refused whatever the
//     base;
//   - a key from the environment may sign calls only to a loopback host. That checks the host, not
//     what listens there: every App call hands a signed App JWT to whatever owns the port, so the
//     key must be a throwaway, as the one packages/dispatch/e2e/run-server.sh generates.
func devSignInLoadedFence(boot bootConfig, serverURL string, app *auth.AppConfig, source appCredentialSource) error {
	if !boot.DevSignIn {
		return nil
	}
	if _, err := routes.DevSignInOrigin(serverURL); err != nil {
		return err
	}
	if app == nil || app.PEM == "" {
		return nil
	}
	if source.Path != "" {
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 refuses the App private key in %s, whatever DISPATCH_GITHUB_API_BASE names: that file is where the real App's key is kept, and with the flag any loopback client can have the App sign calls. Pass a throwaway App in the environment instead (DISPATCH_APP_CLIENT_ID and a generated DISPATCH_APP_PEM_B64, which take precedence over app.json), with DISPATCH_GITHUB_API_BASE naming a GitHub fake on a loopback host, as packages/dispatch/e2e/run-server.sh does", source.Path)
	}
	if !routes.LoopbackURL(boot.GitHubAPIBase) {
		return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 lets the App private key (DISPATCH_APP_PEM_B64) sign calls only to a loopback host: DISPATCH_GITHUB_API_BASE=%q (empty is https://api.github.com) must name 127.0.0.1, [::1] or localhost. Use a throwaway key: every App call hands a signed App JWT to whatever listens there", boot.GitHubAPIBase)
	}
	return nil
}

// loopbackDatabase refuses a DATABASE_URL with a host that is not this machine: the data a
// dev-sign-in server serves to any local process must be a scratch database here. pgx parses
// both the URL and the key=value forms, multi-host lists included, exactly as store.Open will.
func loopbackDatabase(databaseURL string) error {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return fmt.Errorf("DATABASE_URL: %w", err)
	}
	hosts := []string{config.Host}
	for _, fallback := range config.Fallbacks {
		hosts = append(hosts, fallback.Host)
	}
	for _, host := range hosts {
		if !strings.HasPrefix(host, "/") && !routes.LoopbackName(host) {
			return fmt.Errorf("DISPATCH_DEV_SIGNIN=1 serves a loopback database only: DATABASE_URL names host %q", host)
		}
	}
	return nil
}

// explainMigrateError wraps a migration failure Postgres attributes to a missing CREATE
// permission on the vector extension (0072_embeddings_core.up.sql's `create extension if not
// exists vector`) with what that actually means operationally: the deployment's own bootstrap
// (the cluster's master role, which this application's migration role is not) must create the
// extension before this Dispatch ever boots against this database - a one-time ops dependency,
// not a bug in the migration retried. Every other migration failure passes through unchanged.
// Classified the same way the rest of this codebase classifies a specific Postgres failure
// (api/issue_create.go's isUniqueViolation, internal/pgmigrate/lock.go and census.go's own
// errors.As(..., &pgErr) checks): by the stable SQLSTATE code (42501, insufficient_privilege),
// not by matching the fully wrapped error's text, which a locale or Postgres version could
// change; pgErr.Message is Postgres's own unwrapped message, so the extension-name check still
// narrows to this specific failure rather than every insufficient-privilege error any migration
// could ever raise.
func explainMigrateError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	const insufficientPrivilege = "42501"
	if !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege || !strings.Contains(pgErr.Message, `extension "vector"`) {
		return err
	}
	return fmt.Errorf(
		"%w (this database's own migration role cannot CREATE EXTENSION; the deployment's bootstrap, "+
			"run as the cluster's master role, must install the vector extension before Dispatch boots "+
			"against this database for the first time - not something retrying the migration fixes)",
		err,
	)
}

// sessionSigningKey is the key set session cookies are signed with. Under dev sign-in it holds one
// key: the key derived from DISPATCH_DEV_SIGNIN_SHARED_KEY when it is set
// (devSignInSharedSigningKey), otherwise one generated for this process. Outside it, it is
// DISPATCH_SIGNING_KEY's keys when that is set (a deployment's, from its secrets manager),
// otherwise the data dir's signing-key file, created on first start (a local run's). Outside dev
// sign-in the keys must stay the same across deploys, or every dsession cookie is invalidated
// whenever a container rolls; a rotation adds the new key second first. An empty signing-key file
// is refused by name: the empty set signs nothing, and the people store would refuse it only by
// panicking.
func sessionSigningKey(boot bootConfig, dataDir string) (auth.SigningKeys, error) {
	if boot.DevSignIn {
		if boot.DevSignInSharedKey != "" {
			return auth.SingleSigningKey(devSignInSharedSigningKey(boot.DevSignInSharedKey)), nil
		}
		key, err := auth.NewSigningKey()
		if err != nil {
			return auth.SigningKeys{}, err
		}
		return auth.SingleSigningKey(key), nil
	}
	if !boot.SigningKey.Empty() {
		return boot.SigningKey, nil
	}
	path := filepath.Join(dataDir, "signing-key")
	keys, err := auth.LoadOrCreateSigningKey(path)
	if err != nil {
		return auth.SigningKeys{}, err
	}
	if keys.Empty() {
		return auth.SigningKeys{}, fmt.Errorf("the session signing key file %s is empty: put its key back, delete it to have a new key written (which signs out everyone signed in under the old one), or set DISPATCH_SIGNING_KEY", path)
	}
	return keys, nil
}

// devSignInSharedSigningKeyLabel separates the cookie key dev sign-in derives from every key a
// production server signs with.
const devSignInSharedSigningKeyLabel = "dispatch dev sign-in"

// devSignInSharedSigningKey is the cookie key every dev-sign-in process given shared derives, the
// base64url of HMAC-SHA256 keyed by shared over a fixed dev-only label. It is never shared itself,
// so a production DISPATCH_SIGNING_KEY pasted into DISPATCH_DEV_SIGNIN_SHARED_KEY signs no cookie,
// and seals no refresh token, that the production server it came from accepts.
func devSignInSharedSigningKey(shared string) string {
	mac := hmac.New(sha256.New, []byte(shared))
	mac.Write([]byte(devSignInSharedSigningKeyLabel))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// newDocumentService is the document service main serves the document websocket and the API's
// documents from: built, what main made from the rest of the configuration, with every setting the
// service takes from boot filled in (documentDeps).
func newDocumentService(boot bootConfig, built docs.Deps) *docs.Service {
	return docs.New(documentDeps(boot, built))
}

// documentDeps is built with every setting the document service takes from boot filled in. The
// settings tests read its answer, as they build the router through appContextOptions, so dropping
// a hand-off here fails a case (TestDocumentDepsCarryEveryBootSetting).
func documentDeps(boot bootConfig, built docs.Deps) docs.Deps {
	built.Settle = boot.SettleDelay
	built.MarkWait = boot.MarkWait
	built.UnrecordedMarkTTL = boot.UnrecordedMarkTTL
	built.EditNoticeQuiet = boot.EditNoticeQuiet
	built.MaxLiveRooms = boot.MaxLiveRooms
	built.MaxDocConnections = boot.MaxDocConnections
	built.TestHooks = boot.TestHooksEnabled
	return built
}

// dashboardPublishTimeout bounds the boot's publish of its dashboard build and the prune after it.
// A first publish writes the whole build, 45 files and 5.4 MB for the dashboard of 2026-10-27; a
// restart on a stored build writes nothing.
const dashboardPublishTimeout = 60 * time.Second

// publishDashboard stores the dashboard build at webDistDir in the database
// (dashboard.Publish, stamped with buildCommit) and then prunes the builds no process needs any
// more (dashboard.Prune), both within timeout, and returns what it published: the build's id
// (routes.DashboardBuild's) and entry chunk among it. A dist with no index.html (the Go test
// rigs serve an empty directory) publishes nothing, logs that it did not, and returns the zero
// Published, whose empty build and entry say this process serves no dashboard. A failed publish
// is the error returned, and refuses the boot; a failed prune is logged and the start goes on,
// since it leaves only builds the next boot's prune deletes.
func publishDashboard(ctx context.Context, pool *store.Pool, webDistDir string, timeout time.Duration) (dashboard.Published, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	published, err := dashboard.Publish(ctx, pool, webDistDir, buildCommit)
	switch {
	case errors.Is(err, dashboard.ErrNoBuild):
		slog.Info("dispatch: no dashboard build to publish: the web dist holds no index.html", "dir", webDistDir)
	case err != nil:
		return dashboard.Published{}, err
	default:
		slog.Info("dispatch: published dashboard assets", "build", published.Build, "entry", published.Entry,
			"assets", published.Assets, "written", published.Written)
	}
	// The prune keeps every build a live server process serves (processes.Live, read here at boot)
	// beside the newest dashboard.KeepNewest. This process registers only once it listens, after
	// this, so it is not among them yet; the publish above has just renewed its build's
	// last_started_at, which puts it among the newest.
	liveBuilds, err := api.LiveDashboardBuilds(ctx, pool)
	if err != nil {
		slog.Error("dispatch: prune dashboard builds", "error", err)
		return published, nil
	}
	pruned, err := dashboard.Prune(ctx, pool, liveBuilds)
	if err != nil {
		slog.Error("dispatch: prune dashboard builds", "error", err)
		return published, nil
	}
	slog.Info("dispatch: pruned dashboard builds", "builds", pruned.Builds, "assets", pruned.Assets)
	return published, nil
}

// decidedWordingRestoreTimeout bounds the boot's restoration of decided asks' wordings. On the
// 2026-10-11 production dump (3,739 asks, 82,153 events) the restoration and its marking of the
// documents it leaves owed took 0.17-0.77 s.
const decidedWordingRestoreTimeout = 30 * time.Second

// repoRenamesReadTimeout bounds the boot's read of the rename pairs (store.ReadRepoRenames).
// The read is of a table of tens of rows, so the bound is reached only when another session
// holds a lock on that table, and a boot that waited for it would not serve.
const repoRenamesReadTimeout = 30 * time.Second

// readRenames is the boot's one read of the rename pairs (store.ReadRepoRenames), bounded by
// timeout, as the Renames every reader is given (refs.NewRenames). A read that fails is logged at
// ERROR, and answers no pairs and the error, which the rename pass counts (renameRepositories).
// A pair whose new name is no repository GitHub can hold is dropped, not applied: one ERROR line
// names it and why, the pass is given the pairs the Renames holds (refs.Renames.Pairs) and so
// rewrites no field for it, no reader calls its old name by its new one, and so no read is owed
// under a name no read can reach. The other pairs apply. Such a pair is written only by SQL by
// hand, since no route, event or migration writes one; it stays dropped, with its line at every
// start, until its renamed_to in repo_projects is corrected. This refuses the one pair rather
// than the start, so a bad row costs its rename alone and no process stops serving for it.
func readRenames(ctx context.Context, pool *store.Pool, timeout time.Duration) (refs.Renames, error) {
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	pairs, err := store.ReadRepoRenames(readCtx, pool)
	cancel()
	if err != nil {
		slog.Error("dispatch: read the renamed repositories' pairs; serving with none", "error", err)
	}
	renames := refs.NewRenames(pairs)
	for _, dropped := range renames.Dropped() {
		// The new name is cut at 140 characters, the longest name GitHub can hold, since a pair
		// written by hand can hold any text.
		slog.Error("dispatch: dropped a rename pair whose new name GitHub cannot hold; its old name reads as its own until the pair is corrected",
			"from", dropped.From, "to", fmt.Sprintf("%.140s", dropped.To), "error", dropped.Err)
	}
	return renames, err
}

// restoreDecidedWordings is the boot's restoration of the wording each decided ask was decided on,
// where a task of an earlier release reworded it, and its marking owed of each such block ask's own
// document and copies (docs.Service.RestoreDecidedWordings), bounded by timeout. Each ask it
// restored has its own line; this logs their count and the copies' count. A restoration that fails
// or runs out of time is logged and the start goes on, since each document's next settlement
// restores its own asks.
func restoreDecidedWordings(ctx context.Context, documents *docs.Service, timeout time.Duration) {
	bootTask(ctx, timeout, "dispatch: restore decided asks' wordings", "dispatch: restored decided asks' wordings",
		func(ctx context.Context) ([]any, error) {
			restored, copies, err := documents.RestoreDecidedWordings(ctx)
			if err != nil {
				return nil, err
			}
			return []any{"restored", restored, "copies", copies}, nil
		})
}

const reviewContentHashFillTimeout = 30 * time.Second

// fillReviewContentHashes pins reviews from before the hash columns were added.
func fillReviewContentHashes(ctx context.Context, pool *store.Pool, timeout time.Duration) {
	bootTask(ctx, timeout, "dispatch: fill reviews' content hashes", "dispatch: filled reviews' content hashes",
		func(ctx context.Context) ([]any, error) {
			result, err := docs.FillReviewContentHashes(ctx, pool)
			return []any{"filled", result.Filled, "unhashable", result.Unhashable}, err
		})
}

// repoRenamePassTimeout bounds the boot's repository rename pass: one short transaction for each
// line of fields (store.repoRenameLines). A pass that reaches the bound fails the transaction in
// flight and the lines after it, and the next boot writes what is left. The pass's count of the
// rows left has a bound of its own after this one, so the summary says what the bound left.
const repoRenamePassTimeout = 30 * time.Second

// renameRepositories is the boot's pass that writes, in every stored repository field, the new
// name of each rename pair in place of its old name (store.RewriteRenamedRepositories), bounded by
// timeout. pairs and readErr are what the boot's read of the pairs answered. A read that failed is
// one failure of the pass, which then has no pairs and writes nothing, and the rows left on an old
// name are unknown, since the pairs are. The pass logs its own lines; this logs its summary at
// every boot, at INFO when nothing failed and no old name is known to be left, and at ERROR
// otherwise, with left=unknown where nothing counted it. Either way the start goes on, and the
// next boot tries again.
func renameRepositories(ctx context.Context, pool *store.Pool, pairs []store.RepoRename, readErr error, timeout time.Duration) {
	const summary = "dispatch: rewrote renamed repositories' old names"
	bootTask(ctx, timeout, summary, summary, func(ctx context.Context) ([]any, error) {
		pass := store.RewriteRenamedRepositories(ctx, pool, pairs)
		if readErr != nil {
			pass.Failures++
			pass.Left = store.RepoRenamesLeft{}
		}
		return pass.LogArgs(), pass.Err()
	})
}

// issueAskRepairBootTimeout bounds the boot's repair of the asks of closed and reopened issues.
// Each issue commits on its own, so a repair cut short keeps what it repaired, and the interval
// pass (api.RunIssueAskRepair) repairs what it left.
const issueAskRepairBootTimeout = 30 * time.Second

// repairIssueAsks is the boot's repair of the asks a close or a reopen by a release older than
// the withdrawal left (api.RepairIssueAsks), bounded by timeout: open asks on closed issues are
// withdrawn, and withdrawn asks on open issues are restored. It sweeps only the closed issues'
// documents marked owing a settlement within the last api.IssueAskRepairInterval, so a boot does
// not read every closed issue's documents before it serves; the interval pass's first run sweeps
// them all. Each issue it repairs has its own line; this logs the counts. A repair that fails or
// runs out of time is logged and the start goes on, since api.RunIssueAskRepair repairs the rest
// every api.IssueAskRepairInterval.
func repairIssueAsks(ctx context.Context, deps api.Deps, timeout time.Duration) {
	bootTask(ctx, timeout, "dispatch: repair the asks of closed and reopened issues", "dispatch: repaired the asks of closed and reopened issues",
		func(ctx context.Context) ([]any, error) {
			markedSince := time.Now().Add(-api.IssueAskRepairInterval)
			repair, err := api.RepairIssueAsks(ctx, deps, &markedSince)
			return repair.LogArgs(), err
		})
}

// bootTask runs task, a repair the boot makes before it serves, bounded by timeout, and logs what
// it reports: the counts task returns under done at INFO, or, when task fails or runs out of time,
// those counts and the error under failed at ERROR. Either way the start goes on.
func bootTask(ctx context.Context, timeout time.Duration, failed, done string, task func(context.Context) ([]any, error)) {
	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	counts, err := task(taskCtx)
	if err != nil {
		slog.Error(failed, append(counts, "error", err)...)
		return
	}
	slog.Info(done, counts...)
}

// appContextOptions is what main hands routes.BuildAppContext: built, what main made from the
// configuration, with every setting the router takes from boot filled in. The settings tests build
// the router through it, so dropping any hand-off here fails a case.
func appContextOptions(boot bootConfig, built routes.AppContextOptions) routes.AppContextOptions {
	built.SignInRule = signInRule(boot)
	built.AgentTokens = boot.AgentTokens
	built.NewIssueLabel = boot.NewIssueLabel
	built.NewIssueSession = boot.NewIssueSession
	built.InsecureCookie = boot.InsecureCookie
	built.EnvoyURL = boot.EnvoyURL
	built.EnvoyToken = boot.EnvoyToken
	built.EnvoyAccountTokenFile = boot.EnvoyAccountTokenFile
	built.EnvoyGoogleAudience = boot.EnvoyGoogleAudience
	built.GitHubAPIBase = boot.GitHubAPIBase
	built.AgentSecretsURL = boot.AgentSecretsURL
	built.AgentSecretsTokenFile = boot.AgentSecretsTokenFile
	built.LegionStatusScope = boot.LegionStatusScope
	built.TestHooksEnabled = boot.TestHooksEnabled
	built.RouteRosterTTL = boot.RouteRosterTTL
	built.ListenerTakeoverWindow = boot.ListenerTakeoverWindow
	built.HorizonStallAfter = boot.HorizonStallAfter
	built.HorizonWarnEvery = boot.HorizonWarnEvery
	built.ListenerRosterUncached = boot.ListenerRosterUncached
	built.OutboxPublishes = !boot.NATSDisabled
	built.DevSignIn = boot.DevSignIn
	return built
}

// loadEnvoyConfig is envoy.json as Dispatch reads it: the files config.Load finds, the user file
// under the XDG_CONFIG_HOME row, overridden by the DISPATCH_SERVER_URL and NATS_URLS rows of the
// settings table. Each file config.Load found and did not read is logged at WARN once at start.
func loadEnvoyConfig(env settingValues, options config.LoadOptions) (*config.EnvoyConfig, error) {
	cfg, err := config.Load(env.lookup, options)
	if err != nil {
		return nil, err
	}
	for _, warning := range cfg.Warnings {
		slog.Warn(warning.Message, "path", warning.Path, "fix", warning.Fix)
	}
	return cfg, nil
}

// parsePort reads the port a setting names: decimal digits alone, 1-65535. The error names the
// setting, so a refusal says which variable to fix.
func parsePort(name, raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || strings.TrimLeft(raw, "0123456789") != "" || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid %s %q: want a port number 1-65535", name, raw)
	}
	return port, nil
}

// positiveSetting reads the positive whole number the setting name holds: decimal digits alone,
// at least 1, with whitespace at either end ignored; unset or empty, it is 0. The error names the
// setting and the value, so a refusal says which variable to fix.
func positiveSetting(env settingValues, name string) (int, error) {
	raw := strings.TrimSpace(env.get(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || strings.TrimLeft(raw, "0123456789") != "" || value < 1 {
		return 0, fmt.Errorf("%s=%q (expected a positive integer)", name, raw)
	}
	return value, nil
}

// searchFillDeferredLine is the line a process logs once at boot when DISPATCH_SEARCH_FILL=off
// keeps it from starting the keyword fill.
const searchFillDeferredLine = "dispatch: the keyword fill is deferred; this process fills no keyword table"

// searchFillTables are the tables the keyword fill writes: search_text_versions, the keyword
// table, and search_text_versions_fill, its cursor. searchFillTableHoldsRow writes their names into
// SQL, so they are fixed here and never read from input.
var searchFillTables = [...]string{"search_text_versions", "search_text_versions_fill"}

// The three bounds of each read of searchFillTableRows. Each read sets lock_timeout and
// statement_timeout in a transaction of its own, so Postgres itself ends a read that waits for a
// lock on its table (55P03, after searchFillTableLockTimeout) or that runs on for any reason
// (57014, after searchFillTableStatementTimeout), and no read is left running at the server once
// the boot moves on. searchFillTableReadTimeout is the client's own deadline for each table, for a
// server that answers nothing: pgx then closes the connection and asks the server to cancel the
// statement. It also covers taking a connection from the pool, a dial or a wait for a free one
// included, so it sits 4 s past the statement timeout: a slow dial still leaves the server's own
// errors to end the read. The two reads run one after the other before the server listens, so
// they delay the boot by at most twice searchFillTableReadTimeout in all; a lock on a table alone
// delays it by about searchFillTableLockTimeout for that table.
const (
	searchFillTableLockTimeout      = 5 * time.Second
	searchFillTableStatementTimeout = 6 * time.Second
	searchFillTableReadTimeout      = 10 * time.Second
)

// searchFillTableRows answers the deferral line's fields that say whether each of
// searchFillTables holds a row: <table>_rows=none or some, read by searchFillTableHoldsRow. A read
// that fails, a timeout included, gives <table>_rows=unknown and the error in <table>_rows_error,
// and the boot goes on: the line reports the tables and never decides whether the process starts.
// The line is how a deployment learns, from the boot log alone, that a process booted with the
// fill off found both tables empty.
func searchFillTableRows(ctx context.Context, pool *store.Pool) []any {
	fields := make([]any, 0, 4*len(searchFillTables))
	for _, table := range searchFillTables {
		key := table + "_rows"
		holdsRow, err := searchFillTableHoldsRow(ctx, pool, table)
		switch {
		case err != nil:
			fields = append(fields, key, "unknown", key+"_error", err.Error())
		case holdsRow:
			fields = append(fields, key, "some")
		default:
			fields = append(fields, key, "none")
		}
	}
	return fields
}

// searchFillTableHoldsRow reads whether table holds a row by
// `select exists(select 1 from public.<table>)`, which reads no column and stops at the first
// visible row; it may scan dead rows before that one, which searchFillTableStatementTimeout bounds.
// It reads in a read-only transaction of its own that first sets the bounds above, local to that
// transaction. The read takes ACCESS SHARE on the table and holds it until that transaction rolls
// back (the deferred Rollback) or its connection ends, normally milliseconds after the read; no
// statement timeout bounds how long it holds it. Row writes go on beside it, but a TRUNCATE or
// another statement that wants ACCESS EXCLUSIVE waits behind it for that long.
func searchFillTableHoldsRow(ctx context.Context, pool *store.Pool, table string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, searchFillTableReadTimeout)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)`,
		fmt.Sprintf("%dms", searchFillTableLockTimeout.Milliseconds()),
		fmt.Sprintf("%dms", searchFillTableStatementTimeout.Milliseconds())); err != nil {
		return false, err
	}
	var holdsRow bool
	err = tx.QueryRow(ctx, "select exists(select 1 from public."+table+")").Scan(&holdsRow)
	return holdsRow, err
}

// searchFillOff reads DISPATCH_SEARCH_FILL: on, or unset, starts the keyword fill, and off defers
// it. Any other value refuses the boot, naming the variable.
func searchFillOff(env settingValues) (bool, error) {
	switch value := env.get("DISPATCH_SEARCH_FILL"); value {
	case "", "on":
		return false, nil
	case "off":
		return true, nil
	default:
		return false, fmt.Errorf("DISPATCH_SEARCH_FILL=%q (expected on or off)", value)
	}
}

// openFileStore is the uploaded-file store boot names (files.Open: gs://<bucket> is a Cloud
// Storage bucket, a bare name an S3 bucket), or nil when DISPATCH_FILE_STORE_BUCKET is unset,
// which keeps every file in Postgres. A nil interface, never a typed nil, so every caller's nil
// check reads the setting.
func openFileStore(ctx context.Context, boot bootConfig) (files.Store, error) {
	if boot.FileStoreBucket == "" {
		return nil, nil
	}
	return files.Open(ctx, boot.FileStoreBucket)
}

// serverBus is the options every NATS connection of the server is made with: it connects in the
// background (bus.WithConnectInBackground), so a bus that is down at boot or refuses the credential
// leaves the process serving and connects it once the bus answers, and it declares a connection
// whose packets stop coming back lost within seconds (bus.WithSilentCutDetection), so /healthz
// reports a cut bus within seconds of the cut.
func serverBus() []bus.DialOption {
	return []bus.DialOption{bus.WithConnectInBackground(), bus.WithSilentCutDetection()}
}

// serverClient is the options a NATS client of the server connects with: extra, then serverBus's.
func serverClient(extra ...bus.ConnectOption) []bus.ConnectOption {
	for _, option := range serverBus() {
		extra = append(extra, option)
	}
	return extra
}

// dispatchHandler mounts the three probes the process serves - /healthz, /livez and /metrics -
// above every dashboard and API route, so each is answered whatever the router is doing. Go's
// ServeMux prefers the longer pattern, so "GET /healthz", "GET /livez" and "GET /metrics" win
// over the router's "/", which is also what keeps a scrape of /metrics from falling through to
// the SPA shell the router answers an unknown path with. /livez answers 204 and touches nothing:
// it says only that the process answers, for a liveness or startup probe, which must not wait on
// or fail with Postgres, NATS or the file store. /metrics is unauthenticated like the other two,
// so whoever reaches Dispatch's HTTP port can read it, and registry serves it.
func dispatchHandler(handler http.Handler, registry *metrics.Registry, database *store.Store, natsClient *bus.Client, publisher *outbox.Outbox, fileStore files.Store, commit string, announcer *startup.Announcer, issuers issuers, reads *gitHubReads) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthzHandler(database, natsClient, publisher, fileStore, commit, announcer, issuers, reads))
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.Handle("GET /metrics", registry.Handler())
	mux.Handle("/", handler)
	return mux
}

// issuers are the two OIDC issuers the process discovers in the background, as /healthz reports
// them: the sign-in issuer's code flow and the service-account token verifier, each nil when boot
// configures none.
type issuers struct {
	signIn        *oidc.CodeFlow
	serviceTokens *oidc.LazyVerifier
}

// gitHubReads is what /healthz names the GitHub reads from (githubreads): which of the four this
// process runs, and its copy of the record of each read's newest attempt. main builds it only
// where a GitHub App is configured, and nil names none (`github: null`).
type gitHubReads struct {
	// runs holds true for each read whose loop this process runs.
	runs map[githubreads.Read]bool
	// recordCopy is the copy githubreads.Copy.Run keeps, which a write by any process refreshes.
	recordCopy *githubreads.Copy
}

// gitHubReadState is the state /healthz names for one GitHub read.
type gitHubReadState string

const (
	// gitHubReadSucceeded: the newest recorded attempt sent requests to GitHub and noted no
	// failure.
	gitHubReadSucceeded gitHubReadState = "succeeded"
	// gitHubReadFailed: the newest recorded attempt noted a failure.
	gitHubReadFailed gitHubReadState = "failed"
	// gitHubReadNoAttempt: the record holds no attempt of the read yet.
	gitHubReadNoAttempt gitHubReadState = "no_attempt"
	// gitHubReadUnknown: this process has not read the record yet.
	gitHubReadUnknown gitHubReadState = "unknown"
	// gitHubReadOff: this process does not run the read.
	gitHubReadOff gitHubReadState = "off"
)

// gitHubReadEntry is one read's entry in /healthz's github member. gitHubReadEntryOf alone makes
// one: it sets At only where the state is an attempt the record holds, and Cause only where that
// attempt failed, so no entry says failed without its cause, or names a time or a cause its state
// does not carry.
type gitHubReadEntry struct {
	State gitHubReadState `json:"state"`
	// At is when the newest attempt was recorded, by the database's clock, in UTC.
	At *time.Time `json:"at"`
	// Cause is the newest attempt's first failure, at most githubreads.CauseLimit bytes.
	Cause *string `json:"cause"`
}

// gitHubReadEntryOf is read's entry: off when this process does not run it (runs false), unknown
// before the copy's first load (loaded false), no_attempt when the copy holds no row for it, and
// otherwise the row's own outcome, with its time, and with its cause when it failed.
func gitHubReadEntryOf(read githubreads.Read, runs, loaded bool, rows map[githubreads.Read]githubreads.Recorded) gitHubReadEntry {
	if !runs {
		return gitHubReadEntry{State: gitHubReadOff}
	}
	if !loaded {
		return gitHubReadEntry{State: gitHubReadUnknown}
	}
	row, recorded := rows[read]
	if !recorded {
		return gitHubReadEntry{State: gitHubReadNoAttempt}
	}
	at := row.At.UTC()
	if row.Succeeded {
		return gitHubReadEntry{State: gitHubReadSucceeded, At: &at}
	}
	cause := row.Cause
	return gitHubReadEntry{State: gitHubReadFailed, At: &at, Cause: &cause}
}

// gitHubReadsMember is /healthz's github member: one entry for each read, in the order
// githubreads.Reads names them. It is a struct and not a map so that the keys keep that order.
type gitHubReadsMember struct {
	WebhookRedelivery  gitHubReadEntry `json:"webhook_redelivery"`
	ArchitectureImport gitHubReadEntry `json:"architecture_import"`
	DeliveryReconcile  gitHubReadEntry `json:"delivery_reconcile"`
	CIReconcile        gitHubReadEntry `json:"ci_reconcile"`
}

// member is the github member of one answer, nil where no App is configured. It reads the copy
// once, so the four entries come from one load, and reads nothing else.
func (g *gitHubReads) member() *gitHubReadsMember {
	if g == nil {
		return nil
	}
	rows, loaded := g.recordCopy.Rows()
	entry := func(read githubreads.Read) gitHubReadEntry {
		return gitHubReadEntryOf(read, g.runs[read], loaded, rows)
	}
	return &gitHubReadsMember{
		WebhookRedelivery:  entry(githubreads.WebhookRedelivery),
		ArchitectureImport: entry(githubreads.ArchitectureImport),
		DeliveryReconcile:  entry(githubreads.DeliveryReconcile),
		CIReconcile:        entry(githubreads.CIReconcile),
	}
}

// serviceTokenVerifier is the verifier of the projected service-account tokens boot's
// DISPATCH_OIDC_ISSUER issues to DISPATCH_OIDC_AUDIENCE, nil when boot configures none. It reads
// nothing: the issuer is discovered in the background (oidc.LazyVerifier.Run, which main starts at
// boot) or by the first token, and until then a JWT-shaped bearer is answered 503.
func serviceTokenVerifier(boot bootConfig) *oidc.LazyVerifier {
	if boot.OIDCIssuer == "" {
		return nil
	}
	return oidc.NewLazyVerifier(boot.OIDCIssuer, boot.OIDCAudience)
}

// brokerTokenVerifier decides callers presenting an access token the secrets broker issued, under
// the RFC 9449 DPoP scheme. It is built from the broker DISPATCH_AGENT_SECRETS_URL names, which
// the approval relays already call, Dispatch's own projected ServiceAccount token from
// DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE, which it already presents there, and the origins a
// proof's 'htu' may name (proofOrigins): DISPATCH_SERVER_URL's, and each one
// DISPATCH_PROOF_ORIGINS adds, which is the one setting of its own, since a pod or a daemon calls
// Dispatch at an in-cluster address and signs the URL it calls. Spent proofs are recorded in
// Dispatch's own database (store.DPoPProofs), which the maintenance pass sweeps.
//
// It serves a read-only token (ciphertoken.Options.ServesReadOnly): one that holds read, which
// the broker mints from a read entry or narrows when a trade asks for read. Dispatch is the one
// service that asks for it, because its route table classes the GET rows such a token may call
// (the read class, readClasses in internal/dispatch/api), and every other route answers it
// 403 READ_ONLY_TOKEN before anything of the route runs.
//
// A deployment with no broker gets nil: Dispatch then has no such credential kind, and a DPoP
// presentation is refused as any non-bearer Authorization header has always been
// (401 UNAUTHORIZED).
func brokerTokenVerifier(boot bootConfig, database *store.Store, serverURL string) (*ciphertoken.Verifier, error) {
	if boot.AgentSecretsURL == "" {
		return nil, nil
	}
	return ciphertoken.New(ciphertoken.Options{
		BrokerURL: boot.AgentSecretsURL,
		Account:   ciphertoken.NewAccountToken(boot.AgentSecretsTokenFile),
		Origins:   proofOrigins(serverURL, boot.ProofOrigins),
		Replay:    store.NewDPoPProofs(database.Pool),
		// Dispatch's own rule for which address a request came from, the one its access log and
		// its other buckets read; the check itself has none, so this says whose proxy header is
		// trusted here (auth.ForwardedAddress).
		Address: auth.ForwardedAddress,
		// The read class and its gate are Dispatch's own (internal/dispatch/api), so a read-only
		// token reaches the reads of that class and no other route.
		ServesReadOnly: true,
	})
}

// proofOriginEntries reads DISPATCH_PROOF_ORIGINS: each comma-separated entry as the bare origin
// it is, written scheme://host. A value of nothing but whitespace is unset. An entry that is not a
// bare origin - empty, a scheme other than http or https, no host, a path, a query, a fragment or
// user info - refuses the start, naming the entry by its position and by its value where
// redact.URL prints it, as the DISPATCH_AGENT_SECRETS_URL refusal names its value.
func proofOriginEntries(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	entries := strings.Split(value, ",")
	origins := make([]string, 0, len(entries))
	for i, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("DISPATCH_PROOF_ORIGINS entry %d of %d is empty", i+1, len(entries))
		}
		parsed, err := url.Parse(entry)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
			parsed.Path != "" || parsed.User != nil || strings.ContainsAny(entry, "?#") {
			return nil, fmt.Errorf("DISPATCH_PROOF_ORIGINS entry %d of %d is %q (expected an origin: an absolute http(s) URL with no path, query, fragment or user)",
				i+1, len(entries), redact.URL(entry))
		}
		origins = append(origins, parsed.Scheme+"://"+parsed.Host)
	}
	return origins, nil
}

// proofOrigins is every origin a broker token's proof may name in its 'htu': DISPATCH_SERVER_URL's
// first, the one a proof naming none of them is checked against, then each origin added names. An
// origin equal to one before it, compared without case as an origin's scheme and host are, is
// taken once rather than refused: a deployment lists the public origin again beside the
// in-cluster one.
func proofOrigins(serverURL string, added []string) []string {
	origins := append(make([]string, 0, 1+len(added)), strings.TrimSuffix(serverURL, "/"))
	for _, origin := range added {
		if !slices.ContainsFunc(origins, func(taken string) bool { return strings.EqualFold(taken, origin) }) {
			origins = append(origins, origin)
		}
	}
	return origins
}

// healthzHandler answers the readiness probe: 200 while this process can serve requests, which is
// while Postgres answers on the health pool's own connection, and 503 while it does not; `ok` says
// the same. The store's block schema never decides it: a store a newer build migrated is installed
// at this build's own version (pmdoc.Install), and the process keeps serving, so a release that
// ships a type migration takes no process of the previous build out of service. Nothing
// here waits on the shared pool, and each probe bounds its own wait
// (store.healthProbeTimeout, files.healthTimeout), which records why a probe that answers late is
// as bad as one that never answers. The database and file-store probes run side by side: each
// takes up to two seconds, and one after the other they would take four, past the three-second
// prober (the compose healthcheck and the deploy script). A probe that fails while the database is
// unreachable is right for readiness and wrong for liveness: a liveness probe on it would restart
// every server process during a database failover.
//
// `nats` reports whether the server's publisher is connected to NATS (null when NATS is disabled),
// and that is all it does: it never decides `ok`. A process serves reads and writes while the bus
// is down or cut, and its outbox publishes what was written once the bus answers again, so a probe
// that failed with the bus would take every server process out of service at once for an outage
// each of them can serve through. The client declares a cut connection lost within seconds even
// when no packet comes back to say so (bus.WithSilentCutDetection), so the field follows the bus
// that closely. A check that needs the bus asserts `nats: true` itself, as the e2e scripts do.
//
// Where a file store is configured, `files` reports whether its bucket answered, and likewise
// never decides `ok`: a bucket outage, a slow HeadBucket or a grant someone changed would
// otherwise take documents, asks and comments down with the files. A deploy check that wants the
// bucket asserts `files: true` itself.
//
// Where a sign-in issuer is configured, `signin` reports whether its discovery document has been
// read, and where a service-account token issuer is, `service_tokens` reports the same of that
// one; each is null when not configured. Neither decides `ok`: a process whose issuer is down
// serves agents and the people whose confirmation stands, answers only what needs the issuer 503,
// and discovers it in the background (oidc.CodeFlow.Run, oidc.LazyVerifier.Run), so failing the
// probe would take that service away for an outage it serves through. A deploy check that needs
// sign-in asserts `signin: true` itself.
//
// Where a GitHub App is configured, `github` names each GitHub read Dispatch makes under a lease
// (githubreads), `webhook_redelivery`, `architecture_import`, `delivery_reconcile` and
// `ci_reconcile` in that order, each with its newest attempt's `state`, `at` and `cause`; with no
// App it is null. The state is `succeeded` when that attempt sent GitHub requests and noted no
// failure, `failed` when it noted one, `no_attempt` while the record holds none, `unknown` before
// this process has read the record, and `off` where this process does not run the read (no App
// private key, or no NATS for the redelivery sweep and the CI reconcile). `at` is when a
// succeeded or failed attempt was recorded, by the database's clock, and `cause` a failed one's
// first failure, cut to githubreads.CauseLimit bytes; each is null where the state carries none.
// It never decides `ok` or the status: a process serves everything but GitHub's own reads while
// GitHub fails, so failing the probe would take every server process out of service for an outage
// each of them serves through. It is read from this process's copy of the record
// (githubreads.Copy), which a write by any process refreshes, so it adds no statement to the
// database and waits on nothing. The delivery intakes read GitHub too, for each webhook event, and
// report through `delivery_reconcile`: the delivery reconcile reads the same pull requests and
// runs with the same App every interval, so a failure of those reads shows there within one pass.
//
// Beside those it reports what is deployed: `commit`, the legion commit this process's binary
// was built from (null when the build did not stamp one), `fleet_commit`, the commit of the latest
// announcement that every live server process serves one build (startup.Announcer.FleetCommit:
// null when there is none, or before this process's announcer has first read the table, and up
// to one announcer interval behind the table, a read the announcer makes anyway, so the probe
// adds no statement to the database), and `schema_version`, the highest migration the database
// has applied, read by the same probe (null when the database did not answer). A deploy check
// compares commit and schema_version with the commit its image pin names and that commit's
// migrations, so neither is ever filled with a guess.
//
// `outbox_publisher` says whether this process publishes the events outbox now: true while it
// holds the outbox's publisher hold and the hold's deadline has not passed, false when it stands
// by, lost the hold, or runs with NATS disabled (publisher nil). One process of a deployment
// publishes at a time, so a healthy process may well report false, and it never decides `ok`.
//
// `idempotency_keys` is true on every build that judges a write's Idempotency-Key (INFRA-258,
// api/request_keys.go): the deploy check reads it (scripts/e2e/lib/dispatch-keyed-comment.ts), since
// an earlier Dispatch ignores the header and would run a retried write again. It is a fact of the
// binary, never of the probe, so it is true whatever the database or NATS answer.
func healthzHandler(database *store.Store, natsClient *bus.Client, publisher *outbox.Outbox, fileStore files.Store, commit string, announcer *startup.Announcer, issuers issuers, reads *gitHubReads) http.HandlerFunc {
	var reportedCommit *string
	if commit != "" {
		reportedCommit = &commit
	}
	return func(w http.ResponseWriter, req *http.Request) {
		var filesOK *bool
		filesProbed := make(chan struct{})
		if fileStore != nil {
			go func() {
				defer close(filesProbed)
				reachable := true
				if err := fileStore.Healthy(req.Context()); err != nil {
					slog.Warn("dispatch: file store probe failed", "error", err)
					reachable = false
				}
				filesOK = &reachable
			}()
		} else {
			close(filesProbed)
		}
		databaseOK := database != nil && database.Pool != nil
		var schemaVersion *int
		if databaseOK {
			// A 503 that records no reason works against the point of the probe: a closed
			// pool, a deadline on a stalled link, an authentication failure and a refused
			// dial are four incidents with four next steps, and the body distinguishes
			// none of them. One line per failed poll, for as long as the outage lasts.
			version, err := database.Pool.Healthy(req.Context())
			if err != nil {
				slog.Warn("dispatch: health probe failed", "error", err)
				databaseOK = false
			} else {
				schemaVersion = &version
			}
		}
		var natsOK *bool
		if natsClient != nil {
			connected := natsClient.Connected()
			natsOK = &connected
		}
		var signInOK, serviceTokensOK *bool
		if issuers.signIn != nil {
			discovered := issuers.signIn.Discovered()
			signInOK = &discovered
		}
		if issuers.serviceTokens != nil {
			discovered := issuers.serviceTokens.Discovered()
			serviceTokensOK = &discovered
		}
		<-filesProbed
		ok := databaseOK
		status := http.StatusOK
		if !ok {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(struct {
			OK              bool               `json:"ok"`
			DB              bool               `json:"db"`
			NATS            *bool              `json:"nats"`
			Files           *bool              `json:"files"`
			SignIn          *bool              `json:"signin"`
			ServiceTokens   *bool              `json:"service_tokens"`
			GitHub          *gitHubReadsMember `json:"github"`
			Commit          *string            `json:"commit"`
			FleetCommit     *string            `json:"fleet_commit"`
			SchemaVersion   *int               `json:"schema_version"`
			OutboxPublisher bool               `json:"outbox_publisher"`
			IdempotencyKeys bool               `json:"idempotency_keys"`
		}{
			OK: ok, DB: databaseOK, NATS: natsOK, Files: filesOK, SignIn: signInOK, ServiceTokens: serviceTokensOK,
			GitHub: reads.member(), Commit: reportedCommit, FleetCommit: announcer.FleetCommit(), SchemaVersion: schemaVersion,
			OutboxPublisher: publisher.Publishing(), IdempotencyKeys: true,
		})
	}
}

// testHookDuration reads the test-only duration setting name into target, leaving target as it
// is when the setting is unset. Such a setting is refused without DISPATCH_TEST_HOOKS=1, and so
// is any value that is not a positive Go duration.
func testHookDuration(env settingValues, name string, testHooksEnabled bool, target *time.Duration) error {
	raw := strings.TrimSpace(env.get(name))
	if raw == "" {
		return nil
	}
	if !testHooksEnabled {
		return fmt.Errorf("DISPATCH_TEST_HOOKS=1 required with %s", name)
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return fmt.Errorf("%s=%q (expected a positive Go duration)", name, raw)
	}
	*target = value
	return nil
}

// testHookBool reads the test-only yes/no setting name into target, leaving target as it is when
// the setting is unset. Such a setting is refused without DISPATCH_TEST_HOOKS=1; set, only "1"
// turns the hook on, so a value meant as a yes in another spelling leaves it off rather than
// quietly changing the server a test reads.
func testHookBool(env settingValues, name string, testHooksEnabled bool, target *bool) error {
	raw := strings.TrimSpace(env.get(name))
	if raw == "" {
		return nil
	}
	if !testHooksEnabled {
		return fmt.Errorf("DISPATCH_TEST_HOOKS=1 required with %s", name)
	}
	*target = raw == "1"
	return nil
}

// roleHolderWindows are the session windows Dispatch judges a role route's holder by, read
// through the helper the listener reads its own with (envconf.SessionWindows): the listener's
// two settings, their compiled defaults, a warning and the default for a value no duration can
// be read from, and a refusal to start for one that is not positive. It hands the helper the
// settings table's own lookup, so Dispatch still reads its environment in one place
// (settings.go, TestNoReaderBypassesTheSettingsTable).
//
// Both processes judge the same holder, so both carry the same values. Dispatch left to the
// compiled windows calls a holder gone whose horizon the deployment raised, counts its role
// route reached and marks the event published with nothing sent (forwardOf), while the
// listener, carrying the raise, still calls that holder unresponsive and keeps its claim: the
// event reaches it never (dispatch://INFRA-392).
func roleHolderWindows(env settingValues) (roles.Windows, error) {
	live, horizon, err := envconf.SessionWindows(slog.Default(), env.lookup)
	if err != nil {
		return roles.Windows{}, err
	}
	return roles.Windows{Live: live, Horizon: horizon}, nil
}

// resolveListen says where the server serves. With DISPATCH_LISTEN_FD it is the inherited socket
// that names, at the socket's own address, and DISPATCH_LISTEN_HOST and DISPATCH_PORT, each only
// when set, must name that address: no default stands in for one that is unset. An empty value
// reads as unset: without the socket an empty one is its default, and the listener reads an empty
// ENVOY_LISTEN_HOST or PORT as unset too. Without DISPATCH_LISTEN_FD, it is the address
// listenAddress builds, for main to bind. It only inspects the socket; main adopts it, so nothing a
// caller of resolveBootConfig gets owns a descriptor.
func resolveListen(env settingValues) (string, *listenfd.Socket, error) {
	descriptor, set := env.lookup("DISPATCH_LISTEN_FD")
	if !set {
		address, err := listenAddress(env)
		return address, nil, err
	}
	socket, err := listenfd.Inspect("DISPATCH_LISTEN_FD", descriptor)
	if err != nil {
		return "", nil, err
	}
	setting := func(name string) listenfd.Setting {
		value := strings.TrimSpace(env.get(name))
		return listenfd.Setting{Name: name, Value: value, Set: value != ""}
	}
	if err := socket.CheckAgainst(setting("DISPATCH_LISTEN_HOST"), setting("DISPATCH_PORT")); err != nil {
		return "", nil, err
	}
	return socket.Addr().String(), &socket, nil
}

// listenAddress builds the listen address from DISPATCH_LISTEN_HOST and DISPATCH_PORT
// (defaultListenPort when unset). An empty host binds every interface (the containerized
// production default); local compose deployments set 127.0.0.1. An IPv6 host may be written with
// or without brackets: one pair comes off here and JoinHostPort puts it back.
func listenAddress(env settingValues) (string, error) {
	host := strings.TrimSpace(env.get("DISPATCH_LISTEN_HOST"))
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	port := strings.TrimSpace(env.get("DISPATCH_PORT"))
	if port == "" {
		port = defaultListenPort
	} else if _, err := parsePort("DISPATCH_PORT", port); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, port), nil
}
