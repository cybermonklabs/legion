// Package listenerwire holds the bodies of the Envoy listener's HTTP API that its Go clients send
// and read: the requests of POST /v1/interests/subscribe, POST /v1/messages/send, POST
// /v1/messages/publish, POST /v1/interests/unsubscribe, the PUT routes of /v1/legion/agents and PUT
// /v1/legion/projects/<legion_project>, the answers of those and of GET /v1/sessions, GET
// /v1/registry/<session_id>, GET /v1/interests/, GET /v1/roles/<role>, the GET routes of
// /v1/legion/agents and DELETE /v1/sessions/<session_id>, the hand-off the listener publishes for a
// stopped Legion agent's daemon (LegionHandoff) and the subject it publishes it on
// (LegionHandoffSubject), and the envelope and the states they carry. The
// listener (cmd/listener) writes and reads these types, and so does each of its Go clients: the
// broker's wake (internal/cipher/wake), Dispatch's client (internal/dispatch/envoy), the load-test
// harness's listener client (loadtest/listenerclient), the refill rig
// (internal/testnats/cmd/refillrig), and the Legion daemon's publisher, controller probe and agent
// registry writer, which are another module and so need this package public. LegionHandoff is not
// an HTTP body: the listener publishes it on the bus, and the daemon's hand-off reader
// (internal/intake) decodes it.
//
// It imports the standard library alone (deps_test.go), so a client links nothing of the server
// by reading its bodies, and clients_test.go holds each of those clients to these types: one that
// encodes or decodes a body of its own, even one field of one, fails it. The load-test harness's
// main package also calls Dispatch, NATS and Cloud SQL, so the test reads its listener client
// instead, and holds the main package to naming no listener route of its own.
//
// A request's fields are omitempty but where the listener reads an absent field differently from
// an empty one: the listener only decodes a request, so each client sends the fields it sets. A
// field whose absence the listener reads differently is a pointer, written whenever the client
// sets it (MessageBody.Payload and Handoff, PublishRequest.Keep, SubscribeDelivery.Resume,
// SubscribeResume.Seq and LegionProjectRequest.Answering), or carries no omitempty, so an empty
// value arrives as the empty value it is (SubscribeRequest.Topics and Capabilities). An answer's
// fields carry the tags the listener has always written them with. LegionAgentEntry is both: the
// daemon's registry writes send it and the registry's reads answer it, so its fields the daemon
// leaves out are omitempty, and the listener writes each of them on every answer it gives
// (recorded_at).
package listenerwire

import "encoding/json"

// SessionState is what a session's last registration says about it: live, unresponsive or gone.
// The listener's internal/session declares its values (session.Live, session.Unresponsive and
// session.Gone) and the rule that decides them (session.StateOf).
type SessionState string

// RoleState is what a role's claim came to when the listener resolved its holder: live, or one of
// the no-holder reasons the listener's 404 carries, unclaimed and holder_lapsed. The listener's
// internal/roles declares its values (roles.Live, roles.Unclaimed and roles.Lapsed).
type RoleState string

// TmuxPlace is a session's pane and the window and tmux session holding it, in the wire names
// every side of the hop shares (packages/envoy-client's readTmuxPlace reports it,
// packages/contracts' TmuxPlace declares it). No delivery reads it: it is what lets Dispatch draw
// a machine's tmux layout and say which pane runs which session. A session outside tmux is a nil
// TmuxPlace, never a place with empty members: every place names a whole pane.
type TmuxPlace struct {
	Socket      string `json:"socket"`
	ServerPID   string `json:"server_pid"`
	SessionName string `json:"session_name"`
	WindowID    string `json:"window_id"`
	WindowIndex int    `json:"window_index"`
	WindowName  string `json:"window_name"`
	PaneID      string `json:"pane_id"`
	PaneIndex   int    `json:"pane_index"`
	// Layout is tmux's window_layout string, which draws how the window is split. Empty when the
	// client dropped it: a window split finely enough grows a layout past what a registration
	// carries, and the other nine members are sent without it rather than the place being lost.
	Layout string `json:"layout,omitempty"`
	Zoomed bool   `json:"zoomed"`
}

// APIError is the body of a /v1 error answer. Code names an answer a caller acts on beyond its
// status (the list routes' refilling 503, code refilling, and a send's or publish's 503 that may
// have stored the message, CodeMaybeStored); every other answer leaves it out. Expected names the
// request fields the refusal is about.
type APIError struct {
	Code     string   `json:"code,omitempty"`
	Error    string   `json:"error"`
	Expected []string `json:"expected,omitempty"`
}

// CodeMaybeStored is the code of a send's or a publish's 503 after which the stream may hold the
// message: its publish went out and was never acknowledged, or its request ended while the stream
// was still storing an earlier publish under the same message id, an earlier attempt of the same
// message. A caller reads it as a receipt timeout - it may have landed, and a repeat under the
// same idempotency key is recognised - and not as a send that stored nothing. Every other 503 of
// those routes leaves the code out.
const CodeMaybeStored = "maybe_stored"

// ReadsHeader is the request header in which a client of the send and the publish routes names
// the answers it reads beyond the whole envelope every client release reads, as a comma-separated
// list of tokens; the listener reads it on those two routes alone. ReadsKeyTaken is the one token
// there is: the key-taken answer, which a request gets for a copy another sender stored under its
// message id. That answer carries the copy's event_id, topic, dedupe_key, times and expects_reply
// with Duplicate, and leaves every other string field empty, so a client that parses each answer
// as a whole envelope (@legion/envoy-client before it sent this header) rejects it. A request that
// does not name ReadsKeyTaken gets, for such a copy, the envelope the listener built for it, with
// Duplicate: a whole envelope, as a listener before the key-taken answer gave every repeat.
// @legion/envoy-client names ReadsKeyTaken on a send or a publish that names its source_session.
const (
	ReadsHeader   = "Envoy-Reads"
	ReadsKeyTaken = "key-taken"
)

// EdgeRefusal is the body of an edge listener's refusal of a request its token's projects do not
// cover (ENVOY_EDGE_PROJECTS). Reason names the rule: token_not_admitted, topic_not_in_project,
// repository_not_in_project, repositories_unknown, topic_not_published, role_not_in_project,
// session_not_in_project, legion_registry_not_written or sender_not_session (a send or a publish
// whose source is not agent or whose source_session is not the token's session). Refused is set
// on a registration's 403 alone: every topic of the request the edge refused, each once, as the
// request carried it and in its order, which a client drops before it registers the rest again;
// nothing of such a registration is stored. Reason is repository_not_in_project when every
// refused topic is a GitHub topic, and topic_not_in_project when any is not. Expected names the
// request fields the refusal is about.
type EdgeRefusal struct {
	Error    string   `json:"error"`
	Reason   string   `json:"reason"`
	Refused  []string `json:"refused,omitempty"`
	Expected []string `json:"expected,omitempty"`
}

// MessageBody is the message a send or a publish carries, which the listener builds the envelope
// it puts on the bus from. A sender that leaves Source out speaks as an agent.
type MessageBody struct {
	Source         string  `json:"source,omitempty"`
	SourceSession  string  `json:"source_session,omitempty"`
	Message        string  `json:"message,omitempty"`
	Payload        *string `json:"payload,omitempty"`
	IdempotencyKey string  `json:"idempotency_key,omitempty"`
	DedupeKey      string  `json:"dedupe_key,omitempty"`
	InReplyTo      *string `json:"in_reply_to,omitempty"`
	Supersedes     *string `json:"supersedes,omitempty"`
	Urgency        *string `json:"urgency,omitempty"`
	// ExpectsReply arrives as the JSON literal false, "optional" or "required" (and, until no
	// deployment pins a plugin build old enough to write it, the old string "none"), so it is
	// read raw and parsed by ParseExpectsReply rather than decoded as one Go type.
	ExpectsReply json.RawMessage `json:"expects_reply,omitempty"`
	ExpiresAt    *int64          `json:"expires_at,omitempty"`
	// Handoff false keeps the message from being handed to a stopped Legion agent's daemon (the
	// listener's handOffToLegion): the daemon's own notices carry it, so a notice to a stopped
	// agent is refused or kept as it would be with no hand-off. Absent and true are alike, so it is
	// written only by a client that sets it.
	Handoff *bool `json:"handoff,omitempty"`
}

// SendRequest is the body of POST /v1/messages/send: a message for one session.
type SendRequest struct {
	MessageBody
	TargetSession string `json:"target_session,omitempty"`
	// ConnectedOnly asks for a stopped target to be refused rather than written to: Dispatch sets
	// it on every send of a model or thinking switch (isSessionSwitch in internal/dispatch/api),
	// because a switch that applied at an unknown later time is worse for its sender than no
	// switch at all, and on every send of a message whose agents-route request set it. Absent,
	// which is every other sender and every older client, nothing changes.
	ConnectedOnly bool `json:"connected_only,omitempty"`
	// Role marks a send Dispatch makes for a route to a role with that role, which the envelope
	// then carries (Envelope.Role): the target is the session the role's claim named when
	// Dispatch resolved the route. The listener takes it from Dispatch's own verified principal
	// alone (its roleMarkRefusal), since the body's source is the caller's own word, and refuses
	// every other caller carrying it 403 naming the field. A send that carries it is answered 200
	// SendResponse while the role's claim still names the target, and 202 KeptSendResponse when
	// the claim names another session, or none. Absent, nothing changes.
	Role string `json:"role,omitempty"`
}

// PublishRequest is the body of POST /v1/messages/publish: a message for a topic, a role's among
// them. Keep false asks the listener to refuse a role nobody holds with its 404 rather than keep
// the message for the role's next holder, which is what it does when Keep is absent. A role
// message whose MessageBody names no SourceSession is refused with that 404 whatever Keep says,
// since the dead letter of a kept copy that expires goes to the source session alone.
type PublishRequest struct {
	MessageBody
	Topic string `json:"topic,omitempty"`
	Keep  *bool  `json:"keep,omitempty"`
}

// SendResponse is the answer to a targeted send. Duplicate reports that JetStream already held
// another request's copy of this message, so the stream stored nothing new; the publish still
// reached the agent's subject. The envelope then comes from the copy the stream holds, read back
// at the sequence its duplicate verdict named (the listener's publishHeld): that copy whole, for
// its own sender, whose source and source_session are the request's, a token's session where the
// request presents one; the key-taken answer, for a request that names ReadsKeyTaken
// (ReadsHeader); and the envelope the listener built for this request, for any other, which no
// stored message carries. A copy that names no session is whole only for its own request (the
// same event_id). A verdict that names this request's own copy, which a resend after a closed
// connection can, answers that copy with no Duplicate. A copy that cannot be read is the 503 with
// Retry-After: 1 that the listener writes under its storedCopyUnreadablePrefix, never an envelope
// the stream does not hold.
// What recognises that repeat, and for how long, is stated on DELIVERY_DUPLICATE_WINDOW_MS in
// @legion/contracts (contracts.DeliveryDuplicateWindow in the listener's module). It can only be
// true for a send whose dedupe key names the upstream event
// (contracts.DedupeKeyNamesTheUpstreamEvent): one of Dispatch's, and an agent's send under a key
// minted once for its message, which @legion/envoy-client mints for every send that names none,
// so a repeat of either gets it. The listener's sender limit reads the same verdict and charges
// such a repeat nothing (its senderCharge.published), unless the verdict answers the bus's resend
// after a closed connection (bus.StoredAck.Resent). A send handed to a stopped Legion agent's
// daemon (the listener's handOffToLegion) answers the verdict of that hand-off's publish instead,
// whose MsgId comes from the envelope's dedupe key whatever that key names, so a repeat under one
// idempotency key gets it whoever minted the key. Its envelope then comes from the hand-off the
// stream holds, read back on the hand-off's own subject (the listener's answerHandedOff) by the
// rules above, as a live session's repeat's does, and its charge is the wake its send reported,
// which the verdict does not cancel. Absent means false, which is what an older listener's answer
// reads as.
type SendResponse struct {
	Envelope
	// ExpectsReply is this answer's expects_reply: the answered envelope's, written in the
	// dialect the request spoke where the two agree (the listener's expectsReplyAnswer). It
	// shadows the embedded envelope's own field: of two fields sharing one JSON name
	// encoding/json writes the shallower, so these bytes are what the answer carries and every
	// SendResponse the listener builds fills them.
	ExpectsReply json.RawMessage `json:"expects_reply,omitempty"`
	Recipient    string          `json:"recipient"`
	// RecipientState is the state the recipient's record puts it in at the moment the send was
	// accepted: live, or unresponsive for a session that has stopped answering and whose messages
	// the listener still stores, and gone for a session the session registry holds no entry of
	// within the gone horizon that the known-session store still knows, which State then answers
	// stopped. A sender that must know whether its message will be read soon reads it; one that
	// only needs the message stored ignores it.
	RecipientState SessionState `json:"recipient_state"`
	// State is what the connection probe found the recipient to be when the message was stored,
	// connected or stopped, by the rules the listener's sendHandler states. It is a field of its
	// own rather than a fourth value of RecipientState, because every released plugin reads that
	// one as a closed list of three and would fail to read an answer carrying a fourth; a client
	// older than this field drops it. stopped is never a SessionState. A send handed to a stopped
	// Legion agent's daemon (the listener's handOffToLegion) was stored for no session and probed
	// none, so it carries neither word: it is empty, which a client reads as connected, since that
	// daemon answers it now from the agent's own conversation.
	State string `json:"state"`
	// LastSeen is that recipient's last registration (Unix ms), so an unresponsive recipient's
	// sender can say how long it has been stalled: its entry's where it holds one, else its
	// known-session record's last write.
	LastSeen  int64 `json:"last_seen,omitempty"`
	Duplicate bool  `json:"duplicate,omitempty"`
}

// KeptSendResponse is the send route's 202 for a send marked with a role (SendRequest.Role) whose
// claim named another session than the target, or none, when the send arrived: the send's
// envelope as the role's kept lane holds it, its recipient as the request named it, the session
// the claim names (Holder, absent for a role nobody claims), and Kept and KeptUntil, the end of the
// kept copy's hour in Unix milliseconds, named as the publish route's kept answer names them
// (PublishResponse). The message reaches the session that holds the role then, live, through the
// lane's walk. It carries no recipient_state and no state: the message was not stored for the
// target, so nothing about the target was read. The envelope is the copy the lane holds, as
// PublishResponse's kept answer gives it: whole for its own sender, and, with Duplicate, the
// key-taken answer or the envelope built for the request for a copy another sender kept under the
// same key (SendResponse).
type KeptSendResponse struct {
	Envelope
	// ExpectsReply is this answer's expects_reply: the kept copy's, written in the dialect the
	// request spoke where the two agree, shadowing the embedded envelope's own field exactly as
	// SendResponse.ExpectsReply does.
	ExpectsReply json.RawMessage `json:"expects_reply,omitempty"`
	Recipient    string          `json:"recipient"`
	Holder       string          `json:"holder,omitempty"`
	Kept         bool            `json:"kept"`
	KeptUntil    int64           `json:"kept_until"`
	// Duplicate reports that the kept copy is another request's, as PublishResponse.Duplicate
	// reports it for the publish route's kept answer. Absent means false, which is what an older
	// listener's answer reads as.
	Duplicate bool `json:"duplicate,omitempty"`
}

// NoLiveSessionRefusal is the 404 of a send, or of a registry lookup, for a session the listener
// will not answer for: one no record is held for or whose record is past the gone horizon. State
// and LastSeen say which of those it was and when the session last registered, and the error
// keeps the "no live session <id>" prefix exactly (NO_LIVE_SESSION in @legion/contracts,
// contracts.NoLiveSession in the listener's module, then the id): Dispatch classifies a delivery
// failure by that prefix (internal/dispatch/api/envoy_resolve.go), so a listener that says more
// must still say it after those words, and a Dispatch that reads only the prefix is unaffected.
type NoLiveSessionRefusal struct {
	Error    string       `json:"error"`
	State    SessionState `json:"state"`
	LastSeen int64        `json:"last_seen,omitempty"`
}

// PublishResponse is the publish route's accepted answer. Holder and HolderState name the role
// message's holder and the state its own record puts it in, as GET /v1/roles/<role> names them
// and under the same two field names, so a client reads one shape from every role answer: on a
// 200, the live holder the message was stored for; on a 202 kept for a claim whose holder is not
// live, that holder and its state, unresponsive or gone, with LastSeen beside them as the 404
// holder_lapsed names it. Kept, KeptReason and KeptUntil name a role send the listener could not
// deliver and kept on the role's kept lane instead (the listener's roleSend.kept), and are absent
// from every other answer, a delivered role send's included. A role send has three answers and a
// sender tells them apart by status: 200, a live holder took the message; 202, no live session
// holds the role and the message waits on the role's kept lane until KeptUntil for the next
// session that holds it live; 404, nothing was kept. Each field is left out when it is empty, so
// a role send carries no recipient count and a publish to a topic the stream captures carries the
// envelope and its counts alone, and Duplicate where the stream already held the message.
type PublishResponse struct {
	Envelope
	// ExpectsReply is this answer's expects_reply: the answered envelope's, written in the
	// dialect the request spoke where the two agree, shadowing the embedded envelope's own field
	// exactly as SendResponse.ExpectsReply does.
	ExpectsReply json.RawMessage `json:"expects_reply,omitempty"`
	Holder       string          `json:"holder,omitempty"`
	HolderState  SessionState    `json:"holder_state,omitempty"`
	Kept         bool            `json:"kept,omitempty"`
	// KeptReason is the state the resolution found, in the same wire words the 404's own reason
	// carries (RoleRefusal.Reason): unclaimed for a role no claim holds, and holder_lapsed for a
	// claim whose holder is not live, which the listener keeps for too (its roleKeepable).
	KeptReason RoleState `json:"kept_reason,omitempty"`
	// Recipient is the stopped Legion agent a role publish with no live holder was handed to (the
	// listener's handOffToLegion), in place of the holder a delivered one names, and is absent
	// from every other answer.
	Recipient string `json:"recipient,omitempty"`
	// KeptUntil is when the kept copy's hour ends, in milliseconds since the epoch: the time the
	// stream stored the copy plus the listener's roleKeepWindow, against which the walk measures
	// its age. A repeat of a dedupe key whose copy is still on the lane inside its hour stores
	// nothing new and names that copy's, beside that copy's envelope and event id.
	KeptUntil int64 `json:"kept_until,omitempty"`
	// LastSeen is when a lapsed holder last registered (roles.Resolution.LastSeen), as the 404
	// holder_lapsed names it, and absent when nothing can date that holder.
	LastSeen *int64 `json:"last_seen,omitempty"`
	// Recipients and RecipientsConnected are the counts the listener made just before a stored
	// publish was published: how many sessions other than the sender the topic is for, and how
	// many of those are live now. They are pointers so that a count of zero is carried rather
	// than omitted - "no other session subscribes" is the answer that matters most - and nil,
	// left out, where there is no count to give: a listener whose known-session store does not
	// answer, or whose interest copy cannot be served, sends neither; one whose session registry
	// copy cannot be served sends recipients alone; and a role send sends neither, its answer
	// naming its holder. An older client reads neither field.
	Recipients          *int `json:"recipients,omitempty"`
	RecipientsConnected *int `json:"recipients_connected,omitempty"`
	// Duplicate reports that the stream already held another request's copy of this message,
	// under this one's message id, so it stored nothing new and the envelope comes from the copy it
	// holds (the listener's publishHeld) as a send's does (SendResponse). Only a
	// publish whose dedupe key names its event gets a message id
	// (contracts.DedupeKeyNamesTheUpstreamEvent in the listener's module), so only such a repeat
	// gets it: the shared transport's, under the idempotency_key it mints once per message, among
	// them. A role send's 200 carries it for a forward the stream already held for its holder (the
	// listener's roleSend.deliveredHeld), whatever the key's form, since every forward has a
	// message id, and its 202 for a repeat whose kept copy is another request's (roleSend.kept).
	// Beside Recipient it reports that the stream already held the hand-off of a role publish
	// with no live holder, under the MsgId the listener builds from the envelope's dedupe key, a
	// role publish repeated under one dedupe_key or idempotency_key, and the envelope comes from
	// that hand-off's copy (the listener's answerHandedOff), as SendResponse.Duplicate does for a
	// send. Absent means false, which is what an older listener's answer reads as.
	Duplicate bool `json:"duplicate,omitempty"`
	// Warnings names a role send's role when its topic named it in the old form, the role alone,
	// with its new form, project first ("<old> is the old form of <new>; name <new>",
	// store.Registry.ReadRole), as the subscribe and claim routes name an old form; it is left out
	// when the topic named the role in the new form, and from every answer that is no role send.
	Warnings []string `json:"warnings,omitempty"`
}

// SessionInfo is the joined view of a session's interest record (topics, dir, machine) and its
// session entry (its title, its capabilities and how it receives), which GET /v1/sessions lists
// and GET /v1/registry/<session_id> answers for one session. Its host is where the session runs,
// its recorded host or else MachineID; no delivery reads it. An inbox message is the session's
// owner's to deliver: the listener its stored subscription names, else the one MachineID names.
// Any other listener leaves inbox messages to the owner silently.
type SessionInfo struct {
	SessionID string `json:"session_id"`
	// State is what the session's last registration says about it (session.StateOf): live, or
	// unresponsive for one past the live window that is not gone. LastSeen below is that
	// registration, so the two together say how long it has been stalled.
	State     SessionState `json:"state"`
	MachineID string       `json:"machine_id"`
	Host      string       `json:"host"`
	// TmuxPlace is where the session's process sits in tmux, as its last registration reported
	// it. Absent, rather than null, for a session in no pane: a cluster whose sessions run
	// outside tmux adds no bytes to any row of this list.
	TmuxPlace *TmuxPlace `json:"tmux_place,omitempty"`
	Dir       string     `json:"dir"`
	// Port is 0 for every entry the listener writes: it refuses a registration that names a
	// serving port and delivers to no port. An entry an earlier listener stored with a port is
	// answered with that port until it lapses (the session bucket's TTL). It is answered at all
	// because every released client parses it (packages/envoy-client/src/transport.ts), and a
	// client that does not tolerate its absence would break without it.
	Port           int      `json:"port"`
	Title          string   `json:"title"`
	Capabilities   []string `json:"capabilities"`
	SelfSubscribed bool     `json:"self_subscribed"`
	Topics         []string `json:"topics"`
	Roles          []string `json:"roles"`
	UpdatedAt      int64    `json:"updated_at"`
	LastSeen       int64    `json:"last_seen"`
	// The plugin the session registered through and its build; absent for a session whose plugin
	// predates the fields.
	Plugin        string `json:"plugin,omitempty"`
	PluginVersion string `json:"plugin_version,omitempty"`
	// Delivery is how the listener delivers to the session: "consumer" (session.DeliveryConsumer)
	// when its own session consumer holds its stored messages, empty for a session on the core
	// path. Written always, never omitempty: the production reading of "0 sessions on core" has
	// to tell a listener that does not report delivery at all - a build older than this field,
	// which sends no such key - from a session the listener reports as on the core path. With
	// omitempty both are a row with no "delivery" key and the two cannot be told apart, so the
	// count of core sessions would read as zero on a fleet that never reported one.
	Delivery string `json:"delivery"`
	// ClientInstanceID names the client process whose registration this is
	// (session.SessionEntry.ClientInstanceID): one Oh My Pi Envoy extension instance, one
	// Claude Code channel-server process, each minting the id at its own start, so it tells
	// one process's registration from its successor's under the same session id. It is left
	// out where the entry carries none - a client predating it, or one that states no
	// instance - as GET /v1/roles/<role> leaves its own client_instance_id out, so a client
	// reads an absent field as "this registration names no process" rather than as the empty
	// string.
	ClientInstanceID string `json:"client_instance_id,omitempty"`
}

// StoppedSessionInfo is what GET /v1/registry/<session_id>?include=unresponsive,stopped answers
// for a session the session registry holds no entry for within the accepted states and the
// known-session store holds a record of (session.KnownSession): the record's own fields, and
// nothing it does not hold. It carries no port and no host, not even empty ones, because the
// record holds neither and a session that is not running has no address to be reached at - a
// caller that read an empty host as one would dial nothing. State is always stopped, which is
// never a SessionState: a session that holds an entry is answered SessionInfo with the state its
// entry's age gives it, so both shapes of this one request carry state, their shared fields under
// the same names, and a reader of it needs no second rule.
type StoppedSessionInfo struct {
	SessionID    string   `json:"session_id"`
	State        string   `json:"state"`
	LastSeen     int64    `json:"last_seen"`
	Title        string   `json:"title"`
	Capabilities []string `json:"capabilities"`
	Topics       []string `json:"topics"`
}

// SessionRegistration is what GET /v1/registry/<session_id>?view=registration answers: what the
// session bucket holds for that id, with nothing joined to it. It is a type of its own rather
// than a SessionInfo because this view reads neither the session's interest record nor the role
// claims, and a SessionInfo with empty topics and roles would be indistinguishable from a session
// that holds none. It therefore carries none of the full answer's other fields and says only what
// the registration itself says: which session it is, the state that registration puts it in, when
// it was written, and which client process wrote it. A listener that predates the view answers
// the full SessionInfo instead, whose fields of these names mean the same.
type SessionRegistration struct {
	SessionID string       `json:"session_id"`
	State     SessionState `json:"state"`
	// LastSeen and UpdatedAt are both the registration's own write
	// (session.SessionEntry.UpdatedAt), under the two names the full answer gives it, so a caller
	// reads this view with the reader it already has for that one.
	LastSeen  int64 `json:"last_seen"`
	UpdatedAt int64 `json:"updated_at"`
	// ClientInstanceID names the client process whose registration this is
	// (session.SessionEntry.ClientInstanceID), and is what this view exists to answer. It is left
	// out where the entry carries none - a client predating the field, or one that states no
	// instance - as SessionInfo leaves it out, so a reader takes an absent field for "this
	// registration names no process" rather than for the empty string.
	ClientInstanceID string `json:"client_instance_id,omitempty"`
}

// DeleteSessionResponse is what DELETE /v1/sessions/<session_id> answers a request that named a
// condition (client_instance_id, if_not_registered_after): whether the registration is gone, and
// for one that stayed why it did. A request that named none answers 200 with no body at all, as
// that route always has, and so does a listener of a release that judges no condition. Deleted is
// a pointer, which the listener always sets, so a reader tells this answer from a 200 that names
// no outcome.
type DeleteSessionResponse struct {
	Deleted *bool  `json:"deleted"`
	Reason  string `json:"reason,omitempty"`
}

// Interest is one session's persisted Envoy topic subscriptions, as the listener's interest bucket
// stores it (internal/store) and its interest routes answer it (InterestRow).
type Interest struct {
	SessionID string   `json:"session_id"`
	MachineID string   `json:"machine_id"`
	Dir       string   `json:"dir"`
	Topics    []string `json:"topics"`
	UpdatedAt int64    `json:"updated_at"`
}

// InterestRow is one persisted interest as GET /v1/interests/ and GET /v1/interests/<session_id>
// answer it: the stored record plus LastSeen, the session's last heartbeat (Unix ms) as the
// listener's session cache knows it, zero when it does not know it (it lost it in a restart, or
// predates the field). UpdatedAt, the row's last write, can lag that heartbeat by up to the
// listener's interest refresh age (store.InterestRefreshAge), since an unchanged row is rewritten
// only that often.
type InterestRow struct {
	Interest
	LastSeen int64 `json:"last_seen,omitempty"`
}

// SubscribeRequest is the body of POST /v1/interests/subscribe: a session's registration, which
// each agent session's envoy extension sends when it subscribes and at every heartbeat. The
// listener builds the session's entry fresh from it, so a field a registration leaves out is
// written empty, clearing what a previous registration stored.
type SubscribeRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Dir       string `json:"dir,omitempty"`
	// Topics are the topics the session follows. Absent and empty decode to different values, nil
	// and the empty list, so the field is written whatever it holds and the listener reads the
	// value the client had.
	Topics []string `json:"topics"`
	// Port is the session's own HTTP serving port, which the listener parses only to refuse a
	// registration that names one above 0: it delivers nothing to a port, and a session receives
	// from its own consumer and nothing else.
	Port  int    `json:"port,omitempty"`
	Title string `json:"title,omitempty"`
	// Capabilities are what the session advertises (the delivery modes it takes among them),
	// written whatever they hold for the reason Topics are.
	Capabilities   []string `json:"capabilities"`
	SelfSubscribed bool     `json:"self_subscribed,omitempty"`
	// Host is envoy-client's machineID() (ENVOY_MACHINE_ID, then an agent box's AGENTBOX_HOST,
	// falling back to the kernel hostname): where the session process runs, as opposed to the
	// machine id the listener records, which is its own. No delivery reads it. Empty for a
	// session whose plugin predates this field.
	Host string `json:"host,omitempty"`
	// TmuxPlace is where the session's process sits in tmux, as its own client read it from the
	// tmux server it is under (packages/envoy-client's readTmuxPlace). No delivery reads it.
	// Absent for a session in no pane, and for one whose plugin predates the field.
	TmuxPlace *TmuxPlace `json:"tmux_place,omitempty"`
	// Plugin and PluginVersion name the plugin the session registers through (its npm package
	// name) and that plugin's build. Empty for a session whose plugin predates the fields.
	Plugin        string `json:"plugin,omitempty"`
	PluginVersion string `json:"plugin_version,omitempty"`
	// ClientInstanceID is the id the client instance mints at its own start (one Oh My Pi Envoy
	// extension instance, one Claude Code channel-server process) and sends on every
	// registration, so a soft claim naming a predecessor session can tell that predecessor's own
	// instance from another instance's live session. Empty for a client whose plugin predates the
	// field.
	ClientInstanceID string `json:"client_instance_id,omitempty"`
	// Delivery asks for consumer delivery. A registration that carries none, an older plugin's,
	// is refused.
	Delivery *SubscribeDelivery `json:"delivery,omitempty"`
}

// SubscribeDelivery is a registration's delivery object. Consumer asks the listener for the
// session's consumer, relayed on the session's relay subject; Epoch is the plugin's random string
// for its current NATS connection; and Resume, when the session's consumer is missing, seeds the
// new one from the plugin's persisted resume point.
type SubscribeDelivery struct {
	Consumer bool             `json:"consumer,omitempty"`
	Epoch    string           `json:"epoch,omitempty"`
	Resume   *SubscribeResume `json:"resume,omitempty"`
}

// SubscribeResume is a session's resume point: the stream's identity and a sequence below which
// the plugin holds nothing unwritten, so a consumer created from it starts at Seq+1 when StreamID
// is still the stream's. A Seq of 0 is a position too: the plugin holds nothing of the stream, and
// its consumer starts at sequence 1. Seq is nil when the body carries no seq, which names no
// position, so the consumer starts at the first message the stream holds on the session's own
// agent subject, and at the stream's end only when it holds none.
type SubscribeResume struct {
	StreamID string  `json:"stream_id,omitempty"`
	Seq      *uint64 `json:"seq,omitempty"`
}

// SubscribeResponse is the answer to a registration the listener stored: the interest record it
// wrote, the warnings naming each topic it left out, and the session's delivery.
type SubscribeResponse struct {
	Interest
	Warnings []string `json:"warnings,omitempty"`
	// Delivery is the session's delivery, present whenever the registration carried a delivery
	// object.
	Delivery *DeliveryAnswer `json:"delivery,omitempty"`
}

// DeliveryAnswer is the delivery field of a registration's answer. Every registration the
// listener answers at all carries one: the session's consumer is held and relayed, which is the
// only way a message reaches a session.
type DeliveryAnswer struct {
	// Mode is consumer, the one mode a registration is answered.
	Mode string `json:"mode"`
	// Subject is the session's relay subject.
	Subject string `json:"subject,omitempty"`
	// StreamID is the stream's identity: the one a relay frame carries and the plugin's resume
	// point names.
	StreamID string `json:"stream_id,omitempty"`
	// Gap is true when this registration created the consumer at a start below the stream's first
	// sequence: messages it would have delivered are gone.
	Gap *bool `json:"gap,omitempty"`
	// ConsumerStart is the stream sequence the session's consumer starts at, which the plugin
	// keeps as the floor of its resume point: a session that has received nothing holds no point
	// of its own, and with this floor it still asks to resume from where its consumer began
	// rather than from the stream's end, where every message stored while it was away would be
	// lost to it.
	ConsumerStart uint64 `json:"consumer_start,omitempty"`
}

// UnsubscribeRequest is the body of POST /v1/interests/unsubscribe: topics a session hands back.
type UnsubscribeRequest struct {
	SessionID string   `json:"session_id,omitempty"`
	Topics    []string `json:"topics,omitempty"`
	// ClientInstanceID is the process handing these interests back, which is what decides the
	// role claims this release frees: the claims naming that same process and no others
	// (store.RoleReleaseAuthority). A request that leaves it out states no process: it frees no
	// claim, marks this session's claim of each role lane it names as handed back, whether that
	// claim names a process or none, keeps that claim's lane topic in the record, and removes the
	// session's other topics.
	ClientInstanceID string `json:"client_instance_id,omitempty"`
}

// UnsubscribeResponse is the answer to POST /v1/interests/unsubscribe: the topics the listener
// took out of the record, which is not always the topics the request named, since a release that
// may not free a role claim leaves that role's lane topic standing. It is the empty list, never
// null, when nothing was removed.
type UnsubscribeResponse struct {
	Removed []string `json:"removed"`
}

// RoleHolder is GET /v1/roles/<role>'s answer for a role whose holder is live: the role, its
// holder and the holder's state, live on this answer, named exactly as the 404 of a lapsed holder
// (RoleRefusal) and a publish answer name them, the holder's title, directory, machine, host and
// capabilities, its last registration, and the claim's revision. The fields are declared in the
// alphabetical order of their JSON names, which is the order encoding/json wrote the keys of the
// map this answer was before it was a struct, so the answer's bytes did not change; a new field
// keeps that order.
type RoleHolder struct {
	// Capabilities is the empty list, never null, for a holder that advertises none.
	Capabilities []string `json:"capabilities"`
	// ClaimRevision is the role bucket revision of the claim record the resolution read, which a
	// scheduler hands back as POST /v1/roles/set's if_claim_revision, so its claim is refused if
	// the role moved in between.
	ClaimRevision uint64 `json:"claim_revision"`
	// ClientInstanceID is the process the claim names as its holder (store.RoleClaim
	// ClientInstanceID): it is how a client sees that a role is held under its own session id by
	// another process. A claim that names no process - one an earlier build wrote, or one whose
	// claimant stated none - leaves it out, and a client reads an absent field as "no other
	// process holds this role" and an empty one as "another process does".
	ClientInstanceID string `json:"client_instance_id,omitempty"`
	Dir              string `json:"dir"`
	// HandedBack is the hand-back mark the claim carries (store.RoleClaim.HandedBack): the holder
	// gave this role back through a release that could not free the claim, so no registration
	// renews it any more and the role claim reaper frees the role once that renewal has lapsed.
	// It is left out where the claim carries no mark, so a client reads an absent field as "this
	// claim is still its holder's".
	HandedBack  bool         `json:"handed_back,omitempty"`
	Holder      string       `json:"holder"`
	HolderState SessionState `json:"holder_state"`
	Host        string       `json:"host"`
	// LastSeen is the holder's last registration (Unix ms). It is a pointer, which the listener
	// always sets, so a reader tells an answer that names it from one that does not.
	LastSeen  *int64 `json:"last_seen"`
	MachineID string `json:"machine_id"`
	Role      string `json:"role"`
	Title     string `json:"title"`
}

// RoleRefusal is the 404 of a role with no live holder, which GET /v1/roles/<role> answers and a
// role send through POST /v1/messages/publish that kept nothing answers: one with keep false, one
// that names no source session (its Error says so), or one whose keep failed. HolderState says which
// of the three states the claim's holder is in, so a caller can tell a holder that is merely
// unresponsive - stalled, still claiming, still being stored for - from one that is gone.
// ClaimReleased is on every lapsed answer and is always false: a lookup takes nothing from a
// holder, whatever its state, so a caller that used to read this answer as "the role is free now"
// is told plainly that the claim still stands. A listener of an earlier release leaves some of
// the fields out, which decodes as their zero values.
type RoleRefusal struct {
	Error       string       `json:"error"`
	Reason      RoleState    `json:"reason"`
	Holder      string       `json:"holder,omitempty"`
	HolderState SessionState `json:"holder_state,omitempty"`
	// ClientInstanceID is the process the lapsed holder's claim names
	// (store.RoleClaim.ClientInstanceID), as the 200 reports it beside the live holder: it is
	// how a client reading this answer sees that the role is held under its own session id by
	// another process. Absent for a role no claim stands for, and for a claim that names no
	// process - one an earlier build wrote, or one whose claimant stated none.
	ClientInstanceID string `json:"client_instance_id,omitempty"`
	// HandedBack is the hand-back mark the lapsed holder's claim carries
	// (store.RoleClaim.HandedBack), as the 200 reports it beside the live holder: the holder
	// gave this role back through a release that could not free the claim, so no registration
	// renews it any more and the role claim reaper frees the role once that renewal has
	// lapsed, a live window and up to one reap interval later.
	// Absent for a role no claim stands for, and for a claim no such release reached.
	HandedBack bool `json:"handed_back,omitempty"`
	// ClaimRevision is the role's claim revision, as the 200 reports it: the revision of the
	// claim a lapsed holder still holds, and zero for an unclaimed role, so a scheduler
	// reading this 404 can claim the role with if_claim_revision and have that claim refused
	// if the role moved in between.
	ClaimRevision uint64 `json:"claim_revision"`
	// LastSeen is the lapsed holder's last registration in unix milliseconds, absent when the
	// listener can no longer name one.
	LastSeen      *int64 `json:"last_seen,omitempty"`
	ClaimReleased *bool  `json:"claim_released,omitempty"`
}

// RoleSendFailure is the 504 of a role send through POST /v1/messages/publish whose delivery
// window ended with no forward the stream stored for its holder. Reason is delivery_failed: no
// live holder to forward to, a read that failed or ran out of time, or a forward the stream did
// not acknowledge storing (a listener of an earlier release also answered receipt_timeout). Holder
// is the holder the send last resolved, empty when none was; Attempts counts the forwards it made,
// every one the same envelope under one event_id.
type RoleSendFailure struct {
	Error     string `json:"error"`
	Reason    string `json:"reason"`
	EventID   string `json:"event_id"`
	DedupeKey string `json:"dedupe_key"`
	Holder    string `json:"holder"`
	Attempts  int    `json:"attempts"`
}

// CapabilityRefusal is the 403 of a role send whose holder cannot take the payload's delivery
// mode: it does not advertise the mode, or the mode cannot be read unambiguously. Reason is
// capability, and its error reads as /v1/messages/send's answer to the same refusal.
type CapabilityRefusal struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
	Holder string `json:"holder"`
}

// LegionAgentEntry is one Legion session as the listener's Legion agent registry stores it
// (internal/legionagents), and as the Legion daemon's mirror writes it (PUT
// /v1/legion/agents/sessions/<session_id>, and each entry of LegionAgentBatch). Readers ignore
// fields they do not know, so a later writer may add some.
type LegionAgentEntry struct {
	// SessionID is the Oh My Pi or Claude Code session id.
	SessionID string `json:"session_id"`
	// LegionProject is the Legion project token the daemon's role tokens carry (claim.ProjectToken,
	// legionProjectToken in packages/contracts): legion.yaml's project lowercased with every
	// non-alphanumeric dropped. It is not the Dispatch project.
	LegionProject string `json:"legion_project"`
	// Issue is the Dispatch issue key the session works, upper case.
	Issue string `json:"issue"`
	// Role is one of the six workflow roles (legionagents.Roles).
	Role string `json:"role"`
	// RoleToken is legion-<legion_project>-<issue lower-cased>-<role>, byte for byte what
	// claim.NewToken and roleToken build.
	RoleToken string `json:"role_token"`
	// Title is the session's title as stamped (`Legion tester · ACME-71`).
	Title string `json:"title"`
	// Harness is omp or claude.
	Harness string `json:"harness"`
	// StartedAt is when the session started, RFC 3339 UTC.
	StartedAt string `json:"started_at"`
	// RecordedAt is when the listener first recorded the session, RFC 3339 UTC. The listener sets
	// it on every entry it stores and answers; a writer's value is ignored, so a writer leaves it
	// out.
	RecordedAt string `json:"recorded_at,omitempty"`
}

// LegionAgent is an entry with what a read derives from the other sessions of its role token,
// ordered by started_at: the session that replaced it and when, both nil for the newest. Neither
// is stored.
type LegionAgent struct {
	LegionAgentEntry
	ReplacedBy *string `json:"replaced_by"`
	ReplacedAt *string `json:"replaced_at"`
}

// LegionAgentAnswer is one Legion agent registry entry as a read answers it: the entry, the
// session that replaced it within its role token (LegionAgent), whether its session is answering
// now, and whether its project's daemon answers for it. All three are derived at the read and none
// is stored.
//
// Live is the session's own registration state, as every reader of the session registry judges
// it (session.StateOf): true only while that registration is within the live window, never
// membership alone, of the session cache or of the registry. So live false does not say the agent
// has ended: a session past the live window is unresponsive and keeps everything of its own, and
// only a session the registry calls gone has stopped.
//
// Answering is the flag the entry's project last stated in its answering record (LegionProject),
// false while the project has stated none: the condition the listener's hand-off reads on the
// project (its answeringAgent). A reader that sends a message to a stopped agent so that its
// daemon answers it (Dispatch's registeredAgent and unheldRoleTarget) sends it only while this is
// true, since a send the listener hands to no daemon is stored for a session that will not run
// again. A listener that predates the field leaves it out, which decodes as false.
type LegionAgentAnswer struct {
	LegionAgent
	Live      bool `json:"live"`
	Answering bool `json:"answering"`
}

// LegionAgentList is a listing's answer, `[]` when the registry holds no entry.
type LegionAgentList struct {
	Entries []LegionAgentAnswer `json:"entries"`
}

// LegionRoleAgentList is the role read's answer (GET /v1/legion/agents/roles/<role_token>): the
// role token's entries, newest first, the first being its current holder, and Answering, the flag
// the role token's own project last stated in its answering record, false while the project has
// stated none. It is written out whether the read holds entries or not, so a reader learns whether
// a daemon answers for a role the registry holds no entry for: Dispatch keeps a message for such a
// role for its first holder only while one does (unheldRoleTarget). An edge listener answers it
// only for a role of one of its token's projects, and false for any other. A listener that
// predates the field leaves it out, which decodes as false.
type LegionRoleAgentList struct {
	LegionAgentList
	Answering bool `json:"answering"`
}

// LegionAgentBatch is the body of PUT /v1/legion/agents, a batch of at most 256 entries
// (legionagents.MaxBatch) in at most 1 MiB, and its 200's answer: the entries as stored, each
// with its recorded_at.
type LegionAgentBatch struct {
	Entries []LegionAgentEntry `json:"entries"`
}

// LegionProjectRequest is the body of PUT /v1/legion/projects/<legion_project>: the project's
// answering record, which the Legion daemon writes at boot and every minute (its agent registry
// mirror's heartbeat). Answering is a pointer because the listener refuses a body that leaves it
// out, 400, rather than read it as false. UpdatedAt is RFC 3339 UTC.
type LegionProjectRequest struct {
	Answering *bool  `json:"answering,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// LegionProject is a Legion project's answering record as the registry stores it, under the key
// project.<legion_project>, and as PUT /v1/legion/projects/<legion_project> answers it: whether
// the project's daemon answers for its stopped agents, and when it last said so (RFC 3339 UTC).
// The listener hands the project's messages to the daemon while Answering is true, whether or not
// the daemon runs now: only the flag it last stated counts, never the record's age, so a daemon
// that stated true and stopped still gets its agents' messages, which JetStream holds until it
// reads them. UpdatedAt is for an operator's reading.
type LegionProject struct {
	Answering bool   `json:"answering"`
	UpdatedAt string `json:"updated_at"`
}

// LegionHandoff is what the listener publishes, on LegionHandoffSubject(<legion_project>,
// <session_id>), for a message no session took that a stopped Legion agent's daemon answers in its
// place: the topic the message was for (the agent's own, or its role's), the agent session it is
// for (the session addressed, or the role's newest), that session's role token and Legion project
// token, and the envelope exactly as it would have been delivered to the agent. It is
// LegionAgentHandoffSchema (packages/contracts/src/legion-handoff.ts), which the daemon's
// hand-off reader decodes (intake.Handoff in packages/daemon).
type LegionHandoff struct {
	OriginalTopic string   `json:"original_topic"`
	TargetSession string   `json:"target_session"`
	RoleToken     string   `json:"role_token"`
	Project       string   `json:"project"`
	Envelope      Envelope `json:"envelope"`
}

// LegionHandoffSubject is the subject the listener publishes a LegionHandoff for session on,
// notifications.legion.<project>.agents.<session>, project being the Legion project token role
// tokens carry (not the Dispatch project key). The notification stream retains it, and the
// project's daemon reads every hand-off through one durable consumer filtered to
// LegionHandoffSubject(project, ">") (intake.OpenHandoffs in packages/daemon). It is
// legionAgentHandoffSubject in packages/contracts/src/legion-handoff.ts.
func LegionHandoffSubject(project, session string) string {
	return "notifications.legion." + project + ".agents." + session
}
