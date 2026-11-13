package evals

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// The delivery and CI evals' day rows (dispatch://INFRA-2916, R12, R15, R16 and D1). One UTC day's
// delivery population is read once (ReadDeliveryDay), by the reads the delivery routes read their
// window with (delivery.ReadMergedRows, ListDeployRunsStartedIn and ReadCIRows); each row of it is
// attributed to the projects it counts for; each project's rows are built into day parts by the
// measures package's day builders, the builders the delivery routes build their window's parts
// with (DeliveryDay.Rows); and the parts are written as that project's rows of the day
// (WriteProjectDay), which the eval routes and the delivery routes fold through the same folds.
//
// A day holds the population pull requests merged that day (merge-to-production,
// first-commit-to-production, open-to-production, pr-open-to-merge and rework-share), the deploy
// runs on main started that day (deploy-frequency and deploy-run-success), the PR-checks
// workflow's pull_request runs started that day with every job (pr-ci, and its unlisted
// `job:<name>` series), and the deploy workflow's push runs on main started that day with the
// jobs the deploy cards follow (production-deploy, staging-e2e and staging-deploy, the last two
// where the settings name their jobs). A pull request's deploy is its first shipping apply
// (delivery.ContainingRun) among the deploy runs GitHub created from its merge on, unbounded, as
// the delivery routes read it. A deploy's shipped set is every population pull request whose first
// shipping apply it is, whatever day that pull request merged: the deploy's own, which counts it
// as a deploy with pull requests (with_prs) and names the projects it counts for. (The delivery
// routes count with_prs over the pull requests merged in their window alone, so a deploy early in
// a window that shipped only pull requests merged before it is one with pull requests here and
// one without in a route's answer for that window.) Two kinds of row are not settled when their
// day is first built (measures.go names them): a pull request with no deploy yet and a deploy run
// still running; the day is built again when the run that settles them lands. change-fail-rate and
// failed-deploy-recovery get no rows: no failure source exists, so they read "no data".
//
// Attribution (D1, root R27). A pull request counts for the project of the issue it names
// (issues.project_key). One that names no issue, or an issue no row holds, counts for the project
// its repository is mapped to in repo_projects. A deploy run counts for every project the pull
// requests it ships count for, once in each however many of a project's it ships; a run that ships
// none (one that did not reach production, or a deploy that shipped nothing) counts for the
// project of its own repository. A CI run counts for the project of its repository, and so does
// each of its jobs. A repository is mapped by its name compared case-insensitively, or by the name
// its mapping says it was renamed to (refs.CanonicalRepoSQL's rule). A row whose project resolves
// to nothing (it names no issue, and repo_projects maps no repository of that name) counts for no
// project and is in no project's day rows; the delivery routes, which follow no project, still
// count it.
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
// row (k16): their n, sum, num, den, counts and via_repository add up to its own, and their samples
// are its samples. A family a project holds no row of that day writes no row: the explicit zero
// rows of a series from its first input day on are the refresher's (ZeroDeliveryDay,
// ProjectDay.WithZeros).

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
// deletes the rows of that a day no longer holds: neither change-fail-rate nor
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

// Member is one item a project's day counted, as eval_day_members records it beside the day: its
// family (KindDelivery or KindCI), its row key (PullRequestMember, RunMember) and its repository.
type Member struct {
	Family string
	RowKey string
	Repo   string
}

// ProjectDay is one project's delivery and CI rows of one UTC day, as WriteProjectDay writes them:
// the day rows (their built_by and updated_at are the write's) and the items they counted.
type ProjectDay struct {
	Project string
	// Day is the day's UTC midnight.
	Day     time.Time
	Rows    []DayRow
	Members []Member
}

// WithZeros is p with each of zeros (the explicit zero rows of ZeroDeliveryDay) that p holds no row
// of the same eval, series and repository of: the refresher's zero row of a series on a day the
// series has no input, which never stands in for a row the day built.
func (p ProjectDay) WithZeros(zeros []DayRow) ProjectDay {
	held := map[[3]string]bool{}
	for _, row := range p.Rows {
		held[[3]string{row.EvalID, row.Series, row.Repo}] = true
	}
	p.Rows = slices.Clone(p.Rows)
	for _, zero := range zeros {
		if !held[[3]string{zero.EvalID, zero.Series, zero.Repo}] {
			p.Rows = append(p.Rows, zero)
		}
	}
	return p
}

// counted is one project a row counts for, and whether it reached it through repo_projects
// rather than through an issue.
type counted struct {
	project string
	via     bool
}

// dayPR is one pull request merged on the day, as the builders read it, with its repository, its
// row key and the project it counts for ("" for none).
type dayPR struct {
	pr     measures.PR
	repo   string
	member string
	counted
}

// dayDeploy is one deploy run on main started on the day, as the builders read it, with its
// repository, its row key and the projects it counts for.
type dayDeploy struct {
	run      measures.Run
	repo     string
	member   string
	projects []counted
}

// dayCIRun is one CI run started on the day, with its row key and the project of its repository
// ("" for none); every CI run reaches its project through its repository.
type dayCIRun struct {
	run     delivery.DeliveryRun
	member  string
	project string
}

// DeliveryDay is one UTC day's delivery and CI rows, read by ReadDeliveryDay, each attributed to
// the projects it counts for.
type DeliveryDay struct {
	// Day is the day's UTC midnight.
	Day     time.Time
	cards   []measures.DeployJobCard
	prs     []dayPR
	deploys []dayDeploy
	// checks are the PR-checks runs and checkJobs every job of each; pushes the deploy push runs
	// and pushJobs the jobs of each the cards follow.
	checks, pushes      []dayCIRun
	checkJobs, pushJobs map[int64][]delivery.DeliveryRunJob
}

// utcMidnight is the UTC midnight of the UTC day t is on.
func utcMidnight(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// ReadDeliveryDay reads the delivery and CI rows of the UTC day day is on, under settings, and
// attributes each to its projects. Its reads: the day's deploy runs on main with the jobs a deploy
// is judged by (delivery.ListDeployRunsStartedIn); the pull requests merged from since to the day's
// end with every deploy run created from since on (delivery.ReadMergedRows), where since is the
// day's start, or earlier where the day's deploys can ship a pull request merged before it: the
// latest creation among the deploys that succeeded before the day's first deploy did
// (delivery.LatestShipBefore, the beginning of time when none did), or the earliest creation of
// the day's deploys if that is earlier still; the day's CI runs (delivery.ReadCIRows); the
// projects of the issues those pull requests name; and repo_projects. The pull requests merged
// before the day are read only for the shipped sets of the day's deploys.
func (s *Store) ReadDeliveryDay(ctx context.Context, settings delivery.DeliverySettings, day time.Time) (DeliveryDay, error) {
	pool := s.database.Pool
	day = utcMidnight(day)
	end := day.AddDate(0, 0, 1)
	production := settings.Production()
	read := DeliveryDay{Day: day, cards: measures.DeployCards(settings)}

	started, err := delivery.ListDeployRunsStartedIn(ctx, pool, settings, day, end)
	if err != nil {
		return DeliveryDay{}, fmt.Errorf("read the deploy runs of %s: %w", day.Format(time.DateOnly), err)
	}
	dayApplies := delivery.ProductionApplies(started.Runs, started.Jobs, production).Applies
	since := day
	if len(dayApplies) > 0 {
		bound, found, err := delivery.LatestShipBefore(ctx, pool, settings.DeployRepo, production, dayApplies[0].CompletedAt)
		if err != nil {
			return DeliveryDay{}, fmt.Errorf("read the earliest merge the deploys of %s can ship: %w", day.Format(time.DateOnly), err)
		}
		since = time.Time{}
		if found {
			since = minTime(day, bound)
			for _, apply := range dayApplies {
				since = minTime(since, apply.CreatedAt)
			}
		}
	}
	merged, err := delivery.ReadMergedRows(ctx, pool, settings, since, end, false)
	if err != nil {
		return DeliveryDay{}, fmt.Errorf("read the pull requests of %s: %w", day.Format(time.DateOnly), err)
	}
	ci, err := delivery.ReadCIRows(ctx, pool, settings.DeployRepo, day, end, measures.DeployCardJobNames(read.cards))
	if err != nil {
		return DeliveryDay{}, fmt.Errorf("read the CI runs of %s: %w", day.Format(time.DateOnly), err)
	}
	var issueKeys []string
	for _, pr := range merged.PullRequests {
		if pr.IssueKey != nil {
			issueKeys = append(issueKeys, *pr.IssueKey)
		}
	}
	issueProjects, err := readIssueProjects(ctx, s, issueKeys)
	if err != nil {
		return DeliveryDay{}, err
	}
	repos, err := readRepoProjects(ctx, s)
	if err != nil {
		return DeliveryDay{}, err
	}

	attribute := func(pr delivery.DeliveryPullRequest) counted {
		if pr.IssueKey != nil {
			if project, found := issueProjects[*pr.IssueKey]; found {
				return counted{project: project}
			}
		}
		return counted{project: repos.of(pr.Repo), via: true}
	}
	reached := map[int64]bool{}
	for _, apply := range dayApplies {
		reached[apply.RunID] = true
	}
	applies := delivery.ProductionApplies(merged.ApplyRuns, merged.ApplyJobs, production).Applies
	shippedCount := map[int64]int{}
	shippedTo := map[int64][]counted{}
	for _, pr := range merged.PullRequests {
		apply := delivery.ContainingRun(pr, settings.DeployRepo, applies)
		pullRequestCounts := attribute(pr)
		if apply != nil && reached[apply.RunID] {
			shippedCount[apply.RunID]++
			if pullRequestCounts.project != "" {
				shippedTo[apply.RunID] = append(shippedTo[apply.RunID], pullRequestCounts)
			}
		}
		// A pull request merged before the day is read for the shipped sets alone; one whose
		// opening is not known yet (a partial row) has no lead time to count, as the delivery
		// routes leave it out of theirs.
		if pr.MergedAt.Before(day) || pr.CreatedAt == nil {
			continue
		}
		measured := measures.PR{
			ID: pr.Repo + "#" + strconv.Itoa(pr.Number), CreatedAt: *pr.CreatedAt, MergedAt: *pr.MergedAt,
			FirstCommitAt: pr.FirstCommitAt, Rework: pr.Rework,
		}
		if apply != nil {
			runID, completedAt := apply.RunID, apply.CompletedAt
			measured.DeployRunID, measured.DeployedAt = &runID, &completedAt
		}
		read.prs = append(read.prs, dayPR{pr: measured, repo: pr.Repo, member: PullRequestMember(pr.Repo, pr.Number), counted: pullRequestCounts})
	}
	for _, run := range started.Runs {
		deploy := dayDeploy{
			run: measures.Run{
				ID: run.RunID, StartedAt: run.StartedAt, Conclusion: run.Conclusion,
				ReachedProduction: reached[run.RunID], ShippedPRs: shippedCount[run.RunID],
			},
			repo: run.Repo, member: RunMember(run.Repo, run.RunID),
		}
		// Once in each project its pull requests count for, through an issue where any of that
		// project's did.
		for _, shipped := range shippedTo[run.RunID] {
			index := slices.IndexFunc(deploy.projects, func(c counted) bool { return c.project == shipped.project })
			if index < 0 {
				deploy.projects = append(deploy.projects, shipped)
				continue
			}
			deploy.projects[index].via = deploy.projects[index].via && shipped.via
		}
		if len(deploy.projects) == 0 {
			if project := repos.of(run.Repo); project != "" {
				deploy.projects = []counted{{project: project, via: true}}
			}
		}
		read.deploys = append(read.deploys, deploy)
	}
	ciRuns := func(runs []delivery.DeliveryRun) []dayCIRun {
		attributed := make([]dayCIRun, len(runs))
		for i, run := range runs {
			attributed[i] = dayCIRun{run: run, member: RunMember(run.Repo, run.RunID), project: repos.of(run.Repo)}
		}
		return attributed
	}
	read.checks, read.checkJobs = ciRuns(ci.Checks.Runs), ci.Checks.Jobs
	read.pushes, read.pushJobs = ciRuns(ci.Deploys.Runs), ci.Deploys.Jobs
	return read, nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
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

// Projects is every project a row of the day counts for, by key.
func (d DeliveryDay) Projects() []string {
	projects := map[string]bool{}
	for _, pr := range d.prs {
		projects[pr.project] = true
	}
	for _, deploy := range d.deploys {
		for _, c := range deploy.projects {
			projects[c.project] = true
		}
	}
	for _, run := range slices.Concat(d.checks, d.pushes) {
		projects[run.project] = true
	}
	delete(projects, "")
	return slices.Sorted(maps.Keys(projects))
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
// they count; no row and no item for a project no row of the day counts for.
func (d DeliveryDay) Rows(project string) ProjectDay {
	result := ProjectDay{Project: project, Day: d.Day}
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
		members[Member{Family: KindDelivery, RowKey: pr.member, Repo: pr.repo}] = true
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
		members[Member{Family: KindDelivery, RowKey: deploy.member, Repo: deploy.repo}] = true
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
			members[Member{Family: KindCI, RowKey: run.member, Repo: run.run.Repo}] = true
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
// (ProjectDay.WithZeros). pr-ci's job series have none: a job with no result that day has no part.
func ZeroDeliveryDay(settings delivery.DeliverySettings, project string, day time.Time) ProjectDay {
	d := DeliveryDay{Day: utcMidnight(day), cards: measures.DeployCards(settings)}
	return ProjectDay{Project: project, Day: d.Day, Rows: d.buildFamilies(project, "", familyRows{}, true)}
}

// build is the rows of project's cut repo ("" for all repositories) from rows, the families rows
// holds alone.
func (d DeliveryDay) build(project, repo string, rows familyRows) []DayRow {
	return d.buildFamilies(project, repo, rows, false)
}

// buildFamilies is build, and with every set, every family's rows whether rows holds that family
// or not (ZeroDeliveryDay's). Each part's via_repository is the n the same builder counts over the
// family's rows that reached the project through their repository alone.
func (d DeliveryDay) buildFamilies(project, repo string, rows familyRows, every bool) []DayRow {
	var built []DayRow
	add := func(slug string, parts, viaParts []measures.DayPart) {
		via := make(map[string]int, len(viaParts))
		for _, part := range viaParts {
			via[part.Series] = part.N
		}
		for _, part := range parts {
			part.ViaRepository = via[part.Series]
			row := DayRow{
				ProjectKey: project, EvalID: EvalID(project, slug), Repo: repo,
				Listed: !strings.HasPrefix(part.Series, measures.JobSeriesPrefix),
			}
			built = append(built, row.WithPart(part))
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
			add(builder.slug, builder.build(d.Day, prs), builder.build(d.Day, viaPRs))
		}
	}
	if every || len(rows.deploys) > 0 {
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
	if every || len(rows.checks) > 0 {
		parts := measures.PRChecksDay(d.Day, rows.checks, d.checkJobs)
		add(SlugPRCI, parts, parts)
	}
	if every || len(rows.pushes) > 0 {
		parts := measures.DeployJobsDay(d.Day, rows.pushes, d.pushJobs, d.cards)
		for _, card := range d.cards {
			add(cardSlugs[card.CardID], parts[card.CardID], parts[card.CardID])
		}
	}
	return built
}

// WriteDeliveryDay reads the delivery and CI rows of the UTC day day is on (ReadDeliveryDay) and
// writes the rows of project, or of every project they count for where project is "", each
// project in a transaction of its own (WriteProjectDay), and answers the projects written. A
// project named that no row of the day counts for has its delivery and CI rows of the day
// deleted. builtBy is the running build, which every row written records.
func (s *Store) WriteDeliveryDay(ctx context.Context, settings delivery.DeliverySettings, day time.Time, project, builtBy string) ([]string, error) {
	read, err := s.ReadDeliveryDay(ctx, settings, day)
	if err != nil {
		return nil, err
	}
	projects := read.Projects()
	if project != "" {
		projects = []string{project}
	}
	for _, key := range projects {
		if err := s.WriteProjectDay(ctx, read.Rows(key), builtBy); err != nil {
			return nil, err
		}
	}
	return projects, nil
}

// WriteProjectDay writes day as its project's delivery and CI rows of its day, in one transaction:
// each row with `insert … on conflict … do update … where (…) is distinct from (…)`, so a row
// whose every value is what it holds is left as it is (and logs no change, migration
// 20261113082202), recording builtBy, the running build, as the build that wrote it; then it
// deletes the project's rows of that day of the delivery and CI evals (deliveryDaySlugs) that day
// does not hold, such as a repository whose last row moved to another day or project. It records
// day's members in eval_day_members on its day, and deletes the members of the project's delivery
// and CI families recorded on that day that day does not hold, so each item's row there says
// where it was last counted. A row of another project, another day or another eval, and an item
// of another family, is refused before anything is written.
func (s *Store) WriteProjectDay(ctx context.Context, day ProjectDay, builtBy string) error {
	return s.writeProjectDay(ctx, "delivery", day, deliveryDaySlugs, deliveryDayFamilies, builtBy)
}

// writeProjectDay is the write of one project's computed rows of one day: day's rows of the evals
// slugs name, and its members of families, written as WriteProjectDay says. The stale rows it
// deletes are those of slugs alone and the stale members those of families alone, so the
// delivery and the Legion writes of one day leave each other's rows and members as they are.
// what names the rows in a refusal.
func (s *Store) writeProjectDay(ctx context.Context, what string, day ProjectDay, slugs, families []string, builtBy string) error {
	date := utcMidnight(day.Day)
	evalIDs := make([]string, len(slugs))
	for i, slug := range slugs {
		evalIDs[i] = EvalID(day.Project, slug)
	}
	for _, row := range day.Rows {
		if row.ProjectKey != day.Project || !row.Day.Equal(date) || !slices.Contains(evalIDs, row.EvalID) {
			return fmt.Errorf("evals: a %s day row of %s on %s is %s of %s on %s", what, day.Project, date.Format(time.DateOnly),
				row.EvalID, row.ProjectKey, row.Day.Format(time.DateOnly))
		}
	}
	for _, member := range day.Members {
		if !slices.Contains(families, member.Family) {
			return fmt.Errorf("evals: a %s day item of %s on %s is %s of family %s", what, day.Project, date.Format(time.DateOnly),
				member.RowKey, member.Family)
		}
	}
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the day rows of %s on %s: %w", day.Project, date.Format(time.DateOnly), err)
	}
	defer tx.Rollback(ctx)
	if err := upsertDays(ctx, tx, day.Rows, builtBy); err != nil {
		return err
	}
	evals, series, repos := make([]string, len(day.Rows)), make([]string, len(day.Rows)), make([]string, len(day.Rows))
	for i, row := range day.Rows {
		evals[i], series[i], repos[i] = row.EvalID, row.Series, row.Repo
	}
	if _, err := tx.Exec(ctx, `
		delete from eval_days
		where project_key = $1 and day = $2 and eval_id = any($3)
		  and (eval_id, series, repo) not in (select * from unnest($4::text[], $5::text[], $6::text[]))
	`, day.Project, date, evalIDs, evals, series, repos); err != nil {
		return fmt.Errorf("delete the day rows %s no longer holds on %s: %w", day.Project, date.Format(time.DateOnly), err)
	}
	memberFamilies, keys, memberRepos := make([]string, len(day.Members)), make([]string, len(day.Members)), make([]string, len(day.Members))
	for i, member := range day.Members {
		memberFamilies[i], keys[i], memberRepos[i] = member.Family, member.RowKey, member.Repo
	}
	if _, err := tx.Exec(ctx, `
		insert into eval_day_members (project_key, family, row_key, day, repo)
		select $1, member.family, member.row_key, $2, member.repo
		from unnest($3::text[], $4::text[], $5::text[]) as member(family, row_key, repo)
		order by member.family, member.row_key
		on conflict (project_key, family, row_key) do update set day = excluded.day, repo = excluded.repo
		where (eval_day_members.day, eval_day_members.repo) is distinct from (excluded.day, excluded.repo)
	`, day.Project, date, memberFamilies, keys, memberRepos); err != nil {
		return fmt.Errorf("record the items %s counted on %s: %w", day.Project, date.Format(time.DateOnly), err)
	}
	if _, err := tx.Exec(ctx, `
		delete from eval_day_members
		where project_key = $1 and family = any($2) and day = $3
		  and (family, row_key) not in (select * from unnest($4::text[], $5::text[]))
	`, day.Project, families, date, memberFamilies, keys); err != nil {
		return fmt.Errorf("delete the items %s no longer counts on %s: %w", day.Project, date.Format(time.DateOnly), err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the day rows of %s on %s: %w", day.Project, date.Format(time.DateOnly), err)
	}
	return nil
}

// WriteUnownedP0Day writes project's unowned-p0 row of the UTC day day is on: the count of its
// open P0 issues with no owner now (delivery.ListUnownedP0, by the issues_unowned_p0 index) as the
// row's one value and its last_value, last_at the write's own time, so the day's row is the count
// at its last refresh and the `last` fold of a window reads its newest day's. builtBy is the
// running build.
func (s *Store) WriteUnownedP0Day(ctx context.Context, project string, day time.Time, builtBy string) error {
	issues, err := delivery.ListUnownedP0(ctx, s.database.Pool, project)
	if err != nil {
		return fmt.Errorf("count the unowned P0 issues of %s: %w", project, err)
	}
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the unowned-p0 row of %s: %w", project, err)
	}
	defer tx.Rollback(ctx)
	var now time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&now); err != nil {
		return fmt.Errorf("read the time of the unowned-p0 row of %s: %w", project, err)
	}
	count, at := float64(len(issues)), now.UTC()
	row := DayRow{
		ProjectKey: project, EvalID: EvalID(project, SlugUnownedP0), Series: SeriesUnownedP0, Day: utcMidnight(day), Listed: true,
		N: 1, Sum: &count, Max: &count, LastValue: &count, LastAt: &at,
	}
	if err := upsertDays(ctx, tx, []DayRow{row}, builtBy); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the unowned-p0 row of %s: %w", project, err)
	}
	return nil
}

// upsertDays writes rows in one statement, in key order, each with built_by builtBy: a new row is
// inserted, and a stored one rewritten only where a value differs (built_by included, so a new
// build's rebuild marks every row it rebuilt).
func upsertDays(ctx context.Context, tx pgx.Tx, rows []DayRow, builtBy string) error {
	if len(rows) == 0 {
		return nil
	}
	rows = slices.SortedFunc(slices.Values(rows), func(a, b DayRow) int {
		return cmp.Or(cmp.Compare(a.ProjectKey, b.ProjectKey), cmp.Compare(a.EvalID, b.EvalID), cmp.Compare(a.Series, b.Series),
			cmp.Compare(a.Repo, b.Repo), a.Day.Compare(b.Day))
	})
	count := len(rows)
	projects, evals, series, repos := make([]string, count), make([]string, count), make([]string, count), make([]string, count)
	days, lastAts := make([]time.Time, count), make([]*time.Time, count)
	listed := make([]bool, count)
	ns, via := make([]int32, count), make([]int32, count)
	sums, nums, dens, maxes, lastValues := make([]*float64, count), make([]*float64, count), make([]*float64, count), make([]*float64, count), make([]*float64, count)
	samples, counts := make([]*string, count), make([]string, count)
	for i, row := range rows {
		projects[i], evals[i], series[i], repos[i], days[i], listed[i] = row.ProjectKey, row.EvalID, row.Series, row.Repo, utcMidnight(row.Day), row.Listed
		ns[i], via[i] = int32(row.N), int32(row.ViaRepository)
		sums[i], nums[i], dens[i], maxes[i], lastValues[i], lastAts[i] = row.Sum, row.Num, row.Den, row.Max, row.LastValue, row.LastAt
		samples[i] = floatArray(row.Samples)
		encoded, err := json.Marshal(nonNilCounts(row.Counts))
		if err != nil {
			return fmt.Errorf("encode the counts of a day row of %s: %w", row.EvalID, err)
		}
		counts[i] = string(encoded)
	}
	if _, err := tx.Exec(ctx, `
		insert into eval_days (project_key, eval_id, series, repo, day, listed, n, sum, num, den, max, samples,
			last_value, last_at, counts, via_repository, built_by)
		select project_key, eval_id, series, repo, day, listed, n, sum, num, den, max, samples::double precision[],
			last_value, last_at, counts::jsonb, via_repository, $17
		from unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::date[], $6::bool[], $7::int[],
			$8::double precision[], $9::double precision[], $10::double precision[], $11::double precision[], $12::text[],
			$13::double precision[], $14::timestamptz[], $15::text[], $16::int[])
			as given(project_key, eval_id, series, repo, day, listed, n, sum, num, den, max, samples,
				last_value, last_at, counts, via_repository)
		on conflict (project_key, eval_id, series, repo, day) do update
		set listed = excluded.listed, n = excluded.n, sum = excluded.sum, num = excluded.num, den = excluded.den,
			max = excluded.max, samples = excluded.samples, last_value = excluded.last_value, last_at = excluded.last_at,
			counts = excluded.counts, via_repository = excluded.via_repository, built_by = excluded.built_by,
			updated_at = now()
		where (eval_days.listed, eval_days.n, eval_days.sum, eval_days.num, eval_days.den, eval_days.max,
				eval_days.samples, eval_days.last_value, eval_days.last_at, eval_days.counts,
				eval_days.via_repository, eval_days.built_by)
			is distinct from (excluded.listed, excluded.n, excluded.sum, excluded.num, excluded.den, excluded.max,
				excluded.samples, excluded.last_value, excluded.last_at, excluded.counts,
				excluded.via_repository, excluded.built_by)
	`, projects, evals, series, repos, days, listed, ns, sums, nums, dens, maxes, samples, lastValues, lastAts, counts, via, builtBy); err != nil {
		return fmt.Errorf("write the day rows of %s: %w", rows[0].ProjectKey, err)
	}
	return nil
}

// floatArray is values as a Postgres array literal, each the shortest decimal that reads back as
// the same float, so a stored sample is the builder's to the last bit; nil for none.
func floatArray(values []float64) *string {
	if values == nil {
		return nil
	}
	var text strings.Builder
	text.WriteByte('{')
	for i, value := range values {
		if i > 0 {
			text.WriteByte(',')
		}
		text.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	}
	text.WriteByte('}')
	encoded := text.String()
	return &encoded
}

func nonNilCounts(counts map[string]float64) map[string]float64 {
	if counts == nil {
		return map[string]float64{}
	}
	return counts
}
