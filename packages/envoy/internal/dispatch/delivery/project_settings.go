package delivery

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The kinds of deploy source a project may hold (dispatch://INFRA-978, R1): a deploy workflow's
// runs in one repository, or the deploys posted to the record of production events.
const (
	SourceKindGitHubWorkflow = "github_workflow"
	SourceKindEvents         = "events"
)

// The failure sources a project may turn on (R7, R9), each the signals of the record it counts.
const (
	FailureSourceCheckResults  = "check_results"
	FailureSourceReverts       = "reverts"
	FailureSourceIncidentLabel = "incident_label"
	FailureSourceEvents        = "events"
)

// FailureSources is every failure source, in the order the editor lists them.
var FailureSources = []string{FailureSourceCheckResults, FailureSourceReverts, FailureSourceIncidentLabel, FailureSourceEvents}

// SourceRule is one deploy source's rule (dispatch://INFRA-978, R1): its kind, the repository it
// names (nil for the project's default repository), its deploy workflow, production jobs and apply
// step, its staging jobs, and its pull-request checks workflow and roll-up job. A stored source
// holds one (DeploySource), a save names one for each source (SourceInput), and the settings
// route's body is one for each source (api's projectDeploySourceInput), so each field is declared
// once: a save that leaves its sources out takes each stored rule whole, and a save keeps a stored
// row whose rule is its input's (sameRule). The route's body must name the kind, which its
// `required` tag tells the emitter; every answer holds it.
type SourceRule struct {
	Kind                    string   `json:"kind" required:"true" enum:"github_workflow,events"`
	Repo                    *string  `json:"repo"`
	DeployWorkflowPath      *string  `json:"deploy_workflow_path"`
	ProductionJobNames      []string `json:"production_job_names"`
	ProductionApplyStepName *string  `json:"production_apply_step_name"`
	StagingE2EJobName       *string  `json:"staging_e2e_job_name"`
	StagingApplyJobName     *string  `json:"staging_apply_job_name"`
	PRChecksWorkflowPath    *string  `json:"pr_checks_workflow_path"`
	PRChecksRollupJobName   *string  `json:"pr_checks_rollup_job_name"`
}

// DeploySource is one row of project_deploy_sources (migration 20261123010254): its id, its rule,
// and the bookkeeping the intake and the reconcile write for its repository.
type DeploySource struct {
	ID string `json:"id"`
	SourceRule
	LastEventAt     *time.Time `json:"last_event_at"`
	LastReconcileAt *time.Time `json:"last_reconcile_at"`
	LastError       *string    `json:"last_error"`
}

// ProjectSettings is one project's row of project_delivery_settings, its deploy sources in their
// order, and the project's default repository read beside them.
type ProjectSettings struct {
	Project               string         `json:"project"`
	Delivery              bool           `json:"delivery"`
	DefaultRepo           *string        `json:"default_repo"`
	Sources               []DeploySource `json:"sources"`
	PopulationAuthors     []string       `json:"population_authors"`
	ExcludedRepos         []string       `json:"excluded_repos"`
	FailureSources        []string       `json:"failure_sources"`
	FailuresMeasuredSince *time.Time     `json:"failures_measured_since"`
	UpdatedBy             model.Actor    `json:"updated_by"`
	UpdatedAt             time.Time      `json:"updated_at"`
}

// SourceRepoOf is the repository a github_workflow source reads (R2): the one its rule names, else
// the project's default repository, and false where it names none and the project has none. Every
// reader that names a source's repository asks here.
func SourceRepoOf(source SourceRule, defaultRepo *string) (string, bool) {
	if source.Kind != SourceKindGitHubWorkflow {
		return "", false
	}
	if source.Repo != nil {
		return *source.Repo, true
	}
	if defaultRepo == nil || *defaultRepo == "" {
		return "", false
	}
	return *defaultRepo, true
}

// Rules is the deploy rule of each github_workflow source of the project whose repository resolves,
// in the shape every reader of a repository's runs takes (DeliverySettings), and none where delivery
// is off.
func (s ProjectSettings) Rules() []DeliverySettings {
	if !s.Delivery {
		return nil
	}
	var rules []DeliverySettings
	for _, source := range s.Sources {
		repo, ok := SourceRepoOf(source.SourceRule, s.DefaultRepo)
		if !ok {
			continue
		}
		rules = append(rules, DeliverySettings{
			Project:                 s.Project,
			DeployRepo:              repo,
			DeployWorkflowPath:      deref(source.DeployWorkflowPath),
			ProductionJobNames:      source.ProductionJobNames,
			ProductionApplyStepName: source.ProductionApplyStepName,
			PRChecksWorkflowPath:    deref(source.PRChecksWorkflowPath),
			PopulationAuthors:       s.PopulationAuthors,
			ExcludedRepos:           s.ExcludedRepos,
			StagingE2EJobName:       source.StagingE2EJobName,
			StagingApplyJobName:     source.StagingApplyJobName,
			PRChecksRollupJobName:   source.PRChecksRollupJobName,
			LastEventAt:             source.LastEventAt,
			LastReconcileAt:         source.LastReconcileAt,
			LastError:               source.LastError,
			UpdatedBy:               s.UpdatedBy,
			UpdatedAt:               s.UpdatedAt,
		})
	}
	return rules
}

// DeployRules is the rule of every github_workflow source of every delivery-on project among
// projects, ordered by repository. A repository is the source of one delivery-on project at most
// (PutProjectSettings refuses a second), so each repository appears once.
func DeployRules(projects []ProjectSettings) []DeliverySettings {
	var rules []DeliverySettings
	for _, project := range projects {
		rules = append(rules, project.Rules()...)
	}
	slices.SortFunc(rules, func(a, b DeliverySettings) int { return cmp.Compare(a.DeployRepo, b.DeployRepo) })
	return rules
}

// Population is LEGION-294's population rule as a reader of a merged pull request applies it: the
// authors whose merges count and the repositories whose merges do not. With per-project settings it
// is the union of every delivery-on project's authors and excluded repositories (dispatch://INFRA-978,
// D1): a pull request is stored once, whichever project's rule admits it, and an excluded repository
// is excluded for every project. Both lists are sorted and hold each value once.
type Population struct {
	Authors       []string
	ExcludedRepos []string
}

// Admits reports whether a merged pull request of repo by author is in the population: author is
// one of Authors exactly, as GitHub writes a login ([bot] suffix included), and repo is none of
// ExcludedRepos in any case. The window and the task-label rule are IsPopulationPR's.
func (p Population) Admits(repo, author string) bool {
	if slices.ContainsFunc(p.ExcludedRepos, func(excluded string) bool { return strings.EqualFold(excluded, repo) }) {
		return false
	}
	return slices.Contains(p.Authors, author)
}

// SourceRules is what one read of every project's delivery settings gives a reader of runs and pull
// requests: the rule of each source repository (DeployRules), found by its repository (For), and the
// population (Population). The settings cache holds one for the intakes, and each reconcile pass
// reads its own (ReadSourceRules).
type SourceRules struct {
	rules      []DeliverySettings
	byRepo     map[string]int
	population Population
	// defaults is the default repository, as this read found it, of each delivery-on project with a
	// github_workflow source that names no repository: a move of one moves that source's repository
	// (SourceRepoOf), so the settings cache reads the rules again when one moved (defaultsMoved).
	defaults map[string]*string
	// searchScope is the merged pull request search's progress scope of this read
	// (searchProgressScope).
	searchScope string
}

// NewSourceRules is the rules of projects: every delivery-on project's github_workflow sources
// (DeployRules), and the union of the delivery-on projects' populations. The search's progress
// scope lists the populations and the source repositories in the order of projects
// (ListProjectSettings answers them by key) and of each project's own lists and sources, each value
// once, so a rename pass that writes a repository's new name in its place leaves the scope the one
// this read makes of the renamed settings (searchProgressScope).
func NewSourceRules(projects []ProjectSettings) *SourceRules {
	var authors, excluded, repos []string
	defaults := map[string]*string{}
	for _, project := range projects {
		if !project.Delivery {
			continue
		}
		authors = append(authors, project.PopulationAuthors...)
		excluded = append(excluded, project.ExcludedRepos...)
		for _, rule := range project.Rules() {
			repos = append(repos, strings.ToLower(rule.DeployRepo))
		}
		for _, source := range project.Sources {
			if source.Kind == SourceKindGitHubWorkflow && source.Repo == nil {
				defaults[project.Project] = project.DefaultRepo
			}
		}
	}
	rules := newSourceRules(DeployRules(projects), Population{Authors: sortedSet(authors), ExcludedRepos: sortedSet(excluded)}, defaults)
	rules.searchScope = searchProgressScopeOf(appendUnique(nil, authors...), appendUnique(nil, excluded...), appendUnique(nil, repos...))
	return rules
}

// newSourceRules is the SourceRules of rules, ordered by repository, and population.
func newSourceRules(rules []DeliverySettings, population Population, defaults map[string]*string) *SourceRules {
	byRepo := make(map[string]int, len(rules))
	for i, rule := range rules {
		byRepo[RuleKey(rule.DeployRepo)] = i
	}
	return &SourceRules{rules: rules, byRepo: byRepo, population: population, defaults: defaults}
}

// ReadSourceRules reads every project's delivery settings in q (ListProjectSettings) and answers
// their SourceRules.
func ReadSourceRules(ctx context.Context, q Querier) (*SourceRules, error) {
	projects, err := ListProjectSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	return NewSourceRules(projects), nil
}

// RuleKey is the key a repository's rule is found by (For): its name in lower case.
func RuleKey(repo string) string {
	return strings.ToLower(repo)
}

// For is the rule of repo, in any case, and false where no delivery-on project's source reads it.
// A nil SourceRules holds no rule.
func (r *SourceRules) For(repo string) (DeliverySettings, bool) {
	if r == nil {
		return DeliverySettings{}, false
	}
	i, ok := r.byRepo[RuleKey(repo)]
	if !ok {
		return DeliverySettings{}, false
	}
	return r.rules[i], true
}

// All is every source repository's rule, ordered by repository.
func (r *SourceRules) All() []DeliverySettings {
	if r == nil {
		return nil
	}
	return r.rules
}

// Population is the union of the delivery-on projects' populations.
func (r *SourceRules) Population() Population {
	if r == nil {
		return Population{}
	}
	return r.population
}

// DefaultRepo is the default repository, as this read found it, of project, where project is a
// delivery-on project holding a github_workflow source that names no repository: the repository
// that source reads. It is false for every other project, and nil where the project has none.
func (r *SourceRules) DefaultRepo(project string) (*string, bool) {
	if r == nil {
		return nil, false
	}
	repo, held := r.defaults[project]
	return repo, held
}

// sortedSet is values sorted, each once.
func sortedSet(values []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(values)))
}

// lowerSortedSet is repos in lower case, sorted, each once.
func lowerSortedSet(repos []string) []string {
	lower := make([]string, len(repos))
	for i, repo := range repos {
		lower[i] = strings.ToLower(repo)
	}
	return sortedSet(lower)
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// projectSettingsColumns reads failures_measured_since as the date it is, which pgx answers as
// midnight UTC of that day whatever the session's time zone.
const projectSettingsColumns = `s.project_key, s.delivery, p.default_repo, s.population_authors, s.excluded_repos,
	s.failure_sources, s.failures_measured_since, s.updated_by, s.updated_at`

const deploySourceColumns = `id::text, kind, repo, deploy_workflow_path, production_job_names, production_apply_step_name,
	staging_e2e_job_name, staging_apply_job_name, pr_checks_workflow_path, pr_checks_rollup_job_name,
	last_event_at, last_reconcile_at, last_error`

func scanProjectSettings(row pgx.Row) (ProjectSettings, error) {
	var settings ProjectSettings
	var updatedBy []byte
	if err := row.Scan(&settings.Project, &settings.Delivery, &settings.DefaultRepo, &settings.PopulationAuthors,
		&settings.ExcludedRepos, &settings.FailureSources, &settings.FailuresMeasuredSince, &updatedBy, &settings.UpdatedAt); err != nil {
		return ProjectSettings{}, err
	}
	if err := json.Unmarshal(updatedBy, &settings.UpdatedBy); err != nil {
		return ProjectSettings{}, fmt.Errorf("decode the delivery settings author of %s: %w", settings.Project, err)
	}
	return settings, nil
}

// readSources reads the deploy sources of the projects whose settings are in byProject, in order.
func readSources(ctx context.Context, q Querier, byProject map[string]*ProjectSettings) error {
	keys := make([]string, 0, len(byProject))
	for key := range byProject {
		keys = append(keys, key)
	}
	rows, err := q.Query(ctx, `select project_key, `+deploySourceColumns+` from project_deploy_sources
		where project_key = any($1) order by project_key, position`, keys)
	if err != nil {
		return fmt.Errorf("read the deploy sources: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		var source DeploySource
		if err := rows.Scan(append([]any{&project}, sourceDestinations(&source)...)...); err != nil {
			return fmt.Errorf("read the deploy sources: %w", err)
		}
		byProject[project].Sources = append(byProject[project].Sources, source)
	}
	return rows.Err()
}

func sourceDestinations(source *DeploySource) []any {
	return []any{&source.ID, &source.Kind, &source.Repo, &source.DeployWorkflowPath, &source.ProductionJobNames,
		&source.ProductionApplyStepName, &source.StagingE2EJobName, &source.StagingApplyJobName,
		&source.PRChecksWorkflowPath, &source.PRChecksRollupJobName, &source.LastEventAt, &source.LastReconcileAt, &source.LastError}
}

// GetProjectSettings reads project's delivery settings and sources in q, or ErrNoSettings when the
// project has no row. A caller that reads them beside another table holds q as one transaction.
func GetProjectSettings(ctx context.Context, q Querier, project string) (ProjectSettings, error) {
	settings, err := scanProjectSettings(q.QueryRow(ctx, `
		select `+projectSettingsColumns+`
		from project_delivery_settings s join projects p on p.key = s.project_key
		where s.project_key = $1
	`, project))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectSettings{}, ErrNoSettings
	}
	if err != nil {
		return ProjectSettings{}, fmt.Errorf("read the delivery settings of %s: %w", project, err)
	}
	if err := readSources(ctx, q, map[string]*ProjectSettings{project: &settings}); err != nil {
		return ProjectSettings{}, err
	}
	return settings, nil
}

// ListProjectSettings reads every project's delivery settings and sources in q, by project key.
func ListProjectSettings(ctx context.Context, q Querier) ([]ProjectSettings, error) {
	rows, err := q.Query(ctx, `
		select `+projectSettingsColumns+`
		from project_delivery_settings s join projects p on p.key = s.project_key
		order by s.project_key
	`)
	if err != nil {
		return nil, fmt.Errorf("read the projects' delivery settings: %w", err)
	}
	var all []ProjectSettings
	for rows.Next() {
		settings, err := scanProjectSettings(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the projects' delivery settings: %w", err)
		}
		all = append(all, settings)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the projects' delivery settings: %w", err)
	}
	byProject := make(map[string]*ProjectSettings, len(all))
	for i := range all {
		byProject[all[i].Project] = &all[i]
	}
	if len(byProject) > 0 {
		if err := readSources(ctx, q, byProject); err != nil {
			return nil, err
		}
	}
	return all, nil
}

// maxSources is the most deploy sources one project holds: a bound the editor and the settings
// route state, so a save is refused with the reason rather than written past it.
const maxSources = 50

// SourceInput is one deploy source of a save, as the editor sends it: its rule.
type SourceInput = SourceRule

// ProjectSettingsInput is one save of a project's delivery settings: every field the editor sends.
type ProjectSettingsInput struct {
	Project               string
	Delivery              bool
	Sources               []SourceInput
	PopulationAuthors     []string
	ExcludedRepos         []string
	FailureSources        []string
	FailuresMeasuredSince *time.Time
}

// SettingsError is a save the delivery rules refuse (D1): its code, its reason, the field and the
// source's index where the refusal is about one, and, where it is about a repository two sources
// read, the project whose source reads it: this project for two of its own sources, and another
// delivery-on project only where the caller sees that project (sharedRepository).
type SettingsError struct {
	Code    string `json:"code"`
	Message string `json:"error"`
	Field   string `json:"field,omitempty"`
	Source  *int   `json:"source,omitempty"`
	Project string `json:"project,omitempty"`
}

func (e *SettingsError) Error() string { return e.Code + ": " + e.Message }

// ErrProjectNotFound is a project no row holds: a save of it, and a read of its default repository
// (DefaultRepo).
var ErrProjectNotFound = errors.New("no such project")

// DefaultRepo reads project's default repository in q (projects.default_repo), nil for none, or
// ErrProjectNotFound where no project has the key. Every reader of the default alone asks here; a
// reader of the settings reads it beside them (GetProjectSettings, StoredSettings).
func DefaultRepo(ctx context.Context, q Querier, project string) (*string, error) {
	var repo *string
	if err := q.QueryRow(ctx, `select default_repo from projects where key = $1`, project).Scan(&repo); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrProjectNotFound
		}
		return nil, fmt.Errorf("read the default repository of %s: %w", project, err)
	}
	return repo, nil
}

// StoredSettings is what a save of project's delivery settings writes over, read in q: its stored
// settings and sources (GetProjectSettings), or, where it has saved none, its default repository
// (DefaultRepo) with delivery off and no source; ErrProjectNotFound where no project has the key.
// A save reads it once under the project's lock (LockProject) and hands it to PutProjectSettings,
// which reads neither again.
func StoredSettings(ctx context.Context, q Querier, project string) (ProjectSettings, error) {
	settings, err := GetProjectSettings(ctx, q, project)
	if !errors.Is(err, ErrNoSettings) {
		return settings, err
	}
	defaultRepo, err := DefaultRepo(ctx, q, project)
	if err != nil {
		return ProjectSettings{}, err
	}
	return ProjectSettings{Project: project, DefaultRepo: defaultRepo}, nil
}

// The codes of a refused save. DELIVERY_SETTINGS_ACCESS is the settings route's own, for a source
// whose repository the GitHub App cannot read, which the route proves before it saves.
const (
	CodeDeliveryRepositoryShared    = "DELIVERY_REPOSITORY_SHARED"
	CodeDeliveryNoDefaultRepository = "DELIVERY_NO_DEFAULT_REPOSITORY"
	CodeDeliveryNeedsRepository     = "DELIVERY_NEEDS_REPOSITORY"
	CodeDeliverySettingsInput       = "DELIVERY_SETTINGS_INPUT"
	CodeDeliverySettingsAccess      = "DELIVERY_SETTINGS_ACCESS"
)

// LockProject takes the transaction-scoped lock of one project's delivery settings, so two saves of
// one project run one after the other: the second reads the sources the first wrote. A path that
// takes it takes it before any repository's lock (LockRepositories), so no two paths wait on each
// other.
func LockProject(ctx context.Context, tx pgx.Tx, project string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(6902, hashtext($1))`, project); err != nil {
		return fmt.Errorf("lock the delivery settings of %s: %w", project, err)
	}
	return nil
}

// LockRepositories takes the transaction-scoped lock of each repository's delivery ownership
// (store.LockDeployRepository), in name order so two transactions taking two of them cannot wait
// on each other: a save, a default-repository write and the rename pass's line of a default
// repository, each of which can make one repository the source of a delivery-on project, see each
// other's commit. Its first key, 6903, is its own: 6900 is the change log's lock
// (lockChangesStatement), 6901 the Legion observations' (legion.ObservationsLockKey) and 6902 a
// project's (LockProject), so no repository's name can hash onto another family's lock.
func LockRepositories(ctx context.Context, tx pgx.Tx, repos []string) error {
	for _, repo := range lowerSortedSet(repos) {
		if _, err := tx.Exec(ctx, store.LockDeployRepository, repo); err != nil {
			return fmt.Errorf("lock the deploy repository %s: %w", repo, err)
		}
	}
	return nil
}

// RepositoryOwner is the delivery-on project other than project whose github_workflow source reads
// repo, read in tx, and false where none does. The caller holds repo's lock (LockRepositories).
func RepositoryOwner(ctx context.Context, tx pgx.Tx, repo, project string) (string, bool, error) {
	var other string
	err := tx.QueryRow(ctx, `
		select src.project_key from project_deploy_sources src
		where `+sourceOfRepo+` and src.project_key <> $2
		order by src.project_key limit 1
	`, repo, project).Scan(&other)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the project that deploys %s: %w", repo, err)
	}
	return other, true, nil
}

// sharedRepository is the refusal DELIVERY_REPOSITORY_SHARED of repo for the source of index,
// where a source of other, a delivery-on project, reads repo. It names other, in its reason and in
// Project, only where viewer sees it (All, or other among Sees: the rule access's
// refuseSharedSlackChannel names a private project by), the viewer the request resolved before its
// transaction opened; to any other caller it says that a project it cannot see reads repo, so the
// refusal tells it neither the key of a project it cannot see nor that the project deploys from
// repo. A nil viewer sees no project.
func sharedRepository(repo, other string, index *int, viewer *model.Viewer) *SettingsError {
	refusal := &SettingsError{
		Code:    CodeDeliveryRepositoryShared,
		Message: fmt.Sprintf("%s is already a deploy source of a project you cannot see, which has delivery on; a repository is the deploy source of one project at most", repo),
		Field:   "repo",
		Source:  index,
	}
	if viewer != nil && (viewer.All || slices.Contains(viewer.Sees, other)) {
		refusal.Message = fmt.Sprintf("%s is already a deploy source of project %s, which has delivery on; a repository is the deploy source of one project at most", repo, other)
		refusal.Project = other
	}
	return refusal
}

// checkSources holds sources, the deploy sources of stored's project as a write leaves them, each
// that names no repository reading defaultRepo (SourceRepoOf), to the delivery rules, under the
// lock of every repository they read and every repository stored's sources read
// (LockRepositories), which it takes: a save of the settings (PutProjectSettings) and a move of the
// project's default repository (MoveDefaultRepository) both check here, so R2's and D1's rules are
// stated once. Whether delivery is on or off, two github_workflow sources may not read one
// repository, and two that name none read one, defaultRepo, whether there is one or not: the
// refusal of the second has the code duplicate, each caller's own, and names it and the project.
// With delivery on (on), a github_workflow source that reads no repository is refused
// DELIVERY_NO_DEFAULT_REPOSITORY, and one whose repository a source of another delivery-on
// project reads (RepositoryOwner) DELIVERY_REPOSITORY_SHARED, naming that project where viewer
// sees it (sharedRepository). It answers a refusal as a *SettingsError, and nil where the sources
// keep the rules.
func checkSources(ctx context.Context, tx pgx.Tx, stored ProjectSettings, on bool, sources []SourceRule, defaultRepo *string, duplicate string, viewer *model.Viewer) error {
	project := stored.Project
	repos := make([]string, len(sources))
	var lock []string
	// unnamed is the first github_workflow source that reads no repository, which a project with
	// delivery off and no default repository keeps: a second is the same repository as far as the
	// one-repository-per-source index is concerned (migration 20261123010254).
	unnamed := -1
	for i, source := range sources {
		if source.Kind != SourceKindGitHubWorkflow {
			continue
		}
		repo, ok := SourceRepoOf(source, defaultRepo)
		if !ok {
			if on {
				return &SettingsError{
					Code:    CodeDeliveryNoDefaultRepository,
					Message: fmt.Sprintf("source %d of project %s names no repository and the project has no default repository: name the source's repository, or set the project's default repository in Settings -> Projects", i, project),
					Field:   "repo",
					Source:  new(i),
				}
			}
			if unnamed >= 0 {
				return &SettingsError{
					Code:    duplicate,
					Message: fmt.Sprintf("sources %d and %d of project %s both name no repository, so both read the project's default repository: a repository is one source of a project", unnamed, i, project),
					Field:   "repo",
					Source:  new(i),
					Project: project,
				}
			}
			unnamed = i
			continue
		}
		for j := range i {
			if repos[j] != "" && strings.EqualFold(repos[j], repo) {
				return &SettingsError{
					Code:    duplicate,
					Message: fmt.Sprintf("sources %d and %d of project %s both read %s: a repository is one source of a project", j, i, project, repo),
					Field:   "repo",
					Source:  new(i),
					Project: project,
				}
			}
		}
		repos[i] = repo
		lock = append(lock, repo)
	}
	for _, source := range stored.Sources {
		if repo, ok := SourceRepoOf(source.SourceRule, stored.DefaultRepo); ok {
			lock = append(lock, repo)
		}
	}
	if err := LockRepositories(ctx, tx, lock); err != nil {
		return err
	}
	if !on {
		return nil
	}
	for i, repo := range repos {
		if repo == "" {
			continue
		}
		other, shared, err := RepositoryOwner(ctx, tx, repo, project)
		if err != nil {
			return err
		}
		if shared {
			return sharedRepository(repo, other, new(i), viewer)
		}
	}
	return nil
}

// PutProjectSettings saves input as its project's delivery settings in tx over stored, what the
// project held as StoredSettings read it in tx under the project's lock (LockProject), which the
// caller holds: a save reads the stored settings and the default repository once, and this reads
// neither again. It answers a refusal or an error alone; a caller that shows the stored settings
// reads them (GetProjectSettings). With delivery on, a github_workflow source that names no
// repository needs the project's default repository (R2, DELIVERY_NO_DEFAULT_REPOSITORY), and no
// repository may be a source of another delivery-on project (D1, DELIVERY_REPOSITORY_SHARED);
// whatever the switch says, two github_workflow sources may not read one repository
// (DELIVERY_SETTINGS_INPUT), and two that name none read one, the project's default repository,
// whether the project has one or not. The checks (checkSources) run under each repository's lock,
// before any write. The save replaces the project's sources: a source whose repository and rule
// are both unchanged keeps its row, its id and its bookkeeping; every other source is written
// afresh, and its repository's reconcile progress is cleared, so the next reconcile imports it
// from the start, as the singleton's save did.
func PutProjectSettings(ctx context.Context, tx pgx.Tx, input ProjectSettingsInput, stored ProjectSettings, actor model.Actor, viewer *model.Viewer) error {
	input.Sources = slices.Clone(input.Sources)
	for i, source := range input.Sources {
		if source.Repo != nil {
			input.Sources[i].Repo = new(strings.ToLower(strings.TrimSpace(*source.Repo)))
		}
	}
	if err := ValidateProjectSettings(input); err != nil {
		return err
	}
	if err := checkSources(ctx, tx, stored, input.Delivery, input.Sources, stored.DefaultRepo, CodeDeliverySettingsInput, viewer); err != nil {
		return err
	}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		return fmt.Errorf("encode the delivery settings author: %w", err)
	}
	var since *string
	if input.FailuresMeasuredSince != nil {
		day := input.FailuresMeasuredSince.UTC().Format(time.DateOnly)
		since = &day
	}
	if _, err := tx.Exec(ctx, `
		insert into project_delivery_settings (project_key, delivery, population_authors, excluded_repos,
			failure_sources, failures_measured_since, updated_by)
		values ($1, $2, $3, $4, $5, $6::date, $7)
		on conflict (project_key) do update set
			delivery = excluded.delivery,
			population_authors = excluded.population_authors,
			excluded_repos = excluded.excluded_repos,
			failure_sources = excluded.failure_sources,
			failures_measured_since = excluded.failures_measured_since,
			updated_by = excluded.updated_by,
			updated_at = now()
	`, input.Project, input.Delivery, nonNil(input.PopulationAuthors), nonNil(input.ExcludedRepos),
		nonNil(input.FailureSources), since, actorJSON); err != nil {
		return fmt.Errorf("save the delivery settings of %s: %w", input.Project, err)
	}
	return replaceSources(ctx, tx, input, stored.Sources, stored.DefaultRepo)
}

// MoveDefaultRepository holds a write of project's default repository to next (nil: a clear) to
// the delivery rules (D1), in tx, which holds the project's owner lock and has not yet written the
// row, and answers the refusal (*SettingsError) where the move would break one. It applies to a
// delivery-on project holding a github_workflow source that names no repository, which reads the
// default; every other project moves its default freely, a project whose every source names its
// own repository among them. It takes the project's delivery lock first (LockProject), so it reads
// the sources a save of the project wrote. It refuses a clear, DELIVERY_NEEDS_REPOSITORY, since the
// source would read no repository, and holds the project's sources under next to the save's rules
// (checkSources, under the lock of every repository they read under the old default and the new,
// taken after the project's lock as the save takes them), where another source of the project that
// names next is refused DELIVERY_REPOSITORY_SHARED. A move it lets through clears the reconcile
// progress of the old and the new repository (ClearRepositoryProgress), as a save that changes a
// source's repository does, so the next reconcile imports the new one's runs from the start; no
// other delivery-on project's source reads either. An unknown project, a project with no settings,
// and a write of the value the row holds are none of its business, and it answers nil.
func MoveDefaultRepository(ctx context.Context, tx pgx.Tx, project string, next *string, viewer *model.Viewer) error {
	if err := LockProject(ctx, tx, project); err != nil {
		return err
	}
	settings, err := GetProjectSettings(ctx, tx, project)
	if errors.Is(err, ErrNoSettings) {
		return nil
	}
	if err != nil {
		return err
	}
	current := settings.DefaultRepo
	if !settings.Delivery || ((current == nil) == (next == nil) && deref(current) == deref(next)) {
		return nil
	}
	defaulting := slices.IndexFunc(settings.Sources, func(source DeploySource) bool {
		return source.Kind == SourceKindGitHubWorkflow && source.Repo == nil
	})
	if defaulting < 0 {
		return nil
	}
	if next == nil {
		return &SettingsError{
			Code: CodeDeliveryNeedsRepository, Field: "repo", Source: new(defaulting), Project: project,
			Message: fmt.Sprintf("project %s has delivery on and its source %d names no repository, so it reads the default repository: name the source's repository in the project's delivery settings, or turn delivery off, before clearing the default", project, defaulting),
		}
	}
	rules := make([]SourceRule, len(settings.Sources))
	for i, source := range settings.Sources {
		rules[i] = source.SourceRule
	}
	if err := checkSources(ctx, tx, settings, true, rules, next, CodeDeliveryRepositoryShared, viewer); err != nil {
		return err
	}
	moved := []string{*next}
	if current != nil {
		moved = append(moved, *current)
	}
	return ClearRepositoryProgress(ctx, tx, moved)
}

// sameRule is whether a stored source and an input are one rule, every field of SourceRule equal,
// so the save keeps the stored row and its bookkeeping. reflect.DeepEqual compares a nil slice with
// an empty one as two values, which no stored rule and no input of a save differ by: the table
// holds a github_workflow source's production jobs as one name or more and an events source's as
// null, and ValidateProjectSettings refuses an input of any other shape.
func sameRule(stored DeploySource, input SourceInput) bool {
	return reflect.DeepEqual(stored.SourceRule, input)
}

// replaceSources makes the project's sources input's, in its order: a stored source with the same
// repository and rule is kept and moved to its new position, every other stored source is deleted,
// and every input without such a match is inserted; the reconcile progress of each repository whose
// rule moved (one gone, one new, one changed) is cleared.
func replaceSources(ctx context.Context, tx pgx.Tx, input ProjectSettingsInput, stored []DeploySource, defaultRepo *string) error {
	kept := make([]string, len(input.Sources))
	used := make([]bool, len(stored))
	for i, source := range input.Sources {
		for j, old := range stored {
			if !used[j] && sameRule(old, source) {
				kept[i], used[j] = old.ID, true
				break
			}
		}
	}
	var moved []string
	for j, old := range stored {
		if used[j] {
			continue
		}
		if _, err := tx.Exec(ctx, `delete from project_deploy_sources where id = $1`, old.ID); err != nil {
			return fmt.Errorf("delete a deploy source of %s: %w", input.Project, err)
		}
		if repo, ok := SourceRepoOf(old.SourceRule, defaultRepo); ok {
			moved = append(moved, repo)
		}
	}
	// A kept row whose position moves steps out of the way first, past every position a project can
	// hold, so the unique (project_key, position) never meets two rows at one position while they take
	// their new places. A kept row already at its new position is not written at all, so a save that
	// changes nothing writes no source and logs no change (the trigger logs every write of a column
	// other than the bookkeeping, position included).
	positions := map[string]int{}
	rows, err := tx.Query(ctx, `select id::text, position from project_deploy_sources where project_key = $1`, input.Project)
	if err != nil {
		return fmt.Errorf("read the positions of the deploy sources of %s: %w", input.Project, err)
	}
	for rows.Next() {
		var id string
		var position int
		if err := rows.Scan(&id, &position); err != nil {
			rows.Close()
			return fmt.Errorf("read the positions of the deploy sources of %s: %w", input.Project, err)
		}
		positions[id] = position
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the positions of the deploy sources of %s: %w", input.Project, err)
	}
	var shifted []string
	for i, id := range kept {
		if id != "" && positions[id] != i {
			shifted = append(shifted, id)
		}
	}
	if len(shifted) > 0 {
		if _, err := tx.Exec(ctx, `update project_deploy_sources set position = position + $2 where id = any($1::uuid[])`, shifted, maxSources); err != nil {
			return fmt.Errorf("reorder the deploy sources of %s: %w", input.Project, err)
		}
	}
	for i, source := range input.Sources {
		if kept[i] != "" {
			if !slices.Contains(shifted, kept[i]) {
				continue
			}
			if _, err := tx.Exec(ctx, `update project_deploy_sources set position = $2 where id = $1`, kept[i], i); err != nil {
				return fmt.Errorf("reorder a deploy source of %s: %w", input.Project, err)
			}
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into project_deploy_sources (project_key, position, kind, repo, deploy_workflow_path,
				production_job_names, production_apply_step_name, staging_e2e_job_name, staging_apply_job_name,
				pr_checks_workflow_path, pr_checks_rollup_job_name)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, input.Project, i, source.Kind, source.Repo, source.DeployWorkflowPath, source.ProductionJobNames,
			source.ProductionApplyStepName, source.StagingE2EJobName, source.StagingApplyJobName,
			source.PRChecksWorkflowPath, source.PRChecksRollupJobName); err != nil {
			return fmt.Errorf("save a deploy source of %s: %w", input.Project, err)
		}
		if repo, ok := SourceRepoOf(source, defaultRepo); ok {
			moved = append(moved, repo)
		}
	}
	return ClearRepositoryProgress(ctx, tx, moved)
}

// repoStep names one repository's row of a run step's progress. delivery_reconcile_progress is
// keyed by step alone (migration 0083), and a row recorded under another scope is replaced, so two
// source repositories under one step name would each restart the other's import every pass: each
// run step's name carries its repository instead (dispatch://INFRA-978, D1). The names without a
// repository are the release before this one's.
func repoStep(step, repo string) string {
	return step + "/" + strings.ToLower(repo)
}

// ClearRepositoryProgress deletes the recorded progress of every run step of each repository
// (repoStep), so the next reconcile imports the repository's runs from the start of the backfill
// window.
func ClearRepositoryProgress(ctx context.Context, tx pgx.Tx, repos []string) error {
	for _, repo := range lowerSortedSet(repos) {
		if _, err := tx.Exec(ctx, `
			delete from delivery_reconcile_progress
			where (starts_with(step, 'runs/') or starts_with(step, 'backfill/')) and right(step, length($1) + 1) = '/' || $1
		`, repo); err != nil {
			return fmt.Errorf("clear the reconcile progress of %s: %w", repo, err)
		}
	}
	return nil
}

// InputRefusal is the DELIVERY_SETTINGS_INPUT refusal of field, of the source of index where it is
// a source's (nil where it is not), its message the field and the reason format and args give.
func InputRefusal(index *int, field, format string, args ...any) *SettingsError {
	return &SettingsError{Code: CodeDeliverySettingsInput, Message: field + ": " + fmt.Sprintf(format, args...), Field: field, Source: index}
}

// ValidateProjectSettings refuses an input no reader could read, naming its field and its source,
// before any statement runs (InputRefusal): the tables' checks hold the same rules (migration
// 20261123010254), but for a job name or a workflow path of white space alone, which they would
// store and no reader could match. PutProjectSettings runs it, and the settings route runs it
// before it asks GitHub anything.
func ValidateProjectSettings(input ProjectSettingsInput) error {
	refuse := InputRefusal
	if input.Delivery && len(input.Sources) == 0 {
		return refuse(nil, "sources", "delivery on needs at least one deploy source")
	}
	if len(input.Sources) > maxSources {
		return refuse(nil, "sources", "a project holds at most %d deploy sources, and this save names %d", maxSources, len(input.Sources))
	}
	events := 0
	for i, source := range input.Sources {
		index := new(i)
		switch source.Kind {
		case SourceKindGitHubWorkflow:
			if source.Repo != nil {
				if _, _, err := refs.SplitGitHubRepo(*source.Repo); err != nil {
					return refuse(index, "repo", "%q is not a GitHub repository owner/name", *source.Repo)
				}
			}
			if strings.TrimSpace(deref(source.DeployWorkflowPath)) == "" {
				return refuse(index, "deploy_workflow_path", "a github_workflow source names its deploy workflow")
			}
			if len(source.ProductionJobNames) == 0 {
				return refuse(index, "production_job_names", "a github_workflow source names at least one production job")
			}
			for _, name := range source.ProductionJobNames {
				if strings.TrimSpace(name) == "" {
					return refuse(index, "production_job_names", "a production job name is empty")
				}
			}
			if strings.TrimSpace(deref(source.PRChecksWorkflowPath)) == "" {
				return refuse(index, "pr_checks_workflow_path", "a github_workflow source names its pull-request checks workflow")
			}
		case SourceKindEvents:
			events++
			if events > 1 {
				return refuse(index, "kind", "a project reads its posted deploys once: one events source at most")
			}
			if source.Repo != nil || source.DeployWorkflowPath != nil || source.ProductionJobNames != nil ||
				source.ProductionApplyStepName != nil || source.StagingE2EJobName != nil || source.StagingApplyJobName != nil ||
				source.PRChecksWorkflowPath != nil || source.PRChecksRollupJobName != nil {
				return refuse(index, "kind", "an events source reads the record of production events and names no repository and no workflow")
			}
		default:
			return refuse(index, "kind", "%q is neither %s nor %s", source.Kind, SourceKindGitHubWorkflow, SourceKindEvents)
		}
	}
	seen := map[string]bool{}
	for _, source := range input.FailureSources {
		if !slices.Contains(FailureSources, source) {
			return refuse(nil, "failure_sources", "%q is not one of %s", source, strings.Join(FailureSources, ", "))
		}
		if seen[source] {
			return refuse(nil, "failure_sources", "%q is named twice", source)
		}
		seen[source] = true
	}
	if len(input.FailureSources) > 0 && input.FailuresMeasuredSince == nil {
		return refuse(nil, "failures_measured_since", "a failure source counts from a day: name the first day")
	}
	return nil
}

// ReconciledThrough is how far the reconcile has imported rule's repository: the earliest recorded
// progress of its deploy workflow's and its pull-request checks workflow's run listings (their
// runsStep rows under runsProgressScope) and of every installation's merged-pull-request search,
// and nil until each listing has a row and every installation's search row is under the search's
// current scope (searchProgressScope of rules, every project's rules as q holds them now,
// ReadSourceRules, which a reader of several sources reads once): a change of the population or
// of the set of source repositories searches every installation afresh, and a source's numbers
// count the pull requests that search stores. A source saved at T has imported its backfill window
// once this is later than T, which is what a reader waits on before it reads the source's numbers
// (dispatch://INFRA-978, D3); last_reconcile_at moves after the first regular pass, long before
// that.
func ReconciledThrough(ctx context.Context, q Querier, rule DeliverySettings, rules *SourceRules) (*time.Time, error) {
	// The reconcile lists the pull-request checks workflow on its own only where it is another file
	// than the deploy workflow (runOnce), so a source with one workflow waits for one listing.
	names := []string{runsStep(DeliveryRunKindDeploy, rule.DeployRepo)}
	scopes := []string{runsProgressScope(rule.DeployRepo, rule.DeployWorkflowPath)}
	if rule.PRChecksWorkflowPath != rule.DeployWorkflowPath {
		names = append(names, runsStep(DeliveryRunKindPRChecks, rule.DeployRepo))
		scopes = append(scopes, runsProgressScope(rule.DeployRepo, rule.PRChecksWorkflowPath))
	}
	var through *time.Time
	err := q.QueryRow(ctx, `
		with runs as (
			select p.through from delivery_reconcile_progress p
			join unnest($1::text[], $2::text[]) as want(step, scope) on want.step = p.step and want.scope = p.scope
		), search as (
			select through, scope = $4 as current from delivery_reconcile_progress where starts_with(step, $3)
		)
		select case
			when (select count(*) from runs) = cardinality($1::text[])
				and exists (select from search) and not exists (select from search where not current)
			then least((select min(through) from runs), (select min(through) from search))
		end
	`, names, scopes, mergedPullRequestsStepPrefix, searchProgressScope(rules)).Scan(&through)
	if err != nil {
		return nil, fmt.Errorf("read how far %s is reconciled: %w", rule.DeployRepo, err)
	}
	return through, nil
}
