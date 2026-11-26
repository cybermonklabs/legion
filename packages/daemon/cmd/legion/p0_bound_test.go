package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// p0CommandConfigured is legion.yaml's P0 bound of the P0 bound commands' test daemon.
const p0CommandConfigured = 4

// p0BoundState is the state route's source for the P0 bound commands' test daemon: the daemon's own
// read of the bound on the state's snapshot (api.P0BoundStateTx, which the daemon's source.State
// calls), under the rest of a state that holds one P0 holder of two.
type p0BoundState struct{}

func (p0BoundState) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	state := api.State{
		Daemon:    api.DaemonInfo{Project: "LEGION", SchemaVersion: 1, Boots: 1},
		Admission: api.Admission{Cap: 2, P0Holders: new(1), Active: []string{"LEGION-1", "LEGION-2"}, Waiting: []string{}},
	}
	bound, err := api.P0BoundStateTx(ctx, tx, "LEGION", new(p0CommandConfigured))
	state.Admission.P0Bound = &bound
	return state, nil, err
}

// newP0BoundCommandDaemon is a daemon of project LEGION as `legion project set-p0-bound`,
// `clear-p0-bound` and `legion state` reach it: the real api.NewServer over the daemon's handler
// chain (the workflow engine and admission) and a database of the test's own, on a loopback port,
// with the operator token pauseOperatorToken.
func newP0BoundCommandDaemon(t *testing.T) *pauseCommandDaemon {
	t.Helper()
	st := testpg.CloneStore(t, "legion_cmd_p0_bound")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION"}, quiet)
	admission := admit.New(records, engine, 2, "LEGION", quiet)
	admission.UseP0Bound(new(p0CommandConfigured))
	server := httptest.NewServer(api.NewServer("127.0.0.1", 0, api.Options{
		State: p0BoundState{}, StateTransactions: st.Pool(), Pool: st.Pool(), Record: records,
		Handlers: []intake.Handler{engine, admission}, P0BoundConfigured: new(p0CommandConfigured),
		Now:     func() time.Time { return time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC) },
		Project: "legion", DispatchProject: "LEGION", OperatorToken: pauseOperatorToken, Log: quiet,
	}).Handler)
	t.Cleanup(server.Close)
	return &pauseCommandDaemon{t: t, store: st, port: server.Listener.Addr().(*net.TCPAddr).Port,
		tokenFile: writeFile(t, "operator-token", pauseOperatorToken+"\n")}
}

// p0BoundChanges is how many changes of the P0 bound the store holds.
func p0BoundChanges(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), `select count(*) from project_p0_bound_changes`).Scan(&n); err != nil {
		t.Fatalf("count the P0 bound changes: %v", err)
	}
	return n
}

// The operator's half of C3 of dispatch://INFRA-5636: `legion project set-p0-bound` sets the bound
// to a number or to none while the daemon runs and prints the bound in force, legion.yaml's and the
// live one; `legion state` prints the same bound beside the number of P0 holders, the slots in use
// and the cap (C4); `clear-p0-bound` returns to legion.yaml's. A set that names both a number and
// none, or neither, and one with no reason are usage errors that reach nothing.
func TestP0BoundCommands(t *testing.T) {
	t.Parallel()
	d := newP0BoundCommandDaemon(t)
	reach := []string{"--operator-token-file", d.tokenFile}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"--reason", "runaway"}, want: "--bound <n> or --none is required"},
		{args: []string{"--bound", "2", "--none", "--reason", "runaway"}, want: "--bound and --none name two bounds"},
		{args: []string{"--bound", "2"}, want: "--reason is required"},
	} {
		if code, _, errb := d.legion(append(append([]string{"project", "set-p0-bound"}, tc.args...), reach...)...); code != 2 ||
			!strings.Contains(errb, tc.want) {
			t.Fatalf("set-p0-bound %v = %d, stderr %q; want 2 naming %q", tc.args, code, errb, tc.want)
		}
	}
	if code, _, errb := d.legion(append([]string{"project", "set-p0-bound", "--bound", "0", "--reason", "runaway"}, reach...)...); code != 1 ||
		!strings.Contains(errb, "400") || !strings.Contains(errb, "at least 1") {
		t.Fatalf("set-p0-bound --bound 0 = %d, stderr %q; want 1 and the daemon's 400 naming the floor of 1", code, errb)
	}
	if n := p0BoundChanges(t, d.store); n != 0 {
		t.Fatalf("the refused commands recorded %d changes, want none", n)
	}

	const setLine = "P0 bound: 2 in force, 4 in legion.yaml; set to 2 by the operator at 2026-11-24T06:00:00Z: runaway\n"
	if out := d.succeed(append([]string{"project", "set-p0-bound", "--bound", "2", "--reason", "runaway"}, reach...)...); out != setLine {
		t.Fatalf("set-p0-bound printed %q, want %q", out, setLine)
	}
	const stateLine = "P0: 1 of the 2 issues holding a slot are P0 and hold it outside the cap of 2; P0 bound 2 in force, 4 in legion.yaml; " +
		"set to 2 by the operator at 2026-11-24T06:00:00Z: runaway"
	if lines := stateLines(d.succeed("state"), "P0: "); len(lines) != 1 || lines[0] != stateLine {
		t.Fatalf("legion state printed %q, want the one line %q", lines, stateLine)
	}

	const noneLine = "P0 bound: none in force, 4 in legion.yaml; set to none by the operator at 2026-11-24T06:00:00Z: start them all\n"
	if out := d.succeed(append([]string{"project", "set-p0-bound", "--none", "--reason", "start them all"}, reach...)...); out != noneLine {
		t.Fatalf("set-p0-bound --none printed %q, want %q", out, noneLine)
	}

	const clearLine = "P0 bound: 4 in force, 4 in legion.yaml; none set while the daemon runs\n"
	if out := d.succeed(append([]string{"project", "clear-p0-bound", "--reason", "back to the file"}, reach...)...); out != clearLine {
		t.Fatalf("clear-p0-bound printed %q, want %q", out, clearLine)
	}
	if n := p0BoundChanges(t, d.store); n != 3 {
		t.Fatalf("the store holds %d changes after two sets and a clear, want 3", n)
	}
}
