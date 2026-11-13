package evals

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// The Legion evals' day rows (dispatch://INFRA-2916, R6, R12, R16, D3 and D5). One project's UTC
// day is read once (ReadLegionDay): the stored facts of its issues that closed that day
// (legionfacts.ReadClosedIn, through issue_facts_closed), every one an issue with Legion phase
// work since only such an issue has a row, and the filings its Legion sessions made that day
// (legionfacts.ReadFilingsIn, through issue_filings_day). The legion/metrics day builders turn
// them into parts (LegionDay.Rows), and WriteLegionProjectDay writes the parts as the project's
// rows of the day, which the eval routes fold through the six legion/metrics folds the evals'
// source names.
//
// A project's Legion rows of a day are, for each eval with input that day, every series of the
// eval in each of its cuts (metrics.CutSeries, LegionSeries): the all-repositories row (repo ''),
// built from every issue and filing of the day, and one row per repository the eval's input is
// in, built from that repository's alone. An issue's repository, and a filing's, is the one the
// issue (the filed issue) resolves to by the rule that fills issue_facts.repo
// (legionfacts.ReadRepos), resolved when the day is built rather than read from the stored row:
// the rule falls back to the project's default mapping in repo_projects, a change of which moves
// no stored row, and the refresher rebuilds every Legion day when a mapping changes, so the rows
// follow the mapping within that one pass. An issue or a filing that resolves to no repository is
// in the all-repositories row alone, so the repositories' rows add up to that row only where every
// input resolves to one. Every Legion row reached its project through its own issue, so its
// via_repository is 0.
//
// legion-splitting has input on a day with a closed issue or a filing, and the other five on a
// day with a closed issue. legion-idle-time has none on a day before the first UTC midnight at or
// after the project's first claim observation, and legion-slot-wait none before the same of its
// first slot observation (LegionSince): R6 has each read "no data" for a window that starts before
// that observation, and a row of an earlier day would hold hours read with no claim observed,
// every one of them idle, or with no slot observed. A day with no input of an eval writes no row
// of it: the explicit zero rows of a series from its first input day on are the refresher's
// (ZeroLegionDay, ProjectDay.WithZeros).
//
// Each issue counted is an item of the family `legion` in eval_day_members, keyed by its issue
// key, on its close day with its repository, so an issue that closes again on another day
// rewrites the day it left as well as the day it joined. A filing's day is the time of its
// `issue.created` event, which never moves, so a filing is no item.

// The slugs of the Legion evals (dispatch://INFRA-2916/artifact/plan-md, "The seeded catalogue"):
// an eval's id is EvalID(project, slug).
const (
	SlugLegionWorkTime  = "legion-work-time"
	SlugLegionPhaseTime = "legion-phase-time"
	SlugLegionIdleTime  = "legion-idle-time"
	SlugLegionSlotWait  = "legion-slot-wait"
	SlugLegionRework    = "legion-rework"
	SlugLegionSplitting = "legion-splitting"
)

// legionDaySlugs is every eval a Legion day's rows are of, and so every eval WriteLegionProjectDay
// deletes the rows of that a day no longer holds.
var legionDaySlugs = []string{
	SlugLegionWorkTime, SlugLegionPhaseTime, SlugLegionIdleTime, SlugLegionSlotWait, SlugLegionRework,
	SlugLegionSplitting,
}

// legionDayFamilies is the one family of eval_day_members a Legion day records its issues under,
// the evals' kind.
var legionDayFamilies = []string{KindLegion}

// legionBuilders is each Legion eval's day builder over one day's finished issues and filings.
var legionBuilders = map[string]func(day time.Time, finished []metrics.IssueFacts, filed []metrics.IssueFiling) []measures.DayPart{
	SlugLegionWorkTime: func(day time.Time, finished []metrics.IssueFacts, _ []metrics.IssueFiling) []measures.DayPart {
		return metrics.WorkTimeDay(day, finished)
	},
	SlugLegionPhaseTime: func(day time.Time, finished []metrics.IssueFacts, _ []metrics.IssueFiling) []measures.DayPart {
		return metrics.PhaseTimeDay(day, finished)
	},
	SlugLegionIdleTime: func(day time.Time, finished []metrics.IssueFacts, _ []metrics.IssueFiling) []measures.DayPart {
		return metrics.IdleTimeDay(day, finished)
	},
	SlugLegionSlotWait: func(day time.Time, finished []metrics.IssueFacts, _ []metrics.IssueFiling) []measures.DayPart {
		return metrics.SlotWaitDay(day, finished)
	},
	SlugLegionRework: func(day time.Time, finished []metrics.IssueFacts, _ []metrics.IssueFiling) []measures.DayPart {
		return metrics.ReworkDay(day, finished)
	},
	SlugLegionSplitting: metrics.SplittingDay,
}

// LegionSeries is every series a Legion eval's day rows are stored under, each of its series in
// each of its cuts, in the order its builder builds them; none for a slug of no Legion eval.
func LegionSeries(slug string) []string {
	build, ok := legionBuilders[slug]
	if !ok {
		return nil
	}
	parts := build(time.Time{}, nil, nil)
	series := make([]string, len(parts))
	for i, part := range parts {
		series[i] = part.Series
	}
	return series
}

// LegionSince is the first UTC day of the two Legion evals bound to an observation: Claims, the
// first UTC midnight at or after the project's first `claim.<role>` observation, from which
// legion-idle-time has input, and Slots, the same of its first `slot` observation, from which
// legion-slot-wait has. Each is nil while the project has no such observation. A day-aligned
// window that starts before one starts before that observation (R6).
type LegionSince struct {
	Claims *time.Time
	Slots  *time.Time
}

// ReadLegionSince reads project's LegionSince from legion_observations. The refresher reads it
// once a pass and hands it to every day of the project it builds.
func (s *Store) ReadLegionSince(ctx context.Context, project string) (LegionSince, error) {
	var claims, slots *time.Time
	if err := s.database.Pool.QueryRow(ctx, `
		select (select min(observed_at) from legion_observations where project = $1 and starts_with(field, $2)),
		       (select min(observed_at) from legion_observations where project = $1 and field = $3)
	`, project, legion.ClaimFieldPrefix, legion.FieldSlot).Scan(&claims, &slots); err != nil {
		return LegionSince{}, fmt.Errorf("read the first claim and slot observations of %s: %w", project, err)
	}
	return LegionSince{Claims: wholeDayFrom(claims), Slots: wholeDayFrom(slots)}, nil
}

// wholeDayFrom is the first UTC midnight at or after at, nil for nil.
func wholeDayFrom(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	day := utcMidnight(*at)
	if day.Before(*at) {
		day = day.AddDate(0, 0, 1)
	}
	return &day
}

// onOrAfter is whether day is on or after first, a nil first being no day at all.
func onOrAfter(first *time.Time, day time.Time) bool {
	return first != nil && !day.Before(*first)
}

// LegionDay is one project's Legion input of one UTC day, read by ReadLegionDay: the stored facts
// of the issues that closed on it and the filings made on it, with the repository each closed
// issue and each filed issue resolves to.
type LegionDay struct {
	Project string
	// Day is the day's UTC midnight.
	Day    time.Time
	closed []legionfacts.Row
	filed  []legionfacts.Filing
	// repos is the repository of each closed issue and each filed issue that resolves to one.
	repos map[string]string
}

// ReadLegionDay reads project's Legion input of the UTC day day is on: its issues that closed
// that day (legionfacts.ReadClosedIn), the filings its Legion sessions made that day
// (legionfacts.ReadFilingsIn) and the repository each of those issues resolves to now
// (legionfacts.ReadRepos, one read of them all).
func (s *Store) ReadLegionDay(ctx context.Context, project string, day time.Time) (LegionDay, error) {
	start := utcMidnight(day)
	end := start.AddDate(0, 0, 1)
	pool := s.database.Pool
	closed, err := legionfacts.ReadClosedIn(ctx, pool, project, start, end)
	if err != nil {
		return LegionDay{}, err
	}
	filed, err := legionfacts.ReadFilingsIn(ctx, pool, project, start, end)
	if err != nil {
		return LegionDay{}, err
	}
	keys := make([]string, 0, len(closed)+len(filed))
	for _, row := range closed {
		keys = append(keys, row.Issue)
	}
	for _, filing := range filed {
		keys = append(keys, filing.Issue)
	}
	repos, err := legionfacts.ReadRepos(ctx, pool, keys)
	if err != nil {
		return LegionDay{}, err
	}
	return LegionDay{Project: project, Day: start, closed: closed, filed: filed, repos: repos}, nil
}

// legionInput is a day's issues and filings, or one repository's of them, as the builders read
// them.
type legionInput struct {
	finished []metrics.IssueFacts
	filed    []metrics.IssueFiling
}

// Rows is the project's Legion rows of the day under since, built by the legion/metrics day
// builders, and the issues they count; no row and no item for a day with no input.
func (d LegionDay) Rows(since LegionSince) ProjectDay {
	result := ProjectDay{Project: d.Project, Day: d.Day}
	var all legionInput
	byRepo := map[string]*legionInput{}
	repoInput := func(repo string) *legionInput {
		if byRepo[repo] == nil {
			byRepo[repo] = &legionInput{}
		}
		return byRepo[repo]
	}
	for _, row := range d.closed {
		facts := issueFacts(row)
		repo := d.repos[row.Issue]
		all.finished = append(all.finished, facts)
		if repo != "" {
			repoInput(repo).finished = append(repoInput(repo).finished, facts)
		}
		result.Members = append(result.Members, Member{Family: KindLegion, RowKey: row.Issue, Repo: repo})
	}
	for _, filing := range d.filed {
		one := issueFiling(filing)
		all.filed = append(all.filed, one)
		if repo := d.repos[filing.Issue]; repo != "" {
			repoInput(repo).filed = append(repoInput(repo).filed, one)
		}
	}
	result.Rows = d.build("", all, since)
	for _, repo := range slices.Sorted(maps.Keys(byRepo)) {
		result.Rows = append(result.Rows, d.build(repo, *byRepo[repo], since)...)
	}
	slices.SortFunc(result.Members, func(a, b Member) int { return cmp.Compare(a.RowKey, b.RowKey) })
	return result
}

// build is the rows of the project's cut repo ("" for all repositories) from input, the evals with
// input in it alone.
func (d LegionDay) build(repo string, input legionInput, since LegionSince) []DayRow {
	var rows []DayRow
	for _, slug := range legionDaySlugs {
		has := len(input.finished) > 0
		switch slug {
		case SlugLegionSplitting:
			has = has || len(input.filed) > 0
		case SlugLegionIdleTime:
			has = has && onOrAfter(since.Claims, d.Day)
		case SlugLegionSlotWait:
			has = has && onOrAfter(since.Slots, d.Day)
		}
		if has {
			rows = append(rows, legionRows(d.Project, repo, slug, legionBuilders[slug](d.Day, input.finished, input.filed))...)
		}
	}
	return rows
}

// legionRows is parts as project's rows of slug in the cut repo, every one listed.
func legionRows(project, repo, slug string, parts []measures.DayPart) []DayRow {
	rows := make([]DayRow, len(parts))
	key := DayRow{ProjectKey: project, EvalID: EvalID(project, slug), Repo: repo, Listed: true}
	for i, part := range parts {
		rows[i] = key.WithPart(part)
	}
	return rows
}

// ZeroLegionDay is project's all-repositories row of every series of each eval slugs names on day,
// as the day builders build a day with no input (n 0, a zero sum, numerator and denominator, no
// sample, and zero counts): the explicit zero rows the refresher writes for a series from its
// first input day on, on a day with no input (ProjectDay.WithZeros). A slug of no Legion eval has
// none, and neither has the children cut of legion-splitting's root figures, which no day holds.
func ZeroLegionDay(project string, day time.Time, slugs ...string) ProjectDay {
	result := ProjectDay{Project: project, Day: utcMidnight(day)}
	for _, slug := range slugs {
		if build, ok := legionBuilders[slug]; ok {
			result.Rows = append(result.Rows, legionRows(project, "", slug, build(result.Day, nil, nil))...)
		}
	}
	return result
}

// WriteLegionProjectDay writes day as its project's Legion rows of its day, as WriteProjectDay
// writes a delivery day: each row upserted, recording builtBy, the running build; the project's
// rows of that day of the Legion evals (legionDaySlugs) that day does not hold deleted; and its
// issues recorded in eval_day_members on its day under the family `legion`, whose items recorded
// on that day that it does not hold are deleted. A row of another project, another day or an eval
// that is not a Legion one, and an item of another family, is refused before anything is written.
func (s *Store) WriteLegionProjectDay(ctx context.Context, day ProjectDay, builtBy string) error {
	return s.writeProjectDay(ctx, "Legion", day, legionDaySlugs, legionDayFamilies, builtBy)
}

// WriteLegionDay reads project's Legion input of the UTC day day is on (ReadLegionDay) and writes
// its rows under since (LegionDay.Rows, WriteLegionProjectDay). A day with no input deletes the
// project's Legion rows and items of the day. builtBy is the running build, which every row
// written records.
func (s *Store) WriteLegionDay(ctx context.Context, project string, day time.Time, since LegionSince, builtBy string) error {
	read, err := s.ReadLegionDay(ctx, project, day)
	if err != nil {
		return err
	}
	return s.WriteLegionProjectDay(ctx, read.Rows(since), builtBy)
}

// issueFacts is a stored row as the day builders read it: the facts metrics.ProjectFacts computed
// of the issue, which the row holds column for column.
func issueFacts(row legionfacts.Row) metrics.IssueFacts {
	return metrics.IssueFacts{
		OpenedAt: row.OpenedAt, ClosedAt: row.ClosedAt, PhaseHours: row.HoursByPhase, WorkHours: row.WorkHours,
		ReviewRounds: row.ReviewRounds, Root: row.RootKey == row.Issue, Tree: row.RootKey, TreeDepth: row.TreeDepth,
		SlotHours: row.SlotHours, WorkFirstAt: row.WorkFirstAt, WorkLastAt: row.WorkLastAt,
		ParkedHours: row.ParkedHours, QueuedHours: row.QueuedHours, StartedAt: row.StartedAt, MergedAt: row.MergedAt,
		Bars: row.Bars, SentBack: row.SentBack, ImplementingHoursAfterFirstReview: row.ImplementingAfterFirstReview,
		TestingHoursAfterFirstReview: row.TestingAfterFirstReview, Children: row.Children,
		SlotWaitHours: row.SlotWaitHours, IdleByRole: row.IdleByRole,
	}
}

// issueFiling is a stored filing as the splitting builder reads it: the row metric 4's table names
// it by and the filed issue's tree root.
func issueFiling(filing legionfacts.Filing) metrics.IssueFiling {
	return metrics.IssueFiling{
		FiledIssue: metrics.FiledIssue{
			Key: filing.Issue, At: filing.FiledAt, Class: filing.Class, Filer: filing.FilerRole,
			FilerIssue: filing.FilerIssue, FilerPhase: filing.FilerPhase,
		},
		Root: filing.RootKey,
	}
}
