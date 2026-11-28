//go:build e2e

// The live proof's driver for dispatch://INFRA-3518: the report of a production check's look gives
// the look's secret id, which only that look's own task text carries, and the daemon takes the
// look a report is of from that id, never from the turn the report came in (the turn match,
// supervise.Claim.ServingCheck). scripts/e2e/production-check-look-id.sh builds this driver with
// `-tags e2e` and owns the nats-server and the PostgreSQL it runs against; with `--base-commit`
// it lays this one file over a `git archive` of that commit and runs it there under
// PRODUCTION_CHECK_LOOK_ID_BASE=1, which is the proof's negative control.
//
// Each step is one check of the issue's Verification document, in the document's order, and
// prints `== <step>`, what it observed and `ok <step>`, or `FAIL <step> at <assertion>`:
//
//   - late turn (c-late-turn), four parts, each on a daemon and a check of its own: a look-2 task
//     whose send runs out of its deadline after the prompt reached the agent, and whose late turn
//     reports with the look's id (part 1); the same with the daemon restarted between that prompt
//     and its turn (part 2); an implementer's check, seeded as a check opened before the rule that
//     the tester checks production (part 3); and, on the wall clock, an acknowledged prompt whose
//     turn starts only after the daemon's wait for it ran out and its next send is in flight
//     (part 4).
//   - testing turn behind the close (c-no-id, departure 2 of the plan): a tester's testing turn
//     that starts while the first look's task is in flight is no report of that look, whatever it
//     names, and the look's own task reaches the connection after that turn.
//   - ended look (c-ended-look), two parts on one check: a report of a look whose bound ended it
//     counts until the next look starts, and a report of a look the next one replaced records
//     nothing.
//   - id hidden (c-id-hidden): the last step. It searches every look id the run saw in the routes
//     a role reads and in the daemon's whole log.
//
// The Verification document's other two checks of this proof, `task ends with its look`
// (c-task-ends) and `upgrade` (c-upgrade), are steps of their own, in that order, between `ended
// look` and `id hidden` (TestProductionCheckLookIDLive's list), and they feed the step `id hidden`
// as the steps above do: each prompt a role's agent reads goes through lookIDAgent, which records
// the look id of each task it reads (lookIDSeen.task), and a step reads the routes a role reads
// at each moment the check names through lookIDFixture.surfaces.
//
// Each step, and each part of `late turn`, runs as a subtest of its own on a daemon of its own,
// booted as this package's other whole-daemon proofs boot it (startDaemonLogging over the fake
// runtime, liveIssueProof of live_issue_proof_test.go), so one that fails ends there and the next
// still runs: the negative control's one run shows every step's own failure, each at the
// assertion the step names (lookIDLive.fails), as busy_relaunch_live_test.go's steps do. Each
// fixture is a tree of the run's own project: a root in accepting and its child, the issue whose
// production check a part drives, as api/handoff_check_route_test.go's fixture holds them. The
// child reaches testing as production's does — its implementer, a launch the previous daemon
// never recorded that the boot relaunches, completes over HTTP and its tester takes the testing
// task over the worker stream — and closes merged as that fixture closes it (departure 2): GitHub's
// merge of its pull request arrives while it is in testing, and a person then moves it to done on
// Dispatch, which opens the check and starts its first look on the tester's claim, which the
// close spares.
//
// The checking role's agent is a raw shim (lookIDAgent over liveClaimant): it reads every frame
// the daemon sends on a goroutine of its own, answers the protocol negotiation and the state probe
// (get_state) as the real shim and Oh My Pi do, answers busy (the pinned Oh My Pi's session_busy)
// each prompt that arrives in a turn, and hands every other prompt to the step, which answers it,
// late, busy or not at all. The implementer that walks the child to testing is liveClaimant as
// conflict_no_takeover_live_test.go plays it. The daemon's own send deadline is the configuration's
// (Config.WorkerRPCTimeout), and its wait for a turn is the machines' clock: still (fakeRuntime's
// stillClock) everywhere but part 4 of `late turn`, which boots on the wall clock (overrides.clock
// nil). The runtime's sweep of the claims' processes, which sends again a task that waits after a
// failed send or a turn that never came, is the driver's (lookIDFixture.sweep), since the fake
// runtime makes none. A look's bound and a look's wake are brought forward by SQL on
// outbox.next_at, and every seed the plan writes with the daemon stopped
// (lookIDFixture.restartWith) is written so. Each report is the raw JSON body the plugin's
// `handoff_complete` sends (checkOpenedAt, lookId, verdict, summary, wakeAt).
//
// The driver names no Go symbol the base commit lacks, so it builds over it: it reads the routes
// as raw JSON, takes the look id from the task's text, and never reads a column the change adds.
// Under PRODUCTION_CHECK_LOOK_ID_BASE=1 it sends no lookId, reads no look id, and runs only the
// parts each check's negative control names (lookIDLive.base).
package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// lookIDBaseEnv is set to 1 by the script's `--base-commit` run: the driver then runs over the
// base commit, whose daemon refuses a request member it does not know (lookId) and whose task
// gives no look id.
const lookIDBaseEnv = "PRODUCTION_CHECK_LOOK_ID_BASE"

// The run's fixed values: the script that owns the broker, the base branch of every pull request,
// the head the child's pull request is at and merges at, and the merge commit GitHub's merge names
// (publishMergeOf names it for the head).
const (
	lookIDScript = "scripts/e2e/production-check-look-id.sh"
	lookIDBranch = "main"
	lookIDHead   = "c0ffee0000000000000000000000000000000001"
	lookIDMerge  = "merge-" + lookIDHead
)

// lookIDWait bounds every wait of a step for the daemon to do what it is driven to: a step that
// never sees it fails there, and the next step still runs, rather than every later step of the run
// waiting out the test binary's deadline (testwait.Eventually).
const lookIDWait = 2 * time.Minute

// lookIDOutboxSweep is how often the fixtures' daemons sweep their outbox lanes
// (overrides.outboxSweep): the sweep is what runs a row whose due time a step brought forward by
// SQL, a look's bound or a look's wake, where the lane's own timer still waits for the time the row
// was queued for.
const lookIDOutboxSweep = time.Second

// lookIDQuietSends is how many of the daemon's send deadlines a step watches a connection for a
// task that must not come.
const lookIDQuietSends = 3

// What the driver reads of a look's task (workflow's checkTask): lookIDLine is the line that gives
// the look's id, lookIDOpenedLine the part that gives the check's opening time, and lookIDNumber
// the look's number.
var (
	lookIDLine       = regexp.MustCompile("look id `([^`]+)`")
	lookIDOpenedLine = regexp.MustCompile("check_opened_at `([^`]+)`")
	lookIDNumber     = regexp.MustCompile(`Look (\d+), woken by`)
)

// lookIDLive is one run of the proof: the test, or the step or part running now (run, part), the
// base mode, the assertions the running step failed at, and the record the step `id hidden`
// searches.
type lookIDLive struct {
	t    *testing.T
	base bool
	// at is the assertion the running step or part failed at (fails), and failed the parts of the
	// running step that failed, each with its assertion, which run prints.
	at     string
	failed []string
	seen   *lookIDSeen
	// daemonLog is the file every daemon of the run writes its log to (liveDaemonLogEnv).
	daemonLog string
}

// TestProductionCheckLookIDLive drives the proof's steps, each on a daemon of its own, in the
// order of the Verification document. A step added to the proof goes in this list before `id
// hidden`: `task ends with its look` (c-task-ends) after `ended look`, then `upgrade` (c-upgrade).
// `id hidden` stays last, since it searches every look id the steps before it saw.
func TestProductionCheckLookIDLive(t *testing.T) {
	// Not parallel: a live proof on the one NATS server LEGION_E2E_NATS_URL names, which writes the
	// file LEGION_E2E_DAEMON_LOG names, and whose steps watch spans for what must not happen.
	if os.Getenv(liveNATSURLEnv) == "" {
		t.Skipf("%s is unset, so there is no broker to drive the daemon's intake through; %s starts a nats-server and sets it", liveNATSURLEnv, lookIDScript)
	}
	r := &lookIDLive{t: t, base: os.Getenv(lookIDBaseEnv) == "1", seen: &lookIDSeen{}, daemonLog: os.Getenv(liveDaemonLogEnv)}
	if r.base {
		fmt.Printf("   %s=1: the driver sends no lookId, reads no look id and runs the parts each negative control names\n", lookIDBaseEnv)
	}
	r.run("late turn", r.lateTurn)
	r.run("testing turn behind the close", r.testingTurnBehindTheClose)
	r.run("ended look", r.endedLook)
	r.run("id hidden", r.idHidden)
}

// ---- how a step runs and fails ------------------------------------------------------------------

// run runs one step as a subtest of its own, between its `== name` and `ok name (<s>s)` lines,
// with every read failing that subtest rather than the whole run: r.t is the step's own testing.T
// while it runs. A step that fails prints `FAIL name at <assertion> (<s>s)`, naming the assertion
// it failed at (fails) or, for a step of parts, each part that failed with its own, and the next
// step runs.
func (r *lookIDLive) run(name string, body func()) {
	parent := r.t
	parent.Run(name, func(t *testing.T) {
		r.t, r.at, r.failed = t, "", nil
		started := time.Now()
		defer func() {
			r.t = parent
			if t.Failed() {
				at := strings.Join(r.failed, ", ")
				if at == "" {
					at = r.atOrWait()
				}
				fmt.Printf("FAIL %s at %s (%ds)\n", name, at, int(time.Since(started).Seconds()))
			}
		}()
		fmt.Printf("== %s\n", name)
		body()
		if !t.Failed() {
			fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
		}
	})
}

// part runs part n of the running step as a subtest of its own, between its `-- part n: what`
// and `ok part n (<s>s)` lines, so a part that fails prints `FAIL part n at <assertion> (<s>s)`
// and the step's next part still runs; the step then fails naming each part that failed.
func (r *lookIDLive) part(n int, what string, body func()) {
	step := r.t
	label := fmt.Sprintf("part %d", n)
	step.Run(label, func(t *testing.T) {
		r.t, r.at = t, ""
		started := time.Now()
		defer func() {
			r.t = step
			if t.Failed() {
				at := r.atOrWait()
				r.failed = append(r.failed, label+" "+at)
				fmt.Printf("FAIL %s at %s (%ds)\n", label, at, int(time.Since(started).Seconds()))
			}
		}()
		fmt.Printf("-- %s: %s\n", label, what)
		body()
		fmt.Printf("ok %s (%ds)\n", label, int(time.Since(started).Seconds()))
	})
}

// atOrWait is the assertion the running step or part failed at, or the words for a failure of a
// wait or a read of its own, which names no assertion.
func (r *lookIDLive) atOrWait() string {
	if r.at == "" {
		return "none of its named assertions (a wait or a read of its own)"
	}
	return r.at
}

// fails ends the running step or part at its named assertion, which run and part print and the
// script's negative control reads back.
func (r *lookIDLive) fails(assertion, format string, args ...any) {
	r.t.Helper()
	r.at = assertion
	r.t.Fatalf(assertion+": "+format, args...)
}

// note prints what a step drove or read, under its heading.
func (r *lookIDLive) note(format string, args ...any) {
	fmt.Printf("   "+format+"\n", args...)
}

// ---- what the step `id hidden` searches ----------------------------------------------------------

// lookIDSeen is the run's record of what the step `id hidden` searches, fed by every step: the
// text of each task that reached a role's connection (task) with the look id it gave, and each
// body of a route a role reads (read), with the moment it was read.
type lookIDSeen struct {
	mu    sync.Mutex
	ids   []lookIDOf
	texts []string
	reads []lookIDRead
}

// lookIDOf is one look id the run saw, and where: the step, the part and the look whose task gave
// it.
type lookIDOf struct {
	id, where string
}

// lookIDRead is one body of a route a role reads, the moment the step read it, and the route.
type lookIDRead struct {
	moment, route string
	body          []byte
}

// task records text, a task that reached a role's connection where, and answers the look id it
// gives, "" for a task that gives none. Under the base mode no id is read.
func (s *lookIDSeen) task(where, text string, base bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.texts = append(s.texts, text)
	if base {
		return ""
	}
	found := lookIDLine.FindStringSubmatch(text)
	if found == nil {
		return ""
	}
	if !slices.ContainsFunc(s.ids, func(seen lookIDOf) bool { return seen.id == found[1] }) {
		if number := lookIDNumber.FindStringSubmatch(text); number != nil {
			where += ", look " + number[1]
		}
		s.ids = append(s.ids, lookIDOf{id: found[1], where: where})
	}
	return found[1]
}

// read records body, the answer route gave at moment.
func (s *lookIDSeen) read(moment, route string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, lookIDRead{moment: moment, route: route, body: append([]byte(nil), body...)})
}

// ---- one fixture: a daemon of its own over a tree whose child's check a part drives --------------

// lookIDFixture is one daemon of the run and the tree it runs: liveIssueProof grown for a child,
// whose key is the proof's issue and whose pull request is the proof's, under a root of its own;
// the daemon's own log kept in memory as it is written; what each boot is tuned with; and the
// agents whose shims a restart reconnects.
type lookIDFixture struct {
	liveIssueProof
	r *lookIDLive
	// where names the fixture in the record of the step `id hidden` and in its notes: the step
	// and the part.
	where string
	root  string
	// logged is the running daemon's own log, kept in memory as it is written.
	logged *recordingLog
	tune   func(*overrides)
	live   []*lookIDAgent
}

// fixture readies a daemon of the run's own over a fresh project, its tree not yet seeded and the
// daemon not yet booted: the child is the proof's issue, under `production_check: tester`, with a
// check bound of two weeks, the shipped default. tune, when not nil, sets what the part's boots
// need beyond the fixture's own.
func (r *lookIDLive) fixture(where string, tune func(*overrides)) *lookIDFixture {
	r.t.Helper()
	p := newLiveIssueProof(r.t, lookIDScript, lookIDBranch, "the child whose production check a report names by its look id")
	f := &lookIDFixture{liveIssueProof: p, r: r, where: where, root: p.cfg.Project + "-1", tune: tune}
	f.key = p.cfg.Project + "-2"
	f.cfg.Projects = map[string]config.Project{f.cfg.Project: {ProductionCheck: claim.RoleTester}}
	f.cfg.ProductionCheckBound = 336 * time.Hour
	f.serveGitHub(&liveGitHub{sha: lookIDHead})
	// Registered before the boot, so it runs after the daemon's stop.
	r.t.Cleanup(f.leaveStreamEmpty)
	return f
}

// leaveStreamEmpty leaves the run's ENVOY_NOTIFICATIONS stream as the next fixture's proof
// requires it at its create (testnats.CreateStream, which takes the stream that stands and fails
// on any message or consumer it holds): once this fixture's daemon has stopped, the consumers that
// daemon made go and the stream is purged. The stream itself stays: testnats's own empty says why
// a delete of a stream can leave its store, and so its messages, behind for the next create.
func (f *lookIDFixture) leaveStreamEmpty() {
	ctx := context.Background()
	stream, err := f.js.Stream(ctx, "ENVOY_NOTIFICATIONS")
	if err != nil {
		f.t.Errorf("take the notification stream to leave it empty: %v", err)
		return
	}
	listed := stream.ConsumerNames(ctx)
	var consumers []string
	for name := range listed.Name() {
		consumers = append(consumers, name)
	}
	if err := listed.Err(); err != nil {
		f.t.Errorf("list the notification stream's consumers: %v", err)
	}
	for _, name := range consumers {
		if err := stream.DeleteConsumer(ctx, name); err != nil {
			f.t.Errorf("delete the notification stream's consumer %s: %v", name, err)
		}
	}
	if err := stream.Purge(ctx); err != nil {
		f.t.Errorf("purge the notification stream: %v", err)
	}
}

// seedRoot records the root as api/handoff_check_route_test.go's fixture holds it: handed to
// Legion, in accepting, holding its tree's admission slot, its lifecycle open under the workflow,
// and on Dispatch's board.
func (f *lookIDFixture) seedRoot(ctx context.Context, tx pgx.Tx) error {
	if err := f.records.PutIssue(ctx, tx, record.Issue{
		Key: f.root, Repo: liveRepo, DefaultBranch: lookIDBranch, GitHubRepo: liveRepo, Tree: f.root, Project: f.cfg.Project,
		Title: "the tree the checked child merges in", Phase: phase.Accepting, Generation: 1, Status: "in_progress",
		DispatchStatus: "in_progress", Rank: "M", LastDispatchSeq: 1, HandedOver: true,
	}); err != nil {
		return err
	}
	return f.admit(ctx, tx, f.root)
}

// child is the child's record at ph with status, under the root.
func (f *lookIDFixture) child(ph phase.Phase, status string) record.Issue {
	parent := f.root
	return record.Issue{
		Key: f.key, Repo: liveRepo, DefaultBranch: lookIDBranch, GitHubRepo: liveRepo, Tree: f.root, Parent: &parent,
		Project: f.cfg.Project, Title: f.title, Phase: ph, Generation: 1, Status: status, DispatchStatus: status,
		Rank: "N", LastDispatchSeq: 1,
	}
}

// openLifecycle opens the tree's lifecycle under the workflow, which every launch of its claims
// is bound to.
func (f *lookIDFixture) openLifecycle() {
	f.t.Helper()
	if _, err := f.st.OpenTreeLifecycle(context.Background(), f.project, f.root, treelifecycle.AuthorityWorkflow); err != nil {
		f.t.Fatalf("open the lifecycle of %s: %v", f.root, err)
	}
}

// seedImplementing records the tree as the daemon holds it the moment the child's implementer
// began work: the root (seedRoot), the child implementing on an open pull request at lookIDHead,
// and the child's implementer's claim as a launch a previous daemon began and never recorded,
// serving run 1, which the boot relaunches, as seedImplementing does for a root.
func (f *lookIDFixture) seedImplementing() {
	t := f.t
	t.Helper()
	ctx := context.Background()
	f.board.show(f.root, "in_progress")
	f.board.place(f.key, "in_progress", f.root, false)
	f.write("the tree, the child's pull request and the root's slot", func(tx pgx.Tx) error {
		if err := f.seedRoot(ctx, tx); err != nil {
			return err
		}
		if err := f.records.PutIssue(ctx, tx, f.child(phase.Implementing, "in_progress")); err != nil {
			return err
		}
		return f.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: f.key, Repo: liveRepo, Number: f.number, Branch: claim.IssueBranch(f.key),
			HeadSHA: lookIDHead, Base: lookIDBranch, Failing: []string{},
		})
	})
	f.openLifecycle()
	if _, err := f.st.AdmitClaim(ctx, supervise.Claim{
		Token: f.token(claim.RoleImplementer), Project: f.project, Tree: f.root, Issue: f.key, Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
		ServingGeneration: 1,
	}); err != nil {
		t.Fatalf("record the implementer's unfinished launch: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		writePrompt(t, f.cfg, f.token(role))
	}
}

// boot starts the whole daemon on the store as it stands, as liveIssueProof.boot does, with the
// daemon's own log also kept in memory as it is written (recordingLog), the outbox swept every
// lookIDOutboxSweep, and the fixture's tune.
func (f *lookIDFixture) boot() {
	f.t.Helper()
	f.rt.InPod = true
	o := fakeRuntime(f.rt, f.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = f.github.url
	o.outboxSweep = lookIDOutboxSweep
	if f.tune != nil {
		f.tune(&o)
	}
	f.logged = newRecordingLog(f.log())
	f.d = startDaemonLogging(f.t, f.cfg, o, slog.New(f.logged))
}

// sweep stands in for one pass of the runtime's sweep over role's process, which the fake runtime
// makes none of: it reports the process alive at the claim's locator (runtime.Alive). That pass is
// what sends again a task whose send no answer reached, or whose acknowledged prompt started no
// turn within the daemon's wait: each such task waits for the next sweep, hello, turn end or ready
// (supervise's refused and noTurn).
func (f *lookIDFixture) sweep(role claim.Role) {
	f.t.Helper()
	held := f.claimView(role)
	if held.Locator == nil {
		f.t.Fatalf("the %s's claim is %s with no process to sweep", role, held.State)
	}
	f.rt.Emit(runtime.Observation{Locator: *held.Locator, Kind: runtime.Alive})
}

// restartWith stops the daemon, runs between on the store while no daemon runs, which is where a
// deploy leaves what the daemon before wrote, boots the daemon again on the same store and ports,
// and reconnects each live agent's shim with the boot token its launch holds, as a pane's shim
// does when the daemon comes back (stepRecordsRig.restartWith).
func (f *lookIDFixture) restartWith(between func()) {
	f.t.Helper()
	f.d.stop()
	if between != nil {
		between()
	}
	rebindHeldPorts(f.t, &f.cfg)
	f.boot()
	for _, a := range f.live {
		a.reconnect()
	}
}

// exec runs one statement against the daemon's store, failing the step with what it was for.
func (f *lookIDFixture) exec(what, statement string, args ...any) {
	f.t.Helper()
	if _, err := f.st.Pool().Exec(context.Background(), statement, args...); err != nil {
		f.t.Fatalf("%s: %v", what, err)
	}
}

// check is the child's production check as the store holds it, through the store's own read
// (readProductionCheck), whose fields the base commit has too.
func (f *lookIDFixture) check() record.ProductionCheck {
	f.t.Helper()
	return readProductionCheck(f.t, f.st.Pool(), f.records, f.key)
}

// awaitCheck waits for the child's check to hold what want says, and returns it.
func (f *lookIDFixture) awaitCheck(what string, want func(record.ProductionCheck) bool) record.ProductionCheck {
	f.t.Helper()
	var got record.ProductionCheck
	testwait.WithinPolling(f.t, lookIDWait, 100*time.Millisecond, what, func() bool {
		var open bool
		if err := f.st.Pool().QueryRow(context.Background(), `select exists (select 1 from production_checks where issue = $1)`, f.key).Scan(&open); err != nil {
			f.t.Fatalf("read whether %s has a production check: %v", f.key, err)
		}
		if !open {
			return false
		}
		got = f.check()
		return want(got)
	})
	return got
}

// reports is how many reports on the child's check the intake has processed: each is an event of
// the completion route named for the check, its look and its verdict (api's checkReport).
func (f *lookIDFixture) reports() int {
	f.t.Helper()
	var count int
	if err := f.st.Pool().QueryRow(context.Background(),
		"select count(*) from processed_events where source = 'api' and event_id like $1", "check:"+f.key+":%").Scan(&count); err != nil {
		f.t.Fatalf("count the check reports of %s: %v", f.key, err)
	}
	return count
}

// surfaces reads the routes a role reads — GET /legion/v1/state, which the legion tool's
// read_state calls, and GET /legion/v1/issues/<child>/record, which its read_record calls
// (packages/pi-shared/src/daemon-client.ts) — and keeps each body for the step `id hidden`,
// under moment.
func (f *lookIDFixture) surfaces(moment string) {
	f.t.Helper()
	for _, route := range []string{"/legion/v1/state", "/legion/v1/issues/" + f.key + "/record"} {
		status, body := f.d.request(http.MethodGet, route, nil, false)
		if status != http.StatusOK {
			f.t.Fatalf("GET %s = %d %s, want 200", route, status, body)
		}
		f.r.seen.read(f.where+": "+moment, "GET "+route, body)
	}
}

// claimView is the operator's view of role's claim on the child.
func (f *lookIDFixture) claimView(role claim.Role) api.OperatorClaim {
	f.t.Helper()
	return f.d.claim(f.token(role))
}

// launches is how many launches the fake runtime was asked to make of role's claim.
func (f *lookIDFixture) launches(role claim.Role) int {
	token, launched := f.token(role), 0
	for _, call := range f.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			launched++
		}
	}
	return launched
}

// logMark is how many records the running daemon has logged, from which loggedSince reads.
func (f *lookIDFixture) logMark() int { return f.logged.count() }

// awaitLogged waits for the running daemon to log, after the mark from, a line of role's claim
// whose message starts with prefix and whose attributes hold attrs, and returns it.
func (f *lookIDFixture) awaitLogged(what string, from int, role claim.Role, prefix string, attrs map[string]any) loggedRecord {
	f.t.Helper()
	token := string(f.token(role))
	var found *loggedRecord
	testwait.WithinPolling(f.t, lookIDWait, 50*time.Millisecond, what, func() bool {
		found = f.logged.firstAfter(from, func(kept loggedRecord) bool {
			if !strings.HasPrefix(kept.Message, prefix) || kept.Attrs["claim"] != token {
				return false
			}
			for key, value := range attrs {
				if kept.Attrs[key] != value {
					return false
				}
			}
			return true
		})
		return found != nil
	})
	return *found
}

// publishDone publishes Dispatch's own issue.updated event of a person moving the child to done
// under the root, as Dispatch publishes one, the board moving with it.
func (f *lookIDFixture) publishDone() {
	f.t.Helper()
	f.board.place(f.key, "done", f.root, false)
	payload, err := json.Marshal(map[string]any{
		"id": 2, "issue_key": f.key, "project": f.cfg.Project, "seq": 2, "type": "issue.updated",
		"actor": map[string]string{"kind": "user", "id": "operator"}, "notify": true, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"key": f.key, "status": "done", "title": f.title, "parent": f.root, "rank": "N",
			"labels": liveDispatchLabels(false), "last_seq": 2, "project": f.cfg.Project, "resolved_repo": liveRepo,
		},
	})
	if err != nil {
		f.t.Fatalf("encode the Dispatch event of %s: %v", f.key, err)
	}
	f.publish("notifications.dispatch.issue."+f.key+".issue.updated", "dispatch", f.key+"-done-2", string(payload))
}

// grant is a capability grant minted now for session, a session of the child's tree, as its
// `legion` tool mints one per operation: the tree is the root's, which liveSession.grant, a
// grant on a root's own tree, does not name.
func (f *lookIDFixture) grant(session *liveSession) string {
	f.t.Helper()
	status, body := f.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: session.session, Secret: session.secret, Tree: f.root, Issue: f.key}, false)
	if status != http.StatusOK {
		f.t.Fatalf("the grant of %s = %d %s, want 200", session.token, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		f.t.Fatalf("decode the grant of %s: %v", session.token, err)
	}
	return granted.GrantID
}

// toTesting walks the seeded child to testing as production does: its implementer, which the
// boot relaunched, completes implementing at lookIDHead, naming a handoff record of its own, which
// hands the testing phase to the tester; the tester registers, takes the testing task and works
// it. It answers the tester, in the turn of that task.
func (f *lookIDFixture) toTesting() *lookIDAgent {
	t := f.t
	t.Helper()
	imp := f.claimant(claim.RoleImplementer, "ses_implementer")
	imp.startTurn("")
	status, body := f.d.request(http.MethodPost, "/legion/v1/handoff/complete", api.HandoffCompleteRequest{
		GrantID: f.grant(imp.liveSession), Summary: "implemented it", Commit: lookIDHead,
		HandoffID: imp.handoff("implement"), HandoffVersion: 1,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("the implementer's completion = %d %s, want 200", status, body)
	}
	imp.endTurn()
	f.waitFor("the child in testing", func(got api.Issue) bool { return got.Phase == phase.Testing })
	tester := f.agent(f.register(claim.RoleTester, "ses_tester"))
	tester.ready()
	testing := tester.prompt("the tester's testing task", "Phase: testing")
	tester.ack(testing)
	tester.begin(testing.ID)
	f.r.note("%s: %s reached testing; its tester was launched, registered and prompted its testing task (%s), and works it",
		f.where, f.key, firstLine(testing.Message))
	return tester
}

// closeMerged closes the child merged while it is in testing, as the fixture of
// api/handoff_check_route_test.go closes it (departure 2): GitHub's merge of its pull request
// arrives, which the daemon records and tells the architect of, since a merge during testing
// closes nothing, and then a person moves the child to done on Dispatch. The close spares the
// check's role and opens the tester's production check, whose first look starts on the tester's
// claim. It answers the check open at look 1.
func (f *lookIDFixture) closeMerged() record.ProductionCheck {
	f.t.Helper()
	f.waitProcessed(f.publishMergeOf(f.number, f.key, lookIDHead, lookIDBranch))
	f.publishDone()
	check := f.awaitCheck("the close to open the child's production check at look 1", func(c record.ProductionCheck) bool {
		return c.Open() && c.Looks == 1 && c.LookOpen
	})
	if check.Role != claim.RoleTester {
		f.r.fails("check-opened", "the check the close opened is the %s's, want the tester's", check.Role)
	}
	f.r.note("%s: the merge arrived in testing and the person's done closed %s; the check opened at %s for the tester, look 1 open",
		f.where, f.key, check.OpenedAt.UTC().Format(time.RFC3339Nano))
	return check
}

// ---- one role's agent ----------------------------------------------------------------------------

// lookIDAgent is one role's claim as the step plays its agent over the worker stream: the
// registered session (liveClaimant), every prompt the daemon sends it, read on a goroutine of its
// own (listen), and whether the agent is in a turn, which its answer to the daemon's state probe
// says. A prompt that arrives while the agent is in no turn is handed to the step, which answers
// it; one that arrives in a turn is answered busy at once, as the pinned Oh My Pi answers a prompt
// while a turn streams. Every prompt is kept with the moment it arrived (since).
type lookIDAgent struct {
	*liveClaimant
	f       *lookIDFixture
	prompts chan shimwire.Prompt
	mu      sync.Mutex
	inTurn  bool
	arrived []lookIDPrompt
}

// lookIDPrompt is one prompt that reached an agent's connection, when it arrived, and whether the
// agent answered it busy at once, being in a turn.
type lookIDPrompt struct {
	prompt shimwire.Prompt
	at     time.Time
	busy   bool
}

// agent plays session, a registered session of the fixture's, and reads its connection from now
// on (listen).
func (f *lookIDFixture) agent(session *liveSession) *lookIDAgent {
	a := &lookIDAgent{liveClaimant: &liveClaimant{liveSession: session}, f: f, prompts: make(chan shimwire.Prompt, 16)}
	a.listen()
	f.live = append(f.live, a)
	return a
}

// listen reads every frame the daemon sends the agent's connection until it closes or the step
// ends it (shim.serve): it answers the protocol negotiation and the state probe as the real shim
// and Oh My Pi answer them, records each prompt's task for the step `id hidden`, answers busy a
// prompt that arrives in a turn and hands every other prompt to the step. An answer that fails to
// write fails the step unless the daemon has begun to stop, whose stop closes the connection under
// it.
func (a *lookIDAgent) listen() {
	sh, d := a.sh, a.f.d
	reply := func(frame shimwire.Frame) {
		if err := sh.writer.WriteFrame(frame); err != nil && !d.stopped.Load() {
			a.f.t.Errorf("answer %s for the %s: %v", frame.FrameType(), a.role, err)
		}
	}
	sh.serve(func() {
		for {
			line, err := sh.lines.ReadBytes('\n')
			if err != nil {
				return
			}
			frame, err := shimwire.Decode(line)
			if err != nil {
				continue
			}
			switch got := frame.(type) {
			case shimwire.NegotiateProtocol:
				reply(shimwire.Response{ID: got.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
			case shimwire.GetState:
				a.mu.Lock()
				streaming := a.inTurn
				a.mu.Unlock()
				data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
				reply(shimwire.Response{ID: got.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
			case shimwire.Prompt:
				a.f.r.seen.task(a.f.where, got.Message, a.f.r.base)
				a.mu.Lock()
				busy := a.inTurn
				a.arrived = append(a.arrived, lookIDPrompt{prompt: got, at: time.Now(), busy: busy})
				a.mu.Unlock()
				if busy {
					reply(busyAnswer(got))
					continue
				}
				select {
				case a.prompts <- got:
				case <-sh.stopping:
					return
				}
			}
		}
	})
}

// reconnect dials the running daemon's worker stream again with the boot token of the claim's
// latest launch, as the pane's shim does when the daemon comes back, and reads the new connection.
func (a *lookIDAgent) reconnect() {
	t := a.f.t
	t.Helper()
	a.f.built.mu.Lock()
	address := a.f.built.address
	a.f.built.mu.Unlock()
	sh, err := ackedShim(t, address, lastLaunch(t, a.f.rt, a.token).BootToken)
	if err != nil {
		t.Fatalf("the %s's shim reconnecting to the restarted daemon: %v", a.role, err)
	}
	a.sh = sh
	a.listen()
}

// next is the next prompt handed to the step within, and whether one came.
func (a *lookIDAgent) next(within time.Duration) (shimwire.Prompt, bool) {
	select {
	case prompt := <-a.prompts:
		return prompt, true
	case <-time.After(within):
		return shimwire.Prompt{}, false
	}
}

// prompt is the next prompt handed to the step, which must say want: what names it.
func (a *lookIDAgent) prompt(what, want string) shimwire.Prompt {
	t := a.f.t
	t.Helper()
	prompt, ok := a.next(lookIDWait)
	if !ok {
		held := a.f.d.claim(a.token)
		t.Fatalf("%s did not reach the %s's connection within %s: its claim is %s holding %s", what, a.role, lookIDWait, held.State, describeHeld(held.Pending))
	}
	if !strings.Contains(prompt.Message, want) {
		t.Fatalf("the %s was prompted %q, want %s, saying %q", a.role, firstLine(prompt.Message), what, want)
	}
	return prompt
}

// since is every prompt that reached the agent's connection after at, answered busy or not.
func (a *lookIDAgent) since(at time.Time) []lookIDPrompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	var after []lookIDPrompt
	for _, got := range a.arrived {
		if got.at.After(at) {
			after = append(after, got)
		}
	}
	return after
}

// ack is Oh My Pi acknowledging prompt: its admission.
func (a *lookIDAgent) ack(prompt shimwire.Prompt) {
	a.sh.send(shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: true})
}

// busy is Oh My Pi refusing prompt because its session is in a turn (busyAnswer).
func (a *lookIDAgent) busy(prompt shimwire.Prompt) {
	a.sh.send(busyAnswer(prompt))
}

// busyAnswer is Oh My Pi's refusal of prompt while a turn streams: session_busy, in the words the
// pinned Oh My Pi gives.
func busyAnswer(prompt shimwire.Prompt) shimwire.Response {
	return shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: false, Error: keptBusyRefusal, Code: shimwire.CodeSessionBusy}
}

// begin is the agent beginning a turn, on prompt when the turn is that prompt's and on nothing
// when it is a turn of its own (liveClaimant.startTurn): from here its answer to the state probe
// is that it streams, and a prompt sent it is answered busy.
func (a *lookIDAgent) begin(prompt string) {
	a.f.t.Helper()
	a.mu.Lock()
	a.inTurn = true
	a.mu.Unlock()
	a.startTurn(prompt)
}

// finish is the agent ending its turn: the agent_end, and behind it the result of the prompt the
// turn was, where it was one's. It waits for nothing, since what the claim stands in after a
// report's turn is what the step reads.
func (a *lookIDAgent) finish() {
	a.f.t.Helper()
	a.mu.Lock()
	a.inTurn = false
	a.mu.Unlock()
	a.sh.send(shimwire.AgentEnd{})
	if a.turn != "" {
		a.sh.send(shimwire.PromptResult{ID: a.turn, AgentInvoked: true})
	}
	a.turn = ""
}

// awaitIdle waits for the agent's claim to stand idle holding no task: its turn over and the task
// that turn was retired.
func (a *lookIDAgent) awaitIdle(what string) {
	a.f.t.Helper()
	testwait.WithinPolling(a.f.t, lookIDWait, 50*time.Millisecond, what, func() bool {
		held := a.f.d.claim(a.token)
		return held.State == string(supervise.StateIdle) && held.Pending == nil
	})
}

// lookRead is what the agent read of a look's task: the check's opening time and the look's id,
// "" under the base mode.
type lookRead struct {
	opened time.Time
	id     string
}

// readLook reads the check's opening time and the look's id from prompt, a look's task, as the
// agent reads them: the task is the one text that gives the id.
func (a *lookIDAgent) readLook(prompt shimwire.Prompt) lookRead {
	t := a.f.t
	t.Helper()
	opened := lookIDOpenedLine.FindStringSubmatch(prompt.Message)
	if opened == nil {
		t.Fatalf("the look's task gives no check_opened_at: %s", prompt.Message)
	}
	at, err := time.Parse(time.RFC3339Nano, opened[1])
	if err != nil {
		t.Fatalf("the look's task's check_opened_at %q is no RFC 3339 time: %v", opened[1], err)
	}
	read := lookRead{opened: at}
	if !a.f.r.base {
		found := lookIDLine.FindStringSubmatch(prompt.Message)
		if found == nil {
			a.f.r.fails("look-id", "the look's task gives no look id: %s", prompt.Message)
		}
		read.id = found[1]
	}
	return read
}

// report posts the agent's report of verdict, as the plugin's handoff_complete sends it: the
// check's opening time (none for a zero opened), the look's id (none for "", and none under the
// base mode), and wake, when not nil. It answers the status and the refusal, and keeps the answer
// for the step `id hidden`.
func (a *lookIDAgent) report(opened time.Time, lookID, verdict string, wake *time.Time) (int, api.Failure) {
	t := a.f.t
	t.Helper()
	body := map[string]any{"grantId": a.f.grant(a.liveSession), "summary": "production observed for a " + verdict, "verdict": verdict}
	if !opened.IsZero() {
		body["checkOpenedAt"] = opened.UTC().Format(time.RFC3339Nano)
	}
	if lookID != "" && !a.f.r.base {
		body["lookId"] = lookID
	}
	if wake != nil {
		body["wakeAt"] = wake.UTC().Format(time.RFC3339Nano)
	}
	status, answer := a.f.d.request(http.MethodPost, "/legion/v1/handoff/complete", body, false)
	a.f.r.seen.read(a.f.where+": the answer to a report", "POST /legion/v1/handoff/complete", answer)
	if status == http.StatusOK {
		return status, api.Failure{}
	}
	var failure api.Failure
	if err := json.Unmarshal(answer, &failure); err != nil {
		t.Fatalf("decode the %s's refused report %s: %v", a.role, answer, err)
	}
	return status, failure
}

// describeHeld is a claim's pending task as a failure names it.
func describeHeld(p *api.DeliveryView) string {
	if p == nil {
		return "no task"
	}
	confirmed := "unconfirmed"
	if p.ConfirmedAt != nil {
		confirmed = "confirmed"
	}
	return fmt.Sprintf("task %q (%s, %d failed sends: %q)", firstLine(p.Task), confirmed, p.SendFailures, p.LastSendError)
}

// checkTaskLeadText opens every task of a production check's look (workflow's checkTaskLead).
const checkTaskLeadText = "Production check of "

// lookTask says whether text is the task of look n of a production check (workflow's checkTask).
func lookTask(text string, n int) bool {
	return strings.Contains(text, checkTaskLeadText) && strings.Contains(text, fmt.Sprintf("Look %d, woken by", n))
}

// ---- step `late turn` ----------------------------------------------------------------------------

// lateTurn is c-late-turn: a report that the checking role makes in the turn of the open look's
// task is recorded against that look, also when the turn match fails, and the daemon then stops
// sending that task and clears the claim's prompt counts. Each part runs on a daemon and a check
// of its own. At the base commit parts 1 and 3, which the check's negative control names failing,
// and part 4, which it names passing there too, run, and part 2 does not.
func (r *lookIDLive) lateTurn() {
	r.part(1, "the look-2 send runs out of its deadline after the prompt reached the agent; later sends are answered busy, and the late turn reports look 2", func() {
		f, tester := r.secondLook("late turn part 1", nil)
		task := tester.prompt("the look-2 task", checkTaskLeadText)
		r.lateSend(f, tester, task)
		r.lateTurnReport(f, tester, tester.readLook(task))
	})
	if r.base {
		r.note("part 2 does not run at the base commit: the check's negative control names no answer of it there")
	} else {
		r.part(2, "the daemon restarts after the look-2 prompt reached the agent and before its turn starts", func() {
			f, tester := r.secondLook("late turn part 2", nil)
			task := tester.prompt("the look-2 task", checkTaskLeadText)
			tester.ack(task)
			testwait.WithinPolling(f.t, lookIDWait, 50*time.Millisecond, "the daemon to record the look-2 prompt acknowledged, in the store too", func() bool {
				held := f.claimView(claim.RoleTester).Pending
				var stored bool
				if err := f.st.Pool().QueryRow(context.Background(), `select exists (select 1 from pending_task_deliveries where claim_token = $1 and delivered_at is not null)`,
					string(f.token(claim.RoleTester))).Scan(&stored); err != nil {
					f.t.Fatalf("read the tester's stored delivery: %v", err)
				}
				return held != nil && held.DeliveredAt != nil && stored
			})
			f.restartWith(nil)
			r.note("the agent acknowledged the look-2 prompt and the daemon restarted before its turn; the shim reconnected to the new daemon")
			tester.begin(task.ID)
			r.note("the agent's turn of the look-2 prompt started after the restart")
			r.lateTurnReport(f, tester, tester.readLook(task))
		})
	}
	r.part(3, "an implementer's check, seeded as one opened before the rule that the tester checks production, at look 1, on a claim that served its implementing run", func() {
		f := r.fixture("late turn part 3", nil)
		opened := f.seedImplementerCheck()
		f.boot()
		r.note("seeded before the boot: %s done on its merged pull request, its open check of the implementer at look 1 (opened %s) with the start row an older daemon queued for it, and its implementer suspended in its session, having served run 1 and no look",
			f.key, opened.UTC().Format(time.RFC3339Nano))
		imp := f.resume(claim.RoleImplementer, "ses_implementer", 0, "the look-1 start to resume the implementer")
		task := imp.prompt("the look-1 task", checkTaskLeadText)
		r.lateSend(f, imp, task)
		read := imp.readLook(task)
		status, failure := imp.report(read.opened, read.id, "pass", nil)
		r.note("in that turn the implementer reported pass with check_opened_at %s and the look id the look-1 task gave: %d %s %s",
			read.opened.UTC().Format(time.RFC3339Nano), status, failure.Code, failure.Error)
		if status != http.StatusOK {
			r.fails("report", "the implementer's report of look 1 = %d %s %s, want 200", status, failure.Code, failure.Error)
		}
		imp.finish()
		closed := f.awaitCheck("the report to close the implementer's check", func(c record.ProductionCheck) bool { return !c.Open() })
		if closed.Outcome != record.CheckRecorded {
			r.fails("closed-pass", "the implementer's check closed as %s, want %s, the pass", closed.Outcome, record.CheckRecorded)
		}
		r.note("the check closed as %s, the pass", closed.Outcome)
	})
	r.part(4, "on the wall clock: the look-2 prompt is acknowledged, no turn starts within the daemon's wait, and the turn starts while the next send is in flight, which is answered busy", func() {
		f, tester := r.secondLook("late turn part 4", func(o *overrides) { o.clock = nil })
		task := tester.prompt("the look-2 task", checkTaskLeadText)
		mark := f.logMark()
		tester.ack(task)
		ran := f.awaitLogged("the daemon's wait for the look-2 prompt's turn to run out", mark, claim.RoleTester,
			"supervise: acknowledged, and no turn started within the wait", nil)
		r.note("the agent acknowledged the look-2 prompt and started no turn; the daemon's %v wait for its turn ran out", ran.Attrs["wait"])
		f.sweep(claim.RoleTester)
		again := tester.prompt("the look-2 task sent again at the runtime's next sweep", checkTaskLeadText)
		mark = f.logMark()
		tester.begin(task.ID)
		tester.busy(again)
		f.awaitLogged("the daemon to read the busy answer to the second send", mark, claim.RoleTester, "supervise: the agent refused the prompt", map[string]any{"delivery": again.DeliveryID})
		r.note("the daemon sent the task again at the runtime's next sweep; the agent's turn of the first prompt started while that send was in flight, and the agent answered it busy")
		f.surfaces("after a busy answer to look 2's task")
		r.lateTurnReport(f, tester, tester.readLook(task))
	})
}

// secondLook brings a fixture's check to the start of look 2 as parts 1, 2 and 4 of `late turn`
// start: the child closes merged behind the tester's testing turn, so look 1's task waits for that
// turn; the turn ends, look 1's task reaches the tester, and its turn reports pending with a wake,
// check_opened_at and the look's id, which the daemon records and which suspends the tester. With
// no daemon running, the tester's claim is then given one prompt retirement already counted (SQL
// on claims.prompt_retires, which the base has too: a report clears the counts, so the seed comes
// after look 1's), and the daemon boots again. The wake is brought forward by SQL on
// outbox.next_at, look 2 starts and resumes the tester's session, and the tester comes up in it.
// It answers the fixture and the tester, ready, look 2's task not yet read.
func (r *lookIDLive) secondLook(where string, tune func(*overrides)) (*lookIDFixture, *lookIDAgent) {
	f := r.fixture(where, tune)
	f.seedImplementing()
	f.boot()
	tester := f.toTesting()
	f.closeMerged()
	f.surfaces("look 1 open behind the testing turn, before its task is sent")
	tester.finish()
	first := tester.prompt("the look-1 task", checkTaskLeadText)
	tester.ack(first)
	tester.begin(first.ID)
	read := tester.readLook(first)
	wake := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
	if status, failure := tester.report(read.opened, read.id, "pending", &wake); status != http.StatusOK {
		r.t.Fatalf("the tester's report of look 1 = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	f.surfaces("after the report of look 1")
	tester.finish()
	r.note("the testing turn ended, look 1's task reached the tester, and its turn reported pending with wake %s: 200", wake.Format(time.RFC3339Nano))
	f.awaitSuspended(claim.RoleTester, "the pending report to suspend the tester")
	launched := f.launches(claim.RoleTester)
	f.live = slices.DeleteFunc(f.live, func(a *lookIDAgent) bool { return a == tester })
	f.restartWith(func() {
		f.exec("count one prompt retirement on the tester's claim", "update claims set prompt_retires = 1 where token = $1", string(f.token(claim.RoleTester)))
	})
	r.note("with the daemon stopped, the suspended tester's claim was given one prompt retirement, and the daemon booted again")
	f.exec("bring the wake of look 2 forward", "update outbox set next_at = now() where issue = $1 and kind = $2", f.key, string(record.OutboxKindCheckWake))
	f.awaitCheck("the wake to start look 2", func(c record.ProductionCheck) bool { return c.Looks == 2 && c.LookOpen })
	f.surfaces("after look 2 starts")
	return f, f.resume(claim.RoleTester, "ses_tester", launched, "look 2's start to resume the tester")
}

// awaitSuspended waits for role's claim to stand suspended both as its machine publishes it and as
// the store holds it: a machine publishes a claim before the write that keeps it, and a daemon
// stopped between the two boots again on the claim the store held before the suspension, as
// stepClaimant.waitState says.
func (f *lookIDFixture) awaitSuspended(role claim.Role, what string) {
	f.t.Helper()
	token := f.token(role)
	testwait.WithinPolling(f.t, lookIDWait, 100*time.Millisecond, what, func() bool {
		return f.d.claim(token).State == string(supervise.StateSuspended) &&
			storedClaimState(f.t, f.st.Pool(), token) == supervise.StateSuspended
	})
}

// resume waits for the daemon to launch role's claim again after the launched launches it had
// made, which must resume session's file, and for the claim to hold that launch's process, so its
// shim's hello is taken; it then brings the agent up in that launch as the plugin does (register),
// reads the routes a role reads while the look's task cannot have been sent yet, and reports the
// agent ready.
func (f *lookIDFixture) resume(role claim.Role, session string, launched int, what string) *lookIDAgent {
	t := f.t
	t.Helper()
	testwait.WithinPolling(t, lookIDWait, 50*time.Millisecond, what, func() bool { return f.launches(role) > launched })
	launch := lastLaunch(t, f.rt, f.token(role))
	if file := launch.ResumeSessionFile; file != "/sessions/"+session+".jsonl" {
		t.Fatalf("the daemon launched the %s again with the session file %q, want a resume of its own session's", role, file)
	}
	testwait.WithinPolling(t, lookIDWait, 50*time.Millisecond, fmt.Sprintf("the %s's claim to hold the process of its launch %d", role, launch.Generation), func() bool {
		held := f.claimView(role)
		return held.Generation == launch.Generation && held.Locator != nil
	})
	a := f.agent(f.register(role, session))
	f.surfaces(fmt.Sprintf("the %s registered on its resumed launch, before the look's task is sent", role))
	a.ready()
	f.r.note("%s: the %s's session was resumed and came up ready", f.where, role)
	return a
}

// lateSend is the sequence measured on dispatch://INFRA-4207 (part 1 of c-late-turn) up to the
// late turn: the look's task reaches the agent, which answers its send only after the daemon's
// send deadline, so the daemon records that send failed; the agent answers the task's next send
// busy, and once the daemon has read that answer, no send of the task in flight and no prompt of
// it outstanding, the agent's turn of the first prompt starts. Every later send of the task in
// that turn is answered busy (listen).
func (r *lookIDLive) lateSend(f *lookIDFixture, a *lookIDAgent, task shimwire.Prompt) {
	t := f.t
	t.Helper()
	testwait.WithinPolling(t, lookIDWait, 50*time.Millisecond, "the daemon to record the look's send failed at its deadline", func() bool {
		held := f.claimView(a.role).Pending
		return held != nil && held.SendFailures > 0
	})
	failed := f.claimView(a.role).Pending
	a.ack(task)
	r.note("the look's prompt reached the agent, which answered it only after the daemon's %s send deadline: the daemon recorded the send failed (%q)",
		f.cfg.WorkerRPCTimeout, failed.LastSendError)
	f.sweep(a.role)
	again := a.prompt("the look's task sent again at the runtime's next sweep", checkTaskLeadText)
	mark := f.logMark()
	a.busy(again)
	f.awaitLogged("the daemon to read the busy answer to the task's next send", mark, a.role, "supervise: the agent refused the prompt", map[string]any{"delivery": again.DeliveryID})
	f.surfaces("after a busy answer to the look's task")
	a.begin(task.ID)
	r.note("the daemon sent the task again and the agent answered it busy; the agent's turn of the first prompt then started")
}

// lateTurnReport is the report of parts 1, 2 and 4 of `late turn` and what each part passes on:
// in the late turn the tester reports look 2 pending with a new wake, check_opened_at and the look
// id the look-2 task gave (read). The daemon answers 200; GET /legion/v1/state shows look 2 not
// open with the new wake as nextWakeAt; the claim holds no task of look 2; its prompt failures and
// prompt retirements both read 0; and no text of look 2 reaches the connection after the report,
// for lookIDQuietSends of the daemon's send deadlines.
func (r *lookIDLive) lateTurnReport(f *lookIDFixture, tester *lookIDAgent, read lookRead) {
	t := f.t
	t.Helper()
	wake := time.Now().UTC().Add(45 * time.Minute).Truncate(time.Microsecond)
	reported := time.Now()
	status, failure := tester.report(read.opened, read.id, "pending", &wake)
	r.note("in that turn the tester reported look 2 pending with wake %s, check_opened_at %s and the look id the look-2 task gave: %d %s %s",
		wake.Format(time.RFC3339Nano), read.opened.UTC().Format(time.RFC3339Nano), status, failure.Code, failure.Error)
	if status != http.StatusOK {
		r.fails("report", "the report of look 2 = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	f.surfaces("after the report of look 2")
	tester.finish()
	var shown *api.CheckView
	if !testwait.Holds(lookIDWait, 100*time.Millisecond, func() bool {
		shown = f.issue(f.key).Check
		return shown != nil && shown.Looks == 2 && !shown.LookOpen && shown.NextWakeAt.Equal(wake)
	}) {
		r.fails("look-closed", "GET /legion/v1/state shows the check %+v, want look 2 not open with nextWakeAt %s", shown, wake.Format(time.RFC3339Nano))
	}
	r.note("GET /legion/v1/state: look 2 not open, nextWakeAt %s", shown.NextWakeAt.UTC().Format(time.RFC3339Nano))
	var held api.OperatorClaim
	if !testwait.Holds(lookIDWait, 100*time.Millisecond, func() bool {
		held = f.claimView(claim.RoleTester)
		return held.Pending == nil || !lookTask(held.Pending.Task, 2)
	}) {
		r.fails("task-retired", "the tester's claim still holds %s, want no task of look 2", describeHeld(held.Pending))
	}
	if !testwait.Holds(lookIDWait, 100*time.Millisecond, func() bool {
		held = f.claimView(claim.RoleTester)
		return held.Budgets.PromptFailures == 0 && held.Budgets.PromptRetires == 0
	}) {
		r.fails("counts", "the tester's claim reads %d prompt failures and %d prompt retirements, want 0 and 0", held.Budgets.PromptFailures, held.Budgets.PromptRetires)
	}
	r.note("the tester's claim is %s holding %s, with 0 prompt failures and 0 prompt retirements", held.State, describeHeld(held.Pending))
	quiet := lookIDQuietSends * f.cfg.WorkerRPCTimeout
	time.Sleep(quiet)
	for _, got := range tester.since(reported) {
		if lookTask(got.prompt.Message, 2) {
			r.fails("silent", "the task of look 2 reached the connection %s after the report (answered busy: %t): %s",
				got.at.Sub(reported).Round(time.Millisecond), got.busy, firstLine(got.prompt.Message))
		}
	}
	r.note("no text of look 2 reached the connection in the %s after the report", quiet)
}

// seedImplementerCheck records, before any boot, the tree of part 3 of `late turn` as a daemon of
// the release before the rule that the tester checks production left it: the root (seedRoot), the
// child done on its merged pull request, its production check of the implementer open at look 1
// with that look's start row, bound row and the check's bound row, and the child's implementer
// suspended by the daemon in its own session, having served run 1 and no look. The start row's
// task gives check_opened_at and no look id, as that release's did. It answers the check's
// opening time.
func (f *lookIDFixture) seedImplementerCheck() time.Time {
	t := f.t
	t.Helper()
	ctx := context.Background()
	opened := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	bound := opened.Add(f.cfg.ProductionCheckBound)
	f.board.show(f.root, "in_progress")
	f.board.place(f.key, "done", f.root, false)
	stamp := opened.Format(time.RFC3339Nano)
	task := fmt.Sprintf("%s%s: %s.\nRepository %s, pull request #%d, merge commit %s. Look 1, woken by the merge. The check opened at `%s`: your report names it with check_opened_at `%s`.\nThen report with the legion tool's handoff_complete, check_opened_at from this task and a verdict.",
		checkTaskLeadText, f.key, f.title, liveRepo, f.number, lookIDMerge, stamp, stamp)
	rows := []record.OutboxRow{
		mustOutboxRow(t, f.key, record.SuperviseRequest{Op: "start", Tree: f.root, Role: claim.RoleImplementer, Generation: 1,
			CheckLook: 1, CheckOpenedAt: opened, Task: task}, opened),
		mustOutboxRow(t, f.key, record.CheckLook{Look: 1, OpenedAt: opened}, opened.Add(2*time.Hour)),
		mustOutboxRow(t, f.key, record.CheckBound{}, bound),
	}
	f.write("the tree, the child done on its merged pull request, and the implementer's check", func(tx pgx.Tx) error {
		if err := f.seedRoot(ctx, tx); err != nil {
			return err
		}
		if err := f.records.PutIssue(ctx, tx, f.child(phase.Done, "done")); err != nil {
			return err
		}
		if err := f.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestMerged, Issue: f.key, Repo: liveRepo, Number: f.number, Branch: claim.IssueBranch(f.key),
			HeadSHA: lookIDHead, MergeSHA: lookIDMerge, Base: lookIDBranch, Failing: []string{},
		}); err != nil {
			return err
		}
		if err := f.records.PutProductionCheck(ctx, tx, record.ProductionCheck{
			Issue: f.key, Project: f.cfg.Project, Tree: f.root, Repo: liveRepo, Number: f.number, MergeSHA: lookIDMerge,
			OpenedAt: opened, BoundAt: bound, Role: claim.RoleImplementer, Looks: 1, LookOpen: true, LookWakeAt: &opened,
			LastWakeKind: "merge", LastWakeDetail: lookIDMerge, LastWakeAt: &opened,
		}); err != nil {
			return err
		}
		for _, row := range rows {
			if err := f.records.Enqueue(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	})
	f.openLifecycle()
	implementer := f.token(claim.RoleImplementer)
	if _, err := f.st.AdmitClaim(ctx, supervise.Claim{
		Token: implementer, Project: f.project, Tree: f.root, Issue: f.key, Role: claim.RoleImplementer, Generation: 1,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon, Session: "ses_implementer",
		SessionFile: "/sessions/ses_implementer.jsonl", ServingGeneration: 1,
	}); err != nil {
		t.Fatalf("record the implementer's suspended claim: %v", err)
	}
	writePrompt(t, f.cfg, implementer)
	return opened
}

// ---- step `testing turn behind the close` ---------------------------------------------------------

// testingTurnBehindTheClose is c-no-id, with departure 2's setup: the tester's testing turn has
// ended and the tester holds its testing phase when the child closes merged; the daemon sends the
// look-1 task, and the tester's testing turn starts while that send is in flight, which the agent
// answers busy, as Oh My Pi does. From that turn the tester reads check.openedAt with read_record
// and sends a pass three times — with that time and no look id, with that time and a look id of
// 26 base32 characters the driver made up, and with no checkOpenedAt — and each is refused and
// records nothing: the first two answers name look 1 as the current look, say that a report gives
// the look id its look's task gives, and carry a code other than CHECK_LOOK_NOT_SERVED; no answer
// says `works no look`; the store holds the check open at look 1 with no report on it. The testing
// turn then ends, the look-1 task reaches the connection, and a report with check_opened_at and
// the look id from that task is answered 200. At the base commit the first call is answered 409
// CHECK_LOOK_NOT_SERVED, and the step ends there (no-id-refusal).
func (r *lookIDLive) testingTurnBehindTheClose() {
	f := r.fixture("testing turn behind the close", nil)
	f.seedImplementing()
	f.boot()
	tester := f.toTesting()
	tester.finish()
	tester.awaitIdle("the tester's testing turn to end and its task to retire")
	r.note("the tester's turn of its testing task ended; the tester holds the testing phase, idle")
	f.closeMerged()
	first := tester.prompt("the look-1 task", checkTaskLeadText)
	tester.begin("")
	mark := f.logMark()
	tester.busy(first)
	f.awaitLogged("the daemon to read the busy answer to the look-1 send", mark, claim.RoleTester, "supervise: the agent refused the prompt", map[string]any{"delivery": first.DeliveryID})
	r.note("the daemon sent the look-1 task; the tester's testing turn started while that send was in flight, and the agent answered it busy")
	f.surfaces("after a busy answer to look 1's task, in the testing turn")
	opened := f.readRecordOpenedAt()
	before := f.check()
	r.note("read_record (GET /legion/v1/issues/%s/record) gives check.openedAt %s", f.key, opened.UTC().Format(time.RFC3339Nano))

	status, failure := tester.report(opened, "", "pass", nil)
	r.note("a pass with check_opened_at %s and no look id: %d %s %q", opened.UTC().Format(time.RFC3339Nano), status, failure.Code, failure.Error)
	r.refusedNamingLookOne("no-id-refusal", status, failure)
	madeUp := rand.Text()
	status, failure = tester.report(opened, madeUp, "pass", nil)
	r.note("a pass with check_opened_at %s and a look id of %d base32 characters the driver made up: %d %s %q", opened.UTC().Format(time.RFC3339Nano), len(madeUp), status, failure.Code, failure.Error)
	r.refusedNamingLookOne("made-up-id-refusal", status, failure)
	status, failure = tester.report(time.Time{}, "", "pass", nil)
	r.note("a pass with no check_opened_at: %d %s %q", status, failure.Code, failure.Error)
	if status == http.StatusOK || strings.Contains(failure.Error, "works no look") {
		r.fails("no-check-refusal", "the pass with no checkOpenedAt = %d %s %q, want a refusal that does not say \"works no look\"", status, failure.Code, failure.Error)
	}
	after := f.check()
	if !after.Open() || after.Looks != 1 || !after.LookOpen || after.Outcome != "" || f.reports() != 0 || !reflect.DeepEqual(after, before) {
		r.fails("check-untouched", "after the three calls the check is %+v with %d reports processed, want it as before them, open at look 1 with no report on it: %+v", after, f.reports(), before)
	}
	r.note("the store holds the check open at look 1, the look open, with no report on it")

	ended := time.Now()
	tester.finish()
	task := tester.prompt("the look-1 task after the testing turn", checkTaskLeadText)
	r.note("the testing turn ended, and the look-1 task reached the connection %s after it", time.Since(ended).Round(time.Millisecond))
	read := tester.readLook(task)
	tester.ack(task)
	tester.begin(task.ID)
	status, failure = tester.report(read.opened, read.id, "pass", nil)
	r.note("in the look-1 task's turn the tester reported pass with check_opened_at %s and the look id from that task: %d %s %s",
		read.opened.UTC().Format(time.RFC3339Nano), status, failure.Code, failure.Error)
	if status != http.StatusOK {
		r.fails("report", "the report of look 1 from its task's turn = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	f.surfaces("after the report of look 1")
	tester.finish()
	closed := f.awaitCheck("the pass to close the check", func(c record.ProductionCheck) bool { return !c.Open() })
	r.note("the check closed as %s", closed.Outcome)
}

// readRecordOpenedAt is check.openedAt as read_record reads it (GET /legion/v1/issues/<child>/
// record), read as raw JSON.
func (f *lookIDFixture) readRecordOpenedAt() time.Time {
	f.t.Helper()
	status, body := f.d.request(http.MethodGet, "/legion/v1/issues/"+f.key+"/record", nil, false)
	if status != http.StatusOK {
		f.t.Fatalf("GET the record of %s = %d %s, want 200", f.key, status, body)
	}
	f.r.seen.read(f.where+": read_record in the testing turn", "GET /legion/v1/issues/"+f.key+"/record", body)
	var issue struct {
		Check *struct {
			OpenedAt time.Time `json:"openedAt"`
		} `json:"check"`
	}
	if err := json.Unmarshal(body, &issue); err != nil {
		f.t.Fatalf("decode the record of %s: %v", f.key, err)
	}
	if issue.Check == nil || issue.Check.OpenedAt.IsZero() {
		f.t.Fatalf("the record of %s shows no check.openedAt: %s", f.key, body)
	}
	return issue.Check.OpenedAt
}

// refusedNamingLookOne fails the step at assertion unless the answer of status and failure is a
// refusal that names look 1 as the current look, says that a report gives the look id its look's
// task gives, carries a code other than CHECK_LOOK_NOT_SERVED, and does not say `works no look`.
func (r *lookIDLive) refusedNamingLookOne(assertion string, status int, failure api.Failure) {
	r.t.Helper()
	switch {
	case status == http.StatusOK:
		r.fails(assertion, "the call was answered 200; want a refusal")
	case failure.Code == "CHECK_LOOK_NOT_SERVED":
		r.fails(assertion, "the call was answered %d %s %q; want a code other than CHECK_LOOK_NOT_SERVED", status, failure.Code, failure.Error)
	case strings.Contains(failure.Error, "works no look"):
		r.fails(assertion, "the answer %q says \"works no look\"", failure.Error)
	case !strings.Contains(failure.Error, "The current look of the check is look 1."):
		r.fails(assertion, "the answer %q does not name look 1 as the current look", failure.Error)
	case !strings.Contains(failure.Error, "look_id from the task of its look"):
		r.fails(assertion, "the answer %q does not say that a report gives the look id its look's task gives", failure.Error)
	}
}

// ---- step `ended look` ---------------------------------------------------------------------------

// endedLook is c-ended-look, in two parts on one check. Part 1: the look-1 prompt reaches the agent
// and its turn runs with the turn match failing, as in part 1 of `late turn` (lateSend); while
// that turn runs the driver brings the bound of look 1 forward (SQL on the next_at of its
// check_look row), so look 1 ends; before look 2 starts, a report from that turn with
// check_opened_at, look 1's id, pending and a wake is answered 200 and recorded against look 1.
// Part 2: look 2 has started, and a report with check_opened_at and look 1's id is answered 409
// CHECK_LOOK_NOT_OPEN, naming look 2 as the current look, with the check as it was before the call;
// the same report with look 2's id, sent straight after, is answered 200, so that refusal comes
// from the id of the replaced look. At the base commit the report of part 1 is answered 409
// CHECK_LOOK_NOT_SERVED, and the step ends there (part 1 report).
func (r *lookIDLive) endedLook() {
	f := r.fixture("ended look", nil)
	f.seedImplementing()
	f.boot()
	tester := f.toTesting()
	tester.finish()
	tester.awaitIdle("the tester's testing turn to end and its task to retire")
	f.closeMerged()
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	r.lateSend(f, tester, task)
	read := tester.readLook(task)
	f.exec("bring look 1's bound forward", "update outbox set next_at = now() where issue = $1 and kind = $2", f.key, string(record.OutboxKindCheckLook))
	ended := f.awaitCheck("look 1's bound to end it", func(c record.ProductionCheck) bool { return c.Looks == 1 && !c.LookOpen })
	r.note("part 1: while the late turn runs, look 1's bound came due and ended it; the check is at look 1, the look closed, its next wake at %v", ended.WakeAt)
	wake := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
	status, failure := tester.report(read.opened, read.id, "pending", &wake)
	r.note("part 1: from that turn, a pending with wake %s, check_opened_at %s and look 1's id: %d %s %s",
		wake.Format(time.RFC3339Nano), read.opened.UTC().Format(time.RFC3339Nano), status, failure.Code, failure.Error)
	if status != http.StatusOK {
		r.fails("part 1 report", "the report of look 1, which its bound ended, = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	recorded := f.check()
	if f.lookReports(1, "pending") != 1 || recorded.Looks != 1 || recorded.LookOpen || recorded.WakeAt == nil || !recorded.WakeAt.Equal(wake) {
		r.fails("part 1 recorded", "after the report the check is %+v with %d pending reports of look 1, want the report recorded against look 1 with its wake %s",
			recorded, f.lookReports(1, "pending"), wake.Format(time.RFC3339Nano))
	}
	r.note("part 1: the report is recorded against look 1: the intake took it as look 1's pending, and the check names its wake %s", recorded.WakeAt.UTC().Format(time.RFC3339Nano))
	f.surfaces("after the report of look 1, which its bound had ended")
	tester.finish()
	f.awaitSuspended(claim.RoleTester, "the pending report to suspend the tester")

	// The suspension stopped the pane the tester's agent played, so that pane's connection closes,
	// and the stream takes the resumed pane's hello once the listener has read the close.
	_ = tester.sh.conn.Close()
	launched := f.launches(claim.RoleTester)
	f.live = slices.DeleteFunc(f.live, func(a *lookIDAgent) bool { return a == tester })
	f.exec("bring the wake of look 2 forward", "update outbox set next_at = now() where issue = $1 and kind = $2", f.key, string(record.OutboxKindCheckWake))
	f.awaitCheck("the wake to start look 2", func(c record.ProductionCheck) bool { return c.Looks == 2 && c.LookOpen })
	f.surfaces("after look 2 starts")
	second := f.resume(claim.RoleTester, "ses_tester", launched, "look 2's start to resume the tester")
	next := second.prompt("the look-2 task", checkTaskLeadText)
	second.ack(next)
	second.begin(next.ID)
	nextRead := second.readLook(next)
	before := f.check()
	status, failure = second.report(read.opened, read.id, "pass", nil)
	r.note("part 2: look 2 started; a pass with check_opened_at %s and look 1's id: %d %s %q", read.opened.UTC().Format(time.RFC3339Nano), status, failure.Code, failure.Error)
	if status != http.StatusConflict || failure.Code != "CHECK_LOOK_NOT_OPEN" || !strings.Contains(failure.Error, "The current look of the check is look 2.") {
		r.fails("part 2 refusal", "the report with look 1's id = %d %s %q, want 409 CHECK_LOOK_NOT_OPEN naming look 2 as the current look", status, failure.Code, failure.Error)
	}
	if after := f.check(); !reflect.DeepEqual(after, before) {
		r.fails("part 2 check-unchanged", "the refused report changed the check from %+v to %+v", before, after)
	}
	r.note("part 2: the check is the same as before that call")
	status, failure = second.report(nextRead.opened, nextRead.id, "pass", nil)
	r.note("part 2: the same pass with look 2's id, sent straight after: %d %s %s", status, failure.Code, failure.Error)
	if status != http.StatusOK {
		r.fails("part 2 control", "the report with look 2's id = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	f.surfaces("after the report of look 2")
	second.finish()
	closed := f.awaitCheck("the pass to close the check", func(c record.ProductionCheck) bool { return !c.Open() })
	r.note("the check closed as %s", closed.Outcome)
}

// lookReports is how many reports of look n with verdict the intake has processed for the child's
// check: the completion route names each event for the check, the look and the verdict (api's
// checkReport, `check:<issue>:<check>:<look>:<verdict>`).
func (f *lookIDFixture) lookReports(n int, verdict string) int {
	f.t.Helper()
	var count int
	if err := f.st.Pool().QueryRow(context.Background(),
		"select count(*) from processed_events where source = 'api' and event_id like $1",
		fmt.Sprintf("check:%s:%%:%d:%s", f.key, n, verdict)).Scan(&count); err != nil {
		f.t.Fatalf("count the reports of look %d of %s: %v", n, f.key, err)
	}
	return count
}

// ---- step `id hidden` ----------------------------------------------------------------------------

// idHidden is c-id-hidden, at the end of the run: every look id a task that reached a role's
// connection gave in the run (lookIDSeen) is searched for in every body the steps read of GET
// /legion/v1/state and of the record route read_record calls — each read before a look's task is
// sent, after a busy answer to it, after its report and after the next look starts — and in every
// answer to a report, and in the whole of the daemon's log of the run, which every daemon of the
// run appended to. The step passes when none holds one. Its negative control is its own search run
// over the task texts that reached the connections, which must find each id there, so a search
// that finds nothing is one that finds a look id where it is. At the base commit, whose tasks give
// no look id, there is nothing to search, and the step does not run.
func (r *lookIDLive) idHidden() {
	if r.base {
		r.t.Skipf("the base commit gives no look id, so the step has nothing to search; its negative control is its own search over the task texts, not the base")
	}
	r.seen.mu.Lock()
	ids, texts, reads := slices.Clone(r.seen.ids), slices.Clone(r.seen.texts), slices.Clone(r.seen.reads)
	r.seen.mu.Unlock()
	if len(ids) == 0 {
		r.fails("ids", "no task that reached a role's connection in the run gave a look id")
	}
	where := make([]string, 0, len(ids))
	for _, seen := range ids {
		where = append(where, seen.where)
		if !slices.ContainsFunc(texts, func(text string) bool { return strings.Contains(text, seen.id) }) {
			r.fails("search-blind", "the search finds the look id of %s in none of the %d task texts that reached a connection, the one place it is", seen.where, len(texts))
		}
	}
	r.note("the run saw %d look ids in the tasks that reached the connections (%s), and the search finds each in those %d task texts",
		len(ids), strings.Join(slices.Compact(where), "; "), len(texts))
	carriesOpenedAt := 0
	for _, read := range reads {
		if bytes.Contains(read.body, []byte(`"openedAt"`)) {
			carriesOpenedAt++
		}
		for _, seen := range ids {
			if bytes.Contains(read.body, []byte(seen.id)) {
				r.fails("surface", "%s, read %s, holds the look id of %s", read.route, read.moment, seen.where)
			}
		}
	}
	r.note("none of the %d bodies read (GET /legion/v1/state, GET /legion/v1/issues/<issue>/record and the answers to reports) holds a look id; %d of them carry check.openedAt",
		len(reads), carriesOpenedAt)
	if r.daemonLog == "" {
		r.fails("log", "%s names no file, so there is no daemon log of the run to search", liveDaemonLogEnv)
	}
	logged, err := os.ReadFile(r.daemonLog)
	if err != nil {
		r.fails("log", "read the daemon log of the run %s: %v", r.daemonLog, err)
	}
	for _, seen := range ids {
		if bytes.Contains(logged, []byte(seen.id)) {
			r.fails("log", "the daemon's log of the run (%s) holds the look id of %s", r.daemonLog, seen.where)
		}
	}
	r.note("the daemon's whole log of the run, %d bytes, holds none of them", len(logged))
}
