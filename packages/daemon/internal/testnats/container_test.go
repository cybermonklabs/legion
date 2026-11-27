package testnats

import (
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture dials a published port on the machine of the daemon at the endpoint it reaches. A
// daemon on this machine is dialled at loopback; one on another machine is dialled at its host.
func TestTheDaemonHostFollowsTheEndpointsMachine(t *testing.T) {
	local := dockerDaemon{dial: "127.0.0.1"}
	for _, tc := range []struct {
		name, endpoint string
		want           dockerDaemon
		address        string
	}{
		{"the default socket", "unix:///var/run/docker.sock", local, "127.0.0.1:32768"},
		{"a rootless socket", "unix:///run/user/1000/docker.sock", local, "127.0.0.1:32768"},
		{"a named pipe", "npipe:////./pipe/docker_engine", local, "127.0.0.1:32768"},
		{"tcp to another machine", "tcp://10.1.2.3:2376", dockerDaemon{dial: "10.1.2.3"}, "10.1.2.3:32768"},
		{"ssh to another machine", "ssh://builder@docker.example:22", dockerDaemon{dial: "docker.example"}, "docker.example:32768"},
		{"an IPv6 host", "tcp://[fd00::1]:2376", dockerDaemon{dial: "fd00::1"}, "[fd00::1]:32768"},
		{"localhost over tcp", "tcp://localhost:2375", local, "127.0.0.1:32768"},
		{"a loopback address", "tcp://127.0.0.1:2375", local, "127.0.0.1:32768"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := daemonHost(tc.endpoint)
			if err != nil {
				t.Fatalf("daemonHost(%q) = %v", tc.endpoint, err)
			}
			if got != tc.want {
				t.Errorf("daemonHost(%q) = %+v, want %+v", tc.endpoint, got, tc.want)
			}
			if address := net.JoinHostPort(got.dial, "32768"); address != tc.address {
				t.Errorf("address = %q, want %q", address, tc.address)
			}
		})
	}
	for _, tc := range []struct{ name, endpoint, want string }{
		{"an endpoint with no host", "tcp://:2375", "names no host"},
		{"an endpoint of no scheme the CLI takes", "fd://", "is not unix://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := daemonHost(tc.endpoint); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("daemonHost(%q) = %v, want an error containing %q", tc.endpoint, err, tc.want)
			}
		})
	}
}

// TESTCONTAINERS_HOST_OVERRIDE names the host a published port is dialled at, as testcontainers-go
// reads it, whatever machine the endpoint names: a developer container on the host's socket sets
// it to the host as the container reaches it (the bridge gateway, host.docker.internal). Empty,
// it changes nothing.
func TestTheHostOverrideNamesTheHostAPublishedPortIsDialledAt(t *testing.T) {
	local := dockerDaemon{dial: "127.0.0.1"}
	remote := dockerDaemon{dial: "10.1.2.3"}
	for _, tc := range []struct {
		name, override string
		daemon, want   dockerDaemon
		address        string
	}{
		{"the bridge gateway over the host's socket", "172.17.0.1", local, dockerDaemon{dial: "172.17.0.1"}, "172.17.0.1:32768"},
		{"a host name over the host's socket", "host.docker.internal", local, dockerDaemon{dial: "host.docker.internal"}, "host.docker.internal:32768"},
		{"an IPv6 host over the host's socket", "fd00::1", local, dockerDaemon{dial: "fd00::1"}, "[fd00::1]:32768"},
		{"another machine's daemon", "builder.example", remote, dockerDaemon{dial: "builder.example"}, "builder.example:32768"},
		{"localhost", "localhost", local, dockerDaemon{dial: "localhost"}, "localhost:32768"},
		{"a loopback address", "127.0.0.1", remote, local, "127.0.0.1:32768"},
		{"no override", "", local, local, "127.0.0.1:32768"},
		{"no override of another machine's daemon", "", remote, remote, "10.1.2.3:32768"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withHostOverride(tc.daemon, tc.override)
			if got != tc.want {
				t.Errorf("withHostOverride(%+v, %q) = %+v, want %+v", tc.daemon, tc.override, got, tc.want)
			}
			if address := net.JoinHostPort(got.dial, "32768"); address != tc.address {
				t.Errorf("address = %q, want %q", address, tc.address)
			}
		})
	}
}

// The endpoint the fixture reaches is the one the docker CLI resolves, under every setting that
// chooses it: DOCKER_HOST, DOCKER_CONTEXT, both at once in either local/remote combination, and
// the configuration's current context. Each row sets them, asks the CLI which context it resolves
// (`docker context show`) and that context's endpoint (`docker context inspect <name>`), and holds
// cliEndpoint to the answer. The contexts live in a DOCKER_CONFIG of the test's own, so the user's
// configuration is neither read nor changed, and no daemon is dialled. The rows must reach at
// least three different endpoints, or they would show nothing about which setting wins. A machine
// with no docker CLI runs no fixture container at all, so the test skips there.
func TestTheFixtureReachesTheEndpointTheDockerCLIResolves(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("no docker CLI on PATH (%v), and the fixture starts its containers through one", err)
	}
	const (
		localSocket  = "unix:///var/run/docker.sock"
		otherSocket  = "unix:///run/user/1000/docker.sock"
		remoteHost   = "tcp://10.1.2.3:2376"
		remoteServer = "tcp://10.9.8.7:2376"
	)
	t.Setenv("DOCKER_CONFIG", filepath.Join(t.TempDir(), "docker"))
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	for name, host := range map[string]string{"remote": remoteServer, "local": otherSocket} {
		if _, err := docker(t.Context(), nil, "context", "create", name, "--docker", "host="+host); err != nil {
			t.Fatalf("create the test's docker context %s: %v", name, err)
		}
	}
	reached := map[string]bool{}
	for _, tc := range []struct {
		name, dockerHost, dockerContext, current string
	}{
		{"neither variable, the default context", "", "", ""},
		{"neither variable, a current context of the configuration", "", "", "remote"},
		{"DOCKER_HOST alone", remoteHost, "", ""},
		{"DOCKER_CONTEXT alone", "", "remote", ""},
		{"a local DOCKER_HOST beside a remote DOCKER_CONTEXT", localSocket, "remote", ""},
		{"a remote DOCKER_HOST beside a local DOCKER_CONTEXT", remoteHost, "local", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := tc.current
			if current == "" {
				current = "default"
			}
			if _, err := docker(t.Context(), nil, "context", "use", current); err != nil {
				t.Fatalf("docker context use %s: %v", current, err)
			}
			t.Setenv("DOCKER_HOST", tc.dockerHost)
			t.Setenv("DOCKER_CONTEXT", tc.dockerContext)
			resolved, err := docker(t.Context(), nil, "context", "show")
			if err != nil {
				t.Fatalf("docker context show: %v", err)
			}
			want, err := docker(t.Context(), nil, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}", resolved)
			if err != nil {
				t.Fatalf("docker context inspect %s: %v", resolved, err)
			}
			got, err := cliEndpoint(t.Context())
			if err != nil {
				t.Fatalf("cliEndpoint = %v", err)
			}
			if got != want {
				t.Errorf("the fixture reaches %s, and the docker CLI resolves context %s at %s", got, resolved, want)
			}
			reached[want] = true
		})
	}
	if len(reached) < 3 {
		t.Fatalf("the rows reached %d endpoint(s), %v: too few to show which setting wins", len(reached), reached)
	}
}
