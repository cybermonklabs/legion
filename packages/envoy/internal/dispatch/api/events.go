package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// maxEventPage is the most events one page of an issue's or a document's event log holds,
	// and its default.
	maxEventPage = 200
	// maxEventReplay is the most events one page of the global event log holds (readEventsAfterID):
	// the relay's read and GET /api/v1/events/log each page through the log at it, and it is that
	// route's default. An events subscription's catch-up pages the same read at maxLiveReplay
	// instead, the bound its resume is replayed within (replayThrough in live_events.go).
	maxEventReplay = 1000
)

const eventSelect = `select e.id, e.issue_key, e.artifact_id::text, coalesce(e.project_key, i.project_key, ar.project_key, ''), e.seq, e.type, e.actor, e.notify, e.created_at, e.payload
	from events e
	left join issues i on i.key = e.issue_key
	left join artifacts ar on ar.id = e.artifact_id`

// eventReadableSQL is the one read rule of an event, as SQL over the events row eventAlias names,
// for the viewer a statement binds at viewerParam (refs.VisibilityArg). Every read of the event
// log binds it - an issue's or a document's feed (readEvents) and the whole log, which GET
// /api/v1/events/log pages and the relay and the live socket's catch-up read (readEventsAfterID)
// - and the live socket asks it of each event it sends. An event passes:
//
//   - an event of an issue, a document or a project, when the viewer sees that project
//     (access.ProjectClause). user_state.updated and user_thread_state.updated are such events:
//     each write that appends one names the project of what it marked (projectOwner);
//   - a message event of no owner (message.created, message.answered, message.delivery and
//     message.accepted of a conversation on no issue), when the viewer may read the message's
//     conversation (access.PartyClause over the parties its node carries, its root's);
//   - user_agent_state.updated, the one other ownerless event about a person's own thread, built
//     with no owner (state.go, userAgentStateEvent), when the viewer may see the session it names
//     (access.SessionClause) and either its login is the viewer's person or the viewer is open:
//     the rule GET /api/v1/me/agents/state reads that person's state of the session by
//     (userAgentStatesSeenQuery), so a credential reads no state event of a session whose state it
//     does not read;
//   - any other ownerless event (priority.updated, access_group.changed), to an open viewer alone.
//
// An event whose one subject is a session (sessionSubjectEvents) passes, beside that, only when the
// viewer sees that session (access.SessionClause): a follow, a subscription, a delivery and its
// acceptance are the session's, so to a viewer the session is hidden from they are absent, as the
// session is (references_seen.go). An open viewer sees every session but one that acted in a
// project it does not see.
//
// A claim write's event (claimWriteEvents) passes only when the viewer sees the holder of the claim it
// left and of the one it replaced or cleared, each a session (access.SessionClause) or a person
// (access.PersonClause) (claimHolderSeenSQL): its actor is the claimant, so to a viewer that holder
// is hidden from the event is absent, as the claim is from every read of the issue. Every other
// issue event carries the claim in its payload, which the payload filter leaves out
// (payloadReferenceKeys).
func eventReadableSQL(eventAlias, viewerParam string) string {
	e := eventAlias
	project := `coalesce(` + e + `.project_key,
		(select event_issue.project_key from issues event_issue where event_issue.key = ` + e + `.issue_key),
		(select event_document.project_key from artifacts event_document where event_document.id = ` + e + `.artifact_id))`
	return `((case
		when ` + e + `.issue_key is not null or ` + e + `.artifact_id is not null or ` + e + `.project_key is not null
			then ` + access.ProjectClause(project, viewerParam) + `
		when ` + e + `.type like 'message.%'
			then ` + access.PartyClause(access.MessagePartiesSQL(`coalesce(`+e+`.payload->>'message_id', `+e+`.payload->>'id')`), viewerParam) + `
		when ` + e + `.type = 'user_agent_state.updated'
			then (coalesce(` + e + `.payload->>'login' = ` + access.ViewerPerson(viewerParam) + `, false)
					or ` + access.OpenClause(viewerParam) + `)
				and ` + access.SessionClause(e+`.payload->>'session_id'`, viewerParam) + `
		else ` + access.OpenClause(viewerParam) + `
	end) and (` + e + `.type not in (` + sessionSubjectEvents + `)
		or ((` + e + `.payload->>'session_id' is null or ` + access.SessionClause(e+`.payload->>'session_id'`, viewerParam) + `)
			and (coalesce(` + e + `.payload->>'target', '') not like 'session:%' or ` +
		access.SessionClause(`substr(`+e+`.payload->>'target', 9)`, viewerParam) + `)))
		and (case when ` + e + `.type in (` + claimWriteEvents + `)
			then ` + claimHolderSeenSQL(e+`.payload->'claim'`, viewerParam) + `
				and ` + claimHolderSeenSQL(e+`.payload->'previous_claim'`, viewerParam) + `
			else true end))`
}

// sessionSubjectEvents is every event type whose one subject is the session its payload names,
// by session_id or by a `session:<id>` target, as SQL literals.
const sessionSubjectEvents = `'ask.follower_added', 'ask.follower_removed', 'subscription.remove_requested',
	'subscription.removed', 'comment.delivery', 'message.delivery', 'message.accepted'`

// claimWriteEvents is every event type a claim write appends, whose payload names the claim it left
// (claim) and the one it replaced or cleared (previous_claim), as SQL literals.
const claimWriteEvents = `'issue.claimed', 'issue.released'`

// claimHolderSeenSQL passes the claim {actor, at} claimExpr gives, as jsonb, when the viewer bound
// at viewerParam sees its holder: a session by access.SessionClause, a person by
// access.PersonClause. No claim, and a holder of any other kind, passes.
func claimHolderSeenSQL(claimExpr, viewerParam string) string {
	kind, id := `(`+claimExpr+`->'actor'->>'kind')`, `(`+claimExpr+`->'actor'->>'id')`
	return `((` + kind + ` is distinct from 'session' or ` + access.SessionClause(id, viewerParam) + `)
				and (` + kind + ` is distinct from 'user' or ` + access.PersonClause(id, viewerParam) + `))`
}

// readableEvents is eventReadableSQL for who as the events and delivery packages' statements bind
// a read rule (events.Readable): over the events row named events, the viewer at $2.
func readableEvents(who model.Actor) events.Readable {
	return events.Readable{Where: readableEventsWhere, Viewer: refs.VisibilityArg(who)}
}

// readableEventsWhere is readableEvents' rule, the same text for every reader, built once.
var readableEventsWhere = eventReadableSQL("events", "$2")

// listIssueEvents answers an issue's activity feed under an ETag of metadata alone: every append
// to this feed advances the issue's last_seq in the append's own transaction (events.Broker), so
// the issue row this is the feed of, that column, the paging query that chooses the page and the
// variant lines both lists' ETags carry are the whole of what decides the page. The feed's
// validator is therefore read before the page is: a reader holding the ETag is answered 304 for
// one short statement, and one that is not is answered from the kept bytes when the cache holds
// them, having built, encoded and compressed nothing (list_answers.go).
//
// What the validator does not cover is the handful of fields an ask event's payload is given at
// read time from rows elsewhere (attachAskEventFields). The validator decides both answers this
// route gives: the 304 a reader holding the ETag is answered, and the kept bytes
// (listAnswers.answer) a reader who sends no validator at all is written, which is a fresh page
// load. So what follows is what either reader can be shown after it moved, and before this
// change a fresh load always read all of it current.
//
// Each field, and what moves it:
//
//   - referenced_by_count, the edges that point at the ask. An inbound backlink is counted on
//     the write that makes it, which is another issue's comment, message or document, and that
//     write moves that issue's sequence and no sequence of this one. It can be stale. The count
//     is also read as the acting person (refs.QueueBacklinkCounts, which drops an edge whose
//     source that person may not see), while this key names no person, so the kept bytes carry
//     the count the reader that filled them was given. Every node Dispatch writes today is
//     visible to everyone who may read its project (refs/visibility.go), so the two readings
//     are one; a node kind reserved for its author would have to be named in this key.
//   - The part ids of the options and the answer, and the ask's part revision, from ask_blocks.
//     Every write of an ask indexes its parts in the transaction that appends the ask's event
//     (queueAskParts, over docs.Ledger.QueueAskParts, which reviews.go reaches as the ask routes
//     do, through asks.go, ask_transitions.go, issue_ask_lifecycle.go and write_queue.go, and the
//     docs package's planAskPartsCounted for the asks it writes itself: a settlement's block asks,
//     an approval request's rewording and its carry's answer), so an ask's own writes move these
//     and last_seq together. The one write that does not is the indexing of an ask the index
//     owes - one written before its parts were indexed at all - which the block index pass makes
//     on no event (refs.IndexOwedItemBlocks, docs/block_index.go): until that pass runs, a
//     payload carries no part ids and no revision, and the page that was kept after it goes on
//     carrying none.
//   - anchor_artifact and approval_artifact: the project, slug, name and is_primary of the
//     document an ask anchors or asks approval of (attachAskDocumentArtifacts). A document's
//     rename, re-slug or primary flag moves the sequence of the issue that document belongs to,
//     which is this issue only when the ask names a document of its own. An ask that anchors a
//     document on another issue can be stale.
//   - opened_event_id is the id of the ask.opened event itself, written once and never moved.
//
// Each of those lasts exactly as long as the validator does, for the 304 and the kept bytes
// alike: the next event of this issue moves last_seq, which is a new ETag and so a new kept
// entry, and the read behind it attaches all of them again. An entry the cache evicts and a
// restart end it sooner. Those fields are an ask's own facts rather than the feed's, and the
// Inbox, the ask read and the issue read answer them fresh, from no kept list.
//
// An issue of a project the caller does not see is an issue no row holds (issueEventSequence),
// so its feed is the empty page under the ETag a missing key answers, and nothing is kept. The
// kept bytes of a feed the caller does see are filed under what the caller sees beside the key
// (keptViewOf), since referenced_by_count counts only the edges a reader sees.
func (s *server) listIssueEvents(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	options, err := parseEventListOptions(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	lastSeq, createdAt, issueHeld, err := s.issueEventSequence(r.Context(), who, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees := wantsDisplayTrees(r)
	etag := s.eventFeedETag(lastSeq, createdAt, options, trees)
	if answerNotModified(w, r, etag) {
		return
	}
	// While a project is private, a reader that does not see every object is handed no kept page
	// and keeps none (keepsListAnswerFor): what its payloads and counts name moves with rows the
	// key does not name.
	key, keep := listAnswerKeyFor(r, eventFeedRoute, r.PathValue("key")+keptViewOf(who), etag), keepsListAnswerFor(who)
	if keep && s.listAnswers.answer(w, key) {
		return
	}
	events, err := s.readEvents(r.Context(), who, issueOwner(r.PathValue("key")), options)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// One pass over the page: the reader takes each payload's texts as the shaping registers
	// them, reads their stored trees in one query and fills what the table did not hold.
	reader := s.readDisplayTrees(r.Context(), trees)
	attachEventTrees(events, reader)
	reader.resolve()
	encoded, err := encodeListAnswer(events)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.writeListAnswer(w, key, encoded, issueHeld && keep)
}

// keptViewOf is what a kept list answer read for who is filed under beside the owner the request
// names, for a list whose bytes turn on what the reader sees: an ask event's referenced_by_count
// counts only the edges whose source the reader sees (refs.QueueBacklinkCounts), so a feed kept
// for a reader of a private project is never handed to one who does not see it. An open viewer of
// a deployment that holds no private project sees every project, so it files under the owner
// alone, as every reader did before a project could be private.
func keptViewOf(who model.Actor) string {
	if seesEveryObject(who) {
		return ""
	}
	viewer := who.Viewer
	if viewer == nil {
		return "\x00closed"
	}
	return fmt.Sprintf("\x00%s\x00%s\x00%t\x00%s", viewer.Person, viewer.Session, viewer.Open, strings.Join(viewer.Sees, ","))
}

// eventFeedETag hashes what one page of an issue's activity feed is made of: the issue whose feed
// it is, that issue's newest event sequence and the query that pages it, closed with the lines
// both lists' ETags carry (listETag): one line per part, then the hash of them. It names no issue
// key: a validator is compared against the one URL that gave it.
//
// The issue is named by its created_at rather than its key, because the key is the one part of it
// that comes back. last_seq is a counter of the row, so a key taken by a new issue stands at
// sequences the issue before it stood at, and nothing else in the hash would tell the two feeds
// apart: a reader of the new issue would be answered 304, or written the bytes this server kept
// for the old one. That is what the browser harness does between scenarios - it empties `issues`
// and restarts each project's numbering, so every scenario's issues take the keys of the last
// one's (packages/dispatch/e2e/seed.ts) - and it is the reading a restored database gives a
// process that outlived the restore. The comment list is held to the same rule by a counter that
// never comes back with the key (migration 20261106020619); the feed's column does come back, so
// the row's birth stamp is hashed in beside it. It is the same fact, read the same way, that
// tells the event relay an event log that was reset under its cursor from one that merely went
// quiet (events.EventLogWasReset).
func (s *server) eventFeedETag(lastSeq int, createdAt time.Time, options eventListOptions, trees bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "created=%d\nlast_seq=%d\nafter=%d\nbefore=%d\nhas_before=%t\ndescending=%t\nlimit=%d\n",
		createdAt.UnixMicro(), lastSeq, options.after, options.before, options.hasBefore, options.descending, options.limit)
	for _, id := range options.ids {
		_, _ = fmt.Fprintf(hash, "id=%d\n", id)
	}
	return s.listETag(hash, trees)
}

// issueEventSequence is the newest event sequence of the issue key names and the created_at that
// tells that issue from the next one to take its key, with whether any issue holds that key at
// all. A key no issue holds reads zero for both, and its feed is the empty page readEvents
// answers rather than a refusal; the server keeps that page under no key, since the key is the
// path the caller spelled (writeListAnswer). An issue of a project who does not see reads as a
// key no issue holds. Every append to the issue advances the column in the append's own
// transaction (events/broker.go), and nothing moves the stamp, so this one statement is the whole
// of what the feed's validator reads.
func (s *server) issueEventSequence(ctx context.Context, who model.Actor, key string) (int, time.Time, bool, error) {
	var lastSeq int
	var createdAt time.Time
	err := s.deps.Store.Pool.QueryRow(ctx,
		`select last_seq, created_at from issues where key = $1 and `+access.ProjectClause("project_key", "$2"),
		key, refs.VisibilityArg(who)).Scan(&lastSeq, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("read the issue's event sequence: %w", err)
	}
	return lastSeq, createdAt, true, nil
}

// listEventLog pages the whole event log by id, whatever owns each event: an issue, a document,
// a project or nothing. It answers the events after ?after= (default 0) in ascending id order,
// at most ?limit= of them, of those the caller may read (eventReadableSQL): an event of a project
// it does not see, or of a conversation it may not read, is not in the log it pages.
//
// Ids are not commit order (events.Broker.Append), so a caller that pages by the id of the last
// row it was answered would step over a lower id still open and never see it. The page is
// therefore capped at the safe horizon, at or below which every event is final, the same cap and
// the same wait GET /api/v1/references?since= reads under (sinceHorizon): every event committed
// before the request stands at or below it, so paging to the end of this answer skips nothing.
func (s *server) listEventLog(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	after, err := parseNonNegativeInt(query.Get("after"), "after")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	limit, err := parseEventLimit(query.Get("limit"), maxEventReplay)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	through, err := s.sinceHorizon(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events, err := s.readEventsAfterID(r.Context(), who, after, through, limit)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, events)
}

func parseEventListOptions(r *http.Request) (eventListOptions, error) {
	query := r.URL.Query()
	hasBefore := query.Has("before")
	if hasBefore && strings.TrimSpace(query.Get("before")) == "" {
		return eventListOptions{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "before must be an integer from 0 to %d", maxIntegerColumn)
	}
	after, err := parseEventPosition(query.Get("after"), "after")
	if err != nil {
		return eventListOptions{}, err
	}
	before, err := parseEventPosition(query.Get("before"), "before")
	if err != nil {
		return eventListOptions{}, err
	}
	ids, err := parseEventIDs(query.Get("ids"))
	if err != nil {
		return eventListOptions{}, err
	}
	descending, err := parseEventOrder(query.Get("order"))
	if err != nil {
		return eventListOptions{}, err
	}
	limit, err := parseEventLimit(query.Get("limit"), maxEventPage)
	if err != nil {
		return eventListOptions{}, err
	}
	if query.Has("after") && (hasBefore || descending) {
		return eventListOptions{}, errorf(http.StatusBadRequest, "INVALID_QUERY", "after cannot be combined with before or order=desc")
	}
	return eventListOptions{
		after:      after,
		before:     before,
		hasBefore:  hasBefore,
		descending: descending || hasBefore,
		ids:        ids,
		limit:      limit,
	}, nil
}

type eventListOptions struct {
	after      int64
	before     int64
	hasBefore  bool
	descending bool
	ids        []int64
	limit      int
}

// readEvents reads one page of an issue's or a document's event log, of the events who may read
// (eventReadableSQL): an owner of a project who does not see answers the empty page, as an owner
// no row holds does.
func (s *server) readEvents(ctx context.Context, who model.Actor, owner owner, options eventListOptions) ([]model.Event, error) {
	var predicate, ownerValue string
	switch {
	case owner.IssueKey != nil:
		predicate, ownerValue = "e.issue_key = $1", *owner.IssueKey
	case owner.ArtifactID != nil:
		predicate, ownerValue = "e.artifact_id = $1", *owner.ArtifactID
	default:
		return nil, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	viewer := refs.VisibilityArg(who)
	if len(options.ids) > 0 {
		return s.readEventRows(ctx, who, eventSelect+`
			where `+predicate+` and e.id = any($2) and `+eventReadableSQL("e", "$3")+`
			order by e.seq asc
		`, ownerValue, options.ids, viewer)
	}
	if options.descending {
		if options.hasBefore {
			return s.readEventRows(ctx, who, eventSelect+`
				where `+predicate+` and e.seq < $2 and `+eventReadableSQL("e", "$4")+`
				order by e.seq desc limit $3
			`, ownerValue, options.before, options.limit, viewer)
		}
		return s.readEventRows(ctx, who, eventSelect+`
			where `+predicate+` and `+eventReadableSQL("e", "$3")+`
			order by e.seq desc limit $2
		`, ownerValue, options.limit, viewer)
	}
	return s.readEventRows(ctx, who, eventSelect+`
		where `+predicate+` and e.seq > $2 and `+eventReadableSQL("e", "$4")+`
		order by e.seq asc limit $3
	`, ownerValue, options.after, options.limit, viewer)
}

// readEventsAfterID reads one page of the global event log in id order: at most limit of the
// events whose ids are in (after, through] that who may read (eventReadableSQL). Ids are not
// commit order, so a reader that moves its cursor to a page's last row passes a through at or
// below the safe horizon (events.Horizon), where every event is final: no lower id the page
// skipped can commit later.
func (s *server) readEventsAfterID(ctx context.Context, who model.Actor, after, through int64, limit int) ([]model.Event, error) {
	return s.readEventRows(ctx, who, eventSelect+`
		where e.id > $1 and e.id <= $2 and `+eventReadableSQL("e", "$4")+`
		order by e.id asc limit $3
	`, after, through, limit, refs.VisibilityArg(who))
}

// eventLogUnbrokenThrough is the id before the first one missing from the event log counted up
// from 1, or its newest id when none is missing: every id at or below it is a committed event.
// It reads the whole log, so feedEvents asks it only of a log that was reset beneath the relay,
// whose ids restart at 1 (`truncate events restart identity`) and which is as short as the time
// since that reset.
func (s *server) eventLogUnbrokenThrough(ctx context.Context) (int64, error) {
	var through int64
	err := s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce(
			(select min(n) - 1 from (select id, row_number() over (order by id) as n from events) numbered where id <> n),
			(select max(id) from events),
			0)
	`).Scan(&through)
	return through, err
}

// eventFeedHeadWait bounds how long eventFeedHead waits on a process that has proven no horizon
// yet, while no mark it keeps counts (an event insert of this database open since before every
// mark). It waits nowhere else.
const eventFeedHeadWait = 2 * time.Second

// ErrEventHeadUnproven is eventFeedHead's answer when it has no id it can vouch for: GET
// /auth/whoami answers it, and only it, 503 EVENT_HEAD_UNAVAILABLE, which the page asks again for
// as long as it lasts. A failed read of the database or the horizon's pool is another error, which
// whoami answers 503 EVENT_HEAD_READ_FAILED and the page retries on its bounded rule.
var ErrEventHeadUnproven = errors.New("no event id is proven final yet: an event insert has been open since before every mark")

// eventFeedHead is the event_head GET /auth/whoami answers, which a page resumes its first events
// subscription from: an id at or below which every event is final, so a page that starts its reads
// after it and is replayed every event above it misses nothing that commits later.
//
// It answers at once (server.writerFloor): the newest of the server's marks that counts, a mark
// older than every transaction that holds a write lock on events, or the horizon already proven
// here or persisted by any process when none counts. A mark taken a moment ago counts as soon as the event
// writes open then have ended, which for this service's own writes is milliseconds, so the head
// trails the log's newest id by about that. A transaction that holds a transaction id and has
// inserted no event, here or in another database, keeps nothing waiting.
//
// It never answers 0 for a log it cannot vouch for: a process with no mark counting and no horizon
// proven or persisted (a first boot, a reset log) would send its page to replay the whole log. That
// one waits, eventFeedHeadWait at most, marking again, and past it answers ErrEventHeadUnproven,
// which is logged and which the page asks again for.
func (s *server) eventFeedHead(ctx context.Context) (int64, error) {
	floor, ok, _, err := s.writerFloor(ctx)
	if err != nil || ok || floor > 0 {
		return floor, err
	}
	deadline := time.Now().Add(eventFeedHeadWait)
	for wait := 10 * time.Millisecond; time.Now().Before(deadline); wait = min(2*wait, 250*time.Millisecond) {
		if !waitFor(ctx, wait) {
			return 0, ctx.Err()
		}
		if floor, ok, _, err = s.writerFloor(ctx); err != nil || ok || floor > 0 {
			return floor, err
		}
	}
	slog.Warn("dispatch: whoami has no event_head to answer: no horizon is proven yet and an event insert of this database has been open since before every mark", "wait", eventFeedHeadWait)
	return 0, ErrEventHeadUnproven
}

// personEventHead is the event_head GET /auth/whoami answers its caller (Background.EventHead):
// eventFeedHead's id, read as the head that caller's browser reads of the log (viewerEventHead), so
// no event of a project the caller does not see moves it. What the caller sees is resolved from
// the request's own credential, as every read resolves it (optionalActor, then readerFor), never
// from a login handed over by name. whoami answers the person its cookie names, so the caller is
// resolved from the request without its Authorization header: a token presented besides the
// cookie, which optionalActor would resolve first, is not read, and the head is the cookie's
// person's, as the login whoami answers is.
func (s *server) personEventHead(r *http.Request) (int64, error) {
	cookie := r.Clone(r.Context())
	cookie.Header.Del("Authorization")
	caller, _, err := s.optionalActor(cookie)
	if err != nil {
		return 0, err
	}
	through, err := s.eventFeedHead(r.Context())
	if err != nil {
		return 0, err
	}
	reader, err := s.readerFor(r.Context(), caller)
	if err != nil {
		// The error reaches /auth/whoami's log line, so it names no person
		// (TestAFailedEventHeadReadNamesNoPerson).
		return 0, fmt.Errorf("resolve what the signed-in person sees: %w", err)
	}
	return viewerEventHead(r.Context(), s.deps.Store.Pool, reader, through)
}

// viewerEventHead is the head of the event log reader reads at through, an id at or below which
// every event is final (eventFeedHead, or the relay's published cursor): the highest id at or
// below through of an event in reader's gap (eventGapSQL), one it may read or an access change
// that took a project from it, and 0 when there is none. No event of a project reader does not
// see moves it, and a resume from it is replayed exactly what a resume from through would be,
// since no event between the two is one the gap holds. An open viewer of a deployment that holds
// no private project reads every event (every project is public, every session and party it may
// see), so it reads through itself, without the read: the head every caller read before a project
// could be private (R9, TestWhoamiCarriesTheNewestEventID).
func viewerEventHead(ctx context.Context, q queryer, reader model.Actor, through int64) (int64, error) {
	if viewer := reader.Viewer; viewer != nil && viewer.Open && !viewer.AnyPrivate {
		return through, nil
	}
	var head int64
	if err := q.QueryRow(ctx, `
		select coalesce((select e.id from events e where e.id <= $1 and `+eventGapSQL("e", "$2")+` order by e.id desc limit 1), 0)
	`, through, refs.VisibilityArg(reader)).Scan(&head); err != nil {
		return 0, fmt.Errorf("read the caller's head of the event log: %w", err)
	}
	return head, nil
}

func (s *server) readEventRows(ctx context.Context, who model.Actor, query string, arguments ...any) ([]model.Event, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []model.Event{}
	for rows.Next() {
		var event model.Event
		var actor, payload []byte
		if err := rows.Scan(
			&event.ID,
			&event.IssueKey,
			&event.ArtifactID,
			&event.Project,
			&event.Seq,
			&event.Type,
			&actor,
			&event.Notify,
			&event.CreatedAt,
			&payload,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(actor, &event.Actor); err != nil {
			return nil, fmt.Errorf("decode event actor: %w", err)
		}
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.attachAskEventFields(ctx, who, events); err != nil {
		return nil, err
	}
	if err := s.leaveOutHiddenReferences(ctx, s.deps.Store.Pool, who, events); err != nil {
		return nil, err
	}
	if err := s.attachPostMarks(ctx, events); err != nil {
		return nil, err
	}
	return events, nil
}

// postBodyEventKinds is the kind of post each event kind's payload is about: the kind whose
// table the payload's `id` names, and whose own body the payload carries under `body`. They are
// what attachPostMarks marks: every other kind of either post carries no body of its own. The
// receipts carry no text of the post at all (comment.delivery, message.delivery, message.accepted,
// whose payload names the message under message_id and holds no text of it), and a delete event
// (comment.deleted, message.deleted) is built from the post with its body cleared and carries no
// text of the post a reply answers either (deleteEventReplyBlock). The other events of a reply
// also carry the text of the post it answers, which the event log keeps (reply_body and
// reply_block.text, below).
var postBodyEventKinds = map[string]refs.ItemKind{
	"comment.created":          refs.ItemComment,
	"comment.edited":           refs.ItemComment,
	"comment.answered":         refs.ItemComment,
	"comment.resolved":         refs.ItemComment,
	"comment.reopened":         refs.ItemComment,
	"comment.anchor_refreshed": refs.ItemComment,
	// The two suggestion kinds are a comment's own events under another name
	// (comment_actions.go, commentAction).
	"suggestion.accepted": refs.ItemComment,
	"suggestion.rejected": refs.ItemComment,
	"message.created":     refs.ItemMessage,
	"message.edited":      refs.ItemMessage,
	"message.answered":    refs.ItemMessage,
}

// postMarks are the marks a post's author left on it since it was written, as the post itself
// reads with them: when they last replaced its body (comments.edited_at, messages.edited_at), nil
// on a post nobody edited, and when they deleted it and who they are (deleted_at, deleted_by), as
// model.DeletedPostMark reads every other read's copy of that mark. deletedBy is nil where the
// row records no actor, which is the mark an event payload then carries as a null deleted_by.
// quoteCleared is that the comment's stored anchor quote is empty. Only the delete of a comment
// whose insert suggestion no accept made document text, pending or rejected, empties a quote, and
// only its own (clearDeletedPostBody), since that quote is the text the suggestion inserted. No
// other write stores an empty quote - an anchor's quote is refused empty (resolveAnchor) and every
// mark a refresh reads it from covers text. A message carries no anchor, so it is false there.
type postMarks struct {
	editedAt     *time.Time
	deletedAt    *time.Time
	deletedBy    *model.Actor
	quoteCleared bool
}

// postKey names one post an event page holds: the kind of post it is, which names the table its
// id is in (refs.ItemKind.ItemTable), and the id. Each kind's ids are its own, so a comment and a
// message of the same id are two posts here.
type postKey struct {
	kind refs.ItemKind
	id   string
}

// attachPostMarks gives every event of an edited or a deleted comment or message the marks the
// post itself reads with. Of an edited post it sets edited_at to the time its author last
// replaced the body: an event keeps the edited_at of the moment it was written, null on the
// post's creation, beside the body as it stood then, so without the post's own mark a read of
// the log would print that body as if nobody had replaced it since. Of a deleted post it also
// takes the body out of the payload: a read of the post's own events says that it is deleted and
// gives no part of what it said, the anchor quote of a deleted comment's insert suggestion
// included, while the events of a reply that quote it keep the preview they froze of it (below).
// The stored row is never rewritten - the log is the record of what happened - so the marks are
// read here, at every read, which is why readEventRows calls it for readEvents and
// readEventsAfterID alike. The time of each edit stays the created_at of the edit's own event.
//
// It is this file's own way of filling what a payload cannot carry from the write that appended
// it (attachAskEventFields): the events table holds no comment id and no message id, so there is
// nothing for eventSelect to join on, and a column joined onto the row could not remove the
// payload's body anyway.
//
// The frozen previews of another post stay, as the delete leaves them: the reply_body and the
// reply_block.text of a message reply's message.answered and of its message.edited written
// before the delete, and the reply_block.text of a comment reply's comment.created or
// comment.answered and of each later event of that reply written before the delete but its
// comment.deleted. A reply's own delete event, message.deleted or comment.deleted, carries no
// text of the post it answers whenever it is written (deleteEventReplyBlock), and an event of
// the reply written after the delete carries no text of it. Each is part of an event of the post
// that quoted it, and only the quoted post's own events are marked here. So are the document
// events of an accepted suggestion, which are no event of the post: the accept's
// artifact.version, and an artifact.renamed written before the delete, which carries every
// version, keep the name the accept gave the version.
func (s *server) attachPostMarks(ctx context.Context, events []model.Event) error {
	// The ids the page names, by the kind of post each is an id of; that kind names the table
	// readPostMarks reads it in.
	ids := map[refs.ItemKind][]string{}
	// What the first pass found for each event, which the second reads rather than finding
	// again: the post the payload names, and the payload it was read from. A raw payload is
	// nil for every event this read leaves alone.
	keys := make([]postKey, len(events))
	raws := make([]json.RawMessage, len(events))
	// A page holds several events of one post - its creation, its edit, its delete - and the
	// read asks after each post once.
	asked := map[postKey]bool{}
	for index := range events {
		kind, carriesBody := postBodyEventKinds[events[index].Type]
		if !carriesBody {
			continue
		}
		raw, ok := events[index].Payload.(json.RawMessage)
		if !ok {
			return fmt.Errorf("decode %s payload: expected raw JSON", events[index].Type)
		}
		// A payload this server did not write - an older build's, a test's - need name no post:
		// one that names none is passed through as the database returned it.
		var header struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &header); err != nil || header.ID == "" {
			continue
		}
		key := postKey{kind: kind, id: header.ID}
		keys[index], raws[index] = key, raw
		if asked[key] {
			continue
		}
		asked[key] = true
		ids[kind] = append(ids[kind], header.ID)
	}
	marks, err := s.readPostMarks(ctx, ids)
	if err != nil {
		return err
	}
	if len(marks) == 0 {
		return nil
	}
	for index := range events {
		if raws[index] == nil {
			continue
		}
		// Each kind's id names its own table, so a comment event reads the comments' marks and
		// a message event the messages', which is what the key the first pass built says.
		post, found := marks[keys[index]]
		if !found {
			continue
		}
		payload := map[string]any{}
		if err := json.Unmarshal(raws[index], &payload); err != nil {
			return fmt.Errorf("decode %s payload: %w", events[index].Type, err)
		}
		if post.editedAt != nil {
			payload["edited_at"] = post.editedAt
		}
		if post.deletedAt != nil {
			// The body, the mark that it was cut short, and a suggestion's replacement text,
			// which is the comment's text too and which the delete clears with the body.
			delete(payload, "body")
			delete(payload, "body_truncated")
			delete(payload, "suggestion")
			// The quote the row holds empty, which every event the comment wrote before its delete
			// carries in its anchor as it stood then. A quote is document text and stays - an
			// accepted insert's, a replace's, a delete's and a plain comment's - but for one: the
			// text an insert suggestion put in the document and no accept made document text, which
			// the comment's own delete emptied with the body.
			if anchor, ok := payload["anchor"].(map[string]any); ok && post.quoteCleared {
				anchor["quote"] = ""
			}
			payload["deleted_at"] = post.deletedAt
			payload["deleted_by"] = post.deletedBy
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode %s payload: %w", events[index].Type, err)
		}
		events[index].Payload = json.RawMessage(encoded)
	}
	return nil
}

// readPostMarks reads the marks of each post of ids that is edited or deleted, in one statement
// over the table of each kind that has an id to read: the table refs names for the kind, which
// owns the map of an item kind to its storage (refs.ItemKind.ItemTable), as postHistoryOwner
// reads it, with whether a comment's stored anchor quote is empty (postMarks.quoteCleared). Each
// id is matched as text, through the comments_id_text and messages_id_text indexes, so a payload
// id that is not a uuid cannot fail the read, as it would under a cast of the parameter. Each row
// names the kind it came from, which the id alone does not say.
func (s *server) readPostMarks(ctx context.Context, ids map[refs.ItemKind][]string) (map[postKey]postMarks, error) {
	marks := map[postKey]postMarks{}
	// One select per kind that has an id to read, in a fixed order rather than a map's, each
	// with the parameter its ids are passed as.
	selects, arguments := []string{}, []any{}
	for _, kind := range []refs.ItemKind{refs.ItemComment, refs.ItemMessage} {
		if len(ids[kind]) == 0 {
			continue
		}
		items, _, err := kind.ItemTable()
		if err != nil {
			return nil, err
		}
		quoteCleared := "false"
		if kind == refs.ItemComment {
			quoteCleared = "coalesce(anchor->>'quote' = '', false)"
		}
		arguments = append(arguments, ids[kind])
		selects = append(selects, fmt.Sprintf(`
		select '%s'::text, id::text, edited_at, deleted_at, deleted_by, %s from %s
		where id::text = any($%d::text[]) and (edited_at is not null or deleted_at is not null)`, kind, quoteCleared, items, len(arguments)))
	}
	if len(selects) == 0 {
		return marks, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, strings.Join(selects, "\n\t\tunion all"), arguments...)
	if err != nil {
		return nil, fmt.Errorf("read the corrected posts of an event page: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key postKey
		var kind string
		var editedAt, deletedAt *time.Time
		var deletedBy []byte
		var quoteCleared bool
		if err := rows.Scan(&kind, &key.id, &editedAt, &deletedAt, &deletedBy, &quoteCleared); err != nil {
			return nil, fmt.Errorf("read the corrected posts of an event page: %w", err)
		}
		key.kind = refs.ItemKind(kind)
		mark := postMarks{editedAt: editedAt, quoteCleared: quoteCleared}
		var err error
		if mark.deletedAt, mark.deletedBy, err = model.DeletedPostMark(deletedAt, deletedBy); err != nil {
			return nil, fmt.Errorf("post %s: %w", key.id, err)
		}
		marks[key] = mark
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the corrected posts of an event page: %w", err)
	}
	return marks, nil
}

// decodedAskEventPayload is the ask payload that needs read-time fields added before it is written
// to an event response. Other event payloads remain their database JSON.
type decodedAskEventPayload struct {
	event   *model.Event
	payload map[string]any
}

// attachAskEventFields fills the ask fields an event payload cannot carry from the write that
// appended it: the id of the event that opened the ask, its inbound backlink count, the part ids
// of the options and answer it shows with the ask's part revision, and its anchor document. Each
// is the ask's fact now, not the write's. Ask payloads are decoded because those fields are
// added, and an ask.answered payload's selected lists are read as model.AskAnswer reads them
// (normalizeAnsweredSelected); every other payload passes through as the database returned it.
func (s *server) attachAskEventFields(ctx context.Context, who model.Actor, events []model.Event) error {
	asks := []model.Ask{}
	payloads := []decodedAskEventPayload{}
	for index := range events {
		switch events[index].Type {
		case "ask.opened", "ask.anchor_refreshed", "ask.answered", "ask.resolved", "ask.edited", "ask.handed_back", "ask.asked_of":
		default:
			continue
		}
		raw, ok := events[index].Payload.(json.RawMessage)
		if !ok {
			return fmt.Errorf("decode %s payload: expected raw JSON", events[index].Type)
		}
		payload := map[string]any{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode %s payload: %w", events[index].Type, err)
		}
		if events[index].Type == "ask.answered" {
			if err := normalizeAnsweredSelected(raw, payload); err != nil {
				return err
			}
		}
		askID, ok := payload["id"].(string)
		if !ok || askID == "" {
			return fmt.Errorf("decode %s payload: ask id missing", events[index].Type)
		}
		shell, err := askEventShell(askID, raw)
		if err != nil {
			return err
		}
		asks = append(asks, shell)
		payloads = append(payloads, decodedAskEventPayload{event: &events[index], payload: payload})
	}
	askPointers := make([]*model.Ask, len(asks))
	for index := range asks {
		askPointers[index] = &asks[index]
	}
	if err := loadAskDetails(ctx, s.deps.Store.Pool, who, askPointers); err != nil {
		return err
	}
	for index := range payloads {
		payloads[index].payload["opened_event_id"] = *asks[index].OpenedEventID
		payloads[index].payload[model.ReferencedByCountKey] = *asks[index].ReferencedByCount
		attachAskEventPartIDs(payloads[index].payload, asks[index])
	}
	if err := s.attachAskDocumentArtifacts(ctx, who, payloads); err != nil {
		return err
	}
	for _, payload := range payloads {
		encoded, err := json.Marshal(payload.payload)
		if err != nil {
			return fmt.Errorf("encode %s payload: %w", payload.event.Type, err)
		}
		payload.event.Payload = json.RawMessage(encoded)
	}
	return nil
}

// askEventShell is the ask attachAskPartIDs matches the stored parts against: its id and the
// options and the answer this payload itself carries, as the write recorded them. Their text is
// what identifies a part, so a payload the ask has moved past - an ask.opened whose options an
// edit has since replaced, an ask.answered superseded by another answer - matches no part of the
// ask now and carries no id. The payload's own fields are never rewritten from the ask's row;
// only the ids attachAskEventPartIDs writes are added.
func askEventShell(askID string, raw json.RawMessage) (model.Ask, error) {
	var shell struct {
		Options []model.AskOption `json:"options"`
		Answer  *model.AskAnswer  `json:"answer"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		return model.Ask{}, fmt.Errorf("decode the options and answer of ask %s: %w", askID, err)
	}
	return model.Ask{ID: askID, Options: shell.Options, Answer: shell.Answer}, nil
}

// attachAskEventPartIDs writes the part ids attachAskPartIDs read onto the payload's own options
// and answer, with the ask's part revision beside them, so a payload names each option it shows
// as every other read of the ask names it. An option or an answer no part of the ask stands for
// now - an ask.opened of an ask whose options an edit has since replaced, a superseded answer -
// carries no id, and an ask whose parts are not indexed yet no revision either: attachAskPartIDs
// filled neither.
func attachAskEventPartIDs(payload map[string]any, ask model.Ask) {
	if ask.Revision > 0 {
		payload["revision"] = ask.Revision
	}
	options, _ := payload["options"].([]any)
	for index, option := range options {
		entry, decoded := option.(map[string]any)
		if !decoded || ask.Options[index].ID == "" {
			continue
		}
		entry["id"] = ask.Options[index].ID
	}
	if ask.Answer == nil || ask.Answer.ID == "" {
		return
	}
	if answer, carried := payload["answer"].(map[string]any); carried {
		answer["id"] = ask.Answer.ID
	}
}

// normalizeAnsweredSelected writes model.AskAnswer's reading of selected into an ask.answered
// payload's answer and previous_answer: an ask.answered event written before answers stored an
// empty list holds null there, and a client reads the list's length. Every other field of either
// answer stays as the payload holds it, and an answer the payload does not carry stays absent.
func normalizeAnsweredSelected(raw json.RawMessage, payload map[string]any) error {
	var answers struct {
		Answer         *model.AskAnswer `json:"answer"`
		PreviousAnswer *model.AskAnswer `json:"previous_answer"`
	}
	if err := json.Unmarshal(raw, &answers); err != nil {
		return fmt.Errorf("decode ask.answered answers: %w", err)
	}
	setSelected := func(key string, answer *model.AskAnswer) {
		if fields, ok := payload[key].(map[string]any); ok && answer != nil {
			fields["selected"] = answer.Selected
		}
	}
	setSelected("answer", answers.Answer)
	setSelected("previous_answer", answers.PreviousAnswer)
	return nil
}

// attachAskDocumentArtifacts hydrates anchor_artifact and approval_artifact on every ask event
// payload that names one, the same live lookup ask_rows.go's scanAskRow does for a read: an
// anchored ask's quoted document, or an approval request's document, both resolved by their
// current slug and primary flag rather than a value frozen at write time. A document of a
// project who does not see is one the read does not find, so its payload names none.
func (s *server) attachAskDocumentArtifacts(ctx context.Context, who model.Actor, eventPayloads []decodedAskEventPayload) error {
	payloads := map[string][]decodedAskEventPayload{}
	for _, eventPayload := range eventPayloads {
		payload := eventPayload.payload
		_, hasAnchor := payload["anchor"].(map[string]any)
		_, hasApproval := payload["approval"].(map[string]any)
		if !hasAnchor && !hasApproval {
			if payload["anchor"] != nil {
				return fmt.Errorf("decode %s payload: anchor must be an object", eventPayload.event.Type)
			}
			continue
		}
		askID, ok := payload["id"].(string)
		if !ok || askID == "" {
			return fmt.Errorf("decode %s payload: ask id missing", eventPayload.event.Type)
		}
		payloads[askID] = append(payloads[askID], eventPayload)
	}
	if len(payloads) == 0 {
		return nil
	}
	askIDs := make([]string, 0, len(payloads))
	for askID := range payloads {
		askIDs = append(askIDs, askID)
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select a.id::text, aa.project_key, aa.slug, aa.name, aa.is_primary,
			pa.project_key, pa.slug, pa.name, pa.is_primary
		from asks a
		left join artifacts aa on aa.id = `+store.CastableUUID(`a.anchor->>'artifact_id'`)+`
			and (aa.project_key is null or `+access.ProjectClause("aa.project_key", "$2")+`)
		left join artifacts pa on pa.id = (a.approval->>'artifact_id')::uuid
			and (pa.project_key is null or `+access.ProjectClause("pa.project_key", "$2")+`)
		where a.id::text = any($1) and `+access.ItemOwnerClause("a.issue_key", "a.artifact_id", "$2")+`
	`, askIDs, refs.VisibilityArg(who))
	if err != nil {
		return err
	}
	defer rows.Close()
	anchorArtifacts := make(map[string]model.AskAnchorArtifact, len(payloads))
	approvalArtifacts := make(map[string]model.AskAnchorArtifact, len(payloads))
	for rows.Next() {
		var askID string
		var anchor, approval nullableAskArtifactRef
		if err := rows.Scan(
			&askID,
			&anchor.project, &anchor.slug, &anchor.name, &anchor.primary,
			&approval.project, &approval.slug, &approval.name, &approval.primary,
		); err != nil {
			return err
		}
		if artifact := anchor.artifact(); artifact != nil {
			anchorArtifacts[askID] = *artifact
		}
		if artifact := approval.artifact(); artifact != nil {
			approvalArtifacts[askID] = *artifact
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for askID, asks := range payloads {
		anchorArtifact, anchorFound := anchorArtifacts[askID]
		approvalArtifact, approvalFound := approvalArtifacts[askID]
		for _, ask := range asks {
			delete(ask.payload, "anchor_artifact")
			delete(ask.payload, "approval_artifact")
			if anchorFound {
				ask.payload["anchor_artifact"] = anchorArtifact
			}
			if approvalFound {
				ask.payload["approval_artifact"] = approvalArtifact
			}
		}
	}
	return nil
}

func parseNonNegativeInt(raw, field string) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "%s must be a non-negative integer", field)
	}
	return value, nil
}

// parseEventPosition reads an event list's after or before, a position in its owner's event
// sequence: a non-negative integer that fits events.seq, an integer column (fitsIntegerColumn).
// Anything else names no event and is refused 400 INVALID_QUERY before any query is made with
// it. The whole log's after (listEventLog) is an event id, a bigint, and is read without this
// bound.
func parseEventPosition(raw, field string) (int64, error) {
	value, err := parseNonNegativeInt(raw, field)
	if err != nil || !fitsIntegerColumn(value) {
		return 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "%s must be an integer from 0 to %d", field, maxIntegerColumn)
	}
	return value, nil
}

// parseEventLimit reads a page's limit: 1 to most, and most when absent.
func parseEventLimit(raw string, most int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return most, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > most {
		return 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "limit must be between 1 and %d", most)
	}
	return limit, nil
}

func parseEventOrder(raw string) (bool, error) {
	switch strings.TrimSpace(raw) {
	case "", "asc":
		return false, nil
	case "desc":
		return true, nil
	default:
		return false, errorf(http.StatusBadRequest, "INVALID_QUERY", "order must be asc or desc")
	}
}

func parseEventIDs(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 50 {
		return nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "ids must include at most 50 values")
	}
	ids := make([]int64, len(parts))
	for index, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id < 1 {
			return nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "ids must be positive integers")
		}
		ids[index] = id
	}
	return ids, nil
}
