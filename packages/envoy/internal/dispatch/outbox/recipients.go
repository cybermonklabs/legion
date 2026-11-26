package outbox

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// documentCreditsSQL is the body of a CTE that lists, for each row of a relation
// documents(key, artifact_id) in scope, every actor that document credits: the one that created it
// (artifacts.created_by) and every one a version lists (artifact_versions.authors). Each row has
// the key, the author's actor JSON and a rank: the creator first, then the versions in number
// order, and each version's authors in its list's order. An artifact that is not a document
// credits no one. A version's authors are a JSON array, or JSON null where a writer encoded a nil
// list, which every Go reader of the column decodes as no authors; the CTE reads null the same way
// rather than failing on jsonb_array_elements' refusal of a scalar. loadDocumentSessionAuthors,
// loadCommentRecipients and loadPrimaryDocumentAuthors all read it, so a document credits the same
// sessions for each.
const documentCreditsSQL = `
	select d.key, a.created_by as author, array[1, 0, 0]::bigint[] as rank
	from documents d
	join artifacts a on a.id = d.artifact_id and a.kind = 'doc'
	union all
	select d.key, credit.author, array[1, v.number, credit.position]::bigint[]
	from documents d
	join artifacts a on a.id = d.artifact_id and a.kind = 'doc'
	join artifact_versions v on v.artifact_id = a.id
	cross join lateral jsonb_array_elements(nullif(v.authors, 'null'::jsonb))
		with ordinality as credit(author, position)
`

// sessionCreditSQL keeps the credits that name a session by its id. A person has no Envoy topic,
// so people are left out, and so is a session credit with no id, which has no topic either.
const sessionCreditSQL = `author->>'kind' = 'session' and coalesce(author->>'id', '') <> ''`

// commentRecipientsQuery is walkCommentThreads' one statement. $1 is the comment ids, $2
// maxCommentThreadDepth. The walk up (up) finds each comment's thread root, and the walk down
// (thread) reads every comment under that root, for legacy nested rows. The walk stops at two
// limits.
//
//   - The visited guard stops a cycle at its first repeat. comments.reply_to carries no
//     acyclicity constraint, so the walk up carries the ids it has visited (visited) and never
//     steps onto one again (c.id <> all(u.visited)): each row also carries its depth, so a lap of
//     a cycle would otherwise be a row the union has not seen, and the walk would go round until
//     the depth bound stopped it. A cycle has no reply_to-is-null row and so gives no root. The
//     walk down needs no such guard: it starts from a root, whose reply_to is null, and each row
//     it reaches replies to the row it came from, so it reaches no row twice.
//   - The depth bound stops a chain of replies that never repeats a row: neither walk takes more
//     than $2 steps, so a chain that deep is answered whole and a longer one is cut at the bound.
//     A cut is a row of its own in the answer: the key, no session, and cut set. The walk up was
//     cut when a row it reached at the bound still replies to a row it has not visited - a row
//     that replies to one it has visited closes a cycle, which cuts nothing (reply_to <>
//     all(visited)) - and the walk down when a row it reached at the bound has a reply.
//
// The thread's members are both walks' comments, so a cycle or a cut chain still reaches the
// authors of the comments the walk up visited, the replied-to comment's among them. The thread's
// document is the root's anchor's artifact, or the root's own artifact for a comment on a whole
// project artifact, since a reply carries no anchor.
//
// Each recipient row says, beside the key and the session's id, where the read found that
// session: a member of the thread ranks array[0, ...] and a credit of the document array[1, ...],
// and the group's min(rank) decides, so a session that is both comes out a member. A Postgres
// array is 1-indexed, so that first element is (min(rank))[1], which the row carries as
// from_thread. The caller keeps a member of the thread whatever the issue's claim is and leaves a
// credit out on an issue a session claims (addAuthorRoutes), by the rule takesClaimantRoute
// states.
const commentRecipientsQuery = `
	with recursive
	up as (
		select c.id as key, c.id, c.reply_to, c.anchor, c.artifact_id, 0 as depth, array[c.id] as visited
		from comments c
		where c.id = any($1::uuid[])
		union
		select u.key, c.id, c.reply_to, c.anchor, c.artifact_id, u.depth + 1, u.visited || c.id
		from up u
		join comments c on c.id = u.reply_to
		where u.depth < $2 and c.id <> all(u.visited)
	),
	roots as (
		select key, id, coalesce((anchor->>'artifact_id')::uuid, artifact_id) as artifact_id
		from up
		where reply_to is null
	),
	thread as (
		select key, id, 0 as depth from roots
		union
		select t.key, c.id, t.depth + 1
		from thread t
		join comments c on c.reply_to = t.id
		where t.depth < $2
	),
	members as (
		select key, id from up
		union
		select key, id from thread
	),
	documents as (
		select key, artifact_id from roots where artifact_id is not null
	),
	credited as (
		select m.key, c.author,
			array[0, row_number() over (partition by m.key order by c.reply_to is null desc, c.created_at, c.id), 0]::bigint[] as rank
		from members m
		join comments c on c.id = m.id
		union all
		` + documentCreditsSQL + `
	),
	cuts as (
		select key from up where depth = $2 and reply_to is not null and reply_to <> all(visited)
		union
		select t.key from thread t
		where t.depth = $2 and exists (select 1 from comments c where c.reply_to = t.id)
	)
	select key::text, session_id, from_thread, cut
	from (
		select key, author->>'id' as session_id, (min(rank))[1] = 0 as from_thread, false as cut, min(rank) as rank
		from credited
		where ` + sessionCreditSQL + `
		group by key, author->>'id'
		union all
		select key, null, false, true, null
		from cuts
	) walked
	order by key, rank
`

// threadRecipient is one session a person's reply under a comment reaches, and where the read
// found it: fromThread for a session that wrote a comment in the thread, false for one the
// thread's document credits alone (commentRecipientsQuery, addAuthorRoutes).
type threadRecipient struct {
	sessionID  string
	fromThread bool
}

// loadCommentRecipients reads, in one query for a whole batch (or for half of a refused one's ids,
// when loadRecipients runs it again in halves), the sessions a person's reply under each comment
// of commentIDs reaches on their own topics, keyed by that comment's id. commentIDs are canonical
// uuids (validIDs): a reply_to that is no uuid names no comment and is never read. They are every
// session that wrote a comment in the comment's thread (its root first, then each reply in the
// order it was written), marked fromThread, then every session the thread's document credits
// (documentCreditsSQL; none when the thread is on no document), each once and marked fromThread
// only where the thread names it too. A session the thread's document credits alone is left out
// by the caller where the comment is a person's on an issue a session claims (addAuthorRoutes),
// by the rule takesClaimantRoute states. People are left out, since a person has no Envoy topic.
// A comment that has no row has no entry. The actor and the sessions the reply's mentions reach
// are skipped by the caller too.
//
// A comment whose walk the depth bound cut (walkCommentThreads) is logged, one line naming it and
// the bound: the reply then reaches only the sessions the walk found inside the bound, and the
// log is the one place that says so.
func loadCommentRecipients(ctx context.Context, db queryer, commentIDs []string) (map[string][]threadRecipient, error) {
	if len(commentIDs) == 0 {
		return map[string][]threadRecipient{}, nil
	}
	recipients, cut, err := walkCommentThreads(ctx, db, commentIDs)
	if err != nil {
		return nil, err
	}
	for _, comment := range cut {
		slog.Warn("dispatch outbox: the comment thread walk stopped at its depth bound, so a reply under the comment reaches only the sessions the walk found inside it",
			"comment", comment, "depth", maxCommentThreadDepth)
	}
	return recipients, nil
}

// walkCommentThreads runs commentRecipientsQuery over commentIDs, one statement for a whole
// batch, and answers each comment's recipients in the order of their rows, and the comments whose
// walk the depth bound cut, in the order of their ids. A cut comment's recipients are those the
// walk reached inside the bound.
func walkCommentThreads(ctx context.Context, db queryer, commentIDs []string) (map[string][]threadRecipient, []string, error) {
	rows, err := db.Query(ctx, commentRecipientsQuery, commentIDs, maxCommentThreadDepth)
	if err != nil {
		return nil, nil, fmt.Errorf("load comment recipients: %w", err)
	}
	recipients := map[string][]threadRecipient{}
	var cut []string
	var key string
	var sessionID *string
	var fromThread, stopped bool
	if _, err := pgx.ForEachRow(rows, []any{&key, &sessionID, &fromThread, &stopped}, func() error {
		if stopped {
			cut = append(cut, key)
			return nil
		}
		recipients[key] = append(recipients[key], threadRecipient{sessionID: *sessionID, fromThread: fromThread})
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("load comment recipients: %w", err)
	}
	return recipients, cut, nil
}

// primaryDocumentAuthorsQuery is loadPrimaryDocumentAuthors' one statement. $1 is the issue keys.
// The documents are the issues' primary artifacts (artifacts.is_primary, one per issue at most).
const primaryDocumentAuthorsQuery = `
	with documents as (
		select issue_key as key, id as artifact_id
		from artifacts
		where issue_key = any($1::text[]) and is_primary
	),
	credited as (` + documentCreditsSQL + `)
	select key, author->>'id'
	from credited
	where ` + sessionCreditSQL + `
	group by key, author->>'id'
	order by key, min(rank)
`

// loadPrimaryDocumentAuthors reads, in one query for a whole batch, the sessions the primary
// document of each issue of issueKeys credits (documentCreditsSQL), keyed by the issue's key: its
// creator first, then each version's authors, each once. People, and session credits with no id,
// are left out (sessionCreditSQL). An issue with no primary document, or with no row, has no entry.
func loadPrimaryDocumentAuthors(ctx context.Context, db queryer, issueKeys []string) (map[string][]string, error) {
	if len(issueKeys) == 0 {
		return map[string][]string{}, nil
	}
	return loadKeyedSessions(ctx, db, "load primary document authors", primaryDocumentAuthorsQuery, issueKeys)
}

// loadKeyedSessions runs query, one statement for a whole batch whose every row is a key and a
// session id, and returns each key's sessions in the order of its rows. what names the load in its
// error. It reads the two recipient reads whose rows carry no mark beside the session; the
// comment recipients' rows also say where the walk found each session, and whether the walk was
// cut (walkCommentThreads).
func loadKeyedSessions(ctx context.Context, db queryer, what, query string, args ...any) (map[string][]string, error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	sessions := map[string][]string{}
	var key, sessionID string
	if _, err := pgx.ForEachRow(rows, []any{&key, &sessionID}, func() error {
		sessions[key] = append(sessions[key], sessionID)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return sessions, nil
}

// takesPrimaryAuthors reports whether a person's comment event reaches the sessions of its issue's
// primary document (addPrimaryDocumentAuthors): it is a comment.created, a comment.edited or a
// comment.deleted, it notifies (only a person's comment does, events.Broker.Notify), it names an
// issue, and that issue has no route and no session claimant (routing), either of which hears it
// in their place. The comment's thread does not matter.
func takesPrimaryAuthors(event model.Event, routing routing) bool {
	kind := event.Type
	if !event.Notify || (kind != "comment.created" && kind != "comment.edited" && kind != "comment.deleted") {
		return false
	}
	if event.IssueKey == nil || routing.claimant != "" {
		return false
	}
	return routing.route == nil || *routing.route == ""
}

// takesClaimantRoute reports whether an event reaches the session that holds its issue's claim on
// that session's own topic (addClaimantRoute): it names an issue, its actor is a person, the
// batch read a session claimant, and its type is one of the answers, comments and messages a
// claimant is told about. addAuthorRoutes reads the same predicate for its claimed test, so the
// credits of a document are left out exactly where the claimant was added: the credits of a
// document are
// not a second reason to deliver what that claimant already hears on its own topic. This is
// where that rule is decided, and its one statement among this package's reads; the places that
// apply it point here.
func takesClaimantRoute(event model.Event, claimant string) bool {
	// A project document's event names no issue, so there is no claim to read.
	if event.IssueKey == nil || event.Actor.Kind != "user" || claimant == "" {
		return false
	}
	switch event.Type {
	case "ask.answered", "ask.handed_back", "ask.resolved",
		"comment.created", "comment.edited", "comment.deleted", "comment.resolved", "comment.reopened",
		"message.created", "message.answered", "message.edited", "message.deleted":
		return true
	}
	return false
}

// recipientFilter keeps, of the sessions one route of an event reaches on their own topics, each
// one once, and none that is the event's actor (R3's own-event rule). Whether a session is left to
// its own mention of a new comment is the destination build's own rule, asked of the comment's
// attempt rows where every copy bound for a session's own topic asks it
// (destinations.addSession, destinations.leftToMention). addAuthorRoutes,
// addPrimaryDocumentAuthors and the sessions a comment's mentions name (eventDestinations) all
// filter through this.
type recipientFilter struct {
	actor model.Actor
	seen  map[string]bool
}

func newRecipientFilter(event model.Event) *recipientFilter {
	return &recipientFilter{actor: event.Actor, seen: map[string]bool{}}
}

// keep reports whether sessionID is a recipient, the first time the filter is asked about it.
func (f *recipientFilter) keep(sessionID string) bool {
	if f.seen[sessionID] || f.actor.SameAs(model.Actor{Kind: "session", ID: sessionID}) {
		return false
	}
	f.seen[sessionID] = true
	return true
}
