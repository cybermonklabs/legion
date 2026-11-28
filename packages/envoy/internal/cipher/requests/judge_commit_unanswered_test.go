package requests

import (
	"errors"
	"testing"
)

// TestADecisionWhoseCommitGotNoAnswerSaysSo is the tester's minor on cybermonklabs/legion#4135 at
// 032301a2c4: a decision whose COMMIT failed, its connection lost mid-commit, answers
// ErrCommitUncertain, so the judge logs it as that and not as a refusal. Here the
// connection goes the way a failover takes it: with every row of the decision written and before
// its COMMIT (testHoldCommit), the backend that holds the transaction is ended, and the COMMIT then
// meets a closed connection. Nothing applied, so the request is still pending, and LogJudged's
// row for the verdict says applied false: the approval's own row is not there.
func TestADecisionWhoseCommitGotNoAnswerSaysSo(t *testing.T) {
	m, enr, key := newJudgeFixture(t)
	req := pendingJudgeable(t, m, enr, key, "INFRA-12: rotate the deploy key", 600, "DEEL_API_KEY")
	ended := false
	// The decision's own backend, and no other: every test of every package shares one database.
	m.testHoldCommit = func(backend uint32) error {
		return m.Store.Pool.QueryRow(t.Context(), `select pg_terminate_backend($1)`, int64(backend)).Scan(&ended)
	}
	_, err := m.ApplyJudgeDecision(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve"})
	m.testHoldCommit = nil
	if !ended {
		t.Fatal("the hook ended no backend, so the COMMIT did not lose its connection and this proves nothing")
	}
	if !errors.Is(err, ErrCommitUncertain) {
		t.Fatalf("ApplyJudgeDecision whose COMMIT lost its connection = %v; want ErrCommitUncertain", err)
	}
	if got := readDecided(t, m, *req.RecordID); got.state != "pending" || got.events != 0 {
		t.Fatalf("the request after the lost COMMIT = %+v; want it untouched and pending", got)
	}
	res, err := m.LogJudged(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve", "applied": AppliedUnknown, "not_applied": err.Error()})
	if err != nil || res.Write != JudgedWritten {
		t.Fatalf("LogJudged(applied unknown) after a COMMIT that did not apply = %q, %v; want %q, resolved to false", res.Write, err, JudgedWritten)
	}
	if judged := readJudged(t, m, *req.RecordID); len(judged) != 1 || judged[0].applied != "false" {
		t.Errorf("request.judged rows = %+v; want one, applied false", judged)
	}
}
