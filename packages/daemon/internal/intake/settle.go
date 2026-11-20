package intake

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Settler is a handler that holds a rule over a whole fact, decided once every handler has
// applied it: the end of each step's run the fact took an issue out of is the workflow's
// (workflow.Engine). OpenFact answers ctx carrying what the settler keeps of the fact while the
// handlers apply it, and SettleFact decides the rule over the record they left, in the fact's
// own transaction, before it commits. Each runs once per fact for each settler: a settler that
// shares its keeping with another (admission's engine) answers the same ctx, and settles once.
type Settler interface {
	OpenFact(ctx context.Context) context.Context
	SettleFact(ctx context.Context, tx pgx.Tx) error
}

// OpenSettlers opens what each of handlers that is a Settler keeps of one fact, and answers ctx
// carrying it with the settle that runs every one of them, which the caller runs after the last
// handler in the fact's transaction: ApplyFact for each fact, and the daemon's boot reconcile for
// the pass it applies in a transaction of its own.
func OpenSettlers(ctx context.Context, handlers ...Handler) (context.Context, func(context.Context, pgx.Tx) error) {
	var settlers []Settler
	for _, handler := range handlers {
		if settler, ok := handler.(Settler); ok {
			ctx = settler.OpenFact(ctx)
			settlers = append(settlers, settler)
		}
	}
	return ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, settler := range settlers {
			if err := settler.SettleFact(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	}
}
