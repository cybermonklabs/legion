package intake

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// recordCaptureIssue records key, an issue of the CAPTURE project the intake tests decode, with
// its cursor of the Dispatch events it has seen at 0: the cursor moves for a recorded issue alone
// (record.Store.AdvanceSeenSeq).
func recordCaptureIssue(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutIssue(ctx, tx, record.Issue{Key: key, Tree: key, Project: "CAPTURE", Title: "root", Phase: phase.Admitted,
			Generation: 1, Status: "in_progress", Rank: "U", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion"})
	}); err != nil {
		t.Fatalf("record %s: %v", key, err)
	}
}

// seenSeqOf is key's cursor of the Dispatch events it has seen, as the record keeps it.
func seenSeqOf(t *testing.T, pool *pgxpool.Pool, key string) int64 {
	t.Helper()
	ctx := context.Background()
	var issue *record.Issue
	if err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		issue, err = record.NewStore().Issue(ctx, tx, key)
		return err
	}); err != nil || issue == nil {
		t.Fatalf("read %s: %+v, %v", key, issue, err)
	}
	return issue.SeenSeq
}

// seenRefusingPool is a pool (store.TxPool) whose every write of an issue's cursor of the Dispatch
// events it has seen fails while refuse is on, as a PostgreSQL refusing that write would; every
// other statement passes through.
type seenRefusingPool struct {
	store.TxPool
	refuse atomic.Bool
}

func (p *seenRefusingPool) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	tx, err := p.TxPool.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &seenRefusingTx{Tx: tx, pool: p}, nil
}

type seenRefusingTx struct {
	pgx.Tx
	pool *seenRefusingPool
}

func (tx *seenRefusingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "set seen_seq") && tx.pool.refuse.Load() {
		return pgconn.CommandTag{}, errRecordRefused
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

// A message the intake gives up is no event the record has had, so the give-up moves no issue's
// cursor of the Dispatch events it has seen (record.Issue.SeenSeq, seenEvent), as no failed
// delivery does; the replay of its record moves the cursor as the live delivery it stands for
// would have: in the fact's own transaction for a fact, and in the close's for an event no handler
// takes. Two messages of a recorded issue: a fact whose handler fails, and an event no handler
// takes whose cursor write fails, which is a failed delivery as a handler's failure is, given up
// at the bound rather than naked until the stream deletes it.
func TestAGiveUpMovesNoCursorAndItsReplayMovesItAsItsDeliveryWould(t *testing.T) {
	for _, row := range []struct {
		name    string
		subject string
		data    func(t *testing.T) []byte
		// seq is the event's sequence on CAPTURE-3's log.
		seq int64
		// refuseSeen refuses the cursor's write in place of the handler's failure.
		refuseSeen bool
		outcome    ReplayOutcome
	}{
		{name: "a fact whose handler fails", subject: replayIssueSubject("CAPTURE-3"), data: capturedIssueUpdatedEnvelope, seq: 2,
			outcome: ReplayApplied},
		{name: "an event no handler takes whose cursor write fails", subject: "notifications.dispatch.issue.CAPTURE-3.message.created",
			data: func(t *testing.T) []byte {
				return envelopeJSON(t, "seen-message", "dispatch", `{"id":9,"issue_key":"CAPTURE-3","seq":9,"notify":true,"type":"message.created",`+
					`"actor":{"kind":"user","id":"someone"},"payload":{"body":"a word on the issue"}}`)
			}, seq: 9, refuseSeen: true, outcome: ReplayNoFact},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			raw := migratedPool(t)
			recordCaptureIssue(t, raw, "CAPTURE-3")
			pool := &seenRefusingPool{TxPool: raw}
			pool.refuse.Store(row.refuseSeen)
			js, _ := testJetStream(t)
			spec := boundSpec(&lockedBuffer{}, 2, time.Nanosecond)
			spec.AckWait = testwait.Remaining(t)
			consumers, err := OpenConsumers(ctx, js, pool, spec)
			if err != nil {
				t.Fatalf("OpenConsumers: %v", err)
			}
			var fixed atomic.Bool
			handler := handlerFunc(func(context.Context, pgx.Tx, Fact) (Result, error) {
				if !fixed.Load() {
					return Result{}, errAlwaysFails
				}
				return Result{}, nil
			})
			stop := runConsumers(t, consumers, handler)
			sent := publish(t, js, row.subject, row.data(t))
			given := waitGivenUp(t, raw, dispatchConsumerName(spec.Project), sent.Sequence)
			stop()
			if given.GiveUpReason != record.GiveUpBound {
				t.Fatalf("the record = %+v, want it given up at the bound", given)
			}
			if seen := seenSeqOf(t, raw, "CAPTURE-3"); seen != 0 {
				t.Fatalf("CAPTURE-3's cursor after the give-up of its event %d = %d, want 0: the record has not had the event", row.seq, seen)
			}

			fixed.Store(true)
			pool.refuse.Store(false)
			giveUps, err := NewGiveUps(consumers.Records(), handler)
			if err != nil {
				t.Fatalf("NewGiveUps: %v", err)
			}
			answer, err := giveUps.Replay(ctx, record.IntakeSelector{ID: given.ID}, "operator")
			if err != nil || len(answer.Replayed) != 1 || answer.Replayed[0].Outcome != row.outcome {
				t.Fatalf("the replay answered %+v (%v), want %s", answer, err, row.outcome)
			}
			if seen := seenSeqOf(t, raw, "CAPTURE-3"); seen != row.seq {
				t.Fatalf("CAPTURE-3's cursor after the replay = %d, want %d, the replayed event's, as its delivery would have moved it", seen, row.seq)
			}
		})
	}
}

// A replay of a fact applied already with no cursor moved — the boot catch-up applies Dispatch's
// events through the live decoder and moves none (DecodeDispatchEvent) — answers already applied
// and moves the issue's cursor to the event, as a live delivery of a duplicate does: the record
// has had the event.
func TestAReplayOfAFactAppliedAlreadyMovesItsIssuesCursor(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	recordCaptureIssue(t, pool, "CAPTURE-3")
	handler := &keysHandler{}
	data := replayIssueEnvelope(t, "CAPTURE-3")
	decoded, err := decodeMessage(replayIssueSubject("CAPTURE-3"), "CAPTURE", capturedRepositories, data)
	if err != nil {
		t.Fatalf("decode the event: %v", err)
	}
	if _, err := ApplyFact(ctx, pool, decoded.Source, decoded.EventID, decoded.Fact, handler); err != nil {
		t.Fatalf("apply the event as the catch-up does: %v", err)
	}
	if seen := seenSeqOf(t, pool, "CAPTURE-3"); seen != 0 {
		t.Fatalf("CAPTURE-3's cursor after the catch-up's apply = %d, want 0: this test proves nothing", seen)
	}
	given := storedGiveUp(t, pool, dispatchConsumerName("CAPTURE"), replayIssueSubject("CAPTURE-3"), data, 11, 21)
	answer, err := giveUpsOf(t, pool, replaySpec(t, &lockedBuffer{}), handler).Replay(ctx, record.IntakeSelector{ID: given.ID}, "operator")
	if err != nil || len(answer.Replayed) != 1 || answer.Replayed[0].Outcome != ReplayAlreadyApplied {
		t.Fatalf("the replay answered %+v (%v), want already applied", answer, err)
	}
	if seen := seenSeqOf(t, pool, "CAPTURE-3"); seen != 1 {
		t.Fatalf("CAPTURE-3's cursor after the replay of its applied event = %d, want 1, the event's", seen)
	}
}
