package intake

import (
	"context"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// Reader is a handler that reads what a fact needs before the fact's transaction. Such a read may
// wait. Inside the transaction it would hold the fact lock (SerializeFacts), the pool's turn and a
// connection while it waits.
//
// ApplyFact calls ReadFact once per fact, before it takes the pool's turn, on the fact as the store
// keeps it. ReadFact answers ctx, or a context derived from it that carries what it read. ApplyFact
// then applies the fact on that context. pool is the pool the fact applies on. A read of the store
// opens its own transaction there, and closes it before any wait.
//
// A fact applied inside a transaction already open on ctx (store.CommitScoped) is read by no
// Reader. A read there would run inside that transaction and its fact lock. Such a fact is applied
// with nothing read first.
type Reader interface {
	ReadFact(ctx context.Context, pool store.TxPool, fact Fact) context.Context
}

// readFact runs ReadFact of each of handlers that is a Reader, in their order. Each runs on the
// context the one before it answered, and readFact answers the last.
func readFact(ctx context.Context, pool store.TxPool, fact Fact, handlers []Handler) context.Context {
	for _, handler := range handlers {
		if reader, ok := handler.(Reader); ok {
			ctx = reader.ReadFact(ctx, pool, fact)
		}
	}
	return ctx
}
