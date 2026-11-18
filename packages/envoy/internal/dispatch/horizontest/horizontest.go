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
// So the wait is the shared one (internal/testwait), with no bound of its own: a hold of any
// length only delays it, and only the test binary's deadline ends it. When it ends there, its last
// read is the horizon's own account of itself (Watch.String): how many stall warnings
// (events.StallWarning) the horizon logged while the test waited, none included, and the newest of
// them, which names the holder's kind, pid, database and query from the horizon's own diagnosis.
// The one stall the wait refuses to wait out is a holder in the test's own database that has
// itself stood OwnHoldBound, by the horizon's own warning: that is the defect class of
// dispatch://INFRA-636 (a Dispatch write transaction left open across the horizon's proof), and
// the wait fails on it at once, naming the warning. The bound is what keeps that rule from failing
// on every other client of that database - the test's own fixtures, an operator's psql session -
// whose ordinary short transaction holds the horizon exactly as a foreign one does and is no
// defect.
package horizontest

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	// StallWarning is the stall threshold and the repeat a horizon the wait reads must warn at:
	// its stallAfter and its warnEvery (events.Horizon.SetStallWarning, which api.Deps'
	// HorizonStallAfter and HorizonWarnEvery set, and cmd/dispatch's
	// DISPATCH_TEST_HORIZON_STALL_AFTER and DISPATCH_TEST_HORIZON_WARN_EVERY for a test process).
	// At the production five and thirty seconds a hold would go unreported for five seconds and
	// the newest warning could be half a minute old; at this one the horizon names its holder
	// within a fraction of a second and again as the hold goes on, so the own-hold rule and the
	// account of a wait that gave up read the holder as it stands.
	StallWarning = 250 * time.Millisecond
	// OwnHoldBound is how long a hold in the test's own database must have stood, by the
	// horizon's own account, before the wait fails on it rather than waiting it out as it does
	// any other hold. Without a bound the own-hold rule would fail on any transaction in that
	// database, from any client: the horizon warns at StallWarning, so the first warning naming
	// the database would end the wait almost at once.
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
// logged so far, oldest first; ownDatabase is the database the horizon's pool reads; and detail,
// when not nil, is printed after a failure, as a test that started Dispatch as a process prints
// that process's output. It waits as Watch.Poll does.
//
// A gorilla connection is unusable after a read timeout - "After a read has timed out, the
// websocket connection state is corrupt" - so a reader the caller hands in sets no read deadline
// and reads on a goroutine of its own (ReadFirstFrame), and the caller reads the connection again
// only once this has returned.
func Wait[T any](t *testing.T, what string, arrived <-chan T, warnings func() []Warning, ownDatabase string, detail func() string) T {
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

// Watch is what a wait on the horizon reads of it: the own-hold rule it fails on at once
// (Failure), and the account a wait that gave up prints (String). Poll is the wait; a test that
// reads again until an answer the horizon held back is whole (api's awaitHorizonRead) polls
// through it with its read as the condition.
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

// Failure says why the wait for what must end at once, or "" while the horizon's account lets it
// go on: the newest stall warning since the watch began names a holder in the test's own database
// that has held the horizon back at least OwnHoldBound (ownHold).
func (w *Watch) Failure(what string) string {
	since := w.since()
	if len(since) == 0 {
		return ""
	}
	newest := since[len(since)-1]
	if !ownHold(newest, w.ownDatabase) {
		return ""
	}
	return fmt.Sprintf("%s did not arrive, and the event horizon says a client of the test's own database %s (%s) has held it back for %v; its newest stall warning was %s",
		what, w.ownDatabase, newest.Client, newest.heldFor(), newest.Text)
}

// String is the watch's account of the horizon, which a wait that gave up at the test binary's
// deadline prints as its last read: how many stall warnings the horizon has logged since the watch
// began, and the newest of them, which names the holder from the horizon's own diagnosis. A count
// of none says the horizon never reported itself held while the test waited.
func (w *Watch) String() string {
	since := w.since()
	account := fmt.Sprintf("the event horizon logged %d stall warnings while the test waited", len(since))
	if len(since) > 0 {
		account += "; the newest: " + since[len(since)-1].Text
	}
	return account
}

// Poll asks condition until it holds, through the shared wait (testwait.Until) and with no bound
// of its own: a hold of any length only delays it. It fails t at once, naming what, when the
// horizon names an own hold (Failure), and when the wait gives up at the test binary's deadline its
// last read is the watch's account of the horizon (String). On either failure, and on one the
// condition makes itself, detail is printed after it when not nil.
func (w *Watch) Poll(t *testing.T, what string, detail func() string, condition func() bool) {
	t.Helper()
	held := false
	// A failure ends the test goroutine (runtime.Goexit) inside the wait, which runs this before
	// it ends.
	defer func() {
		if !held && detail != nil {
			t.Log(detail())
		}
	}()
	testwait.Until(t, what, func() (*Watch, bool) {
		if condition() {
			return w, true
		}
		if failure := w.Failure(what); failure != "" {
			// The condition may have come true since it was last asked; a run that has it is
			// not failed for the order of two reads.
			if condition() {
				return w, true
			}
			t.Fatal(failure)
		}
		return w, false
	})
	held = true
}

// WaitInProcess is Wait on a horizon of the test's own process, on own's server: it listens for
// the horizon's warnings for as long as it waits (Listen). The horizon must warn at StallWarning.
func WaitInProcess[T any](t *testing.T, what string, arrived <-chan T, own *store.Store) T {
	t.Helper()
	warnings, stop := Listen(t)
	defer stop()
	return Wait(t, what, arrived, warnings, own.Pool.Config().ConnConfig.Database, nil)
}

// ownHold reports whether warning names the one stall a wait refuses to wait out: a holder in
// ownDatabase that has itself held the horizon back at least OwnHoldBound, by the warning's own
// account (heldFor). A hold that has stood the bound in the test's own database is the defect,
// however long the test has waited: waiting it out would pass a test whose job is to fail on it
// (dispatch://INFRA-636). A shorter one is some client's ordinary transaction, which the wait
// waits out exactly as it does a foreign one.
//
// Three readings fall out of the rule, and all three are the safe ones. A warning whose diagnosis
// failed names no database (horizon.go's diagnosis_error arm), and neither does one that found no
// holder ("none open now"); since neither names the test's own database, neither is an own hold. A
// warning that dates neither its holder nor itself holds for zero (heldFor), so it is the plain
// stall it also is. The known limit is the other way about: the warning names only the oldest
// holder at diagnosis time, so a foreign holder can mask one of Dispatch's own.
//
// The rule is judged on how long the holder named has held the horizon (heldFor), not on
// pending_for alone, which is the age of the oldest pending observation: a foreign holder that has
// since released can carry that past OwnHoldBound while the holder now named is a transaction of
// the test's own database that began a moment ago.
func ownHold(warning Warning, ownDatabase string) bool {
	return warning.Named && warning.Database == ownDatabase && warning.heldFor() >= OwnHoldBound
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
// PollInProcess does on a horizon set to warn at StallWarning here, and fails t when a proof fails
// or the horizon names an own hold. own is the store whose database horizon's pool reads. A proof
// waits on the horizon, so each runs on a goroutine of its own (proveOnce) and the wait asks after
// it: the next proof begins once the one before has answered.
func AwaitPersisted(t *testing.T, horizon *events.Horizon, newest int64, own *store.Store) {
	t.Helper()
	horizon.SetStallWarning(StallWarning, StallWarning)
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
// process, on own's server: it listens for the horizon's warnings for as long as it waits (Listen).
// The horizon must warn at StallWarning.
func PollInProcess(t *testing.T, what string, own *store.Store, condition func() bool) {
	t.Helper()
	warnings, stop := Listen(t)
	defer stop()
	NewWatch(warnings, own.Pool.Config().ConnConfig.Database).Poll(t, what, nil, condition)
}
