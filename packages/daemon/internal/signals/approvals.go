package signals

import (
	"maps"
	"slices"
)

// The approvals signal counts the asks open that wait on the project's default assignee. This file
// is the lifecycle of one ask as Dispatch's events turn it, over normalized events (AskEvent), and
// the state of every ask, each issue's assignee and the project's default assignee, for a caller
// that applies each event as it comes (ApprovalState). The decoding of an event, its moment and
// the history a caller keeps of its asks stay with the caller.

// AskEventKind is the kind of an event of an ask, by the part of Dispatch's event type after
// `ask.`; AskCommented is a comment on an ask (comment.created, comment.answered).
type AskEventKind string

const (
	AskOpened     AskEventKind = "opened"
	AskAskedOf    AskEventKind = "asked_of"
	AskEdited     AskEventKind = "edited"
	AskHandedBack AskEventKind = "handed_back"
	AskAnswered   AskEventKind = "answered"
	AskResolved   AskEventKind = "resolved"
	AskCommented  AskEventKind = "commented"
)

// AskReport is what an event's payload says of the ask's own state: nothing, open, or any other
// state (answered, resolved, declined).
type AskReport string

const (
	ReportsNothing AskReport = ""
	ReportsOpen    AskReport = "open"
	ReportsClosed  AskReport = "closed"
)

// AskWaiting is whom a comment on an open ask leaves it waiting on: the person, after an agent's
// comment, or the agent, after a person's reply.
type AskWaiting string

const (
	WaitsOnHuman AskWaiting = "human"
	WaitsOnAgent AskWaiting = "agent"
)

// AskApproval is what an event reports of an approval request's versions: its version, and the
// version the person's decision was requested on, each nil where the event gives none.
type AskApproval struct {
	Version, RequestedVersion *int
}

// AskEvent is one event of an ask, normalized. Ask is the ask's id and Issue its issue, which the
// first event of an ask gives. State is what the payload says of the ask's state. AskedOfSet says
// whether the payload names asked_of at all, null included, and AskedOf is the person it names, ""
// for null: an event that names none leaves the ask's person as it stood. Approval is non-nil only
// where the ask is an approval request and the event carries its versions. WaitingOn is a
// comment's ask_waiting_on, and only an AskCommented event reads it.
type AskEvent struct {
	Kind       AskEventKind
	Ask        string
	Issue      string
	State      AskReport
	AskedOfSet bool
	AskedOf    string
	Approval   *AskApproval
	WaitingOn  AskWaiting
}

// Ask is one ask as its events have left it: whether it is open, whether it waits on a person
// rather than an agent, and whom it is asked of, "" for none.
type Ask struct {
	ID, Issue string
	Open      bool
	Human     bool
	AskedOf   string
}

// Waits says whether the ask is open and waits on a person.
func (a Ask) Waits() bool { return a.Open && a.Human }

// NextAsk is the ask after event e, prev being the ask before it and known whether an earlier event
// made it, and whether e changed it. It follows the daemon's own reading of Dispatch's events and
// Dispatch's contract:
//
//   - opened opens the ask waiting on a person, and any event whose payload's state is open
//     reopens a closed one so, waiting on a person (a re-ask, or a restored one); answered and
//     resolved close it, a decline among them, and a payload state other than open closes it;
//   - an edit that moves an approval request, its requested version below its version, leaves it
//     waiting on its agent, and any other edit leaves its turn as it was; handed_back, the agent
//     handing it back to the person, leaves it waiting on the person;
//   - a comment on a known ask leaves it waiting on whom the comment names, the agent after a
//     person's reply and the person after an agent's, and changes nothing for an ask no event
//     made or a comment that names neither;
//   - asked_of, where an event names it, sets the person the ask is asked of, null clearing it.
func NextAsk(prev Ask, known bool, e AskEvent) (Ask, bool) {
	if e.Kind == AskCommented {
		if !known || (e.WaitingOn != WaitsOnHuman && e.WaitingOn != WaitsOnAgent) {
			return prev, false
		}
		prev.Human = e.WaitingOn == WaitsOnHuman
		return prev, true
	}
	next := prev
	if !known {
		next = Ask{ID: e.Ask, Issue: e.Issue}
	}
	open, human := next.Open, next.Human
	switch e.Kind {
	case AskOpened:
		open, human = true, true
	case AskHandedBack:
		human = true
	case AskEdited:
		if a := e.Approval; a != nil && a.Version != nil && a.RequestedVersion != nil && *a.RequestedVersion < *a.Version {
			human = false
		}
	case AskAnswered, AskResolved:
		open = false
	}
	switch e.State {
	case ReportsOpen:
		open = true
	case ReportsClosed:
		open = false
	}
	if open && !next.Open {
		human = true
	}
	if e.AskedOfSet {
		next.AskedOf = e.AskedOf
	}
	next.Open, next.Human = open, human
	return next, true
}

// ApprovalState is the approvals signal's state as the events seen so far leave it: each ask, the
// assignee of each issue, and the project's default assignee. The zero value is a state with none
// of them. A caller applies each event as it comes (ApplyAsk, Assign, SetDefaultAssignee) in place
// of folding its history again at each pass.
type ApprovalState struct {
	asks            map[string]Ask
	assignees       map[string]string
	defaultAssignee string
}

// ApplyAsk applies an event of an ask, as NextAsk turns it, and says whether it changed the ask.
func (s *ApprovalState) ApplyAsk(e AskEvent) bool {
	prev, known := s.asks[e.Ask]
	next, changed := NextAsk(prev, known, e)
	if !changed {
		return false
	}
	if s.asks == nil {
		s.asks = make(map[string]Ask)
	}
	s.asks[e.Ask] = next
	return true
}

// Assign sets the assignee of issue, "" for none: an issue event that names an assignee, null
// included.
func (s *ApprovalState) Assign(issue, person string) {
	if s.assignees == nil {
		s.assignees = make(map[string]string)
	}
	s.assignees[issue] = person
}

// SetDefaultAssignee sets the project's default assignee, "" for none.
func (s *ApprovalState) SetDefaultAssignee(person string) { s.defaultAssignee = person }

// Waiting is the id of each ask open that waits on a person who is the project's default assignee
// (WaitsOnDefault), sorted: the asks the approvals signal counts.
func (s *ApprovalState) Waiting() []string {
	var waiting []string
	for _, id := range slices.Sorted(maps.Keys(s.asks)) {
		ask := s.asks[id]
		if ask.Waits() && WaitsOnDefault(ask.AskedOf, s.assignees[ask.Issue], s.defaultAssignee) {
			waiting = append(waiting, id)
		}
	}
	return waiting
}
