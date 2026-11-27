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

// `legion replay models` prints the three load models of R11 fitted on a window's record, each
// with its form, its verdict under R11's daily test, the test across two records and the spans it
// skips, and --json gives the same as JSON. The window, from 09:00 to 12:00, holds the record of no
// signal: each model is not fitted, gives no test with its reason, names the whole span as skipped
// and gives no limit for its signal. With no --against the output says that the test across two
// records did not run. With --against, here the same window, each model's rate has no reading and
// gives no test across two records, no repository has a queue to name, and the kills are counted
// from --kills-from, by default 2026-11-18T00:00:00Z. A missing --window and a --kills-from that is
// no time are usage errors.
func TestReplayModelsPrintsEachModelAndItsDailyTest(t *testing.T) {
	t.Parallel()
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
	}
	dir := t.TempDir()
	if err := window.Write(dir); err != nil {
		t.Fatal(err)
	}
	printsLines := func(name, stdout string, lines ...string) {
		t.Helper()
		for _, line := range lines {
			if !strings.Contains(stdout, line+"\n") {
				t.Errorf("%s prints no line %q:\n%s", name, line, stdout)
			}
		}
	}
	code, stdout, stderr := legionRun(t, "replay", "models", "--window", dir)
	if code != 0 || stderr != "" {
		t.Fatalf("legion replay models = %d, stderr %q, stdout:\n%s\nwant 0", code, stderr, stdout)
	}
	printsLines("legion replay models", stdout,
		"# The load models of ACME from 2001-03-05T09:00:00Z to 2001-03-05T12:00:00Z",
		"R11's test across two records did not run, so a model that passes the daily test gives its limit on that test alone.",
		"## Model `tokens`, of the signal `provider`: no test",
		"The model gives no daily test: the window holds no record of the provider in its span.",
		"So it gives no limit for the signal `provider`.",
		"## Model `memory`, of the signal `kills`: no test",
		"The model gives no daily test: the window holds no record of the memory guard's kills in its span.",
		"## Model `queue`, of the signal `queue`: no test",
		"The model gives no daily test: the window holds no record of the merge queue in its span.",
		"R11's test across two records did not run.",
	)
	if strings.Contains(stdout, "## R11's test across two records") {
		t.Errorf("legion replay models with no --against prints a test across two records:\n%s", stdout)
	}

	code, stdout, stderr = legionRun(t, "replay", "models", "--window", dir, "--json")
	var models replay.LoadModels
	if err := json.Unmarshal([]byte(stdout), &models); code != 0 || stderr != "" || err != nil {
		t.Fatalf("legion replay models --json = %d, stderr %q, JSON %v, stdout:\n%s", code, stderr, err, stdout)
	}
	if models.Project != "ACME" || !models.From.Equal(at(9, 0)) || !models.To.Equal(at(12, 0)) || len(models.Models) != 3 || models.Across != nil {
		t.Fatalf("legion replay models --json gives %+v, want ACME's three models from 09:00 to 12:00 and no test across two records", models)
	}
	for i, name := range []string{replay.ModelTokens, replay.ModelMemory, replay.ModelQueue} {
		model := models.Models[i]
		if model.Name != name || model.Fitted || model.Verdict != replay.VerdictNoTest || model.Across != replay.VerdictNotRun || model.GivesLimit ||
			len(model.Days) != 1 || len(model.Skipped) != 1 || !model.Skipped[0].From.Equal(at(9, 0)) || !model.Skipped[0].To.Equal(at(12, 0)) {
			t.Errorf("legion replay models --json gives model %d as %+v, want %s not fitted, no test, not run across records, no limit, one day and the whole span skipped",
				i+1, model, name)
		}
	}

	code, stdout, stderr = legionRun(t, "replay", "models", "--window", dir, "--against", dir)
	if code != 0 || stderr != "" {
		t.Fatalf("legion replay models --against = %d, stderr %q, stdout:\n%s\nwant 0", code, stderr, stdout)
	}
	noReading := "no test: the rate has no reading on one of the two records, which holds no record of its signal or no exposure to it"
	printsLines("legion replay models --against", stdout,
		"R11's test across two records compares the rate that each model rests on with the same rate on the record of ACME from "+
			"2001-03-05T09:00:00Z to 2001-03-05T12:00:00Z, in the last section.",
		"## R11's test across two records",
		"The other record is the window of ACME from 2001-03-05T09:00:00Z to 2001-03-05T12:00:00Z, and the kills are counted from 2026-11-18T00:00:00Z on each record.",
		"| tokens | uncached input tokens for each minute of an issue in flight | – | – (0 over 0) | – (0 over 0) | – | "+noReading+" |",
		"| memory | kills for each hour of an issue in flight | – | – (0 over 0) | – (0 over 0) | – | "+noReading+" |",
		"R11's test across two records gives it no test: no repository's queue has 50 exits from its head in each record.",
		"No repository's queue is left untested.",
	)

	code, stdout, stderr = legionRun(t, "replay", "models", "--window", dir, "--against", dir, "--kills-from", "2001-03-05T10:00:00Z", "--json")
	models = replay.LoadModels{}
	if err := json.Unmarshal([]byte(stdout), &models); code != 0 || stderr != "" || err != nil {
		t.Fatalf("legion replay models --against --json = %d, stderr %q, JSON %v, stdout:\n%s", code, stderr, err, stdout)
	}
	if models.Across == nil || !models.Across.KillsFrom.Equal(at(10, 0)) || len(models.Across.Rates) != 2 || len(models.Across.Untested) != 0 {
		t.Fatalf("legion replay models --against --json gives the test across two records %+v, want the kills counted from 10:00, two rates and no repository", models.Across)
	}
	for _, model := range models.Models {
		if model.Across != replay.VerdictNoTest || model.AcrossReason == "" || model.GivesLimit {
			t.Errorf("legion replay models --against --json gives model %s across records %s (%q) and a limit %t, want no test with its reason and no limit",
				model.Name, model.Across, model.AcrossReason, model.GivesLimit)
		}
	}

	if code, stdout, stderr := legionRun(t, "replay", "models"); code != 2 || stdout != "" || !strings.Contains(stderr, "legion replay models") {
		t.Errorf("legion replay models with no --window = %d, stdout %q, stderr %q; want a usage error, 2", code, stdout, stderr)
	}
	if code, stdout, stderr := legionRun(t, "replay", "models", "--window", dir, "--kills-from", "the 18th"); code != 2 || stdout != "" ||
		!strings.Contains(stderr, "--kills-from") {
		t.Errorf("legion replay models --kills-from 'the 18th' = %d, stdout %q, stderr %q; want a usage error, 2", code, stdout, stderr)
	}
}
