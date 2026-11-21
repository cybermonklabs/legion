package daemon

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// A claim the work hold stopped answers every request that could start a turn with the hold's
// wait (supervise.ErrHeldByWorkHold), and two such requests reach its machine posted rather than
// asked: a pause's resume, which the daemon posts to every machine of its project, and the resume
// of a launch that pause owed. The machine's feeder hands each answer to decisionFailed, which
// logs the wait at Info as the wait it is, never at Error as a failed decision, and the claim
// stays the hold's.
func TestWorkHoldLogsNoErrorForARuleDoorOnAHeldClaim(t *testing.T) {
	var logs testlog.Buffer
	rig := newIssueRig(t)
	rig.sup.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	implementer, machine := rig.ready(claim.RoleImplementer, fake.NewConn())
	if err := machine.Handle(rig.ctx, supervise.RequestSuspend{Claim: implementer, By: supervise.SuspendedByHold,
		Reason: "the design gate of LEGION-208 closed"}); err != nil {
		t.Fatalf("the hold's stop of a ready claim: %v", err)
	}
	resumes := len(rig.rt.CallsOf("Resume"))

	rig.sup.post(implementer, supervise.ResumePause{Claim: implementer, Pause: 7})
	rig.sup.post(implementer, supervise.RequestRuleResume{Claim: implementer,
		Reason: "the project's resume: pause 7 stopped it with a launch owed"})
	awaitDrained(t, rig.sup, implementer, "the two posted events")

	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("the daemon logged an error for a request the work hold holds:\n%s", logs.String())
	}
	var waits []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "supervise: the work hold holds the claim; the event waits for the hold's end") {
			waits = append(waits, line)
		}
	}
	if len(waits) != 2 {
		t.Fatalf("the daemon logged the hold's wait %d times, want once per posted event:\n%s", len(waits), logs.String())
	}
	for _, line := range waits {
		if !strings.Contains(line, "level=INFO") {
			t.Fatalf("the daemon logged the hold's wait as %q, want it at Info", line)
		}
	}
	if c := machine.Claim(); c.State != supervise.StateSuspended || c.SuspendedBy != supervise.SuspendedByHold {
		t.Fatalf("the claim is %s suspended by %q, want it still the hold's", c.State, c.SuspendedBy)
	}
	if got := len(rig.rt.CallsOf("Resume")); got != resumes {
		t.Fatalf("the held claim was resumed %d times, want none", got-resumes)
	}
}
