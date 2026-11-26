package model

import (
	"encoding/json"
	"time"
)

// IssueTreeRead is GET /api/v1/issues/{key}/tree: the nodes of one issue's tree as one reader may
// see them, in the tree's own order (below), and every tuple whose two ends the answer holds.
//
// Issue is the issue read and View the view it answered, "" for the read with no view. Version is
// the version of the view's query node the answer was read at (`&v=`), null for the head.
// Converted, Tree, ViewOf and Documents are the issue's own (IssueTreeIssue), at the top level so a
// page reads them without finding the issue's node first.
//
// Nodes is the answer's nodes: in the read with no view, the skeleton of the issue's tree walked
// from its own node, then its second part, then the context nodes; in a view, the nodes of its
// query and its fixed context; with `kind`, the nodes of the kinds it names out of the skeleton,
// with their ancestors as context. The walk takes each node before its children, and orders a
// node's children so: a conversation item's own blocks first, in their position order; then its
// conversation nodes - asks, comments, messages, lenses and risks - by when each was made, so the
// discussion reads in the order it was written; then its children that hold a rank, by rank; then
// the rest, such as a block new in a deferred version that holds no rank yet, by when each was
// made; each tie by the lower id. Under the issue's node its memo callout stands first, before the
// conversation. The skeleton is every node of the tree but the blocks of the issue's
// documents other than its spec, its Verification document, its plan and its tree's own fragment,
// and but the comments off its comment page. Every answer also carries each comment on the text of
// a node it returns, with every comment of that comment's thread: a comment no person wrote in, and
// in a view every such comment, as context. DocumentNodes, in the read with no view alone, is each
// document of kind doc that GET /api/v1/issues/{key} lists, in that read's order
// (IssueTreeDocumentNode): a page reads its blocks through GET /api/v1/artifacts/{id}/blocks when
// its reader opens it. Tuples is every relation the store holds now between two nodes of the
// answer, of every kind, each end named by the node's store kind and id. Cursor is where the next
// page starts, the next node of the walk, and null on the last page; a page past the first carries
// the tuples whose near end is one of its own nodes. A page ends at the page size of nodes or at
// 2,000,000 bytes of JSON, whichever comes first, and each context node stands on the first page
// whose nodes hold a tuple naming it. Remaining, beside a cursor alone, is the number of nodes the
// read still holds after the page, never 0; a server of the release before sends none.
//
// The comment page is the 200 newest of the comments the answer carries, by made_at and then by
// id, each with the nodes under it: in the read with no view and with `kind`, the issue's
// conversation comments and the comments on the text of its nodes; in a view, the comments on the
// text of its nodes. CommentsBefore is the position of the oldest comment of the page where older
// ones remain, written `<made_at>,<id>` (made_at in RFC 3339, UTC, with microseconds), and null
// where none does: the read with `comments_before=<made_at>,<id>` beside the same view or kinds
// answers the next 200 older comments, with the nodes under them and their tuples, and no other
// node. A comment whose parent the answer does not hold names that parent as its parent and stands
// under the issue, by made_at and then by id. A server of the release before sends CommentsBefore
// in the read with no view alone.
//
// OwedDocuments names each document of the issue that owes its block index (OwedDocument), absent
// where none does: a node of such a document the block index has not reached is not in the answer
// yet, so the read names its document in its place.
type IssueTreeRead struct {
	Issue          string                  `json:"issue"`
	View           string                  `json:"view"`
	Version        *int                    `json:"version"`
	Converted      bool                    `json:"converted"`
	Tree           *string                 `json:"tree"`
	ViewOf         *string                 `json:"view_of"`
	Documents      *IssueTreeDocuments     `json:"documents"`
	Nodes          []IssueTreeNode         `json:"nodes"`
	DocumentNodes  []IssueTreeDocumentNode `json:"document_nodes,omitzero"`
	Tuples         []IssueTreeTuple        `json:"tuples"`
	Cursor         *string                 `json:"cursor"`
	Remaining      *int                    `json:"remaining,omitempty"`
	CommentsBefore json.RawMessage         `json:"comments_before,omitempty"`
	OwedDocuments  []OwedDocument          `json:"owed_documents,omitempty"`
}

// IssueTreeDocumentNode is a document of kind doc of an issue as the read with no view lists it:
// its artifact id, its slug, and BlockCount, the number of blocks GET
// /api/v1/artifacts/{id}/blocks lists for it, whose blocks a page reads there when its reader
// opens it.
type IssueTreeDocumentNode struct {
	ArtifactID string `json:"artifact_id"`
	Slug       string `json:"slug"`
	BlockCount int    `json:"block_count"`
}

// IssueTreeIssue is what each issue of a tree read answers of itself: Converted, whether its
// documents have their document_rooms rows; Tree, the id of its own tree artifact (its
// issue_trees row), the room of each of its converted documents and the artifact a criterion is
// written into, null while it has not converted; ViewOf, the ancestor issue whose requirements it
// is a view of, the one a requirement written on it is refused `409 REQUIREMENT_BELONGS_TO_ANCESTOR`
// naming, null where it is no view; and Documents, while it has not converted, the ids of its spec,
// Verification and plan documents, null once it has. An issue node carries them as members of its
// own; no other node carries them.
type IssueTreeIssue struct {
	Converted bool                `json:"converted" required:"false"`
	Tree      *string             `json:"tree" required:"false"`
	ViewOf    *string             `json:"view_of" required:"false"`
	Documents *IssueTreeDocuments `json:"documents" required:"false"`
}

// IssueTreeDocuments is an unconverted issue's role documents by id, each null where it holds none:
// the documents a role predicate finds, so a file whose name gives a role's slug is none of them.
type IssueTreeDocuments struct {
	Spec         *string `json:"spec"`
	Verification *string `json:"verification"`
	Plan         *string `json:"plan"`
}

// IssueTreeNode is one node of a tree read.
//
// Kind and ID are the node's store kind and id, which no other node of any issue holds; BlockID is
// a block's id within its document, which a child's block may share with its parent's. Supertag
// is the type the node is taken by: a block's typed block, or the node's supertag. Issue names the
// issue the node belongs to where that is not the issue read, and ReadOnly is set on a node of an
// issue under the one read, which its own issue's room edits. Role says what the node is to the
// answer: `tree`, a node of the issue's tree in the read with no view, and with `kind` a node of a
// kind it names; `query`, a node of the view's query; `context`, a node the answer carries for one
// of those.
//
// Parent is the node it stands under in the answer: its store kind and id, its supertag, the first
// line of its text, and its issue where that is not the issue read. A node whose parent the reader
// may not see stands under the nearest ancestor the reader may see. The issue read's own node and
// a context node stand under none.
//
// Rank is its place among its siblings, null where it holds none. PlacementSeq is null for the
// issue read's own node; for any other node it is the seq of its current part_of tuple at the
// read's cut, else, for a child issue that holds none, the seq of its current child_of tuple at that
// cut, else null: the placement revision a move of the node names in its If-Match (PATCH
// /api/v1/issues/{key}/nodes/{id}); a server of the release before sends none. Author is the
// node's authors as the store records them, `[]` for none. State and Visibility are its node state
// and visibility.
// MadeAt is when the node was made, null where the store records no time. Attributes are its
// values as they stand at the read's cut, but its rank and its text, which members of their own
// carry. Text is its words, and TextTree their display tree, the one the read of its kind answers,
// present only for a caller that sent `Dispatch-Trees: 1`.
//
// NoConversation marks a node of the issue that no conversation node stands above. Document names,
// for a typed block of a role document, its document and its position in that document as the
// blocks read lists it. Ask carries an ask's wording and its answer beside its question, which is
// its text, as the decisions read answers them for the ask. Decision, Result, Check and Plan carry
// the members the decisions read, the results read and the ordered read answer for the node; the
// issue's own members ride on an issue node (IssueTreeIssue).
//
// Checks, on a requirement node alone, is the number of checks that serve it at the read's cut, of
// the issue read and of each issue under it, as GET /api/v1/issues/{key}/coverage counts them, 0
// where none does. Title, on a session node alone, is the newest title that session stamped,
// absent where it stamped none. Anchor, on a comment that holds an anchor, is that anchor as
// stored, on a document's block or on a node (IssueTreeAnchor), its orphaned mark as stored. A
// server of the release before sends none of the three.
type IssueTreeNode struct {
	Kind           string             `json:"kind"`
	ID             string             `json:"id"`
	BlockID        string             `json:"block_id,omitempty"`
	Supertag       string             `json:"supertag"`
	Issue          string             `json:"issue,omitempty"`
	ReadOnly       bool               `json:"read_only,omitempty"`
	Role           string             `json:"role" enum:"tree,query,context"`
	Parent         *IssueTreeParent   `json:"parent"`
	Rank           *string            `json:"rank"`
	PlacementSeq   *int64             `json:"placement_seq" required:"false"`
	Author         []Actor            `json:"author"`
	State          string             `json:"state" enum:"live,gone,proposed,settled,overtaken"`
	Visibility     string             `json:"visibility" enum:"project,author"`
	MadeAt         *time.Time         `json:"made_at"`
	Attributes     map[string]string  `json:"attributes"`
	Text           string             `json:"text"`
	TextTree       DisplayTree        `json:"text_tree,omitempty"`
	NoConversation bool               `json:"no_conversation,omitempty"`
	Document       *IssueTreeDocument `json:"document,omitempty"`
	Ask            *IssueTreeAsk      `json:"ask,omitempty"`
	Decision       *IssueTreeDecision `json:"decision,omitempty"`
	Result         *IssueTreeResult   `json:"result,omitempty"`
	Check          *IssueTreeCheck    `json:"check,omitempty"`
	Plan           *IssueTreePlan     `json:"plan,omitempty"`
	Checks         *int               `json:"checks,omitempty"`
	Title          *string            `json:"title,omitempty"`
	Anchor         *IssueTreeAnchor   `json:"anchor,omitempty"`
	*IssueTreeIssue
}

// IssueTreeDecision is what a decision node, and an ask a person answered, answers of the decision
// it is: every member the decisions read's Decision answers for it, the same names and values, and
// beside them the decision's uncertainty (low, medium or high) and the reason for it, as the
// decisions read's entries answer them, each null where the node holds none, as on an ask. A
// server of the release before sends neither, so the document marks both optional.
type IssueTreeDecision struct {
	Decision
	Uncertainty       *string `json:"uncertainty" required:"false"`
	UncertaintyReason *string `json:"uncertainty_reason" required:"false"`
}

// IssueTreeAnchor is the anchor a comment on text answers in a tree read (R31), as stored, in one
// of two forms. A document anchor answers its artifact_id, block_id, mark_id, version, quote and
// orphaned, as the comments read answers them (Anchor, which stores no occurrence), and beside
// them the zero-based occurrence of its quote within block_id, which the read computes from its
// mark's position in that block: the number of the block's matches of the quote that start before
// the mark, the count pmdoc.QuoteFinder.FindInBlock takes. It answers none for an anchor that is
// orphaned or names no block, nor where the document no longer holds the mark inside that block.
// An orphaned document anchor also answers ContainerNodeID: the id a tree read answers the node of
// the nearest typed block that holds the anchor's block by (IssueTreeNode.ID), found up that
// block's part_of chain at the read's cut whether or not the read answers that node, where no block
// of the chain up to it is gone at the cut and the reader may see it. It answers none where no such
// block resolves, and none for an anchor that is not orphaned. A node anchor, on the text of a
// lens, a finding, a risk, a constraint or a result, answers the node it names
// (IssueTreeAnchorNode), its quote, the occurrence of the quote in the node's text it stores, and
// orphaned, and no member of a document anchor.
type IssueTreeAnchor struct {
	ArtifactID      string               `json:"artifact_id,omitempty"`
	BlockID         *string              `json:"block_id,omitempty"`
	MarkID          string               `json:"mark_id,omitempty"`
	Version         int                  `json:"version,omitempty"`
	Node            *IssueTreeAnchorNode `json:"node,omitempty"`
	Quote           string               `json:"quote"`
	Occurrence      *int                 `json:"occurrence,omitempty"`
	Orphaned        bool                 `json:"orphaned"`
	ContainerNodeID string               `json:"container_node_id,omitempty"`
}

// MarshalJSON writes each form of the anchor with its own members alone: a document anchor as the
// comments read writes it (Anchor), its block_id null where it names no block, with its occurrence
// where the read computed one and its container_node_id where the read resolved one, and a node
// anchor as its node, its quote, its occurrence and its orphaned mark.
func (a IssueTreeAnchor) MarshalJSON() ([]byte, error) {
	if a.Node == nil {
		return json.Marshal(struct {
			Anchor
			Occurrence      *int   `json:"occurrence,omitempty"`
			ContainerNodeID string `json:"container_node_id,omitempty"`
		}{Anchor{ArtifactID: a.ArtifactID, BlockID: a.BlockID, MarkID: a.MarkID, Version: a.Version, Quote: a.Quote, Orphaned: a.Orphaned},
			a.Occurrence, a.ContainerNodeID})
	}
	return json.Marshal(struct {
		Node       *IssueTreeAnchorNode `json:"node"`
		Quote      string               `json:"quote"`
		Occurrence *int                 `json:"occurrence"`
		Orphaned   bool                 `json:"orphaned"`
	}{a.Node, a.Quote, a.Occurrence, a.Orphaned})
}

// IssueTreeAnchorNode is the node a node anchor names: its store kind and id, a finding's the
// block's that the store keys it by, and its node state, gone for a node deleted since.
type IssueTreeAnchorNode struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	State string `json:"state" enum:"live,gone,proposed,settled,overtaken"`
}

// IssueTreeEnd names a node by its store kind and id: the ends of a tuple.
type IssueTreeEnd struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// IssueTreeParent is the node a node of a tree read stands under, named so a page can draw it in
// one line without a second read.
type IssueTreeParent struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Supertag  string `json:"supertag"`
	FirstLine string `json:"first_line"`
	Issue     string `json:"issue,omitempty"`
}

// IssueTreeTuple is one relation between two nodes of a tree read: its kind and its two ends.
type IssueTreeTuple struct {
	Kind string       `json:"kind"`
	From IssueTreeEnd `json:"from"`
	To   IssueTreeEnd `json:"to"`
}

// IssueTreeDocument is where a typed block of a role document stands: the document's artifact id
// and slug, and the block's position in the list GET /api/v1/issues/{key}/artifacts/{slug}/blocks
// answers for that document.
type IssueTreeDocument struct {
	ArtifactID string `json:"artifact_id"`
	Slug       string `json:"slug"`
	Position   int    `json:"position"`
}

// IssueTreeAsk is what an ask node answers of its wording from the ask's row beside its question,
// which is the node's text: the options its asker gave, its urgency and whether it takes more than
// one option; and of its answer from its node: the answer's words, the options it selected, who
// answered and when, each null where the node holds none, as on a declined ask. Each is the name
// and the value GET /api/v1/issues/{key}/decisions answers on the ask's entry. Where a person
// answered it, it also answers the decision it is (IssueTreeNode.Decision). A server of the
// release before sends no answer member, so the document marks each optional.
type IssueTreeAsk struct {
	Options    []AskOption `json:"options"`
	Urgency    string      `json:"urgency"`
	Multiple   bool        `json:"multiple"`
	Answer     *string     `json:"answer" required:"false"`
	Selected   []string    `json:"selected" required:"false"`
	AnsweredBy *string     `json:"answered_by" required:"false"`
	AnsweredAt *time.Time  `json:"answered_at" required:"false"`
}

// IssueTreeResult is what a result node answers as GET /api/v1/issues/{key}/results answers it
// (CheckResult): its verdict, its retraction, left out where the result stands, and the check's
// version and text sum when it was recorded.
type IssueTreeResult struct {
	Verdict      string                `json:"verdict"`
	Retracted    *CheckResultRetracted `json:"retracted,omitempty"`
	CheckVersion int                   `json:"check_version"`
	CheckSHA256  string                `json:"check_sha256"`
}

// IssueTreeCheck is what a check node answers of its words as they stand: their text sum and the
// version they last changed in, its document_blocks row's, which the results read answers as a
// check's sha256 and version.
type IssueTreeCheck struct {
	TextSHA256     string `json:"text_sha256"`
	ChangedVersion int    `json:"changed_version"`
}

// IssueTreePlan is what the plan view answers of a node from the ordered read: its position in the
// one order of the whole tree, null for a node in a cycle or overtaken; the nodes it needs; its
// size in whole minutes, null where its author gave none; the skills and the docs its task names,
// each `[]` where it names none, never null and never left out, an issue and its halves included;
// and whether it stands in a cycle the read reports. An issue answers its two halves (Start, Done),
// each a node of the order of its own.
type IssueTreePlan struct {
	Position *int               `json:"position"`
	Needs    []IssueTreeHalfEnd `json:"needs"`
	Size     *string            `json:"size"`
	Skills   []string           `json:"skills"`
	Docs     []string           `json:"docs"`
	Cycle    bool               `json:"cycle"`
	Start    *IssueTreePlan     `json:"start,omitempty"`
	Done     *IssueTreePlan     `json:"done,omitempty"`
}

// IssueTreeHalfEnd names a node of the ordered read: a plan node by its store kind and id, or an
// issue by its key and the half (`start` or `done`) a wait reaches.
type IssueTreeHalfEnd struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Half string `json:"half,omitempty" enum:"start,done"`
}
