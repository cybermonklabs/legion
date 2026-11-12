package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
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
// comment insert first takes the owner row. lastReplyJoin reads it for every ask read, and a
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
// Five reads share it. One is askReadColumns below, the column list every ask read selects: the
// Inbox's rows (api/inbox.go), one ask, an issue's ask list, a document's ask list (api/asks.go)
// and the decisions read (api/decisions.go). The other four write it into a FROM clause of their
// own: the Inbox read's person filter (?assignee=, listInbox in api/inbox.go), the author's read
// behind `dispatch open-asks` (the active CTE of listOpenAsks in api/asks.go), the open ask the
// Replies list leaves out, since the Inbox already lists it (myRepliesOwner in api/replies.go),
// and the read the hand-on route takes before it writes, which is whom the question waited on
// until then (putAskAskedOf in api/ask_asked_of.go).
//
// It names the ask a, its issue i and its project p, so every query reading it joins all three
// the way askReadFrom does, the ask's own document among them: a question owned by an issue has
// no document row and one owned by a document has no issue row, so the project is reached
// through coalesce(i.project_key, ar.project_key). A query that reads the expression without
// those joins fails at run time rather than at compile time, which is what
// TestOneExpressionDecidesWhoWaits drives every one of the five reads for.
//
// It compares with = rather than wrapping either column in lower(btrim(...)): asks.asked_of,
// issues.assignee and projects.default_assignee all hold what model.CanonicalLogin returns, so
// the sides match as they are stored. The expression spans three tables, so no index on
// issues.assignee (store/migrations/0034_issue_assignee.up.sql:6) serves a filter on it.
const askedOfPersonExpression = `coalesce(a.asked_of, i.assignee, p.default_assignee)`

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

// askReadColumns are askRowColumns plus the newest comment in the ask's thread. WaitingOn derives
// from that reply with the moved and newly handed-back approval-request overrides in
// waitingOnExpression, aliased waiting_on so a query ordering by it (the Inbox) names the column
// rather than evaluating the rule a second time; LastReply still names the newest comment.
// WaitingOnPerson is askedOfPersonExpression, the person that turn waits on, aliased
// waiting_on_person for the same reason; every project names a default assignee, so it is null
// only for a question whose project row the read could not reach. Queries selecting them read
// from askReadFrom. Event payloads are built from askRowColumns instead: they carry neither.
const askReadColumns = askRowColumns + `, lr.author, lr.created_at, ` + waitingOnExpression +
	` as waiting_on, ` + askedOfPersonExpression + ` as waiting_on_person`

// askReadFrom joins the three tables askedOfPersonExpression reads beside the ask: its issue as
// i, its own document as ar, and the project of whichever of the two owns it as p. A query over
// it that wants the issue's or the document's own columns reads them from these joins rather
// than joining either table again, as the Inbox read does. Each is an outer join: a question
// owned by an issue has no document row and one owned by a document has no issue row, and the
// constraint asks_one_owner (migration 0009) guarantees exactly one of the two, so the coalesce
// below names the project either way - the shape this repository already reads a question's
// project through (api/inbox.go, api/asks.go, projectListQuery in api/projects.go).
var askReadFrom = askRowFrom + `
	left join issues i on i.key = a.issue_key
	left join artifacts ar on ar.id = a.artifact_id
	left join projects p on p.key = coalesce(i.project_key, ar.project_key)` + lastReplyJoin

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
func attachAskBacklinkCounts(ctx context.Context, q queryer, asks []*model.Ask) error {
	if len(asks) == 0 {
		return nil
	}
	counts, err := refs.BacklinkCounts(ctx, q, "ask", distinctAskIDs(asks), refs.AskBacklinkExclusions)
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
func queueAskDetails(b *pgx.Batch, asks []*model.Ask) func() error {
	askIDs := distinctAskIDs(asks)
	openedEventIDs := make(map[string]int64, len(askIDs))
	counts := map[string]int{}
	partIDs := make([]refs.AskIDs, len(asks))
	events.QueueOpenedEventIDs(b, askIDs, openedEventIDs)
	countsErr := refs.QueueBacklinkCounts(b, "ask", askIDs, refs.AskBacklinkExclusions, counts)
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
func loadAskDetails(ctx context.Context, q queryer, asks []*model.Ask) error {
	batch := &pgx.Batch{}
	apply := queueAskDetails(batch, asks)
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
func attachAskReadFields(ctx context.Context, q queryer, asks []*model.Ask) error {
	if err := attachAskBacklinkCounts(ctx, q, asks); err != nil {
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

// scanAskRead decodes one askReadColumns row; an open ask gets WaitingOn from
// waitingOnExpression, WaitingOnPerson comes from askedOfPersonExpression, the three steps that
// end at the project's default assignee, and the newest reply is returned separately (nil when
// the thread is empty).
func scanAskRead(row pgx.Row, extra ...any) (model.Ask, *model.AskLastReply, error) {
	var replyAuthor []byte
	var repliedAt *time.Time
	var turn string
	var waitingOnPerson *string
	ask, err := scanAskRow(row, append([]any{&replyAuthor, &repliedAt, &turn, &waitingOnPerson}, extra...)...)
	if err != nil {
		return model.Ask{}, nil, err
	}
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
