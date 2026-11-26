package replay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// shadow is a replay's copy of Dispatch (design 10.4): each issue's current payload as Dispatch
// would serve it, and a sequence of its own. It starts where the record stands at the window's
// start: every recorded event before the start is applied at its recorded time and its recorded
// sequence (seedEvent), so each issue's payload, asks and documents are what the record shows then,
// and its sequence is the one the start state's record of it stands at. Every event a replay
// applies from the start on — a recorded event at its replayed time, or the echo of a status write
// the replay's own outbox made — takes its issue's next sequence past that and carries the shadow's
// whole payload, so no sequence fence of the daemon drops it for arriving out of the record's
// order, and an echo that changes the status leaves the issue's links, parent, labels and rank as
// they stand. A recorded issue event changes the payload by what it changed in the record, read as
// the fields its payload differs in from the issue's recorded event before it in the record's
// order, whatever the copy applied before it, so a person's edit lands on the replay's status
// rather than writing back the status the record's daemon had written, and an event the replay
// applies late, an act of a piece of work it moved, changes only what it changed in the record. It
// takes back no field that an event after it in the record's order set and the copy applied first
// (shadowIssue.setBy). The daemon's reads of Dispatch — a boot's listing, a tick's re-read, a status
// write's read, a gate seed's read of a document — are answered from it.
type shadow struct {
	project string
	issues  map[string]*shadowIssue
	// ranks is the rank each issue is shown at in place of Dispatch's, under the branch's
	// FirstStarts (firstStartRanks), as the replay's first slots have moved them (rankWrite);
	// empty where the branch keeps Dispatch's ranks.
	ranks map[string]string
	// lastID is the id of the last event the replay made itself (an echo), echoIDBase before the
	// first: each next one takes the id after it, past every recorded one, so each fact's id
	// (dispatch-<id>) is the replay's own.
	lastID int64
	// approvals is the approvals signal's state as the events the copy applied leave it, each ask of
	// an issue of the project and each such issue's assignee (noteApprovals), fed where readsApprovals
	// says a run reads the signal; approvalsMoved counts the changes, so a reader of the count knows
	// when it stands as it did.
	approvals      signals.ApprovalState
	readsApprovals bool
	approvalsMoved int
	// priorities, where set, is the priority Dispatch resolves for each issue, which the copy shows
	// as each issue's priority in place of its own under a rule with the new P0 start
	// (record.SlotRule.StartsNow), so the daemon's mirror holds it (record.Issue.Priority): each
	// issue of the project at the one standing at each write of its payload (showPriority), at the
	// window's start (showPrioritiesBefore), and at each change of it that no event of the issue
	// makes (priorityWrite). It is the one place a replay supplies the daemon the resolved
	// priority: once the daemon's record keeps the resolved_priority Dispatch serves of its own, the
	// copy serves it there and the daemon's own reading of it takes this one's place. Nil under every
	// other rule, whose copy shows each issue's own and no priority Dispatch resolved (showPriority).
	priorities *resolvedPriorities
}

// shadowIssue is one issue of the shadow.
type shadowIssue struct {
	key, project string
	// seq is the sequence of the issue's last event in the replay: the recorded sequence of its
	// newest event before the window's start (seedEvent), then one more for each event the replay
	// applies.
	seq int64
	// payload is the issue as Dispatch shows it now, nil before its first issue event.
	payload map[string]json.RawMessage
	// shownStatus, shownParent, shownLabels and shownBlockers are the payload's status, parent,
	// labels and blocked_by as str and strs read them, decoded at each write of those fields
	// (decodeShown: take and statusWrite) rather than at each read.
	shownStatus, shownParent   string
	shownLabels, shownBlockers []string
	// setBy is, for each field of the payload a recorded issue event set, the place in the record's
	// order of the last event that set it (the script's index of the event): an event before it in
	// that order, applied later, sets the field no more.
	setBy map[string]int
	// log is every event of the issue the shadow holds, as applied: first the recorded events
	// before the window's start (seedEvent), the first seeded of them, then every event the replay
	// applied. A read of the issue's event log is answered from it (intake.LabelSetter), as
	// Dispatch's log holds the issue's events from before the start too.
	log    []json.RawMessage
	seeded int
	// asks is each ask of the issue as its events leave it, and artifacts each document.
	asks      map[string]*shadowAsk
	artifacts map[string]*shadowArtifact
}

// shadowAsk is one ask as its events leave it: whether it is open, whose turn it stands on, and
// the document an approval request asks a person to approve.
type shadowAsk struct {
	id, kind, artifact, waitingOn string
	open                          bool
}

// shadowArtifact is one document as its events leave it: its latest version, and each verdict on
// a version with the place of its event in the issue's log and its actor.
type shadowArtifact struct {
	latest   int
	verdicts []shadowVerdict
}

// shadowVerdict is one person's verdict on a version of a document.
type shadowVerdict struct {
	version  int
	approved bool
	reason   string
	mark     record.EventMark
	actor    dispatch.Actor
}

// echoIDBase is where the ids of the events a replay makes begin: above every id Dispatch gives
// a project's events in a window.
const echoIDBase = int64(1) << 40

// bookkeeping is the fields of an issue event's payload that every event changes and none of
// them changes by itself: they are written by the shadow for each event it applies.
var bookkeeping = map[string]bool{"last_seq": true, "updated_at": true}

// newShadow is the empty copy of project's Dispatch.
func newShadow(project string, ranks map[string]string) *shadow {
	return &shadow{project: project, issues: map[string]*shadowIssue{}, ranks: ranks, lastID: echoIDBase}
}

// issue is key's shadow, made where none is yet.
func (s *shadow) issue(key, project string) *shadowIssue {
	iss, ok := s.issues[key]
	if !ok {
		if project == "" {
			project, _, _ = strings.Cut(key, "-")
		}
		iss = &shadowIssue{key: key, project: project, setBy: map[string]int{}, asks: map[string]*shadowAsk{},
			artifacts: map[string]*shadowArtifact{}}
		s.issues[key] = iss
	}
	return iss
}

// envelope is a Dispatch event read as the replay rewrites it: each top-level field, and the
// fields the shadow reads of it.
type envelope struct {
	fields  map[string]json.RawMessage
	key     string
	project string
	kind    string
	actor   dispatch.Actor
	payload map[string]json.RawMessage
}

// readEnvelope reads one recorded event, raw, whose envelope parsed already holds
// (ParseDispatchEvent): its top-level fields and its payload, each by name; a payload of null
// reads as none.
func readEnvelope(raw json.RawMessage, parsed DispatchEvent) (envelope, error) {
	e := envelope{key: parsed.Key, project: parsed.Project, kind: parsed.Type, actor: parsed.Actor}
	if err := json.Unmarshal(raw, &e.fields); err != nil {
		return envelope{}, fmt.Errorf("replay: read a recorded Dispatch event: %w", err)
	}
	if payload, ok := e.fields["payload"]; ok {
		if err := json.Unmarshal(payload, &e.payload); err != nil {
			return envelope{}, fmt.Errorf("replay: read the payload of a recorded %s event of %s: %w", e.kind, e.key, err)
		}
	}
	return e, nil
}

// decodePayload decodes e's payload into into from the bytes the event carries, the bytes e.payload
// was read from; an event with no payload leaves into as it is.
func decodePayload(e envelope, into any) error {
	if e.payload == nil {
		return nil
	}
	return json.Unmarshal(e.fields["payload"], into)
}

// issueEvent says whether kind is an event of an issue's own fields, which carries the whole
// issue as its payload.
func issueEvent(kind string) bool {
	return kind == "issue.created" || kind == "issue.updated" || kind == "issue.closed"
}

// daemonWrote says whether actor is the project's daemon, whose own writes a replay makes again
// from its own outbox rather than taking them from the record.
func daemonWrote(actor dispatch.Actor, key string) bool {
	return actor.Kind == "session" && actor.ID == dispatch.DaemonSession(key)
}

// apply applies a recorded event at at: it takes its issue's next sequence, and an issue event
// changes the payload by the fields it changed in the record (take). place is the event's place in
// the record's order, and before the payload of its issue's recorded issue event before it in that
// order, nil for none. It answers the event as the replay applies it, which the daemon's decoder
// reads.
func (s *shadow) apply(e envelope, before map[string]json.RawMessage, place int, at time.Time) (json.RawMessage, error) {
	iss := s.issue(e.key, e.project)
	iss.seq++
	return s.take(iss, e, before, place, at)
}

// seedEvent applies a recorded event from before the window's start at its recorded time at and
// its recorded sequence seq, not its issue's next, which brings the shadow to where the record
// stood at the start: each issue at the sequence of its newest event before it, where the start
// state writes its record's LastDispatchSeq, with the payload, asks and documents the record shows
// then. A write of the daemon's own is applied as any other, since the record's state at the start
// holds it. place and before are apply's. The event joins the issue's log ahead of every event the
// replay applies (shadowIssue.seeded): a replay seeds every event before its start before it
// applies its first.
func (s *shadow) seedEvent(e envelope, before map[string]json.RawMessage, place int, seq int64, at time.Time) error {
	iss := s.issue(e.key, e.project)
	iss.seq = seq
	if _, err := s.take(iss, e, before, place, at); err != nil {
		return err
	}
	iss.seeded = len(iss.log)
	return nil
}

// take applies e, the recorded event at place in the record's order, to iss at at, at the sequence
// iss stands at: an issue event sets each field its payload differs in from before, the payload of
// its issue's recorded issue event before it in that order, and its whole payload where iss holds
// none yet, but a field that an event after it in the record's order set (shadowIssue.setBy); a
// claim event (issue.claimed, issue.released) changes the claim the payload shows where it shows
// one, as Dispatch's next read of the issue would show it (DispatchEvent.Claim), and every event's
// asks and documents are read (notePayload). It answers the event as applied, which joins the
// issue's log.
func (s *shadow) take(iss *shadowIssue, e envelope, before map[string]json.RawMessage, place int, at time.Time) (json.RawMessage, error) {
	fields := maps.Clone(e.fields)
	if issueEvent(e.kind) && e.payload != nil {
		first := iss.payload == nil
		if first {
			iss.payload = map[string]json.RawMessage{}
		}
		for name, value := range e.payload {
			if !first && bookkeeping[name] {
				continue
			}
			if prior, had := before[name]; !first && had && sameJSON(prior, value) {
				continue
			}
			if by, set := iss.setBy[name]; set && by > place {
				continue
			}
			iss.payload[name], iss.setBy[name] = value, place
		}
		s.showPriority(iss, at)
		iss.decodeShown()
		iss.stamp(at)
		payload, err := iss.render(s.ranks)
		if err != nil {
			return nil, err
		}
		fields["payload"] = payload
	}
	if (e.kind == "issue.claimed" || e.kind == "issue.released") && iss.payload != nil {
		raw := e.payload["claim"]
		if holder, shown := eventClaim(raw); shown && (holder != "" || e.kind == "issue.released") {
			iss.payload["claim"] = raw
		}
	}
	if err := s.notePayload(iss, e, at); err != nil {
		return nil, err
	}
	if err := s.noteApprovals(iss, e); err != nil {
		return nil, err
	}
	return iss.event(fields, at)
}

// noteApprovals applies what e, as the copy applied it to iss, says to the approvals signal
// (approvalEventOf) where the run reads the signal (readsApprovals): an ask's event and a comment on
// an ask of an issue of the project, and the assignee an issue event of one names, as the copy
// holds it once the event applied, at the moment the copy applies it.
func (s *shadow) noteApprovals(iss *shadowIssue, e envelope) error {
	if !s.readsApprovals || !claim.KeyOfProject(s.project, iss.key) {
		return nil
	}
	read, err := approvalEventOf(e.kind, iss.key, e.fields["payload"])
	if err != nil {
		return fmt.Errorf("replay: read the %s event of %s for the approvals: %w", e.kind, iss.key, err)
	}
	switch {
	case read.assigns:
		assignee := read.assignee
		if member, held := iss.payload["assignee"]; held {
			var person *string
			if err := json.Unmarshal(member, &person); err != nil {
				return fmt.Errorf("replay: read the assignee of %s for the approvals: %w", iss.key, err)
			}
			assignee = personOf(person)
		}
		s.approvals.Assign(iss.key, assignee)
		s.approvalsMoved++
	case read.asks && s.approvals.ApplyAsk(read.ask):
		s.approvalsMoved++
	}
	return nil
}

// stamp writes the shadow's bookkeeping fields for an event at at.
func (iss *shadowIssue) stamp(at time.Time) {
	iss.payload["last_seq"] = json.RawMessage(fmt.Sprint(iss.seq))
	if _, has := iss.payload["updated_at"]; has {
		iss.payload["updated_at"] = mustJSON(at)
	}
}

// render is the payload as the replay shows it: with the rank the branch shows the issue at.
func (iss *shadowIssue) render(ranks map[string]string) (json.RawMessage, error) {
	shown := iss.payload
	if rank, ok := ranks[iss.key]; ok {
		shown = maps.Clone(iss.payload)
		shown["rank"] = mustJSON(rank)
	}
	return json.Marshal(shown)
}

// event is fields, an event of iss, at its sequence and at at, and kept in its log.
func (iss *shadowIssue) event(fields map[string]json.RawMessage, at time.Time) (json.RawMessage, error) {
	fields["seq"] = json.RawMessage(fmt.Sprint(iss.seq))
	fields["created_at"] = mustJSON(at)
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("replay: write the event of %s at seq %d: %w", iss.key, iss.seq, err)
	}
	iss.log = append(iss.log, raw)
	return raw, nil
}

// versionPayload is what the replay reads of an artifact.version event's payload: the document it
// writes a version of and the version's number.
type versionPayload struct {
	ArtifactID string `json:"artifact_id"`
	Version    struct {
		Number int `json:"number"`
	} `json:"version"`
}

// notePayload reads what a non-issue event says of the issue's asks and documents, at the
// sequence the shadow gave it.
func (s *shadow) notePayload(iss *shadowIssue, e envelope, at time.Time) error {
	switch e.kind {
	case "artifact.version":
		var payload versionPayload
		if err := decodePayload(e, &payload); err != nil {
			return err
		}
		document := iss.document(payload.ArtifactID)
		document.latest = max(document.latest, payload.Version.Number)
		// A newer version leaves the approval request of the document with its agent.
		for _, ask := range iss.asks {
			if ask.artifact == payload.ArtifactID && ask.open {
				ask.waitingOn = "agent"
			}
		}
	case "artifact.approved", "artifact.changes_requested":
		var payload struct {
			ArtifactID string  `json:"artifact_id"`
			Version    int     `json:"version"`
			Reason     *string `json:"reason"`
		}
		if err := decodePayload(e, &payload); err != nil {
			return err
		}
		document := iss.document(payload.ArtifactID)
		document.latest = max(document.latest, payload.Version)
		verdict := shadowVerdict{version: payload.Version, approved: e.kind == "artifact.approved", mark: record.EventMark{Seq: iss.seq, At: at},
			actor: e.actor}
		if payload.Reason != nil {
			verdict.reason = *payload.Reason
		}
		document.verdicts = append(document.verdicts, verdict)
	case "ask.opened", "ask.answered", "ask.resolved", "ask.handed_back", "ask.edited":
		var payload struct {
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			State    string `json:"state"`
			Approval *struct {
				ArtifactID       string `json:"artifact_id"`
				Version          int    `json:"version"`
				RequestedVersion int    `json:"requested_version"`
			} `json:"approval"`
		}
		if err := decodePayload(e, &payload); err != nil {
			return err
		}
		ask, known := iss.asks[payload.ID]
		if !known {
			ask = &shadowAsk{id: payload.ID}
			iss.asks[payload.ID] = ask
		}
		if payload.Kind != "" {
			ask.kind = payload.Kind
		}
		if payload.Approval != nil && payload.Approval.ArtifactID != "" {
			ask.artifact = payload.Approval.ArtifactID
		}
		switch e.kind {
		case "ask.opened", "ask.handed_back":
			ask.open, ask.waitingOn = true, "human"
		case "ask.answered", "ask.resolved":
			ask.open = false
		case "ask.edited":
			if payload.Approval != nil && payload.Approval.RequestedVersion < payload.Approval.Version {
				ask.waitingOn = "agent"
			}
		}
	case "comment.created", "comment.answered":
		var payload struct {
			AskID        string `json:"ask_id"`
			AskWaitingOn string `json:"ask_waiting_on"`
		}
		if err := decodePayload(e, &payload); err != nil {
			return err
		}
		if ask, known := iss.asks[payload.AskID]; known && payload.AskWaitingOn != "" {
			ask.waitingOn = payload.AskWaitingOn
		}
	}
	return nil
}

// document is iss's document artifact, made where none is yet.
func (iss *shadowIssue) document(artifact string) *shadowArtifact {
	document, ok := iss.artifacts[artifact]
	if !ok {
		document = &shadowArtifact{}
		iss.artifacts[artifact] = document
	}
	return document
}

// statusWrite is the event the daemon's write of key's status makes in Dispatch, at at: the
// shadow's payload at status, with the legion label added for a hand-over (handOver), by the
// daemon's own session, at the issue's next sequence. It answers false where the shadow holds no
// payload of the issue to write over, which Dispatch would refuse.
func (s *shadow) statusWrite(key, status string, handOver bool, at time.Time) (json.RawMessage, bool, error) {
	iss, ok := s.issues[key]
	if !ok || iss.payload == nil {
		return nil, false, nil
	}
	iss.seq++
	iss.payload["status"] = mustJSON(status)
	if _, has := iss.payload["closed_at"]; has || status == record.DoneStatus {
		if status == record.DoneStatus {
			iss.payload["closed_at"] = mustJSON(at)
		} else {
			iss.payload["closed_at"] = json.RawMessage("null")
		}
	}
	if handOver {
		labels := iss.labels()
		if !dispatch.CarriesLegionLabel(labels) {
			iss.payload["labels"] = mustJSON(append(labels, dispatch.LegionLabel))
		}
	}
	iss.decodeShown()
	s.showPriority(iss, at)
	iss.stamp(at)
	payload, err := iss.render(s.ranks)
	if err != nil {
		return nil, false, err
	}
	kind := "issue.updated"
	if status == record.DoneStatus {
		kind = "issue.closed"
	}
	raw, err := s.made(iss, kind, jsonActor{Kind: "session", ID: dispatch.DaemonSession(key)}, payload, at)
	return raw, err == nil, err
}

// rankActor is the actor of the replay's own moves of a rank (rankWrite): the replay itself, as
// Dispatch names itself (system), since no person, session or daemon of the record made them.
var rankActor = jsonActor{Kind: "system", ID: "replay-first-starts"}

// rankWrite moves the rank the replay shows key at to rank, or to Dispatch's own where shown is
// false (FirstStarts recorded, firstStarts.rootRank), and answers the event the move makes in
// Dispatch at at: an issue.updated of the shadow's payload at the new rank, whose changed fields
// are the rank, by rankActor, at the issue's next sequence. It answers false where the rank shown
// does not move, and where the shadow holds no payload of the issue to write over, whose first
// event then carries the rank. No recorded event is this one: the payload and the record's place of
// the event that last set each of its fields (shadowIssue.setBy) stay as they were.
func (s *shadow) rankWrite(key, rank string, shown bool, at time.Time) (json.RawMessage, bool, error) {
	if before, had := s.ranks[key]; had == shown && before == rank {
		return nil, false, nil
	}
	if shown {
		s.ranks[key] = rank
	} else {
		delete(s.ranks, key)
	}
	iss, ok := s.issues[key]
	if !ok || iss.payload == nil {
		return nil, false, nil
	}
	iss.seq++
	s.showPriority(iss, at)
	iss.stamp(at)
	moved := maps.Clone(iss.payload)
	if shown {
		moved["rank"] = mustJSON(rank)
	}
	moved["changed"] = mustJSON([]string{"rank"})
	payload, err := json.Marshal(moved)
	if err != nil {
		return nil, false, err
	}
	raw, err := s.made(iss, "issue.updated", rankActor, payload, at)
	return raw, err == nil, err
}

// made is an event the replay makes itself of iss at at, past every recorded one: of kind, by
// actor, carrying payload, at the sequence iss stands at, and kept in its log.
func (s *shadow) made(iss *shadowIssue, kind string, actor jsonActor, payload json.RawMessage, at time.Time) (json.RawMessage, error) {
	s.lastID++
	return iss.event(map[string]json.RawMessage{
		"id": json.RawMessage(fmt.Sprint(s.lastID)), "issue_key": mustJSON(iss.key), "artifact_id": json.RawMessage("null"),
		"project": mustJSON(iss.project), "type": mustJSON(kind), "actor": mustJSON(actor), "notify": json.RawMessage("true"),
		"payload": payload,
	}, at)
}

// showPriority shows iss at the priority Dispatch resolves for it at at, in place of its own,
// where the copy shows the resolved priorities (shadow.priorities) and iss is an issue of the
// project whose payload the copy holds. Where the copy does not, under a rule with no P0 start, it
// shows iss at its own priority alone, as a Dispatch release before resolved_priority serves it:
// with no resolved_priority and no priority_from, which the daemon's intake would read in place of
// the issue's own (record.Issue.AdmissionPriority), so such a rule keeps its admissions and its
// line on each issue's own priority where the record carries the one Dispatch resolved.
func (s *shadow) showPriority(iss *shadowIssue, at time.Time) {
	if s.priorities == nil {
		delete(iss.payload, "resolved_priority")
		delete(iss.payload, "priority_from")
		return
	}
	if iss.payload != nil && iss.project == s.project {
		iss.payload["priority"] = mustJSON(s.priorities.at(iss.key, at).Level())
	}
}

// showPrioritiesBefore shows each issue of the project the copy holds a payload of at the priority
// Dispatch resolves for it just before at, the window's start, where the copy shows the resolved
// priorities: the copy then stands where Dispatch stood, a change of an ancestor after the issue's
// newest event before the start included, as the start state records it (Trace.shownPriorities).
func (s *shadow) showPrioritiesBefore(at time.Time) {
	if s.priorities == nil {
		return
	}
	for _, iss := range s.issues {
		if iss.payload != nil && iss.project == s.project {
			iss.payload["priority"] = mustJSON(s.priorities.before(iss.key, at).Level())
		}
	}
}

// priorityActor is the actor of the replay's own updates of an issue whose resolved priority a
// change of another issue moves (priorityWrite): the replay itself, as Dispatch names itself
// (system), the writer of that change making the change's own event, which the copy applies.
var priorityActor = jsonActor{Kind: "system", ID: "replay-resolved-priority"}

// priorityWrite shows key at the priority Dispatch resolves for it at at, where the copy shows the
// resolved priorities, and answers the event the change makes in Dispatch at at: an issue.updated
// of the shadow's payload at the new priority, whose changed field is the priority, by
// priorityActor, at the issue's next sequence, as Dispatch appends one on each issue whose resolved
// priority a write to another issue changes. It answers false where the copy shows no resolved
// priority, holds no payload of the issue, or shows it at that priority already: an event of the
// issue applied at the change, its own or Dispatch's recorded update of it, showed it. No recorded
// event is this one: the record's place of the event that last set each field
// (shadowIssue.setBy) stays as it was.
func (s *shadow) priorityWrite(key string, at time.Time) (json.RawMessage, bool, error) {
	iss, ok := s.issues[key]
	if s.priorities == nil || !ok || iss.payload == nil {
		return nil, false, nil
	}
	shown := mustJSON(s.priorities.at(key, at).Level())
	if sameJSON(iss.payload["priority"], shown) {
		return nil, false, nil
	}
	iss.seq++
	iss.payload["priority"] = shown
	iss.stamp(at)
	written := maps.Clone(iss.payload)
	if rank, ranked := s.ranks[key]; ranked {
		written["rank"] = mustJSON(rank)
	}
	written["changed"] = mustJSON([]string{"priority"})
	payload, err := json.Marshal(written)
	if err != nil {
		return nil, false, err
	}
	raw, err := s.made(iss, "issue.updated", priorityActor, payload, at)
	return raw, err == nil, err
}

// jsonActor is a Dispatch event's actor as Dispatch writes it.
type jsonActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// status is the issue's status as the shadow holds it, "" for an issue it holds no payload of.
func (iss *shadowIssue) status() string { return iss.shownStatus }

// str is the string field name of the payload, "" where it is absent or null.
func (iss *shadowIssue) str(name string) string {
	var value *string
	if raw, ok := iss.payload[name]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	if value == nil {
		return ""
	}
	return *value
}

// strs is the list field name of the payload, nil where it is absent or null.
func (iss *shadowIssue) strs(name string) []string {
	var value []string
	if raw, ok := iss.payload[name]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return value
}

// decodeShown reads the payload's status, parent, labels and blocked_by into the fields their
// readers answer from, after a write of the payload.
func (iss *shadowIssue) decodeShown() {
	iss.shownStatus, iss.shownParent = iss.str("status"), iss.str("parent")
	iss.shownLabels, iss.shownBlockers = iss.strs("labels"), iss.strs("blocked_by")
}

// labels, parent and blockers are the payload's, each list the caller's own copy.
func (iss *shadowIssue) labels() []string   { return slices.Clone(iss.shownLabels) }
func (iss *shadowIssue) parent() string     { return iss.shownParent }
func (iss *shadowIssue) blockers() []string { return slices.Clone(iss.shownBlockers) }

// rank is the rank the replay shows the issue at.
func (s *shadow) rank(iss *shadowIssue) string {
	if rank, ok := s.ranks[iss.key]; ok {
		return rank
	}
	return iss.str("rank")
}

// claimedBy is who the payload shows holding the issue's Dispatch claim (`<kind>:<id>`, "" for
// nobody), and whether it shows the claim at all, read as the daemon reads a claim Dispatch shows
// (eventClaim): a claim that names no actor, or names it with no kind or no id, shows none.
func (iss *shadowIssue) claimedBy() (string, bool) {
	return eventClaim(iss.payload["claim"])
}

// summary is the issue as a boot's listing shows it (dispatch.IssueSummary), at the rank the
// replay shows it at and with the priority its payload shows (level), which the daemon's listing
// writes to the record as each event does (intake's ListedIssue).
func (s *shadow) summary(iss *shadowIssue) dispatch.IssueSummary {
	var parent *string
	if p := iss.parent(); p != "" {
		parent = &p
	}
	claimed, known := iss.claimedBy()
	labels := iss.labels()
	return dispatch.IssueSummary{Key: iss.key, Title: iss.str("title"), Status: iss.status(), Parent: parent, Rank: s.rank(iss),
		Priority: iss.level(), HandedOver: dispatch.CarriesLegionLabel(labels), Labels: labels, LastSeq: iss.seq,
		ResolvedRepo: iss.str("resolved_repo"), BlockedBy: iss.blockers(), ClaimedBy: claimed, ClaimUnread: !known}
}

// level is the priority the payload shows as Dispatch serves it, 0 for P0 to 3 for P3, and nil for
// none or for a value that is no integer (dispatch.IssueSummary.Priority).
func (iss *shadowIssue) level() *int {
	var value *int
	if raw, ok := iss.payload["priority"]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return value
}

// listing is every issue of the project the shadow holds a payload of, as a boot's listing shows
// them, by key.
func (s *shadow) listing() []dispatch.IssueSummary {
	var listed []dispatch.IssueSummary
	for _, key := range slices.Sorted(maps.Keys(s.issues)) {
		if iss := s.issues[key]; iss.payload != nil && iss.project == s.project {
			listed = append(listed, s.summary(iss))
		}
	}
	return listed
}

// chain is key's Dispatch parent chain as the shadow shows it, from parent up to the top, stopping
// at a repeated key or back at key: the chain the daemon's preparation reads (Ancestry).
func (s *shadow) chain(key, parent string) []string {
	var chain []string
	for ancestor := parent; ancestor != "" && ancestor != key && !slices.Contains(chain, ancestor); {
		chain = append(chain, ancestor)
		next := ""
		if iss, ok := s.issues[ancestor]; ok {
			next = iss.parent()
		}
		ancestor = next
	}
	return chain
}

// approval is what Dispatch's read of a document's approval answers from the shadow
// (dispatch.Approval): its latest version, the verdict on that version, and the approval request
// open on it and whose turn it stands on.
type approval struct {
	latest         int
	approved       *shadowVerdict
	changes        *shadowVerdict
	ask, waitingOn string
	latestReviewer dispatch.Actor
}

// approvalOf is the approval of artifact on key as the shadow holds it.
func (s *shadow) approvalOf(key, artifact string) approval {
	var read approval
	iss, ok := s.issues[key]
	if !ok {
		return read
	}
	if document, ok := iss.artifacts[artifact]; ok {
		read.latest = document.latest
		for i := range document.verdicts {
			verdict := &document.verdicts[i]
			if verdict.version != document.latest {
				continue
			}
			read.approved, read.changes = nil, nil
			if verdict.approved {
				read.approved = verdict
			} else {
				read.changes = verdict
			}
			read.latestReviewer = verdict.actor
		}
	}
	for _, id := range slices.Sorted(maps.Keys(iss.asks)) {
		if ask := iss.asks[id]; ask.open && ask.artifact == artifact {
			read.ask, read.waitingOn = ask.id, ask.waitingOn
		}
	}
	return read
}

// approvedBefore is the highest version below version of the document artifact that a person's
// verdict the shadow holds approved, 0 where none did: Dispatch's answer to the daemon's read of
// the document's review history (dispatch.Client.ApprovedVersionBefore), which takes the highest
// such version wherever it stands, since a changes request approves none and a review of a version
// at or above the one asked about is not before it. The shadow holds the verdicts of every event
// of the window and of its reach before the start (seedEvent), on whichever issue the document is
// recorded, so a document none of those events names answers 0, as a history with no approval in
// it does.
func (s *shadow) approvedBefore(artifact string, version int) int {
	approved := 0
	for _, iss := range s.issues {
		document, ok := iss.artifacts[artifact]
		if !ok {
			continue
		}
		for _, verdict := range document.verdicts {
			if verdict.approved && verdict.version < version && verdict.version > approved {
				approved = verdict.version
			}
		}
	}
	return approved
}

// sameJSON says whether a and b are one JSON value written alike, spaces aside.
func sameJSON(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var x, y bytes.Buffer
	if json.Compact(&x, a) != nil || json.Compact(&y, b) != nil {
		return false
	}
	return bytes.Equal(x.Bytes(), y.Bytes())
}

// mustJSON is v as JSON, for a value that always marshals.
func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
