package core

import (
	"fmt"
	"strings"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/bus"
)

// AuthCallout is a NATS server's authorization callout: every client that is not one of the
// authorization's keyed users is admitted, or refused, by the auth service (dispatch://INFRA-840),
// which reads the client's token and answers with that connection's grant. The server runs in
// config mode, with no accounts block, so the service places each client in the global account,
// $G, beside the static users.
type AuthCallout struct {
	// Issuer is the public key of the account (A and 55 base32 characters) whose seed signs the
	// auth service's answers. The server admits only a user whose JWT that key issued (v2.15.1
	// server/auth_callout.go:166-193); for a sealed answer it does not check who signed the answer
	// itself (:135-152), so the issuer of the user is the check that holds.
	Issuer string `json:"issuer"`
	// XKey is a curve public key (X and 55 base32 characters). The server seals each request to the
	// service with it, so only the service reads a client's token; without it the request, the
	// token in it, crosses $SYS.REQ.USER.AUTH unsealed (v2.15.1 server/auth_callout.go:440-449).
	XKey string `json:"xkey"`
	// ServiceUser is the Name of the keyed user the auth service connects as.
	ServiceUser string `json:"serviceUser"`
}

// The callout block sets no authorization timeout, so nats-server's default of 2 s stands
// (server/const.go AUTH_TIMEOUT): the auth service must answer a client within 1.5 s. A longer
// timeout would promise the service time a Go client does not wait. nats-server sends a client its
// first PING 2 s after it connects, plus up to 20% (server/client.go setFirstPingTimer), also while
// the client's CONNECT still waits for the callout, and nats.go's handshake reads anything but PONG
// as an error ("nats: expected 'PONG', got 'PING'"), so a Go client fails at about 2 s whatever
// timeout is set (nats_callout_timeout_live_test.go). nats.js 2.29.3 waits out that PING and was
// admitted on an answer at 1.0 s, 1.8 s and, under a 5 s timeout, 3.5 s. No nats-server 2.15.1
// setting delays the first PING past 2 s: a ping_interval below 2 s moves it earlier, and
// DisableShortFirstPing is `json:"-"` in server/opts.go (server/client.go:7122-7139).

// The auth service's grant as NATS defines it: the service subscribes to the authorization
// requests (server/auth_callout.go AuthCalloutSubject) and publishes only its answers, to the
// server's reply inbox under $SYS._INBOX. (server/events.go InboxPrefix).
const (
	calloutRequestSubject = "$SYS.REQ.USER.AUTH"
	calloutReplyPrefix    = "$SYS._INBOX."
)

func (c AuthCallout) seedField() string {
	for _, f := range []struct{ field, value string }{
		{"callout.issuer", c.Issuer}, {"callout.xkey", c.XKey}, {"callout.serviceUser", c.ServiceUser},
	} {
		if bus.HoldsSeed(f.value) {
			return f.field
		}
	}
	return ""
}

// validate refuses a callout the server would read otherwise than it says, or that would send a
// client's token unsealed, naming the field: an issuer that is not an account public key, no xkey
// or one that is not a curve public key, a service user that names no keyed user of a, and a
// service user that is a's noAuthUser, as whom a client presenting no credential would answer
// every connection's authorization. Neither key is quoted: seedField has refused a seed in either,
// and a public key is checked by its shape alone. Each refusal is a core.InputError naming
// callout, so a component whose input carries the callout under another name can name that input.
func (c AuthCallout) validate(a Authorization) error {
	if !isPublicKey(c.Issuer, nkeys.PrefixByteAccount) {
		return InputErrorf("callout", "callout.issuer is not an account public key (A and 55 base32 characters with a valid checksum); give the public key of the account whose seed signs the auth service's answers, never the seed")
	}
	if c.XKey == "" {
		return InputErrorf("callout", "callout.xkey is required: without it nats-server sends each connecting client's token to the auth service unsealed, on %s", calloutRequestSubject)
	}
	if !isPublicKey(c.XKey, nkeys.PrefixByteCurve) {
		return InputErrorf("callout", "callout.xkey is not a curve public key (X and 55 base32 characters with a valid checksum); give the public key, never the seed")
	}
	if u, ok := a.user(c.ServiceUser); !ok || u.NKey == "" {
		return InputErrorf("callout", "callout.serviceUser %q names no keyed user: the auth service connects as one of users, with an nkey", c.ServiceUser)
	}
	if c.ServiceUser == a.NoAuthUser {
		return InputErrorf("callout", "callout.serviceUser %q is noAuthUser: a client presenting no credential would become the auth service, and answer every connection's authorization", c.ServiceUser)
	}
	return nil
}

// validateCallout refuses the callout (AuthCallout.validate) and, with PrivateProjects, every
// authorization that could admit a client past the auth service (R12 of dispatch://INFRA-3862): no
// callout; a noAuthUser, whom a client presenting no credential becomes; a service user granted
// more than the auth service's grant (serviceGrantRefusal), which nats-server admits by its seed
// alone and so would be a client past the service; and any user but the service's, each a static
// client user whose seed a client stores or could store, a pending one included. Each refusal
// names the field, and a user by its label (its index, or its input) and name; each of
// PrivateProjects' is a core.InputError naming privateProjects.
func (a Authorization) validateCallout() error {
	if a.Callout != nil {
		if err := a.Callout.validate(a); err != nil {
			return err
		}
	}
	if !a.PrivateProjects {
		return nil
	}
	if a.Callout == nil {
		return InputErrorf("privateProjects", "privateProjects: a project can be private only while the server admits each client through the auth service, and the authorization has no callout")
	}
	for i, u := range a.Users {
		if a.NoAuthUser != "" && u.Name == a.NoAuthUser {
			return InputErrorf("privateProjects", "privateProjects: %s %q is noAuthUser: a client presenting no credential would become it while a project is private", u.label(i), u.Name)
		}
	}
	service, _ := a.user(a.Callout.ServiceUser) // AuthCallout.validate has checked it names a keyed user
	if field, entry, wide := serviceGrantRefusal(service); wide {
		return InputErrorf("privateProjects", "privateProjects: callout.serviceUser %q: %s %q is wider than the auth service's grant, which subscribes %s and publishes only its replies under %s; with a wider grant the service user is a client that connects past the auth service",
			service.Name, field, entry, calloutRequestSubject, calloutReplyPrefix)
	}
	for i, u := range a.Users {
		if u.Name == a.Callout.ServiceUser {
			continue
		}
		what := "a stored client seed"
		if !u.HasSeed {
			what = "a static client user"
		}
		return InputErrorf("privateProjects", "privateProjects: %s %q: %s while a project is private; every client connects through the auth service, so callout.serviceUser %q is the only user", u.label(i), u.Name, what, a.Callout.ServiceUser)
	}
	return nil
}

// serviceGrantRefusal names the first allow entry of the service user's rules that reaches past
// the auth service's grant: a publish entry not under $SYS._INBOX., or a subscribe entry, its queue
// group aside, other than $SYS.REQ.USER.AUTH. A deny only narrows a grant, so none is read. The
// entries are subject patterns (User.validate), whose tokens hold no '.', so an entry beginning
// $SYS._INBOX. matches only subjects under it.
func serviceGrantRefusal(u User) (field, entry string, wide bool) {
	for i, e := range u.Publish.Allow {
		if !strings.HasPrefix(e, calloutReplyPrefix) || len(e) == len(calloutReplyPrefix) {
			return fmt.Sprintf("publish.allow[%d]", i), e, true
		}
	}
	for i, e := range u.Subscribe.Allow {
		if subject, _, _ := strings.Cut(e, " "); subject != calloutRequestSubject {
			return fmt.Sprintf("subscribe.allow[%d]", i), e, true
		}
	}
	return "", "", false
}

// CalloutBlock is the callout's lines of Render's text, "" without a callout, for an authorization
// Validate accepts; Render writes the block from it, so a hash of it is a hash of what the server
// reads. nats-server v2.15.1 refuses a whole reload that changes its callout ("config reload not
// supported for AuthCallout": server/reload.go diffOptions has no case for it), so a server takes a
// changed block only when it restarts; a component that reloads its servers rolls them when this
// text changes. auth_users is in it, so a keyed user added or removed changes it.
func (a Authorization) CalloutBlock() string {
	if a.Callout == nil {
		return ""
	}
	return a.Callout.block(a.Users)
}

// block is the callout's lines of the authorization block, which follow its users: auth_callout
// naming the issuer, the key of every keyed user of users in their order, and the xkey. It writes
// no authorization timeout (see the budget above). auth_users is derived from users and never
// given: in config mode nats-server sends a keyed user it does not list to the callout, even one
// whose nkey signature was valid (v2.15.1 server/auth.go:739-761), so a static user left out of it
// would be sent to the auth service, which knows no such client.
func (c AuthCallout) block(users []User) string {
	keys := []string{}
	for _, u := range users {
		if u.NKey != "" {
			keys = append(keys, u.NKey)
		}
	}
	ascii := PyJSONOptions{EnsureASCII: true}
	return "  auth_callout {\n" +
		"    issuer: " + PyJSON(c.Issuer, ascii) + "\n" +
		"    auth_users: " + PyJSON(keys, ascii) + "\n" +
		"    xkey: " + PyJSON(c.XKey, ascii) + "\n" +
		"  }\n"
}
