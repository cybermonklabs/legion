package store

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
)

// largeArgumentBytes is the size of a statement's arguments from which its connection gives them
// back when it returns to its pool (argumentReleaser). A connection whose statements stay under it
// keeps at most this much while it is idle, 4 MiB over the shared pool's sharedPoolSize
// connections, and its release costs nothing.
const largeArgumentBytes = 64 << 10

// keptArgumentSlots is the most encoded arguments pgx keeps on a connection after a statement. Its
// builder replaces the slice of them once it holds more than 64 (pgx v5.13.0,
// extended_query_builder.go:102-104), so a statement of more arguments keeps none.
const keptArgumentSlots = 64

// argumentReleaseTimeout bounds the statement that overwrites a connection's kept arguments, which
// its releasing caller waits for. The statement is one round trip, measured well under a
// millisecond; a connection that has not answered it in a second is closed instead.
const argumentReleaseTimeout = time.Second

// largeArgumentsKey is the key in a connection's CustomData under which argumentReleaser records
// how many encoded arguments the connection may keep from a large statement.
const largeArgumentsKey = "dispatch.store.largeArguments"

// argumentReleaser makes a pooled connection give back the encoded arguments of its last large
// statement when it returns to its pool. pgx v5.13.0 keeps them otherwise. Its builder encodes a
// statement's arguments into one buffer and keeps a slice with one pointer into that buffer per
// argument (extended_query_builder.go:118-134). After each statement it resets the builder
// (extended_query_builder.go:96-116, from conn.go:612, 623, 634, 891 and 1083). The reset drops a
// buffer over 256 bytes, but it keeps the slice, cut to length zero, while the slice holds 64 or
// fewer, and the old pointers in it still hold the buffer. Only the next statement that encodes at
// least as many arguments overwrites them all. A statement with no arguments encodes none: Exec
// runs it on the simple protocol (conn.go:524-525). pgxpool keeps an idle connection for 30
// minutes, so each of the shared pool's connections held its last large write that long. CI's
// dispatch-memory job found the server's live heap at rest 14.1% above its level before eight
// runs (396 MiB against 347 MiB), and the growth was in
// pgtype.encodePlanJSONCodecEitherFormatByteSlice.Encode.
//
// Measured on the test Postgres: after a statement with one 8 MiB argument, the live heap stays
// 8 MiB up, and one `select $1::int` brings it back. After a statement of three arguments whose
// second is 8 MiB, `select $1::int` leaves it 8 MiB up, and three NULL arguments bring it back. A
// statement of 100 arguments keeps nothing.
//
// So the tracer marks a connection when a statement, or a query of a batch, has arguments over
// largeArgumentBytes. The mark, in the connection's CustomData, is how many arguments that
// statement had, keptArgumentSlots at most. When the connection is released, the tracer runs one
// statement of that many NULL arguments on it. pgx encodes a NULL as nothing, so the statement
// overwrites every kept pointer and keeps nothing itself. A connection with no mark costs one map
// read at its release.
//
// It is a pgxpool.ReleaseTracer, not the pool's AfterRelease. pgxpool runs a set AfterRelease on a
// goroutine of its own at every release, and takes the connection back only when it returns
// (pgxpool/conn.go:52-66). Every release would start a goroutine and finish late, and Pool.Use
// would count a released connection as still in use. TraceRelease runs at the start of Release, on
// the caller's goroutine (pgxpool/conn.go:28-30), so only a marked connection's release waits: for
// the one round trip of the overwrite, 0.35 ms mean and 1.2 ms p99 over 1000 runs on the test
// Postgres. A connection whose overwrite fails is closed, and Release destroys a closed connection
// (pgxpool/conn.go:32-38) rather than keep its arguments.
//
// Open installs it on the shared pool's configuration (withArgumentRelease). Every pool opened
// from a copy of that configuration has it too: the rooms, room-appends, health, lease, outbox,
// compaction, display-tree-fill and event-horizon pools (Pool.separate) and the process
// registry's pool (processes.Register). The listener pool's connections are hijacked and never
// released, so no overwrite runs on them. A test pool that storetest.Traced opens replaces the
// tracer, so it has no overwrite.
//
// Only arguments whose size the tracer reads cheaply count: strings, byte slices and raw JSON,
// pointers to strings and byte slices, and slices of strings, of pointers to strings or of byte
// slices. A value pgx encodes by marshalling it, such as a struct for a jsonb column, does not
// count, and no write of the store passes one large (api's
// TestNoLargeStoreWritePassesAnArgumentTheReleaseCannotCount).
type argumentReleaser struct{}

// withArgumentRelease returns tracer with argumentReleaser added: argumentReleaser alone when
// tracer is nil, otherwise both through pgx's multitracer.
func withArgumentRelease(tracer pgx.QueryTracer) pgx.QueryTracer {
	if tracer == nil {
		return argumentReleaser{}
	}
	return multitracer.New(tracer, argumentReleaser{})
}

// TraceQueryStart implements pgx.QueryTracer.
func (argumentReleaser) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	markLargeArguments(conn, data.Args)
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer.
func (argumentReleaser) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TraceBatchStart implements pgx.BatchTracer. pgx encodes a batch's queries one after another with
// the same builder, so each query is judged on its own arguments.
func (argumentReleaser) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if data.Batch != nil {
		for _, queued := range data.Batch.QueuedQueries {
			markLargeArguments(conn, queued.Arguments)
		}
	}
	return ctx
}

// TraceBatchQuery implements pgx.BatchTracer.
func (argumentReleaser) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

// TraceBatchEnd implements pgx.BatchTracer.
func (argumentReleaser) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// TraceRelease implements pgxpool.ReleaseTracer: it overwrites a marked connection's kept arguments
// before Release gives the connection back to its pool.
func (argumentReleaser) TraceRelease(_ *pgxpool.Pool, data pgxpool.TraceReleaseData) {
	conn := data.Conn
	custom := conn.PgConn().CustomData()
	slots, marked := custom[largeArgumentsKey].(int)
	if !marked {
		return
	}
	delete(custom, largeArgumentsKey)
	// Release destroys a connection in any of these states (pgxpool/conn.go:32-38), and its
	// arguments go with it.
	if conn.IsClosed() || conn.PgConn().IsBusy() || conn.PgConn().TxStatus() != 'I' {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), argumentReleaseTimeout)
	defer cancel()
	sql, args := overwriteArguments(slots)
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		_ = conn.Close(ctx)
	}
}

// markLargeArguments records on conn how many encoded arguments it may keep once a statement with
// args has run, when args are over largeArgumentBytes. A mark already larger stays.
func markLargeArguments(conn *pgx.Conn, args []any) {
	size := 0
	for _, arg := range args {
		size += cheapArgumentBytes(arg)
	}
	if size <= largeArgumentBytes {
		return
	}
	// args may begin with a pgx.QueryExecMode, which encodes nothing; counting it costs at most
	// one NULL argument more in the overwrite.
	slots := min(len(args), keptArgumentSlots)
	custom := conn.PgConn().CustomData()
	if marked, _ := custom[largeArgumentsKey].(int); marked >= slots {
		return
	}
	custom[largeArgumentsKey] = slots
}

// cheapArgumentBytes is how many bytes arg holds, for the kinds of argument whose size is cheap to
// read, and 0 for every other kind. The tracer runs it on every statement's arguments, so it reads
// no value through reflection, as writebatch.go's argumentBytes, which bounds a write phase, does.
func cheapArgumentBytes(arg any) int {
	switch value := arg.(type) {
	case string:
		return len(value)
	case []byte:
		return len(value)
	case json.RawMessage:
		return len(value)
	case *string:
		if value != nil {
			return len(*value)
		}
	case *[]byte:
		if value != nil {
			return len(*value)
		}
	case []string:
		size := 0
		for _, element := range value {
			size += len(element)
		}
		return size
	// The block-text writes bind their texts so, a NULL where a block has no text.
	case []*string:
		size := 0
		for _, element := range value {
			if element != nil {
				size += len(*element)
			}
		}
		return size
	case [][]byte:
		size := 0
		for _, element := range value {
			size += len(element)
		}
		return size
	}
	return 0
}

// overwriteArguments is the statement that overwrites slots kept arguments, and its arguments:
// slots NULLs after pgx.QueryExecModeExec. That mode sends the statement unnamed, in one round trip,
// so no connection's statement cache keeps one statement per number of slots.
func overwriteArguments(slots int) (string, []any) {
	var sql strings.Builder
	sql.WriteString("select cardinality(array[")
	for slot := 1; slot <= slots; slot++ {
		if slot > 1 {
			sql.WriteByte(',')
		}
		sql.WriteByte('$')
		sql.WriteString(strconv.Itoa(slot))
		sql.WriteString("::int")
	}
	sql.WriteString("])")
	args := make([]any, slots+1)
	args[0] = pgx.QueryExecModeExec
	return sql.String(), args
}
