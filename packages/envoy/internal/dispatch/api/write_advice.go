package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

type adviceAsk struct {
	ID       string `json:"id"`
	Question string `json:"question"`
}

// adviceDeclinedAsk is one ask of the issue a person declined to answer: its id, the head of the
// question that was asked, and the head of the person's own note, "" where they wrote none. Both
// texts are cut to what the one reader of them prints (declinedAskHeadUnits), so the field costs
// the write no more than the line it becomes.
//
// A type of its own rather than a third field on adviceAsk, which is the item of
// your_open_asks: that shape is published as its own schema (AdviceAsk) and read as a pair by
// the client, and no open ask ever carries a note, so a note on it would be a field every open
// ask serialises empty. The four sentences Dispatch writes beside the note are the same for
// every decline (model.DeclineDispatchWords), so they are the reader's to print once, not
// something to repeat per item.
type adviceDeclinedAsk struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	Note     string `json:"note"`
}

// The routes of the two writes whose author may be about to put a question to a person: the ask
// route, and a document edit, whose ops can add the ask blocks that settle into asks. Each names
// itself in its own writeAdvice call, which is where the log line of an omitted advice query
// gets its route, and routeEditDocument names the same write to supersededApprovalAdvice.
const (
	routeOpenAsk      = "POST /api/v1/issues/{key}/asks"
	routeEditDocument = "POST /api/v1/artifacts/{id}/edits"
)

type writeAdvice struct {
	IssueStatus             string      `json:"issue_status"`
	SessionWritesSinceHuman int         `json:"session_writes_since_human"`
	YourOpenAsks            []adviceAsk `json:"your_open_asks"`
	// LegionManagedStatus is whether the Legion daemon owns the issue's status (the issue, or
	// an issue above it, carries the legion label). A pointer, omitted when nil, because it is
	// resolved only where a line depends on it - a session's write on an issue in triage, the
	// one case where the client has advice to drop - and a plain bool would serialise false
	// everywhere else and state, wrongly, that Legion does not own the status. Absent means
	// "not asked", which is what @legion/contracts' optional WriteAdvice.legion_managed_status
	// already expresses.
	LegionManagedStatus *bool `json:"legion_managed_status,omitempty"`
	// documentBlocks is set by a document write whose document reads; nil omits its fields.
	*documentBlocks
	// editBlocks is set by a document edit; nil omits its field.
	*editBlocks
	// SupersededApproval is set by a document write that stored a version replacing the one a
	// person had approved; nil (omitted) everywhere else.
	SupersededApproval *supersededApproval `json:"superseded_approval,omitempty"`
	// MovedApprovalRequest is set by a document write that took an open approval request back
	// from the person it waits on; nil (omitted) everywhere else.
	MovedApprovalRequest *movedApprovalRequest `json:"moved_approval_request,omitempty"`
	// DeclinedAsks is each ask of this issue a person declined to answer, so a session about to
	// ask does not ask one of them again. Set only on a write that opens a question (the
	// opensAQuestion its caller passes writeAdvice) and omitted when empty, so a write with
	// nothing to report and a write that was never asked the question look the same to a
	// reader: both have no declined ask to name.
	DeclinedAsks []adviceDeclinedAsk `json:"declined_asks,omitempty"`
	// Suggestions is the related-item check's result, set only on an issue or ask
	// creation; nil (omitted) everywhere else.
	Suggestions *model.Suggestions `json:"suggestions,omitempty"`
}

// suggestionsOnlyAdvice is the advice of a write with no issue state to report: an ask on an
// unlinked project document, which has no issue status, no writes-since-human count and no
// issue-scoped open-ask list, or an issue or ask write whose write-advice read failed. It still
// carries the related-item check's result (withSuggestions).
type suggestionsOnlyAdvice struct {
	Suggestions *model.Suggestions `json:"suggestions"`
}

type adviceQueryError struct {
	query string
	err   error
}

func (e *adviceQueryError) Error() string {
	return fmt.Sprintf("%s: %v", e.query, e.err)
}

func (e *adviceQueryError) Unwrap() error {
	return e.err
}

// computeWriteAdvice runs the issue-scoped advice queries for a session's write. Two of them are
// about the calling session itself - its own writes since the last human event, its own open
// asks - so writeAdvice calls this only for a session actor. The third, the asks of the issue a
// person declined to answer, runs only where opensAQuestion says this write is about to put
// another question to a person: the ask route, and an edit of one of the issue's documents that
// adds an ask block. The fourth, whether Legion owns the issue's status, runs only on an issue
// whose status is triage: that is the only line the answer decides, so an ordinary write on an
// ordinary issue gains no query at all. status is the issue's status as the write committed it,
// the same value writeAdvice reports. route names the caller for the two queries whose failure
// is logged rather than returned (declinedAsksAdvice, legionManagedStatusAdvice).
func (s *server) computeWriteAdvice(
	ctx context.Context,
	tx pgx.Tx,
	route string,
	issueKey string,
	actor model.Actor,
	excludeAskID string,
	status string,
	opensAQuestion bool,
) (*writeAdvice, error) {
	advice := &writeAdvice{YourOpenAsks: []adviceAsk{}, IssueStatus: status}
	if err := s.runAdviceQueryHook(ctx, tx, "session_writes_since_human"); err != nil {
		return nil, &adviceQueryError{query: "session_writes_since_human", err: err}
	}
	if err := tx.QueryRow(ctx, `
		with last_human as materialized (
			select coalesce(max(id), 0) as last_id from events
			where issue_key = $1 and actor->>'kind' = 'user'
		)
		select count(*) from events, last_human
		where issue_key = $1 and actor->>'kind' = 'session' and actor->>'id' = $2
		  and type in ('message.created', 'comment.created', 'ask.opened')
		  and id > last_human.last_id
	`, issueKey, actor.ID).Scan(&advice.SessionWritesSinceHuman); err != nil {
		return nil, &adviceQueryError{query: "session_writes_since_human", err: err}
	}
	if err := s.runAdviceQueryHook(ctx, tx, "your_open_asks"); err != nil {
		return nil, &adviceQueryError{query: "your_open_asks", err: err}
	}
	rows, err := tx.Query(ctx, `
		select id::text, question from asks
		where issue_key = $1 and state = 'open'
		  and author->>'kind' = 'session' and author->>'id' = $2
		  and ($3 = '' or id::text <> $3)
		order by created_at, id
		limit 2
	`, issueKey, actor.ID, excludeAskID)
	if err != nil {
		return nil, &adviceQueryError{query: "your_open_asks", err: err}
	}
	defer rows.Close()
	for rows.Next() {
		var ask adviceAsk
		if err := rows.Scan(&ask.ID, &ask.Question); err != nil {
			return nil, &adviceQueryError{query: "your_open_asks", err: err}
		}
		advice.YourOpenAsks = append(advice.YourOpenAsks, ask)
	}
	if err := rows.Err(); err != nil {
		return nil, &adviceQueryError{query: "your_open_asks", err: err}
	}
	// The issue's declined asks, for a write that opens a question. Its failure is not fatal to
	// the advice either (declinedAsksAdvice).
	if opensAQuestion {
		advice.DeclinedAsks = s.declinedAsksAdvice(ctx, tx, route, issueKey)
	}
	if status == model.IssueStatusTriage {
		advice.LegionManagedStatus = s.legionManagedStatusAdvice(ctx, tx, route, issueKey)
	}
	return advice, nil
}

// legionManagedStatusAdvice answers whether Legion owns the issue's status for a write's advice,
// or nil when the walk failed. It is the last step of computeWriteAdvice and, with the declined
// asks read before it, one of the two whose failure the advice survives: the two reads at the
// top are what a session asked for, while this one decides a single line, and every issue is
// created in triage, so returning an adviceQueryError here would put a third failure mode on
// the path that runs for every session-authored `dispatch issue`. What that would cost a
// creation is its issue state alone: the related-item check runs after the write commits
// whatever the advice answered, and rides a suggestions-only advice when it failed
// (withSuggestions). Nothing runs after this step, so the advice already assembled is complete
// without it.
//
// The walk takes a savepoint of its own inside the advice savepoint, because a failed statement
// aborts the transaction it ran in: run directly on the advice savepoint, its failure would also
// take the statement_timeout reset and the release that follow it (writeAdvice), and the advice
// would be dropped anyway. The failure is logged once, in the shape every omitted advice query
// takes. The 500 ms bound writeAdvice set still covers it: SET LOCAL holds across a savepoint
// opened under it.
func (s *server) legionManagedStatusAdvice(ctx context.Context, tx pgx.Tx, route, issueKey string) *bool {
	walk, err := tx.Begin(ctx)
	if err != nil {
		s.logAdviceError(route, issueKey, "legion_managed_status", err)
		return nil
	}
	if err := s.runAdviceQueryHook(ctx, walk, "legion_managed_status"); err != nil {
		_ = walk.Rollback(ctx)
		s.logAdviceError(route, issueKey, "legion_managed_status", err)
		return nil
	}
	managed, err := legionManagedStatus(ctx, walk, issueKey)
	if err != nil {
		_ = walk.Rollback(ctx)
		s.logAdviceError(route, issueKey, "legion_managed_status", err)
		return nil
	}
	if err := walk.Commit(ctx); err != nil {
		s.logAdviceError(route, issueKey, "legion_managed_status", err)
		return nil
	}
	return &managed
}

// declinedAsksAdvice is each ask of issueKey a person declined to answer, for the advice of a
// write that is about to open another question, or nil when the read failed. It names every one
// of them, oldest first, because R6 asks for each declined ask of the issue: a row cap would
// silently drop exactly the question the session is about to put again, which is the one thing
// this field exists to prevent. What bounds the read is the 500 ms statement_timeout the advice
// holds every query of a write to, not a count of rows.
//
// Its failure is not fatal to the advice, and it takes a savepoint of its own inside the advice
// savepoint, both for the reasons legionManagedStatusAdvice gives for its own. What a failure
// costs here is the reminder alone: the two reads before it are what the calling session asked
// about - the write it just made, and its own open asks - while this one only keeps it from
// asking again what a person has already refused to answer.
func (s *server) declinedAsksAdvice(ctx context.Context, tx pgx.Tx, route, issueKey string) []adviceDeclinedAsk {
	read, err := tx.Begin(ctx)
	if err != nil {
		s.logAdviceError(route, issueKey, "declined_asks", err)
		return nil
	}
	declined, err := s.readDeclinedAsks(ctx, read, issueKey)
	if err != nil {
		_ = read.Rollback(ctx)
		s.logAdviceError(route, issueKey, "declined_asks", err)
		return nil
	}
	if err := read.Commit(ctx); err != nil {
		s.logAdviceError(route, issueKey, "declined_asks", err)
		return nil
	}
	return declined
}

// declinedAskHeadUnits is how much of a declined ask's question and of the person's note the
// reminder carries, in UTF-16 units. The one reader of the field, declinedAskLines in
// packages/envoy-client/src/related-check.ts, prints what it was sent and cuts nothing itself;
// this number is held equal to QUESTION_HEAD in packages/envoy-client/src/ask-answer.ts, the
// length every other surface quotes a question at, so the field carries exactly what the line
// shows, and neither side can grow the other's cost by itself. Every declined ask of the issue
// is still named, and the whole note stands at the ask's own link, which the same line prints.
const declinedAskHeadUnits = 200

// declinedAskHead is the server's copy of the client's textHead (packages/envoy-client/src/
// ask-answer.ts), which read the whole text until this read began cutting it: the text on one
// line, runs of whitespace collapsed to a single space and the ends trimmed, cut to
// declinedAskHeadUnits UTF-16 units with an ellipsis where it was cut, so a reader is never
// shown a sentence that stops for no reason.
//
// The count is itemblocks.Units16's, the unit the rest of this package bounds a text in, and
// the walk reads whole runes, so a character whose UTF-16 form is a surrogate pair is carried
// whole or not at all (model.EventBody cuts the same way). A combining sequence can still be
// cut in two, exactly as textHead's own slice cuts one: the two sides agree, and the mark that
// is left behind sits on the ellipsis.
func declinedAskHead(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	units := 0
	for index, r := range flat {
		if units+utf16.RuneLen(r) > declinedAskHeadUnits {
			return flat[:index] + "…"
		}
		units += utf16.RuneLen(r)
	}
	return flat
}

// declinedAsk is the SQL predicate, on asks, that holds for an ask a person declined to answer
// (model.AskResolutionDeclined), as withdrawnAsk is the one for an ask a close withdrew.
const declinedAsk = `state = 'resolved' and resolution->>'kind' = '` + model.AskResolutionDeclined + `'`

// readDeclinedAsks is declinedAsksAdvice's read, apart so its rows are closed before the
// savepoint they were read on is released. Nil, not an empty list, where the issue holds no
// declined ask: the field is omitted rather than sent empty.
//
// The cut is here rather than in the statement because what it must match is the reader, not
// the database: Postgres counts code points where the reader counts UTF-16 units, its
// whitespace classes are not JavaScript's, and the ellipsis would be a CASE over a second
// length call - three near-misses of textHead where Go matches it in one pass. What crosses
// from Postgres whole is a read of rows the query already fetched, inside the 500 ms the
// advice bounds every query to; what the cut saves is the write's own answer, which every
// session on the issue pays for.
func (s *server) readDeclinedAsks(ctx context.Context, tx pgx.Tx, issueKey string) ([]adviceDeclinedAsk, error) {
	if err := s.runAdviceQueryHook(ctx, tx, "declined_asks"); err != nil {
		return nil, err
	}
	// The note is the resolution's reason, which a person who wrote none leaves empty.
	rows, err := tx.Query(ctx, `
		select id::text, question, coalesce(resolution->>'reason', '') from asks
		where issue_key = $1 and `+declinedAsk+`
		order by created_at, id
	`, issueKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var declined []adviceDeclinedAsk
	for rows.Next() {
		var ask adviceDeclinedAsk
		if err := rows.Scan(&ask.ID, &ask.Question, &ask.Note); err != nil {
			return nil, err
		}
		ask.Question = declinedAskHead(ask.Question)
		ask.Note = declinedAskHead(ask.Note)
		declined = append(declined, ask)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return declined, nil
}

func (s *server) runAdviceQueryHook(ctx context.Context, tx pgx.Tx, query string) error {
	if s.adviceQueryHook == nil {
		return nil
	}
	return s.adviceQueryHook(ctx, tx, query)
}

// writeAdvice is the advice of one write, run on a savepoint of the write's own transaction.
// opensAQuestion says the write is about to put a question to a person, which is what the
// issue's declined asks are read for (computeWriteAdvice): the ask route passes true, a
// document edit passes whether its ops added an ask block, and every other write passes false
// and runs no such read.
func (s *server) writeAdvice(
	ctx context.Context,
	tx pgx.Tx,
	route string,
	issueKey string,
	actor model.Actor,
	excludeAskID string,
	status string,
	opensAQuestion bool,
) *writeAdvice {
	// A person's write has no advice query to run - every one of them is about the calling
	// session, and the triage line this answers is about what a session is told - so it is
	// answered here, outside the savepoint and the two statement_timeout statements that would
	// otherwise run inside the write's own transaction while it holds the issue row. Such a
	// write still gets the triage advice, which is true for a reader who may move the status.
	// Two kinds of actor reach a write: a session, which every bearer naming itself becomes
	// (bearerSessionActor), and the person, which a cookie is and which actingPerson hands back
	// for every credential holding the `person` scope, an agent's personal token among them.
	// That agent gets the person's advice and not a session's, which is right: it is acting as
	// the person, and the advice is about what the acting writer is told. The guard is on the
	// kind rather than on "is a human" so an actor kind the server does not produce today cannot
	// silently acquire session-shaped advice.
	if actor.Kind != "session" {
		return &writeAdvice{YourOpenAsks: []adviceAsk{}, IssueStatus: status}
	}
	adviceTx, err := tx.Begin(ctx)
	if err != nil {
		s.logAdviceError(route, issueKey, "savepoint", err)
		return nil
	}
	// Healthy advice queries complete in single-digit milliseconds. Bound them at 500 ms so a
	// future plan regression cannot hold the issue row lock and a pool connection indefinitely.
	if _, err := adviceTx.Exec(ctx, "set local statement_timeout = '500ms'"); err != nil {
		_ = adviceTx.Rollback(ctx)
		s.logAdviceError(route, issueKey, "statement_timeout", err)
		return nil
	}
	advice, err := s.computeWriteAdvice(ctx, adviceTx, route, issueKey, actor, excludeAskID, status, opensAQuestion)
	if err != nil {
		_ = adviceTx.Rollback(ctx)
		query := "unknown"
		if queryErr, ok := err.(*adviceQueryError); ok {
			query = queryErr.query
		}
		s.logAdviceError(route, issueKey, query, err)
		return nil
	}
	// SET LOCAL crosses a released savepoint, so restore the outer transaction's default before
	// releasing this one. An error path rolls back the savepoint and clears the setting.
	if _, err := adviceTx.Exec(ctx, "set local statement_timeout = default"); err != nil {
		_ = adviceTx.Rollback(ctx)
		s.logAdviceError(route, issueKey, "statement_timeout_reset", err)
		return nil
	}
	if err := adviceTx.Commit(ctx); err != nil {
		s.logAdviceError(route, issueKey, "savepoint", err)
		return nil
	}
	return advice
}

func (s *server) logAdviceError(route, issueKey, query string, err error) {
	slog.Warn("dispatch: write advice omitted", "route", route, "issue", issueKey, "query", query, "error", err)
}

// adviceSavepoint opens the savepoint one document report runs behind and bounds its statements,
// or reports false when the savepoint or the bound could not be set, logging that failure under
// field, the report's own name. Each report runs behind one of these rather than sharing a
// savepoint with the report beside it, so a failed read costs its own report and no other.
//
// The caller rolls the savepoint back, in a defer of its own: a report writes nothing, so its
// savepoint is always rolled back rather than released, and that rollback restores the
// statement_timeout this set. Where this answers false it has rolled back already, and there is
// no savepoint for the caller to hold.
func (s *server) adviceSavepoint(ctx context.Context, tx pgx.Tx, route, issueKey, field string) (pgx.Tx, bool) {
	adviceTx, err := tx.Begin(ctx)
	if err != nil {
		s.logAdviceError(route, issueKey, field, err)
		return nil, false
	}
	// Healthy advice queries complete in single-digit milliseconds. Bound them at 500 ms so a
	// future plan regression cannot hold the write's row locks and a pool connection
	// indefinitely.
	if _, err := adviceTx.Exec(ctx, "set local statement_timeout = '500ms'"); err != nil {
		_ = adviceTx.Rollback(ctx)
		s.logAdviceError(route, issueKey, field, err)
		return nil, false
	}
	return adviceTx, true
}

// documentBlocks is what a document write's advice reports about the typed blocks in the document
// it stored: how many ask blocks it holds, and the typed block openings it holds as text.
type documentBlocks struct {
	DecisionBlocks  int              `json:"decision_blocks"`
	UnparsedOpeners *unparsedOpeners `json:"unparsed_openers,omitempty"`
}

// unparsedOpeners is the typed block openings (`:::ask{…}`) a document holds as text rather than
// as blocks, outside code: written inside a line, or escaped. A writer who
// meant a block would otherwise hear only that the document holds none (LEGION-416). A mention of
// the syntax in prose is reported too, and harmlessly: it is never refused.
type unparsedOpeners struct {
	Count    int      `json:"count"`
	Examples []string `json:"examples"`
}

// editBlocks is what a document edit's advice reports about the typed blocks it wrote: how many ask
// blocks the edited document holds that the document before the edit did not
// (docs.EditOutcome.AddedAskBlocks), as the server's parser reads them, and the related-item check
// each of them got (decisionBlockChecks), omitted when the edit added none.
type editBlocks struct {
	DecisionBlocksAdded int                  `json:"decision_blocks_added"`
	DecisionBlockChecks []decisionBlockCheck `json:"decision_block_checks,omitempty"`
}

// decisionBlockCheck is one added decision block's related-item check: the block, its
// question, and the check's result.
type decisionBlockCheck struct {
	BlockID     string             `json:"block_id"`
	Question    string             `json:"question"`
	Suggestions *model.Suggestions `json:"suggestions"`
}

// supersededApproval is what a document write reports when the version it stored replaced the
// version a person had approved: that approval is now stale, and nothing but a person's review of
// the new version restores it. Every write that supersedes an approval is reported, whatever the
// document is: the approval it made stale is a fact about that document and nothing else. The
// writer is the only one who can be told in time, and only the write that superseded the approval
// is told: a write on a document whose approval was already stale closed nothing.
//
// Whether a Legion design gate was among the things that approval held open is what DesignGates
// answers. Dispatch keeps, issue by issue, which document each gated step records (design_gates,
// PUT /api/v1/issues/{key}/design-gates), and the list is that record read for this document,
// kept to the gates that hold a READY back: the issue is in the workflow there, and the project's own
// list of gated steps names the row's step (supersededApprovalGates). Where nothing is left,
// this write closed no gate anyone here knows of.
type supersededApproval struct {
	// ApprovedVersion is the version a person approved, the one this write superseded.
	ApprovedVersion int `json:"approved_version"`
	// Version is the version this write stored.
	Version int `json:"version"`
	// ApprovedBy is who gave that approval, as its review records the actor's id.
	ApprovedBy string `json:"approved_by"`
	// DesignGates are the design gates Dispatch's record says this document is: one entry per
	// issue and step, kept to the issues still in the workflow whose step the project gates.
	// Omitted when empty, and empty is also what a failed gate read leaves
	// (supersededApprovalAdvice), because an absent list and an empty one say the one thing a
	// client acts on - Dispatch names no gate here.
	DesignGates []supersededApprovalGate `json:"design_gates,omitempty"`
}

// supersededApprovalGate is one design gate the written document records: the issue whose gate
// records it and the step it records there, the pair design_gates is keyed by. The document is
// the one the advice is about, so the row names it nowhere.
type supersededApprovalGate struct {
	Issue string `json:"issue"`
	Step  string `json:"step" enum:"decisions,criteria,spec"`
}

// supersededApprovalAdvice reports the approval the write of version just made stale, or nil when
// it made none, which is every write of a document nobody has approved. It runs inside the write's
// own transaction, after the version row and the approval carry (docs.carryApproval) are written,
// so it reads what the write left: a carried approval is pinned to version itself and reports
// nothing, which is right - the gate reopens on that carry's own artifact.approved event.
//
// The read is the newest review of the document (docs.NewestReview), one entry of
// artifact_reviews_by_artifact_event_seq in docs.ReviewOrder, the order the reviews' events
// committed: artifact_reviews.created_at is its transaction's start and its id is random, so the
// carry's own approval of this version can carry an earlier pair than an approval committed while
// this write was open, and ordering by those two alone would report a gate this write did not
// close. It reports only when that newest review approves the version right below this one: an
// older approval was already stale before this write, and a newer changes request had already
// closed the gate.
//
// Like every other advice query this runs behind a savepoint of its own (adviceSavepoint),
// because a failed statement aborts the transaction it ran in and this must never fail the write
// it describes. It writes nothing, so the savepoint is always rolled back, which also restores
// the statement_timeout it set.
func (s *server) supersededApprovalAdvice(
	ctx context.Context,
	tx pgx.Tx,
	route string,
	issueKey string,
	artifactID string,
	version int,
) *supersededApproval {
	adviceTx, ok := s.adviceSavepoint(ctx, tx, route, issueKey, "superseded_approval")
	if !ok {
		return nil
	}
	defer func() { _ = adviceTx.Rollback(ctx) }()
	approval, err := newestReviewAsApproval(ctx, adviceTx, artifactID, version)
	if err != nil {
		s.logAdviceError(route, issueKey, "superseded_approval", err)
		return nil
	}
	if approval == nil {
		// A write that superseded no approval asks no question about gates, so there is no
		// gate read either: the clause the gates qualify is the one this read did not write.
		return nil
	}
	// The gates run on the savepoint the approval read ran on, under the statement_timeout set
	// above: one more statement on a connection the write already holds. A failed or timed-out
	// read of them costs the gate list alone - never the write, which commits as it always
	// does, and never the first sentence, since the approval this write superseded is true
	// whatever Dispatch holds about gates.
	gates, err := supersededApprovalGates(ctx, adviceTx, artifactID)
	if err != nil {
		s.logAdviceError(route, issueKey, "superseded_approval", err)
		return approval
	}
	approval.DesignGates = gates
	return approval
}

// supersededApprovalGates reads Dispatch's record of which issues' design gates record this
// document (design_gates, written by PUT /api/v1/issues/{key}/design-gates), keeping the rows
// that hold a READY back and no others. Two things decide that, and the row alone says neither.
//
// The issue must be in the workflow, which is every status but the four that take it out
// (model.IssueStatusesOutOfWorkflow): a closed tree, and a tree parked in `backlog` or `icebox`,
// refuse no READY, and an issue still in `triage` runs nothing either. Nothing clears a parked
// issue's gate rows - the daemon suspends its claims and deletes none, and its boot republishes
// them for as long as it stays parked - so the rows outlive the tree that ran, and only the
// re-admission that brings it back clears them.
//
// The deployment must gate the row's step, which is the project's own list (projects.gated_steps,
// written by PUT /api/v1/projects/{key}/gated-steps, the publication of the same boot that writes
// these rows). A daemon records a gate row for a step it does not gate - with `gates.design: off`
// the registration records the spec step, approves it, and no later event ever moves it - and
// that gate refuses nothing. A project whose daemon has published no list gates nothing it holds,
// and prints no clause, which is the direction this whole change takes: a reader told nothing
// asks the architect, where a reader told a gate that is not there stops working.
//
// The order is the issue's key and then the order a gate runs the steps in ($2,
// model.GatedStepsKnown), the order the issue read lists an issue's own gates in.
func supersededApprovalGates(ctx context.Context, q docs.Queryer, artifactID string) ([]supersededApprovalGate, error) {
	rows, err := q.Query(ctx, `
		select g.issue, g.step
		from design_gates g
		join issues i on i.key = g.issue
		join projects p on p.key = i.project_key
		where g.artifact_id = $1 and not (i.status = any($3::text[])) and g.step = any(p.gated_steps)
		order by g.issue, array_position($2::text[], g.step)
	`, artifactID, model.GatedStepsKnown, model.IssueStatusesOutOfWorkflow)
	if err != nil {
		return nil, fmt.Errorf("read the design gates this document records: %w", err)
	}
	defer rows.Close()
	var gates []supersededApprovalGate
	for rows.Next() {
		var gate supersededApprovalGate
		if err := rows.Scan(&gate.Issue, &gate.Step); err != nil {
			return nil, err
		}
		gates = append(gates, gate)
	}
	return gates, rows.Err()
}

// newestReviewAsApproval is supersededApprovalAdvice's rule, over whatever queryer the caller
// holds: the document's newest review in docs.ReviewOrder, reported when it approves the version
// right below version. A document with no review at all, an approval of an older version, an
// approval of version itself (the approval carry's own review) and a changes request each report
// nothing.
func newestReviewAsApproval(ctx context.Context, q docs.Queryer, artifactID string, version int) (*supersededApproval, error) {
	review, err := docs.NewestReview(ctx, q, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the document's newest review: %w", err)
	}
	if review == nil || review.State != "approved" || review.Version != version-1 {
		return nil, nil
	}
	return &supersededApproval{
		ApprovedVersion: review.Version,
		Version:         version,
		ApprovedBy:      review.Actor.ID,
	}, nil
}

// movedApprovalRequest is what a document write reports when the version it stored moved an
// open approval request of that document onto its agent's turn (docs.MoveApprovalAsk): the
// person who was asked no longer sees the request in Waiting on you, and nothing but the
// agent's own hand-back puts it back in front of them. The write is the moment that happened
// and its author the only one who can act on it, so its own answer carries the request rather
// than leaving the agent to read the ask list and notice.
//
// Issue and Project are one pair, taken from the request's first document: a document of an
// issue carries the issue, a document of a project carries the project, and no document carries
// both. They are what a reader needs to name the request's own place, which the ask id alone
// does not say.
type movedApprovalRequest struct {
	AskID     string                  `json:"ask_id"`
	Issue     string                  `json:"issue,omitempty"`
	Project   string                  `json:"project,omitempty"`
	Documents []movedApprovalDocument `json:"documents"`
}

// movedApprovalDocument is one document the moved request names: the document as the artifacts
// row has it now - its name, whether it is its owner's primary document, and the part it plays -
// with the two versions the request carries for it. RequestedVersion is the version its agent
// last handed to the human and Version the version the request names now, so the pair is what
// says this document is the one the write moved.
type movedApprovalDocument struct {
	ArtifactID       string `json:"artifact_id"`
	Name             string `json:"name"`
	Primary          bool   `json:"primary"`
	Role             string `json:"role,omitempty"`
	RequestedVersion int    `json:"requested_version"`
	Version          int    `json:"version"`
}

// movedApprovalRequestAdvice reports the open approval request the write just took back from the
// person, or nil when it took none: a document with no open request, and a request still at the
// version its agent handed over, which is what a write whose own carry answered the request
// leaves. It runs inside the write's own transaction, after the version row, the request's move
// (docs.MoveApprovalAsk) and the approval carry, so it reads the state the write left and not
// the state it found.
//
// The rule is docs.ApprovalAwaitsHandBack, the same one the quiet move and the SQL approvalMoved
// (ask_rows.go) apply, asked here of the request as this write left it.
//
// Like every other advice query this runs behind a savepoint of its own (adviceSavepoint),
// because a failed statement aborts the transaction it ran in and this must never fail the write
// it describes. It writes nothing, so the savepoint is always rolled back, which also restores
// the statement_timeout it set. A failure costs the report alone: the write commits as it always
// does, and the agent is left where it is today, with the request unnamed.
func (s *server) movedApprovalRequestAdvice(
	ctx context.Context,
	tx pgx.Tx,
	route string,
	issueKey string,
	artifactID string,
) *movedApprovalRequest {
	adviceTx, ok := s.adviceSavepoint(ctx, tx, route, issueKey, "moved_approval_request")
	if !ok {
		return nil
	}
	defer func() { _ = adviceTx.Rollback(ctx) }()
	ask, err := docs.OpenApprovalAsk(ctx, adviceTx, artifactID)
	if err != nil {
		s.logAdviceError(route, issueKey, "moved_approval_request", err)
		return nil
	}
	if ask == nil || ask.Approval == nil {
		return nil
	}
	if !docs.ApprovalAwaitsHandBack(ask.Approval.Documents) {
		// The request still waits on the person, so this write took nothing back from them.
		return nil
	}
	rows, err := movedApprovalArtifacts(ctx, adviceTx, ask.Approval.Documents)
	if err != nil {
		s.logAdviceError(route, issueKey, "moved_approval_request", err)
		return nil
	}
	moved := &movedApprovalRequest{
		AskID:     ask.ID,
		Documents: make([]movedApprovalDocument, 0, len(ask.Approval.Documents)),
	}
	// The owner is the first document's, because that is the document the request was opened on
	// and the one a hand-back command names. A document whose row this read did not return
	// leaves the request unplaced rather than placing it wrongly.
	if lead, ok := rows[ask.Approval.Documents[0].ArtifactID]; ok {
		if lead.IssueKey != nil {
			moved.Issue = *lead.IssueKey
		} else {
			moved.Project = lead.Project
		}
	}
	for _, document := range ask.Approval.Documents {
		// The request's own order, because the client's hand-back command names its first
		// document, and the request's own record for a document whose row is gone or was not
		// returned: the report never drops a document the request names.
		entry := movedApprovalDocument{
			ArtifactID:       document.ArtifactID,
			Name:             document.Name,
			RequestedVersion: document.RequestedVersion,
			Version:          document.Version,
		}
		if artifact, ok := rows[document.ArtifactID]; ok {
			entry.Name = artifact.Name
			entry.Primary = artifact.Primary
			entry.Role = documentRoleOf(artifact)
		}
		moved.Documents = append(moved.Documents, entry)
	}
	return moved
}

// movedApprovalArtifacts reads the artifacts rows of every document the request names, keyed by
// id: one statement on the savepoint its caller holds, rather than one read per document, since
// a request names one document or two and either way this is a single round trip. A document
// whose row is missing is simply absent from the map, which is what the caller reports it as.
func movedApprovalArtifacts(
	ctx context.Context,
	q docs.Queryer,
	documents []model.AskApprovalDocument,
) (map[string]model.Artifact, error) {
	ids := make([]string, len(documents))
	for index, document := range documents {
		ids[index] = document.ArtifactID
	}
	// The ids are compared as text, as the graph's own artifact read compares them
	// (refs.loadArtifactNodes): an approval's stored id that is not a uuid would fail the cast
	// and cost the whole report, where this read simply returns no row for it.
	rows, err := q.Query(ctx, `
		select `+artifactColumns+`
		from artifacts where id::text = any($1::text[])
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the documents an approval request names: %w", err)
	}
	artifacts, err := collectArtifacts(rows)
	if err != nil {
		return nil, fmt.Errorf("read the documents an approval request names: %w", err)
	}
	byID := make(map[string]model.Artifact, len(artifacts))
	for _, artifact := range artifacts {
		byID[artifact.ID] = artifact
	}
	return byID, nil
}

// documentWriteAdvice is the whole advice of a document write with no issue state to report: a
// project document's write, or an issue document's whose bounded issue-advice read failed. It
// carries the document half of writeAdvice - the block report its route produces, the approval
// the write superseded, and the approval request it took back from a person - so none of them
// depends on the issue advice beside it.
type documentWriteAdvice struct {
	*documentBlocks
	*editBlocks
	// SupersededApproval is the approval this write made stale; nil (omitted) when it made none.
	SupersededApproval *supersededApproval `json:"superseded_approval,omitempty"`
	// MovedApprovalRequest is the open approval request this write took back from the person it
	// waits on; nil (omitted) when it took none.
	MovedApprovalRequest *movedApprovalRequest `json:"moved_approval_request,omitempty"`
}

const (
	// unparsedOpenerExamples is how many openings the advice quotes.
	unparsedOpenerExamples = 3
	// unparsedOpenerContext is how many bytes of the text before an opening its example quotes,
	// and unparsedOpenerSpan how far past its start the example runs looking for its closing brace.
	unparsedOpenerContext = 16
	unparsedOpenerSpan    = 80
)

// typedOpening matches a typed block's opening as text: three or more colons, a name, and a brace.
var typedOpening = regexp.MustCompile(`:{3,}[A-Za-z0-9_-]+\{`)

// readDocumentBlocks reads markdown, a document's canonical markdown - the rendering a write
// stored, read back (pmdoc.ParseRendering) - for its advice, or nil when it does not read. A
// caller whose own write already read that rendering back passes the tree to documentBlocksFor
// instead, which is this parse of the same markdown.
func readDocumentBlocks(markdown string) *documentBlocks {
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		slog.Warn("dispatch: decision block count failed", "error", err)
		return nil
	}
	return documentBlocksOf(tree)
}

// documentBlocksOf reads tree, a document's canonical markdown parsed, for its advice.
func documentBlocksOf(tree *pmdoc.Node) *documentBlocks {
	blocks := &documentBlocks{}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		switch node.Type {
		case "ask":
			blocks.DecisionBlocks++
		case "code_block":
			// Code is text on purpose, and its children are text nodes, whose own visits read
			// nothing, so the walk moves on past it: pmdoc.Walk's false stops the whole
			// traversal, which would leave every block after the code uncounted.
			return true
		}
		blocks.readOpenersAsText(node)
		return true
	})
	return blocks
}

// documentBlocksFor reads, for its advice, the document a write just stored. readBack is the
// rendering that write stored, read back by its own parse (docs' Ledger.ReadBackOf): the blocks
// are read from that tree rather than from a second parse of the same markdown, and markdown is
// parsed here when the write left none.
func documentBlocksFor(readBack *pmdoc.Node, markdown string) *documentBlocks {
	if readBack != nil {
		return documentBlocksOf(readBack)
	}
	return readDocumentBlocks(markdown)
}

// readOpenersAsText records the typed block openings in node's own text: its text children outside
// inline code, read as runs that any other inline node ends.
func (blocks *documentBlocks) readOpenersAsText(node *pmdoc.Node) {
	var run strings.Builder
	flush := func() {
		text := run.String()
		run.Reset()
		for _, match := range typedOpening.FindAllStringIndex(text, -1) {
			if blocks.UnparsedOpeners == nil {
				blocks.UnparsedOpeners = &unparsedOpeners{Examples: []string{}}
			}
			blocks.UnparsedOpeners.Count++
			if len(blocks.UnparsedOpeners.Examples) < unparsedOpenerExamples {
				blocks.UnparsedOpeners.Examples = append(blocks.UnparsedOpeners.Examples, openerExample(text, match[0], match[1]))
			}
		}
	}
	for _, child := range node.Children {
		if child.Type == "text" && !slices.ContainsFunc(child.Marks, func(mark pmdoc.Mark) bool { return mark.Type == "inlineCode" }) {
			run.WriteString(child.Text)
			continue
		}
		flush()
	}
	flush()
}

// openerExample quotes the opening text[start:end] with a little of the text before it and the
// rest of its attributes, up to its closing brace within unparsedOpenerSpan bytes, on rune
// boundaries.
func openerExample(text string, start, end int) string {
	from := max(0, start-unparsedOpenerContext)
	for from > 0 && !utf8.RuneStart(text[from]) {
		from--
	}
	to := min(len(text), max(end, start+unparsedOpenerSpan))
	if brace := strings.IndexByte(text[end:to], '}'); brace >= 0 {
		to = end + brace + 1
	}
	for to < len(text) && !utf8.RuneStart(text[to]) {
		to++
	}
	example := text[from:to]
	if from > 0 {
		example = "…" + example
	}
	return example
}

// advisedResponse keeps advice on the HTTP response: the underlying models are also event
// payloads, where response-only guidance must not appear.
type advisedResponse struct {
	payload any
	advice  any
}

func (response advisedResponse) MarshalJSON() ([]byte, error) {
	payload, err := json.Marshal(response.payload)
	if err != nil {
		return nil, err
	}
	if len(payload) < 2 || payload[0] != '{' || payload[len(payload)-1] != '}' {
		return nil, fmt.Errorf("write advice payload must be a JSON object")
	}
	advice, err := json.Marshal(response.advice)
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, len(payload)+len(advice)+11)
	encoded = append(encoded, payload[:len(payload)-1]...)
	if len(payload) > 2 {
		encoded = append(encoded, ',')
	}
	encoded = append(encoded, `"advice":`...)
	encoded = append(encoded, advice...)
	encoded = append(encoded, '}')
	return encoded, nil
}

func withAdvice(payload any, advice *writeAdvice) any {
	if advice == nil {
		return payload
	}
	return advisedResponse{payload: payload, advice: advice}
}

// withRawAdvice wraps payload with whatever advice shape the caller already decided is worth
// sending, with no nil-check (contrast withAdvice, which treats a nil *writeAdvice as "send
// nothing). Two kinds of shape ride it: a document write's block report (*documentBlocks/*editBlocks,
// when a project document has no issue state to report, or an issue document's bounded
// issue-advice query failed), and suggestionsOnlyAdvice (withSuggestions): an ask on an unlinked
// project document, or an issue or ask write whose write advice failed.
func withRawAdvice(payload any, advice any) any {
	return advisedResponse{payload: payload, advice: advice}
}
