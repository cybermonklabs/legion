package api

import (
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// unreadDirectRepliesCTE is the one definition of "a reply this login has not read in this
// session": a session's own reply, anywhere under a root this viewer targeted at this session
// (an issue-less message whose own target is `session:<id>`, a broadcast's copy included), newer
// than the later of the viewer's read mark and Clear for that session, and not one the viewer has
// read by its id (which the broadcast page writes for the replies it shows, so an older reply
// elsewhere stays unread where moving the mark would hide it). That scope is deliberate.
// A root the session only received a delivery of - one targeted at a role, or at another
// session - is not a direct message this viewer sent it, so it is never unread for them; the
// conversation window still lists it by activity, like any other conversation the session is in. It is the recursive part of a
// `with recursive` query and ends in two relations: `unread_direct_replies(session_id, root_id,
// reply_id, created_at)`, and the marks it reads them against,
// `direct_marks(session_id, read_through, cleared_before, at)`, which is one of the three
// sources `GET /api/v1/me/agents/state` joins (the viewer's pin is the third).
//
// Both endpoints that read it build on this fragment rather than on a predicate of their own:
// `GET /api/v1/me/agents/state` counts its rows per session (`unread_replies`), and
// `GET /api/v1/agents/{session_id}/messages` unions its roots into the conversation window. The
// two disagreeing is exactly LEGION-301's overflow defect - a reply counted, never shown, then
// marked read by a watermark that only moves forward - so one edit has to move both. The Inbox's
// Agent rows ask the same question of one root at a time (api/replies_agent.go), through the same
// two readings of the marks below, directMarks and replyReadByID.
//
// A delete by the release before leaves this release's tuple standing, since only this release's
// writers append the retraction. That release deletes rows of four of the six read-state tables:
// user_threads (removeArtifact's clear of an untouched document), user_ask_snooze (deleteAskSnooze,
// and the cascade of an ask's delete, which nothing makes), user_agent_thread_dismissed (the
// restore, inboxRestoreAgentThreadRows, and the trigger of migration 20261105093507, which its
// session's reply runs and which this release's own trigger body passes only for that build's
// message) and user_agent_reply_read (pruneAgentReplies, under its read mark and its mark-all). It
// deletes no row of user_agent_read or user_agent_state. Of the reads that answer from tuples -
// this fragment's direct_marks and its read-by-id test, and the Agent rows' myAgentMarks and
// myAgentUnreadReplyTest, built from the same two readings - only the read-by-id test
// (replyReadByID) reads a table that release deletes from, and only through the prune. The prune
// deletes a row only where its reply is at or before the session's read mark as that release reads
// it (user_agent_read under the canonical login), and direct_marks reads that same mark: the row,
// where that release's update took its marker off, else the tuple, which a marked row equals. So a
// reply whose row the prune took is read by the mark either way, the reply_read tuple left standing
// answers what the row would have - not unread - and since a read mark only moves forward, under
// either release, no later write makes that reply unread again. No read here needs the tuple held
// to its row, as INFRA-1149's view join holds an arm whose rows a rollback can remove. The
// dismissal, the snooze and user_threads are still read from their rows, so a tuple a delete
// leaves standing there reaches no read; R9's census is what reads it.
// TestEveryReadStateWriteIsATupleFromThePerson holds the prune case.
//
// Parameters, in this order, so a query appending its own starts at $4:
//
//	$1  the caller's canonical login, which is their person node's id (graphstore.PersonNode):
//	    the marks are read from that node, and it matches the author of their own direct messages,
//	    which the identity source may spell in any casing
//	$2  the caller's raw actor id, which keys the Clear a row of user_agent_state holds where no
//	    tuple states it yet (migration 0033)
//	$3  one session id to narrow to, or null for every session
//
// The login is bound once by unreadDirectRepliesArgs, referenced only inside this fragment
// (direct_roots, direct_marks and unread_direct_replies), and never inside a union arm - the
// window's candidates union takes $3 alone. The hazard a future edit opens is exactly that: put a
// login predicate into one candidate branch (filtering the delivered branch to roots the viewer
// authored, say) and the surface genuinely doubles, with only the call-site mutation's test
// standing behind it.
//
// unreadDirectRepliesArgs builds them, so no call site spells the canonical login itself.
var unreadDirectRepliesCTE = `
	direct_roots as (
		select id as root_id, substr(target, length('session:') + 1) as session_id
		from messages
		where issue_key is null and in_reply_to is null and target like 'session:%'
		  and author->>'kind' = 'user' and lower(author->>'id') = $1
		  and ($3::text is null or target = 'session:' || $3::text)
	),
	direct_thread as (
		select m.id, m.author, m.created_at, direct_roots.root_id, direct_roots.session_id
		from messages m join direct_roots on m.in_reply_to = direct_roots.root_id
		union all
		select m.id, m.author, m.created_at, direct_thread.root_id, direct_thread.session_id
		from messages m join direct_thread on m.in_reply_to = direct_thread.id
	),
	-- The read mark and the Clear are read from the viewer's one person node ($1), whatever
	-- spelling of their login the route that wrote each one read, which ends the two keys the two
	-- tables are read by; a row no tuple states yet keeps its own key (directMarks).
	-- TestAViewersRepliesCountAndClearWhateverTheCasingOfTheirLogin guards it.
	direct_marks as (` + directMarks(true) + `
	),
	unread_direct_replies as (
		select direct_thread.session_id, direct_thread.root_id,
		       direct_thread.id as reply_id, direct_thread.created_at
		from direct_thread
		left join direct_marks on direct_marks.session_id = direct_thread.session_id
		where direct_thread.author->>'kind' = 'session'
		  and direct_thread.created_at > coalesce(direct_marks.at, '-infinity'::timestamptz)
		  and not ` + replyReadByID("direct_thread.id") + `
	)`

// directMarks reads the viewer's read mark and Clear of each session, as
// `(session_id, read_through, cleared_before, at)`, at the later of the two: the newest
// read_through and the newest cleared tuple from the viewer's one person node ($1) into each
// session, whichever spelling of their login the route that wrote it read (graphstore.FromPerson),
// which ends the two keys the two tables were read by - the canonical login for user_agent_read,
// the raw actor id for user_agent_state.
//
// Each kind is read by a skip over graph_tuples_current (newestMarks): a recursive step reads the
// least session past the one before off the index, and a lateral the newest tuple into that
// session, so the read costs two index probes a session however many marks the person stated
// there. A read mark is stated only where it moved and a Clear only where it was given, so a
// person's history of either grows with every write, and a read of that history whole - the
// `distinct on` of graphstore.CurrentTupleRead, which walks and fetches every tuple of the
// relation to keep its newest - would grow with it.
//
// A row no tuple states is answered by the table as it was before the store, on the key it was
// read by: a row of user_agent_read under $1 or of user_agent_state under $2 whose marker is null,
// which an older build wrote during a roll or which predates the store, until the backfill takes
// it once (migration 20261116170000). Such a row stands in for the tuple of its kind and session,
// since an older build's update of a row this release marked takes the marker off and leaves the
// tuple stating what the row said before it.
//
// narrowed reads $3, the session unreadDirectRepliesCTE narrows to; the Inbox's Agent rows bind
// $3 for a statement of their own (api/inbox_dismiss.go) and read every session.
//
// It is guarded by TestAViewersRepliesCountAndClearWhateverTheCasingOfTheirLogin
// (api/direct_messages_test.go), which reads a read mark and a Clear written under different
// spellings of one viewer's login meeting at that viewer, by
// TestEveryReadStateWriteIsATupleFromThePerson (api/read_state_tuples_test.go), which reads both
// marks from the one person node and an older build's row on the key it was written under, and by
// TestTheDirectMarksReadTwoProbesASession (api/direct_marks_plan_test.go), which holds the read of
// ten thousand marks on five sessions to the probes of five sessions.
func directMarks(narrowed bool) string {
	narrow := func(session string) string {
		if !narrowed {
			return ""
		}
		return `
			  and ($3::text is null or ` + session + ` = $3::text)`
	}
	return `
		select mark.session_id,
		       max(mark.at) filter (where mark.kind = '` + graphstore.TupleReadThrough + `') as read_through,
		       max(mark.at) filter (where mark.kind = '` + graphstore.TupleCleared + `') as cleared_before,
		       max(mark.at) as at
		from (
			select stated.to_id as session_id, '` + graphstore.TupleReadThrough + `' as kind, stated.at
			from (` + newestMarks(graphstore.TupleReadThrough, narrowed) + `) stated
			where stated.asserted and not exists (
				select 1 from user_agent_read old
				where old.login = $1 and old.session_id = stated.to_id and old.tuples_stored_at is null
			)
			union all
			select stated.to_id, '` + graphstore.TupleCleared + `', stated.at
			from (` + newestMarks(graphstore.TupleCleared, narrowed) + `) stated
			where stated.asserted and not exists (
				select 1 from user_agent_state old
				where old.login = $2 and old.session_id = stated.to_id and old.tuples_stored_at is null
			)
			union all
			select session_id, '` + graphstore.TupleReadThrough + `', read_through from user_agent_read
			where login = $1 and tuples_stored_at is null` + narrow("session_id") + `
			union all
			select session_id, '` + graphstore.TupleCleared + `', cleared_before from user_agent_state
			where login = $2 and tuples_stored_at is null` + narrow("session_id") + `
		) mark
		group by mark.session_id`
}

// newestMarks is directMarks' read of one kind of mark, read_through or cleared, from the person
// node $1 into sessions: of each session it holds one into, the newest tuple, as
// `(to_id, at, asserted)`, which a `where asserted` over it cuts to the marks that stand, as
// graphstore.CurrentTupleRead's answer is cut. It reads by an index skip over
// graph_tuples_current, whose columns lead (from_kind, from_id, kind, to_kind, to_id) and end in
// seq descending: the recursive step reads the least session past the one before, one probe a
// session, and the lateral the newest tuple into each, one probe more, however many tuples the
// relation holds. narrowed reads $3, the one session to read or null for every session: the skip
// starts at it, takes no step past it, and keeps no other session.
func newestMarks(kind string, narrowed bool) string {
	into := `t.from_kind = '` + graphstore.KindPerson + `' and t.from_id = $1 and t.kind = '` + kind + `'
				  and t.to_kind = '` + graphstore.KindSession + `'`
	start, step, keep := "", "", ""
	if narrowed {
		start = ` and t.to_id >= coalesce($3::text, '')`
		step = ` and $3::text is null`
		keep = ` and ($3::text is null or sessions.to_id = $3::text)`
	}
	return `
			with recursive sessions (to_id) as (
				select min(t.to_id) from graph_tuples t
				where ` + into + start + `
				union all
				select (select min(t.to_id) from graph_tuples t
				        where ` + into + ` and t.to_id > sessions.to_id)
				from sessions
				where sessions.to_id is not null` + step + `
			)
			select sessions.to_id, newest.at, newest.asserted
			from sessions
			cross join lateral (
				select t.at, t.asserted from graph_tuples t
				where ` + into + ` and t.to_id = sessions.to_id
				order by t.seq desc
				limit 1
			) newest
			where sessions.to_id is not null` + keep
}

// replyReadByID is the test that the viewer ($1) read the reply reply names by its id: the
// reply_read tuple from their person node to it stands (graphstore.CurrentTupleRead), or a row of
// user_agent_reply_read no tuple states yet says so, by directMarks' rule for a row of the
// other two tables. Each probe is one index read of its own, fenced from the planner (`offset 0`)
// so the OR between them never turns either into a read of every reply the viewer ever read.
// reply is read in a select of its own before the store's read takes it, because that read names
// graph_tuples `t`, which would shadow a caller's alias of the same name - the Agent rows'
// `thread t` - inside it.
func replyReadByID(reply string) string {
	return `(exists (
			select 1 from user_agent_reply_read old
			where old.login = $1 and old.reply_id = ` + reply + ` and old.tuples_stored_at is null
			offset 0
		) or exists (
			select 1 from (select (` + reply + `)::text as id) asked
			cross join lateral (` + graphstore.CurrentTupleRead(`t.to_id`, `t.asserted`,
		`t.from_kind = '`+graphstore.KindPerson+`' and t.from_id = $1 and t.kind = '`+graphstore.TupleReplyRead+`'
				and t.to_kind = '`+graphstore.KindMessage+`' and t.to_id = asked.id`) + `) stated
			where stated.asserted
			offset 0
		))`
}

// unreadDirectRepliesArgs is unreadDirectRepliesCTE's parameter list for one caller: their
// canonical login, their raw actor id, and the session to narrow to (nil for every session). A
// query appending its own parameters starts at $4.
func unreadDirectRepliesArgs(actorID string, sessionID *string) []any {
	return []any{model.CanonicalLogin(actorID), actorID, sessionID}
}
