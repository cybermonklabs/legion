package record

import "time"

// MergeJump is the jump mark of one issue: the controller's standing instruction that the next
// merge the daemon submits for this issue goes to the head of GitHub's merge queue
// (`enqueuePullRequest` with jump, rather than the auto-merge the daemon arms otherwise). An issue
// holds at most one, so the record keeps one row per issue and a second set replaces the first
// (Store.SetMergeJump).
//
// The mark is spent, not standing: the transaction that submits the jumping merge deletes it
// (Store.ClearMergeJump) and reads the deletion's own answer to know the mark was still there when
// it submitted. That is why the mark carries no state of its own beyond who set it and why — a
// mark that is still in the table has not been spent, and one that is gone was spent or taken off.
//
// Reason is what the controller said the jump is for, and Session the controller session that set
// it. Reason is copied into the record of the jump (MergeJumpRecord) when the merge is submitted,
// so the account of a pull request that went ahead of the queue names the decision after the mark
// itself is gone; Session is named in the message the daemon posts on the issue then and in its
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

// MergeJumpRecord is the account of the newest jump of one repository: what the daemon did when it
// spent a jump mark, and what the queue looked like at that moment. The record keeps one per
// (project, repository), because the question it answers is "what jumped this repository's queue
// last" — a per-issue history would grow without bound and answer nobody.
//
// Issue, Number and Head name the pull request that jumped, At is when the daemon submitted it and
// Reason is the reason the mark carried (MergeJump.Reason). Entries is the queue as it stood
// before the jump, oldest entry first, which is what tells a reader whose merge the jump displaced;
// EntriesTotal is how long the queue was, since Entries may hold only as much of it as the daemon
// read, and EntriesUnknown says GitHub answered no queue at all, so an empty Entries is not
// mistaken for an empty queue.
//
// Refused and Refusal carry a jump that did not go through: the mark is spent either way, and
// without them such a jump would read as a merge that went ahead of the queue. Refusal is empty
// where nothing was refused, and otherwise says which of the two happened: GitHub's own words
// where GitHub answered the mutation, and a sentence naming the failure to deliver where GitHub
// answered nothing at all (daemon's jumpRefusalRecorded). The record keeps no third column for
// that, because the words are what a reader of the record reads.
//
// The record's Issue is not bound to the issue record. An issue is deleted when its tree is
// cleaned up, and the account of what a jump displaced must outlive it, so the column carries the
// key as text with no foreign key (the migration's own note says so).
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
	Refused        bool
	Refusal        string
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
