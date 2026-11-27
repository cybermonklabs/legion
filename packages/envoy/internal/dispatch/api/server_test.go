package api

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"
	"go.uber.org/goleak"

	brokerstoretest "github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/api/apitest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/horizontest"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testwait"
)

// apiTestParallelism is how many of this package's tests run at once when the command line names
// no -parallel. Most tests of the package call t.Parallel; those that change or measure something
// the whole process shares (the default logger, the environment, a package variable, heap
// statistics, the xid rig's GOMAXPROCS and collector) or hold a wall-clock bound stay serial and
// run before the rest. Alone, a database-backed test spends most of its time waiting on Postgres,
// not on a core, so go test's default of GOMAXPROCS (four on CI's arm runner) leaves the runner
// idle, and on a dev box with hundreds of cores it would open more connections than the test
// server allows: each test has a database and pools of its own (storetest.Open), and
// testlimits.ServerConnections sizes the server for a few at once.
//
// Eight, not sixteen: at sixteen on the 4-vCPU race runner, each of three runs failed a different
// test that holds a wall-clock bound (a 500 ms answer, a cold document read answering 503, a
// meaning search's candidates), with no data race and the service at 159 of its 483 connections
// (envoy-go-race-api jobs 137359069610, 137391716843 and 137399725401). The cores, not the
// connections, were what ran out.
//
// go test parses its flags inside m.Run, so a -parallel on the command line still wins.
const apiTestParallelism = 8

// TestMain sets the package's default -parallel (apiTestParallelism) and runs the package's tests
// through the Dispatch harness's Main, which drops their databases and their templates, inside the
// broker harness's Run, which then drops the database brokerstoretest.URL made the contract rows'
// broker schemas in. Both waits end at the bound testdb.TeardownDeadline gives the test binary, and
// the broker's, which begins where the Dispatch harness's may have reached that bound, still waits
// testdb's floor for its own drop. Then it fails the run where a goroutine a test started outlives
// it (goleak): the agent's loop, which the background-run checks of this package run in the test
// process, among them.
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(apiTestParallelism)); err != nil {
		panic("set this package's default -parallel: " + err.Error())
	}
	goleak.VerifyTestMain(testRun(func() int {
		return brokerstoretest.Run(func() int { return storetest.Main(m) })
	}))
}

// testRun is a run of the package's tests as goleak.VerifyTestMain makes one.
type testRun func() int

func (run testRun) Run() int { return run() }

// sharedAgentTokens is a DISPATCH_AGENT_TOKEN setting parsed as the server parses it at boot.
func sharedAgentTokens(t testing.TB, setting string) *auth.SharedAgentTokens {
	t.Helper()
	tokens, err := auth.ParseSharedAgentTokens(setting)
	if err != nil {
		t.Fatalf("ParseSharedAgentTokens(%q): %v", setting, err)
	}
	return tokens
}

// withDocumentService is input with a document service of the test's own, for a test that needs
// none of its own making: one over the input's store, event broker, identity and server URL,
// shut down once the test ends, after every cleanup registered later (a server's wait among
// them). An input with no broker gets one, which the API and the service then share.
func withDocumentService(t testing.TB, input DepsInput) DepsInput {
	t.Helper()
	if input.Events == nil {
		input.Events = events.NewBroker()
	}
	documents := docs.New(docs.Deps{Store: input.Store, Events: input.Events, Identity: input.Identity, ServerURL: input.ServerURL})
	t.Cleanup(func() {
		if err := documents.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the document service down: %v", err)
		}
	})
	input.Docs = documents
	return input
}

// NewDeps builds no document service of its own: a service it built would have no holder to shut
// it down, and its compaction worker would outlive every server it served. An input with none is
// refused, naming the field.
func TestNewDepsRefusesAnInputWithNoDocumentService(t *testing.T) {
	t.Parallel()
	if _, err := NewDeps(DepsInput{}); err == nil || !strings.Contains(err.Error(), "Docs is required") {
		t.Fatalf("an input with no document service = %v, want it refused naming Docs", err)
	}
}

// testServerOptions configure the server an API test drives.
type testServerOptions struct {
	// newIssueLabel and newIssueSession are the two settings the dashboard's search bar reads
	// before it files a person's typed words as an issue (GET /api/v1/settings/new-issue); both
	// empty is the deployment that configured neither.
	newIssueLabel   string
	newIssueSession string
	testHooks       bool
	// store is the database the server runs on; nil opens one of its own (storetest.Open). A
	// test that counts a read's round trips passes a traced store (storetest.Traced).
	store *store.Store
	// renames is the rename pairs the server is given, as a boot gives its own (Deps.Renames).
	// A test that seeds a pair by SQL reads them from its store after the seed (readRenames) and
	// passes that store too. The zero value holds no pairs.
	renames refs.Renames
	// app and githubAPIBase wire the GitHub App access-check client at a fake
	// GitHub; both nil/empty leaves the client in its "no app credentials"
	// state (every check answers ErrNoAppKey).
	app           *auth.AppConfig
	githubAPIBase string
	// settle is the document closer delay. The default keeps settlement-dependent
	// tests fast; a test that holds an issue row lock across the seeded spec's
	// settlement passes a delay that cannot fire before it finishes, because that
	// settlement locks the same row and would otherwise queue behind the test.
	settle time.Duration
	// oidc is the service-account token verifier; nil is the unconfigured
	// deployment, whose bearer handling is untouched by that feature.
	oidc ServiceTokenVerifier
	// brokerTokens decides a caller presenting an access token the secrets broker issued, under
	// the DPoP scheme. It is built from the test's own database, as the persistence seam below
	// is, because the verifier's DPoP replay store is a table of it (store.NewDPoPProofs); nil
	// is the deployment with no broker, which is every test but broker_token_test.go's and
	// project_scope_test.go's, and answers such a presentation 401 UNAUTHORIZED.
	brokerTokens func(*store.Store) *ciphertoken.Verifier
	// agentSecretsURL points Deps' broker relay for the credential pages at a fake secrets broker
	// (DISPATCH_AGENT_SECRETS_URL), and the server presents the account token
	// credentialTestAccountToken there from a file newRegisteredTestServer writes, as envoyURL's
	// is; empty is the deployment with no broker, whose credential routes answer FEATURE_OFF.
	agentSecretsURL string
	// identity resolves a cookie caller; nil is headerIdentity, which names the person the
	// X-Dispatch-User header names.
	identity identity.Identity
	// envoyURL points the Envoy listener client at a fake listener; empty leaves the client
	// unconfigured, which is every test that never asks whether a session is live.
	envoyURL string
	// envoyTimeout shortens that client's window, so a test can exercise a receipt timeout
	// without holding a stand-in listener for the production five seconds.
	envoyTimeout time.Duration
	// persistence is an optional document-store seam for API handlers that need the document
	// service to observe a persistence boundary condition.
	persistence func(*store.Store) docs.VersionedStore
	// agentStream is the live agent conversation relay; nil is the deployment with no NATS,
	// where an agent view is refused STREAM_UNAVAILABLE.
	agentStream agentstream.Source
	// agentPublisher queues a targeted message for a session no store of the listener's holds
	// (Deps.AgentPublisher). Nil is a recorder of the server's own, as a deployment with NATS
	// queues every such message; a test reads it through the Deps newTestServer answers
	// (publishedBy). noAgentPublisher is the deployment with neither NATS nor test hooks, which
	// queues none.
	agentPublisher   AgentPublisher
	noAgentPublisher bool
	// agentWatchInterval replaces how often an agent view re-arms its session (ten seconds), so a
	// test of the re-arm need not wait one out.
	agentWatchInterval time.Duration
	// embedder wires meaning search (LEGION-549); nil (every test but the meaning search tests'
	// own) is the deployment with no embedder configured, where search answers keyword-only.
	embedder embed.Embedder
	// answerer answers a going request's question (INFRA-1279); nil (every test but the going
	// routes' own) is the deployment with no model configured, where a run is recorded and its
	// stream carries the no-model-configured error.
	answerer Answerer
	// answerModel is the model the answerer was configured with, as the capability route names
	// it; decider is the decision model, nil (every test but the capability's own) for none.
	answerModel string
	decider     *Decider
	// files is the uploaded-file store; nil keeps every upload's bytes in Postgres, as a
	// deployment with no bucket does.
	files files.Store
	// lifetime is the lifetime Register runs the server on, building on it the delivery hub GET
	// /ws/live serves and the event stream relay every events subscription reads. Nil is the
	// test's own context (t.Context()), which ends before the cleanups that wait those
	// goroutines out; a test that stops the server before it ends (the socket's) passes one it
	// can cancel itself. It must end by the test's end, so derive it from t.Context().
	lifetime context.Context
	// clock is the server's Deps.Clock, which its timers and the listener client's window run
	// on: nil is clock.Real(), and a test that drives a timer passes a clocktest.Fake.
	clock clock.Clock
	// streamHeartbeat replaces the sockets' fifteen-second heartbeat.
	streamHeartbeat time.Duration
	// takeoverWindow replaces the listener take-over window (Deps.ListenerTakeoverWindow); zero is
	// testTakeoverWindow. A test proving the window itself passes its own, listenerTakeoverWindow
	// for the production twelve seconds.
	takeoverWindow time.Duration
	// testHookTimeout replaces the test hooks' acknowledgement timeout (Deps.TestHookTimeout);
	// zero keeps the production ten seconds.
	testHookTimeout time.Duration
	// tracer is installed on every connection of the server's pool (storetest.Traced), so a test
	// sees each statement the server runs; nil leaves the pool untraced. The xid window tests
	// (xid_window_test.go) pass a storetest.XidTracer.
	tracer pgx.QueryTracer
	// liveSocketAge is the server's Deps.LiveSocketAge: nil draws each socket's age as
	// production does.
	liveSocketAge func() time.Duration
	// liveSocketClock is the server's Deps.LiveSocketClock, which a live socket's pong deadline, its
	// wait for the page's close frame and its drain of a frame past the read limit run on: nil is
	// clock.Real(), and a test whose sockets must not end on a late pong passes clocktest.Held(), or
	// a fake it moves to each deadline.
	liveSocketClock clock.Clock
	// liveKinds adds view kinds to the ones a live socket serves beside the delivery views, or
	// replaces them, before any socket opens.
	liveKinds map[string]liveKind
	// dashboardEntry is the entry chunk of the dashboard build the server serves; empty is a
	// server with no dashboard, as the Go test rigs are.
	dashboardEntry string
	// buildCommit is the commit the server says it was built from (Deps.BuildCommit), which its
	// list validators name; empty is a build nothing stamped, as every `go test` binary is.
	buildCommit string
	// duplicateBound is the near-copy gate's bound (server.duplicateBound); zero keeps
	// production's duplicateGateTimeout. A case that reads the gate's verdict passes
	// nearCopyVerdictBound, so whether the machine met duplicateGateTimeout is not what it measures.
	duplicateBound time.Duration
	// inboxStatementBound is the mark-all's and the dismissal's statement_timeout
	// (server.inboxStatementBound); zero keeps production's inboxReadStatementTimeout. A case that
	// holds a dismissal behind a lock of its own passes pastTheRun (seedHeldInboxRead).
	inboxStatementBound time.Duration
	// mentionLookupBound is the server's bound on each listener lookup a comment's creation
	// makes, by the target looked up (server.mentionLookupBound); nil keeps production's
	// mentionLookupTimeout. A case whose verdict needs a lookup the listener answers to count as
	// answered gives that lookup a bound no run reaches, so a loaded machine cannot turn the
	// answer into an unanswered lookup.
	mentionLookupBound func(target string) time.Duration
	// relatedBound is the related-item check's meaning allowance and its wait for the
	// cross-project leg, with the check's own allowance past them (server.withRelatedBound);
	// zero keeps production's. A case whose verdict is what the check's meaning stage found
	// passes meaningVerdictBound, so whether the machine met production's three seconds is not
	// what it measures. The bound counts from each check's start, the seed writes' checks among
	// them.
	relatedBound time.Duration
	// outboxPublishes is Deps.OutboxPublishes: the server's deployment has an outbox, so a
	// mention's send waits for the earlier events of its owner the outbox still owes its session.
	// False is the deployment with no outbox, which every test that runs none is.
	outboxPublishes bool
	// metrics is the registry the server adds its held-send gauges to (Deps.Metrics); nil is
	// none, as every test that reads no gauge leaves it.
	metrics *metrics.Registry
	// legionStatusScope is Deps.LegionStatusScope (DISPATCH_LEGION_STATUS_SCOPE): R26's refusal
	// of a write that sets a Legion issue's status, takes the legion label off an issue, or moves
	// a Legion issue out from under every labelled issue, from a credential that holds neither
	// the person scope nor the legion-status scope. False is the deployment that has not turned
	// it on, which every test but legion_status_scope_test.go's runs as.
	legionStatusScope bool
	// parentStatusRule turns DISPATCH_PARENT_STATUS_RULE on for the server (Deps.ParentStatusRule).
	parentStatusRule bool
	// listenerRosterUncached is Deps.ListenerRosterUncached: every read of the listener's session
	// list asks the listener and stores no copy, as the browser suite's server runs
	// (DISPATCH_TEST_ROSTER_UNCACHED=1, e2e/run-server.sh). False is the deployment's stored copy.
	listenerRosterUncached bool
}

// testTakeoverWindow is the listener take-over window of a test server that sets none. A send to
// a session that is gone, or a listener that refuses, waits the window out before it settles, and
// the production twelve seconds made every such test sleep that long. 750 ms leaves exactly one
// retry: the first, after takeoverFirstBackoff (250 ms), still starts inside it whenever the first
// try took under half a second, which a test whose take-over answers on the second try relies on,
// and the second, 500 ms later, never does, so a window run out costs about a quarter second.
const testTakeoverWindow = 750 * time.Millisecond

// testServerURL is the public origin every test server serves under: what a link a handler
// writes names, and what a DPoP proof's 'htu' is checked against (broker_token_test.go).
const testServerURL = "https://dispatch.example"

// testSigningKey is the one-key set the package's session cookies are signed with.
var testSigningKey = auth.SingleSigningKey("signing-key")

// headerIdentity is the test header identity that records each named person in the database.
func headerIdentity(database *store.Store) identity.HeaderIdentity {
	return identity.HeaderIdentity{Header: "X-Dispatch-User", People: store.NewPgPeopleStore(database.Pool, testSigningKey, nil)}
}

// seedPeople records emails as people who have signed in: the assignee picker's options.
func seedPeople(t testing.TB, database *store.Store, emails ...string) {
	t.Helper()
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	for _, email := range emails {
		if err := people.Record(context.Background(), email); err != nil {
			t.Fatalf("seed person %q: %v", email, err)
		}
	}
}
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, _, _ := newTestServer(t, testServerOptions{})
	return handler
}

func newTestHandlerWithStore(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{})
	return handler, database
}

func newTestHandlerWithTestHooks(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{testHooks: true})
	return handler, database
}

// testEnvoyAccountTokenFile is a file holding a stand-in for Dispatch's projected token at the
// listener, for a server whose Envoy client is configured (envoyURL set), and "" otherwise.
func testEnvoyAccountTokenFile(t testing.TB, envoyURL string) string {
	t.Helper()
	if envoyURL == "" {
		return ""
	}
	path := filepath.Join(t.TempDir(), "envoy-account-token")
	if err := os.WriteFile(path, []byte("dispatch-test-account-token\n"), 0o600); err != nil {
		t.Fatalf("write Dispatch's test account token: %v", err)
	}
	return path
}

// newTestServer builds the mux an API test drives and hands back the store behind it and the
// dependencies it runs on, so a test that also calls a handler or a delivery step directly
// (directServer) shares this server's broker and document service instead of standing up a
// second pair.
func newTestServer(t testing.TB, options testServerOptions) (http.Handler, *store.Store, Deps) {
	t.Helper()
	handler, database, deps, _ := newRegisteredTestServer(t, options)
	return handler, database, deps
}

// newRegisteredTestServer is newTestServer, answering also the server Register mounted, whose
// delivery hub and open sockets a socket test reads.
func newRegisteredTestServer(t testing.TB, options testServerOptions) (http.Handler, *store.Store, Deps, *server) {
	t.Helper()
	settle := options.settle
	if settle == 0 {
		settle = 20 * time.Millisecond
	}
	lifetime := options.lifetime
	if lifetime == nil {
		lifetime = t.Context()
	}
	database := options.store
	if database == nil {
		database = storetest.Open(t)
	}
	if options.tracer != nil {
		database = &store.Store{Pool: storetest.Traced(t, database.Pool, options.tracer, nil)}
	}
	broker := events.NewBroker()
	var documentPersistence docs.VersionedStore
	if options.persistence != nil {
		documentPersistence = options.persistence(database)
	}
	var brokerTokens *ciphertoken.Verifier
	if options.brokerTokens != nil {
		brokerTokens = options.brokerTokens(database)
	}
	documentService := docs.New(docs.Deps{Store: database, Persistence: documentPersistence, Events: broker, ServerURL: testServerURL, Settle: settle})
	t.Cleanup(func() {
		if err := documentService.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	seedPeople(t, database, "alice", "bob")
	var people identity.Identity = headerIdentity(database)
	if options.identity != nil {
		people = options.identity
	}
	agentPublisher := options.agentPublisher
	switch {
	case options.noAgentPublisher:
		agentPublisher = nil
	case agentPublisher == nil:
		agentPublisher = NewAgentPublishRecorder()
	}
	agentSecretsTokenFile := ""
	if options.agentSecretsURL != "" {
		agentSecretsTokenFile = accountTokenFile(t, credentialTestAccountToken)
	}
	deps, err := NewDeps(DepsInput{
		Store:            database,
		Identity:         people,
		AgentTokens:      sharedAgentTokens(t, "agent-token"),
		NewIssueLabel:    options.newIssueLabel,
		NewIssueSession:  options.newIssueSession,
		ServerURL:        testServerURL,
		Docs:             documentService,
		Events:           broker,
		Renames:          options.renames,
		App:              options.app,
		GitHubAPIBase:    options.githubAPIBase,
		TestHooksEnabled: options.testHooks,
		StreamHeartbeat:  options.streamHeartbeat,
		EnvoyURL:         options.envoyURL,
		EnvoyTimeout:     options.envoyTimeout,
		// Dispatch presents an identity of its own to the listener, as a deployment does: without
		// one it refuses every send for a role (envoy.ErrNoListenerIdentity). The tests' stand-in listeners
		// check no bearer.
		EnvoyAccountTokenFile:  testEnvoyAccountTokenFile(t, options.envoyURL),
		ListenerTakeoverWindow: cmp.Or(options.takeoverWindow, testTakeoverWindow),
		TestHookTimeout:        options.testHookTimeout,
		// The horizon warns of a stall at horizontest.StallWarning rather than at the production
		// five and thirty seconds, as a cmd/dispatch test process's does, so a row's output names
		// whoever holds the horizon while it waits; the wait itself reads no warning
		// (horizontest.WaitInProcess).
		HorizonStallAfter:      horizontest.StallWarning,
		HorizonWarnEvery:       horizontest.StallWarning,
		OIDC:                   options.oidc,
		BrokerTokens:           brokerTokens,
		AgentSecretsURL:        options.agentSecretsURL,
		AgentSecretsTokenFile:  agentSecretsTokenFile,
		AgentStream:            options.agentStream,
		AgentPublisher:         agentPublisher,
		Embedder:               options.embedder,
		Answerer:               options.answerer,
		AnswerModel:            options.answerModel,
		Decider:                options.decider,
		Files:                  options.files,
		DashboardEntry:         options.dashboardEntry,
		BuildCommit:            options.buildCommit,
		OutboxPublishes:        options.outboxPublishes,
		Metrics:                options.metrics,
		LegionStatusScope:      options.legionStatusScope,
		ParentStatusRule:       ParentStatusRule{on: options.parentStatusRule},
		ListenerRosterUncached: options.listenerRosterUncached,
		Clock:                  options.clock,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	deps.LiveSocketAge = options.liveSocketAge
	deps.LiveSocketClock = options.liveSocketClock
	mux := http.NewServeMux()
	served := register(mux, lifetime, deps)
	maps.Copy(served.liveKinds, options.liveKinds)
	if options.agentWatchInterval != 0 {
		served.agentWatchInterval = options.agentWatchInterval
	}
	served.duplicateBound = options.duplicateBound
	served.inboxStatementBound = options.inboxStatementBound
	served.mentionLookupBound = options.mentionLookupBound
	served.withRelatedBound(options.relatedBound)
	// waitBackground, as production's shutdown calls it: every keyed handler and every delivery a
	// handler detached from its request, then the sockets, the hub, the relay and the test hooks'
	// listener. A broadcast's deliveries and a keyed handler outlive the client that started them,
	// so a test that did not wait for them would close its database under a worker still writing.
	t.Cleanup(func() { waitBackgroundOf(t, served) })
	return mux, database, deps, served
}

// waitBackgroundOf waits out served's background work at a test's end. A handler may be held on a
// release only a later cleanup runs - one its test's heldUntilReleased registered before this
// server, as a test that failed while the handler waited leaves it - so the test's releases run
// first (releaseHeld): the test is over, and they would run next anyway. A test's wait has one
// bound, the test binary's deadline less testwait's margin; a benchmark's waits as long as the
// work runs.
func waitBackgroundOf(t testing.TB, served *server) {
	releaseHeld(t)
	test, ok := t.(*testing.T)
	if !ok {
		served.waitBackground()
		return
	}
	waited := make(chan struct{})
	go func() {
		served.waitBackground()
		close(waited)
	}()
	testwait.Receive(test, "the server's background work to end", waited)
}

// directServer is a server a test calls a handler or a delivery step on directly, without the
// mux: it runs on the dependencies of the server under test - the same store, listener, broker
// and document service - so what it writes and what it reads are the rows that handler left. Its
// lifetime is context.Background(): a related-item check it runs starts a statistics refresh and
// may retry its record on that lifetime, which nothing here cancels.
func directServer(deps Deps) *server {
	return &server{deps: deps, lifetime: context.Background()}
}

// awaitArmedAt waits until a timer for at is pending on fake, the server's clock, and fails naming
// what and the deadlines it last saw when none ever is. It looks for the one deadline rather than a
// count, since the server's other timers on the same fake may be pending beside it.
func awaitArmedAt(t *testing.T, fake *clocktest.Fake, what string, at time.Time) {
	t.Helper()
	testwait.Until(t, what+" armed for "+at.Format(time.RFC3339Nano), func() ([]time.Time, bool) {
		armed := fake.Armed()
		return armed, slices.ContainsFunc(armed, at.Equal)
	})
}

// awaitArmedCountAt waits until at least count timers for at are pending on fake, the server's
// clock: a row with count sockets open moves the clock to their heartbeat only once each of them
// has armed it, so none is armed after the move and fires off a heartbeat the row did not drive.
func awaitArmedCountAt(t *testing.T, fake *clocktest.Fake, what string, at time.Time, count int) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("%s armed %d times for %s", what, count, at.Format(time.RFC3339Nano)), func() ([]time.Time, bool) {
		armed := fake.Armed()
		n := 0
		for _, deadline := range armed {
			if deadline.Equal(at) {
				n++
			}
		}
		return armed, n >= count
	})
}

// writeSessionRecord answers GET /v1/registry/{session_id} from the rows a stub listener also
// serves as its live list, so one literal gives both answers and the two agree the way a real
// listener's do. Dispatch resolves a session through this route rather than through the list,
// so a stub that served only the list would answer every lookup "no live session"; an id no row
// names is the listener's own 404, the one absence a caller may act on.
//
// A row that names no state or no last_seen is filled in here as a live session seen just now,
// which is what a real listener answers for a session that is registered and heartbeating: the
// stubs that predate session state say nothing about either, and a row read as unresponsive
// (or as last seen at the epoch) would have Dispatch hold claims it should hand on. A row that
// names them keeps what it names, which is how a test stands up a stalled session.
func writeSessionRecord(t *testing.T, w http.ResponseWriter, sessionsJSON, sessionID string) {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(sessionsJSON), &rows); err != nil {
		t.Errorf("decode the stub listener's sessions: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	for _, row := range rows {
		if row["session_id"] != sessionID {
			continue
		}
		if _, named := row["state"]; !named {
			row["state"] = envoy.StateLive
		}
		if _, named := row["last_seen"]; !named {
			row["last_seen"] = time.Now().UnixMilli()
		}
		_ = json.NewEncoder(w).Encode(row)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"no live session ` + sessionID + `"}`))
}

// waitForDatabaseLocks waits until want backends queue behind a lock that holder's transaction
// owns (storetest.WaitForLockWaiters), and fails the test when they have not by the test binary's
// deadline less testwait's margin, the wait's one bound.
func waitForDatabaseLocks(t *testing.T, holder pgx.Tx, want int) {
	t.Helper()
	storetest.WaitForLockWaiters(t, holder, want)
}

// awaitResponse is the next answer on responses, from a request the test started on a goroutine of
// its own. Its one bound is the test binary's deadline less testwait's margin.
func awaitResponse(t *testing.T, responses <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	return testwait.Receive(t, "the concurrent request's answer", responses)
}

func dispatchRequest(t testing.TB, handler http.Handler, method, target string, body any, login string) *httptest.ResponseRecorder {
	t.Helper()
	if strings.HasSuffix(target, "/answer") {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode answer request body: %v", err)
		}
		input := map[string]any{}
		if string(data) != "null" {
			if err := json.Unmarshal(data, &input); err != nil {
				t.Fatalf("decode answer request body: %v", err)
			}
		}
		if _, provided := input["expected_edited_at"]; !provided {
			input["expected_edited_at"] = nil
		}
		body = input
	}
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func multipartRequest(t *testing.T, handler http.Handler, target string, fields map[string]string, filename, contentType string, content []byte, login string) *httptest.ResponseRecorder {
	t.Helper()
	body, bodyType, err := apitest.MultipartUpload(fields, filename, contentType, content)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, target, body)
	request.Header.Set("Content-Type", bodyType)
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// databaseFingerprint hashes every row of every table, so a test can tell a request wrote nothing
// anywhere: no row inserted, updated or deleted. A test whose earlier requests left writes running
// waits them out before its first fingerprint (awaitBackgroundWrites): one that commits between two
// fingerprints reads as the write of the request between them.
func databaseFingerprint(t *testing.T, database *store.Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `select tablename from pg_tables where schemaname = 'public'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read table names: %v", err)
	}
	if len(tables) == 0 {
		t.Fatal("the schema has no tables; a fingerprint of it proves nothing")
	}
	fingerprint := make(map[string]string, len(tables))
	for _, table := range tables {
		var digest string
		if err := database.Pool.QueryRow(ctx, fmt.Sprintf(
			`select count(*) || ':' || coalesce(md5(string_agg(t::text, ',' order by t::text)), '') from %s t`,
			pgx.Identifier{table}.Sanitize(),
		)).Scan(&digest); err != nil {
			t.Fatalf("fingerprint %s: %v", table, err)
		}
		fingerprint[table] = digest
	}
	return fingerprint
}

// assertUnchanged fails for each table whose rows differ between two fingerprints of one database.
func assertUnchanged(t *testing.T, request string, before, after map[string]string) {
	t.Helper()
	for _, table := range slices.Sorted(maps.Keys(after)) {
		if before[table] != after[table] {
			t.Errorf("%s changed table %s", request, table)
		}
	}
}

// awaitBackgroundWrites waits for every write srv's earlier requests left running past their
// answers: the term-statistics refresh a related-item check starts for a project with no statistics
// yet (ensureSearchTermStats), which commits search_term_stats and search_term_totals rows when it
// finishes, and every detached run (srv.detached), such as a related_checks record's retry. Its one
// bound is the test binary's deadline less testwait's margin.
func awaitBackgroundWrites(t *testing.T, srv *server) {
	t.Helper()
	rows, err := srv.deps.Store.Pool.Query(context.Background(), `select key from projects`)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	projects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read project keys: %v", err)
	}
	for _, project := range projects {
		awaitTermStatsRefresh(t, srv, project)
	}
	idle := make(chan struct{})
	go func() {
		srv.detached.wait()
		close(idle)
	}()
	testwait.Receive(t, "the server's detached work to end", idle)
}

// assertRefusal checks an answer is 400 with code and an error naming field first.
func assertRefusal(t *testing.T, request string, status int, body []byte, code, field string) {
	t.Helper()
	var refusal struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &refusal)
	if status != http.StatusBadRequest || refusal.Code != code || !strings.HasPrefix(refusal.Error, field+" holds a ") {
		t.Errorf("%s = %d %s %q, want 400 %s naming %s", request, status, refusal.Code, refusal.Error, code, field)
	}
}

func decodeBody[T any](t testing.TB, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response body %q: %v", response.Body.String(), err)
	}
	return value
}

// overCapCases are texts just past a cap of limit UTF-16 units, the one-character-over case and
// one counted in UTF-16 units, each with the length the refusal names.
func overCapCases(limit int) map[string]struct {
	text   string
	length int
} {
	return map[string]struct {
		text   string
		length int
	}{
		"one character over": {strings.Repeat("a", limit+1), limit + 1},
		// 501 characters outside the Basic Multilingual Plane are 1,002 UTF-16 units.
		"counted in UTF-16 units": {strings.Repeat("😀", limit/2+1), limit + 2},
	}
}

// assertCapExceeded requires response to be the 400 CAP_EXCEEDED capExceededError writes for a
// field of length units past a cap of limit, followed by "; hint" when hint is not empty.
func assertCapExceeded(t *testing.T, request string, response *httptest.ResponseRecorder, field string, length, limit int, hint string) {
	t.Helper()
	want := fmt.Sprintf("%s is %d characters over the %d-character limit (%d/%d)", field, length-limit, limit, length, limit)
	if hint != "" {
		want += "; " + hint
	}
	assertRefusalMessage(t, request, response, want)
}

// assertBlockCapExceeded requires response to be the 400 CAP_EXCEEDED a body whose block at
// position (counted from 1) holds units UTF-16 units is refused with, the block being of
// blockType.
func assertBlockCapExceeded(t *testing.T, request string, response *httptest.ResponseRecorder, field string, position int, blockType string, units int) {
	t.Helper()
	limit := contracts.DispatchBlockUnits
	want := fmt.Sprintf("%s block %d (%s) is %d characters over the %d-character block limit (%d/%d)",
		field, position, blockType, units-limit, limit, units, limit)
	assertRefusalMessage(t, request, response, want)
}

// assertBlockCountExceeded requires response to be the 400 CAP_EXCEEDED a body of blocks blocks
// is refused with.
func assertBlockCountExceeded(t *testing.T, request string, response *httptest.ResponseRecorder, field string, blocks int) {
	t.Helper()
	limit := contracts.DispatchItemBlocks
	want := fmt.Sprintf("%s is %d blocks over the %d-block limit (%d/%d)", field, blocks-limit, limit, blocks, limit)
	assertRefusalMessage(t, request, response, want)
}

// assertRefusalMessage requires response to be 400 CAP_EXCEEDED reading want.
func assertRefusalMessage(t *testing.T, request string, response *httptest.ResponseRecorder, want string) {
	t.Helper()
	body := response.Body.String()
	refusal := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	if response.Code != http.StatusBadRequest || refusal.Code != "CAP_EXCEEDED" || refusal.Error != want {
		t.Fatalf("%s: status=%d body=%.300s, want 400 CAP_EXCEEDED %q", request, response.Code, body, want)
	}
}

func TestCreateProjectIssueAndReadPrimaryDocument(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	project := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice")
	if project.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", project.Code, project.Body.String())
	}

	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "First issue", "spec": "# Hello",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
		LastSeq           int    `json:"last_seq"`
	}](t, issueResponse)
	if issue.Key != "TEST-1" {
		t.Fatalf("issue key: got %q, want TEST-1", issue.Key)
	}
	if issue.PrimaryArtifactID == "" {
		t.Fatal("created issue has no primary artifact")
	}
	if issue.LastSeq != 1 {
		t.Fatalf("created issue event sequence: got %d, want 1", issue.LastSeq)
	}

	textResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if textResponse.Code != http.StatusOK {
		t.Fatalf("read primary document: status=%d body=%s", textResponse.Code, textResponse.Body.String())
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
		Version  *int   `json:"version"`
	}](t, textResponse)
	if text.Markdown != "# Hello\n" || text.Version == nil || *text.Version != 1 {
		t.Fatalf("primary document: got %#v, want markdown # Hello at version 1", text)
	}
}

func TestCreateIssueWithMissingOrBlankSpecGetsAnEmptyPrimaryDocument(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	project := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice")
	if project.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", project.Code, project.Body.String())
	}

	empty := ""
	whitespace := " \n\t "
	for _, test := range []struct {
		name string
		spec *string
	}{
		{name: "omitted"},
		{name: "empty", spec: &empty},
		{name: "whitespace", spec: &whitespace},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := map[string]any{"project": "TEST", "title": "Seeded issue", "force": true}
			if test.spec != nil {
				input["spec"] = *test.spec
			}
			issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", input, "alice")
			if issueResponse.Code != http.StatusCreated {
				t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
			}
			issue := decodeBody[struct {
				PrimaryArtifactID string `json:"primary_artifact_id"`
			}](t, issueResponse)

			textResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
			if textResponse.Code != http.StatusOK {
				t.Fatalf("read primary document: status=%d body=%s", textResponse.Code, textResponse.Body.String())
			}
			text := decodeBody[struct {
				Markdown string `json:"markdown"`
				Version  *int   `json:"version"`
			}](t, textResponse)
			if text.Markdown != "" || text.Version == nil || *text.Version != 1 {
				t.Fatalf("primary document = %#v, want empty markdown at version 1", text)
			}
		})
	}
}

func TestCreateIssueRejectsSpecOutsideProofSchema(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	project := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice")
	if project.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", project.Code, project.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Invalid spec", "spec": "<details>x</details>",
	}, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_MARKDOWN"`) {
		t.Fatalf("create invalid issue spec: status=%d body=%s", response.Code, response.Body.String())
	}
}

// A quoted closing tag a tab ends opens a block of HTML in the browser editor's parser and none
// in goldmark, and that block swallows the line under it, so nothing the document could be
// stored as reads back as written and the create is refused, naming the line as written
// (pmdoc's htmlFlowRefusal).
func TestCreateIssueRejectsASpecLineTheBrowserReadsAsBlockHTML(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	project := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice")
	if project.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", project.Code, project.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "A closing tag a tab ends", "spec": "> </a\t>\n> text\n",
	}, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_MARKDOWN"`) {
		t.Fatalf("create a spec line the browser reads as block HTML: status=%d body=%s", response.Code, response.Body.String())
	}
	refusal := decodeBody[struct {
		Error string `json:"error"`
	}](t, response).Error
	if !strings.Contains(refusal, `line 1, "> </a\t>"`) {
		t.Fatalf("refusal names no line as written: %q", refusal)
	}
	// The clause of pmdoc's browserBlockHTML naming what the two parsers read apart, which this
	// package cannot import and no other refusal of this route carries.
	if !strings.Contains(refusal, "reads as a block of HTML where goldmark reads the text of a paragraph") {
		t.Fatalf("refusal carries none of the browser-block-HTML wording: %q", refusal)
	}
}

func TestDocumentTextReportsUnavailableService(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store:       database,
		Persistence: failingLoadStore{VersionedStore: docs.NewPgVersioned(database)},
		Events:      broker,
	})
	t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store:    database,
		Identity: headerIdentity(database),
		Docs:     documentService,
		Events:   broker,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), deps).Wait)

	if response := dispatchRequest(t, mux, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, mux, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue", "spec": "before",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, issueResponse)

	response := dispatchRequest(t, mux, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"DOC_SERVICE_UNAVAILABLE"`) {
		t.Fatalf("unavailable document response: status=%d body=%s", response.Code, response.Body.String())
	}

	blocks := dispatchRequest(t, mux, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice")
	if blocks.Code != http.StatusServiceUnavailable || !strings.Contains(blocks.Body.String(), `"code":"DOC_SERVICE_UNAVAILABLE"`) {
		t.Fatalf("unavailable document blocks: status=%d body=%s", blocks.Code, blocks.Body.String())
	}
}

// failingLoadStore is a store that cannot serve any read of a document: a room's load, a cold
// read's stamp, or its load.
type failingLoadStore struct {
	docs.VersionedStore
}

func (failingLoadStore) Load(context.Context, string) (persistence.LoadResult, error) {
	return persistence.LoadResult{}, errors.New("persistence unavailable")
}

func (failingLoadStore) DocumentStamp(context.Context, string) (docs.DocumentStamp, error) {
	return docs.DocumentStamp{}, errors.New("persistence unavailable")
}

func (failingLoadStore) LoadDocument(context.Context, string) (docs.LoadedDocument, error) {
	return docs.LoadedDocument{}, errors.New("persistence unavailable")
}

func TestDocumentEditRejectsInvalidOperationField(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue", "spec": "before",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, issueResponse)

	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "with": "after"}},
	}, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_OP"`) || !strings.Contains(response.Body.String(), "find") {
		t.Fatalf("invalid edit response: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEventNotifyRules(t *testing.T) {
	t.Parallel()
	broker := events.NewBroker()
	user := model.Actor{Kind: "user", ID: "alice"}
	session := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	for _, test := range []struct {
		name  string
		event model.Event
		want  bool
	}{
		{name: "user message", event: model.Event{Type: "message.created", Actor: user}, want: true},
		{name: "session message", event: model.Event{Type: "message.created", Actor: session}, want: false},
		{name: "user title change", event: model.Event{Type: "issue.updated", Actor: user}, want: true},
		{name: "session ask", event: model.Event{Type: "ask.opened", Actor: session}, want: false},
		{name: "session resolve", event: model.Event{Type: "ask.resolved", Actor: session}, want: false},
		{name: "user resolve", event: model.Event{Type: "ask.resolved", Actor: user}, want: true},
		{name: "user named version", event: model.Event{Type: "artifact.version", Actor: user, Payload: map[string]any{"version": model.Version{Named: true}}}, want: true},
		{name: "session named version", event: model.Event{Type: "artifact.version", Actor: session, Payload: map[string]any{"version": model.Version{Named: true}}}, want: false},
		{name: "user unnamed version", event: model.Event{Type: "artifact.version", Actor: user, Payload: map[string]any{"version": model.Version{Named: false}}}, want: false},
		{name: "user human edit", event: model.Event{Type: "artifact.human_edited", Actor: user, Payload: model.ArtifactHumanEditedEventPayload{ArtifactID: "artifact-1", FromVersion: 4, ToVersion: 7}}, want: true},
		{name: "child status", event: model.Event{Type: "child.status", Actor: session}, want: true},
		{name: "child added", event: model.Event{Type: "child.added", Actor: session}, want: true},
		{name: "child removed", event: model.Event{Type: "child.removed", Actor: session}, want: true},
		{name: "project creation", event: model.Event{Type: "project.created", Actor: user}, want: false},
		{name: "repository project mapping", event: model.Event{Type: "settings.repo_project.updated", Actor: user}, want: false},
		{name: "architecture source", event: model.Event{Type: "settings.architecture_source.updated", Actor: user}, want: false},
		{name: "project default assignee", event: model.Event{Type: "settings.project_default_assignee.updated", Actor: user}, want: false},
		{name: "user state", event: model.Event{Type: "user_state.updated", Actor: user}, want: false},
		{name: "user thread state", event: model.Event{Type: "user_thread_state.updated", Actor: user}, want: false},
		{name: "user decision kept", event: model.Event{Type: "decision.kept", Actor: user}, want: false},
		{name: "user priority write", event: model.Event{Type: "priority.updated", Actor: user, Payload: model.PriorityEventPayload{ID: "ship-the-daemon", Change: model.PriorityChanged}}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := broker.Notify(test.event); got != test.want {
				t.Fatalf("Notify(%+v) = %t, want %t", test.event, got, test.want)
			}
		})
	}
}

func TestExternalIssueResolutionAndAutoCreation(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	for number := 1; number <= 2; number++ {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "TEST", "title": "ordinary issue", "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue %d: status=%d body=%s", number, response.Code, response.Body.String())
		}
	}
	unlinked := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/resolve?ref=owner/repo%237", nil, "alice")
	if unlinked.Code != http.StatusNotFound {
		t.Fatalf("resolve unlinked external issue: status=%d body=%s", unlinked.Code, unlinked.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "owner/repo#7", "project": "TEST",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("auto-create external issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key           string `json:"key"`
		ExternalLinks []struct {
			URL string `json:"url"`
		} `json:"external_links"`
	}](t, created)
	if issue.Key != "TEST-3" {
		t.Fatalf("external issue key: got %q, want TEST-3", issue.Key)
	}
	if len(issue.ExternalLinks) != 1 || issue.ExternalLinks[0].URL != "https://github.com/owner/repo/issues/7" {
		t.Fatalf("external links: got %#v", issue.ExternalLinks)
	}
}

func TestExternalIssueReferenceResolvesBothGitHubURLShapes(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	create := func(title string) model.Issue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "TEST", "title": title, "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %q: status=%d body=%s", title, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response)
	}
	link := func(key, url string) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
			"external_links": []map[string]string{{"url": url}},
		}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("link %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
	issueLink := create("GitHub issue link")
	pullLink := create("GitHub pull request link")
	link(issueLink.Key, "https://github.com/owner/repo/issues/42")

	resolvedIssue := dispatchRequest(
		t,
		handler,
		http.MethodGet,
		"/api/v1/issues/resolve?ref=owner/repo%2342",
		nil,
		"alice",
	)
	if resolvedIssue.Code != http.StatusOK {
		t.Fatalf("resolve GitHub issue link: status=%d body=%s", resolvedIssue.Code, resolvedIssue.Body.String())
	}
	if got := decodeBody[struct {
		Key string `json:"key"`
	}](t, resolvedIssue).Key; got != issueLink.Key {
		t.Fatalf("resolved issue key = %q, want %q", got, issueLink.Key)
	}

	link(pullLink.Key, "https://github.com/owner/repo/pull/42")
	resolvedPull := dispatchRequest(
		t,
		handler,
		http.MethodGet,
		"/api/v1/issues/resolve?ref=owner/repo%2342",
		nil,
		"alice",
	)
	if resolvedPull.Code != http.StatusBadRequest {
		t.Fatalf("resolve ambiguous GitHub links: status=%d body=%s", resolvedPull.Code, resolvedPull.Body.String())
	}
	body := resolvedPull.Body.String()
	if !strings.Contains(body, `"code":"AMBIGUOUS_ISSUE_REF"`) ||
		!strings.Contains(body, issueLink.Key) ||
		!strings.Contains(body, pullLink.Key) {
		t.Fatalf("ambiguous response = %s, want AMBIGUOUS_ISSUE_REF naming %s and %s", body, issueLink.Key, pullLink.Key)
	}
}

func TestExternalIssueCreationReusesPullRequestLink(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	existing := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Linked pull request",
	}, "alice")
	if existing.Code != http.StatusCreated {
		t.Fatalf("create existing issue: status=%d body=%s", existing.Code, existing.Body.String())
	}
	issue := decodeBody[model.Issue](t, existing)
	if linked := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"external_links": []map[string]string{{
			"url": "https://github.com/owner/repo/pull/42",
		}},
	}, "alice"); linked.Code != http.StatusOK {
		t.Fatalf("link pull request: status=%d body=%s", linked.Code, linked.Body.String())
	}

	reused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "owner/repo#42", "project": "TEST",
	}, "alice")
	if reused.Code != http.StatusOK {
		t.Fatalf("reuse pull-request link: status=%d body=%s", reused.Code, reused.Body.String())
	}
	if got := decodeBody[model.Issue](t, reused).Key; got != issue.Key {
		t.Fatalf("reused issue key = %q, want %q", got, issue.Key)
	}
	var issueCount int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from issues`).Scan(&issueCount); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if issueCount != 1 {
		t.Fatalf("issue count = %d, want 1", issueCount)
	}
}

// An import names its project: a create that names external and no project, or an empty one,
// is refused naming project, and stores no issue and no link.
func TestExternalIssueRejectsAMissingProject(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	for _, body := range []map[string]string{
		{"external": "owner/repo#1"},
		{"external": "owner/repo#1", "project": ""},
		{"external": "owner/repo#1", "project": "  "},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
		if response.Code != http.StatusBadRequest || responseCode(t, response) != "INVALID_ISSUE" ||
			!strings.Contains(response.Body.String(), "project is required when external is set") {
			t.Errorf("create %v: status=%d body=%s, want 400 INVALID_ISSUE naming project", body, response.Code, response.Body.String())
		}
	}
	var issues, links int
	if err := database.Pool.QueryRow(context.Background(), `
		select (select count(*) from issues), (select count(*) from issue_external_links)
	`).Scan(&issues, &links); err != nil {
		t.Fatalf("count issues and links: %v", err)
	}
	if issues != 0 || links != 0 {
		t.Fatalf("after the refusals: %d issues and %d links, want none", issues, links)
	}
}

// An external ref already linked to an issue of one project is not imported into another: the
// refusal names both projects and no repository setting.
func TestExternalIssueLinkedInAnotherProjectIsRefused(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	for _, project := range []map[string]string{{"key": "CORE", "name": "Core"}, {"key": "OTHER", "name": "Other"}} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", project, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project["key"], response.Code, response.Body.String())
		}
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "owner/repo#1", "project": "CORE",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("import owner/repo#1 into CORE: status=%d body=%s", created.Code, created.Body.String())
	}
	key := decodeBody[model.Issue](t, created).Key
	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "owner/repo#1", "project": "OTHER",
	}, "alice")
	body := refused.Body.String()
	if refused.Code != http.StatusBadRequest || responseCode(t, refused) != "EXTERNAL_PROJECT_MISMATCH" ||
		!strings.Contains(body, key) || !strings.Contains(body, "project CORE") || !strings.Contains(body, "project OTHER") ||
		strings.Contains(body, "setting") {
		t.Fatalf("import owner/repo#1 into OTHER: status=%d body=%s, want 400 EXTERNAL_PROJECT_MISMATCH naming %s, CORE and OTHER", refused.Code, body, key)
	}
	var issues int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from issues`).Scan(&issues); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if issues != 1 {
		t.Fatalf("issue count = %d, want 1", issues)
	}
}

// dispatch_issue's external says what POST /api/v1/issues does with the reference it names: a
// reference already imported into the call's project answers its issue and creates nothing, and
// one imported into another project is refused 400 EXTERNAL_PROJECT_MISMATCH. The words are the
// tool's generated copy (contracts.DispatchTools), the text the catalogue serves, and each promise
// is held to the route here, so a change of either the words or the route fails this test.
func TestDispatchIssuesExternalWordsAreWhatItsRouteAnswers(t *testing.T) {
	t.Parallel()
	var words string
	for _, tool := range contracts.DispatchTools {
		for _, argument := range tool.Arguments {
			if tool.Name == "dispatch_issue" && argument.Name == "external" {
				words = argument.Description
			}
		}
	}
	for _, promise := range []string{"owner/repo#n", "answers its issue and creates nothing", "400 EXTERNAL_PROJECT_MISMATCH"} {
		if !strings.Contains(words, promise) {
			t.Fatalf("dispatch_issue's external says %q, want it to say %q", words, promise)
		}
	}
	handler := newTestHandler(t)
	for _, project := range []map[string]string{{"key": "CORE", "name": "Core"}, {"key": "OTHER", "name": "Other"}} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", project, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project["key"], response.Code, response.Body.String())
		}
	}
	importInto := func(project string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"external": "owner/repo#7", "project": project,
		}, "alice")
	}
	created := importInto("CORE")
	if created.Code != http.StatusCreated {
		t.Fatalf("import owner/repo#7 into CORE: status=%d body=%s", created.Code, created.Body.String())
	}
	key := decodeBody[model.Issue](t, created).Key
	if again := importInto("CORE"); again.Code != http.StatusOK || decodeBody[model.Issue](t, again).Key != key {
		t.Fatalf("import owner/repo#7 into CORE again: status=%d body=%s, want 200 answering %s", again.Code, again.Body.String(), key)
	}
	if refused := importInto("OTHER"); refused.Code != http.StatusBadRequest || responseCode(t, refused) != "EXTERNAL_PROJECT_MISMATCH" {
		t.Fatalf("import owner/repo#7 into OTHER: status=%d body=%s, want 400 EXTERNAL_PROJECT_MISMATCH", refused.Code, refused.Body.String())
	}
}

func TestArtifactVersions(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue", "spec": "# Initial",
	}, "alice")
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, issueResponse)
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}

	first := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "a.png"}, "a.png", "image/png", []byte("first image"), "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("upload first image: status=%d body=%s", first.Code, first.Body.String())
	}
	image := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, first)
	blob := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+image.Artifact.ID+"/versions/1", nil, "alice")
	checksum := sha256.Sum256([]byte("first image"))
	if blob.Code != http.StatusOK || blob.Body.String() != "first image" || blob.Header().Get("Content-Type") != "image/png" || blob.Header().Get("ETag") != hex.EncodeToString(checksum[:]) || blob.Header().Get("Content-Disposition") != `attachment; filename="a.png"; filename*=UTF-8''a.png` || blob.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("stream first blob: status=%d headers=%v body=%s", blob.Code, blob.Header(), blob.Body.String())
	}

	second := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "a.png"}, "a.png", "image/png", []byte("second image"), "alice")
	if second.Code != http.StatusCreated {
		t.Fatalf("upload second image: status=%d body=%s", second.Code, second.Body.String())
	}
	version := decodeBody[struct {
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
	}](t, second)
	if version.Version.Number != 2 {
		t.Fatalf("second image version: got %d, want 2", version.Version.Number)
	}

	notes := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "notes.md", "text/markdown", []byte("# Notes"), "alice")
	if notes.Code != http.StatusCreated {
		t.Fatalf("upload document: status=%d body=%s", notes.Code, notes.Body.String())
	}
	uploaded := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, notes)
	replacement := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md", "precondition": uploadBaseField(t, handler, uploaded.Artifact.ID)}, "notes.md", "text/markdown", []byte("# Revised"), "alice")
	if replacement.Code != http.StatusCreated {
		t.Fatalf("replace document: status=%d body=%s", replacement.Code, replacement.Body.String())
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+uploaded.Artifact.ID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"# Revised\n"`) {
		t.Fatalf("read replaced document: status=%d body=%s", text.Code, text.Body.String())
	}

}

func agentRequest(t *testing.T, handler http.Handler, method, target string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode agent request body: %v", err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestIssueRouteVersionEventsAndChildStatus(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	parentResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Parent",
	}, "alice")
	parent := decodeBody[struct {
		Key string `json:"key"`
	}](t, parentResponse)
	childResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Child", "parent": parent.Key,
	}, "alice")
	child := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, childResponse)
	if childResponse.Code != http.StatusCreated {
		t.Fatalf("create child: status=%d body=%s", childResponse.Code, childResponse.Body.String())
	}
	updated := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]string{
		"route": "role:legion-controller-x",
	}, "alice")
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"route":"role:legion-controller-x"`) {
		t.Fatalf("set valid route: status=%d body=%s", updated.Code, updated.Body.String())
	}
	invalid := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]string{
		"route": "bogus",
	}, "alice")
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"ROUTE_INVALID"`) {
		t.Fatalf("reject invalid route: status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	status := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]string{
		"status": "in_progress",
	}, "alice")
	if status.Code != http.StatusOK {
		t.Fatalf("change child status: status=%d body=%s", status.Code, status.Body.String())
	}

	named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+child.PrimaryArtifactID+"/versions", map[string]string{
		"summary": "checkpoint",
	}, "alice")
	if named.Code != http.StatusCreated || !strings.Contains(named.Body.String(), `"named":true`) {
		t.Fatalf("create user named version: status=%d body=%s", named.Code, named.Body.String())
	}
	blankSummary := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+child.PrimaryArtifactID+"/versions", map[string]string{
		"summary": "   ",
	}, "alice")
	if blankSummary.Code != http.StatusBadRequest || !strings.Contains(blankSummary.Body.String(), `"code":"INVALID_VERSION"`) {
		t.Fatalf("blank-summary named version: status=%d body=%s", blankSummary.Code, blankSummary.Body.String())
	}
	sessionVersion := agentRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+child.PrimaryArtifactID+"/versions", map[string]any{
		"summary": "agent checkpoint",
		"actor":   map[string]string{"kind": "session", "id": "abcdef0123456789"},
	}, "agent-token")
	if sessionVersion.Code != http.StatusCreated {
		t.Fatalf("create session named version: status=%d body=%s", sessionVersion.Code, sessionVersion.Body.String())
	}

	childEvents := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+child.Key+"/events", nil, "alice")
	var childLog []struct {
		Seq    int    `json:"seq"`
		Type   string `json:"type"`
		Notify bool   `json:"notify"`
	}
	if err := json.NewDecoder(childEvents.Body).Decode(&childLog); err != nil {
		t.Fatalf("decode child events: %v", err)
	}
	if len(childLog) != 5 {
		t.Fatalf("child events: got %#v, want five events", childLog)
	}
	for index, event := range childLog {
		if event.Seq != index+1 {
			t.Fatalf("child event %d sequence: got %d, want %d", index, event.Seq, index+1)
		}
	}
	if !childLog[3].Notify || childLog[4].Notify {
		t.Fatalf("named version notifications: got user=%t session=%t", childLog[3].Notify, childLog[4].Notify)
	}

	parentEvents := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+parent.Key+"/events", nil, "alice")
	var parentLog []struct {
		Type   string `json:"type"`
		Notify bool   `json:"notify"`
	}
	if err := json.NewDecoder(parentEvents.Body).Decode(&parentLog); err != nil {
		t.Fatalf("decode parent events: %v", err)
	}
	if len(parentLog) != 2 || parentLog[1].Type != "child.status" || !parentLog[1].Notify {
		t.Fatalf("parent events: got %#v, want child.status notification", parentLog)
	}
}

func TestListIssueEventsSupportsNewestCursorAndIDLookup(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Event queries",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, issueResponse)

	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin event seed transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	broker := events.NewBroker()
	eventIDs := make(map[int]int64, 249)
	for wantSeq := 2; wantSeq <= 250; wantSeq++ {
		event, err := broker.Append(ctx, tx, model.Event{
			IssueKey: new(issue.Key),
			Type:     "test.seeded",
			Actor:    model.Actor{Kind: "session", ID: "seed-session-0123456789"},
			Payload:  map[string]any{"seq": wantSeq},
		})
		if err != nil {
			t.Fatalf("append event %d: %v", wantSeq, err)
		}
		if event.Seq != wantSeq {
			t.Fatalf("seed event sequence: got %d, want %d", event.Seq, wantSeq)
		}
		eventIDs[wantSeq] = event.ID
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit event seed transaction: %v", err)
	}

	type listedEvent struct {
		ID  int64 `json:"id"`
		Seq int   `json:"seq"`
	}
	newest := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events?order=desc", nil, "alice")
	if newest.Code != http.StatusOK {
		t.Fatalf("list newest events: status=%d body=%s", newest.Code, newest.Body.String())
	}
	newestLog := decodeBody[[]listedEvent](t, newest)
	if len(newestLog) != 200 {
		t.Fatalf("newest events: got %d, want 200", len(newestLog))
	}
	for index, event := range newestLog {
		if want := 250 - index; event.Seq != want {
			t.Fatalf("newest event %d: got sequence %d, want %d", index, event.Seq, want)
		}
	}

	before := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events?before=51", nil, "alice")
	if before.Code != http.StatusOK {
		t.Fatalf("list events before cursor: status=%d body=%s", before.Code, before.Body.String())
	}
	beforeLog := decodeBody[[]listedEvent](t, before)
	if len(beforeLog) != 50 {
		t.Fatalf("events before cursor: got %d, want 50", len(beforeLog))
	}
	for index, event := range beforeLog {
		if want := 50 - index; event.Seq != want {
			t.Fatalf("event before cursor %d: got sequence %d, want %d", index, event.Seq, want)
		}
	}

	ids := dispatchRequest(t, handler, http.MethodGet,
		"/api/v1/issues/"+issue.Key+"/events?ids="+fmt.Sprintf("%d,%d,%d", eventIDs[250], eventIDs[2], eventIDs[100]),
		nil, "alice")
	if ids.Code != http.StatusOK {
		t.Fatalf("list event IDs: status=%d body=%s", ids.Code, ids.Body.String())
	}
	idLog := decodeBody[[]listedEvent](t, ids)
	for index, want := range []int{2, 100, 250} {
		if len(idLog) != 3 || idLog[index].Seq != want {
			t.Fatalf("events by ID: got %#v, want sequences [2 100 250]", idLog)
		}
	}

	for _, target := range []string{
		"/api/v1/issues/" + issue.Key + "/events?ids=" + strings.Repeat("1,", 50) + "1",
		"/api/v1/issues/" + issue.Key + "/events?after=0&before=10",
		"/api/v1/issues/" + issue.Key + "/events?after=0&order=desc",
		"/api/v1/issues/" + issue.Key + "/events?after=not-a-number",
		"/api/v1/issues/" + issue.Key + "/events?before=not-a-number",
		"/api/v1/issues/" + issue.Key + "/events?before=",
		"/api/v1/issues/" + issue.Key + "/events?before=%20",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_QUERY"`) {
			t.Fatalf("reject invalid event query %q: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

func TestActorAuthenticationRules(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	unauthenticated := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "")
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("header-less project creation: status=%d body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}
	noActor := agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "agent-token")
	if noActor.Code != http.StatusBadRequest || !strings.Contains(noActor.Body.String(), `"code":"ACTOR_KIND"`) {
		t.Fatalf("bearer without actor: status=%d body=%s", noActor.Code, noActor.Body.String())
	}
	userActor := agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "TEST", "name": "Test project", "actor": map[string]string{"kind": "user", "id": "alice"},
	}, "agent-token")
	if userActor.Code != http.StatusBadRequest || !strings.Contains(userActor.Body.String(), `"code":"ACTOR_KIND"`) {
		t.Fatalf("bearer user actor: status=%d body=%s", userActor.Code, userActor.Body.String())
	}
	wrongToken := agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "TEST", "name": "Test project", "actor": map[string]string{"kind": "session", "id": "abcdef0123456789"},
	}, "wrong-token")
	if wrongToken.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: status=%d body=%s", wrongToken.Code, wrongToken.Body.String())
	}
}

func TestBearerReadRoutesDoNotRequireActor(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST",
		"title":   "Bearer reads",
		"spec":    "# Read me",
		"actor":   map[string]string{"kind": "session", "id": "abcdef0123456789"},
	}, "agent-token")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	askResponse := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Is this readable?",
		"actor":    map[string]string{"kind": "session", "id": "abcdef0123456789"},
	}, "agent-token")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, askResponse)
	comment := agentRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":  "A readable comment",
		"actor": map[string]string{"kind": "session", "id": "abcdef0123456789"},
	}, "agent-token")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", comment.Code, comment.Body.String())
	}

	for _, target := range []string{
		"/api/v1/projects",
		"/api/v1/issues",
		"/api/v1/issues/resolve?ref=" + issue.Key,
		"/api/v1/issues/" + issue.Key,
		"/api/v1/issues/" + issue.Key + "/events",
		"/api/v1/issues/" + issue.Key + "/artifacts",
		"/api/v1/asks/" + ask.ID,
		"/api/v1/issues/" + issue.Key + "/comments",
		"/api/v1/artifacts/" + issue.PrimaryArtifactID,
		"/api/v1/artifacts/" + issue.PrimaryArtifactID + "/text",
		"/api/v1/artifacts/" + issue.PrimaryArtifactID + "/versions/1",
	} {
		t.Run(target, func(t *testing.T) {
			response := agentRequest(t, handler, http.MethodGet, target, nil, "agent-token")
			if response.Code != http.StatusOK {
				t.Fatalf("bearer read: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	// The inbox is one of the rows the person scope opens, and the shared token holds no scope.
	inbox := agentRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "agent-token")
	if inbox.Code != http.StatusForbidden || !strings.Contains(inbox.Body.String(), `"code":"`+codeScopeRequired+`"`) {
		t.Fatalf("the shared token's inbox: status=%d body=%s, want 403 %s", inbox.Code, inbox.Body.String(), codeScopeRequired)
	}
	anonymous := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "")
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous issue read: status=%d body=%s", anonymous.Code, anonymous.Body.String())
	}
}

func TestPerUserIssueStateIsIsolated(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	saved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"pinned": true, "last_read_seq": 1, "dismissed": []string{"ask-1"},
	}, "alice")
	if saved.Code != http.StatusOK {
		t.Fatalf("save Alice state: status=%d body=%s", saved.Code, saved.Body.String())
	}
	alice := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/state", nil, "alice")
	if !strings.Contains(alice.Body.String(), `"TEST-1":{"pinned":true,"last_read_seq":1,"dismissed":["ask-1"],"seq":0}`) {
		t.Fatalf("Alice state: status=%d body=%s", alice.Code, alice.Body.String())
	}
	bob := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/state", nil, "bob")
	if bob.Code != http.StatusOK || strings.Contains(bob.Body.String(), issue.Key) {
		t.Fatalf("Bob state leaked Alice pin: status=%d body=%s", bob.Code, bob.Body.String())
	}
	session := agentRequest(t, handler, http.MethodGet, "/api/v1/me/state", nil, "agent-token")
	if session.Code != http.StatusForbidden || !strings.Contains(session.Body.String(), `"code":"`+codeScopeRequired+`"`) {
		t.Fatalf("the shared token's state: status=%d body=%s, want 403 %s", session.Code, session.Body.String(), codeScopeRequired)
	}
}

func TestUserIssueStateSequenceRejectsStaleWrites(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)

	legacy := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"dismissed": []string{"legacy"},
	}, "alice")
	if legacy.Code != http.StatusOK {
		t.Fatalf("legacy state write: status=%d body=%s", legacy.Code, legacy.Body.String())
	}

	sequenced := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"dismissed": []string{"first"}, "seq": 1,
	}, "alice")
	if sequenced.Code != http.StatusOK {
		t.Fatalf("first sequenced state write: status=%d body=%s", sequenced.Code, sequenced.Body.String())
	}

	stale := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"dismissed": []string{"stale"}, "seq": 1,
	}, "alice")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale state write: status=%d body=%s", stale.Code, stale.Body.String())
	}
	conflict := decodeBody[struct {
		Code  string `json:"code"`
		State struct {
			Dismissed []string `json:"dismissed"`
			Seq       int64    `json:"seq"`
		} `json:"state"`
	}](t, stale)
	if conflict.Code != "STATE_STALE" || len(conflict.State.Dismissed) != 1 || conflict.State.Dismissed[0] != "first" || conflict.State.Seq != 1 {
		t.Fatalf("stale state conflict = %#v", conflict)
	}
}

func TestMessageMutationAppendsAndPublishesEvent(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]string{
		"body": "please review",
	}, "alice")
	if message.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", message.Code, message.Body.String())
	}
	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if !strings.Contains(log.Body.String(), `"type":"message.created"`) || !strings.Contains(log.Body.String(), `"notify":true`) {
		t.Fatalf("message event: status=%d body=%s", log.Code, log.Body.String())
	}
}

func TestResolveIssueRejectsMalformedNativeKey(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/resolve?ref=TEST-not-a-number", nil, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_ISSUE_REF"`) {
		t.Fatalf("resolve malformed key: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestArtifactUploadRecognizesParameterizedMarkdown(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "notes.md",
	}, "notes.md", "text/markdown; charset=utf-8", []byte("# Notes"), "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload parameterized Markdown: status=%d body=%s", response.Code, response.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact struct {
			Kind string `json:"kind"`
		} `json:"artifact"`
	}](t, response)
	if artifact.Artifact.Kind != "doc" {
		t.Fatalf("parameterized Markdown kind: got %q, want doc", artifact.Artifact.Kind)
	}
}

func TestArtifactSlugsIncludeExtensions(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	markdown := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "report.md",
	}, "report.md", "text/markdown", []byte("# Report"), "alice")
	if markdown.Code != http.StatusCreated {
		t.Fatalf("upload report.md: status=%d body=%s", markdown.Code, markdown.Body.String())
	}
	pdf := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "report.pdf",
	}, "report.pdf", "application/pdf", []byte("PDF"), "alice")
	if pdf.Code != http.StatusCreated {
		t.Fatalf("upload report.pdf: status=%d body=%s", pdf.Code, pdf.Body.String())
	}
	markdownArtifact := decodeBody[struct {
		Artifact struct {
			Slug string `json:"slug"`
		} `json:"artifact"`
	}](t, markdown)
	pdfArtifact := decodeBody[struct {
		Artifact struct {
			Slug string `json:"slug"`
		} `json:"artifact"`
	}](t, pdf)
	if markdownArtifact.Artifact.Slug != "report-md" || pdfArtifact.Artifact.Slug != "report-pdf" {
		t.Fatalf("artifact slugs: got Markdown %q and PDF %q, want report-md and report-pdf", markdownArtifact.Artifact.Slug, pdfArtifact.Artifact.Slug)
	}
}

func TestArtifactSlugsDisambiguateNormalizedNameCollisions(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	markdown := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "report.md",
	}, "report.md", "text/markdown", []byte("# Report"), "alice")
	if markdown.Code != http.StatusCreated {
		t.Fatalf("upload report.md: status=%d body=%s", markdown.Code, markdown.Body.String())
	}
	// A name of its own whose slug another artifact already holds gets the next free slug. A name
	// that is that slug is the artifact's own next version
	// (TestAnUploadNamedAfterAnArtifactsSlugIsItsNextVersion).
	dashed := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "Report.md",
	}, "Report.md", "application/octet-stream", []byte("report"), "alice")
	if dashed.Code != http.StatusCreated {
		t.Fatalf("upload Report.md: status=%d body=%s", dashed.Code, dashed.Body.String())
	}
	versioned := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name":         "report.md",
		"precondition": encodeUploadBase(t, uploadBaseNamed(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", "report.md")),
	}, "report.md", "text/markdown", []byte("# Revision"), "alice")
	if versioned.Code != http.StatusCreated {
		t.Fatalf("upload report.md version 2: status=%d body=%s", versioned.Code, versioned.Body.String())
	}
	first := decodeBody[struct {
		Artifact struct {
			Slug string `json:"slug"`
		} `json:"artifact"`
	}](t, markdown)
	second := decodeBody[struct {
		Artifact struct {
			Slug string `json:"slug"`
		} `json:"artifact"`
	}](t, dashed)
	version := decodeBody[struct {
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
	}](t, versioned)
	if first.Artifact.Slug != "report-md" || second.Artifact.Slug != "report-md-2" || version.Version.Number != 2 {
		t.Fatalf("artifact collision result: got slugs %q, %q and version %d; want report-md, report-md-2, 2", first.Artifact.Slug, second.Artifact.Slug, version.Version.Number)
	}
}

func TestConcurrentStatusPatchesUseCommittedPreimage(t *testing.T) {
	t.Parallel()
	// The seeded specs' settlement would lock the child row this test holds; keep it
	// out of the lock queue so only the two status patches are counted as waiters.
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	parentResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Parent",
	}, "alice")
	childResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Child", "parent": "TEST-1",
	}, "alice")
	parent := decodeBody[struct {
		Key string `json:"key"`
	}](t, parentResponse)
	child := decodeBody[struct {
		Key string `json:"key"`
	}](t, childResponse)
	if parentResponse.Code != http.StatusCreated || childResponse.Code != http.StatusCreated {
		t.Fatalf("create parent and child: parent=%d child=%d", parentResponse.Code, childResponse.Code)
	}

	lock, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin status lock: %v", err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if err := store.LockOwner(context.Background(), lock, store.IssueOwner(child.Key)); err != nil {
		t.Fatalf("take the child's owner lock: %v", err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]string{
			"status": "in_progress",
		}, "alice")
	}()
	waitForDatabaseLocks(t, lock, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+child.Key, map[string]string{
			"status": "done",
		}, "alice")
	}()
	waitForDatabaseLocks(t, lock, 2)
	if err := lock.Commit(context.Background()); err != nil {
		t.Fatalf("release child lock: %v", err)
	}
	for range 2 {
		response := awaitResponse(t, responses)
		if response.Code != http.StatusOK {
			t.Fatalf("concurrent status update: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	eventsResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+parent.Key+"/events", nil, "alice")
	var eventLog []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(eventsResponse.Body).Decode(&eventLog); err != nil {
		t.Fatalf("decode parent events: %v", err)
	}
	var transitions []struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	for _, event := range eventLog {
		if event.Type != "child.status" {
			continue
		}
		var transition struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if err := json.Unmarshal(event.Payload, &transition); err != nil {
			t.Fatalf("decode child status payload: %v", err)
		}
		transitions = append(transitions, transition)
	}
	if len(transitions) != 2 || transitions[0].From != "triage" || transitions[0].To != "in_progress" || transitions[1].From != "in_progress" || transitions[1].To != "done" {
		t.Fatalf("child status transition chain: got %#v, want triage→in_progress→done", transitions)
	}
}

func TestConcurrentPartialUserStateUpdatesPreserveFields(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	initial := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"pinned": false,
	}, "alice")
	if initial.Code != http.StatusOK {
		t.Fatalf("create state: status=%d body=%s", initial.Code, initial.Body.String())
	}

	// The project's owner lock, the one putUserState takes before it reads the row: both writes
	// queue behind it, and each reads the row only once the write before it has committed, so the
	// second keeps the field the first wrote.
	lock, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the project lock's holder: %v", err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if err := store.LockOwner(context.Background(), lock, store.ProjectOwner("TEST")); err != nil {
		t.Fatalf("hold the project's owner lock: %v", err)
	}
	responses := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
			"pinned": true,
		}, "alice")
	}()
	waitForDatabaseLocks(t, lock, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
			"last_read_seq": 7,
		}, "alice")
	}()
	waitForDatabaseLocks(t, lock, 2)
	if err := lock.Commit(context.Background()); err != nil {
		t.Fatalf("release the project's owner lock: %v", err)
	}
	for range 2 {
		response := awaitResponse(t, responses)
		if response.Code != http.StatusOK {
			t.Fatalf("concurrent user state update: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	stateResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/state", nil, "alice")
	state := decodeBody[map[string]userIssueState](t, stateResponse)
	if !state[issue.Key].Pinned || state[issue.Key].LastReadSeq != 7 {
		t.Fatalf("user state: got %#v, want pinned with last_read_seq 7", state[issue.Key])
	}
}

func TestIssueExternalLinksRequireAbsoluteHTTPURLs(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "External links",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	for _, address := range []string{"javascript:alert(1)", "file:///etc/passwd", "/relative"} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
			"external_links": []map[string]string{{"url": address}},
		}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_URL"`) {
			t.Fatalf("reject unsafe link %q: status=%d body=%s", address, response.Code, response.Body.String())
		}
	}
	valid := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"external_links": []map[string]string{{"url": "http://100.64.0.1:8766/issues/TEST-1"}},
	}, "alice")
	if valid.Code != http.StatusOK || !strings.Contains(valid.Body.String(), `"external_links":[{"url":"http://100.64.0.1:8766/issues/TEST-1","kind":"url"}]`) {
		t.Fatalf("accept HTTP external link: status=%d body=%s", valid.Code, valid.Body.String())
	}
}

func TestDocumentUploadIndexesDispatchReferences(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Document references",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	uploaded := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "notes.md",
	}, "notes.md", "text/markdown", []byte("See dispatch://TEST-1/artifact/spec."), "alice")
	artifact := decodeBody[struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}](t, uploaded)
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	var references int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'artifact' and to_id = 'TEST-1/spec'
	`, artifact.Artifact.ID).Scan(&references); err != nil {
		t.Fatalf("count uploaded document references: %v", err)
	}
	if references != 1 {
		t.Fatalf("uploaded document references = %d, want 1", references)
	}
}

func TestIssueDocumentCreationIndexesDispatchReferences(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Created references", "spec": "See dispatch://TEST-1/artifact/spec.",
	}, "alice")
	issue := decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	var references int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from refs
		where from_kind = 'artifact' and from_id = $1 and to_kind = 'artifact' and to_id = 'TEST-1/spec'
	`, issue.PrimaryArtifactID).Scan(&references); err != nil {
		t.Fatalf("count created document references: %v", err)
	}
	if references != 1 {
		t.Fatalf("created document references = %d, want 1", references)
	}
}

// cookieRequest sends one request to a newCookieServer handler as alice, with body as its JSON.
type cookieRequest func(method, target string, body any) *httptest.ResponseRecorder

// newCookieServer is a server whose people sign in with a session cookie, beside the shared agent
// bearer: the handler, the store's session table, which a test revokes, alice@d.example's cookie,
// and a request sent with it.
func newCookieServer(t *testing.T) (http.Handler, *store.PgSessionStore, *http.Cookie, cookieRequest) {
	t.Helper()
	handler, sessions, cookie, request, _ := newCookieServerOn(t, storetest.Open(t), 0)
	return handler, sessions, cookie, request
}

// newCookieServerOn is newCookieServer on database, with heartbeat as the API's StreamHeartbeat
// (zero leaves it unset), also returning the server's document service.
func newCookieServerOn(t *testing.T, database *store.Store, heartbeat time.Duration) (http.Handler, *store.PgSessionStore, *http.Cookie, cookieRequest, *docs.Service) {
	t.Helper()
	sessions := store.NewPgSessionStore(database.Pool)
	cookieIdentity := identity.CookieIdentity{SigningKey: testSigningKey, Sessions: sessions}
	broker := events.NewBroker()
	documentService := docs.New(docs.Deps{
		Store: database, Events: broker, Identity: cookieIdentity, Settle: time.Hour,
	})
	t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
	deps, err := NewDeps(DepsInput{
		Store: database, Identity: cookieIdentity, AgentTokens: sharedAgentTokens(t, "agent-token"),
		Docs: documentService, Events: broker, StreamHeartbeat: heartbeat,
	})
	if err != nil {
		t.Fatalf("new API dependencies: %v", err)
	}
	handler := http.NewServeMux()
	// Every server mounts GET /ws/live, the event feed's socket, which a revoked cookie is refused
	// too.
	t.Cleanup(Register(handler, t.Context(), deps).Wait)
	generation, err := sessions.EnsureSession(context.Background(), "alice@d.example")
	if err != nil {
		t.Fatalf("establish alice's session: %v", err)
	}
	// A session cookie is issued at sign-in, and a sign-in records the person (people.Record, as
	// the sign-in callback does), so alice is somebody who has signed in: a project she creates
	// defaults to her, and that default is held to the people table like every other.
	seedPeople(t, database, "alice@d.example")
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie("alice@d.example", generation, testSigningKey, true))
	if err != nil {
		t.Fatalf("parse session cookie: %v", err)
	}
	request := func(method, target string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload *bytes.Reader
		if body == nil {
			payload = bytes.NewReader(nil)
		} else {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("encode request: %v", err)
			}
			payload = bytes.NewReader(encoded)
		}
		req := httptest.NewRequest(method, target, payload)
		req.AddCookie(cookie)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	return handler, sessions, cookie, request, documentService
}

func TestRevokedCookieIsRejectedAcrossDispatchSurfaces(t *testing.T) {
	t.Parallel()
	handler, sessions, cookie, request := newCookieServer(t)
	if response := request(http.MethodPost, "/api/v1/projects", map[string]string{"key": "TEST", "name": "Test"}); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := request(http.MethodPost, "/api/v1/issues", map[string]string{"project": "TEST", "title": "Cookie issue"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)

	if err := sessions.RevokeSessions(context.Background(), "alice@d.example"); err != nil {
		t.Fatalf("revoke alice's sessions: %v", err)
	}
	response := request(http.MethodGet, "/api/v1/issues/"+issue.Key, nil)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"NO_IDENTITY"`) {
		t.Fatalf("revoked cookie issue read: status=%d body=%s", response.Code, response.Body.String())
	}
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	for _, path := range []string{"/ws/doc/" + issue.PrimaryArtifactID, "/ws/live"} {
		wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + path
		connection, response, err := dialLive(t, gws.DefaultDialer, wsURL, http.Header{"Cookie": []string{cookie.Name + "=" + cookie.Value}})
		if connection != nil {
			_ = connection.Close()
		}
		if err == nil || response == nil || (response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden) {
			t.Fatalf("revoked cookie websocket %s: response=%#v err=%v, want rejected handshake", path, response, err)
		}
	}
}

// The document socket takes a signed-in person's cookie and nothing else (INFRA-264). The shared
// agent token the HTTP API accepts is no credential for it: the handshake is refused, where the
// same room admits the cookie. A bearer once named its own session in a header the socket read,
// which made it a second, weaker door into Dispatch.
func TestTheDocumentSocketTakesACookieAndNotTheSharedAgentToken(t *testing.T) {
	t.Parallel()
	handler, _, cookie, request := newCookieServer(t)
	if response := request(http.MethodPost, "/api/v1/projects", map[string]string{"key": "TEST", "name": "Test"}); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := request(http.MethodPost, "/api/v1/issues", map[string]string{"project": "TEST", "title": "Document door"})
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/doc/" + issue.PrimaryArtifactID

	// The same token answers GET /api/v1/whoami as the shared agent, so the socket's refusal is
	// the socket's own and not an unknown value.
	whoami := httptest.NewRequest(http.MethodGet, "/api/v1/whoami", nil)
	whoami.Header.Set("Authorization", "Bearer agent-token")
	whoamiResponse := httptest.NewRecorder()
	handler.ServeHTTP(whoamiResponse, whoami)
	if whoamiResponse.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/whoami with the shared agent token: status=%d body=%s, want 200", whoamiResponse.Code, whoamiResponse.Body.String())
	}
	connection, response, err := dialLive(t, gws.DefaultDialer, wsURL, http.Header{"Authorization": []string{"Bearer agent-token"}})
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the document socket with the shared agent token: response=%#v err=%v, want a refused handshake (401)", response, err)
	}
	// The same room admits the person the browser signs in as, and a bearer beside that cookie is
	// no second credential: the person is who the connection is.
	for name, header := range map[string]http.Header{
		"the signed-in person's cookie":             {"Cookie": []string{cookie.Name + "=" + cookie.Value}},
		"that cookie beside the shared agent token": {"Cookie": []string{cookie.Name + "=" + cookie.Value}, "Authorization": []string{"Bearer agent-token"}},
	} {
		connection, response, err := dialLive(t, gws.DefaultDialer, wsURL, header)
		if err != nil {
			t.Fatalf("the document socket with %s: response=%#v err=%v", name, response, err)
		}
		_ = connection.Close()
	}
}

func TestArtifactUploadSummaryCreatesNamedVersions(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Named uploads", "before")
	for _, test := range []struct {
		name        string
		artifact    string
		contentType string
		content     []byte
	}{
		{name: "document", artifact: "notes.md", contentType: "text/markdown", content: []byte("# Notes")},
		{name: "blob", artifact: "data.pdf", contentType: "application/pdf", content: []byte("%PDF")},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
				"name": test.artifact, "summary": "Initial upload",
			}, test.artifact, test.contentType, test.content, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("upload artifact: status=%d body=%s", response.Code, response.Body.String())
			}
			upload := decodeBody[struct {
				Version model.Version `json:"version"`
			}](t, response)
			if !upload.Version.Named || upload.Version.Summary == nil || *upload.Version.Summary != "Initial upload" {
				t.Fatalf("upload version = %#v, want named version with summary", upload.Version)
			}
		})
	}
}

func TestQuiesceDocumentsHookIsGatedByTestHooksEnabled(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/_test/quiesce", nil, "alice")
	if response.Code != http.StatusNotFound {
		t.Fatalf("quiesce hook without TestHooksEnabled: status=%d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestJSONMutationsRequireApplicationJSON(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	body := `{"key":"TEST","name":"Test project"}`

	plain := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(body))
	plain.Header.Set("X-Dispatch-User", "alice")
	plain.Header.Set("Content-Type", "text/plain")
	plainResponse := httptest.NewRecorder()
	handler.ServeHTTP(plainResponse, plain)
	if plainResponse.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain mutation status = %d body=%s, want %d", plainResponse.Code, plainResponse.Body.String(), http.StatusUnsupportedMediaType)
	}

	jsonRequest := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(body))
	jsonRequest.Header.Set("X-Dispatch-User", "alice")
	jsonRequest.Header.Set("Content-Type", "application/json")
	jsonResponse := httptest.NewRecorder()
	handler.ServeHTTP(jsonResponse, jsonRequest)
	if jsonResponse.Code != http.StatusCreated {
		t.Fatalf("application/json mutation status = %d body=%s, want %d", jsonResponse.Code, jsonResponse.Body.String(), http.StatusCreated)
	}
}

func TestUnauthenticatedLargeIssueBodyIsRejectedBeforeDecoding(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues", strings.NewReader(strings.Repeat(" ", 8<<20)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated large body status = %d body=%s, want %d", response.Code, response.Body.String(), http.StatusUnauthorized)
	}
}

// The two waits below live here, in a file every build compiles, because tests built with and
// without -race both call them: the xid-window tests (built without -race only), and the
// read-only results and drive tests (built with it too).

// awaitSettledDocuments waits until no document owes a settlement and no backend has held an xid
// for a quiet spell, so a test that needed settlement to run first measures its request alone.
func awaitSettledDocuments(t *testing.T, database *store.Store) {
	t.Helper()
	awaitQuietDatabase(t, database, true)
}

// awaitQuietDatabase waits until no backend has held an xid for a quiet spell and, where settled
// asks it, no document owes a settlement either. A document of a closed issue owes its settlement
// until the issue reopens - a closed issue settles nothing, and the reopen arms what it owes
// (docs' settlement) - so a request made on a closed issue waits for the quiet alone. The quiet
// spell is what the measurement needs (no background writer beside it), not a bound: the wait ends
// only once the database has held quiet that long, and its one bound is the test binary's deadline
// (testwait.Until), whose failure says whether the last read found the database busy.
func awaitQuietDatabase(t *testing.T, database *store.Store, settled bool) {
	t.Helper()
	const quiet = 300 * time.Millisecond
	what := "the database to fall quiet (busy at the last read)"
	if settled {
		what = "documents to finish settling and the database to fall quiet (busy at the last read)"
	}
	var calm time.Time
	testwait.Until(t, what, func() (bool, bool) {
		var busy bool
		if err := database.Pool.QueryRow(context.Background(), `
			select ($1::boolean and exists (select 1 from doc_settlements_pending))
			    or exists (select 1 from pg_stat_activity
			               where datname = current_database() and backend_xid is not null and pid <> pg_backend_pid())
		`, settled).Scan(&busy); err != nil {
			t.Fatalf("read settlement state: %v", err)
		}
		switch {
		case busy:
			calm = time.Time{}
		case calm.IsZero():
			calm = time.Now()
		case time.Since(calm) >= quiet:
			return busy, true
		}
		return busy, false
	})
}
