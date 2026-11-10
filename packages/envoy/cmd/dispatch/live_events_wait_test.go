package main

import (
	"fmt"
	"strings"
	"testing"
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

// stallWarningHead is the opening of every stall-warning line: slog's time, level and quoted
// message, then the four attributes warnStall carries whatever its diagnosis found. The shape is
// warnStall's in internal/dispatch/events/horizon.go, as the text handler writes it, and the
// message comes from events.StallWarning rather than being retyped, so a reworded warning moves
// these rows with it.
const stallWarningHead = `time=2026-11-10T02:14:05.113Z level=WARN msg="` + events.StallWarning +
	`" pending_for=5.0987s observed_id=2 safe_horizon=0 observer_xid=4021`

// stallWarningLine is a whole stall-warning line naming database as the holder's, as warnStall's
// default arm logs one (internal/dispatch/events/horizon.go): holder, pid, xact_start, state,
// application_name and database, then the holder's query, which is quoted and holds spaces. The
// rows use whole lines rather than a database= fragment, because the field the wait must read is
// the one in the middle of such a line, with another field after it.
func stallWarningLine(database string) string {
	return stallWarningHead + ` holder=backend pid=1188 xact_start=2026-11-10T02:13:59.998Z` +
		` state=active application_name=psql database=` + database + ` query="begin; select 1"`
}

// eventsStartVerdictName names a verdict, so a failed row reads as the decision the wait made
// rather than as the integer the constants are.
func eventsStartVerdictName(verdict eventsStartVerdict) string {
	switch verdict {
	case eventsStartWaitOn:
		return "eventsStartWaitOn"
	case eventsStartNoSignal:
		return "eventsStartNoSignal"
	case eventsStartSignalStopped:
		return "eventsStartSignalStopped"
	case eventsStartOwnHold:
		return "eventsStartOwnHold"
	}
	return fmt.Sprintf("eventsStartVerdict(%d)", int(verdict))
}

// TestEventsStartVerdictOfKeysTheWaitOnTheServersStallWarning drives every rule of the wait's
// judgement, with the boundary either side where a rule has one. It needs no server, no Postgres
// and no network: the judgement is a function of its state alone.
//
// Every duration is written in terms of eventsStartBase and eventsStartExtension rather than as
// seconds, so that a later change of either constant moves the rows with it instead of leaving
// them to test a boundary that has moved.
func TestEventsStartVerdictOfKeysTheWaitOnTheServersStallWarning(t *testing.T) {
	const ownDatabase = "dispatch_test_ab"
	// A foreign holder: another service's transaction on the same Postgres instance, which holds
	// the horizon back and is no defect of Dispatch's.
	foreign := stallWarningLine("legion_daemon")
	// warnStall's other two arms carry no database= field at all.
	diagnosisFailed := stallWarningHead + ` diagnosis_error="timeout: context deadline exceeded"`
	noHolder := stallWarningHead + ` holder="none open now"`
	for _, tc := range []struct {
		name  string
		state eventsStartState
		want  eventsStartVerdict
	}{
		{
			"a server that has said nothing and is still inside the base bound is waited on, so a merely slow server does not fail the test early",
			eventsStartState{
				elapsed:     eventsStartBase - time.Millisecond,
				ownDatabase: ownDatabase,
			},
			eventsStartWaitOn,
		},
		{
			"a server that never said it was held is failed at the base bound rather than waited out",
			eventsStartState{
				elapsed:     eventsStartBase,
				ownDatabase: ownDatabase,
			},
			eventsStartNoSignal,
		},
		{
			"a server that keeps saying it is held is waited on far past the base bound, which is the flaky deadline this wait replaces",
			eventsStartState{
				elapsed:         4 * eventsStartBase,
				sinceNewWarning: eventsStartExtension / 2,
				warnings:        7,
				lastWarning:     foreign,
				ownDatabase:     ownDatabase,
			},
			eventsStartWaitOn,
		},
		{
			"a server whose newest warning is still inside the extension window is waited on, however long it has been held",
			eventsStartState{
				elapsed:         4 * eventsStartBase,
				sinceNewWarning: eventsStartExtension - time.Millisecond,
				warnings:        7,
				lastWarning:     foreign,
				ownDatabase:     ownDatabase,
			},
			eventsStartWaitOn,
		},
		{
			"a hold nobody releases ends at the extension window in a failure that names it, rather than in a wait (docs/testing.md)",
			eventsStartState{
				elapsed:         4 * eventsStartBase,
				sinceNewWarning: eventsStartExtension,
				warnings:        7,
				lastWarning:     foreign,
				ownDatabase:     ownDatabase,
			},
			eventsStartSignalStopped,
		},
		{
			"a warning naming this process's own database fails at once, so Dispatch holding up its own readers is never waited out (dispatch://INFRA-636)",
			eventsStartState{
				elapsed:     3 * time.Millisecond,
				warnings:    1,
				lastWarning: stallWarningLine(ownDatabase),
				ownDatabase: ownDatabase,
			},
			eventsStartOwnHold,
		},
		{
			"a warning whose diagnosis failed names no database at all and still extends the wait, because the server is saying it is held",
			eventsStartState{
				elapsed:         2 * eventsStartBase,
				sinceNewWarning: eventsStartExtension / 2,
				warnings:        2,
				lastWarning:     diagnosisFailed,
				ownDatabase:     ownDatabase,
			},
			eventsStartWaitOn,
		},
		{
			"a warning that found no holder still open names no database either and extends the wait for the same reason",
			eventsStartState{
				elapsed:         2 * eventsStartBase,
				sinceNewWarning: eventsStartExtension / 2,
				warnings:        2,
				lastWarning:     noHolder,
				ownDatabase:     ownDatabase,
			},
			eventsStartWaitOn,
		},
		{
			"an own-database warning fails before the base bound and inside the extension window, so the own-hold rule is read before the two that would wait",
			eventsStartState{
				elapsed:         eventsStartBase - time.Millisecond,
				sinceNewWarning: eventsStartExtension / 2,
				warnings:        1,
				lastWarning:     stallWarningLine(ownDatabase),
				ownDatabase:     ownDatabase,
			},
			eventsStartOwnHold,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventsStartVerdictOf(tc.state); got != tc.want {
				t.Errorf("eventsStartVerdictOf(%+v) = %s, want %s",
					tc.state, eventsStartVerdictName(got), eventsStartVerdictName(tc.want))
			}
		})
	}
}

// TestStallWarningNamesDatabaseMatchesTheWholeDatabaseField holds the trap the helper exists to
// avoid: a prefix match would read a foreign hold as Dispatch's own and fail the test at once on
// a database the process does not own.
func TestStallWarningNamesDatabaseMatchesTheWholeDatabaseField(t *testing.T) {
	const ownDatabase = "dispatch_test_ab"
	for _, tc := range []struct {
		name     string
		warning  string
		database string
		want     bool
	}{
		{
			"a warning naming a longer database that opens with this one's name is not read as naming this one",
			stallWarningLine(ownDatabase + "cd"), ownDatabase, false,
		},
		{
			"a warning naming exactly this database is read as naming it",
			stallWarningLine(ownDatabase), ownDatabase, true,
		},
		{
			"a line carrying no database= field at all names nothing",
			stallWarningHead + ` holder="none open now"`, ownDatabase, false,
		},
		{
			"a database= field at the line's end, with no space after it, is still read whole",
			stallWarningHead + " database=" + ownDatabase, ownDatabase, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stallWarningNamesDatabase(tc.warning, tc.database); got != tc.want {
				t.Errorf("stallWarningNamesDatabase(%q, %q) = %t, want %t",
					tc.warning, tc.database, got, tc.want)
			}
		})
	}
}

// TestLastStallWarningAnswersTheNewestWholeWarningLine proves the wait reads the newest warning
// whole: the line is what a failure prints, and the database= field in it is what decides whether
// the hold is Dispatch's own.
func TestLastStallWarningAnswersTheNewestWholeWarningLine(t *testing.T) {
	const other = `time=2026-11-10T02:14:05.001Z level=INFO msg="dispatch: live socket subscribed" sub=events`
	older := stallWarningLine("postgres")
	newest := stallWarningLine("legion_daemon")
	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{
			"output holding several warnings among other lines answers the newest one whole, with its fields and without the lines around it",
			strings.Join([]string{other, older, other, newest, other}, "\n") + "\n",
			newest,
		},
		{
			"output holding no warning at all answers the empty string, which the wait reads as a server that has said nothing",
			strings.Join([]string{other, other}, "\n") + "\n",
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastStallWarning(tc.output); got != tc.want {
				t.Errorf("lastStallWarning() = %q, want %q", got, tc.want)
			}
		})
	}
}
