package core_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// CheckConsumerNames takes the three values a creator starts on, unset and the two modes, and
// refuses every other by the caller's path, quoting it, as the creators' own rule refuses it at
// start: case and surrounding whitespace count, and a list is no mode. A seed is refused first,
// naming the path and never the value.
func TestCheckConsumerNamesTakesTheTwoModesAndUnset(t *testing.T) {
	for _, input := range []string{"consumerNames", "listener.consumerNames", "tailnet.listener.consumerNames"} {
		for _, value := range []string{"", "fixed", "fresh"} {
			require.NoError(t, core.CheckConsumerNames(input, value), "%s %q", input, value)
		}
		for _, value := range []string{"Fresh", "FIXED", "fresh ", " fixed", "on", "fixed,fresh", " "} {
			err := core.CheckConsumerNames(input, value)
			requireInputError(t, err, input, input+` "`+value+`" must be "fixed" or "fresh"`)
			require.ErrorContains(t, err, "ENVOY_CONSUMER_NAMES")
			require.ErrorContains(t, err, "leave it unset for fixed")
		}
		err := core.CheckConsumerNames(input, "fresh-"+readmeSeed)
		require.EqualError(t, err, input+" holds an nkey seed (not echoed)")
		requireInputError(t, err, input, "")
	}
}

// Check refuses a consumer-name setting the creators refuse, under core's own name for it, and
// Env, which runs Check, renders nothing then.
func TestListenerConfigCheckRefusesAConsumerNameSettingTheListenerRefuses(t *testing.T) {
	config := core.ListenerConfig{MachineID: "m", Port: 9020, ConsumerNames: "Fresh"}
	want := `consumerNames "Fresh" must be "fixed" or "fresh"`
	requireInputError(t, config.Check(), "consumerNames", want)
	env, err := config.Env([]string{"nats://127.0.0.1:4222"})
	require.Nil(t, env)
	requireInputError(t, err, "consumerNames", want)
}

// Env renders ENVOY_CONSUMER_NAMES exactly when the field is set, as the last variable, after the
// broker check, so a listener whose deployment leaves it unset renders what it always rendered,
// byte for byte, and runs fixed. The name is written out here rather than taken from core, so a
// rename is seen in what the listener reads.
func TestListenerConfigEnvRendersTheConsumerNamesLast(t *testing.T) {
	base := []string{"PORT=9020", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222"}
	broker := core.ListenerConfig{
		MachineID: "m", Port: 9020, SenderMessagesPerMinute: 4,
		AgentSecretsURL: "https://broker.internal.example", AgentSecretsAudience: "broker-audience",
		PublicURL: "https://listener.internal.example",
	}
	brokerEnv := slices.Concat(base, []string{
		"ENVOY_SENDER_MESSAGES_PER_MINUTE=4",
		"ENVOY_AGENT_SECRETS_URL=https://broker.internal.example", "ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE=broker-audience",
		"ENVOY_PUBLIC_URL=https://listener.internal.example",
	})
	withMode := func(c core.ListenerConfig, mode string) core.ListenerConfig {
		c.ConsumerNames = mode
		return c
	}
	plain := core.ListenerConfig{MachineID: "m", Port: 9020}
	for _, c := range []struct {
		name string
		in   core.ListenerConfig
		want []string
	}{
		{"unset renders no variable", plain, base},
		{"fixed renders fixed", withMode(plain, "fixed"), slices.Concat(base, []string{"ENVOY_CONSUMER_NAMES=fixed"})},
		{"fresh renders fresh", withMode(plain, "fresh"), slices.Concat(base, []string{"ENVOY_CONSUMER_NAMES=fresh"})},
		{"unset beside the broker check renders the broker check alone", broker, brokerEnv},
		{"fresh comes after the broker check", withMode(broker, "fresh"), slices.Concat(brokerEnv, []string{"ENVOY_CONSUMER_NAMES=fresh"})},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.in.Env([]string{"nats://127.0.0.1:4222"})
			require.NoError(t, err)
			require.Equal(t, c.want, got)
			if c.in.ConsumerNames == "" {
				require.False(t, slices.ContainsFunc(got, func(v string) bool { return strings.HasPrefix(v, "ENVOY_CONSUMER_NAMES=") }))
			}
		})
	}
}
