package ompdirs

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
}

// The profile root is where Oh My Pi, started under the environment, keeps the profile's data:
// the profile OMP_PROFILE (else PI_PROFILE) names under the config root — OMP_CONFIG_ROOT where it
// names an absolute directory, else HOME's PI_CONFIG_DIR or .omp — unless an XDG data root already
// holds that profile's data, and an honoured PI_CODING_AGENT_DIR elsewhere turns the XDG data root
// off. The state root is the same rule over XDG_STATE_HOME, so each row runs over both, with the
// data home the row names moved to the state home for the state root. The gate's manifest tests
// (internal/daemon) hold the full resolution to Oh My Pi's; these hold the root itself.
func TestEachRootIsWhereOhMyPiKeepsTheProfilesDirectories(t *testing.T) {
	passwdHome := ""
	if u, err := user.Current(); err == nil {
		passwdHome = u.HomeDir
	}
	roots := []struct {
		name, variable string
		resolve        func(env map[string]string, workDir string) (root, profile string, err error)
	}{
		{"ProfileRoot", "XDG_DATA_HOME", ProfileRoot},
		{"StateRoot", "XDG_STATE_HOME", StateRoot},
	}
	for _, testCase := range []struct {
		name    string
		env     func(home, xdg string) map[string]string
		layout  func(t *testing.T, home, xdg string)
		want    func(home, xdg string) string
		profile string
	}{
		{
			name: "the default profile under HOME",
			env:  func(home, xdg string) map[string]string { return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg} },
			want: func(home, _ string) string { return filepath.Join(home, ".omp") },
		},
		{
			name:   "the default profile under an XDG data root that exists",
			env:    func(home, xdg string) map[string]string { return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg} },
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return filepath.Join(xdg, "omp") },
		},
		{
			name: "a named profile, trimmed, whose XDG directory does not exist",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": " work "}
			},
			layout:  func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:    func(home, _ string) string { return filepath.Join(home, ".omp", "profiles", "work") },
			profile: "work",
		},
		{
			name: "a named profile whose XDG directory exists",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "work"}
			},
			layout:  func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp", "profiles", "work")) },
			want:    func(_, xdg string) string { return filepath.Join(xdg, "omp", "profiles", "work") },
			profile: "work",
		},
		{
			name:    "PI_PROFILE when OMP_PROFILE is unset",
			env:     func(home, _ string) map[string]string { return map[string]string{"HOME": home, "PI_PROFILE": "legacy"} },
			want:    func(home, _ string) string { return filepath.Join(home, ".omp", "profiles", "legacy") },
			profile: "legacy",
		},
		{
			name: "an empty OMP_PROFILE, or default, is the default profile",
			env: func(home, _ string) map[string]string {
				return map[string]string{"HOME": home, "OMP_PROFILE": "", "PI_PROFILE": "legacy"}
			},
			want: func(home, _ string) string { return filepath.Join(home, ".omp") },
		},
		{
			name: "no HOME is the account's home directory",
			env:  func(string, string) map[string]string { return map[string]string{} },
			want: func(string, string) string { return filepath.Join(passwdHome, ".omp") },
		},
		{
			name: "PI_CONFIG_DIR names the config root under HOME",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CONFIG_DIR": ".omp-alt"}
			},
			want: func(home, _ string) string { return filepath.Join(home, ".omp-alt") },
		},
		// OMP_CONFIG_ROOT, trimmed, `~` expanded against HOME, and cleaned, is the config root in
		// place of PI_CONFIG_DIR under HOME where it is absolute; a relative, empty or blank one is
		// ignored (getConfigRootOverride, resolveAbsoluteDir). The root here lies outside HOME and
		// is never made: Oh My Pi tests no config root for existence.
		{
			name: "an absolute OMP_CONFIG_ROOT is the config root in place of PI_CONFIG_DIR under HOME",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CONFIG_DIR": ".omp-alt",
					"OMP_CONFIG_ROOT": " " + xdg + "/elsewhere/../root/ "}
			},
			want: func(_, xdg string) string { return filepath.Join(xdg, "root") },
		},
		{
			name: "a named profile under an absolute OMP_CONFIG_ROOT",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CONFIG_DIR": ".omp-alt", "OMP_PROFILE": "work",
					"OMP_CONFIG_ROOT": filepath.Join(xdg, "root")}
			},
			want:    func(_, xdg string) string { return filepath.Join(xdg, "root", "profiles", "work") },
			profile: "work",
		},
		{
			name: "an XDG data root that exists wins over OMP_CONFIG_ROOT",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_CONFIG_ROOT": filepath.Join(xdg, "root")}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return filepath.Join(xdg, "omp") },
		},
		{
			name: "an OMP_CONFIG_ROOT under ~/ is under HOME",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CONFIG_DIR": ".omp-alt", "OMP_CONFIG_ROOT": "~/omp-root"}
			},
			want: func(home, _ string) string { return filepath.Join(home, "omp-root") },
		},
		{
			name: "an OMP_CONFIG_ROOT of ~ is HOME itself",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "work", "OMP_CONFIG_ROOT": "~"}
			},
			want:    func(home, _ string) string { return filepath.Join(home, "profiles", "work") },
			profile: "work",
		},
		{
			name: "a relative OMP_CONFIG_ROOT is ignored",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CONFIG_DIR": ".omp-alt", "OMP_CONFIG_ROOT": "omp-root"}
			},
			want: func(home, _ string) string { return filepath.Join(home, ".omp-alt") },
		},
		{
			name: "an empty OMP_CONFIG_ROOT is ignored",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_CONFIG_ROOT": ""}
			},
			want: func(home, _ string) string { return filepath.Join(home, ".omp") },
		},
		{
			name: "a blank OMP_CONFIG_ROOT is ignored",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CONFIG_DIR": ".omp-alt", "OMP_CONFIG_ROOT": " \t "}
			},
			want: func(home, _ string) string { return filepath.Join(home, ".omp-alt") },
		},
		{
			name: "an honoured PI_CODING_AGENT_DIR turns the XDG data root off",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CODING_AGENT_DIR": filepath.Join(home, "elsewhere")}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(home, _ string) string { return filepath.Join(home, ".omp") },
		},
		{
			name: "a relative PI_CODING_AGENT_DIR naming the config root's own agent directory keeps the XDG data root",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "PI_CODING_AGENT_DIR": "../.omp/agent"}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return filepath.Join(xdg, "omp") },
		},
		{
			name: "the agent directory PI_PROFILE hands down keeps the XDG data root",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "", "PI_PROFILE": "work",
					"PI_CODING_AGENT_DIR": filepath.Join(home, ".omp", "profiles", "work", "agent")}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return filepath.Join(xdg, "omp") },
		},
		// Under an OMP_CONFIG_ROOT, the agent directory a profile hands down is that profile's under
		// the override (isProfileDerivedAgentDir over getProfileAgentDir): that one keeps the XDG
		// data root, and the same profile's under HOME is an agent directory like any other.
		{
			name: "the agent directory PI_PROFILE hands down under OMP_CONFIG_ROOT keeps the XDG data root",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "", "PI_PROFILE": "work",
					"OMP_CONFIG_ROOT":     filepath.Join(home, "root"),
					"PI_CODING_AGENT_DIR": filepath.Join(home, "root", "profiles", "work", "agent")}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return filepath.Join(xdg, "omp") },
		},
		{
			name: "under OMP_CONFIG_ROOT, the profile's agent directory under HOME is honoured and turns the XDG data root off",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "", "PI_PROFILE": "work",
					"OMP_CONFIG_ROOT":     filepath.Join(home, "root"),
					"PI_CODING_AGENT_DIR": filepath.Join(home, ".omp", "profiles", "work", "agent")}
			},
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(home, _ string) string { return filepath.Join(home, "root") },
		},
	} {
		for _, root := range roots {
			t.Run(root.name+"/"+testCase.name, func(t *testing.T) {
				home, xdg := t.TempDir(), t.TempDir()
				if testCase.layout != nil {
					testCase.layout(t, home, xdg)
				}
				env := testCase.env(home, xdg)
				if dataHome, set := env["XDG_DATA_HOME"]; set {
					delete(env, "XDG_DATA_HOME")
					env[root.variable] = dataHome
				}
				got, profile, err := root.resolve(env, filepath.Join(home, "work"))
				if err != nil {
					t.Fatalf("%s: %v", root.name, err)
				}
				if want := testCase.want(home, xdg); got != want {
					t.Errorf("root = %s, want %s", got, want)
				}
				if profile != testCase.profile {
					t.Errorf("profile = %q, want %q", profile, testCase.profile)
				}
			})
		}
	}
}

// The data home moves the data root alone and the state home the state root alone, each where its
// own candidate exists: a pod's shim makes the state root's candidate (StateRootCandidate) and
// nothing under the data home, so the state root moves while the plugins stay under the config root.
func TestTheDataAndStateHomesMoveTheirOwnRootsAlone(t *testing.T) {
	home, data, state := t.TempDir(), t.TempDir(), t.TempDir()
	env := map[string]string{"HOME": home, "XDG_DATA_HOME": data, "XDG_STATE_HOME": state, "OMP_PROFILE": "legion"}
	candidate, ok, err := StateRootCandidate(env)
	if err != nil || !ok || candidate != filepath.Join(state, "omp", "profiles", "legion") {
		t.Fatalf("StateRootCandidate = %s, %t, %v, want %s, true, nil", candidate, ok, err, filepath.Join(state, "omp", "profiles", "legion"))
	}
	mkdir(t, candidate)
	configRoot := filepath.Join(home, ".omp", "profiles", "legion")
	if root, _, err := ProfileRoot(env, home); err != nil || root != configRoot {
		t.Errorf("ProfileRoot = %s, %v, want the config root %s", root, err, configRoot)
	}
	if root, _, err := StateRoot(env, home); err != nil || root != candidate {
		t.Errorf("StateRoot = %s, %v, want the candidate %s", root, err, candidate)
	}
	mkdir(t, filepath.Join(data, "omp", "profiles", "legion"))
	if root, _, err := ProfileRoot(env, home); err != nil || root != filepath.Join(data, "omp", "profiles", "legion") {
		t.Errorf("ProfileRoot = %s, %v, want the data home's %s", root, err, filepath.Join(data, "omp", "profiles", "legion"))
	}
	if root, _, err := StateRoot(env, home); err != nil || root != candidate {
		t.Errorf("StateRoot = %s, %v, want the candidate %s still", root, err, candidate)
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"no state home":       {env: map[string]string{"HOME": home, "OMP_PROFILE": "legion"}},
		"an empty state home": {env: map[string]string{"HOME": home, "XDG_STATE_HOME": "", "OMP_PROFILE": "legion"}},
		"the default profile": {env: map[string]string{"HOME": home, "XDG_STATE_HOME": state}, want: filepath.Join(state, "omp")},
	} {
		candidate, ok, err := StateRootCandidate(tc.env)
		if err != nil || ok != (tc.want != "") || candidate != tc.want {
			t.Errorf("%s: StateRootCandidate = %q, %t, %v, want %q, %t, nil", name, candidate, ok, err, tc.want, tc.want != "")
		}
	}
}

// The agent directory Oh My Pi reads its config.yml from is the `agent` of the profile's config
// root, wherever an XDG data root moves the profile's data, and the environment it was given is
// left as it was. A profile Oh My Pi refuses is refused here too.
func TestConfigAgentDirIsTheConfigRootsAgentWhateverTheDataHome(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	mkdir(t, filepath.Join(data, "omp", "profiles", "legion"))
	env := map[string]string{"HOME": home, "XDG_DATA_HOME": data, "OMP_PROFILE": "legion"}
	if root, _, err := ProfileRoot(env, home); err != nil || root != filepath.Join(data, "omp", "profiles", "legion") {
		t.Fatalf("ProfileRoot = %s, %v, want the data home's, which this test needs to move the data root", root, err)
	}
	if dir, err := ConfigAgentDir(env, home); err != nil || dir != filepath.Join(home, ".omp", "profiles", "legion", "agent") {
		t.Errorf("ConfigAgentDir = %s, %v, want the config root's %s", dir, err, filepath.Join(home, ".omp", "profiles", "legion", "agent"))
	}
	if env["XDG_DATA_HOME"] != data {
		t.Errorf("ConfigAgentDir changed the environment it was given: XDG_DATA_HOME is %q", env["XDG_DATA_HOME"])
	}
	if dir, err := ConfigAgentDir(map[string]string{"HOME": home}, home); err != nil || dir != filepath.Join(home, ".omp", "agent") {
		t.Errorf("ConfigAgentDir of the default profile = %s, %v, want %s", dir, err, filepath.Join(home, ".omp", "agent"))
	}
	if _, err := ConfigAgentDir(map[string]string{"HOME": home, "OMP_PROFILE": "Work"}, home); err == nil || !strings.Contains(err.Error(), `Invalid OMP profile "Work"`) {
		t.Errorf("ConfigAgentDir of a profile Oh My Pi refuses = %v, want the invalid-profile refusal", err)
	}
}

// A profile name Oh My Pi cannot use is refused in its words, naming the name as given, by each root
// and by the state root's candidate.
func TestProfileRootRefusesAProfileOhMyPiRefuses(t *testing.T) {
	for _, name := range []string{"Work", "..", "work.", "-work", "con", "nul.txt"} {
		_, _, err := ProfileRoot(map[string]string{"HOME": t.TempDir(), "OMP_PROFILE": name}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), `Invalid OMP profile "`+name+`"`) {
			t.Errorf("OMP_PROFILE=%q: err = %v, want the invalid-profile refusal naming it", name, err)
		}
		_, _, err = StateRoot(map[string]string{"HOME": t.TempDir(), "OMP_PROFILE": name}, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), `Invalid OMP profile "`+name+`"`) {
			t.Errorf("OMP_PROFILE=%q: StateRoot err = %v, want the invalid-profile refusal naming it", name, err)
		}
		if _, _, err := StateRootCandidate(map[string]string{"XDG_STATE_HOME": t.TempDir(), "OMP_PROFILE": name}); err == nil || !strings.Contains(err.Error(), `Invalid OMP profile "`+name+`"`) {
			t.Errorf("OMP_PROFILE=%q: StateRootCandidate err = %v, want the invalid-profile refusal naming it", name, err)
		}
		if _, valid := NormalizeProfile(name); valid {
			t.Errorf("NormalizeProfile(%q) accepts a name Oh My Pi refuses", name)
		}
	}
	for requested, want := range map[string]string{"": "", "default": "", " work ": "work", "a.b-c_d": "a.b-c_d"} {
		if got, valid := NormalizeProfile(requested); !valid || got != want {
			t.Errorf("NormalizeProfile(%q) = %q, %t, want %q, true", requested, got, valid, want)
		}
	}
}
