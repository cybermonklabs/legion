package requests

import (
	"errors"
	"testing"
)

// TestAJudgedRowIsWrittenWhenTheLandedReadLosesItsConnection is found by the tester of
// cybermonklabs/legion#4135 at 032301a2c4. LogJudged now reads whether a judge's approval landed
// inside the transaction it then writes the verdict's row in (approvalLanded's savepoint). A failure
// of that read that is the database's own (lock_timeout) rolls back the savepoint and the row is
// written with applied "unknown". A failure that is the connection's — the network losing the read,
// a backend ended by a failover — leaves that one transaction unusable, so the write fails too and
// no request.judged row is written at all: a run that asked the model and has no row, the one
// outcome judge.Runner.write exists to prevent. At 2e1415afd4 the read ran on a connection of its
// own and the row was written with applied "unknown".
//
// On the real broker (dev-stack.sh --project-access, the model judge on, the database through a
// wire relay): a judge's approval whose COMMIT the relay held open and then aborted, with the next
// approvalLanded read's connection closed, left request 01dccb0c-…: cancelled, no grant, and no
// request.judged row; the broker logged "the verdict could not be written ... conn closed".
//
// Here the landed read loses its connection the way a failover takes it: the hook ends the backend
// LogJudged's transaction holds, then the read runs on it. The row must still be written, once,
// with applied "unknown", and never false.
func TestAJudgedRowIsWrittenWhenTheLandedReadLosesItsConnection(t *testing.T) {
	const reason = "INFRA-12: rotate the deploy key"
	lost := errors.New("unexpected EOF: the COMMIT's answer was lost")
	m, enr, key := newJudgeFixture(t)
	req := pendingJudgeable(t, m, enr, key, reason, 600, "DEEL_API_KEY")
	ended := false
	m.testLandedRead = func(backend uint32) error {
		// LogJudged's transaction has begun, so its backend is idle in a transaction; this
		// connection of the pool ends it, as a failover ends every backend. It ends that backend
		// alone: every test of every package shares one database, and ending every backend idle in
		// a transaction there cut other tests' transactions.
		return m.Store.Pool.QueryRow(t.Context(), `select pg_terminate_backend($1)`, int64(backend)).Scan(&ended)
	}
	res, err := m.LogJudged(t.Context(), *req.RecordID, judgeModel, map[string]any{"verdict": "approve", "applied": AppliedUnknown, "not_applied": lost.Error()})
	m.testLandedRead = nil
	if !ended {
		t.Fatal("the hook ended no backend, so this run did not lose the landed read's connection and proves nothing")
	}
	if err != nil || res.Write != JudgedAppliedUnknown || res.Verdict != JudgedUnknown {
		t.Errorf("LogJudged(applied false) whose landed read lost its connection = %+v, %v; want %q and verdict %q", res, err, JudgedAppliedUnknown, JudgedUnknown)
	}
	if judged := readJudged(t, m, *req.RecordID); len(judged) != 1 || judged[0].applied != AppliedUnknown || judged[0].verdict != JudgedUnknown {
		t.Errorf("request.judged rows = %+v; want one, applied %q and verdict %q: a run that asked the model keeps its row", judged, AppliedUnknown, JudgedUnknown)
	}
}
