package core_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// The listener judges its bind alone: off loopback it starts only with a credential it verifies, a
// bearer kind or its broker settings, and answers a request that presents nothing 401; on loopback
// /v1 is open to a request that presents nothing unless it takes a bearer kind, its broker settings
// closing nothing there (cmd/listener/apiauth.go, requiresCredential, validateListenerAPIAuth). A
// render that writes a TCP forwarder to the bind passes the forward to the check, which then
// requires a bearer kind, the one credential that closes a loopback /v1: a forwarded listener with
// none, its broker settings alone included, is refused naming the forward. A loopback bind with no
// forwarder needs nothing. Negative controls, run by hand: the check reading the bind alone, under
// which the forwarded row with no credential passes; and the check counting the broker settings
// beside a forward, under which the forwarded broker row passes; each fails naming its row.
func TestCheckBindCredential(t *testing.T) {
	const forward = core.ForwardedToBind("TCPForward 127.0.0.1:9020")
	config := func(host string) core.ListenerConfig {
		return core.ListenerConfig{MachineID: "example-hub", Port: core.DefaultListenerPort, ListenHost: host}
	}
	broker := func(host string) core.ListenerConfig {
		c := config(host)
		c.AgentSecretsURL, c.AgentSecretsAudience, c.PublicURL = "https://example-broker.internal.example", "example-broker-audience", "https://example-hub.example-tailnet.ts.net:9020"
		return c
	}
	for _, row := range []struct {
		name    string
		config  core.ListenerConfig
		bearer  bool
		forward core.ForwardedToBind
		refused string // "" passes
	}{
		{name: "a loopback bind with no forwarder", config: config("127.0.0.1")},
		{name: "the listener's default bind with no forwarder", config: config("")},
		{name: "localhost with no forwarder", config: config("localhost")},
		{name: "::1 with no forwarder", config: config("::1")},
		{name: "a forwarded loopback bind with no credential", config: config("127.0.0.1"), forward: forward,
			refused: "listener binds 127.0.0.1:9020, which TCPForward 127.0.0.1:9020 forwards to"},
		{name: "a forwarded default bind with no credential", config: config(""), forward: forward,
			refused: "listener binds 127.0.0.1:9020, which TCPForward 127.0.0.1:9020 forwards to"},
		{name: "a forwarded loopback bind with a bearer kind", config: config("127.0.0.1"), bearer: true, forward: forward},
		{name: "a forwarded loopback bind with its broker settings alone", config: broker("127.0.0.1"), forward: forward,
			refused: "its broker settings do not"},
		{name: "an off-loopback bind with no credential", config: config("0.0.0.0"),
			refused: "listener binds 0.0.0.0:9020 off loopback"},
		{name: "an off-loopback bind with a bearer kind", config: config("0.0.0.0"), bearer: true},
		{name: "an off-loopback bind with its broker settings", config: broker("0.0.0.0")},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := core.CheckBindCredential("listener", row.config, row.bearer, row.forward)
			if row.refused == "" {
				require.NoError(t, err)
				return
			}
			requireInputError(t, err, "listener", row.refused)
		})
	}
}
