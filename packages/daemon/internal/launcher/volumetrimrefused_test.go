//go:build linux

package launcher

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The file builds only under Linux because the case drives the launcher through
// volumetrimlifecycle_test.go's helpers, which do.

// R2 has the trimmer refuse a value outside its ranges, log the key and the value and stay off,
// and the settings in force are the bound connection's latest answer alone. So an answer it
// refuses that arrives while a sweep runs stops that sweep as an answer with no settings does: at
// its next entry boundary, with its deletions standing and no report sent for it, and the
// trimmer then waits for settings it can run rather than returning. Whether the answer is one the
// trimmer can run is the launcher's own manager's to say (manager.applyAck, volumeTrimming), so
// the case drives the real launcher.
func TestARefusedAnswerStopsTheSweepInFlight(t *testing.T) {
	f := newTrimFixture(t, 25) // a hundred entries, against a volume that never reaches the low mark
	f.useVolume(steady(90))
	f.setSettings(trimSettings())
	trims := useLauncherTrimmer(t, f)

	pod := newFleetDialed(t, 2*time.Second, podRoles...)
	ackPod(pod, trimSettings())
	trims.await()

	// The sweep is blocked where it stands, with seven entries deleted.
	const deleted = 7
	sweeping, release := make(chan struct{}), make(chan struct{})
	releaseSweep := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseSweep)
	blocked := false
	f.clock.setHook(func(*fakeClock) {
		if blocked || len(f.gone()) < deleted {
			return
		}
		blocked = true
		close(sweeping)
		<-release
	})
	offerTick(t, f)
	awaitClosed(t, sweeping, "the sweep to reach its seventh deletion")

	wide := *trimSettings()
	wide.HighPercent = shimwire.VolumeMarkMax + 1
	_ = pod.role(podRoles[0]).conn.Close()
	again := acceptRole(t, pod.listener)
	again.acknowledgeWith(shimwire.LauncherHelloAck{VolumeTrim: &wide})
	pod.conns[podRoles[0]] = again
	releaseSweep()

	testwait.Eventually(t, "the trimmer to stop the sweep on settings it refuses", func() bool {
		return f.logged("the settings stopped the trimmer: the sweep ended at an entry boundary with its deletions standing, and no report is sent for it") == 1
	})
	testwait.Eventually(t, "the trimmer to name the key and the value it refuses", func() bool {
		return f.logged(fmt.Sprintf("high_percent is %d", wide.HighPercent)) == 1
	})
	if trims.hasStopped() {
		t.Error("the trimmer's loop returned on an answer it refuses, want it waiting for settings it can run")
	}
	// The entry the sweep was removing when the answer arrived is never interrupted, so the
	// deletions that stand are the seven it had made and that one.
	if gone := len(f.gone()); gone != deleted+1 {
		t.Errorf("%d entries are gone, want the %d that stood when the refused answer stopped the sweep", gone, deleted+1)
	}
	again.refuseVolumeReport(500 * time.Millisecond)
	f.assertNothingHalfRemoved()
	f.assertProtected()
}
