package daemon

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// rowGate holds the first outbox row its armed predicate takes at a seam of the status row's run
// (overrides.statusRowChecked, overrides.statusRowRead) until the test opens it, and says when
// the row reached it and with what sequence (the read's; 0 at the check).
type rowGate struct {
	mu   sync.Mutex
	want func(record.OutboxRow) bool
	hit  chan int64
	open chan struct{}
}

// arm holds the next row want takes.
func (g *rowGate) arm(want func(record.OutboxRow) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.want, g.hit, g.open = want, make(chan int64, 1), make(chan struct{})
}

// at is the seam: a row the gate is armed for waits here until the gate opens.
func (g *rowGate) at(ctx context.Context, row record.OutboxRow, seq int64) {
	g.mu.Lock()
	want, hit, open := g.want, g.hit, g.open
	if want == nil || !want(row) {
		g.mu.Unlock()
		return
	}
	g.want = nil
	g.mu.Unlock()
	hit <- seq
	select {
	case <-open:
	case <-ctx.Done():
	}
}

// reached waits for the armed row to reach the gate and answers the sequence it brought.
func (g *rowGate) reached(t *testing.T, what string) int64 {
	t.Helper()
	g.mu.Lock()
	hit := g.hit
	g.mu.Unlock()
	select {
	case seq := <-hit:
		return seq
	case <-time.After(2 * time.Minute):
		t.Fatalf("%s never reached its seam", what)
		return 0
	}
}

// release lets the held row go on.
func (g *rowGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.open)
}

// factGate holds the Dispatch consumer at the first fact its armed predicate takes
// (overrides.dropConsumedEvent, which drops nothing here) until the test opens it: the daemon has
// received the event and not applied it.
type factGate struct {
	mu   sync.Mutex
	want func(intake.Fact) bool
	hit  chan struct{}
	open chan struct{}
}

func (g *factGate) arm(want func(intake.Fact) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.want, g.hit, g.open = want, make(chan struct{}, 1), make(chan struct{})
}

func (g *factGate) holds(fact intake.Fact) bool {
	g.mu.Lock()
	want, hit, open := g.want, g.hit, g.open
	if want == nil || !want(fact) {
		g.mu.Unlock()
		return false
	}
	g.want = nil
	g.mu.Unlock()
	hit <- struct{}{}
	<-open
	return false
}

func (g *factGate) reached(t *testing.T, what string) {
	t.Helper()
	g.mu.Lock()
	hit := g.hit
	g.mu.Unlock()
	select {
	case <-hit:
	case <-time.After(2 * time.Minute):
		t.Fatalf("the consumer never reached %s", what)
	}
}

func (g *factGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.open)
}

// idHold holds the outbox rows it was told of by id (overrides.holdOutbox) until the test
// releases them.
type idHold struct {
	mu   sync.Mutex
	held map[int64]bool
}

func (h *idHold) holdID(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held == nil {
		h.held = map[int64]bool{}
	}
	h.held[id] = true
}

func (h *idHold) release(ids ...int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		delete(h.held, id)
	}
}

func (h *idHold) holds(row record.OutboxRow) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held[row.ID]
}

// stopReturnRig is the person-handover rig with the two seams of the status row, a hold of the
// consumer armed on demand, and the daemon's own way of applying a fact (overrides.supervising).
type stopReturnRig struct {
	*personHandoverRig
	checked, read rowGate
	consumer      factGate
	held          idHold
	mu            sync.Mutex
	apply         func(ctx context.Context, source, eventID string, fact intake.Fact) (intake.Result, error)
}

// newStopReturnRig is the rig over a scratch Dispatch with its parent status rule on, three
// slots, booted.
func newStopReturnRig(t *testing.T) *stopReturnRig {
	t.Helper()
	return newStopReturnRigOn(t, scratchDispatchOptions{natsURL: testnats.URL(t)}, 3)
}

// newStopReturnRigOn is newStopReturnRig over the scratch Dispatch board describes, with
// admissionCap slots.
func newStopReturnRigOn(t *testing.T, board scratchDispatchOptions, admissionCap int) *stopReturnRig {
	t.Helper()
	r := &stopReturnRig{personHandoverRig: newPersonHandoverRigOn(t, board, admissionCap)}
	r.tune = func(o *overrides) {
		o.statusRowChecked = func(ctx context.Context, row record.OutboxRow) { r.checked.at(ctx, row, 0) }
		o.statusRowRead = func(ctx context.Context, row record.OutboxRow, seq int64) { r.read.at(ctx, row, seq) }
		o.dropConsumedEvent = r.consumer.holds
		o.holdOutbox = r.held.holds
		o.supervising = func(_ *supervisor, apply func(context.Context, string, string, intake.Fact) (intake.Result, error)) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.apply = apply
		}
	}
	r.boot()
	return r
}

// runningRoot is a root a session labelled for Legion and a person set todo: admitted into a
// slot, its architect launched and registered, and Dispatch showing in_progress. It answers the
// architect's grants.
func (r *stopReturnRig) runningRoot(title string) (scratchIssue, func() string) {
	r.t.Helper()
	root := r.create(asSession, title, map[string]any{"labels": []string{"legion"}})
	grant := r.admit(root.Key)
	r.quiet(root.Key)
	return root, grant
}

// implementing is an issue for each of titles that the daemon runs in implementing, each a root
// a person filed and moved to in_progress, recorded in implementing as an older daemon left it in
// the first run (r25's pattern: the daemon is stopped while the records are written and booted
// over them), its implementer then started by the operator's restart of the phase
// (intake.RestartPhase, as `legion claims retry` applies it), and its launch waited for. The rig
// must hold a slot for each.
func (r *stopReturnRig) implementing(titles ...string) []scratchIssue {
	r.t.Helper()
	r.d.stop()
	issues := make([]scratchIssue, len(titles))
	for i, title := range titles {
		issues[i] = r.create(asPerson, title, map[string]any{"labels": []string{"legion"}})
		r.setStatus(issues[i].Key, "in_progress")
	}
	if err := pgx.BeginFunc(r.t.Context(), r.pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		for i, issue := range issues {
			seen := r.issue(issue.Key)
			if err := records.PutIssue(r.t.Context(), tx, record.Issue{Key: issue.Key, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Project: r.cfg.Project, Tree: issue.Key, Title: issue.Key, Phase: phase.Implementing, Generation: 1,
				Status: "in_progress", DispatchStatus: seen.Status, Rank: "A", HandedOver: true, HoldsGate: true,
				LastDispatchSeq: seen.LastSeq, SeenSeq: seen.LastSeq}); err != nil {
				return err
			}
			if err := records.PutSlot(r.t.Context(), tx, record.Slot{Issue: issue.Key, Index: i, AdmittedAt: time.Now()}); err != nil {
				return err
			}
			// The issue's own lifecycle stands open under the workflow, as admission's promotion
			// leaves it, and the tree's beside it, which the release before reads: a claim binds
			// to the issue's (store's AdmitClaim).
			project, err := claim.ProjectToken(r.cfg.Project)
			if err != nil {
				return err
			}
			if _, err := records.OpenIssueLifecycle(r.t.Context(), tx, project, issue.Key, treelifecycle.AuthorityWorkflow); err != nil {
				return err
			}
			if _, err := records.OpenTreeLifecycle(r.t.Context(), tx, project, issue.Key, treelifecycle.AuthorityWorkflow); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		r.t.Fatalf("seed the issues in implementing: %v", err)
	}
	rebindHeldPorts(r.t, &r.cfg)
	r.boot()
	r.mu.Lock()
	apply := r.apply
	r.mu.Unlock()
	for _, issue := range issues {
		r.quiet(issue.Key)
		if result, err := apply(r.t.Context(), "operator", "restart-"+issue.Key, intake.RestartPhase{Issue: issue.Key, Role: claim.RoleImplementer}); err != nil || result.Refusal != nil {
			r.t.Fatalf("start the implementer of %s = %+v, %v", issue.Key, result.Refusal, err)
		}
		implementer := r.roleToken(issue.Key, claim.RoleImplementer)
		launched := func() bool {
			for _, call := range r.rt.Calls() {
				if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == implementer {
					return true
				}
			}
			return false
		}
		if !testwait.Holds(time.Minute, 50*time.Millisecond, launched) {
			var lines []string
			for _, line := range strings.Split(r.logs.String(), "\n") {
				if strings.Contains(line, issue.Key) {
					lines = append(lines, line)
				}
			}
			r.t.Fatalf("the daemon launched no implementer of %s; its lines about it:\n%s\noutbox: %v", issue.Key, strings.Join(lines, "\n"), r.outboxKinds(issue.Key))
		}
	}
	return issues
}

// outboxKinds is each row of key the outbox holds, as its kind, its payload and its last error.
func (r *stopReturnRig) outboxKinds(key string) []string {
	r.t.Helper()
	rows, err := r.pool.Query(r.t.Context(), "select kind, payload::text, coalesce(last_error, '') from outbox where issue = $1 order by id", key)
	if err != nil {
		r.t.Fatalf("read the outbox of %s: %v", key, err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind, payload, last string
		if err := rows.Scan(&kind, &payload, &last); err != nil {
			r.t.Fatalf("scan an outbox row of %s: %v", key, err)
		}
		kinds = append(kinds, kind+" "+payload+" "+last)
	}
	return kinds
}

// closeRoot is the root architect's close_root of key, with reason, which queues the close.
func (r *stopReturnRig) closeRoot(key string, grant func() string, reason string) {
	r.t.Helper()
	if status, body := r.d.request(http.MethodPost, "/legion/v1/roots/close", api.RootCloseRequest{GrantID: grant(), Issue: key, Reason: reason}, false); status/100 != 2 {
		r.t.Fatalf("close_root of %s = %d; body %s", key, status, body)
	}
}

// fence is the run and phase entry key stands at as the daemon records it.
func (r *stopReturnRig) fence(key string) record.Fence {
	r.t.Helper()
	var fence record.Fence
	if err := pgx.BeginFunc(r.t.Context(), r.pool, func(tx pgx.Tx) error {
		var err error
		fence, err = record.ReadFence(r.t.Context(), tx, record.NewStore(), key, false)
		return err
	}); err != nil {
		r.t.Fatalf("read the fence of %s: %v", key, err)
	}
	return fence
}

// moved sets key's status as the person, and waits until the daemon has applied that edit.
func (r *stopReturnRig) moved(key, status string) scratchIssue {
	r.t.Helper()
	edited := r.setStatus(key, status)
	r.applied(r.newestEvent(key, "issue.updated", func(e scratchEvent) bool { return e.Seq == edited.LastSeq }))
	return edited
}

// resumed waits until key stands at a phase entry past before, its run unchanged: a return to
// work raised the entry (R20 of dispatch://INFRA-3320).
func (r *stopReturnRig) resumed(key string, before record.Fence) record.Fence {
	r.t.Helper()
	var now record.Fence
	testwait.Eventually(r.t, fmt.Sprintf("%s to return to work past entry %d", key, before.Entry), func() bool {
		now = r.fence(key)
		return now.Generation == before.Generation && now.Entry > before.Entry && record.InLegionsWork(r.record(key))
	})
	return now
}

// closeRowOf takes the daemon's own close of key (record.CloseStatusRow).
func closeRowOf(key string) func(record.OutboxRow) bool {
	return func(row record.OutboxRow) bool {
		if row.Kind != record.OutboxKindDispatchStatus || row.Issue != key {
			return false
		}
		payload, err := record.DecodeOutboxPayload(row)
		write, ok := payload.(record.StatusWrite)
		return err == nil && ok && write.Kind == record.CloseStatusRow
	}
}

// statusRowGone waits until the outbox holds no status row of key: each has run to its end.
func (r *stopReturnRig) statusRowGone(key string) {
	r.t.Helper()
	testwait.Eventually(r.t, "the status rows of "+key+" to finish", func() bool { return statusRowsOf(r.t, r.pool, key) == 0 })
}

// A close status queued at a phase entry writes nothing once the issue has stopped and come back
// to work at a later entry, whatever Dispatch shows when the row reads it (R20 of
// dispatch://INFRA-3320; decision 11 of dispatch://INFRA-4730):
//
//   - held after its entry check and before its Dispatch read until the issue has stopped and
//     returned to the same phase and status, the close writes no done when it is released, and
//     the record acknowledges no close; a head that checked the entry before the Dispatch read
//     alone writes done here;
//   - read after Dispatch has recorded the stop and the return while the daemon has not applied
//     them, and held after that read until the daemon has resumed the issue, the close writes no
//     done either: it waits until the record has seen the read's sequence before it checks the
//     entry, and a head that checked the entry right after its read writes done here.
//
// The plain case is the close with nothing between: the row posts its reason before it writes,
// and that message's own event, which no handler takes, moves the issue's cursor
// (record.Issue.SeenSeq), so the wait before the write ends on it and the close lands.
func TestAQueuedClosePausedAcrossAStopWritesNothing(t *testing.T) {
	r := newStopReturnRig(t)
	// Two issues the daemon runs in implementing, whose return to work starts the implementer at a
	// new phase entry (workflow's ReturnToWork); a close of each is queued at its entry as the
	// daemon's own close is, which no writer of this release queues in implementing.
	running := r.implementing("Kestrel across return", "Merlin behind consumer")

	t.Run("a close with nothing between lands, its own reason passing the wait", func(t *testing.T) {
		root, grant := r.runningRoot("Harrier plain finish")
		r.read.arm(closeRowOf(root.Key))
		r.closeRoot(root.Key, grant, "the human decided no change is needed")
		seq := r.read.reached(t, "the close's read")
		r.read.release()
		awaitDispatchStatus(t, r.baseURL, root.Key, "done")
		r.statusRowGone(root.Key)
		// The read the write was fenced on is the one after the reason's post, whose
		// message.created event is the newest the read gave: the consumer dropped it, no handler
		// taking it, and moved the cursor to it.
		posted := r.newestEvent(root.Key, "message.created", nil)
		if posted.Seq != seq {
			t.Fatalf("the close wrote at sequence %d, want the sequence of its own reason's message, %d", seq, posted.Seq)
		}
		if r.processed("dispatch", posted.factID()) {
			t.Fatalf("the reason's message.created was applied as a fact, want it dropped by the consumer: the cursor is what it moves")
		}
		if got := r.record(root.Key).SeenSeq; got < posted.Seq {
			t.Fatalf("the record's cursor = %d, want it at or past the reason's message, %d", got, posted.Seq)
		}
		closed := r.newestEvent(root.Key, "issue.closed", nil)
		if closed.Actor != (scratchActor{Kind: "session", ID: dispatch.DaemonSession(root.Key)}) {
			t.Fatalf("the close was written by %+v, want the daemon", closed.Actor)
		}
	})

	t.Run("held before its read across a stop and a return", func(t *testing.T) {
		issue := running[0]
		before := r.fence(issue.Key)
		r.checked.arm(closeRowOf(issue.Key))
		r.queueClose(issue.Key)
		r.checked.reached(t, "the close's check")
		r.moved(issue.Key, "backlog")
		// The person's return to in_progress leaves Dispatch at the status the close observed, so
		// only its entry tells the close from one the record still owes.
		r.moved(issue.Key, "in_progress")
		r.resumed(issue.Key, before)
		r.checked.release()
		r.statusRowGone(issue.Key)
		if shown := r.issue(issue.Key).Status; shown != "in_progress" {
			t.Fatalf("Dispatch shows %s %s after the close of the entry before the stop ran, want the person's in_progress", issue.Key, shown)
		}
		if got := r.record(issue.Key); got.Phase != phase.Implementing || got.Status == "done" {
			t.Fatalf("the record of %s = %+v, want it in implementing with no close acknowledged", issue.Key, got)
		}
	})

	t.Run("read after Dispatch recorded the stop and the return, held until the daemon applied them", func(t *testing.T) {
		issue := running[1]
		before := r.fence(issue.Key)
		r.checked.arm(closeRowOf(issue.Key))
		r.queueClose(issue.Key)
		r.checked.reached(t, "the close's check")
		r.consumer.arm(func(fact intake.Fact) bool {
			read, ok := fact.(intake.DispatchIssue)
			return ok && read.Key == issue.Key && read.Status == "backlog"
		})
		stopped := r.setStatus(issue.Key, "backlog")
		r.consumer.reached(t, "the stop of "+issue.Key)
		returned := r.setStatus(issue.Key, "in_progress")
		r.read.arm(closeRowOf(issue.Key))
		r.checked.release()
		seq := r.read.reached(t, "the close's read")
		if seq < returned.LastSeq {
			t.Fatalf("the close read %s at sequence %d, want a read after the return at %d", issue.Key, seq, returned.LastSeq)
		}
		if now := r.fence(issue.Key); now != before {
			t.Fatalf("the record of %s stands at %+v while its stop is held, want %+v", issue.Key, now, before)
		}
		r.consumer.release()
		r.applied(r.newestEvent(issue.Key, "issue.updated", func(e scratchEvent) bool { return e.Seq == stopped.LastSeq }))
		r.applied(r.newestEvent(issue.Key, "issue.updated", func(e scratchEvent) bool { return e.Seq == returned.LastSeq }))
		r.resumed(issue.Key, before)
		r.read.release()
		r.statusRowGone(issue.Key)
		if shown := r.issue(issue.Key).Status; shown != "in_progress" {
			t.Fatalf("Dispatch shows %s %s after the close of the entry before the stop ran, want the person's in_progress", issue.Key, shown)
		}
		if got := r.record(issue.Key); got.Phase != phase.Implementing || got.Status == "done" {
			t.Fatalf("the record of %s = %+v, want it in implementing with no close acknowledged", issue.Key, got)
		}
	})
}

// queueClose queues the daemon's own close of key at the fence key stands at, its reason posted
// first, as the close paths queue it (record.CloseStatusWrite, record.NewFencedRow).
func (r *stopReturnRig) queueClose(key string) {
	r.t.Helper()
	recorded := r.record(key)
	if err := pgx.BeginFunc(r.t.Context(), r.pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		row, err := record.NewFencedRow(r.t.Context(), tx, records, key,
			record.CloseStatusWrite("done", "in_progress", "Closed by its architect: a close queued at this phase entry", recorded.Generation), time.Now())
		if err != nil {
			return err
		}
		return records.Enqueue(r.t.Context(), tx, row)
	}); err != nil {
		r.t.Fatalf("queue the close of %s: %v", key, err)
	}
}

// A stop, a close status and a cleanup queued at a phase entry of an issue, each delivered after
// the issue stopped and came back to work at a later entry, change nothing (R17 and R20 of
// dispatch://INFRA-3320): the stop stops nothing of the resumed claim, the close writes nothing,
// and the cleanup acts on no pod; and the same after a stop, a return and a second stop. Each is
// queued with the fence the issue stands at, as every writer queues it (record.NewFencedRow),
// and held unrun (overrides.holdOutbox) until the moves are made. A stop drops the status writes
// queued before it (workflow's DropStatusWrites), so the close, naming the entry before the stop,
// is queued once the moves have landed. The completion of that entry is
// TestACompletionPausedAcrossAStopIsRefusedAtItsCommit's, which holds it at the route's commit.
func TestTheEntryBeforeAStopHasNoLateEffect(t *testing.T) {
	r := newStopReturnRig(t)
	running := r.implementing("Shrike late effects", "Grebe twice halted")
	for i, secondStop := range []bool{false, true} {
		t.Run(fmt.Sprintf("second stop %t", secondStop), func(t *testing.T) {
			root := running[i]
			before := r.fence(root.Key)
			recorded := r.record(root.Key)
			late := r.queueHeld(root.Key,
				record.SuperviseRequest{Op: "suspend", Tree: recorded.Tree, Role: claim.RoleImplementer, Generation: recorded.Generation, Reason: "a late stop"},
				record.IssueSuspend{Tree: recorded.Tree, Generation: recorded.Generation, TreeGeneration: recorded.Generation})
			r.moved(root.Key, "backlog")
			if stopped := r.fence(root.Key); stopped != before {
				t.Fatalf("the stop moved %s from %+v to %+v, want its phase entry kept", root.Key, before, stopped)
			}
			r.moved(root.Key, "in_progress")
			r.resumed(root.Key, before)
			if secondStop {
				r.moved(root.Key, "backlog")
			}
			// The close names the run and the entry it was decided at, before the stop, as its
			// writer named them then; a stop drops the status writes queued before it
			// (workflow's DropStatusWrites), so it is queued once the moves have landed, as a row
			// still in flight at the stop would be delivered.
			closing := record.CloseStatusWrite("done", "in_progress", "Closed by its architect: a late close", recorded.Generation)
			closing.Generation, closing.Entry = before.Generation, before.Entry
			rows := append(r.queueHeld(root.Key, closing), late...)
			calls := len(r.rt.Calls())
			r.held.release(rows...)
			testwait.Eventually(t, "the late rows of "+root.Key+" to finish", func() bool {
				for _, id := range rows {
					var queued bool
					if err := r.pool.QueryRow(t.Context(), "select exists (select 1 from outbox where id = $1)", id).Scan(&queued); err != nil || queued {
						return false
					}
				}
				return true
			})
			want := "in_progress"
			if secondStop {
				want = "backlog"
			}
			if shown := r.issue(root.Key).Status; shown != want {
				t.Fatalf("Dispatch shows %s %s after the late close ran, want the person's %s", root.Key, shown, want)
			}
			for _, call := range r.rt.Calls()[calls:] {
				if call.Method == "Suspend" || call.Method == "Release" {
					t.Fatalf("the late rows made the runtime %s %+v, want nothing stopped", call.Method, call)
				}
			}
			logs := r.logs.String()
			for _, line := range []struct {
				id   int64
				text string
			}{
				{rows[0], "outbox row of an earlier run or phase entry of its issue; finished without acting"},
				{rows[1], "outbox stop superseded by a newer start, or of an earlier run or phase entry; finished without acting"},
				{rows[2], "outbox pod effect of an earlier run or phase entry of its issue; acts on no pod"},
			} {
				if !slices.ContainsFunc(loggedLines(t, logs, line.text), func(logged map[string]any) bool { return logged["row"] == float64(line.id) }) {
					var about []string
					for _, logged := range strings.Split(logs, "\n") {
						if strings.Contains(logged, fmt.Sprintf(`"row":%d,`, line.id)) {
							about = append(about, logged)
						}
					}
					t.Fatalf("no line %q names row %d: the late row acted; its lines:\n%s", line.text, line.id, strings.Join(about, "\n"))
				}
			}
			if !secondStop {
				if got := r.record(root.Key); got.Phase == phase.Done || !record.InLegionsWork(got) {
					t.Fatalf("the record of %s = %+v, want it at work at its new entry", root.Key, got)
				}
			}
		})
	}
}

// queueHeld queues each of payloads on key at the fence key stands at, as every writer queues
// a row (record.NewFencedRow), held unrun until release names it, and answers their ids.
func (r *stopReturnRig) queueHeld(key string, payloads ...record.OutboxPayload) []int64 {
	r.t.Helper()
	var ids []int64
	if err := pgx.BeginFunc(r.t.Context(), r.pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		for _, payload := range payloads {
			row, err := record.NewFencedRow(r.t.Context(), tx, records, key, payload, time.Now())
			if err != nil {
				return err
			}
			if err := records.Enqueue(r.t.Context(), tx, row); err != nil {
				return err
			}
			var id int64
			if err := tx.QueryRow(r.t.Context(), "select max(id) from outbox where issue = $1 and kind = $2", key, string(row.Kind)).Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
			r.held.holdID(id)
		}
		return nil
	}); err != nil {
		r.t.Fatalf("queue the late rows of %s: %v", key, err)
	}
	return ids
}

// The daemon writes no status of any issue but its own: a person's stop of a root, its return and
// a second stop write nothing of its Dispatch parent, at work and not Legion's, nor of the child a
// person filed under it (R20 of dispatch://INFRA-3320). Each of the two keeps every status a person
// gave it, and no status row of either is ever queued.
func TestTheDaemonWritesNoStatusForAnotherIssue(t *testing.T) {
	r := newStopReturnRig(t)
	parent := r.create(asPerson, "Cormorant umbrella", nil)
	r.setStatus(parent.Key, "in_progress")
	root := r.create(asSession, "Shearwater nested root", map[string]any{"labels": []string{"legion"}, "parent": parent.Key})
	r.admit(root.Key)
	child := r.create(asPerson, "Gull leaf", map[string]any{"parent": root.Key})
	before := r.fence(root.Key)

	r.moved(root.Key, "backlog")
	r.moved(root.Key, "todo")
	r.resumed(root.Key, before)
	awaitDispatchStatus(t, r.baseURL, root.Key, "in_progress")
	r.moved(root.Key, "backlog")
	r.statusRowGone(root.Key)
	r.quiet(parent.Key, child.Key)

	daemon := scratchActor{Kind: "session", ID: dispatch.DaemonSession(root.Key)}
	for _, issue := range []scratchIssue{parent, child} {
		for _, event := range r.issueEvents(issue.Key) {
			// A child's move adds a child.status event to its parent under the mover's actor, which
			// is no write of the parent: what is held here is that the daemon moved neither.
			if event.Actor == daemon && (event.Type == "issue.updated" || event.Type == "issue.closed") {
				t.Fatalf("the daemon wrote %s on %s (seq %d), an issue not its own to write", event.Type, issue.Key, event.Seq)
			}
		}
		if rows := statusRowsOf(t, r.pool, issue.Key); rows != 0 {
			t.Fatalf("status rows of %s = %d, want none ever queued", issue.Key, rows)
		}
	}
	if shown := r.issue(parent.Key).Status; shown != "in_progress" {
		t.Fatalf("Dispatch shows the parent %s %s, want the person's in_progress", parent.Key, shown)
	}
	if shown := r.issue(child.Key).Status; shown != "triage" {
		t.Fatalf("Dispatch shows the child %s %s, want triage, where it was filed", child.Key, shown)
	}
	if shown := r.issue(root.Key).Status; shown != "backlog" {
		t.Fatalf("Dispatch shows %s %s after the second stop, want the person's backlog", root.Key, shown)
	}
}
