package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The image probe's settings hold a contract below one to be none: the probe refuses 0 and a
// negative number before it creates any Sandbox, as it refuses a budget or a retry wait that is not
// positive (ProbeImage's `p.Contract < 1`), and it runs for a contract of one, the control that
// shows where the line is. internal/api's test of this name drives the daemon's two other readings
// of a number below one.
func TestANumberBelowOneIsNoContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		contract int
		refused  bool
	}{{contract: 0, refused: true}, {contract: -1, refused: true}, {contract: 1}} {
		t.Run(strconv.Itoa(tc.contract), func(t *testing.T) {
			g := newProbeRig(t, nil)
			g.succeeds(okLine(tc.contract) + "\n")
			p := probeOptionsToDeadline(t)
			p.Contract = tc.contract

			err := g.probe(p)

			if !tc.refused {
				if err != nil {
					t.Fatalf("ProbeImage at contract %d = %v, want a pass: one is a contract", tc.contract, err)
				}
				if n := g.creates.Load(); n != 1 {
					t.Errorf("created the probe Sandbox %d times at contract %d, want once", n, tc.contract)
				}
				g.eventually("the probe Sandbox to be deleted", func() bool { return g.sandbox(probeSandboxName) == nil })
				return
			}
			wantContains(t, err, "image probe: a contract, a positive budget, and a positive retry wait are required")
			if n := g.creates.Load(); n != 0 {
				t.Errorf("created the probe Sandbox %d times at contract %d, want none: the settings are refused before anything is created", n, tc.contract)
			}
		})
	}
}

// The install a stale-plugin relaunch renders into a kept pod holds the installed pi-legion to
// this daemon's contract by equality (pluginCheckScript): a manifest declaring the contract below
// the daemon's, or the one above it, ends the start wrapper with the line that names the package
// and both numbers, and the agent never starts on it. The manifest of the daemon's own contract is
// the control, and starts the agent, so the absent agent of the other two is not a harness that
// ran nothing. Each run is the wrapper the daemon renders (runStart), with a stand-in bun that
// leaves the manifest where `bun add` installs pi-legion, and the real node running the check.
func TestTheKeptPodPluginCheckRefusesAManifestOfAnotherContract(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("no node in this environment: the install's contract check runs on it")
	}
	plugins := testPlugins()
	for name, tc := range map[string]struct {
		declared int
		refused  bool
	}{
		"the daemon's own contract":       {declared: plugins.Contract},
		"the contract below the daemon's": {declared: plugins.Contract - 1, refused: true},
		"the contract above the daemon's": {declared: plugins.Contract + 1, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			manifest := fmt.Sprintf(`{"name":"@sjawhar/pi-legion","version":%q,"legion":{"daemonApiVersion":%d}}`, plugins.PiLegion, tc.declared)
			bun := "#!/bin/sh\nmkdir -p node_modules/@sjawhar/pi-legion\nprintf '%s\\n' " + shellQuote(manifest) + " >node_modules/@sjawhar/pi-legion/package.json\n"
			if err := os.WriteFile(filepath.Join(dir, "bun"), []byte(bun), 0o700); err != nil {
				t.Fatal(err)
			}
			agentLog := filepath.Join(dir, "agent.log")

			got := runStart(t, dir, dir, filepath.Join(dir, "bun"), "", plugins, nil, recorder(agentLog, "agent-stdout"))

			_, err := os.Stat(agentLog)
			agentRan := err == nil
			if !tc.refused {
				if got.exit != 0 || !agentRan {
					t.Fatalf("over a manifest of the daemon's own contract %d the wrapper exited %d and the agent ran: %v, want 0 and the agent started:\n%s",
						tc.declared, got.exit, agentRan, got.stderr)
				}
				return
			}
			if got.exit != 1 {
				t.Errorf("over a manifest of contract %d the wrapper exited %d, want 1, the check's own exit:\n%s", tc.declared, got.exit, got.stderr)
			}
			if agentRan {
				t.Errorf("the agent started on a pi-legion of contract %d, where this daemon requires %d", tc.declared, plugins.Contract)
			}
			refusal := fmt.Sprintf("legion plugins: @sjawhar/pi-legion@%s declares daemon API contract %d; this daemon requires %d", plugins.PiLegion, tc.declared, plugins.Contract)
			if !strings.Contains(got.stderr, refusal) {
				t.Errorf("stderr is %q, want the check's refusal %q", got.stderr, refusal)
			}
			if got.stdout != "" {
				t.Errorf("the wrapper wrote %q to stdout, the shim's frame stream", got.stdout)
			}
		})
	}
}
