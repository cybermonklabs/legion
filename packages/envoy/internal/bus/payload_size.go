// What NATS takes whole: the one size rule every send of this client holds its message to, the
// headers it counts, and the refusal a message past the server's max payload earns. The rest of
// the package's refusals keep files of their own the same way (confirm.go, denials.go).

package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
)

// checkSend is the size check every send of the client makes of msg before it waits on a
// reconnect, reads its context or deadline, or sends anything: the ErrTooLarge of a message whose
// data and headers (messageBytes) are past the max payload the client last read, and nil
// otherwise. nats.go refuses such a message before sending it (nats.ErrMaxPayload, which refused
// names), but a JetStream publish reads its context first (nats.go v1.55.0 requestWithContext), a
// core one is refused on an ended context first (publishConfirmed), and a wait for a reconnect
// comes before either, so a caller whose time had run out read the refusal as a bus that did not
// answer, though the bus was never asked. The sends that make it are every one a caller's payload
// reaches: PublishStoredAck and the raw stored writes through publishStoredRaw,
// PublishCoreContext, StartRequestStoredTo, a window's sends (window.send, window.request) and
// SessionConsumers.Publish. A window checks its deadline first, so a send past it keeps the
// ErrWindowDeadline its caller counts (WindowUntil).
//
// Client.maxPayload is the max payload of the server the client's connection is on, read where a
// connection becomes the client's and at every connect and reconnect of it, and 0 until the
// client's first connection has connected, which refuses nothing, as nats.go itself sends an
// unconnected connection's message without measuring it (!nc.initc). The check reads that field
// and no state of nats.go's: nats.Conn.MaxPayload takes nats.go's connection lock, which a
// reconnect attempt holds across its dial and handshake (nats.go v1.55.0 doReconnect,
// :3554-3581), up to connectTimeout.
func (c *Client) checkSend(msg *nats.Msg) error {
	return payloadTooLarge(int64(messageBytes(msg)), c.maxPayload.Load())
}

// TooLargeToPublish answers the ErrTooLarge a publish of item on its own topic (PublishAckWithin)
// would meet because the message - the envelope as that publish encodes it, and the Nats-Msg-Id
// header it carries where the envelope earns one (storedMsgID) - is past the max payload the
// client last read, and nil for a message within it or while the client knows none. It sends
// nothing and reads no state of nats.go's, as checkSend does not, so a caller asks it before any
// other bus work for the message. The envelope is counted as it is encoded rather than kept.
//
// A caller may add fields to the envelope before it publishes (cmd/listener's sender stamp), and a
// forward of it carries another dedupe key and header (StartRequestStoredTo), so nil does not
// promise every later send of the message fits: each send makes its own check (checkSend).
func (c *Client) TooLargeToPublish(item contracts.Envelope) error {
	maxPayload := c.maxPayload.Load()
	if maxPayload <= 0 {
		return nil
	}
	var encoded byteCount
	if err := json.NewEncoder(&encoded).Encode(item); err != nil {
		// An envelope that does not encode is refused by the publish's own encoding, which
		// names the failure.
		return nil
	}
	// Encode ends what it writes with a newline, which json.Marshal, and so the publish, leaves
	// out.
	size := int64(encoded) - 1
	if c.publishPathOf(item.Topic) == overJetStream {
		size += int64(messageBytes(&nats.Msg{Header: msgIDHeader(storedMsgID(item))}))
	}
	return payloadTooLarge(size, maxPayload)
}

// refused names nats.go's refusal of msg for its size as the ErrTooLarge it is, in the words of
// every size refusal (pastMaxPayload), and returns any other error as it is. It takes the message
// rather than a size so that no caller can measure it by another rule than checkSend's
// (messageBytes). A send reaches it only when its own check let the message through because the
// client's max payload was not the one of the server the message went to: a reconnect to a server
// that takes less, in the moment before onReconnect reads it. checkSubject has already refused
// every subject nats.go would.
func (c *Client) refused(err error, msg *nats.Msg) error {
	if errors.Is(err, nats.ErrMaxPayload) {
		return pastMaxPayload(int64(messageBytes(msg)), c.Connection().MaxPayload())
	}
	return err
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

// pastMaxPayload is the ErrTooLarge of a message of size bytes past the server's max payload, in
// the one wording every refusal of one uses (payloadTooLarge before a send, refused after
// nats.go's), so a caller reads one answer whichever made it.
func pastMaxPayload(size, maxPayload int64) error {
	return fmt.Errorf("%w: a message of %d bytes, its headers included, against the server's max payload of %d bytes", ErrTooLarge, size, maxPayload)
}

// messageBytes is what nats.go holds msg to against the server's max payload: its data and its
// headers as nats.go encodes them (nats.go v1.55.0 Conn.publish counts len(data)+len(hdr)), which
// nats.Msg.Size counts with the subject and the reply, which the max payload leaves out.
func messageBytes(msg *nats.Msg) int {
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

// expectLastSubjectSequence is the header of a stored write that lands only while the stream's
// last sequence on its subject is expected, as nats.ExpectLastSequencePerSubject writes it, on the
// message for the same reason msgIDHeader is.
func expectLastSubjectSequence(expected uint64) nats.Header {
	return nats.Header{nats.ExpectedLastSubjSeqHdr: []string{strconv.FormatUint(expected, 10)}}
}

// storedMsgID is the message id an envelope is stored under on its own topic: the dedupe key and
// the topic together where the dedupe key names the upstream event
// (contracts.DedupeKeyNamesTheUpstreamEvent), because one upstream event can be published on
// several topics under one key and each copy must be retained, and none otherwise.
func storedMsgID(item contracts.Envelope) string {
	if !contracts.DedupeKeyNamesTheUpstreamEvent(item) {
		return ""
	}
	return ForwardMsgID(item.DedupeKey, item.Topic)
}

// byteCount is an io.Writer that counts the bytes written to it and keeps none of them.
type byteCount int64

func (n *byteCount) Write(p []byte) (int, error) {
	*n += byteCount(len(p))
	return len(p), nil
}
