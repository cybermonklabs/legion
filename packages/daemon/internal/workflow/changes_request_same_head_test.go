package workflow

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A person can request changes on one version twice, the second time with the reason revised
// near its end. Where both reasons are over texts.OneAct and share their first 4,000 units, the
// notice of each carries the same cut reason, and ChangesSuperseded, which reads the notice's
// reason against the told reason's head, cannot tell the older request from the one the gate
// records as told: the older request is relayed and kept in place of the newer one. The gate keeps
// the told request's place in Dispatch's event log (ToldChangesMark) and each notice names the
// time of its review, so the two are not the same request (INFRA-3686 tester red test, round 23,
// class of PRRT_kwDORFy7ds67UiZ5).
func TestAnOlderChangesRequestWhoseReasonSharesTheToldReasonsHeadIsReplaced(t *testing.T) {
	const issue = "LEGION-208"
	head := strings.Repeat("r", texts.OneAct)
	first, second := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC), time.Date(2026, 11, 24, 9, 45, 0, 0, time.UTC)
	version := 3
	gates := map[record.DesignStep]record.DesignGate{record.DesignStepSpec: {
		Issue: issue, Step: record.DesignStepSpec, ToldChangesVersion: &version, ToldChangesReason: head + " and name the rollback path",
		ToldChangesBy: "user:alice", ToldChangesMark: record.EventMark{Seq: 41, At: second},
	}}
	older, err := record.NoticeOf(ChangesRequestedNotice(issue, record.DesignStepSpec, version, head+" and cut the migration", "user:alice", first))
	if err != nil {
		t.Fatalf("build the older notice: %v", err)
	}
	newer, err := record.NoticeOf(ChangesRequestedNotice(issue, record.DesignStepSpec, version, head+" and name the rollback path", "user:alice", second))
	if err != nil {
		t.Fatalf("build the newer notice: %v", err)
	}
	if older.Reason != newer.Reason {
		t.Fatalf("premise: the two notices carry different reasons; want the same cut head")
	}
	if !ChangesSuperseded(gates, issue, older) {
		t.Errorf("the older changes request, made at %s, reads as the one the gate told (made at %s): it would be relayed and kept in place of the newer one", first, second)
	}
	if ChangesSuperseded(gates, issue, newer) {
		t.Errorf("the told changes request reads as replaced")
	}
}
