package gcp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/nats"
	"github.com/sjawhar/envoy/internal/testnats"
)

func calloutNatsHub() *gcp.NatsHubArgs {
	args := exampleArgs()
	args.Users = append(args.Users, authServiceUser())
	args.Listener = nil
	args.AuthCallout = authCallout()
	return args
}

func calloutClusteredHub() *gcp.ClusteredNatsHubArgs {
	args := withoutTailnet(clusteredArgs())
	args.Users = append(args.Users, authServiceUser())
	args.AuthCallout = authCallout()
	return args
}

// calloutBlock is the callout's lines of an authorization block Render wrote for keys, the keyed
// users in their order. It sets no authorization timeout, so nats-server's default of 2 s stands.
func calloutBlock(keys ...string) string {
	return "  auth_callout {\n" +
		`    issuer: "` + calloutIssuer + `"` + "\n" +
		`    auth_users: ["` + strings.Join(keys, `", "`) + `"]` + "\n" +
		`    xkey: "` + calloutXKey + `"` + "\n" +
		"  }\n"
}

func otherXKey(t *testing.T) string {
	t.Helper()
	curve, err := nkeys.CreateCurveKeys()
	require.NoError(t, err)
	key, err := curve.PublicKey()
	require.NoError(t, err)
	return key
}

// Both hubs render the callout inside the authorization block, after the users, with every keyed
// user's key in auth_users in the users' order: the session user's last, since the hub renders it
// last; the clustered hub's auth.conf is the VM hub's text for the same users.
func TestTheHubsRenderTheCalloutBlock(t *testing.T) {
	conf, err := gcp.NatsHubAuthConf(calloutNatsHub())
	require.NoError(t, err)
	block := calloutBlock(keyA, keyB, keyC, keyD, keyAuth, keySessions)
	require.Contains(t, conf, "  ]\n"+block+"}\n", "the callout follows the users inside the authorization block")
	require.Equal(t, 1, strings.Count(conf, "auth_callout"))

	s, err := gcp.ResolveClusteredNatsHub(calloutClusteredHub())
	require.NoError(t, err)
	require.Equal(t, conf, s.AuthConf, "the clustered hub renders the VM hub's authorization for the same users")
}

// Without authCallout each hub renders what it rendered before the callout existed: the
// configuration and every other value hold no callout line (R9). The goldens the other tests pin
// (cloud-config.golden, the chart values) are that text, unchanged.
func TestAHubWithoutTheCalloutRendersNoCalloutLine(t *testing.T) {
	conf, err := gcp.NatsHubAuthConf(exampleArgs())
	require.NoError(t, err)
	require.NotContains(t, conf, "auth_callout")
	require.NotContains(t, conf, "timeout")
	values := chartValues(t, runClusteredHub(t, clusteredArgs()))
	require.NotContains(t, podAnnotations(values), "envoy-infra.sjawhar.dev/auth-callout")
}

// A hub behind the auth service runs no listener of its own until the listener has a service
// identity to connect through the auth service with, which arrives with dispatch://INFRA-840's
// t-e-13: today the listener connects as a NATS user whose seed the hub stores. So listener
// (gcp:NatsHub) and tailnet.listener (gcp:ClusteredNatsHub, whose tailnet always runs one) beside
// authCallout are refused, naming both inputs, and nothing registers. The refusal is this slice's
// stated behaviour until t-e-13 lands, and the task that sets the listener's service identity
// removes it.
func TestAHubBehindTheAuthServiceRunsNoListenerOfItsOwn(t *testing.T) {
	t.Run("gcp:NatsHub", func(t *testing.T) {
		args := calloutNatsHub()
		args.Listener = exampleArgs().Listener
		err := gcp.ResolveNatsHub(args)
		var input *core.InputError
		require.ErrorAs(t, err, &input)
		require.Equal(t, "listener", input.Input)
		require.ErrorContains(t, err, "listener and authCallout:")
		require.ErrorContains(t, err, "its service identity arrives with dispatch://INFRA-840 (t-e-13)")
		m, err := run(t, args, nil, true)
		require.ErrorContains(t, err, "listener and authCallout:")
		require.Equal(t, []string{gcpProviderType}, m.Types(), "nothing registered beyond the test's provider")
	})
	t.Run("gcp:ClusteredNatsHub", func(t *testing.T) {
		args := clusteredArgs()
		args.Users = append(args.Users, authServiceUser())
		args.AuthCallout = authCallout()
		_, err := gcp.ResolveClusteredNatsHub(args)
		var input *core.InputError
		require.ErrorAs(t, err, &input)
		require.Equal(t, "tailnet.listener", input.Input)
		require.ErrorContains(t, err, "tailnet.listener and authCallout:")
		require.ErrorContains(t, err, "its service identity arrives with dispatch://INFRA-840 (t-e-13)")
		m, err := runClustered(t, "example-clustered-hub", args, nil)
		require.ErrorContains(t, err, "tailnet.listener and authCallout:")
		require.Empty(t, m.Registrations(), "nothing registered")
	})
}

// core's refusals of the callout reach the consumer as a core.InputError naming authCallout.
func TestAHubRefusesACalloutCoreRefuses(t *testing.T) {
	for _, row := range []struct {
		name string
		edit func(c *nats.AuthCalloutArgs)
		want string
	}{
		{"an issuer that is a user key", func(c *nats.AuthCalloutArgs) { c.Issuer = keyA }, "authCallout: callout.issuer is not an account public key"},
		{"an xkey that is an account key", func(c *nats.AuthCalloutArgs) { c.XKey = calloutIssuer }, "authCallout: callout.xkey is not a curve public key"},
		{"no xkey", func(c *nats.AuthCalloutArgs) { c.XKey = "" }, "authCallout: callout.xkey is required: without it nats-server sends each connecting client's token to the auth service unsealed"},
		{"a service user naming nobody", func(c *nats.AuthCalloutArgs) { c.ServiceUser = "example-nobody" }, `authCallout: callout.serviceUser "example-nobody" names no keyed user`},
		{"the session user as the service user", func(c *nats.AuthCalloutArgs) { c.ServiceUser = "example-sessions" }, `authCallout: callout.serviceUser "example-sessions" is noAuthUser`},
	} {
		t.Run(row.name, func(t *testing.T) {
			vm := calloutNatsHub()
			row.edit(vm.AuthCallout)
			clustered := calloutClusteredHub()
			row.edit(clustered.AuthCallout)
			_, clusteredErr := gcp.ResolveClusteredNatsHub(clustered)
			for _, err := range []error{gcp.ResolveNatsHub(vm), clusteredErr} {
				var input *core.InputError
				require.ErrorAs(t, err, &input)
				require.Equal(t, "authCallout", input.Input, "%v", err)
				require.ErrorContains(t, err, row.want)
			}
		})
	}
}

// nats-server v2.15.1 refuses a reload that changes its authorization callout ("config reload not
// supported for AuthCallout", which clusteredhub_callout_live_test.go measures), so the clustered
// hub's pods carry the SHA-256 of the callout's lines in the annotation
// envoy-infra.sjawhar.dev/auth-callout: a callout change, an added user's key in auth_users
// among them, changes the pod template and rolls the pods one at a time. A change of a user's grant
// alone leaves the callout's lines and the annotation as they were, so the reloader applies it.
func TestClusteredNatsHubRollsThePodsForACalloutChange(t *testing.T) {
	annotation := func(t *testing.T, args *gcp.ClusteredNatsHubArgs) any {
		t.Helper()
		return podAnnotations(chartValues(t, runClusteredHub(t, args)))["envoy-infra.sjawhar.dev/auth-callout"]
	}
	digest := func(block string) string {
		sum := sha256.Sum256([]byte(block))
		return hex.EncodeToString(sum[:])
	}
	before := annotation(t, calloutClusteredHub())
	require.Equal(t, digest(calloutBlock(keyA, keyB, keyC, keyD, keyAuth, keySessions)), before)

	args := calloutClusteredHub()
	args.AuthCallout.XKey = otherXKey(t)
	require.NotEqual(t, before, annotation(t, args), "another xkey rolls the pods")

	_, added := testnats.User(t)
	args = calloutClusteredHub()
	args.Users = append(args.Users, gcp.NATSUserArgs{Name: "example-host-b", NKey: added, MachineID: pulumi.StringRef("example-host-b")})
	require.Equal(t, digest(calloutBlock(keyA, keyB, keyC, keyD, keyAuth, added, keySessions)), annotation(t, args),
		"an added user's key in auth_users rolls the pods")

	args = calloutClusteredHub()
	args.Users[1].Permissions.Publish.Allow = []string{"notifications.>"}
	require.Equal(t, before, annotation(t, args), "a grant change alone leaves the callout's lines, and the pods, as they were")
}

// TestClusteredNatsHubHashesTheRenderedCalloutBlock: the pods' roll annotation is the SHA-256 of
// the callout block exactly as the hub's auth.conf holds it, cut from the rendered text (what
// follows the users' closing "  ]" line, up to the authorization block's own "}" line), so the hash
// cannot drift from what the servers read. For each hub shape it changes, the callout's own,
// another xkey and an added user.
//
// Control: make core's Render write its own block and CalloutBlock leave out the last user's key;
// every row fails.
func TestClusteredNatsHubHashesTheRenderedCalloutBlock(t *testing.T) {
	withXKey := calloutClusteredHub()
	withXKey.AuthCallout.XKey = otherXKey(t)
	_, added := testnats.User(t)
	withUser := calloutClusteredHub()
	withUser.Users = append(withUser.Users, gcp.NATSUserArgs{Name: "example-host-b", NKey: added, MachineID: pulumi.StringRef("example-host-b")})
	for name, args := range map[string]*gcp.ClusteredNatsHubArgs{
		"the callout's own": calloutClusteredHub(), "another xkey": withXKey, "an added user": withUser,
	} {
		t.Run(name, func(t *testing.T) {
			s, err := gcp.ResolveClusteredNatsHub(args)
			require.NoError(t, err)
			_, afterUsers, found := strings.Cut(s.AuthConf, "\n  ]\n")
			require.True(t, found, "no users list in %q", s.AuthConf)
			block, _, found := strings.Cut(afterUsers, "\n}\n")
			require.True(t, found, "no closing authorization line in %q", s.AuthConf)
			require.True(t, strings.HasPrefix(block, "  auth_callout {\n"), "%q", block)
			sum := sha256.Sum256([]byte(block + "\n"))
			require.Equal(t, hex.EncodeToString(sum[:]),
				podAnnotations(chartValues(t, runClusteredHub(t, args)))["envoy-infra.sjawhar.dev/auth-callout"])
		})
	}
}
