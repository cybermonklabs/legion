package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/replay"
)

// tokensWindowDir is a window written into a directory of the test's own: from 09:00 to 12:00 on
// 2001-03-05 under a cap of 1, with no issue, whose record of the provider covers its whole span
// and holds 4,500 uncached input tokens in each minute and no limit of the project's own in any.
func tokensWindowDir(t *testing.T) string {
	t.Helper()
	at := func(hour, minute int) time.Time { return time.Date(2001, time.March, 5, hour, minute, 0, 0, time.UTC) }
	opening := "daemon: opening the store; a connection error that the next attempt can clear is tried again"
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
		Lines:             []string{opening},
		Collector:         record.MainRule.Commit,
		Log:               []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"time":"2001-03-05T09:00:00Z","level":"INFO","msg":%q}`, opening))},
		Provider:          []replay.ProviderMinute{},
		Signals:           map[string][]replay.Span{replay.SignalProvider: {{From: at(9, 0), To: at(12, 0)}}},
	}
	for minute := at(9, 0); minute.Before(at(12, 0)); minute = minute.Add(time.Minute) {
		window.Provider = append(window.Provider, replay.ProviderMinute{Minute: minute, Uncached: 4500})
	}
	dir := filepath.Join(t.TempDir(), "tokens")
	if err := window.Write(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Each command that runs or reads a signal rule's run takes the project's token limit,
// --token-limit (assumption 49), as a whole number of tokens a minute above zero, and refuses 0
// and a number below it as a usage error that names the flag, before it reads a window or runs
// anything. With --from-runs, validate, forecast and compare run nothing, and each run they read
// keeps the token limit it ran with in its run.json, so they refuse the flag. models fits the
// models on the record alone and takes no token limit.
func TestReplayTokenLimitIsAWholeNumberAboveZero(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	document := filepath.Join(dir, "comparison.md")
	for _, args := range [][]string{
		{"run", "--window", dir, "--rule", "main", "--pg-dsn", "postgres://unused", "--out", dir},
		{"signals", "--window", dir, "--at", "2001-03-05T10:00:00Z"},
		{"validate", "--window", dir, "--rule", "main", "--pg-dsn", "postgres://unused", "--out", dir},
		{"forecast", "--before", dir, "--after", dir, "--rule", "main", "--pg-dsn", "postgres://unused", "--out", dir},
		{"compare", "--window", dir, "--window", dir, "--pg-dsn", "postgres://unused", "--runs", dir, "--out", document},
	} {
		for _, value := range []string{"0", "-6000000"} {
			call := append(append([]string{"replay"}, args...), "--token-limit="+value)
			if code, stdout, stderr := legionRun(t, call...); code != 2 || stdout != "" || !strings.Contains(stderr, "--token-limit "+value+":") {
				t.Errorf("legion %s = %d, stdout %q, stderr %q; want a usage error, 2, naming --token-limit %s", strings.Join(call, " "), code, stdout, stderr, value)
			}
		}
	}
	for _, args := range [][]string{
		{"validate", "--from-runs", dir},
		{"forecast", "--from-runs", dir, "--before", "window-before", "--after", "window-after"},
		{"compare", "--from-runs", dir, "--out", document},
	} {
		call := append(append([]string{"replay"}, args...), "--token-limit", "6000000")
		if code, stdout, stderr := legionRun(t, call...); code != 2 || stdout != "" || !strings.Contains(stderr, "so --token-limit does not apply") {
			t.Errorf("legion %s = %d, stdout %q, stderr %q; want a usage error, 2, saying --token-limit does not apply", strings.Join(call, " "), code, stdout, stderr)
		}
	}
	call := []string{"replay", "models", "--window", tokensWindowDir(t), "--token-limit", "6000000"}
	if code, stdout, stderr := legionRun(t, call...); code != 2 || stdout != "" || !strings.Contains(stderr, "--token-limit") {
		t.Errorf("legion %s = %d, stdout %q, stderr %q; want a usage error, 2, naming --token-limit", strings.Join(call, " "), code, stdout, stderr)
	}
}

// `legion replay signals --token-limit` reads the provider's headroom in a minute whose record holds
// no limit of the project's own against the token limit given (assumption 49). On tokensWindowDir,
// at 10:00 the minute before, 09:59, holds 4,500 tokens: with no token limit the headroom there is
// unrecorded, and with --token-limit 6000 it reads 4,500 / 6,000 = 0.75, which binds a limit of 0.75
// until the record's last minute, 11:59, ends at 12:00, where the record stops holding the minute
// before at 12:01. The kills come next, unrecorded, and no line counts the provider's refusals,
// which are no signal.
func TestReplaySignalsReadsTheHeadroomAgainstTheTokenLimit(t *testing.T) {
	t.Parallel()
	dir := tokensWindowDir(t)
	for _, c := range []struct {
		extra    []string
		provider string
	}{
		{nil, "  provider unrecorded\n    limit 0.75: no crossing due\n"},
		{[]string{"--token-limit", "6000"}, "  provider 0.75\n    limit 0.75: crosses at 2001-03-05T12:01:00Z\n"},
	} {
		call := append([]string{"replay", "signals", "--window", dir, "--at", "2001-03-05T10:00:00Z", "--limit", "provider=0.75"}, c.extra...)
		code, stdout, stderr := legionRun(t, call...)
		if code != 0 || stderr != "" || !strings.Contains(stdout, c.provider+"  kills unrecorded\n") || strings.Contains(stdout, "refusals") {
			t.Errorf("legion %s = %d, stderr %q, stdout:\n%s\nwant 0 and the lines:\n%s  kills unrecorded\nand no refusals", strings.Join(call, " "), code, stderr, stdout, c.provider)
		}
	}
}

// `legion replay models` on a window whose provider record holds tokens and no limit of the
// project's own gives the tokens model no daily test and no limit, whatever token limit a replay of
// the window takes (assumption 49): the daily test compares the model with the record only in the
// minutes where the record holds the limit, and models takes no token limit. It says when a model
// gives a limit as the docs page says it.
func TestReplayModelsGivesNoTestWhereTheRecordHoldsNoLimit(t *testing.T) {
	t.Parallel()
	code, stdout, stderr := legionRun(t, "replay", "models", "--window", tokensWindowDir(t))
	if code != 0 || stderr != "" {
		t.Fatalf("legion replay models = %d, stderr %q, stdout:\n%s\nwant 0", code, stderr, stdout)
	}
	for _, line := range []string{
		"A model gives a limit for its signal only where it passes the daily test and, with `--against`, the test across two records.",
		"## Model `tokens`, of the signal `provider`: no test",
		"The model gives no daily test: no limit of the project's own is recorded in the span, which leaves the project's own refusals nothing to test.",
		"So it gives no limit for the signal `provider`.",
	} {
		if !strings.Contains(stdout, line+"\n") {
			t.Errorf("legion replay models prints no line %q:\n%s", line, stdout)
		}
	}
	if strings.Contains(stdout, "each test that runs") {
		t.Errorf("legion replay models still says a model gives a limit where it passes each test that runs:\n%s", stdout)
	}
}
