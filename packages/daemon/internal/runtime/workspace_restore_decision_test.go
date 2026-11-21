package runtime

import (
	"testing"
	"time"
)

// The decision between an issue's two saved copies, read as a table over every rule it keeps: a
// copy the issue does not hold, that is spent or whose work came off the pod asking
// is answered nothing; the newer of the copies the issue holds, by when each take began, is
// offered and the older superseded, a tie going to the release bundle, whether or not the newer
// can be sent this pod; a running copy whose take began before the newest recorded push is
// refused, offered or not, however late it was written, and a release bundle never is; and the
// offered copy is sent to a pod whose first ask is at or after the copy's write, and left for the
// next pod, with nothing recorded, by a pod that first asked before it, whenever its take began.
func TestDecideWorkspaceRestoreAnswersEachCopyByEveryRuleOfTheRestore(t *testing.T) {
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	const released, cameBack, asking = "pod-the-work-came-off", "pod-that-came-back", "pod-asking"
	// Each of these copies was taken and written at the one time, unless a case names both.
	held := func(at time.Time) WorkspaceCopyHeld { return WorkspaceCopyHeld{At: at, Written: at, Source: released} }
	spent := func(at time.Time) WorkspaceCopyHeld {
		return WorkspaceCopyHeld{At: at, Written: at, Spent: true, Source: released}
	}
	fromThisPod := func(at time.Time) WorkspaceCopyHeld { return WorkspaceCopyHeld{At: at, Written: at, Source: asking} }
	// A running copy off a pod other than the one the release bundle came off: a pod that came
	// back without restoring the bundle, which is the newer-copy rule's case.
	elsewhere := func(at time.Time) WorkspaceCopyHeld { return WorkspaceCopyHeld{At: at, Written: at, Source: cameBack} }
	spentElsewhere := func(at time.Time) WorkspaceCopyHeld {
		return WorkspaceCopyHeld{At: at, Written: at, Spent: true, Source: cameBack}
	}
	// A copy whose take began at taken and whose write landed at written.
	takenThenWritten := func(taken, written time.Time) WorkspaceCopyHeld {
		return WorkspaceCopyHeld{At: taken, Written: written, Source: released}
	}
	pushed := WorkspacePush{At: at.Add(time.Minute), Head: "c2"}
	// A pod that first asked long after every copy below, unless a case names its own first ask.
	later := at.Add(time.Hour)
	for _, c := range []struct {
		name                    string
		bundle, running         WorkspaceCopyHeld
		push                    WorkspacePush
		firstAsked              time.Time
		wantBundle, wantRunning WorkspaceBundleTake
		wantOffered             WorkspaceCopyKind
	}{
		{name: "nothing held", push: pushed,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone},
		{name: "a running copy alone, no push", running: held(at),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "a release bundle alone, no push", bundle: held(at),
			wantBundle: WorkspaceBundleSend, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyReleaseBundle},
		{name: "a running copy alone, older than the push", running: held(at), push: pushed,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleRefusedPushed, wantOffered: WorkspaceCopyRunning},
		{name: "a running copy taken after the push", running: held(pushed.At.Add(time.Second)), push: pushed,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "a running copy taken at the push's own instant", running: held(pushed.At), push: pushed,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "a release bundle older than the push is sent", bundle: held(at), push: pushed,
			wantBundle: WorkspaceBundleSend, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyReleaseBundle},
		{name: "a release bundle older than the push is still left for the next pod by a pod that first asked before it", bundle: held(at), push: pushed, firstAsked: at.Add(-time.Minute),
			wantBundle: WorkspaceBundleLeftForNextPod, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyReleaseBundle},
		{name: "the bundle older than the push, the copy after it, which supersedes it", bundle: held(at.Add(-time.Hour)), running: elsewhere(pushed.At.Add(time.Second)), push: pushed,
			wantBundle: WorkspaceBundleSuperseded, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "both older than the push: the refused copy is the newer and supersedes the bundle", bundle: held(at.Add(-time.Hour)), running: elsewhere(at), push: pushed,
			wantBundle: WorkspaceBundleSuperseded, wantRunning: WorkspaceBundleRefusedPushed, wantOffered: WorkspaceCopyRunning},
		{name: "both older than the push: the bundle is the newer and is sent, the copy refused", bundle: held(at), running: elsewhere(at.Add(-time.Hour)), push: pushed,
			wantBundle: WorkspaceBundleSend, wantRunning: WorkspaceBundleRefusedPushed, wantOffered: WorkspaceCopyReleaseBundle},
		{name: "the running copy newer, no push", bundle: held(at), running: elsewhere(at.Add(time.Minute)),
			wantBundle: WorkspaceBundleSuperseded, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "the bundle newer, no push", bundle: held(at.Add(time.Minute)), running: elsewhere(at),
			wantBundle: WorkspaceBundleSend, wantRunning: WorkspaceBundleSuperseded, wantOffered: WorkspaceCopyReleaseBundle},
		{name: "a tie goes to the release bundle", bundle: held(at), running: elsewhere(at),
			wantBundle: WorkspaceBundleSend, wantRunning: WorkspaceBundleSuperseded, wantOffered: WorkspaceCopyReleaseBundle},
		{name: "a copy whose attempt is spent", running: spent(at),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyRunning},
		{name: "the pod the work came off", running: fromThisPod(at),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyRunning},
		{name: "a spent copy older than the push records no refusal", running: spent(at), push: pushed,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyRunning},
		{name: "the pod the work came off records nothing of a bundle older than the push", bundle: fromThisPod(at), push: pushed,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyReleaseBundle},
		{name: "the pod the work came off records no refusal of a copy older than the push", running: fromThisPod(at), push: pushed,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyRunning},
		{name: "the newer copy spent still supersedes the older", bundle: held(at), running: spentElsewhere(at.Add(time.Minute)),
			wantBundle: WorkspaceBundleSuperseded, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyRunning},
		{name: "the older copy off this very pod has nothing recorded", bundle: fromThisPod(at), running: held(at.Add(time.Minute)),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "the newer copy of this very pod supersedes the older", bundle: held(at), running: fromThisPod(at.Add(time.Minute)),
			wantBundle: WorkspaceBundleSuperseded, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyRunning},
		{name: "the older copy spent has nothing recorded", bundle: spent(at), running: elsewhere(at.Add(time.Minute)),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "a pod that first asked at the copy's own instant", running: held(at), firstAsked: at,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "a pod that first asked a moment after the copy was written", running: held(at), firstAsked: at.Add(time.Microsecond),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "a late copy, written a moment after the pod first asked", running: held(at.Add(time.Microsecond)), firstAsked: at,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleLeftForNextPod, wantOffered: WorkspaceCopyRunning},
		{name: "a late release bundle", bundle: held(at.Add(time.Minute)), firstAsked: at,
			wantBundle: WorkspaceBundleLeftForNextPod, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyReleaseBundle},
		{name: "a late copy older than the push is refused by the push", running: held(at), push: pushed, firstAsked: at.Add(-time.Minute),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleRefusedPushed, wantOffered: WorkspaceCopyRunning},
		{name: "a late copy off this very pod has nothing recorded", running: fromThisPod(at.Add(time.Minute)), firstAsked: at,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyRunning},
		{name: "a late copy already spent has nothing recorded", running: spent(at.Add(time.Minute)), firstAsked: at,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyRunning},
		{name: "the newer copy left for the next pod still supersedes the older", bundle: held(at), running: elsewhere(at.Add(time.Minute)), firstAsked: at.Add(30 * time.Second),
			wantBundle: WorkspaceBundleSuperseded, wantRunning: WorkspaceBundleLeftForNextPod, wantOffered: WorkspaceCopyRunning},
		{name: "a late copy whose take began before the pod first asked is left for the next pod", running: takenThenWritten(at.Add(-time.Second), at.Add(time.Second)), firstAsked: at,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleLeftForNextPod, wantOffered: WorkspaceCopyRunning},
		{name: "a late release bundle whose save's take began before the pod first asked", bundle: takenThenWritten(at.Add(-time.Second), at.Add(time.Second)), firstAsked: at,
			wantBundle: WorkspaceBundleLeftForNextPod, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyReleaseBundle},
		{name: "a copy written before the pod first asked is sent however long before its take began", running: takenThenWritten(at.Add(-time.Hour), at), firstAsked: at.Add(time.Microsecond),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "a copy whose take began before the push and whose write landed after it is refused", running: takenThenWritten(pushed.At.Add(-time.Second), pushed.At.Add(time.Second)), push: pushed,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleRefusedPushed, wantOffered: WorkspaceCopyRunning},
		{name: "a copy whose take began at the push's own instant is sent", running: takenThenWritten(pushed.At, pushed.At.Add(time.Second)), push: pushed,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "the newer copy is the one whose take began later, whichever was written later", bundle: takenThenWritten(at, at.Add(10*time.Minute)), running: WorkspaceCopyHeld{At: at.Add(time.Minute), Written: at.Add(2 * time.Minute), Source: cameBack},
			wantBundle: WorkspaceBundleSuperseded, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
	} {
		t.Run(c.name, func(t *testing.T) {
			firstAsked := c.firstAsked
			if firstAsked.IsZero() {
				firstAsked = later
			}
			bundle, running, offered := DecideWorkspaceRestore(c.bundle, c.running, c.push, asking, firstAsked)
			if bundle != c.wantBundle || running != c.wantRunning || offered != c.wantOffered {
				t.Fatalf("DecideWorkspaceRestore = (%v, %v, %q), want (%v, %v, %q)",
					bundle, running, offered, c.wantBundle, c.wantRunning, c.wantOffered)
			}
		})
	}
}

// The drop of the copies a pod's kept volume holds, read as a table over every rule it keeps: a
// copy whose take began in a second after the pod's Sandbox was made came off that volume and is
// dropped, the release bundle and the running copy alike; a copy taken before, or in the
// Sandbox's own second, came off an earlier volume or cannot be ordered against it, and is left to
// the take; a copy the take would answer nothing of — spent, or off the pod asking — is dropped by
// no volume; a pod with no kept volume drops nothing; and where the newer copy is dropped, an
// older one the drop leaves and the take would offer is closed beside it, as the take closes the
// older of two.
func TestDecideKeptVolumeDropAnswersEachCopyByEveryRuleOfTheDrop(t *testing.T) {
	made := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	const released, cameBack, asking = "pod-the-work-came-off", "pod-that-came-back", "pod-asking"
	held := func(at time.Time, source string) WorkspaceCopyHeld {
		return WorkspaceCopyHeld{At: at, Written: at, Source: source}
	}
	spent := func(at time.Time) WorkspaceCopyHeld {
		return WorkspaceCopyHeld{At: at, Written: at, Spent: true, Source: released}
	}
	after, longAfter := made.Add(10*time.Minute), made.Add(20*time.Minute)
	before, longBefore := made.Add(-10*time.Minute), made.Add(-20*time.Minute)
	for _, c := range []struct {
		name                    string
		bundle, running         WorkspaceCopyHeld
		noKeptVolume            bool
		wantBundle, wantRunning WorkspaceBundleTake
		wantNewer               WorkspaceCopyKind
	}{
		{name: "nothing held",
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone},
		{name: "a release bundle taken off this volume", bundle: held(after, released),
			wantBundle: WorkspaceBundleHeldByVolume, wantRunning: WorkspaceBundleNone, wantNewer: WorkspaceCopyReleaseBundle},
		{name: "a running copy taken off this volume", running: held(after, cameBack),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleHeldByVolume, wantNewer: WorkspaceCopyRunning},
		{name: "a release bundle of an earlier volume", bundle: held(before, released),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantNewer: WorkspaceCopyReleaseBundle},
		{name: "a running copy of an earlier volume", running: held(before, cameBack),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantNewer: WorkspaceCopyRunning},
		{name: "a copy taken in the Sandbox's own second", running: held(made.Add(999*time.Millisecond), cameBack),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantNewer: WorkspaceCopyRunning},
		{name: "a copy taken in the second after the Sandbox's", running: held(made.Add(time.Second), cameBack),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleHeldByVolume, wantNewer: WorkspaceCopyRunning},
		{name: "a pod with no kept volume", bundle: held(after, released), running: held(longAfter, cameBack), noKeptVolume: true,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantNewer: WorkspaceCopyRunning},
		{name: "a spent copy off this volume", running: spent(after),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantNewer: WorkspaceCopyRunning},
		{name: "a copy off the pod asking", bundle: held(after, asking),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantNewer: WorkspaceCopyReleaseBundle},
		{name: "both off this volume", bundle: held(after, released), running: held(longAfter, cameBack),
			wantBundle: WorkspaceBundleHeldByVolume, wantRunning: WorkspaceBundleHeldByVolume, wantNewer: WorkspaceCopyRunning},
		{name: "both of earlier volumes", bundle: held(longBefore, released), running: held(before, cameBack),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantNewer: WorkspaceCopyRunning},
		{name: "the newer running copy off this volume closes the older bundle", bundle: held(before, released), running: held(after, cameBack),
			wantBundle: WorkspaceBundleSuperseded, wantRunning: WorkspaceBundleHeldByVolume, wantNewer: WorkspaceCopyRunning},
		{name: "the newer bundle off this volume closes the older running copy", bundle: held(after, released), running: held(before, cameBack),
			wantBundle: WorkspaceBundleHeldByVolume, wantRunning: WorkspaceBundleSuperseded, wantNewer: WorkspaceCopyReleaseBundle},
		{name: "the older copy spent is not closed again", bundle: spent(before), running: held(after, cameBack),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleHeldByVolume, wantNewer: WorkspaceCopyRunning},
		{name: "the older copy off the pod asking is not closed", bundle: held(before, asking), running: held(after, cameBack),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleHeldByVolume, wantNewer: WorkspaceCopyRunning},
		{name: "the newer copy spent leaves the older to the take", bundle: held(before, released), running: WorkspaceCopyHeld{At: after, Written: after, Spent: true, Source: cameBack},
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleNone, wantNewer: WorkspaceCopyRunning},
		{name: "the newer copy off the pod asking leaves an older one off this volume dropped", bundle: held(after, released), running: held(longAfter, asking),
			wantBundle: WorkspaceBundleHeldByVolume, wantRunning: WorkspaceBundleNone, wantNewer: WorkspaceCopyRunning},
	} {
		t.Run(c.name, func(t *testing.T) {
			keptSince := made
			if c.noKeptVolume {
				keptSince = time.Time{}
			}
			bundle, running, newer := DecideKeptVolumeDrop(c.bundle, c.running, asking, keptSince)
			if bundle != c.wantBundle || running != c.wantRunning || newer != c.wantNewer {
				t.Fatalf("DecideKeptVolumeDrop = (%v, %v, %q), want (%v, %v, %q)",
					bundle, running, newer, c.wantBundle, c.wantRunning, c.wantNewer)
			}
		})
	}
}
