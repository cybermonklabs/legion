package gcp_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

const (
	// dispatchNatsAudience is the audience of the projected ServiceAccount token each of
	// Dispatch's pods presents to the NATS auth service with natsAudience: the broker's
	// BROKER_NATS_K8S_AUDIENCE, on a placeholder.
	dispatchNatsAudience = "example-nats-audience"
	// dispatchServiceIdentityGolden is the app's pod with natsAudience.
	dispatchServiceIdentityGolden = "dispatch-pod-with-service-identity.golden"
	// secretProviderClassType is the type a SecretProviderClass registers under.
	secretProviderClassType = "kubernetes:secrets-store.csi.x-k8s.io/v1:SecretProviderClass"
)

// dispatchServiceIdentityArgs is dispatchArgs with both NATS clients behind the auth service:
// natsAudience in natsSeedSecret's place, since a connection presents one credential.
func dispatchServiceIdentityArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	args.NatsSeedSecret = nil
	args.NatsAudience = dispatchNatsAudience
	return args
}

// The app's pod with natsAudience, whole: the pod of dispatch-pod.golden but for its NATS
// credential, the token's file in NATS_NKEY_SEED_FILE's place (NATS_SERVICE_TOKEN_FILE) and the
// projected token of the pod's own ServiceAccount for natsAudience in the seed's CSI volume's place
// (nats-token), mounted where the seed was.
func TestDispatchPodWithServiceIdentityMatchesGolden(t *testing.T) {
	m, err := runDispatch(t, dispatchServiceIdentityArgs(), true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch", dispatchServiceIdentityGolden)
}

// The listener's pod with natsAudience, whole: the pod of dispatch-listener-pod.golden but for the
// same three places.
func TestDispatchListenerPodWithServiceIdentityMatchesGolden(t *testing.T) {
	m, err := runDispatch(t, dispatchServiceIdentityArgs(), true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod-with-service-identity.golden")
}

// registered reports whether the component registered a resource of type typ named name.
func registered(m *pulumitest.Mocks, typ, name string) bool {
	return slices.Contains(registrationNames(registrationsOf(m, typ)), name)
}

// podVolumes indexes a pod template's volumes by name.
func podVolumes(t *testing.T, template map[string]any) map[string]any {
	t.Helper()
	volumes := map[string]any{}
	for _, item := range template["spec"].(map[string]any)["volumes"].([]any) {
		volume := item.(map[string]any)
		volumes[volume["name"].(string)] = volume
	}
	return volumes
}

// With natsAudience the app's envoy-dispatch and the listener's envoy-listener each present a
// projected token of their own pod's ServiceAccount for natsAudience, the app's <hostname> and the
// listener's <hostname>-listener: the token's file in NATS_SERVICE_TOKEN_FILE and no
// NATS_NKEY_SEED_FILE, the projected nats-token volume, living an hour, mounted read-only at
// /var/run/secrets/envoy, and no nats-seed volume or mount. Neither account gets any access to a
// seed: none of the five resources a seeded Dispatch registers for its seed
// (dispatch-registrations.golden), its SecretProviderClass and its four grants, by name and by
// uid, registers, no registered name holds nats-seed, and neither Deployment waits on a grant. The
// listener's uid, which only its seed grant by uid read, is not read, so its account answering
// none fails nothing; the app's still is, as the serviceAccountUid output. The natsSeedSecret row
// is the control: the same checks find the seed, its five resources, the Deployments' two grants
// each, and the refusal of the listener's account with no uid.
func TestDispatchRunsByItsServiceIdentity(t *testing.T) {
	for _, row := range []struct {
		name  string
		args  func() *gcp.DispatchArgs
		token bool
	}{
		{"natsSeedSecret", dispatchArgs, false},
		{"natsAudience", dispatchServiceIdentityArgs, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			for _, pod := range []struct{ deployment, container string }{
				{"example-dispatch", "envoy-dispatch"},
				{"example-dispatch-listener", "envoy-listener"},
			} {
				template := deploymentPodTemplate(t, m, pod.deployment)
				container := podContainer(t, template, pod.container)
				env := containerEnv(t, container)
				volumes := podVolumes(t, template)
				var mounts []string
				for _, item := range container["volumeMounts"].([]any) {
					mounts = append(mounts, item.(map[string]any)["name"].(string))
				}
				if !row.token {
					require.Equal(t, "/var/run/secrets/envoy/nats-seed", env["NATS_NKEY_SEED_FILE"], "control: %s is handed the seed", pod.container)
					require.Contains(t, volumes, "nats-seed", "control: %s's pod mounts the seed", pod.container)
					require.Contains(t, mounts, "nats-seed", "control: %s mounts the seed", pod.container)
					continue
				}
				require.Equal(t, "/var/run/secrets/envoy/nats-token", env["NATS_SERVICE_TOKEN_FILE"], "%s presents its projected token", pod.container)
				require.NotContains(t, env, "NATS_NKEY_SEED_FILE", "%s connects as one identity, and its bus refuses a seed beside the token", pod.container)
				require.NotContains(t, volumes, "nats-seed", "%s's pod mounts no seed", pod.container)
				require.NotContains(t, mounts, "nats-seed", "%s mounts no seed", pod.container)
				require.Equal(t, map[string]any{"name": "nats-token", "projected": map[string]any{"sources": []any{
					map[string]any{"serviceAccountToken": map[string]any{"audience": dispatchNatsAudience, "expirationSeconds": 3600.0, "path": "nats-token"}},
				}}}, volumes["nats-token"], "%s's pod projects its own account's token for natsAudience, living an hour", pod.container)
				require.Contains(t, container["volumeMounts"], map[string]any{"name": "nats-token", "mountPath": "/var/run/secrets/envoy", "readOnly": true},
					"%s mounts the token read-only where NATS_SERVICE_TOKEN_FILE names it", pod.container)
			}

			// The five resources of the seed, each by its name, and the catch-all over every name.
			seed := []struct{ typ, name string }{{secretProviderClassType, "example-dispatch-nats-seed"}}
			for _, grant := range dispatchSeedAccessors {
				seed = append(seed, struct{ typ, name string }{accessorType, grant})
			}
			for _, resource := range seed {
				require.Equal(t, !row.token, registered(m, resource.typ, resource.name), "%s %s registered", resource.typ, resource.name)
			}
			var seedNames []string
			for _, name := range registrationNames(m.Registrations()) {
				if strings.Contains(name, "nats-seed") {
					seedNames = append(seedNames, name)
				}
			}
			if row.token {
				require.Empty(t, seedNames, "no registered name holds nats-seed")
			} else {
				require.Len(t, seedNames, len(seed), "control: the seed's resources are these five alone")
			}

			for _, deployment := range []string{"example-dispatch", "example-dispatch-listener"} {
				var grants []string
				for _, dependency := range m.Named(t, "kubernetes:apps/v1:Deployment", deployment).RPC.GetDependencies() {
					if strings.Contains(dependency, "$"+accessorType+"::") {
						grants = append(grants, dependency)
					}
				}
				if row.token {
					require.Empty(t, grants, "%s waits on no grant: its pods read no secret", deployment)
				} else {
					require.Len(t, grants, 2, "control: %s waits on its own account's two seed grants", deployment)
				}
			}

			// The listener's uid is read for its seed grant by uid alone.
			noUID := withServiceAccountsAnsweringNoUID(newMocks(), "example-dispatch-listener")
			err = runDispatchOnMocks(t, noUID, "example-dispatch", row.args(), newGCPProvider, newRandomProvider, newTailscaleProvider)
			if row.token {
				require.NoError(t, err, "nothing reads the listener's uid in the token mode")
			} else {
				require.ErrorContains(t, err, "ServiceAccount example-dispatch-listener has no uid", "control: the seeded mode's grant by uid reads it")
			}
		})
	}

	// Over the plugin's Construct, the path a consumer's program takes, natsAudience arrives as a
	// plain string, and the app's account's uid is still the serviceAccountUid output.
	inputs := dispatchConstructBaseInputs()
	delete(inputs, "natsSeedSecret")
	inputs["natsAudience"] = property.New(dispatchNatsAudience)
	m, response, err := constructDispatch(t, inputs, false)
	require.NoError(t, err)
	require.False(t, registered(m, secretProviderClassType, "example-dispatch-nats-seed"), "the Construct path classes no seed either")
	uid, ok := response.State.GetOk("serviceAccountUid")
	require.True(t, ok, "serviceAccountUid is in the state")
	require.Equal(t, mockServiceAccountUID("example-dispatch"), uid.AsString(),
		"serviceAccountUid is the uid of ServiceAccount example-dispatch, the one the app's pods run as")
}

// natsAudience is refused, as a core.InputError naming it, and nothing registers: beside
// natsSeedSecret, since each connection presents one credential and the bus refuses a token beside
// a seed; and padded, or holding a control character, since the broker reads its own audience
// trimmed, so a token minted for such an audience would never verify. Neither of the two set is
// natsSeedSecret's refusal (TestDispatchResolverRefusesInvalidInputs' row "no NATS seed secret"),
// a seed in natsAudience is refused with every other plain input's
// (TestDispatchRefusesAnNKeySeedInAPlainInput), and natsUrl stays required beside natsAudience:
// the audience says how the pods connect, not where.
func TestDispatchServiceIdentityRefusals(t *testing.T) {
	for _, row := range []struct {
		name, want string
		edit       func(*gcp.DispatchArgs)
	}{
		{"beside natsSeedSecret", "natsAudience is set beside natsSeedSecret", func(a *gcp.DispatchArgs) {
			a.NatsSeedSecret = pulumi.String("projects/example-project/secrets/example-nats-seed")
		}},
		{"padded", `natsAudience " example-nats-audience" holds whitespace or a control character`, func(a *gcp.DispatchArgs) {
			a.NatsAudience = " " + dispatchNatsAudience
		}},
		{"a control character", `natsAudience "example-nats\x00audience" holds whitespace or a control character`, func(a *gcp.DispatchArgs) {
			a.NatsAudience = "example-nats\x00audience"
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchServiceIdentityArgs()
			row.edit(args)
			_, err := gcp.ResolveDispatch(args)
			requireInputError(t, err, "natsAudience")
			require.ErrorContains(t, err, row.want)
			m, err := runDispatch(t, args, true)
			require.ErrorContains(t, err, row.want)
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType, tailscaleProviderType}, m.Types(),
				"nothing registered beyond the test's providers")
		})
	}
	args := dispatchServiceIdentityArgs()
	args.NatsUrl = nil
	_, err := gcp.ResolveDispatch(args)
	requireInputError(t, err, "natsUrl")
}

// natsAudience is the NATS auth service's, and the NATS token crosses every CONNECT. Equal to
// another audience the app presents a token for or a pod verifies one under, the NATS token would
// be spendable there and a token minted for that one would be a bearer the auth service admits,
// so natsAudience equal to agentSecretsAudience (the secrets broker's), the listener's own
// audience (its Service address, in whichever namespace), oidcAudience, azureFederationAudience,
// or the keyless Azure sign-in's default audience while azureFederationAudience is unset, is
// refused as a core.InputError naming natsAudience, its message naming the other, and nothing
// registers. Five distinct audiences resolve.
func TestDispatchRefusesAnAudienceAnotherAudienceShares(t *testing.T) {
	for _, row := range []struct {
		name, other string
		edit        func(*gcp.DispatchArgs)
	}{
		{"agentSecretsAudience", "agentSecretsAudience", func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl, a.AgentSecretsAudience = "https://example-broker.internal.example", dispatchNatsAudience
		}},
		{"the listener's own audience", "the listener's own audience, http://example-dispatch-listener.dispatch.svc.cluster.local:9020", func(a *gcp.DispatchArgs) {
			a.NatsAudience = "http://example-dispatch-listener.dispatch.svc.cluster.local:9020"
		}},
		{"the listener's own audience in another namespace", "the listener's own audience, http://example-dispatch-listener.example-namespace.svc.cluster.local:9020", func(a *gcp.DispatchArgs) {
			a.Namespace = pulumi.StringRef("example-namespace")
			a.NatsAudience = "http://example-dispatch-listener.example-namespace.svc.cluster.local:9020"
		}},
		{"oidcAudience", "oidcAudience", func(a *gcp.DispatchArgs) { a.OidcAudience = dispatchNatsAudience }},
		{"azureFederationAudience", "azureFederationAudience", func(a *gcp.DispatchArgs) {
			setAzureFederation(a)
			a.AzureFederationAudience = pulumi.StringRef(dispatchNatsAudience)
		}},
		{"the keyless Azure sign-in's default audience", "azureFederationAudience (unset, so api://AzureADTokenExchange)", func(a *gcp.DispatchArgs) {
			setAzureFederation(a)
			a.NatsAudience = "api://AzureADTokenExchange"
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchServiceIdentityArgs()
			row.edit(args)
			_, err := gcp.ResolveDispatch(args)
			requireInputError(t, err, "natsAudience")
			require.ErrorContains(t, err, "natsAudience is also "+row.other+":")
			m, err := runDispatch(t, args, true)
			require.ErrorContains(t, err, "natsAudience is also "+row.other+":")
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType, tailscaleProviderType}, m.Types(),
				"nothing registered beyond the test's providers")
		})
	}
	args := dispatchServiceIdentityArgs()
	args.AgentSecretsUrl, args.AgentSecretsAudience = "https://example-broker.internal.example", "example-broker-audience"
	args.OidcAudience = dispatchOidcAudience
	setAzureFederation(args)
	_, err := gcp.ResolveDispatch(args)
	require.NoError(t, err, "five distinct audiences, the listener's own and the Azure default among them")
}
