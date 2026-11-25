// Package stateevent is the wire shape of the daemon's state events, below every package that
// writes one: the payload of each of the three state rows (Change, Admission, Boot), the fields a
// change can name (Field), the JSON null a change carries for a field that holds none (Null), and
// the phase the state route and a change serve for an issue the daemon holds no record of
// (Unrecorded), with the checks a row must pass before it is queued (Validate).
//
// internal/record queues the rows of its own issue, slot and admission-block writes, internal/store
// computes the change of each claim write, and internal/daemon queues each run's boot row through
// record's insert (record.Postgres.Enqueue). The tests of internal/record import internal/store, so
// internal/store cannot import internal/record. So the shape is written here once: record's state
// payloads are types over this package's (record.StateChange over Change, StateAdmission over
// Admission, StateBoot over Boot), and the store hands each change it computes to the record
// store's queue of a state change (record.Postgres.EnqueueStateChange, which the daemon hands it
// through store.Store.PublishState), which checks it and inserts it as it checks and inserts the
// changes of record's own writes (record.NewOutboxRow, record.Postgres.Enqueue).
package stateevent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The outbox kinds of the three state rows, as record spells them (record.OutboxKindStateChange,
// record.OutboxKindStateAdmission and record.OutboxKindStateBoot).
const (
	KindChange    = "state_change"
	KindAdmission = "state_admission"
	KindBoot      = "state_boot"
)

// Change is what one write moved of what Dispatch observes of one issue, a write that moves several
// issues (a tree's slot release) queueing one Change for each: the issue and each field the write
// moved, from its old value to its new one (FieldChange). Project is the Dispatch project key the
// daemon works for, and the row's issue column holds that key too, so every state row of the
// project runs on its one state lane (record.LaneState). Issue is the issue the change is of. Run
// is the id of the daemon run that wrote the row. The row carries no sequence and no time: it
// takes both when its transaction commits (record.OutboxRow.StateSeq, StateAt), and the executor
// puts them, Project and Run on the message it publishes.
type Change struct {
	Project string        `json:"project"`
	Run     string        `json:"run"`
	Issue   string        `json:"issue"`
	Changes []FieldChange `json:"changes"`
}

// Field names one field of an issue that a state change can move.
type Field string

// The five fields Dispatch observes of an issue, named as the state route serves them.
const (
	FieldPhase     Field = "phase"
	FieldTree      Field = "tree"
	FieldSlot      Field = "slot"
	FieldWaitingOn Field = "waitingOn"
	FieldClaim     Field = "claim"
)

// Fields is every Field, which is every field a change may name.
var Fields = []Field{FieldPhase, FieldTree, FieldSlot, FieldWaitingOn, FieldClaim}

// FieldChange is one field a write moved. Role names the claim's role on a claim change, and is
// empty on a change of any other field. Old and New are the field's values before and after the
// write, each encoded as the state route serves it at that issue: a phase string, a tree key, a
// slot's index, the waitingOn object, a claim's state, or null where the issue holds none (and as
// Old on the issue's first write). They are raw JSON so that a null stays null and a value is the
// route's own encoding. A change carries both, so a nil value is refused: null is written as null
// (Null).
type FieldChange struct {
	Field Field           `json:"field"`
	Role  claim.Role      `json:"role,omitempty"`
	Old   json.RawMessage `json:"old"`
	New   json.RawMessage `json:"new"`
}

// Admission is the whole admission block the state route serves (api.Admission), written when an
// admission pass changed it. Project and Run are a Change's. The block is raw JSON because the
// writer cannot import api: it encodes record.AdmissionBlock, whose JSON is the route's own
// (record.Postgres.QueueAdmissionBlock).
type Admission struct {
	Project   string          `json:"project"`
	Run       string          `json:"run"`
	Admission json.RawMessage `json:"admission"`
}

// Boot is a daemon run's first state event: the settings the state route serves (api.SettingsOf),
// as raw JSON for the reason Admission's block is, and SchemaVersion, the highest migration version
// the store records once that boot's migrations ran (store.SchemaVersion), which the state route
// serves as `daemon.schemaVersion`. A reader compares it with the version its starting point holds:
// a boot that moved the schema can have rewritten what Dispatch observes outside every write that
// queues a state event. Project and Run are a Change's.
type Boot struct {
	Project       string          `json:"project"`
	Run           string          `json:"run"`
	Settings      json.RawMessage `json:"settings"`
	SchemaVersion int64           `json:"schemaVersion"`
}

// Null is the JSON null, the value a change names for a field that holds none: a tree cleared, a
// slot released, a wait ended, a claim the store held no row of, and every field before an issue's
// first write.
var Null = json.RawMessage("null")

// Unrecorded is the phase the state route serves, and a change names, for an issue the workflow does
// not record, where an operator's claim exists and an issue does not (LEGION-272). It is not a
// workflow phase: no issue record holds it, the transition table never reaches it, and no role
// works it, so it lives with the wire shapes that carry it (api.Unrecorded is this constant)
// rather than in the domain's phases.
const Unrecorded phase.Phase = "unrecorded"

// A state row's message is what Dispatch writes its observations from, so each row is checked
// before it is queued, by the checks below. They check the row's own fields, not the whole message
// the contract takes (legionStateMessage), which holds more than they do: each changed field's old
// and new value to that field's values, the run to a UUID, and the admission block and the
// settings to their own schemas.

// Validate refuses a change with no bare project key or no run (checkRow), of no issue key, that
// names no field, or that names a field outside Fields, a claim change with no claim role, a role
// on any other change, or an old or a new value that is no JSON value.
func (c Change) Validate() error {
	if err := checkRow(KindChange, c.Project, c.Run); err != nil {
		return err
	}
	if !claim.IsIssueKey(c.Issue) {
		return fmt.Errorf("a state change names the issue %q, which is no issue key", c.Issue)
	}
	if len(c.Changes) == 0 {
		return fmt.Errorf("a state change of %s names no field the write moved", c.Issue)
	}
	for _, change := range c.Changes {
		if !slices.Contains(Fields, change.Field) {
			return fmt.Errorf("a state change of %s names the field %q, which is none of %v", c.Issue, change.Field, Fields)
		}
		if change.Field == FieldClaim && !claim.IsRole(change.Role) {
			return fmt.Errorf("a claim change of %s names the role %q, which is none of a claim's six", c.Issue, change.Role)
		}
		if change.Field != FieldClaim && change.Role != "" {
			return fmt.Errorf("a %s change of %s names the role %q; only a claim change names one", change.Field, c.Issue, change.Role)
		}
		if !json.Valid(change.Old) || !json.Valid(change.New) {
			return fmt.Errorf("a %s change of %s carries an old or a new value that is no JSON value; write null as null", change.Field, c.Issue)
		}
	}
	return nil
}

// Validate refuses an admission event with no bare project key or no run (checkRow), or whose
// block is no JSON object.
func (a Admission) Validate() error {
	if err := checkRow(KindAdmission, a.Project, a.Run); err != nil {
		return err
	}
	if !jsonObject(a.Admission) {
		return fmt.Errorf("a state admission of %s carries no admission block", a.Project)
	}
	return nil
}

// Validate refuses a boot event with no bare project key or no run (checkRow), whose settings are
// no JSON object, or whose schema version is below zero.
func (b Boot) Validate() error {
	if err := checkRow(KindBoot, b.Project, b.Run); err != nil {
		return err
	}
	if !jsonObject(b.Settings) {
		return fmt.Errorf("a state boot of %s carries no settings", b.Project)
	}
	if b.SchemaVersion < 0 {
		return fmt.Errorf("a state boot of %s carries the schema version %d, which no store records", b.Project, b.SchemaVersion)
	}
	return nil
}

// checkRow holds the two fields every state row carries: the bare project key its lane is keyed
// by, in the one shape the contract takes for one (claim.IsProjectKey), and the run that wrote it.
func checkRow(kind, project, run string) error {
	if !claim.IsProjectKey(project) {
		return fmt.Errorf("a %s row names the project %q, which is no bare Dispatch project key", kind, project)
	}
	if run == "" {
		return fmt.Errorf("a %s row names no run", kind)
	}
	return nil
}

// jsonObject says whether raw is one JSON object.
func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return json.Valid(trimmed) && len(trimmed) > 0 && trimmed[0] == '{'
}
