package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/sjawhar/envoy/listenfd"
	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/registry"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	defaultConfigPath = "./legion.yaml"
	// stopTimeout bounds the wait for a stopping daemon, which returns at most 24 s after its stop
	// begins once its boot is recorded: its stop budget, the close of the workflow's NATS
	// connection, the boot's stamp and the store's close (the daemon package's stopBudget). A
	// signal before then waits out the boot step under way first, each bounded by the boot's own
	// 30 s (the daemon package's Run).
	stopTimeout = 30 * time.Second
	// aliveInterval is how often a wait re-asks whether a process is gone.
	aliveInterval = 20 * time.Millisecond
)

// requestTimeout bounds a CLI read of a daemon's HTTP API: one budget for every read these
// commands make, rather than one per route, because a budget sized for a small body and a second
// sized for a large one drift apart and nobody re-sizes either as the fleet grows.
//
// Sixty seconds, from a measurement rather than from a failure. Read on 2026-11-12 from an issue
// pod of the INFRA deployment (the test handoff of dispatch://INFRA-2554): six reads of `GET
// /legion/v1/state` of that daemon, 2368 issues and 610 of them waiting, answered in 7.05 s,
// 7.06 s and 7.30 s to the first byte and 7.07 s, 7.08 s and 7.32 s for the whole 4.0 MB body.
// That is a dated reading of one deployment and no property of the daemon: the body grows with
// the issues a project holds, and the issue that reading came from adds a sentence to every
// waiting issue and an answer to every issue that is not working. A reader who needs another
// number re-sizes from that measurement rather than from a guess, and takes a reading of the
// deployment in hand.
//
// What it buys and what it costs, both halves, since the second is the price of the first. A
// daemon that is merely slow is read instead of given up on, which is the common case, and a
// command that cannot run at all is the worse failure. A daemon that accepts a connection and then
// hangs now keeps an operator waiting a minute, where five seconds used to end it; a daemon that
// is down refuses the connection and still fails at once, so the minute is the hung case alone.
// The refusal names the route, how long the read took and this budget (get), so that minute reads
// as a budget passed rather than as a mystery. It is a variable so that this package's tests can
// give it their own deadline (programbound): a stand-in daemon a loaded machine made slower than
// the minute would be given up on, and the row would read the refusal.
var requestTimeout = 60 * time.Second

func init() {
	programbound.Register(&requestTimeout)
}

// revision is the commit the binary was built from, linked in by the worker image's build
// (packages/daemon/docker/worker.Dockerfile) and the legion release (.github/workflows/release.yaml)
// with `-ldflags "-X main.revision=<commit>"`; empty otherwise.
var revision string

// release is the legion-v* version a release binary was built for, linked in by the release
// workflow with `-ldflags "-X main.release=v<version>"`; empty in every other build.
var release string

// piLegionVersion and piEnvoyVersion are the `@sjawhar/pi-legion` and `@sjawhar/pi-envoy`
// versions this release published, linked in by the release workflow with
// `-ldflags "-X main.piLegionVersion=<version> -X main.piEnvoyVersion=<version>"` beside
// revision and release; both empty in every other build. They are this binary's own plugin
// pair for a stale-plugin relaunch under Kubernetes (LEGION-164) when `legion.yaml` names none
// (`runtime.kubernetes.plugins`) — `legion version`'s second line, and start's and
// `legion config check`'s resolution of the pair to install (applyLinkedPlugins, daemon.ResolvedPlugins).
var piLegionVersion, piEnvoyVersion string

// helpRequested says whether a pass-through command's first argument asks for its usage: such a
// command reads its own arguments (passThrough), --help among them.
func helpRequested(arg string) bool {
	return arg == "-h" || arg == "-help" || arg == "--help"
}

func newVersionCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print the version and the commit the binary was built from",
		Args:  noArguments,
		RunE: runs(func(context.Context, []string) int {
			version := "(devel)"
			if release != "" {
				version = release
			} else if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
				version = info.Main.Version
			}
			if revision != "" {
				fmt.Fprintf(stdout, "legion %s commit %s\n", version, revision)
			} else {
				fmt.Fprintf(stdout, "legion %s\n", version)
			}
			if piLegionVersion != "" && piEnvoyVersion != "" {
				fmt.Fprintf(stdout, "plugins @sjawhar/pi-legion %s @sjawhar/pi-envoy %s\n", piLegionVersion, piEnvoyVersion)
			}
			return 0
		}),
	}
}

// configOnly refuses a word: the configuration is named only by --config, since a file given as
// an argument would otherwise be ignored while ./legion.yaml is read, and `legion config check`
// would vouch for the wrong file.
func configOnly(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError{fmt.Errorf("unexpected argument %q; name the configuration with --config <file>", args[0])}
	}
	return nil
}

func newStartCommand(stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "run the Legion daemon from legion.yaml in the foreground",
		Args:  configOnly,
	}
	configPath := cmd.Flags().String("config", defaultConfigPath, "path to legion.yaml")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int { return start(ctx, *configPath, stderr) })
	return cmd
}

// newConfigCheckCommand is `legion config check`, checkConfig on the file --config names.
func newConfigCheckCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "check legion.yaml as `legion start` reads it, running none of its key commands, and exit without starting",
		Args:  configOnly,
	}
	configPath := cmd.Flags().String("config", defaultConfigPath, "path to legion.yaml")
	cmd.RunE = runs(func(context.Context, []string) int { return checkConfig(*configPath, stdout, stderr) })
	return cmd
}

// applyLinkedPlugins fills cfg.Runtime.Kubernetes.Plugins from this binary's linked pair
// (piLegionVersion, piEnvoyVersion) when the file names none: the file's
// runtime.kubernetes.plugins wins when it is set, the binary's linked pair otherwise, and neither
// leaves the daemon knowing no plugin pair at all (LEGION-164). PluginsFromBinary records which,
// for the boot log and `legion config check` to name. A no-op under tmux, or once the
// file already named a pair.
func applyLinkedPlugins(cfg *config.Config) {
	k := cfg.Runtime.Kubernetes
	if k == nil || k.Plugins != nil || piLegionVersion == "" || piEnvoyVersion == "" {
		return
	}
	k.Plugins = &config.PluginsConfig{PiLegion: piLegionVersion, PiEnvoy: piEnvoyVersion}
	k.PluginsFromBinary = true
}

// pluginsSourceWords is ResolvedPlugins' source (daemon.PluginsFromFile or daemon.PluginsFromBinary)
// as the `legion config check` line's parenthetical names it.
func pluginsSourceWords(source string) string {
	if source == daemon.PluginsFromBinary {
		return "the binary"
	}
	return source
}

// pluginsLine is `legion config check`'s plugins line: the pair this daemon would install for a
// stale-plugin relaunch and where it came from (pluginsSourceWords), or the gap when it knows
// none. When worker_backends runs any role on Claude Code, the line gains one sentence naming
// those roles: their plugin is the worker image's claude-envoy, which no relaunch installs (the
// daemon package's stalePluginInstall refuses such a claim), so a plugin-contract change for one
// of them is answered by a new image.
func pluginsLine(cfg config.Config) string {
	line := "plugins: none — set runtime.kubernetes.plugins or run a release build; a daemon that knows none relaunches no process on another plugin contract"
	if pair, source := daemon.ResolvedPlugins(cfg); pair != nil {
		line = fmt.Sprintf("plugins: @sjawhar/pi-legion %s @sjawhar/pi-envoy %s (from %s)",
			pair.PiLegion, pair.PiEnvoy, pluginsSourceWords(source))
	}
	// The roles Claude Code may run at all (runtime.Backends), in claim.Roles' order, the default
	// steps' order: a role worker_backends puts on it is always one of them, since the loader refuses any
	// other.
	var claude []string
	for _, role := range runtime.Backends[runtime.BackendClaude].Roles {
		if cfg.BackendFor(role) == runtime.BackendClaude {
			claude = append(claude, string(role))
		}
	}
	if len(claude) == 0 {
		return line
	}
	return line + fmt.Sprintf(". Claude Code runs %s, whose plugin is the worker image's: a plugin-contract change for such a role is answered by a new image, not by an install.",
		strings.Join(claude, ", "))
}

// checkConfig is `legion config check`: every refusal the configuration's read at
// boot makes, through the loader that runs neither GitHub App's private_key_command nor any
// secretsd read, then every refusal boot makes from the configuration, the environment and the
// files they name before it writes anything (daemon.CheckStart, the reads boot's own prepare
// starts with, the daemon's own NATS nkey seed among them, then the store's refusals of a
// postgres_dsn, which daemon.CheckStart lists), and nothing else: no store connection, no team, no
// file written, and no process but `mise where <tool>` when a tmux configuration's omp_invocation
// names a mise tool. Under dispatch_token_broker it reads the transition token's file where the
// file names one, and no identity variable: the trade at the secrets broker is boot's alone
// (dispatch://INFRA-4425 R1). Each refusal is written to stderr through printRefusal, as `legion
// start` writes its own, so a URL a refusal quotes loses its query and its userinfo in both
// commands. The OK line names the design-gate steps the file resolved to, in the order it named
// them (`design-gate-steps=`, `none` for a deployment that gates none), and each NATS nkey seed's
// public key when there is one, never the seed.
// Right after it comes one line per loader warning (config.Config.Warnings) — a key the file
// still sets that this daemon no longer reads, whether a project sets it or the file does at its
// top level — in the words boot logs it with: what the key would have been refused for, and what
// to change. A deprecation is the first thing an operator reads that way, and the exit stays 0,
// since such a key is ignored and never refused.
// Then two lines per project, in key order: one naming its `production_check` and what the word
// decides (productionCheckLine), so an operator reads back which projects open a production
// check at a merge, and one naming its `default_steps` (defaultStepsLine), so an operator reads
// back the steps an issue runs when its owner gives no plan. After them, one line per configured
// repository rename
// (`repository_renames`), so an operator reads back what the daemon will treat as one repository
// before a transfer moves the events onto the new name; a file that configures none prints no
// such line. Then one line per deployment capability the file alone leaves open, in the words
// boot logs it with
// (capabilities.Deployment.OpenFromConfiguration): a report, so the exit stays 0.
// Then, when the file sets what turns the project's pause off (pause.Unavailable: runtime: tmux,
// an idle release of 0 minutes, or a wake pacing of 0), one line `pause unavailable: <setting>`,
// and the exit stays 0: such a daemon answers every pause PAUSE_UNAVAILABLE naming that setting,
// and this is where an operator reads it before a pause is needed (decision 21 of
// dispatch://INFRA-3759). It is a line rather than a capability row, which every controller tick
// of every tmux daemon would report open.
// Then, under either runtime, the intake handler limit's line, but only when
// worker_rpc_timeout_seconds derived a limit above intake's ten-minute floor
// (daemon.IntakeHandlerLimitLine): a check of a file that leaves the floor in place says nothing
// an operator cannot read in the daemon's own default, while the boot log names the limit either
// way.
// Under kubernetes, when a pod's reservation requests less cpu or memory than its limit, one plain
// `resources:` line names those pods as Burstable (capabilities.Deployment.BurstableLine);
// a file that sets no lower request prints none, its pods all Guaranteed. Then, when the image
// probe's attempt budget is derived above slow_command_timeout_seconds (daemon.ProbeBudget), one
// plain line names the derived bound and why; then one names the repository cache each repository
// a live record names gets, as the file settles it;
// then a last plain line names the plugin pair this daemon would install for a stale-plugin
// relaunch — the file's or the binary's — or, with neither, the gap, and, when worker_backends
// runs any role on Claude Code, one sentence more saying that such a role's plugin is the image's
// (pluginsLine); it is its own line rather than a capability row, since it would otherwise be
// open on every unreleased build. All of them run only under kubernetes, since the probe and the
// caches run only there.
func checkConfig(configPath string, stdout, stderr io.Writer) int {
	cfg, err := config.LoadForValidation(configPath, nil)
	if err == nil {
		applyLinkedPlugins(&cfg)
	}
	paneNatsUser, daemonNatsUser := "", ""
	if err == nil {
		paneNatsUser, daemonNatsUser, err = daemon.CheckStart(cfg, os.LookupEnv)
	}
	if err != nil {
		printRefusal(stderr, "legion config check", err)
		return 1
	}
	ok := "Config OK: project=" + cfg.Project + " design-gate-steps=" + designGateSteps(cfg.Gates.Steps())
	if paneNatsUser != "" {
		ok += " nats-nkey-user=" + paneNatsUser
	}
	if daemonNatsUser != "" {
		ok += " nats-daemon-nkey-user=" + daemonNatsUser
	}
	fmt.Fprintln(stdout, ok)
	for _, warning := range cfg.Warnings {
		fmt.Fprintln(stdout, warning)
	}
	for _, key := range slices.Sorted(maps.Keys(cfg.Projects)) {
		fmt.Fprintln(stdout, productionCheckLine(key, cfg.Projects[key]))
		fmt.Fprintln(stdout, defaultStepsLine(key, cfg.Projects[key]))
	}
	for _, rename := range cfg.RepositoryRenames.Pairs() {
		fmt.Fprintf(stdout, "repository rename: %s and %s are one repository wherever the daemon compares a name; keep the pair configured for as long as any record names %s\n",
			rename.From, rename.To, rename.From)
	}
	deployment := daemon.Deployment(cfg)
	for _, gap := range deployment.OpenFromConfiguration() {
		fmt.Fprintln(stdout, gap.OpenLine())
	}
	if setting, unavailable := pause.Unavailable(cfg); unavailable {
		fmt.Fprintln(stdout, "pause unavailable: "+string(setting))
	}
	if line, derived := daemon.IntakeHandlerLimitLine(cfg); derived {
		fmt.Fprintln(stdout, line)
	}
	if cfg.Runtime.Name == "kubernetes" {
		if line := deployment.BurstableLine(); line != "" {
			fmt.Fprintln(stdout, "resources: "+line)
		}
		if line, derived := daemon.ProbeBudgetLine(cfg); derived {
			fmt.Fprintln(stdout, line)
		}
		cache := cfg.Runtime.Kubernetes.RepositoryCache
		fmt.Fprintf(stdout, "repository cache: a %s volume for each repository a live record names, fetched from GitHub every %d minutes, kept %d hours after no live record names it; its container reserves cpu %s, memory %s, ephemeral storage %s under %s\n",
			cache.Volume, cache.RefreshMinutes, cache.RetainHours, reservedUnder(cache.Resources.CPURequest, cache.Resources.CPU),
			reservedUnder(cache.Resources.MemoryRequest, cache.Resources.Memory), cache.Resources.EphemeralStorageRequest, cache.Resources.EphemeralStorage)
		fmt.Fprintln(stdout, pluginsLine(cfg))
	}
	return 0
}

// designGateSteps is the deployment's design-gate steps as the OK line names them: the step
// names in the order the file resolved them to, comma-separated, and `none` where the
// deployment gates no step.
func designGateSteps(steps []record.DesignStep) string {
	if len(steps) == 0 {
		return "none"
	}
	return strings.Join(record.StepNames(steps), ",")
}

// productionCheckLine is `legion config check`'s line for one project's `production_check`: the word
// the file writes (`tester` or `off`, config.Project.ProductionCheckWord) and what it decides.
// The role the key names works the check, and `off` opens none; either way the key decides only
// the checks opened after the load, never one already open, which keeps the role it opened with.
func productionCheckLine(key string, project config.Project) string {
	effect := "no production check"
	if role := project.ProductionCheck; role != "" {
		effect = fmt.Sprintf("a production check the %s works", role)
	}
	return fmt.Sprintf("projects.%s.production_check: %s — a merge that closes one of its issues opens %s",
		key, project.ProductionCheckWord(), effect)
}

// defaultStepsLine is `legion config check`'s line for one project's `default_steps`: the steps
// the project runs by default (config.Project.RunsByDefault, today's line up to the owner's
// acceptance for a project that names none), as the file would write the list (plan.Words), and
// what they decide.
func defaultStepsLine(key string, project config.Project) string {
	return fmt.Sprintf("projects.%s.default_steps: [%s] — a task whose plan block names neither steps nor a sub-owner, and the one task of an issue whose owner gives no plan or whose experiment's group takes none, runs these steps, then Legion's retro and merge",
		key, strings.Join(plan.Words(project.RunsByDefault()), ", "))
}

// reservedUnder is a cpu or memory reservation as `legion config check` names it: the one quantity when
// the request is the limit, as at the defaults, and `<request> under <limit>` when the file set a
// lower request.
func reservedUnder(request, limit string) string {
	if request == limit {
		return limit
	}
	return request + " under " + limit
}

// start runs a legion in this process until its context is done. The registry entry is this
// process's one claim on the team — there is no second record to disagree with it — so `legion
// stop`, `legion status` and `legion legions` read one answer and not the daemon's own opinion
// of itself.
//
// Every error start writes to stderr passes the URL rule the log's handler applies to every
// record (printRefusal).
func start(ctx context.Context, configPath string, stderr io.Writer) int {
	log := slog.New(slog.NewJSONHandler(stderr, redact.HandlerOptions(nil)))
	slog.SetDefault(log)

	// The inherited socket is adopted first, before the configuration's key commands run, so none
	// of them inherits its descriptor: from here on only the close-on-exec copy serving it is open.
	inherited, socket, err := adoptListenFD()
	if err != nil {
		printRefusal(stderr, "legion start", err)
		return 1
	}
	if inherited != nil {
		// daemon.Run closes it once it stops serving; this close is for a refusal before then, and
		// is a harmless second close after.
		defer inherited.Close()
	}
	cfg, err := config.Load(configPath, nil)
	if err != nil {
		printRefusal(stderr, "legion start", err)
		return 1
	}
	// Each warning the loader has (config.Config.Warnings) is logged once, here and nowhere
	// further down: daemon.Run logs none of them, so a boot that goes on to serve writes each
	// line once and not twice. They come before any other work this boot does with the
	// configuration — before the team is claimed, which another legion's entry refuses — so a
	// start that never reaches a boot has logged them too, and the lines are the first a reader of
	// the log meets
	// (docs/solutions/envoy/a-setting-another-repositorys-deployment-sets-is-removed-in-two-releases-pr2674.md).
	// The boot goes on: a key another deployment still sets is ignored, never refused.
	for _, warning := range cfg.Warnings {
		log.Warn(warning)
	}
	// legion.yaml always resolves both bind and port, and both must name the inherited socket: the
	// registry entry, the default daemon_url and `legion state --port` all dial them.
	if inherited != nil {
		bind := listenfd.Setting{Name: "bind", Value: cfg.Bind, Set: true}
		port := listenfd.Setting{Name: "port", Value: strconv.Itoa(cfg.Port), Set: true}
		if err := socket.CheckAgainst(bind, port); err != nil {
			printRefusal(stderr, "legion start: "+configPath, err)
			return 1
		}
	}
	applyLinkedPlugins(&cfg)
	cfg.Revision = revision
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		printRefusal(stderr, "legion start: resolve "+configPath, err)
		return 1
	}
	// The state directory is where the worker stream socket, the panes' secret files, and their
	// home live; the registry refuses a second live legion on it, which only holds if every
	// start names it the same way.
	if cfg.StateDir, err = filepath.Abs(cfg.StateDir); err != nil {
		printRefusal(stderr, "legion start: resolve state_dir", err)
		return 1
	}
	legions, err := registryPath()
	if err != nil {
		printRefusal(stderr, "legion start", err)
		return 1
	}

	// Taking the team is one step, not a read and then a write: two starts released together
	// would both pass a separate check, and the loser's release would delete the winner's entry,
	// leaving that daemon serving with nothing to stop or read it by.
	held, claimed, err := registry.Claim(legions, registry.Entry{
		Team:       cfg.Project,
		ConfigPath: absoluteConfig,
		PID:        os.Getpid(),
		Port:       cfg.Port,
		Bind:       cfg.Bind,
		StateDir:   cfg.StateDir,
		StartedAt:  time.Now().UTC(),
	})
	if err != nil {
		printRefusal(stderr, "legion start", err)
		return 1
	}
	if !claimed {
		fmt.Fprintf(stderr, "legion start: %s is already running (pid %d)\n", cfg.Project, held.PID)
		return 1
	}
	// Only this process's own entry: a daemon that replaced it owns the team now.
	defer func() {
		if err := registry.RemoveIf(legions, cfg.Project, os.Getpid()); err != nil {
			printRefusal(stderr, "legion start", err)
		}
	}()

	if err := daemon.Run(ctx, cfg, log, inherited); err != nil {
		printRefusal(stderr, "legion start", err)
		return 1
	}
	return 0
}

// printRefusal writes prefix, ": ", err's text through the URL rule (redact.URLCredentials) and a
// newline to stderr. Every command's stderr passes the rule already (newRoot) but the three that
// hand it to a child, `legion claude-bridge` among them, which writes its refusals through here:
// an error can wrap a *url.Error, which keeps the URL's query and its user name (Go's client masks
// only a password), and pgx's ParseConfigError of a postgres_dsn it cannot parse quotes the DSN
// with its user name and its query (pgx masks only the password).
func printRefusal(stderr io.Writer, prefix string, err error) {
	fmt.Fprintf(stderr, "%s: %s\n", prefix, redact.URLCredentials(err.Error()))
}

// listenFDVariable names the listening TCP socket `legion start` serves its API on in place of
// binding legion.yaml's bind and port: a test harness opens it on port 0 and holds it until the
// daemon serves, so no other program takes the port in between. `legion config check`
// binds nothing and never reads it.
const listenFDVariable = "LEGION_LISTEN_FD"

// adoptListenFD is the listener serving the socket LEGION_LISTEN_FD names, with what Inspect found
// of it, or nil when the variable is unset. Adopt closes the inherited descriptor whether it serves
// or not; a descriptor Inspect refuses is left as it was, for the refused start's exit to close.
// The variable is unset once the socket is adopted: its descriptor number no longer names it, and
// every process the daemon starts, its panes among them, inherits the daemon's environment.
func adoptListenFD() (net.Listener, listenfd.Socket, error) {
	value, set := os.LookupEnv(listenFDVariable)
	if !set {
		return nil, listenfd.Socket{}, nil
	}
	socket, err := listenfd.Inspect(listenFDVariable, value)
	if err != nil {
		return nil, listenfd.Socket{}, err
	}
	listener, err := listenfd.Adopt(socket)
	if err != nil {
		return nil, listenfd.Socket{}, err
	}
	if err := os.Unsetenv(listenFDVariable); err != nil {
		_ = listener.Close()
		return nil, listenfd.Socket{}, fmt.Errorf("unset %s once its socket is adopted: %w", listenFDVariable, err)
	}
	return listener, socket, nil
}

func newStopCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "stop the daemon registered for legion.yaml's project",
		Args:  configOnly,
	}
	configPath := cmd.Flags().String("config", defaultConfigPath, "path to legion.yaml")
	cmd.RunE = runs(func(context.Context, []string) int { return stopLegion(*configPath, stdout, stderr) })
	return cmd
}

// stopLegion is `legion stop`: it stops the daemon registered for configPath's project.
func stopLegion(configPath string, stdout, stderr io.Writer) int {
	// The file names the team and nothing else here, so its App key commands do not run.
	cfg, err := config.LoadForValidation(configPath, nil)
	if err != nil {
		fmt.Fprintf(stderr, "legion stop: %v\n", err)
		return 1
	}
	entry, found, err := findLegion(cfg.Project)
	if err != nil {
		fmt.Fprintf(stderr, "legion stop: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintf(stderr, "legion stop: no legion is registered for %s\n", cfg.Project)
		return 1
	}

	// A daemon that died without cleaning up leaves an entry naming a pid nothing holds: the
	// stop is that record's repair, not an error.
	if !registry.Alive(entry.PID) {
		legions, err := registryPath()
		if err != nil {
			fmt.Fprintf(stderr, "legion stop: %v\n", err)
			return 1
		}
		if err := registry.RemoveIf(legions, cfg.Project, entry.PID); err != nil {
			fmt.Fprintf(stderr, "legion stop: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "legion %s: not running (removed the entry of pid %d, which is gone)\n",
			cfg.Project, entry.PID)
		return 0
	}
	if err := stopProcess(entry.PID); err != nil {
		fmt.Fprintf(stderr, "legion stop: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "legion %s: stopped (pid %d)\n", cfg.Project, entry.PID)
	return 0
}

// runState prints the daemon's state. It reaches the daemon as every operator command does
// (operatorCall): one decision of which daemon the command reads, and the bearer, which this
// command alone may leave out, read from the same --operator-token-file. With one it also names
// who suspended each stranded claim, which the state body does not carry (strandedSuspenders).
func newStateCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "print the daemon's state: admission, the project's pause, issues, pending Dispatch status writes, claims nothing will wake, the live records' repositories GitHub did not read or a GitHub App is missing from, the durable consumers' lag, the intake's open give-ups (--json for all of it)",
		Args:  configOnly,
	}
	c := newOperatorCall(cmd, "file holding the operator bearer the daemon's operator_token_file names; with it the claims nothing will wake name who suspended each", stdout, stderr)
	asJSON := cmd.Flags().Bool("json", false, "print the state as the daemon serves it")
	cmd.RunE = runs(func(ctx context.Context, _ []string) int { return runState(ctx, c, *asJSON, stdout, stderr) })
	return cmd
}

func runState(ctx context.Context, c *operatorCall, asJSON bool, stdout, stderr io.Writer) int {
	if code, ok := c.check(); !ok {
		return code
	}
	op, ok := c.connect()
	if !ok {
		return 1
	}
	address := op.base
	body, err := get(ctx, address+"/legion/v1/state")
	if err != nil {
		fmt.Fprintf(stderr, "legion state: %v\n", err)
		return 1
	}

	if asJSON {
		if _, err := stdout.Write(body); err != nil {
			fmt.Fprintf(stderr, "legion state: %v\n", err)
			return 1
		}
		return 0
	}

	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		fmt.Fprintf(stderr, "legion state: read the state %s served: %v\n", address, err)
		return 1
	}
	fmt.Fprintf(stdout, "legion %s on %s — schema %d, boot %d started %s (first boot %s)\n",
		state.Daemon.Project, address, state.Daemon.SchemaVersion, state.Daemon.Boots,
		state.Daemon.StartedAt.Format(time.RFC3339), state.Daemon.FirstBootAt.Format(time.RFC3339))
	// `admission.trees` is the roots in Legion's work (R22 of dispatch://INFRA-3320): an issue in
	// work with no issue above it in work on its Dispatch chain.
	fmt.Fprintf(stdout, "admission: %d issues hold a slot, cap %d, %d waiting; %d roots in work; %d issues\n",
		len(state.Admission.Active), state.Admission.Cap, len(state.Admission.Waiting),
		len(state.Admission.Trees), len(state.Issues))
	// The project's pause: a paused line while it stands, a last-pause line while the project runs
	// after one has ended, and neither before any pause, as the state carries one key at most.
	for _, line := range []string{pausedSummary(state.Paused), lastPauseSummary(state.LastPause)} {
		if line != "" {
			fmt.Fprintln(stdout, line)
		}
	}
	for _, line := range waitingLineLines(state.Admission.Waiting, state.Issues) {
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintln(stdout, blockedSummary(state.Admission.Blocked))
	fmt.Fprintln(stdout, heldAdmissionSummary(state.Admission.Held))
	for _, line := range experimentLines(state.Settings, state.Daemon.Project, state.Issues) {
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintln(stdout, awaitingSummary(state.Issues))
	fmt.Fprintln(stdout, checksSummary(openChecks(state.Issues)))
	fmt.Fprintln(stdout, mergeWaitsSummary(mergeWaits(state.Issues)))
	fmt.Fprintln(stdout, roundsSummary(state.Issues))
	fmt.Fprintln(stdout, openCallsSummary(state.Issues))
	fmt.Fprintln(stdout, entrySummary(state.Issues))
	fmt.Fprintln(stdout, pendingSummary(state.PendingStatusWrites))
	fmt.Fprintln(stdout, heldSummary(state.HeldOutboxRows))
	fmt.Fprintln(stdout, strandedSummary(state.StrandedClaims, strandedSuspenders(ctx, op, state.StrandedClaims)))
	if wakes := wakesWaitingSummary(state.WakesWaiting); wakes != "" {
		fmt.Fprintln(stdout, wakes)
	}
	for _, line := range repositoryLines(state.Repositories.Live) {
		fmt.Fprintln(stdout, line)
	}
	for _, line := range webhookAppLines(state.Repositories.WithoutWebhookApp) {
		fmt.Fprintln(stdout, line)
	}
	for _, line := range consumerLines(state.Consumers, state.Daemon.StartedAt, time.Now()) {
		fmt.Fprintln(stdout, line)
	}
	for _, line := range intakeGiveUpLines(state.IntakeGiveUps) {
		fmt.Fprintln(stdout, line)
	}
	// In a pane, the issue record is what a relaunched worker re-reads.
	if key := os.Getenv("LEGION_ISSUE"); key != "" {
		issue, ok := state.Issues[key]
		if !ok {
			fmt.Fprintf(stdout, "issue %s is not recorded\n", key)
			return 0
		}
		record, err := json.MarshalIndent(issue, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "legion state: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "issue %s:\n%s\n", key, record)
	}
	return 0
}

// blockedSummary is the one line an operator reads for the issues Legion waits on a Dispatch
// issue nobody has finished for (api.Admission.Blocked), the ones it has still to start and the
// ones it started that gave their slot back for the wait: how many, and each by
// issue with what it waits on, in the state's own key order. Nothing here is the daemon's to
// clear — a blocker is finished in Dispatch by a person — so the line names them and stops.
func blockedSummary(blocked []api.BlockedIssue) string {
	if len(blocked) == 0 {
		return "blocked on Dispatch links: none"
	}
	waits := make([]string, len(blocked))
	for i, issue := range blocked {
		waits[i] = issue.Issue + " waits on " + strings.Join(issue.BlockedBy, ", ")
	}
	return fmt.Sprintf("blocked on Dispatch links: %d — %s", len(blocked), strings.Join(waits, "; "))
}

// heldAdmissionSummary is the one line an operator reads for the issues the daemon's own record
// holds in the workflow that stand in neither the active set nor the waiting line
// (api.Admission.Held): how many, and how many of each word for why, the words this build knows
// in the ladder's own order (record.HeldReasons) and then each word it does not know in sorted
// order, so two readings of one state print one line and the counts add up to the total whatever
// words a later daemon writes. A word no row carries is left out.
//
// The prefix names the admission list because `legion state` prints a second held line below
// this one, the lifecycle rows the daemon holds behind their issues' running ones
// (heldSummary): these are issues nothing starts, those are outbox rows nothing has sent, and a
// reader counting one must not read the other's number. It names no issue: a deployment holding
// hundreds would print a line per issue, and each row with its word is in `legion state --json`.
func heldAdmissionSummary(held []api.HeldIssue) string {
	if len(held) == 0 {
		return "admission.held: none"
	}
	counts := map[record.HeldReason]int{}
	for _, issue := range held {
		counts[issue.Reason]++
	}
	words := make([]string, 0, len(counts))
	for _, reason := range record.HeldReasons {
		if count := counts[reason]; count > 0 {
			words = append(words, fmt.Sprintf("%s %d", reason, count))
			delete(counts, reason)
		}
	}
	for _, reason := range slices.Sorted(maps.Keys(counts)) {
		words = append(words, fmt.Sprintf("%s %d", reason, counts[reason]))
	}
	noun := "issues"
	if len(held) == 1 {
		noun = "issue"
	}
	return fmt.Sprintf("admission.held: %d %s — %s", len(held), noun, strings.Join(words, ", "))
}

// waitingLineLines are the lines an operator reads for the issues waiting for an admission slot
// (api.State.Admission.Waiting), under the counts the `admission:` line gives: a header with how
// many wait, then one line per issue — its key, its place in the line, the part of the line it
// waits in and the daemon's own rendered sentence for why it waits there
// (api.WaitingLineView.Reason) — in the order `waiting` publishes them, which is the order
// admission promotes them in.
//
// It names waitingLineNamedCap issues and then how many more there are, the shape every capped
// list of these commands prints (claims.go's issuesOrNone): the line on a live deployment runs to
// hundreds, and hundreds of lines are no summary. The whole of it is in `legion state --json`,
// which writes the served body as it stands and carries every member with its own reason.
//
// A daemon that served this caller no reason at all is told from a line with nothing in it by the
// two facts the body carries: `waiting` names issues and not one of them carries a reason. That
// is the answer for a daemon older than the contract that added the sentence and for one older
// than the contract that added `waitingLine` itself, which sends no such key at all, and the
// reader is told which of the two it is reading rather than shown an empty line that is not true.
func waitingLineLines(waiting []string, issues map[string]api.Issue) []string {
	if len(waiting) == 0 {
		return []string{"waiting line: none"}
	}
	rendered := false
	for _, key := range waiting {
		if view := issues[key].WaitingLine; view != nil && view.Reason != "" {
			rendered = true
			break
		}
	}
	if !rendered {
		return []string{"waiting line: the daemon sent no reasons at this caller's contract"}
	}
	named := waiting
	if len(named) > waitingLineNamedCap {
		named = named[:waitingLineNamedCap]
	}
	lines := make([]string, 0, len(named)+2)
	lines = append(lines, fmt.Sprintf("waiting line: %d", len(waiting)))
	for _, key := range named {
		// A body naming an issue in `waiting` whose issue carries no `waitingLine` is no body
		// this daemon sends — the strip that empties the field empties it for every member, which
		// the line above answers — so this line says what it was given rather than printing a
		// place of 0 that no reader could tell from a true one.
		view := issues[key].WaitingLine
		if view == nil {
			lines = append(lines, "  "+key+": the daemon sent no place for this issue")
			continue
		}
		// Both numbers of the printed place come from the member's own view, which is where the
		// daemon put them and what its sentence was rendered against; the count in the header
		// and the one after the named issues are about the list this caller holds.
		lines = append(lines, fmt.Sprintf("  %s %d of %d, %s: %s", key, view.Place, view.Length, view.Part, view.Reason))
	}
	if len(waiting) > waitingLineNamedCap {
		lines = append(lines, fmt.Sprintf("  and %d more", len(waiting)-waitingLineNamedCap))
	}
	return lines
}

// waitingLineNamedCap is how many waiting issues the block above names, the count after them
// standing for the rest. Ten, because the block is a summary under a summary: a deployment whose
// cap holds a dozen trees queues hundreds behind them, and 392 lines of sentence would scroll
// every other line of `legion state` away. Every member with its own sentence is in
// `legion state --json`.
const waitingLineNamedCap = 10

// experimentLines are the lines an operator reads for the experiments the deployment declares
// (the daemon's own project): one per experiment, in the order the daemon's loader declares
// them — the `memory` block's experiment first, then the `experiments` key's own in the file's
// order (api.ProjectSettings.DeclaredExperiments) — counting how many issues were drawn each arm
// of it. An experiment no issue was drawn an arm of is `none` after its name, as a deployment
// that has drawn nothing has always read.
//
// It counts only the rows that record a draw, which are the rows that carry an instant
// (api.AssignmentView's DrawnAt), and leaves out a row a hand-over settled at no arm — no
// variant, no mode and no instant — for every experiment alike, as internal/projection's
// memoryOf leaves that row out of an issue's three memory fields. So the `memory` experiment's
// line counts the issues the memory line counted before any experiment was declared, the
// issues whose `memoryGroup` is set.
//
// The list is built from the settings and not from `settings.projects.<KEY>.experiments` alone,
// because the two keys a project declares an experiment with are carried apart there: a
// deployment that writes the `memory` block and no `experiments` key — which is what this fleet
// runs — declares one experiment and carries an empty `experiments`, and reading that key alone
// would print `experiments: none` while every issue holds a memory assignment.
//
// It counts issues, and not the trees a tree experiment is drawn for, because the state shows
// every draw on the issues it applies to. Each issue's `assignments` holds its own rows, of either
// unit (internal/projection's assignmentViews): a tree-unit row is the one it took at its first
// start from the nearest issue above it in Legion's work that held one, or drew itself
// (internal/admit's takeTreeDraw). So one count, of issues, serves an experiment of either unit.
// A count of trees, read off api.Issue.Tree, would fit a tree experiment alone, and even there it
// would not count draws: an issue that starts under another takes the nearest one's tree-unit rows
// verbatim — the arm, the mode and the instant alike (record.Postgres.CarryAssignments) — so two
// issues hold one draw. It names no issue either: the
// per-issue answers are in `legion state --json`, where a line per issue would print hundreds on
// a live deployment.
//
// Every project declares at least the memory experiment, so there is always a line: a project
// whose settings carry no `memory` block and no experiment of its own declares that one at mode
// off (config.Project.ExperimentsDeclared), and its line counts the arms its issues hold —
// `memory: none` for a deployment that has drawn nothing yet, which is the line such a
// deployment read before any experiment was declared.
//
// A daemon older than contract 141 serves no `assignments` on any issue, and that is the daemon an
// operator who rolls the daemon back and keeps this `legion` reads. So an issue that carries no
// `assignments` is counted from its memory fields instead (api.Issue.MemoryGroup and
// MemoryDrawMode, which every daemon from contract 49 serves), under the `memory` experiment,
// wherever its `memoryGroup` is set, as the memory line counted them before any experiment was
// declared: its group, `memory` or `none`, is the arm and its mode the mode. Such a daemon's
// settings carry the `memory` block and no `experiments` key, and a daemon that serves no
// settings carries neither, so the list above declares the `memory` experiment alone and the
// line is the one a `legion` without this change prints for the same body. A daemon of this
// release serves the memory fields only where an issue's `memory` row records a draw, and serves
// that row in `assignments` as well to every caller but one below contract 141 (api's shapeIssue),
// so in the body it serves `legion state` an issue with no `assignments` carries no `memoryGroup`
// either and the fallback counts nothing; in the body it serves a caller below 141 it counts the
// `memory` draws.
func experimentLines(settings api.Settings, project string, issues map[string]api.Issue) []string {
	declared := settings.Projects[project].DeclaredExperiments(project)
	counts := make(map[string]map[armDraw]int, len(declared))
	count := func(experiment string, draw armDraw) {
		held, counted := counts[experiment]
		if !counted {
			held = map[armDraw]int{}
			counts[experiment] = held
		}
		held[draw]++
	}
	for _, issue := range issues {
		if len(issue.Assignments) == 0 {
			if issue.MemoryGroup != "" {
				count(config.MemoryExperimentName, armDraw{variant: issue.MemoryGroup, mode: issue.MemoryDrawMode})
			}
			continue
		}
		for _, assignment := range issue.Assignments {
			if assignment.DrawnAt == nil {
				continue
			}
			count(assignment.Experiment, armDraw{variant: assignment.Variant, mode: assignment.Mode})
		}
	}
	lines := make([]string, len(declared))
	for i, experiment := range declared {
		lines[i] = experimentLine(experiment.Name, counts[experiment.Name])
	}
	return lines
}

// armDraw is what one experiment line counts issues by: the arm an issue was drawn and the mode
// it was drawn under (api.AssignmentView's Variant and Mode).
type armDraw struct{ variant, mode string }

// label is how the line names the arm: its own name, or `none` for the empty arm, which is the
// arm an experiment at mode off draws.
func (d armDraw) label() string {
	if d.variant == "" {
		return "none"
	}
	return d.variant
}

// experimentLine is one experiment's line: how many issues were drawn an arm of it, and how many
// of those under each arm and mode, `N <arm> (<mode>)`, in the order of the arm's printed name
// and then of the mode, so two runs over one state print one line. The `memory` experiment's two
// arms are `memory` and `none` (config.MemoryVariantNames), the two memory groups, so its line is
// word for word the one `legion state` printed for the memory groups before any experiment was
// declared.
func experimentLine(name string, counts map[armDraw]int) string {
	held := 0
	for _, count := range counts {
		held += count
	}
	if held == 0 {
		return name + ": none"
	}
	draws := slices.SortedFunc(maps.Keys(counts), func(a, b armDraw) int {
		return cmp.Or(cmp.Compare(a.label(), b.label()), cmp.Compare(a.mode, b.mode))
	})
	named := make([]string, len(draws))
	for i, draw := range draws {
		named[i] = fmt.Sprintf("%d %s (%s)", counts[draw], draw.label(), draw.mode)
	}
	noun := "issues"
	if held == 1 {
		noun = "issue"
	}
	return fmt.Sprintf("%s: %d %s — %s", name, held, noun, strings.Join(named, ", "))
}

// awaitingSummary is the one line an operator reads for the roots that wait on a person
// (api.WaitingOnView): such a root keeps the slot it took when the daemon started it, so it
// stands in `active` while it waits and `active` alone does not say what it waits for. It says
// how many, how many of them a person answered and the daemon's own published
// line names (api.WaitingLineView), and each by key with what it waits on and whether, when and by
// what act it was answered.
//
// The second count reads the published line rather than judging the wait a second time. An
// answered root stands in that line, within its priority, among the issues whose work came back, oldest first
// (record.Waiting carries every answered root there, whatever its design gate
// says), until the pass that resumes it. Two answered roots are in no line this summary reads:
// one of another Dispatch project, whose line this daemon does not build, and every one of them
// when the daemon answering is older than contract 58 and has no `waitingLine` to send. The
// caller is the operator's `legion state`, which a daemon of this release serves in full
// (api's stateFor), so nothing strips the field from this reader. So the count is 0 against a
// daemon older than contract 58, which is the true answer for a reader that cannot see the line.
func awaitingSummary(issues map[string]api.Issue) string {
	var keys []string
	standing := 0
	for key, issue := range issues {
		if issue.WaitingOn == nil {
			continue
		}
		keys = append(keys, key)
		if issue.WaitingOn.AnsweredAt != nil && issue.WaitingLine != nil {
			standing++
		}
	}
	if len(keys) == 0 {
		return "roots waiting on a person: none"
	}
	slices.Sort(keys)
	waits := make([]string, len(keys))
	for i, key := range keys {
		wait := issues[key].WaitingOn
		on := "ask " + wait.Ask
		if wait.Kind == "gate" {
			on = fmt.Sprintf("spec version %d of %s", wait.GateVersion, wait.ArtifactID)
		}
		state := "unanswered"
		if wait.AnsweredAt != nil {
			state = fmt.Sprintf("answered at %s (%s)", wait.AnsweredAt.UTC().Format(time.RFC3339), wait.AnsweredBy)
		}
		waits[i] = key + " waits on " + on + ", " + state
	}
	return fmt.Sprintf("roots waiting on a person: %d, %d answered and standing in the admission line — %s",
		len(keys), standing, strings.Join(waits, "; "))
}

// pendingSummary is the one line an operator reads for the Dispatch status writes the daemon has
// not finished: how many wait, and why the oldest — the one its issue's later writes wait behind —
// has not landed.
func pendingSummary(pending []api.PendingStatusWrite) string {
	if len(pending) == 0 {
		return "pending Dispatch status writes: none"
	}
	oldest := pending[0]
	next := oldest.NextAt.UTC().Format(time.RFC3339)
	if oldest.Attempts == 0 {
		return fmt.Sprintf("pending Dispatch status writes: %d; the oldest, for %s, has not run yet, next at %s", len(pending), oldest.Issue, next)
	}
	return fmt.Sprintf("pending Dispatch status writes: %d; the oldest, for %s, has failed %d attempts, next at %s: %s",
		len(pending), oldest.Issue, oldest.Attempts, next, oldest.LastError)
}

// issueCheck is one open production check (api.CheckView) with the issue it belongs to.
type issueCheck struct {
	issue string
	check api.CheckView
}

// openChecks is every open production check the state's issues carry, oldest opened first and then
// by issue key, so the oldest is the one named whatever order the map came in.
func openChecks(issues map[string]api.Issue) []issueCheck {
	var checks []issueCheck
	for key, issue := range issues {
		if issue.Check != nil {
			checks = append(checks, issueCheck{issue: key, check: *issue.Check})
		}
	}
	slices.SortFunc(checks, func(a, b issueCheck) int {
		return cmp.Or(a.check.OpenedAt.Compare(b.check.OpenedAt), cmp.Compare(a.issue, b.issue))
	})
	return checks
}

// checksSummary is the one line an operator reads for the production checks still open: how many,
// and the oldest — its issue, pull request and merge commit, the role working it, which look it
// is on, what woke that look and when, and when its next look is due. The role is on the line
// because a check opened before the release that gave the project `production_check` carries the
// implementer, and one opened after it the role that setting names, and an operator reading the
// oldest check cannot tell them apart otherwise.
func checksSummary(checks []issueCheck) string {
	if len(checks) == 0 {
		return "open production checks: none"
	}
	oldest := checks[0]
	merge := oldest.check.MergeCommit
	if len(merge) > 7 {
		merge = merge[:7]
	}
	if merge == "" {
		merge = "an unrecorded merge"
	}
	look := fmt.Sprintf("look %d", oldest.check.Looks)
	if wake := oldest.check.LastWake; wake != nil {
		look += fmt.Sprintf(" woken by %s at %s", wake.Kind, wake.At.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("open production checks: %d; the oldest, %s #%d at %s, worked by the %s, %s, next look %s",
		len(checks), oldest.issue, oldest.check.PullRequest, merge, oldest.check.Role, look, oldest.check.NextWakeAt.UTC().Format(time.RFC3339))
}

// issueMergeWait is one review round's wait on GitHub (api.MergeWaitView) with the issue it
// belongs to and that issue's pull request, which is where the number the line names comes from.
type issueMergeWait struct {
	issue string
	wait  api.MergeWaitView
	pull  *api.PullRequestView
}

// mergeWaits is every review round of the state's issues that waits on GitHub's merge state for
// the head its reviewer's approval stands on (api.MergeWaitView), oldest head first and then by
// issue key, so the oldest is the one named whatever order the map came in. Oldest is the head
// that has stood longest, the smallest `headUpdatedAt`, because the wait an operator must act on
// is the one a head has sat through, not the one the daemon last read.
func mergeWaits(issues map[string]api.Issue) []issueMergeWait {
	var waits []issueMergeWait
	for key, issue := range issues {
		if issue.MergeWait != nil {
			waits = append(waits, issueMergeWait{issue: key, wait: *issue.MergeWait, pull: issue.PullRequest})
		}
	}
	slices.SortFunc(waits, func(a, b issueMergeWait) int {
		return cmp.Or(olderHead(a.wait.HeadUpdatedAt, b.wait.HeadUpdatedAt), cmp.Compare(a.issue, b.issue))
	})
	return waits
}

// olderHead orders two rounds by the moment their head was last updated, oldest first, and sorts
// a round whose record holds no such moment last however long it may have been there: an unknown
// time is no evidence of age, and a round the daemon knows no time for would otherwise be named
// the oldest ahead of every round it does know one for. Two rounds with no time are equal here,
// and the issue key decides between them.
func olderHead(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return a.Compare(*b)
}

// mergeWaitsSummary is the one line an operator reads for the review rounds waiting on GitHub's
// merge state for their head: how many, and the oldest head — its issue, pull request and head,
// and what the record holds of the daemon's last read of that head. The line says "the oldest
// head" rather than "the oldest" because the order is the head's (`headUpdatedAt`, mergeWaits
// above) and the only moment the line prints is the read's: a reader told "the oldest" takes the
// time beside it for the reason the round was chosen, and the round named is routinely the one
// read most recently. It is printed beside the open production checks because both are work that
// has left the agents and stands on something outside Legion.
//
// What follows the head is one reading, chosen by the one switch below over whether the wait
// carries a merge state and whether it carries the moment of the read, and that switch is the
// whole model: there are four readings and no other, and each says what the record holds rather
// than what did or did not happen at GitHub, so only a reading with a state on record says
// `GitHub said`. `GitHub said <state>, read <time>` is a wait holding a merge state for this head
// and the moment of the read that gave it, in UTC. `GitHub said <state>, read time not recorded`
// is a wait holding a merge state and no moment of the read: the time is what the record lacks,
// so the line never says no read was made. `read <time> with no merge state` is a wait holding the
// moment of a read and no merge state, so the line names the read rather than claiming no answer
// came. `no read recorded for this head` is a wait holding neither: no merge state and no read
// time for the head the wait names, and `record.PullRequest.StateReadAt`'s doc says when that is.
//
// The pull request number is the issue's own view of it. An issue that carries a merge wait and
// no pull request view cannot happen — the daemon builds the wait from the pull request record,
// so there is a record to build the view from as well — but this prints a daemon's answer, which
// an older or a newer contract may shape otherwise, and a line that panicked on it would take
// the whole command down over one issue: such a round is named `#unrecorded` and is read like any
// other.
func mergeWaitsSummary(waits []issueMergeWait) string {
	if len(waits) == 0 {
		return "merge waits: none"
	}
	oldest := waits[0]
	head := oldest.wait.WaitsOn
	if len(head) > 7 {
		head = head[:7]
	}
	number := "unrecorded"
	if oldest.pull != nil {
		number = strconv.Itoa(oldest.pull.Number)
	}
	var reading string
	state, at := oldest.wait.MergeState, oldest.wait.StateReadAt
	switch {
	case state != "" && at != nil:
		reading = "GitHub said " + state + ", read " + at.UTC().Format(time.RFC3339)
	case state != "":
		reading = "GitHub said " + state + ", read time not recorded"
	case at != nil:
		reading = "read " + at.UTC().Format(time.RFC3339) + " with no merge state"
	default:
		reading = "no read recorded for this head"
	}
	return fmt.Sprintf("merge waits: %d; the oldest head, %s #%s, head %s, %s",
		len(waits), oldest.issue, number, head, reading)
}

// issueRound is one update round an issue has open, and the claims in it, by role: the round a
// pod's drift opened over every role of the issue (`pod:<pod uid>`), which is the one round this
// daemon opens. One role's own plugin round (`plugin:<daemon contract>`) is an earlier daemon's,
// and a boot takes every claim out of one it finds (internal/daemon's endPluginRounds), so this
// reads one only in the moments before that pass.
type issueRound struct {
	issue string
	round string
	roles []string
}

// openRounds is every update round the state's issues have open, by issue and then round, each
// with its claims in role order: a round is the daemon holding a process back from replacement
// until its worker has pushed its work and answered.
func openRounds(issues map[string]api.Issue) []issueRound {
	rounds := map[string]*issueRound{}
	var keys []string
	add := func(issue, role string, view api.ClaimView) {
		if view.UpdateRound == "" {
			return
		}
		key := issue + " " + view.UpdateRound
		if _, open := rounds[key]; !open {
			rounds[key] = &issueRound{issue: issue, round: view.UpdateRound}
			keys = append(keys, key)
		}
		named := role
		if view.UpdateHead != "" {
			// The head is the worker's own word, which nothing verifies, so it is quoted: a
			// newline in it would otherwise forge a row of this summary (claims.go's
			// quotedOrNone).
			named += " answered at " + strconv.Quote(view.UpdateHead)
		}
		rounds[key].roles = append(rounds[key].roles, named)
	}
	for key, issue := range issues {
		if issue.Architect != nil {
			add(key, string(legionclaim.RoleArchitect), *issue.Architect)
		}
		for _, role := range slices.Sorted(maps.Keys(issue.Workers)) {
			add(key, string(role), issue.Workers[role].Claim)
		}
	}
	slices.Sort(keys)
	open := make([]issueRound, len(keys))
	for i, key := range keys {
		open[i] = *rounds[key]
	}
	return open
}

// roundsSummary is the one line an operator reads for the update rounds still open: how many, and
// each by issue, round and the roles in it, with the head a role reported when it answered. While
// a round is open the daemon holds those roles' next tasks and replaces nothing, so a round that
// does not end is work that is not moving.
func roundsSummary(issues map[string]api.Issue) string {
	rounds := openRounds(issues)
	if len(rounds) == 0 {
		return "open update rounds: none"
	}
	named := make([]string, len(rounds))
	for i, round := range rounds {
		named[i] = fmt.Sprintf("%s on %s: %s", round.issue, round.round, strings.Join(round.roles, ", "))
	}
	return fmt.Sprintf("open update rounds: %d — %s", len(rounds), strings.Join(named, "; "))
}

// claimCall is one claim's open tool call: the issue and role whose claim it is, and the record
// of the call (api.StepView).
type claimCall struct {
	issue string
	role  string
	step  api.StepView
}

// openCalls is every working claim of the state whose agent is in a tool call the daemon has seen
// the start of and no end of, by issue and then role: a working claim whose openCallWord is
// non-empty. openCallsSummary names each of these rows with that word. That is the shape a stalled
// turn leaves behind: the daemon reads the same record to decide a turn has stopped moving, and
// this is where an operator reads it. Only a claim in state `working` has a turn in flight
// (supervise.StateWorking): a retired or suspended claim keeps the step it was last in on its
// record, on purpose, so a call open there is no agent in a call. A controller's claim is in none
// of these — internal/projection puts it on no issue — and is read with `legion claims list`
// instead.
func openCalls(issues map[string]api.Issue) []claimCall {
	var calls []claimCall
	open := func(issue, role string, view api.ClaimView) {
		if view.State != string(supervise.StateWorking) || openCallWord(view.Step) == "" {
			return
		}
		calls = append(calls, claimCall{issue: issue, role: role, step: *view.Step})
	}
	for key, issue := range issues {
		if issue.Architect != nil {
			open(key, string(legionclaim.RoleArchitect), *issue.Architect)
		}
		for _, role := range slices.Sorted(maps.Keys(issue.Workers)) {
			open(key, string(role), issue.Workers[role].Claim)
		}
	}
	slices.SortFunc(calls, func(a, b claimCall) int {
		return cmp.Or(cmp.Compare(a.issue, b.issue), cmp.Compare(a.role, b.role))
	})
	return calls
}

// openCallsSummary is the one line an operator reads for the agents in a tool call right now: how
// many, and each by issue and role with the call its stream named and when that call started
// (claims.go's openCallWord, Go-quoted because the tool and the call id are the agent's own
// word). A call that has been open for hours is this issue's whole shape — the daemon's backstop
// ends such a turn, and until it does this line says which agent is in one.
//
// The call's arguments are in no column of it: the daemon keeps their length and a fingerprint
// alone, because a shell command routinely holds a credential (supervise.StepRecord).
func openCallsSummary(issues map[string]api.Issue) string {
	calls := openCalls(issues)
	if len(calls) == 0 {
		return "agents in a tool call: none"
	}
	named := make([]string, len(calls))
	for i, call := range calls {
		named[i] = fmt.Sprintf("%s %s %s", call.issue, call.role, strconv.Quote(openCallWord(&call.step)))
	}
	return fmt.Sprintf("agents in a tool call: %d — %s", len(calls), strings.Join(named, "; "))
}

// heldSummary is the one line an operator reads for the lifecycle rows the daemon holds behind
// their issues' running ones (api.State.HeldOutboxRows): how many, and the one that has waited
// longest — the first such in the state's order, so ties go to the one due first.
func heldSummary(held []api.HeldOutboxRow) string {
	if len(held) == 0 {
		return "held lifecycle rows: none"
	}
	longest := slices.MaxFunc(held, func(a, b api.HeldOutboxRow) int { return cmp.Compare(a.WaitedSeconds, b.WaitedSeconds) })
	return fmt.Sprintf("held lifecycle rows: %d; the longest waiting, %s row %d of %s, has waited %s behind row %d",
		len(held), longest.Kind, longest.ID, longest.Issue, time.Duration(longest.WaitedSeconds)*time.Second, longest.WaitsOn)
}

// strandedPhrases is how `legion state` names each move that brings a stranded claim back
// (api.StrandedClaim.Move) before the claims it brings back.
var strandedPhrases = map[api.StrandedMove]string{
	api.StrandedResume:           "resume",
	api.StrandedSuspendedWorkers: "suspended workers, each brought back as its suspendedBy calls for (legion claims list --json)",
	api.StrandedReleasedNoResume: "released, woken over ten minutes ago and the pod never came back",
	api.StrandedRetry:            "failed, retry",
	api.StrandedWorkflowStart:    "failed or retired, needs a workflow start: the architect's retry_or_escalate of a held phase, a transition into the phase, or a tree close and re-admission",
}

// strandedSummary is the one line an operator reads for the claims nothing will wake: how many,
// and each by issue and role under the move that brings it back (api.StrandedClaim.Move, which the
// boot's log line groups by too). A suspended phase worker's move is the troubleshooting page's "A
// tree does not move after a restart" for its suspendedBy, which `legion claims list --json` shows;
// a released one is a pod whose wake was taken and whose replacement never took a turn, which is
// the daemon's own log of that wake to read, not a suspension to lift.
func strandedSummary(stranded []api.StrandedClaim, suspended api.Suspenders) string {
	if len(stranded) == 0 {
		return "claims nothing will wake: none"
	}
	grouped := api.StrandedByMove(stranded, suspended)
	var groups []string
	for _, move := range api.StrandedMoves() {
		if named := grouped[move]; len(named) > 0 {
			groups = append(groups, strandedPhrases[move]+" "+strings.Join(named, ", "))
		}
	}
	return fmt.Sprintf("claims nothing will wake: %d — %s", len(stranded), strings.Join(groups, "; "))
}

// entryBehind is one issue the plain state names for its phase entry: the issue, the role it is
// named for, the entry the phases row of the role of its current phase stands on and the entry
// that role was given (phase.RoleFor, the role the state's two numbers are of), and, for a root's
// architect, the arms the state names for it.
type entryBehind struct {
	issue string
	role  legionclaim.Role
	// numbered is whether entry and given are role's own, which they are for every row but the
	// architect of a root past the phases its architect works, where they are that phase's
	// worker's and the line leaves them off the architect.
	numbered     bool
	entry, given int64
	// arms is every arm of the daemon's architect-stalled reading the state names for this root
	// (api.Issue.ArchitectStalls), and empty for the worker of an issue that is not its tree's
	// root, whose two numbers differ, which the difference names instead.
	arms []record.ArchitectStallArm
}

// entriesBehind is every issue of the state the plain line names, by issue key: a tree's own root
// the state names an arm of the architect-stalled reading for (api.Issue.ArchitectStalls), as its
// architect, and an issue that is not its tree's root whose phase worker has not been given the
// work its phase entry names (api.Issue.PhaseEntry and GivenEntry). The daemon reads the arms by
// the predicates its own wake uses and serves them whether or not the controller was told, so
// this reads no arm of its own.
//
// A root is named for the arms alone, whatever role works the phase it stands in
// (dispatch://INFRA-2614, R6): every root reads as behind for the moments between a transition
// and its delivery, so the difference alone would name a healthy root at every read. A root is an
// issue whose tree is its own key (api.Issue.Tree, claim.IsTreeRoot), and the numbers it carries
// past its architect's phases are its phase worker's, which the line names for no root. The
// numbers are named for the role a phase worker works on any other issue, which is
// dispatch://INFRA-2800's line; a role of the architect's, a child at admitted among them, which
// phase.RoleFor names the architect's phase, is named for nothing. An issue whose current phase
// names no role at all carries neither number and is never named for them: the projection writes
// them only where phase.RoleFor names a role, and that is the role this reads back for the line.
func entriesBehind(issues map[string]api.Issue) []entryBehind {
	var behind []entryBehind
	for _, key := range slices.Sorted(maps.Keys(issues)) {
		issue := issues[key]
		role := phase.RoleFor(issue.Phase)
		if legionclaim.IsTreeRoot(key, issue.Tree) {
			if len(issue.ArchitectStalls) == 0 {
				continue
			}
			row := entryBehind{issue: key, role: legionclaim.RoleArchitect, arms: issue.ArchitectStalls}
			if role == legionclaim.RoleArchitect {
				row.numbered, row.entry, row.given = true, issue.PhaseEntry, issue.GivenEntry
			}
			behind = append(behind, row)
			continue
		}
		if role == "" || role == legionclaim.RoleArchitect || issue.PhaseEntry == issue.GivenEntry {
			continue
		}
		behind = append(behind, entryBehind{issue: key, role: role, numbered: true, entry: issue.PhaseEntry, given: issue.GivenEntry})
	}
	return behind
}

// entrySummary is the one line an operator reads for the roles that have not been given the work
// their phase entry names and for the roots a wake names: how many, and each by issue and role
// with the two numbers where they are that role's, a root with the words of each arm the state
// names for it (record.ArchitectStallArm.Words, the words the wake's reason begins with). For a
// role a phase worker works on an issue that is not its tree's root, a difference here is one of
// four things. An entry raised seconds ago, which the next pass closes: the daemon sends that
// entry's work on every admission pass and on a sweep of its own each minute (admit's
// resumeSlottedWorkers). An issue waiting for its admission slot, whose start row the outbox holds
// unclaimable until the slot comes (workflow.StartOrOwe) and which the pass passes over, since it
// reads the slotted issues alone — every such issue in the waiting line is named here, and its
// difference stands for as long as it waits, which is hours on a full cap. A claim whose worker is
// still working an earlier entry's task, which the pass passes over while that delivery is
// worked: a start the slot held back reaches its worker mid-turn, so the difference reads for the
// rest of that turn and closes on the first pass after the task is retired. Or a role the pass
// skips and sends nothing to at all (admit's owedEntryRoles, which states each skip), whose
// difference stands until something outside that pass moves it.
//
// The daemon's own "a role was behind the entry its phases row names" log line names the roles
// that pass sends the work to and no others, so a difference of that last kind on such an issue is
// read here and nowhere else. A root is named for the arms the state serves instead
// (entriesBehind), whatever its numbers say.
func entrySummary(issues map[string]api.Issue) string {
	behind := entriesBehind(issues)
	if len(behind) == 0 {
		return "roles not given their phase entry, and roots a wake names: none"
	}
	named := make([]string, len(behind))
	for i, row := range behind {
		parts := make([]string, 0, 2+len(row.arms))
		if row.numbered {
			parts = append(parts, fmt.Sprintf("entry %d", row.entry), fmt.Sprintf("given %d", row.given))
		}
		for _, arm := range row.arms {
			parts = append(parts, arm.Words())
		}
		named[i] = fmt.Sprintf("%s %s: %s", row.issue, row.role, strings.Join(parts, ", "))
	}
	return fmt.Sprintf("roles not given their phase entry, and roots a wake names: %d — %s",
		len(behind), strings.Join(named, "; "))
}

// wakesWaitingSummary is the line an operator reads for the wakes the daemon's pacing is holding
// until their turn (api.State.WakesWaiting): how many, and each by claim with what kind of wake it
// is and when it is due, in the state's own claim order. It is empty when nothing waits, and the
// caller then prints nothing: every other line here says "none" because what it counts is always
// worth a glance, while a wake queue is empty at every moment outside a restart or a plugin wave,
// and a line saying so on every read would be noise in the one place an operator reads most.
func wakesWaitingSummary(wakes []api.WakeWaiting) string {
	if len(wakes) == 0 {
		return ""
	}
	held := make([]string, len(wakes))
	for i, wake := range wakes {
		held[i] = fmt.Sprintf("%s %s due %s", wake.Claim, wake.Kind, wake.DueAt.Format(time.RFC3339))
	}
	return fmt.Sprintf("wakes waiting: %d — %s", len(wakes), strings.Join(held, "; "))
}

// strandedSuspenders is who suspended each stranded claim, which the state body does not carry:
// every plugin parses that body with the schema of its own contract, so a key added to it refuses
// the whole state in a session whose plugin predates INFRA-1048 and that the daemon has not
// relaunched, and is dropped by every plugin after it (api.StrandedClaim). The
// operator claim routes serve it, and this reads them with the bearer --operator-token-file
// names, on the daemon op already names. Without that flag, and when the daemon refuses the
// bearer or answers nothing, the lookup is empty and every stranded claim reads as the plain
// suspension it is, which `legion claims list --json` tells apart. The state is the operator's
// one unguarded read, so nothing here fails the command.
func strandedSuspenders(ctx context.Context, op operator, stranded []api.StrandedClaim) api.Suspenders {
	if len(stranded) == 0 || op.bearer == "" {
		return nil
	}
	timed, cancel := context.WithTimeout(ctx, programbound.Of(&requestTimeout))
	defer cancel()
	status, answer, err := op.do(timed, http.MethodGet, claimsRoute, nil)
	if err != nil || status/100 != 2 {
		return nil
	}
	var list api.OperatorClaims
	if json.Unmarshal(answer, &list) != nil {
		return nil
	}
	return api.SuspendersOfOperatorClaims(list.Claims)
}

// repositoryLines are the lines an operator reads for the repositories the live records name
// (api.RepositoriesView.Live) that GitHub's last read did not name or a role App is not installed
// on: one line per such repository, in the state's order, with why — its readError, the Apps
// GitHub answered are not installed on it (api.LiveRepositoryView.NotInstalled), or both — and
// the name it is served under where that is the other name of its configured pair. A repository
// with neither, one GitHub has not yet answered among them, has no line.
func repositoryLines(live []api.LiveRepositoryView) []string {
	var lines []string
	for _, repo := range live {
		var why []string
		for _, reason := range []string{repo.ReadError, repo.NotInstalled()} {
			if reason != "" {
				why = append(why, reason)
			}
		}
		if len(why) == 0 {
			continue
		}
		name := repo.Repo
		if repo.ServedAs != "" {
			name += " (served as " + repo.ServedAs + ")"
		}
		lines = append(lines, "repository "+name+": "+strings.Join(why, "; "))
	}
	return lines
}

// webhookAppLines are the lines an operator reads for the repositories the live records name that
// Dispatch's GitHub App is not known to be installed on (api.RepositoriesView.WithoutWebhookApp,
// which a daemon of contract 152 writes too): GitHub sends Legion no event of such a repository.
// One line per repository with why, in the state's order. Only a list that is present and empty
// gives the one line that says none is missing; a daemon that wrote no list (gaps nil) says
// nothing of the webhook App, so the CLI prints no line for it.
func webhookAppLines(gaps []api.WebhookAppGapView) []string {
	if gaps == nil {
		return nil
	}
	if len(gaps) == 0 {
		return []string{"webhook App missing: none"}
	}
	lines := make([]string, len(gaps))
	for i, gap := range gaps {
		lines[i] = "webhook App missing on " + gap.Repo + ": " + gap.Reason
	}
	return lines
}

// consumerLines are the lines an operator reads for the daemon's durable consumers
// (api.ConsumerLagView), one per consumer in the state's order: how many messages wait for it and
// how old the oldest is (unknown when NATS did not give it), and, when it is stalled, how long it
// has handled none as of now, since its last handled message or, before the first, since the daemon
// started at startedAt; then what NATS did not answer, alone on the line when the consumer's info
// itself could not be read, since nothing else is known then. With no consumers (a daemon with no
// workflow) the one line says so.
func consumerLines(consumers []api.ConsumerLagView, startedAt, now time.Time) []string {
	if len(consumers) == 0 {
		return []string{"durable consumers: none"}
	}
	lines := make([]string, len(consumers))
	for i, consumer := range consumers {
		line := "consumer " + consumer.Name + ": "
		if consumer.Unavailable != "" && consumer.Waiting == 0 {
			lines[i] = line + "unavailable: " + consumer.Unavailable
			continue
		}
		switch {
		case consumer.Waiting == 0:
			line += "0 waiting"
		case consumer.OldestAgeSeconds != nil:
			line += fmt.Sprintf("%d waiting, oldest %s", consumer.Waiting, time.Duration(*consumer.OldestAgeSeconds)*time.Second)
		default:
			line += fmt.Sprintf("%d waiting, oldest unknown", consumer.Waiting)
		}
		if consumer.Stalled {
			if consumer.LastHandledAt != nil {
				line += fmt.Sprintf("; STALLED: none handled for %s, the last at %s",
					now.Sub(*consumer.LastHandledAt).Truncate(time.Second), consumer.LastHandledAt.UTC().Format(time.RFC3339))
			} else {
				line += fmt.Sprintf("; STALLED: none handled for %s, none since the daemon started at %s",
					now.Sub(startedAt).Truncate(time.Second), startedAt.UTC().Format(time.RFC3339))
			}
		}
		if consumer.Unavailable != "" {
			line += "; unavailable: " + consumer.Unavailable
		}
		lines[i] = line
	}
	return lines
}

// plainStateGiveUps is how many of the open give-ups the state lists `legion state` prints a line
// for: the newest, which say what is failing now, while the counts above them say how many there
// are and `legion state --json` lists every one the state carries. Ten lines fit beside the rest
// of the plain output on one screen, and a burst of one cause reads the same in ten as in fifty.
const plainStateGiveUps = 10

// intakeGiveUpLines are the lines an operator reads for the intake's open give-ups
// (api.IntakeGiveUpsView): one per consumer with any, `intake give-ups <consumer>: <n> open,
// newest seq <seq>`, the newest sequence being what `legion intake replay` and `legion intake
// discard` take as --through-seq to reach all of them; then the newest at most plainStateGiveUps,
// `intake give-up <id> <consumer> <subject> seq <n> gave up <time> after <d> deliveries
// (<reason>): <first line of the error>`, each id what --give-up takes. The subject and the error
// are terminalText: the publisher wrote the subject, and the error can quote it. With none open
// the one line says so.
func intakeGiveUpLines(giveUps api.IntakeGiveUpsView) []string {
	if len(giveUps.Consumers) == 0 {
		return []string{"intake give-ups: none"}
	}
	listed := giveUps.Newest[:min(len(giveUps.Newest), plainStateGiveUps)]
	lines := make([]string, 0, len(giveUps.Consumers)+len(listed))
	for _, consumer := range giveUps.Consumers {
		lines = append(lines, fmt.Sprintf("intake give-ups %s: %d open, newest seq %d", consumer.Name, consumer.Open, consumer.NewestSeq))
	}
	for _, giveUp := range listed {
		firstLine, _, _ := strings.Cut(giveUp.LastError, "\n")
		lines = append(lines, fmt.Sprintf("intake give-up %d %s %s seq %d gave up %s after %d deliveries (%s): %s",
			giveUp.ID, giveUp.Consumer, terminalText(giveUp.Subject), giveUp.StreamSeq, giveUp.GaveUpAt.UTC().Format(time.RFC3339),
			giveUp.Deliveries, giveUp.Reason, terminalText(firstLine)))
	}
	return lines
}

// terminalText is text another party wrote, as a line of the operator's terminal shows it: each
// character that does not print is written as its Go escape (`\x1b`, `\a`, `\u202e`), so an ESC
// or a BEL in it cannot clear the screen, set the window title or reorder the line. Text that
// prints is left as it is, so the line keeps the form the guides show. The state's text holds no
// NUL and no byte that is not UTF-8 (storetext.Text), but it can still hold these characters.
func terminalText(text string) string {
	first := strings.IndexFunc(text, func(r rune) bool { return !unicode.IsPrint(r) })
	if first < 0 {
		return text
	}
	var shown strings.Builder
	shown.WriteString(text[:first])
	for _, r := range text[first:] {
		if unicode.IsPrint(r) {
			shown.WriteRune(r)
			continue
		}
		quoted := strconv.QuoteRune(r)
		shown.WriteString(quoted[1 : len(quoted)-1])
	}
	return shown.String()
}

// stateAddress is where a daemon answers: the configured bind and port, with --port overriding
// the port. --port on its own reads a daemon on this box without a configuration to load, which
// is how a proof script reads a legion whose file it did not write. A wildcard bind is dialled
// the same way `legion status` dials it — one rule for both commands that read a configured
// bind. The file is read by the validating loader: finding an address is no reason to run both
// GitHub Apps' private_key_command, which config.Load does on every read.
func stateAddress(configPath string, port int) (string, error) {
	if configPath == "" && port != 0 {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
	}
	if configPath == "" {
		configPath = defaultConfigPath
	}
	cfg, err := config.LoadForValidation(configPath, nil)
	if err != nil {
		return "", err
	}
	if port == 0 {
		port = cfg.Port
	}
	return net.JoinHostPort(dialHost(cfg.Bind), strconv.Itoa(port)), nil
}

func newLegionsCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "legions",
		Short: "list the daemons registered on this machine",
		Args:  noArguments,
	}
	asJSON := cmd.Flags().Bool("json", false, "print the registry as JSON")
	cmd.RunE = runs(func(context.Context, []string) int { return runLegions(*asJSON, stdout, stderr) })
	return cmd
}

func runLegions(asJSON bool, stdout, stderr io.Writer) int {
	legions, err := registryPath()
	if err != nil {
		fmt.Fprintf(stderr, "legion legions: %v\n", err)
		return 1
	}
	entries, err := registry.Read(legions)
	if err != nil {
		fmt.Fprintf(stderr, "legion legions: %v\n", err)
		return 1
	}

	if asJSON {
		if entries == nil {
			entries = []registry.Entry{}
		}
		encoded, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "legion legions: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s\n", encoded)
		return 0
	}
	for _, entry := range entries {
		fmt.Fprintf(stdout, "%s %d %d %s\n",
			entry.Team, entry.PID, entry.Port, entry.StartedAt.Format(time.RFC3339))
	}
	return 0
}

// exactWords refuses every count of words but n: the command's arguments, which what names.
func exactWords(n int, what string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return usageError{fmt.Errorf("name %s (got %d words)", what, len(args))}
		}
		return nil
	}
}

func newStatusCommand(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status <team>",
		Short: "say whether a team's daemon is running",
		Args:  exactWords(1, "one team"),
		RunE: runs(func(ctx context.Context, args []string) int {
			return runStatus(ctx, args[0], stdout, stderr)
		}),
	}
}

func runStatus(ctx context.Context, team string, stdout, stderr io.Writer) int {
	entry, found, err := findLegion(team)
	if err != nil {
		fmt.Fprintf(stderr, "legion status: %v\n", err)
		return 1
	}
	// The registry says which process; the process says whether it is still there; /healthz says
	// whether it is still serving. A legion is all three.
	if !found || !registry.Alive(entry.PID) {
		fmt.Fprintf(stdout, "legion %s: not running\n", team)
		return 0
	}
	address := net.JoinHostPort(dialHost(entry.Bind), strconv.Itoa(entry.Port))
	if _, err := get(ctx, "http://"+address+"/healthz"); err != nil {
		fmt.Fprintf(stderr, "legion %s: pid %d is alive but %s did not answer: %v\n",
			team, entry.PID, address, err)
		return 1
	}
	fmt.Fprintf(stdout, "legion %s: running — pid %d, port %d, healthy, started %s\n",
		team, entry.PID, entry.Port, entry.StartedAt.Format(time.RFC3339))
	return 0
}

// dialHost is where a CLI reaches a legion that recorded its bind: a daemon bound to every
// interface answers on loopback, and `bind` is a shipped key the deployment overlays set, so the
// address is the one the daemon recorded rather than loopback by assumption.
func dialHost(bind string) string {
	switch bind {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	}
	return bind
}

func newRestartCommand(stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "restart <team>",
		Short: "stop a registered daemon and start it again from the configuration it recorded",
		Args:  exactWords(1, "one team"),
		RunE: runs(func(ctx context.Context, args []string) int {
			return runRestart(ctx, args[0], stderr)
		}),
	}
}

func runRestart(ctx context.Context, team string, stderr io.Writer) int {
	entry, found, err := findLegion(team)
	if err != nil {
		fmt.Fprintf(stderr, "legion restart: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintf(stderr, "legion restart: no legion is registered for %s\n", team)
		return 1
	}
	if registry.Alive(entry.PID) {
		if err := stopProcess(entry.PID); err != nil {
			fmt.Fprintf(stderr, "legion restart: %v\n", err)
			return 1
		}
	}
	// The restarted legion is this process, started from the configuration the stopped one
	// recorded: a restart changes the process, never the deployment.
	return start(ctx, entry.ConfigPath, stderr)
}

// stopProcess asks a daemon to stop and waits until it is gone, so what follows a stop — a
// status, a start of the same team — sees the box as the stop left it.
func stopProcess(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(stopTimeout)
	for registry.Alive(pid) {
		if time.Now().After(deadline) {
			return fmt.Errorf("pid %d is still running %s after SIGTERM", pid, stopTimeout)
		}
		time.Sleep(aliveInterval)
	}
	return nil
}

func registryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory the legions registry lives under: %w", err)
	}
	return registry.Path(nil, home), nil
}

func findLegion(team string) (registry.Entry, bool, error) {
	legions, err := registryPath()
	if err != nil {
		return registry.Entry{}, false, err
	}
	return registry.Find(legions, team)
}

// get reads one route of a daemon's API, under the one budget every CLI read of it takes
// (requestTimeout). A read the budget ends says so in the words a reader needs — the route, how
// long it took and the budget it passed — rather than in the transport's own
// `context deadline exceeded`, which names none of the three and leaves the reader to guess
// whether the daemon is slow, the body is large or the command gave up early. The caller's own
// cancellation is not that case and keeps the error it has.
func get(ctx context.Context, url string) ([]byte, error) {
	budget := programbound.Of(&requestTimeout)
	timed, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	began := time.Now()
	refuse := func(err error) error {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("read %s: no answer in %s, past the %s this command allows one read of a daemon's API",
				url, time.Since(began).Round(time.Millisecond), budget)
		}
		return fmt.Errorf("read %s: %w", url, err)
	}
	request, err := http.NewRequestWithContext(timed, http.MethodGet, url, nil)
	if err != nil {
		return nil, refuse(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, refuse(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, refuse(err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d: %s", url, response.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// processEnvironment is this process's environment by name, the last entry for a name winning as
// it does for a child the process starts.
func processEnvironment() map[string]string {
	env := map[string]string{}
	for _, pair := range os.Environ() {
		if name, value, ok := strings.Cut(pair, "="); ok {
			env[name] = value
		}
	}
	return env
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args, os.Stdout, os.Stderr))
}
