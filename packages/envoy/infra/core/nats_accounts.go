package core

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/bus"
)

// The accounts of a private render. JetStream sits in JetStreamAccount, which no client connects
// to, and every user the authorization names, the auth service's among them, sits in
// ClientAccount, which reaches JetStream through imports. The auth service places each client it
// admits in the account its answer's audience names: ClientAccount once the broker's
// BROKER_NATS_ACCOUNT names it (the auth service's t-e-73).
const (
	JetStreamAccount = "JETSTREAM"
	ClientAccount    = "ENVOY"
)

// Why the accounts. A JetStream API request is answered on the request's reply subject, in the
// account the request was made in. In one account a client allowed any JetStream API request can
// aim the reply at a subject a stream captures, and the stream stores the answer there: a
// stream_info_response on a private project's issue subject, or a KV key. A client that holds a
// KV write and a direct get chooses the stored bytes too: it writes a key and asks for it with
// the reply aimed at a captured subject. Its grant cannot stop it, since
// the reply subject is no subject the client publishes.
//
// A request that crosses a service import carries a reply subject of the exporting account's own,
// a fresh _R_. subject, and its answer is delivered back on the requester's reply subject in the
// requester's account (nats-server v2.15.1 server/accounts.go addRespServiceImport,
// newServiceReply). So a reply aimed at a captured subject is answered in ClientAccount, where no
// stream is, and JetStream never publishes on it in JetStreamAccount. A captured subject is reached
// only by a client's own publish through a service import, which its grant is checked against as
// before. Subjects, grants and inbox prefixes keep their names; only the account changes.

// jetStreamServices are the services JetStreamAccount exports and ClientAccount imports: every
// subject a client publishes to that JetStream answers or stores.
//   - $JS.API.> is every JetStream and KV call, answered as a stream of responses: the server
//     drops a singleton response's mapping after its first reply (server/accounts.go:268-275,
//     :2147-2159; server/client.go:5171-5192), so a pull consumer's fetch of five would get one;
//   - $JS.ACK.> is a consumer's acknowledgements, $JS.FC.> an ordered consumer's flow-control
//     replies, and $KV.> a KV write, each published to a subject of JetStreamAccount;
//   - every subject the notification stream carries (bus.StreamCarriedSubjects): its notification
//     subjects and the listener's own kept, resume and takeover lanes, which the listener publishes
//     to. Never all of notifications.>, so a core subject the stream does not carry, a session's
//     relay subject notifications.envoy.deliver.<session> among them, stays in ClientAccount;
//   - measureRoute, the subjects envoy-measure publishes to its own stream, ENVOY_MEASURE
//     (natsgrant.Measure).
func jetStreamServices() []string {
	services := []string{"$JS.API.>", "$JS.ACK.>", "$JS.FC.>", "$KV.>"}
	services = append(services, bus.StreamCarriedSubjects()...)
	return append(services, measureRoute)
}

// streamedResponses is the one service whose responses come as a stream.
const streamedResponses = "$JS.API.>"

// measureRoute is envoy-measure's publish route into ENVOY_MEASURE, as natsgrant.Measure grants it.
const measureRoute = "envoy-measure.>"

// replyRoot begins the reply prefix of every service, the operator's and envoy-measure's, once the
// auth service grants them under one token (its t-e-74: natsgrant.ReplyRoot).
// It is written here until that name is in natsgrant.
const replyRoot = "_INBOX_envoy"

// jetStreamStreams are the streams JetStreamAccount exports and ClientAccount imports: the
// subjects JetStream publishes to itself, inside JetStreamAccount, that a client receives. A push
// or ordered consumer's deliver subject (a KV watch's included: nats.go takes it from the
// connection's inbox prefix) and a stream's republish are published there, and reach a client only
// through an export. _INBOX.> carries what reaches a client on nats.go's default prefix, a
// session's and the listener's push durable's; replyRoot's carries what reaches a service, the
// operator and envoy-measure, whose prefixes t-e-74 puts under it. A request's reply needs no
// export: it comes back through the service import's response mapping, whatever its subject.
func jetStreamStreams() []string {
	return []string{"_INBOX.>", replyRoot + ".>"}
}

// validateJetStreamOperator refuses a JetStreamOperator the render would place otherwise than it
// says, each refusal a core.InputError naming jetStreamOperator: one set without PrivateProjects,
// whose render has no JetStreamAccount; one naming no keyed user of users; and one naming the auth
// service's user, which must sit in ClientAccount, where the callout runs.
func (a Authorization) validateJetStreamOperator() error {
	if a.JetStreamOperator == "" {
		return nil
	}
	if !a.PrivateProjects {
		return InputErrorf("jetStreamOperator", "jetStreamOperator %q: only a render with privateProjects has the %s account it names a user of", a.JetStreamOperator, JetStreamAccount)
	}
	if u, ok := a.user(a.JetStreamOperator); !ok || u.NKey == "" {
		return InputErrorf("jetStreamOperator", "jetStreamOperator %q names no keyed user: the operator of %s is one of users, with an nkey", a.JetStreamOperator, JetStreamAccount)
	}
	if a.Callout != nil && a.Callout.ServiceUser == a.JetStreamOperator {
		return InputErrorf("jetStreamOperator", "jetStreamOperator %q is callout.serviceUser: the auth service connects in %s, where the callout runs", a.JetStreamOperator, ClientAccount)
	}
	return nil
}

// accountsBlock is a private render's accounts block, which holds the users in place of the
// authorization block: JetStreamAccount, with JetStream on, the JetStreamOperator user alone, and
// its exports; then ClientAccount, with every other keyed user of users in their order, and an
// import of each export. ClientAccount sets no jetstream key, so it has none of its own: its
// JetStream calls reach JetStreamAccount through the $JS.API.> import, and without the import they
// are answered "JetStream not enabled for account" (measured on v2.15.1).
func (a Authorization) accountsBlock() string {
	var operator, clients []User
	for _, u := range a.Users {
		switch {
		case u.NKey == "":
		case u.Name == a.JetStreamOperator:
			operator = append(operator, u)
		default:
			clients = append(clients, u)
		}
	}
	var b strings.Builder
	b.WriteString("accounts {\n")
	b.WriteString("  # JetStream sits in an account no client connects to: a JetStream answer aimed at a subject a stream captures is answered in " + ClientAccount + ", where no stream is\n")
	b.WriteString("  " + JetStreamAccount + " {\n    jetstream: enabled\n")
	writeAccountUsers(&b, operator)
	b.WriteString("    exports: [\n")
	sorted := PyJSONOptions{SortKeys: true}
	for _, service := range jetStreamServices() {
		export := Object{{"service", service}}
		if service == streamedResponses {
			export = append(export, Field{"response_type", "stream"})
		}
		b.WriteString("      " + PyJSON(export, sorted) + "\n")
	}
	for _, stream := range jetStreamStreams() {
		b.WriteString("      " + PyJSON(Object{{"stream", stream}}, sorted) + "\n")
	}
	b.WriteString("    ]\n  }\n")
	b.WriteString("  " + ClientAccount + " {\n")
	writeAccountUsers(&b, clients)
	b.WriteString("    imports: [\n")
	for _, service := range jetStreamServices() {
		b.WriteString("      " + PyJSON(Object{{"service", Object{{"account", JetStreamAccount}, {"subject", service}}}}, sorted) + "\n")
	}
	for _, stream := range jetStreamStreams() {
		b.WriteString("      " + PyJSON(Object{{"stream", Object{{"account", JetStreamAccount}, {"subject", stream}}}}, sorted) + "\n")
	}
	b.WriteString("    ]\n  }\n}\n")
	return b.String()
}

// writeAccountUsers writes an account's users list, each user as the authorization block writes
// one (userLines), or nothing for none: an account may hold no user.
func writeAccountUsers(b *strings.Builder, users []User) {
	if len(users) == 0 {
		return
	}
	b.WriteString("    users: [\n")
	for _, u := range users {
		b.WriteString(userLines(u, "      "))
	}
	b.WriteString("    ]\n")
}

// userLines are one keyed user as a users list writes it, each line after indent: a comment naming
// it, then one JSON object with its key and its permissions.
func userLines(u User, indent string) string {
	user := Object{
		{"nkey", u.NKey},
		{"permissions", Object{{"publish", u.Publish.object()}, {"subscribe", u.Subscribe.object()}}},
	}
	return fmt.Sprintf("%s# %s\n%s%s\n", indent, u.Name, indent, PyJSON(user, PyJSONOptions{SortKeys: true}))
}
