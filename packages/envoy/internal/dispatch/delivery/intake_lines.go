package delivery

import (
	"context"
	"log/slog"
	"sync"

	natsgo "github.com/nats-io/nats.go"
)

// deliveryLines is the log lines one delivery of the intake writes, its callees' included, held
// until the delivery knows how it ends (Intake.deliver). A delivery hands its logger to its
// callees on its context (logging.NewContext), and every line on the way to its answer is written
// through the logger that context carries (logging.FromContext): the intake's own, the head
// read's, the attribution's and the GitHub client's. Once the delivery knows its answer, and
// before it gives NATS that answer, its lines are written to the intake's logger (to,
// intakeTuning.log) in the order they were held: each at its own level, except where the delivery
// hands its envelope back outside the hand-back's cadence (handBackLineDue), where each is written
// at DEBUG. So a reader that sees the answer finds the lines in the log. A handed-back envelope is
// delivered every headWriteRedeliverAfter for as long as its fault lasts, so a line written at each
// delivery would be one line every 10 seconds for each of up to intakeMaxAckPending envelopes for
// up to the 72 hours of the bus; the read of the owed read's revision wrote a WARN line at each
// such delivery while pull_request_reads_owed was locked, 22 WARN lines over 21 deliveries in
// 330 s (acceptance of cybermonklabs/legion#4209 at def907052c, D9). A line written after the
// delivery's lines were written, the answer's own among them, goes straight to the intake's logger
// under the same decision, at DEBUG where the delivery's lines were: a fetch of GitHub that
// several callers share outlives its first caller (githubapp.Client.RepositoryToken runs it on
// context.WithoutCancel), so its line can come once that delivery has answered (the quality audit
// of cybermonklabs/legion#4209 at 3a21edb2c9). No line is lost.
type deliveryLines struct {
	// to is the handler of the logger the lines are written to: the intake's (intakeTuning.log).
	to      slog.Handler
	mu      sync.Mutex
	held    []heldLine
	written bool
	// quiet is write's decision, which a line written after it keeps.
	quiet bool
}

// newDeliveryLines is the held lines of one delivery whose lines are written to to, the intake's
// logger.
func newDeliveryLines(to *slog.Logger) *deliveryLines {
	return &deliveryLines{to: to.Handler()}
}

// heldLine is one record a delivery wrote, and the attributes and groups its logger added
// (heldHandler.with).
type heldLine struct {
	with   func(slog.Handler) slog.Handler
	record slog.Record
}

// logger is the logger a delivery writes its lines through.
func (l *deliveryLines) logger() *slog.Logger {
	return slog.New(heldHandler{lines: l})
}

// write writes every held line to the intake logger's handler, in the order they were written, at
// DEBUG where quiet and at its own level otherwise, and sends every later line straight there
// under the same decision.
func (l *deliveryLines) write(quiet bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.written, l.quiet = true, quiet
	for _, line := range l.held {
		_ = l.emit(context.Background(), line.with, line.record)
	}
	l.held = nil
}

// emit writes record through with to the intake logger's handler, at DEBUG where the delivery is
// quiet, and drops it where that handler takes no line of the level it is written at.
func (l *deliveryLines) emit(ctx context.Context, with func(slog.Handler) slog.Handler, record slog.Record) error {
	if l.quiet && record.Level > slog.LevelDebug {
		record.Level = slog.LevelDebug
	}
	handler := l.to
	if with != nil {
		handler = with(handler)
	}
	if !handler.Enabled(ctx, record.Level) {
		return nil
	}
	return handler.Handle(ctx, record)
}

// heldHandler is the slog.Handler of a delivery's logger (deliveryLines). It is enabled where the
// intake's logger is: a line is written at its own level or lower, never higher, so a level the
// intake's logger drops is dropped at once. with is the attributes and groups the logger added.
type heldHandler struct {
	lines *deliveryLines
	with  func(slog.Handler) slog.Handler
}

func (h heldHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.lines.to.Enabled(ctx, level)
}

func (h heldHandler) Handle(ctx context.Context, record slog.Record) error {
	h.lines.mu.Lock()
	defer h.lines.mu.Unlock()
	if h.lines.written {
		return h.lines.emit(ctx, h.with, record)
	}
	h.lines.held = append(h.lines.held, heldLine{with: h.with, record: record.Clone()})
	return nil
}

func (h heldHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.then(func(handler slog.Handler) slog.Handler { return handler.WithAttrs(attrs) })
}

func (h heldHandler) WithGroup(name string) slog.Handler {
	return h.then(func(handler slog.Handler) slog.Handler { return handler.WithGroup(name) })
}

// then is h with step applied after the attributes and groups h adds.
func (h heldHandler) then(step func(slog.Handler) slog.Handler) heldHandler {
	before := h.with
	return heldHandler{lines: h.lines, with: func(handler slog.Handler) slog.Handler {
		if before != nil {
			handler = before(handler)
		}
		return step(handler)
	}}
}

// handBackLineEvery is how many deliveries of one handed-back envelope write their lines at their
// own levels once: about every 5 minutes at headWriteRedeliverAfter.
const handBackLineEvery = 30

// handBackLineDue answers how many times msg has been delivered, and whether a delivery of it that
// hands it back writes its lines at their own levels: at its first delivery and once every
// handBackLineEvery deliveries after it, the 1st, the 31st, the 61st, and at a delivery whose
// count cannot be read (msg.Metadata). Every other such delivery writes them at DEBUG
// (deliveryLines). The count decides the lines' level and nothing else.
func handBackLineDue(msg *natsgo.Msg) (delivered uint64, due bool) {
	if meta, err := msg.Metadata(); err == nil {
		delivered = meta.NumDelivered
	}
	return delivered, delivered%handBackLineEvery == 1 || delivered == 0
}
