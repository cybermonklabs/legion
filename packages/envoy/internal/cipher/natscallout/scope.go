package natscallout

import (
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// scopeGrant is grant, entry's kind's grant, held to the projects entry may reach, reading only
// its own replies (ownReplies). An all-projects entry takes the grant whole. Any other entry would
// have to be held to the projects the broker's own exchange list gives its account and the
// repositories Dispatch maps them to, which this service does not read, so it refuses that entry's
// connection by name rather than admit it to every project.
func scopeGrant(entry ServiceEntry, grant natsgrant.Permissions) (natsgrant.Permissions, error) {
	if entry.AllProjects {
		return ownReplies(entry, grant), nil
	}
	return natsgrant.Permissions{}, refuse(labelHeldToProjects, "held to exchange projects, which this broker cannot read: service "+entry.name())
}

// everyReply is the subscribe nats.go's own inboxes take, where every client's replies arrive.
const everyReply = nats.InboxPrefix + ">"

// ownReplies is grant reading only the replies of entry's own account: each subscribe to _INBOX.>,
// where every client's replies arrive and so where one service would read another's JetStream
// answers and push deliveries, is written as the account's own reply prefix
// (natscred.InboxPrefix(entry.Account)+".>"), which its client sets (internal/bus, keptService);
// and a listener's and Dispatch's grant also subscribe natsgrant.SessionReplyInbox of that account,
// where the sessions they ask answer (the relay's receipts, the connection probe's answer and the
// agent stream's replay). entry.Account is the account its verified token names, as the client
// reads it from the same token (natscred.Identity). A static grant a hub renders is not this
// one: it keeps _INBOX.>, and a hub that renders static users holds no private project.
//
// A subscribe that is not _INBOX.> is kept as it is: the measurement job's own fixed prefix
// (natsgrant.Measure) among them.
func ownReplies(entry ServiceEntry, grant natsgrant.Permissions) natsgrant.Permissions {
	subscribe := make([]string, 0, len(grant.Subscribe)+1)
	for _, subject := range grant.Subscribe {
		if subject == everyReply {
			subject = natscred.InboxPrefix(entry.Account) + ".>"
		}
		subscribe = append(subscribe, subject)
	}
	switch entry.Kind {
	case KindListener, KindDispatch:
		subscribe = append(subscribe, natsgrant.SessionReplyInbox(entry.Account)+".>")
	}
	grant.Subscribe = subscribe
	return grant
}
