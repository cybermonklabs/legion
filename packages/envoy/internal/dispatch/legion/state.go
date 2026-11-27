package legion

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
)

// The Go mirror of the part of a Legion daemon's `GET /legion/v1/state` Dispatch records. The
// daemon owns the wire shape (packages/daemon/internal/api/state.go, mirrored for TypeScript
// readers in packages/contracts/src/legion-api.ts); this names only the fields Dispatch observes
// and lets encoding/json ignore every other one, which is what makes the decode loose:
//
//   - The route widens (dispatch://INFRA-980 is one such widening: `admission.active` listing
//     every issue holding a slot, children included, `admission.waiting` ordered by tree root,
//     `admission.blocked` naming a parent waiting for its children, an `admission.trees` of the
//     live roots, a `tree` on each issue, and a higher DaemonAPIVersion). A reader that refused
//     an unknown field would stop reading the day a daemon rolls onto a wider route, so nothing
//     here uses a strict decode.
//   - Nothing here takes a slot holder for a root: an issue's slot is read from that issue's own
//     `slot`, never from `admission.active`, and the admission block below is recorded as the
//     daemon sent it rather than read as a list of roots.
//   - Tree is optional and stays optional: a daemon that names no tree is read through its
//     issues' parent chain instead.

// State is a daemon's state as Dispatch records it: its issues, its admission block, the settings
// it reports, and nothing else of the document.
type State struct {
	Issues map[string]Issue `json:"issues"`
	// Admission is the daemon's admission block, absent from a document that carries none: the
	// Legion board reads the waiting line's own order from it (the spec's "Rule details": that
	// order puts answered roots first and follows Dispatch's rank after them, so Dispatch's rank
	// alone is not it), the blockers each held issue waits on, and the slot cap and the number of
	// P0 holders its header shows.
	Admission *Admission `json:"admission,omitempty"`
	// Settings is the settings the daemon reports, which hold no secret, each value kept as the
	// bytes the daemon sent: the board carries them and Dispatch models none of them, so a key
	// added to legion.yaml is recorded the day a daemon sends it.
	Settings map[string]json.RawMessage `json:"settings,omitempty"`
	// ControllerLocator is the daemon's record of the session registered as the project's
	// controller (packages/contracts/src/legion-api.ts, legionControllerLocator), absent until a
	// session registers. Dispatch keeps its session alone: the reject of a production event
	// (dispatch://INFRA-978, R8) proves the project's controller by that session.
	ControllerLocator *ControllerLocator `json:"controllerLocator,omitempty"`
}

// ControllerLocator is the members of the daemon's controllerLocator that Dispatch records.
type ControllerLocator struct {
	SessionID string `json:"sessionId,omitempty"`
}

// Admission is the admission block in the members Dispatch records. Every member is optional and
// every other field of the block is ignored, so a daemon that grows one (dispatch://INFRA-980's
// `trees`, which is read here the day a daemon sends it) is recorded, not refused.
type Admission struct {
	// Cap is the deployment's admission cap, the slots the header counts against.
	Cap *int `json:"cap,omitempty"`
	// P0Holders is how many of the issues holding a slot are P0 and hold it outside the cap, the
	// daemon's own count (dispatch://INFRA-5636), which the board's header copies and never
	// counts itself. It is absent from the state of a daemon before that change and from the body
	// a daemon serves a caller below that contract, so none stays none here rather than 0.
	P0Holders *int `json:"p0Holders,omitempty"`
	// Active is every issue holding a slot, which a daemon may widen to children as well as
	// roots (dispatch://INFRA-980). The board counts the slots in use from it, and from each
	// issue's own `slot` only where a daemon sends no `active`.
	Active []string `json:"active,omitempty"`
	// Waiting is the waiting line in the daemon's own order, which the board's Queue column shows
	// without re-sorting.
	Waiting []string `json:"waiting,omitempty"`
	// Trees is the live tree roots, where the daemon names them (dispatch://INFRA-980).
	Trees []string `json:"trees,omitempty"`
	// Blocked is each issue a blocker link holds, with the issues it waits on.
	Blocked []BlockedIssue `json:"blocked,omitempty"`
}

// BlockedIssue is one entry of `admission.blocked`: the issue, and the issues nobody has finished
// that hold it (record.BlockedIssue on the daemon's side).
type BlockedIssue struct {
	Issue     string   `json:"issue"`
	BlockedBy []string `json:"blockedBy,omitempty"`
}

// Issue is one issue of the daemon's record, in the fields Dispatch observes.
type Issue struct {
	Key   string `json:"key"`
	Phase string `json:"phase"`
	// Tree is the issue's tree root (dispatch://INFRA-980), absent from every daemon before it: a
	// pointer so "the daemon sent no tree field" and "the daemon named a root" stay apart.
	Tree *string `json:"tree,omitempty"`
	// Slot is the admission slot the issue holds, absent while it holds none.
	Slot *Slot `json:"slot,omitempty"`
	// WaitingOn is what a root in awaiting_person waits on from a person, kept as the bytes the
	// daemon sent: Dispatch records the wait, it does not model it, so a field added to it is
	// recorded rather than dropped.
	WaitingOn json.RawMessage   `json:"waitingOn,omitempty"`
	Architect *Claim            `json:"architect,omitempty"`
	Workers   map[string]Worker `json:"workers,omitempty"`
}

// Slot is the admission slot an issue holds.
type Slot struct {
	Index int `json:"index"`
}

// Claim is one role's claim, in the one field Dispatch observes: where it is in its life
// (supervise.ClaimState).
type Claim struct {
	State string `json:"state"`
}

// Worker is one phase worker of an issue, keyed by its role in Issue.Workers.
type Worker struct {
	Claim Claim `json:"claim"`
}

// The fields a read observes, one row of legion_observations each where the value moved.
const (
	FieldPhase     = "phase"
	FieldSlot      = "slot"
	FieldWaitingOn = "waiting_on"
	// FieldTree is the issue's tree root as the daemon named it (dispatch://INFRA-980). It is
	// observed only for an issue carrying one: a daemon that sends none leaves the reader to the
	// parent chain rather than to a guess.
	FieldTree = "tree"
	// FieldAdmission is the daemon's whole admission block, observed once per read under
	// AdmissionKey rather than on any issue: the waiting line's order, the blockers and the cap
	// belong to the project, not to one issue.
	FieldAdmission = "admission"
	// FieldSettings is the settings the daemon reports, observed under AdmissionKey beside the
	// admission block: they belong to the project's daemon, not to an issue, and the board
	// carries them (api/legion_board.go).
	FieldSettings = "settings"
	// FieldController is the session the daemon names as the project's registered controller,
	// observed under AdmissionKey: `null` while no session is registered (ControllerSession).
	FieldController = "controller"
	// ClaimFieldPrefix starts the field of one role's claim state: `claim.architect`,
	// `claim.implementer`.
	ClaimFieldPrefix = "claim."
)

// The phases the daemon reports, which is what a FieldPhase observation holds
// (packages/daemon/internal/phase). A reader of the board or of the metrics names them from
// here, so the two never spell one of them differently. A daemon below API contract 86 serves
// `awaiting_merge` and one at 86 or later never does (packages/daemon/internal/api/version.go,
// 86), so the board and the metrics read PhaseAwaitingMerge in what the sampler observed of such
// an earlier daemon. PhaseAwaitingChildren is a parent that an open Dispatch child of it holds
// (dispatch://INFRA-3320, R11): no worker of it works, and it keeps no slot.
const (
	PhaseAdmitted         = "admitted"
	PhaseAwaitingPerson   = "awaiting_person"
	PhaseAwaitingChildren = "awaiting_children"
	PhasePlanning         = "planning"
	PhaseImplementing     = "implementing"
	PhaseTesting          = "testing"
	PhaseReviewing        = "reviewing"
	PhaseAccepting        = "accepting"
	PhaseRetro            = "retro"
	PhaseMerging          = "merging"
	PhaseAwaitingMerge    = "awaiting_merge"
	PhaseDone             = "done"
	PhaseHeld             = "held"
)

// JSONString is a JSON text's string value, or "" for a null, a number, or anything else: an
// observation's value is whatever the daemon sent, so a reader of one names what it expects
// rather than trusting the shape.
func JSONString(raw []byte) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

// AdmissionKey is the issue key every project-level observation is written under — the admission
// block, the settings and the controller session: the empty string, which no issue key can be
// (legion_observations.issue_key is free text with no foreign key, so the rows cost no issue).
const AdmissionKey = ""

// Observation is one field of one issue as a read found it, the value JSON-encoded as
// legion_observations holds it.
type Observation struct {
	IssueKey string
	Field    string
	Value    json.RawMessage
}

// Observations is every observed field of every issue the read carries, by issue key and then by
// field, so two reads of one state produce one list. The project's own three come first, under
// AdmissionKey: the admission block, the settings the daemon reports and the session it names as
// its registered controller, none of which belongs to an issue.
//
// Phase, slot and the wait are observed on every issue, `null` where the issue carries none, so a
// slot released or a wait answered is a change like any other; a first `null` writes no row
// (store.go's insert). A claim is observed for each role the read names — the architect and each
// worker — and a tree only for an issue the daemon named one for. A field this list does not
// carry is one the read no longer holds — an issue gone from the daemon's state, a role's claim
// gone from an issue, a tree the daemon stopped naming — and the store writes it `null` where its
// newest row holds a value (recordObservations), so the newest rows are this read's and nothing
// older. The issue's status and its pull request are not observed, since neither the board nor
// the metrics reads either: the metrics read an issue's status from Dispatch's own events
// (metrics' readStatusMarks).
func (s State) Observations() []Observation {
	keys := slices.Sorted(maps.Keys(s.Issues))
	// The three fields every issue carries and one claim each, and the project's three.
	observations := make([]Observation, 0, len(keys)*4+3)
	var controller any
	if s.ControllerLocator != nil {
		controller = optional(s.ControllerLocator.SessionID)
	}
	for _, project := range []struct {
		field string
		value any
	}{
		{FieldAdmission, s.Admission},
		{FieldSettings, s.Settings},
		{FieldController, controller},
	} {
		observations = append(observations, Observation{
			IssueKey: AdmissionKey, Field: project.field, Value: encodeValue(project.value),
		})
	}
	for _, key := range keys {
		issue := s.Issues[key]
		add := func(field string, value any) {
			observations = append(observations, Observation{IssueKey: key, Field: field, Value: encodeValue(value)})
		}
		add(FieldPhase, issue.Phase)
		if issue.Tree != nil {
			add(FieldTree, optional(*issue.Tree))
		}
		var slot any
		if issue.Slot != nil {
			slot = issue.Slot.Index
		}
		add(FieldSlot, slot)
		add(FieldWaitingOn, rawOrNil(issue.WaitingOn))
		if issue.Architect != nil {
			add(ClaimFieldPrefix+"architect", optional(issue.Architect.State))
		}
		for _, role := range slices.Sorted(maps.Keys(issue.Workers)) {
			add(ClaimFieldPrefix+role, optional(issue.Workers[role].Claim.State))
		}
	}
	return observations
}

// encodeValue is value as the jsonb column holds it: its JSON, or `null` for a value that does not
// encode, since a column that takes no half-written value is better left saying the read found
// nothing than refusing every other field of the issue. A NUL in any of its strings is written as
// U+FFFD (withoutNUL), which jsonb takes.
func encodeValue(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("null")
	}
	return withoutNUL(encoded)
}

// nulEscape is how a JSON text writes the NUL character: valid JSON, and the one escape of a
// string the daemon's Go encoder can write that Postgres's jsonb refuses ("unsupported Unicode
// escape sequence", SQLSTATE 22P05). A read's observations go in one statement, so a wait whose
// detail carries one would fail every observation of the project at every read.
var nulEscape = []byte(`\u0000`)

// replacementEscape is U+FFFD, the character that stands for one that cannot be represented,
// written as an escape as long as nulEscape, so the rewrite is in place.
var replacementEscape = []byte(`\ufffd`)

// withoutNUL rewrites every NUL escape of encoded, in a key or a value, as U+FFFD, in place:
// encoded is json.Marshal's own fresh buffer. A backslash in valid JSON only ever starts an escape
// inside a string, so stepping over each escape whole tells the NUL escape from an escaped
// backslash followed by `u0000`, which is six characters of text and no NUL.
func withoutNUL(encoded []byte) []byte {
	if !bytes.Contains(encoded, nulEscape) {
		return encoded
	}
	for index := 0; index < len(encoded); index++ {
		if encoded[index] != '\\' {
			continue
		}
		if bytes.HasPrefix(encoded[index:], nulEscape) {
			copy(encoded[index:], replacementEscape)
			index += len(nulEscape) - 1
			continue
		}
		// The escaped character, which may itself be a backslash.
		index++
	}
	return encoded
}

// optional is a string field's value, or nil where the daemon left it out: every one of these is
// `omitempty` on the wire, so an empty string is an absent field rather than a value.
func optional(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// rawOrNil is a raw JSON field's value, or nil where the daemon left it out.
func rawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
