package main

import (
	"strings"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// Waiting for the event feed's events_start.
//
// events_start is the last thing a new subscription sends: the relay awaits the event horizon and
// writes the frame only once Await has returned (internal/dispatch/api/live_events.go,
// event_stream_relay.go). No fixed deadline bounds that wait. The horizon's proof is
// instance-wide, as internal/dispatch/events/horizon.go's "Costs" section says: "Any transaction
// that holds a transaction id, in any database on the Postgres instance (Dispatch's own writes,
// another service's on the same Cloud SQL instance, an operator's open psql session, a prepared
// transaction), holds H back until it ends". The quantity is therefore unbounded by design, and
// the largest gap a sample has shown is not a bound on it: a deadline raised to fit yesterday's
// worst run is the same flake tomorrow, on a CI machine where someone else's transaction stands
// for a minute.
//
// So the wait keys on the server's own account of itself. While Dispatch keeps logging
// events.StallWarning, it is saying it is held back by an open transaction, and the test waits on
// for as long as that keeps arriving. When the server says nothing - no warning at all, or no new
// warning for a window - the test fails fast, because then nothing is holding the horizon and the
// missing frame is a defect. The one stall the test refuses to wait out is Dispatch's own: a
// warning naming the process's own database is the live defect class of INFRA-636 (a Dispatch
// write transaction left open across the horizon's proof), which this test exists to fail on.
//
// The numbers below were measured in this issue's pod on 2026-11-10, nproc 2, against PostgreSQL
// 18.6 on 127.0.0.1:5433, each arm driving this one test:
//
//   - U = 217 ms, the worst subscribe-to-events_start gap with no transaction held, over 20
//     subscriptions in 10 runs under 8 busy loops (four times nproc, nice -n 10), all 10 runs
//     passing. CPU load alone does not reproduce the failure.
//   - W = 0.460 s, the worst subscribe-to-first-stall-warning gap, over 10 windows under both
//     loads at once: those busy loops, and a pulse holding a transaction 12 s of every 13.
//   - G = 0.319 s, the worst gap between two consecutive stall warnings while one hold stood, over
//     324 samples under those same two loads.
//
// A gorilla connection is unusable after a read timeout - "After a read has timed out, the
// websocket connection state is corrupt" - and supports one concurrent reader, so the helper sets
// no read deadline at all and reads on a goroutine of its own. The invariant that makes that safe:
// nothing here touches the connection after the helper returns, because the caller reads it next
// (socketEnd and watchAgent in shutdown_test.go, openEventFeed in multiprocess_test.go).
const (
	// eventsStartBase is how long the wait gives a server that has said nothing. Three times the
	// worst of U and W is 1.38 s, but the floor is the soonest a held server can have said so:
	// the test process's stallAfter (testHorizonStallWarning, 250 ms) plus its warnEvery (250 ms)
	// plus horizonDiagnosisTimeout (2 s), which is 2.5 s. Rounded up to a whole second.
	eventsStartBase = 3 * time.Second
	// eventsStartExtension is how long the wait gives a server that has said it is held, since
	// its newest warning. Three times G is 0.96 s, but G's own ceiling is higher than anything
	// measured here, so this one comes from the server's constants rather than the measurement:
	// warnStall logs after its pg_stat_activity diagnosis, on a goroutine of its own (the
	// go h.warnStall(...) call in horizon.go's observation loop), and that diagnosis is bounded
	// by horizonDiagnosisTimeout (2 s), so two logged warnings can stand warnEvery (250 ms) plus
	// horizonBackoffMax (250 ms) plus horizonDiagnosisTimeout apart - 2.5 s - on a server whose
	// pg_stat_activity is slow, which is the server this test meets in CI. Rounded up to a whole
	// second.
	eventsStartExtension = 3 * time.Second
	// eventsStartPoll is how often the wait reads the process's output. A sampling rate, not a
	// bound: it is below a tenth of the extension window, so it costs that window nothing.
	eventsStartPoll = 250 * time.Millisecond
	// eventsStartDrain is what the wait leaves of the run's own deadline, so its failure prints
	// rather than being cut off by the package timeout, which would name no test.
	eventsStartDrain = 30 * time.Second
)

// eventsStartState is what the wait knows at one poll.
type eventsStartState struct {
	elapsed         time.Duration // since the subscribe went out
	sinceNewWarning time.Duration // since the stall-warning count last rose; meaningless while warnings is 0
	warnings        int           // stall warnings in the process's output since the subscribe
	lastWarning     string        // the newest stall-warning line, "" while warnings is 0
	ownDatabase     string        // the database this process was given
}

// eventsStartVerdict is what the wait does with one eventsStartState: wait on, or fail with the
// reason the state gives.
type eventsStartVerdict int

const (
	eventsStartWaitOn        eventsStartVerdict = iota
	eventsStartNoSignal                         // nothing arrived and the server never said it was held
	eventsStartSignalStopped                    // the server stopped saying it was held, and no events_start came
	eventsStartOwnHold                          // the stall is Dispatch's own write transaction, not a foreign one
)

// eventsStartVerdictOf judges one poll of the wait. It is a function of the state alone, so it can
// be driven with no server and no Postgres.
//
// Two readings fall out of the rules, and both are the safe ones. A warning whose diagnosis failed
// carries no database= field at all (horizon.go's diagnosis_error branch), and neither does one
// that found no holder (holder="none open now"); since neither names the test's own database, each
// counts as a stall and extends the wait. The known limit is the other way about: the warning names
// only the oldest holder at diagnosis time, so a foreign holder can mask one of Dispatch's own.
func eventsStartVerdictOf(state eventsStartState) eventsStartVerdict {
	switch {
	// Dispatch holding its own horizon is the defect, whatever the elapsed time: waiting it out
	// would pass a test whose job is to fail on it (dispatch://INFRA-636).
	case state.warnings > 0 && stallWarningNamesDatabase(state.lastWarning, state.ownDatabase):
		return eventsStartOwnHold
	case state.warnings == 0 && state.elapsed >= eventsStartBase:
		return eventsStartNoSignal
	case state.warnings > 0 && state.sinceNewWarning >= eventsStartExtension:
		return eventsStartSignalStopped
	default:
		return eventsStartWaitOn
	}
}

// stallWarningNamesDatabase reports whether warning's database= field is database. The warning is
// one slog text line, so the field is the token between "database=" and the next space or the
// line's end, and the match is of that whole token: a prefix match would read a warning naming
// dispatch_test_abcd as naming dispatch_test_ab.
func stallWarningNamesDatabase(warning, database string) bool {
	_, named, found := strings.Cut(warning, "database=")
	if !found {
		return false
	}
	name, _, _ := strings.Cut(named, " ")
	return name == database
}

// lastStallWarning is the whole newest events.StallWarning line in output, "" when there is none.
// The line, not the message, because the rest of it carries the holder's kind, pid, database and
// query, which is what a failure has to print.
func lastStallWarning(output string) string {
	at := strings.LastIndex(output, events.StallWarning)
	if at < 0 {
		return ""
	}
	line := output[strings.LastIndex(output[:at], "\n")+1:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	return line
}

// eventsStartRead is the one result the events_start reader sends: the frame arrived when both
// fields are zero, else err is the read that failed or unexpected is the type of the frame that
// came in its place.
type eventsStartRead struct {
	err        error
	unexpected string
}

// readEventsStart reads conn on a goroutine of its own until subscription sub's events_start frame
// arrives, skipping heartbeats, and sends exactly one result before returning for good. It sets no
// read deadline: a gorilla connection whose read has timed out is corrupt, and the caller reads
// this one afterwards. The channel is buffered, so the goroutine leaves even when the test has
// already failed and nobody receives, and it touches conn no more once it has sent.
func readEventsStart(conn *gws.Conn, sub string) <-chan eventsStartRead {
	read := make(chan eventsStartRead, 1)
	go func() {
		for {
			var frame struct{ Type, Sub string }
			if err := conn.ReadJSON(&frame); err != nil {
				read <- eventsStartRead{err: err}
				return
			}
			if frame.Type == "events_start" && frame.Sub == sub {
				read <- eventsStartRead{}
				return
			}
			if frame.Type != "heartbeat" {
				read <- eventsStartRead{unexpected: frame.Type}
				return
			}
		}
	}()
	return read
}
