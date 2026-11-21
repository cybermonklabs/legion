// Package graphcensus holds the three operator passes over the node and tuple store: the
// census, which reads the store against the tables that project it and refuses when the two
// disagree; the sizing (`envoy-dispatch census --store --sizing`, readSizing), which reads in
// the census's own snapshot how fast the store grows - the write rate by kind, the tuples and
// nodes each write states, where graph_tuples and graph_nodes reach in a year against
// pgmigrate.CensusTableLimit - and what the block-index reconciliation still owes; and the
// backfill, which writes the nodes and tuples of the rows written before the store existed.
//
// A node is a thing Dispatch holds and a tuple is one fact stated about it
// (store/migrations/20261109013400_graph_store.up.sql); every table Dispatch reads today stays
// where it is and becomes the projection of those two
// (store/migrations/20261109022248_graph_projections.up.sql). The census is what says the
// projection and the store still agree, and it is the gate the backfill is measured by: a row
// is answered from the store the moment it names its node, and the census is how anyone knows
// every row of a kind does and that what the store holds for them is still right.
//
// The census and the sizing write nothing and read one read-only snapshot. The backfill writes
// through graphstore, the one package in the tree that writes graph_nodes and graph_tuples.
package graphcensus

// applicationName is how this package's own connection appears in pg_stat_activity: an operator
// reading the activity of a database the census is being taken of sees which pass holds it. The
// census opens that one connection (store.Connect) and uses no pool and migrates nothing: it is
// taken before a deployment rolls, exactly as the pre-deploy census is (store.Census). The
// backfill writes and holds the server's own pool instead (cmd/dispatch's openMigrated).
const applicationName = "envoy-dispatch census --store"
