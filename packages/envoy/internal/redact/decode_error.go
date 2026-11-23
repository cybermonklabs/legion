package redact

import (
	"errors"
	"reflect"
)

// DecodeError is err, encoding/json's error decoding data into the value into points at, in the
// fixed text Answer.Decode names its own decode failure in (decodeText): into's type and, where it
// can, the member that did not decode and the JSON kind it held, and never a value of data, since
// encoding/json's errors, and the errors of the types it decodes into (time.Time's among them),
// quote the value they could not read. It unwraps to nothing, so no caller reaches the decoder's
// own error. It is for a decode of bytes that are no answer a caller reads through Answer.Decode -
// a message a stream carries, a member an answer carried as data (Answer.StreamTo) - whose error
// prints. into is the destination the decode was given; a nil err is nil.
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
