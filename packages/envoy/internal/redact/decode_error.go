package redact

import (
	"encoding/json"
	"errors"
	"reflect"
)

// DecodeError is err, encoding/json's error decoding data into the value into points at, in the
// fixed text Answer.Decode names its own decode failure in (decodeText): into's type and, where it
// can, the member that did not decode and the JSON kind it held, and never a value of data, since
// encoding/json's errors, and the errors of the types it decodes into (time.Time's among them),
// quote the value they could not read. It unwraps to nothing, so no caller reaches the decoder's
// own error. A decode by json.Unmarshal goes through UnmarshalData, which wraps its error so; this
// is for a decode UnmarshalData does not make (a json.Decoder that refuses an unknown member)
// whose error prints. into is the destination the decode was given; a nil err is nil.
func DecodeError(err error, data []byte, into any) error {
	if err == nil {
		return nil
	}
	t := reflect.TypeOf(into)
	if t == nil {
		// encoding/json refuses a nil destination, which names no type.
		t = reflect.TypeFor[any]()
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return errors.New(decodeText(err, data, t))
}

// UnmarshalData fills into from data as JSON (json.Unmarshal), for bytes that are no answer a
// caller reads through Answer.Decode: a message a stream carries, a member an answer carried as
// data. Its error is DecodeError's fixed text, which quotes no value of data; nil where data
// decodes. A caller decodes such bytes through it, not through json.Unmarshal and then DecodeError
// of the same arguments, a wrap a site can forget and so print encoding/json's own error, which
// quotes the value it could not read.
func UnmarshalData(data []byte, into any) error {
	if err := json.Unmarshal(data, into); err != nil {
		return DecodeError(err, data, into)
	}
	return nil
}

// Unmarshal fills into from a's body as JSON, read as the answer's data and never as its words:
// the one read of an answer a caller decodes by encoding/json itself, for a destination
// Answer.Decode refuses because it takes the answer's text as it came — a json.RawMessage handed
// back to whoever knows what it means, a map of them a proxy forwards. Its error is DecodeError's
// fixed text, which quotes no value of the body. It reads the bytes where they are: a caller that
// wrote them to a buffer of its own (Answer.StreamTo) held the answer a second time.
//
// Nothing it fills carries the record of the answer's request, which Answer.Decode gives every
// RemoteText it decodes, so nothing it fills may print: a caller whose destination holds another
// party's words that print decodes through Answer.Decode instead.
func Unmarshal(a Answer, into any) error { return UnmarshalData(a.bytes(), into) }
