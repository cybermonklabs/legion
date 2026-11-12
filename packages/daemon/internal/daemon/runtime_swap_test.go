package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/settings"
)

// runtimeSwapRig is a settings keeper over a legion.yaml the test rewrites, with the runtime
// build of the daemon behind it: every settle builds the runtime again from the configuration
// that settled, which is what a running daemon does through rebuilder.observe and
// runtimeHolder.rebuild (daemon.go's buildRuntime over p.runtimeFor). The build is the
// production one on either side: readSandbox under kubernetes, which is prepareSandbox's own
// runtimeFor, and under tmux the closure prepareTmux leaves behind (daemon.go's p.runtimeFor),
// which this rig has prepareTmux install from the deployment's own files. It runs on every
// settle rather than only on a change naming the runtime holder, since a configuration the
// daemon is running on must be one its own runtime can be built from whichever key moved.
//
// A build that dies takes the daemon with it: nothing stands between config.Live.Apply's
// observers and the process. So a panic is caught here and recorded, and the test reports it as
// the daemon's death — a panic let through would kill the test binary with the assertions below
// never run.
type runtimeSwapRig struct {
	t      *testing.T
	keeper *settings.Keeper
	path   string
	// tmuxFor is the tmux runtime build prepareTmux installs, which refuses a configuration
	// that is not this runtime's and then answers the factory
	// (TestTheTmuxBuildPrepareTmuxInstallsRefusesAConfigurationThatIsNotItsOwn).
	tmuxFor func(config.Config) (runtimeFactory, error)
	built   []error  // what the runtime build answered, one per settle
	died    []string // what a build that panicked panicked with
}

// observe is the rig's one observer, registered with config.Live.OnChange as rebuilder.observe
// is.
func (r *runtimeSwapRig) observe(cfg *config.Config, _ []config.Change) {
	r.built = append(r.built, r.build(cfg))
}

func (r *runtimeSwapRig) build(cfg *config.Config) (err error) {
	defer func() {
		if died := recover(); died != nil {
			r.died = append(r.died, fmt.Sprint(died))
			err = fmt.Errorf("the runtime build died: %v", died)
		}
	}()
	switch cfg.Runtime.Name {
	case "kubernetes":
		// prepareSandbox's own runtimeFor: the client and every pod option, read again from the
		// configuration this settle handed it (kubernetes.go's p.runtimeFor).
		_, err = readSandbox(*cfg, "demo", "dispatch-token", "", lookup(nil), quietLogger())
	case "tmux":
		// The build prepareTmux leaves behind: its refusal of a configuration that is not this
		// runtime's, before a factory that reads the configuration it is given and
		// dereferences no block of it (tmuxOptions).
		_, err = r.tmuxFor(*cfg)
	default:
		err = fmt.Errorf("runtime %q is neither tmux nor kubernetes", cfg.Runtime.Name)
	}
	return err
}

// settle is the file arm the file watcher calls after an operator's edit (internal/settings'
// Keeper.File), with whatever the build answered for the configuration it settled.
func (r *runtimeSwapRig) settle(body string) ([]config.Change, error) {
	r.t.Helper()
	if err := os.WriteFile(r.path, []byte(body), 0o600); err != nil {
		r.t.Fatalf("rewrite %s: %v", r.path, err)
	}
	return r.keeper.File(r.t.Context())
}

// runtimeSwapFile writes a legion.yaml whose `runtime` key is block, with the kubeconfig and the
// credential files it names beside it, and answers the whole body and its path. The two bodies
// a swap writes differ in that one key and nothing else, as an operator's edit does.
func runtimeSwapFile(t *testing.T, dir, block string) string {
	t.Helper()
	return fmt.Sprintf(`project: DEMO
postgres_dsn: postgres://legion:legion@127.0.0.1:1/legion
state_dir: %s
bind: 10.0.0.5
daemon_url: http://10.0.0.5:13370
envoy_url: http://envoy-listener.internal.example:9020
envoy_token_file: %s/envoy-token
operator_token_file: %s/operator-token
dispatch_url: https://dispatch.internal.example
dispatch_token_file: %s/dispatch-token
nats_urls: [nats://nats.internal.example:4222]
projects:
  DEMO: {}
github_apps:
  implement: { app_id: "1", private_key: implement-test-key }
  review: { app_id: "2", private_key: review-test-key }
%s`, filepath.Join(dir, "state"), dir, dir, dir, block)
}

// newRuntimeSwapRig writes the deployment's files and resolves booted, as a boot does.
func newRuntimeSwapRig(t *testing.T, booted func(dir string) string) *runtimeSwapRig {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"envoy-token": "envoy\n", "operator-token": "operator\n", "dispatch-token": "dispatch\n",
		"kubeconfig": `apiVersion: v1
kind: Config
clusters: [{name: example, cluster: {server: "https://192.0.2.20:6443"}}]
users: [{name: legion-daemon, user: {token: placeholder}}]
contexts: [{name: legion-daemon, context: {cluster: example, user: legion-daemon, namespace: legion}}]
current-context: legion-daemon
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	path := filepath.Join(dir, "legion.yaml")
	if err := os.WriteFile(path, []byte(booted(dir)), 0o600); err != nil {
		t.Fatalf("write legion.yaml: %v", err)
	}
	live, err := config.NewLive(path, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	rig := &runtimeSwapRig{t: t, keeper: settings.New(live, nil, quietLogger()), path: path, tmuxFor: tmuxRuntimeFor(t, *live.Snapshot())}
	live.OnChange(rig.observe)
	return rig
}

// tmuxRuntimeFor is the tmux runtime build of this deployment: the closure prepareTmux installs
// for a daemon booting on these files (daemon.go's p.runtimeFor), with no runtime handed to it,
// which is the one call every rebuild of a tmux daemon's runtime goes through. A rig booted on
// the other runtime's file builds it all the same, since nothing of it comes from the runtime
// the file names; what it is given is the configuration of each settle.
func tmuxRuntimeFor(t *testing.T, cfg config.Config) func(config.Config) (runtimeFactory, error) {
	t.Helper()
	var p plan
	if err := prepareTmux(cfg, quietLogger(), overrides{}, "", tmuxReads{}, &p); err != nil {
		t.Fatalf("prepare the tmux runtime build: %v", err)
	}
	return p.runtimeFor
}

// kubernetesSwapBlock is the `runtime` key of a Kubernetes deployment, its kubeconfig the one
// newRuntimeSwapRig wrote; tmuxSwapBlock is the tmux one, with a pane key of its own, so a
// settle that kept the selector and dropped the block would be seen on either side.
func kubernetesSwapBlock(dir string) string {
	return fmt.Sprintf(`runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/cybermonklabs/legion-worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    storage_class: gp2
    session_store: postgres
    session_dsn_secret: SESSION_DSN
    kubeconfig: %s/kubeconfig
    context: legion-daemon
`, dir)
}

func tmuxSwapBlock(dir string) string {
	return fmt.Sprintf("runtime:\n  tmux:\n    pi_config_files: [%s/overlay.json]\n", dir)
}

// An operator rewrites `runtime` from one runtime to the other under a running daemon — the
// edit docs/deployment.md has them make before a restart — and the daemon settles nothing of
// that file: it goes on running the runtime it booted on with every member of that runtime's
// block, and no rebuild of anything is asked for (config.Live.Apply).
//
// The selector pinned on its own would kill it. `runtime` is one key carrying the selector and
// the block it names (config's readRuntime), so a settle that pinned the name and took the
// file's block leaves `kubernetes` beside no `runtime.kubernetes`, and the rebuild that follows
// reads the Options of every pod out of that nil block (readSandbox's `*cfg.Runtime.Kubernetes`)
// — inside config.Live.Apply, under the settings holder's lock, in a daemon that is supervising
// pods, with no recover between the file watcher and that line.
//
// The second settle of each row is the control: an edit of that same deployment's own file
// settles, and the build it asks for answers nothing. So the silence above is a reading this
// daemon refused, and not a rig whose builds never run.
func TestASwapOfTheRuntimeKeyLeavesTheDaemonRunningTheRuntimeItBootedOn(t *testing.T) {
	for _, tc := range []struct {
		name            string
		booted, swapped func(dir string) string
	}{
		{"kubernetes to tmux", kubernetesSwapBlock, tmuxSwapBlock},
		{"tmux to kubernetes", tmuxSwapBlock, kubernetesSwapBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRuntimeSwapRig(t, func(dir string) string { return runtimeSwapFile(t, dir, tc.booted(dir)) })
			dir := filepath.Dir(rig.path)
			booted := rig.keeper.Live().Snapshot().Runtime

			applied, err := rig.settle(runtimeSwapFile(t, dir, tc.swapped(dir)))

			if len(rig.died) > 0 {
				t.Fatalf("the daemon died rebuilding its runtime for the settled configuration: %s", strings.Join(rig.died, "; "))
			}
			if err != nil {
				t.Fatalf("the file settle: %v", err)
			}
			if len(rig.built) != 0 {
				t.Fatalf("the settle asked for the runtime builds %v, want none at all: a reading of the other runtime settles nothing", rig.built)
			}
			if len(applied) != 0 {
				t.Fatalf("the settle applied %+v, want nothing: not one key of the other runtime's reading settles", applied)
			}
			if settled := rig.keeper.Live().Snapshot().Runtime; !reflect.DeepEqual(settled, booted) {
				t.Fatalf("the settled runtime = %+v, want the whole of the boot's %+v: the daemon supervises that runtime's processes", settled, booted)
			}

			applied, err = rig.settle(runtimeSwapFile(t, dir, tc.booted(dir)) + "admission_cap: 7\n")

			if len(rig.died) > 0 {
				t.Fatalf("the daemon died rebuilding its runtime for its own deployment's configuration: %s", strings.Join(rig.died, "; "))
			}
			if err != nil {
				t.Fatalf("the settle of this deployment's own file: %v", err)
			}
			if len(rig.built) != 1 || rig.built[0] != nil {
				t.Fatalf("the runtime builds of that settle answered %v, want the one build the settle asked for, answering nothing", rig.built)
			}
			if len(applied) != 1 || applied[0].Path != "admission_cap" {
				t.Fatalf("that settle applied %+v, want the one key the file changed", applied)
			}
		})
	}
}

// The tmux runtime build is the closure prepareTmux leaves behind, and its first act is to
// refuse a configuration that is not this runtime's (daemon.go's p.runtimeFor, ownRuntime).
// Every build of a tmux daemon's runtime goes through it, the boot's own and every rebuild's
// (buildRuntime over p.runtimeFor), so a configuration naming the other runtime is answered an
// error there and the daemon keeps the runtime it has (runtimeHolder.rebuild).
//
// The second row is the control: this deployment's own configuration is built, and the factory
// comes back. So the first row is a configuration refused and not a build that answers an
// error whatever it is given.
func TestTheTmuxBuildPrepareTmuxInstallsRefusesAConfigurationThatIsNotItsOwn(t *testing.T) {
	rig := newRuntimeSwapRig(t, func(dir string) string { return runtimeSwapFile(t, dir, tmuxSwapBlock(dir)) })
	own := *rig.keeper.Live().Snapshot()
	other := own
	other.Runtime = config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{Namespace: "legion"}}
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		refusal string
	}{
		{name: "the other runtime's configuration", cfg: other, refusal: `configuration naming runtime "kubernetes"`},
		{name: "its own deployment's configuration", cfg: own},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory, err := rig.tmuxFor(tc.cfg)

			if tc.refusal == "" {
				if err != nil || factory == nil {
					t.Fatalf("the tmux build answered %v and the factory %v, want the factory for its own runtime's configuration", err, factory != nil)
				}
				return
			}
			if err == nil {
				t.Fatalf("the tmux build accepted %+v, want a refusal naming what it was given", tc.cfg.Runtime)
			}
			if !strings.Contains(err.Error(), tc.refusal) {
				t.Errorf("the tmux build answered %q, want it to name %q", err, tc.refusal)
			}
			if factory != nil {
				t.Error("the tmux build answered a factory beside its refusal")
			}
		})
	}
}

// Neither runtime's build dereferences a block the configuration does not carry: a configuration
// that is not the runtime being built from it is answered an error, and the daemon keeps the
// runtime it has (runtimeHolder.rebuild). No loaded configuration is one of these — the loader
// reads the block the selector names and no other, and a reading naming the other runtime
// settles nothing (config.Live.Apply) — and that is the point: a nil block must not be able to
// kill the process even if some later path reaches it. The refusal each row drives is the one
// each build makes: readSandbox's own, and ownRuntime, which is the first act of the tmux
// build prepareTmux installs
// (TestTheTmuxBuildPrepareTmuxInstallsRefusesAConfigurationThatIsNotItsOwn drives that build
// whole).
func TestARuntimeBuildRefusesAConfigurationThatIsNotItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     config.Runtime
		build   func(config.Config) error
		refusal string
	}{
		{
			name:    "the Sandbox build with no kubernetes block",
			cfg:     config.Runtime{Name: "kubernetes"},
			refusal: "no runtime.kubernetes block",
		},
		{
			name:    "the Sandbox build under the other runtime's name",
			cfg:     config.Runtime{Name: "tmux"},
			refusal: `configuration naming runtime "tmux"`,
		},
		{
			name:    "the tmux build carrying the other runtime's block",
			cfg:     config.Runtime{Name: "tmux", Kubernetes: &config.Kubernetes{Namespace: "legion"}},
			build:   func(cfg config.Config) error { return ownRuntime("tmux", cfg) },
			refusal: "carrying a runtime.kubernetes block",
		},
		{
			name:    "the tmux build under the other runtime's name",
			cfg:     config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{Namespace: "legion"}},
			build:   func(cfg config.Config) error { return ownRuntime("tmux", cfg) },
			refusal: `configuration naming runtime "kubernetes"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			build := tc.build
			if build == nil {
				build = func(cfg config.Config) error {
					_, err := readSandbox(cfg, "demo", "dispatch-token", "", lookup(nil), quietLogger())
					return err
				}
			}
			cfg := config.Config{Runtime: tc.cfg}

			var err error
			func() {
				defer func() {
					if died := recover(); died != nil {
						t.Fatalf("the build died on a configuration that is not its own: %v", died)
					}
				}()
				err = build(cfg)
			}()

			if err == nil {
				t.Fatalf("the build accepted %+v, want a refusal naming what it was given", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.refusal) {
				t.Errorf("the build answered %q, want it to name %q", err, tc.refusal)
			}
		})
	}
}

// `runtime: tmux` with no block of its own is what most deployments write, and the tmux build
// takes it: the block is optional (config.Tmux), and a nil one names no pane key (tmuxOptions).
func TestTheTmuxBuildTakesADeploymentWithNoTmuxBlock(t *testing.T) {
	if err := ownRuntime("tmux", config.Config{Runtime: config.Runtime{Name: "tmux"}}); err != nil {
		t.Fatalf("the tmux build refused a deployment with no `runtime.tmux` block: %v", err)
	}
}
