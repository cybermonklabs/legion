package bus

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/internal/redact"
)

// AllowRemoteEnvVar names the variable a run sets to "1" to reach a NATS server that is not this
// machine's, or any NATS through a SOCKS5 proxy (SOCKS5ProxyEnvVar). Every connect refuses one
// otherwise, whether or not it owns the stream: a process that reaches another machine's bus
// publishes into an event stream every agent consumes, and one that owns the stream reconfigures a
// resource several deployments read.
//
// Nothing infers the reach, because nothing can tell the deployed Dispatch from the same binary
// run out of a checkout: both read envoy.natsUrls from the user envoy.json
// (${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json), which on an agent's machine names
// production (LEGION-249). Each deployment states its reach instead
// (packages/envoy/deploy/compose/*.compose.yml, the production deployment's listener and Dispatch
// service definitions, and the on-prem fleet's Pulumi), and it must be set there BEFORE a binary that
// reads it runs on that deployment: without it, this one refuses the shared NATS its deployment
// names and exits. Setting it early costs nothing, because a binary built before the variable
// ignores it.
const AllowRemoteEnvVar = "ENVOY_ALLOW_REMOTE_NATS"

// ErrRemoteNATS is what a connect refuses a NATS server this machine does not run with.
var ErrRemoteNATS = errors.New("bus: refusing a NATS server that is not this machine's")

// refuseRemoteNATS reports the first url naming another machine, unless the run opted in through
// lookup (the connect's environment, WithEnvironment). An empty urls is nats.go's own default,
// this machine's server, and passes. A set SOCKS5ProxyEnvVar is remote reach whatever the URLs
// say: the proxy chooses the server a connection reaches, so a loopback URL no longer names this
// machine's.
func refuseRemoteNATS(urls []string, lookup func(string) (string, bool)) error {
	if allow, _ := lookup(AllowRemoteEnvVar); allow == "1" {
		return nil
	}
	if _, set := lookup(SOCKS5ProxyEnvVar); set {
		// Not the value: a proxy can be written with credentials, and this refusal is logged.
		return fmt.Errorf("%w: %s is set, which sends every connection through a proxy that chooses the server (set %s=1 to reach it)", ErrRemoteNATS, SOCKS5ProxyEnvVar, AllowRemoteEnvVar)
	}
	for _, raw := range urls {
		if LocalNATSURL(raw) {
			continue
		}
		return fmt.Errorf("%w: %s (set %s=1 to reach it)", ErrRemoteNATS, natsServerName(raw), AllowRemoteEnvVar)
	}
	return nil
}

// LocalNATSURL reports whether raw names a server on this machine. It reads the URL and never
// resolves it: a lookup would reach the network the refusal is about, and a name that resolves to
// a loopback address on one machine resolves to a shared server on another - the devbox's own
// envoy-nats is exactly such a name. A URL it cannot read names no server it can vouch for.
func LocalNATSURL(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if !strings.Contains(trimmed, "://") {
		trimmed = "nats://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return false
	}
	// A host name is case-insensitive and its root dot is not part of the name.
	host := strings.TrimSuffix(parsed.Hostname(), ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// natsServerName names raw, a configured NATS server URL, for a log line or an error that is
// logged: by its scheme and host (url.URL.Host) alone, as nats://host:port, and as
// redact.NotPrinted when it cannot. Nothing else of the URL prints. nats.go authenticates with the
// URL's userinfo (connectProto, nats.go v1.55.0 nats.go:3133-3143): a user name alone is a token
// and user:password a password, so both parts are the credential, and url.URL.Redacted, which
// keeps the user name, would log a token whole. Its path, query and fragment name no server, and
// they are the configured text as written, which can carry a credential too. raw is read as
// LocalNATSURL reads it, so a URL with no scheme is named as nats://. A URL that does not parse,
// names no host, or holds an "@" past its authority is named as redact.NotPrinted: a "/", "?" or
// "#" written into a userinfo ends the authority there, so the host url.Parse reads is a piece of
// the credential, and nothing tells that URL from one whose path or query holds an "@". An empty
// raw is "".
func natsServerName(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	withScheme := trimmed
	if !strings.Contains(withScheme, "://") {
		withScheme = "nats://" + withScheme
	}
	parsed, err := url.Parse(withScheme)
	if err != nil || parsed.Host == "" || strings.Contains(parsed.Path, "@") || strings.Contains(parsed.RawQuery, "@") || strings.Contains(parsed.Fragment, "@") {
		return redact.NotPrinted
	}
	return parsed.Scheme + "://" + parsed.Host
}
