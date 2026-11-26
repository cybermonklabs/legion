package sandbox

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// A launcher's hello says whether it waits for a start's credential, and the runtime answers it for
// the slot (launchers.credentialWaits): a launcher of an image before that wait answers its pod's
// uid and false, which a re-role into its slot waits out; a launcher that waits answers true; and a
// slot with no launcher connected, or one whose connection closed, answers true, since the launch
// then builds or waits for a pod of this release.
func TestASlotsLauncherSaysWhetherItWaitsForTheCredential(t *testing.T) {
	launchers := testLaunchers()
	if pod, waits := launchers.credentialWaits(workerToken); pod != "" || !waits {
		t.Fatalf("with no launcher connected = %q, %t; want none, waits", pod, waits)
	}
	old := launchers.accept(workerToken, testTree, time.Time{}, shimwire.LauncherHello{PodUID: "pod-1", LauncherID: "old"})
	if pod, waits := launchers.credentialWaits(workerToken); pod != "pod-1" || waits {
		t.Fatalf("a launcher that said no credential wait = %q, %t; want pod-1, no wait", pod, waits)
	}
	launchers.accept(workerToken, testTree, time.Time{}, shimwire.LauncherHello{PodUID: "pod-2", LauncherID: "new", CredentialWait: true})
	if pod, waits := launchers.credentialWaits(workerToken); pod != "pod-2" || !waits {
		t.Fatalf("a launcher that waits = %q, %t; want pod-2, waits", pod, waits)
	}
	if !old.closed {
		t.Fatal("the earlier launcher's connection is still open: the case's premise")
	}
	launchers.accept(workerToken, testTree, time.Time{}, shimwire.LauncherHello{PodUID: "pod-3", LauncherID: "newest"}).close()
	if pod, waits := launchers.credentialWaits(workerToken); pod != "" || !waits {
		t.Fatalf("after the launcher's connection closed = %q, %t; want none, waits", pod, waits)
	}
}
