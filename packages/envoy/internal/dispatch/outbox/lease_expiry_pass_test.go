package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/roles"
	envoystore "github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// expiryPassRole is the role the rows of this file route an event to. No session claims it, so a
// scan keeps the event's copy on the role's kept lane, and the expiry pass keeps that copy again.
const expiryPassRole = "expiry-pass-held"

// passGate holds one call of the expiry pass. Once armed, it lets a given number of calls of its
// kind through and holds the next: that call reports that it has begun (reached) and goes on only
// once the gate opens, or ends with its context's error when its context ends first. A gate that
// waited past the call's context would measure the gate rather than the holder, whose own end of
// that context is what the rows test. Every call after the held one goes through.
//
// The call's context carries a deadline when the gate sits inside a bound - New puts
// boundedKeptLanes, probeTimeout, in front of a gate on Deps.KeptLanes - and that deadline counts
// from the moment the gate is reached, not from the moment the row cuts or freezes the proxy. A
// held call that deadline ends before the gate opens ended on a healthy connection, before the row
// acted, and the row would then pass by another path than the call it names. So the gate records
// such an end (outlived), and the row fails on it: once its own bound was met (requireHeld) and,
// whatever failed first, as the test ends. An end by cancellation is the product's own - the pass's
// watcher ending the pass at a connection event - and is not recorded.
type passGate struct {
	// due counts down the calls the gate lets through; the call that brings it to zero is the one
	// held. It stays below zero before the gate is armed and after the held call.
	due      atomic.Int64
	reached  chan struct{}
	ended    chan error // the held read's answer, including an end at the gate
	open     chan struct{}
	opened   sync.Once
	outlived atomic.Bool
}

// newPassGate is a gate not yet armed. It opens at the latest as the test ends, so a call it still
// holds then goes on, and the test fails then when its held call's deadline ended that call before
// the gate opened.
func newPassGate(t *testing.T) *passGate {
	gate := &passGate{reached: make(chan struct{}), ended: make(chan error, 1), open: make(chan struct{})}
	t.Cleanup(func() {
		if gate.outlived.Load() {
			t.Errorf("the held call's deadline ended it before the gate opened: the row did not hold the call it names")
		}
	})
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
// An end by ctx's deadline while the gate is still shut is recorded (outlived).
func (g *passGate) wait(ctx context.Context) error {
	select {
	case <-g.open:
		return nil
	case <-ctx.Done():
		select {
		case <-g.open:
		default:
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				g.outlived.Store(true)
			}
		}
		return ctx.Err()
	}
}

// release opens the gate.
func (g *passGate) release() { g.opened.Do(func() { close(g.open) }) }

// requireHeld fails the row when its held call's deadline ended that call before the gate opened
// (outlived): the row met its bound by another path than the call it names.
func (g *passGate) requireHeld(t *testing.T) {
	t.Helper()
	if g.outlived.Load() {
		t.Fatalf("the held call's deadline ended it before the gate opened: the row did not hold the call it names")
	}
}

// gatedLanes is the kept-lane reader a process reads its kept lanes and the expiry notices through
// (Deps.KeptLanes), with a gate in front of its stored-message reads of one subject.
type gatedLanes struct {
	KeptLaneReader
	subject string
	gate    *passGate
}

func (g gatedLanes) NextStoredMessage(ctx context.Context, subject string, seq uint64) (_ bus.StoredMessage, err error) {
	if subject == g.subject && g.gate.takes() {
		defer func() { g.gate.ended <- err }()
		if err := g.gate.wait(ctx); err != nil {
			return bus.StoredMessage{}, err
		}
	}
	return g.KeptLaneReader.NextStoredMessage(ctx, subject, seq)
}

// gatedClaims is the role bucket a process reads a role's claim from (Deps.RoleClaims), with a gate
// in front of its reads of one role's claim.
type gatedClaims struct {
	roles.ClaimStore
	role string
	gate *passGate
}

func (c gatedClaims) RoleClaim(ctx context.Context, role string) (_ envoystore.RoleClaim, err error) {
	if role == c.role && c.gate.takes() {
		defer func() { c.gate.ended <- err }()
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
// gateOn puts the gate in front of that call in the Deps of the process under test, whose readers
// and publisher the row has set to that process's own client; skip is how many calls of the same
// kind the gate lets through once armed, before the one it holds. recorded says the call is the
// keep's (keepExpiredCopiesAgain), which comes once the notice is recorded, so the row checks, when
// the call is held, that the notice's expiry is recorded and still owed its keep.
type expiryPassCall struct {
	name     string
	gateOn   func(deps *Deps, gate *passGate)
	skip     int64
	recorded bool
	keep     bool // the gate holds an asynchronous send, not a read with its own context
	stored   bool // wait for this read to reach the bus's confirmed flush before moving the clock
}

// expiryPassCalls is each NATS call of the expiry pass, in the order the pass makes them. The gate
// is armed once the scan that kept the event's copy has published the event, so the reads and the
// keep of that scan are done by then, and the scan's keep, and its handover's read of the role's
// claim, are not the calls it holds.
var expiryPassCalls = []expiryPassCall{
	{
		name: "the read of the expiry notices",
		gateOn: func(deps *Deps, gate *passGate) {
			deps.KeptLanes = gatedLanes{KeptLaneReader: deps.KeptLanes, subject: keptExpiryNotices, gate: gate}
		},
		stored: true,
	},
	{
		// The notice's record reads the copy it names first (recordKeptExpiry, noticedCopy), and
		// the keep the notice owes reads it again (readExpiredCopy): the gate lets the first
		// through.
		name: "the read of an expired copy",
		gateOn: func(deps *Deps, gate *passGate) {
			deps.KeptLanes = gatedLanes{KeptLaneReader: deps.KeptLanes, subject: bus.KeptRoleSubject(expiryPassRole), gate: gate}
		},
		skip:     1,
		recorded: true,
		stored:   true,
	},
	{
		name: "the read of a role's holder",
		gateOn: func(deps *Deps, gate *passGate) {
			deps.RoleClaims = gatedClaims{ClaimStore: deps.RoleClaims, role: expiryPassRole, gate: gate}
		},
		recorded: true,
	},
	{
		name: "the send of a keep",
		gateOn: func(deps *Deps, gate *passGate) {
			deps.Publisher = gatedKeeps{Publisher: deps.Publisher, subject: bus.KeptRoleSubject(expiryPassRole), gate: gate}
		},
		recorded: true,
		keep:     true,
	},
}

// expiryPassRow is two processes on one database, each with a NATS client of its own: held, whose
// client reaches the server through proxy and whose expiry pass is held at gate, and working,
// which reaches the server directly. recorder is the default logger both write to.
type expiryPassRow struct {
	proxy    *testnats.Proxy
	database *store.Store
	counter  *sendCounter
	recorder *logtest.Recorder
	gate     *passGate
	held     *process
	client   *bus.Client
	working  *process
	probes   atomic.Int64
	window   *probeClock
}

// startExpiryPassRow installs the row's recorder as the default logger, starts the row's two
// processes, held, named name, first, so it takes the publisher hold, and brings held's expiry
// pass to call's gate. An event routed to a role nobody holds has held's scan keep its copy on the
// role's kept lane; the expiry notice of that copy is stored as a listener's walk stores it at the
// end of the copy's hour; and an event written through held wakes it to scan and then make its
// expiry pass, which reads the notice, records it and keeps the copy again. held reads the kept
// lanes, the notices and the role and session buckets through its own client, as cmd/dispatch
// wires them, so what the proxy does reaches every NATS call of the pass.
func startExpiryPassRow(t *testing.T, name string, call expiryPassCall, fake *clocktest.Fake, silent bool) *expiryPassRow {
	t.Helper()
	recorder := &logtest.Recorder{}
	logtest.SetDefault(t, recorder)
	_, url := testnats.Start(t)
	row := &expiryPassRow{proxy: testnats.StartProxy(t, url), database: storetest.Open(t), counter: &sendCounter{}, recorder: recorder, gate: newPassGate(t)}
	windows := fake
	if !silent {
		windows = clocktest.New(time.Time{})
	}
	row.window = &probeClock{Clock: windows, fake: windows}
	seedIssue(t, row.database, "T-1", nil)
	row.held = startProcessWith(t, name, row.database, row.proxy.URL(), row.counter, func(client *bus.Client, deps *Deps) {
		row.client = client
		holders := roles.NewBuckets(client, roles.Windows{})
		deps.RoleClaims, deps.Sessions, deps.KeptLanes = holders, holders, client
		call.gateOn(deps, row.gate)
		deps.Publisher = &probeHook{Publisher: deps.Publisher, probing: func() { row.probes.Add(1) }}
	}, func(o *Outbox) {
		if silent {
			row.window.setOutbox(o)
		} else {
			o.clock = fake
		}
	}, bus.WithWindowClock(row.window), bus.WithPublishAcknowledgementClock(clocktest.New(time.Time{})))
	waitFor(t, "the first process taking the publisher hold", row.held.outbox.Publishing)
	row.working = startProcess(t, "working", storetest.SecondProcess(t, row.database), url, row.counter)
	reader, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the row's own NATS client: %v", err)
	}
	t.Cleanup(reader.Close)

	routed := routedEvent(t, row.database, row.held.broker, "T-2", expiryPassRole, model.Actor{Kind: "user", ID: "alice"})
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

// After a cut inside any of the expiry pass's four NATS calls, the holder releases at
// natsLossGrace and the other process alone publishes what was written after the cut. The three
// rows that hold a read also check that it ends on cancellation or reconnect, not its deadline.
//
// Not parallel: it swaps the default logger (logtest.SetDefault).
func TestAnOutboxCutFromNATSInItsExpiryPassReleasesThePublisher(t *testing.T) {
	for _, call := range expiryPassCalls {
		t.Run(call.name, func(t *testing.T) {
			fake := clocktest.New(time.Time{})
			row := startExpiryPassRow(t, "cut", call, fake, false)
			row.proxy.Cut()
			graceEnd := fake.Now().Add(natsLossGrace)
			waitFor(t, "the cut process's client reporting itself disconnected", func() bool { return !row.client.Connected() })
			row.gate.release()
			if !call.keep {
				err := testwait.Receive(t, "the held read ending at the cut", row.gate.ended)
				if errors.Is(err, context.DeadlineExceeded) ||
					(!errors.Is(err, context.Canceled) && !errors.Is(err, bus.ErrReconnecting)) {
					t.Fatalf("held read ended with %v, want cancellation by the pass or reconnect refusal, not its deadline", err)
				}
			}
			fake.AwaitArmedAt(t, "the grace after the expiry pass ended at the cut", graceEnd)
			crossDeadline(fake, graceEnd, func() {
				if row.held.outbox.hold.Load() == nil {
					t.Fatal("the cut process released the hold before the grace ended")
				}
			})
			waitFor(t, "the cut process releasing the publisher hold at the grace deadline", func() bool {
				return row.held.outbox.hold.Load() == nil
			})
			row.gate.requireHeld(t)
			t.Logf("the cut process released the publisher hold at the grace deadline inside %s", call.name)
			event := writeMessage(t, row.working, "after the cut")
			requireTakenOverAfterCut(t, row.recorder, row.held, row.working, event)
		})
	}
}

// A silence inside any of the expiry pass's four NATS calls has no connection event. The watcher
// starts a probe at probeTimeout, and the failed flush ends the pass so the other holder can
// publish. The silent holder must probe again before it can take back the free hold.
//
// Not parallel: it swaps the default logger (logtest.SetDefault).
func TestAnOutboxWhoseNATSGoesSilentInItsExpiryPassGivesThePublisherToTheOther(t *testing.T) {
	for _, call := range expiryPassCalls {
		t.Run(call.name, func(t *testing.T) {
			fake := clocktest.New(time.Time{})
			row := startExpiryPassRow(t, "silent", call, fake, true)
			row.proxy.Freeze()
			t.Cleanup(row.proxy.Restore)
			due := fake.Now().Add(probeTimeout)
			event := writeMessage(t, row.working, "after the silence")
			fake.AwaitArmedAt(t, "the expiry watcher's first probe", due)
			crossDeadline(fake, due, func() {
				if probes := row.probes.Load(); probes != 0 {
					t.Fatalf("the expiry watcher made %d probes before its first probe was due", probes)
				}
				row.requireStillHeld(t, event)
				row.gate.release()
				if call.stored {
					row.window.awaitConfirmedReadFlush(t)
				}
			})
			probeEnd := due.Add(probeTimeout)
			waitFor(t, "the watcher's probe proceeding from the connection wait to the flush", func() bool {
				return row.window.flushingSince(due)
			})
			fake.AwaitArmedAt(t, "the expiry watcher's probe flush", probeEnd)
			crossDeadline(fake, probeEnd, func() { row.requireStillHeld(t, event) })
			waitFor(t, "the working process publishing the event at the failed probe's deadline", func() bool {
				return row.working.outbox.Publishing() && publishedAt(t, row.database, event.ID) != nil
			})
			row.gate.requireHeld(t)
			requireNoTakeBackWhileSilent(t, row.recorder, row.held, row.working, row.window)
		})
	}
}

func (r *expiryPassRow) requireStillHeld(t *testing.T, event model.Event) {
	t.Helper()
	if r.held.outbox.hold.Load() == nil || !r.client.Connected() || publishedAt(t, r.database, event.ID) != nil {
		t.Fatal("before the probe's deadline, want the silent client connected, its hold still held and the event unpublished")
	}
}

// answeredOnce is a test publisher with no NATS whose connection answers the first probe made of
// it and no probe after, as a connection that goes silent just after a probe answered: every probe
// after the first fails at once, wrapping bus.ErrUnavailable as a flush probe that ran out of time
// does. probes counts the probes made, and answered is closed as the first one answers.
type answeredOnce struct {
	publishOnly
	probes   atomic.Int32
	answered chan struct{}
}

func (p *answeredOnce) WindowUntil(func() time.Time) bus.Sender { return p }

func (p *answeredOnce) PublishAsync(item contracts.Envelope) <-chan error {
	return answered(fmt.Errorf("test publisher serves no publish (to %q)", item.Topic))
}

func (p *answeredOnce) Probe(time.Duration) error {
	if p.probes.Add(1) == 1 {
		close(p.answered)
		return nil
	}
	return fmt.Errorf("test connection: %w: the flush probe got no answer", bus.ErrUnavailable)
}

// An expiry pass that returns after a probe of its connection answered, and before the next probe
// is due, is probed again as it returns when it reports a failure or ran probeTimeout or longer
// since it began, and its watcher answers silent when that probe fails (watchExpiryPass). That is
// a silence that starts just after a probe answered: a call the pass makes then waits out its
// bound, at least probeTimeout, and fails, or the pass reports a failure of its own, and the pass
// returns before the next probe is due. The rule at 0abe7e89 answered such a return finished at
// once while the last probe that answered was sent less than probeTimeout before it
// (returned && time.Since(lastAnswer) < probeTimeout), so the watcher took the answer from before
// the silence for the connection's, the holder kept the hold, and the silence was found only by
// the next batch or pass, past the take-over; under that rule this test fails.
//
// The watcher runs alone, on an outbox whose publisher has no NATS (answeredOnce), with no
// connection-change channel: the first probe, due probeTimeout after began, answers, and every
// probe after it fails. Once the first answer is in and the watcher is blocked again, return the
// pass without moving the clock to the second probe's due time. Only the return can cause that
// probe. Each subtest is one value of the failed the pass reports.
func TestAnExpiryPassThatReturnsAfterAnAnsweredProbeIsProbedAgain(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		failed bool
	}{
		{name: "reporting no failure", failed: false},
		{name: "reporting a failure", failed: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			testwait.Bubble(t, func(t testing.TB) {
				publisher := &answeredOnce{answered: make(chan struct{})}
				outbox := New(Deps{Publisher: publisher})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				pass, endPass := context.WithCancel(ctx)
				defer endPass()
				returned := make(chan bool, 1)
				watched := make(chan expiryPassEnd, 1)
				began := time.Now()
				go func() { watched <- outbox.watchExpiryPass(ctx, nil, returned, endPass, began) }()

				testwait.Receive(t, "the first probe answering", publisher.answered)
				synctest.Wait()
				if probes := publisher.probes.Load(); probes != 1 {
					t.Fatalf("the watcher made %d probes before the pass returned, want only the first probe", probes)
				}
				returned <- row.failed
				end := testwait.Receive(t, "the watcher's answer once the pass returned", watched)
				if end != expiryPassSilent {
					t.Fatalf("the watcher answered %d, want expiryPassSilent (%d); it made %d probes, want 2",
						end, expiryPassSilent, publisher.probes.Load())
				}
				if probes := publisher.probes.Load(); probes != 2 {
					t.Fatalf("the watcher made %d probes, want 2: the scheduled probe and the one at the pass's return", probes)
				}
				if pass.Err() == nil {
					t.Fatal("the watcher answered silent and left the pass's context standing, want it ended")
				}
			})
		})
	}
}
