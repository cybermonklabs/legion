package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// projectDeliverySettingsVersion is the migration that gives each project its own delivery
// settings (dispatch://INFRA-978, R1) and copies the one delivery_settings row to the project whose
// default repository is its deploy repository.
const projectDeliverySettingsVersion = 20261123010254

// seedDeliverySettingsSingleton seeds, at the schema before the migration, the one row the release
// before it writes, deploying acme/widgets.
func seedDeliverySettingsSingleton(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.Pool.Exec(context.Background(), `
		insert into delivery_settings (singleton, deploy_repo, deploy_workflow_path, production_job_names,
			production_apply_step_name, pr_checks_workflow_path, population_authors, excluded_repos,
			staging_e2e_job_name, staging_apply_job_name, pr_checks_rollup_job_name, last_event_at, updated_by)
		values (true, 'acme/widgets', '.github/workflows/deploy.yaml', '{production,production-eu}', 'Apply',
			'.github/workflows/ci.yaml', '{alice,bob}', '{acme/old}', 'e2e', 'staging-apply', 'ci-ok',
			'2026-11-20T10:00:00Z', '{"kind":"user","id":"alice@example.com"}')
	`); err != nil {
		t.Fatalf("seed the singleton delivery settings: %v", err)
	}
}

// The one delivery_settings row moves to the project whose default repository it deploys: a
// settings row with delivery on and its population, and one github_workflow source naming no
// repository with every rule field and the bookkeeping the row held; no other project gets a row,
// and the release before the migration still reads the singleton.
func TestTheSingletonSettingsMoveToTheProjectThatDeploysTheirRepository(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, projectDeliverySettingsVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", projectDeliverySettingsVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, default_repo) values
			('ACME', 'Acme', 'alice', 'acme/widgets'), ('OTHER', 'Other', 'alice', 'acme/other'), ('BARE', 'Bare', 'alice', null)
	`); err != nil {
		t.Fatalf("seed the projects: %v", err)
	}
	seedDeliverySettingsSingleton(t, store)
	if got := censusCount(t, store, projectDeliverySettingsVersion); got != 0 {
		t.Fatalf("census over a row one project's default repository places = %d, want 0", got)
	}
	if err := store.migrate(ctx, migrationsThrough(t, projectDeliverySettingsVersion)); err != nil {
		t.Fatalf("migrate through %d: %v", projectDeliverySettingsVersion, err)
	}

	var rows int
	var project string
	var delivery bool
	var authors, excluded, sources []string
	if err := store.Pool.QueryRow(ctx, `
		select (select count(*) from project_delivery_settings), project_key, delivery, population_authors,
			excluded_repos, failure_sources
		from project_delivery_settings
	`).Scan(&rows, &project, &delivery, &authors, &excluded, &sources); err != nil {
		t.Fatalf("read the copied settings row: %v", err)
	}
	if rows != 1 || project != "ACME" || !delivery || len(authors) != 2 || len(excluded) != 1 || excluded[0] != "acme/old" || len(sources) != 0 {
		t.Fatalf("copied %d settings rows, the first %s delivery=%v authors=%v excluded=%v sources=%v; want one, ACME's, on, with the population and no failure source",
			rows, project, delivery, authors, excluded, sources)
	}
	var sourceRows int
	var kind, workflow, apply, checks, e2e, stagingApply, rollup string
	var repo *string
	var jobs []string
	var lastEvent *time.Time
	if err := store.Pool.QueryRow(ctx, `
		select (select count(*) from project_deploy_sources), kind, repo, deploy_workflow_path, production_job_names,
			production_apply_step_name, pr_checks_workflow_path, staging_e2e_job_name, staging_apply_job_name,
			pr_checks_rollup_job_name, last_event_at
		from project_deploy_sources where project_key = 'ACME'
	`).Scan(&sourceRows, &kind, &repo, &workflow, &jobs, &apply, &checks, &e2e, &stagingApply, &rollup, &lastEvent); err != nil {
		t.Fatalf("read the copied source: %v", err)
	}
	if sourceRows != 1 || kind != "github_workflow" || repo != nil {
		t.Fatalf("copied %d sources, the first %s naming %v; want one github_workflow source naming no repository", sourceRows, kind, repo)
	}
	if workflow != ".github/workflows/deploy.yaml" || len(jobs) != 2 || jobs[1] != "production-eu" || apply != "Apply" ||
		checks != ".github/workflows/ci.yaml" || e2e != "e2e" || stagingApply != "staging-apply" || rollup != "ci-ok" ||
		lastEvent == nil || !lastEvent.Equal(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("the copied source lost a field: %s %v %s %s %s %s %s %v", workflow, jobs, apply, checks, e2e, stagingApply, rollup, lastEvent)
	}
	var singleton int
	if err := store.Pool.QueryRow(ctx, `select count(*) from delivery_settings where singleton`).Scan(&singleton); err != nil || singleton != 1 {
		t.Fatalf("delivery_settings rows after the migration = %d (%v), want the singleton kept", singleton, err)
	}
}

// The census counts the singleton row the copy cannot place: one whose repository is no project's
// default, and one two projects share as their default. The migration's copy would drop either, so
// the census refuses the deploy before it.
func TestCensusCountsASingletonNoOneProjectDeploys(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, projectDeliverySettingsVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", projectDeliverySettingsVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `insert into projects (key, name, default_assignee, default_repo) values ('OTHER', 'Other', 'alice', 'acme/other')`); err != nil {
		t.Fatalf("seed a project: %v", err)
	}
	if got := censusCount(t, store, projectDeliverySettingsVersion); got != 0 {
		t.Fatalf("census with no singleton row = %d, want 0", got)
	}
	seedDeliverySettingsSingleton(t, store)
	if got := censusCount(t, store, projectDeliverySettingsVersion); got != 1 {
		t.Fatalf("census over a row no project's default repository names = %d, want 1", got)
	}
	if _, err := store.Pool.Exec(ctx, `insert into projects (key, name, default_assignee, default_repo) values
		('ONE', 'One', 'alice', 'acme/widgets'), ('TWO', 'Two', 'alice', 'acme/widgets')`); err != nil {
		t.Fatalf("seed two projects sharing a default repository: %v", err)
	}
	if got := censusCount(t, store, projectDeliverySettingsVersion); got != 1 {
		t.Fatalf("census over a row two projects' default repository names = %d, want 1", got)
	}
}

// The tables' checks refuse what no reader could read: a github_workflow source with no
// workflow, an empty production job list, an events source holding a repository, a kind that is
// neither, a repository that is not in lower case (the column checks `repo = lower(repo)`, and
// the row here has capitals), two sources reading one repository (two naming none among them), a
// second events source, a failure source that is not one of the four, and a failure source with
// no first day; a full github_workflow source, a second one naming another repository and one
// events source are accepted.
func TestTheProjectSettingsTablesRefuseARowNoReaderCouldRead(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, default_repo) values ('ACME', 'Acme', 'alice', 'acme/widgets');
		insert into project_delivery_settings (project_key, delivery, updated_by) values ('ACME', true, '{"kind":"user","id":"alice"}')
	`); err != nil {
		t.Fatalf("seed a project and its settings: %v", err)
	}
	type source struct {
		kind     string
		repo     *string
		workflow *string
		jobs     []string
		checks   *string
	}
	insert := func(position int, src source) error {
		_, err := store.Pool.Exec(ctx, `insert into project_deploy_sources (project_key, position, kind, repo,
			deploy_workflow_path, production_job_names, pr_checks_workflow_path) values ('ACME', $1, $2, $3, $4, $5, $6)`,
			position, src.kind, src.repo, src.workflow, src.jobs, src.checks)
		return err
	}
	clear := func() {
		t.Helper()
		if _, err := store.Pool.Exec(ctx, `delete from project_deploy_sources`); err != nil {
			t.Fatal(err)
		}
	}
	workflow, checks := new("d.yaml"), new("ci.yaml")
	full := func(repo *string) source {
		return source{kind: "github_workflow", repo: repo, workflow: workflow, jobs: []string{"production"}, checks: checks}
	}
	for why, refused := range map[string]source{
		"a github_workflow source with no workflow": {kind: "github_workflow"},
		"an empty production job list":              {kind: "github_workflow", workflow: workflow, jobs: []string{}, checks: checks},
		"an events source holding a repository":     {kind: "events", repo: new("acme/deploys")},
		"a kind that is neither":                    {kind: "cron"},
		"a repository with capitals":                full(new("Acme/Deploys")),
	} {
		clear()
		if err := insert(0, refused); err == nil {
			t.Errorf("%s was accepted", why)
		} else if !strings.Contains(err.Error(), "project_deploy_sources") {
			t.Errorf("%s was refused by something other than the table: %v", why, err)
		}
	}
	for why, pair := range map[string][2]source{
		"two sources naming one repository": {full(new("acme/deploys")), full(new("acme/deploys"))},
		"two sources naming none":           {full(nil), full(nil)},
		"two events sources":                {{kind: "events"}, {kind: "events"}},
	} {
		clear()
		if err := insert(0, pair[0]); err != nil {
			t.Fatalf("%s: the first source was refused: %v", why, err)
		}
		if err := insert(1, pair[1]); err == nil {
			t.Errorf("%s were accepted", why)
		}
	}
	clear()
	for position, accepted := range []source{full(nil), full(new("acme/deploys")), {kind: "events"}} {
		if err := insert(position, accepted); err != nil {
			t.Errorf("source %d was refused: %v", position, err)
		}
	}
	for why, sql := range map[string]string{
		"an unknown failure source":          `update project_delivery_settings set failure_sources = '{pager}', failures_measured_since = '2026-11-01'`,
		"a failure source with no first day": `update project_delivery_settings set failure_sources = '{reverts}', failures_measured_since = null`,
	} {
		if _, err := store.Pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s was accepted", why)
		} else if !strings.Contains(err.Error(), "project_delivery_settings_") {
			t.Errorf("%s was refused by something other than a check of the table: %v", why, err)
		}
	}
	if _, err := store.Pool.Exec(ctx, `update project_delivery_settings set failure_sources = '{events,reverts}', failures_measured_since = '2026-11-01'`); err != nil {
		t.Errorf("two failure sources with their first day were refused: %v", err)
	}
}
