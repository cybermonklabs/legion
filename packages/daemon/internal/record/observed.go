package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/stateevent"
)

// WaitingOnView is what a root in awaiting_person waits on from a person (Awaiting), as the state
// route serves it on the root's `waitingOn` (api.WaitingOnView, an alias of this type) and as a
// state event carries it (StateFieldWaitingOn). Kind is `ask` with Ask, the id of the oldest
// question of its tree still with the person, or `gate` with GateVersion and ArtifactID, the spec
// version of its registered document a person has not reviewed. Since is Dispatch's time of the
// hand-back the wait began at, absent when unknown. AnsweredAt and AnsweredBy are absent until a
// person's act answers the wait; the answered root then stands in admission's waiting line as
// well, among the issues whose work came back, oldest first, until the pass that resumes it.
// AnsweredBy is every value of Answer, `declined` — a person who closed an ask of the root rather
// than answer it (contract 68) — among them, and it names how the wait was answered, not always by
// a person.
type WaitingOnView struct {
	Kind        string     `json:"kind"`
	Ask         string     `json:"ask,omitempty"`
	GateVersion int        `json:"gateVersion,omitempty"`
	ArtifactID  string     `json:"artifactId,omitempty"`
	Since       *time.Time `json:"since,omitempty"`
	AnsweredAt  *time.Time `json:"answeredAt,omitempty"`
	AnsweredBy  Answer     `json:"answeredBy,omitempty"`
}

// waitingOnOf is the view of what a root in awaiting_person waits on: the gate shape when the wait
// names a spec version, otherwise the question; the answer only once a person gave one. The act
// that answered travels as the record's own word for it (Answer), `declined` for a person who
// closed an ask of the root rather than answer it among them, so a value the record gains is
// served as it stands and the contract number is what holds a plugin to the list it parses
// (api.DaemonAPIVersion). AnsweredDetail and the sequence of Since are no part of it.
func waitingOnOf(w Awaiting) *WaitingOnView {
	view := &WaitingOnView{Kind: "ask", Ask: w.Ask}
	if w.GateVersion != 0 {
		view = &WaitingOnView{Kind: "gate", GateVersion: w.GateVersion, ArtifactID: w.Artifact}
	}
	if !w.Since.At.IsZero() {
		since := w.Since.At
		view.Since = &since
	}
	if w.AnsweredAt != nil {
		answeredAt := *w.AnsweredAt
		view.AnsweredAt, view.AnsweredBy = &answeredAt, w.AnsweredBy
	}
	return view
}

// IssueObservation is what Dispatch observes of an issue's own record: the three fields of the
// issue's view on the state route that the record's issues row alone decides (the slot and the
// claims are rows of their own). Each field is the value the route serves at that issue, and each
// encodes as a state event names it (StateFieldChange): Phase is the phase string; Tree the root
// key of the issue's tree, nil for an issue that names none, which the route leaves out; WaitingOn
// the view of what the issue waits on from a person, nil unless the issue is a root in
// awaiting_person (Issue.AwaitingPerson), which the route leaves out too.
type IssueObservation struct {
	Phase     phase.Phase
	Tree      *string
	WaitingOn *WaitingOnView
}

// ObservationOf is what Dispatch observes of issue (IssueObservation). It is the one definition of
// those three values: the state route's projection builds each issue's view from it, and PutIssue
// compares it before and after each write to decide which fields a state event names.
func ObservationOf(issue Issue) IssueObservation {
	observed := IssueObservation{Phase: issue.Phase}
	if issue.Tree != "" {
		tree := issue.Tree
		observed.Tree = &tree
	}
	if issue.AwaitingPerson() {
		observed.WaitingOn = waitingOnOf(*issue.Awaiting)
	}
	return observed
}

// observedIssueFields is the fields of an issue a state change of PutIssue can name, in the order
// it names them, which is the order IssueObservation.encoded answers their values in.
var observedIssueFields = [...]StateField{StateFieldPhase, StateFieldTree, StateFieldWaitingOn}

// encoded is each field of o as a state event carries it, in the order of observedIssueFields: the
// JSON the state route serves at the issue, and null where it serves none.
func (o IssueObservation) encoded() ([len(observedIssueFields)]json.RawMessage, error) {
	var values [len(observedIssueFields)]json.RawMessage
	for i, value := range [...]any{o.Phase, o.Tree, o.WaitingOn} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return values, fmt.Errorf("encode the %s Dispatch observes: %w", observedIssueFields[i], err)
		}
		values[i] = encoded
	}
	return values, nil
}

// issueChanges is each field of an issue's observation that a write moved from before to after,
// in the order phase, tree, waitingOn, each with its encoded old and new value; none when the write
// moved nothing Dispatch observes. A nil before is an issue the write recorded for the first time:
// each of its fields was null, so a field whose first value is null too is named by none.
func issueChanges(before *IssueObservation, after IssueObservation) ([]StateFieldChange, error) {
	news, err := after.encoded()
	if err != nil {
		return nil, err
	}
	olds := [len(observedIssueFields)]json.RawMessage{stateevent.Null, stateevent.Null, stateevent.Null}
	if before != nil {
		if olds, err = before.encoded(); err != nil {
			return nil, err
		}
	}
	var changes []StateFieldChange
	for i, field := range observedIssueFields {
		if bytes.Equal(olds[i], news[i]) {
			continue
		}
		changes = append(changes, StateFieldChange{Field: field, Old: olds[i], New: news[i]})
	}
	return changes, nil
}

// observedIssueColumns is the issues columns ObservationOf reads, as scanObservedIssue scans them:
// the phase, the tree, and each awaiting_* column the waitingOn view carries or that says whether
// the issue waits at all (Issue.AwaitingPerson asks for an ask or a gate version).
// awaiting_answered_detail and awaiting_since_seq are not among them: the view carries neither,
// so a write that moves one of them alone moves nothing Dispatch observes.
const observedIssueColumns = `phase, tree, awaiting_ask, awaiting_gate_version, coalesce(awaiting_artifact, ''), awaiting_answered_at, coalesce(awaiting_answered_by, ''), awaiting_since`

// scanObservedIssue reads one row of observedIssueColumns, then extra: the destinations of any
// columns the statement selects after them. It builds the record's wait from the columns as
// scanIssue does (awaitingOf), so a value read here and the one the state route reads through
// Issue are the same value.
func scanObservedIssue(row scanner, extra ...any) (IssueObservation, error) {
	var issue Issue
	var phaseValue string
	var awaitingAsk *string
	var awaitingGate *int
	var awaitingArtifact, answeredBy string
	var answeredAt, since *time.Time
	if err := row.Scan(append([]any{&phaseValue, &issue.Tree, &awaitingAsk, &awaitingGate, &awaitingArtifact, &answeredAt,
		&answeredBy, &since}, extra...)...); err != nil {
		return IssueObservation{}, err
	}
	issue.Phase = phase.Phase(phaseValue)
	issue.Awaiting = awaitingOf(awaitingAsk, awaitingGate, awaitingArtifact, answeredAt, answeredBy, "", nil, since)
	return ObservationOf(issue), nil
}

// awaitingOf is the record's wait as the issues row stores it, nil for a row that names neither an
// ask nor a gate version. Its times are read in UTC, as every other time the record answers is.
func awaitingOf(ask *string, gate *int, artifact string, answeredAt *time.Time, answeredBy, answeredDetail string,
	sinceSeq *int64, since *time.Time) *Awaiting {
	if ask == nil && gate == nil {
		return nil
	}
	if answeredAt != nil {
		utc := answeredAt.UTC()
		answeredAt = &utc
	}
	awaiting := &Awaiting{Artifact: artifact, AnsweredAt: answeredAt, AnsweredBy: Answer(answeredBy),
		AnsweredDetail: answeredDetail, Since: scannedMark(sinceSeq, since)}
	if ask != nil {
		awaiting.Ask = *ask
	}
	if gate != nil {
		awaiting.GateVersion = *gate
	}
	return awaiting
}
