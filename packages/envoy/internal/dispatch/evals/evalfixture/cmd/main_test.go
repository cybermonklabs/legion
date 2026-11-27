package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals/evalfixture"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// now is the moment the histories of these tests end at.
var now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// TestEveryModeButMergePRNeedsAProject: -legion, -base, -attribution, -delivery and -failures each
// write into -project, so each is refused without one, and -merge-pr, which names its pull request
// by repository and number and writes no project's rows, is not.
func TestEveryModeButMergePRNeedsAProject(t *testing.T) {
	for _, mode := range []struct {
		name string
		o    options
	}{
		{"-legion", options{legion: true}},
		{"-base", options{base: true}},
		{"-attribution", options{attribution: true}},
		{"-delivery", options{delivery: true}},
		{"-failures", options{failures: true}},
		{"-delivery -merge-pr", options{delivery: true, mergePR: "acme/api#1"}},
	} {
		mode.o.database = "postgres://unused"
		if err := check(mode.o); err == nil || !strings.Contains(err.Error(), "-project") {
			t.Errorf("%s with no -project was answered %v, want a refusal naming -project", mode.name, err)
		}
	}
	if err := check(options{database: "postgres://unused", mergePR: "acme/api#1"}); err != nil {
		t.Errorf("-merge-pr with no -project was refused: %v", err)
	}
}

// TestTheFailuresModeRefusesTheModesThatWriteItsSettings: -failures saves its projects' delivery
// settings, as -attribution and -delivery save theirs, so it is refused beside either, and -base
// writes the Legion history alone; -controller-session names the controller of -failures'
// projects, so it is refused without -failures. -failures beside -legion, and with a controller
// session, is accepted.
func TestTheFailuresModeRefusesTheModesThatWriteItsSettings(t *testing.T) {
	for _, refused := range []struct {
		name, says string
		o          options
	}{
		{"-failures -attribution", "-failures", options{failures: true, attribution: true}},
		{"-failures -delivery", "-failures", options{failures: true, delivery: true}},
		{"-failures -base", "-base", options{failures: true, base: true}},
		{"-controller-session alone", "-failures", options{legion: true, controllerSession: "ses_controller"}},
	} {
		refused.o.database, refused.o.project = "postgres://unused", "FAIL"
		if err := check(refused.o); err == nil || !strings.Contains(err.Error(), refused.says) {
			t.Errorf("%s was answered %v, want a refusal naming %s", refused.name, err, refused.says)
		}
	}
	for _, accepted := range []options{
		{failures: true, legion: true},
		{failures: true, controllerSession: "ses_controller"},
	} {
		accepted.database, accepted.project = "postgres://unused", "FAIL"
		if err := check(accepted); err != nil {
			t.Errorf("%+v was refused: %v", accepted, err)
		}
	}
}

// TestCheckRefusesNoDatabaseNoModeAndModesThatCannotRunTogether: with a project named, a run that
// names no -database, one that names no mode, -base beside another mode, and -attribution beside
// -delivery are each refused, naming what is wrong; -attribution beside -legion is not.
func TestCheckRefusesNoDatabaseNoModeAndModesThatCannotRunTogether(t *testing.T) {
	for _, refused := range []struct {
		name, reason string
		o            options
	}{
		{"-legion with no -database", "-database", options{project: "LEGK", legion: true}},
		{"no mode", "at least one mode", options{database: "postgres://unused", project: "LEGK"}},
		{"-base -legion", "-base writes the Legion history alone", options{database: "postgres://unused", project: "LEGK", base: true, legion: true}},
		{"-base -merge-pr", "-base writes the Legion history alone", options{database: "postgres://unused", project: "LEGK", base: true, mergePR: "acme/api#1"}},
		{"-attribution -delivery", "each write the project's delivery settings", options{database: "postgres://unused", project: "LEGK", attribution: true, delivery: true}},
	} {
		if err := check(refused.o); err == nil || !strings.Contains(err.Error(), refused.reason) {
			t.Errorf("%s was answered %v, want a refusal naming %q", refused.name, err, refused.reason)
		}
	}
	if err := check(options{database: "postgres://unused", project: "LEGK", attribution: true, legion: true}); err != nil {
		t.Errorf("-attribution -legion was refused: %v", err)
	}
}

// TestTheReportNamesTheSeedOfASeededModeAlone: -delivery, the one mode a seed draws, reports the
// project and the seed it wrote with; -merge-pr alone, with no project, merges the pull request it
// names and reports neither, since it wrote into no project and drew nothing.
func TestTheReportNamesTheSeedOfASeededModeAlone(t *testing.T) {
	database := storetest.Open(t)
	url := database.Pool.Config().ConnString()
	report := func(o options) map[string]json.RawMessage {
		t.Helper()
		var out bytes.Buffer
		o.database = url
		if o.now.IsZero() {
			o.now = now
		}
		if err := run(t.Context(), o, &out); err != nil {
			t.Fatalf("run %+v: %v", o, err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &keys); err != nil {
			t.Fatalf("read the report %s: %v", out.String(), err)
		}
		return keys
	}

	delivered := report(options{project: "DELV", delivery: true, days: 7, seed: 5})
	if string(delivered["seed"]) != "5" || string(delivered["project"]) != `"DELV"` {
		t.Errorf("-delivery -seed 5 reported seed %s and project %s, want 5 and DELV", delivered["seed"], delivered["project"])
	}
	var delivery evalfixture.DeliveryResult
	if err := json.Unmarshal(delivered["delivery"], &delivery); err != nil || delivery.Newest == "" {
		t.Fatalf("-delivery reported %s, want the newest pull request of the deploy repository (%v)", delivered["delivery"], err)
	}

	merged := report(options{mergePR: delivery.Newest, now: now.Add(time.Hour)})
	for _, key := range []string{"seed", "project"} {
		if value, held := merged[key]; held {
			t.Errorf("-merge-pr alone reported %s %s, want no %s", key, value, key)
		}
	}
	var merge evalfixture.MergeResult
	if err := json.Unmarshal(merged["merge_pr"], &merge); err != nil || merge.PullRequest != delivery.Newest {
		t.Errorf("-merge-pr %s reported %s, want that pull request merged (%v)", delivery.Newest, merged["merge_pr"], err)
	}
}
