// Package graphstoretest holds what the tests of more than one package read of the node and
// tuple store and no code of the store reads: the tuple kinds of the base attributes
// (BaseAttributes), the store writers the source scans hold every write of the store to
// (WriteFunctions), and the visibility a test's write reserves a node for its author under
// (VisibilityFor). It is an ordinary package rather than a _test.go file because Go cannot share
// test-only code across packages, and it imports nothing of graphstore, so graphstore's own tests
// can import it: they hold each copy kept here of a value of graphstore to that value
// (graphstore's TestGraphstoretestBaseAttributesAreTheStoresOwn and
// TestGraphstoretestVisibilitiesAreTheStoresOwn).
package graphstoretest

// BaseAttributes is the tuple kind of every base attribute, a fresh slice for each caller: a copy
// of the set graphstore.IsBaseAttribute reads, which graphstore's
// TestGraphstoretestBaseAttributesAreTheStoresOwn holds equal to it, element for element. No other
// attribute's values may be stored under one of these kinds: a value tuple of one of them out of a
// node reads as that node's own visibility, state, project or parties, whoever wrote it, so an
// attribute a type declares under one of these names is stored under another name
// (pmdoc/schemanodes/schemanodestest.StoreName).
func BaseAttributes() []string { return []string{"visibility", "state", "project", "parties"} }

// WriteFunctions is the name of every function of graphstore a write of graph_nodes and
// graph_tuples from outside that package goes through, a fresh slice for each caller: Write and
// Caused in a caller's transaction, and QueueWrite, QueueNodes and QueueCaused, the same writes
// queued on a write phase's batch; WriteArms and WriteArmsUncaused spliced into a caller's
// statement; and QueueAuthored, QueueReached and QueueSessionRecorded, which queue those arms for
// the writers that record an author, a reached session and a session an ask names. Each is a
// function of the package, never a method, so a call of one is named by the package wherever it
// is made. It is the one list the source scans that hold the store's writers read: graphstore's
// register of their call sites (TestEveryStoreWriteCallSiteIsListed) and api's scan of the writers
// of the projected tables (TestNoProjectionRowIsWrittenWithoutTheStore). The list holds FillScopes
// too, the backfill's fill of the project of a node stored with none.
func WriteFunctions() []string {
	return []string{"Write", "WriteArms", "WriteArmsUncaused", "Caused",
		"QueueWrite", "QueueNodes", "QueueCaused",
		"QueueAuthored", "QueueReached", "QueueSessionRecorded", "FillScopes"}
}
