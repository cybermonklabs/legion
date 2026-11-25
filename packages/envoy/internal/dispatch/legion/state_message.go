package legion

// state_message.go holds the state message of a daemon's state lane (dispatch://INFRA-3931): the
// message and the events in it (stateMessage, stateEvent, stateChange), its decoding and the
// checks that reject a message (decodeStateMessage, decodeEvents, changeObservation), what a
// message says of the position it is judged on (standsAt, stateMessage.unfollowed, boots,
// movesSchema), the events above the cursor and the values a transaction leaves (eventsAfter,
// lastValues, recordEvents), and the parts of a split transaction that are held until its last
// message arrives (part, heldParts, holdPart).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
)

// part is one held message of a transaction the daemon split over several (legion_event_parts,
// migration 20261118212431): every message of the transaction but its last carries `more`, and
// none of its events is applied until the last arrives (applyBatch), so no observation holds a
// value of a transaction Dispatch has not read whole. A part is held only where it follows the
// cursor or the part before it (stateMessage.continues), so the parts of a project are one run of
// sequences under one run and one store. Every write of the project's setting that resets the
// cursor deletes them (resetCursor), and recordWhole says which whole reads delete them.
type part struct {
	firstSeq, lastSeq int64
	run, stateID      string
	bytes             int
	events            []stateEvent
}

// heldParts reads project's held parts inside tx, which holds the project's cursor `for update`
// (lockPosition), in sequence order, each part's events decoded and observed as its message's
// were.
func heldParts(ctx context.Context, tx pgx.Tx, project string) ([]part, error) {
	rows, err := tx.Query(ctx, `
		select first_seq, last_seq, run, state_id, bytes, events from legion_event_parts
		where project = $1 order by first_seq
	`, project)
	if err != nil {
		return nil, fmt.Errorf("read the held state events of %s: %w", project, err)
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (part, error) {
		var one part
		var events string
		if err := row.Scan(&one.firstSeq, &one.lastSeq, &one.run, &one.stateID, &one.bytes, &events); err != nil {
			return part{}, err
		}
		one.events, err = decodeEvents([]byte(events))
		return one, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the held state events of %s: %w", project, err)
	}
	return held, nil
}

// holdPart holds message, a part of a split transaction, inside tx: its sequences, run, store,
// commit time, bytes and its events as the daemon sent them. A part held already, delivered
// again, is held once.
func holdPart(ctx context.Context, tx pgx.Tx, project string, message stateMessage, bytes int) error {
	if _, err := tx.Exec(ctx, `
		insert into legion_event_parts (project, first_seq, last_seq, run, state_id, at, bytes, events)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (project, first_seq) do nothing
	`, project, message.FirstSeq, message.Seq, message.Run, message.StateID, message.At, bytes, string(message.RawEvents)); err != nil {
		return fmt.Errorf("hold the state events of %s: %w", project, err)
	}
	return nil
}

// recordEvents appends, inside a batch's transaction, a row for each observation whose value
// differs from legion_current's for its field, and answers how many it wrote. A first `null` writes
// nothing, as a whole read's does (recordObservations), and a field the batch does not name is left
// as it was: an event names what moved and nothing else.
//
// A row is observed at the batch's `at`, the daemon's commit time, or 1 µs after the field's newest
// row where that is later: a whole read stamps its rows with Dispatch's own clock, which can be
// past the daemon's, and legion_current moves only forward, so an event after the read must land
// after it to be the field's newest. The rule holds the other way round too: a whole read stamps
// all its rows at one moment, 1 µs after the project's newest row where that is later than the
// read's time (recordObservations), since the daemon's clock can as well be past Dispatch's. So
// whichever of the two Dispatch writes last is each field's newest, whatever the two clocks say.
// A batch's rows keep their own times: the batch is one daemon transaction, at one `at`.
func recordEvents(ctx context.Context, tx pgx.Tx, project string, at time.Time, observations []Observation) (int64, error) {
	issueKeys, fields, values := observationColumns(observations)
	tag, err := tx.Exec(ctx, `
		with moved as (
			select moved.issue_key, moved.field, moved.value::jsonb as value
			from unnest($2::text[], $3::text[], $4::text[]) as moved(issue_key, field, value)
		)
		insert into legion_observations (project, issue_key, field, value, observed_at)
		select $1, moved.issue_key, moved.field, moved.value,
			greatest($5::timestamptz, newest.observed_at + interval '1 microsecond')
		from moved
		left join legion_current newest
			on newest.project = $1 and newest.issue_key = moved.issue_key and newest.field = moved.field
		where coalesce(newest.value, 'null'::jsonb) is distinct from moved.value
	`, project, issueKeys, fields, values, at)
	if err != nil {
		return 0, fmt.Errorf("store the Legion observations of %s's state events: %w", project, err)
	}
	return tag.RowsAffected(), nil
}

// lastValues is each field the events name at its last value, in sequence order: one fact can
// write a field twice in one transaction (a slot released and granted again), and only the value
// the transaction committed is one the daemon's state ever held.
func lastValues(events []stateEvent) []Observation {
	type field struct{ issue, name string }
	at := map[field]int{}
	var values []Observation
	for _, event := range events {
		for _, observation := range event.observations {
			key := field{observation.IssueKey, observation.Field}
			if index, seen := at[key]; seen {
				values[index].Value = observation.Value
				continue
			}
			at[key] = len(values)
			values = append(values, observation)
		}
	}
	return values
}

// stateMessage is one message of a daemon's state lane (packages/contracts' legionStateMessage):
// the state events of one transaction of the daemon, or of one part of a transaction the daemon
// split over several (More), which share the transaction's project, run, store and commit time.
// Every member is read loosely: a member or an event kind a newer daemon adds is ignored, as every
// consumer of the bus ignores what it does not know.
type stateMessage struct {
	// Project is the Dispatch project key the daemon publishes for.
	Project string `json:"project"`
	// Run is the run id (a UUID) of the daemon process that wrote the events.
	Run string `json:"run"`
	// StateID is the id of the store the events are counted in (the state route's
	// daemon.stateId).
	StateID string `json:"stateId"`
	// FirstSeq and Seq are the first and the last event's state sequence.
	FirstSeq int64 `json:"firstSeq"`
	Seq      int64 `json:"seq"`
	// At is the transaction's commit time on the daemon's clock.
	At time.Time `json:"at"`
	// More is a part of a transaction whose next part is still to come.
	More bool `json:"more"`
	// RawEvents is the message's events as the daemon sent them, which a held part keeps
	// (holdPart), and Events the same decoded and observed (decodeEvents).
	RawEvents json.RawMessage `json:"events"`
	Events    []stateEvent    `json:"-"`
}

// continues reports whether the message is the next part of the transaction whose last part held
// is last: its first sequence the one after last's, under last's run and store.
func (m stateMessage) continues(last part) bool {
	return m.FirstSeq == last.lastSeq+1 && m.Run == last.run && m.StateID == last.stateID
}

// The kinds of state event (packages/daemon's state lane).
const (
	eventChange    = "change"
	eventAdmission = "admission"
	eventBoot      = "boot"
)

// stateEvent is one state event, in the members of its three kinds Dispatch reads: a change names
// its issue and the fields that moved; an admission event carries the whole admission block the
// state route serves, of which Dispatch keeps its own members (Admission); a boot carries the
// settings the state route serves and the schema version of the daemon's store once the boot's
// migrations ran.
type stateEvent struct {
	Seq           int64                      `json:"seq"`
	Kind          string                     `json:"kind"`
	Issue         string                     `json:"issue"`
	Changes       []stateChange              `json:"changes"`
	Admission     *Admission                 `json:"admission"`
	Settings      map[string]json.RawMessage `json:"settings"`
	SchemaVersion int64                      `json:"schemaVersion"`

	// observations is what the event observes, each value through its field's one conversion
	// (state.go's phaseValue and the rest), which decodeEvents fills.
	observations []Observation
}

// stateChange is one field of a change event: the field and its new value, the value the state
// route serves at that issue. A claim names its role. The old value is not read: legion_current
// holds what Dispatch observed last.
type stateChange struct {
	Field string          `json:"field"`
	Role  string          `json:"role"`
	New   json.RawMessage `json:"new"`
}

// The fields a change event names (packages/contracts' legionStateChange).
const (
	changePhase     = "phase"
	changeTree      = "tree"
	changeSlot      = "slot"
	changeWaitingOn = "waitingOn"
	changeClaim     = "claim"
)

// decodeStateMessage decodes one message's envelope, unless keying decoded it already, and its
// payload, and converts each event into the observations it makes (decodeEvents). A message with
// no project, no store, no sequence, sequences that do not run forwards or no time, events that do
// not run from firstSeq to seq one sequence each, or an event whose value is not of its field's
// type, does not decode. The events' rule is the contract's (legionStateMessage's refine): the
// cursor moves to a message's seq (applyBatch), so a seq its events do not reach would move it
// past every later event, and no whole read would follow.
func decodeStateMessage(envelope *contracts.Envelope, data []byte) (stateMessage, error) {
	if envelope == nil {
		var decoded contracts.Envelope
		if err := json.Unmarshal(data, &decoded); err != nil {
			return stateMessage{}, fmt.Errorf("decode the envelope: %w", err)
		}
		envelope = &decoded
	}
	var message stateMessage
	if err := json.Unmarshal([]byte(envelope.Payload), &message); err != nil {
		return stateMessage{}, fmt.Errorf("decode the payload: %w", err)
	}
	switch {
	case message.Project == "":
		return stateMessage{}, errors.New("the message names no project")
	case message.StateID == "":
		return stateMessage{}, errors.New("the message names no store (stateId)")
	case message.FirstSeq < 1 || message.Seq < message.FirstSeq:
		return stateMessage{}, fmt.Errorf("the message's sequences %d to %d do not run forwards from 1", message.FirstSeq, message.Seq)
	case message.At.IsZero():
		return stateMessage{}, errors.New("the message carries no time")
	}
	events, err := decodeEvents(message.RawEvents)
	if err != nil {
		return stateMessage{}, err
	}
	if int64(len(events)) != message.Seq-message.FirstSeq+1 {
		return stateMessage{}, fmt.Errorf("the message's sequences %d to %d carry %d events: its events must run from firstSeq to seq, one sequence each",
			message.FirstSeq, message.Seq, len(events))
	}
	for index, event := range events {
		if want := message.FirstSeq + int64(index); event.Seq != want {
			return stateMessage{}, fmt.Errorf("the message's event %d is at sequence %d, not %d: its events must run from firstSeq to seq, one sequence each",
				index, event.Seq, want)
		}
	}
	message.Events = events
	return message, nil
}

// decodeEvents decodes the JSON array of a message's events, in the order the daemon sent them,
// which is sequence order (decodeStateMessage holds a message to it, and a held part is a message
// that decoded), and fills each event's observations (observe): a message's as it arrives, and a
// held part's as it was held.
func decodeEvents(raw json.RawMessage) ([]stateEvent, error) {
	var events []stateEvent
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &events); err != nil {
			return nil, fmt.Errorf("decode the events: %w", err)
		}
	}
	for index := range events {
		if err := events[index].observe(); err != nil {
			return nil, fmt.Errorf("event %d: %w", events[index].Seq, err)
		}
	}
	return events, nil
}

// observe fills the event's observations. An event of a kind this build does not know, and a
// change of a field it does not know, observes nothing.
func (e *stateEvent) observe() error {
	switch e.Kind {
	case eventChange:
		if e.Issue == "" {
			return errors.New("a change names no issue")
		}
		for _, change := range e.Changes {
			observation, known, err := changeObservation(e.Issue, change)
			if err != nil {
				return fmt.Errorf("%s of %s: %w", change.Field, e.Issue, err)
			}
			if known {
				e.observations = append(e.observations, observation)
			}
		}
	case eventAdmission:
		e.observations = []Observation{{IssueKey: AdmissionKey, Field: FieldAdmission, Value: admissionValue(e.Admission)}}
	case eventBoot:
		e.observations = []Observation{{IssueKey: AdmissionKey, Field: FieldSettings, Value: settingsValue(e.Settings)}}
	}
	return nil
}

// changeObservation is one change as the observation of its field, its new value decoded into the
// type the state document carries that field in and converted by the field's one conversion, so
// it is the value a whole read of the same state records. A `null` is the field the issue carries
// none of, which a whole read records as `null` too. known is false for a field this build does not
// observe.
func changeObservation(issue string, change stateChange) (observation Observation, known bool, err error) {
	observation.IssueKey = issue
	switch change.Field {
	case changePhase:
		var phase *string
		if err := decodeNew(change.New, &phase); err != nil {
			return Observation{}, false, err
		}
		observation.Field, observation.Value = FieldPhase, encodeValue(nil)
		if phase != nil {
			observation.Value = phaseValue(*phase)
		}
	case changeTree:
		var tree *string
		if err := decodeNew(change.New, &tree); err != nil {
			return Observation{}, false, err
		}
		observation.Field, observation.Value = FieldTree, treeValue(tree)
	case changeSlot:
		var index *int
		if err := decodeNew(change.New, &index); err != nil {
			return Observation{}, false, err
		}
		var slot *Slot
		if index != nil {
			slot = &Slot{Index: *index}
		}
		observation.Field, observation.Value = FieldSlot, slotValue(slot)
	case changeWaitingOn:
		observation.Field, observation.Value = FieldWaitingOn, waitingOnValue(change.New)
	case changeClaim:
		if change.Role == "" {
			return Observation{}, false, errors.New("a claim names no role")
		}
		var state *string
		if err := decodeNew(change.New, &state); err != nil {
			return Observation{}, false, err
		}
		claim := ""
		if state != nil {
			claim = *state
		}
		observation.Field, observation.Value = ClaimFieldPrefix+change.Role, claimValue(claim)
	default:
		return Observation{}, false, nil
	}
	return observation, true, nil
}

// decodeNew decodes a change's new value into target, leaving it nil where the value is absent.
func decodeNew(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, target)
}

// eventsAfter is each of events above lastSeq, all of them while the cursor holds no sequence.
func eventsAfter(events []stateEvent, lastSeq *int64) []stateEvent {
	if lastSeq == nil {
		return events
	}
	var after []stateEvent
	for _, event := range events {
		if event.Seq > *lastSeq {
			after = append(after, event)
		}
	}
	return after
}

// standsAt is the position a message is judged on (unfollowed, movesSchema): the last held part's
// sequence and run where parts are held, and the cursor's otherwise. seq is nil for a position
// that holds no sequence, which any message follows.
func standsAt(current cursor, held []part) (seq *int64, run *string) {
	if n := len(held); n > 0 {
		return &held[n-1].lastSeq, &held[n-1].run
	}
	return current.lastSeq, current.run
}

// unfollowed is the whole read a message calls for because it does not follow the position
// current and the parts held for it stand at (standsAt), or "" for a message that follows it. A
// boot above the position that is not the next sequence is ReadReasonBoot. A boot at or below the
// position is a replay of one the position already holds when its run is the position's run - the
// run of the newest message applied or held, or the stateRun of the daemon that served the last
// good whole read (cursor.afterRead) - and otherwise a daemon whose sequence went back (a database
// restore), which is ReadReasonBoot too. A message whose first sequence is past the next one is
// ReadReasonGap: events the stream no longer holds. So is a message above the held parts that does
// not continue them (stateMessage.continues): a part of another run or another store, or one that
// overlaps them, after which the transaction they began cannot be completed.
func (m stateMessage) unfollowed(current cursor, held []part) string {
	last, run := standsAt(current, held)
	if last == nil {
		return ""
	}
	for _, event := range m.Events {
		if event.Kind != eventBoot {
			continue
		}
		switch {
		case event.Seq > *last && event.Seq != *last+1:
			return ReadReasonBoot
		case event.Seq <= *last && (run == nil || *run != m.Run):
			return ReadReasonBoot
		}
	}
	if m.FirstSeq > *last+1 {
		return ReadReasonGap
	}
	if n := len(held); n > 0 && m.Seq > *last && !m.continues(held[n-1]) {
		return ReadReasonGap
	}
	return ""
}

// boots reports whether the message carries a boot event: its daemon run started (cursor.drops
// reads a boot of a store the cursor forgets as a sign that the setting's daemon may now count in
// it).
func (m stateMessage) boots() bool {
	for _, event := range m.Events {
		if event.Kind == eventBoot {
			return true
		}
	}
	return false
}

// movesSchema reports whether the message carries a boot above the position current and the parts
// held for it stand at (standsAt) whose schemaVersion is not the one the cursor holds, where the
// cursor holds one and the position holds a sequence: the boot moved the store's schema, and a
// migration may have rewritten what Dispatch observes outside the writes that publish events
// (ReadReasonSchema). judge asks it only of a message that follows the position (unfollowed). A
// boot that follows with the cursor's schema version moves nothing, whatever run the cursor holds.
func (m stateMessage) movesSchema(current cursor, held []part) bool {
	last, _ := standsAt(current, held)
	if last == nil || current.schemaVersion == nil {
		return false
	}
	for _, event := range m.Events {
		if event.Kind == eventBoot && event.Seq > *last && event.SchemaVersion != *current.schemaVersion {
			return true
		}
	}
	return false
}
