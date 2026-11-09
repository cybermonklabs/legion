package texttrees

import (
	"strconv"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// AskTextField names the field one of an ask's stored texts stands in. A caller that needs only
// the text ignores it (a write's store, the background fill), one that answers a text by its path
// builds that path from it (AskText.Path, api's event trees), and one that holds a tree field for
// each text finds the field from it (api's shapedAsk.shape): a walk that answered a path alone
// would leave that last caller matching strings.
type AskTextField uint8

const (
	// AskQuestionText is the ask's question.
	AskQuestionText AskTextField = iota
	// AskOptionLabelText is the label of the option AskText.Option names.
	AskOptionLabelText
	// AskOptionDescriptionText is the description of that same option.
	AskOptionDescriptionText
	// AskAnswerText is the text a human wrote with their answer.
	AskAnswerText
	// AskResolutionReasonText is why the ask no longer needs an answer.
	AskResolutionReasonText
)

// AskText is one stored text of an ask-shaped object, as VisitAskTexts answers it.
type AskText struct {
	Field AskTextField
	// Option is the index of the option Field stands in, and -1 for every field that is no
	// option's.
	Option int
	Text   string
}

// Path is where this text stands under prefix, as an event's trees map is keyed and as the read
// shapes name their fields - "payload.question", "payload.options.0.label", "payload.answer.text"
// - so a dashboard that reads a payload's tree by its field's path finds it.
func (t AskText) Path(prefix string) string {
	switch t.Field {
	case AskOptionLabelText:
		return prefix + ".options." + strconv.Itoa(t.Option) + ".label"
	case AskOptionDescriptionText:
		return prefix + ".options." + strconv.Itoa(t.Option) + ".description"
	case AskAnswerText:
		return prefix + ".answer.text"
	case AskResolutionReasonText:
		return prefix + ".resolution.reason"
	default:
		return prefix + ".question"
	}
}

// AskContent is the stored content of one ask-shaped object: an ask itself (AskContentOf), or the
// question and options an edit carried away (AskEditPreviousContent), which holds no answer and no
// resolution. It is this package's own shape rather than model.Ask so that the three places an ask
// is held - the row, the event payload and the fill's three jsonb columns - all walk the one way.
type AskContent struct {
	Question   string
	Options    []model.AskOption
	Answer     *model.AskAnswer
	Resolution *model.AskResolution
}

// AskContentOf is everything a reader of ask is shown: its question, its options, the answer it
// carries and the reason it was resolved with.
func AskContentOf(ask model.Ask) AskContent {
	return AskContent{
		Question:   ask.Question,
		Options:    ask.Options,
		Answer:     ask.Answer,
		Resolution: ask.Resolution,
	}
}

// AskEditPreviousContent is the content an edit replaced: the question and the options as they
// stood, which the ask's row no longer holds and every read of its history is shown.
func AskEditPreviousContent(previous model.AskEditPrevious) AskContent {
	return AskContent{Question: previous.Question, Options: previous.Options}
}

// VisitAskTexts calls visit for every stored text content holds, in the order a reader meets them:
// the question, each option's label and description, the answer's text and the resolution's
// reason. It is the one list of an ask's stored texts, which every path that touches one walks:
// the write that stores their trees (AskWrittenTexts, api's asks), the read that registers them
// with its request's reader (api's shapedAsk.shape and askEditReads), the event path that answers
// them by path (api's event trees) and the background fill that stores the trees of an ask's row
// (texttreefill). A text named by one of them and not another would be a tree re-read on every
// request, or one stored for no reader.
//
// An empty text is never visited: it has no tree and no stored row (pmdoc.DisplayJSON), so an
// option with no description, an answer whose author wrote no text and a field a payload holds as
// null are all simply absent.
func VisitAskTexts(content AskContent, visit func(AskText)) {
	visitAskText(visit, AskText{Field: AskQuestionText, Option: -1, Text: content.Question})
	for index, option := range content.Options {
		visitAskText(visit, AskText{Field: AskOptionLabelText, Option: index, Text: option.Label})
		visitAskText(visit, AskText{Field: AskOptionDescriptionText, Option: index, Text: option.Description})
	}
	if content.Answer != nil && content.Answer.Text != nil {
		visitAskText(visit, AskText{Field: AskAnswerText, Option: -1, Text: *content.Answer.Text})
	}
	if content.Resolution != nil {
		visitAskText(visit, AskText{Field: AskResolutionReasonText, Option: -1, Text: content.Resolution.Reason})
	}
}

// visitAskText visits one text where there is something in it.
func visitAskText(visit func(AskText), text AskText) {
	if text.Text == "" {
		return
	}
	visit(text)
}

// AppendAskTexts appends every stored text of content to texts, for a caller that needs the texts
// and not where they stand: the background fill, which stores the tree of each, and the writes
// below.
func AppendAskTexts(texts []string, content AskContent) []string {
	VisitAskTexts(content, func(text AskText) { texts = append(texts, text.Text) })
	return texts
}

// AskWrittenTexts appends to texts every stored text a write of an ask writes: its question, and
// the label and description of each of its answer options. The ask's answer and the reason it was
// resolved with are not among them - they are written by the transitions, which store their own
// (api's closeAsk) - and a rewording's replaced question and labels are this same list over what
// the edit carried away (api's askEditReads).
func AskWrittenTexts(texts []string, question string, options []model.AskOption) []string {
	return AppendAskTexts(texts, AskContent{Question: question, Options: options})
}
