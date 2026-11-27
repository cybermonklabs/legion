package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
)

// priority_plans_test.go holds the priorities read to the work of the plans Postgres makes for
// its statements' own arguments, on every run of one connection (INFRA-5362).
//
// The server's pool prepares every statement it sends (pgx's QueryExecModeCacheStatement), so a
// connection runs a statement's first speedCustomPlans runs under plans made for their arguments
// and may run every later one under the one generic plan Postgres then makes and keeps. The
// statements of the priorities read take the relations' seed as two arrays. While the seed was
// read through unnest, whose rows a generic plan sizes at 10 whatever the arrays hold, the four
// statements over it touched 6.7 to 45.8 times the shared buffers of the plans made for their
// arguments under the generic plan, which the held connection ran from its sixth run on
// (PostgreSQL 18.6, 2026-11-22); priorityRelationSeed says how the seed is read now. The test reads
// the plans a connection really ran, where the speed tests (priority_speed_test.go) count each
// statement under a plan they force.

// planRuns is how many times the test runs each priorities read on the held connection: the
// speedCustomPlans runs Postgres plans for their own arguments, then the runs it may answer with
// the generic plan it keeps from the sixth on.
const planRuns = 10

// planBuffersRatio is the most shared buffers a statement of the read may touch on the held
// connection, against the same statement on the same data under plan_cache_mode =
// force_custom_plan (INFRA-5362's R1). On this corpus the plans made for the statements'
// arguments touch the same count on every run but one whose backend's caches are cold, which
// touches up to three buffers more, and the generic plans of the seed read through unnest touched
// 6.7 to 45.8 times as many, so twice the larger of the two counts stands well between them.
const planBuffersRatio = 2

// TestPrioritiesReadKeepsItsPlans runs the priorities read, the whole body and the summary, ten
// times on one held connection of the server's own pool, through the server's own read
// (readPriorities), and holds every statement of every run to at most planBuffersRatio times the
// shared buffers, hit plus read, that the same statement touches under plan_cache_mode =
// force_custom_plan on a connection of the read's own, the larger of its counts there with the
// backend's caches cold and warm (priorityPlanBaseline). It holds every run's answer, as the route
// encodes it, to be the baseline's byte for byte.
//
// The plans are the ones the connections ran, read from the notices auto_explain sends on them
// (explainedStatements), so the reading adds no run of a statement and changes no plan choice: the
// held connection's plan cache is the one a deployment's connections build. auto_explain is loaded
// into each connection's session (LOAD) and set there, which only a superuser may do: a test server
// whose login is not one skips the test, naming SQLSTATE 42501 and the login, and fails it on
// GitHub Actions, whose login is one (explainingConnection). auto_explain reports each statement's
// top plan node, whose buffer counts cover the nodes below it.
//
// A run that reports a statement the baseline did not, misses one, or reports from another
// connection fails, so a run that read nothing cannot pass, and each run's counts are printed.
func TestPrioritiesReadKeepsItsPlans(t *testing.T) {
	// Not parallel: it holds shared-buffer counts of its own statements to a ratio, and the
	// shared buffers and their caching are the whole Postgres server's, which the package's
	// parallel tests use at the same time; it runs alone, as it did on main.
	corpus := seedSpeedCorpus(t)
	database := corpus.server.deps.Store

	heldPlans := &explainedStatements{}
	heldStore, heldBackend := explainingConnection(t, database, heldPlans)
	heldDeps := corpus.server.deps
	heldDeps.Store = heldStore
	heldServer := directServer(heldDeps)

	reads := []struct {
		name    string
		summary bool
	}{
		{"the whole priorities body", false},
		{"the priorities summary", true},
	}
	baselines := make([]priorityPlanRun, len(reads))
	for index, read := range reads {
		baseline := priorityPlanBaseline(t, corpus.server.deps, read.name, read.summary)
		total := 0.0
		for _, statement := range baseline.statements {
			total += statement.buffers
		}
		if len(baseline.statements) == 0 || total == 0 {
			t.Fatalf("%s under force_custom_plan reported %d statements touching %.0f shared buffers: auto_explain reported no plan, so nothing would be held",
				read.name, len(baseline.statements), total)
		}
		baselines[index] = baseline
		t.Logf("%s answers %d bytes, sha256 %x; under force_custom_plan its %d statements touch at most %s shared buffers",
			read.name, len(baseline.body), sha256.Sum256(baseline.body), len(baseline.statements), baseline.counts())
		for number, statement := range baseline.statements {
			t.Logf("  statement %d of %s: %.70s…", number+1, read.name, strings.Join(strings.Fields(statement.text), " "))
		}
	}

	for run := 1; run <= planRuns; run++ {
		for index, read := range reads {
			baseline := baselines[index]
			held := readPriorityPlans(t, heldServer, heldPlans, heldBackend, read.summary)
			ordered, unmatched := held.inOrderOf(baseline)
			if len(unmatched) > 0 || len(held.statements) != len(baseline.statements) {
				t.Errorf("run %d of %s reported %d statements, want the %d the baseline reported; %d of them are not the baseline's: %q",
					run, read.name, len(held.statements), len(baseline.statements), len(unmatched), unmatched)
				continue
			}
			t.Logf("run %2d of %s: %s shared buffers, against at most %s under force_custom_plan",
				run, read.name, ordered.counts(), baseline.counts())
			for number, statement := range ordered.statements {
				allowed := planBuffersRatio * baseline.statements[number].buffers
				if statement.buffers > allowed {
					t.Errorf("run %d of %s: statement %d (%.60s…) touched %.0f shared buffers, %.1fx the %.0f it touches at most under force_custom_plan, want at most %dx (%.0f)",
						run, read.name, number+1, strings.Join(strings.Fields(statement.text), " "), statement.buffers,
						statement.buffers/baseline.statements[number].buffers, baseline.statements[number].buffers, planBuffersRatio, allowed)
				}
			}
			if !bytes.Equal(held.body, baseline.body) {
				t.Errorf("run %d of %s answered %d bytes, sha256 %x, want the baseline's %d bytes, sha256 %x, byte for byte",
					run, read.name, len(held.body), sha256.Sum256(held.body), len(baseline.body), sha256.Sum256(baseline.body))
			}
		}
	}
}

// priorityPlanRun is one priorities read on one connection: its answer as the route encodes it,
// and each statement it ran as auto_explain reported it, in the order the reports arrived.
type priorityPlanRun struct {
	body       []byte
	statements []explainedStatement
}

// counts is the run's shared buffer counts, one a statement, as the test prints them.
func (r priorityPlanRun) counts() string {
	counts := make([]string, len(r.statements))
	for index, statement := range r.statements {
		counts[index] = fmt.Sprintf("%.0f", statement.buffers)
	}
	return "[" + strings.Join(counts, " ") + "]"
}

// inOrderOf answers r's statements in the order of baseline's, matched by their text, and the
// text of every statement of r that matches none of baseline's or one already matched. Matching
// by text rather than by position leaves the test free of the order the notices arrive in.
func (r priorityPlanRun) inOrderOf(baseline priorityPlanRun) (priorityPlanRun, []string) {
	ordered := priorityPlanRun{body: r.body, statements: make([]explainedStatement, len(baseline.statements))}
	matched := make([]bool, len(baseline.statements))
	var unmatched []string
	for _, statement := range r.statements {
		found := false
		for index, wanted := range baseline.statements {
			if !matched[index] && wanted.text == statement.text {
				ordered.statements[index], matched[index], found = statement, true, true
				break
			}
		}
		if !found {
			unmatched = append(unmatched, fmt.Sprintf("%.60s…", strings.Join(strings.Fields(statement.text), " ")))
		}
	}
	return ordered, unmatched
}

// readPriorityPlans runs one priorities read through served, whose pool is one connection, and
// answers the body the route would write and the plans that connection reported for it. A plan
// reported by another server process fails the test: the plan cache it reads is that one
// connection's.
func readPriorityPlans(t *testing.T, served *server, plans *explainedStatements, backend uint32, summary bool) priorityPlanRun {
	t.Helper()
	plans.take(t)
	// The route reads with an empty list when the request names no project, never nil, which
	// pgx would send as null.
	body, err := served.readPriorities(context.Background(), []string{}, summary)
	if err != nil {
		t.Fatalf("read the priorities (summary %t): %v", summary, err)
	}
	response := httptest.NewRecorder()
	WriteJSON(response, http.StatusOK, body)
	run := priorityPlanRun{body: response.Body.Bytes(), statements: plans.take(t)}
	for _, statement := range run.statements {
		if statement.backend != backend {
			t.Fatalf("a plan of the read came from server process %d, want its connection's %d", statement.backend, backend)
		}
	}
	return run
}

// priorityPlanBaseline is what one priorities read's statements touch under plan_cache_mode =
// force_custom_plan: the read run twice on a connection of its own, first with the backend's caches
// cold and then warm, and each statement's larger count of the two. A held run can meet either: a
// busy server resets the caches of a backend that falls behind its shared invalidation queue, and
// that backend's next statement runs as a connection's first does, up to three buffers more (the
// summary's first statement touched 4 after a reset, against 1 warm, on PostgreSQL 18.6). The two
// runs must report the same statements and answer the same bytes.
func priorityPlanBaseline(t *testing.T, deps Deps, name string, summary bool) priorityPlanRun {
	t.Helper()
	plans := &explainedStatements{}
	baselineStore, backend := explainingConnection(t, deps.Store, plans, "set plan_cache_mode = force_custom_plan")
	deps.Store = baselineStore
	served := directServer(deps)
	cold := readPriorityPlans(t, served, plans, backend, summary)
	warm := readPriorityPlans(t, served, plans, backend, summary)
	ordered, unmatched := warm.inOrderOf(cold)
	if len(unmatched) > 0 || len(warm.statements) != len(cold.statements) {
		t.Fatalf("%s under force_custom_plan reported %d statements with its caches warm and %d with them cold; %d of them are not the cold run's: %q",
			name, len(warm.statements), len(cold.statements), len(unmatched), unmatched)
	}
	if !bytes.Equal(warm.body, cold.body) {
		t.Fatalf("%s under force_custom_plan answered %d bytes, sha256 %x, with its caches warm, and %d bytes, sha256 %x, with them cold",
			name, len(warm.body), sha256.Sum256(warm.body), len(cold.body), sha256.Sum256(cold.body))
	}
	t.Logf("%s under force_custom_plan touches %s shared buffers with its caches cold and %s warm", name, cold.counts(), ordered.counts())
	for index, statement := range ordered.statements {
		cold.statements[index].buffers = max(cold.statements[index].buffers, statement.buffers)
	}
	return cold
}

// explainedStatements are the plans auto_explain sends as notices on the connections it is the
// notice handler of: each statement's text, the server process that ran it, and the shared buffers
// its top plan node touched, hit plus read. Its notice method is the pgconn.NoticeHandler; a plan
// it cannot read is kept as a fault, which take fails the test on, since a notice handler runs
// inside pgx's read of the connection and cannot stop the test itself.
type explainedStatements struct {
	mu         sync.Mutex
	statements []explainedStatement
	faults     []string
}

// explainedStatement is one statement auto_explain reported.
type explainedStatement struct {
	backend uint32
	text    string
	buffers float64
}

// notice keeps the plan an auto_explain notice carries: `duration: <ms> ms  plan:` and, under
// auto_explain.log_format = json, one JSON object holding the statement's text and its plan. Any
// other notice is not a plan and is passed over.
func (e *explainedStatements) notice(connection *pgconn.PgConn, notice *pgconn.Notice) {
	_, plan, found := strings.Cut(notice.Message, " plan:\n")
	if !strings.HasPrefix(notice.Message, "duration: ") || !found {
		return
	}
	var explained struct {
		Text string       `json:"Query Text"`
		Plan analyzedNode `json:"Plan"`
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := json.Unmarshal([]byte(plan), &explained); err != nil {
		e.faults = append(e.faults, fmt.Sprintf("read the plan of a notice (%v): %.200s", err, notice.Message))
		return
	}
	e.statements = append(e.statements, explainedStatement{
		backend: connection.PID(),
		text:    explained.Text,
		buffers: explained.Plan.SharedHitBlocks + explained.Plan.SharedReadBlocks,
	})
}

// take answers the statements reported since the last take, and fails the test on any plan it
// could not read.
func (e *explainedStatements) take(t *testing.T) []explainedStatement {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.faults) > 0 {
		t.Fatalf("auto_explain sent plans the test could not read: %q", e.faults)
	}
	statements := e.statements
	e.statements = nil
	return statements
}

// explainingConnection opens a pool of one connection from database's own pool configuration,
// so every read on it runs on that connection through the statement cache a request's does, with
// plans as its notice handler; loads auto_explain into that connection's session with every
// statement reported at notice level, analyzed with its buffers, as JSON; runs settings after; and
// answers the store and the connection's server process. Loading auto_explain and setting it are
// a superuser's: a login that may not (SQLSTATE 42501, insufficient_privilege), such as the
// NOSUPERUSER role of a Legion pod's test server (legion-testpg), skips the test naming the login
// and the statement, and fails it on GitHub Actions, whose login is a superuser
// (testdb.SkipOutsideCI).
func explainingConnection(t *testing.T, database *store.Store, plans *explainedStatements, settings ...string) (*store.Store, uint32) {
	t.Helper()
	ctx := context.Background()
	explaining := storetest.Reopened(t, database, func(config *pgxpool.Config) {
		config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
		config.ConnConfig.OnNotice = plans.notice
	})
	connection, err := explaining.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("take the connection the plans are read on: %v", err)
	}
	defer connection.Release()
	for _, statement := range append([]string{
		"load 'auto_explain'",
		"set auto_explain.log_min_duration = 0",
		"set auto_explain.log_analyze = on",
		"set auto_explain.log_buffers = on",
		"set auto_explain.log_format = json",
		"set auto_explain.log_level = notice",
	}, settings...) {
		if _, err := connection.Exec(ctx, statement); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42501" {
				testdb.SkipOutsideCI(t, "%s on the connection the plans are read on: the test server's login %q may not (SQLSTATE 42501); the plans this test holds are the ones auto_explain reports, which only a superuser's session can turn on: %v",
					statement, explaining.Pool.Config().ConnConfig.User, err)
			}
			t.Fatalf("%s on the connection the plans are read on: %v", statement, err)
		}
	}
	return explaining, connection.Conn().PgConn().PID()
}
