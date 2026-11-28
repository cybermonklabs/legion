package bus

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/sjawhar/envoy/natsurl"
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

// refuseRemoteNATS reports the first of urls, the list as it was configured, that names another
// machine, unless the run opted in through lookup (the connect's environment, WithEnvironment). An
// empty urls is nats.go's own default, this machine's server, and passes, and so does a blank
// entry, which the connect drops (natsurl.Check). A set SOCKS5ProxyEnvVar is remote reach whatever
// the URLs say: the proxy chooses the server a connection reaches, so a loopback URL no longer
// names this machine's.
//
// The refusal names the URL by its position in urls alone (`NATS server URL 2 of 3`), never by any
// of its text, and so does every line the bus logs about a server (serverOf, natsconn.ConnectionClass). A
// caller splits its list on commas, so a token or password holding ",nats://" (or tls, ws or wss)
// leaves its head a URL of its own, whose host is a piece of the credential: natsurl's tail rule
// lets such a list through, since a later URL that carries a user name and names a NATS scheme is
// one a person writes, and nothing that reads the head can tell it from a host.
func refuseRemoteNATS(urls []string, lookup func(string) (string, bool)) error {
	if allow, _ := lookup(AllowRemoteEnvVar); allow == "1" {
		return nil
	}
	if _, set := lookup(SOCKS5ProxyEnvVar); set {
		// Not the value: a proxy can be written with credentials, and this refusal is logged.
		return fmt.Errorf("%w: %s is set, which sends every connection through a proxy that chooses the server (set %s=1 to reach it)", ErrRemoteNATS, SOCKS5ProxyEnvVar, AllowRemoteEnvVar)
	}
	for index, raw := range urls {
		if strings.TrimSpace(raw) == "" || LocalNATSURL(raw) {
			continue
		}
		return fmt.Errorf("%w: NATS server URL %d of %d names another machine (set %s=1 to reach it)", ErrRemoteNATS, index+1, len(urls), AllowRemoteEnvVar)
	}
	return nil
}

// checkedNATSURLs is urls trimmed, the list Dial and newClient dial, or a refusal naming by its
// position the first URL nats.go would read other than as written (natsurl.Check), before any
// dial: refuseRemoteNATS reads no URL of a run that may reach remote NATS, so this reads them for
// every run. nats.go would refuse such a URL with url.Parse's error, which quotes it whole, after
// the connect's ten attempts, or dial a credential's head as a host name and log it.
func checkedNATSURLs(urls []string) ([]string, error) {
	trimmed, position, why := natsurl.Check(urls)
	if why != "" {
		return nil, fmt.Errorf("bus: NATS server URL %d of %d cannot be read: %s", position, len(urls), why)
	}
	return trimmed, nil
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
