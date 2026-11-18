package core_test

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testwait"
)

// runStartScript runs script, a StartScript, with a stub nats-server first on PATH, so the script
// writes its configuration and its exec ends at once.
func runStartScript(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "nats-server"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "the start script: %s", out)
}

// startFromScript runs script (runStartScript), which writes its configuration to configPath, then
// starts nats-server in this process on that file and serverArgs, through server.ConfigureOptions,
// the parse nats-server's own main runs, so the file and the flags merge as they do in a task, on
// a random client port, since the box's 4222 may be another program's. It returns the address the
// monitoring listener bound.
func startFromScript(t *testing.T, script, configPath string, serverArgs []string) *net.TCPAddr {
	t.Helper()
	runStartScript(t, script)

	fs := flag.NewFlagSet("nats-server", flag.ContinueOnError)
	opts, err := natsserver.ConfigureOptions(fs, append([]string{"--config", configPath}, serverArgs...), nil, nil, nil)
	require.NoError(t, err)
	opts.NoSigs, opts.Port = true, natsserver.RANDOM_PORT
	s, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	go s.Start()
	t.Cleanup(s.Shutdown)
	testwait.Until(t, "nats-server to accept connections", func() (string, bool) {
		return "", s.ReadyForConnections(100 * time.Millisecond)
	})
	addr := s.MonitorAddr()
	require.NotNil(t, addr, "nats-server serves no monitoring")
	return addr
}

// outsideAddress is an IPv4 address of this machine other than loopback: where another host, or
// another container of a task, reaches a listener bound to every address.
func outsideAddress(t *testing.T) net.IP {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP
		}
	}
	require.FailNow(t, "this machine has no IPv4 address but loopback, so no test can tell a loopback listener from one on every address")
	return nil
}

// monitorHealthz reports whether GET /healthz at host:port answers 200 within ctx.
func monitorHealthz(ctx context.Context, host net.IP, port int) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host.String(), fmt.Sprint(port))), nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK, nil
}

// unanswered is how long the read the private start script's monitoring must not answer waits for
// an answer before it counts as none: a listener on loopback alone refuses the outside address at
// once, and a dropped connection is no answer either, so this bound can only make that row pass.
const unanswered = 2 * time.Second

// monitorArgs are server arguments that open nats-server's monitoring on a free port, as a
// component's serverArgs open it on 8222 for its health check (aws/rendering/nats.go:50-51).
var monitorArgs = []string{"--http_port", "-1"}

// TestAPrivateStartScriptServesMonitoringOnLoopbackAlone: nats-server's monitoring answers
// /connz, /subsz and /jsz with no authentication, and they name every client's subjects and
// consumers, a private project's among them. So beside privateProjects the start script's
// configuration binds the monitoring listener to loopback: whatever port serverArgs open it on,
// a health check in the task reads it on 127.0.0.1, and nothing outside the task reaches it, what
// ever the task's security group admits.
//
// The control, TestAStartScriptWithoutPrivateProjectsServesMonitoringAsBefore, runs the same
// arguments without privateProjects: the listener is on every address, as before, so the outside
// address reaches it. It shows this test's outside read can succeed.
func TestAPrivateStartScriptServesMonitoringOnLoopbackAlone(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "nats-server.conf")
	script, err := privateOnly().StartScript(nil, monitorArgs, configPath)
	require.NoError(t, err)
	require.Contains(t, script, "\nhttp: \"127.0.0.1:0\"\n")
	addr := startFromScript(t, script, configPath, monitorArgs)
	require.True(t, addr.IP.IsLoopback(), "monitoring is bound to %s", addr)

	ok, err := monitorHealthz(testwait.Context(t), net.IPv4(127, 0, 0, 1), addr.Port)
	require.NoError(t, err)
	require.True(t, ok, "the task's health check reads monitoring on loopback")
	outside := outsideAddress(t)
	outsideRead, cancel := context.WithTimeout(testwait.Context(t), unanswered)
	defer cancel()
	_, err = monitorHealthz(outsideRead, outside, addr.Port)
	require.Error(t, err, "monitoring answered at %s, outside loopback", outside)
}

// TestAStartScriptWithoutPrivateProjectsServesMonitoringAsBefore is the control above, and R9's
// row for the start script: without privateProjects the configuration sets no monitoring host,
// so the listener is on every address, as before.
func TestAStartScriptWithoutPrivateProjectsServesMonitoringAsBefore(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "nats-server.conf")
	script, err := withCallout().StartScript(nil, monitorArgs, configPath)
	require.NoError(t, err)
	require.NotContains(t, script, "http:")
	addr := startFromScript(t, script, configPath, monitorArgs)
	ok, err := monitorHealthz(testwait.Context(t), outsideAddress(t), addr.Port)
	require.NoError(t, err)
	require.True(t, ok, "monitoring on every address answers at the outside address")
}

// Beside privateProjects, with no monitoring flag in serverArgs, the loopback line opens no
// listener: its port, 0, is nats-server's "none", and a flag's port takes its place.
func TestAPrivateStartScriptOpensNoMonitoringOfItsOwn(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "nats-server.conf")
	script, err := privateOnly().StartScript(nil, nil, configPath)
	require.NoError(t, err)
	runStartScript(t, script)
	opts, err := natsserver.ProcessConfigFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", opts.HTTPHost)
	require.Zero(t, opts.HTTPPort)
	require.False(t, strings.Contains(script, "--http_port"))
}
