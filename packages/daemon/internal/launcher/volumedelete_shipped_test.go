package launcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The removal the deleter ships with, raced in the deleter it ships in.
// TestTheDeletersRemovalFollowsNoLinkWithNoContainment races removeEntryAt itself, and the
// containment cases give the deleter the removal it made before (useFollowingContainedDeleter), so
// neither reads back what the deleter the pod runs does: a deleter whose removeListedEntry
// followed a link again would leave every one of them green. This case runs the contained deleter
// with the removal it ships with (useContainedDeleter) and aims the swap at a directory no cover
// hides — a workspace on the volume, which the deleter sees as the container mounted it — so the
// removal alone is what keeps the link from being followed there.
//
// A trial counts when the swap was made while the removal ran (raceTheSwap renames the
// subdirectory only once the removal has begun emptying it). A following removal then deletes in
// the workspace; the shipped one finishes the directory it holds open and meets the link as a
// name.
//
// The image's own paths point at a tree of the case's (useRoleSystemRoot), as the containment
// cases' do: the deleter covers the container's /tmp, and a runner whose TMPDIR is /tmp keeps the
// fixture's volume there, which the deleter would otherwise refuse to cover and end on.
func TestTheShippedDeletersRemovalFollowsNoLinkIntoTheVolume(t *testing.T) {
	requireRoleNamespaces(t)
	f := newTrimFixture(t, 1)
	useRoleSystemRoot(t, f.cover, filepath.Join(f.coverRoot, "system"))
	useContainedDeleter(t)
	at := f.clock.at.Add(-2 * time.Hour)
	workspace := filepath.Join(t.TempDir(), "workspaces", "repo")
	bait := plantBait(t, workspace, at, "src.go", "pkg/lib.go", "README.md")
	victim := filepath.Join(f.roots[3].Path, "racer-victim")
	sub := filepath.Join(victim, "sub")
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	made, trials := 0, 0
	deadline := time.Now().Add(testwait.Bounded(t, 2*time.Minute, 10*time.Second))
	for made < 3 && trials < 8 && time.Now().Before(deadline) {
		trials++
		buildRacedEntry(t, victim, sub, at)
		swapped := make(chan bool, 1)
		go raceTheSwap(sub, workspace, time.Now().Add(testwait.Bounded(t, 30*time.Second, time.Second)), swapped)
		tr := f.trimmer()
		tr.removals = startRemover // the contained deleter, with the removal it ships with
		_ = tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
		if <-swapped {
			made++
		}
		assertBaitHeld(t, bait, fmt.Sprintf("the shipped deleter's removal followed a link a role left where a subdirectory of the entry stood, into %s", workspace))
		for _, left := range []string{victim, sub + ".real"} {
			if err := os.RemoveAll(left); err != nil {
				t.Fatal(err)
			}
		}
		if t.Failed() {
			t.Fatalf("the case ends at this trial: %d of %d swaps were made while the removal ran", made, trials)
		}
	}
	t.Logf("the swap to %s was made while the removal ran in %d of %d trials", workspace, made, trials)
	if made == 0 {
		t.Fatalf("no swap was made while the removal ran in %d trials, so this case shows nothing", trials)
	}
}
