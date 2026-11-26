package requests

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/record"
)

// TestAJudgesApprovalWhoseCommitAnswerWasLostLeavesOneRow is round 5's audit duplicate on
// cybermonklabs/legion#4135, with round 6's m-b. The judge's approval commits the grant and its
// own request.judged row together (D11). When the COMMIT applied and only its answer was lost,
// ApplyJudgeDecision returns ErrCommitUncertain, and the judge then writes the verdict's row on its
// own with applied "unknown". LogJudged resolves it: when the approval's own row is there, it writes
// nothing more, so the trail holds the one row that says what happened; when it is not, the row
// says applied false; when the read fails, the row keeps applied "unknown". A row that says applied
// false — an approval refused before its COMMIT — is written as given, with no read. The cut is
// testCommitCut, run after the transaction committed.
func TestAJudgesApprovalWhoseCommitAnswerWasLostLeavesOneRow(t *testing.T) {
	const reason = "INFRA-12: rotate the deploy key"
	lost := errors.New("unexpected EOF: the COMMIT's answer was lost")
	failureRow := func(applied any, err error) map[string]any {
		return map[string]any{"verdict": "approve", "applied": applied, "not_applied": err.Error()}
	}
	readFails := func(uint32) error { return errors.New("the read of the approval's row failed") }

	t.Run("the commit applied and its answer was lost", func(t *testing.T) {
		m, enr, key := newJudgeFixture(t)
		req := pendingJudgeable(t, m, enr, key, reason, 600, "DEEL_API_KEY")
		m.testCommitCut = func() error { return lost }
		_, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve"})
		m.testCommitCut = nil
		if !errors.Is(err, lost) || !errors.Is(err, ErrCommitUncertain) {
			t.Fatalf("ApplyJudgeDecision with its COMMIT's answer lost = %v; want ErrCommitUncertain wrapping the lost answer's error", err)
		}
		if got := readDecided(t, m, *req.RecordID); got.state != "granted" {
			t.Fatalf("the request after the cut = %+v; want granted: the COMMIT applied", got)
		}
		wrote, err := m.LogJudged(t.Context(), *req.RecordID, judgeModel, failureRow(AppliedUnknown, err))
		if err != nil || wrote != JudgedAlreadyApplied {
			t.Fatalf("LogJudged(applied unknown) after an approval that applied = %q, %v; want %q and nothing written", wrote, err, JudgedAlreadyApplied)
		}
		if judged := readJudged(t, m, *req.RecordID); len(judged) != 1 || judged[0].applied != "true" {
			t.Errorf("request.judged rows = %+v; want the approval's own row alone, applied true", judged)
		}
	})

	t.Run("the commit failed and did not apply", func(t *testing.T) {
		m, enr, key := newJudgeFixture(t)
		req := pendingJudgeable(t, m, enr, key, reason, 600, "DEEL_API_KEY")
		wrote, err := m.LogJudged(t.Context(), *req.RecordID, judgeModel, failureRow(AppliedUnknown, fmt.Errorf("%w: %w", ErrCommitUncertain, lost)))
		if err != nil || wrote != JudgedWritten {
			t.Fatalf("LogJudged(applied unknown) with no approval applied = %q, %v; want %q", wrote, err, JudgedWritten)
		}
		if judged := readJudged(t, m, *req.RecordID); len(judged) != 1 || judged[0].applied != "false" {
			t.Errorf("request.judged rows = %+v; want one, resolved to applied false", judged)
		}
	})

	t.Run("whether the approval applied cannot be read", func(t *testing.T) {
		m, enr, key := newJudgeFixture(t)
		req := pendingJudgeable(t, m, enr, key, reason, 600, "DEEL_API_KEY")
		m.testLandedRead = readFails
		wrote, err := m.LogJudged(t.Context(), *req.RecordID, judgeModel, failureRow(AppliedUnknown, lost))
		if err != nil || wrote != JudgedAppliedUnknown {
			t.Fatalf("LogJudged(applied unknown) whose read failed = %q, %v; want %q", wrote, err, JudgedAppliedUnknown)
		}
		if judged := readJudged(t, m, *req.RecordID); len(judged) != 1 || judged[0].applied != AppliedUnknown {
			t.Errorf("request.judged rows = %+v; want one, applied %q, never false", judged, AppliedUnknown)
		}
	})

	// Round 6's m-b: an approval the state machine refused before its COMMIT certainly wrote
	// nothing, so its row says applied false whatever the read would have said, and the read is
	// not made: a failing read here would otherwise turn a known false into "unknown".
	t.Run("an approval refused before its commit is written as given, with no read", func(t *testing.T) {
		m, enr, key := newJudgeFixture(t)
		req := pendingJudgeable(t, m, enr, key, reason, 600, "DEEL_API_KEY")
		m.testLandedRead = readFails
		refused := fmt.Errorf("%w: the request now belongs to a private project", record.ErrNotApprover)
		wrote, err := m.LogJudged(t.Context(), *req.RecordID, judgeModel, failureRow(false, refused))
		if err != nil || wrote != JudgedWritten {
			t.Fatalf("LogJudged(applied false) for a refused approval = %q, %v; want %q, with no read", wrote, err, JudgedWritten)
		}
		if judged := readJudged(t, m, *req.RecordID); len(judged) != 1 || judged[0].applied != "false" {
			t.Errorf("request.judged rows = %+v; want one, applied false: the refusal is certain", judged)
		}
	})
}
