package evals

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The delivery and CI evals' day rows (dispatch://INFRA-2916, R12, R15, R16 and D1). One UTC day's
// delivery population is read once (readDeliveryDay), by the rules the delivery routes read their
// window by: delivery.ReadDeploys and ReadMergedRows' two reads (ListPullRequestsInWindow and
// ReadApplyRows), whose answer the routes' one read of a window, delivery.ReadWindowRows, gives
// too (TestAWindowsReadAnswersWhatItsTwoReadsAnswer holds it), and ReadCIRows, which the routes
// read with; each row of it is attributed to the projects it counts for; each project's rows are
// built into day parts by the measures package's day builders, the builders the delivery routes
// build their window's parts with (DeliveryDay.Rows); and the refresher writes the parts as that
// project's rows of the day (refresh.writeDelivery, through writeDeliveryDay), which the eval
// routes and the delivery routes fold through the same folds.
//
// A day holds the population pull requests merged that day (merge-to-production,
// first-commit-to-production, open-to-production, pr-open-to-merge and rework-share), the deploy
// runs on main started that day (deploy-frequency and deploy-run-success), the PR-checks
// workflow's pull_request runs started that day with every job (pr-ci, and its unlisted
// `job:<name>` series), and the deploy workflow's push runs on main started that day with the
// jobs the deploy cards follow (production-deploy, staging-e2e and staging-deploy, the last two
// where the settings name their jobs). A pull request's deploy is its first shipping apply
// (delivery.ContainingRun) among the deploy runs GitHub created from its merge on, the deploy it
// has on the delivery routes. A deploy's shipped set is every population pull request whose first
// shipping apply it is, whatever day that pull request merged (delivery.Deploys.ShippedBy): the
// deploy's own, which counts it as a deploy with pull requests (with_prs), as the delivery routes
// count it, and names the projects it counts for. Two kinds of row are not settled when their
// day is first built (measures.go names them): a pull request with no deploy yet and a deploy run
// still running; the day is built again when the run that settles them lands. change-fail-rate and
// failed-deploy-recovery get no rows: no failure source exists, so they read "no data".
//
// Attribution. A pull request counts for the project of the issue it names
// (issues.project_key). One that names no issue, or an issue no row holds, counts for the project
// its repository is mapped to in repo_projects. A deploy run counts for every project the pull
// requests it ships count for, once in each however many of a project's it ships; a run that ships
// none (one that did not reach production, or a deploy that shipped nothing), or whose every
// shipped pull request counts for no project, counts for the project of its own repository. A CI
// run counts for the project of its repository, and so does each of its jobs. A repository is
// mapped by its name compared case-insensitively, or by the name its mapping says it was renamed
// to (refs.CanonicalRepoSQL's rule). A row whose project resolves to nothing (it names no issue,
// and repo_projects maps no repository of that name) counts for no project and is in no project's
// day rows; the delivery routes, which follow no project, still count it.
//
// A row that reached its project through repo_projects rather than through an issue counts in the
// via_repository of every day row that counts it: a pull request that names no issue (or an
// unknown one), a deploy none of whose pull requests of that project reached it through an issue
// (one that ships none included), and every CI run and job. A day row's via_repository is how many
// of the rows its n counts came so (its builder's n over those rows alone): the deployed pull
// requests of merge-to-production, the deploys that reached production of deploy-frequency, the
// timed results of a CI series. It is never more than n, and the repositories' rows add up to the
// all-repositories row's as their n do.
//
// A project's rows of a day are, for each family of rows it holds that day (its pull requests,
// its deploy runs, its PR-checks runs, its deploy push runs), the all-repositories row (repo '')
// of every series of that family's evals, built from every row of the family counted for the
// project, and one row of each such series per repository the family's rows are in, built from
// that repository's rows alone: the pull request's repository, the run's. So every row counted is
// in one repository's rows, and the repositories' rows of a series partition its all-repositories
// row: their n, sum, num, den, counts and via_repository add up to its own, and their samples
// are its samples. A family a project holds no row of that day writes no row: the explicit zero
// rows of a series from its first input day on are the refresher's (ZeroDeliveryDay,
// ProjectDay.WithZeros).
//
// The evals that read the deploy repository are kept once for every repository that holds a
// delivery run, each as if that repository were the deploy repository (deploy_repos.go). So a day
// is read once for each such repository, its deploys, apply runs and CI runs, beside the day's
// pull requests, read once; each repository's read builds the rows of those evals, keyed by it,
// and the items they count (deliveryRead.days[1:]), and the pull requests alone build the rows of
// pr-open-to-merge and rework-share, which read no deploy, and the pull requests they count, under
// no deploy repository (deliveryRead.days[0]).

// The slugs of the delivery and CI evals (dispatch://INFRA-2916/artifact/plan-md, "The seeded
// catalogue"): an eval's id is EvalID(project, slug).
const (
	SlugDeployFrequency         = "deploy-frequency"
	SlugDeployRunSuccess        = "deploy-run-success"
	SlugMergeToProduction       = "merge-to-production"
	SlugFirstCommitToProduction = "first-commit-to-production"
	SlugOpenToProduction        = "open-to-production"
	SlugPROpenToMerge           = "pr-open-to-merge"
	SlugReworkShare             = "rework-share"
	SlugChangeFailRate          = "change-fail-rate"
	SlugFailedDeployRecovery    = "failed-deploy-recovery"
	SlugPRCI                    = "pr-ci"
	SlugStagingE2E              = "staging-e2e"
	SlugStagingDeploy           = "staging-deploy"
	SlugProductionDeploy        = "production-deploy"
	SlugUnownedP0               = "unowned-p0"
)

// SeriesUnownedP0 is unowned-p0's one series: the open P0 issues with no owner, folded by `last`.
const SeriesUnownedP0 = "issues"

// deliveryDaySlugs is every eval a delivery day's rows are of, and so every eval WriteProjectDay
// writes the rows of that a day no longer holds as zero rows: neither change-fail-rate nor
// failed-deploy-recovery, which get no rows, nor unowned-p0, which WriteUnownedP0Day writes.
var deliveryDaySlugs = []string{
	SlugDeployFrequency, SlugDeployRunSuccess, SlugMergeToProduction, SlugFirstCommitToProduction,
	SlugOpenToProduction, SlugPROpenToMerge, SlugReworkShare, SlugPRCI, SlugStagingE2E,
	SlugStagingDeploy, SlugProductionDeploy,
}

// cardSlugs is the CI eval of each deploy card.
var cardSlugs = map[measures.DeployJobCardID]string{
	measures.CardProductionApply: SlugProductionDeploy,
	measures.CardStagingE2E:      SlugStagingE2E,
	measures.CardStagingApply:    SlugStagingDeploy,
}

// CardOf is the deploy card the CI eval slug times (production-deploy, staging-e2e and
// staging-deploy), found false for any other slug.
func CardOf(slug string) (measures.DeployJobCardID, bool) {
	for card, of := range cardSlugs {
		if of == slug {
			return card, true
		}
	}
	return "", false
}

// The families of eval_day_members a delivery day records its items under: the delivery evals'
// pull requests and deploy runs, and the CI evals' runs. Each is its evals' kind.
var deliveryDayFamilies = []string{KindDelivery, KindCI}

// PullRequestMember is a pull request's row key in eval_day_members: `pull_request:<repo>#<number>`.
func PullRequestMember(repo string, number int) string {
	return "pull_request:" + repo + "#" + strconv.Itoa(number)
}

// RunMember is a run's row key in eval_day_members: `run:<repo>/<run id>`.
func RunMember(repo string, runID int64) string {
	return "run:" + repo + "/" + strconv.FormatInt(runID, 10)
}

// counted is one project a row counts for, and whether it reached it through repo_projects
// rather than through an issue.
type counted struct {
	project string
	via     bool
}

// dayPR is one pull request merged on the day, as the builders read it, with its repository, its
// row key, the project it counts for ("" for none) and the deploy run that ships it, nil while
// none does.
type dayPR struct {
	pr     measures.PR
	repo   string
	member string
	deploy *dayItem
	counted
}

// dayDeploy is one deploy run on main started on the day, as the builders read it, with its
// repository, its row key, the projects it counts for and the pull requests it ships.
type dayDeploy struct {
	run      measures.Run
	repo     string
	member   string
	projects []counted
	ships    []dayItem
}

// dayItem is an item of another day that a day's row is built from: the deploy run that ships a
// pull request merged on the day, or a pull request a deploy run started on the day ships, by its
// row key in eval_day_members and the UTC midnight of its own day (the run's start, the pull
// request's merge). The refresher rewrites that day too when the item's own row changes
// (DeliveryDay.linked).
type dayItem struct {
	member string
	day    time.Time
}

// dayCIRun is one CI run started on the day, with its row key and the project of its repository
// ("" for none); every CI run reaches its project through its repository.
type dayCIRun struct {
	run     delivery.DeliveryRun
	member  string
	project string
}

// DeliveryDay is one UTC day's delivery and CI rows as read with one repository as the deploy
// repository (deployRepo), each attributed to the projects it counts for: the rows of the evals
// that read the deploy repository, keyed by it (deploy_repos.go). With no deploy repository it is
// the day's pull requests alone, none shipped, whose rows are those of the evals no deploy
// repository moves.
type DeliveryDay struct {
	// Day is the day's UTC midnight.
	Day        time.Time
	deployRepo string
	// since is the start of deployRepo's history (keptRepo.since): a day before it holds no row
	// and no item of the repository.
	since   time.Time
	cards   []measures.DeployJobCard
	prs     []dayPR
	deploys []dayDeploy
	// checks are the PR-checks runs and checkJobs every job of each; pushes the deploy push runs
	// and pushJobs the jobs of each the cards follow.
	checks, pushes      []dayCIRun
	checkJobs, pushJobs map[int64][]delivery.DeliveryRunJob
}

// deliveryRead is one UTC day's delivery and CI rows (readDeliveryDay): the day's pull requests
// alone, with no deploy repository (days[0]), and the day as read with each of repos as the deploy
// repository (days[1:], in repos' order). Its rows are theirs together: those of the evals no
// deploy repository moves once, and those of the evals that read it once for each repository. The
// settings rewrite's day holds the reads of the repositories it rewrites alone (rewriteSettings).
type deliveryRead struct {
	// Day is the day's UTC midnight.
	Day   time.Time
	repos []string
	days  []DeliveryDay
}

// repoReads is what a day reads of one repository as the deploy repository, under settings, whose
// DeployRepo is that repository, whose history starts at since: its deploys, its apply runs and its
// CI runs.
type repoReads struct {
	settings delivery.DeliverySettings
	since    time.Time
	deploys  delivery.Deploys
	applies  delivery.ApplyRows
	ci       delivery.CIRows
}

// readDeliveryDay reads the delivery and CI rows of the UTC day day is on, under settings, with
// each of repos as the deploy repository, and attributes each to its projects. Its reads answer
// what the delivery routes' read of a window of that day answers with that repository as the
// deploy repository: the day's deploys and the rows before the day their shipped sets read
// (delivery.ReadDeploys), the pull requests merged in the day (delivery.ListPullRequestsInWindow)
// and every deploy run created from the day's start on (delivery.ReadApplyRows), which are
// delivery.ReadMergedRows' two reads and whose answer the routes' one read,
// delivery.ReadWindowRows, gives too (TestAWindowsReadAnswersWhatItsTwoReadsAnswer holds it); the
// day's CI runs (delivery.ReadCIRows); the projects of the issues those pull requests name; and
// repo_projects. The pull requests are read once, and every other read once for each repository.
// Each deploy's shipped pull requests are delivery.Deploys.ShippedBy's, the rule the routes count
// with_prs by; the pull requests merged before the day are read for them alone. A repository's
// deploy runs created from the day's start on are taken from applyRows where it holds them
// (delivery.ApplyRows.Since): rows a walk read once from the first of its days, which every later
// day of the walk takes its part of rather than reading every run created from its own start to
// now again; and its repo_projects from mapped where it is not nil, a read the refresher makes once
// a page (refresh.repos). The reads depend on none of each other, and are read side by side.
func (s *Store) readDeliveryDay(ctx context.Context, settings delivery.DeliverySettings, repos []keptRepo, day time.Time,
	applyRows map[string]*delivery.ApplyRows, mapped *repoProjects) (deliveryRead, error) {
	pool := s.database.Pool
	day = measures.Midnight(day)
	end := day.AddDate(0, 0, 1)
	cards := measures.DeployCards(settings)
	cardNames := measures.DeployCardJobNames(cards)

	var prs []delivery.DeliveryPullRequest
	reads := make([]repoReads, len(repos))
	parts := []func(context.Context) error{func(ctx context.Context) (err error) {
		if prs, err = delivery.ListPullRequestsInWindow(ctx, pool, day, end); err != nil {
			return fmt.Errorf("read the pull requests of %s: %w", day.Format(time.DateOnly), err)
		}
		return nil
	}}
	for i, kept := range repos {
		repo, one := kept.name, &reads[i]
		one.settings, one.since = settings, kept.since
		one.settings.DeployRepo = repo
		parts = append(parts,
			func(ctx context.Context) (err error) {
				if one.deploys, err = delivery.ReadDeploys(ctx, pool, one.settings, day, end); err != nil {
					return fmt.Errorf("read the deploys of %s on %s: %w", repo, day.Format(time.DateOnly), err)
				}
				return nil
			},
			func(ctx context.Context) (err error) {
				if held := applyRows[repo]; held != nil {
					one.applies, err = held.Since(day)
				} else {
					one.applies, err = delivery.ReadApplyRows(ctx, pool, one.settings, day)
				}
				if err != nil {
					return fmt.Errorf("read the deploy runs of %s from %s on: %w", repo, day.Format(time.DateOnly), err)
				}
				return nil
			},
			func(ctx context.Context) (err error) {
				if one.ci, err = delivery.ReadCIRows(ctx, pool, repo, day, end, cardNames); err != nil {
					return fmt.Errorf("read the CI runs of %s on %s: %w", repo, day.Format(time.DateOnly), err)
				}
				return nil
			})
	}
	if err := store.ReadSideBySide(ctx, parts...); err != nil {
		return deliveryRead{}, err
	}
	held := [][]delivery.DeliveryPullRequest{prs}
	for _, one := range reads {
		held = append(held, one.deploys.Earlier.PullRequests)
	}
	var issueKeys []string
	for _, list := range held {
		for _, pr := range list {
			if pr.IssueKey != nil {
				issueKeys = append(issueKeys, *pr.IssueKey)
			}
		}
	}
	issueProjects, err := readIssueProjects(ctx, s, issueKeys)
	if err != nil {
		return deliveryRead{}, err
	}
	if mapped == nil {
		read, err := readRepoProjects(ctx, s)
		if err != nil {
			return deliveryRead{}, err
		}
		mapped = &read
	}
	attribute := attribution{issues: issueProjects, repos: *mapped}

	read := deliveryRead{Day: day, repos: deployRepoNames(repos), days: make([]DeliveryDay, 0, len(repos)+1)}
	alone := DeliveryDay{Day: day, cards: cards}
	for _, pr := range prs {
		// A pull request whose opening is not known yet (a partial row) has no lead time to
		// count, as the delivery routes leave it out of theirs.
		if pr.CreatedAt != nil {
			alone.prs = append(alone.prs, attribute.pullRequest(pr, nil, nil))
		}
	}
	read.days = append(read.days, alone)
	for _, one := range reads {
		read.days = append(read.days, attribute.dayAs(day, cards, prs, one))
	}
	return read, nil
}

// dayAs is the day's rows as read with one.settings.DeployRepo as the deploy repository: prs, the
// pull requests merged that day, each shipped by its first shipping apply among that repository's
// apply runs (delivery.ContainingRun, under which a pull request of that repository alone ships);
// that repository's deploys, each with the pull requests it ships (delivery.Deploys.ShippedBy);
// and its CI runs; each attributed to its projects.
func (a attribution) dayAs(day time.Time, cards []measures.DeployJobCard, prs []delivery.DeliveryPullRequest, one repoReads) DeliveryDay {
	settings := one.settings
	read := DeliveryDay{Day: day, deployRepo: settings.DeployRepo, since: one.since, cards: cards}
	reached := map[int64]bool{}
	for _, apply := range one.deploys.Applies {
		reached[apply.RunID] = true
	}
	applies := delivery.ProductionApplies(one.applies.Runs, one.applies.Jobs, settings.Production()).Applies
	applyRuns := make(map[int64]delivery.DeliveryRun, len(one.applies.Runs))
	for _, run := range one.applies.Runs {
		applyRuns[run.RunID] = run
	}
	for _, pr := range prs {
		// A partial row, whose opening is not known yet, as readDeliveryDay leaves it out.
		if pr.CreatedAt != nil {
			read.prs = append(read.prs, a.pullRequest(pr, delivery.ContainingRun(pr, settings.DeployRepo, applies), applyRuns))
		}
	}
	shipped := one.deploys.ShippedBy(delivery.MergedRows{PullRequests: prs, ApplyRuns: one.applies.Runs, ApplyJobs: one.applies.Jobs}, settings)
	for _, run := range one.deploys.Started.Runs {
		read.deploys = append(read.deploys, a.deploy(run, reached[run.RunID], shipped[run.RunID]))
	}
	read.checks, read.checkJobs = a.ciRuns(one.ci.Checks.Runs), one.ci.Checks.Jobs
	read.pushes, read.pushJobs = a.ciRuns(one.ci.Deploys.Runs), one.ci.Deploys.Jobs
	return read
}

// Rows is project's rows of the day and the items they count: each of its reads' (DeliveryDay.Rows).
func (d deliveryRead) Rows(project string) ProjectDay {
	result := ProjectDay{Project: project, Day: d.Day}
	for _, day := range d.days {
		built := day.Rows(project)
		result.Rows = append(result.Rows, built.Rows...)
		result.Members = append(result.Members, built.Members...)
	}
	return result
}

// countedFor is every project any of the day's reads counts the item member for, by key.
func (d deliveryRead) countedFor(member string) []string {
	projects := map[string]bool{}
	for _, day := range d.days {
		for _, project := range day.countedFor(member) {
			projects[project] = true
		}
	}
	return slices.Sorted(maps.Keys(projects))
}

// linked is the items of other days any of the day's reads builds the item member into, each
// once: each repository ships a pull request of its own alone, and its deploys are its own runs.
func (d deliveryRead) linked(member string) []dayItem {
	var items []dayItem
	seen := map[dayItem]bool{}
	for _, day := range d.days {
		for _, item := range day.linked(member) {
			if !seen[item] {
				seen[item] = true
				items = append(items, item)
			}
		}
	}
	return items
}

// attribution is what a day's rows are attributed to their projects by: the project of each issue
// the pull requests name (readIssueProjects) and repo_projects. readDeliveryDay attributes one
// day's rows by it, and the refresher's rewrite of a settings change the rows of each day it
// builds (rewriteSettings).
type attribution struct {
	issues map[string]string
	repos  repoProjects
}

// of is the project pr counts for: its issue's, else its repository's, reached through
// repo_projects.
func (a attribution) of(pr delivery.DeliveryPullRequest) counted {
	if pr.IssueKey != nil {
		if project, found := a.issues[*pr.IssueKey]; found {
			return counted{project: project}
		}
	}
	return counted{project: a.repos.of(pr.Repo), via: true}
}

// pullRequest is pr, merged on the day, as the builders read it: shipped by apply (nil while none
// does), whose run, where runs holds it, is the deploy of another day that the day's rows link it
// to.
func (a attribution) pullRequest(pr delivery.DeliveryPullRequest, apply *delivery.Apply, runs map[int64]delivery.DeliveryRun) dayPR {
	entry := dayPR{
		pr: measures.PROf(pr, apply), repo: pr.Repo, member: PullRequestMember(pr.Repo, pr.Number),
		counted: a.of(pr),
	}
	if apply != nil {
		if run, found := runs[apply.RunID]; found {
			entry.deploy = &dayItem{member: RunMember(run.Repo, run.RunID), day: measures.Midnight(run.StartedAt)}
		}
	}
	return entry
}

// deploy is run, a deploy run on main started on the day, as the builders read it: whether it
// reached production, the pull requests it ships first (Deploys.ShippedBy's), and the projects it
// counts for.
func (a attribution) deploy(run delivery.DeliveryRun, reached bool, shipped []*delivery.DeliveryPullRequest) dayDeploy {
	deploy := dayDeploy{
		run: measures.Run{
			ID: run.RunID, StartedAt: run.StartedAt, Conclusion: run.Conclusion,
			ReachedProduction: reached, ShippedPRs: len(shipped),
		},
		repo: run.Repo, member: RunMember(run.Repo, run.RunID),
	}
	for _, pr := range shipped {
		deploy.ships = append(deploy.ships, dayItem{member: PullRequestMember(pr.Repo, pr.Number), day: measures.Midnight(*pr.MergedAt)})
	}
	// Once in each project its pull requests count for, through an issue where any of that
	// project's did.
	for _, pr := range shipped {
		counts := a.of(*pr)
		if counts.project == "" {
			continue
		}
		index := slices.IndexFunc(deploy.projects, func(c counted) bool { return c.project == counts.project })
		if index < 0 {
			deploy.projects = append(deploy.projects, counts)
			continue
		}
		deploy.projects[index].via = deploy.projects[index].via && counts.via
	}
	if len(deploy.projects) == 0 {
		if project := a.repos.of(run.Repo); project != "" {
			deploy.projects = []counted{{project: project, via: true}}
		}
	}
	return deploy
}

// ciRuns is runs, CI runs started on the day, each attributed to the project of its repository.
func (a attribution) ciRuns(runs []delivery.DeliveryRun) []dayCIRun {
	attributed := make([]dayCIRun, len(runs))
	for i, run := range runs {
		attributed[i] = dayCIRun{run: run, member: RunMember(run.Repo, run.RunID), project: a.repos.of(run.Repo)}
	}
	return attributed
}

// readIssueProjects is the project of each of keys that an issue holds.
func readIssueProjects(ctx context.Context, s *Store, keys []string) (map[string]string, error) {
	projects := make(map[string]string, len(keys))
	if len(keys) == 0 {
		return projects, nil
	}
	rows, err := s.database.Pool.Query(ctx, `select key, project_key from issues where key = any($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("read the projects of the pull requests' issues: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, project string
		if err := rows.Scan(&key, &project); err != nil {
			return nil, fmt.Errorf("read the projects of the pull requests' issues: %w", err)
		}
		projects[key] = project
	}
	return projects, rows.Err()
}

// repoProjects is repo_projects as attribution reads it: the project of each mapped repository by
// its lower-cased name, and the project of each name a mapping says its repository was renamed to.
type repoProjects struct {
	direct, renamed map[string]string
}

// of is the project repo is mapped to, by its own name first and then as a renamed repository's
// new name, "" when no mapping names it.
func (r repoProjects) of(repo string) string {
	name := strings.ToLower(repo)
	if project, found := r.direct[name]; found {
		return project
	}
	return r.renamed[name]
}

// readRepoProjects reads every mapping of repo_projects (a row per repository Dispatch maps).
func readRepoProjects(ctx context.Context, s *Store) (repoProjects, error) {
	mapped := repoProjects{direct: map[string]string{}, renamed: map[string]string{}}
	rows, err := s.database.Pool.Query(ctx, `select repo, project, renamed_to from repo_projects order by repo`)
	if err != nil {
		return repoProjects{}, fmt.Errorf("read the repositories' projects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var repo, project string
		var renamedTo *string
		if err := rows.Scan(&repo, &project, &renamedTo); err != nil {
			return repoProjects{}, fmt.Errorf("read the repositories' projects: %w", err)
		}
		mapped.direct[strings.ToLower(repo)] = project
		if renamedTo != nil {
			mapped.renamed[strings.ToLower(*renamedTo)] = project
		}
	}
	return mapped, rows.Err()
}

// countedFor is every project the day counts the item member for (a pull request, a deploy run or
// a CI run, by its row key in eval_day_members), by key: none where the day holds no such item.
func (d DeliveryDay) countedFor(member string) []string {
	projects := map[string]bool{}
	for _, pr := range d.prs {
		if pr.member == member {
			projects[pr.project] = true
		}
	}
	for _, deploy := range d.deploys {
		if deploy.member == member {
			for _, c := range deploy.projects {
				projects[c.project] = true
			}
		}
	}
	for _, run := range slices.Concat(d.checks, d.pushes) {
		if run.member == member {
			projects[run.project] = true
		}
	}
	delete(projects, "")
	return slices.Sorted(maps.Keys(projects))
}

// linked is the items of other days whose rows the day's item member is built into, each with its
// own day: for a pull request merged on the day, the deploy run that ships it, whose start day's
// rows count it among the pull requests that run ships and count the run for its projects; for a
// deploy run started on the day, every pull request it ships, whose merge day's rows hold the
// run's completion as their deploy time. A change of the item's own row moves those days' rows
// too, which is why the refresher rewrites them (refresher_work.go).
func (d DeliveryDay) linked(member string) []dayItem {
	var items []dayItem
	for _, pr := range d.prs {
		if pr.member == member && pr.deploy != nil {
			items = append(items, *pr.deploy)
		}
	}
	for _, deploy := range d.deploys {
		if deploy.member == member {
			items = append(items, deploy.ships...)
		}
	}
	return items
}

// familyRows is one project's rows of a day, or one repository's of them, by family.
type familyRows struct {
	prs     []dayPR
	deploys []dayDeploy
	// deployVia is, for each of deploys, whether it reached the project through its repository.
	deployVia      []bool
	checks, pushes []delivery.DeliveryRun
}

func (f familyRows) empty() bool {
	return len(f.prs)+len(f.deploys)+len(f.checks)+len(f.pushes) == 0
}

// Rows is project's rows of the day, built by the measures package's day builders, and the items
// they count; no row and no item for a project no row of the day counts for. A read with a deploy
// repository builds the rows of the evals that read it alone, each and each item keyed by it; the
// read of the pull requests alone builds those of the evals no deploy repository moves, and its
// pull requests under none. A day before the start of the deploy repository's history (since)
// holds no row and no item of it.
func (d DeliveryDay) Rows(project string) ProjectDay {
	result := ProjectDay{Project: project, Day: d.Day}
	if d.deployRepo != "" && d.Day.Before(d.since) {
		return result
	}
	var all familyRows
	byRepo := map[string]*familyRows{}
	repoRows := func(repo string) *familyRows {
		if byRepo[repo] == nil {
			byRepo[repo] = &familyRows{}
		}
		return byRepo[repo]
	}
	members := map[Member]bool{}
	for _, pr := range d.prs {
		if pr.project != project {
			continue
		}
		all.prs = append(all.prs, pr)
		repoRows(pr.repo).prs = append(repoRows(pr.repo).prs, pr)
		member := Member{Family: KindDelivery, DeployRepo: d.deployRepo, RowKey: pr.member, Repo: pr.repo}
		if pr.deploy != nil {
			member.ShippedBy = pr.deploy.member
		}
		members[member] = true
	}
	for _, deploy := range d.deploys {
		index := slices.IndexFunc(deploy.projects, func(c counted) bool { return c.project == project })
		if index < 0 {
			continue
		}
		via := deploy.projects[index].via
		all.deploys, all.deployVia = append(all.deploys, deploy), append(all.deployVia, via)
		cut := repoRows(deploy.repo)
		cut.deploys, cut.deployVia = append(cut.deploys, deploy), append(cut.deployVia, via)
		members[Member{Family: KindDelivery, DeployRepo: d.deployRepo, RowKey: deploy.member, Repo: deploy.repo}] = true
	}
	for _, family := range []struct {
		runs []dayCIRun
		into func(rows *familyRows, run delivery.DeliveryRun)
	}{
		{d.checks, func(rows *familyRows, run delivery.DeliveryRun) { rows.checks = append(rows.checks, run) }},
		{d.pushes, func(rows *familyRows, run delivery.DeliveryRun) { rows.pushes = append(rows.pushes, run) }},
	} {
		for _, run := range family.runs {
			if run.project != project {
				continue
			}
			family.into(&all, run.run)
			family.into(repoRows(run.run.Repo), run.run)
			members[Member{Family: KindCI, DeployRepo: d.deployRepo, RowKey: run.member, Repo: run.run.Repo}] = true
		}
	}
	if all.empty() {
		return result
	}
	result.Rows = d.build(project, "", all)
	for _, repo := range slices.Sorted(maps.Keys(byRepo)) {
		result.Rows = append(result.Rows, d.build(project, repo, *byRepo[repo])...)
	}
	result.Members = slices.SortedFunc(maps.Keys(members), func(a, b Member) int {
		return cmp.Or(cmp.Compare(a.Family, b.Family), cmp.Compare(a.RowKey, b.RowKey))
	})
	return result
}

// ZeroDeliveryDay is project's all-repositories row of every delivery and CI series on day as the
// day builders build a day with no row (n 0, a zero sum, numerator and denominator, no sample, and
// zero counts), under settings, whose deploy cards decide which CI evals have one: the explicit
// zero rows the refresher writes for a series from its first input day on, on a day with no input
// (ProjectDay.WithZeros). Those of the evals no deploy repository moves are one row each, and
// those of the evals that read it one row for each of repos, keyed by it (deploy_repos.go).
// pr-ci's job series have none: a job with no result that day has no part.
func ZeroDeliveryDay(settings delivery.DeliverySettings, project string, day time.Time, repos []string) ProjectDay {
	zero := ProjectDay{Project: project, Day: measures.Midnight(day)}
	cards := measures.DeployCards(settings)
	for _, repo := range slices.Concat([]string{""}, repos) {
		d := DeliveryDay{Day: zero.Day, deployRepo: repo, cards: cards}
		zero.Rows = append(zero.Rows, d.buildFamilies(project, "", familyRows{}, true)...)
	}
	return zero
}

// build is the rows of project's cut repo ("" for all repositories) from rows, the families rows
// holds alone.
func (d DeliveryDay) build(project, repo string, rows familyRows) []DayRow {
	return d.buildFamilies(project, repo, rows, false)
}

// buildFamilies is build, and with every set, every family's rows whether rows holds that family
// or not (ZeroDeliveryDay's). Each part's via_repository is the n the same builder counts over the
// family's rows that reached the project through their repository alone. A day read with a deploy
// repository builds the rows of the evals that read it, keyed by it, and the read of the pull
// requests alone those of the evals no deploy repository moves (readsDeployRepo): a builder whose
// evals the read does not keep is not run.
func (d DeliveryDay) buildFamilies(project, repo string, rows familyRows, every bool) []DayRow {
	var built []DayRow
	keyed := d.deployRepo != ""
	add := func(slug string, parts, viaParts []measures.DayPart) {
		via := make(map[string]int, len(viaParts))
		for _, part := range viaParts {
			via[part.Series] = part.N
		}
		for _, part := range parts {
			part.ViaRepository = via[part.Series]
			built = append(built, DayRow{
				ProjectKey: project, EvalID: EvalID(project, slug), DeployRepo: d.deployRepo, Repo: repo,
				Listed: !strings.HasPrefix(part.Series, measures.JobSeriesPrefix), DayPart: part,
			})
		}
	}
	if every || len(rows.prs) > 0 {
		var prs, viaPRs []measures.PR
		for _, pr := range rows.prs {
			prs = append(prs, pr.pr)
			if pr.via {
				viaPRs = append(viaPRs, pr.pr)
			}
		}
		for _, builder := range []struct {
			slug  string
			build func(time.Time, []measures.PR) []measures.DayPart
		}{
			{SlugMergeToProduction, measures.MergeToProductionDay},
			{SlugFirstCommitToProduction, measures.FirstCommitToProductionDay},
			{SlugOpenToProduction, measures.OpenToProductionDay},
			{SlugPROpenToMerge, measures.OpenToMergeDay},
			{SlugReworkShare, measures.ReworkShareDay},
		} {
			if readsDeployRepo(builder.slug) == keyed {
				add(builder.slug, builder.build(d.Day, prs), builder.build(d.Day, viaPRs))
			}
		}
	}
	if keyed && (every || len(rows.deploys) > 0) {
		var runs, viaRuns []measures.Run
		for i, deploy := range rows.deploys {
			runs = append(runs, deploy.run)
			if rows.deployVia[i] {
				viaRuns = append(viaRuns, deploy.run)
			}
		}
		add(SlugDeployFrequency, measures.DeployFrequencyDay(d.Day, runs), measures.DeployFrequencyDay(d.Day, viaRuns))
		add(SlugDeployRunSuccess, measures.DeployRunSuccessDay(d.Day, runs), measures.DeployRunSuccessDay(d.Day, viaRuns))
	}
	// Every CI run reaches its project through its repository, so every result it counts does.
	if keyed && (every || len(rows.checks) > 0) {
		parts := measures.PRChecksDay(d.Day, rows.checks, d.checkJobs)
		add(SlugPRCI, parts, parts)
	}
	if keyed && (every || len(rows.pushes) > 0) {
		parts := measures.DeployJobsDay(d.Day, rows.pushes, d.pushJobs, d.cards)
		for _, card := range d.cards {
			add(cardSlugs[card.CardID], parts[card.CardID], parts[card.CardID])
		}
	}
	return built
}

// WriteProjectDay writes day as its project's delivery and CI rows of its day, in one transaction:
// each row with `insert … on conflict … do update … where (…) is distinct from (…)`, so a row
// whose every value is what it holds is left as it is (and logs no change, migration
// 20261117100014), recording builtBy, the running build, as the build that wrote it; then it
// writes each of the project's rows of that day of the delivery and CI evals (deliveryDaySlugs)
// that day does not hold, such as a repository whose last row moved to another day or project, as
// a zero row (zeroStaleDays). It records day's members in eval_day_members on its day, and deletes
// the members of the project's delivery and CI families recorded on that day that day does not
// hold, so each item's row there says where it was last counted; and it folds again the stored
// figures of the days whose rows it moved, where builtBy writes them (figures.go). A row of another
// project, another day or another eval, and an item of another family, is refused before anything
// is written.
func (s *Store) WriteProjectDay(ctx context.Context, day ProjectDay, builtBy string) error {
	return s.writeDeliveryDay(ctx, day, builtBy, true)
}

// writeDeliveryDay is WriteProjectDay, folding the stored figures of the days whose rows it moved
// where figures is set, as the refresher's write of a day outside a walk does (writeProjectDay).
func (s *Store) writeDeliveryDay(ctx context.Context, day ProjectDay, builtBy string, figures bool) error {
	return s.writeProjectDay(ctx, "delivery", day, deliveryDaySlugs, deliveryDayFamilies, builtBy, figures)
}

// WriteUnownedP0Day writes project's unowned-p0 row of the UTC day day is on: the count of its
// open P0 issues with no owner now (delivery.CountUnownedP0, the rule delivery.ListUnownedP0 lists
// by, read through the issues_open_p0 index) as the row's one value and its last_value, last_at
// the moment the count last moved, so the day's row is the count at its last refresh and the
// `last` fold of a window reads its newest day's. A write that counts what the row holds keeps
// the row's time, so it rewrites nothing (built_by aside) and logs no change: the refresher writes
// the row on every change that can move the count, most of which move nothing. builtBy is the
// running build. Its change row waits in the change log's outbox, so the transaction takes no
// change log lock, and is moved into the log once it has committed (drainChanges), as every
// writer of the day rows does; and its transaction folds again the stored figures of the day where
// its row moved and builtBy writes them (figures.go).
func (s *Store) WriteUnownedP0Day(ctx context.Context, project string, day time.Time, builtBy string) error {
	unowned, err := delivery.CountUnownedP0(ctx, s.database.Pool, project)
	if err != nil {
		return fmt.Errorf("count the unowned P0 issues of %s: %w", project, err)
	}
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the unowned-p0 row of %s: %w", project, err)
	}
	defer tx.Rollback(ctx)
	evalID, date := EvalID(project, SlugUnownedP0), measures.Midnight(day)
	var now time.Time
	var stored *float64
	var storedAt *time.Time
	if err := tx.QueryRow(ctx, `
		select now(), stored.last_value, stored.last_at
		from (select 1) as one
		left join eval_days stored on stored.project_key = $1 and stored.eval_id = $2 and stored.writer = $5
			and stored.series = $3 and stored.repo = '' and stored.day = $4
	`, project, evalID, SeriesUnownedP0, date, writerComputation).Scan(&now, &stored, &storedAt); err != nil {
		return fmt.Errorf("read the unowned-p0 row of %s: %w", project, err)
	}
	count, at := float64(unowned), now.UTC()
	if stored != nil && *stored == count && storedAt != nil {
		at = storedAt.UTC()
	}
	row := DayRow{
		ProjectKey: project, EvalID: evalID, Listed: true, DayPart: measures.DayPart{
			Series: SeriesUnownedP0, Day: date, N: 1, Sum: &count, Max: &count, LastValue: &count, LastAt: &at,
		},
	}
	if err := upsertDays(ctx, tx, []DayRow{row}, builtBy); err != nil {
		return err
	}
	if err := foldFiguresInTx(ctx, tx, builtBy, project, map[string]daySpan{evalID: {first: date, last: date}}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the unowned-p0 row of %s: %w", project, err)
	}
	s.drainChanges(ctx)
	return nil
}
