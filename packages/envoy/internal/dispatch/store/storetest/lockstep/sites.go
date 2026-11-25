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
// on that package's own writers. A site no such test drives yet, whose store write is one call of
// a graphstore writer after the rows its path already holds, names the test that proves that
// writer's order instead (graphstore's TestEveryStoreWriteTakesOneLockOrder, refs'
// TestCausedTakesTheStoreLockOrder for Caused, or api's TestEveryCreateStoresItsNode for a
// create's node before its row), as the comment beside it says. A site that is not listed has had
// nobody prove its steps, and a site whose driver is missing or names no test of that package is
// one c-lock-order's run drives no test of, so each fails graphstore's
// TestEveryStoreWriteCallSiteIsListed until it is put right.
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
	// event, an issue's spec, the block-index tick, an issue room's pass under each of its
	// documents' events, and the seed's documents. A settlement's version states it right after
	// its event (appendEvents, above).
	"api/artifacts.go#commitArtifactVersionEvent":   "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/artifacts.go#storeArtifact":                "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/artifacts.go#queueNewArtifact":             "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/issue_create.go#createIssue":               "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"cmd/dispatch-seed/write_docs.go#writeDocument": "cmd/dispatch-seed#TestTheSeedsDocumentWriteTakesTheStoreLockOrder",
	"docs/block_index.go#indexOwedDocument":         "docs#TestEveryDocsStoreWriterTakesTheStoreLockOrder",
	"docs/block_index.go#causeRoomFacts":            "docs#TestEveryDocsStoreWriterTakesTheStoreLockOrder",
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
	// The nodes and tuples of the reasoning kinds a request writes - a lens, its findings, a
	// risk and a constraint - with the author's node where the credential proves one: the issue's
	// owner lock, with the owner lock of each check's document where a risk names a prevented_by
	// (api's lockReasoningOwners), then one Write of every node in ascending (kind, id) and every
	// tuple at the sentinel, appending no event. Its order is the order
	// TestEveryStoreWriteTakesOneLockOrder proves of Write.
	"api/reasoning_write.go#writeReasoningNodes": "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	// One pointer tuple a request appends, retracts or retargets out of a node of an issue
	// (has_kind, triggered_by, constrained_by, prevented_by): the issue's owner lock, with the
	// owner lock of the check's document for an append or a retarget of a prevented_by (api's
	// lockReasoningOwners), then one Write of the tuple, or Retarget, which takes the node's row and
	// writes through Write. It appends no event. Its order is the order
	// TestEveryStoreWriteTakesOneLockOrder proves of Write, after the owner locks
	// writeReasoningNodes takes first too.
	"api/reasoning_write.go#writeReasoningPointer": "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	// One node's state a request writes or settles (api/node_state.go): the issue's owner lock,
	// then one Write of the node as the store holds it but for its state, and, on a move into
	// gone, of every node beneath it (graphstore.WriteState), each as the store holds it but for
	// its state: a plain write that moves the rows and appends their state tuples. It appends no
	// event. Its order is the order TestEveryStoreWriteTakesOneLockOrder proves of Write, after the
	// owner lock writeReasoningNodes takes first too.
	"api/reasoning_write.go#writeNodeState": "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	// A node's move (api/node_move.go, PATCH /api/v1/issues/{key}/nodes/{id}): the owner locks of
	// the issue and of every issue the moved node's subtree and the parent's chain stand under, in
	// one call (lockIssueOwners), then, where the move writes the block's text, the room's lock and
	// the version's write, and in its write phase the move's tuples alone at the sentinel
	// (QueueWrite, no node), the block's parent_block_id, the issue's query mark and the head rows.
	// api's TestEveryApiStoreWriterTakesTheStoreLockOrder holds a flat move to those steps.
	"api/node_move.go#queueNodeMove": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	// An issue's own field write made for a run (api/issue_patch.go's writeIssue, the write core the
	// PATCH and each step of a status change run), which names the issue changed: the issue's owner
	// lock (lockPatchOwners), the issue's row, then, on a context that carries a run alone, the
	// issue's node as PlanChangedForRun plans it (QueueNodes), the issue's own event, and what the
	// node states with its changed_by_run under that event (QueueCaused). api's
	// TestEveryApiStoreWriterTakesTheStoreLockOrder holds it to those steps, through the PATCH.
	"api/issue_patch.go#writeIssue": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	// An issue's relations (R9 and R10 of this branch's spec), which its create and its PATCH state:
	// after the issue's owner row, its row and its issue_links rows, the node of the item its
	// filed_from names where the store held none, through Write, with a comment's row named after
	// it (stateFiledFromNode), then the issue's own node through its node write held for its cause
	// (PlanUncaused, QueueNodes; createIssue's and patchIssue's), the issue's event, and last the
	// relations that event causes, its child_of,
	// needs, duplicate_of and filed_from and the part_of it no longer hangs from, in one statement
	// of the store's arms (queueIssueRelations). api's TestEveryApiStoreWriterTakesTheStoreLockOrder
	// holds a PATCH of its needs, and a PATCH of its filed_from at a comment with no node, to those
	// steps.
	"api/issue_relations.go#stateFiledFromNode":  "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/issue_relations.go#queueIssueRelations": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	// An edit of an ask's or a comment's words made for a run (api/asks.go, api/comment_actions.go),
	// which names the ask or the comment changed: the owner lock of the ask or the comment, under
	// which it reads the row, then, in its write phase, the row's update, then, on a context that
	// carries a run alone, its node as PlanChangedForRun plans it (QueueNodes), its own event, and
	// what the node states with its changed_by_run under that event (QueueCaused). api's
	// TestEveryApiStoreWriterTakesTheStoreLockOrder holds each to those steps.
	"api/asks.go#editAsk":                "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/comment_actions.go#editComment": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	// A comment's anchor block (T17b of this branch's plan), the anchored_in that
	// comments.anchor's block_id projects, kept by each write that moves the anchor's block or its
	// orphan flag beside the comment's create (queueCommentCreated, above): an accept or a
	// reject that finds a suggestion's mark gone orphans the anchor and retracts its anchored_in,
	// after the comment's owner row and its row `for update` (lockedComment), its update, then the
	// retraction through Write at the sentinel (api's TestEveryApiStoreWriterTakesTheStoreLockOrder);
	// the refresh of a document's anchors retracts or states it for each comment whose anchor it
	// orphans or finds again, after the owner row its settlement or version write takes first, the
	// comments update and the comment.anchor_refreshed events it appends, which cause the tuples
	// (docs' TestTheAnchorRefreshTakesTheOwnerRowTheRowTheEventThenTheTuple); and the anchor block
	// backfill states it beside the block id it pins, the comment's row, then the tuple through
	// Write at the sentinel. It takes no owner row, which the register's order admits: once it holds
	// the row it waits on nothing, so no owner-row holder that waits on the row can be waited on in
	// turn (docs' TestTheAnchorBlockBackfillTakesTheRowThenTheTuple holds the issue's row through the
	// whole run, and the run never queues behind it).
	"api/comment_actions.go#orphanCommentAnchor":      "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"docs/anchor_blocks.go#pinCommentAnchorBlock":     "docs#TestTheAnchorBlockBackfillTakesTheRowThenTheTuple",
	"docs/anchor_refresh.go#stateRefreshedAnchoredIn": "docs#TestTheAnchorRefreshTakesTheOwnerRowTheRowTheEventThenTheTuple",
	// An edit or a delete of an issue message and a delete of a comment made for a run
	// (api/message_corrections.go, api/comment_delete.go), which names the message or the comment
	// changed, in the same steps: the issue's owner lock, under which it reads the row, then, in
	// its write phase, the row's update or its clear (clearDeletedPostBody), then, on a context
	// that carries a run alone, its node as PlanChangedForRun plans it (QueueNodes), its own
	// event, and what the node states with its changed_by_run under that event (QueueCaused). api's
	// TestEveryApiStoreWriterTakesTheStoreLockOrder holds each to those steps.
	"api/comment_delete.go#deleteComment":      "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/message_corrections.go#editMessage":   "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/message_corrections.go#deleteMessage": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	// A run of the command bar (api/going.go), which takes no owner lock: its record writes the
	// interaction node, its author's node and their tuples in one Write before the going_requests
	// row, and each frame, in a transaction of its own, takes the run's interaction node row
	// `for no key update`, then the run's row `for key share`, then appends the frame value through
	// Write before the going_events row. api's TestEveryApiStoreWriterTakesTheStoreLockOrder holds
	// both to their steps. A frame of a run a pod of the release before recorded with no node
	// writes that node first through the one writer of a run's node from its row, which the
	// backfill shares (lockGoingRun): it takes the run's going_requests row `for no key update` and
	// then writes in one Write, which the same test holds a frame of such a run to, step by step.
	// The sweep states a run gone an hour after it was asked, in a transaction of its own and in
	// the frame's order (goRunGone): the run's interaction node row `for no key update`, one
	// WriteState of that node, its state tuple at the sentinel, then the delete of the run's
	// going_requests row, whose foreign key cascades to its going_events rows; the same test holds
	// it to those steps.
	"api/going.go#appendGoingEvent":      "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/going.go#goRunGone":             "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/going.go#lockGoingRun":          "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/going.go#recordGoingRequest":    "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"graphstore/interaction.go#WriteRun": "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	// The backfill's write of the node of each run a process of the release before recorded with
	// no node (graphcensus' backfillRuns), through WriteRun in a transaction of its own: the run's
	// going_requests row `for no key update`, then its nodes in ascending (kind, id) and their
	// tuples, with no owner row. graphcensus' TestEveryBackfillBatchTakesTheStoreLockOrder holds it
	// to those steps.
	"graphcensus/backfill_runs.go#writeOwedRun": "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	// The backfill's passes over what a process of the release before writes of a run during a
	// roll (graphcensus' TestTheBackfillMendsWhatAnOlderPodWroteOfARun): a frame it appended with
	// no value, stated through Write after the run's interaction node row `for no key update`, the
	// frame path's own first lock, and a run whose row it deleted, whose node is written gone
	// through Write after that same row. Neither takes an owner row, as no run stands under one;
	// graphcensus' TestEveryBackfillBatchTakesTheStoreLockOrder holds both to those steps.
	"graphcensus/backfill_runs.go#writeGoneRun":   "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/backfill_runs.go#writeRunFrames": "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	// The backfill's retraction of each prevented_by a process of the release before left asserted
	// at a check it dropped or at a check of a document it retired (graphcensus'
	// TestTheBackfillRetractsThePreventedByOfChecksNobodyRuns): the owner row of each such check's
	// document first, the row the index pass and the retirement retract under, then one Write of
	// the retractions. graphcensus' TestEveryBackfillBatchTakesTheStoreLockOrder holds it to those
	// steps.
	"graphcensus/backfill_preventions.go#writeEndedPreventions": "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	// The node of every issue, document, ask and message a request creates, in the project or
	// between the parties it belongs to: written before its row, and caused by the event the
	// create appends after it. createIssue, storeArtifact with queueNewArtifact, and createAskFor
	// are listed above for their block index and follow, and their drivers hold the node step too;
	// these are the others. Their order - the owner lock their path already takes, the node before
	// the row, the event before the tuples - is the order of the write they share (QueueNodes, then
	// QueueCaused; a route that appends its event at once queues the cause in the event's own
	// batch, appendCausingEvent), and api's TestEveryObjectNodeWriterTakesTheStoreLockOrder drives
	// each of them through it.
	// The bytes of an image's or a file's later version (api's statedVersionBytes, which plans them
	// and writes nothing) are stated on the node its row names with no node written, after the
	// owner lock and that row read FOR SHARE, through storeArtifact's QueueCaused after the event
	// it appends, which causes them (api's TestALaterVersionStatesItsBytesInTheStoreLockOrder proves
	// its steps, and that a later version waits for a backfill naming its node).
	// removedDocumentNodes is a removal's nodes of the document and of its versions stated gone,
	// with each changed_by_run of the document's node retracted, after the issue's row and before
	// any row of the document is deleted, and removeArtifact appends what it stated in the batch
	// that appends the artifact.removed event, appendCausingEvent (api's
	// TestARemovalStatesItsDocumentGoneInTheStoreLockOrder). Between the row's delete and that
	// event, where a lens stands on the document's node, removeArtifact reads the stale lenses on
	// it (refs.StaleLensesOn) and what stands beneath each (graphstore.WriteState), under the owner
	// lock it holds and taking none of its own, and writes those nodes in one Write in ascending
	// (kind, id) with their state tuples at the sentinel. Its driver removes a document no lens
	// stands on, and a document a lens with a finding stands on too, holding each step that run
	// takes in turn, the lens's and the finding's node rows and their state tuples among them.
	// createIssueTree is an issue's tree artifact (R5 of this branch's spec): its node and its
	// part_of to the issue through QueueNodes before the artifacts row that names the node and
	// the issue_trees row, queued at the head of its caller's write phase, whose transaction holds
	// the issue's owner lock and causes what the write stated under the event it appends - the
	// order of the write it shares with createIssue's spec (the node, then the row), which
	// TestEveryStoreWriteTakesOneLockOrder proves of the node write. createDecisionsQuery is the
	// decisions query node of an issue born converted (T12b of this branch's plan): in the same
	// queued step, after the tree's issue_trees row and the spec's document_rooms row, its node and
	// its part_of through QueueNodes before the artifacts row naming the node, then its empty
	// version 1 and that version's node, and createIssue causes what both stated under
	// issue.created through QueueCaused. api's TestEveryObjectNodeWriterTakesTheStoreLockOrder runs
	// createIssue born converted and holds it to those steps.
	"api/artifact_removal.go#removeArtifact":       "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/artifact_removal.go#removedDocumentNodes": "api#TestARemovalStatesItsDocumentGoneInTheStoreLockOrder",
	"api/artifacts.go#createIssueTree":             "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/broadcasts.go#writeBroadcast":             "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/issue_create.go#createDecisionsQuery":     "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/message_delivery.go#insertSessionReply":   "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/messages.go#createStoredMessage":          "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"api/reviews.go#openApprovalAsk":               "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	"docs/ask_blocks.go#createAskBlock":            "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	// The node of every version a write inserts, in the row's transaction (this branch's spec):
	// the one writer every insert of a version calls (docs.WriteVersionNode) - a version write
	// (writeVersionTx), an upload (api's writeDocumentVersion and storeArtifact) and an issue's spec
	// (api's createIssue) - after the owner row its caller holds and the block nodes of the
	// version's index, with its facts held for the event the caller appends and causes them with,
	// through Caused at the sites listed above. Its address is drawn from the version's own number,
	// so no other transaction holds it; api's TestEveryObjectNodeWriterTakesTheStoreLockOrder and
	// TestALaterVersionStatesItsBytesInTheStoreLockOrder hold an upload's and an issue create's
	// steps around it, and api's TestEveryVersionRowIsTheProjectionOfAVersionNode holds its node to
	// its row's transaction at each site.
	"docs/mutation.go#WriteVersionNode": "api#TestEveryObjectNodeWriterTakesTheStoreLockOrder",
	// A document's retirement, in the same order under the document's
	// owner row: its artifact node stated gone, then the artifacts row that retires it, then
	// document.retired and the node's facts under it. A removal of an untouched document states
	// its node gone through its own writer (removedDocumentNodes, listed above), with each
	// changed_by_run of the node retracted, under the owner lock and the room, then deletes its
	// rows, appends artifact.removed and the node's facts under it (api's
	// TestEveryApiStoreWriterTakesTheStoreLockOrder holds it to those steps).
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
	// one (graphstore.FillScopes, whose lock and fill are one batch of their own), the block batch
	// under the revision fence (lockBlockDocuments). An issue's batch then states, under those
	// locks, the child_of claim of the parent its row names through WriteArms, which appends at the
	// sentinel, outside every repeat key, and so waits on no row, then names each node on its row,
	// which graphcensus' TestTheObjectBatchTakesTheStoreLockOrder proves too.
	"graphcensus/projects_backfill.go#writeObjectBatch": "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	"graphcensus/projects_backfill.go#writeScopeBatch":  "graphcensus#TestEveryBackfillBatchTakesTheStoreLockOrder",
	// The block-index tick's sweep of the documents a build before this one removed, which states
	// each orphaned node gone as this build's removal does: the issue's owner lock, then the
	// node's row, then the state tuple, under the removal's own event.
	"graphcensus/orphans.go#StateOrphanedGone": "graphcensus#TestTheOrphanSweepTakesTheStoreLockOrder",
	// The backfill's pass over the versions an older build stored with no node
	// (this branch's spec): the owner rows of the batch's documents first, then the version
	// rows read again under them, then the version nodes and their facts, in one graphstore.Write
	// per batch (graphcensus' TestTheVersionBatchTakesTheStoreLockOrder).
	"graphcensus/census_versions.go#writeVersionBatch": "graphcensus#TestTheVersionBatchTakesTheStoreLockOrder",
	// The same nodes written by the block-index pass that drains a mark, or that reads the store's
	// statement of its document stale, for the versions of the documents it covers that hold none
	// (docs' indexRenderedVersion: the reconciliation's drain on either mark, and a request's
	// version write that drains this build's mark first or finds the statement stale
	// (refs.BlockIndexing.Corrects), leaving out its own version and writing none where more than
	// missingVersionNodesBound are missing): under
	// the owner lock its caller took first, which every writer of a version node takes, the version
	// rows read in the pass's read phase (PlanMissingVersionNodes), then, queued after the nodes of
	// the pass, the version nodes and their facts in one graphstore.QueueWrite at the sentinel, the
	// same read and the same write as the batch above.
	"graphcensus/census_versions.go#Queue": "graphcensus#TestTheVersionBatchTakesTheStoreLockOrder",
	// The backfill's pass over the waits a process of the release before wrote with no node
	// (this branch's spec): the issue rows the batch's links stand under first, then the links
	// read again under them, then their needs tuples through Write, then the rows naming the issue's
	// node (graphcensus' TestTheIssueLinkBatchTakesTheStoreLockOrder).
	"graphcensus/census_issues.go#writeIssueLinkBatch": "graphcensus#TestTheIssueLinkBatchTakesTheStoreLockOrder",
	// The backfill's pass over the comments' anchor blocks (T17b of this branch's plan):
	// the owner rows the batch's comments stand under first, then the comments' rows and their
	// nodes' anchored_in read again under them, then the tuples that level the two through Write
	// at the sentinel (graphcensus' TestTheAnchorBatchTakesTheStoreLockOrder).
	"graphcensus/census_anchors.go#writeAnchorBatch": "graphcensus#TestTheAnchorBatchTakesTheStoreLockOrder",
	// The backfill's Keep pass (R32 of this branch's spec): the owner rows the batch's Keeps stand
	// under first, then the Keeps and their decisions' words read again under them, then the
	// decision_keeps rows whose sum is still the one over the words with shaped, the kept_text_sum
	// values through Write at the sentinel, and the head rows (graphcensus'
	// TestTheKeepSumBatchTakesTheStoreLockOrder).
	"graphcensus/census_keeps.go#writeKeepSumBatch": "graphcensus#TestTheKeepSumBatchTakesTheStoreLockOrder",
	// The backfill's pass over the references a document makes: the owner rows the batch's
	// documents stand under first, then the refs rows and their documents' nodes' mentions read
	// again under them, then the mentions tuples each row owes through Write at the sentinel
	// (graphcensus' TestTheReferenceBatchTakesTheStoreLockOrder).
	"graphcensus/census_refs.go#writeReferenceBatch": "graphcensus#TestTheReferenceBatchTakesTheStoreLockOrder",
	// A recorded result (T11 of this branch's plan, R12): the record route's transaction takes the
	// owner locks of the recording issue and of the check's issue (lockIssueOwners), appends
	// result.recorded, and then writes, in one batch, the check_results row and the result's node,
	// the recorder's nodes and every tuple caused by that event (writeRecordedResult); a
	// retraction takes the check issue's owner lock, reads whether the store holds the result's
	// node under it, appends result.retracted, and then writes, in one batch, the
	// check_result_retractions row and the result's retracted value under that event
	// (writeCheckResultRetraction). api's
	// TestEveryApiStoreWriterTakesTheStoreLockOrder holds both to those steps. The backfill's passes
	// over the results and the retractions an earlier release wrote take the owner locks of the
	// batch's check issues first, then read the rows again under them, then write the nodes and
	// tuples, or the retracted values, in one Write at the sentinel (graphcensus'
	// TestTheResultBatchTakesTheStoreLockOrder and TestTheResultRetractionBatchTakesTheStoreLockOrder).
	"api/results.go#writeRecordedResult":                       "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"api/results_retract.go#writeCheckResultRetraction":        "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"graphcensus/census_results.go#writeResultBatch":           "graphcensus#TestTheResultBatchTakesTheStoreLockOrder",
	"graphcensus/census_results.go#writeResultRetractionBatch": "graphcensus#TestTheResultRetractionBatchTakesTheStoreLockOrder",
	// A query's version a write owes where it moved the query's record or its document's rendering
	// and wrote that document no version: the version's row, its node
	// (docs/mutation.go#WriteVersionNode, above) and its version_renders row, and last the node's
	// facts, through Caused. A recorded result, a retraction and an issue room's settlement write
	// no such version in their own transaction: their batch marks the issue's queries owed, and the
	// drain writes the versions in a transaction of its own once they have committed
	// (drainQueryMark), as the block-index tick does for a mark a write left. It takes the issue's
	// owner lock, then the lock of its room (docs.LockQueryRooms), reads, and sends one batch: the
	// rows, the move of an open approval request, a carry, the artifact.version, the clear of the
	// mark, then one call of Caused under the mark's event and the head rows, after the rows its
	// path holds, whose order refs' TestCausedTakesTheStoreLockOrder proves.
	"docs/query_marks.go#drainQueryMark": "refs#TestCausedTakesTheStoreLockOrder",
	// R2's daemon-thread step (docs.DaemonThreadStep): it reads with no lock the blocks that stand
	// under a message of a thread the Legion daemon's session began, then takes every owner lock of
	// their issues in one call of store.LockOwners and each of their rooms' locks in room-id order,
	// reads each block's part_of again, and sends one batch: the retraction and the part_of to the
	// issue's node, the query mark and the head rows.
	"docs/daemon_thread_step.go#queueDaemonThreadMoves": "docs#TestANodeUnderADaemonThreadMovesUnderItsIssue",
	// R6's conversion of an issue into its room (docs.ConvertIssue), one transaction an issue
	// under its owner lock and then each room's lock in sorted order: the tree, the query nodes and
	// the decisions query, the moves of the blocks, the asks and the pointers at the old addresses,
	// then the facts its passes stated, caused under each document's artifact.version.
	"docs/issue_conversion.go#writeTree":           "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	"docs/issue_conversion.go#writeQueries":        "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	"docs/issue_conversion.go#writeDecisionsQuery": "refs#TestCausedTakesTheStoreLockOrder",
	"docs/issue_conversion.go#writeMoves":          "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	"docs/issue_conversion.go#moveAsks":            "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	"docs/issue_conversion.go#movePointers":        "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	"docs/issue_conversion.go#finish":              "refs#TestCausedTakesTheStoreLockOrder",
	// The backfill's passes over the components a process of the release before wrote with no
	// node (R9 of this branch's spec): the architecture source rows of the batch's projects first,
	// in ascending (project, id), then the rows read again under them, then the component nodes and
	// their part_of, or the depends_on of a component whose node stands, in one Write at the
	// sentinel, then the rows naming the node; and over the issues' components, the issue rows
	// first, then the rows read again under them, then their affects through Write, then the rows
	// naming the issue's node (graphcensus' TestTheComponentBatchesTakeTheStoreLockOrder).
	"graphcensus/census_components.go#writeComponentBatch":            "graphcensus#TestTheComponentBatchesTakeTheStoreLockOrder",
	"graphcensus/census_components.go#writeComponentDependsBatch":     "graphcensus#TestTheComponentBatchesTakeTheStoreLockOrder",
	"graphcensus/census_components.go#writeIssueComponentMemberBatch": "graphcensus#TestTheComponentBatchesTakeTheStoreLockOrder",
	// A component's node and its relations as an import and a source's removal state them
	// (R9 of this branch's spec): the import's one Write of every component node of the source,
	// each component it drops stated gone, and the part_of and depends_on that moved, at the
	// sentinel, after the row locks of every source of the project and before the components and
	// component_depends rows it replaces (architecture's TestTheImportTakesTheStoreLockOrder); and
	// a source's removal's Write of each of its components gone with every relation it held
	// retracted, after the source's row and before the delete whose cascade takes the rows (api's
	// TestEveryApiStoreWriterTakesTheStoreLockOrder).
	"architecture/importer.go#RetireComponents": "api#TestEveryApiStoreWriterTakesTheStoreLockOrder",
	"architecture/importer.go#stateComponents":  "architecture#TestTheImportTakesTheStoreLockOrder",
	// The block-index tick's sweep of every stale lens, after its orphan sweep, in a transaction of
	// its own: a read of the stale lenses with no lock, then the owner lock of each issue a stale
	// lens's document names, then the read again under those locks, and one Write of the lenses it
	// kept and the nodes beneath them in ascending (kind, id) and their state tuples at the sentinel,
	// appending no event. Its driver sweeps a stale lens with a finding and a risk under it and holds
	// each step in turn - the owner lock, the three node rows, the state tuples - and the project's
	// row, which the sweep never takes.
	"graphcensus/orphans.go#StateStaleLensesGone": "graphcensus#TestTheStaleLensSweepTakesTheStoreLockOrder",
	// graphstore's own writers that run another of them. QueueAuthored and QueueSessionRecorded
	// are arms of one statement, and QueueReached is QueueSessionRecorded for an attempt that was
	// sent. Write and Caused run QueueWrite and QueueCaused as a batch of their own, and
	// TestEveryStoreWriteTakesOneLockOrder proves the order on them. WriteState runs one Write of
	// the nodes whose state it moves, after the reads of what stands beneath those it takes gone,
	// which take no lock, so its order is the order that test proves of Write, after the locks its
	// caller holds.
	"graphstore/people.go#QueueAuthored":        "graphstore#TestAuthoredAndReachedTakeTheStoreLockOrder",
	"graphstore/people.go#QueueReached":         "graphstore#TestAuthoredAndReachedTakeTheStoreLockOrder",
	"graphstore/people.go#QueueSessionRecorded": "graphstore#TestAuthoredAndReachedTakeTheStoreLockOrder",
	"graphstore/write.go#Caused":                "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	"graphstore/write.go#Write":                 "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	"graphstore/write.go#WriteState":            "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	// Retarget moves one pointer through Write, after the row of the node it moves it out of
	// (planRetarget, its read phase).
	"graphstore/reasoning.go#Retarget": "graphstore#TestEveryStoreWriteTakesOneLockOrder",
	// AddLensKind adds one kind to the lens catalogue through Write: the kind's constraint and
	// lens_kind nodes in ascending (kind, id) and the constrained_by tuple between them, at the
	// sentinel, under a transaction lock on the id and no owner row, since a catalogue node
	// stands under no issue. Its order is the order TestEveryStoreWriteTakesOneLockOrder proves of
	// Write; `envoy-dispatch lens-kind-add` is its one caller.
	"graphstore/lens_catalogue.go#AddLensKind": "graphstore#TestEveryStoreWriteTakesOneLockOrder",
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
	// The publisher's record of a batch, which splices the store's arms into its statement, and
	// its record of a fallback pass, which splices in what each published copy delivered.
	"outbox/mentions.go#recordFallbackCopies": "outbox#TestAFallbackCopyThePassPublishedIsADelivery",
	"outbox/publisher.go#recordBatch":         "outbox#TestRecordBatchTakesAPrefixOfTheStoreLockOrder",
	// The block index: its nodes inside the revision fence, queued on the write phase of the
	// writer that runs it (BlockIndexing.Queue), with the typed-node pass's own nodes and their
	// authors' (refs.PlanBlocks' typed facts), and the person and session nodes of the authors of
	// each block whose node records them in the same node write, whose tuples and cause wait for
	// the event that recorded the version (the sites of QueueCaused above), driven inside the tick
	// and the settlement that run it. Above the deferral bound the typed-node pass runs on its own
	// (refs.DecisionIndexing.Queue), under the document's owner lock its caller holds, its tuples
	// and cause waiting for the same event.
	"refs/blocks.go#Queue":         "docs#TestEveryDocsStoreWriterTakesTheStoreLockOrder",
	"refs/decision_nodes.go#Queue": "docs#TestEverySettlementWriterTakesTheStoreLockOrder",
	// rebuild-refs' reconcile of one source: a document's owner row first (lockDocumentOwner),
	// then its refs rows, then the mentions tuples of the rows the reconcile inserted and deleted,
	// through Caused at the sentinel, since no event records the rebuild (refs'
	// TestARebuildOfADocumentsReferencesTakesTheStoreLockOrder).
	"refs/rebuild.go#replaceInTx": "refs#TestARebuildOfADocumentsReferencesTakesTheStoreLockOrder",
}
