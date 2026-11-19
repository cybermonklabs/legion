package pulumitest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMain runs the package's tests under Main, as a package whose tests use NewCLI must, so the
// PULUMI_HOME NewCLI's test makes is removed.
func TestMain(m *testing.M) { os.Exit(Main(m)) }

// The live programs pin DockerPluginVersion, so a tarball of another version would leave the CLI
// downloading the pinned one unchecked. newHome refuses it by its name before it runs anything.
func TestNewHomeRefusesATarballOfAnotherVersion(t *testing.T) {
	t.Setenv(DockerPluginEnv, "/tmp/pulumi-resource-docker-v4.10.0-linux-amd64.tar.gz")
	dir, err := newHome()
	require.Empty(t, dir, "no PULUMI_HOME made")
	require.ErrorContains(t, err, DockerPluginEnv+"=/tmp/pulumi-resource-docker-v4.10.0-linux-amd64.tar.gz")
	require.ErrorContains(t, err, "v"+DockerPluginVersion)
}

// A CLI runs pulumi with no update check, the Pulumi Cloud API at a local port that refuses, and a
// file backend in a temporary directory of the test's own, whatever the caller's environment sets
// for each: so no command of a live test asks api.pulumi.com anything (package add's registry
// lookup under an AI coding agent's variable included), and none reads or writes a backend another
// test or run uses. Each row runs a command of a CLI through a stand-in pulumi first on PATH,
// which records the environment it was started with, and reads each setting whole from that
// record, under a caller's environment that names Pulumi Cloud for all three.
func TestNewCLIRunsPulumiWithNoCloudToReach(t *testing.T) {
	bin, record := t.TempDir(), filepath.Join(t.TempDir(), "env")
	// The stand-in answers the install of the docker plugin, which NewCLI's first call makes into
	// the test binary's PULUMI_HOME, and records the environment of every other command.
	script := "#!/bin/sh\nif [ \"$1\" = plugin ]; then exit 0; fi\nenv > \"$PULUMITEST_STANDIN_ENV\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "pulumi"), []byte(script), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PULUMITEST_STANDIN_ENV", record)
	t.Setenv(DockerPluginEnv, filepath.Join(t.TempDir(), dockerPluginTarball))
	t.Setenv("PULUMI_API", "https://api.pulumi.com")
	t.Setenv("PULUMI_SKIP_UPDATE_CHECK", "false")
	t.Setenv("PULUMI_BACKEND_URL", "https://api.pulumi.com")

	var backends []string
	for range 2 {
		var backend string
		t.Run("a CLI's command", func(t *testing.T) {
			NewCLI(t, t.TempDir()).Run("version")
			b, err := os.ReadFile(record)
			require.NoError(t, err)
			env := map[string][]string{}
			for line := range strings.Lines(string(b)) {
				name, value, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
				env[name] = append(env[name], value)
			}
			require.Equal(t, []string{"http://127.0.0.1:1"}, env["PULUMI_API"], "PULUMI_API")
			require.Equal(t, []string{"true"}, env["PULUMI_SKIP_UPDATE_CHECK"], "PULUMI_SKIP_UPDATE_CHECK")
			require.Len(t, env["PULUMI_BACKEND_URL"], 1, "PULUMI_BACKEND_URL")
			backend = env["PULUMI_BACKEND_URL"][0]
			dir, ok := strings.CutPrefix(backend, "file://")
			require.True(t, ok && filepath.IsAbs(dir), "PULUMI_BACKEND_URL=%s, want file:// and an absolute directory", backend)
			info, err := os.Stat(dir)
			require.NoError(t, err, "the backend's directory")
			require.True(t, info.IsDir(), "the backend %s is a directory", dir)
		})
		dir := strings.TrimPrefix(backend, "file://")
		_, err := os.Stat(dir)
		require.ErrorIs(t, err, fs.ErrNotExist, "the backend %s is a temporary directory of the test's, removed when it ends", dir)
		backends = append(backends, backend)
	}
	require.NotEqual(t, backends[0], backends[1], "each CLI has a backend of its own")
}
