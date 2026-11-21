package supervise

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Where the plain sentence of a death stands in the role's next task (dispatch://INFRA-1117 R9,
// Verification checks k19 and k20): always before the runtime's own detail. Where the death cut the
// task's turn off, it follows main's line that the previous turn on this task was interrupted. Where
// it cut none off, the task opens with it, and main's sentence that the role was in no turn follows
// the runtime's detail, as main's relaunchSentence composes it. For a disk eviction the sentence on
// $TMPDIR stands between the cause and the runtime's detail. Each cause the daemon tells in plain
// words is walked both ways: a death in a turn of the task, and a death of a ready role holding its
// task that no turn has confirmed yet. TestPlainCauseADeathForMemoryOrDiskIsToldInTheRolesNextTaskBeforeTheRuntimesDetail
// and TestPlainCauseADeathWithATaskNoTurnConfirmedIsToldInTheNextTask hold the words; this test
// holds their order.
func TestPlainCauseTheCauseSentenceStandsBeforeTheRuntimesDetailWhereverTheTaskOpens(t *testing.T) {
	const detail = "pod legion-legion-legion-208 (uid uid-pod-1) Failed: container roles terminated (Error, exit code 137)"
	const disk = "Keep large files under $TMPDIR, on the issue's volume."
	for _, cause := range []struct {
		name  string
		cause runtime.DeathCause
		lead  string
	}{
		{
			name:  "the roles container killed for memory",
			cause: runtime.DeathCause{Kind: runtime.DeathMemoryLimit, Limit: 19 << 30},
			lead:  "the pod's roles container, which runs every role of this issue, was killed because it used more than its 19 GiB memory limit.",
		},
		{
			name: "a node-pressure eviction of the pod's own memory use",
			cause: runtime.DeathCause{Kind: runtime.DeathMemoryRequest, Container: runtime.IssuePodContainer,
				Use: 9437188 << 10, Request: 8 << 30},
			lead: "the kubelet evicted the pod for node memory pressure because its roles container was using 9 GiB, more than its 8 GiB memory request.",
		},
		{
			name:  "an eviction for the pod's own disk",
			cause: runtime.DeathCause{Kind: runtime.DeathDiskLimit, Limit: 10 << 30},
			lead:  "the kubelet evicted the pod because its container's own disk, outside $TMPDIR, passed its 10 GiB limit.",
		},
	} {
		for _, inTurn := range []bool{true, false} {
			walk := "in no turn"
			opening := "Your previous process ended because " + cause.lead
			if inTurn {
				walk = "in a turn of the task"
				opening = "Your previous turn on this task was interrupted. Your process ended because " + cause.lead
			}
			t.Run(cause.name+"/"+walk, func(t *testing.T) {
				h := newHarness(t)
				h.reach(StateReady)
				h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", Generation: 5})
				if inTurn {
					h.must(StreamTurnStart{Claim: testToken})
				}
				h.must(RuntimeObservation{Observation: runtime.Observation{
					Locator: h.locator(), Kind: runtime.Gone, At: h.clock.Now(), Detail: detail, Cause: cause.cause,
					OutOfMemory: cause.cause.Kind == runtime.DeathMemoryLimit,
				}})
				h.relaunched()

				prompts := h.wantPrompts(2)
				task := prompts[len(prompts)-1].Message
				if !strings.HasPrefix(task, opening) {
					t.Fatalf("the relaunched role's task is %q, want it to open with %q", task, opening)
				}
				lead := strings.Index(task, cause.lead)
				runtimeDetail := strings.Index(task, "The runtime's detail:")
				if runtimeDetail < 0 || lead > runtimeDetail {
					t.Errorf("the relaunched role's task carries the cause at %d and the runtime's detail at %d, want the cause first: %q",
						lead, runtimeDetail, task)
				}
				if tmpdir := strings.Index(task, disk); cause.cause.Kind == runtime.DeathDiskLimit &&
					(tmpdir < lead || tmpdir > runtimeDetail) {
					t.Errorf("the relaunched role's task says %q at %d, want it between the cause at %d and the runtime's detail at %d: %q",
						disk, tmpdir, lead, runtimeDetail, task)
				}
				if !inTurn {
					if noTurn := strings.Index(task, "You were not in a turn then."); noTurn < runtimeDetail {
						t.Errorf("the relaunched role's task says that it was in no turn at %d, want it after the runtime's detail at %d: %q",
							noTurn, runtimeDetail, task)
					}
				}
			})
		}
	}
}
