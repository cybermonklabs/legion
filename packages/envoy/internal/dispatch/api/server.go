// Package api serves Dispatch's native workspace HTTP API.
package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/githubrate"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/cipher"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/oidc"
)

// issueKeyShape is a Dispatch issue key, `INFRA-431`, unanchored. Go's regular expressions and
// Postgres's read it alike, so issueKeyPattern and the near-copy gate's title rule
// (duplicateTitleKeyPattern) take the same shape from it.
const issueKeyShape = `[A-Z][A-Z0-9]{1,9}-[0-9]+`

var (
	projectKeyPattern = regexp.MustCompile(contracts.ProjectKeyPattern)
	issueKeyPattern   = regexp.MustCompile(`^` + issueKeyShape + `$`)
)

// projectKeyProblem is every route's refusal of a value that is not a project key: the field it
// came in and the rule, worded once in contracts (PROJECT_KEY_RULE) so the dispatch command and
// the server say the same thing.
func projectKeyProblem(field, value string) string {
	return fmt.Sprintf("%s %q is not a project key: %s", field, value, contracts.ProjectKeyRule)
}

// ServiceTokenVerifier verifies a Kubernetes pod's projected service-account token.
// *oidc.LazyVerifier is the one Dispatch serves with: until its issuer is discovered every Verify
// fails with an error matching oidc.ErrIssuerUnavailable. A Verify that waits on the issuer - its
// discovery or its key set - runs the before-wait hook its context carries first
// (oidc.WithBeforeWait), where the request gives its admission unit back (serviceTokenActor).
type ServiceTokenVerifier interface {
	Verify(ctx context.Context, raw string) (oidc.Claims, error)
}

// AgentPublisher is what a person's targeted message is queued through when no store of the
// listener's holds its session (queueForUnknownSession, message_queue_publish.go): the bus client
// (*bus.Client) where the deployment has NATS, and an AgentPublishRecorder in a test process that
// has none.
type AgentPublisher interface {
	// PublishReportingDuplicateWithin publishes the envelope and returns once the stream has
	// acknowledged it, reporting whether the stream already held it. ctx bounds the whole publish,
	// and a publish whose ctx has ended sends nothing.
	PublishReportingDuplicateWithin(context.Context, contracts.Envelope) (bool, error)
}

// Deps are the API's application dependencies.
type Deps struct {
	Store       *store.Store
	Identity    identity.Identity
	AgentTokens *auth.SharedAgentTokens
	// NewIssueLabel and NewIssueSession configure the issue the dashboard's search bar files
	// from the words a person typed when those words match nothing it found
	// (GET /api/v1/settings/new-issue; cmd/dispatch: DISPATCH_NEW_ISSUE_LABEL,
	// DISPATCH_NEW_ISSUE_SESSION). NewIssueLabel empty is an issue that carries no label;
	// NewIssueSession empty is an issue made with no conversation started on it.
	NewIssueLabel   string
	NewIssueSession string
	ServerURL       string
	Docs            docs.API
	// Embedder embeds a search request's query for meaning search (LEGION-549), and its Identity
	// is the only one search compares queries with (INFRA-393); nil means Dispatch has no
	// embedder, and search answers keyword-only on every request, always setting
	// SearchResponse.Degraded.
	Embedder embed.Embedder
	// Answerer answers one question from what Dispatch holds (INFRA-1279), for the three going
	// routes; nil means the deployment configured no model, where a run is still recorded and
	// its stream carries the error frame contracts.GoingErrorNoModelConfigured, and the
	// capability route says no model is configured.
	Answerer Answerer
	Envoy    *envoy.Client
	Events   *events.Broker
	// GitHub calls the GitHub App API for architecture-source access checks and the web app's
	// GitHub reads; nil is the "no app credentials yet" state and answers ErrNoAppKey.
	GitHub *githubapp.Client
	// BackgroundGitHub is the same App's client with its shared rate limits read and recorded on
	// the background pool, which it opens at each read and write rather than when it is built
	// (ratelimit.OnBackground), so a call through it takes no request-pool connection. The routes'
	// architecture importer calls GitHub through it, and cmd/dispatch's background loops (the
	// architecture ticker, the delivery reconcile, the intakes) share it, so they and a route's
	// import use one installation-token cache. It is GitHub where Deps holds no store.
	BackgroundGitHub *githubapp.Client
	// Architecture imports a project's architecture model from its configured
	// sources; the ticker, the Refresh route, and the sync tool share it, so this process never
	// fetches one project twice at once. The source row, not this sharing, orders every process's
	// writes (architecture.Importer).
	Architecture *architecture.Importer
	// Renames is the rename pairs the boot read (store.ReadRepoRenames). The results routes name
	// a check's repository by its canonical name under them, and GET /api/v1/issues/resolve finds
	// a link stored under either name of a pair. The zero value holds no pairs.
	Renames refs.Renames
	// Attributor attributes the pull requests a results read or a recorded result reads through
	// for (delivery.Attributor.ReadThroughCommitPulls). NewDeps builds it of Renames, so it
	// attributes by the same pairs renamedRepo and resolveIssueRef read.
	Attributor *delivery.Attributor
	// OIDC verifies a JWT-shaped bearer as a Kubernetes pod's projected
	// service-account token; nil is the unconfigured deployment, where a
	// JWT-shaped bearer is only ever an unknown personal token. A configured
	// verifier stays non-nil while its issuer is undiscovered, so such a bearer
	// is answered 503 then, never taken for a personal token.
	OIDC ServiceTokenVerifier
	// BrokerTokens decides a caller presenting an access token the secrets broker issued, under
	// the DPoP scheme (brokerTokenActor); nil is a deployment with no broker, which has no such
	// credential kind and answers a DPoP presentation exactly as it always has.
	BrokerTokens *ciphertoken.Verifier
	// AgentStream relays a live session's own conversation to a human watching it; nil is a
	// deployment with no NATS, where an agent view is refused STREAM_UNAVAILABLE.
	AgentStream agentstream.Source
	// AgentPublisher queues a person's targeted message on its session's own subject when neither
	// the listener's registry nor its known-session store holds that session. Nil is a deployment
	// with no NATS and no test hooks, where such a message settles failed instead.
	AgentPublisher AgentPublisher
	// AgentSecrets relays the credential-request UI to the secrets broker; nil (the broker URL
	// is unconfigured) means the feature is off: the pending list answers null, and every other
	// handler that needs it answers 404 FEATURE_OFF.
	AgentSecrets *cipher.Client
	// LegionStatusScope turns on the refusal of three writes of a Legion issue (cmd/dispatch:
	// DISPATCH_LEGION_STATUS_SCOPE): a write of its status, the removal of its legion label, and
	// a move that leaves it under no labelled issue. With it on, Dispatch refuses each of them to
	// a credential that holds neither model.ScopePerson nor model.ScopeLegionStatus, but for the
	// status write of the architect of an issue above (legion_status_scope.go). False refuses none
	// of them, which is every deployment that has not turned it on.
	LegionStatusScope bool
	// Files holds uploaded files' bytes outside Postgres (cmd/dispatch: DISPATCH_FILE_STORE_BUCKET);
	// nil keeps them in each version's row, and a row that still holds bytes is served from the
	// row either way (files.BackfillRows moves them).
	Files            files.Store
	TestHooksEnabled bool
	// StreamHeartbeat is how often a document socket and a live socket resolve their caller again
	// (whileCallerResolves), each closing once the caller no longer resolves - at once for a
	// refusal, at the third transient failure in a row (heartbeatTransientCeiling) - and a live
	// socket pings its page and sends it a heartbeat frame. It also bounds each of those checks.
	StreamHeartbeat time.Duration
	// Heartbeats counts what those checks met (HeartbeatCounts), which cmd/dispatch reads on its
	// metrics endpoint; nil counts nothing.
	Heartbeats *HeartbeatCounts
	// TreeStores counts the stores of display trees a read left to run behind it that were
	// skipped because as many as the background pool's half already ran (TreeStoreCounts,
	// displayTreeReader.storeOf), which cmd/dispatch reads on its metrics endpoint; nil counts
	// nothing.
	TreeStores *TreeStoreCounts
	// Admission admits every table route's request but the test hooks' (register); nil is a
	// Limiter at admission.DefaultLimits. A test sets a small one to fill it.
	Admission *admission.Limiter
	// ReadDeadline bounds every table route's read (GET, HEAD) that the route itself does not bound
	// otherwise (apiRoute.Download); zero is defaultReadDeadline. A test sets a short one. A write
	// has no deadline.
	ReadDeadline time.Duration
	// DownloadDeadline replaces the deadline of the four file version downloads
	// (downloadDeadline, apiRoute.Download), which bounds the work each does before its answer
	// begins; zero is downloadDeadline's five minutes. Only a test sets one, so a download whose
	// deadline passes under it is the test's choice rather than five minutes of wall clock.
	DownloadDeadline time.Duration
	// RouteRosterTTL replaces how long a stored listener answer serves issue reads and agents
	// lists (routeRosterTTL, issue_route_status.go). Zero keeps five seconds; only a test process
	// sets another (cmd/dispatch: DISPATCH_TEST_ROUTE_ROSTER_TTL, under the test hooks), so a row
	// that reads two processes inside one answer's life needs no timing window.
	RouteRosterTTL time.Duration
	// ListenerTakeoverWindow replaces how long a sender keeps trying a listener that is being
	// replaced (listenerTakeoverWindow, envoy_resolve.go). Zero keeps twelve seconds; a
	// deployment leaves it zero, and only a test process sets another (cmd/dispatch:
	// DISPATCH_TEST_LISTENER_TAKEOVER_WINDOW, under the test hooks), so a send to a session that
	// is gone settles without sleeping the production window.
	ListenerTakeoverWindow time.Duration
	// HorizonStallAfter and HorizonWarnEvery replace how long a pending event-horizon
	// observation waits before the process logs its stall warning, and how often each of that
	// Horizon's three warnings repeats - the stall warning while the stall stands, the
	// failed-observation warning and the failed-persist warning (events.Horizon.SetStallWarning).
	// Zero keeps the horizon's own five and thirty seconds; a deployment leaves both zero, and
	// only a test process sets another (cmd/dispatch: DISPATCH_TEST_HORIZON_STALL_AFTER and
	// DISPATCH_TEST_HORIZON_WARN_EVERY, under the test hooks), so a test whose wait keys on the
	// stall warning sees it inside its own patience.
	HorizonStallAfter time.Duration
	HorizonWarnEvery  time.Duration
	// TestHookTimeout replaces how long a test hook waits for every process's acknowledgement
	// (testHookTimeout, test_hooks_fanout.go). Zero keeps ten seconds; neither
	// routes.BuildAppContext nor cmd/dispatch sets it, so every deployment with the hooks on and
	// the browser harness run ten seconds. Only this package's tests set another, so a test
	// proving a silent process is named does not sleep the full timeout.
	TestHookTimeout time.Duration
	// LiveSocketAge is how long a GET /ws/live socket lives before it closes with 4001 (max age),
	// drawn as it opens. Nil draws it uniformly from [contracts.LiveSocketMinAge,
	// contracts.LiveSocketMaxAge), so the sockets a deploy opened together do not all end
	// together; a test returns a fixed age.
	LiveSocketAge func() time.Duration
	// LiveSocketClock is what a GET /ws/live socket's read deadlines run on: the pong deadline, two
	// heartbeats after the page's last pong, the wait for the page's close frame once the socket
	// sent its own (liveSocketCloseWait), and the drain's wait for the rest of a frame past the read
	// limit, liveSocketCloseWait too, which takes the pong deadline's place (endReads). Nil is
	// clock.Real(), as a deployment leaves it; only a test sets another, a clocktest fake on which
	// no page's reads end until the test moves it there. It is not Clock, so a test that moves the
	// server's clock through its heartbeats ends no read with it.
	LiveSocketClock clock.Clock
	// ListenerRosterUncached is for end-to-end test processes only: every read of the listener's
	// session list (route_status on an issue read, GET /api/v1/agents) asks the listener rather
	// than reading the stored answer every process shares (the route_roster row), so a test that
	// changes the listener's sessions sees the change at its next read. A deployment leaves it
	// false.
	ListenerRosterUncached bool
	// OutboxPublishes is that an outbox publishes this deployment's events: cmd/dispatch sets it
	// unless NATS is disabled (DISPATCH_NATS_DISABLED=1). A mention's send then goes only once no
	// earlier event of its comment's owner still owes a send its session may take
	// (model.MentionCopyHolderSQL, deliverResolvedCommentMention), the order the outbox keeps for its
	// own sends. With no outbox no event is ever sent, so there is no order to keep with them and
	// the wait would never end: the send goes at once, as it does in every test of this package
	// that runs no outbox.
	OutboxPublishes bool
	// Metrics is the process's registry, which register adds the gauges of the mention sends held
	// behind an earlier event to (registerHeldGauges); nil adds none, as this package's tests run.
	Metrics *metrics.Registry
	// AnswersLatency times each GET /api/v1/agents/{session_id}/answers read this process
	// serves from the moment it enters the route's handler chain, as the access-log line times
	// it (routeHandler), for the keyword fill's pause, which shares it (textversions); nil times
	// none.
	AnswersLatency *sessionanswers.Latency
	// DashboardEntry is the entry chunk (`/assets/index-*.js`) of the dashboard build this process
	// serves, "" when it serves none; GET /api/v1/dashboard/builds answers it as serving.
	DashboardEntry string
	// BuildCommit is the legion commit this process was built from, which cmd/dispatch holds in
	// its own `buildCommit` and the image build stamps (docker/Dockerfile's LEGION_COMMIT). It
	// is hashed into every list validator (listETag), so two builds never answer one ETag for
	// two bodies; a build nothing stamped - every `go test` binary, a devbox `go run` - leaves
	// it empty, which is one identity for every such build and is why a deploy is what it is
	// for rather than a key anything depends on being unique.
	BuildCommit string
	// Clock is the clock these timers of this package run on: the heartbeat on which a socket
	// resolves its caller again (whileCallerResolves), the bound on each listener lookup of a
	// comment's route and mentions (resolveMentionTarget's mentionLookupTimeout), the listener
	// client's window, the take-over's tries (takeoverRetry), the related-item check's allowance
	// (relatedBounds), an agent view's wait for its replay (agentStreamReplayTimeout) and the time
	// each access-log line records (accesslog.HandlerOn), which a test hands AnswersLatency's
	// clock too. Three listener lookups bound themselves on the wall clock instead
	// (context.WithTimeout), and only the listener client's window of each runs on this clock:
	// roleHolderMayBe, the question whether a session may hold a role (mentionLookupTimeout);
	// listRouteRoster, the read of the listener's session list (routeRosterTimeout); and a
	// broadcast's resolution of its recipients (resolveBroadcastSessions,
	// broadcastResolveAllowance). Nil, which every deployment leaves it, is clock.Real(), for a
	// server Register builds and for one built directly alike (server.clock); a test passes a
	// clocktest.Fake and moves it.
	Clock clock.Clock
	// ParentStatusRule is DISPATCH_PARENT_STATUS_RULE: while it is on, a write that would leave
	// a parent's status out of line with its children's (R24) is refused 409
	// PARENT_STATUS_CONFLICT. The zero value is off.
	ParentStatusRule ParentStatusRule
}

// defaultStreamHeartbeat is StreamHeartbeat when DepsInput leaves it zero.
const defaultStreamHeartbeat = 15 * time.Second

// DepsInput contains raw boot values used to construct API dependencies.
type DepsInput struct {
	Store       *store.Store
	Identity    identity.Identity
	AgentTokens *auth.SharedAgentTokens
	// NewIssueLabel and NewIssueSession are the two new-issue settings; see Deps. A label longer
	// than one an issue may carry, or a session id that is no session id, refuses startup.
	NewIssueLabel   string
	NewIssueSession string
	ServerURL       string
	EnvoyURL        string
	// EnvoyToken is the bearer the listener client sends (cmd/dispatch: ENVOY_TOKEN); empty sends
	// none.
	EnvoyToken string
	// EnvoyAccountTokenFile is the file holding Dispatch's own projected service-account token for
	// the listener's audience (cmd/dispatch: ENVOY_ACCOUNT_TOKEN_FILE), which every listener call
	// reads and presents in EnvoyToken's place (envoy.WithAccountTokenFile); empty presents
	// EnvoyToken. It is the one credential the listener takes a send's role mark from.
	EnvoyAccountTokenFile string
	// EnvoyGoogleAudience is the audience of the identity token the metadata server mints for
	// Dispatch's Google Cloud machine (cmd/dispatch: ENVOY_ACCOUNT_GOOGLE_AUDIENCE), which every
	// listener call presents in EnvoyToken's place (envoy.WithGoogleIdentity); empty presents none.
	// With neither it nor EnvoyAccountTokenFile, Dispatch presents no identity and refuses every
	// send for a role (envoy.ErrNoListenerIdentity).
	EnvoyGoogleAudience string
	// EnvoyTimeout replaces the listener client's window. Zero keeps the client's own default;
	// a test exercising a receipt timeout sets a short one rather than waiting that out.
	EnvoyTimeout time.Duration
	// Docs is the document service the API serves documents through, and NewDeps refuses an input
	// without one. Its caller builds it (docs.New; cmd/dispatch's newDocumentService) and shuts it
	// down (docs.Service.Shutdown) once the server is done with it. NewDeps builds none of its own:
	// nothing would hold a service it built to shut it down, and that service's compaction worker
	// (startCompactionQueue) would run for the rest of the process.
	Docs docs.API
	// Embedder embeds a search request's query for meaning search (LEGION-549); nil turns it off.
	Embedder embed.Embedder
	// Answerer answers a going request's question (INFRA-1279); nil means no model is
	// configured. See Deps.Answerer; cmd/dispatch builds it (newAnswerer).
	Answerer Answerer
	Events   *events.Broker
	// Renames is the rename pairs the boot read; see Deps.Renames. The zero value holds none.
	// NewDeps builds Deps.Attributor of them.
	Renames refs.Renames

	// App is the loaded GitHub App credentials (nil when unconfigured);
	// GitHubAPIBase overrides the GitHub API origin (DISPATCH_GITHUB_API_BASE).
	App           *auth.AppConfig
	GitHubAPIBase string
	OIDC          ServiceTokenVerifier

	// GitHubRequestTimeout replaces the window of each GitHub call the two GitHub clients make
	// (githubapp.RequestTimeout). Zero keeps the client's own twenty seconds; a deployment leaves
	// it zero (cmd/dispatch sets none), and only a test sets another, so a row that holds GitHub
	// past that window does not see its calls cut off by it.
	GitHubRequestTimeout time.Duration

	// GitHubRate is the process's one count of its calls to GitHub, which the GitHub client
	// counts on (githubapp.WithRateCounter) and the rate reading reports; nil counts none.
	GitHubRate *githubrate.Counter
	// BrokerTokens decides a broker-issued DPoP token; nil where no broker is configured. See
	// Deps.BrokerTokens; cmd/dispatch builds it (brokerTokenVerifier).
	BrokerTokens *ciphertoken.Verifier
	// AgentStream is the live agent conversation relay; nil where the deployment has no NATS.
	AgentStream agentstream.Source
	// AgentPublisher queues a targeted message for a session no store of the listener's holds;
	// nil queues none. See Deps.AgentPublisher; cmd/dispatch passes its NATS client.
	AgentPublisher AgentPublisher
	// AgentSecretsURL is the secrets broker's API base URL (DISPATCH_AGENT_SECRETS_URL); empty
	// means the feature is off. AgentSecretsTokenFile is the file holding Dispatch's own
	// projected Kubernetes ServiceAccount token, which the client presents on every broker call.
	AgentSecretsURL       string
	AgentSecretsTokenFile string
	// LegionStatusScope is DISPATCH_LEGION_STATUS_SCOPE. See Deps.LegionStatusScope.
	LegionStatusScope bool
	// Files is the uploaded-file store; nil keeps files in Postgres. See Deps.Files.
	Files            files.Store
	TestHooksEnabled bool
	// StreamHeartbeat replaces the heartbeat of the document and live sockets
	// (Deps.StreamHeartbeat). Zero keeps fifteen seconds; a test proving a connection closes
	// sets a short one.
	StreamHeartbeat time.Duration
	// Heartbeats is Deps.Heartbeats.
	Heartbeats *HeartbeatCounts
	// TreeStores is Deps.TreeStores.
	TreeStores *TreeStoreCounts
	// Admission, ReadDeadline and DownloadDeadline are Deps.Admission, Deps.ReadDeadline and
	// Deps.DownloadDeadline.
	Admission        *admission.Limiter
	ReadDeadline     time.Duration
	DownloadDeadline time.Duration
	// RouteRosterTTL replaces the five seconds a stored listener answer serves issue reads and
	// agents lists; see Deps.RouteRosterTTL.
	RouteRosterTTL time.Duration
	// ListenerTakeoverWindow replaces the twelve-second listener take-over window; see
	// Deps.ListenerTakeoverWindow.
	ListenerTakeoverWindow time.Duration
	// HorizonStallAfter and HorizonWarnEvery replace the five-second stall threshold and the
	// thirty-second repeat of every event-horizon warning; see Deps.HorizonStallAfter.
	HorizonStallAfter time.Duration
	HorizonWarnEvery  time.Duration
	// TestHookTimeout replaces the ten-second test-hook acknowledgement timeout; see
	// Deps.TestHookTimeout.
	TestHookTimeout time.Duration
	// ListenerRosterUncached is for end-to-end test processes only; see
	// Deps.ListenerRosterUncached.
	ListenerRosterUncached bool
	// OutboxPublishes is that an outbox publishes this deployment's events; see
	// Deps.OutboxPublishes.
	OutboxPublishes bool
	// Metrics is the process's metrics registry; see Deps.Metrics.
	Metrics *metrics.Registry
	// AnswersLatency times the answers reads for the keyword fill's pause; see
	// Deps.AnswersLatency.
	AnswersLatency *sessionanswers.Latency
	// DashboardEntry is this process's dashboard entry chunk; see Deps.DashboardEntry.
	DashboardEntry string
	// BuildCommit is the commit this process was built from; see Deps.BuildCommit.
	BuildCommit string
	// Clock is the clock the API's timers and the listener client's window run on; see
	// Deps.Clock. Nil is clock.Real().
	Clock clock.Clock
	// ParentStatusRule is DISPATCH_PARENT_STATUS_RULE; see Deps.ParentStatusRule.
	ParentStatusRule ParentStatusRule
}

// NewDeps parses boot configuration once and returns API dependencies. It refuses an input with
// no document service (DepsInput.Docs).
func NewDeps(input DepsInput) (Deps, error) {
	newIssue, problem := newIssueSettingsFrom(input.NewIssueLabel, input.NewIssueSession)
	switch problem {
	case "label":
		return Deps{}, fmt.Errorf("invalid DISPATCH_NEW_ISSUE_LABEL %q (at most %d characters)", newIssue.Label, contracts.IssueLabelMax)
	case "session":
		return Deps{}, fmt.Errorf("invalid DISPATCH_NEW_ISSUE_SESSION %q (expected a session id)", newIssue.Session)
	}
	if input.Docs == nil {
		return Deps{}, errors.New("api.NewDeps: Docs is required: the caller builds the document service and shuts it down once its server is done with it")
	}
	if input.Events == nil {
		input.Events = events.NewBroker()
	}
	var envoyClient *envoy.Client
	if envoyURL := strings.TrimSpace(input.EnvoyURL); envoyURL != "" {
		options := []envoy.Option{envoy.WithToken(input.EnvoyToken)}
		if input.EnvoyAccountTokenFile != "" {
			options = append(options, envoy.WithAccountTokenFile(input.EnvoyAccountTokenFile))
		}
		if input.EnvoyGoogleAudience != "" {
			options = append(options, envoy.WithGoogleIdentity(input.EnvoyGoogleAudience))
		}
		if input.EnvoyTimeout > 0 {
			options = append(options, envoy.WithTimeout(input.EnvoyTimeout))
		}
		if input.Clock != nil {
			options = append(options, envoy.WithClock(input.Clock))
		}
		envoyClient = envoy.New(envoyURL, options...)
	}
	// The GitHub client counts its calls on the process's one Counter for the rate reading, and
	// obeys and records the rate limits every process shares (github_rate_limits): GitHub budgets
	// the App and each installation, not this process.
	githubOptions := []githubapp.Option{githubapp.WithRateCounter(input.GitHubRate)}
	if input.Store != nil {
		githubOptions = append(githubOptions, githubapp.WithLimits(ratelimit.New(input.Store.Pool)))
	}
	// Both clients take the window a test put on them (DepsInput.GitHubRequestTimeout).
	newGitHub := func(options ...githubapp.Option) (*githubapp.Client, error) {
		if input.GitHubRequestTimeout > 0 {
			options = append(options, githubapp.WithRequestTimeoutForTest(input.GitHubRequestTimeout))
		}
		return githubapp.New(input.App, input.GitHubAPIBase, options...)
	}
	github, err := newGitHub(githubOptions...)
	if err != nil {
		return Deps{}, err
	}
	// A route's architecture sync imports on the background pool, with the GitHub client whose
	// rate-limit ledger is read and recorded there (Deps.BackgroundGitHub): the route gives its
	// admission unit back while it waits on the import's flight (syncArchitectureSource), which
	// waits on GitHub for up to its sync bound, so none of the flight's statements may run where
	// admission counts the requests' connections. Both open the background pool where they use it,
	// never here (architecture.NewImporterOnBackground, ratelimit.OnBackground): NewDeps opens no
	// pool, so a store whose pool is closed still builds, and a background pool that cannot open
	// fails the import or the ledger read that needed it, as it fails every other background user.
	// cmd/dispatch derives the ticker's importer from this one (Importer.On) with the same client,
	// so a Refresh and a tick of one source still share one flight.
	var architectureImporter *architecture.Importer
	backgroundGitHub := github
	if input.Store != nil && input.Store.Pool != nil {
		// The background client counts its calls on the process's one Counter too
		// (githubapp.WithRateCounter): the importer, the delivery intakes and the reconcile spend
		// the same installations' budget the API's client does.
		if backgroundGitHub, err = newGitHub(githubapp.WithRateCounter(input.GitHubRate), githubapp.WithLimits(ratelimit.OnBackground(input.Store.Pool))); err != nil {
			return Deps{}, err
		}
		architectureImporter = architecture.NewImporterOnBackground(input.Store, backgroundGitHub, input.Events)
	} else {
		architectureImporter = architecture.NewImporter(input.Store, github, input.Events)
	}
	var agentSecretsClient *cipher.Client
	if url := strings.TrimSpace(input.AgentSecretsURL); url != "" {
		agentSecretsClient = cipher.New(url, input.AgentSecretsTokenFile)
	}
	heartbeat := input.StreamHeartbeat
	if heartbeat == 0 {
		heartbeat = defaultStreamHeartbeat
	}
	return Deps{
		Store:                  input.Store,
		Identity:               input.Identity,
		AgentTokens:            input.AgentTokens,
		NewIssueLabel:          newIssue.Label,
		NewIssueSession:        newIssue.Session,
		ServerURL:              strings.TrimSuffix(input.ServerURL, "/"),
		Docs:                   input.Docs,
		Embedder:               input.Embedder,
		Answerer:               input.Answerer,
		Envoy:                  envoyClient,
		Events:                 input.Events,
		GitHub:                 github,
		BackgroundGitHub:       backgroundGitHub,
		Architecture:           architectureImporter,
		Renames:                input.Renames,
		Attributor:             delivery.NewAttributor(input.Renames),
		OIDC:                   input.OIDC,
		BrokerTokens:           input.BrokerTokens,
		AgentStream:            input.AgentStream,
		AgentPublisher:         input.AgentPublisher,
		AgentSecrets:           agentSecretsClient,
		LegionStatusScope:      input.LegionStatusScope,
		Files:                  input.Files,
		TestHooksEnabled:       input.TestHooksEnabled,
		StreamHeartbeat:        heartbeat,
		Heartbeats:             input.Heartbeats,
		TreeStores:             input.TreeStores,
		Admission:              input.Admission,
		ReadDeadline:           input.ReadDeadline,
		DownloadDeadline:       input.DownloadDeadline,
		RouteRosterTTL:         input.RouteRosterTTL,
		ListenerTakeoverWindow: input.ListenerTakeoverWindow,
		HorizonStallAfter:      input.HorizonStallAfter,
		HorizonWarnEvery:       input.HorizonWarnEvery,
		TestHookTimeout:        input.TestHookTimeout,
		ListenerRosterUncached: input.ListenerRosterUncached,
		OutboxPublishes:        input.OutboxPublishes,
		Metrics:                input.Metrics,
		AnswersLatency:         input.AnswersLatency,
		DashboardEntry:         input.DashboardEntry,
		BuildCommit:            input.BuildCommit,
		Clock:                  input.Clock,
		ParentStatusRule:       input.ParentStatusRule,
	}, nil
}

type server struct {
	deps Deps
	// lifetime bounds every live socket and the loops that serve them: it is the process's own
	// context, cancelled when the server is shutting down, so a deploy closes each live socket
	// with 1012, ending every subscription it carries, and http.Server.Shutdown is not held for
	// its whole budget by a connection that never goes idle. The delivery hub and the event
	// stream relay run on it. It bounds no delivery: a send a request leaves running (a
	// broadcast's recipients) finishes across the signal, and the wait Register returns holds
	// shutdown for it (detached). Register sets it from its lifetime parameter and refuses a nil one, since nothing would
	// ever end the work it bounds; a test passes its own t.Context(). A server a test builds
	// directly has none unless the test sets it (directServer sets context.Background()).
	lifetime   context.Context
	routeIndex []routeIndexEntry
	// outside is the request pool's share admission does not hand out, the places of which the
	// work outside admission that reads the pool takes before it does (admission.Outside): a DPoP
	// decision (decideBrokerToken), a socket's heartbeat check (whileCallerResolves), a document
	// socket's admission reads (docs.Service.ServeHTTP, on the same limiter's share), a live
	// socket's upgrade where resolving its caller reads the pool and a stream's caller and run
	// before its answer (underOutsidePlace), and each poll of a stream after, which takes its
	// place without waiting and skips its tick where none is free (streamGoingAnswer). register
	// takes it from the limiter every row is admitted by; a server a test builds directly has
	// none, and a nil Outside limits nothing.
	outside *admission.Outside
	// going is the action catalogue GET /api/v1/going/catalogue serves: the dispatch tools that
	// write, held to the table this server mounts (going_catalogue.go).
	going GoingCatalogue
	// unguardedReplacements counts the document replacements stored naming no token
	// (noteUnguardedReplacement), and unguardedFileReplacements the file and picture ones
	// (noteUnguardedFileReplacement); nil, which counts nothing, where Deps.Metrics is nil.
	unguardedReplacements     *metrics.Counter
	unguardedFileReplacements *metrics.Counter
	// adviceQueryHook runs before each of a write's advice queries, on the savepoint inside the
	// write's transaction that query runs on, with the query's name. Nil outside tests.
	adviceQueryHook func(context.Context, pgx.Tx, string) error
	// suggestionTimeout overrides the allowance the related-item check runs under; zero, which
	// every server the deployment builds leaves it, means relatedCheckAllowance. Only tests set it
	// (newFailingAdviceHandler, and the check's bound tests), so a case's outcome is its own choice
	// rather than its machine's speed.
	suggestionTimeout time.Duration
	// relatedCheckEntered runs at the start of every related-item check, before any of its work.
	// Nil outside tests, and set by a test before the requests it drives, so that a test can hold
	// a write's post-commit check where production has only a slow one.
	relatedCheckEntered func()
	// meaningSearchEntered runs inside the related-item check's meaning stage, with the
	// request-pool connection that stage's read holds and before its query; nil, which every
	// server the deployment builds leaves it, calls nothing. Only a test sets it, to hold the
	// stage where production has only a slow one.
	meaningSearchEntered func()
	// relatedSearchEntered runs inside each of the related-item check's searches, with the
	// connection its rows hold and before they are read; nil, which every server the deployment
	// builds leaves it, calls nothing. Only a test sets it, to hold a stage where production has
	// only a slow query.
	relatedSearchEntered func()
	// crossProjectScanEntered runs inside the related-item check's cross-project leg, with the
	// request-pool connection that leg's read transaction holds (beginMeaningScan) and before
	// its query; nil, which every server the deployment builds leaves it, calls nothing. Only a
	// test sets it, to hold the leg with that connection taken where production has only a slow
	// query - which beforeCrossProjectRead, running before the transaction opens, cannot do.
	crossProjectScanEntered func()
	// beforeCrossProjectRead runs at the head of relatedOtherProjects, before it reads, and its
	// error stands in for the read failing. Only a test sets it: it is how a case holds that one
	// leg of the related-item check past its grace, or makes it fail, while every other leg runs
	// as it runs in production.
	beforeCrossProjectRead func(ctx context.Context) error
	// beforeCommentCommit runs on a comment write's own request, once the comment, its node and
	// every event it appended are written and before its transaction commits, with those events;
	// its error stands in for the commit failing, so the route answers it and rolls the write
	// back. Only a test sets it: it is how a case holds a write open after its events are
	// appended, to read whether any of them reaches a subscriber before the commit, and how it
	// ends one in a rollback.
	beforeCommentCommit func(ctx context.Context, events []model.Event) error
	// betweenPersonPageReads runs on a person page's request once the route has decided it
	// answers 200 and before it lists the rows (listPersonNodes), inside the transaction both
	// reads share; nil, which every server the deployment builds leaves it, runs nothing. Only a
	// test sets it: it is how a case commits a write between the decision and the list, to read
	// that the list answers the snapshot the decision read.
	betweenPersonPageReads func()
	// crossProjectWait overrides how long the related-item check waits for its cross-project leg
	// once its meaning stage has answered (awaitCrossProject); zero, which every server the
	// deployment builds leaves it, means crossProjectGrace. Only a test sets it, so a grace that
	// reaches past the meaning deadline is the test's choice rather than a race between the
	// meaning query and 300 ms of wall clock (INFRA-3904).
	crossProjectWait time.Duration
	// meaningBound overrides the meaning allowance of the related-item check
	// (relatedMeaningAllowance), which the check's own allowance still caps; zero, which every
	// server the deployment builds leaves it, means relatedMeaningAllowance. Only a test sets it,
	// so a meaning deadline no load reaches is the test's choice, and what ends a wait before it
	// can be told from that deadline without a clock (INFRA-3904).
	meaningBound time.Duration
	// searchEmbedBound overrides the bound on a query's embedding (searchEmbedTimeout), a search's
	// and a related-item check's (embedQuery); zero, which every server the deployment builds
	// leaves it, means searchEmbedTimeout. Only a test sets it, so an embedding the test holds
	// at its embedder waits for the test's release rather than for three seconds of wall clock.
	searchEmbedBound time.Duration
	// duplicateBound overrides the bound the near-copy gate's read runs under; zero, which every
	// server the deployment builds leaves it, means duplicateGateTimeout. Only a test sets it: a
	// case that reads the gate's verdict gives it a bound of its own (testServerOptions'
	// duplicateBound), and a forced create whose check does not answer a short one, so a case's
	// outcome is its own choice rather than its machine's speed.
	duplicateBound time.Duration
	// inboxStatementBound overrides the statement_timeout of the mark-all's and the dismissal's
	// transactions (boundInboxStatements); zero, which every server the deployment builds leaves
	// it, means inboxReadStatementTimeout. Only a test sets it (testServerOptions'
	// inboxStatementBound): a case that holds a dismissal behind a lock of its own while it takes
	// its steps gives the dismissal a bound past any run, so the bound Postgres keeps from the
	// moment the dismissal queues never ends a correct run however long a loaded machine takes over
	// those steps (INFRA-3904).
	inboxStatementBound time.Duration
	// beforeDuplicateRead runs at the head of the near-copy gate's read, inside its bound, and its
	// error stands in for the read failing. Only a test sets it: it is how a case holds the read
	// until the bound ends it, so the bound and nothing else decides that case.
	beforeDuplicateRead func(ctx context.Context) error
	// nearCopyRowID overrides how a near_copy_checks row gets its id; nil, which every server the
	// deployment builds leaves it, means a random uuid. Only a test sets it, to put a refused row in
	// place under the id a create will write, as a refusal whose insert landed but whose answer the
	// server never read.
	nearCopyRowID func() string
	// broadcastResolveBudget overrides the budget a broadcast's recipient resolution runs under;
	// zero, which every server the deployment builds leaves it, means broadcastResolveTimeout.
	// Only a test sets it, so a budget that ends before every selected session is read is the
	// test's choice rather than a race against five seconds of wall clock (INFRA-249).
	broadcastResolveBudget time.Duration
	// mentionLookupBound replaces mentionLookupTimeout, the bound on each try of each listener
	// lookup a comment's creation makes, for the target that lookup is of (commentLookupBound);
	// nil, which every server the deployment builds leaves it, keeps mentionLookupTimeout for every
	// target. Only a test sets it, so which of a comment's lookups that bound ends and which the
	// listener's answer ends is the test's choice rather than a race between a loaded machine and
	// 250 ms of wall clock. It is called as each lookup try starts, on that try's own goroutine,
	// just before the try's bound begins, so a test may block in it to hold the start of a lookup
	// (TestAListenerThatAnswersNoLookupDelaysARoutedCommentByOneTimeoutAtMost does).
	mentionLookupBound func(target string) time.Duration
	// priorityPatchChecked runs in patchPriority once a body naming expected has passed its check,
	// holding the locks that check took (the row's, and the order's allocation lock before it where
	// the body also moves the priority) and before the write, with the priority's id; nil, which
	// every server the deployment builds leaves it, calls nothing. Only a test sets it, to hold a
	// patch where it holds those locks while the writes that meet it queue behind them, which
	// production has only for as long as a patch's own statements take.
	priorityPatchChecked func(id string)
	// rosters is how this process reads and refreshes the stored listener answer every process's
	// issue reads judge routes against and agents lists list (issue_route_status.go). Only
	// register gives it non-zero TTLs.
	rosters rosterCache
	// listAnswers keeps what the comment list and the activity feed last answered, under the ETag
	// each answers with, so a warm read of either writes bytes instead of making them again
	// (list_answers.go). Every server register mounts has one; a server a test builds directly
	// (directServer) has none, and every method of the cache takes a nil one, which keeps
	// nothing.
	listAnswers *listAnswerCache
	// deliveryHub keeps the open Delivery and Pipeline pages current (LEGION-690); every server
	// register mounts has one, and only a server a test builds directly (directServer) has none.
	deliveryHub *deliveryHub
	// liveSockets is every open GET /ws/live socket, which the test hooks close.
	liveSockets liveSocketSet
	// liveKinds starts a live socket's subscriptions to the views beside the delivery views, by
	// kind (live_subscriptions.go); a kind it does not hold is refused.
	liveKinds map[string]liveKind
	// liveWriterBeforePick is each live socket's beforePick (live_socket.go): nil outside tests,
	// and set by a test before the socket it drives opens.
	liveWriterBeforePick func(socket *liveSocket, takesStream bool)
	// agentWatchInterval is how often an agent view re-arms its session: agentStreamWatchInterval,
	// which a test replaces.
	agentWatchInterval time.Duration
	// eventRelay is the live feed every events subscription and the delivery hub read (INFRA-66),
	// on every server register mounts: it starts its listening connection on the first of them to
	// subscribe and stops it with the last.
	eventRelay *eventStreamRelay
	// testHooks carries every test hook to every process serving the database
	// (test_hooks_fanout.go); nil without Deps.TestHooksEnabled, and on a server with no Store,
	// which shares nothing with another process and runs a hook on itself.
	testHooks *testHookFanout
	// eventHorizon is the server's one events.Horizon: the safe horizon every reader that pages
	// the event log by a global id cursor reads up to, shared through horizon() so one
	// observation in flight serves them all. horizonOnce builds it on first use rather than in
	// register, which must not touch the Store.
	horizonOnce  sync.Once
	eventHorizon *events.Horizon
	// bootMark closes once the server's boot mark has been taken (takeBootMark), which the marks
	// in eventMarks begin with; nil on a server with no Store.
	bootMark chan struct{}
	// eventMarks is the event log marks the relay's generation starts and GET /auth/whoami bound
	// the ids a writer can still hold open by (event_marks.go).
	eventMarks eventMarks
	// mentions sends comment mentions after their comment's response (comment_mention_sender.go),
	// on every server register mounts.
	mentions *mentionSender
	// detached is everything a request started that runs on past its client: every keyed write's
	// handler (keyedWrites, which runs it on a context the client's going away does not cancel) and
	// every delivery a handler started and did not wait for (a broadcast's recipients, a sweep's
	// resumed attempts). Each runs on a context the request's end does not cancel, and shutdown
	// waits for them (waitBackground) before the database pool closes, so a deploy finishes them as
	// http.Server.Shutdown finishes the requests in flight. A send bound to lifetime instead
	// was cut off by SIGTERM, which cancels that context before the HTTP drain even begins.
	detached backgroundWork
	// treeStores bounds the stores of display trees this server's reads leave running behind
	// them, and starts one store for each set of texts at a time (displayTreeReader.storeOf).
	treeStores treeStores
	// requestKeyLease, requestKeyRenewal and requestKeyRunLimit replace a keyed request's lease
	// (IdempotencyLease, a minute), how often a running handler renews it (keyedLeaseRenewal) and
	// how long a handler runs (keyedHandlerLimit); zero, which every server the deployment builds
	// leaves them, keeps those. Only a test sets them, so a handler that outlives its lease or its
	// limit is the test's choice rather than minutes of wall clock.
	requestKeyLease, requestKeyRenewal, requestKeyRunLimit time.Duration
	// transfer replaces the bounds a request's body arrives under and its answer goes out under
	// (defaultTransferBounds: no silence longer than 20 s, and 32 KiB/s after a minute's grace);
	// the zero value, which every server the deployment builds leaves it, keeps those. Only a
	// test sets it, so a body or an answer that stalls is the test's choice rather than twenty
	// seconds of wall clock.
	transfer transferBounds
	// resultsClock replaces the clock GET /api/v1/issues/{key}/results reads each moment from
	// (resultsNow); nil, which every server the deployment builds leaves it, means time.Now. Only a
	// test sets it, so a read past the span an answer is believed for is the test's choice rather
	// than a wait of that span on the wall clock.
	resultsClock func() time.Time
	// drainDeadlineSet is called with each read deadline a request's body arms on its connection
	// and the moment it armed it from, before the deadline is set on the connection
	// (drainedBody's movingDeadline.report, called from arm); nil, which every server the
	// deployment builds leaves it, calls nothing. Only a test sets it, to judge the deadline in
	// effect when a stalled read ended against the bound the body's reads chose, which no clock
	// the test reads afterwards can name.
	drainDeadlineSet func(began, deadline time.Time)
	// repeatFoundInFlight is called when the keyed layer finds a repeat's key held by a first
	// request still running, before it reads a byte of the repeat's body; nil, which every server
	// the deployment builds leaves it, calls nothing. Only a test sets it, to send the rest of a
	// body it held back once the repeat is waiting on it.
	repeatFoundInFlight func()
	// beforePublish is called when a handler is about to publish what it committed (publish),
	// before the first of its events goes out; nil, which every server the deployment builds
	// leaves it, calls nothing. Only a test sets it, to hold a write between its commit and its
	// events and watch what another writer of the same document may do meanwhile.
	beforePublish func()
	// statusChangeAfterStep is called after each step of a status change commits, with the
	// step's index in the call's plan (status_change.go); nil, which every server the deployment
	// builds leaves it, calls nothing. Only a test sets it, to make another writer's write between
	// two steps of one call.
	statusChangeAfterStep func(ctx context.Context, step int)
	// afterMentionCopyRule is called inside a comment mention's send once its copy rule answered
	// that the send waits for no earlier event, before the re-check of its claim
	// (deliverResolvedCommentMention); nil, which every server the deployment builds leaves it,
	// calls nothing. Only a test sets it, to commit a delete of the comment while the copy rule runs.
	afterMentionCopyRule func()
	// deleteLeavesSettlement has the delete of a comment whose pending insert suggestion it
	// rejects leave the document's next version to the settlement the reject arms, as the delete
	// did before it wrote that version itself (deleteComment, docs.SettleVersion); false, which
	// every server the deployment builds leaves it, settles in the delete. Only a benchmark sets
	// it, to time the delete without its settle beside the delete with it
	// (BenchmarkDeleteOfAPendingInsertSettle).
	deleteLeavesSettlement bool
	// slowWrites is the slow-write test hook's delay for each keyed method and path it was armed
	// for (POST /api/v1/writes/_test/slow), by "METHOD path"; it holds nothing on a server without
	// TestHooksEnabled, which mounts no hook.
	slowWrites sync.Map
	// slowWriteRuns is the slowWriteRecord of each route the slow-write test hook has delayed a
	// run of, by the same "METHOD path" slowWrites uses. A disarm takes the route out of
	// slowWrites and leaves its record here, so a test can disarm a route and still read what the
	// hook did before and since.
	slowWriteRuns sync.Map
	// newIssueOverride is what the new-issue test hook last set
	// (POST /api/v1/settings/new-issue/_test), which GET /api/v1/settings/new-issue answers in
	// place of the two settings Deps carries; nil until a hook call sets one, which is every
	// server without TestHooksEnabled, since that mounts no hook.
	newIssueOverride atomic.Pointer[newIssueSettings]
	// warnedUnmatchedRuns holds a warnedProductionRun for each deploy run productionApplies has
	// asked about - one with no production job or no configured apply step - so this process asks
	// delivery_run_warnings, which decides which process logs the warning (firstToWarn), once per
	// run and configuration rather than on every read. Nothing evicts an entry: the map holds one
	// per configured job names and apply step it has asked about, until the configuration is fixed
	// or the process restarts.
	warnedUnmatchedRuns sync.Map
	// termStats is the per-project term-statistics refreshes a related-item check started
	// (ensureSearchTermStats), keyed by project, so a project's many checks start one.
	termStats singleflight.Group
	// relatedRecordRetrying is how many records being retried in the background (retryRecord:
	// related_checks and near_copy_checks rows, run as detached work) hold or wait on a
	// background-pool connection right now, at most relatedRecordRetryLimit.
	relatedRecordRetrying atomic.Int32
	// branches is what the results reads last learned about each repository's default branch
	// (delivery.Branches), held across requests so the Verification tab's refetch on every
	// result.recorded does not spend one GET /repos/{owner}/{repo} apiece. It answers about
	// every branch a check is compared with, and caches the default branch alone, since what
	// a named branch holds of a commit is kept in the database (branch_commits). The policy is
	// delivery's; the server holds the one cache the process shares.
	branches delivery.Branches
	// commitPulls is the moment before which the results read will not read GitHub again for the
	// pull requests of a commit it could answer for no other way (delivery.CommitPullReads,
	// comparisonReader.readThroughHead), held across requests so one commit costs at most one such
	// read per delivery.CommitPullReadStaleAfter however often the tab refetches, and one per
	// delivery.CommitPullSettledFor once GitHub has put that pull request's head elsewhere. The
	// policy is delivery's; the server holds the one cache the process shares. A read-only
	// caller's results read takes no claim here and only reads the writers' (Refuses), so it asks
	// GitHub where a writer's read at that moment would and leaves the claims as it found them.
	commitPulls delivery.CommitPullReads
}

// warnedProductionRun keys server.warnedUnmatchedRuns: one deploy run under one configured set of
// production job names and optional apply step.
type warnedProductionRun struct {
	repo      string
	runID     int64
	jobNames  string
	applyStep string
}

// queryer is the pgx surface shared by *store.Pool and pgx.Tx, so one loader
// serves both the plain-read and in-transaction paths. A loader that reads
// several statements at once queues them in one batch on it (loadArtifactDetails,
// loadAskDetails) rather than spending a round trip on each.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults
}

// RouteMux is what Register mounts the routes on: an *http.ServeMux in a server, and in the routes
// package's TestAKeyedWriteNeverRunsUnkeyed one that also keeps the pattern of each, so the test
// reads every route the router mounts from the mounting itself rather than from a list kept beside
// it, and finds a write mounted beside the table, which runs outside keyedWrites.
type RouteMux interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Register mounts every native-workspace route on mux. Each route is declared in a file of its
// own (routeDecl, routes_table.go); the same table answers GET /api/v1.
//
// Every route is marked so the shared pool can refuse a second connection to a handler that
// already holds one of its transactions (store.ErrNestedAcquire): one caller, one connection is
// what keeps the pool from deadlocking, and a handler that breaks it fails here instead of in
// production.
//
// Every route refuses a path or query parameter holding U+0000 or a byte that is not UTF-8 before
// its handler runs (refuseUnstorableParameters), as decodeJSON refuses a U+0000 in a body.
//
// Every table route but the test hooks is admitted by Deps.Admission before its handler runs, and
// a read is bounded by a deadline too (routeHandler), so past capacity a request is refused at once
// with a retryable 429 instead of queueing without end. The websockets are mounted outside the
// table and are neither.
//
// Every POST and PATCH route takes an Idempotency-Key, and a repeat under one is answered from the
// key rather than run again (keyedWrites, request_keys.go). Every write route - a POST, PATCH, PUT
// or DELETE - that names its presser in Dispatch-Login runs only as that person
// (refuseChangedLogin).
//
// Every route outside the read class (routeDecl.Read), GET /ws/live among them, answers a read-only
// token 403 READ_ONLY_TOKEN before anything else it does, and every read but GET /api/v1/whoami
// answers one that carries a cut 403 AS_OF_TOKEN_NOT_SERVED (refuseReadOnlyToken).
//
// The server holds the delivery hub (LEGION-690), which starts its goroutines on the first
// subscription and runs them until lifetime ends, and mounts GET /ws/live beside the document
// websocket. The live socket takes a bearer as well as a cookie, so it runs under
// decideBrokerToken; the document websocket takes a cookie alone and does not. It also holds the
// event stream relay (INFRA-66), which every events subscription reads and which starts its own
// listening connection on the first of them, also on lifetime. lifetime is the process's own
// context, cancelled when the server is shutting down (server.lifetime says what else it ends).
// With Deps.TestHooksEnabled and a Store, the server also holds the test hooks' listening
// connection (test_hooks_fanout.go), started here, also on lifetime. The returned Background's
// Wait blocks until every keyed write's handler and every delivery a handler detached from its
// request (a broadcast's recipients) has settled, and until the hub, every live socket (and every
// subscription it ran), the relay and the test hooks' listener have ended. Call it only after
// lifetime has been cancelled and before the database pool closes.
//
// Register panics when lifetime is nil: the hub, the relay and the test hooks' listener would run
// on no context that ever ends, so they would outlive the process's shutdown, or the test that
// built the server, and the returned wait would block for ever once an events subscription had
// started the relay, or at once under the test hooks.
func Register(mux RouteMux, lifetime context.Context, deps Deps) Background {
	s := register(mux, lifetime, deps)
	return Background{Wait: s.waitBackground, ResumeDeliveries: s.resumeOwedDeliveries, EventHead: s.eventFeedHead, EventHorizon: s.horizon,
		PostSystemMessage: s.postSystemMessage}
}

// Background is the work a registered server does beside answering its requests, for the process
// that serves it to drive.
type Background struct {
	// Wait is the wait Register describes, for shutdown.
	Wait func()
	// ResumeDeliveries is one pass of the delivery sweep (resumeOwedDeliveries): every delivery a
	// process that died left owed is sent, once, under its own attempt number. The maintenance loop
	// runs it (RunMaintenanceSweep).
	ResumeDeliveries func(context.Context) error
	// EventHead is the event_head GET /auth/whoami answers (eventFeedHead): an id at or below which
	// every event is already final, which a page resumes its first events subscription from.
	EventHead func(context.Context) (int64, error)
	// EventHorizon answers the server's one events.Horizon (horizon(), event_stream_relay.go),
	// built on the first call: the safe horizon every reader that pages the event log by a global
	// id cursor reads up to. The suggestion outcome sweep takes it (RunSuggestionOutcomeSweep) so
	// it trails that horizon as the live event feed does, rather than observing on a Horizon of
	// its own: one per process is what lets its readers share an observation. Only a server
	// registered with a Store has an event log to answer for.
	EventHorizon func() *events.Horizon
	// PostSystemMessage posts message on its issue under its own id (model.SystemMessage), written
	// by actor, a system actor of Dispatch's own, through the one writer every issue message goes
	// through (postSystemMessage, createStoredMessage), and answers the message's id; a post made
	// again under the same id writes no second message. It runs on the background pool, as the
	// delivery sweep's resume does (withWorkStore). Dispatch's outbox posts the message that
	// says a role-routed event was recorded undelivered with it (internal/dispatch/outbox,
	// postUndeliveredNotes).
	PostSystemMessage func(ctx context.Context, actor model.Actor, message model.SystemMessage) (string, error)
}

// waitBackground waits for every keyed handler and every delivery a request left running past
// its client, then for every live socket and the subscriptions it ran, serving none that opens from
// then on (liveSocketSet.wait), then for the hub's goroutines, the relay's and the test hooks'
// listener. The detached work comes first because it is the only work lifetime does not stop: a
// keyed handler is bounded by keyedHandlerLimit, a delivery by the listener client's receipt
// timeout per recipient, and each reads and writes through the database pool this wait is holding
// open for it.
func (s *server) waitBackground() {
	s.detached.wait()
	s.liveSockets.wait()
	s.deliveryHub.wait()
	s.eventRelay.wait()
	s.mentions.wait()
	if s.testHooks != nil {
		s.testHooks.wait()
	}
}

// backgroundWork counts the work a server runs past its requests' clients, and waits for none to
// be left. Unlike a sync.WaitGroup, work may start while wait is already waiting: a request still
// in flight when shutdown stops waiting for the HTTP drain starts its keyed run then, and wait
// returns only once it sees nothing running.
type backgroundWork struct {
	mu      sync.Mutex
	running int
	// idle is closed when running last fell to zero, and replaced when work starts again.
	idle chan struct{}
}

func (b *backgroundWork) start() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running == 0 {
		b.idle = make(chan struct{})
	}
	b.running++
}

func (b *backgroundWork) done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.running--
	if b.running == 0 {
		close(b.idle)
	}
}

// goDetached runs work on a goroutine of its own, counted until it returns.
func (b *backgroundWork) goDetached(work func()) {
	b.start()
	go func() {
		defer b.done()
		work()
	}()
}

func (b *backgroundWork) wait() {
	for {
		b.mu.Lock()
		running, idle := b.running, b.idle
		b.mu.Unlock()
		if running == 0 {
			return
		}
		<-idle
	}
}

// register is Register, answering the server it mounted, whose delivery hub and event stream
// relay a test waits out before its database closes. It is also called with an empty Deps, as
// TestTheDeploymentsRelatedCheckAllowanceIsTheDocumentedBound does, so it must stay cheap and must
// not dial or read the nil Store: whatever reaches the database or the network starts later, from
// a handler, the relay's first subscriber or the hub's, or on a goroutine of its own, as the event
// log's boot mark does (takeBootMark), for a server that has a Store. The test hooks' listener is
// one of those: it needs Deps.TestHooksEnabled and a Store and dials on a goroutine of its own,
// since a process has to be listening before another process calls a hook.
func register(mux RouteMux, lifetime context.Context, deps Deps) *server {
	if lifetime == nil {
		panic("api.Register: the lifetime parameter is required: it is the context that ends the delivery hub, the event stream relay and every live socket (a test passes t.Context())")
	}
	s := &server{deps: deps, lifetime: lifetime, agentWatchInterval: agentStreamWatchInterval, listAnswers: newListAnswerCache(), rosters: rosterCache{
		ttl: cmp.Or(deps.RouteRosterTTL, routeRosterTTL), silenceTTL: routeRosterSilenceTTL,
	}}
	s.liveKinds = map[string]liveKind{
		liveEventsView: {resumes: true, start: s.startEventsView},
		liveAgentView:  {start: s.startAgentView},
	}
	s.eventRelay = newEventStreamRelay(s, lifetime)
	s.takeBootMark()
	s.mentions = s.startMentionSender()
	s.registerHeldGauges(deps.Metrics)
	s.unguardedReplacements = registerUnguardedReplacements(deps.Metrics)
	s.unguardedFileReplacements = registerUnguardedFileReplacements(deps.Metrics)
	s.deliveryHub = newDeliveryHub(s, lifetime, deliveryHubClock{})
	if deps.TestHooksEnabled && deps.Store != nil {
		s.testHooks = startTestHookFanout(s, lifetime)
	}
	routes := s.routes()
	if undecidable := s.undecidableWrites(routes); len(undecidable) > 0 {
		// A mutating row the project check cannot decide would serve a project-scoped caller a
		// write nothing judged, so the table is refused rather than mounted (project_scope.go).
		panic("api.Register: the route table holds a mutating row the project-scope check cannot decide: " + strings.Join(undecidable, "; "))
	}
	if refused := readClassRefusals(routes); len(refused) > 0 {
		// A GET route stating no class is one nobody has read for what it writes, and a route
		// that is not a GET stating one would open a write to a token minted only to read, so the
		// table is refused rather than mounted with a read-only token served or refused by
		// nothing anyone decided (routeDecl.Read).
		panic("api.Register: the route table's read class does not class every GET route exactly once: " + strings.Join(refused, "; "))
	}
	going, err := buildGoingCatalogue(routes)
	if err != nil {
		// A catalogue entry naming a row this table does not hold or does not class as a write,
		// a placeholder no argument of the tool fills, or a relation named in neither edge
		// family, is refused rather than mounted (going_catalogue.go).
		panic("api.Register: the route table holds a row the going catalogue cannot hold: " + err.Error())
	}
	s.going = going
	if shapeless := shapelessRoutes(routes); len(shapeless) > 0 {
		// A row with no shape is a row the generated OpenAPI document would leave out, so the
		// one definition of the API would stop being one; the table is refused rather than
		// mounted (routes_shapes.go).
		panic("api.Register: the route table holds a row with no request and response shape: " + strings.Join(shapeless, "; "))
	}
	s.routeIndex = routeIndexEntries(routes)
	limiter := deps.Admission
	if limiter == nil {
		limiter = admission.New(admission.DefaultLimits())
	}
	s.outside = limiter.Outside()
	deadline := deps.ReadDeadline
	if deadline == 0 {
		deadline = defaultReadDeadline
	}
	for _, route := range routes {
		mux.HandleFunc(route.Method+" "+route.Pattern, s.routeHandler(route, limiter, deadline))
	}
	if websocket, ok := deps.Docs.(interface {
		ServeHTTP(http.ResponseWriter, *http.Request)
	}); ok {
		// The document service admits a socket by its sign-in cookie alone, so its heartbeat
		// re-checks that cookie and nothing the request carries beside it (signInStillResolves).
		const pattern = "/ws/doc/{room}"
		mux.Handle("GET "+pattern, accesslog.HandlerOn(pattern, s.whileCallerResolves(s.signInStillResolves,
			trackTransactions(s.refuseUnstorableParameters(pattern, websocket.ServeHTTP))), s.clock()))
	}
	mux.Handle("GET "+liveSocketPattern, s.liveSocketHandler(s.serveLiveSocket))
	return s
}

// defaultReadDeadline bounds an ordinary read (GET, HEAD). Twenty seconds is below the thirty the
// load generator and the web app's pages wait before they give up and the sixty the dispatch
// command waits, so the server stops a read's work before its client abandons it, rather than
// finishing work nobody is waiting for while the requests behind it queue. The writing of the
// answer is bounded apart from it (servedAsRead, boundedAnswer), on the silence between its bytes
// rather than on its length, so a client slow to read does not hold what the read holds past it
// and one that keeps reading is answered in full. A write has no deadline: once admitted it runs
// to completion, since an upload takes a minute or more and an architecture import up to two per
// source, and a write cancelled partway rolls back and makes its page retry it for ever. Its
// connection acquires stay bounded by each pool's acquire bound, its wait for another writer's
// owner lock by that lock's own bound (store.LockOwner), and its body by the bounds every
// request's body arrives under (defaultTransferBounds).
const defaultReadDeadline = 20 * time.Second

// routeHandler is the chain register mounts one row of the table behind: the row's own handler
// inside every layer the row runs, in the one order every row runs them. It is the one place that
// order is written, so register and a test that builds a row's chain with the row's handler
// replaced (read_only_token_test.go, which walks the whole table so) build the same chain.
// limiter is what every row but a test hook and a stream is admitted by, and deadline what bounds
// every read but a download (Deps.Admission, Deps.ReadDeadline, register). Its layers, outermost
// first: the access log, the answers reads' timing on that route alone (timedAnswersReads), the
// compression of its answer (compressAnswers), the transaction tracking, the drain a write's early
// answer waits behind, the broker token's decision (decideBrokerToken), admission, a read's
// deadline and pinned connection, the Vary every answer
// carries (varyOnDisplayTrees), the read-only gate (refuseReadOnlyToken), the row's OAuth scopes,
// the parameter refusal every route has, the id prefixes a path carries short, the changed-login
// check, the signed-in person a write needs, a write's project and session scope, an upload's
// closed-issue refusal, and the keyed layer.
//
// Admission comes before the deadline so a request's wait for room is bounded by admission's own
// wait, and the deadline counts only the work the read was admitted to do; the pinned connection
// comes inside both, so only an admitted read takes one and it is back in the pool before the
// read's admission unit is. A refused request never reaches the keyed layer, so it claims no key -
// and the drain is outside admission so that admission's own 429 waits for the body as every other
// early answer does (drainBeforeAnswer), rather than draining it a second way. The drain is also
// where a write's body gets the bounds it arrives under (drainedBody): a write with no room to
// run in reads its body there before it waits for room (admitted), unless it presents no
// credential or its class's queue has no place for it, either of which is answered before a byte
// of the body is read, and one admitted at once parks its units at that body's first read, so a
// body a client trickles holds none of them either way. A test hook is neither admitted nor
// bounded: it is a test's own; nor is a stream admitted (apiRoute.Streams), which its reader holds
// open for the run behind it: its reads of the request pool take places of the pool's share
// admission leaves instead (underOutsidePlace, streamGoingAnswer). The broker token's
// decision is outside admission too, and writes on the request pool, so it takes a place of that
// pool's share admission leaves instead, waiting up to a second for one, and is refused
// admission's own 429 where it is refused one (decideBrokerToken, admission.Outside); it is inside
// the drain, so that 429 waits for the body as admission's own does.
func (s *server) routeHandler(route apiRoute, limiter *admission.Limiter, deadline time.Duration) http.HandlerFunc {
	handler := route.Handler
	if keyedMethod(route.Method) {
		handler = s.keyedWrites(route, handler)
	}
	if route.RefusesClosedIssue {
		handler = s.refuseUploadToAClosedIssue(handler)
	}
	if !admission.Reads(route.Method) {
		// Outside the keyed layer and the upload's spool, so a write to another project's
		// object, into another session's own rows, or by a machine's credential that names no
		// person claims no idempotency key and costs no body (withinProjectScope,
		// withinSessionScope, writeNeedsASignedInPerson).
		handler = s.withinProjectScope(route, handler)
		handler = s.withinSessionScope(route, handler)
		// Outermost of the three, so a machine's credential that names a service and no person,
		// and that no job entry and no personless account entry minted, is told it may make no
		// write at all rather than which session the write it tried belongs to. The project check
		// has nothing to tell it: such a token is held to no one project (broker_token.go). A
		// job's token names a service and no person too, under the project scope named or public,
		// and an account entry's token naming no person does under named; each passes this check
		// to the two inside it, recorded under its service. A read-only token, which may name a
		// service and no person as well, never reaches here: the read-only gate further out has
		// answered it on every write (refuseReadOnlyToken).
		handler = s.writeNeedsASignedInPerson(handler)
		handler = s.refuseChangedLogin(handler)
	}
	// Outside the two scope checks and outside the GET test, so it runs before them and
	// before every handler, on a read as on a write: the whole id it resolves a path
	// prefix into is the value the project check resolves and the value the handler
	// addresses its row by, one value for both (resolveIDPrefixes, project_scope.go).
	handler = s.resolveIDPrefixes(route, handler)
	handler = s.refuseUnstorableParameters(route.Pattern, handler)
	if len(route.OAuthScopes) > 0 {
		// Ahead of the keyed layer and the project and session checks, so the row's own
		// scopes are the first thing a request is held to once the read-only gate outside it
		// has let it through: a credential that holds none is told so rather than told its
		// write belongs to another project or another session, and its refusal claims no
		// idempotency key, so the person who ticks the scope back is not answered the stored
		// refusal under the same key. It is inside the drain, so a client that asked to be
		// told to continue is handled as it is for every other early answer, and inside
		// admission and a read's deadline and pin, since resolving the caller reads the
		// database as the changed-login check below it does: an unadmitted request runs no
		// statement, and a read's runs on its pinned connection.
		handler = s.gateRouteScopes(route.OAuthScopes, handler)
	}
	// Outside the scope gate and every layer inside it, so a read-only token on a row outside
	// the read class, or one that carries a cut on a read that answers no cut, is refused before
	// anything the row does: before the row's own scopes, which would name a scope to ask for
	// where no scope makes the row its to call; before the sign-in check, which would tell it to
	// sign a person in; and before a Dispatch-Login check, an id prefix's lookup, the project and
	// session checks and the Idempotency-Key, none of which it reaches, so it claims no key. It is
	// inside the drain, as the scope gate is, so a client that asked to be told to continue is
	// handled as for every other early answer, and inside admission and a read's deadline, since
	// the Vary outside it is: a personal token it resolves is one statement on the unit it holds,
	// and on a read's pinned connection.
	handler = s.refuseReadOnlyToken(route.Method+" "+route.Pattern, route.Read, handler)
	// Beside it, for the same reasons and at the same depth, an approve-only token on a row the
	// approve list does not name: refused before the row's scopes, which it would otherwise pass
	// with its person's scopes person and approve, and before anything the row does.
	handler = refuseApproveOnlyToken(route.ApproveOnly, handler)
	// One place for the whole table: every answer says its body depends on Dispatch-Trees
	// (varyOnDisplayTrees), so no cache between a browser and Dispatch hands a reader with trees
	// an answer written for a reader without them. It is outside the scope gate and the
	// read-only gate, so a refusal by either carries the header too, and inside admission, whose
	// refusal is no body a cache keeps. The answer's other Vary, on Accept-Encoding, is added
	// where the encoding is chosen (compressAnswers, outside this).
	handler = varyOnDisplayTrees(handler)
	if !route.TestHook {
		// What admission runs once it has admitted the request, under the context admitted
		// hands it (contextHandler): a read's bounds, which derive their own context from that
		// one, and every other row's chain run under it.
		var inside contextHandler
		if admission.Reads(route.Method) {
			// A download's work before its answer is bounded at downloadDeadline's five
			// minutes, which a test shortens (Deps.DownloadDeadline); it is also the read
			// whose deadline stops at its answer, as a stream's does: what the deadline
			// bounds there is the reads before the first frame, and the run behind the
			// stream, not this deadline, decides how long the frames take.
			if route.Download {
				deadline = cmp.Or(s.deps.DownloadDeadline, downloadDeadline)
			}
			inside = s.servedAsRead(deadline, route.Download || route.Streams, route.pinsConnection(), handler)
		} else {
			inside = withContext(handler)
		}
		// A stream takes no admission unit (apiRoute.Streams): its reader holds it open for
		// the run behind it, and a unit of the read class held for all of that would be held
		// against every other read, where the polls that fill it take a request-pool
		// connection only while each one runs. Its reads of the request pool before its answer -
		// its caller's resolution and its run's - hold one place of the pool's share admission
		// leaves instead, given back before the answer begins, and each poll after takes one
		// without waiting (underOutsidePlace, streamGoingAnswer). The live socket register
		// mounts outside the table is served the same way (liveSocketHandler).
		if route.Streams {
			handler = s.underOutsidePlace(admission.Stream, s.presentsCredential, inside.ServeHTTP)
		} else {
			handler = s.admitted(limiter, route.Weight, route.Lane, route.Auth == authPublic, inside)
		}
	}
	// Outside admission, which it comes before, and inside the drain, so its refusal for room
	// waits for a write's body as admission's own does (decideBrokerToken).
	handler = s.decideBrokerToken(handler)
	if !admission.Reads(route.Method) {
		// Every layer below may answer before the body is read - the broker token's refusal and
		// admission's included; the answer waits for the body.
		//
		// A client that asked to be told to continue before it sends its body (Expect:
		// 100-continue) and is answered early sends none of it, and its connection closes after
		// the answer, as long as nothing reads that body: net/http sends 100 Continue at the
		// body's first read, and otherwise closes the connection once the answer begins rather
		// than wait on a body that never comes. It decides from the expect-continue reader its
		// own response keeps (go1.27's response.ecReader and reqBody), not from the request's
		// Body, so it is not that routeContext hands this layer a copy of the request: the
		// handling holds with the drain's body put on net/http's own request. What keeps the body
		// unread is the drain's guard (drainedBody.askedContinue) and the keyed layer's
		// (answerDrainedRepeat), which TestAnEarlyAnswerToAClientWaitingToContinueAsksForNoBody
		// pins: without either, the client is told to continue and the answer waits on its body.
		handler = s.drainBeforeAnswer(route, handler)
	}
	if route.Method+" "+route.Pattern == statusChangeRoute {
		// Outside every layer above and outside decideBrokerToken, so it sees each answer the
		// route gives, and inside compression, so it reads the JSON a layer wrote rather than
		// gzip. The route's declared refusals carry changed: the handler and the keyed layer
		// write it, and every layer outside the keyed layer refuses before any step runs with
		// the {code, error} it answers every route, to which this adds changed []
		// (addChangedToRefusals).
		handler = addChangedToRefusals(handler)
	}
	// The access-log line is outermost, so it records what every layer answered - a keyed
	// replay, a refused login, an early answer the drain held, admission's refusal - and the
	// caller the first of them resolved. Compression sits just inside it, with nothing between
	// them but the answers read's timing below, so every one of those answers is compressed for
	// a caller that takes gzip and the line counts the bytes that went on the wire. The two
	// websockets register mounts beside the table carry no compressed answer: the upgrade is not
	// a body, and the frames after it are the socket's (compressAnswers).
	// The row's request context is made once, outside every layer of its own, with its transaction
	// tracking and its calls to GitHub tagged as the API's (routeContext).
	served := compressAnswers(routeContext(handler))
	if route.Method+" "+route.Pattern == sessionAnswersRoute && s.deps.AnswersLatency != nil {
		// Just inside the access-log line, so the time of an answers read the keyword fill
		// pauses on is the time the line records for it, every layer of the chain included.
		// Admission's wait for room is one of those layers, so a server that queues its
		// answers reads pauses the fill for that wait too. The line is timed on the server's
		// clock, which a test hands the Latency too, so the two read one clock.
		served = timedAnswersReads(s.deps.AnswersLatency, served)
	}
	return accesslog.HandlerFuncOn(route.Pattern, served, s.clock())
}

// admitted runs handler once limiter admits the request at weight, with the ticket on the
// request's context so a wait elsewhere can park it (admission.Ticket.Park), and answers a
// refusal 429 OVERLOADED. That context goes to handler beside the request rather than on a copy
// of it (contextHandler), so a read's bounds, the layer inside, copy the request once for the
// ticket's context and their own.
//
// The request takes its place in admission's arrival order first (admission.Limiter.Arrive) and
// is admitted there and then where the server has room for it. Where it has none, a write whose
// client is already sending reads its whole body off the connection before it joins the queue
// (drainedBody.readWhole) and waits holding no unit: with the body off the connection net/http's
// own read of it runs, so a client that hangs up while its write is queued cancels the request
// there and then and is recorded as the 499 it is, where a queued write with an unread body held
// its slot for its whole wait and was answered a 429 nobody read. A queue absorbing a burst serves
// the write at the place it arrived, ahead of every write that arrived while its body did, and a
// queue that has stood a whole wait serves it as its newest waiter (admission's adaptive LIFO);
// either way the wait it then has is the one wait the queue costs it, so no write is refused for
// waiting twice.
//
// Two writes with no room are answered before a byte of their body is read (readBeforeQueueing),
// since the body is read before any layer that resolves the caller runs and before the queue can
// refuse it, and an upload's is spooled to disk past its first MiB (spoolBody): while the write
// class was busy, clients that sent no credential and trickled 25 MiB bodies each held 25 MiB of
// disk to be answered 429. A write that presents no credential (presentsCredential) is refused as
// its resolution refuses it, 401, as it would be after the read; no row of the table takes a
// write from nobody - its two public rows are reads - and public keeps the read for a row that
// would. A write whose class's queue already holds its cap is refused as Admit would refuse it,
// 429 OVERLOADED with Retry-After: 1 (admission.Arrival.RefusedAtOnce). Either body is then
// drained and discarded, as every early answer's is (drainBeforeAnswer).
//
// The ticket goes to the drain's body as well as to the context: a body still arriving after its
// request was admitted parks it, so a client still sending holds none of the units
// (drainedBody.park). A body that did not arrive is answered by the drain
// (drainingWriter.refuseUnreadBody) and asks for no room at all.
//
// Such a body asks for the units again at its end, and admission may refuse it there: the
// handler is then cancelled with that refusal as its context's cause (drainedBody.resume), so
// its statements and its commit fail where they stand and nothing of the request is written,
// whether or not the handler reads the refusal from the read that ended its body. Its answer is
// the refusal, which admission.WriteUnserved writes from the cause: the client is still there,
// so this cancellation is never the 499 of a client that went away. Only a request whose body went
// through the drain makes it.
//
// A take-back of the request's own waits on other servers may be refused too (admission.GiveBack,
// admission.Away.TakeBack). Where the layer that met the refusal cannot answer it itself - a
// caller's resolution, which gives its units back while it waits on an issuer and asks for them
// again after, and an issue read's roster around its listener call (routeRosterOrUnknown) - it is
// answered here, at the response, whatever the layers between do with the error: the ticket
// records the refusal (admission.Ticket.RefusedTakeBack), the layer that met it stops the
// request's run where it stands (optionalActor, routeRosterOrUnknown, stopOnRefusedTakeBack), and
// this layer answers it 429 OVERLOADED with Retry-After: 1 (answerRefusedTakeBack). A layer that
// reads the resolution's error as no caller and goes on (refuseChangedLogin, callerHoldsScope, a
// project's scope) so never reaches its next layer, its handler or a write of its own. Every other
// network wait (awayOnNetwork) answers the refusal itself: its handler writes it through
// writeHandlerError.
//
// A request whose client went away while it waited is answered nothing, since nobody is left to
// read it, and its line carries 499 rather than the 200 of a handler that wrote nothing: under
// load a client that gives up while queued is the most frequent way a request ends unanswered.
// A heavy read takes its lane before it asks for units (admission.EnterLane, apiRoute.Lane): a
// lane is one route's own limit, and a request waiting for it holds no class unit, so the routes
// whose statements the database spends its processors on cannot take them from everything else.
// Its refusal is the same 429 OVERLOADED, naming the lane. The arrival is taken before the lane
// and passed to it, so the lane's wait is what is left of the request's one wait and the class
// queue gets the rest. A read the lane held waits in a class queue absorbing a burst at the place
// it arrived, and in one that has stood a whole wait as its newest waiter, rather than behind
// everything that arrived while it waited: before that a lane route cost up to two whole waits,
// one in the lane and a fresh one in its class (INFRA-592).
func (s *server) admitted(limiter *admission.Limiter, weight int, lane admission.Lane, public bool, handler contextHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		arrival := limiter.Arrive(admission.ClassOf(r.Method), weight)
		if lane != admission.NoLane {
			leave, err := limiter.EnterLane(r.Context(), lane, arrival)
			if err != nil {
				var refusal *admission.Refusal
				if errors.As(err, &refusal) {
					admission.WriteRefused(w, refusal)
					return
				}
				accesslog.ClientGone(r.Context())
				return
			}
			defer leave()
		}
		ticket := arrival.AdmitNow()
		if ticket == nil {
			if body, drained := r.Body.(*drainedBody); drained && !s.readBeforeQueueing(w, r, body, arrival, public) {
				return
			}
			var err error
			if ticket, err = arrival.Admit(r.Context()); err != nil {
				var refusal *admission.Refusal
				if errors.As(err, &refusal) {
					admission.WriteRefused(w, refusal)
					return
				}
				accesslog.ClientGone(r.Context())
				return
			}
		}
		defer ticket.Release()
		defer answerRefusedTakeBack(w, ticket)
		ctx := admission.WithTicket(r.Context(), ticket)
		if body, drained := r.Body.(*drainedBody); drained {
			var refused context.CancelCauseFunc
			ctx, refused = context.WithCancelCause(ctx)
			defer refused(nil)
			body.admit(ticket, refused)
		}
		handler(ctx, w, r)
	}
}

// readBeforeQueueing reads a write's body whole before the write waits for room
// (drainedBody.readWhole) and reports whether it goes on to wait, having answered it where it
// does not. A write that presents no credential, on a row that is not public, is answered as its
// resolution answers it - 401, read off the request alone (presentsCredential) - and one whose
// class's queue has no place left is answered admission's 429 (admission.Arrival.RefusedAtOnce),
// each with nothing of its body read (admitted says why).
func (s *server) readBeforeQueueing(w http.ResponseWriter, r *http.Request, body *drainedBody, arrival *admission.Arrival, public bool) bool {
	if !public && !s.presentsCredential(r) {
		if _, _, err := s.optionalActor(r); err != nil {
			s.writeAuthenticationError(w, r, err)
			return false
		}
	}
	if refusal := arrival.RefusedAtOnce(); refusal != nil {
		admission.WriteRefused(w, refusal)
		return false
	}
	return body.readWhole()
}

// contextHandler is a layer handed the context its request is to run under beside the request,
// rather than on a copy of it: each r.WithContext copies the whole http.Request. admitted hands
// its handler the ticket's context so, and a read's bounds inside it derive their own context
// from that one and copy the request once for both (servedAsRead), where admitted's copy was
// replaced at once by theirs and run under by nothing.
type contextHandler func(ctx context.Context, w http.ResponseWriter, r *http.Request)

// withContext is handler run on its request under the context it is handed.
func withContext(handler http.HandlerFunc) contextHandler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(ctx))
	}
}

// ServeHTTP runs handler under its request's own context, for a layer no admission runs
// outside: a stream's bounds (routeHandler), and a test's.
func (handler contextHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler(r.Context(), w, r)
}

// errRefusedTakeBack is what a request refused the units it gave back for a wait on another server
// stops its run with, where the layer that met the refusal cannot answer it itself: a caller's
// resolution around an issuer (optionalActor) and an issue read's roster (routeRosterOrUnknown).
// admitted answers it (stopOnRefusedTakeBack). It is never written to a client.
var errRefusedTakeBack = errors.New("the request was refused the admission units it gave back for a wait on another server")

// stopOnRefusedTakeBack stops the run of ctx's request where a layer that cannot answer a refusal
// itself - a caller's resolution around an issuer (optionalActor), an issue read's roster
// (routeRosterOrUnknown) - failed because its ticket was refused the units it gave back for that
// wait (admission.Ticket.RefusedTakeBack): it panics with errRefusedTakeBack, so no layer between
// it and admitted - one that reads the error as no caller or an unknown route and goes on, its
// handler, a write of its own - runs another line of the request, and admitted answers it
// (answerRefusedTakeBack). Both run on the request's own goroutine, so admitted's recovery is the
// one that meets it; a request admission did not count holds no ticket, and nothing is stopped.
func stopOnRefusedTakeBack(ctx context.Context) {
	if refusedTakeBack(ctx) != nil {
		panic(errRefusedTakeBack)
	}
}

// answerRefusedTakeBack is admitted's answer to a request whose take-back was refused
// (stopOnRefusedTakeBack): it recovers the stop and answers the ticket's refusal 429 OVERLOADED
// with Retry-After: 1 (admission.WriteRefused), as admission answers a request it had no room for
// before it ran; nothing else of the request was written, since each stop comes before the
// request's answer. A request whose ticket recorded no refusal costs it one load and recovers
// nothing, so every other panic goes on as it was raised, its stack whole.
func answerRefusedTakeBack(w http.ResponseWriter, ticket *admission.Ticket) {
	refusal := ticket.RefusedTakeBack()
	if refusal == nil {
		return
	}
	if raised := recover(); raised != errRefusedTakeBack {
		if raised != nil {
			panic(raised)
		}
		return
	}
	admission.WriteRefused(w, refusal)
}

// servedAsRead runs a read's handler under deadline, whose passing writeHandlerError answers 503
// REQUEST_TIMEOUT, and, where pin is set, with every request-pool statement it runs on the one
// connection its first statement takes, given back when the handler returns
// (store.PinConnection, apiRoute.pinsConnection). The answer is written under bounds of its own
// (boundedAnswer), the server's own transfer bounds, read at each request as the drain reads
// them (drainBeforeAnswer). The context it derives its own from is the one it is handed
// (contextHandler): admitted's, which carries the ticket, for every read admission counts, and
// the request's own for a stream.
//
// An ordinary read is bounded for its whole length (admission.WithDeadline): its work is the
// database's, and an answer it has begun is written from what it already read. A file version
// download (download, apiRoute.Download) and a stream (streamed, apiRoute.Streams) are bounded
// only until the answer begins (untilAnswer, admission.WithDeadlineUntilAnswer): the deadline
// bounds the work before the first byte - the download's row read and store open, the stream's
// two reads of its run - and the answer's own transfer bound decides how long the bytes that
// follow may take, since from there the read is waiting on its reader's pace, or on the run
// behind the stream, and nothing else. boundedAnswer stops it at the answer's first header or
// byte (boundedAnswer.beginsAnswer).
//
// A download tells that bound which of its own waiting the floor must not judge it on
// (boundsAnswerFloor, leaveOutOfAnswerFloor): the time its reads of the file store take is the
// store's pace, not its reader's, and the store is held to a floor of its own (storeBody).
//
// The last of an answer leaves under the bound as the rest of it did. net/http holds what the
// handler wrote last in its response buffer and the connection's and writes them after the
// handler returns, on a server that sets no WriteTimeout, so a reader that stopped just before the
// end held the goroutine and the connection until TCP gave up, and the access line read
// status=200 with every byte accounted for. The flush sends them here instead, armed as every
// other write is (boundedAnswer.sent). The compression layer, which sits outside this one, holds
// the last bytes of a gzipped answer, its final block and trailer, until the answer is over: the
// answer is ended here, before that flush (answerEnder, compressingWriter.finish), where it was
// ended once the chain had returned to the compression layer, past the bound, and its trailer went
// out after the flush with no deadline on the connection. What net/http writes after the handler
// returns - a chunked answer's terminator, and an answer net/http still measures - goes out under
// the bound too: it is armed once more as the handler returns and left on the connection, which
// net/http takes it off itself once its own last writes are done (go1.27's conn.serve sets the
// write deadline to zero after finishRequest), so a deadline left here reaches no later request on
// the connection.
//
// An answer any of whose writes failed or fell short, the flush of its last bytes included, is
// cut: the header is already on the wire, so the client cannot be told the connection is
// closing, and net/http drops it after the response without a word (shouldReuseConnection), so
// the next request a client writes onto that connection reads an EOF with no answer and leaves
// no access line of its own (INFRA-592). The handler is aborted for it,
// http.ErrAbortHandler, which logs at INFO and makes the line carry aborted=true rather than the
// status=200 of an answer that went out whole, and leaves the connection to net/http to drop.
//
// It runs after the pin is given back, and only where the handler returned normally: a flush
// can wait out the idle bound and the send buffer's drain, which the pinned request-pool
// connection would otherwise be held for, and a panic unwinding - a download's own abort -
// leaves the connection to net/http rather than flushing a connection that is already going.
// Giving the pin back twice costs nothing (store.PinConnection), so the deferred unpin still
// covers the panic.
func (s *server) servedAsRead(deadline time.Duration, untilAnswer bool, pin bool, handler http.HandlerFunc) contextHandler {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request) {
		var cancel context.CancelFunc
		var beginsAnswer func()
		if untilAnswer {
			ctx, cancel, beginsAnswer = admission.WithDeadlineUntilAnswer(ctx, deadline)
		} else {
			ctx, cancel = admission.WithDeadline(ctx, deadline)
		}
		defer cancel()
		answer := &boundedAnswer{
			ResponseWriter: w,
			movingDeadline: movingDeadline{
				control: connectionControl(w), set: (*http.ResponseController).SetWriteDeadline,
				bounds: s.transfer.orDefault(), buffered: true,
			},
			beginsAnswer: beginsAnswer,
		}
		// No lift: the handler's return arms the bound once more for net/http's own last writes
		// (below), and net/http takes it off after them.
		ctx = boundsAnswerFloor(ctx, answer)
		unpin := func() {}
		if pin {
			ctx, unpin = store.PinConnection(ctx)
			defer unpin()
		}
		handler(answer, r.WithContext(ctx))
		unpin()
		if ender, ok := w.(answerEnder); ok && answer.failure == nil {
			ender.finish()
		}
		answer.cut(answer.sent())
		if answer.failure != nil {
			slog.Info("dispatch: a read's answer was cut: its reader did not take all of it",
				"path", r.URL.Path, "written", answer.moved, "until_answer", untilAnswer, "error", answer.failure)
			panic(http.ErrAbortHandler)
		}
		answer.arm()
	}
}

// liveSocketPattern is the path of the live socket, which register mounts beside the table.
const liveSocketPattern = "/ws/live"

// liveSocketHandler is the chain register mounts GET /ws/live behind, around serve, which is
// serveLiveSocket there and a stand-in in a test that walks every mount a read-only token can
// reach (read_only_token_test.go): one place for the order, as routeHandler is for a row.
//
// The read-only gate sits just inside decideBrokerToken, whose decision it reads, and ahead of
// the upgrade. The socket is no row of the table and no read, since a socket delivers whatever
// its views carry, so the gate takes the zero entry, which classes nothing, and a read-only token
// of every kind is answered 403 READ_ONLY_TOKEN before a socket is ever opened for it. The
// approve-only gate sits beside it, and the socket is no route that states ApproveOnly, so an
// approve-only token is answered 403 APPROVE_ONLY_TOKEN there too. The socket is admitted by
// whichever credential optionalActor resolves - a bearer, a broker-issued token or a sign-in
// cookie - so its heartbeat resolves that same credential again (callerStillResolves).
//
// The upgrade's resolution of its caller is request-pool work outside admission where it reads
// that pool - a personal token's row, which the read-only gate resolves, and a sign-in cookie's
// session, which serveLiveSocket does - so it holds a place of the pool's share admission leaves
// from before the gate to just before the upgrade's answer (underOutsidePlace), and an open
// socket holds none. A restart's reconnect sends every page's socket through here at once.
func (s *server) liveSocketHandler(serve http.HandlerFunc) http.Handler {
	return accesslog.HandlerOn(liveSocketPattern, s.whileCallerResolves(s.callerStillResolves, s.decideBrokerToken(
		s.underOutsidePlace(admission.LiveSocketAdmission, s.resolutionReadsRequestPool,
			s.refuseReadOnlyToken(http.MethodGet+" "+liveSocketPattern, readEntry{},
				refuseApproveOnlyToken(false, trackTransactions(s.refuseUnstorableParameters(liveSocketPattern, serve))))))), s.clock())
}

// trackTransactions marks the request's context for the pool's nested-acquire check
// (store.WithTransactionTracking), for a mount beside the route table: the websockets.
func trackTransactions(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(store.WithTransactionTracking(r.Context())))
	}
}

// routeContext is the request context of a row of the route table, made in one copy of the
// request: transaction tracking, as trackTransactions marks it, and every call to GitHub the row
// makes tagged as the API's (GitHubCaller), as TagGitHubCalls tags it.
func routeContext(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(githubrate.WithCaller(store.WithTransactionTracking(r.Context()), GitHubCaller)))
	}
}

// GitHubCaller is the caller kind of every call to GitHub an API request makes
// (githubrate.WithCaller), which the rate reading counts the process's own calls by: the rows of
// the route table, and the GitHub proxy the routes package mounts beside them.
const GitHubCaller = "api"

// TagGitHubCalls tags the request's calls to GitHub as the API's (GitHubCaller): the handler's
// own, those of the keyed layer's run of it, which keeps the request's values past its cancel,
// and those of an architecture sync a route starts, whose flight keeps its starter's values. The
// route table's rows take it with their transaction tracking (routeContext); the routes package's
// GitHub proxy, which is no row, takes it alone.
func TagGitHubCalls(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(githubrate.WithCaller(r.Context(), GitHubCaller)))
	}
}

// whileCallerResolves serves a connection that outlives its request, a websocket, only while the
// credential it was admitted with resolves: on every StreamHeartbeat it asks stillResolves of the
// request again (checkEveryHeartbeat), and once that refuses (a logout, a membership the sign-in
// issuer no longer confirms, a revoked token) it cancels the connection's context, on which the
// document server and the live socket close the socket. stillResolves is the mount's own
// admission asked again, never a wider one: the live socket's is callerStillResolves, which
// resolves whichever credential admitted it, and the document socket's is signInStillResolves,
// the browser credential alone (a sign-in cookie, or an Identity-Aware Proxy assertion), since
// that is all the document service admits it by. A check that resolved anything the request
// carries would let a credential that admitted nothing keep the socket open once the one that did
// had ended. The check runs beside the handler, which holds pooled connections of its own while it
// admits the socket, so it marks a context of its own for the pool (store.ErrNestedAcquire), and
// it resolves on a context detached from the access-log line, which names the caller the socket
// was admitted as (accesslog.Detached).
//
// A check that fails for a transient reason keeps the socket and is asked again at the next
// heartbeat: the database did not serve its read - the pool had no connection free within its
// bound, the database stopped answering, or the connection ended under the read
// (transientCheckFailure, store.UnservedRead) - the check's own bound passed (checkCaller), or the
// request pool's share outside admission refused it a place, so it was never made
// (errHeartbeatCheckSkipped). None of them says anything of the caller. Only
// heartbeatTransientCeiling of them in a row close the socket, as a refusal does, with the last of
// them as the cause, and only a check that resolves the caller ends the run.
// Before, every failure closed the socket, so under load the pool's bounded acquire closed
// connected editors' sockets and sent them back into the load: 1,000 editors made 33,499
// handshakes in INFRA-592's stress step (tester round 38).
//
// The cancellation carries errCallerNoLongerResolves as its cause, so a handler can tell it from
// its client's own going: net/http ends a request's context with the plain context.Canceled when
// the client leaves, and the checks can fail while the client is still there - under load, the
// read of the session or the record of the person finds the pool busy (store.ErrPoolBusy) at the
// ceiling - and a socket still being admitted must then be refused, not left unanswered as a
// client gone (docs.Service.ServeHTTP). The cause changes nothing for a reader of the context's
// Err, which is context.Canceled either way.
//
// A refusal that is only the credential's own life ending (identity.ErrCredentialExpired: an IAP
// assertion, which lives about ten minutes while IAP signs a fresh one for every request it
// passes, that IAP's key set still verifies and whose person is still admitted) cancels the
// context with errCredentialExpired instead, which each socket closes with a code on which the
// page reopens it at once: the live socket with contracts.LiveSocketMaxAgeClose (liveSocket.write),
// and the document socket with the close frame ygo sends for that cause. The reopened socket's
// upgrade carries a fresh assertion, judged again as any upgrade is, so no socket is served past
// its assertion and none is cut as if its person had gone. An expired assertion that is wrong in
// any other way (a key IAP withdrew, a person no longer admitted) is a refusal like any other.
func (s *server) whileCallerResolves(stillResolves func(*http.Request) error, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, closeConnection := context.WithCancelCause(r.Context())
		defer closeConnection(nil)
		check := r.WithContext(store.WithTransactionTracking(accesslog.Detached(ctx)))
		go s.checkEveryHeartbeat(ctx, check, stillResolves, closeConnection)
		handler(w, r.WithContext(ctx))
	}
}

// checkEveryHeartbeat is whileCallerResolves' heartbeat: every StreamHeartbeat until ctx, the
// connection's, ends, it checks the caller with stillResolves (checkCaller) and closes the
// connection with closeConnection on a refusal, or on heartbeatTransientCeiling transient
// failures in a row: with errCredentialExpired for a credential that has only expired, and with
// errCallerNoLongerResolves wrapping the check's error for every other.
func (s *server) checkEveryHeartbeat(ctx context.Context, check *http.Request, stillResolves func(*http.Request) error, closeConnection context.CancelCauseFunc) {
	// The heartbeat runs on the server's clock, so a test that drives a fake decides when the
	// caller is resolved again rather than shortening the interval.
	heartbeat := s.clock().NewTimer(s.deps.StreamHeartbeat)
	defer heartbeat.Stop()
	failedInARow := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C():
			heartbeat.Reset(s.deps.StreamHeartbeat)
		}
		// The check resolves the admission's credential from a detached copy of the request,
		// which for a broker-issued token on the live socket asks the broker's answer alone rather
		// than the proof the upgrade already spent (callerStillResolves).
		err := s.checkCaller(check, stillResolves)
		switch {
		case ctx.Err() != nil:
			// The connection ended while the check ran - its client left, or its handler
			// returned - and the cause it ended with stands.
			return
		case err == nil:
			failedInARow = 0
			continue
		case transientCheckFailure(ctx, err):
			s.deps.Heartbeats.noteTransientFailure()
			failedInARow++
			if failedInARow < heartbeatTransientCeiling {
				continue
			}
			s.deps.Heartbeats.noteCloseAtCeiling()
		case errors.Is(err, identity.ErrCredentialExpired):
			// A refusal that is only the credential's own life ending: the socket closes with the
			// code on which its page reopens it at once (errCredentialExpired).
			closeConnection(errCredentialExpired)
			return
		}
		// Any other refusal, or the ceiling's transient failure.
		closeConnection(fmt.Errorf("%w: %w", errCallerNoLongerResolves, err))
		return
	}
}

// heartbeatTransientCeiling is how many heartbeat checks in a row may fail for a transient reason
// (transientCheckFailure) before the socket closes as one whose caller no longer resolves: 3, so a
// socket outlives two and closes on the third, three heartbeats, about 45 s at the 15 s heartbeat
// (defaultStreamHeartbeat).
//
// It is a ceiling because a check that fails is a check not made, and only a check that is made
// finds a sign-in revoked, a token revoked or a membership ended meanwhile. So while the database
// is busy a socket whose sign-in was revoked stays open for up to those 45 s, where a free pool
// closes it at its next heartbeat, within 15 s: that is a cost to authorization, paid so that a
// busy minute does not close every connected editor's socket and send them all to reconnect into
// it. A check the request pool's share outside admission refuses a place - its queue full, or no
// place free within the second it waits - is not made either, so it counts toward the ceiling as
// a failure does (errHeartbeatCheckSkipped): were it to leave the count as it stands, a socket
// would stay open for as long as that share stayed held, unbounded.
const heartbeatTransientCeiling = 3

// checkCaller asks stillResolves of check once, bounded at a heartbeat, so a check whose pool or
// database stops answering ends by the time the next is due rather than holding the heartbeat, and
// what the check holds, for as long as it hangs. A failure once that bound has passed is the
// bound's (errHeartbeatCheckBound), whatever the check answered.
func (s *server) checkCaller(check *http.Request, stillResolves func(*http.Request) error) error {
	ctx, cancel := context.WithTimeoutCause(check.Context(), s.deps.StreamHeartbeat, errHeartbeatCheckBound)
	defer cancel()
	err := stillResolves(check.WithContext(ctx))
	if err != nil && errors.Is(context.Cause(ctx), errHeartbeatCheckBound) {
		return fmt.Errorf("%w: %w", errHeartbeatCheckBound, err)
	}
	return err
}

// transientCheckFailure reports whether err, a heartbeat check's failure on the connection whose
// context is ctx, says nothing of the caller: the database did not serve the check's read - its
// pool had no connection free within its bound, the database stopped answering it, or its
// connection ended under it (store.UnservedRead) - the check's own bound passed
// (errHeartbeatCheckBound), or the check was never made, refused a place
// (errHeartbeatCheckSkipped). Every other failure is a refusal - a logout, a revoked or expired
// token, a membership the sign-in issuer no longer confirms, or any other error - and closes the
// socket at once.
func transientCheckFailure(ctx context.Context, err error) bool {
	return store.UnservedRead(ctx, err) || errors.Is(err, errHeartbeatCheckBound) || errors.Is(err, errHeartbeatCheckSkipped)
}

// errCallerNoLongerResolves is the cause whileCallerResolves ends a connection's context with
// when a heartbeat's re-resolution of its caller is refused, or fails for a transient reason
// heartbeatTransientCeiling times in a row (stillResolves), wrapping the last check's own error.
var errCallerNoLongerResolves = errors.New("the connection's caller did not resolve again on its heartbeat")

// errHeartbeatCheckBound is the cause a heartbeat check's context ends with when its bound, a
// heartbeat, passes (checkCaller), and what that check's failure wraps.
var errHeartbeatCheckBound = errors.New("the heartbeat's check of its caller ran out its bound")

// errHeartbeatCheckSkipped is a heartbeat check refused a place of the request pool's share
// outside admission (admission.Outside: the queue for one full, or its second's wait run out) and
// so read nothing: it was not made, which says nothing of the caller and counts toward the ceiling
// as a transient failure does. It wraps the refusal.
var errHeartbeatCheckSkipped = errors.New("the heartbeat's check was refused a place of the request pool's share outside admission, and was not made")

// takeHeartbeatPlace takes a place of the request pool's share outside admission for a heartbeat
// check that reads the pool, waiting for one as admission.Outside.Take does, for at most a second,
// well inside the check's own bound of a heartbeat (checkCaller). It answers the place, which the
// caller gives back with its Release once the check is done, and r carrying it
// (admission.WithPlace), which the check resolves its caller with: the resolution holds the place
// for its reads alone, and gives it back while the sign-in issuer renews a lapsed confirmation, or
// while IAP's key set is read to verify an IAP assertion, neither of which reads the pool, taking
// it again after, as an admitted request gives its admission units back for the same waits
// (admission.GiveBack; identity.Membership.Confirm, identity.IAPIdentity.Login). A check refused
// one is skipped (errHeartbeatCheckSkipped, wrapping the refusal), and one whose context ends while
// it waits answers that context's error, which checkCaller reads as its bound where the bound
// ended it.
func (s *server) takeHeartbeatPlace(r *http.Request) (*http.Request, *admission.Place, error) {
	place, err := s.outside.Hold(r.Context(), admission.HeartbeatCheck)
	var refusal *admission.OutsideRefusal
	if errors.As(err, &refusal) {
		return nil, nil, fmt.Errorf("%w: %w", errHeartbeatCheckSkipped, err)
	}
	if err != nil {
		return nil, nil, err
	}
	return r.WithContext(admission.WithPlace(r.Context(), place)), place, nil
}

// underOutsidePlace serves handler holding a place of the request pool's share outside admission
// for user (admission.Outside.Hold), where needs says r's work reads that pool: work admission
// does not count - a live socket's upgrade, a stream - whose reads of the request pool before its
// answer otherwise waited on the pool with nothing bounding how many did. The place rides on r's
// context (admission.WithPlace), so the caller's resolution gives it back while it waits on an
// issuer and takes it again after (admission.GiveBack; identity.Membership.Confirm,
// identity.IAPIdentity.Login), as a document socket's does. handler gives it back where its reads
// end, before its answer is written (serveLiveSocket, streamGoingAnswer), and this gives it back
// as handler returns, on every other way out.
//
// A request that finds every place held waits for one, behind the work that asked before it, for
// at most a second (Limits.OutsideWait). One refused a place - the queue for one full, or its
// second run out - reads nothing and is answered as a request the pool had no room for, 503
// DATABASE_BUSY with Retry-After, as a document socket refused a place is answered 503 with
// Retry-After before its upgrade; one whose client left while it waited is answered nothing
// (writeHandlerError). A request whose work reads no pool takes none.
func (s *server) underOutsidePlace(user admission.OutsideUser, needs func(*http.Request) bool, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !needs(r) {
			handler(w, r)
			return
		}
		place, err := s.outside.Hold(r.Context(), user)
		defer place.Release()
		if err != nil {
			s.writeHandlerError(w, r, fmt.Errorf("%w: %w", store.ErrPoolBusy, err))
			return
		}
		handler(w, r.WithContext(admission.WithPlace(r.Context(), place)))
	}
}

// HeartbeatCounts counts what the heartbeat checks of one API server's sockets met
// (checkEveryHeartbeat): the checks that failed for a transient reason, a skipped one and the one
// that closes its socket at the ceiling included, and the sockets closed at that ceiling
// (heartbeatTransientCeiling). cmd/dispatch makes one, hands it to its API server
// (Deps.Heartbeats) and reads it on its metrics endpoint, so the counts are that server's, and two
// servers in one process - a test's - count apart. A nil HeartbeatCounts counts nothing and reads
// zero.
type HeartbeatCounts struct {
	transientFailures, closesAtCeiling atomic.Uint64
}

// TransientFailures is how many of the server's sockets' heartbeat checks have failed for a
// transient reason (transientCheckFailure): the dispatch_socket_heartbeat_transient_failures_total
// counter of the metrics endpoint.
func (c *HeartbeatCounts) TransientFailures() uint64 {
	if c == nil {
		return 0
	}
	return c.transientFailures.Load()
}

// ClosesAtCeiling is how many of the server's sockets were closed for heartbeatTransientCeiling
// transient failures of their heartbeat check in a row: the
// dispatch_socket_heartbeat_ceiling_closes_total counter of the metrics endpoint.
func (c *HeartbeatCounts) ClosesAtCeiling() uint64 {
	if c == nil {
		return 0
	}
	return c.closesAtCeiling.Load()
}

// noteTransientFailure counts one heartbeat check failed for a transient reason.
func (c *HeartbeatCounts) noteTransientFailure() {
	if c != nil {
		c.transientFailures.Add(1)
	}
}

// noteCloseAtCeiling counts one socket closed at the ceiling.
func (c *HeartbeatCounts) noteCloseAtCeiling() {
	if c != nil {
		c.closesAtCeiling.Add(1)
	}
}

// errCredentialExpired is the cause whileCallerResolves cancels a socket's context with once the
// credential its upgrade carried has expired (identity.ErrCredentialExpired). ygo reads CloseCode
// off the cause and sends the document socket's peer that close frame, and the live socket reads
// the cause itself (liveSocket.write).
var errCredentialExpired error = credentialExpired{}

// credentialExpired is that cause's type: an error whose CloseCode ygo sends.
type credentialExpired struct{}

func (credentialExpired) Error() string {
	return "the credential this socket was opened with has expired; the page opens it again with a fresh one"
}

// CloseCode is the close frame ygo sends the document socket's peer: the document socket's code
// for a socket whose admission no longer stands, on which the provider reconnects and the
// reconnect is admitted again (contracts.DocumentAdmissionRevokedCloseCode).
func (credentialExpired) CloseCode() (int, string) {
	return contracts.DocumentAdmissionRevokedCloseCode, contracts.DocumentAdmissionRevokedCloseReason
}

// signInStillResolves answers whether the browser credential a document socket was admitted with
// still resolves - the sign-in cookie, or under IAP identity the upgrade's IAP assertion - which
// is what whileCallerResolves asks on that socket's every heartbeat. The document service admits a
// socket by that credential and nothing else (docs.Service.requestActor, Identity.Login), and this
// asks the API's Identity the same question; cmd/dispatch hands the API and the document service
// one Identity, so the two read one credential the same way. It reads no Authorization header. A
// bearer or a broker-issued token sent beside the cookie admitted nothing, so it keeps nothing open
// either: resolved here, it would hold the socket open for the token's whole life after the cookie
// ended, a read-only token's included, which may read a document through the API's reads and
// never through this socket.
//
// The credential's rows - the cookie's session, the person record an IAP assertion's Login
// writes - are read and written on the request pool outside admission, so the check takes a place
// of that pool's share outside admission first (takeHeartbeatPlace), and is skipped where it is
// refused one, which counts toward the heartbeat's ceiling.
func (s *server) signInStillResolves(r *http.Request) error {
	if s.deps.Identity == nil {
		return errorf(http.StatusInternalServerError, "IDENTITY_ERROR", "identity service unavailable")
	}
	check, place, err := s.takeHeartbeatPlace(r)
	if err != nil {
		return err
	}
	defer place.Release()
	_, err = s.deps.Identity.Login(check)
	return err
}

type apiError struct {
	status  int
	code    string
	message string
	// retryAfter is the Retry-After the answer carries, in whole seconds, and 0 for an answer
	// that carries none. Only a refusal that knows when the caller may come back sets it:
	// brokerTokenActor's 429, the results write's 409 CHECK_CONTENDED, both deletes' 409
	// DELETE_CONTENDED, linkedReadRefusal's 502 for a linked repository's read GitHub
	// rate-limited with a wait, and the keyed layer's 409 KEY_IN_FLIGHT (writeKeyInFlight,
	// writeKeyFreed). writeHandlerError writes the header.
	retryAfter int
	// scope is the OAuth scope a missing-scope refusal names, and is empty for every other
	// refusal. Only missingScopeError sets it; writeHandlerError writes the challenge header
	// from it, so a refusal a handler returns as an error is the same answer refuseMissingScope
	// writes directly.
	scope string
}

func (e *apiError) Error() string { return e.message }

func errorf(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

// parseQueryInt reads one query parameter as an integer within [min, max]. present is false
// when the caller did not supply the parameter at all; a parameter repeated, not an integer, or
// outside the bound is present with valid false. issue_page.go's parseIssuePage and search.go's
// handler each format their own code and message around a false valid, since the two routes
// disagree on them.
func parseQueryInt(query url.Values, field string, min, max int) (value int, present, valid bool) {
	values, ok := query[field]
	if !ok {
		return 0, false, false
	}
	parsed, err := strconv.Atoi(values[0])
	if len(values) != 1 || err != nil || parsed < min || parsed > max {
		return 0, true, false
	}
	return parsed, true, true
}

// parseQueryBool reads one query parameter as true or false, as parseQueryInt reads one as an
// integer: present is false when the request carried no value the route reads, and a value that
// is neither "true" nor "false" is present with valid false, so each route writes its own code
// and message around it. The two routes that read such a parameter read it differently, and
// both readings are carried here rather than either being changed: trim cuts the value's
// surrounding whitespace before it is read, which search's ?keywords_only= does and GET
// /api/v1/projects/{key}/related does not, and readFirst reads the parameter as url.Values.Get
// does - the first value, however many were sent, and a blank one as no value at all - which
// that related route does where search refuses a repeated or a blank one.
func parseQueryBool(query url.Values, field string, trim, readFirst bool) (value, present, valid bool) {
	values, ok := query[field]
	if !ok {
		return false, false, false
	}
	first := values[0]
	if trim {
		first = strings.TrimSpace(first)
	}
	if readFirst {
		if first == "" {
			return false, false, false
		}
	} else if len(values) != 1 {
		return false, true, false
	}
	switch first {
	case "false":
		return false, true, true
	case "true":
		return true, true, true
	}
	return false, true, false
}

// refuseChangedLogin runs a write route's handler only for the person who pressed the write, when
// the request names one in DispatchLoginHeader: the page sends the login of whoever was signed in
// at the press with every write, retries included, and another tab may sign someone else in in
// place, so a retry goes out on the new cookie before this tab knows. A request whose caller is
// another person, or no person at all (a bearer), is answered 409 LOGIN_CHANGED before anything
// runs - before its Idempotency-Key is read, so its key is untouched. A caller that resolves to
// nobody (a lapsed cookie) is passed on to the 401 written further in - the keyed layer's for a
// keyed write, the handler's for any other - so a write a session lapsed under waits for the
// sign-in that follows. A request without the header - the dispatch command, an agent, a script -
// is not checked. Logins are compared as model.CanonicalLogin compares them.
func (s *server) refuseChangedLogin(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pressed := r.Header.Values(contracts.DispatchLoginHeader)
		if len(pressed) == 0 {
			handler(w, r)
			return
		}
		actor, human, err := s.optionalActor(r)
		if err != nil {
			handler(w, r)
			return
		}
		// The caller is resolved once a request: the key layer and the handler read this.
		r = r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human}))
		login := strings.TrimSpace(pressed[0])
		if len(pressed) == 1 && login != "" && human && model.CanonicalLogin(actor.ID) == model.CanonicalLogin(login) {
			handler(w, r)
			return
		}
		writeError(w, contracts.DispatchLoginChanged, http.StatusConflict, fmt.Sprintf(
			"this write was pressed by %q and its request is not signed in as them: nothing ran; it goes only as the person who pressed it",
			login))
	}
}

// credentialKind is the credential a request's caller is resolved by (requestCredential).
type credentialKind int

const (
	// brokerTokenCredential is an access token the secrets broker issued, presented under the
	// DPoP scheme (brokerTokenActor).
	brokerTokenCredential credentialKind = iota
	// emptyBearerCredential is the bearer scheme with no token, refused 401 UNAUTHORIZED.
	emptyBearerCredential
	// sharedTokenCredential is the deployment's one shared agent token, a comparison with the
	// token it holds (auth.MatchesSharedAgentToken).
	sharedTokenCredential
	// serviceTokenCredential is a JWT-shaped bearer where service-account tokens are verified, a
	// Kubernetes pod's projected token checked against its issuer's keys (serviceTokenActor).
	serviceTokenCredential
	// personalTokenCredential is every other bearer, a personal token's row (personalTokenActor).
	personalTokenCredential
	// signInCookieCredential is no Authorization credential at all: the person Deps.Identity
	// resolves, a sign-in cookie's session.
	signInCookieCredential
)

// requestCredential names the credential optionalActor resolves r's caller by, and the bearer
// token where it is one, judged on r alone with nothing read. It is optionalActor's order, written
// once: the authentication scheme decides before anything reads a token - RFC 9449 Section 7.2
// refuses a DPoP-bound token presented under any other scheme, so a broker-issued token reaches
// Dispatch under DPoP alone and a DPoP presentation is never retried as another kind - then the
// bearer, then the sign-in. Work that must know what a resolution will read before it runs it - a
// heartbeat that takes a place of the request pool's share only for a resolution that reads the
// pool (callerStillResolves) - asks this, so a credential kind added here reaches both.
func (s *server) requestCredential(r *http.Request) (credentialKind, string) {
	if _, presented := ciphertoken.Presented(r); presented {
		return brokerTokenCredential, ""
	}
	token, present := auth.BearerToken(r)
	switch {
	case !present:
		return signInCookieCredential, ""
	case token == "":
		return emptyBearerCredential, ""
	case auth.MatchesSharedAgentToken(r, token, s.deps.AgentTokens):
		return sharedTokenCredential, token
	case s.deps.OIDC != nil && oidc.LooksLikeJWT(token):
		return serviceTokenCredential, token
	}
	return personalTokenCredential, token
}

// presentsCredential reports whether r presents a credential its caller could be resolved by at
// all, judged on r alone with nothing read: every credential requestCredential names but a blank
// bearer, and for a request with no Authorization header a browser credential the identity
// service could resolve (identity.Presents). A request that presents none is refused by its
// resolution - 401, or 500 on a server with no identity service - before that resolution reads
// anything, so work that holds something of the server's for a request before its caller is
// resolved - a write's body read before it waits for room (admitted), a stream's place of the
// request pool's share (underOutsidePlace) - asks this first, and such a request costs none of it.
func (s *server) presentsCredential(r *http.Request) bool {
	switch kind, _ := s.requestCredential(r); kind {
	case emptyBearerCredential:
		return false
	case signInCookieCredential:
		return s.presentsBrowserCredential(r)
	}
	return true
}

// resolutionReadsRequestPool reports whether resolving r's caller reads the request pool, where
// nothing has decided that caller yet: a personal token's row, and a browser credential's session,
// or the person record an IAP assertion's Login writes (readsRequestPool). A broker-issued token
// is decided before (decideBrokerToken), under a place of its own, the shared token and a
// service-account token read no pool, and a request that presents nothing to resolve reads
// nothing (presentsCredential).
func (s *server) resolutionReadsRequestPool(r *http.Request) bool {
	if _, decided := r.Context().Value(resolvedCallerKey{}).(resolvedCaller); decided {
		return false
	}
	kind, _ := s.requestCredential(r)
	switch {
	case !kind.readsRequestPool(), kind == emptyBearerCredential:
		return false
	case kind == signInCookieCredential:
		return s.presentsBrowserCredential(r)
	}
	return true
}

// presentsBrowserCredential reports whether r presents a browser credential this server's
// identity service could resolve (identity.Presents); a server with none resolves no browser.
func (s *server) presentsBrowserCredential(r *http.Request) bool {
	return s.deps.Identity != nil && identity.Presents(s.deps.Identity, r)
}

// optionalActor resolves the request's caller by the credential it presents (requestCredential),
// which the read-only gate reads too (resolvePersonalToken). Each branch names that caller on the
// request's access-log line (accesslog.Note), which costs the line what the authentication
// already resolved and never a lookup of its own. A resolution that
// waited on an issuer and was refused its admission units back after the wait does not return:
// it stops the request's run, and admitted answers it 429 OVERLOADED (stopOnRefusedTakeBack), so no
// caller of this can read the refusal as no caller and go on.
func (s *server) optionalActor(r *http.Request) (model.Actor, bool, error) {
	// The caller may already be decided: a keyed write resolves before its handler
	// (keyedWrites), and a DPoP presentation is decided once a request however many layers
	// resolve it (decideBrokerToken), which is why a refusal rides along — a proof spent on
	// the first resolution cannot answer the second.
	if caller, ok := r.Context().Value(resolvedCallerKey{}).(resolvedCaller); ok {
		return caller.actor, caller.human, caller.err
	}
	kind, token := s.requestCredential(r)
	switch kind {
	case brokerTokenCredential:
		return s.brokerTokenActor(r)
	case emptyBearerCredential:
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "UNAUTHORIZED", "invalid bearer token")
	case sharedTokenCredential:
		accesslog.Note(r.Context(), accesslog.KindSharedToken, "")
		// A credential that holds no scope, not the absence of one: the deployment's one
		// shared token is nobody's, so it acts as no person.
		return model.Actor{Credential: scopelessCredential}, false, nil
	case serviceTokenCredential:
		actor, human, err := s.serviceTokenActor(r, token)
		if err != nil {
			stopOnRefusedTakeBack(r.Context())
		}
		return actor, human, err
	case personalTokenCredential:
		actor, err := s.personalTokenActor(r.Context(), token)
		if err != nil {
			return model.Actor{}, false, err
		}
		if actor.Owner == nil {
			return model.Actor{}, false, errorf(http.StatusUnauthorized, "UNAUTHORIZED", "invalid bearer token")
		}
		accesslog.Note(r.Context(), accesslog.KindToken, *actor.Owner)
		return actor, false, nil
	}
	// What is left is signInCookieCredential: no Authorization header, or a blank one.
	if s.deps.Identity == nil {
		return model.Actor{}, false, errorf(http.StatusInternalServerError, "IDENTITY_ERROR", "identity service unavailable")
	}
	login, err := s.deps.Identity.Login(r)
	if err != nil {
		stopOnRefusedTakeBack(r.Context())
		return model.Actor{}, false, err
	}
	accesslog.Note(r.Context(), accesslog.KindUser, login)
	// A person at a browser is the person, so their cookie holds every scope and names no agent
	// that acted for them. The cookie proved the login, so the actor is Proven.
	return model.Actor{Kind: "user", ID: login, Credential: personCredential, Proven: true}, true, nil
}

// The two credentials every request of their kind shares. Each is a fact of the deployment
// rather than of one request — a browser's cookie holds every scope (model.Scopes: person,
// approve and p0), and the shared token and a service-account token hold none — and the cookie's
// is on the dashboard's hottest path, so allocating either per request would be waste. Nothing
// writes a Credential or appends to its Scopes: Holds reads, actScopes copies the slice header
// onto the act it records, and tokenScopes builds a set of its own.
var (
	scopelessCredential = &model.Credential{}
	personCredential    = &model.Credential{Scopes: model.Scopes}
)

// serviceTokenActor authenticates a JWT-shaped bearer as a Kubernetes pod's
// projected service-account token. A rejected token is a 401 of its own: it
// never falls through to the personal-token lookup, so OIDC_TOKEN_INVALID tells
// an operator the token was verified and refused rather than simply unknown. A
// token the verifier could not judge because its issuer has not been discovered
// is a 503 OIDC_ISSUER_UNAVAILABLE, never a 401: the caller retries rather than
// reads its token as refused.
//
// The verification may wait on the issuer - its discovery document while it is undiscovered, its
// key set for the first token, once the set's Lifetime is up, and for a key id the set does not
// hold - a round trip of up to ten seconds that reads no pool. The request gives its admission
// unit back for that wait, and its pinned connection with it, from the before-wait hook the
// verifier runs where it has decided to wait, before it blocks (oidc.WithBeforeWait,
// admission.GiveBack), and takes the unit back once the verification has answered
// (admission.Away.TakeBack; nothing where the hook never ran), as a browser's resolution does
// around IAP's key set (identity.IAPIdentity.Login): an issuer slow to answer then holds no unit,
// and the requests whose callers resolve from memory are not refused for want of one meanwhile. A
// take-back refused is the request's answer, admission's 429 OVERLOADED (writeHandlerError).
func (s *server) serviceTokenActor(r *http.Request, token string) (model.Actor, bool, error) {
	var away admission.Away
	verifying := oidc.WithBeforeWait(r.Context(), func() { away = admission.GiveBack(r.Context()) })
	claims, err := s.deps.OIDC.Verify(verifying, token)
	if back := away.TakeBack(r.Context()); back != nil {
		return model.Actor{}, false, back
	}
	if errors.Is(err, oidc.ErrIssuerUnavailable) {
		return model.Actor{}, false, errorf(http.StatusServiceUnavailable, "OIDC_ISSUER_UNAVAILABLE",
			"the service-account token issuer cannot be reached; try again in a minute")
	}
	if err != nil {
		reason := oidc.Reason(err)
		// The class only. The error carries the token's own unverified iss and
		// aud, which an unauthenticated caller chooses: optionalActor runs
		// before any authorization, so logging them lets anyone write what they
		// like into the operator's log on every request.
		slog.Warn("dispatch: service-account token rejected", "reason", reason)
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "OIDC_TOKEN_INVALID",
			"service-account token rejected (%s)", reason)
	}
	accesslog.Note(r.Context(), accesslog.KindService, claims.Subject)
	// A credential that holds no scope, not the absence of one: a pod's projected token proves a
	// workload and no person, so it acts as nobody.
	return model.Actor{Service: &claims.Subject, Credential: scopelessCredential}, false, nil
}

// actorFrom is the actor a write records: the caller optionalActor resolved, or, for a bearer
// that proves no session of its own, the session its body names. The request's credential stops
// here (model.Actor.Recorded): a handler stores this actor and compares it with a stored one,
// which carries none.
func (s *server) actorFrom(r *http.Request, supplied *model.Actor) (model.Actor, error) {
	actor, present, err := s.optionalActor(r)
	if err != nil {
		return model.Actor{}, err
	}
	return recordedActor(r, actor, present, supplied)
}

// recordedActor is actorFrom for a caller that optionalActor already resolved: the caller, when
// present says the caller is a person at a browser, else the session that bearerSessionActor
// resolves, with no credential.
func recordedActor(r *http.Request, caller model.Actor, present bool, supplied *model.Actor) (model.Actor, error) {
	if !present {
		var err error
		if caller, err = bearerSessionActor(r, caller, supplied); err != nil {
			return model.Actor{}, err
		}
	}
	return caller.Recorded(), nil
}

// credentialProvesSession is whether the request's credential proved a session of its own - a
// broker-issued token, which the broker binds to one session - rather than a caller that must
// name its session in the body.
func credentialProvesSession(authenticated model.Actor) bool {
	return authenticated.Kind == "session" && authenticated.ID != ""
}

// actorNamesWriter is whether a request body names a writer at all: an actor carrying a kind or
// an id, either of which is the caller saying who writes. A body that names neither leaves the
// writer to the credential.
func actorNamesWriter(supplied *model.Actor) bool {
	return supplied != nil && (strings.TrimSpace(supplied.ID) != "" || strings.TrimSpace(supplied.Kind) != "")
}

// actScopes is the scopes an act records of credential: the ones of model.Scopes, which an act's
// reader may ask about (ActorAct.Holds; DISPATCH_SCOPES and ActorActSchema in @legion/contracts).
// An act records the credential's whole set, so the act of a personal token that holds p0 records
// p0, though no reader of a recorded act asks for it: they ask for approve alone.
// legion-status, which a Legion daemon's own broker token holds and holds alone, is the
// credential's leave to write a Legion issue's status (legion_status_scope.go), not a grant an act
// carries, so such a credential's act records no scope. No credential holds it beside another
// scope (model.Credential), so every other credential's scopes are recorded whole, with no copy.
func actScopes(credential *model.Credential) []string {
	if credential.Holds(model.ScopeLegionStatus) {
		return nil
	}
	return credential.Scopes
}

// bearerSessionActor resolves the acting session for a bearer caller: the caller names its own
// session in the request body, and what the token itself proved — a personal token's owner, a
// service token's verified subject — stays attached for attribution. Neither is ever taken from
// the body, which a caller controls. The session is the caller the access-log line names, since
// it is the one a reader of the log is looking for; the token's own proof is already noted.
//
// A credential that proved a session of its own — a broker-issued token, which the broker binds
// to one session — decides it instead, so such a caller need not name itself at all. A body
// actor is then only an agreement: one that names the same session is taken, for its Origin,
// which nothing else carries; one that names any other writer is 403 ACTOR_SESSION_MISMATCH,
// naming the session the credential proved, rather than silently recording the write as a
// session the caller merely asked for. Every credential that proves no session — a cookie, a
// personal token, a Kubernetes service-account token — keeps the rule above exactly.
func bearerSessionActor(r *http.Request, authenticated model.Actor, supplied *model.Actor) (model.Actor, error) {
	if credentialProvesSession(authenticated) {
		// A body that names no writer at all leaves the session to the credential; one that names
		// anything else than that same session is the mismatch.
		named := actorNamesWriter(supplied)
		if named && (supplied.Kind != "session" || strings.TrimSpace(supplied.ID) != authenticated.ID) {
			return model.Actor{}, errorf(http.StatusForbidden, "ACTOR_SESSION_MISMATCH",
				"this request's credential acts for session %s; its actor names another writer", authenticated.ID)
		}
		// Recorded: the actor a handler writes and compares carries no credential, as a stored
		// actor decoded from JSON carries none (model.Actor.Recorded). It keeps the Proven mark
		// brokerTokenActor set, since the credential, not the body, named this session.
		actor := authenticated.Recorded()
		if supplied != nil {
			actor.Origin = supplied.Origin
		}
		// The credential itself proved this session, and for a token the broker scoped to a
		// person it proved whose session it is too (brokerTokenActor sets Owner from the token
		// and never from a body). That pair is what R14 calls "a session the server itself
		// proved theirs", and the store ties the two with an alias_of tuple on the next write
		// (graphstore.aliases). The act names the session the credential proved, exactly as
		// actingPerson names the agent that acted for a person, and says it was proved
		// (SessionProven), as every act naming a session the credential proved does; the actor's
		// kind and id are left as they were, so every stored author, every event and every reader
		// keeps the shape it has.
		//
		// A session named in a request body earns none of this: the branch below, which every
		// credential that proves no session of its own takes, records the session the caller
		// asked for, no act and no Proven mark, because an actor that merely claims a session
		// proves nothing about whose it is (R14).
		if actor.Owner != nil && *actor.Owner != "" && authenticated.Credential != nil &&
			authenticated.Credential.Session == authenticated.ID {
			actor.Act = &model.ActorAct{
				Session:       authenticated.ID,
				SessionProven: true,
				Scopes:        actScopes(authenticated.Credential),
			}
		}
		accesslog.Note(r.Context(), accesslog.KindSession, actor.ID)
		return actor, nil
	}
	if supplied == nil || supplied.Kind != "session" || strings.TrimSpace(supplied.ID) == "" {
		return model.Actor{}, errorf(http.StatusBadRequest, "ACTOR_KIND", "bearer callers require actor.kind session")
	}
	accesslog.Note(r.Context(), accesslog.KindSession, supplied.ID)
	return model.Actor{
		Kind:    "session",
		ID:      supplied.ID,
		Origin:  supplied.Origin,
		Owner:   authenticated.Owner,
		Service: authenticated.Service,
		// No Credential: this actor is one a handler records and compares, and a stored actor
		// was decoded from JSON, which carries none (model.Actor.Recorded). A layer that needs
		// the request's own credential reads it from optionalActor (callerHoldsScope).
		// Not Proven: the body named this session and the credential proved none.
	}, nil
}

// writeAuthenticationError answers a caller that did not resolve: 401 NO_IDENTITY, or 503
// SIGNIN_UNAVAILABLE for a person whose lapsed confirmation the sign-in issuer could not renew,
// as identity.WriteError renders both; anything else as writeHandlerError does. Only identity's
// own sentinels reach identity.WriteError, so a service-account token's issuer outage
// (serviceTokenActor's OIDC_ISSUER_UNAVAILABLE) never reads as the sign-in issuer's.
func (s *server) writeAuthenticationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, identity.ErrNoIdentity) || errors.Is(err, identity.ErrSignInUnavailable) {
		identity.WriteError(w, r, err)
		return
	}
	s.writeHandlerError(w, r, err)
}

func (s *server) requireAuthenticated(w http.ResponseWriter, r *http.Request) bool {
	if _, _, err := s.optionalActor(r); err != nil {
		s.writeAuthenticationError(w, r, err)
		return false
	}
	return true
}

func (s *server) requireActor(w http.ResponseWriter, r *http.Request, supplied *model.Actor) (model.Actor, bool) {
	actor, err := s.actorFrom(r, supplied)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return model.Actor{}, false
	}
	return actor, true
}

// decodeOptionalJSON decodes the request body into dst when the request carries one, so a route
// whose body is optional (a cookie caller sends none; a bearer names its session in `actor`)
// accepts both. It writes the decode error and returns false on a malformed body.
func (s *server) decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 {
		return true
	}
	if err := decodeJSON(r, dst); err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	return true
}

// codeScopeRequired refuses a caller whose credential does not hold an OAuth scope the route
// needs. What each credential holds is said once, on routeDecl.OAuthScopes.
const codeScopeRequired = "OAUTH_SCOPE_REQUIRED"

// insufficientScopeChallenge is the WWW-Authenticate header RFC 6750 section 3.1 gives a refusal
// for a scope the credential does not hold. Both ways of writing that refusal go through it, so
// a caller cannot tell them apart by the challenge.
func insufficientScopeChallenge(scope string) string {
	return fmt.Sprintf("Bearer error=%q, scope=%q", "insufficient_scope", scope)
}

// refuseMissingScope answers 403 OAUTH_SCOPE_REQUIRED for one scope the request's credential
// does not hold, under the challenge RFC 6750 section 3.1 gives that refusal. writeError sets no
// header, so the header goes on here and the body through it, which keeps the refusal the same
// shape as every other.
func refuseMissingScope(w http.ResponseWriter, scope, message string) {
	w.Header().Set("WWW-Authenticate", insufficientScopeChallenge(scope))
	writeError(w, codeScopeRequired, http.StatusForbidden, message)
}

// missingScopeError is refuseMissingScope as an error, for a check that cannot run at the gate
// because what it reads is known only once a row is loaded (answerAsk's approval asks).
// writeHandlerError writes the challenge header from the error's scope, so such a refusal is the
// gate's own answer in every part a caller can see.
func missingScopeError(scope, message string) *apiError {
	return &apiError{status: http.StatusForbidden, code: codeScopeRequired, message: message, scope: scope}
}

// gateRouteScopes is the one place a route's OAuth scopes are enforced: register wraps every row
// that declares any (routeDecl.OAuthScopes) in it, ahead of the keyed, project and session
// layers, so the declaration is what decides and a handler restates nothing. What each
// credential holds is said once, on routeDecl.OAuthScopes. The first scope the credential lacks,
// in the order the row declares them, is 403 OAUTH_SCOPE_REQUIRED (refuseMissingScope); a caller
// that does not resolve at all answers through writeAuthenticationError, as every gate on this
// server does.
//
// The caller it resolved rides along (resolvedCallerKey), so no layer behind it verifies the
// same bearer twice or presents the same DPoP proof twice.
func (s *server) gateRouteScopes(scopes []string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, human, err := s.optionalActor(r)
		if err != nil {
			s.writeAuthenticationError(w, r, err)
			return
		}
		for _, scope := range scopes {
			if !actor.Credential.Holds(scope) {
				refuseMissingScope(w, scope, fmt.Sprintf(
					"this request's credential does not hold the %s scope, which this route requires", scope))
				return
			}
		}
		handler(w, r.WithContext(context.WithValue(
			r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human})))
	}
}

// refuseReadOnlyToken is the one place a read-only credential (model.Credential.ReadOnly) is held
// to the read class: register wraps every route of the table in it, name being the route's
// "<method> <pattern>" and entry the read class its declaration states (apiRoute.Read), and
// GET /ws/live with the zero entry, which classes nothing. It reads every credential that can be read-only:
//
//   - A broker-issued token is already decided by decideBrokerToken, which runs outside it and
//     carries its decision on the context (resolvedCallerKey); verifying it again would present
//     the request's DPoP proof twice, and a proof is single use.
//   - A personal token is decided nowhere before the handler, so the gate resolves it once
//     through optionalActor and carries the result on the context, its error included, as
//     decideBrokerToken carries its own (resolvePersonalToken). It knows a personal token by the
//     classifier optionalActor resolves by (requestCredential), so it resolves every request
//     optionalActor would resolve as one. Every layer behind it then reads that result rather
//     than looking the token up again, so a token that did not resolve is answered by the layer
//     that answers it today, with the same refusal.
//   - A cookie, the deployment's shared token and a service-account JWT are never read-only, so
//     the gate resolves none of them: a cookie request costs it a context read and a look at an
//     Authorization header it does not carry, and the gate writes no people row.
//
// Then a caller whose credential is read-only (readOnlyCredential, which a results read reads the
// caller by too) is answered 403 READ_ONLY_TOKEN on every row that is not a read
// (readWritesNothing), every write among them, and on GET /ws/live, and next does not run. On a
// read, a read-only credential that carries a cut (model.Credential.Cut) is
// answered 403 AS_OF_TOKEN_NOT_SERVED, naming the cut, on every row but GET /api/v1/whoami,
// since this build answers no read at a past time and must not answer one with the present.
// Every other request runs next as it always did.
//
// Neither refusal carries a WWW-Authenticate challenge, unlike a missing scope's
// (refuseMissingScope): RFC 6750 Section 3.1 has the challenge name the scope a request needs,
// and no scope makes such a route the token's to call. Their codes are the contract a client
// reads.
func (s *server) refuseReadOnlyToken(name string, entry readEntry, next http.HandlerFunc) http.HandlerFunc {
	read := entry.Writes == readWritesNothing
	answersACut := name == whoamiRoute
	return func(w http.ResponseWriter, r *http.Request) {
		r = s.resolvePersonalToken(r)
		credential := readOnlyCredential(r.Context())
		if credential == nil {
			next(w, r)
			return
		}
		if !read {
			writeError(w, contracts.DispatchReadOnlyToken, http.StatusForbidden, readOnlyRefusal)
			return
		}
		// The cut. No read of this build answers at a past time, so every read but whoami runs
		// nothing for a token that carries a cut, rather than answer it with the present. This
		// branch is all the gate does with a cut.
		if !credential.Cut.IsZero() && !answersACut {
			writeError(w, contracts.DispatchAsOfTokenNotServed, http.StatusForbidden, fmt.Sprintf(
				"this request's credential is a read-only token cut at %s, and this build of Dispatch does not answer a read at a past time: nothing ran",
				cutText(credential.Cut)))
			return
		}
		next(w, r)
	}
}

// refuseApproveOnlyToken is the one place an approve-only token is held to the routes that state
// ApproveOnly: register wraps every row of the table in it, listed being the row's
// routeDecl.ApproveOnly, and GET /ws/live with listed false. On a route that does not state it, a
// caller decideBrokerToken resolved to an approve-only credential (model.Credential.ApproveOnly) is
// answered 403 APPROVE_ONLY_TOKEN and next does not run; every other request runs next as it always
// did. A row that states it is handed back as it is. It reads the decision decideBrokerToken
// carried and resolves nothing of its own, for the reason refuseReadOnlyToken gives, and carries no
// WWW-Authenticate challenge, since no scope makes such a route the token's to call.
func refuseApproveOnlyToken(listed bool, next http.HandlerFunc) http.HandlerFunc {
	if listed {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if approveOnlyRequest(r) {
			writeError(w, contracts.DispatchApproveOnlyToken, http.StatusForbidden, approveOnlyRefusal())
			return
		}
		next(w, r)
	}
}

// approveOnlyRequest reports whether decideBrokerToken resolved r's caller to an approve-only
// credential (model.Credential.ApproveOnly), read as readOnlyCredential reads a read-only one
// (resolvedCredential). The person actor a handler acts as (actingPerson) carries no credential,
// so a handler that holds such a token to less than its route reads it here: answerAsk, which
// answers an approval request alone for it.
func approveOnlyRequest(r *http.Request) bool {
	credential := resolvedCredential(r.Context())
	return credential != nil && credential.ApproveOnly
}

// approveOnlyRefusal is the message of every 403 APPROVE_ONLY_TOKEN refuseApproveOnlyToken answers:
// the routes that state ApproveOnly, in words, in the order routes() mounts them (by pattern and
// then method). It is built once, at the first refusal, from the declarations themselves, which
// are all in routeRegistry before any request: so it names every such route a running Dispatch
// declares, and no other.
var approveOnlyRefusal = sync.OnceValue(func() string {
	var listed []routeDecl
	for _, route := range routeRegistry {
		if route.ApproveOnly {
			listed = append(listed, route)
		}
	}
	slices.SortFunc(listed, byPatternThenMethod)
	names := make([]string, len(listed))
	for index, route := range listed {
		names[index] = route.Method + " " + route.Pattern
	}
	return "this request's credential is an approve-only token, which may call only the routes Dispatch's approve list names (" +
		strings.Join(names, ", ") + "): nothing ran"
})

// readOnlyRefusal is the message of every 403 READ_ONLY_TOKEN refuseReadOnlyToken answers: one
// text that names no route, since the read class is the table's own and a refusal that listed it
// would be stale the day a row joins it.
const readOnlyRefusal = "this request's credential is a read-only token, which may call only the GET routes Dispatch classes as reads: nothing ran"

// whoamiRoute is the one read that answers a read-only token that carries a cut: it reads the
// credential rather than Dispatch's state, and names the cut it carries (whoami).
const whoamiRoute = http.MethodGet + " /api/v1/whoami"

// cutText is a cut (model.Credential.Cut) as Dispatch writes it, in whoami's answer and in the
// refusal of a read at a past time: RFC 3339 in UTC, with every fractional digit it has.
func cutText(cut time.Time) string {
	return cut.UTC().Format(time.RFC3339Nano)
}

// resolvePersonalToken is r with its personal token resolved through optionalActor and carried on
// its context (resolvedCallerKey), its refusal included, where nothing has decided r's caller yet,
// the server has a store, and r presents a personal token as optionalActor reads it
// (requestCredential); any other r is answered as it is. optionalActor answers a later call with what
// this carried, so the token is looked up once a request, as a DPoP proof is presented once. A
// server with no store holds no personal token, so it resolves none here, and a row that takes no
// credential answers as it did before the gate rather than reach for a store it lacks.
//
// It resolves no other credential, though optionalActor would resolve any: none other can be
// read-only and undecided here, and resolving one would change the rows that take no credential
// (GET /api/v1, GET /api/v1/schema/blocks), which never resolve their caller. A sign-in is read
// from the store, and some sign-ins record their person (Deps.Identity); the shared token and a
// service-account token name their caller on the access-log line, and the second is verified.
func (s *server) resolvePersonalToken(r *http.Request) *http.Request {
	if _, decided := r.Context().Value(resolvedCallerKey{}).(resolvedCaller); decided || s.deps.Store == nil {
		return r
	}
	if kind, _ := s.requestCredential(r); kind != personalTokenCredential {
		return r
	}
	actor, human, err := s.optionalActor(r)
	return r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human, err: err}))
}

// actingPerson answers the person a request acts for, on a route gateRouteScopes has already
// held to the `person` scope. It names no scope of the route's own: the table decides those, and
// a handler that repeated them could drift from the row it serves. On a row the table gives the
// `person` scope, the refusal below is a backstop the gate makes unreachable. On the calls whose
// row declares no scope — `GET /api/v1/issues?pinned=true` (issue_queries.go), a reword of
// another session's ask (asks.go) and a follow for another session (ask_followers.go) — it is
// the only check of the scope, and those calls must not lose it.
//
// The actor it hands back is the person and not the agent beside them. Every route this opens
// reads the person from actor.ID (inbox.go, agent_tokens.go, ask_transitions.go), and three
// readers of what such a route records match on the actor being the person: the answers query
// (answerEventsQuery in asks.go), the event broker's notice (events.Broker.Notify) and the
// approval review read (docs/approval_review.go). A personal token's own actor carries no ID at
// all and a broker-issued token's is its session, so either would be recorded as somebody else.
// The person is the cookie caller's own login, and otherwise the owner the credential names,
// which is the one field every credential that holds a scope carries; the login is canonical
// (model.CanonicalLogin), the form everything that compares logins compares.
//
// The agent is not lost: Act names the credential that acted — its scopes, the personal token it
// is, the session a broker-issued token proved — so a write records which credential made it and
// not only whom it was made for. A browser's cookie names no agent, since the person themselves
// acted, and carries none. A credential that proved no session takes the one the `session` query
// parameter names where the caller gives it, the convention messages.go already reads; one that
// proved a session is refused a parameter naming another, 403 ACTOR_SESSION_MISMATCH, which is
// what bearerSessionActor answers the same disagreement in a body.
func (s *server) actingPerson(w http.ResponseWriter, r *http.Request) (model.Actor, bool) {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return model.Actor{}, false
	}
	if !actor.Credential.Holds(model.ScopePerson) {
		refuseMissingScope(w, model.ScopePerson, fmt.Sprintf(
			"this request's credential does not hold the %s scope, which this route requires", model.ScopePerson))
		return model.Actor{}, false
	}
	// Past that check the credential is not nil: Credential.Holds answers false for nothing at
	// all.
	credential := actor.Credential
	person := actor.ID
	if actor.Owner != nil {
		person = *actor.Owner
	}
	var act *model.ActorAct
	if credential.TokenID != "" || credential.Session != "" {
		session := credential.Session
		if named := strings.TrimSpace(r.URL.Query().Get("session")); named != "" {
			if session != "" && named != session {
				writeError(w, "ACTOR_SESSION_MISMATCH", http.StatusForbidden, fmt.Sprintf(
					"this request's credential acts for session %s; its session parameter names another session", session))
				return model.Actor{}, false
			}
			session = named
		}
		act = &model.ActorAct{
			Session: session,
			// Only the session the credential itself proved; one a personal token's `?session=`
			// names is the caller's word.
			SessionProven: session != "" && session == credential.Session,
			TokenID:       credential.TokenID,
			TokenName:     credential.TokenName,
			Scopes:        actScopes(credential),
		}
	}
	// Proven: every credential that holds the person scope proves its person - the cookie its
	// login, a personal or broker-issued token the owner it names.
	return model.Actor{Kind: "user", ID: model.CanonicalLogin(person), Act: act, Proven: true}, true
}

// actSessionFromBody records on actor's act the session the request body names, for the
// person-scope handlers that already decode a body actor: answerAsk, putUserState, editAsk and
// the follower routes. A
// credential that proved no session of its own - a personal token - names its agent nowhere
// else on a route whose body is already read, so the body's actor is taken for it, exactly as
// `?session=` is taken on every other route (actingPerson). A credential that did prove one - a
// broker-issued token - decides it, and a body naming another session is the disagreement
// bearerSessionActor and actingPerson both refuse, 403 ACTOR_SESSION_MISMATCH, rather than a
// write recorded as a session the caller merely asked for. The act keeps SessionProven only
// when the body names the session the credential proved; a session the body alone names is not
// proven.
//
// Only those routes read it: decodeJSON ignores a member the route's input struct does not
// declare (it logs the member at debug and decodes the rest), so a body actor on any other route
// is dropped unread and the caller's credential alone decides the act, and four of the mutating
// rows a person's scope opened are DELETEs with no body at all. A browser's actor names no agent
// (act is nil), so there is nothing for a body to name, and its body actor is ignored here as it
// always was.
func actSessionFromBody(w http.ResponseWriter, actor model.Actor, supplied *model.Actor) (model.Actor, bool) {
	named := ""
	if supplied != nil && supplied.Kind == "session" {
		named = strings.TrimSpace(supplied.ID)
	}
	if actor.Act == nil || named == "" {
		return actor, true
	}
	if actor.Act.Session != "" && actor.Act.Session != named {
		writeError(w, "ACTOR_SESSION_MISMATCH", http.StatusForbidden, fmt.Sprintf(
			"this request acts for session %s; its actor names another session", actor.Act.Session))
		return model.Actor{}, false
	}
	// A copy, so a handler holding the actor actingPerson answered keeps the act it was given.
	act := *actor.Act
	act.SessionProven = act.SessionProven && act.Session == named
	act.Session = named
	actor.Act = &act
	return actor, true
}

// callerHoldsScope reports whether this request's credential holds one OAuth scope. It is the
// question every check inside a handler asks that a person's own credential must pass as the
// person at a browser passes it: taking a live holder's claim and releasing anyone's claim
// (issue_claim.go), following or unfollowing an ask for any session (ask_followers.go),
// rewording any ask (asks.go) and reading any direct conversation (messages.go). None of them
// can read it off the actor they hold: a bearer's recorded actor is the session it acts for,
// and actorFrom drops the credential from what it hands back (model.Actor.Recorded). A caller
// that does not resolve at all holds nothing, and the route's own gate answers it.
func (s *server) callerHoldsScope(r *http.Request, scope string) bool {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		return false
	}
	return actor.Credential.Holds(scope)
}

// requireCallerScope refuses a request whose credential does not hold one scope, for a field
// that is not a route's own gate: the claim's `force` (issue_claim.go), which every
// authenticated caller may send and only a person's own credential may set. Where such a check
// passes a caller it lacks the scope for rather than refusing it, callerHoldsScope above is the
// predicate to read instead.
func (s *server) requireCallerScope(w http.ResponseWriter, r *http.Request, scope string) bool {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return false
	}
	if !actor.Credential.Holds(scope) {
		refuseMissingScope(w, scope, fmt.Sprintf(
			"this request's credential does not hold the %s scope, which this field requires", scope))
		return false
	}
	return true
}

// capExceededError names how far a text field is over its cap so one trim lands:
// "<field> is N characters over the M-character limit (L/M)". Lengths are UTF-16 units
// (itemblocks.Units16).
func capExceededError(field string, length, limit int) *apiError {
	return errorf(http.StatusBadRequest, "CAP_EXCEEDED",
		"%s is %d characters over the %d-character limit (%d/%d)", field, length-limit, limit, length, limit)
}

// countExceededError is capExceededError for item counts (options, labels).
func countExceededError(code, field string, count, limit int) *apiError {
	return errorf(http.StatusBadRequest, code,
		"%s is %d over the %d-item limit (%d/%d)", field, count-limit, limit, count, limit)
}

// blockBoundError is what a route answers an itemblocks.Check with: nil where the body broke no
// rule, else the refusal the typed error words, under the code that rule is refused by. A bound
// broken (itemblocks.CapExceeded names the block, itemblocks.CountExceeded the count) carries
// CAP_EXCEEDED, the code every cap refusal carries; a body holding a decision block
// (itemblocks.DecisionBlock) carries DECISION_BLOCK_IN_BODY, since what it is refused for is
// what it holds rather than how long it is. Every route that checks a body calls this one
// helper, so a route added later cannot word the same refusal differently.
func blockBoundError(err error) *apiError {
	if err == nil {
		return nil
	}
	var decision *itemblocks.DecisionBlock
	if errors.As(err, &decision) {
		return errorf(http.StatusBadRequest, "DECISION_BLOCK_IN_BODY", "%s", err)
	}
	return errorf(http.StatusBadRequest, "CAP_EXCEEDED", "%s", err)
}

func capExceeded(w http.ResponseWriter, field string, length, limit int) {
	err := capExceededError(field, length, limit)
	writeError(w, err.code, err.status, err.message)
}

// requireUUIDPath rejects a `{id}` path value that is no id with 400 <KIND>_ID_INPUT before it
// reaches a uuid column (where pgx would surface it as a 500). kind is "ask", "comment", or
// "message".
//
// The value it reads is the one the prefix layer left (resolveIDPrefixes): an id written as a
// unique prefix is already the whole id by the time this runs, and what reaches here is an id
// nothing carries or a value that is no id at all. So the refusal states the rule a caller can
// act on - a whole uuid, or a prefix of it long enough to name one item - rather than telling
// them to write the whole id, which is no longer what the route needs.
//
// A value uuid.Parse reads in one of its other spellings reaches here already rewritten to the
// canonical one: the prefix layer canonicalizes the {id} of every route that takes one
// (canonicalizeUUIDPath), so this parse only decides whether the value is an id at all.
func requireUUIDPath(w http.ResponseWriter, r *http.Request, kind string) bool {
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		writeError(w, strings.ToUpper(kind)+"_ID_INPUT", http.StatusBadRequest, idPathProblem(kind))
		return false
	}
	return true
}

// idPathProblem is how a route refuses a path id that names nothing, worded once so every route
// that reads one says the same thing. An ask's, a comment's and a message's id is resolved from
// a prefix before a handler sees it (routeDecl.ID), so their rule is a whole uuid or a prefix of
// one long enough to name a single item. A handoff's and a thread's is not, so theirs is the
// whole uuid it has always been.
func idPathProblem(kind string) string {
	switch kind {
	case "ask", "comment", "message":
		return kind + " id must be a whole uuid or a unique prefix of 8 characters or more"
	}
	return kind + " id must be a full uuid"
}

const maxJSONRequestBytes int64 = 1 << 20

type maxBytesDiscarder struct{}

func (maxBytesDiscarder) Header() http.Header             { return nil }
func (maxBytesDiscarder) Write(value []byte) (int, error) { return len(value), nil }
func (maxBytesDiscarder) WriteHeader(int)                 {}

// actorInput is the body of every route whose only input is who acts.
type actorInput struct {
	Actor *model.Actor `json:"actor"`
}

// decodeJSON decodes r's body, one JSON value of at most maxJSONRequestBytes, into value. A member
// value does not declare is ignored, because two builds of Dispatch serve one database while a
// release rolls out, and a body a newer build's client sends can reach the older build. A member
// whose loss would change what a request does therefore ships as a new route, never as a member.
// The ignored top-level members are logged at debug with the route (logUndeclaredMembers); the
// test hooks, which no other build calls, refuse them instead (decodeJSONTestHook, and
// refuseUndeclaredMembers for a hook that reads its body itself). A string the body holds
// anywhere that carries U+0000 is refused, naming where it stands (unstorableJSON).
func decodeJSON(r *http.Request, value any) error {
	return decodeJSONWithin(r, value, maxJSONRequestBytes, errJSONTooLarge)
}

// decodeJSONWithin is decodeJSON for a route whose body has a limit of its own
// (apiRoute.bodyLimit): a body
// past limit bytes is tooLarge, the 413 that route answers one with.
func decodeJSONWithin(r *http.Request, value any, limit int64, tooLarge *apiError) error {
	body, err := readJSONBody(r, value, limit, tooLarge)
	if err != nil {
		return err
	}
	logUndeclaredMembers(r, value, body)
	return nil
}

// decodeJSONBody is decodeJSON for a route that reads its body's bytes again for what the decoded
// value cannot say, such as whether a member was there at all (replaceFieldsRefusal); it answers
// them.
func decodeJSONBody(r *http.Request, value any) ([]byte, error) {
	body, err := readJSONBody(r, value, maxJSONRequestBytes, errJSONTooLarge)
	if err != nil {
		return nil, err
	}
	logUndeclaredMembers(r, value, body)
	return body, nil
}

// decodeJSONTestHook is decodeJSON for a route this tree alone calls: the test hooks, mounted only
// under Deps.TestHooksEnabled. No page or command of another build reaches one, so a member the
// hook does not declare is a mistake in the test that sent it, and is refused by name rather than
// ignored.
func decodeJSONTestHook(r *http.Request, value any) error {
	body, err := readJSONBody(r, value, maxJSONRequestBytes, errJSONTooLarge)
	if err != nil {
		return err
	}
	return refuseUndeclaredMembers(value, body)
}

// refuseUndeclaredMembers is decodeJSONTestHook's refusal on its own, for a test hook that reads
// its body itself and decodes it after: it answers the error naming the top-level members of body
// that value's type does not declare, and nil when it declares them all.
func refuseUndeclaredMembers(value any, body []byte) error {
	if undeclared := undeclaredMembers(value, body); len(undeclared) > 0 {
		return errorf(http.StatusBadRequest, "INVALID_JSON", "the hook does not take: %s", strings.Join(undeclared, ", "))
	}
	return nil
}

// readJSONBody decodes r's body, at most limit bytes of it, into value and returns the bytes it
// read, for the caller to judge the members value does not declare by. A body past limit is
// tooLarge, the 413 the route answers one with. A string the body holds anywhere that carries
// U+0000 is refused (unstorableJSON).
func readJSONBody(r *http.Request, value any, limit int64, tooLarge *apiError) ([]byte, error) {
	body, err := readJSONBodyAllowingNUL(r, value, limit, tooLarge)
	if err != nil {
		return nil, err
	}
	if refusal := unstorableJSON("", body); refusal != nil {
		return nil, refusal
	}
	return body, nil
}

// readJSONBodyAllowingNUL is readJSONBody without its refusal of U+0000, for the two routes that
// check what their body carries as another route checks it, under that route's name for it: the
// markdown round trips (decodeRoundTrip), the inline one refusing its text's U+0000 as the upload
// refuses its content's (postMarkdownRoundTrip) and the file one its bytes' as the upload refuses
// its file part's (documentTextRefusal).
func readJSONBodyAllowingNUL(r *http.Request, value any, limit int64, tooLarge *apiError) ([]byte, error) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && !strings.HasSuffix(contentType, "+json")) {
		return nil, errorf(http.StatusUnsupportedMediaType, "JSON_CONTENT_TYPE", "JSON mutations require Content-Type application/json")
	}
	r.Body = http.MaxBytesReader(maxBytesDiscarder{}, r.Body, limit)
	var read bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(r.Body, &read))
	if err := decoder.Decode(value); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return nil, tooLarge
		}
		return nil, errorf(http.StatusBadRequest, "INVALID_JSON", "invalid JSON body: %v", err)
	}
	if decoder.More() {
		return nil, errorf(http.StatusBadRequest, "INVALID_JSON", "request body must contain one JSON value")
	}
	return read.Bytes(), nil
}

// WriteJSON writes body as the JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Warn("dispatch: write json failed", "error", err)
	}
}

// errorBody is the body of every refusal writeError answers, on every route.
type errorBody struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, code string, status int, message string) {
	WriteJSON(w, status, errorBody{Code: code, Error: message})
}

func encodeJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode JSON: %w", err)
	}
	return data, nil
}

func (s *server) appendEvent(ctx context.Context, tx pgx.Tx, event model.Event) (model.Event, error) {
	return s.deps.Events.Append(ctx, tx, event)
}

// appendCausingEvent is appendEvent for an event that causes a write of the node and tuple store
// made before it existed (graphstore.Uncaused): the event's append and the append of what that
// write stated under it, in one batch, the second naming the event's id as the id sequence left it
// (graphstore.LatestEvent). A route that writes as it goes pays one statement for the two, as
// it paid for the event alone.
func (s *server) appendCausingEvent(ctx context.Context, tx pgx.Tx, event model.Event, stated graphstore.Uncaused) (model.Event, error) {
	var appended model.Event
	err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		if err := s.deps.Events.QueueAppend(b, event, func(written model.Event) { appended = written }); err != nil {
			return err
		}
		graphstore.QueueCaused(b, stated, graphstore.LatestEvent)
		return nil
	})
	return appended, err
}

// publish fans out what a handler committed, after its transaction committed and while it still
// holds whatever it took to write - a document's writer gate among them, which ends only once
// these events are out (editArtifact, storeArtifact) - and stores the display trees of the
// stored texts their payloads carry (storeEventDisplayTrees). ctx is the caller's own: a route's
// request context, which by here has committed and holds no connection of the pool, or a
// background writer's. The fan-out is what the gate waits for; the store behind it is not, and
// stands back until the gate is released (docs.AfterWriterGateRelease, storeDisplayTrees). A
// caller that publishes while its transaction is still open stores nothing, and loses no tree by
// it: such an event carries a text its own write already stored.
func (s *server) publish(ctx context.Context, events ...model.Event) {
	if s.beforePublish != nil {
		s.beforePublish()
	}
	for _, event := range events {
		s.deps.Events.Publish(event)
	}
	s.storeEventDisplayTrees(ctx, events)
}

// clock is the clock the server's timers run on: Deps.Clock, clock.Real() where that is nil, for
// a server Register builds and for one built directly alike, so no server literal sets a clock.
func (s *server) clock() clock.Clock {
	if s.deps.Clock != nil {
		return s.deps.Clock
	}
	return clock.Real()
}

// begin opens the transaction of a request: every transaction an API handler opens comes from
// here, so requestTx is what each of them commits and rolls back. It opens it on the store ctx
// names (workStore): the request pool for every request, and the background pool for the
// background work that runs through this server (resumeOwedDeliveries).
func (s *server) begin(ctx context.Context) (pgx.Tx, error) {
	database := s.workStore(ctx)
	if database == nil || database.Pool == nil {
		return nil, errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	return requestTx{Tx: tx}, nil
}

type workStoreKey struct{}

// withWorkStore names database as the store every statement this server makes under ctx runs
// on, in place of its own (workStore). The server's own store is the request pool's, which is
// right for a request and wrong for background work: a loop no client waits on, running through
// the server that answers the requests, names the background pool's store at its entry, so the
// whole path below it - its reads, its transactions (begin) and the display trees its events
// carry (storeDisplayTrees) - takes no connection admission counts for requests. The delivery
// sweep's resume is that loop (resumeOwedDeliveries).
func withWorkStore(ctx context.Context, database *store.Store) context.Context {
	return context.WithValue(ctx, workStoreKey{}, database)
}

// workStore is the store a statement this server makes under ctx runs on: the one ctx names
// (withWorkStore), else the server's own.
func (s *server) workStore(ctx context.Context) *store.Store {
	if database, named := ctx.Value(workStoreKey{}).(*store.Store); named {
		return database
	}
	return s.deps.Store
}

// requestTx is a request's transaction (server.begin): the pool's transaction, which rolls back
// apart from its caller's cancellation already (store.RollbackContext) and commits apart from it
// (store.CommitContext), with one rule of the request's own - no commit is started for a request
// that has already ended.
//
// The commit itself must not be cut mid-way, since its outcome would then be unknown and every
// room the transaction wrote would be failed; but a request whose client went away, or whose own
// bound passed, before its commit began has nothing to protect. Started, that commit would hold
// the rows it locked, its documents' writer gates and the event horizon for up to ten more
// seconds over an answer nobody will read. Refused, the handler's deferred rollback ends the
// transaction at once and writeHandlerError answers the request as the client-gone it is
// (store.ErrRequestEnded).
//
// A document write commits through its ledger, which refuses the same request before it records
// anything of its own (docs.Ledger.Commit): the check here is for the transactions a handler
// commits itself.
type requestTx struct {
	pgx.Tx
}

func (tx requestTx) Commit(ctx context.Context) error {
	if err := store.CommitRefusal(ctx); err != nil {
		return err
	}
	return tx.Tx.Commit(ctx)
}

// readyPool is the pool a route reads through before it opens its transaction, with the
// refusal begin would make when there is no database.
func (s *server) readyPool() (*store.Pool, error) {
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return nil, errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	return s.deps.Store.Pool, nil
}

// lockIssue takes the issue's owner lock and returns its lifecycle status, closed or not. It is
// the lock requireOpenOwnerStatus takes, without the closed refusal, for the writes a closed
// issue still takes: a message, its deliveries and the agent's reply to one. The read is the
// statement after the lock, sent with it, so it sees a close the lock's previous holder
// committed while this one waited (store.LockOwnerAndRead).
func (s *server) lockIssue(ctx context.Context, tx pgx.Tx, key string) (string, error) {
	status, _, err := issueStatus(ctx, key, lockedOwnerRead(tx))
	return status, err
}

// issueStatusRead reads an issue's lifecycle status and whether it is open.
const issueStatusRead = `select status, closed_at is null from issues where key = $1`

// issueStatus is that read made through read: under the issue's owner lock for the check that
// decides (lockIssue, ownerStatus), and through the pool for the check a comment makes before
// its transaction opens, which reads exactly what the locked check will. It refuses nothing:
// each write that a closed issue refuses decides that from open.
func issueStatus(ctx context.Context, key string, read ownerRead) (string, bool, error) {
	var status string
	var open bool
	if err := read(ctx, store.IssueOwner(key), issueStatusRead, []any{key}, &status, &open); err != nil {
		return "", false, err
	}
	return status, open, nil
}

// closedWrite says, of one write that takes the owner's lock through ownerStatus, whether a
// closed issue takes it. A closed issue still takes a comment that is not a suggestion, a
// message, their deliveries and an agent's reply to one (takenWhenClosed); every other write that
// asks here -- an ask, a suggestion, an upload, a document edit and a named version (before the
// document service is called), a handoff, a review, an action on an existing comment -- is
// refusedWhenClosed. The writes that read the issue row for themselves never reach this and
// refuse on their own: a claim (issue_claim.go), a PATCH outside a closed issue's allow-list
// (issue_patch.go), a check result, which a closed issue takes for an after_merge or
// default_branch check (results.go), and a document rebuild with supplied markdown
// (artifact_rebuild.go), which the document service refuses with docs.ErrIssueClosed
// (docs/rebuild.go; writeHandlerError turns it into the same 409 ISSUE_CLOSED).
type closedWrite bool

const (
	takenWhenClosed   closedWrite = false
	refusedWhenClosed closedWrite = true
)

// errIssueClosed is the refusal of a write that a closed issue does not take.
var errIssueClosed error = errorf(http.StatusConflict, "ISSUE_CLOSED", "issue is closed")

// refuseClosed is the one place the writes that take the owner's lock through ownerStatus refuse
// a closed owner: the locked checks (requireOpenOwnerStatus, lockCommentOwner, lockedComment) and
// the pooled pre-check a comment with mentions and an upload make before their transaction opens
// (refuseOwnerBeforeResolving) all decide here. It is not every closed-issue refusal the API
// makes: closedWrite names the writes that decide for themselves. open is what the owner's row
// read (ownerStatus, issueStatus): an issue is open until it closed, and a project document
// always is.
func refuseClosed(open bool, write closedWrite) error {
	if write == refusedWhenClosed && !open {
		return errIssueClosed
	}
	return nil
}

// A version event is built by docs.ArtifactVersionEventPayload wherever it is appended; a
// document's creation is the one other artifact payload, and it states what the write moved
// through the same helper, so both shapes spell the reference keys once.
func artifactCreatedEventPayload(artifact model.Artifact, changes model.ReferenceChanges) map[string]any {
	payload := map[string]any{"artifact": artifact}
	model.NameReferenceChanges(payload, changes)
	return payload
}

// namedVersionDiff is the diff a named version's event carries: from the version before it to it.
// Either may be the version this transaction has just queued (ledger), which the table holds only
// once Commit has run its insert, so each is taken from the ledger when it is that version, and
// read from the table otherwise (transactionVersionMarkdown).
func (s *server) namedVersionDiff(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, artifactID string, version model.Version) (*string, error) {
	if !version.Named || version.Number < 2 {
		return nil, nil
	}
	previous, found, err := transactionVersionMarkdown(ctx, tx, ledger, artifactID, version.Number-1)
	if err != nil {
		return nil, fmt.Errorf("load previous artifact version: %w", err)
	}
	if !found {
		return nil, nil
	}
	current, found, err := transactionVersionMarkdown(ctx, tx, ledger, artifactID, version.Number)
	if err == nil && !found {
		err = pgx.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("load current artifact version: %w", err)
	}
	diff := text.UnifiedDiff(previous, current)
	return &diff, nil
}

// transactionVersionMarkdown is the markdown of version number of artifactID as tx sees it once its
// queue has run: the ledger's, when it is the version this transaction queued
// (docs.Ledger.PendingVersionMarkdown), else the stored row's. False when neither holds it.
func transactionVersionMarkdown(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, artifactID string, number int) (string, bool, error) {
	if markdown, pending := ledger.PendingVersionMarkdown(artifactID, number); pending {
		return markdown, true, nil
	}
	var markdown string
	err := tx.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, number).Scan(&markdown)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return markdown, err == nil, err
}
