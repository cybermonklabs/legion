package intake

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/subjectforms"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// decodedMessage is the checked envelope identity and its workflow fact. A nil Fact is a valid
// additive or non-workflow event that the shipped reducer acknowledges without a state change.
// EventID is the id ApplyFact records the fact under (envoyEnvelope.factID).
type decodedMessage struct {
	Source  string
	EventID string
	Fact    Fact
	// Unread names each optional field the decoder could not read and took as absent. The consumer
	// logs it; the message is not poison.
	Unread []string
	// LegacyName is set for an issue event named in the old form, its key alone
	// (subjectforms.IssueEventKey), of any project: the consumer counts and logs those while the
	// old form is still published (consumeMessage).
	LegacyName bool
	// Refused is a checks settlement of a repository this daemon hears that the decoder dropped:
	// the field it could not read, and how (decodeChecks), with what its payload gives of its
	// identity. It is nil when the decoder refused nothing, and Fact is nil when it is set. A
	// refused settlement is not poison, since no redelivery reads it otherwise, so the consumer
	// acknowledges it and logs it once per event id (consumeMessage).
	Refused *checksRefusal
}

// settlementRead is what a checks settlement's payload gives of the three fields a settlement is
// matched by across the listener's line that published it and the daemon's lines that dropped it
// (SettlementIdentity): its head (sha), its generation and its snapshot. Each is nil where its
// field is absent or does not read as its type: a string, a whole number and a string.
type settlementRead struct {
	SHA        *string
	Generation *int64
	Snapshot   *string
}

// readSettlement is what raw gives of its settlement's identity, each field read on its own, so a
// settlement refused for one of them is still named by the others.
func readSettlement(raw map[string]json.RawMessage) settlementRead {
	var read settlementRead
	if sha, ok := rawString(raw, "sha"); ok {
		read.SHA = &sha
	}
	if generation, ok := rawInt64(raw, "generation"); ok {
		read.Generation = &generation
	}
	if snapshot, ok := rawString(raw, "snapshot"); ok {
		read.Snapshot = &snapshot
	}
	return read
}

// attributes is what was read as log attributes, each field under its own name and every field
// that did not read left out.
func (s settlementRead) attributes() []any {
	var attributes []any
	if s.SHA != nil {
		attributes = append(attributes, "sha", *s.SHA)
	}
	if s.Generation != nil {
		attributes = append(attributes, "generation", *s.Generation)
	}
	if s.Snapshot != nil {
		attributes = append(attributes, "snapshot", *s.Snapshot)
	}
	return attributes
}

// checksRefusal is a checks settlement decodeChecks dropped: why, and what its payload gives of
// its identity, which decodeMessage carries to decodedMessage.Refused.
type checksRefusal struct {
	reason     string
	settlement settlementRead
}

// errIssueNameInNeitherForm is an issue event's name that carries its issue's key in neither form
// (subjectforms.IssueEventKey). It is poison, never another project's event acknowledged with no
// fact: a daemon that took a name it could not read for another project's would stop working its
// own in silence, which is what a daemon reading only the old form would do with the new one.
var errIssueNameInNeitherForm = errors.New("issue event named in neither form of an issue's name")

type envoyEnvelope struct {
	EventID        string `json:"event_id"`
	Source         string `json:"source"`
	SourceEventID  string `json:"source_event_id"`
	Topic          string `json:"topic"`
	DedupeKey      string `json:"dedupe_key"`
	IssuedAt       int64  `json:"issued_at"`
	PayloadSummary string `json:"payload_summary"`
	Payload        string `json:"payload"`
	TraceID        string `json:"trace_id"`
}

// decodeMessage decodes the Envoy envelope first, then the subject's source-specific payload.
// project is the daemon's Dispatch project: another project's issue event is a nil Fact, decided
// from the issue key its subject names before the envelope is read, so a message of another
// project's that does not decode is no give-up of this daemon's, and no record of it (every daemon
// hears every project's issue events). A name in neither form goes on to the decode, which refuses
// it (errIssueNameInNeitherForm). repositories are its configured repositories: a GitHub event
// whose payload names another repository is a nil Fact, whatever subject carried it.
func decodeMessage(subject, project string, repositories []ghrepo.Repository, data []byte) (decodedMessage, error) {
	if key, legacy, ok := subjectforms.IssueEventKey(subject); ok && !projectIssueKey(key, project) {
		return decodedMessage{LegacyName: legacy}, nil
	}
	var envelope envoyEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return decodedMessage{}, fmt.Errorf("decode Envoy envelope: %w", err)
	}
	if err := envelope.valid(); err != nil {
		return decodedMessage{}, err
	}

	var fact Fact
	var unread []string
	var err error
	var legacy bool
	var refused *checksRefusal
	switch {
	case subject == legionWakeSubject:
		if envelope.Source != "envoy" {
			return decodedMessage{}, fmt.Errorf("the listener wake subject has envelope source %q", envelope.Source)
		}
		fact, err = decodeListenerWake(envelope.EventID, envelope.Payload)
	case subject == defaultAssigneeSubject(project):
		if envelope.Source != "dispatch" {
			return decodedMessage{}, fmt.Errorf("Dispatch's default assignee subject has envelope source %q", envelope.Source)
		}
		fact, err = decodeDefaultAssignee(project, envelope.Payload)
	case strings.HasPrefix(subject, subjectforms.IssueTopicPrefix):
		if envelope.Source != "dispatch" {
			return decodedMessage{}, fmt.Errorf("Dispatch subject has envelope source %q", envelope.Source)
		}
		fact, unread, legacy, err = decodeDispatchFact(subject, project, envelope.Payload)
	case strings.HasPrefix(subject, "notifications.github."):
		if envelope.Source != "github" {
			return decodedMessage{}, fmt.Errorf("GitHub subject has envelope source %q", envelope.Source)
		}
		fact, unread, refused, err = decodeGitHubFact(subject, project, repositories, envelope.Payload)
	default:
		return decodedMessage{}, fmt.Errorf("unsupported durable subject %q", subject)
	}
	if err != nil {
		return decodedMessage{}, err
	}
	return decodedMessage{Source: envelope.Source, EventID: envelope.factID(), Fact: fact, Unread: unread, LegacyName: legacy, Refused: refused}, nil
}

func (e envoyEnvelope) valid() error {
	switch {
	case strings.TrimSpace(e.EventID) == "":
		return fmt.Errorf("Envoy envelope has no event_id")
	case strings.TrimSpace(e.Source) == "":
		return fmt.Errorf("Envoy envelope has no source")
	case strings.TrimSpace(e.SourceEventID) == "":
		return fmt.Errorf("Envoy envelope has no source_event_id")
	case strings.TrimSpace(e.Topic) == "":
		return fmt.Errorf("Envoy envelope has no topic")
	case strings.TrimSpace(e.DedupeKey) == "":
		return fmt.Errorf("Envoy envelope has no dedupe_key")
	case e.IssuedAt == 0:
		return fmt.Errorf("Envoy envelope has no issued_at")
	case strings.TrimSpace(e.PayloadSummary) == "":
		return fmt.Errorf("Envoy envelope has no payload_summary")
	case strings.TrimSpace(e.TraceID) == "":
		return fmt.Errorf("Envoy envelope has no trace_id")
	case !json.Valid([]byte(e.Payload)):
		return fmt.Errorf("Envoy envelope payload is not JSON")
	}
	return nil
}

// factID is the id the envelope's fact is recorded under in processed_events, which skips a fact
// already applied (ApplyFact, Applied): the envelope's event_id, except for a GitHub webhook
// delivery's envelope, whose dedupe_key names the delivery, "github." and its X-GitHub-Delivery
// (Envoy's GithubEnvelope), and which takes that dedupe_key. The listener mints a new event_id for
// every request GitHub sends, so a redelivery of the same delivery the stream stored again (asked
// for after the stream's 72-hour duplicate window, or published without a MsgId) would otherwise
// be a new fact. The topic is left out, though the stream's MsgId adds it: a review that mentions
// the trigger is published on its mention topics beside its own, each copy the same fact, decoded
// from the payload and applied once. A checks settlement keeps its event_id: its dedupe_key names a
// commit's settlement generation, which Envoy's CI store starts again once the commit's record
// expires, and processed_events is never pruned. A Dispatch event's event_id is its dedupe_key.
func (e envoyEnvelope) factID() string {
	if e.Source == "github" && e.DedupeKey == "github."+e.SourceEventID {
		return e.DedupeKey
	}
	return e.EventID
}

// legionWakeSubject is the one subject the Envoy listener reports a wake of a released pod on,
// and the whole of the wake durable's filter (OpenConsumers). It is a new leaf under
// notifications.legion.>, which the stream already carries, so it needs no stream change and no
// new grant. Every daemon consumes it: the listener does not know which daemon owns a session.
const legionWakeSubject = "notifications.legion.wake"

// decodeListenerWake decodes one wake report of the Envoy listener (ListenerWake). Its wire shape
// is `{"kind":..., "role":..., "session":..., "at":<epoch milliseconds>}`, the listener's own
// clock in At's shape, because the stream's store time is the NATS server's. A report of a kind
// this daemon does not know, and a role or agent report naming neither a role nor a session, is
// no fact rather than poison: a listener of a later release can report a kind this build has no
// wake for, and nothing is lost by acknowledging it.
//
// The report's clock is bounded to the daemon's own: a payload naming a time in the future is
// read as now. The subject is a stream subject, and a publisher that reaches it chooses every
// field of the envelope and the payload, this `at` and the envelope's source among them; a
// `listener_ready` dated in the year 9999 would otherwise hold R7's gate open for good
// (store.PodRecords.RecordListenerReady keeps the greatest it is given), so a session that can
// publish could have pods released into a silence nothing wakes them out of. The listener
// refuses a client publish on its own subject as well (packages/envoy/cmd/listener/api.go), so
// a forged report has to reach the bus directly; this is the half that holds whatever does.
func decodeListenerWake(eventID, payload string) (Fact, error) {
	var report struct {
		Kind    string `json:"kind"`
		Role    string `json:"role"`
		Session string `json:"session"`
		At      int64  `json:"at"`
	}
	if err := json.Unmarshal([]byte(payload), &report); err != nil {
		return nil, fmt.Errorf("decode the listener's wake report: %w", err)
	}
	switch report.Kind {
	case ListenerWakeReady:
	case ListenerWakeRole:
		if report.Role == "" {
			return nil, nil
		}
	case ListenerWakeAgent:
		if report.Session == "" {
			return nil, nil
		}
	default:
		return nil, nil
	}
	if report.At <= 0 {
		return nil, nil
	}
	at := time.UnixMilli(report.At).UTC()
	if now := time.Now().UTC(); at.After(now) {
		at = now
	}
	return ListenerWake{Kind: report.Kind, Role: report.Role, Session: report.Session, At: at, Report: eventID}, nil
}

// DefaultAssigneeEvent is the type of Dispatch's settings event that moves a project's default
// assignee (Dispatch's putProjectDefaultAssignee): the one name the daemon's decode of it and the
// replay's read of a window's settings events (replay.ParseSettingsEvent) both read.
const DefaultAssigneeEvent = "settings.project_default_assignee.updated"

// defaultAssigneeSubject is the subject Dispatch publishes project's DefaultAssigneeEvent on (its
// outbox's notifications.dispatch.project.<project>.<type>): the one settings subject the
// Dispatch durable's filter takes beside the issue events (OpenConsumers), since the approvals
// signal reads the project's default assignee and no other setting.
func defaultAssigneeSubject(project string) string {
	return "notifications.dispatch.project." + project + "." + DefaultAssigneeEvent
}

// decodeDefaultAssignee decodes Dispatch's move of project's default assignee, as its event log
// and its stream carry it (the event, with its payload {project, assignee}), into
// DispatchDefaultAssignee. An event that lacks its id, sequence or time, is of another type or
// project, or names no assignee is poison: Dispatch writes every one with all of them, since a
// project always names a default (its putProjectDefaultAssignee refuses a body that names none),
// as the replay reads the same event (replay.ParseSettingsEvent).
func decodeDefaultAssignee(project, payload string) (Fact, error) {
	var event struct {
		ID        int64     `json:"id"`
		Project   string    `json:"project"`
		Seq       int64     `json:"seq"`
		Type      string    `json:"type"`
		CreatedAt time.Time `json:"created_at"`
		Payload   struct {
			Assignee *string `json:"assignee"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, fmt.Errorf("decode Dispatch's default assignee event: %w", err)
	}
	switch {
	case event.ID <= 0 || event.Seq <= 0 || event.CreatedAt.IsZero():
		return nil, fmt.Errorf("Dispatch's default assignee event lacks its id, seq or created_at")
	case event.Type != DefaultAssigneeEvent:
		return nil, fmt.Errorf("Dispatch's default assignee subject carries an event of type %q", event.Type)
	case event.Project != project:
		return nil, fmt.Errorf("Dispatch's default assignee event of %s names the project %q", project, event.Project)
	case event.Payload.Assignee == nil || *event.Payload.Assignee == "":
		return nil, fmt.Errorf("Dispatch's default assignee event of %s names no assignee", project)
	}
	return DispatchDefaultAssignee{Assignee: *event.Payload.Assignee, Seq: event.Seq, At: event.CreatedAt.UTC()}, nil
}

type dispatchEvent struct {
	ID       int64  `json:"id"`
	IssueKey string `json:"issue_key"`
	Seq      int64  `json:"seq"`
	Actor    struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"actor"`
	Notify *bool  `json:"notify"`
	Type   string `json:"type"`
	// CreatedAt is Dispatch's time of the event, zero when the event carries none. A review's
	// event shares it with the review's own row, which Dispatch writes in the same transaction.
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

// mark is the event's place in its issue's event log.
func (e dispatchEvent) mark() record.EventMark {
	return record.EventMark{Seq: e.Seq, At: e.CreatedAt.UTC()}
}

// daemonActor says whether the daemon made the event: its actor is the session the daemon writes
// every Dispatch status and message of the event's issue under (dispatch.DaemonSession). It is the
// one statement of that rule, which the decoder (DispatchIssue.DaemonActor) and the set-back's
// check (StatusChange.DaemonActor) both read.
func (e dispatchEvent) daemonActor() bool {
	return e.Actor.Kind == "session" && e.Actor.ID == dispatch.DaemonSession(e.IssueKey)
}

// decodeDispatchFact decodes one Dispatch issue event of project from its name in either form
// (subjectforms.IssueEventKey), reporting whether the name was the old one. A name that carries
// its key in neither form is poison (errIssueNameInNeitherForm).
func decodeDispatchFact(subject, project, payload string) (Fact, []string, bool, error) {
	key, legacy, ok := subjectforms.IssueEventKey(subject)
	if !ok {
		return nil, nil, false, fmt.Errorf("%w: %s", errIssueNameInNeitherForm, subject)
	}
	fact, unread, err := decodeDispatchIssueEvent(key, project, payload)
	return fact, unread, legacy, err
}

// decodeDispatchIssueEvent decodes one Dispatch event of issue key, for project. The stream carries
// every project's events; one whose key is not a project issue key is another daemon's, and is
// skipped before its payload is read, so it is never poison here. A field the mirror only records
// (an issue's claim, an ask's kind) that cannot be read is taken as unknown and named in unread,
// rather than making the event poison: an event the workflow acts on is never lost to a field it
// does not act on.
func decodeDispatchIssueEvent(key, project, payload string) (Fact, []string, error) {
	if !projectIssueKey(key, project) {
		return nil, nil, nil
	}
	var event dispatchEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, nil, fmt.Errorf("decode Dispatch event: %w", err)
	}
	if event.ID <= 0 || event.Seq <= 0 || event.Notify == nil || !claim.IsIssueKey(event.IssueKey) || event.Type == "" || !isJSONObject(event.Payload) {
		return nil, nil, fmt.Errorf("Dispatch durable message payload is not a valid Dispatch event")
	}
	if key != event.IssueKey {
		return nil, nil, fmt.Errorf("Dispatch durable subject key %s disagrees with event issue_key %s", key, event.IssueKey)
	}
	var unread []string
	fact, err := decodeDispatchEvent(event, &unread)
	if err != nil {
		return nil, nil, err
	}
	return fact, unread, nil
}

// projectIssueKey says whether key is an issue key of project, which the daemon decodes events
// of; any other key is another daemon's.
func projectIssueKey(key, project string) bool {
	keyProject, _, _ := strings.Cut(key, "-")
	return keyProject == project && claim.IsIssueKey(key)
}

// ProjectBranchIssue is the issue key branch names when branch is an issue's branch
// (claim.BranchIssue) of project, and false for any other branch: another project's issue in a
// repository the two share, and a branch that names no issue. It is the one rule that decides
// whose a pull request is for the merge queue signal, at the decode of an `enqueued` event and
// wherever admission and the boot read a pull request's branch.
func ProjectBranchIssue(branch, project string) (string, bool) {
	issue, ok := claim.BranchIssue(branch)
	if !ok || !projectIssueKey(issue, project) {
		return "", false
	}
	return issue, true
}

// DecodeDispatchEvent decodes one event of issue key of project as Dispatch's issue event log
// serves it (dispatch.Client.EventsAfter), which is the shape its durable stream carries: the
// boot catch-up applies what the stream did not deliver through the live decoder, so each event
// becomes the fact its live delivery would have been. It returns the fields it took as unknown,
// as the live decoder logs them.
func DecodeDispatchEvent(project, key string, event []byte) (Fact, []string, error) {
	return decodeDispatchIssueEvent(key, project, string(event))
}

// isIssueEvent says whether a Dispatch event of type typ is an issue event: one that carries the
// issue as the event leaves it. decodeDispatchEvent reads a DispatchIssue from each such event,
// and walkIssueEvents gives each such event of an issue's event log, so the two read one list.
func isIssueEvent(typ string) bool {
	switch typ {
	case "issue.created", "issue.updated", "issue.closed":
		return true
	}
	return false
}

// decodeDispatchEvent decodes a checked Dispatch event of the daemon's project by its type, adding
// to unread each field it took as unknown.
func decodeDispatchEvent(event dispatchEvent, unread *[]string) (Fact, error) {
	if isIssueEvent(event.Type) {
		var issue struct {
			Key          string          `json:"key"`
			Status       string          `json:"status"`
			Title        string          `json:"title"`
			Parent       *string         `json:"parent"`
			Rank         string          `json:"rank"`
			Priority     json.RawMessage `json:"priority"`
			Labels       []string        `json:"labels"`
			ResolvedRepo json.RawMessage `json:"resolved_repo"`
			// An event retained from a Dispatch that did not yet send blocked_by decodes to no
			// blockers, as an issue waiting on nothing does: unlike a missing repository, which the
			// consumer fills, nothing reads an absence here.
			BlockedBy []string `json:"blocked_by"`
			// Claim is present on every issue event of a Dispatch that has claims (null for none),
			// and absent from an event retained from before it did, which says nothing of the claim.
			Claim json.RawMessage `json:"claim"`
			// Assignee is present on every issue event of a Dispatch that has assignees (null for
			// none), and absent from an event retained from before it did.
			Assignee json.RawMessage `json:"assignee"`
		}
		if err := json.Unmarshal(event.Payload, &issue); err != nil {
			return nil, fmt.Errorf("decode Dispatch issue payload: %w", err)
		}
		if issue.Key != event.IssueKey {
			return nil, fmt.Errorf("Dispatch issue event payload key disagrees with event issue_key")
		}
		// A Dispatch that resolves an issue's repository sends resolved_repo on every issue event,
		// null for none. An event JetStream retained from an older Dispatch has no such field: it
		// still carries its actor and sequence, which the workflow's fences read, so it is decoded
		// with its repository missing, for the consumer's PrepareRepository to fill.
		missing := issue.ResolvedRepo == nil
		var resolved *string
		if !missing {
			if err := json.Unmarshal(issue.ResolvedRepo, &resolved); err != nil {
				return nil, fmt.Errorf("decode Dispatch issue payload resolved_repo: %w", err)
			}
		}
		parent := ""
		if issue.Parent != nil {
			if !claim.IsIssueKey(*issue.Parent) {
				return nil, fmt.Errorf("Dispatch issue event payload has an invalid parent key")
			}
			parent = *issue.Parent
		}
		actorSession := ""
		daemonActor := event.daemonActor()
		if event.Actor.Kind == "session" && !daemonActor {
			actorSession = event.Actor.ID
		}
		claimedBy, claimKnown := IssueEventClaim(issue.Claim, unread)
		assignee, assigneeKnown := eventPerson(issue.Assignee, "assignee", unread)
		return DispatchIssue{Key: event.IssueKey, Seq: event.Seq, Type: event.Type, Status: issue.Status, Title: issue.Title, Parent: parent, Rank: issue.Rank,
			Priority: issueEventPriority(issue.Priority, unread), HandedOver: dispatch.CarriesLegionLabel(issue.Labels), Labels: issue.Labels,
			ActorSession: actorSession, DaemonActor: daemonActor, ResolvedRepo: deref(resolved),
			ResolvedRepoMissing: missing, Blockers: issue.BlockedBy, ClaimedBy: claimedBy, ClaimKnown: claimKnown,
			Assignee: assignee, AssigneeKnown: assigneeKnown}, nil
	}
	switch event.Type {
	case "artifact.approved", "artifact.changes_requested":
		var artifact struct {
			ArtifactID string `json:"artifact_id"`
			Version    int    `json:"version"`
			Reason     string `json:"reason"`
		}
		if err := json.Unmarshal(event.Payload, &artifact); err != nil {
			return nil, fmt.Errorf("decode Dispatch artifact payload: %w", err)
		}
		if artifact.ArtifactID == "" {
			return nil, fmt.Errorf("Dispatch %s payload has no artifact_id", event.Type)
		}
		if artifact.Version <= 0 {
			return nil, fmt.Errorf("Dispatch %s payload has no positive integer version", event.Type)
		}
		kind := DispatchArtifactApproved
		if event.Type == "artifact.changes_requested" {
			if artifact.Reason == "" {
				return nil, fmt.Errorf("Dispatch artifact.changes_requested payload has no reason")
			}
			kind = DispatchArtifactChangesRequested
		}
		return DispatchArtifact{Key: event.IssueKey, ArtifactID: artifact.ArtifactID, Kind: kind, Version: artifact.Version, Reason: artifact.Reason,
			Actor: dispatch.Actor(event.Actor), Mark: event.mark()}, nil
	case "artifact.version":
		var artifact struct {
			ArtifactID string `json:"artifact_id"`
			Version    struct {
				Number int `json:"number"`
			} `json:"version"`
		}
		if err := json.Unmarshal(event.Payload, &artifact); err != nil {
			return nil, fmt.Errorf("decode Dispatch artifact version payload: %w", err)
		}
		if artifact.ArtifactID == "" {
			return nil, fmt.Errorf("Dispatch artifact.version payload has no artifact_id")
		}
		if artifact.Version.Number <= 0 {
			return nil, fmt.Errorf("Dispatch artifact.version payload has no positive integer version.number")
		}
		return DispatchArtifact{Key: event.IssueKey, ArtifactID: artifact.ArtifactID, Kind: DispatchArtifactVersion, Version: artifact.Version.Number,
			Mark: event.mark(), Actor: dispatch.Actor(event.Actor)}, nil
	case "ask.opened":
		ask, err := decodeAskPayload(event, unread)
		if err != nil {
			return nil, err
		}
		turn := askTurn(event, ask, unread)
		// Settlement restoring a decision block it retracted appends ask.opened with the block's
		// original author: nobody asked anything, so it is the ask's state alone.
		if ask.Restored {
			return DispatchAskRestored{Key: event.IssueKey, Ask: ask.ID, Kind: ask.kind, Artifact: ask.artifact(), Open: true, Mark: event.mark(), Turn: turn}, nil
		}
		// A session author with no id cannot be matched to the tree, so the ask is mirrored as one no
		// session of the tree opened, and the author is reported, rather than the event lost.
		session := ""
		if ask.Author.Kind == "session" {
			if ask.Author.ID == "" {
				*unread = append(*unread, "ask.opened has a session author with no id")
			}
			session = ask.Author.ID
		}
		return DispatchAskOpened{Key: event.IssueKey, Ask: ask.ID, Kind: ask.kind, Artifact: ask.artifact(), AuthorSession: session, Mark: event.mark(), Turn: turn}, nil
	case "ask.answered", "ask.resolved":
		ask, err := decodeAskPayload(event, unread)
		if err != nil {
			return nil, err
		}
		turn := askTurn(event, ask, unread)
		// Settlement restoring a decision block it retracted appends ask.answered with the answer
		// given before the retraction, credited to the editor who restored the block: nobody
		// answered anything, so it is the ask's state alone.
		if ask.Restored {
			return DispatchAskRestored{Key: event.IssueKey, Ask: ask.ID, Kind: ask.kind, Artifact: ask.artifact(), Mark: event.mark(), Turn: turn}, nil
		}
		closed := DispatchAskClosed{Key: event.IssueKey, Ask: ask.ID, Kind: ask.kind, Artifact: ask.artifact(), State: strings.TrimPrefix(event.Type, "ask."),
			Actor: dispatch.Actor(event.Actor), Mark: event.mark(), Turn: turn}
		// A person's decline is the one resolution the daemon acts on: it resumes the root's
		// architect with Dispatch's own words and the person's note (workflow's resumeTask), so
		// both travel as data from Dispatch rather than as a copy of four sentences here. Any
		// other resolution kind, and a close whose payload carries none, is the close alone.
		if ask.Resolution != nil && ask.Resolution.Kind == ResolutionDeclined {
			closed.Decline = &record.AskDecline{Note: ask.Resolution.Reason, Words: ask.Resolution.DispatchWords}
		}
		return closed, nil
	case "ask.handed_back":
		// An approval request its asker returned to the person (Dispatch's handBackApprovalAsk):
		// whatever the thread held, it now waits on the human, as a session's human-turn reply does.
		ask, err := decodeAskPayload(event, unread)
		if err != nil {
			return nil, err
		}
		return DispatchAskReply{Key: event.IssueKey, Ask: ask.ID, Kind: ask.kind, Artifact: ask.artifact(), WaitingOn: "human",
			Actor: dispatch.Actor(event.Actor), Mark: event.mark(), Turn: askTurn(event, ask, unread)}, nil
	case "ask.asked_of":
		// A hand-on moves which person the ask waits on, and no root waits on a person, so the
		// workflow reads nothing of it: it is the approvals signal's alone.
		ask, err := decodeAskPayload(event, unread)
		if err != nil {
			return nil, err
		}
		return DispatchAskTurn{Key: event.IssueKey, Turn: askTurn(event, ask, unread), Mark: event.mark()}, nil
	case "ask.edited":
		// An edit changes an ask's turn only when it moves an approval request: Dispatch appends
		// ask.edited in a version's own transaction for the document's open approval request
		// (docs.MoveApprovalAsk), and shows a request whose requested_version is below its version
		// waiting on its agent (approvalMoved in waitingOnExpression). Any other edit, of a question
		// or of a request still at the version handed to the person, leaves the turn as it was, and
		// is the approvals signal's alone (DispatchAskTurn), which reads its state and asked_of. A
		// request edit with no requested_version or no version (an event recorded before Dispatch
		// kept them) cannot say whether it moved, so it moves no turn either, and is reported.
		ask, err := decodeAskPayload(event, unread)
		if err != nil {
			return nil, err
		}
		turn := askTurn(event, ask, unread)
		other := DispatchAskTurn{Key: event.IssueKey, Turn: turn, Mark: event.mark()}
		if ask.kind != record.AskApproval || ask.Approval == nil || ask.Approval.ArtifactID == "" {
			return other, nil
		}
		if ask.Approval.Version == nil || ask.Approval.RequestedVersion == nil {
			*unread = append(*unread, "ask.edited of an approval request has no version or requested_version")
			return other, nil
		}
		if *ask.Approval.RequestedVersion >= *ask.Approval.Version {
			return other, nil
		}
		return DispatchApprovalMoved{Key: event.IssueKey, Ask: ask.ID, Artifact: ask.Approval.ArtifactID, Mark: event.mark(), Turn: turn}, nil
	case "comment.created", "comment.answered":
		// comment.answered is a reply a session sends through a delivery's callback (Dispatch's
		// comment_delivery), which joins its thread as comment.created does and carries the same
		// ask_id, ask_state and ask_waiting_on. A comment on no ask is no fact.
		var comment struct {
			ID        string `json:"id"`
			Ask       string `json:"ask_id"`
			State     string `json:"ask_state"`
			WaitingOn string `json:"ask_waiting_on"`
		}
		if err := json.Unmarshal(event.Payload, &comment); err != nil {
			return nil, fmt.Errorf("decode Dispatch %s payload: %w", event.Type, err)
		}
		if comment.Ask == "" {
			return nil, nil
		}
		// A turn the daemon cannot read could be either, so the reply moves nothing.
		switch comment.WaitingOn {
		case "", "human", "agent":
		default:
			*unread = append(*unread, fmt.Sprintf("ask_waiting_on %q is no turn", comment.WaitingOn))
			return nil, nil
		}
		// Only an open ask has a turn, so a reply with no turn is on a closed ask (DispatchAskReply).
		// The ask's state says so outright where Dispatch sends it; a reply whose state and turn
		// disagree could mean either, so it moves nothing.
		if comment.State != "" && (comment.State == "open") != (comment.WaitingOn != "") {
			*unread = append(*unread, fmt.Sprintf("ask_state %q disagrees with ask_waiting_on %q", comment.State, comment.WaitingOn))
			return nil, nil
		}
		turn := signals.AskEvent{Kind: signals.AskCommented, Ask: comment.Ask, Issue: event.IssueKey, WaitingOn: signals.AskWaiting(comment.WaitingOn)}
		return DispatchAskReply{Key: event.IssueKey, Ask: comment.Ask, WaitingOn: comment.WaitingOn, Actor: dispatch.Actor(event.Actor),
			Mark: event.mark(), Comment: comment.ID, Turn: turn}, nil
	case "issue.claimed", "issue.released":
		var payload struct {
			Claim json.RawMessage `json:"claim"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode Dispatch %s payload: %w", event.Type, err)
		}
		// A claim event whose claim cannot be named records nothing: the next issue event, the
		// periodic re-read or the next boot's listing carries the claim Dispatch shows.
		holder, known := IssueEventClaim(payload.Claim, unread)
		if known && event.Type == "issue.claimed" && holder == "" {
			*unread = append(*unread, "claim is null on issue.claimed")
			known = false
		}
		if !known {
			return nil, nil
		}
		return DispatchClaim{Key: event.IssueKey, Seq: event.Seq, ClaimedBy: holder}, nil
	default:
		return nil, nil
	}
}

// IssueEventClaim reads an issue event's claim, `<kind>:<id>`, "" for null, and whether the event
// showed it at all: an absent field is unknown, and so is a claim that is no claim object or one
// naming no actor, each of those two named in unread. The daemon's decoder reads every claim an
// event carries by it, and a replay reads the claims of its copy of Dispatch by it.
func IssueEventClaim(raw json.RawMessage, unread *[]string) (string, bool) {
	if raw == nil {
		return "", false
	}
	var claimed *struct {
		Actor struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"actor"`
	}
	if err := json.Unmarshal(raw, &claimed); err != nil {
		*unread = append(*unread, fmt.Sprintf("claim %s is not a claim: %v", raw, err))
		return "", false
	}
	if claimed == nil {
		return "", true
	}
	if claimed.Actor.Kind == "" || claimed.Actor.ID == "" {
		*unread = append(*unread, fmt.Sprintf("claim %s names no actor", raw))
		return "", false
	}
	return claimed.Actor.Kind + ":" + claimed.Actor.ID, true
}

// issueEventPriority reads an issue event's own priority, Dispatch's 0 for P0 to 3 for P3 or null,
// as the record keeps it (record.DecodePriority); an absent field is none. A value Dispatch never
// writes, a level outside 0 to 3 or no integer at all, is none and named in unread rather than
// making the event poison: the event can carry a status the workflow acts on.
func issueEventPriority(raw json.RawMessage, unread *[]string) record.Priority {
	if raw == nil {
		return record.NoPriority
	}
	priority, read, err := record.DecodePriority(raw)
	switch {
	case err != nil:
		*unread = append(*unread, fmt.Sprintf("priority %s is not a priority: %v", raw, err))
	case !read:
		*unread = append(*unread, fmt.Sprintf("priority %s is no level Dispatch writes", raw))
	}
	return priority
}

// askPayload is what the daemon reads of an ask.* event's payload, which is the ask itself
// (Dispatch's model.AskEventPayload and AskEditEventPayload): what the approvals signal reads of
// it (AskTurnPayload), the author of an opening, settlement's restored mark, an approval request's
// document with the version it now names and the version its agent last handed to the person, and
// a close's resolution (Dispatch's model.AskResolution), which says why a resolved ask needs no
// human answer and carries the person's note and Dispatch's own words apart.
type askPayload struct {
	AskTurnPayload
	Author struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"author"`
	Restored bool `json:"restored"`
	Approval *struct {
		ArtifactID string `json:"artifact_id"`
		AskVersions
	} `json:"approval"`
	Resolution *struct {
		Kind          string `json:"kind"`
		Reason        string `json:"reason"`
		DispatchWords string `json:"dispatch_words"`
	} `json:"resolution"`
	kind string
}

// AskTurnPayload is what the approvals signal reads of an ask event's payload, the ask as its row
// holds it after the event: its id, its kind, its state (nil where the payload carries none), and
// the person it is handed to (AskedOf), JSON null for nobody and absent from an event recorded
// before Dispatch had it, which says nothing of it. An approval request's versions sit in the
// payload's approval beside its document (AskVersions). The daemon's decode of an ask event
// (askPayload) and the replay's read of a window's events both read it, and AskTurn turns it into
// the event the signal reads.
type AskTurnPayload struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	State   *string         `json:"state"`
	AskedOf json.RawMessage `json:"asked_of"`
}

// AskVersions is an approval request's versions as its ask event's payload names them: the
// version of the document the request now names, and the version its agent last handed to the
// person.
type AskVersions struct {
	Version          *int `json:"version"`
	RequestedVersion *int `json:"requested_version"`
}

// AskTurn is the ask event of type typ on issue, of p, as the approvals signal reads it
// (signals.AskEvent): its kind by the part of typ after `ask.`, its ask and issue, its state
// (open, or any other state as closed, nothing where the payload carries none), an approval
// request's versions (approval, nil where the payload names none), and its asked_of person where
// the payload names one, null included. It is the one reading of an ask event's turn, the daemon's
// and the replay's. An asked_of that is no person is its error, beside the turn read without it:
// the daemon takes it as absent and names it in unread (askTurn), and the replay refuses the event.
func (p AskTurnPayload) AskTurn(typ, issue string, approval *AskVersions) (signals.AskEvent, error) {
	turn := signals.AskEvent{Kind: signals.AskEventKind(strings.TrimPrefix(typ, "ask.")), Ask: p.ID, Issue: issue}
	if p.State != nil {
		turn.State = signals.ReportsClosed
		if *p.State == "open" {
			turn.State = signals.ReportsOpen
		}
	}
	if p.Kind == record.AskApproval && approval != nil {
		turn.Approval = &signals.AskApproval{Version: approval.Version, RequestedVersion: approval.RequestedVersion}
	}
	if p.AskedOf != nil {
		var person *string
		if err := json.Unmarshal(p.AskedOf, &person); err != nil {
			return turn, err
		}
		turn.AskedOf, turn.AskedOfSet = deref(person), true
	}
	return turn, nil
}

// askTurn is event, of ask, as the approvals signal reads it (AskTurnPayload.AskTurn), an asked_of
// that is no person taken as absent and named in unread, as eventPerson names a member that is
// no person.
func askTurn(event dispatchEvent, ask askPayload, unread *[]string) signals.AskEvent {
	var versions *AskVersions
	if ask.Approval != nil {
		versions = &ask.Approval.AskVersions
	}
	turn, err := ask.AskTurn(event.Type, event.IssueKey, versions)
	if err != nil {
		*unread = append(*unread, fmt.Sprintf("asked_of %s is no person: %v", ask.AskedOf, err))
	}
	return turn
}

// eventPerson reads a person member of an event (an issue's assignee): the login, "" for null, and
// whether the event carried the member at all. A member that is no login is taken as absent and
// named in unread, never poison: the event can carry what the workflow acts on.
func eventPerson(raw json.RawMessage, field string, unread *[]string) (string, bool) {
	if raw == nil {
		return "", false
	}
	var person *string
	if err := json.Unmarshal(raw, &person); err != nil {
		*unread = append(*unread, fmt.Sprintf("%s %s is no person: %v", field, raw, err))
		return "", false
	}
	return deref(person), true
}

// decodeAskPayload reads event's ask, refusing one with no id, with its kind as askKind keeps it.
func decodeAskPayload(event dispatchEvent, unread *[]string) (askPayload, error) {
	var ask askPayload
	if err := json.Unmarshal(event.Payload, &ask); err != nil {
		return askPayload{}, fmt.Errorf("decode Dispatch %s payload: %w", event.Type, err)
	}
	if ask.ID == "" {
		return askPayload{}, fmt.Errorf("Dispatch %s payload has no id", event.Type)
	}
	ask.kind = askKind(ask.Kind, unread)
	return ask, nil
}

// artifact is the document an approval request asks a person to approve, "" for any other ask.
func (a askPayload) artifact() string {
	if a.kind != record.AskApproval || a.Approval == nil {
		return ""
	}
	return a.Approval.ArtifactID
}

// askKind is an ask event's kind as the mirror keeps it: record.AskQuestion or record.AskApproval,
// and "" for an absent kind or one this daemon does not know (an event retained from before
// Dispatch had the two, whose kind is `action`), the latter named in unread.
func askKind(kind string, unread *[]string) string {
	switch kind {
	case record.AskQuestion, record.AskApproval, "":
		return kind
	}
	*unread = append(*unread, fmt.Sprintf("ask kind %q is not one the daemon knows", kind))
	return ""
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func isJSONObject(raw json.RawMessage) bool {
	var item map[string]json.RawMessage
	return json.Unmarshal(raw, &item) == nil && item != nil
}

// decodeGitHubFact decodes one GitHub event's payload into its fact, with each optional field it
// could not read (unread). refused is set only for a checks settlement of a heard repository that
// decodeChecks dropped: a payload that is no object, names a repository this daemon does not hear,
// or is of another kind is no checks settlement of this daemon, and is no fact in silence.
func decodeGitHubFact(subject, project string, repositories []ghrepo.Repository, payload string) (fact Fact, unread []string, refused *checksRefusal, err error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &raw); err != nil || raw == nil {
		return nil, nil, nil, nil
	}
	// The payload names the event's repository; the subject's segments cannot, since a name may hold
	// a dot. Only the event of a repository GitHub intake hears (ConsumerSpec.Repositories, as
	// SetRepositories changes them) is this daemon's, matched case-insensitively as GitHub matches
	// owner and repository names, and its fact names the heard repository. The heard set already
	// carries both names of a configured rename (the daemon's heardRepositories), so the match
	// itself takes no equivalence.
	repo, _ := rawString(raw, "repo")
	at := slices.IndexFunc(repositories, func(heard ghrepo.Repository) bool { return strings.EqualFold(heard.String(), repo) })
	if at < 0 {
		return nil, nil, nil, nil
	}
	repository := repositories[at]
	kind, ok := rawString(raw, "kind")
	if !ok {
		return nil, nil, nil, nil
	}
	switch kind {
	case "pr":
		fact, err = decodePullRequest(project, repository, raw)
	case "review":
		fact, unread, err = decodeReview(repository, raw)
	case "push":
		fact, err = decodePush(repository, raw)
	case "checks":
		var reason string
		if fact, unread, reason = decodeChecks(subject, repository, raw); reason != "" {
			refused = &checksRefusal{reason: reason, settlement: readSettlement(raw)}
		}
	}
	// Comments, and every other kind, route to the current role but do not change the durable
	// workflow record.
	return fact, unread, refused, err
}

// decodePullRequest decodes a pull request's lifecycle event on an issue's branch
// (claim.BranchIssue). One on any other branch is no fact, read before anything else of the
// event, so it is acknowledged without a transaction: the workflow records a pull request only on
// an issue's branch (PullRequestOpened), and every later event applies only to a recorded one. A
// repository the daemon hears whose other pull requests open and move many times a minute would
// otherwise cost the daemon one transaction under the fact lock (ApplyFact) for every one, all of
// them changing nothing. Every action carries head_ref.
func decodePullRequest(project string, repository ghrepo.Repository, raw map[string]json.RawMessage) (Fact, error) {
	branch, _ := rawString(raw, "head_ref")
	_, isIssueBranch := claim.BranchIssue(branch)
	if !isIssueBranch {
		return nil, nil
	}
	number, action, ok := githubIdentity(raw)
	if !ok {
		return nil, nil
	}
	repo := repository.String()
	sha, _ := rawString(raw, "head_sha")
	// The listener normalizes GitHub's base.ref to base_ref; one that predates the field carries
	// none, and the base is then left as the record has it, for the next read to fill.
	base, _ := rawString(raw, "base_ref")
	body, _ := rawString(raw, "body")
	updatedAt, _ := rawTimestamp(raw, "updated_at")
	switch action {
	case "opened", "reopened":
		// A reopened pull request is open again, recorded as when it opened.
		if sha == "" {
			return nil, nil
		}
		url, _ := rawString(raw, "url")
		return PullRequestOpened{Repo: repo, Number: number, Branch: branch, HeadSHA: sha, Base: base, Body: body, URL: url, UpdatedAt: updatedAt,
			Reopened: action == "reopened"}, nil
	case "synchronize":
		if sha == "" {
			return nil, nil
		}
		before, _ := rawString(raw, "before")
		sender, _ := rawString(raw, "sender")
		return PullRequestSynchronized{Repo: repo, Number: number, Branch: branch, Before: before, Sender: sender, HeadSHA: sha, Base: base, Body: body,
			UpdatedAt: updatedAt}, nil
	case "closed":
		merged, _ := rawString(raw, "merged")
		if merged == "true" {
			mergeSHA, _ := rawString(raw, "merge_commit_sha")
			return PullRequestMerged{Repo: repo, Number: number, MergeSHA: mergeSHA, HeadSHA: sha}, nil
		}
		return PullRequestClosed{Repo: repo, Number: number, HeadSHA: sha, UpdatedAt: updatedAt}, nil
	case "dequeued":
		// A removal from the merge queue is told at the head it left the queue at. One the
		// listener sends with no head names none, so it is no fact, as a synchronize with none is no fact.
		if sha == "" {
			return nil, nil
		}
		return PullRequestDequeued{Repo: repo, Number: number, HeadSHA: sha}, nil
	case "enqueued":
		// Only pull requests whose branch names an issue of this daemon's project feed its merge
		// queue signal: another project's pull request in a shared repository keeps nothing. One the
		// listener sends with no head is no fact, as a removal with none is no fact.
		if _, ours := ProjectBranchIssue(branch, project); !ours || sha == "" {
			return nil, nil
		}
		return PullRequestEnqueued{Repo: repo, Number: number, Branch: branch}, nil
	case "review_requested", "review_request_removed":
		// A review request opened or withdrawn moves what a `blocked` head's round waits on, and
		// the read it asks for counts the requests; one with no head names none to read for.
		if sha == "" {
			return nil, nil
		}
		return PullRequestReviewRequests{Repo: repo, Number: number, HeadSHA: sha}, nil
	default:
		return nil, nil
	}
}

// The kinds of a pull request's timeline event that move it in the merge queue: GitHub's own
// names, as its REST timeline API spells them.
const (
	QueueAdded   = "added_to_merge_queue"
	QueueRemoved = "removed_from_merge_queue"
	QueueMerged  = "merged"
)

// IsQueueMove says whether kind, the kind of a pull request's timeline event, moves it in the
// merge queue (QueueAdded, QueueRemoved or QueueMerged).
func IsQueueMove(kind string) bool {
	return kind == QueueAdded || kind == QueueRemoved || kind == QueueMerged
}

// queueEventURL matches the url GitHub's REST timeline API gives each issue event,
// <API>/repos/<owner>/<name>/issues/events/<id>, the API at any host and path, and captures
// GitHub's own owner and repository name: the repository the event is of as GitHub names it now,
// whichever name the read was made under, which differs from the name the record keeps after a
// transfer.
var queueEventURL = regexp.MustCompile(`^https?://[^/?#]+(?:/[^?#]*)?/repos/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/issues/events/[0-9]+$`)

// QueueTimelineEvent is a merge-queue event of a pull request's timeline, read
// (ParseQueueTimelineEvent): the pull request, the event's kind (GitHub's `event`: QueueAdded,
// QueueRemoved or QueueMerged), GitHub's id of it, the repository its url names (owner/name, as
// GitHub names it now) and when GitHub recorded it.
type QueueTimelineEvent struct {
	PullRequest int
	Event       string
	ID          int64
	Repo        string
	At          time.Time
}

// Signal is e as the queue signal keeps it: an entry for an added_to_merge_queue event, and an
// exit for a removal or a merge.
func (e QueueTimelineEvent) Signal() signals.QueueEvent {
	return signals.QueueEvent{ID: e.ID, Repo: e.Repo, PullRequest: e.PullRequest, At: e.At, Enters: e.Event == QueueAdded}
}

// ParseQueueTimelineEvent reads item, an event of pull request number's timeline as GitHub's REST
// timeline API serves it (GET /repos/<owner>/<name>/issues/<n>/timeline): the one reading of a
// merge-queue event, the daemon's read of a timeline and the replay's read of a window's queue
// file alike. It refuses an item that is no merge-queue event or that has no created_at, one with
// no event id, and one whose url names no repository (queueEventURL), each refusal saying which,
// rather than an event with a time or a repository made up.
func ParseQueueTimelineEvent(number int, item []byte) (QueueTimelineEvent, error) {
	var event struct {
		ID        *int64     `json:"id"`
		URL       *string    `json:"url"`
		Event     *string    `json:"event"`
		CreatedAt *time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(item, &event); err != nil {
		return QueueTimelineEvent{}, fmt.Errorf("the timeline event of #%d is no timeline event: %w", number, err)
	}
	if event.Event == nil || !IsQueueMove(*event.Event) || event.CreatedAt == nil {
		return QueueTimelineEvent{}, fmt.Errorf("the timeline event of #%d is no merge queue event with its created_at", number)
	}
	if event.ID == nil || *event.ID <= 0 {
		return QueueTimelineEvent{}, fmt.Errorf("the %s event of #%d holds no event id", *event.Event, number)
	}
	if event.URL == nil {
		return QueueTimelineEvent{}, fmt.Errorf("the %s event of #%d holds no url, so it names no repository", *event.Event, number)
	}
	named := queueEventURL.FindStringSubmatch(*event.URL)
	if named == nil {
		return QueueTimelineEvent{}, fmt.Errorf("the url %q of the %s event of #%d names no repository, as <API>/repos/<owner>/<name>/issues/events/<id> does",
			*event.URL, *event.Event, number)
	}
	return QueueTimelineEvent{PullRequest: number, Event: *event.Event, ID: *event.ID, Repo: named[1] + "/" + named[2], At: event.CreatedAt.UTC()}, nil
}

func decodeReview(repository ghrepo.Repository, raw map[string]json.RawMessage) (Fact, []string, error) {
	number, action, ok := githubIdentity(raw)
	if !ok || action != "submitted" {
		return nil, nil, nil
	}
	state, _ := rawString(raw, "state")
	commitID, _ := rawString(raw, "commit_id")
	headSHA, _ := rawString(raw, "head_sha")
	author, _ := rawString(raw, "author")
	body, _ := rawString(raw, "body")
	bodyTruncated, _ := rawString(raw, "body_truncated")
	// The listener carries GitHub's review id as a decimal string; one that predates it carries
	// none, and the review is then ordered by when it is processed.
	var id int64
	if text, ok := rawString(raw, "review_id"); ok && text != "" {
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil || parsed <= 0 {
			return nil, nil, fmt.Errorf("review_id %q is not a positive integer", text)
		}
		id = parsed
	}
	// submitted_at is GitHub's RFC 3339 time; a listener that predates it carries none. One that
	// cannot be read is taken as none, and reported: a review without a time is ordered by its id,
	// so it is recorded rather than lost as poison.
	submittedAt, ok := rawTimestamp(raw, "submitted_at")
	var unread []string
	if !ok {
		unread = append(unread, fmt.Sprintf("submitted_at %s is not an RFC 3339 time", raw["submitted_at"]))
	}
	return PullRequestReview{Repo: repository.String(), Number: number, ID: id, SubmittedAt: submittedAt, State: strings.ToLower(state),
		CommitID: commitID, HeadSHA: headSHA, Author: author, Body: body, BodyTruncated: bodyTruncated == "true"}, unread, nil
}

// decodePush decodes a push to a branch: to an issue's branch (claim.BranchIssue) it is the Push
// the workflow applies to the pull request it records there, and to any other branch it is a
// BasePush, the only notice the daemon gets that a pull request's base moved. GitHub recomputes
// whether each head on a base still merges into it when the base moves and sends no event for the
// pull request itself, so a head the record holds as clean goes behind or dirty unheard.
//
// A base push costs the daemon one transaction under the fact lock (ApplyFact), which a heard
// repository that other automation pushes to many times a minute pays per push; the cost is paid
// because the alternative is a mergeability the record cannot know is stale. The handler's first
// read is one indexed lookup of the open pull requests recorded on that base
// (record.Store.OpenPullRequestsOnBase), and it ends the transaction at once when there are none,
// which is every push to a branch Legion has no pull request on.
//
// A push to anything but a branch - a tag - is no fact, and neither is one that names no branch or
// no commit it left.
//
// A push to an issue's branch whose after is record.NoCommit is no fact either. It is GitHub
// deleting the branch, which a repository that deletes a merged pull request's branch
// (delete_branch_on_merge) does right after every merge. The deletion names no head, so it is no
// Push. Workflow's push refuses a deletion from any other source too, and says why. A push to a
// base with that after is still a BasePush: the commit it records (record.PullRequest.BaseMoved) is
// only a mark that the merge state on record may be stale. It is compared for equality and cleared
// by the next read, and nothing reads it as a commit.
func decodePush(repository ghrepo.Repository, raw map[string]json.RawMessage) (Fact, error) {
	ref, _ := rawString(raw, "ref")
	branch, isHead := strings.CutPrefix(ref, "refs/heads/")
	if !isHead || branch == "" {
		return nil, nil
	}
	after, ok := rawString(raw, "after")
	if !ok || after == "" {
		return nil, nil
	}
	if _, isIssueBranch := claim.BranchIssue(branch); !isIssueBranch {
		return BasePush{Repo: repository.String(), Branch: branch, After: after}, nil
	}
	if after == record.NoCommit {
		return nil, nil
	}
	pusher, _ := rawString(raw, "pusher")
	before, _ := rawString(raw, "before")
	sender, _ := rawString(raw, "sender")
	return Push{Repo: repository.String(), Branch: branch, Before: before, After: after, Pusher: pusher, Sender: sender}, nil
}

// decodeChecks decodes a checks settlement of the pull request its subject names. A settlement
// that lacks any field the daemon orders or judges it by - its head, check runs, generation,
// snapshot, failed or cancelled - is no fact, and refused names that field and what was wrong
// with it, the first such field in that order, which the consumer logs (decodedMessage.Refused):
// a settlement dropped in silence leaves its pull request waiting with nothing to say why
// (dispatch://INFRA-4135 R6). refused is "" for a settlement that decodes. What it says beyond
// those costs the rest nothing: each part the decoder cannot read is taken as absent and named in
// the unread list the consumer logs, every such part rather than the first. A run's word that is
// no word (rawCheckRuns) is dropped from that run alone, which is then named by its conclusion. A
// skipped group that cannot be read names no run skipped. Its commit statuses are read last
// (rawStatuses): a list that cannot be read marks the fact so (PullRequestChecks.StatusesUnread),
// rather than the settlement's check runs being lost with it.
//
// Each run the skipped group names is skipped (record.RunSkipped), the one conclusion the decoder
// puts on a run: the failed and cancelled groups are read into the lists, from which classify
// concludes every other run (stampedRuns), and those lists come first there, so a name the
// listener put in both (which it never does) is still failed or cancelled. A name the group gives
// with no run in check_runs changes nothing, and a settlement without the group names no run
// skipped.
func decodeChecks(subject string, repository ghrepo.Repository, raw map[string]json.RawMessage) (fact Fact, unread []string, refused string) {
	number, ok := rawNumber(raw, "number")
	if !ok {
		return nil, nil, "number is absent or not a pull request number"
	}
	if subject != githubRepositoryPrefix(repository)+".pr."+strconv.Itoa(number)+".checks" {
		return nil, nil, fmt.Sprintf("number %d is not the pull request its subject names", number)
	}
	headSHA, ok := rawString(raw, "sha")
	if !ok {
		return nil, nil, "sha is absent or not a string"
	}
	if headSHA == "" {
		return nil, nil, "sha is empty"
	}
	runs, unread, defect := rawCheckRuns(raw["check_runs"])
	if defect != "" {
		return nil, nil, "check_runs " + defect
	}
	generation, ok := rawInt64(raw, "generation")
	if !ok {
		return nil, nil, "generation is absent or not a whole number"
	}
	if generation < 0 {
		return nil, nil, fmt.Sprintf("generation is %d, below 0", generation)
	}
	snapshot, ok := rawString(raw, "snapshot")
	if !ok {
		return nil, nil, "snapshot is absent or not a string"
	}
	if snapshot == "" {
		return nil, nil, "snapshot is empty"
	}
	failed, defect := rawStatusGroup(raw["failed"])
	if defect != "" {
		return nil, nil, "failed " + defect
	}
	cancelled, defect := rawStatusGroup(raw["cancelled"])
	if defect != "" {
		return nil, nil, "cancelled " + defect
	}
	if value, present := raw["skipped"]; present {
		skipped, unreadable := rawStatusGroup(value)
		if unreadable != "" {
			unread = append(unread, "skipped "+unreadable+": no check run of "+headSHA+" is read as skipped")
		}
		for _, name := range skipped {
			if i, found := slices.BinarySearchFunc(runs, name, func(run record.AttemptRun, name string) int {
				return strings.Compare(run.Name, name)
			}); found {
				runs[i].Conclusion = record.RunSkipped
			}
		}
	}
	checks := PullRequestChecks{Repo: repository.String(), Number: number, HeadSHA: headSHA, CheckRuns: runs, Generation: generation, Snapshot: snapshot, Failing: failed, Cancelled: cancelled}
	statuses, defect := rawStatuses(raw["statuses"])
	if defect != "" {
		checks.StatusesUnread = true
		return checks, append(unread, "statuses "+defect+": the pull request keeps the statuses it holds for "+headSHA), ""
	}
	checks.Statuses = statuses
	return checks, unread, ""
}

func githubIdentity(raw map[string]json.RawMessage) (int, string, bool) {
	number, numberOK := rawNumber(raw, "number")
	action, actionOK := rawString(raw, "action")
	return number, action, numberOK && actionOK
}

// rawCheckRuns reads a settlement's check runs, sorted by name: each entry's name and id, which
// every run must carry, unique by name, or the list is not read at all and defect says why, and
// the word GitHub gave the run (rawConclusionWord), which is optional and read apart from them, so
// a word that is not one costs its run nothing but the word. unread names each word dropped.
func rawCheckRuns(value json.RawMessage) (runs []record.AttemptRun, unread []string, defect string) {
	if value == nil {
		return nil, nil, "is absent"
	}
	var incoming []struct {
		Name       string          `json:"name"`
		ID         int64           `json:"id"`
		Conclusion json.RawMessage `json:"conclusion"`
	}
	if err := json.Unmarshal(value, &incoming); err != nil {
		return nil, nil, "does not parse as a list of check runs, each a name and an id"
	}
	if len(incoming) == 0 {
		return nil, nil, "is an empty list"
	}
	seen := make(map[string]struct{}, len(incoming))
	runs = make([]record.AttemptRun, 0, len(incoming))
	for _, item := range incoming {
		if item.Name == "" {
			return nil, nil, "names a run with no name"
		}
		if item.ID <= 0 {
			return nil, nil, fmt.Sprintf("names %q with the id %d, not above 0", item.Name, item.ID)
		}
		if _, duplicate := seen[item.Name]; duplicate {
			return nil, nil, fmt.Sprintf("names %q twice", item.Name)
		}
		seen[item.Name] = struct{}{}
		word, unreadable := rawConclusionWord(item.Conclusion)
		if unreadable != "" {
			unread = append(unread, "check_runs["+item.Name+"].conclusion "+unreadable+": the run is named by its conclusion")
		}
		runs = append(runs, record.AttemptRun{Name: item.Name, ID: item.ID, GitHubConclusion: word})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Name < runs[j].Name })
	return runs, unread, ""
}

// conclusionWord is the form of a word the daemon keeps as the one GitHub gave a check run:
// lower-case letters and underscores, one to 32 of them, which every conclusion GitHub names has
// (success, timed_out, action_required, startup_failure, ...) and which leaves room for a word it
// adds later. The word is read into the texts the roles read (workflow's settledChecks), so
// nothing else a settlement could carry there, such as markup, a line break or an instruction,
// reaches a role: a run whose word is not of this form is named by its conclusion, as a run with
// no word is.
var conclusionWord = regexp.MustCompile(`^[a-z_]{1,32}$`)

// rawConclusionWord reads one check run's optional conclusion word. An absent or null word is no
// word and no defect. Anything but a string of conclusionWord's form is dropped, and defect says
// what it was: it quotes a string no longer than a word, and only counts the bytes of a longer
// one, since the consumer logs it.
func rawConclusionWord(value json.RawMessage) (string, string) {
	if value == nil || string(value) == "null" {
		return "", ""
	}
	var word string
	if err := json.Unmarshal(value, &word); err != nil {
		return "", "is not a string"
	}
	if len(word) > 32 {
		return "", fmt.Sprintf("is a string of %d bytes, longer than a word's 32", len(word))
	}
	if !conclusionWord.MatchString(word) {
		return "", fmt.Sprintf("is %q, not a word of lower-case letters and underscores", word)
	}
	return word, ""
}

// rawStatusGroup reads one of a settlement's groups of check names, {count, checks}: the names,
// none of them empty, as many as the count says. defect says why the group could not be read, ""
// when it was. A null group is an empty one, and an absent one is not read.
func rawStatusGroup(value json.RawMessage) ([]string, string) {
	if value == nil {
		return nil, "is absent"
	}
	var group struct {
		Count  int      `json:"count"`
		Checks []string `json:"checks"`
	}
	if err := json.Unmarshal(value, &group); err != nil {
		return nil, "is not a group of check names"
	}
	if group.Count < 0 || len(group.Checks) != group.Count {
		return nil, fmt.Sprintf("counts %d checks and names %d", group.Count, len(group.Checks))
	}
	if slices.Contains(group.Checks, "") {
		return nil, "names a check with no name"
	}
	return group.Checks, ""
}

func rawString(raw map[string]json.RawMessage, key string) (string, bool) {
	value, present := raw[key]
	if !present {
		return "", false
	}
	var decoded string
	if err := json.Unmarshal(value, &decoded); err != nil {
		return "", false
	}
	return decoded, true
}

func rawNumber(raw map[string]json.RawMessage, key string) (int, bool) {
	value, present := raw[key]
	if !present {
		return 0, false
	}
	var integer int64
	if rawInt, ok := rawInt64Value(value); ok {
		integer = rawInt
	} else {
		var text string
		if err := json.Unmarshal(value, &text); err != nil || !decimalDigits.MatchString(text) {
			return 0, false
		}
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return 0, false
		}
		integer = parsed
	}
	if integer < 0 || integer > int64(math.MaxInt) {
		return 0, false
	}
	return int(integer), true
}

func rawInt64(raw map[string]json.RawMessage, key string) (int64, bool) {
	value, present := raw[key]
	if !present {
		return 0, false
	}
	return rawInt64Value(value)
}

func rawInt64Value(value json.RawMessage) (int64, bool) {
	var integer int64
	if err := json.Unmarshal(value, &integer); err != nil {
		return 0, false
	}
	return integer, true
}

// decimalDigits is a number as Envoy writes GitHub's numeric fields into a payload: decimal digits
// alone, with none of the sign or base prefix strconv would otherwise accept. rawNumber holds a
// number written as a string to it.
var decimalDigits = regexp.MustCompile(`^[0-9]+$`)

// rawTimestamp reads an optional RFC 3339 time: an absent, null or empty field is the zero time,
// and ok is false only when the field holds anything else, which is taken as the zero time too.
func rawTimestamp(raw map[string]json.RawMessage, key string) (time.Time, bool) {
	value, present := raw[key]
	if !present || string(value) == "null" {
		return time.Time{}, true
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return time.Time{}, false
	}
	if text == "" {
		return time.Time{}, true
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}
