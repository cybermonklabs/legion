package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// One project's delivery settings (dispatch://INFRA-978, R1 to R4): the switch, the deploy
// sources, the population rule, the failure sources with the day they are counted from, the code
// groups (dispatch://INFRA-3206, R10), and the targets of the seven evals the editor names. GET
// answers every authenticated reader, since the project's tab explains its numbers with them; PUT
// is the deployment's write (scopeDeployment, routes_table.go), so a credential the broker scoped
// to one project is refused it by name, the project its path names included. The settings live in
// project_delivery_settings and project_deploy_sources (delivery.PutProjectSettings); the targets
// live in the project's eval rows alone (R3), each a new version of its eval, written in the save's
// one transaction (evals.Store.RegisterWith). GET and PUT /api/v1/settings/delivery, the singleton
// the release before reads, keep answering beside this route for one release
// (settings_delivery.go).

// deliverySettingsTargetSlugs are the evals whose targets the settings route reads and writes, in
// the order the editor shows them (the spec's Design): deploys a day, change fail rate, merge to
// production, pull request open to merge, deploy-run success, failed deployment recovery time and
// P0 issues without an owner.
var deliverySettingsTargetSlugs = []string{
	evals.SlugDeployFrequency, evals.SlugChangeFailRate, evals.SlugMergeToProduction, evals.SlugPROpenToMerge,
	evals.SlugDeployRunSuccess, evals.SlugFailedDeployRecovery, evals.SlugUnownedP0,
}

// deliveryTargetOff is the value a target takes that judges nothing: the eval's target is none.
const deliveryTargetOff = "off"

// projectDeliverySettings is what GET and PUT /api/v1/projects/{key}/settings/delivery answer: the
// project's stored settings, each source with its id, the repository it reads (resolved_repo,
// delivery.SourceRepoOf: the one it names, else the project's default repository, null where it
// reads none) and how far that repository is reconciled (reconciled_through, delivery.
// ReconciledThrough: null for an events source, a source of a project with delivery off, and a
// repository whose first import has not finished), the code groups in order (an empty list for a
// project that names none), and the targets read back from the project's eval rows
// (evals_current): each slug of deliverySettingsTargetSlugs the project holds an eval of, a number
// or "off". failures_measured_since is a UTC day, YYYY-MM-DD.
type projectDeliverySettings struct {
	Project               string                `json:"project"`
	Delivery              bool                  `json:"delivery"`
	DefaultRepo           *string               `json:"default_repo"`
	Sources               []projectDeploySource `json:"sources"`
	PopulationAuthors     []string              `json:"population_authors"`
	ExcludedRepos         []string              `json:"excluded_repos"`
	FailureSources        []string              `json:"failure_sources"`
	FailuresMeasuredSince *string               `json:"failures_measured_since"`
	CodeGroups            []delivery.CodeGroup  `json:"code_groups"`
	Targets               json.RawMessage       `json:"targets"`
	UpdatedBy             model.Actor           `json:"updated_by"`
	UpdatedAt             time.Time             `json:"updated_at"`
}

// projectDeploySource is one deploy source as the settings route answers it: the stored row, the
// repository it reads, and how far that repository is reconciled.
type projectDeploySource struct {
	delivery.DeploySource
	ResolvedRepo      *string    `json:"resolved_repo"`
	ReconciledThrough *time.Time `json:"reconciled_through"`
}

// projectDeliverySettingsInput is the body PUT /api/v1/projects/{key}/settings/delivery takes: every
// field of the settings, replacing what the project holds, and the targets it changes. targets is
// an object of the slugs of deliverySettingsTargetSlugs, each a number or "off"; a slug it leaves
// out keeps its target, and a slug whose target is the eval's already writes no version.
// code_groups is the project's whole list of code groups, in order (codeGroupsRefusal): absent or
// null keeps the list the project holds, as the release before this one, which sends none, needs,
// and [] clears it.
type projectDeliverySettingsInput struct {
	Delivery              bool                       `json:"delivery" required:"true"`
	Sources               []projectDeploySourceInput `json:"sources" required:"true"`
	PopulationAuthors     []string                   `json:"population_authors"`
	ExcludedRepos         []string                   `json:"excluded_repos"`
	FailureSources        []string                   `json:"failure_sources"`
	FailuresMeasuredSince *string                    `json:"failures_measured_since"`
	CodeGroups            *[]delivery.CodeGroup      `json:"code_groups"`
	Targets               json.RawMessage            `json:"targets"`
	Actor                 *model.Actor               `json:"actor"`
}

// projectDeploySourceInput is one deploy source of a save: a github_workflow source names its
// workflow, its production jobs and its pull-request checks workflow, and may name its repository
// (absent or null: the project's default repository); an events source names none of them.
type projectDeploySourceInput struct {
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

// getProjectDeliverySettings answers GET /api/v1/projects/{key}/settings/delivery: the project's
// delivery settings (projectDeliverySettings), or null where it has none. An unknown project is 404
// PROJECT_NOT_FOUND.
func (s *server) getProjectDeliverySettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, key); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer, err := s.readProjectDeliverySettings(r.Context(), key)
	if errors.Is(err, delivery.ErrNoSettings) {
		WriteJSON(w, http.StatusOK, nil)
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// readProjectDeliverySettings reads project's settings as the route answers them, or
// delivery.ErrNoSettings where it has none.
func (s *server) readProjectDeliverySettings(ctx context.Context, project string) (projectDeliverySettings, error) {
	pool := s.deps.Store.Pool
	settings, err := delivery.GetProjectSettings(ctx, pool, project)
	if err != nil {
		return projectDeliverySettings{}, err
	}
	answer := projectDeliverySettings{
		Project: settings.Project, Delivery: settings.Delivery, DefaultRepo: settings.DefaultRepo,
		Sources: make([]projectDeploySource, len(settings.Sources)), PopulationAuthors: settings.PopulationAuthors,
		ExcludedRepos: settings.ExcludedRepos, FailureSources: settings.FailureSources, CodeGroups: settings.CodeGroups,
		UpdatedBy: settings.UpdatedBy, UpdatedAt: settings.UpdatedAt,
	}
	if settings.FailuresMeasuredSince != nil {
		answer.FailuresMeasuredSince = new(settings.FailuresMeasuredSince.Format(time.DateOnly))
	}
	// The rules the reconcile walks: none of a project with delivery off, whose repositories it
	// does not walk, so their sources say nothing of how far they are reconciled. Every project's
	// rules, whose search scope each source's reconcile is read under, are read once, for the
	// first source the reconcile walks.
	rules := map[string]delivery.DeliverySettings{}
	for _, rule := range settings.Rules() {
		rules[delivery.RuleKey(rule.DeployRepo)] = rule
	}
	var walkedRules *delivery.SourceRules
	for i, source := range settings.Sources {
		answer.Sources[i] = projectDeploySource{DeploySource: source}
		repo, reads := delivery.SourceRepoOf(source, settings.DefaultRepo)
		if !reads {
			continue
		}
		answer.Sources[i].ResolvedRepo = &repo
		rule, walked := rules[delivery.RuleKey(repo)]
		if !walked {
			continue
		}
		if walkedRules == nil {
			if walkedRules, err = delivery.ReadSourceRules(ctx, pool); err != nil {
				return projectDeliverySettings{}, err
			}
		}
		if answer.Sources[i].ReconciledThrough, err = delivery.ReconciledThrough(ctx, pool, rule, walkedRules); err != nil {
			return projectDeliverySettings{}, err
		}
	}
	entries, err := s.registry().ProjectEvals(ctx, project, deliverySettingsTargetSlugs)
	if err != nil {
		return projectDeliverySettings{}, err
	}
	targets := make(map[string]any, len(entries))
	for slug, entry := range entries {
		targets[slug] = deliveryTargetOff
		if entry.Definition.Target != nil {
			targets[slug] = *entry.Definition.Target
		}
	}
	if answer.Targets, err = json.Marshal(targets); err != nil {
		return projectDeliverySettings{}, fmt.Errorf("encode the targets of %s: %w", project, err)
	}
	return answer, nil
}

// putProjectDeliverySettings answers PUT /api/v1/projects/{key}/settings/delivery: it replaces the
// project's delivery settings with the body's and writes each target the body changes as a new
// version of the project's eval, in one transaction, and answers the stored settings as GET does.
//
// It refuses, writing nothing: a field no reader could read, 400 DELIVERY_SETTINGS_INPUT naming the
// field and, for a source's, the source's index (delivery.ValidateProjectSettings, and the targets,
// the first day and the code groups here, codeGroupsRefusal naming the group in its message); code
// groups that would let a code eval hold more series than an eval holds, 400
// DELIVERY_SETTINGS_INPUT of the field code_groups with the count and the bound
// (evals.CheckCodeGroupSeriesRoom, read under the project's settings lock in the save's
// transaction); a source's repository the GitHub App cannot read, 409 DELIVERY_SETTINGS_ACCESS
// naming the source, proved for every github_workflow source while delivery is on and before
// anything is written, as the singleton's save proves its one repository (a save that turns
// delivery off proves nothing, so a project whose repository the App lost can still stop); and the
// D1 refusals, 409 DELIVERY_NO_DEFAULT_REPOSITORY and DELIVERY_REPOSITORY_SHARED, the latter naming
// the other project (delivery.PutProjectSettings).
// A repository is stored lower-case under its new name where a rename pair renames it away, as an
// excluded one is. The change rows both writes log are moved into the change log once the
// transaction commits, as every eval write's are (evals.Store.RegisterWith).
func (s *server) putProjectDeliverySettings(w http.ResponseWriter, r *http.Request) {
	var input projectDeliverySettingsInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	ctx := r.Context()
	key := r.PathValue("key")
	project, err := s.loadProject(ctx, s.deps.Store.Pool, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	save, refusal := s.projectSettingsSave(key, input)
	if refusal == nil {
		refusal = delivery.ValidateProjectSettings(save)
	}
	targets, targetRefusal := parseDeliveryTargets(input.Targets)
	if refusal == nil {
		refusal = targetRefusal
	}
	if refusal != nil {
		s.writeSettingsRefusal(w, refusal)
		return
	}
	if save.Delivery {
		denied, err := s.checkSourceAccess(ctx, save, project.DefaultRepo)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if denied != nil {
			s.writeSettingsRefusal(w, denied)
			return
		}
	}
	changes, unheld, err := s.deliveryTargetChanges(ctx, key, targets, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if unheld != nil {
		s.writeSettingsRefusal(w, unheld)
		return
	}
	if _, err := s.registry().RegisterWith(ctx, key, func(ctx context.Context, tx pgx.Tx) error {
		if save.CodeGroups != nil {
			if err := codeGroupSeriesRoom(ctx, tx, key, *save.CodeGroups); err != nil {
				return err
			}
		}
		_, err := delivery.PutProjectSettings(ctx, tx, save, actor)
		return err
	}, changes...); err != nil {
		var settingsRefusal *delivery.SettingsError
		switch {
		case errors.As(err, &settingsRefusal):
			s.writeSettingsRefusal(w, settingsRefusal)
		case errors.Is(err, delivery.ErrProjectNotFound):
			writeError(w, "PROJECT_NOT_FOUND", http.StatusNotFound, "no project has the key "+key)
		default:
			s.writeEvalError(w, err)
		}
		return
	}
	answer, err := s.readProjectDeliverySettings(ctx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}

// codeGroupSeriesRoom refuses, in tx, a save of groups as project's code groups that would let a
// code eval hold more series than MaxEvalSeries (evals.CheckCodeGroupSeriesRoom), as 400
// DELIVERY_SETTINGS_INPUT of the field code_groups with the check's reason. It takes the project's
// settings lock first, which PutProjectSettings then takes again, so the groups the check reads as
// stored are the ones this save replaces.
func codeGroupSeriesRoom(ctx context.Context, tx pgx.Tx, project string, groups []delivery.CodeGroup) error {
	if err := delivery.LockProject(ctx, tx, project); err != nil {
		return err
	}
	names := make([]string, len(groups))
	for i, group := range groups {
		names[i] = group.Name
	}
	var full *evals.Error
	err := evals.CheckCodeGroupSeriesRoom(ctx, tx, project, names)
	if errors.As(err, &full) {
		return delivery.InputRefusal(nil, "code_groups", "%s", full.Message)
	}
	return err
}

// projectSettingsSave is the save input names, its repositories canonical: each source's
// repository trimmed and lower-cased as GitHub could hold it (refs.ParseGitHubRepo), and each
// repository, a source's or an excluded one, under its new name where a rename pair renames it
// away (refs.Renames.Canonical), so no write stores an old name. A github_workflow source's job
// names are stored as the singleton's save stores them, since every reader matches a job by its
// exact name: each production job name trimmed (trimmedProductionJobNames, which refuses a blank
// name and a name given twice once trimmed; a source naming none is ValidateProjectSettings' to
// refuse), and each optional job and step name trimmed, or null where it is blank
// (optionalJobName). Each code group's prefixes and logins are trimmed, since no head branch and no
// login holds white space at either end. A source's repository GitHub could hold no repository of,
// a first day that is no YYYY-MM-DD day, or a code group codeGroupsRefusal refuses, is refused,
// named.
func (s *server) projectSettingsSave(project string, input projectDeliverySettingsInput) (delivery.ProjectSettingsInput, error) {
	save := delivery.ProjectSettingsInput{
		Project: project, Delivery: input.Delivery, Sources: make([]delivery.SourceInput, len(input.Sources)),
		PopulationAuthors: input.PopulationAuthors, FailureSources: input.FailureSources,
		ExcludedRepos: make([]string, len(input.ExcludedRepos)),
	}
	for i, excluded := range input.ExcludedRepos {
		save.ExcludedRepos[i] = s.deps.Renames.Canonical(excluded)
	}
	for i, source := range input.Sources {
		save.Sources[i] = delivery.SourceInput(source)
		if source.Repo != nil {
			owner, name, err := refs.ParseGitHubRepo(strings.TrimSpace(*source.Repo))
			if err != nil {
				return delivery.ProjectSettingsInput{}, delivery.InputRefusal(new(i), "repo",
					"source %d names %q, which is no GitHub repository owner/name", i, *source.Repo)
			}
			save.Sources[i].Repo = new(s.deps.Renames.Canonical(owner + "/" + name))
		}
		if source.Kind != delivery.SourceKindGitHubWorkflow {
			continue
		}
		if len(source.ProductionJobNames) > 0 {
			names, refusal := trimmedProductionJobNames(source.ProductionJobNames)
			if refusal != "" {
				return delivery.ProjectSettingsInput{}, &delivery.SettingsError{
					Code: delivery.CodeDeliverySettingsInput, Field: "production_job_names", Source: new(i), Message: refusal,
				}
			}
			save.Sources[i].ProductionJobNames = names
		}
		for _, name := range []**string{
			&save.Sources[i].ProductionApplyStepName, &save.Sources[i].StagingE2EJobName,
			&save.Sources[i].StagingApplyJobName, &save.Sources[i].PRChecksRollupJobName,
		} {
			*name = optionalJobName(*name)
		}
	}
	if input.FailuresMeasuredSince != nil {
		day, err := time.Parse(time.DateOnly, strings.TrimSpace(*input.FailuresMeasuredSince))
		if err != nil {
			return delivery.ProjectSettingsInput{}, delivery.InputRefusal(nil, "failures_measured_since",
				"%q is no day YYYY-MM-DD", *input.FailuresMeasuredSince)
		}
		save.FailuresMeasuredSince = &day
	}
	if input.CodeGroups != nil {
		groups := make([]delivery.CodeGroup, len(*input.CodeGroups))
		for i, group := range *input.CodeGroups {
			groups[i] = delivery.CodeGroup{Name: group.Name, BranchPrefixes: trimmedEntries(group.BranchPrefixes), AuthorLogins: trimmedEntries(group.AuthorLogins)}
		}
		if refusal := codeGroupsRefusal(groups); refusal != nil {
			return delivery.ProjectSettingsInput{}, refusal
		}
		save.CodeGroups = &groups
	}
	return save, nil
}

// trimmedEntries is entries, each trimmed, in order: a list, never nil, so a group stores an empty
// list where its body sent none.
func trimmedEntries(entries []string) []string {
	trimmed := make([]string, len(entries))
	for i, entry := range entries {
		trimmed[i] = strings.TrimSpace(entry)
	}
	return trimmed
}

// codeGroupNameMax is the most characters a code group's name holds.
const codeGroupNameMax = 64

// codeGroupSeriesBases are the bases of the series a code group names, `<base>:<name>`
// (fold.GroupSeries): survival in the code survival eval, and rework, iteration, noissue and
// other in the post-merge rework eval.
var codeGroupSeriesBases = []string{fold.SeriesSurvival, fold.SeriesRework, fold.SeriesIteration, fold.SeriesNoIssue, fold.SeriesOther}

// codeGroupsRefusal refuses a save's code groups, in order, where a regroup could not write their
// series or a reader could not tell two of them apart, and answers nil where it takes them. The
// refusal is 400 DELIVERY_SETTINGS_INPUT of the field code_groups, whose message names the group
// by its place, from 1 as the settings form numbers its groups, and by its name. A name is refused
// that is blank, holds more than codeGroupNameMax characters, or is the name of an earlier group in
// any case, and so is one that makes a series name the registry refuses (evals.CheckSeriesName over
// each of codeGroupSeriesBases: whitespace, a control character or a `.`). A blank prefix or login
// is refused. A group with no prefix and no login takes every change no group before it took, so
// it is the last group, and a project holds one at most.
func codeGroupsRefusal(groups []delivery.CodeGroup) *delivery.SettingsError {
	refuse := func(at int, format string, args ...any) *delivery.SettingsError {
		label := fmt.Sprintf("group %d", at+1)
		if name := groups[at].Name; strings.TrimSpace(name) != "" {
			label += fmt.Sprintf(" %q", name)
		}
		return delivery.InputRefusal(nil, "code_groups", "%s: %s", label, fmt.Sprintf(format, args...))
	}
	catchAll := -1
	for at, group := range groups {
		if strings.TrimSpace(group.Name) == "" {
			return refuse(at, "a group needs a name")
		}
		if length := utf8.RuneCountInString(group.Name); length > codeGroupNameMax {
			return refuse(at, "a name holds at most %d characters, and this one holds %d", codeGroupNameMax, length)
		}
		for _, base := range codeGroupSeriesBases {
			series := fold.GroupSeries(base, group.Name)
			if err := evals.CheckSeriesName(series); err != nil {
				return refuse(at, "the name makes the series %q, and %v", series, err)
			}
		}
		if first := slices.IndexFunc(groups[:at], func(earlier delivery.CodeGroup) bool { return strings.EqualFold(earlier.Name, group.Name) }); first >= 0 {
			return refuse(at, "group %d is named %q, and two groups' names differ by more than case", first+1, groups[first].Name)
		}
		if slices.Contains(group.BranchPrefixes, "") {
			return refuse(at, "a head-branch prefix is blank")
		}
		if slices.Contains(group.AuthorLogins, "") {
			return refuse(at, "an author login is blank")
		}
		if len(group.BranchPrefixes) > 0 || len(group.AuthorLogins) > 0 {
			continue
		}
		if catchAll >= 0 {
			return refuse(at, "group %d already has no prefix and no login, and so takes every change no group before it took: one group at most has neither", catchAll+1)
		}
		catchAll = at
	}
	if catchAll >= 0 && catchAll != len(groups)-1 {
		return refuse(catchAll, "a group with no prefix and no login takes every change no group before it took, so no group after it would take any: it is the last group")
	}
	return nil
}

// parseDeliveryTargets reads a save's targets: each slug of deliverySettingsTargetSlugs it names,
// with the target it sets (nil for "off"). Another slug, or a value that is neither a number nor
// "off", null included, is refused, named.
func parseDeliveryTargets(raw json.RawMessage) (map[string]*float64, error) {
	targets := map[string]*float64{}
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return targets, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, delivery.InputRefusal(nil, "targets", "an object of each eval's slug and its target, a number or \"off\"")
	}
	for slug, value := range values {
		field := "targets." + slug
		if !slices.Contains(deliverySettingsTargetSlugs, slug) {
			return nil, delivery.InputRefusal(nil, field, "the settings set the targets of %s alone", strings.Join(deliverySettingsTargetSlugs, ", "))
		}
		// A null decodes into a pointer as nil, with no error, where it would leave a number at 0.
		var number *float64
		if err := json.Unmarshal(value, &number); err == nil && number != nil {
			targets[slug] = number
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err == nil && text == deliveryTargetOff {
			targets[slug] = nil
			continue
		}
		return nil, delivery.InputRefusal(nil, field, "a target is a number or \"off\", and %s is neither", value)
	}
	return targets, nil
}

// checkSourceAccess proves the GitHub App can read the repository of each github_workflow source
// of save (delivery.SourceRepoOf over the project's default repository defaultRepo), before
// anything is written: a repository it cannot read is the refusal it answers, 409
// DELIVERY_SETTINGS_ACCESS naming the source. A source that reads no repository is left to the
// save, which refuses it DELIVERY_NO_DEFAULT_REPOSITORY.
func (s *server) checkSourceAccess(ctx context.Context, save delivery.ProjectSettingsInput, defaultRepo *string) (*delivery.SettingsError, error) {
	for i, source := range save.Sources {
		repo, reads := delivery.SourceRepoOf(delivery.DeploySource{Kind: source.Kind, Repo: source.Repo}, defaultRepo)
		if !reads {
			continue
		}
		owner, name, _ := strings.Cut(repo, "/")
		if _, err := s.deps.GitHub.CheckSource(ctx, owner, name); err != nil {
			if errors.Is(err, githubapp.ErrNoAppKey) || errors.Is(err, githubapp.ErrNoInstallation) || errors.Is(err, githubapp.ErrNoContentsRead) {
				return &delivery.SettingsError{
					Code: delivery.CodeDeliverySettingsAccess, Field: "repo", Source: new(i),
					Message: fmt.Sprintf("source %d reads %s, which Dispatch's GitHub App cannot read: %v", i, repo, err),
				}, nil
			}
			return nil, err
		}
	}
	return nil, nil
}

// deliveryTargetChanges is a register write of each target of targets that is not its eval's
// target already, as actor (Registration.Target alone, so every other field is carried forward
// from the eval's newest version), in deliverySettingsTargetSlugs' order. A target of an eval the
// project holds no version of is refused, named: its evals are seeded when the project is made.
func (s *server) deliveryTargetChanges(ctx context.Context, project string, targets map[string]*float64, actor model.Actor) ([]evals.Registration, *delivery.SettingsError, error) {
	if len(targets) == 0 {
		return nil, nil, nil
	}
	entries, err := s.registry().ProjectEvals(ctx, project, deliverySettingsTargetSlugs)
	if err != nil {
		return nil, nil, err
	}
	var changes []evals.Registration
	for _, slug := range deliverySettingsTargetSlugs {
		target, set := targets[slug]
		if !set {
			continue
		}
		entry, held := entries[slug]
		if !held {
			return nil, delivery.InputRefusal(nil, "targets."+slug, "project %s holds no %s eval to set the target of", project, slug), nil
		}
		if equalPointers(entry.Definition.Target, target) {
			continue
		}
		changes = append(changes, evals.Registration{Project: project, Slug: slug, Caller: actor, Target: evals.Set(target)})
	}
	return changes, nil, nil
}

// writeSettingsRefusal answers a refused save with its own body (delivery.SettingsError: its code,
// its reason, and the field, the source and the other project where it names them): 400 for
// DELIVERY_SETTINGS_INPUT and 409 for every other code. Any other error is writeHandlerError's.
func (s *server) writeSettingsRefusal(w http.ResponseWriter, err error) {
	var refusal *delivery.SettingsError
	if !errors.As(err, &refusal) {
		s.writeHandlerError(w, err)
		return
	}
	status := http.StatusConflict
	if refusal.Code == delivery.CodeDeliverySettingsInput {
		status = http.StatusBadRequest
	}
	WriteJSON(w, status, refusal)
}
