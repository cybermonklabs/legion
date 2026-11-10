package main

import (
	"fmt"
	"strconv"
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
	// database, across 27 distinct backends, every one of them carrying no application_name -
	// which is what Dispatch's own pools look like and what this test's own storetest pool
	// looks like too, so the samples do not say whose the transactions were. The worst age was
	// 1.449 s. Three times that is 4.35 s, so the bound is
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
// or when the hold is one in the process's own database (eventsStartVerdictOf).
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
				// The verdict rests on a pending_for that parsed, so the field is there
				// to print, and it is printed as the server wrote it.
				pendingFor, _ := stallWarningField(state.lastWarning, "pending_for")
				p.failEventsStart(t, read, "the event feed's events_start did not arrive in %v, and the server says a client of its own database %s has held the event horizon back for %s (%s); its newest stall warning was %q\n%s",
					state.elapsed.Round(time.Millisecond), p.database,
					pendingFor, stallWarningClient(state.lastWarning),
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
	eventsStartOwnHold                          // the stall is a hold in the process's own database, not a foreign one
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
// while ours is the one named. That is accepted, and pending_for stays the quantity the rule is
// gated on, because outside that race the named holder was already open when the observation
// began and so has been open at least as long as pending_for: a rule gated on pending_for fires
// no sooner than one gated on the holder's own age, which is the conservative reading against
// the 1.449 s worst case the bound is sized from. The holder's own age is on the line too -
// xact_start, against the record's time= - and reading that instead would only fire sooner.
func eventsStartVerdictOf(state eventsStartState) eventsStartVerdict {
	// A pending_for the line does not carry, and one that will not parse, both leave this at
	// 0, below the bound, which is what makes such a warning extend the wait rather than end
	// it.
	field, _ := stallWarningField(state.lastWarning, "pending_for")
	pendingFor, _ := time.ParseDuration(field)
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

// stallWarningField returns the value of warning's key= field, warning being one slog text line,
// and whether the line carried that field at all. A field the line does not carry and a field
// carrying the empty string are different things to a caller: reading the first as the second had
// stallWarningNamesDatabase answer true for a line with no database= field at all whenever the
// database asked about was "".
//
// The read walks the line as the sequence of key=value fields it is, rather than searching it for
// the text "key=", because a field's value is chosen by a client and so can hold that text: a
// client whose application_name is "database=dispatch_test_ab backfill" would have a search read
// that name as the line's database= field, and so have a hold in a foreign database read as one
// in the process's own. The walk compares whole keys too, so a caller asking for "name" is not
// given application_name's value.
//
// A value slog quoted is read whole and unquoted, and the walk resumes past the whole quoted
// literal, so the warning's query= and an application_name the client set with a space in it
// (pgAdmin sets "pgAdmin 4 - <connection>") both read as they were written; an unquoted value is
// the token from the "=" to the next space or the line's end.
func stallWarningField(warning, key string) (string, bool) {
	for rest := warning; rest != ""; {
		name, after, found := strings.Cut(rest, "=")
		if !found {
			return "", false
		}
		var value string
		if literal, err := strconv.QuotedPrefix(after); err == nil {
			// QuotedPrefix answers a literal it has already parsed, so Unquote cannot fail
			// on it.
			unquoted, _ := strconv.Unquote(literal)
			value, rest = unquoted, strings.TrimPrefix(after[len(literal):], " ")
		} else {
			value, rest, _ = strings.Cut(after, " ")
		}
		if name == key {
			return value, true
		}
	}
	return "", false
}

// stallWarningNamesDatabase reports whether warning carries a database= field whose value is
// database. Both halves of that matter. The match is of the whole field, because a prefix match
// would read a warning naming dispatch_test_abcd as naming dispatch_test_ab; and a line carrying
// no database= field at all names no database rather than the empty one, so an empty database
// argument matches only a line that really carries database="".
func stallWarningNamesDatabase(warning, database string) bool {
	value, found := stallWarningField(warning, "database")
	return found && value == database
}

// stallWarningClient names the client the warning found holding the transaction, for the own-hold
// failure. Any client of the process's own database holds the horizon back the same way - Dispatch
// itself, an operator's psql session, another test's fixture - so the failure names which one
// rather than saying Dispatch holds it. A client that set no application_name, which is what
// Dispatch's own connections look like, is said to have set none rather than printed as a blank.
func stallWarningClient(warning string) string {
	application, _ := stallWarningField(warning, "application_name")
	pid, _ := stallWarningField(warning, "pid")
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
// message, then the attributes warnStall carries whatever its diagnosis found. pending_for is the
// one of them the wait reads, so each row writes the value it wants, and
// stallWarningHeadNoPendingFor writes the head of the one shape that carries no pending_for at
// all. The shape is warnStall's in internal/dispatch/events/horizon.go, as the text handler
// writes it, and the message comes from events.StallWarning rather than being retyped, so a
// reworded warning moves these rows with it.
func stallWarningHead(pendingFor time.Duration) string {
	return stallWarningHeadOf(" pending_for=" + pendingFor.String())
}

// stallWarningHeadNoPendingFor is the head of a warning carrying no pending_for field at all.
// eventsStartVerdictOf reads such a line as the plain stall it also is, so a row must be able to
// write one without hand-writing the whole line.
func stallWarningHeadNoPendingFor() string {
	return stallWarningHeadOf("")
}

// stallWarningHeadOf writes the head with the pending_for field already rendered: either
// " pending_for=<value>" or nothing at all.
func stallWarningHeadOf(pendingFor string) string {
	return `time=2026-11-10T02:14:05.113Z level=WARN msg="` + events.StallWarning + `"` + pendingFor +
		` observed_id=2 safe_horizon=0 observer_xid=4021`
}

// stallWarningLine is a whole stall-warning line naming database as the holder's and pendingFor as
// how long the oldest pending observation has waited, as warnStall's default arm logs one for a
// psql session (internal/dispatch/events/horizon.go). The rows use whole lines rather than a
// database= fragment, because the fields the wait must read sit in the middle of such a line,
// with another field after each.
func stallWarningLine(database string, pendingFor time.Duration) string {
	return stallWarningHead(pendingFor) + stallWarningHolder("psql", database)
}

// stallWarningHolder is what warnStall's default arm appends once its diagnosis found a holder:
// holder, pid, xact_start, state, application_name and database, then the holder's query, which
// is quoted and holds spaces. application is written as slog would write what the client set, so
// a row that wants the quoted application_name of a client whose name holds a space passes it
// with its quotes.
func stallWarningHolder(application, database string) string {
	return ` holder=backend pid=1188 xact_start=2026-11-10T02:13:59.998Z state=active` +
		` application_name=` + application + ` database=` + database + ` query="begin; select 1"`
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
			"a warning naming this process's own database whose pending_for has passed the own-hold bound fails whatever the elapsed time, so a hold in the process's own database holding up its live readers is never waited out (dispatch://INFRA-636)",
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
			"a warning naming this process's own database that carries no pending_for field at all extends the wait, because the own-hold rule is judged on an age and a warning carrying none cannot end the wait",
			eventsStartState{
				elapsed:         2 * eventsStartBase,
				sinceNewWarning: eventsStartExtension / 2,
				warnings:        2,
				lastWarning:     stallWarningHeadNoPendingFor() + stallWarningHolder("psql", ownDatabase),
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
		{
			"a line carrying no database= field at all is not read as naming the empty database, which a field read that could not tell a missing field from an empty one would have it do",
			stallWarningHead(eventsStartOwnHoldBound) + ` holder="none open now"`, "", false,
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

// TestStallWarningNamesDatabaseRestsOnAFieldReadThatTakesAQuotedValueWhole holds the other traps
// of the field read both that helper and the own-hold failure's client naming go through. slog
// quotes a value holding a space, and a read that stopped at the first space would cut it. The
// field that meets one is application_name, because the client sets it - pgAdmin names its
// connections "pgAdmin 4 - <connection>" - and it is what stallWarningClient names the holding
// client by. That the client sets it is the second trap: the name it chooses can hold the text of
// another field, so the read must find the line's own fields rather than text the line carries,
// and it must tell a field from one whose name ends in the key asked for.
func TestStallWarningNamesDatabaseRestsOnAFieldReadThatTakesAQuotedValueWhole(t *testing.T) {
	const ownDatabase = "dispatch_test_ab"
	const foreignDatabase = "dispatch_test_cd"
	for _, tc := range []struct {
		name        string // what the row proves
		warning     string // the whole stall-warning line the row reads
		key         string // the field the row asks the read for
		value       string // the value the read must answer
		found       bool   // whether the line carries that field at all
		application string // the application_name the line's holder set, which the client naming carries whole
		namesOwn    bool   // whether the line is read as naming this process's own database
	}{
		{
			name:        "a quoted application_name holding a space is read whole, so the own-hold failure names the client as it named itself rather than cutting it at its first space",
			warning:     stallWarningHead(eventsStartOwnHoldBound) + stallWarningHolder(`"pgAdmin 4 - dispatch"`, ownDatabase),
			key:         "application_name",
			value:       "pgAdmin 4 - dispatch",
			found:       true,
			application: "pgAdmin 4 - dispatch",
			namesOwn:    true,
		},
		{
			name:        "a client chooses its own application_name, so a line whose application_name holds the text database=<this process's own database> is read as the hold in another database that its own database= field says it is: a field read that searched the line as text answered that text instead, and had the wait fail on a foreign hold as its own",
			warning:     stallWarningHead(eventsStartOwnHoldBound) + stallWarningHolder(`"database=`+ownDatabase+` backfill"`, foreignDatabase),
			key:         "database",
			value:       foreignDatabase,
			found:       true,
			application: "database=" + ownDatabase + " backfill",
			namesOwn:    false,
		},
		{
			name:        "a key that is the tail of a longer key is not that key: a line carrying application_name and no name= field of its own carries no name= field at all",
			warning:     stallWarningHead(eventsStartOwnHoldBound) + stallWarningHolder(`"pgAdmin 4 - dispatch"`, ownDatabase),
			key:         "name",
			value:       "",
			found:       false,
			application: "pgAdmin 4 - dispatch",
			namesOwn:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found := stallWarningField(tc.warning, tc.key)
			if got != tc.value || found != tc.found {
				t.Errorf("stallWarningField(%q, %q) = %q, %t; want %q, %t", tc.warning, tc.key, got, found, tc.value, tc.found)
			}
			if client := stallWarningClient(tc.warning); !strings.Contains(client, tc.application) {
				t.Errorf("stallWarningClient(%q) = %q, want it to name the client %q", tc.warning, client, tc.application)
			}
			if names := stallWarningNamesDatabase(tc.warning, ownDatabase); names != tc.namesOwn {
				t.Errorf("stallWarningNamesDatabase(%q, %q) = %t, want %t", tc.warning, ownDatabase, names, tc.namesOwn)
			}
		})
	}
}
