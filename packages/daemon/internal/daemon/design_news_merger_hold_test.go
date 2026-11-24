package daemon

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// A merger released before it reads what changed could merge it, so its start waits while its
// member is owed news whose change set is not recorded yet; once the pair is recorded the same row
// runs, and the merger's task carries the news.
func TestTheMergersStartWaitsWhileItsMembersChangeSetIsUnrecorded(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Merging)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleMerger, supervise.StateQueued, "", 0)
	const own = "Merge it."
	row := mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleMerger,
		Generation: 1, Phase: phase.Merging, Entry: 1, Task: own}, time.Now())

	if err := r.runner.execute(r.ctx, row); !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the merger's start with its news unrecorded = %v, want it held (wait.ErrWaiting)", err)
	}
	if spawns := len(r.rt.CallsOf("Spawn")); spawns != 0 {
		t.Fatalf("the runtime spawned %d sessions while the merger's news was unrecorded, want none", spawns)
	}

	r.record(reconcilePair, "")
	if err := r.runner.execute(r.ctx, row); err != nil {
		t.Fatalf("the merger's start once its news is recorded = %v, want it run", err)
	}
	machine, found := r.sup.Machine(mustClaimToken(t, reconcileRoot, claim.RoleMerger))
	if !found {
		t.Fatal("the merger's start created no claim")
	}
	task, news := pendingNews(t, machine)
	if !strings.HasPrefix(task, own+"\n\n") || !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 4)}) {
		t.Fatalf("the merger's task = %q carrying %+v, want its own text and the spec from 4 to 5", task, news)
	}
}

// The review history is read under the bound a change set is: a merger whose record knows no
// version it took waits while Dispatch keeps failing to say which version came before, each attempt
// of the design_changes row returning the failure for its retry. The row's third attempt records the
// history unread, naming the failed read, and writes no version: the told row still knows none, and
// no pair from version 0 is recorded, since a read that failed says nothing about the document.
// That releases the merger within the row's bounded attempts, and its task says the version its
// phase last took could not be read and why, never that the version is a first approval, carrying
// the item with no earlier version. Once Dispatch answers, the next reconcile reads the history
// again, fills the told row with the version it gives and clears what it recorded unread.
func TestThreeFailedHistoryReadsRecordTheHistoryUnreadWithNoVersionAndReleaseTheMerger(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Merging)
	r.told(reconcileRoot, record.AudienceHolder, -1)
	r.holder(reconcileRoot, claim.RoleMerger, supervise.StateQueued, "", 0)
	answers := false
	r.client.before = func(string, int) (int, error) {
		if !answers {
			return 0, errors.New("dispatch: 502 BAD_GATEWAY")
		}
		return 3, nil
	}
	const own = "Merge it."
	row := mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleMerger,
		Generation: 1, Phase: phase.Merging, Entry: 1, Task: own}, time.Now())
	question := record.DesignHistoryQuestion{ArtifactID: reconcileDoc, To: 5}

	for attempts := range designChangesAttempts - 1 {
		if err := r.reconcile(attempts); err == nil || !strings.Contains(err.Error(), "502 BAD_GATEWAY") {
			t.Fatalf("attempt %d under a failing review history = %v, want the failure returned for the outbox to retry", attempts+1, err)
		}
		if _, held := r.historyUnread(question); held {
			t.Fatalf("attempt %d recorded the history unread, want nothing recorded before the last attempt", attempts+1)
		}
		if err := r.runner.execute(r.ctx, row); !errors.Is(err, wait.ErrWaiting) {
			t.Fatalf("the merger's start after attempt %d = %v, want it held (wait.ErrWaiting)", attempts+1, err)
		}
	}
	if err := r.reconcile(designChangesAttempts - 1); err != nil {
		t.Fatalf("the last attempt under a failing review history = %v, want the history recorded unread and the row finished", err)
	}
	if took, known := r.toldOf(reconcileRoot, record.AudienceHolder).Took(record.DesignStepSpec); known {
		t.Fatalf("the merger's told version = %d after the last attempt, want none written: a read that failed is no version", took)
	}
	unread, held := r.historyUnread(question)
	if !held || !strings.Contains(unread.Reason, "review history") || !strings.Contains(unread.Reason, "502 BAD_GATEWAY") {
		t.Fatalf("the unread history of %+v = %+v (held %t), want it recorded, naming the failed read and its error", question, unread, held)
	}
	if changes, held := r.recorded(record.VersionPair{ArtifactID: reconcileDoc, From: 0, To: 5}); held {
		t.Fatalf("the change set from version 0 = %+v, want none recorded: nobody read that no version came before", changes)
	}
	if len(r.client.history) != designChangesAttempts || len(r.client.changes) != 0 {
		t.Fatalf("Dispatch was asked the history %d times and a change set %d times, want %d and none", len(r.client.history),
			len(r.client.changes), designChangesAttempts)
	}

	if err := r.runner.execute(r.ctx, row); err != nil {
		t.Fatalf("the merger's start once the history is recorded unread = %v, want it released", err)
	}
	machine, found := r.sup.Machine(mustClaimToken(t, reconcileRoot, claim.RoleMerger))
	if !found {
		t.Fatal("the merger's start created no claim")
	}
	task, news := pendingNews(t, machine)
	for _, want := range []string{"version 5, since a version the daemon could not read",
		`The version your phase last took could not be read: "Dispatch failed 3 reads of the document's review history, the last with: dispatch: 502 BAD_GATEWAY".`} {
		if !strings.Contains(task, want) {
			t.Fatalf("the merger's task = %q, want it to say %q", task, want)
		}
	}
	if strings.Contains(task, "first version a person approved") || len(news.Items) != 1 || news.Items[0].HistoryUnread == nil ||
		!news.Items[0].HistoryUnread.ReadAt.Equal(unread.ReadAt) {
		t.Fatalf("the merger's task = %q carrying %+v, want no first approval and the item with no earlier version, carrying the unread history", task, news)
	}
	// The moment read back through the delivery's JSON is the recorded one; compare the rest whole.
	news.Items[0].HistoryUnread.ReadAt = unread.ReadAt
	unknown := heldItem(reconcileRoot, 0)
	unknown.From = nil
	unknown.HistoryUnread = &record.DeliveryHistoryUnread{Reason: unread.Reason, ReadAt: unread.ReadAt}
	if !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{unknown}) {
		t.Fatalf("the merger's task carries %+v, want the item with no earlier version and the unread history it was attached with", news.Items)
	}

	answers = true
	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile once Dispatch answers: %v", err)
	}
	if took, known := r.toldOf(reconcileRoot, record.AudienceHolder).Took(record.DesignStepSpec); !known || took != 3 {
		t.Fatalf("the merger's told version once Dispatch answers = %d (known %t), want 3 written from the review history", took, known)
	}
	if unread, held := r.historyUnread(question); held {
		t.Fatalf("the unread history once Dispatch answers = %+v, want it cleared", unread)
	}
}

// queueReconcile queues the tree's design_changes row (record.QueueDesignNews), due now, as the
// attempt after attempts failed ones, for the outbox's own drain (outbox.RunOnce) to claim, lease,
// run and finish as a lane does.
func (r *newsReconcileRig) queueReconcile(attempts int) {
	r.t.Helper()
	r.inTx(func(tx pgx.Tx) error {
		if err := record.QueueDesignNews(r.ctx, r.records, tx, reconcileRoot, time.Now()); err != nil {
			return err
		}
		_, err := tx.Exec(r.ctx, `update outbox set attempts = $2 where kind = 'design_changes' and issue = $1`, reconcileRoot, attempts)
		return err
	})
}

// reconcilesQueued is how many design_changes rows of the tree the outbox holds.
func (r *newsReconcileRig) reconcilesQueued() int {
	r.t.Helper()
	var queued int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where kind = 'design_changes' and issue = $1`, reconcileRoot).Scan(&queued); err != nil {
		r.t.Fatalf("count the design_changes rows: %v", err)
	}
	return queued
}

// An unread review history stands only while no reconcile of its tree is pending: a pending one
// reads the history again and replaces the row. So a merger whose news is an unread history that a
// finished reconcile recorded is released on it, but once a transition queues the tree's reconcile
// beside the merger's start, as it does in production, the start waits and its news says nothing of
// the old failure; and when that reconcile has run, Dispatch answering now, the merger is released
// with the version the history gives, and never told the failure its confirmation would have kept.
func TestAPendingReconcileHoldsTheMergerOffAnUnreadHistoryUntilItReadsItAgain(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	root := r.member(reconcileRoot, "", phase.Merging)
	r.told(reconcileRoot, record.AudienceHolder, -1)
	r.holder(reconcileRoot, claim.RoleMerger, supervise.StateQueued, "", 0)
	answers := false
	r.client.before = func(string, int) (int, error) {
		if !answers {
			return 0, errors.New("dispatch: 502 BAD_GATEWAY")
		}
		return 3, nil
	}
	question := record.DesignHistoryQuestion{ArtifactID: reconcileDoc, To: 5}
	const own = "Merge it."
	start := record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging, Entry: 1, Task: own}
	row := mustOutboxRow(t, reconcileRoot, start, time.Now())

	// The reconcile's last attempt runs in the outbox and finishes, the history unread.
	r.queueReconcile(designChangesAttempts - 1)
	if err := r.runner.RunOnce(r.ctx); err != nil {
		t.Fatalf("run the tree's reconcile: %v", err)
	}
	if _, held := r.historyUnread(question); !held || r.reconcilesQueued() != 0 {
		t.Fatalf("after the reconcile's last attempt the history is recorded unread %t with %d reconciles queued, want it recorded and the row finished",
			held, r.reconcilesQueued())
	}
	// The control: with no reconcile pending, the unread history stands and the merger would be
	// released on it.
	if err := r.runner.mergerWaitsForTheDesignGate(r.ctx, root, claim.RoleMerger); err != nil {
		t.Fatalf("the merger's hold with the history unread and no reconcile pending = %v, want it released", err)
	}

	// Dispatch answers again, and a transition queues the tree's reconcile beside the merger's start.
	answers = true
	r.queueReconcile(0)
	if err := r.runner.mergerWaitsForTheDesignGate(r.ctx, root, claim.RoleMerger); !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the merger's hold with a reconcile pending = %v, want it held (wait.ErrWaiting): that reconcile reads the history again", err)
	}
	if err := r.runner.execute(r.ctx, row); !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the merger's start with a reconcile pending = %v, want it held (wait.ErrWaiting)", err)
	}
	if spawns := len(r.rt.CallsOf("Spawn")); spawns != 0 {
		t.Fatalf("the runtime spawned %d sessions while the reconcile was pending, want none", spawns)
	}
	held, err := r.runner.newsForStart(r.ctx, root, start, mustClaimToken(t, reconcileRoot, claim.RoleMerger))
	if err != nil {
		t.Fatalf("read the news the merger's start would carry: %v", err)
	}
	if held.task != own || held.news != nil || strings.Contains(held.task, "could not be read") || strings.Contains(held.task, "502 BAD_GATEWAY") {
		t.Fatalf("the news the merger's start would carry with a reconcile pending = %q carrying %s, want its own text alone and no word of the old failure",
			held.task, held.news)
	}

	// The pending reconcile runs, reads the history again, and replaces the unread row.
	if err := r.runner.RunOnce(r.ctx); err != nil {
		t.Fatalf("run the pending reconcile: %v", err)
	}
	if took, known := r.toldOf(reconcileRoot, record.AudienceHolder).Took(record.DesignStepSpec); !known || took != 3 {
		t.Fatalf("the merger's told version after the reconcile = %d (known %t), want 3 written from the review history", took, known)
	}
	if unread, held := r.historyUnread(question); held || r.reconcilesQueued() != 0 {
		t.Fatalf("after the reconcile the unread history = %+v (held %t) with %d reconciles queued, want it cleared and the row finished",
			unread, held, r.reconcilesQueued())
	}
	if err := r.runner.execute(r.ctx, row); err != nil {
		t.Fatalf("the merger's start once the reconcile read the history = %v, want it released", err)
	}
	machine, found := r.sup.Machine(mustClaimToken(t, reconcileRoot, claim.RoleMerger))
	if !found {
		t.Fatal("the merger's start created no claim")
	}
	task, news := pendingNews(t, machine)
	if !strings.Contains(task, "version 5, since version 3, which your phase last took") || strings.Contains(task, "could not be read") ||
		strings.Contains(task, "502 BAD_GATEWAY") || !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 3)}) {
		t.Fatalf("the merger's task = %q carrying %+v, want the spec from version 3, which the history gave, and no word of the old failure", task, news)
	}
}

// A merger released on an unread review history carries the item its start attached until its
// task is taken, with the history's reason (record.DeliveryNewsItem.HistoryUnread), and supervise
// renders that task again from the record at its send (workflowRuntime.designNews). The record
// moves before the send in one of two ways: Dispatch answers and a reconcile fills the told row and
// records the pair, or a reconcile is queued while Dispatch still fails. Neither loses what the
// start attached. Once the told row knows the version and the pair is recorded, the send tells
// that pair and the delivery goes on carrying the item from that version; while it does not, the
// send tells the unread history the start attached, with its reason, and the delivery keeps it.
// The merger's task never goes as its bare text, with none of the news it was held for.
func TestAMergerReleasedOnAnUnreadHistoryIsSentWhatItsStartAttached(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// answers says whether Dispatch answers the review history once the merger is released.
		answers bool
		says    []string
		never   []string
		// rekeyed says the send re-keys the item to version 3, the version the told row came to know.
		rekeyed bool
	}{
		{name: "Dispatch answered and the pair is recorded", answers: true, rekeyed: true,
			says:  []string{"version 5, since version 3, which your phase last took", `"Design" changed`},
			never: []string{"could not be read", "502 BAD_GATEWAY"}},
		{name: "a reconcile is pending while Dispatch still fails", answers: false,
			says: []string{"version 5, since a version the daemon could not read",
				`The version your phase last took could not be read: "Dispatch failed 3 reads of the document's review history, the last with: dispatch: 502 BAD_GATEWAY".`},
			never: []string{"first version a person approved"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			sends := &workflowRuntime{pool: r.pool, records: r.records, gatedSteps: r.runner.gatedSteps, dispatchProject: "LEGION", log: quietLogger()}
			r.sup.deps.DesignNews = sends.designNews
			r.member(reconcileRoot, "", phase.Merging)
			r.told(reconcileRoot, record.AudienceHolder, -1)
			r.holder(reconcileRoot, claim.RoleMerger, supervise.StateQueued, "", 0)
			answers := false
			r.client.before = func(string, int) (int, error) {
				if !answers {
					return 0, errors.New("dispatch: 502 BAD_GATEWAY")
				}
				return 3, nil
			}
			for attempts := range designChangesAttempts {
				_ = r.reconcile(attempts)
			}
			const own = "Merge it."
			if err := r.runner.execute(r.ctx, mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot,
				Role: claim.RoleMerger, Generation: 1, Phase: phase.Merging, Entry: 1, Task: own}, time.Now())); err != nil {
				t.Fatalf("the merger's start on the unread history = %v, want it released", err)
			}
			released, found := r.sup.Machine(mustClaimToken(t, reconcileRoot, claim.RoleMerger))
			if !found {
				t.Fatal("the merger's start created no claim")
			}
			if _, attached := pendingNews(t, released); len(attached.Items) != 1 || attached.Items[0].From != nil ||
				attached.Items[0].HistoryUnread == nil || !strings.Contains(attached.Items[0].HistoryUnread.Reason, "502 BAD_GATEWAY") {
				t.Fatalf("the merger's start attached %+v, want the item with no earlier version and the unread history's reason", attached.Items)
			}

			answers = tc.answers
			if tc.answers {
				if err := r.reconcile(0); err != nil {
					t.Fatalf("reconcile once Dispatch answers: %v", err)
				}
				if took, _ := r.toldOf(reconcileRoot, record.AudienceHolder).Took(record.DesignStepSpec); took != 3 {
					t.Fatalf("the merger's told version once Dispatch answered = %d, want 3: the case's own premise", took)
				}
			} else {
				r.queueReconcile(0)
			}

			machine, conn := connectWorker(t, r.ctx, r.sup, reconcileRoot, claim.RoleMerger)
			sent := deliveredTask(t, machine)
			prompts := conn.Prompts()
			if len(prompts) != 1 || prompts[0].DeliveryID != sent.ID {
				t.Fatalf("the merger's prompts = %+v, want its task, once", prompts)
			}
			message := prompts[0].Message
			if !strings.HasPrefix(message, own+"\n\n") {
				t.Fatalf("the merger's task sent = %q, want its own text and the news its start attached, never the bare text", message)
			}
			for _, want := range tc.says {
				if !strings.Contains(message, want) {
					t.Fatalf("the merger's task sent = %q, want it to say %q", message, want)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(message, never) {
					t.Fatalf("the merger's task sent = %q, want no %q", message, never)
				}
			}
			var carried record.DeliveryNews
			if err := json.Unmarshal(sent.News, &carried); err != nil {
				t.Fatalf("decode the news the sent delivery carries: %v", err)
			}
			switch item := carried.Items; {
			case len(item) != 1:
				t.Fatalf("the sent delivery carries %+v, want the one item", item)
			case tc.rekeyed && (item[0].From == nil || *item[0].From != 3 || item[0].HistoryUnread != nil):
				t.Fatalf("the sent delivery carries %+v, want the item re-keyed to version 3 with no unread history", item[0])
			case !tc.rekeyed && (item[0].From != nil || item[0].HistoryUnread == nil || !strings.Contains(item[0].HistoryUnread.Reason, "502 BAD_GATEWAY")):
				t.Fatalf("the sent delivery carries %+v, want the item as attached, with its unread history", item[0])
			}
		})
	}
}

// The reconcile that records an unread history settles on it in the same run: the outbox leases its
// own row while it runs, so the row is not a pending reconcile, and the news goes. A reconcile that
// records it while another of the tree is still to run settles nothing on it, since that one reads
// the history again.
func TestTheReconcileThatRecordsAnUnreadHistorySettlesOnItUnlessAnotherIsPending(t *testing.T) {
	t.Parallel()
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.told(reconcileRoot, record.AudienceHolder, -1)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
	r.client.before = func(string, int) (int, error) { return 0, errors.New("dispatch: 502 BAD_GATEWAY") }
	question := record.DesignHistoryQuestion{ArtifactID: reconcileDoc, To: 5}

	// Another reconcile of the tree is queued while this one gives up on the history.
	r.queueReconcile(designChangesAttempts - 1)
	if err := r.reconcile(designChangesAttempts - 1); err != nil {
		t.Fatalf("the last attempt beside a queued reconcile = %v, want the row finished", err)
	}
	if _, held := r.historyUnread(question); !held {
		t.Fatal("the last attempt recorded no unread history, want it recorded")
	}
	if starts := r.newsStarts(); len(starts) != 0 {
		t.Fatalf("the news-only starts beside a queued reconcile = %v, want none: that reconcile reads the history again", startsTo(starts))
	}

	// The queued reconcile runs in the outbox, under its lease, and its own last attempt settles.
	if err := r.runner.RunOnce(r.ctx); err != nil {
		t.Fatalf("run the queued reconcile: %v", err)
	}
	if r.reconcilesQueued() != 0 {
		t.Fatalf("design_changes rows after the queued reconcile ran = %d, want it finished", r.reconcilesQueued())
	}
	if got := startsTo(r.newsStarts()); !reflect.DeepEqual(got, []string{reconcileRoot + "/reviewer@reviewing"}) {
		t.Fatalf("the news-only starts after the reconcile that ran under its lease = %v, want the reviewer's: its own row is no pending reconcile", got)
	}
}
