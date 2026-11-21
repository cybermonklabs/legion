package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A copy sent to a pod stays unspent until a role of that pod registers (RecordPodRegistration,
// heldCopy). The tests here hold that rule for both kinds of copy where it applies: a pod, or its
// roles container, that ends before any role of it registered leaves the copy for the next start
// of the issue, the pod after it or its own restarted container, and the copy is decided again
// for that ask; a pod where a role registered spends it, for its restarted container and every
// pod after it.

// unspentCopyKind is one of the two tables a saved copy is kept in, with the write that stores a
// copy in it, as the daemon makes that write.
type unspentCopyKind struct {
	name   string
	copies copyTable
	save   func(ctx context.Context, s *Store, project, issue string, begun WorkspaceCopyBegun, b runtime.WorkspaceBundle) error
}

// unspentCopyRelease is the release every release bundle of these tests is saved under.
const unspentCopyRelease int64 = 11

var unspentCopyKinds = []unspentCopyKind{
	{
		name: "release bundle", copies: releaseBundleCopies,
		save: func(ctx context.Context, s *Store, _, issue string, begun WorkspaceCopyBegun, b runtime.WorkspaceBundle) error {
			return s.PutWorkspaceBundle(ctx, issue, unspentCopyRelease, begun, b)
		},
	},
	{
		name: "running copy", copies: runningCopyCopies,
		save: func(ctx context.Context, s *Store, project, issue string, begun WorkspaceCopyBegun, b runtime.WorkspaceBundle) error {
			return s.PutRunningWorkspaceCopy(ctx, project, issue, begun, b)
		},
	},
}

// unspentCopyWork is the work the copy of each test holds, taken off the pod the work came off.
func unspentCopyWork(body string) runtime.WorkspaceBundle {
	return runtime.WorkspaceBundle{
		Bundle: []byte(body), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"},
		Bookmarks: map[string]string{"side": "dffe88ae2f793e13436cef3921adc988433fbd4e"},
		PodUID:    "pod-the-work-came-off",
	}
}

// saveUnspentCopy stores b as issue's copy of kind, its take begun just before the write, with the
// issue recorded for the project whose pods ask (recordCopyIssues), since a running copy is written
// only for a project that records its issue (PutRunningWorkspaceCopy).
func saveUnspentCopy(t *testing.T, s *Store, kind unspentCopyKind, issue string, b runtime.WorkspaceBundle) {
	t.Helper()
	recordCopyIssues(t, s, issue)
	if err := kind.save(context.Background(), s, copyProject, issue, copyBegun(t, s), b); err != nil {
		t.Fatalf("save the %s of %s: %v", kind.name, issue, err)
	}
}

// askFor is the take of pod's role start by a pod of the project that records issue
// (recordCopyIssues), which fails the test on an error.
func askFor(t *testing.T, s *Store, issue, pod string) runtime.WorkspaceRestore {
	t.Helper()
	recordCopyIssues(t, s, issue)
	restore, err := s.TakeWorkspaceCopyForRestore(context.Background(), copyProject, issue, pod)
	if err != nil {
		t.Fatalf("the take of %s for %s: %v", issue, pod, err)
	}
	return restore
}

// registerIn is a role of pod registering, as the daemon records it before it answers.
func registerIn(t *testing.T, s *Store, issue, pod string) {
	t.Helper()
	if err := s.RecordPodRegistration(context.Background(), issue, pod); err != nil {
		t.Fatalf("record a registration in %s of %s: %v", pod, issue, err)
	}
}

// requireSent fails the test unless taken is a send of b whole.
func requireSent(t *testing.T, who string, taken runtime.WorkspaceCopyTaken, b runtime.WorkspaceBundle) {
	t.Helper()
	got := taken.Bundle
	if taken.Take != runtime.WorkspaceBundleSend || string(got.Bundle) != string(b.Bundle) || got.Head != b.Head {
		t.Fatalf("%s was answered %v of %q at %s, want %q at %s sent whole", who, taken.Take, got.Bundle, got.Head, b.Bundle, b.Head)
	}
}

// requireNothing fails the test unless taken sends nothing and records nothing.
func requireNothing(t *testing.T, who string, taken runtime.WorkspaceCopyTaken) {
	t.Helper()
	if taken.Take != runtime.WorkspaceBundleNone || len(taken.Bundle.Bundle) != 0 {
		t.Fatalf("%s was answered %v with %d bytes, want nothing sent", who, taken.Take, len(taken.Bundle.Bundle))
	}
}

// A copy sent to a pod that ended before any role of it registered is sent to the pod after it:
// no role of the first pod ran, so its disk took nothing of the work and the work is in the
// database alone. The send moves the copy's attempt to the new pod, so the first pod's report,
// should its launcher's report reach the daemon late, keeps nothing.
func TestAnUnspentCopyIsSentToThePodAfterAPodWhereNoRoleRegistered(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	for i, kind := range unspentCopyKinds {
		t.Run(kind.name, func(t *testing.T) {
			issue := []string{"INFRA-3101", "INFRA-3102"}[i]
			saved := unspentCopyWork("PACK the work for the pod after")
			saveUnspentCopy(t, s, kind, issue, saved)

			first := askFor(t, s, issue, "pod-a").Of(kind.copies.kind)
			requireSent(t, "the first pod", first, saved)

			// pod-a ends before any of its roles registers, and pod-b replaces it.
			next := askFor(t, s, issue, "pod-b").Of(kind.copies.kind)
			requireSent(t, "the pod after the one that ended unregistered", next, saved)
			if row := heldCopyRow(t, s, kind.copies, issue); row.attemptPod != "pod-b" || row.outcome != sentCopyOutcome {
				t.Fatalf("the %s holds %+v, want its attempt the send to pod-b", kind.name, row)
			}

			// pod-a's report of its restore names a send the row no longer holds.
			if err := s.KeepRestoredWorkspaceCopy(ctx, issue, "pod-a", first.Bundle.Release); err != nil {
				t.Fatal(err)
			}
			if row := heldCopyRow(t, s, kind.copies, issue); !row.held || row.attemptPod != "pod-b" {
				t.Fatalf("after pod-a's late report the %s holds %+v, want it as pod-b's send left it", kind.name, row)
			}
		})
	}
}

// A copy sent to a pod whose roles container restarts before any role registered is sent again
// to that pod: the launcher's record of the restore it ran died with the container, and no role
// has touched the workspace since, so the restore runs again over the same disk. The send is the
// same copy under the same restore id, which is what the launcher's slice sequence is named by.
func TestAnUnspentCopyIsSentAgainToARestartedContainerOfItsPod(t *testing.T) {
	s := migratedStore(t)
	for i, kind := range unspentCopyKinds {
		t.Run(kind.name, func(t *testing.T) {
			issue := []string{"INFRA-3111", "INFRA-3112"}[i]
			saved := unspentCopyWork("PACK the work for the restarted container")
			saveUnspentCopy(t, s, kind, issue, saved)

			first := askFor(t, s, issue, "pod-a").Of(kind.copies.kind)
			requireSent(t, "the pod's first role start", first, saved)
			again := askFor(t, s, issue, "pod-a").Of(kind.copies.kind)
			requireSent(t, "the pod's restarted container", again, saved)
			if again.Bundle.Release != first.Bundle.Release {
				t.Fatalf("the copy was sent again under restore id %d, want the first send's %d", again.Bundle.Release, first.Bundle.Release)
			}
		})
	}
}

// A role of the pod a copy was sent to registering spends the copy: that role may change the
// workspace from then on, so the pod's restarted container is sent nothing, and nor is any pod
// after it. The record is the pod's, so another role start of the pod, which is all a restarted
// container's ask is, leaves it standing, and a second registration keeps the first one's time. A
// registration in a pod that never asked, and one in a pod of another issue, spend nothing.
func TestAnUnspentCopyIsSpentOnceARoleOfItsPodRegisters(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	for i, kind := range unspentCopyKinds {
		t.Run(kind.name, func(t *testing.T) {
			issue := []string{"INFRA-3121", "INFRA-3122"}[i]
			saved := unspentCopyWork("PACK the work a role registered over")
			saveUnspentCopy(t, s, kind, issue, saved)

			requireSent(t, "the pod's first role start", askFor(t, s, issue, "pod-a").Of(kind.copies.kind), saved)
			// Registrations elsewhere: a pod of the issue that never asked, which leaves no row,
			// and pod-a's uid under another issue.
			registerIn(t, s, issue, "pod-that-never-asked")
			registerIn(t, s, "INFRA-3199", "pod-a")
			requireSent(t, "the pod's next role start after registrations in other pods", askFor(t, s, issue, "pod-a").Of(kind.copies.kind), saved)

			registerIn(t, s, issue, "pod-a")
			var registered time.Time
			if err := s.Pool().QueryRow(ctx, `select registered_at from workspace_restore_asks where issue = $1 and pod_uid = $2`,
				issue, "pod-a").Scan(&registered); err != nil {
				t.Fatalf("read pod-a's registration: %v", err)
			}
			for _, ask := range []string{"pod-a", "pod-a", "pod-b", "pod-c"} {
				requireNothing(t, ask+" after a role of pod-a registered", askFor(t, s, issue, ask).Of(kind.copies.kind))
			}
			registerIn(t, s, issue, "pod-a")
			var still time.Time
			if err := s.Pool().QueryRow(ctx, `select registered_at from workspace_restore_asks where issue = $1 and pod_uid = $2`,
				issue, "pod-a").Scan(&still); err != nil || !still.Equal(registered) {
				t.Fatalf("pod-a's registration reads %s (%v) after its asks and a second registration, want the first's %s", still, err, registered)
			}
			if row := heldCopyRow(t, s, kind.copies, issue); !row.held || row.attemptPod != "pod-a" || row.outcome != sentCopyOutcome {
				t.Fatalf("the %s holds %+v, want its bytes kept and its attempt pod-a's send", kind.name, row)
			}
			if err := s.RecordPodRegistration(ctx, issue, ""); err == nil {
				t.Fatal("a registration that named no pod was recorded")
			}
		})
	}
}

// A copy sent again is decided again, by every rule of runtime.DecideWorkspaceRestore, and not
// replayed: a push recorded after a running copy's take began refuses it to the next pod where it
// was sent to the one before; a newer copy written since closes it unsent; and a copy written
// after a pod's first ask is left for the pod after that one on that pod's next ask, whatever pod
// it was sent to in between.
func TestAnUnspentCopySentAgainIsDecidedAgain(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)

	t.Run("a push recorded since", func(t *testing.T) {
		for i, kind := range unspentCopyKinds {
			t.Run(kind.name, func(t *testing.T) {
				issue := []string{"INFRA-3131", "INFRA-3132"}[i]
				saved := unspentCopyWork("PACK the work before the push")
				saved.Head = "c1"
				saveUnspentCopy(t, s, kind, issue, saved)
				requireSent(t, "pod-a", askFor(t, s, issue, "pod-a").Of(kind.copies.kind), saved)
				recordPush(t, s, issue, "c1", "c2")

				resent := askFor(t, s, issue, "pod-b").Of(kind.copies.kind)
				if kind.copies == releaseBundleCopies {
					// The push rule never refuses a release bundle, on its first send or again.
					requireSent(t, "pod-b", resent, saved)
					return
				}
				if resent.Take != runtime.WorkspaceBundleRefusedPushed || len(resent.Bundle.Bundle) != 0 {
					t.Fatalf("pod-b was answered %v with %d bytes, want the running copy refused as older than the push", resent.Take, len(resent.Bundle.Bundle))
				}
				if row := heldCopyRow(t, s, kind.copies, issue); row.outcome != refusedPushedOutcome || row.attemptPod != "pod-b" {
					t.Fatalf("the %s holds %+v, want its attempt pod-b's refusal", kind.name, row)
				}
				requireNothing(t, "pod-c after the refusal", askFor(t, s, issue, "pod-c").Of(kind.copies.kind))
			})
		}
	})

	// A running copy is never closed by a newer release bundle: the bundle's save drops the
	// running copy in its own transaction (PutWorkspaceBundle). So the case is a release bundle's.
	t.Run("a newer copy written since", func(t *testing.T) {
		const issue = "INFRA-3133"
		bundle := unspentCopyWork("PACK the release's work")
		saveUnspentCopy(t, s, unspentCopyKinds[0], issue, bundle)
		requireSent(t, "pod-a", askFor(t, s, issue, "pod-a").ReleaseBundle, bundle)
		newer := unspentCopyWork("PACK a newer copy off another pod")
		newer.PodUID = "pod-other"
		if err := s.PutRunningWorkspaceCopy(ctx, copyProject, issue, copyBegun(t, s), newer); err != nil {
			t.Fatal(err)
		}

		restore := askFor(t, s, issue, "pod-b")
		if closed := restore.ReleaseBundle; closed.Take != runtime.WorkspaceBundleSuperseded || len(closed.Bundle.Bundle) != 0 {
			t.Fatalf("the release bundle was answered %v with %d bytes, want it closed unsent beside the newer copy", closed.Take, len(closed.Bundle.Bundle))
		}
		requireSent(t, "pod-b, of the newer copy", restore.Running, newer)
		if row := heldCopyRow(t, s, releaseBundleCopies, issue); row.outcome != supersededCopyOutcome || row.attemptPod != "pod-b" {
			t.Fatalf("the release bundle holds %+v, want its attempt pod-b's supersession", row)
		}
		requireNothing(t, "pod-c, of the release bundle", askFor(t, s, issue, "pod-c").ReleaseBundle)
	})

	t.Run("a copy written after the asking pod's first ask", func(t *testing.T) {
		for i, kind := range unspentCopyKinds {
			t.Run(kind.name, func(t *testing.T) {
				issue := []string{"INFRA-3134", "INFRA-3135"}[i]
				// pod-b asks while the issue holds nothing, and works on.
				requireNothing(t, "pod-b's first ask", askFor(t, s, issue, "pod-b").Of(kind.copies.kind))
				late := unspentCopyWork("PACK the copy written after pod-b's first ask")
				saveUnspentCopy(t, s, kind, issue, late)
				// pod-a first asks after the write, is sent the copy and ends unregistered.
				requireSent(t, "pod-a", askFor(t, s, issue, "pod-a").Of(kind.copies.kind), late)

				restore := askFor(t, s, issue, "pod-b")
				left := restore.Of(kind.copies.kind)
				if left.Take != runtime.WorkspaceBundleLeftForNextPod || len(left.Bundle.Bundle) != 0 || !left.Written.After(restore.FirstAsked) {
					t.Fatalf("pod-b's next ask was answered %v with %d bytes of a copy written at %s against its first ask at %s, want it left for the next pod",
						left.Take, len(left.Bundle.Bundle), left.Written, restore.FirstAsked)
				}
				if row := heldCopyRow(t, s, kind.copies, issue); row.attemptPod != "pod-a" || row.outcome != sentCopyOutcome {
					t.Fatalf("the %s holds %+v, want nothing recorded by pod-b's ask: pod-a's send stands", kind.name, row)
				}
				requireSent(t, "pod-c, the pod after", askFor(t, s, issue, "pod-c").Of(kind.copies.kind), late)
			})
		}
	})
}

// A restore its pod's launcher confirmed is kept as that pod's own running copy, as every
// confirmed restore is (KeepRestoredWorkspaceCopy): the pod's restarted container is sent nothing,
// its workspace holding the work, and the pod after it is sent the work, whether or not a role of
// the restoring pod registered. A sent copy whose pod has no row of asks — the send of an earlier
// daemon, which recorded none — is spent, as every sent copy was before.
func TestAnUnspentCopyRestoredIsKeptAsItsPodsOwnAndAMissingAskReadsAsSpent(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	for i, kind := range unspentCopyKinds {
		t.Run(kind.name+": confirmed", func(t *testing.T) {
			issue := []string{"INFRA-3141", "INFRA-3142"}[i]
			saved := unspentCopyWork("PACK the work pod-a restored")
			saveUnspentCopy(t, s, kind, issue, saved)
			sent := askFor(t, s, issue, "pod-a").Of(kind.copies.kind)
			requireSent(t, "pod-a", sent, saved)
			if err := s.KeepRestoredWorkspaceCopy(ctx, issue, "pod-a", sent.Bundle.Release); err != nil {
				t.Fatal(err)
			}
			if source := storedRunningCopy(t, s, issue).sourcePod; source != "pod-a" {
				t.Fatalf("the kept copy names %q, want pod-a, whose disk holds the work", source)
			}
			requireNothing(t, "pod-a's restarted container", askFor(t, s, issue, "pod-a").Running)
			requireSent(t, "pod-b", askFor(t, s, issue, "pod-b").Running, saved)
		})
		t.Run(kind.name+": sent by an earlier daemon", func(t *testing.T) {
			issue := []string{"INFRA-3143", "INFRA-3144"}[i]
			saveUnspentCopy(t, s, kind, issue, unspentCopyWork("PACK the work an earlier daemon sent"))
			if _, err := s.Pool().Exec(ctx, `update `+kind.copies.table+`
				set attempt_pod_uid = 'pod-of-an-earlier-daemon', attempt_at = now(), attempt_outcome = $2 where issue = $1`,
				issue, sentCopyOutcome); err != nil {
				t.Fatal(err)
			}
			requireNothing(t, "a pod after an earlier daemon's send", askFor(t, s, issue, "pod-b").Of(kind.copies.kind))
		})
	}
}

// rollbackStatements are what docs/kubernetes.md's "Rolling back the running copy" has an
// operator run against the database, with this release's daemon stopped, before the earlier
// daemon runs: the running copies deleted, and every pod that asked for a restore marked as one
// where a role registered, since a role that registers under the earlier daemon is recorded by
// none (migrations' 20261114090103_workspace_restore_asks).
const rollbackStatements = `delete from workspace_running_copies;
update workspace_restore_asks set registered_at = first_asked_at where registered_at is null;`

// A copy sent to a pod before the rollback, before any role of that pod registered, is spent once
// the rollback's statements have run, as the earlier daemon's rule has every sent copy spent. The
// pod's roles register under the earlier daemon, which records nothing, so without the statement a
// release bundle the rollback keeps would read as unspent after the roll forward and be sent again
// to that pod's restarted roles container, over the work its roles did, and to the pod after it.
// The running copy is gone with its table's rows. A pod that first asks after the roll forward is
// held to this release's rule from its first ask: a copy sent to it stays unspent for its
// restarted container until a role of it registers.
func TestAnUnspentCopySentBeforeARollbackIsSpentByTheRollback(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	for i, kind := range unspentCopyKinds {
		t.Run(kind.name, func(t *testing.T) {
			issue := []string{"INFRA-3151", "INFRA-3152"}[i]
			saved := unspentCopyWork("PACK the work sent before the rollback")
			saveUnspentCopy(t, s, kind, issue, saved)
			requireSent(t, "pod-a before the rollback", askFor(t, s, issue, "pod-a").Of(kind.copies.kind), saved)

			if _, err := s.Pool().Exec(ctx, rollbackStatements); err != nil {
				t.Fatalf("run the rollback's statements: %v", err)
			}
			// The earlier daemon runs, pod-a's roles register under it, and the roll forward
			// follows; then pod-a's roles container restarts and pod-b replaces pod-a.
			requireNothing(t, "pod-a's restarted container after the roll forward", askFor(t, s, issue, "pod-a").Of(kind.copies.kind))
			requireNothing(t, "pod-b after the roll forward", askFor(t, s, issue, "pod-b").Of(kind.copies.kind))

			later := unspentCopyWork("PACK the work saved after the roll forward")
			saveUnspentCopy(t, s, kind, issue, later)
			requireSent(t, "pod-c, which first asks after the roll forward", askFor(t, s, issue, "pod-c").Of(kind.copies.kind), later)
			requireSent(t, "pod-c's restarted container", askFor(t, s, issue, "pod-c").Of(kind.copies.kind), later)
		})
	}
}
