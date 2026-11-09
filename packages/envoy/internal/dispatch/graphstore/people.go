package graphstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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

// Authored writes who wrote one thing, inside the caller's transaction and beside the event that
// stored it: the author's own node with the two base attributes every node carries, the aliases
// the server proved of them, and the authored_by tuple from the thing to its author. A write
// whose actor names nobody writes nothing.
//
// It goes through the same arms the publisher's record splices in (WriteArms), so an author's
// node is made once, by the write that first records them, and a later write of the same person
// or session moves nothing: what Dispatch holds of them is not this comment's to restate. The
// tuples it appends carry the event as their cause, under which a repeat appends nothing.
//
// It writes no page for a session: a session's own page belongs to a session that claimed no
// role, and whether a run holds a role is resolved where the roles are read (outbox.recordBatch),
// never here.
func Authored(ctx context.Context, tx pgx.Tx, kind, id string, actor model.Actor, at time.Time, event int64) error {
	authorKind, authorID, ok := AuthorOf(actor)
	if !ok {
		return nil
	}
	nodes := []Node{agentNode(authorKind, authorID, actor, at, event)}
	tuples := []Tuple{{
		FromKind: kind, FromID: id, Kind: TupleAuthoredBy,
		ToKind: authorKind, ToID: authorID,
		At: at, Who: actor, SourceEvent: event, Asserted: true,
	}}
	tuples = append(tuples, aliases(authorKind, authorID, actor, at, event)...)
	arms, params, err := WriteArms(1, nodes, tuples, nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
		return fmt.Errorf("write who wrote %s %s: %w", kind, id, err)
	}
	return nil
}

// aliases are the tuples that tie a person to how this write names them: the session the server
// itself proved theirs, which only a credential acting as its person carries, and the raw actor
// id the record stored where that is not the canonical login. A session author has neither: a
// session is nobody's until a credential proves it.
func aliases(authorKind, authorID string, actor model.Actor, at time.Time, event int64) []Tuple {
	if authorKind != KindPerson {
		return nil
	}
	var tuples []Tuple
	if actor.Act != nil && actor.Act.Session != "" {
		tuples = append(tuples, Tuple{
			FromKind: KindSession, FromID: actor.Act.Session, Kind: TupleAliasOf,
			ToKind: KindPerson, ToID: authorID,
			At: at, Who: actor, SourceEvent: event, Asserted: true,
		})
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
