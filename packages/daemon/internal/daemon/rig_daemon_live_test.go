//go:build e2e

// TestRigDaemonLive is the label's whole daemon for one scored run of the skill-scenario rig
// (packages/pi-legion/scripts/skill-scenarios/label-texts.sh daemon): Task 11a's rig over the
// run's own PostgreSQL, NATS and Dispatch, with a GitHub stand-in of the test's own, and the
// world's agent a real pane that the daemon itself launches. It has no own case. Its parts that
// need no Oh My Pi, and their tests, are in rig_daemon_pane_test.go, which has no build tag.
//
// The run:
//
//  1. It reads the request (RIG_TEXTS_REQUEST) and the world's world.json, builds the pull
//     request's head in the pane's working directory, and records the world's passes at that head
//     in the run's Dispatch. A before_merge check with no pass there ends the run before the daemon
//     boots: no <ready> is written and no pane is launched.
//  2. It boots the daemon over rigRuntime: the moment's claim goes to localRuntime once its real
//     launch is due, and every other launch to the fake runtime, whose claims fake claimants work.
//  3. It moves the moment's claim to the real pane as C4's table says. The implementer of a retro
//     and the architect at a notice move by the controller's fresh_session after their last fake
//     step; the merger's first launch is the real one.
//  4. It writes <ready> once the daemon took the real pane's claims/ready, then reports W5's
//     defects as the fake implementer, writes <status> every 10 seconds, and marks the pull request
//     merged once the gh stand-in's log shows `gh pr merge` answered as merged. At SIGTERM it
//     writes <record> and passes.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

func TestRigDaemonLive(t *testing.T) {
	t.Parallel()
	files, err := rigDaemonLiveFiles(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(files.request)
	if err != nil {
		t.Fatal(err)
	}
	req, err := decodeRigDaemonRequest(data)
	if err != nil {
		t.Fatalf("the daemon request %s: %v", files.request, err)
	}
	world, err := readRigWorld(req)
	if err != nil {
		t.Fatal(err)
	}
	if req.PostgresURL != processDSN {
		t.Fatal("the request's postgresUrl and this binary's LEGION_TEST_PG_DSN differ: label-texts.sh daemon sets the one from the other, and the databases of this run are made on LEGION_TEST_PG_DSN's server")
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stop)

	l := newRigDaemonLive(t, req, world)
	l.transfer(stop)
	l.writeReady(files.ready)
	l.reportWorldDefects()
	l.serve(files, stop)
}

// rigDaemonLive is one scored run: the rig's daemon, the request and the world, the local runtime
// of the real pane and the runtime that routes to it, and what the run has read of the real pane.
type rigDaemonLive struct {
	*rigDaemon
	req rigDaemonRequest
	// file is the world's world.json; the world the daemon runs is the rig's (rigDaemon.world).
	file   rigWorldFile
	local  *localRuntime
	routes *rigRuntime
	// role is the real pane's role, and moment its claim.
	role   claim.Role
	moment claim.Token
	// paneURL is the daemon address the real pane is given: the recording handler, or the
	// intercept control's proxy in front of it.
	paneURL string
	// replaced is the fake claimant's session the real pane replaced, "" at a first launch;
	// session, generation and sessionDir are the real pane's, read from its registration and its
	// ready; readyAt is when the daemon took that ready.
	replaced, session, sessionDir string
	generation                    uint64
	readyAt                       time.Time
	// inTurnAt is the last moment the status poll found the moment's claim working, zero before
	// it found it so; merged is whether the run has published the pull request's merge.
	inTurnAt time.Time
	merged   bool
}

// newRigDaemonLive readies the run before its first fake step: the `legion` the pane's shim is,
// built from this checkout, the local runtime under the request's HOME and Oh My Pi profile, the
// pull request's head in the pane's working directory, the world's passes at that head, and the
// daemon booted over rigRuntime.
func newRigDaemonLive(t *testing.T, req rigDaemonRequest, world rigWorldFile) *rigDaemonLive {
	t.Helper()
	for _, tool := range []string{"git", "jj"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH: %v", tool, err)
		}
	}
	if info, err := os.Stat(req.Omp); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("omp %s is no executable file: %v", req.Omp, err)
	}
	token, err := os.ReadFile(req.DispatchTokenFile)
	if err != nil {
		t.Fatalf("read the Dispatch token file: %v", err)
	}
	proof := rigLiveProof(t, req)
	if err := os.MkdirAll(req.PaneLogs, 0o700); err != nil {
		t.Fatal(err)
	}
	local := newLocalRuntime(t, proof.cfg.StateDir, buildProofLegion(t, proofCheckoutRoot(t), t.TempDir()), req.Omp, req.Home, req.Profile)
	local.paneLogs = req.PaneLogs
	local.envoyURL, local.natsURLs = req.EnvoyURL, proof.cfg.NatsURLs
	local.dispatchURL, local.dispatchTokenFile = req.DispatchURL, req.DispatchTokenFile
	// The gh stand-in reads its run from SKILL_SCENARIO_RUN; the pane's gh becomes the stand-in once
	// the boot has written the launcher directory (rigPaneGH, below). The local runtime passes no
	// launch secret on, so the Envoy bearer is named here.
	local.environment = []string{
		"SKILL_SCENARIO_RUN=" + req.RunDir,
		"ENVOY_TOKEN_FILE=" + req.EnvoyTokenFile,
	}
	head := buildRigHead(t, req, world, local.workDir(req.Issue), t.TempDir())

	dispatch := &scratchDispatch{t: t, base: req.DispatchURL, token: strings.TrimSpace(string(token))}
	r := rigDaemonOver(t, proof, dispatch, newDoneByChecksGitHub(t), rigDaemonWorld{
		issue: req.Issue, title: world.Title, number: world.Pull, head: head, steps: req.Steps,
	})
	if err := passAtHead(r.dispatchAPI(), req.Issue, head, world.Passes); err != nil {
		t.Fatal(err)
	}
	t.Logf("the passes %v of %s are recorded at the head %s, and each before_merge check has one", world.Passes, req.Issue, head)

	l := &rigDaemonLive{rigDaemon: r, req: req, file: world, local: local, role: rigMomentRoles[req.Moment]}
	l.moment = r.token(l.role)
	l.routes = newRigRuntime(r.rt, local, l.moment)
	if req.Moment == "merging" {
		// The daemon first launches the merger at merging: that launch is the real one.
		l.routes.switchMoment()
	}
	daemonLog, err := os.OpenFile(filepath.Join(req.PaneLogs, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemonLog.Close() })
	r.daemonLog = slog.New(slog.NewJSONHandler(daemonLog, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var handed createdIssue
	if err := r.dispatchAPI().call(http.MethodGet, "/api/v1/issues/"+req.Issue, nil, &handed, http.StatusOK); err != nil {
		t.Fatalf("read %s, which the rig filed in the run's Dispatch: %v", req.Issue, err)
	}
	r.built = &built{}
	r.start(overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, apps appauth.Source,
			repository func(context.Context, string) (ghrepo.Repository, string, error), st *store.Store, cached func() []ghrepo.Repository) (runtime.Runtime, error) {
			r.built.mu.Lock()
			defer r.built.mu.Unlock()
			r.built.conns, r.built.address, r.built.apps, r.built.repository, r.built.store, r.built.cached = listener, address, apps, repository, st, cached
			local.conns, local.stream = listener, address
			return l.routes, nil
		},
		listen: heldListen,
	}, handed)
	l.paneURL = r.calls.url
	if req.PaneDaemon == "intercept-report" {
		proxy := httptest.NewServer(interceptReport(r.calls))
		t.Cleanup(proxy.Close)
		l.paneURL = proxy.URL
	}
	local.daemonURL = l.paneURL
	if err := rigPaneGH(proof.cfg.StateDir, req.RunDir); err != nil {
		t.Fatal(err)
	}
	return l
}

// rigLiveProof is newLiveProofOn over the run's NATS: the stream the daemon consumes is the run's
// own, which its Envoy listener has made already and may hold the run's messages, so it is made
// only where it is missing. The daemon publishes its notices to the run's listener with the run's
// Envoy bearer, and reads the run's Dispatch with the agent's bearer.
func rigLiveProof(t *testing.T, req rigDaemonRequest) liveProof {
	t.Helper()
	cfg := testConfig(t)
	cfg.NatsURLs = []string{req.NatsURL}
	cfg.DispatchTokenFile = req.DispatchTokenFile
	cfg.EnvoyURL, cfg.EnvoyTokenFile = req.EnvoyURL, req.EnvoyTokenFile
	p := liveProof{t: t, cfg: cfg, records: record.NewStore()}
	p.js = workflowJetStream(t, req.NatsURL)
	ctx := context.Background()
	if _, err := p.js.Stream(ctx, "ENVOY_NOTIFICATIONS"); errors.Is(err, jetstream.ErrStreamNotFound) {
		testnats.CreateStream(t, p.js, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	} else if err != nil {
		t.Fatalf("read the stream ENVOY_NOTIFICATIONS of the run's NATS: %v", err)
	}
	var err error
	if p.st, err = store.Open(ctx, cfg.PostgresDSN); err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(p.st.Close)
	if _, err := p.st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	return p
}

// buildRigHead makes workspace the pane's working copy as late_completion_live_test.go makes one
// for a real pane that completes through the plugin, and answers the pull request's head: a jj
// repository colocated with git, whose base commit holds the rig's worker fixture's README.md on
// main, and whose head is the world's head.patch over that base, on legion/<issue>, at an origin
// of the run's own under work. The commands run under the request's HOME, which the pane is told,
// so the pane's jj reads the same repository configuration.
func buildRigHead(t *testing.T, req rigDaemonRequest, world rigWorldFile, workspace, work string) string {
	t.Helper()
	run := fixturegit.Isolated(t, req.Home)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("make the pane's working directory %s: %v", workspace, err)
	}
	jj := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(run.Out(workspace, append([]string{"jj"}, args...)...))
	}
	run.Run(workspace, fixturegit.JJInit("", true)...)
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("# widgets\n\nSmall CLI helpers.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jj("commit", "-m", "init", "README.md")
	jj("bookmark", "set", "main", "-r", "@-")
	run.Run(workspace, "git", "apply", filepath.Join(req.World, "head.patch"))
	jj("commit", "-m", world.Title)
	branch := claim.IssueBranch(req.Issue)
	jj("bookmark", "set", branch, "-r", "@-")
	head := jj("log", "-r", "@-", "--no-graph", "-T", "commit_id")
	origin := filepath.Join(work, "origin.git")
	run.Run(work, fixturegit.InitBare(origin, "main")...)
	run.Run(work, "git", "--git-dir", origin, "fetch", "--quiet", jj("git", "root"),
		"refs/heads/main:refs/heads/main", "refs/heads/"+branch+":refs/heads/"+branch)
	jj("git", "remote", "add", "origin", origin)
	jj("git", "fetch", "--remote", "origin")
	if pushed := jj("log", "-r", fmt.Sprintf("remote_bookmarks(exact:%q, exact:\"origin\")", branch), "--no-graph", "-T", "commit_id"); pushed != head {
		t.Fatalf("%s@origin is %q, want the head %s", branch, pushed, head)
	}
	t.Logf("the pane's working copy %s stands on the head %s, which %s@origin is (%s)", workspace, head, branch, origin)
	return head
}

// transfer walks the earlier steps with fake claimants and moves the moment's claim to the real
// pane, as C4's table says, then waits for the daemon to take the real pane's ready.
func (l *rigDaemonLive) transfer(stop <-chan os.Signal) {
	t := l.t
	t.Helper()
	steps := l.req.Steps
	switch l.req.Moment {
	case "retro":
		at := slices.Index(steps, "implementer")
		if at < 0 {
			t.Fatalf("the steps %v name no implementer, whose retro is the moment", steps)
		}
		l.passSteps(0, at+1)
		l.freshSessionOf(claim.RoleImplementer)
		l.passSteps(at+1, len(steps))
	case "merging":
		l.passSteps(0, len(steps))
		rating, reason := 4, "The plan fit the work."
		l.work(claim.RoleImplementer, "", api.HandoffCompleteRequest{Summary: "retro written", Commit: l.world.head,
			PlanRating: &rating, PlanReason: &reason}, phase.Merging)
		t.Logf("the walk: the fake implementer's retro passed; %s stands in merging", l.key)
	case "notice":
		if len(steps) == 0 || steps[len(steps)-1] != "acceptance:owner" {
			t.Fatalf("the steps %v end in no acceptance:owner, the architect's sign-off the moment follows", steps)
		}
		l.passSteps(0, len(steps))
		l.freshSessionOf(claim.RoleArchitect)
	}
	l.awaitReady(stop)
}

// freshSessionOf moves role's claim from its fake claimant to the real pane: the next launch of
// the claim goes to the local runtime, the controller's fresh_session stops the claim and launches
// it again in a new session, and the fake claimant's shim connection closes.
func (l *rigDaemonLive) freshSessionOf(role claim.Role) {
	t := l.t
	t.Helper()
	fake := l.worker(role)
	l.replaced = fake.session
	l.routes.switchMoment()
	if l.controllerSession == "" {
		l.registerController()
	}
	if status, body := l.freshSession(role); status != http.StatusOK || strings.Contains(body, `"phaseNotRestarted"`) {
		t.Fatalf("the controller's fresh session of %s = %d %s, want 200 with no phaseNotRestarted", l.moment, status, body)
	}
	fake.dropConnection()
	delete(l.claims, l.moment)
	t.Logf("the controller's fresh session stopped %s in the fake claimant's session %s; its next launch is the real pane's", l.moment, l.replaced)
}

// rigCallBody is what the run reads of a registration's or a ready's body.
type rigCallBody struct {
	ClaimToken     string `json:"claimToken"`
	SessionID      string `json:"sessionId"`
	OmpSessionFile string `json:"ompSessionFile"`
	Generation     uint64 `json:"generation"`
}

// awaitReady waits for a claims/ready of the moment's claim that the daemon took (204) from a
// session other than the one the real pane replaced, then reads that session's registration. A
// real pane that ends first, and a SIGTERM that comes first, fail the run.
func (l *rigDaemonLive) awaitReady(stop <-chan os.Signal) {
	t := l.t
	t.Helper()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	for l.session == "" {
		select {
		case <-stop:
			t.Fatalf("SIGTERM came before the daemon took the ready of %s's real pane", l.moment)
		case <-poll.C:
		}
		for _, call := range l.calls.all() {
			var body rigCallBody
			if call.Method != http.MethodPost || call.Path != "/legion/v1/claims/ready" || call.Status != http.StatusNoContent ||
				json.Unmarshal(call.Body, &body) != nil || body.ClaimToken != string(l.moment) || body.SessionID == l.replaced {
				continue
			}
			l.session, l.generation = body.SessionID, body.Generation
			if l.readyAt, _ = time.Parse(time.RFC3339Nano, call.At); l.readyAt.IsZero() {
				l.readyAt = time.Now()
			}
			break
		}
		if l.session == "" && len(l.local.LaunchesOf(l.moment)) > 0 && !l.paneAlive() {
			t.Fatalf("the real pane of %s ended before the daemon took its ready; its logs are under %s", l.moment, l.req.PaneLogs)
		}
	}
	for _, call := range l.calls.all() {
		var body rigCallBody
		if call.Method == http.MethodPost && call.Path == "/legion/v1/claims/register" && call.Status == http.StatusOK &&
			json.Unmarshal(call.Body, &body) == nil && body.SessionID == l.session {
			l.sessionDir = filepath.Dir(body.OmpSessionFile)
		}
	}
	if l.sessionDir == "" {
		t.Fatalf("the recorded calls hold no registration of %s's real session %s", l.moment, l.session)
	}
	if held := l.d.claim(l.moment).Session; held != l.session {
		t.Fatalf("the daemon took the ready of %s from %s, and its claim names %q", l.moment, l.session, held)
	}
	t.Logf("the daemon took the ready of %s's real pane, session %s at generation %d, which replaced %q", l.moment, l.session, l.generation, l.replaced)
}

// rigDaemonReady is RigDaemonReady of scored-runs-contracts.ts.
type rigDaemonReady struct {
	DaemonURL    string  `json:"daemonUrl"`
	Claim        string  `json:"claim"`
	Session      string  `json:"session"`
	Replaced     *string `json:"replaced"`
	Generation   uint64  `json:"generation"`
	Head         string  `json:"head"`
	SystemPrompt string  `json:"systemPrompt"`
	SessionDir   string  `json:"sessionDir"`
}

// writeReady writes <ready>: the real pane's daemon address, its claim and session, the session
// it replaced, the launch's generation, the head, the prompt file the local runtime wrote for it,
// and the directory Oh My Pi writes its session in.
func (l *rigDaemonLive) writeReady(path string) {
	t := l.t
	t.Helper()
	prompt := runtime.SecretFilePath(l.cfg.StateDir, string(l.moment)+"-"+runtime.SystemPromptFile)
	if _, err := os.Stat(prompt); err != nil {
		t.Fatalf("the local runtime wrote no prompt file for %s: %v", l.moment, err)
	}
	var replaced *string
	if l.replaced != "" {
		replaced = &l.replaced
	}
	writeRigFile(t, path, rigDaemonReady{DaemonURL: l.paneURL, Claim: string(l.moment), Session: l.session, Replaced: replaced,
		Generation: l.generation, Head: l.world.head, SystemPrompt: prompt, SessionDir: l.sessionDir})
}

// reportWorldDefects is W5's plant: the fake implementer takes its retro's task and reports each
// defect the world names, once the architect's real pane is ready, so the daemon's notice of each
// reaches that pane.
func (l *rigDaemonLive) reportWorldDefects() {
	t := l.t
	t.Helper()
	if len(l.file.Reported) == 0 {
		return
	}
	if l.role == claim.RoleImplementer {
		t.Fatalf("the world names defects for the fake implementer to report, and the implementer is the real pane")
	}
	implementer := l.worker(claim.RoleImplementer)
	implementer.take()
	for _, text := range l.file.Reported {
		if status, body := l.reportDefect(implementer, text); status != http.StatusOK {
			t.Fatalf("POST %s as the fake implementer in retro = %d %s, want 200", rigDefectRoute, status, body)
		}
	}
	t.Logf("the fake implementer reported the world's %d defects", len(l.file.Reported))
}

// rigDaemonStatus is RigDaemonStatus of scored-runs-contracts.ts.
type rigDaemonStatus struct {
	At        string          `json:"at"`
	PaneAlive bool            `json:"paneAlive"`
	Turn      string          `json:"turn"`
	Activity  json.RawMessage `json:"activity"`
}

// serve runs until SIGTERM: once a second it reads the moment's claim and the gh stand-in's log,
// every 10 seconds it writes <status>, and at SIGTERM it writes <record> and returns.
func (l *rigDaemonLive) serve(files rigDaemonFiles, stop <-chan os.Signal) {
	l.t.Helper()
	l.writeStatus(files.status)
	written := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			writeRigFile(l.t, files.record, l.rigRecord())
			l.t.Logf("SIGTERM: the record of %s is written to %s", l.key, files.record)
			return
		case <-tick.C:
		}
		l.watchMerge()
		if time.Since(written) >= 10*time.Second {
			l.writeStatus(files.status)
			written = time.Now()
		} else if l.d.claim(l.moment).State == string(supervise.StateWorking) {
			l.inTurnAt = time.Now()
		}
	}
}

// writeStatus writes <status>: whether the real pane's process lives, whether its claim is in a
// turn, and the activity its session last reported after it was last seen in one (null while it
// is in one, and before it reported any).
func (l *rigDaemonLive) writeStatus(path string) {
	l.t.Helper()
	status := rigDaemonStatus{At: time.Now().UTC().Format(time.RFC3339Nano), PaneAlive: l.paneAlive(), Turn: "idle", Activity: json.RawMessage("null")}
	if l.d.claim(l.moment).State == string(supervise.StateWorking) {
		status.Turn, l.inTurnAt = "running", time.Now()
	} else {
		since := l.readyAt
		if l.inTurnAt.After(since) {
			since = l.inTurnAt
		}
		status.Activity = l.activitySince(since)
	}
	writeRigFile(l.t, path, status)
}

// activitySince is the activity of the newest claims/activity the real session sent after since,
// null when it sent none.
func (l *rigDaemonLive) activitySince(since time.Time) json.RawMessage {
	newest := json.RawMessage("null")
	for _, call := range l.calls.all() {
		var body struct {
			SessionID string          `json:"sessionId"`
			Activity  json.RawMessage `json:"activity"`
		}
		at, err := time.Parse(time.RFC3339Nano, call.At)
		if err != nil || call.Method != http.MethodPost || call.Path != "/legion/v1/claims/activity" || call.Status/100 != 2 ||
			json.Unmarshal(call.Body, &body) != nil || body.SessionID != l.session || !at.After(since) {
			continue
		}
		newest = body.Activity
	}
	return newest
}

// paneAlive is whether the moment's real pane, its newest launch, still runs.
func (l *rigDaemonLive) paneAlive() bool {
	l.local.mu.Lock()
	pane := l.local.panes[l.moment]
	l.local.mu.Unlock()
	if pane == nil {
		return false
	}
	select {
	case <-pane.done:
		return false
	default:
		return true
	}
}

// watchMerge reads the gh stand-in's log, and once it shows `gh pr merge` answered as merged it
// marks the pull request merged in the test's GitHub stand-in and publishes GitHub's closed and
// merged of it, as Task 11a's rig does (doneByChecksRig.merge). The daemon then moves the issue on
// by its own path.
func (l *rigDaemonLive) watchMerge() {
	if l.merged {
		return
	}
	data, err := os.ReadFile(l.req.GHLog)
	if err != nil {
		return
	}
	for line := range strings.Lines(string(data)) {
		var call struct {
			Argv []string `json:"argv"`
			Exit *int     `json:"exit"`
		}
		if json.Unmarshal([]byte(line), &call) != nil || call.Exit == nil || *call.Exit != 0 {
			continue
		}
		argv := call.Argv
		if len(argv) > 0 && argv[0] == "gh" {
			argv = argv[1:]
		}
		if len(argv) >= 2 && argv[0] == "pr" && argv[1] == "merge" {
			merge := l.standIn.merge()
			event := l.publishPullRequest("closed")
			l.merged = true
			l.t.Logf("the gh stand-in answered `gh %s` as merged: GitHub's stand-in merged #%d as %s, envelope %s", strings.Join(argv, " "), l.number, merge, event)
			return
		}
	}
}

// writeRigFile writes v as JSON to path, through a file beside it that it then renames, so a
// reader never reads half of it.
func writeRigFile(t *testing.T, path string, v any) {
	t.Helper()
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	partial := path + ".partial"
	if err := os.WriteFile(partial, append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(partial, path); err != nil {
		t.Fatal(err)
	}
}
