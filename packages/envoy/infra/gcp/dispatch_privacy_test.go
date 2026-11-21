package gcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
)

// The example broker the private listener reads privacy from, and the audience it verifies.
const (
	dispatchPrivacyBrokerURL      = "https://example-broker.internal.example"
	dispatchPrivacyBrokerAudience = "example-broker-audience"
)

// setListenerPrivacySource puts Dispatch's listener in the required privacy mode, beside
// natsAudience, which that mode needs.
func setListenerPrivacySource(a *gcp.DispatchArgs) {
	setNatsAudience(a)
	a.ListenerPrivacySource = &gcp.DispatchListenerPrivacySourceArgs{BrokerUrl: dispatchPrivacyBrokerURL, BrokerAudience: dispatchPrivacyBrokerAudience}
}

// dispatchPrivateListenerArgs is dispatchArgs with Dispatch's listener in the required privacy mode.
func dispatchPrivateListenerArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	setListenerPrivacySource(args)
	return args
}

// Without listenerPrivacySource the listener's pod is today's, with the seed and with natsAudience
// (the goldens TestDispatchListenerPodMatchesGolden and TestDispatchPodWithServiceIdentityMatchesGolden
// hold). With it the listener runs in the required privacy mode, as its own account
// <hostname>-listener: its NATS token is that account's projected natsAudience token, it mounts that
// account's projected token for the broker's audience at /var/run/secrets/envoy-broker/token and no
// CSI volume, and its environment holds the privacy URL (the broker's base URL), the flag, the four
// broker settings with its own Service address as its origin, and ENVOY_ALL_PROJECTS_ACCOUNTS naming
// Dispatch's account, beside the verifier of Dispatch's token and the shared bearer, as the golden
// dispatch-listener-pod-private.golden records. projectPrivacyRequired renders
// DISPATCH_PROJECT_PRIVACY_REQUIRED=true on the app; unset, the app's environment is today's.
func TestDispatchsListenerFollowsItsPrivacySource(t *testing.T) {
	for golden, args := range map[string]func() *gcp.DispatchArgs{
		"dispatch-listener-pod.golden":                       dispatchArgs,
		"dispatch-listener-pod-with-service-identity.golden": dispatchServiceIdentityArgs,
	} {
		t.Run("without it: "+golden, func(t *testing.T) {
			m, err := runDispatch(t, args(), true)
			require.NoError(t, err)
			requirePodGolden(t, m, "example-dispatch-listener", golden)
		})
	}

	m, err := runDispatch(t, dispatchPrivateListenerArgs(), true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod-private.golden")
	template := deploymentPodTemplate(t, m, "example-dispatch-listener")
	container := podContainer(t, template, "envoy-listener")
	env := containerEnv(t, container)
	require.Equal(t, "example-dispatch-listener", template["spec"].(map[string]any)["serviceAccountName"], "example-dispatch-listener: the listener runs as its own account")
	require.Equal(t, dispatchNatsAudience, projectedAudience(t, template, container, "/var/run/secrets/envoy/nats-token"), "example-dispatch-listener: its NATS token")
	require.Equal(t, dispatchPrivacyBrokerAudience, projectedAudience(t, template, container, "/var/run/secrets/envoy-broker/token"), "example-dispatch-listener: its broker token")
	require.False(t, podMountsTheSeed(template), "example-dispatch-listener: no CSI volume")
	origin := "http://example-dispatch-listener.dispatch.svc.cluster.local:9020"
	for name, want := range map[string]any{
		"ENVOY_PROJECT_PRIVACY_URL":              dispatchPrivacyBrokerURL,
		"ENVOY_PROJECT_PRIVACY_REQUIRED":         "true",
		"ENVOY_ALL_PROJECTS_ACCOUNTS":            "system:serviceaccount:dispatch:example-dispatch",
		"ENVOY_AGENT_SECRETS_URL":                dispatchPrivacyBrokerURL,
		"ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": "/var/run/secrets/envoy-broker/token",
		"ENVOY_PUBLIC_URL":                       origin,
		"ENVOY_OIDC_AUDIENCE":                    origin,
		"NATS_SERVICE_TOKEN_FILE":                "/var/run/secrets/envoy/nats-token",
	} {
		require.Equal(t, want, env[name], "example-dispatch-listener: %s", name)
	}
	require.NotContains(t, env, "ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE", "example-dispatch-listener: no Google audience beside the token file")
	require.NotContains(t, containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, "example-dispatch"), "envoy-dispatch")), "DISPATCH_PROJECT_PRIVACY_REQUIRED", "projectPrivacyRequired unset")

	required := dispatchPrivateListenerArgs()
	required.ProjectPrivacyRequired = true
	m, err = runDispatch(t, required, true)
	require.NoError(t, err)
	require.Equal(t, "true", containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, "example-dispatch"), "envoy-dispatch"))["DISPATCH_PROJECT_PRIVACY_REQUIRED"])
}

// Each refusal of the required privacy mode names its input, before anything registers.
func TestDispatchRefusesAnIncompletePrivacySource(t *testing.T) {
	for _, row := range []struct {
		name, input, err string
		edit             func(*gcp.DispatchArgs)
	}{
		{"privateHubs without listenerPrivacySource", "listenerPrivacySource", "privateHubs names hubs", func(a *gcp.DispatchArgs) {
			a.PrivateHubs = []string{"nats://hub:4222"}
		}},
		{"listenerPrivacySource without natsAudience", "listenerPrivacySource", "without natsAudience", func(a *gcp.DispatchArgs) {
			a.ListenerPrivacySource = &gcp.DispatchListenerPrivacySourceArgs{BrokerUrl: dispatchPrivacyBrokerURL, BrokerAudience: dispatchPrivacyBrokerAudience}
		}},
		{"projectPrivacyRequired without listenerPrivacySource", "projectPrivacyRequired", "without listenerPrivacySource", func(a *gcp.DispatchArgs) {
			a.ProjectPrivacyRequired = true
		}},
		{"a blank brokerUrl", "listenerPrivacySource.brokerUrl", "is blank", func(a *gcp.DispatchArgs) {
			setListenerPrivacySource(a)
			a.ListenerPrivacySource.BrokerUrl = " "
		}},
		{"a blank brokerAudience", "listenerPrivacySource.brokerAudience", "is blank", func(a *gcp.DispatchArgs) {
			setListenerPrivacySource(a)
			a.ListenerPrivacySource.BrokerAudience = "\t"
		}},
		{"a brokerUrl with a path", "listenerPrivacySource.brokerUrl", "base URL", func(a *gcp.DispatchArgs) {
			setListenerPrivacySource(a)
			a.ListenerPrivacySource.BrokerUrl = dispatchPrivacyBrokerURL + "/v1/project-privacy"
		}},
		{"an http brokerUrl", "listenerPrivacySource.brokerUrl", "base URL", func(a *gcp.DispatchArgs) {
			setListenerPrivacySource(a)
			a.ListenerPrivacySource.BrokerUrl = "http://example-broker.internal.example"
		}},
		{"whitespace in brokerAudience", "listenerPrivacySource.brokerAudience", "whitespace", func(a *gcp.DispatchArgs) {
			setListenerPrivacySource(a)
			a.ListenerPrivacySource.BrokerAudience = "example broker audience"
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchArgs()
			row.edit(args)
			_, err := gcp.ResolveDispatch(args)
			var input *core.InputError
			require.ErrorAs(t, err, &input, "%v", err)
			require.Equal(t, row.input, input.Input, "%v", err)
			require.ErrorContains(t, err, row.err)
			m, err := runDispatch(t, args, true)
			require.Error(t, err)
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType, tailscaleProviderType}, m.Types(),
				"nothing registered beyond the test's providers")
		})
	}
}
