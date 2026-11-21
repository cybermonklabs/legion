package natsgrant_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/natsgrant"
)

// The hub-admission grant holds the connection to its own reply prefix: it subscribes that prefix
// and publishes the one ping subject under it, so the connection proves it was admitted and reaches
// no project subject, no JetStream or KV call and no server request.
func TestHubAdmissionHoldsOnlyItsOwnPrefix(t *testing.T) {
	const identity = "system:serviceaccount:dispatch:dispatch"
	prefix := natsgrant.InboxPrefix(identity)
	ping := natsgrant.HubAdmissionPing(identity)
	require.True(t, strings.HasPrefix(ping, prefix+"."), "the ping %q is under the prefix %q", ping, prefix)
	require.Equal(t, natsgrant.Permissions{Publish: []string{ping}, Subscribe: []string{prefix + ".>"}}, natsgrant.HubAdmission(identity))
	_, system := natsgrant.NamesSystemSubject(natsgrant.HubAdmission(identity))
	require.False(t, system)
	_, open := natsgrant.OpenDirection(natsgrant.HubAdmission(identity))
	require.False(t, open)
	require.NotEqual(t, ping, natsgrant.HubAdmissionPing("system:serviceaccount:dispatch:other"), "two accounts share no ping")
}

// Dispatch's listener's grant is the listener's (Listener) without the session consumers' calls,
// written out here, and every entry of it is in Dispatch's grant: the listener grant Dispatch's own
// user is built on. So Dispatch's listener keeps its durable and its role lane and refuses consumer
// delivery, as the listener beside envoy-dispatch does today.
func TestDispatchListenerIsTheListenerWithoutSessionConsumers(t *testing.T) {
	const machineID = "example-dispatch"
	sessionConsumers := []string{
		"$JS.API.CONSUMER.CREATE." + bus.Stream + ".*",
		"$JS.API.CONSUMER.CREATE." + bus.Stream + ".*.>",
		"$JS.API.CONSUMER.INFO." + bus.Stream + ".*",
		"$JS.API.CONSUMER.DELETE." + bus.Stream + ".*",
		"$JS.API.CONSUMER.MSG.NEXT." + bus.Stream + ".*",
		"$JS.API.CONSUMER.LIST." + bus.Stream,
		"$JS.ACK." + bus.Stream + ".*.>",
	}
	listener := natsgrant.Listener(machineID)
	got := natsgrant.DispatchListener(machineID)
	require.Equal(t, natsgrant.Permissions{
		Publish:   slices.DeleteFunc(slices.Clone(listener.Publish), func(entry string) bool { return slices.Contains(sessionConsumers, entry) }),
		Subscribe: listener.Subscribe,
	}, got)
	require.Len(t, got.Publish, len(listener.Publish)-len(sessionConsumers), "every session consumer call is in Listener")
	dispatch := natsgrant.Dispatch(machineID)
	for _, entry := range got.Publish {
		require.Contains(t, dispatch.Publish, entry)
	}
	for _, entry := range got.Subscribe {
		require.Contains(t, dispatch.Subscribe, entry)
	}
}

// Dispatch's app's grant on a hub rendered private is Dispatch's less what only a listener uses:
// the listener durable's info, create and acknowledgements, and the two role-lane queue
// subscriptions, each written out here. Every other entry stays, in Dispatch's order.
func TestDispatchAppIsDispatchLessTheListenersOwn(t *testing.T) {
	const machineID = "example-dispatch"
	durable := bus.ListenerDurable(machineID)
	listenerOnly := []string{
		"$JS.API.CONSUMER.INFO." + bus.Stream + "." + durable,
		"$JS.API.CONSUMER.CREATE." + bus.Stream + "." + durable,
		"$JS.ACK." + bus.Stream + "." + durable + ".>",
	}
	roleLanes := []string{
		contracts.RoleTopicPrefix + "> " + bus.ListenerQueue(machineID),
		contracts.RoleTopicPrefix + "> " + bus.RoleLaneQueue,
	}
	dispatch := natsgrant.Dispatch(machineID)
	got := natsgrant.DispatchApp(machineID)
	require.Equal(t, natsgrant.Permissions{
		Publish:   slices.DeleteFunc(slices.Clone(dispatch.Publish), func(entry string) bool { return slices.Contains(listenerOnly, entry) }),
		Subscribe: slices.DeleteFunc(slices.Clone(dispatch.Subscribe), func(entry string) bool { return slices.Contains(roleLanes, entry) }),
	}, got)
	require.Len(t, got.Publish, len(dispatch.Publish)-len(listenerOnly), "every listener-only call is in Dispatch")
	require.Len(t, got.Subscribe, len(dispatch.Subscribe)-len(roleLanes), "both role lanes are in Dispatch")
	for _, kept := range []string{
		bus.NotificationTopicPrefix + ">",
		"$JS.API.STREAM.CREATE." + bus.Stream,
		"$KV." + bus.RoleClaimBucket + ".>",
		"$KV." + bus.SessionBucket + ".>",
		"_INBOX.>",
	} {
		require.True(t, slices.Contains(got.Publish, kept) || slices.Contains(got.Subscribe, kept), "DispatchApp keeps %s", kept)
	}
}
