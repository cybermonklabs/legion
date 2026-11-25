package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/replay"
)

// `legion replay signals` prints R11's signals of a window at each moment --at names, each limit's
// next crossing --limit gives, and the window's kills matched between the launcher's lines and the
// daemon's. The window, from 09:00 to 12:00, holds a record of the kills alone: the launcher's
// lines of kills in ACME-1's pod at 09:40, 09:50 and 10:35, and the daemon's line of the first.
// At 09:50 two kills count and the limit 2 binds until 09:40's leaves the hour at 10:40; at 10:35
// three count, 2 at 10:40 still binds, and the count falls below it at 10:50. Every other signal
// is unrecorded, and the approvals change only at events. A limit that names no signal, and a
// missing --at, are usage errors.
func TestReplaySignalsPrintsEachSignalAndItsNextCrossing(t *testing.T) {
	at := func(hour, minute int) time.Time { return time.Date(2001, time.March, 5, hour, minute, 0, 0, time.UTC) }
	kill := func(when time.Time, pid int) replay.LauncherLine {
		return replay.LauncherLine{Time: when, Pod: "legion-acme-acme-1",
			Text: fmt.Sprintf("launcher: memory guard: ended pid=%d role=architect size=9000000000 reading=9000000000 counter=1 limit=8589934592", pid)}
	}
	opening := "daemon: opening the store; a connection error that the next attempt can clear is tried again"
	told := "workflow: the role whose process the memory guard ended is told why"
	window := replay.Window{
		Project:           "ACME",
		From:              at(9, 0),
		To:                at(12, 0),
		Caps:              []replay.CapAt{{At: at(9, 0), Cap: 1}},
		Parts:             []replay.Part{{From: at(9, 0), To: at(12, 0), Release: "fixture", Commit: record.MainRule.Commit, Rule: record.MainRule.Name}},
		TickInterval:      replay.Duration(30 * time.Minute),
		GateSteps:         []record.DesignStep{},
		Linger:            replay.Duration(2 * time.Hour),
		ReviewRoundCap:    3,
		StalledTreeNotice: replay.Duration(6 * time.Hour),
		WorkerSilence:     replay.Duration(4 * time.Hour),
		WorkerBackground:  replay.Duration(4 * time.Hour),
		Experiments:       []config.Experiment{},
		Lines:             []string{opening, told},
		Collector:         record.MainRule.Commit,
		Log: []json.RawMessage{
			json.RawMessage(fmt.Sprintf(`{"time":"2001-03-05T09:00:00Z","level":"INFO","msg":%q}`, opening)),
			json.RawMessage(fmt.Sprintf(`{"time":"2001-03-05T09:40:02Z","level":"INFO","msg":%q,"issue":"ACME-1","role":"legion-acme-acme-1-architect",`+
				`"pod":"00000000-0000-4000-8000-0000000000aa","guard":"guard-1","sequence":1,"pid":100,"cause":"","sizeBytes":9000000000,`+
				`"readingBytes":9000000000,"counterBytes":1,"limitBytes":8589934592}`, told)),
		},
		Launcher: []replay.LauncherLine{kill(at(9, 40), 100), kill(at(9, 50), 101), kill(at(10, 35), 102)},
		Signals:  map[string][]replay.Span{replay.SignalKills: {{From: at(9, 0), To: at(12, 0)}}},
	}
	dir := t.TempDir()
	if err := window.Write(dir); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := legionRun(t, "replay", "signals", "--window", dir, "--at", "2001-03-05T09:50:00Z", "--at", "2001-03-05T10:35:00Z",
		"--limit", "kills=2", "--limit", "approvals=1")
	unrecorded := func(kills string) string {
		return "  idle unrecorded\n  queue unrecorded\n  provider unrecorded\n  refusals unrecorded\n" + kills +
			"  approvals unrecorded\n    limit 1: no crossing, it changes only at events\n"
	}
	want := "the signals of ACME, idle after 1h0m0s\n" +
		"at 2001-03-05T09:50:00Z\n" + unrecorded("  kills 2\n    limit 2: crosses at 2001-03-05T10:40:00Z\n") +
		"at 2001-03-05T10:35:00Z\n" + unrecorded("  kills 3\n    limit 2: crosses at 2001-03-05T10:50:00Z\n") +
		"the memory guard's kills: 1 in both the launcher's lines and the daemon's\n" +
		"  only in the launcher's lines: 2001-03-05T09:50:00Z legion-acme-acme-1 architect pid 101\n" +
		"  only in the launcher's lines: 2001-03-05T10:35:00Z legion-acme-acme-1 architect pid 102\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Errorf("legion replay signals = %d, stderr %q, stdout:\n%s\nwant 0 and:\n%s", code, stderr, stdout, want)
	}
	for _, args := range [][]string{
		{"replay", "signals", "--window", dir, "--at", "2001-03-05T09:50:00Z", "--limit", "nothing=1"},
		{"replay", "signals", "--window", dir},
	} {
		if code, stdout, stderr := legionRun(t, args...); code != 2 || stdout != "" || !strings.Contains(stderr, "legion replay signals") {
			t.Errorf("legion %s = %d, stdout %q, stderr %q; want a usage error, 2", strings.Join(args, " "), code, stdout, stderr)
		}
	}
}
