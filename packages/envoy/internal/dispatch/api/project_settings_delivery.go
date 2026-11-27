package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// One project's delivery settings (dispatch://INFRA-978, R1 to R4): the switch, the deploy
// sources, the population rule, the failure sources with the day they are counted from, and the
// targets of the seven evals the editor names. GET answers every authenticated reader, since the
// project's tab explains its numbers with them; PUT is the deployment's write (scopeDeployment,
// routes_table.go), so a credential the broker scoped to one project is refused it by name, the
// project its path names included. The settings live in project_delivery_settings and
// project_deploy_sources (delivery.PutProjectSettings); the targets live in the project's eval rows
// alone (R3), each a new version of its eval, written in the save's one transaction
// (evals.Store.RegisterWith). GET and PUT /api/v1/settings/delivery, the singleton the release
// before reads, keep answering beside this route for one release (settings_delivery.go).

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
// repository whose first import has not finished), and the targets read back from the project's
// eval rows (evals_current): each slug of deliverySettingsTargetSlugs the project holds an eval of,
// a number or "off", an object in every answer (`{}` where the project holds none of them), which
// its `required:"true"` tells the emitter, since it leaves a field rawFieldSchemas names out of
// `required` otherwise. failures_measured_since is a UTC day, YYYY-MM-DD.
type projectDeliverySettings struct {
	Project               string                `json:"project"`
	Delivery              bool                  `json:"delivery"`
	DefaultRepo           *string               `json:"default_repo"`
	Sources               []projectDeploySource `json:"sources"`
	PopulationAuthors     []string              `json:"population_authors"`
	ExcludedRepos         []string              `json:"excluded_repos"`
	FailureSources        []string              `json:"failure_sources"`
	FailuresMeasuredSince *string               `json:"failures_measured_since"`
	Targets               json.RawMessage       `json:"targets" required:"true"`
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

// projectDeliverySettingsInput is the body PUT /api/v1/projects/{key}/settings/delivery takes:
// every top-level field is optional, and a field the body leaves out keeps its stored value (the
// two-build rule, store/README.md: a build that does not know a field saves without dropping it).
// Explicit null clears a field that may be empty; sources, when present, replaces the list;
// targets is an object of the slugs of deliverySettingsTargetSlugs, each a number or "off", where
// a slug it leaves out keeps its target and a slug whose target is the eval's already writes no
// version.
type projectDeliverySettingsInput struct {
	Delivery              bool                       `json:"delivery"`
	Sources               []projectDeploySourceInput `json:"sources"`
	PopulationAuthors     []string                   `json:"population_authors"`
	ExcludedRepos         []string                   `json:"excluded_repos"`
	FailureSources        []string                   `json:"failure_sources"`
	FailuresMeasuredSince *string                    `json:"failures_measured_since"`
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
		ExcludedRepos: settings.ExcludedRepos, FailureSources: settings.FailureSources,
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

// putProjectDeliverySettings answers PUT /api/v1/projects/{key}/settings/delivery: it updates the
// project's delivery settings with each top-level field the body names (keeping the stored value
// of every top-level field it leaves out as the settings stand when the save is written, so a save
// that waited for another save of the project keeps what that one wrote; null clears a field that
// may be empty; sources, when present, replaces the list) and writes each target the body names
// that its eval does not hold already as a new version of the project's eval, in one transaction
// (settingsSave), and answers the stored settings as GET does.
//
// It refuses, writing nothing: a field no reader could read, 400 DELIVERY_SETTINGS_INPUT naming the
// field and, for a source's, the source's index (delivery.ValidateProjectSettings, and the targets
// and the first day here); a source's repository the GitHub App cannot read, 409
// DELIVERY_SETTINGS_ACCESS naming the source, proved while delivery is on for every repository a
// github_workflow source of the written settings reads, the default repository a source naming
// none reads included, before the transaction opens, as the singleton's save proves its one
// repository (a save that turns delivery off proves nothing, so a project whose repository the App
// lost can still stop); the D1 refusals, 409 DELIVERY_NO_DEFAULT_REPOSITORY and
// DELIVERY_REPOSITORY_SHARED, the latter naming the other project (delivery.PutProjectSettings);
// and 409 DELIVERY_SETTINGS_CHANGED, where another write moved what the save rests on under each
// of its attempts (resolveThenLock). A repository is stored lower-case under its new name where a
// rename pair renames it away, as an excluded one is. The settings tables' triggers write their
// `settings` change rows into the change log itself, in the save's transaction, taking the change
// log's lock at the first write that moves a logged column and holding it to the commit
// (migration 20261123010254, delivery_change_log); the target writes' change rows wait in the
// change log's outbox and are moved into the log once the transaction commits, as every eval
// write's are (evals.Store.RegisterWith).
func (s *server) putProjectDeliverySettings(w http.ResponseWriter, r *http.Request) {
	var input projectDeliverySettingsInput
	body, err := decodeJSONBody(r, &input)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	present, err := topLevelJSONMembers(body)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ctx := r.Context()
	key := r.PathValue("key")
	if _, err := s.loadProject(ctx, s.deps.Store.Pool, key); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	named, refusal := s.projectSettingsSave(key, input)
	if refusal != nil {
		s.writeSettingsRefusal(w, refusal)
		return
	}
	targets, targetRefusal := parseDeliveryTargets(input.Targets)
	save := &settingsSave{
		server: s, project: key, named: named, present: present, targets: targets, targetRefusal: targetRefusal,
		actor: actor, proved: map[string]bool{},
	}
	if _, err := resolveThenLock(ctx, save.resolve, save.write, settingsChanged(key)); err != nil {
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

// codeDeliverySettingsChanged is the refusal of a settings save under each of whose attempts
// (resolutionAttempts) another write moved the stored settings or a target it names, so it could
// not prove what it would write: 409, with nothing written.
const codeDeliverySettingsChanged = "DELIVERY_SETTINGS_CHANGED"

// settingsChanged is project's refusal codeDeliverySettingsChanged.
func settingsChanged(project string) *delivery.SettingsError {
	message := fmt.Sprintf("another write moved project %s's delivery settings, or a target this save names, under each of its %d attempts: read them again and save again", project, resolutionAttempts)
	return &delivery.SettingsError{Code: codeDeliverySettingsChanged, Message: message}
}

// settingsSave is one PUT of a project's delivery settings as resolveThenLock runs it: the fields
// its body names (named, its repositories canonical: projectSettingsSave) and which of them it
// names (present), the targets it sets and the refusal of its targets, its writer, and the
// repositories an attempt has proved the GitHub App can read, by lower-case name. Each attempt
// reads the stored settings before its transaction (resolve) and again once it holds the project's
// lock (write), and writes the second read's value of each field the body leaves out.
type settingsSave struct {
	server        *server
	project       string
	named         delivery.ProjectSettingsInput
	present       map[string]jsonMember
	targets       map[string]*float64
	targetRefusal error
	actor         model.Actor
	proved        map[string]bool
}

// resolve is an attempt's work before its transaction, holding no lock: the save as the stored
// settings fill it (merged), refused where no reader could read it (delivery.ValidateProjectSettings,
// then the targets), the GitHub App's read of each repository its github_workflow sources read
// proved while delivery is on (checkSourceAccess), and the register write of each target the body
// names that its eval does not hold already (deliveryTargetChanges).
func (save *settingsSave) resolve(ctx context.Context) ([]evals.Registration, error) {
	s := save.server
	merged, defaultRepo, err := save.merged(ctx, s.deps.Store.Pool)
	if err != nil {
		return nil, err
	}
	if err := delivery.ValidateProjectSettings(merged); err != nil {
		return nil, err
	}
	if save.targetRefusal != nil {
		return nil, save.targetRefusal
	}
	if merged.Delivery {
		denied, err := s.checkSourceAccess(ctx, merged, defaultRepo, save.proved)
		if err != nil {
			return nil, err
		}
		if denied != nil {
			return nil, denied
		}
	}
	changes, unheld, err := s.deliveryTargetChanges(ctx, save.project, save.targets, save.actor)
	if err != nil {
		return nil, err
	}
	if unheld != nil {
		return nil, unheld
	}
	return changes, nil
}

// write is an attempt's transaction: the settings and the target changes resolve worked out, in
// one write (evals.Store.RegisterWith, which takes the changed evals' locks first), under the
// project's lock (delivery.LockProject, then PutProjectSettings' repository locks), so each field
// the body leaves out is read where no other save of the project can commit before this one. It
// asks GitHub nothing. It rolls back and answers errStaleResolution, for resolveThenLock to resolve
// again, where delivery is on and a source reads a repository no attempt proved (another save's
// source, or the default repository a source naming none reads, written while this one waited),
// and where a target the body names moved since resolve read it (targetsMoved).
func (save *settingsSave) write(ctx context.Context, changes []evals.Registration) ([]evals.Registered, error) {
	return save.server.registry().RegisterWith(ctx, save.project, func(ctx context.Context, tx pgx.Tx) error {
		if err := delivery.LockProject(ctx, tx, save.project); err != nil {
			return err
		}
		merged, defaultRepo, err := save.merged(ctx, tx)
		if err != nil {
			return err
		}
		if merged.Delivery && !save.provedEvery(merged, defaultRepo) {
			return errStaleResolution
		}
		moved, err := save.targetsMoved(ctx, tx, changes)
		if err != nil {
			return err
		}
		if moved {
			return errStaleResolution
		}
		_, err = delivery.PutProjectSettings(ctx, tx, merged, save.actor)
		return err
	}, changes...)
}

// merged is the save as q holds the project now: the fields the body names and the stored value of
// each field it leaves out (keepStoredDeliverySettings), and the project's default repository,
// which a github_workflow source naming no repository reads.
func (save *settingsSave) merged(ctx context.Context, q delivery.Querier) (delivery.ProjectSettingsInput, *string, error) {
	merged := save.named
	stored, err := delivery.GetProjectSettings(ctx, q, save.project)
	if err == nil {
		keepStoredDeliverySettings(&merged, save.present, stored)
		return merged, stored.DefaultRepo, nil
	}
	if !errors.Is(err, delivery.ErrNoSettings) {
		return delivery.ProjectSettingsInput{}, nil, err
	}
	var defaultRepo *string
	if err := q.QueryRow(ctx, `select default_repo from projects where key = $1`, save.project).Scan(&defaultRepo); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return delivery.ProjectSettingsInput{}, nil, delivery.ErrProjectNotFound
		}
		return delivery.ProjectSettingsInput{}, nil, fmt.Errorf("read the default repository of %s: %w", save.project, err)
	}
	return merged, defaultRepo, nil
}

// provedEvery reports whether an attempt proved every repository a github_workflow source of merged
// reads (delivery.SourceRepoOf over defaultRepo). A source that reads none is PutProjectSettings'
// to refuse.
func (save *settingsSave) provedEvery(merged delivery.ProjectSettingsInput, defaultRepo *string) bool {
	for _, source := range merged.Sources {
		repo, reads := delivery.SourceRepoOf(delivery.DeploySource{Kind: source.Kind, Repo: source.Repo}, defaultRepo)
		if reads && !save.proved[strings.ToLower(repo)] {
			return false
		}
	}
	return true
}

// targetsMoved reports whether a target the body names stands in tx at another value than the one
// changes were worked out from (deliveryTargetChanges): one that now differs from what the body sets
// and has no change among them, which the write would leave as another write moved it although the
// body sets it, or one that now equals it and has one, which would write a version of a target the
// eval holds already (a person's write of an unmoved target is still a version, evals'
// sameDefinition). The eval of each change is locked (RegisterWith), so its target stands until
// the save commits; another eval the body names is not, and a write that moves it after this read
// comes after this save, which writes nothing to it.
func (save *settingsSave) targetsMoved(ctx context.Context, tx pgx.Tx, changes []evals.Registration) (bool, error) {
	for _, slug := range deliverySettingsTargetSlugs {
		target, named := save.targets[slug]
		if !named {
			continue
		}
		entry, held, err := evals.EvalIn(ctx, tx, evals.EvalID(save.project, slug))
		if err != nil {
			return false, err
		}
		changed := slices.ContainsFunc(changes, func(change evals.Registration) bool { return change.Slug == slug })
		if !held || equalPointers(entry.Definition.Target, target) == changed {
			return true, nil
		}
	}
	return false, nil
}

// topLevelJSONMembers reads the top-level names independently of their values. In the typed
// decode both an omitted list and an explicit null list are nil; only the latter clears one.
func topLevelJSONMembers(body []byte) (map[string]jsonMember, error) {
	var members map[string]jsonMember
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&members); err != nil {
		return nil, fmt.Errorf("read the settings body's members: %w", err)
	}
	return members, nil
}

// keepStoredDeliverySettings fills each field of save that present, the body's top-level names,
// leaves out with stored's value, without normalizing those values again. The save reads stored
// both before its transaction, for its validation and its GitHub access check, which both read the
// whole save, and under the project's lock, for what it writes (settingsSave).
func keepStoredDeliverySettings(save *delivery.ProjectSettingsInput, present map[string]jsonMember, stored delivery.ProjectSettings) {
	has := func(field string) bool {
		if _, named := present[field]; named {
			return true
		}
		// encoding/json matches struct fields case-insensitively.
		for name := range present {
			if strings.EqualFold(field, name) {
				return true
			}
		}
		return false
	}
	deliveryNamed, sourcesNamed := has("delivery"), has("sources")
	authorsNamed, excludedNamed := has("population_authors"), has("excluded_repos")
	failuresNamed, sinceNamed := has("failure_sources"), has("failures_measured_since")
	if !deliveryNamed {
		save.Delivery = stored.Delivery
	}
	if !sourcesNamed {
		save.Sources = make([]delivery.SourceInput, len(stored.Sources))
		for i, source := range stored.Sources {
			save.Sources[i] = delivery.SourceInput{
				Kind:                    source.Kind,
				Repo:                    source.Repo,
				DeployWorkflowPath:      source.DeployWorkflowPath,
				ProductionJobNames:      source.ProductionJobNames,
				ProductionApplyStepName: source.ProductionApplyStepName,
				StagingE2EJobName:       source.StagingE2EJobName,
				StagingApplyJobName:     source.StagingApplyJobName,
				PRChecksWorkflowPath:    source.PRChecksWorkflowPath,
				PRChecksRollupJobName:   source.PRChecksRollupJobName,
			}
		}
	}
	if !authorsNamed {
		save.PopulationAuthors = stored.PopulationAuthors
	}
	if !excludedNamed {
		save.ExcludedRepos = stored.ExcludedRepos
	}
	if !failuresNamed {
		save.FailureSources = stored.FailureSources
	}
	if !sinceNamed {
		save.FailuresMeasuredSince = stored.FailuresMeasuredSince
	}
}

// projectSettingsSave is the save input names, its repositories canonical: each source's
// repository trimmed and lower-cased as GitHub could hold it (parseSourceRepo), and each
// repository, a source's or an excluded one, under its new name where a rename pair renames it
// away (refs.Renames.Canonical), so no write stores an old name. A github_workflow source's job
// names are stored as the singleton's save stores them, since every reader matches a job by its
// exact name: each production job name trimmed (trimmedProductionJobNames, which refuses a blank
// name and a name given twice once trimmed; a source naming none is ValidateProjectSettings' to
// refuse), and each optional job and step name trimmed, or null where it is blank
// (optionalJobName). A source's repository GitHub could hold no repository of, or a first day that
// is no YYYY-MM-DD day, is refused, named.
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
			owner, name, err := parseSourceRepo(*source.Repo)
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
	return save, nil
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
// of save (delivery.SourceRepoOf over the project's default repository defaultRepo) that proved,
// the repositories an earlier attempt of the save proved, by lower-case name, does not hold, and
// adds each it proves to proved. It runs before the save's transaction opens, so no lock waits on
// GitHub: a repository it cannot read is the refusal it answers, 409 DELIVERY_SETTINGS_ACCESS
// naming the source. A source that reads no repository is left to the save, which refuses it
// DELIVERY_NO_DEFAULT_REPOSITORY.
func (s *server) checkSourceAccess(ctx context.Context, save delivery.ProjectSettingsInput, defaultRepo *string, proved map[string]bool) (*delivery.SettingsError, error) {
	for i, source := range save.Sources {
		repo, reads := delivery.SourceRepoOf(delivery.DeploySource{Kind: source.Kind, Repo: source.Repo}, defaultRepo)
		if !reads || proved[strings.ToLower(repo)] {
			continue
		}
		owner, name, _ := strings.Cut(repo, "/")
		if _, err := s.deps.GitHub.CheckSource(ctx, owner, name); err != nil {
			if appCannotRead(err) {
				return &delivery.SettingsError{
					Code: delivery.CodeDeliverySettingsAccess, Field: "repo", Source: new(i),
					Message: fmt.Sprintf("source %d reads %s, which Dispatch's GitHub App cannot read: %v", i, repo, err),
				}, nil
			}
			return nil, err
		}
		proved[strings.ToLower(repo)] = true
	}
	return nil, nil
}

// appCannotRead reports whether err, the answer of a check of a repository
// (githubapp.Client.CheckSource), says Dispatch's GitHub App cannot read it: no app key, no
// installation that covers it, or no contents read.
func appCannotRead(err error) bool {
	return errors.Is(err, githubapp.ErrNoAppKey) || errors.Is(err, githubapp.ErrNoInstallation) || errors.Is(err, githubapp.ErrNoContentsRead)
}

// deliveryTargetChanges is a register write of each target of targets that is not its eval's
// target already, as actor (Registration.Target alone, so every other field is carried forward
// from the eval's newest version), in deliverySettingsTargetSlugs' order, read before the save's
// transaction opens; the transaction reads each target again (settingsSave.targetsMoved). A target
// of an eval the project holds no version of is refused, named: its evals are seeded when the
// project is made.
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
