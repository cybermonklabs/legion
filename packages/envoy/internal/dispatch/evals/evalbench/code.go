package evalbench

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/evals/evalfixture"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The code survival results of dispatch://INFRA-3206 (R15, check k17): what the daily run of
// codesurvival keeps of a default repository's first-parent chain, written at the rate the
// reference window of cybermonklabs/legion measured, so the Evals tab's first paint is measured
// over the rows a project holds after a year of runs. The rows are the run's results alone: the
// changes, the pairs, the fingerprint of each replayed day and the repository, never a patch; and
// the project's code groups, which the regroup builds the two code evals' day rows from.
//
// The history is codeDays days of the chain of INFRA's default repository, the first of Repos, from
// the day codeDays days before Now's to the day before it. A day's commits come at codeCommitsPerDay
// a day at the load's scale, a share of them (codeChangeShare) a change that landed a pull request,
// and each change holds the pairs that later changes made of its lines, codeRemovalsPerChange of
// them on average, which is the rate of pairs the reference window had (codePairsPer90Days). The
// days that began to wait at least 33 days before Now's day are measured, as a run measures them
// (survival.WaitsFrom), and the codeTailDays days after the newest of them hold the fingerprint the
// run's replay left; the days after those wait, and hold no row. The repository's row says a daily
// pass ran on Now's day and that the next may not start before a year after Now (retry_at, which
// the run's guard reads beside checked_on), so the server's run does no pass during the
// measurement, on Now's day or after it (a job that runs past UTC midnight, or an EVALBENCH_NOW
// before the server's day), and asks GitHub for nothing; code_projects is left empty, so the
// regroup's pass at the server's start builds INFRA's day rows from these results, with no request
// to GitHub and no App.

// The rates of the reference window (plan, T12), from cybermonklabs/legion's default branch: 63.4
// first-parent commits a day, 1,962 changes among its 3,356 commits, 905 of them the agents', and
// 27,967 pairs in 90 days.
const (
	// codeDays is how many days of the chain the history holds.
	codeDays           = 365
	codeCommitsPerDay  = 63.4
	codeChangeShare    = 1962.0 / 3356.0
	codeLegionShare    = 905.0 / 1962.0
	codePairsPer90Days = 27_967.0
	// codeRemovalsPerChange is how many pairs a measured change holds on average: the pairs of
	// 90 days over the changes of 90 days.
	codeRemovalsPerChange = codePairsPer90Days / 90 / (codeCommitsPerDay * codeChangeShare)
	// codeTailDays is how many days after the newest measured day a run's replay fingerprints
	// (survival's tailDays).
	codeTailDays = 30
	// codeFirstPR is the number of the first change's pull request.
	codeFirstPR = 1000
	// The share of the agents' changes, and of every other change, that name an INFRA issue.
	codeAgentIssueShare = 0.85
	codeOtherIssueShare = 0.35
)

// codeGroups are INFRA's two code groups, in the order its delivery settings hold them
// (project_delivery_settings.code_groups): the agents' changes, by a head branch under legion/ or
// by the login of one of their Apps, and every other change after them.
var codeGroups = []delivery.CodeGroup{
	{Name: "Legion", BranchPrefixes: []string{"legion/"}, AuthorLogins: []string{"legion-implementer[bot]", "acme-legion-implementer[bot]"}},
	{Name: "Other", BranchPrefixes: []string{}, AuthorLogins: []string{}},
}

// codeTables are the tables of the code survival results, the repository's row, the replayed
// days, the changes, the pairs and the projects whose day rows were built, which a head with the
// results holds and the base does not.
var codeTables = []string{"code_repos", "code_days", "code_changes", "code_pairs", "code_projects"}

// codeChange is one change of the chain before it is written: the commit a pull request landed as,
// the pull request's facts, and its lines. added is alive, removedByNonPR and the lines its pairs
// removed before its mark.
type codeChange struct {
	sha             string
	committed       time.Time
	pr              int
	headRef, author string
	issue           string
	added           int
	alive           int
	removedByNonPR  int
}

// codePair is one pair of a measured change (the adder) and a later change that removed some of its
// lines before its mark (the remover), both by their index among the plan's changes.
type codePair struct {
	adder, remover int
	lines          int
	daysBetween    float64
}

// codePlan is the history before it is written.
type codePlan struct {
	// first is the first day of the chain, and chain every first-parent commit of it in order.
	first time.Time
	chain []survival.Commit
	// changes is every change of the chain in order, measured the first measured of them, those of
	// the days the run has measured: the changes of the days after hold the pairs' removers and are
	// no row.
	changes  []codeChange
	measured int
	// measuredDays is how many days, from the first, are measured: those that began to wait by Now.
	measuredDays int
	pairs        []codePair
	// fingerprints is the fingerprint of each day of the chain, from the first.
	fingerprints []survival.Fingerprint
}

// planCode draws the history of spec: every commit, change and pair, the same for one seed. It draws
// from its own random stream, so it moves no other part of the load.
func planCode(spec Spec, history projectHistory) codePlan {
	rng := newRand(spec.Seed, "code survival")
	opened := make([]time.Time, len(history.issues))
	for i, issue := range history.issues {
		opened[i] = issue.opened
	}
	plan := codePlan{first: survival.DayOf(spec.Now).AddDate(0, 0, -codeDays)}
	pr, carry := codeFirstPR, 0.0
	for d := range codeDays {
		day := plan.first.AddDate(0, 0, d)
		// The day's commits are the day's rate to the whole commit, the rest carried to the next
		// day, so the history holds the rate however small the scale.
		carry += codeCommitsPerDay * spec.Scale
		commitsToday := int(carry)
		carry -= float64(commitsToday)
		offsets := make([]time.Duration, commitsToday)
		for i := range offsets {
			offsets[i] = within(rng, 24*time.Hour)
		}
		slices.Sort(offsets)
		for _, offset := range offsets {
			commit := survival.Commit{SHA: sha(rng), CommittedAt: day.Add(offset)}
			plan.chain = append(plan.chain, commit)
			if rng.Float64() < codeChangeShare {
				plan.changes = append(plan.changes, planChange(rng, history, opened, commit, pr))
				pr++
			}
		}
		if !survival.WaitsFrom(day).After(spec.Now) {
			plan.measuredDays = d + 1
		}
	}
	newest := plan.first.AddDate(0, 0, plan.measuredDays)
	plan.measured = sort.Search(len(plan.changes), func(i int) bool { return !plan.changes[i].committed.Before(newest) })

	// The pairs: each measured change holds about codeRemovalsPerChange of them, each by a later
	// change within its 30 days, and its lines are what was alive at its mark and what was removed
	// before it.
	removals := 0.0
	for i := range plan.measured {
		change := &plan.changes[i]
		removals += codeRemovalsPerChange * (0.5 + rng.Float64())
		wanted := int(removals)
		removals -= float64(wanted)
		until := survival.Mark(change.committed)
		low := i + 1
		high := sort.Search(len(plan.changes), func(j int) bool { return plan.changes[j].committed.After(until) })
		var taken []int
		removed := 0
		for ; wanted > 0 && low < high; wanted-- {
			remover := -1
			for range 8 {
				if next := low + rng.IntN(high-low); !slices.Contains(taken, next) {
					remover = next
					break
				}
			}
			if remover < 0 {
				continue
			}
			taken = append(taken, remover)
			lines := 1 + int(rng.ExpFloat64()*2)
			removed += lines
			plan.pairs = append(plan.pairs, codePair{adder: i, remover: remover, lines: lines,
				daysBetween: plan.changes[remover].committed.Sub(change.committed).Hours() / 24})
		}
		change.added = change.alive + change.removedByNonPR + removed
	}
	plan.fingerprints = survival.Fingerprints(plan.chain, plan.first, plan.first.AddDate(0, 0, codeDays-1))
	return plan
}

// planChange draws the change commit landed as: its pull request number pr, its head branch and
// author, the issue it names where it names one, and its lines alive at the mark and removed by
// commits that merged no pull request. An agents' change is, by the share of codeLegionShare, one
// of a head branch under legion/ by a login of an agents' App, or by the branch alone, or by the
// login alone, so every way a group takes a change is held; every other change is a person's.
func planChange(rng *rand.Rand, history projectHistory, opened []time.Time, commit survival.Commit, pr int) codeChange {
	change := codeChange{sha: commit.SHA, committed: commit.CommittedAt, pr: pr}
	agents := rng.Float64() < codeLegionShare
	share := codeOtherIssueShare
	if agents {
		share = codeAgentIssueShare
	}
	if rng.Float64() < share {
		change.issue = issueOpenAt(rng, history, opened, commit.CommittedAt)
	}
	word := words[rng.IntN(len(words))]
	tail := word + "-" + strconv.Itoa(pr)
	if change.issue != "" {
		tail = change.issue + "-" + word
	}
	person := authors[rng.IntN(len(authors))]
	bot := codeGroups[0].AuthorLogins[rng.IntN(len(codeGroups[0].AuthorLogins))]
	switch u := rng.Float64(); {
	case !agents:
		change.headRef, change.author = "feature/"+tail, person
	case u < 0.8:
		change.headRef, change.author = codeGroups[0].BranchPrefixes[0]+tail, bot
	case u < 0.9:
		change.headRef, change.author = codeGroups[0].BranchPrefixes[0]+tail, person
	default:
		change.headRef, change.author = "bot/"+tail, bot
	}
	change.alive = max(1, min(20_000, int(math.Round(90*math.Exp(1.1*rng.NormFloat64())))))
	change.removedByNonPR = int(float64(change.alive) * 0.1 * rng.Float64())
	return change
}

// issueOpenAt is the key of an INFRA issue open at at, drawn from those opened in the fortnight
// before it as a pull request's issue is (planPullRequests), and "" when the draws find none.
func issueOpenAt(rng *rand.Rand, history projectHistory, opened []time.Time, at time.Time) string {
	last := sort.Search(len(opened), func(i int) bool { return opened[i].After(at) })
	first := sort.Search(len(opened), func(i int) bool { return !opened[i].Before(at.Add(-14 * 24 * time.Hour)) })
	if last <= first {
		return ""
	}
	for range 8 {
		issue := history.issues[first+rng.IntN(last-first)]
		if issue.closed == nil || issue.closed.After(at) {
			return issue.key
		}
	}
	return ""
}

// orNull is s as a column value, null for the empty string.
func orNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// checkCodeSchema refuses a database that lacks any table of the code survival results: this head's
// load writes them (Spec.NoCode leaves them out), and a database of a base that predates them takes
// the load without.
func checkCodeSchema(ctx context.Context, pool *store.Pool) error {
	held, err := evalfixture.ExistingTables(ctx, pool, codeTables)
	if err != nil {
		return err
	}
	var lacks []string
	for _, table := range codeTables {
		if !slices.Contains(held, table) {
			lacks = append(lacks, table)
		}
	}
	if len(lacks) > 0 {
		return fmt.Errorf("the load writes the code survival results, and the database lacks %s (a database of a base that predates them takes -code=false)", strings.Join(lacks, ", "))
	}
	return nil
}

// writeCode writes INFRA's code groups (a save of its delivery settings' code_groups, which
// writeDelivery has written the settings of) and the code survival results of planCode, in one
// transaction. The save logs a settings change, which the load's consumers are owed none of
// (Load), and is drained once it has committed as every save of settings is.
func writeCode(ctx context.Context, pool *store.Pool, spec Spec, history projectHistory) error {
	plan := planCode(spec, history)
	repo, today := strings.ToLower(Repos[0]), survival.DayOf(spec.Now)
	groups, err := json.Marshal(codeGroups)
	if err != nil {
		return fmt.Errorf("encode the code groups: %w", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the code survival results: %w", err)
	}
	defer tx.Rollback(ctx)
	if saved, err := tx.Exec(ctx, `update project_delivery_settings set code_groups = $2::jsonb where project_key = $1`, Project, groups); err != nil {
		return fmt.Errorf("save the code groups of %s: %w", Project, err)
	} else if saved.RowsAffected() != 1 {
		return fmt.Errorf("save the code groups of %s: it holds no delivery settings", Project)
	}
	days := min(plan.measuredDays+codeTailDays, codeDays)
	for _, copied := range []struct {
		table   string
		columns []string
		source  pgx.CopyFromSource
	}{
		{"code_repos", []string{"repo", "branch", "start_day", "state", "checked_on", "last_measured_on", "retry_at"},
			pgx.CopyFromSlice(1, func(int) ([]any, error) {
				return []any{repo, "main", plan.first, string(codesurvival.RepoReadable), today, today, spec.Now.AddDate(1, 0, 0)}, nil
			})},
		{"code_days", []string{"repo", "day", "commits", "digest", "state", "measured_at", "tried_at"},
			pgx.CopyFromSlice(days, func(i int) ([]any, error) {
				day, fingerprint := plan.first.AddDate(0, 0, i), plan.fingerprints[i]
				if i >= plan.measuredDays {
					return []any{repo, day, int32(fingerprint.Commits), fingerprint.Digest[:], string(survival.DayReplayed), nil, nil}, nil
				}
				measuredAt := survival.WaitsFrom(day)
				return []any{repo, day, int32(fingerprint.Commits), fingerprint.Digest[:], string(survival.DayMeasured), measuredAt, measuredAt}, nil
			})},
		{"code_changes", []string{"repo", "sha", "day", "committed_at", "pr_number", "head_ref", "author", "issue_key", "added", "alive", "removed_by_non_pr"},
			pgx.CopyFromSlice(plan.measured, func(i int) ([]any, error) {
				c := plan.changes[i]
				return []any{repo, c.sha, survival.DayOf(c.committed), c.committed, int32(c.pr), c.headRef, c.author, orNull(c.issue),
					int32(c.added), int32(c.alive), int32(c.removedByNonPR)}, nil
			})},
		{"code_pairs", []string{"repo", "adder_sha", "remover_sha", "remover_pr", "remover_head_ref", "remover_author", "remover_issue_key", "lines", "days_between"},
			pgx.CopyFromSlice(len(plan.pairs), func(i int) ([]any, error) {
				p := plan.pairs[i]
				remover := plan.changes[p.remover]
				return []any{repo, plan.changes[p.adder].sha, remover.sha, int32(remover.pr), remover.headRef, remover.author, orNull(remover.issue),
					int32(p.lines), p.daysBetween}, nil
			})},
	} {
		if err := copyRows(ctx, tx, copied.table, copied.columns, copied.source); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the code survival results: %w", err)
	}
	delivery.DrainAfterCommit(ctx, pool, "evalbench: drain the change rows of the code groups' save")
	return nil
}
