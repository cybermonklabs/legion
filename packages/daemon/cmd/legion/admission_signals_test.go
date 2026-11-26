package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// signalsLineState is a state of project ACME with a slots reading and admission by signals as
// the daemon serves them, each series of another shape: a value under its limit, one the daemon
// cannot compute, one with no limit, one holding since a pass dated it, and one holding with no pass
// that dated it.
type signalsLineState struct{ reading store.SignalsReading }

func (s signalsLineState) State(context.Context, pgx.Tx) (api.State, []supervise.Claim, error) {
	return api.State{
		Daemon: api.DaemonInfo{Project: "ACME", SchemaVersion: 1, Boots: 1},
		Admission: api.Admission{Cap: 2, Active: []string{}, Waiting: []string{},
			Slots: &record.SlotsReading{InUse: 0, Cap: 2, Line: "slots: 0 of 2 in use"}, Signals: &s.reading},
	}, nil, nil
}

// TestStateSignalLine: `legion state` prints one line for admission by signals right after the
// slots line, with each series' value, limit and hold as `legion state --json` gives them (S2):
// a series the daemon cannot compute reads as not recorded, a hold no pass dated reads as undated,
// and every series of signals.Names() is on it (/series-inventory).
func TestStateSignalLine(t *testing.T) {
	since := time.Date(2026, 11, 26, 7, 0, 0, 0, time.UTC)
	reading := store.SignalsReading{On: true}
	shapes := map[string]store.SignalSeriesReading{
		signals.Idle:      {Value: new(0.25), Limit: new(0.5)},
		signals.Queue:     {Absent: new("the daemon has not begun keeping the merge queue's events")},
		signals.Provider:  {Value: new(0.00001)},
		signals.Refusals:  {Value: new(3.0), Limit: new(2.0), Holds: true, Since: &since},
		signals.Kills:     {Value: new(6.0), Limit: new(5.0), Holds: true},
		signals.Approvals: {Value: new(1.0), Limit: new(1000000.0)},
	}
	for _, series := range signals.Names() {
		row := shapes[series]
		signal, part := store.SignalPart(series)
		row.Signal = signal
		if part != "" {
			row.Part = &part
		}
		reading.Series = append(reading.Series, row)
	}
	st := testpg.CloneStore(t, "legion_cmd_signals")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(api.NewServer("127.0.0.1", 0, api.Options{
		State: signalsLineState{reading: reading}, StateTransactions: st.Pool(),
		Project: "acme", DispatchProject: "ACME", OperatorToken: "operator-bearer-for-signals-tests", Log: quiet,
	}).Handler)
	t.Cleanup(server.Close)
	port := strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port)
	code, text, errb := legionRun(t, "state", "--port", port)
	if code != 0 || errb != "" {
		t.Fatalf("legion state = %d, stderr %q", code, errb)
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	at := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "signals: ") {
			if at >= 0 {
				t.Fatalf("legion state printed two signals lines:\n%s", text)
			}
			at = i
		}
	}
	if at < 1 || lines[at-1] != "slots: 0 of 2 in use" {
		t.Fatalf("legion state printed its signals line at %d, want it right after the slots line:\n%s", at, text)
	}
	// A number is printed as a decimal, never in scientific notation: the largest limit of a count
	// and a headroom far below 1 read as a person wrote them.
	want := "signals: admission by signals on; idle 0.25 limit 0.5 does not hold; queue not recorded limit none does not hold; " +
		"provider/headroom 0.00001 limit none does not hold; provider/refusals 3 limit 2 holds since 2026-11-26T07:00:00Z; " +
		"kills 6 limit 5 holds, undated; approvals 1 limit 1000000 does not hold"
	if lines[at] != want {
		t.Errorf("legion state's signals line =\n%s\nwant\n%s", lines[at], want)
	}
	t.Run("series-inventory", func(t *testing.T) {
		_, raw, _ := legionRun(t, "state", "--json", "--port", port)
		var served struct {
			Admission struct {
				Signals store.SignalsReading `json:"signals"`
			} `json:"admission"`
		}
		if err := json.Unmarshal([]byte(raw), &served); err != nil {
			t.Fatalf("read legion state --json: %v", err)
		}
		for _, series := range signals.Names() {
			signal, part := store.SignalPart(series)
			name := signal
			if part != "" {
				name += "/" + part
			}
			if !strings.Contains(lines[at], "; "+name+" ") {
				t.Errorf("the signals line names no %s: %s", name, lines[at])
			}
			found := false
			for _, row := range served.Admission.Signals.Series {
				found = found || row.Signal == signal && (row.Part == nil) == (part == "")
			}
			if !found {
				t.Errorf("legion state --json serves no %s", name)
			}
		}
	})
}
