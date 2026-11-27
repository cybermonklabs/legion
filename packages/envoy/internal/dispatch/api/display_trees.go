package api

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// displayTreesHeader is what a caller sends to be given the display tree of every stored text in
// the answer, beside the markdown itself: `Dispatch-Trees: 1` and nothing else. The dashboard
// sets it on every request it makes (DispatchApiClient#answered); the command-line tool and every
// agent client send none, so their bytes are exactly what they were. Every tree field is
// omitempty, so the header is the only thing that puts one in a response.
const displayTreesHeader = "Dispatch-Trees"

// wantsDisplayTrees reports whether this request asked for the display trees.
func wantsDisplayTrees(r *http.Request) bool {
	return r.Header.Get(displayTreesHeader) == "1"
}

// varyOnDisplayTrees tells every cache between the browser and Dispatch that the body depends on
// the request's Dispatch-Trees header, so an answer given to the dashboard is never handed to the
// command-line tool or the other way round. Register wraps every mounted route in it rather than
// each handler that reads the header naming itself: a route that gains a tree field later cannot
// then forget the Vary, and a route that carries no stored text pays one constant header.
func varyOnDisplayTrees(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", displayTreesHeader)
		handler(w, r)
	}
}

// displayTreeBuilds makes one reading of one text at a time. A request reads the texts the table
// held no tree of on several processors at once (buildDisplayTrees), and the display cache holds
// a tree only once it is built, so without this the goroutines of one read - and two readers of
// one text at once - would each parse it. The key is the text itself: one reading per text,
// which is what the cache is keyed by (its SHA-256 and the mode, and this builds only
// DisplayLatest, the one mode a list is read in).
var displayTreeBuilds singleflight.Group

// builtDisplayTree is the tree of a text whose sum its caller already holds, read once however
// many goroutines want it. A mode other than the latest one is read outside the singleflight:
// its one reader is a past version's own text, which no list repeats and no second shaper asks
// for at the same moment.
func builtDisplayTree(sum [32]byte, markdown string, mode pmdoc.DisplayMode) json.RawMessage {
	if mode != pmdoc.DisplayLatest {
		return pmdoc.DisplayJSONOf(sum, markdown, mode)
	}
	built, _, _ := displayTreeBuilds.Do(markdown, func() (any, error) {
		return pmdoc.DisplayJSONOf(sum, markdown, mode), nil
	})
	tree, _ := built.(json.RawMessage)
	return tree
}

// displayExactJudgements makes one judgement of one text at a time (pmdoc.DisplayExact): the
// verdict cache holds a verdict only once it is made, so without this two requests that read one
// description at once, cold, would each parse it. The key is the text's SHA-256, which is what
// the verdict cache is keyed by.
var displayExactJudgements singleflight.Group

// judgedDisplayExact is whether Parse takes whole a text whose sum its caller already holds,
// judged once however many goroutines ask at the same moment.
func judgedDisplayExact(sum [32]byte, markdown string) bool {
	judged, _, _ := displayExactJudgements.Do(string(sum[:]), func() (any, error) {
		return pmdoc.DisplayExact(sum, markdown), nil
	})
	exact, _ := judged.(bool)
	return exact
}

// buildDisplayTrees reads the count texts of one request that the table held no tree of, each by
// its own index, on as many processors as the process may use. A read of a 200-comment issue
// after a restart reads 200 texts, each a markdown parse; one after another that is the whole
// cost of the read (the cold read's profile), and the processors sit idle. Each build writes
// only its own text's tree, so the answer's bytes and their order are what they were.
//
// A single text, and a process that may use one processor, are read where they stand: a
// goroutine would cost more than it saves.
func buildDisplayTrees(count int, build func(index int)) {
	workers := runtime.GOMAXPROCS(0)
	if count < 2 || workers < 2 {
		for index := range count {
			build(index)
		}
		return
	}
	if workers > count {
		workers = count
	}
	var wait sync.WaitGroup
	texts := make(chan int)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range texts {
				build(index)
			}
		}()
	}
	for index := range count {
		texts <- index
	}
	close(texts)
	wait.Wait()
}

// shapeBuilt shapes one row of a response that reads its texts where they stand rather than
// from the table: a write's own answer, which carries the text the request has just written and
// stored the tree of, and a cut or a piece of a stored text a read answers - an edge's excerpt, an
// Inbox row's subject, an ambiguous id's candidate first line, a string inside a handoff's body -
// which no writer stores a tree of. The shaping is the shaping every read uses, over a reader of
// its own that is resolved before the row is handed back (builtDisplayTrees).
func shapeBuilt(trees bool, shape func(reader *displayTreeReader)) {
	reader := builtDisplayTrees(trees)
	shape(reader)
	reader.resolve()
}

// The read shapes below are the dashboard's own view of a stored row: each embeds the row the
// whole deployment shares and shadows the fields whose text gains a tree, so the JSON keeps every
// key it had and gains `<field>_tree` beside each text. The tree never goes on the shared row
// itself, because an event payload carries that row and no payload gains a field; an event's own
// trees ride in the envelope's `trees` map instead (eventDisplayTrees).
//
// Each shape is built from its row by shape(reader), called once the row is final - after the
// part ids, the reach and everything else a read attaches - and before the request's reader is
// resolved, which is what fills every tree field at once. With no reader it only re-wraps what
// the row already held, so it answers the keys and the values the route wrote before this
// existed. The order of those keys is the row's own only where the shadowing field is its last:
// encoding/json writes an embedded row's fields first and the fields beside them after, so a
// shadowed field anywhere else moves to the end. A shape whose bytes keep the row's order
// declares the row's fields itself instead of embedding it (graphEdgeRead).

// askOptionRead is one answer choice with the trees of its label and description.
type askOptionRead struct {
	model.AskOption
	LabelTree       model.DisplayTree `json:"label_tree,omitempty"`
	DescriptionTree model.DisplayTree `json:"description_tree,omitempty"`
}

// askResolutionRead is why an ask no longer needs an answer, with the tree of that reason.
type askResolutionRead struct {
	model.AskResolution
	ReasonTree model.DisplayTree `json:"reason_tree,omitempty"`
}

// askEditPreviousRead is the content an ask had before an edit, with its question's and its
// options' trees.
type askEditPreviousRead struct {
	model.AskEditPrevious
	QuestionTree model.DisplayTree `json:"question_tree,omitempty"`
	Options      []askOptionRead   `json:"options"`
}

// askEditRead is one recorded rewording of an ask, the content it replaced carrying its trees.
type askEditRead struct {
	model.AskEdit
	Previous askEditPreviousRead `json:"previous"`
}

// shapedAsk is an ask as the dashboard reads it: the ask, with the trees of its question, of
// each option, of its answer's text and of its resolution's reason. It is not called askRead,
// which is main's own name for the body GET /api/v1/asks/{id} answers (asks.go), exactly as
// shapedMessage is not called messageRead.
type shapedAsk struct {
	model.Ask
	QuestionTree model.DisplayTree        `json:"question_tree,omitempty"`
	Options      []askOptionRead          `json:"options"`
	Answer       *model.AskAnswerWithTree `json:"answer"`
	Resolution   *askResolutionRead       `json:"resolution,omitempty"`
}

// shape rebuilds the shadowing fields from the ask they stand in front of, and registers each of
// its texts with the request's reader. The texts are texttrees.VisitAskTexts, the one walk this
// read, the event path and the background fill of the trees all make, and treeField says where
// each of them lands here: a text the walk gains that this shape has no field for is registered
// nowhere, which is what the agreement test refuses (ask_texts_test.go). The labels the answer
// chose are registered beside the walk (registerChosenLabels), since each is an option's label
// the walk already names.
func (a *shapedAsk) shape(reader *displayTreeReader) {
	a.Options = askOptionReads(a.Ask.Options)
	a.Answer = askAnswerWithTree(a.Ask.Answer)
	a.Resolution = nil
	if a.Ask.Resolution != nil {
		a.Resolution = &askResolutionRead{AskResolution: *a.Ask.Resolution}
	}
	registerAskTexts(reader, texttrees.AskContentOf(a.Ask), a.treeField)
	registerChosenLabels(reader, a.Answer)
}

// treeField is the field of this read that carries the tree of text, and nil for a text this
// read shows nowhere.
func (a *shapedAsk) treeField(text texttrees.AskText) *model.DisplayTree {
	switch text.Field {
	case texttrees.AskQuestionText:
		return &a.QuestionTree
	case texttrees.AskAnswerText:
		if a.Answer == nil {
			return nil
		}
		return &a.Answer.TextTree
	case texttrees.AskResolutionReasonText:
		if a.Resolution == nil {
			return nil
		}
		return &a.Resolution.ReasonTree
	default:
		return askOptionTreeField(a.Options, text)
	}
}

// registerAskTexts registers every stored text of content with reader, each against the field of
// the read that shows it (fieldOf). A text that read holds no field for - an answer on an edit's
// previous content, which only the ask itself carries - is registered nowhere and read nowhere.
//
// Each field must stand where it will still stand when the reader resolves, which is why every
// shadowing field is built before the walk and none of them is appended to afterwards.
func registerAskTexts(
	reader *displayTreeReader,
	content texttrees.AskContent,
	fieldOf func(texttrees.AskText) *model.DisplayTree,
) {
	texttrees.VisitAskTexts(content, func(text texttrees.AskText) {
		if field := fieldOf(text); field != nil {
			reader.text(field, text.Text)
		}
	})
}

// askOptionTreeField is the field of reads that carries text's tree, and nil for a text that is
// no option's or whose option these reads do not hold.
func askOptionTreeField(reads []askOptionRead, text texttrees.AskText) *model.DisplayTree {
	if text.Option < 0 || text.Option >= len(reads) {
		return nil
	}
	switch text.Field {
	case texttrees.AskOptionLabelText:
		return &reads[text.Option].LabelTree
	case texttrees.AskOptionDescriptionText:
		return &reads[text.Option].DescriptionTree
	default:
		return nil
	}
}

// askOptionReads is the options shaped to carry the trees of their label and description,
// keeping an absent list absent so a row that carried no options still writes null rather than
// an empty list. The trees themselves are registered by the walk over the ask or over the
// content an edit replaced (registerAskTexts).
func askOptionReads(options []model.AskOption) []askOptionRead {
	if options == nil {
		return nil
	}
	reads := make([]askOptionRead, len(options))
	for index, option := range options {
		reads[index] = askOptionRead{AskOption: option}
	}
	return reads
}

// newShapedAsk is one ask shaped for a response of its own: a write's answer, which reads its
// texts where they stand (shapeBuilt). A read of several asks shapes them through shapedAsks,
// over the request's own reader.
func newShapedAsk(ask model.Ask, trees bool) shapedAsk {
	read := shapedAsk{Ask: ask}
	shapeBuilt(trees, read.shape)
	return read
}

// shapedAsks is a list of asks shaped for the dashboard, an absent list staying absent.
func shapedAsks(asks []model.Ask, reader *displayTreeReader) []shapedAsk {
	if asks == nil {
		return nil
	}
	reads := make([]shapedAsk, len(asks))
	for index := range asks {
		reads[index].Ask = asks[index]
		reads[index].shape(reader)
	}
	return reads
}

// askAnswerReads is a thread's answers with the trees of their texts and of the labels each
// chose. An earlier answer can name an option the ask no longer holds, and its label's tree is
// read as every other's is.
func askAnswerReads(answers []model.AskAnswer, reader *displayTreeReader) []model.AskAnswerWithTree {
	if answers == nil {
		return nil
	}
	reads := make([]model.AskAnswerWithTree, len(answers))
	for index := range answers {
		reads[index] = model.AskAnswerWithTree{AskAnswer: answers[index]}
		reader.textOf(&reads[index].TextTree, answers[index].Text)
		registerChosenLabels(reader, &reads[index])
	}
	return reads
}

// askEditReads is an ask's recorded rewordings, each previous content carrying its trees.
func askEditReads(edits []model.AskEdit, reader *displayTreeReader) []askEditRead {
	if edits == nil {
		return nil
	}
	reads := make([]askEditRead, len(edits))
	for index, edit := range edits {
		reads[index] = askEditRead{AskEdit: edit, Previous: askEditPreviousRead{
			AskEditPrevious: edit.Previous,
			Options:         askOptionReads(edit.Previous.Options),
		}}
		previous := &reads[index].Previous
		registerAskTexts(reader, texttrees.AskEditPreviousContent(edit.Previous), previous.treeField)
	}
	return reads
}

// treeField is the field of this previous content that carries the tree of text, and nil for a
// text an edit never carried away: the answer and the resolution are the ask's own.
func (p *askEditPreviousRead) treeField(text texttrees.AskText) *model.DisplayTree {
	if text.Field == texttrees.AskQuestionText {
		return &p.QuestionTree
	}
	return askOptionTreeField(p.Options, text)
}

// replyBlockRead is the block a reply answers, with the tree of the excerpt it stored.
type replyBlockRead struct {
	model.ReplyBlock
	TextTree model.DisplayTree `json:"text_tree,omitempty"`
}

// newReplyBlockRead is the block a reply answers shaped for the dashboard, nil for a reply that
// names none.
func newReplyBlockRead(block *model.ReplyBlock, reader *displayTreeReader) *replyBlockRead {
	if block == nil {
		return nil
	}
	read := &replyBlockRead{ReplyBlock: *block}
	reader.text(&read.TextTree, block.Text)
	return read
}

// shapedComment is a comment as the dashboard reads it: the comment, the tree of its body, and
// the block it answers carrying the tree of that block's excerpt. It is named as shapedMessage
// is, and for the same reason: commentRead is the body GET /api/v1/comments/{id} answers.
type shapedComment struct {
	model.Comment
	BodyTree   model.DisplayTree `json:"body_tree,omitempty"`
	ReplyBlock *replyBlockRead   `json:"reply_block"`
}

// shape rebuilds the shadowing fields from the comment and registers its texts with the
// request's reader.
func (c *shapedComment) shape(reader *displayTreeReader) {
	c.ReplyBlock = newReplyBlockRead(c.Comment.ReplyBlock, reader)
	reader.text(&c.BodyTree, c.Comment.Body)
}

// newShapedComment is one comment shaped for a response of its own, its texts read where they
// stand (shapeBuilt).
func newShapedComment(comment model.Comment, trees bool) shapedComment {
	read := shapedComment{Comment: comment}
	shapeBuilt(trees, read.shape)
	return read
}

// shapedComments is a list of comments shaped for the dashboard, an absent list staying absent.
func shapedComments(comments []model.Comment, reader *displayTreeReader) []shapedComment {
	if comments == nil {
		return nil
	}
	reads := make([]shapedComment, len(comments))
	for index := range comments {
		reads[index].Comment = comments[index]
		reads[index].shape(reader)
	}
	return reads
}

// shapedMessage is a message as the dashboard reads it, shaped exactly as a comment is. It is not
// called messageRead, which is main's own name for the conversation this package answers at
// GET /api/v1/messages/{id} (messages.go): both carry a Message field, so code written against
// one name under the other's meaning type-checks and decodes nothing (INFRA-635).
type shapedMessage struct {
	model.Message
	BodyTree   model.DisplayTree `json:"body_tree,omitempty"`
	ReplyBlock *replyBlockRead   `json:"reply_block"`
}

// shape rebuilds the shadowing fields from the message and registers its texts with the
// request's reader.
func (m *shapedMessage) shape(reader *displayTreeReader) {
	m.ReplyBlock = newReplyBlockRead(m.Message.ReplyBlock, reader)
	reader.text(&m.BodyTree, m.Message.Body)
}

// newShapedMessage is one message shaped for a response of its own, its texts read where they
// stand (shapeBuilt).
func newShapedMessage(message model.Message, trees bool) shapedMessage {
	read := shapedMessage{Message: message}
	shapeBuilt(trees, read.shape)
	return read
}

// shapedMessages is a list of messages shaped for the dashboard, an absent list staying absent.
func shapedMessages(messages []model.Message, reader *displayTreeReader) []shapedMessage {
	if messages == nil {
		return nil
	}
	reads := make([]shapedMessage, len(messages))
	for index := range messages {
		reads[index].Message = messages[index]
		reads[index].shape(reader)
	}
	return reads
}

// artifactReviewRead is one review of a document with the tree of the reason it gave.
type artifactReviewRead struct {
	model.ArtifactReview
	ReasonTree model.DisplayTree `json:"reason_tree,omitempty"`
}

// artifactReviewReads is a document's reviews shaped for the dashboard.
func artifactReviewReads(reviews []model.ArtifactReview, reader *displayTreeReader) []artifactReviewRead {
	if reviews == nil {
		return nil
	}
	reads := make([]artifactReviewRead, len(reviews))
	for index := range reviews {
		reads[index] = artifactReviewRead{ArtifactReview: reviews[index]}
		reader.textOf(&reads[index].ReasonTree, reviews[index].Reason)
	}
	return reads
}

// artifactApprovalRead is a document's approval with the tree of the reason its latest review
// gave. The tree stands here rather than on model.ArtifactApproval because that approval is part
// of the artifact an event payload carries, and no payload gains a tree field.
type artifactApprovalRead struct {
	model.ArtifactApproval
	ReasonTree model.DisplayTree `json:"reason_tree,omitempty"`
}

// artifactRead is a document as the dashboard reads it: the document, with its approval carrying
// the tree of that approval's reason.
type artifactRead struct {
	model.Artifact
	Approval *artifactApprovalRead `json:"approval,omitempty"`
}

// shape rebuilds the shadowing approval from the document it stands in front of and registers
// its reason with the request's reader. It runs where a route answers a reader rather than in
// attachApprovals, which every read of a document goes through, request or not: only a reader is
// shown the reason, and only one that asked for the trees.
func (a *artifactRead) shape(reader *displayTreeReader) {
	a.Approval = nil
	if approval := a.Artifact.Approval; approval != nil {
		a.Approval = &artifactApprovalRead{ArtifactApproval: *approval}
		reader.textOf(&a.Approval.ReasonTree, approval.Reason)
	}
}

// newArtifactApprovalRead is one approval shaped for a response of its own: the answer of the
// call that wrote it, which reads its reason where it stands (shapeBuilt).
func newArtifactApprovalRead(approval model.ArtifactApproval, trees bool) artifactApprovalRead {
	read := artifactApprovalRead{ArtifactApproval: approval}
	shapeBuilt(trees, func(reader *displayTreeReader) {
		reader.textOf(&read.ReasonTree, approval.Reason)
	})
	return read
}

// artifactReads is a list of documents shaped for the dashboard, an absent list staying absent.
func artifactReads(artifacts []model.Artifact, reader *displayTreeReader) []artifactRead {
	if artifacts == nil {
		return nil
	}
	reads := make([]artifactRead, len(artifacts))
	for index := range artifacts {
		reads[index].Artifact = artifacts[index]
		reads[index].shape(reader)
	}
	return reads
}

// blockPinRead is a block reference pinned to a version, with the tree of the text that version
// held.
type blockPinRead struct {
	model.BlockPin
	TextTree model.DisplayTree `json:"text_tree,omitempty"`
}

// graphNodeRead is one end of a reference-graph edge as the dashboard reads it: the node, the
// tree of its own text, and the pin carrying the tree of the text it recorded. A reference's
// title is read from one of those trees, as an ask's and a comment's titles are read from theirs.
type graphNodeRead struct {
	model.GraphNode
	TextTree model.DisplayTree `json:"text_tree,omitempty"`
	Pin      *blockPinRead     `json:"pin,omitempty"`
}

// shape rebuilds the shadowing pin from the node and registers both texts with the request's
// reader, each with the footnotes it calls that its document defines outside it where the node is
// a document's block (graphFootnotes, registerBlockTree).
func (n *graphNodeRead) shape(reader *displayTreeReader, footnotes graphFootnotes) {
	n.Pin = nil
	if pin := n.GraphNode.Pin; pin != nil {
		n.Pin = &blockPinRead{BlockPin: *pin}
		source := footnotes.pin
		source.markdown = pin.Text
		registerBlockTree(reader, &n.Pin.TextTree, source)
	}
	source := footnotes.latest[graphNodeText]
	source.markdown = n.GraphNode.Text
	registerBlockTree(reader, &n.TextTree, source)
}

// graphExcerptRead is the excerpt an edge shows, with the tree of its text. Every excerpt that
// has a text carries one, a name's as well as a body's head: the head of a block, of an ask, a
// comment or a message, or a document's mention at the block holding it, is drawn through the
// renderer from this tree, and an issue's title, a document's name and a component's title
// (refs.Edges) are drawn as the characters they are, so their tree is sent and never read. One
// rule for every excerpt keeps the answer's own promise that a text with a `_tree` beside it
// always has it, which the dashboard's recordings hold every answer to.
type graphExcerptRead struct {
	model.GraphExcerpt
	TextTree model.DisplayTree `json:"text_tree,omitempty"`
}

// graphEdgeRead is one edge of a reference-graph read as the dashboard reads it: the edge, with
// its excerpt carrying the tree of the excerpt's text. It declares model.GraphEdge's fields in
// the edge's own order rather than embedding the edge and shadowing its excerpt, which would
// write `excerpt` after `created_at` and `source_seq` for every caller (the read shapes above):
// so an answer without the trees header is the bytes model.GraphEdge writes, and one with it
// differs only by the `text_tree` inside each excerpt.
type graphEdgeRead struct {
	Kind      string            `json:"kind"`
	Direction string            `json:"direction"`
	Node      model.GraphNode   `json:"node"`
	Block     *model.GraphNode  `json:"block,omitempty"`
	Excerpt   *graphExcerptRead `json:"excerpt,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	SourceSeq *int64            `json:"source_seq"`
}

// graphEdgeReads is a read's edges shaped for the dashboard, an absent list staying absent, each
// excerpt's tree read where it stands for a caller that asked for the trees (shapeBuilt). An
// excerpt is a cut - the head of a block's or a node's text (refs.BlockExcerpt, refs.Edges) - or
// a name, which no writer stores a tree of: registered with the request's reader it would be
// asked of the table and stored there, one row for every distinct excerpt that no reader of a
// stored text ever takes. An excerpt of a document's block is read with the footnotes it calls
// that its document defines outside the block (graphFootnotes, registerBlockTree). Being a cut,
// such an excerpt can stop inside a block whose end the cut leaves out, a fenced code block among
// them, which then holds the stand-in definitions read after it, so its tree does not end in them
// and is set nowhere, with no warning, since its source is a cut (blockTreeSource): that excerpt
// is read again from its text alone, since every excerpt with a text carries a tree
// (graphExcerptRead).
func graphEdgeReads(edges []model.GraphEdge, trees bool, footnotes graphFootnotes) []graphEdgeRead {
	if edges == nil {
		return nil
	}
	reads := make([]graphEdgeRead, len(edges))
	shapeBuilt(trees, func(reader *displayTreeReader) {
		for index := range edges {
			edge, read := &edges[index], &reads[index]
			*read = graphEdgeRead{
				Kind: edge.Kind, Direction: edge.Direction, Node: edge.Node, Block: edge.Block,
				CreatedAt: edge.CreatedAt, SourceSeq: edge.SourceSeq,
			}
			if edge.Excerpt != nil {
				read.Excerpt = &graphExcerptRead{GraphExcerpt: *edge.Excerpt}
				source := footnotes.latest[index]
				source.markdown, source.cut = edge.Excerpt.Text, true
				registerBlockTree(reader, &read.Excerpt.TextTree, source)
			}
		}
	})
	if len(footnotes.latest) == 0 {
		return reads
	}
	shapeBuilt(trees, func(reader *displayTreeReader) {
		for index := range reads {
			if excerpt := reads[index].Excerpt; excerpt != nil && excerpt.TextTree == nil {
				reader.text(&excerpt.TextTree, excerpt.Text)
			}
		}
	})
	return reads
}

// graphReferencesRead is what GET /api/v1/references answers: the queried node and every edge
// shaped to carry their trees.
type graphReferencesRead struct {
	Node  graphNodeRead   `json:"node"`
	Edges []graphEdgeRead `json:"edges"`
	// UnknownKinds is model.GraphReferences' own member, kept here because this shape answers
	// that route now: the edge kinds this build does not know, which a newer build wrote while
	// two builds serve one database.
	UnknownKinds []string `json:"unknown_kinds,omitempty"`
}

// referenceLinkRead is one row of an issue's references read as the dashboard reads it: the row,
// with the tree of its name. It declares model.ReferenceLink's fields in the row's own order, as
// graphEdgeRead declares the edge's, and the tree after them, so an answer without the trees
// header is the bytes model.ReferenceLink writes, and one with it differs only by the `name_tree`
// that closes each row.
type referenceLinkRead struct {
	Node     model.GraphNode    `json:"node"`
	Name     string             `json:"name"`
	Kinds    []string           `json:"kinds"`
	Via      model.ReferenceVia `json:"via"`
	NameTree model.DisplayTree  `json:"name_tree,omitempty"`
}

// issueReferencesRead is what GET /api/v1/issues/{key}/references answers: model.IssueReferences'
// fields in its own order, each row of links and linked_from shaped to carry its name's tree.
type issueReferencesRead struct {
	Members    []model.ReferenceMember `json:"members"`
	Truncated  bool                    `json:"truncated"`
	Links      []referenceLinkRead     `json:"links"`
	LinkedFrom []referenceLinkRead     `json:"linked_from"`
}

// newIssueReferencesRead is an issue's references read shaped for the dashboard, each row's name
// read into its tree where it stands for a caller that asked for the trees (shapeBuilt). A name
// is the words the graph read shows for the far node (refs.Closure): a document's name, an
// issue's title, or the head of an item's text, a cut no writer stores a tree of. So it is read
// as graphEdgeReads reads an excerpt, and stored nowhere.
func newIssueReferencesRead(references model.IssueReferences, trees bool) issueReferencesRead {
	read := issueReferencesRead{Members: references.Members, Truncated: references.Truncated}
	shapeBuilt(trees, func(reader *displayTreeReader) {
		read.Links = referenceLinkReads(references.Links, reader)
		read.LinkedFrom = referenceLinkReads(references.LinkedFrom, reader)
	})
	return read
}

// referenceLinkReads is rows shaped to carry each name's tree, an absent list staying absent and
// an empty one empty. The list is made whole before any name is registered, since each tree field
// must stand where it will still stand when reader resolves.
func referenceLinkReads(rows []model.ReferenceLink, reader *displayTreeReader) []referenceLinkRead {
	if rows == nil {
		return nil
	}
	reads := make([]referenceLinkRead, len(rows))
	for index, row := range rows {
		reads[index] = referenceLinkRead{Node: row.Node, Name: row.Name, Kinds: row.Kinds, Via: row.Via}
		reader.text(&reads[index].NameTree, row.Name)
	}
	return reads
}

// architectureComponentRead is one component of the model with the tree of the prose its
// source's files declare it with.
type architectureComponentRead struct {
	model.ArchitectureTreeComponent
	ProseTree model.DisplayTree `json:"prose_tree,omitempty"`
}

// architectureTreeRead is what GET /api/v1/projects/{key}/architecture answers: the tree, with
// every component shaped to carry the tree of its prose and every Not architectural row the tree
// of its reason (architectureTreeNoneRead). The other side lists name issues and carry no text.
// It embeds model.ArchitectureTree, so the document still declares the model's own component and
// row beside the shaped ones, which a client of the model imports. The three shadows are
// declared in the order the route has always written them - the Not architectural rows, the
// retired links, then the components, which the first shaping already moved to the end - so an
// answer to a caller that asked for no trees keeps its bytes. RetiredLinks carries no text of its
// own: it is shadowed only to keep its place after the rows.
type architectureTreeRead struct {
	model.ArchitectureTree
	NotArchitectural []architectureTreeNoneRead      `json:"not_architectural"`
	RetiredLinks     []model.ArchitectureTreeRetired `json:"retired_links"`
	Components       []architectureComponentRead     `json:"components"`
}

// handoffRead is a handoff as the dashboard reads it: the handoff, with the tree of every text
// its card draws from the body.
type handoffRead struct {
	model.Handoff
	// FieldTrees is the tree of each string the card draws from the body, keyed by that string's
	// JSON Pointer into the body (RFC 6901): `/summary` for a top-level field, `/proof/0` for the
	// first entry of a `proof` list, a `~` or a `/` in a field's name written `~0` or `~1` - the
	// rows handoffFields splits the body into (web/src/features/conversation/conversation-model.ts).
	// So a field named `proof.0` (`/proof.0`) and the first proof entry (`/proof/0`) never share a
	// key. A value that is no string - a number, a boolean, null, an object, a list other than
	// proof, or an entry of proof that is no string - is drawn as the JSON it is and has none, as
	// an empty string has none. The root's pointer "" is the whole body, sent only when the body
	// is not a JSON object, which the card then draws whole; the API refuses such a body
	// (createHandoff), so one is a body written before that rule.
	FieldTrees map[string]model.DisplayTree `json:"field_trees,omitempty"`
}

// handoffProofField is the body field whose list the card splits into one row per entry.
const handoffProofField = "proof"

// handoffPointerToken writes a field's name as one reference token of a JSON Pointer (RFC 6901):
// each `~` as `~0` and each `/` as `~1`, in one pass, so the `~` an escape writes is never escaped
// again.
var handoffPointerToken = strings.NewReplacer("~", "~0", "/", "~1")

// shape registers each string of the body with reader, each filling its own key of FieldTrees.
// With no reader the body is not decoded at all: only a caller that asked for the trees pays for
// reading it.
func (h *handoffRead) shape(reader *displayTreeReader) {
	h.FieldTrees = nil
	if reader == nil {
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(h.Body), &fields); err != nil || fields == nil {
		h.registerText(reader, "", h.Body)
		return
	}
	for name, value := range fields {
		pointer := "/" + handoffPointerToken.Replace(name)
		if name == handoffProofField && len(value) > 0 && value[0] == '[' {
			var entries []json.RawMessage
			if json.Unmarshal(value, &entries) == nil {
				for index, entry := range entries {
					h.registerString(reader, pointer+"/"+strconv.Itoa(index), entry)
				}
			}
			continue
		}
		h.registerString(reader, pointer, value)
	}
}

// registerString registers the text value holds under pointer, when value is a JSON string.
func (h *handoffRead) registerString(reader *displayTreeReader, pointer string, value json.RawMessage) {
	if len(value) == 0 || value[0] != '"' {
		return
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		h.registerText(reader, pointer, text)
	}
}

// registerText registers text to fill FieldTrees under pointer. A map's values have no address,
// so the tree is handed over rather than pointed at (displayTreeReader.textInto).
func (h *handoffRead) registerText(reader *displayTreeReader, pointer, text string) {
	reader.textInto(func(tree json.RawMessage) {
		if h.FieldTrees == nil {
			h.FieldTrees = make(map[string]model.DisplayTree)
		}
		h.FieldTrees[pointer] = model.DisplayTree(tree)
	}, text)
}

// newAskAnswerWithTree is an answer a read gives with the trees of its text and of the labels it
// chose, nil where nobody answered. A search hit's ask and a related item's ask both read one. An
// ask read shapes its own answer through askAnswerWithTree instead and registers that text with
// the rest of the ask's (shapedAsk.shape), so the one walk names every text of an ask exactly
// once.
func newAskAnswerWithTree(answer *model.AskAnswer, reader *displayTreeReader) *model.AskAnswerWithTree {
	read := askAnswerWithTree(answer)
	if read != nil {
		reader.textOf(&read.TextTree, answer.Text)
		registerChosenLabels(reader, read)
	}
	return read
}

// askAnswerWithTree is that same shape with no tree registered, for a caller that registers the
// answer's texts itself.
func askAnswerWithTree(answer *model.AskAnswer) *model.AskAnswerWithTree {
	if answer == nil {
		return nil
	}
	return &model.AskAnswerWithTree{AskAnswer: *answer}
}

// registerChosenLabels registers each label answer chose with reader, each filling the entry of
// SelectedTrees at its own index, so the page draws a chosen option from the tree of exactly the
// label beside it and never parses one.
//
// The reader is the request's own, the one that copies a stored text's tree from the table and
// stores what the table did not hold, because a label is a stored text and no cut of one: it is
// the whole label of an option the ask held when it was answered (selectedOptions), and an
// option's label is a text the table keeps a tree of - stored by the API's write of the ask
// (texttrees.AskWrittenTexts, at its creation and at each edit) and by the background fill of
// every ask's row (texttreefill). So the read copies the bytes the option's own tree was read
// from, and a label of an option the ask has since renamed or removed, which a reader of an
// earlier answer has no other tree for, is read the same way. The label is no text of the walk
// (texttrees.VisitAskTexts): the walk names the option it is the label of.
//
// Every answer a read gives carries the list, the ask's own current answer too, so a reader of an
// answer is never handed a chosen label without its tree. With no reader the list stays nil and
// is left out, so a caller that asked for no trees reads the bytes it read before; it is made
// whole before any entry is registered, because each entry must stand where it will stand when
// the reader resolves.
func registerChosenLabels(reader *displayTreeReader, answer *model.AskAnswerWithTree) {
	if reader == nil || answer == nil || len(answer.Selected) == 0 {
		return
	}
	answer.SelectedTrees = make([]model.DisplayTree, len(answer.Selected))
	for index, label := range answer.Selected {
		reader.text(&answer.SelectedTrees[index], label)
	}
}
