package daemon

//ci:reads packages/contracts/fixtures/daemon-api/* TestTheStateFixturesAreTheDaemonsCompositionOfTheirSeeds (testgolden.Check, paths from apifixture.Path)
//ci:reads packages/daemon/internal/api/state_golden_test.go apiSettings (stringConstant parses apiGoldenFile, ../api/state_golden_test.go, for its settingsFixtureFile and minimalSettingsFile)

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/apifixture"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/cipher"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testgolden"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// updateStateFixtures is this package's `-update` flag: it writes the state fixtures the test
// below composes again.
var updateStateFixtures = flag.Bool("update", false, "rewrite the daemon-api state fixtures this package composes")

// apiGoldenFile is internal/api's file of goldens, which declares the two legion.yaml files the
// goldens load: settingsFixtureFile, the maximal kubernetes file, and minimalSettingsFile, the
// smallest tmux one.
var apiGoldenFile = filepath.Join("..", "api", "state_golden_test.go")

// apiSettings is the legion.yaml text that apiGoldenFile declares as the constant name, read from
// that file's source, so each state fixture is composed on the very file internal/api's settings
// tests hold the state's settings to. The settings text stays at its public path because the leak
// guard the pushes run, the shared public leak check, published by tag from a repository the
// deployment names (docs/kubernetes.md), honours no reviewed-lines row for a line that holds a
// key format marker (its leak-check/public_leak_check.py, method Reviewed.marker), and a copy
// would publish its credential-shaped lines at a new path. A read that fails fails the test,
// naming the file and the constant (stringConstant).
func apiSettings(t testing.TB, name string) string {
	t.Helper()
	text, err := stringConstant(apiGoldenFile, name)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// stringConstant is the value of the string constant name that the Go source file at path
// declares. The error names the file and the constant where the file does not parse, declares no
// such constant, or declares it as anything but one non-empty string literal: what the next
// reader sees is that the fixture moved, never an empty settings file.
func stringConstant(path, name string) (string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return "", fmt.Errorf("read the constant %s of %s: %w", name, path, err)
	}
	for _, decl := range file.Decls {
		declared, ok := decl.(*ast.GenDecl)
		if !ok || declared.Tok != token.CONST {
			continue
		}
		for _, spec := range declared.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range value.Names {
				if ident.Name != name {
					continue
				}
				var literal *ast.BasicLit
				if i < len(value.Values) {
					literal, _ = value.Values[i].(*ast.BasicLit)
				}
				if literal == nil || literal.Kind != token.STRING {
					return "", fmt.Errorf("%s declares the constant %s, but not as one string literal", path, name)
				}
				text, err := strconv.Unquote(literal.Value)
				if err != nil {
					return "", fmt.Errorf("unquote the constant %s of %s: %w", name, path, err)
				}
				if text == "" {
					return "", fmt.Errorf("%s declares the constant %s as the empty string", path, name)
				}
				return text, nil
			}
		}
	}
	return "", fmt.Errorf("%s declares no constant %s: the settings fixture moved, and this read must follow it", path, name)
}

// The two legion.yaml files every state fixture is composed on are read from internal/api's
// goldens, and the read is held to work: each constant is found and loads as the daemon loads it,
// the maximal file carries the planted credentials internal/api's settings tests hold the state's
// settings to strip (so the fixtures are composed over real ones), and a constant the file does
// not declare fails the read, naming the file and the constant.
func TestTheStateFixturesSettingsAreReadFromInternalAPIsGoldens(t *testing.T) {
	t.Parallel()
	maximal, minimal := apiSettings(t, "settingsFixtureFile"), apiSettings(t, "minimalSettingsFile")
	for _, planted := range []string{"BEGIN", "PRIVATE KEY", "fixture-dsn-password"} {
		if !strings.Contains(maximal, planted) {
			t.Errorf("settingsFixtureFile of %s holds no %q, which its settings tests plant", apiGoldenFile, planted)
		}
	}
	if !strings.Contains(minimal, "fixture-dsn-password") {
		t.Errorf("minimalSettingsFile of %s holds no planted DSN password", apiGoldenFile)
	}
	if cfg := apifixture.LoadSettings(t, maximal); cfg.Runtime.Name != "kubernetes" {
		t.Errorf("settingsFixtureFile loads under runtime %q, want kubernetes", cfg.Runtime.Name)
	}
	if cfg := apifixture.LoadSettings(t, minimal); cfg.Project != "LEGION" {
		t.Errorf("minimalSettingsFile loads project %q, want LEGION", cfg.Project)
	}
	const missing = "noSuchSettingsFile"
	_, err := stringConstant(apiGoldenFile, missing)
	if err == nil || !strings.Contains(err.Error(), apiGoldenFile) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("the read of a constant %s does not declare answered %v, want an error naming the file and the constant", apiGoldenFile, err)
	}
}

// The state fixtures are the daemon's own answer over a seeded record. Each case seeds a cloned
// database through the stores' own writers, builds this daemon's state source over it as serve
// builds it (stateSource), over a supervision of the stored claims and capability reports as
// openSupervision builds one (newSupervision) and the workflow's own admission (workflowAdmission),
// and asks GET /legion/v1/state of the API server over that source as `legion state` asks it
// (served). So each fixture is what (*source).State, the deployment as
// supervision.deploymentWith states it and the state route's own reads beside its source answer
// over the seed: a value no writer accepts fails the seed, a field the source adds, drops or
// moves moves the fixture, and a value no rule of the daemon serves over the seed cannot reach one.
//
// Each fixture is the body as the route serves it to a caller at this daemon's own contract (what
// served answers), with 0 in place of `daemon.schemaVersion` (withNoSchemaVersion) and less the
// route's `contract` mark (api's stateFor; apifixture.WithoutContractMarks), both taken off here,
// in the test, before the fixture is compared or written. Both numbers of the mark name this
// daemon's own contract, which is LastNumberedContract plus the build's count of entries, so a
// fixture that held it would be an edit of every contract change. The marks taken off are held to
// that number instead, and the plugin's schema reads the mark as optional. The served schema
// version is held to the newest migration the store embeds, for the same reason at every
// migration. internal/api's populatedState reads state.json back for that package's goldens, and
// every route that package serves the state through writes the mark again.
func TestTheStateFixturesAreTheDaemonsCompositionOfTheirSeeds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, file string
		seed       func(s *seeding) composition
	}{
		{name: "populated", file: "state.json", seed: seedPopulated},
		{name: "paused", file: "state-paused.json", seed: seedPaused},
		{name: "stage3", file: "state-stage3.json", seed: seedStage3},
		{name: "operator-claim", file: "state-operator-claim.json", seed: seedOperatorClaim},
		{name: "last-pause", file: "state-last-pause.json", seed: seedLastPause},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSeeding(t)
			body, taken := apifixture.WithoutContractMarks(t, withNoSchemaVersion(t, s.served(tc.seed(s))), "daemon", "caller")
			if own := api.DaemonAPIVersion(); !maps.Equal(taken, map[string]int{"daemon": own, "caller": own}) {
				t.Fatalf("the state route marked the body %v, want the daemon and the caller mark both at %d, the marks of a caller at this daemon's own contract", taken, own)
			}
			testgolden.Check(t, apifixture.Path(tc.file), json.RawMessage(body), *updateStateFixtures, apifixture.StateFixturesRewrite)
		})
	}
}

// composition is what the daemon of one state holds beside the seeded store: its configuration,
// the one settings file, and what it knows in memory alone, which no store holds and so no seed
// can write. Each field names the place the daemon keeps it, which is where served puts it.
type composition struct {
	cfg config.Config
	// now is the daemon's one clock at the read (plan.clock), which the source, the workflow
	// engine and the admission read (source.now, workflow.Config.Clock, admit's UseClock).
	now time.Time
	// startedAt is when this daemon process started (source.startedAt), which its boot row also
	// records.
	startedAt time.Time
	// wakes are the launches the pacing holds in the machines (supervision.waitingWakes). A
	// machine holds one only inside a wave, and none restored from the store does, so this is one
	// of the two inputs of the source served gives by hand.
	wakes []supervise.WaitingWake
	// dispatchTokens are the Sandbox runtime's counts of the Dispatch tokens its launches handed
	// out (sandbox.Runtime.DispatchTokens), which the source reads under dispatch_token_broker
	// alone (dispatchTokensOf); nil is a deployment without the setting, whose state carries no
	// `dispatchCredential`. The runtime counts them in memory as it launches, and the composition
	// runs no runtime, so this is the other input served gives by hand.
	dispatchTokens *sandbox.DispatchTokens
	// secretsIssued is whether the daemon's broker login holds a credential (plan.secretsLogin,
	// issuedSecretsLogin); false is a deployment that enrolls no pod, whose daemon holds no login.
	secretsIssued bool
	// probed and image are what the worker image's probe reported (supervision.probed and
	// imageReport): whether it passed, the model-fallback mark it read, and how many of its
	// attempts waited on the pool's capacity, with the scheduler's reason.
	probed bool
	image  bootprobe.ImageReport
	// caches are the runtime's repository caches as their last health read found them
	// (supervision.cacheStatuses); nil is a runtime that keeps none.
	caches []sandbox.CacheStatus
	// repositories is the repository state (plan.repositories), holding what GitHub and Dispatch
	// last answered of each name as the refresher's writers keep it (repositories.keepRead,
	// keepWebhookAnswer); nil is the boot's own, which holds no answer yet.
	repositories *repositories
	// consumers is the durable consumers' lag as NATS answers it after the state's commit
	// (api.Options.ConsumerLag); nil is a daemon with no workflow, which serves none.
	consumers []intake.ConsumerLag
}

// served is GET /legion/v1/state of the daemon c describes over s's store, asked as `legion state`
// asks it: a caller in no pod, stating no contract, which the route serves the body this
// daemon's own contract writes (api's contractsFor).
func (s *seeding) served(c composition) []byte {
	s.t.Helper()
	cfg, log := c.cfg, quietLogger()
	projectToken, err := claim.ProjectToken(cfg.Project)
	s.must("the project token of "+cfg.Project, err)
	// What the boot reads of the store before it serves (openSupervision): the project's claims,
	// each restored to a machine of its own, and its sessions' capability reports.
	claims, err := s.st.Claims(s.ctx)
	s.must("the claims", err)
	claims = ofProject(claims, projectToken)
	reports, err := s.st.CapabilityReports(s.ctx)
	s.must("the capability reports", err)
	clock := &heldClock{at: c.now}
	repos := c.repositories
	if repos == nil {
		repos = newRepositories(cfg.Project, cfg.RepositoryRenames)
	}
	p := plan{project: projectToken, clock: clock, repositories: repos}
	if c.secretsIssued {
		p.secretsLogin = issuedSecretsLogin(s.t)
	}
	// The supervision the boot builds over them (newSupervision, as openSupervision builds it),
	// with no runtime: the composition runs none, and gives the repository caches and the Dispatch
	// token counts by hand. It holds no refresh gate, which openSupervision adds for the run path and
	// the state route never reads. The image probe's report is set after, as run sets it.
	supervising := newSupervision(cfg, log, p, nil, supervisorOf(s.t, claims...), claims, reports)
	supervising.imageReport, supervising.probed = c.image, c.probed
	if c.caches != nil {
		supervising.cacheStatuses = func() []sandbox.CacheStatus { return c.caches }
	}
	// The workflow's records, engine and admission (openWorkflow), over the seed's own record store:
	// the admission's reading is the state's architect-stalled arms.
	records := projectRecords{Store: s.records, project: cfg.Project}
	engineCfg := engineConfig(cfg, "", nil)
	engineCfg.Clock = clock.Now
	admission := workflowAdmission(records, workflow.New(records, engineCfg, log), cfg, clock.Now, log)
	view := stateSource(cfg, s.st, records, c.startedAt, supervising, p, admission.ArchitectStallArms)
	view.wakes = func() []supervise.WaitingWake { return c.wakes }
	if c.dispatchTokens != nil {
		if !cfg.DispatchTokenBroker {
			s.t.Fatal("the seed gives Dispatch token counts to a daemon without dispatch_token_broker, which reads none (dispatchTokensOf)")
		}
		tokens := *c.dispatchTokens
		view.dispatchTokens = func() sandbox.DispatchTokens { return tokens }
	}
	opts := api.Options{State: view, StateTransactions: s.st.Pool(), Steps: cfg.Gates.Steps(), Log: log}
	if c.consumers != nil {
		opts.ConsumerLag = func(context.Context) []intake.ConsumerLag { return c.consumers }
	}
	recorder := httptest.NewRecorder()
	api.NewServer("127.0.0.1", 0, opts).Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil))
	if recorder.Code != http.StatusOK {
		s.t.Fatalf("GET /legion/v1/state = %d %s, want 200", recorder.Code, recorder.Body)
	}
	return recorder.Body.Bytes()
}

// withNoSchemaVersion is body, the state route's answer, with 0 written where it names the
// store's schema version (api.DaemonInfo.SchemaVersion). The route serves the version the clone's
// store reads (store.Store.SchemaVersionTx), which every migration added moves; a fixture that
// held it would go stale at every migration and conflict as text between any two pull requests
// that add one. So the served version is held here to the newest migration the store embeds
// (migrations.All), and the fixture holds 0 in its place, as apifixture.WithoutContractMarks
// takes off the marks of this daemon's own contract.
func withNoSchemaVersion(t testing.TB, body []byte) []byte {
	t.Helper()
	var served struct {
		Daemon struct {
			SchemaVersion int64 `json:"schemaVersion"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode the state body's schema version: %v", err)
	}
	all, err := migrations.All()
	if err != nil || len(all) == 0 {
		t.Fatalf("read the migrations the store embeds: %d of them, %v", len(all), err)
	}
	if newest := int64(all[len(all)-1].Version); served.Daemon.SchemaVersion != newest {
		t.Fatalf("the state serves daemon.schemaVersion %d, want %d, the newest migration the store embeds",
			served.Daemon.SchemaVersion, newest)
	}
	version := []byte(`"schemaVersion":` + strconv.FormatInt(served.Daemon.SchemaVersion, 10))
	if n := bytes.Count(body, version); n != 1 {
		t.Fatalf("the state body names %s %d times, want once", version, n)
	}
	return bytes.Replace(body, version, []byte(`"schemaVersion":0`), 1)
}

// issuedSecretsLogin is the daemon's broker login holding a credential, logged in as the boot's
// login loop logs in (cipher.Client.Login) at a stand-in of the broker's token route that issues
// one for an hour of the wall clock, the clock the login's state is read on (LoginStatus).
func issuedSecretsLogin(t *testing.T) *cipher.Client {
	t.Helper()
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/launcher-credentials/token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"credential_id": "cred-fixture", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(broker.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("the-daemon's-token\n"), 0o600); err != nil {
		t.Fatalf("write the token file: %v", err)
	}
	login := &cipher.Client{URL: broker.URL, HTTP: broker.Client(), TokenFile: tokenFile, Log: quietLogger()}
	if _, err := login.Login(context.Background()); err != nil {
		t.Fatalf("log in at the stand-in broker: %v", err)
	}
	if state := login.LoginStatus().State; state != "issued" {
		t.Fatalf("the broker login reads %q, want issued", state)
	}
	return login
}

// seeding is one cloned database of this package's template, written through the stores' own
// writers, and the one record store every writer of the seed writes through and the state reads.
type seeding struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	records *record.Postgres
}

// newSeeding is a seeding over a clone of this package's template (testpg.CloneStore), which
// skips the test where LEGION_TEST_PG_DSN names no server. Under -update it fails there instead:
// internal/api's goldens are written from state.json after this test writes it
// (apifixture.StateFixturesRewrite), so a skip that exits 0 would leave them written from the old
// state.json, and the rewrite would look done.
func newSeeding(t *testing.T) *seeding {
	t.Helper()
	if *updateStateFixtures && os.Getenv("LEGION_TEST_PG_DSN") == "" {
		t.Fatal("-update composes the state fixtures over a seeded database, and none can be cloned, so it would write nothing: " + testdb.SkipMessage)
	}
	return &seeding{t: t, ctx: context.Background(), st: testpg.CloneStore(t, "legion_state_fixtures"), records: record.NewStore()}
}

// in runs write in one transaction of the store, as the workflow's own writes run (store.Transact).
func (s *seeding) in(what string, write func(ctx context.Context, tx pgx.Tx) error) {
	s.t.Helper()
	if err := store.Transact(s.ctx, s.st.Pool(), write); err != nil {
		s.t.Fatalf("seed %s: %v", what, err)
	}
}

// must fails the seed where a writer of the store refused what it was given.
func (s *seeding) must(what string, err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatalf("seed %s: %v", what, err)
	}
}

// token is the claim token of role on issue in project.
func (s *seeding) token(project, issue string, role claim.Role) claim.Token {
	s.t.Helper()
	projectToken, err := claim.ProjectToken(project)
	s.must("the project token of "+project, err)
	token, err := claim.NewToken(projectToken, issue, role)
	s.must("the token of "+issue+" "+string(role), err)
	return token
}

// recorded is one issue record and what Dispatch said of it, as admission records an issue from
// the reading that handed it over (admit's record, admit.go): the record, its mirror row
// (blockers.Mirror's write of the same reading), and the moment the daemon last saw it move
// (record.Store.MarkIssueMoved, the one writer of that column).
type recorded struct {
	issue  record.Issue
	mirror record.DispatchReading
	moved  time.Time
}

// putRecorded writes each of rs: the mirror row first, then the record and its movement.
func (s *seeding) putRecorded(rs ...recorded) {
	s.t.Helper()
	for _, r := range rs {
		s.in("the record of "+r.issue.Key, func(ctx context.Context, tx pgx.Tx) error {
			if err := s.records.PutDispatchIssue(ctx, tx, r.mirror); err != nil {
				return err
			}
			if err := s.records.PutIssue(ctx, tx, r.issue); err != nil {
				return err
			}
			return s.records.MarkIssueMoved(ctx, tx, r.issue.Key, r.moved)
		})
	}
}

// mirrorOnly writes what Dispatch says of an issue the daemon keeps no record of.
func (s *seeding) mirrorOnly(reading record.DispatchReading) {
	s.t.Helper()
	s.in("the mirror row of "+reading.Key, func(ctx context.Context, tx pgx.Tx) error {
		return s.records.PutDispatchIssue(ctx, tx, reading)
	})
}

// openTrees opens each tree's lifecycle under the workflow, as admission's promotion does
// (record.Store.OpenTreeLifecycle in the fact's transaction).
func (s *seeding) openTrees(project string, trees ...string) {
	s.t.Helper()
	projectToken, err := claim.ProjectToken(project)
	s.must("the project token", err)
	for _, tree := range trees {
		s.in("the lifecycle of "+tree, func(ctx context.Context, tx pgx.Tx) error {
			_, err := s.records.OpenTreeLifecycle(ctx, tx, projectToken, tree, treelifecycle.AuthorityWorkflow)
			return err
		})
	}
}

// claimAs admits c's claim to its tree's open lifecycle (store.Store.AdmitClaim, the one way a
// claim enters the store) and then writes it as its machine last persisted it (store.Store.PutClaim,
// or PutClaimAndDelivery where it holds a pending task), with the step the stream last reported
// (store.Store.PutClaimStep).
func (s *seeding) claimAs(c supervise.Claim) supervise.Claim {
	s.t.Helper()
	admitted, err := s.st.AdmitClaim(s.ctx, supervise.Claim{Token: c.Token, Project: c.Project, Tree: c.Tree, Issue: c.Issue,
		Role: c.Role, Generation: c.Generation, State: supervise.StateQueued})
	s.must("the admission of claim "+string(c.Token), err)
	c.TreeEpoch = admitted.TreeEpoch
	if pending := c.Pending; pending != nil {
		c.Pending = nil
		s.must("the claim "+string(c.Token)+" and its task", s.st.PutClaimAndDelivery(s.ctx, c, *pending))
		c.Pending = pending
	} else {
		s.must("the claim "+string(c.Token), s.st.PutClaim(s.ctx, c))
	}
	if !c.Step.At.IsZero() {
		known, err := s.st.PutClaimStep(s.ctx, c.Token, c.Step)
		s.must("the step of claim "+string(c.Token), err)
		if !known {
			s.t.Fatalf("seed the step of claim %s: the store holds no such claim", c.Token)
		}
	}
	return c
}

// enqueue queues payload for issue due at nextAt (record.Store.Enqueue), as the writer of that
// row does, and answers the row's id.
func (s *seeding) enqueue(issue string, payload record.OutboxPayload, nextAt time.Time) int64 {
	s.t.Helper()
	row, err := record.NewOutboxRow(issue, payload, nextAt)
	s.must("the outbox row of "+issue, err)
	var id int64
	s.in("the outbox row of "+issue, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = s.records.EnqueueID(ctx, tx, row)
		return err
	})
	return id
}

// lease takes row id's lease at at for leaseFor, as a lane claims a row it runs
// (record.Store.ClaimRow), and answers the leased row.
func (s *seeding) lease(id int64, at time.Time, leaseFor time.Duration) record.OutboxRow {
	s.t.Helper()
	var row record.OutboxRow
	s.in("the lease of outbox row", func(ctx context.Context, tx pgx.Tx) error {
		leased, ok, err := s.records.ClaimRow(ctx, tx, id, at, leaseFor)
		if err != nil {
			return err
		}
		if !ok {
			s.t.Fatalf("seed the lease of outbox row %d: no lane may run it at %s", id, at)
		}
		row = leased
		return nil
	})
	return row
}
