package record

import "time"

// MergeJump is the jump mark of one issue: the controller's standing instruction that the daemon
// ask GitHub to put the next merge it submits for this issue at the head of GitHub's merge queue
// (`enqueuePullRequest` with jump, rather than the auto-merge the daemon arms otherwise). An issue
// holds at most one, so the record keeps one row per issue and a second set replaces the first
// (Store.SetMergeJump, which writes only while the issue is still one the daemon runs).
//
// The mark is spent, not standing: the transaction that submits the jumping merge deletes it
// (Store.ClearMergeJump) and reads the deletion's own answer to know the mark was still there when
// it submitted. That is why the mark carries no state of its own beyond who set it and why — a
// mark that is still in the table has not been spent, and one that is gone was spent or taken off.
//
// Reason is what the controller said the jump is for, and Session the controller session that set
// it. Reason is copied into the record of the jump (MergeJumpRecord) when the merge is submitted,
// so the account of what Legion asked GitHub for names the decision after the mark itself is
// gone; Session is named in the message the daemon posts on the issue then and in its
// log line, and the record keeps no column for it. SetAt is when the mark was set, by the
// daemon's clock, kept in UTC as every time this package reads is.
type MergeJump struct {
	Issue   string
	Reason  string
	Session string
	SetAt   time.Time
}

// MergeHold is one hold of a project's merge hold list: while it is live, the daemon submits no
// merge whose pull request changes a file one of Paths matches. It is the controller's way of
// keeping a path still — a release window, a migration landing by hand — without stopping the
// workflow around it: an issue whose merge a hold catches waits on its outbox row and submits when
// the hold ends.
//
// Paths are unanchored regular expressions, matched against the paths the pull request changes, so
// a hold of "packages/daemon/" catches every file under that directory with no pattern language of
// its own. A hold carries several because one decision usually covers several paths, and they
// share the one reason and the one end time.
//
// The list is replaced whole (Store.ReplaceMergeHolds), never edited row by row, which is why a
// hold has no identifier: its place in the list is its order, and the next replacement renumbers
// everything. A controller that wants to drop one hold sends the list without it.
//
// Repository is 'owner/name', and "" is every repository the project serves: a project whose
// issues span repositories can hold a path in one of them, or in all, without naming a list.
// EndsAt is when the hold stops holding; the daemon deletes a hold at or past its end when it
// reads the list (Store.MergeHolds), so an ended hold leaves the record rather than waiting for
// the next replacement. Session and SetAt record who set the list and when, as MergeJump's do.
type MergeHold struct {
	Paths      []string
	Repository string
	Reason     string
	EndsAt     time.Time
	Session    string
	SetAt      time.Time
}

// MergeJumpOutcome is what became of one jump's own request to GitHub, kept in
// MergeJumpRecord.Outcome. Every record carries one of the four from the moment it exists:
// the transaction that spends the mark writes MergeJumpAsked before the mutation is sent, and
// the write that follows GitHub's answer replaces it. There is no value for "nothing recorded",
// because a reader that had to tell an outcome from a field nobody wrote would read the most
// hopeful of the states that field covered.
//
//   - MergeJumpAsked: Legion asked GitHub and has not learned what came of it. It is a state
//     and not a default: a daemon stopped between the mutation and the write after it, and a
//     worker whose lease moved on before that write, both leave it, and it says what is true of
//     them — Legion asked and never learned.
//   - MergeJumpAccepted: the enqueue mutation came back with no error at all, which is GitHub
//     taking the request.
//   - MergeJumpRefused: an answer came back that decides it — GitHub's own GraphQL error, or
//     the 4xx GitHub decided the request itself with.
//   - MergeJumpUnknown: anything that came back and decides nothing — a 5xx of GitHub's own
//     edge, a 2xx carrying no data — and anything that did not come back at all — a dial that
//     failed, a timeout, a connection closed before any status arrived. MergeJumpRecord.Answer
//     tells those apart in its own words, and no reader branches on the difference, because
//     what may be said of either is the one thing: Legion asked, and what GitHub did with the
//     request is not known here.
//
// Acceptance is no queue position. MergeJumpAccepted is the mutation answering and nothing
// more, and where the merge then stands in GitHub's queue is read on GitHub: no reader of this
// record may say where a jump's merge stands, for this value as much as for the other three.
// MergeJumpRecord.Entries is the queue as the daemon read it just before the request and never
// a line of what followed, so it names what the jump was asked to go ahead of and when, which
// is the one thing about the queue the record knows.
type MergeJumpOutcome string

const (
	MergeJumpAsked    MergeJumpOutcome = "asked"
	MergeJumpAccepted MergeJumpOutcome = "accepted"
	MergeJumpRefused  MergeJumpOutcome = "refused"
	MergeJumpUnknown  MergeJumpOutcome = "unknown"
)

// MergeJumpOutcomes is the four outcomes, in the order a jump moves through them. The column's
// check constraint, every reader that renders all four and the tests that write each one read
// this list, so a fifth value is added in one place.
var MergeJumpOutcomes = []MergeJumpOutcome{MergeJumpAsked, MergeJumpAccepted, MergeJumpRefused, MergeJumpUnknown}

// MergeJumpRecord is the account of the newest jump of one repository: the jump the daemon asked
// GitHub for when it spent a jump mark, and what the queue looked like at that moment. The record
// keeps one per (project, repository), because the question it answers is "what did Legion last
// ask this repository's queue to let through" — a per-issue history would grow without bound and
// answer nobody.
//
// It is the request and not the merge. The daemon writes it in the transaction that spends the
// mark, which commits before GitHub's enqueue is sent and long before GitHub answers it (the
// daemon's readyMergeMarks.Jump): the spend and its account are one decision under the merge
// row's lease. So a record says Legion asked for this jump, never that the pull request stands at
// the head of the queue, and a reader that needs GitHub's own answer reads the queue on GitHub.
//
// Issue, Number and Head name the pull request the jump was asked for, At is when the daemon spent
// the mark and asked, and Reason is the reason the mark carried (MergeJump.Reason). Entries is the
// queue as it stood just before the request, oldest entry first, which is what tells a reader
// whose merge the jump was to go ahead of; EntriesTotal is how long the queue was, since Entries
// may hold only as much of it as the daemon read, and EntriesUnknown says GitHub answered no queue
// at all, so an empty Entries is not mistaken for an empty queue.
//
// Outcome is what became of that request and Answer the words that came back with it
// (MergeJumpOutcome): the transaction that spends the mark writes MergeJumpAsked, and the one
// write that follows GitHub's answer to that mutation (Store.SetMergeJumpOutcome) replaces it
// with what happened. Answer carries GitHub's own status and words, or the transport failure's
// own text, for MergeJumpRefused and MergeJumpUnknown, and is empty for MergeJumpAsked and
// MergeJumpAccepted, which have nothing to quote.
//
// The mark is spent whichever way the jump ends, so without those two a jump that did not take
// would stand as the request alone left it, and one that did would be indistinguishable from it.
//
// The record's Issue is not bound to the issue record. An issue is deleted when its tree is
// cleaned up, and the account of what a jump asked to go ahead of must outlive it, so the column
// carries the key as text with no foreign key (the migration's own note says so).
type MergeJumpRecord struct {
	Repository     string
	Issue          string
	Number         int
	Head           string
	At             time.Time
	Reason         string
	Entries        []MergeQueueEntryRecord
	EntriesTotal   int
	EntriesUnknown bool
	Outcome        MergeJumpOutcome
	Answer         string
}

// MergeQueueEntryRecord is one entry of GitHub's merge queue as it stood before a jump, kept in
// MergeJumpRecord.Entries. Number is the pull request's number, Position its place in the queue as
// the daemon read it, Jump whether that entry itself jumped the queue, State GitHub's own state of
// the entry (QUEUED, MERGEABLE, UNMERGEABLE and the rest, in GitHub's spelling, so a state this
// daemon does not know is still recorded), and EnqueuedAt when GitHub says it joined the queue.
//
// It is stored as a JSON array in one jsonb column rather than a table of its own: nothing queries
// one entry, the snapshot is written once with the record and read whole with it. The json tags
// are the field names the API and the plugin surface use, so the stored snapshot and what a reader
// is shown spell the entry the same way.
type MergeQueueEntryRecord struct {
	Number     int       `json:"number"`
	Position   int       `json:"position"`
	Jump       bool      `json:"jump"`
	State      string    `json:"state"`
	EnqueuedAt time.Time `json:"enqueuedAt"`
}

// MergeHeldIssue is one issue whose merge a hold is keeping back: its ready_merge outbox row
// carries the moment the hold first caught it (the payload's heldSince), and DueAt is when that
// row runs again (the row's own next_at), which is when the daemon looks at the holds once more.
// It is what a reader is shown of a held merge — the issue and when it will be tried again — and
// nothing else of the row, since the row's payload is the submission's own business.
type MergeHeldIssue struct {
	Issue string
	DueAt time.Time
}
