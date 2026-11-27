package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

// The pause lines of `legion state` and of the pause commands' answers show a pause's reason and
// the controller's session as terminalText: the controller or the operator wrote them, so an ESC
// or a BEL in them can neither set the window title nor ring.
func TestThePauseLinesShowAReasonAsTerminalText(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	ended := since.Add(20 * time.Minute)
	const reason = "\x1b]0;x\x07"
	by := api.PauseActorView{Kind: "controller", Session: "ses-\x07"}
	const words = `pause 7 since 2026-11-16T09:00:00Z by the controller (session ses-\a): \x1b]0;x\a`
	for _, tc := range []struct {
		name, got, want string
	}{
		{name: "an open pause", got: pausedSummary(&api.PausedView{ID: 7, Since: since, By: by, Reason: reason}),
			want: "paused: " + words + "; waits on no role"},
		{name: "the last pause", got: lastPauseSummary(&api.PauseView{ID: 7, Since: since, By: by, Reason: reason,
			EndedAt: &ended, EndedBy: &api.PauseActorView{Kind: "operator"}}),
			want: "last pause: " + words + "; ended 2026-11-16T09:20:00Z by the operator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("the line = %q, want %q", tc.got, tc.want)
			}
			if strings.ContainsAny(tc.got, "\x1b\x07") {
				t.Fatalf("the line = %q, which holds a raw ESC or BEL", tc.got)
			}
		})
	}
}
