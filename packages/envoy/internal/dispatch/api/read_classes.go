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
	// readWritesNothing is a read. Wherever its writes run, on the request's own context or on
	// the server's context in work the request starts or waits on, the row writes nothing but
	// what readClasses lists: the proof's spent row, a personal token's last use, the caches
	// Dispatch rebuilds from what it holds, and the shared work the server does for every reader
	// alike. A read that writes any of it on the server's context carries a WriteNote saying so.
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

// read_classes.go holds what a read class is. Which class each route has is the route's own
// declaration (routeDecl.Read, one file per route), and register refuses to mount a table whose
// GET routes are not all classed (readClassRefusals, routes_table.go).
