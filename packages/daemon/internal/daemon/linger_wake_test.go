package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// heldReasonIn is the word a decoded admission block's held list gives issue, "" where it names
// none.
func heldReasonIn(t *testing.T, block any, issue string) string {
	t.Helper()
	encoded, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Held []record.HeldIssue `json:"held"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode the block %s: %v", encoded, err)
	}
	for _, held := range decoded.Held {
		if held.Issue == issue {
			return string(held.Reason)
		}
	}
	return ""
}

// lastAdmissionMessage is the newest published state message that carries an admission event, and
// that event's block, decoded.
func lastAdmissionMessage(t *testing.T, r *admissionStateRig) (stateMessage, any) {
	t.Helper()
	var message stateMessage
	var block any
	for _, state := range statesPublished(t, r.publisher, 0) {
		for _, event := range state.events {
			if event.Kind == "admission" {
				message, block = state.message, decodedJSON(t, event.Admission)
			}
		}
	}
	return message, block
}

// The admission block's held list reads a root's linger against the pass's clock (record.Lingering
// in HeldReasonOf's tree_closed word), so a member of a parked tree whose root still lingers reads
// tree_closed, and the same member reads tree_parked once the linger runs out, with no write at all.
// The pass that wrote the block reported that deadline to the daemon's one timer for it
// (admit.Admission.UseLingerWake, lingerWake), and at the deadline the timer applies an admission
// pass (wakeAtLingerDeadlines, intake.LingerDeadline): the changed block's admission event is
// published then, with no fact the test applies and no tick, carrying the block GET
// /legion/v1/state serves. That pass reports no deadline ahead, so the timer is armed for none, and
// the loop ends with the daemon's stop.
//
// The deadline is real time a few seconds out, which the test waits for: the pass reads the
// admission's own clock, and the deadline is what that clock must pass.
func TestTheAdmissionBlockALingerDeadlineChangesIsPublishedAtTheDeadline(t *testing.T) {
	const root, child = "LEGION-100", "LEGION-101"
	r := newAdmissionStateRig(t)
	w := &workflowRuntime{pool: r.pool, handlers: r.handlers, dispatchProject: "LEGION", bootID: "linger-wake-test",
		log: quietLogger(), lingerWake: newLingerWake()}
	r.admission.UseLingerWake(w.lingerWake.arm)
	running, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { w.wakeAtLingerDeadlines(store.ForConcurrentWorker(running)); close(stopped) }()
	t.Cleanup(func() { stop(); <-stopped })

	// A tree a person parked while it ran: its root left the workflow at backlog, its linger armed
	// (the workflow's beginLinger puts the root at done), and its member waits in it.
	until := time.Now().Add(4 * time.Second).UTC().Truncate(time.Microsecond)
	r.seed(t, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		parked := r.root(root, "A", "backlog")
		parked.Phase, parked.LingerUntil = phase.Done, &until
		if err := records.PutIssue(ctx, tx, parked); err != nil {
			return err
		}
		if _, err := records.OpenTreeLifecycle(ctx, tx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
			return err
		}
		member := r.root(child, "B", "todo")
		parent := root
		member.Tree, member.Parent = root, &parent
		return records.PutIssue(ctx, tx, member)
	})
	r.apply(t, "entry-sweep", "sweep", intake.EntrySweep{})
	r.drain(t)
	if !time.Now().Before(until) {
		t.Fatalf("the first pass ended after the linger's deadline %s; the case needs it to end before", until)
	}
	first, block := lastAdmissionMessage(t, r)
	if first.Seq == 0 {
		t.Fatal("the first pass published no admission event")
	}
	if got := heldReasonIn(t, block, child); got != string(record.HeldTreeClosed) {
		t.Fatalf("before the deadline the block holds %s as %q, want %q: its root's linger still runs; block %v", child, got, record.HeldTreeClosed, block)
	}
	if armed := w.lingerWake.next(); !armed.Equal(until) {
		t.Fatalf("the timer's deadline is %s, want the root's linger deadline %s", armed, until)
	}
	published := len(r.publisher.publishes())

	// No fact from here on: the timer alone runs the pass that publishes the change.
	testwait.Eventually(t, "the admission event of the block the linger's deadline changed", func() bool {
		r.drain(t)
		return len(r.publisher.publishes()) > published
	})
	message, changed := lastAdmissionMessage(t, r)
	if message.Seq <= first.Seq {
		t.Fatalf("the publishes after the deadline carry no admission event after sequence %d", first.Seq)
	}
	if got := heldReasonIn(t, changed, child); got != string(record.HeldTreeParked) {
		t.Fatalf("after the deadline the block holds %s as %q, want %q; block %v", child, got, record.HeldTreeParked, changed)
	}
	if want := r.servedBlock(t); !reflect.DeepEqual(changed, want) {
		t.Fatalf("the event carries the block %v, want the block the state serves, %v", changed, want)
	}
	at, err := time.Parse(time.RFC3339Nano, message.At)
	if err != nil {
		t.Fatal(err)
	}
	if lag := at.Sub(until); lag < 0 || lag > 5*time.Second {
		t.Fatalf("the change committed at %s, %s from the deadline %s, want at the deadline", at, lag, until)
	}
	t.Logf("the change committed %s after the linger's deadline", at.Sub(until))
	var wakes int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from processed_events where source = 'linger-deadline'`).Scan(&wakes); err != nil {
		t.Fatal(err)
	}
	if wakes != 1 {
		t.Fatalf("the timer applied %d passes, want the one at the deadline", wakes)
	}
	testwait.Eventually(t, "the timer armed for no deadline once none lies ahead", func() bool { return w.lingerWake.next().IsZero() })

	stop()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the timer's loop did not end with the daemon's stop")
	}
}
