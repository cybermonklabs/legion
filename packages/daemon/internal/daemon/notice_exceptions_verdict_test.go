package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A phase-finished notice the release before this one published named the tester's verdict under
// the key verdict, which record.Notice no longer has. Migration 20261125000602_phase_verdict_gone
// strips the key from every notice still queued, but a listener reports a failed forward with the
// payload as it went out, so a notice published before the upgrade and reported failed after it
// carries the key. The daemon queues that notice again without it, as the migration leaves a
// queued one, and the architect still gets its phase-finished notice. A key Notice never had still
// refuses the report, so the decode stays strict for everything else.
func TestAFailedForwardOfANoticeThatNamesAVerdictIsQueuedAgainWithoutIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra string
		want  []string
	}{
		{"a verdict the release before published", "verdict",
			[]string{`LEGION-2 {"kind": "phase-finished", "role": "tester", "phase": "testing", "resends": 1, "summary": "tested the head", "version": 3, "resend_of": 7}`}},
		{"control: a key no release published", "verdicts", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaimOn(t, sup, "LEGION-1", supervise.StateWorking, "ses_live")
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", now: time.Now}
			published := map[string]any{"kind": "phase-finished", "role": "tester", "phase": "testing", "summary": "tested the head", "version": 3, tc.extra: "fail"}
			report := laneReport{"evt-exception", "delivery_failed", architectTopic(t, "LEGION-1"), "phase-finished on LEGION-2", published, "legion-outbox:7", "ses_live"}

			if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
				t.Fatalf("rehold: %v", err)
			}
			if got := queuedNotices(t, pool); len(got) != len(tc.want) || len(got) == 1 && got[0] != tc.want[0] {
				t.Fatalf("queued %v, want %v", got, tc.want)
			}
		})
	}
}
