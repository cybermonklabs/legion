package core

import (
	"fmt"
	"strings"

	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/natsgrant"
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

// calloutPingLines are the top-level options a callout adds after the authorization block: the
// hub's backstop that drops a replica of the auth service that hangs from the callout's queue
// group, where the server's defaults (ping_interval 2 minutes, ping_max 2: server/const.go:120-123)
// keep it there 4 to 8 minutes while each client sent to it is refused. It is the only thing that
// acts when a replica's node loses its network; a liveness probe that restarts a replica whose
// process stops answering, in seconds, is dispatch://INFRA-840's to provide.
//
// nats-server v2.15.1 (server/client.go processPingTimer, :5943-6001) fires each connection's ping
// timer every ping_interval. It skips a client's PING when it read messages, subscriptions or a
// client PING from it within the interval (:5970-5978, :1684-1688, :2814-2818; a PONG does not
// count), and closes the connection as a "Stale Connection" when one more PING would exceed
// ping_max outstanding (:5983-5993). So a replica that hangs is closed 40 to 80 s later: 2 to 3
// intervals if it had sent nothing in the interval before, 3 to 4 if it sent data up to the hang;
// its queue subscription goes with it (nats_ping_live_test.go).
//
// A client whose event loop blocks, or whose link stalls, is kept for any stall shorter than 40 s,
// where 3 s would close it after 6 to 12 s; envoy-client's own 10 s Bun.spawnSync block is
// survived. The Legion daemon (its own natsauth.detectSilentCuts, on every connection its Connect
// opens), and Dispatch's server and the session listener's stream connection (envoy's
// bus.WithSilentCutDetection), ping the server every 1 s, and the TS plugins every 10 s, so the
// server does not ping those connections while they are up. The listener's role-lane and
// session-consumer connections, and other Go clients, keep nats.go's 2-minute ping, so the server
// pings each of them when it is idle for a whole interval: every 20 s instead of every 2 minutes.
// Apart from that, the server pings every client once an hour to measure its round trip (:5966,
// const.go:224). The first PING to a new client stays at 2 s (client.go:7128-7142), so the
// callout's 1.5 s answer budget is unchanged.
//
// It does not catch a replica whose request handler deadlocks while its process runs: nats.go
// answers a PING from its read loop (nats.go v1.55.0 processPing at nats.go:4278, called at parser.go:324), not
// from a handler, so the replica keeps its place in the group. Only the service can leave the
// group then, by unsubscribing or closing its connection; that is INFRA-840's to provide.
//
// ping_interval also governs a server's routes unless cluster.ping_interval is set, so the
// clustered hub pins the routes' own (gcp clusteredHubCluster). The lines are part of
// CalloutLines, whose change rolls the clustered hub's pods.
const calloutPingLines = "# a replica of the auth service that answers nothing is dropped from the callout's queue group within 80 s\n" +
	"ping_interval: \"20s\"\n" +
	"ping_max: 2\n"

// privateProjectsTagLines are the lines a callout adds after the ping lines beside
// PrivateProjects: the server tag natsgrant.PrivateProjectsServerTag. nats-server v2.15.1 reads
// server_tags (server/opts.go:1803) and copies the server's tags into each authorization request
// it sends the auth service (claim.Server.Tags = s.getOpts().Tags, server/auth_callout.go:377). The
// request's server claim also carries the server's name, host, id, version, cluster and xkey
// (auth_callout.go:367-373, :383), and none of those says how the hub was rendered, so the tag is
// how a request proves that its hub was rendered with PrivateProjects: there, and only there, the
// auth service admits Dispatch's hub-admission proof and holds every listener to required privacy
// mode (natsgrant.HubAdmissionAudience, natsgrant.PrivacyRequiredSuffix). A hub rendered without
// PrivateProjects carries no tag, so its requests prove nothing.
//
// The lines are part of CalloutLines, so the clustered hub rolls its pods when the tag comes or
// goes (CalloutLines says why). nats-server also reloads server_tags (server/reload.go:360-366,
// :1698-1699), so a change of the tag alone reaches a running server whose file is reloaded, as
// the clustered hub's reloader does, before any restart. Each reload, whatever it changes, decides
// every callout connection again: it always reloads the users (reload.go:1651-1654, :1706-1707,
// :2091-2093). A client the auth service named carries that name and no nkey
// (auth_callout.go:326-333), so it names no configured user, and the reload closes it; it
// reconnects through the callout (reload.go:2291-2299, :2349-2352, :2398-2432). Envoy's auth
// service names each client it admits. An unnamed client that presented no user name or nkey goes
// through the callout again on its live connection (reload.go:2354-2358).
const privateProjectsTagLines = "# this hub is rendered with privateProjects: the auth service admits Dispatch's hub-admission proof only on a request that carries this tag, and there admits a listener only in required privacy mode\n" +
	"server_tags: [\"" + natsgrant.PrivateProjectsServerTag + "\"]\n"

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

// CalloutLines is every line Render's text holds because of the callout, "" without one, for an
// authorization Validate accepts: the auth_callout block inside the authorization block, then the
// ping lines and, beside PrivateProjects, the server tag's lines that end the text (calloutParts,
// which Render writes from too). nats-server v2.15.1 refuses a whole reload that changes its
// callout ("config reload not supported for AuthCallout": server/reload.go diffOptions has no case
// for it), so a server takes a changed block only when it restarts. The ping lines come and go
// only with that block, so the one reload of them the server would take is an edit of
// calloutPingLines itself (reload.go:1751-1754), which each connection applies when its timer next
// re-arms, within one interval of 20 s (reload.go:670-672 only logs; client.go:6037-6050). A change
// of the server tag alone does reload, and each reload decides every callout connection again
// (privateProjectsTagLines). A component that reloads its servers rolls them when this text
// changes, so every server takes a change at one known moment, its restart, after the component
// has written the new text (the clustered hub's ConfigMap), with each connection's timer started
// fresh; a reload of the tag comes whenever the file on a server changes (the kubelet's update of
// a mounted ConfigMap), which nothing orders. The clustered hub's pin of its routes' ping (gcp
// clusteredHubCluster) appears and goes with the callout, so it always arrives with such a roll.
// auth_users is in it, so a keyed user added or removed changes it.
func (a Authorization) CalloutLines() string {
	block, tail := a.calloutParts()
	return block + tail
}

// calloutParts are the callout's lines of Render's text, both "" without a callout: block, the
// auth_callout block inside the authorization block, after its users, and tail, the ping lines
// and, beside PrivateProjects, the server tag's lines, which end the text. Render and CalloutLines
// both write them from here, so a line one writes the other holds. Validate refuses PrivateProjects
// without a callout, so no other text carries the tag.
func (a Authorization) calloutParts() (block, tail string) {
	if a.Callout == nil {
		return "", ""
	}
	tail = calloutPingLines
	if a.PrivateProjects {
		tail += privateProjectsTagLines
	}
	return a.Callout.block(a.Users), tail
}

// block is the callout's lines of the authorization block, which follow its users: auth_callout
// naming the issuer, the key of every keyed user of users in their order, and the xkey. It writes no
// authorization timeout (see the budget above). auth_users is derived from users and never given:
// in config mode nats-server sends a keyed user it does not list to the callout, even one whose
// nkey signature was valid (v2.15.1 server/auth.go:739-761), so a static user left out of it would
// be sent to the auth service, which knows no such client.
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
