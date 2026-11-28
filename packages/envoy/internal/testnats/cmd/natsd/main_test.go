package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"
)

// -callout-keys makes the auth callout's three keys once, writes each seed readable by its owner
// alone and the server's configuration beside them, and a restart on the same directory keeps the
// keys a running broker already read.
func TestCalloutKeysAreMadeOnceAndKeptAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	config, err := calloutConfig(dir)
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

	again, err := calloutConfig(dir)
	require.NoError(t, err)
	require.Equal(t, config, again)
	for name := range seeds {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Equal(t, first[name], string(raw), "%s changed across a restart", name)
	}

	require.NoError(t, os.Remove(filepath.Join(dir, "issuer.seed")))
	_, err = calloutConfig(dir)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "issuer.seed"), "the refusal names the missing seed: %v", err)
}
