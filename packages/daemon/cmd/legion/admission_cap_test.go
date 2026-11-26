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

// capCommandStart and capCommandCeiling are the starting cap (admission_cap) and the ceiling
// (admission_ceiling) of the cap commands' test daemon.
const (
	capCommandStart   = 4
	capCommandCeiling = 6
)

// capCommandAt is the clock of the test daemon's admission, the time every change is stamped at.
var capCommandAt = time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)

// capState is the state route's source for the cap commands' test daemon: the cap keys as the
// daemon's own source.State fills them (api.FillAdmissionCapTx), under the rest of a state that
// holds nothing.
type capState struct{}

func (capState) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	state := api.State{
		Daemon:    api.DaemonInfo{Project: "LEGION", SchemaVersion: 1, Boots: 1},
		Admission: api.Admission{Active: []string{}, Waiting: []string{}},
	}
	err := api.FillAdmissionCapTx(ctx, tx, "LEGION", capCommandStart, capCommandCeiling, &state.Admission)
	return state, nil, err
}

// newCapCommandDaemon is a daemon of project LEGION as `legion project set-admission-cap`,
// `clear-admission-cap` and `legion state` reach it: the real api.NewServer over the daemon's
// handler chain (the workflow engine and admission) and a database of the test's own, on a
// loopback port, with the operator token pauseOperatorToken.
func newCapCommandDaemon(t *testing.T) *pauseCommandDaemon {
	t.Helper()
	st := testpg.CloneStore(t, "legion_cmd_cap")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION"}, quiet)
	admission := admit.New(records, engine, capCommandStart, "LEGION", quiet)
	admission.UseCeiling(capCommandCeiling)
	admission.UseClock(func() time.Time { return capCommandAt })
	server := httptest.NewServer(api.NewServer("127.0.0.1", 0, api.Options{
		State: capState{}, StateTransactions: st.Pool(), Pool: st.Pool(), Record: records,
		Handlers:     []intake.Handler{engine, admission},
		AdmissionCap: capCommandStart, AdmissionCeiling: capCommandCeiling, Now: func() time.Time { return capCommandAt },
		Project: "legion", DispatchProject: "LEGION", OperatorToken: pauseOperatorToken, Log: quiet,
	}).Handler)
	t.Cleanup(server.Close)
	return &pauseCommandDaemon{t: t, store: st, port: server.Listener.Addr().(*net.TCPAddr).Port,
		tokenFile: writeFile(t, "operator-token", pauseOperatorToken+"\n")}
}

// capChanges is how many changes of the cap the store holds.
func capChanges(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), `select count(*) from project_admission_cap_changes`).Scan(&n); err != nil {
		t.Fatalf("count the cap changes: %v", err)
	}
	return n
}

// TestAdmissionCapCommands is the CLI half of dispatch://INFRA-5616: `legion project
// set-admission-cap --cap <n> --reason <text>` sets the cap with the operator's token, here above
// the starting cap of 4 and under the ceiling of 6, and prints the cap in force, the ceiling and the
// cap set with its reason quoted; `legion state` prints the same line; a cap above the ceiling is
// refused with the daemon's sentence naming it; and `clear-admission-cap` returns the daemon to the
// starting cap. A missing flag is a usage error that reaches nothing.
func TestAdmissionCapCommands(t *testing.T) {
	d := newCapCommandDaemon(t)
	reach := []string{"--operator-token-file", d.tokenFile}

	if code, _, errb := d.legion(append([]string{"project", "set-admission-cap", "--reason", "a pool is down"}, reach...)...); code != 2 ||
		!strings.Contains(errb, "--cap is required") {
		t.Fatalf("set-admission-cap with no --cap = %d, stderr %q; want 2 naming --cap", code, errb)
	}
	if code, _, errb := d.legion(append([]string{"project", "set-admission-cap", "--cap", "3"}, reach...)...); code != 2 ||
		!strings.Contains(errb, "--reason is required") {
		t.Fatalf("set-admission-cap with no --reason = %d, stderr %q; want 2 naming --reason", code, errb)
	}
	if code, _, errb := d.legion(append([]string{"project", "set-admission-cap", "--cap", "7", "--reason", "more"}, reach...)...); code != 1 ||
		!strings.Contains(errb, "409") || !strings.Contains(errb, "ceiling of 6") {
		t.Fatalf("set-admission-cap above the ceiling = %d, stderr %q; want 1 and the daemon's 409 naming the ceiling of 6", code, errb)
	}
	if n := capChanges(t, d.store); n != 0 {
		t.Fatalf("the refused commands recorded %d changes, want none", n)
	}

	const setLine = `admission cap: 5 in force, ceiling 6; set to 5 by the operator at 2026-11-24T06:00:00Z: "a pool is down\nand a second line"` + "\n"
	if out := d.succeed(append([]string{"project", "set-admission-cap", "--cap", "5", "--reason", "a pool is down\nand a second line"}, reach...)...); out != setLine {
		t.Fatalf("set-admission-cap printed %q, want %q", out, setLine)
	}
	if lines := stateLines(d.succeed("state"), "admission cap: "); len(lines) != 1 || lines[0]+"\n" != setLine {
		t.Fatalf("legion state printed %q, want the one line %q", lines, setLine)
	}

	const clearLine = "admission cap: 4 in force, ceiling 6; no cap set while the daemon runs\n"
	if out := d.succeed(append([]string{"project", "clear-admission-cap", "--reason", "the pool is back"}, reach...)...); out != clearLine {
		t.Fatalf("clear-admission-cap printed %q, want %q", out, clearLine)
	}
	if n := capChanges(t, d.store); n != 2 {
		t.Fatalf("the store holds %d changes after a set and a clear, want 2", n)
	}
}
