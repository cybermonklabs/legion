//go:build linux

package launcher

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
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
	f.useVolume(steady(90))
	f.setSettings(trimSettings())

	raceSwaps(t, racedVictim(f), workspace, 3, 8, at, func(_ int, swapped <-chan bool) bool {
		tr := f.trimmer()
		tr.removals = startRemover(&pidGuard{}) // the contained deleter, with the removal it ships with
		_ = tr.sweep(context.Background(), *trimSettings(), steady(90)(0))
		made := <-swapped
		assertBaitHeld(t, bait, fmt.Sprintf("the shipped deleter's removal followed a link a role left where a subdirectory of the entry stood, into %s", workspace))
		return made
	})
}
