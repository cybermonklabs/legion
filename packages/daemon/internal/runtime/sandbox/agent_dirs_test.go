package sandbox

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// reroledSpec is a launch of the implementer's agent on a step of the role tester, as a claim whose
// key names both steps starts its tester generation.
func reroledSpec(t *testing.T) runtime.SpawnSpec {
	return testSpec(t, implementerToken, claim.RoleTester, testTree)
}

// A generation of an agent on its second role is its agent's: its Claude Code directory and its
// state home are the ones of the slot it runs in, its agent's name, never the step's role. So the
// transcript its first role's generation left under the agent's directory is the one its tester
// generation resumes, and the start frame names that session.
func TestAClaudeAgentOnItsSecondRoleResumesItsTranscript(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := reroledSpec(t)
	spec.Backend = runtime.BackendClaude
	spec.Generation, spec.BootToken, spec.ResumeSessionFile = 2, "boot-implementer-g2", claudeTranscript(claim.RoleImplementer)
	if _, err := r.prepare(spec); err != nil {
		t.Fatalf("prepare the tester generation of %s resuming %s: %v", implementerToken, spec.ResumeSessionFile, err)
	}
	l := launchOf(t, r, spec)
	argv := l.agentArgv(r.agent)
	if want := []string{"--resume", claudeTranscriptID, "--append-system-prompt-file", systemPromptPath(spec.Generation)}; !slices.Equal(argv[max(len(argv)-4, 0):], want) {
		t.Errorf("the agent command ends %q, want %q", argv[max(len(argv)-4, 0):], want)
	}
	home := scratchRoot + "/state/implementer"
	if claude := claudeCodeBinary(home, spec.Generation); !slices.Contains(argv, claude) {
		t.Errorf("the agent command %q does not run the Claude Code its agent's state home installs, %s", argv, claude)
	}
	start := launcherCommand(l, r)
	if start.ResumeFile != spec.ResumeSessionFile {
		t.Errorf("the start frame resumes %q, want %s", start.ResumeFile, spec.ResumeSessionFile)
	}
	env := startEnv(start)
	for name, want := range map[string]string{"CLAUDE_CONFIG_DIR": home + "/claude", "XDG_STATE_HOME": home} {
		if env[name] != want {
			t.Errorf("the tester generation of %s is told %s=%q, want %q", implementerToken, name, env[name], want)
		}
	}
}

// Two claims on one role, the implementer's agent on its tester step and the tester's own, are two
// agents in two slots: each keeps its own state home and its own plugin generation, so neither
// reads or replaces the other's.
func TestTwoClaimsOfOneRoleKeepTwoStateHomes(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	plugins := &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
	extensions := map[claim.Token]string{}
	for _, tc := range []struct {
		spec runtime.SpawnSpec
		home string
	}{
		{reroledSpec(t), scratchRoot + "/state/implementer"},
		{workerSpec(t), scratchRoot + "/state/tester"},
	} {
		tc.spec.Plugins = plugins
		l := launchOf(t, r, tc.spec)
		if got := startEnv(launcherCommand(l, r))["XDG_STATE_HOME"]; got != tc.home {
			t.Errorf("%s on the role %s is told XDG_STATE_HOME=%q, want %q", tc.spec.Claim, tc.spec.Role, got, tc.home)
		}
		argv := l.agentArgv(r.agent)
		at := slices.Index(argv, "--extension")
		if at < 0 || at+1 == len(argv) {
			t.Fatalf("%s's agent command %q names no extension", tc.spec.Claim, argv)
		}
		if extension := argv[at+1]; !strings.HasPrefix(extension, tc.home+"/") {
			t.Errorf("%s's extension %s is not under its state home %s", tc.spec.Claim, extension, tc.home)
		}
		extensions[tc.spec.Claim] = argv[at+1]
	}
	if extensions[implementerToken] == extensions[workerToken] {
		t.Errorf("the two claims of the role tester load one plugin directory, %s", extensions[workerToken])
	}
}

// A generation's address record is kept under its slot: a re-roled claim's generation is recorded
// under its agent's name, the slot its observation reads, and the record of the claim named for the
// role it now runs stays that claim's own. So an address that moved since the re-roled
// generation started is observed, and the other claim's generation is not read as this one.
func TestAMovedAddressIsObservedOnAReroledClaimsGeneration(t *testing.T) {
	g := newRig(t, nil)
	tester := workerSpec(t)
	tester.Generation, tester.BootToken = 2, "boot-tester-g2"
	g.spawn(tester)
	reroled := reroledSpec(t)
	reroled.Generation, reroled.BootToken = 4, "boot-implementer-g4"
	loc := g.spawn(reroled)

	s := g.sandbox(SandboxName(implementerToken))
	if s == nil {
		t.Fatalf("no Sandbox %s", SandboxName(implementerToken))
	}
	generationOf := func(slot claim.Role) uint64 {
		t.Helper()
		raw, ok := s.Annotations[addressesAnnotation(slot)]
		if !ok {
			t.Fatalf("the slot %s holds no address record; the Sandbox holds %v", slot, s.Annotations)
		}
		var record addressRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			t.Fatalf("read the slot %s's address record %q: %v", slot, raw, err)
		}
		return record.Generation
	}
	if got := generationOf(claim.RoleImplementer); got != 4 {
		t.Errorf("the slot implementer records generation %d, want the re-roled claim's generation 4", got)
	}
	if got := generationOf(claim.RoleTester); got != 2 {
		t.Errorf("the slot tester records generation %d, want its own claim's generation 2", got)
	}

	restarted := secondRuntime(t, g, func(o *Options) { o.DaemonURL = movedDaemonURL })
	r2 := restarted()
	g.connectRunning(implementerToken, loc.Sandbox.Generation)
	g.eventually("the re-roled claim's launcher to report its child to the restarted daemon", func() bool {
		state, connected := r2.launchers.state(implementerToken, loc.Sandbox.PodUID)
		return connected && state.Child != nil
	})
	obs := readopt(t, g.ctx, r2, loc)[0]
	if obs.Kind != runtime.StaleAddress {
		t.Fatalf("%s, want StaleAddress: %s", obs.Kind, obs.Detail)
	}
	if want := (movedAddress{"LEGION_DAEMON_URL", testOptions().DaemonURL, movedDaemonURL}).String(); !strings.Contains(obs.Detail, want) {
		t.Errorf("detail %q lacks %q", obs.Detail, want)
	}
}
