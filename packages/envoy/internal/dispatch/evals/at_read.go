package evals

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
)

// The read-time path of the delivery and CI evals (dispatch://INFRA-2918, D7a; R21): an eval's
// rows of each day of a read's window and compare range, built when the list is read from the
// stored single records of that day (its pull requests, its deploy runs, its posted deploys, its
// CI runs and their jobs) by the day builders the refresher writes the eval's stored rows with
// (deliveryReadOf, DeliveryDay.Rows), for each key a project's read folds the rows of (its own and
// the owner keys of its published default repository, eval_project_owners), under the rules the
// refresher keeps a series' first input day and its zero rows by (dropUnshipped, zerosFrom,
// ProjectDay.WithZeros), so the fold of them the stored rows are folded with (SeriesValuesOf)
// answers what the stored rows answer. The three failure measures' rows are built from the record
// of deploys and failures (production_events) by the builder the refresher writes them with
// (failureRecord.day, failure_days.go). A read takes it where it is narrowed by what no stored row
// is kept by: the delivery timeline's facets and its search, which select pull requests
// (Narrowing), so such a read builds the evals that count pull requests this way
// (CountsPullRequests) and states every other eval from its stored rows.
//
// unowned-p0 has no read-time path: its row is the count of open P0 issues with no owner at the
// moment the refresher wrote it (WriteUnownedP0Day), which no stored record of a past day holds.
// The code evals have none either: their rows are the counts of a replay of GitHub's patches
// (WriteCodeDaysTx), which a read never runs. The Legion and the logged evals have none either:
// no filter of the Evals tab narrows them.

// atReadFamily is what a delivery day reads for an eval's rows: the records its builder builds
// them from.
type atReadFamily int

const (
	// atReadPullRequests is the day's pull requests, each shipped by its first shipping apply
	// among its repository's apply runs, or by the first posted deploy that names it.
	atReadPullRequests atReadFamily = iota + 1
	// atReadDeploys is the day's deploy runs on main and posted deploys, each with the pull
	// requests it ships first.
	atReadDeploys
	// atReadChecks is the day's PR-checks runs, each with every job.
	atReadChecks
	// atReadPushes is the day's deploy push runs on main, with the jobs the deploy cards follow,
	// each counted for the projects of the deploy it is (atReadDeploys).
	atReadPushes
	// atReadFailures is the record of deploys and failures (production_events) of the project, from
	// its first day on (readFailureRecord), whose rows the refresher's builder builds
	// (failureRecord.day: measures.ChangeFailRateDay, RecoveryDay and ReworkRateDay).
	atReadFailures
)

// atReadFamilies is the read-time path of each delivery and CI eval, by slug: every one of the
// catalogue but unowned-p0 and the code evals.
var atReadFamilies = map[string]atReadFamily{
	SlugMergeToProduction:       atReadPullRequests,
	SlugFirstCommitToProduction: atReadPullRequests,
	SlugOpenToProduction:        atReadPullRequests,
	SlugPROpenToMerge:           atReadPullRequests,
	SlugReworkShare:             atReadPullRequests,
	SlugDeployFrequency:         atReadDeploys,
	SlugDeployRunSuccess:        atReadDeploys,
	SlugPRCI:                    atReadChecks,
	SlugProductionDeploy:        atReadPushes,
	SlugStagingE2E:              atReadPushes,
	SlugStagingDeploy:           atReadPushes,
	SlugChangeFailRate:          atReadFailures,
	SlugFailedDeployRecovery:    atReadFailures,
	SlugDeploymentReworkRate:    atReadFailures,
}

// ComputedAtRead reports whether definition's eval has a read-time path (ComputeAtRead): a
// computed delivery or CI eval of the catalogue but unowned-p0.
func ComputedAtRead(definition Definition) bool {
	_, held := atReadFamilies[definition.Slug]
	return definition.Computed() && held
}

// CountsPullRequests reports whether definition's eval is a computed eval that counts pull
// requests: merge-to-production, first-commit-to-production, open-to-production, pr-open-to-merge
// and rework-share, the numbers the delivery timeline's facets and search narrow (api's
// measuresPRs), and the evals a Narrowing narrows.
func CountsPullRequests(definition Definition) bool {
	return definition.Computed() && atReadFamilies[definition.Slug] == atReadPullRequests
}

// AtRead is one read-time computation of evals' rows (ComputeAtRead).
type AtRead struct {
	// Entries is the evals built, each with a read-time path (ComputedAtRead); with Narrow set,
	// evals that count pull requests alone (CountsPullRequests).
	Entries []Entry
	// DeployRepos is the repositories the stored rows stand recorded under, as the caller read
	// them in the same snapshot (DeployReposIn): the rows of the deploy repositories each eval's
	// stored read reads (DeployRepoOf) are the rows kept.
	DeployRepos *DeployRepos
	// Repos is the repositories whose rows are kept, as DayWindowIn keeps them: none for each
	// project's own rows.
	Repos []string
	// Window and Compare are the ranges of whole UTC days built, Compare nil for none.
	Window  Range
	Compare *Range
	// Narrow, where set, narrows the pull requests the rows count (Narrowing); nil counts every
	// one.
	Narrow Narrowing
	// Now is the read's moment: a failure measure's rows stand through its UTC day and no later,
	// as the refresher writes them through its own (refresh.writeFailures).
	Now time.Time
}

// NarrowInput is what a Narrowing selects from: every pull request merged on a day a read-time
// computation builds, in ListPullRequestsInWindow's order, and each deploy source repository the
// rows are built for (Sources), whose apply runs ship a pull request of that repository as the
// rows ship it (delivery.ContainingRun). A pull request of no such repository is shipped by none.
type NarrowInput struct {
	PullRequests []delivery.DeliveryPullRequest
	Sources      []NarrowSource
}

// NarrowSource is one deploy source repository of a NarrowInput: the rule the rows of it are built
// under (Settings, its DeployRepo the repository as its rows are keyed), and its apply runs from
// the first day built on, with the jobs each is judged by.
type NarrowSource struct {
	Settings  delivery.DeliverySettings
	ApplyRuns []delivery.DeliveryRun
	ApplyJobs map[int64][]delivery.DeliveryRunJob
}

// Narrowing answers, reading what it selects by in q (the computation's snapshot), which of
// input's pull requests the rows count: keep[i] for input.PullRequests[i].
type Narrowing func(ctx context.Context, q delivery.Querier, input NarrowInput) (keep []bool, err error)

// ComputeAtRead builds, reading in q (one read's snapshot, BeginSnapshot), the rows of read's
// evals on each day of its window and its compare range, as DayWindowIn answers the stored rows
// of those days: for each day, the day's pull requests (one read of each span of consecutive days,
// split by the day each merged), and, where an eval needs them, each repository's deploys and CI
// runs of the day, read as the refresher's read of a page's days reads them (readDeliveryDays),
// with each repository the rows stand built for, under the rule recorded with its rows
// (eval_days_rules, keptOf); each repository's apply runs read once from the first day on
// (delivery.ApplyRows.Since), as the refresher's walk reads them, and each events repository's
// posted deploys once from the first day on (postedDeploys), which hold every one a day's rows
// read; attributed to their projects by one read of what attributes every day's rows
// (readAttribution, through the store's attributor); built into the rows of the day of each key
// the read's projects fold the rows of (ownersIn) by the refresher's day builders (deliveryReadOf,
// deliveryRead.Rows), each row's via-repository count as the project reads the key (viaAsRead);
// less each lead time to production that counts no pull request before its first input day
// (dropUnshipped); and with the zero row of each series the day holds no input of from its first
// input day on, on each
// day the stored rows are whole through (eval_days_serving), as the refresher writes them
// (zerosFrom), each key's first input days read from its stored rows as the refresher reads them
// (firstDaysIn). Each row kept is of an eval read names, under the project's eval id of its slug
// (asReader), of a deploy repository its stored read reads (DeployRepoOf), listed, and of one of
// read.Repos (each project's own row where it names none), answered once for each range whose days
// hold it. The failure measures' rows are built apart (failureRowsAtRead), whatever rules the
// refresher records.
//
// With read.Narrow set, the pull requests are narrowed to those it keeps before anything is built,
// and only the evals that count pull requests are built: a deploy counts what it ships whatever a
// filter keeps. A deployment whose refresher records no rule of a deploy source (no delivery-on
// project with a source) has no delivery or CI row, as the refresher writes none, and answers
// none.
func (s *Store) ComputeAtRead(ctx context.Context, q delivery.Querier, read AtRead) ([]WindowRow, error) {
	if len(read.Entries) == 0 {
		return nil, nil
	}
	families := map[atReadFamily]bool{}
	keyed := false
	var delivered, failures []Entry
	for _, entry := range read.Entries {
		family, held := atReadFamilies[entry.Definition.Slug]
		switch {
		case !entry.Definition.Computed() || !held:
			return nil, fmt.Errorf("evals: %s has no read-time path", entry.Definition.EvalID)
		case read.Narrow != nil && family != atReadPullRequests:
			return nil, fmt.Errorf("evals: %s counts no pull request, and a narrowed read builds only the evals that do", entry.Definition.EvalID)
		case family == atReadFailures:
			failures = append(failures, entry)
			continue
		}
		delivered = append(delivered, entry)
		families[family] = true
		keyed = keyed || KeyedByDeployRepo(entry.Definition)
	}
	ranges := []Range{read.Window}
	if read.Compare != nil {
		ranges = append(ranges, *read.Compare)
	}
	days := atReadDays(ranges)
	if len(days) == 0 {
		return nil, nil
	}
	repos := read.Repos
	if len(repos) == 0 {
		repos = []string{""}
	}
	rows, err := failureRowsAtRead(ctx, q, failures, repos, ranges, days, read.Now)
	if err != nil || len(delivered) == 0 {
		return rows, err
	}
	recorded, err := builtUnderIn(ctx, q)
	if err != nil || len(recorded) == 0 {
		return rows, err
	}
	prs, err := atReadPullRequestsOf(ctx, q, days)
	if err != nil {
		return nil, err
	}

	// The evals keyed by the deploy repository read each repository the rows stand built for;
	// the others read the day's pull requests alone.
	all := keptOf(recorded)
	var kept []keptRepo
	if keyed {
		kept = all
	}
	needDeploys := families[atReadDeploys] || families[atReadPushes]
	needCI := families[atReadChecks] || families[atReadPushes]
	// The apply runs and the posted deploys ship the pull requests of the evals that read the
	// deploy repository and say which a deploy ships; a narrowing reads each source's apply runs
	// too, since a pull request's deploy is among what it selects by (the timeline's deployed
	// facet).
	shipping := len(kept) > 0 && (families[atReadPullRequests] || needDeploys)
	applies := map[string]delivery.ApplyRows{}
	posted := map[string]postedRecord{}
	var sources []NarrowSource
	for _, repo := range all {
		if IsEventsRepo(repo.name) {
			if shipping {
				if posted[repo.name], err = postedDeploys(ctx, q, repo.rule.Project, days[0]); err != nil {
					return nil, err
				}
			}
			continue
		}
		if read.Narrow == nil && !shipping {
			continue
		}
		applied, err := delivery.ReadApplyRows(ctx, q, repo.rule, days[0])
		if err != nil {
			return nil, fmt.Errorf("read the deploy runs of %s from %s on: %w", repo.name, days[0].Format(time.DateOnly), err)
		}
		applies[repo.name] = applied
		sources = append(sources, NarrowSource{Settings: repo.rule, ApplyRuns: applied.Runs, ApplyJobs: applied.Jobs})
	}
	if read.Narrow != nil {
		if prs, err = narrowedPullRequests(ctx, q, read.Narrow, sources, days, prs); err != nil {
			return nil, err
		}
	}

	reads := make([][]repoReads, len(days))
	var pulls []delivery.DeliveryPullRequest
	var checks []delivery.DeliveryRun
	for i, day := range days {
		pulls = append(pulls, prs[i]...)
		if len(kept) == 0 {
			continue
		}
		end := day.AddDate(0, 0, 1)
		reads[i] = make([]repoReads, len(kept))
		for k, repo := range kept {
			one := &reads[i][k]
			one.settings, one.since = repo.rule, repo.since
			one.settings.DeployRepo = repo.name
			if IsEventsRepo(repo.name) {
				one.posted = posted[repo.name]
				continue
			}
			if held, found := applies[repo.name]; found {
				if one.applies, err = held.Since(day); err != nil {
					return nil, err
				}
			}
			if needDeploys {
				if one.deploys, err = delivery.ReadDeploys(ctx, q, one.settings, day, end); err != nil {
					return nil, fmt.Errorf("read the deploys of %s on %s: %w", repo.name, day.Format(time.DateOnly), err)
				}
				pulls = append(pulls, one.deploys.Earlier.PullRequests...)
			}
			if needCI {
				cardNames := measures.DeployCardJobNames(measures.DeployCards(one.settings))
				if one.ci, err = delivery.ReadCIRows(ctx, q, repo.name, day, end, cardNames); err != nil {
					return nil, fmt.Errorf("read the CI runs of %s on %s: %w", repo.name, day.Format(time.DateOnly), err)
				}
				checks = append(checks, one.ci.Checks.Runs...)
			}
		}
	}
	attribute, err := readAttribution(ctx, q, s.attributor, pulls, checks)
	if err != nil {
		return nil, err
	}
	next, err := deliveryServedIn(ctx, q)
	if err != nil {
		return nil, err
	}

	definitions := make(map[string]Definition, len(delivered))
	var projects []string
	for _, entry := range delivered {
		definitions[entry.Definition.EvalID] = entry.Definition
		if !slices.Contains(projects, entry.Definition.ProjectKey) {
			projects = append(projects, entry.Definition.ProjectKey)
		}
	}
	owners, err := ownersIn(ctx, q, projects)
	if err != nil {
		return nil, err
	}
	rules := keptRules(kept)
	zeros := map[string][]DayRow{}
	for _, project := range projects {
		for _, owner := range owners[project] {
			if _, held := zeros[owner.key]; !held {
				zeros[owner.key] = ZeroDeliveryDay(owner.key, days[0], rules).Rows
			}
		}
	}
	firsts, err := firstDaysIn(ctx, q, zeros, nil)
	if err != nil {
		return nil, err
	}
	deployRepoOf := make(map[string][]string, len(definitions))
	for id, definition := range definitions {
		deployRepoOf[id] = DeployRepoOf(definition, read.DeployRepos)
	}
	for i, day := range days {
		built := deliveryReadOf(day, kept, prs[i], reads[i], attribute)
		for _, project := range projects {
			for _, owner := range owners[project] {
				first := firsts[owner.key]
				own := built.Rows(owner.key)
				own.Rows = dropUnshipped(own.Rows, first, day)
				if next != nil && day.Before(*next) {
					own = own.WithZeros(zerosFrom(ZeroDeliveryDay(owner.key, day, rules).Rows, first, day))
				}
				for _, row := range own.Rows {
					id := asReader(owner.key, project, row.EvalID)
					if deployRepos, held := deployRepoOf[id]; !held || !slices.Contains(deployRepos, row.DeployRepo) || !row.Listed || !slices.Contains(repos, row.Repo) {
						continue
					}
					part := row.DayPart
					if !owner.via {
						part.ViaRepository = 0
					}
					rows = inRanges(rows, ranges, day, WindowRow{EvalID: id, Part: part, owner: owner.key, repo: row.Repo})
				}
			}
		}
	}
	return rows, nil
}

// inRanges is rows with row, a row of day, once for each of ranges (the window, then the compare
// range) that holds day, under that range's name.
func inRanges(rows []WindowRow, ranges []Range, day time.Time, row WindowRow) []WindowRow {
	for r, of := range ranges {
		if day.Before(of.From) || !day.Before(of.To) {
			continue
		}
		row.Range = RangeWindow
		if r > 0 {
			row.Range = RangeCompare
		}
		rows = append(rows, row)
	}
	return rows
}

// failureRowsAtRead builds, reading in q, the rows of entries' failure measures on each of days
// that ranges hold, as the refresher's whole write of a project builds them
// (refresh.writeFailures): each project's failure measures as its delivery settings turn them on
// (failureProjectOf), none where they are off; its record read once, over the deploys of every
// project (readFailureRecord); and each day from its first day through the UTC day of now built
// (failureRecord.day). They are the project's own rows, which no owner key holds and no deploy
// repository keys, and which the project reads with none of them counted through a repository
// (viaAsRead: its own key's via is false). Each row kept is of an eval entries names and of one of
// repos ("" for the project's own row).
func failureRowsAtRead(ctx context.Context, q delivery.Querier, entries []Entry, repos []string, ranges []Range, days []time.Time, now time.Time) ([]WindowRow, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	named := map[string]bool{}
	var projects []string
	for _, entry := range entries {
		named[entry.Definition.EvalID] = true
		if !slices.Contains(projects, entry.Definition.ProjectKey) {
			projects = append(projects, entry.Definition.ProjectKey)
		}
	}
	settings, err := delivery.ListProjectSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	every, err := ProjectsIn(ctx, q)
	if err != nil {
		return nil, err
	}
	today := measures.Midnight(now)
	var rows []WindowRow
	for _, one := range settings {
		project, on := failureProjectOf(one)
		if !on || !slices.Contains(projects, project.project) {
			continue
		}
		record, err := readFailureRecord(ctx, q, project, every)
		if err != nil {
			return nil, err
		}
		for _, day := range days {
			if day.After(today) {
				break
			}
			for _, row := range record.day(day).Rows {
				if named[row.EvalID] && row.Listed && slices.Contains(repos, row.Repo) {
					part := row.DayPart
					part.ViaRepository = 0
					rows = inRanges(rows, ranges, day, WindowRow{EvalID: row.EvalID, Part: part, owner: project.project, repo: row.Repo})
				}
			}
		}
	}
	return rows, nil
}

// atReadDays is each UTC day of ranges, ranges of whole UTC days, once, oldest first.
func atReadDays(ranges []Range) []time.Time {
	var days []time.Time
	for _, of := range ranges {
		for day := of.From; day.Before(of.To); day = day.AddDate(0, 0, 1) {
			days = append(days, day)
		}
	}
	slices.SortFunc(days, time.Time.Compare)
	return slices.CompactFunc(days, time.Time.Equal)
}

// atReadPullRequestsOf reads in q the pull requests merged on each of days (consecutive UTC days,
// oldest first), by day, each day's in ListPullRequestsInWindow's order: one read of each span of
// consecutive days, split by the day each merged, which answers what a read of each day answers.
func atReadPullRequestsOf(ctx context.Context, q delivery.Querier, days []time.Time) ([][]delivery.DeliveryPullRequest, error) {
	byDay := make([][]delivery.DeliveryPullRequest, len(days))
	for start := 0; start < len(days); {
		end := start + 1
		for end < len(days) && days[end].Equal(days[end-1].AddDate(0, 0, 1)) {
			end++
		}
		from, to := days[start], days[end-1].AddDate(0, 0, 1)
		read, err := delivery.ListPullRequestsInWindow(ctx, q, from, to)
		if err != nil {
			return nil, fmt.Errorf("read the pull requests merged from %s to %s: %w", from.Format(time.DateOnly), to.Format(time.DateOnly), err)
		}
		at := start
		for _, pr := range read {
			merged := measures.Midnight(*pr.MergedAt)
			for !days[at].Equal(merged) {
				at++
			}
			byDay[at] = append(byDay[at], pr)
		}
		start = end
	}
	return byDay, nil
}

// narrowedPullRequests is prs, each day's pull requests, with only those narrow keeps, which it is
// given in day order with the deploy source repositories the rows ship them by (NarrowInput).
func narrowedPullRequests(ctx context.Context, q delivery.Querier, narrow Narrowing, sources []NarrowSource,
	days []time.Time, prs [][]delivery.DeliveryPullRequest) ([][]delivery.DeliveryPullRequest, error) {
	input := NarrowInput{PullRequests: slices.Concat(prs...), Sources: sources}
	keep, err := narrow(ctx, q, input)
	if err != nil {
		return nil, err
	}
	if len(keep) != len(input.PullRequests) {
		return nil, fmt.Errorf("evals: a narrowing kept %d answers of %d pull requests", len(keep), len(input.PullRequests))
	}
	narrowed := make([][]delivery.DeliveryPullRequest, len(days))
	at := 0
	for i := range days {
		for _, pr := range prs[i] {
			if keep[at] {
				narrowed[i] = append(narrowed[i], pr)
			}
			at++
		}
	}
	return narrowed, nil
}

// deliveryServedIn is, read in q, the first day the stored delivery and CI rows are not whole
// through (eval_days_serving's record of the delivery walk, servingNext's), nil where no walk has
// built a day of them: the days before it hold their series' zero rows.
func deliveryServedIn(ctx context.Context, q delivery.Querier) (*time.Time, error) {
	var next time.Time
	err := q.QueryRow(ctx, `select next_day from eval_days_serving where family = $1 and project_key = ''`, walkDelivery).Scan(&next)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read what the delivery day rows are served through: %w", err)
	}
	return new(measures.Midnight(next)), nil
}
