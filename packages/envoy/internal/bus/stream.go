package bus

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/natsversion"
)

const Stream = "ENVOY_NOTIFICATIONS"

// StreamIDMetadataKey is the stream configuration's metadata key holding the stream's identity:
// set once, when the stream is created or when a start first finds it without one, and never
// changed after. It lives in the configuration, so a backup and restore, a reconcile and a leader
// election all keep it. The stream's Created time is no identity: a restore changes it, and after
// a clustered restore the servers can report different Created times (per nats-server's clustered
// restore code), so it can change again at a later leader election. Every relay frame carries the
// key's value in its Envoy-Stream-Id header (cmd/listener/session_relay.go), so a session can tell
// a cursor of its own apart from one belonging to a stream that was restored.
const StreamIDMetadataKey = "envoy_stream_id"

// The JetStream KV buckets Envoy's stores keep, each named for what it holds.
const (
	// InterestBucket holds each session's interests, the topics it subscribes to, keyed by session
	// id (internal/store).
	InterestBucket = "envoy_interests"
	// RoleClaimBucket holds each role lane's claim, its holder session, keyed by role
	// (internal/store).
	RoleClaimBucket = "envoy_roles"
	// SessionBucket holds each live session's registration, keyed by session id
	// (internal/session).
	SessionBucket = "envoy_sessions"
	// KnownSessionBucket holds one record per session that registered in the last 30 days, keyed
	// by session id (internal/session). A record says the session was known in the last 30 days,
	// never that it is registered now: it outlives both stores that answer for a session that
	// runs, the session entry (SessionBucket) and the interest row (InterestBucket), so a send to
	// a stopped session is queued rather than refused and a publish counts that session as a
	// recipient. The code that needs a live session or a role holder therefore never reads it:
	// role resolution (roleHolderSessions.Holder in cmd/listener/role_holder.go) and the reapers
	// each ask the stores whose records end when the session is gone.
	KnownSessionBucket = "envoy_known_sessions"
	// CIStateBucket holds the CI state of each commit of a pull request (internal/cistore).
	CIStateBucket = "envoy_ci_state"
	// LegionAgentBucket holds the Legion agent registry: each Legion session the daemon ran, under
	// session.<id>, and the same entry under its issue (issue.<ISSUE>.<id>) and its role token
	// (role.<role_token>.<id>), never deleted (internal/legionagents).
	LegionAgentBucket = "legion_agents"
	// DPoPProofBucket holds one record per RFC 9449 proof a listener has already spent, keyed by
	// the proof's 'jti' and target URI, so a captured proof presented a second time inside its
	// acceptance window is refused by whichever listener of a deployment receives it
	// (cmd/listener/brokerauth.go). Its records are the only ones Envoy keeps on a bucket TTL
	// alone: nothing but a create of the same key reads a record back, and a key that has outlived
	// the window its proof could be presented in says nothing, so each key expires rather than being
	// deleted, except the record an edge listener deletes to give a proof back after a 503 it
	// answered before serving anything (proofSpend.release in cmd/listener).
	DPoPProofBucket = "envoy_dpop_proofs"
	// WebhookRedeliveryBucket holds the cursor and records of Dispatch's sweep of the GitHub App
	// webhook's failed deliveries (internal/dispatch/redeliver).
	WebhookRedeliveryBucket = "envoy_webhook_redelivery"
	// PrivateProjectsBucket holds one key per private project, its value the repositories
	// Dispatch names for it, and a key exists exactly while its project is private: Dispatch writes
	// it and the listeners read it to hold each session to its token's projects
	// (dispatch://INFRA-3862). It is deliberately in no list of the buckets a static grant
	// reaches (ListenerBuckets): only the auth service grants its calls, so a hub that runs none
	// refuses both its write and its read, which both sides read as no project being private.
	PrivateProjectsBucket = "envoy_private_projects"
)

// ListenerBuckets reports the KV buckets envoy-listener opens, in the order its NATS grant lists
// them: the interest and role-claim buckets (internal/store), the session registry and the
// known-session store (internal/session), the Legion agent registry (internal/legionagents), the
// CI state bucket, which only a listener with the GitHub webhook route opens (internal/cistore),
// and the spent-proof bucket, which every listener opens at its start so an edge listener, whose
// grant creates no bucket, finds it (cmd/listener/brokerauth.go). The listener's keeper raises
// each of them (WithKeptBuckets), and envoy-infra grants the listener and the host each one's
// calls (packages/envoy/infra/core/permissions.go), so a bucket named here is both kept and
// granted.
// A bucket named here is granted to the host and Dispatch users too; the note on that file's
// ListenerBuckets loop (listenerGrant) says where what that widens is written down.
// Each call returns a fresh slice, so no caller changes another's list.
func ListenerBuckets() []string {
	return []string{InterestBucket, RoleClaimBucket, SessionBucket, KnownSessionBucket, LegionAgentBucket, CIStateBucket, DPoPProofBucket}
}

// NotificationTopicPrefix begins every notification topic: the subjects the stream carries, the
// role lanes and the exception lanes.
const NotificationTopicPrefix = "notifications."

// ExceptionTopicPrefix begins a delivery's exception lane: a failed or refused delivery is reported
// on ExceptionTopicPrefix followed by its original topic.
const ExceptionTopicPrefix = NotificationTopicPrefix + "envoy.exceptions."

// LegionWakeTopic is the one topic the listener reports a wake of a released Legion issue pod
// on: a message published to a role topic with no live holder, a message sent to the agent
// subject of a session that is gone, and the listener's own `listener_ready`. It is a new leaf
// under notifications.legion.>, which the stream already carries, so it is retained with no
// stream change and no new grant. Only the listener sees these, and it does not know which
// daemon owns a session: every Legion daemon consumes the topic on a durable of its own and acts
// only on a released claim of its own project.
const LegionWakeTopic = NotificationTopicPrefix + "legion.wake"

// DispatchDeliveryDurable is the stream's durable consumer Dispatch's delivery intake binds, over
// every GitHub notification (internal/dispatch/delivery).
const DispatchDeliveryDurable = "delivery-events"

// DispatchJobDurable is the stream's durable consumer Dispatch's job intake binds, over a deploy
// run's job events (internal/dispatch/delivery).
const DispatchJobDurable = "delivery-jobs"

// ListenerDurable is the durable consumer of the stream that the listener of machine machineID
// binds. A space in the machine id is a hyphen in the name.
func ListenerDurable(machineID string) string {
	return "listener-" + strings.ReplaceAll(machineID, " ", "-")
}

// ListenerQueue is the queue group the listener of machine machineID subscribes the role lanes in,
// so NATS hands each role message to one of the machine's listeners while a rolling deploy runs two.
// A space in the machine id is a hyphen in the name.
func ListenerQueue(machineID string) string {
	return "envoy-listener-" + strings.ReplaceAll(machineID, " ", "-")
}

// RoleLaneQueue is the queue group every listener subscribes the role lanes in, whatever its
// machine, so NATS hands each role message to one listener and the holder's stream copy is
// forwarded once. A listener whose NATS user may not subscribe in it (a hub whose grant predates
// it) subscribes in its machine's group, ListenerQueue, instead (ConnectRoleLane).
const RoleLaneQueue = "envoy-roles"

// DeliverSubjectPrefix begins the relay subject a listener publishes a session's consumer
// messages on, DeliverSubject. No stream subject captures it (StreamCaptures), so a relay frame
// travels over core NATS and the stream stores nothing twice.
const DeliverSubjectPrefix = NotificationTopicPrefix + "envoy.deliver."

// DeliverSubject is the relay subject of session sessionID: the subject the listener that owns the
// session publishes each message of its consumer (SessionConsumerName) on, and the session
// subscribes to. Every message a session on NATS gets travels on it: no session takes a message
// from a plain NATS subscription of its own (packages/envoy/docs/architecture.md, "Session
// consumers" under "Runtime shape").
func DeliverSubject(sessionID string) string {
	return DeliverSubjectPrefix + sessionID
}

// RelayReplyPrefix begins the receipt family of the edge listeners: an edge machine's relay frame
// receipts are this prefix and its machine id, then a nuid, the session id and a token
// (relayreply.<machine>.<nuid>.<session id>.<token>; natsgrant.EdgeRelayReplyPrefix).
// Its grant subscribes only its own machine's and publishes every machine's, since the listener
// serving a session's relay socket answers on the family of the machine holding the session's
// consumer (natsgrant.Edge). The edge's session relay names its frames' receipts here, and the
// relay socket publishes each receipt its client sends on the subject the frame named
// (cmd/listener's startSessionRelays and relay_socket.go). No stream subject captures it
// (StreamCaptures), so a receipt travels over core NATS, as the frame does, and the stream stores
// none.
const RelayReplyPrefix = NotificationTopicPrefix + "envoy.relayreply."

// SessionReplySubject is the reply subject of one request made of session sessionID,
// <inbox>.<session id>.<token>: inbox a reply inbox of the requester's own (nats.NewInbox), then
// the session's id, then a token of 128 random bits (crypto/rand.Text, 26 base32 characters).
// Every request a session answers carries one - the relay's frames (cmd/listener's
// sessionRelays), the listener's connection probe and Dispatch's agent stream replay
// (internal/dispatch/agentstream). The id is there so that a grant can admit a session's own
// replies alone (SessionReplies) and the relay can refuse a reply that names another session
// than its frame's; the token is random so that no session can guess the subject of a request
// made of another, since it is derived from nothing a session can see, and a session's grant
// reads no other session's relay or control subject. A session's own grant, the one the NATS auth
// service gives it (natsgrant.Session), publishes SessionReplies in place of every _INBOX.
// subject, so a session on that grant can neither answer a request made of another session nor
// guess the subject of one. The relay moves a frame only on a reply whose subject names the
// session the frame was sent to (sessionRelays.onReply), and logs the first reply that names
// another at WARN, then at most one a minute. A holder of the shared session user or of a host
// user reads every session's relay subject and may publish to every _INBOX. subject, so none of
// this holds against it: the relay reads the session a reply subject names, not who published the
// reply. sessionID is one subject token, as every session with a consumer's is
// (SessionConsumerName).
func SessionReplySubject(inbox, sessionID string) string {
	return inbox + "." + sessionID + "." + rand.Text()
}

// SessionReplies is the one publish entry under _INBOX. a grant of session sessionID needs: it
// admits every reply subject SessionReplySubject makes for that session, and none it makes for
// another, nor a requester's plain inbox (_INBOX.<id> or nats.go's _INBOX.<id>.<token>).
//
// sessionID must be one subject token: no dot, no wildcard (* or >) and no whitespace, the rule
// SessionConsumerName holds a session's id to. SessionReplies does not check it, so its caller
// must: an id of * admits every session's replies, and since the entry ends in >, the entry of an
// id also admits the replies of every id that extends it by a dot (a's entry admits a.b's, and
// a.b's admits a.b.c's), so no session id may hold a dot. An id such as a* is one plain token and
// admits nothing extra. The one builder of a session grant checks it first: natsgrant.Session
// calls natsgrant.SessionToken before it calls SessionReplies, and the NATS auth service refuses
// such an id as bad_session_id (natscallout's decideSession).
func SessionReplies(sessionID string) string {
	return nats.InboxPrefix + "*." + sessionID + ".>"
}

// ConnectionProbeHeader is the one header the listener's connection probe carries on a session's
// relay subject (DeliverSubject), with the value "1", and no body: a core request whose answer, or
// a second of silence, says something still subscribes the subject, and whose no-responders says
// nothing does (cmd/listener/connection_probe.go). It is not a relay frame header (Envoy-Subject
// and the rest, cmd/listener/session_relay.go): the probe carries none of those, since a message on
// the relay subject that carried them would be read as a frame and move the session's resume point
// past the messages it stands for. Every subscriber of a relay subject that stands for a session -
// the plugins, and each stand-in for one in the tests, the load test and the end-to-end scripts -
// answers a message carrying this header done, when it carries a reply subject, before it decodes
// anything, and takes nothing else from it: no frame, no envelope, no count. The plugins answer it
// in their shared consumer delivery (packages/envoy-client/src/consumer-delivery.ts,
// RelayHeader.probe); one older than the header answers the same bytes, which decode as no frame,
// done too. A subscriber that is no session and holds the subject only through a wildcard -
// natstail on notifications.>, an end-to-end capture of every notification - passes it over
// unanswered and takes nothing from it either: an answer would speak for a session it is not.
const ConnectionProbeHeader = "Envoy-Probe"

// KeptTopicPrefix begins the listener's kept lane: the role messages it kept because it could not
// deliver them and the drain cursors over them (cmd/listener/role_keep.go). The listener alone
// publishes under it and reads it, no session may register a topic under it
// (ReachesPrivateStreamSubject), and no consumer of the notification stream delivers one: the
// stream carries the prefix (streamCarriedSubjects) where no consumer filter does
// (StreamSubjects).
const KeptTopicPrefix = NotificationTopicPrefix + "envoy.kept."

// KeptRoleSubjectPrefix begins the kept lane of every role (KeptRoleSubject): what a reader of all
// of them filters the stream's subjects on and reads each role's own token back out of
// (Client.KeptRoleLanes).
const KeptRoleSubjectPrefix = KeptTopicPrefix + "role."

// KeptRoleSubject is the kept lane of role: the role messages the listener could not deliver, each
// held until the role is claimed and then forwarded to the new holder. The role name is written
// into the segment as contracts.RoleTopicPrefix writes it, unsanitized, so a role's kept subject
// follows from its role topic.
func KeptRoleSubject(role string) string {
	return KeptRoleSubjectPrefix + role
}

// KeptRoleCursorSubject is the drain cursor of role: how far the listener has read that role's
// kept lane, so a kept message already forwarded is not forwarded again. The role name is written
// into the segment as contracts.RoleTopicPrefix writes it, unsanitized, so a role's cursor subject
// follows from its role topic.
func KeptRoleCursorSubject(role string) string {
	return KeptTopicPrefix + "cursor." + role
}

// streamDuplicateWindow covers the entire retained notification lifetime, so
// an outbox retry after a crash before published_at is recorded cannot create a
// second retained Dispatch event while the original remains observable, and a
// webhook redelivery of an event the stream already holds adds no second copy.
// GitHub redelivers only deliveries from the past three days, so its whole
// redelivery horizon falls inside this window.
//
// It is also the window of the dashboard's retry promise, which the stream holds only in part: it
// keeps one copy of a repeat and reports it duplicate, while a core-NATS subscriber is handed the
// repeat anyway and drops it itself. The promise, and the part each side plays, is stated once on
// DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts, from which contracts.DeliveryDuplicateWindow
// is generated.
const streamDuplicateWindow = contracts.DeliveryDuplicateWindow

// streamSubjects are the notification subjects the stream carries, and the list every consumer
// filter over them takes (StreamSubjects). The stream carries two subjects more, the listener's
// own kept and resume lanes (listenerOwnStreamLanes, streamCarriedSubjects).
var streamSubjects = []string{
	"notifications.agent.>",
	"notifications.dispatch.>",
	"notifications.github.>",
	"notifications.slack.>",
	// The Go Legion daemon's notices for the project's controller
	// (notifications.legion.<project>.controller); its workflow notices go to each owning
	// architect's role topic.
	"notifications.legion.>",
	"notifications.ghostwispr.>",
	"notifications.whatsapp.>",
	// A team's own topic, the one namespace an agent names itself
	// (notifications.team.deploy-squad): the listener's publish route stores it so a session
	// that was not connected reads it from its consumer later, and refuses by name every other
	// notifications. topic no subject here captures (cmd/listener/api.go).
	"notifications.team.>",
	ExceptionTopicPrefix + contracts.AgentTopicPrefix + ">",
	ExceptionTopicPrefix + "notifications.team.>",
}

// listenerOwnStreamLanes are the subjects the stream carries for the listener itself: the kept
// lane, holding the role messages it could not deliver and the per-role drain cursors over them
// (KeptTopicPrefix), the resume lane, holding where each deleted session consumer stopped
// (SessionResumeSubjectPrefix), and the takeover lane, holding each role a session took from
// another and how far the copy of the other's role messages has gone (TakeoverSubjectPrefix).
// Each is the stream's alone and no consumer filter takes one (consumerFilterSubjects): a kept
// copy still carries its role topic in the envelope, so a consumer handed one would deliver it as
// a role message and re-trigger role delivery on every listener, and a drain cursor, a resume
// record and a takeover record are no envelope at all. The listener reads the lanes itself instead
// (cmd/listener/role_keep.go, cmd/listener/role_takeover.go, SessionConsumers). A lane added here
// is carried by the stream and kept out of every consumer's filter by that one list.
var listenerOwnStreamLanes = []string{KeptTopicPrefix + ">", SessionResumeSubjectPrefix + ">", TakeoverSubjectPrefix + ">"}

// ListenerOwnLanes reports the lanes the stream carries for the listener itself
// (listenerOwnStreamLanes), each as the prefix every topic of that lane begins with
// (`notifications.envoy.kept.`) rather than the `>` pattern the stream carries it as. That is the
// shape a caller names a lane in and builds a topic of one with: the publish route's refusal
// names the lane it refused, and the row that tests that refusal is the lanes themselves
// (cmd/listener), so a lane the stream gains is named and covered with nothing written out a
// second time. A caller judging one topic asks ListenerOwnLaneOf instead, which reads the same
// list.
func ListenerOwnLanes() []string {
	lanes := make([]string, len(listenerOwnStreamLanes))
	for i, lane := range listenerOwnStreamLanes {
		lanes[i] = listenerOwnLanePrefix(lane)
	}
	return lanes
}

// ListenerOwnLaneOf is the lane of the listener's own that topic lies under, spelled as
// ListenerOwnLanes spells it, and "" for a topic under none. It is the one test a caller makes
// of such a topic, and the name it puts in a refusal: the publish route refuses one, since a
// session writing a drain cursor or a resume floor would take over a role's lane or move
// another session's delivery (cmd/listener/api.go). So a lane added to listenerOwnStreamLanes
// is covered wherever the question is asked, and no caller lists a lane of its own.
func ListenerOwnLaneOf(topic string) string {
	for _, lane := range listenerOwnStreamLanes {
		if SubjectContains(lane, topic) {
			return listenerOwnLanePrefix(lane)
		}
	}
	return ""
}

// listenerOwnLanePrefix is lane, a subject pattern of listenerOwnStreamLanes, as the prefix every
// topic of that lane begins with: the pattern without its trailing `>`.
func listenerOwnLanePrefix(lane string) string {
	return strings.TrimSuffix(lane, ">")
}

// streamCarriedSubjects is every subject the stream itself carries: the notification subjects
// and the listener's own lanes (listenerOwnStreamLanes).
var streamCarriedSubjects = slices.Concat(streamSubjects, listenerOwnStreamLanes)

// privateStreamSubjects are the subjects of streamSubjects a topic under which ever carries one
// agent session's private payload, read from the code that publishes it. A session may register
// no interest reaching one but its own agent subject and its own project's controller notice lane
// (ReachesPrivateStreamSubject, which POST /v1/interests/subscribe applies), since a registered
// interest is any topic a caller names and the stored topics drive both the listener's fan-out and
// the session's own consumer filter. Every other stream subject is deliberately absent, which
// TestEveryStreamSubjectIsClassifiedForInterestRegistration names one by one, so a new stream
// subject carrying a session's payload cannot be added without listing it here.
var privateStreamSubjects = []string{
	// Each session's own inbox: Dispatch's routes, follower and author copies, and every
	// message sent to one session (contracts.AgentSubject, and contracts.LegacyAgentSubject in
	// the old form).
	contracts.AgentTopicPrefix + ">",
	// The Legion daemon's notices for one project's controller (daemon notify.ControllerTopic),
	// which only the session holding that project's controller role may register
	// (ControllerNoticeRole), and no other session may.
	contracts.LegionNoticeTopicPrefix + ">",
	// A failed delivery's report, carrying the undelivered message's payload and the session it
	// was for (cmd/listener publishDeliveryException); a session's own lane included.
	ExceptionTopicPrefix + contracts.AgentTopicPrefix + ">",
	// A failed delivery on a team topic, carrying that message's payload and the session it
	// was for, as the agent lane above does; only the exception's own reader may have it.
	ExceptionTopicPrefix + "notifications.team.>",
	// A role message the listener could not deliver, carrying that message's payload and the
	// holder it was meant for, and the per-role drain cursor over them: the listener alone
	// reads the lane, and no session may register a topic under it.
	KeptTopicPrefix + ">",
	// The resume floor of a session whose consumer was deleted, which names the session and
	// the stream sequence its delivery stopped at (SessionResumeSubject): no session may
	// register a topic under it and no replay hands one out.
	SessionResumeSubjectPrefix + ">",
	// A role a session took from another, which names both sessions and how far the copy of the
	// previous holder's role messages has read (TakeoverSubject): the listener alone reads the
	// lane, and no session may register a topic under it.
	TakeoverSubjectPrefix + ">",
}

// ControllerNoticeRole reports the Legion role token whose holder topic's notices are for, and
// whether topic is a controller notice lane at all: project <p>'s lane is
// contracts.LegionNoticeTopicPrefix + <p> + contracts.ControllerNoticeSuffix and its role token
// is contracts.ControllerRoleToken(<p>), all three generated from legionControllerNoticeSubject
// and controllerToken in packages/contracts (scripts/gen-go.ts), so a rename there reaches this
// package rather than a hand copy of it. Both are read here from the one project token the lane
// names, so no caller builds the pair a second time. Only a literal lane names a role: a wildcard
// token, an empty project, a longer subject, and the issue notices of the same family
// (notifications.legion.<p>.<issue key>) name none.
func ControllerNoticeRole(topic string) (string, bool) {
	project, found := strings.CutPrefix(topic, contracts.LegionNoticeTopicPrefix)
	if !found {
		return "", false
	}
	project, found = strings.CutSuffix(project, contracts.ControllerNoticeSuffix)
	if !found || project == "" || strings.ContainsAny(project, ".*>") {
		return "", false
	}
	return contracts.ControllerRoleToken(project), true
}

// ControllerNoticeLane is ControllerNoticeRole read backwards: the notice lane whose holder role
// is, and whether role is a Legion project's controller role at all. The project is read out of
// role by the shape contracts.ControllerRoleToken gives it, taken from that func itself, and the
// lane built from it must name role again, so a role token of any other shape names no lane.
func ControllerNoticeLane(role string) (string, bool) {
	prefix, suffix, _ := strings.Cut(contracts.ControllerRoleToken("\x00"), "\x00")
	project, found := strings.CutPrefix(role, prefix)
	if !found {
		return "", false
	}
	if project, found = strings.CutSuffix(project, suffix); !found {
		return "", false
	}
	lane := contracts.LegionNoticeTopicPrefix + project + contracts.ControllerNoticeSuffix
	if named, isLane := ControllerNoticeRole(lane); !isLane || named != role {
		return "", false
	}
	return lane, true
}

// NamesControllerNoticeLane reports whether any of topics is a controller notice lane
// (ControllerNoticeRole). It is the one question a caller asks before reading a controller role's
// claim: that lane is the only private stream subject whose owner ReachesPrivateStreamSubject
// reads the session's roles for, so a topic set naming none needs no claim read
// (store.Registry's controllerLaneRoles).
func NamesControllerNoticeLane(topics []string) bool {
	return slices.ContainsFunc(topics, func(topic string) bool {
		_, lane := ControllerNoticeRole(topic)
		return lane
	})
}

// ReachesPrivateStreamSubject reports whether a topic sessionID registers as an interest reaches a
// private stream subject (privateStreamSubjects) that is not its own: it overlaps one under NATS
// wildcard rules (subjectsOverlap), so another session's inbox, a failed-delivery lane (a session's
// own included), another project's controller notice lane, and wildcards reaching any of them
// (notifications.>, notifications.*.<x>, notifications.envoy.>) count. Two literal subjects are a
// session's own: its agent subject, in either form (contracts.LegacyAgentSubject, and
// contracts.AgentSubject under a project, NoProject or a wildcard over every project, whose old
// form contracts.LegacyTwin reads back), and the controller notice lane of a project whose
// controller role roles names (ControllerNoticeRole), which its holder registers because a session
// receives what its own consumer delivers and nothing else, the lane's core subscription having
// gone with the rest of that receive path (INFRA-771). roles is what the session holds, as each
// controller role's own claim record says (store.Registry's controllerLaneRoles, which reads the
// record of every lane's role from the claim bucket rather than the listener's copy of the
// claims), so a session that lost the role loses the lane at its next registration. A session id
// holding `*` or `>` makes a wildcard of its "own" agent subject, which is no session's alone and
// counts too.
func ReachesPrivateStreamSubject(sessionID, topic string, roles []string) bool {
	if !OverlapsPrivateStreamSubject(topic) {
		return false
	}
	if role, lane := ControllerNoticeRole(topic); lane {
		return !slices.Contains(roles, role)
	}
	own := contracts.LegacyAgentSubject(sessionID)
	twin, _ := contracts.LegacyTwin(topic)
	return (topic != own && twin != own) || strings.ContainsAny(sessionID, "*>")
}

// OverlapsPrivateStreamSubject reports whether topic overlaps a private stream subject
// (privateStreamSubjects) under NATS wildcard rules, whoever's that subject is: the first question
// ReachesPrivateStreamSubject asks, so a topic it answers false for reaches no session's private
// subject, whatever session or roles a caller would weigh it for.
func OverlapsPrivateStreamSubject(topic string) bool {
	return slices.ContainsFunc(privateStreamSubjects, func(private string) bool { return subjectsOverlap(topic, private) })
}

// StreamSubjects reports the notification subjects the stream carries, excluding the role lanes
// and the listener's own lanes, the kept one and the resume one: the list every consumer filter
// built over the notification subjects uses. The stream's own list, which carries both of those
// lanes too, is StreamCarriedSubjects.
func StreamSubjects() []string {
	return slices.Clone(streamSubjects)
}

// StreamCarriedSubjects reports every subject the stream itself carries (streamCarriedSubjects):
// the notification subjects and the listener's own kept and resume lanes. A caller that builds or
// checks the stream takes it, where a consumer's filter takes StreamSubjects.
func StreamCarriedSubjects() []string {
	return slices.Clone(streamCarriedSubjects)
}

// StreamCaptures reports whether the stream retains a publish to topic: some subject this binary
// compiles into the stream matches it. Every such subject is a literal prefix followed by `.>`
// (TestEveryStreamSubjectIsALiteralPrefixAndWildcard), which matches a literal topic exactly when
// the topic extends that prefix by at least one token, so no subject is split. A topic is read
// literally: a `*` or `>` in it is a character of the topic, not a wildcard, which is what nats.go
// publishes it as.
func StreamCaptures(topic string) bool {
	for _, subject := range streamCarriedSubjects {
		prefix := strings.TrimSuffix(subject, ">")
		if len(topic) > len(prefix) && strings.HasPrefix(topic, prefix) {
			return true
		}
	}
	return false
}

// subjectsCapture reports whether a stream carrying subjects retains a publish to topic: some
// subject matches it, token by token, a `*` matching one token and a trailing `>` one or more. It
// takes any subject shape, since the subjects it is given are the deployed stream's, which a newer
// build may have shaped otherwise than StreamCaptures assumes. A topic is read literally, as
// StreamCaptures reads it.
func subjectsCapture(subjects []string, topic string) bool {
	topicTokens := strings.Split(topic, ".")
	return slices.ContainsFunc(subjects, func(subject string) bool {
		subjectTokens := strings.Split(subject, ".")
		for index, token := range subjectTokens {
			if token == ">" {
				return len(topicTokens) > index
			}
			if index >= len(topicTokens) || (token != "*" && token != topicTokens[index]) {
				return false
			}
		}
		return len(subjectTokens) == len(topicTokens)
	})
}

// readStreamSubjects reads the subjects the deployed stream carries, which may hold subjects this
// build does not compile: a newer build adds its own, and reconciliation keeps them.
func readStreamSubjects(js nats.JetStreamContext) ([]string, error) {
	info, err := js.StreamInfo(Stream)
	if err != nil {
		return nil, err
	}
	return info.Config.Subjects, nil
}

// streamConfigGeneration is the generation of streamCfg's settings, stamped on the stream as its
// streamGenerationKey metadata. Every change to streamCfg's MaxAge, Duplicates, Discard or
// Subjects takes a new generation and a new row in
// TestTheStreamConfigEqualsItsGenerationsPinnedRow. A build that finds a higher generation
// deployed leaves the stream as it is, at the start (ensureStreamWithConfig) and in the keeper
// (keepStream), so while two builds run, the older one never reverts what the newer one set.
// The consumer cap (streamMaxConsumers) takes none: like the copies and the byte limit it is
// only ever raised, so two builds that compile different caps never lower each other's.
const streamConfigGeneration = 5

// streamGenerationKey is the stream metadata key that holds the generation of its settings. A
// stream without it was last set by a build before generations: generation 0.
const streamGenerationKey = "dispatch.stream-generation"

// streamMaxConsumers is the consumer cap this binary compiles into the stream (streamCfg), the one
// setting that bounds how many sessions can hold a session consumer of their own. What a stream
// with no cap means to the server changed in nats-server 2.15. From that release the server caps
// a stream with no cap, a deployed 0 and the -1 it answers for one alike, at a default of its
// own, and that default is the server's own rather than an account's: the server-wide
// JetStreamLimits.DefaultMaxConsumers, which a server whose configuration leaves it unset starts
// at JSDefaultMaxConsumersPerStream, 1000 (server/opts.go:6255-6256 in v2.15.1, the constant at
// server/jetstream_api.go:427). Such a stream fills at 1000 consumers, and every session after
// that is refused its own (`maximum consumers limit reached`, code 10026) and falls back to core
// delivery. Before 2.15 a stream with no cap has no limit at all: v2.10.29 normalises a deployed
// 0 to -1 (server/stream.go:1227) and checks the cap only while it is positive
// (server/consumer.go around 808). The hub runs 2.15.1, where the default of 1000 is the failure
// this cap answers.
//
// That 1000 is what the version gate stands for: serverDefaultsAnUnsetCap reads "nats-server 2.15
// or later" and this binary takes it to mean "a stream with no cap is capped at 1000". An
// operator can break that link two ways, and the binary checks neither. A server started with
// `jetstream { limits { default_max_consumers: <n> } }` runs with <n> in place of the 1000
// (server/opts.go:390 and :2555 in v2.15.1); at -1, or at any number above this cap, a stream
// with no cap allows more consumers than this binary compiles, and a start and the keeper then
// lower what the server allows to 24000 rather than raise it. An account whose own JetStream
// `max_consumers` is positive has that applied to every stream of the account
// (server/consumer.go:1162-1171): above this cap, a start and the keeper lower an uncapped
// stream's limit to 24000 the same way; below it, the server refuses a stream whose cap is above
// the account's (server/jetstream.go:2560-2562), so the create and the raise are both refused. An
// account limit of -1 counts as not set and changes nothing, since the server applies its default
// over it (server/consumer.go:1166-1168). No deployment of this repository sets either: the hub's
// rendered jetstream block carries max_file_store, max_memory_store and store_dir and nothing else
// (infra/gcp/clusteredhub_live_test.go), and it renders no accounts block, so every client runs
// in the global account with its JetStream limits unset.
//
// The population the cap must cover is not the live session count. The measurement below was
// taken while nothing deleted a session's consumer when the session ended, the only delete being
// inside SessionConsumers.RestartAtFloor (session_consumers.go), and the server removing a
// consumer SessionConsumerInactiveThreshold after its last pull, which was then 72 hours. The
// population was therefore every distinct session that pulled inside those 72 hours. It was read
// twice on 2026-11-06, by sampling the listener's session list every 20 seconds and counting each
// id that appeared in no earlier read, which needs no assumption about how long a session lives:
// 238 reads from 01:53:37Z to 02:33:35Z gave 42 new ids, 63.1 an hour, 36 of them stamped inside
// the window by their UUIDv7, 54.0 an hour, against 663 live; 136 reads from 04:23:40Z to
// 05:09:16Z gave 61 new ids, 80.3 an hour, 57 of them stamped inside it, 75.0 an hour, against
// 668 live. That population was live + 72 x rate: 4,551 to 5,206 by the first reading and 6,068
// to 6,447 by the second.
//
// What bounds the population today is not that threshold but two deletes it only backs up
// (SessionConsumerInactiveThreshold): a deregistration deletes the session's consumer
// (cmd/listener/api.go) and the sweep deletes the consumer of every session the registry judges
// gone, at most 256 a pass (cmd/listener/session_consumer_sweep.go), so the population is the
// live sessions, plus the sessions that ended without deregistering, which keep their consumers
// about the gone horizon plus one ENVOY_SESSION_REAP_INTERVAL plus one sweep interval after
// their last registration - about 60 to 85 at the rates above - plus the consumers a hub still
// holds from before the sweep existed.
//
// dispatch://INFRA-514's capacity gate asks for at least twice the larger of the stream's consumer
// count and that measured population, so the gate's own number is 9,102 to 12,894 across the two
// readings. 24000 is 1.9 to 2.6 times the gate's number and 3.7 to 5.3 times that population. The
// reading sets it: at the fleet's growth between the two, a cap only 1.2 to 1.3 times the gate's
// own number would need raising again within a release or two, and the cap costs nothing while it
// is not reached, since the server allocates per live consumer. It costs what its consumers cost
// when they do reach it, measured twice on nats-server 2.15.1, 2,000 consumers of the listener's
// own shape (session_consumers.go) on a file-backed stream: 54.7 and 52.6 KiB resident and 0.56
// KiB of store per consumer, so 24000 consumers are about 1.20 to 1.25 GiB of memory and about 13
// MiB of store, against the hub's 16 GiB machine, where nats-server held 1.10 GiB resident on
// 2026-10-30.
const streamMaxConsumers = 24000

// streamMaxConsumersFloor is the first nats-server that takes a change of MaxConsumers on a stream
// that already exists. Every server before it refuses such an update with maxConsumersRefusal,
// measured on v2.10.29, v2.11.0 and v2.12.0; v2.12.5 and later take it. A create carries the cap
// on every version, so under an older server the stream holds the cap only from the create, and a
// refused raise names the floor (refusedSettings).
const streamMaxConsumersFloor = "v2.12.5"

// streamMaxConsumersDefaultMajor and streamMaxConsumersDefaultMinor are the first nats-server that
// applies a consumer cap of its own to a stream that sets none, 2.15.0: it reads such a stream as
// asking for JSDefaultMaxConsumersPerStream, which is 1000, the limit streamMaxConsumers records.
// Every server before it leaves such a stream with no limit, so there is nothing there for this
// binary's cap to fix.
const (
	streamMaxConsumersDefaultMajor = 2
	streamMaxConsumersDefaultMinor = 15
)

// connectedDefaultsAnUnsetCap is serverDefaultsAnUnsetCap of the version conn's server reports,
// read as natsversion.ConnectedVersion reads it: only once the connection reports one. A
// connection that is reconnecting or draining answers "" for its server's version. A bus
// connection buffers nothing while it reconnects (ReconnectBufSize -1, internal/bus Connect), so
// an ensure's request on it then fails at once; but a reconnect that lands between an empty read
// and the ensure's first request let the ensure write the stream with an older server's answer:
// on nats-server 2.15 a stream with no consumer cap, capped at 1000, was left as it is. A
// connection that reports no version before ctx is done or the wait passes answers the error, and
// the caller decides nothing; a ctx already done looks once
// (TestAnEnsureHookAcrossAReconnectDecidesNothingOnTheEmptyVersion).
func connectedDefaultsAnUnsetCap(ctx context.Context, conn *nats.Conn) (bool, error) {
	version, err := connectedVersion(ctx, conn)
	if err != nil {
		return false, err
	}
	return serverDefaultsAnUnsetCap(version), nil
}

// connectedVersion is the read connectedDefaultsAnUnsetCap makes, natsversion.ConnectedVersion; a
// test replaces it to put a second outage exactly between a reconnect and that read
// (TestAFlapRunsTheSecondDisconnectHookAtOnce).
var connectedVersion = natsversion.ConnectedVersion

// serverDefaultsAnUnsetCap reports whether serverVersion is the version of a server that applies
// its own default consumer cap to a stream that sets none: nats-server
// streamMaxConsumersDefaultMajor.Minor or later. serverVersion is what the connected server
// reports (connectedDefaultsAnUnsetCap), and the whole question this binary asks of a deployed
// stream whose cap is -1 or 0: on such a server that stream is capped at 1000 and is raised to
// streamMaxConsumers, on every older one it is unlimited and is left as it is (ownedStream).
//
// Only the major and the minor are read, since the first such server is a minor's own first
// release, and each is the number its field begins with, so a pre-release (`2.15.0-RC.1`) counts
// as the release it leads to and an optional leading `v` is taken as nats.go's own version parse
// takes it (versionComponents). A string that carries no major and minor is no such server; the
// empty string a connection answers outside CONNECTED never reaches here.
func serverDefaultsAnUnsetCap(serverVersion string) bool {
	major, rest, split := strings.Cut(strings.TrimPrefix(serverVersion, "v"), ".")
	if !split {
		return false
	}
	minor, _, _ := strings.Cut(rest, ".")
	gotMajor, gotMinor := versionNumber(major), versionNumber(minor)
	if gotMajor < 0 || gotMinor < 0 {
		return false
	}
	return gotMajor > streamMaxConsumersDefaultMajor ||
		(gotMajor == streamMaxConsumersDefaultMajor && gotMinor >= streamMaxConsumersDefaultMinor)
}

// versionNumber is the number a version's field begins with: 15 from `15` and from a
// pre-release's `15-RC` alike, and -1 from a field that begins with no digit, the empty field
// included, and from one carrying more digits than an int holds.
func versionNumber(field string) int {
	digits := 0
	for digits < len(field) && field[digits] >= '0' && field[digits] <= '9' {
		digits++
	}
	number, err := strconv.Atoi(field[:digits])
	if err != nil {
		return -1
	}
	return number
}

// streamCfg is the stream this binary creates, before ConnectOwningStream sets its replicas
// (WithReplicas) and byte limit (WithStreamMaxBytes). Its age equals the duplicate window by
// construction: the dashboard promises a same-mode retry cannot deliver twice for exactly as long
// as the stream can still recognise the repeat, and a deployment where the two differed would break
// that promise. A full stream refuses a new message (DiscardNew) rather than drop a stored one, so a
// stored message leaves only by age: the stream holds every message it took for the whole window,
// byte limit or not, and the promise holds. The refused publish's sender gets ErrStreamFull, which
// is not an ErrRefused: the same publish is taken once age has made room.
//
// It carries the consumer cap (streamMaxConsumers) as well, which a create takes on every
// nats-server; an update that changes a deployed stream's cap needs streamMaxConsumersFloor or
// later, and the reconcile owns the cap from there (ownedStream).
var streamCfg = &nats.StreamConfig{
	Name:         Stream,
	Subjects:     streamCarriedSubjects,
	Retention:    nats.LimitsPolicy,
	MaxConsumers: streamMaxConsumers,
	MaxAge:       streamDuplicateWindow,
	Duplicates:   streamDuplicateWindow,
	Discard:      nats.DiscardNew,
	Storage:      nats.FileStorage,
}

// roleLaneSubjects are the role lanes and their exceptions twin. They travel over core NATS and
// the stream must never retain them.
var roleLaneSubjects = []string{
	contracts.RoleTopicPrefix + ">",
	ExceptionTopicPrefix + contracts.RoleTopicPrefix + ">",
}

// RoleLane reports whether topic is a role lane or the exception lane of one (roleLaneSubjects),
// read literally as a published topic is, as subjectsCapture reads it: notifications.role.> is a
// role lane and so is every topic under it. The publish path sends such a topic over core NATS
// (publishPathOf), and a session's consumer filter leaves it out (cmd/listener's
// sessionConsumerFilter), since the stream never stores it and a role message reaches its holder
// as a forward on the holder's agent subject.
func RoleLane(topic string) bool {
	return subjectsCapture(roleLaneSubjects, topic)
}

// subjectCapturesRoleLanes reports whether some role-lane subject would land in a stream carrying
// subject.
func subjectCapturesRoleLanes(subject string) bool {
	return slices.ContainsFunc(roleLaneSubjects, func(roleLane string) bool { return subjectsOverlap(subject, roleLane) })
}

func streamCapturesRoleLanes(subjects []string) bool {
	return slices.ContainsFunc(subjects, subjectCapturesRoleLanes)
}

func purgeLegacyRoleMessages(js nats.JetStreamContext) error {
	for _, subject := range roleLaneSubjects {
		if err := js.PurgeStream(Stream, &nats.StreamPurgeRequest{Subject: subject}); err != nil {
			return err
		}
	}
	return nil
}

// consumerFilterSubjects is subjects without the listener's own lanes
// (listenerOwnStreamLanes), the list a consumer's filter may carry: the stream's own list holds
// both prefixes (streamCarriedSubjects) and no consumer filter ever does.
func consumerFilterSubjects(subjects []string) []string {
	return slices.DeleteFunc(slices.Clone(subjects), func(subject string) bool {
		return slices.ContainsFunc(listenerOwnStreamLanes, func(lane string) bool {
			return subjectsOverlap(subject, lane)
		})
	})
}

func migrateLegacyConsumerFilters(js nats.JetStreamContext, subjects []string) error {
	for name := range js.ConsumerNames(Stream) {
		info, err := js.ConsumerInfo(Stream, name)
		if err != nil {
			return err
		}
		if info.Config.FilterSubject != "notifications.>" {
			continue
		}
		config := info.Config
		config.FilterSubject = ""
		config.FilterSubjects = consumerFilterSubjects(subjects)
		if _, err := js.UpdateConsumer(Stream, &config); err != nil {
			return err
		}
	}
	return nil
}

func migrateRoleLanesOffStream(js nats.JetStreamContext, oldConfig, newConfig *nats.StreamConfig) error {
	if !streamCapturesRoleLanes(oldConfig.Subjects) || streamCapturesRoleLanes(newConfig.Subjects) {
		return nil
	}
	if err := migrateLegacyConsumerFilters(js, newConfig.Subjects); err != nil {
		return err
	}
	return purgeLegacyRoleMessages(js)
}

// subjectsOverlap reports whether some subject matches both patterns. JetStream refuses two such
// subjects in one stream.
func subjectsOverlap(a, b string) bool {
	return tokensOverlap(strings.Split(a, "."), strings.Split(b, "."))
}

// SubjectsOverlap reports whether some subject matches both patterns a and b (subjectsOverlap), for
// a caller outside this package. No production code calls it: the listener weighs a session's
// filters through SessionConsumerReduction, which records which topics fed each merge. The
// listener's reduction test reads it to check that each of its rows overlaps as the case says
// (cmd/listener's TestAFilterWhoseTopicsReduceToAnotherSessionsSubjectsLeavesThemOut), and it stays
// exported because a test in another package cannot reach the unexported one.
func SubjectsOverlap(a, b string) bool {
	return subjectsOverlap(a, b)
}

// tokensOverlap is subjectsOverlap on two subjects already split into their tokens, for a caller
// that weighs each subject against many others and splits it once.
func tokensOverlap(aTokens, bTokens []string) bool {
	for index := 0; index < len(aTokens) && index < len(bTokens); index++ {
		aToken, bToken := aTokens[index], bTokens[index]
		if aToken == ">" || bToken == ">" {
			return true
		}
		if aToken != "*" && bToken != "*" && aToken != bToken {
			return false
		}
	}
	return len(aTokens) == len(bTokens)
}

// streamReconciliation is the subject list the stream carries once this build has ensured it, and
// what that did to the deployed list: the list is the deployed one, then each of this build's
// subjects the deployed list lacks. Every process of the listener and of Dispatch's server ensures
// this one stream (bus.ConnectOwningStream), on each connect, and they deploy separately: during a
// release two builds of each run at once. So a deployed subject this build does not know may be one
// another live process still needs; the ensure keeps it and names it in foreign. Two kinds of
// deployed subject go:
//   - one that captures the role lanes, which travel over core NATS and must never be retained
//     (migrateRoleLanesOffStream);
//   - one that overlaps a subject of this build's (a widened, narrowed or split subject), because
//     JetStream refuses both in one stream and the ensure would fail. This build's shape wins
//     (replaced), and the next ensure of a build with the other shape puts that one back.
//
// Reconciliation runs only on a stream stamped with this build's generation or an older one: a
// stream a newer build stamped keeps its subjects as they are (ensureStreamWithConfig).
//
// Retiring any other subject is an operator step, taken once no deployment compiled with it can
// start again: `nats stream edit ENVOY_NOTIFICATIONS --subjects=... -f`.
type streamReconciliation struct {
	subjects []string
	replaced []subjectReplacement
	foreign  []string
}

// subjectReplacement is a deployed subject an ensure dropped and every one of its own subjects that
// overlapped it.
type subjectReplacement struct {
	dropped string
	kept    []string
}

func reconcileSubjects(deployed, own []string) streamReconciliation {
	result := streamReconciliation{subjects: make([]string, 0, len(deployed)+len(own))}
	for _, subject := range deployed {
		if subjectCapturesRoleLanes(subject) {
			continue
		}
		if !slices.Contains(own, subject) {
			var overlapping []string
			for _, ownSubject := range own {
				if subjectsOverlap(subject, ownSubject) {
					overlapping = append(overlapping, ownSubject)
				}
			}
			if len(overlapping) > 0 {
				result.replaced = append(result.replaced, subjectReplacement{dropped: subject, kept: overlapping})
				continue
			}
			result.foreign = append(result.foreign, subject)
		}
		result.subjects = append(result.subjects, subject)
	}
	for _, subject := range own {
		if !slices.Contains(result.subjects, subject) {
			result.subjects = append(result.subjects, subject)
		}
	}
	return result
}

// log reports the reconciliation to logger once the stream carries it.
func (r streamReconciliation) log(logger *slog.Logger) {
	for _, replacement := range r.replaced {
		logger.Warn("envoy nats stream subject replaced by an overlapping one", slog.String("dropped", replacement.dropped), slog.Any("kept", replacement.kept))
	}
	if len(r.foreign) > 0 {
		logger.Info("envoy nats stream keeps subjects this binary does not compile", slog.Any("subjects", r.foreign))
	}
}

// streamEnsure is who ensures the stream (ensureStreamWithConfig), which decides whether the
// ensure waits for the leader of a stream it finds with no leader that takes writes
// (leaderNotServing): one that another program created or raised a moment ago, or that lost its
// leader.
type streamEnsure int

const (
	// atStart is a program's start in the foreground (install): a write it sends to a stream in its
	// first election, or to a leader that has not applied its first entry as leader, draws no
	// answer, so it waits for a leader that takes writes, as the start that created or raised the
	// stream does.
	atStart streamEnsure = iota + 1
	// atReconnect is the reconnect hook (ensureStreamOn), a background start's first ensure
	// included: the hooks after it rewatch the program's state, and a stream a connection event
	// finds with no leader may stay so past the hang guard, as one whose members are lost does, so
	// it waits for no election it did not cause. A stream it must reconcile is still updated, and
	// while the cluster has no quorum that update fails at once and so does the run, as at main,
	// which recovery runs again once the quorum is back. A create or a raise it makes itself is
	// still followed by a wait for the leader, as at a start; no publish waits for the run
	// (WithConnectInBackground, onReconnect), so a run that wait ends at the guard holds no write,
	// only the hooks after it, which recovery runs again after its backoff.
	atReconnect
)

// ensureStreamWithConfig ensures the stream for at, the caller that ensures it (streamEnsure): a
// program's start (atStart) or the reconnect hook (atReconnect). It creates the stream with cfg,
// under a fresh StreamIDMetadataKey, or reconciles the deployed one towards cfg, and returns the
// subjects the stream carries afterwards.
// cfg's Replicas is the deployment's replica count and its MaxBytes the deployment's byte limit, 0
// when it sets none. defaultsAnUnsetCap is what the connected server is, read from the version it
// answers (serverDefaultsAnUnsetCap): whether a deployed stream with no consumer cap is one the
// server caps at a default of its own.
//
// A deployed stream stamped with a generation higher than streamConfigGeneration was set by a
// newer build, which owns its settings: it is left exactly as it is, so while two builds run the
// older one never reverts what the newer one set.
//
// Any other is reconciled. The update starts from the deployed configuration and sets only what
// this binary owns:
//   - the subjects (reconcileSubjects);
//   - the age and the duplicate window, cfg's, and discard, cfg's DiscardNew, so a full stream
//     refuses a new message and keeps every stored one (a deployed DiscardOld stream is switched);
//   - the replicas, the deployed count or cfg's when that is more: a start never writes fewer
//     copies than it read, so a program whose setting is lower (an older setting, or one restarted
//     before the others) leaves a raised stream it reads as it is, though not one raised after its
//     read (below); lowering is an operator step that needs every program's setting lowered first;
//   - the byte limit, the deployed limit or cfg's when that is more, cfg's on a stream with none
//     and the deployed one when cfg sets none: a start never writes a lower limit than it read, so
//     an operator's raise stands, and lowering or removing one is an operator step that needs
//     every program's setting lowered or removed first;
//   - the consumer cap, the deployed cap or cfg's (streamMaxConsumers) when that is more, and
//     cfg's on a stream whose cap is unset, a deployed -1 or 0 alike, only where
//     defaultsAnUnsetCap holds and that stream is therefore capped at the server's own default
//     of 1000: a start never writes a lower cap than it read, so an operator's raise stands, and
//     lowering one is an operator step that needs every program's binary lowered first;
//   - the metadata, the deployed metadata kept as it is, its StreamIDMetadataKey included; a stream
//     without that key gets one (identifiedMetadata), and this build's generation is stamped on it
//     (stampGeneration).
//
// Every other deployed setting (storage, placement, compression, the message count and size
// limits, direct get and the rest) stays as it is. A stream that already holds all of that gets no
// update, and one that lacks only the key or the generation gets its own configuration back with
// them added. A refused create or update names the settings it carried (refusedSettings). A
// create carries the consumer cap on every nats-server, but a server before
// streamMaxConsumersFloor refuses an update that changes the cap of a stream that already exists,
// so on such a server a start against a deployed stream whose cap is positive and below cfg's is
// refused. A stream with no cap is sent no cap change at all on a server before 2.15, where an
// unset cap is no limit rather than the default of 1000, so such a server is handed no update it
// refuses for a stream that has nothing this cap fixes.
//
// Every bus.ConnectOwningStream caller runs this on the one stream, and JetStream's stream update
// has no compare-and-swap, so two starts can each read the stream before either writes, and the
// later write wins. A start can therefore lose to a concurrent start with other settings: one that
// read the stream before this start's write and writes after it puts back the subjects, the
// copies, the byte limit and the metadata it read. The client this start returns keeps the stream
// (keepStream): within one keeper period it raises the copies and the byte limit to what its
// settings ask for where the stream holds less, puts back StreamIDMetadataKey, the age, the
// duplicate window and discard, and adds back each of its own subjects the stream lost that
// overlaps none the stream carries, so the race ends at the most copies and the largest byte limit
// any program asks for. Programs of a deployment with different ENVOY_NATS_REPLICAS or
// ENVOY_STREAM_MAX_BYTES therefore do not fight over the stream: neither is ever lowered, and the
// largest stands. Two races show even with the same settings, both of them settled where the
// update is sent (updateStream): nats-server can leave one of two replica changes proposed at once
// unanswered (seen with v2.10.29), and every nats-server before v2.12.5 can refuse the later of
// two updates of one stream as overlapping an existing stream, the stream itself, when the other
// update lands while the server checks this one (TestTwoStartsRaisingOneStreamAtOnceBothSucceed). A
// stream one start creates first is reconciled by the other, keeping the id it was created with
// (AddStream answers ErrStreamNameAlreadyInUse, as its config's id differs). Two that both find no
// id write the same one, derived from what both read. A third race is settled here: a server that
// leaves one of two starts' stream reads or creates unanswered, which each request below is sent
// again for (sentAgain), since a start that returns such an error ends the program. Those sends go
// on until the server answers, ctx ends or the start's hang guard passes (startHangGuard): the
// first send of a create of a stream of more than one copy is answered only once its group has
// elected a leader, however long the election takes. A request that drew no answer by then is
// named in the error, `ENVOY_NOTIFICATIONS STREAM.CREATE: 3 sends drew no answer when the wait
// ended: ...`. A create sent again while that election runs is answered before it ends, with the
// stream still leaderless, and so is a raise of the stream's copies, at once (as a bucket's
// raise is, awaitReplicasCurrent): so a create of more than one copy, and an update that raises
// the copies, are followed by a wait for a leader that takes writes (awaitStreamLeader). Measured
// with nats-server v2.15.1: a start whose stream's first election was held 11 s and 30 s answered
// after its second send at 10 s, and its first publish then failed "no response from stream"; a
// start that raised the stream from one copy to three answered at once, and its first publish,
// with no election held, drew no answer within its 5 s. A start that ended its wait on the new
// leader's name lost its first publish too, while that leader had not yet applied its first entry
// as leader (leaderNotServing). Its lines, the reconciliation's and those of its waits, go to
// logger.
func ensureStreamWithConfig(ctx context.Context, logger *slog.Logger, js nats.JetStreamContext, cfg *nats.StreamConfig, defaultsAnUnsetCap bool, at streamEnsure) ([]string, error) {
	ctx, cancel := hangGuard(ctx, startHangGuard)
	defer cancel()
	ctx = withReadLimit(ctx)
	read, create := Stream+" STREAM.INFO", Stream+" STREAM.CREATE"
	readStream := func() (*nats.StreamInfo, error) { return js.StreamInfo(Stream) }
	info, err := readAgain(ctx, logger, read, readLimitOf(ctx), readStream)
	if errors.Is(err, nats.ErrStreamNotFound) {
		created := *cfg
		created.Metadata = stampGeneration(cfg.Metadata)
		created.Metadata[StreamIDMetadataKey] = id.New()
		// A create whose answer was lost is made again with the same configuration and the same
		// id, drawn once above. A server that applied the lost create reads the second send as an
		// equal create and answers it with the stream (v2.15.1 server/jetstream_cluster.go
		// jsClusteredStreamRequest, "This is an equal assignment"), so the start goes on; only
		// another start's create, which carries another id, draws ErrStreamNameAlreadyInUse, which
		// this branch reads as such and follows with the read below.
		_, err = sentAgain(ctx, logger, create, func() (*nats.StreamInfo, error) { return js.AddStream(&created) })
		if err == nil {
			if created.Replicas > 1 {
				err = awaitStreamLeader(ctx, logger, js, afterCreate, Stream)
			}
			if err != nil {
				return nil, err
			}
			return created.Subjects, nil
		}
		if unanswered(err) || ctx.Err() != nil {
			return nil, namedRequest(create, err)
		}
		if !errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			return nil, refusedSettings(err, nil, &created)
		}
		info, err = readAgain(ctx, logger, read, readLimitOf(ctx), readStream)
	}
	if unanswered(err) || ctx.Err() != nil {
		return nil, namedRequest(read, err)
	}
	if err != nil {
		return nil, err
	}
	if leaderless(info) && at == atStart {
		// The stream another start created or raised a moment ago has no leader that takes writes
		// yet: that start's own wait does not reach this one, and a write sent now draws no answer.
		if err := awaitStreamLeader(ctx, logger, js, afterOpen, Stream); err != nil {
			return nil, err
		}
	}
	if newerBuild(info) {
		logLeftToNewerBuild(logger, info)
		return info.Config.Subjects, nil
	}
	desired, reconciliation := reconciledStream(info, cfg, defaultsAnUnsetCap)
	if reflect.DeepEqual(desired, info.Config) {
		reconciliation.log(logger)
		return info.Config.Subjects, nil
	}
	// The migrate and the purge below are requests a server can leave unanswered, and each is
	// abandoned when ctx ends (Bounded), so the ensure ends at ctx or the start's hang guard like
	// every other wait of it, naming the write that was in flight. The update bounds itself
	// (updateStream).
	written := func(step string, write func() error) error {
		_, answered, err := Bounded(ctx, func() (struct{}, error) { return struct{}{}, write() })
		if !answered {
			return fmt.Errorf("%s %s: no answer when the wait ended: %w", step, Stream, context.Cause(ctx))
		}
		return err
	}
	migratingRoleLanes := streamCapturesRoleLanes(info.Config.Subjects) && !streamCapturesRoleLanes(desired.Subjects)
	if err := written("migrate", func() error { return migrateRoleLanesOffStream(js, &info.Config, &desired) }); err != nil {
		return nil, err
	}
	// An update sent again is reconciled from the stream as its read found it, and none is sent
	// onto a stream a newer build stamped while this one's update went unanswered (updateStream).
	reconcile := func(current *nats.StreamInfo) (nats.StreamConfig, bool) {
		if newerBuild(current) {
			return nats.StreamConfig{}, false
		}
		reconciled, _ := reconciledStream(current, cfg, defaultsAnUnsetCap)
		return reconciled, true
	}
	update, err := updateStream(ctx, logger, js, info, reconcile, func(again *nats.StreamInfo) bool {
		reconciled, write := reconcile(again)
		return write && reflect.DeepEqual(reconciled, again.Config)
	})
	switch {
	case err != nil && ctx.Err() != nil:
		// updateStream ends on a silence only when ctx does, and a silence refused no setting.
		return nil, fmt.Errorf("update %s: %w", Stream, err)
	case err != nil:
		return nil, refusedSettings(err, &update.from.Config, &update.sent)
	case update.declined:
		// A newer build's start wrote the stream while this one's update was unanswered: its
		// settings stand, as on a first read that finds them, and so does its new election.
		if leaderless(update.from) && at == atStart {
			if err := awaitStreamLeader(ctx, logger, js, afterOpen, Stream); err != nil {
				return nil, err
			}
		}
		logLeftToNewerBuild(logger, update.from)
		return update.from.Config.Subjects, nil
	}
	_, reconciliation = reconciledStream(update.from, cfg, defaultsAnUnsetCap)
	reconciliation.log(logger)
	if update.sent.Replicas > info.Config.Replicas {
		if err := awaitStreamLeader(ctx, logger, js, afterRaise, Stream); err != nil {
			return nil, err
		}
	}
	if migratingRoleLanes {
		if err := written("purge", func() error { return purgeLegacyRoleMessages(js) }); err != nil {
			return nil, err
		}
	}
	return update.sent.Subjects, nil
}

// newerBuild reports whether info's stream carries the settings of a newer build, one that stamped
// a generation above this build's (streamGenerationKey), which this build leaves exactly as they
// are: its start (ensureStreamWithConfig) and its keeper (keepStream) ask it of every read they
// would write from, the first one and each one after an update that drew no answer.
func newerBuild(info *nats.StreamInfo) bool {
	return MetadataGeneration(info.Config.Metadata, streamGenerationKey) > streamConfigGeneration
}

// logLeftToNewerBuild logs to logger that a start found info's stream carrying a newer build's
// settings and leaves them as they are (newerBuild).
func logLeftToNewerBuild(logger *slog.Logger, info *nats.StreamInfo) {
	logger.Info("envoy nats stream settings belong to a newer build, so this build leaves them as they are",
		slog.Int("deployed_generation", MetadataGeneration(info.Config.Metadata, streamGenerationKey)), slog.Int("own_generation", streamConfigGeneration))
}

// MetadataGeneration is the generation metadata holds under key, as a stream or a durable consumer
// carries it: 0 when it holds none, as one a build before generations last set does, or holds one
// that is not a number.
func MetadataGeneration(metadata map[string]string, key string) int {
	generation, err := strconv.Atoi(metadata[key])
	if err != nil {
		return 0
	}
	return generation
}

// OwnMetadata is a copy of deployed without the keys nats-server sets itself (the `_nats.` prefix),
// which the server sets again on every create and update, with room for extra more keys. The copy
// is never nil, so a caller can add keys to it.
func OwnMetadata(deployed map[string]string, extra int) map[string]string {
	metadata := make(map[string]string, len(deployed)+extra)
	for key, value := range deployed {
		if !strings.HasPrefix(key, "_nats.") {
			metadata[key] = value
		}
	}
	return metadata
}

// stampGeneration is metadata with this build's generation stamped on it. The copy keeps every
// other key, the ones nats-server sets itself included, so a configuration it goes into equals the
// deployed one when nothing else changed. It is never nil, so a caller can add keys to it.
func stampGeneration(metadata map[string]string) map[string]string {
	stamped := maps.Clone(metadata)
	if stamped == nil {
		stamped = map[string]string{}
	}
	stamped[streamGenerationKey] = strconv.Itoa(streamConfigGeneration)
	return stamped
}

// reconciledStream is the configuration ensureStreamWithConfig writes over the deployed stream info
// describes, and the subject reconciliation in it: the deployed configuration with what this binary
// owns set from cfg (ownedStream, which defaultsAnUnsetCap reaches) and the subjects reconciled
// (reconcileSubjects).
func reconciledStream(info *nats.StreamInfo, cfg *nats.StreamConfig, defaultsAnUnsetCap bool) (nats.StreamConfig, streamReconciliation) {
	reconciliation := reconcileSubjects(info.Config.Subjects, cfg.Subjects)
	desired := ownedStream(info, cfg, defaultsAnUnsetCap)
	desired.Subjects = reconciliation.subjects
	return desired, reconciliation
}

// keptStream is the configuration the keeper (keepStream) writes over the deployed stream info
// describes: the deployed configuration with what this binary owns set from cfg (ownedStream,
// which defaultsAnUnsetCap reaches) and the deployed subjects with this binary's own added where
// the keeper may (keptSubjects).
func keptStream(info *nats.StreamInfo, cfg *nats.StreamConfig, defaultsAnUnsetCap bool) nats.StreamConfig {
	kept := ownedStream(info, cfg, defaultsAnUnsetCap)
	kept.Subjects = keptSubjects(info.Config.Subjects, cfg.Subjects)
	return kept
}

// ownedStream is the deployed configuration info describes with every setting this binary owns
// but the subjects set from cfg, as both a start (reconciledStream) and the keeper (keptStream)
// write it: the replicas, the deployed count or cfg's when that is more; the byte limit, the
// deployed limit or cfg's when that is more, cfg's on a stream with none and the deployed one when
// cfg sets none; the consumer cap, the deployed cap or cfg's when that is more, and cfg's on a
// stream whose cap is unset where defaultsAnUnsetCap holds; the age, the duplicate window and
// discard, cfg's; and the metadata with a StreamIDMetadataKey (identifiedMetadata) and this
// build's generation (stampGeneration). Neither the copies nor the byte limit nor the cap is ever
// lowered, so an operator's raise of any of them stands; lowering one is an operator step once
// every program's setting is lowered. The age, the window and discard are compiled into the
// binary, not deployment settings (streamCfg), and a release that changed one of them takes a new
// streamConfigGeneration, which an older release's keeper leaves alone (keepStream), so the two
// never put their own value back in turn while both run.
//
// A deployed cap of -1 or 0 is a stream that sets no cap. What that means is the connected
// server's, which defaultsAnUnsetCap carries here (serverDefaultsAnUnsetCap): from nats-server
// 2.15 such a stream is capped at the server's own default of 1000
// (JSDefaultMaxConsumersPerStream), so it is raised to streamMaxConsumers as a smaller cap is;
// on every server before that it has no limit at all, so it is left exactly as it is and the
// binary writes no cap where none was asked for. A stream that already carries a positive cap
// below this binary's is raised on every server, since that cap bounds its consumers wherever it
// runs. The update that carries either raise needs streamMaxConsumersFloor or later on a stream
// that already exists (refusedSettings).
func ownedStream(info *nats.StreamInfo, cfg *nats.StreamConfig, defaultsAnUnsetCap bool) nats.StreamConfig {
	owned := info.Config
	owned.Replicas = max(info.Config.Replicas, cfg.Replicas)
	if cfg.MaxBytes > 0 {
		owned.MaxBytes = max(info.Config.MaxBytes, cfg.MaxBytes)
	}
	if cfg.MaxConsumers > 0 && (info.Config.MaxConsumers > 0 || defaultsAnUnsetCap) {
		owned.MaxConsumers = max(info.Config.MaxConsumers, cfg.MaxConsumers)
	}
	owned.MaxAge = cfg.MaxAge
	owned.Duplicates = cfg.Duplicates
	owned.Discard = cfg.Discard
	owned.Metadata = stampGeneration(identifiedMetadata(info))
	return owned
}

// keptSubjects is the subject list the keeper writes: the deployed list, then each of own, this
// binary's subjects, that the list lacks and that overlaps no deployed subject. It does not check
// own for a subject that captures a role lane, because no subject this binary compiles captures
// one, which TestStreamConfigExcludesRoleLanes in stream_config_test.go guarantees. It only adds:
// unlike a start (reconcileSubjects) it removes and replaces nothing, so two releases with
// different subject lists never switch the stream back and forth, and a subject of this binary's
// that another release's overlapping one replaced stays replaced until this binary's next start.
func keptSubjects(deployed, own []string) []string {
	kept := slices.Clone(deployed)
	for _, subject := range own {
		if slices.ContainsFunc(kept, func(other string) bool { return subjectsOverlap(subject, other) }) {
			continue
		}
		kept = append(kept, subject)
	}
	return kept
}

// streamUpdate is what updateStream came to: from, the read of the stream its last decision was
// made on (the caller's own read, or the read after a send that drew no answer or an overlap
// refusal); sent, the update it sent from that read, which the caller's error names
// (refusedSettings) and a keeper's line reports against from (restoredFields); and declined, true
// when change declined to write the stream as from found it, so nothing was sent from it.
type streamUpdate struct {
	from     *nats.StreamInfo
	sent     nats.StreamConfig
	declined bool
}

// updateStream writes change, the update a caller makes of a stream given a read of it, starting
// from info, the caller's own read, and succeeds when the server accepts it or when the stream,
// read again, holds what the caller asked for (holds). change may decline a read (false), and the
// call then sends nothing more and reports it declined (streamUpdate): the stream ensure and the
// keeper decline a stream a newer build stamped (newerBuild). Two answers say nothing of whether
// the update landed, and after either the stream is read again:
//   - an answer that never came (unanswered): nats-server v2.15.1 drops an update with no answer
//     when its metadata proposal fails because the term moved (jsClusteredStreamUpdateRequestLocked,
//     server/jetstream_cluster.go), and v2.10.29 was seen to leave one of two replica changes
//     proposed at once unanswered. A stream that holds what was asked for has taken the update; one
//     that does not is sent it again, for as long as ctx lasts, so an update the server dropped
//     cannot fail the step at the caller's JetStream wait. Each send waits that MaxWait, a hang
//     guard of one send, and the step ends on a server that never answers only when ctx does,
//     which every caller bounds: a start's or a keeper pass's hang guard (startHangGuard), the
//     session reaper's RequestTimeout. An update sent again while the one before it is still
//     proposed is checked against that proposal (streamAssignmentOrInflight, v2.15.1), so a raise
//     sent again stacks on the raise in flight rather than choosing another peer set;
//   - a refusal of the update's subjects as overlapping an existing stream (overlapRefused), which
//     every nats-server before v2.12.5 can give the later of two updates of one stream: the server
//     reads the stream's assignment, releases its lock to check the new configuration, and then
//     looks for an overlap among the account's assignments skipping only the assignment it read,
//     so an update that lands in between leaves the stream overlapping itself
//     (jsClusteredStreamUpdateRequest, v2.10.29 server/jetstream_cluster.go; nats-server commit
//     cb1afd68d014, first released in v2.12.5, skips by name). Nothing was written, so a stream
//     that does not hold the update is sent it again, the write a server without that race would
//     have taken after the other, up to overlapSends refused sends in all. A stream another stream
//     really does overlap is refused every send, and the last refusal is returned.
//
// Each update sent again is change applied to the stream as the read before it found it, so it
// changes only what the caller changes and never writes back what another writer changed since
// the caller's read, and none is sent onto a stream that read finds change declines, such as one
// a newer build wrote while this update went unanswered. It goes bucketAppliedInterval after that
// read, as sentAgain pauses between two sends. Any other refusal is the update's own error. A
// send in flight when ctx ends is abandoned, its answer dropped (Bounded), and the step ends
// naming ctx's cause.
//
// The read after the update is a read of a stream's state, sent again while the server leaves it
// unanswered, each send up to its read limit, for as long as ctx lasts (readAgain): an update
// sent while the stream's group elects a leader can draw no answer, and a read sent then can be
// lost too, so a read sent once could leave a raise that landed reported as one that failed. The
// lines of those reads sent again go to logger.
func updateStream(ctx context.Context, logger *slog.Logger, js nats.JetStreamContext, info *nats.StreamInfo, change func(*nats.StreamInfo) (nats.StreamConfig, bool), holds func(*nats.StreamInfo) bool) (streamUpdate, error) {
	name := info.Config.Name
	result := streamUpdate{from: info}
	for overlaps := 0; ; {
		update, write := change(result.from)
		if !write {
			result.declined = true
			return result, nil
		}
		result.sent = update
		_, answered, err := Bounded(ctx, func() (*nats.StreamInfo, error) { return js.UpdateStream(&update) })
		switch {
		case !answered:
			return result, fmt.Errorf("the update drew no answer when the wait ended: %w", context.Cause(ctx))
		case overlapRefused(err):
			overlaps++
		case !unanswered(err):
			return result, err
		}
		again, readErr := readAgain(ctx, logger, name+" STREAM.INFO", readLimitOf(ctx), func() (*nats.StreamInfo, error) { return js.StreamInfo(name) })
		switch {
		case readErr == nil && holds(again):
			return result, nil
		case readErr != nil:
			return result, fmt.Errorf("%w, and the read after it: %w", err, readErr)
		case overlaps == overlapSends:
			return result, err
		}
		if ended := pause(ctx, bucketAppliedInterval); ended != nil {
			return result, fmt.Errorf("%w, and the wait ended before it was sent again: %w", err, ended)
		}
		result.from = again
	}
}

// overlapSends bounds the sends of one update the server refuses as overlapping an existing stream
// (updateStream). Each such refusal of a stream no other stream overlaps means another update of
// that stream landed while the server checked this one, so five sends outlast four such updates in
// a row, and a stream another stream does overlap costs four sends and five stream reads more
// before its refusal is returned.
const overlapSends = 5

// jsStreamSubjectOverlap is nats-server's error code for a stream create or update whose subjects
// overlap an existing stream's (JSStreamSubjectOverlapErr). nats.go names no constant for it.
const jsStreamSubjectOverlap = 10065

// overlapRefused reports whether err is nats-server's refusal of a stream's subjects as
// overlapping an existing stream's.
func overlapRefused(err error) bool {
	var apiErr *nats.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == jsStreamSubjectOverlap
}

// maxConsumersRefusal is what nats-server answers an update that changes a stream's MaxConsumers
// with, word for word, on every server before streamMaxConsumersFloor: the text it hands
// NewJSStreamInvalidConfigError (server/stream.go), measured on v2.10.29, v2.11.0 and v2.12.0,
// which nats.go gives the caller as the whole description of an *nats.APIError, under its own
// `nats: ` prefix.
const maxConsumersRefusal = "stream configuration update can not change MaxConsumers"

// maxConsumersRefused reports whether err is that refusal, read by the server's own words and not
// by its code: the code is JSStreamInvalidConfigF, which nats.go names nats.JSStreamInvalidConfig
// (10052) and which carries whatever the server has to say about a stream's configuration, the
// storage type and the retention policy an update may not change among them, so a code test would
// take any of those for this one. The words are read off the error's own string, so a refusal
// still counts when something has wrapped it on its way here, and when it reaches the bus as
// anything other than an *nats.APIError.
func maxConsumersRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), maxConsumersRefusal)
}

// unanswered reports whether err is a JetStream request's wait running out with no answer, rather
// than the server's refusal.
func unanswered(err error) bool {
	return errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded)
}

// refusedSettings is err, the server's answer to request, a create (deployed nil) or an update of
// the stream deployed, named with each deployment setting whose value request changes:
// ReplicasEnvVar when it sets more than one copy the stream does not have (a create, or a raise),
// StreamMaxBytesEnvVar when it sets a byte limit the stream does not have. The bus is handed the
// numbers and not where they came from, so the names are those of the settings every deployed
// program reads them from (WithReplicas, WithStreamMaxBytes). One copy is never named: a server
// refuses no stream for having one. nil, and a refusal of a request that changes none of them, are
// returned as they are, and so is an error that is no answer of the server's at all: a request the
// server left unanswered (unanswered) or one whose wait ended (context.Canceled, or the hang
// guard's cause, which wraps context.DeadlineExceeded), since a server that answered nothing
// refused no setting.
//
// An update that changes the deployed stream's consumer cap names the cap too, under the name
// nats-server spells it (max_consumers), with the value this binary asks for; the cap is compiled
// in rather than read from a variable (streamMaxConsumers), so it is named as the field and not
// as a setting an operator can change. That name carries the first server which can take the
// change (streamMaxConsumersFloor) only when the server's answer is its own refusal of the change
// (maxConsumersRefused), a bare maxConsumersRefusal that says neither which field nor which
// server takes it. Every other refusal of such an update - a raise of the copies the cluster has
// no room for on a server that would have taken the cap, an overlap still refused after its
// sends - names the cap alone, as the copies and the byte limit are named, since the server the
// cap needs is nothing to an operator whose update was refused for another reason. A create is
// never named: a create carries the cap on every nats-server.
func refusedSettings(err error, deployed, request *nats.StreamConfig) error {
	if err == nil || unanswered(err) || errors.Is(err, context.Canceled) {
		return err
	}
	var settings []string
	if request.Replicas > 1 && (deployed == nil || request.Replicas != deployed.Replicas) {
		settings = append(settings, fmt.Sprintf("%s=%d", ReplicasEnvVar, request.Replicas))
	}
	if request.MaxBytes > 0 && (deployed == nil || request.MaxBytes != deployed.MaxBytes) {
		settings = append(settings, fmt.Sprintf("%s=%d", StreamMaxBytesEnvVar, request.MaxBytes))
	}
	if deployed != nil && request.MaxConsumers != deployed.MaxConsumers {
		named := fmt.Sprintf("max_consumers %d", request.MaxConsumers)
		if maxConsumersRefused(err) {
			named += fmt.Sprintf(" (only nats-server %s and later can change it on an existing stream)", streamMaxConsumersFloor)
		}
		settings = append(settings, named)
	}
	if len(settings) == 0 {
		return err
	}
	return fmt.Errorf("%s: %w", strings.Join(settings, ", "), err)
}

// identifiedMetadata is a copy of the deployed stream's metadata that holds a StreamIDMetadataKey:
// the stream's own when it has one, deployedStreamID otherwise.
func identifiedMetadata(info *nats.StreamInfo) map[string]string {
	metadata := maps.Clone(info.Config.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	if metadata[StreamIDMetadataKey] == "" {
		metadata[StreamIDMetadataKey] = deployedStreamID(info.Created)
	}
	return metadata
}

// deployedStreamID is the StreamIDMetadataKey a start gives a deployed stream that has none: 16
// bytes of the SHA-256 of the stream's name and the Created time STREAM.INFO answered, in hex. Every
// start that reads the stream from one leader before any of them writes derives the same id, so
// writers racing to add it write one value, where a random id would leave the last writer's.
func deployedStreamID(created time.Time) string {
	sum := sha256.Sum256([]byte(Stream + "\x00" + created.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:16])
}
