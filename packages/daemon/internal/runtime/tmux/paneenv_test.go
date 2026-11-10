package tmux

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The operator's `runtime.tmux` block reaches every pane: its plain variables after the runtime's
// own and before the spec's, sorted, and its Oh My Pi overlays in PI_CONFIG_FILES, which names
// them and the role's own and nothing else.
func TestThePaneIsToldTheOperatorsVariablesAndOverlays(t *testing.T) {
	spec := testSpec()
	in := PaneInputs{
		StateDir: "/state", Workspace: "/state/workspaces/LEGION-43", DaemonURL: "http://127.0.0.1:13370",
		OperatorEnv:   map[string]string{"LAKE_CACHE_DIR": "/legion/lake-cache", "DISPATCH_PUBLIC_URL": "https://dispatch.example"},
		PiConfigFiles: []string{"/etc/legion-operator/overlay.yml", "/etc/legion-operator/project.yml"},
	}
	pairs := panePairs(spec, in, secretFiles("/state", spec))
	var values []string
	for i := 1; i < len(pairs); i += 2 {
		values = append(values, pairs[i])
	}
	want := "PI_CONFIG_FILES=/etc/legion-operator/overlay.yml:/etc/legion-operator/project.yml"
	if !slices.Contains(values, want) {
		t.Errorf("a pane's pairs = %q, want %q among them", values, want)
	}
	order := []string{"XDG_STATE_HOME=/state/home/.local/state", "DISPATCH_PUBLIC_URL=https://dispatch.example", "LAKE_CACHE_DIR=/legion/lake-cache", "JJ_USER=legion-tester"}
	last := -1
	for _, pair := range order {
		at := slices.Index(values, pair)
		if at < 0 || at < last {
			t.Fatalf("a pane's pairs = %q, want %q in that order", values, order)
		}
		last = at
	}
}

// New refuses, before any pane opens, an operator pane variable that would change what a pane is
// told by anything but the operator — a name the runtime sets on a tree or controller pane, PATH,
// the git identity each launch sets, a provider key or its pointer, a credential-shaped name — and
// an overlay PI_CONFIG_FILES cannot name.
func TestNewRefusesAnOperatorPaneVariableTheRuntimeOwns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		overlays []string
		want     string
	}{
		{"a tree pane's variable", map[string]string{"LEGION_ISSUE": "X-1"}, nil, "tmux runtime: pane variable LEGION_ISSUE is one the runtime sets on every pane itself"},
		{"the controller's marker", map[string]string{"LEGION_CONTROLLER": "1"}, nil, "tmux runtime: pane variable LEGION_CONTROLLER is one the runtime sets on every pane itself"},
		{"PATH", map[string]string{"PATH": "/usr/bin"}, nil, "tmux runtime: pane variable PATH is one the runtime sets on every pane itself"},
		{"the git identity", map[string]string{"GIT_AUTHOR_NAME": "someone"}, nil, "tmux runtime: pane variable GIT_AUTHOR_NAME is part of the git identity each launch sets for its role"},
		{"a credential", map[string]string{"LEDGER_TOKEN": "hunter2"}, nil, "tmux runtime: pane variable LEDGER_TOKEN is credential-shaped: a secret reaches a pane as a file (provider_keys), never as a value"},
		{"not a variable name", map[string]string{"1X": "y"}, nil, `tmux runtime: pane variable "1X" is not an environment variable name`},
		{"a relative overlay", nil, []string{"overlay.yml"}, `tmux runtime: Oh My Pi overlay "overlay.yml" is not an absolute path PI_CONFIG_FILES can name`},
		{"an overlay holding the separator", nil, []string{"/etc/a:b.yml"}, `tmux runtime: Oh My Pi overlay "/etc/a:b.yml" is not an absolute path PI_CONFIG_FILES can name`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(paneEnvOptions(t, tc.env, tc.overlays))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("New = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := New(paneEnvOptions(t, map[string]string{"ELAN_HOME": "/opt/elan", "LEDGER_DSN_FILE": "/etc/legion-ledger/dsn"}, []string{"/etc/legion-operator/overlay.yml"})); err != nil {
		t.Errorf("New with plain variables, a _FILE pointer and an absolute overlay: %v", err)
	}
}

// At boot the runtime also refuses, before any pane opens, a pane variable that places the session
// a resume reads, one a Claude Code pane is told after the operator's, a launch secret's pointer,
// which every launch would then be refused over, and a value that is a URL carrying a password:
// each named with why, never with its value.
func TestNewRefusesAPaneVariableThatMovesSessionsOrHoldsAPassword(t *testing.T) {
	const password = "hunter2-boot"
	for _, tc := range []struct {
		name, value, want string
	}{
		{"OMP_SESSION_STORAGE", "sql", "tmux runtime: pane variable OMP_SESSION_STORAGE: it decides where Oh My Pi keeps the session a resume reads, which the runtime places itself"},
		{"OMP_SESSION_SQL_DSN", "/etc/acme/dsn", "tmux runtime: pane variable OMP_SESSION_SQL_DSN: it decides where Oh My Pi keeps the session a resume reads, which the runtime places itself"},
		{"OMP_SESSION_SQL_DSN_FILE", "/etc/acme/dsn", "tmux runtime: pane variable OMP_SESSION_SQL_DSN_FILE: it decides where Oh My Pi keeps the session a resume reads, which the runtime places itself"},
		{"PI_CODING_AGENT_DIR", "/srv/agent", "tmux runtime: pane variable PI_CODING_AGENT_DIR: it decides where Oh My Pi keeps the session a resume reads, which the runtime places itself"},
		{"PI_CONFIG_DIR", ".elsewhere", "tmux runtime: pane variable PI_CONFIG_DIR: it decides where Oh My Pi keeps the session a resume reads, which the runtime places itself"},
		{"CLAUDE_CONFIG_DIR", "/srv/claude", "tmux runtime: pane variable CLAUDE_CONFIG_DIR: the runtime sets it on every Claude Code pane itself, after the operator's variables"},
		{"CLAUDE_CODE_DISABLE_CRON", "0", "tmux runtime: pane variable CLAUDE_CODE_DISABLE_CRON: the runtime sets it on every Claude Code pane itself, after the operator's variables"},
		{"ENVOY_TOKEN_FILE", "/etc/acme/envoy", "tmux runtime: pane variable ENVOY_TOKEN_FILE: it points at the launch secret ENVOY_TOKEN, which the runtime hands every launch as a file of the launch's own"},
		{"NATS_NKEY_SEED_FILE", "/etc/acme/seed", "tmux runtime: pane variable NATS_NKEY_SEED_FILE: it points at the launch secret NATS_NKEY_SEED, which the runtime hands every launch as a file of the launch's own"},
		{"WIDGETS_DATABASE_URL", "postgres://acme:" + password + "@db.internal.example/widgets", "tmux runtime: pane variable WIDGETS_DATABASE_URL: its value is a URL carrying a password: a secret reaches a pane as a file (provider_keys), never as a value on tmux's command line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPaneEnv(map[string]string{"ELAN_HOME": "/opt/elan", tc.name: tc.value}, nil)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("checkPaneEnv = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("the refusal %q holds the value's password", err)
			}
		})
	}
	// The Claude Code pane's pairs are exactly the names refused: none is told after the operator's
	// variables without the operator being refused it first.
	pairs := claudeEnvironment("/state/claude/c")
	for i := 1; i < len(pairs); i += 2 {
		name, _, _ := strings.Cut(pairs[i], "=")
		if checkPaneEnv(map[string]string{name: "x"}, nil) == nil {
			t.Errorf("a Claude Code pane is told %s after the operator's variables, and the operator may set it", name)
		}
	}
	if err := checkPaneEnv(map[string]string{"WIDGETS_URL": "https://widgets.example/acme", "LEDGER_DSN_FILE": "/etc/legion-ledger/dsn",
		// Claude Code's background tasks are on under either runtime, so nothing a Claude Code pane
		// is told turns them off and the operator's own value stands.
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": "1"}, nil); err != nil {
		t.Errorf("checkPaneEnv with a URL holding no password, a _FILE pointer of the operator's and Claude Code's background tasks: %v", err)
	}
}

// paneEnvOptions are the Options of a runtime given the operator's pane variables and overlays.
func paneEnvOptions(t *testing.T, env map[string]string, overlays []string) Options {
	t.Helper()
	requireTmux(t)
	return Options{
		Project: "omp", StateDir: t.TempDir(), StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", Launchers: ompOnly("omp"), StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=/usr/bin:/bin"},
		Executable: func() (string, error) { return "/opt/legion", nil }, GitHubCredential: staticCredential,
		ControllerCredential: staticControllerCredential, GH: fakeGH, ClaimRepository: acmeWidgets,
		PaneEnv: env, PiConfigFiles: overlays,
	}
}

// A spec variable, or a secret's pointer, of a name the operator's pane variables set would have
// one of the two silently win, so the launch is refused before tmux is asked anything.
func TestALaunchCollidingWithAnOperatorPaneVariableIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*runtime.SpawnSpec)
		want   string
	}{
		{"a spec variable", func(s *runtime.SpawnSpec) { s.Env["ELAN_HOME"] = "/home/me/.elan" },
			"spawn legion-omp-legion-43-tester: Env sets ELAN_HOME, which runtime.tmux.pane_env sets on every pane"},
		{"a secret's pointer", func(s *runtime.SpawnSpec) { s.Secrets["LEDGER_DSN"] = "dsn" },
			"spawn legion-omp-legion-43-tester: secret LEDGER_DSN's pointer LEDGER_DSN_FILE is set on every pane by runtime.tmux.pane_env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeServerRuntime(t, staticCredential, nil)
			f.operatorEnv = map[string]string{"ELAN_HOME": "/opt/elan", "LEDGER_DSN_FILE": "/etc/legion-ledger/dsn"}
			spec := f.spec(t, claim.RoleTester)
			tc.mutate(&spec)
			if _, err := f.Spawn(context.Background(), spec); err == nil || err.Error() != tc.want {
				t.Fatalf("Spawn = %v, want %q", err, tc.want)
			}
			if len(f.argvs) != 0 {
				t.Errorf("a refused launch ran tmux: %q", f.argvs)
			}
		})
	}
}

// The boot gate probes a pane's Oh My Pi under the environment a pane gets: the operator's
// variables and overlays among it (gateEnvironment, internal/daemon), which PaneEnvironment and the
// block compose as panePairs does; a pane launched by a runtime given the block is told the same.
func TestALaunchedPaneIsToldTheOperatorsBlock(t *testing.T) {
	f := newFakeServerRuntime(t, staticCredential, nil)
	f.operatorEnv = map[string]string{"LAKE_CACHE_DIR": "/legion/lake-cache"}
	f.piConfigFiles = []string{"/etc/legion-operator/overlay.yml"}
	spec := f.spec(t, claim.RoleTester)
	if _, err := f.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	pairs := f.pairs(t)
	for _, want := range []string{"LAKE_CACHE_DIR=/legion/lake-cache", "PI_CONFIG_FILES=/etc/legion-operator/overlay.yml"} {
		if !slices.Contains(pairs, want) {
			t.Errorf("the pane's pairs = %q, want %q", pairs, want)
		}
	}
	for _, pair := range pairs {
		if strings.HasPrefix(pair, "PATH=") {
			t.Errorf("PATH rode a -e pair: %q", pair)
		}
	}
}

// role_config_files reaches the panes of the roles it names alone: such a pane's PI_CONFIG_FILES is
// the operator's overlays, then the role's own in order, and nothing after them — the
// daemon-launched controller's pane too. A pane of a role it does not name gets the very pane
// command a daemon without the key gives it.
func TestOnlyTheNamedRolesPanesAreToldTheirOwnOverlays(t *testing.T) {
	own := map[claim.Role][]string{
		claim.RoleTester:     {"/etc/legion-operator/fallback.yml", "/etc/legion-operator/tester.yml"},
		claim.RoleController: {"/etc/legion-operator/fallback.yml"},
	}
	stateDir := t.TempDir()
	runtimeOn := func(t *testing.T, files map[claim.Role][]string) *fakeServerRuntime {
		return newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) {
			o.StateDir, o.PiConfigFiles, o.RoleConfigFiles = stateDir, []string{"/etc/legion-operator/overlay.yml"}, files
		})
	}
	// specs are one launch spec per role, the same for every runtime below, so two launches of a
	// role differ by the runtime alone.
	specs := map[claim.Role]runtime.SpawnSpec{claim.RoleController: controllerSpec(t, "omp")}
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer, claim.RoleArchitect} {
		specs[role] = runtimeOn(t, nil).spec(t, role)
	}
	// launch is the new-window argv, and its pairs, of one launch of role by a runtime given the
	// operator's overlay and files, on stateDir.
	launch := func(t *testing.T, files map[claim.Role][]string, role claim.Role) ([]string, []string) {
		t.Helper()
		f := runtimeOn(t, files)
		if _, err := f.Spawn(context.Background(), specs[role]); err != nil {
			t.Fatalf("Spawn %s: %v", role, err)
		}
		for _, argv := range f.argvs {
			if verb(argv) == "new-window" {
				return argv, f.pairs(t)
			}
		}
		t.Fatal("no new-window")
		return nil, nil
	}
	for role, want := range map[claim.Role]string{
		claim.RoleTester:     "PI_CONFIG_FILES=/etc/legion-operator/overlay.yml:/etc/legion-operator/fallback.yml:/etc/legion-operator/tester.yml",
		claim.RoleController: "PI_CONFIG_FILES=/etc/legion-operator/overlay.yml:/etc/legion-operator/fallback.yml",
	} {
		if _, pairs := launch(t, own, role); !slices.Contains(pairs, want) {
			t.Errorf("the %s's pane pairs = %q, want %q", role, pairs, want)
		}
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleArchitect} {
		with, pairs := launch(t, own, role)
		without, _ := launch(t, nil, role)
		if !slices.Equal(with, without) {
			t.Errorf("the %s's new-window with another role's overlays =\n%q\nwant the one without the key:\n%q", role, with, without)
		}
		if want := "PI_CONFIG_FILES=/etc/legion-operator/overlay.yml"; !slices.Contains(pairs, want) {
			t.Errorf("the %s's pane pairs = %q, want %q", role, pairs, want)
		}
	}
}

// New refuses a role's overlay PI_CONFIG_FILES cannot name, as it refuses the operator's.
func TestNewRefusesARoleOverlayPIConfigFilesCannotName(t *testing.T) {
	for _, file := range []string{"fallback.yml", "/etc/a:b.yml"} {
		opts := paneEnvOptions(t, nil, nil)
		opts.RoleConfigFiles = map[claim.Role][]string{claim.RoleMerger: {file}}
		want := `tmux runtime: Oh My Pi overlay "` + file + `" is not an absolute path PI_CONFIG_FILES can name`
		if _, err := New(opts); err == nil || err.Error() != want {
			t.Errorf("New with the merger's overlay %q = %v, want %q", file, err, want)
		}
	}
}

// standInCheck writes a stand-in shared public leak check in a directory of its own and answers
// its path. The file's name is the real check's, so a daemon that finds it on its PATH resolves
// it as the check's own documentation says to.
func standInCheck(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "public-leak-check.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// daemonPATH is the PATH the fake runtime's options carry, which names the stub tmux New
// resolves: a case that adds a directory to the daemon's PATH keeps that one on it.
func daemonPATH(o *Options) string {
	for _, entry := range o.Environ {
		if path, ok := strings.CutPrefix(entry, "PATH="); ok {
			return path
		}
	}
	return ""
}

// A pane is told the shared public leak check the daemon resolved on its own machine, by each of
// the three routes the check documents, and is told no such variable where the daemon resolves
// none. A pane can resolve none of the three itself — PUBLIC_LEAK_CHECK is off the allow list,
// its XDG_CONFIG_HOME is the daemon's rather than the operator's, and the check's installer puts
// its files on no PATH a pane has — so a pane told nothing would have every `legion gh` and
// every rescue push refused. A machine that carries no check is told nothing, which refuses
// every public write of that pane with the setup error naming the check: the honest answer, and
// no launch failure, since Legion installs no check on a tmux machine.
func TestAPaneIsToldTheCheckTheDaemonResolvedOnItsOwnMachine(t *testing.T) {
	for _, tc := range []struct {
		name string
		// environ adds the route's names to the daemon's own environment and answers the check
		// the pane must then be told, "" where the machine carries none.
		environ func(t *testing.T, o *Options) string
	}{
		{
			name: "PUBLIC_LEAK_CHECK in the daemon's own environment",
			environ: func(t *testing.T, o *Options) string {
				check := standInCheck(t)
				o.Environ = append(slices.Clone(o.Environ), "PUBLIC_LEAK_CHECK="+check)
				return check
			},
		},
		{
			name: "public-leak-check.sh on the daemon's PATH",
			environ: func(t *testing.T, o *Options) string {
				check := standInCheck(t)
				o.Environ = append(slices.Clone(o.Environ), "PATH="+filepath.Dir(check)+":"+daemonPATH(o))
				return check
			},
		},
		{
			name: "the pointer file under the daemon's configuration home",
			environ: func(t *testing.T, o *Options) string {
				check := standInCheck(t)
				config := t.TempDir()
				if err := os.MkdirAll(filepath.Join(config, "public-leak-check"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(config, "public-leak-check", "path"), []byte(check+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				o.Environ = append(slices.Clone(o.Environ), "XDG_CONFIG_HOME="+config)
				return check
			},
		},
		{
			name: "a machine that carries none",
			environ: func(t *testing.T, o *Options) string {
				// Nothing is added: the fake runtime's environment is a PATH naming the stub
				// tmux's directory alone, with no HOME and no XDG_CONFIG_HOME, so each of the
				// three places answers nothing.
				return ""
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want string
			f := newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) { want = tc.environ(t, o) })
			if _, err := f.Spawn(context.Background(), f.spec(t, claim.RoleTester)); err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			told := ""
			n := 0
			for _, pair := range f.pairs(t) {
				if value, ok := strings.CutPrefix(pair, "PUBLIC_LEAK_CHECK="); ok {
					told, n = value, n+1
				}
			}
			switch {
			case want == "" && n != 0:
				t.Errorf("the pane was told PUBLIC_LEAK_CHECK=%q, want no such pair: the daemon resolved no check", told)
			case want != "" && (n != 1 || told != want):
				t.Errorf("the pane was told %d PUBLIC_LEAK_CHECK pairs, the last %q; want one naming the check the daemon resolved, %q", n, told, want)
			}
		})
	}
}

// The resolved check is no runtime-owned name: an operator's pane variable and a spec's own Env
// are told after it and outrank it, since an operator may point a pane at another copy of the
// check. Both are refused for a name the runtime owns, so this is what keeps them allowed.
func TestAnOperatorsCheckAndASpecsOutrankTheResolvedOne(t *testing.T) {
	if ownedNames["PUBLIC_LEAK_CHECK"] {
		t.Fatal("PUBLIC_LEAK_CHECK is a runtime-owned name, so no pane variable and no spec Env may set it")
	}
	in := PaneInputs{StateDir: "/state", Workspace: "/state/workspaces/LEGION-43", LeakCheck: "/opt/machine/public-leak-check.sh"}
	in.OperatorEnv = map[string]string{"PUBLIC_LEAK_CHECK": "/opt/operator/public-leak-check.sh"}
	spec := testSpec()
	// The order tmux applies is the order of the pairs, so the last of a name is what the pane
	// holds: the daemon's resolved check, then the operator's, then the spec's.
	want := []string{"/opt/machine/public-leak-check.sh", "/opt/operator/public-leak-check.sh"}
	for _, told := range [][]string{nil, {"/opt/spec/public-leak-check.sh"}} {
		if told != nil {
			spec.Env["PUBLIC_LEAK_CHECK"] = told[0]
			want = append(want, told[0])
		}
		var got []string
		for _, pair := range panePairs(spec, in, secretFiles("/state", spec)) {
			if value, ok := strings.CutPrefix(pair, "PUBLIC_LEAK_CHECK="); ok {
				got = append(got, value)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("a pane's PUBLIC_LEAK_CHECK pairs = %q, want %q in that order", got, want)
		}
	}
}
