package graphstore

import (
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The relations between issues, each a tuple out of the issue's node (dispatch://INFRA-1253 R9,
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
