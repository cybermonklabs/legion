package delivery

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// DeploySource is one row of project_deploy_sources (migration 20261123010254): its kind, the
// repository it names (nil for the project's default repository), its rule, and the bookkeeping the
// intake and the reconcile write for its repository.
type DeploySource struct {
	ID                      string     `json:"id"`
	Kind                    string     `json:"kind" enum:"github_workflow,events"`
	Repo                    *string    `json:"repo"`
	DeployWorkflowPath      *string    `json:"deploy_workflow_path"`
	ProductionJobNames      []string   `json:"production_job_names"`
	ProductionApplyStepName *string    `json:"production_apply_step_name"`
	StagingE2EJobName       *string    `json:"staging_e2e_job_name"`
	StagingApplyJobName     *string    `json:"staging_apply_job_name"`
	PRChecksWorkflowPath    *string    `json:"pr_checks_workflow_path"`
	PRChecksRollupJobName   *string    `json:"pr_checks_rollup_job_name"`
	LastEventAt             *time.Time `json:"last_event_at"`
	LastReconcileAt         *time.Time `json:"last_reconcile_at"`
	LastError               *string    `json:"last_error"`
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
	CodeGroups            []CodeGroup    `json:"code_groups"`
	UpdatedBy             model.Actor    `json:"updated_by"`
	UpdatedAt             time.Time      `json:"updated_at"`
}

// CodeGroup is one of a project's code groups (dispatch://INFRA-3206, R10), which the code
// survival and post-merge rework evals count each merged change for: its name, which names its
// series of each eval, and the head-branch prefixes and author logins of its changes. A change
// belongs to the first of the project's groups with a prefix that starts its head branch or a
// login equal to the login that opened it; a group with neither takes every change no group before
// it took. The column project_delivery_settings.code_groups holds the list, in order (migration
// 20261126133047), and the settings route holds a group to the rules a regroup can write
// (api/project_settings_delivery.go, codeGroupsRefusal).
type CodeGroup struct {
	Name           string   `json:"name"`
	BranchPrefixes []string `json:"branch_prefixes"`
	AuthorLogins   []string `json:"author_logins"`
}

// SurvivalGroups is groups, in order, as the code survival computation reads them
// (survival.GroupOf): the run and the regroup hand a project's settings to it through here.
func SurvivalGroups(groups []CodeGroup) []survival.Group {
	converted := make([]survival.Group, len(groups))
	for i, group := range groups {
		converted[i] = survival.Group(group)
	}
	return converted
}

// CodeGroupNames is the names of groups, in order.
func CodeGroupNames(groups []CodeGroup) []string {
	names := make([]string, len(groups))
	for i, group := range groups {
		names[i] = group.Name
	}
	return names
}

// CodeGroupsJSON is groups' canonical JSON: the list in order, every group's list of prefixes and
// of logins written, empty or not. It is what a save stores in project_delivery_settings.code_groups
// (PutProjectSettings), and what codesurvival hashes into the digest of the groups a project's day
// rows stand built under.
func CodeGroupsJSON(groups []CodeGroup) ([]byte, error) {
	canonical := make([]CodeGroup, len(groups))
	for i, group := range groups {
		canonical[i] = CodeGroup{Name: group.Name, BranchPrefixes: nonNil(group.BranchPrefixes), AuthorLogins: nonNil(group.AuthorLogins)}
	}
	return json.Marshal(canonical)
}

// SourceRepoOf is the repository a github_workflow source reads (R2): the one it names, else the
// project's default repository, and false where it names none and the project has none. Every
// reader that names a source's repository asks here.
func SourceRepoOf(source DeploySource, defaultRepo *string) (string, bool) {
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
		repo, ok := SourceRepoOf(source, s.DefaultRepo)
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

// SourceRulesOf is the SourceRules of rules alone, each found by its DeployRepo, with no
// population and no default repositories: what judges runs and pull requests by rules read
// elsewhere, such as those the eval registry's day rows stand built under (api's evalNarrowing).
func SourceRulesOf(rules []DeliverySettings) *SourceRules {
	return newSourceRules(rules, Population{}, nil)
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
	s.failure_sources, s.failures_measured_since, s.code_groups, s.updated_by, s.updated_at`

const deploySourceColumns = `id::text, kind, repo, deploy_workflow_path, production_job_names, production_apply_step_name,
	staging_e2e_job_name, staging_apply_job_name, pr_checks_workflow_path, pr_checks_rollup_job_name,
	last_event_at, last_reconcile_at, last_error`

func scanProjectSettings(row pgx.Row) (ProjectSettings, error) {
	var settings ProjectSettings
	var codeGroups, updatedBy []byte
	if err := row.Scan(&settings.Project, &settings.Delivery, &settings.DefaultRepo, &settings.PopulationAuthors,
		&settings.ExcludedRepos, &settings.FailureSources, &settings.FailuresMeasuredSince, &codeGroups, &updatedBy,
		&settings.UpdatedAt); err != nil {
		return ProjectSettings{}, err
	}
	if err := json.Unmarshal(codeGroups, &settings.CodeGroups); err != nil {
		return ProjectSettings{}, fmt.Errorf("decode the code groups of %s: %w", settings.Project, err)
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

// SourceInput is one deploy source of a save, as the editor sends it.
type SourceInput struct {
	Kind                    string
	Repo                    *string
	DeployWorkflowPath      *string
	ProductionJobNames      []string
	ProductionApplyStepName *string
	StagingE2EJobName       *string
	StagingApplyJobName     *string
	PRChecksWorkflowPath    *string
	PRChecksRollupJobName   *string
}

// ProjectSettingsInput is one save of a project's delivery settings: every field the editor sends.
// CodeGroups nil keeps the code groups the project holds, which is what a body that names none
// asks (the release before this one sends none); an empty list clears them.
type ProjectSettingsInput struct {
	Project               string
	Delivery              bool
	Sources               []SourceInput
	PopulationAuthors     []string
	ExcludedRepos         []string
	FailureSources        []string
	FailuresMeasuredSince *time.Time
	CodeGroups            *[]CodeGroup
}

// SettingsError is a save the delivery rules refuse (D1): its code, its reason, the field and the
// source's index where the refusal is about one, and the other project where one shares a
// repository.
type SettingsError struct {
	Code    string `json:"code"`
	Message string `json:"error"`
	Field   string `json:"field,omitempty"`
	Source  *int   `json:"source,omitempty"`
	Project string `json:"project,omitempty"`
}

func (e *SettingsError) Error() string { return e.Code + ": " + e.Message }

// ErrProjectNotFound is a save of a project no row holds.
var ErrProjectNotFound = errors.New("no such project")

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

// LockRepositories takes the transaction-scoped lock of each repository's delivery ownership, in
// name order so two transactions taking two of them cannot wait on each other: a save and a
// default-repository write that would each make one repository the source of a delivery-on project
// see each other's commit.
func LockRepositories(ctx context.Context, tx pgx.Tx, repos []string) error {
	for _, repo := range lowerSortedSet(repos) {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(6901, hashtext($1))`, repo); err != nil {
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

func sharedRepository(repo, other string, index *int) *SettingsError {
	return &SettingsError{
		Code:    CodeDeliveryRepositoryShared,
		Message: fmt.Sprintf("%s is already a deploy source of project %s, which has delivery on; a repository is the deploy source of one project at most", repo, other),
		Source:  index,
		Project: other,
	}
}

// PutProjectSettings saves input as its project's delivery settings in tx and answers the stored
// settings. With delivery on, a github_workflow source that names no repository needs the project's
// default repository (R2, DELIVERY_NO_DEFAULT_REPOSITORY), and no repository may be a source of
// another delivery-on project (D1, DELIVERY_REPOSITORY_SHARED); the checks and the write run under
// each repository's lock. Whatever the switch says, two github_workflow sources may not read one
// repository (DELIVERY_SETTINGS_INPUT), and two that name none read one, the project's default
// repository, whether the project has one or not. The save replaces the project's sources: a
// source whose repository and rule are both unchanged keeps its row, its id and its bookkeeping;
// every other source is written afresh, and its repository's reconcile progress is cleared, so the
// next reconcile imports it from the start, as the singleton's save did.
func PutProjectSettings(ctx context.Context, tx pgx.Tx, input ProjectSettingsInput, actor model.Actor) (ProjectSettings, error) {
	input.Sources = slices.Clone(input.Sources)
	for i, source := range input.Sources {
		if source.Repo != nil {
			input.Sources[i].Repo = new(strings.ToLower(strings.TrimSpace(*source.Repo)))
		}
	}
	if err := ValidateProjectSettings(input); err != nil {
		return ProjectSettings{}, err
	}
	if err := LockProject(ctx, tx, input.Project); err != nil {
		return ProjectSettings{}, err
	}
	var defaultRepo *string
	if err := tx.QueryRow(ctx, `select default_repo from projects where key = $1`, input.Project).Scan(&defaultRepo); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProjectSettings{}, ErrProjectNotFound
		}
		return ProjectSettings{}, fmt.Errorf("read the default repository of %s: %w", input.Project, err)
	}
	repos := make([]string, len(input.Sources))
	var lock []string
	// unnamed is the first github_workflow source that reads no repository, which a project with
	// delivery off and no default repository keeps: a second is the same repository as far as the
	// one-repository-per-source index is concerned (migration 20261123010254).
	unnamed := -1
	for i, source := range input.Sources {
		repo, ok := SourceRepoOf(DeploySource{Kind: source.Kind, Repo: source.Repo}, defaultRepo)
		if source.Kind != SourceKindGitHubWorkflow {
			continue
		}
		if !ok {
			if input.Delivery {
				return ProjectSettings{}, &SettingsError{
					Code:    CodeDeliveryNoDefaultRepository,
					Message: fmt.Sprintf("source %d of project %s names no repository and the project has no default repository: name the source's repository, or set the project's default repository in Settings -> Projects", i, input.Project),
					Field:   "repo",
					Source:  new(i),
				}
			}
			if unnamed >= 0 {
				return ProjectSettings{}, &SettingsError{
					Code:    CodeDeliverySettingsInput,
					Message: fmt.Sprintf("sources %d and %d both name no repository, so both read the project's default repository: a repository is one source of a project", unnamed, i),
					Field:   "repo",
					Source:  new(i),
				}
			}
			unnamed = i
			continue
		}
		for j := range i {
			if repos[j] != "" && strings.EqualFold(repos[j], repo) {
				return ProjectSettings{}, &SettingsError{
					Code:    CodeDeliverySettingsInput,
					Message: fmt.Sprintf("sources %d and %d both read %s: a repository is one source of a project", j, i, repo),
					Field:   "repo",
					Source:  new(i),
				}
			}
		}
		repos[i] = repo
		lock = append(lock, repo)
	}
	previous, err := GetProjectSettings(ctx, tx, input.Project)
	if err != nil && !errors.Is(err, ErrNoSettings) {
		return ProjectSettings{}, err
	}
	for _, source := range previous.Sources {
		if repo, ok := SourceRepoOf(source, defaultRepo); ok {
			lock = append(lock, repo)
		}
	}
	if err := LockRepositories(ctx, tx, lock); err != nil {
		return ProjectSettings{}, err
	}
	if input.Delivery {
		for i, repo := range repos {
			if repo == "" {
				continue
			}
			other, shared, err := RepositoryOwner(ctx, tx, repo, input.Project)
			if err != nil {
				return ProjectSettings{}, err
			}
			if shared {
				return ProjectSettings{}, sharedRepository(repo, other, new(i))
			}
		}
	}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		return ProjectSettings{}, fmt.Errorf("encode the delivery settings author: %w", err)
	}
	var since *string
	if input.FailuresMeasuredSince != nil {
		day := input.FailuresMeasuredSince.UTC().Format(time.DateOnly)
		since = &day
	}
	// codeGroups is the groups the save writes (CodeGroupsJSON), and null where the input names
	// none, which keeps the stored groups (an insert stores the column's empty list).
	var codeGroups []byte
	if input.CodeGroups != nil {
		if codeGroups, err = CodeGroupsJSON(*input.CodeGroups); err != nil {
			return ProjectSettings{}, fmt.Errorf("encode the code groups of %s: %w", input.Project, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		insert into project_delivery_settings (project_key, delivery, population_authors, excluded_repos,
			failure_sources, failures_measured_since, code_groups, updated_by)
		values ($1, $2, $3, $4, $5, $6::date, coalesce($7::jsonb, '[]'), $8)
		on conflict (project_key) do update set
			delivery = excluded.delivery,
			population_authors = excluded.population_authors,
			excluded_repos = excluded.excluded_repos,
			failure_sources = excluded.failure_sources,
			failures_measured_since = excluded.failures_measured_since,
			code_groups = coalesce($7::jsonb, project_delivery_settings.code_groups),
			updated_by = excluded.updated_by,
			updated_at = now()
	`, input.Project, input.Delivery, nonNil(input.PopulationAuthors), nonNil(input.ExcludedRepos),
		nonNil(input.FailureSources), since, codeGroups, actorJSON); err != nil {
		return ProjectSettings{}, fmt.Errorf("save the delivery settings of %s: %w", input.Project, err)
	}
	if err := replaceSources(ctx, tx, input, previous.Sources, defaultRepo); err != nil {
		return ProjectSettings{}, err
	}
	return GetProjectSettings(ctx, tx, input.Project)
}

// sameRule is whether a stored source and an input name one repository and one rule, so the save
// keeps the stored row and its bookkeeping.
func sameRule(stored DeploySource, input SourceInput) bool {
	eq := func(a, b *string) bool { return deref(a) == deref(b) && (a == nil) == (b == nil) }
	return stored.Kind == input.Kind && eq(stored.Repo, input.Repo) && eq(stored.DeployWorkflowPath, input.DeployWorkflowPath) &&
		slices.Equal(stored.ProductionJobNames, input.ProductionJobNames) && eq(stored.ProductionApplyStepName, input.ProductionApplyStepName) &&
		eq(stored.StagingE2EJobName, input.StagingE2EJobName) && eq(stored.StagingApplyJobName, input.StagingApplyJobName) &&
		eq(stored.PRChecksWorkflowPath, input.PRChecksWorkflowPath) && eq(stored.PRChecksRollupJobName, input.PRChecksRollupJobName)
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
		if repo, ok := SourceRepoOf(old, defaultRepo); ok {
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
		if repo, ok := SourceRepoOf(DeploySource{Kind: source.Kind, Repo: source.Repo}, defaultRepo); ok {
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
	steps := [][2]string{{runsStep(DeliveryRunKindDeploy, rule.DeployRepo), runsProgressScope(rule.DeployRepo, rule.DeployWorkflowPath)}}
	if rule.PRChecksWorkflowPath != rule.DeployWorkflowPath {
		steps = append(steps, [2]string{runsStep(DeliveryRunKindPRChecks, rule.DeployRepo), runsProgressScope(rule.DeployRepo, rule.PRChecksWorkflowPath)})
	}
	names, scopes := make([]string, len(steps)), make([]string, len(steps))
	for i, step := range steps {
		names[i], scopes[i] = step[0], step[1]
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
