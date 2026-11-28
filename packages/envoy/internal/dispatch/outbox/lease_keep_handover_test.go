package outbox

import (
	"errors"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The role the keep-handover rows route an event to, and the session that claims it while the
// keep is in flight.
const keepHandoverRole, keepHandoverHolder = "keep-handover-held", "ses_keep_handover_held"

// keepHandoverRow is two processes on one database, each with a NATS client of its own: held, whose
// client reaches the server through proxy and whose scan batch's handover behind a keep is held
// at gate, and working, which reaches the server directly. recorder is the default logger both
// write to, and keeps the publisher held publishes through, which counts its kept publishes.
type keepHandoverRow struct {
	proxy    *testnats.Proxy
	recorder *logtest.Recorder
	gate     *passGate
	keeps    *takenKeeps
	held     *process
	client   *bus.Client
	working  *process
	probes   *probeClock
}

// startKeepHandoverRow installs the row's recorder as the default logger, starts the row's two
// processes, held, named name, first, so it takes the publisher hold, writes kept events routed to
// keepHandoverRole on T-2, and brings the batch of held's that reads them to the first keep's
// handover's read of the role's kept lane, where the gate holds it.
//
// The events are committed in one transaction (routedEvents), so the batch that reads one reads
// them all: one owner, one destination, so one chain, whose keeps go one after another, each once
// the one before it answered (sendChains). Nobody holds the role when that batch resolves it, so
// it keeps each copy; keepHandoverHolder claims the role while the first keep is in flight
// (claimedUnderTheKeep, wrapping held's own publisher, which counts its sends), so once the stream
// acknowledged that keep the handover behind it (handOverKept) resolves a live holder, reads the
// role's drain cursor and then the lane (keptCopyLeadsTheLane). held reads the role and session
// buckets and the kept lanes through its own client, as cmd/dispatch wires them, so what the proxy
// does reaches each of those reads, and New puts its bound (boundedKeptLanes) in front of the gate.
//
// The gate holds the first read of the role's kept subject after it is armed, which is armed before
// the events are written, and that read is the first keep's handover's: the handover runs only
// once the keep's stored publish was acknowledged (roleForward.keep), and the expiry pass reads a
// kept subject only for an expiry notice, of which none is stored. The row shows it when the gate
// is reached: the claim was made, the lane holds the first keep's one copy, held has made that one
// kept publish and no other - a second keep behind it on the chain waits for the first's answer -
// and the stream holds no notice.
func startKeepHandoverRow(t *testing.T, name string, kept int) *keepHandoverRow {
	t.Helper()
	row := &keepHandoverRow{recorder: &logtest.Recorder{}, gate: newPassGate(t)}
	fake := clocktest.New(time.Time{})
	row.probes = &probeClock{Clock: clocktest.WithNow(fake, time.Now), fake: fake}
	logtest.SetDefault(t, row.recorder)
	_, url := testnats.Start(t)
	row.proxy = testnats.StartProxy(t, url)
	direct, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the row's own NATS client: %v", err)
	}
	t.Cleanup(direct.Close)
	rig := newRoleRigOn(t, url, direct, testnats.Connect(t, url))
	seedIssue(t, rig.database, "T-1", nil)
	counter := &sendCounter{}
	claimed := make(chan error, 1)
	row.held = startProcessWith(t, name, rig.database, row.proxy.URL(), counter, func(client *bus.Client, deps *Deps) {
		row.client = client
		// The rig dates its registrations on roleRigNow, so held judges them on that clock too.
		holders := roles.NewBuckets(client, roles.Windows{Now: roleRigNow})
		deps.RoleClaims, deps.Sessions = holders, holders
		deps.KeptLanes = gatedLanes{KeptLaneReader: client, subject: bus.KeptRoleSubject(keepHandoverRole), gate: row.gate}
		row.keeps = claimedUnderTheKeep(deps.Publisher, func() {
			claimed <- rig.claimUnder(keepHandoverRole, keepHandoverHolder, session.SessionEntry{SelfSubscribed: true})
		})
		deps.Publisher = row.keeps
	}, row.probes.setOutbox, bus.WithWindowClock(row.probes))
	waitFor(t, "the first process taking the publisher hold", row.held.outbox.Publishing)
	row.working = startProcess(t, "working", storetest.SecondProcess(t, rig.database), url, counter)

	row.gate.arm(0)
	routed := routedEvents(t, rig.database, row.held.broker, "T-2", keepHandoverRole, model.Actor{Kind: "user", ID: "alice"}, kept)
	for _, event := range routed {
		row.held.broker.Publish(event)
	}
	testwait.Receive(t, "the handover behind the first keep reaching its read of the role's kept lane", row.gate.reached)
	if err := testwait.Receive(t, "the claim made under the first keep", claimed); err != nil {
		t.Fatalf("the claim made under the first keep: %v", err)
	}
	if made := row.keeps.keeps.Load(); made != 1 {
		t.Fatalf("held made %d kept publishes when the first keep's handover was reached, want that keep alone", made)
	}
	keptMsgID := bus.KeptMsgID(fmt.Sprintf("dispatch-%d", routed[0].ID), keepHandoverRole)
	if lane := keptLane(t, direct, keepHandoverRole); len(lane) != 1 || lane[0].MsgID != keptMsgID {
		t.Fatalf("role %s's kept lane holds %d copies (first id %q) while the handover's read is held, want the first keep's one under %q",
			keepHandoverRole, len(lane), msgIDOf(lane), keptMsgID)
	}
	if _, err := direct.NextStoredMessage(testwait.Context(t), keptExpiryNotices, 1); !errors.Is(err, natsgo.ErrMsgNotFound) {
		t.Fatalf("read the stream's expiry notices: %v, want none stored", err)
	}
	t.Logf("the handover behind the first of %d keeps of role %s is held at its read of the kept lane", kept, keepHandoverRole)
	return row
}

// A holder whose NATS connection is cut, or goes silent - still connected, nothing answering -
// while a scan batch's handover behind a keep reads the role's kept lane gives the publisher hold
// up for NATS lost. The kept-lane read has probeTimeout to finish. A cut invalidates the pinned
// window immediately; for a silence, the failed read is followed by a flush probe with its own
// probeTimeout. Neither path can send a second keep while the first keep's handover is failing.
// The process whose NATS still works takes over and publishes what was written after the loss.
//
// Not parallel: it swaps the default logger (logtest.SetDefault).
func TestAnOutboxThatLosesNATSInAKeepsHandoverGivesThePublisherToTheOther(t *testing.T) {
	t.Run("cut", func(t *testing.T) {
		row := startKeepHandoverRow(t, "cut", 1)
		row.proxy.Cut()
		waitFor(t, "the cut process's client reporting itself disconnected", func() bool { return !row.client.Connected() })
		row.endKeptLaneRead(t)
		event := row.publishedAfter(t, "a cut")
		requireTakenOverAfterCut(t, row.recorder, row.held, row.working, event)
	})
	t.Run("silence", func(t *testing.T) {
		row := startKeepHandoverRow(t, "silent", 1)
		row.silenced(t)
	})
	t.Run("silence with a second keep behind", func(t *testing.T) {
		row := startKeepHandoverRow(t, "silent", 2)
		row.silenced(t)
	})
}

// silenced drives the kept-lane read and the follow-up probe, then checks that only the
// working process publishes and that the silent process does not take back the free hold.
func (r *keepHandoverRow) silenced(t *testing.T) {
	t.Helper()
	r.proxy.Freeze()
	t.Cleanup(r.proxy.Restore)
	probeEnd := r.endKeptLaneRead(t).Add(probeTimeout)
	r.probes.fake.AwaitArmedAt(t, "the keep's handover probe after the kept-lane read failed", probeEnd)
	crossDeadline(r.probes.fake, probeEnd, func() { r.requireStillHeld(t, "the handover probe") })
	r.publishedAfter(t, "a silence")
	requireNoTakeBackWhileSilent(t, r.recorder, r.held, r.working, r.probes)
}

// endKeptLaneRead shows the bound is armed while the gate still holds the read, then
// opens the gate and moves just far enough for the read's own deadline to end it.
func (r *keepHandoverRow) endKeptLaneRead(t *testing.T) time.Time {
	t.Helper()
	fake := r.probes.fake
	readEnd := fake.Now().Add(probeTimeout)
	fake.AwaitArmedAt(t, "the handover's kept-lane read bound", readEnd)
	crossDeadline(fake, readEnd, func() {
		r.requireStillHeld(t, "the kept-lane read")
		r.gate.release()
		if r.client.Connected() {
			r.probes.awaitConfirmedReadFlush(t)
		}
	})
	return readEnd
}

func (r *keepHandoverRow) requireStillHeld(t *testing.T, wait string) {
	t.Helper()
	if r.held.outbox.hold.Load() == nil {
		t.Fatalf("the holder gave the publisher hold up before the end of %s", wait)
	}
}

// Write the event once the held process lets the hold go, so its scan cannot try to send
// the event down the dead connection and make the test measure that batch instead.
func (r *keepHandoverRow) publishedAfter(t *testing.T, what string) model.Event {
	t.Helper()
	waitFor(t, "the held process giving the publisher hold up", func() bool {
		return r.held.outbox.hold.Load() == nil
	})
	event := writeMessage(t, r.working, "after "+what)
	waitFor(t, "the working process publishing the event written after "+what, func() bool {
		return r.working.outbox.Publishing() && publishedAt(t, r.working.store, event.ID) != nil
	})
	r.gate.requireHeld(t)
	if made := r.keeps.keeps.Load(); made != 1 {
		t.Fatalf("held made %d kept publishes before releasing the hold, want the first keep alone", made)
	}
	return event
}
