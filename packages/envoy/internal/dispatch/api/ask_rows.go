package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// askRowColumns are docs.AskColumns plus the documents a block ask, a quoted anchor or an
// approval request names; every ask read carries whichever document applies. Queries selecting
// them read from askRowFrom.
const askRowColumns = docs.AskColumns + `, ba.id::text, ba.slug, ba.is_primary,
	aa.project_key, aa.slug, aa.name, aa.is_primary,
	pa.project_key, pa.slug, pa.name, pa.is_primary`

// askRowFrom is a var, not a const, because its anchor join is built from store.CastableUUID: a
// stored anchor resolves to the document its artifact_id casts to, whatever the spelling, and one
// whose artifact_id casts to no uuid resolves to no document instead of failing the read with
// SQLSTATE 22P02.
var askRowFrom = `from asks a
	left join artifacts ba on ba.id = a.block_artifact_id
	left join artifacts aa on aa.id = ` + store.CastableUUID(`a.anchor->>'artifact_id'`) + `
	left join artifacts pa on pa.id = (a.approval->>'artifact_id')::uuid`

// newestReply selects the newest comment in the thread of the ask aliased a: the one that
// committed last, since a comment's created_at is when its insert ran (migration 0065) and every
// comment insert first takes the owner's lock. lastReplyJoin reads it for every ask read, and a
// hand-back records its id, so both agree on which reply is newest.
const newestReply = `select c.id, c.author, c.created_at, c.turn from comments c
			where c.ask_id = a.id
			order by c.created_at desc, c.id desc
			limit 1`

// lastReplyJoin attaches the newest comment in the ask's thread as lr; the ask must be
// aliased a. askReadFrom and listOpenAsks both read the reply through it.
const lastReplyJoin = `
		left join lateral (` + newestReply + `) lr on true`

// approvalMoved says the ask aliased a is an approval request a version moved past the version its
// agent last handed to the human. A request that names two documents is moved when either of them
// is: each document of approval.documents carries its own version and requested_version, and the
// first of them is the pair approval.version and approval.requested_version repeat.
// waitingOnExpression and describeAsk, which reads it for a reply's
// waiting_on (commentThreadTarget.waitingOnAfter), share it.
const approvalMoved = `(a.kind = 'approval'
		and jsonb_path_exists(a.approval, '$.documents[*] ? (@.requested_version < @.version)'))`

// waitingOnExpression is the one turn rule every open-ask read, the Inbox order, a document's
// awaiting approval and the approval route's hand-back decision share; a reply's own waiting_on
// applies it to the reply it just inserted (commentThreadTarget.waitingOnAfter). A moved approval
// request waits on its agent until that agent hands its current version back. A hand-back records
// the reply that was newest when it ran (handed_back_reply_id), so the request waits on the human
// until a newer reply's turn decides it. Every other ask follows its newest reply's turn.
const waitingOnExpression = `case
	when ` + approvalMoved + `
	then 'agent'
	when a.kind = 'approval' and lr.id = a.handed_back_reply_id
	then 'human'
	else coalesce(lr.turn, 'human')
end`

// askedOfPersonExpression is the one rule for which person a question reaches, resolved as the
// question is read, in three steps: the person somebody handed it to, the assignee of its issue
// until somebody does, and the default assignee of the question's project while the issue holds
// nobody - or where there is no issue at all, which is a question on a project document. Every
// project names a default and the column refuses nothing
// (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql), so every question
// reaches a person.
//
// Six reads share it. One is askReadColumns below, the column list every ask read selects: the
// Inbox's rows (api/inbox.go), one ask, an issue's ask list, a document's ask list (api/asks.go)
// and the decisions read (api/decisions.go). The other five write it into a statement of their
// own: the Inbox read's person filter (?assignee=, listInbox in api/inbox.go), the author's read
// behind `dispatch open-asks` (the active CTE of listOpenAsks in api/asks.go), the open ask the
// Replies list leaves out, since the Inbox already lists it (myRepliesOwner in api/replies.go),
// the read the hand-on route takes before it writes, which is whom the question waited on
// until then (putAskAskedOf in api/ask_asked_of.go), and the person a write that opens a
// question answers with (askWaitingOnPerson below).
//
// It names the ask a, its issue i and its project p, so every query reading it joins all three
// the way askReadFrom does, the ask's own document among them: a question owned by an issue has
// no document row and one owned by a document has no issue row, so the project is reached
// through coalesce(i.project_key, ar.project_key). A query that reads the expression without
// those joins fails at run time rather than at compile time, which is why
// TestOneExpressionDecidesWhoWaits drives the routes behind the reads and
// TestTheWriteNamesThePerson the writes behind askWaitingOnPerson; the joins themselves are
// askedOfPersonJoins below, which every statement reading the expression builds its FROM clause
// from - the Inbox's person filter through askReadFrom, as it sits in the statement that
// selects askReadColumns.
//
// It compares with = rather than wrapping either column in lower(btrim(...)): asks.asked_of,
// issues.assignee and projects.default_assignee all hold what model.CanonicalLogin returns, so
// the sides match as they are stored. The expression spans three tables, so no index on
// issues.assignee (store/migrations/0034_issue_assignee.up.sql:6) serves a filter on it.
const askedOfPersonExpression = `coalesce(a.asked_of, i.assignee, p.default_assignee)`

// askedOfPersonJoins are the three tables askedOfPersonExpression reads beside the ask aliased
// a: its issue as i, its own document as ar, and the project of whichever of the two owns it as
// p. Five FROM clauses carry them - askReadFrom and askWaitingOnPerson below, the active CTE of
// listOpenAsks (api/asks.go), the not-exists subquery of myRepliesOwner (api/replies.go) and the
// read putAskAskedOf takes before it writes (api/ask_asked_of.go) - and each builds them from
// this one text, so the rule and the tables it names move together. askReadFrom serves two of
// the six reads, askReadColumns and the Inbox's person filter, which share one statement.
// TestOneExpressionDecidesWhoWaits holds them to this one spelling as it holds the expression to
// its own.
//
// Each is an outer join: a question owned by an issue has no document row and one owned by a
// document has no issue row, and the constraint asks_one_owner (migration 0009) guarantees
// exactly one of the two, so the coalesce names the project either way - the shape this
// repository already reads a question's project through (api/inbox.go, api/asks.go,
// projectListQuery in api/projects.go). The leading newline makes it a block a caller appends
// to its own `from <table> a`, whatever that caller indents its statement by.
const askedOfPersonJoins = `
	left join issues i on i.key = a.issue_key
	left join artifacts ar on ar.id = a.artifact_id
	left join projects p on p.key = coalesce(i.project_key, ar.project_key)`

// askItemRefExpression is the SPA item route of the ask aliased a, whose issue is aliased i and
// whose project document is aliased ar: an issue ask's `/issues/<key>/asks/<id>`, which lands an
// anchored ask on its document and an unanchored one on its Conversation turn (the bare issue page
// reads no `?ask=`), or a document ask's `?ask=` link. listOpenAsks and listMyAnswers share it.
const askItemRefExpression = `case
	when i.key is not null then '/issues/' || i.key || '/asks/' || a.id::text
	else '/projects/' || ar.project_key || '/documents/' || ar.slug || '?ask=' || a.id::text
end`

// askWaitingOn reads whom the open ask askID waits on, for the approval route's hand-back decision.
func askWaitingOn(ctx context.Context, q queryer, askID string) (string, error) {
	var waitingOn string
	if err := q.QueryRow(ctx, `select `+waitingOnExpression+`
		from asks a`+lastReplyJoin+`
		where a.id = $1 and a.state = 'open'`, askID).Scan(&waitingOn); err != nil {
		return "", fmt.Errorf("read ask waiting_on: %w", err)
	}
	return waitingOn, nil
}

// askWaitingOnPerson reads the person a question on owner reaches, for a write that opens one or
// hands an approval request back and must name that person on its answer. askedOf is the
// question's own asked_of as the write leaves it: nil for a question the write opens, and the
// open request's stored field for one it hands back, which a hand-back does not touch. It is the
// one rule every read of that person shares (askedOfPersonExpression) over the three tables that
// rule names beside the ask - its issue, its own document and the project of whichever of the
// two owns it, the joins askReadFrom makes - read against a row of one holding the ask's three
// columns rather than against the stored ask: the writes of both callers are queued on the
// ledger (docs.Ledger), so the row a write opens is not there until Commit, and the read runs in
// the read phase, under the owner's lock the caller holds, before anything is written. The
// person is scanned into a string: the foreign keys, asks_one_owner and the not-null
// projects.default_assignee leave no question reaching nobody, so a null here is a broken
// invariant and fails the scan rather than answering a missing field.
//
// It is deliberately not the whole of askReadColumns: a write's answer needs this one field,
// and the two handlers that call it (createAskFor in api/asks.go, requestArtifactApproval in
// api/reviews.go) already hold the ask they wrote. A route whose answer is a full ask read
// back - the hand-on's - reads it through loadAsk instead.
func askWaitingOnPerson(ctx context.Context, q queryer, owner owner, askedOf *string) (string, error) {
	var person string
	if err := q.QueryRow(ctx, `select `+askedOfPersonExpression+`
		from (select $1::text as asked_of, $2::text as issue_key, $3::uuid as artifact_id) a`+askedOfPersonJoins,
		askedOf, owner.IssueKey, owner.ArtifactID).Scan(&person); err != nil {
		return "", fmt.Errorf("read ask waiting_on_person: %w", err)
	}
	return person, nil
}

// askReadColumns are askRowColumns plus the newest comment in the ask's thread. WaitingOn derives
// from that reply with the moved and newly handed-back approval-request overrides in
// waitingOnExpression, aliased waiting_on so a query ordering by it (the Inbox) names the column
// rather than evaluating the rule a second time; LastReply still names the newest comment.
// WaitingOnPerson is askedOfPersonExpression, the person that turn waits on, aliased
// waiting_on_person for the same reason; every project names a default assignee, so it is null
// only for a question whose project row the read could not reach. node_answer is the ask's
// answer state as its node holds it (askNodeAnswerColumn), which scanAskRead lays over the row's:
// every read of an answer reads the node (R3). Queries selecting them read from askReadFrom.
// Event payloads are built from askRowColumns instead: they carry none of the three. So is the
// read a transition decides on under its owner's lock (readAskForTransition), whose state is the
// row's, as the one writer of an answer state checks the state it moves from against the row
// (asks.Settle, Change.From).
var askReadColumns = askRowColumns + `, lr.author, lr.created_at, ` + waitingOnExpression +
	` as waiting_on, ` + askedOfPersonExpression + ` as waiting_on_person, ` + askNodeAnswerColumn + ` as node_answer`

// askNodeAnswerColumn is the answer state of the ask aliased a as its node holds it: each fact of
// its answer that stands on the node (asks.NodeAnswerTuples), as a JSON list of nodeFact, as
// docs' askNodeAnswers reads the text routes' asks. It is null for a row that names no node
// (asks.node_kind): the one writer of an answer state writes the answer on the node only once the
// row names it (asks.Settle), so the row of an ask a process of the release before wrote, until
// the boot step's backfill marks it, and of an ask whose relations are past the store's bound
// (graphstore.AskFacts) is its answer's only record. readAskNodeAnswer decodes it; askNodeAnswerOf
// writes it for an ask of another alias.
var askNodeAnswerColumn = askNodeAnswerOf("a")

// askNodeAnswerOf is askNodeAnswerColumn for the ask aliased ask.
func askNodeAnswerOf(ask string) string {
	return `case when ` + ask + `.node_kind is not null then (
		select coalesce(json_agg(json_build_object('kind', f.kind, 'to_kind', f.to_kind, 'to_id', f.to_id)), '[]'::json)
		from (` + asks.NodeAnswerTuples(ask+`.id::text`) + `) f
		where f.asserted) end`
}

// askNodeValueOf is the value of kind that stands on the node of the ask aliased ask, null where
// none stands: the newest tuple of kind (graphstore.CurrentTupleRead) with its far end bound to a
// value, so the read walks graph_tuples_values in that index's own order and sorts nothing - the
// Inbox's reply rows hold their plans to no sort (TestMyRepliesQueriesReadTheirStatesIndex). It is
// a probe of the ask's own node that the expressions below read under a case or an or, so it ends
// in an offset 0, which keeps the planner from pulling it up into a read of the log for every ask
// at once.
func askNodeValueOf(ask, kind string) string {
	return `(select f.to_id from (` + graphstore.CurrentTupleRead(`t.kind`, `t.to_id, t.asserted`,
		`t.from_kind = '`+graphstore.KindAsk+`' and t.from_id = `+ask+`.id::text and t.kind = '`+kind+
			`' and t.to_kind = '`+graphstore.KindValue+`'`) + `) f where f.asserted offset 0)`
}

// askNodeRelationOf is the far end of the relation of kind that stands on the node of the ask
// aliased ask, null where none stands: the newest tuple of each far end
// (graphstore.CurrentTupleRead), read through graph_tuples_current in its own order. The one
// writer of an answer state keeps one far end of each relation it writes standing and retracts
// the others (asks.Settle), so the probe answers the first that stands. It ends in an offset 0, as
// askNodeValueOf does.
func askNodeRelationOf(ask, kind string) string {
	return `(select f.to_id from (` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = '`+graphstore.KindAsk+`' and t.from_id = `+ask+`.id::text and t.kind = '`+kind+
			`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `) f where f.asserted limit 1 offset 0)`
}

// askNodeStateOf is the answer state of the ask aliased ask as asks.NodeAnswer.ApplyTo lays it
// over the row, for a statement that picks asks by it (R3): the answer_state its node holds where
// the row names the node, open where the node holds none, and the row's own where the row names no
// node. An owner's ask lists by state (listIssueAsksQueryOpen and its siblings) and a session's
// open asks on one issue (your_open_asks) pick by it alone; a statement that reads open asks
// across owners picks by it beside the row's state (askOpenOf). A read that decides a transition
// reads the row's state instead, as the one writer of an answer state checks the state it moves
// from against the row (asks.Settle), and so do the open counts (agentOpenAskCounts,
// projectListQuery, issueSummaryJoins, priorityCountsQuery).
func askNodeStateOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then ` + ask + `.state else coalesce(` +
		askNodeValueOf(ask, graphstore.TupleAnswerState) + `, '` + asks.StateOpen + `') end`
}

// askOpenOf is how a statement that reads open asks across owners - the Inbox (listInbox), the
// asks the Replies list leaves out for it (myRepliesOwner), a session's or a project's open asks
// (listOpenAsks) and the open approval requests of a list of documents
// (openApprovalAsksStatement) - picks the open ask aliased ask: its row's state open, which the
// partial index asks_open (0001) holds, so the index picks the candidates out of every ask
// stored, and its node's state open (askNodeStateOf), which decides (R3). An ask its node holds
// anything but open is never picked. The two agree outside a roll; inside its window the row is
// the newer of the two, written by a process of the release before and not yet stated on the node
// by the decisions step's catch-up, so an ask that process answered is left out until then.
func askOpenOf(ask string) string {
	return ask + `.state = '` + asks.StateOpen + `' and ` + askNodeStateOf(ask) + ` = '` + asks.StateOpen + `'`
}

// askNodeAnswerTextOf is the words of the answer of the ask aliased ask as ApplyTo lays them over
// the row: the answer value its node holds on an ask the node holds answered, none on any other
// the node holds, and the row's answer's text where the row names no node.
func askNodeAnswerTextOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then ` + ask + `.answer->>'text' when ` +
		askNodeValueOf(ask, graphstore.TupleAnswerState) + ` = '` + asks.StateAnswered + `' then ` +
		askNodeValueOf(ask, graphstore.TupleAnswer) + ` end`
}

// askNodeAnswererOf and askNodeAnsweredAtOf are the person who answered the ask aliased ask and
// when, as ApplyTo lays its answer over the row: the node's answered_by and answered_at on an ask
// the node holds answered, null on any other the node holds, and the row's answer's user and at
// where the row names no node. The Replies list's unread count reads an answer as a write in the
// ask's thread through them (myRepliesPageRest).
func askNodeAnswererOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then ` + ask + `.answer->>'user' when ` +
		askNodeValueOf(ask, graphstore.TupleAnswerState) + ` = '` + asks.StateAnswered + `' then ` +
		askNodeRelationOf(ask, graphstore.TupleAnsweredBy) + ` end`
}

func askNodeAnsweredAtOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then (` + ask + `.answer->>'at')::timestamptz when ` +
		askNodeValueOf(ask, graphstore.TupleAnswerState) + ` = '` + asks.StateAnswered + `' then (` +
		askNodeValueOf(ask, graphstore.TupleAnsweredAt) + `)::timestamptz end`
}

// askNodeResolutionOf is the resolution of the ask aliased ask as ApplyTo lays it over the row, as
// jsonb: the resolution value its node holds on an ask the node holds resolved, none on any other
// the node holds, and the row's own where the row names no node. The write advice's declined asks
// read a decline through it (readDeclinedAsks), as every read of an answer reads the node (R3).
func askNodeResolutionOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then ` + ask + `.resolution when ` +
		askNodeValueOf(ask, graphstore.TupleAnswerState) + ` = '` + asks.StateResolved + `' then (` +
		askNodeValueOf(ask, graphstore.TupleResolution) + `)::jsonb end`
}

// askNodeAnswerWordsOf is the answer of the ask aliased ask as ApplyTo lays it over the row, as
// jsonb of asks.answer's shape, the one ask_search_suffix reads the words of: the answer and
// selection values its node holds on an ask the node holds answered, null on any other the node
// holds, and the row's own answer where the row names no node.
func askNodeAnswerWordsOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then ` + ask + `.answer when ` +
		askNodeValueOf(ask, graphstore.TupleAnswerState) + ` = '` + asks.StateAnswered + `' then jsonb_build_object('text', ` +
		askNodeValueOf(ask, graphstore.TupleAnswer) + `, 'selected', (` + askNodeValueOf(ask, graphstore.TupleSelected) + `)::jsonb) end`
}

// askSearchTextOf is the text of the ask aliased ask that a search hit shows (search's txt
// lateral) and an answer is given to read for the hit (goingItemTextQuery): its question, a space,
// and ask_search_suffix of its options and of its answer as its node holds it
// (askNodeAnswerWordsOf), as every read of an answer reads the node (R3).
//
// The vector search ranks the ask by is still built from its row (asks_search, over asks.answer:
// migrations 0087 and 20261120021308). The row is the node's projection, which the one writer of
// an answer state writes in the same transaction (asks.Settle), so the two hold the same words but
// in two cases: inside a roll's window, where a process of the release before wrote the row alone
// and the decisions step's catch-up has not stated it on the node yet, and on an ask its node holds
// resolved, whose row keeps the answer it held, which no read shows. A query can match such words,
// and the hit then shows the question and the options without them; the vector ranks and shows no
// answer, and moving it onto the node would take a trigger over graph_tuples and a rewrite of every
// ask's vector, a migration of its own.
func askSearchTextOf(ask string) string {
	return ask + `.question || ' ' || ask_search_suffix(` + ask + `.options, ` + askNodeAnswerWordsOf(ask) + `)`
}

// nodeFact is one value or relation of a node as a statement aggregates it: its kind, the kind
// of its far end, and that far end, the value itself for a value.
type nodeFact struct {
	Kind   string `json:"kind"`
	ToKind string `json:"to_kind"`
	ToID   string `json:"to_id"`
}

// readAskNodeAnswer decodes askNodeAnswerColumn into what the node of an ask holds of its answer
// state, as the one decoder of the node's answer reads it (asks.NodeAnswer), or answers which of
// its facts does not parse. A null column is a row that names no node, whose answer the node does
// not hold (asks.NodeAnswer.Named).
func readAskNodeAnswer(encoded []byte) (asks.NodeAnswer, error) {
	if encoded == nil {
		return asks.NodeAnswer{}, nil
	}
	var facts []nodeFact
	if err := json.Unmarshal(encoded, &facts); err != nil {
		return asks.NodeAnswer{}, fmt.Errorf("the node's answer: %w", err)
	}
	node := asks.NodeAnswer{Named: true}
	for _, fact := range facts {
		if err := node.Read(fact.Kind, fact.ToID); err != nil {
			return asks.NodeAnswer{}, err
		}
	}
	return node, nil
}

// askAnswerRead is the answer state and answer of the ask askID as every ask read shows them
// (asks.NodeAnswer.ApplyTo), for a read that selects the ask's row's state and answer (asks.state,
// and asks.answer, nil for none) and its node's answer (askNodeAnswerColumn) rather than a whole
// ask: a person's answers (listMyAnswers), the Inbox's reply rows (answerReplyRows) and the
// answered questions of other projects (relatedOtherProjects).
func askAnswerRead(askID, state string, answer, nodeAnswer []byte) (model.Ask, error) {
	ask := model.Ask{ID: askID, State: state}
	if len(answer) > 0 {
		ask.Answer = new(model.AskAnswer)
		if err := json.Unmarshal(answer, ask.Answer); err != nil {
			return model.Ask{}, fmt.Errorf("decode the answer of ask %s: %w", askID, err)
		}
	}
	node, err := readAskNodeAnswer(nodeAnswer)
	if err != nil {
		return model.Ask{}, fmt.Errorf("read ask %s: %w", askID, err)
	}
	node.ApplyTo(&ask)
	return ask, nil
}

// askReadFrom is askRowFrom with the three joins askedOfPersonExpression reads beside the ask
// (askedOfPersonJoins) and the ask's newest reply. A query over it that wants the issue's or
// the document's own columns reads them from those joins rather than joining either table
// again, as the Inbox read does.
var askReadFrom = askRowFrom + askedOfPersonJoins + lastReplyJoin

// nullableAskArtifactRef holds the nullable columns of one optional document an ask names - a
// quoted anchor's or an approval request's artifact_id, both resolved through a left join that
// may find no row. artifact is nil exactly when the join found none.
type nullableAskArtifactRef struct {
	project, slug, name *string
	primary             *bool
}

func (ref nullableAskArtifactRef) artifact() *model.AskAnchorArtifact {
	if ref.project == nil {
		return nil
	}
	return &model.AskAnchorArtifact{
		Project: *ref.project, Slug: *ref.slug, Name: *ref.name, Primary: *ref.primary,
	}
}

// scanAskRow decodes one askRowColumns row; extra receives the columns after them.
func scanAskRow(row pgx.Row, extra ...any) (model.Ask, error) {
	var blockArtifactID, blockArtifactSlug *string
	var blockArtifactPrimary *bool
	var anchor, approval nullableAskArtifactRef
	ask, err := docs.ScanAsk(
		row,
		append(
			[]any{
				&blockArtifactID,
				&blockArtifactSlug,
				&blockArtifactPrimary,
				&anchor.project,
				&anchor.slug,
				&anchor.name,
				&anchor.primary,
				&approval.project,
				&approval.slug,
				&approval.name,
				&approval.primary,
			},
			extra...,
		)...,
	)
	if err != nil {
		return model.Ask{}, err
	}
	if blockArtifactID != nil {
		ask.BlockArtifact = &model.AskBlockArtifact{
			ID: *blockArtifactID, Slug: *blockArtifactSlug, Primary: *blockArtifactPrimary,
		}
	}
	ask.AnchorArtifact = anchor.artifact()
	ask.ApprovalArtifact = approval.artifact()
	return ask, nil
}

// attachAskBacklinkCounts reads every listed ask's inbound graph-edge count in one query. The
// count is nullable on model.Ask because mutation responses do not pay for it; list and detail
// readers always set it, including zero. An ask's own clarification replies are inbound edges
// too (`graph_edges`' `replies_to` arm), and the card renders that thread inline directly
// below the count, so they are not what "referenced by" counts.
func attachAskBacklinkCounts(ctx context.Context, q queryer, who model.Actor, asks []*model.Ask) error {
	if len(asks) == 0 {
		return nil
	}
	counts, err := refs.BacklinkCounts(ctx, q, who, "ask", distinctAskIDs(asks), refs.AskBacklinkExclusions)
	if err != nil {
		return err
	}
	applyAskBacklinkCounts(asks, counts)
	return nil
}

// applyAskBacklinkCounts stamps each ask with its count, zero included, from what the count
// query answered: attachAskBacklinkCounts for one ask, and queueAskDetails for every list of
// asks.
func applyAskBacklinkCounts(asks []*model.Ask, counts map[string]int) {
	for _, ask := range asks {
		count := counts[ask.ID]
		ask.ReferencedByCount = &count
	}
}

// queueAskDetails queues in b what every listed ask carries beside its row - its identity event,
// its inbound graph-edge count, and the part ids of its options and of its answer with its part
// revision - and returns the apply that stamps them onto the asks once the batch is closed. The
// three statements are queued in that order wherever an ask is read: the issue read queues them
// beside its other reads (readIssueDetails), and a read of its own sends them as a batch of its
// own (loadAskDetails). No ask queues nothing and its apply stamps nothing. A node kind refs
// declares no counting arms for is refused as the count is queued, which queues nothing and
// answers nothing itself, so the apply reports that refusal.
func queueAskDetails(b *pgx.Batch, who model.Actor, asks []*model.Ask) func() error {
	askIDs := distinctAskIDs(asks)
	openedEventIDs := make(map[string]int64, len(askIDs))
	counts := map[string]int{}
	partIDs := make([]refs.AskIDs, len(asks))
	events.QueueOpenedEventIDs(b, askIDs, openedEventIDs)
	countsErr := refs.QueueBacklinkCounts(b, who, "ask", askIDs, refs.AskBacklinkExclusions, counts)
	refs.QueueAskPartIDs(b, asks, partIDs)
	return func() error {
		if countsErr != nil {
			return countsErr
		}
		if err := applyOpenedEventIDs(asks, openedEventIDs); err != nil {
			return err
		}
		applyAskBacklinkCounts(asks, counts)
		applyAskPartIDs(asks, partIDs)
		return nil
	}
}

// loadAskDetails sends queueAskDetails's statements as a batch of their own on q, for an ask read
// that queues nothing else: an owner's asks, the Inbox's rows, an event payload's asks. The batch
// holds q's connection until its results close, so nothing else reads the database in between.
func loadAskDetails(ctx context.Context, q queryer, who model.Actor, asks []*model.Ask) error {
	batch := &pgx.Batch{}
	apply := queueAskDetails(batch, who, asks)
	if batch.Len() == 0 {
		return nil
	}
	if err := q.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}
	return apply()
}

// attachAskPartIDs reads, in one query over `ask_blocks`, the part id of every listed ask's
// options and answer and the ask's current part revision (`refs.AskPartIDs`), so every read
// that shows an ask's options shows the reference of each: `dispatch://<key>/ask/<id>#b-<part
// id>`, pinned to the ask's `revision` with `@v<N>`. It is read beside attachAskBacklinkCounts,
// which every list and detail read of an ask already pays for, and never by a write, whose
// answer carries no id. A list of asks reads the same ids in its own batch instead
// (queueAskDetails, over refs.QueueAskPartIDs).
//
// Which option or answer each stored part stands for, and what an ask the index has not reached
// or an option no part stands for carries, is refs.AskPartIDs'.
func attachAskPartIDs(ctx context.Context, q queryer, asks []*model.Ask) error {
	if len(asks) == 0 {
		return nil
	}
	partIDs, err := refs.AskPartIDs(ctx, q, asks)
	if err != nil {
		return err
	}
	applyAskPartIDs(asks, partIDs)
	return nil
}

// applyAskPartIDs stamps each ask with the ids read for it, in the asks' own order: the part id
// of each of its options and of its answer, and the revision its parts have reached. Both halves
// of the read end here - attachAskPartIDs for one ask or a read of its own, queueAskDetails for
// a list read in a batch.
func applyAskPartIDs(asks []*model.Ask, partIDs []refs.AskIDs) {
	for index, ask := range asks {
		ask.Revision = partIDs[index].Revision
		for option := range ask.Options {
			ask.Options[option].ID = partIDs[index].Options[option]
		}
		if ask.Answer != nil {
			ask.Answer.ID = partIDs[index].Answer
		}
	}
}

// attachAskReadFields is what the read of one ask (getAsk) attaches beyond its row: its inbound
// graph-edge count (attachAskBacklinkCounts), then the part ids of its options and of its answer
// with its part revision (attachAskPartIDs). A write attaches neither. A list of asks reads the
// same fields, with their identity events, in one batch instead (queueAskDetails, loadAskDetails).
func attachAskReadFields(ctx context.Context, q queryer, who model.Actor, asks []*model.Ask) error {
	if err := attachAskBacklinkCounts(ctx, q, who, asks); err != nil {
		return err
	}
	return attachAskPartIDs(ctx, q, asks)
}

func (s *server) loadAskAnchorArtifact(
	ctx context.Context,
	q queryer,
	artifactID string,
) (*model.AskAnchorArtifact, error) {
	var artifact model.AskAnchorArtifact
	if err := q.QueryRow(ctx, `
		select coalesce(project_key, ''), slug, name, is_primary
		from artifacts
		where id = $1
	`, artifactID).Scan(&artifact.Project, &artifact.Slug, &artifact.Name, &artifact.Primary); err != nil {
		return nil, err
	}
	return &artifact, nil
}

// scanAskRead decodes one askReadColumns row; its state, answer and resolution are its node's
// (asks.NodeAnswer.ApplyTo), an open ask gets WaitingOn from waitingOnExpression, WaitingOnPerson
// comes from askedOfPersonExpression, the three steps that end at the project's default assignee,
// and the newest reply is returned separately (nil when the thread is empty).
func scanAskRead(row pgx.Row, extra ...any) (model.Ask, *model.AskLastReply, error) {
	var replyAuthor []byte
	var repliedAt *time.Time
	var turn string
	var waitingOnPerson *string
	var nodeAnswer []byte
	ask, err := scanAskRow(row, append([]any{&replyAuthor, &repliedAt, &turn, &waitingOnPerson, &nodeAnswer}, extra...)...)
	if err != nil {
		return model.Ask{}, nil, err
	}
	node, err := readAskNodeAnswer(nodeAnswer)
	if err != nil {
		return model.Ask{}, nil, fmt.Errorf("read ask %s: %w", ask.ID, err)
	}
	node.ApplyTo(&ask)
	if ask.State == "open" {
		ask.WaitingOn = turn
	}
	if waitingOnPerson != nil {
		ask.WaitingOnPerson = *waitingOnPerson
	}
	if repliedAt == nil {
		return ask, nil, nil
	}
	var reply model.AskLastReply
	if err := json.Unmarshal(replyAuthor, &reply.Author); err != nil {
		return model.Ask{}, nil, fmt.Errorf("decode ask last reply author: %w", err)
	}
	reply.CreatedAt = timestampValue(*repliedAt)
	return ask, &reply, nil
}
