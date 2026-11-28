package graphcensus

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// answerer is the person node an asks row's answer names, written as asks.AnswererOf names it:
// the canonical login of the answer's user (user_threads_login is model.CanonicalLogin in SQL),
// empty where it names nobody. It reads `asks k`.
const answerer = `coalesce(user_threads_login(k.answer->>'user'), '')`

// authorNode is the far end of the authored_by tuple a row whose author is the jsonb actor states,
// written kind:id: the node graphstore.AuthorOf reads off the actor the row stored - the session
// it names, else the person authorLogin reads, a login canonicalised as commentWriterPeople
// canonicalises one - and null for an actor that names nobody, whose row states no authored_by at
// all.
func authorNode(actor string) string {
	return `case
			when ` + actor + `->>'kind' = 'session' and coalesce(` + actor + `->>'id', '') <> ''
				then '` + graphstore.KindSession + `:' || (` + actor + `->>'id')
			else '` + graphstore.KindPerson + `:' || user_threads_login(` + authorLogin(actor) + `)
		end`
}

// authoredLink is the authored_by link a row whose author is the jsonb actor states, as
// statedLinks takes one: kind>to_kind:to_id, null for an actor naming nobody and for a session
// past the store's bound, of whom the writer states nothing (graphstore.AuthorFacts).
func authoredLink(actor string) string {
	return `case when ` + actor + `->>'kind' <> '` + graphstore.KindSession + `'
		           or octet_length(coalesce(` + actor + `->>'id', '')) <= ` + addressBound + `
		      then '` + graphstore.TupleAuthoredBy + `>' || ` + authorNode(actor) + ` end`
}

// commentWriters are the comments the store holds, each as the actor that wrote it and the time it
// was written: every comments row the store holds (refs.CommentInTheStore). It is who wrote a
// comment, as commentWriterPeople and recordedSessions read it.
var commentWriters = `
	select c.author as actor, c.created_at as at
	from comments c` + refs.CommentStoreJoins + `
	where ` + refs.CommentInTheStore

// commentWriterPeople is each person a comment the store holds or held names as its writer
// (commentWriters), one row for each such comment and person, with the time the comment was
// written and the event that stored it (null for a row). It reads an actor as graphstore.AuthorOf
// does (authorLogin): the login an actor of kind user carries, or the owner a personal token's
// actor carries, each canonicalised the way user_threads_login canonicalises a login (migration
// 0095, which is model.CanonicalLogin in SQL). Beside them are the owners of the sessions the
// server proved that wrote a comment: the comment's author is the session, whose act names that
// same session and whose owner is the person it belongs to, and the write that ties the two with
// an alias_of tuple makes the owner's node with it (graphstore.AuthorFacts), except for a session
// past the store's bound, of which nothing is written.
//
// Only the comments the store holds name anybody here (refs.CommentInTheStore): a comment
// anchored to or owned by a document whose address is past the bound is left whole out of the
// store, so it states no authored_by and makes no author node, and a person whose only comment is
// one of those is no node of the store and no difference of this census.
//
// It is the one reading of who a comment names, read by two: the census's person row side
// (commentPeople) and the sizing's people (sizingWritesWith), so the sizing counts the people
// the census holds to a node for a comment. It reads no table of the store, so the sizing can
// read it on a database the store does not exist in yet.
var commentWriterPeople = `
	select user_threads_login(author.login) as login, written.at
	from (` + commentWriters + `) written
	cross join lateral (values (` + authorLogin("written.actor") + `),
		(case when ` + provedSessionOwner("written.actor") + ` then written.actor->>'owner' end)) author (login)
	where author.login is not null and user_threads_login(author.login) <> ''`

// provedSessionOwner is true of a stored actor, the SQL expression actor, that is a session the
// server proved its owner's (graphstore's provedOwner): the actor is the session, its act names
// that same session, and the session is within the store's bound. Its `owner`, where it names one,
// is then the person the session belongs to, whom the write that stored the actor ties to the
// session by an alias_of tuple and makes a node of (graphstore.AuthorFacts). It is
// commentWriterPeople's second arm, spelled once here so that commentWriterPeople and the sizing's
// count of a comment's aliases (sizingWritesWith) read one rule.
func provedSessionOwner(actor string) string {
	return actor + `->>'kind' = 'session' and coalesce(` + actor + `->>'id', '') <> ''
		  and ` + actor + `->'act'->>'session' = ` + actor + `->>'id'
		  and octet_length(` + actor + `->>'id') <= ` + addressBound
}

// commentPeople are the people the census holds to a person node, each once: who wrote a comment
// the store holds (commentWriterPeople), and the people an ask, a decision or a reading of the
// store names, below.
//
// The backfill gives the people comments name their nodes in its comment pass (writeCommentBatch,
// graphstore.AuthorFacts) and needs no pass of its own for them: a row names its author to that
// pass.
//
// An ask whose row names its node names people too: its author, read as a
// comment's is (graphstore.AskFacts states its authored_by), and the person whose answer it holds,
// whose node its answered_by and decided_by point at (asks.Settle). A node is history and never
// goes, as a session that once followed an ask still owes its node (recordedSessions), so a person
// whose answer a reopen took off the row is still the person that answer named: every person an
// ask's answered_by or decided_by ever named, asserted or retracted since, is read here off the
// store's own log, which holds that answer still. Only an answer the store took names anybody so,
// and an answer a build before the store wrote and took off again names nobody, since the
// backfill states a row's answer as it stands (graphcensus' asks pass). The log is read from the
// person's node through graph_tuples_as_of_in, whose partial predicate the arm states beside its
// join, so the planner keys the index rather than reading the table whole.
//
// Who wrote each decision, reading and ask the store holds an authored_by or decided_by of names
// people too, and so does every person an ask's answer ever named, and every author of a lens, a
// finding, a risk or a constraint and of a run whose row is gone (storedAuthors, read as
// the relation authors names: censusAuthors where the census binds what it read once,
// inlineAuthors where a statement reads it itself), and so does the owner the server proved of
// each such writer that is a session (storedOwners).
//
// The authors of the runs going_requests holds are read off each row's author by the same two
// rules as a comment's, since the run's writer (graphstore.InteractionFacts) states its author
// through graphstore.AuthorFacts as the comment path does.
func commentPeople(authors string) string {
	return `
	select distinct user_threads_login(author.login) as login
	from (
		select named.login
		from (` + commentWriterPeople + `) named
		union all
		select asker.login
		from asks k
		cross join lateral (values (` + authorLogin("k.author") + `),
			(case when ` + provedSessionOwner("k.author") + ` then k.author->>'owner' end)) asker (login)
		where k.node_kind is not null
		union all
		select k.answer->>'user' from asks k where k.node_kind is not null and k.answer is not null
		union all
		select runner.login
		from going_requests r
		cross join lateral (values (` + authorLogin("r.author") + `),
			(case when ` + provedSessionOwner("r.author") + ` then r.author->>'owner' end)) runner (login)
		union all
		select stored.id from ` + authors + ` where stored.kind = '` + graphstore.KindPerson + `'
		union all
		select owner.login from (` + storedOwners(authors) + `) owner
	) author (login)
	where author.login is not null and user_threads_login(author.login) <> ''`
}

// storedAuthors are who wrote the decisions and the readings of every document, and every ask the
// store names an author of, as (kind, id, at): each a person or a session, whose node the writer
// writes beside the tie that names it (refs.PlanBlocks, refs.PlanDecisionNodes and
// graphstore.AskFacts, through graphstore.AuthorFacts). A decision's author is the far end of its
// decided_by, which its block_refs row projects, and that row is what is read for it. A reading's
// author is the far end of its authored_by, which no table projects (the tuple table), so the row
// read for it is the tuple itself, the authored_by each reading's node not gone states now, as
// graph_edges' tuple arm would answer it. An ask's author is read the same way, off the
// authored_by every ask node states now: an ask one of whose documents is past the store's bound
// keeps a row that names no node (graphstore.AskPlaces.Fit), which the ask author arms of
// commentPeople leave out, while its writer states the ask's node, its authored_by and its
// author's node all the same. Each names a node the store must hold, so a writer that stated the
// tie and not the node is a difference here. An ask a build before the store wrote states no
// authored_by until the backfill marks its row, so it names no author here.
//
// The authors of blocks name people and sessions too (T7 of the issue-tree plan). They are read
// for each block whose authors the block_author kind holds: an authored node or a block of an
// issue room, of a document that owes no block index (blockVersionAuthors, blockAuthorsHeld). A
// block that the issue conversion moved into its room takes the authors the conversion copied to
// its node. Those are the authors its old node's authored_by names. Where that names none, they
// are the authors of the first version of the old document that held the block, where Dispatch
// knows that version. Every other block takes the authors of the version that first held it. The
// block index and the conversion state each such authored_by through graphstore.AuthorFacts,
// which writes the author's node. A copied authored_by names a node its first writer wrote.
//
// A node is history and never goes: a decision or a reading deleted or retyped since takes its
// row and its current tie away and leaves its author's node standing, so every person and session
// a block's decided_by or authored_by ever named, asserted or retracted since, is read too, off
// the store's log (writtenHistory), and so is every person a decision's kept_by ever named: a
// person's Keep (api's keepDecisionNode, and the conversion's Keep it carries onto a section's
// decision) writes the keeper's node beside it, and a person whose only act is a Keep is held to
// that node by it alone. So is every person an ask's answered_by or decided_by ever named
// (answeredHistory): a person whose answer a reopen took off the row is still the person that
// answer named.
//
// The census reads it once for its run (readStoredAuthors) and binds what it read into the names
// of the person and session kinds (censusAuthors), so its two reads of the log cost one statement
// a census whatever plan the planner takes for them; the backfill's session pass reads it inside
// its own statement (inlineAuthors).
//
// The authors of the reasoning nodes and of the runs whose row is gone (reasoningAuthored,
// goneRunsAuthored) are read here too, so the person and session row sides read them bound as
// well and read the log for them no more than for the rest.
var storedAuthors = `
	select r.to_kind as kind, r.to_id as id, r.created_at as at
	from block_refs r
	where r.kind = '` + graphstore.TupleDecidedBy + `'
	  and r.to_kind in ('` + graphstore.KindPerson + `', '` + graphstore.KindSession + `')
	union all
	` + currentAuthors(`held.kind = '`+refs.BlockNodeKind+`' and held.supertag = '`+refs.ReadingSupertag+`'`) + `
	union all
	` + currentAuthors(`held.kind = '`+graphstore.KindAsk+`'`) + `
	union all
	` + blockVersionAuthors + `
	union all
	` + reasoningAuthored + `
	union all
	` + goneRunsAuthored + `
	union all
	` + writtenHistory + `
	union all
	` + answeredHistory

// censusAuthors is storedAuthors as the census binds it into the names of the person and session
// kinds, the kinds, ids and times readStoredAuthors read as $1, $2 and $3 (projection.authored),
// and inlineAuthors is storedAuthors read in the statement that names it. Each is a FROM item
// named stored, of (kind, id, at).
const censusAuthors = `unnest($1::text[], $2::text[], $3::timestamptz[]) stored (kind, id, at)`

var inlineAuthors = `(` + storedAuthors + `) stored`

// answeredHistory is every person an ask's answered_by or decided_by ever named, and every person
// an ask's removed_by ever named (the person who took the ask's block out of its document, whose
// node that write makes), asserted or retracted since, as (kind, id, at) (storedAuthors), read
// from each person's node: the probe of graph_tuples binds graph_tuples_as_of_in's partial
// predicate (to_kind <> 'value') beside its join, as graphstore.CurrentIn binds it, so that index
// is one the probe can take.
var answeredHistory = `select answered.kind, answered.id, answered.made_at from graph_nodes answered
		where answered.kind = '` + graphstore.KindPerson + `' and exists (
			select 1 from graph_tuples t
			where t.to_kind = answered.kind and t.to_kind <> '` + graphstore.KindValue + `' and t.to_id = answered.id
			  and t.kind in ('` + graphstore.TupleAnsweredBy + `', '` + graphstore.TupleDecidedBy + `', '` + graphstore.TupleRemovedBy + `')
			  and t.from_kind = '` + graphstore.KindAsk + `')`

// writtenHistory is every person and session a block's decided_by or authored_by ever named, and
// every person a decision's kept_by ever named, asserted or retracted since, as (kind, id, at)
// (storedAuthors), read as answeredHistory reads its people, from each writer's node. The join to
// a writer of either kind gives the planner no constant to prove graph_tuples_as_of_in's predicate
// from, so the probe binds it itself.
var writtenHistory = `select writer.kind, writer.id, writer.made_at
	from graph_nodes writer
	where writer.kind in ('` + graphstore.KindPerson + `', '` + graphstore.KindSession + `') and exists (
		select 1 from graph_tuples t
		where t.to_kind = writer.kind and t.to_kind <> '` + graphstore.KindValue + `' and t.to_id = writer.id
		  and t.kind in ('` + graphstore.TupleDecidedBy + `', '` + graphstore.TupleAuthoredBy + `', '` + refs.TupleKeptBy + `')
		  and t.from_kind = '` + refs.BlockNodeKind + `')`

// currentAuthors is the author every node held of graph_nodes that holders selects states now:
// the far end of its current authored_by into a person or a session, as (kind, id, at), read
// through graph_tuples_current, one probe per node.
func currentAuthors(holders string) string {
	return `select written.to_kind, written.to_id, written.at
	from graph_nodes held
	cross join lateral (
		` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_kind, t.to_id, t.at, t.asserted`,
		`t.from_kind = held.kind and t.from_id = held.id and t.kind = '`+graphstore.TupleAuthoredBy+`'
		  and t.to_kind <> '`+graphstore.KindValue+`' and t.to_kind in ('`+graphstore.KindPerson+`', '`+graphstore.KindSession+`')`) + `
	) written
	where ` + holders + ` and written.asserted`
}

// storedOwners are the people the server proved own a session that wrote a decision, a reading or
// an ask (storedAuthors): the write that names such a session ties it to its owner with an
// alias_of tuple and makes the owner's node with it (graphstore.AuthorFacts). Neither block_refs
// nor any other table records the actor a decision's author was proved by, so the owner is read as
// the store holds that tie now, the far end of each alias_of the session states. authors is the
// relation storedAuthors is read as (censusAuthors or inlineAuthors).
//
// The probe is keyed by the session and names no far end: an alias_of points at a person and at
// nothing else, since graphstore's aliases is its one writer, so it states graph_tuples_current's
// predicate (to_kind <> 'value', and kind <> 'delivered_to', which the kind it binds proves) where
// it would bind to_kind = 'person'. Only an index led by from_kind and from_id keys the probe:
// graph_tuples_as_of_in, led by to_kind, holds no from_id to key it by.
func storedOwners(authors string) string {
	return `
	select owner.to_id as login
	from (select distinct stored.id from ` + authors + ` where stored.kind = '` + graphstore.KindSession + `') writer
	cross join lateral (
		` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = '`+graphstore.KindSession+`' and t.from_id = writer.id and t.kind = '`+graphstore.TupleAliasOf+`'
		  and t.to_kind <> '`+graphstore.KindValue+`'`) + `
	) owner
	where owner.asserted`
}

// authorLogin is the login of the person graphstore.AuthorOf names of the actor a jsonb column
// holds, uncanonicalised, and null for an actor it names a session of or nobody.
func authorLogin(actor string) string {
	return `case
			when ` + actor + `->>'kind' = 'user' and coalesce(` + actor + `->>'id', '') <> '' then ` + actor + `->>'id'
			when ` + actor + `->>'kind' = 'session' and coalesce(` + actor + `->>'id', '') <> '' then null
			when coalesce(` + actor + `->>'owner', '') <> '' then ` + actor + `->>'owner'
		end`
}

// reasoningAuthored is the author every lens, finding of one (a block whose id names its lens,
// lensBlock), risk and constraint states now, as (kind, id, at) (storedAuthors): the far end of
// its current authored_by into a person or a session. Their one writer (api's writeReasoningNodes)
// states that tuple to the writer graphstore.AuthorOf names of the node's made_by, and that
// writer's own node beside it (graphstore.AuthorFacts), whatever the credential proved (spec v46
// R12), so the far end is a person or a session the store owes a node, and the owner the server
// proved of a session is read off its alias_of (storedOwners). A node written on the shared
// bearer, or by a pod on a session its body names, names that session, which is so on both sides;
// a node whose made_by names only a workload states none, and that workload is on neither.
var reasoningAuthored = currentAuthors(`(` + kindIn("held.kind", []string{graphstore.KindLens, graphstore.KindRisk, graphstore.KindConstraint}) +
	` or (held.kind = '` + refs.BlockNodeKind + `' and ` + graphstore.FindingIDSQL("held.id") + `))`)

// goneRunsAuthored is the author every run whose row is gone states now - one the sweep stated gone
// and removed the rows of an hour after it was asked (api's sweepGoingRequests), or one a process
// of the release before deleted an hour old during a roll, whose interaction the sweep or the
// backfill stated gone (backfillGoneRuns) - in the shape of reasoningAuthored. The row that named
// the author went with the run, so the store's own record of the writer stands on the row side, as
// it does for a reasoning node: an author's node is history and stays, and without this a person
// whose one write was such a run would read as a node no row projects.
var goneRunsAuthored = currentAuthors(`held.kind = '` + graphstore.KindInteraction + `' and held.state = '` + graphstore.StateGone + `'`)

// recordedSessions are the sessions a row of the store names or an event of the log has named,
// each once, with the first event that named it (null for a session only a row names) and the
// earliest time a row or an event names it. They are every write R14 records a session from: a
// comment a session wrote (graphstore.AuthorOf), as its row names it (commentWriters); every
// session a delivery reached - a comment mention's and a targeted message's attempt alike,
// whether the outbox's record states it (outbox.deliveredGraph) or the attempt's own settle does
// (graphstore.QueueReached), and a fallback copy the outbox published to a session's own topic
// (outbox.fallbackDelivered); an ask a session opened, every session that follows an ask now, and
// every session an ask.follower_added event ever named (asks.Follow), so a follower added and
// removed before the store existed is held to a node as well. A node is history and never goes,
// so a session that once followed still owes it. The same holds of a session that asked: every
// session an ask.opened event named as the ask's author is read too, so a session that asked
// still owes its node whatever becomes of the ask's row, and the log keeps addressing it. And
// every session that wrote a decision, a reading or an ask the store names an author of
// (storedAuthors).
//
// A comment mention's attempt is read twice for the same reason: as its row, and as the
// comment.delivery receipt its settle appends in the transaction that records the session
// (api's completeCommentDelivery), from which the outbox records that session as well
// (outbox.mentionReached). The attempt's row cannot exist without its comment, and the receipt
// keeps its state and its session id in the log whatever becomes of either, so a session only a
// mention reached stays held to that receipt. A fallback copy's row carries no foreign key to its
// comment, and it holds its session as the attempt's receipt does.
//
// R14 is exact about which attempt is a delivery: an attempt the listener took is one, and a
// pending or failed one is not. So this binds the two states that mean it
// (model.DeliveryTakenStatesSQL), on an attempt's row and on its receipt alike, the test the
// writers bind too (graphstore.QueueReached, outbox.mentionReached): an attempt still pending has
// reached nobody yet and a failed one never will, and counting either here would hold the store to
// a node no writer is meant to have written. A queued attempt later answered is read by the same
// test before and after, so the flip to sent owes no node the settle did not already write.
//
// Only the comments the store holds name a session here (commentWriters), for the reason
// commentWriterPeople binds the same: a comment left out of the store records no author at all.
// The author of a run going_requests holds is a session here where the row's author is one, and
// the author of a reasoning node, or of a run whose row is gone, where its authored_by names one
// (storedAuthors).
//
// It is one definition read by two: the census's session row side and the backfill's session
// pass (backfillSessions), so the sessions the backfill gives a node are exactly the sessions the
// census holds to one, as addressedRoles is for roles. authors is the relation storedAuthors is
// read as: the census binds what it read once (censusAuthors), the backfill reads it in its own
// statement (inlineAuthors).
func recordedSessions(authors string) string {
	return `
	select id, min(event) as event, min(at) as at from (
		select written.actor->>'id' as id, null::bigint as event, written.at
		from (` + commentWriters + `) written
		where written.actor->>'kind' = 'session' and coalesce(written.actor->>'id', '') <> ''
		union all
		select r.author->>'id', null, r.created_at from going_requests r
		where r.author->>'kind' = 'session' and coalesce(r.author->>'id', '') <> ''
		union all
		select d.session_id, null, d.created_at from comment_deliveries d
		where coalesce(d.session_id, '') <> '' and d.state in ` + model.DeliveryTakenStatesSQL + `
		union all
		select d.session_id, null, d.created_at from message_deliveries d
		where coalesce(d.session_id, '') <> '' and d.state in ` + model.DeliveryTakenStatesSQL + `
		union all
		select substring(f.destination from ` + strconv.Itoa(len(contracts.AgentTopicPrefix)+1) + `), null, f.created_at
		from comment_fallback_copies f
		where f.published and f.destination like '` + contracts.AgentTopicPrefix + `%'
		union all
		select a.author->>'id', null, a.created_at from asks a
		where a.author->>'kind' = 'session' and coalesce(a.author->>'id', '') <> ''
		union all
		select f.session_id, null, f.since from ask_followers f
		union all
		select case when e.type in ('ask.follower_added', 'comment.delivery') then e.payload->>'session_id'
		            when e.payload->'author'->>'kind' = 'session' then e.payload->'author'->>'id' end,
		       e.id, e.created_at
		from events e
		where e.type in ('ask.follower_added', 'ask.opened')
		   or (e.type = 'comment.delivery' and e.payload->>'state' in ` + model.DeliveryTakenStatesSQL + `)
		union all
		select stored.id, null, stored.at from ` + authors + ` where stored.kind = '` + graphstore.KindSession + `'
	) sessions
	where id is not null and id <> ''
	group by id`
}

// addressedRoles are the roles a row of the store addresses now or an event of the log has ever
// addressed (R19), each once, with the first event that named it, null for a role only a row
// names. The rows are a comment mention's target, a message's target and an issue's route as they
// stand now. The events are the route every issue event recorded the issue under, the target
// every message event recorded, and the target of each mention a new comment's event named. A
// node is history and never goes, so an issue routed to one role and then re-routed to another
// still owes the first its node and its page: issues.route names only the second, and the
// issue's own events name both.
//
// It is one definition read by two: the census's role and page row side (roleNames)
// and the backfill's role pass (backfillRoles), so the roles the backfill gives a node are
// exactly the roles the census holds to one. The publisher writes a role's node and its page from
// those same fields of those same events (outbox.addressedRoles), whether or not it routed
// anything to the role. Every role a row names now was named by the event its write appended, so
// the two sides agree once the outbox has read the log; a role named by an event the outbox has
// not read yet is a row with no node until it does.
//
// It reads every event of six types, which no index narrows, so a census reads it once and binds
// the names into every statement that reads the role and page rows (projection.names).
const addressedRoles = `
	select name, min(event) as event from (
		select substring(d.target from 6) as name, null::bigint as event
		from comment_deliveries d where d.target like 'role:%'
		union all
		select substring(m.target from 6), null from messages m where m.target like 'role:%'
		union all
		select substring(i.route from 6), null from issues i where i.route like 'role:%'
		union all
		select substring(addressed.target from 6), e.id
		from events e
		cross join lateral (
			select e.payload->>'route' where e.type in ('issue.created', 'issue.updated', 'issue.closed')
			union all
			select e.payload->>'target' where e.type in ('message.created', 'message.answered')
			union all
			select mention->>'target'
			from jsonb_array_elements(case when e.type = 'comment.created'
			                                and jsonb_typeof(e.payload->'mentions') = 'array'
			                               then e.payload->'mentions' end) mention
		) addressed (target)
		where e.type in ('issue.created', 'issue.updated', 'issue.closed',
		                 'message.created', 'message.answered', 'comment.created')
		  and addressed.target like 'role:%'
	) roles
	where name <> ''
	group by name`

// roleNames is the role row side as a kind binds it (projection.names): the name of every role
// addressedRoles names, which the role kind and the page kind both read.
const roleNames = `select addressed.name from (` + addressedRoles + `) addressed`

// readNames reads the row side a kind binds (projection.names) once for a run of the census, in
// one statement, so the event log is scanned once however many statements then read the kind's
// rows; kind names the kind in an error, and args are what names binds, the stored authors for a
// kind whose names read them (projection.authored). It runs inside the census's snapshot, so every
// statement after it reads the names that snapshot holds.
func readNames(ctx context.Context, tx pgx.Tx, kind, names string, args []any) ([]string, error) {
	rows, err := tx.Query(ctx, names, args...)
	if err != nil {
		return nil, fmt.Errorf("read the %s row side: %w", kind, err)
	}
	defer rows.Close()
	read := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read a name of the %s row side: %w", kind, err)
		}
		read = append(read, name)
	}
	return read, rows.Err()
}

// storedWriters is storedAuthors as the census read it, column by column, for censusAuthors.
type storedWriters struct {
	kinds, ids []string
	ats        []time.Time
}

// readStoredAuthors reads storedAuthors once for a run of the census, in one statement, so the
// store's log its history arms read is read once however many statements then read the person and
// the session row sides, whatever plan the planner takes for those arms. It runs inside the
// census's snapshot, as readNames does, so every statement after it reads what that snapshot holds.
func readStoredAuthors(ctx context.Context, tx pgx.Tx) (*storedWriters, error) {
	rows, err := tx.Query(ctx, `select stored.kind, stored.id, stored.at from `+inlineAuthors)
	if err != nil {
		return nil, fmt.Errorf("read who wrote the decisions, readings and asks the store holds: %w", err)
	}
	defer rows.Close()
	authors := &storedWriters{kinds: []string{}, ids: []string{}, ats: []time.Time{}}
	for rows.Next() {
		var kind, id string
		var at time.Time
		if err := rows.Scan(&kind, &id, &at); err != nil {
			return nil, fmt.Errorf("read one who wrote a decision, a reading or an ask: %w", err)
		}
		authors.kinds, authors.ids, authors.ats = append(authors.kinds, kind), append(authors.ids, id), append(authors.ats, at)
	}
	return authors, rows.Err()
}
