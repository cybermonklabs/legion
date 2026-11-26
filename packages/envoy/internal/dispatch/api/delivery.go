package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// defaultDeliveryWindow is the timeline's default when the caller passes neither from nor to: the
// last 7 days, matching the Acceptance criterion's "one recent 7-day window".
const defaultDeliveryWindow = 7 * 24 * time.Hour

// maxDeliveryWindow bounds how wide [from, to) may be: CONTRACT.md measures 28 days of this
// population at roughly 3,500 pull requests, so three times that backfill window is generous
// headroom for a real dashboard query while still refusing the unbounded "from the epoch" request
// an authenticated-but-careless caller (any agent session; GET /api/v1/delivery/timeline is
// authAny) could otherwise send against the shared pool.
const maxDeliveryWindow = 3 * 28 * 24 * time.Hour

// The timeline's facets, by the query parameter that selects each and the facet_counts key that
// counts it. A pull request carries one or more values for each (deliveryRow.values).
var deliveryFacets = []string{"repo", "parent_agent", "session", "issue", "priority", "component", "author", "rework", "deployed"}

// The facets the page colours merges by, by the color_counts key that counts each.
var deliveryColorFacets = []string{"repo", "author", "priority", "component", "parent_agent"}

// Placeholder facet values, split by cause as the page labels them (features/delivery/lib/facets.ts
// holds the same four): a pull request naming no issue, an issue with no priority or no component,
// and a pull request whose commits name no session.
const (
	deliveryNoIssue     = "__no_issue__"
	deliveryNoPriority  = "__no_priority__"
	deliveryNoComponent = "__no_component__"
	deliveryNoSession   = "__no_session__"
)

// The rework facet's two values, as the page names them (features/delivery/lib/facets.ts's
// ReworkFacet): a pull request that adds value, and one that reworks earlier work.
const (
	deliveryReworkValue  = "value"
	deliveryReworkRework = "rework"
)

// deliveryPrioritySelection reads a priority selection as the page writes it (P0-P3) or as a
// person types it (p0, or the bare digit the issue routes store).
var deliveryPrioritySelection = regexp.MustCompile(`^[pP]?([0-3])$`)

// parseDeliveryWindow reads from/to (RFC3339) from query, defaulting to the last 7 days when
// either is absent, and refuses a malformed value, an inverted window, or one wider than
// maxDeliveryWindow. A from the query leaves out is to less defaultDeliveryWindow, held to the UTC
// years 1 to 9999 as a named one is (parseOptionalRFC3339, R18): a to in the first seven days of
// the year 1 would otherwise start the window in the year 0. Its refusal names the from it derived
// and where it came from, since the query that earns it names no from.
func parseDeliveryWindow(query url.Values) (from, to time.Time, err error) {
	if to, err = parseOptionalRFC3339(query, "to", time.Now().UTC()); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if from, err = parseOptionalRFC3339(query, "from", to.Add(-defaultDeliveryWindow)); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if year := from.UTC().Year(); !query.Has("from") && year < 1 {
		return time.Time{}, time.Time{}, fmt.Errorf("from is not named, so it is to less the default %d days, %s, a moment of the year %d: from must be a moment of the UTC years 1 to 9999",
			defaultDeliveryWindow/(24*time.Hour), from.UTC().Format(time.RFC3339Nano), year)
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, errors.New("from must be before to")
	}
	if to.Sub(from) > maxDeliveryWindow {
		return time.Time{}, time.Time{}, errors.New("from..to must not span more than 84 days")
	}
	return from, to, nil
}

// The query keys each delivery read takes one value of, in the order its shape declares them,
// which a query names once at most (repeatedQueryKey): the fields of its route shape's query
// struct (routeShapes' Params) that hold one value (oneValueQueryKeys), read once when the package
// starts. Every facet the timeline and the measures filter by (deliveryFacets) is a list field
// there, and so stays repeatable.
var (
	deliveryWindowOneValueKeys   = oneValueQueryKeys(deliveryWindowQuery{})
	deliveryPipelineOneValueKeys = oneValueQueryKeys(deliveryPipelineQuery{})
)

// parseDeliveryWindowQuery reads the window of GET /api/v1/delivery/timeline's or
// GET /api/v1/delivery/measures's query (parseDeliveryWindow), refusing first a query that names
// one of the keys those routes read one value of, from, to or q, more than once.
func parseDeliveryWindowQuery(query url.Values) (from, to time.Time, err error) {
	if err := repeatedQueryKey(query, deliveryWindowOneValueKeys); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return parseDeliveryWindow(query)
}

// parseOptionalRFC3339 is query's name parameter as an RFC3339 time of the UTC years 1 to 9999
// (parseRFC3339), or fallback when the query has no such parameter.
func parseOptionalRFC3339(query url.Values, name string, fallback time.Time) (time.Time, error) {
	if !query.Has(name) {
		return fallback, nil
	}
	return parseRFC3339(name, query.Get(name))
}

// parseRFC3339 is text, given for the query parameter name, as an RFC3339 time, refusing a value
// that does not parse, or one whose UTC year is outside 1 to 9999 (R18); its error is the text of
// the route's 400 INVALID_QUERY, naming the parameter. RFC 3339 writes the years 0000 to 9999 with
// any offset, so 9999-12-31T23:00:00-14:00 parses and is a moment of the year 10000, which no JSON
// answer can write: a route that echoed it answered an empty body.
func parseRFC3339(name, text string) (time.Time, error) {
	value := strings.TrimSpace(text)
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC3339 timestamp", name)
	}
	if year := parsed.UTC().Year(); year < 1 || year > 9999 {
		return time.Time{}, fmt.Errorf("%s must be a moment of the UTC years 1 to 9999, and %s is one of the year %d", name, value, year)
	}
	return parsed, nil
}

// deliveryNotConfiguredCode and deliveryNotConfiguredMessage are the one refusal a Dispatch with
// no delivery-on project gives, from this route and from the live hub's worker alike.
const (
	deliveryNotConfiguredCode    = "DELIVERY_NOT_CONFIGURED"
	deliveryNotConfiguredMessage = "delivery is not configured: no project has delivery on; turn it on through PUT /api/v1/projects/{key}/settings/delivery"
)

// parseOptionalWindow is a record route's [from, to) window (parseWindow).
func parseOptionalWindow(query url.Values) (from, to *time.Time, err error) {
	return parseWindow(windowBound{name: "from", text: query.Get("from")}, windowBound{name: "to", text: query.Get("to")})
}

// windowBound is one bound of a route's window: the name of its query parameter and the text the
// query gives it. The caller reads the text with its literal name, which the query-key walk
// (routes_query_keys_test.go) resolves.
type windowBound struct{ name, text string }

// parseWindow is a route's window, [since, until): each bound an RFC3339 time, nil where its text
// is blank, and since before until where both are given, as the caller wrote them. Its error is
// the text of the route's 400 INVALID_QUERY, naming the bound.
func parseWindow(sinceBound, untilBound windowBound) (since, until *time.Time, err error) {
	if since, err = sinceBound.parsed(); err != nil {
		return nil, nil, err
	}
	if until, err = untilBound.parsed(); err != nil {
		return nil, nil, err
	}
	if since != nil && until != nil && !since.Before(*until) {
		return nil, nil, fmt.Errorf("%s must be before %s", sinceBound.name, untilBound.name)
	}
	return since, until, nil
}

// parsed is the bound as an RFC3339 time, nil where its text is blank.
func (bound windowBound) parsed() (*time.Time, error) {
	if strings.TrimSpace(bound.text) == "" {
		return nil, nil
	}
	parsed, err := parseRFC3339(bound.name, bound.text)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// readDeliveryRules reads every project's delivery settings in q and answers the rules the
// deployment-wide delivery reads read them by (dispatch://INFRA-978, D9): the rule of every
// github_workflow source of a delivery-on project, each repository under its own (delivery.DeployRules),
// and the union of their populations. A delivery-off project's sources are none of them, so its
// repository's runs are read nowhere. Where no project has delivery on, it answers
// delivery.ErrNoSettings, which the routes and the live views refuse as DELIVERY_NOT_CONFIGURED. A
// delivery-on project whose sources are an events source alone gives no repository: its read counts
// the population's pull requests and no run.
//
// Each source repository's rule is the one its stored day rows stand built under
// (evals.BuiltSettingsIn): the production rule and the deploy cards' jobs as the refresher recorded
// them with the repository's rows (eval_days_rules), and the saved ones where it has recorded none
// yet. The Evals tab reads the same, so between a save of a source's rule and the refresher's commit
// that brings the rows to it the timeline, the measures, the Pipeline and a deploy run's drill-down
// answer the old rule, and from that commit on the new one: one page shows one state. Which
// repositories they read, the population and every field the record does not keep (the workflows,
// the bookkeeping) are the saved settings'.
func readDeliveryRules(ctx context.Context, q delivery.Querier) (*delivery.SourceRules, error) {
	projects, err := delivery.ListProjectSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(projects, func(project delivery.ProjectSettings) bool { return project.Delivery }) {
		return nil, delivery.ErrNoSettings
	}
	saved := delivery.DeployRules(projects)
	repos := make([]string, len(saved))
	for i, rule := range saved {
		repos[i] = rule.DeployRepo
	}
	built, err := evals.BuiltSettingsIn(ctx, q, repos)
	if err != nil {
		return nil, err
	}
	recorded := make(map[string]*delivery.DeliverySettings, len(built))
	for i, rule := range built {
		recorded[delivery.RuleKey(repos[i])] = rule
	}
	for _, project := range projects {
		if !project.Delivery {
			continue
		}
		for i, source := range project.Sources {
			repo, ok := delivery.SourceRepoOf(source, project.DefaultRepo)
			if !ok {
				continue
			}
			if rule := recorded[delivery.RuleKey(repo)]; rule != nil {
				source.ProductionJobNames, source.ProductionApplyStepName = rule.ProductionJobNames, rule.ProductionApplyStepName
				source.StagingE2EJobName, source.StagingApplyJobName = rule.StagingE2EJobName, rule.StagingApplyJobName
				project.Sources[i] = source
			}
		}
	}
	return delivery.NewSourceRules(projects), nil
}

// deliveryRules reads the delivery reads' rules (readDeliveryRules), answering 404
// DELIVERY_NOT_CONFIGURED (or the store's error) and false when it cannot.
func (s *server) deliveryRules(w http.ResponseWriter, r *http.Request) (*delivery.SourceRules, bool) {
	rules, err := readDeliveryRules(r.Context(), s.deps.Store.Pool)
	if err != nil {
		if errors.Is(err, delivery.ErrNoSettings) {
			writeError(w, deliveryNotConfiguredCode, http.StatusNotFound, deliveryNotConfiguredMessage)
			return nil, false
		}
		s.writeHandlerError(w, err)
		return nil, false
	}
	return rules, true
}

// deliveryApplies is the deploys among runs, every source repository's runs judged by that
// repository's own rule (delivery.ProductionApplies), by the repository's delivery.RuleKey. A run
// of a repository no rule reads is judged by none: the reads read no such run.
func deliveryApplies(rules *delivery.SourceRules, runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob) map[string][]delivery.Apply {
	byRepo := map[string][]delivery.DeliveryRun{}
	for _, run := range runs {
		key := delivery.RuleKey(run.Repo)
		byRepo[key] = append(byRepo[key], run)
	}
	applies := make(map[string][]delivery.Apply, len(byRepo))
	for key, repoRuns := range byRepo {
		if rule, ok := rules.For(key); ok {
			applies[key] = delivery.ProductionApplies(repoRuns, jobsByRun, rule.Production()).Applies
		}
	}
	return applies
}

// deliveryContainingRun is the deploy that ships pr first (delivery.ContainingRun) among applies, the
// deploys of every source repository (deliveryApplies), and the repository that tracks it: pr's own
// where a source reads it, and "" where none does, whose pull request no deploy ships
// (delivery.ComputeDeployedStatus answers it not tracked).
func deliveryContainingRun(rules *delivery.SourceRules, pr delivery.DeliveryPullRequest, applies map[string][]delivery.Apply) (*delivery.Apply, string) {
	if _, tracked := rules.For(pr.Repo); !tracked {
		return nil, ""
	}
	return delivery.ContainingRun(pr, pr.Repo, applies[delivery.RuleKey(pr.Repo)]), pr.Repo
}

// deliveryEvals reads the evals of slugs whose targets the deployment-wide delivery answers judge
// by (dispatch://INFRA-978, D9): the project's whose deploy sources read every source repository,
// where one delivery-on project owns them all (evals.Store.DeployRepoEvals of any of them), and
// the catalogue's, whose targets are main's defaults, where two or more projects own them or none
// is read, since no one project's targets judge figures of several projects.
func (s *server) deliveryEvals(ctx context.Context, rules *delivery.SourceRules, slugs []string) (map[string]evals.Entry, error) {
	repo := ""
	if all := rules.All(); len(all) > 0 && !slices.ContainsFunc(all, func(rule delivery.DeliverySettings) bool { return rule.Project != all[0].Project }) {
		repo = all[0].DeployRepo
	}
	return s.registry().DeployRepoEvals(ctx, repo, slugs)
}

// getDeliveryTimeline answers GET /api/v1/delivery/timeline: merges, deploys, pipeline failures
// and waiting-to-deploy PRs within [from, to) and the given facets and search, computed from
// stored facts at request time (deployed_status, root_failing_job, the Parent agent and session
// labels, the issue facts and every count are never stored -- "the server computes every
// measure from stored facts on request"), and the measures of that same window and selection, so a
// page showing both reads the population once. GET /api/v1/delivery/measures is the measures
// alone, for another window. The answer is readDeliveryPopulation's read and
// deliveryTimelineCompute's compute, the two halves the live hub calls too, so a live page's
// snapshot is this body.
func (s *server) getDeliveryTimeline(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	from, to, err := parseDeliveryWindowQuery(query)
	if err != nil {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, err.Error())
		return
	}
	rules, ok := s.deliveryRules(w, r)
	if !ok {
		return
	}
	population, err := s.readDeliveryPopulation(r.Context(), rules, from, to, query, true)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, deliveryTimelineCompute(rules, population, from, to, time.Now().UTC()))
}

// deliveryTimelineCompute is GET /api/v1/delivery/timeline's body (deliveryTimelineWithMeasures)
// for [from, to) from a population read for that window or narrowed to it, each source
// repository's runs judged by its own rule of rules: the timeline (deliveryTimelineOf) and the
// window's measures beside it. It reads nothing; now is only the measures' computed_at.
func deliveryTimelineCompute(rules *delivery.SourceRules, pop deliveryPopulation, from, to, now time.Time) deliveryTimelineWithMeasures {
	derived := pop.derive(rules)
	return deliveryTimelineWithMeasures{
		DeliveryTimelineResponse: deliveryTimelineOf(rules, pop, derived, from, to),
		Measures:                 deliveryMeasuresOver(rules, pop, derived, from, to, now),
	}
}

// deliveryTimelineOf is the timeline of [from, to) from a population and what derive computed
// from it: its merges under the facets and the search, the pull requests waiting at its start
// under the same, its deploy runs, and the counts and labels beside them. GET
// /api/v1/delivery/timeline answers it with the measures (deliveryTimelineCompute), and GET
// /api/v1/projects/{key}/timeline pages it over one project's population (projectTimelineCompute).
func deliveryTimelineOf(rules *delivery.SourceRules, pop deliveryPopulation, derived deliveryDerived, from, to time.Time) delivery.DeliveryTimelineResponse {
	// A run's prs[] is every window pull request it shipped first, whatever the facets: the page
	// sizes a deploy and lists its drill-down by everything it shipped.
	prViews := make([]delivery.DeliveryPRView, 0, len(derived.rows))
	for _, row := range derived.rows {
		if row.matches(derived.selection, "") {
			prViews = append(prViews, row.view)
		}
	}

	// The pull requests still waiting at `from`, under the same facets and search, so the
	// waiting line starts the window at their count rather than at zero.
	waitingViews := make([]delivery.DeliveryWaitingPRView, 0, len(derived.waiting))
	for _, waiting := range derived.waiting {
		if waiting.row.matches(derived.selection, "") {
			waitingViews = append(waitingViews, waiting.view)
		}
	}

	runViews := make([]delivery.DeliveryRunView, 0, len(pop.applyRuns))
	for _, run := range pop.applyRuns {
		if run.StartedAt.Before(from) || !run.StartedAt.Before(to) {
			continue
		}
		jobs := pop.jobsByRun[run.RunID]
		rule, _ := rules.For(run.Repo)
		shipped := derived.shipped[run.RunID]
		if shipped == nil {
			// Always a slice, never nil, so it always serializes as `[]`, not `null` -- the
			// common case for any successful deploy that shipped no in-window population PR.
			shipped = []delivery.DeliveryShippedPRView{}
		}
		runViews = append(runViews, delivery.DeliveryRunView{
			ID: run.RunID, URL: run.URL, HeadSHA: run.HeadSHA, HeadAt: run.HeadCommitAt,
			StartedAt: run.StartedAt, CompletedAt: run.CompletedAt, Conclusion: run.Conclusion,
			Production:     deliveryProductionView(jobs, rule.Production()),
			FailedJobs:     deliveryJobViews(delivery.FailedJobs(jobs)),
			RootFailingJob: deliveryJobView(delivery.RootFailingJob(jobs)),
			PRs:            shipped,
		})
	}
	// Listed by head commit time, then id, which the page's live merge mirrors
	// (applyDelta.ts's ORDERS); applyRuns itself is in creation order (readDeliveryRows).
	slices.SortFunc(runViews, func(a, b delivery.DeliveryRunView) int {
		return cmp.Or(a.HeadAt.Compare(b.HeadAt), cmp.Compare(a.ID, b.ID))
	})

	return delivery.DeliveryTimelineResponse{
		Window:          delivery.DeliveryWindowView{From: from, To: to},
		PRs:             prViews,
		Waiting:         waitingViews,
		Runs:            runViews,
		FacetCounts:     deliveryFacetCounts(derived.rows, derived.selection),
		ColorCounts:     deliveryColorCounts(derived.rows),
		Components:      derived.components,
		IssueTitles:     derived.issueTitles,
		Freshness:       derived.freshness,
		SessionGroups:   pop.groups,
		SessionTitles:   derived.sessionTitles,
		SessionPRCounts: derived.sessionPRCounts,
	}
}

// deliveryTimelineWithMeasures is the timeline's wire shape: delivery.DeliveryTimelineResponse
// with the window's measures beside it (packages/contracts/src/dispatch-api.ts's
// DeliveryTimelineResponse.measures). The measures' type lives in this package, since the
// measures package imports delivery, so the field is added here rather than in delivery/model.go.
type deliveryTimelineWithMeasures struct {
	delivery.DeliveryTimelineResponse
	Measures deliveryMeasuresResponse `json:"measures"`
}

// deliveryPopulation is every read the timeline's and the measures' answers for one window and
// query make, as stored, before anything is computed from them: the population pull requests
// merged in the window, the source repositories' pull requests still waiting at its start, the
// source repositories' deploy runs on main GitHub created from its start (containment's input) and
// those started inside it (the measures'), each with its jobs, the open unowned P0 issues, the
// session groups, and, where the answer shows them, the titles of the sessions and the facts of the
// issues those pull requests name, and the components of those issues' projects. Each source
// repository's runs are read under its own rule and held together, every run carrying its
// repository, by which the compute judges it by that rule again (deliveryApplies). Nothing that
// computes from it reads again (derive), so the live hub keeps it and computes a later window from
// it narrowed (narrowedTo) without a read.
type deliveryPopulation struct {
	query url.Values
	// prs is every population pull request merged in the window, oldest merge first.
	prs []delivery.DeliveryPullRequest
	// withWaiting is whether waiting was read: the source repositories' pull requests merged
	// before the window that had not shipped by its start, oldest merge first, which only the
	// timeline reads.
	withWaiting bool
	waiting     []delivery.WaitingPullRequest
	// applyRuns is every source repository's deploy runs on main GitHub created from the window's
	// start up to the bound past which no run ships a pull request merged before its end first
	// (delivery.ReadWindowRuns; no bound for a window ending at the present), oldest creation
	// first, and jobsByRun their jobs: the production jobs of each, and in the timeline's read
	// (withWaiting), which lists the runs with their failed jobs, each of their jobs that failed too
	// (readDeliveryRows).
	applyRuns []delivery.DeliveryRun
	jobsByRun map[int64][]delivery.DeliveryRunJob
	// windowRuns is every source repository's deploy runs on main started in the window, oldest
	// start first, and windowJobs their production jobs: the measures' deploys. windowShipped is the
	// population pull requests each of the window's deploys ships first, whenever they merged
	// (delivery.Deploys.ShippedBy under its repository's rule): the deploys with pull requests, and
	// the projects each counts for (projectTimelinePopulation). It is taken once, when the
	// population is read, so a narrowed population keeps what a deploy ships whatever window holds
	// it.
	windowRuns    []delivery.DeliveryRun
	windowJobs    map[int64][]delivery.DeliveryRunJob
	windowShipped map[int64][]*delivery.DeliveryPullRequest
	unownedP0     []delivery.UnownedIssue
	// targetEvals is the evals whose targets the measures judge them by (evals.DeliveryTargetSlugs,
	// deliveryEvals), read with the window's rows, so a narrowed population judges by the targets
	// it was read with.
	targetEvals map[string]evals.Entry
	// failures is the three failure measures' stored parts of the window's days, with their first
	// day, of the one delivery-on project (evals.Store.DeploymentFailures), none where there is not
	// exactly one: the measures fold the parts of the days they answer (deliveryMeasuresOver), so a
	// narrowed population keeps them.
	failures evals.DeploymentFailures
	groups   []delivery.SessionGroup
	// titles, issues and components are the session titles, the facts of the issues the pull
	// requests name and the components of those issues' projects, read where the population is
	// labeled and nil elsewhere.
	titles     map[string]string
	issues     map[string]deliveryIssue
	components map[string]delivery.DeliveryComponentView
	// unfetchable is how many population pull requests can no longer be fetched from GitHub.
	unfetchable int
}

// labeled is whether titles, issues and components were read: the session titles, the facts of
// the issues the pull requests name and the components of those issues' projects, which label
// the timeline's rows and decide which rows a facet or the search selects. A read that lists no
// waiting line (the measures, the Pipeline's lead times) and selects nothing shows none of them,
// so it reads none (readDeliveryRows), and each is nil.
func (p deliveryPopulation) labeled() bool {
	return p.withWaiting || parseDeliverySelection(p.query, nil).selects()
}

// deliveryDerived is what the timeline and the measures compute from a deliveryPopulation: each
// pull request with its view and its facet values, the request's search and facet selection, the
// waiting rows, which window pull requests each deploy run shipped first, the labels of every
// component and issue the pull requests name, and the freshness.
type deliveryDerived struct {
	// rows is every population pull request merged in the window, before the facets, in merge
	// order: the timeline counts facets and colours over it. A row passing
	// row.matches(selection, "") is one the timeline's prs[] shows and the measures' pull-request
	// figures count.
	rows []deliveryRow
	// shipped is, for each deploy run, every row whose first shipping apply it is, whatever the
	// facets: the timeline sizes a deploy and lists its drill-down by it.
	shipped     map[int64][]delivery.DeliveryShippedPRView
	selection   deliverySelection
	waiting     []deliveryWaitingRow
	components  map[string]delivery.DeliveryComponentView
	issueTitles map[string]string
	// sessionTitles and sessionPRCounts are, for each session that is the first session of a
	// window pull request (the sessions a Parent agent can be), its title (or its id when it has
	// none) and how many window pull requests it is first on, before the facets.
	sessionTitles   map[string]string
	sessionPRCounts map[string]int
	// freshness is the timeline's and the measures' freshness object: the source repositories'
	// intake and reconcile times and last reconcile error, folded by repository
	// (delivery.FreshnessOf), and how many population pull requests can no longer be fetched from
	// GitHub.
	freshness delivery.DeliveryFreshnessView
}

// deliveryWaitingRow is one pull request still waiting at the window's start: its facet values, so
// the timeline's waiting line counts it under the same facets and search, and its waiting view.
type deliveryWaitingRow struct {
	row  deliveryRow
	view delivery.DeliveryWaitingPRView
}

// readDeliveryPopulation is every read the timeline's and the measures' answers for one window and
// query make (the population pull requests, each source repository's deploy runs on main GitHub
// created at or after `from` with their jobs, the measures' runs started in the window with their
// jobs, the unowned P0 list, the evals whose targets judge the measures and the failure measures'
// stored parts); nothing that computes from it reads again. With withWaiting it also reads each
// source repository's pull requests still waiting at `from` (ListPullRequestsWaitingAt), resolved
// through the same issue and session lookups so the timeline's waiting line filters them alike;
// the measures read none. withWaiting is the timeline's read, the one that lists the deploy runs,
// so it is also the one that reads the failed jobs of the runs GitHub created from `from` on beside
// their production jobs; every other read takes their production jobs alone (readDeliveryRows).
func (s *server) readDeliveryPopulation(ctx context.Context, rules *delivery.SourceRules, from, to time.Time, query url.Values, withWaiting bool) (deliveryPopulation, error) {
	return s.readDeliveryRows(ctx, rules, from, to, query, withWaiting, true)
}

// readDeliveryRows is readDeliveryPopulation, and with withMeasures false leaves out the measures'
// runs, unowned P0 list and target evals, which the Pipeline page's lead times do not read. Each
// source repository of rules is read under its own rule, and the population holds their rows
// together (dispatch://INFRA-978, D9): the runs of several repositories in creation order, the
// window's in start order and the waiting pull requests in merge order, each list as one
// repository's is ordered.
func (s *server) readDeliveryRows(ctx context.Context, rules *delivery.SourceRules, from, to time.Time, query url.Values, withWaiting, withMeasures bool) (deliveryPopulation, error) {
	pool := s.deps.Store.Pool
	pop := deliveryPopulation{query: query, withWaiting: withWaiting, jobsByRun: map[int64][]delivery.DeliveryRunJob{}, windowJobs: map[int64][]delivery.DeliveryRunJob{}}
	sources := rules.All()

	// Independent reads, side by side. They never shared a transaction (each is its own
	// statement on the pool, or, the target evals' read, a read-only snapshot of its own,
	// evals.Store.DeployRepoEvals), so running them at once loses no consistency they had. Each
	// takes one connection under a hold mark of its own (store.ForConcurrentRead): on the
	// request's shared mark one read's open cursor would refuse the rest. The run jobs depend on
	// the runs, so they follow the runs within the runs' own read.
	//
	// A timeline request of one source repository therefore holds up to nine of the shared pool's
	// connections (store.sharedPoolSize), and three more for each further source repository, for the
	// length of its longest read (each repository's runs read their two lists of jobs side by side,
	// and its earlier rows their pull requests beside their runs, within their own read). That is
	// fine because none of them takes a lock past the one any read takes of the tables it reads, a
	// read-only snapshot no more than a statement does, so none waits on another caller or makes
	// another caller wait on it: the deadlock the pool's guard exists to prevent needs a caller
	// already holding a connection, which ForConcurrentRead refuses. A read past the pool's free
	// connections waits for one, as any request does.
	applies := make([]delivery.RunsWithJobs, len(sources))
	deploys := make([]delivery.Deploys, len(sources))
	waiting := make([][]delivery.WaitingPullRequest, len(sources))
	independent := []func(ctx context.Context) error{
		func(ctx context.Context) (err error) {
			pop.prs, err = delivery.ListPullRequestsInWindow(ctx, pool, from, to)
			return err
		},
		func(ctx context.Context) (err error) {
			pop.unfetchable, err = delivery.CountUnfetchablePullRequests(ctx, pool)
			return err
		},
		func(ctx context.Context) (err error) {
			pop.groups, err = delivery.ListSessionGroups(ctx, pool)
			return err
		},
	}
	for i, rule := range sources {
		// Every deploy run on main of the repository GitHub created from `from` up to the bound
		// past which no run ships a pull request merged before `to` first (delivery.MergedRows
		// says why not cut at `to`; no bound for a window ending at the present) and, for the
		// measures, the window's deploys with the rows before the window their shipped sets read,
		// read by the rules the eval registry's day rows read by. Containment and the measures
		// judge a run by its production jobs alone (delivery.Production.JudgedJobNames says why
		// that judges every run as its every job would); the timeline lists each deploy run it
		// shows with its failed jobs and the job that failed first, which can be any job the run
		// holds, so its read takes every failed job beside them (delivery.FailedJobs and
		// RootFailingJob keep the failed jobs alone, so they answer from these what they answer
		// from every job). The timeline's runs[] list is filtered to [from, to) separately.
		independent = append(independent, func(ctx context.Context) (err error) {
			applies[i], deploys[i], err = delivery.ReadWindowRuns(ctx, pool, rule, from, to, withWaiting, withMeasures)
			return err
		})
		if withWaiting {
			// The repository's pull requests still waiting at `from`, so the waiting line starts
			// the window at their count rather than at zero.
			independent = append(independent, func(ctx context.Context) (err error) {
				waiting[i], err = delivery.ListPullRequestsWaitingAt(ctx, pool, rule.DeployRepo, rule.Production(), from)
				return err
			})
		}
	}
	if withMeasures {
		independent = append(independent,
			// Every project's: the delivery measures follow no project.
			func(ctx context.Context) (err error) {
				pop.unownedP0, err = delivery.ListUnownedP0(ctx, pool, "")
				return err
			},
			func(ctx context.Context) (err error) {
				pop.targetEvals, err = s.deliveryEvals(ctx, rules, evals.DeliveryTargetSlugs)
				return err
			},
			func(ctx context.Context) (err error) {
				pop.failures, err = s.registry().DeploymentFailures(ctx, evalDaysOf(from, to))
				return err
			})
		pop.windowShipped = map[int64][]*delivery.DeliveryPullRequest{}
	}
	if err := store.ReadSideBySide(ctx, independent...); err != nil {
		return deliveryPopulation{}, err
	}
	for i, rule := range sources {
		pop.applyRuns = append(pop.applyRuns, applies[i].Runs...)
		maps.Copy(pop.jobsByRun, applies[i].Jobs)
		pop.waiting = append(pop.waiting, waiting[i]...)
		if withMeasures {
			pop.windowRuns = append(pop.windowRuns, deploys[i].Started.Runs...)
			maps.Copy(pop.windowJobs, deploys[i].Started.Jobs)
			maps.Copy(pop.windowShipped, deploys[i].ShippedBy(delivery.MergedRows{PullRequests: pop.prs, ApplyRuns: applies[i].Runs, ApplyJobs: applies[i].Jobs}, rule))
		}
		// Warned here, once per run, rather than in the compute, which runs again on every expiry.
		s.productionApplies(ctx, applies[i].Runs, applies[i].Jobs, rule.Production())
		s.productionApplies(ctx, deploys[i].Started.Runs, deploys[i].Started.Jobs, rule.Production())
	}
	if len(sources) > 1 {
		slices.SortStableFunc(pop.applyRuns, func(a, b delivery.DeliveryRun) int { return a.Created().Compare(b.Created()) })
		slices.SortStableFunc(pop.windowRuns, func(a, b delivery.DeliveryRun) int { return a.StartedAt.Compare(b.StartedAt) })
		slices.SortStableFunc(pop.waiting, func(a, b delivery.WaitingPullRequest) int { return a.MergedAt.Compare(*b.MergedAt) })
	}

	// The labels are the timeline's, and a selection's: the measures and the lead times answer
	// the same without them where nothing is selected, so those reads leave them out. They cost
	// the 84-day compare range's measures 6 to 17 ms of their 69 to 88 ms of database time at
	// INFRA's size (dispatch://INFRA-2916, R8).
	if !pop.labeled() {
		return pop, nil
	}
	if err := readDeliveryLabels(ctx, pool, &pop); err != nil {
		return deliveryPopulation{}, err
	}
	return pop, nil
}

// readDeliveryLabels reads into pop, in q, what labels its pull requests and waiting entries and
// decides which of them a facet or the search selects: the titles of the sessions they name, the
// facts of the issues they name and the components of those issues' projects. The delivery routes
// read them on the pool (readDeliveryRows) and an eval list read narrowed by a facet in its
// snapshot (evalNarrowing.keep).
func readDeliveryLabels(ctx context.Context, q delivery.Querier, pop *deliveryPopulation) error {
	sessionIDs := []string{}
	issueKeys := []string{}
	for _, pr := range pop.prs {
		sessionIDs = append(sessionIDs, pr.Sessions...)
		if pr.IssueKey != nil {
			issueKeys = append(issueKeys, *pr.IssueKey)
		}
	}
	for _, pr := range pop.waiting {
		sessionIDs = append(sessionIDs, pr.Sessions...)
		if pr.IssueKey != nil {
			issueKeys = append(issueKeys, *pr.IssueKey)
		}
	}
	var err error
	if pop.titles, err = delivery.ResolveSessionTitles(ctx, q, sessionIDs); err != nil {
		return err
	}
	if pop.issues, err = deliveryIssueFacts(ctx, q, issueKeys); err != nil {
		return err
	}
	pop.components, err = deliveryComponents(ctx, q, deliveryIssueProjects(pop.issues))
	return err
}

// deliveryIssueProjects is the projects issues belong to, once each.
func deliveryIssueProjects(issues map[string]deliveryIssue) []string {
	projects := []string{}
	for _, issue := range issues {
		if !slices.Contains(projects, issue.project) {
			projects = append(projects, issue.project)
		}
	}
	return projects
}

// derive computes, from the population alone, each pull request's deploy from its own source
// repository's deploys (deliveryApplies, deliveryContainingRun), its issue's facts, its Parent agent
// and its facet values, the waiting rows, and the labels and selection the timeline reads beside
// them. The issue titles and components are those of the issues the population's pull requests
// name, so a narrowed population derives what a fresh read of its window would.
func (p deliveryPopulation) derive(rules *delivery.SourceRules) deliveryDerived {
	applies := deliveryApplies(rules, p.applyRuns, p.jobsByRun)
	parentAgents := delivery.NewParentAgents(p.groups, p.titles)
	sessionTitles := map[string]string{}
	sessionPRCounts := map[string]int{}
	issueTitles := map[string]string{}
	projects := map[string]bool{}
	nameIssue := func(pr delivery.DeliveryPullRequest) {
		if pr.IssueKey == nil {
			return
		}
		if issue, found := p.issues[*pr.IssueKey]; found {
			issueTitles[*pr.IssueKey] = issue.title
			projects[issue.project] = true
		}
	}
	for _, pr := range p.prs {
		nameIssue(pr)
		if len(pr.Sessions) == 0 {
			continue
		}
		first := pr.Sessions[0]
		sessionPRCounts[first]++
		sessionTitles[first] = delivery.SessionTitle(p.titles, first)
	}
	for _, pr := range p.waiting {
		nameIssue(pr.DeliveryPullRequest)
	}
	components := map[string]delivery.DeliveryComponentView{}
	for key, component := range p.components {
		if project, _, _ := strings.Cut(key, "/"); projects[project] {
			components[key] = component
		}
	}

	// viewOf is a pull request as the timeline shows it, shipped by deployRun at deployedAt.
	viewOf := func(pr delivery.DeliveryPullRequest, status delivery.DeployedStatus, deployRun *int64, deployedAt *time.Time) delivery.DeliveryPRView {
		view := delivery.DeliveryPRView{
			ID: pr.Repo + "#" + strconv.Itoa(pr.Number), Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL,
			Author: pr.Author, CreatedAt: pr.CreatedAt, MergedAt: pr.MergedAt, FirstCommitAt: pr.FirstCommitAt,
			Additions: pr.Additions, Deletions: pr.Deletions, Partial: pr.Partial, Rework: pr.Rework,
			Issue: pr.IssueKey, Components: []string{}, Sessions: pr.Sessions,
			DeployRun: deployRun, DeployedAt: deployedAt, DeployedStatus: status,
			UnfetchableReason: pr.UnfetchableReason,
		}
		if pr.IssueKey != nil {
			if issue, found := p.issues[*pr.IssueKey]; found {
				view.IssueTitle = &issue.title
				view.Components = issue.components
				if issue.priority != nil {
					label := "P" + strconv.Itoa(*issue.priority)
					view.Priority = &label
				}
			}
		}
		if len(pr.Sessions) > 0 {
			agent := parentAgents.Value(pr.Sessions[0])
			view.ParentAgent = &agent
		}
		return view
	}

	rows := make([]deliveryRow, 0, len(p.prs))
	shipped := map[int64][]delivery.DeliveryShippedPRView{}
	for _, pr := range p.prs {
		apply, tracked := deliveryContainingRun(rules, pr, applies)
		var deployRun *int64
		var deployedAt *time.Time
		if apply != nil {
			runID, completedAt := apply.RunID, apply.CompletedAt
			deployRun, deployedAt = &runID, &completedAt
		}
		view := viewOf(pr, delivery.ComputeDeployedStatus(pr, tracked, apply), deployRun, deployedAt)
		if deployRun != nil {
			shipped[*deployRun] = append(shipped[*deployRun], delivery.DeliveryShippedPRView{ID: view.ID, Title: view.Title})
		}
		rows = append(rows, newDeliveryRow(view))
	}

	waitingRows := make([]deliveryWaitingRow, 0, len(p.waiting))
	for _, pr := range p.waiting {
		status := delivery.DeployedStatusWaiting
		if pr.DeployRun != nil {
			status = delivery.DeployedStatusDeployed
		}
		waitingRows = append(waitingRows, deliveryWaitingRow{
			row:  newDeliveryRow(viewOf(pr.DeliveryPullRequest, status, pr.DeployRun, pr.DeployedAt)),
			view: delivery.DeliveryWaitingPRView{MergedAt: *pr.MergedAt, DeployedAt: pr.DeployedAt},
		})
	}

	return deliveryDerived{
		rows:            rows,
		shipped:         shipped,
		selection:       parseDeliverySelection(p.query, components),
		waiting:         waitingRows,
		components:      components,
		issueTitles:     issueTitles,
		sessionTitles:   sessionTitles,
		sessionPRCounts: sessionPRCounts,
		freshness:       delivery.FreshnessOf(rules.All(), p.unfetchable),
	}
}

// narrowedTo is the population for [from', to') with from' at or after the start it was read for.
// A row merged before from' leaves the window, and joins the waiting entries when a source
// repository's rule reads its repo and it had not shipped by from' (ListPullRequestsWaitingAt reads
// only a source repository's pull requests; the window's rows hold every population repo). A
// waiting entry that shipped before from' leaves them. A run started before from', or one GitHub
// created before from', leaves the lists that filter on that time. The maps keep only the keys the
// remaining rows and runs name (derive reads them by those keys). This is
// ListPullRequestsWaitingAt's rule: a pull request shipped by from' exactly when its ship time (the
// earliest successful production job of a run of its repository created after its merge) is
// before from'. A leaving row's ship is read from the runs read here, which hold every run created
// after its merge that can ship it first (delivery.ReadWindowRuns), since it merged at or after
// the start they were read from. The window's end moves with from', past the read's end: a
// preset's read ends at the present, so nothing stored lies past it.
func (p deliveryPopulation) narrowedTo(rules *delivery.SourceRules, from time.Time) deliveryPopulation {
	narrowed := p
	narrowed.prs = make([]delivery.DeliveryPullRequest, 0, len(p.prs))
	var left []delivery.DeliveryPullRequest
	for _, pr := range p.prs {
		if pr.MergedAt.Before(from) {
			left = append(left, pr)
			continue
		}
		narrowed.prs = append(narrowed.prs, pr)
	}
	narrowed.applyRuns, narrowed.jobsByRun = runsWhere(p.applyRuns, p.jobsByRun, func(run delivery.DeliveryRun) bool { return !run.Created().Before(from) })
	narrowed.windowRuns, narrowed.windowJobs = runsWhere(p.windowRuns, p.windowJobs, func(run delivery.DeliveryRun) bool { return !run.StartedAt.Before(from) })
	if !p.withWaiting {
		return narrowed
	}
	narrowed.waiting = make([]delivery.WaitingPullRequest, 0, len(p.waiting)+len(left))
	for _, waiting := range p.waiting {
		if waiting.DeployedAt == nil || !waiting.DeployedAt.Before(from) {
			narrowed.waiting = append(narrowed.waiting, waiting)
		}
	}
	applies := deliveryApplies(rules, p.applyRuns, p.jobsByRun)
	for _, pr := range left {
		apply, tracked := deliveryContainingRun(rules, pr, applies)
		if tracked == "" {
			continue
		}
		waiting := delivery.WaitingPullRequest{DeliveryPullRequest: pr}
		if apply != nil {
			if apply.CompletedAt.Before(from) {
				continue
			}
			runID, completedAt := apply.RunID, apply.CompletedAt
			waiting.DeployRun, waiting.DeployedAt = &runID, &completedAt
		}
		narrowed.waiting = append(narrowed.waiting, waiting)
	}
	return narrowed
}

// runsWhere is the runs keep holds, in their order, with only their jobs.
func runsWhere(runs []delivery.DeliveryRun, jobs map[int64][]delivery.DeliveryRunJob, keep func(delivery.DeliveryRun) bool) ([]delivery.DeliveryRun, map[int64][]delivery.DeliveryRunJob) {
	kept := make([]delivery.DeliveryRun, 0, len(runs))
	keptJobs := make(map[int64][]delivery.DeliveryRunJob, len(runs))
	for _, run := range runs {
		if !keep(run) {
			continue
		}
		kept = append(kept, run)
		if runJobs, ok := jobs[run.RunID]; ok {
			keptJobs[run.RunID] = runJobs
		}
	}
	return kept, keptJobs
}

// nextExpiry is the first moment after now at which a preset window of length n, ending at the
// present, loses something the population holds or its daily series moves: just past the oldest
// row's merge + n, the oldest run start (each list that filters on starts) + n, the oldest creation
// among the apply runs delivery.ReadWindowRuns read + n, and each waiting entry's ship time + n;
// and the next UTC midnight (measures.DailySeries's days, which measures' windowDays lays out),
// where a whole-day window's first and last days move together. Nothing else in the timeline or
// the measures reads the present (the LEGION-690 PR's Task 0 checks, Step 17).
func nextExpiry(pop deliveryPopulation, n time.Duration, now time.Time) time.Time {
	next := nextUTCMidnight(now)
	consider := func(leaves time.Time) {
		// A row is in the window while its time is at or after the start, so it leaves just past
		// its time + n.
		if at := leaves.Add(n).Add(time.Nanosecond); at.After(now) && at.Before(next) {
			next = at
		}
	}
	for _, pr := range pop.prs {
		consider(*pr.MergedAt)
	}
	for _, run := range pop.applyRuns {
		consider(run.Created())
		consider(run.StartedAt)
	}
	for _, run := range pop.windowRuns {
		consider(run.StartedAt)
	}
	for _, waiting := range pop.waiting {
		if waiting.DeployedAt != nil {
			consider(*waiting.DeployedAt)
		}
	}
	return next
}

// nextUTCMidnight is the first UTC midnight after t.
func nextUTCMidnight(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

// getDeliveryRun answers GET /api/v1/delivery/runs/{id}: one run of a source repository of a
// delivery-on project (readDeliveryRules) with every job it ran (readDeliveryRun,
// deliveryRunDetail), for the drill-down's job lists. A run of a repository no such source reads
// is no run of it, as the timeline lists none; its 404 names the repositories the sources read, or
// says they read none, where every source is an events one.
func (s *server) getDeliveryRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	runID, err := parseDeliveryRunID(r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rules, ok := s.deliveryRules(w, r)
	if !ok {
		return
	}
	repos := make([]string, 0, len(rules.All()))
	for _, rule := range rules.All() {
		repos = append(repos, rule.DeployRepo)
	}
	ctx := r.Context()
	pool := s.deps.Store.Pool
	run, err := readDeliveryRun(ctx, pool, repos, runID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	detail, err := deliveryRunDetail(ctx, pool, run)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, detail)
}

// parseDeliveryRunID reads a run id of the path: a positive integer, else 400 RUN_ID_INPUT.
func parseDeliveryRunID(value string) (int64, error) {
	runID, err := strconv.ParseInt(value, 10, 64)
	if err != nil || runID < 1 {
		return 0, errorf(http.StatusBadRequest, "RUN_ID_INPUT", "run id must be a positive integer")
	}
	return runID, nil
}

// readDeliveryRun reads run runID of the first of repos (by name) that holds one, or answers 404
// NOT_FOUND where none of repos holds such a run: a run id is one repository's.
func readDeliveryRun(ctx context.Context, pool *store.Pool, repos []string, runID int64) (delivery.DeliveryRun, error) {
	run, err := delivery.ScanRun(pool.QueryRow(ctx, `select `+delivery.RunColumns+` from delivery_runs where repo = any($1) and run_id = $2 order by repo limit 1`, repos, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		of := " of " + strings.Join(repos, " or ")
		if len(repos) == 0 {
			of = ": no deploy source of a delivery-on project reads a repository"
		}
		return delivery.DeliveryRun{}, errorf(http.StatusNotFound, "NOT_FOUND", "no run %d%s", runID, of)
	}
	return run, err
}

// deliveryRunDetail is run with every job it ran, in ListRunJobs' order (by when each started, a
// job that never started last, then by name), each with its result and timings: the drill-down's
// job lists, which GET /api/v1/delivery/runs/{id} and GET /api/v1/projects/{key}/deploy-runs/{id}
// both answer.
func deliveryRunDetail(ctx context.Context, pool *store.Pool, run delivery.DeliveryRun) (delivery.DeliveryRunDetailView, error) {
	jobs, err := delivery.ListRunJobs(ctx, pool, run.Repo, run.RunID)
	if err != nil {
		return delivery.DeliveryRunDetailView{}, err
	}
	views := make([]delivery.DeliveryRunJobDetailView, len(jobs))
	for i, job := range jobs {
		views[i] = delivery.DeliveryRunJobDetailView{Name: job.Name, StartedAt: job.StartedAt, CompletedAt: job.CompletedAt, Conclusion: job.Conclusion}
	}
	return delivery.DeliveryRunDetailView{ID: run.RunID, URL: run.URL, Jobs: views}, nil
}

func deliveryJobViews(jobs []delivery.DeliveryRunJob) []delivery.DeliveryRunJobView {
	views := make([]delivery.DeliveryRunJobView, len(jobs))
	for i, job := range jobs {
		views[i] = delivery.DeliveryRunJobView{Name: job.Name, CompletedAt: job.CompletedAt}
	}
	return views
}

func deliveryJobView(job *delivery.DeliveryRunJob) *delivery.DeliveryRunJobView {
	if job == nil {
		return nil
	}
	return &delivery.DeliveryRunJobView{Name: job.Name, CompletedAt: job.CompletedAt}
}

// deliveryProductionView is the run's production job (Production.Job), its conclusion as a deploy
// reads it (Production.Conclusion: skipped for a job that succeeded having applied nothing), or nil
// when the run has none.
func deliveryProductionView(jobs []delivery.DeliveryRunJob, production delivery.Production) *delivery.DeliveryRunProductionView {
	job := production.Job(jobs)
	if job == nil {
		return nil
	}
	return &delivery.DeliveryRunProductionView{Conclusion: production.Conclusion(*job), CompletedAt: job.CompletedAt}
}

// productionApplies is delivery.ProductionApplies for a read. It warns once per run and
// configuration across every process serving the database (firstToWarn) about each successful run
// with no listed production job, naming every listed name and every job the run holds, and about
// each run whose production job lists no step named the configured apply step: the timeline and
// the measures judge the same runs on every read, on whichever process the read reached, so a line
// would otherwise repeat on every page load. A changed list or step is a new key, so a
// misconfiguration is named again under its new value.
//
// Most reads hand it a run's production jobs alone (readDeliveryRows), which judge the run as its
// every job would but leave an unmatched run's own job names out, and those names are what an
// operator fixes the setting from. So the run's every job is read here, once per process the
// warning is logged on, for a run that is normally none. A read that fails still logs the
// warning, with the names it was given and the error.
func (s *server) productionApplies(ctx context.Context, runs []delivery.DeliveryRun, jobsByRun map[int64][]delivery.DeliveryRunJob, production delivery.Production) []delivery.Apply {
	judged := delivery.ProductionApplies(runs, jobsByRun, production)
	names := strings.Join(production.JobNames, "\x00")
	for _, missing := range judged.Unmatched {
		if !s.firstToWarn(ctx, warnedProductionRun{repo: missing.Run.Repo, runID: missing.Run.RunID, jobNames: names}, production.JobNames) {
			continue
		}
		attributes := []any{"repo", missing.Run.Repo, "run_id", missing.Run.RunID, "configured_job_names", production.JobNames}
		if jobs, err := delivery.ListRunJobs(ctx, s.deps.Store.Pool, missing.Run.Repo, missing.Run.RunID); err != nil {
			attributes = append(attributes, "job_names_seen", missing.JobNames, "job_names_read_error", err)
		} else {
			seen := make([]string, len(jobs))
			for i, job := range jobs {
				seen[i] = job.Name
			}
			attributes = append(attributes, "job_names_seen", seen)
		}
		slog.Warn("dispatch delivery: a successfully concluded run has no job matching the configured production_job_names -- it will never count as a deploy", attributes...)
	}
	for _, run := range judged.StepMissing {
		key := warnedProductionRun{repo: run.Repo, runID: run.RunID, jobNames: names, applyStep: *production.ApplyStep}
		if !s.firstToWarn(ctx, key, production.JobNames) {
			continue
		}
		slog.Warn("dispatch delivery: a run's production job succeeded but lists no step named the configured production_apply_step_name -- it will never count as a deploy",
			"repo", run.Repo, "run_id", run.RunID, "configured_job_names", production.JobNames, "configured_apply_step", *production.ApplyStep)
	}
	return judged.Applies
}

// firstToWarn reports whether this process is the one to log the warning key names, jobNames
// being key's names as a list. This process's warnedUnmatchedRuns answers first, so each process
// asks the database once per key; then the insert into delivery_run_warnings (migration 0161)
// decides between processes: only the one whose insert wrote the row logs, and every other finds
// the row and stays quiet.
//
// A database that cannot record the row fails open: the warning is logged anyway, with the error.
// A repeated warning costs a log line, where a lost one hides a misconfiguration that counts no
// deploy, ever.
func (s *server) firstToWarn(ctx context.Context, key warnedProductionRun, jobNames []string) bool {
	if _, warned := s.warnedUnmatchedRuns.LoadOrStore(key, struct{}{}); warned {
		return false
	}
	err := s.deps.Store.Pool.QueryRow(ctx, `
		insert into delivery_run_warnings (repo, run_id, job_names, apply_step) values ($1, $2, $3, $4)
		on conflict do nothing
		returning true
	`, key.repo, key.runID, jobNames, key.applyStep).Scan(new(bool))
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		slog.Warn("dispatch delivery: could not record a configuration warning for every process, so this process logs it whether or not another already has",
			"repo", key.repo, "run_id", key.runID, "error", err)
	}
	return true
}

// deliveryIssue is what the timeline reads of one issue a pull request names: its project, title,
// priority, and effective components (qualified `<project>/<id>`, resolved as every issue read
// resolves them: the nearest issue on its parent chain that chose, through issueComponentsTerms).
type deliveryIssue struct {
	project    string
	title      string
	priority   *int
	components []string
}

// deliveryIssueFactsQuery is the pull-request timeline's read of the issues a delivery names,
// with their components. The text is built once, at start, for the reason issueRowStatement
// gives.
var deliveryIssueFactsQuery = `
		with recursive subjects as (select i.key from issues i where i.key = any($1)),
		` + issueComponentsTerms + `
		select i.key, i.project_key, i.title, i.priority, coalesce(comp.ids, '{}'::text[])
		from issues i
		` + issueComponentsJoin + `
		where i.key = any($1)
	`

// deliveryIssueFacts reads in q the facts of the issues keys names (deliveryIssueFactsQuery), by key.
func deliveryIssueFacts(ctx context.Context, q delivery.Querier, keys []string) (map[string]deliveryIssue, error) {
	issues := make(map[string]deliveryIssue, len(keys))
	if len(keys) == 0 {
		return issues, nil
	}
	rows, err := q.Query(ctx, deliveryIssueFactsQuery, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var issue deliveryIssue
		var ids []string
		if err := rows.Scan(&key, &issue.project, &issue.title, &issue.priority, &ids); err != nil {
			return nil, err
		}
		issue.components = make([]string, len(ids))
		for i, id := range ids {
			issue.components[i] = deliveryComponentKey(issue.project, id)
		}
		issues[key] = issue
	}
	return issues, rows.Err()
}

// deliveryComponentKey names a component as the graph addresses it, `<project>/<id>`: component
// ids are unique only within their project.
func deliveryComponentKey(project, id string) string {
	return project + "/" + id
}

// deliveryComponents names, read in q, every component of projects, keyed by deliveryComponentKey.
func deliveryComponents(ctx context.Context, q delivery.Querier, projects []string) (map[string]delivery.DeliveryComponentView, error) {
	components := map[string]delivery.DeliveryComponentView{}
	if len(projects) == 0 {
		return components, nil
	}
	rows, err := q.Query(ctx, `select project_key, id, title, parent from components where project_key = any($1)`, projects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var project, id, title string
		var parent *string
		if err := rows.Scan(&project, &id, &title, &parent); err != nil {
			return nil, err
		}
		view := delivery.DeliveryComponentView{Title: title}
		if parent != nil {
			qualified := deliveryComponentKey(project, *parent)
			view.Parent = &qualified
		}
		components[deliveryComponentKey(project, id)] = view
	}
	return components, rows.Err()
}

// deliveryRow is one window pull request's view with its value for each facet: one value for most,
// every session for session, every effective component for component, and none for issue when it
// names no issue (the issue facet offers no placeholder).
type deliveryRow struct {
	view   delivery.DeliveryPRView
	values map[string][]string
	search string
}

// newDeliveryRow reads a view's facet values. Every colour-by facet (deliveryColorFacets) gets at
// least one value, a placeholder when the pull request has none, since deliveryColorCounts colours
// a row by its first. session gets no placeholder: parent_agent's __no_session__ already filters
// the pull requests no session wrote.
func newDeliveryRow(view delivery.DeliveryPRView) deliveryRow {
	values := map[string][]string{
		"repo":     {view.Repo},
		"session":  view.Sessions,
		"issue":    {},
		"author":   {view.Author},
		"rework":   {deliveryReworkValue},
		"deployed": {string(view.DeployedStatus)},
	}
	if view.ParentAgent != nil {
		values["parent_agent"] = []string{*view.ParentAgent}
	} else {
		values["parent_agent"] = []string{deliveryNoSession}
	}
	if view.Rework {
		values["rework"] = []string{deliveryReworkRework}
	}
	switch {
	case view.Issue == nil:
		values["priority"] = []string{deliveryNoIssue}
		values["component"] = []string{deliveryNoIssue}
	default:
		values["issue"] = []string{*view.Issue}
		values["priority"] = []string{deliveryNoPriority}
		if view.Priority != nil {
			values["priority"] = []string{*view.Priority}
		}
		values["component"] = []string{deliveryNoComponent}
		if len(view.Components) > 0 {
			values["component"] = view.Components
		}
	}
	return deliveryRow{view: view, values: values, search: strings.ToLower(view.Title) + "\x00" + strings.ToLower(view.ID)}
}

// deliverySelection is the request's facet selections, each a set of values a row must hold one of
// (a selected component standing for itself and every component below it), and its search, the
// lowercased text a row's title or id must contain.
type deliverySelection struct {
	facets map[string][]string
	search string
}

// selects is whether the selection can keep a row out: a search, or a facet holding a value.
// Where it selects nothing, every row matches whatever its labels (row.matches).
func (s deliverySelection) selects() bool {
	return s.search != "" || len(s.facets) > 0
}

func parseDeliverySelection(query url.Values, components map[string]delivery.DeliveryComponentView) deliverySelection {
	selection := deliverySelection{facets: map[string][]string{}, search: strings.ToLower(strings.TrimSpace(query.Get("q")))}
	for _, facet := range deliveryFacets {
		values := query[facet]
		if len(values) == 0 {
			continue
		}
		switch facet {
		case "priority":
			normalized := make([]string, len(values))
			for i, value := range values {
				normalized[i] = value
				if match := deliveryPrioritySelection.FindStringSubmatch(value); match != nil {
					normalized[i] = "P" + match[1]
				}
			}
			values = normalized
		case "component":
			values = deliveryComponentDescendants(components, values)
		}
		selection.facets[facet] = values
	}
	return selection
}

// deliveryComponentDescendants is selected plus every component below a selected one.
func deliveryComponentDescendants(components map[string]delivery.DeliveryComponentView, selected []string) []string {
	children := map[string][]string{}
	for id, component := range components {
		if component.Parent != nil {
			children[*component.Parent] = append(children[*component.Parent], id)
		}
	}
	expanded := []string{}
	stack := slices.Clone(selected)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if slices.Contains(expanded, id) {
			continue
		}
		expanded = append(expanded, id)
		stack = append(stack, children[id]...)
	}
	return expanded
}

// matches is whether the row passes the search and every selected facet but skip.
func (row deliveryRow) matches(selection deliverySelection, skip string) bool {
	if selection.search != "" && !strings.Contains(row.search, selection.search) {
		return false
	}
	for facet, selected := range selection.facets {
		if facet == skip {
			continue
		}
		if !slices.ContainsFunc(row.values[facet], func(value string) bool { return slices.Contains(selected, value) }) {
			return false
		}
	}
	return true
}

// deliveryFacetCounts counts, per facet, the rows passing the search and every other facet's
// selection by that facet's values, so selecting a value never zeroes its own count.
func deliveryFacetCounts(rows []deliveryRow, selection deliverySelection) map[string]map[string]int {
	counts := make(map[string]map[string]int, len(deliveryFacets))
	for _, facet := range deliveryFacets {
		counts[facet] = map[string]int{}
	}
	for _, row := range rows {
		for _, facet := range deliveryFacets {
			if !row.matches(selection, facet) {
				continue
			}
			for _, value := range row.values[facet] {
				counts[facet][value]++
			}
		}
	}
	return counts
}

// deliveryColorCounts counts every row, with no facet or search applied, by the value it is
// coloured by under each colour-by facet: its first value there. newDeliveryRow gives every
// colour-by facet at least one value (TestNewDeliveryRowGivesEveryColourFacetAValue holds it), so
// the index never misses.
func deliveryColorCounts(rows []deliveryRow) map[string]map[string]int {
	counts := make(map[string]map[string]int, len(deliveryColorFacets))
	for _, facet := range deliveryColorFacets {
		counts[facet] = map[string]int{}
	}
	for _, row := range rows {
		for _, facet := range deliveryColorFacets {
			counts[facet][row.values[facet][0]]++
		}
	}
	return counts
}
