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

// calloutPingLines are the top-level lines core renders after the authorization block beside a
// callout.
const calloutPingLines = "# a replica of the auth service that answers nothing is dropped from the callout's queue group within 80 s\n" +
	"ping_interval: \"20s\"\n" +
	"ping_max: 2\n"

// privateProjectsTagLines are the lines core renders after the ping lines beside privateProjects:
// the server tag nats-server copies into each authorization request.
const privateProjectsTagLines = "# this hub is rendered with privateProjects: nats-server names this tag in each authorization request it sends the auth service\n" +
	`server_tags: ["envoy-private-projects"]` + "\n"

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

// The clustered hub's pods carry the SHA-256 of the callout's lines, the auth_callout block, the
// ping lines and, beside privateProjects, the server tag, in the annotation
// envoy-infra.sjawhar.dev/auth-callout: a change of any of them, an added user's key in auth_users
// or privateProjects set or removed among them, changes the pod template and rolls the pods one at
// a time, and a change of a user's grant alone leaves those lines and the annotation as they were,
// so the reloader applies it. core.Authorization.CalloutLines says why a change rolls rather than
// reloads.
//
// Controls: hash the auth_callout block alone (core.Authorization.CalloutLines without the ping
// lines); the first two rows fail. Write the server tag's lines after the callout's tail, outside
// CalloutLines; the privateProjects row fails, since the annotation no longer changes with it.
func TestClusteredNatsHubRollsThePodsForACalloutChange(t *testing.T) {
	annotation := func(t *testing.T, args *gcp.ClusteredNatsHubArgs) any {
		t.Helper()
		return podAnnotations(chartValues(t, runClusteredHub(t, args)))["envoy-infra.sjawhar.dev/auth-callout"]
	}
	digest := func(lines string) string {
		sum := sha256.Sum256([]byte(lines))
		return hex.EncodeToString(sum[:])
	}
	before := annotation(t, calloutClusteredHub())
	block := calloutBlock(keyA, keyB, keyC, keyD, keyAuth, keySessions)
	require.Equal(t, digest(block+calloutPingLines), before)
	require.NotEqual(t, digest(block), before,
		"the ping lines are in the digest, so a hub whose auth.conf had the callout without them rolls its pods to take them")

	args := calloutClusteredHub()
	args.AuthCallout.XKey = otherXKey(t)
	require.NotEqual(t, before, annotation(t, args), "another xkey rolls the pods")

	_, added := testnats.User(t)
	args = calloutClusteredHub()
	args.Users = append(args.Users, gcp.NATSUserArgs{Name: "example-host-b", NKey: added, MachineID: pulumi.StringRef("example-host-b")})
	require.Equal(t, digest(calloutBlock(keyA, keyB, keyC, keyD, keyAuth, added, keySessions)+calloutPingLines), annotation(t, args),
		"an added user's key in auth_users rolls the pods")

	args = calloutClusteredHub()
	args.Users[1].Permissions.Publish.Allow = []string{"notifications.>"}
	require.Equal(t, before, annotation(t, args), "a grant change alone leaves the callout's lines, and the pods, as they were")

	// The server tag is a callout line, so setting privateProjects rolls every pod and removing it
	// rolls them back: nats-server also reloads server_tags, and each reload decides every callout
	// connection again, but the roll is what brings a change of these lines to every server at one
	// known moment, its restart, after the ConfigMap is applied.
	public := privateClusteredHub()
	public.PrivateProjects = false
	untagged := annotation(t, public)
	tagged := annotation(t, privateClusteredHub())
	require.Equal(t, digest(calloutBlock(keyAuth)+calloutPingLines+privateProjectsTagLines), tagged,
		"setting privateProjects rolls the pods to take the server tag")
	require.NotEqual(t, untagged, tagged, "privateProjects leaves the pods' annotation as it was, so the tag would reach no running server")
	require.Equal(t, digest(calloutBlock(keyAuth)+calloutPingLines), untagged,
		"removing privateProjects rolls the pods back to the untagged lines")
}

// Beside a callout, the chart's cluster block pins the routes' own ping at today's effective
// values, ping_interval "30s" and ping_max 2, and without a callout the cluster block is as it was
// (R9). clusteredHubCluster says why the routes need the pin.
//
// Control: drop the merge from the cluster block; the first row fails.
func TestClusteredNatsHubPinsTheRoutesPingBesideACallout(t *testing.T) {
	cluster := func(t *testing.T, args *gcp.ClusteredNatsHubArgs) map[string]any {
		t.Helper()
		return chartValues(t, runClusteredHub(t, args))["config"].(map[string]any)["cluster"].(map[string]any)
	}
	require.Equal(t, map[string]any{"ping_interval": "30s", "ping_max": 2.0}, cluster(t, calloutClusteredHub())["merge"])
	require.Equal(t, map[string]any{"enabled": true, "replicas": 3.0, "noAdvertise": true}, cluster(t, clusteredArgs()),
		"without a callout the cluster block is as it was")
}

// TestClusteredNatsHubHashesTheRenderedCalloutBlock: the pods' roll annotation is the SHA-256 of
// the callout's lines exactly as the hub's auth.conf holds them: the auth_callout block cut from
// the rendered text (what follows the users' closing "  ]" line, up to the authorization block's
// own "}" line), then the ping lines and, beside privateProjects, the server tag's lines
// (TestClusteredNatsHubRollsThePodsForACalloutChange), which end it. So the hash cannot drift from
// what the servers read. For each hub shape it changes, the callout's own, another xkey and an
// added user.
//
// Control: make core's Render write its own block and CalloutLines leave out the last user's key;
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
			require.True(t, strings.HasSuffix(s.AuthConf, calloutPingLines), "%q", s.AuthConf)
			sum := sha256.Sum256([]byte(block + "\n" + calloutPingLines))
			require.Equal(t, hex.EncodeToString(sum[:]),
				podAnnotations(chartValues(t, runClusteredHub(t, args)))["envoy-infra.sjawhar.dev/auth-callout"])
		})
	}
}
