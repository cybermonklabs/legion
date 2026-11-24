package storetest

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// LargeArguments is the tracer of a pool whose writes a test holds to the kinds of argument a
// released connection counts (store.CountedArgumentBytesForTest): a connection gives back a
// statement's arguments only when the bytes it counts pass store.LargeArgumentBytesForTest, so a
// write that passes a large value of another kind, such as a map pgx marshals for a jsonb column,
// leaves that value on its idle connection. It reads the arguments of every statement the pool
// runs, alone or in a batch, and keeps the most bytes one statement's arguments held under each
// write (Begin) and each statement whose arguments held more than the bound while the bytes the
// release counts in them did not. Traced installs it on a pool.
type LargeArguments struct {
	mu sync.Mutex
	// write is the write running now, which the statements read are put down to.
	write string
	// largest is the most bytes one statement's arguments held, by write.
	largest map[string]int
	// uncounted is each statement whose large arguments the release cannot count.
	uncounted []string
}

// NewLargeArguments is a LargeArguments that has read nothing.
func NewLargeArguments() *LargeArguments {
	return &LargeArguments{largest: map[string]int{}}
}

// Begin puts every statement read from now on down to write.
func (r *LargeArguments) Begin(write string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.write = write
}

// RequireLarge fails t for each of writes none of whose statements held more than
// store.LargeArgumentBytesForTest of arguments: such a write shows nothing of a large one.
func (r *LargeArguments) RequireLarge(t testing.TB, writes ...string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, write := range writes {
		if held := r.largest[write]; held <= store.LargeArgumentBytesForTest {
			t.Errorf("%s passed at most %d bytes of arguments to one statement, want more than %d: it shows nothing of a large write",
				write, held, store.LargeArgumentBytesForTest)
		}
	}
}

// RequireCounted fails t for each statement read whose arguments held more than
// store.LargeArgumentBytesForTest in kinds the release does not count, naming its write, its
// statement and each argument the release did not count.
func (r *LargeArguments) RequireCounted(t testing.TB) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, statement := range r.uncounted {
		t.Errorf("a statement's arguments pass %d bytes in a kind the release does not count: %s",
			store.LargeArgumentBytesForTest, statement)
	}
}

func (r *LargeArguments) read(sql string, args []any) {
	held, counted := 0, 0
	var uncounted []string
	for index, arg := range args {
		bytes := heldBytes(reflect.ValueOf(arg), map[uintptr]struct{}{})
		counts := store.CountedArgumentBytesForTest(arg)
		held += bytes
		counted += counts
		if counts < bytes {
			uncounted = append(uncounted, fmt.Sprintf("argument %d, a %T holding %d bytes", index+1, arg, bytes))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.largest[r.write] = max(r.largest[r.write], held)
	if held > store.LargeArgumentBytesForTest && counted <= store.LargeArgumentBytesForTest {
		r.uncounted = append(r.uncounted, fmt.Sprintf("%s ran %q with %s",
			r.write, statementStart(sql), strings.Join(uncounted, ", ")))
	}
}

// TraceQueryStart implements pgx.QueryTracer.
func (r *LargeArguments) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.read(data.SQL, data.Args)
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer.
func (r *LargeArguments) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TraceBatchStart implements pgx.BatchTracer: it reads each query of the batch on its own, as the
// release judges them.
func (r *LargeArguments) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if data.Batch != nil {
		for _, queued := range data.Batch.QueuedQueries {
			r.read(queued.SQL, queued.Arguments)
		}
	}
	return ctx
}

// TraceBatchQuery implements pgx.BatchTracer.
func (r *LargeArguments) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

// TraceBatchEnd implements pgx.BatchTracer.
func (r *LargeArguments) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// statementStart is the first 120 bytes of sql with its runs of white space made one space,
// enough to find the statement by.
func statementStart(sql string) string {
	start := strings.Join(strings.Fields(sql), " ")
	if len(start) > 120 {
		return start[:120] + "…"
	}
	return start
}

// heldBytes is the bytes of text and binary data value holds, whatever its kind: every string and
// byte slice or array it reaches through pointers, interfaces, slices, arrays, maps and struct
// fields, each pointer read once.
func heldBytes(value reflect.Value, seen map[uintptr]struct{}) int {
	switch value.Kind() {
	case reflect.String:
		return value.Len()
	case reflect.Pointer:
		if value.IsNil() {
			return 0
		}
		if _, read := seen[value.Pointer()]; read {
			return 0
		}
		seen[value.Pointer()] = struct{}{}
		return heldBytes(value.Elem(), seen)
	case reflect.Interface:
		if value.IsNil() {
			return 0
		}
		return heldBytes(value.Elem(), seen)
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return value.Len()
		}
		bytes := 0
		for index := range value.Len() {
			bytes += heldBytes(value.Index(index), seen)
		}
		return bytes
	case reflect.Map:
		bytes := 0
		for entry := value.MapRange(); entry.Next(); {
			bytes += heldBytes(entry.Key(), seen) + heldBytes(entry.Value(), seen)
		}
		return bytes
	case reflect.Struct:
		bytes := 0
		for index := range value.NumField() {
			bytes += heldBytes(value.Field(index), seen)
		}
		return bytes
	}
	return 0
}
