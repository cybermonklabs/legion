package runtime

import (
	"testing"
	"time"
)

// The decision between an issue's two saved copies, read as a table: a copy older than the newest
// recorded push is refused whichever kind it is, the newer of the rest is offered and the older
// superseded, a tie goes to the release bundle, and an issue with no push recorded refuses nothing
// by the push rule.
func TestDecideWorkspaceRestoreRefusesEveryCopyOlderThanThePushAndOffersTheNewerOfTheRest(t *testing.T) {
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	held := func(at time.Time) WorkspaceCopyHeld { return WorkspaceCopyHeld{Held: true, At: at} }
	pushed := WorkspacePush{At: at.Add(time.Minute), Head: "c2"}
	for _, c := range []struct {
		name              string
		bundle, running   WorkspaceCopyHeld
		push              WorkspacePush
		wantBundle, wantR WorkspaceCopyAct
	}{
		{"nothing held", WorkspaceCopyHeld{}, WorkspaceCopyHeld{}, pushed, WorkspaceCopyNotHeld, WorkspaceCopyNotHeld},
		{"a running copy alone, no push", WorkspaceCopyHeld{}, held(at), WorkspacePush{}, WorkspaceCopyNotHeld, WorkspaceCopyOffered},
		{"a running copy alone, older than the push", WorkspaceCopyHeld{}, held(at), pushed, WorkspaceCopyNotHeld, WorkspaceCopyRefusedPushed},
		{"a running copy taken after the push", WorkspaceCopyHeld{}, held(pushed.At.Add(time.Second)), pushed, WorkspaceCopyNotHeld, WorkspaceCopyOffered},
		{"a running copy taken at the push's own instant", WorkspaceCopyHeld{}, held(pushed.At), pushed, WorkspaceCopyNotHeld, WorkspaceCopyOffered},
		{"an earlier release's bundle older than the push", held(at.Add(-time.Hour)), WorkspaceCopyHeld{}, pushed, WorkspaceCopyRefusedPushed, WorkspaceCopyNotHeld},
		{"both older than the push", held(at.Add(-time.Hour)), held(at), pushed, WorkspaceCopyRefusedPushed, WorkspaceCopyRefusedPushed},
		{"the bundle older than the push, the copy after it", held(at.Add(-time.Hour)), held(pushed.At.Add(time.Second)), pushed, WorkspaceCopyRefusedPushed, WorkspaceCopyOffered},
		{"the running copy newer, no push", held(at), held(at.Add(time.Minute)), WorkspacePush{}, WorkspaceCopySuperseded, WorkspaceCopyOffered},
		{"the bundle newer, no push", held(at.Add(time.Minute)), held(at), WorkspacePush{}, WorkspaceCopyOffered, WorkspaceCopySuperseded},
		{"a tie, no push", held(at), held(at), WorkspacePush{}, WorkspaceCopyOffered, WorkspaceCopySuperseded},
	} {
		t.Run(c.name, func(t *testing.T) {
			bundle, running := DecideWorkspaceRestore(c.bundle, c.running, c.push)
			if bundle != c.wantBundle || running != c.wantR {
				t.Fatalf("DecideWorkspaceRestore = (%v, %v), want (%v, %v)", bundle, running, c.wantBundle, c.wantR)
			}
		})
	}
}
