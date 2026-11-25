package testnats

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

// CalloutAuthTimeout is how long a server CalloutConfig describes waits for the auth service's
// answer before it refuses the client, in seconds. It is nats-server's own default
// (AUTH_TIMEOUT, server/const.go), which the deployed hub takes by naming none, written here so
// the fixture's value is visible beside the configuration it renders. A longer one would buy
// nothing: a Go client's connect allows two seconds whatever the server waits, so the auth
// service answers well inside it (natscallout.decideBudget).
const CalloutAuthTimeout = 2

// CalloutKeys are the three keys a server with an auth callout and its auth service share: the
// service's own NATS user, which the server admits without a callout; the issuer, the account key
// that signs every answer and every user it admits; and the curve key the server seals each
// authorization request to.
type CalloutKeys struct {
	ServiceSeed, IssuerSeed, XKeySeed       string
	ServicePublic, IssuerPublic, XKeyPublic string
}

// NewCalloutKeys makes the three keys: the service a user key, the issuer an account key and the
// xkey a curve key (nkeys.CreateCurveKeys, whose public key is the X25519 key its seed opens with).
func NewCalloutKeys(t testing.TB) CalloutKeys {
	t.Helper()
	keys, err := MakeCalloutKeys()
	if err != nil {
		t.Fatalf("make the auth callout's keys: %v", err)
	}
	return keys
}

// MakeCalloutKeys is NewCalloutKeys for a caller with no test, natsd's -callout-keys.
func MakeCalloutKeys() (CalloutKeys, error) {
	var keys CalloutKeys
	for _, key := range []struct {
		create       func() (nkeys.KeyPair, error)
		seed, public *string
	}{
		{nkeys.CreateUser, &keys.ServiceSeed, &keys.ServicePublic},
		{nkeys.CreateAccount, &keys.IssuerSeed, &keys.IssuerPublic},
		{nkeys.CreateCurveKeys, &keys.XKeySeed, &keys.XKeyPublic},
	} {
		pair, err := key.create()
		if err != nil {
			return CalloutKeys{}, err
		}
		seed, err := pair.Seed()
		if err != nil {
			return CalloutKeys{}, err
		}
		public, err := pair.PublicKey()
		if err != nil {
			return CalloutKeys{}, err
		}
		*key.seed, *key.public = string(seed), public
	}
	return keys, nil
}

// StaticUser is an nkey user a callout server admits by its own configuration, listed in the
// callout's auth_users so the server never asks the service about it, with the grant the server
// holds it to.
type StaticUser struct {
	NKey               string
	Publish, Subscribe []string
}

// CalloutConfig is the configuration of a server with JetStream whose clients the auth service
// admits: the service's own user, which subscribes the authorization subject and publishes its
// answers to the server's reply inboxes; each static user with its grant; and an auth_callout
// block naming the issuer, the global account ($G) every admitted user is placed in, every static
// user (the service's among them) as an auth user, and the curve key the requests are sealed to.
// A client the configuration does not list is sent to the service. Keys with no XKeyPublic name
// no curve key, so the server sends its requests in the clear: the control a test of the sealing
// starts.
func CalloutConfig(keys CalloutKeys, static ...StaticUser) string {
	return calloutConfig(keys, nil, static)
}

// CalloutAccounts is a hub's two accounts as a private render lays them out (plan C's t-c-29,
// whose render is cybermonklabs/legion#4851's accounts block in packages/envoy/infra/core, a module
// this one cannot import): JetStream holds JetStream and no client, and Clients holds every user
// the configuration names, the auth service's among them, runs the auth callout, and imports from
// JetStream each of Services as a service and each of Streams as a stream. Streamed are the
// services of Services whose responses come as a stream rather than one reply. ToJetStream are the
// subjects Clients exports to JetStream as streams, which JetStream imports: a client's publish on
// one reaches JetStream's own subscriptions as it was sent, where a service import adds a
// Nats-Request-Info header to it.
type CalloutAccounts struct {
	JetStream, Clients                       string
	Services, Streamed, Streams, ToJetStream []string
}

// CalloutAccountsConfig is CalloutConfig in accounts' layout: the same users, grants and
// auth_callout block, with every user in accounts.Clients, which the callout names in place of
// the global account, and JetStream on in accounts.JetStream alone.
func CalloutAccountsConfig(keys CalloutKeys, accounts CalloutAccounts, static ...StaticUser) string {
	return calloutConfig(keys, &accounts, static)
}

// calloutAuthorization is CalloutConfig's authorization block alone, every user in the global
// account, which a cluster member's configuration takes after its own JetStream and cluster blocks
// (StartClusterWithCallout).
func calloutAuthorization(keys CalloutKeys, static ...StaticUser) string {
	return strings.TrimPrefix(calloutConfig(keys, nil, static), "jetstream {}\n")
}

// calloutConfig is CalloutConfig, with every user in the global account when accounts is nil, and
// in accounts' layout otherwise.
func calloutConfig(keys CalloutKeys, accounts *CalloutAccounts, static []StaticUser) string {
	users := []string{
		userEntry(keys.ServicePublic, []string{"$SYS._INBOX.>"}, []string{"$SYS.REQ.USER.AUTH"}),
	}
	authUsers := []string{fmt.Sprintf("%q", keys.ServicePublic)}
	for _, user := range static {
		users = append(users, userEntry(user.NKey, user.Publish, user.Subscribe))
		authUsers = append(authUsers, fmt.Sprintf("%q", user.NKey))
	}
	xkey := ""
	if keys.XKeyPublic != "" {
		xkey = fmt.Sprintf("\n    xkey: %q", keys.XKeyPublic)
	}
	// The callout block, naming the account every admitted user is placed in.
	callout := func(account string) string {
		return fmt.Sprintf("  auth_callout {\n    issuer: %q\n    account: %q\n    auth_users: [ %s ]%s\n  }",
			keys.IssuerPublic, account, strings.Join(authUsers, ", "), xkey)
	}
	if accounts == nil {
		return fmt.Sprintf(`jetstream {}
authorization {
  timeout: %d
  users = [
    %s
  ]
%s
}
`, CalloutAuthTimeout, strings.Join(users, "\n    "), callout("$G"))
	}
	var exports, imports, toJetStream, fromClients []string
	for _, service := range accounts.Services {
		export := fmt.Sprintf("{ service: %q }", service)
		if slices.Contains(accounts.Streamed, service) {
			export = fmt.Sprintf("{ service: %q, response_type: stream }", service)
		}
		exports = append(exports, export)
		imports = append(imports, fmt.Sprintf("{ service: { account: %q, subject: %q } }", accounts.JetStream, service))
	}
	for _, stream := range accounts.Streams {
		exports = append(exports, fmt.Sprintf("{ stream: %q }", stream))
		imports = append(imports, fmt.Sprintf("{ stream: { account: %q, subject: %q } }", accounts.JetStream, stream))
	}
	for _, stream := range accounts.ToJetStream {
		toJetStream = append(toJetStream, fmt.Sprintf("{ stream: %q }", stream))
		fromClients = append(fromClients, fmt.Sprintf("{ stream: { account: %q, subject: %q } }", accounts.Clients, stream))
	}
	// list is an account's imports or exports entry, or nothing when there are none.
	list := func(key string, entries []string) string {
		if len(entries) == 0 {
			return ""
		}
		return fmt.Sprintf("\n    %s: [\n      %s\n    ]", key, strings.Join(entries, "\n      "))
	}
	return fmt.Sprintf(`jetstream {}
accounts {
  %q {
    jetstream: enabled%s%s
  }
  %q {
    users = [
      %s
    ]%s%s
  }
}
authorization {
  timeout: %d
%s
}
`, accounts.JetStream, list("exports", exports), list("imports", fromClients), accounts.Clients, strings.Join(users, "\n      "),
		list("imports", imports), list("exports", toJetStream), CalloutAuthTimeout, callout(accounts.Clients))
}

// GrantedUsersConfig is the configuration of a server with JetStream whose whole authorization is
// users, each nkey user held to its grant's allow lists as a hub renders a static user: a client
// that names none of them is refused, since the configuration names no no_auth_user. A subscribe
// entry may carry a queue group after a space, as a grant writes one.
func GrantedUsersConfig(users ...StaticUser) string {
	entries := make([]string, len(users))
	for i, user := range users {
		entries[i] = userEntry(user.NKey, user.Publish, user.Subscribe)
	}
	return fmt.Sprintf("jetstream {}\nauthorization {\n  users = [\n    %s\n  ]\n}\n", strings.Join(entries, "\n    "))
}

// userEntry is one nkey user of a configuration, with its grant.
func userEntry(nkey string, publish, subscribe []string) string {
	return fmt.Sprintf("{ nkey: %q, permissions: { publish: { allow: [%s] }, subscribe: { allow: [%s] } } }",
		nkey, quoted(publish), quoted(subscribe))
}

func quoted(subjects []string) string {
	out := make([]string, len(subjects))
	for index, subject := range subjects {
		out[index] = fmt.Sprintf("%q", subject)
	}
	return strings.Join(out, ", ")
}

// StartWithCallout runs, in this process, the server CalloutConfig describes, shut down when the
// test ends, and returns its client URL once it admits the service's own user.
func StartWithCallout(t testing.TB, keys CalloutKeys, static ...StaticUser) (url string) {
	t.Helper()
	_, url = StartWithCalloutServer(t, keys, static...)
	return url
}

// StartWithCalloutAccounts runs, in this process, the server CalloutAccountsConfig describes, shut
// down when the test ends, and returns its client URL once it admits the service's own user.
func StartWithCalloutAccounts(t testing.TB, keys CalloutKeys, accounts CalloutAccounts, static ...StaticUser) (url string) {
	t.Helper()
	server := start(t, CalloutAccountsConfig(keys, accounts, static...), []string{"-js"})
	awaitSeed(t, server.URL(), keys.ServiceSeed)
	return server.URL()
}

// StartWithCalloutServer runs the server StartWithCallout runs and returns it with its client URL,
// for a test that stops, starts or restarts it under connected clients. Its relay and its own
// client port listen on a loopback address of their own, as every server start makes does.
//
// A test of the auth service counts what its hub sends the service, and the hub sends it a
// request for every client that reaches it. In the envoy-go-race step a client another test left
// behind, with no credential, reached a callout test's hub through a port the kernel had given out
// again, and its replica counted three dropped requests where the test had sent one; while the
// server's own client port listened on 127.0.0.1, such a client still reached the hub there, and
// TestAClosingReplicaLogsWhatItCountedSinceItsLastLine/drops counted five drops after its first
// line where its two connects made two (job 135649687816). The address of its own (start) puts the
// hub out of such a client's reach, whichever port it gets.
func StartWithCalloutServer(t testing.TB, keys CalloutKeys, static ...StaticUser) (*Server, string) {
	t.Helper()
	server := start(t, CalloutConfig(keys, static...), []string{"-js"})
	awaitSeed(t, server.URL(), keys.ServiceSeed)
	return server, server.URL()
}

// StartClusterWithCallout runs n members of a cluster (StartCluster), each configured as
// StartWithCalloutServer configures its one server - the auth service's own user, each static user
// with its grant, and the auth callout naming the issuer, $G and the curve key - and routed as
// StartClusterConfig routes its members, with JetStream clustered across them, as the clustered hub
// runs. Every member's relay and client port listen on a loopback address of their own, as every
// server start makes. It returns once one member leads the JetStream meta group, which counts all
// n, and every member admits the service's own user.
//
// No member admits a client with no credential, so the cluster is not awaited as StartCluster's
// is (AwaitReady, whose last step connects with none and asks JetStream for the account's info): a
// member holds such a client for its authorization timeout, waiting for a service that answers
// nothing yet. A test reaches JetStream through a static user of its own, or through a client the
// service it starts admits.
func StartClusterWithCallout(t testing.TB, n int, keys CalloutKeys, static ...StaticUser) *Cluster {
	t.Helper()
	cluster := startCluster(t, n, n, calloutAuthorization(keys, static...))
	cluster.awaitMeta(t)
	for _, server := range cluster.Servers {
		awaitSeed(t, server.URL(), keys.ServiceSeed)
	}
	return cluster
}

// awaitSeed waits, until testDeadline, for the server at uri to admit the nkey user whose seed is
// seed, each connect's dials cut to what is left (waitEnd): the proof it speaks the client
// protocol and enforces its configured users. A server with an auth callout cannot be probed with a
// client that has no credential, as awaitAuth probes one, because the server holds such a client
// for its authorization timeout while it waits for an answer no service gives yet.
func awaitSeed(t testing.TB, uri, seed string) {
	t.Helper()
	bound := boundOf(t)
	for {
		options, retire := bound.dial()
		conn, connectErr := ConnectSeed(t, uri, seed, options...)
		if connectErr == nil {
			retire()
			conn.Close()
			return
		}
		if bound.over() {
			t.Fatalf("NATS at %s did not admit the auth service's user by %s, short of the test's deadline: %v", uri, bound, connectErr)
		}
		bound.pause()
	}
}
