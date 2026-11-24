package model

import (
	"encoding/json"
	"time"
)

// IssueTreeRead is GET /api/v1/issues/{key}/tree: the nodes of one issue's tree as one reader may
// see them, in the tree's own order, and every tuple whose two ends the answer holds.
//
// Issue is the issue read and View the view it answered, "" for the read with no view. Version is
// the version of the view's query node the answer was read at (`&v=`), null for the head.
// Converted, Tree, ViewOf and Documents are the issue's own (IssueTreeIssue), at the top level so a
// page reads them without finding the issue's node first.
//
// Nodes is the answer's nodes: in the read with no view, the skeleton of the issue's tree walked
// from its own node (each before its children, ranked siblings by rank and then the rest by when
// each was made), then its second part, then the context nodes; in a view, the nodes of its query
// and its fixed context. The skeleton is every node of the tree but the blocks of the issue's
// documents other than its spec, its Verification document, its plan and its tree's own fragment,
// and but the comments off its comment page. DocumentNodes, in the read with no view alone, is each
// document of kind doc that GET /api/v1/issues/{key} lists, in that read's order
// (IssueTreeDocumentNode): a page reads its blocks through GET /api/v1/artifacts/{id}/blocks when
// its reader opens it. Tuples is every relation the store holds now between two nodes of the
// answer, of every kind, each end named by the node's store kind and id. Cursor is where the next
// page starts, the next node of the walk, and null on the last page; a page past the first carries
// the tuples whose near end is one of its own nodes.
//
// The comment page is the 200 newest of the issue's conversation comments, by made_at and then by
// id, each with the nodes under it. CommentsBefore, in the read with no view alone, is the
// position of the oldest comment of the page where older ones remain, written `<made_at>,<id>`
// (made_at in RFC 3339, UTC, with microseconds), and null where none does: the read with
// `comments_before=<made_at>,<id>` answers the next 200 older comments, with the nodes under them
// and their tuples, and no other node. A comment whose parent the answer does not hold names that
// parent as its parent and stands under the issue, by made_at and then by id.
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
// answer: `tree`, a node of the issue's tree in the read with no view; `query`, a node of the
// view's query; `context`, a node the answer carries for one of those.
//
// Parent is the node it stands under in the answer: its store kind and id, its supertag, the first
// line of its text, and its issue where that is not the issue read. A node whose parent the reader
// may not see stands under the nearest ancestor the reader may see. The issue read's own node and
// a context node stand under none.
//
// Rank is its place among its siblings, null where it holds none. Author is the node's authors as
// the store records them, `[]` for none. State and Visibility are its node state and visibility.
// MadeAt is when the node was made, null where the store records no time. Attributes are its
// values as they stand at the read's cut, but its rank and its text, which members of their own
// carry. Text is its words, and TextTree their display tree, the one the read of its kind answers,
// present only for a caller that sent `Dispatch-Trees: 1`.
//
// NoConversation marks a node of the issue that no conversation node stands above. Document names,
// for a typed block of a role document, its document and its position in that document as the
// blocks read lists it. Ask carries an ask's wording beside its question, which is its text, as the
// decisions read answers it for the ask. Decision, Result, Check and Plan carry the members the
// decisions read, the results read and the ordered read answer for the node; the issue's own
// members ride on an issue node (IssueTreeIssue).
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
	Decision       *Decision          `json:"decision,omitempty"`
	Result         *IssueTreeResult   `json:"result,omitempty"`
	Check          *IssueTreeCheck    `json:"check,omitempty"`
	Plan           *IssueTreePlan     `json:"plan,omitempty"`
	*IssueTreeIssue
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
// one option, as GET /api/v1/issues/{key}/decisions answers them on the ask's entry. Its answer is
// the node's own values (IssueTreeNode.Attributes), and where a person answered it, the decision it
// is (IssueTreeNode.Decision).
type IssueTreeAsk struct {
	Options  []AskOption `json:"options"`
	Urgency  string      `json:"urgency"`
	Multiple bool        `json:"multiple"`
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
// size, null where its author gave none; and whether it stands in a cycle the read reports. An
// issue answers its two halves (Start, Done), each a node of the order of its own.
type IssueTreePlan struct {
	Position *int               `json:"position"`
	Needs    []IssueTreeHalfEnd `json:"needs"`
	Size     *string            `json:"size"`
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
