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
// database, a state directory and a Dispatch project of its own.
//
// What is stood in for lies outside what a session decides. GitHub is the package's
// repositoriesGitHub, and no session has a GitHub credential. The two Apps' tokens are
// workflowTokenRecorder's placeholders. The daemon's own notices go to the package's
// noticeListener, so a session hears what the daemon sends only as a task on its worker stream,
// and what it hears over Envoy is another session's message. The runtime launches only the claims
// a run seeds: a phase the daemon starts beyond them (the implementer a stale reviewer's move back
// starts) is refused, so no session runs that the step did not ask for.
//
// Two arms. The head arm launches each pane on the prompts the daemon snapshots below its state
// directory at boot (prompts.New). The base arm writes the base commit's prompt files,
// APPROVED_VERSION_NEWS_BASE_PROMPTS, over that snapshot before the first pane starts: a pane
// reads its role prompt from those files at launch (specs.rolePromptPaths, promptArgument), so the
// arm needs no second daemon. The addressing the daemon appends after the files is this
// checkout's in both arms. Each run reads the prompt back from its pane's own command line and
// ends the proof when the arm it ran is not the one it meant.
//
// Three steps, each run three times in each arm, each run on trees of its own:
//   - a4-the-architect-tells-the-child-reviewer: under root-issues, a root waiting in admitted and
//     its child in reviewing, whose reviewer is live; the root architect is asked to write version
//     2 of the child's Verification document. It passes when that version exists, the reviewer's
//     transcript holds an Envoy message naming version 2 and the changed check, and the child has
//     no approval request;
//   - off-the-architect-tells-the-holders: under gates.design off, a root in implementing and its
//     child in reviewing, both holders live; the architect is asked to write version 2 of the
//     root's spec. It passes when that version exists and each holder's transcript holds an Envoy
//     message naming version 2 and the changed section;
//   - answers-a-stale-reviewer-moves-back-a-current-one-comments: under root-issues, two roots in
//     reviewing, each reviewer told what its review rests on; the person approves a version 2 of
//     each spec, one changing the section the review rests on and one a section it does not touch.
//     It passes when the first reviewer's request_backward_move names version 2 and moves its
//     issue back, and the second writes a comment on its issue naming version 2 and stays.
//
// Each run prints `== <step> <arm> <n>`, its evidence (the tool calls and the messages, from the
// sessions' own transcripts) and `run <step> <arm> <n>: PASS|FAIL …` with its time and the model
// calls it made, which the script counts. A head run that fails ends the proof there, since the
// proof then fails whatever the rest show, and every model call after it would be spent for
// nothing; a base run's verdict is the control's, and never fails the driver.
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
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testbuild"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The script's inputs to this driver. The key file is a path, never the key: the script exports
// nothing of the key, so the builds and the broker the lib starts never hold it.
const (
	sessionsKeyFileEnv     = "APPROVED_VERSION_NEWS_MODEL_KEY_FILE"
	sessionsBasePromptsEnv = "APPROVED_VERSION_NEWS_BASE_PROMPTS"
	sessionsBaseCommitEnv  = "APPROVED_VERSION_NEWS_BASE_COMMIT"
	sessionsModelEnv       = "APPROVED_VERSION_NEWS_MODEL"
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

// The two arms: the prompts this checkout composes, and the base commit's.
const (
	sessionsHead = "head"
	sessionsBase = "base"
)

// sessionsRuns is how many runs each arm makes of each step.
const sessionsRuns = 3

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

// The words each document is written with and changed to, and the words of the change a message
// must name.
const (
	sessionsDesign      = "The reader takes one page at a time."
	sessionsDesignAgain = "The reader takes two pages at a time, and keeps the second for the next read."
	sessionsLater       = "A later issue, not this one, adds an export of the pages to CSV."
	sessionsLaterAgain  = "A later issue, not this one, adds an export of the pages to CSV and to JSON."
	sessionsCheck       = "The read answers 200."
	sessionsCheckAgain  = "The read answers 200 with the page it read."
)

// The sentence of each arm's prompt the run reads back from a pane's own command line: the head's
// carries it and the base commit's does not. The architect's is its answer to a version the daemon
// sends no news of (prompts/go/architect-common.md); a phase worker's is its answer to a news task
// (prompts/go/worker-common.md).
const (
	sessionsArchitectMark = "Send that news yourself once you write such a version"
	sessionsWorkerMark    = "A news task tells you that a person approved a new version of your tree's design."
)

// sessionsVersion2 is a text naming version 2 of a document as a word of its own.
var sessionsVersion2 = regexp.MustCompile(`(?i)\b(version\s*2|v2)\b`)

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

// TestRealSessionsTellAndAnswerANewVersionLive runs each step three times in the head arm and
// then three times in the base arm, and stops at the first head run that fails. A run that
// -test.run leaves out is neither, which is how one run is driven alone.
func TestRealSessionsTellAndAnswerANewVersionLive(t *testing.T) {
	fixtureenv.Live(t)
	s := newSessionsLive(t)
	steps := []struct {
		name  string
		gates config.DesignGate
		body  func(*sessionsRun) (bool, string)
	}{
		{sessionsA4, config.DesignGateRootIssues, (*sessionsRun).a4},
		{sessionsOff, config.DesignGateOff, (*sessionsRun).off},
		{sessionsAnswers, config.DesignGateRootIssues, (*sessionsRun).answers},
	}
	runs := 0
	for _, step := range steps {
		for _, arm := range []string{sessionsHead, sessionsBase} {
			for n := 1; n <= sessionsRuns; n++ {
				ran, passed := s.run(step.name, arm, n, step.gates, step.body)
				if !ran {
					continue
				}
				runs++
				if !passed && arm == sessionsHead {
					fmt.Printf("model calls: %d in all, over %d runs\n", s.calls.count(), runs)
					t.Fatalf("%s %s run %d failed, so the head arm does not pass every run; its evidence is above", step.name, arm, n)
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
// Verification document with its one check ("" for none), and the start task each role's claim
// holds at the boot ("" for none).
type sessionsMember struct {
	key, tree, parent string
	phase             phase.Phase
	roles             []claim.Role
	spec              string
	verification      string
	check             string
	tasks             map[claim.Role]string
}

// run makes one run in a subtest of its own, whose cleanup stops its daemon, and answers whether
// it ran, which a -test.run pattern can leave out, and whether it passed. Its sessions end with its
// verdict, before anything else of the run is read, so none spends a model call past it. A fault
// of the harness fails the subtest and ends the proof, since no count of runs means anything past
// it; the step's own verdict is the answer.
func (s *sessionsLive) run(step, arm string, n int, gates config.DesignGate, body func(*sessionsRun) (bool, string)) (ran, passed bool) {
	name := fmt.Sprintf("%s %s %d", step, arm, n)
	succeeded := s.t.Run(fmt.Sprintf("%s-%s-%d", step, arm, n), func(t *testing.T) {
		ran = true
		started := time.Now()
		calls := s.calls.count()
		fmt.Printf("== %s\n", name)
		r := s.newRun(t, name, arm, gates)
		why := "the harness failed; its log is above"
		defer func() {
			r.rt.killEverything()
			if r.d != nil {
				r.keepTranscripts()
			}
			verdict := "PASS"
			if !passed {
				verdict = "FAIL: " + why
			}
			fmt.Printf("run %s: %s in %ds, %d model calls\n", name, verdict, int(time.Since(started).Seconds()), s.calls.count()-calls)
		}()
		passed, why = body(r)
	})
	if !succeeded {
		s.t.Fatalf("the harness failed in run %s; its log is above", name)
	}
	return ran, passed
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

// seed records every member as the daemon would hold it mid-run, as part one's seed does (an
// admission slot, an open lifecycle, an open pull request unless it stands in admitted, and each
// of its roles a launch a previous daemon began, which the boot relaunches), with the start task
// its role holds where it holds one. A root whose deployment gates spec holds its gate open at
// version 1, which the person approved on Dispatch too, and each member in a phase with work holds
// the told row of version 1. No role prompt file is written below the state directory: such a
// file is a launch's whole prompt (specs.rolePromptPaths), and the composed prompts would reach
// no pane.
func (r *sessionsRun) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
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
	one := 1
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		for _, member := range r.members {
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
			if gated && member.spec != "" {
				if err := r.records.PutGate(ctx, tx, record.DesignGate{
					Issue: member.key, Step: record.DesignStepSpec, ArtifactID: member.spec, LatestVersion: 1, ApprovedVersion: &one, EverOpened: true,
				}); err != nil {
					return err
				}
			}
			if err := r.admit(ctx, tx, member.key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	for _, member := range r.members {
		if member.parent == "" {
			if _, err := r.st.OpenTreeLifecycle(ctx, r.project, member.key, treelifecycle.AuthorityWorkflow); err != nil {
				t.Fatalf("open the lifecycle of %s: %v", member.key, err)
			}
		}
		for _, role := range member.roles {
			token := r.token(member, role)
			r.launchable[token] = true
			if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
				Token: token, Project: r.project, Tree: member.tree, Issue: member.key, Role: role, Generation: 1,
				State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
			}); err != nil {
				t.Fatalf("record the %s's unfinished launch on %s: %v", role, member.key, err)
			}
			if task := member.tasks[role]; task != "" {
				if err := r.st.PutDelivery(ctx, token, supervise.Delivery{
					ID: "seeded-start-" + randomSuffix(t), Task: task, Generation: 1, Phase: member.phase, QueuedAt: time.Now(),
				}); err != nil {
					t.Fatalf("record the start task of %s: %v", token, err)
				}
			}
		}
		if gated && member.phase != phase.Admitted {
			if _, err := r.st.Pool().Exec(ctx, `insert into design_news_told (issue, audience, versions, opening_owed, told_at)
				values ($1, 'holder', '{"spec": 1}'::jsonb, false, now())`, member.key); err != nil {
				t.Fatalf("seed the told row of %s: %v", member.key, err)
			}
		}
		// The root architect took version 1 too: the approval of version 1 that Dispatch carries
		// is the one its gate was opened on, and is news to nobody.
		if gated && member.parent == "" {
			if _, err := r.st.Pool().Exec(ctx, `insert into design_news_told (issue, audience, versions, opening_owed, told_at)
				values ($1, 'architect', '{"spec": 1}'::jsonb, false, now())`, member.key); err != nil {
				t.Fatalf("seed the architect's told row of %s: %v", member.key, err)
			}
		}
	}
}

// boot boots the whole daemon over the local runtime, in the base arm after writing the base
// commit's prompt files over the snapshot the boot wrote, and waits for every seeded claim's pane
// to report ready.
func (r *sessionsRun) boot() {
	t := r.t
	t.Helper()
	r.seed()
	r.daemon = newRecordingLog(r.log())
	gated := &sessionsRuntime{localRuntime: r.rt, launchable: r.launchable}
	r.d = startDaemonWithin(t, r.cfg, overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Tokens,
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

// checkArm reads the prompt the pane of role on member was launched with from its own command
// line and fails the run's harness when it is not the arm's: the head arm's carries mark and the
// base arm's does not.
func (r *sessionsRun) checkArm(member *sessionsMember, role claim.Role, mark string) {
	r.t.Helper()
	token := r.token(member, role)
	prompt := r.launchedPrompt(token)
	carries := strings.Contains(prompt, mark)
	if carries != (r.arm == sessionsHead) {
		r.t.Fatalf("the %s arm's %s was launched with a prompt of %d bytes that carries the head's sentence %q: %t", r.arm, token, len(prompt), mark, carries)
	}
	said := "lacks"
	if carries {
		said = "carries"
	}
	r.note("%s runs the %s arm's prompt: its own command line %s the head's sentence %q", token, r.arm, said, mark)
}

// launchedPrompt is the `--append-system-prompt` word token's Oh My Pi was started with, read from
// /proc as role_prompt_rule_live_test.go reads it: the pid the locator carries is the shim's, whose
// child is Oh My Pi.
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
				return argv[i+1]
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

// sessionsTranscript is what one session's own transcript holds, as this proof reads it: the text
// of each Envoy message card, of each prompt it was given, and each tool call its model made.
type sessionsTranscript struct {
	envoy []string
	user  []string
	calls []sessionsCall
}

// sessionsCall is one tool call: the tool, its arguments, and the call as its transcript writes it.
// A call through the tool's xd:// device is the tool's own; eval code is the tool `eval`, whose
// code is its text.
type sessionsCall struct {
	name string
	args map[string]any
	text string
}

// transcript reads the session file token's pane registered, which is empty until the session
// writes it.
func (r *sessionsRun) transcript(member *sessionsMember, role claim.Role) sessionsTranscript {
	r.t.Helper()
	var read sessionsTranscript
	file := r.claim(r.token(member, role)).SessionFile
	if file == "" {
		return read
	}
	body, err := os.ReadFile(file)
	if err != nil {
		return read
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		var entry struct {
			Type       string          `json:"type"`
			CustomType string          `json:"customType"`
			Content    json.RawMessage `json:"content"`
			Message    *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		switch {
		case entry.Type == "custom_message" && entry.CustomType == "envoy-message":
			read.envoy = append(read.envoy, sessionsText(entry.Content))
		case entry.Type == "message" && entry.Message != nil && entry.Message.Role == "user":
			read.user = append(read.user, sessionsText(entry.Message.Content))
		case entry.Type == "message" && entry.Message != nil && entry.Message.Role == "assistant":
			var parts []struct {
				Type      string          `json:"type"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(entry.Message.Content, &parts)
			for _, part := range parts {
				if part.Type == "toolCall" {
					read.calls = append(read.calls, sessionsToolCall(part.Name, part.Arguments))
				}
			}
		}
	}
	return read
}

// sessionsText is a message's content as text: the string itself, or its text parts joined.
func sessionsText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(content, &parts)
	said := make([]string, 0, len(parts))
	for _, part := range parts {
		said = append(said, part.Text)
	}
	return strings.Join(said, "\n")
}

// sessionsToolCall is one call as this proof reads it, a device write read as its tool's call.
func sessionsToolCall(name string, raw json.RawMessage) sessionsCall {
	var args map[string]any
	_ = json.Unmarshal(raw, &args)
	if path, _ := args["path"].(string); name == "write" && strings.HasPrefix(strings.TrimSpace(path), "xd://") {
		content, _ := args["content"].(string)
		var inner map[string]any
		_ = json.Unmarshal([]byte(content), &inner)
		return sessionsCall{name: strings.TrimPrefix(strings.TrimSpace(path), "xd://"), args: inner, text: content}
	}
	if code, _ := args["code"].(string); name == "eval" {
		return sessionsCall{name: name, args: args, text: code}
	}
	return sessionsCall{name: name, args: args, text: string(raw)}
}

// calling is every call of tool, by itself, its device, or eval code that names it.
func (read sessionsTranscript) calling(tool string) []sessionsCall {
	var found []sessionsCall
	for _, call := range read.calls {
		if call.name == tool || (call.name == "eval" && strings.Contains(call.text, tool)) {
			found = append(found, call)
		}
	}
	return found
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

// toldOf is the first Envoy message card that names version 2 and any of change, and "" when none
// does.
func (read sessionsTranscript) toldOf(change ...string) string {
	for _, card := range read.envoy {
		if !sessionsVersion2.MatchString(card) {
			continue
		}
		for _, words := range change {
			if strings.Contains(strings.ToLower(card), strings.ToLower(words)) {
				return card
			}
		}
	}
	return ""
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
// directory, under the run's own name.
func (r *sessionsRun) keepTranscripts() {
	dir := filepath.Join(r.s.evidence, strings.ReplaceAll(r.name, " ", "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.t.Errorf("make %s: %v", dir, err)
		return
	}
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
	}
	r.note("transcripts kept in %s", dir)
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

// architectTask is the task the driver gives a root architect: one edit of one document, the
// tree as its catch-up would show it, and nothing about who to tell.
func architectTask(document, issue, find, with, tree string) string {
	return fmt.Sprintf("A task from the operator of this tree. Write a new version, version 2, of the %s of %s. "+
		"In it, replace the words `%s` with `%s`. Make that one edit with `dispatch doc-edit`, and change nothing else "+
		"in that document. Your tree stands as follows: %s.", document, issue, find, with, tree)
}

// Step A4. A root waits in admitted while its child stands in reviewing under a live reviewer;
// the root's gate stands open at version 1 of its spec. The root architect is asked to write
// version 2 of the child's Verification document. The child's checks are the architect's to
// change and no person approves them, so the daemon sends no news of it: the reviewer hears of it
// only from the architect.
func (r *sessionsRun) a4() (bool, string) {
	root := &sessionsMember{phase: phase.Admitted, roles: []claim.Role{claim.RoleArchitect}}
	child := &sessionsMember{phase: phase.Reviewing, roles: []claim.Role{claim.RoleReviewer}, check: "c-reader-pages", tasks: map[claim.Role]string{}}
	r.tree(sessionsSpec("A reader that pages.", sessionsDesign, sessionsLater), root, child)
	child.tasks[claim.RoleReviewer] = holderStart(child.key, phase.Reviewing)
	r.boot()
	r.checkArm(root, claim.RoleArchitect, sessionsArchitectMark)
	if !r.settled(child, claim.RoleReviewer) {
		return false, fmt.Sprintf("the reviewer of %s did not end its start turn within the run's bound", child.key)
	}
	r.deliver(root, claim.RoleArchitect, architectTask(
		fmt.Sprintf("Verification document (`verification.md`, document %s)", child.verification), child.key, sessionsCheck, sessionsCheckAgain,
		fmt.Sprintf("%s, the root, in admitted, waiting for its child; %s, its child, in reviewing, where its reviewer holds it", root.key, child.key)))
	change := []string{child.check, "with the page it read"}
	told := ""
	var quiet time.Time
	r.until(r.deadline, func() bool {
		told = r.transcript(child, claim.RoleReviewer).toldOf(change...)
		return told != "" || r.quiet(root, claim.RoleArchitect, &quiet)
	})
	architect := r.transcript(root, claim.RoleArchitect)
	r.printCalls(root, claim.RoleArchitect, architect.bash("dispatch "), "ran")
	r.printCalls(root, claim.RoleArchitect, architect.calling("envoy_publish"), "called")
	if told == "" {
		// The architect settled: a message it sent may still be on its way.
		r.until(time.Now().Add(sessionsGrace), func() bool {
			told = r.transcript(child, claim.RoleReviewer).toldOf(change...)
			return told != ""
		})
	}
	written, text := r.version2(child.verification)
	if !written || !strings.Contains(text, sessionsCheckAgain) {
		return false, fmt.Sprintf("Dispatch holds no version 2 of %s's Verification document with the new words", child.key)
	}
	r.evidence("version 2 of %s's Verification document %s holds %q", child.key, child.verification, sessionsCheckAgain)
	if told == "" {
		return false, fmt.Sprintf("the reviewer of %s was sent no Envoy message naming version 2 and the change", child.key)
	}
	r.evidence("the reviewer of %s's transcript holds the Envoy message %s", child.key, sessionsExcerpt(told))
	// The architect may still be in its turn: a request for approval would come in it.
	r.settled(root, claim.RoleArchitect)
	if asks := r.approvalAsks(child.key); len(asks) > 0 {
		return false, fmt.Sprintf("Dispatch holds approval requests %v on %s", asks, child.key)
	}
	r.evidence("Dispatch holds no approval request on %s", child.key)
	return true, ""
}

// Step off. Under gates.design off a root in implementing and its child in reviewing are each held
// by a live worker. The root architect is asked to write version 2 of the root's spec, which a
// deployment that gates nothing sends no news of: each holder hears of it only from the architect.
func (r *sessionsRun) off() (bool, string) {
	root := &sessionsMember{phase: phase.Implementing, roles: []claim.Role{claim.RoleArchitect, claim.RoleImplementer}, tasks: map[claim.Role]string{}}
	child := &sessionsMember{phase: phase.Reviewing, roles: []claim.Role{claim.RoleReviewer}, tasks: map[claim.Role]string{}}
	r.tree(sessionsSpec("A reader that pages.", sessionsDesign+" The child issue builds the reader's fetch, and the root builds the cache in front of it.", sessionsLater), root, child)
	root.tasks[claim.RoleImplementer] = holderStart(root.key, phase.Implementing)
	child.tasks[claim.RoleReviewer] = holderStart(child.key, phase.Reviewing)
	r.boot()
	r.checkArm(root, claim.RoleArchitect, sessionsArchitectMark)
	if !r.settled(root, claim.RoleImplementer) || !r.settled(child, claim.RoleReviewer) {
		return false, "a holder did not end its start turn within the run's bound"
	}
	r.deliver(root, claim.RoleArchitect, architectTask(
		fmt.Sprintf("spec (document %s)", root.spec), root.key, sessionsDesign, sessionsDesignAgain,
		fmt.Sprintf("%s, the root, in implementing, where its implementer holds it; %s, its child, in reviewing, where its reviewer holds it", root.key, child.key)))
	change := []string{"Design", "two pages"}
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
	if written, text := r.version2(root.spec); !written || !strings.Contains(text, "two pages") {
		return false, fmt.Sprintf("Dispatch holds no version 2 of %s's spec with the new words", root.key)
	}
	r.evidence("version 2 of %s's spec %s holds the new Design sentence", root.key, root.spec)
	var missing []string
	for i, holder := range holders {
		if told[i] == "" {
			missing = append(missing, fmt.Sprintf("the %s of %s", holder.role, holder.member.key))
			continue
		}
		r.evidence("the %s of %s's transcript holds the Envoy message %s", holder.role, holder.member.key, sessionsExcerpt(told[i]))
	}
	if len(missing) > 0 {
		return false, fmt.Sprintf("no Envoy message naming version 2 and the change reached %s", strings.Join(missing, " or "))
	}
	return true, ""
}

// holderStart is the start task a holder of the a4 and off steps holds at the boot, in place of the
// resume the daemon would send a holder with none: this proof runs no repository and no pull
// request, so a holder that resumed its phase would find nothing to work on and spend its turns
// saying so. It waits instead, as a holder between turns does, for what the step sends it.
func holderStart(issue string, p phase.Phase) string {
	return fmt.Sprintf("This proof runs no GitHub and no repository: %s has no pull request or working copy to work on, and you have no "+
		"GitHub access, so run no `gh`, `git` or `jj`. You hold %s in %s, and its work stands where your earlier session left it. Start no "+
		"work now: reply with one line that starts with WAITING, and end your turn.", issue, issue, p)
}

// reviewerStart is the start task a reviewer of the answers step holds at the boot: what its
// review has read so far and what it rests on, since this proof runs no pull request a review
// could read, and an order to wait.
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
func (r *sessionsRun) answers() (bool, string) {
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
			return false, fmt.Sprintf("the reviewer of %s did not end its start turn within the run's bound", member.key)
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
			return false, fmt.Sprintf("the reviewer of %s was never given the news of version 2", member.key)
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
		r.settled(member, claim.RoleReviewer)
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
		return false, strings.Join(why, "; ")
	}
	return true, ""
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
