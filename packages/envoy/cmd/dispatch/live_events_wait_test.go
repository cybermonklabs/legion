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
// missing frame is a defect. The one stall the test refuses to wait out is a hold in the
// process's own database that has already stood eventsStartOwnHoldBound: that is the live defect
// class of INFRA-636 (a Dispatch write transaction left open across the horizon's proof), which
// this test exists to fail on. The bound is what keeps that rule from failing on every other
// client of that database - the test's own fixtures, an operator's psql session - whose ordinary
// short transaction holds the horizon exactly as a foreign one does and is no defect.
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
	// eventsStartOwnHoldBound is how long a hold in the process's own database must have stood,
	// by the server's own account, before the wait fails on it rather than counting it as a
	// stall. Without a bound the own-database rule is a deadline of about 0.3 s on any
	// transaction in that database, from any client: every test process runs stallAfter at
	// testHorizonStallWarning (250 ms), so the first warning naming the database ends the wait
	// almost at once.
	//
	// Sampling pg_stat_activity every 100 ms while this test ran 10 times under the calibrated
	// load (a rolling foreign hold, a new 25 s transaction every second in postgres, beside 8
	// busy loops at four times nproc), in this issue's pod on 2026-11-10, nproc 2, PostgreSQL
	// 18.6: 202 samples caught a transaction holding a transaction id in a dispatch_test_*
	// database, across 27 distinct backends, every one of them Dispatch's own (empty
	// application_name). The worst age was 1.449 s. Three times that is 4.35 s, so the bound is
	// 5 s, rounded up to a whole second. That sampling rate means a transaction shorter than
	// 100 ms can be missed entirely, which does not matter for a worst case: a hold too short
	// to be sampled is far below the one the bound is sized from.
	eventsStartOwnHoldBound = 5 * time.Second
)

// waitEventsStart waits for subscription sub's events_start frame on conn, which the caller has
// just subscribed, and returns once it has arrived. The frame is behind the event horizon, so the
// wait is on the server's own account of itself rather than on a clock: while the server keeps
// logging events.StallWarning the wait goes on, and it fails - with the process's whole output,
// naming the newest warning - when the server never said it was held, when it stopped saying so,
// or when the hold is Dispatch's own (eventsStartVerdictOf).
func (p *dispatchProcess) waitEventsStart(t *testing.T, conn *gws.Conn, sub string) {
	t.Helper()
	subscribed := time.Now()
	// Warnings already logged belong to a hold before this subscribe, perhaps one long over, so
	// the wait counts only what the server says from here on.
	before := len(p.logLines(events.StallWarning))
	read := readEventsStart(conn, sub)
	ticker := time.NewTicker(eventsStartPoll)
	defer ticker.Stop()
	// A run given no deadline (go test -timeout 0) has no drain arm to fail on: nil channel.
	var runOut <-chan time.Time
	if deadline, ok := t.Deadline(); ok {
		drain := time.NewTimer(time.Until(deadline.Add(-eventsStartDrain)))
		defer drain.Stop()
		runOut = drain.C
	}
	state := eventsStartState{ownDatabase: p.database}
	rose := subscribed
	for {
		select {
		case result := <-read:
			p.requireEventsStart(t, result)
			return
		case <-runOut:
			p.failEventsStart(t, read, "the event feed's events_start did not arrive in the %v the run had left; the horizon's newest stall warning was %q\n%s",
				time.Since(subscribed).Round(time.Millisecond), state.lastWarning, p.Output.String())
			return
		case now := <-ticker.C:
			// Count, never Contains: a contains check latches on the first warning and
			// would extend the wait for ever on a server that has stopped warning. The
			// count is of the lines that hold the message rather than of the message's
			// occurrences - two different quantities, which agree here because the
			// message is slog's msg field and warnStall is its only producer - and the
			// line is what the failures print, since it carries the holder's kind, pid,
			// database and query.
			warnings := p.logLines(events.StallWarning)
			if since := len(warnings) - before; since > state.warnings {
				state.warnings = since
				state.lastWarning = warnings[len(warnings)-1]
				rose = now
			}
			state.elapsed = now.Sub(subscribed)
			state.sinceNewWarning = now.Sub(rose)
			switch eventsStartVerdictOf(state) {
			case eventsStartNoSignal:
				p.failEventsStart(t, read, "the event feed's events_start did not arrive in %v, and the server never said the event horizon was held back by an open transaction\n%s",
					state.elapsed.Round(time.Millisecond), p.Output.String())
				return
			case eventsStartSignalStopped:
				p.failEventsStart(t, read, "the event feed's events_start did not arrive in %v, and the server stopped saying the event horizon was held back %v ago; its newest stall warning was %q\n%s",
					state.elapsed.Round(time.Millisecond), state.sinceNewWarning.Round(time.Millisecond), state.lastWarning, p.Output.String())
				return
			case eventsStartOwnHold:
				p.failEventsStart(t, read, "the event feed's events_start did not arrive in %v, and the server says a client of its own database %s has held the event horizon back for %s (%s); its newest stall warning was %q\n%s",
					state.elapsed.Round(time.Millisecond), p.database,
					stallWarningField(state.lastWarning, "pending_for"), stallWarningClient(state.lastWarning),
					state.lastWarning, p.Output.String())
				return
			}
		}
	}
}

// requireEventsStart takes one result the reader delivered: it returns on the events_start the
// wait is for, and fails the test on the read that failed or the frame that came in its place.
// Both arms that can take a delivered result go through it, so neither can come to answer a
// delivered frame differently from the other.
func (p *dispatchProcess) requireEventsStart(t *testing.T, result eventsStartRead) {
	t.Helper()
	switch {
	case result.err != nil:
		t.Fatalf("read the event feed's events_start: %v\n%s", result.err, p.Output.String())
	case result.unexpected != "":
		t.Fatalf("the live socket answered the event feed's subscribe with a %s frame, want its events_start", result.unexpected)
	}
}

// failEventsStart is the one place the wait fails from. A result the reader has already delivered
// wins over any verdict: the drain timer and the ticker can be ready at the same select, which
// chooses uniformly among the arms that are ready, so a frame delivered in the same tick as a
// failing verdict would otherwise fail a run that had read its events_start. Every caller returns
// after calling it, which is right either way: t.Fatalf does not return, and a delivered result
// has been answered here.
func (p *dispatchProcess) failEventsStart(t *testing.T, read <-chan eventsStartRead, format string, args ...any) {
	t.Helper()
	select {
	case result := <-read:
		p.requireEventsStart(t, result)
		return
	default:
	}
	t.Fatalf(format, args...)
}

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
// Three readings fall out of the rules, and all three are the safe ones. A warning whose diagnosis
// failed carries no database= field at all (horizon.go's diagnosis_error branch), and neither does
// one that found no holder (holder="none open now"); since neither names the test's own database,
// each counts as a stall and extends the wait. A warning whose pending_for is missing or will not
// parse is read the same way: the quantity the own-hold rule is judged on is absent, so the
// warning is taken as the plain stall it also is. The known limit is the other way about: the
// warning names only the oldest holder at diagnosis time, so a foreign holder can mask one of
// Dispatch's own.
//
// pending_for is the age of the oldest pending observation rather than of the holder the warning
// names, so a foreign holder that has since released can carry it above eventsStartOwnHoldBound
// while ours is the one named. That is accepted, because the alternative needs a clock of how long
// this holder has been the named one, which the test does not have.
func eventsStartVerdictOf(state eventsStartState) eventsStartVerdict {
	// A missing or unparseable pending_for leaves this at 0, below the bound, which is what
	// makes such a warning extend the wait rather than end it.
	pendingFor, _ := time.ParseDuration(stallWarningField(state.lastWarning, "pending_for"))
	switch {
	// A hold in this process's own database that has stood the bound is the defect, whatever
	// the elapsed time: waiting it out would pass a test whose job is to fail on it
	// (dispatch://INFRA-636). A shorter one is some client's ordinary transaction and extends
	// the wait, exactly as a foreign warning does.
	case state.warnings > 0 && pendingFor >= eventsStartOwnHoldBound &&
		stallWarningNamesDatabase(state.lastWarning, state.ownDatabase):
		return eventsStartOwnHold
	case state.warnings == 0 && state.elapsed >= eventsStartBase:
		return eventsStartNoSignal
	case state.warnings > 0 && state.sinceNewWarning >= eventsStartExtension:
		return eventsStartSignalStopped
	default:
		return eventsStartWaitOn
	}
}

// stallWarningField returns the value of warning's key= field, warning being one slog text line:
// the token from that "=" to the next space or the line's end, with surrounding double quotes
// stripped, and "" when the line carries no such field. A quoted value holding a space - the
// warning's query= - is not read whole, and no caller needs one: the four fields read here
// (database, pending_for, pid, application_name) carry no space.
func stallWarningField(warning, key string) string {
	_, value, found := strings.Cut(warning, key+"=")
	if !found {
		return ""
	}
	token, _, _ := strings.Cut(value, " ")
	return strings.Trim(token, `"`)
}

// stallWarningNamesDatabase reports whether warning's database= field is database. The match is of
// that whole field: a prefix match would read a warning naming dispatch_test_abcd as naming
// dispatch_test_ab.
func stallWarningNamesDatabase(warning, database string) bool {
	return stallWarningField(warning, "database") == database
}

// stallWarningClient names the client the warning found holding the transaction, for the own-hold
// failure. Any client of the process's own database holds the horizon back the same way - Dispatch
// itself, an operator's psql session, another test's fixture - so the failure names which one
// rather than saying Dispatch holds it. A client that set no application_name, which is what
// Dispatch's own connections look like, is said to have set none rather than printed as a blank.
func stallWarningClient(warning string) string {
	application, pid := stallWarningField(warning, "application_name"), stallWarningField(warning, "pid")
	if application == "" {
		application = "(none set)"
	}
	if pid == "" {
		pid = "(none named)"
	}
	return "application_name " + application + ", pid " + pid
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
// message, then the four attributes warnStall carries whatever its diagnosis found. pendingFor is
// the one of those four the wait reads, so each row writes the value it wants. The shape is
// warnStall's in internal/dispatch/events/horizon.go, as the text handler writes it, and the
// message comes from events.StallWarning rather than being retyped, so a reworded warning moves
// these rows with it.
func stallWarningHead(pendingFor time.Duration) string {
	return `time=2026-11-10T02:14:05.113Z level=WARN msg="` + events.StallWarning +
		`" pending_for=` + pendingFor.String() + ` observed_id=2 safe_horizon=0 observer_xid=4021`
}

// stallWarningLine is a whole stall-warning line naming database as the holder's and pendingFor as
// how long the oldest pending observation has waited, as warnStall's default arm logs one
// (internal/dispatch/events/horizon.go): holder, pid, xact_start, state, application_name and
// database, then the holder's query, which is quoted and holds spaces. The rows use whole lines
// rather than a database= fragment, because the fields the wait must read sit in the middle of
// such a line, with another field after each.
func stallWarningLine(database string, pendingFor time.Duration) string {
	return stallWarningHead(pendingFor) + ` holder=backend pid=1188 xact_start=2026-11-10T02:13:59.998Z` +
		` state=active application_name=psql database=` + database + ` query="begin; select 1"`
}

// String names a verdict, so a failed row reads as the decision the wait made rather than as the
// integer the constants are.
func (verdict eventsStartVerdict) String() string {
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
// Every duration is written in terms of eventsStartBase, eventsStartExtension and
// eventsStartOwnHoldBound rather than as seconds, so that a later change of any of the three
// moves the rows with it instead of leaving them to test a boundary that has moved.
func TestEventsStartVerdictOfKeysTheWaitOnTheServersStallWarning(t *testing.T) {
	const ownDatabase = "dispatch_test_ab"
	// A foreign holder: another service's transaction on the same Postgres instance, which holds
	// the horizon back and is no defect of Dispatch's, however long it has stood.
	foreign := stallWarningLine("legion_daemon", 2*eventsStartOwnHoldBound)
	// warnStall's other two arms carry no database= field at all.
	diagnosisFailed := stallWarningHead(2*eventsStartOwnHoldBound) + ` diagnosis_error="timeout: context deadline exceeded"`
	noHolder := stallWarningHead(2*eventsStartOwnHoldBound) + ` holder="none open now"`
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
			"a warning naming this process's own database whose pending_for has passed the own-hold bound fails whatever the elapsed time, so Dispatch holding up its own readers is never waited out (dispatch://INFRA-636)",
			eventsStartState{
				elapsed:     3 * time.Millisecond,
				warnings:    1,
				lastWarning: stallWarningLine(ownDatabase, 2*eventsStartOwnHoldBound),
				ownDatabase: ownDatabase,
			},
			eventsStartOwnHold,
		},
		{
			"a warning naming this process's own database whose pending_for is under the own-hold bound extends the wait like any other stall, so a client's second-and-a-half transaction in that database fails nothing",
			eventsStartState{
				elapsed:         4 * eventsStartBase,
				sinceNewWarning: eventsStartExtension / 2,
				warnings:        5,
				lastWarning:     stallWarningLine(ownDatabase, eventsStartOwnHoldBound-time.Millisecond),
				ownDatabase:     ownDatabase,
			},
			eventsStartWaitOn,
		},
		{
			"a warning naming this process's own database fails at the own-hold bound itself, which is where the rule starts to bite",
			eventsStartState{
				elapsed:         4 * eventsStartBase,
				sinceNewWarning: eventsStartExtension / 2,
				warnings:        5,
				lastWarning:     stallWarningLine(ownDatabase, eventsStartOwnHoldBound),
				ownDatabase:     ownDatabase,
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
				lastWarning:     stallWarningLine(ownDatabase, 2*eventsStartOwnHoldBound),
				ownDatabase:     ownDatabase,
			},
			eventsStartOwnHold,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventsStartVerdictOf(tc.state); got != tc.want {
				t.Errorf("eventsStartVerdictOf(%+v) = %s, want %s", tc.state, got, tc.want)
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
			stallWarningLine(ownDatabase+"cd", eventsStartOwnHoldBound), ownDatabase, false,
		},
		{
			"a warning naming exactly this database is read as naming it",
			stallWarningLine(ownDatabase, eventsStartOwnHoldBound), ownDatabase, true,
		},
		{
			"a line carrying no database= field at all names nothing",
			stallWarningHead(eventsStartOwnHoldBound) + ` holder="none open now"`, ownDatabase, false,
		},
		{
			"a database= field at the line's end, with no space after it, is still read whole",
			stallWarningHead(eventsStartOwnHoldBound) + " database=" + ownDatabase, ownDatabase, true,
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
