package natscallout

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// writeGate holds a writer's writes, as a reader that stops reading holds the writes of whatever
// writes to it: the broker's stdout when the reader of the pod's log pipe falls behind (the
// deliver comment's "a log line is a write, which can block"), and a metrics server's write when a
// scrape client stops reading. A write that calls hold waits until release, and the first one
// marks the gate entered, which awaitHeld waits for.
type writeGate struct {
	entered  chan struct{}
	once     sync.Once
	released chan struct{}
}

func newWriteGate() *writeGate {
	return &writeGate{entered: make(chan struct{}), released: make(chan struct{})}
}

// hold marks the gate entered and waits until it is released.
func (g *writeGate) hold() {
	g.once.Do(func() { close(g.entered) })
	<-g.released
}

// release lets every held write through, and every later one.
func (g *writeGate) release() { close(g.released) }

// awaitHeld waits until a write is held at the gate, failing the test, naming what, when none is
// within five seconds.
func (g *writeGate) awaitHeld(t *testing.T, what string) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("no write reached %s", what)
	}
}

// stalledLog is a log sink whose writes the gate holds once stalled is set.
type stalledLog struct {
	*writeGate
	stalled atomic.Bool
}

func newStalledLog() *stalledLog {
	return &stalledLog{writeGate: newWriteGate()}
}

func (l *stalledLog) Write(p []byte) (int, error) {
	if l.stalled.Load() {
		l.hold()
	}
	return len(p), nil
}

// awaitStuck waits until a write has reached the stalled log and waits there: the reporter's,
// since nothing else of the service writes to its log.
func (l *stalledLog) awaitStuck(t *testing.T) {
	t.Helper()
	l.awaitHeld(t, "the stalled log: the reporter never wrote the line it owed")
}

// startWithStalledLog starts the service on url with a log that stalls once the test sets its
// stalled flag, and the reporter on a 10 ms tick, so a line it owes reaches the stalled log within
// the test rather than a second later. It releases the log before the service's own cleanup closes
// the service, which waits for every refusal.
func startWithStalledLog(t *testing.T, url string, keys testnats.CalloutKeys, deps Deps) (*stalledLog, *Service) {
	t.Helper()
	withLiveness(t, 10*time.Millisecond, 10*time.Second)
	log := newStalledLog()
	deps.Log = slog.New(slog.NewTextHandler(log, nil))
	service := startService(t, url, keys, deps, Decide)
	t.Cleanup(log.release)
	return log, service
}

// stallWithALine stalls log and puts the reporter's write on it: one refusal, which the reporter
// owes a line, and then a wait until that line's write is stuck. What the test does next it does
// with the reporter blocked in its write.
func stallWithALine(t *testing.T, url string, log *stalledLog) {
	t.Helper()
	log.stalled.Store(true)
	requireRefusedAtOnce(t, url, "first")
	log.awaitStuck(t)
}

// requireRefusedAtOnce connects once with no credential and a client that would wait five
// seconds, and requires the refusal inside half a second.
func requireRefusedAtOnce(t *testing.T, url, which string) {
	t.Helper()
	began := time.Now()
	nc, err := nats.Connect(url, nats.NoReconnect(), nats.Timeout(5*time.Second))
	took := time.Since(began)
	if err == nil {
		nc.Close()
	}
	require.ErrorIs(t, err, nats.ErrAuthorization, "the %s connection holding no credential was not refused (after %s)", which, took)
	require.Less(t, took, 500*time.Millisecond,
		"the %s refusal was heard after %s: nothing answered it, and the hub refused the client at its own timeout", which, took)
}

// requireHoldsNothing requires that, within half a second, the replica holds no request: none
// waiting for a decision slot, none counted against the refusing bound and no slot taken. The
// heartbeat takes a slot for an instant each tick, so the slots are read until they read empty. A
// refusal whose goroutine waits on the log holds one of these for as long as the log stalls.
func requireHoldsNothing(t *testing.T, service *Service) {
	t.Helper()
	testwait.WithinPolling(t, 500*time.Millisecond, 5*time.Millisecond, "the replica holding no request", func() bool {
		return service.waiting.Load() == 0 && service.refusing.Load() == 0 && len(service.slots) == 0
	})
}

// A refused client hears its refusal at once even while the broker's log cannot be written, and
// the refusal leaves the replica holding nothing. The waiting bound refuses rather than drops so
// that "its client hears at once, and why" (maxRefusing), and refusals left the delivery goroutine
// because a log line is a write that can block (deliver). At c24391bbc6 answer wrote the refusal's
// line before it sent the answer, so while the log was stalled a refusal waited on its line and
// its client heard nothing until the hub refused it at its own two-second timeout; and the refusing
// bound then counted refusals stuck on the log, so once maxRefusing of them were stuck every
// request past the waiting bound was dropped, from a replica with nothing else to do. On the real
// stack (a broker whose stdout reader took 5 KB/s, 3000 session connections at once) 1401 of the
// 3000, and 47 of 78 connections holding no credential beside them, waited out the client's 2 s,
// and 471 were dropped past_bounds; the same run against the refusal on the delivery goroutine
// (8ed6996826's) answered all but none late. Now the answer goes out first and the refusal is only
// counted (answer).
//
// Each row stalls the log, puts the reporter's write on it (stallWithALine), and then connects
// with no credential while that write is stuck: the refusal must be heard at once, and its
// goroutine must end, holding neither a decision slot nor the refusing bound's place. The last
// row's bound is one refusal, which a refusal still waiting on the log would fill.
func TestARefusalIsAnsweredWhileTheLogCannotBeWritten(t *testing.T) {
	for name, row := range map[string]struct {
		waitingBound, refusingBound int
	}{
		// Every request is past the waiting bound, refused on a goroutine of its own.
		"past the waiting bound": {0, maxRefusing},
		// A request decided in a slot and refused there.
		"refused in a decision slot": {maxWaiting, maxRefusing},
		// Past the waiting bound, with a refusing bound of one.
		"past the waiting bound, with a refusing bound of one": {0, 1},
	} {
		t.Run(name, func(t *testing.T) {
			withWaitingBound(t, row.waitingBound)
			withRefusingBound(t, row.refusingBound)
			keys := testnats.NewCalloutKeys(t)
			url := testnats.StartWithCallout(t, keys)
			log, service := startWithStalledLog(t, url, keys, Deps{})
			stallWithALine(t, url, log)
			requireRefusedAtOnce(t, url, "second")
			requireHoldsNothing(t, service)
		})
	}
}

// A refusal made while the log cannot be written does not keep its decision slot from the callers
// behind it. When answer wrote the refusal's line itself, a request refused in a slot still held
// the slot while that line waited (deliver releases it only once answer returns), so while the log
// was stalled every refusal made in a slot took that slot out of the replica for good, and once the
// slots were gone no caller, however live its credential, was decided. With one slot and the
// reporter's write stuck on the log: a connection holding no credential is refused in the slot, and
// a live session connecting after it must be admitted inside half a second.
func TestARefusalStuckOnTheLogHoldsNoDecisionSlot(t *testing.T) {
	withDecisionSlots(t, 1)
	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys)
	live := newSession(t, "ses_live", 15*time.Minute)
	log, _ := startWithStalledLog(t, url, keys, Deps{Introspect: introspector(live)})
	stallWithALine(t, url, log)

	requireRefusedAtOnce(t, url, "second")
	requireLiveAdmittedAtOnce(t, url, live, "a refusal stuck on the log")
}

// requireLiveAdmittedAtOnce requires live's connection admitted inside half a second; behind
// names what would hold the slot it needs, for the failure's message.
func requireLiveAdmittedAtOnce(t *testing.T, url string, live *session, behind string) {
	t.Helper()
	began := time.Now()
	nc, err := connectSession(t, url, live, nil)
	took := time.Since(began)
	require.NoError(t, err, "the live session was not admitted (after %s): the slot it needed was held by %s", took, behind)
	nc.Close()
	require.Less(t, took, 500*time.Millisecond, "the live session was admitted after %s, behind %s", took, behind)
}

// A pass in which Dispatch refuses the broker's own token on one question and another question
// fails is held as refused (every held daemon is refused project_access_refused), so no line of
// it may tell the operator that the last complete pass is kept: that is the opposite of what the
// broker does. Seen on the real stack: a 401 about one project beside another question whose
// connection closed logged that the last complete pass is kept, at WARNING, while every held
// daemon was refused. Here OPS's question is refused and PUB's fails.
func TestARefusedPassLogsNoKeptPass(t *testing.T) {
	now := time.Now()
	services := []ServiceEntry{{Kind: KindDaemon, Account: pubDaemonAccount}}
	dispatch := &dispatchAnswers{answers: map[string][]string{"OPS": {"acme/ops"}, "PUB": {"acme/pub"}}}
	tokens := map[string]platformToken{"token": {account: pubDaemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	var logged bytes.Buffer
	deps := Deps{
		ServiceToken: serviceTokens(tokens), Exchange: exchangeOf(map[string][]string{pubDaemonAccount: {"OPS", "PUB"}}),
		Access: dispatch.access,
		Log:    slog.New(slog.NewTextHandler(&logged, nil)),
	}
	deps.held = newHeldAccess(deps, services)
	require.NotNil(t, deps.held)
	deps.held.refresh(t.Context())
	logged.Reset()

	deps.held.access = func(_ context.Context, project, _ string) (projectaccess.Answer, error) {
		if project == "OPS" {
			return projectaccess.Answer{}, projectaccess.ErrBrokerRefused
		}
		return projectaccess.Answer{}, errors.New("projectaccess: ask dispatch: EOF")
	}
	deps.held.refresh(t.Context())

	_, err := Decide(t.Context(), deps, services, serviceRequest("token"))
	requireRefused(t, err, labelAccessRefused, "Dispatch refuses the broker's own account token")
	require.NotContains(t, logged.String(), "last complete pass is kept",
		"a refused pass was logged as keeping the last complete pass:\n%s", logged.String())
}
