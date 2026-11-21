package gcp_test

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
)

// senderLimitRenderings are the sender limits a component takes, each with the variables its
// listener's environment then holds: none for no limit, the rate alone, and the rate then the
// burst. The names are written out here rather than taken from bus, so a rename is seen in what
// the listener reads.
var senderLimitRenderings = []struct {
	name        string
	rate, burst *int
	want        []string
}{
	{"no limit", nil, nil, nil},
	{"a rate alone", pulumi.IntRef(4), nil, []string{"ENVOY_SENDER_MESSAGES_PER_MINUTE=4"}},
	{"a rate and a burst", pulumi.IntRef(4), pulumi.IntRef(8), []string{"ENVOY_SENDER_MESSAGES_PER_MINUTE=4", "ENVOY_SENDER_MESSAGE_BURST=8"}},
}

// senderLimitRefusals are the sender limits every component refuses before it registers anything,
// each with the input its refusal names, below the component's own path for its listener.
var senderLimitRefusals = []struct {
	name        string
	rate, burst *int
	input       string
}{
	{"a rate of 0", pulumi.IntRef(0), nil, "senderMessagesPerMinute"},
	{"a rate of 60001", pulumi.IntRef(60001), nil, "senderMessagesPerMinute"},
	{"a burst of 0", pulumi.IntRef(4), pulumi.IntRef(0), "senderMessageBurst"},
	{"a burst with no rate", nil, pulumi.IntRef(8), "senderMessageBurst"},
}

// senderVars are the sender limit's variables a container's environment holds, NAME=value, the
// rate's first.
func senderVars(env map[string]any) []string {
	var vars []string
	for _, name := range []string{"ENVOY_SENDER_MESSAGES_PER_MINUTE", "ENVOY_SENDER_MESSAGE_BURST"} {
		if value, ok := env[name]; ok {
			vars = append(vars, name+"="+value.(string))
		}
	}
	return vars
}

// gcp:ClusteredNatsHub renders its session listener's sender limit into the listener container's
// environment exactly as tailnet.listener gives it. A limit core.CheckSenderLimit refuses stops
// the constructor with the InputError naming tailnet.listener's input, before anything registers.
func TestClusteredNatsHubListenerSenderMessageLimit(t *testing.T) {
	for _, row := range senderLimitRenderings {
		t.Run("gcp:ClusteredNatsHub renders "+row.name, func(t *testing.T) {
			args := clusteredArgs()
			listener(args).SenderMessagesPerMinute, listener(args).SenderMessageBurst = row.rate, row.burst
			template := deploymentPodTemplate(t, runClusteredHub(t, args), clusteredListenerName)
			require.Equal(t, row.want, senderVars(containerEnv(t, podContainer(t, template, "listener"))))
		})
	}
	for _, row := range senderLimitRefusals {
		t.Run("gcp:ClusteredNatsHub refuses "+row.name, func(t *testing.T) {
			args := clusteredArgs()
			listener(args).SenderMessagesPerMinute, listener(args).SenderMessageBurst = row.rate, row.burst
			_, err := gcp.ResolveClusteredNatsHub(args)
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, "tailnet.listener."+row.input, refused.Input)

			m, err := runClustered(t, "example-clustered-hub", args, nil)
			require.ErrorContains(t, err, "envoy-infra:gcp:ClusteredNatsHub example-clustered-hub: tailnet.listener."+row.input+" ")
			require.Empty(t, m.Types(), "nothing registered")
		})
	}
}

// gcp:Dispatch renders its sender limit into the listener container's environment exactly as it
// is given, and into the app's never, since envoy-dispatch reads no such setting. A limit
// core.CheckSenderLimit refuses stops the constructor with the InputError naming the input,
// before anything but the test's providers registers: the resolver checks it, since the
// listener's environment renders only once natsUrl resolves, and names its refusals natsUrl's.
func TestDispatchListenerSenderMessageLimit(t *testing.T) {
	for _, row := range senderLimitRenderings {
		t.Run("gcp:Dispatch renders "+row.name, func(t *testing.T) {
			args := dispatchArgs()
			args.SenderMessagesPerMinute, args.SenderMessageBurst = row.rate, row.burst
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			require.Equal(t, row.want, senderVars(listenerEnv(t, m)))
			require.Empty(t, senderVars(appEnv(t, m)), "the app reads no sender limit")
		})
	}
	for _, row := range senderLimitRefusals {
		t.Run("gcp:Dispatch refuses "+row.name, func(t *testing.T) {
			args := dispatchArgs()
			args.SenderMessagesPerMinute, args.SenderMessageBurst = row.rate, row.burst
			_, err := gcp.ResolveDispatch(args)
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, row.input, refused.Input)

			m, err := runDispatch(t, args, true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.input+" ")
			require.ElementsMatch(t, []string{gcpProviderType, randomProviderType, tailscaleProviderType}, m.Types(),
				"nothing registered beyond the test's providers")
		})
	}
}
