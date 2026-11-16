// Package horizontest holds the wait a test makes on work that waits on the event horizon. Three
// packages make it: internal/dispatch/api and internal/dispatch/routes, whose servers run in the
// test's own process, and cmd/dispatch, whose server is a process the test starts. It is an
// ordinary package rather than a _test.go file because Go cannot share test-only code across
// packages (internal/logtest says the same).
//
// The event horizon's proof is instance-wide (internal/dispatch/events/horizon.go, "Costs"): any
// transaction that holds a transaction id, in any database on the Postgres instance, holds the
// horizon back until it ends - Dispatch's own writes, another service's on the same instance, an
// operator's open psql session, and on CI's one Postgres another package's test. Work that waits
// on the horizon - the event feed's events_start, which feedEvents writes only once the relay's
// first cursor has awaited it (firstRelayCursor, internal/dispatch/api/event_stream_relay.go), an
// event the feed sends live, a horizon persisted above the log's newest event - is therefore as
// late as the longest such transaction, which no test owns. No fixed deadline bounds that
// (docs/testing.md, "A test that waits for the event feed's first frame"): a deadline raised to fit
// the worst run anyone has seen is the same flake on the day someone holds a transaction for a
// minute.
//
// So the wait keys on the horizon's own account of itself. While the horizon keeps logging
// events.StallWarning it is saying it is held back by an open transaction, and the wait goes on
// for as long as that keeps arriving. When the horizon says nothing - no warning at all, or no new
// warning for a window - the wait reads the instance's holder itself, as the horizon's diagnosis
// does, since the goroutine that logs the warning can be late on a loaded machine; it fails fast
// only when no transaction holds an id, because then nothing is holding the horizon and the
// missing result is a defect. The one stall the wait refuses to wait out is a holder in the test's
// own database that has itself stood OwnHoldBound: that is the defect class of
// dispatch://INFRA-636 (a Dispatch write transaction left open across the horizon's proof). The
// bound is what keeps that rule from failing on every other client of that database - the test's
// own fixtures, an operator's psql session - whose ordinary short transaction holds the horizon
// exactly as a foreign one does and is no defect. Each failure names the newest warning, so the
// holder is in the output.
//
// The numbers below were measured in a pod on 2026-11-10, nproc 2, against PostgreSQL 18.6, each
// arm driving cmd/dispatch's events_start wait (TestShutdown's openLiveEvents):
//
//   - U = 217 ms, the worst subscribe-to-events_start gap with no transaction held, over 20
//     subscriptions in 10 runs under 8 busy loops (four times nproc, nice -n 10), all 10 runs
//     passing. CPU load alone does not reproduce the failure.
//   - W = 0.460 s, the worst subscribe-to-first-stall-warning gap, over 10 windows under both
//     loads at once: those busy loops, and a pulse holding a transaction 12 s of every 13.
//   - G = 0.319 s, the worst gap between two consecutive stall warnings while one hold stood, over
//     324 samples under those same two loads.
package horizontest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// StallWarning is the stall threshold and the repeat a horizon the wait keys on must warn at:
	// its stallAfter and its warnEvery (events.Horizon.SetStallWarning, which api.Deps'
	// HorizonStallAfter and HorizonWarnEvery set, and cmd/dispatch's
	// DISPATCH_TEST_HORIZON_STALL_AFTER and DISPATCH_TEST_HORIZON_WARN_EVERY for a test process).
	// At the production five and thirty seconds the first warning would come after Base had
	// already failed the wait.
	StallWarning = 250 * time.Millisecond
	// Base is how long the wait gives a horizon that has said nothing. Three times the worst of U
	// and W is 1.38 s, but the floor is the soonest a held horizon can have said so: its
	// stallAfter (StallWarning) plus its warnEvery (StallWarning) plus horizonDiagnosisTimeout
	// (2 s), the bound on the pg_stat_activity diagnosis warnStall runs before it logs, which is
	// 2.5 s. Rounded up to a whole second.
	Base = 3 * time.Second
	// Extension is how long the wait gives a horizon that has said it is held, since its newest
	// warning. Three times G is 0.96 s, but G's own ceiling is higher than anything measured, so
	// this one comes from the horizon's constants rather than the measurement: warnStall logs
	// after its diagnosis, on a goroutine of its own (the go h.warnStall(...) call in horizon.go's
	// observation loop), and that diagnosis is bounded by horizonDiagnosisTimeout (2 s), so two
	// logged warnings can stand warnEvery (StallWarning) plus horizonBackoffMax (250 ms) plus
	// horizonDiagnosisTimeout apart - 2.5 s - on a server whose pg_stat_activity is slow, which
	// is the server a test meets in CI. Rounded up to a whole second.
	Extension = 3 * time.Second
	// Poll is how often the wait reads the warnings. A sampling rate, not a bound: it is below a
	// tenth of Extension, so it costs that window nothing.
	Poll = 250 * time.Millisecond
	// Drain is what the wait leaves of the run's own deadline, so its failure prints rather than
	// being cut off by the package timeout, which would name no test.
	Drain = 30 * time.Second
	// OwnHoldBound is how long a hold in the test's own database must have stood, by the
	// horizon's own account, before the wait fails on it rather than counting it as a stall.
	// Without a bound the own-database rule is a deadline of about 0.3 s on any transaction in
	// that database, from any client: the horizon warns at StallWarning, so the first warning
	// naming the database would end the wait almost at once.
	//
	// Sampling pg_stat_activity every 100 ms while cmd/dispatch's events_start test ran 10 times
	// under the calibrated load (a rolling foreign hold, a new 25 s transaction every second in
	// postgres, beside 8 busy loops at four times nproc), in a pod on 2026-11-10, nproc 2,
	// PostgreSQL 18.6: 202 samples caught a transaction holding a transaction id in a
	// dispatch_test_* database, across 27 distinct backends, every one of them carrying no
	// application_name - which is what Dispatch's own pools look like and what a test's own
	// storetest pool looks like too, so the samples do not say whose the transactions were. The
	// worst age was 1.449 s. Three times that is 4.35 s, so the bound is 5 s, rounded up to a
	// whole second. That sampling rate means a transaction shorter than 100 ms can be missed
	// entirely, which does not matter for a worst case: a hold too short to be sampled is far
	// below the one the bound is sized from.
	OwnHoldBound = 5 * time.Second
)

// Warning is one stall warning, as the wait reads it.
type Warning struct {
	// PendingFor is the age of the oldest pending observation the warning reports. It is zero
	// when the warning carries none, or none that reads as a duration, and the own-hold rule
	// then takes the warning as the plain stall it also is.
	PendingFor time.Duration
	// Database is the database of the holder the warning names, and Named whether it names one
	// at all: warnStall's diagnosis_error arm and its "none open now" arm carry no database, and
	// a warning naming none is not one naming the empty database.
	Database string
	Named    bool
	// Client names the holder by its application_name and pid, for the own-hold failure: a
	// transaction in the test's own database can be anyone's - Dispatch's own, an operator's
	// psql session, another test's fixture - so the failure says which.
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
// which an earlier holder, released since, can have carried past OwnHoldBound while the
// transaction named is a young one that arrived after it. A warning that dates neither its holder
// nor itself holds for zero, which the own-hold rule reads as the plain stall it also is.
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

// Listen makes the default logger's handler one that keeps every stall warning and passes every
// record on to the handler it replaced, until stop, which t's cleanup also calls. A horizon logs
// its warnings to slog.Default as it stands when it warns (events.Horizon's log), so this hears
// every horizon of the test's own process, its second server's included, and a test that captures
// its logs itself still gets every line. warnings answers the stall warnings kept so far, oldest
// first. A warning logged through a logger made by With or WithGroup is kept without the
// attributes those added; warnStall uses neither.
//
// Only slog's default changes: the log package keeps its own output and flags. slog.SetDefault
// points the log package at the new handler, and slog's own default handler writes through the
// log package, so a record handed on to it there would wait for the lock the log package holds
// while a log.Print is under way: the first line logged would deadlock every goroutine that logs.
// So when the handler Listen replaces is slog's own, records are handed on to one that writes as
// it does on a logger of its own over the log package's writer (asSlogWrites).
func Listen(t *testing.T) (warnings func() []Warning, stop func()) {
	t.Helper()
	kept := &keptWarnings{}
	previous, output, flags := slog.Default(), log.Writer(), log.Flags()
	next := previous.Handler()
	if next == slogsOwn {
		next = asSlogWrites(next, output, log.Prefix(), flags)
	}
	slog.SetDefault(slog.New(keepWarnings{next: next, kept: kept}))
	log.SetOutput(output)
	log.SetFlags(flags)
	var once sync.Once
	stop = func() {
		once.Do(func() {
			slog.SetDefault(previous)
			log.SetOutput(output)
			log.SetFlags(flags)
		})
	}
	t.Cleanup(stop)
	return kept.read, stop
}

// slogsOwn is slog's own default handler, the one the process starts with: no package's init
// replaces it, so it is the handler slog.Default has here.
var slogsOwn = slog.Default().Handler()

// slogWrites writes each record as slog's own default handler does - "LEVEL message key=value…"
// through a log.Logger, with the log package's prefix and flags - but on a log.Logger of its own
// over the writer the log package had, so it never takes the lock of the log package's logger.
// own is slog's handler, which answers Enabled as before.
type slogWrites struct {
	own    slog.Handler
	logger *log.Logger
	mu     *sync.Mutex
	buffer *bytes.Buffer
	// attrs renders a record's attributes into buffer as slog's text form, with the time, level
	// and message dropped, since the line carries those itself.
	attrs slog.Handler
}

func asSlogWrites(own slog.Handler, output io.Writer, prefix string, flags int) slogWrites {
	buffer := &bytes.Buffer{}
	return slogWrites{
		own:    own,
		logger: log.New(output, prefix, flags),
		mu:     &sync.Mutex{},
		buffer: buffer,
		attrs: slog.NewTextHandler(buffer, &slog.HandlerOptions{
			Level: slog.LevelDebug,
			ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
				if len(groups) == 0 && (attr.Key == slog.TimeKey || attr.Key == slog.LevelKey || attr.Key == slog.MessageKey) {
					return slog.Attr{}
				}
				return attr
			},
		}),
	}
}

func (h slogWrites) Enabled(ctx context.Context, level slog.Level) bool {
	return h.own.Enabled(ctx, level)
}

func (h slogWrites) Handle(ctx context.Context, record slog.Record) error {
	h.mu.Lock()
	h.buffer.Reset()
	err := h.attrs.Handle(ctx, record)
	attrs := strings.TrimSuffix(h.buffer.String(), "\n")
	h.mu.Unlock()
	if err != nil {
		return err
	}
	line := record.Level.String() + " " + record.Message
	if attrs != "" {
		line += " " + attrs
	}
	return h.logger.Output(2, line)
}

func (h slogWrites) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.attrs = h.attrs.WithAttrs(attrs)
	return h
}

func (h slogWrites) WithGroup(name string) slog.Handler {
	h.attrs = h.attrs.WithGroup(name)
	return h
}

// keptWarnings is the stall warnings Listen's handler kept; the goroutines a test's server logs
// from share it.
type keptWarnings struct {
	mu      sync.Mutex
	records []slog.Record
}

func (k *keptWarnings) add(record slog.Record) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.records = append(k.records, record.Clone())
}

func (k *keptWarnings) read() []Warning {
	k.mu.Lock()
	records := append([]slog.Record(nil), k.records...)
	k.mu.Unlock()
	warnings := make([]Warning, 0, len(records))
	for _, record := range records {
		warnings = append(warnings, FromRecord(record))
	}
	return warnings
}

// keepWarnings is Listen's handler: it keeps each stall warning and hands every record on to next
// whenever next would take it.
type keepWarnings struct {
	next slog.Handler
	kept *keptWarnings
}

func (h keepWarnings) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn || h.next.Enabled(ctx, level)
}

func (h keepWarnings) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == events.StallWarning {
		h.kept.add(record)
	}
	if !h.next.Enabled(ctx, record.Level) {
		return nil
	}
	return h.next.Handle(ctx, record)
}

func (h keepWarnings) WithAttrs(attrs []slog.Attr) slog.Handler {
	return keepWarnings{next: h.next.WithAttrs(attrs), kept: h.kept}
}

func (h keepWarnings) WithGroup(name string) slog.Handler {
	return keepWarnings{next: h.next.WithGroup(name), kept: h.kept}
}

// Wait returns the one result arrived delivers: the outcome of work that waits on the event
// horizon, which what names in a failure. warnings answers every stall warning the horizon has
// logged so far, oldest first; ownDatabase is the database the horizon's pool reads; probe, when
// not nil, reads the instance's holder itself when the horizon has fallen silent (Watch); and
// detail, when not nil, is printed after a failure, as a test that started Dispatch as a process
// prints that process's output.
//
// A gorilla connection is unusable after a read timeout - "After a read has timed out, the
// websocket connection state is corrupt" - so a reader the caller hands in sets no read deadline
// and reads on a goroutine of its own (ReadFirstFrame), and the caller reads the connection again
// only once this has returned.
func Wait[T any](t *testing.T, what string, arrived <-chan T, warnings func() []Warning, ownDatabase string, probe Querier, detail func() string) T {
	t.Helper()
	watch := NewWatch(t, warnings, ownDatabase, probe)
	ticker := time.NewTicker(Poll)
	defer ticker.Stop()
	for {
		select {
		case result := <-arrived:
			return result
		case <-ticker.C:
			failure := watch.Failure(what)
			if failure == "" {
				continue
			}
			// A result already delivered wins over a failing verdict: the ticker and the
			// result can be ready at the same select, which chooses among ready arms at
			// random, so a result delivered in the same tick would otherwise fail a run that
			// had it.
			select {
			case result := <-arrived:
				return result
			default:
			}
			if detail != nil {
				failure += "\n" + detail()
			}
			t.Fatal(failure)
			var none T
			return none
		}
	}
}

// Watch is the judgement Wait makes, for a wait the caller drives itself: a test that reads again
// until an answer the horizon held back is whole (api's awaitHorizonRead) has no result to send on
// a channel, and asks the watch after each read whether to read again.
//
// Silence alone does not end a wait that has a probe. The warnings come from a goroutine of the
// horizon's that runs a diagnosis and then logs, and a loaded machine can put that goroutine off
// past Base or Extension while a transaction holds the horizon as surely as before. So when the
// horizon has said nothing for that long, the watch reads the holder itself, with the query the
// diagnosis runs (events.StallHolderSQL), and fails only if no transaction holds an id on the
// instance; a holder it finds counts as a warning, so the own-hold rule judges it too.
type Watch struct {
	warnings func() []Warning
	probe    Querier
	began    time.Time
	// before is how many warnings were logged before the watch began: they belong to a hold
	// before it, perhaps one long over, so the watch counts only what the horizon says from then.
	before int
	// seen is how many of the warnings since the watch began it has counted.
	seen int
	// rose is when the watch last counted a warning or a holder it read itself, or began while it
	// has counted neither.
	rose time.Time
	// drain is Drain before the run's own deadline, and zero for a run given none (go test
	// -timeout 0), which has no drain to fail on.
	drain   time.Time
	current state
}

// Querier is what a watch reads the holder through: the pool of the test's own server, or a
// connection to the database a Dispatch process reads (Dialed).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NewWatch starts a watch now. warnings answers every stall warning the horizon has logged so far,
// oldest first, ownDatabase is the database the horizon's pool reads, and probe, when not nil, is
// what the watch reads the holder through once the horizon has fallen silent.
func NewWatch(t *testing.T, warnings func() []Warning, ownDatabase string, probe Querier) *Watch {
	t.Helper()
	now := time.Now()
	watch := &Watch{warnings: warnings, probe: probe, began: now, before: len(warnings()), rose: now, current: state{ownDatabase: ownDatabase}}
	if deadline, ok := t.Deadline(); ok {
		watch.drain = deadline.Add(-Drain)
	}
	return watch
}

// Failure says why the wait for what must end now, naming the newest warning, or "" while the
// horizon's account lets it go on (verdictOf).
func (w *Watch) Failure(what string) string {
	now := time.Now()
	// A count of the warnings since the watch began, never whether one was seen: a check that
	// latched on the first warning would extend the wait for ever on a horizon that has stopped
	// warning.
	if since := w.warnings()[w.before:]; len(since) > w.seen {
		w.current.warnings += len(since) - w.seen
		w.seen = len(since)
		w.current.last = since[len(since)-1]
		w.rose = now
	}
	w.observe(now)
	elapsed := w.current.elapsed.Round(time.Millisecond)
	if !w.drain.IsZero() && !now.Before(w.drain) {
		return fmt.Sprintf("%s did not arrive in the %v the run had left; the event horizon's newest stall warning was %s",
			what, elapsed, w.current.lastText())
	}
	verdict := verdictOf(w.current)
	var probed string
	if (verdict == noSignal || verdict == signalStopped) && w.probe != nil {
		switch held, found, err := readHolder(w.probe); {
		case err != nil:
			probed = fmt.Sprintf("; reading the instance's holder failed too: %v", err)
		case !found:
			probed = "; and no transaction on the instance holds a transaction id now"
		default:
			// A holder stands, whether or not the horizon's warning about it has come yet.
			w.current.warnings++
			w.current.last = held
			w.rose = now
			w.observe(now)
			verdict = verdictOf(w.current)
		}
	}
	switch verdict {
	case noSignal:
		return fmt.Sprintf("%s did not arrive in %v, and the event horizon never said it was held back by an open transaction%s",
			what, elapsed, probed)
	case signalStopped:
		return fmt.Sprintf("%s did not arrive in %v, and the event horizon stopped saying it was held back %v ago%s; its newest stall warning was %s",
			what, elapsed, w.current.sinceNewWarning.Round(time.Millisecond), probed, w.current.lastText())
	case ownHold:
		return fmt.Sprintf("%s did not arrive in %v, and the event horizon says a client of the test's own database %s (%s) has held it back for %v; its newest stall warning was %s",
			what, elapsed, w.current.ownDatabase, w.current.last.Client, w.current.last.heldFor(), w.current.lastText())
	}
	return ""
}

// observe sets the watch's clocks at now.
func (w *Watch) observe(now time.Time) {
	w.current.elapsed = now.Sub(w.began)
	w.current.sinceNewWarning = now.Sub(w.rose)
}

// Poll asks condition every every until it holds, for as long as the horizon's account lets the
// wait go on, and fails t naming what, with detail after it when not nil, when that account ends
// the wait first: a condition on work that waits on the horizon, such as a reader's cursor reaching
// an event, has no result to send on a channel.
func (w *Watch) Poll(t *testing.T, what string, every time.Duration, detail func() string, condition func() bool) {
	t.Helper()
	for !condition() {
		if failure := w.Failure(what); failure != "" {
			// The condition may have come true since it was last asked; a run that has it is
			// not failed for the order of two reads.
			if condition() {
				return
			}
			if detail != nil {
				failure += "\n" + detail()
			}
			t.Fatal(failure)
		}
		time.Sleep(every)
	}
}

// probeTimeout bounds the watch's own read of the holder, as horizonDiagnosisTimeout bounds the
// horizon's.
const probeTimeout = 2 * time.Second

// readHolder reads the instance's oldest transaction holding an id through db, with the query the
// horizon's diagnosis runs, as a warning: dated by the database's own clock, so its holder's age
// is read on one clock, and holding for that age (pending_for), since a watch that reads it has
// been waiting at least as long as the holder it finds has stood.
func readHolder(db Querier) (Warning, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	var kind, state, application, database, query string
	var pid int32
	var started *time.Time
	var at time.Time
	err := db.QueryRow(ctx, `select kind, pid, started, state, application_name, database, query, clock_timestamp() from (`+
		events.StallHolderSQL+`) holder`).Scan(&kind, &pid, &started, &state, &application, &database, &query, &at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Warning{}, false, nil
	case err != nil:
		return Warning{}, false, err
	}
	held := Warning{At: at, Database: database, Named: true, Client: clientOf(application, strconv.Itoa(int(pid)))}
	if started != nil {
		held.HolderSince = *started
		held.PendingFor = at.Sub(*started)
	}
	held.Text = fmt.Sprintf("(read by the wait itself, the horizon's warning not yet logged) holder=%s pid=%d xact_start=%v state=%s application_name=%q database=%s query=%q",
		kind, pid, held.HolderSince, state, application, database, query)
	return held, true, nil
}

// Dialed is a Querier that opens a connection of its own to databaseURL for each read and closes
// it once the row is read: for a test whose Dispatch runs as a process, which holds no pool the
// watch could read through, and whose connections the process may need.
func Dialed(databaseURL string) Querier { return dialed(databaseURL) }

type dialed string

func (d dialed) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	conn, err := pgx.Connect(ctx, string(d))
	if err != nil {
		return failedRow{err}
	}
	return closingRow{conn: conn, row: conn.QueryRow(ctx, sql, args...)}
}

// failedRow is a row whose read fails with the connection's error.
type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

// closingRow closes its connection once it has been read.
type closingRow struct {
	conn *pgx.Conn
	row  pgx.Row
}

func (r closingRow) Scan(dest ...any) error {
	defer r.conn.Close(context.Background())
	return r.row.Scan(dest...)
}

// WaitInProcess is Wait on a horizon of the test's own process, on own's server: it listens for
// the horizon's warnings for as long as it waits (Listen), and reads the holder through own's pool
// once they fall silent. The horizon must warn at StallWarning.
func WaitInProcess[T any](t *testing.T, what string, arrived <-chan T, own *store.Store) T {
	t.Helper()
	warnings, stop := Listen(t)
	defer stop()
	return Wait(t, what, arrived, warnings, own.Pool.Config().ConnConfig.Database, own.Pool, nil)
}

// state is what the wait knows at one poll.
type state struct {
	elapsed         time.Duration // since the wait began
	sinceNewWarning time.Duration // since the warning count last rose; meaningless while warnings is 0
	warnings        int           // stall warnings since the wait began
	last            Warning       // the newest of them; the zero Warning while warnings is 0
	ownDatabase     string        // the database the horizon's pool reads
}

// lastText is the newest warning for a failure to print.
func (s state) lastText() string {
	if s.warnings == 0 {
		return "(none since the wait began)"
	}
	return s.last.Text
}

// verdict is what the wait does with one state: wait on, or fail with the reason the state gives.
type verdict int

const (
	waitOn        verdict = iota
	noSignal              // nothing arrived and the horizon never said it was held
	signalStopped         // the horizon stopped saying it was held, and nothing arrived
	ownHold               // the stall is a hold in the test's own database, not a foreign one
)

// String names a verdict, so a failed row reads as the decision the wait made rather than as the
// integer the constants are.
func (v verdict) String() string {
	switch v {
	case waitOn:
		return "waitOn"
	case noSignal:
		return "noSignal"
	case signalStopped:
		return "signalStopped"
	case ownHold:
		return "ownHold"
	}
	return fmt.Sprintf("verdict(%d)", int(v))
}

// verdictOf judges one poll of the wait. It is a function of the state alone, so it can be driven
// with no server and no Postgres.
//
// Three readings fall out of the rules, and all three are the safe ones. A warning whose diagnosis
// failed names no database (horizon.go's diagnosis_error arm), and neither does one that found no
// holder ("none open now"); since neither names the test's own database, each counts as a stall
// and extends the wait. A warning that dates neither its holder nor itself holds for zero
// (heldFor), so the own-hold rule takes it as the plain stall it also is. The known limit is the
// other way about: the warning names only the oldest holder at diagnosis time, so a foreign holder
// can mask one of Dispatch's own.
//
// The own-hold rule is judged on how long the holder named has held the horizon (heldFor), not on
// pending_for alone, which is the age of the oldest pending observation: a foreign holder that has
// since released can carry that past OwnHoldBound while the holder now named is a transaction of
// the test's own database that began a moment ago. Such a warning extends the wait as any short
// hold does; a transaction of that database that has itself stood OwnHoldBound still fails it.
func verdictOf(s state) verdict {
	switch {
	// A hold in the test's own database that has stood the bound is the defect, whatever the
	// elapsed time: waiting it out would pass a test whose job is to fail on it
	// (dispatch://INFRA-636). A shorter one is some client's ordinary transaction and extends
	// the wait, exactly as a foreign warning does.
	case s.warnings > 0 && s.last.heldFor() >= OwnHoldBound && s.last.Named && s.last.Database == s.ownDatabase:
		return ownHold
	case s.warnings == 0 && s.elapsed >= Base:
		return noSignal
	case s.warnings > 0 && s.sinceNewWarning >= Extension:
		return signalStopped
	default:
		return waitOn
	}
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

// AwaitPersisted proves horizon's H as often as it can until the H some process persisted reaches
// newest, waiting as WaitInProcess does on the horizon's own account of itself (the horizon is set
// to warn at StallWarning here), and fails t when that account ends the wait first. own is the
// store whose database horizon's pool reads.
func AwaitPersisted(t *testing.T, horizon *events.Horizon, newest int64, own *store.Store) {
	t.Helper()
	horizon.SetStallWarning(StallWarning, StallWarning)
	if err := WaitInProcess(t, fmt.Sprintf("a persisted horizon of %d", newest), persistedThrough(t.Context(), horizon, newest), own); err != nil {
		t.Fatal(err)
	}
}

// persistedThrough proves horizon's H on a goroutine of its own, as often as it can, until the H
// some process persisted reaches newest, and then sends nil, or sends the error that stopped it. It
// stops when ctx ends, which t.Context does as the test ends, failed or not.
func persistedThrough(ctx context.Context, horizon *events.Horizon, newest int64) <-chan error {
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := horizon.Safe(ctx); err != nil {
				done <- fmt.Errorf("observe the horizon: %w", err)
				return
			}
			persisted, err := horizon.Persisted(ctx)
			if err != nil {
				done <- fmt.Errorf("read the persisted horizon: %w", err)
				return
			}
			if persisted >= newest {
				done <- nil
				return
			}
			select {
			case <-ctx.Done():
				done <- ctx.Err()
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	return done
}

// PollInProcess asks condition every 10 ms until it holds, for as long as a horizon of the test's
// own process, on own's server, keeps saying it is held back (Watch.Poll), and fails the test
// naming what when the horizon's account ends the wait first. The horizon must warn at
// StallWarning.
func PollInProcess(t *testing.T, what string, own *store.Store, condition func() bool) {
	t.Helper()
	warnings, stop := Listen(t)
	defer stop()
	NewWatch(t, warnings, own.Pool.Config().ConnConfig.Database, own.Pool).Poll(t, what, 10*time.Millisecond, nil, condition)
}
