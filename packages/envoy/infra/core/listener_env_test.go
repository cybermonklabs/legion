package core_test

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// heredocEscape is a URL that, written raw into the watchdog's env-file heredoc, would end the
// heredoc and run a command of its own in the create script, which runs as root on the host.
const heredocEscape = "nats://hub.internal.example:4222\nENVEOF\ntouch /root/injected\ncat > /dev/null << 'ENVEOF'"

// requireInputError requires err to be a core.InputError refusing input, whose message holds msg.
func requireInputError(t *testing.T, err error, input, msg string) {
	t.Helper()
	require.ErrorContains(t, err, msg)
	var refused *core.InputError
	require.True(t, errors.As(err, &refused), "%v is a core.InputError", err)
	require.Equal(t, input, refused.Input)
}

// Check refuses every value of the config the listener cannot run on, each naming its input. This
// is the one table of those refusals: a component that calls Check tests only that it does, and
// where its refusal goes.
func TestListenerConfigCheck(t *testing.T) {
	type checkCase struct {
		name  string
		in    core.ListenerConfig
		input string // the input a refusal names
		err   string
	}
	cases := []checkCase{
		{name: "the default port", in: core.ListenerConfig{MachineID: "m", Port: core.DefaultListenerPort}},
		{name: "no machine id", in: core.ListenerConfig{Port: 9020}, input: "machineId", err: "machineId is empty"},
		{name: "a blank machine id", in: core.ListenerConfig{MachineID: " ", Port: 9020}, input: "machineId", err: "machineId has leading or trailing whitespace"},
		{name: "a machine id with a newline", in: core.ListenerConfig{MachineID: "a\nENVEOF\nid", Port: 9020}, input: "machineId", err: "machineId contains a control character"},
		{name: "a machine id with a tab", in: core.ListenerConfig{MachineID: "example\thost", Port: 9020}, input: "machineId", err: "machineId contains a control character"},
		{name: "port 0, which is no port", in: core.ListenerConfig{MachineID: "m"}, input: "port", err: "port 0 is outside 1-65535"},
		{name: "a negative port", in: core.ListenerConfig{MachineID: "m", Port: -1}, input: "port", err: "port -1 is outside 1-65535"},
		{name: "a port above 65535", in: core.ListenerConfig{MachineID: "m", Port: 65536}, input: "port", err: "port 65536 is outside 1-65535"},
		{name: "a listen host", in: core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: "0.0.0.0"}},
		{name: "a listen host with a control character", in: core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: "0.0.0.0\n"}, input: "listenHost", err: `listenHost contains a control character: "0.0.0.0\n"`},
		// A seed is refused before the control-character refusal that would quote it.
		{name: "a listen host holding a seed", in: core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: readmeSeed + "\n"}, input: "listenHost", err: "listenHost holds an nkey seed (not echoed)"},
		{
			name:  "a listen host holding a seed, lowercase and split across DNS labels",
			in:    core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: strings.ToLower(readmeSeed[:29] + "." + readmeSeed[29:])},
			input: "listenHost", err: "listenHost holds an nkey seed (not echoed)",
		},
		{name: "the allowed hosts an operator writes", in: core.ListenerConfig{MachineID: "m", Port: 9020, AllowedHosts: acceptedAllowedHosts}},
		{name: "the most NATS replicas, five", in: core.ListenerConfig{MachineID: "m", Port: 9020, NATSReplicas: 5}},
		{name: "a negative NATS replicas", in: core.ListenerConfig{MachineID: "m", Port: 9020, NATSReplicas: -1}, input: "natsReplicas", err: "natsReplicas -1 is outside 0-5"},
		{name: "more NATS replicas than NATS keeps", in: core.ListenerConfig{MachineID: "m", Port: 9020, NATSReplicas: 6}, input: "natsReplicas", err: "natsReplicas 6 is outside 0-5"},
		{name: "a stream byte limit", in: core.ListenerConfig{MachineID: "m", Port: 9020, StreamMaxBytes: 1}},
		{name: "a negative stream byte limit", in: core.ListenerConfig{MachineID: "m", Port: 9020, StreamMaxBytes: -1}, input: "streamMaxBytes", err: "streamMaxBytes -1 is below 0"},
	}
	// The listener refuses each of these at boot, so a config carrying one deploys a listener
	// that never serves; the entry is refused by its index, beside an entry that is fine.
	for _, entry := range refusedAllowedHosts {
		cases = append(cases, checkCase{
			name:  "an allowed host " + entry,
			in:    core.ListenerConfig{MachineID: "m", Port: 9020, AllowedHosts: []string{"ok.internal.example", entry}},
			input: "allowedHosts",
			err:   fmt.Sprintf("allowedHosts[1] is %q: want a host name or address with no port, scheme or path", entry),
		})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.in.Check()
			env, envErr := c.in.Env([]string{"nats://127.0.0.1:4222"})
			if c.err != "" {
				requireInputError(t, err, c.input, c.err)
				require.NotContains(t, strings.ToUpper(err.Error()), readmeSeed[:12], "the refusal quotes no part of the seed")
				// Env runs Check itself, so a caller that skips Check still gets its refusal.
				require.Nil(t, env)
				requireInputError(t, envErr, c.input, c.err)
				return
			}
			require.NoError(t, err)
			require.NoError(t, envErr)
		})
	}
}

// Env runs Check, then renders the environment, and refuses the URLs itself.
func TestListenerConfigEnv(t *testing.T) {
	cases := []struct {
		name string
		in   core.ListenerConfig
		urls []string
		want []string
		err  string // a refusal of natsUrls
	}{
		{
			name: "remote NATS, the default port",
			in:   core.ListenerConfig{MachineID: "example-host-a", Port: core.DefaultListenerPort},
			urls: []string{"nats://hub.internal.example:4222"},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=example-host-a", "NATS_URLS=nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1"},
		},
		{
			name: "loopback NATS needs no remote opt-in",
			in:   core.ListenerConfig{MachineID: "m", Port: 9021},
			urls: []string{"nats://127.0.0.1:4222"},
			want: []string{"PORT=9021", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222"},
		},
		{
			name: "any remote URL in the list opts in",
			in:   core.ListenerConfig{MachineID: "m", Port: 9020},
			urls: []string{"nats://localhost:4222", "nats://hub.internal.example:4222"},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://localhost:4222,nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1"},
		},
		{
			name: "a listen host, right after the port",
			in:   core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: "0.0.0.0"},
			urls: []string{"nats://hub.internal.example:4222"},
			want: []string{"PORT=9020", "ENVOY_LISTEN_HOST=0.0.0.0", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1"},
		},
		{
			name: "allowed hosts, right after the listen host and as written",
			in:   core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: "0.0.0.0", AllowedHosts: acceptedAllowedHosts},
			urls: []string{"nats://127.0.0.1:4222"},
			want: []string{"PORT=9020", "ENVOY_LISTEN_HOST=0.0.0.0", "ENVOY_ALLOWED_HOSTS=Hub.Internal.Example.,192.0.2.7,[2001:db8::1]", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222"},
		},
		{
			name: "no allowed host leaves ENVOY_ALLOWED_HOSTS unset",
			in:   core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: "0.0.0.0"},
			urls: []string{"nats://127.0.0.1:4222"},
			want: []string{"PORT=9020", "ENVOY_LISTEN_HOST=0.0.0.0", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222"},
		},
		{
			name: "no NATS replicas and no stream byte limit render neither variable",
			in:   core.ListenerConfig{MachineID: "m", Port: 9020, NATSReplicas: 0, StreamMaxBytes: 0},
			urls: []string{"nats://127.0.0.1:4222"},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222"},
		},
		{
			name: "NATS replicas alone renders ENVOY_NATS_REPLICAS last",
			in:   core.ListenerConfig{MachineID: "m", Port: 9020, NATSReplicas: 3},
			urls: []string{"nats://127.0.0.1:4222"},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222", "ENVOY_NATS_REPLICAS=3"},
		},
		{
			name: "a stream byte limit alone renders ENVOY_STREAM_MAX_BYTES last",
			in:   core.ListenerConfig{MachineID: "m", Port: 9020, StreamMaxBytes: 21474836480},
			urls: []string{"nats://127.0.0.1:4222"},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222", "ENVOY_STREAM_MAX_BYTES=21474836480"},
		},
		{
			name: "NATS replicas then the stream byte limit, after the proxy",
			in:   core.ListenerConfig{MachineID: "m", Port: 9020, ListenHost: "0.0.0.0", AllowedHosts: []string{"hub.internal.example"}, SOCKS5Proxy: "127.0.0.1:1055", NATSReplicas: 3, StreamMaxBytes: 1073741824},
			urls: []string{"nats://hub.internal.example:4222"},
			want: []string{"PORT=9020", "ENVOY_LISTEN_HOST=0.0.0.0", "ENVOY_ALLOWED_HOSTS=hub.internal.example", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1", "ENVOY_NATS_SOCKS5_PROXY=127.0.0.1:1055", "ENVOY_NATS_REPLICAS=3", "ENVOY_STREAM_MAX_BYTES=1073741824"},
		},
		{name: "no URL", in: core.ListenerConfig{MachineID: "m", Port: 9020}, err: "natsUrls needs at least one URL"},
		{name: "a comma in a URL", in: core.ListenerConfig{MachineID: "m", Port: 9020}, urls: []string{"nats://a:1,nats://b:2"}, err: "natsUrls[0] contains a comma"},
		{name: "an empty URL", in: core.ListenerConfig{MachineID: "m", Port: 9020}, urls: []string{" "}, err: "natsUrls[0] is empty"},
		{name: "a refused URL is named by its index", in: core.ListenerConfig{MachineID: "m", Port: 9020}, urls: []string{"nats://127.0.0.1:4222", ""}, err: "natsUrls[1] is empty"},
		{name: "a URL with a newline", in: core.ListenerConfig{MachineID: "m", Port: 9020}, urls: []string{heredocEscape}, err: "natsUrls[0] contains a control character"},
		{name: "a URL with any control character, by its index", in: core.ListenerConfig{MachineID: "m", Port: 9020}, urls: []string{"nats://127.0.0.1:4222", "nats://hub.internal.example:4222\r"}, err: "natsUrls[1] contains a control character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.in.Env(c.urls)
			if c.err != "" {
				requireInputError(t, err, "natsUrls", c.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
}

func TestListenerConfigSOCKS5Proxy(t *testing.T) {
	config := core.ListenerConfig{
		MachineID:   "example-dispatch",
		Port:        core.DefaultListenerPort,
		SOCKS5Proxy: "127.0.0.1:1055",
	}
	env, err := config.Env([]string{"nats://hub.internal.example:4222"})
	require.NoError(t, err)
	require.Equal(t, []string{
		"PORT=9020",
		"ENVOY_MACHINE_ID=example-dispatch",
		"NATS_URLS=nats://hub.internal.example:4222",
		"ENVOY_ALLOW_REMOTE_NATS=1",
		"ENVOY_NATS_SOCKS5_PROXY=127.0.0.1:1055",
	}, env)

	err = (core.ListenerConfig{
		MachineID:   "example-dispatch",
		Port:        core.DefaultListenerPort,
		SOCKS5Proxy: "socks5://127.0.0.1:1055",
	}).Check()
	requireInputError(t, err, "socks5Proxy", "ENVOY_NATS_SOCKS5_PROXY")

	_, err = config.Env([]string{"nats://127.0.0.1:4222"})
	requireInputError(t, err, "socks5Proxy", "natsUrls")
}

// serviceIdentityVars are the two variables a listener's bus connection reads a service's platform
// identity from (internal/bus, credential), written out here rather than taken from bus, so a
// rename of one is seen in the environment a listener boots on.
var serviceIdentityVars = []string{"NATS_SERVICE_TOKEN_FILE", "NATS_SERVICE_GOOGLE_AUDIENCE"}

// A listener behind the auth service presents a service's platform identity on its NATS
// connection: a projected ServiceAccount token's file, in a pod, or the audience of the VM's
// Google identity token, each as the bus reads it, after the broker check, and each only when it
// is set, so a config that sets neither renders what it always rendered. The listener connects as
// one identity, so both set are refused naming both; a seed in either is refused naming the
// field and never quoting it, and so is whitespace or a control character, which one docker run
// argument or a watchdog's heredoc line cannot carry.
func TestListenerEnvServiceIdentity(t *testing.T) {
	urls := []string{"nats://127.0.0.1:4222"}
	base := core.ListenerConfig{
		MachineID: "m", Port: 9020,
		AgentSecretsURL: "https://example-broker.internal.example", AgentSecretsAudience: "example-broker-audience",
		PublicURL: "http://example-hub.example-tailnet.ts.net:9020",
	}
	without, err := base.Env(urls)
	require.NoError(t, err)
	for _, name := range serviceIdentityVars {
		require.NotContains(t, strings.Join(without, "\n"), name, "a config with no service identity renders neither variable")
	}
	for _, row := range []struct {
		name string
		edit func(*core.ListenerConfig)
		line string
	}{
		{"a projected token's file", func(c *core.ListenerConfig) { c.ServiceTokenFile = "/var/run/secrets/envoy/nats-token" },
			"NATS_SERVICE_TOKEN_FILE=/var/run/secrets/envoy/nats-token"},
		{"a VM's Google audience", func(c *core.ListenerConfig) { c.ServiceGoogleAudience = "example-nats-audience" },
			"NATS_SERVICE_GOOGLE_AUDIENCE=example-nats-audience"},
	} {
		t.Run(row.name, func(t *testing.T) {
			config := base
			row.edit(&config)
			got, err := config.Env(urls)
			require.NoError(t, err)
			require.Equal(t, append(slices.Clone(without), row.line), got, "the identity follows the broker check, and nothing else changes")
		})
	}
	for _, row := range []struct {
		name, input, err string
		edit             func(*core.ListenerConfig)
	}{
		{"both identities", "serviceGoogleAudience", "serviceTokenFile and serviceGoogleAudience are both set: the listener connects to NATS as one identity",
			func(c *core.ListenerConfig) {
				c.ServiceTokenFile, c.ServiceGoogleAudience = "/var/run/secrets/envoy/nats-token", "example-nats-audience"
			}},
		{"a seed as the token's file", "serviceTokenFile", "serviceTokenFile holds an nkey seed (not echoed)",
			func(c *core.ListenerConfig) { c.ServiceTokenFile = "/var/run/" + readmeSeed }},
		{"a lowercase seed as the audience", "serviceGoogleAudience", "serviceGoogleAudience holds an nkey seed (not echoed)",
			func(c *core.ListenerConfig) { c.ServiceGoogleAudience = strings.ToLower(readmeSeed) }},
		{"a token file with a space", "serviceTokenFile", "serviceTokenFile holds whitespace or a control character, which NATS_SERVICE_TOKEN_FILE cannot carry (not echoed)",
			func(c *core.ListenerConfig) { c.ServiceTokenFile = "/var/run/token --privileged" }},
		{"an audience with a line break", "serviceGoogleAudience", "serviceGoogleAudience holds whitespace or a control character, which NATS_SERVICE_GOOGLE_AUDIENCE cannot carry (not echoed)",
			func(c *core.ListenerConfig) { c.ServiceGoogleAudience = "example-nats-audience\nENVEOF" }},
		{"a token file with a NUL", "serviceTokenFile", "serviceTokenFile holds whitespace or a control character, which NATS_SERVICE_TOKEN_FILE cannot carry (not echoed)",
			func(c *core.ListenerConfig) { c.ServiceTokenFile = "/var/run/token\x00" }},
		{"an audience with a DEL", "serviceGoogleAudience", "serviceGoogleAudience holds whitespace or a control character, which NATS_SERVICE_GOOGLE_AUDIENCE cannot carry (not echoed)",
			func(c *core.ListenerConfig) { c.ServiceGoogleAudience = "example-nats\x7faudience" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			config := base
			row.edit(&config)
			requireInputError(t, config.Check(), row.input, row.err)
			env, err := config.Env(urls)
			require.Nil(t, env)
			requireInputError(t, err, row.input, row.err)
			require.NotContains(t, strings.ToUpper(err.Error()), readmeSeed[:12], "the refusal quotes no part of the seed")
		})
	}
}

// privateListenerConfig is a listener of a hub that may make a project private, in the form
// gcp:ClusteredNatsHub renders it: the broker check in its token-file form, and the privacy
// settings.
func privateListenerConfig() core.ListenerConfig {
	return core.ListenerConfig{
		MachineID: "m", Port: 9020, ListenHost: "0.0.0.0",
		AgentSecretsURL: "https://example-broker.internal.example", BrokerTokenFile: "/var/run/secrets/envoy-broker/token",
		PublicURL:        "http://example-hub-listener.example-namespace.svc.cluster.local:9020",
		ServiceTokenFile: "/var/run/secrets/envoy/nats-token",
		PrivacySourceURL: "https://example-broker.internal.example", PrivacyRequired: true,
		AllProjectsAccounts: []string{"system:serviceaccount:example-dispatch:example-dispatch"},
	}
}

// The privacy settings render last, after the service identity, in a fixed order:
// ENVOY_PROJECT_PRIVACY_URL, ENVOY_PROJECT_PRIVACY_REQUIRED=true and ENVOY_ALL_PROJECTS_ACCOUNTS,
// each only on a config that sets it, so a config that sets none renders what it always rendered
// (R9), and the goldens every component that renders a listener pins hold none of them. The broker
// check's token-file form renders ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE between the audience's
// place and ENVOY_PUBLIC_URL. The names are written out here rather than taken from the listener,
// so a rename is seen in what the listener reads.
func TestListenerEnvRendersThePrivacySettingsLast(t *testing.T) {
	urls := []string{"nats://example-hub.example-namespace.svc.cluster.local:4222"}
	private := privateListenerConfig()
	public := private
	public.PrivacySourceURL, public.PrivacyRequired, public.AllProjectsAccounts = "", false, nil
	off, err := public.Env(urls)
	require.NoError(t, err)
	for _, name := range []string{"ENVOY_PROJECT_PRIVACY", "ENVOY_ALL_PROJECTS_ACCOUNTS"} {
		require.NotContains(t, strings.Join(off, "\n"), name, "a config with no privacy setting renders none")
	}
	require.Equal(t, []string{
		"PORT=9020", "ENVOY_LISTEN_HOST=0.0.0.0", "ENVOY_MACHINE_ID=m",
		"NATS_URLS=nats://example-hub.example-namespace.svc.cluster.local:4222", "ENVOY_ALLOW_REMOTE_NATS=1",
		"ENVOY_AGENT_SECRETS_URL=https://example-broker.internal.example",
		"ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE=/var/run/secrets/envoy-broker/token",
		"ENVOY_PUBLIC_URL=http://example-hub-listener.example-namespace.svc.cluster.local:9020",
		"NATS_SERVICE_TOKEN_FILE=/var/run/secrets/envoy/nats-token",
	}, off)
	on, err := private.Env(urls)
	require.NoError(t, err)
	require.Equal(t, append(slices.Clone(off),
		"ENVOY_PROJECT_PRIVACY_URL=https://example-broker.internal.example",
		"ENVOY_PROJECT_PRIVACY_REQUIRED=true",
		"ENVOY_ALL_PROJECTS_ACCOUNTS=system:serviceaccount:example-dispatch:example-dispatch"), on)
}

// The introspection limit renders right after the broker check, only on a config that sets it.
// The limit needs the broker check and its bounds, which the listener refuses at boot.
func TestListenerEnvRendersTheIntrospectionLimit(t *testing.T) {
	urls := []string{"nats://example-hub.example-namespace.svc.cluster.local:4222"}
	plain, err := privateListenerConfig().Env(urls)
	require.NoError(t, err)
	c := privateListenerConfig()
	c.IntrospectionsPerSecond, c.IntrospectionBurst = 50, 200
	got, err := c.Env(urls)
	require.NoError(t, err)
	at := slices.Index(plain, "ENVOY_PUBLIC_URL=http://example-hub-listener.example-namespace.svc.cluster.local:9020") + 1
	want := slices.Concat(plain[:at],
		[]string{"ENVOY_AGENT_SECRETS_INTROSPECTIONS_PER_SECOND=50", "ENVOY_AGENT_SECRETS_INTROSPECTION_BURST=200"},
		plain[at:])
	require.Equal(t, want, got)
	require.NoError(t, core.CheckPrivacySource("tailnet.listener", c, true))

	for _, row := range []struct {
		name, input, err string
		edit             func(*core.ListenerConfig)
	}{
		{"a rate past its bound", "introspectionsPerSecond", "introspectionsPerSecond 1001 must be from 1 to 1000",
			func(c *core.ListenerConfig) { c.IntrospectionsPerSecond = 1001 }},
		{"a burst past its bound", "introspectionBurst", "introspectionBurst 100001 must be from 1 to 100000",
			func(c *core.ListenerConfig) { c.IntrospectionBurst = 100001 }},
		{"a limit without the broker check", "introspectionBurst", "introspectionBurst is set without agentSecretsUrl",
			func(c *core.ListenerConfig) {
				*c = core.ListenerConfig{MachineID: "m", Port: 9020, IntrospectionBurst: 20}
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			config := privateListenerConfig()
			row.edit(&config)
			requireInputError(t, config.Check(), row.input, row.err)
		})
	}
}

// CheckBroker takes the broker check in either identity's form, the VM's Google audience or a
// pod's projected token file, and refuses what the listener's resolveListenerBrokerConfig refuses
// at boot: both identities at once, and a missing URL or origin beside either.
func TestCheckBrokerTakesEitherIdentity(t *testing.T) {
	const url, audience, file, origin = "https://example-broker.internal.example", "example-broker-audience", "/var/run/secrets/envoy-broker/token", "http://example-listener:9020"
	require.NoError(t, core.CheckBroker("", url, audience, "", origin), "the audience form")
	require.NoError(t, core.CheckBroker("", url, "", file, origin), "the token-file form")
	require.NoError(t, core.CheckBroker("", "", "", "", ""), "no broker check at all")
	for _, row := range []struct {
		name, url, audience, file, origin string
		input, err                        string
	}{
		{"both identities", url, audience, file, origin, "brokerTokenFile",
			"agentSecretsAudience and brokerTokenFile are both set: the listener introspects as one identity"},
		{"a token file with no broker", "", "", file, origin, "agentSecretsUrl", "agentSecretsUrl is required when brokerTokenFile is set"},
		{"a token file with no origin", url, "", file, "", "publicUrl", "publicUrl is required when agentSecretsUrl is set"},
		{"a broker with no identity", url, "", "", origin, "agentSecretsAudience",
			"agentSecretsAudience is required when agentSecretsUrl is set: the listener takes a broker access token only with ENVOY_AGENT_SECRETS_URL, ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE or ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE, and ENVOY_PUBLIC_URL"},
		{"a token file with a space", url, "", "/var/run/token --privileged", origin, "brokerTokenFile",
			"brokerTokenFile holds whitespace or a control character, which ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE cannot carry"},
	} {
		t.Run(row.name, func(t *testing.T) {
			requireInputError(t, core.CheckBroker("", row.url, row.audience, row.file, row.origin), row.input, row.err)
		})
	}
}

// CheckPrivacySource holds a listener of a hub that may make a project private to the settings its
// required mode reads: the privacy URL, the flag, the all-projects callers, and the broker check in
// its token-file form (the URL, the token file and the origin, and no Google audience beside the
// file), since the listener reads each decision's privacy from the secrets broker with the
// projected token of its own account. Each refusal names the listener, as the component spells it,
// and the variable left out. A listener of a hub that makes no project private renders none of the
// privacy settings, whatever else it renders (R9).
func TestCheckPrivacySourceNamesTheListenerAndThePiece(t *testing.T) {
	require.NoError(t, core.CheckPrivacySource("tailnet.listener", privateListenerConfig(), true))
	public := privateListenerConfig()
	public.PrivacySourceURL, public.PrivacyRequired, public.AllProjectsAccounts = "", false, nil
	require.NoError(t, core.CheckPrivacySource("tailnet.listener", public, false), "an all-public hub's listener with a broker check and no privacy setting")
	for _, row := range []struct {
		name, piece string
		edit        func(*core.ListenerConfig)
	}{
		{"no privacy URL", "ENVOY_PROJECT_PRIVACY_URL", func(c *core.ListenerConfig) { c.PrivacySourceURL = "" }},
		{"no flag", "ENVOY_PROJECT_PRIVACY_REQUIRED", func(c *core.ListenerConfig) { c.PrivacyRequired = false }},
		{"no all-projects callers", "ENVOY_ALL_PROJECTS_ACCOUNTS", func(c *core.ListenerConfig) { c.AllProjectsAccounts = nil }},
		{"no broker URL", "ENVOY_AGENT_SECRETS_URL", func(c *core.ListenerConfig) { c.AgentSecretsURL = "" }},
		{"no broker token", "ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE", func(c *core.ListenerConfig) { c.BrokerTokenFile = "" }},
		{"no origin", "ENVOY_PUBLIC_URL", func(c *core.ListenerConfig) { c.PublicURL = "" }},
		{"an audience beside the token file", "", func(c *core.ListenerConfig) { c.AgentSecretsAudience = "example-broker-audience" }},
	} {
		t.Run(row.name, func(t *testing.T) {
			config := privateListenerConfig()
			row.edit(&config)
			want := "tailnet.listener renders no " + row.piece
			if row.piece == "" {
				want = "tailnet.listener renders ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE beside ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"
			}
			requireInputError(t, core.CheckPrivacySource("tailnet.listener", config, true), "tailnet.listener", want)
		})
	}
	// The privacy URL is the secrets broker's base URL, to which the listener appends the route
	// itself, so a URL naming a path, the route included, is refused naming the listener; a bare
	// trailing slash is no path.
	slash := privateListenerConfig()
	slash.PrivacySourceURL += "/"
	require.NoError(t, core.CheckPrivacySource("tailnet.listener", slash, true), "the base URL with a trailing slash")
	route := privateListenerConfig()
	route.PrivacySourceURL += "/v1/project-privacy"
	requireInputError(t, core.CheckPrivacySource("tailnet.listener", route, true), "tailnet.listener",
		`tailnet.listener renders ENVOY_PROJECT_PRIVACY_URL "https://example-broker.internal.example/v1/project-privacy" with the path "/v1/project-privacy"`)
	// The listener presents its broker token at the privacy URL, so another origin is refused.
	foreign := privateListenerConfig()
	foreign.PrivacySourceURL = "https://example-elsewhere.internal.example"
	requireInputError(t, core.CheckPrivacySource("tailnet.listener", foreign, true), "tailnet.listener",
		`tailnet.listener renders ENVOY_PROJECT_PRIVACY_URL "https://example-elsewhere.internal.example" at an origin other than ENVOY_AGENT_SECRETS_URL "https://example-broker.internal.example"`)
	for _, row := range []struct {
		name, piece string
		edit        func(*core.ListenerConfig)
	}{
		{"a privacy URL", "ENVOY_PROJECT_PRIVACY_URL", func(c *core.ListenerConfig) { c.PrivacyRequired = false }},
		{"the flag", "ENVOY_PROJECT_PRIVACY_REQUIRED", func(c *core.ListenerConfig) { c.PrivacySourceURL = "" }},
		{"all-projects callers", "ENVOY_ALL_PROJECTS_ACCOUNTS", func(c *core.ListenerConfig) { c.PrivacySourceURL, c.PrivacyRequired = "", false }},
	} {
		t.Run("an all-public hub's listener with "+row.name, func(t *testing.T) {
			config := privateListenerConfig()
			row.edit(&config)
			requireInputError(t, core.CheckPrivacySource("tailnet.listener", config, false), "tailnet.listener",
				"tailnet.listener renders "+row.piece+" on a hub that makes no project private")
		})
	}
}

// Check refuses a privacy setting the listener would refuse at boot: the URL without the flag and
// the flag without the URL, which plan C's listener refuses as a half-set mode, a URL that is not an
// http(s) URL with a host, and an all-projects caller that is not a ServiceAccount's subject or
// repeats an earlier one.
func TestListenerConfigCheckRefusesAHalfSetPrivacyMode(t *testing.T) {
	for _, row := range []struct {
		name, input, err string
		edit             func(*core.ListenerConfig)
	}{
		{"a URL without the flag", "privacyRequired", "privacySourceUrl is set without privacyRequired", func(c *core.ListenerConfig) { c.PrivacyRequired = false }},
		{"the flag without a URL", "privacySourceUrl", "privacySourceUrl is required with privacyRequired", func(c *core.ListenerConfig) { c.PrivacySourceURL = "" }},
		{"a URL with no scheme", "privacySourceUrl", "privacySourceUrl \"example-broker\" must be an https URL with a host",
			func(c *core.ListenerConfig) { c.PrivacySourceURL = "example-broker" }},
		{"a plaintext URL", "privacySourceUrl", "privacySourceUrl \"http://example-broker.internal.example\" must be an https URL with a host",
			func(c *core.ListenerConfig) { c.PrivacySourceURL = "http://example-broker.internal.example" }},
		{"a caller that is no subject", "allProjectsAccounts", "allProjectsAccounts[0] \"dispatch\" must be system:serviceaccount:<namespace>:<name>",
			func(c *core.ListenerConfig) { c.AllProjectsAccounts = []string{"dispatch"} }},
		{"a repeated caller", "allProjectsAccounts", "allProjectsAccounts[1] \"system:serviceaccount:example-dispatch:example-dispatch\" repeats allProjectsAccounts[0]",
			func(c *core.ListenerConfig) {
				c.AllProjectsAccounts = append(c.AllProjectsAccounts, c.AllProjectsAccounts[0])
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			config := privateListenerConfig()
			row.edit(&config)
			requireInputError(t, config.Check(), row.input, row.err)
		})
	}
}

// Check refuses a sender limit the listener refuses when it boots, and takes every limit it boots
// on: none, and a rate from 1 to 60000 with or without a burst. Env runs Check, so a refused limit
// renders nothing.
func TestListenerConfigCheckRefusesASenderMessageLimitTheListenerRefuses(t *testing.T) {
	for _, c := range []struct {
		name        string
		rate, burst int
		input, err  string // the input a refusal names, and its message
	}{
		{name: "no limit"},
		{name: "the smallest rate alone", rate: 1},
		{name: "the largest rate with a burst", rate: 60000, burst: 4},
		{name: "a negative rate", rate: -1, input: "senderMessagesPerMinute", err: "senderMessagesPerMinute -1 is outside 0-60000"},
		{name: "a rate above 60000", rate: 60001, input: "senderMessagesPerMinute", err: "senderMessagesPerMinute 60001 is outside 0-60000"},
		{name: "a negative burst", rate: 4, burst: -1, input: "senderMessageBurst", err: "senderMessageBurst -1 is below 0"},
		{name: "a burst with no rate", burst: 8, input: "senderMessageBurst", err: "senderMessageBurst 8 is set with no senderMessagesPerMinute"},
	} {
		t.Run(c.name, func(t *testing.T) {
			config := core.ListenerConfig{MachineID: "m", Port: 9020, SenderMessagesPerMinute: c.rate, SenderMessageBurst: c.burst}
			err := config.Check()
			env, envErr := config.Env([]string{"nats://127.0.0.1:4222"})
			if c.err == "" {
				require.NoError(t, err)
				require.NoError(t, envErr)
				return
			}
			requireInputError(t, err, c.input, c.err)
			require.Nil(t, env)
			requireInputError(t, envErr, c.input, c.err)
		})
	}
}

// Env renders each sender variable exactly when its field is above 0, after the stream byte limit
// and before the broker check, so a listener with no limit renders what it always rendered. The
// names are written out here rather than taken from bus, so a rename is seen in what the listener
// reads.
func TestListenerConfigEnvRendersTheSenderMessageLimit(t *testing.T) {
	base := []string{"PORT=9020", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222"}
	for _, c := range []struct {
		name string
		in   core.ListenerConfig
		want []string
	}{
		{"no limit renders neither variable", core.ListenerConfig{MachineID: "m", Port: 9020}, base},
		{
			"a rate alone renders the rate alone",
			core.ListenerConfig{MachineID: "m", Port: 9020, SenderMessagesPerMinute: 4},
			slices.Concat(base, []string{"ENVOY_SENDER_MESSAGES_PER_MINUTE=4"}),
		},
		{
			"a rate and a burst render both, the rate first",
			core.ListenerConfig{MachineID: "m", Port: 9020, SenderMessagesPerMinute: 4, SenderMessageBurst: 8},
			slices.Concat(base, []string{"ENVOY_SENDER_MESSAGES_PER_MINUTE=4", "ENVOY_SENDER_MESSAGE_BURST=8"}),
		},
		{
			"after the stream byte limit and before the broker check",
			core.ListenerConfig{
				MachineID: "m", Port: 9020, NATSReplicas: 3, StreamMaxBytes: 1073741824,
				SenderMessagesPerMinute: 60000, SenderMessageBurst: 1,
				AgentSecretsURL: "https://broker.internal.example", AgentSecretsAudience: "broker-audience",
				PublicURL: "https://listener.internal.example",
			},
			slices.Concat(base, []string{
				"ENVOY_NATS_REPLICAS=3", "ENVOY_STREAM_MAX_BYTES=1073741824",
				"ENVOY_SENDER_MESSAGES_PER_MINUTE=60000", "ENVOY_SENDER_MESSAGE_BURST=1",
				"ENVOY_AGENT_SECRETS_URL=https://broker.internal.example", "ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE=broker-audience",
				"ENVOY_PUBLIC_URL=https://listener.internal.example",
			}),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.in.Env([]string{"nats://127.0.0.1:4222"})
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
}

// CheckSenderLimit is a component's rule for its two sender inputs: unset is no limit, a rate is a
// whole number from 1 to 60000, and a burst is at least 1 and comes with a rate. Each refusal names
// the input by the component's own path, prefix and all, and each limit it answers is one Check
// takes, so the listener a component deploys boots on it.
func TestCheckSenderLimitRefusesEachSenderMessageInputByItsPath(t *testing.T) {
	n := func(v int) *int { return &v }
	for _, prefix := range []string{"", "listener.", "tailnet.listener."} {
		t.Run("prefix "+strconv.Quote(prefix), func(t *testing.T) {
			for _, c := range []struct {
				name                string
				rate, burst         *int
				wantRate, wantBurst int
				input, err          string // %[1]s is the prefix
			}{
				{name: "unset"},
				{name: "a rate alone", rate: n(4), wantRate: 4},
				{name: "a rate and a burst", rate: n(4), burst: n(8), wantRate: 4, wantBurst: 8},
				{name: "the smallest rate and burst", rate: n(1), burst: n(1), wantRate: 1, wantBurst: 1},
				{name: "the largest rate", rate: n(60000), wantRate: 60000},
				{
					name: "a rate of 0", rate: n(0), input: "senderMessagesPerMinute",
					err: "%[1]ssenderMessagesPerMinute 0 is outside 1-60000; leave it unset for no limit",
				},
				{
					name: "a negative rate", rate: n(-1), input: "senderMessagesPerMinute",
					err: "%[1]ssenderMessagesPerMinute -1 is outside 1-60000",
				},
				{
					name: "a rate of 60001", rate: n(60001), input: "senderMessagesPerMinute",
					err: "%[1]ssenderMessagesPerMinute 60001 is outside 1-60000",
				},
				{
					name: "a burst of 0", rate: n(4), burst: n(0), input: "senderMessageBurst",
					err: "%[1]ssenderMessageBurst 0 is below 1",
				},
				{
					name: "a negative burst", rate: n(4), burst: n(-1), input: "senderMessageBurst",
					err: "%[1]ssenderMessageBurst -1 is below 1",
				},
				{
					name: "a burst with no rate", burst: n(8), input: "senderMessageBurst",
					err: "%[1]ssenderMessageBurst is given without %[1]ssenderMessagesPerMinute",
				},
			} {
				t.Run(c.name, func(t *testing.T) {
					rate, burst, err := core.CheckSenderLimit(prefix, c.rate, c.burst)
					if c.err != "" {
						requireInputError(t, err, prefix+c.input, fmt.Sprintf(c.err, prefix))
						return
					}
					require.NoError(t, err)
					require.Equal(t, [2]int{c.wantRate, c.wantBurst}, [2]int{rate, burst})
					require.NoError(t, core.ListenerConfig{MachineID: "m", Port: 9020, SenderMessagesPerMinute: rate, SenderMessageBurst: burst}.Check())
				})
			}
		})
	}
}
