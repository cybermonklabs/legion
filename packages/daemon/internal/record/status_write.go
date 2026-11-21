package record

// A Dispatch status write is the one outbox payload whose retry is judged differently for each
// thing a write can be for, so what it is for is a value of its own (StatusRowKind), stored in
// the row and set by one constructor per kind. They live here rather than beside the other
// payloads because they are a vocabulary in their own right: five kinds, five constructors and
// the rule that every reader of the list is held to the same five.

// StatusRowKind is what a status row is for, and the one thing that decides the rule the outbox
// judges its retry by (the daemon's stillWanted). It is stored in the payload rather than worked
// out from the row's other fields: while the kind was derived from a set of marks, a kind nobody
// had given a mark read as the ordinary one and was judged by the ordinary rule in silence, so a
// row queued at a status out of the workflow — a re-assert of the daemon's own status over a
// session's close, a person's handover, the daemon's own close — was dropped for standing at the
// very status it exists to undo. Every kind is named once, in StatusRowKinds, which the daemon's
// rule switch and the tests of both packages read, so a kind added to that list and nowhere else
// fails its row and reddens the package rather than borrowing another kind's rule.
type StatusRowKind string

const (
	// OrdinaryStatusRow is the workflow moving an issue along inside the workflow: the phase
	// statuses. The done a merge or an architect's close writes is a CloseStatusRow.
	OrdinaryStatusRow StatusRowKind = "ordinary"
	// PersonHandoverStatusRow is a person's handover: a person's legion label on an unrecorded
	// root in triage, or a person's approval of a parked labelled root's primary document. It is
	// queued at the triage, backlog or icebox that act found, to bring the issue into the
	// workflow. The pending-row rule (Store.PersonHandoverPending) reads the kind to tell such a
	// row from the workflow's other status rows, which suppress nothing: while one is pending for
	// at delivery when the re-read issue no longer carries the label: a person who took it off
	// withdrew the handover.
	PersonHandoverStatusRow StatusRowKind = "person-handover"
	// ReassertStatusRow is the daemon setting its own status back over one a session wrote
	// (workflow's sessionStatusWrite): the session's close or park stands in Dispatch and the
	// record kept the daemon's status, so this row is queued observing a status the issue is not
	// meant to be at — done, backlog or icebox — exactly as a person's handover is. Asking the
	// ordinary rule of it, whether the issue is still in the workflow, would drop the row for
	// being at the status it exists to undo.
	ReassertStatusRow StatusRowKind = "reassert"
	// CloseStatusRow is the daemon's own close of an issue: the done a merge or an architect's
	// close_root writes, queued in the transaction that takes the record out of the workflow. The
	// ordinary rule cannot judge it, for the reason the re-assert's cannot: one refusal — an
	// it to its retry, which would find the record already out of the workflow and drop the write
	// that puts Dispatch there, leaving the issue open in Dispatch for good while the record stands
	// closed and frees its slot. Its own rule asks the question that matters instead: the close
	// still stands in the record at the generation the row names.
	CloseStatusRow StatusRowKind = "close"
)

// StatusRowKinds is every kind of status row this release queues, listed once. The daemon's rule
// switch and the tests of both packages read this list, so a kind is added in one place and every
// reader of it is held to the same four. The release before also queued "hand-over" rows, whose
// executor is gone with the hand-over; the first boot's conversion finishes them.
var StatusRowKinds = []StatusRowKind{OrdinaryStatusRow, PersonHandoverStatusRow, ReassertStatusRow, CloseStatusRow}

// StatusWrite is a Dispatch status the daemon writes. ObservedStatus is the status Dispatch shows
// until the write runs: the runner writes Status only while Dispatch still shows ObservedStatus, so a
// move someone else made since the write was queued stands. Kind is what the write is for, set by
// the constructor its queueing site goes through — OrdinaryStatusWrite, PersonHandoverStatusWrite,
// ReassertStatusWrite or CloseStatusWrite — which are the only places one is built, so no row
// reaches the outbox without a kind (validateOutboxPayload refuses one that does).
type StatusWrite struct {
	Kind           StatusRowKind `json:"kind"`
	Status         string        `json:"status"`
	ObservedStatus string        `json:"observedStatus"`
	// Reason is posted as a message on the issue before the runner writes the status, so it is on
	// the issue before the status lands: a done write's reason, and the reason of a workflow move
	// nothing else on Dispatch explains (workflow's moveReason: a worker's backward move). Empty
	// for a write with no reason to record.
	Reason string `json:"reason,omitempty"`
	// CloseGeneration fences the daemon's own close (CloseStatusRow) on the run it closed: every
	// attempt of the row writes done only while the record still holds that generation, so a
	// person's reopen, which re-admits the issue in a generation of its own, drops a close that
	// has not landed rather than closing the issue the person just took back. Zero in a row no generation is
	// known for — one migration 20261102003600 gave its kind, queued by a release that fenced
	// such a write on nothing — which writes as that release wrote it.
	CloseGeneration uint64 `json:"closeGeneration,omitempty"`
	// Generation and Entry are the issue's run and phase entry the write was queued at (Fence),
	// which the outbox checks on the record before it writes: a write of an earlier one is not
	// written (R20 of dispatch://INFRA-3320). CloseGeneration above is the release before's fence
	// of a close, which it requires and this release keeps writing.
	Generation uint64 `json:"generation,omitempty"`
	Entry      int64  `json:"entry,omitempty"`
	// IncludeChildren, UnlinkChildren and IncludeParent are the options of Dispatch's
	// status-change route the write carries (dispatch.StatusChange, R24 of dispatch://INFRA-3320):
	// the same status on each open issue under the issue, each open child unlinked, each ancestor
	// not in work moved to in_progress. A row with any of them is written on that route under a
	// request key of its own attempt (decision 6 of dispatch://INFRA-4730, the daemon's
	// changeStatus), and a row with none on the issue PATCH, which takes none of them.
	IncludeChildren bool `json:"includeChildren,omitempty"`
	UnlinkChildren  bool `json:"unlinkChildren,omitempty"`
	IncludeParent   bool `json:"includeParent,omitempty"`
	// After is the outbox row whose write this one waits for: a child's write whose parent is
	// written in the same release wave waits until the parent's row has finished (decision 5).
	// Zero waits for none.
	After int64 `json:"after,omitempty"`
	// Controller says the controller asked for the write (its set_status), so a refusal the
	// record undoes is told to the project's controller rather than to the issue's architect
	// (decision 12 of dispatch://INFRA-4730).
	Controller bool `json:"controller,omitempty"`
}

// HasOption says whether the write carries an option of the status-change route, the one route
// that takes them (StatusWrite.IncludeChildren).
func (w StatusWrite) HasOption() bool {
	return w.IncludeChildren || w.UnlinkChildren || w.IncludeParent
}

func (StatusWrite) OutboxKind() OutboxKind { return OutboxKindDispatchStatus }

// OrdinaryStatusWrite is the workflow's own move of an issue inside the workflow, to status from
// the observed status Dispatch shows until the write runs. reason is posted as a message before
// the status lands (StatusWrite.Reason): a done write's, or a move's that no worker's act on
// Dispatch explains; every other write has none.
func OrdinaryStatusWrite(status, observed, reason string) StatusWrite {
	return StatusWrite{Kind: OrdinaryStatusRow, Status: status, ObservedStatus: observed, Reason: reason}
}

// PersonHandoverStatusWrite is the write a person's handover queues, observed at the status that
// person's act found (PersonHandoverStatusRow).
func PersonHandoverStatusWrite(status, observed string) StatusWrite {
	return StatusWrite{Kind: PersonHandoverStatusRow, Status: status, ObservedStatus: observed}
}

// ReassertStatusWrite is the daemon's own status set back over the one a session wrote, which the
// row observes (ReassertStatusRow).
func ReassertStatusWrite(status, observed string) StatusWrite {
	return StatusWrite{Kind: ReassertStatusRow, Status: status, ObservedStatus: observed}
}

// CloseStatusWrite is the daemon's own close of an issue, to status (done) from the observed
// status Dispatch shows, with reason posted as a message before the status lands, fenced on the
// generation of the run the close ended (CloseStatusRow).
func CloseStatusWrite(status, observed, reason string, generation uint64) StatusWrite {
	return StatusWrite{Kind: CloseStatusRow, Status: status, ObservedStatus: observed, Reason: reason,
		CloseGeneration: generation}
}
