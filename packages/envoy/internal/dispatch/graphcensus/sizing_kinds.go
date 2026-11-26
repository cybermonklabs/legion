package graphcensus

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The five children of the parent change whose writes the sizing sums (R4), each with the kinds
// R20 gives it: this one; the one that moves decisions, readings, asks and their answers; the one
// that moves the inbox, the messages and the read state; the one that moves interactions, lenses,
// lens kinds, constraints and risks; and the one that moves issues, their documents, a block's
// text and rank, results and components.
const (
	movedHere       = "this-change"
	movedWithAsks   = "asks-child"
	movedWithInbox  = "inbox-child"
	movedWithLenses = "lenses-child"
	movedWithBlocks = "blocks-child"
)

// StoreKind is one term of the projection: one kind of write the five children of
// the parent change move, the child that moves it, how many nodes and how many tuples one such
// write appends, and how many such writes the database's own history holds inside the window.
// Rows a day is the sum over the terms of writes a day times tuples a write (R4).
//
// Every term a table can count reads tables that exist before this branch's first migration, so
// the sizing reads the same history on a database the store does not exist in yet, which is the
// database R4 takes its readings from. Where a writer's tuples vary from one write to the next,
// each fact is a term of its own, counted over the writes that state it: a comment states its
// node and three base attributes always - its visibility, its state, and its project or the
// parties it is between - and an authored_by, an anchored_to, an owned_by and its
// author's aliases only where its row names each. A person, a session, a role and a role's page
// are each written once, by the write that first records them, so each is a term counted over
// the ones the window records for the first time.
//
// A kind production writes no row of before its child ships - a decision and a reading of
// the asks child, a criterion of the blocks child and every kind of the lenses child - has no
// history to read, so its term carries an assumed rate (Assumed),
// which its Driver states with the reason for it and the report marks as assumed, rather than
// reading zero and leaving the kind out of the sum. A decision, a reading and a criterion are
// blocks of a document, whose node and placement the block terms count already, so each of their
// terms counts only the tuples it states beyond a block's.
type StoreKind struct {
	Name, MovedBy, Driver string
	Nodes, Tuples         int
	// Writes is how many writes of the kind the window holds.
	Writes int64
	// RowsADay is what the database was written at over the window, TuplesADay and
	// NodeRowsADay what that puts in graph_tuples and in graph_nodes a day, and Share the
	// kind's part of every term's tuples a day.
	RowsADay, TuplesADay, NodeRowsADay, Share float64
	// Replayed and Unreadable are the version rows the moved-parent term parsed and how many
	// of them did not parse (replayParents), zero for every other term.
	Replayed, Unreadable int64
	// Assumed is the writes the window is taken to hold for a kind with no history to read,
	// and zero for every term a statement or the replay counts.
	Assumed int64
	// count is the scalar SQL that counts the kind's writes inside the window, over the rows
	// sizingWritesWith names, and empty for the three terms replayParents counts and for an assumed
	// term.
	count string
}

// movedParents is the name of the term no statement can count: a block's parent is the nesting
// of its rendered version, which no table written before the store holds (replayParents).
// movedRanks is the rank each of those blocks takes in its new place, and movedTextPlaces its new
// place in the text, both counted by the same replay.
const (
	movedParents    = "block_parent_moved"
	movedRanks      = "block_rank_moved"
	movedTextPlaces = "block_text_place_moved"
)

// authoredPointers is the name of the term of what an authored block - a criterion, a
// requirement, a check or a plan node - writes beyond the facts of every block: the tuple of each
// serves, needs and parent pointer its opening line writes (refs' authoredSourcesOf and
// pointerPairs), which block_refs projects as a row of the pointer's kind, and a plan node's size
// in whole minutes (refs' planBlockValues). authoredPointerKinds is the kinds of those rows, as
// SQL: serves (refs' servesKind), needs and parent (pmdoc.ParentAttribute). The term block_ref
// leaves those rows to it, so no tuple is counted by both.
const (
	authoredPointers     = "authored_pointer"
	authoredPointerKinds = `('serves', '` + graphstore.TupleNeeds + `', '` + pmdoc.ParentAttribute + `')`
)

// storeKinds are the terms R4 states, each beside the writer or the requirement whose tuples it
// counts:
//   - a comment's node and its visibility, state and project or parties (graphstore.Write), the
//     authored_by and the aliases of whoever wrote it (graphstore.AuthorFacts), and the artifacts
//     it is anchored to and owned by (api's commentStored);
//   - a delivery to a comment's mention, one delivered_to (outbox.deliveredGraph), and the same
//     one tuple for each session or role the bus reached with a comment event
//     (outbox.deliveredGraph) and for each fallback copy the outbox published
//     (outbox.fallbackDelivered);
//   - a version's blocks as refs.IndexBlocks states them: a block it inserts afresh, its node, its
//     three base attributes (its project being its document's), its in_document and its part_of;
//     a block it restores, its state and its in_document, with at most the retraction and the
//     assertion of a parent the restore moved; a block it drops, its state gone; a block whose
//     parent moved, the retraction of the old parent and the assertion of the new; and a block
//     reference, the pair's assertion and, at most, the retraction that ends it, which no table
//     records once the pair is gone;
//   - the node and three base attributes of an issue, an artifact, an ask and a message, and the
//     gone state a document's removal states of its node, each written by this build's own create
//     or removal;
//   - the values of each block a version wrote: the text value of each block whose text it wrote,
//     the rank, the placement, the text_parent and the text_rank of a block's first row, and the
//     new rank and the new place in the text of a block it moved under another parent (refs'
//     planBlockValues); each version's node with its base
//     attributes, its part_of and its two values (graphstore.VersionFacts), the mime, size and
//     sha256 values an image's or a file's version states on its document
//     (graphstore.ArtifactBytes), and the part_of an issue's new document states
//     (graphstore.ArtifactPartOf);
//   - the inbox child's delivered_to of a message, and one tuple for each read mark, clear,
//     dismissal and snooze. Each of the tables those four are written to keeps the newest mark of
//     a key and no earlier one, so its term counts every key marked inside the window once;
//   - the person, session, role and page nodes the first write that records each makes
//     (graphstore.AuthorFacts, graphstore.QueueReached, outbox.deliveredGraph), each with its two
//     base attributes: a page's part_of its role, and a session's part_of the role it claimed,
//     with that role's held_by and the retraction that ends it, or else a page of its own with
//     that page's base attributes and its part_of the session (R14);
//   - the inbox child's message's author and parent, and the tuples its target and its
//     broadcast become, a reply's meaning tuple and its part_of, a comment's mentions, a
//     person's part in a thread, the delivered_to of each write to every other participant and of
//     an ask to the person it waits on, the move of that person, the body blocks of a comment, a
//     message and an ask with their links, and a delivery attempt's node (its spec's
//     #b-r-kinds-move, #b-r-delivery-written, #b-r-participation and #b-r-mentions);
//   - the blocks child's document version node, the node of a removed document's version stated
//     gone, an artifact's part_of its issue, an issue's child_of,
//     needs, filed_from and duplicate_of, an issue's room - its tree artifact, its decisions query
//     and their first versions - and the values of each query node, a document's references, a
//     comment's anchored_in its anchor's block, the authored_by of a block of an issue's room, a
//     result's node with its values and tuples, its screenshots and its retraction, a component's
//     node with its part_of, depends_on and affects, a retired component's node stated gone with
//     those relations retracted, a criterion's authored_by, and an authored block's serves, needs
//     and parent pointers and a plan node's size
//     (#b-r-issue-relations, #b-r-result-node, #b-r-artifacts-in-tree, #b-r-reasoning-tree,
//     #b-r-one-room, #b-r-queries);
//   - the asks child's ask's answer_state, question, options, authored_by, anchored_to and
//     owned_by, and what its other events state - an edit, an answer, a resolution, a follower - a
//     Keep, and an agent's decision and its reading (#b-r-ask-node, #b-r-decision-node,
//     #b-r-reading);
//   - the lenses child's interaction, its frames and its node stated gone once the release before
//     deletes its row, a lens and its finding blocks, a lens kind, a constraint, a risk, a node's
//     state and a pointer's retarget (#b-r-interaction, #b-r-lens, #b-r-lens-catalogue,
//     #b-r-constraint-risk), every one of them assumed;
//   - and R7's delete of a node, which takes a lens and its findings gone, each node's state gone
//     and every relation out of it retracted, assumed too.
//
// Every node of an issue, a document, an ask, a message, a comment and a block states its project
// or its parties beside its visibility and state, written by the create that makes it: each of
// those kinds' terms counts that tuple, and none has a second term for its node, which its own
// term counts already.
func storeKinds() []StoreKind {
	return []StoreKind{{
		Name: "comment", MovedBy: movedHere, Nodes: 1, Tuples: 3,
		Driver: "comments.created_at: the comment's node and its visibility, state and project or parties",
		count:  `(select count(*) from sized_comments)`,
	}, {
		Name: graphstore.TupleAuthoredBy, MovedBy: movedHere, Tuples: 1,
		Driver: "comments.created_at, each naming an author the store can key",
		count:  `(select count(*) from sized_comments where authored)`,
	}, {
		Name: graphstore.TupleAnchoredTo, MovedBy: movedHere, Tuples: 1,
		Driver: "comments.created_at, each anchored to a document the store can key",
		count:  `(select count(*) from sized_comments where anchored)`,
	}, {
		Name: graphstore.TupleOwnedBy, MovedBy: movedHere, Tuples: 1,
		Driver: "comments.created_at, each owned by a document the store can key",
		count:  `(select count(*) from sized_comments where owned)`,
	}, {
		Name: "author_alias", MovedBy: movedHere, Tuples: 1,
		Driver: "comments.created_at: each alias_of and recorded_as the credential that wrote one proved",
		count:  `(select coalesce(sum(aliases), 0) from sized_comments)`,
	}, {
		Name: "anchor_block", MovedBy: movedWithBlocks, Tuples: 6,
		Driver: "comments.created_at, each anchored to a document the store can key: its anchored_in to the block holding" +
			" its quote (comments.anchor's block_id) and the part_of to that block of a comment that answers nothing (R31)," +
			" at most the retraction of both once the quote's mark is gone, an accept's or a refresh's, and at most their" +
			" restatement once a refresh finds the mark again, which over-states an anchor that names no quote. A comment on" +
			" the text of a node is new in this release, so the window holds none for it to count",
		count: `(select count(*) from sized_comments where anchored)`,
	}, {
		Name: "delivery", MovedBy: movedHere, Tuples: 1,
		Driver: "comment_deliveries.created_at, taken by the listener: a delivered_to for each attempt that reached its destination",
		count: `(select count(*) from comment_deliveries where state in ` + model.DeliveryTakenStatesSQL +
			` and created_at > ` + sizingWindow + `)`,
	}, {
		Name: "bus_delivery", MovedBy: movedHere, Tuples: 1,
		Driver: "events.published_destinations of the comment events written in the window, each a session's or a role's" +
			" topic as the outbox reads one: a delivered_to for each destination the bus reached",
		count: `(select count(*) from events e cross join lateral unnest(e.published_destinations) reached (topic)
			where e.type like 'comment.%' and e.created_at > ` + sizingWindow + `
			  and jsonb_typeof(e.payload->'id') = 'string' and e.payload->>'id' <> ''
			  and octet_length(e.payload->>'id') <= ` + addressBound + `
			  and ` + reachesANode("reached.topic") + `)`,
	}, {
		Name: "fallback_delivery", MovedBy: movedHere, Tuples: 1,
		Driver: "comment_fallback_copies.settled_at of the copies the outbox published in the window, each to a session's" +
			" or a role's topic: a delivered_to for each",
		count: `(select count(*) from comment_fallback_copies f
			where f.published and f.settled_at > ` + sizingWindow + ` and ` + reachesANode("f.destination") + `)`,
	}, {
		Name: "block", MovedBy: movedHere, Nodes: 1, Tuples: 5,
		Driver: "document_block_texts of the versions written in the window, a block's first row: its node, its visibility," +
			" state and project, its in_document and its part_of",
		count: `(select count(*) from sized_texts where written and earlier is null)`,
	}, {
		Name: "block_restored", MovedBy: movedHere, Tuples: 4,
		Driver: "document_block_texts of the versions written in the window, a text after a row that deleted the block:" +
			" its state and its in_document, and at most the retraction and the assertion of a parent the restore moved",
		count: `(select count(*) from sized_texts where written and not earlier)`,
	}, {
		Name: "block_gone", MovedBy: movedHere, Tuples: 1,
		Driver: "document_block_texts of the versions written in the window, a row that deleted the block: its state gone",
		count:  `(select count(*) from sized_texts where not written)`,
	}, {
		Name: movedParents, MovedBy: movedHere, Tuples: 2,
		Driver: "each version the block index indexed in the window against the one it indexed before, parsed and placed" +
			" as refs.PlaceBlocks places them, with blocks matched by their text, those of one document with equal text" +
			" in document order, so the term is exact except there: the old parent's retraction and the new one's assertion",
	}, {
		Name: "block_ref", MovedBy: movedHere, Tuples: 2,
		Driver: "block_refs.created_at of each link but an authored block's serves, needs and parent pointers, which " +
			authoredPointers + " counts: the pair's assertion and, at most, the retraction that ends it",
		count: `(select count(*) from block_refs where created_at > ` + sizingWindow + ` and kind not in ` + authoredPointerKinds + `)`,
	}, {
		Name: "document_ref", MovedBy: movedWithBlocks, Tuples: 2,
		Driver: "refs.created_at of a document's references: the mentions out of the document's node and, at most, the" +
			" retraction that ends it",
		count: `(select count(*) from refs where from_kind = '` + graphstore.KindArtifact + `' and created_at > ` + sizingWindow + `)`,
	}, {
		Name: "block_text", MovedBy: movedWithBlocks, Tuples: 1,
		Driver: "document_block_texts of the versions written in the window, each row with a text: its text value",
		count:  `(select count(*) from sized_texts where written)`,
	}, {
		Name: "block_values", MovedBy: movedWithBlocks, Tuples: 4,
		Driver: "document_block_texts of the versions written in the window, a block's first row: its rank, its" +
			" placement, its text_parent and its text_rank",
		count: `(select count(*) from sized_texts where written and earlier is null)`,
	}, {
		Name: movedRanks, MovedBy: movedWithBlocks, Tuples: 1,
		Driver: "each block the moved-parent term counts: its rank among its new siblings, where no sibling it left or" +
			" joined takes a new one",
	}, {
		Name: movedTextPlaces, MovedBy: movedWithBlocks, Tuples: 3,
		Driver: "each block the moved-parent term counts, which refs.PlaceBlocks places by its text: its new" +
			" text_parent, the retraction of its old one, and its text_rank among its new text siblings",
	}, {
		Name: "block_author", MovedBy: movedWithBlocks, Tuples: 1,
		Driver: "document_block_texts of the versions written in the window, a block's first row in an issue's spec," +
			" Verification document or plan, the documents the issue's room holds once the issue converts (R6), each" +
			" block of which records its authors: one authored_by for each author the version that first holds it lists",
		count: `(select coalesce(sum(authors), 0) from sized_texts where written and earlier is null and role)`,
	}, {
		Name: graphstore.KindVersion, MovedBy: movedWithBlocks, Nodes: 1, Tuples: 6,
		Driver: "artifact_versions.created_at: a version's node, its visibility, its state and its document's project or" +
			" parties, its part_of its document, and its number and created_at values",
		count: `(select count(*) from artifact_versions where created_at > ` + sizingWindow + `)`,
	}, {
		Name: "query_version", MovedBy: movedWithBlocks, Tuples: 1,
		Driver: "artifact_versions.created_at of a version of an issue's spec, Verification document or plan: the" +
			" definition_version a version written while its document is a query node holds, so a version of a" +
			" document of an issue that has not converted is over-stated",
		count: `(select count(*) from artifact_versions v join artifacts a on a.id = v.artifact_id
			where v.created_at > ` + sizingWindow + ` and ` + roleDocument("a") + `)`,
	}, {
		Name: "version_bytes", MovedBy: movedWithBlocks, Tuples: 3,
		Driver: "artifact_versions.created_at of an image's or a file's version: its document's mime, size and sha256 values",
		count:  `(select count(*) from artifact_versions where created_at > ` + sizingWindow + ` and mime is not null)`,
	}, {
		Name: "document_part_of", MovedBy: movedWithBlocks, Tuples: 1,
		Driver: "artifacts.created_at of an issue's document: its part_of its issue",
		count:  `(select count(*) from artifacts where created_at > ` + sizingWindow + ` and issue_key is not null)`,
	}, {
		Name: "version_removed", MovedBy: movedWithBlocks, Tuples: 1,
		Driver: "artifact.removed events in the window: the node of the one version an untouched document holds, stated" +
			" gone with the document (api's removedDocumentNodes)",
		count: `(select count(*) from events where type = 'artifact.removed' and created_at > ` + sizingWindow + `)`,
	}, {
		Name: "message_delivery", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "message_deliveries.created_at, taken by the listener: a delivered_to for each attempt that reached its session",
		count: `(select count(*) from message_deliveries where state in ` + model.DeliveryTakenStatesSQL +
			` and created_at > ` + sizingWindow + `)`,
	}, {
		Name: "read_mark", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "user_threads.read_through and user_agent_read.read_through in the window, and user_agent_reply_read" +
			" of a reply written in the window: a read_through or reply_read",
		count: `((select count(*) from user_threads where read_through > ` + sizingWindow + `)
			+ (select count(*) from user_agent_read where read_through > ` + sizingWindow + `)
			+ (select count(*) from user_agent_reply_read r join messages m on m.id = r.reply_id
			   where m.created_at > ` + sizingWindow + `))`,
	}, {
		Name: "clear", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "user_agent_state.cleared_before in the window: a cleared",
		count:  `(select count(*) from user_agent_state where cleared_before > ` + sizingWindow + `)`,
	}, {
		Name: "dismissal", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "user_agent_thread_dismissed.dismissed_at and user_threads.dismissed_at in the window: a dismissed",
		count: `((select count(*) from user_agent_thread_dismissed where dismissed_at > ` + sizingWindow + `)
			+ (select count(*) from user_threads where dismissed_at > ` + sizingWindow + `))`,
	}, {
		Name: "snooze", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "user_ask_snooze.snoozed_until after the window opened, which every snooze written in it is: a snoozed",
		count:  `(select count(*) from user_ask_snooze where snoozed_until > ` + sizingWindow + `)`,
	}, {
		Name: graphstore.KindPerson, MovedBy: movedHere, Nodes: 1, Tuples: 2,
		Driver: "comments.created_at of the first comment that names each person, as its author or as the owner of the" +
			" session it proved: the person's node and its visibility and state",
		count: `(select count(*) from sized_people where first > ` + sizingWindow + `)`,
	}, {
		Name: graphstore.KindSession, MovedBy: movedHere, Nodes: 2, Tuples: 5,
		Driver: "the first write recordedSessions dates for each session, in the window: its node and its visibility and" +
			" state, and either its part_of the role it claimed with that role's held_by and the retraction that ends it," +
			" or a page of its own with that page's visibility, state and part_of - three tuples either way, and two" +
			" nodes at most, since which one a session took is the role bucket's and no table's",
		count: `(select count(*) from (` + recordedSessions(sizingAuthors) + `) recorded
			where recorded.at > ` + sizingWindow + ` and octet_length(recorded.id) <= ` + addressBound + `)`,
	}, {
		Name: graphstore.KindRole, MovedBy: movedHere, Nodes: 1, Tuples: 2,
		Driver: "the first event addressedRoles reads for each role, in the window: the role's node and its visibility and state",
		count:  `(select count(*) from sized_roles where octet_length(name) <= ` + addressBound + `)`,
	}, {
		Name: graphstore.KindPage, MovedBy: movedHere, Nodes: 1, Tuples: 3,
		Driver: "the first event addressedRoles reads for each role, in the window: the role's page, the page's" +
			" visibility and state and its part_of the role",
		count: `(select count(*) from sized_roles
			where octet_length('` + graphstore.KindRole + `/' || name) <= ` + addressBound + `)`,
	}, {
		Name: "message", MovedBy: movedHere, Nodes: 1, Tuples: 3,
		Driver: "messages.created_at: the message's node and its visibility, state and project or parties",
		count:  `(select count(*) from sized_messages)`,
	}, {
		Name: "message_relation", MovedBy: movedWithInbox, Tuples: 2,
		Driver: "messages.created_at: the message's authored_by and its part_of the issue it is posted on or the page" +
			" of the agent it is addressed to",
		count: `(select count(*) from sized_messages)`,
	}, {
		Name: "message_pointer", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "messages.created_at, each target and broadcast_id a message names: the tuple each becomes",
		count:  `(select count(target) + count(broadcast_id) from sized_messages)`,
	}, {
		Name: "reply", MovedBy: movedWithInbox, Tuples: 2,
		Driver: "comments.created_at of a comment naming a reply_to, an ask_id, an answers or a replies_to_block, and" +
			" messages.created_at of a message naming an in_reply_to or a replies_to_block: its answers or replies_to" +
			" tuple and its part_of what it answers",
		count: `((select count(*) from sized_comments where replies) + (select count(*) from sized_messages where replies))`,
	}, {
		Name: "mention", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "comment_mentions of the comments written in the window: a mentions tuple for each target, and a" +
			" second for each role target that resolved to a session",
		count: `(select count(*) + count(m.resolved_session_id)
			from comment_mentions m join sized_comments c on c.id = m.comment_id)`,
	}, {
		Name: "participation", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "user_threads rows whose thread's root ask, comment or message was written in the window: a" +
			" participates_in from the person to that root",
		count: `(select count(*) from user_threads u
			left join asks a on u.kind = 'ask' and a.id = u.thread_id
			left join comments c on u.kind = 'comment' and c.id = u.thread_id
			left join messages m on u.kind = 'message' and m.id = u.thread_id
			where coalesce(a.created_at, c.created_at, m.created_at) > ` + sizingWindow + `)`,
	}, {
		Name: "person_delivery", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "each comment, issue message and answer written in the window, to every other participant user_threads" +
			" holds of its thread, and each ask opened in the window, to the person it waits on: a delivered_to",
		count: `((select count(*) from sized_threads t
			   join user_threads u on u.thread_id = t.thread and u.login is distinct from t.writer)
			+ (select count(*) from sized_ask_events where type = 'ask.opened'))`,
	}, {
		Name: "ask_asked_of", MovedBy: movedWithInbox, Tuples: 4,
		Driver: "ask.asked_of events in the window: a delivered_to and a participates_in to the person the ask now" +
			" waits on, and the retraction of each for the person it waited on",
		count: `(select count(*) from sized_ask_events where type = 'ask.asked_of')`,
	}, {
		Name: "body_block", MovedBy: movedWithInbox, Nodes: 1, Tuples: 3,
		Driver: "comment_blocks, message_blocks and ask_blocks created in the window: the body block's node, its" +
			" visibility and state, and its part_of the comment, message or ask",
		count: `((select count(*) from comment_blocks where created_at > ` + sizingWindow + `)
			+ (select count(*) from message_blocks where created_at > ` + sizingWindow + `)
			+ (select count(*) from ask_blocks where created_at > ` + sizingWindow + `))`,
	}, {
		Name: "body_block_gone", MovedBy: movedWithInbox, Tuples: 1,
		Driver: "comment_blocks, message_blocks and ask_blocks removed in the window: the body block's state gone",
		count: `((select count(*) from comment_blocks where removed_at > ` + sizingWindow + `)
			+ (select count(*) from message_blocks where removed_at > ` + sizingWindow + `)
			+ (select count(*) from ask_blocks where removed_at > ` + sizingWindow + `))`,
	}, {
		Name: "body_block_ref", MovedBy: movedWithInbox, Tuples: 2,
		Driver: "comment_block_refs, message_block_refs and ask_block_refs created in the window: the link's assertion" +
			" and, at most, the retraction that ends it",
		count: `((select count(*) from comment_block_refs where created_at > ` + sizingWindow + `)
			+ (select count(*) from message_block_refs where created_at > ` + sizingWindow + `)
			+ (select count(*) from ask_block_refs where created_at > ` + sizingWindow + `))`,
	}, {
		Name: "delivery_attempt", MovedBy: movedWithInbox, Nodes: 1, Tuples: 14,
		Driver: "comment_deliveries.created_at and message_deliveries.created_at, every attempt whatever its state: its" +
			" node, its visibility and state, at most its attempt, state, envelope_id, error, resolve_error, duplicate," +
			" claimed_at, accepted_at and accepted_as values once each, its owed_to and the retraction that ends it, and" +
			" its answered_by",
		count: `((select count(*) from comment_deliveries where created_at > ` + sizingWindow + `)
			+ (select count(*) from message_deliveries where created_at > ` + sizingWindow + `))`,
	}, {
		Name: "issue", MovedBy: movedHere, Nodes: 1, Tuples: 3,
		Driver: "issues.created_at: the issue's node and its visibility, state and project",
		count:  `(select count(*) from issues where created_at > ` + sizingWindow + `)`,
	}, {
		Name: "issue_relation", MovedBy: movedWithBlocks, Tuples: 1,
		Driver: "issues.parent_key of the issues created in the window, and issue_links.created_at: a child_of or a needs",
		count: `((select count(*) from issues where created_at > ` + sizingWindow + ` and parent_key is not null)
			+ (select count(*) from issue_links where created_at > ` + sizingWindow + `))`,
	}, {
		Name: "issue_origin", MovedBy: movedWithBlocks, Tuples: 2,
		Driver: "issues.created_at: at most its filed_from and its duplicate_of (R9), which no table before this branch" +
			" holds, so every issue is taken to state both, which over-states an issue that states neither",
		count: `(select count(*) from issues where created_at > ` + sizingWindow + `)`,
	}, {
		Name: "issue_room", MovedBy: movedWithBlocks, Nodes: 4, Tuples: 21,
		Driver: "issues.created_at, each issue born converted: its tree artifact's node and its decisions query's, each" +
			" with its visibility, state and project and its part_of the issue, and the two documents' empty first" +
			" versions, each a node with its visibility, state, project, part_of, number and created_at, the decisions" +
			" query's with its definition_version",
		count: `(select count(*) from issues where created_at > ` + sizingWindow + `)`,
	}, {
		Name: "query_node", MovedBy: movedWithBlocks, Tuples: 4,
		Driver: "artifacts.created_at of an issue's spec, Verification document or plan, and issues.created_at, each" +
			" issue's decisions query: the query node's query, takes, subtree and definition_version values",
		count: `((select count(*) from artifacts where created_at > ` + sizingWindow + ` and ` + roleDocument("artifacts") + `)
			+ (select count(*) from issues where created_at > ` + sizingWindow + `))`,
	}, {
		Name: "artifact", MovedBy: movedHere, Nodes: 1, Tuples: 3,
		Driver: "artifacts.created_at: the artifact's node and its visibility, state and project or parties",
		count:  `(select count(*) from artifacts where created_at > ` + sizingWindow + `)`,
	}, {
		Name: "artifact_removed", MovedBy: movedHere, Tuples: 1,
		Driver: "artifact.removed and " + model.DocumentRetiredEventType + " events in the window: the removed or retired" +
			" document's node stated gone (api's removeArtifact, docs' RetireDocument)",
		count: `(select count(*) from events where type in ('artifact.removed', '` + model.DocumentRetiredEventType + `')
			and created_at > ` + sizingWindow + `)`,
	}, {
		// Counted off refs' recordsAuthors and blockAuthors: a criterion records its authors as
		// authored_by tuples anywhere it is written; a criterion in an issue's spec, Verification
		// document or plan is already counted by block_author, so this counts one outside those
		// role documents (document_block_texts rows whose opening line opens a criterion block).
		Name: "criterion", MovedBy: movedWithBlocks, Tuples: 1,
		Driver: "document_block_texts of the versions written in the window, a criterion block's first row outside an" +
			" issue's spec, Verification document or plan (which block_author counts): one authored_by for each author" +
			" the version that first holds it lists, its node, values and placement being a block's, which the block" +
			" terms count",
		count: `(select coalesce(sum(authors), 0) from sized_texts where written and earlier is null and criterion and not role)`,
	}, {
		// Counted off the block index's links (refs' pointerPairs, queueBlockRefs), whose rows of the
		// kinds authoredPointerKinds names are an authored block's pointers, and off the values a
		// pass states of a plan node (refs' planBlockValues), whose size the opening line of its
		// text writes (refs.WrittenSizeSQL reads it there too).
		Name: authoredPointers, MovedBy: movedWithBlocks, Tuples: 2,
		Driver: "block_refs.created_at of each serves, needs and parent pointer an authored block writes, and" +
			" document_block_texts of the versions written in the window, each row of a plan_task block whose opening line" +
			" writes a size: the pointer's tuple or the size value in whole minutes, and at most the retraction that ends it",
		count: `((select count(*) from block_refs where created_at > ` + sizingWindow + ` and kind in ` + authoredPointerKinds + `)
			+ (select count(*) from sized_texts where sized))`,
	}, {
		// Counted off graphstore.ResultFacts, which the record route (api's writeRecordedResult) and
		// the backfill (backfillResults) write a result through, for one a session recorded naming
		// no screenshot, which result_screenshot counts: graphstore.Write's visibility, state and
		// project of the node it makes; the twelve values; the part_of and the result_of its check;
		// graphstore.AuthorFacts' authored_by with the one alias_of a session's credential can prove
		// of its owner; and the run_session.
		Name: "result", MovedBy: movedWithBlocks, Nodes: 1, Tuples: 20,
		Driver: "check_results.created_at: the result's node, its visibility, state and project, its verdict, surface," +
			" run, observed, commit, role, failed, skipped, excluded, negative_control, check_version and check_sha256" +
			" values, its part_of and its result_of its check, its authored_by its recorder and at most the alias_of" +
			" of a recording session to the person its credential proved, and its run_session",
		count: `(select count(*) from check_results where created_at > ` + sizingWindow + `)`,
	}, {
		Name: "result_screenshot", MovedBy: movedWithBlocks, Tuples: 1,
		Driver: "check_results.screenshots of the results written in the window: a tuple to each screenshot",
		count: `(select coalesce(sum(cardinality(screenshots)), 0) from check_results
			where created_at > ` + sizingWindow + `)`,
	}, {
		Name: "result_retraction", MovedBy: movedWithBlocks, Tuples: 1,
		Driver: "check_result_retractions.created_at: the result's retracted value",
		count:  `(select count(*) from check_result_retractions where created_at > ` + sizingWindow + `)`,
	}, {
		// Counted off graphstore.ComponentNode, the node an import (architecture's stateComponents)
		// and the backfill (writeComponentBatch) write a component as: in its project.
		Name: "component", MovedBy: movedWithBlocks, Nodes: 1, Tuples: 3,
		Driver: "components of an architecture snapshot imported in the window, each the node the import writes or" +
			" rewrites, which over-states a component an import left as it was: its node and its visibility, state" +
			" and project",
		count: `(select count(*) from sized_components)`,
	}, {
		Name: "component_relation", MovedBy: movedWithBlocks, Tuples: 1,
		Driver: "the parent and the component_depends of those components, and the issue_component_members of an" +
			" issue_components row updated in the window: a part_of, a depends_on or an affects",
		count: `((select count(parent) from sized_components)
			+ (select count(*) from component_depends d
			   join sized_components c on c.project_key = d.project_key and c.id = d.from_id)
			+ (select count(*) from issue_component_members m join issue_components i on i.issue_key = m.issue_key
			   where i.updated_at > ` + sizingWindow + `))`,
	}, {
		// Counted off architecture's retiredComponents, which an import that drops a component
		// (stateComponents) and a source's removal (RetireComponents) write a component through: its
		// node stated gone and the retraction of each part_of and depends_on it holds.
		Name: "component_retired", MovedBy: movedWithBlocks, Tuples: 3, Assumed: sizingWindowDays / 7,
		Driver: "assumed at one a week, since no table keeps a component a re-import dropped or a source's removal" +
			" took: its node stated gone and the retraction of its part_of and of its depends_on, taken as one each",
	}, {
		Name: "ask", MovedBy: movedHere, Nodes: 1, Tuples: 3,
		Driver: "ask.opened events in the window: the ask's node and its visibility, state and project or parties",
		count:  `(select count(*) from sized_ask_events where type = 'ask.opened')`,
	}, {
		Name: "ask_opened", MovedBy: movedWithAsks, Tuples: 8,
		Driver: "ask.opened events in the window: the ask's answer_state, question and options values, its authored_by" +
			" and, at most, its anchored_to and owned_by, and the part_of the document holding its block, which an ask" +
			" block's anchored_to names too, and asked_on to the block itself",
		count: `(select count(*) from sized_ask_events where type = 'ask.opened')`,
	}, {
		Name: "ask_edit", MovedBy: movedWithAsks, Tuples: 2,
		Driver: "ask.edited events in the window: at most the ask's question and options values",
		count:  `(select count(*) from sized_ask_events where type = 'ask.edited')`,
	}, {
		Name: "answer", MovedBy: movedWithAsks, Tuples: 6,
		Driver: "ask.answered events in the window: the answer text, the options it selects, its author, as answered_by" +
			" and as the decided_by that makes the answered ask a person's decision, and its time, and the ask's" +
			" answer_state",
		count: `(select count(*) from sized_ask_events where type = 'ask.answered')`,
	}, {
		Name: "ask_resolved", MovedBy: movedWithAsks, Tuples: 3,
		Driver: "ask.resolved events in the window: the ask's answer_state and its resolution, and at most the" +
			" retraction that ends that resolution, which the reopen that restores the ask appends",
		count: `(select count(*) from sized_ask_events where type = 'ask.resolved')`,
	}, {
		Name: "ask_follower", MovedBy: movedWithAsks, Tuples: 1,
		Driver: "ask.follower_added and ask.follower_removed events in the window: a followed_by or the retraction that" +
			" ends one",
		count: `(select count(*) from sized_ask_events where type in ('ask.follower_added', 'ask.follower_removed'))`,
	}, {
		Name: "keep", MovedBy: movedWithAsks, Tuples: 2,
		Driver: "decision_keeps.kept_at in the window: a kept_by and a kept_text_sum on the decision",
		count:  `(select count(*) from decision_keeps where kept_at > ` + sizingWindow + `)`,
	}, {
		Name: "ask_copy", MovedBy: movedWithAsks, Tuples: 1,
		Driver: "document_blocks.changed_at in the window of an ask block that names an ask of another document of its" +
			" owner and that no ask row of its own document names, a copy of that ask: the copy's copied_from, which" +
			" a settlement claims once and no table projects",
		count: `(select count(*) from document_blocks b join artifacts d on d.id = b.artifact_id
			where b.type = 'ask' and b.changed_at > ` + sizingWindow + `
			  and not exists (select 1 from asks own where own.block_artifact_id = b.artifact_id and own.block_id = b.block_id)
			  and exists (select 1 from asks source join artifacts s on s.id = source.block_artifact_id
			              where source.block_id = b.block_id and source.block_artifact_id <> b.artifact_id
			                and s.issue_key is not distinct from d.issue_key and s.project_key = d.project_key))`,
	}, {
		Name: "ask_invalid", MovedBy: movedWithAsks, Tuples: 2,
		Driver: "block.invalid events in the window, each a settlement's flag of a block that fails the strict read:" +
			" at most the reason as its ask node's invalid value, where the block has an ask row, and the retraction" +
			" that ends it once the block reads again",
		count: `(select count(*) from events where type = 'block.invalid' and created_at > ` + sizingWindow + `)`,
	}, {
		Name: "ask_removal", MovedBy: movedWithAsks, Tuples: 2,
		Driver: "document_block_texts of the versions written in the window, a row that deleted the block of an" +
			" answered ask, which only a person's own write takes out: the ask node's removed_by to that person, and" +
			" at most the retraction that ends it once an ask block of that ask stands in the document again, which" +
			" no table projects",
		count: `(select count(*) from artifact_versions v
			join document_block_texts t on t.artifact_id = v.artifact_id and t.version = v.number
			where v.created_at > ` + sizingWindow + ` and t.text is null
			  and exists (select 1 from asks k where k.block_artifact_id = t.artifact_id and k.block_id = t.block_id
			              and k.state = 'answered'))`,
	}, {
		Name: "block_move", MovedBy: movedWithAsks, Tuples: 1, Assumed: 1 * sizingWindowDays,
		Driver: "assumed at one a day, since the one writer of a moved_to out of a block is the decisions step's" +
			" conversion, which moves each section and ask block of a decisions document an earlier release wrote" +
			" into its issue's spec once, a backlog no table dates: the moved address's moved_to, its node and" +
			" placement being a block's, which the block terms count",
	}, {
		Name: "decision", MovedBy: movedWithAsks, Tuples: 5, Assumed: 10 * sizingWindowDays,
		Driver: "assumed at 10 a day, since production writes no agent's decision before the asks child ships and a" +
			" section of an issue's decisions document, the form an earlier release wrote one in, is no row: its" +
			" uncertainty and uncertainty_reason values, one shaped, its from_ask and its decided_by, its node and" +
			" placement being a block's, which the block terms count",
	}, {
		Name: "reading", MovedBy: movedWithAsks, Tuples: 1, Assumed: 10 * sizingWindowDays,
		Driver: "assumed at one under each assumed decision, 10 a day, since production writes no reading before" +
			" the asks child ships: its authored_by, its node and its part_of its decision being a block's, which the" +
			" block terms count",
	}, {
		// Counted off graphstore.InteractionFacts, which the going POST (api's recordGoingRequest), the
		// frame path and the backfill (graphstore.WriteRun) write a run through, the backfill each run
		// recorded before this release: the words, ends_at and source values, and
		// graphstore.AuthorFacts' authored_by with its author's aliases (an alias_of and a recorded_as
		// at most); graphstore.Write's base attributes of the node it makes, the visibility, the state
		// and the project where the run names one; the changed_by_run a write made for the run appends
		// out of a node it changed (store.WithRun); and the state the sweep states the run's node gone
		// at, an hour after it was asked (api's goRunGone, through graphstore.WriteState).
		Name: "interaction", MovedBy: movedWithLenses, Nodes: 1, Tuples: 11, Assumed: 10 * sizingWindowDays,
		Driver: "assumed at 10 runs a day, since production writes no interaction before this release and" +
			" going_requests holds a run for an hour alone (contracts.GoingRequestTTL): the release before" +
			" deletes it then, and this release's sweep states its node gone and removes its row, so no table" +
			" of the database the sizing reads holds the window's runs: its node, its visibility and state, at" +
			" most its project, its words, ends_at and source values, its authored_by, at most two aliases of" +
			" its author, one changed_by_run into it and its state gone an hour on",
	}, {
		// Counted off graphstore.FrameTuple, the one value tuple the frame path (api's
		// appendGoingEvent) and the backfill (graphstore.ReadRunFrames) write of each frame.
		Name: "frame", MovedBy: movedWithLenses, Tuples: 1, Assumed: 500 * sizingWindowDays,
		Driver: "assumed at 50 for each assumed run, 500 a day, since a run's going_events rows go with its" +
			" going_requests row an hour after it was asked: one frame value each",
	}, {
		// Counted off writeGoneRun, the backfill's writer that states gone a run's node whose row the
		// release before deleted (backfillGoneRuns): the state value it moves. The sweep's state of a
		// run's node gone (api's goRunGone) is counted under interaction.
		Name: "interaction_gone", MovedBy: movedWithLenses, Tuples: 1, Assumed: 10 * sizingWindowDays,
		Driver: "assumed at one for each assumed run, 10 a day, since the release before deletes every run an hour" +
			" old (contracts.GoingRequestTTL) and no table holds the window's runs: the run's node stated gone once" +
			" that release has deleted its row, which over-states a run once no process of that release serves",
	}, {
		// Counted off api's invokeLens and writeReasoningNodes: graphstore.Write's visibility, state
		// and project, the issue's; the part_of, the has_kind and, where the body names them, the
		// verdict value and the triggered_by; the artifact_id value of a lens placed on a document or
		// on a block of one (graphstore.AttributeArtifactID); and, where graphstore.AuthorOf names a
		// writer of its made_by, graphstore.AuthorFacts' authored_by and the author's aliases, stated
		// once a write.
		Name: "lens", MovedBy: movedWithLenses, Nodes: 1, Tuples: 11, Assumed: 2 * sizingWindowDays,
		Driver: "assumed at 2 a day, a pre-mortem and one other, since production writes no lens before" +
			" this release: its node, its visibility, state and project, its part_of the node it analyses," +
			" its has_kind, at most its verdict value, its artifact_id value and its triggered_by, its authored_by" +
			" and at most two aliases of its author",
	}, {
		// Counted off api's retireConvertedDocument, the one writer of a moved_to out of a document's
		// address, which appends one through writeReasoningPointer each time the pre-mortem conversion
		// (api.ConvertPremortem) retires a document.
		Name: "document_move", MovedBy: movedWithLenses, Tuples: 1, Assumed: 1 * sizingWindowDays,
		Driver: "assumed at one a day, the pre-mortem the lens term assumes a day, since the one writer of a moved_to" +
			" out of a document's address is the pre-mortem conversion, which moves the address of each pre-mortem" +
			" document an earlier release wrote to the lens it writes in its place once, a backlog no table dates:" +
			" the document's moved_to, its lens and risks being counted by the lens and risk terms and its" +
			" retirement by artifact_removed",
	}, {
		// Counted off api's newFinding and writeReasoningNodes, a finding written with its lens
		// (invokeLens) or on its own (addLensFinding): the visibility, state and project, the part_of
		// and the text value, and graphstore.AuthorFacts' authored_by and aliases, which a finding
		// written with its lens shares with that write.
		Name: "lens_finding", MovedBy: movedWithLenses, Nodes: 1, Tuples: 8, Assumed: 10 * sizingWindowDays,
		Driver: "assumed at 5 under each assumed lens, 10 a day: a finding block's node, its visibility, state and" +
			" project, its part_of its lens, its text value, its authored_by and at most two aliases of its author," +
			" which over-states a finding written with its lens, whose write states those aliases once",
	}, {
		// Counted off the seed of migrations 20261123174710, ten kinds, and 20261123174720, twelve,
		// which graphstore.AddLensKind writes the shape of: the lens_kind node's visibility, state and
		// words values and its constrained_by, and its constraint node's visibility, state, standard
		// and reference values, in no project and by no author.
		Name: "lens_kind", MovedBy: movedWithLenses, Nodes: 2, Tuples: 8, Assumed: 22,
		Driver: "assumed at the 22 kinds the catalogue's seed writes, R7's ten and twelve further ones, taken as" +
			" landing inside the window, which over-states a write made once: its node, its visibility and state, its" +
			" words, its constrained_by, and its source's constraint node with that node's visibility, state, source" +
			" and reference",
	}, {
		// Counted off api's writeConstraint and writeReasoningNodes: the visibility, state and project,
		// the part_of its issue, one value of each field the body names (constraintAttributes), and
		// graphstore.AuthorFacts' authored_by and aliases; and the constrained_by the requirement's
		// pointer write appends (POST .../nodes/{id}/pointers).
		Name: "constraint", MovedBy: movedWithLenses, Nodes: 1, Tuples: 11, Assumed: 1 * sizingWindowDays,
		Driver: "assumed at 1 a day, since production writes no constraint before this release: its node, its" +
			" visibility, state and project, its part_of its issue, at most three values, a standard and its" +
			" reference or a measurement, its value and where it was taken, its authored_by, at most two aliases of" +
			" its author, and the constrained_by of the requirement it forced",
	}, {
		// Counted off api's writeRisk and writeReasoningNodes, which the pre-mortem conversion
		// (api.ConvertPremortem) writes its risks through too: the visibility, state and project, the
		// part_of its lens, the heading, early_warning and prevention values the route requires, and
		// graphstore.AuthorFacts' authored_by and aliases, with one prevented_by.
		Name: "risk", MovedBy: movedWithLenses, Nodes: 1, Tuples: 11, Assumed: 11 * sizingWindowDays,
		Driver: "assumed at 11 a day, one pre-mortem's risks a day as the one this tree holds has eleven, since" +
			" production writes no risk before this release: its node, its visibility, state and project, its" +
			" part_of its lens, its heading, early_warning and prevention values, its authored_by, at most two aliases" +
			" of its author, and one prevented_by",
	}, {
		// Counted off api's writeNodeState, which the state route and the settle route write a
		// node's state through: the one state value it appends.
		Name: "node_state", MovedBy: movedWithLenses, Tuples: 1, Assumed: 10 * sizingWindowDays,
		Driver: "assumed at 10 a day, one for each assumed finding, since production writes no node state but live and" +
			" gone before the lenses child ships: a node's proposed, settled or overtaken state value",
	}, {
		// Counted off api's deleteNode (R7's DELETE /api/v1/issues/{key}/nodes/{id}), which takes a
		// lens, a finding, a risk or a constraint gone with what stands under it: each such node's
		// state gone (deletePlan.goneNodes) and, as no other writer of those kinds does, the
		// retraction of every relation out of it (deletePlan.retractions). A block of a document it
		// takes leaves its document's text, which the version the delete writes of that document
		// states gone, as the block terms count; the version a query's drain writes after it is the
		// version terms'; and the anchored_in and the part_of of a comment whose node anchor it
		// orphans are anchor_block's.
		Name: "node_delete", MovedBy: movedHere, Tuples: 7, Assumed: 1 * sizingWindowDays,
		Driver: "assumed at 1 a day, half the lenses the lens term assumes, since production writes no lens, finding," +
			" risk or constraint before this release: a delete of a lens with one finding, each one's state gone and the" +
			" retraction of every relation out of each - the lens's part_of, has_kind and authored_by, the finding's" +
			" part_of and authored_by - which over-states a lens with no finding and under-states one with several",
	}, {
		// Counted off graphstore.Retarget, which the pointer route (api's writeReasoningPointer)
		// moves a pointer through: the retraction of the far end it held and the assertion of the
		// new one.
		Name: "pointer_retarget", MovedBy: movedWithLenses, Tuples: 2, Assumed: 1 * sizingWindowDays,
		Driver: "assumed at 1 a day, since production writes no pointer before the lenses child ships: a retarget of" +
			" a lens's, a risk's or a requirement's pointer, the retraction of the far end it held and the assertion" +
			" of the new one",
	}}
}

// sizingAuthors is the relation recordedSessions reads storedAuthors as when the sizing reads it:
// a FROM item named stored, of (kind, id, at), that holds no row. Every writer storedAuthors adds
// is read off the store itself - a decision's or a reading's decided_by or authored_by, an ask
// node's authored_by - and the sizing reads no table of the store, so it runs on a database the
// store does not exist in yet. A session that asked is counted all the same, by the asks rows and
// the ask.opened events recordedSessions reads beside it, and a decision or a reading is a kind
// production writes none of yet, whose rate the sizing assumes.
const sizingAuthors = `(select null::text, null::text, null::timestamptz where false) stored (kind, id, at)`

// reachesANode is true of a topic the outbox reads as a node of the store (outbox's
// nodeOfSubject): a session's own topic or a role's, with a name after the prefix, and that name
// inside the store's address bound, since the outbox leaves out a delivered_to whose far end is
// past it (outbox.deliveredGraph, outbox.fallbackDelivered). topic is the SQL expression that
// holds the topic. The prefixes are ASCII, so the bytes past the prefix are the name's.
func reachesANode(topic string) string {
	names := func(prefix string) string {
		return `(` + topic + ` like '` + prefix + `_%' and octet_length(` + topic + `) - ` +
			strconv.Itoa(len(prefix)) + ` <= ` + addressBound + `)`
	}
	return `(` + names(contracts.AgentTopicPrefix) + ` or ` + names(contracts.RoleTopicPrefix) + `)`
}

// sizingWritesWith is the sets of rows several terms count over, each read once for every term
// that counts it: the comments written inside the window with the facts each one's write states
// beside its node; the document_block_texts rows of the versions written inside it, each with
// whether the block's row before it held a text (null for the block's first row), how many authors
// its version lists, whether its document is an issue's spec, Verification document or plan
// (roleDocument), whether it is a criterion block's row, and whether it is a plan_task block's row
// whose opening line writes a size, as the renderer writes one there (refs.WrittenSizeSQL); the
// messages written inside it, and each issue message's thread root, found up in_reply_to as
// user_threads_message finds it; the thread of each comment,
// issue message and answer written inside it, with its writer as user_threads_record names one; the
// ask events inside it; the people a comment first names, the roles an event first addresses, and
// the components an import inside it wrote.
//
// A comment's facts are read off its row as their writers read them: its author as authoredLink
// does (graphstore.AuthorOf), a document it is anchored to or owned by only where the document's
// address fits the store's bound, and its aliases as graphstore's aliases states them - the owner a
// session's credential proved, the session a person's credential proved, and the raw id a person
// was recorded under where that is not their canonical login. The person of the last two is the
// author authorLogin reads off the row, where that author is a person, as commentWriterPeople's
// first arm reads one. rooms says the database holds the issue rooms' tables (documentTexts).
//
// The people are the ones the census holds to a node (commentWriterPeople), each with the time
// the first comment naming them was written.
func sizingWritesWith(rooms bool) string {
	return `
	with recursive sized_comments as (
		select c.id, coalesce(c.ask_id, c.reply_to, c.id) as thread,
		       case when c.author->>'kind' = 'user' then user_threads_login(c.author->>'id') end as writer,
		       (c.reply_to is not null or c.ask_id is not null or c.answers is not null
		        or c.replies_to_block is not null) as replies,
		       (` + authoredLink("c.author") + `) is not null as authored,
		       coalesce(octet_length(anchor.ref_key) <= ` + addressBound + `, false) as anchored,
		       coalesce(octet_length(owner.ref_key) <= ` + addressBound + `, false) as owned,
		       (case when ` + provedSessionOwner("c.author") + `
		                  and user_threads_login(coalesce(c.author->>'owner', '')) <> '' then 1 else 0 end)
		       + (case when person.login <> '' and coalesce(c.author->'act'->>'session', '') <> ''
		                    and coalesce(c.author->'act'->>'token_id', '') = ''
		                    and octet_length(c.author->'act'->>'session') <= ` + addressBound + ` then 1 else 0 end)
		       + (case when person.login <> '' and coalesce(c.author->>'id', '') <> ''
		                    and c.author->>'id' <> person.login then 1 else 0 end) as aliases
		from comments c` + refs.CommentStoreJoins + `
		cross join lateral (select user_threads_login(` + authorLogin("c.author") + `)) person (login)
		where c.created_at > ` + sizingWindow + `
	),
	sized_texts as (
		select t.text is not null as written, earlier.held as earlier,
		       case when jsonb_typeof(v.authors) = 'array' then jsonb_array_length(v.authors) else 0 end as authors,
		       exists (select 1 from artifacts d where d.id = v.artifact_id and ` + roleDocument("d") + `) as role,
		       coalesce(split_part(t.text, chr(10), 1) ~ '^:{3,}criterion\{', false) as criterion,
		       coalesce(split_part(t.text, chr(10), 1) ~ '^:{3,}plan_task\{.* ` + refs.AttributePlanSize + `="', false) as sized
		from artifact_versions v
		join document_block_texts t on ` + documentTexts("t", "v.artifact_id", rooms) + ` and t.version = v.number
		left join lateral (
			select p.text is not null as held from document_block_texts p
			where p.artifact_id = t.artifact_id and p.block_id = t.block_id and p.version < t.version
			order by p.version desc limit 1
		) earlier on true
		where v.created_at > ` + sizingWindow + `
	),
	sized_messages as (
		select m.id, m.in_reply_to, m.target, m.broadcast_id, m.issue_key is not null as threaded,
		       (m.in_reply_to is not null or m.replies_to_block is not null) as replies,
		       case when m.author->>'kind' = 'user' then user_threads_login(m.author->>'id') end as writer
		from messages m
		where m.created_at > ` + sizingWindow + `
	),
	message_roots (id, root, parent, depth) as (
		select m.id, m.id, m.in_reply_to, 0 from sized_messages m where m.threaded
		union all
		select r.id, p.id, p.in_reply_to, r.depth + 1
		from message_roots r join messages p on p.id = r.parent
		where r.depth < 64
	),
	sized_ask_events as (
		select e.type, e.payload,
		       case when e.actor->>'kind' = 'user' then user_threads_login(e.actor->>'id') end as writer
		from events e
		where e.type like 'ask.%' and e.created_at > ` + sizingWindow + `
	),
	sized_threads as (
		select c.thread, c.writer from sized_comments c
		union all
		select r.root, m.writer from message_roots r join sized_messages m on m.id = r.id
		where r.parent is null or r.depth = 64
		union all
		select case when e.payload->>'id' ~ '^[0-9a-fA-F-]{36}$' then (e.payload->>'id')::uuid end, e.writer
		from sized_ask_events e where e.type = 'ask.answered'
	),
	sized_people as (
		select named.login, min(named.at) as first
		from (` + commentWriterPeople + `) named
		group by named.login
	),
	sized_roles as (
		select addressed.name
		from (` + addressedRoles + `) addressed
		join events e on e.id = addressed.event
		where e.created_at > ` + sizingWindow + `
	),
	sized_components as (
		select c.project_key, c.id, c.parent
		from components c
		join architecture_snapshots s on s.id = c.snapshot_id
		where s.imported_at > ` + sizingWindow + `
	)`
}

// readStoreKinds reads how many writes of each term the window holds: every term a statement
// counts in one statement over sizingWritesWith, so each set of rows it names is read once, the
// moved parents, their ranks and their places in the text by one replayParents, and an assumed
// term from the writes it is assumed at.
func readStoreKinds(ctx context.Context, tx pgx.Tx) ([]StoreKind, error) {
	kinds := storeKinds()
	counts := make([]string, 0, len(kinds))
	targets := make([]any, 0, len(kinds))
	for index := range kinds {
		if kinds[index].count == "" {
			continue
		}
		counts = append(counts, kinds[index].count)
		targets = append(targets, &kinds[index].Writes)
	}
	// The sizing reads a database before this branch's migrations as well, which holds no issue
	// room's table (documentTexts).
	var rooms bool
	if err := tx.QueryRow(ctx, `select to_regclass('issue_room_blocks') is not null`).Scan(&rooms); err != nil {
		return nil, fmt.Errorf("read whether the database holds issue rooms: %w", err)
	}
	if err := tx.QueryRow(ctx, sizingWritesWith(rooms)+"\n\tselect "+strings.Join(counts, ",\n\t\t")).Scan(targets...); err != nil {
		return nil, fmt.Errorf("read the writes of each kind inside the window: %w", err)
	}
	replayed, err := replayParents(ctx, tx, rooms)
	if err != nil {
		return nil, err
	}
	for index := range kinds {
		kind := &kinds[index]
		switch {
		case kind.Name == movedParents:
			kind.Writes, kind.Replayed, kind.Unreadable = replayed.moved, replayed.read, replayed.unreadable
		case kind.Name == movedRanks, kind.Name == movedTextPlaces:
			kind.Writes = replayed.moved
		case kind.Assumed > 0:
			kind.Writes = kind.Assumed
		}
		kind.RowsADay = float64(kind.Writes) / sizingWindowDays
		kind.TuplesADay = kind.RowsADay * float64(kind.Tuples)
		kind.NodeRowsADay = kind.RowsADay * float64(kind.Nodes)
	}
	return kinds, nil
}

// roleDocument is true of the artifacts row aliased artifact while it is an issue's spec, its
// Verification document or its plan: the three documents an issue's room holds once the issue
// converts (R5, R6), each a query node of the issue's tree (graphstore.QueryNode).
func roleDocument(artifact string) string {
	return `(` + artifact + `.issue_key is not null and (` + artifact + `.is_primary or ` + artifact + `.slug in ('spec', '` +
		model.ArtifactSlugVerification + `', '` + model.ArtifactSlugPlan + `')))`
}
