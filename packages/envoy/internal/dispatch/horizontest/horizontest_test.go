package horizontest

import (
	"context"
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// processDefault is the default logger as the process starts, before any test swapped it: slog's
// own default handler, which writes through the log package. A test whose server logs to it is the
// ordinary case, in api's tests above all.
var processDefault = slog.Default()

// TestListenOverSlogsOwnDefaultHandlerPassesEveryRecordOn listens while the default logger is
// slog's own, as it is for a test that captures no log: the stall warning is kept, and the warning,
// another slog record and a log.Print each reach the log package's writer once. Wrapped naively,
// slog's handler and the log package take the log package's lock in turn - slog.SetDefault points
// the log package at the new handler, which hands the record to slog's handler, which writes
// through the log package - and the first record logged deadlocks every goroutine that logs.
func TestListenOverSlogsOwnDefaultHandlerPassesEveryRecordOn(t *testing.T) {
	written := &logtest.Buffer{}
	previous, output, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(processDefault)
	log.SetOutput(written)
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(output)
		log.SetFlags(flags)
	})
	warnings, stop := Listen(t)
	logged := make(chan struct{})
	go func() {
		defer close(logged)
		slog.Warn(events.StallWarning, "pending_for", time.Second, "database", "postgres")
		slog.Info("dispatch: request")
		log.Print("a log package line")
	}()
	if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), testwait.Poll, func() bool {
		select {
		case <-logged:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("logging under Listen over slog's own default handler never returned: the handler and the log package deadlocked")
	}
	stop()
	if kept := warnings(); len(kept) != 1 || kept[0].PendingFor != time.Second {
		t.Fatalf("Listen kept %+v, want the one stall warning", kept)
	}
	text := written.String()
	for _, line := range []string{events.StallWarning, "dispatch: request", "a log package line"} {
		if count := strings.Count(text, line); count != 1 {
			t.Errorf("the log package's writer got %q %d times, want once; it holds %q", line, count, text)
		}
	}
}

// stallWarningHead is the opening of every stall-warning line: slog's time, level and quoted
// message, then the attributes warnStall carries whatever its diagnosis found. pending_for is the
// one of them the wait reads, so each row writes the value it wants, and
// stallWarningHeadNoPendingFor writes the head of the one shape that carries no pending_for at
// all. The shape is warnStall's in internal/dispatch/events/horizon.go, as the text handler writes
// it, and the message comes from events.StallWarning rather than being retyped, so a reworded
// warning moves these rows with it.
func stallWarningHead(pendingFor time.Duration) string {
	return stallWarningHeadOf(" pending_for=" + pendingFor.String())
}

// stallWarningHeadNoPendingFor is the head of a warning carrying no pending_for field at all.
func stallWarningHeadNoPendingFor() string {
	return stallWarningHeadOf("")
}

// stallWarningHeadOf writes the head with the pending_for field already rendered: either
// " pending_for=<value>" or nothing at all.
func stallWarningHeadOf(pendingFor string) string {
	return `time=` + stallWarningAt.Format(time.RFC3339Nano) + ` level=WARN msg="` + events.StallWarning + `"` + pendingFor +
		` observed_id=2 safe_horizon=0 observer_xid=4021`
}

// stallWarningAt is when the fixture warnings were logged. Their holder's transaction began
// 2 OwnHoldBound before (stallWarningHolderOf), so a fixture's pending_for is what the own-hold rule
// reads unless a row dates its holder otherwise (stallWarningLineHeldFor).
var stallWarningAt = time.Date(2026, 11, 10, 2, 14, 5, 113_000_000, time.UTC)

// stallWarningLine is a whole stall-warning line naming database as the holder's and pendingFor as
// how long the oldest pending observation has waited, as warnStall's default arm logs one for a
// psql session. The rows use whole lines rather than a database= fragment, because the fields the
// wait must read sit in the middle of such a line, with another field after each.
func stallWarningLine(database string, pendingFor time.Duration) string {
	return stallWarningHead(pendingFor) + stallWarningHolder("psql", database)
}

// stallWarningHolder is what warnStall's default arm appends once its diagnosis found a holder:
// holder, pid, xact_start, state, application_name and database, then the holder's query, which
// is quoted and holds spaces. application is written as slog would write what the client set, so
// a row that wants the quoted application_name of a client whose name holds a space passes it
// with its quotes.
func stallWarningHolder(application, database string) string {
	return stallWarningHolderOf(application, database, `"begin; select 1"`)
}

// stallWarningHolderOf writes those same fields with the holder's query rendered as slog would
// write what pg_stat_activity gave it, quotes and all. The query is the client's own text, like
// the application_name beside it, so a row that needs a query of a shape no query Dispatch sends
// has - one carrying a backquote - writes it here.
func stallWarningHolderOf(application, database, query string) string {
	return stallWarningHolderSince(application, database, query, stallWarningAt.Add(-2*OwnHoldBound))
}

// stallWarningHolderSince writes those fields for a holder whose transaction began at since.
func stallWarningHolderSince(application, database, query string, since time.Time) string {
	return ` holder=backend pid=1188 xact_start=` + since.Format(time.RFC3339Nano) + ` state=active` +
		` application_name=` + application + ` database=` + database + ` query=` + query
}

// stallWarningLineHeldFor is stallWarningLine for a holder whose own transaction had stood held
// when the warning named it, whatever pendingFor says of the oldest pending observation.
func stallWarningLineHeldFor(database string, pendingFor, held time.Duration) string {
	return stallWarningHead(pendingFor) + stallWarningHolderSince("psql", database, `"begin; select 1"`, stallWarningAt.Add(-held))
}

// stallWarningRecord is a stall warning as a horizon in the test's own process logs it, holder
// fields and all; attrs are its attributes after the message.
func stallWarningRecord(attrs ...any) slog.Record {
	record := slog.NewRecord(time.Now(), slog.LevelWarn, events.StallWarning, 0)
	record.Add(attrs...)
	return record
}

// heldRecord is stallWarningRecord with the fields warnStall's default arm logs for a holder in
// database, pendingFor being the oldest pending observation's age and the holder's transaction
// having begun 2 OwnHoldBound before the record.
func heldRecord(pendingFor time.Duration, database string) slog.Record {
	return heldRecordFor(pendingFor, 2*OwnHoldBound, database)
}

// heldRecordFor is heldRecord for a holder whose own transaction had stood held when it was named.
func heldRecordFor(pendingFor, held time.Duration, database string) slog.Record {
	return stallWarningRecord("pending_for", pendingFor, "observed_id", int64(2), "safe_horizon", int64(0),
		"holder", "backend", "pid", int32(86), "xact_start", time.Now().Add(-held), "state", "active",
		"application_name", "psql", "database", database, "query", "select 1")
}

// TestVerdictOfKeysTheWaitOnTheStallWarning drives every rule of the wait's judgement, with the
// boundary either side where a rule has one, on warnings read both ways a test reads them: as a
// Dispatch process's output line and as a record a horizon of the test's own process logged. It
// needs no server, no Postgres and no network: the judgement is a function of its state alone.
//
// Every duration is written in terms of Base, Extension and OwnHoldBound rather than as seconds,
// so that a later change of any of the three moves the rows with it instead of leaving them to
// test a boundary that has moved.
func TestVerdictOfKeysTheWaitOnTheStallWarning(t *testing.T) {
	const ownDatabase = "dispatch_test_ab"
	// A foreign holder: an event write of another database, which a horizon reading that database
	// reports - another server's in the same process, which Listen hears too - and which is no
	// defect of this test's, however long it has stood.
	foreign := FromLine(stallWarningLine("legion_daemon", 2*OwnHoldBound))
	// warnStall's other two arms carry no database= field at all.
	diagnosisFailed := FromLine(stallWarningHead(2*OwnHoldBound) + ` diagnosis_error="timeout: context deadline exceeded"`)
	noHolder := FromLine(stallWarningHead(2*OwnHoldBound) + ` holder="none open now"`)
	for _, row := range []struct {
		name  string
		state state
		want  verdict
	}{
		{
			"a horizon that has said nothing and is still inside the base bound is waited on, so a merely slow server does not fail the test early",
			state{elapsed: Base - time.Millisecond},
			waitOn,
		},
		{
			"a horizon that never said it was held is failed at the base bound rather than waited out",
			state{elapsed: Base},
			noSignal,
		},
		{
			"a horizon that keeps saying it is held is waited on far past the base bound, which is the flaky deadline this wait replaces",
			state{elapsed: 4 * Base, sinceNewWarning: Extension / 2, warnings: 7, last: foreign},
			waitOn,
		},
		{
			"a horizon whose newest warning is still inside the extension window is waited on, however long it has been held",
			state{elapsed: 4 * Base, sinceNewWarning: Extension - time.Millisecond, warnings: 7, last: foreign},
			waitOn,
		},
		{
			"a hold nobody releases ends at the extension window in a failure that names it, rather than in a wait",
			state{elapsed: 4 * Base, sinceNewWarning: Extension, warnings: 7, last: foreign},
			signalStopped,
		},
		{
			"a foreign hold a horizon of the test's own process reports is waited on while it warns",
			state{elapsed: time.Minute, sinceNewWarning: Extension - time.Millisecond, warnings: 9, last: FromRecord(heldRecord(time.Minute, "postgres"))},
			waitOn,
		},
		{
			"a foreign hold a horizon of the test's own process stopped reporting fails at the extension window",
			state{elapsed: time.Minute, sinceNewWarning: Extension, warnings: 9, last: FromRecord(heldRecord(time.Minute, "postgres"))},
			signalStopped,
		},
		{
			"a warning naming the test's own database whose pending_for has passed the own-hold bound fails whatever the elapsed time, so a hold holding up its live readers is never waited out (dispatch://INFRA-636)",
			state{elapsed: 3 * time.Millisecond, warnings: 1, last: FromLine(stallWarningLine(ownDatabase, 2*OwnHoldBound))},
			ownHold,
		},
		{
			"a warning naming the test's own database for a transaction that began a moment ago is no own hold, though pending_for is past the bound: a foreign holder released since carried the oldest observation's age there (the transaction named holds for its own age)",
			state{elapsed: 4 * Base, sinceNewWarning: Extension / 2, warnings: 5, last: FromLine(stallWarningLineHeldFor(ownDatabase, 2*OwnHoldBound, 100*time.Millisecond))},
			waitOn,
		},
		{
			"the same race on a record of the test's own process is no own hold either",
			state{elapsed: 4 * Base, sinceNewWarning: Extension / 2, warnings: 5, last: FromRecord(heldRecordFor(2*OwnHoldBound, 100*time.Millisecond, ownDatabase))},
			waitOn,
		},
		{
			"a transaction of the test's own database that has itself stood the bound is an own hold, whatever came before it",
			state{elapsed: 4 * Base, sinceNewWarning: Extension / 2, warnings: 5, last: FromLine(stallWarningLineHeldFor(ownDatabase, 2*OwnHoldBound, OwnHoldBound))},
			ownHold,
		},
		{
			"so is one on a record of the test's own process",
			state{elapsed: 4 * Base, sinceNewWarning: Extension / 2, warnings: 5, last: FromRecord(heldRecordFor(2*OwnHoldBound, OwnHoldBound+time.Second, ownDatabase))},
			ownHold,
		},
		{
			"a warning naming the test's own database that dates no holder (xact_start) holds for zero and extends the wait",
			state{elapsed: 4 * Base, sinceNewWarning: Extension / 2, warnings: 5, last: FromLine(stallWarningHead(2*OwnHoldBound) + ` holder=backend pid=1188 state=active application_name=psql database=` + ownDatabase)},
			waitOn,
		},
		{
			"a warning naming the test's own database whose pending_for is under the own-hold bound extends the wait like any other stall, so a client's second-and-a-half transaction in that database fails nothing",
			state{elapsed: 4 * Base, sinceNewWarning: Extension / 2, warnings: 5, last: FromLine(stallWarningLine(ownDatabase, OwnHoldBound-time.Millisecond))},
			waitOn,
		},
		{
			"a warning naming the test's own database fails at the own-hold bound itself, which is where the rule starts to bite",
			state{elapsed: 4 * Base, sinceNewWarning: Extension / 2, warnings: 5, last: FromLine(stallWarningLine(ownDatabase, OwnHoldBound))},
			ownHold,
		},
		{
			"an own-database record inside its bound extends the wait",
			state{elapsed: time.Second, warnings: 1, last: FromRecord(heldRecord(OwnHoldBound-time.Millisecond, ownDatabase))},
			waitOn,
		},
		{
			"an own-database record at its bound fails",
			state{elapsed: time.Second, warnings: 1, last: FromRecord(heldRecord(OwnHoldBound, ownDatabase))},
			ownHold,
		},
		{
			"a record naming another database whose name the own one prefixes is a foreign hold",
			state{elapsed: time.Second, warnings: 1, last: FromRecord(heldRecord(time.Minute, ownDatabase+"cd"))},
			waitOn,
		},
		{
			"a warning whose diagnosis failed names no database at all and still extends the wait, because the horizon is saying it is held",
			state{elapsed: 2 * Base, sinceNewWarning: Extension / 2, warnings: 2, last: diagnosisFailed},
			waitOn,
		},
		{
			"a warning that found no holder still open names no database either and extends the wait for the same reason",
			state{elapsed: 2 * Base, sinceNewWarning: Extension / 2, warnings: 2, last: noHolder},
			waitOn,
		},
		{
			"a warning naming the test's own database that carries no pending_for field at all extends the wait, because the own-hold rule is judged on an age and a warning carrying none cannot end the wait",
			state{elapsed: 2 * Base, sinceNewWarning: Extension / 2, warnings: 2, last: FromLine(stallWarningHeadNoPendingFor() + stallWarningHolder("psql", ownDatabase))},
			waitOn,
		},
		{
			"a record whose pending_for is no duration extends the wait for the same reason",
			state{elapsed: time.Second, warnings: 1, last: FromRecord(stallWarningRecord("pending_for", "a minute", "database", ownDatabase))},
			waitOn,
		},
		{
			"an own-database warning fails before the base bound and inside the extension window, so the own-hold rule is read before the two that would wait",
			state{elapsed: Base - time.Millisecond, sinceNewWarning: Extension / 2, warnings: 1, last: FromLine(stallWarningLine(ownDatabase, 2*OwnHoldBound))},
			ownHold,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			row.state.ownDatabase = ownDatabase
			if got := verdictOf(row.state); got != row.want {
				t.Errorf("verdictOf(%+v) = %s, want %s", row.state, got, row.want)
			}
		})
	}
}

// TestAWatchOfATestThatWaitsUntilTheDeadlineIsEndedBySilenceNowhere drives Watch.Failure past each
// bound: a horizon that never warned past Base, and one that stopped warning past Extension. A test
// that did not call UntilTheDeadline is failed at each, which is the control that this row reads a
// watch the bounds can end; one that called it, and its subtest, are failed at neither, but still at
// the drain before the run's deadline and on an own hold past OwnHoldBound, whether a warning names
// it or the wait's own read of the holder finds it, each naming what it waited for.
func TestAWatchOfATestThatWaitsUntilTheDeadlineIsEndedBySilenceNowhere(t *testing.T) {
	const ownDatabase = "dispatch_test_ab"
	const what = "an event of events"
	none := func() []Warning { return nil }
	// silentPast is a watch on a horizon that has said nothing for past each bound since it began.
	silentPast := func(t *testing.T) *Watch {
		watch := NewWatch(t, none, ownDatabase, nil)
		watch.began = time.Now().Add(-2 * max(Base, Extension))
		watch.drain = time.Time{}
		return watch
	}
	// stoppedPast is a watch whose horizon warned once of a foreign holder, its last warning
	// counted past Extension ago.
	stoppedPast := func(t *testing.T) *Watch {
		warned := []Warning{FromLine(stallWarningLine("other_service", time.Second))}
		watch := NewWatch(t, func() []Warning { return warned }, ownDatabase, nil)
		watch.began = time.Now().Add(-2 * max(Base, Extension))
		watch.rose = watch.began
		watch.current.warnings, watch.current.last = 1, warned[0]
		watch.drain = time.Time{}
		return watch
	}
	t.Run("a test that did not call it", func(t *testing.T) {
		if failure := silentPast(t).Failure(what); !strings.Contains(failure, "never said it was held back") {
			t.Errorf("a silent horizon past Base: Failure = %q, want the base bound's failure", failure)
		}
		if failure := stoppedPast(t).Failure(what); !strings.Contains(failure, "stopped saying it was held back") {
			t.Errorf("a horizon silent past Extension: Failure = %q, want the extension window's failure", failure)
		}
	})
	t.Run("a test that called it", func(t *testing.T) {
		UntilTheDeadline(t)
		for name, watch := range map[string]*Watch{"silent past Base": silentPast(t), "silent past Extension": stoppedPast(t)} {
			if failure := watch.Failure(what); failure != "" {
				t.Errorf("a horizon %s: Failure = %q, want the wait to go on", name, failure)
			}
		}
		t.Run("and its subtest", func(t *testing.T) {
			if failure := silentPast(t).Failure(what); failure != "" {
				t.Errorf("a subtest's silent horizon past Base: Failure = %q, want the wait to go on", failure)
			}
		})
		drained := silentPast(t)
		drained.drain = time.Now().Add(-time.Millisecond)
		if failure := drained.Failure(what); !strings.HasPrefix(failure, what+" did not arrive in the ") {
			t.Errorf("past the drain: Failure = %q, want the drain's failure naming %q", failure, what)
		}
		held := []Warning{FromLine(stallWarningLine(ownDatabase, 2*OwnHoldBound))}
		ownHeld := NewWatch(t, func() []Warning { return held }, ownDatabase, nil)
		ownHeld.before = 0
		ownHeld.drain = time.Time{}
		if failure := ownHeld.Failure(what); !strings.Contains(failure, "a client of the test's own database") {
			t.Errorf("an own hold past OwnHoldBound: Failure = %q, want the own-hold failure", failure)
		}
		// A horizon whose warnings have not come, while the wait's own read of the holder finds a
		// transaction of the test's own database that has stood twice OwnHoldBound.
		probed := NewWatch(t, none, ownDatabase, heldBy{database: ownDatabase, since: 2 * OwnHoldBound})
		probed.began = time.Now().Add(-2 * max(Base, Extension))
		probed.drain = time.Time{}
		if failure := probed.Failure(what); !strings.Contains(failure, "a client of the test's own database") {
			t.Errorf("an own holder the probe found past OwnHoldBound: Failure = %q, want the own-hold failure", failure)
		}
	})
}

// heldBy is a Querier whose holder read (readHolder) finds a transaction of database that began
// since ago, by the database's own clock.
type heldBy struct {
	database string
	since    time.Duration
}

func (h heldBy) QueryRow(context.Context, string, ...any) pgx.Row { return h }

// Scan fills readHolder's columns: kind, pid, started, state, application_name, database, query
// and clock_timestamp().
func (h heldBy) Scan(dest ...any) error {
	now := time.Now()
	started := now.Add(-h.since)
	*dest[0].(*string) = "backend"
	*dest[1].(*int32) = 4242
	*dest[2].(**time.Time) = &started
	*dest[3].(*string) = "idle in transaction"
	*dest[4].(*string) = "psql"
	*dest[5].(*string) = h.database
	*dest[6].(*string) = "begin; select 1"
	*dest[7].(*time.Time) = now
	return nil
}

// TestFromLineMatchesTheWholeDatabaseField holds the trap a field read exists to avoid: a prefix
// match would read a foreign hold as the test's own and fail the wait at once on a database the
// test does not own.
func TestFromLineMatchesTheWholeDatabaseField(t *testing.T) {
	const ownDatabase = "dispatch_test_ab"
	for _, row := range []struct {
		name     string
		warning  string
		database string
		want     bool
	}{
		{
			"a warning naming a longer database that opens with this one's name is not read as naming this one",
			stallWarningLine(ownDatabase+"cd", OwnHoldBound), ownDatabase, false,
		},
		{
			"a warning naming exactly this database is read as naming it",
			stallWarningLine(ownDatabase, OwnHoldBound), ownDatabase, true,
		},
		{
			"a line carrying no database= field at all names nothing",
			stallWarningHead(OwnHoldBound) + ` holder="none open now"`, ownDatabase, false,
		},
		{
			"a database= field at the line's end, with no space after it, is still read whole",
			stallWarningHead(OwnHoldBound) + " database=" + ownDatabase, ownDatabase, true,
		},
		{
			"a line carrying no database= field at all is not read as naming the empty database, which a field read that could not tell a missing field from an empty one would have it do",
			stallWarningHead(OwnHoldBound) + ` holder="none open now"`, "", false,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			warning := FromLine(row.warning)
			if got := warning.Named && warning.Database == row.database; got != row.want {
				t.Errorf("FromLine(%q) names %q: %t, want %t", row.warning, row.database, got, row.want)
			}
		})
	}
}

// TestFieldTakesAQuotedValueWhole holds the other traps of the field read both the own-hold rule
// and the own-hold failure's client naming go through. slog quotes a value holding a space, and a
// read that stopped at the first space would cut it. The field that meets one is
// application_name, because the client sets it - pgAdmin names its connections "pgAdmin 4 -
// <connection>" - and it is what a Warning's Client names the holding client by. That the client
// sets it is the second trap: the name it chooses can hold the text of another field, so the read
// must find the line's own fields rather than text the line carries, and it must tell a field from
// one whose name ends in the key asked for. The third is which values count as quoted at all: slog
// quotes with a double quote alone, so a read that took every shape a Go string literal has would
// read a value a client chose, opening with a backquote, as a raw literal running across the
// fields after it.
func TestFieldTakesAQuotedValueWhole(t *testing.T) {
	const ownDatabase = "dispatch_test_ab"
	const foreignDatabase = "dispatch_test_cd"
	// The client of the backquote row: an application_name opening with a backquote, which slog
	// writes unquoted because a backquote is in its safe set, and the query that client was
	// running, which carries the closing backquote and, after it, the text
	// database=<the test's own database>. slog quotes the query, since it holds spaces.
	const backquotedName = "`backfill"
	const queryClosingTheBackquote = `"set application_name = '` + "`" + ` database=` + ownDatabase + ` backfill'"`
	for _, row := range []struct {
		name        string // what the row proves
		warning     string // the whole stall-warning line the row reads
		key         string // the field the row asks the read for
		value       string // the value the read must answer
		found       bool   // whether the line carries that field at all
		application string // the application_name the line's holder set, which Client carries whole
		namesOwn    bool   // whether the line is read as naming the test's own database
	}{
		{
			name:        "a quoted application_name holding a space is read whole, so the own-hold failure names the client as it named itself rather than cutting it at its first space",
			warning:     stallWarningHead(OwnHoldBound) + stallWarningHolder(`"pgAdmin 4 - dispatch"`, ownDatabase),
			key:         "application_name",
			value:       "pgAdmin 4 - dispatch",
			found:       true,
			application: "pgAdmin 4 - dispatch",
			namesOwn:    true,
		},
		{
			name:        "a client chooses its own application_name, so a line whose application_name holds the text database=<the test's own database> is read as the hold in another database that its own database= field says it is: a field read that searched the line as text answered that text instead, and had the wait fail on a foreign hold as its own",
			warning:     stallWarningHead(OwnHoldBound) + stallWarningHolder(`"database=`+ownDatabase+` backfill"`, foreignDatabase),
			key:         "database",
			value:       foreignDatabase,
			found:       true,
			application: "database=" + ownDatabase + " backfill",
			namesOwn:    false,
		},
		{
			name:        "a key that is the tail of a longer key is not that key: a line carrying application_name and no name= field of its own carries no name= field at all",
			warning:     stallWarningHead(OwnHoldBound) + stallWarningHolder(`"pgAdmin 4 - dispatch"`, ownDatabase),
			key:         "name",
			value:       "",
			found:       false,
			application: "pgAdmin 4 - dispatch",
			namesOwn:    true,
		},
		{
			name:        "a value a third party chose can open a Go raw string literal: an application_name opening with a backquote is read as the token slog wrote, because a read that took every literal strconv.QuotedPrefix takes ran that value on to the closing backquote in the holder's query and read the line's database out of the query's text, which had a foreign hold read as the test's own",
			warning:     stallWarningHead(OwnHoldBound) + stallWarningHolderOf(backquotedName, foreignDatabase, queryClosingTheBackquote),
			key:         "database",
			value:       foreignDatabase,
			found:       true,
			application: backquotedName,
			namesOwn:    false,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			got, found := Field(row.warning, row.key)
			if got != row.value || found != row.found {
				t.Errorf("Field(%q, %q) = %q, %t; want %q, %t", row.warning, row.key, got, found, row.value, row.found)
			}
			warning := FromLine(row.warning)
			if !strings.Contains(warning.Client, row.application) {
				t.Errorf("FromLine(%q).Client = %q, want it to name the client %q", row.warning, warning.Client, row.application)
			}
			if names := warning.Named && warning.Database == ownDatabase; names != row.namesOwn {
				t.Errorf("FromLine(%q) names %q: %t, want %t", row.warning, ownDatabase, names, row.namesOwn)
			}
		})
	}
}

// TestFromRecordReadsWhatWarnStallLogged reads a record's own attributes, typed as warnStall
// logged them, and renders it as the deployment's text handler would for a failure to print.
func TestFromRecordReadsWhatWarnStallLogged(t *testing.T) {
	warning := FromRecord(heldRecord(7*time.Second, "dispatch_test_ab"))
	if warning.PendingFor != 7*time.Second || !warning.Named || warning.Database != "dispatch_test_ab" {
		t.Errorf("FromRecord = %+v, want pending_for 7s in database dispatch_test_ab", warning)
	}
	if warning.Client != "application_name psql, pid 86" {
		t.Errorf("FromRecord(...).Client = %q, want psql's pid 86", warning.Client)
	}
	if !strings.Contains(warning.Text, `msg="`+events.StallWarning+`"`) || !strings.Contains(warning.Text, "database=dispatch_test_ab") {
		t.Errorf("FromRecord(...).Text = %q, want the warning as the text handler writes it", warning.Text)
	}
	if none := FromRecord(stallWarningRecord("pending_for", time.Minute, "holder", "none open now")); none.Named || none.Client != "application_name (none set), pid (none named)" {
		t.Errorf("FromRecord of a warning that found no holder = %+v, want no database and no client named", none)
	}
}

// TestListenKeepsEachStallWarningAndPassesEveryRecordOn listens while the default logger already
// belongs to a test that captures its own records: the stall warning is kept, every record still
// reaches the test's own handler, and once stopped nothing more is kept and the test's handler is
// the default again.
func TestListenKeepsEachStallWarningAndPassesEveryRecordOn(t *testing.T) {
	captured := &logtest.Recorder{}
	logtest.SetDefault(t, captured)
	warnings, stop := Listen(t)
	slog.Info("dispatch: request")
	slog.Warn(events.StallWarning, "pending_for", time.Second, "database", "postgres")
	if kept := warnings(); len(kept) != 1 || kept[0].PendingFor != time.Second || kept[0].Database != "postgres" {
		t.Fatalf("Listen kept %+v, want the one stall warning", kept)
	}
	stop()
	slog.Warn(events.StallWarning, "pending_for", 2*time.Second)
	if kept := warnings(); len(kept) != 1 {
		t.Fatalf("Listen kept %d warnings after it stopped, want the 1 from before", len(kept))
	}
	var messages []string
	for _, record := range captured.Records() {
		messages = append(messages, record.Message)
	}
	if want := []string{"dispatch: request", events.StallWarning, events.StallWarning}; strings.Join(messages, "|") != strings.Join(want, "|") {
		t.Fatalf("the test's own handler got %q, want %q", messages, want)
	}
}
