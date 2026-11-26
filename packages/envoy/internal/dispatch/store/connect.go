package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/redact"
)

// Connect opens one connection of its own to the database databaseURL names, outside any pool,
// named applicationName in pg_stat_activity, so an operator reading the activity of a database
// sees which pass holds it. It migrates nothing: an operator pass that holds one connection - the
// store census (graphcensus) - works on the schema the deployment's own server migrated. A URL pgx
// cannot read is refused in fixed words (redact.Masked): pgx's ParseConfigError quotes the URL
// with its password masked but its user name and every other query pair kept, and the error it
// wraps can quote a query key alone.
func Connect(ctx context.Context, databaseURL, applicationName string) (*pgx.Conn, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Postgres URL: %w", redact.Masked(err, "pgx cannot read its connection settings"))
	}
	config.RuntimeParams["application_name"] = applicationName
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return conn, nil
}
