package admit

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root its architect closes before its first phase frees its admission slot once Dispatch shows
// the close the daemon wrote (R24 of dispatch://INFRA-3320: close_root queues the close, and the
// issue leaves its phase at Dispatch's done), and the next waiting root takes it: no human status
// write is needed to end the tree. Each slot change is one journal line naming the issue, the
// slots the project holds after it, and the cap. The keys are issue keys, since the close names
// its claims by them (claim.NewToken).
func TestAClosedRootFreesItsSlotForTheNextWaitingRoot(t *testing.T) {
	pool := migratedPool(t)
	var logged bytes.Buffer
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(&logged, nil)))
	seedSlotted(t, pool, "LEGION-101", "A")
	seedWaiting(t, pool, "LEGION-102", "B")

	result, err := intake.ApplyFact(context.Background(), pool, "api", "close", intake.CloseRoot{Issue: "LEGION-101", Reason: "no change"}, admission.engine, admission)
	if err != nil || result.Refusal != nil {
		t.Fatalf("close = %+v, %v", result.Refusal, err)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-101", Index: 0, AdmittedAt: fixedNow}})
	apply(t, pool, admission, "closed", intake.DispatchIssue{Key: "LEGION-101", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "done", Title: "LEGION-101", Rank: "A", HandedOver: true}, admission.engine)
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-102", Index: 0, AdmittedAt: fixedNow}})
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if strings.Contains(line, `msg="admission: slot `) {
			got = append(got, line[strings.Index(line, "msg="):])
		}
	}
	want := []string{
		`msg="admission: slot released" issue=LEGION-101 slots=0 cap=1`,
		`msg="admission: slot taken" issue=LEGION-102 slots=1 cap=1`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("slot lines\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
