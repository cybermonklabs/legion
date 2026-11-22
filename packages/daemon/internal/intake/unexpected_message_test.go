package intake

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// unexpectedMessage is a jetstream.Msg each of whose methods fails the row t runs, naming the call
// and what it was given, and answers as a delivery that holds nothing: zero values, and
// errUnexpectedCall from each acknowledgement and from the metadata. A fake delivery embeds it and
// defines the methods its row expects, so any other call a handling makes of it (a nak, a
// termination, a read of a part the fake does not carry) fails the row with that call named, at
// the line of the handling that made it, since each method here is a helper. A fake that embeds
// jetstream.Msg itself and leaves it nil, as deliveredMessage and storedMessage did, panics with a
// nil pointer in the method the compiler generates for it instead, which names nothing and ends
// every row of the test binary still to run; and a method nats.go adds to the interface stops this
// file compiling rather than reaching a row as that panic.
type unexpectedMessage struct{ t testing.TB }

var _ jetstream.Msg = unexpectedMessage{}

// errUnexpectedCall is what an unexpected acknowledgement or metadata read answers once the row
// has failed for it, so the handling goes on as it does after a failed call and the consumer's log
// shows what it did next.
var errUnexpectedCall = errors.New("the fake delivery refused a call its row does not expect")

// fail fails the row for call.
func (m unexpectedMessage) fail(call string) {
	m.t.Helper()
	m.t.Errorf("the handling called %s on the delivery, which this row does not expect of it", call)
}

func (m unexpectedMessage) Metadata() (*jetstream.MsgMetadata, error) {
	m.t.Helper()
	m.fail("Metadata()")
	return nil, errUnexpectedCall
}

func (m unexpectedMessage) Data() []byte {
	m.t.Helper()
	m.fail("Data()")
	return nil
}

func (m unexpectedMessage) Headers() nats.Header {
	m.t.Helper()
	m.fail("Headers()")
	return nil
}

func (m unexpectedMessage) Subject() string {
	m.t.Helper()
	m.fail("Subject()")
	return ""
}

func (m unexpectedMessage) Reply() string {
	m.t.Helper()
	m.fail("Reply()")
	return ""
}

func (m unexpectedMessage) Ack() error {
	m.t.Helper()
	m.fail("Ack()")
	return errUnexpectedCall
}

func (m unexpectedMessage) DoubleAck(context.Context) error {
	m.t.Helper()
	m.fail("DoubleAck()")
	return errUnexpectedCall
}

func (m unexpectedMessage) Nak() error {
	m.t.Helper()
	m.fail("Nak()")
	return errUnexpectedCall
}

func (m unexpectedMessage) NakWithDelay(delay time.Duration) error {
	m.t.Helper()
	m.fail(fmt.Sprintf("NakWithDelay(%s)", delay))
	return errUnexpectedCall
}

func (m unexpectedMessage) InProgress() error {
	m.t.Helper()
	m.fail("InProgress()")
	return errUnexpectedCall
}

func (m unexpectedMessage) Term() error {
	m.t.Helper()
	m.fail("Term()")
	return errUnexpectedCall
}

func (m unexpectedMessage) TermWithReason(reason string) error {
	m.t.Helper()
	m.fail(fmt.Sprintf("TermWithReason(%q)", reason))
	return errUnexpectedCall
}
