package listenerwire

import (
	"encoding/json"
	"fmt"
)

// ExpectsReply is the envelope's expects_reply field: what the sender wants back. On the wire it
// is the JSON literal false, "optional" or "required"; absent from a message that says nothing
// about a reply. This type is hand-written beside the generated mirror in envelope_generated.go,
// whose Envelope holds the field as this type and whose Validate() calls the Validate below, so
// one vocabulary is decided here and nowhere else. A constant's Go string is this package's own
// name for the value, not a word the wire carries: MarshalJSON writes the wire form and
// ParseExpectsReply reads every form accepted, the retired one named below among them.
type ExpectsReply string

const (
	// ExpectsReplyUnset is the absent field. It is never marshalled: every struct field holding
	// one carries omitempty, which encoding/json decides from the string's length before it
	// reaches MarshalJSON, so the empty value is dropped rather than written.
	ExpectsReplyUnset ExpectsReply = ""
	// ExpectsReplyNone wants no reply. It marshals to the JSON literal false.
	ExpectsReplyNone ExpectsReply = "no-reply"
	// ExpectsReplyOptional invites a reply without waiting on one.
	ExpectsReplyOptional ExpectsReply = "optional"
	// ExpectsReplyRequired waits on a reply.
	ExpectsReplyRequired ExpectsReply = "required"
)

// ExpectsReplyVocabulary is the wire vocabulary every refusal names, the literal false among the
// two words, so a sender reads what to write rather than the enum the field used to be. The
// listener's own 400 for this field composes its text from it (cmd/listener/api.go), so one edit
// moves both.
const ExpectsReplyVocabulary = "false, optional, required"

// expectsReplyLegacyNone is the retired wire word for wanting no reply, which the field carried
// before the JSON literal false replaced it and which ParseExpectsAsSpoken still reads as
// ExpectsReplyNone. Only an answer to a request that wrote it writes it, back to that request
// (ExpectsAsSpoken.LegacyNone): retiring the leniency is deleting this constant, the one case of
// ParseExpectsAsSpoken that names it, and LegacyNone.
const expectsReplyLegacyNone = "none"

// MarshalJSON writes the no-reply value as the JSON literal false and the other two as their
// quoted words.
func (e ExpectsReply) MarshalJSON() ([]byte, error) {
	switch e {
	case ExpectsReplyNone:
		return []byte("false"), nil
	case ExpectsReplyOptional:
		return []byte(`"optional"`), nil
	case ExpectsReplyRequired:
		return []byte(`"required"`), nil
	case ExpectsReplyUnset:
		return nil, fmt.Errorf("expects_reply is absent and must be omitted, not written")
	default:
		return nil, expectsReplyRefusal()
	}
}

// UnmarshalJSON reads what ParseExpectsReply accepts.
func (e *ExpectsReply) UnmarshalJSON(raw []byte) error {
	parsed, err := ParseExpectsReply(raw)
	if err != nil {
		return err
	}
	*e = parsed
	return nil
}

// ParseExpectsReply reads one expects_reply field as it arrived: the JSON literal false, the
// strings "optional" and "required", and - leniently - the old string "none", which means the
// same as false. A field that is absent, as a nil raw or as JSON null, is the unset value, since
// saying nothing about a reply is not an error. Anything else is refused by naming the vocabulary.
// It is the value ParseExpectsAsSpoken reads, which also keeps the spelling.
//
// The "none" case is the one piece of leniency here, and what removes it is a count, not a date:
// every producer and every skill line of this repository now writes false, so the case goes once
// no deployment pins a plugin build old enough to write "none".
func ParseExpectsReply(raw json.RawMessage) (ExpectsReply, error) {
	spoken, err := ParseExpectsAsSpoken(raw)
	return spoken.Value, err
}

// ExpectsAsSpoken is a request's expects_reply as the request spoke it: its value, and whether it
// said no reply with the retired "none" rather than the literal false. It is what the listener's
// answer to a send or a publish writes (SendResponse, KeptSendResponse and PublishResponse), so
// each sender reads back the dialect it spoke - the literal false, "optional", "required" or
// "none" - and the zero value, which those answers leave out, is a request that said nothing
// about a reply or wrote null. The listener reads it from the request (ParseExpectsAsSpoken),
// refusing anything else before an answer is built, and the envelope it puts on the bus carries
// Value alone, in the one new vocabulary. An answer writes the request's word, not its bytes: a
// word the request wrote with escapes comes back without them.
//
// Answering the request's own spelling is what keeps the ingress leniency's promise whole. A
// plugin build old enough to write "none" holds an EnvelopeSchema whose expects_reply is the
// three-word enum (packages/contracts/src/envelope.ts), and it parses this very answer
// (packages/envoy-client/src/transport.ts): a literal false there fails that parse, so its send
// is reported unconfirmed and re-sent, and its publish throws. LegacyNone goes with the leniency
// itself, on the count ParseExpectsReply's comment names.
type ExpectsAsSpoken struct {
	// Value is what the request asked for, which the envelope carries.
	Value ExpectsReply
	// LegacyNone is set beside ExpectsReplyNone alone, for a request that wrote "none".
	LegacyNone bool
}

// MarshalJSON writes Value as ExpectsReply writes it, and "none" back to a request that wrote it.
func (s ExpectsAsSpoken) MarshalJSON() ([]byte, error) {
	if !s.LegacyNone {
		return s.Value.MarshalJSON()
	}
	if s.Value != ExpectsReplyNone {
		return nil, expectsReplyRefusal()
	}
	return []byte(`"` + expectsReplyLegacyNone + `"`), nil
}

// UnmarshalJSON reads what ParseExpectsAsSpoken accepts.
func (s *ExpectsAsSpoken) UnmarshalJSON(raw []byte) error {
	parsed, err := ParseExpectsAsSpoken(raw)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// ParseExpectsAsSpoken reads one expects_reply field as ParseExpectsReply says, and keeps whether
// it wrote no reply as the retired "none" (LegacyNone).
func ParseExpectsAsSpoken(raw json.RawMessage) (ExpectsAsSpoken, error) {
	if len(raw) == 0 {
		return ExpectsAsSpoken{}, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ExpectsAsSpoken{}, expectsReplyRefusal()
	}
	switch read := value.(type) {
	case nil:
		return ExpectsAsSpoken{}, nil
	case bool:
		if !read {
			return ExpectsAsSpoken{Value: ExpectsReplyNone}, nil
		}
	case string:
		if read == expectsReplyLegacyNone {
			return ExpectsAsSpoken{Value: ExpectsReplyNone, LegacyNone: true}, nil
		}
		switch ExpectsReply(read) {
		case ExpectsReplyOptional, ExpectsReplyRequired:
			return ExpectsAsSpoken{Value: ExpectsReply(read)}, nil
		}
	}
	return ExpectsAsSpoken{}, expectsReplyRefusal()
}

// Validate refuses a value no producer may write. envelope_generated.go's Envelope.Validate calls
// it for expects_reply, the one property whose own type the generator defers to (valueChecks in
// packages/contracts/scripts/gen-go.ts), in place of a switch over words.
func (e ExpectsReply) Validate() error {
	switch e {
	case ExpectsReplyUnset, ExpectsReplyNone, ExpectsReplyOptional, ExpectsReplyRequired:
		return nil
	}
	return expectsReplyRefusal()
}

func expectsReplyRefusal() error {
	return fmt.Errorf("expects_reply must be one of: %s", ExpectsReplyVocabulary)
}
