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

// edgeFrameVerdict is whether the edge withholds from sessionID, whose token lists projects, a
// frame of the message stored on subject with data, and why:
//
//   - a GitHub event is the repository rule's (relayVerdict), with no reader withheld
//     answer_unknown, and one whose envelope names a topic that is no GitHub event's is withheld;
//   - a message on an agent subject passes only on the session's own (ownFrameAgentSubject), and
//     there by the topic its envelope names: the session's own agent subject is a message to the
//     session, a send or a dead letter, which passes; any other topic is a forward, a role's
//     message forwarded to the claim's holder among them, judged as a frame of that topic. A
//     forward of a GitHub event or of another agent subject, and an envelope that does not
//     decode, are withheld;
//   - every other frame passes when contracts.SubjectProject gives it a project the token lists
//     and, where its envelope names a topic, that topic is of the same project: the subject a
//     relay header names and the envelope it carries are both what a publisher on the relay
//     subject writes, so a frame whose envelope names another project's event under a header of
//     the token's project is withheld.
func edgeFrameVerdict(repositories *edgeRepositories, sessionID string, projects []string, subject string, data []byte) (withhold bool, reason string) {
	topic, named := envelopeTopic(data)
	if strings.HasPrefix(subject, contracts.GithubTopicPrefix) {
		if named && !strings.HasPrefix(topic, contracts.GithubTopicPrefix) {
			return true, withheldProjectNotHeld
		}
		if repositories == nil {
			return true, withheldAnswerUnknown
		}
		return repositories.relayVerdict(projects, subject, data)
	}
	if strings.HasPrefix(subject, contracts.AgentTopicPrefix) {
		if !ownFrameAgentSubject(sessionID, projects, subject) || !named {
			return true, withheldProjectNotHeld
		}
		if ownFrameAgentSubject(sessionID, projects, topic) {
			return false, ""
		}
		if strings.HasPrefix(topic, contracts.GithubTopicPrefix) || strings.HasPrefix(topic, contracts.AgentTopicPrefix) {
			return true, withheldProjectNotHeld
		}
		subject = topic
	}
	project, _, ok := contracts.SubjectProject(subject)
	if !ok || !projectHeld(projects, project) {
		return true, withheldProjectNotHeld
	}
	if named {
		if topicProject, _, ok := contracts.SubjectProject(topic); !ok || topicProject != project {
			return true, withheldProjectNotHeld
		}
	}
	return false, ""
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
// message is stored on: the old form, which names no project, or the new form under one of
// projects. A wildcard in the project's place is no stored subject, so it is no one's here.
func ownFrameAgentSubject(sessionID string, projects []string, subject string) bool {
	if subject == contracts.LegacyAgentSubject(sessionID) {
		return true
	}
	rest, found := strings.CutPrefix(subject, contracts.AgentTopicPrefix)
	if !found {
		return false
	}
	project, target, found := strings.Cut(rest, ".")
	return found && target == sessionID && project != "*" && projectHeld(projects, project)
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
