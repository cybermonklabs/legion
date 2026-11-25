// Package sessionanswers is the read model of what people wrote that Dispatch delivered to one
// session: GET /api/v1/agents/{session_id}/answers' read, which the outbox's tests read back
// through too, so the route reads what the publisher records.
package sessionanswers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// declinedResolution is the SQL predicate, on a resolution the events carry, that holds for an
// ask a person declined to answer (model.AskResolutionDeclined).
const declinedResolution = `resolution->>'kind' = '` + model.AskResolutionDeclined + `'`

// nodeAnswer is the answer of the ask a as every read of an answer shows it (R3), as
// model.AskAnswer's JSON without its act: where the row names its node (asks.node_kind), the answer
// the node holds on an ask it holds answered - its words, its selection, its time and its person,
// each fact that stands read through the store's one reading of the log
// (graphstore.CurrentTupleRead), as api's askNodeAnswerColumn reads them - and null on an ask it
// holds otherwise; and the row's own answer where the row names no node, its answer's one record
// until the boot step's backfill marks it. The node holds no act, so scan lays the row's on it
// where the row holds the same person's answer at the same time, as api's askNodeAnswer.applyTo
// does. api's reader is not this package's to import (api imports it), so the statement is written
// here over the same facts.
var nodeAnswer = `case when a.node_kind is null then a.answer else (
		select case when f.state = '` + asks.StateAnswered + `' then jsonb_build_object(
			'user', f.answered_by, 'selected', coalesce(f.selected::jsonb, '[]'::jsonb), 'text', f.answer, 'at', f.answered_at) end
		from (
			select max(n.to_id) filter (where n.kind = '` + graphstore.TupleAnswerState + `') as state,
			       max(n.to_id) filter (where n.kind = '` + graphstore.TupleAnswer + `') as answer,
			       max(n.to_id) filter (where n.kind = '` + graphstore.TupleSelected + `') as selected,
			       max(n.to_id) filter (where n.kind = '` + graphstore.TupleAnsweredAt + `') as answered_at,
			       max(n.to_id) filter (where n.kind = '` + graphstore.TupleAnsweredBy + `') as answered_by
			from (` + graphstore.CurrentTupleRead(
	`t.kind, t.to_kind, case when t.to_kind = '`+graphstore.KindValue+`' then '' else t.to_id end`,
	`t.kind, t.to_id, t.asserted`,
	`t.from_kind = '`+graphstore.KindAsk+`' and t.from_id = a.id::text and t.kind in ('`+
		graphstore.TupleAnswerState+`', '`+graphstore.TupleAnswer+`', '`+graphstore.TupleSelected+`', '`+
		graphstore.TupleAnsweredAt+`', '`+graphstore.TupleAnsweredBy+`')`) + `) n
			where n.asserted
		) f
	) end`

// ownsAsk is the SQL predicate, on the ask that the alias a names, that holds when the session $1
// owns that ask: it wrote the ask (asks.author) or follows it, read through ask_followers' primary
// key (ask_id, session_id, 0029). It is false, never null, where a is null: a comment outside an
// ask's thread.
const ownsAsk = `((a.author->>'kind' = 'session' and a.author->>'id' = $1) is true
	       or exists (select 1 from ask_followers f where f.ask_id = a.id and f.session_id = $1))`

// query reads, in one statement, every item a person wrote that Dispatch delivered to the session
// $1 since $2, $3 being the session's own topic (contracts.LegacyAgentSubject). Its three sources:
//
//   - S1 (reached): events a person wrote of a type that carries a person's words to a session -
//     ask.answered, comment.created, message.created, message.answered (a reply to a message) and
//     ask.resolved, of which the list takes the declines alone (an ask a person chose not to
//     answer) - created since the window's start, whose published_destinations hold the session's
//     topic: the outbox's own record of each topic it published the event to
//     (outbox.publishDestination), so the list holds what the follower, author and session routes
//     delivered, and never what an issue's role: route delivered to a role topic, which names no
//     session. It reads the partial index events_person_words_with_declines for the window and
//     filters the array on those rows.
//   - S2 (message_ids' second arm): the session's message deliveries the listener took: targeted
//     messages, direct or on an issue, and broadcast copies. `sent` is one it handed to the
//     running session, `queued` one it stored for a session that is not running and hands over
//     when it runs again (api/envoy_resolve.go, deliveryOutcome); both are words a person wrote
//     that reach the session, so both are read (model.DeliveryTakenStatesSQL). It is an index-only
//     scan of message_deliveries_session_taken (20261114081435, which replaced 0147's
//     message_deliveries_session_created), whose predicate holds both states and which carries
//     `state` beside message_id: Postgres drops a predicate's clause from a scan only where it can
//     prove it, and over two states it keeps the arm's own state test, which the carried column
//     answers from the index. message_id is the one column the arm returns, so a cached generic
//     plan chooses it over message_deliveries_session (0051, session_id alone) too.
//   - S3 (mentioned): the session's comment deliveries the listener took, a mention's
//     (comment_deliveries_session, 0146), by the same two states.
//
// A delivery row's created_at is its transaction's now(), which for a first attempt can precede the
// comment's or message's own clock_timestamp() created_at by as long as that transaction ran, so
// the window is applied to the comment or message and not to its delivery. Each delivery arm still
// reads only the session's rows created since an hour before the window, a range of its index on
// (session_id, created_at desc), so a long-lived session's earlier deliveries - a session that
// receives every broadcast copy holds tens of thousands - are not read on every request: a
// delivery of an item in the window was created in or after the transaction that wrote the item,
// and no such transaction runs for an hour.
//
// Each source names a row by its id, and each kind's ids are a set, so an item reached several ways
// is one row. Every row is then read at its current words: an ask's current answer, as its node
// holds it (nodeAnswer), when that answer's own time is in the window; a comment or message's
// current body, with its edit time and deletion mark. A decline is read from its ask.resolved
// event, whose published_destinations hold the session's topic. The asks row supplies the
// question, owner and whether the session owns the ask. The read excludes agent and system posts.
// IDs are compared as text through the asks_id_text, comments_id_text and messages_id_text
// indexes, so an event with a non-UUID payload ID cannot fail the read.
//
// owns, on the three arms that name an ask, says whether the session owns that ask (ownsAsk): the
// answer's ask, the decline's ask, and the ask whose thread a comment is in (none outside one).
// scan tells the session that an ask is its own only then. The follower route reaches the ask's
// followers, but the session route, the claimant route, a credit on the issue's primary document
// and a mention reach other sessions too, and each of those reads the item by its own reason.
//
// replaced counts an ask's real answers as api's answerEventsQuery does: the ask.answered events
// whose actor is the user the answer names, one per answer time (a settlement that restores an
// answered block writes the same answer again under its editor). answer_events reads the listed
// asks' ask.answered events by their payload id alone (events_ask_payload_id) and is materialized,
// so the actor check runs on those few rows: with it inside the scan, the planner ANDs in
// events_person_words_with_declines, whose predicate the check implies, and reads every entry of
// that index for each ask. row_answer is the row's own answer beside it, whose act scan lays on the
// node's answer; it is null on every other arm.
var query = `
	with reached as (
		select e.type, e.payload->>'id' as id, e.payload->'resolution' as resolution
		from events e
		where e.actor->>'kind' = 'user'
		  and e.type in ('ask.answered', 'comment.created', 'message.created', 'message.answered',
		                 'ask.resolved')
		  and e.created_at >= $2
		  and $3 = any(e.published_destinations)
	), mentioned as (
		select cd.comment_id::text as id
		from comment_deliveries cd
		where cd.session_id = $1 and cd.state in ` + model.DeliveryTakenStatesSQL + ` and cd.created_at >= $2 - interval '1 hour'
	), answer_ids as (
		select distinct id from reached where type = 'ask.answered'
	), answer_events as materialized (
		select e.payload->>'id' as id, e.actor, e.payload->'answer' as answer
		from events e
		where e.type = 'ask.answered' and e.payload->>'id' in (select id from answer_ids)
	), decline_ids as (
		-- A decline carries its own words, so the arm keeps the resolution the event recorded.
		-- An ask holds at most one decline: a resolved ask is not resolved again (api's
		-- transitionAskTx), and the only resolutions anything takes back are the retraction a
		-- document's settlement wrote when the block left (docs.settlementRetraction) and the
		-- withdrawal an issue's close wrote, which its reopen undoes - never a decline. The
		-- distinct on is what holds the list to one item per ask, the latest decline of it,
		-- whatever the events hold.
		select distinct on (id) id, resolution
		from reached
		where type = 'ask.resolved' and ` + declinedResolution + `
		order by id, (resolution->>'at')::timestamptz desc
	), comment_ids as (
		select id, bool_or(m) as mentioned
		from (
			select id, false as m from reached where type = 'comment.created'
			union all
			select id, true from mentioned
		) s
		group by id
	), message_ids as (
		select id from reached where type in ('message.created', 'message.answered')
		union
		select md.message_id::text
		from message_deliveries md
		where md.session_id = $1 and md.state in ` + model.DeliveryTakenStatesSQL + `
		  and md.created_at >= $2 - interval '1 hour'
	)
	select 'answer' as kind, a.id::text as id, (n.answer->>'at')::timestamptz as at,
	       null::timestamptz as edited_at, null::timestamptz as deleted_at, null::jsonb as deleted_by,
	       null::jsonb as author, null::text as body, null::text as dispatch_words, n.answer, a.answer as row_answer,
	       (select count(distinct ae.answer->>'at') from answer_events ae
	         where ae.id = x.id and ae.actor->>'kind' = 'user' and ae.actor->>'id' = ae.answer->>'user') > 1 as replaced,
	       a.id::text as ask_id, a.question as ask_question,
	       i.key as issue_key, i.title as issue_title, ar.project_key, ar.slug, ar.name,
	       false as mentioned, false as in_comment_thread, false as on_document,
	       false as replies_to_session,
	       ` + ownsAsk + ` as owns
	from answer_ids x
	join asks a on a.id::text = x.id
	cross join lateral (select ` + nodeAnswer + ` as answer) n
	left join issues i on i.key = a.issue_key
	left join artifacts ar on ar.id = a.artifact_id
	where n.answer is not null and (n.answer->>'at')::timestamptz >= $2
	union all
	select 'comment', c.id::text, c.created_at, c.edited_at, c.deleted_at, c.deleted_by, c.author, c.body, null, null, null, false,
	       a.id::text, a.question,
	       i.key, i.title, ar.project_key, ar.slug, ar.name,
	       x.mentioned,
	       c.reply_to is not null,
	       c.artifact_id is not null or c.anchor->>'artifact_id' is not null,
	       false,
	       ` + ownsAsk + `
	from comment_ids x
	join comments c on c.id::text = x.id
	left join asks a on a.id = c.ask_id
	left join issues i on i.key = c.issue_key
	left join artifacts ar on ar.id = c.artifact_id
	where c.author->>'kind' = 'user' and c.created_at >= $2
	union all
	select 'message', m.id::text, m.created_at, m.edited_at, m.deleted_at, m.deleted_by, m.author, m.body, null, null, null, false,
	       null, null,
	       i.key, i.title, null, null, null,
	       false, false, false,
	       exists (select 1 from messages p where p.id = m.in_reply_to
	               and p.author->>'kind' = 'session' and p.author->>'id' = $1),
	       false
	from message_ids x
	join messages m on m.id::text = x.id
	left join issues i on i.key = m.issue_key
	where m.author->>'kind' = 'user' and m.created_at >= $2
	union all
	-- A decline has no answer to take a time from, so it sits at the resolution's own time, and
	-- that time, not the event's, is what the window holds it to: the event is written moments
	-- after the resolution it carries.
	select 'declined', a.id::text, (x.resolution->>'at')::timestamptz, null, null, null,
	       x.resolution->'actor', x.resolution->>'reason', x.resolution->>'dispatch_words',
	       null, null, false,
	       a.id::text, a.question,
	       i.key, i.title, ar.project_key, ar.slug, ar.name,
	       false, false, false,
	       false,
	       ` + ownsAsk + `
	from decline_ids x
	join asks a on a.id::text = x.id
	left join issues i on i.key = a.issue_key
	left join artifacts ar on ar.id = a.artifact_id
	where (x.resolution->>'at')::timestamptz >= $2
	order by at desc, id desc`

// List lists what people wrote that Dispatch delivered to session since since, newest first by
// time, then by id descending: an ask's current answer, a comment or a message at its current
// body with the mark of a delete, or a person's decline of an ask, each (kind, id) once (query).
//
// An item reached several ways carries one relation, the most specific to the session, in this
// order of precedence. A session owns an ask when it wrote the ask or follows it (ownsAsk), and
// only an owner is told that an ask is its own. A comment is replied_on_your_ask when it is in the
// thread of an ask the session owns, else mentioned_you when a mention delivered it to the session
// (it named this session in particular), else replied_in_a_comment_thread when it replies to
// another comment, else commented_on_a_document when it is on a document (anchored on one, or on
// a project document), else commented_on_an_issue. A message is replied_to_your_message when it
// replies to a message the session wrote, else sent_you_a_message. An answer is answered_your_ask
// for an owner and answered_an_ask for any other session. A decline is declined_your_ask for an
// owner, with the words Dispatch wrote for the asking agent, and declined_an_ask, without those
// words, for any other session. The follower route delivers an ask's answer, decline and replies
// to its followers; the session route, the claimant route and a credit on the issue's primary
// document each deliver some of them to sessions that do not own the ask.
func List(ctx context.Context, pool *store.Pool, session string, since time.Time) ([]model.SessionAnswer, error) {
	rows, err := pool.Query(ctx, query, session, since, contracts.LegacyAgentSubject(session))
	if err != nil {
		return nil, fmt.Errorf("read session %q answers: %w", session, err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.SessionAnswer, error) {
		return scan(row, session)
	})
	if err != nil {
		return nil, fmt.Errorf("read session %q answers: %w", session, err)
	}
	if items == nil {
		items = []model.SessionAnswer{}
	}
	return items, nil
}

// scan decodes one query row into the item it is to session.
func scan(row pgx.Row, session string) (model.SessionAnswer, error) {
	var (
		item                                               model.SessionAnswer
		author, answer, rowAnswer, deletedBy               []byte
		deletedAt                                          *time.Time
		body, dispatchWords, askID, askQuestion            *string
		issueKey, issueTitle, docProject, docSlug, docName *string
		mentioned, inCommentThread, onDocument             bool
		repliesToSession, owns                             bool
	)
	if err := row.Scan(
		&item.Kind, &item.ID, &item.At, &item.EditedAt, &deletedAt, &deletedBy,
		&author, &body, &dispatchWords, &answer, &rowAnswer, &item.Replaced,
		&askID, &askQuestion, &issueKey, &issueTitle, &docProject, &docSlug, &docName,
		&mentioned, &inCommentThread, &onDocument, &repliesToSession, &owns,
	); err != nil {
		return model.SessionAnswer{}, err
	}
	// The mark of a deleted post, as every other read of one carries it: the time in UTC and
	// the actor that deleted it, from the one decode that stands behind them all.
	var err error
	if item.DeletedAt, item.DeletedBy, err = model.DeletedPostMark(deletedAt, deletedBy); err != nil {
		return model.SessionAnswer{}, fmt.Errorf("%s %s: %w", item.Kind, item.ID, err)
	}
	switch item.Kind {
	case model.SessionAnswerKindAnswer:
		var current model.AskAnswer
		if err := json.Unmarshal(answer, &current); err != nil {
			return model.SessionAnswer{}, fmt.Errorf("decode ask %s answer: %w", item.ID, err)
		}
		// The node holds no act: the answer carries the row's where the row holds the node's own
		// answer, the same person's at the same time, as api's askNodeAnswer.applyTo lays it.
		if current.Act == nil && len(rowAnswer) > 0 {
			var row model.AskAnswer
			if err := json.Unmarshal(rowAnswer, &row); err != nil {
				return model.SessionAnswer{}, fmt.Errorf("decode ask %s row answer: %w", item.ID, err)
			}
			if row.At.Equal(current.At) && model.CanonicalLogin(row.User) == current.User {
				current.Act = row.Act
			}
		}
		item.Relation = model.SessionAnswerRelationAnsweredAnAsk
		if owns {
			item.Relation = model.SessionAnswerRelationAnsweredYourAsk
		}
		item.At = current.At
		// The answer's own act, which a comment's or a message's author carries in its stored
		// JSON: the person answered, and the agent that answered for them is named beside them.
		item.Author = model.Actor{Kind: "user", ID: current.User, Act: current.Act}
		if current.Text != nil {
			item.Text = *current.Text
		}
		item.Selected = current.Selected
	case model.SessionAnswerKindDeclined:
		// The person who declined, with the agent that acted for them beside them where the
		// resolution's actor carries one, as an answer's author carries its act.
		if err := json.Unmarshal(author, &item.Author); err != nil {
			return model.SessionAnswer{}, fmt.Errorf("decode ask %s resolution actor: %w", item.ID, err)
		}
		// The person's own note, which they need not write. Nobody chose an option, so the item
		// draws none.
		if body != nil {
			item.Text = *body
		}
		item.Relation = model.SessionAnswerRelationDeclinedAnAsk
		// The words Dispatch wrote for the asking agent go to the ask's owners alone: they tell
		// the asker to decide its next step, which is no other session's to decide.
		if owns {
			item.Relation = model.SessionAnswerRelationDeclinedYourAsk
			if dispatchWords != nil {
				item.DispatchWords = *dispatchWords
			}
		}
	case model.SessionAnswerKindComment, model.SessionAnswerKindMessage:
		if err := json.Unmarshal(author, &item.Author); err != nil {
			return model.SessionAnswer{}, fmt.Errorf("decode %s %s author: %w", item.Kind, item.ID, err)
		}
		item.Text = *body
		switch {
		case item.Kind == model.SessionAnswerKindMessage && repliesToSession:
			item.Relation = model.SessionAnswerRelationRepliedToYourMessage
		case item.Kind == model.SessionAnswerKindMessage:
			item.Relation = model.SessionAnswerRelationSentYouMessage
		// A reply in the thread of an ask the session owns outranks a mention: it is a reply on
		// the session's own ask however it arrived. owns is false for a comment outside an ask's
		// thread, whose ask the comment arm's left join leaves null (ownsAsk), so owns alone says
		// the comment is in such a thread. Any other session reads the reason that delivered it.
		case owns:
			item.Relation = model.SessionAnswerRelationRepliedOnYourAsk
		case mentioned:
			item.Relation = model.SessionAnswerRelationMentionedYou
		case inCommentThread:
			item.Relation = model.SessionAnswerRelationRepliedInCommentThread
		case onDocument:
			item.Relation = model.SessionAnswerRelationCommentedOnDocument
		default:
			item.Relation = model.SessionAnswerRelationCommentedOnIssue
		}
	default:
		return model.SessionAnswer{}, fmt.Errorf("session answer %s has kind %q", item.ID, item.Kind)
	}
	item.At = item.At.UTC()
	if item.EditedAt != nil {
		edited := item.EditedAt.UTC()
		item.EditedAt = &edited
	}
	if askID != nil {
		item.Ask = &model.SessionAnswerAsk{ID: *askID, Question: *askQuestion}
	}
	item.Owner, item.Ref, item.Href = place(item.Kind, item.ID, session, issueKey, issueTitle, docProject, docSlug, docName)
	return item, nil
}

// place is where an item lives: its owner, its dispatch:// address and its relative deep link, in
// the shapes DISPATCH_HREF_REFERENCES (packages/contracts/src/dispatch-href.ts) lists - under its
// issue, or under the project document holding it. A message on no issue is a direct message to
// the session: it has no owner and no address, and links to the session's conversation.
func place(kind, id, session string, issueKey, issueTitle, docProject, docSlug, docName *string) (*model.OpenAskOwner, *string, string) {
	var segment string
	switch kind {
	// A decline is about an ask, as an answer is, so it lives at that ask's address.
	case model.SessionAnswerKindAnswer, model.SessionAnswerKindDeclined:
		segment = "ask"
	case model.SessionAnswerKindComment:
		segment = "comment"
	case model.SessionAnswerKindMessage:
		segment = "message"
	}
	switch {
	case issueKey != nil:
		ref := "dispatch://" + *issueKey + "/" + segment + "/" + id
		return &model.OpenAskOwner{Issue: &model.OpenAskIssue{Key: *issueKey, Title: *issueTitle}},
			&ref, "/issues/" + *issueKey + "/" + segment + "s/" + id
	case docProject != nil:
		ref := "dispatch://" + *docProject + "/artifact/" + *docSlug + "/" + segment + "/" + id
		return &model.OpenAskOwner{Document: &model.OpenAskDocument{Project: *docProject, Slug: *docSlug, Name: *docName}},
			&ref, "/projects/" + *docProject + "/documents/" + *docSlug + "?" + segment + "=" + id
	default:
		return nil, nil, "/agents/" + url.PathEscape(session) + "/live"
	}
}
