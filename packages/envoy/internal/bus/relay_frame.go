package bus

import "fmt"

// A stored message reaches a session in consumer mode as a relay frame: cmd/listener's relay
// (sessionRelays.publishFrame) publishes the stored bytes, unchanged, on the session's relay
// subject (DeliverSubject) under headers of its own (relayFrameHeader), and with none of the
// stored message's own headers, its Nats-Msg-Id among them. NATS takes the frame only whole, its
// data and those headers within the server's max payload, so an envelope the stream holds with
// less room than the relay's headers take reaches no session whole: the relay sends the session
// a notice in its place. relayFrameHeaderBase is the most bytes those headers take beside the
// subject they name, and RelayFrameHeaderBytes adds the subject.
//
// Each header line is its name, ": ", its value and CRLF, and the block adds its first line,
// "NATS/1.0" and CRLF (10 bytes), and a closing CRLF (2): Envoy-Subject without its subject (17),
// Envoy-Stream-Seq (20) and Envoy-Delivered (19) beside 20 digits each, a uint64's most,
// Envoy-Stream-Id (19) beside 32 characters, the 16 bytes in hex every stream identity this
// package writes takes (id.New, deployedStreamID), Envoy-Stored-At (19) beside 30, RFC 3339 with
// nanoseconds in UTC, Envoy-Gap (13) beside "true", and Envoy-Gap-Reason (20) beside its longest
// value, repository_withheld (19). cmd/listener's TestTheRelayFrameHeaderBoundIsTheRelaysLargestHeader
// holds the relay's largest header block to it, so a header the relay adds fails that test
// until this bound counts it.
const relayFrameHeaderBase = 12 + 17 + (20 + 20) + (19 + 32) + (19 + 20) + (19 + 30) + (13 + 4) + (20 + 19)

// RelayFrameHeaderBytes is the most bytes the headers of the relay frame of a message stored on
// subject take (relayFrameHeaderBase): the room against the server's max payload that a publish of
// an envelope on its own topic leaves beside the envelope (publishJetStream, TooLargeToPublish),
// so a message the listener accepts reaches a session whole.
func RelayFrameHeaderBytes(subject string) int {
	return relayFrameHeaderBase + len(subject)
}

// relayFrameTooLarge is the ErrTooLarge of a message of data bytes stored on subject whose relay
// frame, its data and the relay's headers (RelayFrameHeaderBytes), is past maxPayload, and nil
// for one within it or for a maxPayload of 0, which a client that knows no max payload reads.
func relayFrameTooLarge(data int64, subject string, maxPayload int64) error {
	frame := data + int64(RelayFrameHeaderBytes(subject))
	if maxPayload <= 0 || frame <= maxPayload {
		return nil
	}
	return fmt.Errorf("%w: a message of %d bytes, which reaches a session in a relay frame of %d bytes with the relay's headers, against the server's max payload of %d bytes",
		ErrTooLarge, data, frame, maxPayload)
}
