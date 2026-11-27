package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The bounds of the merge read: one bounded pass of GETs, resumed where it stopped.
const (
	// mergeReadPassGETs is the GET attempts one pass spends across every pending merge read: a
	// page, a deferred redirect's GET and a retry each spend one.
	mergeReadPassGETs = 8
	// mergeReadPass is how long a pass lasts: a read that finds the pass spent waits for the next.
	mergeReadPass = time.Minute
	// mergeReadTurnGap is when a read whose GET left work runs its next turn: each turn sends at
	// most one GET, saves what it answered, and returns.
	mergeReadTurnGap = time.Second
	// mergeReadSlotWait is when a read that found the database-wide slot held tries again. The wait
	// spends no GET and no attempt.
	mergeReadSlotWait = time.Second
	// mergeReadStageBound is each request's deadline, the Dispatch client's own bound.
	mergeReadStageBound = 10 * time.Second
	// mergeReadAttempts is how many times a stage is tried: a third failure stays unread, with no
	// automatic fourth attempt.
	mergeReadAttempts = 3
	// mergeReadParked is when an unread or stopped read looks at its row again. The look sends
	// nothing unless a door asked a links refresh (record.MergeReadProgress.Refresh), and a door
	// that asks makes the row due at once.
	mergeReadParked = 24 * time.Hour
)

// mergeReadRetryWaits are the waits before a failed stage's second and third attempts.
var mergeReadRetryWaits = [mergeReadAttempts - 1]time.Duration{time.Minute, 5 * time.Minute}

// The stages, states and sources of a merge read, as its progress stores them
// (record.MergeReadProgress).
const (
	mergeStageLinks      = "links"
	mergeStageResults    = "results"
	mergeStageBases      = "bases"
	mergeStageCandidates = "candidates"
	mergeStageMetadata   = "metadata"

	mergeStateUnread  = "unread"
	mergeStateStopped = "stopped"

	mergeSourceLink       = "link"
	mergeSourceCleared    = "cleared"
	mergeSourceRetained   = "retained"
	mergeSourceProduction = "production"
	mergeSourceResults    = "results"
)

// mergeReadPassCount is the pass every merge read of one daemon spends its GETs from: when the
// pass began, and how many GET attempts it has spent.
type mergeReadPassCount struct {
	mu    sync.Mutex
	start time.Time
	spent int
}

// take spends one GET attempt of the pass at now, beginning a new pass where the last one ended,
// and answers false with the start of the next pass where this one is spent.
func (p *mergeReadPassCount) take(now time.Time) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.start.IsZero() || !now.Before(p.start.Add(mergeReadPass)) {
		p.start, p.spent = now, 0
	}
	if p.spent >= mergeReadPassGETs {
		return p.start.Add(mergeReadPass), false
	}
	p.spent++
	return time.Time{}, true
}

// mergeRequest is the one GET a merge read's turn makes: its stage, and what it reads. Base and
// Candidate index the progress's bases and candidates.
type mergeRequest struct {
	stage     string
	base      int
	candidate int
}

// mergeReadAnswer is what one GET answered: the issue's links, its results, a page of pull
// requests a commit is in, or one pull request.
type mergeReadAnswer struct {
	links   []dispatch.IssueLink
	results dispatch.IssueResults
	page    githubrest.Page[mergeCommitPull]
	pull    githubPullRequest
}

// mergeCommitPull is one pull request GitHub lists for a commit (GET
// /repos/{repo}/commits/{sha}/pulls): its number is all the read takes.
type mergeCommitPull struct {
	Number int `json:"number"`
}

// mergeReader makes a merge read's requests: of Dispatch, the issue and its results, and of
// GitHub, through a client of each repository the read names. The daemon's outbox and the
// operator's census (`legion merge-history-report --read-candidates`) read through it alike.
type mergeReader struct {
	dispatch dispatch.Client
	github   func(ctx context.Context, repository ghrepo.Repository) (githubrest.Client, error)
}

// mergeRead runs one turn of the merge read of the row's issue (record.MergeRead): it sends at
// most one request, saves the progress it made (record.MergeReadProgress) under the row's lease,
// and returns. Its candidates are the issue's cleared clocks, its retained pull request and the
// pull request of its production check, the pull requests its external links name, and those
// GitHub lists for each merge commit Dispatch's results compare a check with. Each candidate is read
// from GitHub. The newest merged candidate that names no other issue answers the read, and the turn
// carries it back as one intake.MergeRead fact of the row's issue and generation, and the row
// finishes. A search that ends with no answering candidate posts nothing: an unread merge is no
// merge. It saves its progress as unread, and the row waits for a door's refresh.
//
// Every request takes the one database-wide slot (store.MergeReadSlot) and one attempt of the
// daemon's pass (mergeReadPassCount). The attempt is reserved in the row's progress, committed on
// the slot's connection, before the request is sent, so a crash cannot give a spent attempt back:
// the next turn finds the reservation and counts it uncertain. The slot is given back before the
// answer is applied. A failed request is tried at most three times, a minute and then five
// minutes apart, or later where GitHub's rate limit asks.
func (r *outbox) mergeRead(ctx context.Context, row record.OutboxRow, payload record.MergeRead) error {
	if r.dispatch == nil {
		return errors.New("merge read executor has no Dispatch client")
	}
	now := r.now().UTC()
	var issue *record.Issue
	var history record.MergeHistoryRow
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if issue, err = r.records.Issue(ctx, tx, row.Issue); err != nil || issue == nil {
			return err
		}
		history, err = r.records.MergeHistoryOf(ctx, tx, row.Issue)
		return err
	}); err != nil {
		return fmt.Errorf("read the record of %s for its merge read: %w", row.Issue, err)
	}
	if issue == nil || issue.Generation != payload.Generation {
		r.log.Info("outbox merge read finished without reading: its issue's generation moved", "row", row.ID, "issue", row.Issue,
			"generation", payload.Generation)
		return nil
	}
	recorded, err := issue.Repository()
	if err != nil {
		return fmt.Errorf("read the repository of %s for its merge read: %w", row.Issue, err)
	}
	reader := mergeReader{dispatch: r.dispatch, github: func(ctx context.Context, repository ghrepo.Repository) (githubrest.Client, error) {
		return implementClient(ctx, r.tokens, r.githubAPI, r.repositories.serving(repository))
	}}
	progress := payload.Progress
	if progress.Reserved {
		// An attempt whose answer no turn saved: it was sent, or may have been. It stays spent.
		progress = mergeReadFailed(progress, errors.New("a request whose answer was never saved: the daemon stopped after it was reserved"), now, true)
		return r.saveMergeRead(ctx, row, progress, false, now)
	}
	request, merge, done := nextMergeRequest(payload.Issue, recorded, history.Evidence, &progress)
	if done {
		if merge.Merged() {
			return r.answerMergeRead(ctx, row, payload, merge, progress, now)
		}
		return r.saveMergeRead(ctx, row, progress, false, now)
	}
	if !progress.RetryAt.IsZero() && progress.RetryAt.After(now) {
		return wait.Until(progress.RetryAt, fmt.Sprintf("the merge read of %s tries its stage again at %s", row.Issue, progress.RetryAt.Format(time.RFC3339)))
	}
	slot, taken, err := r.pool.TakeMergeReadSlot(ctx)
	if err != nil {
		return err
	}
	if !taken {
		return wait.Until(now.Add(mergeReadSlotWait), fmt.Sprintf("the merge read of %s waits for the database's one merge read slot", row.Issue))
	}
	// The pass is read and spent under the slot, which every request of this daemon takes, so two
	// lanes never both spend its last attempt.
	if next, ok := r.mergeReads.peek(now); !ok {
		slot.Release(ctx)
		return wait.Until(next, fmt.Sprintf("the merge read of %s waits for the next pass: this one spent its %d GETs", row.Issue, mergeReadPassGETs))
	}
	consumed := progress.Refresh && request.stage == mergeStageLinks
	progress.Attempts++
	progress.Reserved = true
	reserved, err := checkpointMergeRead(ctx, slot.Transact, row, progress, false)
	if err != nil || !reserved {
		slot.Release(ctx)
		if err == nil {
			err = fmt.Errorf("reserve an attempt of the merge read of %s: %w", row.Issue, record.ErrOutboxLeaseLost)
		}
		return err
	}
	r.mergeReads.spend(now)
	sending, cancel := context.WithTimeout(ctx, mergeReadStageBound)
	answer, sendErr := reader.send(sending, payload.Issue, progress, request)
	cancel()
	slot.Release(ctx)
	progress.Reserved = false
	if sendErr != nil {
		progress = mergeReadFailed(progress, sendErr, now, false)
		// A refresh whose read failed stays asked, and is tried again as its stage is.
		return errors.Join(r.saveMergeRead(ctx, row, progress, false, now), mergeReadLimit(sendErr))
	}
	progress = applyMergeAnswer(payload.Issue, recorded, progress, request, answer)
	if _, merge, done := nextMergeRequest(payload.Issue, recorded, history.Evidence, &progress); done && merge.Merged() {
		return r.answerMergeRead(ctx, row, payload, merge, progress, now)
	}
	return r.saveMergeRead(ctx, row, progress, consumed, now)
}

// peek says whether the pass has an attempt left at now, without spending it, and else when the
// next pass begins.
func (p *mergeReadPassCount) peek(now time.Time) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.start.IsZero() || !now.Before(p.start.Add(mergeReadPass)) {
		return time.Time{}, true
	}
	if p.spent >= mergeReadPassGETs {
		return p.start.Add(mergeReadPass), false
	}
	return time.Time{}, true
}

// spend spends the attempt peek found: one GET of the pass.
func (p *mergeReadPassCount) spend(now time.Time) { p.take(now) }

// mergeReadLimit is err where it is GitHub's rate limit, so the outbox holds every row that spends
// that budget until the limit ends (noteLimit), and nil otherwise.
func mergeReadLimit(err error) error {
	if after, _ := githubrest.RetryAfter(err); after > 0 {
		return err
	}
	return nil
}

// answerMergeRead carries the newest merge the read found back as one intake.MergeRead fact of the
// row's issue and generation, and the row finishes: the workflow applies it under the fact lock.
// A fact that does not apply leaves the progress saved, and the row runs again.
func (r *outbox) answerMergeRead(ctx context.Context, row record.OutboxRow, payload record.MergeRead, merge record.Merge, progress record.MergeReadProgress, now time.Time) error {
	fact := intake.MergeRead{Issue: payload.Issue, Generation: payload.Generation, Merge: merge}
	if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("merge-read:%s:%d:%d", row.Issue, row.ID, progress.Attempts), fact, r.handlers...); err != nil {
		return errors.Join(fmt.Errorf("apply the merge read of %s: %w", row.Issue, err), r.saveMergeRead(ctx, row, progress, false, now))
	}
	r.log.Info("outbox merge read found the issue's merge", "row", row.ID, "issue", row.Issue, "repo", merge.Repo, "number", merge.Number,
		"attempts", progress.Attempts)
	return nil
}

// saveMergeRead saves progress under the row's lease and answers when the row runs next: at once
// after a turn that left work, when its stage's retry is due, and a day on for a read that is
// unread or stopped.
func (r *outbox) saveMergeRead(ctx context.Context, row record.OutboxRow, progress record.MergeReadProgress, consumedRefresh bool, now time.Time) error {
	saved, err := checkpointMergeRead(ctx, func(ctx context.Context, write func(context.Context, pgx.Tx) error) error {
		return store.Transact(ctx, r.pool, write)
	}, row, progress, consumedRefresh)
	if err != nil {
		return err
	}
	if !saved {
		return fmt.Errorf("save the merge read of %s: %w", row.Issue, record.ErrOutboxLeaseLost)
	}
	switch {
	case progress.State != "":
		return wait.Until(now.Add(mergeReadParked), fmt.Sprintf("the merge read of %s is %s: %s", row.Issue, progress.State, mergeReadCause(progress)))
	case !progress.RetryAt.IsZero():
		return wait.Until(progress.RetryAt, fmt.Sprintf("the merge read of %s tries its %s stage again: %s", row.Issue, progress.Stage, progress.Cause))
	}
	return wait.Until(now.Add(mergeReadTurnGap), fmt.Sprintf("the merge read of %s goes on at its next turn", row.Issue))
}

// mergeReadCause is what an unread or stopped read waits on, in the words a log line takes.
func mergeReadCause(progress record.MergeReadProgress) string {
	if progress.State == mergeStateStopped {
		return "its " + progress.Stage + " stage failed three times: " + progress.Cause
	}
	return "no candidate answered with a merge that names the issue"
}

// checkpointMergeRead writes progress over the row's progress in a transaction transact opens,
// under the row's lease (store.CheckpointMergeRead), and answers whether it did.
func checkpointMergeRead(ctx context.Context, transact func(context.Context, func(context.Context, pgx.Tx) error) error, row record.OutboxRow,
	progress record.MergeReadProgress, consumedRefresh bool) (bool, error) {
	encoded, err := json.Marshal(progress)
	if err != nil {
		return false, err
	}
	var saved bool
	err = transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		saved, err = store.CheckpointMergeRead(ctx, tx, row.ID, row.LeaseToken, encoded, consumedRefresh)
		return err
	})
	return saved, err
}

// mergeReadFailed is progress after a failed attempt of its stage: the failure counted and its
// cause kept as the daemon stores a far side's error, the third failure stopping the read, and
// else the next attempt due a minute and then five minutes on, or later where GitHub's rate limit
// asks. An uncertain attempt, one reserved and never answered, is counted apart too.
func mergeReadFailed(progress record.MergeReadProgress, cause error, now time.Time, uncertain bool) record.MergeReadProgress {
	progress.Reserved = false
	if uncertain {
		progress.Uncertain++
	}
	progress.Failures++
	progress.Cause = storetext.FarSideError(cause.Error())
	if progress.Failures >= mergeReadAttempts {
		progress.State, progress.RetryAt = mergeStateStopped, time.Time{}
		return progress
	}
	retry := now.Add(mergeReadRetryWaits[progress.Failures-1])
	if after, _ := githubrest.RetryAfter(cause); now.Add(after).After(retry) {
		retry = now.Add(after)
	}
	progress.RetryAt = retry
	return progress
}

// nextMergeRequest is the request the read's next turn makes, or, where it needs none, whether the
// read is done and the merge that answered it (the zero Merge for a search that ended unanswered,
// which it marks unread in progress). It seeds progress with the candidates the record holds, so
// a clock moved to the issue since the last turn is read too.
//
// An issue whose evidence is ANSWERED takes no candidate read: its known merge answers it, after
// one ordinary read of its pull request where the merge lacks the base or the final head (stage
// metadata). Otherwise the stages run in order: the issue's links, its results, each merge
// commit's pull requests page by page, and each candidate. A read that is unread or stopped makes
// a request only for a door's links refresh.
func nextMergeRequest(issue string, recorded ghrepo.Repository, evidence record.MergeEvidence, progress *record.MergeReadProgress) (mergeRequest, record.Merge, bool) {
	if evidence.Class() == record.MergeHistoryAnswered {
		known := evidence.Merge
		if known.Head != "" && known.Base != "" || known.Number <= 0 {
			return mergeRequest{}, known, true
		}
		// The known merge's own pull request is the one candidate, kept as the row the merge was
		// known from: the production check where only it holds the merge, else the retained row.
		repo, source := known.Repo, mergeSourceRetained
		if repo == "" {
			repo = recorded.String()
		}
		if evidence.MergeOnlyCheck && !evidence.Snapshot && !evidence.MergedRow {
			source = mergeSourceProduction
		}
		addMergeCandidate(progress, repo, known.Number, source)
		progress.Stage = mergeStageMetadata
		for i, candidate := range progress.Candidates {
			if candidate.Number != known.Number || !strings.EqualFold(candidate.Repo, repo) {
				continue
			}
			if candidate.Answer != nil || progress.State == mergeStateStopped {
				return mergeRequest{}, mergeWithMetadata(known, progress), true
			}
			return mergeRequest{stage: mergeStageMetadata, candidate: i}, record.Merge{}, false
		}
	}
	seedMergeCandidates(recorded, evidence, progress)
	if progress.State != "" {
		if progress.Refresh {
			return mergeRequest{stage: mergeStageLinks}, record.Merge{}, false
		}
		return mergeRequest{}, record.Merge{}, true
	}
	switch progress.Stage {
	case "", mergeStageLinks:
		progress.Stage = mergeStageLinks
		return mergeRequest{stage: mergeStageLinks}, record.Merge{}, false
	case mergeStageResults:
		return mergeRequest{stage: mergeStageResults}, record.Merge{}, false
	}
	for i, base := range progress.Bases {
		if !base.Done {
			progress.Stage = mergeStageBases
			return mergeRequest{stage: mergeStageBases, base: i}, record.Merge{}, false
		}
	}
	for i, candidate := range progress.Candidates {
		if candidate.Answer == nil {
			progress.Stage = mergeStageCandidates
			return mergeRequest{stage: mergeStageCandidates, candidate: i}, record.Merge{}, false
		}
	}
	if merge, ok := newestMerge(issue, *progress); ok {
		return mergeRequest{}, merge, true
	}
	progress.State, progress.Cursor, progress.Failures, progress.Cause, progress.RetryAt = mergeStateUnread, "", 0, "", time.Time{}
	return mergeRequest{}, record.Merge{}, true
}

// mergeWithMetadata is known, the merge an ANSWERED issue's record holds, with the fields its one
// metadata read answered where the record lacks them; it is known as it is where no read answered.
func mergeWithMetadata(known record.Merge, progress *record.MergeReadProgress) record.Merge {
	for _, candidate := range progress.Candidates {
		answer := candidate.Answer
		if answer == nil || candidate.Number != known.Number || !answer.Merged {
			continue
		}
		if known.Head == "" {
			known.Head = answer.Head
		}
		if known.Base == "" {
			known.Base = answer.Base
		}
		if known.SHA == "" {
			known.SHA = answer.SHA
		}
		if known.MergedAt.IsZero() {
			known.MergedAt = answer.MergedAt
		}
		if known.Repo == "" {
			known.Repo = candidate.Repo
		}
	}
	return known
}

// seedMergeCandidates adds to progress the candidates the record holds of the issue, each once:
// its cleared clocks, its retained pull request, and its production check's pull request, or that
// check's merge commit as a base where the check names pull request 0, which proves no merge. A
// row that names no repository is of the issue's own.
func seedMergeCandidates(recorded ghrepo.Repository, evidence record.MergeEvidence, progress *record.MergeReadProgress) {
	repoOf := func(repo string) string {
		if repo == "" {
			return recorded.String()
		}
		return repo
	}
	for _, clock := range evidence.Clocks {
		addMergeCandidate(progress, repoOf(clock.Repo), clock.Number, mergeSourceCleared)
	}
	if pr := evidence.PullRequest; pr != nil {
		addMergeCandidate(progress, repoOf(pr.Repo), pr.Number, mergeSourceRetained)
	}
	if check := evidence.Check; check != nil {
		if check.Number > 0 {
			addMergeCandidate(progress, repoOf(check.Repo), check.Number, mergeSourceProduction)
		} else if check.MergeSHA != "" {
			addMergeBase(progress, repoOf(check.Repo), check.MergeSHA, mergeSourceProduction)
		}
	}
}

// addMergeCandidate adds the pull request repo#number to progress's candidates unless it holds it
// already, by repository without case and number. A candidate added to a search that ended unread
// is new input, so the search goes on.
func addMergeCandidate(progress *record.MergeReadProgress, repo string, number int, source string) bool {
	if number <= 0 || repo == "" {
		return false
	}
	for _, held := range progress.Candidates {
		if held.Number == number && strings.EqualFold(held.Repo, repo) {
			return false
		}
	}
	progress.Candidates = append(progress.Candidates, record.MergeReadCandidate{Repo: repo, Number: number, Source: source})
	return true
}

// addMergeBase adds the merge commit repo@sha to progress's bases unless it holds it already, by
// repository without case and commit.
func addMergeBase(progress *record.MergeReadProgress, repo, sha, source string) {
	if sha == "" || repo == "" {
		return
	}
	for _, held := range progress.Bases {
		if strings.EqualFold(held.SHA, sha) && strings.EqualFold(held.Repo, repo) {
			return
		}
	}
	progress.Bases = append(progress.Bases, record.MergeReadBase{Repo: repo, SHA: sha, Source: source})
}

// newestMerge is the merge of the newest merged candidate that may answer for issue
// (MergeReadAnswer.Names), by GitHub's merged_at and then the higher number, and whether one did.
func newestMerge(issue string, progress record.MergeReadProgress) (record.Merge, bool) {
	var newest record.Merge
	for _, candidate := range progress.Candidates {
		answer := candidate.Answer
		if answer == nil || !answer.Merged || !answer.Names {
			continue
		}
		merge := record.Merge{Number: candidate.Number, Head: answer.Head, SHA: answer.SHA, Base: answer.Base, Repo: candidate.Repo, MergedAt: answer.MergedAt}
		if !newest.Merged() || merge.MergedAt.After(newest.MergedAt) || merge.MergedAt.Equal(newest.MergedAt) && merge.Number > newest.Number {
			newest = merge
		}
	}
	return newest, newest.Merged()
}

// send makes request's one GET: Dispatch's read of the issue or of its results, one page of the
// pull requests GitHub lists for a merge commit (from its first page or the cursor progress saved,
// a deferred redirect included), or GitHub's read of one pull request.
func (m mergeReader) send(ctx context.Context, issue string, progress record.MergeReadProgress, request mergeRequest) (mergeReadAnswer, error) {
	switch request.stage {
	case mergeStageLinks:
		read, err := m.dispatch.GetIssue(ctx, issue)
		if err != nil {
			return mergeReadAnswer{}, fmt.Errorf("read the links of %s: %w", issue, err)
		}
		return mergeReadAnswer{links: read.Links}, nil
	case mergeStageResults:
		results, err := m.dispatch.IssueResults(ctx, issue)
		if err != nil {
			return mergeReadAnswer{}, fmt.Errorf("read the results of %s: %w", issue, err)
		}
		return mergeReadAnswer{results: results}, nil
	case mergeStageBases:
		base := progress.Bases[request.base]
		repository, err := ghrepo.Parse("the merge commit's repository", base.Repo)
		if err != nil {
			return mergeReadAnswer{}, err
		}
		github, err := m.github(ctx, repository)
		if err != nil {
			return mergeReadAnswer{}, fmt.Errorf("mint the implement App token for %s: %w", repository.Owner(), err)
		}
		path := progress.Cursor
		if path == "" {
			path = "/commits/" + url.PathEscape(base.SHA) + "/pulls"
		}
		page, err := githubrest.GetPage[mergeCommitPull](ctx, github, path)
		if err != nil {
			return mergeReadAnswer{}, fmt.Errorf("read the pull requests of %s@%s: %w", base.Repo, base.SHA, err)
		}
		if page.Next != "" && page.Next == progress.Cursor || page.Redirect != "" && page.Redirect == progress.Cursor {
			return mergeReadAnswer{}, fmt.Errorf("read the pull requests of %s@%s: GitHub named the page it answered as the next one, %s", base.Repo, base.SHA, progress.Cursor)
		}
		return mergeReadAnswer{page: page}, nil
	default:
		// A candidate's read, or an ANSWERED issue's one read of its known merge's pull request
		// (stage metadata), which nextMergeRequest kept as a candidate too.
		candidate := progress.Candidates[request.candidate]
		repository, err := ghrepo.Parse("the pull request's repository", candidate.Repo)
		if err != nil {
			return mergeReadAnswer{}, err
		}
		github, err := m.github(ctx, repository)
		if err != nil {
			return mergeReadAnswer{}, fmt.Errorf("mint the implement App token for %s: %w", repository.Owner(), err)
		}
		var pull githubPullRequest
		if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", candidate.Number), &pull); err != nil {
			return mergeReadAnswer{}, fmt.Errorf("read %s#%d: %w", candidate.Repo, candidate.Number, err)
		}
		return mergeReadAnswer{pull: pull}, nil
	}
}

// applyMergeAnswer is progress once request's GET answered with answer: the stage's answer kept and
// the read moved to what comes next. The links add each pull request they name as a candidate;
// the results add each merge commit a check is compared with as a base, and each merged pull
// request basis as a candidate; a page adds each pull request it lists and saves where the next
// page is; a pull request's read is that candidate's answer.
func applyMergeAnswer(issue string, recorded ghrepo.Repository, progress record.MergeReadProgress, request mergeRequest, answer mergeReadAnswer) record.MergeReadProgress {
	progress.Failures, progress.Cause, progress.RetryAt = 0, "", time.Time{}
	switch request.stage {
	case mergeStageLinks:
		refresh := progress.State != ""
		progress.Refresh = false
		progress.Links = nil
		added := false
		for _, link := range answer.links {
			progress.Links = append(progress.Links, link.URL)
			if repo, number, ok := pullRequestOfLink(link.URL); ok && addMergeCandidate(&progress, repo, number, mergeSourceLink) {
				added = true
			}
		}
		switch {
		case refresh && added:
			// A new link is new input: the search goes on to read it.
			progress.State, progress.Stage = "", mergeStageCandidates
		case !refresh:
			progress.Stage = mergeStageResults
		}
	case mergeStageResults:
		for _, check := range answer.results.Checks {
			for _, basis := range check.ComparedWith {
				repo := basis.Repo
				if repo == "" {
					repo = recorded.String()
				}
				addMergeBase(&progress, repo, basis.MergeCommit, mergeSourceResults)
				if basis.Kind == dispatch.BasisPullRequest && basis.Merged && basis.PullRequest != nil {
					addMergeCandidate(&progress, repo, *basis.PullRequest, mergeSourceResults)
				}
			}
		}
		progress.Stage = mergeStageBases
	case mergeStageBases:
		base := &progress.Bases[request.base]
		for _, pull := range answer.page.Items {
			addMergeCandidate(&progress, base.Repo, pull.Number, base.Source)
		}
		switch {
		case answer.page.Redirect != "":
			progress.Cursor = answer.page.Redirect
		case answer.page.Next != "":
			progress.Cursor = answer.page.Next
		default:
			base.Done, progress.Cursor = true, ""
		}
	case mergeStageMetadata:
		progress.Candidates[request.candidate].Answer = mergeAnswerOf(issue, answer.pull)
		progress.MetadataReads++
	default:
		progress.Candidates[request.candidate].Answer = mergeAnswerOf(issue, answer.pull)
	}
	return progress
}

// mergeAnswerOf is GitHub's read of one pull request as a candidate's answer: whether it merged,
// whether it may answer for issue, its final head, merge commit, base and merged_at. A pull
// request whose branch names another issue (claim.BranchIssue) never answers: one record held its
// number for both, and the other issue's merge starts nothing here. The read guesses no candidate
// from a branch's name: every candidate came from the issue's own rows, links or results.
func mergeAnswerOf(issue string, pull githubPullRequest) *record.MergeReadAnswer {
	names := true
	if key, ok := claim.BranchIssue(pull.Head.Ref); ok {
		names = strings.EqualFold(key, issue)
	}
	return &record.MergeReadAnswer{Merged: pull.Merged, Names: names, Head: pull.Head.SHA, SHA: pull.MergeCommitSHA, Base: pull.Base.Ref,
		MergedAt: pull.MergedAt.UTC()}
}

// pullRequestOfLink is the repository and number of the GitHub pull request link names
// (https://github.com/{owner}/{repo}/pull/{number}, any further path or fragment aside), and
// whether it names one.
func pullRequestOfLink(link string) (string, int, bool) {
	parsed, err := url.Parse(link)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", 0, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", 0, false
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return "", 0, false
	}
	repository, err := ghrepo.Parse("the link's repository", parts[0]+"/"+parts[1])
	if err != nil {
		return "", 0, false
	}
	return repository.String(), number, true
}
