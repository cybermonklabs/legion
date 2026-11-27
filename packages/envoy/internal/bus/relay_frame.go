package bus

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
// Envoy-Stream-Id (19) beside StreamIDBytes, Envoy-Stored-At (19) beside 30, RFC 3339 with
// nanoseconds in UTC, Envoy-Gap (13) beside "true", and Envoy-Gap-Reason (20) beside its longest
// value, repository_withheld (19). cmd/listener's TestTheRelayFrameHeaderBoundIsTheRelaysLargestHeader
// requires the relay's largest header block to be this bound exactly, over every gap reason the
// relay can write: the relay names a gap only by a value of its relayGapReason type, whose names
// the row ranges over whole, so a header the relay adds or lengthens fails that test until this
// bound counts it, and so does a bound set larger than the relay needs.
const relayFrameHeaderBase = 12 + 17 + (20 + 20) + (19 + StreamIDBytes) + (19 + 20) + (19 + 30) + (13 + 4) + (20 + 19)

// StreamIDBytes is the length of every stream identity this package writes (StreamIDMetadataKey):
// 16 bytes in hex, from id.New for a stream it creates and deployedStreamID for one it finds
// without an identity. A relay frame carries it in its Envoy-Stream-Id header, and
// RelayFrameHeaderBytes reserves this much for it. A start keeps an identity the stream already
// holds whatever its length (identifiedMetadata), so a stream given a longer one by hand makes
// frames whose headers pass the reserve by the difference, and the relay sends the notice of a
// message that leaves less room than that (cmd/listener's publishTooLargeNotice).
const StreamIDBytes = 32

// RelayFrameHeaderBytes is the most bytes the headers of the relay frame of a message stored on
// subject take (relayFrameHeaderBase): the room against the server's max payload that the
// listener's own checks of a message leave beside its envelope, so a message they accept reaches a
// session whole. Those checks are TooLargeToPublish, which both message routes ask first, the
// publish of an envelope on its own topic (publishJetStream, by ownTopicTooLarge), and a role
// send's forward to a live holder (RelayFrameTooLarge). Every other stored write is held to the
// stream's bound alone: a publish window's sends (Dispatch's outbox), PublishStoredAck and
// PublishStoredAs (a role's kept copy, a delivery exception), the raw stored writes, and the
// forwards no sender waits on - the kept lane's walk, which hands on a copy the stream already
// holds, and the role lane's delivery of a role message it took from its core-NATS subscription,
// whose publisher waits for no answer (cmd/listener's startRoleLane). A message they store with
// less room than this reaches a session as the relay's notice in its place.
func RelayFrameHeaderBytes(subject string) int {
	return relayFrameHeaderBase + len(subject)
}
