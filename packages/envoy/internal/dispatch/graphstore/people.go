package graphstore

import (
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The node kinds of the people and the agents Dispatch records. Each is a supertag of the
// closed list (migration 20261109051232), and none of them carries a `dispatch://` address:
// model.ParseRoute's `role:<topic>` and `session:<id>` are delivery routes, not addresses.
//
// A person is their login, lower-cased and trimmed (model.CanonicalLogin, and user_threads_login
// in SQL), which is the one answer to who a person is. A session is one run of an agent, named
// by its session id, and a role is the topic token a run claims, the only agent address Dispatch
// keeps across a restart: the role is therefore the agent, and the page under it is the agent's
// page. A session that claimed no role carries a page of its own instead.
const (
	KindPerson  = "person"
	KindSession = "session"
	KindRole    = "role"
	KindPage    = "page"
)

// The tuple kinds those nodes are tied by.
//
// A thing points at whoever wrote it with TupleAuthoredBy, and at everyone it reached with
// TupleDeliveredTo - a delivery to a role ends at the role, and reaches a person only through
// the session that attempt recorded. A role points at the session holding it with TupleHeldBy,
// which ends with a retraction when a later resolution reads the role unclaimed or its holder
// gone. TuplePartOf hangs a page under the role or session whose page it is, and a session
// under the role it claimed while it ran.
//
// The two that tie a person to how a record spells them: TupleAliasOf points from a session the
// server itself proved to be a person's - a credential that acted as its person, model.Actor.Act
// - to that person, and an actor that merely claims a session earns none. TupleRecordedAs
// carries the raw actor id a record stored, as a value, for a login whose stored spelling is not
// the canonical one.
const (
	TupleAuthoredBy  = "authored_by"
	TupleDeliveredTo = "delivered_to"
	TupleHeldBy      = "held_by"
	TuplePartOf      = "part_of"
	TupleAliasOf     = "alias_of"
	TupleRecordedAs  = "recorded_as"
)

// RolePageID and SessionPageID are the ids of the two pages there are: the page of a role, and
// the page of a session that claimed none. A page is named after whose it is, as an attribute
// node is named after its supertag, so the two cannot collide and neither needs a read to say
// which it is - though a reader follows the TuplePartOf tuple rather than the spelling of an id.
func RolePageID(role string) string { return KindRole + "/" + role }

func SessionPageID(session string) string { return KindSession + "/" + session }

// PersonNode, SessionNode, RoleNode and PageNode are the four nodes this store holds of people
// and agents, as the write that first records one makes it: project-visible, live, carrying no
// attribute of their own, and made by the actor of that write at its time.
func PersonNode(login string, who model.Actor, at time.Time, event int64) Node {
	return agentNode(KindPerson, model.CanonicalLogin(login), who, at, event)
}

func SessionNode(session string, who model.Actor, at time.Time, event int64) Node {
	return agentNode(KindSession, session, who, at, event)
}

func RoleNode(role string, who model.Actor, at time.Time, event int64) Node {
	return agentNode(KindRole, role, who, at, event)
}

func PageNode(id string, who model.Actor, at time.Time, event int64) Node {
	return agentNode(KindPage, id, who, at, event)
}

func agentNode(kind, id string, who model.Actor, at time.Time, event int64) Node {
	return Node{
		Kind:        kind,
		ID:          id,
		Supertag:    kind,
		Visibility:  VisibilityProject,
		State:       StateLive,
		MadeAt:      at,
		MadeBy:      who,
		SourceEvent: event,
	}
}

// AuthorOf is the node that wrote something, from the actor a record stored: the person their
// login names, or the session one run of an agent is. A credential that names neither - the
// deployment's shared token, which resolves as {} - wrote it as nobody this store can name, and
// ok is false. A personal token is recorded as the person it acted for (api.actingPerson), and
// the session it acted from rides along on Act, which Aliases ties to that person.
func AuthorOf(actor model.Actor) (kind, id string, ok bool) {
	switch {
	case actor.Kind == "user" && actor.ID != "":
		return KindPerson, model.CanonicalLogin(actor.ID), true
	case actor.Kind == "session" && actor.ID != "":
		return KindSession, actor.ID, true
	case actor.Owner != nil && *actor.Owner != "":
		// A personal token with no session of its own: the person it belongs to is who the
		// server proved, and is who wrote this.
		return KindPerson, model.CanonicalLogin(*actor.Owner), true
	}
	return "", "", false
}

// AuthorFacts is what this store holds of who wrote one thing: the author's own node, made by
// actor at at and caused by event as the write that first records them makes it, and the tuples
// that write states with it - the authored_by tuple from the thing to its author, then the
// aliases the server proved of them. named is false for an actor that names nobody (AuthorOf),
// of whom nothing is held.
//
// A session the server proved its owner's is tied to that person by an alias_of tuple, and the
// person is a node of the store from that same write on (provedOwner): a tuple whose far end
// names no node would leave a person with deliveries on no page, since the page of a login the
// store holds no person of answers nothing (api's listPersonNodes). So nodes is the author's node
// and, for such a session, its owner's node after it.
//
// It is the one statement of these facts. QueueAuthored writes them on their own; a writer that
// writes the thing's own node in a write of its own puts these nodes beside that one and these
// tuples beside its own, so the write takes every node row before it appends a tuple, as the
// lock order on api/inbox_read.go's markInboxReadThreads has it (graphcensus's
// writeCommentBatch).
func AuthorFacts(kind, id string, actor model.Actor, at time.Time, event int64) (nodes []Node, tuples []Tuple, named bool) {
	authorKind, authorID, ok := AuthorOf(actor)
	// A session id carries no length bound (R14), so a comment whose author is a session past
	// the store's address bound names an author the store cannot key beside the thing: its node
	// is not written, nor the authored_by tuple nor any alias, and named is false. The comment
	// keeps its own row, which the old comments arm answers (api's commentStoreWrite).
	if !ok || !AddressFits(authorID) {
		return nil, nil, false
	}
	tuples = []Tuple{{
		FromKind: kind, FromID: id, Kind: TupleAuthoredBy,
		ToKind: authorKind, ToID: authorID,
		At: at, Who: actor, SourceEvent: event, Asserted: true,
	}}
	tuples = append(tuples, aliases(authorKind, authorID, actor, at, event)...)
	nodes = []Node{agentNode(authorKind, authorID, actor, at, event)}
	if authorKind == KindSession {
		if owner, proved := provedOwner(authorID, actor); proved {
			nodes = append(nodes, PersonNode(owner, actor, at, event))
		}
	}
	return nodes, tuples, true
}

// provedOwner is the person a session author's credential proved the session belongs to: a token
// the secrets broker scoped to a person, recorded as the session it proved with that session on
// its Act and the person as its Owner (api.bearerSessionActor). A session named in a body carries
// no Act, and so has no owner here.
func provedOwner(session string, actor model.Actor) (string, bool) {
	if actor.Act == nil || actor.Act.Session != session || actor.Owner == nil {
		return "", false
	}
	owner := model.CanonicalLogin(*actor.Owner)
	return owner, owner != ""
}

// provedSession is the session a person's act names where the server itself proved it: the
// session a broker-issued token is bound to, which api.actingPerson copies onto the act and which
// no request can name otherwise. A personal token proves no session, so the one its act names is
// whatever the caller claimed - a `?session=` parameter (api.actingPerson) or a body actor
// (api.actSessionFromBody) - and the act says so by naming the token: only a personal token's act
// carries a TokenID.
func provedSession(actor model.Actor) (string, bool) {
	if actor.Act == nil || actor.Act.TokenID != "" || actor.Act.Session == "" {
		return "", false
	}
	return actor.Act.Session, true
}

// QueueAuthored queues on a write phase's batch who wrote one thing, beside the event that stored
// it: the author's own node with the two base attributes every node carries, the aliases the
// server proved of them, and the authored_by tuple from the thing to its author (AuthorFacts). A
// write whose actor names nobody writes nothing. For a write whose event that phase appends
// before it, event is LatestEvent.
//
// It goes through the same arms the publisher's record splices in (WriteArms), so an author's
// node is made once, by the write that first records them, and a later write of the same person
// or session moves nothing: what Dispatch holds of them is not this comment's to restate. The
// tuples it appends carry the event as their cause, under which a repeat appends nothing.
//
// It writes no page for a session: a session's own page belongs to a session that claimed no
// role, and whether a run holds a role is resolved where the roles are read (outbox.recordBatch),
// never here.
func QueueAuthored(b *store.WriteBatch, kind, id string, actor model.Actor, at time.Time, event int64) error {
	nodes, tuples, named := AuthorFacts(kind, id, actor, at, event)
	if !named {
		return nil
	}
	arms, params, err := WriteArms(1, nodes, tuples, nil)
	if err != nil {
		return err
	}
	b.Queue(fmt.Sprintf("write who wrote %s %s", kind, id), "with "+arms+"\nselect 1", params...)
	return nil
}

// QueueReached queues the record of the session a delivery attempt reached on the batch of the
// transaction that settles that attempt. R14 gives the store writer three first writes of a
// session id: "a comment whose author is a session, an ask a session opened or follows, or a
// delivery attempt that reached one" (QueueAuthored and asks.Follow are the other two). The
// moment an attempt reaches a session is the moment the listener took its send, which is where
// its settling transaction writes that state, so that transaction is where the node belongs.
//
// An attempt the listener took is a delivery, which model.DeliveryTaken answers for: `sent` to
// a session that is running, and `queued` for one that is not, which reaches it when it runs
// again. A pending or failed attempt is passed over, and so is an attempt that resolved no
// session: neither reached anybody, so neither records one. That is the same sentence the store
// census binds its session row side with (graphcensus's recordedSessions), so the writer and
// the census read one rule and a reader of either finds the other. The later move of a queued
// attempt to answered states nothing new, the node being there from the settle on.
//
// It writes the node alone (QueueSessionRecorded), with the cause in hand or not. A settle that
// appends its receipt earlier in the same batch passes LatestEvent: the node is then stated under
// the receipt in its one statement, and the write answered holds nothing. A caller that passes
// NoEvent queues the receipt after this and moves the node onto it (QueueCaused): the node is
// made at the sentinel and answered as a write held for its cause (Uncaused), as a comment's node
// learns the event that stored it, and the node's two base attributes are appended there under
// that same event. A resend of one attempt, and a second attempt reaching the same session,
// finds the node there and answers none, so the settle moves nothing another write made.
func QueueReached(b *store.WriteBatch, state, session string, who model.Actor, at time.Time, event int64) (Uncaused, error) {
	if !model.DeliveryTaken(state) {
		return Uncaused{}, nil
	}
	made, err := QueueSessionRecorded(b, strings.TrimSpace(session), who, at, event)
	if err != nil {
		return Uncaused{}, fmt.Errorf("record the session %s a delivery reached: %w", session, err)
	}
	return made, nil
}

// QueueSessionRecorded queues the node of a session a write records on that write's batch, and
// answers the write held for its cause: the node, which the cause moves only where this write
// made it. An attempt that reached a session records it here (QueueReached). A follow of an ask
// records it with the same node arm spliced into its follower insert (asks.Follow), so that a
// follow costs one statement.
//
// It goes through the same arms every other write of this store splices in (WriteArms): the node
// row is upserted `do nothing`, which locks no row that is already there. One node, so the
// ascending (kind, id) order a multi-node write takes is kept by having nothing to order.
//
// Where event is NoEvent the cause comes later: the writer appends its event after this and
// moves the node onto it (QueueCaused over the answered write). The base attributes are then no
// part of the statement here (WriteArmsUncaused): the node is made marked as made (nodeMarks),
// and the append that comes with its cause states them, because no tuple may be updated to learn
// its cause afterwards and a node row moved onto the event would otherwise name an event its own
// base tuples do not. Where event names the write's own cause they are stated in the arm, under
// it, and the write answered holds nothing: a node this write did not make is another write's,
// keeps that write's cause, and is described by that write's own base tuples.
//
// A session id carries no length bound, and a session past the store's address bound is no node
// the store can key: nothing is written for it, as the comment path writes none for a session
// author past the bound (AuthorFacts). The census leaves such a session out of both sides
// (graphcensus's recordedSessions).
func QueueSessionRecorded(b *store.WriteBatch, session string, who model.Actor, at time.Time, event int64) (Uncaused, error) {
	if session == "" || !AddressFits(session) {
		return Uncaused{}, nil
	}
	nodes := []Node{SessionNode(session, who, at, event)}
	// The arms state the node's base attributes where this write's cause is in hand, and state
	// none where it comes later, which the write answered holds for it instead.
	var (
		arms   string
		params []any
		write  Uncaused
		err    error
	)
	if event == NoEvent {
		arms, params, write, err = WriteArmsUncaused(1, nodes, nil, nil)
	} else {
		arms, params, err = WriteArms(1, nodes, nil, nil)
	}
	if err != nil {
		return Uncaused{}, err
	}
	b.Queue(fmt.Sprintf("record the session %s", session), "with "+arms+"\nselect count(*) from graph_nodes_written", params...)
	return write, nil
}

// aliases are the tuples that tie a person to how this write names them: the session the server
// itself proved theirs, and the raw actor id the record stored where that is not the canonical
// login. A session an actor merely claims earns neither, which is R14's rule: a session is
// nobody's until a credential proves it.
//
// The proved session reaches here two ways, and both are one fact - the credential proved the
// session and proved whose it is.
//
//   - The author is the person, and the agent that acted for them is named beside them: a
//     credential acting as its person (api.actingPerson), whose Act carries the session.
//   - The author is the session itself, and the credential that proved it names the person it
//     belongs to: a token the secrets broker scoped to a person, recorded as the session it
//     proved with that session on its Act (api.bearerSessionActor). Authorship stays the
//     session's - a thing a session wrote is on no person's page - and the alias is what makes
//     a delivery to that session reach its person (api's personNodesQuery).
//
// A credential that proved no session carries no Act, so a body-named session - a personal
// token's, the shared bearer's - is no alias of anybody, and nor is the session a personal
// token's act names, which its caller claimed (provedSession).
func aliases(authorKind, authorID string, actor model.Actor, at time.Time, event int64) []Tuple {
	alias := func(session, person string) Tuple {
		return Tuple{
			FromKind: KindSession, FromID: session, Kind: TupleAliasOf,
			ToKind: KindPerson, ToID: person,
			At: at, Who: actor, SourceEvent: event, Asserted: true,
		}
	}
	if authorKind == KindSession {
		owner, proved := provedOwner(authorID, actor)
		if !proved {
			return nil
		}
		return []Tuple{alias(authorID, owner)}
	}
	if authorKind != KindPerson {
		return nil
	}
	var tuples []Tuple
	// The proved session's own id has no length bound, so an alias whose session end is past the
	// store's address bound is not written: the session is no node of the store either, and a
	// delivery to it reaches its person by no alias (api's personNodesQuery). The author remains
	// a named person, so its node and its authored_by tuple still stand.
	if session, proved := provedSession(actor); proved && AddressFits(session) {
		tuples = append(tuples, alias(session, authorID))
	}
	if actor.ID != "" && actor.ID != authorID {
		tuples = append(tuples, Tuple{
			FromKind: KindPerson, FromID: authorID, Kind: TupleRecordedAs,
			ToKind: KindValue, ToID: actor.ID,
			At: at, Who: actor, SourceEvent: event, Asserted: true,
		})
	}
	return tuples
}
