package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// pauseOperatorToken is the operator bearer of the pause commands' test daemon.
const pauseOperatorToken = "operator-bearer-for-pause-tests"

// pauseCommandClock is the test daemon's pause clock, moved by the test alone.
type pauseCommandClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *pauseCommandClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *pauseCommandClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// pauseState is the state route's source for the pause commands' test daemon: the daemon's own
// read of the project's pause on the state's snapshot (api.PauseStateTx, which the daemon's
// source.State calls), and the project's claims, under the rest of a state that holds nothing.
type pauseState struct {
	store *store.Store
	now   func() time.Time
}

func (s pauseState) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	state := api.State{
		Daemon:    api.DaemonInfo{Project: "LEGION", SchemaVersion: 1, Boots: 1},
		Admission: api.Admission{Cap: 2, Active: []string{}, Waiting: []string{}},
	}
	var err error
	if state.Paused, state.LastPause, err = api.PauseStateTx(ctx, tx, "LEGION", s.now()); err != nil {
		return api.State{}, nil, err
	}
	claims, err := s.store.ClaimsTx(ctx, tx)
	return state, claims, err
}

// pauseCommandDaemon is a daemon of project LEGION as `legion project` and `legion state` reach it:
// the real api.NewServer with the real pause service (internal/pause) over a database of the
// test's own, on a loopback port.
type pauseCommandDaemon struct {
	t         *testing.T
	store     *store.Store
	svc       *pause.Service
	clock     *pauseCommandClock
	port      int
	tokenFile string
}

func newPauseCommandDaemon(t *testing.T) *pauseCommandDaemon {
	t.Helper()
	st := testpg.CloneStore(t, "legion_cmd_pause")
	clock := &pauseCommandClock{at: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// A configuration that can hold a pause: runtime: kubernetes with the idle release on and a
	// wake pacing of 8 a minute (pause.Unavailable).
	capable := config.Config{Runtime: config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{IdleRelease: config.DefaultIdleRelease()}},
		WorkerWakesPerMinute: 8}
	svc, err := pause.New(pause.Options{Store: st, Records: record.NewStore(), Project: "LEGION", Config: capable, Log: quiet, Now: clock.now})
	if err != nil {
		t.Fatalf("build the pause service: %v", err)
	}
	server := httptest.NewServer(api.NewServer("127.0.0.1", 0, api.Options{
		State: pauseState{store: st, now: clock.now}, StateTransactions: st.Pool(),
		Project: "legion", DispatchProject: "LEGION", OperatorToken: pauseOperatorToken, Pause: svc, Log: quiet,
	}).Handler)
	t.Cleanup(server.Close)
	return &pauseCommandDaemon{t: t, store: st, svc: svc, clock: clock, port: server.Listener.Addr().(*net.TCPAddr).Port,
		tokenFile: writeFile(t, "operator-token", pauseOperatorToken+"\n")}
}

// legion runs `legion <args>` against the daemon's port, and holds its output to the one rule every
// run keeps: the operator token is in neither stream, whatever happened.
func (d *pauseCommandDaemon) legion(args ...string) (int, string, string) {
	d.t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), append(append([]string{"legion"}, args...), "--port", strconv.Itoa(d.port)), &out, &errb)
	for name, stream := range map[string]string{"stdout": out.String(), "stderr": errb.String()} {
		if strings.Contains(stream, pauseOperatorToken) {
			d.t.Fatalf("legion %v printed the operator token on %s: %q", args, name, stream)
		}
	}
	return code, out.String(), errb.String()
}

// succeed runs `legion <args>` and requires exit 0 with nothing on stderr.
func (d *pauseCommandDaemon) succeed(args ...string) string {
	d.t.Helper()
	code, out, errb := d.legion(args...)
	if code != 0 || errb != "" {
		d.t.Fatalf("legion %v = %d, stderr %q; want 0 and no stderr", args, code, errb)
	}
	return out
}

// pauses is how many pauses the store holds.
func (d *pauseCommandDaemon) pauses() int {
	d.t.Helper()
	var n int
	if err := d.store.Pool().QueryRow(context.Background(), `select count(*) from project_pauses`).Scan(&n); err != nil {
		d.t.Fatalf("count the pauses: %v", err)
	}
	return n
}

// stateLines is every line `legion state` printed that begins with prefix.
func stateLines(out, prefix string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestPauseCommands is the CLI half of check k5 of dispatch://INFRA-3759: `legion project pause
// --reason <text>` and `legion project resume` pause and resume the project with the operator's
// token and record the operator; `legion state` prints one line with the pause's since, by, reason
// and the roles it waits on while the project is paused, and one line with the last pause and who
// ended it while it runs; and `legion config check` names each setting under which a daemon cannot
// hold a pause.
func TestPauseCommands(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (legionState).
	t.Run("pause, state, resume, state", func(t *testing.T) {
		d := newPauseCommandDaemon(t)
		// A live role of the project, whose stop the pause asks and waits on.
		if err := d.store.PutClaim(context.Background(), supervise.Claim{Token: "legion-legion-legion-208-implementer",
			Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: legionclaim.RoleImplementer, Generation: 1,
			State: supervise.StateIdle}); err != nil {
			t.Fatalf("seed the implementer's claim: %v", err)
		}
		reach := []string{"--operator-token-file", d.tokenFile}
		const reason = "the model provider is refusing every turn"

		// No --reason is a usage error that reaches nothing, a blank one the daemon's refusal, and
		// a wrong bearer the operator routes' 403: none of them pauses anything.
		if code, _, errb := d.legion(append([]string{"project", "pause"}, reach...)...); code != 2 || !strings.Contains(errb, "--reason is required") {
			t.Fatalf("legion project pause with no --reason = %d, stderr %q; want 2 naming --reason", code, errb)
		}
		if code, _, errb := d.legion(append([]string{"project", "pause", "--reason", "  "}, reach...)...); code != 1 || !strings.Contains(errb, "400") || !strings.Contains(errb, "needs a reason") {
			t.Fatalf("legion project pause with a blank reason = %d, stderr %q; want 1 and the daemon's 400", code, errb)
		}
		wrong := writeFile(t, "wrong-token", "not-the-operator\n")
		if code, _, errb := d.legion("project", "pause", "--reason", reason, "--operator-token-file", wrong); code != 1 || !strings.Contains(errb, "403") {
			t.Fatalf("legion project pause with a wrong token = %d, stderr %q; want 1 and 403", code, errb)
		}
		if n := d.pauses(); n != 0 {
			t.Fatalf("the refused pauses left %d pauses, want none", n)
		}

		paused := d.succeed(append([]string{"project", "pause", "--reason", reason}, reach...)...)
		open, found, err := d.svc.OpenPause(context.Background())
		if err != nil || !found {
			t.Fatalf("the store holds no open pause after legion project pause: %v", err)
		}
		if open.By.Kind != store.PauseByOperator || open.By.Session != "" || open.Reason != reason {
			t.Fatalf("the pause is %+v, want the operator's for %q", open, reason)
		}
		pauseLine := "pause " + strconv.FormatInt(open.ID, 10) + " since 2026-11-16T09:00:00Z by the operator: " + reason
		if paused != "project paused: "+pauseLine+"\n" {
			t.Fatalf("legion project pause printed %q, want %q", paused, "project paused: "+pauseLine+"\n")
		}
		if code, _, errb := d.legion(append([]string{"project", "pause", "--reason", "again"}, reach...)...); code != 1 || !strings.Contains(errb, "409") || !strings.Contains(errb, "already paused") {
			t.Fatalf("a second legion project pause = %d, stderr %q; want 1 and the daemon's 409 naming the pause that stands", code, errb)
		}

		state := d.succeed("state")
		lines := stateLines(state, "paused: ")
		want := "paused: " + pauseLine + "; waits on 1 role — LEGION-208 implementer"
		if len(lines) != 1 || lines[0] != want {
			t.Fatalf("legion state while paused printed %q among\n%s\nwant the one line %q", lines, state, want)
		}
		if last := stateLines(state, "last pause: "); len(last) != 0 {
			t.Fatalf("legion state while paused printed %q, want no last pause", last)
		}

		d.clock.advance(20 * time.Minute)
		resumed := d.succeed(append([]string{"project", "resume"}, reach...)...)
		endedLine := pauseLine + "; ended 2026-11-16T09:20:00Z by the operator"
		if want := "project resumed: " + endedLine + "; 0 held rows released\n"; resumed != want {
			t.Fatalf("legion project resume printed %q, want %q", resumed, want)
		}
		last, found, err := d.svc.LastEndedPause(context.Background())
		if err != nil || !found || last.ID != open.ID || last.EndedBy.Kind != store.PauseByOperator {
			t.Fatalf("the last pause after legion project resume = %+v (found %t, %v), want pause %d ended by the operator", last, found, err, open.ID)
		}
		if code, _, errb := d.legion(append([]string{"project", "resume"}, reach...)...); code != 1 || !strings.Contains(errb, "409") || !strings.Contains(errb, "not paused") {
			t.Fatalf("legion project resume of a running project = %d, stderr %q; want 1 and the daemon's 409", code, errb)
		}

		state = d.succeed("state")
		lines = stateLines(state, "last pause: ")
		if len(lines) != 1 || lines[0] != "last pause: "+endedLine {
			t.Fatalf("legion state after the resume printed %q among\n%s\nwant the one line %q", lines, state, "last pause: "+endedLine)
		}
		if still := stateLines(state, "paused: "); len(still) != 0 {
			t.Fatalf("legion state after the resume printed %q, want no paused line", still)
		}

		// --json prints the daemon's answer as it served it.
		var again api.ProjectPauseResponse
		if err := decodeAnswer([]byte(d.succeed(append([]string{"project", "pause", "--reason", "a second outage", "--json"}, reach...)...)), &again); err != nil ||
			again.Pause.ID == open.ID || again.Pause.By.Kind != string(store.PauseByOperator) {
			t.Fatalf("legion project pause --json answered %+v (%v), want a new pause by the operator", again, err)
		}
		var ended api.ProjectResumeResponse
		if err := decodeAnswer([]byte(d.succeed(append([]string{"project", "resume", "--json"}, reach...)...)), &ended); err != nil ||
			ended.Pause.ID != again.Pause.ID || ended.Pause.EndedBy == nil || ended.Pause.EndedBy.Kind != string(store.PauseByOperator) {
			t.Fatalf("legion project resume --json answered %+v (%v), want pause %d ended by the operator", ended, err, again.Pause.ID)
		}
		if n := d.pauses(); n != 2 {
			t.Fatalf("the store holds %d pauses, want the two the commands made", n)
		}
	})

	// dispatch://INFRA-3583 R13: while a project release runs, `legion project pause` prints the
	// daemon's 409 naming the release and exits 1, and nothing is paused; once the release has
	// finished, the pause is taken.
	t.Run("a pause while a project release runs is refused", func(t *testing.T) {
		d := newPauseCommandDaemon(t)
		ctx := context.Background()
		reach := []string{"--operator-token-file", d.tokenFile}
		started := d.clock.now().Add(-time.Minute)
		release, _, _, err := d.store.StartProjectRelease(ctx, "legion", func() time.Time { return started }, 30*time.Minute)
		if err != nil {
			t.Fatalf("record the release: %v", err)
		}
		code, _, errb := d.legion(append([]string{"project", "pause", "--reason", "an outage"}, reach...)...)
		if code != 1 || !strings.Contains(errb, "409") || !strings.Contains(errb, "project release "+strconv.FormatInt(release.ID, 10)+" runs") ||
			!strings.Contains(errb, "nothing was paused") {
			t.Fatalf("legion project pause while release %d runs = %d, stderr %q; want 1 and the daemon's 409 naming the release", release.ID, code, errb)
		}
		if n := d.pauses(); n != 0 {
			t.Fatalf("the refused pause left %d pauses, want none", n)
		}
		if err := d.store.FinishProjectRelease(ctx, release.ID, []byte(`{}`), d.clock.now()); err != nil {
			t.Fatalf("finish the release: %v", err)
		}
		if paused := d.succeed(append([]string{"project", "pause", "--reason", "an outage"}, reach...)...); !strings.HasPrefix(paused, "project paused: ") {
			t.Fatalf("legion project pause once the release finished printed %q, want the pause", paused)
		}
	})

	// `legion config check` names the gap under each setting that turns the pause off, and says
	// nothing of the pause for a file that can hold one; the exit stays 0 either way.
	t.Run("config check names the setting that turns the pause off", func(t *testing.T) {
		legionState(t)
		tmux, _ := workflowConfig(t, 13370, "")
		for _, tc := range []struct {
			name, config, want string
		}{
			{"runtime: tmux", tmux, tmuxPauseUnavailable},
			{"idle_minutes: 0", kubernetesCheckConfig(t, "    idle_release: {idle_minutes: 0}\n"),
				"pause unavailable: runtime.kubernetes.idle_release.idle_minutes: 0\n"},
			{"worker_wakes_per_minute: 0", kubernetesCheckConfig(t, "worker_wakes_per_minute: 0\n"), "pause unavailable: worker_wakes_per_minute: 0\n"},
			{"a file that can hold a pause", kubernetesCheckConfig(t, ""), ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var out, errb bytes.Buffer
				code := run(context.Background(), []string{"legion", "config", "check", "--config", tc.config}, &out, &errb)
				if code != 0 || errb.Len() != 0 {
					t.Fatalf("legion config check = %d, stderr %q; want 0", code, errb.String())
				}
				if named := strings.Count(out.String(), "pause unavailable: "); tc.want == "" && named != 0 {
					t.Fatalf("stdout %q names a pause gap for a file that can hold a pause", out.String())
				} else if tc.want != "" && (named != 1 || !strings.Contains(out.String(), tc.want)) {
					t.Fatalf("stdout %q names the pause gap %d times, want the one line %q", out.String(), named, tc.want)
				}
			})
		}
	})
}

// scriptedRelease is the daemon's project release as `legion project release` reaches it: the
// start answers the first of views, and each read after it the next one, the last for ever, so a
// test scripts a release that runs and then finishes. A dry run answers dry, unless ran says a
// release already ran in this process, which every start then answers, as the daemon's does. The
// real release is internal/daemon's, whose own tests run it; this is what the command prints and
// how it exits.
type scriptedRelease struct {
	mu    sync.Mutex
	noPod bool
	// paused is the refusal every start answers while a pause holds the project.
	paused error
	ran    bool
	views  []api.ProjectReleaseResponse
	dry    api.ProjectReleaseResponse
	reads  int
	starts []api.ProjectReleaseRequest
	// dryTakes is how long a dry run's start takes before it answers, and readTakes how long a
	// read of the release takes.
	dryTakes  time.Duration
	readTakes time.Duration
}

func (r *scriptedRelease) Start(ctx context.Context, bound time.Duration, dryRun bool) (api.ProjectReleaseResponse, bool, error) {
	if dryRun && r.dryTakes > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(r.dryTakes):
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	request := api.ProjectReleaseRequest{DryRun: dryRun}
	if bound > 0 {
		request.Bound = bound.String()
	}
	r.starts = append(r.starts, request)
	switch {
	case r.noPod:
		return api.ProjectReleaseResponse{}, false, api.ErrProjectReleaseRunsNoPod
	case r.paused != nil:
		return api.ProjectReleaseResponse{}, false, r.paused
	case r.ran:
		return r.views[0], false, nil
	case dryRun:
		return r.dry, false, nil
	}
	return r.views[0], true, nil
}

func (r *scriptedRelease) Read(ctx context.Context) (api.ProjectReleaseResponse, bool, error) {
	if r.readTakes > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(r.readTakes):
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	return r.views[min(r.reads, len(r.views)-1)], true, nil
}

func (*scriptedRelease) AskRound() string { return "" }

func (*scriptedRelease) Answer(context.Context, legionclaim.Token, string, uint64, string) error {
	return errors.New("the scripted release takes no answer")
}

// releaseDaemon is the operator surface with release as its project release, and the release's
// poll shortened so a release that finishes is read without waiting five seconds a read.
func releaseDaemon(t *testing.T, release *scriptedRelease) *operatorDaemon {
	t.Helper()
	poll := projectReleasePoll
	projectReleasePoll = 20 * time.Millisecond
	t.Cleanup(func() { projectReleasePoll = poll })
	return newOperatorDaemon(t, func(o *api.Options) { o.ProjectRelease = release })
}

// projectRelease runs `legion project release <args>` and holds it to the rule every operator
// command keeps: the operator token is in neither stream.
func (d *operatorDaemon) projectRelease(args ...string) (int, string, string) {
	d.t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{"legion", "project", "release"}, args...), &out, &errb)
	for name, stream := range map[string]string{"stdout": out.String(), "stderr": errb.String()} {
		if strings.Contains(stream, claimsOperatorToken) {
			d.t.Fatalf("legion project release %v printed the operator token on %s: %q", args, name, stream)
		}
	}
	return code, out.String(), errb.String()
}

// releaseView is a release record started now with bound, unfinished, with sandboxes and claims
// as its lines.
func releaseView(bound time.Duration, sandboxes []api.ReleasedSandbox, claims []api.ReleasedClaim) api.ProjectReleaseResponse {
	started := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	return api.ProjectReleaseResponse{ID: 7, StartedAt: started, BoundAt: started.Add(bound), Bound: bound.String(),
		Result: api.ProjectReleaseResult{Sandboxes: sandboxes, Claims: claims}.Listed()}
}

// finishedView is view finished, with its lines as they ended.
func finishedView(view api.ProjectReleaseResponse, sandboxes []api.ReleasedSandbox, claims []api.ReleasedClaim) api.ProjectReleaseResponse {
	finished := view.StartedAt.Add(10 * time.Minute)
	view.FinishedAt = &finished
	view.Result = api.ProjectReleaseResult{Sandboxes: sandboxes, Claims: claims}.Listed()
	return view
}

var (
	releasedCleanPod  = api.ReleasedSandbox{Name: "legion-legion-legion-208", Kind: "issue", Issue: "LEGION-208", Outcome: api.ReleaseDeletedClean}
	releasedCache     = api.ReleasedSandbox{Name: "legion-legion-cache-acme-widgets", Kind: "cache", Outcome: api.ReleaseDeleted}
	releasedArchitect = api.ReleasedClaim{Token: "legion-legion-legion-208-architect", Issue: "LEGION-208", Role: legionclaim.RoleArchitect,
		Session: "ses_architect", Ended: "project_release", BootStarts: true}
	releasedImplementer = api.ReleasedClaim{Token: "legion-legion-legion-208-implementer", Issue: "LEGION-208", Role: legionclaim.RoleImplementer,
		Session: "ses_implementer", Ended: "project_release", BootStarts: true}
)

// finishingRelease is a release that runs for one read and finishes at the next with done's lines.
func finishingRelease(done []api.ReleasedSandbox, claims []api.ReleasedClaim) *scriptedRelease {
	running := releaseView(time.Minute, []api.ReleasedSandbox{{Name: done[0].Name, Kind: done[0].Kind, Issue: done[0].Issue, Outcome: api.ReleaseWaiting}},
		[]api.ReleasedClaim{{Token: claims[0].Token, Issue: claims[0].Issue, Role: claims[0].Role, Session: claims[0].Session}})
	return &scriptedRelease{views: []api.ProjectReleaseResponse{running, running, finishedView(running, done, claims)}}
}

// dispatch://INFRA-3583 R6: the command refuses to run without --operator-token-file, with the
// usage error's exit 2, and reaches no daemon.
func TestProjectReleaseRefusesToRunWithoutTheOperatorTokenFile(t *testing.T) {
	d := releaseDaemon(t, finishingRelease([]api.ReleasedSandbox{releasedCleanPod}, []api.ReleasedClaim{releasedArchitect}))
	code, _, errb := d.projectRelease("--port", "1")
	if code != 2 || !strings.Contains(errb, "--operator-token-file is required") {
		t.Fatalf("legion project release with no --operator-token-file = %d, stderr %q; want 2 naming the flag", code, errb)
	}
	d.wantNothingReached()
}

// The command finds the daemon as `legion state` finds it (daemonAddress): the address
// LEGION_DAEMON_URL names on a pane, and --port over the configured bind otherwise; and it sends
// the bearer, the bound and no dry run.
func TestProjectReleaseFindsTheDaemonAsLegionStateDoes(t *testing.T) {
	for name, reach := range map[string]func(t *testing.T, d *operatorDaemon) []string{
		"--port": func(_ *testing.T, d *operatorDaemon) []string { return d.reach() },
		"LEGION_DAEMON_URL": func(t *testing.T, d *operatorDaemon) []string {
			t.Setenv("LEGION_DAEMON_URL", d.url)
			return []string{"--operator-token-file", d.tokenFile}
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := releaseDaemon(t, finishingRelease([]api.ReleasedSandbox{releasedCleanPod}, []api.ReleasedClaim{releasedArchitect}))
			code, _, errb := d.projectRelease(append(reach(t, d), "--bound", "1m")...)
			if code != 0 {
				t.Fatalf("legion project release = %d, stderr %q; want 0", code, errb)
			}
			first := d.operatorRequests()[0]
			var body api.ProjectReleaseRequest
			if err := json.Unmarshal(first.body, &body); err != nil {
				t.Fatal(err)
			}
			if first.method != http.MethodPost || first.path != api.ProjectReleaseRoute || first.authorization != "Bearer "+claimsOperatorToken ||
				body != (api.ProjectReleaseRequest{Bound: "1m0s"}) {
				t.Fatalf("the first request was %s %s %+v with %q, want the release's POST with the bound and the bearer", first.method, first.path, body, first.authorization)
			}
		})
	}
}

// One line per Sandbox and one per claim the release stopped, each claim's line with its issue,
// role and session and whether the next boot starts it; exit 0 for a release that left no Sandbox.
func TestProjectReleasePrintsOneLinePerSandboxAndPerStoppedClaim(t *testing.T) {
	d := releaseDaemon(t, finishingRelease([]api.ReleasedSandbox{releasedCleanPod, releasedCache},
		[]api.ReleasedClaim{releasedArchitect, releasedImplementer}))
	code, out, errb := d.projectRelease(d.reach()...)
	if code != 0 || errb != "" {
		t.Fatalf("legion project release = %d, stderr %q; want 0 and nothing said", code, errb)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := []string{
		"sandbox legion-legion-legion-208 issue LEGION-208 deleted_clean",
		"sandbox legion-legion-cache-acme-widgets cache - deleted",
		"claim legion-legion-legion-208-architect LEGION-208 architect ses_architect project_release (the next boot starts it)",
		"claim legion-legion-legion-208-implementer LEGION-208 implementer ses_implementer project_release (the next boot starts it)",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("legion project release printed\n%s\nwant\n%s", out, strings.Join(want, "\n"))
	}
	if gets := len(d.operatorRequests()) - 1; gets < 2 {
		t.Fatalf("the command read the release %d times, want it polled until it finished", gets)
	}
}

// --json prints the release as the daemon served it, one JSON object.
func TestProjectReleasePrintsOneJSONObject(t *testing.T) {
	release := finishingRelease([]api.ReleasedSandbox{releasedCleanPod}, []api.ReleasedClaim{releasedArchitect})
	d := releaseDaemon(t, release)
	code, out, errb := d.projectRelease(append(d.reach(), "--json")...)
	if code != 0 {
		t.Fatalf("legion project release --json = %d, stderr %q; want 0", code, errb)
	}
	decoder := json.NewDecoder(strings.NewReader(out))
	var got api.ProjectReleaseResponse
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("--json printed %q, not one object: %v", out, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("--json printed more than one value: %q", out)
	}
	if got.ID != 7 || got.FinishedAt == nil || len(got.Result.Sandboxes) != 1 || got.Result.Sandboxes[0].Outcome != api.ReleaseDeletedClean {
		t.Fatalf("--json printed %+v, want the finished release", got)
	}
}

// A release that names a pod it left exits 1, and says which.
func TestProjectReleaseExitsOneForALeftPod(t *testing.T) {
	left := api.ReleasedSandbox{Name: "legion-legion-legion-209", Kind: "issue", Issue: "LEGION-209", Outcome: api.ReleaseLeft,
		Reason: "an operator suspended its implementer"}
	held := api.ReleasedClaim{Token: "legion-legion-legion-209-implementer", Issue: "LEGION-209", Role: legionclaim.RoleImplementer,
		Session: "ses_held", Ended: "operator"}
	d := releaseDaemon(t, finishingRelease([]api.ReleasedSandbox{left, releasedCleanPod}, []api.ReleasedClaim{held, releasedArchitect}))
	code, out, errb := d.projectRelease(d.reach()...)
	if code != 1 {
		t.Fatalf("legion project release with a left pod = %d, want 1; stdout %q", code, out)
	}
	if !strings.Contains(out, "sandbox legion-legion-legion-209 issue LEGION-209 left: left because an operator suspended its implementer") ||
		!strings.Contains(out, "claim legion-legion-legion-209-implementer LEGION-209 implementer ses_held operator (the next boot does not start it)") {
		t.Fatalf("legion project release printed %q, want the left pod's line and its held claim's", out)
	}
	if !strings.Contains(errb, "left 1 Sandbox(es) of the project: legion-legion-legion-209") {
		t.Fatalf("stderr %q does not name the left pod", errb)
	}
}

// A release that deleted a pod with work lost exits 0, and its line names that workspace with its
// count of unpushed commits, whether its working copy held a change, and the asked role's answer.
func TestProjectReleaseExitsZeroWithWorkLostAndNamesTheWorkspace(t *testing.T) {
	commits, changed := 2, true
	answered := time.Date(2026, 11, 15, 9, 4, 0, 0, time.UTC)
	lost := api.ReleasedSandbox{Name: "legion-legion-legion-209", Kind: "issue", Issue: "LEGION-209", Outcome: api.ReleaseDeletedLost,
		Commits: &commits, WorkingCopyChanged: &changed,
		Asked: []api.ReleasedAsk{{Role: legionclaim.RoleImplementer, AnsweredAt: &answered}, {Role: legionclaim.RoleTester, Bound: true}}}
	d := releaseDaemon(t, finishingRelease([]api.ReleasedSandbox{lost}, []api.ReleasedClaim{releasedArchitect}))
	code, out, errb := d.projectRelease(d.reach()...)
	if code != 0 {
		t.Fatalf("legion project release with work lost = %d, stderr %q; want 0", code, errb)
	}
	want := "sandbox legion-legion-legion-209 issue LEGION-209 deleted_lost: the workspace of LEGION-209 held 2 unpushed commit(s) and its working copy a change at the last read; " +
		"asked implementer answered at 2026-11-15T09:04:00Z, tester did not answer by the bound"
	if !strings.Contains(out, want+"\n") {
		t.Fatalf("legion project release printed %q, want the line %q", out, want)
	}
}

// A pod deleted after no role process ran at its last read keeps the count an earlier read took,
// and its line says the count is that earlier read's and gives the reason as it is, not as why a
// pod was left.
func TestProjectReleaseNamesTheReadAPodsCountIsFrom(t *testing.T) {
	commits, unchanged := 3, false
	kept := api.ReleasedSandbox{Name: "legion-legion-legion-209", Kind: "issue", Issue: "LEGION-209", Outcome: api.ReleaseDeletedReadRefused,
		Commits: &commits, WorkingCopyChanged: &unchanged, Reason: "no role process ran at the last read",
		Asked: []api.ReleasedAsk{{Role: legionclaim.RoleImplementer, Bound: true}}}
	d := releaseDaemon(t, finishingRelease([]api.ReleasedSandbox{kept}, []api.ReleasedClaim{releasedArchitect}))
	code, out, errb := d.projectRelease(d.reach()...)
	if code != 0 {
		t.Fatalf("legion project release = %d, stderr %q; want 0", code, errb)
	}
	want := "sandbox legion-legion-legion-209 issue LEGION-209 deleted_read_refused: the workspace of LEGION-209 held 3 unpushed commit(s) and its working copy no change at an earlier read; " +
		"no role process ran at the last read; asked implementer did not answer by the bound"
	if !strings.Contains(out, want+"\n") {
		t.Fatalf("legion project release printed %q, want the line %q", out, want)
	}
}

// When --wait ends before the release finishes, the command prints the release as it stands,
// exits 1 and says the release goes on.
func TestProjectReleaseExitsOneWhenTheWaitEndsFirst(t *testing.T) {
	running := releaseView(time.Hour, []api.ReleasedSandbox{{Name: releasedCleanPod.Name, Kind: "issue", Issue: "LEGION-208", Outcome: api.ReleaseWaiting}},
		[]api.ReleasedClaim{{Token: releasedArchitect.Token, Issue: "LEGION-208", Role: legionclaim.RoleArchitect, Session: "ses_architect"}})
	d := releaseDaemon(t, &scriptedRelease{views: []api.ProjectReleaseResponse{running}})
	began := time.Now()
	code, out, errb := d.projectRelease(append(d.reach(), "--wait", "300ms")...)
	if code != 1 || !strings.Contains(errb, "the release goes on in the daemon") || !strings.Contains(errb, "the command's wait of 300ms ended") {
		t.Fatalf("legion project release --wait 300ms = %d, stderr %q; want 1 naming the wait of 300ms and saying the release goes on", code, errb)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("the command took %s, want it ended by its --wait", took)
	}
	if !strings.Contains(out, "sandbox legion-legion-legion-208 issue LEGION-208 waiting") {
		t.Fatalf("legion project release printed %q, want the release as it stands", out)
	}
}

// A read of the release that --wait cuts leaves the release the daemon already answered as it
// stands: the command prints that release and exits 1 saying the release goes on, as it does when
// the wait ends between two reads. Each read here takes longer than the whole wait, so the wait
// ends inside the first read on every run; TestProjectReleaseExitsOneWhenTheWaitEndsFirst meets
// the same cut only when a read happens to start just before its wait ends.
func TestProjectReleasePrintsTheReleaseItHoldsWhenTheWaitCutsARead(t *testing.T) {
	running := releaseView(time.Hour, []api.ReleasedSandbox{{Name: releasedCleanPod.Name, Kind: "issue", Issue: "LEGION-208", Outcome: api.ReleaseWaiting}},
		[]api.ReleasedClaim{{Token: releasedArchitect.Token, Issue: "LEGION-208", Role: legionclaim.RoleArchitect, Session: "ses_architect"}})
	d := releaseDaemon(t, &scriptedRelease{views: []api.ProjectReleaseResponse{running}, readTakes: 3 * time.Second})
	code, out, errb := d.projectRelease(append(d.reach(), "--wait", "300ms")...)
	if code != 1 || !strings.Contains(errb, releaseGoesOn) {
		t.Fatalf("legion project release --wait 300ms whose read outlasts the wait = %d, stderr %q; want 1 saying %q", code, errb, releaseGoesOn)
	}
	if !strings.Contains(out, "sandbox legion-legion-legion-208 issue LEGION-208 waiting") {
		t.Fatalf("legion project release printed %q (stderr %q), want the release the daemon answered at its start, as it stands", out, errb)
	}
}

// A dry run whose request is not answered within --wait exits 1 saying that a dry run records and
// starts nothing, not that a release goes on in the daemon: nothing was started.
func TestProjectReleaseDryRunTheWaitEndsSaysNothingStarted(t *testing.T) {
	d := releaseDaemon(t, &scriptedRelease{dryTakes: 3 * time.Second})
	code, _, errb := d.projectRelease(append(d.reach(), "--dry-run", "--wait", "300ms")...)
	if code != 1 || !strings.Contains(errb, "a dry run records and starts nothing") || strings.Contains(errb, "goes on in the daemon") {
		t.Fatalf("legion project release --dry-run --wait 300ms = %d, stderr %q; want 1 saying the dry run started nothing", code, errb)
	}
}

// --dry-run sends the dry run's request and exits 0 on the daemon's answer, printing what a
// release would do now: the roles it would ask, the pod of another image, and the claims it would
// stop.
func TestProjectReleaseDryRunExitsZeroWhenTheDaemonAnswers(t *testing.T) {
	commits, unchanged := 1, false
	finished := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	release := &scriptedRelease{dry: api.ProjectReleaseResponse{DryRun: true, StartedAt: finished, BoundAt: finished.Add(30 * time.Minute),
		Bound: "30m0s", FinishedAt: &finished, Result: api.ProjectReleaseResult{
			Sandboxes: []api.ReleasedSandbox{{Name: "legion-legion-legion-209", Kind: "issue", Issue: "LEGION-209", Outcome: api.ReleaseDeletedLost,
				Commits: &commits, WorkingCopyChanged: &unchanged, OtherImage: true, Asked: []api.ReleasedAsk{{Role: legionclaim.RoleImplementer}}},
				{Name: "legion-legion-legion-210", Kind: "issue", Issue: "LEGION-210", Outcome: api.ReleaseLeft, Reason: "an operator suspended its tester"}},
			Claims: []api.ReleasedClaim{{Token: "legion-legion-legion-209-implementer", Issue: "LEGION-209", Role: legionclaim.RoleImplementer,
				Session: "ses_implementer", Ended: "project_release", BootStarts: true}},
		}}}
	d := releaseDaemon(t, release)
	code, out, errb := d.projectRelease(append(d.reach(), "--dry-run")...)
	if code != 0 {
		t.Fatalf("legion project release --dry-run = %d, stderr %q; want 0", code, errb)
	}
	if len(release.starts) != 1 || !release.starts[0].DryRun || len(d.operatorRequests()) != 1 {
		t.Fatalf("the dry run sent %+v in %d requests, want one dry-run request", release.starts, len(d.operatorRequests()))
	}
	for _, want := range []string{
		"sandbox legion-legion-legion-209 issue LEGION-209 deleted_lost: the workspace of LEGION-209 held 1 unpushed commit(s) and its working copy no change at the last read; its pod runs an image other than the daemon's own; a release would ask implementer",
		"sandbox legion-legion-legion-210 issue LEGION-210 left: left because an operator suspended its tester",
		"claim legion-legion-legion-209-implementer LEGION-209 implementer ses_implementer project_release (the next boot would start it)",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("the dry run printed %q, want the line %q", out, want)
		}
	}
}

// A daemon that refuses the release — one on the tmux runtime — makes the command exit 1 with the
// daemon's sentence.
func TestProjectReleaseExitsOneWhenTheDaemonRefuses(t *testing.T) {
	d := releaseDaemon(t, &scriptedRelease{noPod: true})
	code, _, errb := d.projectRelease(d.reach()...)
	if code != 1 || !strings.Contains(errb, "409") || !strings.Contains(errb, api.ProjectReleaseRunsNoPod) {
		t.Fatalf("legion project release on tmux = %d, stderr %q; want 1 with the daemon's 409 and its sentence", code, errb)
	}
}

// dispatch://INFRA-3583 R13: a daemon whose release a pause refuses — the project paused, or a pod
// a pause stopped still held — makes the command exit 1, its dry run too, printing the daemon's 409
// with the held pod, its claim to resume and the resume that ends the hold, and reading no
// release after it.
func TestProjectReleaseExitsOneWhenAPauseHoldsTheProject(t *testing.T) {
	refused := &store.ProjectPausedError{Project: "legion", Held: []store.HeldPausePod{{Issue: "LEGION-208", Pause: 7,
		HeldAt: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC), Claims: []legionclaim.Token{"legion-legion-legion-208-implementer"}}}}
	release := &scriptedRelease{paused: refused}
	d := releaseDaemon(t, release)
	for _, flags := range [][]string{{"--dry-run"}, nil} {
		code, out, errb := d.projectRelease(append(d.reach(), flags...)...)
		if code != 1 || out != "" || !strings.Contains(errb, "409") || !strings.Contains(errb, "project release refused: a pause still holds 1 pod(s)") ||
			!strings.Contains(errb, "LEGION-208 (resume one of legion-legion-legion-208-implementer)") || !strings.Contains(errb, "legion claims resume --claim <token>") {
			t.Fatalf("legion project release %v while a pause holds a pod = %d, stdout %q, stderr %q; want 1 with the daemon's 409 naming the held pod", flags, code, out, errb)
		}
	}
	release.mu.Lock()
	defer release.mu.Unlock()
	if release.reads != 0 {
		t.Fatalf("the refused command read the release %d times, want none", release.reads)
	}
}

// A daemon that already ran a release in this process answers --dry-run with that release
// (projectReleaser.Start), and the command reads and prints it as the release it is: it polls
// the release while it runs, prints none of a dry run's words, says on stderr that the dry run
// was answered with that release, and exits 1 for a pod the release left.
func TestProjectReleaseDryRunAnsweredWithAReleaseReadsThatRelease(t *testing.T) {
	left := api.ReleasedSandbox{Name: "legion-legion-legion-209", Kind: "issue", Issue: "LEGION-209", Outcome: api.ReleaseLeft,
		Reason: "an operator suspended its implementer"}
	release := finishingRelease([]api.ReleasedSandbox{left, releasedCleanPod}, []api.ReleasedClaim{releasedArchitect})
	release.ran = true
	d := releaseDaemon(t, release)
	code, out, errb := d.projectRelease(append(d.reach(), "--dry-run")...)
	if code != 1 {
		t.Fatalf("legion project release --dry-run answered with a release that left a pod = %d, want 1; stdout %q, stderr %q", code, out, errb)
	}
	release.mu.Lock()
	starts, reads := slices.Clone(release.starts), release.reads
	release.mu.Unlock()
	if len(starts) != 1 || !starts[0].DryRun {
		t.Fatalf("the command sent %+v, want one dry run's request", starts)
	}
	if reads < 2 {
		t.Fatalf("the command read the release %d times, want it read until the release finished", reads)
	}
	if strings.Contains(out, "would") {
		t.Fatalf("the command printed %q, a dry run's words for a release that ran", out)
	}
	if !strings.Contains(out, "claim legion-legion-legion-208-architect LEGION-208 architect ses_architect project_release (the next boot starts it)\n") {
		t.Fatalf("the command printed %q, want the release's claim line", out)
	}
	if !strings.Contains(errb, "answered the dry run with that release") || !strings.Contains(errb, "left 1 Sandbox(es) of the project: legion-legion-legion-209") {
		t.Fatalf("stderr %q does not say the dry run was answered with the release, or does not name the left pod", errb)
	}
}

// Once the daemon has answered --dry-run with the release it already ran, the command follows
// that release, so a read of it the command's wait cuts says the release goes on in the daemon
// and to run the command again — not that a dry run records and starts nothing, which is true of
// no run that reads a release.
func TestProjectReleaseDryRunAnsweredWithAReleaseSaysThatReleaseGoesOn(t *testing.T) {
	running := releaseView(time.Hour, []api.ReleasedSandbox{{Name: releasedCleanPod.Name, Kind: "issue", Issue: "LEGION-208", Outcome: api.ReleaseWaiting}},
		[]api.ReleasedClaim{{Token: releasedArchitect.Token, Issue: "LEGION-208", Role: legionclaim.RoleArchitect, Session: "ses_architect"}})
	d := releaseDaemon(t, &scriptedRelease{ran: true, views: []api.ProjectReleaseResponse{running}, readTakes: 3 * time.Second})
	code, _, errb := d.projectRelease(append(d.reach(), "--dry-run", "--wait", "300ms")...)
	if code != 1 || !strings.Contains(errb, releaseGoesOn) || strings.Contains(errb, "a dry run records and starts nothing") {
		t.Fatalf("legion project release --dry-run --wait 300ms answered with a release = %d, stderr %q; want 1 saying %q", code, errb, releaseGoesOn)
	}
}

// A released daemon refuses an operator's spawn, retry, resume and deliver 409 with the sentence
// that names the release, and each `legion claims` command exits 1 printing it. The routes' own
// case is internal/api's TestProjectReleaseRefusesEveryStartAndPromptOfARole.
func TestProjectReleaseRefusesTheOperatorsStartsThroughTheCommands(t *testing.T) {
	released := &atomic.Bool{}
	d := newOperatorDaemon(t, func(o *api.Options) { o.ProjectReleaseHolds = released.Load })
	d.succeed(d.spawnArgs()...)
	d.ready(architectClaim)
	released.Store(true)
	claim := "--claim=" + string(architectClaim)
	for name, args := range map[string][]string{
		"spawn":   append(d.spawnArgs()[1:], "--wait", "5s"),
		"retry":   append(d.reach(), claim),
		"resume":  append(d.reach(), claim),
		"deliver": append(d.reach(), claim, "--task", "Carry on."),
	} {
		t.Run(name, func(t *testing.T) {
			code, _, errb := d.claims(append([]string{name}, args...)...)
			if code != 1 || !strings.Contains(errb, "409") || !strings.Contains(errb, name+" refused: "+api.ProjectReleaseHeldSentence) {
				t.Fatalf("legion claims %s while released = %d, stderr %q; want 1 with the release's 409", name, code, errb)
			}
		})
	}
}
