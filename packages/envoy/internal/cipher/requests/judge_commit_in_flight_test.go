package requests

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/store"
)

// TestLogJudgedWaitsForAnApprovalStillCommitting is round 6's m-g on cybermonklabs/legion#4135. The
// judge writes its verdict's row on its own when its approval answered an error, and that error
// can come while the approval's transaction is still committing: its request.judged row is
// written and not yet visible. LogJudged takes the request's row for share before it reads whether
// the approval's own row is there, which waits for that COMMIT or its abort and then reads what
// settled, so an approval that commits leaves its one row and LogJudged writes nothing. The wait is
// bounded by LogJudged's context: an approval that holds past it gets a row saying applied
// "unknown", never false. testHoldCommit holds the approval with every row written.
func TestLogJudgedWaitsForAnApprovalStillCommitting(t *testing.T) {
	// Not parallel: it holds LogJudged's row to the half of a one-second context that approvalLanded's wait leaves it (landedWait), a product latency the load of the package's parallel tests could push past (LogJudged on the subtest's one-second context); the bound is a context's deadline, not a measured time.
	const reason = "INFRA-12: rotate the deploy key"
	failureRow := map[string]any{"verdict": "approve", "applied": AppliedUnknown, "not_applied": "the decision's COMMIT failed: unexpected EOF"}
	type logged struct {
		wrote JudgedWrite
		err   error
	}
	// hold runs the judge's approval of recordID in the background, held uncommitted with its
	// rows written, and answers once it is held; release ends the hold with err, nil to commit.
	hold := func(t *testing.T, m *Machine, recordID string) (release func(error), approved <-chan error) {
		t.Helper()
		held, releases, done := make(chan struct{}), make(chan error), make(chan error, 1)
		var once sync.Once
		m.testHoldCommit = func(uint32) error {
			once.Do(func() { close(held) })
			return <-releases
		}
		go func() {
			_, err := m.ApplyJudgeDecision(context.Background(), recordID, judgeModel, map[string]any{"verdict": "approve"})
			done <- err
		}()
		select {
		case <-held:
		case err := <-done:
			t.Fatalf("the judge's approval ended before its COMMIT was held: %v", err)
		}
		return func(err error) { releases <- err }, done
	}

	t.Run("the approval commits while LogJudged waits", func(t *testing.T) {
		m, enr, key := newJudgeFixture(t)
		req := pendingJudgeable(t, m, enr, key, reason, 600, "DEEL_API_KEY")
		release, approved := hold(t, m, *req.RecordID)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		answered := make(chan logged, 1)
		go func() {
			res, err := m.LogJudged(ctx, *req.RecordID, judgeModel, failureRow)
			answered <- logged{res.Write, err}
		}()
		select {
		case got := <-answered:
			// LogJudged did not wait: let the approval commit, and count what the trail holds.
			release(nil)
			<-approved
			t.Fatalf("LogJudged = %q, %v while the approval's COMMIT was in flight, and the trail holds %d request.judged rows %+v; want it to wait for the COMMIT and write nothing", got.wrote, got.err, len(readJudged(t, m, *req.RecordID)), readJudged(t, m, *req.RecordID))
		case <-time.After(500 * time.Millisecond):
		}
		release(nil)
		if err := <-approved; err != nil {
			t.Fatalf("the held approval, released: %v", err)
		}
		got := <-answered
		if got.err != nil || got.wrote != JudgedAlreadyApplied {
			t.Fatalf("LogJudged after the COMMIT = %q, %v; want %q and nothing written", got.wrote, got.err, JudgedAlreadyApplied)
		}
		if judged := readJudged(t, m, *req.RecordID); len(judged) != 1 || judged[0].applied != "true" {
			t.Errorf("request.judged rows = %+v; want the approval's own row alone, applied true", judged)
		}
	})

	t.Run("the approval holds past LogJudged's deadline", func(t *testing.T) {
		m, enr, key := newJudgeFixture(t)
		req := pendingJudgeable(t, m, enr, key, reason, 600, "DEEL_API_KEY")
		release, approved := hold(t, m, *req.RecordID)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		res, err := m.LogJudged(ctx, *req.RecordID, judgeModel, failureRow)
		release(errors.New("the test aborts the held approval"))
		<-approved
		if err != nil || res.Write != JudgedAppliedUnknown {
			t.Fatalf("LogJudged while the approval held past its deadline = %q, %v; want %q", res.Write, err, JudgedAppliedUnknown)
		}
		if judged := readJudged(t, m, *req.RecordID); len(judged) != 1 || judged[0].applied != AppliedUnknown {
			t.Errorf("request.judged rows = %+v; want LogJudged's alone, applied %q, never false", judged, AppliedUnknown)
		}
	})
}

// TestLogJudgedRecordsACancelCommittingUnderIt: the row LogJudged writes says what became of the
// request as the request's own row says it once every write already holding that row has ended.
// A session's cancel takes the request's row for update; LogJudged takes it for share, so a cancel
// still committing when the judge writes its row is waited for, and the row and its verdict say
// not pending rather than handed to the person. The cancel is held here, written and uncommitted.
func TestLogJudgedRecordsACancelCommittingUnderIt(t *testing.T) {
	m, enr, key := newJudgeFixture(t)
	req := pendingJudgeable(t, m, enr, key, "ACME-12: rotate the deploy key", 600, "DEEL_API_KEY")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := m.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select 1 from requests where id=$1 for update`, req.ID); err != nil {
		t.Fatal(err)
	}
	actor := "session:" + enr
	end := store.RequestEnd{State: "cancelled", Actor: actor, DecidedBy: &actor, Detail: "cancelled by the requesting session"}
	if _, err := store.EndPendingRequests(ctx, tx, store.RequestByID, req.ID, end); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		log JudgedLog
		err error
	}
	answered := make(chan answer, 1)
	go func() {
		log, err := m.LogJudged(ctx, *req.RecordID, judgeModel, map[string]any{"error": "the model did not answer"})
		answered <- answer{log, err}
	}()
	select {
	case got := <-answered:
		t.Fatalf("LogJudged = %+v, %v while the cancel was still committing; want it to wait for the cancel and record it", got.log, got.err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the held cancel: %v", err)
	}
	got := <-answered
	if got.err != nil || got.log.Verdict != JudgedNotPending {
		t.Fatalf("LogJudged after the cancel committed = %+v, %v; want verdict %q", got.log, got.err, JudgedNotPending)
	}
	if judged := readJudged(t, m, *req.RecordID); len(judged) != 1 || judged[0].verdict != JudgedNotPending {
		t.Errorf("request.judged rows = %+v; want one, verdict %q", judged, JudgedNotPending)
	}
}
