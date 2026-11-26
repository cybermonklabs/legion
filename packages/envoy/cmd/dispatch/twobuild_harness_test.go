//go:build twobuild

// Two builds of Dispatch serving one Postgres and one NATS at once, which is what a rolling release
// runs: the build that serves now (the previous build) and the build that rolls in beside it (this
// tree). Each row starts one envoy-dispatch of each build, on one database at this tree's schema
// that the builds migrated in production's order (laneDatabase: the previous build boots on it,
// which is the SQL half of the rule in cmd/dispatch/README.md) and one NATS server, with one
// signing key, and drives the surface the other build's clients use: the CLI
// (twobuild_cli_test.go), the stored documents and the document
// socket (twobuild_documents_test.go), the messages and subscription settings on the bus
// (twobuild_messages_test.go), what the page admission does while the previous build's process
// is live and once it has left the registry (twobuild_transition_test.go), the migrations
// (twobuild_migrations_test.go), the owner lock (twobuild_owner_lock_test.go), the command
// bar's runs beside the release before them (twobuild_going_runs_test.go), what the previous
// build does with a read-only personal token this tree mints (twobuild_read_token_test.go), and
// the routes a personal token holding p0 keeps on each build (twobuild_p0_token_test.go). This
// file is the harness every twobuild_*_test.go starts its builds with.
//
// The previous build is a directory packages/envoy/scripts/build-dispatch-build.sh writes, named by
// DISPATCH_PREVIOUS_BUILD. The suite skips only when that variable is unset; a directory it names
// that lacks a part fails the suite, naming the parts it lacks. One row of the transition file
// runs another earlier build instead, the first-rollout build named by
// DISPATCH_FIRST_ROLLOUT_BUILD, on the same terms, and fails when that build is of another commit
// than firstRolloutRelease; the row of twobuild_going_runs_test.go runs the release
// goingReleaseCommit, named by DISPATCH_GOING_RELEASE_BUILD, on the same terms too. The current
// side is this tree: its server from cmdtest.Build, its CLI run as
// `bun packages/envoy-client/bin/dispatch.ts`, its corpus read from the working tree.
//
// Its own build tag and its own CI job, dispatch-two-builds in .github/workflows/ci.yaml, for the
// reason the multiprocess suite has its own: a row waits for durable hand-overs and settings
// polls, tens of seconds each. That job builds the previous release, the first-rollout release
// and the going-runs release with build-dispatch-build.sh, caches each build by its commit, and
// runs this suite against them.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// ───── the previous build ───────────────────────────────────────────────────

// previousBuildVariable names the directory build-dispatch-build.sh wrote for the previous build.
const previousBuildVariable = "DISPATCH_PREVIOUS_BUILD"

// firstRolloutBuildVariable names the directory build-dispatch-build.sh wrote for the first-rollout
// build, firstRolloutRelease, which TestThisBuildsPageTurnsWritableOnceThePreviousBuildsProcessLeavesTheFleet
// runs as its earlier build.
const firstRolloutBuildVariable = "DISPATCH_FIRST_ROLLOUT_BUILD"

// firstRolloutRelease is the commit of the first-rollout build, the release tagged
// legion-envoy-v30.20.2. It is the newest release before the fleet columns (migration
// 20261101030537_server_processes_schema), which legion-envoy-v30.21.0 is the first to carry and
// to register its schema numbers in (cybermonklabs/legion#2461): a process of it reads as version
// 0 in the fleet, which is the first rollout of those numbers. The tag is lightweight, and a
// lightweight tag can be moved, so the pin is the commit. Two other places hold the same commit:
// FIRST_ROLLOUT_COMMIT in ci.yaml's dispatch-two-builds job, and DISPATCH_E2E_FIRST_ROLLOUT_COMMIT,
// which that job gives the browser run against the first-rollout build.
const firstRolloutRelease = "edb69e0d7588ec037847182e74a04a343a0664c9"

// previousBuildParts is every part of a previous build's directory a row reads, as
// build-dispatch-build.sh lays it out.
var previousBuildParts = []struct {
	path string
	dir  bool
}{
	{"COMMIT", false},
	{"envoy-dispatch", false},
	{"dispatch.js", false},
	{"web/index.html", false},
	{"pmdoc/testdata/corpus", true},
	{"pmdoc/schema/blocks.json", false},
	{"text/testdata/dispatch-text-references.json", false},
	{"text/testdata/dispatch-href-references.json", false},
}

// previousBuild is the directory of the previous build and the commit it was built from.
type previousBuild struct {
	dir    string
	commit string
}

// openPreviousBuild reads DISPATCH_PREVIOUS_BUILD, and openFirstRolloutBuild
// DISPATCH_FIRST_ROLLOUT_BUILD (openBuild).
func openPreviousBuild(t *testing.T) previousBuild {
	t.Helper()
	return openBuild(t, previousBuildVariable, "--previous-release")
}

// openFirstRolloutBuild also fails the test when the build's COMMIT is another commit than
// firstRolloutRelease, before any server starts.
func openFirstRolloutBuild(t *testing.T) previousBuild {
	t.Helper()
	buildArguments := "--commit " + firstRolloutRelease
	build := openBuild(t, firstRolloutBuildVariable, buildArguments)
	if build.commit != firstRolloutRelease {
		t.Fatalf("%s=%s is a build of commit %s, but the first-rollout build is commit %s (firstRolloutRelease). Build it again with packages/envoy/scripts/build-dispatch-build.sh %s --out <dir>.",
			firstRolloutBuildVariable, build.dir, build.commit, firstRolloutRelease, buildArguments)
	}
	return build
}

// openBuild reads the build the variable names. It skips the test when the variable is unset,
// naming the build-dispatch-build.sh arguments that build it, and fails it when the directory the
// variable names lacks any part a row reads.
func openBuild(t *testing.T, variable, buildArguments string) previousBuild {
	t.Helper()
	dir, set := os.LookupEnv(variable)
	if !set {
		t.Skipf("%s is unset. Build it with packages/envoy/scripts/build-dispatch-build.sh %s --out <dir>, then set %s=<dir>.",
			variable, buildArguments, variable)
	}
	if dir == "" {
		t.Fatalf("%s is set but empty. Set it to the directory build-dispatch-build.sh wrote.", variable)
	}
	var missing []string
	for _, part := range previousBuildParts {
		info, err := os.Stat(filepath.Join(dir, part.path))
		switch {
		case err != nil:
			missing = append(missing, part.path)
		case part.dir && !info.IsDir(), !part.dir && info.IsDir():
			missing = append(missing, part.path+" (wrong kind of file)")
		case part.dir:
			if entries, err := os.ReadDir(filepath.Join(dir, part.path)); err != nil || len(entries) == 0 {
				missing = append(missing, part.path+" (empty)")
			}
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%s=%s is not a complete build. It has no %s. Build it again with packages/envoy/scripts/build-dispatch-build.sh %s.",
			variable, dir, strings.Join(missing, ", "), buildArguments)
	}
	commit, err := os.ReadFile(filepath.Join(dir, "COMMIT"))
	if err != nil {
		t.Fatalf("read the previous build's COMMIT: %v", err)
	}
	return previousBuild{dir: dir, commit: strings.TrimSpace(string(commit))}
}

func (b previousBuild) path(parts ...string) string {
	return filepath.Join(append([]string{b.dir}, parts...)...)
}

// ───── the two builds ───────────────────────────────────────────────────────

// twoBuilds is one process of each build on one database and one NATS server: a is the previous
// build's, b is this tree's.
type twoBuilds struct {
	*dispatchPair
	build previousBuild
	// binary is this tree's envoy-dispatch, built once so a restart runs the same binary.
	binary string
}

// newTwoBuilds opens the previous build, the database and the NATS server, and starts nothing.
// newFirstRolloutBuilds is the same with the first-rollout build as the earlier one.
func newTwoBuilds(t *testing.T) *twoBuilds {
	t.Helper()
	return newTwoBuildsOf(t, openPreviousBuild(t))
}

func newFirstRolloutBuilds(t *testing.T) *twoBuilds {
	t.Helper()
	return newTwoBuildsOf(t, openFirstRolloutBuild(t))
}

// newTwoBuildsOf's pair runs on the lane's database for build (laneDatabaseAfter), and fails the
// test unless build booted on it first (requireEarlierBuildFirst).
func newTwoBuildsOf(t *testing.T, build previousBuild) *twoBuilds {
	t.Helper()
	binary := cmdtest.Build(t, "envoy-dispatch")
	b := &twoBuilds{dispatchPair: newPairOn(t, laneDatabaseAfter(t, build, binary)), build: build, binary: binary}
	requireEarlierBuildFirst(t, b)
	return b
}

// laneDatabaseAfter is the lane's database for build, the earlier side, and current, this tree's
// envoy-dispatch: what each row of build and its owner lock probe run on.
func laneDatabaseAfter(t *testing.T, build previousBuild, current string) *store.Store {
	t.Helper()
	return laneDatabase(t, "two builds after "+build.dir, build.path("envoy-dispatch"), current)
}

// laneDatabase is the database a row's two builds share: a clone of a template made once per key
// in the test binary, in production's order. The earlier build migrates the empty database, as the
// release before this tree migrated production's, and the later one then migrates it forward, as a
// rollout does (migrateInOrder), so the earlier build boots on what production would hold. A
// database the later build migrated from empty, as storetest.Open's is for this tree, records the
// later build's text of every migration as applied, which an earlier build that judges the applied
// text refuses wherever the later one edits a migration it holds: the lane would be red for every
// pull request that edits a shipped migration, its replaces line in place
// (TestTheReleaseBeforeBootsOverTheLaneDatabaseOfAPullRequestThatEditsAShippedMigration, which runs
// this function with such a pair). The arrival mark a boot of a build before the owner lock stamps
// (owner_lock_legacy_mark) is cleared after the boots, as every test database starts without one.
func laneDatabase(t *testing.T, key, earlier, later string) *store.Store {
	t.Helper()
	return storetest.OpenFrom(t, key, func(url string) error {
		migrateInOrder(t, url, earlier, later)
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		_, err = conn.Exec(ctx, "delete from owner_lock_legacy_mark")
		return err
	})
}

// lanePageEntry is the entry chunk of lanePage, the dashboard every process migrateInOrder boots
// serves.
const lanePageEntry = "/assets/index-twobuild-lane.js"

// migrateInOrder boots each of binaries in turn alone on the database url names, with a NATS
// server of its own, and stops it as a deploy does once it serves: the first migrates the empty
// database, as a release before this one migrated production's, and each after it migrates the
// database forward, as each rollout does. Each process deregisters as it stops, so none counts in
// a later process's fleet. Each serves one dashboard, lanePage, and publishes it at boot, as every
// release's process does: the database's row of that page (dashboard_builds) keeps the commit of
// the process that published it first, since a later publish of the same page renews only its last
// start (requireEarlierBuildFirst reads it). The page is no build a row starts, so no row's process
// serves it and no row reads it.
func migrateInOrder(t *testing.T, url string, binaries ...string) {
	t.Helper()
	_, nats := multiprocessNATS(t)
	page := lanePage(t)
	for _, binary := range binaries {
		p := startDispatchProcessWith(t, url, dispatchOptions{NATSURL: nats, SigningKey: multiprocessSigningKey, Binary: binary, WebDist: page})
		p.waitHealthy(t)
		stop(t, p)
	}
}

// lanePage writes the web dist migrateInOrder's processes serve: an index.html that names
// lanePageEntry, and that file.
func lanePage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	entry := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(lanePageEntry, "/")))
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("// the two-build lane's page\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	page := `<!doctype html><script type="module" src="` + lanePageEntry + `"></script>` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// requireEarlierBuildFirst fails the test unless b's earlier build booted first on b's database,
// before this tree did, which is production's order. The database's row of lanePage keeps the
// commit of the first process that published it (migrateInOrder), and of the two only the earlier
// build's binary carries a commit (build-dispatch-build.sh stamps it; cmdtest.Build stamps none).
// A database the earlier build never booted on, as storetest.Open's, holds no such row, and one
// this tree booted first names the empty commit.
func requireEarlierBuildFirst(t *testing.T, b *twoBuilds) {
	t.Helper()
	var commits []string
	if err := b.database.Pool.QueryRow(context.Background(),
		"select coalesce(array_agg(commit order by commit), '{}') from dashboard_builds where entry = $1", lanePageEntry).Scan(&commits); err != nil {
		t.Fatalf("read which build published the lane's page first: %v", err)
	}
	if len(commits) != 1 || commits[0] != b.build.commit {
		t.Fatalf("the database the rows run on was not migrated first by the %s: its rows of the lane's page (%s in dashboard_builds) name the commits %q; want exactly %s, the build that boots first in production's order (laneDatabase)",
			b.previousName(), lanePageEntry, commits, b.build.commit)
	}
}

// options is what both builds' processes start with: the pair's NATS, the one signing key, and env.
func (b *twoBuilds) options(env map[string]string) dispatchOptions {
	return dispatchOptions{NATSURL: b.nats, SigningKey: multiprocessSigningKey, Env: env}
}

// startPrevious starts a process of the previous build, its own web app served, and returns once it
// answers /healthz. It serves on the socket the test holds, as the current build does
// (startDispatchProcessWith): every build this suite runs serves an inherited DISPATCH_LISTEN_FD,
// the first-rollout build, the oldest, among them. A port the test let go of for the process to
// bind would be any program's to take first, and the start would fail on `address already in use`.
func (b *twoBuilds) startPrevious(t *testing.T, env map[string]string) *dispatchProcess {
	t.Helper()
	options := b.options(env)
	options.Binary, options.WebDist = b.build.path("envoy-dispatch"), b.build.path("web")
	b.a = startDispatchProcessWith(t, b.databaseURL(t), options)
	b.a.waitHealthy(t)
	return b.a
}

// startCurrent starts a process of this tree and returns once it answers /healthz.
func (b *twoBuilds) startCurrent(t *testing.T, env map[string]string) *dispatchProcess {
	t.Helper()
	options := b.options(env)
	options.Binary = b.binary
	b.b = startDispatchProcessWith(t, b.databaseURL(t), options)
	b.b.waitHealthy(t)
	return b.b
}

func (b *twoBuilds) previous() *dispatchProcess { return b.a }
func (b *twoBuilds) current() *dispatchProcess  { return b.b }

// buildName is what a failure calls a process: which build it runs.
func (b *twoBuilds) buildName(p *dispatchProcess) string {
	if p == b.a {
		return b.previousName()
	}
	return b.currentName()
}

// previousName and currentName are what a failure calls each build.
func (b *twoBuilds) previousName() string {
	return "previous build (" + shortCommit(b.build.commit) + ")"
}

func (b *twoBuilds) currentName() string { return "current build" }

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// stop stops p as a deploy does, SIGTERM and its graceful shutdown, and waits for it to exit.
func stop(t *testing.T, p *dispatchProcess) {
	t.Helper()
	p.Terminate(t)
	p.WaitExit(t, "SIGTERM")
}

// getJSON reads path from p as the test's login and decodes it into into.
func getJSON(t *testing.T, p *dispatchProcess, path string, into any) []byte {
	t.Helper()
	answer := p.call(t, http.MethodGet, path, "", http.StatusOK)
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("decode GET %s: %v: %s", path, err, answer)
		}
	}
	return answer
}

// jsonBody is value as a request body.
func jsonBody(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode a request body: %v", err)
	}
	return string(body)
}

// ───── the previous build's owner lock protocol ─────────────────────────────

// registeredOwnerLockProtocol waits up to within for the one server process database's registry
// holds (server_processes) and answers the owner lock protocol it registered with
// (owner_lock_protocol: store.OwnerLockProtocol for this tree, 0, the column's default, for a build
// before the owner lock, which writes no such column). process is that process, and name what a
// failure calls it. The read polls, since a build may serve a moment before its registration
// commits. It fails the test when the registry holds more than one process, when process exits
// before its row reads, or when within passes first.
func registeredOwnerLockProtocol(t *testing.T, database *store.Store, process *dispatchProcess, name string, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		rows, err := database.Pool.Query(context.Background(), `select owner_lock_protocol from server_processes`)
		if err != nil {
			t.Fatalf("read the registered server processes: %v", err)
		}
		protocols, err := pgx.CollectRows(rows, pgx.RowTo[int])
		if err != nil {
			t.Fatalf("read the registered server processes: %v", err)
		}
		switch {
		case len(protocols) == 1:
			return protocols[0]
		case len(protocols) > 1:
			t.Fatalf("the registry holds %d server processes, at owner lock protocols %v, where the %s's should be its only one",
				len(protocols), protocols, name)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the %s registered no server process within %s:\n%s", name, within, process.Output.String())
		}
		select {
		case <-process.Exited:
			t.Fatalf("the %s exited before it registered:\n%s", name, process.Output.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// probedOwnerLockProtocols is the owner lock protocol each previous build registered when
// previousBuildOwnerLockProtocol booted it, by the build's directory. It lasts as long as the test
// binary, so the rows that ask boot one probe between them.
var probedOwnerLockProtocols = struct {
	sync.Mutex
	byDir map[string]int
}{byDir: map[string]int{}}

// previousBuildOwnerLockProtocol answers the owner lock protocol build registers, for a row that
// must know it before build boots on the row's own database. A probe there would be the very thing
// such a row tests: a build before the owner lock that boots on a database is an arrival there,
// which stamps the database's mark of an earlier build (owner_lock_legacy_mark) for an hour
// (store.OwnerLockMarkWindow) and puts every process of this tree on it in the mode beside an
// earlier build. So the probe boots build once alone, on a database of its own (laneDatabaseAfter, a
// clone the row's database is not, at this tree's schema in production's order, which build boots
// on) and a NATS server of its own, reads the protocol its process registers
// (registeredOwnerLockProtocol), and kills the process. The answer is kept by the build's directory
// for the rest of the test binary, and a later row reads it without a boot.
func previousBuildOwnerLockProtocol(t *testing.T, build previousBuild) int {
	t.Helper()
	probed := &probedOwnerLockProtocols
	probed.Lock()
	defer probed.Unlock()
	if protocol, known := probed.byDir[build.dir]; known {
		return protocol
	}
	name := "probe of the previous build (" + shortCommit(build.commit) + ")"
	database := laneDatabaseAfter(t, build, cmdtest.Build(t, "envoy-dispatch"))
	_, nats := multiprocessNATS(t)
	probe := startDispatchProcessWith(t, databaseURLOf(t, database),
		dispatchOptions{NATSURL: nats, SigningKey: multiprocessSigningKey, Binary: build.path("envoy-dispatch")})
	protocol := registeredOwnerLockProtocol(t, database, probe, name, 60*time.Second)
	if err := probe.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the %s: %v", name, err)
	}
	select {
	case <-probe.Exited:
	case <-time.After(30 * time.Second):
		t.Fatalf("the %s was still running 30s after it was killed:\n%s", name, probe.Output.String())
	}
	probed.byDir[build.dir] = protocol
	return protocol
}
