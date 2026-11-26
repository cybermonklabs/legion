//go:build basebuild

package api

// This tree's check c-read-cost: no read this tree moves gets slower, and no event outruns its node.
//
// The test is TestNoReadThisTreeMovesGetsSlower at the end of this file; everything above it is
// the corpus it writes, the two servers it boots and the six reads it measures, with two more old
// sides of the tree read beside it: the old page's first load, and that load with the page's
// Verification, Plan and Decisions tabs opened after it. It carries the basebuild
// tag, as cmd/dispatch's store_census_test.go does, because it builds the server of an earlier
// commit (testbaseline.Build), which needs a checkout that holds that commit; no CI job builds
// the tag. It stands in this package, outside cmd/dispatch, because its last step runs
// TestNoEventOutrunsItsNode, which holds the server's own comment write at a seam only this
// package's tests can set (server.beforeCommentCommit). It takes from 1 to 3 hours and is run on
// its own, by name, as
//
//	BASELINE_SHA=<the commit the pull request body's BASELINE_SHA= line names> go test -tags basebuild ./internal/dispatch/api/ -run '^TestNoReadThisTreeMovesGetsSlower$' -timeout 170m -count=1 -v
//
// from packages/envoy, with DISPATCH_TEST_DATABASE_URL naming a Postgres server it may create a
// database on. READ_COST_READS, for diagnosis, names the arms to measure (readCostArmFilter),
// READ_COST_KEEP_DATABASE=1, for diagnosis, keeps the run's database when the test ends
// (readCostKeepDatabase), and READ_COST_PPROF=<arm>, for diagnosis, takes a CPU profile of this
// head's server over that arm's new-side readings (readCostPprofArm); a run of the check leaves
// all three unset.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testbaseline"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The sampling, as inbox_dismissed_scale_test.go takes it and widened to p90 and p99: one warm-up
// reading that is not counted, then readCostRuns readings of each read on each side. p99 by
// nearest rank is the ceil(0.99 x n)th reading, so at 200 it is the 198th rather than the slowest.
const readCostRuns = 200

// readCostRatioBound is how much slower at p50 a read this tree moves may be than the read it
// replaces, or than its recorded baseline: the 2x inbox_dismissed_scale_test.go held its reads to
// before it held them to bounds on their work instead.
const readCostRatioBound = 2.0

// The waits. A first boot migrates a whole schema and this head's boot migrates the corpus, so a
// boot gets readCostBootWait, asked every readCostBootPoll since each asking reads the process's
// whole output. readCostOwedWait and readCostOwedPoll are the check's: every owed count is read
// every 2 s and the run fails once one has stayed above zero for 300 s. The release before drains
// what its own writes left owed before its side is measured, within readCostDrainWait, which no
// check states.
const (
	readCostBootWait  = 15 * time.Minute
	readCostBootPoll  = 500 * time.Millisecond
	readCostStopWait  = 2 * time.Minute
	readCostOwedWait  = 300 * time.Second
	readCostOwedPoll  = 2 * time.Second
	readCostDrainWait = 30 * time.Minute
)

// readCostFullCommit is a whole commit id, which is all BASELINE_SHA may name: the run reads it
// from the pull request body that way, and a branch name or a short id names a commit that moves.
var readCostFullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// readCostChainBaselinePath is where the chain read's own baseline is recorded. The chain read
// replaces no read, so its first run records its readings here, with the pod and the date, and
// gates on nothing; every later run over the same corpus on the same pod is held to twice that
// recorded p50.
var readCostChainBaselinePath = filepath.Join("testdata", "read-cost-chain-baseline.json")

// ───── the two servers ────────────────────────────────────────────────────

// startReadCostServer starts binary, a build stamped with commit, over databaseURL on a loopback
// port the test holds until the process serves on it, and waits until it serves. It inherits
// nothing from the test's environment: a home of its own, the person named by a trusted header,
// the shared agent bearer, no NATS, listener as its Envoy listener, and the settings that stop the
// background work a setting stops (readCostStatements).
func startReadCostServer(t *testing.T, client *http.Client, name, binary, commit, databaseURL, listener string) *readCostServer {
	t.Helper()
	return startReadCostServerProfiled(t, client, name, binary, commit, databaseURL, listener, false)
}

// readCostPprofListening is the line a server writes once its profiler listens, with the address
// it bound (cmd/dispatch's startPprof).
var readCostPprofListening = regexp.MustCompile(`msg="dispatch: pprof listening" addr=(127\.0\.0\.1:[0-9]+)`)

// startReadCostServerProfiled is startReadCostServer that, where profiled is set, also has the
// process serve its Go profiler with DISPATCH_PPROF_PORT=0, on a loopback port the kernel chooses,
// and keeps the address from the line naming it (readCostPprofListening), which the process writes
// before it serves. READ_COST_PPROF asks it of this head's server (readCostPprofArm).
func startReadCostServerProfiled(t *testing.T, client *http.Client, name, binary, commit, databaseURL, listener string,
	profiled bool) *readCostServer {
	t.Helper()
	home := t.TempDir()
	held := cmdtest.HoldPort(t)
	environment := map[string]string{
		"HOME":                             home,
		"XDG_CONFIG_HOME":                  home,
		"XDG_DATA_HOME":                    home,
		"DATABASE_URL":                     databaseURL,
		"DISPATCH_AGENT_TOKEN":             readCostAgentToken,
		"DISPATCH_IDENTITY":                "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED": "1",
		"DISPATCH_LISTEN_HOST":             "127.0.0.1",
		"DISPATCH_PORT":                    strconv.Itoa(held.Port),
		"DISPATCH_WEB_DIST":                home,
		"DISPATCH_NATS_DISABLED":           "1",
		"DISPATCH_SIGNING_KEY":             readCostSigningKey,
		"DISPATCH_TEST_HOOKS":              "1",
		"ENVOY_URL":                        listener,
		// A build that lacks one of these settings ignores it: the release before BASELINE_SHA names
		// has no keyword fill, and so no DISPATCH_SEARCH_FILL.
		"DISPATCH_SEARCH_FILL":                     "off",
		"DISPATCH_TEST_EMBEDDER_OFF":               "1",
		"DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL": "24h",
	}
	if profiled {
		environment["DISPATCH_PPROF_PORT"] = "0"
	}
	cmd := exec.Command(binary)
	cmd.Dir = home
	for variable, value := range environment {
		cmd.Env = append(cmd.Env, variable+"="+value)
	}
	slices.Sort(cmd.Env)
	held.Pass(cmd, "DISPATCH_LISTEN_FD")
	server := &readCostServer{name: name, commit: commit, binary: binary, port: held.Port, process: cmdtest.Start(t, name, cmd), client: client}
	server.waitServing(t)
	if profiled {
		listening := readCostPprofListening.FindStringSubmatch(server.process.Output.String())
		if listening == nil {
			t.Fatalf("%s serves, but logged no line naming its profiler's address:\n%s", name, readCostTail(server.process.Output.String()))
		}
		server.pprof = listening[1]
		t.Logf("c-read-cost: %s serves its profiler at %s", name, server.pprof)
	}
	return server
}

// waitServing waits until the process has logged that it serves on its own port and /healthz
// there answers 200, and fails with the tail of its output when it exits first or never does.
func (s *readCostServer) waitServing(t *testing.T) {
	t.Helper()
	started := time.Now()
	listening := `msg="dispatch: listening" addr=127.0.0.1:` + strconv.Itoa(s.port)
	exited := false
	served := testwait.Holds(readCostBootWait, readCostBootPoll, func() bool {
		select {
		case <-s.process.Exited:
			exited = true
			return true
		default:
		}
		if !strings.Contains(s.process.Output.String(), listening) {
			return false
		}
		response, err := s.client.Get(s.url("/healthz"))
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	switch {
	case exited:
		t.Fatalf("%s exited before it served:\n%s", s.name, readCostTail(s.process.Output.String()))
	case !served:
		t.Fatalf("%s did not serve within %s:\n%s", s.name, readCostBootWait, readCostTail(s.process.Output.String()))
	}
	t.Logf("%s serves at %s, %s after it started", s.name, s.url(""), time.Since(started).Round(time.Millisecond))
}

// stop sends the process SIGTERM, as a deploy stops it, and waits until it has exited, so the
// next build boots over a database no process of this one serves.
func (s *readCostServer) stop(t *testing.T) {
	t.Helper()
	s.process.Terminate(t)
	stopped := testwait.Holds(readCostStopWait, readCostBootPoll, func() bool {
		select {
		case <-s.process.Exited:
			return true
		default:
			return false
		}
	})
	if !stopped {
		t.Fatalf("%s was still running %s after SIGTERM:\n%s", s.name, readCostStopWait, readCostTail(s.process.Output.String()))
	}
	t.Logf("%s stopped", s.name)
}

// readCostListener is a stand-in Envoy listener: every session it is asked about is live and takes
// every delivery, and every send is handed over at once, so the reader's messages to the agent are
// delivered and the agent can answer each one.
func readCostListener(t *testing.T) string {
	t.Helper()
	var sends atomic.Int64
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch session, lookup := strings.CutPrefix(r.URL.Path, "/v1/registry/"); {
		case r.Method == http.MethodGet && lookup:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"session_id": session, "title": session, "capabilities": []string{"btw", "aside", "steer"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var send struct {
				TargetSession string `json:"target_session"`
			}
			_ = json.NewDecoder(r.Body).Decode(&send)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"event_id": "read-cost-" + strconv.FormatInt(sends.Add(1), 10), "recipient": send.TargetSession,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	return listener.URL
}

// ───── the run's database ─────────────────────────────────────────────────

// readCostDatabase creates an empty database for this run alone on the server
// DISPATCH_TEST_DATABASE_URL names, turns JIT off on it (readCostJITOff) and answers its URL; it is
// dropped when the test ends, unless keep is set (readCostKeepDatabase). It is empty, and no clone
// of a migrated template, because the release before must migrate it itself. The variable unset
// fails the run rather than skipping it, since a skipped measurement reads as one that held.
func readCostDatabase(t *testing.T, keep bool) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(storetest.DatabaseURLEnv))
	if raw == "" {
		t.Fatalf("%s is unset: c-read-cost boots both builds over a database it creates on that server", storetest.DatabaseURLEnv)
	}
	server, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("read %s: %v", storetest.DatabaseURLEnv, err)
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("name the run's database: %v", err)
	}
	name := "read_cost_" + hex.EncodeToString(suffix)
	admin := *server
	admin.Path = "/postgres"
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		t.Fatalf("connect to the test server: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create the run's database: %v", err)
	}
	if !keep {
		t.Cleanup(func() { dropReadCostDatabase(t, admin.String(), name) })
	}
	readCostJITOff(t, conn, name)
	own := *server
	own.Path = "/" + name
	t.Logf("the run's database is %s", name)
	if keep {
		t.Logf("c-read-cost: %s keeps the run's database %s (jit off), so this run is not a reading of the check", readCostKeepVariable, name)
		kept := filepath.Join(os.TempDir(), readCostKeptDatabaseFile)
		if err := os.WriteFile(kept, []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write the kept database's name %s to %s: %v", name, kept, err)
		}
	}
	return own.String()
}

// readCostKeepVariable, set to 1 for diagnosis, keeps c-read-cost's run's database when the test
// ends (readCostKeepDatabase), and readCostKeptDatabaseFile is the file in $TMPDIR its name is
// written to.
const (
	readCostKeepVariable     = "READ_COST_KEEP_DATABASE"
	readCostKeptDatabaseFile = "read-cost-kept-database"
)

// readCostKeepDatabase reads READ_COST_KEEP_DATABASE before anything is built or read. Unset, it
// answers false, and the run's database is dropped when the test ends, as a run of the check drops
// it. Set to 1, it answers true, and readCostDatabase keeps the database, says in one line that the
// run is not a reading of the check and writes the database's name to readCostKeptDatabaseFile in
// $TMPDIR. Set to anything else, it fails, naming the value it takes.
func readCostKeepDatabase(t *testing.T) bool {
	t.Helper()
	switch named := strings.TrimSpace(os.Getenv(readCostKeepVariable)); named {
	case "":
		return false
	case "1":
		return true
	default:
		t.Fatalf("%s=%q: it takes 1, which keeps the run's database when the test ends, or is left unset", readCostKeepVariable, named)
		return false
	}
}

// readCostJITOff turns JIT off on the database name, over conn to its server, right after the
// database is created and before any server connects to it, so every session on it plans as
// production's do: production's Postgres is Cloud SQL, which does not support LLVM JIT, and a
// Postgres that JITs adds the compile to every statement whose plan costs more than
// jit_above_cost (one statement read 122 ms with JIT, against about 5 ms without).
func readCostJITOff(t *testing.T, conn *pgx.Conn, name string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), "alter database "+name+" set jit = off"); err != nil {
		t.Fatalf("alter database %s set jit = off: %v", name, err)
	}
}

// holdReadCostJITOff reads jit over conn, a session opened on the run's database after
// readCostJITOff as every server's are, and fails the run where it reads anything but off.
func holdReadCostJITOff(t *testing.T, check string, conn *pgx.Conn) {
	t.Helper()
	var jit string
	if err := conn.QueryRow(context.Background(), "show jit").Scan(&jit); err != nil {
		t.Fatalf("%s: show jit on the run's database: %v", check, err)
	}
	if jit != "off" {
		t.Fatalf("%s: a session on the run's database reads jit as %s, where production's Cloud SQL runs without JIT", check, jit)
	}
	t.Logf("%s: jit is off on the run's databases, as production's Cloud SQL runs (it does not support LLVM JIT)", check)
}

// dropReadCostDatabase drops the run's database once every process on it has gone, trying again
// while Postgres still tears down a backend whose client has closed (55006).
func dropReadCostDatabase(t *testing.T, adminURL, name string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Logf("drop the run's database %s: connect: %v", name, err)
		return
	}
	defer conn.Close(ctx)
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := conn.Exec(ctx, "drop database if exists "+name)
		if err == nil {
			return
		}
		var postgres *pgconn.PgError
		if !errors.As(err, &postgres) || postgres.Code != "55006" || time.Now().After(deadline) {
			t.Logf("drop the run's database %s: %v", name, err)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// readCostConnect opens the test's own connection to the run's database, for the owed counts, the
// announcement, the view's definition and vacuum, none of which is a read the run measures.
func readCostConnect(t *testing.T, databaseURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect to the run's database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// readCostVacuum runs `vacuum (analyze)` over the run's database before a side is measured: a
// freshly written table has no statistics and no all-visible pages, and a plan chosen over that is
// one production never runs (cmd/dispatch-seed's vacuum says the same).
func readCostVacuum(t *testing.T, conn *pgx.Conn, side string) {
	t.Helper()
	started := time.Now()
	if _, err := conn.Exec(context.Background(), `vacuum (analyze)`); err != nil {
		t.Fatalf("vacuum (analyze) before the %s is measured: %v", side, err)
	}
	t.Logf("vacuum (analyze) before the %s is measured took %s", side, time.Since(started).Round(time.Millisecond))
}

// ───── the corpus, written through the release before's own writers ──────

// seedReadCost writes the corpus through the server's own API, as its own writers store it: no
// row is written by SQL.
func seedReadCost(t *testing.T, s *readCostServer) *readCostCorpus {
	t.Helper()
	started := time.Now()
	for _, person := range []string{readCostReader, readCostWriter, readCostStranger} {
		s.expect(t, "sign in "+person, http.MethodGet, "/api/v1/whoami", person, nil, http.StatusOK)
	}
	s.expect(t, "create the project "+readCostProject, http.MethodPost, "/api/v1/projects", readCostWriter,
		map[string]string{"key": readCostProject, "name": "The reads this tree moves"}, http.StatusCreated)
	s.expect(t, "create the project "+readCostElsewise, http.MethodPost, "/api/v1/projects", readCostStranger,
		map[string]string{"key": readCostElsewise, "name": "History unrelated to the reader"}, http.StatusCreated)

	corpus := &readCostCorpus{}
	corpus.treeIssue = createReadCostIssue(t, s, readCostProject, "The issue whose tree is expanded",
		readCostRoleText("requirement", "r", readCostSpecBlocks/2, func(int) string { return "" }), readCostWriter)
	corpus.treeSpec = readCostSpecOf(t, s, corpus.treeIssue)
	seedReadCostTreeIssue(t, s, corpus)

	corpus.coverageIssue = createReadCostIssue(t, s, readCostProject, "The requirement fifty checks serve",
		":::requirement{#r1}\nOne requirement that fifty checks serve.\n:::\n", readCostWriter)
	uploadReadCostDocument(t, s, corpus.coverageIssue, "verification.md", readCostVerificationText(corpus.coverageIssue), readCostWriter)

	corpus.citedIssue = createReadCostIssue(t, s, readCostProject, "The issue five hundred comments cite", "", readCostWriter)
	citing := createReadCostIssue(t, s, readCostProject, "The comments that cite it", "", readCostWriter)
	readCostInParallel(t, "comments citing "+corpus.citedIssue, readCostBacklinks, func(index int) error {
		_, err := s.write(http.MethodPost, "/api/v1/issues/"+citing+"/comments", readCostWriter, map[string]string{
			"body": fmt.Sprintf("Comment %d cites dispatch://%s as the issue it leans on.", index, corpus.citedIssue),
		}, http.StatusCreated)
		return err
	})

	seedReadCostInbox(t, s, corpus)
	seedReadCostUnrelated(t, s)
	t.Logf("seeded the corpus through %s in %s", s.name, time.Since(started).Round(time.Millisecond))
	return corpus
}

// seedReadCostTreeIssue writes the rest of the tree issue as its writer: its Verification document
// and its plan, each check and each plan task serving a requirement of the spec, the requirements
// in turn; its tree document, tree.md; its comments; and the asks an agent session opens on it.
func seedReadCostTreeIssue(t *testing.T, s *readCostServer, corpus *readCostCorpus) {
	t.Helper()
	issue := corpus.treeIssue
	serves := func(index int) string {
		return fmt.Sprintf(` serves="dispatch://%s/spec#b-r%d"`, issue, index%(readCostSpecBlocks/2)+1)
	}
	corpus.treeVerification = uploadReadCostDocument(t, s, issue, "verification.md", readCostRoleText("check", "c", readCostVerificationBlocks/2,
		func(index int) string { return serves(index) + ` surface="cli" run="go test ./..."` }), readCostWriter)
	corpus.treePlan = uploadReadCostDocument(t, s, issue, "plan.md", readCostRoleText("plan_task", "t", readCostPlanBlocks/2, serves), readCostWriter)
	corpus.treeDocument = uploadReadCostDocument(t, s, issue, "tree.md", readCostTreeText(), readCostWriter)
	readCostInParallel(t, "comments on "+issue, readCostTreeComments, func(index int) error {
		_, err := s.write(http.MethodPost, "/api/v1/issues/"+issue+"/comments", readCostWriter, map[string]string{
			"body": fmt.Sprintf("Comment %d of the tree issue's conversation.", index),
		}, http.StatusCreated)
		return err
	})
	readCostInParallel(t, "asks on "+issue, readCostTreeAsks, func(index int) error {
		_, err := s.write(http.MethodPost, "/api/v1/issues/"+issue+"/asks", "", map[string]any{
			"question": fmt.Sprintf("Which way does question %d of the tree issue go?", index),
			"actor":    map[string]string{"kind": "session", "id": readCostTreeAsker},
		}, http.StatusCreated)
		return err
	})
}

// seedReadCostInbox writes the reader's readCostEntries delivered entries: readCostAsks open asks
// on issues assigned to them, opened by readCostAskers agent sessions; readCostThreads comment
// threads, each a root the reader wrote and the writer's answer; and readCostConversation one-off
// messages the reader sent the agent, each answered by it.
func seedReadCostInbox(t *testing.T, s *readCostServer, corpus *readCostCorpus) {
	t.Helper()
	for index := range readCostAskers {
		corpus.askIssues = append(corpus.askIssues, createReadCostIssue(t, s, readCostProject,
			fmt.Sprintf("Questions waiting on the reader, batch %02d", index), "", readCostReader))
	}
	readCostInParallel(t, "asks waiting on the reader", readCostAsks, func(index int) error {
		asker := index % readCostAskers
		_, err := s.write(http.MethodPost, "/api/v1/issues/"+corpus.askIssues[asker]+"/asks", "", map[string]any{
			"question": fmt.Sprintf("Which way does question %d of asker %d go?", index, asker),
			"actor":    map[string]string{"kind": "session", "id": fmt.Sprintf("ses_read_cost_asker_%02d", asker)},
		}, http.StatusCreated)
		return err
	})
	readCostInParallel(t, "threads the reader wrote and the writer answered", readCostThreads, func(index int) error {
		issue := corpus.askIssues[index%len(corpus.askIssues)]
		root, err := s.write(http.MethodPost, "/api/v1/issues/"+issue+"/comments", readCostReader, map[string]string{
			"body": fmt.Sprintf("Thread %d of the reader, on the %s by the pond.", index, readCostSearchWord),
		}, http.StatusCreated)
		if err != nil {
			return err
		}
		id, err := readCostIDOf(root)
		if err != nil {
			return err
		}
		_, err = s.write(http.MethodPost, "/api/v1/issues/"+issue+"/comments", readCostWriter, map[string]string{
			"body": fmt.Sprintf("The writer answers thread %d.", index), "reply_to": id,
		}, http.StatusCreated)
		return err
	})
	readCostInParallel(t, "messages the reader sent the agent and the agent answered", readCostConversation, func(index int) error {
		sent, err := s.write(http.MethodPost, "/api/v1/agents/"+readCostConverse+"/messages", readCostReader, map[string]string{
			"body": fmt.Sprintf("Direct question %d to the agent.", index), "delivery": "btw",
		}, http.StatusCreated)
		if err != nil {
			return err
		}
		id, err := readCostIDOf(sent)
		if err != nil {
			return err
		}
		_, err = s.write(http.MethodPost, "/api/v1/messages/"+id+"/reply", "", map[string]any{
			"actor": map[string]string{"kind": "session", "id": readCostConverse}, "attempt": 1,
			"body": fmt.Sprintf("The agent's answer to question %d.", index),
		}, http.StatusCreated)
		return err
	})
}

// seedReadCostUnrelated writes the history unrelated to the reader: readCostUnrelatedDocument
// documents of another person in another project, readCostUnrelatedBlocks blocks each, ten times
// the rows the reader wrote, none naming the reader or holding the word their search reads.
func seedReadCostUnrelated(t *testing.T, s *readCostServer) {
	t.Helper()
	issue := createReadCostIssue(t, s, readCostElsewise, "The stranger's ledgers", "", readCostStranger)
	readCostInParallel(t, "documents of unrelated history", readCostUnrelatedDocument, func(index int) error {
		var text strings.Builder
		fmt.Fprintf(&text, "# The stranger's ledger %d\n\n", index)
		for entry := range readCostUnrelatedBlocks - 1 {
			fmt.Fprintf(&text, "Entry %d of ledger %d records the tide at the lighthouse.\n\n", entry, index)
		}
		_, err := s.write(http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", readCostStranger,
			map[string]string{"name": fmt.Sprintf("ledger-%02d.md", index), "content": text.String()}, http.StatusCreated)
		return err
	})
}

// readCostSpecOf answers the artifact id of issue's spec, its primary document, as the issue's
// own read names it (primary_artifact_id).
func readCostSpecOf(t *testing.T, s *readCostServer, issue string) string {
	t.Helper()
	var read struct {
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	s.read(t, "read the spec of "+issue, "/api/v1/issues/"+issue, &read)
	if read.PrimaryArtifactID == "" {
		t.Fatalf("GET /api/v1/issues/%s on %s names no primary_artifact_id, the issue's spec", issue, s.name)
	}
	return read.PrimaryArtifactID
}

// readCostIDOf is the id an answer names, for a writer goroutine.
func readCostIDOf(answer readCostAnswer) (string, error) {
	var named struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(answer.body, &named); err != nil || named.ID == "" {
		return "", fmt.Errorf("read the id out of %s: %v", answer, err)
	}
	return named.ID, nil
}

// readCostInParallel runs job for each index below jobs on readCostWriters goroutines and fails the
// test with the first error any of them answered, once all have stopped.
func readCostInParallel(t *testing.T, what string, jobs int, job func(index int) error) {
	t.Helper()
	started := time.Now()
	var (
		next   atomic.Int64
		failed atomic.Bool
		first  error
		once   sync.Once
		group  sync.WaitGroup
	)
	for range readCostWriters {
		group.Add(1)
		go func() {
			defer group.Done()
			for !failed.Load() {
				index := int(next.Add(1) - 1)
				if index >= jobs {
					return
				}
				if err := job(index); err != nil {
					once.Do(func() { first = fmt.Errorf("%s, number %d: %w", what, index, err) })
					failed.Store(true)
					return
				}
			}
		}()
	}
	group.Wait()
	if first != nil {
		t.Fatal(first)
	}
	t.Logf("wrote %d %s in %s", jobs, what, time.Since(started).Round(time.Millisecond))
}

// readCostRoleText is a role document of the tree issue made of count typed blocks of typ alone,
// each opened with its #id, prefix and its number, and the attributes attributes gives it, and
// holding one paragraph, so the blocks read lists two blocks for each. Its words write no issue key
// and no #id, so the old page's references of the document read nothing for it
// (features/refs/useTextReferences.ts:73-84 at the commit BASELINE_SHA names).
func readCostRoleText(typ, prefix string, count int, attributes func(index int) string) string {
	var text strings.Builder
	for index := range count {
		fmt.Fprintf(&text, ":::%s{#%s%d%s}\nThe %s numbered %d of the tree issue.\n:::\n\n", typ, prefix, index+1, attributes(index), typ, index+1)
	}
	return text.String()
}

// readCostVerificationText is the Verification document of readCostChecks checks, each serving the
// requirement r1 of issue's spec.
func readCostVerificationText(issue string) string {
	var text strings.Builder
	for index := range readCostChecks {
		fmt.Fprintf(&text, ":::check{#c%d serves=\"dispatch://%s/spec#b-r1\" surface=\"cli\" run=\"go test ./...\"}\n"+
			"Check %d proves the one requirement.\n:::\n\n", index+1, issue, index+1)
	}
	return text.String()
}

// ───── the owed counts ────────────────────────────────────────────────────

// readCostOwedCount is one count a backfill or a conversion step owes, as the SQL that reads it.
type readCostOwedCount struct {
	name, sql string
}

// readCostBaseOwed is what the release before's own block index still owes after its writes: a
// document whose version it has not indexed, and the comments, messages and asks whose own blocks
// it has not. Its side is measured once all four read 0, so no tick of its own runs over the corpus
// while it is read.
var readCostBaseOwed = []readCostOwedCount{
	{"document_block_versions_owed", `(select count(*) from document_block_versions where version is null)`},
	{"comment_block_state_owed", `(select count(*) from comment_block_state where owed_at is not null)`},
	{"message_block_state_owed", `(select count(*) from message_block_state where owed_at is not null)`},
	{"ask_block_state_owed", `(select count(*) from ask_block_state where owed_at is not null)`},
}

// readCostHeadOwed is what this tree's backfills still owe at this head, read as the preamble of
// the issue tree's Verification document reads its backfill's counts: the documents owed a block
// index by either mark (a null version, or the backfill's store_owed_at), and the comments,
// messages and asks owed one of their own blocks; then the object pass that names the issues,
// documents, asks and messages an earlier build wrote with no node, and this build's block-index
// event cursor, which is behind while it has not read every event the log holds. The boot's run of
// the graph store's backfill is read from its own log line (readCostBackfillDone). $1 is this
// head's commit.
var readCostHeadOwed = []readCostOwedCount{
	{"document_block_versions_owed", `(select count(*) from document_block_versions where version is null or store_owed_at is not null)`},
	{"comment_block_state_owed", `(select count(*) from comment_block_state where owed_at is not null)`},
	{"message_block_state_owed", `(select count(*) from message_block_state where owed_at is not null)`},
	{"ask_block_state_owed", `(select count(*) from ask_block_state where owed_at is not null)`},
	{"object_nodes_owed", `(select count(*) from block_index_event_cursors where object_nodes_owed_at is not null)`},
	{"block_index_cursor_behind", `(select case when exists (select 1 from block_index_event_cursors where build_commit = $1)
		and not exists (select 1 from block_index_event_cursors where build_commit = $1
		                and event_id < (select coalesce(max(id), 0) from events)) then 0 else 1 end)`},
}

// readCostBackfillDone is the line the boot's run of the graph store's backfill logs once it has
// run to its end (runBootBackfill).
const readCostBackfillDone = `msg="dispatch: boot backfill done"`

// waitReadCostOwed reads every count of counts every readCostOwedPoll, never on a fixed sleep,
// printing each reading with the database's clock_timestamp(), and returns once every count reads 0
// and, where ended is given, the line it names has been logged. Past within it fails, naming each
// count that stayed above zero.
func waitReadCostOwed(t *testing.T, conn *pgx.Conn, side string, counts []readCostOwedCount, within time.Duration,
	ended func() bool, endedName string, args ...any) {
	t.Helper()
	columns := make([]string, len(counts))
	for index, count := range counts {
		columns[index] = count.sql
	}
	statement := `select clock_timestamp()::text, ` + strings.Join(columns, ", ")
	started := time.Now()
	var stayed []string
	settled := testwait.Holds(within, readCostOwedPoll, func() bool {
		values := make([]int64, len(counts))
		var at string
		targets := []any{&at}
		for index := range values {
			targets = append(targets, &values[index])
		}
		if err := conn.QueryRow(context.Background(), statement, args...).Scan(targets...); err != nil {
			t.Fatalf("%s: read the owed counts: %v", side, err)
		}
		var line strings.Builder
		stayed = nil
		for index, count := range counts {
			fmt.Fprintf(&line, " %s=%d", count.name, values[index])
			if values[index] != 0 {
				stayed = append(stayed, fmt.Sprintf("%s=%d", count.name, values[index]))
			}
		}
		if ended != nil {
			done := ended()
			fmt.Fprintf(&line, " %s=%t", endedName, done)
			if !done {
				stayed = append(stayed, endedName+"=false")
			}
		}
		t.Logf("c-read-cost: %s owes at %s:%s", side, at, line.String())
		return len(stayed) == 0
	})
	if !settled {
		t.Fatalf("c-read-cost: %s still owes after %s: %s", side, within, strings.Join(stayed, ", "))
	}
	t.Logf("c-read-cost: %s owes nothing, %s after the wait began", side, time.Since(started).Round(time.Millisecond))
}

// ───── the shape of the corpus, held on each side ─────────────────────────

// requireReadCostOldShape reads the corpus back through the release before's reads, each one the
// read its side measures.
func requireReadCostOldShape(t *testing.T, s *readCostServer, corpus *readCostCorpus) {
	t.Helper()
	shape := &readCostShape{}
	requireReadCostTreeIssueShape(t, s, corpus, shape)

	var asks []json.RawMessage
	s.read(t, "read the asks waiting on the reader", "/api/v1/inbox?assignee=me", &asks)
	var threads struct {
		Total int `json:"total"`
	}
	s.read(t, "read the reader's threads", "/api/v1/me/replies?kind=reply,agent&state=inbox", &threads)
	var messages []json.RawMessage
	s.read(t, "read the agent's messages", "/api/v1/agents/"+readCostConverse+"/messages", &messages)
	t.Logf("c-read-cost: %s answers the reader %d asks, %d threads and %d messages of their conversation with the agent",
		s.name, len(asks), threads.Total, len(messages))
	shape.hold(t, s.name, "entries delivered to the reader (asks and threads)", len(asks)+threads.Total, readCostEntries, false)

	requireReadCostSharedShape(t, s, corpus, shape)
	shape.require(t, s.name)
}

// readCostRead is one read this tree moves: what the page reads now (new), what it read before
// (old, nil for a read that replaces nothing), each as one reading that may take several requests
// and answers their summed time, what each side's requests are, for the table, and the arm of the
// check it stands in (readCostArms). A read whose against names another read is a second old side
// of that read: it has no new side of its own, its row sets it beside the new side of the read
// against names, it gates nothing, and note is what its row's last cell says of it. apart, where
// set, is the reads such a side leaves out of its readings, which its old side takes apart after
// them (readCostApart), and apartNote what they are.
type readCostRead struct {
	name, arm, oldRoutes, newRoutes string
	old, new                        func(s *readCostServer) (time.Duration, error)
	against, note                   string
	apart                           func(s *readCostServer) (time.Duration, error)
	apartNote                       string
}

// readCostGets times the GETs of paths as the reader, one after another, and answers their summed
// time: one reading of a read the page makes as those requests. An answer other than 200 is no
// reading of the read, and fails it.
func readCostGets(paths ...string) func(s *readCostServer) (time.Duration, error) {
	return func(s *readCostServer) (time.Duration, error) {
		var took time.Duration
		for _, path := range paths {
			answer, err := s.write(http.MethodGet, path, readCostReader, nil, http.StatusOK)
			if err != nil {
				return 0, err
			}
			took += answer.took
		}
		return took, nil
	}
}

// readCostArms are the arms of the check by the names READ_COST_READS takes, each the arm of one
// read of readCostReads, but for the tree arm, which holds the tree read and beside it the old
// page's first load, alone and with its role tabs.
var readCostArms = []string{"inbox", "tree", "coverage", "chain", "references", "search"}

// readCostArmsVariable names the arms a run measures, for diagnosis (readCostArmFilter).
const readCostArmsVariable = "READ_COST_READS"

// readCostArmFilter reads READ_COST_READS, a comma-separated list of arms (readCostArms), before
// anything is built or read. Unset, it answers nil, and every arm is measured, as a run of the
// check measures them. Set, it answers the arms it names and says in one line that the run is not
// a reading of the check; where it names anything else it fails, naming the arms it accepts.
func readCostArmFilter(t *testing.T) map[string]bool {
	t.Helper()
	named := strings.TrimSpace(os.Getenv(readCostArmsVariable))
	if named == "" {
		return nil
	}
	arms := map[string]bool{}
	for arm := range strings.SplitSeq(named, ",") {
		arm = strings.TrimSpace(arm)
		if !slices.Contains(readCostArms, arm) {
			t.Fatalf("%s=%q names %q, which is no arm of c-read-cost: it takes a comma-separated list of the arms %s",
				readCostArmsVariable, named, arm, strings.Join(readCostArms, ", "))
		}
		arms[arm] = true
	}
	t.Logf("c-read-cost: %s=%s measures only those arms, on both sides, so this run is not a reading of the check", readCostArmsVariable, named)
	return arms
}

// readCostPprofVariable names the one arm whose new-side readings a run profiles, for diagnosis
// (readCostPprofArm).
const readCostPprofVariable = "READ_COST_PPROF"

// readCostPprofArm reads READ_COST_PPROF, one arm of readCostArms, before anything is built or read.
// Unset, it answers "", and no profile is taken, as a run of the check takes none. Set, it answers
// the arm and says in one line that the run is not a reading of the check: this head's server then
// serves its profiler (startReadCostServerProfiled), and the arm's new-side readings take one CPU
// profile of it (measureReadCostProfiled). Where it names anything else, or an arm arms (the
// arms READ_COST_READS names) leaves out, it fails, naming the arms it accepts.
func readCostPprofArm(t *testing.T, arms map[string]bool) string {
	t.Helper()
	named := strings.TrimSpace(os.Getenv(readCostPprofVariable))
	if named == "" {
		return ""
	}
	if !slices.Contains(readCostArms, named) {
		t.Fatalf("%s=%q is no arm of c-read-cost: it takes one of the arms %s", readCostPprofVariable, named, strings.Join(readCostArms, ", "))
	}
	if arms != nil && !arms[named] {
		measured := slices.DeleteFunc(slices.Clone(readCostArms), func(arm string) bool { return !arms[arm] })
		t.Fatalf("%s=%s names an arm %s leaves out, so none of its readings would be profiled: it takes one of the arms %s",
			readCostPprofVariable, named, readCostArmsVariable, strings.Join(measured, ", "))
	}
	t.Logf("c-read-cost: %s=%s takes a CPU profile of this head's server over the %s arm's new-side readings, so this run is not a reading of the check",
		readCostPprofVariable, named, named)
	return named
}

// readCostChosen is reads less those of an arm arms leaves out, each of which it says it skips;
// nil arms leaves out none.
func readCostChosen(t *testing.T, reads []readCostRead, arms map[string]bool) []readCostRead {
	t.Helper()
	if arms == nil {
		return reads
	}
	return slices.DeleteFunc(reads, func(read readCostRead) bool {
		if arms[read.arm] {
			return false
		}
		t.Logf("c-read-cost: %s leaves out the arm %s, so %s is measured on neither side", readCostArmsVariable, read.arm, read.name)
		return true
	})
}

// readCostOldPage is one load of the old issue page as a second old side of the tree read takes
// it: the paths one load GETs, one after another, the routes they are, and what its row says of
// it; and the paths of the reads it leaves out of its readings, which its old side takes apart
// (readCostApart), with what they are.
type readCostOldPage struct {
	paths, routes []string
	note          string
	apart         []string
	apartNote     string
}

// readCostRoute is one read of the old page: the route it is, as a row names it, and the path a
// reading of it GETs.
type readCostRoute struct {
	route, path string
}

// sendOnce sends each of reads once on s as the reader, before any reading of the page, and adds
// each that answers 200 to the page's reads. One that answers 404 is one the old build does not
// serve for the tree issue, which it says and leaves out of every reading of what, answering its
// route; any other answer fails the run.
func (page *readCostOldPage) sendOnce(t *testing.T, s *readCostServer, what string, reads []readCostRoute) []string {
	t.Helper()
	var unserved []string
	for _, read := range reads {
		answer, err := s.send(http.MethodGet, read.path, readCostReader, nil)
		switch {
		case err != nil:
			t.Fatalf("c-read-cost: %s: %v", what, err)
		case answer.status == http.StatusNotFound:
			unserved = append(unserved, read.route)
			t.Logf("c-read-cost: %s answers GET %s with %s: the old build does not serve this read of the tree issue, so every "+
				"reading of %s leaves it out", s.name, read.path, answer, what)
		case answer.status != http.StatusOK:
			t.Fatalf("c-read-cost: %s: GET %s on %s answered %s", what, read.path, s.name, answer)
		default:
			page.paths = append(page.paths, read.path)
			page.routes = append(page.routes, read.route)
		}
	}
	return unserved
}

// readCostIssueKey is an issue key a text writes, bare or in a dispatch:// address.
var readCostIssueKey = regexp.MustCompile(`\b[A-Z][A-Z0-9]*-[0-9]+\b`)

// readCostOldFirstLoad is the old issue page's first load of the tree issue on the release before
// s: the reads about the issue that the web client of the commit BASELINE_SHA names sends to draw
// it on the Spec tab, which the page opens on, in the order the page sends them. Each is cited at
// that commit, under packages/dispatch/web/src; the page gate is features/issue/IssuePage.tsx:401,
// and the Spec tab mounts the document and its requirements at features/issue/IssuePage.tsx:474-489.
//
//   - GET /api/v1/issues/{key}: features/issue/useIssueDetail.ts:19 (issueQuery,
//     api/queries.ts:86-95; api/client.ts:1074-1075).
//   - GET /api/v1/issues/{key}/comments, the margin's threads: features/margin/useMarginItems.ts:334-345
//     (api/client.ts:1211-1214, with no artifact).
//   - GET /api/v1/issues/{key}/asks?state=all, the margin's asks and the spec's ask blocks, one
//     query under one key: features/margin/useAnsweredAsks.ts:12-26 and
//     features/doc/ProofDocument.tsx:371-375 (api/client.ts:1141-1144).
//   - GET /api/v1/artifacts/{spec}: features/doc/ProofDocument.tsx:313-316 (api/client.ts:1366-1367).
//   - GET /api/v1/artifacts/{spec}/text: features/doc/ProofDocument.tsx:317 (liveTextQueryOptions,
//     :100-104; api/client.ts:1391-1392).
//   - GET /api/v1/artifacts/{spec}/blocks: features/doc/ProofDocument.tsx:362-365
//     (artifactBlocksQuery, api/queries.ts:137-141; api/client.ts:1407-1408), the key
//     features/doc/spec-marks.ts:152-156 reads as well.
//   - GET /api/v1/issues/{key}/decisions, the stale-block marks: features/doc/spec-marks.ts:131
//     (issueDecisionsQuery, :30-34; api/client.ts:1413-1414).
//   - GET /api/v1/issues/{key}/coverage: features/issue/SpecRequirements.tsx:54
//     (issueCoverageQuery, api/queries.ts:102-106; api/client.ts:1111-1112).
//   - GET /api/v1/references?kind=part_of&to=dispatch%3A%2F%2F{key}%2Fspec, the blocks changed
//     since the person last read the spec: features/doc/spec-marks.ts:146-151
//     (api/client.ts:1527-1530), which the client sends once the person's read mark
//     (spec-marks.ts:133-137) names a version.
//   - GET /api/v1/references/questions?id={spec}&kind=artifact, the question map of the spec, the
//     source features/doc/ProofDocument.tsx:302 names: features/refs/useTextReferences.ts:81-84
//     (questionsQuery, features/refs/reference-target.ts:169-174; api/client.ts:1537-1540), which
//     the client sends where the text writes a #id.
//   - GET /api/v1/issues/{cited}/summary for each issue the spec cites:
//     features/refs/useTextReferences.ts:73-77 (issueSummaryQuery,
//     features/refs/reference-target.ts:84-90; api/client.ts:1080-1081). The cited issues are the
//     issue keys the spec's text writes (readCostIssueKey); the corpus's spec writes none.
//
// Every reading sends each of them, whatever the read mark and the text. Left out are the shell's
// reads that are not about the issue (/auth/whoami, /projects, /priorities, /agents,
// /projects/{key}/architecture-sources, the block schema), the person-only reads production
// answers an agent with 403 (/me/state, /inbox, /me/documents/{id}/read,
// /issues/{key}/subscribers and the shell's counts), and the lazy reads of the other tabs, of
// which readCostOldRoleTabs adds the role tabs'. Each read is sent once here first (sendOnce).
func readCostOldFirstLoad(t *testing.T, s *readCostServer, corpus *readCostCorpus) readCostOldPage {
	t.Helper()
	key, spec := corpus.treeIssue, corpus.treeSpec
	var text struct {
		Markdown string `json:"markdown"`
	}
	s.read(t, "read the tree issue's spec", "/api/v1/artifacts/"+spec+"/text", &text)
	var blocks []readCostBlock
	s.read(t, "read the blocks of the tree issue's spec", "/api/v1/artifacts/"+spec+"/blocks", &blocks)
	reads := []readCostRoute{
		{"/api/v1/issues/{key}", "/api/v1/issues/" + key},
		{"/api/v1/issues/{key}/comments", "/api/v1/issues/" + key + "/comments"},
		{"/api/v1/issues/{key}/asks?state=all", "/api/v1/issues/" + key + "/asks?state=all"},
		{"/api/v1/artifacts/{spec}", "/api/v1/artifacts/" + spec},
		{"/api/v1/artifacts/{spec}/text", "/api/v1/artifacts/" + spec + "/text"},
		{"/api/v1/artifacts/{spec}/blocks", "/api/v1/artifacts/" + spec + "/blocks"},
		{"/api/v1/issues/{key}/decisions", "/api/v1/issues/" + key + "/decisions"},
		{"/api/v1/issues/{key}/coverage", "/api/v1/issues/" + key + "/coverage"},
		{"/api/v1/references?kind=part_of&to={key}/spec", "/api/v1/references?kind=part_of&to=" + url.QueryEscape("dispatch://"+key+"/spec")},
		{"/api/v1/references/questions?id={spec}&kind=artifact", "/api/v1/references/questions?id=" + spec + "&kind=artifact"},
	}
	var cited []string
	for _, issue := range readCostIssueKey.FindAllString(text.Markdown, -1) {
		if !slices.Contains(cited, issue) {
			cited = append(cited, issue)
			reads = append(reads, readCostRoute{"/api/v1/issues/{cited}/summary", "/api/v1/issues/" + issue + "/summary"})
		}
	}
	var load readCostOldPage
	unserved := load.sendOnce(t, s, "the old page's first load of "+key, reads)
	load.note = fmt.Sprintf("the spec holds %d blocks and cites %d issues; a load is %d reads", len(blocks), len(cited), len(load.paths))
	if len(unserved) > 0 {
		load.note += ", leaving out what the old build does not serve: " + strings.Join(unserved, ", ")
	}
	t.Logf("c-read-cost: one reading of the old page's first load of %s on %s is these %d reads, one after another: %s; %s",
		key, s.name, len(load.paths), strings.Join(load.paths, " + "), load.note)
	return load
}

// readCostOldRoleTabs is the old issue page's first load of the tree issue (firstLoad) with its
// Verification, Plan and Decisions tabs then opened, on the release before s: every read of the
// first load, then the reads each tab adds, in the order the tabs stand. Each is cited at the
// commit BASELINE_SHA names, under packages/dispatch/web/src. The Verification and Plan tabs are
// features/issue/IssuePage.tsx:493-512, each a RoleDocumentPanel (:164-211) that mounts its tab
// and, where the issue holds the document, the document through ArtifactDocument (:197-206) and
// ProofDocument (features/artifacts/ArtifactDocument.tsx:173-185). Each finds its document by the
// issue read's verification_artifact_id or plan_artifact_id (features/issue/useIssueDetail.ts:21-22,
// api/issue-cache.ts:17-29), and with none draws an empty state and reads nothing
// (features/verification/VerificationTab.tsx:261-271, features/plan/PlanTab.tsx:146-156). The
// Decisions tab is features/issue/IssuePage.tsx:580-594.
//
//   - Verification: GET /api/v1/issues/{key}/results, features/verification/VerificationTab.tsx:209-210
//     (issueResultsQuery, api/queries.ts:111-115; api/client.ts:1104-1105); then the document's
//     GET /api/v1/artifacts/{id}, /text and /blocks, as the first load reads the spec's
//     (features/doc/ProofDocument.tsx:313-317,362-365).
//   - Plan: the plan's GET /api/v1/artifacts/{id}, /text and /blocks, the same lines; its task
//     list reads the blocks under the same key (features/plan/PlanTab.tsx:74-77), so once.
//   - Decisions: GET /api/v1/issues/{key}/decisions (features/issue/DecisionsPanel.tsx:375-378),
//     under the key the first load's decisions read has (features/doc/spec-marks.ts:30-34), so the
//     tab sends no read of its own while that answer is fresh (staleTime 30 s, main.tsx:29).
//
// Neither role tab sends a read the first load sent: the document's asks are the issue's
// (features/doc/ProofDocument.tsx:371-375), and so are its margin's comments
// (features/margin/useMarginItems.ts:334-345). Neither sends GET /api/v1/issues/{key}/comments?artifact={id}:
// ArtifactDocument reads it only where the address names a comment
// (features/artifacts/ArtifactDocument.tsx:112-122), and the panel names none
// (features/issue/IssuePage.tsx:198-205). A check's or a task's Referenced-by panel reads nothing
// until it is opened (features/refs/ReferencedBy.tsx:329-345), the plan's approval chip reads its
// reviews only in its dialog (features/doc/ApprovalChip.tsx:66-77), and a document's references
// (features/doc/ProofDocument.tsx:1167) read an issue summary for each bare key and the question
// map for a #id its words write (features/refs/useTextReferences.ts:73-84), which the corpus's
// role documents do not write.
//
// The Plan tab sends one read more for each task it lists, which every reading leaves out and the
// old side takes apart after its readings (readCostApart): GET /api/v1/references?to={block} for
// each plan_task and each block the tasks' serves and needs name (readCostPlanTaskReads). Every
// read is sent once here first (sendOnce); a tab whose document the issue does not hold reads none
// of it, which is said and named in the row as absent.
func readCostOldRoleTabs(t *testing.T, s *readCostServer, corpus *readCostCorpus, firstLoad readCostOldPage) readCostOldPage {
	t.Helper()
	key := corpus.treeIssue
	var issue struct {
		VerificationArtifactID string `json:"verification_artifact_id"`
		PlanArtifactID         string `json:"plan_artifact_id"`
	}
	s.read(t, "read the tree issue's role documents", "/api/v1/issues/"+key, &issue)
	documentReads := func(name, id string) []readCostRoute {
		return []readCostRoute{
			{"/api/v1/artifacts/{" + name + "}", "/api/v1/artifacts/" + id},
			{"/api/v1/artifacts/{" + name + "}/text", "/api/v1/artifacts/" + id + "/text"},
			{"/api/v1/artifacts/{" + name + "}/blocks", "/api/v1/artifacts/" + id + "/blocks"},
		}
	}
	var reads, tasks []readCostRoute
	var absent []string
	if issue.VerificationArtifactID == "" {
		absent = append(absent, "the Verification tab's /api/v1/issues/{key}/results and its document's three reads, since the issue holds no Verification document")
	} else {
		reads = append(reads, readCostRoute{"/api/v1/issues/{key}/results", "/api/v1/issues/" + key + "/results"})
		reads = append(reads, documentReads("verification", issue.VerificationArtifactID)...)
	}
	if issue.PlanArtifactID == "" {
		absent = append(absent, "the Plan tab's three reads of its document, since the issue holds no plan")
	} else {
		reads = append(reads, documentReads("plan", issue.PlanArtifactID)...)
		tasks = readCostPlanTaskReads(t, s, key, issue.PlanArtifactID)
	}
	decisions := "the Decisions tab adds no read: it draws from the first load's /api/v1/issues/{key}/decisions"
	if !slices.Contains(firstLoad.routes, "/api/v1/issues/{key}/decisions") {
		decisions = "the Decisions tab adds no read"
		absent = append(absent, "the Decisions tab's /api/v1/issues/{key}/decisions, which the old build does not serve")
	}
	what := "the old page's first load of " + key + " and its role tabs"
	tabs := readCostOldPage{paths: slices.Clone(firstLoad.paths), routes: slices.Clone(firstLoad.routes)}
	absent = append(absent, tabs.sendOnce(t, s, what, reads)...)
	var apart readCostOldPage
	if unserved := apart.sendOnce(t, s, "the Plan tab's reads of the tasks of "+key, tasks); len(unserved) > 0 {
		absent = append(absent, fmt.Sprintf("%d of the Plan tab's reads of its tasks", len(unserved)))
	}
	tabs.apart = apart.paths
	leftOut := "none"
	if len(tabs.apart) > 0 {
		tabs.apartNote = fmt.Sprintf("the %d GET /api/v1/references?to={block} the Plan tab sends, one for each plan_task it lists and each "+
			"block their serves and needs name (features/plan/PlanTab.tsx:79-99,124-128, BlockLink :25-27; "+
			"features/refs/reference-target.ts:450-453), which its readings leave out", len(tabs.apart))
		leftOut = tabs.apartNote + ", taken apart on the line below"
	}
	if len(absent) == 0 {
		absent = append(absent, "none")
	}
	tabs.note = fmt.Sprintf("a load is the first load's %d reads and the role tabs' %d, %d reads: %s; %s; left out of every reading: %s; "+
		"absent: %s", len(firstLoad.paths), len(tabs.paths)-len(firstLoad.paths), len(tabs.paths), strings.Join(tabs.routes, " + "),
		decisions, leftOut, strings.Join(absent, "; "))
	t.Logf("c-read-cost: one reading of %s on %s is these %d reads, one after another: %s; %s", what, s.name, len(tabs.paths),
		strings.Join(tabs.paths, " + "), tabs.note)
	return tabs
}

// readCostPlanTaskReads is what the old Plan tab reads for the tasks it lists: GET
// /api/v1/references?to={block} for each block its tasks name, each block once, as its query key
// is its reference (features/refs/reference-target.ts:144-148,156-161). Its task list takes the
// plan's plan_task blocks (features/plan/PlanTab.tsx:79-99) and draws each (:124-128) as a Task
// (:53-62), whose BlockLink (:25-27, at :56) reads the task's own block through useReferenceTarget
// and useReferenceData (features/refs/reference-target.ts:546-552,450-453; api/client.ts:1527-1531);
// the task's reference is dispatch://{key}/artifact/{slug}#b-{id} (PlanTab.tsx:83-93;
// features/refs/routes.ts:715-719, features/refs/block-reference.ts:103-105). Its Pointers draw a
// BlockLink for each block its serves and needs name (PlanTab.tsx:16-21,46,57-58), each read the
// same way.
func readCostPlanTaskReads(t *testing.T, s *readCostServer, key, plan string) []readCostRoute {
	t.Helper()
	var artifact struct {
		Slug string `json:"slug"`
	}
	s.read(t, "read the tree issue's plan", "/api/v1/artifacts/"+plan, &artifact)
	var blocks []struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Attributes []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"attributes"`
	}
	s.read(t, "read the blocks of the tree issue's plan", "/api/v1/artifacts/"+plan+"/blocks", &blocks)
	var reads []readCostRoute
	named := map[string]bool{}
	read := func(reference string) {
		if !named[reference] {
			named[reference] = true
			reads = append(reads, readCostRoute{"/api/v1/references?to={block}", "/api/v1/references?to=" + url.QueryEscape(reference)})
		}
	}
	for _, block := range blocks {
		if block.Type != "plan_task" {
			continue
		}
		read("dispatch://" + key + "/artifact/" + url.PathEscape(artifact.Slug) + "#b-" + block.ID)
		for _, attribute := range block.Attributes {
			if attribute.Name == "serves" || attribute.Name == "needs" {
				for _, pointer := range strings.Fields(attribute.Value) {
					read(pointer)
				}
			}
		}
	}
	return reads
}

// readCostReads are the six reads, and beside the tree read two more old sides of it. Where the
// route stands at both commits - the coverage read, the references panel with the header's
// backlink count, and search - each side is that route. The inbox's new read replaces three, which
// a reading of the release before makes one after another, as its page did. The tree read replaces
// the reads of an issue's documents, held to the blocks read of the issue's tree document,
// tree.md; the old page's first load (readCostOldFirstLoad) is every read that page made to draw
// the same issue, and with its role tabs (readCostOldRoleTabs) every read it made to show the four
// role documents the tree read answers at once, but the reads it makes of the plan's tasks, which
// the old side takes apart; neither gates anything yet. The chain read replaces nothing, so it
// carries a baseline of its own (readCostChainBound).
func readCostReads(corpus *readCostCorpus, firstLoad, roleTabs readCostOldPage) []readCostRead {
	cited := "/api/v1/references?to=" + url.QueryEscape("dispatch://"+corpus.citedIssue)
	inboxOld := []string{"/api/v1/inbox?assignee=me", "/api/v1/me/replies?kind=reply,agent&state=inbox",
		"/api/v1/agents/" + readCostConverse + "/messages"}
	var tasks func(s *readCostServer) (time.Duration, error)
	if len(roleTabs.apart) > 0 {
		tasks = readCostGets(roleTabs.apart...)
	}
	return []readCostRead{
		{
			name: "inbox", arm: "inbox", oldRoutes: strings.Join(inboxOld, " + "), newRoutes: "/api/v1/me/inbox?state=inbox",
			old: readCostGets(inboxOld...), new: readCostGets("/api/v1/me/inbox?state=inbox"),
		},
		{
			name: "issue tree expansion", arm: "tree",
			oldRoutes: "/api/v1/artifacts/{tree.md}/blocks", newRoutes: "/api/v1/issues/{tree issue}/tree",
			old: readCostGets("/api/v1/artifacts/" + corpus.treeDocument + "/blocks"),
			new: readCostGets("/api/v1/issues/" + corpus.treeIssue + "/tree"),
		},
		{
			name: "issue tree first load (old page)", arm: "tree",
			oldRoutes: strings.Join(firstLoad.routes, " + "), newRoutes: "the new side of the issue tree expansion",
			old: readCostGets(firstLoad.paths...), against: "issue tree expansion", note: firstLoad.note,
		},
		{
			name: "issue tree first load and role tabs (old page)", arm: "tree",
			oldRoutes: strings.Join(roleTabs.routes, " + "), newRoutes: "the new side of the issue tree expansion",
			old: readCostGets(roleTabs.paths...), against: "issue tree expansion", note: roleTabs.note,
			apart: tasks, apartNote: roleTabs.apartNote,
		},
		{
			name: "coverage", arm: "coverage",
			oldRoutes: "/api/v1/issues/{issue}/coverage", newRoutes: "/api/v1/issues/{issue}/coverage",
			old: readCostGets("/api/v1/issues/" + corpus.coverageIssue + "/coverage"),
			new: readCostGets("/api/v1/issues/" + corpus.coverageIssue + "/coverage"),
		},
		{
			name: "chain", arm: "chain",
			oldRoutes: "none: the chain read replaces no read", newRoutes: "/api/v1/issues/{issue}/chain",
			new: readCostGets("/api/v1/issues/" + corpus.coverageIssue + "/chain"),
		},
		{
			name: "references panel with its backlink counts", arm: "references",
			oldRoutes: "/api/v1/references?to={issue} + /api/v1/issues/{issue}", newRoutes: "/api/v1/references?to={issue} + /api/v1/issues/{issue}",
			old: readCostGets(cited, "/api/v1/issues/"+corpus.citedIssue),
			new: readCostGets(cited, "/api/v1/issues/"+corpus.citedIssue),
		},
		{
			name: "search", arm: "search",
			oldRoutes: "/api/v1/search?q=" + readCostSearchWord, newRoutes: "/api/v1/search?q=" + readCostSearchWord,
			old: readCostGets("/api/v1/search?q=" + readCostSearchWord),
			new: readCostGets("/api/v1/search?q=" + readCostSearchWord),
		},
	}
}

// readCostReading is one read's readings on one side: the four percentiles and every reading, so
// a number in the table can be read against the spread it came from, and the database time of
// the readings and of the idle interval after them (readCostStatements), nil where it was not
// measured, and where its old side leaves reads out of its readings, those reads taken apart (nil
// for every other read).
type readCostReading struct {
	p50, p90, p95, p99 time.Duration
	runs               []time.Duration
	database, idle     *readCostDatabaseTime
	apart              *readCostApart
}

// measureReadCost takes one warm-up reading that is not counted - the first run of a statement on
// a connection pays for the pages every run after it finds cached - and then readCostRuns readings,
// with the percentiles inbox_dismissed_scale_test.go takes (percentileOfDismissedScale, a nearest
// rank, so every number printed is a reading that happened), widened to p90 and p99, and the
// database time of those readings (readCostStatements.measure). It prints them on one line for
// check, naming the read and what it is read on.
func measureReadCost(t *testing.T, check string, statements *readCostStatements, s *readCostServer, read, on string,
	take func(*readCostServer) (time.Duration, error)) readCostReading {
	t.Helper()
	return measureReadCostProfiled(t, check, statements, s, read, on, take, "")
}

// measureReadCostProfiled is measureReadCost that, where arm names an arm (READ_COST_PPROF), takes
// one CPU profile of s over the readCostRuns readings, asked for just before the first of them
// (startReadCostProfile), with the readings sent as they always are, and reads it once they and
// the idle interval after them are done (readCostProfile.read).
func measureReadCostProfiled(t *testing.T, check string, statements *readCostStatements, s *readCostServer, read, on string,
	take func(*readCostServer) (time.Duration, error), arm string) readCostReading {
	t.Helper()
	warmUp, err := take(s)
	if err != nil {
		t.Fatalf("%s: the warm-up reading of %s on %s: %v", check, read, on, err)
	}
	var profile *readCostProfile
	runs := make([]time.Duration, 0, readCostRuns)
	database, idle := statements.measure(t, readCostRuns, func() {
		if arm != "" {
			profile = startReadCostProfile(t, s, arm, warmUp)
		}
		for range readCostRuns {
			took, err := take(s)
			if err != nil {
				t.Fatalf("%s: a reading of %s on %s: %v", check, read, on, err)
			}
			runs = append(runs, took)
		}
		if profile != nil {
			profile.ended = time.Now()
		}
	})
	sorted := slices.Clone(runs)
	slices.Sort(sorted)
	reading := readCostReading{
		p50:      percentileOfDismissedScale(sorted, 0.50),
		p90:      percentileOfDismissedScale(sorted, 0.90),
		p95:      percentileOfDismissedScale(sorted, 0.95),
		p99:      percentileOfDismissedScale(sorted, 0.99),
		runs:     runs,
		database: database,
		idle:     idle,
	}
	t.Logf("%s: %s on %s: p50 %.1f ms, p90 %.1f ms, p95 %.1f ms, p99 %.1f ms over %d readings%s", check, read, on,
		msOf(reading.p50), msOf(reading.p90), msOf(reading.p95), msOf(reading.p99), readCostRuns, readCostDatabaseLine(database, idle))
	if profile != nil {
		profile.read(t, s)
	}
	return reading
}

// readCostProfileLead is how long a profile runs before the readings it covers begin: the time
// the server takes to read the request and start profiling, which no answer reports, since
// net/http/pprof writes nothing until the profile ends.
const readCostProfileLead = 250 * time.Millisecond

// readCostProfile is one CPU profile of a server (startReadCostProfile): the arm whose readings it
// covers, its length in seconds, when its request was sent, when the readings began and ended,
// and, once done is closed, the profile or the error that stands in its place.
type readCostProfile struct {
	arm                string
	seconds            int
	sent, began, ended time.Time
	done               chan struct{}
	body               []byte
	err                error
}

// startReadCostProfile asks s's profiler for one CPU profile (/debug/pprof/profile?seconds=n), n
// long enough to cover readCostProfileLead and then readCostRuns readings of twice warmUp each, with
// a second to spare, and answers once the request is sent and readCostProfileLead has passed, just
// before the first reading. The profile is read on its own goroutine until readCostProfile.read
// waits for it.
func startReadCostProfile(t *testing.T, s *readCostServer, arm string, warmUp time.Duration) *readCostProfile {
	t.Helper()
	length := readCostProfileLead + 2*readCostRuns*warmUp
	profile := &readCostProfile{arm: arm, seconds: int((length+time.Second-1)/time.Second) + 1, done: make(chan struct{})}
	address := "http://" + s.pprof + "/debug/pprof/profile?seconds=" + strconv.Itoa(profile.seconds)
	sent := make(chan struct{})
	var once sync.Once
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { once.Do(func() { close(sent) }) },
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		t.Fatalf("c-read-cost: pprof %s: ask %s for a CPU profile: %v", arm, address, err)
	}
	client := &http.Client{Timeout: time.Duration(profile.seconds)*time.Second + readCostStopWait}
	go func() {
		defer close(profile.done)
		response, err := client.Do(request)
		if err != nil {
			profile.err = err
			return
		}
		defer response.Body.Close()
		profile.body, profile.err = io.ReadAll(response.Body)
		if profile.err == nil && response.StatusCode != http.StatusOK {
			profile.err = fmt.Errorf("%s answered %d: %s", address, response.StatusCode, bytes.TrimSpace(profile.body))
		}
	}()
	select {
	case <-sent:
	case <-profile.done:
		t.Fatalf("c-read-cost: pprof %s: ask %s for a CPU profile: %v", arm, address, profile.err)
	}
	profile.sent = time.Now()
	time.Sleep(readCostProfileLead)
	profile.began = time.Now()
	return profile
}

// read waits for the profile, writes it to $TMPDIR/read-cost-<arm>.pprof, fails the run where the
// readings it was asked to cover ran past its end, and logs go tool pprof's top 25 functions of it
// by cumulative time, read against s's build, each line prefixed "c-read-cost: pprof <arm>:".
func (profile *readCostProfile) read(t *testing.T, s *readCostServer) {
	t.Helper()
	<-profile.done
	if profile.err != nil {
		t.Fatalf("c-read-cost: pprof %s: the CPU profile of %s: %v", profile.arm, s.name, profile.err)
	}
	path := filepath.Join(os.TempDir(), "read-cost-"+profile.arm+".pprof")
	if err := os.WriteFile(path, profile.body, 0o644); err != nil {
		t.Fatalf("c-read-cost: pprof %s: write the CPU profile to %s: %v", profile.arm, path, err)
	}
	t.Logf("c-read-cost: pprof %s: a CPU profile of %s over %d s, from %s before its %d new-side readings began; they took %s; written to %s",
		profile.arm, s.name, profile.seconds, profile.began.Sub(profile.sent).Round(time.Millisecond), readCostRuns,
		profile.ended.Sub(profile.began).Round(time.Millisecond), path)
	if end := profile.sent.Add(time.Duration(profile.seconds) * time.Second); profile.ended.After(end) {
		t.Errorf("c-read-cost: pprof %s: the readings ran %s past the profile's end, so it covers only their first part",
			profile.arm, profile.ended.Sub(end).Round(time.Millisecond))
	}
	out, err := exec.Command("go", "tool", "pprof", "-top", "-cum", "-nodecount=25", s.binary, path).CombinedOutput()
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		t.Logf("c-read-cost: pprof %s: %s", profile.arm, line)
	}
	if err != nil {
		t.Errorf("c-read-cost: pprof %s: go tool pprof -top -cum -nodecount=25 %s %s: %v", profile.arm, s.binary, path, err)
	}
}

// measureReadCostSide takes every read of reads that the side has (old, or new) on s, keyed by
// name, and on the old side, after a read's readings, the reads it leaves out of them, taken apart
// (measureReadCostApart). The readings of the read of the arm profile names, where it names one
// (READ_COST_PPROF), take one CPU profile of s (measureReadCostProfiled).
func measureReadCostSide(t *testing.T, statements *readCostStatements, s *readCostServer, reads []readCostRead, old bool,
	skip map[string]bool, profile string) map[string]readCostReading {
	t.Helper()
	readings := map[string]readCostReading{}
	for _, read := range reads {
		take := read.new
		if old {
			take = read.old
		}
		if take == nil || skip[read.name] {
			if take != nil && read.arm == profile {
				t.Errorf("c-read-cost: pprof %s: %s takes no readings on %s, so no profile of them was taken", profile, read.name, s.name)
			}
			continue
		}
		arm := ""
		if read.arm == profile {
			arm = profile
		}
		reading := measureReadCostProfiled(t, "c-read-cost", statements, s, read.name, s.name, take, arm)
		if old && read.apart != nil {
			reading.apart = measureReadCostApart(t, statements, s, read)
		}
		readings[read.name] = reading
	}
	return readings
}

// readCostApartLoads is how many loads of the reads a second old side leaves out of its readings
// are taken apart, one after another, after its readings.
const readCostApartLoads = 3

// readCostApart is the reads a second old side leaves out of its readings, taken apart: the time
// of each of readCostApartLoads loads of all of them, and their database time and that of the idle
// interval after them, each per load. It gates nothing; it shows what the reads left out cost.
type readCostApart struct {
	loads          []time.Duration
	database, idle *readCostDatabaseTime
}

// measureReadCostApart takes readCostApartLoads loads of the reads read leaves out of its readings
// on s, one after another, with no warm-up, between two snapshots of the database time.
func measureReadCostApart(t *testing.T, statements *readCostStatements, s *readCostServer, read readCostRead) *readCostApart {
	t.Helper()
	apart := &readCostApart{}
	apart.database, apart.idle = statements.measure(t, readCostApartLoads, func() {
		for range readCostApartLoads {
			took, err := read.apart(s)
			if err != nil {
				t.Fatalf("c-read-cost: a load of the reads %s leaves out, on %s: %v", read.name, s.name, err)
			}
			apart.loads = append(apart.loads, took)
		}
	})
	return apart
}

// readCostApartLine is the line beside the row of a read whose old side leaves reads out of its
// readings: what they are, the time of each load of them taken apart, and their database time per
// load, a load being one reading of that line.
func readCostApartLine(read readCostRead, apart *readCostApart) string {
	return fmt.Sprintf("%s, beside its row and gating nothing: %s; %d loads of them, one after another after its readings, took %v ms, "+
		"each load one reading here%s", read.name, read.apartNote, len(apart.loads), readCostMilliseconds(apart.loads),
		readCostDatabaseLine(apart.database, apart.idle))
}

// ───── the chain read's own baseline ──────────────────────────────────────

// readCostPod is the machine a reading was taken on. Host names the pod; the rest is what decides
// whether two runs ran on the same kind of machine, which a pod's own name does not, since every
// run of the check gets a pod of its own.
type readCostPod struct {
	Host      string `json:"host"`
	CPUs      int    `json:"cpus"`
	CPUModel  string `json:"cpu_model"`
	MemoryKiB int64  `json:"memory_kib"`
	Platform  string `json:"platform"`
}

// moved is each way pod differs from other as a kind of machine, nothing where the two are the
// same kind.
func (pod readCostPod) moved(other readCostPod) []string {
	var moved []string
	if pod.CPUs != other.CPUs {
		moved = append(moved, fmt.Sprintf("cpus %d -> %d", other.CPUs, pod.CPUs))
	}
	if pod.CPUModel != other.CPUModel {
		moved = append(moved, fmt.Sprintf("cpu model %q -> %q", other.CPUModel, pod.CPUModel))
	}
	if pod.MemoryKiB != other.MemoryKiB {
		moved = append(moved, fmt.Sprintf("memory %d KiB -> %d KiB", other.MemoryKiB, pod.MemoryKiB))
	}
	if pod.Platform != other.Platform {
		moved = append(moved, fmt.Sprintf("platform %s -> %s", other.Platform, pod.Platform))
	}
	return moved
}

// readCostPodNow is the machine this run reads on: its host name, its CPUs and their model, the
// memory the kernel reports, and the platform.
func readCostPodNow() readCostPod {
	host, _ := os.Hostname()
	pod := readCostPod{Host: host, CPUs: runtime.NumCPU(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if info, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for line := range strings.Lines(string(info)) {
			if name, value, found := strings.Cut(line, ":"); found && strings.TrimSpace(name) == "model name" {
				pod.CPUModel = strings.TrimSpace(value)
				break
			}
		}
	}
	if info, err := os.ReadFile("/proc/meminfo"); err == nil {
		for line := range strings.Lines(string(info)) {
			if value, found := strings.CutPrefix(line, "MemTotal:"); found {
				pod.MemoryKiB, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "kB")), 10, 64)
				break
			}
		}
	}
	return pod
}

// readCostCorpusName is the corpus a chain reading was taken over, as the constants that write it
// state it: a run over another corpus reads another read.
func readCostCorpusName() string {
	return fmt.Sprintf("%d blocks at depth %d; a tree issue of %d, %d and %d role blocks, %d comments and %d asks; "+
		"%d entries (%d asks, %d threads, %d messages); %d backlinks; %d checks; %d unrelated rows; %d readings",
		readCostTreeBlocks, readCostTreeDepth, readCostSpecBlocks, readCostVerificationBlocks, readCostPlanBlocks, readCostTreeComments,
		readCostTreeAsks, readCostEntries, readCostAsks, readCostThreads, readCostConversation, readCostBacklinks, readCostChecks,
		readCostUnrelatedDocument*readCostUnrelatedBlocks, readCostRuns)
}

// readCostChainBaseline is the chain read's recorded baseline: its readings, the commit they were
// taken at, the corpus, the pod and the date. A baseline recorded before the p90 was read holds
// none, and reads 0.
type readCostChainBaseline struct {
	RecordedAt string      `json:"recorded_at"`
	Commit     string      `json:"commit"`
	Corpus     string      `json:"corpus"`
	Pod        readCostPod `json:"pod"`
	Runs       int         `json:"runs"`
	P50MS      float64     `json:"p50_ms"`
	P90MS      float64     `json:"p90_ms"`
	P95MS      float64     `json:"p95_ms"`
	P99MS      float64     `json:"p99_ms"`
}

// cells is the baseline as the old side of the chain read's row: its percentiles, "-" for a p90 it
// does not hold, and no database time, which no baseline records.
func (baseline *readCostChainBaseline) cells() readCostCells {
	p90 := "-"
	if baseline.P90MS > 0 {
		p90 = fmt.Sprintf("%.1f", baseline.P90MS)
	}
	return readCostCells{fmt.Sprintf("%.1f", baseline.P50MS), p90, fmt.Sprintf("%.1f", baseline.P95MS), fmt.Sprintf("%.1f", baseline.P99MS), "-", "-"}
}

// readCostChainBound reads the chain read's recorded baseline and answers it where it holds for
// this run: the same corpus on the same kind of pod. Where none is recorded, or the corpus or the
// pod moved, it records this run's reading as the baseline, with the pod and the date, says what
// moved, and answers nil: that run gates the chain read on nothing. The record is printed whole,
// so a run whose working copy is thrown away (a cluster Job) leaves it in its log to be committed
// at readCostChainBaselinePath.
func readCostChainBound(t *testing.T, commit string, reading readCostReading) *readCostChainBaseline {
	t.Helper()
	now := readCostChainBaseline{
		RecordedAt: time.Now().UTC().Format(time.RFC3339), Commit: commit, Corpus: readCostCorpusName(), Pod: readCostPodNow(),
		Runs: readCostRuns, P50MS: msOf(reading.p50), P90MS: msOf(reading.p90), P95MS: msOf(reading.p95), P99MS: msOf(reading.p99),
	}
	var moved []string
	recorded, err := os.ReadFile(readCostChainBaselinePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		moved = append(moved, "no baseline was recorded")
	case err != nil:
		t.Fatalf("read the chain read's baseline %s: %v", readCostChainBaselinePath, err)
	default:
		var before readCostChainBaseline
		if err := json.Unmarshal(recorded, &before); err != nil {
			t.Fatalf("decode the chain read's baseline %s: %v", readCostChainBaselinePath, err)
		}
		if before.Corpus != now.Corpus {
			moved = append(moved, fmt.Sprintf("the corpus moved from %q to %q", before.Corpus, now.Corpus))
		}
		moved = append(moved, now.Pod.moved(before.Pod)...)
		if len(moved) == 0 {
			cells := before.cells()
			t.Logf("c-read-cost: the chain read's baseline was recorded at %s on %s (%s) at %s: p50, p90, p95 and p99 %s ms",
				before.RecordedAt, before.Pod.Host, readCostPodName(before.Pod), before.Commit, strings.Join(cells[:4], ", "))
			return &before
		}
	}
	encoded, err := json.MarshalIndent(now, "", "  ")
	if err != nil {
		t.Fatalf("encode the chain read's baseline: %v", err)
	}
	if err := os.WriteFile(readCostChainBaselinePath, append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("record the chain read's baseline at %s: %v", readCostChainBaselinePath, err)
	}
	compact, _ := json.Marshal(now)
	t.Logf("c-read-cost: the chain read records its own baseline, since %s, and gates on nothing in this run; written to %s: %s",
		strings.Join(moved, "; "), readCostChainBaselinePath, compact)
	return nil
}

// readCostPodName is a pod's kind, as a reader is told it.
func readCostPodName(pod readCostPod) string {
	return fmt.Sprintf("%d CPUs of %s, %d KiB, %s", pod.CPUs, pod.CPUModel, pod.MemoryKiB, pod.Platform)
}

// ───── the run ────────────────────────────────────────────────────────────

// No read this tree moves gets slower, and no event outruns its node.
//
// Six reads are measured, each over HTTP through one client against a server process over one
// database: the inbox, the issue tree expansion, the coverage read, the chain read, the references
// panel with its backlink counts, and search. The old side of each is the answer of the read it
// replaces, on the build of the commit BASELINE_SHA names; the new side is this head's. Beside the
// tree read stand two more old sides of it, which gate nothing yet: the reads the old issue page
// made on its first load to draw the same issue (readCostOldFirstLoad), and those with the reads
// its Verification, Plan and Decisions tabs then made (readCostOldRoleTabs), whose Plan tab's read
// of each task is left out of its readings and taken apart after them. The run has four steps.
// First, it seeds the corpus on the release before, through that build's own writers, and
// measures the old side there. Second, it stops that server and boots this head over the same
// database, which migrates it; it then writes the announcement of another build listing every
// relation kind and the capability of the three children, and boots this head again, so that the
// boot's step deleting what the children kept for one release (startup.Retire) runs, and prints the
// view definition the references panel is then measured over. Third, it waits until every backfill
// of this tree owes nothing, reading each owed count every 2 s and failing after 300 s, and holds
// the corpus to its stated shape through this head's own reads, the tree read among them
// (requireReadCostTreeReadShape). Fourth, and only then, it measures
// the new side. The two sides are minutes apart in one run, which is the comparison a shared box
// allows.
//
// Each read is held to a ratio of its own baseline's p50, as dismissedScaleBounds holds that
// test's operations: the inbox to 2.00 against the reads it replaces, and each other read to twice
// its baseline's p50. The chain read replaces nothing, so its first run records its own baseline
// with the pod and the date and gates on nothing; every later run is held to twice that recorded
// p50. Every p90, p95 and p99 is printed and bound at nothing, since the 198th of 200 readings on
// a Postgres shared with every other suite moves further than a change does. So is each read's
// database time per reading on each side, with the statements behind it, and that of an idle
// interval after it (readCostStatements), where the server's pg_stat_statements can be read;
// where it cannot, the run says so in one line and fails on none of it. The run then reads the
// event horizon from the subscriber's side (TestNoEventOutrunsItsNode) and prints its own wall
// time. READ_COST_READS, for diagnosis, measures only the arms it names, on both sides
// (readCostArmFilter); a run with it set is no reading of the check. READ_COST_KEEP_DATABASE=1,
// for diagnosis, keeps the run's database when the test ends and writes its name to
// $TMPDIR/read-cost-kept-database (readCostKeepDatabase); a run with it set is no reading of the
// check either. READ_COST_PPROF=<arm>, for diagnosis, has this head's server serve its profiler
// and takes one CPU profile of it over that arm's new-side readings, which it writes to
// $TMPDIR/read-cost-<arm>.pprof and logs the top of (readCostPprofArm); a run with it set is no
// reading of the check either.
//
// It fails, never skips, where READ_COST_READS names anything but arms of the check,
// READ_COST_PPROF names anything but one of the arms measured, READ_COST_KEEP_DATABASE is set to
// anything but 1, or BASELINE_SHA is unset or names no 40-character commit, before it builds or
// measures anything: a reading of the replaced reads needs the server of that commit.
func TestNoReadThisTreeMovesGetsSlower(t *testing.T) {
	began := time.Now()
	t.Cleanup(func() {
		t.Logf("c-read-cost: TestNoReadThisTreeMovesGetsSlower took %s of wall time", time.Since(began).Round(time.Second))
	})
	arms := readCostArmFilter(t)
	pprofArm := readCostPprofArm(t, arms)
	keep := readCostKeepDatabase(t)
	baseCommit := readCostBaseCommit(t)
	databaseURL := readCostDatabase(t, keep)
	headCommit := gitOutput(t, ".", "rev-parse", "HEAD")
	if !readCostFullCommit.MatchString(headCommit) || headCommit == baseCommit {
		t.Fatalf("this head's commit %q must be a whole commit other than %s=%s, or the two builds cannot be told apart",
			headCommit, testbaseline.Variable, baseCommit)
	}
	baseBinary := testbaseline.Build(t, baseCommit, testbaseline.Variable)
	headBinary := cmdtest.BuildCommand(t, cmdtest.Command{
		Name:   "envoy-dispatch-head",
		Dir:    envoyModuleRoot(t),
		Target: "./cmd/dispatch",
		Flags:  []string{"-ldflags", "-X main.buildCommit=" + headCommit},
	})
	t.Logf("c-read-cost: the old side runs on the build of %s, the commit %s names; the new side on the build of this "+
		"checkout's working copy, stamped with its HEAD %s", baseCommit, testbaseline.Variable, headCommit)
	client := readCostClient()
	listener := readCostListener(t)
	conn := readCostConnect(t, databaseURL)
	holdReadCostJITOff(t, "c-read-cost", conn)
	statements := newReadCostStatements(t, "c-read-cost", databaseURL)

	// ───── first: the corpus and the old side, on the release before ─────
	base := startReadCostServer(t, client, "the release before ("+baseCommit+")", baseBinary, baseCommit, databaseURL, listener)
	corpus := seedReadCost(t, base)
	waitReadCostOwed(t, conn, base.name, readCostBaseOwed, readCostDrainWait, nil, "")
	readCostVacuum(t, conn, "old side")
	requireReadCostOldShape(t, base, corpus)
	firstLoad := readCostOldFirstLoad(t, base, corpus)
	reads := readCostChosen(t, readCostReads(corpus, firstLoad, readCostOldRoleTabs(t, base, corpus, firstLoad)), arms)
	old := measureReadCostSide(t, statements, base, reads, true, nil, "")
	base.stop(t)

	// ───── second: this head over the same database, booted twice ─────
	first := startReadCostServer(t, client, "this head's first boot ("+headCommit+")", headBinary, headCommit, databaseURL, listener)
	if _, err := conn.Exec(context.Background(), `insert into server_build_announcements (commit, tuple_kinds) values ($1, $2)`,
		readCostOtherBuild, refs.TupleKinds()); err != nil {
		t.Fatalf("announce another build listing every relation kind: %v", err)
	}
	t.Logf("c-read-cost: announced another build, %s, listing every relation kind and capability: %v", readCostOtherBuild, refs.TupleKinds())
	first.stop(t)
	head := startReadCostServerProfiled(t, client, "this head ("+headCommit+")", headBinary, headCommit, databaseURL, listener, pprofArm != "")
	var view string
	if err := conn.QueryRow(context.Background(), `select pg_get_viewdef('graph_edges'::regclass, true)`).Scan(&view); err != nil {
		t.Fatalf("read the definition of graph_edges: %v", err)
	}
	t.Logf("c-read-cost: the references panel is measured over this definition of graph_edges:\n%s", view)

	// ───── third: every owed count at 0, then the corpus's shape at this head ─────
	waitReadCostOwed(t, conn, head.name, readCostHeadOwed, readCostOwedWait,
		func() bool { return strings.Contains(head.process.Output.String(), readCostBackfillDone) }, "boot_backfill_done", headCommit)
	readCostVacuum(t, conn, "new side")
	served, skeleton := requireReadCostNewShape(t, head, corpus)

	// ───── fourth: the new side ─────
	missing := map[string]bool{"inbox": !served}
	now := measureReadCostSide(t, statements, head, reads, false, missing, pprofArm)
	var chain *readCostChainBaseline
	if reading, measured := now["chain"]; measured {
		chain = readCostChainBound(t, headCommit, reading)
	}
	head.stop(t)

	t.Logf("c-read-cost: old side %s, new side %s; %d readings of each read on each side", baseCommit, headCommit, readCostRuns)
	t.Log("c-read-cost: read\t" + strings.Join(readCostHeads("old", "new"), "\t") + "\tdb new/old\tratio of p50s\tbound")
	for _, read := range reads {
		t.Logf("c-read-cost: %s", readCostRow(read, old, now, chain, missing))
		if read.arm == "tree" && read.against == "" && !skeleton {
			t.Logf("c-read-cost: the tree arm's readings are a pre-run: this head's tree read is the read INFRA-1253's spec v213 "+
				"replaces, not yet its skeleton read (pull request #4180), and still answers the %d blocks of tree.md", readCostTreeBlocks)
		}
		if reading := old[read.name]; reading.apart != nil {
			t.Logf("c-read-cost: %s", readCostApartLine(read, reading.apart))
		}
	}
	sides := []struct {
		name     string
		readings map[string]readCostReading
	}{{"old side", old}, {"new side", now}}
	for _, read := range reads {
		for _, side := range sides {
			if reading, ok := side.readings[read.name]; ok {
				logReadCostStatements(t, "c-read-cost", read.name, side.name, reading.database)
			}
		}
	}
	for _, read := range reads {
		for _, side := range sides {
			if reading, ok := side.readings[read.name]; ok {
				t.Logf("c-read-cost: %s, %s, every reading in order, in ms: %v", read.name, side.name, readCostMilliseconds(reading.runs))
			}
		}
	}

	// The bounds, one subtest per read, so a read over its bound names itself and the rest still
	// report theirs. Each is taken on the p50, where this hardware can tell a slower read from its
	// own noise; the p99s above are printed and bound at nothing. A second old side of a read gates
	// nothing, so it has none.
	for _, read := range reads {
		if read.against != "" {
			continue
		}
		t.Run(read.name, func(t *testing.T) {
			reading, measured := now[read.name]
			switch {
			case missing[read.name]:
				t.Errorf("%s: this head answers %s with 404, so the read has no new reading to hold to %.2fx", read.name, read.newRoutes, readCostRatioBound)
			case !measured:
				t.Errorf("%s: no new reading was taken", read.name)
			case read.old == nil && chain == nil:
				t.Logf("%s: this run recorded the read's own baseline and holds it to nothing", read.name)
			case read.old == nil:
				if ratio := msOf(reading.p50) / chain.P50MS; ratio > readCostRatioBound {
					t.Errorf("%s: p50 %.1f ms is %.2fx its recorded baseline's p50 of %.1f ms (%s), over %.2fx",
						read.name, msOf(reading.p50), ratio, chain.P50MS, chain.RecordedAt, readCostRatioBound)
				}
			default:
				before := old[read.name]
				if ratio := float64(reading.p50) / float64(before.p50); ratio > readCostRatioBound {
					t.Errorf("%s: p50 %.1f ms is %.2fx the p50 of %.1f ms of the read it replaces (%s), over %.2fx",
						read.name, msOf(reading.p50), ratio, msOf(before.p50), read.oldRoutes, readCostRatioBound)
				}
			}
		})
	}

	// Last, the event horizon from the subscriber's side: a held commit shows no event, a released
	// one shows the event and its node together, and a rollback shows none ever.
	t.Run("the event horizon: no event outruns its node", TestNoEventOutrunsItsNode)
}

// readCostBaseCommit reads BASELINE_SHA and fails, never skips, where it is unset or names no
// 40-character commit, naming the variable, before anything is built or read; then it holds the
// checkout to that commit (testbaseline.Commit).
func readCostBaseCommit(t *testing.T) string {
	t.Helper()
	named := strings.TrimSpace(os.Getenv(testbaseline.Variable))
	if named == "" {
		t.Fatalf("%s is unset: c-read-cost reads each replaced read on the server of the commit it names, so it measures nothing "+
			"without it; set it to the 40-character commit of the pull request body's %s= line", testbaseline.Variable, testbaseline.Variable)
	}
	if !readCostFullCommit.MatchString(named) {
		t.Fatalf("%s=%q names no 40-character commit: c-read-cost reads the commit the pull request body pins, whole", testbaseline.Variable, named)
	}
	if commit := testbaseline.Commit(t); commit != named {
		t.Fatalf("%s=%s resolves to %s in this checkout", testbaseline.Variable, named, commit)
	}
	return named
}

// readCostRow is one read's row of the table: its two p50s, p90s, p95s and p99s, its two database
// times per reading and its two idle intervals', the ratio of its database times, and the ratio of
// its p50s with the bound that ratio is held to. A second old side of a read (against) is set
// beside that read's new side, and its last cell says it gates nothing and what its note holds.
func readCostRow(read readCostRead, old, now map[string]readCostReading, chain *readCostChainBaseline, missing map[string]bool) string {
	newSide := cmp.Or(read.against, read.name)
	reading, measured := now[newSide]
	before := old[read.name]
	var cells []string
	switch {
	case read.against != "":
		newCells, ratio := readCostNoCells("missing"), "-"
		if measured {
			newCells, ratio = reading.cells(), fmt.Sprintf("%.2f", float64(reading.p50)/float64(before.p50))
		}
		cells = append(readCostTableCells(before.cells(), newCells), readCostDatabaseRatio(before.database, reading.database), ratio,
			fmt.Sprintf("none yet: its new side is that of %s; %s", read.against, read.note))
	case missing[read.name] || !measured:
		cells = append(readCostTableCells(before.cells(), readCostNoCells("missing")),
			"-", "-", fmt.Sprintf("%.2fx on p50: this head does not serve %s", readCostRatioBound, read.newRoutes))
	case read.old == nil && chain == nil:
		cells = append(readCostTableCells(readCostNoCells("-"), reading.cells()),
			"-", "-", "none: this run records the read's own baseline")
	case read.old == nil:
		cells = append(readCostTableCells(chain.cells(), reading.cells()), "-", fmt.Sprintf("%.2f", msOf(reading.p50)/chain.P50MS),
			fmt.Sprintf("%.2fx on p50 of the baseline recorded at %s", readCostRatioBound, chain.RecordedAt))
	default:
		cells = append(readCostTableCells(before.cells(), reading.cells()), readCostDatabaseRatio(before.database, reading.database),
			fmt.Sprintf("%.2f", float64(reading.p50)/float64(before.p50)), fmt.Sprintf("%.2fx on p50 of %s", readCostRatioBound, read.oldRoutes))
	}
	return read.name + "\t" + strings.Join(cells, "\t")
}

// readCostColumns are the columns each side of a read has in a table: its percentiles, its
// database time per reading, and the database time per reading of the idle interval after it.
var readCostColumns = [...]string{"p50 ms", "p90 ms", "p95 ms", "p99 ms", "db ms/read", "idle db ms/read"}

// readCostCells is one side's cells of a read's row, in readCostColumns' order.
type readCostCells [len(readCostColumns)]string

// cells is the reading as one side's cells: "-" for a database time that was not measured.
func (reading readCostReading) cells() readCostCells {
	return readCostCells{fmt.Sprintf("%.1f", msOf(reading.p50)), fmt.Sprintf("%.1f", msOf(reading.p90)), fmt.Sprintf("%.1f", msOf(reading.p95)),
		fmt.Sprintf("%.1f", msOf(reading.p99)), reading.database.readCell(), reading.idle.idleCell()}
}

// readCostNoCells is a side that has no reading, every cell of it cell.
func readCostNoCells(cell string) readCostCells {
	var cells readCostCells
	for column := range cells {
		cells[column] = cell
	}
	return cells
}

// readCostHeads are the heads of the sides' columns, each column for every side in turn, as
// readCostTableCells orders the cells.
func readCostHeads(sides ...string) []string {
	heads := make([]string, 0, len(readCostColumns)*len(sides))
	for _, column := range readCostColumns {
		for _, side := range sides {
			heads = append(heads, side+" "+column)
		}
	}
	return heads
}

// readCostTableCells are the sides' cells, each column for every side in turn.
func readCostTableCells(sides ...readCostCells) []string {
	cells := make([]string, 0, len(readCostColumns)*len(sides))
	for column := range readCostColumns {
		for _, side := range sides {
			cells = append(cells, side[column])
		}
	}
	return cells
}

// readCostMilliseconds is every reading in milliseconds to a tenth, in the order taken.
func readCostMilliseconds(runs []time.Duration) []float64 {
	milliseconds := make([]float64, len(runs))
	for index, run := range runs {
		milliseconds[index] = float64(run.Microseconds()/100) / 10
	}
	return milliseconds
}
