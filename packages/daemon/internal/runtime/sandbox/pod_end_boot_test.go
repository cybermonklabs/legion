package sandbox

import (
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A pod the cluster ended while no daemon ran is read at the next boot, when the informers sync,
// and that is before the boot's reconciliation adopts the claims that ran in it (ReconcileOrphans):
// the reading is made with no claim of the pod watched, which writes no line (logEndReading). The
// reconciliation writes the line once it has adopted those claims (tellEndReadings), naming the
// pod, the reading and the roles the end ended, once: a second reconciliation writes nothing more.
//
// The control is ReconcileOrphans without tellEndReadings: the end of that pod is then in no line
// at all, and the test fails at the line it wants after the reconciliation.
func TestAPodEndReadAtBootIsLoggedOnceItsClaimsAreAdopted(t *testing.T) {
	g := newRig(t, nil)
	loc := g.spawn(workerSpec(t))
	name, uid := loc.Sandbox.Name, loc.Sandbox.PodUID
	// The cluster ends the pod while the daemon is down.
	g.update(g.pod(name), func(p *corev1.Pod) {
		p.Status = endedPodStatus(evictedReason, disruptionTarget("TerminationByKubelet"))
	})
	logs := &lockedLog{}
	secondRuntime(t, g, func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})()
	if end := g.awaitEndReading(uid); end.reading != runtime.PodEndNotOurs {
		t.Fatalf("the restarted runtime read %+v of the pod's end, want it read as %s", end, runtime.PodEndNotOurs)
	}
	if lines := linesHolding(logs.String(), endReadingLine); len(lines) != 0 {
		t.Fatalf("the end was logged %q before any claim of its pod was watched, want no line yet", lines)
	}

	for range 2 {
		if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &loc}}, 0); err != nil {
			t.Fatal(err)
		}
	}

	lines := linesHolding(logs.String(), endReadingLine)
	role, _ := workerToken.Role()
	if len(lines) != 1 || !strings.Contains(lines[0], "uid="+uid) ||
		!strings.Contains(lines[0], "reading="+string(runtime.PodEndNotOurs)) || !strings.Contains(lines[0], string(role)) {
		t.Fatalf("the boot's reconciliation logged the pod's end %q, want one line naming the pod, the reading %s and the %s:\n%s",
			lines, runtime.PodEndNotOurs, role, logs.String())
	}
}
