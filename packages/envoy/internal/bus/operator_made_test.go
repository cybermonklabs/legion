package bus_test

import (
	"context"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// operatorMade is a program's environment beside the auth service: ENVOY_STREAMS_OPERATOR_MADE=1,
// and nothing else set.
func operatorMade(name string) (string, bool) {
	if name == bus.StreamsOperatorMadeEnvVar {
		return "1", true
	}
	return "", false
}

// makeNotificationStream makes ENVOY_NOTIFICATIONS on js as the operator would: the stream's own
// subjects, on file, at one copy.
func makeNotificationStream(t *testing.T, js natsgo.JetStreamContext) {
	t.Helper()
	_, err := js.AddStream(&natsgo.StreamConfig{Name: bus.Stream, Subjects: bus.StreamSubjects(), Storage: natsgo.FileStorage, MaxAge: time.Hour})
	require.NoError(t, err)
}

// Beside the auth service (ENVOY_STREAMS_OPERATOR_MADE=1) a start reads its streams and makes
// none: with ENVOY_NOTIFICATIONS missing, ConnectOwningStream stops naming it and the operator's
// command, and once the operator has made it, a listener start's open of a missing KV_envoy_roles
// stops the same way. The start asks the server for no stream create and no update, and a bucket
// the operator made then opens as it is.
func TestAStartStopsOnAMissingStream(t *testing.T) {
	_, uri := testnats.Start(t)
	admin := testnats.Connect(t, uri)
	t.Cleanup(admin.Close)
	adminJS, err := admin.JetStream()
	require.NoError(t, err)
	writes := testnats.WatchStreamWrites(t, uri)

	_, err = bus.ConnectOwningStream([]string{uri}, bus.WithEnvironment(operatorMade))
	require.ErrorIs(t, err, bus.ErrStreamNotMade, "a start with no notification stream went on")
	require.ErrorContains(t, err, bus.Stream)
	require.ErrorContains(t, err, "envoy-infra streams update")
	require.Empty(t, writes.Take(t), "the start asked for a stream write")
	_, err = adminJS.StreamInfo(bus.Stream)
	require.ErrorIs(t, err, natsgo.ErrStreamNotFound, "the start made the notification stream")

	makeNotificationStream(t, adminJS)
	writes.Take(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithEnvironment(operatorMade))
	require.NoError(t, err, "a start found the stream the operator made and did not go on")
	t.Cleanup(client.Close)

	bucket := &natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage}
	_, err = bus.EnsureKeyValue(t.Context(), client.Conn, client.JS(), bucket)
	require.ErrorIs(t, err, bus.ErrStreamNotMade, "a start with no role bucket went on")
	require.ErrorContains(t, err, "KV_"+bus.RoleClaimBucket)
	require.ErrorContains(t, err, "envoy-infra streams update")
	_, err = adminJS.StreamInfo("KV_" + bus.RoleClaimBucket)
	require.ErrorIs(t, err, natsgo.ErrStreamNotFound, "the start made the role bucket")
	require.Empty(t, writes.Take(t), "the start asked for a stream write")

	made, err := adminJS.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.RoleClaimBucket, Storage: natsgo.FileStorage, TTL: time.Hour})
	require.NoError(t, err)
	_, err = made.Put("role.a", []byte("ses_a"))
	require.NoError(t, err)
	writes.Take(t)
	kv, err := bus.EnsureKeyValue(t.Context(), client.Conn, client.JS(), bucket)
	require.NoError(t, err, "a start did not open the bucket the operator made")
	entry, err := kv.Get("role.a")
	require.NoError(t, err)
	require.Equal(t, "ses_a", string(entry.Value()))
	require.Empty(t, writes.Take(t), "the start asked for a stream write")
	info, err := adminJS.StreamInfo("KV_" + bus.RoleClaimBucket)
	require.NoError(t, err)
	require.Equal(t, time.Hour, info.Config.MaxAge, "the start changed the bucket the operator made")
}

// Beside the auth service the program runs no keeper and sets no bucket's age: a stream whose
// age, duplicate window and discard differ from this binary's, which a keeper puts back within one
// period, is left as the operator made it, and SetKeyValueMaxAge answers the age the bucket
// carries and writes none.
func TestAnOperatorMadeStartKeepsNothing(t *testing.T) {
	_, uri := testnats.Start(t)
	admin := testnats.Connect(t, uri)
	t.Cleanup(admin.Close)
	adminJS, err := admin.JetStream()
	require.NoError(t, err)
	makeNotificationStream(t, adminJS)
	_, err = adminJS.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.SessionBucket, Storage: natsgo.FileStorage, TTL: 5 * time.Minute})
	require.NoError(t, err)
	writes := testnats.WatchStreamWrites(t, uri)

	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithEnvironment(operatorMade),
		bus.WithKeepInterval(10*time.Millisecond), bus.WithKeptBuckets(bus.SessionBucket))
	require.NoError(t, err)
	t.Cleanup(client.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	age, err := bus.SetKeyValueMaxAge(ctx, client.Conn, client.JS(), bus.SessionBucket, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, age, "SetKeyValueMaxAge answered an age the bucket does not carry")
	time.Sleep(50 * 10 * time.Millisecond)
	require.Empty(t, writes.Take(t), "the program wrote a stream the operator made")
	info, err := adminJS.StreamInfo(bus.Stream)
	require.NoError(t, err)
	require.Equal(t, time.Hour, info.Config.MaxAge, "the keeper put back the stream's age")
}

// The setting reads "1" as on, "0", unset or blank as off, and refuses anything else by name,
// before any dial: a "true" read as off would have the start make its streams.
func TestTheOperatorMadeSettingIsReadStrictly(t *testing.T) {
	for raw, want := range map[string]bool{"": false, " ": false, "0": false, "1": true, " 1 ": true} {
		got, err := bus.ParseStreamsOperatorMade(raw)
		require.NoError(t, err, "%q", raw)
		require.Equal(t, want, got, "%q", raw)
	}
	for _, raw := range []string{"true", "yes", "2", "on"} {
		_, err := bus.ParseStreamsOperatorMade(raw)
		require.ErrorContains(t, err, bus.StreamsOperatorMadeEnvVar, "%q", raw)
		require.ErrorContains(t, err, raw, "%q", raw)
	}
	_, err := bus.ConnectOwningStream([]string{"nats://127.0.0.1:1"}, bus.WithEnvironment(func(name string) (string, bool) {
		if name == bus.StreamsOperatorMadeEnvVar {
			return "yes", true
		}
		return "", false
	}))
	require.ErrorContains(t, err, bus.StreamsOperatorMadeEnvVar)
}
