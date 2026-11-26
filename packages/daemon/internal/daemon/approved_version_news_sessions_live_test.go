//go:build e2e

// The driver of part two of dispatch://INFRA-175's c-live: real Oh My Pi sessions, on the role
// prompts this checkout composes and on the base commit's, answering a new version of a design.
// Part one (approved_version_news_live_test.go) proves what the daemon sends; this part proves what
// the prompts make an agent do with a new version: an architect that writes a new version of a
// child's checks, or of a root's spec under `gates.design: off`, tells the workers holding the
// phases the version changes with its own Envoy message and requests no approval, and a worker
// told of an approved version moves its issue back when its work is stale and writes a comment
// when it is current. scripts/e2e/approved-version-news.sh builds this driver with `-tags e2e`.
//
// Every part a session touches is the real one, built from this checkout or the pod's own. Each
// pane is a real `omp --mode rpc` (LEGION_OMP_PATH) under the real `legion worker-shim`, started by
// localruntime_live_test.go, with this checkout's `@sjawhar/pi-envoy` and `@sjawhar/pi-legion`
// packed into an Oh My Pi profile of the run's own, and this checkout's `dispatch` command first on
// its PATH. Its model is the real one: the model gateway ANTHROPIC_BASE_URL names, reached through
// a counting proxy of the driver's own, with the key the file APPROVED_VERSION_NEWS_MODEL_KEY_FILE
// names, which this driver reads at the start of each run and gives to that run's panes' own
// environment and to nothing else. The script can write a new key into that file while part two
// runs, the pod's identity token it mints again before the hour it lasts ends, so a run that starts
// late reads the key the file holds then. The Envoy listener the panes register with, claim their
// roles at and publish to is `packages/envoy/cmd/listener`, on a broker of this driver's own, natsd
// (`packages/envoy/internal/testnats/cmd/natsd`), as scripts/e2e/envoy-session-delivery.sh runs
// them: the listener is built and tested against the checkout's nats-server, which the pinned one
// the script's lib starts is not. Dispatch is the real server built from the checkout, on that
// broker and a database of its own, with header identity and the person `smoke`, as part one runs
// it. Each run boots the whole daemon through run, the function `legion start` calls, on a
// database, a state directory and a Dispatch project of its own, with `review_round_cap` at the
// loader's own default (testConfig), so the state a pane's `legion` tool reads is one the daemon
// API contract describes.
//
// What is stood in for lies outside what a session decides. GitHub is the package's
// repositoriesGitHub, and no session has a GitHub credential. The two Apps' tokens are
// workflowTokenRecorder's placeholders. The daemon's own notices go to the package's
// noticeListener, so a session hears what the daemon sends only as a task on its worker stream,
// and what it hears over Envoy is another session's message. The runtime launches only the claims
// a run seeds: a phase the daemon starts beyond them (the implementer a stale reviewer's move back
// starts) is refused, so no session runs that the step did not ask for.
//
// Each run seeds its trees where production's trees stand mid-run (sessionsRun.seed). The issues,
// their pull requests, gates, slots and lifecycles are written through the store, as part one
// writes them, each issue with the Dispatch status of its phase (sessionsStatus). Each role the
// run seeds is then started through the daemon's own start path, by the function part one's seed
// starts its claims with (startRoles): a root's architect as admission's promotion starts it
// (workflow.Engine.StartArchitect), a child's own architect as the child's first phase start
// starts it for every child the daemon starts (Engine.StartFirstPhase, which queues the plain
// start workflow's childArchitectStart builds), and a member's phase worker as the promotion of a
// mid-phase member starts it (Engine.StartPromotedWorker). Each start of a root's architect or a
// worker raises its role's entry on the role's phases row; the plain start of a child's own
// architect raises none and carries no task. A worker's raise also writes its issue's holder row
// of the told record, from the gate the seed wrote (record.Store.BaselineTold). Each claim is a
// launch the daemon before the boot began and never recorded, which the boot relaunches. That
// daemon's outbox ran each start, and every other row a child's first phase start queued (the row
// that creates its branch, the notices of its move into planning), so the seed takes those rows
// off the queue and writes the claim as the start leaves it: given the start's entry. A role
// whose step needs a task of its own (holderStart, parentStart) holds that task at that entry, in
// place of the start's own task; an answers reviewer holds the start's own stored task with a note
// of the step's before its review round's two sentences (reviewerNote). Any other root architect
// took its start's task before the boot, and holds none, and a child's own architect was given
// none. A root's architect row of the told record comes from its tree's first opening, which the
// seed does not run, so the seed writes it by hand and checks it against the row that opening
// writes. Before the boot the seed checks each claim by part one's rule (standAtTheirEntries): it
// stands at an entry of 1 or more of its role's phases row, given that entry, and it holds its
// task at that entry; a child's own architect stands at no entry, given none, holding no task, as
// its plain start leaves it. A seed that differs fails the run's harness before any pane starts,
// and the failure names the claim or the row, the field and both values.
//
// Two arms. The head arm launches each pane on the prompts the daemon snapshots below its state
// directory at boot (prompts.New). The base arm writes the base commit's prompt files,
// APPROVED_VERSION_NEWS_BASE_PROMPTS, over that snapshot before the first pane starts: a pane
// reads its role prompt from those files at launch (specs.rolePromptPaths,
// runtime.ComposePrompt), so the arm needs no second daemon. The addressing the daemon appends
// after the files is this checkout's in both arms. Each run reads the prompt back from the file
// its pane's own command line names and ends the proof when the arm it ran is not the one it
// meant.
//
// Three steps, each run three times in each arm it runs in, each run on trees of its own; a4 and
// answers run in the head arm alone, and off in both:
//   - a4-the-architect-tells-the-child-reviewer: under root-issues, a root waiting in admitted and
//     its child in reviewing, whose reviewer and own architect are live, with the root's architect
//     live above them; the child's architect is asked to write version 2 of its issue's
//     Verification document. Once it tells its parent of the version, the proof reviews it in the
//     parent's place and form and tells the child's architect, from the parent's own session
//     (sessionsRun.reviewAsParent); a run whose child's architect did not tell its parent stopped
//     before the assertion. It passes when the reviewer's transcript holds an Envoy message naming
//     version 2 and the changed check, the child's architect's transcript holds its envoy_publish
//     to the reviewer's role topic naming the same, and the child has no approval request;
//   - off-the-architect-tells-the-holders: under gates.design off, a root in retro and its child
//     in reviewing, both holders and the child's own architect live; the root's architect
//     is asked to write version 2 of the root's spec. It passes when each holder's transcript
//     holds an Envoy message naming version 2 and the change, and the root architect's transcript
//     holds its envoy_publish to that holder's role topic naming the same;
//   - answers-a-stale-reviewer-moves-back-a-current-one-comments: under root-issues, two roots in
//     reviewing, each with its own architect live and a live reviewer whose start task is the one
//     the daemon's start path stores, with a note of what its pull request changes and that its
//     review read version 1 of the spec; the person approves a version 2 of each spec, written as
//     its root architect's own session, one changing the `## Design` that pull request implements
//     and one only the `## Later work`, which it does not touch. It passes when the first
//     reviewer's request_backward_move names version 2 and moves its issue back, and the second
//     writes a comment of its own on its issue naming version 2 and stays.
//
// The base arms of a4 and answers are retired: each passed 10 of its 10 runs at the merge base,
// main 578c6abb, and its head arm 3 of 3, so no control can separate the arms there. Each has a
// reason of its own.
//   - a4 (at 26392323; dispatch://INFRA-175/artifact/a4-base10-2639232-txt and a4-head3-2639232-txt):
//     main's #4203 gives every started child its own architect, and a4 drives R4 through it; the
//     base commit's own prompts tell that architect to tell its workers of a version once its
//     parent approves it, which is R4.
//   - answers (at e5081eb2; dispatch://INFRA-175/artifact/answers-base10-e5081eb-txt and
//     answers-head3-e5081eb-txt): the rule it tests, that a holder whose work a new version makes
//     stale moves back with a reason naming the version and the change and one whose work it leaves
//     current writes a comment naming the version and why, is written in the daemon's own news task
//     (holderRule, internal/workflow/design_news.go), and both arms run the head's daemon, so the
//     base's role prompts need not carry it.
//
// A run with no arm picked makes both in the head arm alone (sessionsStep.arms), and the script's
// last line names each one's measurement in its place, from the list that holds it (head_only
// in scripts/e2e/approved-version-news.sh). off is the one step whose control can bite at its base,
// where its base arm passed 0 of 3 runs. The command that repeats a measurement is
// APPROVED_VERSION_NEWS_STEP=<a4 or answers> APPROVED_VERSION_NEWS_ARM=base
// APPROVED_VERSION_NEWS_RUNS=10 bash scripts/e2e/approved-version-news.sh.
//
// Three variables the script passes through narrow a run of part two (pickSessions):
// APPROVED_VERSION_NEWS_STEP names one step by its word (a4, off or answers),
// APPROVED_VERSION_NEWS_ARM the one arm (head or base) every step it keeps runs in, a retired base
// arm among them, and APPROVED_VERSION_NEWS_RUNS the count of runs each arm makes of each step, so
// one command measures one arm of one step as often as it asks.
//
// Before its first run the driver prints the plan of the runs the pick makes (sessionsPick.plan):
// each step, the arms it runs in and the count of runs. A pick it refuses prints `plan refused:
// <why>` and fails the test. With -part-two-plan (sessionsPlanOnly) the driver prints the plan and
// makes no run, so the script reads the steps, their arms and the count of runs from the driver's
// own table before the real run, for its counts and the deadline it gives this driver.
//
// Each run prints `== <step> <arm> <n>`, its evidence (the tool calls and the messages, from the
// sessions' own transcripts) and `run <step> <arm> <n>: <outcome> …` with its time and the model
// calls it made, which the script counts. The outcome is a word of sessionsOutcome. PASS and FAIL
// are runs that reached the step's assertion, and the script counts them for the arm. SETUP is a
// run that stopped before its assertion, an architect that did not write the version its task asks
// for among them. HARNESS is a run the harness failed: a fault of the harness before or after the
// step's own outcome, or a session whose transcript holds the plugin's refusal of a daemon answer,
// which ran with a `legion` tool that could not read the daemon. The line prints after the run's
// daemon stops, so a daemon that does not stop cleanly makes the run HARNESS too. Neither SETUP
// nor HARNESS says anything about the prompts the arms differ in, so the script does not count
// them. A HARNESS run ends the proof in either arm, since no count of runs means anything past it.
// A head run that does not pass ends the proof there, whatever its outcome: the head must reach
// its assertion and pass in every run, and every model call after a run that did not would be
// spent for nothing. A base run that ends PASS, FAIL or SETUP is the control's to read, and fails
// nothing.
package daemon

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The script's inputs to this driver. The key file is a path, never the key: the script exports
// nothing of the key, so the builds and the broker the lib starts never hold it. The three that
// pick a run's steps, arms and count of runs are beside the step table
// (approved_version_news_transcript_test.go).
const (
	sessionsKeyFileEnv     = "APPROVED_VERSION_NEWS_MODEL_KEY_FILE"
	sessionsBasePromptsEnv = "APPROVED_VERSION_NEWS_BASE_PROMPTS"
	sessionsBaseCommitEnv  = "APPROVED_VERSION_NEWS_BASE_COMMIT"
	sessionsModelEnv       = "APPROVED_VERSION_NEWS_MODEL"
)

// sessionsPlanOnly is -part-two-plan: the driver prints the plan of the runs the pick makes, and
// makes none of them. The script runs the driver so once before the real run, and reads the plan.
var sessionsPlanOnly = flag.Bool("part-two-plan", false, "print the plan of part two's runs the pick makes, and make none of them")

// sessionsDefaultModel is the model every session runs when the script names none, at
// sessionsThinking: the fleet's own, as the operator's pane overlay names it for every role. The
// prompts are written for the model the fleet runs, so a cheaper one would measure that model
// rather than the prompts.
const sessionsDefaultModel = "claude-opus-5-5"

// sessionsThinking is the thinking level every session runs at, the fleet's own.
const sessionsThinking = "xhigh"

// sessionsRunBound bounds a run from its daemon's boot to its verdict. A run still waiting at its
// bound fails, and its sessions are ended with it, so an agent that wanders spends no more than
// this.
const sessionsRunBound = 8 * time.Minute

// sessionsSettle is how long a session must hold no task and stay out of a turn before its work
// counts as done: Oh My Pi's own plugins can start a turn again just after one ends.
const sessionsSettle = 20 * time.Second

// sessionsGrace is how long a run waits, once the sessions whose work it reads have settled, for a
// message one of them sent to show in its recipient's transcript.
const sessionsGrace = 30 * time.Second

// The words each document is written with and changed to. The id of the one check of A4's child,
// sessionsCheckID, is beside the reads of a transcript (approved_version_news_transcript_test.go).
const (
	sessionsDesign      = "The reader takes one page at a time."
	sessionsDesignAgain = "The reader takes two pages at a time, and keeps the second for the next read."
	sessionsLater       = "A later issue, not this one, adds an export of the pages to CSV."
	sessionsLaterAgain  = "A later issue, not this one, adds an export of the pages to CSV and to JSON."
	sessionsCheck       = "The read answers 200."
	sessionsCheckAgain  = "The read answers 200 with the page it read."
)

// The sentence of each arm's prompt the run reads back from the file a pane's command line names:
// the head's carries it and the base commit's does not. The architect's is its answer to a version
// the daemon sends no news of (prompts/go/architect-common.md); a phase worker's is its answer to a
// news task (prompts/go/worker-common.md).
const (
	sessionsArchitectMark = "Send that news yourself: for a version of a child's documents"
	sessionsWorkerMark    = "A news task tells you of an approved new version of your tree's design."
)

// sessionsLive is what every run of part two shares: the broker, the listener and Dispatch, the
// `legion` and the Oh My Pi profile every pane runs, the model's way in, and the base prompts.
type sessionsLive struct {
	t        *testing.T
	natsURL  string
	listener string
	dispatch *scratchDispatch
	client   *http.Client
	omp      string
	legion   string
	home     string
	profile  string
	overlays []string
	// keyFile is the file of the model key, which each run reads at its start (modelKey) and gives
	// to its panes' environment and to nothing else: no line this driver prints holds the key.
	keyFile string
	model   string
	calls   *sessionsModelProxy
	// base is the directory the base commit's prompt files were extracted into, and baseCommit
	// the commit, as a note names it.
	base, baseCommit string
	// evidence is the directory each run's transcripts are copied into.
	evidence string
}

// TestRealSessionsTellAndAnswerANewVersionLive prints the plan of the steps, arms and count of runs
// the pick names (pickSessions, sessionsPick.plan), by default each step three times in the head
// arm and then, for off, three times in the base arm, and then runs them, unless -part-two-plan
// asks for the plan alone. It stops at the first head run that does not pass: one
// that failed its assertion, or one that stopped before it. It stops at a run of either arm that
// the harness failed too (run). A run that -test.run leaves out is none of these.
func TestRealSessionsTellAndAnswerANewVersionLive(t *testing.T) {
	fixtureenv.Live(t)
	pick, err := pickSessions(os.Getenv(sessionsStepEnv), os.Getenv(sessionsArmEnv), os.Getenv(sessionsRunsEnv))
	if err != nil {
		fmt.Printf("plan refused: %v\n", err)
		t.Fatal(err)
	}
	for _, step := range pick.steps {
		if sessionsBodies[step.name] == nil {
			t.Fatalf("the step table names %s, which sessionsBodies holds no body of", step.name)
		}
	}
	fmt.Print(pick.plan())
	if *sessionsPlanOnly {
		return
	}
	s := newSessionsLive(t)
	runs := 0
	for _, step := range pick.steps {
		for _, arm := range pick.arms(step) {
			for n := 1; n <= pick.runs; n++ {
				ran, outcome := s.run(step, arm, n)
				if !ran {
					continue
				}
				runs++
				if outcome != sessionsPassed && arm == sessionsHead {
					fmt.Printf("model calls: %d in all, over %d runs\n", s.calls.count(), runs)
					t.Fatalf("%s %s run %d ended %s, so the head arm did not reach its assertion and pass in every run; its evidence is above",
						step.name, arm, n, outcome)
				}
			}
		}
	}
	fmt.Printf("model calls: %d in all, over %d runs\n", s.calls.count(), runs)
}

// newSessionsLive starts what every run shares: natsd and the listener on it, Dispatch on that
// broker, the `legion` and the two plugins built from this checkout in a profile of the run's
// own, routed to the real model through the counting proxy. Unset, the key file skips the test:
// only the script names one.
func newSessionsLive(t *testing.T) *sessionsLive {
	t.Helper()
	keyFile := os.Getenv(sessionsKeyFileEnv)
	if keyFile == "" {
		t.Skipf("%s is unset, so there is no model key for a session; scripts/e2e/approved-version-news.sh names it", sessionsKeyFileEnv)
	}
	s := &sessionsLive{t: t, client: &http.Client{Timeout: 30 * time.Second}}
	for name, into := range map[string]*string{
		liveOmpPathEnv: &s.omp, sessionsBasePromptsEnv: &s.base, sessionsBaseCommitEnv: &s.baseCommit,
	} {
		if *into = os.Getenv(name); *into == "" {
			t.Fatalf("%s is unset; scripts/e2e/approved-version-news.sh sets it beside %s", name, sessionsKeyFileEnv)
		}
	}
	gateway := os.Getenv("ANTHROPIC_BASE_URL")
	if gateway == "" {
		t.Fatal("ANTHROPIC_BASE_URL is unset, so there is no model gateway for a session to reach")
	}
	s.keyFile = keyFile
	// A file that holds no key fails the test here, before anything starts; each run reads the
	// file again at its start.
	s.modelKey(t)
	if s.model = os.Getenv(sessionsModelEnv); s.model == "" {
		s.model = sessionsDefaultModel
	}
	s.calls = newSessionsModelProxy(t, gateway)
	logs := t.TempDir()
	if daemonLog := os.Getenv(liveDaemonLogEnv); daemonLog != "" {
		logs = filepath.Dir(daemonLog)
	}
	s.evidence = filepath.Join(logs, "transcripts")
	if err := os.MkdirAll(s.evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	s.natsURL = startSessionsNATS(t, logs)
	s.listener = startSessionsListener(t, logs, s.natsURL)
	root := proofCheckoutRoot(t)
	dispatchLog, err := os.OpenFile(filepath.Join(logs, "dispatch.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open Dispatch's log: %v", err)
	}
	t.Cleanup(func() { dispatchLog.Close() })
	s.dispatch = startScratchDispatchOn(t, scratchDispatchOptions{natsURL: s.natsURL, root: root, log: dispatchLog})
	version, err := exec.Command(s.omp, "--version").Output()
	if err != nil {
		t.Fatalf("read the Oh My Pi version of %s: %v", s.omp, err)
	}
	work := t.TempDir()
	s.legion = buildProofLegion(t, root, work)
	s.home = filepath.Join(work, "omp-home")
	s.profile = "legion-approved-version-news"
	installProofPlugins(t, root, work, s.home, s.profile, s.omp)
	// The checkout's own `dispatch`, from the pi-envoy package just packed, ahead of the pod's: a
	// pane's PATH is this process's behind the daemon's launcher directory (workerbin.Path).
	bin := filepath.Join(work, "pi-envoy", "bin")
	for _, file := range []string{filepath.Join(bin, "dispatch"), filepath.Join(work, "pi-envoy", "dist", "dispatch.js")} {
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("the packed pi-envoy holds no %s, so a pane would run another build's dispatch: %v", file, err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s.writeProfile(work)
	fmt.Printf("   Oh My Pi %s at %s, model anthropic/%s through a counting proxy in front of the gateway, "+
		"Envoy listener %s and Dispatch %s on natsd %s, `dispatch` from %s; the base arm's prompts from %s\n",
		strings.TrimSpace(string(version)), s.omp, s.model, s.listener, s.dispatch.base, s.natsURL, bin, s.baseCommit)
	return s
}

// modelKey is the model key keyFile holds now. The script can mint the key again while part two
// runs (the pod's identity token, which lasts an hour), so each run reads it at its start (newRun)
// rather than once for every run. A file that cannot be read or holds no key fails t.
func (s *sessionsLive) modelKey(t *testing.T) string {
	t.Helper()
	key, err := os.ReadFile(s.keyFile)
	if err != nil {
		t.Fatalf("read the model key file %s: %v", s.keyFile, err)
	}
	trimmed := strings.TrimSpace(string(key))
	if trimmed == "" {
		t.Fatalf("the model key file %s is empty", s.keyFile)
	}
	return trimmed
}

// writeProfile writes the profile every pane shares, and the overlays each is told: the one
// provider, the gateway's anthropic-messages API behind the counting proxy, whose key a pane reads
// from its own ANTHROPIC_API_KEY, so the key is written to no file; the one model for every model
// role, so no role reaches another; and the setup wizard off, which a pane would otherwise wait on.
func (s *sessionsLive) writeProfile(work string) {
	t := s.t
	t.Helper()
	agent := filepath.Join(s.home, ".omp", "profiles", s.profile, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("make the profile's agent directory %s: %v", agent, err)
	}
	selector := "anthropic/" + s.model
	writeProofFile(t, filepath.Join(agent, "models.yml"), fmt.Sprintf(
		"# Written by packages/daemon/internal/daemon/approved_version_news_sessions_live_test.go.\n"+
			"providers:\n  anthropic:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: ANTHROPIC_API_KEY\n"+
			"    models:\n      - {id: %s, name: %s, reasoning: true, input: [text], contextWindow: 1000000, maxTokens: 128000}\n",
		s.calls.url, s.model, s.model))
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/approved_version_news_sessions_live_test.go.\n"+
			"enabledModels:\n  - "+selector+"\ndefaultThinkingLevel: "+sessionsThinking+"\nstartup:\n  setupWizard: false\n")
	dir := filepath.Join(work, "overlays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	writeProofFile(t, operator, "# The operator's pane overlay (runtime.tmux.pi_config_files).\ndefaultThinkingLevel: "+sessionsThinking+"\n")
	role := filepath.Join(dir, "role.yml")
	writeRoleOverlay(t, role, selector)
	s.overlays = []string{operator, role}
}

// sessionsRun is one run of one step in one arm: its booted daemon, the trees it seeded, and the
// claims whose panes it launched.
type sessionsRun struct {
	liveAgentProof
	s         *sessionsLive
	name, arm string
	// repo is the default repository of the run's project; launchable is every claim the run
	// seeded, the only ones its runtime starts.
	repo       string
	launchable map[claim.Token]bool
	members    []*sessionsMember
	// deadline is the run's bound.
	deadline time.Time
}

// sessionsMember is one issue of a run's trees: its key, its tree's root, its parent ("" for a
// root), its phase, the roles whose claims the run seeds, the root's spec and the issue's
// Verification document with its one check ("" for none), and the task each role's claim holds at
// the boot, at the entry the start path raised for it: a task of the step's own in place of that
// start's task (tasks), or the start's own stored task with a note of the step's before the
// review round's two sentences it ends with (notes, a reviewer's alone), or nothing, for a role
// that took its start's task before the boot and for a child's own architect, whose start carries
// none (sessionsRun.seedClaim).
type sessionsMember struct {
	key, tree, parent string
	phase             phase.Phase
	roles             []claim.Role
	spec              string
	verification      string
	check             string
	tasks             map[claim.Role]string
	notes             map[claim.Role]string
}

// ownArchitect says whether the run seeds the member's own architect on a child: the claim #4203
// gives every child the daemon starts, which its first phase start starts.
func (m *sessionsMember) ownArchitect() bool {
	return m.parent != "" && slices.Contains(m.roles, claim.RoleArchitect)
}

// run makes one run in a subtest of its own, whose cleanup stops its daemon, and answers whether
// it ran, which a -test.run pattern can leave out, and how it ended. Its sessions end with its
// outcome, before anything else of the run is read, so none spends a model call past it. A fault
// of the harness fails the subtest, prints the run's line as HARNESS and ends the proof, since no
// count of runs means anything past it. These are such faults:
//   - a fault before the step's own outcome;
//   - a fault in ending the sessions (localRuntime.killEverything) or in keeping their transcripts
//     after it;
//   - a fault in a cleanup of the subtest, such as the daemon's stop or the store's close, since
//     the line prints after them (sessionsRunLine.printAtEnd);
//   - a session whose transcript holds the plugin's refusal of a daemon answer (sessionsEnding).
//
// Otherwise the step's own outcome is the answer.
func (s *sessionsLive) run(step sessionsStep, arm string, n int) (ran bool, outcome sessionsOutcome) {
	name := fmt.Sprintf("%s %s %d", step.name, arm, n)
	line := newSessionsRunLine()
	succeeded := s.t.Run(fmt.Sprintf("%s-%s-%d", step.name, arm, n), func(t *testing.T) {
		ran = true
		// The run's line is the subtest's first cleanup, so it prints last.
		line.printAtEnd(t, name, s.calls.count)
		fmt.Printf("== %s\n", name)
		var r *sessionsRun
		defer func() {
			if r == nil {
				return
			}
			r.rt.killEverything()
			if r.d == nil {
				return
			}
			line.refused = r.keepTranscripts()
			for _, session := range line.refused {
				t.Errorf("the legion plugin refused a daemon answer as a body the daemon API contract does not describe, in the transcript of %s", session)
			}
		}()
		r = s.newRun(t, name, arm, step.gates)
		line.outcome, line.why = sessionsBodies[step.name](r)
	})
	if !succeeded {
		s.t.Fatalf("the harness failed in run %s; its log is above", name)
	}
	return ran, line.outcome
}

// newRun readies a run's configuration, database, Dispatch project and runtime. The step seeds its
// trees and boots the daemon itself (boot).
func (s *sessionsLive) newRun(t *testing.T, name, arm string, gates config.DesignGate) *sessionsRun {
	t.Helper()
	cfg := workflowConfig(t, s.natsURL)
	// A Dispatch project key is at most ten characters (contracts ProjectKeyPattern), and the
	// daemon's project is its Dispatch project.
	cfg.Project = "AS" + randomSuffix(t)[:6]
	cfg.Projects = map[string]config.Project{cfg.Project: {}}
	cfg.AdmissionCap = 8
	cfg.Gates.Design = gates.Steps()
	cfg.DispatchURL = s.dispatch.base
	cfg.PostgresDSN = testdb.Database(t, "legion_version_sessions")
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte(s.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	notices := newNoticeListener()
	envoy := httptest.NewServer(notices)
	t.Cleanup(envoy.Close)
	cfg.EnvoyURL = envoy.URL
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	r := &sessionsRun{
		liveAgentProof: liveAgentProof{
			liveProof: liveProof{t: t, cfg: cfg, notices: notices, st: st, records: record.NewStore(), project: project},
			legion:    s.legion, omp: s.omp,
		},
		s: s, name: name, arm: arm, repo: "acme/" + strings.ToLower(cfg.Project), launchable: map[claim.Token]bool{},
	}
	r.dispatchAs(http.MethodPost, "/api/v1/projects", map[string]string{"key": cfg.Project, "name": "Approved version news sessions"}, nil)
	r.dispatchAs(http.MethodPut, "/api/v1/projects/"+cfg.Project+"/default-repository", map[string]any{"repo": r.repo}, nil)
	r.rt = newLocalRuntime(t, cfg.StateDir, s.legion, s.omp, s.home, s.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = cfg.DaemonURL, s.listener, []string{s.natsURL}
	r.rt.overlays = func(claim.Token) []string { return s.overlays }
	r.rt.dispatchURL, r.rt.dispatchTokenFile = s.dispatch.base, cfg.DispatchTokenFile
	r.rt.environment = []string{"ANTHROPIC_API_KEY=" + s.modelKey(t)}
	return r
}

// tree makes one tree on Dispatch: its root, carrying spec, and each child under it, each with the
// legion label and the status of the phase it stands in (sessionsStatus), as a running daemon
// leaves them; a member with a check carries a Verification document of that one check.
func (r *sessionsRun) tree(spec string, members ...*sessionsMember) {
	r.t.Helper()
	for i, member := range members {
		body := map[string]any{"project": r.cfg.Project, "title": fmt.Sprintf("Approved version news sessions, member %d", i), "force": true}
		if i == 0 {
			body["spec"] = spec
		} else {
			member.parent = members[0].key
			body["parent"] = member.parent
		}
		var created struct {
			Key     string `json:"key"`
			Primary string `json:"primary_artifact_id"`
		}
		r.dispatchAs(http.MethodPost, "/api/v1/issues", body, &created)
		if created.Key == "" || (i == 0 && created.Primary == "") {
			r.t.Fatalf("Dispatch created a member with key %q and spec %q", created.Key, created.Primary)
		}
		r.dispatchAs(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": sessionsStatus(member.phase), "labels": []string{"legion"}}, nil)
		member.key, member.tree = created.Key, members[0].key
		if i == 0 {
			member.spec, member.tree = created.Primary, created.Key
		}
		if member.check != "" {
			var uploaded struct {
				Artifact struct {
					ID string `json:"id"`
				} `json:"artifact"`
			}
			r.dispatchAs(http.MethodPost, "/api/v1/issues/"+member.key+"/artifacts",
				map[string]string{"name": "verification.md", "content": versionNewsVerification(member.check, sessionsCheck)}, &uploaded)
			if uploaded.Artifact.ID == "" {
				r.t.Fatalf("the Verification document of %s uploaded as no document", member.key)
			}
			member.verification = uploaded.Artifact.ID
		}
		r.members = append(r.members, member)
		r.note("%s in %s on Dispatch (tree %s): spec %q, Verification document %q", member.key, member.phase, member.tree, member.spec, member.verification)
	}
}

// sessionsStatus is the Dispatch status of an issue that stands in p as the daemon writes it at
// the move into p (workflow.PhaseStatus), so the boot's status repair, which rewrites every status
// that differs from its phase's, has nothing to write. A phase the daemon writes no status for,
// admitted, keeps in_progress, the status of a root its admission took into the workflow.
func sessionsStatus(p phase.Phase) string {
	if status := workflow.PhaseStatus(p); status != "" {
		return status
	}
	return "in_progress"
}

// sessionsSpec is a root spec: a `## Summary`, a `## Design` of design and a `## Later work`
// section of later.
func sessionsSpec(summary, design, later string) string {
	return "# The reader\n\n## Summary\n\n" + summary + "\n\n## Design\n\n" + design + "\n\n## Later work\n\n" + later + "\n"
}

// token is the claim token of role on member.
func (r *sessionsRun) token(member *sessionsMember, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, member.key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// checkArm reads the prompt the pane of role on member was launched with from the file its own
// command line names and fails the run's harness when it is not the arm's: the head arm's carries
// mark and the base arm's does not. A file that holds no prompt at all, empty or whitespace alone,
// is neither arm's, so it fails the harness in either arm rather than pass as the base commit's
// prompt, which would make the control bite for a reason the prompts under test do not give.
func (r *sessionsRun) checkArm(member *sessionsMember, role claim.Role, mark string) {
	r.t.Helper()
	token := r.token(member, role)
	prompt := r.launchedPrompt(token)
	if strings.TrimSpace(prompt) == "" {
		r.t.Fatalf("the %s arm's %s was launched with a prompt file of %d bytes that holds no prompt", r.arm, token, len(prompt))
	}
	carries := strings.Contains(prompt, mark)
	if carries != (r.arm == sessionsHead) {
		r.t.Fatalf("the %s arm's %s was launched with a prompt of %d bytes that carries the head's sentence %q: %t", r.arm, token, len(prompt), mark, carries)
	}
	said := "lacks"
	if carries {
		said = "carries"
	}
	r.note("%s runs the %s arm's prompt: the file its own command line names %s the head's sentence %q", token, r.arm, said, mark)
}

// launchedPrompt is the prompt token's Oh My Pi was started with: the contents of the file its one
// `--append-system-prompt` word names, read from /proc as role_prompt_rule_live_test.go reads it.
// The pid the locator carries is the shim's, whose child is Oh My Pi, and the launch writes that
// file before the process starts and never while it runs (runtime.WriteSystemPrompt). A file that
// cannot be read stops the run, since Oh My Pi would then have taken the path as the prompt's text.
func (r *sessionsRun) launchedPrompt(token claim.Token) string {
	t := r.t
	t.Helper()
	held := r.claim(token)
	if held.Locator == nil {
		t.Fatalf("%s is %s with no locator", token, held.State)
	}
	shim, _, err := parseLocalIncarnation(held.Locator.Incarnation)
	if err != nil {
		t.Fatalf("the locator of %s names no pid: %v", token, err)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		status, err := os.ReadFile("/proc/" + entry.Name() + "/status")
		if err != nil || parentOf(string(status)) != shim {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		if argv[0] != r.omp {
			continue
		}
		for i := len(argv) - 2; i >= 0; i-- {
			if argv[i] == promptWord {
				text, err := os.ReadFile(argv[i+1])
				if err != nil {
					t.Fatalf("%s's Oh My Pi was launched with %s %s, which could not be read: %v", token, promptWord, argv[i+1], err)
				}
				return string(text)
			}
		}
	}
	t.Fatalf("the shim of %s (pid %d) has no Oh My Pi child launched with %s", token, shim, promptWord)
	return ""
}

// deliver gives role on member task through the operator's route, as `legion claims deliver`
// does: the task reaches the session once it is between turns.
func (r *sessionsRun) deliver(member *sessionsMember, role claim.Role, task string) {
	r.t.Helper()
	token := r.token(member, role)
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver", api.DeliverRequest{Task: task}, true)
	if status/100 != 2 {
		r.t.Fatalf("deliver a task to %s = %d %s", token, status, body)
	}
	r.note("POST /legion/v1/operator/claims/%s/deliver:\n%s", token, indentOutput(task))
}

// settled waits until role on member holds no task and has stayed out of a turn for
// sessionsSettle, and answers false when the run's bound comes first.
func (r *sessionsRun) settled(member *sessionsMember, role claim.Role) bool {
	r.t.Helper()
	var since time.Time
	return r.until(r.deadline, func() bool { return r.quiet(member, role, &since) })
}

// quiet reads role's claim on member once and answers whether it has held no task and stood out of
// a turn for sessionsSettle, since being the first read that found it so (zero while it is busy).
// A wait that also watches for something else polls it beside its own read.
func (r *sessionsRun) quiet(member *sessionsMember, role claim.Role, since *time.Time) bool {
	r.t.Helper()
	held := r.claim(r.token(member, role))
	switch {
	case held.State == string(supervise.StateWorking) || held.Pending != nil:
		*since = time.Time{}
	case since.IsZero():
		*since = time.Now()
	}
	return !since.IsZero() && time.Since(*since) >= sessionsSettle
}

// until polls ready every two seconds until it holds or until is past, and answers whether it held.
func (r *sessionsRun) until(until time.Time, ready func() bool) bool {
	for {
		if ready() {
			return true
		}
		if time.Now().After(until) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

// transcript reads the session file token's pane registered, which is empty until the session
// writes it.
func (r *sessionsRun) transcript(member *sessionsMember, role claim.Role) sessionsTranscript {
	r.t.Helper()
	file := r.claim(r.token(member, role)).SessionFile
	if file == "" {
		return sessionsTranscript{}
	}
	body, err := os.ReadFile(file)
	if err != nil {
		return sessionsTranscript{}
	}
	return sessionsReadTranscript(body)
}

// bash is every bash command whose text holds words.
func (read sessionsTranscript) bash(words string) []sessionsCall {
	var found []sessionsCall
	for _, call := range read.calling("bash") {
		if strings.Contains(call.text, words) {
			found = append(found, call)
		}
	}
	return found
}

// sessionsExcerpt is text as evidence prints it: on one line, cut at 600 bytes.
func sessionsExcerpt(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 600 {
		text = text[:600] + "…"
	}
	return strconv.Quote(text)
}

// evidence prints one piece of a run's evidence.
func (r *sessionsRun) evidence(format string, args ...any) {
	fmt.Printf("   evidence: "+format+"\n", args...)
}

// printCalls prints each call of tool member's role made, the call and its arguments.
func (r *sessionsRun) printCalls(member *sessionsMember, role claim.Role, calls []sessionsCall, what string) {
	for _, call := range calls {
		r.evidence("the %s of %s %s: %s %s", role, member.key, what, call.name, sessionsExcerpt(call.text))
	}
}

// keepTranscripts copies the session file of every claim the run seeded into the evidence
// directory, under the run's own name, and answers each session whose transcript holds the
// plugin's refusal of a daemon answer (refusedByContract): its claim and the refusal, sorted.
func (r *sessionsRun) keepTranscripts() []string {
	dir := filepath.Join(r.s.evidence, strings.ReplaceAll(r.name, " ", "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.t.Errorf("make %s: %v", dir, err)
		return nil
	}
	var refused []string
	for token := range r.launchable {
		file := r.claim(token).SessionFile
		if file == "" {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			r.t.Errorf("read the transcript of %s: %v", token, err)
			continue
		}
		writeProofFile(r.t, filepath.Join(dir, string(token)+".jsonl"), string(body))
		if refusal := sessionsReadTranscript(body).refusedByContract(); refusal != "" {
			refused = append(refused, string(token)+": "+sessionsExcerpt(refusal))
		}
	}
	slices.Sort(refused)
	r.note("transcripts kept in %s", dir)
	return refused
}

// version2 answers whether artifact has a version 2, and that version as Dispatch answers it
// (dispatchExchange, with the agent bearer).
func (r *sessionsRun) version2(artifact string) (bool, string) {
	r.t.Helper()
	status, answer, err := dispatchExchange(r.s.client, r.s.dispatch.base, http.MethodGet, "/api/v1/artifacts/"+artifact+"/versions/2", nil,
		bearing(r.s.dispatch.token))
	if err != nil {
		r.t.Fatalf("read version 2 of %s: %v", artifact, err)
	}
	return status == http.StatusOK, string(answer)
}

// approvalAsks is every approval request Dispatch holds on key, as the issue's asks list it.
func (r *sessionsRun) approvalAsks(key string) []string {
	r.t.Helper()
	var asks []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	r.dispatchAgent(http.MethodGet, "/api/v1/issues/"+key+"/asks?state=all", nil, http.StatusOK, &asks)
	var approvals []string
	for _, ask := range asks {
		if ask.Kind == "approval" {
			approvals = append(approvals, ask.ID)
		}
	}
	return approvals
}

// comments is the body of every comment on key, or, where by is a session, of every comment that
// session wrote: Dispatch records a comment a session's `dispatch comment` posts as that session's
// (model.Actor, kind session), and Oh My Pi's session is the one a pane's claim registers.
func (r *sessionsRun) comments(key, by string) []string {
	r.t.Helper()
	var listed []struct {
		Body   string `json:"body"`
		Author struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"author"`
	}
	r.dispatchAgent(http.MethodGet, "/api/v1/issues/"+key+"/comments", nil, http.StatusOK, &listed)
	var bodies []string
	for _, comment := range listed {
		if by == "" || (comment.Author.Kind == "session" && comment.Author.ID == by) {
			bodies = append(bodies, comment.Body)
		}
	}
	return bodies
}

// approve is the person's approval of artifact's latest version, which must be want.
func (r *sessionsRun) approve(artifact string, want int) {
	r.t.Helper()
	var review struct {
		Version int    `json:"version"`
		State   string `json:"state"`
	}
	r.dispatchAs(http.MethodPost, "/api/v1/artifacts/"+artifact+"/reviews", map[string]string{"state": "approved"}, &review)
	if review.Version != want || review.State != "approved" {
		r.t.Fatalf("%s's review of %s = %s at version %d, want approved at version %d", versionNewsPerson, artifact, review.State, review.Version, want)
	}
}

// edit writes the next version of artifact through Dispatch's edit route as session, replacing
// find with with, which must store version want.
func (r *sessionsRun) edit(artifact, session, find, with string, want int) {
	r.t.Helper()
	var edited struct {
		Version *struct {
			Number int `json:"number"`
		} `json:"version"`
	}
	r.dispatchAgent(http.MethodPost, "/api/v1/artifacts/"+artifact+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": find, "with": with}},
		"actor": map[string]string{"kind": "session", "id": session},
	}, http.StatusOK, &edited)
	if edited.Version == nil || edited.Version.Number != want {
		r.t.Fatalf("the edit of %s stored version %+v, want %d", artifact, edited.Version, want)
	}
}

// dispatchAs sends one request to Dispatch as the person smoke, requiring a 2xx answer.
func (r *sessionsRun) dispatchAs(method, path string, body, into any) {
	r.t.Helper()
	r.dispatchCall(method, path, body, 0, into, dispatchUser(versionNewsPerson))
}

// dispatchAgent sends one request to Dispatch with the agent bearer, requiring want.
func (r *sessionsRun) dispatchAgent(method, path string, body any, want int, into any) {
	r.t.Helper()
	r.dispatchCall(method, path, body, want, into, bearing(r.s.dispatch.token))
}

// dispatchCall is one request to Dispatch (dispatchExchange); want 0 takes any 2xx answer. A
// failure fails the run's own test, from the goroutine that runs it.
func (r *sessionsRun) dispatchCall(method, path string, body any, want int, into any, identify func(*http.Request)) {
	r.t.Helper()
	status, answer, err := dispatchExchange(r.s.client, r.s.dispatch.base, method, path, body, identify)
	if err != nil {
		r.t.Fatal(err)
	}
	if refused := dispatchRefused(method, path, want, status, answer); refused != "" {
		r.t.Fatal(refused)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			r.t.Fatalf("decode Dispatch's answer to %s %s (%s): %v", method, path, answer, err)
		}
	}
}

// stateIssue is key as GET /legion/v1/state shows it now.
func (r *sessionsRun) stateIssue(key string) api.Issue {
	r.t.Helper()
	issue, ok := r.d.state().Issues[key]
	if !ok {
		r.t.Fatalf("GET /legion/v1/state records no issue %s", key)
	}
	return issue
}
