package legion

// issue.go holds the one per-issue rule of what the sampler recorded (dispatch://INFRA-4869): how
// an issue's newest observations read as its phase, its slot, what it waits on and the claims of
// its roles (IssueState), and where the project's admission block puts it (AdmissionOf, and
// Admission's Place, Blockers and BlockersOf). Two readers take an issue through it: the Legion
// board, whose cards are every issue of the project (api/legion_board.go), and one issue's read,
// GET /api/v1/issues/{key}/legion, which reads that issue alone (ReadIssue). One rule stated twice
// is two rules, and a card that put an issue in one phase, or a role in a turn, while the issue's
// own answer said another would be two answers about the same issue a page draws side by side (the
// spec's R1: "the card and the answer agree on the phase, the roles in a turn and the slot").

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/legionagents"
)

// claimWorking is the claim state of a role in a turn (the daemon's supervise.ClaimState), the one
// state the board's cards show a role for.
const claimWorking = "working"

// IssueState is one issue's newest observations, each taken through Observe: its phase and when
// Dispatch stored it, its slot, what it waits on, and every claim of its roles. The zero value is
// an issue nothing was observed of.
type IssueState struct {
	// Phase and PhaseSince are the newest phase observation and when it was recorded, which is
	// when the issue entered that phase (a row is written only where the value moved). Phase is ""
	// for a value that is not a string.
	Phase      string
	PhaseSince time.Time
	// Slot is the index of the admission slot the issue holds, nil while it holds none.
	Slot *int
	// WaitingOn is what a root in `awaiting_person` waits on, the daemon's object exactly as the
	// sampler stored it, nil where the issue waits on nothing. It stays bytes: Dispatch records
	// the wait and does not model it, so a field a later daemon adds reaches every reader, and each
	// reader takes from it what it shows (the board's card takes three fields).
	WaitingOn json.RawMessage
	// claims is the state of each role's claim, by role: every role the daemon names a claim of,
	// whatever its state, a `retired` claim and a role Dispatch does not know included.
	claims map[string]string
}

// RoleClaim is one role with a claim on an issue, and where that claim is in its life (the
// daemon's supervise.ClaimState: `working`, `idle`, `retired`, or whatever a later daemon names).
type RoleClaim struct {
	Role  string `json:"role"`
	State string `json:"state"`
}

// Observe takes one newest observation into the issue, as legion_current holds it: the field, its
// value as JSON and when it was recorded. A value that is not the shape the field is read as
// leaves the field as it stands: the daemon owns the wire shape, and a reader of one names what it
// expects rather than trusting it. A `null` is the sampler's word for a field the newest good read
// no longer carries (recordObservations), so it clears the slot, the wait and a role's claim; a
// claim whose value is not a string names no state, and is no claim either. A field this does not
// name, such as the issue's `tree`, is not part of its state. The slot is decoded by the one
// decode of the field (ParseSlot), which the stored per-issue facts read it by too. The wait keeps
// value's own bytes, so a caller hands value over and does not write to it again.
func (s *IssueState) Observe(field string, value []byte, at time.Time) {
	switch {
	case field == FieldPhase:
		s.Phase, s.PhaseSince = JSONString(value), at
	case field == FieldSlot:
		if slot, read := ParseSlot(value); read {
			s.Slot = slot
		}
	case field == FieldWaitingOn:
		switch trimmed := bytes.TrimSpace(value); {
		case string(trimmed) == "null":
			s.WaitingOn = nil
		case len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed):
			s.WaitingOn = json.RawMessage(trimmed)
		}
	case strings.HasPrefix(field, ClaimFieldPrefix):
		role, state := strings.TrimPrefix(field, ClaimFieldPrefix), JSONString(value)
		if state == "" {
			delete(s.claims, role)
			return
		}
		if s.claims == nil {
			s.claims = map[string]string{}
		}
		s.claims[role] = state
	}
}

// Working is the roles of the issue in a turn, their claim `working`, sorted by name: the roles a
// board card shows. It is never nil, so a card with no role in a turn carries an empty list.
func (s *IssueState) Working() []string {
	working := []string{}
	for role, state := range s.claims {
		if state == claimWorking {
			working = append(working, role)
		}
	}
	slices.Sort(working)
	return working
}

// Roles is every claim of the issue with its state: the roles legionagents.Roles names first, in
// that order, which is the order of the default steps and the one an issue's agents are listed in,
// then every other role by name, so a role a later daemon adds is shown after the ones Dispatch
// knows rather than dropped. It is never nil.
func (s *IssueState) Roles() []RoleClaim {
	roles := make([]RoleClaim, 0, len(s.claims))
	for _, role := range legionagents.Roles {
		if state, held := s.claims[role]; held {
			roles = append(roles, RoleClaim{Role: role, State: state})
		}
	}
	known := len(roles)
	for role, state := range s.claims {
		if !slices.Contains(legionagents.Roles, role) {
			roles = append(roles, RoleClaim{Role: role, State: state})
		}
	}
	slices.SortFunc(roles[known:], func(a, b RoleClaim) int { return strings.Compare(a.Role, b.Role) })
	return roles
}

// Place is where key stands in the daemon's waiting line: its index in Waiting plus one, so the
// first in the line is 1, and the line's length. ok is false, with both 0, where the line does not
// name key, and for a nil Admission, a project whose daemon recorded none. An issue the line names
// twice, which no daemon sends, stands where it is named first, as the board's Queue column shows
// it.
func (a *Admission) Place(key string) (place, length int, ok bool) {
	if a == nil {
		return 0, 0, false
	}
	index := slices.Index(a.Waiting, key)
	if index < 0 {
		return 0, 0, false
	}
	return index + 1, len(a.Waiting), true
}

// AdmissionOf is the admission block a newest `admission` value holds, or nil where the value does
// not decode as one. The board and one issue's read both take the project's `admission` row
// through it (api/legion_board.go's observeProject, ReadIssue), so both read a value of another
// shape as no block: the daemon owns the wire shape, and a reader of one names what it expects
// rather than trusting it. It is the one decode of the block, which the metrics and the stored
// per-issue facts read it by too.
func AdmissionOf(value []byte) *Admission {
	var admission Admission
	if err := json.Unmarshal(value, &admission); err != nil {
		return nil
	}
	return &admission
}

// Blockers is the issues `admission.blocked` names as holding each issue, by that issue, each list
// in the order the daemon stored it, never sorted, and nil for a nil Admission. It is the one rule
// of which entry names an issue's blockers: the map is filled in the block's stored order, so an
// issue two entries name, which no daemon sends, takes the last of them. The waiting line the
// board and the stored facts place their issues by reads the whole map once (NewWaitingLine), and
// BlockersOf reads one issue's list from it. Each list is the block's own; a caller reads it and
// does not change it.
func (a *Admission) Blockers() map[string][]string {
	if a == nil {
		return nil
	}
	blockers := make(map[string][]string, len(a.Blocked))
	for _, blocked := range a.Blocked {
		blockers[blocked.Issue] = blocked.BlockedBy
	}
	return blockers
}

// BlockersOf is the issues `admission.blocked` names as holding key, by Blockers' rule: in the
// order the daemon stored them, never sorted, and the last entry's list where two entries name
// key. It is an empty list, never nil, where no entry names key, where the entry's list is nil,
// and for a nil Admission, so an answer that carries it says [] rather than null. A list that is
// not empty is the block's own; a caller reads it and does not change it.
func (a *Admission) BlockersOf(key string) []string {
	if blockers := a.Blockers()[key]; blockers != nil {
		return blockers
	}
	return []string{}
}

// IssueCurrent is one issue's Legion state as Dispatch holds it (ReadIssue): whether the daemon
// records the issue, its newest observations, whether Dispatch saw its phase start, and the
// project's admission block, which places it in the waiting line and names what holds it.
type IssueCurrent struct {
	// Recorded is whether the daemon records the issue: its newest `phase` row holds a value. An
	// issue with no `phase` row was never in a state Dispatch recorded, which covers every issue of
	// a project whose daemon no read has answered, and one whose newest `phase` row is `null` left
	// the daemon's state (recordObservations writes every field of an issue gone from the state
	// `null`). A reader shows no state of an issue that is not recorded, whatever its other rows
	// hold.
	Recorded bool
	// State is the issue's newest observations, each taken through IssueState.Observe.
	State IssueState
	// PhaseStartSeen is whether Dispatch saw the issue enter the phase it is in: legion_observations
	// holds a `phase` row of the issue older than the newest one whose value is distinct from the
	// newest one's, a `null` counting as a value. Where it is false, the phase is the first value
	// Dispatch stored for the issue, so PhaseSince is when Dispatch first saw it there, not when it
	// started; after planning, `null`, planning, the current planning is not the first value
	// stored, so it is seen, and PhaseSince is when the issue came back (dispatch://INFRA-4869's
	// plan, decision 5). The sampler never writes two equal rows in a row (recordObservations), so
	// "distinct" changes nothing for its rows; it keeps the answer right for rows another writer
	// left that break that rule.
	PhaseStartSeen bool
	// Admission is the project's admission block as the newest read recorded it, nil while no read
	// has recorded one: before the first read of a daemon, for a daemon that sends none, and for a
	// value of another shape, which the board reads as none too (AdmissionOf).
	Admission *Admission
}

// ReadIssue reads one issue's Legion state in one statement over legion_current: the issue's
// rows and the project's `admission` row, and nothing else of the project (not its `settings`
// row, and no other issue's rows). It takes ReadCurrent's null rule: a row whose newest value is
// `null` is a field the newest good read no longer carries, so it is observed as absent, and an
// issue whose newest `phase` row is `null` is not recorded (IssueCurrent.Recorded). The `phase`
// row's answer of IssueCurrent.PhaseStartSeen is a probe of the issue's own `phase` rows in
// legion_observations before the newest, through the index legion_observations_point (project,
// issue_key, field, observed_at), so it reads none of the history of any other field or issue.
//
// A caller that reads it beside another sampler table does so in one repeatable-read
// transaction (Queryer), as the board reads ReadCurrent, so the answer is one moment's.
func ReadIssue(ctx context.Context, db Queryer, project, key string) (IssueCurrent, error) {
	rows, err := db.Query(ctx, `
		select newest.issue_key, newest.field, newest.value, newest.observed_at,
			case when newest.field = $3 then exists (
				select 1
				from legion_observations older
				where older.project = newest.project and older.issue_key = newest.issue_key
					and older.field = newest.field and older.observed_at < newest.observed_at
					and older.value is distinct from newest.value
			) else false end
		from legion_current newest
		where newest.project = $1 and newest.value <> 'null'::jsonb
			and (newest.issue_key = $2 or (newest.issue_key = $4 and newest.field = $5))
	`, project, key, FieldPhase, AdmissionKey, FieldAdmission)
	if err != nil {
		return IssueCurrent{}, fmt.Errorf("read the Legion state of %s: %w", key, err)
	}
	defer rows.Close()
	var current IssueCurrent
	for rows.Next() {
		var issueKey, field string
		var value []byte
		var observedAt time.Time
		var startSeen bool
		if err := rows.Scan(&issueKey, &field, &value, &observedAt, &startSeen); err != nil {
			return IssueCurrent{}, fmt.Errorf("scan a Legion observation of %s: %w", key, err)
		}
		if issueKey == AdmissionKey && field == FieldAdmission {
			current.Admission = AdmissionOf(value)
			continue
		}
		if field == FieldPhase {
			current.Recorded, current.PhaseStartSeen = true, startSeen
		}
		current.State.Observe(field, value, observedAt)
	}
	if err := rows.Err(); err != nil {
		return IssueCurrent{}, fmt.Errorf("read the Legion state of %s: %w", key, err)
	}
	return current, nil
}
