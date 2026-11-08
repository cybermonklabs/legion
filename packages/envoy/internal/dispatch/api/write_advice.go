package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type adviceAsk struct {
	ID       string `json:"id"`
	Question string `json:"question"`
}

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
	// Suggestions is the related-item check's result, set only on an issue or ask
	// creation; nil (omitted) everywhere else.
	Suggestions *model.Suggestions `json:"suggestions,omitempty"`
}

// suggestionsOnlyAdvice is the advice of a write with no issue state to report: an ask on an
// unlinked project document, which has no issue status, no writes-since-human count and no
// issue-scoped open-ask list, or an issue or ask write whose write-advice read failed. It still
// carries the related-item check's result and, for a create that seeded a spec, the block report
// that create read of it (withSuggestions): both are the write's own, and neither depends on the
// issue advice beside them.
type suggestionsOnlyAdvice struct {
	*documentBlocks
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
// asks - so writeAdvice calls this only for a session actor. The third, whether Legion owns the
// issue's status, runs only on an issue whose status is triage: that is the only line the answer
// decides, so an ordinary write on an ordinary issue gains no query at all. status is the
// issue's status as the write committed it, the same value writeAdvice reports. route names the
// caller for the one query whose failure is logged rather than returned
// (legionManagedStatusAdvice).
//
// q is the request pool, not the write's transaction: every one of these queries reads rows the
// write has already committed (writeAdvice).
func (s *server) computeWriteAdvice(
	ctx context.Context,
	q queryer,
	route string,
	issueKey string,
	actor model.Actor,
	excludeAskID string,
	status string,
) (*writeAdvice, error) {
	advice := &writeAdvice{YourOpenAsks: []adviceAsk{}, IssueStatus: status}
	if err := s.runAdviceQueryHook(ctx, q, "session_writes_since_human"); err != nil {
		return nil, &adviceQueryError{query: "session_writes_since_human", err: err}
	}
	if err := q.QueryRow(ctx, `
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
	if err := s.runAdviceQueryHook(ctx, q, "your_open_asks"); err != nil {
		return nil, &adviceQueryError{query: "your_open_asks", err: err}
	}
	rows, err := q.Query(ctx, `
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
	if status == model.IssueStatusTriage {
		advice.LegionManagedStatus = s.legionManagedStatusAdvice(ctx, q, route, issueKey)
	}
	return advice, nil
}

// legionManagedStatusAdvice answers whether Legion owns the issue's status for a write's advice,
// or nil when the walk failed. It is the last step of computeWriteAdvice and the only one whose
// failure is not fatal to the advice: the two before it are what a session asked for, while this
// one decides a single line, and every issue is created in triage, so returning an
// adviceQueryError here would put a third failure mode on the path that runs for every
// session-authored `dispatch issue`. What that would cost a creation is its issue state alone: the
// related-item check runs after the write commits whatever the advice answered, and rides a
// suggestions-only advice when it failed (withSuggestions). Nothing runs after this step, so the
// advice already assembled is complete without it.
//
// It takes no savepoint: the advice runs on the pool after the write committed (writeAdvice), so
// a failed statement here aborts nothing - there is no transaction to abort, and the write it
// describes is already durable. The failure is logged once, in the shape every omitted advice
// query takes, and the allowance writeAdvice set bounds the walk as it bounds the two queries
// before it.
func (s *server) legionManagedStatusAdvice(ctx context.Context, q queryer, route, issueKey string) *bool {
	if err := s.runAdviceQueryHook(ctx, q, "legion_managed_status"); err != nil {
		s.logAdviceError(route, issueKey, "legion_managed_status", err)
		return nil
	}
	managed, err := legionManagedStatus(ctx, q, issueKey)
	if err != nil {
		s.logAdviceError(route, issueKey, "legion_managed_status", err)
		return nil
	}
	return &managed
}

// runAdviceQueryHook runs the test seam before an advice query, on whatever that query runs on:
// the request pool for the issue-scoped queries, the write's own transaction for the
// superseded-approval read that stays inside it.
func (s *server) runAdviceQueryHook(ctx context.Context, q queryer, query string) error {
	if s.adviceQueryHook == nil {
		return nil
	}
	return s.adviceQueryHook(ctx, q, query)
}

// adviceAllowance is the most a write waits on its issue-scoped advice (writeAdvice). Healthy
// advice queries complete in single-digit milliseconds; past this the write is answered without
// its advice rather than held for a read that only decorates the answer. It bounds the queries
// through the request's own context, as the related-item check bounds its searches
// (relatedBounds): the advice no longer runs inside the write's transaction, where a context
// bound would have been destructive and `set local statement_timeout` was the only bound
// available.
const adviceAllowance = 500 * time.Millisecond

// errAdviceAllowanceEnded is the cause a query cut off at adviceAllowance carries, so a log line
// tells the allowance from a client that went away.
var errAdviceAllowanceEnded = errors.New("the write advice's allowance ended")

// writeAdvice is the issue-scoped advice of a session's write, or nil when its read failed.
//
// It runs after the write committed, on the request pool, bounded by the request's context and
// adviceAllowance - never inside the write's transaction. Every query it makes reads rows the
// write has already committed: the count of the session's writes since the last human event and
// the session's open asks both count the write's own rows, which are durable by the time this
// runs, and the legion label walk reads the issue chain, which this write leaves as it found it
// unless it wrote the labels itself, in which case they are committed too. status is passed in
// by the caller, read under the write's own lock, so the one field that does come from under the
// locks is read there and not here. Nothing in the write's events reads the advice - it is
// response-only (advisedResponse) - so no event depends on when it is computed.
//
// Run inside the transaction, as it was, these five to nine round trips stood between the write's
// last lock and its COMMIT: for their whole length the transaction held the event id it had
// drawn, and so the event horizon every live reader of the instance waits on, the issue or
// artifact owner row, the document's advisory lock and the document's writer gate. Each round
// trip is scheduling latency under load, not database time, which is why the horizon's stall
// warnings named an advice statement as the holder's last query in 23 of 36 samples (INFRA-592).
func (s *server) writeAdvice(
	ctx context.Context,
	route string,
	issueKey string,
	actor model.Actor,
	excludeAskID string,
	status string,
) *writeAdvice {
	// A person's write has no advice query to run - every one of them is about the calling
	// session, and the triage line this answers is about what a session is told - so it is
	// answered here, before the pool is asked for anything. Such a write still gets the triage
	// advice, which is true for a reader who may move the status.
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
	pool, err := s.readyPool()
	if err != nil {
		s.logAdviceError(route, issueKey, "pool", err)
		return nil
	}
	bounded, cancel := context.WithTimeoutCause(ctx, adviceAllowance, errAdviceAllowanceEnded)
	defer cancel()
	advice, err := s.computeWriteAdvice(bounded, pool, route, issueKey, actor, excludeAskID, status)
	if err != nil {
		query := "unknown"
		if queryErr, ok := err.(*adviceQueryError); ok {
			query = queryErr.query
		}
		// Which bound ended it: the allowance above, or the request itself - a client that went
		// away, or a keyed run past its limit. pgx answers a cut-off statement with its
		// context's error and not with the cause, so the cause is read here.
		if cause := context.Cause(bounded); errors.Is(cause, errAdviceAllowanceEnded) {
			err = fmt.Errorf("%w: %w", cause, err)
		}
		s.logAdviceError(route, issueKey, query, err)
		return nil
	}
	return advice
}

func (s *server) logAdviceError(route, issueKey, query string, err error) {
	slog.Warn("dispatch: write advice omitted", "route", route, "issue", issueKey, "query", query, "error", err)
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
// the new version restores it. Legion's design gate is exactly such an approval - a person's
// approval of a root issue's spec at a version - so a write that supersedes it closes that gate,
// and every READY in the tree is refused until a person approves again. The writer is the only
// one who can be told in time, and only the write that superseded the approval is told: a write
// on a document whose approval was already stale closed nothing.
type supersededApproval struct {
	// ApprovedVersion is the version a person approved, the one this write superseded.
	ApprovedVersion int `json:"approved_version"`
	// Version is the version this write stored.
	Version int `json:"version"`
	// ApprovedBy is who gave that approval, as its review records the actor's id.
	ApprovedBy string `json:"approved_by"`
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
// Like no other advice query, this one stays inside the write's transaction: the version row and
// the carry it reads are this write's own and are not committed yet, and its verdict is ordered
// against the reviews committed before the write by event_seq, which a read taken after the
// commit would answer differently. So it runs behind a savepoint, because a failed statement
// aborts the transaction it ran in and this must never fail the write it describes. It writes
// nothing, so the savepoint is always rolled back, which also restores the statement_timeout it
// set. That rollback runs on a context of its own (store.RollbackContext): on the cancelled
// context of a request whose client went away it was never sent, and the write's own rollback
// then had a savepoint still open under it.
func (s *server) supersededApprovalAdvice(
	ctx context.Context,
	tx pgx.Tx,
	route string,
	issueKey string,
	artifactID string,
	version int,
) *supersededApproval {
	adviceTx, err := tx.Begin(ctx)
	if err != nil {
		s.logAdviceError(route, issueKey, "superseded_approval", err)
		return nil
	}
	defer func() {
		rollback, cancel := store.RollbackContext(ctx)
		defer cancel()
		_ = adviceTx.Rollback(rollback)
	}()
	if _, err := adviceTx.Exec(ctx, "set local statement_timeout = '500ms'"); err != nil {
		s.logAdviceError(route, issueKey, "superseded_approval", err)
		return nil
	}
	if err := s.runAdviceQueryHook(ctx, adviceTx, "superseded_approval"); err != nil {
		s.logAdviceError(route, issueKey, "superseded_approval", err)
		return nil
	}
	approval, err := newestReviewAsApproval(ctx, adviceTx, artifactID, version)
	if err != nil {
		s.logAdviceError(route, issueKey, "superseded_approval", err)
		return nil
	}
	return approval
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

// documentWriteAdvice is the whole advice of a document write with no issue state to report: a
// project document's write, or an issue document's whose bounded issue-advice read failed. It
// carries the document half of writeAdvice - the block report its route produces, and the
// approval the write superseded - so neither depends on the issue advice beside it.
type documentWriteAdvice struct {
	*documentBlocks
	*editBlocks
	SupersededApproval *supersededApproval `json:"superseded_approval,omitempty"`
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
