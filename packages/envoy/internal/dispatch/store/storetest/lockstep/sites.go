package lockstep

import "maps"

// StoreWriteCallSites is the register of the store's writers (storeWriteCallSites), a fresh map
// for each caller. It is held here, in a package no writer of the store imports, so the two tests
// that read it read one list: graphstore's TestEveryStoreWriteCallSiteIsListed, which holds it to
// the module's source and each site's driver to a test of that site's package, and graphcensus's
// TestTheSizingCountsEveryTupleTheWritersAppend, which drives every site it lists and reads off
// graph_tuples what each one appended.
func StoreWriteCallSites() map[string]string { return maps.Clone(storeWriteCallSites) }

// storeWriteCallSites is every function outside the tests that calls one of graphstore's writers
// (graphstoretest.WriteFunctions), named as docs' register names a pass: the file, from
// internal/dispatch where it lives under it, and the function, without its receiver. Beside each
// site is the test that drives its lock steps against the order written on api/inbox_read.go's
// markInboxReadThreads, as "<package dir>#<Test>": the package's directory, named as a site's
// file is (graphstore's siteKey), and the test's name. Such a test runs the site's real code,
// holds each step it takes in another transaction, and reads where the write waits and which
// table locks it holds by then. graphstore's TestEveryStoreWriteTakesOneLockOrder proves the order
// on that package's own writers. A site that is not listed has had nobody prove its steps, and a
// site whose driver is missing or names no test of that package is one c-lock-order's run drives
// no test of, so each fails graphstore's TestEveryStoreWriteCallSiteIsListed until it is put right.
var storeWriteCallSites = map[string]string{
	// The follow of an ask, which records the following session's node in the caller's
	// transaction or write phase, and each site that moves that node onto the event it appends
	// after the follow, and appends the node's base attributes under it, through QueueCaused:
	// the follower route (appendCausingEvent), an ask's create, an approval request renewed,
	// and a settlement's asks, each stated right after its ask.opened (appendEvents). asks'
	// TestAFollowTakesTheStoreLockOrder proves both steps on the follow alone, and each caller's
	// test proves them inside its caller. The comment path and a delivery's reply give the follow
	// their own event instead, which it states in its own statement.
	"asks/followers.go#queueFollow":    "asks#TestAFollowTakesTheStoreLockOrder",
	"api/server.go#appendCausingEvent": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/asks.go#createAskFor":         "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/reviews.go#renewApprovalAsk":  "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"docs/settlement.go#appendEvents":  "docs#TestEveryDocsStoreWriterTakesTheStoreLockOrder",
	// The block index's cause: each site that appends what a version's index pass stated under
	// the event that recorded the version, through QueueCaused (refs'
	// TestCausedTakesTheStoreLockOrder proves the step on its own): an upload, a version write's
	// event, an issue's spec, the eval registry's write of a project's evals-md, the block-index
	// tick and the seed's documents. A settlement's version states it right after its event
	// (appendEvents, above). The API's test drives the registry's write through a register write,
	// the route that reaches it with one connection the test can watch.
	"api/artifacts.go#commitArtifactVersionEvent":   "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/artifacts.go#storeArtifact":                "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"docs/new_document.go#QueueNewArtifact":         "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/issue_create.go#createIssue":               "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"cmd/dispatch-seed/write_docs.go#writeDocument": "cmd/dispatch-seed#TestTheSeedsDocumentWriteTakesTheStoreLockOrder",
	"docs/block_index.go#indexOwedDocument":         "docs#TestEveryDocsStoreWriterTakesTheStoreLockOrder",
	"evals/document.go#queueDocumentEvent":          "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	// The comment path, which the comment route and the comment an issue write leaves share: the
	// comment's node before the comments row that names it (queueCommentRow, through
	// graphstore.QueueNodes), then its tuples and its cause under the event appended after both,
	// and who wrote it (queueCommentCreated, through QueueCaused and QueueAuthored).
	"api/comment_create.go#queueCommentCreated": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/comment_create.go#queueCommentRow":     "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	// A delivery's reply and the settle of a delivery attempt, which records the session the
	// attempt reached, and the one record both replies share of what their answer makes of the
	// attempt they answer: an attempt the answer flips to sent is one the listener took, and
	// its session is owed a node (queueAnsweredDelivery, which the reply's driver runs last).
	"api/comment_delivery.go#completeCommentDelivery": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/comment_delivery.go#queueAnsweredDelivery":   "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/comment_delivery.go#replyComment":            "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/message_delivery.go#completeMessageDelivery": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	// The node of every issue, document, ask and message a request creates, in the project or
	// between the parties it belongs to: written before its row, and caused by the event the
	// create appends after it. createIssue, storeArtifact with docs' QueueNewArtifact, and createAskFor
	// are listed above for their block index and follow, and their drivers hold the node step too;
	// these are the others. Their order - the owner lock their path already takes, the node before
	// the row, the event before the tuples - is the order of the write they share (QueueNodes, then
	// QueueCaused; a route that appends its event at once queues the cause in the event's own
	// batch, appendCausingEvent), and api's TestEveryObjectNodeWriterTakesTheStoreLockOrder drives
	// each of them through it.
	"api/broadcasts.go#writeBroadcast":           "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/message_delivery.go#insertSessionReply": "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/messages.go#createStoredMessage":        "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/reviews.go#openApprovalAsk":             "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"docs/ask_blocks.go#createAskBlock":          "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	// The removal of an untouched document states its node gone, before its row goes, caused by
	// the artifact.removed event it appends after the row (Write of the node the document's
	// create would have written, for a document an earlier build made with none, then
	// node write it holds for its cause (PlanUncaused, QueueNodes), then the event and the gone
	// statement in one batch, appendCausingEvent).
	"api/artifact_removal.go#removeArtifact": "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	// The seed's comments, with their author's person node and each comment's authored_by
	// tuple, which its one batch write states as a seeded database's own writer, the session
	// nodes of the followers its ask batch copies, and its issues', asks' and messages' nodes,
	// each written in the batch that copies their rows, in its project, before any row of it.
	"cmd/dispatch-seed/write_core.go#writeIssues":         "cmd/dispatch-seed#TestTheSeedsObjectNodeWritesTakeTheStoreLockOrder",
	"cmd/dispatch-seed/write_items.go#writeAsks":          "cmd/dispatch-seed#TestTheSeedsObjectNodeWritesTakeTheStoreLockOrder",
	"cmd/dispatch-seed/write_items.go#writeComments":      "cmd/dispatch-seed#TestTheSeedsCommentWriteTakesTheStoreLockOrder",
	"cmd/dispatch-seed/write_items.go#writeFollowerNodes": "cmd/dispatch-seed#TestTheSeedsFollowerWriteTakesTheStoreLockOrder",
	"cmd/dispatch-seed/write_items.go#writeMessages":      "cmd/dispatch-seed#TestTheSeedsObjectNodeWritesTakeTheStoreLockOrder",
	// The backfill command's five batches: the blocks, the block references and the comments
	// written before the store existed, the role and page nodes of the roles the census holds to
	// one (addressedRoles), and the session nodes of the sessions it holds to one
	// (recordedSessions). The comments are written through the backfill's comment write
	// (refs.CommentWrite.Queue), which the block-index tick queues too, for the comments
	// the events after its build's cursor name whose rows name no node, after its cursor row and
	// its marking statement's document_block_versions rows (docs' test holds it to that).
	"graphcensus/backfill_blocks.go#writeBlockBatch":        "graphcensus#TestTheBlockBatchTakesEveryOwnerRowBeforeAnyFenceRow",
	"graphcensus/backfill_block_refs.go#writeBlockRefBatch": "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"refs/comment_nodes.go#Queue":                           "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/backfill_roles.go#writeRoleBatch":          "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/backfill_sessions.go#writeSessionBatch":    "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	// The backfill's passes over the project of every node: the nodes of the issues, documents,
	// asks and messages that their marker does not mark, in one node write (PlanUncaused,
	// QueueNodes) and Caused over each run of the batch's nodes of one cause (a row's own create
	// event, or the sentinel), after the owner locks a document's removal takes (lockObjectOwners)
	// and under them, and the project of the comment and block nodes written before a node named
	// one (graphstore.FillScopes), the block batch under the revision fence (lockBlockDocuments).
	"graphcensus/projects_backfill.go#writeObjectBatch": "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/projects_backfill.go#writeScopeBatch":  "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	// The fill itself appends what it states through Write; writeScopeBatch is its one caller, and
	// its driver holds the fill's steps.
	"graphstore/scope.go#FillScopes": "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	// The block-index tick's sweep of the documents a build before this one removed, which states
	// each orphaned node gone as this build's removal does: the issue's owner lock, then the
	// node's row, then the state tuple, under the removal's own event.
	"graphcensus/orphans.go#StateOrphanedGone": "graphcensus#TestTheOrphanSweepTakesTheStoreLockOrder",
	// graphstore's own writers that run another of them. QueueAuthored and QueueSessionRecorded
	// are arms of one statement, and QueueReached is QueueSessionRecorded for an attempt that was
	// sent. Write and Caused run QueueWrite and QueueCaused as a batch of their own, and
	// TestEveryStoreWriteTakesOneLockOrder proves the order on them.
	"graphstore/people.go#QueueAuthored":        "graphstore#TestAuthoredAndReachedTakeTheStoreLockOrder",
	"graphstore/people.go#QueueReached":         "graphstore#TestAuthoredAndReachedTakeTheStoreLockOrder",
	"graphstore/people.go#QueueSessionRecorded": "graphstore#TestAuthoredAndReachedTakeTheStoreLockOrder",
	"graphstore/write.go#Caused":                "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	"graphstore/write.go#Write":                 "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	// The publisher's record of a batch, which splices the store's arms into its statement, and
	// its record of a fallback pass, which splices in what each published copy delivered.
	"outbox/mentions.go#recordFallbackCopies": "outbox#TestAFallbackCopyThePassPublishedIsADelivery",
	"outbox/publisher.go#recordBatch":         "outbox#TestRecordBatchTakesAPrefixOfTheStoreLockOrder",
	// The block index: its nodes inside the revision fence, queued on the write phase of the
	// writer that runs it (BlockIndexing.Queue), whose tuples and cause wait for the event that
	// recorded the version (the sites of QueueCaused above), driven inside the tick and the
	// settlement that run it.
	"refs/blocks.go#Queue": "docs#TestEveryDocsStoreWriterTakesTheStoreLockOrder",
}
