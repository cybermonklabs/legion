package api

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

// results_merge.go holds what one results read knows of the merges of the pull requests it
// orders: whether a set of merged pull request rows can be ordered by when each merged
// (fillMergeTimes), over the read's memo (mergeTimeFills).

// mergeTimeFills is one results read's memo of the merge times it learned (fillMergeTimes): the
// comparisonReader holds it (comparisonReader.fills), and deriveCIResults and the comparison after
// it both fill through that reader's, so the read learns no merge time twice. learned holds, by
// repository and number, each pull request the read asked GitHub about and the time it learned
// there, nil where it learned none. readOnly and answers are the reader's
// (comparisonReader.readOnly, comparisonReader.answers), which newComparisonReader sets: a
// read-only caller's refresh stores nothing and adds what it read to answers (refreshMerge).
type mergeTimeFills struct {
	mu       sync.Mutex
	learned  map[string]*time.Time
	readOnly bool
	answers  *delivery.HeadAnswers
}

// fillMergeTimes reports whether rows, the merged pull request rows one read orders by when each
// merged, can be ordered (ordered): a set of fewer than two rows needs no order, and a set holding
// a row with no time of merge cannot be ordered, since the row ordered last may have merged last.
// So no read names an older merge as the newest. Where it learns a row's time, it sets that row's
// MergedAt in rows, so the caller orders by it.
//
// Before it answers, it learns the time of each row of a set of two or more that holds none and
// that the running rule has not judged yet (an AttributionRule below
// delivery.AttributionRuleVersion): a row stored before pull_request_heads.merged_at, or written
// from the envelope of a listener older than that field. It learns it once per read of the results
// (refreshMerge), so the CI read and the comparison after it ask GitHub once between them. A
// writer's read makes the reconcile's own refresh of a row, which stores GitHub's time of the
// merge and stamps the row with the running rule whatever GitHub answered, so no later read asks
// again: a pull request GitHub no longer holds, or one of a repository the App cannot read, keeps
// no time, and its set stays one that cannot be ordered, as does a set whose read failed now. The
// reconcile's rule arm offers every other such row once (delivery.ListStalePullRequestHeads). A
// read-only caller's read asks GitHub the same and stores nothing (refreshMerge).
func (s *server) fillMergeTimes(ctx context.Context, fills *mergeTimeFills, rows []delivery.MergeTime) (ordered bool) {
	if len(rows) < 2 {
		return true
	}
	ordered = true
	for index := range rows {
		row := &rows[index]
		if row.MergedAt == nil && row.AttributionRule < delivery.AttributionRuleVersion {
			row.MergedAt = s.refreshMerge(ctx, fills, row.Repo, row.Number)
		}
		if row.MergedAt == nil {
			ordered = false
		}
	}
	return ordered
}

// refreshMerge is when pull request number of repo merged, as the row holds it after one refresh
// from GitHub in this read of the results, read back by its number
// (delivery.Attributor.StoredMergeTime) through the request's answers, since the refresh answers
// only whether it failed: nil where the row holds no time of its merge, a refresh or a read that
// failed included, each logged. A refresh that failed leaves the row as it was. fills keeps each
// answer, so one read of the results asks GitHub about a pull request once. A deployment that
// holds no App key refreshes nothing.
//
// A writer's read refreshes the row and stores it (delivery.Attributor.RefreshPullRequestHead). A
// read-only caller's read stores nothing: it reads the pull request exactly as the refresh would
// (delivery.Attributor.ReadPullRequestHead), a stamp included, and adds the answer to its
// request's answers (fills.answers), so the read back and every later read of the heads read the
// row as a writer's read reads the row its refresh stored.
func (s *server) refreshMerge(ctx context.Context, fills *mergeTimeFills, repo string, number int) *time.Time {
	if s.deps.GitHub == nil {
		return nil
	}
	key := repo + "#" + strconv.Itoa(number)
	fills.mu.Lock()
	defer fills.mu.Unlock()
	if learned, asked := fills.learned[key]; asked {
		return learned
	}
	if fills.learned == nil {
		fills.learned = map[string]*time.Time{}
	}
	var answers delivery.HeadAnswers
	if fills.answers != nil {
		answers = *fills.answers
	}
	var refreshErr error
	if fills.readOnly {
		var pulls delivery.CommitPulls
		pulls, refreshErr = s.deps.Attributor.ReadPullRequestHead(ctx, s.deps.Store.Pool, s.deps.GitHub, repo, number, s.resultsNow(), answers)
		fills.answers.Add(pulls)
		answers = *fills.answers
	} else {
		refreshErr = s.deps.Attributor.RefreshPullRequestHead(ctx, s.deps.Store.Pool, s.deps.GitHub, s.deps.Events, repo, number, s.resultsNow())
	}
	if refreshErr != nil {
		slog.Warn("dispatch: refresh a merged pull request a results read orders", "repo", repo, "number", number, "error", refreshErr)
	}
	learned, err := s.deps.Attributor.StoredMergeTime(ctx, s.deps.Store.Pool, repo, number, answers)
	if err != nil {
		slog.Warn("dispatch: read a refreshed merged pull request", "repo", repo, "number", number, "error", err)
	}
	fills.learned[key] = learned
	return learned
}
