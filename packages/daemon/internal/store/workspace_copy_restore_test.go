package store

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// Both kinds of saved copy keep the same rules — one decision (runtime.DecideWorkspaceRestore)
// taken and recorded in the one transaction that spends their attempt (takeCopy) — so one case
// runs against each table.
//
// The pod the work came off is answered nothing and spends nothing. Every start of every role asks
// for a copy to restore, and that pod goes on starting roles — a release stops its roles and one
// relaunched before the teardown starts again, and a running copy is taken while the pod works.
// Its workspace is the work, and were it to spend the attempt the pod that replaces it, whose
// workspace holds only what was pushed, would be answered nothing. It is the pod's uid that says
// so, and no time.
//
// A pod that first asked before the copy was written is sent nothing of it and nothing is recorded
// on the copy: that pod has been working since its first ask, so the copy is not applied over that
// work, and the copy's one attempt stays for the first pod that asks after the write. Both times
// are the store's, so the answer reads the copy's write off the row and the first ask off the
// store's own record, and names both for the restore's line.
//
// A copy is sent to one pod's launcher once. The launcher's record of the restore it ran dies with
// the process that ran it, so a roles container that restarts comes back to the same disk with
// that record gone and the workspace hours of work past the copy; the daemon's record survives,
// written in the take itself, before a slice leaves the daemon. The bytes stay in every case.
func TestEitherSavedCopyOfAWorkspaceBundleHasOneRestoreAttempt(t *testing.T) {
	const issue = "INFRA-945"
	for _, c := range []struct {
		name      string
		copies    copyTable
		source    string
		save      func(ctx context.Context, s *Store, begun WorkspaceCopyBegun, b runtime.WorkspaceBundle) error
		restoreID func(id int64) bool
	}{
		{
			name: "release bundle", copies: releaseBundleCopies, source: releasedPodUID,
			save: func(ctx context.Context, s *Store, begun WorkspaceCopyBegun, b runtime.WorkspaceBundle) error {
				return s.PutWorkspaceBundle(ctx, issue, 11, begun, b)
			},
			restoreID: func(id int64) bool { return id == 11 },
		},
		{
			name: "running copy", copies: runningCopyCopies, source: runningPodUID,
			save: func(ctx context.Context, s *Store, begun WorkspaceCopyBegun, b runtime.WorkspaceBundle) error {
				return s.PutRunningWorkspaceCopy(ctx, issue, begun, b)
			},
			restoreID: func(id int64) bool { return id >= MinRunningWorkspaceCopyRestoreID },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s := migratedStore(t)
			// A pod whose first role start asked before the copy was written, while the issue
			// held nothing: it starts on what its fetch provisioned and works on.
			const working = "pod-working-before-the-copy"
			if restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, working); err != nil || restore.Of(c.copies.kind).Take != runtime.WorkspaceBundleNone {
				t.Fatalf("a pod of an issue that holds nothing was answered %v (%v), want nothing", restore.Of(c.copies.kind).Take, err)
			}
			saved := runtime.WorkspaceBundle{
				Bundle: []byte("PACK the saved work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
				Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"},
				Bookmarks: map[string]string{"legion/INFRA-945": "dffe88ae2f793e13436cef3921adc988433fbd4e"},
				PodUID:    c.source,
			}
			if err := c.save(ctx, s, copyBegun(t, s), saved); err != nil {
				t.Fatal(err)
			}
			written := heldCopyRow(t, s, c.copies, issue)
			// Two starts in the pod the work came off: neither is answered and neither spends the
			// attempt.
			for range 2 {
				restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, c.source)
				if got := restore.Of(c.copies.kind); err != nil || got.Take != runtime.WorkspaceBundleNone {
					t.Fatalf("the pod the work came off was answered %q (%v, %v); want nothing to send", got.Bundle.Bundle, got.Take, err)
				}
			}
			if row := heldCopyRow(t, s, c.copies, issue); row.attemptPod != "" {
				t.Fatalf("the pod the work came off spent the attempt: %+v", row)
			}

			// The pod that was working before the write asks again, as its next role starts.
			restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, working)
			left := restore.Of(c.copies.kind)
			if err != nil || left.Take != runtime.WorkspaceBundleLeftForNextPod || len(left.Bundle.Bundle) != 0 {
				t.Fatalf("a pod that first asked before the copy was written was answered %v with %d bytes (%v), want nothing sent and the copy left for the next pod",
					left.Take, len(left.Bundle.Bundle), err)
			}
			if !left.Written.Equal(written.writtenAt) || !restore.FirstAsked.Before(written.writtenAt) {
				t.Fatalf("the copy left for the next pod reads as written at %s against a first ask at %s, want the row's write at %s, after that ask: the line names both",
					left.Written, restore.FirstAsked, written.writtenAt)
			}
			if row := heldCopyRow(t, s, c.copies, issue); row.attemptPod != "" || row.outcome != "" {
				t.Fatalf("leaving the copy for the next pod recorded %+v on it, want nothing", row)
			}

			// The first pod to ask after the write is sent the copy whole, and that send is the
			// attempt.
			const after = "pod-after-the-copy"
			restore, err = s.TakeWorkspaceCopyForRestore(ctx, issue, after)
			sent := restore.Of(c.copies.kind)
			if b := sent.Bundle; err != nil || sent.Take != runtime.WorkspaceBundleSend || string(b.Bundle) != string(saved.Bundle) || b.Head != saved.Head ||
				!slices.Equal(b.Tips, saved.Tips) || !maps.Equal(b.Bookmarks, saved.Bookmarks) || !b.At.Equal(written.at) || !c.restoreID(b.Release) {
				t.Fatalf("the first pod to ask after the copy was written was answered %+v (%v, %v); want the copy whole", b, sent.Take, err)
			}
			// The same pod again, which is its roles container restarting, the pod working before
			// the write, and a third pod after an eviction: the spent attempt answers each nothing.
			for _, pod := range []string{after, working, "pod-after-an-eviction"} {
				restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, pod)
				if again := restore.Of(c.copies.kind); err != nil || again.Take != runtime.WorkspaceBundleNone {
					t.Fatalf("%s was answered %q (%v, %v) after the copy's one attempt, want nothing to send", pod, again.Bundle.Bundle, again.Take, err)
				}
			}
			if row := heldCopyRow(t, s, c.copies, issue); !row.held || row.size != saved.Size() || row.outcome != sentCopyOutcome || row.attemptPod != after {
				t.Fatalf("after the send the store holds %+v, want the %d bytes kept and the send recorded against its pod", row, saved.Size())
			}

			// The next save is other work, with an attempt of its own.
			next := saved
			next.Bundle, next.Head = []byte("PACK the next save"), "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9"
			if err := c.save(ctx, s, copyBegun(t, s), next); err != nil {
				t.Fatal(err)
			}
			restore, err = s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-after-the-next-save")
			if b := restore.Of(c.copies.kind); err != nil || b.Take != runtime.WorkspaceBundleSend || string(b.Bundle.Bundle) != string(next.Bundle) {
				t.Fatalf("the pod after the next save was answered %q (%v, %v), want the next save", b.Bundle.Bundle, b.Take, err)
			}

			// A take with no pod to name is refused: the rules have nothing to read.
			if _, err := s.TakeWorkspaceCopyForRestore(ctx, issue, ""); err == nil {
				t.Fatal("a take that named no pod was answered")
			}
		})
	}
}

// The late copy, in the order it happens: a copy of the pod before this one is read while that pod
// works, that pod ends, the new pod's first role start asks and takes whatever stood, and only then
// is the old pod's copy written. The new pod's first restore is not refused: it is sent what stood
// at its ask, or nothing where nothing stood. Every later ask of the new pod — another role's
// start, its roles container restarting — is sent nothing of the late copy and records nothing on
// it: the new pod has been working since its first ask, and the copy would `jj edit` its workspace
// back over that work. The late copy's take began before the new pod's first ask, since the old pod
// was read before it ended, so its own time is before that ask; the ask is weighed against the
// copy's write, which the issue's copy lock orders against the ask, and the copy is left all the
// same. The pod after the new one, whose first ask comes after the write, is sent it. That holds
// only while the new pod takes no copy of its own: the new pod's next copy replaces the late one,
// and a copy of the new pod that finds nothing unpushed drops it, so the late copy's work is lost
// then; that needs a write stalled through a whole pod start, and is accepted.
func TestACopyWrittenAfterAPodFirstAskedIsLeftForThePodAfterIt(t *testing.T) {
	const issue, ended, asked, next = "INFRA-1117", "pod-that-ended", "pod-that-asked-first", "pod-after-it"
	for _, c := range []struct {
		name     string
		standing bool
	}{
		{"a copy stood at the new pod's first ask", true},
		{"nothing stood at the new pod's first ask", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s := migratedStore(t)
			standing := runtime.WorkspaceBundle{Bundle: []byte("PACK the copy that stood"), Head: "c1", PodUID: ended}
			if c.standing {
				if err := s.PutRunningWorkspaceCopy(ctx, issue, copyBegun(t, s), standing); err != nil {
					t.Fatal(err)
				}
			}
			// The ended pod's last copy: its take began and its bundler read the workspace before
			// the pod ended, and it is not yet written.
			late := runtime.WorkspaceBundle{Bundle: []byte("PACK the ended pod's last copy"), Head: "c2",
				Tips: []string{"c2"}, Bookmarks: map[string]string{"side": "c2"}, PodUID: ended}
			lateBegun := copyBegun(t, s)

			restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, asked)
			if err != nil {
				t.Fatal(err)
			}
			first := restore.Running
			if c.standing && (first.Take != runtime.WorkspaceBundleSend || string(first.Bundle.Bundle) != string(standing.Bundle)) {
				t.Fatalf("the new pod's first ask was answered %v of %q, want the copy that stood sent", first.Take, first.Bundle.Bundle)
			}
			if !c.standing && first.Take != runtime.WorkspaceBundleNone {
				t.Fatalf("the new pod's first ask was answered %v, want nothing: nothing stood", first.Take)
			}

			if err := s.PutRunningWorkspaceCopy(ctx, issue, lateBegun, late); err != nil {
				t.Fatal(err)
			}
			row := heldCopyRow(t, s, runningCopyCopies, issue)
			if !row.at.Before(restore.FirstAsked) || !row.writtenAt.After(restore.FirstAsked) {
				t.Fatalf("the late copy was taken at %s and written at %s against the new pod's first ask at %s, want its take before that ask and its write after it",
					row.at, row.writtenAt, restore.FirstAsked)
			}
			for i := range 2 {
				restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, asked)
				got := restore.Running
				if err != nil || got.Take == runtime.WorkspaceBundleSend || len(got.Bundle.Bundle) != 0 {
					t.Fatalf("ask %d of the new pod after the late copy was written was answered %v with %d bytes (%v), want nothing sent",
						i+2, got.Take, len(got.Bundle.Bundle), err)
				}
				if i == 0 && (got.Take != runtime.WorkspaceBundleLeftForNextPod || !got.Written.Equal(row.writtenAt) || !restore.FirstAsked.Before(row.writtenAt)) {
					t.Fatalf("the new pod's second ask was answered %v of a copy written at %s against its first ask at %s, want the late copy written at %s left for the next pod",
						got.Take, got.Written, restore.FirstAsked, row.writtenAt)
				}
			}

			restore, err = s.TakeWorkspaceCopyForRestore(ctx, issue, next)
			sent := restore.Running
			if err != nil || sent.Take != runtime.WorkspaceBundleSend || string(sent.Bundle.Bundle) != string(late.Bundle) ||
				sent.Bundle.Head != late.Head || !slices.Equal(sent.Bundle.Tips, late.Tips) || !maps.Equal(sent.Bundle.Bookmarks, late.Bookmarks) {
				t.Fatalf("the pod after the new one was answered %v of %q (%v), want the late copy's work whole", sent.Take, sent.Bundle.Bundle, err)
			}
			if row := heldCopyRow(t, s, runningCopyCopies, issue); row.attemptPod != next || row.outcome != sentCopyOutcome {
				t.Fatalf("the late copy holds %+v, want its one attempt the send to %s", row, next)
			}
		})
	}
}

// heldCopyWrite is the advisory lock key a test holds to keep every insert of a running copy in
// flight: the row's time is stamped and its transaction has not committed (holdCopyWrites).
const heldCopyWrite = 4242

// holdCopyWrites makes every insert of a running copy wait, once its row is formed and its time
// stamped, until the test lets heldCopyWrite go, and answers the call that lets it go. The test
// holds the key in a transaction of its own; the trigger takes it shared, inside the copy's write.
func holdCopyWrites(t *testing.T, s *Store) func() {
	t.Helper()
	ctx := context.Background()
	held, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.Exec(ctx, `select pg_advisory_xact_lock($1)`, heldCopyWrite); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`create function hold_copy_write() returns trigger language plpgsql as $$
			begin perform pg_advisory_xact_lock_shared(4242); return new; end $$`,
		`create trigger hold_copy_write before insert on workspace_running_copies
			for each row execute function hold_copy_write()`,
	} {
		if _, err := s.Pool().Exec(ctx, statement); err != nil {
			t.Fatalf("hold the copy writes: %v", err)
		}
	}
	let := func() { _ = held.Rollback(ctx) }
	t.Cleanup(let)
	return let
}

// advisoryWaiters is how many transactions of this test's database wait for an advisory lock.
func advisoryWaiters(t *testing.T, s *Store) int {
	t.Helper()
	var waiting int
	if err := s.Pool().QueryRow(context.Background(), `select count(*) from pg_locks
		where locktype = 'advisory' and not granted
			and database = (select oid from pg_database where datname = current_database())`).Scan(&waiting); err != nil {
		t.Fatalf("count the advisory lock waiters: %v", err)
	}
	return waiting
}

// A copy's write and a pod's first ask of one issue each take the issue's copy lock first and read
// the clock only once they hold it, so whichever comes first has committed before the other reads
// the clock. Here the write of the ended pod's copy has stamped its write time and is held before
// it commits when the new pod's first role start asks. The ask waits for the write, and so it reads
// the copy and is sent it at that first ask, and its next ask is sent nothing. Were the two not
// taken in one order, the ask would be recorded after the copy's write time while the copy was not
// yet committed, it would read nothing, and its next ask — another role's start, hours of work on —
// would be sent a copy written before its first ask, over that work.
func TestACopyWriteInFlightWhenAPodFirstAsksIsReadAtThatAsk(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	const issue, pod = "INFRA-1117", "pod-that-asks"
	let := holdCopyWrites(t, s)
	copied := runtime.WorkspaceBundle{Bundle: []byte("PACK the ended pod's copy"), Head: "c1", PodUID: "pod-that-ended"}
	begun := copyBegun(t, s)

	written := make(chan error, 1)
	go func() { written <- s.PutRunningWorkspaceCopy(ForConcurrentWorker(ctx), issue, begun, copied) }()
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool { return advisoryWaiters(t, s) == 1 }) {
		t.Fatal("the copy's write never reached the hold")
	}

	type answer struct {
		restore runtime.WorkspaceRestore
		err     error
	}
	asked := make(chan answer, 1)
	go func() {
		restore, err := s.TakeWorkspaceCopyForRestore(ForConcurrentWorker(ctx), issue, pod)
		asked <- answer{restore, err}
	}()
	// The ask either waits behind the write, which is a second waiter, or answers at once.
	var first answer
	answered := false
	if !testwait.Holds(testwait.Bounded(t, 30*time.Second, time.Second), testwait.Poll, func() bool {
		select {
		case first = <-asked:
			answered = true
			return true
		default:
			return advisoryWaiters(t, s) == 2
		}
	}) {
		t.Fatal("the pod's first ask neither waited nor answered")
	}
	let()
	if err := <-written; err != nil {
		t.Fatalf("write the copy: %v", err)
	}
	if !answered {
		first = <-asked
	}
	writtenAt := heldCopyRow(t, s, runningCopyCopies, issue).writtenAt
	if sent := first.restore.Running; first.err != nil || sent.Take != runtime.WorkspaceBundleSend || string(sent.Bundle.Bundle) != string(copied.Bundle) {
		t.Fatalf("the pod's first ask, made while the copy's write was in flight, was answered %v of %q (%v), want the copy sent at that ask",
			sent.Take, sent.Bundle.Bundle, first.err)
	}
	if !writtenAt.Before(first.restore.FirstAsked) {
		t.Fatalf("the copy was written at %s and the pod first asked at %s, want the write first", writtenAt, first.restore.FirstAsked)
	}
	if again, err := s.TakeWorkspaceCopyForRestore(ctx, issue, pod); err != nil || again.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the pod's next ask was answered %v (%v), want nothing: the copy had its one attempt at the first", again.Running.Take, err)
	}
}

// recordPush records the push of issue's branch from before to after as the workflow's push
// handler does, in a transaction of its own, and answers whether it moved the record.
func recordPush(t *testing.T, s *Store, issue, before, after string) bool {
	t.Helper()
	var moved bool
	if err := s.Tx(context.Background(), func(tx pgx.Tx) error {
		recorded, err := s.RecordIssuePush(context.Background(), tx, issue, before, after)
		moved = recorded
		return err
	}); err != nil {
		t.Fatalf("record the push of %s from %s to %s: %v", issue, before, after, err)
	}
	return moved
}

// A running copy older than the newest push of the issue's branch the daemon recorded is sent to
// no pod. Restoring it would `jj edit` back to its head and move each bookmark `--allow-backwards`
// behind the pushed commits. An earlier release's bundle beside it, older still, is not sent in
// its place: the running copy is the newer of the two, so the bundle is closed unsent, as it would
// be beside any newer copy. Each take spends its copy's attempt and keeps its bytes, and the
// answer names the push it read, which the restore's warning carries.
//
// A push recorded late, behind a newer one, moves neither the time nor the head, although the
// store records it later; and an issue with no push recorded refuses nothing by this rule. The
// copy the daemon takes after the push is a new row with an attempt of its own, and it is the one
// the next pod is sent. Every time here is the store's, so the order the takes begin in is the
// order the rule reads.
func TestARunningCopyOlderThanTheNewestRecordedPushIsSentToNoPod(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	earlier := runtime.WorkspaceBundle{Bundle: []byte("PACK an earlier release's work"), Head: "c0", PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 7, copyBegun(t, s), earlier); err != nil {
		t.Fatal(err)
	}
	running := runtime.WorkspaceBundle{Bundle: []byte("PACK the copy before the push"), Head: "c1", PodUID: runningPodUID}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", copyBegun(t, s), running); err != nil {
		t.Fatal(err)
	}
	recordPush(t, s, "INFRA-1117", "c1", "c2")
	pushed := newestPush(t, s, "INFRA-1117")
	recordPush(t, s, "INFRA-1117", "c0", "c1")

	restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-after-the-push")
	if err != nil {
		t.Fatal(err)
	}
	if !restore.Push.At.Equal(pushed.At) || restore.Push.Head != "c2" {
		t.Fatalf("the take read the push %+v, want c2 at %s: a push recorded late moved the record", restore.Push, pushed.At)
	}
	if refused := restore.Running; refused.Take != runtime.WorkspaceBundleRefusedPushed || len(refused.Bundle.Bundle) != 0 {
		t.Fatalf("the running copy was answered %v with %d bytes, want it refused as older than the push with no body", refused.Take, len(refused.Bundle.Bundle))
	}
	if closed := restore.ReleaseBundle; closed.Take != runtime.WorkspaceBundleSuperseded || len(closed.Bundle.Bundle) != 0 {
		t.Fatalf("the release bundle was answered %v with %d bytes, want it closed unsent by the newer running copy", closed.Take, len(closed.Bundle.Bundle))
	}
	for copies, outcome := range map[copyTable]string{runningCopyCopies: refusedPushedOutcome, releaseBundleCopies: supersededCopyOutcome} {
		if row := heldCopyRow(t, s, copies, "INFRA-1117"); !row.held || row.outcome != outcome || row.attemptPod != "pod-after-the-push" {
			t.Fatalf("the %s holds %+v, want its bytes kept and its attempt spent as %s", copies.kind, row, outcome)
		}
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-after-that"); err != nil ||
		restore.ReleaseBundle.Take != runtime.WorkspaceBundleNone || restore.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the pod after was answered %+v (%v), want nothing: each take was its copy's one attempt", restore, err)
	}

	// The copy the daemon takes once it has recorded the push is newer than it, and is sent to the
	// next pod to ask.
	after := runtime.WorkspaceBundle{Bundle: []byte("PACK the copy after the push"), Head: "c3", PodUID: runningPodUID}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", copyBegun(t, s), after); err != nil {
		t.Fatal(err)
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-after-the-copy"); err != nil ||
		restore.Running.Take != runtime.WorkspaceBundleSend || string(restore.Running.Bundle.Bundle) != string(after.Bundle) {
		t.Fatalf("the copy taken after the push was answered %q (%v, %v), want it sent", restore.Running.Bundle.Bundle, restore.Running.Take, err)
	}

	// An issue whose branch has no push recorded refuses nothing by this rule.
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-2760", copyBegun(t, s), running); err != nil {
		t.Fatal(err)
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-2760", "pod-replacement"); err != nil || restore.Running.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("a copy of an issue with no push recorded was answered %v (%v), want it sent", restore.Running.Take, err)
	}
}

// A copy whose take began before a push and whose write lands after the push was recorded is
// refused by the push all the same. Its bundler read the workspace after the take began and
// possibly before the pushed commits were made, so restoring it would put the next pod's workspace
// back behind them. The daemon marks a copy in flight overtaken only once the push's transaction
// has committed, so such a copy can reach the store; its time is when its take began, and that is
// before the push. Stamped at its write it would be after the push, and sent. A copy whose take
// begins after the push's record is sent.
func TestARunningCopyWhoseTakeBeganBeforeAPushIsRefusedHoweverLateItIsWritten(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	const issue = "INFRA-1117"
	read := runtime.WorkspaceBundle{Bundle: []byte("PACK the workspace as read before the push"), Head: "c1", PodUID: runningPodUID}
	begun := copyBegun(t, s)
	recordPush(t, s, issue, "c1", "c2")
	pushed := newestPush(t, s, issue)
	if err := s.PutRunningWorkspaceCopy(ctx, issue, begun, read); err != nil {
		t.Fatal(err)
	}
	row := heldCopyRow(t, s, runningCopyCopies, issue)
	if !row.at.Before(pushed.At) || !row.writtenAt.After(pushed.At) {
		t.Fatalf("the copy was taken at %s and written at %s against the push recorded at %s, want its take before the push and its write after it",
			row.at, row.writtenAt, pushed.At)
	}
	restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-after-the-push")
	if refused := restore.Running; err != nil || refused.Take != runtime.WorkspaceBundleRefusedPushed || len(refused.Bundle.Bundle) != 0 {
		t.Fatalf("the copy read before the push was answered %v with %d bytes (%v), want it refused as older than the push", refused.Take, len(refused.Bundle.Bundle), err)
	}
	if !restore.Running.Bundle.At.Equal(row.at) {
		t.Fatalf("the refused copy reads as taken at %s, want the row's %s, which the restore's warning names", restore.Running.Bundle.At, row.at)
	}

	// The copy the push asks for begins its take after the push's record, and is sent.
	after := runtime.WorkspaceBundle{Bundle: []byte("PACK the workspace as read after the push"), Head: "c2", PodUID: runningPodUID}
	if err := s.PutRunningWorkspaceCopy(ctx, issue, copyBegun(t, s), after); err != nil {
		t.Fatal(err)
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-after-the-copy"); err != nil ||
		restore.Running.Take != runtime.WorkspaceBundleSend || string(restore.Running.Bundle.Bundle) != string(after.Bundle) {
		t.Fatalf("the copy taken after the push was answered %q (%v, %v), want it sent", restore.Running.Bundle.Bundle, restore.Running.Take, err)
	}
}

// A release bundle older than the newest push of the issue's branch the daemon recorded is sent
// all the same. The release took it while the issue's roles were idle, before it stopped them, so
// a push recorded after it — GitHub's "Update branch", a person's push, a webhook the daemon
// applied late — holds none of its work, and the bundle is the one place that work is. The
// launcher's confirmation of the restore then moves it out of the bundle table, so the row that
// would hold the issue's pod (workspaceBundleUnrestored) is gone, and keeps it as the restoring
// pod's running copy, both its times stamped at the confirmation: the pod after that one, made by
// a second end before any turn of the first, is sent the work, and the push recorded before the
// confirmation refuses it no more than it refused the bundle.
func TestAReleaseBundleOlderThanTheNewestRecordedPushIsSentAndItsConfirmationLiftsTheHold(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	unrestored := func() bool {
		t.Helper()
		var held bool
		if err := s.Tx(ctx, func(tx pgx.Tx) error {
			var err error
			held, err = workspaceBundleUnrestored(ctx, tx, "INFRA-1117")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return held
	}
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the work the release saved"), Head: "c1",
		Tips: []string{"c1"}, Bookmarks: map[string]string{"side": "c1"}, PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 7, copyBegun(t, s), saved); err != nil {
		t.Fatal(err)
	}
	recordPush(t, s, "INFRA-1117", "c0", "c2")
	pushed := newestPush(t, s, "INFRA-1117")
	if bundle := heldCopyRow(t, s, releaseBundleCopies, "INFRA-1117"); !bundle.at.Before(pushed.At) {
		t.Fatalf("the bundle was taken at %s and the push recorded at %s, want the push after it", bundle.at, pushed.At)
	}

	restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-after-the-push")
	if err != nil {
		t.Fatal(err)
	}
	if !restore.Push.At.Equal(pushed.At) || restore.Push.Head != "c2" {
		t.Fatalf("the take read the push %+v, want c2 at %s", restore.Push, pushed.At)
	}
	sent := restore.ReleaseBundle
	if sent.Take != runtime.WorkspaceBundleSend || restore.Offered != runtime.WorkspaceCopyReleaseBundle ||
		string(sent.Bundle.Bundle) != string(saved.Bundle) || sent.Bundle.Head != saved.Head || !maps.Equal(sent.Bundle.Bookmarks, saved.Bookmarks) {
		t.Fatalf("the release bundle taken before the push was answered %+v with %q offered, want it offered and sent whole", sent, restore.Offered)
	}
	if row := heldCopyRow(t, s, releaseBundleCopies, "INFRA-1117"); !row.held || row.outcome != sentCopyOutcome || row.attemptPod != "pod-after-the-push" {
		t.Fatalf("the release bundle holds %+v, want its attempt spent sending it", row)
	}
	if !unrestored() {
		t.Fatal("the bundle reads as restored before any launcher confirmed it")
	}
	if err := s.KeepRestoredWorkspaceCopy(ctx, "INFRA-1117", "pod-after-the-push", sent.Bundle.Release); err != nil {
		t.Fatal(err)
	}
	if unrestored() {
		t.Fatal("the confirmed restore of the bundle still reads as work no pod has restored, which holds the issue's pod")
	}
	restore, err = s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-after-the-second-end")
	if err != nil {
		t.Fatal(err)
	}
	if again := restore.Running; again.Take != runtime.WorkspaceBundleSend || string(again.Bundle.Bundle) != string(saved.Bundle) ||
		again.Bundle.Head != saved.Head || !maps.Equal(again.Bundle.Bookmarks, saved.Bookmarks) {
		t.Fatalf("the pod after a second end was answered %v with %q at %s, want the restored bundle's work sent whole and not refused by the push",
			again.Take, again.Bundle.Bundle, again.Bundle.Head)
	}
}

// An issue can hold both copies at once, and the pod that replaces its pod is offered the newer
// while the older is closed unsent in the same take: the pods after this one hold the descendants
// of the work it restores, so a copy left open for them would be applied over everything pushed in
// between. The store holds both only with the running copy the newer: a release's save drops the
// running copy in its own transaction, and every copy taken after it begins its take later by the
// store's clock, so the release bundle is offered beside a running copy in the decision's own
// table alone (runtime.DecideWorkspaceRestore), where a tie goes to it.
func TestTheBundleRestoreOffersTheNewerCopyAndSupersedesTheOlderInOneTake(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 7, copyBegun(t, s), runtime.WorkspaceBundle{Bundle: []byte("PACK the release's work"), Head: "b", PodUID: releasedPodUID}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", copyBegun(t, s), runtime.WorkspaceBundle{Bundle: []byte("PACK the running copy"), Head: "r", PodUID: runningPodUID}); err != nil {
		t.Fatal(err)
	}
	restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-replacement")
	if err != nil {
		t.Fatal(err)
	}
	if sent := restore.Running; sent.Take != runtime.WorkspaceBundleSend || len(sent.Bundle.Bundle) == 0 {
		t.Fatalf("the running copy was answered %+v, want it sent", sent)
	}
	if closed := restore.ReleaseBundle; closed.Take != runtime.WorkspaceBundleSuperseded || len(closed.Bundle.Bundle) != 0 || closed.Size == 0 {
		t.Fatalf("the release bundle was answered %+v, want it closed unsent with its size named", closed)
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
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 7, copyBegun(t, s), runtime.WorkspaceBundle{Bundle: []byte("PACK the release's work"), Head: "b", PodUID: releasedPodUID}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", copyBegun(t, s), runtime.WorkspaceBundle{Bundle: []byte("PACK the running copy"), Head: "r", PodUID: runningPodUID}); err != nil {
		t.Fatal(err)
	}
	refuseWrites(t, s, runningCopiesTable, "update")
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-replacement"); err == nil {
		t.Fatalf("a take whose running copy could not be written answered %+v, want its error", restore)
	}
	for _, copies := range []copyTable{releaseBundleCopies, runningCopyCopies} {
		if row := heldCopyRow(t, s, copies, "INFRA-1117"); !row.held || row.attemptPod != "" || row.outcome != "" {
			t.Fatalf("after the failed take the %s holds %+v, want it as it was before the take", copies.kind, row)
		}
	}
	// The control: the same take with the table writable closes the one and sends the other.
	liftRefusal(t, s, runningCopiesTable)
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-replacement"); err != nil || restore.Running.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("the take once the table was writable answered %q (%v, %v), want the running copy sent", restore.Running.Bundle.Bundle, restore.Running.Take, err)
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
	copied := runtime.WorkspaceBundle{Bundle: []byte("PACK the running copy"), Head: "r", PodUID: runningPodUID}
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the release's work"), Head: "b", PodUID: releasedPodUID}
	releaseOf := func() int64 {
		t.Helper()
		var release int64
		if err := s.Pool().QueryRow(ctx, `select release_id from workspace_bundles where issue = $1`, "INFRA-1117").Scan(&release); err != nil {
			t.Fatalf("read the release of the bundle: %v", err)
		}
		return release
	}

	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", copyBegun(t, s), copied); err != nil {
		t.Fatal(err)
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 7, copyBegun(t, s), saved); err != nil {
		t.Fatal(err)
	}
	if row := heldCopyRow(t, s, runningCopyCopies, "INFRA-1117"); row.held {
		t.Fatalf("the release's save left the running copy %+v", row)
	}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", copyBegun(t, s), copied); err != nil {
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
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 7, copyBegun(t, s), saved); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", copyBegun(t, s), copied); err != nil {
		t.Fatal(err)
	}
	refuseWrites(t, s, runningCopiesTable, "delete")
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 8, copyBegun(t, s), saved); err == nil {
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
