package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The boot reports what the provisioned list and the store say of private projects, and stays up
// whatever it finds: a project the database holds private that the list does not name is one
// ERROR line naming it and both repairs; a listed key that is no private project yet is one INFO
// line; and, with a list, a store a private project's reads would answer wrongly is one ERROR line
// naming its first rows and the command that repairs it.
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
		reportPrivateProjects(ctx, database, state)
		return logged.String()
	}
	unprovisioned := "dispatch: private project not provisioned: PRIV is private but DISPATCH_PRIVATE_PROJECTS does not name it"

	t.Run("PRIV off an empty list", func(t *testing.T) {
		logged := report(t, nil)
		line := lineHolding(logged, unprovisioned)
		if !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "legion.PrivateProjects") ||
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
	t.Run("an incomplete store", func(t *testing.T) {
		// PRIV-1 is an issue a build before the node writers stored with no node.
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('PRIV-1', 'PRIV', 1, 'One', '{"kind":"user","id":"alice@example.com"}', 'U');
		`); err != nil {
			t.Fatalf("seed PRIV-1 with no node: %v", err)
		}
		logged := report(t, []string{"PRIV"})
		if line := lineHolding(logged, "issue PRIV-1: missing"); !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "envoy-dispatch backfill-graph-store") {
			t.Fatalf("the boot's report over an incomplete store has no ERROR line naming PRIV-1 and its repair:\n%s", logged)
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
// and over an incomplete store: it logs both and answers /healthz 200, ok, naming the project in
// private_projects_unprovisioned. The whole wiring of main: the setting's read, the boot's report
// and the probe's cached read.
func TestAProcessServesOverAnUnprovisionedPrivateProject(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, visibility) values ('PRIV', 'Private', 'alice@example.com', 'private');
	`); err != nil {
		t.Fatalf("seed the private project PRIV: %v", err)
	}
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{Env: map[string]string{"DISPATCH_PRIVATE_PROJECTS": "OTHER"}})
	process.waitHealthy(t)
	output := process.Output.String()
	for _, want := range []string{
		"private project not provisioned: PRIV is private",
		"session_projects_filled holds no row",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("the boot's log holds no line naming %q:\n%s", want, output)
		}
	}
	response, err := http.Get(process.url("/healthz"))
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer response.Body.Close()
	var health struct {
		OK            bool     `json:"ok"`
		Unprovisioned []string `json:"private_projects_unprovisioned"`
	}
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	if response.StatusCode != http.StatusOK || !health.OK || len(health.Unprovisioned) != 1 || health.Unprovisioned[0] != "PRIV" {
		t.Fatalf("/healthz = %d %+v, want 200, ok, and PRIV unprovisioned", response.StatusCode, health)
	}
}
