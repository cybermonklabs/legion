package delivery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A read-only caller's read of an issue's results (api's comparisonReader, plan D32) asks GitHub
// what a writer's read made at the same moment asks and answers what it answers, and stores
// nothing. Its read-throughs (AnswerCommitPulls, AnswerLinkedHead) run the writer's own code
// (readThroughCommitPulls, refreshPullRequestHead, storePullRequestFacts) with a facts that keeps
// each write for the request instead of the pool (HeadAnswers): every statement of that code runs
// in a transaction of the pool that first makes the writes the request kept before it, in their
// order, and is rolled back. A write it makes is kept, and every later read of the request, the
// reads of the results path among them (HeadsAttributedTo, NewestPullRequestHead,
// BaseBranchMergedFor, BaseBranchMergedIn, PullRequestsNotKnown, LinkedHeadsUnread), runs in such
// a transaction too (HeadAnswers.Read). So each read sees the rows a writer's read would see at
// that place, by the writer's own statements and triggers, and no copy of any rule of the write
// exists to drift from it.
//
// What a rolled-back transaction leaves: no row of any table, and no NOTIFY (a notification is
// sent only when its transaction commits). Every write the read-throughs make goes through that
// transaction; their reads of GitHub are made outside it, so no transaction is held open across a
// call to GitHub, and each one holds the row locks of the request's writes for the milliseconds of
// one read. A read-only read takes no place in the order of reads (startRead): it reads the place
// the last read took and advances no sequence (peekRead), which orders its answers against every
// stored row as the place it would take orders them (the trigger pull_request_heads_older_read
// compares the places with `<`). One thing outlives the rollback: where a write made again owes a
// read (the triggers of pull_request_heads), the owe's revision is a value of
// pull_request_reads_owed_revision_seq, which PostgreSQL never gives back. That leaves a gap in
// the sequence, which no reader reads: a revision is compared only with itself (settleOwedRead),
// and a place only with another place.

// HeadAnswers is what one read-only caller's results read learned from GitHub and stored nowhere:
// each write a writer's read would have made with it, in the order the read made them, as the
// writer's own code (answerWrite), and the merges among them (mergeLearnedOnBranch). The request's
// reader makes one and drops it with the request, so no answer reaches another request. Its zero
// value holds no write, which is what a writer's read passes, and every read over it reads the
// stored rows alone.
type HeadAnswers struct {
	writes []answerWrite
	merges []PullRequestHead
}

// answerWrite is one write a writer's read makes, kept to be made again: the writer's own call,
// on the transaction a later read opens.
type answerWrite func(ctx context.Context, db refs.Pool) error

// transactionBegin is what HeadAnswers needs of a querier it reads through: a pool, or a
// transaction, which opens a savepoint.
type transactionBegin interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// errAnswersNeedTransaction is a read with answers through a querier that cannot open a
// transaction: no caller makes one, since each read of the results path is handed the pool.
var errAnswersNeedTransaction = errors.New("a read with a read-only read's answers needs a querier that opens a transaction")

// empty is whether a holds no write.
func (a HeadAnswers) empty() bool {
	return len(a.writes) == 0
}

// Read runs read on q where a holds no write, which is every writer's read. Else it runs read in a
// transaction of q that first makes a's writes in their order and is rolled back after read, so
// read sees the rows a writer's read would see once its read-throughs had stored them, and stores
// nothing. A write that fails there is the read's error.
func (a HeadAnswers) Read(ctx context.Context, q Querier, read func(q Querier) error) error {
	if a.empty() {
		return read(q)
	}
	begin, ok := q.(transactionBegin)
	if !ok {
		return errAnswersNeedTransaction
	}
	return a.inTransaction(ctx, begin, func(tx pgx.Tx) error { return read(tx) })
}

// inTransaction runs step in a transaction of begin that first makes a's writes, and rolls the
// transaction back, on a context of its own as upsertHead's rollback does.
func (a HeadAnswers) inTransaction(ctx context.Context, begin transactionBegin, step func(tx pgx.Tx) error) error {
	tx, err := begin.Begin(ctx)
	if err != nil {
		return fmt.Errorf("open the transaction of a read-only read: %w", err)
	}
	defer func() {
		rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), headWriteRollback)
		defer cancel()
		_ = tx.Rollback(rollback)
	}()
	for _, write := range a.writes {
		if err := write(ctx, tx); err != nil {
			return fmt.Errorf("make a read-only read's writes again: %w", err)
		}
	}
	return step(tx)
}

// keep makes write as the request's next write, in a transaction of begin that holds the writes
// before it, rolls it back, and keeps write where it succeeded, with merge among the merges where
// merge is a head whose write seeds its base branch (PullRequestHead.seedsBranch). It answers
// write's own answer and error.
func (a *HeadAnswers) keep(
	ctx context.Context, begin transactionBegin, write func(ctx context.Context, db refs.Pool) (bool, error), merge *PullRequestHead,
) (bool, error) {
	var answer bool
	if err := a.inTransaction(ctx, begin, func(tx pgx.Tx) error {
		var err error
		answer, err = write(ctx, tx)
		return err
	}); err != nil {
		return answer, err
	}
	a.writes = append(a.writes, func(ctx context.Context, db refs.Pool) error {
		_, err := write(ctx, db)
		return err
	})
	if merge != nil && merge.seedsBranch() {
		a.merges = append(a.merges, *merge)
	}
	return answer, nil
}

// mergeLearnedOnBranch is whether the writes of the caller's request record sha as on branch of
// repoFull: the yes UpsertPullRequestHead writes for a merged pull request's merge commit on its
// base branch (seedsBranch), whether or not its event-time guard applies the head row, compared
// as branch_commits keys it, the repository without case and the branch and the commit exactly
// (readBranchCommit). Branches.ReadCommitOnBranch reads branch_commits on the pool, outside the
// request's transactions, so it reads these as the row a writer's write would have stored.
func mergeLearnedOnBranch(answers HeadAnswers, repoFull, branch, sha string) bool {
	for _, head := range answers.merges {
		if strings.EqualFold(head.Repo, repoFull) && head.BaseRef == branch && head.MergeCommitSHA == sha {
			return true
		}
	}
	return false
}

// facts is where one read of GitHub keeps what it learned: the pool, for every writer, and for a
// read-only caller's results read the request's answers (AnswerCommitPulls, AnswerLinkedHead),
// whose every statement runs in a transaction of the pool that is rolled back (HeadAnswers).
type facts struct {
	pool    *store.Pool
	answers *HeadAnswers
}

// read runs read on the pool for a writer, and in a transaction holding the request's writes for
// a read-only read.
func (f facts) read(ctx context.Context, read func(q refs.Pool) error) error {
	if f.answers == nil || f.answers.empty() {
		return read(f.pool)
	}
	return f.answers.inTransaction(ctx, f.pool, func(tx pgx.Tx) error { return read(tx) })
}

// write makes write on the pool for a writer, and for a read-only read keeps it for the request
// (HeadAnswers.keep), with merge, the head it writes where it writes one.
func (f facts) write(ctx context.Context, write func(ctx context.Context, db refs.Pool) (bool, error), merge *PullRequestHead) (bool, error) {
	if f.answers == nil {
		return write(ctx, f.pool)
	}
	return f.answers.keep(ctx, f.pool, write, merge)
}

// startRead is the read's place in the order of every read of GitHub: one a writer takes
// (startRead), and the last one taken for a read-only read, which takes none (peekRead).
func (f facts) startRead(ctx context.Context) (readStart, error) {
	if f.answers == nil {
		return startRead(ctx, f.pool)
	}
	return peekRead(ctx, f.pool)
}

// peekRead is startRead for a read-only caller's read, which advances no sequence: the place the
// last read that took one took, and the reader's clock. A writer's read that began at the same
// moment would take a place after it, and every read that began later takes a place after that,
// so against each stored row's place this one is earlier exactly where that one would be: the
// trigger pull_request_heads_older_read keeps the stored row only where the answer's place is
// less than its own.
func peekRead(ctx context.Context, q refs.Queryer) (readStart, error) {
	var read readStart
	if err := q.QueryRow(ctx, `select last_value from pull_request_reads_owed_revision_seq`).Scan(&read.order); err != nil {
		return readStart{}, fmt.Errorf("read the place of the last read of GitHub: %w", err)
	}
	read.at = time.Now()
	return read, nil
}

// frozenHead is head with a copy of its texts of its own, so a write kept for a request
// (facts.write) makes the head as it was when it was kept, whatever its caller changes after.
func frozenHead(head PullRequestHead) PullRequestHead {
	if head.Texts != nil {
		texts := *head.Texts
		texts.CommitMessages = slices.Clone(texts.CommitMessages)
		head.Texts = &texts
	}
	return head
}

// seedsBranch is whether writing head records its merge commit as on its base branch, for good:
// the one rule UpsertPullRequestHead's doc comment argues, kept here so a read-only read that
// stores nothing reads the same yes a writer's store of the same head would have written
// (mergeLearnedOnBranch).
func (head PullRequestHead) seedsBranch() bool {
	return head.State == "merged" && head.BaseRef != "" && head.MergeCommitSHA != ""
}
