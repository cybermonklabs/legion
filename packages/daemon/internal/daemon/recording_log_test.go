package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// ---- the daemon's log as a step reads it back ----------------------------------------------------

// loggedRecord is one record the daemon wrote, as a step reads it back: the message, its
// attributes decoded from JSON, and the moment it was written.
type loggedRecord struct {
	Message string
	Attrs   map[string]any
	at      time.Time
}

// recordingLog is the daemon's own log handler with every record also kept in memory, so a step
// can read what the daemon decided and when. The records still go wherever liveProof.log() sends
// them, which is the script's evidence file. A handler the daemon derives with WithAttrs or
// WithGroup is another recordingLog over the derived inner handler, keeping its records on the
// same keptRecords and carrying the derivation's attributes, so a step reads a claim's `claim`
// attribute whether the daemon set it per record or on the logger.
type recordingLog struct {
	*keptRecords
	inner slog.Handler
	attrs []slog.Attr
}

// keptRecords is the records the root handler and every handler derived from it share.
type keptRecords struct {
	mu      sync.Mutex
	records []loggedRecord
}

func newRecordingLog(logger *slog.Logger) *recordingLog {
	return &recordingLog{keptRecords: &keptRecords{}, inner: logger.Handler()}
}

func (l *recordingLog) Enabled(context.Context, slog.Level) bool { return true }

func (l *recordingLog) Handle(ctx context.Context, record slog.Record) error {
	kept := loggedRecord{Message: record.Message, Attrs: map[string]any{}, at: time.Now()}
	for _, attr := range l.attrs {
		kept.Attrs[attr.Key] = decodeAttr(attr.Value)
	}
	record.Attrs(func(attr slog.Attr) bool {
		kept.Attrs[attr.Key] = decodeAttr(attr.Value)
		return true
	})
	l.mu.Lock()
	l.records = append(l.records, kept)
	l.mu.Unlock()
	if l.inner.Enabled(ctx, record.Level) {
		return l.inner.Handle(ctx, record)
	}
	return nil
}

// decodeAttr is one attribute as a step compares it: a number as float64, whatever its Go type, so
// a step's own literal need not match the daemon's; a time as its RFC 3339 text with nanoseconds,
// which time.Parse(time.RFC3339Nano, ...) reads back, where slog.Value.String would give Go's own
// layout; a duration and every other value as the text the log shows.
func decodeAttr(value slog.Value) any {
	switch value.Kind() {
	case slog.KindInt64:
		return float64(value.Int64())
	case slog.KindUint64:
		return float64(value.Uint64())
	case slog.KindFloat64:
		return value.Float64()
	case slog.KindBool:
		return value.Bool()
	case slog.KindTime:
		return value.Time().Format(time.RFC3339Nano)
	case slog.KindString:
		return value.String()
	default:
		return value.String()
	}
}

func (l *recordingLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingLog{keptRecords: l.keptRecords, inner: l.inner.WithAttrs(attrs),
		attrs: append(append([]slog.Attr{}, l.attrs...), attrs...)}
}

func (l *recordingLog) WithGroup(name string) slog.Handler {
	return &recordingLog{keptRecords: l.keptRecords, inner: l.inner.WithGroup(name), attrs: l.attrs}
}

func (l *recordingLog) matching(want func(loggedRecord) bool) []loggedRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var matched []loggedRecord
	for _, kept := range l.records {
		if want(kept) {
			matched = append(matched, kept)
		}
	}
	return matched
}

// relaunchStampOf is the relaunch an R6 controller line names (its relaunchedAt, the claim's
// LastRelaunch as the daemon stamped it), read back as recordingLog keeps it; the zero time where
// the line carries none or carries it in a form time.Parse(time.RFC3339Nano, ...) does not read.
func relaunchStampOf(rec loggedRecord) time.Time {
	text, _ := rec.Attrs["relaunchedAt"].(string)
	stamped, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}
	}
	return stamped
}
