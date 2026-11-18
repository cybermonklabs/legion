package routes_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/routes"
)

// opener opens h for the target it was given, and records that target.
func opener(h *fakeHub, opened *routes.Target) routes.Opener {
	return func(target routes.Target) (routes.Source, error) {
		*opened = target
		return h, nil
	}
}

// runRoutes runs `envoy-infra routes` with args over h.
func runRoutes(h *fakeHub, args ...string) (int, string, string, routes.Target) {
	var stdout, stderr bytes.Buffer
	var opened routes.Target
	code := routes.Command(context.Background(), args, &stdout, &stderr, opener(h, &opened))
	return code, stdout.String(), stderr.String(), opened
}

// `routes record` writes the record of a finished roll and `routes check` reads it back: exit 0
// for the clean hub, 1 once another process held a route, 2 once a pod cannot be read, each with
// its verdict's line and the findings above it. Each run opens the hub named by its flags and
// closes it. A record of another hub is refused.
func TestRecordThenCheck(t *testing.T) {
	h := cleanHub()
	h.atEnv()
	out := filepath.Join(t.TempDir(), "routes.json")
	code, stdout, stderr, opened := runRoutes(h, "record", "--namespace", "envoy", "--statefulset", "envoy-nats", "--out", out,
		"--context", "example-context", "--kubectl", "/usr/local/bin/kubectl")
	require.Equal(t, 0, code, "stderr %q", stderr)
	require.Contains(t, stdout, "recorded the 3 pods of envoy/envoy-nats at ")
	require.Equal(t, routes.Target{Namespace: "envoy", StatefulSet: "envoy-nats", Kubectl: "/usr/local/bin/kubectl", Context: "example-context", RoutePort: 6222}, opened)
	require.True(t, h.closed, "record closed the hub it opened")
	body, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(body), `"uid": "uid-envoy-nats-2"`)

	check := func(h *fakeHub) (int, string) {
		t.Helper()
		code, stdout, stderr, opened := runRoutes(h, "check", "--namespace", "envoy", "--statefulset", "envoy-nats", "--record", out)
		require.Equal(t, 8222, opened.MonitorPort, "the monitoring port by default")
		require.Equal(t, 6222, opened.RoutePort, "the route port by default")
		require.True(t, h.closed, "check closed the hub it opened")
		require.Empty(t, stderr)
		return code, stdout
	}
	code, stdout = check(cleanHub())
	require.Equal(t, 0, code, "stdout %q", stdout)
	require.Contains(t, stdout, "clean: authorization is in force on the 3 pods of envoy/envoy-nats, every route joins two of the hub's servers")

	impostor := cleanHub()
	impostor.routes["envoy-nats-1"] = append(impostor.routes["envoy-nats-1"], routes.Route{RemoteID: "NIMPOSTOR", RemoteName: "envoy-nats-2", IP: "10.0.0.12"})
	code, stdout = check(impostor)
	require.Equal(t, 1, code, "stdout %q", stdout)
	require.Contains(t, stdout, `envoy-nats-1: password read: has a route to a server that is none of the hub's others: ip 10.0.0.12, remote_id "NIMPOSTOR"`)
	require.Contains(t, stdout, "password read: treat the route password as read")

	refused := cleanHub()
	refused.refused["envoy-nats-0 log"] = errors.New("forbidden")
	code, stdout = check(refused)
	require.Equal(t, 2, code, "stdout %q", stdout)
	require.Contains(t, stdout, "envoy-nats-0: unknown: its nats container's log could not be read: forbidden")
	require.Contains(t, stdout, "unknown: the check could not read all it needs")

	code, _, stderr, _ = runRoutes(cleanHub(), "check", "--namespace", "envoy", "--statefulset", "other", "--record", out)
	require.Equal(t, 2, code)
	require.Contains(t, stderr, "is the record of envoy/envoy-nats, not of envoy/other")
}

// A record of a roll that has not finished is not written, and record exits 2; nor is one taken
// once a pod's route port requires authorization.
func TestRecordRefusesAnUnfinishedRoll(t *testing.T) {
	h := cleanHub()
	h.atEnv()
	h.pods[2].Ready = false
	out := filepath.Join(t.TempDir(), "routes.json")
	code, _, stderr, _ := runRoutes(h, "record", "--namespace", "envoy", "--statefulset", "envoy-nats", "--out", out)
	require.Equal(t, 2, code)
	require.Contains(t, stderr, "not recorded: envoy-nats-2 not Ready")
	require.NoFileExists(t, out)

	code, _, stderr, _ = runRoutes(cleanHub(), "record", "--namespace", "envoy", "--statefulset", "envoy-nats", "--out", out)
	require.Equal(t, 2, code)
	require.Contains(t, stderr, "not recorded: authorization is already in force on envoy-nats-0, envoy-nats-1, envoy-nats-2")
	require.NoFileExists(t, out)
}

// The command's usage: a missing subcommand or flag is a usage error, exit 2, and so is a word or
// flag it does not take, which it does not echo, as no word given to envoy-infra is echoed; --help
// is the usage alone, exit 0. None opens the hub.
func TestRoutesUsage(t *testing.T) {
	const canary = "SUCANARYNOTASEED"
	for _, row := range []struct {
		args    []string
		code    int
		problem string
	}{
		{nil, 2, "envoy-infra routes: the subcommand is record or check\n"},
		{[]string{""}, 2, "envoy-infra routes: the subcommand is record or check\n"},
		{[]string{canary}, 2, "envoy-infra routes: the subcommand is record or check\n"},
		{[]string{"--help"}, 0, ""},
		{[]string{"check", "--help"}, 0, ""},
		{[]string{"record", "--namespace", "envoy"}, 2, "--namespace and --statefulset are required"},
		{[]string{"record", "--namespace", "envoy", "--statefulset", "envoy-nats"}, 2, "--out is required"},
		{[]string{"check", "--namespace", "envoy", "--statefulset", "envoy-nats"}, 2, "--record is required"},
		{[]string{"check", "--namespace", "envoy", "--statefulset", "envoy-nats", "--record", "r", "--monitor-port", "0"}, 2, "--monitor-port is a port"},
		{[]string{"check", "--namespace", "envoy", "--statefulset", "envoy-nats", "--record", "r", "--route-port", "65536"}, 2, "--route-port is a port"},
		{[]string{"check", "--" + canary, "x"}, 2, "a flag it does not take, or a value it cannot parse"},
		{[]string{"record", "--namespace", "envoy", "--statefulset", "envoy-nats", "--out", "o", canary}, 2, "it takes no words after its flags"},
	} {
		t.Run(strings.Join(row.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := routes.Command(context.Background(), row.args, &stdout, &stderr, func(routes.Target) (routes.Source, error) {
				t.Fatal("the usage opened the hub")
				return nil, nil
			})
			require.Equal(t, row.code, code, "stderr %q", stderr.String())
			require.Contains(t, stderr.String(), "usage: envoy-infra routes record --namespace <ns> --statefulset <name> --out <file>")
			require.Contains(t, stderr.String(), row.problem)
			require.NotContains(t, stderr.String()+stdout.String(), canary)
		})
	}
}
