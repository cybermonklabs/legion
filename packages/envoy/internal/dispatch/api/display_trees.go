package api

import (
	"encoding/json"
	"net/http"
	"runtime"
	"sync"

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
// stored the tree of. The shaping is the shaping every read uses, over a reader of its own that
// is resolved before the row is handed back (builtDisplayTrees).
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
// the row already held, so the bytes are the bytes the route wrote before this existed.

// askOptionRead is one answer choice with the trees of its label and description.
type askOptionRead struct {
	model.AskOption
	LabelTree       json.RawMessage `json:"label_tree,omitempty"`
	DescriptionTree json.RawMessage `json:"description_tree,omitempty"`
}

// askResolutionRead is why an ask no longer needs an answer, with the tree of that reason.
type askResolutionRead struct {
	model.AskResolution
	ReasonTree json.RawMessage `json:"reason_tree,omitempty"`
}

// askEditPreviousRead is the content an ask had before an edit, with its question's and its
// options' trees.
type askEditPreviousRead struct {
	model.AskEditPrevious
	QuestionTree json.RawMessage `json:"question_tree,omitempty"`
	Options      []askOptionRead `json:"options"`
}

// askEditRead is one recorded rewording of an ask, the content it replaced carrying its trees.
type askEditRead struct {
	model.AskEdit
	Previous askEditPreviousRead `json:"previous"`
}

// askRead is an ask as the dashboard reads it: the ask, with the trees of its question, of each
// option, of its answer's text and of its resolution's reason.
type askRead struct {
	model.Ask
	QuestionTree json.RawMessage          `json:"question_tree,omitempty"`
	Options      []askOptionRead          `json:"options"`
	Answer       *model.AskAnswerWithTree `json:"answer"`
	Resolution   *askResolutionRead       `json:"resolution,omitempty"`
}

// shape rebuilds the shadowing fields from the ask they stand in front of, and registers each of
// its texts with the request's reader. The texts are texttrees.VisitAskTexts, the one walk this
// read, the event path and the background fill of the trees all make, and treeField says where
// each of them lands here: a text the walk gains that this shape has no field for is registered
// nowhere, which is what the agreement test refuses (ask_texts_test.go).
func (a *askRead) shape(reader *displayTreeReader) {
	a.Options = askOptionReads(a.Ask.Options)
	a.Answer = askAnswerWithTree(a.Ask.Answer)
	a.Resolution = nil
	if a.Ask.Resolution != nil {
		a.Resolution = &askResolutionRead{AskResolution: *a.Ask.Resolution}
	}
	registerAskTexts(reader, texttrees.AskContentOf(a.Ask), a.treeField)
}

// treeField is the field of this read that carries the tree of text, and nil for a text this
// read shows nowhere.
func (a *askRead) treeField(text texttrees.AskText) *json.RawMessage {
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
	fieldOf func(texttrees.AskText) *json.RawMessage,
) {
	texttrees.VisitAskTexts(content, func(text texttrees.AskText) {
		if field := fieldOf(text); field != nil {
			reader.text(field, text.Text)
		}
	})
}

// askOptionTreeField is the field of reads that carries text's tree, and nil for a text that is
// no option's or whose option these reads do not hold.
func askOptionTreeField(reads []askOptionRead, text texttrees.AskText) *json.RawMessage {
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

// newAskRead is one ask shaped for a response of its own: a write's answer, which reads its
// texts where they stand (shapeBuilt). A read of several asks shapes them through askReads, over
// the request's own reader.
func newAskRead(ask model.Ask, trees bool) askRead {
	read := askRead{Ask: ask}
	shapeBuilt(trees, read.shape)
	return read
}

// askReads is a list of asks shaped for the dashboard, an absent list staying absent.
func askReads(asks []model.Ask, reader *displayTreeReader) []askRead {
	if asks == nil {
		return nil
	}
	reads := make([]askRead, len(asks))
	for index := range asks {
		reads[index].Ask = asks[index]
		reads[index].shape(reader)
	}
	return reads
}

// askAnswerReads is a thread's answers with the trees of their texts.
func askAnswerReads(answers []model.AskAnswer, reader *displayTreeReader) []model.AskAnswerWithTree {
	if answers == nil {
		return nil
	}
	reads := make([]model.AskAnswerWithTree, len(answers))
	for index := range answers {
		reads[index] = model.AskAnswerWithTree{AskAnswer: answers[index]}
		reader.textOf(&reads[index].TextTree, answers[index].Text)
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
func (p *askEditPreviousRead) treeField(text texttrees.AskText) *json.RawMessage {
	if text.Field == texttrees.AskQuestionText {
		return &p.QuestionTree
	}
	return askOptionTreeField(p.Options, text)
}

// replyBlockRead is the block a reply answers, with the tree of the excerpt it stored.
type replyBlockRead struct {
	model.ReplyBlock
	TextTree json.RawMessage `json:"text_tree,omitempty"`
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

// commentRead is a comment as the dashboard reads it: the comment, the tree of its body, and the
// block it answers carrying the tree of that block's excerpt.
type commentRead struct {
	model.Comment
	BodyTree   json.RawMessage `json:"body_tree,omitempty"`
	ReplyBlock *replyBlockRead `json:"reply_block"`
}

// shape rebuilds the shadowing fields from the comment and registers its texts with the
// request's reader.
func (c *commentRead) shape(reader *displayTreeReader) {
	c.ReplyBlock = newReplyBlockRead(c.Comment.ReplyBlock, reader)
	reader.text(&c.BodyTree, c.Comment.Body)
}

// newCommentRead is one comment shaped for a response of its own, its texts read where they
// stand (shapeBuilt).
func newCommentRead(comment model.Comment, trees bool) commentRead {
	read := commentRead{Comment: comment}
	shapeBuilt(trees, read.shape)
	return read
}

// commentReads is a list of comments shaped for the dashboard, an absent list staying absent.
func commentReads(comments []model.Comment, reader *displayTreeReader) []commentRead {
	if comments == nil {
		return nil
	}
	reads := make([]commentRead, len(comments))
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
	BodyTree   json.RawMessage `json:"body_tree,omitempty"`
	ReplyBlock *replyBlockRead `json:"reply_block"`
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
	ReasonTree json.RawMessage `json:"reason_tree,omitempty"`
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
	ReasonTree json.RawMessage `json:"reason_tree,omitempty"`
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
	TextTree json.RawMessage `json:"text_tree,omitempty"`
}

// graphNodeRead is one end of a reference-graph edge as the dashboard reads it: the node, the
// tree of its own text, and the pin carrying the tree of the text it recorded. A reference's
// title is read from one of those trees, as an ask's and a comment's titles are read from theirs.
type graphNodeRead struct {
	model.GraphNode
	TextTree json.RawMessage `json:"text_tree,omitempty"`
	Pin      *blockPinRead   `json:"pin,omitempty"`
}

// shape rebuilds the shadowing pin from the node and registers both texts with the request's
// reader.
func (n *graphNodeRead) shape(reader *displayTreeReader) {
	n.Pin = nil
	if pin := n.GraphNode.Pin; pin != nil {
		n.Pin = &blockPinRead{BlockPin: *pin}
		reader.text(&n.Pin.TextTree, pin.Text)
	}
	reader.text(&n.TextTree, n.GraphNode.Text)
}

// graphReferencesRead is what GET /api/v1/references answers: the queried node shaped to carry
// its trees, and the edges, which carry no stored text of their own.
type graphReferencesRead struct {
	Node  graphNodeRead     `json:"node"`
	Edges []model.GraphEdge `json:"edges"`
}

// architectureComponentRead is one component of the model with the tree of the prose its
// source's files declare it with.
type architectureComponentRead struct {
	model.ArchitectureTreeComponent
	ProseTree json.RawMessage `json:"prose_tree,omitempty"`
}

// architectureTreeRead is what GET /api/v1/projects/{key}/architecture answers: the tree, with
// every component shaped to carry that tree. The side lists name issues and carry no prose.
type architectureTreeRead struct {
	model.ArchitectureTree
	Components []architectureComponentRead `json:"components"`
}

// newAskAnswerWithTree is an answer a read gives with the tree of its text, nil where nobody
// answered. A search hit's ask and a person's answer history both read one. An ask read shapes
// its own answer through askAnswerWithTree instead and registers that text with the rest of the
// ask's (askRead.shape), so the one walk names every text of an ask exactly once.
func newAskAnswerWithTree(answer *model.AskAnswer, reader *displayTreeReader) *model.AskAnswerWithTree {
	read := askAnswerWithTree(answer)
	if read != nil {
		reader.textOf(&read.TextTree, answer.Text)
	}
	return read
}

// askAnswerWithTree is that same shape with no tree registered, for a caller that registers the
// answer's text itself.
func askAnswerWithTree(answer *model.AskAnswer) *model.AskAnswerWithTree {
	if answer == nil {
		return nil
	}
	return &model.AskAnswerWithTree{AskAnswer: *answer}
}
