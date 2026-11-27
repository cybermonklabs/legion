package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

// existenceTrace keeps every statement one recorded request (recordingKey) issued, each with the
// command tag it ended on: its SQL, and how many rows it read, locked or wrote ("SELECT 1",
// "UPDATE 0"). A call that names a hidden object must issue, row count for row count, the
// statements the call naming an object nobody holds issues: a read or a row lock the hidden call
// makes before its check of what the caller sees finds a row the other call does not, and a
// statement it makes after that read is one the other call never reaches. It records on the
// request's own context alone, so the background work of a running server is no part of the
// record.
type existenceTrace struct {
	mu         sync.Mutex
	statements []string
	// generation counts the takes, so a statement that ends after the record it began in was
	// taken fills nothing.
	generation int
}

// existenceEntry is the entry a statement's start leaves on its context, which its end fills.
type existenceEntry struct{ generation, index int }

type existenceEntryKey struct{}

func (r *existenceTrace) start(ctx context.Context, sql string) context.Context {
	if ctx.Value(recordingKey{}) == nil {
		return ctx
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, oneLine(sql))
	return context.WithValue(ctx, existenceEntryKey{}, existenceEntry{generation: r.generation, index: len(r.statements) - 1})
}

func (r *existenceTrace) end(ctx context.Context, tag string, err error) {
	entry, ok := ctx.Value(existenceEntryKey{}).(existenceEntry)
	if !ok {
		return
	}
	outcome := tag
	if err != nil {
		outcome = "error"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry.generation == r.generation && entry.index < len(r.statements) {
		r.statements[entry.index] += " => " + outcome
	}
}

func (r *existenceTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return r.start(ctx, data.SQL)
}

func (r *existenceTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	r.end(ctx, data.CommandTag.String(), data.Err)
}

func (r *existenceTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (r *existenceTrace) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if ctx.Value(recordingKey{}) == nil {
		return
	}
	outcome := data.CommandTag.String()
	if data.Err != nil {
		outcome = "error"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, oneLine(data.SQL)+" => "+outcome)
}

func (r *existenceTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// take answers what the trace holds and empties it.
func (r *existenceTrace) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	taken := r.statements
	r.statements = nil
	r.generation++
	return taken
}

// walkListenerCalls keeps every request the walk's fake Envoy listener was sent, as "METHOD path",
// in the order it was sent them. It is not listenerCalls (listener_takeover_test.go), the count a
// stand-in listener keeps of the calls it serves and holds.
type walkListenerCalls struct {
	mu    sync.Mutex
	calls []string
}

func (c *walkListenerCalls) add(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r.Method+" "+r.URL.RequestURI())
}

// take answers the calls held and empties the record.
func (c *walkListenerCalls) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	taken := c.calls
	c.calls = nil
	return taken
}

// rewritten is each of values with every key of written replaced by its value, longest key first,
// so a record of the call naming nothing reads as the call naming the hidden object would have
// read had it been answered as missing.
func rewritten(values []string, written map[string]string) []string {
	keys := make([]string, 0, len(written))
	for key := range written {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(left, right string) int { return len(right) - len(left) })
	out := make([]string, len(values))
	for index, value := range values {
		for _, key := range keys {
			value = strings.ReplaceAll(value, key, written[key])
		}
		out[index] = value
	}
	return out
}

// firstDifference names where two records part: the index of the first entry that differs, and
// each record's entry there ("(none)" past a record's end).
func firstDifference(hidden, missing []string) string {
	at := func(record []string, index int) string {
		if index < len(record) {
			return record[index]
		}
		return "(none)"
	}
	for index := range max(len(hidden), len(missing)) {
		if at(hidden, index) != at(missing, index) {
			return fmt.Sprintf("at %d of %d and %d: the hidden call %s; the call naming nothing %s",
				index, len(hidden), len(missing), at(hidden, index), at(missing, index))
		}
	}
	return ""
}
