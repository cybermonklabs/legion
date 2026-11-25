package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
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

// commentRecipientsQuery is loadCommentRecipients' one statement. $1 is the comment ids, $2
// maxCommentThreadDepth. The walk up (up) finds each comment's thread root. comments.reply_to has
// no acyclicity constraint, so the walk unions on the visited row and stops at the depth bound: a
// cycle then has no reply_to-is-null row and gives no root, rather than spinning. The walk down
// (thread) reads every comment under that root, to the same depth, for legacy nested rows. The
// thread's members are both walks' comments, so a cycle still reaches the authors of the comments
// the walk up visited, the replied-to comment's among them. The thread's document is the root's
// anchor's artifact, or the root's own artifact for a comment on a whole project artifact, since
// a reply carries no anchor.
//
// Each row says, beside the key and the session's id, where the read found that session: a
// member of the thread ranks array[0, ...] and a credit of the document array[1, ...], and the
// group's min(rank) decides, so a session that is both comes out a member. A Postgres array is
// 1-indexed, so that first element is (min(rank))[1], which the row carries as from_thread. The
// caller keeps a member of the thread whatever the issue's claim is. It leaves a credit out on an
// issue a session claims (addAuthorRoutes), by the rule takesClaimantRoute states, and on an issue
// nobody routes or claims where a member of the thread is live or unresponsive (takesThreadRule,
// creditFilter). The members are also the sessions whose state that rule reads (threadSessions).
const commentRecipientsQuery = `
	with recursive
	up as (
		select c.id as key, c.id, c.reply_to, c.anchor, c.artifact_id, 0 as depth
		from comments c
		where c.id = any($1::uuid[])
		union
		select u.key, c.id, c.reply_to, c.anchor, c.artifact_id, u.depth + 1
		from up u
		join comments c on c.id = u.reply_to
		where u.depth < $2
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
	)
	select key::text, author->>'id', (min(rank))[1] = 0 as from_thread
	from credited
	where ` + sessionCreditSQL + `
	group by key, author->>'id'
	order by key, min(rank)
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
// by the rule takesClaimantRoute states, and where the comment is a person's reply on an issue
// nobody routes or claims whose thread has a live or unresponsive member (takesThreadRule,
// creditFilter); the members marked fromThread are the sessions that rule reads the state of
// (threadSessions). People are left out, since a person has no Envoy topic. A comment that has no
// row has no entry. The actor and the sessions the reply's mentions reach are skipped by the
// caller too.
func loadCommentRecipients(ctx context.Context, db queryer, commentIDs []string) (map[string][]threadRecipient, error) {
	if len(commentIDs) == 0 {
		return map[string][]threadRecipient{}, nil
	}
	return loadThreadRecipients(ctx, db, "load comment recipients", commentRecipientsQuery, commentIDs, maxCommentThreadDepth)
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
// error.
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

// loadThreadRecipients runs query, loadCommentRecipients' one statement for a whole batch, whose
// every row is a key, a session id and whether the read found that session in the thread, and
// returns each key's recipients in the order of its rows. The other two recipient reads carry no
// such mark and run through loadKeyedSessions. what names the load in its error.
func loadThreadRecipients(ctx context.Context, db queryer, what, query string, args ...any) (map[string][]threadRecipient, error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	recipients := map[string][]threadRecipient{}
	var key, sessionID string
	var fromThread bool
	if _, err := pgx.ForEachRow(rows, []any{&key, &sessionID, &fromThread}, func() error {
		recipients[key] = append(recipients[key], threadRecipient{sessionID: sessionID, fromThread: fromThread})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return recipients, nil
}

// takesPrimaryAuthors reports whether a person's comment event reaches the sessions of its issue's
// primary document (addPrimaryDocumentAuthors): it is a comment.created, a comment.edited or a
// comment.deleted, it notifies (only a person's comment does, events.Broker.Notify), it names an
// issue, and that issue has no route and no session claimant (routing), either of which hears it
// in their place. A reply in a thread whose session counts reaches only those of them a mention of
// the reply decides: the thread rule leaves out every other session that only a document credits
// (takesThreadRule, creditFilter.copyOf).
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

// takesThreadRule reports whether the thread rule may leave the credits of a document out of
// event: takesPrimaryAuthors holds for it, and it is a person's reply in a thread or the edit or
// the delete of one - a reply under a comment (reply_to) or a comment in an ask's thread (ask_id).
// Whether the rule does is whether a session of that thread counts (routing.threadHeard,
// threadHearing.hear). A person's reply answers the thread, so the sessions of the thread hear it,
// and a session that only wrote or edited a document the reply is about does not. The rule takes
// copies away and adds none.
func takesThreadRule(event model.Event, routing routing) bool {
	if !takesPrimaryAuthors(event, routing) {
		return false
	}
	return payloadString(event.Payload, "ask_id") != "" || payloadString(event.Payload, "reply_to") != ""
}

// threadSessions is the sessions of the thread event is in, where the thread rule may apply
// (takesThreadRule): for a comment in an ask's thread the ask's followers, which are the sessions
// addFollowerRoutes reaches, and otherwise the sessions that wrote a comment in the thread it
// replies under (threadRecipient.fromThread), which addAuthorRoutes reaches. The actor is left
// out. These are the sessions the follower and author rules reach and no other, so an asker that
// stopped following its ask is not one. A read that failed names none: the event's destinations
// end at that read all the same.
func threadSessions(event model.Event, routing routing, look lookup) []string {
	if !takesThreadRule(event, routing) {
		return nil
	}
	var members []string
	if askID := payloadString(event.Payload, "ask_id"); askID != "" {
		followers, err := look.followers(askID)
		if err != nil {
			return nil
		}
		members = followers
	} else {
		recipients, err := look.commentRecipients(payloadString(event.Payload, "reply_to"))
		if err != nil {
			return nil
		}
		for _, recipient := range recipients {
			if recipient.fromThread {
				members = append(members, recipient.sessionID)
			}
		}
	}
	sessions := make([]string, 0, len(members))
	for _, sessionID := range members {
		if !event.Actor.SameAs(model.Actor{Kind: "session", ID: sessionID}) {
			sessions = append(sessions, sessionID)
		}
	}
	return sessions
}

// RegistryReader reads one session's record from Envoy's registry, unresponsive sessions
// included, as envoy.Client.Session does: a record's State is live, unresponsive or gone,
// envoy.ErrNotFound is a session the registry holds no record of or one that stopped, an error
// that is envoy.ErrUnreachable, envoy.ErrAccountToken or a deadline (registryUnanswered) is the
// registry not answering, and any other error is the registry's answer that it cannot read that
// one session's record, such as the 500 the listener gives for an interest record it cannot read.
type RegistryReader interface {
	Session(ctx context.Context, sessionID string) (envoy.Session, error)
}

// registryReadsAtOnce is how many reads of Envoy's registry one batch has open at once
// (countedSessions). A thread holds every session that wrote in it or follows its ask, so a read
// for each of them at once would open as many requests to the listener as the batch's threads
// hold sessions.
const registryReadsAtOnce = 8

// threadHearing is the thread rule's read of Envoy's registry for one batch (hearingFor): each
// event the rule takes whose thread names a session (takesThreadRule, threadSessions), with those
// sessions, and every session they name once. The read is made only for those events, and only
// their owners wait for it: every event of such an owner is resolved, planned and sent once the
// read answered (plan), so that the owner's events keep their order, while the batch plans and
// sends every other owner at once (publishBatch, sendChains). A registry that does not answer
// costs the owners it holds the client's own timeout, and holds no other event of the batch.
type threadHearing struct {
	registry RegistryReader
	look     lookup
	threads  []heardThread
	sessions []string
	// owners holds the owner of each event whose thread is read, and order lists them in the
	// batch's order. events are the batch's resolvable events of those owners: the events whose
	// destinations wait for the read.
	owners map[string]struct{}
	order  []string
	events []*batchEvent
}

// heardThread is one event the thread rule takes and the sessions of its thread.
type heardThread struct {
	event    *batchEvent
	sessions []string
}

// heardPlan is the plan of the owners that waited for Envoy's registry (threadHearing.plan): their
// chains, and those owners of them with no chain, which have nothing to send. sendChains takes it
// on beside the batch's other chains.
type heardPlan struct {
	chains []*chain
	idle   []string
}

// hearingFor is the thread rule's read for resolvable, the batch's events whose destinations can
// be read, as look answers their threads (threadSessions): nil, reading nothing and holding no
// owner, where deps holds no registry reader or no thread of resolvable names a session.
func hearingFor(deps Deps, resolvable []*batchEvent, look lookup) *threadHearing {
	if deps.Registry == nil {
		return nil
	}
	hearing := &threadHearing{registry: deps.Registry, look: look, owners: map[string]struct{}{}}
	asked := map[string]struct{}{}
	for _, event := range resolvable {
		sessions := threadSessions(event.event, event.routing, look)
		if len(sessions) == 0 {
			continue
		}
		hearing.threads = append(hearing.threads, heardThread{event: event, sessions: sessions})
		for _, sessionID := range sessions {
			if _, ok := asked[sessionID]; !ok {
				asked[sessionID] = struct{}{}
				hearing.sessions = append(hearing.sessions, sessionID)
			}
		}
		if _, ok := hearing.owners[event.owner]; !ok {
			hearing.owners[event.owner] = struct{}{}
			hearing.order = append(hearing.order, event.owner)
		}
	}
	if len(hearing.threads) == 0 {
		return nil
	}
	for _, event := range resolvable {
		if hearing.waits(event.owner) {
			hearing.events = append(hearing.events, event)
		}
	}
	return hearing
}

// waits reports whether owner's events wait for the read. A nil hearing holds no owner.
func (h *threadHearing) waits(owner string) bool {
	if h == nil {
		return false
	}
	_, ok := h.owners[owner]
	return ok
}

// hear decides the thread rule for each event the hearing reads a thread for (takesThreadRule).
// It reads the state of every session of those threads from Envoy's registry (countedSessions),
// and marks each event whose thread has a session the registry reads live or unresponsive
// (routing.threadHeard). That event's destinations then leave out every session that only a
// document credits (creditFilter). A session that is gone, stopped or unknown to the registry does
// not count, and neither does one whose read failed or was never made. So a thread none of whose
// sessions counts reaches the credits as before, and so does every thread where the registry does
// not answer or deps holds no reader (a nil hearing, which reads nothing).
//
// It reads each session once for the whole batch, after the batch's lookups, with no statement
// open and no pooled connection held, since the read is a call to another service. Nothing it read
// is stored: a retried event reads its thread again, so a retry reaches the credits where every
// session of the thread has gone since, or where the registry does not answer it.
func (h *threadHearing) hear(ctx context.Context) {
	if h == nil {
		return
	}
	counted := countedSessions(ctx, h.registry, h.sessions)
	for _, thread := range h.threads {
		thread.event.routing.threadHeard = slices.ContainsFunc(thread.sessions, func(sessionID string) bool {
			_, ok := counted[sessionID]
			return ok
		})
	}
}

// plan reads the hearing's threads (hear) on a goroutine of its own, then reads the destinations
// of the events that waited for it (readDestinations), resolves their role routes' holders with
// the roles the batch resolved already (resolveRoleHolders), plans their chains (planChains), and
// answers that plan on the channel it returns, once. The batch's own goroutine touches none of
// those events until it takes the plan, and is done with resolved and the hearing's lookups before
// plan is called. A nil hearing plans nothing and returns a nil channel.
func (h *threadHearing) plan(ctx context.Context, deps Deps, resolved map[string]roleResolution) <-chan heardPlan {
	if h == nil {
		return nil
	}
	planned := make(chan heardPlan, 1)
	go func() {
		h.hear(ctx)
		for _, event := range h.events {
			readDestinations(ctx, deps, event, h.look)
		}
		resolveRoleHolders(ctx, deps, h.events, resolved)
		chains := planChains(h.events)
		sending := map[string]struct{}{}
		for _, c := range chains {
			sending[c.owner] = struct{}{}
		}
		var idle []string
		for _, owner := range h.order {
			if _, ok := sending[owner]; !ok {
				idle = append(idle, owner)
			}
		}
		planned <- heardPlan{chains: chains, idle: idle}
	}()
	return planned
}

// countedSessions reads each of sessionIDs from registry, at most registryReadsAtOnce at once, and
// answers the ones it reads live or unresponsive. A session the registry holds no record of is no
// failure. A read the registry refuses for that one session (RegistryReader) counts nothing for
// that session alone, and is logged: the registry answers, so the other sessions are read as
// before. A read the registry does not answer (registryUnanswered) counts nothing either, is
// logged unless ctx ended, and no read starts after it, so the sessions not yet read count nothing
// either, as a registry that does not answer sends the credit copy. A registry that hangs then
// costs one read's wait, which the reader's own timeout bounds (envoy.Client's window), rather
// than that wait for every registryReadsAtOnce sessions of the batch's threads.
func countedSessions(ctx context.Context, registry RegistryReader, sessionIDs []string) map[string]struct{} {
	counted := map[string]struct{}{}
	var mu sync.Mutex
	unanswered := false
	var wg sync.WaitGroup
	slots := make(chan struct{}, registryReadsAtOnce)
	for index, sessionID := range sessionIDs {
		slots <- struct{}{}
		mu.Lock()
		stop := unanswered
		mu.Unlock()
		if stop || ctx.Err() != nil {
			if stop {
				slog.Warn("dispatch outbox: Envoy's registry did not answer, so the thread sessions not read yet do not count",
					"unread", len(sessionIDs)-index)
			}
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			session, err := registry.Session(ctx, sessionID)
			if err != nil {
				if errors.Is(err, envoy.ErrNotFound) {
					return
				}
				if registryUnanswered(err) {
					mu.Lock()
					unanswered = true
					mu.Unlock()
				}
				if ctx.Err() == nil {
					slog.Warn("dispatch outbox: a thread session's state could not be read from Envoy's registry, so it does not count",
						"session_id", sessionID, "error", err)
				}
				return
			}
			if session.State != envoy.StateLive && session.State != envoy.StateUnresponsive {
				return
			}
			mu.Lock()
			counted[sessionID] = struct{}{}
			mu.Unlock()
		})
	}
	wg.Wait()
	return counted
}

// registryUnanswered reports whether err, a failed read of Envoy's registry, is the registry not
// answering rather than its answer about one session: envoy.ErrUnreachable (no connection, a
// connection lost, the client's timeout, an answer whose body stopped after its headers or was cut
// short of its length, or a 502, 503 or 504 from in front of a listener that is gone or starting),
// envoy.ErrAccountToken (a read the client did not make, Dispatch's identity token not to be
// presented within the client's window - a metadata server that hangs, or a token file that
// cannot be read - which the read of every other session would repeat), or a deadline that ended
// the read. Any other failure is a status the listener chose for that session's record, or a
// whole body that does not decode, which says nothing about the other sessions.
func registryUnanswered(err error) bool {
	return errors.Is(err, envoy.ErrUnreachable) || errors.Is(err, envoy.ErrAccountToken) ||
		errors.Is(err, context.DeadlineExceeded)
}

// creditFilter is the rule a session that only a document credits is asked by, on a person's
// comment or its edit. Such a session is left out where the issue's session claimant hears the
// comment on its own topic (takesClaimantRoute), and where a session of the comment's thread counts
// (routing.threadHeard, takesThreadRule); it is kept everywhere else. A session the comment names
// whose copy of it a mention decides is kept all the same (dispatch://INFRA-1368's R2,
// dispatch://INFRA-3686's R6): one whose mention failed before the batch read it, which takes the
// copy under that mention's key (mentionDeliveries.fallbackKeys, destinations.addSession), and one
// a mention of the comment still decides (mentionDeliveries.decidesCopyFor), which stays in the
// list so that the mention rule runs for it - holding the event while that send is live, and
// keeping the fallback row the pass publishes when it fails (destinations.leftTo). A session the
// comment may yet name - an attempt of an @role: mention that names no session yet is still to
// be sent, and its sender may pin it to that session, or a person's Retry of that mention, which
// re-resolves the role, may - is neither: where the batch could name no holder for that
// attempt's role and the attempt is within unnamedForwardBound, its copy waits outside the
// event's list, holding nothing of the owner's later events, until the sender has pinned the
// attempt, and is decided then (creditWaits); past that bound, where the batch resolved the role
// to another session, or to this one with the attempt past its lease, and where no attempt of
// the role is owed, the copy is held as a fallback row that the pass publishes only should an
// attempt be pinned to that session and fail - the first attempt, or a Retry, whose failed
// receipt reopens the row the pass settled meanwhile (creditHeldAsARow,
// mentionDeliveries.creditCopyOf, reopenOnReceipt).
// The comment's edit follows the comment's copy, and its delete does too where the thread rule
// takes the delete (takesThreadRule): each reaches such a session where that session's mention
// failed, and waits while that mention, or the comment's copy that follows it, is still to be
// decided, within that same bound and holding nothing of the owner's later events; past the
// bound each reaches the session that got, or is owed, the comment's copy by the failed-mention
// rule, and no other credit (mentionDeliveries.editCreditCopyOf), and a session whose copy of the
// comment is a credit's row gets its missed edits from that row (creditRowEdits). A delete the
// thread rule does not take reads none of the comment's mentions (newCreditFilter): where the
// claimant route leaves the credits out, it leaves out every one of them. A withdrawn row sends
// no comment. addPrimaryDocumentAuthors and addAuthorRoutes both ask this filter.
type creditFilter struct {
	leavesOut bool
	// mentions is what the comment's mentions did, as the batch read them: the reading the
	// destination build holds for a new comment (eventDestinations), and for an edit, or a delete
	// the thread rule takes, the reading of the comment it changes, which only this filter reads
	// (followsComment).
	mentions mentionDeliveries
	// followsComment selects the original comment's failed-mention decision for a correction.
	followsComment bool
}

// creditCopy is what the credits filter makes of the copy of one session that only a document
// credits (creditFilter.copyOf).
type creditCopy int

const (
	// creditLeftOut: the event gives that session no copy.
	creditLeftOut creditCopy = iota
	// creditKept: the copy is built, and asks the mention rule as every other copy bound for a
	// session's own topic does (destinations.addSession).
	creditKept
	// creditWaits: a mention of the comment that is still to settle, or still to name a session,
	// decides whether that session gets the copy. The copy makes no send this read and the event
	// is read again; a new comment's copy, like an edit's, waits outside the list and holds
	// nothing, so none of the owner's later events waits on it (destinations.waitForCredits,
	// planChains).
	creditWaits
	// creditHeldAsARow: the comment names an @role: mention, and an attempt of it may yet be
	// pinned to this session or to another: one that names no session yet and still owes a send,
	// past the bound the copy waits such an attempt out for, or with its role resolved to another
	// session or to this one past the attempt's lease, or a Retry, which re-resolves the role. The
	// copy is held as a comment_fallback_copies row of that session's own topic, which holds
	// nothing of the event and which the fallback pass publishes under the key of that session's
	// failed mention, or settles with nothing sent until the receipt of such a mention reopens it
	// (destinations.holdForCredits, fallbackCopy.creditOf, reopenOnReceipt).
	creditHeldAsARow
)

// newCreditFilter reads the rule for event, once for a route that asks it, and reads the one thing
// that decides a session and is not in the destination build already: what the comment's mentions
// did. A new comment's are the reading the build holds (eventDestinations), and an edit's or a
// delete's are the reading of the comment it changes (lookup.mentionDeliveries), which this filter
// alone reads and which keys none of that event's copies. Either reading is the store's - the
// comment's attempts, and the comment_mentions rows they are opened under - and an edit's or a
// delete's is never its payload's: the API reads the edited comment back without its mentions
// (editComment, loadComment in internal/dispatch/api), so an edit's payload names none whatever
// the comment names, and the destination build reads a payload's mentions for a new comment
// alone. The filter reads every edit it can limit, and a delete only where the thread rule can
// apply. A comment with no attempts answers the empty reading, which names nobody. A delete
// fails every pending attempt in the delete's own write (deleteComment in internal/dispatch/api),
// so the reading includes those failures. The first pass can run before the registry is read
// (threadHearing.hear), and it must register every id the second pass reads (wanted). A read that
// failed ends the event's destinations, as every other lookup does.
//
// It is asked once per route and never per credit row: the first pass answers no row at all
// (wanted), so an ask made per row would register no id and the second pass would find nothing
// read for it.
func newCreditFilter(event model.Event, routing routing, look lookup, d *destinations) (creditFilter, error) {
	filter := creditFilter{
		leavesOut: takesClaimantRoute(event, routing.claimant) || routing.threadHeard,
		mentions:  d.mentions,
	}
	switch event.Type {
	case "comment.edited":
		if !filter.leavesOut && !takesThreadRule(event, routing) {
			return filter, nil
		}
	case "comment.deleted":
		if !takesThreadRule(event, routing) {
			return filter, nil
		}
	default:
		return filter, nil
	}
	if commentID := payloadString(event.Payload, "id"); commentID != "" {
		reading, err := look.mentionDeliveries(commentID)
		if err != nil {
			return creditFilter{}, err
		}
		filter.mentions, filter.followsComment = reading, true
	}
	return filter, nil
}

// copyOf reports what becomes of the copy of a session that only a document credits.
func (f creditFilter) copyOf(sessionID string) creditCopy {
	switch {
	case !f.leavesOut:
		return creditKept
	case f.followsComment:
		return f.mentions.editCreditCopyOf(sessionID)
	}
	return f.mentions.creditCopyOf(sessionID)
}

// takesClaimantRoute reports whether an event reaches the session that holds its issue's claim on
// that session's own topic (addClaimantRoute): it names an issue, its actor is a person, the
// batch read a session claimant, and its type is one of the answers, comments and messages a
// claimant is told about. addAuthorRoutes reads the same predicate for its claimed test, beside
// the thread rule (creditFilter), so the credits of a document are left out where the claimant was
// added: the credits of a document are
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

// recipientFilter keeps, of the sessions one route of a notifying event reaches on their own
// topics, each one once, and none that is the event's actor. Whether a session is left to its
// own mention of a new comment is the destination build's own rule, asked of the comment's
// attempt rows where every copy bound for a session's own topic asks it
// (destinations.addSession, destinations.leftToMention). addAuthorRoutes and
// addPrimaryDocumentAuthors both filter through this.
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
