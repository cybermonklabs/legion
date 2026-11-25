//go:build natslive

package core_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/aws/rendering"
	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/testnats"
)

// healthCheckArgs are the server arguments the README gives aws:NatsService, the store left out: the
// task's health check reads monitoring on 8222, which --http_port opens.
var healthCheckArgs = []string{"-js", "--http_port", "8222"}

// taskHealthCheck is the NATS container's health check command as aws:NatsService registers it in
// the task definition for a, and the configuration its start script writes. ECS runs a CMD-SHELL
// health check with /bin/sh -c in the container.
func taskHealthCheck(t *testing.T, a core.Authorization) (command, config string) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "nats-server.conf")
	launch, err := rendering.NatsLaunch(&a, nil, healthCheckArgs, configPath)
	require.NoError(t, err)
	var defs []map[string]any
	require.NoError(t, json.Unmarshal([]byte(rendering.NatsContainerDefinitions(rendering.NatsContainers{
		NatsImage: testnats.HubImage, Launch: launch, TailscaleImage: "example.registry/tailscale:example",
		TailnetHostname: "example-nats", TailnetTags: []string{"tag:example"}, TailnetSecretArn: "arn:example:secret",
		LogGroup: "example-log-group", Region: "example-region",
	})), &defs))
	nats := defs[0]
	check := nats["healthCheck"].(map[string]any)["command"].([]any)
	require.Equal(t, "CMD-SHELL", check[0])
	script := nats["command"].([]any)[0].(string)

	// Run the start script with a stub nats-server first on PATH: it writes the configuration the
	// task's server reads and its exec ends at once.
	runStartScript(t, script)
	written, err := os.ReadFile(configPath)
	require.NoError(t, err)
	return check[1].(string), string(written)
}

// runTaskHealthCheck runs the hub's image on config with healthCheckArgs, as the task's start
// script execs nats-server, waits until monitoring answers on 127.0.0.1, and then runs the task's
// health check command in the container, as ECS does. The container starts on config itself,
// with no restart (testnats.StartImageReady says why): in envoy-infra-go a restart of it once
// failed, "address already in use". It fails as UNKNOWN when the container's loopback has no IPv6
// address: there `localhost` names 127.0.0.1 alone, and no run can tell a health check that reads
// only 127.0.0.1 from one that reads either loopback.
func runTaskHealthCheck(t *testing.T, config, command string) (int, string) {
	t.Helper()
	ctx := context.Background()
	ctr, _ := testnats.StartImageReady(t, testnats.HubImage, config, healthCheckArgs)

	run := func(cmd string) (int, string) {
		code, r, err := ctr.Exec(ctx, []string{"/bin/sh", "-c", cmd})
		require.NoError(t, err)
		out, err := io.ReadAll(r)
		require.NoError(t, err)
		return code, strings.TrimSpace(string(out))
	}
	code, v6 := run("cat /proc/sys/net/ipv6/conf/lo/disable_ipv6")
	require.Equal(t, 0, code, "read the container's loopback IPv6 setting: %s", v6)
	require.True(t, strings.HasSuffix(v6, "0"), "UNKNOWN: this container's loopback has no IPv6 (disable_ipv6 %q), so localhost names 127.0.0.1 alone and the run cannot see the defect", v6)
	require.Eventually(t, func() bool {
		code, _ := run("wget -qO- http://127.0.0.1:8222/healthz")
		return code == 0
	}, 30*time.Second, 200*time.Millisecond, "monitoring never answered on 127.0.0.1:8222")
	return run(command)
}

// TestThePrivateTasksHealthCheckPasses (found by the tester of #4118 at ca09cdf594): beside
// privateProjects the start script binds monitoring to 127.0.0.1, and aws:NatsService's container
// health check, `wget -qO- http://localhost:8222/healthz`, must still pass in the task. On the hub's
// image (nats:2.15.1-alpine) busybox wget takes ::1 for localhost when the loopback has IPv6, the
// Linux default, and finds nothing listening there, so ECS would take the container for unhealthy.
func TestThePrivateTasksHealthCheckPasses(t *testing.T) {
	a := privateOnly()
	command, config := taskHealthCheck(t, a)
	require.Contains(t, config, "\nhttp: \"127.0.0.1:0\"\n", "the private start script binds monitoring to loopback")
	code, out := runTaskHealthCheck(t, config, command)
	require.Equal(t, 0, code, "the task's health check %q failed in the private task: %s", command, out)
}

// TestThePublicTasksHealthCheckPasses is the control, which shows the run above can pass: the same
// task without privateProjects, whose monitoring is on every address, passes the same health check.
func TestThePublicTasksHealthCheckPasses(t *testing.T) {
	a := privateOnly()
	a.PrivateProjects = false
	command, config := taskHealthCheck(t, a)
	require.NotContains(t, config, "http:")
	code, out := runTaskHealthCheck(t, config, command)
	require.Equal(t, 0, code, "the task's health check %q failed in the public task: %s", command, out)
}
