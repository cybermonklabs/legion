package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
)

// requestTimer is the pool tracer that adds what a request spends in the database to that
// request's access-log line (accesslog.Timing), so an operator can split a slow request's
// elapsed_ms: each statement's time from its send to its end, and how many statements it sent, as
// db_ms and db_calls, and each wait for a pooled connection as db_wait_ms. pgx hands it the
// context its caller passed, so a statement run on a request's context adds to that request's
// line, and one run on another context (the server's lifetime, a background worker's) adds
// nothing and costs one read of that context's values.
//
// pgx ends a statement where its caller is done with it: an Exec when it answers, a Query when its
// rows close, so the caller's reading and scanning of the rows is inside its time, a batch when its
// results close, and a copy when it ends. A statement pgx prepares first (its statement cache) is
// prepared inside that span, so a prepare is never counted on its own, while a transaction's begin
// and commit are statements of their own. A batch counts each of its queries. A wait for a
// connection runs from the start of the pool's Acquire to the connection it hands over, so it
// takes in the time to open one when the pool opens one for it. The overwrite argumentReleaser
// runs at a release runs on a context of its own and is counted nowhere.
//
// Open installs it on the shared pool's configuration (withRequestTime), and every pool opened from
// a copy of that configuration has it too (Pool.separate, processes.Register). A test pool that
// storetest.Traced opens replaces the tracer, so its requests' lines count nothing.
type requestTimer struct{}

// withRequestTime returns tracer with requestTimer ahead of it, so a request's statement is timed
// from before any other tracer's start: requestTimer alone when tracer is nil, otherwise both
// through pgx's multitracer.
func withRequestTime(tracer pgx.QueryTracer) pgx.QueryTracer {
	if tracer == nil {
		return requestTimer{}
	}
	return multitracer.New(requestTimer{}, tracer)
}

// timedSpan is the context a request's statement, batch, copy or acquire runs under: the context
// it was started with, the request's Timing, when it started and how many statements it sends. It
// is one allocation, made only for a request's context, and its end finds it among the context's
// values (timedSpanOf).
type timedSpan struct {
	context.Context
	timing     *accesslog.Timing
	started    time.Time
	statements int
}

// timedSpanKey is the value key under which a timedSpan answers for itself.
type timedSpanKey struct{}

// Value answers the span itself for timedSpanKey and the context it was started with for every
// other key.
func (s *timedSpan) Value(key any) any {
	if key == (timedSpanKey{}) {
		return s
	}
	return s.Context.Value(key)
}

// startTimed is ctx as a timedSpan of statements statements started now, or ctx itself when it
// carries no request's line.
func startTimed(ctx context.Context, statements int) context.Context {
	timing := accesslog.TimingOf(ctx)
	if timing == nil {
		return ctx
	}
	return &timedSpan{Context: ctx, timing: timing, started: time.Now(), statements: statements}
}

// timedSpanOf is the timedSpan startTimed made ctx, nil when it made none.
func timedSpanOf(ctx context.Context) *timedSpan {
	span, _ := ctx.Value(timedSpanKey{}).(*timedSpan)
	return span
}

// endStatements adds the statements of the span ctx is, and the time since they were sent, to its
// request's line.
func endStatements(ctx context.Context) {
	if span := timedSpanOf(ctx); span != nil {
		span.timing.AddDatabase(time.Since(span.started), span.statements)
	}
}

// TraceQueryStart implements pgx.QueryTracer: Query, QueryRow and Exec.
func (requestTimer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return startTimed(ctx, 1)
}

// TraceQueryEnd implements pgx.QueryTracer.
func (requestTimer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	endStatements(ctx)
}

// TraceBatchStart implements pgx.BatchTracer: the batch is timed whole and counts its queries.
func (requestTimer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	statements := 0
	if data.Batch != nil {
		statements = data.Batch.Len()
	}
	return startTimed(ctx, statements)
}

// TraceBatchQuery implements pgx.BatchTracer.
func (requestTimer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

// TraceBatchEnd implements pgx.BatchTracer.
func (requestTimer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchEndData) {
	endStatements(ctx)
}

// TraceCopyFromStart implements pgx.CopyFromTracer.
func (requestTimer) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceCopyFromStartData) context.Context {
	return startTimed(ctx, 1)
}

// TraceCopyFromEnd implements pgx.CopyFromTracer.
func (requestTimer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceCopyFromEndData) {
	endStatements(ctx)
}

// TraceAcquireStart implements pgxpool.AcquireTracer.
func (requestTimer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return startTimed(ctx, 0)
}

// TraceAcquireEnd implements pgxpool.AcquireTracer: a failed acquire's wait counts too.
func (requestTimer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireEndData) {
	if span := timedSpanOf(ctx); span != nil {
		span.timing.AddPoolWait(time.Since(span.started))
	}
}
