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

// calloutNatsHub is exampleArgs behind the auth service: today's users and session user stay, the
// auth service's user joins them, and the hub runs no listener of its own (calloutListener adds
// one).
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

// calloutNatsAudience is the audience the listener of a hub behind the auth service presents its
// platform identity token for: the broker's BROKER_NATS_GOOGLE_AUDIENCE for a VM's Google token,
// its BROKER_NATS_K8S_AUDIENCE for a pod's projected one.
const calloutNatsAudience = "example-nats-audience"

// calloutListener is a gcp:NatsHub listener behind the auth service, with its broker settings: no
// user, no seed, and the audience of the VM's Google identity token it would connect with instead.
// The hub refuses it as it refuses every listener of its own (TestAForwardedLoopbackListenerIsOffLoopback).
func calloutListener() *gcp.HubListenerArgs {
	return &gcp.HubListenerArgs{Image: listenerImage, NatsAudience: calloutNatsAudience,
		AgentSecretsUrl: brokerURL, AgentSecretsAudience: brokerAudience, PublicUrl: listenerOrigin}
}

// calloutClusteredHubWithTailnet is clusteredArgs behind the auth service, its tailnet's listener
// connecting by the projected token of its own ServiceAccount for calloutNatsAudience: no user and
// no seed Secret, with clusteredArgs' broker settings.
func calloutClusteredHubWithTailnet() *gcp.ClusteredNatsHubArgs {
	args := clusteredArgs()
	args.Users = append(args.Users, authServiceUser())
	args.AuthCallout = authCallout()
	args.Tailnet.Listener = withBroker(&gcp.ClusteredHubListenerArgs{Image: listenerImage, NatsAudience: calloutNatsAudience})
	return args
}

// A clustered hub behind the auth service runs its session listener by the listener's service
// identity, which the auth service admits it by (t-e-13), and not as a user whose seed the hub
// stores: the start is not refused, the authorization holds no user for the listener, so its key is
// in no auth_users list, and the listener is handed no seed. It presents the projected token of its
// own ServiceAccount for tailnet.listener.natsAudience, read from a file of the pod, which mounts no
// Secret. This is the case PlanC's slice C1 refused, inverted. gcp:NatsHub runs no listener of its
// own from t-e-19 on (TestAForwardedLoopbackListenerIsOffLoopback).
func TestACalloutHubRunsItsListenerByServiceIdentity(t *testing.T) {
	t.Run("gcp:ClusteredNatsHub", func(t *testing.T) {
		args := calloutClusteredHubWithTailnet()
		s, err := gcp.ResolveClusteredNatsHub(args)
		require.NoError(t, err)
		require.NotContains(t, s.AuthConf, keyListener, "the authorization holds no user for the listener")
		withoutListener, err := gcp.ResolveClusteredNatsHub(calloutClusteredHub())
		require.NoError(t, err)
		require.Equal(t, withoutListener.AuthConf, s.AuthConf)

		m := runClusteredHub(t, args)
		requirePodGolden(t, m, clusteredListenerName, "clustered-hub-listener-pod-service-identity.golden")
		template := deploymentPodTemplate(t, m, clusteredListenerName)
		env := containerEnv(t, podContainer(t, template, "listener"))
		require.Equal(t, "/var/run/secrets/envoy/nats-token", env["NATS_SERVICE_TOKEN_FILE"])
		require.NotContains(t, env, "NATS_NKEY_SEED_FILE", "the listener is handed no seed")
		volumes := template["spec"].(map[string]any)["volumes"].([]any)
		require.Equal(t, []any{
			map[string]any{"name": "nats-token", "projected": map[string]any{"sources": []any{
				map[string]any{"serviceAccountToken": map[string]any{"audience": calloutNatsAudience, "expirationSeconds": 3600.0, "path": "nats-token"}},
			}}},
			map[string]any{"name": "envoy-broker-token", "projected": map[string]any{"sources": []any{
				map[string]any{"serviceAccountToken": map[string]any{"audience": clusteredBrokerAudience, "expirationSeconds": 3600.0, "path": "token"}},
			}}},
		}, volumes, "the projected tokens of the listener's own ServiceAccount, its NATS one and its broker one, and no Secret")
	})
}

// A clustered hub's session listener behind the auth service is no user of the hub, but it still
// binds the durable and the queue group of its machine id, so a static user given that machine id
// is refused as it is beside a seeded listener: two listeners would share one durable. gcp:NatsHub
// runs no listener of its own from t-e-19 on.
func TestACalloutHubReservesItsServiceListenersDurable(t *testing.T) {
	t.Run("gcp:ClusteredNatsHub", func(t *testing.T) {
		args := calloutClusteredHubWithTailnet()
		args.Tailnet.Listener.MachineID = pulumi.StringRef("example host a")
		_, err := gcp.ResolveClusteredNatsHub(args)
		var input *core.InputError
		require.ErrorAs(t, err, &input)
		require.Equal(t, "users[0].machineId", input.Input, "%v", err)
		require.ErrorContains(t, err, "users[0]: machineId gives the same durable name as the hub's own listener")
	})
}

// A control character that is no whitespace (NUL, DEL) in a session listener's natsAudience is
// refused as whitespace is: no environment line can carry it.
func TestAHubListenersAudienceRefusesAControlCharacter(t *testing.T) {
	for _, value := range []string{"nats\x00audience", "nats\x7faudience"} {
		t.Run("gcp:ClusteredNatsHub", func(t *testing.T) {
			args := calloutClusteredHubWithTailnet()
			args.Tailnet.Listener.NatsAudience = value
			_, err := gcp.ResolveClusteredNatsHub(args)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, "tailnet.listener.natsAudience", input.Input, "%v", err)
			require.ErrorContains(t, err, "whitespace or a control character")
		})
	}
}

// Without authCallout a clustered hub renders its session listener as before: the listener
// connects as the user the hub renders for its seed, and the goldens the other tests pin are that
// text. So natsAudience, which only the auth service reads, is refused without it, and with it the
// listener's seed inputs are refused, since the listener connects as one identity: nkey,
// seedSecretName and seedSecretKey. natsAudience is required with authCallout, and refused holding
// whitespace or a control character. Each refusal is a core.InputError naming the input, and
// nothing registers. gcp:NatsHub runs no listener of its own from t-e-19 on.
func TestAHubListenersIdentityFollowsTheCallout(t *testing.T) {
	// row is one refusal: bare starts from the hub with no callout, and every other row from the
	// hub behind the auth service whose listener connects by its service identity.
	type row struct {
		name, input, want string
		bare              bool
	}
	t.Run("gcp:ClusteredNatsHub", func(t *testing.T) {
		for _, r := range []struct {
			row
			edit func(*gcp.ClusteredHubListenerArgs)
		}{
			{row{"natsAudience without authCallout", "tailnet.listener.natsAudience", "tailnet.listener.natsAudience is set without authCallout", true},
				func(l *gcp.ClusteredHubListenerArgs) { l.NatsAudience = calloutNatsAudience }},
			{row{"no natsAudience with authCallout", "tailnet.listener.natsAudience", "tailnet.listener.natsAudience is required with authCallout", false},
				func(l *gcp.ClusteredHubListenerArgs) { l.NatsAudience = "" }},
			{row{"a listener nkey with authCallout", "tailnet.listener.nkey", "tailnet.listener.nkey is set beside authCallout", false},
				func(l *gcp.ClusteredHubListenerArgs) { l.NKey = keyListener }},
			{row{"a seed Secret's name with authCallout", "tailnet.listener.seedSecretName", "tailnet.listener.seedSecretName is set beside authCallout", false},
				func(l *gcp.ClusteredHubListenerArgs) { l.SeedSecretName = "envoy-hub-listener-seed" }},
			{row{"a seed Secret's key with authCallout", "tailnet.listener.seedSecretKey", "tailnet.listener.seedSecretKey is set beside authCallout", false},
				func(l *gcp.ClusteredHubListenerArgs) { l.SeedSecretKey = "seed" }},
			{row{"a natsAudience with a line break", "tailnet.listener.natsAudience", "tailnet.listener.natsAudience holds whitespace or a control character", false},
				func(l *gcp.ClusteredHubListenerArgs) { l.NatsAudience = calloutNatsAudience + "\nx" }},
		} {
			t.Run(r.name, func(t *testing.T) {
				args := calloutClusteredHubWithTailnet()
				if r.bare {
					args = clusteredArgs()
				}
				r.edit(args.Tailnet.Listener)
				_, err := gcp.ResolveClusteredNatsHub(args)
				var input *core.InputError
				require.ErrorAs(t, err, &input)
				require.Equal(t, r.input, input.Input, "%v", err)
				require.ErrorContains(t, err, r.want)
				m, err := runClustered(t, "example-clustered-hub", args, nil)
				require.ErrorContains(t, err, r.want)
				require.Empty(t, m.Registrations(), "nothing registered")
			})
		}
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

// The clustered hub's pods carry the SHA-256 of the callout's lines, the auth_callout block and the
// ping lines, in the annotation envoy-infra.sjawhar.dev/auth-callout: a change of either, an added
// user's key in auth_users among them, changes the pod template and rolls the pods one at a time,
// and a change of a user's grant alone leaves those lines and the annotation as they were, so the
// reloader applies it. core.Authorization.CalloutLines says why a change rolls rather than reloads.
//
// Control: hash the auth_callout block alone (core.Authorization.CalloutLines without the ping
// lines); the first two rows fail.
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
// own "}" line), then the ping lines that end it. So the hash cannot drift from what the servers
// read. For each hub shape it changes, the callout's own, another xkey and an added user.
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
