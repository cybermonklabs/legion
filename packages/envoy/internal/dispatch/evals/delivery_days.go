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

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The delivery and CI evals' day rows (dispatch://INFRA-2916, R12, R15, R16 and D1). One UTC day's
// delivery population is read once (readDeliveryDays, every day a page names at once), by the
// rules the delivery routes read their window by: delivery.ReadDeploys and ReadMergedRows' two
// reads (ListPullRequestsInWindow and ReadApplyRows), whose answer the routes' one read of a
// window, delivery.ReadWindowRows, gives too (TestAWindowsReadAnswersWhatItsTwoReadsAnswer holds
// it), and ReadCIRows, which the routes read with; each row of it is attributed to the projects it
// counts for; each project's rows are built into day parts by the measures package's day builders,
// the builders the delivery routes build their window's parts with (DeliveryDay.Rows); and the
// refresher writes the parts as that project's rows of the day (a walk's day through
// refresh.writeDelivery and walkWrite, writeProjectDay, or staged by a rebuild; a page's days
// through writeNamed, writeProjectDays), which the eval routes and the delivery routes fold
// through the same folds.
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
// Attribution, decided in one place (attribution), by R15: a pull request counts for its issue's
// project, and one with no issue for each project whose default repository is its repository. Its
// issue is delivery's attribution (delivery/attribution.go): the first of its ranked candidates
// whose issue resolves to its repository, where an issue resolves to the repository its own chain
// names (an ancestor's of its project included) or, where its chain names none, to its project's
// default repository. So what a pull request counts for can hang on the default repositories, and
// its rows read those the evals refresher publishes (eval_days_defaults), at read time. Its
// candidates make its attribution list (attributionListOf): its conditions, the projects of the
// candidates ranked above its terminal whose issues own no repository, each held exactly when its
// published default is the pull request's repository; and its terminal, the project of the first
// candidate whose issue owns the pull request's repository. A list with no condition counts for
// its terminal in that project's own rows, or, with no terminal either, under its repository's
// plain owner key (owners.go), which each project whose default is the repository reads. A list
// with a condition counts under an owner key of its repository that carries it, which the project
// it picks under the published defaults reads (its first condition whose default is the
// repository, else its terminal), or, where it picks none, each project whose default is the
// repository (eval_owner_readers, migration 20261117100006). A pull request whose inputs are not
// stored (not read from GitHub yet) has the one list its row's issue_key gives: that issue's
// project as its terminal, or none. A PR-checks run counts where its pull request counts, under the
// same key, and so does each of its jobs; one of no pull request, or of one no row holds, counts
// under its repository's plain owner key. A deploy run counts for the terminal of each constant
// list the pull requests it ships hold, in that project's own rows, once in each however many of
// a project's it ships, and so does each job of it a deploy card follows, its push run on main
// being that deploy; where it ships a pull request whose list is not constant or has no terminal,
// or ships none (one that did not reach production among them), it counts once more under an owner
// key of its repository that names those projects as its sharers and carries the distinct lists of
// the rest, which each project those lists or the repository's default reach reads once. A push
// run on main that is no deploy run of the day counts under its repository's plain owner key.
//
// The candidates are read from the database alone (delivery.Attributor.ResolvedCandidates): the
// pull request's stored inputs, the issues' external links, and the issues with their chains. A
// move of a project's default repository moves no list, so it rewrites no day row: the page that
// publishes it moves the readers (publishDefaults). Any other input that moves a list is a change
// the refresher reads where it moves the issue delivery_pull_requests.issue_key names, which the
// reconcile's sweep writes again a batch at a time (delivery.Attributor.AttributePullRequests).
// One change moves a list and not that issue: a link, a reparent or a change of an issue's own
// repository that changes a candidate not held under the live defaults (a condition gained or
// lost, or a terminal ranked below the held issue). Its day is rebuilt when the sweep or another
// change next names that pull request, as a day a change of issue_key moves is.
//
// A day row's via_repository is how many of the rows its n counts reached their owner through
// their repository rather than through an issue (its builder's n over those rows alone): every row
// of an owner key counts it, and no row of a project. A project reads it of an owner key's rows
// only where it reads them through the repository (eval_project_owners.via: a plain key, or every
// list of the key that reaches it picks none), and reads 0 where a list picks it, which is through
// its issue (viaAsRead), so a project's read of its own rows and its owner keys' adds up the rows
// it counts through a repository. It is never more than n, and the repositories' rows add up to
// the all-repositories row's as their n do.
//
// A project's rows of a day are, for each family of rows it holds that day (its pull requests,
// its deploy runs, its PR-checks runs, its deploy push runs), the all-repositories row (repo '')
// of every series of that family's evals, built from every row of the family counted for the
// project, and one row of each such series per repository the family's rows are in, built from
// that repository's rows alone (the pull request's repository, the run's), where it counts
// something: a repository's part that is empty, such as a lead time of the day's pull requests
// when no deploy has shipped any of them yet, is no row (repositoryRows), since R12 writes no zero
// row for a repository, and that repository's day reads "no data" for the series: each delivery
// and CI eval with day rows lists one series, so the day holds no row of the eval (cutZeros). So
// every row counted is in one repository's rows, and the repositories' rows of a series partition
// its all-repositories row, an empty part adding nothing: their n, sum, num, den, counts and
// via_repository add up to its own, and their samples are its samples. A family a project holds no
// row of that day writes no row: the explicit zero rows of a series from its first input day on
// are the refresher's, in the all-repositories row alone (ZeroDeliveryDay, ProjectDay.WithZeros).
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

// SeriesUnownedP0 is unowned-p0's one series: the open P0 issues with no owner, the count of the
// newest day's rows (unownedP0Statistics).
const SeriesUnownedP0 = "issues"

// deliveryDaySlugs is every eval a delivery day's rows are of, and so every eval whose rows a day
// no longer holds the write of the day deletes (deleteStaleDays): neither change-fail-rate nor
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

// counted is one project or owner key (owners.go) a row counts for, and whether the row reached it
// through its repository rather than through an issue (via_repository): every row of an owner key
// did, whichever project reads it (viaAsRead says how each reads it), and no row of a project's own.
type counted struct {
	project string
	via     bool
}

// dayPR is one pull request merged on the day, as the builders read it, with its repository, its
// row key, the project or owner key it counts for and the deploy run that ships it, nil while
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

// dayCIRun is one CI run started on the day, with its row key and what it counts for: a PR-checks
// run where its pull request counts (attribution.check), a deploy push run where its deploy counts
// (attribution.pushRuns), each a project or its repository's owner key.
type dayCIRun struct {
	run      delivery.DeliveryRun
	member   string
	projects []counted
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

// deliveryRead is one UTC day's delivery and CI rows (readDeliveryDays): the day's pull requests
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

// readDeliveryDays reads the delivery and CI rows of each of days (UTC midnights, in order and
// each once), under settings, with each of repos as the deploy repository, and attributes each to
// its projects: answers index-aligned with days. Its reads answer, for each day, what the delivery
// routes' read of a window of that day answers with that repository as the deploy repository: the
// day's deploys and the rows before the day their shipped sets read (delivery.ReadDeploys), the
// pull requests merged in the day (delivery.ListPullRequestsInWindow) and the deploy runs created
// from the day's start on that can ship them (the routes' bound, windowApplyRunsCreated), which are
// delivery.ReadWindowRows' rows (TestAWindowsReadAnswersWhatItsTwoReadsAnswer holds them to
// delivery.ReadMergedRows' two reads); the day's CI runs (delivery.ReadCIRows); and what
// attributes them (readAttribution): the attribution list of each of those pull requests and of
// the pull requests the day's PR-checks runs are of. Each read is made once for every day
// (delivery.ListPullRequestsOnDays, ReadDaysDeploys and ReadCIDays), so a page naming many days
// makes as many statements of each as a page naming one, every job of each day's PR-checks runs
// aside, which is read a day at a time, a few days side by side; the pull requests are read once,
// and every other read once for each repository. Each deploy's shipped pull requests are
// delivery.Deploys.ShippedBy's, the rule the routes count with_prs by; the pull requests merged
// before a day are read for them alone. A repository's deploy runs created from a day's start on
// are taken from applyRows where it holds them (delivery.ApplyRows.Since): rows a walk read once
// from the first of its days, every run created from then to now, which every later day of the
// walk takes its part of. The reads of the rows depend on none of each other, and are read side by
// side, and then the reads of what attributes them, side by side too.
func (s *Store) readDeliveryDays(ctx context.Context, settings delivery.DeliverySettings, repos []keptRepo, days []time.Time,
	applyRows map[string]*delivery.ApplyRows) ([]deliveryRead, error) {
	pool := s.database.Pool
	cards := measures.DeployCards(settings)
	cardNames := measures.DeployCardJobNames(cards)

	// repoDays is what the days read of one repository as the deploy repository.
	type repoDays struct {
		settings delivery.DeliverySettings
		since    time.Time
		deploys  delivery.DaysDeploys
		ci       []delivery.CIRows
	}
	var prs [][]delivery.DeliveryPullRequest
	reads := make([]repoDays, len(repos))
	parts := []func(context.Context) error{func(ctx context.Context) (err error) {
		if prs, err = delivery.ListPullRequestsOnDays(ctx, pool, days); err != nil {
			return fmt.Errorf("read the pull requests of %s: %w", daysOf(days), err)
		}
		return nil
	}}
	for i, kept := range repos {
		repo, one := kept.name, &reads[i]
		one.settings, one.since = settings, kept.since
		one.settings.DeployRepo = repo
		parts = append(parts,
			func(ctx context.Context) (err error) {
				if one.deploys, err = delivery.ReadDaysDeploys(ctx, pool, one.settings, days, applyRows[repo] == nil); err != nil {
					return fmt.Errorf("read the deploys of %s on %s: %w", repo, daysOf(days), err)
				}
				return nil
			},
			func(ctx context.Context) (err error) {
				if one.ci, err = delivery.ReadCIDays(ctx, pool, repo, days, cardNames); err != nil {
					return fmt.Errorf("read the CI runs of %s on %s: %w", repo, daysOf(days), err)
				}
				return nil
			})
	}
	if err := store.ReadSideBySide(ctx, parts...); err != nil {
		return nil, err
	}
	var pulls []delivery.DeliveryPullRequest
	var checks []delivery.DeliveryRun
	for d := range days {
		pulls = append(pulls, prs[d]...)
		for _, one := range reads {
			pulls = append(pulls, one.deploys.Deploys[d].Earlier.PullRequests...)
			checks = append(checks, one.ci[d].Checks.Runs...)
		}
	}
	attribute, err := readAttribution(ctx, s, pulls, checks)
	if err != nil {
		return nil, err
	}

	built := make([]deliveryRead, len(days))
	for d, day := range days {
		read := deliveryRead{Day: day, repos: deployRepoNames(repos), days: make([]DeliveryDay, 0, len(repos)+1)}
		alone := DeliveryDay{Day: day, cards: cards}
		for _, pr := range prs[d] {
			// A pull request whose opening is not known yet (a partial row) has no lead time to
			// count, as the delivery routes leave it out of theirs.
			if pr.CreatedAt != nil {
				alone.prs = append(alone.prs, attribute.pullRequest(pr, nil, nil))
			}
		}
		read.days = append(read.days, alone)
		for _, one := range reads {
			held := &one.deploys.Applies
			if walked := applyRows[one.settings.DeployRepo]; walked != nil {
				held = walked
			}
			applies, err := held.Since(day)
			if err != nil {
				return nil, fmt.Errorf("read the deploy runs of %s from %s on: %w", one.settings.DeployRepo, day.Format(time.DateOnly), err)
			}
			read.days = append(read.days, attribute.dayAs(day, cards, prs[d], repoReads{
				settings: one.settings, since: one.since, deploys: one.deploys.Deploys[d], applies: applies, ci: one.ci[d],
			}))
		}
		built[d] = read
	}
	return built, nil
}

// daysOf names days in an error: the one day, or the first and the last.
func daysOf(days []time.Time) string {
	switch len(days) {
	case 0:
		return "no day"
	case 1:
		return days[0].Format(time.DateOnly)
	}
	return fmt.Sprintf("%d days from %s to %s", len(days), days[0].Format(time.DateOnly), days[len(days)-1].Format(time.DateOnly))
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
		// A partial row, whose opening is not known yet, as readDeliveryDays leaves it out.
		if pr.CreatedAt != nil {
			read.prs = append(read.prs, a.pullRequest(pr, delivery.ContainingRun(pr, settings.DeployRepo, applies), applyRuns))
		}
	}
	shipped := one.deploys.ShippedBy(delivery.MergedRows{PullRequests: prs, ApplyRuns: one.applies.Runs, ApplyJobs: one.applies.Jobs}, settings)
	for _, run := range one.deploys.Started.Runs {
		read.deploys = append(read.deploys, a.deploy(run, reached[run.RunID], shipped[run.RunID]))
	}
	read.checks, read.checkJobs = a.checkRuns(one.ci.Checks.Runs), one.ci.Checks.Jobs
	read.pushes, read.pushJobs = a.pushRuns(one.ci.Deploys.Runs, read.deploys), one.ci.Deploys.Jobs
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

// owners is every owner key (OwnerKey) any of the day's reads counts an item for, in key order:
// the repositories' rows the day builds beside the projects'.
func (d deliveryRead) owners() []string {
	owners := map[string]bool{}
	add := func(counts []counted) {
		for _, c := range counts {
			if isOwner(c.project) {
				owners[c.project] = true
			}
		}
	}
	for _, day := range d.days {
		for _, pr := range day.prs {
			add([]counted{pr.counted})
		}
		for _, deploy := range day.deploys {
			add(deploy.projects)
		}
		for _, run := range slices.Concat(day.checks, day.pushes) {
			add(run.projects)
		}
	}
	return slices.Sorted(maps.Keys(owners))
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

// attributionList is what decides the projects one pull request counts for (the Attribution
// paragraph above), as its ranked candidates make it (attributionListOf): its conditions, the
// projects of the candidates ranked above its terminal whose issues own no repository, each once
// and in rank order, each held exactly when that project's published default repository is the
// pull request's repository; and its terminal, the project of the first candidate whose issue
// owns the pull request's repository, "" for none. Under the published defaults it picks its first
// condition held, else its terminal, else none, which the repository's owners read: what
// eval_owner_readers decides at read time for an owner key that carries it (migration
// 20261117100006).
type attributionList struct {
	conditions []string
	terminal   string
}

// constant reports whether list picks the same whatever the defaults are: its terminal, or none.
func (l attributionList) constant() bool {
	return len(l.conditions) == 0
}

// none reports whether list holds neither a condition nor a terminal: it picks none whatever the
// defaults are.
func (l attributionList) none() bool {
	return l.constant() && l.terminal == ""
}

// String is list as an owner key carries it (OwnerKey) and eval_day_owners.lists keeps it: its
// conditions in rank order joined by ',', then '>', then its terminal ("" for none).
func (l attributionList) String() string {
	return strings.Join(l.conditions, ",") + ">" + l.terminal
}

// attributionListOf is the attribution list a pull request's candidates make, in their rank
// order (delivery.Attributor.ResolvedCandidates): a candidate whose issue no row holds, or whose
// chain ran out of depth, is passed over, as the attribution passes it over; one whose issue owns
// the pull request's repository ends the list as its terminal; one whose issue owns another
// repository is passed over, held to no default; and one whose issue owns none adds its project
// as a condition, once. A list whose candidates reach no terminal has none.
func attributionListOf(candidates []delivery.ResolvedCandidate) attributionList {
	var list attributionList
	for _, candidate := range candidates {
		switch {
		case candidate.Project == "" || candidate.TooDeep:
		case candidate.Owned != "":
			if candidate.OwnedHere {
				list.terminal = candidate.Project
				return list
			}
		case !slices.Contains(list.conditions, candidate.Project):
			list.conditions = append(list.conditions, candidate.Project)
		}
	}
	return list
}

// attribution is what a day's rows are attributed to their projects by, and the one place the rule
// is decided (the Attribution paragraph above): the attribution list of each pull request a day's
// rows count, by its row key (PullRequestMember), for each pull request a row holds
// (readAttribution). readDeliveryDays attributes the rows of each day it reads by it, a span the
// rows of each day it builds (deliverySpanInput), and the refresher's rewrite of a settings change
// the rows of each day it builds (rewriteSettings).
type attribution struct {
	lists map[string]attributionList
}

// counts is what a row of repo that list decides counts for: its terminal's project, where list
// is constant and has one; else an owner key of repo carrying list, through the repository, which
// is the plain key where list holds nothing.
func (a attribution) counts(repo string, list attributionList) counted {
	if list.constant() && list.terminal != "" {
		return counted{project: list.terminal}
	}
	return counted{project: OwnerKey(repo, nil, []attributionList{list}), via: true}
}

// of is what pr counts for, by its attribution list (counts).
func (a attribution) of(pr delivery.DeliveryPullRequest) counted {
	return a.counts(pr.Repo, a.lists[PullRequestMember(pr.Repo, pr.Number)])
}

// check is what a PR-checks run counts for: where its pull request counts (counts, by the pull
// request's list), under its repository's plain owner key where it names no pull request or one no
// row holds.
func (a attribution) check(run delivery.DeliveryRun) []counted {
	var list attributionList
	if run.PRNumber != nil {
		list = a.lists[PullRequestMember(run.Repo, *run.PRNumber)]
	}
	return []counted{a.counts(run.Repo, list)}
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
// reached production, the pull requests it ships first (Deploys.ShippedBy's), and what it counts
// for: the terminal of each constant list its pull requests hold, once each, and, where it ships a
// pull request whose list is not constant or has no terminal, or ships none, an owner key of its
// repository with those projects as its sharers and the distinct lists of the rest.
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
	// Once in each project a constant list of its pull requests counts for; once more under its
	// repository's key where any of them counts by another list, or none ships.
	owned := len(shipped) == 0
	var sharers []string
	var lists []attributionList
	for _, pr := range shipped {
		list := a.lists[PullRequestMember(pr.Repo, pr.Number)]
		if !list.constant() || list.terminal == "" {
			owned, lists = true, append(lists, list)
			continue
		}
		if !slices.Contains(sharers, list.terminal) {
			deploy.projects = append(deploy.projects, counted{project: list.terminal})
			sharers = append(sharers, list.terminal)
		}
	}
	if owned {
		deploy.projects = append(deploy.projects, counted{project: OwnerKey(run.Repo, sharers, lists), via: true})
	}
	return deploy
}

// checkRuns is runs, PR-checks runs started on the day, each attributed to what its pull request
// counts for (check).
func (a attribution) checkRuns(runs []delivery.DeliveryRun) []dayCIRun {
	attributed := make([]dayCIRun, len(runs))
	for i, run := range runs {
		attributed[i] = dayCIRun{run: run, member: RunMember(run.Repo, run.RunID), projects: a.check(run)}
	}
	return attributed
}

// pushRuns is runs, the deploy workflow's push runs on main started on the day, each attributed
// to what the deploy it is counts for: deploys is every deploy run on main started on the day
// (deploy), which holds every push run on main among them, and a run it does not hold counts for
// its repository's plain owner key.
func (a attribution) pushRuns(runs []delivery.DeliveryRun, deploys []dayDeploy) []dayCIRun {
	projects := make(map[string][]counted, len(deploys))
	for _, deploy := range deploys {
		projects[deploy.member] = deploy.projects
	}
	attributed := make([]dayCIRun, len(runs))
	for i, run := range runs {
		member := RunMember(run.Repo, run.RunID)
		counts, held := projects[member]
		if !held {
			counts = []counted{{project: OwnerKey(run.Repo, nil, nil), via: true}}
		}
		attributed[i] = dayCIRun{run: run, member: member, projects: counts}
	}
	return attributed
}

// readAttribution reads what attributes a day's rows (attribution): the attribution list of each
// of prs, the pull requests the day's rows count, and of the pull request each of checks, PR-checks
// runs, names, where a row holds it. A pull request whose attribution inputs are stored has the
// list its candidates make (delivery.Attributor.ResolvedCandidates, attributionListOf); one whose
// inputs are not stored yet the one its row's issue_key gives, that issue's project as its
// terminal, or none where it names no issue the issues table holds. Of prs, whose rows the day
// read already, it reads the projects of the issues named by those with no inputs; of a PR-checks
// run's pull request, whose row it has not read, the project of the issue its row names where its
// row holds no inputs. The three reads depend on none of each other, and are read side by side;
// none is made where it has nothing to read. Each sends its list in statements of at most
// statementBytes (statementChunks): a span's and a settings rewrite's pull requests are every one
// the history holds, which grow with it.
func readAttribution(ctx context.Context, s *Store, prs []delivery.DeliveryPullRequest, checks []delivery.DeliveryRun) (attribution, error) {
	pool := s.database.Pool
	var resolve []delivery.PullRequestKey
	var keys []string
	keyed := map[string][]string{}
	// checked is a PR-checks run's pull request: its row key, its repository and its number.
	type checked struct {
		member, repo string
		number       int32
	}
	var pulls []checked
	named := map[string]bool{}
	for _, pr := range prs {
		member := PullRequestMember(pr.Repo, pr.Number)
		if named[member] {
			continue
		}
		named[member] = true
		switch {
		case pr.Attribution != nil:
			resolve = append(resolve, delivery.PullRequestKey{Repo: pr.Repo, Number: pr.Number})
		case pr.IssueKey != nil:
			if len(keyed[*pr.IssueKey]) == 0 {
				keys = append(keys, *pr.IssueKey)
			}
			keyed[*pr.IssueKey] = append(keyed[*pr.IssueKey], member)
		}
	}
	for _, run := range checks {
		if run.PRNumber == nil {
			continue
		}
		member := PullRequestMember(run.Repo, *run.PRNumber)
		if named[member] {
			continue
		}
		named[member] = true
		resolve = append(resolve, delivery.PullRequestKey{Repo: run.Repo, Number: *run.PRNumber})
		pulls = append(pulls, checked{member: member, repo: run.Repo, number: int32(*run.PRNumber)})
	}
	resolved := map[delivery.PullRequestKey][]delivery.ResolvedCandidate{}
	issueProjects, checkProjects := map[string]string{}, map[string]string{}
	var reads []func(context.Context) error
	if len(resolve) > 0 {
		reads = append(reads, func(ctx context.Context) error {
			size := func(pull *delivery.PullRequestKey) int { return 4 + len(pull.Repo) + 4 + 4 }
			for chunk := range statementChunks(resolve, paramBytes(2), size) {
				found, err := s.attributor.ResolvedCandidates(ctx, pool, chunk)
				if err != nil {
					return err
				}
				maps.Copy(resolved, found)
			}
			return nil
		})
	}
	if len(keys) > 0 {
		reads = append(reads, func(ctx context.Context) error {
			for chunk := range statementChunks(keys, paramBytes(1), textBytes) {
				rows, err := pool.Query(ctx, `select key, project_key from issues where key = any($1)`, chunk)
				if err != nil {
					return fmt.Errorf("read the projects of the pull requests' issues: %w", err)
				}
				var key, project string
				if _, err := pgx.ForEachRow(rows, []any{&key, &project}, func() error {
					issueProjects[key] = project
					return nil
				}); err != nil {
					return fmt.Errorf("read the projects of the pull requests' issues: %w", err)
				}
			}
			return nil
		})
	}
	if len(pulls) > 0 {
		reads = append(reads, func(ctx context.Context) error {
			size := func(pull *checked) int { return 4 + len(pull.member) + 4 + len(pull.repo) + 4 + 4 }
			for chunk := range statementChunks(pulls, paramBytes(3), size) {
				members, repos, numbers := make([]string, len(chunk)), make([]string, len(chunk)), make([]int32, len(chunk))
				for i, pull := range chunk {
					members[i], repos[i], numbers[i] = pull.member, pull.repo, pull.number
				}
				rows, err := pool.Query(ctx, `
					select given.member, issue.project_key
					from unnest($1::text[], $2::text[], $3::int[]) as given(member, repo, number)
					join delivery_pull_requests request on request.repo = given.repo and request.number = given.number
					join issues issue on issue.key = request.issue_key
					where request.attribution_title_keys is null
				`, members, repos, numbers)
				if err != nil {
					return fmt.Errorf("read the issues of the %d pull requests the PR-checks runs are of: %w", len(pulls), err)
				}
				var member, project string
				if _, err := pgx.ForEachRow(rows, []any{&member, &project}, func() error {
					checkProjects[member] = project
					return nil
				}); err != nil {
					return fmt.Errorf("read the issues of the %d pull requests the PR-checks runs are of: %w", len(pulls), err)
				}
			}
			return nil
		})
	}
	if err := store.ReadSideBySide(ctx, reads...); err != nil {
		return attribution{}, err
	}
	attribute := attribution{lists: make(map[string]attributionList, len(named))}
	for key, project := range issueProjects {
		for _, member := range keyed[key] {
			attribute.lists[member] = attributionList{terminal: project}
		}
	}
	for member, project := range checkProjects {
		attribute.lists[member] = attributionList{terminal: project}
	}
	for pull, candidates := range resolved {
		attribute.lists[PullRequestMember(pull.Repo, pull.Number)] = attributionListOf(candidates)
	}
	return attribute, nil
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
			for _, c := range run.projects {
				projects[c.project] = true
			}
		}
	}
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
	// deployVia, checkVia and pushVia are, for each of deploys, checks and pushes, whether it
	// reached the project through its repository.
	deployVia         []bool
	checks, pushes    []delivery.DeliveryRun
	checkVia, pushVia []bool
}

func (f familyRows) empty() bool {
	return len(f.prs)+len(f.deploys)+len(f.checks)+len(f.pushes) == 0
}

// Rows is project's rows of the day, built by the measures package's day builders, and the items
// they count; no row and no item for a project no row of the day counts for. A repository's row
// stands only where its part counts something (repositoryRows), and every item the day counts is
// recorded whether or not its repository's rows stand. A read with a deploy repository builds the
// rows of the evals that read it alone, each and each item keyed by it; the read of the pull
// requests alone builds those of the evals no deploy repository moves, and its pull requests under
// none. A day before the start of the deploy repository's history (since) holds no row and no item
// of it.
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
		into func(rows *familyRows, run delivery.DeliveryRun, via bool)
	}{
		{d.checks, func(rows *familyRows, run delivery.DeliveryRun, via bool) {
			rows.checks, rows.checkVia = append(rows.checks, run), append(rows.checkVia, via)
		}},
		{d.pushes, func(rows *familyRows, run delivery.DeliveryRun, via bool) {
			rows.pushes, rows.pushVia = append(rows.pushes, run), append(rows.pushVia, via)
		}},
	} {
		for _, run := range family.runs {
			index := slices.IndexFunc(run.projects, func(c counted) bool { return c.project == project })
			if index < 0 {
				continue
			}
			via := run.projects[index].via
			family.into(&all, run.run, via)
			family.into(repoRows(run.run.Repo), run.run, via)
			members[Member{Family: KindCI, DeployRepo: d.deployRepo, RowKey: run.member, Repo: run.run.Repo}] = true
		}
	}
	if all.empty() {
		return result
	}
	result.Rows = d.build(project, "", all)
	for _, repo := range slices.Sorted(maps.Keys(byRepo)) {
		result.Rows = append(result.Rows, repositoryRows(d.build(project, repo, *byRepo[repo]))...)
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
// pr-ci's job series have none: a job with no result that day has no part. These are project
// totals even when keyed by deploy repository: a repository's cut gets no zero row, built or
// explicit (repositoryRows, R12), so its day with no row of a series reads "no data".
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
	if keyed && (every || len(rows.checks) > 0) {
		parts := measures.PRChecksDay(d.Day, rows.checks, d.checkJobs)
		add(SlugPRCI, parts, measures.PRChecksDay(d.Day, viaOnly(rows.checks, rows.checkVia), d.checkJobs))
	}
	if keyed && (every || len(rows.pushes) > 0) {
		parts := measures.DeployJobsDay(d.Day, rows.pushes, d.pushJobs, d.cards)
		viaParts := measures.DeployJobsDay(d.Day, viaOnly(rows.pushes, rows.pushVia), d.pushJobs, d.cards)
		for _, card := range d.cards {
			add(cardSlugs[card.CardID], parts[card.CardID], viaParts[card.CardID])
		}
	}
	return built
}

// viaOnly is the runs of runs whose via is set, those that reached the project through their
// repository, in their order.
func viaOnly(runs []delivery.DeliveryRun, via []bool) []delivery.DeliveryRun {
	var kept []delivery.DeliveryRun
	for i, run := range runs {
		if via[i] {
			kept = append(kept, run)
		}
	}
	return kept
}

// WriteUnownedP0Day writes project's unowned-p0 rows of the UTC day day is on: its open P0 issues
// with no owner now (delivery.ListUnownedP0, read through the issues_open_p0 index), counted in
// the project's own row and in a row of each repository they resolve to, by the rule the
// Legion day rows cut an issue by (legionfacts.ReadRepos: its own repository, else an ancestor's,
// else legionfacts.DefaultRepo, which stands for the project's published default repository); and
// a row of 0 for each repository whose newest row at or before the day counts an issue and which
// counts none now, so a read of that repository never reads a count that went away. A repository
// whose newest row is 0 gets no row, and a window that holds no row of it reads "no data" (R12).
// Each row's count is its n, sum, max and last value, so the repositories' rows add up to the
// project's, n and sum alike, and a read of repositories folds the newest day's rows it reads
// (unownedP0Statistics). An issue whose parent chain the walk could not finish resolves to none and
// is counted in the project's row alone, as in the Legion rows. A day written before the rows were
// kept by repository holds the project's row alone, with n 1: a read filtered by repositories
// finds no row of that day, so a window of such days alone reads "no data" (R12), and a window
// that holds later days too reads the newest of those.
//
// Each row's last_at is the moment its count last moved, so the day's rows are the counts at its
// last refresh. A write that counts what a row holds keeps that row's time, so it rewrites nothing
// and logs no change: the refresher writes the rows on every change that can move a count, most of
// which move nothing. builtBy is the running build. Its change row waits in the change log's
// outbox, so the transaction takes no change log lock, and is moved into the log once it has
// committed (drainChanges), as every writer of the day rows does; and its transaction folds again
// the stored figures of the day where its rows moved and builtBy writes them (figures.go).
func (s *Store) WriteUnownedP0Day(ctx context.Context, project string, day time.Time, builtBy string) error {
	pool := s.database.Pool
	issues, err := delivery.ListUnownedP0(ctx, pool, project)
	if err != nil {
		return fmt.Errorf("list the unowned P0 issues of %s: %w", project, err)
	}
	keys := make([]string, len(issues))
	for index, issue := range issues {
		keys[index] = issue.Key
	}
	repos, err := legionfacts.ReadRepos(ctx, pool, keys)
	if err != nil {
		return fmt.Errorf("the unowned P0 issues of %s: %w", project, err)
	}
	// counts is each row's count, by its repository, "" for the project's own.
	counts := map[string]int{"": len(keys)}
	for _, key := range keys {
		if repo := repos[key]; repo != "" {
			counts[repo]++
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the unowned-p0 rows of %s: %w", project, err)
	}
	defer tx.Rollback(ctx)
	evalID, date := EvalID(project, SlugUnownedP0), measures.Midnight(day)
	// newest is each repository's newest row at or before the day, the project's among them, read
	// over the primary key's range of the eval's series.
	type newestRow struct {
		day   time.Time
		value float64
		at    time.Time
	}
	newest := map[string]newestRow{}
	rows, err := tx.Query(ctx, `
		select now(), stored.repo, stored.day, stored.last_value, stored.last_at
		from (select 1) as one
		left join lateral (
			select distinct on (repo) repo, day, last_value, last_at from eval_days
			where project_key = $1 and eval_id = $2 and writer = $3 and deploy_repo = '' and series = $4 and day <= $5
			order by repo, day desc
		) stored on true
	`, project, evalID, writerComputation, SeriesUnownedP0, date)
	if err != nil {
		return fmt.Errorf("read the unowned-p0 rows of %s: %w", project, err)
	}
	var now time.Time
	var repo *string
	var storedDay, storedAt *time.Time
	var stored *float64
	if _, err := pgx.ForEachRow(rows, []any{&now, &repo, &storedDay, &stored, &storedAt}, func() error {
		if repo != nil && stored != nil && storedAt != nil {
			newest[*repo] = newestRow{day: storedDay.UTC(), value: *stored, at: storedAt.UTC()}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read the unowned-p0 rows of %s: %w", project, err)
	}
	written := make([]DayRow, 0, len(counts)+len(newest))
	write := func(repo string, n int) {
		count, at := float64(n), now.UTC()
		if last, held := newest[repo]; held && last.day.Equal(date) && last.value == count {
			at = last.at
		}
		written = append(written, DayRow{ProjectKey: project, EvalID: evalID, Repo: repo, Listed: true, DayPart: measures.DayPart{
			Series: SeriesUnownedP0, Day: date, N: n, Sum: &count, Max: &count, LastValue: &count, LastAt: &at,
		}})
	}
	for repo, n := range counts {
		write(repo, n)
	}
	for repo, last := range newest {
		if _, counted := counts[repo]; !counted && last.value > 0 {
			write(repo, 0)
		}
	}
	if err := upsertDays(ctx, tx, written, builtBy); err != nil {
		return err
	}
	if err := foldFiguresInTx(ctx, tx, builtBy, map[string]map[string]daySpan{project: {evalID: {first: date, last: date}}}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the unowned-p0 rows of %s: %w", project, err)
	}
	s.drainChanges(ctx)
	return nil
}

// unownedP0Statistics is unowned-p0's fold of parts, its day rows of a range (computedSources):
// value, the count of the newest day that holds a row, the sum of that day's last values; and n,
// the sum of the rows' n, the issues they count. A day of one cut can hold several rows, each the
// count of its own issues: the published default repository's cut reads that repository's row and
// the project's row stored under legionfacts.DefaultRepo (repoCut, cutOf), and a read of several
// repositories each one's, so that day's count is their sum, as the project's own row is the sum
// of its repositories' (WriteUnownedP0Day). Every figure is nil where parts hold no row of the
// series.
func unownedP0Statistics(parts []measures.DayPart, _ Range) Statistics {
	stats := blank("value", "n")
	n, held, counted := 0, false, false
	var newest time.Time
	var value float64
	for _, part := range parts {
		if part.Series != SeriesUnownedP0 {
			continue
		}
		n, held = n+part.N, true
		switch {
		case part.LastValue == nil:
		case !counted || part.Day.After(newest):
			newest, value, counted = part.Day, *part.LastValue, true
		case part.Day.Equal(newest):
			value += *part.LastValue
		}
	}
	if held {
		stats["n"] = new(float64(n))
	}
	if counted {
		stats["value"] = new(value)
	}
	return stats
}
