package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/brokerclient"
	"golang.org/x/term"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/registry"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
)

// controllerSecretVariable names the controller capability's secret: its file is written under it,
// and the session finds that file through the variable with "_FILE" appended, as every pane finds
// a secret file.
const controllerSecretVariable = "LEGION_CONTROLLER_SECRET"

// stdinIsTerminal says whether this process's stdin is a terminal; a test replaces it.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// controllerCommands is `legion controller`'s subcommands, the one list of them `legion controller
// --help` names.
var controllerCommands = map[string]command{
	"start": runControllerStart,
}

func runController(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "controller", controllerCommands, args, stdout, stderr)
}

func runControllerStart(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("controller start", "usage: legion controller start --config <controller.yaml> [--daemon-url <url>]", stderr)
	configPath := flags.String("config", "", "path to the operator-side controller.yaml (required)")
	daemonURL := flags.String("daemon-url", "", "the daemon's API address, overriding the file's daemon_url")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if *configPath == "" || flags.NArg() > 0 {
		flags.Usage()
		return 2
	}
	code, err := controllerStart(ctx, *configPath, *daemonURL, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "legion controller start: %v\n", err)
		return 1
	}
	return code
}

// controllerStart is `legion controller start`, the operator's side of the controller under
// `controller: operator`, where the daemon launches none. In order, and nothing is kept, and nothing
// but the probe is launched, until the daemon has answered: read the strict operator-side file;
// refuse an operator token file others can read, then a stdin that is no terminal (stdinIsTerminal:
// given a positional word there, Oh My Pi runs headless print mode, which would run the start
// command after the mint revoked the running controller), a blank or unreadable Envoy token file,
// a NATS nkey seed file that is blank, unreadable, or holds no nkey user seed, a configured
// `dispatch_url` whose file names neither of the two Dispatch credentials, a secrets broker that
// refuses this machine's identity under `dispatch_token_broker` (controllerDispatchBroker) and a
// blank or unreadable Dispatch token file under `dispatch_token_file`, an instructions file that
// is missing or blank, an Oh My Pi invocation that does not resolve, a sign-in directory of
// the controller's own (`<state_dir>/controller-signins`, its AGENT_SECRETS_SIGNIN_DIR) that
// holds any entry, which is otherwise made, or kept, 0700 and empty before anything runs under
// the controller's environment (controllerSignIns says why), a path at its key directory
// (`<state_dir>/controller-keys`, its AGENT_SECRETS_KEY_DIR) that is no directory, which is
// otherwise made, or kept 0700 with what it holds but for a machine session `agent-secrets` kept
// there, which the start drops whenever a helper socket is set or present in the controller's
// environment, so one read made while the helper was down cannot pin later reads to the machine
// identity, and a drop of that session that fails (controllerKeys says why);
// then probe that Oh My Pi as the controller will run it — the launch prefix, the invocation, the
// controller's whole environment, in
// `<state_dir>/controller`, created for it, at the operator's terminal — and refuse a
// pi-legion it does not load, loads without pi-envoy or beside the pre-split package, or loads
// speaking another daemon API contract than this binary's, each of which would refuse the
// controller at session start (daemon.ProbeController). The probe
// runs `omp models`, which starts no session, so it neither registers, takes the controller role,
// nor reads a controller secret. Then fetch the controller secret with the operator token as a
// bearer, naming the contract the probe held the plugin to, which the daemon refuses before it
// mints when it is not its own (the daemon mints a fresh capability and revokes the previous
// controller's); write it 0600 under the local state directory beside the `legion` launcher and
// the deployment instructions; then fetch the controller's GitHub credential
// (fetchControllerCredential) and write it under `<state_dir>/gh` for `gh` to read, or, on a
// daemon with no GitHub App for this project, an empty 0700 directory and nothing else; any other
// refusal of that fetch is a failure like every other here, before Oh My Pi runs. A fetch that
// found a credential starts a goroutine that re-fetches and rewrites that directory every
// controllerGitHubRefreshInterval while Oh My Pi runs, logging to
// `<state_dir>/github-credential.log` rather than this process's streams, and stopping when Oh My
// Pi exits or the daemon says the capability is no longer current
// (controllerCredentialSuperseded); then run Oh My Pi interactive — the launch prefix and the
// resolved invocation with one joined `--append-system-prompt` and the one positional word
// daemon.ControllerStartCommand, no `--resume`, no `--mode rpc`, and daemon.ControllerStartMessage
// as LEGION_CONTROLLER_START_MESSAGE in its environment for that pi-legion command to send as the
// first turn — in the foreground with the same environment, and answer
// its exit code. A refusal before the secret is written removes the directories made for the
// probe, the sign-in directory and the key directory, so the state directory is as it was, but
// for a machine session dropped from the key directory under a helper socket: the helper signs in
// its place.
func controllerStart(ctx context.Context, configPath, daemonURL string, stderr io.Writer) (int, error) {
	absolute, err := filepath.EvalSymlinks(configPath)
	if err == nil {
		absolute, err = filepath.Abs(absolute)
	}
	if err != nil {
		return 0, fmt.Errorf("controller configuration %s could not be read: %w", configPath, err)
	}
	cfg, err := config.LoadController(absolute, daemonURL)
	if err != nil {
		return 0, err
	}
	operatorToken, err := config.ReadPrivateSecretPointer("operator_token_file", cfg.OperatorTokenFile)
	if err != nil {
		return 0, err
	}
	// Before every other check, but after the operator token file's own, which a script with no
	// terminal relies on reaching: with a positional word and a stdin that is no terminal, Oh My Pi
	// runs print mode, headless, and would run the start command only after the mint had revoked the
	// running controller.
	if !stdinIsTerminal() {
		return 0, errors.New("stdin is not a terminal: Oh My Pi would run headless print mode, running the controller start command after the daemon's mint had revoked the running controller; run `legion controller start` at a terminal")
	}
	// Every check this machine can make on its own comes before the fetch, and none of them writes
	// under the state directory but the probe's own directory and the controller's sign-in and key
	// directories, each removed again on a refusal where the start made it: the daemon mints a
	// fresh capability on every request and revokes the incumbent controller's, so a failure found
	// here never cuts the running controller off for nothing.
	if cfg.EnvoyTokenFile != "" {
		if _, err := config.ReadSecretPointer("envoy_token_file", cfg.EnvoyTokenFile); err != nil {
			return 0, err
		}
	}
	if cfg.NatsNkeySeedFile != "" {
		if _, err := natsauth.SeedFile(cfg.NatsNkeySeedFile); err != nil {
			return 0, err
		}
	}
	// Exactly one credential writes the configured Dispatch, and the file says which: the bearer
	// `dispatch_token_file` points at, read here as every other pointer is, or this machine's own
	// identity (`dispatch_token_broker`), traded at the secrets broker once here to prove the
	// controller can hold a credential at all (controllerDispatchBroker). The loader takes a
	// `dispatch_url` with neither, since it reads no environment and the broker route is proved
	// only by trading; naming neither here is a controller that could write nothing, so the start
	// refuses it by name.
	switch {
	case cfg.DispatchURL == "":
	case cfg.DispatchTokenBroker:
		if err := controllerDispatchBroker(ctx, stderr); err != nil {
			return 0, err
		}
	case cfg.DispatchTokenFile != "":
		if _, err := config.ReadSecretPointer("dispatch_token_file", cfg.DispatchTokenFile); err != nil {
			return 0, err
		}
	default:
		return 0, errors.New("dispatch_url is configured with no Dispatch credential: set dispatch_token_file to the bearer's file, or dispatch_token_broker: true where this machine proves an identity of its own and the controller's session trades it at the secrets broker")
	}
	if cfg.InstructionsPath != "" {
		if _, err := config.ReadDeploymentInstructions(cfg.InstructionsPath); err != nil {
			return 0, err
		}
	}
	invocation, err := omplaunch.ResolveInvocation(cfg.OmpInvocation, os.Getenv)
	if err != nil {
		return 0, err
	}
	stateDir := cfg.StateDir
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return 0, fmt.Errorf("find the home directory the controller's state lives under: %w", err)
		}
		stateDir = filepath.Join(filepath.Dir(registry.Path(nil, home)), cfg.Project+"-controller")
	}
	controllerDir := filepath.Join(stateDir, "controller")
	token := string(legionclaim.ControllerToken(cfg.Project))
	gh, err := controllerRealGH(stateDir)
	if err != nil {
		return 0, err
	}
	// One environment, probed and then launched: the operator's own with the controller's set on
	// top, later pairs replacing inherited values of the same name. The controller is pane-side, so
	// the daemon's own NATS seed, which an operator shell that also runs a daemon may export, is
	// dropped by value and by pointer; its pane seed pointer is the controller's set's. A pane's boot
	// token, which a start from inside a Legion pane inherits, is dropped too: the plugin takes a
	// controller carrying one for a controller the daemon launched (`controller: daemon`). So are
	// the Slack channels the daemon tells its own controller (LEGION_SLACK_CHANNELS): only that
	// controller can reply in Slack, and an inherited list is no project's word for this one. A
	// machine that holds its Dispatch credential at the broker drops the operator's own Dispatch
	// bearer and pointer as well: either would be presented in place of the traded token as a
	// bearer Dispatch refuses (controllerDispatchBroker). The drop is not the whole of it — the
	// operator's HOME is the controller's, and the envoy.json there holds that person's
	// `dispatch.token`, which no delete here reaches — so the controller's set carries
	// DISPATCH_TOKEN_BROKER, the session's word that its Dispatch credential is traded at the
	// secrets broker and nothing else, under which the client reads no bearer from any source and
	// turns Dispatch off saying so when it can trade none (dispatch-config.ts, dispatchTarget).
	// That route trades a person's sign-in where the machine holds one, so the set also carries
	// AGENT_SECRETS_SIGNIN_DIR under every configuration, naming the empty directory of the
	// controller's own that controllerSignIns keeps and says why, and AGENT_SECRETS_KEY_DIR,
	// naming the key directory of its own that controllerKeys keeps and says why. The one
	// difference between the probed and the launched environment is GH_CONFIG_DIR, which names
	// the default owner's directory only once the credential fetch after the mint has named that
	// owner.
	env := processEnvironment()
	delete(env, natsauth.DaemonSeedVariable)
	delete(env, natsauth.DaemonSeedFileVariable)
	delete(env, "LEGION_BOOT_TOKEN")
	delete(env, "LEGION_BOOT_TOKEN_FILE")
	delete(env, "LEGION_SLACK_CHANNELS")
	if cfg.DispatchTokenBroker {
		delete(env, "DISPATCH_TOKEN")
		delete(env, "DISPATCH_TOKEN_FILE")
	}
	for _, pair := range controllerEnvironment(cfg, stateDir, runtime.SecretFilePath(stateDir, token), gh) {
		env[pair[0]] = pair[1]
	}
	// The sign-in directory first, since its refusal comes before any directory is made, then the
	// key directory, which drops a machine session kept there whenever a helper socket is set or
	// present in this environment, as the controller's `agent-secrets` will find it, both before
	// the probe, which runs Oh My Pi under this environment; then the probe's own directory. A
	// refusal from here to the mint removes what any of them made (removeDirs), deepest first.
	created, err := controllerSignIns(controllerSignInDir(stateDir))
	if err != nil {
		return 0, err
	}
	made, err := controllerKeys(controllerKeyDir(stateDir), func(name string) string { return env[name] }, stderr)
	if err != nil {
		removeDirs(created)
		return 0, err
	}
	created = append(created, made...)
	if made, err = makeDirs(controllerDir); err != nil {
		removeDirs(created)
		return 0, err
	}
	created = append(created, made...)
	// The project's repository mapping (GET /legion/v1/state, unauthenticated) is read and
	// validated before the capability mint: the mint revokes the incumbent controller's
	// registration and grants (api.controllerSecret), so a repository-mapping failure found here
	// must never cut a running controller off for a replacement that will not start.
	repositories, err := controllerRepositories(ctx, cfg.DaemonURL)
	if err != nil {
		removeDirs(created)
		return 0, err
	}
	fmt.Fprintf(stderr, "[legion] checking the controller's Oh My Pi (%s) in %s before the daemon mints a capability\n",
		omplaunch.WithPrefix(cfg.OmpLaunchPrefix, invocation), controllerDir)
	secret, designSteps, err := probeAndMint(ctx, cfg.DaemonURL, daemon.ControllerProbe{
		Omp: invocation, Prefix: cfg.OmpLaunchPrefix, Env: env, WorkDir: controllerDir, Stdin: os.Stdin, Stderr: stderr,
		Contract: api.DaemonAPIVersion, Log: slog.New(slog.NewTextHandler(stderr, nil)),
	}, operatorToken)
	if err != nil {
		removeDirs(created)
		return 0, err
	}

	if _, err := runtime.WriteSecretFile(stateDir, token, controllerSecretVariable, secret); err != nil {
		return 0, fmt.Errorf("write the controller secret: %w", err)
	}
	stopGitHubRefresh, defaultOwner, err := controllerStartGitHubCredential(ctx, cfg.DaemonURL, secret, stateDir, stderr)
	if err != nil {
		return 0, err
	}
	defer stopGitHubRefresh()
	if defaultOwner != "" {
		env["GH_CONFIG_DIR"] = ghconfig.Dir(controllerGHDir(stateDir), defaultOwner)
	}
	composer, err := prompts.New(stateDir)
	if err != nil {
		return 0, fmt.Errorf("snapshot controller role prompts: %w", err)
	}
	controllerPrompts, err := composer.ControllerPromptPaths(false)
	if err != nil {
		return 0, err
	}
	executable, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("resolve the legion executable the controller's launcher runs: %w", err)
	}
	if err := workerbin.Install(stateDir, executable); err != nil {
		return 0, err
	}
	instructionsFile := ""
	if cfg.InstructionsPath != "" {
		if instructionsFile, err = config.MaterializeDeploymentInstructions(cfg.InstructionsPath, stateDir, cfg.Project); err != nil {
			return 0, err
		}
	}
	// The design-gate steps the daemon gates, then the project's whole repository list (fetched
	// and validated above, before the capability mint), is the controller's addressing, the line
	// its take comment reads before it promises anyone a design approval (skill://legion-controller).
	// The start message travels as LEGION_CONTROLLER_START_MESSAGE (controllerEnvironment) and the
	// command line ends with the word daemon.ControllerStartCommand, whose doc says why.
	addressing := daemon.DesignGateFragment(designSteps) + " " + daemon.RepositoriesSentence(repositories)
	command := omplaunch.WithPrefix(cfg.OmpLaunchPrefix, invocation) + " " + omplaunch.SystemPromptArgument(runtime.PromptParts{
		RolePromptPaths:            controllerPrompts,
		Addressing:                 addressing,
		DeploymentInstructionsPath: instructionsFile,
	}) + " " + shellprefix.Word(daemon.ControllerStartCommand)
	fmt.Fprintf(stderr, "[legion] starting the controller for %s against %s; state in %s\n", cfg.Project, cfg.DaemonURL, stateDir)
	// Interactive and in the foreground: the operator's terminal is Oh My Pi's. The child is not
	// bound to ctx — a Ctrl-C reaches Oh My Pi through the terminal's process group and is its to
	// handle, while this process, whose signals main has already caught, waits for its exit.
	omp := exec.Command("sh", "-c", command)
	omp.Dir = controllerDir
	for _, name := range slices.Sorted(maps.Keys(env)) {
		omp.Env = append(omp.Env, name+"="+env[name])
	}
	omp.Stdin, omp.Stdout, omp.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := omp.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if code := exit.ExitCode(); code > 0 {
				return code, nil
			}
			return 1, nil // killed by a signal
		}
		return 0, fmt.Errorf("run Oh My Pi: %w", err)
	}
	return 0, nil
}

// controllerEnvironment is what the controller's Oh My Pi is told on top of the operator's own
// environment: the controller marker and role, the daemon, project and state directory, the Envoy,
// GitHub and Dispatch settings every pane carries, PI_SHELL_PREFIX, which keeps this state
// directory's legion launcher and gh wrapper first in the agent's bash tool as on every pane, and
// LEGION_CONTROLLER_START_MESSAGE, which the pi-legion extension's daemon.ControllerStartCommand,
// the word on Oh My Pi's command line, sends as the session's first turn
// (daemon.ControllerStartMessage). LEGION_GH_OWNERS_DIR names
// `<state_dir>/gh`, the gh bundle controllerStartGitHubCredential writes and keeps fresh, and
// LEGION_GH_PATH gh, the real gh the wrapper runs; GH_CONFIG_DIR names the bundle's root here, an
// empty directory when the daemon has no GitHub App, and controllerStart points it at the default
// owner's directory once the fetch names one; GH_TOKEN, GITHUB_TOKEN and GH_HOST are blanked so gh
// and git read the bundle's hosts.yml rather than an inherited pointer of the operator's own.
// Secrets travel as `<NAME>_FILE` pointers only, and under `dispatch_token_broker` — this machine
// holding its Dispatch credential at the secrets broker (controllerDispatchBroker) — DISPATCH_URL
// travels with DISPATCH_TOKEN_BROKER=1 and no bearer beside it at all: the client trades for a
// DPoP-bound token of its own, and that variable is what keeps it on that route. Without it the
// client takes the broker route only where no bearer resolves, and the operator's own envoy.json,
// which the controller reads because it keeps the operator's HOME, names a `dispatch.token` that
// resolves first — the controller would write Dispatch as the operator, which is the whole of
// what this setting exists to stop. Set, no bearer of any source is read, and a machine that can
// trade nothing turns Dispatch off saying so rather than falling back to a person's token.
// AGENT_SECRETS_SIGNIN_DIR names `<state_dir>/controller-signins` (controllerSignInDir) under
// every configuration, a sign-in directory of the controller's own that controllerStart keeps
// empty, for the reason controllerSignIns gives, and AGENT_SECRETS_KEY_DIR names
// `<state_dir>/controller-keys` (controllerKeyDir), a key directory of its own, for the reason
// controllerKeys gives, from which controllerStart drops a machine session `agent-secrets` kept
// there whenever a helper socket is set or present, so one read made while the helper was down
// cannot pin later reads to the machine identity. AGENT_SECRETS_HELPER_SOCK and XDG_RUNTIME_DIR
// stay as the operator's environment has them, so the helper whose socket is set or present
// there signs the controller's reads. Later pairs replace any inherited value of the same name.
func controllerEnvironment(cfg config.ControllerConfig, stateDir, secretFile, gh string) [][2]string {
	bin := workerbin.LauncherDir(stateDir)
	env := [][2]string{
		{"LEGION_CONTROLLER", "1"},
		{"LEGION_ROLE", "controller"},
		{"LEGION_DAEMON_URL", cfg.DaemonURL},
		{"LEGION_PROJECT", cfg.Project},
		{"LEGION_STATE_DIR", stateDir},
		{"ENVOY_NATS_URL", strings.Join(cfg.NatsURLs, ",")},
		{"ENVOY_URL", cfg.EnvoyURL},
		// The launcher directory leads exactly once, so a start from inside a Legion pane never
		// puts that pane's legion ahead of this one.
		{"PATH", workerbin.Path(os.Getenv("PATH"), stateDir)},
		{"PI_SHELL_PREFIX", shellprefix.For(bin)},
		{"GH_CONFIG_DIR", controllerGHDir(stateDir)},
		{"GH_TOKEN", ""},
		{"GITHUB_TOKEN", ""},
		{"GH_HOST", ""},
		{"LEGION_GH_OWNERS_DIR", controllerGHDir(stateDir)},
		{"LEGION_GH_PATH", gh},
		{"LEGION_CONTROLLER_START_MESSAGE", daemon.ControllerStartMessage},
		{brokerclient.SignInDirVar, controllerSignInDir(stateDir)},
		{"AGENT_SECRETS_KEY_DIR", controllerKeyDir(stateDir)},
	}
	if cfg.DispatchURL != "" {
		env = append(env, [2]string{"DISPATCH_URL", cfg.DispatchURL})
		if cfg.DispatchTokenBroker {
			env = append(env, [2]string{"DISPATCH_TOKEN_BROKER", "1"})
		} else {
			env = append(env, [2]string{"DISPATCH_TOKEN_FILE", cfg.DispatchTokenFile})
		}
	}
	env = append(env, [2]string{controllerSecretVariable + "_FILE", secretFile})
	if cfg.EnvoyTokenFile != "" {
		env = append(env, [2]string{"ENVOY_TOKEN_FILE", cfg.EnvoyTokenFile})
	}
	if cfg.NatsNkeySeedFile != "" {
		env = append(env, [2]string{natsauth.SeedFileVariable, cfg.NatsNkeySeedFile})
	}
	return env
}

// controllerRealGH is the real gh the controller's `legion gh` runs, its LEGION_GH_PATH: the
// operator's own LEGION_GH_PATH when set, which a start from inside a Legion pane inherits from
// that pane, and otherwise the first gh on the operator's PATH that is not this state directory's
// own wrapper (workerbin.LauncherDir), which would run itself, and that is no script
// (daemon.RefuseScriptGH): a gh wrapper of the operator's machine there would answer in place of
// the review App's token. Either is an executable file; a LEGION_GH_PATH that is relative, absent
// or not executable, a script first on PATH, and no gh at all, are refused before the daemon is
// asked for anything, since the mint that follows replaces the running controller's capability.
func controllerRealGH(stateDir string) (string, error) {
	if gh := os.Getenv("LEGION_GH_PATH"); gh != "" {
		if !filepath.IsAbs(gh) {
			return "", fmt.Errorf("LEGION_GH_PATH %q is not an absolute path: the controller's gh wrapper (legion gh) runs it", gh)
		}
		info, err := os.Stat(gh)
		if err != nil {
			return "", fmt.Errorf("LEGION_GH_PATH %q: %w; the controller's gh wrapper (legion gh) runs it", gh, err)
		}
		if !executableFile(info) {
			return "", fmt.Errorf("LEGION_GH_PATH %q is not an executable file: the controller's gh wrapper (legion gh) runs it", gh)
		}
		return gh, nil
	}
	wrapper := workerbin.LauncherDir(stateDir)
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) == wrapper {
			continue
		}
		gh := filepath.Join(dir, "gh")
		if info, err := os.Stat(gh); err == nil && executableFile(info) {
			if err := daemon.RefuseScriptGH(gh); err != nil {
				return "", err
			}
			return gh, nil
		}
	}
	return "", errors.New("gh is not on PATH: the controller's gh wrapper (legion gh) runs it; install gh or set LEGION_GH_PATH to an absolute executable path")
}

// executableFile says whether info is a regular file someone may execute.
func executableFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// probeAndMint is the controller probe and then the one daemon call, which mints the capability:
// the probe's refusal comes before the mint, never after it, and the call names the contract the
// probe held the plugin to, so a daemon of another contract refuses it before minting.
func probeAndMint(ctx context.Context, daemonURL string, probe daemon.ControllerProbe, operatorToken string) (string, []record.DesignStep, error) {
	if err := daemon.ProbeController(ctx, probe); err != nil {
		return "", nil, err
	}
	return fetchControllerSecret(ctx, daemonURL, operatorToken, probe.Contract)
}

// controllerRepositories is the project's repository mapping, read from the one unauthenticated
// route that serves it (GET /legion/v1/state's `repositories` field, the bytes `legion state
// --json` prints), for the operator's controller addressing (daemon.RepositoriesSentence), which
// names the repositories the way a worker's does. controllerStart reads it before the capability
// mint, so a failure here never revokes a running controller for a replacement that will not start.
// Bounded by requestTimeout, as every other CLI read of the daemon's API is (main.go's get).
//
// An empty or all-refused mapping is never a refusal: this route cannot tell a daemon with no
// workflow (which always serves that shape) from a workflow whose mapping a refresh emptied, and
// refusing either would block the operator action that recovers the project (admitting work,
// AGENTS.md "Handover"). The addressing names the state instead. It refuses only an unusable read:
// a body without a `repositories` object (absent or null, a shape no conforming daemon serves) and
// a mapped entry that is not `owner/name`, which refuses the whole list so the sentence never
// names a silently narrowed subset.
func controllerRepositories(ctx context.Context, daemonURL string) (api.RepositoriesView, error) {
	timed, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	status, body, err := operator{base: daemonURL}.do(timed, http.MethodGet, "/legion/v1/state", nil)
	if err != nil {
		return api.RepositoriesView{}, fmt.Errorf("could not reach the Legion daemon at %s: %v; is the port-forward running? (never falls back to another address)", daemonURL, err)
	}
	if status/100 != 2 {
		return api.RepositoriesView{}, fmt.Errorf("%s/legion/v1/state: the daemon answered %d: %s", daemonURL, status, strings.TrimSpace(string(body)))
	}
	var state struct {
		Repositories *api.RepositoriesView `json:"repositories"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return api.RepositoriesView{}, fmt.Errorf("%s/legion/v1/state answered with a body that is not a JSON object naming repositories: %w", daemonURL, err)
	}
	if state.Repositories == nil {
		return api.RepositoriesView{}, fmt.Errorf("%s/legion/v1/state's body names no repositories field (null or absent): not a shape a conforming daemon ever serves", daemonURL)
	}
	for _, mapped := range state.Repositories.Mapped {
		if _, err := ghrepo.Parse("repositories.mapped", mapped.Repo); err != nil {
			return api.RepositoriesView{}, fmt.Errorf("%s/legion/v1/state names an invalid mapped repository %q: %w", daemonURL, mapped.Repo, err)
		}
	}
	return *state.Repositories, nil
}

// controllerSignInDir is the controller's own sign-in store within stateDir, its
// AGENT_SECRETS_SIGNIN_DIR (controllerEnvironment), which controllerSignIns keeps empty.
func controllerSignInDir(stateDir string) string {
	return filepath.Join(stateDir, "controller-signins")
}

// controllerSignIns makes dir, the controller's sign-in directory (controllerSignInDir), 0700
// and empty, and answers the directories it created (makeDirs) for a refusal to remove.
//
// Why a directory of its own: the controller keeps the operator's HOME and XDG_STATE_HOME, and
// the sign-in store every client reads when AGENT_SECRETS_SIGNIN_DIR is unset lies under them
// (packages/envoy's brokerclient.SignIns, packages/broker-client's signins.ts), where
// `agent-secrets login` keeps the operator's record. Every credential the controller's session
// trades at the secrets broker — its Dispatch credential under `dispatch_token_broker`, its
// Envoy credential wherever no bearer names one — would find that record and trade the
// operator's sign-in, and the controller would act for that person. An empty directory of its
// own keeps both on this machine's identity, under every configuration.
//
// So a directory there that already holds any entry is refused, naming it and what it holds: a
// record there would be traded as a person's sign-in, and nothing here tells a stale copy from
// one put there on purpose, so the operator empties it. A path there that is no directory, a
// symbolic link included, which could name a store that is not the controller's, is refused
// too. An empty directory of another mode is made 0700, the mode every sign-in store has
// (brokerclient.SignInStore.Write), rather than refused: it is the controller's own and holds
// nothing, so a refusal later in the start leaves it 0700 rather than undoing that. Every refusal
// here comes before the probe and before the mint, which revokes the running controller
// (controllerStart).
func controllerSignIns(dir string) ([]string, error) {
	return controllerOwnDir(dir, "sign-in directory", "AGENT_SECRETS_SIGNIN_DIR, which this start makes 0700 and keeps empty", func() error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("read the controller's sign-in directory %s: %w", dir, err)
		}
		if len(entries) > 0 {
			names := make([]string, len(entries))
			for i, entry := range entries {
				names[i] = entry.Name()
			}
			return fmt.Errorf("the controller's sign-in directory %s holds %s: it is the controller's AGENT_SECRETS_SIGNIN_DIR, kept empty so that every credential its session trades at the secrets broker trades this machine's identity, and a sign-in there would act for a person; empty it and start again", dir, strings.Join(names, ", "))
		}
		return nil
	})
}

// controllerKeyDir is the controller's own agent-secrets key directory within stateDir, its
// AGENT_SECRETS_KEY_DIR (controllerEnvironment), which controllerKeys makes 0700.
func controllerKeyDir(stateDir string) string {
	return filepath.Join(stateDir, "controller-keys")
}

// controllerKeys makes dir, the controller's key directory (controllerKeyDir), 0700, keeping
// what it holds but a machine session `agent-secrets` kept there, which it drops whenever a
// helper socket is set or present under getenv, the controller's environment; it says so on
// stderr, and answers the directories it created (makeDirs) for a refusal to remove.
//
// Why a directory of its own: the controller's session runs `agent-secrets` for its secret reads
// (a Slack thread's, skill://legion-controller). With no key.pem in its key directory, no sign-in
// (controllerSignIns) and no helper socket set or present, that command registers a machine
// session for a machine that proves an identity of its own and writes its key, enrollment and
// marker into the key directory, AGENT_SECRETS_KEY_DIR else $XDG_RUNTIME_DIR/agent-secrets
// (packages/envoy's cmd/agent-secrets, keyDir and selectSigner). In the operator's key
// directory, every later `agent-secrets` call in the operator's own shells would then sign as
// that session. So the controller's reads sign as the helper's session whenever a helper socket
// is set or present, as a machine session of the controller's own, kept here, where
// AGENT_SECRETS_MACHINE_IDENTITY is set and no helper socket is, and otherwise fail with `no
// session identity`. Neither a key.pem in the operator's key directory (an agent box's, or a
// pod's that ran `agent-secrets enroll --pod`) nor the operator's sign-in signs them, which is
// intended: either would make the controller sign as a session or a person it is not.
//
// Why the drop: `agent-secrets` takes a key.pem in its key directory before the helper, so a
// machine session kept here from one read made while no helper socket was set or present — the
// helper down, or not yet installed — would sign every later read, across restarts and reboots,
// as a machine identity that may hold none of the grants the controller reads. So whenever a
// helper socket is set or present in the controller's environment (brokerclient.HelperSocket,
// the one rule `agent-secrets` takes the helper by, read in the environment the controller's
// `agent-secrets` runs in), the start drops that session (brokerclient.DropMachineSession) and
// the helper signs; a socket that is set with nothing listening fails those reads rather than
// registering a session anew. With no helper socket set or present, what the directory holds is
// kept: it is the machine session the controller's `agent-secrets` registered at an earlier
// start, which the command uses first while it is here. A key.pem with no `machine-session`
// marker beside it is no session `agent-secrets` registered for this machine (a launcher's, or
// one `agent-secrets enroll --pod` wrote), and is kept either way.
//
// A path there that is no directory, a symbolic link included, which could name the operator's
// key directory, is refused, and so is a drop that fails, naming the directory and the socket; a
// directory of another mode is made 0700, the mode the command makes a key directory with. Every
// refusal here comes before the probe and before the mint, which revokes the running controller
// (controllerStart).
func controllerKeys(dir string, getenv func(string) string, stderr io.Writer) ([]string, error) {
	return controllerOwnDir(dir, "key directory", "AGENT_SECRETS_KEY_DIR, which this start makes 0700 so that the controller's agent-secrets session is kept apart from the operator's own", func() error {
		sock, there := brokerclient.HelperSocket(getenv)
		if !there {
			return nil
		}
		dropped, err := brokerclient.DropMachineSession(dir)
		if err != nil {
			return fmt.Errorf("the machine session kept in the controller's key directory %s could not be dropped: a helper socket is set or present (%s), so the helper, not that session, must sign the controller's agent-secrets reads: %w", dir, sock, err)
		}
		if dropped {
			fmt.Fprintf(stderr, "[legion] dropped the machine session kept in the controller's key directory %s: a helper socket is set or present (%s), so the helper signs the controller's agent-secrets reads\n", dir, sock)
		}
		return nil
	})
}

// controllerOwnDir makes dir, a directory of the controller's own within its state directory,
// 0700, and answers the directories it created (makeDirs) for a refusal to remove: the one shape
// of controllerSignIns and controllerKeys. what names the directory in every refusal ("sign-in
// directory", "key directory"), and role is what the refusal of a path there that is no
// directory says it is, after "it is the controller's ". kept runs only on a directory that was
// there already, before it is made 0700, and its error is the start's refusal, which leaves the
// directory's mode as it was.
func controllerOwnDir(dir, what, role string, kept func() error) ([]string, error) {
	var created []string
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if created, err = makeDirs(dir); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("the controller's %s %s could not be read: %w", what, dir, err)
	case !info.IsDir():
		return nil, fmt.Errorf("the controller's %s %s is not a directory (mode %v): it is the controller's %s; remove it and start again", what, dir, info.Mode(), role)
	default:
		if err := kept(); err != nil {
			return nil, err
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		removeDirs(created)
		return nil, fmt.Errorf("make the controller's %s %s 0700: %w", what, dir, err)
	}
	return created, nil
}

// makeDirs creates dir and its missing parents, 0700, and answers the ones it created, deepest
// last.
func makeDirs(dir string) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || filepath.Dir(d) == d {
			break
		}
		missing = append([]string{d}, missing...)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		removeDirs(missing)
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	return missing, nil
}

// removeDirs removes the directories makeDirs created, deepest first, each only while empty.
func removeDirs(created []string) {
	for i := len(created) - 1; i >= 0; i-- {
		_ = os.Remove(created[i])
	}
}

// fetchControllerSecret is `POST /legion/v1/controller/secret` with the operator token as a
// bearer and the plugin's daemon API contract in the body, answering the capability and the
// design-gate steps the deployment gates. A failed request names the daemon URL and never tries
// another address; a refusal quotes the daemon's `error`, except a daemon from before contract
// 12, which refuses the contract field itself as unknown, so the refusal names the side to
// upgrade instead.
//
// An answer that names no step list at all (`designGateSteps` absent or null) is a daemon from
// before contract 55, and one that names a step this release does not know is a daemon beyond
// it: both are refused rather than guessed at, since the controller would otherwise promise, or
// withhold, a design approval on an assumption. The empty list is not that case — it is a
// deployment that gates no step, which the daemon answers as `[]`.
func fetchControllerSecret(ctx context.Context, daemonURL, operatorToken string, contract int) (string, []record.DesignStep, error) {
	const route = "/legion/v1/controller/secret"
	status, body, err := operator{base: daemonURL, bearer: operatorToken}.do(ctx, http.MethodPost, route, api.ControllerSecretRequest{PluginContract: contract})
	if err != nil {
		return "", nil, fmt.Errorf("could not reach the Legion daemon at %s: %v; is the port-forward running? (never falls back to another address)", daemonURL, err)
	}
	if status/100 != 2 {
		var refused legionclaim.Refusal
		if status == http.StatusBadRequest && json.Unmarshal(body, &refused) == nil &&
			strings.Contains(refused.Message, `unknown field "pluginContract"`) {
			return "", nil, fmt.Errorf("%s%s: the daemon speaks a daemon API contract older than 12, and this legion speaks %d; upgrade the daemon to this legion's release, or start the controller with the legion built with that daemon", daemonURL, route, contract)
		}
		hint := ""
		if status == http.StatusForbidden {
			hint = " — the operator token does not match the daemon's operator_token_file"
		}
		return "", nil, fmt.Errorf("%s%s: %s%s", daemonURL, route, refusal(status, body), hint)
	}
	var answer api.ControllerSecretResponse
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", nil, fmt.Errorf("%s%s answered with a body that is not JSON", daemonURL, route)
	}
	if answer.Secret == "" {
		return "", nil, fmt.Errorf("%s%s answered with no secret", daemonURL, route)
	}
	if answer.DesignGateSteps == nil {
		return "", nil, fmt.Errorf("%s%s answered no design gate steps (designGateSteps); upgrade the daemon to this legion's release", daemonURL, route)
	}
	for _, step := range answer.DesignGateSteps {
		if !step.Valid() {
			return "", nil, fmt.Errorf("%s%s answered design gate step %q, not %s; upgrade the daemon to this legion's release", daemonURL, route, step, record.StepWords())
		}
	}
	return answer.Secret, answer.DesignGateSteps, nil
}
