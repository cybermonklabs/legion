package store

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// copyAct is one of the store's acts on an issue's copies whose time the restore compares, as
// TestEveryInterleavingOfTheCopysTimesKeepsBothInvariants runs it.
type copyAct int

const (
	// actTake begins the take of a copy of the working pod (BeginWorkspaceCopy).
	actTake copyAct = iota
	// actWrite writes that copy (PutRunningWorkspaceCopy).
	actWrite
	// actPush records a push of the issue's branch (RecordIssuePush).
	actPush
	// actAsk is the replacement pod's first ask for a restore (TakeWorkspaceCopyForRestore).
	actAsk
	// actConfirm is the replacement pod's confirmation of what that ask sent it
	// (KeepRestoredWorkspaceCopy). A pod that was sent nothing confirms nothing.
	actConfirm
	// actSave is an idle release of the working pod saving its bundle: the release's mark, the
	// begin of the save's own take (BeginWorkspaceCopy) and the bundle's write (PutWorkspaceBundle).
	actSave
	// actCancel cancels that release (CancelIssueRelease).
	actCancel
	copyActs
)

// String is the act's name as an order's failure line spells the order.
func (a copyAct) String() string {
	return [copyActs]string{"take", "write", "push", "ask", "confirm", "save", "cancel"}[a]
}

// follows is the act a must come after, if any: a copy is written after its take began, a pod
// confirms what its ask sent it, and a release is cancelled after its save.
func (a copyAct) follows() (copyAct, bool) {
	switch a {
	case actWrite:
		return actTake, true
	case actConfirm:
		return actAsk, true
	case actCancel:
		return actSave, true
	}
	return 0, false
}

// copyActOrders is every order of the seven acts in which each act comes after the act it follows:
// every permutation of them, built one place at a time, that places no act before the act it needs.
// The acts are tried in their declared order at each place, so every run yields the same orders in
// the same sequence.
func copyActOrders() [][]copyAct {
	var orders [][]copyAct
	order := make([]copyAct, 0, copyActs)
	var placed [copyActs]bool
	var place func()
	place = func() {
		if len(order) == int(copyActs) {
			orders = append(orders, slices.Clone(order))
			return
		}
		for act := range copyActs {
			if need, ok := act.follows(); placed[act] || ok && !placed[need] {
				continue
			}
			placed[act] = true
			order = append(order, act)
			place()
			order = order[:len(order)-1]
			placed[act] = false
		}
	}
	place()
	return orders
}

// copyWork is a set of units of the working pod's work, one bit a unit: unit 0 is the unpushed
// work that pod holds as an order begins, and unit i+1 is the work it does just before act i of
// the order, while it works.
type copyWork uint16

// String is the set's units in ascending order, as a failure line names the work.
func (w copyWork) String() string {
	units := make([]string, 0, bits.OnesCount16(uint16(w)))
	for u := range 16 {
		if w&(1<<u) != 0 {
			units = append(units, fmt.Sprint(u))
		}
	}
	return "{" + strings.Join(units, " ") + "}"
}

// The three pods of an order, and the two copies' contents. Each copy's content is its own, so the
// bytes a pod is sent say which copy it was, and the pod a running copy names says whether a
// confirmation or a cancel moved it there.
const (
	interleavingWorkingPod     = "pod-a-working"
	interleavingReplacementPod = "pod-b-replacement"
	interleavingNextPod        = "pod-c-after-the-second-end"
	interleavingTakenCopy      = "PACK the working pod's copy"
	interleavingSavedBundle    = "PACK the working pod's release bundle"
)

// copyOrder is what one order's model holds, as plain values beside the store: when each act ran,
// which work the working pod A holds and whether it works, which work each copy carries, what the
// replacement pod B holds, and what the pushes carry. Of the store it keeps only what an act
// answered: what a pod was sent, and whether B's confirmation kept a copy.
type copyOrder struct {
	name string
	at   [copyActs]int
	ran  [copyActs]bool

	// a is what A's workspace holds. A works until B first asks, apart from the time its
	// release has stopped its roles, from the save until the cancel.
	a    copyWork
	idle bool

	// taken is the work A's copy carries, read as its take began, and saved the work its release
	// bundle carries, read as the save's take began. The copy the cancel moves carries the bundle's.
	taken, saved copyWork

	// b is what B's workspace holds from its first ask on: what was pushed by then, which its own
	// fetch put there, and what that ask sent it.
	b copyWork

	// pushed is what the push carried, which is on GitHub and lost with no pod.
	pushed copyWork

	// kept is whether B's confirmation kept what B restored as B's own running copy, taken at the
	// confirmation. A confirmation of a copy the store no longer holds keeps nothing.
	kept bool
}

// works is whether A does work before the act about to run: from the start until B first asks,
// apart from the time its release has stopped its roles.
func (o *copyOrder) works() bool { return !o.ran[actAsk] && !o.idle }

// sent is the work the copy a restore sent carries, nothing when it sent none, and the copy as the
// store answered it. It checks the send against the first invariant: a running copy is sent only
// when its take began at or after the newest push recorded before the send, its take being the
// act the model knows it by — A's copy's take, the cancel that moved the release bundle into the
// running copy, or the confirmation that kept what B restored as B's own. A release bundle is
// exempt.
func (o *copyOrder) sent(t *testing.T, pod string, restore runtime.WorkspaceRestore) (copyWork, runtime.WorkspaceCopyTaken) {
	t.Helper()
	taken := restore.Of(restore.Offered)
	if taken.Take != runtime.WorkspaceBundleSend {
		return 0, taken
	}
	var work copyWork
	switch string(taken.Bundle.Bundle) {
	case interleavingTakenCopy:
		work = o.taken
	case interleavingSavedBundle:
		work = o.saved
	default:
		t.Fatalf("%s: pod %s was sent %q, which is no copy this order wrote", o.name, pod, taken.Bundle.Bundle)
	}
	if restore.Offered == runtime.WorkspaceCopyReleaseBundle {
		return work, taken
	}
	var took copyAct
	switch {
	case taken.Bundle.PodUID == interleavingReplacementPod:
		took = actConfirm
	case taken.Bundle.PodUID == interleavingWorkingPod && string(taken.Bundle.Bundle) == interleavingTakenCopy:
		took = actTake
	case taken.Bundle.PodUID == interleavingWorkingPod:
		took = actCancel
	default:
		t.Fatalf("%s: pod %s was sent a running copy off pod %q, which no act of this order names", o.name, pod, taken.Bundle.PodUID)
	}
	if !o.ran[took] {
		t.Fatalf("%s: pod %s was sent the running copy %q off pod %s, which only the %s writes, and it has not run",
			o.name, pod, taken.Bundle.Bundle, taken.Bundle.PodUID, took)
	}
	if o.ran[actPush] && o.at[took] < o.at[actPush] {
		t.Errorf("%s: invariant 1: pod %s was sent the running copy %q off pod %s carrying %v, whose take began at the %s (act %d), before the push recorded at act %d",
			o.name, pod, taken.Bundle.Bundle, taken.Bundle.PodUID, work, took, o.at[took], o.at[actPush])
	}
	return work, taken
}

// markInterleavingRelease is the mark an idle release of issue writes before its save
// (ReleaseIdleIssue), written by hand with no role suspended, so the cancel has the release to end
// and no claim to resume; it answers the release's id, which the save stores its bundle under.
func markInterleavingRelease(t *testing.T, s *Store, issue string) int64 {
	t.Helper()
	var release int64
	if err := s.Pool().QueryRow(context.Background(), `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role)
		values ('legion', $1, 1, 1, 1, 1, 0, 'architect') returning id`, issue).Scan(&release); err != nil {
		t.Fatalf("mark the release of %s: %v", issue, err)
	}
	return release
}

// runningCopySource is the pod issue's running copy names, "" when the issue holds none. Only a
// confirmation names the pod that restored the work, so a row naming B says B's confirmation kept
// a copy.
func runningCopySource(t *testing.T, s *Store, issue string) string {
	t.Helper()
	var source string
	if err := s.Pool().QueryRow(context.Background(), `select coalesce(
		(select source_pod_uid from workspace_running_copies where issue = $1), '')`, issue).Scan(&source); err != nil {
		t.Fatalf("read the pod the running copy of %s names: %v", issue, err)
	}
	return source
}

// runCopyOrder runs order on issue, checking each send against the first invariant as it is made,
// and then the order's end against the second (TestEveryInterleavingOfTheCopysTimesKeepsBothInvariants).
// It answers how many sends it checked and whether work was lost to the stated exception alone.
func runCopyOrder(t *testing.T, s *Store, issue string, order []copyAct) (sends int, excepted bool) {
	t.Helper()
	ctx := context.Background()
	recordCopyIssues(t, s, issue)
	names := make([]string, len(order))
	for i, act := range order {
		names[i] = act.String()
	}
	o := copyOrder{name: "order " + strings.Join(names, " "), a: 1}
	var begun WorkspaceCopyBegun
	var confirmed int64
	var confirm bool
	for step, act := range order {
		if o.works() {
			o.a |= 1 << (step + 1)
		}
		o.at[act], o.ran[act] = step, true
		switch act {
		case actTake:
			begun = copyBegun(t, s)
			o.taken = o.a
		case actWrite:
			// A copy off a pod whose release bundle stands is refused, and so is one whose take
			// began before the stored running copy's. The model counts the copy's work as work a
			// copy carried all the same, so a refusal that dropped work no other copy holds fails
			// the second invariant.
			err := s.PutRunningWorkspaceCopy(ctx, copyProject, issue, begun, runtime.WorkspaceBundle{
				Bundle: []byte(interleavingTakenCopy), Head: "c-taken", PodUID: interleavingWorkingPod})
			if err != nil && !errors.Is(err, ErrReleaseBundleSaved) && !errors.Is(err, ErrNewerCopyStands) {
				t.Fatalf("%s: write the copy: %v", o.name, err)
			}
		case actPush:
			recordPush(t, s, issue, "c-before-the-push", "c-pushed")
			// The push carries the work of the pod that pushed: B's once B works, A's while A
			// works, and neither's while A's release has stopped its roles, when what the daemon
			// records is a push from outside the pod, such as GitHub's "Update branch".
			switch {
			case o.ran[actAsk]:
				o.pushed |= o.b
			case !o.idle:
				o.pushed |= o.a
			}
		case actAsk:
			restore, err := s.TakeWorkspaceCopyForRestore(ctx, copyProject, issue, interleavingReplacementPod)
			if err != nil {
				t.Fatalf("%s: the replacement pod's first ask: %v", o.name, err)
			}
			work, taken := o.sent(t, interleavingReplacementPod, restore)
			o.b = o.pushed | work
			if taken.Take == runtime.WorkspaceBundleSend {
				sends++
				confirmed, confirm = taken.Bundle.Release, true
			}
		case actConfirm:
			if confirm {
				if err := s.KeepRestoredWorkspaceCopy(ctx, issue, interleavingReplacementPod, confirmed); err != nil {
					t.Fatalf("%s: the replacement pod's confirmation: %v", o.name, err)
				}
				o.kept = runningCopySource(t, s, issue) == interleavingReplacementPod
			}
		case actSave:
			release := markInterleavingRelease(t, s, issue)
			if err := s.PutWorkspaceBundle(ctx, issue, release, copyBegun(t, s), runtime.WorkspaceBundle{
				Bundle: []byte(interleavingSavedBundle), Head: "c-saved", PodUID: interleavingWorkingPod}); err != nil {
				t.Fatalf("%s: save the release bundle: %v", o.name, err)
			}
			o.saved, o.idle = o.a, true
		case actCancel:
			if _, err := s.CancelIssueRelease(ctx, "legion", issue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()}); err != nil {
				t.Fatalf("%s: cancel the release: %v", o.name, err)
			}
			o.idle = false
		}
	}

	// B ends from outside the pod with no act of its own — no copy, no release — and C first asks.
	restore, err := s.TakeWorkspaceCopyForRestore(ctx, copyProject, issue, interleavingNextPod)
	if err != nil {
		t.Fatalf("%s: the next pod's first ask: %v", o.name, err)
	}
	received, taken := o.sent(t, interleavingNextPod, restore)
	if taken.Take == runtime.WorkspaceBundleSend {
		sends++
	}

	// The work only stored copies hold: what the copies handed to the store carried, which no
	// push carried, now that no pod of the issue is live. A copy B confirmed carries what B was
	// sent, which a copy handed to the store carried already.
	stored := (o.taken | o.saved) &^ o.pushed
	missing := stored &^ received
	if missing == 0 {
		return sends, false
	}
	// A late copy is one of A's written after B first asked: its own copy, the release bundle, or
	// the copy the cancel moved, which is the bundle's work written at the cancel. The working pod
	// B's own copy taken after such a copy's take began outranks it, so the work that late copies
	// alone carried is lost to that copy: the stated exception. B's own copy is either of two. One
	// is the copy the daemon takes at each push of B it records (internal/daemon's
	// copyWorkspaceAtPush), which replaces the late copy; the test records the push alone, so the
	// late copy stays and the push refuses it instead. The other is the copy B's confirmation
	// keeps as B's own, taken at the confirmation, which the store keeps over a late copy whose
	// write stalled through B's start, restore and confirmation (PutRunningWorkspaceCopy,
	// ErrNewerCopyStands): the orders `push save cancel take ask confirm write`, `push save cancel
	// ask take confirm write`, `save push cancel take ask confirm write`, `save push cancel ask
	// take confirm write` and `save cancel ask push take confirm write`, where A's copy, taken
	// after the cancel resumed A's roles, carries the work A did since. A copy written before B
	// first asked is no late copy, and work it carried is never excused.
	late := func(written copyAct) bool { return o.at[written] > o.at[actAsk] }
	pushedAfter := func(took copyAct) bool { return o.at[actPush] > o.at[actAsk] && o.at[actPush] > o.at[took] }
	keptAfter := func(took copyAct) bool { return o.kept && o.at[actConfirm] > o.at[took] }
	var forfeited, onTime copyWork
	for _, c := range []struct {
		work          copyWork
		took, written copyAct
	}{{o.taken, actTake, actWrite}, {o.saved, actSave, actSave}, {o.saved, actCancel, actCancel}} {
		switch {
		case !late(c.written):
			onTime |= c.work
		case pushedAfter(c.took), keptAfter(c.took):
			forfeited |= c.work
		}
	}
	if missing&^(forfeited&^onTime) == 0 {
		return sends, true
	}
	t.Errorf("%s: invariant 2: after pod %s ended, stored copies alone held %v, which no push carried, and pod %s was sent %v (%v of the running copy, %v of the release bundle); the exception covers none of %v, which no late copy alone carried with a copy of the working pod's own taken after it",
		o.name, interleavingReplacementPod, stored, interleavingNextPod, received,
		restore.Running.Take, restore.ReleaseBundle.Take, missing&^(forfeited&^onTime))
	return sends, false
}

// TestEveryInterleavingOfTheCopysTimesKeepsBothInvariants runs the store's acts whose times the
// restore compares — the take of the working pod's copy, that copy's write, a push's record, the
// replacement pod's first ask, its confirmation of what it was sent, an idle release's save and
// that release's cancel — over one issue in every order in which a copy is written after its take
// began, a confirmation follows its ask, and a cancel follows its release's save: 630 orders, each
// on an issue of its own. After each order it checks two invariants.
//
//  1. No pod is sent a running copy whose take began before the newest push recorded before the
//     send. A release bundle is exempt: the release took it while the roles were idle, so no push
//     recorded after it is a push of their work. A running copy a cancel or a confirmation wrote
//     began its take at that act, when it began to stand for a live pod's workspace.
//  2. No work that a live pod does not hold is dropped while another stored copy could carry it,
//     with the one stated exception that the working pod's own next copy replaces a late copy, a
//     copy written after a pod's first ask. Each order ends with the replacement pod ended from
//     outside, with no act of its own, and a next pod's first ask: the work the copies handed to
//     the store carried and no push carried must reach that pod, or be lost to that exception
//     alone.
//
// The model is plain values beside the store (copyOrder). Pod A works and is copied; pod B replaces
// it, first asks, and is the working pod from then on; pod C asks after B's end. A holds a unit of
// work as the order begins and does one more before each act while it works, which is until B
// first asks, apart from the time its release has stopped its roles, from the save to the cancel.
// A copy carries what A held as its take began; B holds what was pushed by its first ask and what
// that ask sent it. A push carries the work of the pod that pushed: A's before B first asks, B's
// after, and nobody's while A's release has stopped A's roles, when a push the daemon records is
// one from outside the pod. The working pod's own next copy is either of B's own copies taken
// after a late copy's take began: the copy the daemon takes at each push of B it records
// (internal/daemon's copyWorkspaceAtPush), which replaces the late copy — the test records the
// push alone, so the late copy stays and the push refuses it instead, and its work is lost either
// way — and the copy B's confirmation keeps as B's own, which the store keeps over a late copy
// written after it (runCopyOrder names those orders).
//
// The orders are the store's. The daemon's own guards keep it from making some of them — one copy
// of an issue in flight at a time, which a release's save waits out, and a copy that a push
// overtook while it was built left unstored (internal/daemon's runningCopies) — and the store is
// held to both invariants in every order all the same, so that a loss the store's rules allow is
// seen here and not only once a guard of the daemon's misses.
//
// docs/kubernetes.md's table under "The times the restore compares" names each time these acts
// set, the clock and the statement that set it, and the rule that reads it.
func TestEveryInterleavingOfTheCopysTimesKeepsBothInvariants(t *testing.T) {
	s := migratedStore(t)
	var orders, sends, excepted int
	for i, order := range copyActOrders() {
		sent, lost := runCopyOrder(t, s, fmt.Sprintf("INFRA-1117-%03d", i), order)
		orders++
		sends += sent
		if lost {
			excepted++
		}
	}
	t.Logf("ran %d orders of the seven acts; checked %d sends against the first invariant; %d orders lost work to the working pod's own next copy alone",
		orders, sends, excepted)
	if orders == 0 {
		t.Fatal("no order ran, so neither invariant was checked")
	}
}
