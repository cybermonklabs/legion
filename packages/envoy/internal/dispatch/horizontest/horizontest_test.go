package horizontest

import (
	"fmt"
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// processDefault is the default logger as the process starts, before any test swapped it: slog's
// own default handler, which writes through the log package. A test whose server logs to it is the
// ordinary case, in api's tests above all.
var processDefault = slog.Default()

// TestALoggerOverSlogsOwnDefaultHandlerPassesEveryRecordOn logs through Logger while the default
// logger is slog's own, as it is for a test that captures no log: the stall warning is kept for the
// database's Listen, and the warning, another slog record and a log.Print each reach the log
// package's writer once, from a goroutine, with no lock of the log package taken twice.
func TestALoggerOverSlogsOwnDefaultHandlerPassesEveryRecordOn(t *testing.T) {
	written := &logtest.Buffer{}
	previous, output, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(processDefault)
	log.SetOutput(written)
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(output)
		log.SetFlags(flags)
	})
	const database = "dispatch_test_logger_over_slogs_own"
	warnings, stop := Listen(t, database)
	logger := Logger(database)
	logged := make(chan struct{})
	go func() {
		defer close(logged)
		logger.Warn(events.StallWarning, "pending_for", time.Second, "database", "postgres")
		logger.Info("dispatch: request")
		log.Print("a log package line")
	}()
	// A handler and the log package that deadlocked would never close logged.
	testwait.Receive(t, "logging through Logger over slog's own default handler to return", logged)
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

// TestEachDatabasesListenReadsOnlyTheWarningsOfItsOwnLogger runs two tests beside each other, each
// on a database of its own, as api's tests run: the one whose horizon logs a stall warning reads it,
// and the one whose horizon logs none reads none, whatever the other logged. A keeper the two
// shared, as a reader of the whole process's default logger was, would hand the quiet one the
// other's warning.
func TestEachDatabasesListenReadsOnlyTheWarningsOfItsOwnLogger(t *testing.T) {
	// listening closes once the quiet test listens, so the warning the other logs falls inside the
	// quiet test's Listen whatever order the two run in.
	listening, stalled := make(chan struct{}), make(chan struct{})
	t.Run("a horizon that stalls", func(t *testing.T) {
		t.Parallel()
		const database = "dispatch_test_listen_stalls"
		warnings, _ := Listen(t, database)
		testwait.Receive(t, "the other database's test to listen", listening)
		Logger(database).Warn(events.StallWarning, "pending_for", time.Second, "database", database)
		close(stalled)
		if kept := warnings(); len(kept) != 1 || kept[0].Database != database {
			t.Fatalf("Listen on %s read %+v, want the one stall warning its own horizon logged", database, kept)
		}
	})
	t.Run("a horizon that does not", func(t *testing.T) {
		t.Parallel()
		const database = "dispatch_test_listen_quiet"
		warnings, _ := Listen(t, database)
		close(listening)
		testwait.Receive(t, "the other database's horizon to log its stall warning", stalled)
		Logger(database).Info("dispatch: request")
		if kept := warnings(); len(kept) != 0 {
			t.Fatalf("Listen on %s read %+v, want none: its own horizon logged no stall warning", database, kept)
		}
	})
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
// fixtureHolderAge before (stallWarningHolderOf), unless a row dates its holder otherwise
// (stallWarningLineHeldFor).
var stallWarningAt = time.Date(2026, 11, 10, 2, 14, 5, 113_000_000, time.UTC)

// fixtureHolderAge is how long before a fixture warning its holder's transaction began, and the
// pending_for most fixtures carry: the age of a hold the fixtures stage, read off the warning
// alone, and no bound of any wait.
const fixtureHolderAge = 10 * time.Second

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
	return stallWarningHolderSince(application, database, query, stallWarningAt.Add(-fixtureHolderAge))
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
// having begun fixtureHolderAge before the record.
func heldRecord(pendingFor time.Duration, database string) slog.Record {
	return heldRecordFor(pendingFor, fixtureHolderAge, database)
}

// heldRecordFor is heldRecord for a holder whose own transaction had stood held when it was named.
func heldRecordFor(pendingFor, held time.Duration, database string) slog.Record {
	return stallWarningRecord("pending_for", pendingFor, "observed_id", int64(2), "safe_horizon", int64(0),
		"holder", "backend", "pid", int32(86), "xact_start", time.Now().Add(-held), "state", "active",
		"application_name", "psql", "database", database, "query", "select 1")
}

// TestTheAccountNamesAHolderInTheTestsOwnDatabase drives what the account of a wait that gave up
// (Watch.String) says of the holder the newest warning names, on warnings read both ways a test
// reads them: as a Dispatch process's output line and as a record a horizon of the test's own
// process logged. A holder in the test's own database is named as one, with its client and how
// long its own transaction had held the horizon back (heldFor), since a hold there can be the
// defect of dispatch://INFRA-636 or any other client's transaction; every other warning is named
// by its text alone. It needs no server, no Postgres and no network: the account is a function of
// the warnings alone. held is checked on the lines, which date the warning and its holder
// exactly; a record dates itself as it is made, so held is not checked there (-1).
func TestTheAccountNamesAHolderInTheTestsOwnDatabase(t *testing.T) {
	const ownDatabase = "dispatch_test_ab"
	for _, row := range []struct {
		name    string
		warning Warning
		own     bool
		held    time.Duration
	}{
		{
			"a foreign holder, an event write of another database that a horizon reading that database reports, is named by its warning alone: it is no client of the test's own database",
			FromLine(stallWarningLine("legion_daemon", 2*fixtureHolderAge)),
			false, fixtureHolderAge,
		},
		{
			"so is a foreign hold a horizon of the test's own process reports",
			FromRecord(heldRecord(time.Minute, "postgres")),
			false, -1,
		},
		{
			"a warning naming the test's own database is named as a hold there, so a hold holding up its live readers is in the account of the wait it held up (dispatch://INFRA-636)",
			FromLine(stallWarningLine(ownDatabase, 2*fixtureHolderAge)),
			true, fixtureHolderAge,
		},
		{
			"a hold there is dated by its own transaction, though pending_for is older: a foreign holder released since carried the oldest observation's age there",
			FromLine(stallWarningLineHeldFor(ownDatabase, 2*fixtureHolderAge, 100*time.Millisecond)),
			true, 100 * time.Millisecond,
		},
		{
			"and by pending_for where that is younger than the transaction",
			FromLine(stallWarningLine(ownDatabase, time.Second)),
			true, time.Second,
		},
		{
			"so is one on a record of the test's own process",
			FromRecord(heldRecordFor(2*fixtureHolderAge, 100*time.Millisecond, ownDatabase)),
			true, -1,
		},
		{
			"a warning naming the test's own database that dates no holder (xact_start) is named as a hold there that held for zero",
			FromLine(stallWarningHead(2*fixtureHolderAge) + ` holder=backend pid=1188 state=active application_name=psql database=` + ownDatabase),
			true, 0,
		},
		{
			"a record naming another database whose name the own one prefixes is a foreign hold",
			FromRecord(heldRecord(time.Minute, ownDatabase+"cd")),
			false, -1,
		},
		{
			"a warning whose diagnosis failed names no database at all, so it names no hold in the test's own",
			FromLine(stallWarningHead(2*fixtureHolderAge) + ` diagnosis_error="timeout: context deadline exceeded"`),
			false, 0,
		},
		{
			"a warning that found no holder still open names no database either",
			FromLine(stallWarningHead(2*fixtureHolderAge) + ` holder="none open now"`),
			false, 0,
		},
		{
			"a warning naming the test's own database that carries no pending_for field at all is named as a hold there that held for zero, the age it cannot read",
			FromLine(stallWarningHeadNoPendingFor() + stallWarningHolder("psql", ownDatabase)),
			true, 0,
		},
		{
			"a record whose pending_for is no duration holds for zero for the same reason",
			FromRecord(stallWarningRecord("pending_for", "a minute", "database", ownDatabase)),
			true, 0,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := ownDatabaseHold(row.warning, ownDatabase); got != row.own {
				t.Errorf("ownDatabaseHold(%+v, %q) = %t, want %t", row.warning, ownDatabase, got, row.own)
			}
			if got := row.warning.heldFor(); row.held >= 0 && got != row.held {
				t.Errorf("heldFor(%+v) = %v, want %v", row.warning, got, row.held)
			}
			var logged []Warning
			watch := NewWatch(func() []Warning { return logged }, ownDatabase)
			logged = append(logged, row.warning)
			account := watch.String()
			named := "a client of the test's own database " + ownDatabase
			if strings.Contains(account, named) != row.own {
				t.Errorf("the account %q names a hold in the test's own database: %t, want %t", account, !row.own, row.own)
			}
			if row.own {
				for _, want := range []string{row.warning.Client, "held it back for " + row.warning.heldFor().String()} {
					if !strings.Contains(account, want) {
						t.Errorf("the account %q does not name %q", account, want)
					}
				}
			}
			if !strings.Contains(account, row.warning.Text) {
				t.Errorf("the account %q does not carry the newest warning %q", account, row.warning.Text)
			}
		})
	}
}

// TestAWatchsAccountNamesTheNewestWarningSinceItBegan: a warning logged before the watch began
// belongs to a hold before the wait, perhaps one long over, so the account names nothing of it;
// the newest warning since is the one the account names, as a hold in the test's own database
// with its client and its warning when it is one, and as its warning alone when it is not.
func TestAWatchsAccountNamesTheNewestWarningSinceItBegan(t *testing.T) {
	const ownDatabase = "dispatch_test_ab"
	own := FromLine(stallWarningLine(ownDatabase, 2*fixtureHolderAge))
	foreign := FromLine(stallWarningLine("postgres", 2*fixtureHolderAge))
	named := "a client of the test's own database " + ownDatabase
	logged := []Warning{own}
	watch := NewWatch(func() []Warning { return logged }, ownDatabase)
	if account := watch.String(); strings.Contains(account, named) || strings.Contains(account, own.Text) {
		t.Fatalf("the account of a watch whose only warning came before it began names that warning: %s", account)
	}
	logged = append(logged, foreign)
	if account := watch.String(); strings.Contains(account, named) {
		t.Fatalf("the account of a watch whose newest warning names a foreign holder names a hold in the test's own database: %s", account)
	}
	logged = append(logged, own)
	account := watch.String()
	for _, want := range []string{named, own.Client, own.Text} {
		if !strings.Contains(account, want) {
			t.Errorf("the account %q of an own hold does not name %q", account, want)
		}
	}
	logged = append(logged, foreign)
	if account := watch.String(); strings.Contains(account, named) {
		t.Fatalf("the account of a watch whose newest warning names a foreign holder again names the own hold before it: %s", account)
	}
}

// TestAWatchsAccountIsWhatTheHorizonSaid is the last read a wait that gave up at the test binary's
// deadline prints: the count of warnings since the watch began and the newest of them, which names
// the holder from the horizon's own diagnosis, or a count of none when the horizon never said it
// was held.
func TestAWatchsAccountIsWhatTheHorizonSaid(t *testing.T) {
	before := FromLine(stallWarningLine("legion_daemon", fixtureHolderAge))
	newest := FromLine(stallWarningLine("postgres", 2*fixtureHolderAge))
	for _, row := range []struct {
		name   string
		logged []Warning
		want   []string
	}{
		{"a horizon that warned is named by its newest warning", []Warning{newest},
			[]string{"logged 1 stall warnings", "the newest: " + newest.Text, "database=postgres"}},
		{"a horizon that never warned while the test waited says so", nil,
			[]string{"logged 0 stall warnings"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			logged := []Warning{before}
			watch := NewWatch(func() []Warning { return logged }, "dispatch_test_ab")
			logged = append(logged, row.logged...)
			account := fmt.Sprintf("%+v", watch)
			for _, want := range row.want {
				if !strings.Contains(account, want) {
					t.Errorf("the account %q does not hold %q", account, want)
				}
			}
			if strings.Contains(account, before.Text) {
				t.Errorf("the account %q names the warning logged before the watch began", account)
			}
		})
	}
}

// TestSilenceEndsNoWaitOnTheHorizon holds what main's UntilTheDeadline row
// (TestAWatchOfATestThatWaitsUntilTheDeadlineIsEndedBySilenceNowhere) held, for every wait on the
// horizon here: a horizon that says nothing ends no wait. It holds that on a horizon that never
// warned and on one that warned once of a foreign holder after the watch began and then said
// nothing.
//
// Wait runs in a synctest bubble, through the t a wait takes there (testwait.Bubble), so its asks
// at the shared wait's pace pass on the bubble's clock and cost no time: its first ask reads the
// once-warned horizon's warning, and every ask after it reads the horizon's silence. The result is
// sent only once the bubble has idled silenceSpan past the watch's start (testwait.Idle), so Wait
// asks its condition and reads the watch at every pace through that much silence, timed from the
// watch's start and from the warning it read. A silence bound shorter than silenceSpan in Wait or
// Poll fails the row by name: main's 3 s Base and Extension among them, which main's poll loop
// judged at every ask (verdictOf) and a merge of main could bring back into Poll. A Wait that never
// returned the result fails by name too: the bubble's t names what was left of the row's deadline
// as the bubble began, past the bubble's start, so Wait gives up the shared wait's margin before
// that on the bubble's clock, which its asks move at the shared wait's pace.
//
// Wait runs on the test goroutine, so Idle and the send run on a goroutine of their own; Idle's why
// is never empty, so it fails nothing there. The test goroutine waits for that goroutine to end
// however the row ends: a Wait that failed would otherwise leave it asleep in the bubble once the
// test goroutine had ended, which synctest.Test ends in a panic rather than in the row's failure.
func TestSilenceEndsNoWaitOnTheHorizon(t *testing.T) {
	// silenceSpan is how long the horizon says nothing before the result is sent: past main's
	// Base and Extension (3 s each, verdictOf), the silence bounds a merge of main could bring
	// back. It is the span of the absence this row proves (testwait.Idle), not a wait's bound.
	const silenceSpan = fixtureHolderAge
	for _, row := range []struct {
		name   string
		logged []Warning
	}{
		{"a horizon that never warned", nil},
		{"a horizon that warned once of a foreign holder and then said nothing", []Warning{FromLine(stallWarningLine("other_service", time.Second))}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			testwait.Bubble(t, func(t testing.TB) {
				arrived, ended := make(chan string, 1), make(chan struct{})
				go func() {
					defer close(ended)
					testwait.Idle(t, silenceSpan, fmt.Sprintf("main's silence verdict, Base and Extension (3 s each, verdictOf), "+
						"which a merge of main could bring back into Poll: the result comes %v into the silence, past it", silenceSpan))
					arrived <- "the result"
				}()
				defer testwait.Receive(t, "the goroutine that sends the result to end", ended)
				what := fmt.Sprintf("a result sent once the horizon had said nothing for %v", silenceSpan)
				if got := Wait(t, what, arrived, saying(row.logged), "dispatch_test_ab", nil); got != "the result" {
					t.Fatalf("Wait answered %q; want the result", got)
				}
			})
		})
	}
}

// saying is a horizon's warnings as a watch begun on it reads them: none at its first read, which
// the watch makes as it begins (NewWatch), and logged at every read after it, so logged was said
// after the watch began.
func saying(logged []Warning) func() []Warning {
	reads := 0
	return func() []Warning {
		reads++
		if reads == 1 {
			return nil
		}
		return logged
	}
}

// TestFromLineMatchesTheWholeDatabaseField holds the trap a field read exists to avoid: a prefix
// match would read a foreign hold as the test's own, and the account of a wait that gave up would
// name a client of a database the test does not own as its own.
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
			stallWarningLine(ownDatabase+"cd", fixtureHolderAge), ownDatabase, false,
		},
		{
			"a warning naming exactly this database is read as naming it",
			stallWarningLine(ownDatabase, fixtureHolderAge), ownDatabase, true,
		},
		{
			"a line carrying no database= field at all names nothing",
			stallWarningHead(fixtureHolderAge) + ` holder="none open now"`, ownDatabase, false,
		},
		{
			"a database= field at the line's end, with no space after it, is still read whole",
			stallWarningHead(fixtureHolderAge) + " database=" + ownDatabase, ownDatabase, true,
		},
		{
			"a line carrying no database= field at all is not read as naming the empty database, which a field read that could not tell a missing field from an empty one would have it do",
			stallWarningHead(fixtureHolderAge) + ` holder="none open now"`, "", false,
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

// TestFieldTakesAQuotedValueWhole holds the other traps of the field read that the account's
// naming of a hold in the test's own database, and of its client, goes through. slog quotes a
// value holding a space, and a read that stopped at the first space would cut it. The field that
// meets one is
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
			name:        "a quoted application_name holding a space is read whole, so the account of an own hold names the client as it named itself rather than cutting it at its first space",
			warning:     stallWarningHead(fixtureHolderAge) + stallWarningHolder(`"pgAdmin 4 - dispatch"`, ownDatabase),
			key:         "application_name",
			value:       "pgAdmin 4 - dispatch",
			found:       true,
			application: "pgAdmin 4 - dispatch",
			namesOwn:    true,
		},
		{
			name:        "a client chooses its own application_name, so a line whose application_name holds the text database=<the test's own database> is read as the hold in another database that its own database= field says it is: a field read that searched the line as text answered that text instead, and had the account name a foreign hold as the test's own",
			warning:     stallWarningHead(fixtureHolderAge) + stallWarningHolder(`"database=`+ownDatabase+` backfill"`, foreignDatabase),
			key:         "database",
			value:       foreignDatabase,
			found:       true,
			application: "database=" + ownDatabase + " backfill",
			namesOwn:    false,
		},
		{
			name:        "a key that is the tail of a longer key is not that key: a line carrying application_name and no name= field of its own carries no name= field at all",
			warning:     stallWarningHead(fixtureHolderAge) + stallWarningHolder(`"pgAdmin 4 - dispatch"`, ownDatabase),
			key:         "name",
			value:       "",
			found:       false,
			application: "pgAdmin 4 - dispatch",
			namesOwn:    true,
		},
		{
			name:        "a value a third party chose can open a Go raw string literal: an application_name opening with a backquote is read as the token slog wrote, because a read that took every literal strconv.QuotedPrefix takes ran that value on to the closing backquote in the holder's query and read the line's database out of the query's text, which had a foreign hold read as the test's own",
			warning:     stallWarningHead(fixtureHolderAge) + stallWarningHolderOf(backquotedName, foreignDatabase, queryClosingTheBackquote),
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

// TestALoggerKeepsEachStallWarningAndPassesEveryRecordOn logs through Logger while the default
// logger belongs to a test that captures its own records: the stall warning is kept, every record
// still reaches the test's own handler, and once the Listen has stopped it reads nothing more.
func TestALoggerKeepsEachStallWarningAndPassesEveryRecordOn(t *testing.T) {
	captured := &logtest.Recorder{}
	logtest.SetDefault(t, captured)
	const database = "dispatch_test_logger_keeps"
	warnings, stop := Listen(t, database)
	logger := Logger(database)
	logger.Info("dispatch: request")
	logger.Warn(events.StallWarning, "pending_for", time.Second, "database", "postgres")
	if kept := warnings(); len(kept) != 1 || kept[0].PendingFor != time.Second || kept[0].Database != "postgres" {
		t.Fatalf("Listen kept %+v, want the one stall warning", kept)
	}
	stop()
	logger.Warn(events.StallWarning, "pending_for", 2*time.Second)
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
