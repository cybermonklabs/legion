// Package fold builds the day parts of dispatch://INFRA-3206's two evals, code survival at 30
// days and post-merge rework at 30 days, from the stored results of survival.Measure, and folds
// them back into each group's figures: BuildDays is the day builder, and FoldSurvival,
// FoldRework and FoldStatus the folds the evals' sources name. It is a package of its own because
// delivery/measures imports delivery, which reaches GitHub and the store, and the survival
// computation imports the standard library alone.
package fold

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
)

// The series of the two evals. Each group g of a project has its series `<base>:<g>`
// (GroupSeries): survival:<g> in the survival eval; rework:<g> (the headline), iteration:<g>,
// noissue:<g> and other:<g> in the rework eval. Both evals have the control series status.
const (
	SeriesStatus    = "status"
	SeriesSurvival  = "survival"
	SeriesRework    = "rework"
	SeriesIteration = "iteration"
	SeriesNoIssue   = "noissue"
	SeriesOther     = "other"
)

// The counts of a status part: whether its day is measured, marked or blocked, the files the
// blocking commit withheld, how many groups the project names, whether its default repository is
// one the App cannot read, and, on a marked day, that day's UTC midnight in Unix seconds. Every
// group part carries repo_unreadable too.
const (
	CountMeasured       = "measured"
	CountMarked         = "marked"
	CountBlocked        = "blocked"
	CountBlockedFiles   = "blocked_files"
	CountGroups         = "groups"
	CountRepoUnreadable = "repo_unreadable"
	CountFirstMarkedDay = "first_marked_day"
)

// GroupSeries is group's series of the base series, `<base>:<group>`.
func GroupSeries(base, group string) string { return base + ":" + group }

// Project is the project a build is for: its key, and its default repository ("" for none).
type Project struct {
	Key         string
	DefaultRepo string
}

// Change is one stored change of a repository, with the project of its issue as the caller
// resolved it ("" with no issue).
type Change struct {
	Repo         string
	IssueProject string
	survival.Change
}

// Pair is one stored pair of a repository.
type Pair struct {
	Repo string
	survival.Pair
}

// Day is one of the project's decided days: measured, marked or blocked (with the files the
// blocking commit withheld), as its default repository's day is.
type Day struct {
	Day          time.Time
	State        survival.DayState
	BlockedFiles int
}

// BuildInput is what BuildDays reads: the project, its groups in order, whether its default
// repository is one the App cannot read, its decided days, and the stored changes and pairs of
// the repositories that hold changes for it.
type BuildInput struct {
	Project        Project
	Groups         []survival.Group
	RepoUnreadable bool
	Days           []Day
	Changes        []Change
	Pairs          []Pair
}

// CountsFor says whether change counts for project (R11): a change with an issue counts for its
// issue's project, and one with no issue for each project whose default repository holds it.
func CountsFor(change Change, project Project) bool {
	if change.IssueKey != "" {
		return change.IssueProject != "" && change.IssueProject == project.Key
	}
	return project.DefaultRepo != "" && strings.EqualFold(change.Repo, project.DefaultRepo)
}

// BuildDays builds the project's day parts of the two evals, the survival eval's and the rework
// eval's, in day order. A measured day has, for each group g in order, survival:<g> (the lines g's
// changes that count for the project added that were alive at their marks, over the lines they
// added, N their count and ViaRepository those with no issue) and rework:<g>, iteration:<g>,
// noissue:<g> and other:<g> (the lines of those removed in that kind, over the lines they added),
// a zero part for a group with no change; and each eval's status part. A marked or blocked day has
// the status part alone, and a day that is none of the three has no part (R7). A pair's kind is
// survival.KindOf's, and a change's removed_by_non_pr is of the kind other.
func BuildDays(in BuildInput) (survivalParts, reworkParts []measures.DayPart) {
	byDay := map[time.Time][]*Change{}
	for index := range in.Changes {
		change := &in.Changes[index]
		if CountsFor(*change, in.Project) {
			day := survival.DayOf(change.Day)
			byDay[day] = append(byDay[day], change)
		}
	}
	pairs := map[changeKey][]*survival.Pair{}
	for index := range in.Pairs {
		pair := &in.Pairs[index]
		key := changeKey{strings.ToLower(pair.Repo), pair.AdderSHA}
		pairs[key] = append(pairs[key], &pair.Pair)
	}
	unreadable := 0.0
	if in.RepoUnreadable {
		unreadable = 1
	}
	days := slices.Clone(in.Days)
	slices.SortStableFunc(days, func(a, b Day) int { return a.Day.Compare(b.Day) })
	for _, decided := range days {
		day := survival.DayOf(decided.Day)
		status := map[string]float64{CountMeasured: 0, CountMarked: 0, CountBlocked: 0, CountBlockedFiles: 0,
			CountGroups: float64(len(in.Groups)), CountRepoUnreadable: unreadable}
		switch decided.State {
		case survival.DayMeasured:
			status[CountMeasured] = 1
			for index, tally := range tallyDay(byDay[day], pairs, in.Groups) {
				part := func(base string, lines int) measures.DayPart {
					part := measures.RatioPart(day, GroupSeries(base, in.Groups[index].Name), lines, tally.added,
						map[string]float64{CountRepoUnreadable: unreadable})
					part.N, part.ViaRepository = tally.changes, tally.via
					return part
				}
				survivalParts = append(survivalParts, part(SeriesSurvival, tally.alive))
				reworkParts = append(reworkParts, part(SeriesRework, tally.rework), part(SeriesIteration, tally.iteration),
					part(SeriesNoIssue, tally.noIssue), part(SeriesOther, tally.other))
			}
		case survival.DayMarked:
			status[CountMarked] = 1
			status[CountFirstMarkedDay] = float64(day.Unix())
		case survival.DayBlocked:
			status[CountBlocked] = 1
			status[CountBlockedFiles] = float64(decided.BlockedFiles)
		default:
			continue
		}
		survivalParts = append(survivalParts, measures.DayPart{Series: SeriesStatus, Day: day, Counts: status})
		reworkParts = append(reworkParts, measures.DayPart{Series: SeriesStatus, Day: day, Counts: maps.Clone(status)})
	}
	return survivalParts, reworkParts
}

// changeKey is a change's repository, in lower case, and SHA.
type changeKey struct{ repo, sha string }

// groupTally is one group's changes of one day, added up: how many, how many with no issue, the
// lines they added and those alive at their marks, and the lines removed in each kind.
type groupTally struct {
	changes, via, added, alive        int
	rework, iteration, noIssue, other int
}

// tallyDay is each group's tally of changes, one day's changes that count for the project, with
// the pairs of each, in the order of groups. A change in no group is in no tally.
func tallyDay(changes []*Change, pairs map[changeKey][]*survival.Pair, groups []survival.Group) []groupTally {
	tallies := make([]groupTally, len(groups))
	for _, change := range changes {
		group, ok := survival.GroupOf(change.ChangeFacts, groups)
		if !ok {
			continue
		}
		tally := &tallies[slices.IndexFunc(groups, func(g survival.Group) bool { return g.Name == group })]
		tally.changes++
		tally.added += change.Added
		tally.alive += change.AliveAtMark
		if change.IssueKey == "" {
			tally.via++
		}
		tally.other += change.RemovedByNonPR
		for _, pair := range pairs[changeKey{strings.ToLower(change.Repo), change.SHA}] {
			switch survival.KindOf(group, change.ChangeFacts, pair.Remover, groups) {
			case survival.KindRework:
				tally.rework += pair.Lines
			case survival.KindIteration:
				tally.iteration += pair.Lines
			case survival.KindNoIssue:
				tally.noIssue += pair.Lines
			default:
				tally.other += pair.Lines
			}
		}
	}
	return tallies
}
