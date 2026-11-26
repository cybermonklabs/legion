package replay

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"
)

// simClock is a replay's simulated clock: the one clock its engine (workflow.Config.Clock), its
// admission pass (admit.Admission.UseClock), its record store (record.NewStoreWithClock), its claim
// store (store.Store.UseClock) and its log (clockHandler) read, so everything the daemon's own code
// writes or compares in a replay is the window's time and not the moment the replay ran.
//
// The driver moves it to the moment of each fact it applies (advance). Each read of Now answers a
// moment after the read before it, one microsecond later where the instant has not moved — the
// finest time Postgres keeps — so the writes of one pass keep the order the pass wrote them in, as
// the daemon's wall clock keeps it: an owed start's created_at comes before the release mark the
// same pass writes after it, and the waiting line sorts the two as written (record.Waiting). The
// drift is a few microseconds an instant and never carries past the next fact's moment.
type simClock struct {
	mu sync.Mutex
	// at is the instant the driver last moved the clock to, and last the newest moment Now
	// answered. instants is each instant the clock has stood at, in order (instantOf).
	at, last time.Time
	instants []time.Time
}

// newSimClock is a clock standing at at.
func newSimClock(at time.Time) *simClock {
	return &simClock{at: at.UTC(), instants: []time.Time{at.UTC()}}
}

// instantOf is the instant the clock stood at when it answered t, a moment Now answered: the
// latest instant at or before t, t itself where none is. A replay's signals read a moment its daemon
// or its stub wrote, such as the end of a turn, at the instant of the fact that wrote it, as the
// record's lines give it, never a microsecond of the clock's drift after it.
func (c *simClock) instantOf(t time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := sort.Search(len(c.instants), func(i int) bool { return c.instants[i].After(t) })
	if i == 0 {
		return t
	}
	return c.instants[i-1]
}

// Now is the clock's next moment: the instant it stands at, or one microsecond after the moment it
// last answered where that is not before the instant.
func (c *simClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.at
	if !next.After(c.last) && !c.last.IsZero() {
		next = c.last.Add(time.Microsecond)
	}
	c.last = next
	return next
}

// Instant is the instant the clock stands at, which a log line is stamped with: the moment of the
// fact it was written in, whatever moment the writes of that fact read.
func (c *simClock) Instant() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// advance moves the clock to at, where at is later than the instant it stands at. A clock is
// never moved back: a fact the driver applies late (one held while the daemon was down) is
// applied at the clock's own instant.
func (c *simClock) advance(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.After(c.at) {
		c.at = at.UTC()
		c.instants = append(c.instants, c.at)
	}
}

// lineTap is what the driver reads of the daemon's own lines as the replay writes them: the msgs
// it acts on, each with its attributes, in the order the daemon wrote them. The daemon's code says
// what it did in its lines — a phase changed, an issue left the workflow, a slot was given — and
// the driver reads them there rather than reading the store again after every fact, which is what
// opens a segment no start row begins (accepting's), applies a held sign-off again at a member's
// leave, and counts the issues in flight.
type lineTap struct {
	mu    sync.Mutex
	msgs  map[string]bool
	lines []tappedLine
}

// tappedLine is one line the tap kept: its msg and its attributes by key.
type tappedLine struct {
	msg   string
	attrs map[string]slog.Value
}

// newLineTap taps the lines of msgs.
func newLineTap(msgs ...string) *lineTap {
	tap := &lineTap{msgs: make(map[string]bool, len(msgs))}
	for _, msg := range msgs {
		tap.msgs[msg] = true
	}
	return tap
}

// keep keeps line, a line of a msg the tap reads, for the next take.
func (t *lineTap) keep(line tappedLine) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
}

// take is every line tapped since the last take, in order.
func (t *lineTap) take() []tappedLine {
	t.mu.Lock()
	defer t.mu.Unlock()
	taken := t.lines
	t.lines = nil
	return taken
}

// str is the string attribute key of l, "" where it has none.
func (l tappedLine) str(key string) string {
	if value, ok := l.attrs[key]; ok {
		return value.Resolve().String()
	}
	return ""
}

// unwritten is the msgs of the lines the daemon writes inside a transaction once a pass for each
// candidate it passes over, which a replay leaves out of its log: about a
// million lines a run, which no measure reads, since the trace builds a link wait from events.
var unwritten = map[string]bool{
	waitsOnIssuesLine: true,
}

// clockHandler is the handler of a replay's log: the daemon's JSON handler (inner), with each
// record's time set from the simulated clock, the lines unwritten names left out, and the lines
// the tap, which every handler has, asks for handed to it as they are written.
type clockHandler struct {
	inner slog.Handler
	clock *simClock
	tap   *lineTap
	// attrs is what the logger was given by With, which the tap reads with the record's own.
	attrs []slog.Attr
}

// Enabled is the inner handler's.
func (h *clockHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle stamps r with the clock's instant, taps it, and writes it unless it is a kind left
// unwritten.
func (h *clockHandler) Handle(ctx context.Context, r slog.Record) error {
	r.Time = h.clock.Instant()
	if h.tap.msgs[r.Message] {
		attrs := make(map[string]slog.Value, len(h.attrs)+r.NumAttrs())
		for _, attr := range h.attrs {
			attrs[attr.Key] = attr.Value
		}
		r.Attrs(func(attr slog.Attr) bool {
			attrs[attr.Key] = attr.Value
			return true
		})
		h.tap.keep(tappedLine{msg: r.Message, attrs: attrs})
	}
	if unwritten[r.Message] {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs is the handler with attrs on every record, for the inner handler and the tap alike.
func (h *clockHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &clockHandler{inner: h.inner.WithAttrs(attrs), clock: h.clock, tap: h.tap, attrs: slices.Concat(h.attrs, attrs)}
}

// WithGroup is the handler under group. The tap reads a grouped record's attributes by their own
// keys, which no line it taps nests.
func (h *clockHandler) WithGroup(name string) slog.Handler {
	return &clockHandler{inner: h.inner.WithGroup(name), clock: h.clock, tap: h.tap, attrs: h.attrs}
}
