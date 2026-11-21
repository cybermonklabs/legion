package core

import (
	"net"
	"strconv"
)

// ForwardedToBind names a TCP forwarder a component's own render writes, or would write, to its
// listener's bind, as the render writes it, such as Tailscale Serve's `TCPForward 127.0.0.1:9020`.
// The zero value is no forwarder. A render that writes one passes it to CheckBindCredential, so no
// forwarder reopens /v1 to callers off the host.
type ForwardedToBind string

// bearerKinds are the credentials that close a loopback listener's /v1, as a refusal names them,
// and bindCredentials what a listener off loopback starts with: a bearer kind, which a component
// renders beside ListenerConfig, or the broker settings it carries.
const (
	bearerKinds     = "a bearer kind (ENVOY_OIDC_ISSUER and ENVOY_OIDC_AUDIENCE, or ENVOY_OIDC_GOOGLE_AUDIENCE)"
	bindCredentials = bearerKinds + " or its broker settings (ENVOY_AGENT_SECRETS_URL, ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE or ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE, and ENVOY_PUBLIC_URL)"
)

// CheckBindCredential refuses a listener that would serve /v1 to callers off its host with no
// credential it verifies. listener is the component's own path for it (such as "listener"), c the
// environment the component is about to render, bearer whether the component renders a bearer kind
// beside c (the projected-token verifier or Google identities, which ListenerConfig does not carry),
// and forward the TCP forwarder the component renders to c's bind, "" for none.
//
// The listener judges its bind alone (ENVOY_LISTEN_HOST, 127.0.0.1 when unset;
// cmd/listener/apiauth.go, requiresCredential and validateListenerAPIAuth): off loopback it starts
// only with a bearer kind or its broker settings and answers a request that presents nothing 401;
// on loopback /v1 is closed only by a bearer kind, and with the broker settings alone it still
// serves a request that presents nothing, fenced by nothing but the request's own Host header. A
// forward to the bind puts the listener off its host in effect, and the listener cannot see it, so
// with one a bearer kind is required, and a listener without one, its broker settings alone
// included, is refused naming the forward: it would serve /v1 open to whoever the forward admits.
// Without a forward, a loopback bind needs nothing, as dev stacks and compose run it, and an
// off-loopback bind with no credential is refused naming the bind, as the listener refuses it at
// boot; there the broker settings count when AgentSecretsURL is set, which Check holds to a whole
// set (CheckBroker). Each refusal is an InputError naming listener.
func CheckBindCredential(listener string, c ListenerConfig, bearer bool, forward ForwardedToBind) error {
	if bearer {
		return nil
	}
	host := c.ListenHost
	if host == "" {
		host = "127.0.0.1"
	}
	bind := net.JoinHostPort(host, strconv.Itoa(c.Port))
	if forward != "" {
		return InputErrorf(listener, "%s binds %s, which %s forwards to: the listener judges loopback by its own bind and cannot see the forward, so it serves /v1 with no credential to every caller the forward admits unless it takes %s, the one credential that closes /v1 on loopback; its broker settings do not",
			listener, bind, forward, bearerKinds)
	}
	if isLoopbackHost(host) || c.AgentSecretsURL != "" {
		return nil
	}
	return InputErrorf(listener, "%s binds %s off loopback, where the listener refuses to start without %s", listener, bind, bindCredentials)
}

// isLoopbackHost is the listener's own reading of its bind (cmd/listener/apiauth.go,
// isLoopbackHost), copied for the reason brokerVars gives: localhost, or a loopback address.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
