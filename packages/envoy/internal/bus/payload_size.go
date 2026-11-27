// What NATS takes whole: the one size rule every send of this client holds its message to, the
// headers it counts, the room a stored message's relay frame needs beside them, and the one refusal
// a message past the server's max payload earns. The rest of the package's refusals keep files of
// their own the same way (confirm.go, denials.go).

package bus

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
)

// checkSend is the size check every send of the client makes of msg before it waits on a
// reconnect, reads its context or deadline, or sends anything: the ErrTooLarge of a message whose
// data and headers (MessageBytes) are past the max payload the client last read, and nil
// otherwise. nats.go refuses such a message before sending it (nats.ErrMaxPayload, which refused
// names), but a JetStream publish reads its context first (nats.go v1.55.0 requestWithContext), a
// core one is refused on an ended context first (publishConfirmed), and a wait for a reconnect
// comes before either, so a caller whose time had run out read the refusal as a bus that did not
// answer, though the bus was never asked. The sends that make it are every one a caller's payload
// reaches: PublishStoredAck and the raw stored writes through publishStoredRaw,
// PublishCoreContext, StartRequestStoredTo, a window's sends (window.send, window.request) and
// SessionConsumers.Publish; publishJetStream holds its message to the same rule as the first half
// of ownTopicTooLarge. A publish window reads its deadline before this check (WindowUntil). It
// reads Client.maxPayload, whose comment says why that field and not nats.Conn.MaxPayload.
func (c *Client) checkSend(msg *nats.Msg) error {
	return payloadTooLarge(int64(MessageBytes(msg)), c.maxPayload.Load())
}

// TooLargeToPublish answers the ErrTooLarge a publish of item on its own topic (PublishAckWithin)
// would meet, and nil for a message that publish takes or while the client knows no max payload.
// A topic the stream stores holds the envelope to ownTopicTooLarge's two bounds, the rule
// publishJetStream holds the same envelope to; any other topic holds it to the max payload alone,
// as its core publish does (PublishCoreContext). It sends nothing and reads no state of nats.go's,
// as checkSend does not, so a caller asks it before any other bus work for the message. The
// envelope is measured rather than kept (encodedBytes).
//
// A caller may add fields to the envelope before it publishes (cmd/listener's sender stamp), and a
// forward of it carries another dedupe key and header (StartRequestStoredTo), so nil does not
// promise every later send of the message fits: the publish makes its own check
// (publishJetStream), a forward its own of the stream's bound alone (checkSend), and a caller
// whose sender still waits for a forward asks RelayFrameTooLarge of it first.
func (c *Client) TooLargeToPublish(item contracts.Envelope) error {
	size, maxPayload, measured := c.measured(item)
	if !measured {
		return nil
	}
	if c.publishPathOf(item.Topic) != overJetStream {
		return payloadTooLarge(size, maxPayload)
	}
	return ownTopicTooLarge(size, msgIDHeader(storedMsgID(item)), item.Topic, maxPayload)
}

// RelayFrameTooLarge is the ErrTooLarge of a forward of item to subject whose relay frame, the
// envelope as encoded and the relay's headers (RelayFrameHeaderBytes), would be past the max payload
// the client last read (relayFrameTooLarge), and nil for one within it or while the client knows
// none. It sends nothing. cmd/listener's role send asks it of its forward to a live holder, whose
// sender is still waiting for the answer.
func (c *Client) RelayFrameTooLarge(subject string, item contracts.Envelope) error {
	size, maxPayload, measured := c.measured(item)
	if !measured {
		return nil
	}
	return relayFrameTooLarge(size, subject, maxPayload)
}

// measured is item's size as a send encodes it (encodedBytes) and the max payload the client last
// read, the two numbers TooLargeToPublish and RelayFrameTooLarge hold a message to, and false when
// there is nothing to hold it to: the client knows no max payload, which it reads as 0, or the
// envelope does not encode. The max payload is read first, so a client that knows none encodes
// nothing.
func (c *Client) measured(item contracts.Envelope) (size, maxPayload int64, ok bool) {
	maxPayload = c.maxPayload.Load()
	if maxPayload <= 0 {
		return 0, 0, false
	}
	size, ok = encodedBytes(item)
	return size, maxPayload, ok
}

// ownTopicTooLarge is the rule a publish of an envelope of size bytes on topic, a topic the stream
// stores, under header, its Nats-Msg-Id or none (storedMsgID), is held to: the stream's bound first,
// the envelope and header within maxPayload, which is checkSend's rule, so a message past the max
// payload by itself is named as such; then the relay frame's, the envelope and the relay's headers
// within it too (relayFrameTooLarge), since the relay hands a session the stored envelope under
// headers of its own and none of the stored message's. TooLargeToPublish asks it of an envelope it
// measured and publishJetStream of the bytes it sends, so the route's check and the publish cannot
// hold one envelope to two rules.
func ownTopicTooLarge(size int64, header nats.Header, topic string, maxPayload int64) error {
	if err := payloadTooLarge(size+int64(MessageBytes(&nats.Msg{Header: header})), maxPayload); err != nil {
		return err
	}
	return relayFrameTooLarge(size, topic, maxPayload)
}

// relayFrameTooLarge is the ErrTooLarge of a message of data bytes stored on subject whose relay
// frame, its data and the relay's headers (RelayFrameHeaderBytes), is past maxPayload, and nil for
// one within it or for a maxPayload of 0, which a client that knows no max payload reads. The
// refusal's Size is the frame, the bytes held against maxPayload, and its Data the message's own.
func relayFrameTooLarge(data int64, subject string, maxPayload int64) error {
	frame := data + int64(RelayFrameHeaderBytes(subject))
	if maxPayload <= 0 || frame <= maxPayload {
		return nil
	}
	return &MessageTooLargeError{Size: frame, Data: data, MaxPayload: maxPayload}
}

// encodedBytes is the size of item as a send encodes it (json.Marshal), counted rather than kept,
// and false for an envelope that does not encode: an expects_reply outside its vocabulary
// (listenerwire.ExpectsReply.MarshalJSON), which no route builds. Such an envelope has no size to
// hold to a bound, so a check answers nil for it, and the send it checked fails on the same
// encoding and returns that encoding's error as it is.
func encodedBytes(item contracts.Envelope) (int64, bool) {
	var encoded byteCount
	if err := json.NewEncoder(&encoded).Encode(item); err != nil {
		return 0, false
	}
	// Encode ends what it writes with a newline, which json.Marshal, and so the send, leaves out.
	return int64(encoded) - 1, true
}

// refused names nats.go's refusal of msg for its size as the ErrTooLarge it is, as the client's
// own check names a message past the max payload (MessageTooLargeError), and its refusal of a send
// while it reconnects the connection as the ErrReconnecting it is (Reconnecting): a send can still
// meet that refusal after its wait for the connection, when the connection drops again before the
// send. It takes the message rather than a size so that no caller can measure it by another rule
// than checkSend's (MessageBytes). A send meets the size refusal only when its own check let the
// message through because the client's max payload was not the one of the server the message went
// to: a reconnect to a server that takes less, in the moment before onReconnect reads it.
// checkSubject has already refused every subject nats.go would.
func (c *Client) refused(err error, msg *nats.Msg) error {
	if errors.Is(err, nats.ErrMaxPayload) {
		return pastMaxPayload(int64(MessageBytes(msg)), c.Connection().MaxPayload())
	}
	return Reconnecting(err)
}

// payloadTooLarge is the ErrTooLarge of a message of size bytes, headers included, past
// maxPayload, and nil for one within it or for a maxPayload of 0, which a client whose first
// connection has not connected reads.
func payloadTooLarge(size, maxPayload int64) error {
	if maxPayload <= 0 || size <= maxPayload {
		return nil
	}
	return pastMaxPayload(size, maxPayload)
}

// pastMaxPayload is the ErrTooLarge of a message of size bytes, headers included, past the server's
// max payload: payloadTooLarge's before a send and refused's after nats.go's, so a caller reads one
// answer whichever made it.
func pastMaxPayload(size, maxPayload int64) error {
	return &MessageTooLargeError{Size: size, MaxPayload: maxPayload}
}

// MessageTooLargeError is the ErrTooLarge of a message past the server's max payload, with the
// numbers the refusal was made on: the type every refusal of this package for a message past the
// max payload takes, in one of two wordings. A subject past what NATS takes is ErrTooLarge too, but
// not this type (checkSubject), and neither is a key-value bucket's refusal (kv.go).
//
// Size is the bytes held against MaxPayload: the message's data and headers as nats.go counts them
// (MessageBytes), or, for the refusal of a message stored with less room than its relay frame needs
// (relayFrameTooLarge), that frame, the data and the relay's headers (RelayFrameHeaderBytes).
// MaxPayload is the max payload the message was held to: the one the client last read for its own
// checks, or the connection's own for nats.go's (refused). Data is the message's own bytes in
// relayFrameTooLarge's refusal, whose wording names both, and 0 in every other. A caller that must
// name those numbers, as cmd/listener's relay does in the notice of a frame it could not publish,
// reads them from the refusal rather than from the client again, which a reconnect may have moved
// since.
type MessageTooLargeError struct {
	Size, Data, MaxPayload int64
}

func (e *MessageTooLargeError) Error() string {
	if e.Data > 0 {
		return fmt.Sprintf("%s: a message of %d bytes, which reaches a session in a relay frame of %d bytes with the relay's headers, against the server's max payload of %d bytes",
			ErrTooLarge, e.Data, e.Size, e.MaxPayload)
	}
	return fmt.Sprintf("%s: a message of %d bytes, its headers included, against the server's max payload of %d bytes", ErrTooLarge, e.Size, e.MaxPayload)
}

// Unwrap is ErrTooLarge, so errors.Is reads the refusal as ErrTooLarge and as ErrRefused.
func (e *MessageTooLargeError) Unwrap() error { return ErrTooLarge }

// MessageBytes is what nats.go holds msg to against the server's max payload: its data and its
// headers as nats.go encodes them (nats.go v1.55.0 Conn.publish counts len(data)+len(hdr)), which
// nats.Msg.Size counts with the subject and the reply, which the max payload leaves out. It is the
// one count of a message's size: every check here makes it, and so does a caller outside the
// package that measures a frame (cmd/listener's tests).
func MessageBytes(msg *nats.Msg) int {
	return msg.Size() - len(msg.Subject) - len(msg.Reply)
}

// msgIDHeader is the header a stored publish under msgID carries: Nats-Msg-Id, or none for an
// empty msgID. The stored writes carry it on the message rather than as a publish option, so the
// size check counts the bytes nats.go counts.
func msgIDHeader(msgID string) nats.Header {
	if msgID == "" {
		return nil
	}
	return nats.Header{nats.MsgIdHdr: []string{msgID}}
}

// byteCount is an io.Writer that counts the bytes written to it and keeps none of them.
type byteCount int64

func (n *byteCount) Write(p []byte) (int, error) {
	*n += byteCount(len(p))
	return len(p), nil
}
