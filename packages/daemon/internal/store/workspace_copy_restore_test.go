package store

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Both kinds of saved copy keep the same two rules — one decision (runtime.DecideWorkspaceRestore)
// taken and recorded in the one transaction that spends their attempt (takeCopy) — so one case
// runs against each table.
//
// The pod the work came off is answered nothing and spends nothing. Every start of every role asks
// for a copy to restore, and that pod goes on starting roles — a release stops its roles and one
// relaunched before the teardown starts again, and a running copy is taken while the pod works.
// Its workspace is the work, and were it to spend the attempt the pod that replaces it, whose
// workspace holds only what was pushed, would be answered nothing. It is the pod's uid that says
// so, not its clock, so a node clock running ahead of the API server's cannot make that pod look
// like the replacement, nor the replacement like it.
//
// A pod that is not that one and was not created after the copy spends the attempt on a refusal,
// and no later pod is sent the copy. Such a pod is one the release did not replace, or the
// replacement itself under a node clock running ahead of the API server's; either works and
// pushes from its own fetch for as long as it lives, so a copy kept open for the pod after it would
// be applied over that pushed work. A creationTimestamp has one-second resolution, so a pod
// created inside the copy's own second is taken as the newer one.
//
// A copy is sent to one pod's launcher once. The launcher's record of the restore it ran dies with
// the process that ran it, so a roles container that restarts comes back to the same disk with
// that record gone and the workspace hours of work past the copy; the daemon's record survives,
// written in the take itself, before a slice leaves the daemon. The bytes stay in every case.
func TestEitherSavedCopyOfAWorkspaceBundleHasOneRestoreAttemptReadToItsSecond(t *testing.T) {
	const issue = "INFRA-945"
	for _, c := range []struct {
		name      string
		copies    copyTable
		source    string
		save      func(ctx context.Context, s *Store, b runtime.WorkspaceBundle) error
		restoreID func(id int64) bool
	}{
		{
			name: "release bundle", copies: releaseBundleCopies, source: releasedPodUID,
			save: func(ctx context.Context, s *Store, b runtime.WorkspaceBundle) error {
				return s.PutWorkspaceBundle(ctx, issue, 11, b)
			},
			restoreID: func(id int64) bool { return id == 11 },
		},
		{
			name: "running copy", copies: runningCopyCopies, source: runningPodUID,
			save: func(ctx context.Context, s *Store, b runtime.WorkspaceBundle) error {
				return s.PutRunningWorkspaceCopy(ctx, issue, b)
			},
			restoreID: func(id int64) bool { return id >= MinRunningWorkspaceCopyRestoreID },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s := migratedStore(t)
			at := time.Date(2026, 11, 2, 9, 30, 0, 750_000_000, time.UTC)
			saved := runtime.WorkspaceBundle{
				Bundle: []byte("PACK the saved work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
				Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"},
				Bookmarks: map[string]string{"legion/INFRA-945": "dffe88ae2f793e13436cef3921adc988433fbd4e"},
				At:        at, PodUID: c.source,
			}
			if err := c.save(ctx, s, saved); err != nil {
				t.Fatal(err)
			}
			// Two starts in the pod the work came off, one stamped before the copy and one after:
			// neither is answered and neither spends the attempt.
			for _, created := range []time.Time{at.Add(-3 * time.Hour), at.Add(time.Minute)} {
				got, take, err := takeOf(ctx, s, c.copies.kind, issue, c.source, created)
				if err != nil || take != runtime.WorkspaceBundleNone {
					t.Fatalf("the pod the work came off, asking as created %s against the copy at %s, was answered %q (%v, %v); want nothing to send",
						created, at, got.Bundle, take, err)
				}
			}
			if row := heldCopyRow(t, s, c.copies, issue); row.attemptPod != "" {
				t.Fatalf("the pod the work came off spent the attempt: %+v", row)
			}

			// A pod stamped a minute before the copy was taken.
			refused, take, err := takeOf(ctx, s, c.copies.kind, issue, "pod-skewed-replacement", at.Add(-time.Minute))
			if err != nil || take != runtime.WorkspaceBundleRefusedOlder {
				t.Fatalf("a pod created before the copy was answered %v (%v), want it refused as not newer than the copy", take, err)
			}
			if len(refused.Bundle) != 0 {
				t.Fatalf("the refused pod was answered %d bytes of bundle, want none", len(refused.Bundle))
			}
			if !refused.At.Equal(at) || !c.restoreID(refused.Release) {
				t.Fatalf("the refusal reads the copy as id %d taken at %s, want this table's id and %s: the warning names both", refused.Release, refused.At, at)
			}
			// Every pod after it is answered nothing, however new: the refusal was the attempt.
			for _, pod := range []struct {
				uid     string
				created time.Time
			}{
				{"pod-after-an-eviction", at.Add(time.Hour)},
				{"pod-after-a-park-and-unpark", at.Add(2 * time.Hour)},
			} {
				got, take, err := takeOf(ctx, s, c.copies.kind, issue, pod.uid, pod.created)
				if err != nil || take != runtime.WorkspaceBundleNone || len(got.Bookmarks) != 0 {
					t.Fatalf("%s, created %s after the copy at %s, was answered %q with bookmarks %v (%v, %v); want nothing: the refusal was the copy's one attempt",
						pod.uid, pod.created, at, got.Bundle, got.Bookmarks, take, err)
				}
			}
			if row := heldCopyRow(t, s, c.copies, issue); !row.held || row.size != saved.Size() || row.outcome != refusedOlderCopyOutcome {
				t.Fatalf("after the refusal the store holds %+v, want the %d bytes kept and the refusal recorded", row, saved.Size())
			}

			// The next save is other work, with an attempt of its own, and a pod created inside
			// its own second is taken as the newer.
			next := saved
			next.Bundle, next.Head = []byte("PACK the next save"), "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9"
			if err := c.save(ctx, s, next); err != nil {
				t.Fatal(err)
			}
			sent, take, err := takeOf(ctx, s, c.copies.kind, issue, "pod-of-the-same-second", at.Truncate(time.Second))
			if err != nil || take != runtime.WorkspaceBundleSend || string(sent.Bundle) != string(next.Bundle) || sent.Head != next.Head ||
				!slices.Equal(sent.Tips, next.Tips) || !maps.Equal(sent.Bookmarks, next.Bookmarks) || !c.restoreID(sent.Release) {
				t.Fatalf("a pod created inside the copy's own second was answered %+v (%v, %v); want the copy whole", sent, take, err)
			}
			// The same pod again, which is its roles container restarting, then the pod that
			// replaces it, then a third after an eviction: each newer than the copy, so the spent
			// attempt is the only thing refusing them.
			for i, pod := range []string{"pod-of-the-same-second", "pod-after-it", "pod-after-an-eviction"} {
				again, take, err := takeOf(ctx, s, c.copies.kind, issue, pod, at.Add(time.Duration(i+1)*time.Minute))
				if err != nil || take != runtime.WorkspaceBundleNone {
					t.Fatalf("%s was answered %q (%v, %v) after the copy's one attempt, want nothing to send", pod, again.Bundle, take, err)
				}
			}
			if row := heldCopyRow(t, s, c.copies, issue); !row.held || row.size != next.Size() || row.outcome != sentCopyOutcome || row.attemptPod != "pod-of-the-same-second" {
				t.Fatalf("after the send the store holds %+v, want the %d bytes kept and the send recorded against its pod", row, next.Size())
			}

			// A take with no pod to name, or no word of when its pod was created, is refused: the
			// rules have nothing to read.
			if _, err := s.TakeWorkspaceCopyForRestore(ctx, issue, "", at.Add(time.Minute)); err == nil {
				t.Fatal("a take that named no pod was answered")
			}
			if _, err := s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-uid-5", time.Time{}); err == nil {
				t.Fatal("a take that said nothing of when its pod was created was answered")
			}
		})
	}
}

// recordPush records the push of issue's branch from before to after as the workflow's push
// handler does, in a transaction of its own.
func recordPush(t *testing.T, s *Store, issue, before, after string, at time.Time) {
	t.Helper()
	if err := s.Tx(context.Background(), func(tx pgx.Tx) error {
		return s.RecordIssuePush(context.Background(), tx, issue, before, after, at)
	}); err != nil {
		t.Fatalf("record the push of %s from %s to %s: %v", issue, before, after, err)
	}
}

// A saved copy older than the newest push of the issue's branch the daemon recorded is sent to no
// pod, whichever table holds it. Restoring it would `jj edit` back to its head and move each
// bookmark `--allow-backwards` behind the pushed commits, and the next push would remove them from
// GitHub. A release bundle left from an earlier release is refused the same way as a running copy
// taken before the push: both are rows the store holds, and neither is newer than the push. Each
// refusal spends its copy's attempt and keeps its bytes, and the answer names the push it read,
// which the restore's warning carries.
//
// A push recorded late, behind a newer one, moves neither the time nor the head, although the
// daemon applies it later; and an issue with no push recorded refuses nothing by this rule. The
// copy the daemon takes after the push is a new row with an attempt of its own, and it is the one
// the next pod is sent.
func TestABundleOrRunningCopyOlderThanTheNewestRecordedPushIsSentToNoPod(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	earlier := runtime.WorkspaceBundle{Bundle: []byte("PACK an earlier release's work"), Head: "c0", At: at.Add(-time.Hour), PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 7, earlier); err != nil {
		t.Fatal(err)
	}
	running := runtime.WorkspaceBundle{Bundle: []byte("PACK the copy before the push"), Head: "c1", At: at, PodUID: runningPodUID}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", running); err != nil {
		t.Fatal(err)
	}
	pushed := at.Add(time.Minute)
	recordPush(t, s, "INFRA-1117", "c1", "c2", pushed)
	recordPush(t, s, "INFRA-1117", "c0", "c1", pushed.Add(time.Hour))

	restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-after-the-push", at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !restore.Push.At.Equal(pushed) || restore.Push.Head != "c2" {
		t.Fatalf("the take read the push %+v, want c2 at %s: a push recorded late moved the record", restore.Push, pushed)
	}
	if len(restore.Copies) != 2 {
		t.Fatalf("the take answered %+v, want both copies refused", restore.Copies)
	}
	for _, taken := range restore.Copies {
		if taken.Take != runtime.WorkspaceBundleRefusedPushed || len(taken.Bundle.Bundle) != 0 {
			t.Fatalf("the %s was answered %v with %d bytes, want it refused as older than the push with no body", taken.Kind, taken.Take, len(taken.Bundle.Bundle))
		}
	}
	for _, copies := range []copyTable{releaseBundleCopies, runningCopyCopies} {
		if row := heldCopyRow(t, s, copies, "INFRA-1117"); !row.held || row.outcome != refusedPushedOutcome || row.attemptPod != "pod-after-the-push" {
			t.Fatalf("the %s holds %+v, want its bytes kept and its attempt spent refusing it", copies.kind, row)
		}
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-after-that", at.Add(2*time.Hour)); err != nil || len(restore.Copies) != 0 {
		t.Fatalf("the pod after was answered %+v (%v), want nothing: each refusal was its copy's one attempt", restore.Copies, err)
	}

	// The copy the daemon takes once it has recorded the push is newer than it, and is sent.
	after := runtime.WorkspaceBundle{Bundle: []byte("PACK the copy after the push"), Head: "c3", At: pushed.Add(time.Second), PodUID: runningPodUID}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", after); err != nil {
		t.Fatal(err)
	}
	if sent, take, err := takeOf(ctx, s, runtime.WorkspaceCopyRunning, "INFRA-1117", "pod-after-the-copy", at.Add(3*time.Hour)); err != nil ||
		take != runtime.WorkspaceBundleSend || string(sent.Bundle) != string(after.Bundle) {
		t.Fatalf("the copy taken after the push was answered %q (%v, %v), want it sent", sent.Bundle, take, err)
	}

	// An issue whose branch has no push recorded refuses nothing by this rule.
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-2760", running); err != nil {
		t.Fatal(err)
	}
	if _, take, err := takeOf(ctx, s, runtime.WorkspaceCopyRunning, "INFRA-2760", "pod-replacement", at.Add(time.Hour)); err != nil || take != runtime.WorkspaceBundleSend {
		t.Fatalf("a copy of an issue with no push recorded was answered %v (%v), want it sent", take, err)
	}
}

// An issue can hold both copies at once, and the pod that replaces its pod is offered the newer
// while the older is closed unsent in the same take: the pods after this one hold the descendants
// of the work it restores, so a copy left open for them would be applied over everything pushed in
// between. A tie goes to the release bundle, taken once the roles had stopped.
func TestTheBundleRestoreOffersTheNewerCopyAndSupersedesTheOlderInOneTake(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		issue        string
		runningAt    time.Time
		sent, closed runtime.WorkspaceCopyKind
	}{
		{"INFRA-1117", at.Add(5 * time.Minute), runtime.WorkspaceCopyRunning, runtime.WorkspaceCopyReleaseBundle},
		{"INFRA-2760", at, runtime.WorkspaceCopyReleaseBundle, runtime.WorkspaceCopyRunning},
	} {
		if err := s.PutWorkspaceBundle(ctx, c.issue, 7, runtime.WorkspaceBundle{Bundle: []byte("PACK the release's work"), Head: "b", At: at, PodUID: releasedPodUID}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutRunningWorkspaceCopy(ctx, c.issue, runtime.WorkspaceBundle{Bundle: []byte("PACK the running copy"), Head: "r", At: c.runningAt, PodUID: runningPodUID}); err != nil {
			t.Fatal(err)
		}
		restore, err := s.TakeWorkspaceCopyForRestore(ctx, c.issue, "pod-replacement", at.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		takes := map[runtime.WorkspaceCopyKind]runtime.WorkspaceCopyTaken{}
		for _, taken := range restore.Copies {
			takes[taken.Kind] = taken
		}
		if sent := takes[c.sent]; sent.Take != runtime.WorkspaceBundleSend || len(sent.Bundle.Bundle) == 0 {
			t.Fatalf("%s: the %s was answered %+v, want it sent", c.issue, c.sent, sent)
		}
		if closed := takes[c.closed]; closed.Take != runtime.WorkspaceBundleSuperseded || len(closed.Bundle.Bundle) != 0 || closed.Size == 0 {
			t.Fatalf("%s: the %s was answered %+v, want it closed unsent with its size named", c.issue, c.closed, closed)
		}
	}
}

// refuseWrites makes every op (`update`, `delete`) of a row of table fail inside the database,
// which is how a test makes one statement of a transaction fail after another has succeeded.
func refuseWrites(t *testing.T, s *Store, table, op string) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`create or replace function refuse_copy_write() returns trigger language plpgsql as $$ begin raise exception 'refused by the test'; end $$`,
		`create trigger refuse_copy_write before ` + op + ` on ` + table + ` for each row execute function refuse_copy_write()`,
	} {
		if _, err := s.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("make %s of %s fail: %v", op, table, err)
		}
	}
}

// liftRefusal undoes refuseWrites on table.
func liftRefusal(t *testing.T, s *Store, table string) {
	t.Helper()
	if _, err := s.Pool().Exec(context.Background(), `drop trigger refuse_copy_write on `+table); err != nil {
		t.Fatalf("let %s be written again: %v", table, err)
	}
}

// The choice between the two copies is one transaction: a take that fails part-way leaves both
// rows as they were. Here the older copy, the release bundle, is closed first and the newer
// running copy's take then fails; were the two separate statements, the store would be left with
// the older closed and the newer open for some later pod, and this pod given neither.
func TestAFailedBundleRestoreTakeLeavesBothCopiesAsTheyWere(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 7, runtime.WorkspaceBundle{Bundle: []byte("PACK the release's work"), Head: "b", At: at, PodUID: releasedPodUID}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", runtime.WorkspaceBundle{Bundle: []byte("PACK the running copy"), Head: "r", At: at.Add(time.Minute), PodUID: runningPodUID}); err != nil {
		t.Fatal(err)
	}
	refuseWrites(t, s, runningCopiesTable, "update")
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-replacement", at.Add(time.Hour)); err == nil {
		t.Fatalf("a take whose running copy could not be written answered %+v, want its error", restore)
	}
	for _, copies := range []copyTable{releaseBundleCopies, runningCopyCopies} {
		if row := heldCopyRow(t, s, copies, "INFRA-1117"); !row.held || row.attemptPod != "" || row.outcome != "" {
			t.Fatalf("after the failed take the %s holds %+v, want it as it was before the take", copies.kind, row)
		}
	}
	// The control: the same take with the table writable closes the one and sends the other.
	liftRefusal(t, s, runningCopiesTable)
	if sent, take, err := takeOf(ctx, s, runtime.WorkspaceCopyRunning, "INFRA-1117", "pod-replacement", at.Add(time.Hour)); err != nil || take != runtime.WorkspaceBundleSend {
		t.Fatalf("the take once the table was writable answered %q (%v, %v), want the running copy sent", sent.Bundle, take, err)
	}
	if row := heldCopyRow(t, s, releaseBundleCopies, "INFRA-1117"); row.outcome != supersededCopyOutcome {
		t.Fatalf("the release bundle holds %+v, want it closed unsent", row)
	}
}

// A saved release bundle and a dropped one each take the issue's running copy with them, in their
// own transaction: the bundle is the newer copy of the same workspace, and a release that found
// nothing unpushed, or an issue whose work is over, leaves nothing any pod should restore. A
// running-copy delete that fails fails the whole write, so the bundle's half of it is not left
// standing beside the stale copy.
func TestSavingOrDroppingAReleaseBundleDropsTheRunningCopyInItsOwnTransaction(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	copied := runtime.WorkspaceBundle{Bundle: []byte("PACK the running copy"), Head: "r", At: at, PodUID: runningPodUID}
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the release's work"), Head: "b", At: at.Add(time.Minute), PodUID: releasedPodUID}
	releaseOf := func() int64 {
		t.Helper()
		var release int64
		if err := s.Pool().QueryRow(ctx, `select release_id from workspace_bundles where issue = $1`, "INFRA-1117").Scan(&release); err != nil {
			t.Fatalf("read the release of the bundle: %v", err)
		}
		return release
	}

	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", copied); err != nil {
		t.Fatal(err)
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 7, saved); err != nil {
		t.Fatal(err)
	}
	if row := heldCopyRow(t, s, runningCopyCopies, "INFRA-1117"); row.held {
		t.Fatalf("the release's save left the running copy %+v", row)
	}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", copied); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-1117"); err != nil {
		t.Fatal(err)
	}
	for _, copies := range []copyTable{releaseBundleCopies, runningCopyCopies} {
		if row := heldCopyRow(t, s, copies, "INFRA-1117"); row.held {
			t.Fatalf("the bundle's drop left the %s %+v", copies.kind, row)
		}
	}

	// Each write is one transaction: with the running copy's delete failing, neither the save
	// nor the drop changes the bundle.
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 7, saved); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", copied); err != nil {
		t.Fatal(err)
	}
	refuseWrites(t, s, runningCopiesTable, "delete")
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 8, saved); err == nil {
		t.Fatal("a save whose running-copy delete failed succeeded")
	}
	if release := releaseOf(); release != 7 {
		t.Fatalf("the failed save left the bundle of release %d, want release 7's as it was", release)
	}
	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-1117"); err == nil {
		t.Fatal("a drop whose running-copy delete failed succeeded")
	}
	for _, copies := range []copyTable{releaseBundleCopies, runningCopyCopies} {
		if row := heldCopyRow(t, s, copies, "INFRA-1117"); !row.held {
			t.Fatalf("the failed drop took the %s", copies.kind)
		}
	}
}
