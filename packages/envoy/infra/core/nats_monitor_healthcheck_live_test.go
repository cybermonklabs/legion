//go:build natslive

package core_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/aws/rendering"
	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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
// health check command in the container, as ECS does. It fails as UNKNOWN when the container's
// loopback has no IPv6 address: there `localhost` names 127.0.0.1 alone, and no run can tell a
// health check that reads only 127.0.0.1 from one that reads either loopback.
//
// The container runs on Docker's default bridge network and publishes no port, so its loopback
// and the 8222 healthCheckArgs open are its own, as a task's are, and Docker chooses no host port
// for it (docs/testing.md, "A container a test starts publishes no port"). The test reaches it
// through docker exec alone. testnats.StartImageConfig runs its server on the host's network on
// ports the kernel chooses, where 8222 and the loopback would be the machine's, so this test
// drives the docker CLI itself, as tests3 does for the same reason.
func runTaskHealthCheck(t *testing.T, config, command string) (int, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "nats-server.conf")
	require.NoError(t, os.WriteFile(file, []byte(config), 0o644))
	// One CPU and 512 MiB, as every container the repository's tests start is capped on a shared
	// host: the task's server holds no stream here.
	id := strings.TrimSpace(dockerCLI(t, append([]string{"create", "--cpus", "1", "--memory", "512m",
		testnats.HubImage, "-c", "/etc/nats/nats-server.conf"}, healthCheckArgs...)...))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", id).Run() })
	dockerCLI(t, "cp", file, id+":/etc/nats/nats-server.conf")
	dockerCLI(t, "start", id)

	run := func(cmd string) (int, string) {
		out, err := exec.Command("docker", "exec", id, "/bin/sh", "-c", cmd).CombinedOutput()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0, strings.TrimSpace(string(out))
		case errors.As(err, &exit):
			return exit.ExitCode(), strings.TrimSpace(string(out))
		}
		t.Fatalf("docker exec %s: %v", cmd, err)
		return 0, ""
	}
	code, v6 := run("cat /proc/sys/net/ipv6/conf/lo/disable_ipv6")
	require.Equal(t, 0, code, "read the container's loopback IPv6 setting: %s", v6)
	require.True(t, strings.HasSuffix(v6, "0"), "UNKNOWN: this container's loopback has no IPv6 (disable_ipv6 %q), so localhost names 127.0.0.1 alone and the run cannot see the defect", v6)
	testwait.Eventually(t, "monitoring to answer on 127.0.0.1:8222", func() bool {
		code, _ := run("wget -qO- http://127.0.0.1:8222/healthz")
		return code == 0
	})
	return run(command)
}

// dockerCLI runs the docker CLI with args and returns what it printed on stdout, failing the test
// with what it printed on stderr when it fails.
func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, exit.Stderr)
	}
	require.NoError(t, err, "docker %s", strings.Join(args, " "))
	return string(out)
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
