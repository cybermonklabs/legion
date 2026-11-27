package api

import (
	"context"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// requestStatements counts every statement sent on a context that carries requestStatementsKey,
// one by one and in batches alike, so a test counts one request's own statements; visibility counts
// those of them that are the rule of references' read (seenReferencesSQL).
type requestStatements struct{ sent, visibility atomic.Int64 }

type requestStatementsKey struct{}

func (c *requestStatements) count(ctx context.Context, sql string) {
	if ctx.Value(requestStatementsKey{}) == nil {
		return
	}
	c.sent.Add(1)
	if sql == seenReferencesSQL {
		c.visibility.Add(1)
	}
}

func (c *requestStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.count(ctx, data.SQL)
	return ctx
}

func (c *requestStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *requestStatements) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (c *requestStatements) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	c.count(ctx, data.SQL)
}

func (c *requestStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
