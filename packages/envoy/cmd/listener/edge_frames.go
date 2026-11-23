package main

// edge_frames.go is the edge listener's rule for every frame it hands an outside session: the
// relay's, before it publishes a frame on the session's relay subject (sessionRelays.edgeWithholds),
// and the relay socket's, before it writes one to the client (relaySocket.forward). A frame passes
// only when it is of a project the session's token lists, when it is the session's own message, or
// when it is a GitHub event that the repository rule passes (edgeRepositories.relayVerdict).
//
// The projects are a token's own, and never a stored record's: the socket holds the token it was
// opened with, which it asks the broker about again and closes on when the answer names other
// projects (relaySocket.admitted, relaySocket.verdict), and the relay holds the token this replica
// admitted the session's registration on, in memory (edgeAdmissions). So nothing the bus stores -
// a session's entry, an interest row, a role claim, the filter of the session's consumer - can
// put another project's frame in front of the session. Every frame the rule withholds is
// acknowledged undelivered and counted under project_not_held.

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// withheldProjectNotHeld is the reason envoy_edge_github_withheld_total counts a frame under that
// is of no project the session's token lists.
const withheldProjectNotHeld = "project_not_held"

// withheldNotAdmitted is why the relay withholds, unacknowledged and uncounted, a frame of a
// session this listener admitted no registration of (sessionRelays.edgeWithholds).
const withheldNotAdmitted = "not_admitted"

// frameAction is what the edge does with a frame (edgeFrameVerdict): the control flow its callers
// switch on, so a new mode is a value they must handle rather than a reason string that falls
// through to acknowledge-and-count. The metric label a counted drop or a withhold carries is
// frameVerdict.reason, a field apart from the action.
type frameAction uint8

const (
	// framePass delivers the frame.
	framePass frameAction = iota
	// frameDrop acknowledges the frame undelivered and counts it under its reason.
	frameDrop
	// frameHold withholds the frame unacknowledged, for the consumer to deliver again
	// (withheldNotAdmitted); the relay alone reaches it, and it is never counted.
	frameHold
	// frameAnswerUnknown withholds a GitHub frame for want of a fresh repository answer: the relay
	// marks the session's gap and acknowledges it, the socket answers it nak, and it is sent again.
	frameAnswerUnknown
)

// frameVerdict is the edge's decision on one frame: its action, the metric label (reason) a
// counted drop or a withhold logs under, and whether a fresh repository answer made it (answered):
// a GitHub event relayed because a fresh answer names its repository, or dropped because every
// answer of the session's projects is fresh and none names it (relayVerdict). Only such a verdict
// ends a run of GitHub frames given back for want of an answer (sessionRelays.endGiveBacks,
// relaySocket.endUnknownNaks); a drop of an event that names no repository, or of an envelope that
// is no GitHub event, says nothing of the answer.
type frameVerdict struct {
	action   frameAction
	reason   string
	answered bool
}

func framePasses() frameVerdict             { return frameVerdict{action: framePass} }
func frameDrops(reason string) frameVerdict { return frameVerdict{action: frameDrop, reason: reason} }
func frameHolds() frameVerdict              { return frameVerdict{action: frameHold, reason: withheldNotAdmitted} }
func frameAnswerUnknownVerdict() frameVerdict {
	return frameVerdict{action: frameAnswerUnknown, reason: withheldAnswerUnknown}
}

// edgeFrameVerdict is whether the edge withholds from sessionID, whose token lists projects, a
// frame of the message stored on subject with data, and why:
//
//   - a GitHub event is the repository rule's (relayVerdict), with no reader withheld
//     answer_unknown, and one whose envelope names a topic that is no GitHub event's is withheld;
//   - a message on an agent subject passes only on the session's own (ownFrameAgentSubject): a
//     role message - a role-lane forward or a role-marked send, told by the read the session
//     relay tells one by (readRoleFrame) - passes only for the project its role's claim works for
//     (roleOfProjects), judged by the role and not by the subject it is stored on, since a forged
//     role claim or a kept or taken copy can store another project's role's message on the
//     session's own subject; any other frame, a message whose role that read cannot tell among
//     them, as the relay sends one, passes by the topic its envelope names: the session's own
//     agent subject is a message to the session, a send or a dead letter, which passes, and any
//     other topic is judged as a frame of that topic. A forward of a GitHub event or of another
//     agent subject, and an envelope that names no topic, are withheld;
//   - every other frame passes when contracts.SubjectProject gives it a project the token lists
//     and, where its envelope names a topic, that topic is of the same project: the subject a
//     relay header names and the envelope it carries are both what a publisher on the relay
//     subject writes, so a frame whose envelope names another project's event under a header of
//     the token's project is withheld.
func edgeFrameVerdict(repositories *edgeRepositories, sessionID string, projects []string, subject string, data []byte) frameVerdict {
	topic, named := envelopeTopic(data)
	if strings.HasPrefix(subject, contracts.GithubTopicPrefix) {
		if named && !strings.HasPrefix(topic, contracts.GithubTopicPrefix) {
			return frameDrops(withheldProjectNotHeld)
		}
		if repositories == nil {
			return frameAnswerUnknownVerdict()
		}
		return repositories.relayVerdict(projects, subject, data)
	}
	if strings.HasPrefix(subject, contracts.AgentTopicPrefix) {
		if !ownFrameAgentSubject(sessionID, projects, subject) {
			return frameDrops(withheldProjectNotHeld)
		}
		// A role message - a role-lane forward, whose topic names the lane, or a role-marked
		// send, whose role mark names the role - is judged by the project its role's claim works
		// for (roleOfProjects), not by the subject the frame is stored on. A forged role claim can
		// name the session for another project's role and have Dispatch send that role's message
		// on the session's own subject, and a kept or a taken copy of the session's own project's
		// role keeps the previous holder's subject as its topic; both reach the session on its own
		// agent subject. The role is read as the session relay, a takeover pass and the kept
		// lane's walk read it (readRoleFrame): by the exact keys the plugins' transport reads,
		// past a byte order mark and at any depth, so a frame the session's plugin reads as a
		// role's message is judged by that role here, whatever encoding/json makes of its other
		// fields or its case-folded keys. It holds a role lane to the project edgeTopicRule does
		// (contracts.RoleClaimOf). A message whose role that read cannot tell is no role message,
		// as the relay sends it, and is judged by its topic below.
		if frame, err := readRoleFrame(data); err == nil && frame.role != "" {
			if roleOfProjects(frame.role, projects) {
				return framePasses()
			}
			return frameDrops(withheldProjectNotHeld)
		}
		if !named {
			return frameDrops(withheldProjectNotHeld)
		}
		if ownFrameAgentSubject(sessionID, projects, topic) {
			return framePasses()
		}
		if strings.HasPrefix(topic, contracts.GithubTopicPrefix) || strings.HasPrefix(topic, contracts.AgentTopicPrefix) {
			return frameDrops(withheldProjectNotHeld)
		}
		subject = topic
	}
	project, _, ok := contracts.SubjectProject(subject)
	if !ok || !slices.Contains(projects, project) {
		return frameDrops(withheldProjectNotHeld)
	}
	if named {
		if topicProject, _, ok := contracts.SubjectProject(topic); !ok || topicProject != project {
			return frameDrops(withheldProjectNotHeld)
		}
	}
	return framePasses()
}

// envelopeTopic is the topic of the envelope that data holds, and false when data decodes as no
// envelope or its envelope names no topic.
func envelopeTopic(data []byte) (string, bool) {
	var envelope struct {
		Topic string `json:"topic"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Topic == "" {
		return "", false
	}
	return envelope.Topic, true
}

// ownFrameAgentSubject reports whether subject is sessionID's own agent subject in a form a
// message is stored on: ownAgentSubject's forms without the wildcard project, which no publisher
// stores a message under, so it is no one's here.
func ownFrameAgentSubject(sessionID string, projects []string, subject string) bool {
	return ownAgentSubject(sessionID, projects, subject) && subject != contracts.AgentTopicPrefix+"*."+sessionID
}

// edgeAdmissions is the projects of the token each session's latest registration on this replica
// was admitted on (subscribeHandler), the list the relay holds that session's frames to
// (sessionRelays.edgeWithholds). It is memory only and is never read back from a record: a session
// this replica admitted no registration of since it started has none here, and the relay holds its
// messages in its consumer until it registers through this replica (sessionRelays.relayNATS),
// which the plugin does at every heartbeat. An entry is forgotten once its session's record has
// left the session cache (prune): the map holds the sessions whose records the bucket holds and
// those admitted within the last edgeAdmissionGrace, each one session id and its token's list, at
// most oauthwire.MaxProjects keys.
type edgeAdmissions struct {
	mu       sync.Mutex
	admitted map[string]edgeAdmission
}

// edgeAdmission is the list of the token one session's latest registration on this replica was
// admitted on, and when this replica admitted it.
type edgeAdmission struct {
	projects []string
	at       time.Time
}

// edgeAdmissionGrace is how long an admission is kept whatever the session cache holds
// (edgeAdmissions.prune). A registration admits its session before it writes the session's entry
// (subscribeHandler), within sendRequestBudget, and the entry reaches the cache by the watch after
// that write; an admission younger than this may be one whose entry is still on its way, and is
// kept.
const edgeAdmissionGrace = relayPruneInterval

// admit records projects as the list of the token sessionID's registration was admitted on.
func (a *edgeAdmissions) admit(sessionID string, projects []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admitted == nil {
		a.admitted = map[string]edgeAdmission{}
	}
	a.admitted[sessionID] = edgeAdmission{projects: slices.Clone(projects), at: time.Now()}
}

// of is the list sessionID's latest admitted registration's token carried, and whether this
// replica admitted one.
func (a *edgeAdmissions) of(sessionID string) ([]string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	admission, admitted := a.admitted[sessionID]
	return admission.projects, admitted
}

// prune forgets each admission made before cutoff whose session gone reports has no record, so a
// replica keeps no list for a session that deregistered or lapsed. gone runs under a.mu, so it may
// not reach the admissions; the session cache's Get does not. A session that registers here again
// is admitted again.
func (a *edgeAdmissions) prune(cutoff time.Time, gone func(sessionID string) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for sessionID, admission := range a.admitted {
		if admission.at.Before(cutoff) && gone(sessionID) {
			delete(a.admitted, sessionID)
		}
	}
}
