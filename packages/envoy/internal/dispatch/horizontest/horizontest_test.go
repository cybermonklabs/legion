package horizontest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

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

// heldRead is the holder a watch reads itself (readHolder) when the instance's oldest transaction
// holding an id is one of database's that began since ago.
func heldRead(t *testing.T, database string, since time.Duration) Warning {
	t.Helper()
	held, found, err := readHolder(heldBy{database: database, since: since})
	if err != nil || !found {
		t.Fatalf("readHolder of a holder in %s that began %v ago = %+v, %t, %v; want that holder", database, since, held, found, err)
	}
	return held
}

// TestVerdictOfKeysTheWaitOnTheStallWarning drives every rule of the wait's judgement, with the
// boundary either side where a rule has one, on warnings read both ways a wait reads them: as a
// Dispatch process's output line and as a holder the wait read itself (readHolder), which is all a
// wait in the test's own process reads. It needs no server, no Postgres and no network: the
// judgement is a function of its state alone.
//
// Every duration is written in terms of Base, Extension and OwnHoldBound rather than as seconds,
// so that a later change of any of the three moves the rows with it instead of leaving them to
// test a boundary that has moved.
func TestVerdictOfKeysTheWaitOnTheStallWarning(t *testing.T) {
	t.Parallel()
	const ownDatabase = "dispatch_test_ab"
	// A foreign holder: an event write of another database, which a horizon reading that database
	// reports, and which is no defect of this test's, however long it has stood.
	foreign := FromLine(stallWarningLine("legion_daemon", 2*OwnHoldBound))
	// warnStall's other two arms carry no database= field at all.
	diagnosisFailed := FromLine(stallWarningHead(2*OwnHoldBound) + ` diagnosis_error="timeout: context deadline exceeded"`)
	noneOpen := FromLine(stallWarningHead(2*OwnHoldBound) + ` holder="none open now"`)
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
			"a foreign holder the wait read itself is waited on inside the extension window",
			state{elapsed: time.Minute, sinceNewWarning: Extension - time.Millisecond, warnings: 9, last: heldRead(t, "postgres", time.Minute)},
			waitOn,
		},
		{
			"a foreign holder the wait read itself and has not read again for the extension window is a stopped signal, the verdict at which the wait reads the holder again",
			state{elapsed: time.Minute, sinceNewWarning: Extension, warnings: 9, last: heldRead(t, "postgres", time.Minute)},
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
			"a holder of the test's own database the wait read itself, whose transaction began a moment ago, is no own hold either",
			state{elapsed: 4 * Base, sinceNewWarning: Extension / 2, warnings: 5, last: heldRead(t, ownDatabase, 100*time.Millisecond)},
			waitOn,
		},
		{
			"a transaction of the test's own database that has itself stood the bound is an own hold, whatever came before it",
			state{elapsed: 4 * Base, sinceNewWarning: Extension / 2, warnings: 5, last: FromLine(stallWarningLineHeldFor(ownDatabase, 2*OwnHoldBound, OwnHoldBound))},
			ownHold,
		},
		{
			"so is one the wait read itself",
			state{elapsed: 4 * Base, sinceNewWarning: Extension / 2, warnings: 5, last: heldRead(t, ownDatabase, OwnHoldBound+time.Second)},
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
			"an own-database holder the wait read itself inside its bound extends the wait",
			state{elapsed: time.Second, warnings: 1, last: heldRead(t, ownDatabase, OwnHoldBound-time.Millisecond)},
			waitOn,
		},
		{
			"an own-database holder the wait read itself at its bound fails",
			state{elapsed: time.Second, warnings: 1, last: heldRead(t, ownDatabase, OwnHoldBound)},
			ownHold,
		},
		{
			"a holder the wait read itself in another database whose name the own one prefixes is a foreign hold",
			state{elapsed: time.Second, warnings: 1, last: heldRead(t, ownDatabase+"cd", time.Minute)},
			waitOn,
		},
		{
			"a warning whose diagnosis failed names no database at all and still extends the wait, because the horizon is saying it is held",
			state{elapsed: 2 * Base, sinceNewWarning: Extension / 2, warnings: 2, last: diagnosisFailed},
			waitOn,
		},
		{
			"a warning that found no holder still open names no database either and extends the wait for the same reason",
			state{elapsed: 2 * Base, sinceNewWarning: Extension / 2, warnings: 2, last: noneOpen},
			waitOn,
		},
		{
			"a warning naming the test's own database that carries no pending_for field at all extends the wait, because the own-hold rule is judged on an age and a warning carrying none cannot end the wait",
			state{elapsed: 2 * Base, sinceNewWarning: Extension / 2, warnings: 2, last: FromLine(stallWarningHeadNoPendingFor() + stallWarningHolder("psql", ownDatabase))},
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

// TestAWatchInTheTestsOwnProcessIsEndedBySilenceNowhere drives both watches' Failure past each
// bound: a horizon that never warned past Base, and one whose newest warning or holder came past
// Extension ago. A watch on a Dispatch process (NewWatch) is failed at each, which is the control
// that this row reads a watch the bounds can end. A watch in the test's own process (the
// InProcessWatch NewWatchInProcess makes, here over a stand-in pool) is failed at neither,
// whatever its reads of the holder find; it reads the holder no more often than every Poll, and not
// again within Extension of a holder it found; it is failed at the drain before the run's
// deadline, naming what it waited for and the newest holder it read, and on a holder of the test's
// own database past OwnHoldBound that its own read found.
func TestAWatchInTheTestsOwnProcessIsEndedBySilenceNowhere(t *testing.T) {
	t.Parallel()
	const ownDatabase = "dispatch_test_ab"
	const what = "an event of events"
	none := func() []Warning { return nil }
	// pastBoth moves watch's clocks so that it began, and last counted a warning or a holder,
	// twice the longer of Base and Extension ago, and gives it no drain.
	pastBoth := func(watch *Watch) *Watch {
		watch.began = time.Now().Add(-2 * max(Base, Extension))
		watch.rose = watch.began
		watch.drain = time.Time{}
		return watch
	}
	// inProcess is a watch in the test's own process reading the holder through probe, begun twice
	// the longer of Base and Extension ago when past is true, and given no drain.
	inProcess := func(t *testing.T, probe Querier, past bool) *InProcessWatch {
		watch := newInProcessWatch(t, ownDatabase, probe)
		if past {
			watch.began = time.Now().Add(-2 * max(Base, Extension))
		}
		watch.drain = time.Time{}
		return watch
	}
	refused := failing{errors.New("connection refused")}

	t.Run("a watch on a Dispatch process", func(t *testing.T) {
		if failure := pastBoth(NewWatch(t, none, ownDatabase, nil)).Failure(what); !strings.Contains(failure, "never said it was held back") {
			t.Errorf("a silent horizon past Base: Failure = %q, want the base bound's failure", failure)
		}
		if failure := pastBoth(NewWatch(t, none, ownDatabase, noHolder{})).Failure(what); !strings.Contains(failure, "no event write of the database is open now") {
			t.Errorf("a silent horizon past Base whose holder the watch read and found none: Failure = %q, want the base bound's failure naming that read", failure)
		}
		warned := []Warning{FromLine(stallWarningLine("other_service", time.Second))}
		stopped := pastBoth(NewWatch(t, func() []Warning { return warned }, ownDatabase, nil))
		stopped.current.warnings, stopped.current.last = 1, warned[0]
		if failure := stopped.Failure(what); !strings.Contains(failure, "stopped saying it was held back") {
			t.Errorf("a horizon silent past Extension: Failure = %q, want the extension window's failure", failure)
		}
	})

	t.Run("a watch in the test's own process", func(t *testing.T) {
		for name, probe := range map[string]Querier{
			"whose read finds no holder":        noHolder{},
			"whose read finds a foreign holder": heldBy{database: "other_service", since: time.Minute},
			"whose read fails":                  refused,
		} {
			if failure := inProcess(t, probe, true).Failure(what); failure != "" {
				t.Errorf("past Base, %s: Failure = %q, want the wait to go on", name, failure)
			}
		}
		// A foreign holder read once, then past Extension since: the watch reads it again and
		// goes on.
		reads := 0
		foreign := inProcess(t, readsCounted{next: heldBy{database: "other_service", since: time.Minute}, reads: &reads}, true)
		for read := 1; read <= 2; read++ {
			if failure := foreign.Failure(what); failure != "" {
				t.Errorf("read %d of a foreign holder: Failure = %q, want the wait to go on", read, failure)
			}
			foreign.heldAt, foreign.read = foreign.heldAt.Add(-2*Extension), foreign.read.Add(-2*Extension)
		}
		if reads != 2 {
			t.Errorf("the watch read the holder %d times over two Failures past Extension apart, want 2", reads)
		}
	})

	t.Run("its reads of the holder", func(t *testing.T) {
		reads := 0
		counted := readsCounted{next: noHolder{}, reads: &reads}
		if failure := inProcess(t, counted, false).Failure(what); failure != "" || reads != 0 {
			t.Errorf("inside Base: Failure = %q after %d reads of the holder, want the wait to go on with none", failure, reads)
		}
		paced := inProcess(t, counted, true)
		for range 3 {
			paced.Failure(what)
		}
		if reads != 1 {
			t.Errorf("three Failures inside one Poll past Base read the holder %d times, want once", reads)
		}
		paced.read = paced.read.Add(-Poll)
		paced.Failure(what)
		if reads != 2 {
			t.Errorf("a Failure a Poll after the last read made %d reads in all, want 2", reads)
		}
		// A read that found a holder puts the next read Extension away, though Poll has passed.
		heldReads := 0
		held := inProcess(t, readsCounted{next: heldBy{database: "other_service", since: time.Minute}, reads: &heldReads}, true)
		held.Failure(what)
		held.read = held.read.Add(-Poll)
		held.Failure(what)
		if heldReads != 1 {
			t.Errorf("a Failure a Poll after a read that found a holder, within Extension of it, made %d reads in all, want 1", heldReads)
		}
	})

	t.Run("the drain", func(t *testing.T) {
		held := inProcess(t, heldBy{database: "other_service", since: time.Minute}, true)
		held.Failure(what)
		// The read that found the holder was an hour before the drain: the failure says so, and
		// gives the holder's own age at that read, not the hour.
		held.heldAt = held.heldAt.Add(-time.Hour)
		held.drain = time.Now().Add(-time.Millisecond)
		failure := held.Failure(what)
		if !strings.HasPrefix(failure, what+" did not arrive in the ") || !strings.Contains(failure, "database=other_service") {
			t.Errorf("past the drain after a read found a foreign holder: Failure = %q, want the drain's failure naming %q and that holder", failure, what)
		}
		if !strings.Contains(failure, "was read 1h0m0") || !strings.Contains(failure, "held its transaction id for 1m0s") {
			t.Errorf("past the drain an hour after the read that found a holder of a minute: Failure = %q, want it to say the read was an hour before and the holder had stood a minute", failure)
		}
		unread := inProcess(t, refused, true)
		unread.Failure(what)
		unread.drain = time.Now().Add(-time.Millisecond)
		if failure := unread.Failure(what); !strings.HasPrefix(failure, what+" did not arrive in the ") || !strings.Contains(failure, "was none") || !strings.Contains(failure, "connection refused") {
			t.Errorf("past the drain after a read that failed: Failure = %q, want the drain's failure naming %q, no holder and the read's error", failure, what)
		}
	})

	t.Run("an own hold", func(t *testing.T) {
		// The wait's own read of the holder finds a transaction of the test's own database that
		// has stood twice OwnHoldBound.
		if failure := inProcess(t, heldBy{database: ownDatabase, since: 2 * OwnHoldBound}, true).Failure(what); !strings.Contains(failure, "a client of the test's own database") || !strings.Contains(failure, "pid 4242") {
			t.Errorf("an own holder the read found past OwnHoldBound: Failure = %q, want the own-hold failure naming the client", failure)
		}
		if failure := inProcess(t, heldBy{database: ownDatabase, since: time.Second}, true).Failure(what); failure != "" {
			t.Errorf("an own holder the read found inside OwnHoldBound: Failure = %q, want the wait to go on", failure)
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

// noHolder is a Querier whose holder read finds no event write of the database open.
type noHolder struct{}

func (noHolder) QueryRow(context.Context, string, ...any) pgx.Row { return failedRow{pgx.ErrNoRows} }

// failing is a Querier whose holder read fails with err.
type failing struct{ err error }

func (f failing) QueryRow(context.Context, string, ...any) pgx.Row { return failedRow{f.err} }

// readsCounted is a Querier that counts the holder reads made through it and answers each as next
// does.
type readsCounted struct {
	next  Querier
	reads *int
}

func (r readsCounted) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	*r.reads++
	return r.next.QueryRow(ctx, sql, args...)
}

// TestReadHolderReadsWhatTheDiagnosisQueryAnswers reads the holder a watch finds itself as the
// warning the wait judges: naming its database and its client by application_name and pid, and
// holding for its own age on the database's clock. A read that answers no row is no holder, and
// a read that fails is an error.
func TestReadHolderReadsWhatTheDiagnosisQueryAnswers(t *testing.T) {
	t.Parallel()
	held, found, err := readHolder(heldBy{database: "dispatch_test_ab", since: 7 * time.Second})
	if err != nil || !found {
		t.Fatalf("readHolder = %+v, %t, %v; want the holder", held, found, err)
	}
	if !held.Named || held.Database != "dispatch_test_ab" || held.heldFor() != 7*time.Second {
		t.Errorf("readHolder = %+v, want a holder in database dispatch_test_ab held for 7s", held)
	}
	if held.Client != "application_name psql, pid 4242" {
		t.Errorf("readHolder(...).Client = %q, want psql's pid 4242", held.Client)
	}
	if !strings.Contains(held.Text, "database=dispatch_test_ab") || !strings.Contains(held.Text, `query="begin; select 1"`) {
		t.Errorf("readHolder(...).Text = %q, want the holder's database and query", held.Text)
	}
	if held, found, err := readHolder(noHolder{}); found || err != nil {
		t.Errorf("readHolder of no row = %+v, %t, %v; want no holder and no error", held, found, err)
	}
	if _, found, err := readHolder(failing{errors.New("connection refused")}); found || err == nil {
		t.Errorf("readHolder of a failed read = found %t, error %v; want the error", found, err)
	}
}

// TestFromLineMatchesTheWholeDatabaseField holds the trap a field read exists to avoid: a prefix
// match would read a foreign hold as the test's own and fail the wait at once on a database the
// test does not own.
func TestFromLineMatchesTheWholeDatabaseField(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
