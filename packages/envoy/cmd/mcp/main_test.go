package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/heldprocess"
	"github.com/sjawhar/envoy/internal/mcpbridge"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// runAsMain is the variable that makes this test binary run envoy-mcp's main instead of its
// tests, so a test can start envoy-mcp as a process of its own and signal it.
const runAsMain = "ENVOY_MCP_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runAsMain) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// standIn is a stand-in MCP server that never reads its standard input after the handshake, so
// the end of that input does not end it: only a kill does (heldprocess.Script, which records its
// pid in pidFile and becomes `sleep <seconds>`). An answering stand-in answers the bridge's
// handshake first, the initialize call that is each server's first request, so the bridge takes
// it as started; one that does not answer holds the bridge in that handshake's wait.
type standIn struct {
	pidFile, seconds string
	answers          bool
}

// script is the stand-in's shell script.
func (s standIn) script() string {
	handshake := ""
	if s.answers {
		handshake = "read -r request\nprintf '%s\\n' '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}'\n"
	}
	return "#!/bin/sh\n" + handshake + heldprocess.Script(s.pidFile, s.seconds)
}

// TestATerminationDuringTheBridgesStartStopsTheServersItStarted: a SIGTERM sent to envoy-mcp
// alone - a container's stop, a `kill` - while the bridge waits on a server's handshake ends
// envoy-mcp within the stop's own bound (mcpbridge.StopGrace), not the 30 s the handshake's call
// may wait, and every server it started is gone with it: the one still starting, and each that had
// started before it and ignores the end of its input, which the bridge stops at the same time, so
// three of them take one StopGrace rather than three. Before, the watch was armed only once every
// server had started, so the signal met its default action: envoy-mcp died at once and the server
// ran on, its input closed and nobody waiting for it; and the stop that followed the start stopped
// its servers one after another, StopGrace each. A SIGHUP and a SIGQUIT sent to envoy-mcp's pid
// alone stop it the same way (shutdownSignals), where before the SIGHUP's default action, and the
// SIGQUIT's goroutine dump, ended it with the server running. And envoy-mcp started with SIGHUP
// ignored, as `nohup` starts it, still ignores it once its watch is armed, so a closing terminal
// leaves it running, and a SIGTERM still stops it.
func TestATerminationDuringTheBridgesStartStopsTheServersItStarted(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/cmdline"); err != nil {
		t.Fatalf("/proc does not answer (%v): this test reads from it whether the stand-in servers still run", err)
	}
	_, natsURL := testnats.Start(t)
	for _, row := range []struct {
		name string
		// started are the seconds of the stand-ins that answer the handshake and then ignore their
		// input; held is the last server's, which never answers.
		started []string
		held    string
		// signal is what the row sends envoy-mcp's pid once the bridge waits on the held server;
		// ignored, where set, is a signal envoy-mcp is started with ignored.
		signal  syscall.Signal
		ignored string
	}{
		{name: "the one server still starting", held: "289", signal: syscall.SIGTERM},
		{name: "three started servers and one still starting", started: []string{"271", "272", "273"}, held: "274", signal: syscall.SIGTERM},
		{name: "the one server still starting, on a SIGHUP", held: "284", signal: syscall.SIGHUP},
		{name: "the one server still starting, on a SIGQUIT", held: "285", signal: syscall.SIGQUIT},
		{name: "the one server still starting, started ignoring SIGHUP", held: "270", signal: syscall.SIGTERM, ignored: "HUP"},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			var standIns []standIn
			for _, seconds := range row.started {
				standIns = append(standIns, standIn{pidFile: filepath.Join(dir, seconds+".pid"), seconds: seconds, answers: true})
			}
			standIns = append(standIns, standIn{pidFile: filepath.Join(dir, row.held+".pid"), seconds: row.held})
			var servers []map[string]any
			for i, s := range standIns {
				heldprocess.KillOnCleanup(t, s.pidFile, s.seconds)
				stub := filepath.Join(dir, "mcp-server-"+s.seconds)
				if err := os.WriteFile(stub, []byte(s.script()), 0o700); err != nil {
					t.Fatal(err)
				}
				servers = append(servers, map[string]any{
					"name": fmt.Sprintf("stand-in-%d", i), "command": []string{stub}, "source": "test",
					"topic_template": "test.{id}", "uri_pattern": "test://(?P<id>.+)",
				})
			}
			config, err := json.Marshal(map[string]any{"servers": servers})
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(dir, "mcp.json")
			if err := os.WriteFile(configPath, config, 0o600); err != nil {
				t.Fatal(err)
			}
			printed, err := os.Create(filepath.Join(dir, "printed"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { printed.Close() })
			envoyMCP := exec.Command(os.Args[0], "-test.run=^$")
			if row.ignored != "" {
				// `sh -c 'trap "" HUP; exec …'` is what nohup does to the command it runs: envoy-mcp
				// starts in the process started here, with the signal ignored.
				envoyMCP = exec.Command("/bin/sh", "-c", "trap '' "+row.ignored+`; exec "$@"`, "sh", os.Args[0], "-test.run=^$")
			}
			envoyMCP.Env = append(os.Environ(), runAsMain+"=1", "ENVOY_MCP_CONFIG="+configPath,
				"ENVOY_MACHINE_ID=test-machine", "NATS_URLS="+natsURL)
			envoyMCP.Stdout, envoyMCP.Stderr = printed, printed
			if err := envoyMCP.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan struct{})
			go func() {
				_ = envoyMCP.Wait()
				close(exited)
			}()
			t.Cleanup(func() {
				select {
				case <-exited:
				default:
					_ = envoyMCP.Process.Kill()
					<-exited
				}
			})
			// The bridge starts its servers in turn, so the last one runs only once every one
			// before it has answered its handshake: the bridge waits on the last one's now.
			testwait.Eventually(t, "the bridge to start every stand-in server", func() bool {
				for _, s := range standIns {
					if _, running := heldprocess.Recorded(s.pidFile, s.seconds); !running {
						return false
					}
				}
				return true
			})
			if row.ignored != "" && !heldprocess.Ignores(envoyMCP.Process.Pid, syscall.SIGHUP) {
				t.Errorf("envoy-mcp, started with SIGHUP ignored, no longer ignores it once its watch is armed; a closing terminal would end it: %s", readPrinted(t, printed.Name()))
			}
			if err := envoyMCP.Process.Signal(row.signal); err != nil {
				t.Fatal(err)
			}
			// The stop closes each server's input and kills each still running once StopGrace has
			// passed, all at once, so StopGrace is the longest envoy-mcp may take however many
			// servers it holds; the rest of the bound is room for a loaded machine, and the whole
			// stays short of three StopGraces, which a stop of the three started servers one after
			// another takes, and of the handshake's own 30 s wait, which a stop that waited on the
			// start would run to.
			bound := mcpbridge.StopGrace + 5*time.Second
			select {
			case <-exited:
			case <-time.After(bound):
				t.Fatalf("envoy-mcp still runs %s after its %v during the bridge's start; want it stopped within the stop's bound: %s", bound, row.signal, readPrinted(t, printed.Name()))
			}
			for _, s := range standIns {
				pid, _ := heldprocess.Recorded(s.pidFile, s.seconds)
				if !testwait.Holds(bound, testwait.Poll, func() bool { return !heldprocess.Running(pid, s.seconds) }) {
					t.Errorf("the stand-in server `sleep %s` (pid %d) still runs %s after envoy-mcp ended on a %v during the bridge's start; want it stopped with the bridge: %s", s.seconds, pid, bound, row.signal, readPrinted(t, printed.Name()))
				}
			}
			if state := envoyMCP.ProcessState; state == nil || !state.Exited() || state.ExitCode() != 0 {
				t.Errorf("envoy-mcp ended %v after a %v during the bridge's start; want it to stop the bridge and exit 0: %s", state, row.signal, readPrinted(t, printed.Name()))
			}
		})
	}
}

// readPrinted is what envoy-mcp wrote, for a failure's message.
func readPrinted(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return "(unread: " + err.Error() + ")"
	}
	return string(raw)
}
