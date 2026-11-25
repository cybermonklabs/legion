package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An expiry record that waits on the event owner's lock - here an open transaction that holds the
// issue's row, as any write on the issue does - for longer than the publisher hold survives with
// no renewal does not lose the hold: the record holds the hold's row only across its commit, so
// the hold's renewals go on while the record waits. The hold here is a real one, renewed as
// cmd/dispatch's is (publisherHoldOptions: a 4 s loss window, renewed every second). With the
// hold's row locked for the whole record, the renewals wait behind the record and the hold is
// lost.
func TestAnExpiryRecordWaitingOnTheOwnerLockKeepsThePublisherHold(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-record-waits"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := New(rig.deps())
	hold, err := duty.TryHold(ctx, rig.database.Pool, publisherHold, outbox.instance, publisherHoldOptions)
	if err != nil || hold == nil {
		t.Fatalf("take the publisher hold: %v", err)
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })
	outbox.scan(ctx, hold)
	for keep := 1; keep < maxRoleKeeps; keep++ {
		lane := keptLane(t, rig.client, role)
		expire(t, rig.client, role, lane[len(lane)-1])
		outbox.passKeptExpiries(ctx, hold)
	}
	lane := keptLane(t, rig.client, role)
	if len(lane) != maxRoleKeeps {
		t.Fatalf("role %s's kept lane holds %d copies, want %d before the last expiry", role, len(lane), maxRoleKeeps)
	}
	expire(t, rig.client, role, lane[maxRoleKeeps-1])

	// Another write holds the issue's row while the record that appends the event's
	// role.undelivered record waits on it, for longer than the hold's loss window.
	blocker, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `select 1 from issues where key = 'T-1' for update`); err != nil {
		t.Fatalf("lock T-1's row: %v", err)
	}
	passed := make(chan struct{})
	go func() {
		defer close(passed)
		outbox.passKeptExpiries(ctx, hold)
	}()
	select {
	case <-hold.Lost():
		_ = blocker.Rollback(ctx)
		<-passed
		t.Fatal("the publisher hold was lost while an expiry record waited on the issue's owner lock")
	case <-time.After(publisherHoldOptions.TTL):
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	<-passed
	select {
	case <-hold.Lost():
		t.Fatal("the publisher hold was lost once the issue's owner lock was released")
	default:
	}
	outbox.passKeptExpiries(ctx, hold)
	if records := undeliveredCount(t, rig.database, event.ID); records != 1 {
		t.Fatalf("event %d has %d role.undelivered records once the owner lock was free, want one", event.ID, records)
	}
}

// refusingKeeps is a publisher whose window refuses every keep on role's kept lane for good, as
// NATS refuses a send however often it is sent (bus.ErrRefused), and passes every other send on.
type refusingKeeps struct {
	*bus.Client
	role string
}

func (p refusingKeeps) WindowUntil(deadline func() time.Time) bus.Sender {
	return refusingKeepsWindow{Sender: p.Client.WindowUntil(deadline), role: p.role}
}

type refusingKeepsWindow struct {
	bus.Sender
	role string
}

func (w refusingKeepsWindow) PublishStoredAsync(subject string, item contracts.Envelope, msgID string) <-chan error {
	if subject == bus.KeptRoleSubject(w.role) {
		return answered(fmt.Errorf("test window: %w", bus.ErrRefused))
	}
	return w.Sender.PublishStoredAsync(subject, item, msgID)
}

// An expired copy whose keep NATS refuses for good cannot be kept again, so its event ends as one
// whose copy is gone does: its row settles undelivered and one role.undelivered record is appended,
// with the reason expired, after the one keep that ended. Before this rule a refused keep was a
// failed attempt, retried with backoff for good, and never reached its record.
func TestARefusedKeepEndsItsEventRecordedUndelivered(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-keep-refused"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	holding(t, rig.deps()).scan(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)
	if len(lane) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's one", role, len(lane))
	}
	expire(t, rig.client, role, lane[0])

	refusing := holding(t, rig.publisherOn(refusingKeeps{Client: rig.client, role: role}))
	refusing.passKeptExpiries(ctx, unfenced{})

	rows := expiryRows(t, rig.database, event.ID, role)
	if len(rows) != 1 || !rows[0].settled || rows[0].outcome == nil || *rows[0].outcome != expiryUndelivered {
		t.Fatalf("event %d's expiries are %+v, want its one settled undelivered", event.ID, rows)
	}
	records := undeliveredRecords(t, rig.database, "T-1")
	if len(records) != 1 {
		t.Fatalf("T-1 holds %d role.undelivered events, want one", len(records))
	}
	if record := records[0].Payload.(model.RoleUndeliveredEventPayload); record.Reason != model.RoleUndeliveredExpired || record.Keeps != 1 {
		t.Fatalf("the record is %+v, want reason expired after 1 keep", record)
	}
}

// A notice whose own attempt disagrees with the outbox's count - here it says 5 for the event's
// first expiry - is recorded at the outbox's next attempt, 1, and the event is kept again. Before
// this rule the notice's attempt decided the row, so this notice reached the bound at once.
func TestTheOutboxsCountDecidesAnExpirysAttempt(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "expiry-attempt-count"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)
	notice := expiryNoticeOf(t, role, lane[0])
	notice.Attempt = 5
	if err := rig.client.PublishKeptCopyExpiry(ctx, notice); err != nil {
		t.Fatalf("store the notice: %v", err)
	}

	outbox.passKeptExpiries(ctx, unfenced{})

	rows := expiryRows(t, rig.database, event.ID, role)
	if len(rows) != 1 || rows[0].attempt != 1 || rows[0].outcome == nil || *rows[0].outcome != expiryKept {
		t.Fatalf("event %d's expiries are %+v, want attempt 1 kept again", event.ID, rows)
	}
	if records := undeliveredRecords(t, rig.database, "T-1"); len(records) != 0 {
		t.Fatalf("T-1 holds %d role.undelivered events, want none after the first expiry", len(records))
	}
}

// A notice that names no copy this outbox kept for its role is passed over, and only the cursor
// moves past it: one for a role the event was never routed to, and one stored on another role's
// expiry subject than its own role's. Before this rule both were recorded and kept again.
func TestANoticeForACopyTheOutboxNeverKeptIsPassedOver(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role, other = "expiry-notice-routed", "expiry-notice-elsewhere"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.RoleTopicPrefix+other)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.deps())
	outbox.scan(ctx, unfenced{})
	lane := keptLane(t, rig.client, role)

	unrouted := expiryNoticeOf(t, role, lane[0])
	unrouted.Role = other
	if err := rig.client.PublishKeptCopyExpiry(ctx, unrouted); err != nil {
		t.Fatalf("store the notice for a role the event was never routed to: %v", err)
	}
	misfiled, err := json.Marshal(expiryNoticeOf(t, role, lane[0]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.client.JS().Publish(bus.KeptExpiredSubject(other), misfiled); err != nil {
		t.Fatalf("store the notice on another role's expiry subject: %v", err)
	}

	outbox.passKeptExpiries(ctx, unfenced{})

	for _, r := range []string{role, other} {
		if rows := expiryRows(t, rig.database, event.ID, r); len(rows) != 0 {
			t.Fatalf("the outbox recorded %+v for role %s, want nothing", rows, r)
		}
	}
	var cursor int64
	if err := rig.database.Pool.QueryRow(ctx, `select coalesce((select sequence from outbox_kept_expiry_cursor), 0)`).Scan(&cursor); err != nil || cursor == 0 {
		t.Fatalf("the expiry cursor is %d (%v), want it past both notices", cursor, err)
	}
	if lane := keptLane(t, rig.client, role); len(lane) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's one", role, len(lane))
	}
	if lane := keptLane(t, rig.client, other); len(lane) != 0 {
		t.Fatalf("role %s's kept lane holds %d copies, want none", other, len(lane))
	}
}
