package graphstore

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot is the packages/envoy module, read from this package's own directory.
const moduleRoot = "../../.."

// graphstoreImportPath is the path a file outside this package names the store by.
const graphstoreImportPath = "github.com/sjawhar/envoy/internal/dispatch/graphstore"

// graphstoreDirectory is this package's own directory under moduleRoot, where its writers are
// named without a package.
const graphstoreDirectory = "internal/dispatch/graphstore"

// graphstoretestDirectory is the one package besides the store's own whose statements write its
// tables: graphstoretest, which seeds for other packages' tests the rows the store's writers
// refuse to write, and which no deployment builds. Any other package under the store's directory
// is held to the rule like every package outside it.
const graphstoretestDirectory = graphstoreDirectory + "/graphstoretest"

// storeWriteFunctions are the calls a write of graph_nodes and graph_tuples from outside this
// package goes through, the same thirteen docs' register of the tree's passes scans for
// (storeWriterCall): Write, WriteUncaused and Caused in a caller's transaction, WriteArms and
// WriteArmsUncaused spliced into a caller's statement, Authored, Reached and SessionRecorded,
// which run those arms for the writers that record an author, a reached session and a session
// an ask names, EraseNode, the erase's delete of a node and its tuples, RedactValues, the erase's
// rewrite of the quote of an erased block in the text values of the blocks holding it,
// FillScopes, the backfill's fill of the project of a node stored with none, Retarget, the move
// of one pointer tuple of a node from one far end to another, and QueueFrameTuple, Write's append
// of one frame value of a run queued on a caller's batch. Each is a function of the package,
// never a method, so a call of one is named by the package and found here wherever it is made. A
// write that holds a statement of its own instead is found by rawStoreWrite, and none outside
// this package may hold one.
var storeWriteFunctions = []string{"Write", "WriteUncaused", "WriteArms", "WriteArmsUncaused", "Caused",
	"Authored", "Reached", "SessionRecorded", "EraseNode", "RedactValues", "FillScopes", "Retarget", "QueueFrameTuple"}

// rawStoreWrite matches a statement that writes graph_nodes or graph_tuples itself rather than
// through one of storeWriteFunctions: an insert, an update or a delete, in any case, over any
// whitespace, and under the schema's name as well as bare.
var rawStoreWrite = regexp.MustCompile(`(?is)\b(?:insert\s+into|update|delete\s+from)\s+(?:public\s*\.\s*)?graph_(?:nodes|tuples)\b`)

// storeWriteCallSites is every function outside the tests that calls one of storeWriteFunctions,
// named as docs' register names a pass: the file, from internal/dispatch where it lives under it,
// and the function, without its receiver. TestEveryStoreWriteTakesOneLockOrder proves the lock
// order on this package's own writers, and each site listed here has its lock steps proven in its
// own package against that order; a site that is not listed has had nobody prove its steps, so it
// fails here until it joins this list.
var storeWriteCallSites = []string{
	// The follow of an ask, which records the following session's node in the caller's
	// transaction, and each site that moves that node onto the event it appends after the
	// follow, and appends the node's base attributes under it, through Caused: the follower
	// route, an ask's create, an approval request renewed, and a settlement's asks (asks'
	// TestAFollowTakesTheStoreLockOrder proves both steps). The comment path and a delivery's
	// reply give the follow their own event instead, which it states in its own statement.
	"asks/followers.go#Follow",
	"api/ask_followers.go#changeAskFollower",
	"api/asks.go#createAskFor",
	"api/reviews.go#renewApprovalAsk",
	"docs/ask_blocks.go#causeFollows",
	// The shaped ties of each ask block a settlement opened, when it writes no version: appended
	// under the ask.opened that records the ask, through Caused, after the follow's node above.
	"docs/ask_blocks.go#causeOpenedAskTies",
	// The block index's cause: each site that appends what a version's index pass stated under
	// the event that recorded the version, through Caused (refs' TestCausedTakesTheStoreLockOrder
	// proves the step): an upload, a version write's event, an issue's spec, the block-index tick,
	// a settlement's version, an issue room's pass under each of its documents' events, and the
	// seed's documents.
	"api/artifacts.go#commitArtifactVersionEvent",
	"api/artifacts.go#storeArtifact",
	"api/issue_create.go#createIssue",
	"cmd/dispatch-seed/write_docs.go#writeDocument",
	"docs/block_index.go#indexOwedDocument",
	"docs/block_index.go#causeRoomFacts",
	"docs/settlement.go#commitVersion",
	// The comment path: the comment's node before the comments row that names it, then its
	// tuples and its cause under the event appended after both, and who wrote it.
	"api/comment_create.go#caused",
	"api/comment_create.go#createCommentFor",
	"api/comment_create.go#writeNode",
	// A delivery's reply and the settle of a delivery attempt, which records the session the
	// attempt reached, and the one record both replies share of what their answer makes of the
	// attempt they answer: an attempt the answer flips to sent is one the listener took, and
	// its session is owed a node (recordAnsweredDelivery).
	"api/comment_delivery.go#completeCommentDelivery",
	"api/comment_delivery.go#recordAnsweredDelivery",
	"api/comment_delivery.go#replyComment",
	"api/message_delivery.go#completeMessageDelivery",
	// The nodes and tuples of the reasoning kinds a request writes - a lens, its findings, a
	// risk and a constraint - with the author's node where the credential proves one: the issue's
	// owner row, then one Write of every node in ascending (kind, id) and every tuple at the
	// sentinel, appending no event. Its order is the order TestEveryStoreWriteTakesOneLockOrder
	// proves of Write.
	"api/reasoning_write.go#writeReasoningNodes",
	// One pointer tuple a request appends, retracts or retargets out of a node of an issue
	// (has_kind, triggered_by, constrained_by, prevented_by): the issue's owner row, then one
	// Write of the tuple, or Retarget, which takes the node's row and writes through Write. It
	// appends no event. Its order is the order TestEveryStoreWriteTakesOneLockOrder proves of
	// Write, after the owner row writeReasoningNodes takes first too.
	"api/reasoning_write.go#writeReasoningPointer",
	// One node's state a request writes or settles (api/node_state.go): the issue's owner row,
	// then one Write of the node as the store holds it but for its state, a plain write that
	// moves the row and appends the state tuple. It appends no event. Its order is the order
	// TestEveryStoreWriteTakesOneLockOrder proves of Write, after the owner row writeReasoningNodes
	// takes first too.
	"api/reasoning_write.go#writeNodeState",
	// An issue's own field write made for a run (api/issue_patch.go), which names the issue changed:
	// the issue's owner row (lockPatchRows), the issue's row, then, on a context that carries a
	// run alone, the issue's node through WriteUncaused, the issue's own event, and what the node
	// states with its changed_by_run under that event through Caused. api's
	// TestEveryApiStoreWriterTakesTheStoreLockOrder holds it to those steps.
	"api/issue_patch.go#patchIssue",
	// An issue's relations (dispatch://INFRA-1253 R9, R10), which its create and its PATCH state:
	// after the issue's owner row, its row and its issue_links rows, the node of the item its
	// filed_from names where the store held none, through Write, with a comment's row named after
	// it (stateFiledFromNode), then the issue's own node through WriteUncaused (createIssue's and
	// patchIssue's), the issue's event, and last the relations that event causes, its child_of,
	// needs, duplicate_of and filed_from and the part_of it no longer hangs from, in one statement
	// of the store's arms (stateIssueRelations). api's TestEveryApiStoreWriterTakesTheStoreLockOrder
	// holds a PATCH of its needs, and a PATCH of its filed_from at a comment with no node, to those
	// steps.
	"api/issue_relations.go#stateFiledFromNode",
	"api/issue_relations.go#stateIssueRelations",
	// An edit of an ask's or a comment's words made for a run (api/asks.go, api/comment_actions.go),
	// which names the ask or the comment changed: the owner row of the ask or the comment, its row
	// `for no key update` or `for update` and then its update, then, on a context that carries a
	// run alone, its node through WriteUncaused, its own event, and what the node states with its
	// changed_by_run under that event through Caused. api's
	// TestEveryApiStoreWriterTakesTheStoreLockOrder holds each to those steps.
	"api/asks.go#editAsk",
	"api/comment_actions.go#editComment",
	// A run of the command bar (api/going.go), which takes no owner row: its record writes the
	// interaction node, its author's node and their tuples in one Write before the going_requests
	// row, and each frame, in a transaction of its own, takes the run's interaction node row
	// `for no key update`, then the run's row `for key share`, then appends the frame value through
	// Write before the going_events row. api's TestEveryApiStoreWriterTakesTheStoreLockOrder holds
	// both to their steps. A frame of a run a pod of the release before recorded with no node
	// writes that node first through the backfill's writer for one run, which takes the run's
	// going_requests row `for no key update` and then writes in one Write; its order is the order
	// TestEveryStoreWriteTakesOneLockOrder proves of Write.
	"api/going.go#appendGoingEvent",
	"api/going.go#recordGoingRequest",
	"graphcensus/interactions.go#writeRun",
	// The backfill's passes over what a process of the release before writes of a run during a
	// roll (graphcensus' TestTheBackfillMendsWhatAnOlderPodWroteOfARun): a frame it appended with
	// no value, stated through Write after the run's interaction node row `for no key update`, the
	// frame path's own first lock, and a run whose row it deleted, whose node is written gone
	// through Write after that same row. Neither takes an owner row, as no run stands under one;
	// the order is the order TestEveryStoreWriteTakesOneLockOrder proves of Write.
	"graphcensus/backfill.go#writeGoneRun",
	"graphcensus/backfill.go#writeRunFrames",
	// The node of every issue, document, ask and message a request creates, in the project or
	// between the parties it belongs to: written before its row, and
	// caused by the event the create appends after it. createIssue, storeArtifact and
	// createAskFor are listed above for their block index and follow; these are the others. Their
	// order - the owner row their path already takes, the node before the row, the event before
	// the tuples - is the order of the write they share (WriteUncaused, then Caused), which
	// TestEveryStoreWriteTakesOneLockOrder proves on this package's own writers, and the node
	// before its row is held at each site by api's TestEveryCreateStoresItsNode and the node-first
	// trigger of object_nodes_issue_test.go and object_nodes_items_test.go. statedVersionBytes is
	// the bytes of an image's or a file's later version, stated on the node its row names with no
	// node written, after the owner row and that row read FOR SHARE and before the event
	// storeArtifact appends, which causes them (dispatch://INFRA-1253; api's
	// TestALaterVersionStatesItsBytesInTheStoreLockOrder proves its steps, and that a later version
	// waits for a backfill naming its node). removedDocumentNodes is a removal's nodes of the
	// document and of its versions stated gone, after the issue's row and before any row of the
	// document is deleted, and removeArtifact appends what it stated under the artifact.removed
	// event, through Caused (api's TestARemovalStatesItsDocumentGoneInTheStoreLockOrder).
	// createIssueTree is an issue's tree artifact (dispatch://INFRA-1253 R5): its node and its
	// part_of to the issue through WriteUncaused before the artifacts row that names the node and
	// the issue_trees row, in its caller's transaction, which holds the issue's row and causes what
	// the write stated under the event it appends - the order of the write it shares with
	// createIssue's spec (WriteUncaused, then the row), which TestEveryStoreWriteTakesOneLockOrder
	// proves of WriteUncaused.
	"api/artifact_removal.go#removeArtifact",
	"api/artifact_removal.go#removedDocumentNodes",
	"api/artifacts.go#createIssueTree",
	"api/artifacts.go#statedVersionBytes",
	"api/broadcasts.go#writeBroadcast",
	"api/message_delivery.go#insertSessionReply",
	"api/messages.go#createStoredMessage",
	"api/messages.go#writeMessageNode",
	"api/reviews.go#openApprovalAsk",
	"docs/ask_blocks.go#createAskBlock",
	// The node of every version a write inserts, in the row's transaction (dispatch://INFRA-1253):
	// the one writer every insert of a version calls (docs.WriteVersionNode) - a version write
	// (writeVersionTx), an upload (api's writeDocumentVersion and storeArtifact) and an issue's spec
	// (api's createIssue) - after the owner row its caller holds and the block nodes of the
	// version's index, with its facts held for the event the caller appends and causes them with,
	// through Caused at the sites listed above. Its address is drawn from the version's own number,
	// so no other transaction holds it; api's TestEveryObjectNodeWriterTakesTheStoreLockOrder and
	// TestALaterVersionStatesItsBytesInTheStoreLockOrder hold an upload's and an issue create's
	// steps around it, and api's TestEveryVersionRowIsTheProjectionOfAVersionNode holds its node to
	// its row's transaction at each site.
	"docs/mutation.go#WriteVersionNode",
	// A document's retirement (dispatch://INFRA-1150 R11), in the same order under the document's
	// owner row: its artifact node stated gone, then the artifacts row that retires it, then
	// document.retired and the node's facts under it. A removal of an untouched document states
	// its node gone through the same writer (removedDocumentNodes, listed above, through
	// docs.WriteRemovedNode), with each changed_by_run of the node retracted, under the owner row
	// and the room, then deletes its rows, appends artifact.removed and the node's facts under it
	// (api's TestEveryApiStoreWriterTakesTheStoreLockOrder holds it to those steps).
	"docs/retire.go#RetireDocument",
	"docs/retire.go#writeRetiredNode",
	// A person's Keep of an agent's decision (dispatch://INFRA-1150 R2): under the issue's owner
	// row, the person's node, then the decision_keeps row that projects the Keep, then
	// decision.kept, and the kept_by tuple and the kept_text_sum value under it, through Caused.
	"api/decisions.go#keepDecisionNode",
	// The decisions step's restore and conversion of an issue (dispatch://INFRA-1150 R6 to R9),
	// under the issue's owner row and its asks' rows: each document's version and its event, the
	// index pass's facts under that event through Caused, then the moves, the restored asks'
	// relations and the carried Keeps, with the person node a Keep names, under the spec's event.
	"docs/decision_restore.go#writeVersion",
	"docs/decision_restore.go#writeFacts",
	// The seed's comments, with their author's person node and each comment's authored_by
	// tuple, which its one batch write states as a seeded database's own writer, the session
	// nodes of the followers its ask batch copies, and its issues', asks' and messages' nodes,
	// each written in the batch that copies their rows, in its project, before any row of it.
	"cmd/dispatch-seed/write_core.go#writeIssues",
	"cmd/dispatch-seed/write_items.go#writeComments",
	"cmd/dispatch-seed/write_items.go#writeFollowerNodes",
	"cmd/dispatch-seed/write_items.go#writeItemNodes",
	// The backfill command's five batches: the blocks, the block references and the comments
	// written before the store existed, the role and page nodes of the roles the census holds to
	// one (addressedRoles), and the session nodes of the sessions it holds to one
	// (recordedSessions).
	"graphcensus/backfill.go#writeBlockBatch",
	"graphcensus/backfill.go#writeBlockRefBatch",
	"graphcensus/backfill.go#writeCommentBatch",
	"graphcensus/backfill.go#writeRoleBatch",
	"graphcensus/backfill.go#writeSessionBatch",
	// The backfill's passes over the project of every node: the nodes of the issues, documents,
	// asks and messages written before their writers wrote one, and the project of the comment and
	// block nodes written before a node named one (graphstore.FillScopes), the block batch under the
	// revision fence (lockBlockDocuments). graphcensus' TestTheBackfillGivesEveryRowItsNodeAndProject
	// runs both. An issue's or a document's batch takes the owner rows it stands under first, then
	// its node rows, then, for an issue, the child_of claim of the parent its row names under that
	// lock through WriteArms, which appends at the sentinel, outside every repeat key, and so waits
	// on no row, then names each node on its row (dispatch://INFRA-1253), which graphcensus'
	// TestTheObjectBatchTakesTheStoreLockOrder proves; an ask's or a message's takes no other row.
	"graphcensus/projects.go#writeObjectBatch",
	"graphcensus/projects.go#writeScopeBatch",
	// The backfill's pass over the versions an older build stored with no node
	// (dispatch://INFRA-1253): the owner rows of the batch's documents first, then the version
	// rows read again under them, then the version nodes and their facts, in one graphstore.Write
	// per batch (graphcensus' TestTheVersionBatchTakesTheStoreLockOrder).
	"graphcensus/census_versions.go#writeVersionBatch",
	// The backfill's pass over the waits a process of the release before wrote with no node
	// (dispatch://INFRA-1253): the issue rows the batch's links stand under first, then the links
	// read again under them, then their needs tuples through Write, then the rows naming the issue's
	// node (graphcensus' TestTheIssueLinkBatchTakesTheStoreLockOrder).
	"graphcensus/census_issues.go#writeIssueLinkBatch",
	// The erase, the one writer that deletes from the store, through EraseNode: the tuples on both
	// sides of the node, then the node's row. It takes the owner rows it writes under before that
	// row, and its projection rows after it (grapherase's TestTheEraseTakesTheStoreLockOrder).
	"grapherase/erase.go#apply",
	// The erase's end of the versions it deletes: each one's node stated gone, in the erase's
	// transaction, under the owner rows the erase takes first (dispatch://INFRA-1253; grapherase's
	// TestTheEraseOfABlockAVersionHeldEndsThatVersionsNode).
	"grapherase/erase.go#endErasedVersions",
	// The erase's rewrite of the text values of the blocks holding the erased block, through
	// RedactValues, in the erase's transaction and under the owner rows it takes first, beside its
	// rewrite of those blocks' document_block_texts rows (dispatch://INFRA-1253; grapherase's
	// TestTheEraseTakesTheStoreLockOrder).
	"grapherase/redaction.go#redactBlocks",
	// This package's own writers that run another of them: Authored and SessionRecorded are
	// arms of one statement, and Reached is SessionRecorded for an attempt that was sent.
	"graphstore/people.go#Authored",
	"graphstore/people.go#Reached",
	"graphstore/people.go#SessionRecorded",
	// The one writer of an ask's answer state (dispatch://INFRA-1150): asks.Settle states the answer
	// on the ask's node, appended at once under the event it is given or held for the caller's, and
	// then writes the row; the catch-up of a follow another build recorded states it on the node.
	// Each site that appends what a transition held under the event it appends after it, through
	// Caused: the answer, resolve and decline routes' transition, an issue's close and reopen, the
	// approval carry's answer, and a settlement's retractions and restorations. The settlement
	// states the copies among a document's ask blocks as claims once its version is indexed, and
	// beside them the readability of each ask whose block it read, as that ask node's `invalid`
	// value (G1), and the backfill of the boot step's first phase states the asks and the
	// followers written before this release, each batch with its rows locked first.
	"api/ask_transitions.go#closeAskTx",
	"api/issue_ask_lifecycle.go#recordWithdrawnAsksTx",
	"api/issue_ask_lifecycle.go#restoreIssueAsksTx",
	"asks/followers.go#CatchUpFollower",
	"asks/settle.go#Settle",
	"docs/approval_carry.go#answerApprovalAsk",
	"docs/ask_blocks.go#causeSettledAnswers",
	"docs/ask_blocks.go#claimCopies",
	"docs/ask_blocks.go#stateInvalidReasons",
	"graphcensus/backfill.go#writeAskBatch",
	"graphcensus/backfill.go#writeAskFollowerBatch",
	// Retarget moves one pointer through Write, after the row of the node it moves it out of.
	"graphstore/reasoning.go#Retarget",
	// AddLensKind adds one kind to the lens catalogue through Write: the kind's constraint and
	// lens_kind nodes in ascending (kind, id) and the constrained_by tuple between them, at the
	// sentinel, under a transaction lock on the id and no owner row, since a catalogue node
	// stands under no issue. Its order is the order TestEveryStoreWriteTakesOneLockOrder proves of
	// Write; `envoy-dispatch lens-kind-add` is its one caller.
	"graphstore/lens_catalogue.go#AddLensKind",
	// The publisher's record of a batch, which splices the store's arms into its statement, and
	// its record of a fallback pass, which splices in what each published copy delivered.
	"outbox/mentions.go#recordFallbackCopies",
	"outbox/publisher.go#recordBatch",
	// The block index: its nodes inside the revision fence, with the person and session nodes of
	// the authors of each block whose node records them (dispatch://INFRA-1253's T7) in the same
	// node write, whose tuples and cause wait for the event that recorded the version (the sites
	// of Caused above).
	"refs/blocks.go#IndexBlocks",
	// The typed-node pass (dispatch://INFRA-1150): the decision and reading nodes of a version and
	// their authors' nodes, in the transaction that accepts it, under the document's owner row its
	// caller holds, after the block index's own nodes where that runs and on its own above the
	// deferral bound, its tuples and cause waiting for the same event (the sites of Caused above).
	"refs/decision_nodes.go#IndexDecisionNodes",
}

// TestEveryStoreWriteCallSiteIsListed reads the module's source, every Go file but the tests,
// and finds each function that names one of storeWriteFunctions, called or passed on as a value,
// and each function outside this package whose string literals hold a statement writing
// graph_nodes or graph_tuples (rawStoreWrite). It fails on a site storeWriteCallSites does not
// list, naming the site and the call, so a new writer of the store cannot appear without its lock
// steps being proven, and on a listed site the source no longer holds, so the list never names a
// writer that is gone. It fails on every statement outside this package that writes the store,
// naming the site and the statement: this package is the one place that writes those tables, so
// such a statement moves behind a writer here. It parses the source rather than matching its
// text, so a call written over two lines, a comment that names a writer or spells a statement,
// and a method of another type that shares a writer's name are each read for what they are.
func TestEveryStoreWriteCallSiteIsListed(t *testing.T) {
	found, raw := map[string][]string{}, map[string][]string{}
	parsed := 0
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != moduleRoot && skippedDirectory(path, entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// A file that names the store names it by its import path or is the store's own
		// package, and both spell the word; a file that writes the store's tables itself spells
		// one of their names, in whatever case its statement is written in. The rest of the
		// module is never parsed.
		lowered := bytes.ToLower(source)
		if !bytes.Contains(source, []byte("graphstore")) &&
			!bytes.Contains(lowered, []byte("graph_nodes")) && !bytes.Contains(lowered, []byte("graph_tuples")) {
			return nil
		}
		relative, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		files := token.NewFileSet()
		file, err := parser.ParseFile(files, relative, source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", relative, err)
		}
		parsed++
		for site, calls := range storeWriteSitesIn(relative, files, file) {
			found[site] = append(found[site], calls...)
		}
		for site, statements := range rawStoreWritesIn(relative, files, file) {
			raw[site] = append(raw[site], statements...)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's source under %s: %v", moduleRoot, err)
	}
	if parsed == 0 || len(found) == 0 {
		t.Fatalf("read %d files naming graphstore or its tables and found %d store writers: the walk read nothing worth checking", parsed, len(found))
	}
	t.Logf("read %d files naming graphstore or its tables under %s and found %d store writer sites", parsed, moduleRoot, len(found))

	sites := make([]string, 0, len(found))
	for site := range found {
		sites = append(sites, site)
	}
	slices.Sort(sites)
	for _, site := range sites {
		t.Logf("store writer %s (%s)", site, strings.Join(found[site], ", "))
		if !slices.Contains(storeWriteCallSites, site) {
			t.Errorf("%s writes the store (%s) and is not in storeWriteCallSites: a new writer of graph_nodes and graph_tuples takes the lock order written on api/inbox_read.go's markInboxReadThreads, so its steps are proven in its own package and then it is listed here",
				site, strings.Join(found[site], ", "))
		}
	}
	for _, site := range storeWriteCallSites {
		if _, ok := found[site]; !ok {
			t.Errorf("storeWriteCallSites lists %s, and no file of the module holds a call of a store writer or a statement writing the store there any more: take it off the list, since the proofs of the writers rest on it naming the ones there are", site)
		}
	}
	for _, site := range slices.Sorted(maps.Keys(raw)) {
		t.Errorf("%s writes the store with a statement of its own (%s): graphstore is the one place that writes graph_nodes and graph_tuples, so the statement moves behind one of its writers (storeWriteFunctions) and the site calls that writer",
			site, strings.Join(raw[site], ", "))
	}
}

// skippedDirectory is a directory the walk does not enter: one Go itself ignores (testdata, a
// name starting with a dot or an underscore), a vendored or installed tree, and another module.
func skippedDirectory(path, name string) bool {
	if name == "testdata" || name == "vendor" || name == "node_modules" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	_, err := os.Stat(filepath.Join(path, "go.mod"))
	return err == nil
}

// storeWriteSitesIn is every site one file names a store writer at, each with the calls it makes
// as "graphstore.<writer> at <file>:<line>". In the store's own package the writers are named
// bare; anywhere else through the name the file imports graphstore under.
func storeWriteSitesIn(relative string, files *token.FileSet, file *ast.File) map[string][]string {
	bare := filepath.ToSlash(filepath.Dir(relative)) == graphstoreDirectory && file.Name.Name == "graphstore"
	qualifier := ""
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != graphstoreImportPath {
			continue
		}
		switch {
		case spec.Name == nil:
			qualifier = "graphstore"
		case spec.Name.Name == ".":
			bare = true
		case spec.Name.Name != "_":
			qualifier = spec.Name.Name
		}
	}
	if !bare && qualifier == "" {
		return nil
	}

	sites := map[string][]string{}
	for _, declaration := range file.Decls {
		site := siteKey(relative, "package-level declaration")
		var body ast.Node = declaration
		if decl, ok := declaration.(*ast.FuncDecl); ok {
			site = siteKey(relative, decl.Name.Name)
			if decl.Body == nil {
				continue
			}
			body = decl.Body
		}
		record := func(name *ast.Ident) {
			line := files.Position(name.Pos()).Line
			sites[site] = append(sites[site], "graphstore."+name.Name+" at "+relative+":"+strconv.Itoa(line))
		}
		var visit func(node ast.Node) bool
		visit = func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if x, ok := node.X.(*ast.Ident); ok && qualifier != "" && x.Name == qualifier &&
					slices.Contains(storeWriteFunctions, node.Sel.Name) {
					record(node.Sel)
					return false
				}
				// The name after a dot is a field or a method of something else, never the
				// store's own function, so only the expression before it is read on.
				ast.Inspect(node.X, visit)
				return false
			case *ast.Ident:
				// Bare, a writer is named only in the store's own package or in a file that
				// dot-imports it.
				if bare && slices.Contains(storeWriteFunctions, node.Name) {
					record(node)
				}
			}
			return true
		}
		ast.Inspect(body, visit)
	}
	return sites
}

// rawStoreWritesIn is every site one file writes the store at with a statement of its own, each
// with the statements it holds as "<statement> at <file>:<line>", read from the file's string
// literals alone, so a statement a comment spells is none. The store's own package writes its
// tables that way by design, through the writers storeWriteFunctions names, and graphstoretest
// seeds what those writers refuse (graphstoretestDirectory), so the files of those two packages
// alone are not read here.
func rawStoreWritesIn(relative string, files *token.FileSet, file *ast.File) map[string][]string {
	if directory := filepath.ToSlash(filepath.Dir(relative)); directory == graphstoreDirectory || directory == graphstoretestDirectory {
		return nil
	}
	sites := map[string][]string{}
	for _, declaration := range file.Decls {
		site := siteKey(relative, "package-level declaration")
		if decl, ok := declaration.(*ast.FuncDecl); ok {
			site = siteKey(relative, decl.Name.Name)
		}
		ast.Inspect(declaration, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			for _, match := range rawStoreWrite.FindAllStringIndex(text, -1) {
				// A raw string holds its lines as the source does, so a statement inside one is
				// on the literal's line plus the line breaks before it.
				line := files.Position(literal.Pos()).Line
				if strings.HasPrefix(literal.Value, "`") {
					line += strings.Count(text[:match[0]], "\n")
				}
				statement := strings.ToLower(strings.Join(strings.Fields(text[match[0]:match[1]]), " "))
				sites[site] = append(sites[site], statement+" at "+relative+":"+strconv.Itoa(line))
			}
			return true
		})
	}
	return sites
}

// siteKey names a site as docs' register of the tree's passes names one (passKey): the file,
// from internal/dispatch where it lives under it and from the module otherwise, and the function.
func siteKey(relative, function string) string {
	return strings.TrimPrefix(relative, "internal/dispatch/") + "#" + function
}
