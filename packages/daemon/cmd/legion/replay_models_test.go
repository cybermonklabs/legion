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
// with its form, its verdict under R11's daily test and the spans it skips, and --json gives the
// same as JSON. The window, from 09:00 to 12:00, holds the record of no signal: each model is not
// fitted, gives no test with its reason, names the whole span as skipped and gives no limit for
// its signal. A missing --window is a usage error.
func TestReplayModelsPrintsEachModelAndItsDailyTest(t *testing.T) {
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
	code, stdout, stderr := legionRun(t, "replay", "models", "--window", dir)
	if code != 0 || stderr != "" {
		t.Fatalf("legion replay models = %d, stderr %q, stdout:\n%s\nwant 0", code, stderr, stdout)
	}
	for _, line := range []string{
		"# The load models of ACME from 2001-03-05T09:00:00Z to 2001-03-05T12:00:00Z",
		"## Model `tokens`, of the signal `provider`: no test",
		"The model gives no test: the window holds no record of the provider in its span, so it gives no limit for the signal `provider`.",
		"## Model `memory`, of the signal `kills`: no test",
		"The model gives no test: the window holds no record of the memory guard's kills in its span, so it gives no limit for the signal `kills`.",
		"## Model `queue`, of the signal `queue`: no test",
		"The model gives no test: the window holds no record of the merge queue in its span, so it gives no limit for the signal `queue`.",
	} {
		if !strings.Contains(stdout, line+"\n") {
			t.Errorf("legion replay models prints no line %q:\n%s", line, stdout)
		}
	}

	code, stdout, stderr = legionRun(t, "replay", "models", "--window", dir, "--json")
	var models replay.LoadModels
	if err := json.Unmarshal([]byte(stdout), &models); code != 0 || stderr != "" || err != nil {
		t.Fatalf("legion replay models --json = %d, stderr %q, JSON %v, stdout:\n%s", code, stderr, err, stdout)
	}
	if models.Project != "ACME" || !models.From.Equal(at(9, 0)) || !models.To.Equal(at(12, 0)) || len(models.Models) != 3 {
		t.Fatalf("legion replay models --json gives %+v, want ACME's three models from 09:00 to 12:00", models)
	}
	for i, name := range []string{replay.ModelTokens, replay.ModelMemory, replay.ModelQueue} {
		model := models.Models[i]
		if model.Name != name || model.Fitted || model.Verdict != replay.VerdictNoTest || model.GivesLimit || len(model.Days) != 1 ||
			len(model.Skipped) != 1 || !model.Skipped[0].From.Equal(at(9, 0)) || !model.Skipped[0].To.Equal(at(12, 0)) {
			t.Errorf("legion replay models --json gives model %d as %+v, want %s not fitted, no test, no limit, one day and the whole span skipped", i+1, model, name)
		}
	}

	if code, stdout, stderr := legionRun(t, "replay", "models"); code != 2 || stdout != "" || !strings.Contains(stderr, "legion replay models") {
		t.Errorf("legion replay models with no --window = %d, stdout %q, stderr %q; want a usage error, 2", code, stdout, stderr)
	}
}
