package outbox

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// graphWriter is who the node and tuple store records as the writer of what a batch delivered.
// The event's own actor wrote the thing; that it reached a session is this process's fact and
// nobody's act, as the schema seed's writer is the seed (migration 20261109051232).
var graphWriter = model.Actor{Kind: "system", ID: "dispatch-outbox"}

// graphWrite is what one record states about the store: the nodes of the roles, sessions and
// pages it reached, the delivered_to tuples of what it delivered, and the claims - each role's
// holder and what hangs under what - which are written only where they change (graphstore.Claim).
type graphWrite struct {
	nodes  []graphstore.Node
	tuples []graphstore.Tuple
	claims []graphstore.Claim
}

// resolvedRole is a role of a batch as its resolution left it: the event that first routed to it
// in the order the batch reads its events, or, for a role no destination of the batch names, the
// event that first named it in its payload (addressedRoles), which is the cause the store records;
// the session holding it, empty for a role read unclaimed or whose holder is gone; whether the
// resolution reached the claim at all, which a role only a payload names never does; and when the
// publisher read that holder (roleForward.resolvedAt), which dates the claim. A claim that could
// not be read states nothing, and the role keeps the holder the store holds.
type resolvedRole struct {
	event      int64
	holder     string
	resolved   bool
	resolvedAt time.Time
}

// deliveredGraph is what the settled events of one record state about the people and agents of
// the store (graphstore): the role each event routed to with the session resolveRoleHolders
// found holding it, the session each sent mention attempt reached, and one delivered_to tuple
// per delivery of something this tree gives a node.
//
// A node and a tuple are separate facts here, and the two requirements part them.
//
// The node side is R19's: the row a store census holds a node against is "the set of distinct
// actors and sessions the tables this child projects hold", which for a session is a comment
// whose author it is and a delivery attempt the listener took against it. So a session is
// recorded here only where such a row exists - the receipt of a mention attempt the listener
// took (mentionReached) is that row's own record - and a destination this batch reached over the
// bus is not: a route delivery leaves no row in `comment_deliveries` or `message_deliveries`,
// so no table this child projects holds that session and a node for it would be a node the
// census rightly reports as projected by no row. The two writers that do see such a row record
// it there: graphstore.QueueAuthored for a comment's author, and graphstore.QueueReached where an
// attempt settles in a state the listener took it in.
//
// The tuple side is R1's: a tuple's far end is an address and never a foreign key, and a tuple
// may point at an address a later child writes. So the delivered_to tuple of a destination this
// batch reached is stated whatever node that session has yet. Its near end is the item the event
// is about (deliveredItem): a comment, or a message, whose node the message's own writer writes.
// So an event about either appends one tuple for each session or role it reached, and an event
// about anything else - an issue, an ask, a document, a project - appends none here.
//
// That arm is what events.published_destinations projects (R2's session half). The record that
// adds an event's reached topics to the array (recordBatch) appends, in the same statement, one
// delivered_to tuple from the event's item to the node each of those topics names
// (nodeOfSubject), caused by that event. So for an event deliveredItem names an item of, the
// session and role topics of its array are exactly the far ends of the delivered_to tuples this
// writer appended under it. The array holds two kinds of topic no tuple projects, each told apart
// by what it is, so neither reads as a difference: an owner topic (the issue's, the project's or
// the document's family, contracts.Envelope.Topic), which names no node and so starts with
// neither contracts.AgentTopicPrefix nor contracts.RoleTopicPrefix; and every session and role
// topic of an event deliveredItem names no item of, told by its type. Two deliveries this writer
// appends are no topic of any array, and rows of their own project them: a mention attempt the
// listener took (mentionReached), caused by its comment.delivery receipt, which is published to
// its owner topic alone and whose comment_deliveries row is that delivery's attempt; and a
// fallback copy (fallbackDelivered), caused by the comment.created event it is a copy of, whose
// destination that event never lists, since a destination a fallback row owns is no destination
// of the event (destinations.addDestination), and whose comment_fallback_copies row says it was
// published.
//
// A delivery is a destination this batch reached, and a mention attempt the listener took. A
// mention's is recorded under the comment.delivery receipt its settle appended (mentionReached),
// by the batch that reads that receipt: the receipt is the one event that says the listener took
// the attempt, and an attempt carries exactly one (api's settleDeliveryAttempt), so each taken
// attempt is recorded once, however long after its comment it settled. The comment's own event
// cannot record it. Its batch reads the attempt as it stands then, and nothing holds the comment back
// for a mention of a session it sends no copy to, so that batch commonly runs before the send is
// answered and a delivery that settled after it would be recorded nowhere; and stated by both
// events, one delivery would be two rows of the log. The outbox sends a session a mention
// delivered nothing, the mention having delivered it. An attempt still pending or failed
// delivered nothing, and its receipt states nothing here. A delivery to a role ends at the role
// node: the session it was forwarded to is what the role's held_by tuple names.
//
// A session's own page, and that page's part_of the session, are written only for a session that
// claimed no role (R14): a session that claimed one is part_of that role, and the role's page is
// its page. Whether it claimed one is what this batch resolved (held, below) or, for a role an
// earlier batch resolved, what the store already holds (inARole, which sessionsInARole reads
// before the record), since a receipt is commonly recorded in a batch that resolves no role at
// all. The session's node is written either way: the attempt's row names it.
//
// The roles and their pages are written whatever a row says of their holders, because their own
// row side is there: `comment_deliveries.target`, `messages.target` and `issues.route` each hold
// the role something was addressed to now, and the event log holds every role an event ever
// addressed (graphcensus's addressedRoles). A node is history and never goes, so each event's
// own roles are written beside the routes the batch resolved (addressedRoles): the route an
// issue event records, which an agent's write wakes nobody for and a re-route read in the same
// batch would leave unrouted, the target of a targeted message, which no route reaches, and the
// role a comment's mention names on an issue routed elsewhere. Nothing read such a role's claim,
// so it is written with its page and the page's part_of and no held_by; a role a destination of
// the batch resolved keeps that resolution.
//
// at dates every row the record states but two: a role's held_by and its holder's part_of the
// role are dated by when the batch resolved the role's holder (resolvedRole.resolvedAt), because
// the store orders those claims by that time (graphstore's claimArms). A record is written once
// its owner's sends are answered, and owners publish independently, so the order records land in
// is neither the order of their events nor the order their resolutions were made in.
func deliveredGraph(events []*batchEvent, inARole map[string]struct{}, at time.Time) graphWrite {
	roles := map[string]resolvedRole{}
	var roleOrder []string
	sessions := map[string]int64{}
	var sessionOrder []string
	var write graphWrite
	recordSession := func(id string, event int64) {
		if id == "" {
			return
		}
		if _, held := sessions[id]; !held {
			sessions[id] = event
			sessionOrder = append(sessionOrder, id)
		}
	}
	for _, event := range events {
		cause := event.event.ID
		for _, dest := range event.destinationsOf {
			if dest.role == "" || dest.forward == nil {
				continue
			}
			if _, held := roles[dest.role]; held {
				continue
			}
			holder, resolved := resolvedHolder(dest.forward)
			roles[dest.role] = resolvedRole{event: cause, holder: holder, resolved: resolved, resolvedAt: dest.forward.resolvedAt}
			roleOrder = append(roleOrder, dest.role)
		}
		kind, id, delivers := deliveredItem(event.event)
		for _, subject := range event.outcome.reached {
			toKind, toID, ok := nodeOfSubject(subject)
			if !ok {
				continue
			}
			// The tuple alone: this destination was reached over the bus, so no projected table
			// holds it and the node is not this writer's to make (R19). The far end is an
			// address, which R1 lets a tuple point at whether a node stands there yet or not.
			if delivers {
				write.tuples = append(write.tuples, deliveredTo(kind, id, toKind, toID, at, cause))
			}
		}
		if comment, session, sent := mentionReached(event.event); sent {
			recordSession(session, cause)
			write.tuples = append(write.tuples,
				deliveredTo(graphstore.KindComment, comment, graphstore.KindSession, session, at, cause))
		}
	}
	// After every destination, so that a role a destination resolved keeps its resolution and a
	// role only an event's payload names is written unresolved.
	for _, event := range events {
		for _, role := range addressedRoles(event.event) {
			if _, held := roles[role]; held {
				continue
			}
			roles[role] = resolvedRole{event: event.event.ID}
			roleOrder = append(roleOrder, role)
		}
	}
	held := map[string]struct{}{}
	for _, role := range roleOrder {
		claim := roles[role]
		write.nodes, write.claims = graphstore.RoleFacts(write.nodes, write.claims, role, graphWriter, at, claim.event)
		if !claim.resolved {
			continue
		}
		toKind := ""
		if claim.holder != "" {
			toKind = graphstore.KindSession
		}
		write.claims = append(write.claims, graphstore.Claim{
			FromKind: graphstore.KindRole, FromID: role, Kind: graphstore.TupleHeldBy,
			ToKind: toKind, ToID: claim.holder,
			At: claim.resolvedAt, Who: graphWriter, SourceEvent: claim.event,
		})
		// A session holds one role at a time, so the first role of this batch that resolved to
		// it is the one it is part of; a second would state two parents from one read. The
		// tuple alone, for the reason the node side states above: a holder this batch resolved
		// over the bus is in no table this child projects, so its node is not this writer's to
		// make and one written here would be a node the census rightly reports as projected by
		// no row. A holder a comment or a settled attempt does name takes its node from the
		// writer that saw that row (graphstore.QueueAuthored, graphstore.QueueReached).
		if _, holds := held[claim.holder]; claim.holder == "" || holds {
			continue
		}
		held[claim.holder] = struct{}{}
		write.claims = append(write.claims,
			partOf(graphstore.KindSession, claim.holder, graphstore.KindRole, role, claim.resolvedAt, claim.event))
	}
	for _, session := range sessionOrder {
		cause := sessions[session]
		write.nodes = append(write.nodes, graphstore.SessionNode(session, graphWriter, at, cause))
		_, holds := held[session]
		_, claimed := inARole[session]
		if holds || claimed {
			continue
		}
		page := graphstore.SessionPageID(session)
		write.nodes = append(write.nodes, graphstore.PageNode(page, graphWriter, at, cause))
		write.claims = append(write.claims,
			partOf(graphstore.KindPage, page, graphstore.KindSession, session, at, cause))
	}
	// A role name carries no length bound (model's roleRoutePattern), and the shared agent bearer
	// can route an issue to one: a role of 1,400 bytes once failed graph_tuples_once with
	// "index row size ... exceeds ... 2704" and the issue's outbox never drained again. The record
	// goes on without it. Every node, page and tuple whose address is past the store's bound is
	// left out, so the record still commits and the stream drains; the role's row keeps answering
	// from the old arm, and the census counts the address as a class of its own rather than a
	// difference (graphcensus). A role's own node, page and part_of are held to the bound where
	// they are stated (graphstore.RoleFacts): a role whose name fits but whose page (role/<name>)
	// does not keeps its node and loses its page, each address held to the bound on its own. The
	// rest - a role's held_by, a session and its page, a delivered_to - are left out here.
	write.nodes = slices.DeleteFunc(write.nodes, func(n graphstore.Node) bool {
		return !graphstore.AddressFits(n.ID)
	})
	write.tuples = slices.DeleteFunc(write.tuples, func(t graphstore.Tuple) bool {
		return !graphstore.AddressFits(t.FromID) ||
			(t.ToKind != graphstore.KindValue && !graphstore.AddressFits(t.ToID))
	})
	write.claims = slices.DeleteFunc(write.claims, func(c graphstore.Claim) bool {
		return !graphstore.AddressFits(c.FromID) || (c.ToID != "" && !graphstore.AddressFits(c.ToID))
	})
	return write
}

// resolvedHolder is the session a role's forward says holds it now, and whether the resolution
// reached the claim at all. A read that failed (heldStateUnresolved) reached none, and the store
// is told nothing rather than that the role is unclaimed; a holder that is not answering holds
// its claim still, which is the holder the forward carries.
func resolvedHolder(forward *roleForward) (string, bool) {
	if forward.heldState == heldStateUnresolved {
		return "", false
	}
	return forward.holder, true
}

// addressedRoles are the roles an event addresses by its payload, whether or not the batch routes
// it to them: the route an issue event records the issue under (model.IssueEventPayload), the
// target of a message event (model.MessageEventPayload), and the target of each mention a new
// comment names (model.CommentEventPayload). graphcensus's addressedRoles reads the same fields
// of the same events as a role's row side, so the census holds a role to a node exactly where
// this writes one.
func addressedRoles(event model.Event) []string {
	var roles []string
	add := func(target string) {
		if role, found := strings.CutPrefix(target, "role:"); found && role != "" {
			roles = append(roles, role)
		}
	}
	switch event.Type {
	case "issue.created", "issue.updated", "issue.closed":
		add(payloadString(event.Payload, "route"))
	case "message.created", "message.answered":
		add(payloadString(event.Payload, "target"))
	case "comment.created":
		values, _ := event.Payload.(map[string]any)
		mentions, _ := values["mentions"].([]any)
		for _, mention := range mentions {
			add(payloadString(mention, "target"))
		}
	}
	return roles
}

// deliveredTo is one delivery: the thing delivered, pointing at the session or the role it
// reached, caused by the event that carried it.
func deliveredTo(fromKind, fromID, toKind, toID string, at time.Time, event int64) graphstore.Tuple {
	return graphstore.Tuple{
		FromKind: fromKind, FromID: fromID, Kind: graphstore.TupleDeliveredTo,
		ToKind: toKind, ToID: toID,
		At: at, Who: graphWriter, SourceEvent: event, Asserted: true,
	}
}

// partOf hangs one node under another, as the claim it is: a page under whose page it is, and a
// session under the role it claimed, each ending when the store is told a different one.
func partOf(fromKind, fromID, toKind, toID string, at time.Time, event int64) graphstore.Claim {
	return graphstore.Claim{
		FromKind: fromKind, FromID: fromID, Kind: graphstore.TuplePartOf,
		ToKind: toKind, ToID: toID,
		At: at, Who: graphWriter, SourceEvent: event,
	}
}

// nodeOfSubject is the node of the store a destination's subject reaches: the session an agent
// subject names, or the role a role topic names. An owner's topic family reaches whoever
// subscribed to it, whom nothing records, so it reaches no node.
func nodeOfSubject(subject string) (kind, id string, ok bool) {
	if session, found := strings.CutPrefix(subject, contracts.AgentTopicPrefix); found {
		return graphstore.KindSession, session, session != ""
	}
	if role, found := strings.CutPrefix(subject, contracts.RoleTopicPrefix); found {
		return graphstore.KindRole, role, role != ""
	}
	return "", "", false
}

// deliveredItem is the node an event delivered: the comment or the message the event is about,
// which the event's payload names as `id` - a comment's event and a message's create and answer
// carry the item itself - or, on a delivery receipt and a message's acceptance, as
// `comment_id` or `message_id`. mentionReached is the other reading of a comment's receipt. An
// event about anything else reaches its destinations all the same and is recorded by no tuple of
// this writer (deliveredGraph).
func deliveredItem(event model.Event) (kind, id string, ok bool) {
	switch {
	case strings.HasPrefix(event.Type, "comment."):
		kind, id = graphstore.KindComment, cmp.Or(payloadString(event.Payload, "id"), payloadString(event.Payload, "comment_id"))
	case strings.HasPrefix(event.Type, "message."):
		kind, id = graphstore.KindMessage, cmp.Or(payloadString(event.Payload, "id"), payloadString(event.Payload, "message_id"))
	}
	return kind, id, id != ""
}

// mentionReached is the delivery a comment.delivery receipt states: the comment its attempt
// carried and the session that attempt reached. Only the receipt of an attempt the listener
// took against a session states one, R14's "an attempt the listener took is a delivery; a
// pending or failed one is not" (model.DeliveryTaken, which answers `sent` and `queued`): a
// failed attempt reached nobody, and a taken one whose resolution named no session reached
// nobody the store can name. A queued attempt is one the listener stored for a session that is
// not running, and it reaches that session when it runs again, so the later move of such an
// attempt to answered states nothing new. The session is trimmed as graphstore.QueueReached
// trims it, so the tuple points at the node that settle made.
func mentionReached(event model.Event) (comment, session string, ok bool) {
	if event.Type != "comment.delivery" || !model.DeliveryTaken(payloadString(event.Payload, "state")) {
		return "", "", false
	}
	comment = payloadString(event.Payload, "comment_id")
	session = strings.TrimSpace(payloadString(event.Payload, "session_id"))
	return comment, session, comment != "" && session != ""
}

// sessionsInARole are the sessions the mention receipts among events reached, the attempts the
// listener took (mentionReached), that the store already holds in a role (graphstore.ClaimedRoles,
// the one rule a message to a session reads its page by too, of every such session in one
// statement). A session that claimed a role carries no page of its own (deliveredGraph), and the
// batch that records a receipt commonly resolves no role, so its own resolution cannot say. The
// read runs under the statement gate the batch's other statements take (lanes.statement), and a
// read that fails fails the record: a page written or left out on a guess would stand for good,
// while a record that fails leaves its events to be read and recorded again.
func sessionsInARole(ctx context.Context, deps Deps, events []*batchEvent) (map[string]struct{}, error) {
	var sessions []string
	for _, event := range events {
		if _, session, sent := mentionReached(event.event); sent && !slices.Contains(sessions, session) {
			sessions = append(sessions, session)
		}
	}
	if len(sessions) == 0 {
		return nil, nil
	}
	var roles map[string]string
	if err := deps.lanes.statement(func() error {
		var err error
		roles, err = graphstore.ClaimedRoles(ctx, deps.Store.Pool, sessions)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read whether the sessions a mention reached claimed a role: %w", err)
	}
	inARole := make(map[string]struct{}, len(roles))
	for session := range roles {
		inARole[session] = struct{}{}
	}
	return inARole, nil
}
