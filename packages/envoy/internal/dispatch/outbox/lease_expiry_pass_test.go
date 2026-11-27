package outbox

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/roles"
	envoystore "github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// expiryPassRole is the role the rows of this file route an event to. No session claims it, so a
// scan keeps the event's copy on the role's kept lane, and the expiry pass keeps that copy again.
const expiryPassRole = "expiry-pass-held"

// silentTakeOver bounds the take-over from a holder whose NATS connection went silent, from the
// silence to the other process publishing an event written after it: the in-process take-over of
// a silence between passes (7.06 s to 7.08 s,
// TestAnOutboxWhoseNATSGoesSilentGivesThePublisherToTheOther at cedc5234) with a margin, which
// leaves the rest of the 10 s of dispatch://INFRA-590 to the two-process row
// (TestAPublisherCutFromNATSHandsOverWithin10Seconds, cmd/dispatch/multiprocess_test.go).
const silentTakeOver = 8 * time.Second

// passGate holds one call of the expiry pass. Once armed, it lets a given number of calls of its
// kind through and holds the next: that call reports that it has begun (reached) and goes on only
// once the gate opens, or ends with its context's error when its context ends first. A gate that
// waited past the call's context would measure the gate rather than the holder, whose own end of
// that context is what the rows test. Every call after the held one goes through.
type passGate struct {
	// due counts down the calls the gate lets through; the call that brings it to zero is the one
	// held. It stays below zero before the gate is armed and after the held call.
	due     atomic.Int64
	reached chan struct{}
	open    chan struct{}
	opened  sync.Once
}

// newPassGate is a gate not yet armed. It opens at the latest as the test ends, so a call it still
// holds then goes on.
func newPassGate(t *testing.T) *passGate {
	gate := &passGate{reached: make(chan struct{}), open: make(chan struct{})}
	t.Cleanup(gate.release)
	return gate
}

// arm has the gate let skip calls of its kind through and hold the next one.
func (g *passGate) arm(skip int64) { g.due.Store(skip + 1) }

// takes reports whether this call is the one the gate holds, and marks the gate reached when it
// is.
func (g *passGate) takes() bool {
	if g.due.Add(-1) != 0 {
		return false
	}
	close(g.reached)
	return true
}

// wait is the held call's wait: until the gate opens, or until ctx ends, whose error it answers.
func (g *passGate) wait(ctx context.Context) error {
	select {
	case <-g.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release opens the gate.
func (g *passGate) release() { g.opened.Do(func() { close(g.open) }) }

// gatedLanes is the bus client a process reads its kept lanes and the expiry notices through
// (Deps.KeptLanes), with a gate in front of its stored-message reads of one subject.
type gatedLanes struct {
	*bus.Client
	subject string
	gate    *passGate
}

func (g gatedLanes) NextStoredMessage(ctx context.Context, subject string, seq uint64) (bus.StoredMessage, error) {
	if subject == g.subject && g.gate.takes() {
		if err := g.gate.wait(ctx); err != nil {
			return bus.StoredMessage{}, err
		}
	}
	return g.Client.NextStoredMessage(ctx, subject, seq)
}

// gatedClaims is the role bucket a process reads a role's claim from (Deps.RoleClaims), with a gate
// in front of its reads of one role's claim.
type gatedClaims struct {
	roles.ClaimStore
	role string
	gate *passGate
}

func (c gatedClaims) RoleClaim(ctx context.Context, role string) (envoystore.RoleClaim, error) {
	if role == c.role && c.gate.takes() {
		if err := c.gate.wait(ctx); err != nil {
			return envoystore.RoleClaim{}, err
		}
	}
	return c.ClaimStore.RoleClaim(ctx, role)
}

// gatedKeeps is the client a process publishes through (Deps.Publisher), whose windows have a gate
// in front of their keeps on one subject. A send takes no context, so the held keep hands its
// window an answer channel at once and is sent, and that channel answered, only once the gate
// opens: the pass waits for that answer within its own context (awaitAnswer), and that is what
// ends its wait.
type gatedKeeps struct {
	Publisher
	subject string
	gate    *passGate
}

func (p gatedKeeps) WindowUntil(deadline func() time.Time) bus.Sender {
	return gatedKeepWindow{Sender: p.Publisher.WindowUntil(deadline), keeps: p}
}

type gatedKeepWindow struct {
	bus.Sender
	keeps gatedKeeps
}

func (w gatedKeepWindow) PublishStoredAsync(subject string, item contracts.Envelope, msgID string) <-chan error {
	if subject != w.keeps.subject || !w.keeps.gate.takes() {
		return w.Sender.PublishStoredAsync(subject, item, msgID)
	}
	answer := make(chan error, 1)
	go func() {
		<-w.keeps.gate.open
		answer <- <-w.Sender.PublishStoredAsync(subject, item, msgID)
	}()
	return answer
}

// expiryPassCall is one NATS call of the expiry pass (passKeptExpiries) that a row holds at a gate.
// gateOn puts the gate in front of that call in the Deps of the process under test, built on that
// process's own client; skip is how many calls of the same kind the gate lets through once armed,
// before the one it holds. recorded says the call is the keep's (keepExpiredCopiesAgain), which
// comes once the notice is recorded, so the row checks, when the call is held, that the notice's
// expiry is recorded and still owed its keep.
type expiryPassCall struct {
	name     string
	gateOn   func(deps *Deps, client *bus.Client, gate *passGate)
	skip     int64
	recorded bool
}

// expiryPassCalls is each NATS call of the expiry pass, in the order the pass makes them. The gate
// is armed once the scan that kept the event's copy has published the event, so the reads and the
// keep of that scan are done by then, and the scan's keep, and its handover's read of the role's
// claim, are not the calls it holds.
var expiryPassCalls = []expiryPassCall{
	{
		name: "the read of the expiry notices",
		gateOn: func(deps *Deps, client *bus.Client, gate *passGate) {
			deps.KeptLanes = gatedLanes{Client: client, subject: keptExpiryNotices, gate: gate}
		},
	},
	{
		// The notice's record reads the copy it names first (recordKeptExpiry, noticedCopy), and
		// the keep the notice owes reads it again (readExpiredCopy): the gate lets the first
		// through.
		name: "the read of an expired copy",
		gateOn: func(deps *Deps, client *bus.Client, gate *passGate) {
			deps.KeptLanes = gatedLanes{Client: client, subject: bus.KeptRoleSubject(expiryPassRole), gate: gate}
		},
		skip:     1,
		recorded: true,
	},
	{
		name: "the read of a role's holder",
		gateOn: func(deps *Deps, _ *bus.Client, gate *passGate) {
			deps.RoleClaims = gatedClaims{ClaimStore: deps.RoleClaims, role: expiryPassRole, gate: gate}
		},
		recorded: true,
	},
	{
		name: "the send of a keep",
		gateOn: func(deps *Deps, _ *bus.Client, gate *passGate) {
			deps.Publisher = gatedKeeps{Publisher: deps.Publisher, subject: bus.KeptRoleSubject(expiryPassRole), gate: gate}
		},
		recorded: true,
	},
}

// expiryPassRow is two processes on one database, each with a NATS client of its own: held, whose
// client reaches the server through proxy and whose expiry pass is held at gate, and working,
// which reaches the server directly.
type expiryPassRow struct {
	proxy    *testnats.Proxy
	database *store.Store
	counter  *sendCounter
	gate     *passGate
	held     *process
	client   *bus.Client
	working  *process
}

// startExpiryPassRow starts the row's two processes, held, named name, first, so it takes the
// publisher hold, and brings held's expiry pass to call's gate. An event routed to a role nobody
// holds has held's scan keep its copy on the role's kept lane; the expiry notice of that copy is
// stored as a listener's walk stores it at the end of the copy's hour; and an event written
// through held wakes it to scan and then make its expiry pass, which reads the notice, records it
// and keeps the copy again. held reads the kept lanes, the notices and the role and session
// buckets through its own client, as cmd/dispatch wires them, so what the proxy does reaches every
// NATS call of the pass.
func startExpiryPassRow(t *testing.T, name string, call expiryPassCall) *expiryPassRow {
	t.Helper()
	_, url := testnats.Start(t)
	row := &expiryPassRow{proxy: testnats.StartProxy(t, url), database: storetest.Open(t), counter: &sendCounter{}, gate: newPassGate(t)}
	seedIssue(t, row.database, "T-1", nil)
	row.held = startProcessWith(t, name, row.database, row.proxy.URL(), row.counter, func(client *bus.Client, deps *Deps) {
		row.client = client
		holders := roles.NewBuckets(client, roles.Windows{})
		deps.RoleClaims, deps.Sessions, deps.KeptLanes = holders, holders, client
		call.gateOn(deps, client, row.gate)
	})
	waitFor(t, "the first process taking the publisher hold", row.held.outbox.Publishing)
	row.working = startProcess(t, "working", storetest.SecondProcess(t, row.database), url, row.counter)
	reader, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the row's own NATS client: %v", err)
	}
	t.Cleanup(reader.Close)

	routed := (&roleRig{database: row.database, broker: row.held.broker}).routedEvent(t, "T-2", expiryPassRole, model.Actor{Kind: "user", ID: "alice"})
	row.held.broker.Publish(routed)
	waitFor(t, "the holder publishing the event routed to the role", func() bool {
		return publishedAt(t, row.database, routed.ID) != nil
	})
	lane := keptLane(t, reader, expiryPassRole)
	if len(lane) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies, want the scan's one", expiryPassRole, len(lane))
	}
	row.gate.arm(call.skip)
	expire(t, reader, expiryPassRole, lane[0])
	writeMessage(t, row.held, "wakes the holder's expiry pass")
	testwait.Receive(t, "the holder's expiry pass reaching "+call.name, row.gate.reached)
	if call.recorded {
		if rows := expiryRows(t, row.database, routed.ID, expiryPassRole); len(rows) != 1 || rows[0].settled {
			t.Fatalf("event %d's expiries are %+v while %s is held, want the notice's one recorded and owed its keep", routed.ID, rows, call.name)
		}
	}
	t.Logf("the holder's expiry pass is held at %s", call.name)
	return row
}

// A holder whose NATS connection is cut while its expiry pass makes one of the pass's NATS calls
// releases the publisher hold within natsLossGrace of the cut, as a holder cut between its passes
// does (TestAnOutboxThatLosesNATSReleasesThePublisher). Each call is a subtest (expiryPassCalls):
// the read of the listeners' expiry notices, the read of an expired copy back from its role's kept
// lane, the read of the role's holder and the send of the keep. cmd/dispatch gives the outbox one
// bus client for all four (Deps.KeptLanes, Deps.Publisher, and the role and session buckets it
// reads Deps.RoleClaims and Deps.Sessions through), and each call made while that client
// reconnects waits for the reconnect up to a bound of its own: a stored-message read
// storedMessageWait in internal/bus, 10 s; a read of the buckets readTimeout in internal/roles,
// 2 s; a keep publishAcknowledgementTimeout in internal/bus, 5 s. A holder that waits such a call
// out before it counts the loss releases the hold only after that bound and natsLossGrace, so the
// other process takes the hold past the 10 s that TestAPublisherCutFromNATSHandsOverWithin10Seconds
// (cmd/dispatch/multiprocess_test.go) holds.
//
// Not parallel: its subject is that bound, the release within natsLossGrace and a half-second of it.
func TestAnOutboxCutFromNATSInItsExpiryPassReleasesThePublisher(t *testing.T) {
	for _, call := range expiryPassCalls {
		t.Run(call.name, func(t *testing.T) {
			row := startExpiryPassRow(t, "cut", call)
			row.proxy.Cut()
			downAt := time.Now()
			waitFor(t, "the cut process's client reporting itself disconnected", func() bool { return !row.client.Connected() })
			row.gate.release()
			waitWithin(t, natsLossGrace+500*time.Millisecond, "the cut process releasing the publisher hold", func() bool {
				return row.held.outbox.hold.Load() == nil
			})
			t.Logf("the cut process released the publisher hold %s after a cut inside %s",
				time.Since(downAt).Round(time.Millisecond), call.name)
			event := writeMessage(t, row.working, "after the cut")
			waitForPublished(t, row.database, row.counter, event)
			if !row.working.outbox.Publishing() || row.held.outbox.Publishing() {
				t.Fatalf("publishing %v after the cut, want only the process whose NATS works", publishing(row.held, row.working))
			}
		})
	}
}

// A holder whose NATS connection goes silent - still connected, nothing answering - while its
// expiry pass makes one of the pass's NATS calls gives the publisher hold up for NATS lost, and the
// other process, whose NATS works, takes the hold and publishes an event written after the silence
// within silentTakeOver of the silence, as it does after a silence between passes
// (TestAnOutboxWhoseNATSGoesSilentGivesThePublisherToTheOther). Each call is a subtest, as in
// TestAnOutboxCutFromNATSInItsExpiryPassReleasesThePublisher. No connection event comes, so the
// holder has to find the silence itself: a holder that waits a call out to its own bound - 10 s
// for a stored-message read (storedMessageWait in internal/bus) - before a batch's sends go
// unanswered and their probe fails hands the hold over that bound later. The silent process then
// does not take the hold back while its connection stays silent.
//
// Not parallel: its subject is the take-over within silentTakeOver of the silence.
func TestAnOutboxWhoseNATSGoesSilentInItsExpiryPassGivesThePublisherToTheOther(t *testing.T) {
	for _, call := range expiryPassCalls {
		t.Run(call.name, func(t *testing.T) {
			row := startExpiryPassRow(t, "silent", call)
			row.proxy.Freeze()
			t.Cleanup(row.proxy.Restore)
			frozenAt := time.Now()
			row.gate.release()
			event := writeMessage(t, row.working, "after the silence")
			waitWithin(t, silentTakeOver-time.Since(frozenAt), "the working process publishing the event written after the silence", func() bool {
				return row.working.outbox.Publishing() && publishedAt(t, row.database, event.ID) != nil
			})
			t.Logf("the working process published the event %s after a silence inside %s",
				time.Since(frozenAt).Round(time.Millisecond), call.name)
			for range 20 {
				if row.held.outbox.Publishing() {
					t.Fatalf("publishing %v while the silent process's connection stays silent, want only the process whose NATS works",
						publishing(row.held, row.working))
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}
}
