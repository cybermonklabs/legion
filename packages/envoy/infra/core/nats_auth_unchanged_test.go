package core_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/infra/core"
)

// hubHeader is the header the hub-shaped goldens were rendered with.
var hubHeader = []string{"Rendered by envoy-infra from hub-shaped placeholder inputs.", "Public keys only."}

// hubShaped is an authorization in the shape a hub renders (gcp authorization): keyed users with a
// seed for the hub's own listener, Dispatch and a daemon, a pending user, and a session user with
// no seed whom a client presenting no credential becomes. Its keys are public keys only; their
// seeds were discarded when they were made.
func hubShaped() core.Authorization {
	return core.Authorization{
		Users: []core.User{
			{
				Name:    "example-hub",
				NKey:    "UCVLUXHFBITELHZUNIQG35K5PGQJIP6RX4LUG32L5I22JL7VGXKJQ4BV",
				Publish: &core.Rules{Allow: []string{"notifications.>", "$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS"}, Deny: []string{"notifications.envoy.deliver.>"}},
				Subscribe: &core.Rules{Allow: []string{
					"_INBOX.>", "notifications.role.> notifications-role", "notifications.café.>",
				}},
				HasSeed: true,
			},
			{
				Name:      "example-dispatch",
				NKey:      "UAUK2YFQ7SM5YNV4VH6NPEKN3UZVY43S4JUE5767FR4CX76R7E7JTWYJ",
				Publish:   &core.Rules{Allow: []string{"notifications.dispatch.>", "$JS.API.>"}},
				Subscribe: &core.Rules{Allow: []string{"_INBOX.>"}},
				HasSeed:   true,
			},
			{
				Name:      "example-daemon",
				NKey:      "UBHS7ZOUV24G5LDHYM4H4PBRLBEYK62JIFJVBMFBRCY63D3XC45MCRKF",
				Publish:   &core.Rules{Allow: []string{"$JS.API.CONSUMER.>", "$JS.ACK.ENVOY_NOTIFICATIONS.>"}},
				Subscribe: &core.Rules{Allow: []string{"_INBOX.>"}},
				HasSeed:   true,
			},
			{Name: "example-pending", HasSeed: true},
			{
				Name:      "example-sessions",
				NKey:      "UAZUULVPMGDGSEXFS26DBYEAMLBGWFX6HKOCQGIWTDBZARXTK3VRF5W5",
				Publish:   &core.Rules{Allow: []string{"notifications.agent.>"}},
				Subscribe: &core.Rules{Allow: []string{"_INBOX.>", "notifications.envoy.deliver.*"}},
			},
		},
		NoAuthUser: "example-sessions",
	}
}

// requireSameText fails the test unless got is want byte for byte, naming the first line that
// differs (1-based) with both texts' line there, so a failure says where the rendering moved.
func requireSameText(t *testing.T, want, got string) {
	t.Helper()
	if got == want {
		return
	}
	wantLines, gotLines := strings.SplitAfter(want, "\n"), strings.SplitAfter(got, "\n")
	for i := range max(len(wantLines), len(gotLines)) {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			t.Fatalf("the rendered text differs from the golden first at line %d:\n  golden:   %q\n  rendered: %q", i+1, w, g)
		}
	}
}

// TestAuthorizationWithNoPrivateProjectRendersAsBefore (c-hub-config-unchanged, R9): an
// authorization with no callout and no private projects renders exactly the text the writer at
// this change's merge base (cybermonklabs/legion 545b211633) rendered for the same users,
// authorization-before.golden, which was written once by running that commit's Render on
// hubShaped() and is never regenerated.
//
// Negative control: make render write the callout block whatever Callout holds (drop its
// `a.Callout != nil` guard, rendering an empty AuthCallout when it is nil). This test then fails at
// line 14, the rendered `  auth_callout {` where the golden closes the authorization block with `}`.
func TestAuthorizationWithNoPrivateProjectRendersAsBefore(t *testing.T) {
	text, err := hubShaped().Render(hubHeader)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	requireSameText(t, golden(t, "authorization-before.golden"), text)
}
