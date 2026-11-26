package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// The rows of the two code evals of dispatch://INFRA-3206, code survival at 30 days and post-merge
// rework at 30 days (evalComputedItems): the changes the survival eval's numbers count (`change`),
// the pairs the rework eval's numbers count (`pair`), and, in both, each reason the range reads "no
// data" other than days still waiting (`absent`).
//
// They are read from the code survival run's stored results (codesurvival: code_changes,
// code_pairs, code_days and code_repos) in the answer's snapshot, where the project's day rows
// counted them. A change counts for the project as R11 gives it (fold.CountsFor, the rule the run
// builds the day rows by), and is answered where the status row of its day, in the cut the request
// reads (the project's own rows, or each repository it names), says that day was measured: a
// marked or blocked day answers no change of its own. It is of the group the project's code groups
// give it (survival.GroupOf), and answered under that group's series survival:<g>; a change of no
// group counts in no number and answers no row. A pair is answered where its adder is, under the
// series its kind and its adder's group name (survival.KindOf): rework:<g>, iteration:<g>,
// noissue:<g> or other:<g>, so a pair where either change has no issue answers under noissue:<g>
// alone (or other:<g>, where its remover is of another group), never as a different issue's. A
// removal by a commit that merged no pull request is no pair: other:<g> counts its lines as the
// adder's removed_by_non_pr, which answers no row of its own.
//
// An absent row answers for every series of the eval. The reasons it names are each marked day of
// the range and each commit that blocks days of it, which code_days names where the status rows say
// the days are marked or blocked; a default repository the App cannot read (code_repos); no default
// repository, where the range holds no measured day; and no code group. Every address is the
// repository's on GitHub, or the project's settings page for a reason a setting gives.

// codeMeasureSource is the source of a change row and of a pair row: the function that measured
// them.
const codeMeasureSource = "delivery/survival.Measure"

// The causes of an absent row (cols.cause): a day a force-push marked (R8), a commit whose patch
// GitHub gave neither whole nor through files[] (R9), a default repository the App cannot read, no
// default repository, and no code group (R10, R11).
const (
	absentMarked         = "marked"
	absentBlocked        = "blocked"
	absentRepoUnreadable = "repo_unreadable"
	absentNoDefaultRepo  = "no_default_repo"
	absentNoGroup        = "no_group"
)

// The relations of a code row's links: via, the commit the row was measured from, and related,
// the adder's commit of a pair (dispatch://INFRA-979 R37, the IANA registry's names).
const (
	linkRelVia     = "via"
	linkRelRelated = "related"
)

// codeKindSeries is the base of the series each kind of removal is counted under (fold.BuildDays).
var codeKindSeries = map[survival.Kind]string{
	survival.KindRework:    fold.SeriesRework,
	survival.KindIteration: fold.SeriesIteration,
	survival.KindNoIssue:   fold.SeriesNoIssue,
	survival.KindOther:     fold.SeriesOther,
}

// The row kinds of the two code evals and their readers.
var (
	changeItems = evalItemKind{kind: evalRowChange, source: (*server).changeItemSource}
	pairItems   = evalItemKind{kind: evalRowPair, source: (*server).pairItemSource}
	absentItems = evalItemKind{kind: evalRowAbsent, read: (*server).absentItemRows}
)

// codeProject is a project as the code evals' rows read it, once a request (evalRequestReads):
// its key and its default repository in lower case ("" for none), as the run reads them; its code
// groups in order, none where it names none; and its default repository's row of code_repos, where
// the run has read that repository.
type codeProject struct {
	fold       fold.Project
	groups     []survival.Group
	repo       codesurvival.Repo
	repoStored bool
}

// codeProjectOf is read's project as the code evals' rows read it (codeProject).
func codeProjectOf(ctx context.Context, read evalItemsRead) (*codeProject, error) {
	if read.once.code != nil {
		return read.once.code, nil
	}
	project := &codeProject{fold: fold.Project{Key: read.project}}
	if err := read.tx.QueryRow(ctx, `select lower(coalesce(default_repo, '')) from projects where key = $1`, read.project).
		Scan(&project.fold.DefaultRepo); err != nil {
		return nil, fmt.Errorf("read the default repository of %s: %w", read.project, err)
	}
	settings, err := delivery.GetProjectSettings(ctx, read.tx, read.project)
	switch {
	case errors.Is(err, delivery.ErrNoSettings):
	case err != nil:
		return nil, err
	default:
		project.groups = delivery.SurvivalGroups(settings.CodeGroups)
	}
	if project.fold.DefaultRepo != "" {
		if project.repo, project.repoStored, err = codesurvival.GetRepo(ctx, read.tx, project.fold.DefaultRepo); err != nil {
			return nil, err
		}
	}
	read.once.code = project
	return project, nil
}

// codeCutDay is a day, as Unix seconds, of a cut: a repository, or "" for the project's own rows.
type codeCutDay struct {
	cut string
	day int64
}

// codeChange is a change that counts for the project (fold.Change, with its issue's project), the
// group the project's groups give it, and when its day's status row was written, which its links
// are recorded at.
type codeChange struct {
	fold.Change
	group    string
	recorded time.Time
}

// codeRange is what a code eval's readers of one range read once (evalRangeReads): the project, the
// eval's status rows of the range in the cuts the request reads, in day order, and each by its cut
// and day; the changes that count for the project on the days their cut measured, each of a group,
// in commit order; for each marked day of a cut, the repositories of the changes that count for the
// project on it; and the first change of each repository's marked day, by the repository and the
// day.
type codeRange struct {
	project *codeProject
	rows    []evals.DayRow
	status  map[codeCutDay]evals.DayRow
	changes []codeChange
	held    map[codeCutDay][]string
	first   map[codeCutDay]string
}

// codeDayState is the state a code eval's status row states of its day: measured, marked or
// blocked, and "" for a row of none (fold.BuildDays).
func codeDayState(row evals.DayRow) survival.DayState {
	switch {
	case row.Counts[fold.CountMarked] == 1:
		return survival.DayMarked
	case row.Counts[fold.CountBlocked] == 1:
		return survival.DayBlocked
	case row.Counts[fold.CountMeasured] == 1:
		return survival.DayMeasured
	}
	return ""
}

// codeRangeOf is read's range as the code evals' rows read it (codeRange). Without a repository
// filter every change of every repository the run reads (code_repos) is read on the days the
// project's own rows measured or marked; with one, each named repository's changes on the days its
// own rows did; every repository's in one statement (codesurvival.ChangesOn). A change is the
// project's where fold.CountsFor says so, with its issue's project read from issues, as the run
// reads it. No change of a day whose status row states it blocked is read.
func codeRangeOf(ctx context.Context, read evalItemsRead) (*codeRange, error) {
	if read.shared.code != nil {
		return read.shared.code, nil
	}
	project, err := codeProjectOf(ctx, read)
	if err != nil {
		return nil, err
	}
	rows, err := evals.EvalDaysIn(ctx, read.tx, read.entry.Definition, read.deployRepos, fold.SeriesStatus, read.repos, read.rng.days)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(rows, func(a, b evals.DayRow) int { return cmp.Or(a.Day.Compare(b.Day), cmp.Compare(a.Repo, b.Repo)) })
	r := &codeRange{project: project, rows: rows, status: map[codeCutDay]evals.DayRow{}, held: map[codeCutDay][]string{},
		first: map[codeCutDay]string{}}
	for _, row := range rows {
		r.status[codeCutDay{row.Repo, row.Day.Unix()}] = row
	}
	cutOf := func(repo string) string {
		if len(read.repos) == 0 {
			return ""
		}
		return repo
	}
	repos := read.repos
	if len(repos) == 0 {
		if repos, err = codeRepos(ctx, read.tx); err != nil {
			return nil, err
		}
	}
	var dayRepos []string
	var days []time.Time
	for _, repo := range repos {
		for _, row := range rows {
			if state := codeDayState(row); row.Repo == cutOf(repo) && (state == survival.DayMeasured || state == survival.DayMarked) {
				dayRepos, days = append(dayRepos, repo), append(days, row.Day)
			}
		}
	}
	stored, err := codesurvival.ChangesOn(ctx, read.tx, dayRepos, days)
	if err != nil {
		return nil, err
	}
	projects, err := issueProjectsOf(ctx, read.tx, stored)
	if err != nil {
		return nil, err
	}
	for _, change := range stored {
		change.IssueProject = projects[change.IssueKey]
		key := codeCutDay{cutOf(change.Repo), change.Day.Unix()}
		row := r.status[key]
		state := codeDayState(row)
		// The changes of a repository come in commit order, so its day's first is the first seen.
		if first := (codeCutDay{change.Repo, key.day}); state == survival.DayMarked && r.first[first] == "" {
			r.first[first] = change.SHA
		}
		if !fold.CountsFor(change, project.fold) {
			continue
		}
		switch state {
		case survival.DayMeasured:
			if group, ok := survival.GroupOf(change.ChangeFacts, project.groups); ok {
				r.changes = append(r.changes, codeChange{Change: change, group: group, recorded: row.UpdatedAt})
			}
		case survival.DayMarked:
			if !slices.Contains(r.held[key], change.Repo) {
				r.held[key] = append(r.held[key], change.Repo)
			}
		}
	}
	slices.SortStableFunc(r.changes, func(a, b codeChange) int {
		return cmp.Or(a.CommittedAt.Compare(b.CommittedAt), cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.SHA, b.SHA))
	})
	read.shared.code = r
	return r, nil
}

// codeRepos is every repository the code survival run reads, sorted.
func codeRepos(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx, `select repo from code_repos order by repo`)
	if err != nil {
		return nil, fmt.Errorf("read the code repositories: %w", err)
	}
	repos, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the code repositories: %w", err)
	}
	return repos, nil
}

// issueProjectsOf is the project of each issue changes name, by the issue's key; an issue no row
// holds has none.
func issueProjectsOf(ctx context.Context, tx pgx.Tx, changes []fold.Change) (map[string]string, error) {
	var keys []string
	for _, change := range changes {
		if change.IssueKey != "" {
			keys = append(keys, change.IssueKey)
		}
	}
	projects := map[string]string{}
	if len(keys) == 0 {
		return projects, nil
	}
	rows, err := tx.Query(ctx, `select key, project_key from issues where key = any($1::text[])`, keys)
	if err != nil {
		return nil, fmt.Errorf("read the projects of the changes' issues: %w", err)
	}
	var key, project string
	if _, err := pgx.ForEachRow(rows, []any{&key, &project}, func() error {
		projects[key] = project
		return nil
	}); err != nil {
		return nil, fmt.Errorf("read the projects of the changes' issues: %w", err)
	}
	return projects, nil
}

// pagedItems is items as a source whose rows are built a page at a time: every item counts in the
// total, and build makes the rows of the page's items alone.
func pagedItems[T any](items []T, build func(T) evalItemRow) evalItemSource {
	return func(_ context.Context, skip, want int) ([]evalItemRow, int, error) {
		start := min(skip, len(items))
		page := items[start:min(start+max(want, 0), len(items))]
		rows := make([]evalItemRow, len(page))
		for index, item := range page {
			rows[index] = build(item)
		}
		return rows, len(items), nil
	}
}

// changeItemSource is the changes the survival eval's day rows of the range counted (codeRangeOf),
// each a row keyed `<repo>@<sha>`, of the series survival:<g> of its group, in commit order: its
// SHA, its pull request, its group, its issue (null with none), the lines it added, those alive at
// its 30-day mark and their share (null where it added none), and whether it reached the project
// through its repository, with no issue (via_repository); measured at its commit, addressed to its
// pull request, with a via link to its commit. series keeps one group's changes, and row one
// change.
func (s *server) changeItemSource(ctx context.Context, read evalItemsRead) (evalItemSource, error) {
	r, err := codeRangeOf(ctx, read)
	if err != nil {
		return nil, err
	}
	var changes []*codeChange
	for index := range r.changes {
		change := &r.changes[index]
		if (read.request.series == "" || read.request.series == fold.GroupSeries(fold.SeriesSurvival, change.group)) &&
			(read.request.row == "" || read.request.row == codeChangeKey(change.Repo, change.SHA)) {
			changes = append(changes, change)
		}
	}
	return pagedItems(changes, func(change *codeChange) evalItemRow {
		var share *float64
		if change.Added > 0 {
			share = new(float64(change.AliveAtMark) / float64(change.Added))
		}
		return evalItemRow{
			RowKey:  codeChangeKey(change.Repo, change.SHA),
			RowKind: evalRowChange,
			Cols: map[string]any{
				"sha": change.SHA, "pr_number": change.PR, "group": change.group, "issue": nullableText(change.IssueKey),
				"added": change.Added, "alive": change.AliveAtMark, "share": share, "via_repository": change.IssueKey == "",
			},
			MeasuredAt: change.CommittedAt,
			Source:     codeMeasureSource,
			Href:       githubPullURL(change.Repo, change.PR),
			Links:      []evals.Link{commitLink(linkRelVia, change.Repo, change.SHA, "commit", change.recorded)},
			Range:      read.rng.label,
		}
	}), nil
}

// codeChangeKey is the row key of the change sha of repo.
func codeChangeKey(repo, sha string) string { return repo + "@" + sha }

// codePair is one stored pair of repo whose adder is a change the range's day rows counted, and
// the kind of its removal.
type codePair struct {
	survival.Pair
	repo  string
	adder *codeChange
	kind  survival.Kind
}

// key is the pair's row key, `<repo>@<adder>><remover>`.
func (p codePair) key() string { return p.repo + "@" + p.AdderSHA + ">" + p.RemoverSHA }

// pairItemSource is the pairs the rework eval's day rows of the range counted: each stored pair
// whose adder is a change of codeRangeOf (codesurvival.PairsOf, one statement over those changes),
// the remover of a later change that removed lines of it at or before its 30-day mark, in the
// adders' commit order, each a row keyed `<repo>@<adder>><remover>` of the series its kind and its
// adder's group name (codeKindSeries): both issues (each null with none), the kind, the lines
// removed, the days between the two commits, both pull requests and the adder's group; measured at
// the adder's commit, addressed to the remover's pull request, with a via link to the remover's
// commit and a related link to the adder's. series keeps one series' pairs, and row one pair.
func (s *server) pairItemSource(ctx context.Context, read evalItemsRead) (evalItemSource, error) {
	r, err := codeRangeOf(ctx, read)
	if err != nil {
		return nil, err
	}
	adders := make(map[string]*codeChange, len(r.changes))
	adderRepos, adderSHAs := make([]string, len(r.changes)), make([]string, len(r.changes))
	for index := range r.changes {
		change := &r.changes[index]
		adders[codeChangeKey(change.Repo, change.SHA)] = change
		adderRepos[index], adderSHAs[index] = change.Repo, change.SHA
	}
	stored, err := codesurvival.PairsOf(ctx, read.tx, adderRepos, adderSHAs)
	if err != nil {
		return nil, err
	}
	var pairs []codePair
	for _, pair := range stored {
		adder := adders[codeChangeKey(pair.Repo, pair.AdderSHA)]
		one := codePair{Pair: pair.Pair, repo: pair.Repo, adder: adder, kind: survival.KindOf(adder.group, adder.ChangeFacts, pair.Remover, r.project.groups)}
		if (read.request.series == "" || read.request.series == fold.GroupSeries(codeKindSeries[one.kind], adder.group)) &&
			(read.request.row == "" || read.request.row == one.key()) {
			pairs = append(pairs, one)
		}
	}
	slices.SortStableFunc(pairs, func(a, b codePair) int {
		return cmp.Or(a.adder.CommittedAt.Compare(b.adder.CommittedAt), cmp.Compare(a.repo, b.repo), cmp.Compare(a.AdderSHA, b.AdderSHA),
			cmp.Compare(a.RemoverSHA, b.RemoverSHA))
	})
	return pagedItems(pairs, func(pair codePair) evalItemRow {
		return evalItemRow{
			RowKey:  pair.key(),
			RowKind: evalRowPair,
			Cols: map[string]any{
				"adder_issue": nullableText(pair.adder.IssueKey), "remover_issue": nullableText(pair.Remover.IssueKey), "kind": string(pair.kind),
				"lines": pair.Lines, "days_between": pair.DaysBetween, "adder_pr": pair.adder.PR, "remover_pr": pair.Remover.PR,
				"group": pair.adder.group,
			},
			MeasuredAt: pair.adder.CommittedAt,
			Source:     codeMeasureSource,
			Href:       githubPullURL(pair.repo, pair.Remover.PR),
			Links: []evals.Link{
				commitLink(linkRelVia, pair.repo, pair.RemoverSHA, "removing commit", pair.adder.recorded),
				commitLink(linkRelRelated, pair.repo, pair.AdderSHA, "adding commit", pair.adder.recorded),
			},
			Range: read.rng.label,
		}
	}), nil
}

// absentItemRows is each reason the range reads "no data" in the code evals other than days still
// waiting, one row each, every series' (cols.cause): first the settings, each addressed to the
// project's settings page with no link and measured at the read, since a setting is of the present:
// a default repository the App cannot read (repo_unreadable, its repo and the reason code_repos
// holds), no default repository where no day of the range was measured for the project
// (no_default_repo), and no code group (no_group); then the marked days and the blocking commits
// of the range (stoppedItemRows), by day.
func (s *server) absentItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	r, err := codeRangeOf(ctx, read)
	if err != nil {
		return nil, err
	}
	project, now := r.project, time.Now().UTC()
	setting := func(key, source string, cols map[string]any) evalItemRow {
		return evalItemRow{RowKey: key, RowKind: evalRowAbsent, Cols: cols, MeasuredAt: now, Source: source,
			Href: projectSettingsPage(project.fold.Key), Links: []evals.Link{}, Range: read.rng.label}
	}
	var rows []evalItemRow
	if project.repoStored && project.repo.State == codesurvival.RepoUnreadable {
		rows = append(rows, setting(absentRepoUnreadable+"@"+project.repo.Repo, "code_repos",
			map[string]any{"cause": absentRepoUnreadable, "repo": project.repo.Repo, "reason": project.repo.Reason}))
	}
	if project.fold.DefaultRepo == "" && !slices.ContainsFunc(r.rows, func(row evals.DayRow) bool { return codeDayState(row) == survival.DayMeasured }) {
		rows = append(rows, setting(absentNoDefaultRepo, "projects.default_repo", map[string]any{"cause": absentNoDefaultRepo}))
	}
	if len(project.groups) == 0 {
		rows = append(rows, setting(absentNoGroup, "project_delivery_settings.code_groups", map[string]any{"cause": absentNoGroup}))
	}
	stopped, err := r.stoppedItemRows(ctx, read)
	if err != nil {
		return nil, err
	}
	return append(rows, stopped...), nil
}

// codeStopped is one repository's day as code_days holds it: marked, or blocked by a commit with
// the count of its withheld files and why, and when the run last tried it.
type codeStopped struct {
	repo   string
	day    time.Time
	state  survival.DayState
	sha    string
	files  int
	reason string
	tried  time.Time
}

// stoppedItemRows is the absent rows of the range's marked and blocked days, by day and then by
// key. The repositories of a marked or blocked day of a cut are the cut's own, or for the project's
// own rows its default repository and each repository holding a change that counts for it that day,
// as the run builds the rows (codesurvival's projectRows); each of them whose day code_days holds
// in the state the status row states answers. A marked day of a repository is one row keyed
// `marked@<day>@<repo>`, measured on its day: the day, the repository and its first change, to
// which it is addressed, or to the project's settings page where the day holds none. A blocking
// commit is one row keyed `blocked@<repo>@<sha>`, measured on the oldest day of the range it
// blocks: its repository, its SHA, how many files it withheld, why, and the days of the range it
// blocks; addressed to the commit, with a via link to it.
func (r *codeRange) stoppedItemRows(ctx context.Context, read evalItemsRead) ([]evalItemRow, error) {
	want := map[codeCutDay]survival.DayState{}
	var repos []string
	var days []time.Time
	for _, row := range r.rows {
		state := codeDayState(row)
		if state != survival.DayMarked && state != survival.DayBlocked {
			continue
		}
		candidates := []string{row.Repo}
		if row.Repo == "" {
			candidates = nil
			if r.project.fold.DefaultRepo != "" {
				candidates = append(candidates, r.project.fold.DefaultRepo)
			}
			candidates = append(candidates, r.held[codeCutDay{"", row.Day.Unix()}]...)
		}
		for _, repo := range candidates {
			key := codeCutDay{repo, row.Day.Unix()}
			if _, named := want[key]; !named {
				want[key] = state
				repos, days = append(repos, repo), append(days, row.Day)
			}
		}
	}
	if len(repos) == 0 {
		return nil, nil
	}
	stopped, err := codeStoppedIn(ctx, read.tx, repos, days)
	if err != nil {
		return nil, err
	}
	var rows []evalItemRow
	blocked := map[string]int{}
	for _, day := range stopped {
		if want[codeCutDay{day.repo, day.day.Unix()}] != day.state {
			continue
		}
		date := day.day.Format(time.DateOnly)
		if day.state == survival.DayMarked {
			sha := r.first[codeCutDay{day.repo, day.day.Unix()}]
			href := projectSettingsPage(r.project.fold.Key)
			if sha != "" {
				href = githubCommitURL(day.repo, sha)
			}
			rows = append(rows, evalItemRow{
				RowKey: absentMarked + "@" + date + "@" + day.repo, RowKind: evalRowAbsent,
				Cols:       map[string]any{"cause": absentMarked, "day": date, "repo": day.repo, "sha": nullableText(sha)},
				MeasuredAt: day.day, Source: "code_days", Href: href, Links: []evals.Link{}, Range: read.rng.label,
			})
			continue
		}
		key := absentBlocked + "@" + day.repo + "@" + day.sha
		if index, found := blocked[key]; found {
			row := &rows[index]
			row.Cols["days"] = append(row.Cols["days"].([]string), date)
			if day.tried.After(row.Links[0].RecordedAt) {
				row.Links[0].RecordedAt = day.tried
			}
			continue
		}
		blocked[key] = len(rows)
		rows = append(rows, evalItemRow{
			RowKey: key, RowKind: evalRowAbsent,
			Cols: map[string]any{
				"cause": absentBlocked, "repo": day.repo, "sha": day.sha, "files": day.files, "reason": nullableText(day.reason),
				"days": []string{date},
			},
			MeasuredAt: day.day, Source: "code_days", Href: githubCommitURL(day.repo, day.sha),
			Links: []evals.Link{commitLink(linkRelVia, day.repo, day.sha, "commit", day.tried)}, Range: read.rng.label,
		})
	}
	slices.SortStableFunc(rows, func(a, b evalItemRow) int {
		return cmp.Or(a.MeasuredAt.Compare(b.MeasuredAt), cmp.Compare(a.RowKey, b.RowKey))
	})
	return rows, nil
}

// codeStoppedIn is each marked or blocked day code_days holds of the repositories and days, pairs
// of repos and days, read in tx, by day and repository.
func codeStoppedIn(ctx context.Context, tx pgx.Tx, repos []string, days []time.Time) ([]codeStopped, error) {
	rows, err := tx.Query(ctx, `
		select c.repo, c.day, c.state, coalesce(c.blocked_sha, ''), coalesce(c.blocked_files, 0), coalesce(c.blocked_reason, ''),
			coalesce(c.tried_at, c.marked_at, c.measured_at)
		from code_days c join unnest($1::text[], $2::date[]) as w(repo, day) on c.repo = w.repo and c.day = w.day
		where c.state in ('marked', 'blocked')
		order by c.day, c.repo`, repos, days)
	if err != nil {
		return nil, fmt.Errorf("read the marked and blocked days of %v: %w", repos, err)
	}
	stopped, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (codeStopped, error) {
		var (
			day   codeStopped
			state string
		)
		err := row.Scan(&day.repo, &day.day, &state, &day.sha, &day.files, &day.reason, &day.tried)
		day.day, day.state, day.tried = day.day.UTC(), survival.DayState(state), day.tried.UTC()
		return day, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the marked and blocked days of %v: %w", repos, err)
	}
	return stopped, nil
}

// githubCommitURL is the address of sha, a commit of repo, on GitHub, and githubPullURL that of
// repo's pull request number.
func githubCommitURL(repo, sha string) string { return "https://github.com/" + repo + "/commit/" + sha }

func githubPullURL(repo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
}

// commitLink is a link of rel to sha, a commit of repo, on GitHub, labelled what and the commit's
// short SHA, recorded when the result that names it was stored.
func commitLink(rel, repo, sha, what string, recorded time.Time) evals.Link {
	return evals.Link{Rel: rel, Href: githubCommitURL(repo, sha), Label: what + " " + sha[:min(len(sha), 7)], RecordedAt: recorded}
}

// projectSettingsPage is the address of project's settings page, where its default repository and
// its code groups are set.
func projectSettingsPage(project string) string {
	return "/projects/" + url.PathEscape(project) + "/settings"
}
