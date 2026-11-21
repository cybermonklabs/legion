package gcp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/natsgrant"
)

const (
	// privateHubURL is Dispatch's own hub, the first of privateHubs, and otherPrivateHubURL a
	// second hub that serves private projects.
	privateHubURL      = "nats://envoy-hub.envoy.svc.cluster.local:4222"
	otherPrivateHubURL = "nats://other-hub.envoy.svc.cluster.local:4222"
	// privacySourceURL is the secrets broker's base URL, the privacy source every listener of a
	// private hub reads in required mode.
	privacySourceURL = "https://envoy-broker.dispatch.svc.cluster.local:8443"
	// The paths gcp:Dispatch projects Dispatch's two tokens at with privateHubs.
	hubAdmissionTokenPath = "/var/run/secrets/dispatch-hub-admission/token"
	readinessTokenPath    = "/var/run/secrets/dispatch-readiness/token"
	// dispatchListenerInstances is the headless Service over Dispatch's own listener's pods, and
	// its in-cluster name.
	dispatchListenerInstances     = "example-dispatch-listener-instances"
	dispatchListenerInstancesHost = dispatchListenerInstances + ".dispatch.svc.cluster.local"
)

// privateListener is one entry of privateListeners for a workload named name serving hub, whose
// listener verifies tokens for the audience https://<name>.example.
func privateListener(name, hub string) gcp.DispatchPrivateListenerArgs {
	host := name + "-instances.envoy.svc.cluster.local"
	return gcp.DispatchPrivateListenerArgs{Name: name, Hub: hub, Source: privacySourceURL, Host: host, Instances: host + ":9020", Audience: "https://" + name + ".example"}
}

// privateHubsArgs is dispatchArgs with two private hubs, Dispatch's own first, a listener workload
// for each, the listener's privacy source, and the service-account verifier the go-live report is
// read through.
func privateHubsArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	args.OidcAudience = dispatchOidcAudience
	args.PrivateHubs = []string{privateHubURL, otherPrivateHubURL}
	args.ListenerPrivacySource = &gcp.DispatchListenerPrivacySourceArgs{BrokerUrl: privacySourceURL}
	args.PrivateListeners = []gcp.DispatchPrivateListenerArgs{
		privateListener("envoy-hub-listener", privateHubURL),
		privateListener("other-hub-listener", otherPrivateHubURL),
	}
	return args
}

// projectedToken answers the audience and the expiry of the projected ServiceAccount token the
// file at path in container lies on, failing when no volume mounted into container holds path or
// the volume projects no such token under path's own name.
func projectedToken(t *testing.T, template, container map[string]any, path string) (audience string, expiration any) {
	t.Helper()
	var volumeName, mountPath string
	for _, item := range container["volumeMounts"].([]any) {
		mount := item.(map[string]any)
		if dir := strings.TrimSuffix(mount["mountPath"].(string), "/") + "/"; strings.HasPrefix(path, dir) {
			volumeName, mountPath = mount["name"].(string), dir
			require.Equal(t, true, mount["readOnly"], "the volume holding %s is mounted read-only", path)
		}
	}
	require.NotEmpty(t, volumeName, "no volume mounted into %s holds %s", container["name"], path)
	for _, item := range template["spec"].(map[string]any)["volumes"].([]any) {
		volume := item.(map[string]any)
		if volume["name"] != volumeName {
			continue
		}
		for _, source := range volume["projected"].(map[string]any)["sources"].([]any) {
			if token, ok := source.(map[string]any)["serviceAccountToken"].(map[string]any); ok && token["path"] == strings.TrimPrefix(path, mountPath) {
				return token["audience"].(string), token["expirationSeconds"]
			}
		}
	}
	t.Fatalf("%s lies on volume %s, which projects no ServiceAccount token at it", path, volumeName)
	return "", nil
}

// registered reports whether the run registered a resource of typ named name.
func registered(m *pulumitest.Mocks, typ, name string) bool {
	for _, r := range m.Registrations() {
		if r.Type == typ && r.Name == name {
			return true
		}
	}
	return false
}

// TestDispatchRendersItsListenerInventory: with privateHubs, gcp:Dispatch mounts in envoy-dispatch
// the hub-admission token (audience natsgrant.HubAdmissionAudience) that DISPATCH_HUB_ADMISSION_TOKEN_FILE
// names, and the token an operator reads the go-live report with (Dispatch's own audience) beside
// DISPATCH_SERVICE_ACCOUNT, Dispatch's own account; it renders DISPATCH_PRIVATE_LISTENERS from each
// entry of privateListeners and Dispatch's own listener for the first hub, whose source is
// listenerPrivacySource's broker; it renders the listener's replicas as a headless Service whose
// selector is the listener Deployment's own pod labels, publishing not-ready addresses; the
// listener's own pod is unchanged, since it requires a bearer and serves a replica's read for the
// credential it presents, whatever its Host; and the listener's NetworkPolicy admits Dispatch's
// app pods at the listener's port. With no privateHubs none of it is rendered. Each input it
// cannot render is refused naming the input. Control: the Service selecting Dispatch's app pods
// (app: example-dispatch); the selector row fails naming the labels.
func TestDispatchRendersItsListenerInventory(t *testing.T) {
	m, err := runDispatch(t, privateHubsArgs(), true)
	require.NoError(t, err)
	template := deploymentPodTemplate(t, m, "example-dispatch")
	app := podContainer(t, template, "envoy-dispatch")
	env := containerEnv(t, app)

	require.Equal(t, hubAdmissionTokenPath, env["DISPATCH_HUB_ADMISSION_TOKEN_FILE"])
	audience, expiration := projectedToken(t, template, app, hubAdmissionTokenPath)
	require.Equal(t, natsgrant.HubAdmissionAudience, audience, "the hub-admission token's audience is the one the auth service admits the proof at")
	require.EqualValues(t, 3600, expiration)
	audience, _ = projectedToken(t, template, app, readinessTokenPath)
	require.Equal(t, dispatchOidcAudience, audience, "the readiness token is for Dispatch's own verifier")
	require.Equal(t, "system:serviceaccount:dispatch:example-dispatch", env["DISPATCH_SERVICE_ACCOUNT"])

	raw, ok := env["DISPATCH_PRIVATE_LISTENERS"].(string)
	require.True(t, ok, "envoy-dispatch carries no DISPATCH_PRIVATE_LISTENERS")
	var listeners []map[string]string
	require.NoError(t, json.Unmarshal([]byte(raw), &listeners), raw)
	require.Equal(t, []map[string]string{
		{"name": "envoy-hub-listener", "hub": privateHubURL, "source": privacySourceURL, "host": "envoy-hub-listener-instances.envoy.svc.cluster.local", "instances": "envoy-hub-listener-instances.envoy.svc.cluster.local:9020", "token_file": "/var/run/secrets/envoy-listener-envoy-hub-listener/token"},
		{"name": "other-hub-listener", "hub": otherPrivateHubURL, "source": privacySourceURL, "host": "other-hub-listener-instances.envoy.svc.cluster.local", "instances": "other-hub-listener-instances.envoy.svc.cluster.local:9020", "token_file": "/var/run/secrets/envoy-listener-other-hub-listener/token"},
		{"name": "example-dispatch-listener", "hub": privateHubURL, "source": privacySourceURL, "host": dispatchListenerInstancesHost, "instances": dispatchListenerInstancesHost + ":9020", "token_file": "/var/run/secrets/envoy-listener/token"},
	}, listeners, "each entry of privateListeners, then Dispatch's own listener for its own hub alone")
	// Each listener verifies a token for its own audience alone, so each entry's file holds
	// Dispatch's own token for that entry's audience. Dispatch's own listener's is the token every
	// call to that listener presents, which the component mounts whatever privateHubs holds.
	for _, name := range []string{"envoy-hub-listener", "other-hub-listener"} {
		audience, expiration := projectedToken(t, template, app, "/var/run/secrets/envoy-listener-"+name+"/token")
		require.Equal(t, "https://"+name+".example", audience, "%s's token is for that listener's audience", name)
		require.EqualValues(t, 3600, expiration)
	}

	listenerTemplate := deploymentPodTemplate(t, m, "example-dispatch-listener")
	labels := listenerTemplate["metadata"].(map[string]any)["labels"]
	spec := m.Named(t, "kubernetes:core/v1:Service", dispatchListenerInstances).Inputs.Mappable()["spec"].(map[string]any)
	require.Equal(t, labels, spec["selector"], "the instances Service selects the listener Deployment's own pods")
	require.Equal(t, "None", spec["clusterIP"], "the instances Service is headless, so its name resolves to every replica")
	require.Equal(t, true, spec["publishNotReadyAddresses"], "a replica that is not ready is read too")
	ports := spec["ports"].([]any)
	require.Len(t, ports, 1)
	require.EqualValues(t, core.DefaultListenerPort, ports[0].(map[string]any)["port"])
	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
	require.Equal(t, []string{"example-dispatch"}, policyIngressPods(t, m, "example-dispatch-listener", core.DefaultListenerPort),
		"the listener's policy admits Dispatch's app pods at the listener's port")

	t.Run("no private hub", func(t *testing.T) {
		m, err := runDispatch(t, dispatchArgs(), true)
		require.NoError(t, err)
		env := containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, "example-dispatch"), "envoy-dispatch"))
		for _, name := range []string{"DISPATCH_HUB_ADMISSION_TOKEN_FILE", "DISPATCH_SERVICE_ACCOUNT", "DISPATCH_PRIVATE_LISTENERS"} {
			requireOptionalEnv(t, env, name, "")
		}
		require.False(t, registered(m, "kubernetes:core/v1:Service", dispatchListenerInstances), "an all-public deployment renders no instances Service")
		requirePodGolden(t, m, "example-dispatch", "dispatch-pod.golden")
		requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
	})

	for _, row := range []struct {
		name, input, naming string
		edit                func(*gcp.DispatchArgs)
	}{
		{"a hub no entry serves", "privateHubs", "no entry of privateListeners serves the hub " + otherPrivateHubURL, func(a *gcp.DispatchArgs) {
			a.PrivateListeners = a.PrivateListeners[:1]
		}},
		{"an entry naming a hub outside privateHubs", "privateListeners[1]", "privateListeners[1] (other-hub-listener) serves the hub " + otherPrivateHubURL + ", which privateHubs does not name", func(a *gcp.DispatchArgs) {
			a.PrivateHubs = a.PrivateHubs[:1]
		}},
		{"an entry without instances", "privateListeners[0]", "privateListeners[0] (envoy-hub-listener) has no instances", func(a *gcp.DispatchArgs) {
			a.PrivateListeners[0].Instances = ""
		}},
		{"instances that are no host:port", "privateListeners[0]", "privateListeners[0] (envoy-hub-listener) has instances that are not host:port", func(a *gcp.DispatchArgs) {
			a.PrivateListeners[0].Instances = "envoy-hub-listener-instances.envoy.svc.cluster.local"
		}},
		{"a blank source", "privateListeners[1]", "privateListeners[1] (other-hub-listener) has no source", func(a *gcp.DispatchArgs) {
			a.PrivateListeners[1].Source = " "
		}},
		{"a source carrying the privacy route", "privateListeners[1]", "privateListeners[1] (other-hub-listener) has a source that is not the secrets broker's base URL", func(a *gcp.DispatchArgs) {
			a.PrivateListeners[1].Source = privacySourceURL + "/v1/project-privacy"
		}},
		{"a blank host", "privateListeners[0]", "privateListeners[0] (envoy-hub-listener) has no host", func(a *gcp.DispatchArgs) {
			a.PrivateListeners[0].Host = ""
		}},
		{"a blank name", "privateListeners[0]", "privateListeners[0] has no name", func(a *gcp.DispatchArgs) {
			a.PrivateListeners[0].Name = ""
		}},
		{"the name of Dispatch's own listener", "privateListeners[1]", "privateListeners[1] (example-dispatch-listener) has the name of", func(a *gcp.DispatchArgs) {
			a.PrivateListeners[1].Name = "example-dispatch-listener"
		}},
		{"a blank audience", "privateListeners[0]", "privateListeners[0] (envoy-hub-listener) has no audience", func(a *gcp.DispatchArgs) {
			a.PrivateListeners[0].Audience = " "
		}},
		{"a name that is no DNS label", "privateListeners[1]", "privateListeners[1] (Other/Hub) has a name that is not a DNS-1123 label", func(a *gcp.DispatchArgs) {
			a.PrivateListeners[1].Name = "Other/Hub"
		}},
		{"the host of another entry", "privateListeners[1]", "privateListeners[1] (other-hub-listener) has the host of privateListeners[0]", func(a *gcp.DispatchArgs) {
			a.PrivateListeners[1].Host = a.PrivateListeners[0].Host
		}},
		{"the host of Dispatch's own listener", "privateListeners[0]", "privateListeners[0] (envoy-hub-listener) has the host of Dispatch's own listener", func(a *gcp.DispatchArgs) {
			a.PrivateListeners[0].Host = dispatchListenerInstancesHost
		}},
		{"privateListeners without privateHubs", "privateListeners[0]", "privateListeners[0] (envoy-hub-listener) serves the hub " + privateHubURL + ", which privateHubs does not name", func(a *gcp.DispatchArgs) {
			a.PrivateHubs, a.ListenerPrivacySource = nil, nil
		}},
		{"privateHubs without listenerPrivacySource", "listenerPrivacySource", "listenerPrivacySource is required when privateHubs is set", func(a *gcp.DispatchArgs) {
			a.ListenerPrivacySource = nil
		}},
		{"a blank broker URL", "listenerPrivacySource", "listenerPrivacySource.brokerUrl is blank", func(a *gcp.DispatchArgs) {
			a.ListenerPrivacySource.BrokerUrl = " "
		}},
		{"a broker URL carrying the privacy route", "listenerPrivacySource", "listenerPrivacySource.brokerUrl is not the secrets broker's base URL", func(a *gcp.DispatchArgs) {
			a.ListenerPrivacySource.BrokerUrl = privacySourceURL + "/v1/project-privacy"
		}},
		{"a broker URL that is no URL", "listenerPrivacySource", "listenerPrivacySource.brokerUrl is not the secrets broker's base URL", func(a *gcp.DispatchArgs) {
			a.ListenerPrivacySource.BrokerUrl = "envoy-broker.dispatch.svc.cluster.local"
		}},
		{"listenerPrivacySource without privateHubs", "listenerPrivacySource", "listenerPrivacySource is set and privateHubs is not", func(a *gcp.DispatchArgs) {
			a.PrivateHubs, a.PrivateListeners = nil, nil
		}},
		{"privateHubs without oidcAudience", "oidcAudience", "oidcAudience is required when privateHubs is set", func(a *gcp.DispatchArgs) {
			a.OidcAudience = ""
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := privateHubsArgs()
			row.edit(args)
			_, err := gcp.ResolveDispatch(args)
			require.ErrorContains(t, err, row.naming)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, row.input, input.Input, "%v", err)
		})
	}
}
