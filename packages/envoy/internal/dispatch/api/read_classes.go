package api

// readWrites is what one GET row of the table writes when it serves a request: whether it is a
// read, which a read-only token may call, or a GET that writes, which no read-only token may
// call. A read-only token is any credential that holds read: a token the secrets broker minted
// from a read entry or narrowed on request, and a personal token minted with read.
type readWrites int

const (
	// readWritesUnset is the zero value: an entry that decides nothing. It is never a class, so
	// an entry that leaves Writes unset is a row nobody has read yet.
	readWritesUnset readWrites = iota
	// readWritesNothing is a read: a route a read-only credential may call. Wherever its writes
	// run, on the request's own context or on the server's context in work the request starts or
	// waits on, such a route writes nothing but these eight:
	//
	//   - the insert into dpop_proofs that spends a broker token's proof;
	//   - the update of a personal token's last_used_at;
	//   - the insert into text_trees that a read with Dispatch-Trees: 1 makes;
	//   - the upsert of a stale route_roster;
	//   - the event horizon's observer persisting event_horizon;
	//   - a project's term statistics refresh, in search_term_stats, search_term_totals and
	//     search_term_queue;
	//   - the GitHub client recording GitHub's rate limit in github_rate_limits;
	//   - a compaction's rewrite and delete in doc_updates and doc_compaction_owed.
	//
	// A read that writes any of them on the server's context carries a WriteNote saying so. Every
	// other GET route is a GET that writes (readWritesState), which no read-only credential may
	// call.
	readWritesNothing
	// readWritesState is a GET that writes: anything a read may not, wherever it runs. Its
	// WriteNote names what it writes.
	readWritesState
)

// readEntry is the read class of one GET row of the table: what the row writes, and the note
// that says so where the class alone does not.
type readEntry struct {
	// Writes is the row's class: readWritesNothing or readWritesState, never readWritesUnset.
	Writes readWrites
	// WriteNote names what the row writes, and the statement or the function that writes it. A
	// readWritesState row always carries one. A readWritesNothing row carries one when its
	// handler makes the server write on its own context, or when its class differs from what a
	// first reading of its handler suggests, and then it says why.
	WriteNote string
}

// docsCompactionNote is the WriteNote of every read that reads a document's live text through
// the document service (docs.Service's Text, TextWithBlocks, TextWithToken,
// TextWithTokenAndBlocks, Blocks and BlockPath, each through its readDocument).
const docsCompactionNote = "Writes nothing itself. A cold read of a document whose stored history holds more than one doc_updates row queues the document on the document service's compaction queue (noteStoredUpdates), and the queue's worker compacts it later on its own context: it updates and deletes doc_updates and deletes doc_compaction_owed, as the hourly sweep does for every due document, whoever reads it. A compaction changes nothing a reader reads."

// read_classes.go holds what a read class is: the two classes, what each means, and the note a
// class carries. Which class a route has is the route's own declaration (routeDecl.Read, one file
// per route), which routes() answers as apiRoute.Read and the read-only gate reads
// (refuseLimitedToken). register refuses to mount a table whose GET routes are not all classed,
// or whose write states a class (readClassRefusals, routes_table.go), so a GET route added later
// is served to no read-only credential until someone has read its handler and classed it.
