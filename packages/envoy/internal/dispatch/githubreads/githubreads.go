// Package githubreads is the record of each GitHub read Dispatch makes under a lease, which
// GET /healthz answers: for each read, whether its newest attempt succeeded, when it ended and the
// first failure it met.
//
// An attempt is one pass a loop runs under its lease (duty.Exclusive). It travels on the pass's
// context (Begin), so the GitHub client, which sends every request the pass makes, marks each one
// sent and notes the answers no caller takes as data (githubapp's Client), and the loop notes the
// failures only it can tell from data at the points where it gives part of the read up. The loop
// records the attempt when its pass ends (Finish), and each process keeps a copy of the record
// (Copy) that the probe reads, so the probe sends nothing to the database.
//
// It imports the standard library and internal/dispatch/text alone, so the GitHub client can
// import it, and it reads nothing from the environment.
package githubreads

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Read names one of the GitHub reads the record holds, as the github_read_attempts table and
// GET /healthz spell it.
type Read string

// The four reads, one for each loop that reads GitHub under a lease.
const (
	WebhookRedelivery  Read = "webhook_redelivery"
	ArchitectureImport Read = "architecture_import"
	DeliveryReconcile  Read = "delivery_reconcile"
	CIReconcile        Read = "ci_reconcile"
)

// Reads returns the four reads in the order GET /healthz names them.
func Reads() []Read {
	return []Read{WebhookRedelivery, ArchitectureImport, DeliveryReconcile, CIReconcile}
}

// CauseLimit is the most bytes a recorded cause holds, which the table's check holds it to too. A
// GitHub error can carry a whole response body, and a health answer that grows with it would be
// a probe whose cost no reader can bound.
const CauseLimit = 1024

// Channel is the Postgres channel the table's trigger notifies with a read's name each time a row
// is written, so every process refreshes its copy of the record (Copy) when any process records
// an attempt.
const Channel = "dispatch_github_read_attempts"

// recordTimeout bounds Finish's write and loadTimeout each read of the record Copy makes: one
// statement each, which a healthy database answers in milliseconds.
const (
	recordTimeout = 5 * time.Second
	loadTimeout   = 5 * time.Second
)

// Attempt is what one leased pass noted: whether it sent any request to GitHub, and the first
// failure noted on it. It is safe for concurrent use, since a pass can send requests from several
// goroutines at once.
type Attempt struct {
	mu     sync.Mutex
	sent   bool
	failed bool
	cause  string
}

// Held is a held scope of an attempt (Hold): the failures noted on its context wait in it until
// its caller keeps them (Keep) or drops them (Drop). A caller that asks again, or asks another
// way, within its pass runs each try in one, because only the outcome it ends with counts, and
// Reached tells it whether a try has an outcome at all.
//
// Only the first failure a scope holds can become a cause, since the attempt keeps the first
// failure that reaches it and a scope passes its notes on in the order they arrived; so a scope
// holds that one and forgets the rest.
//
// A nil *Held is the scope of a context that carries no attempt, and Keep and Drop do nothing on
// it.
type Held struct {
	attempt *Attempt
	// parent is the enclosing held scope, nil when the attempt itself encloses this one.
	parent *Held
	// reached is whether a request was sent in the scope or in one it encloses (NoteSent), or a
	// failure reached it (Reached). It is apart from mu because NoteSent marks every scope a
	// request is sent in on each request a pass sends, and takes no scope's lock for it.
	reached atomic.Bool

	mu sync.Mutex
	// settled is whether Keep or Drop has run, and kept which of the two. A note that arrives
	// after Keep goes straight on to the enclosing scope, and one after Drop is forgotten.
	settled bool
	kept    bool
	failed  bool
	cause   string
}

// frame is what a context carries: the attempt, and the innermost held scope, nil outside any.
type frame struct {
	attempt *Attempt
	held    *Held
}

// frameKey is the context key of the frame.
type frameKey struct{}

// Begin returns ctx carrying a new Attempt, and the Attempt.
func Begin(ctx context.Context) (context.Context, *Attempt) {
	attempt := &Attempt{}
	return context.WithValue(ctx, frameKey{}, frame{attempt: attempt}), attempt
}

// NoteSent marks the attempt ctx carries as having sent a request to GitHub, and every held scope
// ctx carries, the innermost and each that encloses it, as having reached GitHub (Held.Reached).
// Inside a held scope it still marks the attempt at once, since a request sent is sent whatever
// its caller then makes of the answer. It does nothing on a ctx carrying no attempt.
func NoteSent(ctx context.Context) {
	current, ok := ctx.Value(frameKey{}).(frame)
	if !ok {
		return
	}
	current.attempt.mu.Lock()
	current.attempt.sent = true
	current.attempt.mu.Unlock()
	for held := current.held; held != nil; held = held.parent {
		held.reached.Store(true)
	}
}

// NoteFailure notes err, by its Error() text made a Cause, as a failure on the attempt ctx
// carries, or holds it in the innermost held scope ctx carries. It does nothing for a nil err, on
// a ctx carrying no attempt, or once the attempt holds its first failure, and makes no text then:
// only that first failure is ever the cause, whatever scope a later note waits in, and an error's
// text can carry a whole response body, so a request outside the leased passes, and every note
// after the cause, cost nothing here. A held scope makes the text of the first note it holds
// while unsettled, before it knows whether it will keep it, and of each note that arrives after
// Keep; it makes none for a later note while unsettled or for any note after Drop.
func NoteFailure(ctx context.Context, err error) {
	if err == nil {
		return
	}
	if current, ok := unfailed(ctx); ok {
		current.note(err)
	}
}

// NoteFailuref is NoteFailure of fmt.Errorf(format, args...), which it formats only where the
// note can still become the cause: on a ctx carrying an attempt that holds no failure yet. A loop
// notes with it what it gives up, worded with the error it gave up on, at points a request outside
// the leased passes reaches too.
func NoteFailuref(ctx context.Context, format string, args ...any) {
	if current, ok := unfailed(ctx); ok {
		current.note(fmt.Errorf(format, args...))
	}
}

// unfailed returns the frame ctx carries while its attempt holds no failure: a note made after
// the attempt's first failure can never become its cause, in a held scope or out of one.
func unfailed(ctx context.Context) (frame, bool) {
	current, ok := ctx.Value(frameKey{}).(frame)
	if !ok {
		return frame{}, false
	}
	if _, failed, _ := current.attempt.Outcome(); failed {
		return frame{}, false
	}
	return current, true
}

// note holds err in the innermost held scope, or notes it on the attempt outside any.
func (f frame) note(err error) {
	if f.held != nil {
		f.held.note(err)
		return
	}
	f.attempt.fail(Cause(err.Error()))
}

// fail keeps cause as the attempt's when it is the first failure to reach it.
func (a *Attempt) fail(cause string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return
	}
	a.failed = true
	a.cause = cause
}

// Hold returns a child of ctx whose failure notes wait in the returned Held until its caller
// calls Keep or Drop. Held scopes nest: a note goes to the innermost one, and Keep passes it on
// to the scope that encloses that one, or to the attempt. On a ctx carrying no attempt it returns
// ctx itself and a nil *Held.
func Hold(ctx context.Context) (context.Context, *Held) {
	current, ok := ctx.Value(frameKey{}).(frame)
	if !ok {
		return ctx, nil
	}
	held := &Held{attempt: current.attempt, parent: current.held}
	return context.WithValue(ctx, frameKey{}, frame{attempt: current.attempt, held: held}), held
}

// note holds err's cause while the scope is unsettled and holds none yet, passes it on once the
// scope was kept, and forgets it otherwise, making the cause's text only in the first two cases.
func (h *Held) note(err error) {
	h.reached.Store(true)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case !h.settled:
		if !h.failed {
			h.failed = true
			h.cause = Cause(err.Error())
		}
	case h.kept:
		h.passOn(Cause(err.Error()))
	}
}

// fail holds cause, a note an inner scope kept, while the scope is unsettled, passes it on once
// the scope was kept, and forgets it once the scope was dropped.
func (h *Held) fail(cause string) {
	h.reached.Store(true)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case !h.settled:
		if !h.failed {
			h.failed = true
			h.cause = cause
		}
	case h.kept:
		h.passOn(cause)
	}
}

// passOn hands cause to the enclosing scope, or to the attempt. h.mu is held, so the notes of
// one scope pass on in the order they arrived; an inner scope's lock is always taken before an
// outer one's, so two scopes never wait on each other.
func (h *Held) passOn(cause string) {
	if h.parent != nil {
		h.parent.fail(cause)
		return
	}
	h.attempt.fail(cause)
}

// Keep passes the held notes on, in order, to the enclosing scope or the attempt, and every note
// that arrives after it as well. A second Keep or Drop does nothing.
func (h *Held) Keep() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.settled {
		return
	}
	h.settled = true
	h.kept = true
	if h.failed {
		h.passOn(h.cause)
	}
	h.failed = false
	h.cause = ""
}

// Drop forgets the held notes, and every note that arrives after it. A second Keep or Drop does
// nothing.
func (h *Held) Drop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.settled {
		return
	}
	h.settled = true
	h.failed = false
	h.cause = ""
}

// Reached reports whether the try the scope holds reached GitHub: a request was sent in it or in
// a scope it encloses (NoteSent), or a failure reached it, noted in it or kept by a scope it
// encloses. A failure counts with no request sent because the client notes a limit
// github_rate_limits holds without sending anything, and that limit is GitHub's own answer,
// recorded: a try it held has an outcome of its own as much as a try GitHub answered. A try that
// reached nothing holds nothing and says nothing of GitHub, so a caller that asks again settles
// an earlier try's scope by the next try that reached GitHub, dropping its notes then, and keeps
// them when no later try did (the architecture import's flight). A note made once the attempt
// holds its first failure reaches no scope (NoteFailure), so Reached does not count it, and by
// then the cause is settled whatever the caller keeps. A nil scope reached nothing.
func (h *Held) Reached() bool {
	return h != nil && h.reached.Load()
}

// Outcome reports whether any request was sent, whether a failure reached the attempt, and the
// first failure's cause (already made with Cause). A nil attempt sent nothing and noted nothing.
func (a *Attempt) Outcome() (sent, failed bool, cause string) {
	if a == nil {
		return false, false, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sent, a.failed, a.cause
}

// Cause is value made storable and cut to CauseLimit bytes. The text is made storable first
// (text.StorableReplacement: invalid UTF-8 and U+0000 become U+FFFD, which a Postgres text column
// needs), then cut at a whole character (text.HeadBytes) and ended with
// " … (cut from <n> bytes)", n being the storable text's length, inside the limit, so a reader
// sees that it was cut and by how much.
func Cause(value string) string {
	value = text.StorableReplacement(value)
	if len(value) <= CauseLimit {
		return value
	}
	marker := " … (cut from " + strconv.Itoa(len(value)) + " bytes)"
	head, _ := text.HeadBytes(value, CauseLimit-len(marker))
	return head + marker
}

// Recorder writes a read's newest attempt: whether it succeeded, and the cause of its first
// failure, "" when it succeeded. The store's GitHubReadAttempts.Record is the one in production.
type Recorder func(ctx context.Context, read Read, succeeded bool, cause string) error

// Finish records attempt's outcome through record, at the end of the pass that carried it.
//
// It records nothing when record is nil (a loop a test builds without one), when caller has ended
// (the context duty.Exclusive handed the pass: this process's shutdown cut the pass short, so
// the pass did not read what it set out to), or when the attempt sent nothing and noted no
// failure (the pass had nothing to read, and an attempt that asked GitHub nothing says nothing
// about it). Otherwise it records failed with the first cause, or succeeded.
//
// The write runs on caller's values without its cancel, bounded by recordTimeout, because a pass
// can end on its own deadline and its outcome is still worth recording. A failed write is logged
// at ERROR on logger, or slog.Default() when it is nil, and changes nothing else of the pass.
func Finish(caller context.Context, read Read, attempt *Attempt, record Recorder, logger *slog.Logger) {
	if record == nil || caller.Err() != nil {
		return
	}
	sent, failed, cause := attempt.Outcome()
	if !sent && !failed {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(caller), recordTimeout)
	defer cancel()
	if err := record(ctx, read, !failed, cause); err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("dispatch: the newest attempt of a GitHub read was not recorded; GET /healthz names the one before it", "read", read, "error", err)
	}
}

// Recorded is one row of the record: a read's newest attempt.
type Recorded struct {
	Read Read
	// At is when the attempt was recorded, by the database's clock.
	At        time.Time
	Succeeded bool
	// Cause is the first failure's text, "" when Succeeded.
	Cause string
}

// Copy is this process's copy of the record, which GET /healthz reads so that the probe sends
// nothing to the database and a failure of that read cannot change its answer. Run keeps it; its
// zero value holds nothing until Run's first load succeeds.
type Copy struct {
	rows atomic.Pointer[map[Read]Recorded]
}

// Run loads the record at start and again after each wake, until ctx ends. wake is the
// notification of Channel (pglisten.Hub.Wake signals it after each notification and after each
// LISTEN that succeeds, so a write made while the hub had no connection is caught up on). Each
// load is bounded by loadTimeout. A failed load is logged at WARN on logger, or slog.Default()
// when it is nil, and keeps the copy the last good load made.
func (c *Copy) Run(ctx context.Context, load func(context.Context) ([]Recorded, error), wake <-chan struct{}, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	for {
		c.load(ctx, load, logger)
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
	}
}

// load reads the record once and keeps it. A load the ending of ctx cut short is not logged: it
// is the process stopping, not the database failing.
func (c *Copy) load(ctx context.Context, load func(context.Context) ([]Recorded, error), logger *slog.Logger) {
	bounded, cancel := context.WithTimeout(ctx, loadTimeout)
	defer cancel()
	recorded, err := load(bounded)
	if err != nil {
		if ctx.Err() == nil {
			logger.Warn("dispatch: the record of GitHub reads was not read; GET /healthz keeps the copy it has", "error", err)
		}
		return
	}
	rows := make(map[Read]Recorded, len(recorded))
	for _, row := range recorded {
		rows[row.Read] = row
	}
	c.rows.Store(&rows)
}

// Rows returns the rows of the last good load, keyed by read, and whether a load has succeeded
// yet. The map is shared with every other reader and must not be changed.
func (c *Copy) Rows() (rows map[Read]Recorded, loaded bool) {
	held := c.rows.Load()
	if held == nil {
		return nil, false
	}
	return *held, true
}
