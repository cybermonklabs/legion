package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A pod whose Sandbox owns the issue's kept volume mounts the volume every copy of the issue's
// workspace taken since that Sandbox was made came off: the release keeps the Sandbox and its
// volume (runtime.IssueReleaser), and an eviction or a replacement of the pod keeps them too. So
// the drop (DropHeldWorkspaceCopies) deletes such a copy, the release bundle and the running copy
// alike, and sends that pod nothing of it; a copy taken before the Sandbox was made came off an
// earlier volume, which a reset or a change of the disk's shape replaced, and the take restores
// it as for any pod. The tests here hold both sides for both kinds of copy.

// keptVolumePod is the pod of every drop of these tests that drops a copy: a pod of the Sandbox
// that owns the issue's kept volume, and not the pod any copy came off.
const keptVolumePod = "pod-of-the-kept-sandbox"

// dropFor is the drop at a role start of pod, whose Sandbox owns issue's kept volume and was made
// at keptSince, by a pod of the project that records issue (recordCopyIssues). It fails the test
// on an error.
func dropFor(t *testing.T, s *Store, issue, pod string, keptSince time.Time) runtime.WorkspaceRestore {
	t.Helper()
	recordCopyIssues(t, s, issue)
	dropped, err := s.DropHeldWorkspaceCopies(context.Background(), copyProject, issue, pod, keptSince)
	if err != nil {
		t.Fatalf("the drop of %s for %s: %v", issue, pod, err)
	}
	return dropped
}

// requireRowAsItWas fails the test unless issue's copy in copies' table is still the row was: held,
// taken at the same time, with the same attempt.
func requireRowAsItWas(t *testing.T, s *Store, copies copyTable, issue string, was copyRow) {
	t.Helper()
	row := heldCopyRow(t, s, copies, issue)
	if !row.held || !row.at.Equal(was.at) || row.size != was.size || row.attemptPod != was.attemptPod || row.outcome != was.outcome {
		t.Fatalf("the %s of %s is %+v, want it as it was, %+v", copies.kind, issue, row, was)
	}
}

// A copy taken in a second after the pod's kept Sandbox was made came off that volume, and the
// drop deletes it in one statement that reads none of its body. It answers the copy's restore id,
// its time, its size and the pod its work came off, so the runtime can name them, and nothing is
// left for this pod's take or for the pod after it: a release bundle's row no longer holds the
// issue's pod (workflow.IdleHoldBundleUnrestored), and a running copy is gone with it.
//
// The drop keeps the take's rules: the pod the work came off drops nothing, since its workspace
// is the work and the copy is for the pod that replaces it; and a pod of a project that does not
// record the issue drops nothing and leaves the copy in the database, since the row names no
// project and the copy can be another project's daemon's, saved for its own pod. A drop that
// names no project, no pod, or no creation of the pod's Sandbox has nothing to read the rule by
// and is refused: a zero creation would read as older than every copy.
func TestACopyAKeptVolumeHoldsIsDroppedWithoutItsBody(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	for i, kind := range unspentCopyKinds {
		t.Run(kind.name, func(t *testing.T) {
			issue := []string{"INFRA-3201", "INFRA-3202"}[i]
			saved := unspentCopyWork("PACK the work the kept volume holds")
			saveUnspentCopy(t, s, kind, issue, saved)
			row := heldCopyRow(t, s, kind.copies, issue)
			second := row.at.Truncate(time.Second)

			requireNothing(t, "the pod the work came off", dropFor(t, s, issue, saved.PodUID, second.Add(-time.Hour)).Of(kind.copies.kind))
			if got, err := s.DropHeldWorkspaceCopies(ctx, "legion", issue, "pod-of-legions-spawn", second.Add(-time.Hour)); err != nil || !reflect.DeepEqual(got, runtime.WorkspaceRestore{}) {
				t.Fatalf("legion's pod of %s was answered %+v (%v), want nothing: INFRA records the issue", issue, got, err)
			}
			for _, refused := range []struct {
				name, project, pod string
				keptSince          time.Time
			}{
				{"no project", "", keptVolumePod, second.Add(-time.Hour)},
				{"no pod", copyProject, "", second.Add(-time.Hour)},
				{"no creation of the Sandbox", copyProject, keptVolumePod, time.Time{}},
			} {
				if got, err := s.DropHeldWorkspaceCopies(ctx, refused.project, issue, refused.pod, refused.keptSince); err == nil {
					t.Fatalf("a drop that named %s was answered %+v, want it refused", refused.name, got)
				}
			}
			requireRowAsItWas(t, s, kind.copies, issue, row)

			// The next pod of the kept Sandbox, made in the second before the copy was taken.
			dropped := dropFor(t, s, issue, keptVolumePod, second.Add(-time.Second)).Of(kind.copies.kind)
			if dropped.Take != runtime.WorkspaceBundleHeldByVolume || !dropped.Bundle.At.Equal(row.at) || dropped.Size != len(saved.Bundle) ||
				dropped.Bundle.PodUID != saved.PodUID {
				t.Fatalf("the drop answered %v of the copy taken at %s, %d bytes off %s, want %v of the copy taken at %s, %d bytes off %s",
					dropped.Take, dropped.Bundle.At, dropped.Size, dropped.Bundle.PodUID, runtime.WorkspaceBundleHeldByVolume, row.at, len(saved.Bundle), saved.PodUID)
			}
			if len(dropped.Bundle.Bundle) != 0 {
				t.Fatalf("the drop read %d bytes of the copy, want none", len(dropped.Bundle.Bundle))
			}
			if kind.copies == releaseBundleCopies && dropped.Bundle.Release != unspentCopyRelease ||
				kind.copies == runningCopyCopies && dropped.Bundle.Release < MinRunningWorkspaceCopyRestoreID {
				t.Fatalf("the drop answered restore id %d, want the copy's own", dropped.Bundle.Release)
			}
			if gone := heldCopyRow(t, s, kind.copies, issue); gone.held {
				t.Fatalf("after the drop the %s is %+v, want it deleted", kind.name, gone)
			}
			requireNothing(t, "a second drop", dropFor(t, s, issue, keptVolumePod, second.Add(-time.Second)).Of(kind.copies.kind))
			requireNothing(t, "the take of the pod after it", askFor(t, s, issue, "pod-after-the-kept-one").Of(kind.copies.kind))
		})
	}
}

// A copy taken before the pod's Sandbox was made came off an earlier volume: a reset or a change
// of the disk's shape made the Sandbox, and its volume, after the copy was taken. The drop answers
// nothing of it and records nothing on it, and the take restores it to that pod as for any pod. A
// Sandbox made in the copy's own second cannot be ordered against the copy, since a
// creationTimestamp has one-second resolution, and the copy is restored there too: a drop deletes
// the work, and a copy the launcher refuses as older than its workspace costs a notice.
func TestACopyOfAnEarlierVolumeIsRestoredToThePodOfAKeptVolume(t *testing.T) {
	s := migratedStore(t)
	for i, kind := range unspentCopyKinds {
		t.Run(kind.name, func(t *testing.T) {
			issue := []string{"INFRA-3211", "INFRA-3212"}[i]
			saved := unspentCopyWork("PACK the work of an earlier volume")
			saveUnspentCopy(t, s, kind, issue, saved)
			second := heldCopyRow(t, s, kind.copies, issue).at.Truncate(time.Second)
			for _, sandbox := range []struct {
				name, pod string
				made      time.Time
			}{
				{"made an hour after the copy, as a reset makes it", "pod-of-a-reset", second.Add(time.Hour)},
				{"made in the copy's own second", "pod-of-the-same-second", second},
			} {
				before := heldCopyRow(t, s, kind.copies, issue)
				requireNothing(t, "the drop of a Sandbox "+sandbox.name, dropFor(t, s, issue, sandbox.pod, sandbox.made).Of(kind.copies.kind))
				requireRowAsItWas(t, s, kind.copies, issue, before)
				// No role of the pod registers, so the copy stays unspent for the next.
				requireSent(t, "the pod of a Sandbox "+sandbox.name, askFor(t, s, issue, sandbox.pod).Of(kind.copies.kind), saved)
			}
		})
	}
}

// A copy whose attempt is spent stays, whatever volume the pod asking mounts: its row is the
// record of that attempt, and the take sends it to no pod again.
func TestASpentCopyIsDroppedByNoKeptVolume(t *testing.T) {
	s := migratedStore(t)
	for i, kind := range unspentCopyKinds {
		t.Run(kind.name, func(t *testing.T) {
			issue := []string{"INFRA-3221", "INFRA-3222"}[i]
			saved := unspentCopyWork("PACK the work of a spent attempt")
			saveUnspentCopy(t, s, kind, issue, saved)
			requireSent(t, "the pod of a reset", askFor(t, s, issue, "pod-of-a-reset").Of(kind.copies.kind), saved)
			registerIn(t, s, issue, "pod-of-a-reset")
			spent := heldCopyRow(t, s, kind.copies, issue)

			requireNothing(t, "the pod of the kept Sandbox after it", dropFor(t, s, issue, keptVolumePod, spent.at.Truncate(time.Second).Add(-time.Hour)).Of(kind.copies.kind))
			requireRowAsItWas(t, s, kind.copies, issue, spent)
		})
	}
}

// The copies of an issue another project records, and of an issue no project records, are not
// this project's to drop: the drop of this project's pod answers nothing, records nothing on
// either copy and records no ask of that pod, and each row stays in the database as it was, here
// for the pod of the daemon that saved it. That daemon's project recorded each issue when it saved
// the copies, as the save of a running copy needs (PutRunningWorkspaceCopy), and the record of the
// second has gone since.
func TestADropTakesNoCopyOfAnIssueAnotherProjectRecords(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	for _, issue := range []string{"OTHER-8", "NOBODY-3"} {
		if _, err := s.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ($1, $1, 'OTHER', 'root', 'implementing', 1, 'in_progress', 'A', 1)`, issue); err != nil {
			t.Fatalf("record %s as OTHER's issue: %v", issue, err)
		}
		others := unspentCopyWork("PACK of another daemon's pod")
		for _, kind := range unspentCopyKinds {
			others.PodUID = "pod-other-" + kind.name
			if err := kind.save(ctx, s, "other", issue, copyBegun(t, s), others); err != nil {
				t.Fatalf("save OTHER's %s of %s: %v", kind.name, issue, err)
			}
		}
	}
	if _, err := s.Pool().Exec(ctx, `delete from issues where key = 'NOBODY-3'`); err != nil {
		t.Fatalf("drop the record of NOBODY-3: %v", err)
	}
	for _, issue := range []string{"OTHER-8", "NOBODY-3"} {
		rows := map[copyTable]copyRow{}
		for _, kind := range unspentCopyKinds {
			rows[kind.copies] = heldCopyRow(t, s, kind.copies, issue)
		}
		got, err := s.DropHeldWorkspaceCopies(ctx, copyProject, issue, "pod-of-infras-spawn", rows[releaseBundleCopies].at.Truncate(time.Second).Add(-time.Hour))
		if err != nil || !reflect.DeepEqual(got, runtime.WorkspaceRestore{}) {
			t.Fatalf("INFRA's pod of %s was answered %+v (%v), want nothing: INFRA does not record %s", issue, got, err, issue)
		}
		for copies, row := range rows {
			requireRowAsItWas(t, s, copies, issue, row)
		}
		var asks int
		if err := s.Pool().QueryRow(ctx, `select count(*) from workspace_restore_asks where issue = $1`, issue).Scan(&asks); err != nil || asks != 0 {
			t.Fatalf("INFRA's drop of %s left %d recorded asks (%v), want none", issue, asks, err)
		}
	}
}

// Where the issue holds both copies, the drop deletes each that came off the pod's kept volume and
// keeps the take's choice between the two: where it deletes the newer copy and the older came off
// an earlier volume, it closes the older unsent against this pod, as the take closes the older of
// two (runtime.DecideWorkspaceRestore), since the volume holds the newer work and the older copy
// applied over it would put the workspace back. The take that follows sends that pod nothing.
// Where neither came off the volume, the drop answers nothing and the take decides between them
// as for any pod, sending the newer and closing the older.
func TestADropOfBothCopiesDeletesEachTheVolumeHoldsAndClosesAnOlderOneBesideIt(t *testing.T) {
	s := migratedStore(t)
	const released, cameBack = "pod-the-release-took-the-work-off", "pod-that-came-back-without-it"
	for i, c := range []struct {
		name                         string
		bundleAt, runningAt          time.Duration
		madeAt                       time.Duration
		wantBundle, wantCopy         runtime.WorkspaceBundleTake
		wantTakeBundle, wantTakeCopy runtime.WorkspaceBundleTake
	}{
		{name: "both off this volume", bundleAt: -10 * time.Minute, runningAt: -5 * time.Minute, madeAt: -time.Hour,
			wantBundle: runtime.WorkspaceBundleHeldByVolume, wantCopy: runtime.WorkspaceBundleHeldByVolume,
			wantTakeBundle: runtime.WorkspaceBundleNone, wantTakeCopy: runtime.WorkspaceBundleNone},
		{name: "the newer, the running copy, off this volume and the bundle off an earlier one", bundleAt: -time.Hour, runningAt: -5 * time.Minute, madeAt: -30 * time.Minute,
			wantBundle: runtime.WorkspaceBundleSuperseded, wantCopy: runtime.WorkspaceBundleHeldByVolume,
			wantTakeBundle: runtime.WorkspaceBundleNone, wantTakeCopy: runtime.WorkspaceBundleNone},
		{name: "the newer, the bundle, off this volume and the running copy off an earlier one", bundleAt: -10 * time.Minute, runningAt: -time.Hour, madeAt: -30 * time.Minute,
			wantBundle: runtime.WorkspaceBundleHeldByVolume, wantCopy: runtime.WorkspaceBundleSuperseded,
			wantTakeBundle: runtime.WorkspaceBundleNone, wantTakeCopy: runtime.WorkspaceBundleNone},
		{name: "both off earlier volumes", bundleAt: -time.Hour, runningAt: -50 * time.Minute, madeAt: -30 * time.Minute,
			wantBundle: runtime.WorkspaceBundleNone, wantCopy: runtime.WorkspaceBundleNone,
			wantTakeBundle: runtime.WorkspaceBundleSuperseded, wantTakeCopy: runtime.WorkspaceBundleSend},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			issue := []string{"INFRA-3231", "INFRA-3232", "INFRA-3233", "INFRA-3234"}[i]
			recordCopyIssues(t, s, issue)
			now := storeClock(t, s)
			// The bundle is saved first, since its save drops the running copy; a running copy older
			// than it is one whose take began before the save and whose write landed after it.
			bundle := unspentCopyWork("PACK the released work")
			bundle.PodUID = released
			if err := s.PutWorkspaceBundle(ctx, issue, unspentCopyRelease, WorkspaceCopyBegun{at: now.Add(c.bundleAt)}, bundle); err != nil {
				t.Fatal(err)
			}
			running := unspentCopyWork("PACK the work of the pod that came back")
			running.PodUID = cameBack
			if err := s.PutRunningWorkspaceCopy(ctx, copyProject, issue, WorkspaceCopyBegun{at: now.Add(c.runningAt)}, running); err != nil {
				t.Fatal(err)
			}
			rows := map[copyTable]copyRow{releaseBundleCopies: heldCopyRow(t, s, releaseBundleCopies, issue), runningCopyCopies: heldCopyRow(t, s, runningCopyCopies, issue)}

			dropped := dropFor(t, s, issue, keptVolumePod, now.Add(c.madeAt).Truncate(time.Second))
			if dropped.ReleaseBundle.Take != c.wantBundle || dropped.Running.Take != c.wantCopy {
				t.Fatalf("the drop answered %v of the bundle and %v of the running copy, want %v and %v",
					dropped.ReleaseBundle.Take, dropped.Running.Take, c.wantBundle, c.wantCopy)
			}
			for copies, take := range map[copyTable]runtime.WorkspaceBundleTake{releaseBundleCopies: c.wantBundle, runningCopyCopies: c.wantCopy} {
				row := heldCopyRow(t, s, copies, issue)
				switch take {
				case runtime.WorkspaceBundleHeldByVolume:
					if row.held {
						t.Fatalf("after the drop the %s is %+v, want it deleted", copies.kind, row)
					}
				case runtime.WorkspaceBundleSuperseded:
					if !row.held || row.size != rows[copies].size || row.attemptPod != keptVolumePod || row.outcome != supersededCopyOutcome {
						t.Fatalf("after the drop the %s is %+v, want its bytes kept and its attempt closed against %s", copies.kind, row, keptVolumePod)
					}
				default:
					requireRowAsItWas(t, s, copies, issue, rows[copies])
				}
			}

			taken := askFor(t, s, issue, keptVolumePod)
			if taken.ReleaseBundle.Take != c.wantTakeBundle || taken.Running.Take != c.wantTakeCopy {
				t.Fatalf("the take after the drop answered %v of the bundle and %v of the running copy, want %v and %v",
					taken.ReleaseBundle.Take, taken.Running.Take, c.wantTakeBundle, c.wantTakeCopy)
			}
		})
	}
}

// The drop records the asking pod's first ask, as the take does: a role start whose drop deleted a
// copy takes nothing more, and the pod has been working since that start. So a copy written after
// it — here a late running copy whose take began on an earlier volume, before the Sandbox was
// made, and whose write landed after the drop — is left for the next pod at the pod's next role
// start, and never sent over the work the pod did since its first.
func TestADropRecordsThePodsFirstAskSoALateCopyIsLeftForTheNextPod(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	const issue = "INFRA-3241"
	recordCopyIssues(t, s, issue)
	now := storeClock(t, s)
	made := now.Add(-time.Hour).Truncate(time.Second)
	bundle := unspentCopyWork("PACK the released work")
	if err := s.PutWorkspaceBundle(ctx, issue, unspentCopyRelease, copyBegun(t, s), bundle); err != nil {
		t.Fatal(err)
	}
	if dropped := dropFor(t, s, issue, keptVolumePod, made); dropped.ReleaseBundle.Take != runtime.WorkspaceBundleHeldByVolume {
		t.Fatalf("the pod's first role start dropped %v of the bundle, want it dropped", dropped.ReleaseBundle.Take)
	}

	late := unspentCopyWork("PACK a late copy of an earlier volume")
	late.PodUID = "pod-of-an-earlier-volume"
	if err := s.PutRunningWorkspaceCopy(ctx, copyProject, issue, WorkspaceCopyBegun{at: now.Add(-2 * time.Hour)}, late); err != nil {
		t.Fatal(err)
	}
	requireNothing(t, "the drop of the pod's next role start", dropFor(t, s, issue, keptVolumePod, made).Running)
	if taken := askFor(t, s, issue, keptVolumePod).Running; taken.Take != runtime.WorkspaceBundleLeftForNextPod {
		t.Fatalf("the take of the pod's next role start answered %v of the late copy, want %v", taken.Take, runtime.WorkspaceBundleLeftForNextPod)
	}
}
