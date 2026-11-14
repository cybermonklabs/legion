package graphstore

import (
	"context"
	"fmt"
	"strings"
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

// AuthorFacts is what this store holds of who wrote one thing: the author's own node, made by
// actor at at and caused by event as the write that first records them makes it, and the tuples
// that write states with it - the authored_by tuple from the thing to its author, then the
// aliases the server proved of them. named is false for an actor that names nobody (AuthorOf),
// of whom nothing is held.
//
// It is the one statement of these facts. Authored writes them on their own; a writer that
// writes the thing's own node in a write of its own puts this node beside that one and these
// tuples beside its own, so the write takes every node row before it appends a tuple, as the
// lock order on api/inbox_read.go's markInboxReadThreads has it (graphcensus's
// writeCommentBatch).
func AuthorFacts(kind, id string, actor model.Actor, at time.Time, event int64) (node Node, tuples []Tuple, named bool) {
	authorKind, authorID, ok := AuthorOf(actor)
	if !ok {
		return Node{}, nil, false
	}
	tuples = []Tuple{{
		FromKind: kind, FromID: id, Kind: TupleAuthoredBy,
		ToKind: authorKind, ToID: authorID,
		At: at, Who: actor, SourceEvent: event, Asserted: true,
	}}
	tuples = append(tuples, aliases(authorKind, authorID, actor, at, event)...)
	return agentNode(authorKind, authorID, actor, at, event), tuples, true
}

// Authored writes who wrote one thing, inside the caller's transaction and beside the event that
// stored it: the author's own node with the two base attributes every node carries, the aliases
// the server proved of them, and the authored_by tuple from the thing to its author
// (AuthorFacts). A write whose actor names nobody writes nothing.
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
	node, tuples, named := AuthorFacts(kind, id, actor, at, event)
	if !named {
		return nil
	}
	arms, params, err := WriteArms(1, []Node{node}, tuples, nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
		return fmt.Errorf("write who wrote %s %s: %w", kind, id, err)
	}
	return nil
}

// DeliverySent is the one state of a delivery attempt that is a delivery: R14's "a sent attempt
// is a delivery; a pending or failed one is not".
const DeliverySent = "sent"

// Reached records the session a delivery attempt reached, inside the transaction that settles
// that attempt. R14 gives the store writer two first writes of a session id, and this is the
// second of them: "a comment whose author is a session, or a delivery attempt that reached one"
// (graphstore.Authored is the first). The moment an attempt reaches a session is the moment its
// send landed, which is where its settling transaction writes `sent`, so that transaction is
// where the node belongs.
//
// A pending or failed attempt is passed over, and so is an attempt that resolved no session:
// neither reached anybody, so neither records one. That is the same sentence the store census
// binds its session row side with (graphcensus's recordedSessions), so the writer and the census
// read one rule and a reader of either finds the other.
//
// It writes the node alone, through the same arms every other write of this store splices in
// (WriteArms): the node row is upserted `do nothing`, and the base attributes are stated only
// for a row that insert returned, so a resend of one attempt - and a second attempt reaching the
// same session - appends nothing. One node, so the ascending (kind, id) order a multi-node write
// takes is kept by having nothing to order. The write takes no event: the attempt's settle
// appends its receipt after this, so the node is made at the sentinel (NoEvent), under which a
// repeat appends nothing because the node row is already there.
func Reached(ctx context.Context, tx pgx.Tx, state, session string, who model.Actor, at time.Time) error {
	session = strings.TrimSpace(session)
	if state != DeliverySent || session == "" {
		return nil
	}
	arms, params, err := WriteArms(1, []Node{SessionNode(session, who, at, NoEvent)}, nil, nil)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "with "+arms+"\nselect 1", params...); err != nil {
		return fmt.Errorf("record the session %s a delivery reached: %w", session, err)
	}
	return nil
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
// token's, the shared bearer's - is no alias of anybody.
func aliases(authorKind, authorID string, actor model.Actor, at time.Time, event int64) []Tuple {
	alias := func(session, person string) Tuple {
		return Tuple{
			FromKind: KindSession, FromID: session, Kind: TupleAliasOf,
			ToKind: KindPerson, ToID: person,
			At: at, Who: actor, SourceEvent: event, Asserted: true,
		}
	}
	if authorKind == KindSession {
		if actor.Act == nil || actor.Act.Session != authorID || actor.Owner == nil ||
			model.CanonicalLogin(*actor.Owner) == "" {
			return nil
		}
		return []Tuple{alias(authorID, model.CanonicalLogin(*actor.Owner))}
	}
	if authorKind != KindPerson {
		return nil
	}
	var tuples []Tuple
	if actor.Act != nil && actor.Act.Session != "" {
		tuples = append(tuples, alias(actor.Act.Session, authorID))
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
