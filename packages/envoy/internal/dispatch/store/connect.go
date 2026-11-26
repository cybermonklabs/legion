package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Connect opens one connection of its own to the database databaseURL names, outside any pool,
// named applicationName in pg_stat_activity, so an operator reading the activity of a database
// sees which pass holds it. It migrates nothing: an operator pass that holds one connection - the
// store census (graphcensus), the census's snapshots of what only docs reads and the version check
// (CheckVersions) - works on the schema the deployment's own server migrated.
func Connect(ctx context.Context, databaseURL, applicationName string) (*pgx.Conn, error) {
	config, err := connConfig(databaseURL, applicationName)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return conn, nil
}

// connConfig is the configuration of one connection of its own to the database databaseURL names:
// named applicationName in pg_stat_activity, and carrying the writer mark every connection this
// build opens to Dispatch's database carries (markWriter).
func connConfig(databaseURL, applicationName string) (*pgx.ConnConfig, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Postgres URL: %w", err)
	}
	config.RuntimeParams["application_name"] = applicationName
	markWriter(config)
	return config, nil
}
