package gcp_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
)

// The inputs a private hub's session listener reads each decision's privacy with: the secrets
// broker's base URL, which is the privacy source too (the listener appends the privacy route to
// it), the broker's audience (BROKER_K8S_OIDC_AUDIENCE), which the projected token of the
// listener's own account carries, the listener's own origin, and Dispatch's account, the one
// caller served every project's sessions.
const (
	privacyBrokerURL      = "https://example-broker.example-dispatch.svc.cluster.local:8443"
	privacySourceURL      = privacyBrokerURL
	privacyBrokerAudience = "example-broker-audience"
	// privacyTailnetOrigin is the origin the tailnet's sessions reach clusteredArgs' session
	// listener at, the Tailscale Service's name, which a private listener with tailnet checks
	// every DPoP proof against; privacyPublicURL is its cluster Service's, which is its origin only
	// without tailnet.
	privacyTailnetOrigin   = "http://envoy-hub.example-tailnet.ts.net:9020"
	privacyPublicURL       = "http://envoy-nats-listener.envoy.svc.cluster.local:9020"
	privacyDispatchAccount = "system:serviceaccount:example-dispatch:example-dispatch"
	// clusteredListenerInstances is the headless Service over the session listener's replicas.
	clusteredListenerInstances = clusteredListenerName + "-instances"
)

// withPrivacySource gives l the inputs a private hub's listener reads privacy with, and lists
// Dispatch's account among the subjects its token check admits, since an all-projects caller is
// first a caller the listener verifies. It sets no publicUrl: with tailnet the hub takes the
// tailnet's origin. It returns l.
func withPrivacySource(l *gcp.ClusteredHubListenerArgs) *gcp.ClusteredHubListenerArgs {
	l.AgentSecretsUrl, l.BrokerAudience = privacyBrokerURL, privacyBrokerAudience
	l.PrivacySourceUrl, l.AllProjectsAccounts = privacySourceURL, []string{privacyDispatchAccount}
	l.OidcAccounts = append(l.OidcAccounts, privacyDispatchAccount)
	return l
}

// privateModeFloor is the listener release the tests take as the first that enforces the required
// mode (gcp.SetPrivateModeListenerFloor), and privateListenerImage a listener image whose tag names
// it: the component names no such release yet, so without the seam every private listener is
// refused.
const privateModeFloor = "99.0.0"

var privateListenerImage = strings.Replace(listenerImage, "@sha256:", ":v"+privateModeFloor+"@sha256:", 1)

// privateListenerHub is a gcp:ClusteredNatsHub that may make a project private and runs a session
// listener: the auth service's user alone behind the callout, no session user, and the listener
// declared as the hub's own input, so it requires a token of every caller, connecting by its own
// account's projected token, with the privacy inputs and an image that names privateModeFloor. A
// test that resolves it sets the floor first (gcp.SetPrivateModeListenerFloor).
func privateListenerHub() *gcp.ClusteredNatsHubArgs {
	args := calloutClusteredHubWithTailnet()
	args.Users, args.SessionUser = []gcp.NATSUserArgs{authServiceUser()}, nil
	args.PrivateProjects = true
	withPrivacySource(ownListener(args)).Image = privateListenerImage
	return args
}

// TestAPrivateHubsListenerNeedsTheRequiredModeRelease: with privateProjects the session listener
// renders the privacy settings, which hold each broker-token caller to the projects it may see only
// in a listener release that enforces the required mode. So the hub refuses a private session
// listener unless its image names a release at or after privateModeListenerFloor; with no floor
// named, as today, it refuses every image, naming privateProjects, and registers nothing.
//
// Control: the empty-floor refusal removed; the first row's hub resolves, and the test fails.
func TestAPrivateHubsListenerNeedsTheRequiredModeRelease(t *testing.T) {
	gcp.SetPrivateModeListenerFloor(t, "")
	_, err := gcp.ResolveClusteredNatsHub(privateListenerHub())
	require.ErrorContains(t, err, "privateProjects needs a session listener release that enforces the required mode; none is named yet")
	var input *core.InputError
	require.ErrorAs(t, err, &input)
	require.Equal(t, "privateProjects", input.Input)
	m, err := runClustered(t, "example-clustered-hub", privateListenerHub(), nil)
	require.ErrorContains(t, err, "none is named yet")
	require.Empty(t, m.Registrations(), "nothing registered")

	gcp.SetPrivateModeListenerFloor(t, privateModeFloor)
	for _, row := range []struct {
		name, image string
		ok          bool
	}{
		{"the floor's own release", privateListenerImage, true},
		{"a later release, as its name", strings.Replace(listenerImage, "@sha256:", ":legion-envoy-v99.1.0@sha256:", 1), true},
		{"an earlier release", strings.Replace(listenerImage, "@sha256:", ":v98.9.9@sha256:", 1), false},
		{"no release in the tag", listenerImage, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := privateListenerHub()
			args.Listener.Image = row.image
			_, err := gcp.ResolveClusteredNatsHub(args)
			if row.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "listener.image "+strconv.Quote(row.image)+" must name, as its tag, a listener release at or after v"+privateModeFloor)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, "listener.image", input.Input)
		})
	}
}

// privateTailnetListenerHub is privateListenerHub with its session listener declared as the
// tailnet entry's, with no clientNamespaces: the listener the tailnet hosts alone reach, which an
// all-public hub runs with its broker settings alone. It keeps the token check, which the private
// mode needs for Dispatch's principal.
func privateTailnetListenerHub() *gcp.ClusteredNatsHubArgs {
	args := privateListenerHub()
	args.Listener, args.Tailnet.Listener = nil, args.Listener
	return args
}

// TestAPrivateTailnetListenerHasNoOpenV1: on a hub with privateProjects, the tailnet-only session
// listener checks the token a caller presents, its broker token or Dispatch's projected token, and
// serves a caller that presents nothing 401, as every session listener does from t-e-19 on: it
// renders its token check (ENVOY_OIDC_*), which is how its readiness route knows Dispatch's own
// account, beside its broker settings, and no ENVOY_V1_OPEN_WITH_TOKEN_CHECKS, ENVOY_ALLOWED_HOSTS
// or ENVOY_API_ALLOW_UNAUTHENTICATED. With tailnet every session listener renders the per-address
// introspection limit sized for the tailnet hosts' shared addresses (50 a second, a burst of 200);
// without tailnet it renders none.
//
// Controls, each run by hand: the open setting rendered again for a tailnet-only listener (the
// test fails, naming ENVOY_V1_OPEN_WITH_TOKEN_CHECKS); the limit dropped (it fails naming
// ENVOY_AGENT_SECRETS_INTROSPECTIONS_PER_SECOND).
func TestAPrivateTailnetListenerHasNoOpenV1(t *testing.T) {
	gcp.SetPrivateModeListenerFloor(t, privateModeFloor)
	envOf := func(args *gcp.ClusteredNatsHubArgs) map[string]any {
		t.Helper()
		return containerEnv(t, podContainer(t, deploymentPodTemplate(t, runClusteredHub(t, args), clusteredListenerName), "listener"))
	}

	m := runClusteredHub(t, privateTailnetListenerHub())
	requirePodGolden(t, m, clusteredListenerName, "clustered-hub-tailnet-listener-pod-private.golden")
	env := containerEnv(t, podContainer(t, deploymentPodTemplate(t, m, clusteredListenerName), "listener"))
	for name, want := range map[string]any{
		"ENVOY_OIDC_ISSUER":                             clusteredListenerIssuer,
		"ENVOY_OIDC_ACCOUNTS":                           clusteredListenerAccount + "," + privacyDispatchAccount,
		"ENVOY_AGENT_SECRETS_URL":                       privacyBrokerURL,
		"ENVOY_AGENT_SECRETS_INTROSPECTIONS_PER_SECOND": "50",
		"ENVOY_AGENT_SECRETS_INTROSPECTION_BURST":       "200",
		"ENVOY_PUBLIC_URL":                              privacyTailnetOrigin,
	} {
		require.Equal(t, want, env[name], "the private tailnet-only listener renders %s", name)
	}
	for _, name := range []string{"ENVOY_V1_OPEN_WITH_TOKEN_CHECKS", "ENVOY_ALLOWED_HOSTS", "ENVOY_API_ALLOW_UNAUTHENTICATED"} {
		require.NotContains(t, env, name, "the private tailnet-only listener renders %s", name)
	}

	own := envOf(privateListenerHub())
	require.NotContains(t, own, "ENVOY_V1_OPEN_WITH_TOKEN_CHECKS", "the hub's own listener requires a token of every caller, in either mode")
	require.NotContains(t, own, "ENVOY_ALLOWED_HOSTS")
	require.Equal(t, "50", own["ENVOY_AGENT_SECRETS_INTROSPECTIONS_PER_SECOND"], "with tailnet the limit is the tailnet hosts'")

	standalone := privateListenerHub()
	standaloneListener(standalone).PublicUrl = privacyPublicURL
	s, err := gcp.ResolveClusteredNatsHub(standalone)
	require.NoError(t, err)
	for _, line := range s.Listener.Env {
		require.NotContains(t, line, "ENVOY_AGENT_SECRETS_INTROSPECTION", "without tailnet no forwarder shares an address, so the listener keeps its default limit")
		require.NotContains(t, line, "ENVOY_V1_OPEN_WITH_TOKEN_CHECKS")
	}

	// On an all-public hub the tailnet-only listener takes no token check at all, its broker
	// settings alone.
	public := privateTailnetListenerHub()
	public.PrivateProjects = false
	_, err = gcp.ResolveClusteredNatsHub(public)
	require.ErrorContains(t, err, "tailnet.listener.oidcIssuer is given on a tailnet.listener with no clientNamespaces, which the tailnet hosts alone reach and which takes their callers' broker tokens")
}

// podJSON is the listener's pod template as the mocks recorded it, for a byte-for-byte comparison.
func podJSON(t *testing.T, args *gcp.ClusteredNatsHubArgs) string {
	t.Helper()
	b, err := json.MarshalIndent(deploymentPodTemplate(t, runClusteredHub(t, args), clusteredListenerName), "", "  ")
	require.NoError(t, err)
	return string(b)
}

// TestTheClusteredSessionListenerFollowsItsHubsPrivacy: a gcp:ClusteredNatsHub's session listener
// renders its broker settings on every hub, and its privacy settings exactly when its hub may make
// a project private (R9). With agentSecretsUrl set, both values of privateProjects render the four
// broker settings, in their token-file form, and mount the broker-audience token at
// /var/run/secrets/envoy-broker/token (t-e-19: the broker check is the session listener's credential
// off loopback); with privateProjects false, the privacy inputs set or not, the pod spec and env are
// the bytes of the same hub without them, and no privacy setting, headless Service or policy rule
// is rendered. With privateProjects true the env also holds the privacy URL, the flag,
// ENVOY_ALL_PROJECTS_ACCOUNTS and the token check, and the headless Service over the listener's
// replicas and the policy's rule for Dispatch's namespace exist. gcp:NatsHub, which runs no
// listener of its own from t-e-19 on, refuses one beside privateProjects too.
//
// Negative controls, run by hand, each on its own: drop one piece from the private render (the
// privacy URL, the broker token, ENVOY_AGENT_SECRETS_URL, ENVOY_PUBLIC_URL, the flag or
// ENVOY_ALL_PROJECTS_ACCOUNTS), render an audience beside the token file, or mount a token of the
// natsAudience at the broker token's path; each is refused or fails here naming the listener and
// the piece. Gate the broker settings on privateProjects, as t-e-13 rendered them; the all-public
// row fails naming ENVOY_AGENT_SECRETS_URL.
func TestTheClusteredSessionListenerFollowsItsHubsPrivacy(t *testing.T) {
	gcp.SetPrivateModeListenerFloor(t, privateModeFloor)
	t.Run("all-public: the broker settings and their token, no privacy setting, the privacy inputs set or not", func(t *testing.T) {
		public := privateListenerHub()
		public.PrivateProjects = false
		today := privateListenerHub()
		today.PrivateProjects = false
		today.Listener.PrivacySourceUrl, today.Listener.AllProjectsAccounts = "", nil
		require.Equal(t, podJSON(t, today), podJSON(t, public), "the all-public listener's pod spec and env, privacy inputs set")
		m := runClusteredHub(t, public)
		template := deploymentPodTemplate(t, m, clusteredListenerName)
		listener := podContainer(t, template, "listener")
		env := containerEnv(t, listener)
		for _, want := range [][2]string{
			{"ENVOY_AGENT_SECRETS_URL", privacyBrokerURL},
			{"ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", "/var/run/secrets/envoy-broker/token"},
			{"ENVOY_PUBLIC_URL", privacyTailnetOrigin},
		} {
			require.Equal(t, want[1], env[want[0]], "envoy-nats-listener of an all-public hub renders %s", want[0])
		}
		for _, name := range []string{"ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE", "ENVOY_API_ALLOW_UNAUTHENTICATED",
			"ENVOY_PROJECT_PRIVACY_URL", "ENVOY_PROJECT_PRIVACY_REQUIRED", "ENVOY_ALL_PROJECTS_ACCOUNTS"} {
			require.NotContains(t, env, name, "envoy-nats-listener of an all-public hub renders %s", name)
		}
		require.Contains(t, listener["volumeMounts"], map[string]any{"name": "envoy-broker-token", "mountPath": "/var/run/secrets/envoy-broker", "readOnly": true})
		require.Contains(t, template["spec"].(map[string]any)["volumes"], map[string]any{"name": "envoy-broker-token", "projected": map[string]any{
			"sources": []any{map[string]any{"serviceAccountToken": map[string]any{
				"audience": privacyBrokerAudience, "expirationSeconds": 3600.0, "path": "token",
			}}},
		}})
		for _, r := range m.Registrations() {
			require.NotEqual(t, clusteredListenerInstances, r.Name, "an all-public hub renders no headless Service")
		}
		rules := m.Named(t, networkPolicyType, clusteredListenerName).Inputs.Mappable()["spec"].(map[string]any)["ingress"].([]any)
		require.NotContains(t, rules, namespaceRule(9020, "example-dispatch"), "an all-public hub's policy has no rule for the all-projects callers")
	})

	t.Run("private: broker settings, privacy settings, token check, volume, Service and policy", func(t *testing.T) {
		m := runClusteredHub(t, privateListenerHub())
		requirePodGolden(t, m, clusteredListenerName, "clustered-hub-listener-pod-private.golden")
		template := deploymentPodTemplate(t, m, clusteredListenerName)
		listener := podContainer(t, template, "listener")
		env := containerEnv(t, listener)
		for name, want := range map[string]any{
			"ENVOY_AGENT_SECRETS_URL":                privacyBrokerURL,
			"ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": "/var/run/secrets/envoy-broker/token",
			"ENVOY_PUBLIC_URL":                       privacyTailnetOrigin,
			"ENVOY_PROJECT_PRIVACY_URL":              privacySourceURL,
			"ENVOY_PROJECT_PRIVACY_REQUIRED":         "true",
			"ENVOY_ALL_PROJECTS_ACCOUNTS":            privacyDispatchAccount,
			"ENVOY_OIDC_ISSUER":                      clusteredListenerIssuer,
			"ENVOY_OIDC_AUDIENCE":                    clusteredListenerAudience,
			"ENVOY_OIDC_ACCOUNTS":                    clusteredListenerAccount + "," + privacyDispatchAccount,
		} {
			require.Equal(t, want, env[name], "envoy-nats-listener of a private hub renders %s", name)
		}
		require.NotContains(t, env, "ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE", "envoy-nats-listener renders no Google audience beside its token file")
		require.Contains(t, listener["volumeMounts"], map[string]any{"name": "envoy-broker-token", "mountPath": "/var/run/secrets/envoy-broker", "readOnly": true})
		require.Contains(t, template["spec"].(map[string]any)["volumes"], map[string]any{"name": "envoy-broker-token", "projected": map[string]any{
			"sources": []any{map[string]any{"serviceAccountToken": map[string]any{
				"audience": privacyBrokerAudience, "expirationSeconds": 3600.0, "path": "token",
			}}},
		}}, "envoy-nats-listener's broker token is its own account's, for the broker's audience and not natsAudience %q", calloutNatsAudience)

		instances := m.Named(t, serviceType, clusteredListenerInstances)
		requireClusteredKubernetesChild(t, instances)
		require.Equal(t, map[string]any{
			"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]any{"name": clusteredListenerInstances, "namespace": "envoy"},
			"spec": map[string]any{
				"clusterIP": "None", "publishNotReadyAddresses": true,
				"selector": listenerPodLabels,
				"ports":    []any{map[string]any{"port": 9020.0, "targetPort": 9020.0}},
			},
		}, instances.Inputs.Mappable())
		rules := m.Named(t, networkPolicyType, clusteredListenerName).Inputs.Mappable()["spec"].(map[string]any)["ingress"].([]any)
		require.Contains(t, rules, namespaceRule(9020, "example-dispatch"), "the policy admits Dispatch's namespace at the listener's port")

		// A publicUrl that names the tailnet's origin is the same listener.
		named := privateListenerHub()
		named.Listener.PublicUrl = privacyTailnetOrigin
		require.Equal(t, podJSON(t, privateListenerHub()), podJSON(t, named), "publicUrl set to the tailnet's origin changes nothing")
	})

	t.Run("private, without tailnet: the origin is publicUrl", func(t *testing.T) {
		args := privateListenerHub()
		standaloneListener(args).PublicUrl = privacyPublicURL
		s, err := gcp.ResolveClusteredNatsHub(args)
		require.NoError(t, err)
		require.Contains(t, s.Listener.Env, "ENVOY_PUBLIC_URL="+privacyPublicURL)
	})

	t.Run("private: each missing or wrong input is refused, naming it", func(t *testing.T) {
		for _, row := range []struct {
			name, input, err string
			edit             func(*gcp.ClusteredNatsHubArgs)
		}{
			{"no broker URL", "listener.agentSecretsUrl", "listener.agentSecretsUrl is required",
				func(a *gcp.ClusteredNatsHubArgs) { a.Listener.AgentSecretsUrl = "" }},
			{"no origin without tailnet", "listener.publicUrl", "listener.publicUrl is required with listener.agentSecretsUrl without tailnet",
				func(a *gcp.ClusteredNatsHubArgs) { standaloneListener(a).PublicUrl = "" }},
			{"an origin other than the tailnet's beside tailnet", "listener.publicUrl",
				`listener.publicUrl "` + privacyPublicURL + `" is not ` + privacyTailnetOrigin + `, the origin the tailnet's sessions reach the listener at`,
				func(a *gcp.ClusteredNatsHubArgs) { a.Listener.PublicUrl = privacyPublicURL }},
			{"no broker audience", "listener.brokerAudience", "listener.brokerAudience is required with listener.agentSecretsUrl",
				func(a *gcp.ClusteredNatsHubArgs) { a.Listener.BrokerAudience = "" }},
			{"no privacy URL", "listener.privacySourceUrl", "listener.privacySourceUrl is required with privateProjects",
				func(a *gcp.ClusteredNatsHubArgs) { a.Listener.PrivacySourceUrl = "" }},
			{"no all-projects caller", "listener.allProjectsAccounts", "listener.allProjectsAccounts is required with privateProjects",
				func(a *gcp.ClusteredNatsHubArgs) { a.Listener.AllProjectsAccounts = nil }},
			{"a privacy URL naming the route", "listener",
				`listener renders ENVOY_PROJECT_PRIVACY_URL "` + privacyBrokerURL + `/v1/project-privacy" with the path "/v1/project-privacy"`,
				func(a *gcp.ClusteredNatsHubArgs) {
					a.Listener.PrivacySourceUrl = privacyBrokerURL + "/v1/project-privacy"
				}},
			{"an envoy-nats token at the broker token's path", "listener.brokerAudience",
				"listener.brokerAudience is listener.natsAudience's audience",
				func(a *gcp.ClusteredNatsHubArgs) { a.Listener.BrokerAudience = calloutNatsAudience }},
			{"an all-projects caller the token check does not admit", "listener.allProjectsAccounts",
				`listener.allProjectsAccounts[0] "` + privacyDispatchAccount + `" is not in listener.oidcAccounts`,
				func(a *gcp.ClusteredNatsHubArgs) { a.Listener.OidcAccounts = a.Listener.OidcAccounts[:1] }},
			{"a tailnet-only listener with no token check", "tailnet.listener.oidcIssuer",
				"tailnet.listener.oidcIssuer is required because privateProjects is set: the tailnet.listener checks a projected ServiceAccount token that is presented",
				func(a *gcp.ClusteredNatsHubArgs) {
					l := a.Listener
					l.OidcIssuer, l.OidcAudience, l.OidcAccounts = "", "", nil
					a.Listener, a.Tailnet.Listener = nil, l
				}},
		} {
			t.Run(row.name, func(t *testing.T) {
				args := privateListenerHub()
				row.edit(args)
				_, err := gcp.ResolveClusteredNatsHub(args)
				require.ErrorContains(t, err, row.err)
				var input *core.InputError
				require.ErrorAs(t, err, &input)
				require.Equal(t, row.input, input.Input)
				m, err := runClustered(t, "example-clustered-hub", args, nil)
				require.ErrorContains(t, err, row.err)
				require.Empty(t, m.Registrations(), "nothing registered")
			})
		}
	})

	t.Run("gcp:NatsHub refuses its own listener beside privateProjects too", func(t *testing.T) {
		args := privateNatsHub()
		args.Listener = calloutListener()
		err := gcp.ResolveNatsHub(args)
		require.ErrorContains(t, err, hubListenerRefusal)
		var input *core.InputError
		require.ErrorAs(t, err, &input)
		require.Equal(t, "listener", input.Input)
		m, err := run(t, args, nil, true)
		require.ErrorContains(t, err, hubListenerRefusal)
		require.Equal(t, []string{gcpProviderType}, m.Types(), "nothing registered beyond the test's provider")
	})
}
