package lockstep

import "maps"

// StoreWriteCallSites is the register of the store's writers (storeWriteCallSites), a fresh map
// for each caller. It is held here, in a package no writer of the store imports, so the two tests
// that read it read one list: graphstore's TestEveryStoreWriteCallSiteIsListed, which holds it to
// the module's source and each site's driver to a test the package it names declares, and
// graphcensus's TestTheSizingCountsEveryTupleTheWritersAppend, which drives every site it lists
// and reads off graph_tuples what each one appended.
func StoreWriteCallSites() map[string]string { return maps.Clone(storeWriteCallSites) }

// storeWriteCallSites is every function outside the tests that calls one of graphstore's writers
// (graphstoretest.WriteFunctions), named as docs' register names a pass: the file, from
// internal/dispatch where it lives under it, and the function, without its receiver. Beside each
// site is a test that runs the site's real code, as "<package dir>#<Test>": the directory of the
// package that declares it, named as a site's file is (graphstore's siteKey), and the test's name.
// For most sites it is the test that drives the site's lock steps against the order written on
// api/inbox_read.go's markInboxReadThreads: it holds each step the site takes in another
// transaction, and reads where the write waits and which table locks it holds by then.
// graphstore's TestEveryStoreWriteTakesOneLockOrder proves the order on that package's own
// writers. A site no lock-step test drives says so in its comment, and the test beside it runs its
// real code. A site that is not listed has had nobody look at its steps, and a site whose test is
// missing or names no test of that package is one c-lock-order's run drives no test of, so each
// fails graphstore's TestEveryStoreWriteCallSiteIsListed until it is put right.
var storeWriteCallSites = map[string]string{
	// The follow of an ask, which records the following session's node in the caller's
	// transaction or write phase, and each site that moves that node onto the event it appends
	// after the follow, and appends the node's base attributes under it, through QueueCaused:
	// the follower route (appendCausingEvent), an ask's create, an approval request renewed,
	// and a settlement's asks, each stated right after its ask.opened (appendEvents). asks'
	// TestAFollowTakesTheStoreLockOrder proves both steps on the follow alone, and each caller's
	// test proves them inside its caller. The comment path and a delivery's reply give the follow
	// their own event instead, which it states in its own statement.
	"asks/followers.go#Follow":         "asks#TestAFollowTakesTheStoreLockOrder",
	"asks/followers.go#queueFollow":    "asks#TestAFollowTakesTheStoreLockOrder",
	"asks/followers.go#Queue":          "asks#TestAFollowTakesTheStoreLockOrder",
	"api/server.go#appendCausingEvent": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/asks.go#createAskFor":         "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/reviews.go#renewApprovalAsk":  "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"docs/settlement.go#appendEvents":  "docs#TestEveryDocsStoreWriterTakesTheStoreLockOrder",
	// The block index's cause: each site that appends what a version's index pass stated under
	// the event that recorded the version, through QueueCaused (refs'
	// TestCausedTakesTheStoreLockOrder proves the step on its own): an upload, a version write's
	// event, an issue's spec, the block-index tick and the seed's documents. A settlement's
	// version states it right after its event (appendEvents, above).
	"api/artifacts.go#commitArtifactVersionEvent":   "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/artifacts.go#storeArtifact":                "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/artifacts.go#queueNewArtifact":             "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/issue_create.go#createIssue":               "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"cmd/dispatch-seed/write_docs.go#writeDocument": "cmd/dispatch-seed#TestTheSeedsDocumentWriteTakesTheStoreLockOrder",
	"docs/block_index.go#indexOwedDocument":         "docs#TestEveryDocsStoreWriterTakesTheStoreLockOrder",
	// The comment path, which the comment route and the comment an issue write leaves share: the
	// comment's node before the comments row that names it (queueCommentRow, through
	// graphstore.QueueNodes), then its tuples and its cause under the event appended after both,
	// and who wrote it (queueCommentCreated, through QueueCaused and QueueAuthored). The comment
	// route writes the node of each delivery attempt its mentions open before the attempt's row
	// itself (createCommentFor, through QueueNodes), which the comment's event causes
	// (queueCommentCreated).
	"api/comment_create.go#createCommentFor":    "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
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
	// Every other write of a delivery attempt's row. Each states in the same write phase what the
	// row now holds (graphstore.QueueAttempted), and the two that open an attempt queue its node
	// before the row (graphstore.AttemptOpened's plan, through QueueNodes), as the creates listed
	// with their own nodes do (createCommentFor, createStoredMessage, queueBroadcastCopy).
	// QueueAttempted appends tuples alone, which lock no row, so it adds no step to its caller's
	// order; an opened attempt's node is one no other write can name yet, written before the row
	// whose foreign key names it. No lock-step test drives these sites: api's
	// TestMentionsAreTuplesAndEveryAttemptIsANode runs each one's real code and reads what it
	// states of its attempt.
	"api/comment_delivery.go#holdCommentDeliveryClaim":        "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/comment_delivery.go#openCommentDeliveryAttempt":      "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/comment_delivery.go#recordCommentDeliveryResolution": "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/comment_delivery.go#standDown":                       "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/comment_delivery.go#supersedeCommentDelivery":        "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/comment_delivery.go#take":                            "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/comment_mention_claims.go#claimQueuedMention":        "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/comment_mention_claims.go#refreshMentionClaims":      "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/comment_mention_claims.go#releaseMentionClaims":      "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/delivery_sweep.go#settleUndeliverableMention":        "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/message_delivery.go#acceptDelivery":                  "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/message_delivery.go#insertMessageDeliveryAttempt":    "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/message_delivery.go#claimAttempt":                    "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/message_delivery.go#recordPendingMessageDelivery":    "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	"api/message_delivery.go#replyMessage":                    "api#TestMentionsAreTuplesAndEveryAttemptIsANode",
	// The mention rule's settle of an abandoned attempt before it publishes that session's copy,
	// which states it through QueueAttempted as the attempt writes above do. No lock-step test
	// drives it: the outbox's credit filter test runs its real code.
	"outbox/mentions.go#settleAbandoned": "outbox#TestTheCreditsFilterKeepsACreditExactlyWhereAMentionDecidesItsCopy",
	// The node of every issue, document, ask and message a request creates, in the project or
	// between the parties it belongs to: written before its row, and caused by the event the
	// create appends after it. createIssue, storeArtifact with queueNewArtifact, and createAskFor
	// are listed above for their block index and follow, and their drivers hold the node step too;
	// these are the others. Their order - the owner lock their path already takes, the node before
	// the row, the event before the tuples - is the order of the write they share (QueueNodes, then
	// QueueCaused; a route that appends its event at once queues the cause in the event's own
	// batch, appendCausingEvent), and api's TestEveryObjectNodeWriterTakesTheStoreLockOrder drives
	// each of them through it.
	"api/broadcasts.go#queueBroadcastCopy":       "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/message_delivery.go#insertSessionReply": "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/messages.go#createStoredMessage":        "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/reviews.go#openApprovalAsk":             "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"docs/ask_blocks.go#createAskBlock":          "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	// A broadcast's pages, the page each recipient's copy hangs from, queued before the first
	// copy's node and caused by each copy's event, which the broadcast's driver to two recipients
	// holds at the recipients' pages.
	"api/broadcasts.go#queue":      "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/broadcasts.go#queueCause": "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	// The removal of an untouched document states its node gone, before its row goes, caused by
	// the artifact.removed event it appends after the row (Write of the node the document's
	// create would have written, for a document an earlier build made with none, then
	// node write it holds for its cause (PlanUncaused, QueueNodes), then the event and the gone
	// statement in one batch, appendCausingEvent).
	"api/artifact_removal.go#removeArtifact": "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	// A document's retirement, in the same order under the document's
	// owner row: its artifact node stated gone, then the artifacts row that retires it, then
	// document.retired and the node's facts under it.
	"docs/retire.go#RetireDocument":   "docs#TestTheDecisionsStepsWritersTakeTheStoreLockOrder",
	"docs/retire.go#writeRetiredNode": "docs#TestTheDecisionsStepsWritersTakeTheStoreLockOrder",
	// A person's Keep of an agent's decision: under the issue's owner
	// row, the person's node, then the decision_keeps row that projects the Keep, then
	// decision.kept, and the kept_by tuple and the kept_text_sum value under it, through Caused.
	"api/decisions.go#keepDecisionNode": "api#TestEveryAskTransitionAndKeepTakesTheStoreLockOrder",
	// The decisions step's restore and conversion of an issue,
	// under the issue's owner row and its asks' rows: each document's version and its event, the
	// index pass's facts under that event through Caused, then the moves, the restored asks'
	// relations and the carried Keeps, with the person node a Keep names, under the spec's event.
	"docs/decision_restore.go#writeVersion": "docs#TestTheDecisionsStepsWritersTakeTheStoreLockOrder",
	"docs/decision_restore.go#writeFacts":   "docs#TestTheDecisionsStepsWritersTakeTheStoreLockOrder",
	// A person's own write that takes an answered ask's block out of its document: the person's
	// node at the sentinel, after the version's block index and before the event that records the
	// version, and the removed_by tuple appended with the index's facts under that event. A
	// rendered version's removals ride in its block index's own write (refs.PlanBlocks' beside);
	// an unrendered one's are queued here.
	"docs/removed_by.go#queueRemovals": "docs#TestAPersonsRemovalOfAnAnsweredAskTakesTheStoreLockOrder",
	// The seed's comments, with their author's person node and each comment's authored_by
	// tuple, which its one batch write states as a seeded database's own writer, the session
	// nodes of the followers its ask batch copies, and its issues', asks' and messages' nodes,
	// each written in the batch that copies their rows, in its project, before any row of it.
	"cmd/dispatch-seed/write_core.go#writeIssues":         "cmd/dispatch-seed#TestTheSeedsObjectNodeWritesTakeTheStoreLockOrder",
	"cmd/dispatch-seed/write_items.go#writeComments":      "cmd/dispatch-seed#TestTheSeedsCommentWriteTakesTheStoreLockOrder",
	"cmd/dispatch-seed/write_items.go#writeFollowerNodes": "cmd/dispatch-seed#TestTheSeedsFollowerWriteTakesTheStoreLockOrder",
	"cmd/dispatch-seed/write_items.go#writeItemNodes":     "cmd/dispatch-seed#TestTheSeedsObjectNodeWritesTakeTheStoreLockOrder",
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
	// TestEveryStoreWriteTakesOneLockOrder proves the order on them. QueueCaused hands the facts of
	// a write that name a person (Uncaused.People, a mention of one) to QueueToPerson after the
	// write's own append, which writes the person nodes `do nothing` and their tuples.
	"graphstore/people.go#QueueAuthored":        "graphstore#TestAuthoredAndReachedTakeTheStoreLockOrder",
	"graphstore/people.go#QueueReached":         "graphstore#TestAuthoredAndReachedTakeTheStoreLockOrder",
	"graphstore/people.go#QueueSessionRecorded": "graphstore#TestAuthoredAndReachedTakeTheStoreLockOrder",
	"graphstore/write.go#Caused":                "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	"graphstore/write.go#QueueCaused":           "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	"graphstore/write.go#Write":                 "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	// ToPerson and FromPerson run their queued forms (QueueToPerson, QueueFromPerson) as a batch of
	// their own, for a writer that queues no write phase, and their callers' drivers hold their
	// steps.
	"graphstore/from_person.go#FromPerson": "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphstore/to_person.go#ToPerson":     "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	// The one writer of an ask's answer state: asks.Settled.Queue states the answer on the ask's
	// node, appended under the event it is given or held for the caller's, and then writes the
	// row; the catch-up of a follow another build recorded states it on the node. Each site that
	// appends what a transition held under the event it appends after it, through QueueCaused:
	// the answer, resolve and decline routes' transition, an issue's close and reopen, the
	// approval carry's answer, and a settlement's retractions and restorations (appendEvents,
	// above). The settlement states the copies among a document's ask blocks as claims once its
	// version is indexed, and beside them the readability of each ask whose block it read, as
	// that ask node's `invalid` value (G1), and the backfill of the boot step's first phase states
	// the asks and the followers written before this release, each batch under its owners' locks
	// first. The decisions step's catch-up drives Settle, under the ask's owner lock and then its
	// row.
	"api/ask_transitions.go#closeAskTx":                  "api#TestEveryAskTransitionAndKeepTakesTheStoreLockOrder",
	"api/issue_ask_lifecycle.go#recordWithdrawnAsksTx":   "api#TestEveryAskTransitionAndKeepTakesTheStoreLockOrder",
	"api/issue_ask_lifecycle.go#recordRestoredAsksTx":    "api#TestEveryAskTransitionAndKeepTakesTheStoreLockOrder",
	"asks/followers.go#CatchUpFollower":                  "docs#TestTheDecisionsStepsWritersTakeTheStoreLockOrder",
	"asks/settle.go#Queue":                               "docs#TestTheCatchUpTakesTheAsksOwnerRowBeforeItsRow",
	"docs/approval_carry.go#answerApprovalAsk":           "docs#TestEverySettlementWriterTakesTheStoreLockOrder",
	"docs/ask_blocks.go#claimCopies":                     "docs#TestEverySettlementWriterTakesTheStoreLockOrder",
	"docs/ask_blocks.go#stateInvalidReasons":             "docs#TestEverySettlementWriterTakesTheStoreLockOrder",
	"graphcensus/backfill_asks.go#writeAskBatch":         "graphcensus#TestTheAskBatchesTakeEveryOwnerRowBeforeTheirRows",
	"graphcensus/backfill_asks.go#writeAskFollowerBatch": "graphcensus#TestTheAskBatchesTakeEveryOwnerRowBeforeTheirRows",
	// The backfill of every kind the one inbox moves (graphcensus.backfillInbox), each batch in the
	// order the live writers take - owner rows, then an ask's project rows, then the item rows and
	// an item's block state, then the item's nodes and tuples, then a thread's user_threads rows in
	// ascending login and the deliveries to them, then the markers: the messages, the comment
	// pointers, an item's blocks, their links (writeLinks), the item's citations, a comment's
	// delivery routes and what an older build's outbox published (writeRelations), the attempt rows,
	// the user_threads rows and the five other read-state tables. What each comment, message, ask,
	// answer and decline an older build wrote owed its thread is inboxwrite.RecordOlder's, listed
	// with the one writer below.
	"graphcensus/backfill_inbox.go#writeMessages":            "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/backfill_inbox.go#writeCommentPointers":     "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/backfill_inbox.go#writeBlocks":              "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/backfill_inbox.go#writeLinks":               "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/backfill_inbox.go#writeRelations":           "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/backfill_inbox.go#writeAttempts":            "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/backfill_inbox_threads.go#writeUserThreads": "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/backfill_inbox_threads.go#writeMarks":       "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	// The publisher's record of a batch, which splices the store's arms into its statement, and
	// its record of a fallback pass, which splices in what each published copy delivered.
	"outbox/mentions.go#recordFallbackCopies": "outbox#TestAFallbackCopyThePassPublishedIsADelivery",
	"outbox/publisher.go#recordBatch":         "outbox#TestRecordBatchTakesAPrefixOfTheStoreLockOrder",
	// The block index: its nodes inside the revision fence, queued on the write phase of the
	// writer that runs it (BlockIndexing.Queue), with the typed-node pass's own nodes and their
	// authors' (refs.PlanBlocks' typed facts), whose tuples and cause wait for the event that
	// recorded the version (the sites of QueueCaused above), driven inside the tick and the
	// settlement that run it. Above the deferral bound the typed-node pass runs on its own
	// (refs.DecisionIndexing.Queue), under the document's owner lock its caller holds, its tuples
	// and cause waiting for the same event.
	"refs/blocks.go#Queue":         "docs#TestEveryDocsStoreWriterTakesTheStoreLockOrder",
	"refs/decision_nodes.go#Queue": "docs#TestEverySettlementWriterTakesTheStoreLockOrder",
	// The index of an item's body: the block nodes of a comment, a message or an ask, queued
	// before the block rows that name them (refs.ItemIndexing.Queue), whose part_of, links and the
	// item's citations wait for the event of the write that indexed them (QueueCaused at the item
	// writers' sites above, an edit's among them), and each owed pass, which appends them at the
	// sentinel in its own transaction: the owed index of a reply's parent (refs.ItemRepair.Queue),
	// the re-index of old text and the rebuild (refs' reindexPlan.queue and replaceInTx), a
	// restored wording, an approval ask's rewrite and the seed. api's
	// TestEveryApiStoreWriterTakesTheStoreLockOrder holds the comment and reply paths to the block
	// steps, and TestEveryObjectNodeWriterTakesTheStoreLockOrder the ask and message paths. No
	// lock-step test drives the rebuild, an edit or a restored wording: the test beside each runs its
	// real code.
	"refs/item_blocks.go#Queue":          "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"refs/reply_blocks.go#Queue":         "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"refs/rebuild.go#queue":              "refs#TestARebuildBesideAnEditKeepsTheEditsReferences",
	"refs/rebuild.go#replaceInTx":        "refs#TestARebuildBesideAnEditKeepsTheEditsReferences",
	"api/asks.go#editAsk":                "api#TestItemBodyBlocksAreBlockNodesWithTheirLinksAndCitations",
	"api/comment_actions.go#editComment": "api#TestItemBodyBlocksAreBlockNodesWithTheirLinksAndCitations",
	// A comment's delete and an issue message's edit and delete (api/comment_delete.go,
	// api/message_corrections.go): the body's blocks re-indexed or taken out under the route's own
	// event (QueueCaused), each pending send's attempt failed under its receipt (QueueAttempted)
	// and, for an edit, the participation of each person the new text mentions (QueueToPerson). No
	// lock-step test drives them: the api test beside each runs its real code.
	"api/comment_delete.go#deleteComment":              "api#TestDeleteCommentFailsEachPendingMentionAttempt",
	"api/message_corrections.go#deleteMessage":         "api#TestDeleteMessageFailsEachPendingSend",
	"api/message_corrections.go#editMessage":           "api#TestMessageBlockIndexFollowsAnEditAndADelete",
	"docs/approval_ask.go#rewriteApprovalAsk":          "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"docs/decided_wording.go#restoreWordings":          "docs#TestARestorationRefusesAnAskAnsweredSinceItsPlan",
	"cmd/dispatch-seed/write_items.go#stateAtSentinel": "cmd/dispatch-seed#TestTheSeedsCommentWriteTakesTheStoreLockOrder",
	// The one writer of the threads a person takes part in (inboxwrite.Recording.Queue), which every
	// comment, message, ask and answer write queues after its event: what an answer's ask still owes
	// the store, its writer's participates_in, every user_threads row of the thread in one pass,
	// then the delivered_to of each other participant through QueueToPerson and the move of their
	// rows, and its writer's own row (queueKeepRow). Where the write is its writer's own answer or
	// reply (the own-answer rule, R1), queueKeepRow first appends the writer's dismissed mark
	// through QueueFromPerson and its upsert sets the row's dismissal; under an Agent thread, Queue
	// appends that mark, upserts the writer's user_agent_thread_dismissed row, and then inserts the
	// thread's unread replies into user_agent_reply_read, stating each reply_read in the same
	// statement (MarkArm), all after the root its caller took `for update` before its own insert
	// (D6). It takes the owner lock its caller took first and no other node row than the person
	// nodes QueueToPerson and QueueFromPerson upsert `do nothing`. api's
	// TestEveryApiStoreWriterTakesTheStoreLockOrder holds the comment and reply paths to its
	// user_threads steps.
	"inboxwrite/record.go#Queue":        "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"inboxwrite/thread.go#queueKeepRow": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	// The same writer's clear of the Agent dismissals a session's reply ends
	// (queueAgentDismissalsClear): the root message `for key share`, then the retraction of each
	// dismissed mark of that root through QueueMarksEnded, then the delete of the
	// user_agent_thread_dismissed rows. No lock-step test follows these steps; api's
	// TestADismissHoldingTheAgentRootGivesTheThreadBackOnTheReply runs them, holding the root as the
	// dismiss route takes it `for update`.
	"inboxwrite/thread.go#queueAgentDismissalsClear": "api#TestADismissHoldingTheAgentRootGivesTheThreadBackOnTheReply",
	// The same writer's record of a write an older build made (inboxwrite.RecordOlder), which the
	// backfill runs for each such comment, message, ask, answer and decline in the batch that marks
	// it, after its owner and item rows: participates_in, the user_threads rows of the people it
	// reached locked in ascending login, then their delivered_to through ToPerson and the
	// retractions of the dismissals it ended through FromPerson.
	"inboxwrite/older.go#RecordOlder": "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	// The move of the person open asks wait on (inboxwrite.QueueMoveWaiting), which patchIssue's
	// assignee, putAskAskedOf, putProjectDefaultAssignee, the reopen's restore of withdrawn asks and
	// a settlement's restore of an ask it retracted queue after their event: the asks rows it moves
	// locked `for no key update` in ascending id, after the owner lock and the project's lock its
	// caller took, then the person tuples, which write the person nodes `do nothing` before the
	// tuples, in ascending (kind, id).
	"inboxwrite/waiting.go#QueueMoveWaiting": "api#TestEveryMoveOfWhoAnAskWaitsOnTakesTheStoreLockOrder",
	// The writes of a person's read state: each route that writes one of the read-state tables
	// queues, through QueueFromPerson and under the event it appends after its statement, the mark
	// that statement stored - a thread read, Mark all read and the dismissal (whose transactions
	// end in commitInboxMarks), a conversation's Clear, read mark and replies read by id, and a
	// snooze and an unsnooze (the removal of an untouched document, listed above, retracts the marks
	// of the Inbox rows it deletes). Each takes the lock order written on api/inbox_read.go's
	// markInboxReadThreads as it did, and the one statement QueueFromPerson adds comes after its
	// event and every row it locks: it writes the person's node `do nothing` and appends tuples,
	// which lock no row of another table. No lock-step test drives these sites: api's
	// TestEveryReadStateWriteIsATupleFromThePerson runs each one's real code and reads what it
	// states.
	"api/ask_snooze.go#announceAskSnooze": "api#TestEveryReadStateWriteIsATupleFromThePerson",
	"api/inbox_read.go#commitInboxMarks":  "api#TestEveryReadStateWriteIsATupleFromThePerson",
	"api/replies.go#putThreadRead":        "api#TestEveryReadStateWriteIsATupleFromThePerson",
	"api/state.go#putUserAgentState":      "api#TestEveryReadStateWriteIsATupleFromThePerson",
	// The participation of each person a comment or a message newly mentions in its thread (R4's
	// mention arm, inboxwrite.MentionParticipation), which the writer that resolves the mention
	// queues right after that write's record: the person nodes `do nothing` and the tuples, after
	// every row the record locked. An edit queues the same after what its index states
	// (editComment, listed above).
	"api/comment_create.go#queueRecordAndMentions": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
}
