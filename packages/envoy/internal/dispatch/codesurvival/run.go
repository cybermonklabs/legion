package codesurvival

// run.go is the daily run of dispatch://INFRA-3206 (plan, "The run"): for each repository that is
// some project's default repository, once a UTC day, it reads the branch's first-parent chain from
// GitHub, marks the measured days a force-push touched, picks the days to measure, reads the pull
// requests and the patches of their replay range, measures them (survival.Measure) and stores the
// results and the two code evals' day rows in one transaction. It keeps no patch (R3), and it
// stops, writing no result, when GitHub's budget falls below a quarter of its limit (R12).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Tick is the period of the run's leased loop (duty.RunLeased): a tick does a repository's daily
// pass where its last finished on an earlier UTC day and no budget stop asks it to wait, so a run
// stopped at the budget floor goes again within the hour after GitHub's window resets.
const Tick = time.Hour

// Run is the daily run: Pass is the pass of one tick. Pool is Dispatch's database, Client
// Dispatch's GitHub App, and Attributor the boot's one attribution, which gives each change its
// issue (R5). Build is the running build, which every day row it writes records (evals). Now is
// the run's clock, time.Now where nil, which decides the day of a pass and the days that wait.
// GitHubReads records each pass as the newest attempt of githubreads.CodeSurvival, and Logger is
// where each repository's line goes, slog.Default() where nil.
type Run struct {
	Pool        *store.Pool
	Client      *githubapp.Client
	Attributor  *delivery.Attributor
	Build       string
	Now         func() time.Time
	GitHubReads githubreads.Recorder
	Logger      *slog.Logger

	// workers and buffer are the stream's shape, streamWorkers and streamBuffer where zero.
	workers int
	buffer  int64
	// afterRows runs, where set, between the results transaction's row locks and its series
	// writes (prepare).
	afterRows func()
}

// now is the run's clock.
func (r *Run) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// logger is where the run logs.
func (r *Run) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// The ways a repository's pass stops, as its line names them.
const (
	stoppedNone      = "none"
	stoppedBudget    = "budget"
	stoppedRateLimit = "rate_limit"
	stoppedFailure   = "failure"
)

// budgetStop is a pass stopped at the budget floor (R12): the budget the answers named, and the
// requests the pass still had to make, where it stopped before reading them.
type budgetStop struct {
	budget   Budget
	estimate int
}

func (e *budgetStop) Error() string {
	if e.estimate > 0 {
		return fmt.Sprintf("GitHub's remaining budget of %d less the %d requests the run would make falls below a quarter of the limit of %d", e.budget.Remaining, e.estimate, e.budget.Limit)
	}
	return fmt.Sprintf("GitHub's remaining budget of %d fell below a quarter of the limit of %d", e.budget.Remaining, e.budget.Limit)
}

// belowFloor reports whether budget, less the requests still to make, falls below a quarter of its
// limit. A budget no answer named is never below it.
func belowFloor(budget Budget, requests int) bool {
	return budget.Limit > 0 && 4*(budget.Remaining-requests) < budget.Limit
}

// Pass is one tick: the daily pass of each repository that is some project's default repository
// and is due one (passRepo), one repository after the other. Each pass is one attempt of the
// GitHub read record (githubreads), recorded through GitHubReads when it ends, so GET /healthz
// names its outcome.
func (r *Run) Pass(caller context.Context) {
	ctx, attempt := githubreads.Begin(caller)
	defer githubreads.Finish(caller, githubreads.CodeSurvival, attempt, r.GitHubReads, r.Logger)
	repos, err := defaultRepos(ctx, r.Pool)
	if err != nil {
		if ctx.Err() == nil {
			r.logger().Warn("codesurvival: read the repositories to measure", "error", err)
		}
		return
	}
	for _, repo := range repos {
		if ctx.Err() != nil {
			return
		}
		r.passRepo(ctx, repo)
	}
}

// defaultRepos is every distinct projects.default_repo, in lower case, sorted.
func defaultRepos(ctx context.Context, db Querier) ([]string, error) {
	rows, err := db.Query(ctx, `
		select distinct lower(default_repo) from projects where default_repo is not null and default_repo <> '' order by 1`)
	if err != nil {
		return nil, fmt.Errorf("read the default repositories: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// runLine is what a repository's pass logs (plan, "The log line").
type runLine struct {
	daysMeasured, daysMarked int
	peakPatchBytes           int64
	peakLines                int
	stopped                  string
}

// passRepo is repo's daily pass, where one is due: its last pass finished on an earlier UTC day
// than today, and no budget stop asks it to wait past now. It logs one line, and records how the
// pass ended in code_repos: a budget stop or a rate limit sets the moment it may go again the same
// day (retry_at); a failure, or a repository the App cannot read, waits for the next daily pass.
func (r *Run) passRepo(ctx context.Context, repo string) {
	now := r.now()
	today := survival.DayOf(now)
	stored, found, err := GetRepo(ctx, r.Pool, repo)
	if err != nil {
		r.logger().Warn("codesurvival: read the state of a repository", "repo", repo, "error", err)
		return
	}
	if found && (!stored.CheckedOn.Before(today) || (!stored.RetryAt.IsZero() && now.Before(stored.RetryAt))) {
		return
	}
	if !found {
		stored = Repo{Repo: repo, State: RepoReadable}
	}
	started := time.Now()
	gh := NewGitHub(r.Client, r.Pool, r.Attributor)
	var line runLine
	err = r.daily(ctx, gh, stored, now, &line)
	switch limited, rateLimited := githubapp.AsRateLimit(err); {
	case err == nil:
		line.stopped = stoppedNone
	case ctx.Err() != nil:
		// This process's shutdown cut the pass short: it read nothing it can keep, and the next
		// process's tick does the pass again.
		return
	case errors.As(err, new(*budgetStop)):
		line.stopped = stoppedBudget
		retry := gh.Budget().Reset
		if !retry.After(now) {
			retry = now.Add(Tick)
		}
		r.stopRepo(ctx, repo, func(row *Repo) { row.RetryAt = retry })
		githubreads.NoteFailuref(ctx, "the code survival run of %s stopped at the budget floor: %v", repo, err)
	case rateLimited:
		line.stopped = stoppedRateLimit
		wait := limited.Wait
		if wait <= 0 {
			wait = Tick
		}
		r.stopRepo(ctx, repo, func(row *Repo) { row.RetryAt = now.Add(wait) })
	default:
		line.stopped = stoppedFailure
		r.stopRepo(ctx, repo, func(row *Repo) { row.CheckedOn, row.RetryAt = today, time.Time{} })
		githubreads.NoteFailure(ctx, fmt.Errorf("the code survival run of %s: %w", repo, err))
		r.logger().Warn("codesurvival: a repository's run failed; it is tried again at the next daily pass", "repo", repo, "error", err)
	}
	counts, budget := gh.Counts(), gh.Budget()
	r.logger().Info("codesurvival: run",
		"repo", repo,
		"days_measured", line.daysMeasured,
		"days_marked", line.daysMarked,
		"patches_diff", counts.Diff,
		"patches_compare", counts.Compare,
		"patches_files", counts.Files,
		"requests", counts.Requests(),
		"bytes", counts.Bytes,
		"seconds", time.Since(started).Seconds(),
		"peak_patch_bytes", line.peakPatchBytes,
		"peak_lines", line.peakLines,
		"requests_attribution", counts.Attribution,
		"limit", budget.Limit,
		"remaining", budget.Remaining,
		"stopped", line.stopped)
}

// stopRepo writes, as change says, repo's row of code_repos after a pass that stored no result.
func (r *Run) stopRepo(ctx context.Context, repo string, change func(*Repo)) {
	row, found, err := GetRepo(ctx, r.Pool, repo)
	if err == nil {
		if !found {
			row = Repo{Repo: repo, State: RepoReadable}
		}
		change(&row)
		err = PutRepo(ctx, r.Pool, row)
	}
	if err != nil && ctx.Err() == nil {
		r.logger().Warn("codesurvival: write the state of a repository", "repo", repo, "error", err)
	}
}

// daily is the daily pass of stored.Repo, as of now (plan, "A daily pass of one repository"):
//
//  1. The repository, its default branch and that branch's tip (Open). One the App cannot read is
//     stored unreadable with its reason, and its projects' day rows say so (repo_unreadable).
//  2. The first-parent chain from the tip back to the oldest day holding a fingerprint, or the
//     start day, less three days (Chain, ChainSince). The first pass sets the start day
//     (survival.StartDay), no earlier than the day of the branch's first commit where the chain
//     reaches it: no day before a repository's first commit waits to be measured.
//  3. Each stored day's fingerprint compared with the chain, and the measured days a changed day
//     touches marked, in a transaction of their own before anything else (WriteMarks); then the
//     changed days' fingerprints are those of the chain (RefreshFingerprints), so a later pass
//     marks no day for a change it has already marked.
//  4. The days to measure (survival.SelectDays); with none, the pass ends.
//  5. The budget: the requests the rest of the pass will make, counted before any is sent.
//  6. The merged pull requests of the replay range and each change's issue (PullRequests,
//     Changes).
//  7. The patches of the range, streamed in chain order into survival.Measure.
//  8. One transaction: the results, the day rows of every project they count for, and the
//     repository's row (store).
func (r *Run) daily(ctx context.Context, gh *GitHub, stored Repo, now time.Time, line *runLine) error {
	repo, today := stored.Repo, survival.DayOf(now)
	opened, err := gh.Open(ctx, repo)
	if reason, unreadable := Unreadable(err); unreadable {
		was := stored.State
		stored.State, stored.Reason, stored.CheckedOn, stored.RetryAt = RepoUnreadable, reason, today, time.Time{}
		if err := PutRepo(ctx, r.Pool, stored); err != nil {
			return err
		}
		if was != RepoUnreadable {
			if err := r.rebuildProjectsOf(ctx, repo, now); err != nil {
				return err
			}
		}
		return fmt.Errorf("the GitHub App cannot read %s: %s", repo, reason)
	}
	if err != nil {
		return err
	}
	was := stored.State
	if stored.StartDay.IsZero() {
		stored.StartDay = survival.StartDay(now)
	}
	stored.Branch, stored.State, stored.Reason = opened.Branch, RepoReadable, ""
	if err := PutRepo(ctx, r.Pool, stored); err != nil {
		return err
	}
	if was == RepoUnreadable {
		if err := r.rebuildProjectsOf(ctx, repo, now); err != nil {
			return err
		}
	}

	fingerprints, err := FingerprintsFrom(ctx, r.Pool, repo, time.Time{})
	if err != nil {
		return err
	}
	oldest := stored.StartDay
	if len(fingerprints) > 0 && fingerprints[0].Day.Before(oldest) {
		oldest = fingerprints[0].Day
	}
	chain, err := gh.Chain(ctx, opened, ChainSince(oldest))
	if err != nil {
		return err
	}
	if root := survival.DayOf(chainRoot(chain)); len(fingerprints) == 0 && root.After(stored.StartDay) {
		// No day was replayed yet, so no day before the branch's first commit waits.
		stored.StartDay = root
		if err := PutRepo(ctx, r.Pool, stored); err != nil {
			return err
		}
	}

	if changed := survival.ChangedDays(fingerprints, chain); len(changed) > 0 {
		statuses, err := DayStatuses(ctx, r.Pool, repo, fingerprints[0].Day, fingerprints[len(fingerprints)-1].Day)
		if err != nil {
			return err
		}
		var measured []time.Time
		for _, status := range statuses {
			if status.State == survival.DayMeasured {
				measured = append(measured, status.Day)
			}
		}
		if marks := survival.MarkedDays(changed, measured); len(marks) > 0 {
			marked, err := WriteMarks(ctx, r.Pool, repo, marks, now, r.Build)
			if err != nil {
				return err
			}
			line.daysMarked = len(marked)
		}
		fresh := make([]survival.Fingerprint, len(changed))
		for index, day := range changed {
			fresh[index] = survival.Fingerprints(chain, day, day)[0]
		}
		if err := RefreshFingerprints(ctx, r.Pool, repo, fresh); err != nil {
			return err
		}
	}

	statuses, err := DayStatuses(ctx, r.Pool, repo, stored.StartDay, today)
	if err != nil {
		return err
	}
	days := survival.SelectDays(survival.Selection{Now: now, StartDay: stored.StartDay, Days: statuses})
	if len(days) == 0 {
		stored.CheckedOn, stored.RetryAt = today, time.Time{}
		return PutRepo(ctx, r.Pool, stored)
	}
	replayed := survival.ReplayRange(chain, days[0], days[len(days)-1])

	estimate, err := r.estimate(ctx, gh, opened, chain, replayed, days[0])
	if err != nil {
		return err
	}
	if budget := gh.Budget(); belowFloor(budget, estimate) {
		return &budgetStop{budget: budget, estimate: estimate}
	}
	pulls, err := gh.PullRequests(ctx, opened, days[0], replayed)
	if err != nil {
		return err
	}
	if err := gh.Changes(ctx, opened, replayed, pulls); err != nil {
		return err
	}
	if budget := gh.Budget(); belowFloor(budget, 0) {
		return &budgetStop{budget: budget}
	}

	shas := make([]string, len(replayed))
	for index, commit := range replayed {
		shas[index] = commit.SHA
	}
	workers, buffer := r.workers, r.buffer
	if workers == 0 {
		workers = streamWorkers
	}
	if buffer == 0 {
		buffer = streamBuffer
	}
	patches := newStream(ctx, len(replayed), workers, buffer, shas, func(ctx context.Context, index int) (PatchRead, error) {
		read, err := gh.Patch(ctx, opened, replayed[index])
		if err == nil {
			if budget := gh.Budget(); belowFloor(budget, 0) {
				return PatchRead{}, &budgetStop{budget: budget}
			}
		}
		return read, err
	})
	out, err := survival.Measure(survival.MeasureInput{Days: days, Chain: replayed, Patch: patches.patch})
	cause := patches.finish()
	line.peakPatchBytes = patches.peakBytes()
	switch {
	case cause != nil:
		return cause
	case err != nil:
		return err
	}
	line.peakLines = out.PeakLines
	if len(out.Fingerprints) > 0 {
		// The fingerprints of the days the replay read, from the whole chain the pass read, as the
		// next pass's ChangedDays recomputes them: a commit of one of those days that a commit
		// dated after the range comes before in chain order is no commit of the range, and is still
		// one of its day.
		out.Fingerprints = survival.Fingerprints(chain, out.Fingerprints[0].Day, out.Fingerprints[len(out.Fingerprints)-1].Day)
	}
	if err := r.store(ctx, stored, days, out, patches.reasons, now); err != nil {
		return err
	}
	for _, result := range out.Days {
		if result.State == survival.DayMeasured {
			line.daysMeasured++
		}
	}
	return nil
}

// chainRoot is the committer time of chain's first commit where it is the branch's root, a commit
// with no parent, and the zero time where the chain begins at a later commit.
func chainRoot(chain []survival.Commit) time.Time {
	if len(chain) == 0 || len(chain[0].Parents) > 0 {
		return time.Time{}
	}
	return chain[0].CommittedAt
}

// estimate is the requests a pass will make after its chain read, counted before it makes any
// (R12): the pages of the closed pull requests list, one per 100 of the chain's commits dated on
// or after d0, the oldest selected day, and one more; one for each commit of the range, its patch;
// and one for each commit of the range whose pull request attribution has not judged under its
// rule, its first page of commit messages, where every commit of the range whose merged pull
// request Dispatch has not stored counts as one. The list's pages are an estimate and no bound:
// a commit of the chain landed one merged pull request at most, but the list holds every closed
// pull request updated on or after d0, of every base, and so also those that never merged and
// those merged into a branch that has not landed, which no read before the list counts. A list
// longer than the estimate, and a pull request over 100 commits, which takes more pages of
// commit messages, are met by the budget check after the list (daily), and the counts the pass
// logs hold them.
func (r *Run) estimate(ctx context.Context, gh *GitHub, opened Repository, chain, replayed []survival.Commit, d0 time.Time) (int, error) {
	since := 0
	for _, commit := range chain {
		if !commit.CommittedAt.Before(d0) {
			since++
		}
	}
	shas := make([]string, len(replayed))
	for index, commit := range replayed {
		shas[index] = commit.SHA
	}
	rows, err := r.Pool.Query(ctx, `
		select distinct number from delivery_pull_requests
		where lower(repo) = lower($1) and merge_commit_sha = any($2::text[]) and merged_at is not null`, opened.Repo, shas)
	if err != nil {
		return 0, fmt.Errorf("read the stored pull requests of %s: %w", opened.Repo, err)
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return 0, fmt.Errorf("read the stored pull requests of %s: %w", opened.Repo, err)
	}
	judged, err := r.Attributor.JudgedIssues(ctx, r.Pool, opened.Repo, numbers)
	if err != nil {
		return 0, err
	}
	return since/perPage + 1 + len(replayed) + len(replayed) - len(judged), nil
}

// store writes a measurement in one transaction (plan, "A daily pass of one repository", step 6):
// the rows of the selected days in code_changes, code_pairs and code_days replaced, the replayed
// days' fingerprints, the marks of the measured days cleared and the blocked days' reasons
// (ReplaceDays); the repository's row, measured and checked today; and the day rows of every
// project the results count for, before the replace and after it, rebuilt for those days from the
// stored results read back in the transaction, with their group series and their groups digest
// (prepare, builder.write). The outbox is drained once it has committed.
func (r *Run) store(ctx context.Context, stored Repo, days []time.Time, out survival.MeasureOutput, reasons map[string]string, now time.Time) error {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the results of %s: %w", stored.Repo, err)
	}
	defer tx.Rollback(ctx)
	before, err := projectsOfRepo(ctx, tx, stored.Repo, days)
	if err != nil {
		return err
	}
	after, err := projectsOfIssues(ctx, tx, out.Changes)
	if err != nil {
		return err
	}
	b, _, err := prepare(ctx, tx, union(before, after), r.Build, now, r.afterRows)
	if err != nil {
		return err
	}
	if err := ReplaceDays(ctx, tx, stored.Repo, days, out, now, reasons); err != nil {
		return err
	}
	today := survival.DayOf(now)
	stored.LastMeasuredOn, stored.CheckedOn, stored.RetryAt = today, today, time.Time{}
	if err := PutRepo(ctx, tx, stored); err != nil {
		return err
	}
	if err := b.write(ctx, days); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the results of %s: %w", stored.Repo, err)
	}
	delivery.DrainAfterCommit(ctx, r.Pool, "codesurvival: the run's day rows could not move their changes into the delivery change log; the next drain moves them")
	return nil
}

// rebuildProjectsOf rebuilds every day row of each project whose default repository repo is, in
// one transaction: what a repository that turned unreadable, or readable again, does to the rows
// of its projects, which repo_unreadable marks while the App cannot read it.
func (r *Run) rebuildProjectsOf(ctx context.Context, repo string, now time.Time) error {
	rows, err := r.Pool.Query(ctx, `select key from projects where lower(default_repo) = $1 order by key`, strings.ToLower(repo))
	if err != nil {
		return fmt.Errorf("read the projects of %s: %w", repo, err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the projects of %s: %w", repo, err)
	}
	return rebuild(ctx, r.Pool, keys, r.Build, now)
}

// rebuild rebuilds every day row of each of keys from the stored results in one transaction, and
// drains the outbox once it has committed; it answers no error for a project the seed has not
// written the code evals of, whose rows a regroup builds later.
func rebuild(ctx context.Context, pool *store.Pool, keys []string, build string, now time.Time) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the rebuild of %v: %w", keys, err)
	}
	defer tx.Rollback(ctx)
	b, _, err := prepare(ctx, tx, keys, build, now, nil)
	if err != nil {
		return err
	}
	if err := b.write(ctx, nil); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the rebuild of %v: %w", keys, err)
	}
	delivery.DrainAfterCommit(ctx, pool, "codesurvival: a rebuild's day rows could not move their changes into the delivery change log; the next drain moves them")
	return nil
}
