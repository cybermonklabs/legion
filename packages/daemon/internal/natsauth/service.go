package natsauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/natsconn"
	"github.com/sjawhar/envoy/natscred"
)

// ServiceTokenFileKey is the `legion.yaml` key naming the file of the daemon's service token: the
// projected Kubernetes ServiceAccount token its own connection presents, as user
// natscred.ServiceUser, to a hub whose connections the NATS auth service admits, which answers the
// token's account with the grant the broker's BROKER_NATS_SERVICES lists it for under a daemon/-/*
// entry. ServiceTokenFileVariable names the file otherwise, and the key outranks it, as the seed's
// key outranks its variables. The variable is not NATS_SERVICE_TOKEN_FILE, the Envoy Go bus
// clients' own: an Envoy client that inherited the daemon's environment would connect as the
// daemon's account. No variable holds the token itself, since a token that rotates is read again
// from its file, and no pane is ever handed either name.
const (
	ServiceTokenFileKey      = "nats_daemon_service_token_file"
	ServiceTokenFileVariable = "NATS_DAEMON_SERVICE_TOKEN_FILE"
)

// Source is where the daemon's own NATS credential came from, as its boot line names it.
type Source string

const (
	SourceServiceToken Source = "service-token" // ServiceTokenFileKey or ServiceTokenFileVariable
	SourceDaemonSeed   Source = "daemon"        // DaemonSeed
	SourcePaneSeed     Source = "pane"          // the pane seed every pane receives
	SourceNone         Source = "none"          // no credential
)

// Credential is the daemon's own NATS credential: the service token, one seed, or none, never two,
// so no connection made from it presents a seed beside the token. Its fields are unexported, and
// only DaemonCredential makes one with a credential in it; a zero Credential names no source,
// presents nothing, and waits out only what Unreachable does.
type Credential struct {
	source Source
	// seed is the seed presented, the daemon seed's or the pane seed's; "" for the token or none.
	seed string
	// file, setting and account are the service token's: its file, the setting that named it
	// (ServiceTokenFileKey or ServiceTokenFileVariable) and the account its token names, each ""
	// unless source is SourceServiceToken.
	file, setting, account string
}

// Source is where the credential came from.
func (c Credential) Source() Source { return c.source }

// Seed is the nkey seed the credential presents (Connect's seed), the daemon seed or the pane
// seed; "" for the service token or no credential.
func (c Credential) Seed() string { return c.seed }

// ServiceToken is the service token's file and the setting that named it (ConnectService's
// tokenFile and setting) and the account its token named when DaemonCredential read it (what the
// daemon says it connects as), each "" unless Source is SourceServiceToken. It carries no token:
// each connect reads the file afresh.
func (c Credential) ServiceToken() (file, setting, account string) {
	return c.file, c.setting, c.account
}

// WaitsOut is the decision the start's NATS wait takes on err, a failure of its first connect with
// this credential: whether to try again. For every credential it waits out what Unreachable does,
// a NATS not reachable yet. For the service token it also waits out a refusal (Refused): the NATS
// auth service refuses a token while it cannot read the key set of the token's issuer, and the
// server answers that refusal as it answers every other, so the client cannot tell it from an
// account the broker does not list, and waits out both. A refusal of a seed, or of no credential,
// is a misconfiguration no wait fixes, and stops the start as it did before the token.
func (c Credential) WaitsOut(err error) bool {
	return Unreachable(err) || (c.source == SourceServiceToken && Refused(err))
}

// RefuseServiceTokenBesideDaemonSeed refuses the service token set beside any setting of the
// daemon seed, on presence alone, reading no file. The token's setting is tokenFile, the path
// ServiceTokenFileKey names ("" when legion.yaml names none), else ServiceTokenFileVariable when
// lookup finds it set. The seed's is seedFile, the path DaemonSeedFileKey names, else
// DaemonSeedFileVariable, else DaemonSeedVariable, by DaemonSeed's own precedence (resolve) with no
// reader, as Configured decides on presence. A variable set but empty is set. The daemon's own
// connection presents one credential, and taking one of two would be a guess at which the operator
// meant, so the start is refused naming both before either is read: whatever the seed's file or
// value holds, the refusal is the same. It names the setting that wins on each side (the token's
// key over its variable; the seed's key, then its file variable, then its value variable), in the
// Envoy bus's words for its own two credentials, and carries no path and no value, either of which
// can be the credential pasted where its setting goes. The pane seed's settings are never refused:
// the daemon hands the pane seed to the agents it launches and does not connect with it beside the
// token.
func RefuseServiceTokenBesideDaemonSeed(seedFile, tokenFile string, lookup func(string) (string, bool)) error {
	_, token, tokenSet := serviceTokenSetting(tokenFile, lookup)
	seed, seedSet := resolve(daemonSeed, seedFile, lookup, nil)
	if !tokenSet || !seedSet {
		return nil
	}
	return fmt.Errorf("%s and %s are both set; the daemon's own NATS connection presents one credential, so set one", token, seed.setting)
}

// serviceTokenSetting is the path the service token's setting names, and that setting: tokenFile
// under ServiceTokenFileKey when legion.yaml names one, else ServiceTokenFileVariable when set,
// whatever it holds; set is false when neither is.
func serviceTokenSetting(tokenFile string, lookup func(string) (string, bool)) (path, setting string, set bool) {
	if tokenFile != "" {
		return tokenFile, ServiceTokenFileKey, true
	}
	if path, set := lookup(ServiceTokenFileVariable); set {
		return path, ServiceTokenFileVariable, true
	}
	return "", "", false
}

// DaemonCredential is the daemon's own NATS credential, the one each of its connections presents:
// the service token, else the daemon seed, else the pane seed, else none, never two.
//
// It runs RefuseServiceTokenBesideDaemonSeed first, over seedFile, tokenFile and lookup as that
// reads them, so a token beside a daemon seed setting is refused naming both before any file is
// read. With the token set (the key's path over the variable's), the variable set but empty is
// refused naming it, never read as no setting; an entry of urls that carries userinfo is refused
// by its position alone (userinfoURL), since nats.go presents that userinfo in place of the token;
// and the file is read once, here, for the account its token names (ServiceIdentity, with its
// refusals), so a file the daemon cannot use refuses its start rather than each connect. The answer
// is then the token with no seed: not the daemon seed, and not paneSeed in its place, though the
// pane seed's settings stay allowed beside the token, since the daemon hands that seed to the
// agents it launches and does not connect with it. With no token set the answer is the order the
// daemon chose before the token: DaemonSeed(seedFile, lookup), with its refusals, else paneSeed,
// the pane seed the daemon's launch secrets already read ("" for none), else no credential.
func DaemonCredential(seedFile, tokenFile string, urls []string, paneSeed string, lookup func(string) (string, bool)) (Credential, error) {
	if err := RefuseServiceTokenBesideDaemonSeed(seedFile, tokenFile, lookup); err != nil {
		return Credential{}, err
	}
	if path, setting, set := serviceTokenSetting(tokenFile, lookup); set {
		if path == "" {
			return Credential{}, fmt.Errorf("%s is set but empty", setting)
		}
		if err := userinfoURL(urls, setting); err != nil {
			return Credential{}, err
		}
		account, err := ServiceIdentity(path, setting)
		if err != nil {
			return Credential{}, err
		}
		return Credential{source: SourceServiceToken, file: path, setting: setting, account: account}, nil
	}
	seed, err := DaemonSeed(seedFile, lookup)
	switch {
	case err != nil:
		return Credential{}, err
	case seed != "":
		return Credential{source: SourceDaemonSeed, seed: seed}, nil
	case paneSeed != "":
		return Credential{source: SourcePaneSeed, seed: paneSeed}, nil
	default:
		return Credential{source: SourceNone}, nil
	}
}

// ServiceIdentity is the account the service token in the file at path names, path being what
// setting (ServiceTokenFileKey or ServiceTokenFileVariable) named: the token read as the Envoy bus
// reads it (natscred.ServiceFile), and its account read unverified as natscred.Identity reads it,
// a Google token's email, else its subject (system:serviceaccount:<namespace>:<name> for a
// projected Kubernetes token). It is what the daemon says it connects as; the auth service
// verifies the token and decides for itself. A file that cannot be read is refused by setting and
// the system's reason alone, never its path, which is where a person who pasted the token itself
// puts it; an empty file by its path too; a file whose token names no account by setting and path.
// No mode is held against the file: a projected token is mounted 0644 unless its volume says
// otherwise, and the bus checks none either. No error carries the token.
func ServiceIdentity(path, setting string) (string, error) {
	token, err := natscred.ServiceFile(path, setting).Read()
	if err != nil {
		return "", err
	}
	account, err := natscred.Identity(token)
	if err != nil {
		return "", fmt.Errorf("%s names %s, whose token names no account: %w", setting, path, err)
	}
	return account, nil
}

// userinfoURL refuses the first of urls that carries userinfo (a user name, with or without a
// password) beside the service token setting names, each read as nats.go reads it: trimmed, an
// empty one dropped, nats:// added where it names no scheme. nats.go presents a server URL's own
// userinfo on the CONNECT and then never asks the connection's user handler (connectProto), so a
// connect to that server would present that user, not natscred.ServiceUser with the token. It names
// the URL by its position in urls alone, as unreadableURL does: a URL's userinfo is a credential,
// and its host can be a part of one. A URL that does not parse is unreadableURL's to refuse, and
// the configuration's (config.readNatsURLs), not this rule's.
func userinfoURL(urls []string, setting string) error {
	for index, raw := range urls {
		server := strings.TrimSpace(raw)
		if server == "" {
			continue
		}
		if !strings.Contains(server, "://") {
			server = "nats://" + server
		}
		if parsed, err := url.Parse(server); err == nil && parsed.User != nil {
			return fmt.Errorf("the NATS server URL %d of %d carries a user name, which the NATS client presents in place of user %s and the service token %s names: write the URL with no user name or password",
				index+1, len(urls), natscred.ServiceUser, setting)
		}
	}
	return nil
}

// ConnectService dials urls with the daemon's service token: user natscred.ServiceUser and, as the
// password, the token in the file at tokenFile, which setting named, read afresh at each CONNECT
// (natscred.Service), so a reconnect presents the token the kubelet wrote last. The hub sends the
// CONNECT to the NATS auth service, which verifies the token and answers its account with the
// grant the broker's BROKER_NATS_SERVICES lists it for (natsgrant.Daemon() under a daemon/-/*
// entry). A server URL that cannot be read as written (unreadableURL), or that carries userinfo
// (userinfoURL), is refused by its position in urls before any dial. It pings and names itself as
// Connect does, and a connect that fails prints as Connect's does, as its class alone
// (natsconn.ConnectionError), errors.Is and errors.As still reading the error beneath it. Its
// replies come on nats.go's own inbox prefix, _INBOX, the replies natsgrant.Daemon() lets the
// daemon subscribe.
//
// Three things a seed's connection does not do:
//   - It retries a refusal: natscred.Service sets nats.go's IgnoreAuthErrorAbort, since the auth
//     service refuses a token while it cannot read the key set of the token's issuer, so a refused
//     reconnect is tried again rather than closing the connection after the same refusal twice.
//   - It reports those refusals at most once a minute (reportRefusals, on the wall clock): nats.go
//     hands each refused reconnect to the AsyncErrorCB a caller's option set (LogEvents'), about
//     every two seconds of an outage.
//   - It moves to a later token natscred.ServiceRenewBefore before the one it presented lapses
//     (natscred's Keeper, started once the connection is made and ended by its close, the daemon's
//     own Close included), since the user the auth service answers expires with its token.
//
// The credential, the refusal report, the pings and the name are appended after the caller's own
// options, so they hold whatever a caller passed before them.
func ConnectService(urls []string, tokenFile, setting, name string, options ...nats.Option) (*nats.Conn, error) {
	if err := unreadableURL(urls); err != nil {
		return nil, err
	}
	if err := userinfoURL(urls, setting); err != nil {
		return nil, err
	}
	credential, keeper := natscred.Service(natscred.ServiceFile(tokenFile, setting))
	options = append(options, credential, reportRefusals(time.Now), detectSilentCuts, nats.Name(name))
	conn, err := nats.Connect(strings.Join(urls, ","), options...)
	if err != nil {
		return nil, natsconn.ConnectionError(err)
	}
	// Keep returns once the connection closes, so this goroutine ends with it.
	go keeper.Keep(context.Background(), conn)
	return conn, nil
}

// Refused reports whether err is the server refusing the connection's credential: nats.go's
// authorization violation, which a hub whose connections the NATS auth service admits answers for
// each refusal of that service, whatever its reason (the service tells the server why; the server
// tells the client only that it was refused). For a seed, or no credential, that is a
// misconfiguration Unreachable refuses; for the daemon's service token it is waited out, at the
// start (Credential.WaitsOut) and after the connect (ConnectService).
func Refused(err error) bool {
	return errors.Is(err, nats.ErrAuthorization)
}

// refusalReportEvery is how often, within one outage, reportRefusals lets a refusal of the
// connection's credential through. nats.go tries a refused reconnect again about every two seconds
// (ReconnectWait), and forever under ReconnectForever, so an hour of an auth service that cannot
// verify the token would be thousands of identical lines; one a minute still names the outage in
// every log window that matters, as the Envoy bus's failed reconnects do (reconnectErrEvery).
const refusalReportEvery = time.Minute

// reportRefusals is ConnectService's refusal report: the connection option that wraps the
// AsyncErrorCB and ReconnectedCB the options before it set (LogEvents'). A refusal of the
// connection's credential (Refused), which nats.go reports with no subscription at each refused
// reconnect, passes on the first of an outage and then once refusalReportEvery of now has gone by
// since the last that passed, a report exactly that long after it included; a reconnect ends the
// outage, so the next one's first refusal passes at once. Every other report passes, and leaves the
// outage's minute where it was: one that names a subscription (a permission violation), and any
// error that is no refusal. now is a parameter, never a package variable, so a test drives it on a
// clock of its own while others run beside it. With no AsyncErrorCB set before it, it sets none:
// nats.go installs its default handler then, and there is no handler of a caller's to hold back.
func reportRefusals(now func() time.Time) nats.Option {
	return func(o *nats.Options) error {
		report, restored := o.AsyncErrorCB, o.ReconnectedCB
		if report == nil {
			return nil
		}
		outage := &refusalOutage{now: now}
		o.AsyncErrorCB = func(c *nats.Conn, sub *nats.Subscription, err error) {
			if sub == nil && Refused(err) && !outage.pass() {
				return
			}
			report(c, sub, err)
		}
		o.ReconnectedCB = func(c *nats.Conn) {
			outage.end()
			if restored != nil {
				restored(c)
			}
		}
		return nil
	}
}

// refusalOutage is reportRefusals' state across a connection's callbacks, which nats.go runs on a
// goroutine of its own and a test may run from several: whether a refusal of the current outage
// has passed, and when the last one did.
type refusalOutage struct {
	now func() time.Time

	mu       sync.Mutex
	reported bool
	last     time.Time
}

// pass reports whether a refusal reported now passes: the outage's first does, and after it one
// once refusalReportEvery has gone by since the last that passed.
func (r *refusalOutage) pass() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at := r.now()
	if r.reported && at.Sub(r.last) < refusalReportEvery {
		return false
	}
	r.reported, r.last = true, at
	return true
}

// end ends the outage, so the next refusal is the first of another.
func (r *refusalOutage) end() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reported = false
}
