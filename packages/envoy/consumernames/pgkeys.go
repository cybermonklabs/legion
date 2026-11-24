package consumernames

// IntakeConsumersTable is the Postgres table Dispatch's and the Legion daemons' selection records
// are kept in, by the store of package consumernames/pgstore, which alone links a Postgres driver:
// a binary that keeps only the session family's KVStore, the listener, links none. The family's
// migration creates it as:
//
//	create table intake_consumers (
//	    logical  text primary key,
//	    record   jsonb not null,
//	    revision bigint not null check (revision > 0)
//	);
//
// Several families' stores can share one database, as several projects' daemons do, so every row
// is keyed by its family: a record of logical name L in family F's store is the row F/L (PGKey),
// never deleted, whose record is a Record carrying F/L as its own key; F's sentinel is the row
// envoy.sentinel/F (PGSentinelKey), whose record is a StoreState carrying that key. A family holds
// no '/' but a daemon's, after "daemon", and a logical name none, so no two rows of two families
// share a key, and a family's rows are exactly those whose key begins F/.
const IntakeConsumersTable = "intake_consumers"

// pgSentinelPrefix begins every Postgres store sentinel's key (PGSentinelKey). It holds a '.',
// which no family holds, so no family's record key begins with it.
const pgSentinelPrefix = "envoy.sentinel/"

// PGSentinelKey is the key of family's sentinel row: envoy.sentinel/<family>, for example
// envoy.sentinel/dispatch or envoy.sentinel/daemon/<project>.
func PGSentinelKey(family string) string { return pgSentinelPrefix + family }

// PGKey is the key of logical's record row in family's store: <family>/<logical>, for example
// dispatch/delivery-events or daemon/<project>/legion-go-<project>-github.
func PGKey(family, logical string) string { return family + "/" + logical }
