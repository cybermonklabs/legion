package testnats

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/testwait"
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
	return fmt.Sprintf(`jetstream {}
authorization {
  timeout: %d
  users = [
    %s
  ]
  auth_callout {
    issuer: %q
    account: "$G"
    auth_users: [ %s ]%s
  }
}
`, CalloutAuthTimeout, strings.Join(users, "\n    "), keys.IssuerPublic, strings.Join(authUsers, ", "), xkey)
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

// StartWithCalloutServer runs the server StartWithCallout runs and returns it with its client URL,
// for a test that stops, starts or restarts it under connected clients. Its relay listens on a
// loopback address of its own (calloutHost).
func StartWithCalloutServer(t testing.TB, keys CalloutKeys, static ...StaticUser) (*Server, string) {
	t.Helper()
	server := startOn(t, calloutHost(), CalloutConfig(keys, static...), []string{"-js"})
	awaitSeed(t, server.URL(), keys.ServiceSeed)
	return server, server.URL()
}

// calloutHost is the address a callout server's relay listens on: on Linux, where every address
// of 127.0.0.0/8 is the loopback, one chosen at random, and elsewhere 127.0.0.1.
//
// A test of the auth service counts what its hub sends the service, and the hub sends it a
// request for every client that reaches it. A relay holds its port only for its own test's life.
// A client that test leaves behind keeps dialing the port (a bus client reconnects forever), and
// the kernel can give the same port to a later server's relay, in this test binary or in another
// binary go test runs beside it. In the envoy-go-race step a client with no credential reached a
// callout test's hub that way, and its replica counted three dropped requests where the test had
// sent one. Such a client dials the address its own server had, so a relay on an address of its
// own is out of its reach, whichever port it gets.
func calloutHost() string {
	if runtime.GOOS != "linux" {
		return "127.0.0.1"
	}
	return fmt.Sprintf("127.%d.%d.%d", 1+rand.IntN(254), rand.IntN(256), 1+rand.IntN(254))
}

// awaitSeed waits for the server at uri to admit the nkey user whose seed is seed: the proof it
// speaks the client protocol and enforces its configured users. A server with an auth callout
// cannot be probed with a client that has no credential, as awaitAuth probes one, because the
// server holds such a client for its authorization timeout while it waits for an answer no
// service gives yet. A server can answer a dial before the NATS protocol handshake, so a refused
// connect is tried again through the shared wait, whose reading is the last refusal.
func awaitSeed(t testing.TB, uri, seed string) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("NATS at %s to admit the auth service's user", uri), func() (error, bool) {
		conn, err := ConnectSeed(t, uri, seed)
		if err != nil {
			return err, false
		}
		conn.Close()
		return nil, true
	})
}
