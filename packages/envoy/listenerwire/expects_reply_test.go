package listenerwire

import (
	"encoding/json"
	"strings"
	"testing"
)

// expectsReplyHolder is a reader of the field as a message carries it: the tag the generated
// envelope gives expects_reply, over the new type.
type expectsReplyHolder struct {
	ExpectsReply ExpectsReply `json:"expects_reply,omitempty"`
}

func TestParseExpectsReplyReadsEveryAcceptedWireValue(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want ExpectsReply
	}{
		{name: "false", raw: `false`, want: ExpectsReplyNone},
		{name: "optional", raw: `"optional"`, want: ExpectsReplyOptional},
		{name: "required", raw: `"required"`, want: ExpectsReplyRequired},
		{name: "the old none", raw: `"none"`, want: ExpectsReplyNone},
		{name: "null", raw: `null`, want: ExpectsReplyUnset},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseExpectsReply(json.RawMessage(test.raw))
			if err != nil {
				t.Fatalf("ParseExpectsReply(%s) = %v", test.raw, err)
			}
			if got != test.want {
				t.Errorf("ParseExpectsReply(%s) = %q, want %q", test.raw, got, test.want)
			}

			var holder expectsReplyHolder
			if err := json.Unmarshal([]byte(`{"expects_reply":`+test.raw+`}`), &holder); err != nil {
				t.Fatalf("unmarshal {\"expects_reply\":%s}: %v", test.raw, err)
			}
			if holder.ExpectsReply != test.want {
				t.Errorf("unmarshalled expects_reply = %q, want %q", holder.ExpectsReply, test.want)
			}
		})
	}
}

func TestParseExpectsReplyRefusesAnythingElseByNamingTheVocabulary(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "true", raw: `true`},
		{name: "another word", raw: `"later"`},
		// The Go constant's value is this package's own name for wanting no reply, never a word
		// the wire carries, so a sender writing it is refused like any other unknown token.
		{name: "the Go name for no reply", raw: `"no-reply"`},
		{name: "the empty string", raw: `""`},
		{name: "a number", raw: `2`},
		{name: "an object", raw: `{"expects":"reply"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseExpectsReply(json.RawMessage(test.raw))
			if err == nil {
				t.Fatalf("ParseExpectsReply(%s) = %q, want a refusal", test.raw, got)
			}
			if !strings.Contains(err.Error(), "false, optional, required") {
				t.Errorf("refusal = %q, want it to name false, optional, required", err)
			}

			var holder expectsReplyHolder
			unmarshalErr := json.Unmarshal([]byte(`{"expects_reply":`+test.raw+`}`), &holder)
			if unmarshalErr == nil {
				t.Fatalf("unmarshal {\"expects_reply\":%s} = %q, want a refusal", test.raw, holder.ExpectsReply)
			}
			if !strings.Contains(unmarshalErr.Error(), "false, optional, required") {
				t.Errorf("unmarshal refusal = %q, want it to name false, optional, required", unmarshalErr)
			}
		})
	}
}

func TestExpectsReplyMarshalsNoReplyAsTheLiteralFalse(t *testing.T) {
	for _, test := range []struct {
		value ExpectsReply
		want  string
	}{
		{value: ExpectsReplyNone, want: `{"expects_reply":false}`},
		{value: ExpectsReplyOptional, want: `{"expects_reply":"optional"}`},
		{value: ExpectsReplyRequired, want: `{"expects_reply":"required"}`},
		// The absent value is dropped by the field's omitempty, which encoding/json decides
		// from the string's length before MarshalJSON runs, so its refusal is unreachable
		// through a tagged field.
		{value: ExpectsReplyUnset, want: `{}`},
	} {
		t.Run(string(test.value), func(t *testing.T) {
			encoded, err := json.Marshal(expectsReplyHolder{ExpectsReply: test.value})
			if err != nil {
				t.Fatalf("marshal %q: %v", test.value, err)
			}
			if string(encoded) != test.want {
				t.Errorf("marshal %q = %s, want %s", test.value, encoded, test.want)
			}
		})
	}
}

func TestExpectsReplyValidateRefusesAWordNoProducerMayWrite(t *testing.T) {
	for _, value := range []ExpectsReply{ExpectsReplyUnset, ExpectsReplyNone, ExpectsReplyOptional, ExpectsReplyRequired} {
		if err := value.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want it accepted", value, err)
		}
	}
	err := ExpectsReply("later").Validate()
	if err == nil || !strings.Contains(err.Error(), "false, optional, required") {
		t.Errorf("Validate(\"later\") = %v, want a refusal naming false, optional, required", err)
	}
}

// answerExpectsReply is one answer type that carries the request's expects_reply back
// (ExpectsAsSpoken), read from an answer's JSON and as encoding/json reads it.
type answerExpectsReply struct {
	name   string
	decode func(body []byte) (spoken ExpectsAsSpoken, envelope ExpectsReply, err error)
	encode func(spoken ExpectsAsSpoken) ([]byte, error)
}

// answersCarryingExpectsReply are the three answers whose expects_reply is the request's own:
// each shadows the embedded envelope's field, so the answer's key fills the answer's member and
// leaves the envelope's unset.
var answersCarryingExpectsReply = []answerExpectsReply{
	{
		name: "SendResponse",
		decode: func(body []byte) (ExpectsAsSpoken, ExpectsReply, error) {
			var answer SendResponse
			err := json.Unmarshal(body, &answer)
			return answer.ExpectsReply, answer.Envelope.ExpectsReply, err
		},
		encode: func(spoken ExpectsAsSpoken) ([]byte, error) {
			return json.Marshal(SendResponse{ExpectsReply: spoken})
		},
	},
	{
		name: "KeptSendResponse",
		decode: func(body []byte) (ExpectsAsSpoken, ExpectsReply, error) {
			var answer KeptSendResponse
			err := json.Unmarshal(body, &answer)
			return answer.ExpectsReply, answer.Envelope.ExpectsReply, err
		},
		encode: func(spoken ExpectsAsSpoken) ([]byte, error) {
			return json.Marshal(KeptSendResponse{ExpectsReply: spoken})
		},
	},
	{
		name: "PublishResponse",
		decode: func(body []byte) (ExpectsAsSpoken, ExpectsReply, error) {
			var answer PublishResponse
			err := json.Unmarshal(body, &answer)
			return answer.ExpectsReply, answer.Envelope.ExpectsReply, err
		},
		encode: func(spoken ExpectsAsSpoken) ([]byte, error) {
			return json.Marshal(PublishResponse{ExpectsReply: spoken})
		},
	},
}

// expectsReplyKey is the expects_reply an encoded answer carries, as it is written there, and
// the empty string for an answer with no such key.
func expectsReplyKey(t *testing.T, encoded []byte) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("read the keys of %s: %v", encoded, err)
	}
	return string(fields["expects_reply"])
}

// An answer to a send or a publish carries the request's expects_reply back in the spelling the
// request wrote (the listener's validateMessageBody), and a reader of the answer reads the value
// it names and that spelling: the literal false, "optional", "required", the retired "none" - also
// escaped, as a word and not as bytes - and nothing at all for a request that said nothing or
// wrote null. Writing the read answer again gives the request's word back, so the listener, which
// writes these types, answers an old plugin's "none" with "none" and every other sender as
// ExpectsReply writes it.
func TestAnAnswerReadsAndWritesExpectsReplyAsTheRequestSpokeIt(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		want      ExpectsAsSpoken
		// written is the expects_reply the answer writes back, empty for no key.
		written string
	}{
		{name: "absent", raw: ``, want: ExpectsAsSpoken{}, written: ``},
		{name: "null", raw: `null`, want: ExpectsAsSpoken{}, written: ``},
		{name: "false", raw: `false`, want: ExpectsAsSpoken{Value: ExpectsReplyNone}, written: `false`},
		{name: "optional", raw: `"optional"`, want: ExpectsAsSpoken{Value: ExpectsReplyOptional}, written: `"optional"`},
		{name: "required", raw: `"required"`, want: ExpectsAsSpoken{Value: ExpectsReplyRequired}, written: `"required"`},
		{name: "the old none", raw: `"none"`, want: ExpectsAsSpoken{Value: ExpectsReplyNone, LegacyNone: true}, written: `"none"`},
		{name: "the old none escaped", raw: `"\u006eone"`, want: ExpectsAsSpoken{Value: ExpectsReplyNone, LegacyNone: true}, written: `"none"`},
	} {
		body := `{"event_id":"evt-1","recipient":"ses_target"}`
		if test.raw != "" {
			body = `{"event_id":"evt-1","expects_reply":` + test.raw + `,"recipient":"ses_target"}`
		}
		spoken, err := ParseExpectsAsSpoken(json.RawMessage(test.raw))
		if err != nil || spoken != test.want {
			t.Errorf("ParseExpectsAsSpoken(%s) = %+v, %v; want %+v", test.raw, spoken, err, test.want)
		}
		for _, answer := range answersCarryingExpectsReply {
			t.Run(test.name+"/"+answer.name, func(t *testing.T) {
				read, envelope, err := answer.decode([]byte(body))
				if err != nil {
					t.Fatalf("decode %s: %v", body, err)
				}
				if read != test.want {
					t.Errorf("the answer's expects_reply read %+v, want %+v", read, test.want)
				}
				if envelope != ExpectsReplyUnset {
					t.Errorf("the embedded envelope's expects_reply read %q, want it shadowed and unset", envelope)
				}
				encoded, err := answer.encode(read)
				if err != nil {
					t.Fatalf("encode the answer read from %s: %v", body, err)
				}
				if written := expectsReplyKey(t, encoded); written != test.written {
					t.Errorf("the answer writes expects_reply %q, want %q: %s", written, test.written, encoded)
				}
			})
		}
	}
}

// No listener writes back a value outside the vocabulary: its route refuses such a request with
// its 400 before it builds an answer. An answer that carries one is refused as the request is, by
// naming the vocabulary, and so is a value this package cannot write: the retired spelling beside
// anything but no reply.
func TestAnAnswerRefusesAnExpectsReplyNoListenerWritesBack(t *testing.T) {
	for _, raw := range []string{`true`, `"later"`, `"no-reply"`, `""`, `2`, `{"expects":"reply"}`} {
		body := `{"event_id":"evt-1","expects_reply":` + raw + `}`
		for _, answer := range answersCarryingExpectsReply {
			t.Run(raw+"/"+answer.name, func(t *testing.T) {
				read, _, err := answer.decode([]byte(body))
				if err == nil || !strings.Contains(err.Error(), "false, optional, required") {
					t.Fatalf("decode %s = %+v, %v; want a refusal naming false, optional, required", body, read, err)
				}
			})
		}
	}
	_, err := json.Marshal(SendResponse{ExpectsReply: ExpectsAsSpoken{Value: ExpectsReplyOptional, LegacyNone: true}})
	if err == nil || !strings.Contains(err.Error(), "false, optional, required") {
		t.Errorf("marshal the retired none beside optional = %v, want a refusal naming false, optional, required", err)
	}
}
