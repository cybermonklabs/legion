package evals

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

// deploy_repos.go is how the delivery and CI evals that read the deploy repository are kept
// (dispatch://INFRA-2916, R14; dispatch://INFRA-978, D1): once for every deploy source repository
// that holds a delivery run, each repository's rows computed under its own source's rule
// (delivery.DeployRules) and keyed by it (eval_days.deploy_repo, migration 20261117100006), with
// their stored figures and their members. A project's read folds the rows of each of its source
// repositories as the refresher last recorded them (eval_days_rules, DeployReposIn): counts and
// hours summed, a share or a quantile folded over the union of the repositories' parts, as a
// repository filter folds several repositories' rows. So a source that moves, a named repository
// changed or the default repository a source naming none reads moved, is a selection the
// refresher's page records in the commit that brings the rows to it, and a read answers the old
// selection whole or the new one whole.
//
// What reads the deploy repository is what a day reads of it (readDeliveryDays): its deploy runs
// (delivery.ReadDeploys), its apply runs, under which containment ships its pull requests alone
// (delivery.ContainingRun), and its CI runs (delivery.ReadCIRows). So the three lead times to
// production, deploy-frequency, deploy-run-success, pr-ci with its job series and the three deploy
// cards are kept per repository, and so are the members of the delivery and CI families those
// rows count: each pull request with the run of that repository that ships it, that repository's
// deploy runs, and its CI runs. pr-open-to-merge and rework-share read a pull request's opening,
// merge and rework and no deploy, and keep one row, as do the Legion evals, the logged evals and
// unowned-p0; the pull requests the first two count are kept once more under no deploy repository.
//
// A project's events source reads its posted deploys (production_events rows of kind deploy and
// source api, dispatch://INFRA-978 D1, R12), which count for that project alone while it has
// delivery on and the source: they are kept under a deploy repository of the project's own
// (EventsRepo), whose deploy-frequency counts each posted deploy on the day it finished and whose
// merge-to-production times each pull request a posted deploy names from its merge to the deploy
// (postedDayAs), where that deploy ships it before any GitHub deploy does: a pull request's lead
// times count once, at the first deploy that ships it (firstShipped). It is kept, recorded and
// selected as a source repository is, under no job name.
//
// The repositories kept are the source repositories that hold a delivery run (readDeployRepos),
// and the events repository of each project with an events source that holds a posted deploy
// (readPostedRepos). The intake keeps the runs of a source repository alone, so a repository's
// runs are those stored while it was a source; one that never was holds none, and so no row: a
// source naming it reads "no data" for every eval that reads the deploy repository (a repository
// that never deployed has nothing to count), and the rest as before. A repository that stops
// being a source is no longer kept, and its rows go. The set of them, each with its start
// (keptRepo.since), is recorded with each one's rule (eval_days_rules), and a page that reads
// another set, a repository whose first runs arrived, one whose runs went, one whose start moved
// or one that became or stopped being a source, builds the days of each that differs in its own
// commit (syncKept, span.go): every day of a new repository from its start on, its rows and its
// members of every pull request it counts, as a whole walk over the same runs builds them, where
// the change that brought the runs names only the days of the runs.
//
// The first day of each of a repository's series, which a read names (data_since, each series its
// own) and before which a range reads "no data", is that repository's own, and never earlier than
// the start of the history the reconcile holds for it (keptRepo.since): no row of it is kept
// before its first deploy or PR-checks run. A lead time to production begins on the merge day of
// the first pull request the repository ships (dropUnshipped): a day before it whose pull requests
// none of the repository's deploys ships counts nothing toward a lead time, and holds no row of
// it, where a day after it holds its row, a zero one included.

// deployRepoSlugs is the evals (by slug) whose rows and items read the deploy repository: the
// delivery and CI evals a day keeps once per repository. The failure measures (change-fail-rate,
// failed-deploy-recovery and deployment-rework-rate) are none of them: their rows are the project's
// own, built from the record of its deploys and failures (dispatch://INFRA-978, D7).
var deployRepoSlugs = []string{
	SlugMergeToProduction, SlugFirstCommitToProduction, SlugOpenToProduction, SlugDeployFrequency,
	SlugDeployRunSuccess, SlugPRCI, SlugStagingE2E, SlugStagingDeploy, SlugProductionDeploy,
}

// readsDeployRepo reports whether the eval slug names is one whose rows are kept per deploy
// repository (deployRepoSlugs).
func readsDeployRepo(slug string) bool {
	return slices.Contains(deployRepoSlugs, slug)
}

// KeyedByDeployRepo reports whether a read of definition's eval reads the rows of a deploy
// repository: a computed eval of deployRepoSlugs. A logged eval's rows are its readings', which no
// deploy repository moves, whatever its slug.
func KeyedByDeployRepo(definition Definition) bool {
	return definition.Computed() && readsDeployRepo(definition.Slug)
}

// DeployRepos is the repositories whose rows a read of a project's evals that read the deploy
// repository folds, as the refresher last recorded them with the rows (eval_days_rules,
// DeployReposIn): every source repository of every delivery-on project, whose rows count for a
// project only where the attribution counts a pull request, a deploy or a run for it (a deploy of
// one project's repository that ships another's pull request counts for both, dispatch://INFRA-978
// A8, and one that ships none for its source's project alone, R12), and the project's own events
// repository. owners is each source repository's project, by repository.
type DeployRepos struct {
	sources []string
	events  map[string]string
	owners  map[string]string
}

// Of is the repositories project's read folds: every source repository, and its events repository
// where one is recorded; none for a nil DeployRepos.
func (d *DeployRepos) Of(project string) []string {
	if d == nil {
		return []string{}
	}
	repos := slices.Clone(d.sources)
	if repo, held := d.events[project]; held {
		repos = append(repos, repo)
	}
	return nonNil(repos)
}

// Owned is the source repositories project's own sources read, in the byte order of their names.
func (d *DeployRepos) Owned(project string) []string {
	if d == nil {
		return nil
	}
	var owned []string
	for _, repo := range d.sources {
		if d.owners[repo] == project {
			owned = append(owned, repo)
		}
	}
	return owned
}

// DeployRepoOf is the deploy repositories whose day rows, stored figures and members a read of
// definition's eval reads, where the rows are recorded under deployRepos (DeployReposIn): its
// project's repositories for an eval keyed by them (KeyedByDeployRepo, DeployRepos.Of), and the
// rows of none ("") for every other eval, whose rows no deploy repository moves.
func DeployRepoOf(definition Definition, deployRepos *DeployRepos) []string {
	if KeyedByDeployRepo(definition) {
		return deployRepos.Of(definition.ProjectKey)
	}
	return []string{""}
}

// pairs is each of projects' repositories (Of), as two arrays of one element per project and
// repository, which deployRepoCase reads.
func (d *DeployRepos) pairs(projects []string) (keys, repos []string) {
	keys, repos = []string{}, []string{}
	for _, project := range projects {
		for _, repo := range d.Of(project) {
			keys, repos = append(keys, project), append(repos, repo)
		}
	}
	return keys, repos
}

// DeployReposOf is the DeployRepos that read repo alone ("" for none): one cut of the stored
// figures (figureCut), or one repository a test reads, project's where repo is its events
// repository.
func DeployReposOf(project, repo string) *DeployRepos {
	switch posted := IsEventsRepo(repo); {
	case repo == "":
		return nil
	case posted:
		return &DeployRepos{events: map[string]string{project: repo}}
	}
	return &DeployRepos{sources: []string{repo}, owners: map[string]string{repo: project}}
}

// DeployReposIn is, read in q, the repositories whose rows a read of a project's evals that read
// the deploy repository answers (DeployRepos): those the refresher recorded with the stored rows
// (eval_days_rules, recordRules), every source repository and each project's events repository
// once a posted deploy is kept under it (kept_since set: one with no post holds no row). The
// project settings name another source from their save on, or another default repository from its
// move on, while the rows of the repositories the move brings in stand under another rule, or
// none, until the refresher's page commits, so a read by the saved settings would answer a state
// between the two; the record moves in the commit that brings the rows to the settings
// (rewriteSettings, or the page's commit), so a read in one snapshot answers the old selection
// whole or the new one whole (BuiltSettingsIn says the same of a repository's rule). One statement
// over a table of one row per source repository.
func DeployReposIn(ctx context.Context, q delivery.Querier) (*DeployRepos, error) {
	rows, err := q.Query(ctx, `
		select project_key, repo, kept_since is not null from eval_days_rules order by repo
	`)
	if err != nil {
		return nil, fmt.Errorf("read the deploy repositories the day rows stand recorded under: %w", err)
	}
	repos := &DeployRepos{events: map[string]string{}, owners: map[string]string{}}
	var project, repo string
	var kept bool
	if _, err := pgx.ForEachRow(rows, []any{&project, &repo, &kept}, func() error {
		switch posted := IsEventsRepo(repo); {
		case !posted:
			repos.sources, repos.owners[repo] = append(repos.sources, repo), project
		case kept:
			repos.events[project] = repo
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the deploy repositories the day rows stand recorded under: %w", err)
	}
	return repos, nil
}

// keyedEvals is the ids of entries' evals keyed by the deploy repository (KeyedByDeployRepo): the
// evals a read's `case` gives their project's repositories' rows (deployRepoCase), every other
// eval reading the rows of none.
func keyedEvals(entries []Entry) []string {
	keyed := []string{}
	for _, entry := range entries {
		if KeyedByDeployRepo(entry.Definition) {
			keyed = append(keyed, entry.Definition.EvalID)
		}
	}
	return keyed
}

// deployRepoCase is the condition that keeps, of the day rows or the stored figures of the evals
// it is applied to, those of the deploy repositories each eval's read reads (DeployRepoOf): of the
// evals the array parameter keyed names (keyedEvals) the rows of each repository of their project
// the array parameters projects and repos name pairwise (DeployRepos.pairs), and of every other
// eval the rows of none.
func deployRepoCase(column, keyed, projects, repos string) string {
	return `(case when eval_id = any(` + keyed + `::text[])
		then (project_key, ` + column + `) in (select * from unnest(` + projects + `::text[], ` + repos + `::text[]))
		else ` + column + ` = '' end)`
}

// EventsRepo is the deploy repository project's posted deploys are kept under (the package
// comment's events repository): no repository's name, which holds a slash.
func EventsRepo(project string) string {
	return delivery.SourceKindEvents + ":" + project
}

// IsEventsRepo reports whether repo is a project's events repository (EventsRepo) rather than a
// repository's name.
func IsEventsRepo(repo string) bool {
	return strings.HasPrefix(repo, delivery.SourceKindEvents+":")
}

// ruleKey is the key a repository's rule is held by (refresh.rules, refresh.builtUnder): its name
// without case, as a source names it and as the runs of it are stored either way.
func ruleKey(repo string) string {
	return strings.ToLower(repo)
}

// keptRepo is one repository whose rows of the evals that read the deploy repository are kept:
// its rule (rule.DeployRepo the repository as its rows are keyed, rule.Project its source's
// project), whether it holds a deploy run, which the production rule and the deploy cards read,
// and since, the UTC midnight of its first deploy or PR-checks run, before which no row of it is
// kept. An events repository (EventsRepo) holds no deploy run and no job name in its rule, and its
// since is the first day a posted deploy of the project finished (readPostedRepos).
//
// since is the start of the history the reconcile holds for the repository: the reconcile lists
// the deploy repository's runs from delivery.BackfillWindow before the first pass after the
// settings naming it were saved, and the intake keeps its runs alone, so its first stored run is
// the first moment its history holds; nothing else records that moment per repository, since a
// settings change starts the reconcile's progress anew. It is deliveryInputStart's bound of the
// runs, per repository. The pull requests reach further back, the whole population's history, so
// a first deploy that ships a pull request merged before the repository's history began would
// otherwise time a lead time from before it: such a day holds no row of the repository.
type keptRepo struct {
	name    string
	deploys bool
	since   time.Time
	rule    delivery.DeliverySettings
}

// readDeployRepos reads the repositories whose rows of the evals that read the deploy repository
// are kept: every repository that holds a delivery run, in the byte order of their names, each
// with whether it holds a deploy run and the start of its history (keptRepo.since). One
// statement walks delivery_runs' primary key from one repository to the next (repo, run_id), one
// probe each, and reads delivery_runs_kind_started_at's first entry of each repository and kind,
// so it reads a handful of index entries however many runs there are.
func readDeployRepos(ctx context.Context, q delivery.Querier) ([]keptRepo, error) {
	rows, err := q.Query(ctx, `
		with recursive held as (
			(select repo from delivery_runs order by repo limit 1)
			union all
			select (select run.repo from delivery_runs run where run.repo > held.repo order by run.repo limit 1)
			from held where held.repo is not null
		), first as (
			select held.repo,
				(select min(started_at) from delivery_runs run where run.repo = held.repo and run.kind = $1) as deploy,
				(select min(started_at) from delivery_runs run where run.repo = held.repo and run.kind = $2) as checks
			from held where held.repo is not null
		)
		select repo, deploy is not null, least(deploy, checks) from first
	`, string(delivery.DeliveryRunKindDeploy), string(delivery.DeliveryRunKindPRChecks))
	if err != nil {
		return nil, fmt.Errorf("read the repositories that hold a delivery run: %w", err)
	}
	var repos []keptRepo
	var one keptRepo
	if _, err := pgx.ForEachRow(rows, []any{&one.name, &one.deploys, &one.since}, func() error {
		one.since = measures.Midnight(one.since)
		repos = append(repos, one)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the repositories that hold a delivery run: %w", err)
	}
	slices.SortFunc(repos, func(a, b keptRepo) int { return cmp.Compare(a.name, b.name) })
	return repos, nil
}

// readPostedRepos reads the events repository of each of rules (eventsRule: one per project with
// delivery on and an events source) that holds a posted deploy, with its rule and its start: the
// UTC midnight of its first posted deploy's finish (prodevents.FirstPostedAt, one row of the record
// for each project). A pull request merged before that day that a posted deploy names is an item
// of no day of the repository, as one merged before a source repository's first run is of no day
// of it (keptRepo.since).
func readPostedRepos(ctx context.Context, q delivery.Querier, rules []delivery.DeliverySettings) ([]keptRepo, error) {
	projects := make([]string, len(rules))
	for i, rule := range rules {
		projects[i] = rule.Project
	}
	firsts, err := prodevents.FirstPostedAt(ctx, q, projects)
	if err != nil {
		return nil, err
	}
	var repos []keptRepo
	for i, first := range firsts {
		if first != nil {
			repos = append(repos, keptRepo{name: rules[i].DeployRepo, since: measures.Midnight(*first), rule: rules[i]})
		}
	}
	return repos, nil
}

// eventsRule is the rule of project's events repository (EventsRepo): its project alone, which
// its posted deploys count for, and no job name, since no run of it is judged.
func eventsRule(project string) delivery.DeliverySettings {
	return delivery.DeliverySettings{Project: project, DeployRepo: EventsRepo(project), ProductionJobNames: []string{}}
}

// postedDeploy is what a day's rows read of a posted deploy (postedDayAs): its row id, the moment
// it finished, its repository ("" for none), and each pull request it names in the shape
// `owner/repo#n` (parsePullRequestRef), in the order it names them.
type postedDeploy struct {
	id         string
	finishedAt time.Time
	repo       string
	shipped    []pullRequestRef
}

// postedRecord is a project's posted deploys that finished from a day on, read once
// (postedDeploys), as a day's rows read them (postedDayAs): by the UTC day each finished, newest
// first (ties by id, the greater first), and by each pull request one names (foldedRef), the first
// to finish first (ties by id, the lesser first).
type postedRecord struct {
	finishedOn map[time.Time][]postedDeploy
	naming     map[pullRequestRef][]postedDeploy
}

// postedDeploys reads, in q, project's posted deploys that finished from from on
// (prodevents.PostedDeploysFrom): the rows of the record of kind deploy and source api, which a
// system posted to the project's route (dispatch://INFRA-978, R1 and R12).
func postedDeploys(ctx context.Context, q delivery.Querier, project string, from time.Time) (postedRecord, error) {
	posted, err := prodevents.PostedDeploysFrom(ctx, q, project, from)
	if err != nil {
		return postedRecord{}, err
	}
	record := postedRecord{finishedOn: map[time.Time][]postedDeploy{}, naming: map[pullRequestRef][]postedDeploy{}}
	for _, one := range posted {
		deploy := postedDeploy{id: one.ID, finishedAt: one.FinishedAt, repo: one.Repo}
		for _, ref := range one.PullRequests {
			if repo, number, ok := parsePullRequestRef(ref); ok {
				deploy.shipped = append(deploy.shipped, pullRequestRef{repo: repo, number: number})
			}
		}
		day := measures.Midnight(deploy.finishedAt)
		record.finishedOn[day] = append(record.finishedOn[day], deploy)
		for _, pr := range deploy.shipped {
			key := foldedRef(pr.repo, pr.number)
			record.naming[key] = append(record.naming[key], deploy)
		}
	}
	for _, deploys := range record.naming {
		slices.SortFunc(deploys, func(a, b postedDeploy) int {
			return cmp.Or(a.finishedAt.Compare(b.finishedAt), cmp.Compare(a.id, b.id))
		})
	}
	return record, nil
}

// shipping is the posted deploy of p that ships pr, merged at merged: the first to finish at or
// after its merge (ties by id) of those that name it, nil where none does.
func (p postedRecord) shipping(pr delivery.DeliveryPullRequest, merged time.Time) *postedDeploy {
	naming := p.naming[foldedRef(pr.Repo, pr.Number)]
	at, _ := slices.BinarySearchFunc(naming, merged, func(deploy postedDeploy, merged time.Time) int {
		return deploy.finishedAt.Compare(merged)
	})
	if at == len(naming) {
		return nil
	}
	return &naming[at]
}

// foldedRef is the key postedRecord holds a pull request by: its number, and its repository's name
// with each rune the least of the runes strings.EqualFold holds equal to it (its
// unicode.SimpleFold orbit), so two names EqualFold calls one are one key.
func foldedRef(repo string, number int) pullRequestRef {
	folded := strings.Map(func(r rune) rune {
		least := r
		for other := unicode.SimpleFold(r); other != r; other = unicode.SimpleFold(other) {
			least = min(least, other)
		}
		return least
	}, repo)
	return pullRequestRef{repo: folded, number: number}
}

// parsePullRequestRef is the repository and number of a pull request a posted deploy names as
// `owner/repo#n` (production_events.pull_requests), false for a name of no other shape.
func parsePullRequestRef(ref string) (string, int, bool) {
	hash := strings.LastIndex(ref, "#")
	if hash <= 0 || !strings.Contains(ref[:hash], "/") {
		return "", 0, false
	}
	number, err := strconv.ParseInt(ref[hash+1:], 10, 32)
	if err != nil || number <= 0 {
		return "", 0, false
	}
	return ref[:hash], int(number), true
}

// PostedMember is a posted deploy's row key in eval_day_members: `production_event:<id>`.
func PostedMember(id string) string {
	return "production_event:" + id
}

// keptRules is each of repos' rule, its DeployRepo the repository as its rows are keyed, in their
// order: what ZeroDeliveryDay builds each repository's zero rows under.
func keptRules(repos []keptRepo) []delivery.DeliverySettings {
	rules := make([]delivery.DeliverySettings, len(repos))
	for i, repo := range repos {
		rules[i] = repo.rule
	}
	return rules
}

// deployRepoNames is the names of repos, in their order.
func deployRepoNames(repos []keptRepo) []string {
	names := make([]string, len(repos))
	for i, repo := range repos {
		names[i] = repo.name
	}
	return names
}

// evalKey is one eval's rows of one deploy repository ("" for an eval no deploy repository moves):
// what the refresher keeps each series' first input day by, since each repository's rows of an
// eval that reads it begin on that repository's own first day.
type evalKey struct {
	eval, deployRepo string
}

// keyOf is row's evalKey.
func keyOf(row DayRow) evalKey {
	return evalKey{eval: row.EvalID, deployRepo: row.DeployRepo}
}

// compareEvalKeys orders eval keys by eval and then by deploy repository, each by its bytes.
func compareEvalKeys(a, b evalKey) int {
	return cmp.Or(cmp.Compare(a.eval, b.eval), cmp.Compare(a.deployRepo, b.deployRepo))
}

// leadTimeSlugs is the evals that time a pull request to the deploy that ships it, each of which
// counts in a repository's rows the pull requests that repository ships alone.
var leadTimeSlugs = []string{SlugMergeToProduction, SlugFirstCommitToProduction, SlugOpenToProduction}

// dropUnshipped is rows, a project's rows of day as a day's reads build them, without each lead
// time to production of a deploy repository whose all-repositories row counts no pull request
// (n 0), where the eval holds no row on or before day (first, firstOf's): a day before the first
// pull request the repository ships counts nothing toward its lead times, as a day before a
// series' first input day is no day of it. From the first day that counts one on, a day that
// counts none holds its row as it is built, a zero one.
func dropUnshipped(rows []DayRow, first map[evalKey]time.Time, day time.Time) []DayRow {
	unshipped := map[evalKey]bool{}
	for _, row := range rows {
		if !unshippedLeadTime(&row) {
			continue
		}
		if since, held := first[keyOf(row)]; !held || day.Before(since) {
			unshipped[keyOf(row)] = true
		}
	}
	if len(unshipped) == 0 {
		return rows
	}
	return slices.DeleteFunc(slices.Clone(rows), func(row DayRow) bool { return unshipped[keyOf(row)] })
}

// inputKeys is each eval and deploy repository of which rows, a project's rows of a day as its
// reads build them, hold input: an all-repositories row, but a lead time to production of a
// deploy repository that counts no pull request (unshippedLeadTime), which no first input day
// begins at. A day that holds no input of a series is no first input day of it.
func inputKeys(rows []DayRow) map[evalKey]bool {
	input := map[evalKey]bool{}
	for _, row := range rows {
		if row.Repo != "" || unshippedLeadTime(&row) {
			continue
		}
		input[keyOf(row)] = true
	}
	return input
}

// unshippedLeadTime reports whether row is a deploy repository's all-repositories row of a lead
// time to production that counts no pull request (n 0): one dropUnshipped drops before the eval's
// first input day, and no input of the eval (inputKeys).
func unshippedLeadTime(row *DayRow) bool {
	return row.DeployRepo != "" && row.Repo == "" && row.N == 0 && slices.Contains(leadTimeSlugs, slugOf(row.ProjectKey, row.EvalID))
}
