// Package daemon: this file holds the controller's marks on one issue's merge as the row that
// submits that merge reads and spends them (readyMergeMarks, the ghautomerge.Marks of
// readyMerge): the project's merge holds, which keep a submission back and whose held stretch
// this closes again, and the issue's jump mark, which sends GitHub's enqueue at the head of the
// merge queue. The messages and notices each mark is told in are here with them; the submission
// itself is auto_merge.go's.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghautomerge"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// readyMergeMarks is what the controller decided about one issue's merge, as one run of its
// ready_merge row reads it the moment before the submission goes out (ghautomerge.Marks). Two
// marks reach this seam, and both are the controller's alone: the project's merge holds
// (record.MergeHold), which keep a submission back while a hold matches a file the pull request
// changes, and the issue's jump mark (record.MergeJump), which sends GitHub's enqueue at the head
// of the merge queue in place of the auto-merge arm. ghautomerge knows neither, so every rule
// about which GitHub call is sent stays in that package and every rule about the controller's
// marks stays here.
//
// It is one value per run of one row because every answer it gives is about that run: the hold
// decision is cached for the run (holding), the payload it writes is the run's own copy, and the
// jump record it builds is what a refusal of that jump updates (jumped). runner, row and payload
// are the row as readyMerge holds it — payload is readyMerge's own variable, so a field this
// writes is read by every arm of the run that follows. github is the GraphQL client the
// submission is made with, which the queue read before a jump reuses, and rest is the
// repository's REST base, the client readyMerge built for a failing job's logs, which the read of
// the pull request's changed files reuses: both reads are this row's own and neither is worth a
// second client.
type readyMergeMarks struct {
	runner  *outbox
	row     record.OutboxRow
	payload *record.ReadyMerge
	repo    ghrepo.Repository
	github  githubrest.Client
	rest    githubrest.Client
	// decided and held are the run's one hold decision: held is the hold that holds this
	// submission, nil where none does, and decided says the decision was made. Submit asks for it
	// once, and readyMerge asks again on the conflict arm Submit answers before it chooses a
	// submission, so a run can ask twice and must read the pull request's changed files at most
	// once.
	decided bool
	held    *ghautomerge.Held
	// jumped is the record written when the mark was spent (record.MergeJumpRecord), kept so that
	// GitHub's refusal of that jump updates the record it wrote rather than building a second one
	// from values read again: the entries the jump would have displaced are what the refusal has
	// to keep, and they were read before the mutation and nowhere else.
	jumped record.MergeJumpRecord
}

// Hold is the project's merge holds asked of this one submission: the error the hold answers
// with (*ghautomerge.Held) where one holds it, which Submit returns as the whole answer of the
// run with nothing sent, and nil where none does.
//
// The decision is made once per run and kept. Submit asks it for the submission it chose, and
// readyMerge asks it again for the merge-queue removal Submit decides before that choice, so
// without the cache a run could read the pull request's changed files twice. A read that failed
// is not cached: it is the row's failure, the row retries, and the next run reads again.
func (m *readyMergeMarks) Hold(ctx context.Context) error {
	if !m.decided {
		held, err := m.holding(ctx)
		if err != nil {
			return err
		}
		m.decided, m.held = true, held
	}
	if m.held == nil {
		return nil
	}
	return m.held
}

// holding is the hold that holds this submission, nil where none does, decided in the order that
// costs a project with no hold nothing at all (requirement R9):
//
//  1. the project's live holds, read in one transaction that also deletes the holds whose end
//     time has passed (record.Store.MergeHolds). No live hold answers nil here, and nothing of
//     GitHub is read at all: a project that holds nothing pays one Postgres read per merge run.
//  2. the holds whose repository covers this issue's repository. An empty Repository covers every
//     repository the project serves, and a named one is compared with the repository the daemon
//     serves this issue under, without regard to case, as GitHub compares an owner and a
//     repository's name alike (ghrepo.Serves). None left answers nil, again with nothing read of
//     GitHub.
//  3. the pull request's changed files, read once (githubrest.PullRequestPaths), matched against
//     each hold's patterns (heldPaths).
//
// A list GitHub cut at githubrest.PullRequestFilesLimit matches every covering hold, whatever
// the paths it did show: the daemon cannot tell whether the files it never saw are a hold's, and
// a hold that is missed lets through the one merge it exists to keep back (requirement R6). The
// hold that answers is the one with the earliest end time among those that matched, so the row's
// next run is the first moment any of them could have stopped holding, and the holds that outlast
// it are read again then.
//
// The decision writes before it answers (tellHeld): the moment the hold first caught this
// submission, and, once, the message on the Dispatch issue and the architect's notice.
func (m *readyMergeMarks) holding(ctx context.Context) (*ghautomerge.Held, error) {
	r := m.runner
	now := r.now().UTC()
	var holds []record.MergeHold
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		holds, err = r.records.MergeHolds(ctx, tx, r.dispatchProject, now)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read the merge holds of %s before outbox row %d submits its merge: %w", r.dispatchProject, m.row.ID, err)
	}
	// The list this read answered is this run's own, so the holds that cover another repository
	// are dropped out of it rather than copied into a second slice.
	covering := slices.DeleteFunc(holds, func(hold record.MergeHold) bool {
		return hold.Repository != "" && !strings.EqualFold(hold.Repository, m.repo.String())
	})
	if len(covering) == 0 {
		return nil, nil
	}
	paths, cut, err := githubrest.PullRequestPaths(ctx, m.rest, m.payload.Number)
	if err != nil {
		return nil, fmt.Errorf("read the files pull request #%d of %s changes, which the merge holds of %s are matched against: %w",
			m.payload.Number, m.repo, r.dispatchProject, err)
	}
	var holding *record.MergeHold
	for index, hold := range covering {
		if !cut && !heldPaths(hold, paths) {
			continue
		}
		if holding == nil || hold.EndsAt.Before(holding.EndsAt) {
			holding = &covering[index]
		}
	}
	if holding == nil {
		return nil, nil
	}
	if err := m.tellHeld(ctx, *holding, now); err != nil {
		return nil, err
	}
	return &ghautomerge.Held{Number: m.payload.Number, Repo: m.repo, Head: m.payload.Head, Reason: holding.Reason, Ends: holding.EndsAt}, nil
}

// heldPaths says whether hold catches a change that touches paths: each of the hold's patterns is
// an unanchored regular expression matched against every path, which is how the project's by_hand
// merge rules are matched (config.MergeRules.MatchByHand), so a controller writing a hold and a
// project writing a merge rule write the same kind of pattern.
//
// A pattern the daemon cannot compile catches everything. The daemon cannot tell what such a
// pattern would have matched, and the hold exists to keep merges back: reading it as matching
// nothing would turn a typo into a hold that silently holds nothing, where this way the hold
// holds, says so on the issue, and ends at its own end time like any other.
func heldPaths(hold record.MergeHold, paths []string) bool {
	for _, pattern := range hold.Paths {
		matcher, err := regexp.Compile(pattern)
		if err != nil {
			return true
		}
		for _, path := range paths {
			if matcher.MatchString(path) {
				return true
			}
		}
	}
	return false
}

// tellHeld records that hold holds this submission, in one transaction fenced on the row's lease
// (replacePayload): the moment the open held stretch began (record.ReadyMerge.HeldSince) where
// the row carries none, and, the first time the row is held at all (HeldTold), that flag with the
// two rows that tell of it — one message on the Dispatch issue naming the hold, its paths, its
// reason, the session that set it and when it ends, and one merge-held notice for the issue's
// architect.
//
// The three are one write because the promise is exactly one message and exactly one notice
// however many times the row runs under the hold (requirement R8): a flag written apart from its
// rows could stand without them, and rows written apart from the flag could be written again by
// the next run. A replacement that did not take — the row's lease gone to another worker — writes
// nothing at all, and nothing of it is kept on the run's own payload either; that worker reads
// the same holds and tells of them itself. The hold still answers either way, since what holds a
// merge back is the hold and not the telling of it.
func (m *readyMergeMarks) tellHeld(ctx context.Context, hold record.MergeHold, now time.Time) error {
	if !m.payload.HeldSince.IsZero() && m.payload.HeldTold {
		return nil
	}
	payload := *m.payload
	if payload.HeldSince.IsZero() {
		payload.HeldSince = now
	}
	var told []record.OutboxRow
	if !payload.HeldTold {
		payload.HeldTold = true
		post, err := record.NewOutboxRow(m.row.Issue, readyMergeHeldPost(payload, hold), now)
		if err != nil {
			return err
		}
		notice, err := record.NewOutboxRow(m.row.Issue, record.Notice{Kind: "merge-held", Role: claim.RoleArchitect,
			Reason: readyMergeHeldReason(payload, hold)}, now)
		if err != nil {
			return err
		}
		told = append(told, post, notice)
	}
	replaced, err := m.runner.replacePayload(ctx, m.row, payload, told...)
	if err != nil {
		return fmt.Errorf("record the merge hold that holds outbox row %d back: %w", m.row.ID, err)
	}
	if !replaced {
		return nil
	}
	*m.payload = payload
	if len(told) > 0 {
		m.runner.log.Info("outbox READY merge is held by one of the project's merge holds; the issue is told and its architect once",
			"row", m.row.ID, "issue", m.row.Issue, "number", payload.Number, "head", payload.Head,
			"paths", hold.Paths, "reason", hold.Reason, "session", hold.Session, "ends", hold.EndsAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// closeHeld closes the held stretch the payload may have open, on the first run that did not end
// held — err is what Submit answered that run with, and a *ghautomerge.Held in it is a run that
// ended held — whatever else that run then does: every arm of readyMerge below the call reads
// the payload this closes, and the wait rememberWait measures is the one the close leaves
// (requirement R7). It is called above the conflict arm, because a run can end held in several
// ways and reach the arms by none of them. Marks.Hold is asked only once GitHub reports the pull
// request mergeable, so a held head that stops being mergeable is never asked again: without
// this close its stretch would stay open for ever and the merge-waiting notice would be measured
// from a hold that stopped holding days ago. A replacement that did not take is no failure, since
// the worker that holds the row reads the same submission itself.
func (m *readyMergeMarks) closeHeld(ctx context.Context, err error) error {
	var heldRun *ghautomerge.Held
	if m.payload.HeldSince.IsZero() || errors.As(err, &heldRun) {
		return nil
	}
	m.payload.Held += m.runner.now().UTC().Sub(m.payload.HeldSince)
	m.payload.HeldSince = time.Time{}
	if _, failure := m.runner.replacePayload(ctx, m.row, *m.payload); failure != nil {
		return fmt.Errorf("record the time a merge hold held outbox row %d back: %w", m.row.ID, failure)
	}
	return nil
}

// Jump is the issue's jump mark asked of the enqueue this submission is about to send: true where
// the mark was there and this run spent it, so Submit sends GitHub's enqueue with jump instead of
// the auto-merge enable, and false where the issue carries no mark or another worker's run spent
// it first.
//
// Nothing of GitHub is read for an issue with no mark. With one, the queue the jump is about to go
// ahead of is read before anything is sent (ghautomerge.QueueEntries, bounded by jumpQueueRead),
// because once the enqueue lands the queue it displaced is gone; a read GitHub refused is no
// failure of the jump (queueBefore).
//
// The mark is spent at the mutation and not before (requirement R2): one transaction fenced on
// the row's lease clears the mark, writes the record of the jump and enqueues the message that
// posts it, and only then does Submit send the enqueue. A run that sends no enqueue — the lease
// gone, the mark already spent, the submission chosen not an enqueue at all — spends nothing. The
// one honest exception is a daemon that dies between that commit and GitHub's answer: the mark is
// gone, the merge was never submitted as a jump, and the controller marks the issue again, which
// the state shows it needing by showing the issue unmarked.
func (m *readyMergeMarks) Jump(ctx context.Context, branch string) (bool, error) {
	r := m.runner
	var mark *record.MergeJump
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		mark, err = r.records.MergeJumpOf(ctx, tx, m.row.Issue)
		return err
	}); err != nil {
		return false, fmt.Errorf("read the jump mark of %s before outbox row %d submits its merge: %w", m.row.Issue, m.row.ID, err)
	}
	if mark == nil {
		return false, nil
	}
	entries, total, unknown := m.queueBefore(ctx, branch)
	jumped := record.MergeJumpRecord{Repository: m.repo.String(), Issue: m.row.Issue, Number: m.payload.Number, Head: m.payload.Head,
		At: r.now().UTC(), Reason: mark.Reason, Entries: entries, EntriesTotal: total, EntriesUnknown: unknown}
	told, err := record.NewOutboxRow(m.row.Issue, readyMergeJumpedPost(jumped, *mark), jumped.At)
	if err != nil {
		return false, err
	}
	// The spend is fenced on the row's lease as every other write this row makes is: the worker
	// whose replacement takes is the worker that sends the enqueue. The two ways it can write
	// nothing are different answers. A lease gone to another worker ends this run with a wait and
	// sends no mutation at all, as resubmitAfterConflict does with the same fence: that worker
	// makes this submission, and a second enqueue of the same head from this run would ask GitHub
	// for a merge nobody is waiting on an answer to. ClearMergeJump answering no row is the mark
	// spent or taken off between the read above and this transaction, which leaves an unmarked
	// issue: the submission goes out as it does for every issue that carries no mark.
	held, spent, err := m.markWrite(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		cleared, err := r.records.ClearMergeJump(ctx, tx, m.row.Issue)
		if err != nil || !cleared {
			return false, err
		}
		if err := r.records.PutMergeJumpRecord(ctx, tx, r.dispatchProject, jumped); err != nil {
			return false, err
		}
		if err := r.records.Enqueue(ctx, tx, told); err != nil {
			return false, err
		}
		return true, nil
	})
	switch {
	case err != nil:
		return false, fmt.Errorf("spend the jump mark of %s as outbox row %d submits its merge: %w", m.row.Issue, m.row.ID, err)
	case !held:
		return false, wait.Errorf("outbox row %d no longer holds its lease, so it submits nothing: the worker that holds it makes this merge", m.row.ID)
	case !spent:
		return false, nil
	}
	m.jumped = jumped
	r.log.Info("outbox sends READY's merge to the head of the merge queue on the controller's jump mark, which this run spends",
		"row", m.row.ID, "issue", m.row.Issue, "repo", m.repo.String(), "number", jumped.Number, "head", jumped.Head,
		"branch", branch, "reason", jumped.Reason, "session", mark.Session, "queue", jumped.EntriesTotal, "queueUnknown", jumped.EntriesUnknown)
	return true, nil
}

// JumpRefused is a jump that did not take, told to this row before Submit sends the plain
// submission in the same attempt: GitHub's own answer where GitHub gave one, and a request
// Legion could not deliver where GitHub never answered at all (ghautomerge.JumpFailure). The
// mark is already spent, so the account of the jump has to carry the failure or a jump GitHub
// never made would read as one that went ahead of the queue: the record is updated in place,
// keeping the entries the jump would have displaced (requirement R4), the issue is told, and the
// controller's own notice carries the issue, the pull request, the head and what happened.
//
// Both texts and the record say which of the two it was. A transport failure rendered as
// GitHub's refusal would tell the controller that GitHub decided something it was never asked,
// and would send it reading the repository's merge-queue rules for a network failure.
//
// The failure's own text is cut once here (ghautomerge.CutLine), before the record and both
// texts are written from it, as ghautomerge cuts the strings it renders: an answer GitHub
// writes at any length, or a transport failure naming a whole URL, would otherwise reach a
// Dispatch block whole and could have the block refused, which would tell the architect of a
// refused message instead of telling anyone of the jump that did not take. A write the row's
// lease no longer carries writes nothing and fails nothing: Submit sends the plain submission
// either way, and the worker that holds the row reads the same submission itself.
func (m *readyMergeMarks) JumpRefused(ctx context.Context, failure ghautomerge.JumpFailure) error {
	r := m.runner
	now := r.now().UTC()
	failure.Answer = ghautomerge.CutLine(failure.Answer)
	refused := m.jumped
	refused.Refused, refused.Refusal = true, jumpRefusalRecorded(failure)
	post, err := record.NewOutboxRow(m.row.Issue, readyMergeJumpRefusedPost(refused, failure), now)
	if err != nil {
		return err
	}
	notice, err := record.NewOutboxRow(m.row.Issue, record.ControllerNotice{Kind: record.MergeJumpRefusedNotice,
		Reason: readyMergeJumpRefusedReason(refused, failure)}, now)
	if err != nil {
		return err
	}
	_, wrote, err := m.markWrite(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		if err := r.records.PutMergeJumpRecord(ctx, tx, r.dispatchProject, refused); err != nil {
			return false, err
		}
		for _, enqueue := range []record.OutboxRow{post, notice} {
			if err := r.records.Enqueue(ctx, tx, enqueue); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("record the jump outbox row %d made and GitHub did not take: %w", m.row.ID, err)
	}
	if wrote {
		m.jumped = refused
		r.log.Warn("outbox asked GitHub to send READY's merge to the head of the merge queue and the jump did not take; the plain submission follows in this attempt",
			"row", m.row.ID, "issue", m.row.Issue, "number", refused.Number, "head", refused.Head,
			"answered", failure.Answered, "refusal", refused.Refusal)
	}
	return nil
}

// jumpRefusalRecorded is what the record of the jump keeps in Refusal (record.MergeJumpRecord):
// GitHub's own words where GitHub answered the enqueue, and the failure named as a request Legion
// never delivered where it did not. Refused is true either way, since the jump did not go ahead
// of the queue whichever it was, so these words are where a reader of the record alone tells
// GitHub's refusal from a jump GitHub never saw.
func jumpRefusalRecorded(failure ghautomerge.JumpFailure) string {
	if failure.Answered {
		return failure.Answer
	}
	return "Legion could not deliver the jump to GitHub: " + failure.Answer
}

// queueBefore is the merge queue of branch as it stood before this jump, as the record keeps it
// (record.MergeQueueEntryRecord), GitHub's own count of the whole queue, and whether the read
// failed. A read GitHub refused is no failure of the jump (requirement R3): the record says its
// entries are unknown, every other field of it stands, and the enqueue is sent. The queue is
// read for the account of what the jump displaced and for nothing else, so a jump the controller
// asked for is never lost to a read that answered nothing.
//
// The read runs on a budget of its own (jumpQueueRead) off the row's, so a queue GitHub is slow
// to answer cannot spend what the mutation and the spend transaction after it need.
func (m *readyMergeMarks) queueBefore(ctx context.Context, branch string) ([]record.MergeQueueEntryRecord, int, bool) {
	read, cancel := context.WithTimeout(ctx, jumpQueueRead)
	defer cancel()
	entries, total, err := ghautomerge.QueueEntries(read, m.github, m.repo, branch)
	if err != nil {
		m.runner.log.Warn("outbox could not read the merge queue its READY merge is about to jump; the jump is made and its record says the queue is unknown",
			"row", m.row.ID, "issue", m.row.Issue, "number", m.payload.Number, "branch", branch, "error", err)
		return nil, 0, true
	}
	recorded := make([]record.MergeQueueEntryRecord, 0, len(entries))
	for _, entry := range entries {
		recorded = append(recorded, record.MergeQueueEntryRecord{Number: entry.Number, Position: entry.Position,
			Jump: entry.Jump, State: entry.State, EnqueuedAt: entry.EnqueuedAt.UTC()})
	}
	return recorded, total, false
}

// markWrite runs write in one transaction fenced on the row's lease and answers two things: that
// the lease was still this run's, and that write wrote. The row's payload is replaced first under
// its lease token, exactly as resubmitAfterConflict fences its own write (replacePayload), and a
// replacement that did not take leaves the transaction with nothing else written at all. The
// payload written is the run's own copy, so a mark write never loses what another decision of
// this run put on it.
//
// The two answers are apart because the callers read them differently. A lease gone to another
// worker means this run must do nothing more at all, since that worker is making the submission;
// write answering false means the mark it went to spend was not there, which leaves an ordinary
// unmarked submission.
//
// The transaction carries the commit hooks (intake.WithCommitHooks), so the lane of every row
// this enqueues is woken as the write commits, which is what makes a message posted beside a
// spent mark reach Dispatch without waiting for the next sweep.
func (m *readyMergeMarks) markWrite(ctx context.Context, write func(context.Context, pgx.Tx) (bool, error)) (bool, bool, error) {
	scoped, committed := intake.WithCommitHooks(ctx)
	var held, wrote bool
	if err := pgx.BeginFunc(scoped, m.runner.pool, func(tx pgx.Tx) error {
		replaced, err := m.runner.records.ReplaceOutboxPayload(scoped, tx, m.row.ID, m.row.LeaseToken, *m.payload)
		if err != nil || !replaced {
			return err
		}
		held = true
		wrote, err = write(scoped, tx)
		return err
	}); err != nil {
		return false, false, err
	}
	committed()
	return held, wrote, nil
}

// readyMergeHeldPost is the message posted on the issue the first time one of the project's merge
// holds keeps its merge back (readyMergeMarks.tellHeld): the pull request and READY's head, the
// hold's path patterns, the reason the controller set it for, the session that set it, and when
// it ends. It is written on the issue because the issue is where the question "why has this not
// merged" is asked, and it names the owner and the end time so that the answer needs no second
// reader: the hold is nobody's to undo on the issue, and the merge goes out when it ends.
//
// The hold's own words are the controller's and reach a Dispatch block from an outbox payload, so
// each of them is cut once here (ghautomerge.CutLine), as GitHub's are everywhere else in the
// daemon's merge messages.
func readyMergeHeldPost(payload record.ReadyMerge, hold record.MergeHold) record.MessagePost {
	return record.MessagePost{Body: fmt.Sprintf("Legion has not submitted the merge of pull request #%d at READY's head %s: one of the project's merge holds matches a file this pull request changes. The hold covers %s, %s set it for %q, and it ends at %s. Nothing is anyone's to do: the issue stays in awaiting_merge, and the daemon submits the merge once the hold ends.",
		payload.Number, payload.Head, heldPathsNamed(hold), markSession(hold.Session), ghautomerge.CutLine(hold.Reason), hold.EndsAt.UTC().Format(time.RFC3339))}
}

// readyMergeHeldReason is the merge-held notice's reason, the architect's one telling that a merge
// hold holds this issue's merge (readyMergeMarks.tellHeld): the same facts the issue's own message
// carries, in one sentence, since a notice is read in a list of notices. The architect is told
// because the issue stands still and its architect is the one who would otherwise look for a
// reason on GitHub; the controller set the hold and needs no telling.
func readyMergeHeldReason(payload record.ReadyMerge, hold record.MergeHold) string {
	return fmt.Sprintf("Legion has not submitted the merge of pull request #%d at READY's head %s: a merge hold of this project covers %s until %s (%s set it for %q). The issue stays in awaiting_merge and the merge is submitted when the hold ends.",
		payload.Number, payload.Head, heldPathsNamed(hold), hold.EndsAt.UTC().Format(time.RFC3339), markSession(hold.Session), ghautomerge.CutLine(hold.Reason))
}

// heldPathsNamed is a hold's path patterns as a message names them, each cut once and separated
// by ", ".
func heldPathsNamed(hold record.MergeHold) string {
	named := make([]string, 0, len(hold.Paths))
	for _, path := range hold.Paths {
		named = append(named, ghautomerge.CutLine(path))
	}
	return strings.Join(named, ", ")
}

// markSession is the controller session that set a mark as a message names it, and "the
// controller" for a mark that records none: a hold list or a jump mark written by a deployment
// before the session was recorded, or by a route that names none. Both marks carry the session
// the same way, so both are named by this one rule.
func markSession(session string) string {
	if session == "" {
		return "the controller"
	}
	return ghautomerge.CutLine(session)
}

// readyMergeJumpedPost is the message posted on the issue when the daemon spends its jump mark
// (readyMergeMarks.Jump): the pull request and READY's head sent to the head of the merge queue,
// the controller's reason, the session that marked the issue, and what the queue held at that
// moment, which is whose merge the jump went ahead of. It is written on the issue because the
// jump is a decision about this issue's merge that the tree reads here, and it names the queue as
// it stood because that is the one moment the account can be taken: once the enqueue lands, the
// queue it displaced is gone.
func readyMergeJumpedPost(jumped record.MergeJumpRecord, mark record.MergeJump) record.MessagePost {
	return record.MessagePost{Body: fmt.Sprintf("Legion sent the merge of pull request #%d at READY's head %s to the head of %s's merge queue on the controller's jump mark: %s marked the issue for %q. %s The mark is spent: the next merge of this issue is submitted as any other.",
		jumped.Number, jumped.Head, jumped.Repository, markSession(mark.Session), ghautomerge.CutLine(jumped.Reason), jumpedQueueNamed(jumped))}
}

// readyMergeJumpRefusedPost is the message posted on the issue when the jump did not take
// (readyMergeMarks.JumpRefused): what happened to it, and what follows it. The mark is spent
// either way, and the plain submission goes out in the same attempt, so the message says both: a
// reader of the issue must not take a failed jump for a merge that went ahead of the queue, nor
// for a merge that was never submitted.
//
// GitHub's answer is quoted as GitHub's own only where GitHub gave one (ghautomerge.JumpFailure):
// a timeout or a 502 is a request Legion could not deliver, and writing it as GitHub's refusal
// would put words on the issue that GitHub never said.
func readyMergeJumpRefusedPost(refused record.MergeJumpRecord, failure ghautomerge.JumpFailure) record.MessagePost {
	lead := fmt.Sprintf("Legion could not send the merge of pull request #%d at READY's head %s to the head of %s's merge queue, and GitHub never answered the request: %s.",
		refused.Number, refused.Head, refused.Repository, failure.Answer)
	if failure.Answered {
		lead = fmt.Sprintf("GitHub refused to send the merge of pull request #%d at READY's head %s to the head of %s's merge queue: %s.",
			refused.Number, refused.Head, refused.Repository, failure.Answer)
	}
	return record.MessagePost{Body: lead + " Legion submitted the merge as it does without a mark, in the same attempt, so this merge waits in the queue behind what was already in it. The jump mark is spent: the controller marks the issue again if the merge is still to jump."}
}

// readyMergeJumpRefusedReason is the merge-jump-refused controller notice's reason: the issue, its
// pull request, READY's head and what became of the jump (requirement R4) — GitHub's own answer
// where GitHub answered the enqueue, and a request Legion could not deliver where it did not. It
// is the controller's notice and no architect's, since the jump was the controller's instruction
// and the merge itself is on its way; the controller is the one who decides whether to mark the
// issue again, and a failure Legion never delivered is a different decision from a refusal
// GitHub's own rules gave.
func readyMergeJumpRefusedReason(refused record.MergeJumpRecord, failure ghautomerge.JumpFailure) string {
	if failure.Answered {
		return fmt.Sprintf("GitHub refused the jump mark Legion spent on %s: the merge of pull request #%d of %s at READY's head %s was not sent to the head of the merge queue. GitHub answered: %s. Legion submitted the merge without the jump in the same attempt, and the mark is spent.",
			refused.Issue, refused.Number, refused.Repository, refused.Head, failure.Answer)
	}
	return fmt.Sprintf("The jump mark Legion spent on %s did not reach GitHub: the merge of pull request #%d of %s at READY's head %s was not sent to the head of the merge queue. GitHub answered nothing, and Legion could not deliver the request: %s. Legion submitted the merge without the jump in the same attempt, and the mark is spent.",
		refused.Issue, refused.Number, refused.Repository, refused.Head, failure.Answer)
}

// jumpedQueueNamed is the queue a jump went ahead of as a message names it: the pull requests that
// stood in it, oldest entry first, up to what the read carried, with GitHub's own count where the
// queue was longer than that. A queue nobody could read says so rather than reading as an empty
// one (record.MergeJumpRecord.EntriesUnknown), and an empty queue says the jump displaced nothing,
// which is the usual case and worth stating.
func jumpedQueueNamed(jumped record.MergeJumpRecord) string {
	switch {
	case jumped.EntriesUnknown:
		return "GitHub did not answer what the queue held, so this merge's place in it is unrecorded."
	case len(jumped.Entries) == 0:
		return "The queue held nothing, so the jump displaced no merge."
	}
	numbers := make([]string, 0, len(jumped.Entries))
	for _, entry := range jumped.Entries {
		numbers = append(numbers, fmt.Sprintf("#%d", entry.Number))
	}
	named := fmt.Sprintf("The queue held %s", strings.Join(numbers, ", "))
	if jumped.EntriesTotal > len(jumped.Entries) {
		named = fmt.Sprintf("%s and %d more", named, jumped.EntriesTotal-len(jumped.Entries))
	}
	return named + ", which this merge now goes ahead of."
}
