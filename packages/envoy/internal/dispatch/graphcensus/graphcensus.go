// Package graphcensus holds the two operator passes over the node and tuple store: the census,
// which reads the store against the tables that project it and refuses when the two disagree,
// and the backfill, which writes the nodes and tuples of the rows written before the store
// existed.
//
// A node is a thing Dispatch holds and a tuple is one fact stated about it
// (store/migrations/20261109013400_graph_store.up.sql); every table Dispatch reads today stays
// where it is and becomes the projection of those two
// (store/migrations/20261109022248_graph_projections.up.sql). The census is what says the
// projection and the store still agree, and it is the gate the backfill is measured by: a kind
// is cut over (graph_store_cutover) once every row of it was written beside its node, and the
// census is how anyone knows that is still true.
//
// The census writes nothing and reads one read-only snapshot. The backfill writes through
// graphstore, the one package in the tree that writes graph_nodes and graph_tuples.
package graphcensus

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applicationName is how this package's own connection appears in pg_stat_activity: an operator
// reading the activity of a database the census is being taken of sees which pass holds it.
const applicationName = "envoy-dispatch census --store"

// connect opens the census's one connection, named in pg_stat_activity. The census uses no
// pool and migrates nothing: it is taken before a deployment rolls, exactly as the pre-deploy
// census is (store.Census). The backfill writes and holds the server's own pool instead
// (cmd/dispatch's openMigrated).
func connect(ctx context.Context, databaseURL, name string) (*pgx.Conn, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Postgres URL: %w", err)
	}
	config.RuntimeParams["application_name"] = name
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return conn, nil
}
