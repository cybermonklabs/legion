package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The boot reports what the two lists say of private projects, and stays up whatever it finds: a
// project the database holds private that neither list names is one ERROR line naming it and both
// repairs; a provisioned key that is no private project yet is one INFO line; a project private
// before its full switch is one INFO line naming it as such, and a key of that list that is no
// private project yet another. The store's own check is not the boot's
// (TestAProcessServesOverAnUnprovisionedPrivateProject).
func TestABootReportsAnUnprovisionedPrivateProjectAndStaysUp(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, visibility) values ('PRIV', 'Private', 'alice@example.com', 'private');
		insert into session_projects_filled default values;
	`); err != nil {
		t.Fatalf("seed the private project PRIV: %v", err)
	}
	report := func(t *testing.T, list, beforeFullSwitch []string) string {
		t.Helper()
		var logged cmdtest.LockedBuffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		state, err := startPrivateProjectsState(t.Context(), database.Pool, list, beforeFullSwitch)
		if err != nil {
			t.Fatalf("the first read of the private projects with the lists %v and %v: %v", list, beforeFullSwitch, err)
		}
		reportPrivateProjects(state)
		return logged.String()
	}
	const beforeFullSwitchLine = "dispatch: private before its full switch"
	unprovisioned := "dispatch: private project not provisioned: PRIV is private but DISPATCH_PRIVATE_PROJECTS does not name it"

	t.Run("PRIV off an empty list", func(t *testing.T) {
		logged := report(t, nil, nil)
		line := lineHolding(logged, unprovisioned)
		if !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "shared/legion/privateprojects/lists.go") ||
			!strings.Contains(line, "envoy-dispatch project-visibility PRIV public") {
			t.Fatalf("the boot's report names PRIV with no ERROR line naming it and both repairs:\n%s", logged)
		}
	})
	t.Run("PRIV on the list", func(t *testing.T) {
		if logged := report(t, []string{"PRIV"}, nil); strings.Contains(logged, "level=ERROR") || strings.Contains(logged, beforeFullSwitchLine) {
			t.Fatalf("the boot's report with PRIV on the list logged an ERROR:\n%s", logged)
		}
	})
	t.Run("NEW provisioned but no project", func(t *testing.T) {
		logged := report(t, []string{"NEW", "PRIV"}, nil)
		if line := lineHolding(logged, "provisioned but not private"); !strings.Contains(line, "level=INFO") || !strings.Contains(line, "NEW") || strings.Contains(line, "PRIV") {
			t.Fatalf("the boot's report names NEW, provisioned and no private project, with no INFO line naming it alone:\n%s", logged)
		}
	})
	t.Run("PRIV private before its full switch", func(t *testing.T) {
		logged := report(t, nil, []string{"PRIV"})
		if strings.Contains(logged, "level=ERROR") {
			t.Fatalf("the boot's report with PRIV private before its full switch logged an ERROR:\n%s", logged)
		}
		if line := lineHolding(logged, beforeFullSwitchLine); !strings.Contains(line, "level=INFO") || !strings.Contains(line, "projects=PRIV") ||
			!strings.Contains(line, "DISPATCH_PRIVATE_BEFORE_FULL_SWITCH") {
			t.Fatalf("the boot's report names PRIV, private before its full switch, with no INFO line naming it as such:\n%s", logged)
		}
	})
	t.Run("DAYONE named before its full switch but not private yet", func(t *testing.T) {
		logged := report(t, nil, []string{"DAYONE", "PRIV"})
		// The line names the setting, DISPATCH_PRIVATE_BEFORE_FULL_SWITCH, so it is read by its
		// projects attribute, the line's last, which must be DAYONE alone: PRIV is private.
		if line := lineHolding(logged, "may be made private before its full switch, not private yet"); !strings.Contains(line, "level=INFO") ||
			!strings.HasSuffix(line, " projects=DAYONE\n") {
			t.Fatalf("the boot's report names DAYONE, listed and no private project, with no INFO line naming it alone:\n%s", logged)
		}
	})
	t.Run("an incomplete store is not the boot's to read", func(t *testing.T) {
		// PRIV-1 is an issue a build before the node writers stored with no node: the store's
		// check names it (api.TestTheStoreReportNamesEveryBackfillNotRun), and the boot serves
		// without waiting for that read.
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('PRIV-1', 'PRIV', 1, 'One', '{"kind":"user","id":"alice@example.com"}', 'U');
		`); err != nil {
			t.Fatalf("seed PRIV-1 with no node: %v", err)
		}
		if logged := report(t, []string{"PRIV"}, nil); strings.Contains(logged, "PRIV-1") {
			t.Fatalf("the boot's report read the store's census:\n%s", logged)
		}
	})
}

// lineHolding is the first line of logged that holds substring, or "".
func lineHolding(logged, substring string) string {
	for line := range strings.Lines(logged) {
		if strings.Contains(line, substring) {
			return line
		}
	}
	return ""
}

// A process given DISPATCH_PRIVATE_PROJECTS serves over a private project its list does not name
// and over an incomplete store: it logs both, the project's key in the log, and answers /healthz
// 200, ok, degraded for it, naming no key. The store's ERROR line comes once as the maintenance
// loop starts and then hourly, so the loop's next passes repeat it nowhere. The whole wiring of
// main: the setting's read, the boot's report, the loop's check and the probe's cached read.
func TestAProcessServesOverAnUnprovisionedPrivateProject(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, visibility) values ('PRIV', 'Private', 'alice@example.com', 'private');
	`); err != nil {
		t.Fatalf("seed the private project PRIV: %v", err)
	}
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{Env: map[string]string{
		"DISPATCH_PRIVATE_PROJECTS":                "OTHER",
		"DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL": "250ms",
	}})
	process.waitHealthy(t)
	// Two passes of the maintenance loop have run once two `leased pass ran` lines name its lease:
	// one process runs them one after another.
	const storeLine, maintenancePass = "the store cannot hold a private project yet",
		`msg="dispatch duty: leased pass ran" lease=suggestion-outcomes`
	for deadline := time.Now().Add(60 * time.Second); strings.Count(process.Output.String(), maintenancePass) < 2 ||
		!strings.Contains(process.Output.String(), storeLine); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the store's line and two passes of the maintenance loop:\n%s", process.Output.String())
		}
	}
	output := process.Output.String()
	for _, want := range []string{
		"private project not provisioned: PRIV is private",
		"session_projects_filled holds no row",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("the process's log holds no line naming %q:\n%s", want, output)
		}
	}
	if got := strings.Count(output, storeLine); got != 1 {
		t.Errorf("the process logged %d lines saying %q over its start and %d passes of the maintenance loop, want 1:\n%s",
			got, storeLine, strings.Count(output, maintenancePass), output)
	}
	response, err := http.Get(process.url("/healthz"))
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer response.Body.Close()
	var health struct {
		OK       bool     `json:"ok"`
		Degraded []string `json:"degraded"`
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read /healthz: %v", err)
	}
	if err := json.Unmarshal(raw, &health); err != nil {
		t.Fatalf("decode /healthz %s: %v", raw, err)
	}
	if response.StatusCode != http.StatusOK || !health.OK || !slices.Equal(health.Degraded, []string{"private_project_unprovisioned"}) || strings.Contains(string(raw), "PRIV") {
		t.Fatalf(`/healthz = %d %s, want 200, ok, degraded ["private_project_unprovisioned"], and no project's key`, response.StatusCode, raw)
	}
}

// A process given DISPATCH_PRIVATE_BEFORE_FULL_SWITCH=PRIV,1 over PRIV private (spec "Day one and
// the full switch") ignores the entry 1 with one ERROR line naming the setting and the entry, names
// PRIV in one INFO line as private before its full switch, never reports it as a private project
// no list names (not at boot, not in two passes of the maintenance loop), and answers /healthz
// 200, ok, degraded [private_before_full_switch], naming no key. The whole wiring of main: the
// setting's read, the boot's report, the loop's list and the probe's cached read.
func TestAProcessServesAProjectPrivateBeforeItsFullSwitch(t *testing.T) {
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee, visibility) values ('PRIV', 'Private', 'alice@example.com', 'private');
		insert into session_projects_filled default values;
		insert into access_events_scrubbed default values;
	`); err != nil {
		t.Fatalf("seed the private project PRIV: %v", err)
	}
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{Env: map[string]string{
		"DISPATCH_PRIVATE_BEFORE_FULL_SWITCH":      "PRIV,1",
		"DISPATCH_TEST_MAINTENANCE_SWEEP_INTERVAL": "250ms",
	}})
	process.waitHealthy(t)
	const maintenancePass = `msg="dispatch duty: leased pass ran" lease=suggestion-outcomes`
	waitUntil(t, 60*time.Second, "two passes of the maintenance loop", func() bool {
		return strings.Count(process.Output.String(), maintenancePass) >= 2
	})
	output := process.Output.String()
	if line := lineHolding(output, "dispatch: private before its full switch"); !strings.Contains(line, "level=INFO") || !strings.Contains(line, "projects=PRIV") {
		t.Errorf("the process's log holds no INFO line naming PRIV private before its full switch:\n%s", output)
	}
	if line := lineHolding(output, "setting=DISPATCH_PRIVATE_BEFORE_FULL_SWITCH"); !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "entry=1") ||
		!strings.Contains(line, "PRIVATE_PROJECT_NOT_PROVISIONED") {
		t.Errorf("the process's log holds no ERROR line naming the ignored entry 1 of DISPATCH_PRIVATE_BEFORE_FULL_SWITCH:\n%s", output)
	}
	if strings.Contains(output, "private project not provisioned: PRIV") {
		t.Errorf("the process reported PRIV, private before its full switch, as a private project no list names:\n%s", output)
	}
	health := process.readHealthz(t)
	if health.status != http.StatusOK || !health.OK || !slices.Equal(health.Degraded, []string{"private_before_full_switch"}) || strings.Contains(health.body, "PRIV") {
		t.Fatalf(`/healthz = %d %s, want 200, ok, degraded ["private_before_full_switch"], and no project's key`, health.status, health.body)
	}
}
