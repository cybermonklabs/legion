package gcp_test

import (
	"path"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// dispatchNatsAudience is the audience each pod's projected ServiceAccount token is minted for
// behind the auth service, the broker's BROKER_NATS_K8S_AUDIENCE.
const dispatchNatsAudience = "example-nats-audience"

// setNatsAudience moves both NATS clients behind the auth service: natsAudience in
// natsSeedSecret's place, since a connection presents one credential.
func setNatsAudience(a *gcp.DispatchArgs) {
	a.NatsSeedSecret = nil
	a.NatsAudience = dispatchNatsAudience
}

// dispatchServiceIdentityArgs is dispatchArgs with both NATS clients behind the auth service.
func dispatchServiceIdentityArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	setNatsAudience(args)
	return args
}

// With natsAudience, each pod carries its own NATS token and no seed, and the listener
// credential is today's: the app presents its account token beside the shared bearer, and the
// listener admits both.
func TestDispatchPodWithServiceIdentityMatchesGolden(t *testing.T) {
	m, err := runDispatch(t, dispatchServiceIdentityArgs(), true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch", "dispatch-pod-with-service-identity.golden")
	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod-with-service-identity.golden")
}

// projectedAudience is the audience of the projected ServiceAccount token at file in container's
// pod: the volume container mounts at file's directory projects exactly one source, a token
// written to file's base name. A file on no such volume fails the test, naming it.
func projectedAudience(t *testing.T, template, container map[string]any, file string) string {
	t.Helper()
	var volumeName string
	for _, item := range container["volumeMounts"].([]any) {
		mount := item.(map[string]any)
		if mount["mountPath"] == path.Dir(file) {
			require.Equal(t, true, mount["readOnly"], "%s is mounted read-only", file)
			volumeName = mount["name"].(string)
		}
	}
	require.NotEmpty(t, volumeName, "no volume of %s is mounted at %s's directory", container["name"], file)
	for _, item := range template["spec"].(map[string]any)["volumes"].([]any) {
		volume := item.(map[string]any)
		if volume["name"] != volumeName {
			continue
		}
		projected, ok := volume["projected"].(map[string]any)
		require.True(t, ok, "%s lies on volume %s, which projects nothing", file, volumeName)
		sources := projected["sources"].([]any)
		require.Len(t, sources, 1, "volume %s projects one token", volumeName)
		token := sources[0].(map[string]any)["serviceAccountToken"].(map[string]any)
		require.Equal(t, path.Base(file), token["path"], "volume %s writes its token to %s", volumeName, file)
		require.Equal(t, 3600.0, token["expirationSeconds"], "the verifiers admit a platform token living at most an hour")
		return token["audience"].(string)
	}
	t.Fatalf("no volume %s in the pod", volumeName)
	return ""
}

// podMountsTheSeed reports whether a pod template has a volume reading the NATS seed's
// SecretProviderClass.
func podMountsTheSeed(template map[string]any) bool {
	for _, item := range template["spec"].(map[string]any)["volumes"].([]any) {
		csi, ok := item.(map[string]any)["csi"].(map[string]any)
		if ok && csi["volumeAttributes"].(map[string]any)["secretProviderClass"] == "example-dispatch-nats-seed" {
			return true
		}
	}
	return false
}

// registered reports whether the component registered a resource of type typ named name.
func registered(m *pulumitest.Mocks, typ, name string) bool {
	for _, r := range m.Registrations() {
		if r.Type == typ && r.Name == name {
			return true
		}
	}
	return false
}

// natsAudience moves the app's and the listener's NATS connections behind the auth service: each
// presents a projected token of its own pod's ServiceAccount for natsAudience, the app's
// <hostname> and the listener's <hostname>-listener, read from the file NATS_SERVICE_TOKEN_FILE
// names, and nothing of the seed is rendered, mounted, classed or granted to either account.
//
// The listener credential moves in no row: in both, the listener admits the shared bearer and the
// app's projected token, and the app presents its token beside the shared bearer, so no update of
// this component switches the credential the listener takes and the one the app presents at once
// (the two Deployments roll apart). Without natsAudience both pods connect by the seed as today.
func TestDispatchRunsByItsServiceIdentity(t *testing.T) {
	listenerBearer := map[string]any{"name": "example-dispatch-listener", "key": "ENVOY_API_TOKEN"}
	for _, row := range []struct {
		name string
		edit func(*gcp.DispatchArgs)
		nats bool
	}{
		{"the seed", func(*gcp.DispatchArgs) {}, false},
		{"natsAudience", setNatsAudience, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchArgs()
			row.edit(args)
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			appTemplate := deploymentPodTemplate(t, m, "example-dispatch")
			appContainer := podContainer(t, appTemplate, "envoy-dispatch")
			app := containerEnv(t, appContainer)
			listenerTemplate := deploymentPodTemplate(t, m, "example-dispatch-listener")
			listenerContainer := podContainer(t, listenerTemplate, "envoy-listener")
			listener := containerEnv(t, listenerContainer)

			for _, pod := range []struct {
				name, account       string
				template, container map[string]any
				env                 map[string]any
			}{
				{"envoy-dispatch", "example-dispatch", appTemplate, appContainer, app},
				{"envoy-listener", "example-dispatch-listener", listenerTemplate, listenerContainer, listener},
			} {
				require.Equal(t, pod.account, pod.template["spec"].(map[string]any)["serviceAccountName"],
					"%s runs as its own account, whose projected token is the one it presents", pod.name)
				require.Equal(t, false, pod.template["spec"].(map[string]any)["automountServiceAccountToken"],
					"%s mounts no default ServiceAccount token: none of its containers reads one", pod.name)
				if !row.nats {
					require.Equal(t, "/var/run/secrets/envoy/nats-seed", pod.env["NATS_NKEY_SEED_FILE"], "%s keeps its seed", pod.name)
					require.NotContains(t, pod.env, "NATS_SERVICE_TOKEN_FILE", pod.name)
					require.True(t, podMountsTheSeed(pod.template), "%s mounts the seed", pod.name)
					continue
				}
				require.NotContains(t, pod.env, "NATS_NKEY_SEED_FILE", "%s connects as one identity, and its bus refuses a seed beside the token", pod.name)
				require.False(t, podMountsTheSeed(pod.template), "%s mounts no seed", pod.name)
				file, ok := pod.env["NATS_SERVICE_TOKEN_FILE"].(string)
				require.True(t, ok, "%s presents a platform token to the auth service", pod.name)
				require.Equal(t, dispatchNatsAudience, projectedAudience(t, pod.template, pod.container, file), "%s's NATS token is for natsAudience", pod.name)
			}
			require.Equal(t, !row.nats, registered(m, "kubernetes:secrets-store.csi.x-k8s.io/v1:SecretProviderClass", "example-dispatch-nats-seed"), "the seed's SecretProviderClass")
			for _, grant := range []string{"example-dispatch-nats-seed-accessor", "example-dispatch-listener-nats-seed-accessor"} {
				require.Equal(t, !row.nats, registered(m, "gcp:secretmanager/secretIamMember:SecretIamMember", grant), "the seed's grant %s", grant)
			}

			require.Equal(t, listenerBearer, listener["ENVOY_API_TOKEN"], "the listener admits the shared bearer")
			require.Equal(t, listenerBearer, app["ENVOY_TOKEN"], "the app keeps the shared bearer beside its token")
			require.True(t, registered(m, "kubernetes:core/v1:Secret", "example-dispatch-listener"))
			audience, ok := listener["ENVOY_OIDC_AUDIENCE"].(string)
			require.True(t, ok, "the listener admits the app's projected token")
			require.Equal(t, azureIssuer, listener["ENVOY_OIDC_ISSUER"], "the listener verifies tokens this cluster minted")
			require.Equal(t, "system:serviceaccount:dispatch:example-dispatch", listener["ENVOY_OIDC_ACCOUNTS"], "of the app's account alone")
			file, ok := app["ENVOY_ACCOUNT_TOKEN_FILE"].(string)
			require.True(t, ok, "the app presents its own token to its listener")
			require.Equal(t, audience, projectedAudience(t, appTemplate, appContainer, file), "the app's listener token is for the listener's audience")
		})
	}
}

// natsAudience is the pods' one NATS credential kind, so natsSeedSecret beside it is refused, and
// without it natsSeedSecret stays required. natsAudience is held to what a projected token's
// audience and an environment variable can carry: no seed, no whitespace, no control character.
// Each refusal names its input, and nothing registers.
func TestDispatchServiceIdentityRefusals(t *testing.T) {
	for _, row := range []struct {
		name, input, err string
		edit             func(*gcp.DispatchArgs)
	}{
		{"a seed secret beside natsAudience", "natsSeedSecret", "natsSeedSecret is set beside natsAudience", func(a *gcp.DispatchArgs) {
			a.NatsAudience = dispatchNatsAudience
		}},
		{"neither", "natsSeedSecret", "natsSeedSecret is required unless natsAudience is set", func(a *gcp.DispatchArgs) {
			a.NatsSeedSecret = nil
		}},
		{"whitespace in natsAudience", "natsAudience", "natsAudience holds whitespace or a control character", func(a *gcp.DispatchArgs) {
			setNatsAudience(a)
			a.NatsAudience = "example nats audience"
		}},
		{"a control character in natsAudience", "natsAudience", "natsAudience holds whitespace or a control character", func(a *gcp.DispatchArgs) {
			setNatsAudience(a)
			a.NatsAudience = "example\x00nats"
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchArgs()
			row.edit(args)
			_, err := gcp.ResolveDispatch(args)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, row.input, input.Input, "%v", err)
			require.ErrorContains(t, err, row.err)
			m, err := runDispatch(t, args, true)
			require.Error(t, err)
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType, tailscaleProviderType}, m.Types(),
				"nothing registered beyond the test's providers")
		})
	}
	// natsUrl stays required with natsAudience: the audience says how the pods connect, not where.
	args := dispatchServiceIdentityArgs()
	args.NatsUrl = nil
	_, err := gcp.ResolveDispatch(args)
	require.ErrorContains(t, err, "natsUrl is required")
}

// A seed in natsAudience is refused before any other refusal, as a seed in every plain input is
// (dispatchSeededInput): here ahead of the missing natsUrl, which the resolver refuses before it
// reads the audience, so the refusal names the seed's input and never the URL.
func TestDispatchRefusesASeedInAnAudienceFirst(t *testing.T) {
	seed, _ := testnats.User(t)
	args := dispatchArgs()
	setNatsAudience(args)
	args.NatsAudience = seed
	args.NatsUrl = nil
	_, err := gcp.ResolveDispatch(args)
	require.EqualError(t, err, "natsAudience holds an nkey seed (not echoed)")
}

// natsAudience is the auth service's, and the NATS token crosses every CONNECT. Equal to another
// audience the app presents a token for or verifies one under, the NATS token would be spendable
// there and a token minted for that one would be a bearer the auth service admits, so natsAudience
// beside agentSecretsAudience (the secrets broker's), the listener's own audience (its Service
// address, in whichever namespace), oidcAudience or the Azure audience is refused, naming both,
// before anything registers. The comparison is of the audiences as their verifiers read them,
// trimmed, so the other one padded with whitespace on either side is refused too; five distinct
// audiences resolve.
func TestDispatchRefusesAnAudienceAnotherAudienceShares(t *testing.T) {
	for _, row := range []struct {
		name, other string
		edit        func(*gcp.DispatchArgs)
	}{
		{"agentSecretsAudience", "agentSecretsAudience", func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl, a.AgentSecretsAudience = "https://example-broker.internal.example", dispatchNatsAudience
		}},
		{"agentSecretsAudience padded", "agentSecretsAudience", func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl, a.AgentSecretsAudience = "https://example-broker.internal.example", " "+dispatchNatsAudience+"\n"
		}},
		{"the listener's own audience", "the listener's own audience, http://example-dispatch-listener.dispatch.svc.cluster.local:9020", func(a *gcp.DispatchArgs) {
			a.NatsAudience = "http://example-dispatch-listener.dispatch.svc.cluster.local:9020"
		}},
		{"the listener's own audience in another namespace", "the listener's own audience, http://example-dispatch-listener.other-ns.svc.cluster.local:9020", func(a *gcp.DispatchArgs) {
			a.Namespace = pulumi.StringRef("other-ns")
			a.NatsAudience = "http://example-dispatch-listener.other-ns.svc.cluster.local:9020"
		}},
		{"oidcAudience", "oidcAudience", func(a *gcp.DispatchArgs) { a.OidcAudience = dispatchNatsAudience }},
		{"oidcAudience padded", "oidcAudience", func(a *gcp.DispatchArgs) { a.OidcAudience = " " + dispatchNatsAudience + "\t" }},
		{"azureFederationAudience", "azureFederationAudience", func(a *gcp.DispatchArgs) {
			setAzureFederation(a)
			a.AzureFederationAudience = pulumi.StringRef(dispatchNatsAudience)
		}},
		{"azureFederationAudience padded", "azureFederationAudience", func(a *gcp.DispatchArgs) {
			setAzureFederation(a)
			a.AzureFederationAudience = pulumi.StringRef("\n" + dispatchNatsAudience + " ")
		}},
		{"the keyless Azure sign-in's default audience", "azureFederationAudience (unset, so api://AzureADTokenExchange)", func(a *gcp.DispatchArgs) {
			setAzureFederation(a)
			a.NatsAudience = "api://AzureADTokenExchange"
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchArgs()
			setNatsAudience(args)
			row.edit(args)
			_, err := gcp.ResolveDispatch(args)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, "natsAudience", input.Input, "%v", err)
			require.ErrorContains(t, err, "natsAudience is also "+row.other+":")
			m, err := runDispatch(t, args, true)
			require.Error(t, err)
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType, tailscaleProviderType}, m.Types(),
				"nothing registered beyond the test's providers")
		})
	}
	args := dispatchServiceIdentityArgs()
	args.AgentSecretsUrl, args.AgentSecretsAudience = "https://example-broker.internal.example", "example-broker-audience"
	args.OidcAudience = dispatchOidcAudience
	setAzureFederation(args)
	_, err := gcp.ResolveDispatch(args)
	require.NoError(t, err, "five distinct audiences, the listener's own among them")
}

// Each audience is rendered as its verifier reads it, trimmed: Dispatch's DISPATCH_OIDC_AUDIENCE,
// and the audiences the kubelet mints the broker's and Entra's projected tokens for, so a padded
// input names the audience it is compared as (TestDispatchRefusesAnAudienceAnotherAudienceShares).
func TestDispatchRendersEachAudienceTrimmed(t *testing.T) {
	args := dispatchArgs()
	args.AgentSecretsUrl, args.AgentSecretsAudience = "https://example-broker.internal.example", " example-broker-audience\t"
	args.OidcAudience = "\n" + dispatchOidcAudience + " "
	setAzureFederation(args)
	args.AzureFederationAudience = pulumi.StringRef("  api://example-audience  ")
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	template := deploymentPodTemplate(t, m, "example-dispatch")
	container := podContainer(t, template, "envoy-dispatch")
	require.Equal(t, dispatchOidcAudience, containerEnv(t, container)["DISPATCH_OIDC_AUDIENCE"])
	require.Equal(t, "example-broker-audience", projectedAudience(t, template, container, "/var/run/secrets/dispatch/agent-secrets-account-token"))
	require.Equal(t, "api://example-audience", projectedAudience(t, template, container, "/var/run/secrets/azure-federation/token"))
}

// An audience input set to whitespace alone names no audience, and is refused by name before
// anything registers, as a blank azureFederationAudience is: trimmed to nothing it would render no
// verifier, so Dispatch would boot with no OIDC caller verification, or with no broker relay, and
// say nothing of why. Dispatch's own boot refused the half pair such an oidcAudience
// rendered before the trim; the component now refuses it first.
func TestDispatchRefusesABlankAudienceByName(t *testing.T) {
	for _, row := range []struct {
		name, input string
		edit        func(*gcp.DispatchArgs)
	}{
		{"oidcAudience alone", "oidcAudience", func(a *gcp.DispatchArgs) { a.OidcAudience = "   " }},
		{"oidcAudience beside oidcIssuer", "oidcAudience", func(a *gcp.DispatchArgs) {
			a.OidcIssuer, a.OidcAudience = "https://issuer.example", "\t\n"
		}},
		{"agentSecretsAudience beside agentSecretsUrl", "agentSecretsAudience", func(a *gcp.DispatchArgs) {
			a.AgentSecretsUrl, a.AgentSecretsAudience = "https://example-broker.internal.example", " "
		}},
		{"agentSecretsAudience alone", "agentSecretsAudience", func(a *gcp.DispatchArgs) { a.AgentSecretsAudience = " \t" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchArgs()
			row.edit(args)
			_, err := gcp.ResolveDispatch(args)
			var input *core.InputError
			require.ErrorAs(t, err, &input, "%v", err)
			require.Equal(t, row.input, input.Input, "%v", err)
			require.ErrorContains(t, err, row.input+" is blank")
			m, err := runDispatch(t, args, true)
			require.Error(t, err)
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType, tailscaleProviderType}, m.Types(),
				"nothing registered beyond the test's providers")
		})
	}
}
