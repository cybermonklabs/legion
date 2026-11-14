package runtime

import (
	"testing"
	"time"
)

// The decision between an issue's two saved copies, read as a table over every rule it keeps: a
// copy the issue does not hold, whose one attempt is spent or whose work came off the pod asking
// is answered nothing; the newer of the copies the issue holds is offered and the older
// superseded, a tie going to the release bundle, whether or not the newer can be sent this pod;
// a running copy older than the newest recorded push is refused, offered or not, and a release
// bundle never is; and the offered copy is sent to a pod created after it, read to its own
// second, and refused to any other.
func TestDecideWorkspaceRestoreAnswersEachCopyByEveryRuleOfTheRestore(t *testing.T) {
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	const released, cameBack, asking = "pod-the-work-came-off", "pod-that-came-back", "pod-asking"
	held := func(at time.Time) WorkspaceCopyHeld { return WorkspaceCopyHeld{At: at, Source: released} }
	spent := func(at time.Time) WorkspaceCopyHeld { return WorkspaceCopyHeld{At: at, Spent: true, Source: released} }
	fromThisPod := func(at time.Time) WorkspaceCopyHeld { return WorkspaceCopyHeld{At: at, Source: asking} }
	// A running copy off a pod other than the one the release bundle came off: a pod that came
	// back without restoring the bundle, which is the newer-copy rule's case.
	elsewhere := func(at time.Time) WorkspaceCopyHeld { return WorkspaceCopyHeld{At: at, Source: cameBack} }
	spentElsewhere := func(at time.Time) WorkspaceCopyHeld { return WorkspaceCopyHeld{At: at, Spent: true, Source: cameBack} }
	pushed := WorkspacePush{At: at.Add(time.Minute), Head: "c2"}
	// A pod created long after every copy below, unless a case names its own creation.
	later := at.Add(time.Hour)
	for _, c := range []struct {
		name                    string
		bundle, running         WorkspaceCopyHeld
		push                    WorkspacePush
		created                 time.Time
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
		{name: "a release bundle older than the push is still refused to a pod older than it", bundle: held(at), push: pushed, created: at.Add(-time.Minute),
			wantBundle: WorkspaceBundleRefusedOlder, wantRunning: WorkspaceBundleNone, wantOffered: WorkspaceCopyReleaseBundle},
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
		{name: "a pod created inside the copy's own second", running: held(at.Add(999 * time.Millisecond)), created: at,
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleSend, wantOffered: WorkspaceCopyRunning},
		{name: "a pod created the second before the copy's", running: held(at.Add(time.Second)), created: at.Add(999 * time.Millisecond),
			wantBundle: WorkspaceBundleNone, wantRunning: WorkspaceBundleRefusedOlder, wantOffered: WorkspaceCopyRunning},
		{name: "the newer copy refused by the clock still supersedes the older", bundle: held(at), running: elsewhere(at.Add(time.Minute)), created: at.Add(30 * time.Second),
			wantBundle: WorkspaceBundleSuperseded, wantRunning: WorkspaceBundleRefusedOlder, wantOffered: WorkspaceCopyRunning},
	} {
		t.Run(c.name, func(t *testing.T) {
			created := c.created
			if created.IsZero() {
				created = later
			}
			bundle, running, offered := DecideWorkspaceRestore(c.bundle, c.running, c.push, asking, created)
			if bundle != c.wantBundle || running != c.wantRunning || offered != c.wantOffered {
				t.Fatalf("DecideWorkspaceRestore = (%v, %v, %q), want (%v, %v, %q)",
					bundle, running, offered, c.wantBundle, c.wantRunning, c.wantOffered)
			}
		})
	}
}
