package delivery

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

var settingsAuthor = model.Actor{Kind: "user", ID: "alice@example.com"}

// settingsStore is a store holding projects ACME (default acme/widgets), BETA (default acme/beta)
// and BARE (no default repository).
func settingsStore(t *testing.T) (*store.Pool, context.Context) {
	t.Helper()
	database := storetest.Open(t)
	ctx := t.Context()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee, default_repo) values
		('ACME', 'Acme', 'alice', 'acme/widgets'), ('BETA', 'Beta', 'alice', 'acme/beta'), ('BARE', 'Bare', 'alice', null)`); err != nil {
		t.Fatalf("seed the projects: %v", err)
	}
	return database.Pool, ctx
}

func workflowSource(repo *string, workflow string) SourceInput {
	return SourceInput{
		Kind: SourceKindGitHubWorkflow, Repo: repo, DeployWorkflowPath: new(workflow),
		ProductionJobNames: []string{"production"}, PRChecksWorkflowPath: new(".github/workflows/ci.yaml"),
	}
}

func save(t *testing.T, ctx context.Context, pool *store.Pool, input ProjectSettingsInput) (ProjectSettings, error) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	saved, err := PutProjectSettings(ctx, tx, input, settingsAuthor)
	if err != nil {
		return ProjectSettings{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return saved, nil
}

func refusedWith(t *testing.T, err error, code string) *SettingsError {
	t.Helper()
	var refusal *SettingsError
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("got %v, want a %s refusal", err, code)
	}
	return refusal
}

// Each project keeps its own settings and sources: ACME reads two repositories, one it names and
// its default through a source that names none, and BETA one; DeployRules gives each repository its
// own rule and owner, and a project with delivery off gives none.
func TestEachProjectKeepsItsOwnDeliverySettings(t *testing.T) {
	pool, ctx := settingsStore(t)
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	acme, err := save(t, ctx, pool, ProjectSettingsInput{
		Project: "ACME", Delivery: true,
		Sources:           []SourceInput{workflowSource(new("acme/deploys"), "deploy.yaml"), workflowSource(nil, "release.yaml")},
		PopulationAuthors: []string{"alice"}, FailureSources: []string{FailureSourceReverts}, FailuresMeasuredSince: &since,
	})
	if err != nil {
		t.Fatalf("save ACME: %v", err)
	}
	if _, err := save(t, ctx, pool, ProjectSettingsInput{Project: "BETA", Delivery: true, Sources: []SourceInput{workflowSource(nil, "ship.yaml")}}); err != nil {
		t.Fatalf("save BETA: %v", err)
	}
	if _, err := save(t, ctx, pool, ProjectSettingsInput{Project: "BARE", Delivery: false}); err != nil {
		t.Fatalf("save BARE off: %v", err)
	}
	if len(acme.Sources) != 2 || acme.Sources[0].Repo == nil || *acme.Sources[0].Repo != "acme/deploys" || acme.Sources[1].Repo != nil {
		t.Fatalf("ACME's sources read back %+v, want acme/deploys then one naming none", acme.Sources)
	}
	if acme.FailuresMeasuredSince == nil || !acme.FailuresMeasuredSince.Equal(since) || !slices.Equal(acme.FailureSources, []string{FailureSourceReverts}) {
		t.Errorf("ACME's failure sources read back %v since %v", acme.FailureSources, acme.FailuresMeasuredSince)
	}
	all, err := ListProjectSettings(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, rule := range DeployRules(all) {
		got = append(got, rule.DeployRepo+" "+rule.Project+" "+rule.DeployWorkflowPath)
	}
	want := []string{"acme/beta BETA ship.yaml", "acme/deploys ACME deploy.yaml", "acme/widgets ACME release.yaml"}
	if !slices.Equal(got, want) {
		t.Errorf("DeployRules = %v, want %v", got, want)
	}
	if _, err := GetProjectSettings(ctx, pool, "NONE"); !errors.Is(err, ErrNoSettings) {
		t.Errorf("a project with no row answered %v, want ErrNoSettings", err)
	}
}

// A repository is the source of one delivery-on project at most, and of one source of it: a save
// naming another delivery-on project's repository, by name or through its default, is refused with
// that project, and so is a project naming one repository twice; with the other project off, the
// repository is free.
func TestASaveThatWouldShareADeployRepositoryIsRefused(t *testing.T) {
	pool, ctx := settingsStore(t)
	if _, err := save(t, ctx, pool, ProjectSettingsInput{Project: "ACME", Delivery: true, Sources: []SourceInput{workflowSource(nil, "d.yaml")}}); err != nil {
		t.Fatalf("save ACME: %v", err)
	}
	_, err := save(t, ctx, pool, ProjectSettingsInput{Project: "BETA", Delivery: true, Sources: []SourceInput{workflowSource(new("Acme/Widgets"), "d.yaml")}})
	if refusal := refusedWith(t, err, CodeDeliveryRepositoryShared); refusal.Project != "ACME" || refusal.Source == nil || *refusal.Source != 0 {
		t.Errorf("the refusal names project %q and source %v, want ACME and 0", refusal.Project, refusal.Source)
	}
	_, err = save(t, ctx, pool, ProjectSettingsInput{Project: "BETA", Delivery: true,
		Sources: []SourceInput{workflowSource(new("acme/beta"), "a.yaml"), workflowSource(nil, "b.yaml")}})
	refusedWith(t, err, CodeDeliverySettingsInput)
	if _, err := save(t, ctx, pool, ProjectSettingsInput{Project: "ACME", Delivery: false, Sources: []SourceInput{workflowSource(nil, "d.yaml")}}); err != nil {
		t.Fatalf("turn ACME off: %v", err)
	}
	if _, err := save(t, ctx, pool, ProjectSettingsInput{Project: "BETA", Delivery: true, Sources: []SourceInput{workflowSource(new("acme/widgets"), "d.yaml")}}); err != nil {
		t.Errorf("BETA naming acme/widgets with ACME off was refused: %v", err)
	}
}

// Delivery on with a github_workflow source that names no repository needs a default repository
// (R2); a source that names one does not, and delivery off needs neither.
func TestDeliveryOnNeedsARepositoryForEverySource(t *testing.T) {
	pool, ctx := settingsStore(t)
	_, err := save(t, ctx, pool, ProjectSettingsInput{Project: "BARE", Delivery: true, Sources: []SourceInput{workflowSource(nil, "d.yaml")}})
	refusedWith(t, err, CodeDeliveryNoDefaultRepository)
	if _, err := save(t, ctx, pool, ProjectSettingsInput{Project: "BARE", Delivery: false, Sources: []SourceInput{workflowSource(nil, "d.yaml")}}); err != nil {
		t.Errorf("BARE off with a source naming none was refused: %v", err)
	}
	if _, err := save(t, ctx, pool, ProjectSettingsInput{Project: "BARE", Delivery: true, Sources: []SourceInput{workflowSource(new("bare/deploys"), "d.yaml")}}); err != nil {
		t.Errorf("BARE on with a source naming its repository was refused: %v", err)
	}
	_, err = save(t, ctx, pool, ProjectSettingsInput{Project: "BARE", Delivery: true})
	refusedWith(t, err, CodeDeliverySettingsInput)
}

// A save keeps a source whose repository and rule are unchanged, with its id, its bookkeeping and
// its repository's reconcile progress, and clears the progress of a repository whose rule moved.
func TestASaveKeepsAnUnchangedSourceAndClearsAChangedOnesProgress(t *testing.T) {
	pool, ctx := settingsStore(t)
	first, err := save(t, ctx, pool, ProjectSettingsInput{Project: "ACME", Delivery: true,
		Sources: []SourceInput{workflowSource(new("acme/deploys"), "deploy.yaml"), workflowSource(nil, "release.yaml")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update project_deploy_sources set last_event_at = '2026-11-22T10:00:00Z' where project_key = 'ACME'`); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"acme/deploys", "acme/widgets"} {
		workflow := map[string]string{"acme/deploys": "deploy.yaml", "acme/widgets": "release.yaml"}[repo]
		if err := RecordReconcileProgress(ctx, pool, repoStep("runs/deploy", repo), repo+" "+workflow, time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}
	second, err := save(t, ctx, pool, ProjectSettingsInput{Project: "ACME", Delivery: true,
		Sources: []SourceInput{workflowSource(nil, "release-v2.yaml"), workflowSource(new("acme/deploys"), "deploy.yaml")}})
	if err != nil {
		t.Fatal(err)
	}
	if second.Sources[1].ID != first.Sources[0].ID || second.Sources[1].LastEventAt == nil {
		t.Errorf("the unchanged source is %+v, want the stored row %s with its bookkeeping", second.Sources[1], first.Sources[0].ID)
	}
	if second.Sources[0].ID == first.Sources[1].ID || second.Sources[0].LastEventAt != nil {
		t.Errorf("the changed source kept the old row %+v", second.Sources[0])
	}
	kept, err := ReconcileProgressThrough(ctx, pool, repoStep("runs/deploy", "acme/deploys"), "acme/deploys deploy.yaml")
	if err != nil || kept.IsZero() {
		t.Errorf("the unchanged repository's progress = %v (%v), want it kept", kept, err)
	}
	cleared, err := ReconcileProgressThrough(ctx, pool, repoStep("runs/deploy", "acme/widgets"), "acme/widgets release.yaml")
	if err != nil || !cleared.IsZero() {
		t.Errorf("the changed repository's progress = %v (%v), want it cleared", cleared, err)
	}
}

// A source is reconciled through the earliest of its repository's two run listings and of every
// installation's merged-pull-request search, and through nothing until each listing has a row and
// every installation's search row was recorded under the search's current scope: a reader waiting
// for the import does not read a repository whose pull-request checks have not been listed yet, nor
// one whose pull requests the search has not stored under the population and the source
// repositories in force.
func TestReconciledThroughWaitsForEveryStepOfTheRepository(t *testing.T) {
	pool, ctx := settingsStore(t)
	saved, err := save(t, ctx, pool, ProjectSettingsInput{Project: "ACME", Delivery: true, PopulationAuthors: []string{"octocat"},
		Sources: []SourceInput{{Kind: SourceKindGitHubWorkflow, Repo: new("acme/deploys"), DeployWorkflowPath: new("deploy.yaml"),
			ProductionJobNames: []string{"production"}, PRChecksWorkflowPath: new("ci.yaml")}}})
	if err != nil {
		t.Fatal(err)
	}
	rule := saved.Rules()[0]
	rules, err := ReadSourceRules(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if through, err := ReconciledThrough(ctx, pool, rule, rules); err != nil || through != nil {
		t.Fatalf("with no progress: %v (%v), want nil", through, err)
	}
	deployed, checked, searched := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 22, 7, 0, 0, 0, time.UTC)
	if err := RecordReconcileProgress(ctx, pool, runsStep(DeliveryRunKindDeploy, rule.DeployRepo), "acme/deploys deploy.yaml", deployed); err != nil {
		t.Fatal(err)
	}
	if through, err := ReconciledThrough(ctx, pool, rule, rules); err != nil || through != nil {
		t.Fatalf("with the deploy listing alone: %v (%v), want nil", through, err)
	}
	if err := RecordReconcileProgress(ctx, pool, runsStep(DeliveryRunKindPRChecks, rule.DeployRepo), "acme/deploys ci.yaml", checked); err != nil {
		t.Fatal(err)
	}
	if through, err := ReconciledThrough(ctx, pool, rule, rules); err != nil || through != nil {
		t.Fatalf("with both listings and no search: %v (%v), want nil", through, err)
	}
	current := searchProgressScope(rules)
	if err := RecordReconcileProgress(ctx, pool, mergedPullRequestsStep(1), current, searched); err != nil {
		t.Fatal(err)
	}
	if err := RecordReconcileProgress(ctx, pool, mergedPullRequestsStep(2), "authors=octocat excluded= deploy=acme/other", deployed); err != nil {
		t.Fatal(err)
	}
	if through, err := ReconciledThrough(ctx, pool, rule, rules); err != nil || through != nil {
		t.Fatalf("with one installation's search under an old scope: %v (%v), want nil", through, err)
	}
	if err := RecordReconcileProgress(ctx, pool, mergedPullRequestsStep(2), current, deployed); err != nil {
		t.Fatal(err)
	}
	if through, err := ReconciledThrough(ctx, pool, rule, rules); err != nil || through == nil || !through.Equal(searched) {
		t.Fatalf("with both listings and every search: %v (%v), want the earliest, %v", through, err, searched)
	}
}

// A source whose pull-request checks run in its deploy workflow has one listing, which the
// reconcile records under the deploy step alone, so that row and the search's are how far it is
// reconciled.
func TestReconciledThroughOfOneWorkflowWaitsForItsOneListing(t *testing.T) {
	pool, ctx := settingsStore(t)
	rule := DeliverySettings{DeployRepo: "acme/one", DeployWorkflowPath: "ci.yaml", PRChecksWorkflowPath: "ci.yaml"}
	listed := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	if err := RecordReconcileProgress(ctx, pool, runsStep(DeliveryRunKindDeploy, rule.DeployRepo), "acme/one ci.yaml", listed); err != nil {
		t.Fatal(err)
	}
	rules, err := ReadSourceRules(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordReconcileProgress(ctx, pool, mergedPullRequestsStep(1), searchProgressScope(rules), listed.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if through, err := ReconciledThrough(ctx, pool, rule, rules); err != nil || through == nil || !through.Equal(listed) {
		t.Fatalf("one workflow listed through %v: %v (%v)", listed, through, err)
	}
}

// A source repository the rename pass renames (store.RewriteRenamedRepositories) keeps its
// progress (dispatch://INFRA-978, D3): the reconcile reads its run steps and the search's progress
// under the names and the scopes it writes now, so the renamed source is reconciled through what it
// was before the rename, as is the project's other source, and the reconcile imports nothing again.
func TestARenamedSourceRepositoryKeepsItsProgress(t *testing.T) {
	pool, ctx := settingsStore(t)
	if _, err := save(t, ctx, pool, ProjectSettingsInput{Project: "ACME", Delivery: true, PopulationAuthors: []string{"octocat"},
		ExcludedRepos: []string{"acme/playground"}, Sources: []SourceInput{workflowSource(nil, "deploy.yaml"), workflowSource(new("acme/old"), "ship.yaml")}}); err != nil {
		t.Fatal(err)
	}
	rules, err := ReadSourceRules(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	through := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, rule := range rules.All() {
		for kind, path := range map[DeliveryRunKind]string{DeliveryRunKindDeploy: rule.DeployWorkflowPath, DeliveryRunKindPRChecks: rule.PRChecksWorkflowPath} {
			if err := RecordReconcileProgress(ctx, pool, runsStep(kind, rule.DeployRepo), runsProgressScope(rule.DeployRepo, path), through); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := RecordReconcileProgress(ctx, pool, mergedPullRequestsStep(1), searchProgressScope(rules), through); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into repo_projects (repo, project, created_by, renamed_to)
		values ('acme/old', 'ACME', '{"kind":"user","id":"alice"}', 'acme/new')`); err != nil {
		t.Fatal(err)
	}
	pairs, err := store.ReadRepoRenames(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if pass := store.RewriteRenamedRepositories(ctx, pool, pairs); pass.Err() != nil {
		t.Fatalf("the rename pass = %+v: %v", pass, pass.Err())
	}

	renamed, err := ReadSourceRules(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := renamed.For("acme/old"); ok {
		t.Fatalf("after the rename a source still reads acme/old: %+v", renamed.All())
	}
	for _, repo := range []string{"acme/new", "acme/widgets"} {
		rule, ok := renamed.For(repo)
		if !ok {
			t.Fatalf("after the rename no source reads %s: %+v", repo, renamed.All())
		}
		if got, err := ReconciledThrough(ctx, pool, rule, renamed); err != nil || got == nil || !got.Equal(through) {
			t.Errorf("after the rename %s is reconciled through %v (%v), want %v as before it", repo, got, err, through)
		}
	}
}

// A project with delivery off and no default repository may hold a github_workflow source that
// names no repository, and a second one is refused, naming it: the two would read one
// repository, the default the project does not hold, which the one-repository-per-source index
// holds as one. Nothing is written.
func TestASecondSourceNamingNoRepositoryOfAProjectWithNoDefaultIsRefused(t *testing.T) {
	pool, ctx := settingsStore(t)
	_, err := save(t, ctx, pool, ProjectSettingsInput{Project: "BARE", Delivery: false,
		Sources: []SourceInput{workflowSource(nil, "a.yaml"), workflowSource(new("bare/deploys"), "b.yaml"), workflowSource(nil, "c.yaml")}})
	if refusal := refusedWith(t, err, CodeDeliverySettingsInput); refusal.Source == nil || *refusal.Source != 2 || refusal.Field != "repo" {
		t.Errorf("the refusal %+v names source %v and field %q, want source 2's repo", refusal, refusal.Source, refusal.Field)
	}
	if _, err := GetProjectSettings(ctx, pool, "BARE"); !errors.Is(err, ErrNoSettings) {
		t.Errorf("after the refused save BARE's settings read %v, want ErrNoSettings", err)
	}
}

// A project's first failure day reads back as the day saved, midnight UTC, whatever the session's
// time zone: east of UTC as at UTC.
func TestTheFirstFailureDayReadsBackInEveryTimeZone(t *testing.T) {
	pool, ctx := settingsStore(t)
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := save(t, ctx, pool, ProjectSettingsInput{Project: "ACME", Delivery: true, Sources: []SourceInput{workflowSource(nil, "d.yaml")},
		FailureSources: []string{FailureSourceReverts}, FailuresMeasuredSince: &since}); err != nil {
		t.Fatalf("save ACME: %v", err)
	}
	for _, zone := range []string{"UTC", "Asia/Tokyo"} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `select set_config('timezone', $1, true)`, zone); err != nil {
			t.Fatalf("set the time zone %s: %v", zone, err)
		}
		one, err := GetProjectSettings(ctx, tx, "ACME")
		if err != nil {
			t.Fatal(err)
		}
		all, err := ListProjectSettings(ctx, tx)
		if err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback(ctx)
		for _, read := range []ProjectSettings{one, all[0]} {
			if read.FailuresMeasuredSince == nil || !read.FailuresMeasuredSince.Equal(since) {
				t.Errorf("in the time zone %s ACME's first failure day reads %v, want %s", zone, read.FailuresMeasuredSince, since.Format(time.RFC3339))
			}
		}
	}
}

// sameCodeGroups reports whether a and b hold the same groups in the same order.
func sameCodeGroups(a, b []CodeGroup) bool {
	return slices.EqualFunc(a, b, func(x, y CodeGroup) bool {
		return x.Name == y.Name && slices.Equal(x.BranchPrefixes, y.BranchPrefixes) && slices.Equal(x.AuthorLogins, y.AuthorLogins)
	})
}

// A save that changes a project's code groups and nothing else is a change of its settings: the
// trigger of migration 20261123010254 compares every column but the bookkeeping, code_groups
// among them, so the save notifies SettingsChannel with the project key, which is what wakes a
// reader of the groups (dispatch://INFRA-3206). A save whose input leaves the groups out
// (CodeGroups nil) keeps them, moves nothing else and announces nothing.
//
// Postgres hands a listener the notifications of committed transactions in commit order, so a
// sentinel sent after a save has committed is the next notification unless the save announced one
// first.
func TestAChangeOfCodeGroupsAloneNotifiesTheProject(t *testing.T) {
	pool, ctx := settingsStore(t)
	groups := []CodeGroup{{Name: "agents", BranchPrefixes: []string{"bots/"}, AuthorLogins: []string{"octo-bot[bot]"}}}
	input := ProjectSettingsInput{Project: "ACME", Delivery: true, Sources: []SourceInput{workflowSource(nil, "d.yaml")}, CodeGroups: &groups}
	if _, err := save(t, ctx, pool, input); err != nil {
		t.Fatalf("save ACME with one code group: %v", err)
	}
	listener := listenConn(t, ctx, pool, SettingsChannel)

	input.CodeGroups = nil
	kept, err := save(t, ctx, pool, input)
	if err != nil {
		t.Fatalf("save ACME with no code groups named: %v", err)
	}
	if !sameCodeGroups(kept.CodeGroups, groups) {
		t.Fatalf("a save that names no code groups left %+v, want %+v kept", kept.CodeGroups, groups)
	}
	const sentinel = "sentinel"
	if _, err := pool.Exec(ctx, `select pg_notify($1, $2)`, SettingsChannel, sentinel); err != nil {
		t.Fatalf("send the sentinel on %s: %v", SettingsChannel, err)
	}
	if notification := notified(t, listener); notification.Payload != sentinel {
		t.Fatalf("a save that kept every setting announced %q on %s before the sentinel, want nothing", notification.Payload, SettingsChannel)
	}

	regrouped := append(slices.Clone(groups), CodeGroup{Name: "everyone", BranchPrefixes: []string{}, AuthorLogins: []string{}})
	input.CodeGroups = &regrouped
	saved, err := save(t, ctx, pool, input)
	if err != nil {
		t.Fatalf("save ACME with a second code group: %v", err)
	}
	if !sameCodeGroups(saved.CodeGroups, regrouped) {
		t.Fatalf("the save stored code groups %+v, want %+v", saved.CodeGroups, regrouped)
	}
	if notification := notified(t, listener); notification.Payload != "ACME" {
		t.Errorf("a save that changed the code groups alone announced %q on %s, want the project ACME", notification.Payload, SettingsChannel)
	}
}
