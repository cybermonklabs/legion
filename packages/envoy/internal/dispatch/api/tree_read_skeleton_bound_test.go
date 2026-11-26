//go:build basebuild

package api

// This branch's check c-tree-read-bound: the head read of the tree route answers the skeleton of an
// issue within the three bounds of R8, and pages it within the bounds of nodes and bytes.
//
// TestTheTreeReadAnswersTheSkeletonWithinItsBounds boots the build of BASELINE_SHA, the release
// this branch lands after, seeds four issues through its own routes in the sizes of production and
// reads them as the old issue page does; then it stops that build, boots this checkout's over the
// same database and reads the same issues through the tree route. It carries the basebuild tag,
// as cmd/dispatch's store_census_test.go does, because it builds the server of an earlier commit
// out of the repository (git archive), and no CI job builds the tag. It takes about an hour and is
// run, with the check's replay (TestTheTwoCutsAnswerAsTheUncutWalk, which carries no tag), by name,
// as
//
//	BASELINE_SHA=<the commit the pull request body's BASELINE_SHA= line names> go test -tags basebuild ./internal/dispatch/api/ -run '^(TestTheTreeReadAnswersTheSkeletonWithinItsBounds|TestTheTwoCutsAnswerAsTheUncutWalk)$' -timeout 170m -count=1 -v
//
// from packages/envoy, with DISPATCH_TEST_DATABASE_URL naming a Postgres server it may create a
// database on, which loads pg_stat_statements (shared_preload_libraries=pg_stat_statements,
// pg_stat_statements.track=all): the database time it prints is read there, and the check's median
// stage (packages/dispatch/e2e/acceptance/tree-read-db-medians.sh) judges it over three Jobs.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testbaseline"
	"github.com/sjawhar/envoy/internal/testtmp"
	"github.com/sjawhar/envoy/internal/testwait"
)

// skeletonBoundMS is R8's bound on the p90 of the first page of each read of the tree route, on
// every issue and for every filter. The bound on database time, at most twice the old page's
// median, is the median stage's (packages/dispatch/e2e/acceptance/tree-read-db-medians.sh), over
// the three Jobs' lines, and no one run judges it.
const skeletonBoundMS = 200.0

// The sampling: skeletonWarmUps readings that are not counted, then skeletonReadings readings of
// each read, as the reader with display trees, on each side.
const (
	skeletonWarmUps  = 5
	skeletonReadings = 30
)

// The waits: a boot migrates the corpus, the head's boot fills the head rows of every issue, and
// the block index drains what the seeding left owed.
const (
	skeletonBootWait = 15 * time.Minute
	skeletonBootPoll = 500 * time.Millisecond
	skeletonStopWait = 2 * time.Minute
	skeletonOwedWait = 30 * time.Minute
	skeletonOwedPoll = 2 * time.Second
)

// skeletonWriters is how many writes the seeding keeps in flight at once: the corpus is about
// 9,500 comments, which one at a time cost the run many minutes.
const skeletonWriters = 8

// skeletonProject is the corpus's project.
const skeletonProject = "SKEL"

// skeletonCommentPage is how many comments the read with no view answers, the newest of the
// conversation (R2's page): the check's count, held as the test's own, so a read whose page the
// code made larger fails the count rather than moves it.
const skeletonCommentPage = 200

// skeletonBackfillDone is the line this build logs once its boot backfill is done.
const skeletonBackfillDone = `msg="dispatch: boot backfill done"`

// skeletonShape is one issue of the corpus, in the sizes of production's census (R8): the typed
// blocks of each role document, each holding a paragraph that the blocks read lists as a block of
// its own, the comments and asks, and the blocks of each other document, which are paragraphs.
// label is the stable name each line of its readings prints it by.
type skeletonShape struct {
	name, label                     string
	requirements, checks, planTasks int
	comments, asks                  int
	others                          []int
}

// skeletonShapes are the four issues: the one whose skeleton of 3,006 parts is the largest but one,
// the one of the most comments, the one of the most blocks, and the one of the largest spec.
var skeletonShapes = []skeletonShape{
	{name: "the largest plan", label: "largest-plan", requirements: 31, checks: 53, planTasks: 1_393, comments: 47, asks: 5},
	{name: "the most comments", label: "most-comments", requirements: 3, comments: 9_252},
	{name: "the most blocks", label: "most-blocks", planTasks: 730, comments: 113, asks: 5, others: skeletonOtherDocuments()},
	{name: "the largest spec", label: "largest-spec", requirements: 1_393},
}

// The view content each issue holds beside its census shape, so that each view reads something on
// both sides: c-tree-read-bound seeds "a readable role document and matching content on the old
// side where a fixture would not exercise a view", and "decisions and answered asks so the
// Decisions measurements are not empty". Where the census gives a role document no typed block,
// the issue's holds skeletonViewBlocks of them; each spec holds skeletonDecisions decision blocks
// where the release before declares that type (skeletonBaseDecides); each issue the census gives
// no ask holds skeletonViewAsks; and a person answers skeletonAnswered of each issue's asks.
const (
	skeletonViewBlocks = 3
	skeletonDecisions  = 2
	skeletonViewAsks   = 2
	skeletonAnswered   = 2
)

// skeletonHeld is count, or skeletonViewBlocks where the census gives none.
func skeletonHeld(count int) int {
	if count == 0 {
		return skeletonViewBlocks
	}
	return count
}

// heldAsks is how many asks the issue holds: its census's, or skeletonViewAsks where it gives none.
func (shape skeletonShape) heldAsks() int {
	if shape.asks == 0 {
		return skeletonViewAsks
	}
	return shape.asks
}

// skeletonOtherDocuments is the 65 other documents of the issue of the most blocks: 45,008 blocks
// in all, the largest of 5,600 and the rest as even as whole blocks make them.
func skeletonOtherDocuments() []int {
	documents := []int{5_600}
	rest, count := 45_008-5_600, 64
	for index := range count {
		size := rest / count
		if index < rest%count {
			size++
		}
		documents = append(documents, size)
	}
	return documents
}

// skeletonIssue is one issue of the corpus as seeded: its key, its role documents' ids, the ids of
// its other documents, and how many decision blocks its spec holds.
type skeletonIssue struct {
	skeletonShape
	key                      string
	spec, verification, plan string
	documents                []string
	decisions                int
}

// ───── the two servers and the run's database ─────────────────────────────

// startSkeletonServer starts binary, a build stamped with commit, over databaseURL on a loopback
// port the test holds until the process serves on it, and waits until it serves. It inherits
// nothing from the test's environment: a home of its own, the person named by a trusted header,
// the shared agent bearer, no NATS, and the settings that stop the background work a setting
// stops; a build that lacks a setting ignores it.
func startSkeletonServer(t *testing.T, client *http.Client, name, binary, commit, databaseURL, listener string) *readCostServer {
	t.Helper()
	home := t.TempDir()
	held := cmdtest.HoldPort(t)
	environment := map[string]string{
		"HOME":                                     home,
		"XDG_CONFIG_HOME":                          home,
		"XDG_DATA_HOME":                            home,
		"DATABASE_URL":                             databaseURL,
		"DISPATCH_AGENT_TOKEN":                     readCostAgentToken,
		"DISPATCH_IDENTITY":                        "header:X-Dispatch-User",
		"DISPATCH_IDENTITY_HEADER_TRUSTED":         "1",
		"DISPATCH_LISTEN_HOST":                     "127.0.0.1",
		"DISPATCH_PORT":                            strconv.Itoa(held.Port),
		"DISPATCH_WEB_DIST":                        home,
		"DISPATCH_NATS_DISABLED":                   "1",
		"DISPATCH_SIGNING_KEY":                     readCostSigningKey,
		"DISPATCH_TEST_HOOKS":                      "1",
		"ENVOY_URL":                                listener,
		"DISPATCH_SEARCH_FILL":                     "off",
		"DISPATCH_TEST_EMBEDDER_OFF":               "1",
		"DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL": "24h",
	}
	cmd := exec.Command(binary)
	cmd.Dir = home
	for variable, value := range environment {
		cmd.Env = append(cmd.Env, variable+"="+value)
	}
	slices.Sort(cmd.Env)
	held.Pass(cmd, "DISPATCH_LISTEN_FD")
	server := &readCostServer{name: name, commit: commit, port: held.Port, process: cmdtest.Start(t, name, cmd), client: client}
	started := time.Now()
	listening := `msg="dispatch: listening" addr=127.0.0.1:` + strconv.Itoa(server.port)
	exited := false
	served := testwait.Holds(skeletonBootWait, skeletonBootPoll, func() bool {
		select {
		case <-server.process.Exited:
			exited = true
			return true
		default:
		}
		if !strings.Contains(server.process.Output.String(), listening) {
			return false
		}
		response, err := client.Get(server.url("/healthz"))
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
	switch {
	case exited:
		t.Fatalf("%s exited before it served:\n%s", name, skeletonTail(server.process.Output.String()))
	case !served:
		t.Fatalf("%s did not serve within %s:\n%s", name, skeletonBootWait, skeletonTail(server.process.Output.String()))
	}
	t.Logf("%s serves at %s, %s after it started", name, server.url(""), time.Since(started).Round(time.Millisecond))
	return server
}

// stopSkeletonServer sends the process SIGTERM, as a deploy stops it, and waits until it has
// exited, so the next build boots over a database no process of this one serves.
func stopSkeletonServer(t *testing.T, s *readCostServer) {
	t.Helper()
	s.process.Terminate(t)
	stopped := testwait.Holds(skeletonStopWait, skeletonBootPoll, func() bool {
		select {
		case <-s.process.Exited:
			return true
		default:
			return false
		}
	})
	if !stopped {
		t.Fatalf("%s was still running %s after SIGTERM:\n%s", s.name, skeletonStopWait, skeletonTail(s.process.Output.String()))
	}
	t.Logf("%s stopped", s.name)
}

// skeletonTail is the last lines of a process's output, which a failure quotes.
func skeletonTail(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-60):], "\n")
}

// skeletonListener is a stand-in Envoy listener, which answers every session as live: the corpus
// sends no message, and a build that asks finds one.
func skeletonListener(t *testing.T) string {
	t.Helper()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch session, lookup := strings.CutPrefix(r.URL.Path, "/v1/registry/"); {
		case r.Method == http.MethodGet && lookup:
			_ = json.NewEncoder(w).Encode(map[string]any{"session_id": session, "title": session, "capabilities": []string{}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(listener.Close)
	return listener.URL
}

// skeletonDatabase creates an empty database for this run alone on the server
// DISPATCH_TEST_DATABASE_URL names and answers its URL; it is dropped when the test ends. It is
// empty because the release before migrates it itself. The variable unset fails the run, since a
// skipped measurement reads as one that held.
func skeletonDatabase(t *testing.T) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(storetest.DatabaseURLEnv))
	if raw == "" {
		t.Fatalf("%s is unset: c-tree-read-bound boots both builds over a database it creates on that server", storetest.DatabaseURLEnv)
	}
	server, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("read %s: %v", storetest.DatabaseURLEnv, err)
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("name the run's database: %v", err)
	}
	name := "tree_read_bound_" + hex.EncodeToString(suffix)
	admin := *server
	admin.Path = "/postgres"
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		t.Fatalf("connect to the test server: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		t.Fatalf("create the run's database: %v", err)
	}
	t.Cleanup(func() { dropSkeletonDatabase(t, admin.String(), name) })
	own := *server
	own.Path = "/" + name
	t.Logf("the run's database is %s", name)
	return own.String()
}

// dropSkeletonDatabase drops a database of the run once every process on it has gone, trying again
// while Postgres still tears down a backend whose client has closed (55006).
func dropSkeletonDatabase(t *testing.T, adminURL, name string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Logf("drop the database %s: connect: %v", name, err)
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
			t.Logf("drop the database %s: %v", name, err)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// skeletonBaseBuild builds `./cmd/dispatch` at commit, the one BASELINE_SHA names, out of that
// commit's own tree, which `git archive` writes into a directory of this test's own, stamped with
// that commit as the image build stamps one, as cmd/dispatch's baseBuildOf builds it.
func skeletonBaseBuild(t *testing.T, commit string) string {
	t.Helper()
	root := gitOutput(t, ".", "rev-parse", "--show-toplevel")
	source := testtmp.Dir(t, filepath.Join("packages", "envoy"))
	archive := exec.Command("git", "-C", root, "archive", commit)
	extract := exec.Command("tar", "-x", "-C", source)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		t.Fatalf("read the tree of %s: %v", commit, err)
	}
	var problems strings.Builder
	archive.Stderr = &problems
	extract.Stdin = pipe
	if err := extract.Start(); err != nil {
		t.Fatalf("extract the tree of %s: %v", commit, err)
	}
	if err := archive.Start(); err != nil {
		t.Fatalf("read the tree of %s: %v", commit, err)
	}
	if err := archive.Wait(); err != nil {
		t.Fatalf("read the tree of %s: %v\n%s", commit, err, problems.String())
	}
	if err := extract.Wait(); err != nil {
		t.Fatalf("extract the tree of %s: %v", commit, err)
	}
	return cmdtest.BuildCommand(t, cmdtest.Command{
		Name:   "envoy-dispatch-base",
		Dir:    filepath.Join(source, "packages", "envoy"),
		Target: "./cmd/dispatch",
		Flags:  []string{"-ldflags", "-X main.buildCommit=" + commit},
		Env:    os.Environ(),
	})
}

// skeletonBaseCommit reads BASELINE_SHA and fails, never skips, where it is unset or names no
// 40-character commit this checkout holds, naming the variable, before anything is built.
func skeletonBaseCommit(t *testing.T) string {
	t.Helper()
	named := strings.TrimSpace(os.Getenv(testbaseline.Variable))
	if named == "" {
		t.Fatalf("%s is unset: c-tree-read-bound reads the old issue page on the server of the commit it names, the release this "+
			"branch lands after; set it to the 40-character commit of the pull request body's %s= line", testbaseline.Variable, testbaseline.Variable)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(named) {
		t.Fatalf("%s=%q names no 40-character commit", testbaseline.Variable, named)
	}
	if commit := testbaseline.Commit(t); commit != named {
		t.Fatalf("%s=%s resolves to %s in this checkout", testbaseline.Variable, named, commit)
	}
	return named
}

// skeletonConnect opens the test's own connection to the run's database, for the owed counts, the
// marks and analyze, none of which is a read the run measures.
func skeletonConnect(t *testing.T, databaseURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect to the run's database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// skeletonAnalyze analyzes the run's database before a side is read: a freshly written table has
// no statistics, and a plan chosen over that is one production never runs.
func skeletonAnalyze(t *testing.T, conn *pgx.Conn, side string) {
	t.Helper()
	started := time.Now()
	if _, err := conn.Exec(context.Background(), `analyze`); err != nil {
		t.Fatalf("analyze before the %s is read: %v", side, err)
	}
	t.Logf("analyze before the %s is read took %s", side, time.Since(started).Round(time.Millisecond))
}

// waitSkeletonOwed waits until no document owes its block index, and, where done is given, until
// done holds, polling every skeletonOwedPoll for skeletonOwedWait.
func waitSkeletonOwed(t *testing.T, conn *pgx.Conn, side string, done func() (bool, string)) {
	t.Helper()
	var owed int
	why := ""
	held := testwait.Holds(skeletonOwedWait, skeletonOwedPoll, func() bool {
		if err := conn.QueryRow(context.Background(),
			`select count(*) from document_block_versions where version is null or store_owed_at is not null`).Scan(&owed); err != nil {
			why = err.Error()
			return false
		}
		if owed > 0 {
			why = fmt.Sprintf("%d documents owe their block index", owed)
			return false
		}
		if done != nil {
			ok, reason := done()
			why = reason
			return ok
		}
		return true
	})
	if !held {
		t.Fatalf("the %s did not settle within %s: %s", side, skeletonOwedWait, why)
	}
}

// skeletonOwed is how many documents and how many comments, messages and asks the block index's
// window arm owes their windows, as the build that serves reads them: a seeded corpus leaves every
// document and comment owed, and the arm writes 200 owners a tick, so until it is done its
// statements run beside every reading at a rate of their own.
type skeletonOwed struct{ documents, items int }

func (owed skeletonOwed) String() string {
	return fmt.Sprintf("%d documents and %d comments, messages and asks", owed.documents, owed.items)
}

// skeletonOwedLimit is the most owners one count of the window arm's backlog lists of each kind,
// past every owner of the corpus.
const skeletonOwedLimit = 1 << 20

// skeletonBaseDocumentsOwedWindows is the release before's read of the documents its window arm
// owes windows (refs' documentsOwedWindowsQuery at BASELINE_SHA): this head's reads a table that
// release's schema does not hold yet (document_rooms), so the old side is counted by its own.
const skeletonBaseDocumentsOwedWindows = `
	select count(*) from document_block_versions v
	left join lateral (
		select 1 from document_blocks b where b.artifact_id = v.artifact_id limit 1
	) held (block) on true
	where v.windows_owed_at is not null and (v.version is not null or held.block is not null)`

// windowsOwed counts the window arm's backlog over the test's own connection to the run's
// database: this head's reads of it (refs.DocumentsOwedWindows, refs.ItemsOwedWindows) on this
// head's side, and the release before's own read of the documents on its side. It is read only
// between the intervals pg_stat_statements is read over.
func (s *skeletonStatements) windowsOwed(t *testing.T) skeletonOwed {
	t.Helper()
	ctx := context.Background()
	var owed skeletonOwed
	if s.head {
		documents, err := refs.DocumentsOwedWindows(ctx, s.run, "", skeletonOwedLimit)
		if err != nil {
			t.Fatalf("c-tree-read-bound: count the documents owed their windows: %v", err)
		}
		owed.documents = len(documents)
	} else if err := s.run.QueryRow(ctx, skeletonBaseDocumentsOwedWindows).Scan(&owed.documents); err != nil {
		t.Fatalf("c-tree-read-bound: count the documents owed their windows: %v", err)
	}
	for _, kind := range []refs.ItemKind{refs.ItemComment, refs.ItemMessage, refs.ItemAsk} {
		items, err := refs.ItemsOwedWindows(ctx, s.run, kind, "", skeletonOwedLimit)
		if err != nil {
			t.Fatalf("c-tree-read-bound: count the %ss owed their windows: %v", kind, err)
		}
		owed.items += len(items)
	}
	return owed
}

// waitWindowsOwed waits until the window arm owes no document and no item its windows, polling
// every skeletonOwedPoll for skeletonOwedWait, and says what it waited for and how long: the
// backlog the seeding or this head's boot left is a loop that ends, which the readings wait out
// rather than count or subtract.
func (s *skeletonStatements) waitWindowsOwed(t *testing.T, side string) {
	t.Helper()
	started := time.Now()
	first := s.windowsOwed(t)
	owed := first
	held := testwait.Holds(skeletonOwedWait, skeletonOwedPoll, func() bool {
		owed = s.windowsOwed(t)
		return owed == skeletonOwed{}
	})
	if !held {
		t.Fatalf("c-tree-read-bound: the %s's window arm still owes the windows of %s after %s", side, owed, skeletonOwedWait)
	}
	t.Logf("c-tree-read-bound: the %s's window arm owed the windows of %s; none is owed after %s", side, first,
		time.Since(started).Round(time.Second))
}

// ───── the corpus, written through the release before's own routes ─────────

// skeletonTyped is a document of count typed blocks of directive, each holding one paragraph, with
// attributes for each block's id.
func skeletonTyped(directive, prefix string, count int, attributes func(id string) string) string {
	var text strings.Builder
	for index := range count {
		id := fmt.Sprintf("%s%d", prefix, index+1)
		fmt.Fprintf(&text, ":::%s{#%s%s}\nThe %s %s holds one paragraph.\n:::\n\n", directive, id, attributes(id), directive, id)
	}
	return text.String()
}

// skeletonProse is a document of count paragraphs.
func skeletonProse(document string, count int) string {
	var text strings.Builder
	for index := range count {
		fmt.Fprintf(&text, "Paragraph %d of %s, written to be read when its reader opens it.\n\n", index+1, document)
	}
	return text.String()
}

// skeletonInParallel runs job for each index below count on skeletonWriters goroutines, and fails
// with the first error a job answered.
func skeletonInParallel(t *testing.T, what string, count int, job func(index int) error) {
	t.Helper()
	var next sync.Mutex
	nextIndex := 0
	var failure error
	var failed sync.Once
	var group sync.WaitGroup
	for range skeletonWriters {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				next.Lock()
				index := nextIndex
				nextIndex++
				next.Unlock()
				if index >= count || failure != nil {
					return
				}
				if err := job(index); err != nil {
					failed.Do(func() { failure = err })
					return
				}
			}
		}()
	}
	group.Wait()
	if failure != nil {
		t.Fatalf("%s: %v", what, failure)
	}
}

// skeletonBaseDecides reports whether the release before, the build of commit, declares the typed
// block `decision` (its pmdoc/schema/blocks.json, read out of that commit's own tree): a release
// that declares none takes no decision block, and its old page reads a person's answered asks as
// its only decisions.
func skeletonBaseDecides(t *testing.T, commit string) bool {
	t.Helper()
	var schema struct {
		Types []struct {
			Name string `json:"name"`
		} `json:"types"`
	}
	raw := gitOutput(t, ".", "show", commit+":packages/envoy/internal/dispatch/pmdoc/schema/blocks.json")
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatalf("read the block schema of %s: %v", commit, err)
	}
	for _, declared := range schema.Types {
		if declared.Name == refs.DecisionSupertag {
			return true
		}
	}
	return false
}

// seedSkeleton writes the four issues through s, as the writer, a person: each issue with its
// spec, its Verification document, its plan, its other documents, its asks, of which the writer
// answers skeletonAnswered, and its comments, each a person's, so each comment is a conversation
// node (R2). Each role document holds the census's typed blocks, or skeletonViewBlocks where the
// census gives none, and the spec skeletonDecisions decision blocks where decides is true.
func seedSkeleton(t *testing.T, s *readCostServer, decides bool) []*skeletonIssue {
	t.Helper()
	started := time.Now()
	for _, person := range []string{readCostReader, readCostWriter} {
		s.expect(t, "sign in "+person, http.MethodGet, "/api/v1/whoami", person, nil, http.StatusOK)
	}
	s.expect(t, "create the project "+skeletonProject, http.MethodPost, "/api/v1/projects", readCostWriter,
		map[string]string{"key": skeletonProject, "name": "The skeleton's corpus"}, http.StatusCreated)
	none := func(string) string { return "" }
	var issues []*skeletonIssue
	for _, shape := range skeletonShapes {
		issue := &skeletonIssue{skeletonShape: shape}
		spec := skeletonTyped("requirement", "r", skeletonHeld(shape.requirements), none)
		if decides {
			issue.decisions = skeletonDecisions
			spec += skeletonTyped(refs.DecisionSupertag, "d", skeletonDecisions, none)
		}
		issue.key = createReadCostIssue(t, s, skeletonProject, "The issue of "+shape.name, spec, readCostWriter)
		serves := func(string) string {
			return ` serves="` + refs.BlockRef("dispatch://"+issue.key+"/spec", "r1") + `" surface="cli" run="go test ./..."`
		}
		issue.verification = uploadReadCostDocument(t, s, issue.key, "verification.md",
			skeletonTyped(graphstore.BlockTypeCheck, "k", skeletonHeld(shape.checks), serves), readCostWriter)
		issue.plan = uploadReadCostDocument(t, s, issue.key, "plan.md", skeletonTyped("plan_task", "p", skeletonHeld(shape.planTasks), none), readCostWriter)
		for index, blocks := range shape.others {
			name := fmt.Sprintf("notes-%02d.md", index+1)
			issue.documents = append(issue.documents, uploadReadCostDocument(t, s, issue.key, name, skeletonProse(name, blocks), readCostWriter))
		}
		for index := range shape.heldAsks() {
			asked := s.expect(t, fmt.Sprintf("ask %d on %s", index+1, issue.key), http.MethodPost, "/api/v1/issues/"+issue.key+"/asks", readCostWriter,
				map[string]any{"question": fmt.Sprintf("Question %d: which way do we take?", index+1), "options": []map[string]string{{"label": "Yes"}, {"label": "No"}}},
				http.StatusCreated)
			if index >= skeletonAnswered {
				continue
			}
			var ask struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(asked.body, &ask); err != nil || ask.ID == "" {
				t.Fatalf("read ask %d of %s back: %v: %s", index+1, issue.key, err, asked)
			}
			s.expect(t, fmt.Sprintf("answer ask %d of %s", index+1, issue.key), http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", readCostWriter,
				map[string]any{"selected": []string{"Yes"}, "text": fmt.Sprintf("Yes, answer %d: we take that way.", index+1)}, http.StatusOK)
		}
		skeletonInParallel(t, "write the comments of "+issue.key, shape.comments, func(index int) error {
			_, err := s.write(http.MethodPost, "/api/v1/issues/"+issue.key+"/comments", readCostWriter,
				map[string]any{"body": fmt.Sprintf("Comment %d of %s: a note on the work.", index+1, issue.key)}, http.StatusCreated)
			return err
		})
		issues = append(issues, issue)
		t.Logf("c-tree-read-bound: seeded %s, the issue of %s (%s): %d requirements, %d decisions, %d checks, %d plan tasks, %d comments, %d asks of which %d answered, %d other documents",
			issue.key, shape.name, shape.label, skeletonHeld(shape.requirements), issue.decisions, skeletonHeld(shape.checks), skeletonHeld(shape.planTasks),
			shape.comments, shape.heldAsks(), skeletonAnswered, len(shape.others))
	}
	for _, issue := range issues {
		var read struct {
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}
		s.read(t, "read "+issue.key, "/api/v1/issues/"+issue.key, &read)
		issue.spec = read.PrimaryArtifactID
	}
	t.Logf("c-tree-read-bound: seeding took %s", time.Since(started).Round(time.Second))
	return issues
}

// ───── database time, from pg_stat_statements ──────────────────────────────

// skeletonStatements reads pg_stat_statements for the top-level statements of the run's database
// and role, over a connection to a database of its own, as c-read-cost reads it
// (readCostStatements on the root's branch): a snapshot before a batch of readings and one after,
// and their difference per reading over the statements that ran at least once per reading.
type skeletonStatements struct {
	conn     *pgx.Conn
	database uint32
	role     uint32
	// run is the test's own connection to the run's database, which `show jit` and the window
	// arm's backlog (windowsOwed) are read over; head says this head's build serves it.
	run  *pgx.Conn
	head bool
}

// jit is `show jit` as the run's database answers it, and it fails the run where it reads other
// than off: production's Postgres compiles no statement, so every speed and database-time test of
// this branch reads with jit off, and a reading that compiles them spends what production never
// does.
func (s *skeletonStatements) jit(t *testing.T) string {
	t.Helper()
	var jit string
	if err := s.run.QueryRow(context.Background(), `show jit`).Scan(&jit); err != nil {
		t.Fatalf("c-tree-read-bound: read show jit on the run's database: %v", err)
	}
	if jit != "off" {
		t.Fatalf("c-tree-read-bound: show jit reads %q on the run's database, want off: production compiles no statement, so no reading "+
			"of this run is the one the bounds hold", jit)
	}
	return jit
}

// skeletonSnapshot is pg_stat_statements at one moment, by queryid.
type skeletonSnapshot struct {
	at         time.Time
	statements map[int64]skeletonStatementTotal
	evicted    int64
}

// skeletonStatementTotal is one statement's text, calls and total_exec_time in milliseconds.
type skeletonStatementTotal struct {
	query string
	calls int64
	ms    float64
}

// skeletonDatabaseTime is the database time of one interval per reading of the batch it is read
// against: ms and calls of the statements that ran at least once per reading, otherMS and others of
// those that ran fewer times, the calls per reading of each statement by its normalized text, and
// the time per reading of each statement that ran at least once per reading, by its text; and, for
// a batch, the window arm's backlog before it and after the idle interval that follows it.
type skeletonDatabaseTime struct {
	readings   int
	length     time.Duration
	ms         float64
	calls      float64
	otherMS    float64
	others     int
	evicted    int64
	byText     map[string]float64
	spentBy    map[string]float64
	owedBefore skeletonOwed
	owedAfter  skeletonOwed
}

// newSkeletonStatements opens what database time is read with, for the run's database at
// databaseURL: a database of its own beside it, dropped when the test ends, holding the extension.
// Where pg_stat_statements cannot be read the run fails, printing that database time was not
// measured and why, because two of the bounds hold on it. runConn is the test's own connection to
// the run's database, which `show jit` is read over.
func newSkeletonStatements(t *testing.T, databaseURL string, runConn *pgx.Conn) *skeletonStatements {
	t.Helper()
	unmeasured := func(format string, args ...any) *skeletonStatements {
		t.Helper()
		t.Fatalf("c-tree-read-bound: database time was not measured: %s; it needs pg_stat_statements in the server's "+
			"shared_preload_libraries with pg_stat_statements.track=all, and a role that may create the extension", fmt.Sprintf(format, args...))
		return nil
	}
	run, err := url.Parse(databaseURL)
	if err != nil {
		return unmeasured("read the run's database URL: %v", err)
	}
	database := strings.TrimPrefix(run.Path, "/")
	own := database + "_statements"
	admin, statements := *run, *run
	admin.Path, statements.Path = "/postgres", "/"+own
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return unmeasured("connect to the test server: %v", err)
	}
	_, err = conn.Exec(ctx, "create database "+own+" strategy file_copy")
	_ = conn.Close(ctx)
	if err != nil {
		return unmeasured("create %s, the database the snapshots are read from: %v", own, err)
	}
	t.Cleanup(func() { dropSkeletonDatabase(t, admin.String(), own) })
	s := &skeletonStatements{}
	if s.conn, err = pgx.Connect(ctx, statements.String()); err != nil {
		return unmeasured("connect to %s: %v", own, err)
	}
	t.Cleanup(func() { _ = s.conn.Close(context.Background()) })
	if _, err := s.conn.Exec(ctx, `create extension if not exists pg_stat_statements`); err != nil {
		return unmeasured("create extension pg_stat_statements in %s: %v", own, err)
	}
	var track string
	if err := s.conn.QueryRow(ctx, `select (select oid from pg_database where datname = $1),
		(select oid from pg_roles where rolname = current_user), coalesce(current_setting('pg_stat_statements.track', true), '')`,
		database).Scan(&s.database, &s.role, &track); err != nil {
		return unmeasured("read the oids of the run's database %s and of its role: %v", database, err)
	}
	if _, err := s.snapshot(ctx); err != nil {
		return unmeasured("%v", err)
	}
	if track != "top" && track != "all" {
		return unmeasured("pg_stat_statements.track is %q, so it records no statement", track)
	}
	s.run = runConn
	t.Logf("c-tree-read-bound: database time is read from pg_stat_statements (track=%s) for the top-level statements of the run's "+
		"database %s (oid %d) and its role (oid %d), over a connection to %s; show jit = %s", track, database, s.database, s.role, own, s.jit(t))
	return s
}

// snapshot reads pg_stat_statements for the run's database and role.
func (s *skeletonStatements) snapshot(ctx context.Context) (skeletonSnapshot, error) {
	snapshot := skeletonSnapshot{at: time.Now(), statements: map[int64]skeletonStatementTotal{}}
	if err := s.conn.QueryRow(ctx, `select dealloc from pg_stat_statements_info`).Scan(&snapshot.evicted); err != nil {
		return snapshot, fmt.Errorf("read pg_stat_statements_info: %w", err)
	}
	rows, err := s.conn.Query(ctx, `select queryid, query, calls, total_exec_time from pg_stat_statements(true)
		where dbid = $1 and userid = $2 and toplevel`, s.database, s.role)
	if err != nil {
		return snapshot, fmt.Errorf("read pg_stat_statements: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var query int64
		var total skeletonStatementTotal
		if err := rows.Scan(&query, &total.query, &total.calls, &total.ms); err != nil {
			return snapshot, fmt.Errorf("read pg_stat_statements: %w", err)
		}
		snapshot.statements[query] = total
	}
	return snapshot, rows.Err()
}

// measure runs batch, which takes readings readings, between two snapshots, then waits an idle
// interval of the batch's length with nothing reading before a third: the database time of the
// batch and of the idle interval, each per reading of the batch, the batch's with the window arm's
// backlog read before the first snapshot and after the third. A snapshot it cannot read fails the
// run, printing that database time was not measured.
func (s *skeletonStatements) measure(t *testing.T, readings int, batch func()) (database, idle *skeletonDatabaseTime) {
	t.Helper()
	ctx := context.Background()
	owed := s.windowsOwed(t)
	before, err := s.snapshot(ctx)
	if err != nil {
		t.Fatalf("c-tree-read-bound: database time was not measured: %v", err)
	}
	batch()
	after, err := s.snapshot(ctx)
	if err != nil {
		t.Fatalf("c-tree-read-bound: database time was not measured: %v", err)
	}
	database = skeletonDatabaseTimeOf(before, after, readings)
	time.Sleep(database.length)
	rested, err := s.snapshot(ctx)
	if err != nil {
		t.Fatalf("c-tree-read-bound: database time was not measured over the idle interval: %v", err)
	}
	database.owedBefore, database.owedAfter = owed, s.windowsOwed(t)
	return database, skeletonDatabaseTimeOf(after, rested, readings)
}

// skeletonDatabaseTimeOf is the database time between two snapshots per reading of a batch.
func skeletonDatabaseTimeOf(before, after skeletonSnapshot, readings int) *skeletonDatabaseTime {
	spent := &skeletonDatabaseTime{readings: readings, length: after.at.Sub(before.at), evicted: after.evicted - before.evicted,
		byText: map[string]float64{}, spentBy: map[string]float64{}}
	for query, now := range after.statements {
		was := before.statements[query]
		if now.calls < was.calls {
			was = skeletonStatementTotal{}
		}
		calls, ms := now.calls-was.calls, now.ms-was.ms
		switch {
		case calls >= int64(readings):
			spent.ms += ms
			spent.calls += float64(calls)
			spent.spentBy[strings.Join(strings.Fields(now.query), " ")] += ms / float64(readings)
		case calls > 0:
			spent.otherMS += ms
			spent.others++
		}
		if calls > 0 {
			spent.byText[skeletonNormalized(now.query)] += float64(calls) / float64(readings)
		}
	}
	spent.ms /= float64(readings)
	spent.calls /= float64(readings)
	spent.otherMS /= float64(readings)
	return spent
}

// skeletonLiteral is a constant pg_stat_statements writes as a parameter, and a parameter itself.
var skeletonLiteral = regexp.MustCompile(`'(?:[^']|'')*'|\$\d+|\b\d+(?:\.\d+)?\b`)

// skeletonNormalized is a statement's text with each constant and each parameter written `?` and
// its spaces collapsed, which pg_stat_statements' text of it and the text the server sent agree on.
func skeletonNormalized(query string) string {
	return strings.Join(strings.Fields(strings.ToLower(skeletonLiteral.ReplaceAllString(query, "?"))), " ")
}

// line is the database time a read's line prints beside its percentiles.
func (spent *skeletonDatabaseTime) line(idle *skeletonDatabaseTime) string {
	line := fmt.Sprintf("database time %.2f ms per reading in %.1f statements, beside %.2f ms per reading in %d statements that ran fewer than %d times",
		spent.ms, spent.calls, spent.otherMS, spent.others, spent.readings)
	evicted := spent.evicted
	if idle != nil {
		line += fmt.Sprintf(", and %.2f ms per reading over an idle %s after the batch, %.2f ms of it in statements that ran %d times or more",
			idle.ms+idle.otherMS, idle.length.Round(time.Millisecond), idle.ms, idle.readings)
		evicted += idle.evicted
	}
	if evicted > 0 {
		line += fmt.Sprintf("; pg_stat_statements evicted %d entries meanwhile, and the time they held is not counted", evicted)
	}
	line += fmt.Sprintf("; the window arm owed the windows of %s before the batch and of %s after its idle interval", spent.owedBefore, spent.owedAfter)
	queries := slices.SortedFunc(maps.Keys(spent.spentBy), func(left, right string) int {
		return cmp.Compare(spent.spentBy[right], spent.spentBy[left])
	})
	for index, query := range queries[:min(3, len(queries))] {
		line += fmt.Sprintf("; top %d: %.2f ms per reading: %.100s", index+1, spent.spentBy[query], query)
	}
	return line
}

// ───── the loaders the read sends ──────────────────────────────────────────

// sqlTaken is a refs.Queryer that records the SQL of each statement a read would send and runs
// none: each statement answers errSQLTaken, so a loader's text is read off it without the run's
// database ever running it, and pg_stat_statements holds only what the builds sent.
type sqlTaken struct{ sql []string }

// errSQLTaken is what every statement sqlTaken is given answers.
var errSQLTaken = errors.New("the statement was taken, not run")

func (taken *sqlTaken) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	taken.sql = append(taken.sql, sql)
	return nil, errSQLTaken
}

func (taken *sqlTaken) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	taken.sql = append(taken.sql, sql)
	return takenRow{}
}

// takenRow is the row of a statement sqlTaken did not run.
type takenRow struct{}

func (takenRow) Scan(...any) error { return errSQLTaken }

// skeletonLoaders is the normalized text of the statement of each loader of R8 that the read with
// no view sends, taken off one call of each that runs nothing (sqlTaken): the facts read
// (refs.TreeFactsOf) and the rows read (refs.TreeNodeRows).
func skeletonLoaders(t *testing.T, key string) map[string]string {
	t.Helper()
	ctx := context.Background()
	issue := refs.Target{Kind: graphstore.KindIssue, ID: key}
	loaders := map[string]func(taken *sqlTaken) error{
		"refs.TreeFactsOf": func(taken *sqlTaken) error {
			_, err := refs.TreeFactsOf(ctx, taken, []refs.Target{issue}, refs.TreeCut{ReadSeq: 1, At: time.Now()})
			return err
		},
		"refs.TreeNodeRows": func(taken *sqlTaken) error {
			_, err := refs.TreeNodeRows(ctx, taken, []refs.Target{issue})
			return err
		},
	}
	texts := map[string]string{}
	for name, load := range loaders {
		taken := &sqlTaken{}
		if err := load(taken); !errors.Is(err, errSQLTaken) || len(taken.sql) != 1 {
			t.Fatalf("take the statement of %s: %d statements, %v", name, len(taken.sql), err)
		}
		texts[name] = skeletonNormalized(taken.sql[0])
	}
	return texts
}

// ───── the readings ────────────────────────────────────────────────────────

// skeletonReading is one read's readings on one side: the p50, p90 and p99 of its readings, the
// bytes of its answer, and its database time per reading with the idle interval after it.
type skeletonReading struct {
	what           string
	p50, p90, p99  time.Duration
	bytes          int
	database, idle *skeletonDatabaseTime
}

// measureSkeletonRead reads path on s as the reader with display trees, skeletonWarmUps times
// uncounted and skeletonReadings times between two snapshots of pg_stat_statements, and prints its
// line: the p50, the p90 and the p99 in ms, the bytes of the answer, and the database time and the
// statements per reading.
func measureSkeletonRead(t *testing.T, statements *skeletonStatements, s *readCostServer, what, path string) skeletonReading {
	t.Helper()
	read := func() readCostAnswer {
		answer, err := s.send(http.MethodGet, path, readCostReader, nil)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if answer.status != http.StatusOK {
			t.Fatalf("%s: GET %s on %s answered %s", what, path, s.name, answer)
		}
		return answer
	}
	for range skeletonWarmUps {
		read()
	}
	reading := skeletonReading{what: what}
	var runs []time.Duration
	reading.database, reading.idle = statements.measure(t, skeletonReadings, func() {
		for range skeletonReadings {
			answer := read()
			runs = append(runs, answer.took)
			reading.bytes = len(answer.body)
		}
	})
	slices.Sort(runs)
	reading.p50 = percentileOfDismissedScale(runs, 0.50)
	reading.p90 = percentileOfDismissedScale(runs, 0.90)
	reading.p99 = percentileOfDismissedScale(runs, 0.99)
	t.Logf("c-tree-read-bound: %s, %s: p50 %.1f ms, p90 %.1f ms, p99 %.1f ms over %d readings after %d warm-ups, %d bytes; %s; show jit = %s",
		s.name, what, msOf(reading.p50), msOf(reading.p90), msOf(reading.p99), skeletonReadings, skeletonWarmUps, reading.bytes,
		reading.database.line(reading.idle), statements.jit(t))
	return reading
}

// skeletonOldReads is the old issue page's reads of issue (R8), by name: the nine of its first
// load, and the reads of its three tabs - the Verification tab's results read and, for the
// Verification document and the plan, each document's read, its text, its blocks and its comments.
// The Decisions tab draws from the decisions read the first load makes.
func skeletonOldReads(issue *skeletonIssue) map[string]string {
	spec := url.QueryEscape("dispatch://" + issue.key + "/spec")
	reads := map[string]string{
		"comments":       "/api/v1/issues/" + issue.key + "/comments",
		"asks":           "/api/v1/issues/" + issue.key + "/asks?state=all",
		"spec":           "/api/v1/artifacts/" + issue.spec,
		"spec text":      "/api/v1/artifacts/" + issue.spec + "/text",
		"spec blocks":    "/api/v1/artifacts/" + issue.spec + "/blocks",
		"decisions":      "/api/v1/issues/" + issue.key + "/decisions",
		"coverage":       "/api/v1/issues/" + issue.key + "/coverage",
		"spec part_of":   "/api/v1/references?kind=part_of&to=" + spec,
		"spec questions": "/api/v1/references/questions?kind=artifact&id=" + issue.spec,
		"results":        "/api/v1/issues/" + issue.key + "/results",
	}
	for name, document := range map[string]string{"verification": issue.verification, "plan": issue.plan} {
		reads[name] = "/api/v1/artifacts/" + document
		reads[name+" text"] = "/api/v1/artifacts/" + document + "/text"
		reads[name+" blocks"] = "/api/v1/artifacts/" + document + "/blocks"
		reads[name+" comments"] = "/api/v1/issues/" + issue.key + "/comments?artifact=" + document
	}
	return reads
}

// skeletonFilter is one read of the tree route c-tree-read-bound reads on each issue: the stable
// name its lines print, its query, and the old page's reads (skeletonOldReads) it is held to, nil
// for all of them, the old page's first load and its three tabs together.
type skeletonFilter struct {
	name, query string
	old         []string
}

// skeletonFilters are the reads of the tree route each issue is read with (R8, R30): the read with
// no view and the read with the conversation's kinds, each held to the old page's first load and its
// three tabs together; the spec view to the spec's artifact, text and blocks reads; the Verification
// view to that document's artifact, text, blocks and comments reads and the issue's results read;
// the plan view to the plan's four reads; and the decisions view to the issue's Decisions read, which
// it replaces once the root removes that route.
var skeletonFilters = []skeletonFilter{
	{name: "no-view"},
	{name: "kind", query: kindFilterQuery(kindFilterConversation...)},
	{name: "spec", query: "?view=" + graphstore.QuerySpec, old: []string{"spec", "spec text", "spec blocks"}},
	{name: "verification", query: "?view=" + graphstore.QueryVerification,
		old: []string{"verification", "verification text", "verification blocks", "verification comments", "results"}},
	{name: "plan", query: "?view=" + graphstore.QueryPlan, old: []string{"plan", "plan text", "plan blocks", "plan comments"}},
	{name: "decisions", query: "?view=" + graphstore.QueryDecisions, old: []string{"decisions"}},
}

// oldMS is the old page's database time per reading of the reads filter is held to, the sum of
// each read's own (old, by name): every read of the page where filter names none.
func (filter skeletonFilter) oldMS(old map[string]float64) float64 {
	names := filter.old
	if names == nil {
		names = slices.Sorted(maps.Keys(old))
	}
	sum := 0.0
	for _, name := range names {
		sum += old[name]
	}
	return sum
}

// measureSkeletonOld reads each of the old page's reads of each issue on s, the build of
// BASELINE_SHA, each line naming the issue, the read and its request, and answers each read's
// database time per reading, by issue and by name.
func measureSkeletonOld(t *testing.T, statements *skeletonStatements, s *readCostServer, issues []*skeletonIssue) map[string]map[string]float64 {
	t.Helper()
	old := map[string]map[string]float64{}
	for _, issue := range issues {
		reads := skeletonOldReads(issue)
		old[issue.key] = map[string]float64{}
		for _, name := range slices.Sorted(maps.Keys(reads)) {
			what := fmt.Sprintf("%s (%s) old page, %s, GET %s", issue.key, issue.label, name, reads[name])
			old[issue.key][name] = measureSkeletonRead(t, statements, s, what, reads[name]).database.ms
		}
	}
	return old
}

// ───── the pages this head answers ─────────────────────────────────────────

// skeletonFetch answers a GET of path as the reader, with display trees: its status and its body.
type skeletonFetch func(path string) (int, []byte, error)

// skeletonServerFetch is a GET on the booted build s as the reader.
func skeletonServerFetch(s *readCostServer) skeletonFetch {
	return func(path string) (int, []byte, error) {
		answer, err := s.send(http.MethodGet, path, readCostReader, nil)
		return answer.status, answer.body, err
	}
}

// skeletonHandlerFetch is a GET on a test server's handler as login, with display trees.
func skeletonHandlerFetch(handler http.Handler, login string) skeletonFetch {
	return func(path string) (int, []byte, error) {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Dispatch-User", login)
		request.Header.Set(displayTreesHeader, "1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code, response.Body.Bytes(), nil
	}
}

// skeletonPage is one page of a tree read: its answer, how many bytes of JSON it took, and the
// cursor its request named, "" for the first page.
type skeletonPage struct {
	model.IssueTreeRead
	bytes  int
	cursor string
}

// skeletonPageLimit is the most pages one read is followed through.
const skeletonPageLimit = 1_000

// walkSkeletonPages reads path, a GET of the tree route, and each page after it that the cursor of
// the page before names, under the same filter and the same cut, until a page answers no cursor,
// and holds the pages to R8, recording each failure with label (the issue and the filter), the
// cursor and the bytes of the page, and going on: each page holds at most limit nodes and at most
// issueTreePageBytes bytes of uncompressed JSON, tuples and context included; a page with a cursor
// answers remaining, positive and equal to the nodes every page after it holds, and the last page
// none; each page answers the first page's comments_before, which no cursor moves; each page past
// the first starts at the node its cursor names; no node stands on two pages; and no node but a
// context node stands before its parent, where the read holds that parent.
func walkSkeletonPages(t *testing.T, label string, fetch skeletonFetch, path string, limit int) []skeletonPage {
	t.Helper()
	var pages []skeletonPage
	request, cursor := path, ""
	for len(pages) < skeletonPageLimit {
		status, body, err := fetch(request)
		if err != nil {
			t.Errorf("%s, cursor %q: GET %s: %v", label, cursor, request, err)
			return pages
		}
		if status != http.StatusOK {
			t.Errorf("%s, cursor %q: GET %s answered %d: %.2000s", label, cursor, request, status, body)
			return pages
		}
		page := skeletonPage{bytes: len(bytes.TrimSuffix(body, []byte("\n"))), cursor: cursor}
		if err := json.Unmarshal(body, &page.IssueTreeRead); err != nil {
			t.Errorf("%s, cursor %q: decode GET %s: %v", label, cursor, request, err)
			return pages
		}
		if len(page.Nodes) > limit || page.bytes > issueTreePageBytes {
			t.Errorf("%s, cursor %q: the page holds %d nodes in %d bytes, want at most %d nodes and %d bytes", label, cursor, len(page.Nodes), page.bytes, limit, issueTreePageBytes)
		}
		switch {
		case page.Cursor != nil && page.Remaining == nil:
			t.Errorf("%s, cursor %q, %d bytes: cursor present, remaining absent", label, cursor, page.bytes)
		case page.Cursor != nil && *page.Remaining <= 0:
			t.Errorf("%s, cursor %q, %d bytes: remaining %d beside a cursor", label, cursor, page.bytes, *page.Remaining)
		case page.Cursor == nil && page.Remaining != nil:
			t.Errorf("%s, cursor %q, %d bytes: remaining %d on a page with no cursor", label, cursor, page.bytes, *page.Remaining)
		}
		if len(pages) > 0 && !bytes.Equal(page.CommentsBefore, pages[0].CommentsBefore) {
			t.Errorf("%s, cursor %q, %d bytes: the page answers comments_before %s, the first page %s", label, cursor, page.bytes, page.CommentsBefore, pages[0].CommentsBefore)
		}
		if cursor != "" {
			named, _ := base64.RawURLEncoding.DecodeString(cursor)
			if len(page.Nodes) == 0 || page.Nodes[0].Kind+":"+page.Nodes[0].ID != string(named) {
				t.Errorf("%s, cursor %q, %d bytes: the page does not start at %q, the node its cursor names", label, cursor, page.bytes, named)
			}
		}
		pages = append(pages, page)
		if page.Cursor == nil {
			break
		}
		cursor = *page.Cursor
		separator := "?"
		if strings.Contains(path, "?") {
			separator = "&"
		}
		request = path + separator + "cursor=" + url.QueryEscape(cursor)
	}
	if len(pages) == 0 {
		return pages
	}
	if pages[len(pages)-1].Cursor != nil {
		t.Errorf("%s still answers a cursor after %d pages", label, skeletonPageLimit)
	}
	after := 0
	for index := len(pages) - 1; index >= 0; index-- {
		if page := pages[index]; page.Remaining != nil && *page.Remaining != after {
			t.Errorf("%s, cursor %q, %d bytes: remaining %d, and the pages after it hold %d nodes", label, page.cursor, page.bytes, *page.Remaining, after)
		}
		after += len(pages[index].Nodes)
	}
	at := map[string]int{}
	position := 0
	for _, page := range pages {
		for _, node := range page.Nodes {
			address := node.Kind + ":" + node.ID
			if _, held := at[address]; held {
				t.Errorf("%s, cursor %q, %d bytes: the read answers %s a second time", label, page.cursor, page.bytes, address)
			} else {
				at[address] = position
			}
			position++
		}
	}
	position = 0
	for _, page := range pages {
		for _, node := range page.Nodes {
			if node.Parent != nil && node.Role != treeRoleContext {
				if parent, held := at[node.Parent.Kind+":"+node.Parent.ID]; held && parent > position {
					t.Errorf("%s, cursor %q, %d bytes: the read answers %s:%s before its parent %s:%s", label, page.cursor, page.bytes, node.Kind, node.ID, node.Parent.Kind, node.Parent.ID)
				}
			}
			position++
		}
	}
	return pages
}

// skeletonNodes is every node pages answer, in their order.
func skeletonNodes(pages []skeletonPage) []model.IssueTreeNode {
	var nodes []model.IssueTreeNode
	for _, page := range pages {
		nodes = append(nodes, page.Nodes...)
	}
	return nodes
}

// skeletonCount is how many of nodes hold supertag, or, where supertag is "", are of kind.
func skeletonCount(nodes []model.IssueTreeNode, kind, supertag string) int {
	count := 0
	for _, node := range nodes {
		if supertag != "" && node.Supertag == supertag || supertag == "" && node.Kind == kind {
			count++
		}
	}
	return count
}

// ───── the shape this head reads ───────────────────────────────────────────

// holdSkeletonCount records, and prints, a count of a read that differs from what the corpus wrote,
// and goes on.
func holdSkeletonCount(t *testing.T, key, what string, read, want int) {
	t.Helper()
	if read != want {
		t.Errorf("%s answers %d %s, not %d", key, read, what, want)
	}
}

// requireSkeletonShape holds each corpus to its shape through this head's reads of its tree, each
// read's pages followed through its cursor (walks, by issue and filter), before anything is
// measured. The read with no view answers the typed blocks of the role documents, the comments of
// the comment page, the asks and a comments_before naming the oldest comment of the page where
// older ones remain; on the issue of the most blocks, no block of its other documents, in that read
// or in the read with the conversation's kinds, and document_nodes listing each document of kind doc
// the issue read lists with the count of blocks the blocks read lists for it. Each view answers its
// own kinds: the spec view the requirements, the Verification view the checks, the plan view the
// plan tasks, and the decisions view the decisions and the asks.
func requireSkeletonShape(t *testing.T, s *readCostServer, issues []*skeletonIssue, walks map[string]map[string][]skeletonPage) {
	t.Helper()
	for index, issue := range issues {
		pages := walks[issue.key]
		whole := skeletonNodes(pages["no-view"])
		holdSkeletonCount(t, issue.key, "requirement", skeletonCount(whole, "", "requirement"), skeletonHeld(issue.requirements))
		holdSkeletonCount(t, issue.key, graphstore.BlockTypeCheck, skeletonCount(whole, "", graphstore.BlockTypeCheck), skeletonHeld(issue.checks))
		holdSkeletonCount(t, issue.key, "plan_task", skeletonCount(whole, "", "plan_task"), skeletonHeld(issue.planTasks))
		holdSkeletonCount(t, issue.key, refs.DecisionSupertag, skeletonCount(whole, "", refs.DecisionSupertag), issue.decisions)
		holdSkeletonCount(t, issue.key, "comments", skeletonCount(whole, graphstore.KindComment, ""), min(issue.comments, skeletonCommentPage))
		holdSkeletonCount(t, issue.key, "asks", skeletonCount(whole, graphstore.KindAsk, ""), issue.heldAsks())
		for _, view := range []struct {
			filter, what, kind, supertag string
			want                         int
		}{
			{"spec", "requirement in its spec view", "", "requirement", skeletonHeld(issue.requirements)},
			{"verification", "check in its Verification view", "", graphstore.BlockTypeCheck, skeletonHeld(issue.checks)},
			{"plan", "plan_task in its plan view", "", "plan_task", skeletonHeld(issue.planTasks)},
			{"decisions", "decision in its decisions view", "", refs.DecisionSupertag, issue.decisions},
			{"decisions", "asks in its decisions view", graphstore.KindAsk, "", issue.heldAsks()},
		} {
			holdSkeletonCount(t, issue.key, view.what, skeletonCount(skeletonNodes(pages[view.filter]), view.kind, view.supertag), view.want)
		}
		if len(pages["no-view"]) == 0 {
			continue
		}
		first := pages["no-view"][0]
		var position *string
		if err := json.Unmarshal(first.CommentsBefore, &position); err != nil {
			t.Errorf("%s answers comments_before %q, want a position or null: %v", issue.key, first.CommentsBefore, err)
		}
		oldest := ""
		for _, node := range whole {
			if node.Kind != graphstore.KindComment || node.MadeAt == nil {
				continue
			}
			written := (refs.CommentPosition{MadeAt: *node.MadeAt, ID: node.ID}).String()
			if oldest == "" || written < oldest {
				oldest = written
			}
		}
		switch {
		case issue.comments <= skeletonCommentPage && position != nil:
			t.Errorf("%s answers comments_before %q, want null: it holds %d comments", issue.key, *position, issue.comments)
		case issue.comments > skeletonCommentPage && (position == nil || *position != oldest):
			t.Errorf("%s answers comments_before %v, want %q, the oldest comment of its page", issue.key, position, oldest)
		}
		if index != 2 {
			continue
		}
		var detail struct {
			Artifacts []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"artifacts"`
		}
		s.read(t, "read "+issue.key, "/api/v1/issues/"+issue.key, &detail)
		var documents []string
		for _, artifact := range detail.Artifacts {
			if artifact.Kind == "doc" {
				documents = append(documents, artifact.ID)
			}
		}
		listed := make([]string, 0, len(first.DocumentNodes))
		largest := 0
		for _, document := range first.DocumentNodes {
			listed = append(listed, document.ArtifactID)
			var blocks []json.RawMessage
			s.read(t, "read the blocks of "+document.Slug, "/api/v1/artifacts/"+document.ArtifactID+"/blocks", &blocks)
			if document.BlockCount != len(blocks) {
				t.Errorf("%s answers %d blocks of %s in document_nodes, not %d", issue.key, document.BlockCount, document.Slug, len(blocks))
			}
			largest = max(largest, document.BlockCount)
		}
		if !slices.Equal(listed, documents) {
			t.Errorf("%s answers document_nodes %v, not the documents of kind doc the issue read lists, %v", issue.key, listed, documents)
		}
		holdSkeletonCount(t, issue.key, "blocks in its largest other document", largest, slices.Max(issue.others))
		for _, document := range issue.documents {
			var artifact struct {
				RefKey string `json:"ref_key"`
			}
			s.read(t, "read the document "+document, "/api/v1/artifacts/"+document, &artifact)
			for _, filter := range []string{"no-view", "kind"} {
				for _, node := range skeletonNodes(pages[filter]) {
					if strings.HasPrefix(node.ID, artifact.RefKey+"#") {
						t.Errorf("%s answers, filter %s, the block %s of its other document %s", issue.key, filter, node.ID, artifact.RefKey)
						break
					}
				}
			}
		}
	}
}

// ───── the comments on the largest spec, and the plan of 12,050 tasks ───────

// skeletonSpecComments is how many comments c-tree-read-bound writes on the spec's blocks of the
// issue of the largest spec, as many as the issue of the most comments holds.
const skeletonSpecComments = 9_252

// requireCommentedSpec writes, through this head's route, skeletonSpecComments comments on the
// spec's blocks of issue, each a person's, anchored in the paragraph of one requirement by its
// block (AnchorInput's block_id), and reads the spec view once the block index and the window arm
// owe nothing: its p90 is at most skeletonBoundMS, and across its node pages it answers the 200
// newest of those comments, with a comments_before naming the oldest of them.
func requireCommentedSpec(t *testing.T, statements *skeletonStatements, s *readCostServer, conn *pgx.Conn, issue *skeletonIssue) {
	t.Helper()
	started := time.Now()
	skeletonInParallel(t, "comment on the spec's blocks of "+issue.key, skeletonSpecComments, func(index int) error {
		block := fmt.Sprintf("r%d", index%issue.requirements+1)
		_, err := s.write(http.MethodPost, "/api/v1/issues/"+issue.key+"/comments", readCostWriter, map[string]any{
			"body":   fmt.Sprintf("Comment %d on %s: a note on its words.", index+1, block),
			"anchor": map[string]any{"artifact": issue.spec, "block_id": block, "quote": fmt.Sprintf("The requirement %s holds one paragraph.", block)},
		}, http.StatusCreated)
		return err
	})
	t.Logf("c-tree-read-bound: %d comments on the spec's blocks of %s took %s", skeletonSpecComments, issue.key, time.Since(started).Round(time.Second))
	side := "comments on the spec of " + issue.key
	waitSkeletonOwed(t, conn, side, nil)
	statements.waitWindowsOwed(t, side)
	skeletonAnalyze(t, conn, side)
	path := "/api/v1/issues/" + issue.key + "/tree?view=" + graphstore.QuerySpec
	label := fmt.Sprintf("%s (%s), the spec view beside %d comments on its blocks", issue.key, issue.label, skeletonSpecComments)
	if p90 := msOf(measureSkeletonRead(t, statements, s, label, path).p90); p90 > skeletonBoundMS {
		t.Errorf("%s has p90 %.1f ms, above %.0f", label, p90, skeletonBoundMS)
	}
	pages := walkSkeletonPages(t, label, skeletonServerFetch(s), path, issueTreePageSize)
	type listedComment struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
	}
	var listed []listedComment
	s.read(t, "list the comments of "+issue.key, "/api/v1/issues/"+issue.key+"/comments", &listed)
	slices.SortFunc(listed, func(left, right listedComment) int {
		return cmp.Or(right.CreatedAt.Compare(left.CreatedAt), strings.Compare(right.ID, left.ID))
	})
	if len(listed) < skeletonCommentPage {
		t.Errorf("%s lists %d comments, not the %d written on its spec's blocks", issue.key, len(listed), skeletonSpecComments)
		return
	}
	newest := map[string]bool{}
	for _, comment := range listed[:skeletonCommentPage] {
		newest[comment.ID] = true
	}
	held := 0
	for _, node := range skeletonNodes(pages) {
		if node.Kind != graphstore.KindComment {
			continue
		}
		held++
		if !newest[node.ID] {
			t.Errorf("%s answers the comment %s, which is none of the %d newest on the spec's blocks", label, node.ID, skeletonCommentPage)
		}
	}
	holdSkeletonCount(t, issue.key, "comments in its spec view beside the comments on its blocks", held, skeletonCommentPage)
	oldest := listed[skeletonCommentPage-1]
	want := strconv.Quote((refs.CommentPosition{MadeAt: oldest.CreatedAt, ID: oldest.ID}).String())
	if len(pages) > 0 && string(pages[0].CommentsBefore) != want {
		t.Errorf("%s answers comments_before %s, want %s, the oldest of the %d newest comments", label, pages[0].CommentsBefore, want, skeletonCommentPage)
	}
}

// skeletonPagedTasks is how many plan tasks the plan of the issue of the most blocks holds for the
// paging case of c-tree-read-bound.
const skeletonPagedTasks = 12_050

// requirePagedPlan writes, through this head's upload, a plan of skeletonPagedTasks tasks over the
// plan of issue, the issue of the most blocks, and follows the plan-task filter's cursor to null once
// the block index and the window arm owe nothing: each page stays below both bounds, the first ends
// at the bound of bytes before the node limit, and every task stands exactly once, in walk order.
func requirePagedPlan(t *testing.T, statements *skeletonStatements, s *readCostServer, conn *pgx.Conn, issue *skeletonIssue) {
	t.Helper()
	var text struct {
		Token string `json:"token"`
	}
	s.read(t, "read the plan of "+issue.key, "/api/v1/artifacts/"+issue.plan+"/text", &text)
	s.expect(t, fmt.Sprintf("upload a plan of %d tasks to %s", skeletonPagedTasks, issue.key), http.MethodPost, "/api/v1/issues/"+issue.key+"/artifacts", readCostWriter,
		map[string]any{
			"name": "plan.md", "content": skeletonTyped("plan_task", "p", skeletonPagedTasks, func(string) string { return "" }),
			"precondition": map[string]string{"document": text.Token},
		}, http.StatusCreated)
	side := "the plan of " + issue.key
	waitSkeletonOwed(t, conn, side, nil)
	statements.waitWindowsOwed(t, side)
	label := fmt.Sprintf("%s (%s), filter kind=plan_task over %d plan tasks", issue.key, issue.label, skeletonPagedTasks)
	pages := walkSkeletonPages(t, label, skeletonServerFetch(s), "/api/v1/issues/"+issue.key+"/tree"+kindFilterQuery("plan_task"), issueTreePageSize)
	if len(pages) < 2 || len(pages[0].Nodes) >= issueTreePageSize {
		nodes, size := 0, 0
		if len(pages) > 0 {
			nodes, size = len(pages[0].Nodes), pages[0].bytes
		}
		t.Errorf("%s answers %d pages, the first of %d nodes in %d bytes, want it ended at %d bytes before %d nodes", label, len(pages), nodes, size, issueTreePageBytes, issueTreePageSize)
	}
	var tasks []string
	for _, node := range skeletonNodes(pages) {
		if node.Supertag == "plan_task" {
			tasks = append(tasks, node.BlockID)
		}
	}
	holdSkeletonCount(t, issue.key, "plan_task over the pages of its plan-task filter", len(tasks), skeletonPagedTasks)
	for index, task := range tasks[:min(len(tasks), skeletonPagedTasks)] {
		if want := fmt.Sprintf("p%d", index+1); task != want {
			t.Errorf("%s answers the plan task %s at %d, want %s: every task once, in walk order", label, task, index, want)
			break
		}
	}
}

// ───── a smaller node limit ────────────────────────────────────────────────

// skeletonSmallPage is the node limit of the test server c-tree-read-bound's two cases of a smaller
// node limit read on.
const skeletonSmallPage = 2

// requireSmallPages is c-tree-read-bound's two cases of a smaller node limit, on a test server that
// newTestServer starts with a node limit of skeletonSmallPage (testServerOptions.issueTreePageSize),
// and not on the booted builds. Alice writes an author-only pre-mortem lens on r1 of an issue of four
// requirements. Each read of skeletonFilters, as alice, is followed
// through its cursor and held to R8 (walkSkeletonPages), and the read with no view ends a page at the
// node limit, far below the bound of bytes. Bob's read with no view, followed through its cursor,
// answers on each page with a cursor remaining the nodes he may see after it, alice's lens never
// among them, and alice's read of the same issue counts her lens.
func requireSmallPages(t *testing.T) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{issueTreePageSize: skeletonSmallPage})
	createReferenceAPIProject(t, handler, "SMALL")
	var spec strings.Builder
	for index := range 4 {
		spec.WriteString(requirementMarkdown(fmt.Sprintf("r%d", index+1), fmt.Sprintf("The small board names its row %d.", index+1)) + "\n")
	}
	issue := createDigestIssue(t, handler, database, "SMALL", "A small issue read two nodes a page", spec.String(), false)
	rig := reasoningRig{t: t, handler: handler, database: database, issue: issue.Key}
	lens := rig.invoke(refs.BlockRef("dispatch://"+issue.Key+"/spec", "r1"), graphstore.LensKindPreMortem, "alice",
		map[string]any{"visibility": graphstore.VisibilityAuthor})
	path := "/api/v1/issues/" + issue.Key + "/tree"
	label := func(filter, login string) string {
		return fmt.Sprintf("%s at a node limit of %d, filter %s, as %s", issue.Key, skeletonSmallPage, filter, login)
	}
	read := map[string][]skeletonPage{}
	for _, filter := range skeletonFilters {
		pages := walkSkeletonPages(t, label(filter.name, "alice"), skeletonHandlerFetch(handler, "alice"), path+filter.query, skeletonSmallPage)
		if filter.query == "" {
			read["alice"] = pages
		}
	}
	read["bob"] = walkSkeletonPages(t, label("no-view", "bob"), skeletonHandlerFetch(handler, "bob"), path, skeletonSmallPage)
	nodeEnded := false
	for _, page := range read["alice"] {
		if page.Cursor != nil && len(page.Nodes) == skeletonSmallPage && page.bytes < issueTreePageBytes {
			nodeEnded = true
		}
	}
	if !nodeEnded {
		t.Errorf("%s answers no page ended at the node limit with a cursor: %d pages", label("no-view", "alice"), len(read["alice"]))
	}
	holds := func(pages []skeletonPage) bool {
		return slices.ContainsFunc(skeletonNodes(pages), func(node model.IssueTreeNode) bool {
			return node.Kind == graphstore.KindLens && node.ID == lens.ID
		})
	}
	if holds(read["bob"]) {
		t.Errorf("%s answers alice's author-only lens %s", label("no-view", "bob"), lens.ID)
	}
	if !holds(read["alice"]) {
		t.Errorf("%s answers no lens %s of hers", label("no-view", "alice"), lens.ID)
	}
	if alice, bob := len(skeletonNodes(read["alice"])), len(skeletonNodes(read["bob"])); alice <= bob {
		t.Errorf("alice's read of %s holds %d nodes and bob's %d, want alice's to count her lens too", issue.Key, alice, bob)
	}
}

// ───── the test ────────────────────────────────────────────────────────────

// TestTheTreeReadAnswersTheSkeletonWithinItsBounds is c-tree-read-bound. It first reads its two
// cases of a smaller node limit on a test server of its own (requireSmallPages). Then it boots the
// build of BASELINE_SHA over a database of its own, seeds the four issues of skeletonShapes, with
// the view content each holds, through that build's routes, waits until no document owes its block
// index and the block index's window arm owes no document and no item its windows, and reads on
// each issue each of the old page's reads. Then it stops that build, boots this checkout's over the
// same database, waits until the boot's backfill is done, each issue holds its mark, no document
// owes its block index and the window arm owes nothing, follows each read of skeletonFilters on each
// issue through its cursor (walkSkeletonPages), holds each corpus to its shape, and reads each read
// of skeletonFilters on each issue. Each read is read skeletonReadings times after skeletonWarmUps
// warm-ups, after an analyze, and its line prints its p50, p90 and p99, its bytes, its database
// time and statements per reading, the idle interval after it, and the window arm's backlog before
// it and after that interval. Last, on this head, it writes the comments on the largest spec's
// blocks (requireCommentedSpec) and the plan of 12,050 tasks (requirePagedPlan).
//
// It prints, for each issue and each read of skeletonFilters, `c-tree-read-bound: database time per
// reading: fixture=<label> read=<filter> old=<ms> new=<ms>`, which the median stage judges over three
// Jobs (tree-read-db-medians.sh). It fails where a read's p90 is above skeletonBoundMS, and where a
// statement of a loader the read with no view sends runs more than once per reading of it or is not
// in pg_stat_statements, and it records each count, page and bound it reads otherwise and goes on.
func TestTheTreeReadAnswersTheSkeletonWithinItsBounds(t *testing.T) {
	began := time.Now()
	t.Cleanup(func() {
		t.Logf("c-tree-read-bound: TestTheTreeReadAnswersTheSkeletonWithinItsBounds took %s of wall time", time.Since(began).Round(time.Second))
	})
	baseCommit := skeletonBaseCommit(t)
	requireSmallPages(t)
	databaseURL := skeletonDatabase(t)
	headCommit := gitOutput(t, ".", "rev-parse", "HEAD")
	if headCommit == baseCommit {
		t.Fatalf("this head's commit %s is %s=%s, so the two builds cannot be told apart", headCommit, testbaseline.Variable, baseCommit)
	}
	baseBinary := skeletonBaseBuild(t, baseCommit)
	module, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("find the module root: %v", err)
	}
	headBinary := cmdtest.BuildCommand(t, cmdtest.Command{
		Name: "envoy-dispatch-head", Dir: module, Target: "./cmd/dispatch",
		Flags: []string{"-ldflags", "-X main.buildCommit=" + headCommit},
	})
	decides := skeletonBaseDecides(t, baseCommit)
	t.Logf("c-tree-read-bound: the old side runs on the build of %s, the commit %s names, which declares the typed block decision: %t; this head's side "+
		"on the build of this checkout's working copy, stamped with its HEAD %s", baseCommit, testbaseline.Variable, decides, headCommit)
	client := readCostClient()
	listener := skeletonListener(t)
	conn := skeletonConnect(t, databaseURL)
	statements := newSkeletonStatements(t, databaseURL, conn)

	// ───── first: the corpus and the old page, on the release before ─────
	base := startSkeletonServer(t, client, "the release before ("+baseCommit+")", baseBinary, baseCommit, databaseURL, listener)
	issues := seedSkeleton(t, base, decides)
	waitSkeletonOwed(t, conn, "release before's corpus", nil)
	statements.waitWindowsOwed(t, "release before")
	skeletonAnalyze(t, conn, "old side")
	old := measureSkeletonOld(t, statements, base, issues)
	stopSkeletonServer(t, base)

	// ───── second: this head over the same database ─────
	head := startSkeletonServer(t, client, "this head ("+headCommit+")", headBinary, headCommit, databaseURL, listener)
	keys := make([]string, len(issues))
	for index, issue := range issues {
		keys[index] = issue.key
	}
	waitSkeletonOwed(t, conn, "head's boot", func() (bool, string) {
		if !strings.Contains(head.process.Output.String(), skeletonBackfillDone) {
			return false, "the boot backfill is not done"
		}
		var marked int
		if err := conn.QueryRow(context.Background(), `select count(*) from issue_head_marks where issue_key = any($1)`, keys).Scan(&marked); err != nil {
			return false, err.Error()
		}
		return marked == len(keys), fmt.Sprintf("%d of the %d issues hold their mark", marked, len(keys))
	})
	statements.head = true
	statements.waitWindowsOwed(t, "head")
	skeletonAnalyze(t, conn, "new side")
	fetch := skeletonServerFetch(head)
	walks := map[string]map[string][]skeletonPage{}
	for _, issue := range issues {
		walks[issue.key] = map[string][]skeletonPage{}
		for _, filter := range skeletonFilters {
			label := fmt.Sprintf("%s (%s), filter %s", issue.key, issue.label, filter.name)
			walks[issue.key][filter.name] = walkSkeletonPages(t, label, fetch, "/api/v1/issues/"+issue.key+"/tree"+filter.query, issueTreePageSize)
		}
	}
	requireSkeletonShape(t, head, issues, walks)
	loaders := skeletonLoaders(t, issues[0].key)

	// ───── third: this head's reads, the two cases written on this head, and the bounds ─────
	type headRead struct {
		issue   *skeletonIssue
		filter  skeletonFilter
		reading skeletonReading
	}
	var reads []headRead
	for _, issue := range issues {
		for _, filter := range skeletonFilters {
			what := fmt.Sprintf("%s (%s), the tree read, filter %s", issue.key, issue.label, filter.name)
			reads = append(reads, headRead{issue: issue, filter: filter,
				reading: measureSkeletonRead(t, statements, head, what, "/api/v1/issues/"+issue.key+"/tree"+filter.query)})
		}
	}
	requireCommentedSpec(t, statements, head, conn, issues[3])
	requirePagedPlan(t, statements, head, conn, issues[2])
	stopSkeletonServer(t, head)

	for _, read := range reads {
		key, label, filter := read.issue.key, read.issue.label, read.filter.name
		t.Logf("c-tree-read-bound: database time per reading: fixture=%s read=%s old=%.3f new=%.3f", label, filter,
			read.filter.oldMS(old[key]), read.reading.database.ms)
		if p90 := msOf(read.reading.p90); p90 > skeletonBoundMS {
			t.Errorf("%s (%s), filter %s, has p90 %.1f ms, above %.0f", key, label, filter, p90, skeletonBoundMS)
		}
		if read.filter.query != "" {
			continue
		}
		for _, loader := range slices.Sorted(maps.Keys(loaders)) {
			calls := read.reading.database.byText[loaders[loader]]
			t.Logf("c-tree-read-bound: %s, the no-view tree read runs the statement of %s %.2f times per read", key, loader, calls)
			if calls > 1 {
				t.Errorf("the no-view tree read of %s runs the statement of %s %.2f times per read", key, loader, calls)
			}
		}
	}
	final, err := statements.snapshot(context.Background())
	if err != nil {
		t.Fatalf("c-tree-read-bound: database time was not measured: %v", err)
	}
	held := map[string]bool{}
	for _, total := range final.statements {
		held[skeletonNormalized(total.query)] = true
	}
	for _, loader := range slices.Sorted(maps.Keys(loaders)) {
		if !held[loaders[loader]] {
			t.Errorf("the statement of %s is not in pg_stat_statements", loader)
		}
	}
}
