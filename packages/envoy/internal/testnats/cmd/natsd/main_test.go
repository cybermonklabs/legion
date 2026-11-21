package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/streamset"
	"github.com/sjawhar/envoy/internal/testnats"
)

// testAdminKey is a fresh admin user's public key.
func testAdminKey(t *testing.T) string {
	t.Helper()
	admin, err := nkeys.CreateUser()
	require.NoError(t, err)
	key, err := admin.PublicKey()
	require.NoError(t, err)
	return key
}

// Behind the callout natsd makes every protected stream of the declared set as its own admin user
// before any client connects, each at the build's configuration, so the dev stack's listener and
// Dispatch, which run beside the auth service and make none, find them; the admin's seed is
// written nowhere, and the configuration names its public key alone.
func TestBehindTheCalloutTheDeclaredStreamsAreMadeAsTheAdmin(t *testing.T) {
	admin, err := nkeys.CreateUser()
	require.NoError(t, err)
	adminKey, err := admin.PublicKey()
	require.NoError(t, err)
	dir := t.TempDir()
	config, err := calloutConfig(dir, adminKey)
	require.NoError(t, err)
	text, err := os.ReadFile(config)
	require.NoError(t, err)
	require.Contains(t, string(text), adminKey)
	seed, err := admin.Seed()
	require.NoError(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		require.NoError(t, err)
		require.NotContains(t, string(raw), string(seed), "%s holds the admin's seed", entry.Name())
	}

	keys := testnats.NewCalloutKeys(t)
	url := testnats.StartWithCallout(t, keys, adminUser(adminKey))
	require.NoError(t, makeStreams(url, admin))
	conn, err := natsgo.Connect(url, natsgo.Nkey(adminKey, admin.Sign))
	require.NoError(t, err)
	defer conn.Close()
	js, err := conn.JetStream()
	require.NoError(t, err)
	made := 0
	for _, stream := range streamset.Declared(streamset.Settings{}) {
		if !stream.Protected || stream.Transient {
			continue
		}
		info, err := js.StreamInfo(stream.Name)
		require.NoError(t, err, "natsd did not make %s", stream.Name)
		var drift []string
		for _, difference := range streamset.Drift(stream, streamset.Info{StreamInfo: *info}) {
			drift = append(drift, difference.String())
		}
		require.Empty(t, drift, "%s was made unlike the build's configuration", stream.Name)
		made++
	}
	require.Equal(t, 9, made, "the declared set's protected streams")
	require.NoError(t, makeStreams(url, admin), "a restart on the same store made them again")
}

// -callout-keys makes the auth callout's three keys once, writes each seed readable by its owner
// alone and the server's configuration beside them, and a restart on the same directory keeps the
// keys a running broker already read.
func TestCalloutKeysAreMadeOnceAndKeptAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	config, err := calloutConfig(dir, testAdminKey(t))
	require.NoError(t, err)
	seeds := map[string]nkeys.PrefixByte{
		"service.seed": nkeys.PrefixByteUser,
		"issuer.seed":  nkeys.PrefixByteAccount,
		"xkey.seed":    nkeys.PrefixByteCurve,
	}
	first := map[string]string{}
	for name, prefix := range seeds {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), name)
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		got, _, err := nkeys.DecodeSeed(raw)
		require.NoError(t, err, name)
		require.Equal(t, prefix, got, name)
		first[name] = string(raw)
	}
	text, err := os.ReadFile(config)
	require.NoError(t, err)
	require.Contains(t, string(text), "auth_callout")

	again, err := calloutConfig(dir, testAdminKey(t))
	require.NoError(t, err)
	require.Equal(t, config, again)
	for name := range seeds {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, first[name], string(raw), "%s changed across a restart", name)
	}

	require.NoError(t, os.Remove(filepath.Join(dir, "issuer.seed")))
	_, err = calloutConfig(dir, testAdminKey(t))
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "issuer.seed"), "the refusal names the missing seed: %v", err)
}
