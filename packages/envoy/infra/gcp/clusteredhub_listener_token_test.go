package gcp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
)

// clusteredNatsAudience is the audience of the projected ServiceAccount token a clustered hub's
// session listener presents to the auth service in the token mode: the broker's
// BROKER_NATS_K8S_AUDIENCE, on a placeholder.
const clusteredNatsAudience = "example-nats-audience"

// clusteredListenerAt is args' session listener and the path its refusals name: listener where it
// is declared as the hub's own input, and tailnet.listener otherwise.
func clusteredListenerAt(a *gcp.ClusteredNatsHubArgs) (*gcp.ClusteredHubListenerArgs, string) {
	if a.Listener != nil {
		return a.Listener, "listener"
	}
	return a.Tailnet.Listener, "tailnet.listener"
}

// byToken puts l in the token mode and returns it: natsAudience set to clusteredNatsAudience, and
// none of the seed's three inputs.
func byToken(l *gcp.ClusteredHubListenerArgs) *gcp.ClusteredHubListenerArgs {
	l.NKey, l.SeedSecretName, l.SeedSecretKey, l.NatsAudience = "", "", "", clusteredNatsAudience
	return l
}

// tokenClusteredHub is calloutClusteredHub with its session listener declared by placement, an edit
// of clusteredListenerPlacements, in the token mode.
func tokenClusteredHub(placement func(*gcp.ClusteredNatsHubArgs)) *gcp.ClusteredNatsHubArgs {
	args := calloutClusteredHub()
	placement(args)
	l, _ := clusteredListenerAt(args)
	byToken(l)
	return args
}

// The token-mode listener's pod, whole, for clusteredArgs' tailnet.listener behind the auth
// service: the seeded pod of clustered-hub-listener-pod.golden but for its NATS credential, the
// token's file in NATS_NKEY_SEED_FILE's place (NATS_SERVICE_TOKEN_FILE) and the projected token of
// the pod's own ServiceAccount for natsAudience in the seed Secret's volume's (nats-token). It
// prints how many objects the hub rendered and how many containers that pod holds, as the seeded
// pod's golden test does.
func TestAClusteredHubsTokenListenerPodMatchesGolden(t *testing.T) {
	args := clusteredArgs()
	args.Users = append(args.Users, authServiceUser())
	args.AuthCallout = authCallout()
	byToken(args.Tailnet.Listener)
	m := runClusteredHub(t, args)
	requirePodGolden(t, m, clusteredListenerName, "clustered-hub-listener-pod-service-identity.golden")
	containers := clusteredListenerPodSpec(t, m)["containers"].([]any)
	t.Logf("%d objects rendered, %d container(s) in the listener pod", len(m.Registrations()), len(containers))
}

// Wherever the listener is declared (clusteredListenerPlacements), the token mode hands its
// container the projected token's file, /var/run/secrets/envoy/nats-token, in
// NATS_SERVICE_TOKEN_FILE, and no NATS_NKEY_SEED_FILE; its one volume is the projected token of
// the pod's own ServiceAccount for natsAudience, living an hour, mounted read-only at
// /var/run/secrets/envoy, with no Secret and no nats-seed mount; and the ServiceAccount still
// automounts no token, since the listener calls no Kubernetes API.
func TestAClusteredHubsTokenListenerPresentsItsProjectedToken(t *testing.T) {
	for _, placement := range clusteredListenerPlacements() {
		t.Run(placement.name, func(t *testing.T) {
			m := runClusteredHub(t, tokenClusteredHub(placement.edit))
			template := deploymentPodTemplate(t, m, clusteredListenerName)
			listener := podContainer(t, template, "listener")
			env := containerEnv(t, listener)
			require.Equal(t, "/var/run/secrets/envoy/nats-token", env["NATS_SERVICE_TOKEN_FILE"])
			require.NotContains(t, env, "NATS_NKEY_SEED_FILE", "the listener is handed no seed")
			require.Equal(t, []any{map[string]any{"name": "nats-token", "mountPath": "/var/run/secrets/envoy", "readOnly": true}}, listener["volumeMounts"],
				"the token's volume alone is mounted, and no nats-seed")
			require.Equal(t, []any{map[string]any{"name": "nats-token", "projected": map[string]any{"sources": []any{
				map[string]any{"serviceAccountToken": map[string]any{"audience": clusteredNatsAudience, "expirationSeconds": 3600.0, "path": "nats-token"}},
			}}}}, template["spec"].(map[string]any)["volumes"], "one volume, the projected token of the listener's own ServiceAccount, and no Secret")
			require.Equal(t, false, m.Named(t, serviceAccountType, clusteredListenerName).Inputs.Mappable()["automountServiceAccountToken"])
		})
	}
}

// Wherever the listener is declared, the token mode renders no NATS user for it: auth.conf holds
// no user named after its machine id, auth_users lists the keyed users of users alone, the auth
// service's among them, and the whole authorization is the one the same hub renders with no
// listener at all. Its durable is still its own: a user whose listenerMachineId is the listener's
// machine id is refused, naming it, as beside the seeded listener, since two listeners would share
// one durable.
func TestAClusteredHubsTokenListenerHasNoStaticUser(t *testing.T) {
	without, err := gcp.ResolveClusteredNatsHub(calloutClusteredHub())
	require.NoError(t, err)
	for _, placement := range clusteredListenerPlacements() {
		t.Run(placement.name, func(t *testing.T) {
			s, err := gcp.ResolveClusteredNatsHub(tokenClusteredHub(placement.edit))
			require.NoError(t, err)
			require.NotContains(t, s.AuthConf, "    # envoy-hub\n", "auth.conf holds a user for the listener")
			require.Contains(t, s.AuthConf, "  ]\n"+calloutBlock(keyA, keyB, keyC, keyD, keyAuth, keySessions)+"}\n",
				"auth_users lists the keyed users of users alone, the auth service's among them")
			require.Equal(t, without.AuthConf, s.AuthConf, "the token-mode listener adds nothing to the hub's authorization")

			args := tokenClusteredHub(placement.edit)
			args.Users[0].MachineID, args.Users[0].ListenerMachineID = nil, pulumi.StringRef("envoy-hub")
			_, err = gcp.ResolveClusteredNatsHub(args)
			requireInputError(t, err, "users[0].listenerMachineId")
			require.ErrorContains(t, err, "users[0]: listenerMachineId gives the same durable name as the hub's own listener")
		})
	}
}

// The callout's lines hold auth_users, so a hub that moves its listener from the seeded mode to
// the token mode changes the pods' auth-callout annotation, from the digest of a block that lists
// the listener's key to that of one that lists none, and rolls its pods, as every callout change
// does: nats-server refuses to reload one (TestClusteredNatsHubReloadRefusesACalloutChange).
func TestAClusteredHubsTokenListenerRollsTheHubOnTheSwitch(t *testing.T) {
	annotation := func(t *testing.T, args *gcp.ClusteredNatsHubArgs) any {
		t.Helper()
		return podAnnotations(chartValues(t, runClusteredHub(t, args)))["envoy-infra.sjawhar.dev/auth-callout"]
	}
	digest := func(lines string) string {
		sum := sha256.Sum256([]byte(lines))
		return hex.EncodeToString(sum[:])
	}
	for _, placement := range clusteredListenerPlacements() {
		t.Run(placement.name, func(t *testing.T) {
			seeded := calloutClusteredHub()
			placement.edit(seeded)
			before, after := annotation(t, seeded), annotation(t, tokenClusteredHub(placement.edit))
			require.Equal(t, digest(calloutBlock(keyA, keyB, keyC, keyD, keyAuth, keyListener, keySessions)+calloutPingLines), before)
			require.Equal(t, digest(calloutBlock(keyA, keyB, keyC, keyD, keyAuth, keySessions)+calloutPingLines), after)
			require.NotEqual(t, before, after, "the switch leaves the pods' template as it was, so no server restarts with the listener's key out of auth_users")
		})
	}
}

// natsAudience is refused, wherever the listener is declared, as a core.InputError naming
// <path>.natsAudience, and nothing registers: without authCallout, whose auth service alone admits
// a token; beside each of the seed's three inputs, naming it, since a listener connects as one
// identity; padded, or holding a control character, since natsAudience may hold neither anywhere,
// the broker trimming its own audience; and equal to the listener's own oidcAudience or
// oidcIssuer, whose URL a cluster's API server takes as an audience, where it requires a
// credential (the hub's own listener, and a tailnet.listener with clientNamespaces). A machine id
// no BROKER_NATS_SERVICES entry can name, one holding a space, '/' or '=', or the id '-', each of
// which core takes, is refused naming <path>.machineId. Each want holds %[1]s for the listener's
// path.
func TestAClusteredHubsListenerAudienceRefusals(t *testing.T) {
	type (
		A = gcp.ClusteredNatsHubArgs
		L = gcp.ClusteredHubListenerArgs
	)
	rows := []struct {
		name, input string
		edit        func(a *A, l *L)
		want        []string
	}{
		{"without authCallout", "natsAudience", func(a *A, _ *L) { a.AuthCallout = nil },
			[]string{"%[1]s.natsAudience is set without authCallout"}},
		{"beside nkey", "natsAudience", func(_ *A, l *L) { l.NKey = keyListener },
			[]string{"%[1]s.natsAudience is set beside %[1]s.nkey"}},
		{"beside seedSecretName", "natsAudience", func(_ *A, l *L) { l.SeedSecretName = "envoy-hub-listener-seed" },
			[]string{"%[1]s.natsAudience is set beside %[1]s.seedSecretName"}},
		{"beside seedSecretKey", "natsAudience", func(_ *A, l *L) { l.SeedSecretKey = "seed" },
			[]string{"%[1]s.natsAudience is set beside %[1]s.seedSecretKey"}},
		{"padded audience", "natsAudience", func(_ *A, l *L) { l.NatsAudience = " " + clusteredNatsAudience },
			[]string{`%[1]s.natsAudience " example-nats-audience" holds whitespace or a control character`}},
		{"a control character", "natsAudience", func(_ *A, l *L) { l.NatsAudience = "example-nats\x00audience" },
			[]string{`%[1]s.natsAudience "example-nats\x00audience" holds whitespace or a control character`}},
		{"the listener's own oidcAudience", "natsAudience", func(a *A, l *L) {
			if a.Listener == nil {
				// A tailnet.listener requires a caller's token only with clientNamespaces.
				withCredential(l).ClientNamespaces = []string{"legion"}
			}
			l.NatsAudience = l.OidcAudience
		}, []string{"%[1]s.natsAudience is %[1]s.oidcAudience's audience"}},
		{"the listener's own oidcIssuer", "natsAudience", func(a *A, l *L) {
			if a.Listener == nil {
				// A tailnet.listener requires a caller's token only with clientNamespaces.
				withCredential(l).ClientNamespaces = []string{"legion"}
			}
			l.NatsAudience = l.OidcIssuer
		}, []string{"%[1]s.natsAudience is %[1]s.oidcIssuer's URL"}},
		{"a machine id holding a space", "machineId", func(_ *A, l *L) { l.MachineID = pulumi.StringRef("envoy hub") },
			[]string{`%[1]s.machineId "envoy hub" holds a space`}},
		{"a machine id holding a slash", "machineId", func(_ *A, l *L) { l.MachineID = pulumi.StringRef("a/b") },
			[]string{`%[1]s.machineId "a/b" holds a space, '*', '>', '/', '=' or a control character, or is '-', which no BROKER_NATS_SERVICES entry can name`}},
		{"a machine id holding an equals sign", "machineId", func(_ *A, l *L) { l.MachineID = pulumi.StringRef("a=b") },
			[]string{`%[1]s.machineId "a=b" holds a space, '*', '>', '/', '=' or a control character, or is '-', which no BROKER_NATS_SERVICES entry can name`}},
		{"the machine id '-'", "machineId", func(_ *A, l *L) { l.MachineID = pulumi.StringRef("-") },
			[]string{`%[1]s.machineId "-" holds a space, '*', '>', '/', '=' or a control character, or is '-', which no BROKER_NATS_SERVICES entry can name`}},
	}
	for _, placement := range clusteredListenerPlacements() {
		t.Run(placement.name, func(t *testing.T) {
			for _, row := range rows {
				t.Run(row.name, func(t *testing.T) {
					args := tokenClusteredHub(placement.edit)
					l, path := clusteredListenerAt(args)
					row.edit(args, l)
					_, err := gcp.ResolveClusteredNatsHub(args)
					requireInputError(t, err, path+"."+row.input)
					for _, want := range row.want {
						require.ErrorContains(t, err, fmt.Sprintf(want, path))
					}
					m, err := runClustered(t, "example-clustered-hub", args, nil)
					require.ErrorContains(t, err, fmt.Sprintf(row.want[0], path))
					require.Empty(t, m.Registrations(), "nothing registered")
				})
			}
		})
	}
}

// The token mode names no NATS user after the listener's machine id, so a dotted id, which the
// shared machine-id rule takes and the auth service takes in a listener entry, is taken wherever
// the listener is declared: the hub resolves, the listener runs as it, and the hub registers. The
// same id in the seeded mode names the listener's user, which core refuses, naming the listener,
// as TestClusteredNatsHubResolverRefusesInvalidInputs' row of the dotted machine id holds.
func TestAClusteredHubsTokenListenerTakesADottedMachineID(t *testing.T) {
	for _, placement := range clusteredListenerPlacements() {
		t.Run(placement.name, func(t *testing.T) {
			args := tokenClusteredHub(placement.edit)
			l, path := clusteredListenerAt(args)
			l.MachineID = pulumi.StringRef("envoy.hub")
			s, err := gcp.ResolveClusteredNatsHub(args)
			require.NoError(t, err)
			require.Equal(t, "envoy.hub", s.Listener.MachineID)
			require.Contains(t, s.Listener.Env, "ENVOY_MACHINE_ID=envoy.hub")
			m, err := runClustered(t, "example-clustered-hub", args, nil)
			require.NoError(t, err)
			require.Contains(t, m.Types(), clusteredHubType)

			seeded := calloutClusteredHub()
			placement.edit(seeded)
			l, _ = clusteredListenerAt(seeded)
			l.MachineID = pulumi.StringRef("envoy.hub")
			_, err = gcp.ResolveClusteredNatsHub(seeded)
			requireInputError(t, err, path)
			require.ErrorContains(t, err, `listener "envoy.hub": name must match`)
		})
	}
}
