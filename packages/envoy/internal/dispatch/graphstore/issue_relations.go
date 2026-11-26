package graphstore

import (
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The relations between issues, each a tuple out of the issue's node (R9 of the issue-tree spec,
// R10). child_of names the issue's parent, which issues.parent_key projects; needs an issue it
// waits on, which an issue_links row projects, and is the kind a plan node's wait is stated
// under too, since waiting is one relation; duplicate_of the issue of its project it repeats;
// and filed_from the ask, comment or message it was filed out of. No row projects the last two.
// child_of, duplicate_of and filed_from are single-valued, so their writers state each as a
// Claim, which retracts the far end the issue held before; needs holds a set, whose writer
// appends what it adds and retracts what it drops.
const (
	TupleChildOf     = "child_of"
	TupleNeeds       = "needs"
	TupleDuplicateOf = "duplicate_of"
	TupleFiledFrom   = "filed_from"
)

// IssueChildOf is the fact that hangs an issue under its parent issue: the child_of tuple from the
// issue's node to its parent's, which issues.parent_key projects. Every writer that names an issue
// row's node states it beside that node for the parent the row holds. Its cause is the writer's
// to fill, or the sentinel where no event recorded the write.
func IssueChildOf(key, parent string, who model.Actor, at time.Time) Tuple {
	return Tuple{
		FromKind: KindIssue, FromID: key, Kind: TupleChildOf,
		ToKind: KindIssue, ToID: parent,
		At: at, Who: who, SourceEvent: NoEvent, Asserted: true,
	}
}

// IssueNeeds is the fact that issue key waits on issue target, or, with asserted false, that it
// waits on it no more: the needs tuple an issue_links row projects. Its cause is the writer's to
// fill, or the sentinel where no event recorded the write.
func IssueNeeds(key, target string, asserted bool, who model.Actor, at time.Time) Tuple {
	return Tuple{
		FromKind: KindIssue, FromID: key, Kind: TupleNeeds,
		ToKind: KindIssue, ToID: target,
		At: at, Who: who, SourceEvent: NoEvent, Asserted: asserted,
	}
}

// TupleMentions is the kind a citation is stated under: a document's mention of a node, which a
// refs row of the document projects (ArtifactMentions), and a block's mention of one, which a
// block_refs row projects (refs' IndexBlocks).
const TupleMentions = "mentions"

// ArtifactMentions is the fact that a document cites a node, or, with asserted false, that it
// cites it no more: the mentions tuple out of the document's node, addressed by its ref_key
// (ArtifactNode), to the far end a refs row of the document names, its to_kind and to_id as the
// row holds them. A refs row whose source is a document projects it; a comment's, a message's or
// an ask's projects none. Its cause is the writer's to fill, or the sentinel where no event
// recorded the write.
func ArtifactMentions(refKey, toKind, toID string, asserted bool, who model.Actor, at time.Time) Tuple {
	return Tuple{
		FromKind: KindArtifact, FromID: refKey, Kind: TupleMentions,
		ToKind: toKind, ToID: toID,
		At: at, Who: who, SourceEvent: NoEvent, Asserted: asserted,
	}
}

// QueueMentionsDated queues on b the statement that dates each mention the document documentID's
// node states now as the refs row of the pair dates it: every current mentions tuple out of the
// node whose time or legacy_source_seq is not the row's created_at and source_seq is stated
// again, asserting the same far end, at the row's created_at and carrying its source_seq, signed
// by who signed the tuple and caused by the event eventID names, or by the event the write phase
// appended last where it is nil (store.LatestEventID). graph_edges answers a document's mention
// at its tuple's own time and sequence once the boot step has deleted the tuple arm's read of the
// refs row (store.GraphEdgesIssueTreeRetired), so a tuple holds the row's.
//
// It runs right after the update that stamps the document's refs rows with that event (refs'
// queueStamp, docs' stampVersionEvent), in the transaction that holds the document's owner: the
// stamp dates a row rebuild-refs inserted with no source_seq, whose tuple was appended with none
// (Replacement.ArtifactMentions), and this states it again with the stamp's. A row the same write
// inserts has no current tuple yet, or one its writer appended under the same event, which agrees,
// so neither is stated here. A document whose row names no node states nothing (its mentions are
// the view's arm over refs to answer until the backfill states them).
func QueueMentionsDated(b *store.WriteBatch, documentID string, eventID *int64) {
	event, args := store.LatestEventID, []any{documentID}
	if eventID != nil {
		event, args = "$2", append(args, *eventID)
	}
	b.Queue("date the mentions of document "+documentID+" as their refs rows", `
		insert into graph_tuples (`+tupleWriteColumns+`)
		select '`+KindArtifact+`', document.ref_key, '`+TupleMentions+`', stated.to_kind, stated.to_id, stated.created_at,
		       held.who, `+event+`, stated.source_seq, true
		from artifacts document
		join refs stated on stated.from_kind = '`+KindArtifact+`' and stated.from_id = document.id::text
		cross join lateral (
			select t.at, t.who, t.legacy_source_seq, t.asserted from graph_tuples t
			where t.from_kind = '`+KindArtifact+`' and t.from_id = document.ref_key and t.kind = '`+TupleMentions+`'
			  and t.to_kind = stated.to_kind and t.to_id = stated.to_id and t.to_kind <> '`+KindValue+`'
			order by t.seq desc limit 1
		) held
		where document.id = $1::uuid and document.node_kind is not null
		  and octet_length(stated.to_id) <= `+strconv.Itoa(MaxAddressBytes)+`
		  and held.asserted
		  and (held.at <> stated.created_at or held.legacy_source_seq is distinct from stated.source_seq)
		order by stated.to_kind, stated.to_id
		on conflict do nothing
	`, args...)
}
