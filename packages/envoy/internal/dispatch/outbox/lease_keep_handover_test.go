package outbox

import (
	"errors"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
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

// keepHandoverTakeOver bounds the take-over from a holder cut from NATS, or whose connection went
// silent, at the handover behind a keep: from the cut or the silence to the other process
// publishing an event written after it. R3 of dispatch://INFRA-6066 holds it to the 8 s that R2
// holds a silence in the expiry pass to (silentTakeOver).
const keepHandoverTakeOver = silentTakeOver

// keepHandoverRow is two processes on one database, each with a NATS client of its own: held, whose
// client reaches the server through proxy and whose scan batch's handover behind a keep is held
// at gate, and working, which reaches the server directly. recorder is the default logger both
// write to.
type keepHandoverRow struct {
	proxy    *testnats.Proxy
	recorder *logtest.Recorder
	gate     *passGate
	held     *process
	client   *bus.Client
	working  *process
}

// startKeepHandoverRow installs the row's recorder as the default logger, starts the row's two
// processes, held, named name, first, so it takes the publisher hold, and brings a batch of held's
// to its handover's read of the role's kept lane, where the gate holds it.
//
// An event is routed to keepHandoverRole, which nobody holds when held's batch resolves it, so the
// batch keeps the copy; keepHandoverHolder claims the role while that keep is in flight
// (claimedUnderTheKeep, wrapping held's own publisher, which counts its sends), so once the stream
// acknowledged the keep the handover behind it (handOverKept) resolves a live holder, reads the
// role's drain cursor and then the lane (keptCopyLeadsTheLane). held reads the role and session
// buckets and the kept lanes through its own client, as cmd/dispatch wires them, so what the proxy
// does reaches each of those reads, and New puts its bound (boundedKeptLanes) in front of the gate.
//
// The gate holds the first read of the role's kept subject after it is armed, which is armed before
// the event is written, and that read is the handover's: the handover runs only once the keep's
// stored publish was acknowledged (roleForward.keep), and the expiry pass reads a kept subject only
// for an expiry notice, of which none is stored. The row shows both when the gate is reached: the
// claim was made, the lane holds the keep's one copy, and the stream holds no notice.
func startKeepHandoverRow(t *testing.T, name string) *keepHandoverRow {
	t.Helper()
	row := &keepHandoverRow{recorder: &logtest.Recorder{}, gate: newPassGate(t)}
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
		holders := roles.NewBuckets(client, roles.Windows{})
		deps.RoleClaims, deps.Sessions = holders, holders
		deps.KeptLanes = gatedLanes{KeptLaneReader: client, subject: bus.KeptRoleSubject(keepHandoverRole), gate: row.gate}
		deps.Publisher = claimedUnderTheKeep(deps.Publisher, func() {
			claimed <- rig.claimUnder(keepHandoverRole, keepHandoverHolder, session.SessionEntry{SelfSubscribed: true})
		})
	})
	waitFor(t, "the first process taking the publisher hold", row.held.outbox.Publishing)
	row.working = startProcess(t, "working", storetest.SecondProcess(t, rig.database), url, counter)

	row.gate.arm(0)
	kept := routedEvent(t, rig.database, row.held.broker, "T-2", keepHandoverRole, model.Actor{Kind: "user", ID: "alice"})
	row.held.broker.Publish(kept)
	testwait.Receive(t, "the handover behind the keep reaching its read of the role's kept lane", row.gate.reached)
	if err := testwait.Receive(t, "the claim made under the keep", claimed); err != nil {
		t.Fatalf("the claim made under the keep: %v", err)
	}
	keptMsgID := bus.KeptMsgID(fmt.Sprintf("dispatch-%d", kept.ID), keepHandoverRole)
	if lane := keptLane(t, direct, keepHandoverRole); len(lane) != 1 || lane[0].MsgID != keptMsgID {
		t.Fatalf("role %s's kept lane holds %d copies (first id %q) while the handover's read is held, want the keep's one under %q",
			keepHandoverRole, len(lane), msgIDOf(lane), keptMsgID)
	}
	if _, err := direct.NextStoredMessage(testwait.Context(t), keptExpiryNotices, 1); !errors.Is(err, natsgo.ErrMsgNotFound) {
		t.Fatalf("read the stream's expiry notices: %v, want none stored", err)
	}
	t.Logf("the handover behind the keep is held at its read of role %s's kept lane", keepHandoverRole)
	return row
}

// A holder whose NATS connection is cut, or goes silent - still connected, nothing answering -
// while a scan batch's handover behind a keep reads the role's kept lane gives the publisher hold
// up for NATS lost, and the other process, whose NATS works, takes the hold and publishes an event
// written after the cut or the silence within keepHandoverTakeOver, the 8 s of R3 of
// dispatch://INFRA-6066. The keep's answer waits for its handover (roleForward.keep, handOverKept)
// and the scan waits for its batches, so a read the cut or the silence holds holds the holder's
// scan, and the holder counts the loss, or finds the silence, only once that read gave up. Without
// the bound New puts on every read through Deps.KeptLanes (boundedKeptLanes, probeTimeout), the read
// waits up to storedMessageWait in internal/bus, 10 s, past the take-over. The subtest cut then
// checks what TestAnOutboxThatLosesNATSReleasesThePublisher checks after its cut
// (requireTakenOverAfterCut), and silence what TestAnOutboxWhoseNATSGoesSilentGivesThePublisherToTheOther
// checks after its silence (requireNoTakeBackWhileSilent).
//
// Not parallel: its subject is the take-over within keepHandoverTakeOver, and it swaps the default
// logger (logtest.SetDefault).
func TestAnOutboxThatLosesNATSInAKeepsHandoverGivesThePublisherToTheOther(t *testing.T) {
	t.Run("cut", func(t *testing.T) {
		row := startKeepHandoverRow(t, "cut")
		row.proxy.Cut()
		cutAt := time.Now()
		waitFor(t, "the cut process's client reporting itself disconnected", func() bool { return !row.client.Connected() })
		row.gate.release()
		event := row.publishedAfter(t, cutAt, "a cut")
		requireTakenOverAfterCut(t, row.recorder, row.held, row.working, event)
	})
	t.Run("silence", func(t *testing.T) {
		row := startKeepHandoverRow(t, "silent")
		row.proxy.Freeze()
		t.Cleanup(row.proxy.Restore)
		frozenAt := time.Now()
		row.gate.release()
		row.publishedAfter(t, frozenAt, "a silence")
		requireNoTakeBackWhileSilent(t, row.recorder, row.held, row.working)
	})
}

// publishedAfter is the event the working process publishes after what happened at lostAt - a
// cut or a silence of held's connection - within keepHandoverTakeOver of it: it waits for held to
// let the publisher hold go, writes the event through the working process on T-1, waits for that
// process to publish it, and logs how long after lostAt that was.
//
// The event is written once held let the hold go, not as the gate opens: held's scan is still
// running then, and a wake it takes while its batches run starts a batch of its own (Outbox.scan),
// which would read the event and send it into the cut or the silence, waiting up to
// publishAcknowledgementTimeout for the connection or the acknowledgement and then for a probe
// (publishBatch). The row would then time that batch rather than the handover, and held's send
// would be a second send of the event.
func (r *keepHandoverRow) publishedAfter(t *testing.T, lostAt time.Time, what string) model.Event {
	t.Helper()
	waitWithin(t, keepHandoverTakeOver-time.Since(lostAt), "the held process giving the publisher hold up", func() bool {
		return r.held.outbox.hold.Load() == nil
	})
	event := writeMessage(t, r.working, "after "+what)
	waitWithin(t, keepHandoverTakeOver-time.Since(lostAt), "the working process publishing the event written after "+what, func() bool {
		return r.working.outbox.Publishing() && publishedAt(t, r.working.store, event.ID) != nil
	})
	elapsed := time.Since(lostAt).Round(time.Millisecond)
	t.Logf("the working process published the event %s after %s inside the handover's read of the kept lane (%s under the %s bound)",
		elapsed, what, keepHandoverTakeOver-elapsed, keepHandoverTakeOver)
	return event
}
