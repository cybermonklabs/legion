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

// calloutListener is exampleArgs' listener behind the auth service: no user, no seed, and the
// audience of the VM's Google identity token it connects with instead.
func calloutListener() *gcp.HubListenerArgs {
	return &gcp.HubListenerArgs{Image: listenerImage, NatsAudience: calloutNatsAudience}
}

// calloutClusteredHubWithTailnet is clusteredArgs behind the auth service, its tailnet's listener
// connecting by the projected token of its own ServiceAccount for calloutNatsAudience: no user and
// no seed Secret.
func calloutClusteredHubWithTailnet() *gcp.ClusteredNatsHubArgs {
	args := clusteredArgs()
	args.Users = append(args.Users, authServiceUser())
	args.AuthCallout = authCallout()
	args.Tailnet.Listener = &gcp.ClusteredHubListenerArgs{Image: listenerImage, NatsAudience: calloutNatsAudience}
	return args
}

// A hub behind the auth service runs its own listener by the listener's service identity, which
// the auth service admits it by (t-e-13), and not as a user whose seed the
// hub stores: the start is not refused, the authorization holds no user for the listener, so its
// key is in no auth_users list, and the listener is handed no seed. gcp:NatsHub's VM listener
// presents the VM's Google identity token for listener.natsAudience, with no env file and no
// fetch of a seed before its start, and the hub's account is granted no seed secret;
// gcp:ClusteredNatsHub's presents the projected token of its own ServiceAccount for
// tailnet.listener.natsAudience, read from a file of the pod, which mounts no Secret. This is the
// case PlanC's slice C1 refused, inverted.
func TestACalloutHubRunsItsListenerByServiceIdentity(t *testing.T) {
	t.Run("gcp:NatsHub", func(t *testing.T) {
		args := calloutNatsHub()
		args.Listener = calloutListener()
		doc, _, err := gcp.CloudConfigOf(args)
		require.NoError(t, err)
		unit := fileOf(t, doc, "/etc/systemd/system/envoy-listener.service")
		require.Contains(t, unit, " -e NATS_SERVICE_GOOGLE_AUDIENCE="+calloutNatsAudience+" "+listenerImage+"\n",
			"the listener presents the VM's Google identity token for natsAudience")
		for _, absent := range []string{"--env-file", "listener-seed-env", "NATS_NKEY_SEED", "NATS_SERVICE_TOKEN_FILE"} {
			require.NotContains(t, unit, absent, "the listener is handed no seed")
		}
		for _, f := range doc.WriteFiles {
			require.NotEqual(t, "/etc/envoy/listener-seed-env", f.Path, "the VM writes no seed script")
		}
		conf, err := gcp.NatsHubAuthConf(args)
		require.NoError(t, err)
		withoutListener, err := gcp.NatsHubAuthConf(calloutNatsHub())
		require.NoError(t, err)
		require.Equal(t, withoutListener, conf, "the authorization holds no user for the listener")
		require.NotContains(t, conf, keyListener)

		m, err := run(t, args, nil, true)
		require.NoError(t, err)
		accessors := 0
		for _, r := range m.Registrations() {
			require.NotEqual(t, "example-hub-listener-seed-accessor", r.Name, "the hub's account is granted no seed secret")
			if r.Type == accessorType {
				accessors++
			}
		}
		require.Equal(t, 1, accessors, "the tailnet auth key's accessor alone")
	})
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
		require.Len(t, volumes, 1, "the token's volume alone")
		require.Equal(t, map[string]any{"name": "nats-token", "projected": map[string]any{"sources": []any{
			map[string]any{"serviceAccountToken": map[string]any{"audience": calloutNatsAudience, "expirationSeconds": 3600.0, "path": "nats-token"}},
		}}}, volumes[0], "a projected token of the listener's own ServiceAccount, and no Secret")
	})
}

// systemd expands specifiers and environment references and parses quotes and backslashes in
// ExecStart. The listener must receive natsAudience literally, not a different token audience or
// a command systemd cannot start. Ordinary audiences retain the existing unit's bytes.
func TestACalloutHubPreservesItsListenerAudienceInTheUnit(t *testing.T) {
	for _, row := range []struct {
		name, audience, argument string
	}{
		{"ordinary", "example-nats-audience", "NATS_SERVICE_GOOGLE_AUDIENCE=example-nats-audience"},
		{"encoded URL", "https://broker.example/nats%2Flistener", `"NATS_SERVICE_GOOGLE_AUDIENCE=https://broker.example/nats%%2Flistener"`},
		{"unit specifier", "nats-%n", `"NATS_SERVICE_GOOGLE_AUDIENCE=nats-%%n"`},
		{"environment reference", "nats-${USER}", `"NATS_SERVICE_GOOGLE_AUDIENCE=nats-$${USER}"`},
		{"quotes", `nats-'listener'"audience"`, `"NATS_SERVICE_GOOGLE_AUDIENCE=nats-'listener'\"audience\""`},
		{"backslash", `nats-\x41`, `"NATS_SERVICE_GOOGLE_AUDIENCE=nats-\\x41"`},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := calloutNatsHub()
			args.Listener = calloutListener()
			args.Listener.NatsAudience = row.audience
			doc, _, err := gcp.CloudConfigOf(args)
			require.NoError(t, err)
			unit := fileOf(t, doc, "/etc/systemd/system/envoy-listener.service")
			require.Contains(t, unit, " -e "+row.argument+" "+listenerImage+"\n")
		})
	}
}

// Every value the listener's unit carries reaches it literally, not natsAudience alone: the broker
// check's URL and audience and the public URL are inputs a consumer writes too, and an encoded
// `%2F` or a `$` in one made systemd refuse the unit's load or change the value. A unit whose
// values hold no such character is byte for byte what it was (the goldens).
func TestTheHubListenerUnitCarriesEveryValueLiterally(t *testing.T) {
	for _, callout := range []bool{false, true} {
		args := exampleArgs()
		if callout {
			args = calloutNatsHub()
			args.Listener = calloutListener()
		}
		args.Listener.AgentSecretsUrl = "https://broker.example/agent%2Fsecrets"
		args.Listener.AgentSecretsAudience = "https://broker.example/aud%2F$USER"
		args.Listener.PublicUrl = "https://envoy.example/v1%2Fx"
		doc, _, err := gcp.CloudConfigOf(args)
		require.NoError(t, err, "authCallout %t", callout)
		unit := fileOf(t, doc, "/etc/systemd/system/envoy-listener.service")
		for _, argument := range []string{
			`"ENVOY_AGENT_SECRETS_URL=https://broker.example/agent%%2Fsecrets"`,
			`"ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE=https://broker.example/aud%%2F$$USER"`,
			`"ENVOY_PUBLIC_URL=https://envoy.example/v1%%2Fx"`,
		} {
			require.Contains(t, unit, " -e "+argument+" ", "authCallout %t", callout)
		}
	}
}

// A hub's own listener behind the auth service is no user of the hub, but it still binds the
// durable and the queue group of its machine id, so a static user given that machine id is
// refused on each hub as it is beside a seeded listener: two listeners would share one durable.
func TestACalloutHubReservesItsServiceListenersDurable(t *testing.T) {
	t.Run("gcp:NatsHub", func(t *testing.T) {
		args := calloutNatsHub()
		args.Listener = calloutListener()
		args.Users[3].ListenerMachineID = pulumi.StringRef("example-hub")
		requireMachineIDRefusal(t, args, "users[3].listenerMachineId",
			"users[3]: listenerMachineId gives the same durable name as the hub's own listener")
	})
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

// A control character that is no whitespace (NUL, DEL) in a hub listener's natsAudience is refused
// as whitespace is: no environment line or unit can carry it.
func TestAHubListenersAudienceRefusesAControlCharacter(t *testing.T) {
	for _, value := range []string{"nats\x00audience", "nats\x7faudience"} {
		t.Run("gcp:NatsHub", func(t *testing.T) {
			args := calloutNatsHub()
			args.Listener = calloutListener()
			args.Listener.NatsAudience = value
			err := gcp.ResolveNatsHub(args)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, "listener.natsAudience", input.Input, "%v", err)
			require.ErrorContains(t, err, "whitespace or a control character")
		})
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

// Without authCallout a hub renders its listener exactly as before: the listener connects as the
// user the hub renders for its seed, and the goldens the other tests pin are that text. So
// natsAudience, which only the auth service reads, is refused without it, and with it the
// listener's seed inputs are refused, since the listener connects as one identity: nkey and
// seedSecret on gcp:NatsHub, nkey, seedSecretName and seedSecretKey on gcp:ClusteredNatsHub.
// natsAudience is required with authCallout, and refused holding whitespace or a control
// character. Each refusal is a core.InputError naming the input, and nothing registers.
func TestAHubListenersIdentityFollowsTheCallout(t *testing.T) {
	// row is one refusal: bare starts from the hub with no callout, and every other row from the
	// hub behind the auth service whose listener connects by its service identity.
	type row struct {
		name, input, want string
		bare              bool
	}
	t.Run("gcp:NatsHub", func(t *testing.T) {
		for _, r := range []struct {
			row
			edit func(*gcp.HubListenerArgs)
		}{
			{row{"natsAudience without authCallout", "listener.natsAudience", "listener.natsAudience is set without authCallout", true},
				func(l *gcp.HubListenerArgs) { l.NatsAudience = calloutNatsAudience }},
			{row{"no natsAudience with authCallout", "listener.natsAudience", "listener.natsAudience is required with authCallout", false},
				func(l *gcp.HubListenerArgs) { l.NatsAudience = "" }},
			{row{"a listener nkey with authCallout", "listener.nkey", "listener.nkey is set beside authCallout", false},
				func(l *gcp.HubListenerArgs) { l.NKey = keyListener }},
			{row{"a listener seed secret with authCallout", "listener.seedSecret", "listener.seedSecret is set beside authCallout", false},
				func(l *gcp.HubListenerArgs) { l.SeedSecret = listenerSeed }},
			{row{"a natsAudience with a space", "listener.natsAudience",
				"listener.natsAudience holds whitespace or a control character, which NATS_SERVICE_GOOGLE_AUDIENCE cannot carry", false},
				func(l *gcp.HubListenerArgs) { l.NatsAudience = calloutNatsAudience + " --privileged" }},
		} {
			t.Run(r.name, func(t *testing.T) {
				args := exampleArgs()
				if !r.bare {
					args = calloutNatsHub()
					args.Listener = calloutListener()
				}
				r.edit(args.Listener)
				err := gcp.ResolveNatsHub(args)
				var input *core.InputError
				require.ErrorAs(t, err, &input)
				require.Equal(t, r.input, input.Input, "%v", err)
				require.ErrorContains(t, err, r.want)
				m, err := run(t, args, nil, true)
				require.ErrorContains(t, err, r.want)
				require.Equal(t, []string{gcpProviderType}, m.Types(), "nothing registered beyond the test's provider")
			})
		}
	})
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
