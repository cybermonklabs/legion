package delivery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// factsTarget is one pull request a pass reads the facts of: its ref, under the canonical name,
// and the stored name of its timeline row where it has no results row ("" where it has one).
type factsTarget struct {
	ref      PullRequestRef
	timeline string
}

// factsOutcome is what the read of one target came to (readFactsOf).
type factsOutcome int

const (
	// factsWritten: GitHub's answer is written, or the refresh of a too-large answer stored it.
	factsWritten factsOutcome = iota
	// factsNoAnswer: GitHub gave nothing for the pull request, or the App cannot read its
	// repository. That is no proof that it does not exist.
	factsNoAnswer
	// factsAbsent: GitHub proved that no such pull request exists (pullRequestAbsent).
	factsAbsent
	// factsNotProved: GitHub answered the pull request absent, and gave no proof of it.
	factsNotProved
	// factsNoRowHolds: GitHub's answer holds a value no row holds (errNoRowHolds).
	factsNoRowHolds
)

// readFactsOf reads the facts of targets by the batched query, FactsBatchSize pull requests a
// query (readFactsBatchWithin), and settles each target it reads with what its read came to. It
// groups the targets by installation (passInstallations.repositoryToken of the canonical name,
// githubapp.Client.TokenScope), and reads the groups in parallel and each group's queries in
// sequence, within the installation's budget for the pass (passInstallations.factsBudget). It
// writes each answer under the canonical name and drops the row under ref.StoredUnder
// (upsertHeadUnderCanonicalName). With proveAbsence, an answer of absence goes to
// pullRequestAbsent, which asks GitHub once more with the query's token. A rate limit goes to
// limited and ends the reads of its installation, and the other installations go on. Any other
// failure of GitHub is logged and ends the reads of its installation for this pass. A target of a
// query that failed is not settled. It answers each failure of the database. The attempt of the
// pass notes each read of GitHub that this function logs and gives up (githubreads).
func (r *Reconcile) readFactsOf(
	ctx context.Context, installations *passInstallations, limited *passRateLimit, targets []factsTarget,
	now time.Time, proveAbsence bool, settle func(context.Context, factsTarget, factsOutcome) error,
) error {
	var (
		mu       sync.Mutex
		failures []error
	)
	fail := func(err error) {
		if err != nil {
			mu.Lock()
			failures = append(failures, err)
			mu.Unlock()
		}
	}
	type group struct {
		scope, token string
		targets      []factsTarget
	}
	var groups []*group
	byScope := map[string]*group{}
	for _, target := range targets {
		token, err := installations.repositoryToken(ctx, r.github, target.ref.Owner, target.ref.Repo)
		switch {
		case err == nil:
		case RepositoryUnreadable(err):
			fail(settle(ctx, target, factsNoAnswer))
			continue
		case limited.keep(err):
			continue
		default:
			slog.Warn("dispatch delivery: mint the token of a read of a pull request's facts; the next pass reads it",
				"repo", target.ref.Owner+"/"+target.ref.Repo, "number", target.ref.Number, "error", err)
			githubreads.NoteFailuref(ctx, "read the facts of %s PR #%d: %w", target.ref.Owner+"/"+target.ref.Repo, target.ref.Number, err)
			continue
		}
		scope := r.github.TokenScope(token)
		g, found := byScope[scope]
		if !found {
			g = &group{scope: scope, token: token}
			byScope[scope] = g
			groups = append(groups, g)
		}
		g.targets = append(g.targets, target)
	}
	var wait sync.WaitGroup
	for _, g := range groups {
		budget := installations.factsBudget(g.scope)
		wait.Go(func() {
			for start := 0; start < len(g.targets); start += FactsBatchSize {
				chunk := g.targets[start:min(start+FactsBatchSize, len(g.targets))]
				refs := make([]PullRequestRef, len(chunk))
				for i, target := range chunk {
					refs[i] = target.ref
				}
				answers, err := readFactsBatchWithin(ctx, r.pool, r.github, installations, g.token, refs, now, budget)
				for _, target := range chunk {
					answer, found := answers[target.ref]
					switch {
					case found && answer.Head != nil:
						_, written := upsertHeadUnderCanonicalName(ctx, r.pool, *answer.Head, target.ref.StoredUnder)
						switch {
						case errors.Is(written, errNoRowHolds):
							fail(settle(ctx, target, factsNoRowHolds))
						case written != nil:
							fail(written)
						default:
							fail(settle(ctx, target, factsWritten))
						}
					case found && answer.Stored:
						fail(settle(ctx, target, factsWritten))
					case found && answer.Absent:
						outcome := factsNotProved
						if proveAbsence {
							absent, askErr := pullRequestAbsent(ctx, r.github, &pullRequestRefusal{err: ErrPullRequestNotFound,
								owner: target.ref.Owner, repo: target.ref.Repo, number: target.ref.Number, token: g.token}, nil)
							if askErr != nil && !limited.keep(askErr) {
								slog.Warn("dispatch delivery: ask GitHub whether a repository holds a pull request it answered absent; the next pass reads it again",
									"repo", target.ref.Owner+"/"+target.ref.Repo, "number", target.ref.Number, "error", askErr)
								githubreads.NoteFailuref(ctx, "ask GitHub whether %s holds PR #%d: %w", target.ref.Owner+"/"+target.ref.Repo, target.ref.Number, askErr)
							}
							if absent {
								outcome = factsAbsent
							}
						}
						fail(settle(ctx, target, outcome))
					case !found && err == nil:
						fail(settle(ctx, target, factsNoAnswer))
					}
				}
				if err != nil {
					if !errors.Is(err, errFactsBudgetSpent) && !limited.keep(err) {
						slog.Warn("dispatch delivery: read the facts of pull requests; the next pass reads them again",
							"installation", g.scope, "pull_requests", len(chunk), "error", err)
						first := chunk[0].ref
						githubreads.NoteFailuref(ctx, "read the facts of %d pull requests under %s, the first %s PR #%d: %w",
							len(chunk), g.scope, first.Owner+"/"+first.Repo, first.Number, err)
					}
					return
				}
			}
		})
	}
	wait.Wait()
	return errors.Join(failures...)
}

// stampFactsRefused records that the last read of target's facts was refused: attribution_refused_at
// on each results row of the pull request, under each name the renaming rule reads as its canonical
// name, or facts_refused_at on its timeline row where it has no results row. The line reads a
// refused pull request after every other, the oldest refusal first, and no refusal ends its read.
func (r *Reconcile) stampFactsRefused(ctx context.Context, target factsTarget) error {
	if target.timeline != "" {
		if _, err := r.pool.Exec(ctx, `update delivery_pull_requests set facts_refused_at = clock_timestamp() where repo = $1 and number = $2`,
			target.timeline, target.ref.Number); err != nil {
			return fmt.Errorf("record the refused read of %s#%d: %w", target.timeline, target.ref.Number, err)
		}
		return nil
	}
	return r.stampResultsRows(ctx, target.ref, "attribution_refused_at")
}

// stampResultsRows sets column, attribution_refused_at or attribution_absent_at, to now on each
// results row of ref's pull request, under each name the renaming rule reads as its canonical name.
// Neither column is a fact of the head, so the write owes no read.
func (r *Reconcile) stampResultsRows(ctx context.Context, ref PullRequestRef, column string) error {
	canonical := ref.Owner + "/" + ref.Repo
	if _, err := r.pool.Exec(ctx, `update pull_request_heads set `+column+` = clock_timestamp()
		where number = $2 and lower(`+refs.CanonicalRepoSQL("repo")+`) = lower($1)`, canonical, ref.Number); err != nil {
		return fmt.Errorf("record %s of %s#%d: %w", column, canonical, ref.Number, err)
	}
	return nil
}

// FactsLine is what one facts line counted (readFactsLine): the pull requests with no facts
// before its reads (LeftBefore) and after them (Left), the pull requests a pass still reads after
// them (ReadsLeft), and the part of those whose last read was refused (Refused).
type FactsLine struct{ LeftBefore, Left, ReadsLeft, Refused int }

// factsEntry is one pull request of the facts line, with what the line orders it by.
type factsEntry struct {
	target                                               factsTarget
	hasResults, open, unread, textsCurrent, messagesRead bool
	known, hasFacts, likelyWrong                         bool
	fallback                                             *string
	refusedAt, newest                                    *time.Time
}

// rank is the place of the entry's kind in the line's order: open rows, then rows whose answer is
// not known, then results rows no read of GitHub wrote, then rows whose fallback differs from the
// answer of their title and branch alone, then the rest, then rows whose messages are not read
// though a higher source decides their answer, and last the rows refused before.
func (e factsEntry) rank() int {
	switch {
	case e.refusedAt != nil:
		return 6
	case e.open:
		return 0
	case e.textsCurrent && !e.known:
		return 1
	case e.hasResults && e.unread:
		return 2
	case e.likelyWrong:
		return 3
	case e.textsCurrent && !e.messagesRead:
		return 5
	default:
		return 4
	}
}

// readFactsLine reads the facts of the pull requests a pass still reads, the facts line: each
// results row that is open or merged, that GitHub has not proved absent, and whose texts are not
// current or whose messages are not read, and each timeline row that is not unfetchable and has no
// results row under any name (factsLineJudged). It takes each pull request once, under its
// canonical name, and reads it by the batched query in the line's order (factsEntry.rank), within
// the installation's budget of the pass, which the open arm spent first (readFactsOf). So it reads
// at most FactsQueriesPerPass × FactsBatchSize pull requests of each installation.
//
// It writes each answer under the canonical name and drops the row under an old name. A write
// that checkHeadRow refuses stamps the pull request refused, and the line goes on. An answer of
// absence goes to pullRequestAbsent: its proof sets attribution_absent_at on the results rows, or
// marks the timeline row unfetchable (MarkPullRequestUnfetchable), and no proof stamps it refused.
// GitHub giving nothing for a pull request stamps it refused too. No single refusal ends a read: a
// refused pull request stays in the line, after every other.
func (r *Reconcile) readFactsLine(ctx context.Context, installations *passInstallations, limited *passRateLimit) (FactsLine, error) {
	entries, err := r.factsEntries(ctx)
	if err != nil {
		return FactsLine{}, err
	}
	var line FactsLine
	for _, entry := range entries {
		if !entry.hasFacts {
			line.LeftBefore++
		}
		if entry.refusedAt != nil {
			line.Refused++
		}
	}
	line.Left, line.ReadsLeft = line.LeftBefore, len(entries)
	if len(entries) == 0 {
		return line, nil
	}
	if err := r.markLikelyWrong(ctx, entries); err != nil {
		return line, err
	}
	slices.SortStableFunc(entries, compareFactsEntries)
	targets := make([]factsTarget, len(entries))
	for i, entry := range entries {
		targets[i] = entry.target
	}
	readErr := r.readFactsOf(ctx, installations, limited, targets, time.Now(), true, r.settleFacts)
	counts, err := ReadAttributionCounts(ctx, r.pool)
	if err != nil {
		return line, errors.Join(readErr, err)
	}
	line.Left, line.ReadsLeft, line.Refused = counts.FactsLeft, counts.ReadsLeft, counts.FactsRefused
	return line, readErr
}

// settleFacts acts on what the facts line's read of target came to (readFactsOf).
func (r *Reconcile) settleFacts(ctx context.Context, target factsTarget, outcome factsOutcome) error {
	switch outcome {
	case factsWritten:
		return nil
	case factsAbsent:
		if target.timeline != "" {
			if err := MarkPullRequestUnfetchable(ctx, r.pool, target.timeline, target.ref.Number, "GitHub proved that no such pull request exists"); err != nil {
				return fmt.Errorf("mark %s#%d unfetchable: %w", target.timeline, target.ref.Number, err)
			}
			return nil
		}
		return r.stampResultsRows(ctx, target.ref, "attribution_absent_at")
	default:
		return r.stampFactsRefused(ctx, target)
	}
}

// compareFactsEntries orders the facts line: by rank, the oldest refusal first among the refused,
// and then the newest first, by the time GitHub last updated the pull request and then its number.
func compareFactsEntries(a, b factsEntry) int {
	if c := cmp.Compare(a.rank(), b.rank()); c != 0 {
		return c
	}
	if a.refusedAt != nil && b.refusedAt != nil {
		if c := a.refusedAt.Compare(*b.refusedAt); c != 0 {
			return c
		}
	}
	switch {
	case a.newest != nil && b.newest == nil:
		return -1
	case a.newest == nil && b.newest != nil:
		return 1
	case a.newest != nil && b.newest != nil:
		if c := b.newest.Compare(*a.newest); c != 0 {
			return c
		}
	}
	if c := cmp.Compare(b.target.ref.Number, a.target.ref.Number); c != 0 {
		return c
	}
	return strings.Compare(a.target.ref.Owner+"/"+a.target.ref.Repo, b.target.ref.Owner+"/"+b.target.ref.Repo)
}

// factsEntries reads the entries of the facts line, each under its canonical name, with what the
// line orders it by (attributionStatements.factsLine).
func (r *Reconcile) factsEntries(ctx context.Context) ([]factsEntry, error) {
	statements, err := attribution()
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, statements.factsLine)
	if err != nil {
		return nil, fmt.Errorf("read the facts line: %w", err)
	}
	defer rows.Close()
	var entries []factsEntry
	for rows.Next() {
		var entry factsEntry
		var canonical, storedUnder, timeline string
		if err := rows.Scan(&canonical, &entry.target.ref.Number, &storedUnder, &entry.hasResults, &timeline,
			&entry.open, &entry.unread, &entry.textsCurrent, &entry.messagesRead, &entry.known, &entry.hasFacts,
			&entry.fallback, &entry.refusedAt, &entry.newest); err != nil {
			return nil, fmt.Errorf("read the facts line: %w", err)
		}
		owner, repo, err := refs.SplitGitHubRepo(canonical)
		if err != nil {
			slog.Warn("dispatch delivery: a pull request of the facts line names no repository GitHub can have", "repo", canonical, "number", entry.target.ref.Number, "error", err)
			continue
		}
		entry.target.ref.Owner, entry.target.ref.Repo, entry.target.ref.StoredUnder = owner, repo, storedUnder
		if !entry.hasResults {
			entry.target.timeline = timeline
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the facts line: %w", err)
	}
	return entries, nil
}

// markLikelyWrong marks each entry with a results row and no current texts whose fallback differs
// from the answer of the statement over the keys of its title and branch alone
// (attributionStatements.titleAndBranchAnswers): the rows whose stored issue is most likely wrong,
// which the line reads before the rest.
func (r *Reconcile) markLikelyWrong(ctx context.Context, entries []factsEntry) error {
	var repos []string
	var numbers []int
	for _, entry := range entries {
		if entry.hasResults && !entry.textsCurrent {
			repos, numbers = append(repos, entry.target.ref.Owner+"/"+entry.target.ref.Repo), append(numbers, entry.target.ref.Number)
		}
	}
	if len(repos) == 0 {
		return nil
	}
	statements, err := attribution()
	if err != nil {
		return err
	}
	rows, err := r.pool.Query(ctx, statements.titleAndBranchAnswers, repos, numbers)
	if err != nil {
		return fmt.Errorf("read the answers of the titles and branches of the facts line: %w", err)
	}
	defer rows.Close()
	answers := map[PullRequestKey]*string{}
	for rows.Next() {
		var key PullRequestKey
		var issue *string
		if err := rows.Scan(&key.Repo, &key.Number, &issue); err != nil {
			return fmt.Errorf("read the answers of the titles and branches of the facts line: %w", err)
		}
		answers[key] = issue
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the answers of the titles and branches of the facts line: %w", err)
	}
	for i := range entries {
		entry := &entries[i]
		guess, found := answers[PullRequestKey{Repo: entry.target.ref.Owner + "/" + entry.target.ref.Repo, Number: entry.target.ref.Number}]
		entry.likelyWrong = found && !equalIssue(guess, entry.fallback)
	}
	return nil
}
