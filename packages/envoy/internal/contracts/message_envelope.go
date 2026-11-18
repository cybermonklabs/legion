package contracts

import "github.com/sjawhar/envoy/internal/id"

// Message is what the sender of a message writes into its envelope: everything but the topic and
// dedupe key it goes under, which the route sending it decides (MessageEnvelope,
// AgentSendEnvelope), and the ids and the time the envelope is given as it is built.
type Message struct {
	// Source names who sent it; empty is "agent", a session's own message.
	Source        string
	SourceSession string
	// Text is the message. Its first non-empty line, cut to 160 runes (OneLineSummary), is the
	// envelope's payload summary.
	Text string
	// Payload, when set, is the envelope's payload as given, an empty one included. Unset, the
	// payload is Text whole where the summary is not all of it (a longer or many-line message),
	// and empty where it is.
	Payload    *string
	InReplyTo  string
	Supersedes string
	// Urgency and ExpectsReply left empty are absent from the envelope, as a sender that names
	// neither left them.
	Urgency      string
	ExpectsReply ExpectsReply
	ExpiresAt    *int64
}

// MessageEnvelope is the envelope m goes on the bus as under topic and dedupeKey: a fresh event
// id, source event id and trace id, issued now, from source "agent" where m names none, its
// payload summary and payload read from m as Message says. The listener's publish route builds
// every message it is asked to publish with it, and AgentSendEnvelope every direct send.
func MessageEnvelope(m Message, topic, dedupeKey string) Envelope {
	summary := OneLineSummary(m.Text)
	payload := ""
	if m.Payload != nil {
		payload = *m.Payload
	} else if m.Text != summary {
		payload = m.Text
	}
	source := m.Source
	if source == "" {
		source = "agent"
	}
	return Envelope{
		EventID:        id.New(),
		Source:         source,
		SourceSession:  m.SourceSession,
		SourceEventID:  id.New(),
		Topic:          topic,
		DedupeKey:      dedupeKey,
		IssuedAt:       NowMillis(),
		ExpiresAt:      m.ExpiresAt,
		PayloadSummary: summary,
		Payload:        payload,
		TraceID:        id.New(),
		InReplyTo:      m.InReplyTo,
		Supersedes:     m.Supersedes,
		Urgency:        m.Urgency,
		ExpectsReply:   m.ExpectsReply,
	}
}

// AgentSendEnvelope is the envelope of a direct message to targetSession as the stream stores it:
// on the session's own topic, in the form every reader takes (LegacyAgentSubject), under
// AgentSendDedupeKey(targetSession, idempotencyKey), with a key minted here for a send that names
// none, which only a re-send of that very envelope repeats (MintedDedupeKeyPattern).
//
// Two senders build it, and both build it here so the two cannot drift apart: the listener's send
// route (POST /v1/messages/send), which stores it for a session it holds a record of, and
// Dispatch's own queueing of a person's targeted message, which publishes the envelope that route
// would have stored straight onto the session's topic when neither the listener's registry nor
// its known-session store holds the session. That copy then reaches the session's host as the
// send would have, under the same topic and dedupe key, so the stream and the host take a copy
// whose send did land as that same message again rather than a second one.
func AgentSendEnvelope(targetSession, idempotencyKey string, m Message) Envelope {
	if idempotencyKey == "" {
		idempotencyKey = id.New()
	}
	return MessageEnvelope(m, LegacyAgentSubject(targetSession), AgentSendDedupeKey(targetSession, idempotencyKey))
}
