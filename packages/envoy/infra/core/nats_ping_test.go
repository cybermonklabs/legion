package core_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// calloutPingLines are the top-level lines a callout adds after the authorization block: the
// server closes a connection that has sent nothing for 40 to 80 s, so a hung replica of the auth
// service leaves the callout's queue group (nats_ping_live_test.go).
const calloutPingLines = "# a replica of the auth service that answers nothing is dropped from the callout's queue group within 80 s\n" +
	"ping_interval: \"20s\"\n" +
	"ping_max: 2\n"

// TestACalloutRendersThePingLines: with a callout, ping_interval and ping_max end the text, after
// the authorization block and no_auth_user; without one, the text is byte for byte what it was
// before callouts existed (authorization-before.golden), so a hub with no auth service keeps
// nats-server's defaults.
func TestACalloutRendersThePingLines(t *testing.T) {
	text, err := withCallout().Render(hubHeader)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(text, "\n}\n"+
		"# example-sessions: a client presenting no credential becomes this user\n"+
		`no_auth_user: "UAZUULVPMGDGSEXFS26DBYEAMLBGWFX6HKOCQGIWTDBZARXTK3VRF5W5"`+"\n"+
		calloutPingLines), "%q", text)
	require.Equal(t, 1, strings.Count(text, "ping_interval"))

	private, err := privateOnly().Render(nil)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(private, "  }\n}\n"+calloutPingLines), "%q", private)

	without, err := hubShaped().Render(hubHeader)
	require.NoError(t, err)
	requireSameText(t, golden(t, "authorization-before.golden"), without)
}
