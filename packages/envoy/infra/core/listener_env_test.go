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
