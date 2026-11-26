// Package horizontest holds the wait a test makes on work that waits on the event horizon. Three
// packages make it: internal/dispatch/api and internal/dispatch/routes, whose servers run in the
// test's own process, and cmd/dispatch, whose server is a process the test starts. It is an
// ordinary package rather than a _test.go file because Go cannot share test-only code across
// packages (internal/logtest says the same).
//
// Every event write of Dispatch's database holds the event horizon back until it ends
// (internal/dispatch/events/horizon.go, "Costs"): a test's own fixture, the server's own request
// writes, a migration, an operator's psql session that writes the event log. A transaction in
// another database, or one that writes no event, holds nothing. Work that waits on the horizon -
// the event feed's events_start, which feedEvents writes only once the relay's first cursor has
// awaited it (firstRelayCursor, internal/dispatch/api/event_stream_relay.go), an event the feed
// sends live, a horizon persisted above the log's newest event - is therefore as late as the
// longest such write, which a loaded machine can keep open for seconds. No fixed deadline bounds
// that (docs/testing.md, "A test that waits for the event feed's first frame"): a deadline raised
// to fit the worst run anyone has seen is the same flake on the day a write is held for a minute.
//
// So the wait is the shared one (internal/testwait), with no bound of its own: a hold of any
// length only delays it, and only the test binary's deadline ends it. When it ends there, its last
// read is the horizon's own account of itself (Watch.String): how many stall warnings
// (events.StallWarning) the horizon logged while the test waited, none included, and the newest of
// them, which names the oldest event writer's kind, pid, database and query from the horizon's own
// diagnosis. When that holder is a client of the test's own database - the defect class of
// dispatch://INFRA-636, a Dispatch write transaction left open across the horizon's proof, among
// its causes - the account says so, naming the client and how long its transaction had stood
// when it was named. The wait does not end on such a hold before the give-up: a hold in that
// database can be any ordinary event write - the test's own fixtures, the server's own requests -
// stretched as long as a loaded machine stretches it, and any age the wait ended at would be a
// bound of its own, which the shared wait has none of.
package horizontest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// StallWarning is the stall threshold and the repeat a horizon the wait reads must warn at: its
// stallAfter and its warnEvery (events.Horizon.SetStallWarning, which api.Deps' HorizonStallAfter
// and HorizonWarnEvery set, and cmd/dispatch's DISPATCH_TEST_HORIZON_STALL_AFTER and
// DISPATCH_TEST_HORIZON_WARN_EVERY for a test process). At the production five and thirty seconds
// a hold would go unreported for five seconds and the newest warning could be half a minute old;
// at this one the horizon names its holder within a fraction of a second and again as the hold
// goes on, so the account of a wait that gave up reads the holder as it stands.
const StallWarning = 250 * time.Millisecond

// Warning is one stall warning, as the wait reads it.
type Warning struct {
	// PendingFor is the age of the oldest pending observation the warning reports. It is zero
	// when the warning carries none, or none that reads as a duration, and the account then
	// dates no hold by it (heldFor).
	PendingFor time.Duration
	// Database is the database of the holder the warning names, and Named whether it names one
	// at all: warnStall's diagnosis_error arm and its "none open now" arm carry no database, and
	// a warning naming none is not one naming the empty database.
	Database string
	Named    bool
	// Client names the holder by its application_name and pid, for the account of a hold in the
	// test's own database (Watch.String): a transaction there can be anyone's - Dispatch's own,
	// an operator's psql session, another test's fixture - so the account says which.
	Client string
	// At is when the warning was logged and HolderSince the named holder's xact_start, both
	// read off the warning, so their difference is how long that holder had stood when it was
	// named. Either is zero when the warning carries none.
	At, HolderSince time.Time
	// Text is the warning as written, for a failure to print: it names the holder's kind, pid,
	// database and query.
	Text string
}

// heldFor is how long the holder the warning names has held the horizon back, by the warning's
// own account: the age of the oldest pending observation, but no more than the holder's own
// transaction had stood when it was named. pending_for alone is the oldest observation's age,
// which an earlier holder, released since, can have carried far past the age of the transaction
// now named, a young one that arrived after it. A warning that dates neither its holder nor itself
// holds for zero.
func (w Warning) heldFor() time.Duration {
	if w.At.IsZero() || w.HolderSince.IsZero() {
		return 0
	}
	return min(w.PendingFor, w.At.Sub(w.HolderSince))
}

// FromRecord reads a warning a horizon in the test's own process logged, from the attributes
// warnStall logged it with, with no parse.
func FromRecord(record slog.Record) Warning {
	warning := Warning{At: record.Time}
	var application, pid string
	record.Attrs(func(attr slog.Attr) bool {
		value := attr.Value.Resolve()
		switch attr.Key {
		case "pending_for":
			if value.Kind() == slog.KindDuration {
				warning.PendingFor = value.Duration()
			}
		case "xact_start":
			if value.Kind() == slog.KindTime {
				warning.HolderSince = value.Time()
			}
		case "database":
			if value.Kind() == slog.KindString {
				warning.Database, warning.Named = value.String(), true
			}
		case "application_name":
			application = value.String()
		case "pid":
			pid = value.String()
		}
		return true
	})
	warning.Client = clientOf(application, pid)
	var text strings.Builder
	_ = slog.NewTextHandler(&text, nil).Handle(context.Background(), record)
	warning.Text = strings.TrimSuffix(text.String(), "\n")
	return warning
}

// FromLine reads a warning a Dispatch process wrote, as one line of slog's text handler.
func FromLine(line string) Warning {
	warning := Warning{Text: line}
	if pendingFor, found := Field(line, "pending_for"); found {
		// A pending_for that will not parse leaves this at zero, below the bound.
		warning.PendingFor, _ = time.ParseDuration(pendingFor)
	}
	// A time that will not parse leaves its field zero, so the warning holds for zero (heldFor).
	if at, found := Field(line, "time"); found {
		warning.At, _ = time.Parse(time.RFC3339Nano, at)
	}
	if since, found := Field(line, "xact_start"); found {
		warning.HolderSince, _ = time.Parse(time.RFC3339Nano, since)
	}
	warning.Database, warning.Named = Field(line, "database")
	application, _ := Field(line, "application_name")
	pid, _ := Field(line, "pid")
	warning.Client = clientOf(application, pid)
	return warning
}

// clientOf names a holder by its application_name and pid. A client that set no application_name,
// which is what Dispatch's own connections look like, is said to have set none rather than printed
// as a blank.
func clientOf(application, pid string) string {
	if application == "" {
		application = "(none set)"
	}
	if pid == "" {
		pid = "(none named)"
	}
	return "application_name " + application + ", pid " + pid
}

// Field returns the value of line's key= field, line being one line of slog's text handler, and
// whether the line carried that field at all. A field the line does not carry and a field carrying
// the empty string are different things to a caller: reading the first as the second would have a
// line with no database= field read as naming the database "".
//
// The read walks the line as the sequence of key=value fields it is, rather than searching it for
// the text "key=", because a field's value is chosen by a client and so can hold that text: a
// client whose application_name is "database=dispatch_test_ab backfill" would have a search read
// that name as the line's database= field, and so have a hold in a foreign database read as one in
// the test's own. The walk compares whole keys too, so a caller asking for "name" is not given
// application_name's value.
//
// A value slog quoted is read whole and unquoted, and the walk resumes past the whole quoted
// literal, so the warning's query= and an application_name the client set with a space in it
// (pgAdmin sets "pgAdmin 4 - <connection>") both read as they were written; an unquoted value is
// the token from the "=" to the next space or the line's end.
//
// The quoted read is keyed on an opening double quote, the one quote slog writes, and not on
// strconv.QuotedPrefix's own scope, "a single-quoted, double-quoted, or backquoted Go string
// literal". A backquote is in slog's safe set, so a value a third party chose - an
// application_name, or the holder's query read out of pg_stat_activity - can open with one and is
// written unquoted; read as the raw string literal it opens, such a value runs across spaces and
// whole fields to the next backquote, which that same query can carry, and the field after it is
// then read out of the query's text: a foreign hold read as the test's own. A value that opens
// with a double quote and parses as no literal falls back to the unquoted read for the same
// reason, so the walk takes exactly what slog writes and no shape more.
func Field(line, key string) (string, bool) {
	for rest := line; rest != ""; {
		name, after, found := strings.Cut(rest, "=")
		if !found {
			return "", false
		}
		// The unquoted read, which is also where a value no quoted read takes whole ends up:
		// the token from the "=" to the next space or the line's end.
		var value string
		value, rest, _ = strings.Cut(after, " ")
		if strings.HasPrefix(after, `"`) {
			if literal, err := strconv.QuotedPrefix(after); err == nil {
				// A literal QuotedPrefix has already parsed cannot fail to unquote, and a
				// value that is no literal keeps the unquoted read above.
				if unquoted, err := strconv.Unquote(literal); err == nil {
					value, rest = unquoted, strings.TrimPrefix(after[len(literal):], " ")
				}
			}
		}
		if name == key {
			return value, true
		}
	}
	return "", false
}

// Lines reads each line lines returns, oldest first, as a warning (FromLine): a Dispatch process's
// output lines that carry events.StallWarning, which the caller selects.
func Lines(lines func() []string) func() []Warning {
	return func() []Warning {
		var warnings []Warning
		for _, line := range lines() {
			warnings = append(warnings, FromLine(line))
		}
		return warnings
	}
}

// Logger is the logger a test gives the horizons of a server that reads database
// (api.Deps.HorizonLogger, events.Horizon.SetLogger). Its handler keeps each stall warning for
// database's Listen and hands every record on to slog.Default's handler as it stands at that record,
// so a test that captures its logs itself still gets every line. Each test's database is a clone of
// its own, so a Listen on it reads only the warnings of that test's own horizons, whatever tests run
// beside it, and none of them swaps slog.Default to be heard. A warning logged through a logger made
// by With or WithGroup from it is kept without the attributes those added; warnStall uses neither.
func Logger(database string) *slog.Logger {
	return slog.New(&keepWarnings{kept: keptFor(database)})
}

// Listen answers the stall warnings logged through Logger(database) from now until stop, which t's
// cleanup also calls, oldest first: those of every horizon of a server on database, its second
// server's included, and of no other.
func Listen(t testing.TB, database string) (warnings func() []Warning, stop func()) {
	t.Helper()
	kept := keptFor(database)
	start := kept.count()
	var (
		mu  sync.Mutex
		end = -1 // how many warnings database had kept when stop ran; -1 until it runs
	)
	warnings = func() []Warning {
		mu.Lock()
		upTo := end
		mu.Unlock()
		return kept.read(start, upTo)
	}
	stop = func() {
		mu.Lock()
		defer mu.Unlock()
		if end < 0 {
			end = kept.count()
		}
	}
	t.Cleanup(stop)
	return warnings, stop
}

// kept is the stall warnings Logger's handlers have kept, one keeper a database.
var kept = struct {
	mu sync.Mutex
	by map[string]*keptWarnings
}{by: map[string]*keptWarnings{}}

// keptFor is database's keeper, made the first time a Logger or a Listen names it.
func keptFor(database string) *keptWarnings {
	kept.mu.Lock()
	defer kept.mu.Unlock()
	k, ok := kept.by[database]
	if !ok {
		k = &keptWarnings{}
		kept.by[database] = k
	}
	return k
}

// keptWarnings is the stall warnings one database's Logger handlers kept; the goroutines of that
// test's servers log into it.
type keptWarnings struct {
	mu      sync.Mutex
	records []slog.Record
}

func (k *keptWarnings) add(record slog.Record) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.records = append(k.records, record.Clone())
}

// count is how many warnings k has kept so far.
func (k *keptWarnings) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.records)
}

// read answers the warnings kept from the start'th on, oldest first, and when end is not negative
// only those before the end'th.
func (k *keptWarnings) read(start, end int) []Warning {
	k.mu.Lock()
	upTo := len(k.records)
	if end >= 0 {
		upTo = end
	}
	records := append([]slog.Record(nil), k.records[start:upTo]...)
	k.mu.Unlock()
	warnings := make([]Warning, 0, len(records))
	for _, record := range records {
		warnings = append(warnings, FromRecord(record))
	}
	return warnings
}

// keepWarnings is Logger's handler: it keeps each stall warning in kept and hands every record on
// to slog.Default's handler as it stands at that record, with the attributes and groups With and
// WithGroup added (scope), whenever that handler would take it.
type keepWarnings struct {
	kept  *keptWarnings
	scope []func(slog.Handler) slog.Handler
}

// next is slog.Default's handler now, with the handler's scope applied in the order it was added.
func (h *keepWarnings) next() slog.Handler {
	next := slog.Default().Handler()
	for _, apply := range h.scope {
		next = apply(next)
	}
	return next
}

func (h *keepWarnings) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn || h.next().Enabled(ctx, level)
}

func (h *keepWarnings) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == events.StallWarning {
		h.kept.add(record)
	}
	next := h.next()
	if !next.Enabled(ctx, record.Level) {
		return nil
	}
	return next.Handle(ctx, record)
}

func (h *keepWarnings) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &keepWarnings{kept: h.kept, scope: append(slices.Clip(h.scope), func(next slog.Handler) slog.Handler { return next.WithAttrs(attrs) })}
}

func (h *keepWarnings) WithGroup(name string) slog.Handler {
	return &keepWarnings{kept: h.kept, scope: append(slices.Clip(h.scope), func(next slog.Handler) slog.Handler { return next.WithGroup(name) })}
}

// Wait returns the one result arrived delivers: the outcome of work that waits on the event
// horizon, which what names in a failure. warnings answers every stall warning the horizon has
// logged so far, oldest first; ownDatabase is the database the horizon's pool reads; and detail,
// when not nil, is printed after a failure, as a test that started Dispatch as a process prints
// that process's output. It waits as Watch.Poll does.
//
// A gorilla connection is unusable after a read timeout - "After a read has timed out, the
// websocket connection state is corrupt" - so a reader the caller hands in sets no read deadline
// and reads on a goroutine of its own (ReadFirstFrame), and the caller reads the connection again
// only once this has returned.
func Wait[T any](t testing.TB, what string, arrived <-chan T, warnings func() []Warning, ownDatabase string, detail func() string) T {
	t.Helper()
	var result T
	NewWatch(warnings, ownDatabase).Poll(t, what, detail, func() bool {
		select {
		case result = <-arrived:
			return true
		default:
			return false
		}
	})
	return result
}

// Watch is what a wait on the horizon reads of it: the account a wait that gave up prints
// (String). Poll is the wait; a test that reads again until an answer the horizon held back is
// whole (api's awaitHorizonRead) polls through it with its read as the condition.
type Watch struct {
	warnings    func() []Warning
	ownDatabase string
	// before is how many warnings were logged before the watch began: they belong to a hold
	// before it, perhaps one long over, so the watch reads only what the horizon says from then.
	before int
}

// NewWatch starts a watch now. warnings answers every stall warning the horizon has logged so far,
// oldest first, and ownDatabase is the database the horizon's pool reads.
func NewWatch(warnings func() []Warning, ownDatabase string) *Watch {
	return &Watch{warnings: warnings, ownDatabase: ownDatabase, before: len(warnings())}
}

// since is the stall warnings the horizon has logged since the watch began, oldest first.
func (w *Watch) since() []Warning {
	return w.warnings()[w.before:]
}

// String is the watch's account of the horizon, which a wait that gave up at the test binary's
// deadline prints as its last read: how many stall warnings the horizon has logged since the watch
// began, and the newest of them, which names the holder from the horizon's own diagnosis. A count
// of none says the horizon never reported itself held while the test waited. When the newest
// names a holder in the test's own database (ownDatabaseHold), the account says so, with the
// client the warning names and how long its transaction had held the horizon back when it was
// named (heldFor): a hold in that database is Dispatch's own defect (dispatch://INFRA-636) or some
// other client's transaction there, and the client tells which.
func (w *Watch) String() string {
	since := w.since()
	account := fmt.Sprintf("the event horizon logged %d stall warnings while the test waited", len(since))
	if len(since) == 0 {
		return account
	}
	newest := since[len(since)-1]
	if ownDatabaseHold(newest, w.ownDatabase) {
		account += fmt.Sprintf("; the newest names a client of the test's own database %s (%s) whose transaction had held it back for %v",
			w.ownDatabase, newest.Client, newest.heldFor())
	}
	return account + "; the newest: " + newest.Text
}

// Poll asks condition until it holds, through the shared wait (testwait.Until) and with no bound
// of its own: a hold of any length, in any database, the test's own included, only delays it. When
// the wait gives up at the test binary's deadline its last read is the watch's account of the
// horizon (String), which names the holder; on that failure, and on one the condition makes
// itself, detail is printed after it when not nil.
func (w *Watch) Poll(t testing.TB, what string, detail func() string, condition func() bool) {
	t.Helper()
	held := false
	// A failure ends the test goroutine (runtime.Goexit) inside the wait, which runs this before
	// it ends.
	defer func() {
		if !held && detail != nil {
			t.Log(detail())
		}
	}()
	testwait.Until(t, what, func() (*Watch, bool) { return w, condition() })
	held = true
}

// WaitInProcess is Wait on a horizon of the test's own process, on own's server: it reads the
// warnings that server's horizons log through Logger(own's database) for as long as it waits
// (Listen). The horizon must warn at StallWarning and log through that Logger.
func WaitInProcess[T any](t *testing.T, what string, arrived <-chan T, own *store.Store) T {
	t.Helper()
	database := DatabaseOf(own)
	warnings, stop := Listen(t, database)
	defer stop()
	return Wait(t, what, arrived, warnings, database, nil)
}

// DatabaseOf is the database own's pool reads: the name a Logger and a Listen for a server on own
// take.
func DatabaseOf(own *store.Store) string {
	return own.Pool.Config().ConnConfig.Database
}

// ownDatabaseHold reports whether warning names a holder in ownDatabase, the database the horizon's
// pool reads. A warning whose diagnosis failed names no database (horizon.go's diagnosis_error
// arm), and neither does one that found no holder ("none open now"), so neither names the test's
// own. The warning names only the oldest holder at diagnosis time, so a foreign holder can mask
// one of Dispatch's own.
func ownDatabaseHold(warning Warning, ownDatabase string) bool {
	return warning.Named && warning.Database == ownDatabase
}

// FirstFrame is the one result ReadFirstFrame sends: the first frame but a heartbeat, with the
// type and sub it names, or the read that failed.
type FirstFrame struct {
	Data []byte
	Type string
	Sub  string
	Err  error
}

// ReadFirstFrame reads conn on a goroutine of its own until its first frame but a heartbeat, and
// sends exactly one result. It sets no read deadline, because a gorilla connection whose read has
// timed out is corrupt and the caller reads this one afterwards. The channel is buffered, so the
// goroutine leaves even when the test has failed and nobody receives - the test's cleanup closes
// conn, which ends the read - and it touches conn no more once it has sent.
func ReadFirstFrame(conn *gws.Conn) <-chan FirstFrame {
	read := make(chan FirstFrame, 1)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				read <- FirstFrame{Err: err}
				return
			}
			var frame struct{ Type, Sub string }
			if err := json.Unmarshal(data, &frame); err != nil || frame.Type != "heartbeat" {
				read <- FirstFrame{Data: data, Type: frame.Type, Sub: frame.Sub}
				return
			}
		}
	}()
	return read
}

// AwaitPersisted proves horizon's H until the H some process persisted reaches newest, waiting as
// PollInProcess does on a horizon set here to warn at StallWarning through Logger(own's database),
// and fails t when a proof fails or the shared wait gives up. own is the store whose database horizon's pool reads. A proof
// waits on the horizon, so each runs on a goroutine of its own (proveOnce) and the wait asks after
// it: the next proof begins once the one before has answered.
func AwaitPersisted(t *testing.T, horizon *events.Horizon, newest int64, own *store.Store) {
	t.Helper()
	horizon.SetStallWarning(StallWarning, StallWarning)
	horizon.SetLogger(Logger(DatabaseOf(own)))
	var proof <-chan persisted // the proof under way, nil between two
	PollInProcess(t, fmt.Sprintf("a persisted horizon of %d", newest), own, func() bool {
		if proof == nil {
			proof = proveOnce(t.Context(), horizon)
		}
		select {
		case answer := <-proof:
			proof = nil
			if answer.err != nil {
				t.Fatal(answer.err)
			}
			return answer.h >= newest
		default:
			return false
		}
	})
}

// persisted is what one proof of the horizon answers: the H some process persisted once it was
// proven, or the error that stopped the proof.
type persisted struct {
	h   int64
	err error
}

// proveOnce proves horizon's H once on a goroutine of its own and sends what was persisted then.
// It stops when ctx ends, which t.Context does as the test ends, failed or not; the channel is
// buffered, so the goroutine leaves even when nobody receives.
func proveOnce(ctx context.Context, horizon *events.Horizon) <-chan persisted {
	done := make(chan persisted, 1)
	go func() {
		if _, err := horizon.Safe(ctx); err != nil {
			done <- persisted{err: fmt.Errorf("observe the horizon: %w", err)}
			return
		}
		h, err := horizon.Persisted(ctx)
		if err != nil {
			err = fmt.Errorf("read the persisted horizon: %w", err)
		}
		done <- persisted{h: h, err: err}
	}()
	return done
}

// PollInProcess asks condition until it holds, as Watch.Poll does, on a horizon of the test's own
// process, on own's server: it reads the warnings that server's horizons log through Logger(own's
// database) for as long as it waits (Listen). The horizon must warn at StallWarning and log through
// that Logger.
func PollInProcess(t *testing.T, what string, own *store.Store, condition func() bool) {
	t.Helper()
	database := DatabaseOf(own)
	warnings, stop := Listen(t, database)
	defer stop()
	NewWatch(warnings, database).Poll(t, what, nil, condition)
}
