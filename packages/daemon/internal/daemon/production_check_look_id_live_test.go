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
//   - testing turn behind the close (c-no-id): a tester's testing turn that starts while the
//     first look's task is in flight is no report of that look, whatever it names, and the look's
//     own task reaches the connection after that turn.
//   - ended look (c-ended-look), two parts on one check: a report of a look whose bound ended it
//     counts until the next look starts, and a report of a look the next one replaced records
//     nothing.
//   - task ends with its look (c-task-ends), each part on a daemon and a check of its own: the
//     task of a look the turn match fails for ends with its look, however the look ends — a
//     report of it (part 1), its bound (part 2), an operator's hold that the next look's start
//     meets (part 3), a claim that failed once its prompt failures ran out (part 4), or a later
//     merge whose check replaces it (part 5) — and the next look's task takes its place, also
//     behind an acknowledged prompt of the ended look whose turn starts late (part 6); the check
//     of an issue that runs again stays open at its look and takes the report of that look from
//     the session whose turn ran its task, once that turn has ended — a root reopened inside its
//     tree's linger (part 7) and a child that re-enters (part 8); and the check of an issue that
//     ran again closes as not run when the run's next merge replaces it (part 9) and when the run
//     leaves with no merge (part 10, a child and then a root), with no row of its own for the
//     issue's claims and pod. A last part, which no check numbers, holds that the ready after a
//     fresh session sends nothing of a look that ended.
//   - upgrade (c-upgrade), the ten parts the check names, each on a daemon and a check of its own,
//     part 6 in both of its orders: the store left as a daemon of the release before this change
//     leaves it after the way back, written with the daemon stopped, and the daemon booted on it.
//     What the way back does is the guide's section "Rolling back the release whose look report
//     gives its look id" (docs/kubernetes.md), and which open looks the boot gives a new look id
//     and a new task is workflow.Engine.RepairOpenChecks's, with its lookTaskGivesAnotherID. Each
//     part seeds one look: an open look with no look id (part 1), the last look (part 2), a look
//     whose tester the operator holds (part 3), a look whose check holds the id made for the
//     previous look (part 4), a look that ended at the operator's hold (part 5), a look beside a
//     start row of it that release queued (part 6), a look whose check holds the id made for the
//     check a later merge replaced (part 7), a look whose task gives no check_opened_at (part 8),
//     a look of a root out of the workflow whose tester holds a task that gives the look's kept
//     id in a delivery that holds none (part 9), and a look of a root that runs again whose
//     tester holds the look's id (part 10). Parts 1 to 9 hold that no text that release left
//     reaches a connection; parts 1 to 4 and 6 to 9, that the look gets a new id and its new task
//     reaches the tester once it registers on this change's contract; part 5, that the next
//     look's task does; part 10, that no task of a look reaches the tester and the look keeps its
//     id; and each, that a report with the look id the look's task gave the tester is taken.
//   - id hidden (c-id-hidden): the last step. It searches every look id the run saw in the routes
//     a role reads and in the daemon's whole log. Every step feeds it: each prompt a role's agent
//     reads goes through lookIDAgent, which records the look id of each task it reads
//     (lookIDSeen.task), and a step reads the routes a role reads at each moment its check names
//     through lookIDFixture.surfaces.
//
// Each step, and each part of `late turn`, `task ends with its look` and `upgrade`, runs as a
// subtest of its own on a daemon of its own, booted as this package's other whole-daemon proofs
// boot it (startDaemonLogging over the fake runtime, liveIssueProof of live_issue_proof_test.go),
// so one that fails ends there and the next still runs: the negative control's one run shows every
// step's own failure, each at the assertion the step names (lookIDLive.fails), as
// busy_relaunch_live_test.go's steps do. Each fixture is a tree of the run's own project: a root
// in accepting and its child, the issue whose production check a part drives, as
// api/handoff_check_route_test.go's fixture holds them, or, for a part whose checked issue is a
// root, that root alone (rootFixture). The checked issue reaches testing as production's does —
// its implementer, a launch the previous daemon never recorded that the boot relaunches, takes
// the implementing task the daemon sends it and completes over HTTP in that task's turn, and its
// tester takes the testing task over the worker stream — and closes merged as that fixture closes
// it, since a merge during testing closes nothing: GitHub's merge of its pull request arrives
// while it is in testing, and a person then moves it to done on Dispatch, which opens the check
// and starts its first look on the tester's claim, which a child's close spares and a root's
// linger suspends and the look resumes.
//
// The checking role's agent is a raw shim (lookIDAgent over liveClaimant): it reads every frame
// the daemon sends on a goroutine of its own, answers the protocol negotiation and the state probe
// (get_state) as the real shim and Oh My Pi do, answers busy (the pinned Oh My Pi's session_busy)
// each prompt that arrives in a turn, and hands every other prompt to the step, which answers it,
// late, busy or not at all. The implementer that walks the child to testing is liveClaimant,
// which takes the implementing task the daemon sends it at its ready (takeTask), and the step
// records that task too (lookIDSeen.task); the implementer of a run after a re-entry is a
// lookIDAgent, so every task that reaches a connection is recorded. The daemon's own send
// deadline is the configuration's (Config.WorkerRPCTimeout), and its wait for a turn is the
// machines' clock: still (fakeRuntime's stillClock) everywhere but part 4 of `late turn`, which
// boots on the wall clock (overrides.clock nil). The runtime's sweep of the claims' processes,
// which sends again a task that waits after a failed send or a turn that never came, is the
// driver's (lookIDFixture.sweep), since the fake runtime makes none. A look's bound and a look's
// wake are brought forward by SQL on outbox.next_at, and every seed a part writes with the
// daemon stopped (lookIDFixture.restartWith) is written so. Each report is the raw JSON body the
// plugin's `handoff_complete` sends (checkOpenedAt, lookId, verdict, summary, wakeAt). An issue
// that runs again gets its branch from GitHub's stand-in (lookIDGitHub) and its next pull request
// from GitHub's events; the rows a person's move queues are read from the database's own record
// of every outbox write (recordOutboxWrites), since the outbox deletes a row once it has run.
//
// The driver names no Go symbol the base commit lacks, so it builds over it: it reads the routes
// as raw JSON, takes the look id from the task's text, and reads and writes each column the change
// adds by SQL alone. Under PRODUCTION_CHECK_LOOK_ID_BASE=1 it sends no lookId, reads no look id,
// reads and writes no column the change adds, and runs only the parts each check's negative control
// names (lookIDLive.base).
package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	planwords "github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store/migrations"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// lookIDBaseEnv is set to 1 by the script's `--base-commit` run: the driver then runs over the
// base commit, whose daemon refuses a request member it does not know (lookId) and whose task
// gives no look id.
const lookIDBaseEnv = "PRODUCTION_CHECK_LOOK_ID_BASE"

// The run's fixed values: the script that owns the broker, the base branch of every pull request,
// the head the checked issue's pull request is at and merges at, the merge commit GitHub's merge
// names (publishMergeOf names it for the head), and the head of the next pull request a run of the
// issue after its re-entry or re-admission opens and merges.
const (
	lookIDScript = "scripts/e2e/production-check-look-id.sh"
	lookIDBranch = "main"
	lookIDHead   = "c0ffee0000000000000000000000000000000001"
	lookIDMerge  = "merge-" + lookIDHead
	lookIDHead2  = "c0ffee0000000000000000000000000000000002"
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
// order of the Verification document. `id hidden` stays last, since it searches every look id the
// steps before it saw.
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
	r.run("task ends with its look", r.taskEndsWithItsLook)
	r.run("upgrade", r.upgrade)
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
	r.partNamed(fmt.Sprintf("part %d", n), what, body)
}

// partNamed is part under label, for the one part no check numbers (fresh session).
func (r *lookIDLive) partNamed(label, what string, body func()) {
	step := r.t
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
		if !t.Failed() {
			fmt.Printf("ok %s (%ds)\n", label, int(time.Since(started).Seconds()))
		}
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
// whose key is the proof's issue and whose pull request is the proof's, under a root of its own,
// or for that root itself (rootFixture); each issue as Dispatch's board holds it, which a person's
// move publishes (move); the daemon's own log kept in memory as it is written; what each boot is
// tuned with; and the agents whose shims a restart reconnects.
type lookIDFixture struct {
	liveIssueProof
	r *lookIDLive
	// where names the fixture in the record of the step `id hidden` and in its notes: the step
	// and the part.
	where string
	root  string
	// issues is each issue of the fixture as Dispatch's board holds it, by key.
	issues map[string]*lookIDIssue
	// logged is the running daemon's own log, kept in memory as it is written.
	logged *recordingLog
	tune   func(*overrides)
	live   []*lookIDAgent
	// implementer is the implementer that walked the checked issue to testing (toTesting), whose
	// pane the close stops (closeMerged).
	implementer *liveClaimant
}

// lookIDIssue is one issue as Dispatch holds it and its events name it: its parent ("" for a
// root), title and rank as the fixture seeded them, whether it is handed to Legion, and the
// sequence number of its latest event, which each move raises.
type lookIDIssue struct {
	parent, title, rank string
	handed              bool
	seq                 int
}

// fixture readies a daemon of the run's own over a fresh project, its tree not yet seeded and the
// daemon not yet booted: the child is the proof's issue, under `production_check: tester`, with a
// check bound of two weeks, the shipped default. tune, when not nil, sets what the part's boots
// need beyond the fixture's own.
func (r *lookIDLive) fixture(where string, tune func(*overrides)) *lookIDFixture {
	r.t.Helper()
	return r.fixtureOf(where, false, tune)
}

// rootFixture is fixture for a part whose checked issue is a root: the proof's issue is the root
// of its own tree, which holds no other issue, and seedRootImplementing seeds it.
func (r *lookIDLive) rootFixture(where string) *lookIDFixture {
	r.t.Helper()
	return r.fixtureOf(where, true, nil)
}

// fixtureOf is fixture, with the root as the proof's issue where rootChecked is set. GitHub's
// stand-in serves the proof's pull request (liveGitHub) and the read and the write that make an
// issue's branch at the start of a new run (lookIDGitHub).
func (r *lookIDLive) fixtureOf(where string, rootChecked bool, tune func(*overrides)) *lookIDFixture {
	r.t.Helper()
	p := newLiveIssueProof(r.t, lookIDScript, lookIDBranch, "the child whose production check a report names by its look id")
	f := &lookIDFixture{liveIssueProof: p, r: r, where: where, root: p.cfg.Project + "-1", tune: tune}
	if rootChecked {
		f.issues = map[string]*lookIDIssue{f.root: {title: f.title, rank: "U", handed: true, seq: 1}}
	} else {
		f.key = p.cfg.Project + "-2"
		f.issues = map[string]*lookIDIssue{
			f.root: {title: "the tree the checked child merges in", rank: "M", handed: true, seq: 1},
			f.key:  {parent: f.root, title: f.title, rank: "N", seq: 1},
		}
	}
	f.cfg.Projects = map[string]config.Project{f.cfg.Project: {ProductionCheck: claim.RoleTester}}
	f.cfg.ProductionCheckBound = 336 * time.Hour
	// A root's close lingers its tree for the shipped 72 hours, inside which a person's todo
	// re-admits the root (the package's test configuration lingers none).
	f.cfg.Linger = 72 * time.Hour
	g := &liveGitHub{sha: lookIDHead, also: map[string]any{
		"/git/ref/heads/" + lookIDBranch: map[string]any{"ref": "refs/heads/" + lookIDBranch, "object": map[string]string{"sha": "base-0"}},
	}}
	g.t, g.key, g.number, g.base = f.t, f.key, f.number, f.base
	g.url = githubtest.Server(f.t, githubtest.Spec{Handle: lookIDGitHub{g}.ServeHTTP})
	f.github = g
	board := httptest.NewServer(lookIDDispatch{f.board})
	r.t.Cleanup(board.Close)
	f.cfg.DispatchURL = board.URL
	// Registered before the boot, so it runs after the daemon's stop.
	r.t.Cleanup(f.leaveStreamEmpty)
	return f
}

// lookIDDispatch is the fixture's Dispatch: liveDispatch, which also answers the read of a
// document's reviews as it answers the read of its changes, as Dispatch answers for a document it
// does not hold (404 ARTIFACT_NOT_FOUND): the board keeps no document. The design news of the
// root's spec gate, approved (seedRoot), reads the reviews of that document at the minute's tick,
// which the daemon then records unreadable at once.
type lookIDDispatch struct {
	*liveDispatch
}

func (b lookIDDispatch) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/v1/artifacts/") && strings.HasSuffix(req.URL.Path, "/reviews") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"`+dispatch.ArtifactNotFound+`","error":"artifact not found"}`)
		return
	}
	b.liveDispatch.ServeHTTP(w, req)
}

// lookIDGitHub is the fixture's GitHub: liveGitHub, which also answers the read of the default
// branch's head the create of an issue's branch makes (its also), and the create itself, as
// GitHub answers a new ref (ghbranch.Create). A run that starts again after a re-entry or a
// re-admission makes its branch before any start of its roles, which waits for it.
type lookIDGitHub struct {
	*liveGitHub
}

func (g lookIDGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodPost && req.URL.Path == "/repos/"+liveRepo+"/git/refs" {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
		return
	}
	g.liveGitHub.ServeHTTP(w, req)
}

// serve makes GitHub's stand-in answer number, key's pull request on its branch at head, open,
// beside the one it answers already: the pull request a new run of key opens.
func (f *lookIDFixture) serve(key string, number int, head string) {
	f.github.mu.Lock()
	defer f.github.mu.Unlock()
	path := fmt.Sprintf("/pulls/%d", number)
	f.github.also[path] = map[string]any{
		"number": number, "state": "open", "mergeable": true,
		"base": map[string]string{"ref": lookIDBranch}, "head": map[string]string{"ref": claim.IssueBranch(key), "sha": head},
	}
	f.github.also[path+"/reviews"] = []any{}
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
// and on Dispatch's board. A root in accepting had its spec approved before its children ran, so
// its design gate stands open (openSpecGate): a child set back to todo under it waits at admitted
// for that gate and for a slot alone before its new run starts (record.Waiting).
func (f *lookIDFixture) seedRoot(ctx context.Context, tx pgx.Tx) error {
	if err := f.records.PutIssue(ctx, tx, record.Issue{
		Key: f.root, Repo: liveRepo, DefaultBranch: lookIDBranch, GitHubRepo: liveRepo, Tree: f.root, Project: f.cfg.Project,
		Title: "the tree the checked child merges in", Phase: phase.Accepting, Generation: 1, Status: "in_progress",
		DispatchStatus: "in_progress", Rank: "M", LastDispatchSeq: 1, HandedOver: true,
	}); err != nil {
		return err
	}
	if err := f.records.PutGate(ctx, tx, openSpecGate(f.root, 1)); err != nil {
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

// seedRootImplementing records a root fixture's tree, the root alone, as liveIssueProof's
// seedImplementing records an issue the moment its implementer began work: implementing on an
// open pull request at lookIDHead, holding its admission slot, its lifecycle open, and its
// implementer's claim a launch the boot relaunches.
func (f *lookIDFixture) seedRootImplementing() {
	f.t.Helper()
	f.liveIssueProof.seedImplementing(lookIDHead)
}

// seedTwinRoot records twin, the root of a tree of its own, as seedRootImplementing records the
// root a part checks, with its open pull request numbered number and no claim yet: the twin with
// no open check whose move a part compares (sameRowsAsTheTwin). Its first run leaves with no merge
// (reopenTwinRoot), so no check opens for it.
func (f *lookIDFixture) seedTwinRoot(twin string, number int) {
	f.t.Helper()
	ctx := context.Background()
	f.issues[twin] = &lookIDIssue{title: "the twin root, whose first run leaves with no merge", rank: "W", handed: true, seq: 1}
	f.board.show(twin, "in_progress")
	f.write("the twin root, its pull request and its slot", func(tx pgx.Tx) error {
		if err := f.records.PutIssue(ctx, tx, record.Issue{
			Key: twin, Repo: liveRepo, DefaultBranch: lookIDBranch, GitHubRepo: liveRepo, Tree: twin, Project: f.cfg.Project,
			Title: f.issues[twin].title, Phase: phase.Implementing, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "W", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := f.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: twin, Repo: liveRepo, Number: number, Branch: claim.IssueBranch(twin),
			HeadSHA: lookIDHead, Base: lookIDBranch, Failing: []string{},
		}); err != nil {
			return err
		}
		return f.admit(ctx, tx, twin)
	})
	if _, err := f.st.OpenTreeLifecycle(ctx, f.project, twin, treelifecycle.AuthorityWorkflow); err != nil {
		f.t.Fatalf("open the lifecycle of %s: %v", twin, err)
	}
	f.serve(twin, number, lookIDHead)
	f.prompts(twin)
}

// seedTwinChild records twin, a second child of the root, done on no merged pull request, so no
// check opened for it: the twin with no open check whose move a part compares
// (sameRowsAsTheTwin).
func (f *lookIDFixture) seedTwinChild(twin string) {
	f.t.Helper()
	parent := f.root
	f.issues[twin] = &lookIDIssue{parent: f.root, title: "the twin child, done with no check", rank: "O", seq: 1}
	f.board.place(twin, "done", f.root, false)
	f.write("the twin child", func(tx pgx.Tx) error {
		return f.records.PutIssue(context.Background(), tx, record.Issue{
			Key: twin, Repo: liveRepo, DefaultBranch: lookIDBranch, GitHubRepo: liveRepo, Tree: f.root, Parent: &parent,
			Project: f.cfg.Project, Title: f.issues[twin].title, Phase: phase.Done, Generation: 1, Status: "done",
			DispatchStatus: "done", Rank: "O", LastDispatchSeq: 1,
		})
	})
	f.prompts(twin)
}

// runsImplementerThenTester gives the project the default steps `implementer tester`, which the
// first step of a run with no plan of its own takes (workflow's planFirstStep): a child that
// re-enters runs them, so its new run reaches testing at its implementer's completion. A run
// seeded before the boot holds no plan rows and walks today's line, which these steps do not
// change.
func (f *lookIDFixture) runsImplementerThenTester() {
	f.t.Helper()
	steps, err := planwords.ParseSteps("implementer tester")
	if err != nil {
		f.t.Fatalf("read the steps implementer tester: %v", err)
	}
	project := f.cfg.Projects[f.cfg.Project]
	project.DefaultSteps = steps
	f.cfg.Projects[f.cfg.Project] = project
}

// prompts writes the role prompt of every role of key, as the daemon's own launch writes it: a
// run that starts again launches roles the seed never launched.
func (f *lookIDFixture) prompts(key string) {
	f.t.Helper()
	for _, role := range claim.Roles {
		token, err := claim.NewToken(f.project, key, role)
		if err != nil {
			f.t.Fatal(err)
		}
		writePrompt(f.t, f.cfg, token)
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

// logMark is how many records the running daemon has logged: the mark a later read of the log
// starts from (awaitLogged, loggedIndex).
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

// move publishes Dispatch's own issue.updated event of a person moving key to status, under its
// parent as the fixture seeded it (lookIDIssue), as Dispatch publishes one, the board moving with
// it, and answers the event's id, which awaitMoved waits on. Each move of an issue is the next
// event of its sequence.
func (f *lookIDFixture) move(key, status string) string {
	f.t.Helper()
	issue := f.issues[key]
	if issue == nil {
		f.t.Fatalf("move %s, an issue the fixture did not seed", key)
	}
	issue.seq++
	f.board.place(key, status, issue.parent, issue.handed)
	fields := map[string]any{
		"key": key, "status": status, "title": issue.title, "rank": issue.rank, "labels": liveDispatchLabels(issue.handed),
		"last_seq": issue.seq, "project": f.cfg.Project, "resolved_repo": liveRepo,
	}
	if issue.parent != "" {
		fields["parent"] = issue.parent
	}
	payload, err := json.Marshal(map[string]any{
		"id": issue.seq, "issue_key": key, "project": f.cfg.Project, "seq": issue.seq, "type": "issue.updated",
		"actor": map[string]string{"kind": "user", "id": "operator"}, "notify": true, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": fields,
	})
	if err != nil {
		f.t.Fatalf("encode the Dispatch event of %s: %v", key, err)
	}
	return f.publish("notifications.dispatch.issue."+key+".issue.updated", "dispatch", fmt.Sprintf("%s-%s-%d", key, status, issue.seq), string(payload))
}

// awaitMoved waits for the daemon to have applied the Dispatch event eventID (move): the intake
// writes its processed_events row in the transaction that applies it.
func (f *lookIDFixture) awaitMoved(eventID string) {
	f.t.Helper()
	testwait.WithinPolling(f.t, lookIDWait, 50*time.Millisecond, "the daemon to apply the Dispatch event "+eventID, func() bool {
		var applied bool
		if err := f.st.Pool().QueryRow(context.Background(), "select exists (select 1 from processed_events where event_id = $1)", eventID).Scan(&applied); err != nil {
			f.t.Fatalf("read the processed event %s: %v", eventID, err)
		}
		return applied
	})
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
// boot relaunched, comes up ready and takes the implementing task the daemon sends it at that
// ready (liveClaimant.takeTask), as an agent answers each prompt it reads, and in that task's turn
// completes implementing at lookIDHead, naming a handoff record of its own, which hands the
// testing phase to the tester; the turn then ends. A send of that task left unanswered would stay
// in flight while the tester comes up, which holds the tester's testing task (supervise's
// issueInTurn), and that send's end at its deadline is no turn's end (IssueTurnEnded), while the
// fake runtime makes no sweep that would send the held task then. The tester registers, takes the
// testing task and works it. It answers the tester, in the turn of that task.
func (f *lookIDFixture) toTesting() *lookIDAgent {
	t := f.t
	t.Helper()
	imp := f.claimant(claim.RoleImplementer, "ses_implementer")
	// The tester's pin of a race an earlier form of this fixture lost in 3 of 10 runs: once
	// the implementer is ready, the daemon sends it its implementing task, and this pause lets that
	// send start before the turn below, as it can in any run. The fixture must still bring the
	// tester its testing task. The daemon holds the tester's phase task while the implementer's send
	// is in flight (supervise's issueInTurn), and the fake runtime runs no sweep to send it later.
	time.Sleep(time.Second)
	implementing := imp.takeTask("Phase: implementing")
	f.r.seen.task(f.where, implementing.Message, f.r.base)
	status, body := f.d.request(http.MethodPost, "/legion/v1/handoff/complete", api.HandoffCompleteRequest{
		GrantID: f.grant(imp.liveSession), Summary: "implemented it", Commit: lookIDHead,
		HandoffID: imp.handoff("implement"), HandoffVersion: 1,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("the implementer's completion = %d %s, want 200", status, body)
	}
	imp.endTurn()
	f.implementer = imp
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

// closeMerged closes the checked issue merged while it is in testing, as the fixture of
// api/handoff_check_route_test.go closes it: GitHub's merge of its pull request arrives, which the
// daemon records and tells the architect of, since a merge during testing closes nothing, and then
// a person moves the issue to done on Dispatch. The close opens the tester's production check,
// whose first look starts on the tester's claim, which a child's close spares (a root's:
// closeRootMerged). It answers the check open at look 1.
func (f *lookIDFixture) closeMerged() record.ProductionCheck {
	f.t.Helper()
	f.waitProcessed(f.publishMergeOf(f.number, f.key, lookIDHead, lookIDBranch))
	f.move(f.key, "done")
	check := f.awaitCheck("the close to open the production check at look 1", func(c record.ProductionCheck) bool {
		return c.Open() && c.Looks == 1 && c.LookOpen
	})
	if check.Role != claim.RoleTester {
		f.r.fails("check-opened", "the check the close opened is the %s's, want the tester's", check.Role)
	}
	f.r.note("%s: the merge arrived in testing and the person's done closed %s; the check opened at %s for the tester, look 1 open",
		f.where, f.key, check.OpenedAt.UTC().Format(time.RFC3339Nano))
	f.stopImplementer()
	return check
}

// stopImplementer is the end of the pane of the implementer that walked the checked issue to
// testing: the close takes its claim out — a child's close retires it, a root's linger suspends it
// — and the pane it stops closes its connection, as endedLook's tester's does, so a later launch
// of the claim, a new run's implementing start, binds the stream afresh.
func (f *lookIDFixture) stopImplementer() {
	f.t.Helper()
	if f.implementer == nil {
		return
	}
	token := f.token(claim.RoleImplementer)
	testwait.WithinPolling(f.t, lookIDWait, 50*time.Millisecond, "the close to stop the implementer's claim", func() bool {
		state := f.d.claim(token).State
		return state == string(supervise.StateRetired) || state == string(supervise.StateSuspended)
	})
	_ = f.implementer.sh.conn.Close()
	f.implementer = nil
}

// closeRootMerged is closeMerged for a root fixture, whose close lingers its tree: the linger
// suspends every claim of the tree, the tester's, idle, among them (workflow's beginLinger), and
// look 1's start, queued behind those rows, resumes the tester's session. The suspension stopped
// the pane tester played (stopAgent), and the tester comes up in the resumed launch. It answers
// the resumed tester and the check open at look 1.
func (f *lookIDFixture) closeRootMerged(tester *lookIDAgent) (*lookIDAgent, record.ProductionCheck) {
	f.t.Helper()
	launched := f.launches(claim.RoleTester)
	check := f.closeMerged()
	f.relaunched(claim.RoleTester, launched, "look 1's start to resume the tester the linger suspended")
	f.stopAgent(tester)
	return f.resume(claim.RoleTester, "ses_tester", launched, "look 1's start to resume the tester the linger suspended"), check
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
// It dials through dialShimUntilAcked, which the base commit's tree holds too, since the base run
// builds this file over that tree.
func (a *lookIDAgent) reconnect() {
	t := a.f.t
	t.Helper()
	a.f.built.mu.Lock()
	address := a.f.built.address
	a.f.built.mu.Unlock()
	a.sh = dialShimUntilAcked(t, address, lastLaunch(t, a.f.rt, a.token).BootToken)
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

// refuse is Oh My Pi refusing prompt for a cause of its own while its session is in no turn: a
// refusal with no code, which the daemon charges to the claim's prompt failures once the agent's
// answer to its state probe says it streams nothing (supervise's agentRefusalOutcome).
func (a *lookIDAgent) refuse(prompt shimwire.Prompt) {
	a.sh.send(shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: false, Error: "the session could not take the prompt"})
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
	f.wakeComesDue(2)
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
	f.t.Helper()
	return f.comeUp(role, session, "/sessions/"+session+".jsonl", launched, what)
}

// relaunched waits for the daemon to launch role's claim again after the launched launches it had
// made, and answers that launch.
func (f *lookIDFixture) relaunched(role claim.Role, launched int, what string) runtime.SpawnSpec {
	f.t.Helper()
	testwait.WithinPolling(f.t, lookIDWait, 50*time.Millisecond, what, func() bool { return f.launches(role) > launched })
	return lastLaunch(f.t, f.rt, f.token(role))
}

// comeUp is resume of a launch whose session file is resumes: a resume of a session, or "" for a
// spawn in a new session, as a fresh session's start makes, whose agent registers in session.
func (f *lookIDFixture) comeUp(role claim.Role, session, resumes string, launched int, what string) *lookIDAgent {
	t := f.t
	t.Helper()
	launch := f.relaunched(role, launched, what)
	if launch.ResumeSessionFile != resumes {
		t.Fatalf("the daemon launched the %s again with the session file %q, want %q", role, launch.ResumeSessionFile, resumes)
	}
	testwait.WithinPolling(t, lookIDWait, 50*time.Millisecond, fmt.Sprintf("the %s's claim to hold the process of its launch %d", role, launch.Generation), func() bool {
		held := f.claimView(role)
		return held.Generation == launch.Generation && held.Locator != nil
	})
	a := f.agent(f.register(role, session))
	f.surfaces(fmt.Sprintf("the %s registered on its new launch, before the look's task is sent", role))
	a.ready()
	f.r.note("%s: the %s came up ready in session %s", f.where, role, session)
	return a
}

// stopAgent is the end of the pane a's agent played, which the daemon stopped — a suspension, a
// retirement, a failure: its connection closes, as the stopped pane's does, and a restart of the
// daemon no longer reconnects it.
func (f *lookIDFixture) stopAgent(a *lookIDAgent) {
	_ = a.sh.conn.Close()
	f.live = slices.DeleteFunc(f.live, func(live *lookIDAgent) bool { return live == a })
}

// lateSend is the sequence measured in production (part 1 of c-late-turn) up to the
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
	check := record.ProductionCheck{
		Issue: f.key, Project: f.cfg.Project, Tree: f.root, Repo: liveRepo, Number: f.number, MergeSHA: lookIDMerge,
		OpenedAt: opened, BoundAt: bound, Role: claim.RoleImplementer, Looks: 1, LookOpen: true, LookWakeAt: &opened,
		LastWakeKind: "merge", LastWakeDetail: lookIDMerge, LastWakeAt: &opened,
	}
	task := f.olderTask(check, 1, "the merge", false, true)
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
		if err := f.records.PutProductionCheck(ctx, tx, check); err != nil {
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

// testingTurnBehindTheClose is c-no-id: the tester's testing turn has ended and the tester holds
// its testing phase when the child closes merged (closeMerged); the daemon sends the look-1 task,
// and the tester's testing turn starts while that send is in flight, which the agent answers busy,
// as Oh My Pi does. From that turn the tester reads check.openedAt with read_record and sends a
// pass three times — with that time and no look id, with that time and a look id of 26 base32
// characters the driver made up, and with no checkOpenedAt — and each is refused and records
// nothing: the first two answers name look 1 as the current look, say that a report gives the look
// id its look's task gives, and carry a code other than CHECK_LOOK_NOT_SERVED; no answer says
// `works no look`; the store holds the check open at look 1 with no report on it. The testing turn
// then ends, the look-1 task reaches the connection, and a report with check_opened_at and the look
// id from that task is answered 200. At the base commit the first call is answered 409
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
	r.busyTurn(f, tester, first)
	r.note("the daemon sent the look-1 task; the tester's testing turn started while that send was in flight, and the agent answered it busy")
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
	seen := f.recordCheck("read_record in the testing turn")
	if seen.Check == nil || seen.Check.OpenedAt.IsZero() {
		f.t.Fatalf("the record of %s shows no check.openedAt: %+v", f.key, seen)
	}
	return seen.Check.OpenedAt
}

// lookIDRecord is what a step reads of the record route read_record calls (GET
// /legion/v1/issues/<issue>/record), as raw JSON: the issue's phase and its check, present while
// the check is open (api.CheckView).
type lookIDRecord struct {
	Phase phase.Phase `json:"phase"`
	Check *struct {
		OpenedAt time.Time `json:"openedAt"`
		Looks    int       `json:"looks"`
	} `json:"check"`
}

// recordCheck reads the checked issue's record as read_record reads it, and keeps the body for
// the step `id hidden`, under moment.
func (f *lookIDFixture) recordCheck(moment string) lookIDRecord {
	f.t.Helper()
	route := "/legion/v1/issues/" + f.key + "/record"
	status, body := f.d.request(http.MethodGet, route, nil, false)
	if status != http.StatusOK {
		f.t.Fatalf("GET the record of %s = %d %s, want 200", f.key, status, body)
	}
	f.r.seen.read(f.where+": "+moment, "GET "+route, body)
	var seen lookIDRecord
	if err := json.Unmarshal(body, &seen); err != nil {
		f.t.Fatalf("decode the record of %s: %v", f.key, err)
	}
	return seen
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
	f, tester, _ := r.firstLook("ended look", nil)
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	r.lateSend(f, tester, task)
	read := tester.readLook(task)
	ended := f.boundComesDue()
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
	f.stopAgent(tester)
	launched := f.launches(claim.RoleTester)
	f.wakeComesDue(2)
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

// ---- step `task ends with its look` --------------------------------------------------------------

// taskEndsWithItsLook is c-task-ends, each part on a daemon and a check of its own. Parts 1 to 5
// send look 1's task to the tester, the turn match failing for it, and end look 1 one of five
// ways; each passes when no text of the ended look reaches the tester's connection after the look
// ended, and the task of the next look, with its own number and look id, reaches the connection
// and is the claim's pending task. Part 6 is the next look's task taking the place of an
// acknowledged prompt of the ended look, whose turn starts late. Parts 7 and 8 run the checked
// issue again with its check open — a root reopened inside its tree's linger, a child that
// re-enters under its live tree — and take the report of the look from the session whose turn
// ran its task, once that turn has ended, so the report's own retirement of the look's task
// cannot hide a fence that failed; part 8 then takes the new run to testing and to its next
// merge. Parts 9 and 10 close such a check as not run, at the new run's next merge and at its
// leave with no merge, with no issue close or suspension of the close's own: the move queues the
// rows the same move of a twin with no open check queues. A last part, which no check numbers, is
// the ready after a fresh session.
//
// At the base commit the parts the check's negative control names run: parts 2, 3 and 4 fail at
// next-look, the start row of the next look waiting on `a delivery is already pending` behind the
// ended look's task, which the daemon sends again; part 8 at report, 409 CHECK_NOT_OPEN, since the
// re-entry closed the check; part 9 at close-line, the new check written over the old one; and
// the halves of part 10 at child closed-at-move and root closed-at-move.
func (r *lookIDLive) taskEndsWithItsLook() {
	if r.base {
		r.note("parts 1, 5, 6 and 7 and the ready after a fresh session do not run at the base commit: the check's negative control names no answer of them there")
	} else {
		r.part(1, "look 1 ends by its report, made in the late turn of its task", r.endsByReport)
	}
	r.part(2, "look 1 ends by its bound, brought forward while a turn the daemon took for no task's runs", r.endsByBound)
	r.part(3, "the operator suspends the tester, look 1 ends by its bound, look 2's start meets the operator's hold, and the operator resumes the tester", r.endsByOperatorHold)
	r.part(4, "look 1 ends with the tester's claim, failed once its prompt failures and its prompt retirements ran out", r.endsByFailedClaim)
	if !r.base {
		r.part(5, "the check of the child's next merge replaces look 1's, the child having re-entered while the late turn of look 1's task ran", r.endsByALaterMerge)
		r.part(6, "the next look's task takes the place of look 1's acknowledged prompt, is answered busy, and that prompt's turn starts late", r.nextTaskBehindALateTurn)
		r.part(7, "a root reopened inside its tree's linger while the late turn of look 1's task runs reports look 1 from that session after the turn", r.rootRunsAgain)
	}
	r.part(8, "a done child set back to todo under its live tree while the late turn of look 1's task runs reports look 1 from that session after the turn, then passes it from its new run's testing turn", r.childRunsAgain)
	r.part(9, "a root reopened inside its tree's linger, its check open after a pending report, merges the next pull request of its new run", r.rootMergesAgain)
	r.part(10, "first half: a re-entered child, its check open after a pending report, moves to backlog before any merge of its new run", r.childParked)
	r.part(10, "second half: a root reopened inside its tree's linger, its check open after a pending report, moves to backlog before any merge of its new run", r.rootParked)
	if !r.base {
		r.partNamed("fresh session", "the tester's claim gets a fresh session after look 1 ended", r.freshSessionReady)
	}
}

// firstLook readies a part on a daemon and a check of its own, as `ended look` starts: the child,
// which prepare readies further before the boot where it is not nil, reaches testing, its tester's
// testing turn ends and its task retires, and the child closes merged, which opens the check at
// look 1 and sends look 1's task to the idle tester. It answers the fixture, the tester and the
// check.
func (r *lookIDLive) firstLook(where string, prepare func(*lookIDFixture)) (*lookIDFixture, *lookIDAgent, record.ProductionCheck) {
	f := r.fixture(where, nil)
	f.seedImplementing()
	if prepare != nil {
		prepare(f)
	}
	f.boot()
	tester := f.toTesting()
	tester.finish()
	tester.awaitIdle("the tester's testing turn to end and its task to retire")
	return f, tester, f.closeMerged()
}

// rootLook is firstLook for a root fixture: the root reaches testing, its tester's testing turn
// ends, and the root closes merged, which lingers its tree and starts look 1 on the tester's
// resumed session (closeRootMerged).
func (r *lookIDLive) rootLook(where string, prepare func(*lookIDFixture)) (*lookIDFixture, *lookIDAgent, record.ProductionCheck) {
	f := r.rootFixture(where)
	f.seedRootImplementing()
	if prepare != nil {
		prepare(f)
	}
	f.boot()
	tester := f.toTesting()
	tester.finish()
	tester.awaitIdle("the tester's testing turn to end and its task to retire")
	tester, check := f.closeRootMerged(tester)
	return f, tester, check
}

// busyTurn is the turn match failing for task, a look's task that reached a's connection while
// the agent was in no turn: the agent begins a turn of its own while that send is in flight and
// answers the prompt busy, as Oh My Pi answers a prompt while a turn streams. The daemon takes
// that turn for no task's, and task waits for its end unread and unconfirmed: no prompt of it was
// acknowledged, so no result of a turn ever names it.
func (r *lookIDLive) busyTurn(f *lookIDFixture, a *lookIDAgent, task shimwire.Prompt) {
	f.t.Helper()
	a.begin("")
	mark := f.logMark()
	a.busy(task)
	f.awaitLogged("the daemon to read the busy answer to the look's send", mark, a.role, "supervise: the agent refused the prompt", map[string]any{"delivery": task.DeliveryID})
	f.surfaces("after a busy answer to the look's task")
}

// lookOf reads text as a look's task (workflow's checkTask): the look's number and the opening
// time of its check, and whether text is a look's task that gives both.
func lookOf(text string) (int, time.Time, bool) {
	number, opened := lookIDNumber.FindStringSubmatch(text), lookIDOpenedLine.FindStringSubmatch(text)
	if !strings.Contains(text, checkTaskLeadText) || number == nil || opened == nil {
		return 0, time.Time{}, false
	}
	n, err := strconv.Atoi(number[1])
	if err != nil {
		return 0, time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, opened[1])
	if err != nil {
		return 0, time.Time{}, false
	}
	return n, at, true
}

// isLook says whether text is the task of look n of the check opened at opened.
func isLook(text string, n int, opened time.Time) bool {
	got, at, ok := lookOf(text)
	return ok && got == n && at.Equal(opened)
}

// silent fails the running part at assertion when the task of any look but look n of the check
// opened at opened reached the connection of one of agents after since, answered busy or not: the
// text of a look that had ended, sent again. Look 0 is none, so with it every look's task counts.
func (r *lookIDLive) silent(assertion string, since time.Time, n int, opened time.Time, agents ...*lookIDAgent) {
	r.t.Helper()
	for _, a := range agents {
		for _, got := range a.since(since) {
			if strings.Contains(got.prompt.Message, checkTaskLeadText) && !isLook(got.prompt.Message, n, opened) {
				r.fails(assertion, "the task of an ended look reached the %s's connection %s after it ended (answered busy: %t): %s",
					a.role, got.at.Sub(since).Round(time.Millisecond), got.busy, firstLine(got.prompt.Message))
			}
		}
	}
	r.note("no text of an ended look reached the tester's connection after it ended")
}

// nextLook waits for the task of look n of the check opened at opened to reach a's connection and
// holds it to what the step passes on (ownLook). A text of another look that reaches the
// connection first is the text of a look that ended, sent again: with one, the part fails at
// next-look once the start row of look n names what it waits on (startRowError), and with none
// once the wait runs out, naming those texts, what the claim holds and that start row's last error
// — at the base commit `a delivery is already pending`, the ended look's task the claim keeps. A
// task that is no look's the agent takes and works in a turn of its own (workNoLooksTask), and look
// n's task follows it.
func (r *lookIDLive) nextLook(f *lookIDFixture, a *lookIDAgent, n int, opened time.Time, ended ...string) lookRead {
	r.t.Helper()
	deadline := time.Now().Add(lookIDWait)
	var others []string
	for {
		if prompt, came := a.next(time.Second); came {
			switch {
			case isLook(prompt.Message, n, opened):
				return r.ownLook(f, a, prompt, n, opened, ended)
			case strings.Contains(prompt.Message, checkTaskLeadText):
				others = append(others, firstLine(prompt.Message))
			default:
				r.workNoLooksTask(a, prompt)
			}
		}
		if waits := f.startRowError(n); (len(others) > 0 && waits != "") || time.Now().After(deadline) {
			held := f.claimView(a.role)
			r.fails("next-look", "the task of look %d did not reach the %s's connection; %d texts of other looks did (%s); its claim is %s holding %s; the start row of look %d last met %q",
				n, a.role, len(others), strings.Join(others, " | "), held.State, describeHeld(held.Pending), n, waits)
		}
	}
}

// ownLook holds prompt, the task of look n of the check opened at opened, to what the step passes
// on: at the head it gives a look id of its own — none an ended look's task gave (ended), and the
// one the store holds for look n of that check (storedLookID) — and it is the claim's pending
// task, whose stored look id is that id. It answers what the agent read of it.
func (r *lookIDLive) ownLook(f *lookIDFixture, a *lookIDAgent, prompt shimwire.Prompt, n int, opened time.Time, ended []string) lookRead {
	r.t.Helper()
	read := a.readLook(prompt)
	if !r.base {
		if reused, stored := slices.Contains(ended, read.id), f.storedLookID(n, opened); reused || read.id != stored {
			r.fails("own-id", "look %d's task gives the look id of an ended look: %t; the id the store holds for look %d: %t, want false and true",
				n, reused, n, read.id == stored)
		}
	}
	task, lookID := f.pendingTask(a.role)
	if !isLook(task, n, opened) || (!r.base && lookID != read.id) {
		r.fails("pending", "the %s's claim holds %q, with the look id of the task that reached it: %t; want the task of look %d",
			a.role, firstLine(task), lookID == read.id, n)
	}
	f.surfaces(fmt.Sprintf("after look %d's task reached the connection", n))
	r.note("look %d's task reached the %s's connection with its own number and look id, and is the claim's pending task", n, a.role)
	return read
}

// storedLookID is the look id the store holds for look n of the check opened at opened
// (production_checks.look_id for that look and check instance), "" for none. The base has no such
// column, and the base mode never reads it.
func (f *lookIDFixture) storedLookID(n int, opened time.Time) string {
	f.t.Helper()
	var id string
	var look int
	var at *time.Time
	if err := f.st.Pool().QueryRow(context.Background(),
		`select coalesce(look_id, ''), coalesce(look_id_look, 0), look_id_opened_at from production_checks where issue = $1`, f.key).Scan(&id, &look, &at); err != nil {
		f.t.Fatalf("read the look id the store holds for %s: %v", f.key, err)
	}
	if look != n || at == nil || !at.Equal(opened) {
		return ""
	}
	return id
}

// pendingTask is the text of role's pending task as the claim store holds it, and at the head the
// look id it carries (pending_task_deliveries.look_id, which the base lacks): "" for no task.
func (f *lookIDFixture) pendingTask(role claim.Role) (string, string) {
	f.t.Helper()
	query := `select task, '' from pending_task_deliveries where claim_token = $1`
	if !f.r.base {
		query = `select task, coalesce(look_id, '') from pending_task_deliveries where claim_token = $1`
	}
	var task, lookID string
	err := f.st.Pool().QueryRow(context.Background(), query, string(f.token(role))).Scan(&task, &lookID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ""
	}
	if err != nil {
		f.t.Fatalf("read the %s's pending task: %v", role, err)
	}
	return task, lookID
}

// startRowError is the last error the newest start row of look n of the checked issue met, as the
// outbox keeps it while the row waits to run again: "" for a row that met none or has run.
func (f *lookIDFixture) startRowError(n int) string {
	f.t.Helper()
	var why string
	err := f.st.Pool().QueryRow(context.Background(), `select last_error from outbox where issue = $1 and kind = $2
		and payload->>'op' = 'start' and payload->>'checkLook' = $3 order by id desc limit 1`,
		f.key, string(record.OutboxKindSupervise), strconv.Itoa(n)).Scan(&why)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		f.t.Fatalf("read the start row of look %d of %s: %v", n, f.key, err)
	}
	return why
}

// boundComesDue brings look 1's bound, its check_look row, due now, waits for it to end the look,
// and answers the check that ended it.
func (f *lookIDFixture) boundComesDue() record.ProductionCheck {
	f.t.Helper()
	f.exec("bring look 1's bound forward", "update outbox set next_at = now() where issue = $1 and kind = $2", f.key, string(record.OutboxKindCheckLook))
	return f.awaitCheck("look 1's bound to end it", func(c record.ProductionCheck) bool { return c.Looks == 1 && !c.LookOpen })
}

// wakeComesDue brings the check's wake, its check_wake row, due now, where look n is due next, and
// waits for it to start look n.
func (f *lookIDFixture) wakeComesDue(n int) {
	f.t.Helper()
	f.exec(fmt.Sprintf("bring the wake of look %d forward", n), "update outbox set next_at = now() where issue = $1 and kind = $2", f.key, string(record.OutboxKindCheckWake))
	f.awaitCheck(fmt.Sprintf("the wake to start look %d", n), func(c record.ProductionCheck) bool { return c.Looks == n && c.LookOpen })
	f.surfaces(fmt.Sprintf("after look %d starts", n))
}

// operate is the operator's request of role's claim, suspend or resume (POST
// /legion/v1/operator/claims/<token>/<request>), as `legion claims` sends it: a suspension of a
// claim in a turn is held to that turn's end, and a resume is answered once its start is under way.
func (f *lookIDFixture) operate(role claim.Role, request string) {
	f.t.Helper()
	status, body := f.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(f.token(role))+"/"+request, nil, true)
	if status != http.StatusOK && status != http.StatusAccepted {
		f.t.Fatalf("the operator's %s of the %s = %d %s, want 200 or 202", request, role, status, body)
	}
}

// awaitSuspensionsRan waits for the outbox to have run every suspension queued for the checked
// issue — its claims' (supervise rows of op suspend) and its Sandbox's (issue_suspend rows) — so
// none lands on what the step starts next.
func (f *lookIDFixture) awaitSuspensionsRan(what string) {
	f.t.Helper()
	testwait.WithinPolling(f.t, lookIDWait, 100*time.Millisecond, what, func() bool {
		var left int
		if err := f.st.Pool().QueryRow(context.Background(), `select count(*) from outbox where issue = $1
			and ((kind = $2 and payload->>'op' = 'suspend') or kind = $3)`,
			f.key, string(record.OutboxKindSupervise), string(record.OutboxKindIssueSuspend)).Scan(&left); err != nil {
			f.t.Fatalf("count the suspensions queued for %s: %v", f.key, err)
		}
		return left == 0
	})
}

// nextLookOnTheResumedTester is what follows a look that ended on a tester the end suspended: the
// suspension stopped the pane tester played (stopAgent), the driver brings the wake of look n
// forward, look n starts and resumes the tester's session, and look n's task reaches the resumed
// connection (nextLook). No text of an ended look reaches either connection after ended (silent).
func (r *lookIDLive) nextLookOnTheResumedTester(f *lookIDFixture, tester *lookIDAgent, n int, opened, ended time.Time, endedIDs ...string) {
	r.t.Helper()
	f.awaitSuspended(claim.RoleTester, fmt.Sprintf("the end of look %d to suspend the tester", n-1))
	f.stopAgent(tester)
	launched := f.launches(claim.RoleTester)
	f.wakeComesDue(n)
	resumed := f.resume(claim.RoleTester, "ses_tester", launched, fmt.Sprintf("look %d's start to resume the tester", n))
	r.nextLook(f, resumed, n, opened, endedIDs...)
	r.silent("silent", ended, n, opened, tester, resumed)
}

// endsByReport is part 1: look 1's task reaches the tester, which answers its send late, so the
// turn match fails for it (lateSend), and in that late turn the tester reports look 1 pending
// with its look id, which ends it. Look 2's task reaches the resumed tester.
func (r *lookIDLive) endsByReport() {
	f, tester, check := r.firstLook("task ends with its look part 1", nil)
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	r.lateSend(f, tester, task)
	read := tester.readLook(task)
	wake := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
	ended := time.Now()
	status, failure := tester.report(read.opened, read.id, "pending", &wake)
	r.note("in the late turn the tester reported look 1 pending with check_opened_at and the look id from its task: %d %s %s", status, failure.Code, failure.Error)
	if status != http.StatusOK {
		r.fails("report", "the report of look 1 = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	f.surfaces("after the report of look 1")
	tester.finish()
	f.awaitCheck("the report to end look 1", func(c record.ProductionCheck) bool { return c.Looks == 1 && !c.LookOpen })
	r.nextLookOnTheResumedTester(f, tester, 2, check.OpenedAt, ended, read.id)
}

// endsByBound is part 2: look 1's task reaches the tester, which answers it busy in a turn of its
// own (busyTurn), and look 1's bound, brought forward, ends the look while that turn runs; the
// turn then ends. Look 2's task reaches the resumed tester.
func (r *lookIDLive) endsByBound() {
	f, tester, check := r.firstLook("task ends with its look part 2", nil)
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	r.busyTurn(f, tester, task)
	read := tester.readLook(task)
	r.note("look 1's task reached the tester, which began a turn of its own while that send was in flight and answered it busy")
	ended := time.Now()
	f.boundComesDue()
	tester.finish()
	r.note("look 1's bound, brought forward while that turn ran, ended it; the turn then ended")
	r.nextLookOnTheResumedTester(f, tester, 2, check.OpenedAt, ended, read.id)
}

// endsByOperatorHold is part 3: look 1's task reaches the tester, which the operator suspends in a
// turn of its own, look 1's task unread (holdInATurn); look 1's bound ends it, and look 2's start
// meets the operator's hold, which ends look 2 at its start (secondLookMeetsTheHold). The operator
// resumes the tester, and look 3's task, the next look's, reaches it.
func (r *lookIDLive) endsByOperatorHold() {
	f, tester, check := r.firstLook("task ends with its look part 3", nil)
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	read := tester.readLook(task)
	r.holdInATurn(f, tester, task)
	ended := time.Now()
	r.secondLookMeetsTheHold(f)
	launched := f.launches(claim.RoleTester)
	f.operate(claim.RoleTester, "resume")
	resumed := f.resume(claim.RoleTester, "ses_tester", launched, "the operator's resume to launch the tester again")
	f.wakeComesDue(3)
	r.nextLook(f, resumed, 3, check.OpenedAt, read.id)
	r.silent("silent", ended, 3, check.OpenedAt, tester, resumed)
}

// holdInATurn is the operator's suspension of the tester while it is in a turn of its own: task,
// look 1's task, reached the tester, which answered it busy in that turn (busyTurn); the operator's
// suspension is held to that turn's end and lands there, with look 1's task unread, and stops the
// pane the tester's agent played (stopAgent).
func (r *lookIDLive) holdInATurn(f *lookIDFixture, tester *lookIDAgent, task shimwire.Prompt) {
	r.t.Helper()
	r.busyTurn(f, tester, task)
	f.operate(claim.RoleTester, "suspend")
	tester.finish()
	token := f.token(claim.RoleTester)
	testwait.WithinPolling(f.t, lookIDWait, 100*time.Millisecond, "the operator's suspension to land at the end of the tester's turn", func() bool {
		held := f.claimView(claim.RoleTester)
		return held.State == string(supervise.StateSuspended) && held.SuspendedBy == string(supervise.SuspendedByOperator) &&
			storedClaimState(f.t, f.st.Pool(), token) == supervise.StateSuspended
	})
	f.stopAgent(tester)
	r.note("the operator suspended the tester in a turn of its own, look 1's task unread; the suspension landed at that turn's end")
}

// secondLookMeetsTheHold is what follows the operator's hold of the tester (holdInATurn): look 1's
// bound, brought forward, ends look 1, and look 2's wake, brought forward, starts look 2, whose start
// meets the operator's hold, which ends look 2 at its start (the daemon's checkLookHeld). It waits
// for the suspensions the two ends queued to run.
func (r *lookIDLive) secondLookMeetsTheHold(f *lookIDFixture) {
	r.t.Helper()
	f.boundComesDue()
	f.exec("bring the wake of look 2 forward", "update outbox set next_at = now() where issue = $1 and kind = $2", f.key, string(record.OutboxKindCheckWake))
	held := f.awaitCheck("look 2's start to meet the operator's hold, which ends it", func(c record.ProductionCheck) bool { return c.Looks == 2 && !c.LookOpen })
	r.note("look 1's bound ended it, and look 2's start met the operator's hold, which ended look 2; its next wake is at %v", held.WakeAt)
	f.awaitSuspensionsRan("the suspensions the ends of looks 1 and 2 queued to run")
}

// endsByFailedClaim is part 4: look 1's task reaches the tester, which answers it busy in a turn of
// its own (busyTurn); once that turn ends, the agent refuses each send of the task (refuse) — the
// runtime's sweep sending it again after each — until the prompt failures run out, which retires
// the process and relaunches its session, and run out again, which spends the last prompt
// retirement and fails the claim. The failure ends look 1, and look 2's start retries the claim,
// whose relaunched session takes look 2's task.
func (r *lookIDLive) endsByFailedClaim() {
	f, tester, check := r.firstLook("task ends with its look part 4", nil)
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	r.busyTurn(f, tester, task)
	read := tester.readLook(task)
	tester.finish()
	r.note("look 1's task reached the tester, which answered it busy in a turn of its own; that turn ended")
	agents, agent := []*lookIDAgent{tester}, tester
	for retired := 1; retired <= f.cfg.PromptRetireLimit; retired++ {
		launched := f.launches(claim.RoleTester)
		for refused := 1; refused <= f.cfg.PromptFailureLimit; refused++ {
			again := agent.prompt("look 1's task sent again", checkTaskLeadText)
			mark := f.logMark()
			agent.refuse(again)
			if refused < f.cfg.PromptFailureLimit {
				f.awaitLogged("the daemon to count the refusal a prompt failure", mark, claim.RoleTester, "supervise: prompt failure", nil)
				f.sweep(claim.RoleTester)
			}
		}
		if retired < f.cfg.PromptRetireLimit {
			f.relaunched(claim.RoleTester, launched, "the prompt failures' retirement to launch the tester's session again")
			f.stopAgent(agent)
			agent = f.resume(claim.RoleTester, "ses_tester", launched, "the prompt failures' retirement to launch the tester's session again")
			agents = append(agents, agent)
		}
	}
	ended := time.Now()
	testwait.WithinPolling(f.t, lookIDWait, 100*time.Millisecond, "the last prompt retirement to fail the tester's claim", func() bool {
		return f.claimView(claim.RoleTester).State == string(supervise.StateFailed)
	})
	f.awaitCheck("the failed claim to end look 1", func(c record.ProductionCheck) bool { return c.Looks == 1 && !c.LookOpen })
	r.note("the agent refused each send of look 1's task: %d prompt failures retired the process and %d more failed the claim, which ended look 1",
		f.cfg.PromptFailureLimit, f.cfg.PromptFailureLimit)
	f.stopAgent(agent)
	launched := f.launches(claim.RoleTester)
	f.wakeComesDue(2)
	resumed := f.resume(claim.RoleTester, "ses_tester", launched, "look 2's start to retry the failed claim")
	r.nextLook(f, resumed, 2, check.OpenedAt, read.id)
	r.silent("silent", ended, 2, check.OpenedAt, append(agents, resumed)...)
}

// endsByALaterMerge is part 5: look 1's task reaches the tester, which answers its send late
// (lateSend); while that late turn runs a person sets the child back to todo under its live tree,
// and it re-enters; the turn then ends. The new run's next pull request merges and the person's
// done closes the child again, which opens the check that replaces look 1's, whose look-1 task
// reaches the tester.
func (r *lookIDLive) endsByALaterMerge() {
	f, tester, check := r.firstLook("task ends with its look part 5", nil)
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	r.lateSend(f, tester, task)
	read := tester.readLook(task)
	ended := time.Now()
	f.rerun(f.key)
	tester.finish()
	r.note("while the late turn of look 1's task ran, a person set %s back to todo under its live tree, and it re-entered; the turn then ended", f.key)
	next := f.nextMerge(check)
	r.nextLook(f, tester, 1, next.OpenedAt, read.id)
	r.silent("silent", ended, 1, next.OpenedAt, tester)
}

// nextTaskBehindALateTurn is part 6, the order row 4 of supervise's
// TestAnEndedLooksTaskGivesWayToTheNextTask holds on a machine: the tester acknowledges look 1's
// prompt and starts no turn of it; the child re-enters, and its next merge's check opens, whose
// look-1 task, the next look's, takes the place of look 1's and reaches the tester, which answers
// that send busy; the turn of look 1's prompt then starts late and ends. That turn confirms
// nothing, and the next look's task is sent again after it.
func (r *lookIDLive) nextTaskBehindALateTurn() {
	f, tester, check := r.firstLook("task ends with its look part 6", nil)
	old := tester.prompt("the look-1 task", checkTaskLeadText)
	tester.ack(old)
	testwait.WithinPolling(f.t, lookIDWait, 50*time.Millisecond, "the daemon to record look 1's prompt acknowledged", func() bool {
		held := f.claimView(claim.RoleTester).Pending
		return held != nil && held.DeliveredAt != nil
	})
	read := tester.readLook(old)
	reentered := time.Now()
	f.rerun(f.key)
	r.note("the tester acknowledged look 1's prompt and started no turn of it; a person then set %s back to todo, and it re-entered", f.key)
	next := f.nextMerge(check)
	sent := tester.prompt("look 1's task of the check that replaced it", checkTaskLeadText)
	if !isLook(sent.Message, 1, next.OpenedAt) {
		r.fails("next-look", "the tester was sent %q, want the task of look 1 of the check that opened at %s", firstLine(sent.Message), next.OpenedAt.UTC().Format(time.RFC3339Nano))
	}
	mark := f.logMark()
	tester.busy(sent)
	f.awaitLogged("the daemon to read the busy answer to the next look's send", mark, claim.RoleTester, "supervise: the agent refused the prompt", map[string]any{"delivery": sent.DeliveryID})
	f.surfaces("after a busy answer to the next look's task")
	tester.begin(old.ID)
	r.note("the tester answered the next look's send busy, and the turn of look 1's acknowledged prompt then started late")
	ended := time.Now()
	tester.finish()
	r.nextLook(f, tester, 1, next.OpenedAt, read.id)
	if !slices.ContainsFunc(tester.since(ended), func(got lookIDPrompt) bool { return !got.busy && isLook(got.prompt.Message, 1, next.OpenedAt) }) {
		r.fails("sent-again", "the next look's task did not reach the tester's connection after the late turn of look 1's prompt ended")
	}
	r.note("the next look's task was sent again after the late turn ended")
	r.silent("silent", reentered, 1, next.OpenedAt, tester)
}

// rootRunsAgain is part 7: look 1's task reaches the root's tester, which answers its send late
// (lateSend); while that turn runs a person sets the root back to todo inside its tree's linger,
// which re-admits it, and the turn then ends. Nothing of look 1 reaches the connection after the
// turn; the tester's report from that session, posted only after the driver read the connection
// for lookIDQuietSends send deadlines, so the report's own retirement of the look's task cannot
// hide a fence that failed, is answered 200 and recorded against look 1; and while the root runs,
// read_record shows the check open, and a wake row made due starts no look.
func (r *lookIDLive) rootRunsAgain() {
	f, tester, check := r.rootLook("task ends with its look part 7", nil)
	read, ended := r.lateTurnAcrossTheRerun(f, tester)
	wake := r.pendingFromThatSession(f, tester, read, ended, true, false)
	r.pendingRecorded(f, wake)
	r.silent("silent", ended, 0, time.Time{}, tester)
	r.checkOpenWhileItRuns(f, check)
	f.wakeStartsNoLook("wake-starts-none")
}

// childRunsAgain is part 8: part 7 for a done child, which a person sets back to todo under its
// live tree while the late turn runs, so it re-enters (workflow's ReenterChild); read_record after
// the re-entry and before the report shows the check open at the same look. The new run then
// reaches its testing phase, whose task reaches the tester in the same session; from that session
// the tester reports look 1 pass, which closes the check as recorded while testing stays open. The
// child's next pull request then merges and the person's done closes it, which opens a new check
// at look 1 with a look id of its own, and a report with look 1's id of the old check is refused
// and records nothing.
func (r *lookIDLive) childRunsAgain() {
	f, tester, check := r.firstLook("task ends with its look part 8", (*lookIDFixture).runsImplementerThenTester)
	implementer := f.launches(claim.RoleImplementer)
	read, ended := r.lateTurnAcrossTheRerun(f, tester)
	before := f.recordCheck("after the re-entry, before the report")
	wake := r.pendingFromThatSession(f, tester, read, ended, true, false)
	r.pendingRecorded(f, wake)
	r.silent("silent", ended, 0, time.Time{}, tester)
	if before.Check == nil || !before.Check.OpenedAt.Equal(check.OpenedAt) || before.Check.Looks != 1 {
		r.fails("record-open", "read_record after the re-entry and before the report shows the check %+v, want it open at look 1, opened %s",
			before.Check, check.OpenedAt.UTC().Format(time.RFC3339Nano))
	}
	r.note("read_record after the re-entry and before the report showed the check open at look 1")
	r.checkOpenWhileItRuns(f, check)
	f.wakeStartsNoLook("wake-starts-none")
	r.newRunToTesting(f, tester, implementer)
	status, failure := tester.report(read.opened, read.id, "pass", nil)
	r.note("from the testing turn of that session the tester reported look 1 pass with check_opened_at and its look id: %d %s %s", status, failure.Code, failure.Error)
	if status != http.StatusOK {
		r.fails("pass-report", "the pass of look 1 from the new run's testing turn = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	f.surfaces("after the pass of look 1, from the new run's testing turn")
	closed := f.awaitCheck("the pass to close the check", func(c record.ProductionCheck) bool { return !c.Open() })
	if closed.Outcome != record.CheckRecorded {
		r.fails("closed-pass", "the check closed as %s, want %s, the pass", closed.Outcome, record.CheckRecorded)
	}
	if got := f.issue(f.key).Phase; got != phase.Testing {
		r.fails("testing-open", "after the pass %s stands in %s, want its testing phase still open", f.key, got)
	}
	r.note("the check closed as %s, and %s's testing phase stays open", closed.Outcome, f.key)
	f.mergeAndDone(f.key, f.number+1, lookIDHead2)
	next := f.replacedBy(check)
	if !r.base {
		if id := f.storedLookID(1, next.OpenedAt); id == "" || id == read.id {
			r.fails("new-check", "the new check holds a look id of its own for look 1: %t, want true", id != "" && id != read.id)
		}
	}
	unchanged, reports := f.check(), f.reports()
	status, failure = tester.report(read.opened, read.id, "pass", nil)
	r.note("a pass with check_opened_at and the look id of the old check's look 1: %d %s %q", status, failure.Code, failure.Error)
	if after := f.check(); status == http.StatusOK || !reflect.DeepEqual(after, unchanged) || f.reports() != reports {
		r.fails("old-id-refused", "the pass with the old look's id = %d %s, the check changed: %t, reports processed %d, want a refusal that records nothing (%d)",
			status, failure.Code, !reflect.DeepEqual(after, unchanged), f.reports(), reports)
	}
}

// rootMergesAgain is part 9: the root of part 7, its check open after a pending report, and a twin
// root reopened inside its linger with no open check (reopenTwinRoot). The next pull request of
// each new run merges and a person's done closes each again. The daemon's log has the old check's
// close as not run, naming the new merge, before the new check's open; the issue_close and
// suspension rows the root's done queues are the rows the twin's queues, since the leave queues
// them in the close's own transaction (sameRowsAsTheTwin); and look 1's task of the new check
// reaches the tester.
func (r *lookIDLive) rootMergesAgain() {
	var twin string
	var twinNumber int
	f, tester, check := r.rootLook("task ends with its look part 9", func(f *lookIDFixture) {
		twin, twinNumber = f.cfg.Project+"-3", f.number+100
		f.seedTwinRoot(twin, twinNumber)
		recordOutboxWrites(f.t, f.st.Pool())
	})
	read, ended := r.lateTurnAcrossTheRerun(f, tester)
	r.pendingFromThatSession(f, tester, read, ended, false, true)
	f.reopenTwinRoot(twin)
	mark := f.logMark()
	launched := f.launches(claim.RoleTester)
	f.nextPullRequest(f.key, f.number+1, lookIDHead2)
	moved := f.mergeAndDone(f.key, f.number+1, lookIDHead2)
	next := f.replacedBy(check)
	twinMoved := f.mergeAndDone(twin, twinNumber, lookIDHead)
	merge := "merge-" + lookIDHead2
	// The workflow writes both lines once the transaction that closes the old check and opens the
	// new one has committed (workflow's logOnCommit), after the write replacedBy read, and the
	// close's line first (markCheckClosed, which openCheck calls before it logs the open): the read
	// waits for the open's line, up to the bound, and reads the close's line with it.
	closedAt, openedAt := -1, -1
	testwait.Holds(lookIDWait, 50*time.Millisecond, func() bool {
		closedAt = f.loggedIndex(mark, func(kept loggedRecord) bool {
			return kept.Message == "workflow: production check closed" && kept.Attrs["issue"] == f.key &&
				kept.Attrs["outcome"] == string(record.CheckNotRun) && kept.Attrs["replacedBy"] == merge
		})
		openedAt = f.loggedIndex(mark, func(kept loggedRecord) bool {
			return kept.Message == "workflow: production check opened" && kept.Attrs["issue"] == f.key && kept.Attrs["mergeCommit"] == merge
		})
		return openedAt >= 0
	})
	switch {
	case closedAt < 0:
		r.fails("close-line", "the daemon's log has no line `workflow: production check closed` of the old check of %s with the outcome not_run and the merge commit of the new merge (replacedBy %s)", f.key, merge)
	case openedAt < closedAt:
		r.fails("close-line", "the daemon's log has the old check's close at its record %d and the new check's open at its record %d, want the close first", closedAt, openedAt)
	}
	r.note("the daemon's log closes the old check as not_run, replaced by %s, before it opens the new check", merge)
	r.sameRowsAsTheTwin(f, "rows", moved, twinMoved, twin)
	f.relaunched(claim.RoleTester, launched, "the new check's look-1 start to resume the tester the linger suspended")
	f.stopAgent(tester)
	resumed := f.resume(claim.RoleTester, "ses_tester", launched, "the new check's look-1 start to resume the tester the linger suspended")
	r.nextLook(f, resumed, 1, next.OpenedAt, read.id)
}

// childParked is the first half of part 10: the child of part 8, its check open after a pending
// report, and a twin child set back to todo the same way with no open check, each moved to
// backlog before any merge of its new run (parked).
func (r *lookIDLive) childParked() {
	var twin string
	f, tester, _ := r.firstLook("task ends with its look part 10, a child", func(f *lookIDFixture) {
		twin = f.cfg.Project + "-3"
		f.seedTwinChild(twin)
		recordOutboxWrites(f.t, f.st.Pool())
	})
	read, ended := r.lateTurnAcrossTheRerun(f, tester)
	r.pendingFromThatSession(f, tester, read, ended, false, true)
	f.rerun(twin)
	r.parked(f, "child", twin)
}

// rootParked is the second half of part 10: the root of part 7, its check open after a pending
// report, and a twin root reopened inside its linger with no open check, each moved to backlog
// before any merge of its new run (parked).
func (r *lookIDLive) rootParked() {
	var twin string
	f, tester, _ := r.rootLook("task ends with its look part 10, a root", func(f *lookIDFixture) {
		twin = f.cfg.Project + "-3"
		f.seedTwinRoot(twin, f.number+100)
		recordOutboxWrites(f.t, f.st.Pool())
	})
	read, ended := r.lateTurnAcrossTheRerun(f, tester)
	r.pendingFromThatSession(f, tester, read, ended, false, true)
	f.reopenTwinRoot(twin)
	r.parked(f, "root", twin)
}

// parked is part 10's move and what each half passes on, for a checked issue that ran again with
// its check open (who: child or root): a person moves it to backlog before any merge of its new
// run. The store shows the check closed as not_run at the move; a wake row of the check made due
// then starts no look; the issue_close and suspension rows the move queues are the rows the same
// move of twin, with no open check, queues, since the leave queues them in the close's own
// transaction (sameRowsAsTheTwin); and the close writes no Dispatch status on the issue, whose
// status stays backlog.
func (r *lookIDLive) parked(f *lookIDFixture, who, twin string) {
	r.t.Helper()
	// The status the issue's new run writes over the person's todo is queued in a fact's
	// transaction (admission's re-admission of a root, or its promotion of a child), and the
	// board lane writes it to Dispatch after that transaction, on a goroutine of its own. So the
	// count the move is held to is read once the outbox holds no status write of the issue, or at
	// the bound.
	testwait.Holds(lookIDWait, 100*time.Millisecond, func() bool {
		var queued bool
		if err := f.st.Pool().QueryRow(context.Background(), `select exists (select 1 from outbox where issue = $1 and kind = $2)`,
			f.key, string(record.OutboxKindDispatchStatus)).Scan(&queued); err != nil {
			f.t.Fatalf("read whether a status write of %s is queued: %v", f.key, err)
		}
		return !queued
	})
	writes := f.statusWrites(f.key)
	movedAt := time.Now()
	moved := f.move(f.key, "backlog")
	f.awaitMoved(moved)
	closed := f.check()
	switch {
	case closed.Open():
		r.fails(who+" closed-at-move", "the check is open after the move to backlog at %s, at look %d (look open: %t); want it closed as not_run at the move",
			movedAt.UTC().Format(time.RFC3339Nano), closed.Looks, closed.LookOpen)
	case closed.ClosedAt == nil || closed.ClosedAt.Before(movedAt):
		r.fails(who+" closed-at-move", "the check closed as %s at %v, before the move to backlog at %s; want it open until the move and closed as not_run at it",
			closed.Outcome, closed.ClosedAt, movedAt.UTC().Format(time.RFC3339Nano))
	case closed.Outcome != record.CheckNotRun:
		r.fails(who+" closed-at-move", "the check closed at the move as %s, want %s", closed.Outcome, record.CheckNotRun)
	}
	r.note("the person's move of %s to backlog closed its check as %s", f.key, closed.Outcome)
	f.wakeStartsNoLook(who + " wake-starts-none")
	twinMoved := f.move(twin, "backlog")
	f.awaitMoved(twinMoved)
	r.sameRowsAsTheTwin(f, who+" rows", moved, twinMoved, twin)
	time.Sleep(liveSettle)
	if got, status := f.statusWrites(f.key), f.boardStatus(f.key); got != writes || status != "backlog" {
		r.fails(who+" status", "the daemon wrote %d Dispatch statuses on %s after the move, which stands at %s on the board; want none, and backlog", got-writes, f.key, status)
	}
	r.note("the daemon wrote no Dispatch status on %s, which stays backlog", f.key)
}

// freshSessionReady is the part no check numbers: look 1's task reaches the tester, which answers
// it busy in a turn of its own (busyTurn); look 1's bound ends it, and the end suspends the tester
// once that turn ends. Once every suspension the end queued has run, the controller gives the
// tester's claim a fresh session (freshSession), whose agent comes up ready in a new session:
// nothing of look 1 reaches its connection at that ready, nor for lookIDQuietSends send deadlines
// after it, and look 2's task reaches it once look 2 is due.
func (r *lookIDLive) freshSessionReady() {
	f, tester, check := r.firstLook("task ends with its look fresh session", nil)
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	r.busyTurn(f, tester, task)
	read := tester.readLook(task)
	ended := time.Now()
	f.boundComesDue()
	tester.finish()
	f.awaitSuspended(claim.RoleTester, "the end of look 1 to suspend the tester")
	f.awaitSuspensionsRan("the suspensions the end of look 1 queued to run")
	f.stopAgent(tester)
	launched := f.launches(claim.RoleTester)
	notRestarted := f.freshSession(claim.RoleTester)
	r.note("look 1 ended by its bound; the controller then gave the tester's claim a fresh session (phaseNotRestarted %q)", notRestarted)
	fresh := f.comeUp(claim.RoleTester, "ses_tester_fresh", "", launched, "the fresh session's start of the tester")
	quiet := lookIDQuietSends * f.cfg.WorkerRPCTimeout
	time.Sleep(quiet)
	r.silent("ready-silent", ended, 0, time.Time{}, tester, fresh)
	r.note("nothing of look 1 reached the fresh session's connection at its ready, nor in the %s after it", quiet)
	f.wakeComesDue(2)
	r.nextLook(f, fresh, 2, check.OpenedAt, read.id)
}

// freshSession is the controller's fresh_session of role's claim on the checked issue (POST
// /legion/v1/claims/fresh-session), with the controller grant the operator mints (POST
// /legion/v1/grants under the operator's token), each as raw JSON. It answers the sentence the
// answer gives for a phase it did not restart.
func (f *lookIDFixture) freshSession(role claim.Role) string {
	f.t.Helper()
	status, body := f.d.request(http.MethodPost, "/legion/v1/grants", map[string]any{}, true)
	if status != http.StatusOK {
		f.t.Fatalf("the operator's controller grant = %d %s, want 200", status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		f.t.Fatalf("decode the controller grant: %v", err)
	}
	status, body = f.d.request(http.MethodPost, "/legion/v1/claims/fresh-session",
		map[string]any{"grantId": granted.GrantID, "issue": f.key, "role": string(role)}, false)
	if status != http.StatusOK && status != http.StatusAccepted {
		f.t.Fatalf("the fresh session of the %s = %d %s, want 200 or 202", role, status, body)
	}
	var answered struct {
		PhaseNotRestarted string `json:"phaseNotRestarted"`
	}
	if err := json.Unmarshal(body, &answered); err != nil {
		f.t.Fatalf("decode the fresh session's answer %s: %v", body, err)
	}
	return answered.PhaseNotRestarted
}

// lateTurnAcrossTheRerun is how parts 7 to 10 go on once look 1 is open: look 1's task reaches the
// tester, which answers its send late, so the turn match fails for it (lateSend); while that turn
// runs a person sets the checked issue back to todo — a child under its live tree re-enters, a
// root inside its tree's linger is re-admitted — and it runs again (rerun); the turn then ends. It
// answers what the tester read of look 1's task and the moment the turn ended.
func (r *lookIDLive) lateTurnAcrossTheRerun(f *lookIDFixture, tester *lookIDAgent) (lookRead, time.Time) {
	r.t.Helper()
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	r.lateSend(f, tester, task)
	read := tester.readLook(task)
	f.rerun(f.key)
	r.note("while the late turn of look 1's task ran, a person set %s back to todo, and it runs again at run %d; the turn then ended", f.key, f.issue(f.key).Generation)
	ended := time.Now()
	tester.finish()
	return read, ended
}

// pendingFromThatSession is the tester's report of look 1 from the session whose turn ran its
// task, once that turn has ended at ended: pending, with a wake 30 minutes out, check_opened_at and
// the look id from the task. Where watch is set it is posted only after the driver read the
// tester's connection for lookIDQuietSends of the daemon's send deadlines past ended, so the
// report's own retirement of the look's task cannot hide a fence that failed. A report that is its
// part's setup (setup) and is refused at the base commit is noted, and the part goes on to what
// its negative control names. It answers the wake.
func (r *lookIDLive) pendingFromThatSession(f *lookIDFixture, tester *lookIDAgent, read lookRead, ended time.Time, watch, setup bool) time.Time {
	r.t.Helper()
	if watch {
		quiet := lookIDQuietSends * f.cfg.WorkerRPCTimeout
		time.Sleep(time.Until(ended.Add(quiet)))
		r.note("the driver read the tester's connection for %s past the turn's end before the report", quiet)
	}
	wake := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
	status, failure := tester.report(read.opened, read.id, "pending", &wake)
	r.note("from that session the tester reported look 1 pending with wake %s, check_opened_at and the look id from its task: %d %s %s",
		wake.Format(time.RFC3339Nano), status, failure.Code, failure.Error)
	f.surfaces("after the report of look 1, from the session of its turn")
	switch {
	case status == http.StatusOK:
	case setup && r.base:
		r.note("at the base commit that report is refused; the part goes on to what its negative control names")
	default:
		r.fails("report", "the report of look 1 = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	return wake
}

// pendingRecorded fails the running part at recorded unless the pending report of look 1 is
// recorded against that look: the intake took it as look 1's pending, and the check, still open,
// holds look 1 closed and names the report's wake.
func (r *lookIDLive) pendingRecorded(f *lookIDFixture, wake time.Time) {
	r.t.Helper()
	recorded, reports := f.check(), f.lookReports(1, "pending")
	if !recorded.Open() || reports != 1 || recorded.Looks != 1 || recorded.LookOpen || recorded.WakeAt == nil || !recorded.WakeAt.Equal(wake) {
		r.fails("recorded", "after the report the check is open: %t, at look %d (open: %t), with its wake at %v and %d pending reports of look 1; want it open at look 1, closed, with the wake %s and one report",
			recorded.Open(), recorded.Looks, recorded.LookOpen, recorded.WakeAt, reports, wake.Format(time.RFC3339Nano))
	}
	r.note("the report is recorded against look 1, and the open check names its wake")
}

// checkOpenWhileItRuns fails the running part at record-open unless read_record shows the checked
// issue running again with the check that opened with check open at look 1.
func (r *lookIDLive) checkOpenWhileItRuns(f *lookIDFixture, check record.ProductionCheck) {
	r.t.Helper()
	seen := f.recordCheck("while the issue runs again, after the report")
	if seen.Phase == phase.Done || seen.Check == nil || !seen.Check.OpenedAt.Equal(check.OpenedAt) || seen.Check.Looks != 1 {
		r.fails("record-open", "read_record shows %s in %s with the check %+v; want it running and its check open at look 1, opened %s",
			f.key, seen.Phase, seen.Check, check.OpenedAt.UTC().Format(time.RFC3339Nano))
	}
	r.note("while %s runs again in %s, read_record shows its check open at look 1", f.key, seen.Phase)
}

// wakeStartsNoLook brings the check's wake row due now and fails the running part at assertion
// unless the row runs and starts no look: the check waits while its issue runs again
// (workflow's checkWakeDue defers the wake), and a closed check starts none.
func (f *lookIDFixture) wakeStartsNoLook(assertion string) {
	f.t.Helper()
	before := f.check()
	mark := f.logMark()
	tag, err := f.st.Pool().Exec(context.Background(), "update outbox set next_at = now() where issue = $1 and kind = $2", f.key, string(record.OutboxKindCheckWake))
	if err != nil {
		f.t.Fatalf("bring the check's wake forward: %v", err)
	}
	if tag.RowsAffected() == 0 {
		f.r.fails(assertion, "the check, open: %t, at look %d, holds no wake row to make due", before.Open(), before.Looks)
	}
	var ran *loggedRecord
	testwait.WithinPolling(f.t, lookIDWait, 100*time.Millisecond, "the wake row made due to run", func() bool {
		ran = f.logged.firstAfter(mark, func(kept loggedRecord) bool {
			return kept.Attrs["issue"] == f.key && (kept.Message == "workflow: production check wake deferred while its issue runs again" ||
				kept.Message == "workflow: production check wake starts no look")
		})
		return ran != nil || f.check().Looks != before.Looks
	})
	if after := f.check(); after.Looks != before.Looks || after.LookOpen {
		f.r.fails(assertion, "the wake made due started look %d of the check (look open: %t), want no look", after.Looks, after.LookOpen)
	}
	f.r.note("the wake made due now started no look: %s", ran.Message)
}

// rerun is a person setting key back to todo (move): a child of a live tree re-enters it
// (workflow's ReenterChild), and a root inside its tree's linger is re-admitted (admit's
// readmit). It waits for key to run again at its next generation.
func (f *lookIDFixture) rerun(key string) {
	f.t.Helper()
	before := f.issue(key).Generation
	f.prompts(key)
	f.awaitMoved(f.move(key, "todo"))
	testwait.WithinPolling(f.t, lookIDWait, 100*time.Millisecond, key+" to run again at its next generation", func() bool {
		return f.issue(key).Generation > before
	})
}

// reopenTwinRoot takes the twin root, seeded implementing on an open pull request, out of the
// workflow with no merge — a person's done, which lingers its tree and opens no check — and back in
// inside that linger, a person's todo, which re-admits it (rerun): a root reopened inside its linger
// with no open check.
func (f *lookIDFixture) reopenTwinRoot(twin string) {
	f.t.Helper()
	f.awaitMoved(f.move(twin, "done"))
	f.rerun(twin)
	f.r.note("the twin root %s left with no merge and was reopened inside its tree's linger, with no check", twin)
}

// nextPullRequest is the next pull request of key's new run, numbered number at head: GitHub's
// stand-in serves it from then on (serve), and GitHub's opened event of it, which the daemon
// records (workflow's pullRequestOpened), arrives.
func (f *lookIDFixture) nextPullRequest(key string, number int, head string) {
	f.t.Helper()
	f.serve(key, number, head)
	f.waitProcessed(f.publishPullRequest(livePullRequestEvent{action: "opened", number: number, issue: key, head: head,
		base: lookIDBranch, title: f.issues[key].title, updated: time.Now().UTC()}, fmt.Sprintf("pr-%d-opened", number)))
}

// mergeAndDone is GitHub's merge of key's pull request number at head, and then a person's done,
// as closeMerged closes the checked issue the first time: a merge short of the merging phase
// closes nothing, and the done closes the issue on its merged pull request. It answers the done's
// event id.
func (f *lookIDFixture) mergeAndDone(key string, number int, head string) string {
	f.t.Helper()
	f.waitProcessed(f.publishMergeOf(number, key, head, lookIDBranch))
	moved := f.move(key, "done")
	f.awaitMoved(moved)
	return moved
}

// nextMerge is the checked issue's next pull request of its new run, numbered one past the first
// at lookIDHead2, opened and merged, and the person's done (mergeAndDone). It answers the check
// that merge opens, which replaces old (replacedBy).
func (f *lookIDFixture) nextMerge(old record.ProductionCheck) record.ProductionCheck {
	f.t.Helper()
	f.nextPullRequest(f.key, f.number+1, lookIDHead2)
	f.mergeAndDone(f.key, f.number+1, lookIDHead2)
	return f.replacedBy(old)
}

// replacedBy waits for the close of the checked issue's next merge to open the check that replaces
// old, at look 1, and answers it.
func (f *lookIDFixture) replacedBy(old record.ProductionCheck) record.ProductionCheck {
	f.t.Helper()
	next := f.awaitCheck("the close of the next merge to open the check that replaces the old one", func(c record.ProductionCheck) bool {
		return c.Open() && c.OpenedAt.After(old.OpenedAt) && c.Looks == 1
	})
	f.r.note("%s: the next pull request merged and the person's done closed %s again; a new check opened at %s, at look 1",
		f.where, f.key, next.OpenedAt.UTC().Format(time.RFC3339Nano))
	return next
}

// newRunToTesting brings the child's run after its re-entry to testing as production does, over
// the default steps implementer then tester (runsImplementerThenTester): the run's implementing
// start launches the implementer's session again, kept since the child's close (its launches
// before the re-entry were implementerLaunches), which takes its task, opens the next pull
// request at lookIDHead2 and completes. The testing task then reaches tester's claim, alive in the
// session it worked look 1 in and launched no more since, which takes it and works it.
func (r *lookIDLive) newRunToTesting(f *lookIDFixture, tester *lookIDAgent, implementerLaunches int) {
	r.t.Helper()
	testerLaunches := f.launches(claim.RoleTester)
	imp := f.resume(claim.RoleImplementer, "ses_implementer", implementerLaunches, "the new run's implementing start to launch the implementer again")
	task := imp.prompt("the new run's implementing task", "")
	imp.ack(task)
	imp.begin(task.ID)
	f.nextPullRequest(f.key, f.number+1, lookIDHead2)
	status, body := f.d.request(http.MethodPost, "/legion/v1/handoff/complete", api.HandoffCompleteRequest{
		GrantID: f.grant(imp.liveSession), Summary: "implemented the next run", Commit: lookIDHead2,
		HandoffID: imp.handoff("implement"), HandoffVersion: 1,
	}, false)
	if status != http.StatusOK {
		f.t.Fatalf("the new run's implementer completion = %d %s, want 200", status, body)
	}
	imp.finish()
	f.waitFor("the child's new run in testing", func(got api.Issue) bool { return got.Phase == phase.Testing && got.Generation > 1 })
	testing := tester.prompt("the new run's testing task", "Phase: testing")
	if launched := f.launches(claim.RoleTester); launched != testerLaunches {
		f.t.Fatalf("the tester was launched %d times since look 1, want its testing task in the session it worked look 1 in", launched-testerLaunches)
	}
	tester.ack(testing)
	tester.begin(testing.ID)
	r.note("the new run reached testing; its testing task reached the tester in the session of look 1, which works it")
}

// sameRowsAsTheTwin fails the running part at assertion unless the issue_close and suspension rows
// the transaction of the move moved queued for the checked issue are the rows the transaction of
// the same move of twin, which has no open check, queued (movedRows): the leave the close is part
// of queues those rows in the close's own transaction, so only the twin tells the close's own rows
// apart. The close's own retire_look row is neither kind, and so is left out.
func (r *lookIDLive) sameRowsAsTheTwin(f *lookIDFixture, assertion, moved, twinMoved, twin string) {
	r.t.Helper()
	mine, theirs := f.movedRows(moved, f.key), f.movedRows(twinMoved, twin)
	if len(mine) == 0 || !slices.Equal(mine, theirs) {
		r.fails(assertion, "the move of %s queued the issue_close and suspension rows %q, and the same move of its twin %s, which has no open check, queued %q; want the same rows",
			f.key, mine, twin, theirs)
	}
	r.note("the move queued %d issue_close and suspension rows, the rows the same move of the twin %s, with no open check, queued", len(mine), twin)
}

// movedRows is every issue_close and suspension row — a supervise row of op issue_close or
// suspend, and an issue_suspend row — the transaction that applied the Dispatch event moved
// queued, read from the database's own record of every outbox write (recordOutboxWrites) by that
// transaction's id, which its processed_events row carries (xmin). Each is its kind and its payload
// with key, the moved issue, written `<issue>`, sorted.
func (f *lookIDFixture) movedRows(moved, key string) []string {
	f.t.Helper()
	rows, err := f.st.Pool().Query(context.Background(), `select w.kind, w.payload::text from outbox_written w
		join processed_events p on w.xmin = p.xmin
		where p.event_id = $1 and ((w.kind = $2 and w.payload->>'op' in ('issue_close', 'suspend')) or w.kind = $3)`,
		moved, string(record.OutboxKindSupervise), string(record.OutboxKindIssueSuspend))
	if err != nil {
		f.t.Fatalf("read the rows the move %s queued: %v", moved, err)
	}
	defer rows.Close()
	var queued []string
	for rows.Next() {
		var kind, payload string
		if err := rows.Scan(&kind, &payload); err != nil {
			f.t.Fatalf("read a row the move %s queued: %v", moved, err)
		}
		queued = append(queued, kind+" "+strings.ReplaceAll(payload, key, "<issue>"))
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("read the rows the move %s queued: %v", moved, err)
	}
	slices.Sort(queued)
	return queued
}

// loggedIndex is the index of the first record the running daemon logged, from the mark from on,
// that want holds of, and -1 for none.
func (f *lookIDFixture) loggedIndex(from int, want func(loggedRecord) bool) int {
	f.logged.mu.Lock()
	defer f.logged.mu.Unlock()
	for i := from; i < len(f.logged.records); i++ {
		if want(f.logged.records[i]) {
			return i
		}
	}
	return -1
}

// statusWrites is how many Dispatch statuses the daemon has written on key.
func (f *lookIDFixture) statusWrites(key string) int {
	written := 0
	for _, write := range f.board.writes() {
		if write.Issue == key && write.Status != "" {
			written++
		}
	}
	return written
}

// boardStatus is key's status as Dispatch's board holds it, the daemon's writes applied.
func (f *lookIDFixture) boardStatus(key string) string {
	f.board.mu.Lock()
	defer f.board.mu.Unlock()
	return f.board.issues[key].status
}

// ---- step `upgrade` ------------------------------------------------------------------------------

// upgrade is c-upgrade, each part on a daemon and a check of its own. A part brings its check to the
// look it names on the daemon of the run, stops that daemon, writes into the store what a daemon of
// the release before this change leaves there (bootOver, seedOlder), and boots the daemon at the
// head on that store, as a deploy of this change boots it. The tester's claim of each part is one
// that release registered (lookIDOldContract): the boot's stale-plugin lane relaunches its process,
// or, where the operator holds it, the operator's resume launches it, or the boot's new start of
// the look resumes it where the look's end suspended it, and its agent registers on this change's
// contract (cameUpOnThisContract). The part passes when the boot gives the open look a look id of
// its own and keeps its number and the check open (lookGivenAnID, parts 1 to 4, 6 to 8 and 9);
// when no text that release left — the tester's pending task, and in part 6 the task of that
// release's start row of the look — reaches a connection (notOlder, noOlderText); and when the
// task of the open look, giving check_opened_at and the look's id, reaches the connection
// (upgradedLook) and a report with both is answered 200 (reportUpgraded). Part 6 runs in both
// orders, the older start row after the boot's row, as the check names it, and before it.
//
// What the way back does is the guide's section "Rolling back the release whose look report gives
// its look id" (docs/kubernetes.md), and which open looks the boot gives a new look id and a new
// task is workflow.Engine.RepairOpenChecks's, with its lookTaskGivesAnotherID. After the way back,
// the seed of parts 1, 2, 3, 5, 6 and 8 leaves the check with no look id (seedOlder says where
// such a check comes from), so where those parts leave a look open the boot finds it with no look
// id of its own. Part 7's seed leaves the check with the look id of the check a later merge
// replaced. Parts 4 and 9 keep the id the way back kept in their seed (lookIDSeed.keepsID) and
// read that it did (wayBackKept) on a root that stays out of the workflow, as its merge left it.
// Part 9 seeds such a root whose tester holds a task that gives the kept id in a delivery that
// holds none, and holds that its look gets a new id and a new task; part 10, which runs the way
// back with no seed, holds that a root that runs again keeps the id, so the report its tester
// makes with it is taken.
//
// At the base commit, whose schema has no look-id column, parts 4, 7, 9 and 10, which hold the
// boot to a stored look id, do not run, and the driver writes and reads no look-id column. Each
// part that runs fails at seeded-text (each half of part 6 at its own): the text the release before
// left reaches the connection once the claim is ready, since the base's boot gives no look an id
// and no new task — in part 3 it leaves the look as it found it.
func (r *lookIDLive) upgrade() {
	if r.base {
		r.note("parts 4, 7, 9 and 10 do not run at the base commit: each holds the boot to a stored look id, and the base's schema has no look-id column")
	}
	r.part(1, "an open look with no look id, the text of that look with no look id pending on a tester the release before registered", func() {
		r.upgradeOpenLook("upgrade part 1", false, true)
	})
	r.part(2, "the same, the look marked the last look of its check, whose pass closes the check", func() {
		r.upgradeOpenLook("upgrade part 2", true, true)
	})
	r.part(3, "the same, the tester held by the operator's suspension, which the operator lifts after the boot", r.upgradeHeldLook)
	if !r.base {
		r.part(4, "a stored look id made for look 1 while the check is open at look 2: the way back kept it while the checked root was out of the workflow, and the release before's wake then started look 2", r.upgradeRolledBackLook)
	}
	r.part(5, "a look that ended at the operator's hold, the tester held and the text of an earlier look pending; the operator resumes the tester after the boot, and the next look's wake comes due", r.upgradeEndedLook)
	r.part(6, "first order: an open look with no look id, and a start row of that look the release before queued, which waits on `a delivery is already pending` and runs after the boot's row", func() {
		r.upgradeOlderStart("older-row-after ", time.Hour)
	})
	r.part(6, "reverse order: the same, the older start row running before the boot's row", func() {
		r.upgradeOlderStart("older-row-first ", -time.Minute)
	})
	if !r.base {
		r.part(7, "a check a later merge replaced while the release before ran: the new check open at look 1, the stored look id made for look 1 of the replaced check", r.upgradeReplacedCheck)
	}
	r.part(8, "an open look with no look id whose pending text a daemon before cybermonklabs/legion#5463 wrote, with no check_opened_at", func() {
		r.upgradeOpenLook("upgrade part 8", false, false)
	})
	if !r.base {
		r.part(9, "the look id the way back kept for look 1 of a root out of the workflow, and this release's text of look 1 pending on the tester in a delivery that holds no look id", r.upgradeStrippedLookTask)
		r.part(10, "a root that runs again, whose tester holds look 1's id from its task; the way back keeps the id, and the boot leaves it to the tester's report", r.upgradeRunningRoot)
	}
}

// lookIDOldContract is the plugin contract of the release before this change, on which each part
// of `upgrade` seeds its tester's claim registered: the number below the one this build gives the
// entry report-gives-its-look-id (api/contract_20261127185859_report-gives-its-look-id.go), read
// from the build's own numbering (api.NumberedEntries), since the entry's constant is a symbol the
// base commit lacks. At the base commit, which has no such entry, it is the number below the base's
// own contract. Either way it is not the running daemon's contract, so the boot's stale-plugin lane
// relaunches a live claim registered on it.
func lookIDOldContract() int {
	for _, entry := range api.NumberedEntries() {
		if entry.Slug == "report-gives-its-look-id" {
			return entry.Number - 1
		}
	}
	return api.DaemonAPIVersion() - 1
}

// lookIDSeed is what a part of `upgrade` writes into the store with no daemon running, as a daemon
// of the release before this change leaves it (seedOlder): task, the text of the tester's pending
// task, in a delivery that holds no look id, and look, the look it is the task of; then, the words
// the part's note gives what that release did after the way back and before it left that task (""
// for nothing the note names); and, at the head, what the check's look-id columns hold after the
// way back. Where stored is not "", the check holds stored, made for look storedLook of the check
// opened at storedOpened: what that release's write of a later merge's check leaves of the check
// it replaced (part 7). Else, where keepsID, the check keeps the look id the way back kept (parts 4
// and 9, which read it, wayBackKept). Else the check holds no look id (seedOlder says where such a
// check comes from).
type lookIDSeed struct {
	task         string
	look         int
	then         string
	keepsID      bool
	stored       string
	storedLook   int
	storedOpened time.Time
}

// olderTask is the text of look n of check as a daemon of the release before this change wrote it:
// workflow's checkTask with no look id, woken naming what woke the look and last saying it is the
// check's last, and, where namesCheck is false, as an earlier daemon wrote it before its task named
// the check's opening time, with no check_opened_at either. Its closing instructions are shortened
// to the one sentence that says what the report gives.
func (f *lookIDFixture) olderTask(check record.ProductionCheck, n int, woken string, last, namesCheck bool) string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s%s: %s.\nRepository %s, pull request #%d, merge commit %s. Look %d, woken by %s.",
		checkTaskLeadText, f.key, f.title, check.Repo, check.Number, check.MergeSHA, n, woken)
	if namesCheck {
		stamp := check.OpenedAt.UTC().Format(time.RFC3339Nano)
		fmt.Fprintf(&text, " The check opened at `%s`: your report names it with check_opened_at `%s`.", stamp, stamp)
	}
	if last {
		text.WriteString(" This is the last look: no look follows.")
	}
	if namesCheck {
		text.WriteString("\nThen report with the legion tool's handoff_complete, check_opened_at from this task and a verdict.")
	} else {
		text.WriteString("\nThen report with the legion tool's handoff_complete and a verdict, committing nothing.")
	}
	return text.String()
}

// lookIDMigration is the version of this change's migration,
// store/migrations/20261127171352_production_check_look_id.up.sql, whose header ends with the way
// back of this release (migrations.WayBack).
const lookIDMigration = 20261127171352

// seedOlder writes seed into the store of the stopped daemon, as a daemon of the release before
// this change leaves it. At the head the way back of this release runs first (wayBack), as an
// operator runs it once every daemon of the database has stopped and before the release before
// starts. Then the check's three look-id columns are left as the release before leaves them. Where
// seed stores one, they hold the look id made for look 1 of the check a later merge replaced (part
// 7), as the release before's write of the later merge's check leaves in place the look id of the
// check it replaced, which holds the boot to whatever look-id columns it finds. Where seed keeps
// the id (keepsID), they hold the id the way back kept (parts 4 and 9, which read it,
// wayBackKept). Else they are null. A check holds no look id in two cases only: the
// release before opened it on a row of production_checks that it inserted, or it was open at the
// first deploy of this release, whose migration leaves the look-id columns null on every row. The
// table has one row for each issue, and the release before names no look-id column, so it leaves
// the look id in place on a row that this release wrote (parts 4 and 7). With the columns null,
// the boot finds an open look of the check with no look id of its own
// (record.ProductionCheck.CurrentLookID is ""). Last, the tester's pending task holds seed's text,
// a task of seed's look of the check, in a delivery that holds no look id
// — a claim that holds none, whose look ended and took its task with it (part 5), is given one —
// and the tester's claim is registered on that release's contract (lookIDOldContract). The base
// commit has none of the look-id columns and no such migration, so there only the task and the
// contract are written.
func (f *lookIDFixture) seedOlder(seed lookIDSeed) {
	f.t.Helper()
	ctx := context.Background()
	token := string(f.token(claim.RoleTester))
	if !f.r.base {
		f.wayBack()
		switch {
		case seed.stored != "":
			f.exec("leave the check with the look id the release before leaves",
				"update production_checks set look_id = $2, look_id_look = $3, look_id_opened_at = $4 where issue = $1",
				f.key, seed.stored, seed.storedLook, seed.storedOpened)
		case !seed.keepsID:
			f.exec("leave the check with no look id, as on a row the release before inserted or at the first deploy of this release",
				"update production_checks set look_id = null, look_id_look = null, look_id_opened_at = null where issue = $1", f.key)
		}
	}
	tag, err := f.st.Pool().Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, check_look, check_opened_at)
		select $1, $2, $3, now(), i.generation, $5, c.opened_at from issues i join production_checks c on c.issue = i.key where i.key = $4
		on conflict (claim_token) do update set task = excluded.task`, token, outboxDeliveryPrefix+"older-"+randomSuffix(f.t), seed.task, f.key, seed.look)
	if err != nil || tag.RowsAffected() != 1 {
		f.t.Fatalf("leave the text of look %d the release before wrote pending on the tester's claim: %d rows, %v", seed.look, tag.RowsAffected(), err)
	}
	f.exec("register the tester's claim on the contract of the release before", "update claims set plugin_contract = $2 where token = $1", token, lookIDOldContract())
	if !f.r.base {
		f.exec("take the look id off the tester's pending task", "update pending_task_deliveries set look_id = null where claim_token = $1", token)
	}
}

// wayBack runs the way back of this release for the fixture's project with no daemon running, its
// key in place of <PROJECT>, as an operator runs it: the block that ends the header of this
// change's migration (migrations.WayBack), whose guide section, "Rolling back the release whose
// look report gives its look id" (docs/kubernetes.md), says what it does. The base commit has no
// such migration.
func (f *lookIDFixture) wayBack() {
	f.t.Helper()
	wayBack, err := migrations.WayBack(lookIDMigration)
	if err != nil {
		f.t.Fatalf("read the way back of migration %d: %v", lookIDMigration, err)
	}
	f.exec("run the way back of this release for the fixture's project", strings.ReplaceAll(wayBack, "<PROJECT>", f.cfg.Project))
}

// wayBackKept fails the running part at way-back-kept unless, with the daemon stopped once the way
// back has run (seedOlder), the checked issue's check still holds id as the look id made for look
// n of the check opened at opened, as the way back leaves an open check (wayBack names where that
// is said): parts 4 and 9 stand on that with their root out of the workflow.
func (r *lookIDLive) wayBackKept(f *lookIDFixture, n int, opened time.Time, id string) {
	r.t.Helper()
	if stored := f.storedLookID(n, opened); stored != id {
		r.fails("way-back-kept", "after the way back the check holds as look %d's id the id that look's task gave: %t, want true, since the way back keeps the look id of every open check",
			n, stored == id)
	}
}

// wakeReason is what the checked issue's wake is (record.CheckWakeDue.Reason, read from its
// check_wake row): the words the task of the look that wake starts is woken by.
func (f *lookIDFixture) wakeReason() string {
	f.t.Helper()
	var reason string
	if err := f.st.Pool().QueryRow(context.Background(), `select payload->>'reason' from outbox where issue = $1 and kind = $2 order by id desc limit 1`,
		f.key, string(record.OutboxKindCheckWake)).Scan(&reason); err != nil {
		f.t.Fatalf("read the reason of the wake row of %s: %v", f.key, err)
	}
	return reason
}

// olderLookStarts starts look n of check with no daemon running, as a daemon of the release before
// this change starts it when the check's wake comes due while the issue is out of the workflow
// (workflow's checkWakeDue and startLook, short of the look id): the check_wake row it ran is
// spent; the check counts look n open from now, woken by that wake (wakeNamed, `check wake`, with
// reason) and with no wake standing; and the look's bound row is queued a look bound out
// (workflow.CheckLookBound). The look id the check holds stays as it stood: that release names no
// look-id column, so its start of the look leaves the id in place. The task the look's start gave
// the tester is seedOlder's.
func (f *lookIDFixture) olderLookStarts(check record.ProductionCheck, n int, reason string) {
	f.t.Helper()
	f.exec("spend the check's wake row, which started the look", "delete from outbox where issue = $1 and kind = $2", f.key, string(record.OutboxKindCheckWake))
	f.exec(fmt.Sprintf("count look %d of the check open from now", n), `update production_checks set looks = $2, look_open = true, look_wake_at = now(),
		last_wake_kind = 'check wake', last_wake_detail = $3, last_wake_at = now(), wake_at = null, last = false where issue = $1`, f.key, n, reason)
	f.write(fmt.Sprintf("the bound row of look %d", n), func(tx pgx.Tx) error {
		return f.records.Enqueue(context.Background(), tx, mustOutboxRow(f.t, f.key, record.CheckLook{Look: n, OpenedAt: check.OpenedAt}, time.Now().Add(workflow.CheckLookBound)))
	})
}

// bootOver stops the fixture's daemon, writes seed with no daemon running (seedOlder) and then what
// more the part writes (more, when not nil), and boots the daemon at the head on that store. The
// stop comes once every start row of the checked issue has run (awaitStartsRan), so the store
// holds no row under the stopped daemon's lease, which would hold back each row of the issue until
// the lease ran out. The pane tester's agent played, when not nil, is not reconnected: the claim's
// next process is the one the boot's stale-plugin lane relaunches, or, for a claim the operator
// holds, the one the operator's resume launches, or, for a claim the look's end suspended, the one
// the boot's new start of the look resumes (cameUpOnThisContract). It answers the moment of the
// stop, after which a part reads what reached a connection, and how many launches of the tester the
// run had made by then.
func (r *lookIDLive) bootOver(f *lookIDFixture, tester *lookIDAgent, seed lookIDSeed, more func()) (time.Time, int) {
	r.t.Helper()
	f.awaitStartsRan("every start row of the checked issue to run before the daemon's stop")
	launched, stopped := f.launches(claim.RoleTester), time.Now()
	f.restartWith(func() {
		if tester != nil {
			f.stopAgent(tester)
		}
		f.seedOlder(seed)
		if more != nil {
			more()
		}
	})
	way, then, stored := "", "", ""
	if !r.base {
		way = "the way back ran and "
	}
	if seed.then != "" {
		then = " " + seed.then
	}
	if seed.stored != "" && !r.base {
		stored = fmt.Sprintf(", and the check holds the look id made for look %d of the check opened %s", seed.storedLook, seed.storedOpened.UTC().Format(time.RFC3339Nano))
	}
	r.note("with the daemon stopped, %sthe store was left as the release before leaves it%s: the tester's pending task a text of look %d that release left there, in a delivery that holds no look id, and its claim registered on contract %d%s; the daemon then booted on it",
		way, then, seed.look, lookIDOldContract(), stored)
	return stopped, launched
}

// awaitStartsRan waits for the outbox to hold no start row of the checked issue.
func (f *lookIDFixture) awaitStartsRan(what string) {
	f.t.Helper()
	testwait.WithinPolling(f.t, lookIDWait, 100*time.Millisecond, what, func() bool {
		var left int
		if err := f.st.Pool().QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = $2 and payload->>'op' = 'start'`,
			f.key, string(record.OutboxKindSupervise)).Scan(&left); err != nil {
			f.t.Fatalf("count the start rows of %s: %v", f.key, err)
		}
		return left == 0
	})
}

// lookGivenAnID fails the running part at look-id unless the boot left the check open at look n,
// opened at opened, with the look open, and, at the head, gave that look a look id of its own: the
// store holds one made for look n of that check (storedLookID, by SQL), and not stale, the id the
// seed left. The base commit has no look-id column, so there only the check's own fields are read.
func (r *lookIDLive) lookGivenAnID(f *lookIDFixture, n int, opened time.Time, stale string) {
	r.t.Helper()
	var check record.ProductionCheck
	var id string
	if !testwait.Holds(lookIDWait, 100*time.Millisecond, func() bool {
		check = f.check()
		if !r.base {
			id = f.storedLookID(n, opened)
		}
		return check.Open() && check.Looks == n && check.LookOpen && check.OpenedAt.Equal(opened) && (r.base || (id != "" && id != stale))
	}) {
		r.fails("look-id", "after the boot the check is open: %t, at look %d (open: %t), opened %s, and holds a look id of its own made for look %d of that check: %t; want it open at look %d, the look open, with such an id",
			check.Open(), check.Looks, check.LookOpen, check.OpenedAt.UTC().Format(time.RFC3339Nano), n, id != "" && id != stale, n)
	}
	if r.base {
		r.note("after the boot the check is open at look %d, the look open, as the seed left it", n)
		return
	}
	r.note("the boot gave look %d a look id of its own, made for that look of the check, and kept the check open at look %d, the look open", n, n)
}

// cameUpOnThisContract is the tester the release before registered, come up again: the boot's
// stale-plugin lane relaunches its session (supervise's refreshPlugin), or, for a claim the
// operator held, the operator's resume launches it, or, for a claim the look's end suspended, the
// boot's new start of the look resumes it (what names which), and its agent registers on the
// running daemon's contract (register) and reports ready (resume). It answers that agent.
func (r *lookIDLive) cameUpOnThisContract(f *lookIDFixture, launched int, what string) *lookIDAgent {
	r.t.Helper()
	a := f.resume(claim.RoleTester, "ses_tester", launched, what)
	if got := f.claimView(claim.RoleTester).PluginContract; got != api.DaemonAPIVersion() {
		r.t.Fatalf("the tester's agent came up registered on contract %d, want this daemon's %d", got, api.DaemonAPIVersion())
	}
	r.note("the tester, which the release before registered on contract %d, came up registered on this daemon's contract %d", lookIDOldContract(), api.DaemonAPIVersion())
	return a
}

// notOlder fails the running part at who+"seeded-text" when text, a prompt that reached a's
// connection (how says when), carries one of older, the texts the release before left.
func (r *lookIDLive) notOlder(who string, a *lookIDAgent, text string, older []string, how string) {
	r.t.Helper()
	for _, left := range older {
		if strings.Contains(text, left) {
			r.fails(who+"seeded-text", "the text the release before left reached the connection of the %s %s: %s", a.role, how, firstLine(text))
		}
	}
}

// noOlderText fails the running part at who+"seeded-text" when one of older, the texts the release
// before left, reached the connection of one of agents after since, the daemon's stop, answered
// busy or not.
func (r *lookIDLive) noOlderText(who string, since time.Time, older []string, agents ...*lookIDAgent) {
	r.t.Helper()
	for _, a := range agents {
		for _, got := range a.since(since) {
			r.notOlder(who, a, got.prompt.Message, older,
				fmt.Sprintf("%s after the daemon's stop (answered busy: %t)", got.at.Sub(since).Round(time.Millisecond), got.busy))
		}
	}
	r.note("no text the release before left reached a connection after the daemon's stop")
}

// workNoLooksTask is the agent taking and working prompt, a task that is no look's — the daemon's
// own record of what the last let-go of the process ended (supervise's queueRelaunch, which the
// ready after a relaunch or a resume delivers) — in a turn of its own, as an agent does.
func (r *lookIDLive) workNoLooksTask(a *lookIDAgent, prompt shimwire.Prompt) {
	r.t.Helper()
	a.ack(prompt)
	a.begin(prompt.ID)
	a.finish()
	r.note("the %s took and worked a task that is no look's: %s", a.role, firstLine(prompt.Message))
}

// upgradedLook waits for the task of look n of the check opened at opened to reach a's connection
// after the boot, and holds it to what the step passes on (ownLook): at the head it gives the look
// id the store holds for that look, not stale, the id the seed left, and it is the claim's pending
// task. A text the release before left (older) must never come: the part fails at who+"seeded-text"
// the moment one reaches the connection, which is the base's failure once the claim is ready, and at
// who+"upgraded-task" at the task of any other look, or when look n's does not come. A task that is
// no look's the agent takes and works (workNoLooksTask). It answers the prompt and what the agent
// read of it.
func (r *lookIDLive) upgradedLook(f *lookIDFixture, a *lookIDAgent, n int, opened time.Time, who, stale string, older ...string) (shimwire.Prompt, lookRead) {
	r.t.Helper()
	var ended []string
	if stale != "" {
		ended = []string{stale}
	}
	for deadline := time.Now().Add(lookIDWait); time.Now().Before(deadline); {
		prompt, came := a.next(time.Second)
		if !came {
			continue
		}
		r.notOlder(who, a, prompt.Message, older, "after its ready")
		switch {
		case isLook(prompt.Message, n, opened):
			return prompt, r.ownLook(f, a, prompt, n, opened, ended)
		case strings.Contains(prompt.Message, checkTaskLeadText):
			r.fails(who+"upgraded-task", "the %s was sent the task of another look, %q, want look %d's of the check opened %s",
				a.role, firstLine(prompt.Message), n, opened.UTC().Format(time.RFC3339Nano))
		default:
			r.workNoLooksTask(a, prompt)
		}
	}
	held := f.claimView(a.role)
	r.fails(who+"upgraded-task", "the task of look %d did not reach the %s's connection within %s; its claim is %s holding %s",
		n, a.role, lookIDWait, held.State, describeHeld(held.Pending))
	return shimwire.Prompt{}, lookRead{}
}

// olderTextStaysAway reads a's connection for lookIDQuietSends of the daemon's send deadlines after
// the agent came up while no look is open, and fails the running part at seeded-text when a text the
// release before left (older) reaches it, which is the base's failure once the claim is ready, and
// at quiet when the task of any look does. A task that is no look's the agent takes and works.
func (r *lookIDLive) olderTextStaysAway(f *lookIDFixture, a *lookIDAgent, older ...string) {
	r.t.Helper()
	quiet := lookIDQuietSends * f.cfg.WorkerRPCTimeout
	until := time.Now().Add(quiet)
	for left := time.Until(until); left > 0; left = time.Until(until) {
		prompt, came := a.next(left)
		if !came {
			break
		}
		r.notOlder("", a, prompt.Message, older, "after its ready")
		if strings.Contains(prompt.Message, checkTaskLeadText) {
			r.fails("quiet", "the %s was sent the task of a look while none is open: %s", a.role, firstLine(prompt.Message))
		}
		r.workNoLooksTask(a, prompt)
	}
	r.note("no task of a look reached the tester's connection in the %s after it came up", quiet)
}

// reportUpgraded is the report of the look the boot gave an id, from the turn of its task: verdict,
// with check_opened_at and the look id that task gave (read), and for a pending a wake 30 minutes
// out. The part fails at report unless it is answered 200; the turn then ends. It answers the wake,
// nil for a verdict that names none.
func (r *lookIDLive) reportUpgraded(f *lookIDFixture, a *lookIDAgent, read lookRead, verdict string) *time.Time {
	r.t.Helper()
	var wake *time.Time
	if verdict == "pending" {
		at := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
		wake = &at
	}
	status, failure := a.report(read.opened, read.id, verdict, wake)
	r.note("in that task's turn the tester reported %s with check_opened_at %s and the look id the task gave: %d %s %s",
		verdict, read.opened.UTC().Format(time.RFC3339Nano), status, failure.Code, failure.Error)
	if status != http.StatusOK {
		r.fails("report", "the %s with check_opened_at and the look id the task gave = %d %s %s, want 200", verdict, status, failure.Code, failure.Error)
	}
	f.surfaces("after the report of the look the boot gave an id")
	a.finish()
	return wake
}

// refusedOtherID fails the running part at assertion unless a pass with check_opened_at from the
// task of the look the boot gave an id (read) and other, the look id the seed left (what names it),
// is answered 409 CHECK_LOOK_NOT_OPEN naming look n as the current look and records nothing: the
// check is as it was before the call, and the intake processed no report of it (reports).
func (r *lookIDLive) refusedOtherID(f *lookIDFixture, a *lookIDAgent, assertion string, read lookRead, other string, n int, what string) {
	r.t.Helper()
	before, reports := f.check(), f.reports()
	status, failure := a.report(read.opened, other, "pass", nil)
	r.note("a pass with check_opened_at %s and %s: %d %s %q", read.opened.UTC().Format(time.RFC3339Nano), what, status, failure.Code, failure.Error)
	if status != http.StatusConflict || failure.Code != "CHECK_LOOK_NOT_OPEN" || !strings.Contains(failure.Error, fmt.Sprintf("The current look of the check is look %d.", n)) {
		r.fails(assertion, "the pass with %s = %d %s %q, want 409 CHECK_LOOK_NOT_OPEN naming look %d as the current look", what, status, failure.Code, failure.Error, n)
	}
	if after, processed := f.check(), f.reports(); !reflect.DeepEqual(after, before) || processed != reports {
		r.fails(assertion, "the refused pass changed the check from %+v to %+v, and the intake processed %d reports of the check, want %d", before, after, processed, reports)
	}
	r.note("the check is the same as before that call, and the intake processed no report of it")
}

// upgradeOpenLook is parts 1, 2 and 8: look 1 opens and its task reaches the idle tester, whose
// agent does not answer it. With the daemon stopped, the store is left as the release before leaves
// it (bootOver), the check with no look id (seedOlder), the tester's pending task the text of
// look 1 that release wrote — with check_opened_at (namesCheck), as the base writes it, or with
// none, as an earlier daemon wrote it before its task named the check's opening time — and, where
// last, the look marked the last of its check. Once the boot has given the look an id, the
// stale-plugin lane relaunches the tester, its agent comes up on this change's contract, and the
// look's task with that id reaches it; the report it gives is answered 200, a pass for a last
// look, which closes the check as recorded, and a pending otherwise.
func (r *lookIDLive) upgradeOpenLook(where string, last, namesCheck bool) {
	f, tester, check := r.firstLook(where, nil)
	tester.prompt("the look-1 task", checkTaskLeadText)
	older := f.olderTask(check, 1, "the merge", last, namesCheck)
	stopped, launched := r.bootOver(f, tester, lookIDSeed{task: older, look: 1}, func() {
		if last {
			f.exec("mark look 1 the last look of its check", "update production_checks set last = true where issue = $1", f.key)
		}
	})
	r.lookGivenAnID(f, 1, check.OpenedAt, "")
	a := r.cameUpOnThisContract(f, launched, "the stale-plugin lane to relaunch the tester the release before registered")
	prompt, read := r.upgradedLook(f, a, 1, check.OpenedAt, "", "", older)
	a.ack(prompt)
	a.begin(prompt.ID)
	if !last {
		r.reportUpgraded(f, a, read, "pending")
	} else {
		r.reportUpgraded(f, a, read, "pass")
		closed := f.awaitCheck("the pass to close the check", func(c record.ProductionCheck) bool { return !c.Open() })
		if closed.Outcome != record.CheckRecorded {
			r.fails("closed-pass", "the check closed as %s, want %s, the pass", closed.Outcome, record.CheckRecorded)
		}
		r.note("the check closed as %s, the pass", closed.Outcome)
	}
	r.noOlderText("", stopped, []string{older}, tester, a)
}

// upgradeHeldLook is part 3: look 1's task reaches the tester, which the operator suspends in a
// turn of its own (holdInATurn). With the daemon stopped, the store is left as the release before
// leaves it, the check with no look id (seedOlder), and the held tester's pending task that
// release's text of look 1. The boot gives the look an id and a start row of its new task, which
// meets the operator's hold and waits on it, the look kept open (reissueWaitsOnTheHold). The
// operator then resumes the tester, its agent comes up on this change's contract, the waiting row
// is brought due, and the look's task with its id reaches the agent; a pending with that id is
// answered 200.
func (r *lookIDLive) upgradeHeldLook() {
	f, tester, check := r.firstLook("upgrade part 3", nil)
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	r.holdInATurn(f, tester, task)
	older := f.olderTask(check, 1, "the merge", false, true)
	stopped, launched := r.bootOver(f, nil, lookIDSeed{task: older, look: 1}, nil)
	r.lookGivenAnID(f, 1, check.OpenedAt, "")
	if !r.base {
		r.reissueWaitsOnTheHold(f, check.OpenedAt)
	}
	f.operate(claim.RoleTester, "resume")
	a := r.cameUpOnThisContract(f, launched, "the operator's resume to launch the tester again")
	if !r.base {
		f.exec("bring the boot's start row of look 1 due now", `update outbox set next_at = now() where issue = $1 and kind = $2
			and payload->>'op' = 'start' and payload->>'lookReissue' = 'true'`, f.key, string(record.OutboxKindSupervise))
	}
	prompt, read := r.upgradedLook(f, a, 1, check.OpenedAt, "", "", older)
	a.ack(prompt)
	a.begin(prompt.ID)
	r.reportUpgraded(f, a, read, "pending")
	r.noOlderText("", stopped, []string{older}, tester, a)
}

// reissueWaitsOnTheHold fails the running part at hold-waits unless the boot's start row of look 1,
// the one marked the new task of an open look (lookReissue), meets the operator's hold and waits on
// its backoff as a phase start does, while the look stays open with the id the boot gave it and the
// tester stays held: a start of a new look ends its look on that hold instead (the outbox's
// checkLookHeld). The base commit queues no such row.
func (r *lookIDLive) reissueWaitsOnTheHold(f *lookIDFixture, opened time.Time) {
	r.t.Helper()
	given := f.storedLookID(1, opened)
	var attempts int
	var why string
	if !testwait.Holds(lookIDWait, 100*time.Millisecond, func() bool {
		err := f.st.Pool().QueryRow(context.Background(), `select attempts, last_error from outbox where issue = $1 and kind = $2
			and payload->>'op' = 'start' and payload->>'lookReissue' = 'true'`, f.key, string(record.OutboxKindSupervise)).Scan(&attempts, &why)
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		if err != nil {
			f.t.Fatalf("read the boot's start row of look 1 of %s: %v", f.key, err)
		}
		return attempts > 0 && why != ""
	}) {
		r.fails("hold-waits", "the boot's start row of look 1 did not meet the operator's hold and wait within %s: %d attempts, last error %q", lookIDWait, attempts, why)
	}
	check, held := f.check(), f.claimView(claim.RoleTester)
	if kept := f.storedLookID(1, opened); !check.Open() || check.Looks != 1 || !check.LookOpen || kept == "" || kept != given ||
		held.State != string(supervise.StateSuspended) || held.SuspendedBy != string(supervise.SuspendedByOperator) {
		r.fails("hold-waits", "while the boot's start row waits the check is open: %t, at look %d (open: %t), with the look id the boot gave it: %t, and the tester is %s by %q; want look 1 open with that id and the tester held by the operator",
			check.Open(), check.Looks, check.LookOpen, kept != "" && kept == given, held.State, held.SuspendedBy)
	}
	r.note("the boot's start row of look 1 met the operator's hold and waits (%d attempts, last error %q); look 1 stays open with the id the boot gave it, and the tester stays held", attempts, why)
}

// upgradeRolledBackLook is part 4, which the base does not run, on a root's check: look 1's task
// reaches the tester, whose turn reports it pending with a wake and its look id. The root stays
// out of the workflow, as its merge left it. With the daemon stopped, the way back runs and keeps
// look 1's id (wayBackKept reads that it did), and the seed keeps that id (seedOlder, keepsID);
// the store is then left as the release before leaves it once the wake starts look 2: the check
// open at look 2 and still holding the id made for look 1, since that release names no look-id
// column, so its start of look 2 leaves the id in place (olderLookStarts), and the tester's
// pending task that release's text of look 2. The boot gives look 2 an id of its own and a new
// start, which resumes the tester the pending report suspended, on this change's contract, and
// look 2's task reaches it; from its turn a pass with look 1's id is answered 409
// CHECK_LOOK_NOT_OPEN, and a pending with look 2's new id 200.
func (r *lookIDLive) upgradeRolledBackLook() {
	f, tester, check := r.rootLook("upgrade part 4", nil)
	first := tester.prompt("the look-1 task", checkTaskLeadText)
	tester.ack(first)
	tester.begin(first.ID)
	one := tester.readLook(first)
	wake := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
	if status, failure := tester.report(one.opened, one.id, "pending", &wake); status != http.StatusOK {
		r.t.Fatalf("the tester's report of look 1 = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	tester.finish()
	r.note("look 1's task reached the tester, whose turn reported it pending with wake %s and its look id: 200", wake.Format(time.RFC3339Nano))
	f.awaitSuspended(claim.RoleTester, "the pending report to suspend the tester")
	f.stopAgent(tester)
	r.note("%s stays out of the workflow, as its merge left it", f.key)
	reason := f.wakeReason()
	older := f.olderTask(check, 2, "the wake named at the last look — "+reason, false, true)
	stopped, launched := r.bootOver(f, nil, lookIDSeed{task: older, look: 2,
		then: "once its wake started look 2 on the check, which holds the look id made for look 1 that the way back kept while the root was out of the workflow", keepsID: true},
		func() {
			r.wayBackKept(f, 1, check.OpenedAt, one.id)
			f.olderLookStarts(check, 2, reason)
		})
	r.lookGivenAnID(f, 2, check.OpenedAt, one.id)
	a := r.cameUpOnThisContract(f, launched, "the boot's new start of look 2 to resume the tester")
	prompt, read := r.upgradedLook(f, a, 2, check.OpenedAt, "", one.id, older)
	a.ack(prompt)
	a.begin(prompt.ID)
	r.refusedOtherID(f, a, "old-id-refused", read, one.id, 2, "look 1's id, the one the store held for look 1")
	r.reportUpgraded(f, a, read, "pending")
	r.noOlderText("", stopped, []string{older}, tester, a)
}

// upgradeStrippedLookTask is part 9, which the base does not run: on a root's check, the root out
// of the workflow as its merge left it, the tester holds this release's task of look 1, which
// gives the look id this release made for it, in a delivery that holds no look id, and so holds
// that id: a state a rollback can leave (workflow.Engine.RepairOpenChecks says how). A start row
// gives its task once (supervise.Claim.DeliveredRows), so the run reaches the same store this way:
// this release runs the row before the daemon stops, and its task reaches the tester's connection
// then, and the tester does not answer it; with the daemon stopped the way back runs and keeps
// look 1's id (seedOlder, wayBackKept), and the seed keeps that id (keepsID) and writes the
// claim's delivery as the release before writes it, with no look id. The boot gives look 1 a new
// look id and a new start (lookTaskGivesAnotherID, whose reason its log line gives), and the
// look's task with the new id reaches the tester relaunched on this change's contract; from its
// turn a pass with the kept id is answered 409 CHECK_LOOK_NOT_OPEN and records nothing, and a
// pending with the new id is answered 200 and recorded against look 1.
func (r *lookIDLive) upgradeStrippedLookTask() {
	f, tester, check := r.rootLook("upgrade part 9", nil)
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	kept := tester.readLook(task)
	given, _ := f.pendingTask(claim.RoleTester)
	if !isLook(given, 1, check.OpenedAt) || !strings.Contains(given, kept.id) {
		r.t.Fatalf("the tester's claim holds %q, want this release's task of look 1, which gives the look id that reached the tester", firstLine(given))
	}
	r.note("look 1's task, which gives the look id this release made for it, reached the tester, which does not answer it")
	r.note("%s stays out of the workflow, as its merge left it", f.key)
	stopped, launched := r.bootOver(f, tester, lookIDSeed{task: given, look: 1,
		then: "when it runs a start row of look 1 that this release queued and the way back stripped of its look id, the root out of the workflow, the row's text giving the id the way back kept", keepsID: true},
		func() { r.wayBackKept(f, 1, check.OpenedAt, kept.id) })
	r.lookGivenAnID(f, 1, check.OpenedAt, kept.id)
	why := "none"
	if at := f.loggedIndex(0, func(logged loggedRecord) bool {
		return logged.Message == "workflow: the boot gave an open production check look an id of its own and a new task" && logged.Attrs["issue"] == f.key
	}); at >= 0 {
		why = fmt.Sprint(f.loggedAt(at).Attrs["why"])
	}
	r.note("the boot's log line of that new id says why: %s", why)
	a := r.cameUpOnThisContract(f, launched, "the stale-plugin lane to relaunch the tester the release before registered")
	prompt, read := r.upgradedLook(f, a, 1, check.OpenedAt, "", kept.id, given)
	a.ack(prompt)
	a.begin(prompt.ID)
	r.refusedOtherID(f, a, "old-id-refused", read, kept.id, 1, "the look id this release made for look 1, which the way back kept")
	wake := r.reportUpgraded(f, a, read, "pending")
	r.pendingRecorded(f, *wake)
	r.noOlderText("", stopped, []string{given}, tester, a)
}

// upgradeRunningRoot is part 10, which the base does not run: look 1's task reaches the root's
// tester, which answers its send late, and while that late turn runs a person sets the root back
// to todo inside its tree's linger, which re-admits it (lateTurnAcrossTheRerun), so the tester
// holds look 1's id from a task this release delivered while the root runs again. With the daemon
// stopped the way back runs (wayBack), and the store is otherwise left as the release before
// leaves it while the root runs: no look starts and nothing else changes. The daemon boots on it,
// and the tester's pane connects again. The part passes when no task of a look reaches the
// tester's connection for lookIDQuietSends of the daemon's send deadlines after the boot (silent);
// when the tester's report of look 1 from that session, pending with that id, is answered 200 and
// recorded against look 1 (pendingFromThatSession, pendingRecorded); and when the store held that
// id as look 1's, the current look's, once the daemon had booted (kept-id). The report is judged
// before the stored id is, so a way back that clears the id fails the part at the report it loses,
// as c-upgrade's negative control at the head names: with a statement added to the way back's
// block that clears the look id of every open check of the project, the boot gives look 1 a new
// id, whose start the fence finishes unrun while the root runs, and this report is refused 409
// CHECK_LOOK_NOT_OPEN.
func (r *lookIDLive) upgradeRunningRoot() {
	f, tester, check := r.rootLook("upgrade part 10", nil)
	read, _ := r.lateTurnAcrossTheRerun(f, tester)
	f.awaitStartsRan("every start row of the checked issue to run before the daemon's stop")
	stopped := time.Now()
	f.restartWith(f.wayBack)
	booted := time.Now()
	r.note("with the daemon stopped, the way back ran while %s runs again, and nothing else of the store changed, as the release before leaves it while the root runs and starts no look; the daemon then booted on it, and the tester's pane connected again", f.key)
	after, stored := f.check(), f.storedLookID(1, check.OpenedAt)
	r.note("after the boot the check is open: %t, at look %d (open: %t), and holds as look 1's id the id the tester's task gave: %t", after.Open(), after.Looks, after.LookOpen, stored == read.id)
	quiet := lookIDQuietSends * f.cfg.WorkerRPCTimeout
	time.Sleep(time.Until(booted.Add(quiet)))
	r.silent("silent", stopped, 0, time.Time{}, tester)
	r.note("no task of a look reached the tester's connection in the %s after the boot, while %s runs again", quiet, f.key)
	wake := r.pendingFromThatSession(f, tester, read, booted, false, false)
	r.pendingRecorded(f, wake)
	if !after.Open() || after.Looks != 1 || !after.LookOpen || stored != read.id {
		r.fails("kept-id", "after the boot the check was open: %t, at look %d (open: %t), and held as look 1's id the id the tester's task gave: %t; want look 1 open with that id",
			after.Open(), after.Looks, after.LookOpen, stored == read.id)
	}
	r.note("the store held, after the boot, the look id the tester's task gave as the id of look 1, the current look")
}

// upgradeEndedLook is part 5: look 1's task reaches the tester, which the operator suspends in a
// turn of its own (holdInATurn); look 1 ends at its bound and look 2's start meets the operator's
// hold, which ends look 2 (secondLookMeetsTheHold). With the daemon stopped, the store is left as
// the release before leaves it, the check with no look id (seedOlder), and the held tester's
// pending task that release's text of look 1. After the boot the operator resumes the tester,
// which comes up on this change's contract, and no text of a look reaches it (olderTextStaysAway);
// the wake of look 3, brought due now, starts look 3, whose task with its look id reaches the
// tester, and a pending with that id is answered 200.
func (r *lookIDLive) upgradeEndedLook() {
	f, tester, check := r.firstLook("upgrade part 5", nil)
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	r.holdInATurn(f, tester, task)
	r.secondLookMeetsTheHold(f)
	older := f.olderTask(check, 1, "the merge", false, true)
	stopped, launched := r.bootOver(f, nil, lookIDSeed{task: older, look: 1}, nil)
	f.operate(claim.RoleTester, "resume")
	a := r.cameUpOnThisContract(f, launched, "the operator's resume to launch the tester again")
	r.olderTextStaysAway(f, a, older)
	f.wakeComesDue(3)
	prompt, read := r.upgradedLook(f, a, 3, check.OpenedAt, "", "", older)
	a.ack(prompt)
	a.begin(prompt.ID)
	r.reportUpgraded(f, a, read, "pending")
	r.noOlderText("", stopped, []string{older}, a)
}

// upgradeOlderStart is the two halves of part 6, each named by who: look 1's task reaches the idle
// tester, whose agent does not answer it. With the daemon stopped, the store is left as the release
// before leaves it, the check with no look id (seedOlder), the tester's pending task that
// release's text of look 1, and beside it that release's own start row of look 1, which waits on
// that task (olderStart), due at due from the stop: an hour out, so the boot's start row runs
// first and the older row is brought due once the boot's task has reached the tester, or a minute
// before, so the older row runs first. The boot gives the look an id, the stale-plugin lane
// relaunches the tester, and the boot's task reaches it; the older row finishes without acting, on
// the side of the boot's row the half names (olderStartRanInOrder), leaving the boot's task the
// claim's; and a pending with the look's id is answered 200.
func (r *lookIDLive) upgradeOlderStart(who string, due time.Duration) {
	f, tester, check := r.firstLook("upgrade part 6, "+strings.TrimSpace(who), nil)
	tester.prompt("the look-1 task", checkTaskLeadText)
	older := f.olderTask(check, 1, "the merge", false, true)
	var row int64
	stopped, launched := r.bootOver(f, tester, lookIDSeed{task: older, look: 1}, func() {
		row = f.olderStart(check, older, due)
		// The tester's pin (INFRA-3518) of the race that failed the first half of this part once in 9
		// runs at 537d4ff1: the outbox logs that the older row finishes before it deletes the row, so a
		// read made at the log line can find the row still in the outbox. A delete of this row that
		// takes a second, as a slow commit can, makes that order hold in every run of that half.
		if due <= 0 {
			return
		}
		f.exec("make the delete of the older start row take a second", `create or replace function look_id_slow_delete() returns trigger
			language plpgsql as $$ begin perform pg_sleep(1); return old; end $$`)
		f.exec("make the delete of the older start row take a second", fmt.Sprintf(
			"create trigger look_id_slow_delete_%d before delete on outbox for each row when (old.id = %d) execute function look_id_slow_delete()", row, row))
	})
	r.lookGivenAnID(f, 1, check.OpenedAt, "")
	a := r.cameUpOnThisContract(f, launched, "the stale-plugin lane to relaunch the tester the release before registered")
	prompt, read := r.upgradedLook(f, a, 1, check.OpenedAt, who, "", older)
	if due > 0 {
		f.exec("bring the older start row due now, the boot's row having run", "update outbox set next_at = now() where id = $1", row)
	}
	r.olderStartRanInOrder(f, who, row, prompt.DeliveryID, due < 0)
	if task, lookID := f.pendingTask(claim.RoleTester); !isLook(task, 1, check.OpenedAt) || lookID != read.id {
		r.fails(who+"older-row", "after the older start row ran the tester's claim holds %q, with the look id of the boot's task: %t; want the boot's task",
			firstLine(task), lookID == read.id)
	}
	r.note("the tester's claim still holds the boot's task of look 1")
	a.ack(prompt)
	a.begin(prompt.ID)
	r.reportUpgraded(f, a, read, "pending")
	r.noOlderText(who, stopped, []string{older}, tester, a)
}

// olderStart queues, with no daemon running, the start row of look 1 of check that a daemon of the
// release before queued beside that look's task, and that waits on the task: it carries no look id,
// its task is text, and its last attempt met `a delivery is already pending`. It is due at due from
// now. It answers the row's id.
func (f *lookIDFixture) olderStart(check record.ProductionCheck, text string, due time.Duration) int64 {
	f.t.Helper()
	row := mustOutboxRow(f.t, f.key, record.SuperviseRequest{Op: "start", Tree: f.root, Role: claim.RoleTester, Generation: 1,
		CheckLook: 1, CheckOpenedAt: check.OpenedAt, Task: text}, time.Now().Add(due))
	row.Attempts, row.LastError = 3, "a delivery is already pending"
	var id int64
	f.write("the start row of look 1 the release before queued", func(tx pgx.Tx) error {
		var err error
		id, err = f.records.EnqueueID(context.Background(), tx, row)
		return err
	})
	return id
}

// olderStartRanInOrder fails the running part at who+"older-row" unless the outbox ran the older
// start row id and finished it without acting because it gives no look id (the outbox's
// checkLookHolds), and at who+"order" unless it ran on the side of the boot's own start row the half
// set: before it where first, after it otherwise. The boot's row is the one whose task reached the
// connection, delivery (`outbox:<row>`), and the order is that of the daemon's line of the older
// row's finish and its first line naming that delivery.
func (r *lookIDLive) olderStartRanInOrder(f *lookIDFixture, who string, id int64, delivery string, first bool) {
	r.t.Helper()
	finished := -1
	if !testwait.Holds(lookIDWait, 50*time.Millisecond, func() bool {
		finished = f.loggedIndex(0, func(kept loggedRecord) bool {
			return kept.Message == "production check start finishes without acting" && kept.Attrs["row"] == float64(id)
		})
		return finished >= 0
	}) {
		r.fails(who+"older-row", "the daemon logged no finish of the older start row %d of look 1 within %s", id, lookIDWait)
	}
	// The outbox logs that line in checkLookHolds and deletes the row only once the run has
	// returned, in a transaction of its own (outbox.finish), so the row may still stand at the
	// line: the read waits for it to leave, and the row stands in the outbox only when it is still
	// there at the bound.
	left := true
	testwait.Holds(lookIDWait, 50*time.Millisecond, func() bool {
		if err := f.st.Pool().QueryRow(context.Background(), "select exists (select 1 from outbox where id = $1)", id).Scan(&left); err != nil {
			f.t.Fatalf("read whether the older start row %d stands: %v", id, err)
		}
		return !left
	})
	if why := f.loggedAt(finished).Attrs["why"]; why != "the row gives no look id: an older daemon queued it" || left {
		r.fails(who+"older-row", "the older start row %d finished because %q, and stands in the outbox: %t; want it finished without acting because it gives no look id", id, why, left)
	}
	r.note("the older start row of look 1 finished without acting: it gives no look id")
	// The daemon's first line that names the boot's delivery is supervise's queue's when the boot's
	// task takes the place of the older one, written before the task is sent; with no older task
	// left to replace, the first such line can be one written after the task reached the
	// connection, as at its send's deadline. So the read waits for one, up to the bound.
	boot := -1
	testwait.Holds(lookIDWait, 50*time.Millisecond, func() bool {
		boot = f.loggedIndex(0, func(kept loggedRecord) bool { return kept.Attrs["delivery"] == delivery })
		return boot >= 0
	})
	switch {
	case boot < 0:
		r.fails(who+"order", "the daemon's log names no delivery %s, the boot's start row's task", delivery)
	case first && finished > boot:
		r.fails(who+"order", "the older start row finished at the log's record %d, after the boot's row gave its task at record %d; want it first", finished, boot)
	case !first && finished < boot:
		r.fails(who+"order", "the older start row finished at the log's record %d, before the boot's row gave its task at record %d; want it after", finished, boot)
	}
	if first {
		r.note("the older start row ran before the boot's row, which gave the look's task (%s)", delivery)
		return
	}
	r.note("the older start row ran after the boot's row, which gave the look's task (%s)", delivery)
}

// loggedAt is the record the running daemon logged at index i.
func (f *lookIDFixture) loggedAt(i int) loggedRecord {
	f.logged.mu.Lock()
	defer f.logged.mu.Unlock()
	return f.logged.records[i]
}

// upgradeReplacedCheck is part 7, which the base does not run: look 1's task reaches the tester,
// which answers its send late (lateSend); while that late turn runs a person sets the child back to
// todo under its live tree, and it re-enters; the new run's next pull request merges and the
// person's done closes the child again, which opens the check that replaces the first, whose look-1
// task reaches the tester. With the daemon stopped, the store is left as the release before leaves
// it after its write of that new check: the check still holds the look id made for look 1 of the
// check it replaced, and the tester's pending task is that release's text of the new check's look 1.
// The boot gives that look an id of its own, whose task reaches the tester relaunched on this
// change's contract; from its turn a pass with the replaced check's id is answered 409
// CHECK_LOOK_NOT_OPEN, and a pending with the new id 200.
func (r *lookIDLive) upgradeReplacedCheck() {
	f, tester, check := r.firstLook("upgrade part 7", nil)
	task := tester.prompt("the look-1 task", checkTaskLeadText)
	r.lateSend(f, tester, task)
	replaced := tester.readLook(task)
	f.rerun(f.key)
	tester.finish()
	r.note("while the late turn of look 1's task ran, a person set %s back to todo under its live tree, and it re-entered; the turn then ended", f.key)
	next := f.nextMerge(check)
	r.nextLook(f, tester, 1, next.OpenedAt, replaced.id)
	older := f.olderTask(next, 1, "the merge", false, true)
	stopped, launched := r.bootOver(f, tester, lookIDSeed{task: older, look: 1, stored: replaced.id, storedLook: 1, storedOpened: check.OpenedAt}, nil)
	r.lookGivenAnID(f, 1, next.OpenedAt, replaced.id)
	a := r.cameUpOnThisContract(f, launched, "the stale-plugin lane to relaunch the tester the release before registered")
	prompt, read := r.upgradedLook(f, a, 1, next.OpenedAt, "", replaced.id, older)
	a.ack(prompt)
	a.begin(prompt.ID)
	r.refusedOtherID(f, a, "replaced-id-refused", read, replaced.id, 1, "the look id of look 1 of the replaced check")
	r.reportUpgraded(f, a, read, "pending")
	r.noOlderText("", stopped, []string{older}, tester, a)
}

// ---- step `id hidden` ----------------------------------------------------------------------------

// idHidden is c-id-hidden, at the end of the run: every look id a task that reached a role's
// connection gave in the run (lookIDSeen) is searched for in every body the steps read of GET
// /legion/v1/state and of the record route read_record calls — each read before a look's task is
// sent, after a busy answer to it, after its report and after the next look starts — and in every
// answer to a report, and in the whole of the daemon's log of the run, which every daemon of the
// run appended to. The step passes when none holds one. Its negative control is its own search run
// over the task texts that reached the connections, which must find each id there, so a search
// that finds nothing is one that finds a look id where it is. Its input must hold what a search
// would find a leak in: at least one body read carries check.openedAt, and the log holds the line
// `workflow: production check opened`, which every run writes. At the base commit, whose tasks
// give no look id, there is nothing to search, and the step does not run.
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
	if carriesOpenedAt == 0 {
		r.fails("surface-empty", "none of the %d bodies read carries check.openedAt, so the search read no body that shows an open check", len(reads))
	}
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
	const everyRunLogs = "workflow: production check opened"
	if !bytes.Contains(logged, []byte(everyRunLogs)) {
		r.fails("log-empty", "the daemon's log of the run (%s, %d bytes) holds no line %q, which every run writes, so the search read no log of the run", r.daemonLog, len(logged), everyRunLogs)
	}
	r.note("the daemon's whole log of the run, %d bytes, holds none of them, and holds the line %q every run writes", len(logged), everyRunLogs)
}
