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
// names, which this driver reads into memory and gives to each pane's own environment and to
// nothing else. The Envoy listener the panes register with, claim their roles at and publish to is
// `packages/envoy/cmd/listener`, on a broker of this driver's own, natsd
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
// writes them. Each role the run seeds is then started through the daemon's own start path, by
// the function part one's seed starts its claims with (startRoles): a root's architect as
// admission's promotion starts it (workflow.Engine.StartArchitect), a child's own architect as the
// child's first phase start starts it for every child the daemon starts (Engine.StartFirstPhase,
// which queues the plain start workflow's childArchitectStart builds), and a member's phase worker
// as the promotion of a mid-phase member starts it (Engine.StartPromotedWorker). Each start of a
// root's architect or a worker raises its role's entry on the role's phases row; the plain start
// of a child's own architect raises none and carries no task. A worker's raise also writes its
// issue's holder row of the told record, from the gate the seed wrote (record.Store.BaselineTold).
// Each claim is a launch the daemon before the boot began and never recorded, which the boot
// relaunches. That daemon's outbox ran each start, and every other row a child's first phase start
// queued (the row that creates its branch, the notices of its move into planning), so the seed
// takes those rows off the queue and writes the claim as the start leaves it: given the start's
// entry. A role whose step needs a task of its own (holderStart, reviewerStart, parentStart) holds
// that task at that entry, in place of the start's own task. Any other root architect took its
// start's task before the boot, and holds none, and a child's own architect was given none. A
// root's architect row of the told record comes from its tree's first opening, which the seed
// does not run, so the seed writes it by hand and checks it against the row that opening writes.
// Before the boot the seed checks each claim by part one's rule (standAtTheirEntries): it stands
// at an entry of 1 or more of its role's phases row, given that entry, and it holds its task at
// that entry; a child's own architect stands at no entry, given none, holding no task, as its
// plain start leaves it. A seed that differs fails the run's harness before any pane starts, and
// the failure names the claim or the row, the field and both values.
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
// Three steps, each run three times in each arm, each run on trees of its own:
//   - a4-the-architect-tells-the-child-reviewer: under root-issues, a root waiting in admitted and
//     its child in reviewing, whose reviewer and own architect are live, with the root's architect
//     live above them; the child's architect is asked to write version 2 of its issue's
//     Verification document. Once it tells its parent of the version, the proof reviews it in the
//     parent's place and form and tells the child's architect, from the parent's own session
//     (sessionsRun.reviewAsParent); a run whose child's architect did not tell its parent stopped
//     before the assertion. It passes when the reviewer's transcript holds an Envoy message naming
//     version 2 and the changed check, the child's architect's transcript holds its envoy_publish
//     to the reviewer's role topic naming the same, and the child has no approval request;
//   - off-the-architect-tells-the-holders: under gates.design off, a root in implementing and its
//     child in reviewing, both holders and the child's own architect live; the root's architect
//     is asked to write version 2 of the root's spec. It passes when each holder's transcript
//     holds an Envoy message naming version 2 and the change, and the root architect's transcript
//     holds its envoy_publish to that holder's role topic naming the same;
//   - answers-a-stale-reviewer-moves-back-a-current-one-comments: under root-issues, two roots in
//     reviewing, each reviewer told what its review rests on; the person approves a version 2 of
//     each spec, one changing the section the review rests on and one a section it does not touch.
//     It passes when the first reviewer's request_backward_move names version 2 and moves its
//     issue back, and the second writes a comment on its issue naming version 2 and stays.
//
// Three variables the script passes through narrow a run of part two (pickSessions):
// APPROVED_VERSION_NEWS_STEP names one step by its word (a4, off or answers),
// APPROVED_VERSION_NEWS_ARM one arm (head or base), and APPROVED_VERSION_NEWS_RUNS the count of
// runs each arm makes of each step, so one command measures one arm of one step as often as it
// asks.
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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The script's inputs to this driver. The key file is a path, never the key: the script exports
// nothing of the key, so the builds and the broker the lib starts never hold it.
const (
	sessionsKeyFileEnv     = "APPROVED_VERSION_NEWS_MODEL_KEY_FILE"
	sessionsBasePromptsEnv = "APPROVED_VERSION_NEWS_BASE_PROMPTS"
	sessionsBaseCommitEnv  = "APPROVED_VERSION_NEWS_BASE_COMMIT"
	sessionsModelEnv       = "APPROVED_VERSION_NEWS_MODEL"
	// The three that narrow a run of part two to one step, one arm or another count of runs
	// (pickSessions), each unset for every step, both arms and sessionsRuns.
	sessionsStepEnv = "APPROVED_VERSION_NEWS_STEP"
	sessionsArmEnv  = "APPROVED_VERSION_NEWS_ARM"
	sessionsRunsEnv = "APPROVED_VERSION_NEWS_RUNS"
)

// sessionsDefaultModel is the model every session runs when the script names none, at
// sessionsThinking: the fleet's own, as the operator's pane overlay names it for every role. The
// prompts are written for the model the fleet runs, so a cheaper one would measure that model
// rather than the prompts.
const sessionsDefaultModel = "claude-opus-5-5"

// sessionsThinking is the thinking level every session runs at, the fleet's own.
const sessionsThinking = "xhigh"

// The steps of part two, which the script counts under these names.
const (
	sessionsA4      = "a4-the-architect-tells-the-child-reviewer"
	sessionsOff     = "off-the-architect-tells-the-holders"
	sessionsAnswers = "answers-a-stale-reviewer-moves-back-a-current-one-comments"
)

// sessionsStep is one step of part two: its name, which each run's line prints; its word, which
// the script's last line names it by and APPROVED_VERSION_NEWS_STEP picks it by; the design gate
// its runs boot under; and its body.
type sessionsStep struct {
	name, word string
	gates      config.DesignGate
	body       func(*sessionsRun) (sessionsOutcome, string)
}

// sessionsSteps is every step of part two, in the order a run makes them.
var sessionsSteps = []sessionsStep{
	{sessionsA4, "a4", config.DesignGateRootIssues, (*sessionsRun).a4},
	{sessionsOff, "off", config.DesignGateOff, (*sessionsRun).off},
	{sessionsAnswers, "answers", config.DesignGateRootIssues, (*sessionsRun).answers},
}

// The two arms: the prompts this checkout composes, and the base commit's.
const (
	sessionsHead = "head"
	sessionsBase = "base"
)

// sessionsRuns is how many runs each arm makes of each step, unless APPROVED_VERSION_NEWS_RUNS
// names another count.
const sessionsRuns = 3

// sessionsRunBound bounds a run from its daemon's boot to its verdict. A run still waiting at its
// bound fails, and its sessions are ended with it, so an agent that wanders spends no more than
// this.
const sessionsRunBound = 8 * time.Minute

// sessionsPick is what one run of part two makes: the steps, the arms, and how many runs each arm
// makes of each step.
type sessionsPick struct {
	steps []sessionsStep
	arms  []string
	runs  int
}

// pickSessions is the pick that APPROVED_VERSION_NEWS_STEP, APPROVED_VERSION_NEWS_ARM and
// APPROVED_VERSION_NEWS_RUNS name, given here as step, arm and runs: one step by its word, one
// arm, and a count of runs of 1 or more. Each left empty picks every step, both arms, or
// sessionsRuns runs. A value that names none of these is refused, naming what it may be.
func pickSessions(step, arm, runs string) (sessionsPick, error) {
	pick := sessionsPick{steps: sessionsSteps, arms: []string{sessionsHead, sessionsBase}, runs: sessionsRuns}
	if step != "" {
		words := make([]string, len(sessionsSteps))
		for i, each := range sessionsSteps {
			words[i] = each.word
			if each.word == step {
				pick.steps = []sessionsStep{each}
			}
		}
		if !slices.Contains(words, step) {
			return sessionsPick{}, fmt.Errorf("%s=%q names no step of part two: it is one of %s", sessionsStepEnv, step, strings.Join(words, ", "))
		}
	}
	switch arm {
	case "":
	case sessionsHead, sessionsBase:
		pick.arms = []string{arm}
	default:
		return sessionsPick{}, fmt.Errorf("%s=%q names no arm: it is %s or %s", sessionsArmEnv, arm, sessionsHead, sessionsBase)
	}
	if runs != "" {
		count, err := strconv.Atoi(runs)
		if err != nil || count < 1 {
			return sessionsPick{}, fmt.Errorf("%s=%q is no count of runs: it is a whole number of 1 or more", sessionsRunsEnv, runs)
		}
		pick.runs = count
	}
	return pick, nil
}

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
	sessionsArchitectMark = "Send that news yourself once you write such a version"
	sessionsWorkerMark    = "A news task tells you that a person approved a new version of your tree's design."
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
	// key is the model key, which reaches each pane's environment and nothing else: no line this
	// driver prints holds it.
	key   string
	model string
	calls *sessionsModelProxy
	// base is the directory the base commit's prompt files were extracted into, and baseCommit
	// the commit, as a note names it.
	base, baseCommit string
	// evidence is the directory each run's transcripts are copied into.
	evidence string
}

// TestRealSessionsTellAndAnswerANewVersionLive runs the steps, arms and count of runs the pick
// names (pickSessions), by default every step three times in the head arm and then three times in
// the base arm, and stops at the first head run that does not pass: one that failed its
// assertion, or one that stopped before it. It stops at a run of either arm that the harness
// failed too (run). A run that -test.run leaves out is none of these.
func TestRealSessionsTellAndAnswerANewVersionLive(t *testing.T) {
	fixtureenv.Live(t)
	pick, err := pickSessions(os.Getenv(sessionsStepEnv), os.Getenv(sessionsArmEnv), os.Getenv(sessionsRunsEnv))
	if err != nil {
		t.Fatal(err)
	}
	s := newSessionsLive(t)
	runs := 0
	for _, step := range pick.steps {
		for _, arm := range pick.arms {
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
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read the model key file %s: %v", keyFile, err)
	}
	if s.key = strings.TrimSpace(string(key)); s.key == "" {
		t.Fatalf("the model key file %s is empty", keyFile)
	}
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

// sessionsModelProxy is the way every pane reaches the model: a reverse proxy in front of the
// gateway that counts each model request it forwards, so a run can say how many calls it made. It
// keeps nothing of a request: no header, no body.
type sessionsModelProxy struct {
	url   string
	calls atomic.Int64
}

// newSessionsModelProxy starts the proxy for the test's life.
func newSessionsModelProxy(t *testing.T, gateway string) *sessionsModelProxy {
	t.Helper()
	target, err := url.Parse(gateway)
	if err != nil {
		t.Fatalf("ANTHROPIC_BASE_URL is no URL: %v", err)
	}
	p := &sessionsModelProxy{}
	forward := httputil.NewSingleHostReverseProxy(target)
	// A turn streams its answer: each event goes on as it arrives.
	forward.FlushInterval = -1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/messages") {
			p.calls.Add(1)
		}
		req.Host = target.Host
		forward.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	p.url = server.URL
	return p
}

// count is how many model requests the proxy has forwarded.
func (p *sessionsModelProxy) count() int64 { return p.calls.Load() }

// startSessionsNATS starts natsd from the checkout, with JetStream on a store of its own, on a port
// the kernel picks, which its log names; it is stopped when the test ends.
func startSessionsNATS(t *testing.T, logs string) string {
	t.Helper()
	binary := testbuild.EnvoyBinary(t, "./internal/testnats/cmd/natsd")
	output := &syncBuffer{}
	command := exec.Command(binary, "-p", "-1", "-js", "-sd", t.TempDir())
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	command.Stdout, command.Stderr = sessionsLog(t, logs, "natsd.log", output), sessionsLog(t, logs, "natsd.log", output)
	startSessionsProcess(t, "natsd", command)
	listening := regexp.MustCompile(`Listening for client connections on [^ ]*:(\d+)`)
	var port string
	sessionsAwait(t, "natsd to listen", 30*time.Second, func() bool {
		if match := listening.FindStringSubmatch(output.String()); match != nil {
			port = match[1]
			return true
		}
		return false
	})
	return "nats://127.0.0.1:" + port
}

// startSessionsListener starts the Envoy listener from the checkout on natsURL, serving on a
// loopback port held for it (ENVOY_LISTEN_FD), and waits for its /healthz to answer healthy. Its
// environment is its own: nothing of this pod's Envoy or Dispatch settings reaches it.
func startSessionsListener(t *testing.T, logs, natsURL string) string {
	t.Helper()
	binary := testbuild.EnvoyBinary(t, "./cmd/listener")
	held, port := heldSocket(t)
	command := exec.Command(binary)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "ENVOY_LISTEN_FD=3", "ENVOY_LISTEN_HOST=127.0.0.1",
		"ENVOY_MACHINE_ID=approved-version-news", "NATS_URLS=" + natsURL,
	}
	command.ExtraFiles = []*os.File{held}
	log := sessionsLog(t, logs, "listener.log", nil)
	command.Stdout, command.Stderr = log, log
	startSessionsProcess(t, "the Envoy listener", command)
	_ = held.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	sessionsAwait(t, "the Envoy listener's /healthz to answer healthy", 90*time.Second, func() bool {
		response, err := pollClient.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		var health struct {
			Status string `json:"status"`
		}
		return json.NewDecoder(response.Body).Decode(&health) == nil && health.Status == "healthy"
	})
	return base
}

// sessionsLog is the file name in the evidence directory, appended to, with also when it is not
// nil.
func sessionsLog(t *testing.T, logs, name string, also io.Writer) io.Writer {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(logs, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { file.Close() })
	if also == nil {
		return file
	}
	return io.MultiWriter(file, also)
}

// startSessionsProcess starts command and stops it when the test ends: SIGTERM, then SIGKILL ten
// seconds on.
func startSessionsProcess(t *testing.T, what string, command *exec.Cmd) {
	t.Helper()
	if err := command.Start(); err != nil {
		t.Fatalf("start %s: %v", what, err)
	}
	exited := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-exited
		}
	})
}

// sessionsAwait polls ready every 250 ms until it holds, and fails the test naming what after
// within.
func sessionsAwait(t *testing.T, what string, within time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", within, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// sessionsRun is one run of one step in one arm: its booted daemon, the trees it seeded, and the
// claims whose panes it launched.
type sessionsRun struct {
	liveAgentProof
	s         *sessionsLive
	name, arm string
	// repo is the repository the run's project maps; launchable is every claim the run seeded,
	// the only ones its runtime starts.
	repo       string
	launchable map[claim.Token]bool
	members    []*sessionsMember
	// deadline is the run's bound.
	deadline time.Time
}

// sessionsMember is one issue of a run's trees: its key, its tree's root, its parent ("" for a
// root), its phase, the roles whose claims the run seeds, the root's spec and the issue's
// Verification document with its one check ("" for none), and the task each role's claim holds at
// the boot, at the entry the start path raised for it, in place of that start's own task ("" for a
// role that took its start's task before the boot, and for a child's own architect, whose start
// carries none: sessionsRun.seedClaim).
type sessionsMember struct {
	key, tree, parent string
	phase             phase.Phase
	roles             []claim.Role
	spec              string
	verification      string
	check             string
	tasks             map[claim.Role]string
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
		line.outcome, line.why = step.body(r)
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
	r.dispatchAs(http.MethodPut, "/api/v1/settings/repo-projects/"+r.repo, map[string]any{"project": cfg.Project, "default": true}, nil)
	r.rt = newLocalRuntime(t, cfg.StateDir, s.legion, s.omp, s.home, s.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = cfg.DaemonURL, s.listener, []string{s.natsURL}
	r.rt.overlays = func(claim.Token) []string { return s.overlays }
	r.rt.dispatchURL, r.rt.dispatchTokenFile = s.dispatch.base, cfg.DispatchTokenFile
	r.rt.environment = []string{"ANTHROPIC_API_KEY=" + s.key}
	return r
}

// tree makes one tree on Dispatch: its root, carrying spec, and each child under it, each in
// progress with the legion label, as a running daemon leaves them; a member with a check carries a
// Verification document of that one check.
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
		r.dispatchAs(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, nil)
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

// sessionsSpec is a root spec: a `## Summary`, a `## Design` of design and a `## Later work`
// section of later.
func sessionsSpec(summary, design, later string) string {
	return "# The reader\n\n## Summary\n\n" + summary + "\n\n## Design\n\n" + design + "\n\n## Later work\n\n" + later + "\n"
}

// seed records every member as the daemon holds it mid-run and starts each role the run seeds
// through the daemon's own start path, in one transaction under a commit scope
// (intake.WithCommitHooks), as a fact the daemon applies writes them, as part one's seed does. Each
// member is recorded in its tree, in its phase, holding an admission slot, on an open pull request
// unless it stands in admitted, and its tree's lifecycle is opened under the workflow. A root whose
// deployment gates spec holds its gate open at version 1, which the person approved on Dispatch
// too, and its architect's row of the told record says so (seedArchitectTold). Each role is then
// started by part one's startRoles, which raises its entry and, for a phase worker, writes its
// issue's holder row from that gate; a child's own architect is started by the child's first
// phase start, as every started child's is, which raises no entry. The start's row is taken off
// the outbox (takeStart), and so are the other rows a child's first phase start queued
// (takeFirstPhase), and the claim is written as that start leaves it (seedClaim), once the
// lifecycles are open. No role prompt file is written below the state directory: such a file is
// a launch's whole prompt (specs.rolePromptPaths), and the composed prompts would reach no pane.
//
// A row the seed writes by hand that differs from the row the start path writes fails the run's
// harness here, after the transaction commits, naming the row, the field and both values. The
// claims are checked before the boot (standAtTheirEntries).
func (r *sessionsRun) seed() {
	t := r.t
	t.Helper()
	ctx, committed := intake.WithCommitHooks(context.Background())
	gated := len(r.cfg.Gates.Design) > 0
	if gated {
		for _, member := range r.members {
			if member.spec != "" {
				r.approve(member.spec, 1)
			}
		}
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:5], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	// The engine the booted daemon runs, built from the same configuration (engineConfig), so
	// each start written through it is the row the daemon's own admission writes.
	engine := workflow.New(record.NewStore(), engineConfig(r.cfg, "", nil), quietLogger())
	one := 1
	var starts map[claim.Token]record.SuperviseRequest
	var differences []string
	var firstPhase map[string][]string
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		starts, differences, firstPhase = map[claim.Token]record.SuperviseRequest{}, nil, map[string][]string{}
		issues := make([]record.Issue, len(r.members))
		for i, member := range r.members {
			var listed struct {
				Rank    string `json:"rank"`
				LastSeq int64  `json:"last_seq"`
			}
			r.dispatchAgent(http.MethodGet, "/api/v1/issues/"+member.key, nil, http.StatusOK, &listed)
			issue := record.Issue{
				Key: member.key, Repo: r.repo, DefaultBranch: "main", GitHubRepo: r.repo, Tree: member.tree, Project: r.cfg.Project,
				Title: "Approved version news sessions", Phase: member.phase, Generation: 1, Status: "in_progress",
				DispatchStatus: "in_progress", Rank: listed.Rank, LastDispatchSeq: listed.LastSeq,
			}
			if member.parent != "" {
				parent := member.parent
				issue.Parent = &parent
			}
			issues[i] = issue
			if err := r.records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			if member.phase != phase.Admitted {
				number++
				if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
					State: record.PullRequestOpen, Issue: member.key, Repo: r.repo, Number: int(number), Branch: "legion/" + member.key,
					HeadSHA: versionNewsHead(member.key), Base: "main", Failing: []string{},
				}); err != nil {
					return err
				}
			}
			if err := r.admit(ctx, tx, member.key); err != nil {
				return err
			}
		}
		for i, member := range r.members {
			if !gated || member.spec == "" {
				continue
			}
			// The opening the architect's row is checked against is read before the gate row is
			// written, so the probe meets a tree whose gate has never opened.
			path, err := toldAtFirstOpening(ctx, tx, engine, issues[i], member.spec)
			if err != nil {
				return err
			}
			if err := r.records.PutGate(ctx, tx, record.DesignGate{
				Issue: member.key, Step: record.DesignStepSpec, ArtifactID: member.spec, LatestVersion: 1, ApprovedVersion: &one, EverOpened: true,
			}); err != nil {
				return err
			}
			differs, err := seedArchitectTold(ctx, tx, member.key, path)
			if err != nil {
				return err
			}
			if differs != "" {
				differences = append(differences, differs)
			}
		}
		for i, member := range r.members {
			// The last row the outbox held before the member's starts, so that the rows a
			// child's first phase start queues beside its architect's are known (takeFirstPhase).
			var before int64
			if err := tx.QueryRow(ctx, `select coalesce(max(id), 0) from outbox`).Scan(&before); err != nil {
				return err
			}
			if err := startRoles(ctx, tx, r.records, engine, issues[i], member.roles); err != nil {
				return err
			}
			for _, role := range member.roles {
				start, err := takeStart(ctx, tx, member.key, role)
				if err != nil {
					return err
				}
				starts[r.token(member, role)] = start
			}
			if member.ownArchitect() {
				taken, err := takeFirstPhase(ctx, tx, member.key, before)
				if err != nil {
					return err
				}
				firstPhase[member.key] = taken
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the trees through the daemon's start path: %v", err)
	}
	committed()
	if len(differences) > 0 {
		t.Fatalf("setup: %s", strings.Join(differences, "; "))
	}
	for _, member := range r.members {
		if member.parent == "" {
			if _, err := r.st.OpenTreeLifecycle(ctx, r.project, member.key, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("open the lifecycle of %s: %v", member.key, err)
			}
		}
	}
	for _, member := range r.members {
		for _, role := range member.roles {
			r.seedClaim(ctx, member, role, starts[r.token(member, role)])
		}
		if taken, ok := firstPhase[member.key]; ok {
			r.note("%s's first phase start, which started its own architect, also queued %s, which the daemon before the boot ran: "+
				"the seed took them off the outbox", member.key, strings.Join(taken, ", "))
		}
		if gated && member.spec != "" {
			r.note("%s's architect took %s at its tree's first opening, in a row written by hand and the same as the row that opening writes",
				member.key, sessionsArchitectTook)
		}
	}
}

// sessionsArchitectTook is what the architect's row of the told record holds of a root whose
// deployment gates spec once its gate opened at version 1 of the spec: that version, as the row
// prints it.
const sessionsArchitectTook = `{"spec": 1}`

// seedArchitectTold writes the architect's row of the told record of root by hand, as its tree's
// first opening leaves it, and answers how it differs from path, the row that opening writes
// (toldAtFirstOpening), "" where it does not. The opening takes the row's baseline at the version
// it approves (workflow's putGateTold), so the approval of version 1 that Dispatch carries is news
// to nobody. It settles no opening on the row: a tree whose root holds no gate row has no step
// closed by a row of its own, so its standing gate reads open before the opening as after it, and
// no opened message goes (tellGateMoved). The seed writes the gate open without that opening, so
// the start path writes no such row here.
func seedArchitectTold(ctx context.Context, tx pgx.Tx, root string, path versionNewsTold) (string, error) {
	differs, err := seedToldRow(ctx, tx, root, "architect", sessionsArchitectTook, "the architect's told row of "+root, path)
	if err != nil {
		return "", err
	}
	if len(differs) == 0 {
		return "", nil
	}
	return fmt.Sprintf("the architect's row of the told record of %s: %s, where the start path writes the row at its tree's first opening, "+
		"at version 1 of its spec", root, strings.Join(differs, ", ")), nil
}

// toldAtFirstOpening is the architect's row of the told record the daemon writes for root at its
// tree's first opening, read in a savepoint of tx that is rolled back once the row is read
// (toldInSavepoint). It is read while root holds no gate row, so the tree has never opened. In the
// savepoint the architect's registration of spec at version 1 is applied as the daemon applies it
// (workflow.Engine.Apply), carrying the person's approval of that version as register_gate reads it
// from Dispatch (intake.GateRegistered): that registration opens the gate, which writes the row.
func toldAtFirstOpening(ctx context.Context, tx pgx.Tx, engine *workflow.Engine, root record.Issue, spec string) (versionNewsTold, error) {
	return toldInSavepoint(ctx, tx, "the architect's told row of "+root.Key+" at its first opening", func(probe pgx.Tx) (versionNewsTold, error) {
		if _, err := engine.Apply(ctx, probe, intake.GateRegistered{Issue: root.Key, Step: record.DesignStepSpec, ArtifactID: spec,
			Version: 1, DispatchVersion: 1, ApprovedVersion: 1}); err != nil {
			return versionNewsTold{}, fmt.Errorf("register version 1 of the spec of %s, approved: %w", root.Key, err)
		}
		return heldTold(ctx, probe, root.Key, "architect")
	})
}

// takeStart takes off the outbox the one start the daemon's start path queued in tx for role on
// issue, and answers it. Part two's claims are launches the daemon before the boot began, so that
// daemon's outbox ran their starts: the start's row goes, as that run finished it, and no start
// of the boot runs it again against the claim the seed writes in its place (seedClaim).
func takeStart(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (record.SuperviseRequest, error) {
	rows, err := tx.Query(ctx, `delete from outbox where kind = $1 and issue = $2 and payload->>'op' = 'start' and payload->>'role' = $3
		returning id, payload`, string(record.OutboxKindSupervise), issue, string(role))
	if err != nil {
		return record.SuperviseRequest{}, fmt.Errorf("take the start of the %s of %s off the outbox: %w", role, issue, err)
	}
	taken, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record.OutboxRow, error) {
		var payload []byte
		taken := record.OutboxRow{Kind: record.OutboxKindSupervise, Issue: issue}
		err := row.Scan(&taken.ID, &payload)
		taken.Payload = payload
		return taken, err
	})
	if err != nil {
		return record.SuperviseRequest{}, fmt.Errorf("take the start of the %s of %s off the outbox: %w", role, issue, err)
	}
	if len(taken) != 1 {
		return record.SuperviseRequest{}, fmt.Errorf("the daemon's start path queued %d starts of the %s of %s, where it queues one", len(taken), role, issue)
	}
	payload, err := record.DecodeOutboxPayload(taken[0])
	if err != nil {
		return record.SuperviseRequest{}, err
	}
	start, ok := payload.(record.SuperviseRequest)
	if !ok {
		return record.SuperviseRequest{}, fmt.Errorf("the start of the %s of %s decoded as %T", role, issue, payload)
	}
	return start, nil
}

// takeFirstPhase takes off the outbox every row queued in tx on issue, a child, after the row
// after, once takeStart has taken the starts of its roles, and answers what each row was, oldest
// first. They are the rest of the child's first phase start (workflow.Engine.StartFirstPhase),
// which started its own architect: the row that creates its branch and the notices of its move
// into planning. Its planner's start is not among them: the start of the phase worker the child
// stands in took that row's place, as a phase worker's start takes out the one its issue already
// owes (record.Store.EnqueueStart), and takeStart took that start. The daemon before the boot ran
// these rows long before the child reached the phase it stands in, as it ran the starts, so none
// is left for the boot to run again.
func takeFirstPhase(ctx context.Context, tx pgx.Tx, issue string, after int64) ([]string, error) {
	rows, err := tx.Query(ctx, `with taken as (delete from outbox where issue = $1 and id > $2 returning id, kind, payload)
		select kind, coalesce(payload->>'op', ''), coalesce(payload->>'role', ''), coalesce(payload->>'kind', '') from taken order by id`,
		issue, after)
	if err != nil {
		return nil, fmt.Errorf("take the rest of the first phase start of %s off the outbox: %w", issue, err)
	}
	taken, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var kind, op, role, notice string
		if err := row.Scan(&kind, &op, &role, &notice); err != nil {
			return "", err
		}
		switch {
		case kind == string(record.OutboxKindSupervise):
			return fmt.Sprintf("the %s of its %s", op, role), nil
		case kind == string(record.OutboxKindNotice):
			return fmt.Sprintf("a %s notice", notice), nil
		}
		return "a row of kind " + kind, nil
	})
	if err != nil {
		return nil, fmt.Errorf("take the rest of the first phase start of %s off the outbox: %w", issue, err)
	}
	return taken, nil
}

// seedClaim records role's claim on member as start, the start the daemon's start path queued for
// it, leaves it, with that start's launch begun and never recorded by the daemon before the boot,
// which the boot relaunches: launching, with the hash of a boot token no pane holds. A start that
// raised an entry gives the claim that entry, with the handshake the queue writes beside it
// (supervise.Claim.GivenEntry, Handshake). A role its step gives a task of its own (member.tasks)
// holds that task as the delivery of that entry, of the start's phase and run, unconfirmed, in
// place of the start's own task: the daemon sends it once the relaunched pane is ready. A role
// with none took its start's task before the boot (supervise.Claim.ServedEntry), as a root
// architect mid-run has, and holds nothing. The plain start of a child's own architect
// (workflow's childArchitectStart) raises no entry and carries no task, so its claim is given
// none, holds no handshake and serves no run: its first instruction is the catch-up at its
// launch's ready, and a step's task reaches it through the operator's route once the run has
// booted (deliver). The run's runtime launches this claim and the others the run seeds, and no
// other.
func (r *sessionsRun) seedClaim(ctx context.Context, member *sessionsMember, role claim.Role, start record.SuperviseRequest) {
	t := r.t
	t.Helper()
	token := r.token(member, role)
	r.launchable[token] = true
	now := time.Now()
	task := member.tasks[role]
	seeded := supervise.Claim{
		Token: token, Project: r.project, Tree: member.tree, Issue: member.key, Role: role, Generation: 1,
		State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}
	switch {
	case start.Entry == 0 && task != "":
		t.Fatalf("setup: the step gives the %s of %s a start task, where its start raised no entry to hold it at", role, member.key)
	case start.Entry > 0:
		seeded.ServingGeneration, seeded.GivenEntry = 1, start.Entry
		seeded.Handshake = supervise.EntryHandshake{Entry: start.Entry, Generation: start.Generation, QueuedAt: now}
		if task == "" {
			seeded.ServedEntry, seeded.Handshake.ConfirmedAt = start.Entry, now
		}
	}
	if _, err := r.st.AdmitClaim(ctx, seeded); err != nil {
		t.Fatalf("record the %s's unfinished launch on %s: %v", role, member.key, err)
	}
	if task == "" {
		return
	}
	if err := r.st.PutDelivery(ctx, token, supervise.Delivery{
		ID: "seeded-start-" + randomSuffix(t), Task: task, Generation: start.Generation, Phase: start.Phase, Entry: start.Entry, QueuedAt: now,
	}); err != nil {
		t.Fatalf("record the start task of %s: %v", token, err)
	}
}

// standAtTheirEntries is the seed's SETUP check, part one's (versionNewsLive.standAtTheirEntries)
// by the same rule (claimEntries, entryRefusal): each claim the seed wrote stands at an entry of 1
// or more of its role's phases row, given that entry, and a claim that holds a task holds it at
// that entry. A child's own architect is held to its plain start's rule instead
// (plainStartRefusal): no entry, given none, holding no task. It runs before the boot, where part
// one's runs after it, because part two's claims are written before the boot rather than by it: a
// run whose seed differs stops before any pane starts or spends a model call. A claim that stands
// anywhere else fails the run's harness, and the failure names the claim and both numbers. Each
// tree's claims are noted with their entries, which is the run's evidence of them.
func (r *sessionsRun) standAtTheirEntries() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	var trees []string
	stood := map[string][]string{}
	for _, member := range r.members {
		if !slices.Contains(trees, member.tree) {
			trees = append(trees, member.tree)
		}
		for _, role := range member.roles {
			token := r.token(member, role)
			name := fmt.Sprintf("the %s of %s", role, member.key)
			entry, given, err := claimEntries(ctx, r.st.Pool(), member.key, role, token)
			if err != nil {
				t.Fatalf("setup: read the entries of %s: %v", name, err)
			}
			refusal := entryRefusal(name, entry, given)
			if role == claim.RoleArchitect && member.parent != "" {
				refusal = plainStartRefusal(name, entry, given)
			}
			if refusal != "" {
				t.Fatalf("setup: %s", refusal)
			}
			var held *int64
			if err := r.st.Pool().QueryRow(ctx, `select (select entry from pending_task_deliveries where claim_token = $1)`,
				string(token)).Scan(&held); err != nil {
				t.Fatalf("setup: read the task %s holds: %v", name, err)
			}
			holds := "its start's task taken before the boot"
			if entry == 0 {
				holds = "holding no task, as its plain start gives none"
			}
			if held != nil {
				if *held != entry {
					t.Fatalf("setup: %s holds its start task at entry %d, where the daemon's start path raised entry %d for it", name, *held, entry)
				}
				holds = "holding its start task at that entry"
			}
			stood[member.tree] = append(stood[member.tree], fmt.Sprintf("the %s of %s at entry %d, %s", role, member.key, entry, holds))
		}
	}
	for _, tree := range trees {
		r.note("tree %s as the daemon's start path left it, each claim given its entry: %s", tree, strings.Join(stood[tree], "; "))
	}
}

// plainStartRefusal is why a child's own architect, named name, that stands at entry of its role's
// phases row and was given given does not stand where its plain start leaves it (workflow's
// childArchitectStart, which the child's first phase start queues): that start raises no entry
// and gives none, so both are 0. It is "" where the claim stands there. A claim given an entry, or
// a child whose architect's row names one, was not started the way the daemon starts every
// child's own architect, and the sentence names the claim and both numbers.
func plainStartRefusal(name string, entry, given int64) string {
	if entry == 0 && given == 0 {
		return ""
	}
	return fmt.Sprintf("%s stands at entry %d of its phases row and was given entry %d, where the plain start of a child's own architect, "+
		"which its first phase start queues, raises no entry and gives none", name, entry, given)
}

// boot boots the whole daemon over the local runtime, in the base arm after writing the base
// commit's prompt files over the snapshot the boot wrote, and waits for every seeded claim's pane
// to report ready. The seed is checked before the daemon starts (standAtTheirEntries).
func (r *sessionsRun) boot() {
	t := r.t
	t.Helper()
	r.seed()
	r.standAtTheirEntries()
	r.daemon = newRecordingLog(r.log())
	gated := &sessionsRuntime{localRuntime: r.rt, launchable: r.launchable}
	r.d = startDaemonWithin(t, r.cfg, overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Source,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			// The boot snapshotted the prompts before it builds the runtime (prompts.New, in the
			// plan), and no pane starts before the runtime exists.
			if r.arm == sessionsBase {
				if err := r.s.writeBasePrompts(r.cfg.StateDir); err != nil {
					return nil, err
				}
			}
			r.rt.conns, r.rt.stream = listener, address
			return gated, nil
		},
		listen:         heldListen,
		workflowTokens: &workflowTokenRecorder{},
	}, slog.New(r.daemon), 2*time.Minute)
	r.deadline = time.Now().Add(sessionsRunBound)
	for _, member := range r.members {
		for _, role := range member.roles {
			r.awaitReady(r.token(member, role))
		}
	}
}

// sessionsRuntime is the local runtime with every launch refused but those of the claims a run
// seeded: a phase the daemon starts after a step's act starts no session of its own.
type sessionsRuntime struct {
	*localRuntime
	launchable map[claim.Token]bool
}

func (g *sessionsRuntime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if !g.launchable[spec.Claim] {
		return runtime.Locator{}, fmt.Errorf("spawn %s: this proof starts only the sessions its run seeded", spec.Claim)
	}
	return g.localRuntime.Spawn(ctx, spec)
}

func (g *sessionsRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if !g.launchable[spec.Claim] {
		return runtime.Locator{}, fmt.Errorf("resume %s: this proof starts only the sessions its run seeded", spec.Claim)
	}
	return g.localRuntime.Resume(ctx, prev, spec)
}

// writeBasePrompts writes the base commit's role prompt files over the snapshot below stateDir:
// its roles/ over prompts/shared/ and its go/ over prompts/go/. A snapshot file the base holds no
// copy of fails the arm, since a pane reading it would run a prompt of neither commit.
func (s *sessionsLive) writeBasePrompts(stateDir string) error {
	written, differ := 0, 0
	for _, pair := range [][2]string{{"roles", "shared"}, {"go", "go"}} {
		from := filepath.Join(s.base, "packages", "daemon", "internal", "prompts", pair[0])
		to := filepath.Join(stateDir, "prompts", pair[1])
		if err := filepath.WalkDir(to, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			rel, err := filepath.Rel(to, path)
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(from, rel)); err != nil {
				return fmt.Errorf("the snapshot holds %s, which the base commit's prompts hold no copy of: %w", path, err)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			rel, err := filepath.Rel(from, path)
			if err != nil {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			target := filepath.Join(to, rel)
			if head, err := os.ReadFile(target); err != nil || !bytes.Equal(head, body) {
				differ++
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			written++
			return os.WriteFile(target, body, 0o600)
		}); err != nil {
			return err
		}
	}
	fmt.Printf("   the base arm: %d prompt files of %s written over the snapshot below %s, %d of them unlike the head's\n",
		written, s.baseCommit, stateDir, differ)
	return nil
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

// version2 answers whether artifact has a version 2, and that version as Dispatch answers it.
func (r *sessionsRun) version2(artifact string) (bool, string) {
	r.t.Helper()
	request, err := http.NewRequest(http.MethodGet, r.s.dispatch.base+"/api/v1/artifacts/"+artifact+"/versions/2", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+r.s.dispatch.token)
	response, err := r.s.client.Do(request)
	if err != nil {
		r.t.Fatalf("read version 2 of %s: %v", artifact, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode == http.StatusOK, string(body)
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

// comments is the body of every comment on key.
func (r *sessionsRun) comments(key string) []string {
	r.t.Helper()
	var listed []struct {
		Body string `json:"body"`
	}
	r.dispatchAgent(http.MethodGet, "/api/v1/issues/"+key+"/comments", nil, http.StatusOK, &listed)
	bodies := make([]string, len(listed))
	for i, comment := range listed {
		bodies[i] = comment.Body
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
	r.dispatchCall(method, path, body, 0, into, func(request *http.Request) { request.Header.Set("X-Dispatch-User", versionNewsPerson) })
}

// dispatchAgent sends one request to Dispatch with the agent bearer, requiring want.
func (r *sessionsRun) dispatchAgent(method, path string, body any, want int, into any) {
	r.t.Helper()
	r.dispatchCall(method, path, body, want, into, func(request *http.Request) { request.Header.Set("Authorization", "Bearer "+r.s.dispatch.token) })
}

// dispatchCall is one request to Dispatch; want 0 takes any 2xx answer. A failure fails the run's
// own test, from the goroutine that runs it.
func (r *sessionsRun) dispatchCall(method, path string, body any, want int, into any, identify func(*http.Request)) {
	r.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			r.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, r.s.dispatch.base+path, reader)
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	identify(request)
	response, err := r.s.client.Do(request)
	if err != nil {
		r.t.Fatalf("Dispatch %s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		r.t.Fatalf("read Dispatch's answer to %s %s: %v", method, path, err)
	}
	if (want != 0 && response.StatusCode != want) || (want == 0 && response.StatusCode/100 != 2) {
		r.t.Fatalf("Dispatch %s %s = %d %s", method, path, response.StatusCode, answer)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			r.t.Fatalf("decode Dispatch's answer to %s %s (%s): %v", method, path, answer, err)
		}
	}
}

// architectTask is the task the driver gives an architect: one edit of one document, the tree as
// its catch-up would show it, and nothing about who to tell.
func architectTask(document, issue, find, with, tree string) string {
	return fmt.Sprintf("A task from the operator of this tree. Write a new version, version 2, of the %s of %s. "+
		"In it, replace the words `%s` with `%s`. Make that one edit with `dispatch doc-edit`, and change nothing else "+
		"in that document. Your tree stands as follows: %s.", document, issue, find, with, tree)
}

// Step A4. A root waits in admitted while its child stands in reviewing under a live reviewer and
// a live architect of its own, which the child's first phase start started, as it starts every
// child's (workflow.Engine.StartFirstPhase); the root's gate stands open at version 1 of its spec,
// and the root's architect is live above the child. The child's architect is asked to write
// version 2 of its own issue's Verification document. A child's documents are its architect's to
// change and no person approves them, so the daemon sends no news of the version: the reviewer
// hears of it only from the architect that wrote it. Both arms' prompts tell that architect to
// tell the architect above it of each version it writes, and tell that parent to review each
// version, post its verdict as a `Spec review of` comment on the child and tell the child's
// architect with envoy_publish; the head's tell the child's architect to tell the holder of each
// issue the version changes once the version has that approving review.
//
// The parent's review is the proof's (reviewAsParent), in the parent's form and through the
// parent's own channels, once the child's architect has told the parent of the version. A live
// parent reviewed the fixture's version in round 13's first run and asked for changes to what the
// version had not changed, the child's empty spec and its check's run and negative control, so a
// live review measures the fixture rather than the telling R4 is about; and the version must have
// its approving review before the head's prompt puts that telling to the child's architect at
// all. The root's architect is live all the same, as every running tree's is, and its start task
// tells it that the operator reviews the child's versions in its place (parentStart), so the
// version gets one review. A run whose version the child's architect did not write, or did not
// tell its parent of, stopped before the assertion. The assertion is the child's architect's own
// envoy_publish to the reviewer's role topic naming version 2 and the changed check, since a card
// in the reviewer's transcript does not say who sent it or how; the reviewer's transcript holding
// a message that names both; and no approval request on the child.
func (r *sessionsRun) a4() (sessionsOutcome, string) {
	root := &sessionsMember{phase: phase.Admitted, roles: []claim.Role{claim.RoleArchitect}, tasks: map[claim.Role]string{}}
	child := &sessionsMember{phase: phase.Reviewing, roles: []claim.Role{claim.RoleArchitect, claim.RoleReviewer}, check: sessionsCheckID,
		tasks: map[claim.Role]string{}}
	r.tree(sessionsSpec("A reader that pages.", sessionsDesign, sessionsLater), root, child)
	root.tasks[claim.RoleArchitect] = parentStart(root.key, child.key)
	child.tasks[claim.RoleReviewer] = holderStart(child.key, phase.Reviewing)
	r.boot()
	r.checkArm(child, claim.RoleArchitect, sessionsArchitectMark)
	r.checkArm(root, claim.RoleArchitect, sessionsArchitectMark)
	for _, held := range []struct {
		member *sessionsMember
		role   claim.Role
	}{{root, claim.RoleArchitect}, {child, claim.RoleReviewer}} {
		if !r.settled(held.member, held.role) {
			return sessionsSetup, fmt.Sprintf("the %s of %s did not end its start turn within the run's bound", held.role, held.member.key)
		}
	}
	r.deliver(child, claim.RoleArchitect, architectTask(
		fmt.Sprintf("Verification document (`verification.md`, document %s)", child.verification), child.key, sessionsCheck, sessionsCheckAgain,
		fmt.Sprintf("%s, the root, in admitted, waiting for its child; %s, its child and your own issue, in reviewing, where its reviewer holds it",
			root.key, child.key)))
	change := sessionsChecksChange
	reviewer := notify.RoleTopicPrefix + string(r.token(child, claim.RoleReviewer))
	parentTopic := notify.RoleTopicPrefix + string(r.token(root, claim.RoleArchitect))
	told, review := "", ""
	var architectQuiet, parentQuiet time.Time
	r.until(r.deadline, func() bool {
		architect := r.transcript(child, claim.RoleArchitect)
		if _, toldParent := architect.publishedTo(parentTopic, "verification"); review == "" && toldParent {
			if written, text := r.version2(child.verification); sessionsUnwritten("", written, text, sessionsCheckAgain) == "" {
				review = r.reviewAsParent(child, root)
				// The review starts the child's architect's next turn, which no quiet read
				// before it can have seen.
				architectQuiet, parentQuiet = time.Time{}, time.Time{}
				return false
			}
		}
		told = r.transcript(child, claim.RoleReviewer).toldOf(change...)
		if _, published := architect.publishedTo(reviewer, change...); published && told != "" {
			return true
		}
		// The route stopped wherever it stopped once both architects are out of a turn at once,
		// each holding no task.
		architectDone, parentDone := r.quiet(child, claim.RoleArchitect, &architectQuiet), r.quiet(root, claim.RoleArchitect, &parentQuiet)
		return architectDone && parentDone
	})
	if told == "" {
		// The architects settled: a message one of them sent may still be on its way.
		r.until(time.Now().Add(sessionsGrace), func() bool {
			told = r.transcript(child, claim.RoleReviewer).toldOf(change...)
			return told != ""
		})
	}
	// The child's architect may still be in its turn: a request for approval would come in it.
	if !r.settled(child, claim.RoleArchitect) {
		r.evidence("the architect of %s was still in a turn or held a task at the run's bound, so what follows is what it did by then", child.key)
	}
	architect, parent := r.transcript(child, claim.RoleArchitect), r.transcript(root, claim.RoleArchitect)
	r.printCalls(child, claim.RoleArchitect, architect.bash("dispatch "), "ran")
	r.printCalls(child, claim.RoleArchitect, architect.calling("envoy_publish"), "called")
	r.printCalls(root, claim.RoleArchitect, parent.bash("dispatch "), "ran")
	r.printCalls(root, claim.RoleArchitect, parent.calling("envoy_publish"), "called")
	written, text := r.version2(child.verification)
	stopped := sessionsUnwritten(child.key+"'s Verification document", written, text, sessionsCheckAgain)
	switch {
	case stopped != "":
	case review == "":
		stopped = fmt.Sprintf("the architect of %s made no envoy_publish to %s, its parent's role topic, naming version 2 of its Verification document, "+
			"so its parent never heard of the version to review it, and the run did not reach its assertion", child.key, parentTopic)
	default:
		r.evidence("version 2 of %s's Verification document %s holds %q", child.key, child.verification, sessionsCheckAgain)
	}
	var why []string
	if told == "" {
		why = append(why, fmt.Sprintf("the reviewer of %s was sent no Envoy message naming version 2 and the change", child.key))
	} else {
		r.evidence("the reviewer of %s's transcript holds the Envoy message %s", child.key, sessionsExcerpt(told))
	}
	if missed := r.publishedBy(architect, "the architect of "+child.key, child, claim.RoleReviewer, change); missed != "" {
		why = append(why, missed)
	}
	if asks := r.approvalAsks(child.key); len(asks) > 0 {
		why = append(why, fmt.Sprintf("Dispatch holds approval requests %v on %s", asks, child.key))
	} else {
		r.evidence("Dispatch holds no approval request on %s", child.key)
	}
	return sessionsJudged(stopped, why)
}

// parentStart is the start task the root architect of the a4 step holds at the boot, at the entry
// the daemon's start path raised for it, in place of that start's own task: that the proof runs
// no repository, where its tree stands, and that the operator reviews each version of child's
// documents in its place, posting the review as its `Spec review of` comment and telling child's
// architect in its name (reviewAsParent). So it reviews none of them itself, and each version gets
// one review. It says nothing of who tells the child's reviewer.
func parentStart(root, child string) string {
	return fmt.Sprintf("This proof runs no GitHub and no repository, and you have no GitHub access, so run no `gh`, `git` or `jj`. You hold the "+
		"tree of %s, and its work stands where your earlier session left it: %s, the root, in admitted, waiting for its child; %s, its child, "+
		"in reviewing, where its reviewer and its own architect hold it. In this proof the operator of this tree reviews each version of "+
		"%s's spec and Verification document in your place: it posts that review on %s as your `Spec review of` comment and tells %s's "+
		"architect of it in your name, so review none of them yourself. Start no work now: reply with one line that starts with WAITING, "+
		"and end your turn.", root, root, child, child, child, child)
}

// reviewAsParent is the a4 step's parent's review of version 2 of child's Verification document,
// which the proof makes in the place of root's architect once the child's architect has told that
// parent of the version. It is what a live parent posts, through the live parent's own channels,
// as the parent's prompt tells it to (skill://legion-architect/references/decomposition.md,
// "Reviewing a child's spec"): a `dispatch comment` on the child as the parent's own session,
// whose first two lines are those round 12's live root architect wrote when it approved this
// version, `Spec review of <child> v1 by <root>: approved` and `Verification document v2`, with
// its reason after them; then an Envoy message from the parent's session to the role topic of the
// child's architect, in the words round 13's live parent published its verdict in, `<root> review
// of <child>: <verdict>, spec v1 and Verification document v2`, naming the comment. The comment is
// read back as an approval (sessionsApprovedReview), and a review that does not read as one, or a
// message the listener refuses, fails the run's harness. It answers the comment's body.
func (r *sessionsRun) reviewAsParent(child, root *sessionsMember) string {
	t := r.t
	t.Helper()
	session := r.claim(r.token(root, claim.RoleArchitect)).Session
	if session == "" {
		t.Fatalf("the architect of %s holds no session to post its review of %s as", root.key, child.key)
	}
	body := fmt.Sprintf("Spec review of %s v1 by %s: approved\nVerification document v2\n\n"+
		"I read version 2 of the Verification document against the root spec at its approved version 1. It changes one check, %s, from %q "+
		"to %q. The check still serves the root's design, %q, narrows nothing of it and adds no scope.\n",
		child.key, root.key, sessionsCheckID, sessionsCheck, sessionsCheckAgain, sessionsDesign)
	var posted struct {
		ID string `json:"id"`
	}
	r.dispatchAgent(http.MethodPost, "/api/v1/issues/"+child.key+"/comments", map[string]any{
		"body": body, "actor": map[string]string{"kind": "session", "id": session},
	}, 0, &posted)
	if posted.ID == "" || sessionsApprovedReview(child.key, root.key, r.comments(child.key)) == "" {
		t.Fatalf("the review the proof posted on %s as %s's architect (comment %q) does not read back as an approval of version 2", child.key, root.key, posted.ID)
	}
	topic := notify.RoleTopicPrefix + string(r.token(child, claim.RoleArchitect))
	message := fmt.Sprintf("%s review of %s: approved, spec v1 and Verification document v2: dispatch://%s/comment/%s",
		root.key, child.key, child.key, posted.ID)
	encoded, err := json.Marshal(map[string]string{
		"source": "agent", "source_session": session, "topic": topic, "message": message, "idempotency_key": "a4-review-" + randomSuffix(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := r.s.client.Post(r.s.listener+"/v1/messages/publish", "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("publish the review of %s to %s as %s's architect: %v", child.key, topic, root.key, err)
	}
	answer, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode/100 != 2 {
		t.Fatalf("publish the review of %s to %s as %s's architect = %d %s", child.key, topic, root.key, response.StatusCode, answer)
	}
	r.note("the parent's review, posted as the session %s of the architect of %s: comment %s on %s, %s; and published to %s, %s",
		session, root.key, posted.ID, child.key, sessionsExcerpt(body), topic, sessionsExcerpt(message))
	return body
}

// publishedBy reads the transcript of the architect named who for its envoy_publish of the change
// to the role topic of role on member, which the architect's prompt asks for
// (prompts/go/architect-common.md). It prints the call it found as evidence and answers "", or
// answers why the step failed.
func (r *sessionsRun) publishedBy(architect sessionsTranscript, who string, member *sessionsMember, role claim.Role, change []string) string {
	r.t.Helper()
	topic := notify.RoleTopicPrefix + string(r.token(member, role))
	call, published := architect.publishedTo(topic, change...)
	if !published {
		return fmt.Sprintf("%s made no envoy_publish to %s, the role topic of the %s of %s, naming version 2 and the change",
			who, topic, role, member.key)
	}
	r.evidence("%s's envoy_publish to %s, the role topic of the %s of %s: %s %s", who, topic, role, member.key, call.name, sessionsExcerpt(call.text))
	return ""
}

// Step off. Under gates.design off a root in implementing and its child in reviewing are each held
// by a live worker, and the child has its own live architect, which its first phase start started,
// as it starts every child's (workflow.Engine.StartFirstPhase). The root architect is asked to
// write version 2 of the root's spec, which a deployment that gates nothing sends no news of: each
// holder hears of it only from the architect, by its own envoy_publish to that holder's role
// topic.
func (r *sessionsRun) off() (sessionsOutcome, string) {
	root := &sessionsMember{phase: phase.Implementing, roles: []claim.Role{claim.RoleArchitect, claim.RoleImplementer}, tasks: map[claim.Role]string{}}
	child := &sessionsMember{phase: phase.Reviewing, roles: []claim.Role{claim.RoleArchitect, claim.RoleReviewer}, tasks: map[claim.Role]string{}}
	r.tree(sessionsSpec("A reader that pages.", sessionsDesign+" The child issue builds the reader's fetch, and the root builds the cache in front of it.", sessionsLater), root, child)
	root.tasks[claim.RoleImplementer] = holderStart(root.key, phase.Implementing)
	child.tasks[claim.RoleReviewer] = holderStart(child.key, phase.Reviewing)
	r.boot()
	r.checkArm(root, claim.RoleArchitect, sessionsArchitectMark)
	if !r.settled(root, claim.RoleImplementer) || !r.settled(child, claim.RoleReviewer) {
		return sessionsSetup, "a holder did not end its start turn within the run's bound"
	}
	r.deliver(root, claim.RoleArchitect, architectTask(
		fmt.Sprintf("spec (document %s)", root.spec), root.key, sessionsDesign, sessionsDesignAgain,
		fmt.Sprintf("%s, the root, in implementing, where its implementer holds it; %s, its child, in reviewing, where its reviewer holds it", root.key, child.key)))
	change := sessionsSpecChange
	holders := []struct {
		member *sessionsMember
		role   claim.Role
	}{{root, claim.RoleImplementer}, {child, claim.RoleReviewer}}
	told := make([]string, len(holders))
	all := func() bool {
		for i, holder := range holders {
			if told[i] == "" {
				told[i] = r.transcript(holder.member, holder.role).toldOf(change...)
			}
		}
		return !slices.Contains(told, "")
	}
	var quiet time.Time
	r.until(r.deadline, func() bool { return all() || r.quiet(root, claim.RoleArchitect, &quiet) })
	r.until(time.Now().Add(sessionsGrace), all)
	architect := r.transcript(root, claim.RoleArchitect)
	r.printCalls(root, claim.RoleArchitect, architect.bash("dispatch "), "ran")
	r.printCalls(root, claim.RoleArchitect, architect.calling("envoy_publish"), "called")
	r.printCalls(child, claim.RoleArchitect, r.transcript(child, claim.RoleArchitect).calling("envoy_publish"), "called")
	written, text := r.version2(root.spec)
	unwritten := sessionsUnwritten(root.key+"'s spec", written, text, "two pages")
	if unwritten == "" {
		r.evidence("version 2 of %s's spec %s holds the new Design sentence", root.key, root.spec)
	}
	var why []string
	for i, holder := range holders {
		if told[i] == "" {
			why = append(why, fmt.Sprintf("no Envoy message naming version 2 and the change reached the %s of %s", holder.role, holder.member.key))
		} else {
			r.evidence("the %s of %s's transcript holds the Envoy message %s", holder.role, holder.member.key, sessionsExcerpt(told[i]))
		}
		if missed := r.publishedBy(architect, "the architect of "+root.key, holder.member, holder.role, change); missed != "" {
			why = append(why, missed)
		}
	}
	return sessionsJudged(unwritten, why)
}

// holderStart is the start task a holder of the a4 and off steps holds at the boot, at the entry the
// daemon's start path raised for it, in place of that start's own task, the phase's resume task
// (workflow.ResumePhaseTask: "Resume the existing phase work"). That task does not serve the step:
// this proof runs no repository and no pull request, so a holder that resumed its phase would find
// nothing to work on and spend its turns saying so. It waits instead, as a holder between turns
// does, for what the step sends it.
func holderStart(issue string, p phase.Phase) string {
	return fmt.Sprintf("This proof runs no GitHub and no repository: %s has no pull request or working copy to work on, and you have no "+
		"GitHub access, so run no `gh`, `git` or `jj`. You hold %s in %s, and its work stands where your earlier session left it. Start no "+
		"work now: reply with one line that starts with WAITING, and end your turn.", issue, issue, p)
}

// reviewerStart is the start task a reviewer of the answers step holds at the boot, at the entry
// the daemon's start path raised for it, in place of that start's own task, the phase's resume
// task: what its review has read so far and what it rests on, since this proof runs no pull
// request a review could read, and an order to wait. The resume task names neither, and the step
// needs both: the news of version 2 is stale or current against the section the review rests on.
func reviewerStart(issue, spec string) string {
	return fmt.Sprintf("This proof runs no GitHub: %s has no pull request to read and you have no GitHub access, so run no `gh` and no `git`. "+
		"You hold the review of %s, and it stands as follows. You read version 1 of its spec (document %s), and you checked the change "+
		"against its `## Design` section, which reads %q. The change makes the reader fetch one page on each call, which is what that "+
		"section asks. Your review rests on that section and on nothing else of the spec. Start no other work now: reply with one line "+
		"that starts with WAITING, and end your turn.", issue, issue, spec, sessionsDesign)
}

// Step answers. Two roots stand in reviewing, each under a live reviewer whose start task says its
// review rests on the spec's `## Design`. Version 2 of the first root's spec changes that section,
// and version 2 of the second's changes its `## Later work` alone; the person approves both, and
// the daemon sends each reviewer the news. The stale reviewer must move its issue back with a
// reason naming the version; the current one must comment on its issue naming the version, and
// stay.
func (r *sessionsRun) answers() (sessionsOutcome, string) {
	stale := &sessionsMember{phase: phase.Reviewing, roles: []claim.Role{claim.RoleReviewer}, tasks: map[claim.Role]string{}}
	current := &sessionsMember{phase: phase.Reviewing, roles: []claim.Role{claim.RoleReviewer}, tasks: map[claim.Role]string{}}
	r.tree(sessionsSpec("A reader that pages.", sessionsDesign, sessionsLater), stale)
	r.tree(sessionsSpec("A reader that pages.", sessionsDesign, sessionsLater), current)
	for _, member := range []*sessionsMember{stale, current} {
		member.tasks[claim.RoleReviewer] = reviewerStart(member.key, member.spec)
	}
	r.boot()
	for _, member := range []*sessionsMember{stale, current} {
		r.checkArm(member, claim.RoleReviewer, sessionsWorkerMark)
		if !r.settled(member, claim.RoleReviewer) {
			return sessionsSetup, fmt.Sprintf("the reviewer of %s did not end its start turn within the run's bound", member.key)
		}
	}
	r.note("both reviewers ended their start turns")
	r.edit(stale.spec, "ses_architect_"+strings.ToLower(stale.key), sessionsDesign, sessionsDesignAgain, 2)
	r.edit(current.spec, "ses_architect_"+strings.ToLower(current.key), sessionsLater, sessionsLaterAgain, 2)
	r.approve(stale.spec, 2)
	r.approve(current.spec, 2)
	r.note("version 2 of %s's spec changes `## Design`, version 2 of %s's changes `## Later work`; %s approved both", stale.key, current.key, versionNewsPerson)
	for _, member := range []*sessionsMember{stale, current} {
		news := ""
		if !r.until(r.deadline, func() bool {
			for _, prompt := range r.transcript(member, claim.RoleReviewer).user {
				if strings.Contains(prompt, "The approved design of") {
					news = prompt
					return true
				}
			}
			return false
		}) {
			return sessionsSetup, fmt.Sprintf("the reviewer of %s was never given the news of version 2", member.key)
		}
		r.evidence("the reviewer of %s was given the news %s", member.key, sessionsExcerpt(news))
	}
	commented := func() string {
		for _, body := range r.comments(current.key) {
			if sessionsVersion2.MatchString(body) {
				return body
			}
		}
		return ""
	}
	r.until(r.deadline, func() bool {
		return r.stateIssue(stale.key).Phase != phase.Reviewing && commented() != ""
	})
	for _, member := range []*sessionsMember{stale, current} {
		if !r.settled(member, claim.RoleReviewer) {
			r.evidence("the reviewer of %s was still in a turn or held a task at the run's bound, so what follows is what it did by then", member.key)
		}
	}
	staleRead, currentRead := r.transcript(stale, claim.RoleReviewer), r.transcript(current, claim.RoleReviewer)
	moves := func(read sessionsTranscript) []sessionsCall {
		var found []sessionsCall
		for _, call := range read.calling("legion") {
			if op, _ := call.args["op"].(string); op == "request_backward_move" || strings.Contains(call.text, "request_backward_move") {
				found = append(found, call)
			}
		}
		return found
	}
	staleMoves, currentMoves := moves(staleRead), moves(currentRead)
	r.printCalls(stale, claim.RoleReviewer, staleMoves, "called")
	r.printCalls(stale, claim.RoleReviewer, staleRead.bash("dispatch comment"), "ran")
	r.printCalls(current, claim.RoleReviewer, currentMoves, "called")
	r.printCalls(current, claim.RoleReviewer, currentRead.bash("dispatch comment"), "ran")
	var why []string
	named := false
	for _, call := range staleMoves {
		if reason, _ := call.args["reason"].(string); sessionsVersion2.MatchString(reason) || sessionsVersion2.MatchString(call.text) {
			named = true
		}
	}
	if got := r.stateIssue(stale.key).Phase; got == phase.Reviewing || !named {
		why = append(why, fmt.Sprintf("the stale reviewer of %s made no request_backward_move naming version 2 that moved it (it stands in %s)", stale.key, got))
	} else {
		r.evidence("%s left reviewing on its reviewer's request_backward_move naming version 2, and stands in %s: the move starts the implementer, "+
			"whose launch this proof's runtime refuses, so the daemon holds the issue once that launch fails", stale.key, got)
	}
	body := commented()
	if body == "" {
		why = append(why, fmt.Sprintf("the current reviewer of %s wrote no comment on it naming version 2", current.key))
	} else {
		r.evidence("Dispatch holds the comment on %s %s", current.key, sessionsExcerpt(body))
	}
	if got := r.stateIssue(current.key).Phase; got != phase.Reviewing || len(currentMoves) > 0 {
		why = append(why, fmt.Sprintf("the current reviewer of %s moved back (it stands in %s, %d request_backward_move calls)", current.key, got, len(currentMoves)))
	} else {
		r.evidence("%s stays in reviewing, and its reviewer made no request_backward_move", current.key)
	}
	if len(why) > 0 {
		return sessionsFailed, strings.Join(why, "; ")
	}
	return sessionsPassed, ""
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
