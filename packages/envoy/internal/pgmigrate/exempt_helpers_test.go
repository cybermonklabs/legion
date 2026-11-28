package pgmigrate

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// tableSizeExemption classifies sql as Census does, with a classifier of its own that keeps no
// catalogRelation answer, so each test reads every name.
func tableSizeExemption(ctx context.Context, census, catalog pgx.Tx, sql string) (classification, error) {
	c := &classifier{ctx: ctx, census: census, catalog: catalog}
	return c.tableSizeExemption(sql)
}
