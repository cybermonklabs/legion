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

// The boot reports what the provisioned list says of private projects, and stays up whatever it
// finds: a project the database holds private that the list does not name is one ERROR line
// naming it and both repairs; a listed key that is no private project yet is one INFO line. The
// store's own check is not the boot's (TestAProcessServesOverAnUnprovisionedPrivateProject).
func TestABootReportsAnUnprovisionedPrivateProjectAndStaysUp(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, visibility) values ('PRIV', 'Private', 'alice@example.com', 'private');
		insert into session_projects_filled default values;
	`); err != nil {
		t.Fatalf("seed the private project PRIV: %v", err)
	}
	report := func(t *testing.T, list []string) string {
		t.Helper()
		var logged cmdtest.LockedBuffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		state, err := startPrivateProjectsState(t.Context(), database.Pool, list)
		if err != nil {
			t.Fatalf("the first read of the private projects with the list %v: %v", list, err)
		}
		reportPrivateProjects(state)
		return logged.String()
	}
	unprovisioned := "dispatch: private project not provisioned: PRIV is private but DISPATCH_PRIVATE_PROJECTS does not name it"

	t.Run("PRIV off an empty list", func(t *testing.T) {
		logged := report(t, nil)
		line := lineHolding(logged, unprovisioned)
		if !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "shared/legion/privateprojects/lists.go") ||
			!strings.Contains(line, "envoy-dispatch project-visibility PRIV public") {
			t.Fatalf("the boot's report names PRIV with no ERROR line naming it and both repairs:\n%s", logged)
		}
	})
	t.Run("PRIV on the list", func(t *testing.T) {
		if logged := report(t, []string{"PRIV"}); strings.Contains(logged, "level=ERROR") {
			t.Fatalf("the boot's report with PRIV on the list logged an ERROR:\n%s", logged)
		}
	})
	t.Run("NEW provisioned but no project", func(t *testing.T) {
		logged := report(t, []string{"NEW", "PRIV"})
		if line := lineHolding(logged, "provisioned but not private"); !strings.Contains(line, "level=INFO") || !strings.Contains(line, "NEW") || strings.Contains(line, "PRIV") {
			t.Fatalf("the boot's report names NEW, provisioned and no private project, with no INFO line naming it alone:\n%s", logged)
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
		if logged := report(t, []string{"PRIV"}); strings.Contains(logged, "PRIV-1") {
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
	if response.StatusCode != http.StatusOK || !health.OK || !slices.Equal(health.Degraded, []string{unprovisionedPrivateProjectReason}) || strings.Contains(string(raw), "PRIV") {
		t.Fatalf("/healthz = %d %s, want 200, ok, degraded [%q], and no project's key", response.StatusCode, raw, unprovisionedPrivateProjectReason)
	}
}
