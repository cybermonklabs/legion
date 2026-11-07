package api

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"log/slog"
	"strings"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/texttrees"
)

// eventPayloadPrefix opens every key of an event's trees map: the path of the text the tree was
// read from, rooted at the envelope, so a reader finds a tree by the field it belongs to
// ("payload.body", "payload.options.0.label").
const eventPayloadPrefix = "payload"

// attachEventTrees registers the trees of every listed event with the request's reader, which
// fills them when it resolves. The REST event lists carry them only under the header, so a
// caller that asked for none has no reader and nothing is read; a /ws/live frame always carries
// them, since a browser can set no header on a socket (liveEventFrame).
//
// Each payload is read for its texts once. The texts, their sums, the one query of their stored
// trees and the reading of what that query did not answer all belong to the reader, so nothing
// here walks a payload a second time and no text is hashed twice.
func attachEventTrees(events []model.Event, reader *displayTreeReader) {
	if reader == nil {
		return
	}
	for index := range events {
		event := &events[index]
		eventPayloadTexts(events[index], func(path, text string) {
			reader.textInto(func(tree json.RawMessage) {
				if event.Trees == nil {
					event.Trees = make(map[string]json.RawMessage, 1)
				}
				event.Trees[path] = tree
			}, text)
		})
	}
}

// eventDisplayTrees is the display tree of every stored text one event's payload carries, keyed
// by that text's path: the body a comment or a message event carries, and an ask event's
// question, options, answer text and resolution reason, the question and options an edit
// replaced among them. The texts themselves are found by eventPayloadTexts, which the publish
// path reads the same payload with to store those trees (storeEventDisplayTrees).
//
// It reads each text where it stands rather than from the table: its one caller is a live
// frame, which is no request and has no reader of its own, and whose event the publish path is
// storing the trees of at that moment.
//
// The map is made on the first text there is a tree for, so an event with no text, and one whose
// every text failed to encode, answers nil and costs no map.
func eventDisplayTrees(event model.Event) map[string]json.RawMessage {
	reader := builtDisplayTrees(true)
	events := []model.Event{event}
	attachEventTrees(events, reader)
	reader.resolve()
	return events[0].Trees
}

// eventPayloadTexts calls visit for every stored text the event's payload carries, under that
// text's path rooted at the envelope: the body a comment or a message event carries, and an ask
// event's question, options, answer text and resolution reason, the question and options an edit
// replaced among them. A payload is read as the JSON it is rather than as its Go type, because a
// read serves it as the database stored it (readEvents) and a publish as the type the write
// built, and both have to answer the same keys.
//
// The event's type says which fields can hold a text, and only those are read: the whole payload
// of a feed's event is several times the size of its body, and a decode of all of it -
// generically, as a map of raw members - was the warm read's largest cost. An event of a type
// that can hold no stored text is not read at all. A field the payload does not hold, or holds as
// null or as anything but a string, is read as the empty string, which is no text: it is never
// visited.
func eventPayloadTexts(event model.Event, visit func(path, text string)) {
	ask := strings.HasPrefix(event.Type, "ask.")
	if !ask && !eventTypeHoldsBody(event.Type) {
		return
	}
	payload := eventPayloadJSON(event)
	if payload == nil {
		return
	}
	if ask {
		askEventPayloadTexts(payload, visit)
		return
	}
	visitText(visit, eventPayloadPrefix, "body", eventBodyText(payload))
}

// eventTypeHoldsBody reports whether an event of this type can carry a stored body. The comment
// and suggestion events carry the comment itself (model.CommentEventPayload) and the message
// events the message (model.MessageEventPayload); those are the only payloads Dispatch appends
// that hold a markdown body, so an event of any other type - an issue update, a child's status,
// a document version - carries no stored text and no tree.
func eventTypeHoldsBody(eventType string) bool {
	return strings.HasPrefix(eventType, "comment.") ||
		strings.HasPrefix(eventType, "message.") ||
		strings.HasPrefix(eventType, "suggestion.")
}

// askEventTexts is every stored text an ask event's payload can hold: the ask's own
// (model.Ask), the content an edit replaced (`previous`), the answer an edit or a second answer
// superseded (`previous_answer`), and a body, which no ask payload carries today and which is
// read here so that one that gains it answers the same key every other event's body answers.
//
// The payload is decoded into the model types a write publishes rather than into mirrors of
// them, so that the one walk of an ask's texts (texttrees.VisitAskTexts) answers here exactly
// what it answers a read shape and the background fill: a text a mirror had not been given
// would be a key this path never answered and a tree no reader of the feed takes.
type askEventTexts struct {
	model.Ask
	Body           string                 `json:"body"`
	Previous       *model.AskEditPrevious `json:"previous"`
	PreviousAnswer *model.AskAnswer       `json:"previous_answer"`
}

// askEventPayloadTexts visits the stored texts of an ask event: the ask's own, the ones an edit
// replaced, and the superseded answer's. An ask event is a handful of a feed's events where a
// comment is hundreds of them, and its texts stand in five places, so its payload is decoded in
// one pass into the fields that can hold one rather than scanned for each.
//
// A payload no text can be read from - one that is not a JSON object, which no payload Dispatch
// appends is - decodes to no texts, and a field of an unexpected type is left as it stood, which
// for a text is the empty string and is visited for nothing: a malformed field costs its own
// tree and no other's. That is what the decode's error says, and why it is not read.
func askEventPayloadTexts(raw json.RawMessage, visit func(path, text string)) {
	var payload askEventTexts
	_ = json.Unmarshal(raw, &payload)
	visitText(visit, eventPayloadPrefix, "body", payload.Body)
	visitAskContentTexts(visit, eventPayloadPrefix, texttrees.AskContentOf(payload.Ask))
	if payload.Previous != nil {
		visitAskContentTexts(
			visit, eventPayloadPrefix+".previous", texttrees.AskEditPreviousContent(*payload.Previous),
		)
	}
	if payload.PreviousAnswer != nil && payload.PreviousAnswer.Text != nil {
		visitText(visit, eventPayloadPrefix+".previous_answer", "text", *payload.PreviousAnswer.Text)
	}
}

// visitAskContentTexts visits the texts of one ask-shaped object standing at path, each under
// the path its field answers by (texttrees.AskText.Path).
func visitAskContentTexts(visit func(path, text string), path string, content texttrees.AskContent) {
	texttrees.VisitAskTexts(content, func(text texttrees.AskText) { visit(text.Path(path), text.Text) })
}

// visitText visits text under "<path>.<name>", where there is something in it. A field the payload
// does not hold, or holds as null or as anything but a string, decoded to the empty string, which
// has no tree to read or store.
func visitText(visit func(path, text string), path, name, text string) {
	if text == "" {
		return
	}
	visit(path+"."+name, text)
}

// eventPayloadJSON is the event's payload as JSON, whatever it is held as: the JSON the database
// returned, or the typed value a write published, which is encoded here. It answers nil where
// that encoding failed, which leaves the event without trees.
func eventPayloadJSON(event model.Event) json.RawMessage {
	if raw, held := event.Payload.(json.RawMessage); held {
		return raw
	}
	encoded, err := json.Marshal(event.Payload)
	if err != nil {
		slog.Warn("dispatch: event payload encoding failed", "type", event.Type, "error", err)
		return nil
	}
	return encoded
}

// quotedBodyName is the member a comment's or a message's text stands in, as the payload writes
// it: the scan below compares a member's name against these bytes, which costs it no string of
// its own. Neither Postgres's jsonb nor Go's encoder writes an escape in a name of ASCII
// letters, so the two are the same bytes wherever the member is there at all.
const quotedBodyName = `"body"`

// eventBodyText is the text the payload's body member holds, and the empty string where it holds
// none. It reads the payload only as far as that member and no further: an event's payload is
// several times its body in bytes, and Postgres orders a jsonb object's members by the length of
// their names and then by their bytes, so `body` stands among the first of a comment's or a
// message's and the rest of them are never parsed.
func eventBodyText(raw json.RawMessage) string {
	scanner, held := eventPayloadScanners.Get().(*eventPayloadScanner)
	if !held {
		scanner = &eventPayloadScanner{}
		scanner.decoder = jsontext.NewDecoder(&scanner.reader)
	}
	defer eventPayloadScanners.Put(scanner)
	scanner.reader.Reset(raw)
	scanner.decoder.Reset(&scanner.reader)
	// A payload that is not a JSON object - no payload Dispatch appends is anything else - holds
	// no member at all, and the opening brace is read past so the members follow.
	if scanner.decoder.PeekKind() != '{' {
		return ""
	}
	if _, err := scanner.decoder.ReadToken(); err != nil {
		return ""
	}
	for scanner.decoder.PeekKind() == '"' {
		name, err := scanner.decoder.ReadValue()
		if err != nil {
			return ""
		}
		if string(name) != quotedBodyName {
			if err := scanner.decoder.SkipValue(); err != nil {
				return ""
			}
			continue
		}
		value, err := scanner.decoder.ReadValue()
		if err != nil {
			return ""
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return ""
		}
		return text
	}
	return ""
}

// eventPayloadScanner reads one payload's members in the order they stand, far enough to answer
// for the one wanted. The decoder and the reader under it are kept between events because a feed
// reads one for every event it answers, and each would otherwise allocate a buffer the size of
// the payload it read.
type eventPayloadScanner struct {
	reader  bytes.Reader
	decoder *jsontext.Decoder
}

// eventPayloadScanners holds those scanners between reads.
var eventPayloadScanners sync.Pool
