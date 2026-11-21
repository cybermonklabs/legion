package grapherase

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// ErasedRows is one table the erase emptied of a node's rows.
type ErasedRows struct {
	Table string `json:"table"`
	Rows  int64  `json:"rows"`
}

// ErasedVersion is one version of the node's own document whose markdown held the erased text,
// named as a reader addresses it. It carries no markdown: the record of an erase holds no erased
// text.
type ErasedVersion struct {
	Document string `json:"document"`
	Issue    string `json:"issue,omitempty"`
	Project  string `json:"project,omitempty"`
	Version  int    `json:"version"`
}

// ErasedReview is one approval whose content_sha256 covered the erased bytes. It cannot be
// rewritten - artifact_review_content_sha256_update raises on any statement that moves a stored
// digest - so the erase ends it, and names it by its document and its version.
type ErasedReview struct {
	ID       string `json:"id"`
	Document string `json:"document"`
	Issue    string `json:"issue,omitempty"`
	Version  int    `json:"version"`
	State    string `json:"state"`
}

// ErasedDocument is one document whose stored history the erase replaces - the node's own, or
// one whose margin record holds an erased comment's body - named as a reader addresses it, with
// what replacing it costs. It carries no text.
type ErasedDocument struct {
	Document string `json:"document"`
	Issue    string `json:"issue,omitempty"`
	Project  string `json:"project,omitempty"`
	// Updates, Checkpoints and Snapshots are the rows of the history the one seed replaces, which
	// a run reads without loading the document, so a run without --confirm lists them too.
	Updates     int64 `json:"updates"`
	Checkpoints int64 `json:"checkpoints"`
	Snapshots   int64 `json:"snapshots"`
	// Read is whether the run loaded the live state, which only a confirmed run does: loading it
	// takes the document's owner row and its room lock (docs.ReadErasedLiveText), and a run
	// without --confirm takes no lock. The fields below are read with it.
	Read bool `json:"-"`
	// Held is whether the erased block's own text stood in its live text, which then reads as
	// the marker. A document whose live text held none - a version of it held the text, and an
	// edit took it out since, or the body erased stood in its margin and never in its text - has
	// its history replaced alone, since the items that edit deleted, and the margin records, can
	// still hold the bytes there.
	Held bool `json:"live_text_held"`
	// Left is whether its live text holds the erased bytes where its markdown spells them
	// otherwise than the erased text is written - an escaped `*` or `_`, for one - so no marker
	// can stand where they are. Such a document refuses the erase (Unredacted).
	Left bool `json:"live_bytes_left"`
	// AnchorMarks and MarginRecords are what the seed takes out of the live document
	// (docs.ErasedLiveText): the anchor marks inside the erased block, and the erased comment's
	// margin records. Every other mark and record stands.
	AnchorMarks   int `json:"anchor_marks"`
	MarginRecords int `json:"margin_records"`
	artifact      string
	// scope is what the seed takes out of this document's live state: the block of it redacted,
	// empty where the run redacts none of its text, and the erased comment's margin entries
	// (docs.ReadErasedLiveText).
	scope docs.ErasedScope
	live  *docs.ErasedLiveText
}

// Operator is who ran the erase: the database role the credential is, and the account and host
// the command ran from. Only a person holding the deployment's database credential runs it, so
// this is the whole of who that was.
type Operator struct {
	DatabaseUser string `json:"database_user"`
	SystemUser   string `json:"system_user,omitempty"`
	Host         string `json:"host,omitempty"`
}

// NotRecounted is one project of Erasure.Recounted whose count after the commit failed, with the
// error that ended it (recountTermStats).
type NotRecounted struct {
	Project string
	Error   string
}

// Erasure is one run of `envoy-dispatch erase`: the node it addressed, every row it would
// delete, rewrite, redact or leave (Listing), and - once Applied - what it did.
type Erasure struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Supertag string `json:"supertag"`
	// Tuples is how many tuples state something out of the node, Inbound how many state
	// something whose far end it is.
	Tuples  int64 `json:"tuples"`
	Inbound int64 `json:"inbound_tuples"`
	// Rows are the rows the run deleted, by table.
	Rows []ErasedRows `json:"rows"`
	// RedactedRows are the rows that derive from the node and that the run rewrote where it may
	// take no row away: an excerpt of a block holding it, the append-only event log, the one
	// stored update its document's replaced history is seeded with, and the search vectors and
	// embedding rows written again from the latest version left. Each that held the bytes holds
	// the marker where they were.
	RedactedRows []ErasedRows `json:"redacted"`
	// Versions and Reviews are the versions of the node's own document whose markdown holds the
	// erased text and the approvals whose digest covered those bytes.
	Versions []ErasedVersion `json:"versions"`
	Reviews  []ErasedReview  `json:"reviews"`
	// Documents is the node's own document when its stored history is replaced.
	Documents []ErasedDocument `json:"documents"`
	// Recounted are the projects whose term statistics counted words of the items the run
	// deleted or rewrote: under its locks the run took the lexemes it removed out of them
	// (api.ForgetErasedTermsTx), and after its commit it counts each again in full, outside its
	// locks (recountTermStats). The record is written before that count, so it holds the
	// projects and not how their count went.
	Recounted []string `json:"recounted"`
	// NotRecounted are the projects of Recounted whose count after the commit failed. Their
	// statistics hold none of the erased words and lack the counts of the lexemes the run took
	// out that other items still hold, until the project's next refresh pass, which counts it in
	// full. It is not recorded, for the reason above.
	NotRecounted []NotRecounted `json:"-"`
	// Digest is the SHA-256 of Listing. A run without --confirm prints it, and a run with
	// --confirm erases only when the listing it reads under its own locks has the digest it was
	// given (Mismatch), so what an operator read is what goes.
	Digest string `json:"digest"`
	// Listing is every row the run would delete, rewrite, redact or leave, one line each, in the
	// order Write prints them. It is not recorded: its last section names other nodes' rows, and
	// the record of an erase is of what it removed.
	Listing []string `json:"-"`
	// LeftAfterCommit are the rows the search after the commit found that hold the erased bytes
	// and derive from another node, and that the search before the locks did not list
	// (searchLeftAfterCommit): rows written after that search, which the run left as it leaves
	// every row it lists. Each is a line as the listing's section of rows left gives it. The
	// digest does not cover them: the listing was confirmed before they were found. Like Listing
	// it is not recorded - the record of an erase in graph_erasures holds no listing - so these
	// rows reach the report alone, as the rows the listing leaves do.
	LeftAfterCommit []string `json:"-"`
	// searchedAfterCommit is whether the search after the commit ended, and so whether
	// LeftAfterCommit is every row it found: a search that failed found none the report can name.
	searchedAfterCommit bool
	// Confirmed is the digest --confirm named, and empty for a run without it.
	Confirmed string `json:"-"`
	// Blockers are the rows of other nodes that state something about this node: a reply that
	// points at an erased comment, a block another block sits in or cites, a comment its author
	// is. Each refuses the run, because deleting them would remove somebody else's words, and
	// leaving them would leave rows stating a fact about a node no longer there, which the store
	// census reads as a difference. They are on the listing too.
	Blockers []string `json:"-"`
	// Applied is whether this run removed anything.
	Applied bool      `json:"-"`
	RanBy   Operator  `json:"-"`
	RanAt   time.Time `json:"-"`
	// Swept is how many display trees the sweep after the commit deleted (sweepTrees): each is
	// one a read that began before the commit stored back inside the window it waits out.
	Swept int64 `json:"-"`
	// sums are the keys of the display trees the run deleted (treeSums). They are held here and
	// nowhere else - no row, no record, no log - since each is the digest of an erased text, and
	// they go with the process once the sweep has read them.
	sums [][]byte
}

// Mismatch reports whether a confirmed run read a listing other than the one its --confirm
// digest names: a row it would delete, rewrite, redact or leave changed since the run that
// printed that digest, so it removes nothing.
func (e *Erasure) Mismatch() bool { return e.Confirmed != "" && e.Confirmed != e.Digest }

// Unredacted names every document whose live text holds the erased bytes where its markdown
// spells them otherwise (ErasedDocument.Left). The erase refuses while one is listed: the seed of
// its history would hold those bytes, and a run that reported success over them would not be
// an erase. An edit that takes them out of the document is what lets the erase run.
func (e *Erasure) Unredacted() []string {
	var documents []string
	for _, document := range e.Documents {
		if document.Left {
			documents = append(documents, document.Document)
		}
	}
	return documents
}

// removed adds one table's deleted rows to the report, keeping one line per table.
func (e *Erasure) removed(table string, rows int64) {
	e.Rows = countedRows(e.Rows, table, rows)
}

// redacted adds one table's rewritten rows to the report, counted as removed counts a deleted
// one, so a run says what it rewrote as well as what it emptied.
func (e *Erasure) redacted(table string, rows int64) {
	e.RedactedRows = countedRows(e.RedactedRows, table, rows)
}

// countedRows adds one table's rows to a count of them, keeping one line per table.
func countedRows(counted []ErasedRows, table string, rows int64) []ErasedRows {
	if rows == 0 {
		return counted
	}
	for i := range counted {
		if counted[i].Table == table {
			counted[i].Rows += rows
			return counted
		}
	}
	return append(counted, ErasedRows{Table: table, Rows: rows})
}

// leftAfterCommitHead is the head of the report's section of the rows the search after the commit
// found (Erasure.LeftAfterCommit), in the words of the listing's section of rows left.
const leftAfterCommitHead = "rows that hold the erased bytes and derive from another node, written after the search and found after the commit, left as they are"

// Write prints the listing with its digest, so an operator reads every row before confirming
// it, and then what a confirmed run read and did: each document whose live state it loaded, and
// once Applied every table it emptied or rewrote and every row its search after the commit found
// that its listing does not name.
func (e *Erasure) Write(w io.Writer) {
	verb := "would remove"
	switch {
	case e.Applied:
		verb = "removed"
	case e.Confirmed != "":
		verb = "refused to remove"
	}
	fmt.Fprintf(w, "erase %s:%s (supertag %s), %s by %s at %s\n",
		e.Kind, e.ID, e.Supertag, verb, e.RanBy.DatabaseUser, e.RanAt.Format(time.RFC3339))
	fmt.Fprintf(w, "  listing digest: %s\n", e.Digest)
	for _, line := range e.Listing {
		fmt.Fprintln(w, line)
	}
	if e.Mismatch() {
		fmt.Fprintf(w, "  --confirm named %s, which is not this listing's digest %s\n", e.Confirmed, e.Digest)
	}
	for _, document := range e.Documents {
		if !document.Read {
			continue
		}
		if document.Left {
			fmt.Fprintf(w, "  live document %s%s: not redacted: its live text holds the erased bytes where its markdown spells "+
				"them otherwise than the erased text is written - an escaped * or _, for one - so no marker can stand "+
				"where they are; the erase refuses until an edit takes them out of the document\n",
				document.Document, owner(document.Issue, document.Project))
			continue
		}
		text := "its live text holds none of the erased text"
		if document.Held {
			text = "its live text is redacted, the marker standing where the erased text was"
		}
		fmt.Fprintf(w, "  live document %s%s: %s; %d anchor mark(s) and %d margin record(s) go with its history\n",
			document.Document, owner(document.Issue, document.Project), text, document.AnchorMarks, document.MarginRecords)
	}
	if !e.Applied {
		return
	}
	for _, rows := range e.Rows {
		fmt.Fprintf(w, "  rows: %s %d\n", rows.Table, rows.Rows)
	}
	for _, rows := range e.RedactedRows {
		fmt.Fprintf(w, "  redacted: %s %d\n", rows.Table, rows.Rows)
	}
	if len(e.Recounted) > 0 {
		fmt.Fprintf(w, "  term statistics: the erased words taken out of %s under the locks\n", strings.Join(e.Recounted, ", "))
	}
	var counted []string
	for _, project := range e.Recounted {
		if !slices.ContainsFunc(e.NotRecounted, func(failed NotRecounted) bool { return failed.Project == project }) {
			counted = append(counted, project)
		}
	}
	if len(counted) > 0 {
		fmt.Fprintf(w, "  term statistics counted again after the commit: %s\n", strings.Join(counted, ", "))
	}
	for _, failed := range e.NotRecounted {
		fmt.Fprintf(w, "  term statistics of %s not counted again after the commit (%s): they hold none of the erased "+
			"words, and lack the counts of the words taken out that other items still hold until the next refresh "+
			"pass, which counts %s in full\n", failed.Project, failed.Error, failed.Project)
	}
	if e.searchedAfterCommit {
		fmt.Fprintf(w, "  %s: %d\n", leftAfterCommitHead, len(e.LeftAfterCommit))
		for _, line := range e.LeftAfterCommit {
			fmt.Fprintln(w, "    "+line)
		}
	} else {
		fmt.Fprintf(w, "  %s: not known, the search after the commit failed\n", leftAfterCommitHead)
	}
	fmt.Fprintf(w, "  text_trees swept after the commit: %d\n", e.Swept)
}

// owner is " of " the issue a document belongs to, or else its project, for a line that names
// the document.
func owner(issue, project string) string {
	if issue != "" {
		return " of " + issue
	}
	if project != "" {
		return " of " + project
	}
	return ""
}
