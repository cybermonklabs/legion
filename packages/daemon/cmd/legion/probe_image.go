package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/shim"
)

// imageClaudeRoot and imageClaudeCodePin are where the worker image carries the claude-envoy root
// and the Claude Code pin (packages/daemon/docker/worker.Dockerfile; claudePluginDir and
// claudeCodePin, internal/runtime/sandbox/names.go, which a Claude role's pod names): the defaults
// of --claude-root and --claude-code-pin.
const (
	imageClaudeRoot    = "/opt/legion/claude-envoy"
	imageClaudeCodePin = "/opt/legion/claude-code"
)

// digits is what --daemon-api-version accepts before it is read as a number.
var digits = regexp.MustCompile(`^[0-9]+$`)

// newProbeImageCommand is `legion probe-image`, run inside the worker image: by its build's final step
// (packages/daemon/docker/worker.Dockerfile), and with the daemon's own contract by the daemon's
// probe Sandbox, which reads the OK line back from the pod's log (internal/runtime/sandbox,
// ProbeImage). Both pass --plugin-root and --envoy-plugin-root, the Legion and Envoy plugin
// directories a pod loads as its two explicit extensions, and the command refuses a run without
// either (exit 2): the probe certifies the lane a pod uses, so a build line that lost a flag fails
// the build instead of probing a lane no pod loads. Every run also checks the Claude lane:
// --claude-root, the claude-envoy package root the image carries, and --claude-code-pin, the
// directory of the Claude Code pin a pod installs Claude Code from at launch, both defaulting to
// where the worker image carries them, so the daemon's probe Sandbox, which names neither, checks
// them as the build does. The image carries no Claude Code — its licence grants no
// redistribution and the image is public — so the lane holds the root's built channel server and
// generated Legion plugin, and its manifest to the same contract, and holds the pin to one a pod
// can install and verify, exact and checksummed (daemon.ProbeImage, whose verifyClaude says why
// it reads the pin rather than installing from it); the claude-code capability row checks the
// same two facts.
// It runs the image's launch probes under the image's own environment (daemon.ProbeImage), then
// the capability check (capabilities.CheckImage) under the same environment and launch: every
// image-site capability of the list every worker is held to (LEGION-578), with the whole table
// printed, one line per capability, before anything else — so the probe pod's log reads as the
// declared list — and a missing one named on stderr, exit 1, after the table. Then it reads
// whether the image's Oh My Pi falls back to another model (capabilities.ReadModelFallback): a
// failure to read it is not a refusal — the probe writes the failure to stderr and carries on
// with the mark bootprobe.ModelFallbackUnread, so the deployment's capability report shows that
// row open rather than the probe's own line going missing. It then reads the image's Oh My Pi's
// version and whether its help lists --session-id (capabilities.ReadOmpVersion and
// ReadSessionIDFlag) onto the OK line's omp-version and omp-session-id marks, each `unread` with
// the failure on stderr when its read fails before the deadline, since only a daemon that sets an
// exchange audience needs the flag and refuses the image without it; a read the deadline cut has
// no verdict, like every other check. When every check passes, it prints
// bootprobe.OKLine with the capabilities, model-fallback, omp-version and omp-session-id marks; a
// failed capability is the probe's message, exit 1, so a broken image never publishes. Each check
// is judged as it returns, by the deadline --probe-budget sets: what it saw fail before the
// deadline is exit 1, while a check the deadline cut, a capability row among them
// (capabilities.StatusUnanswered), has no verdict, which ends nothing, so the later checks still
// run. Where nothing failed and something has no verdict, it prints bootprobe.NoVerdictLine, naming
// each, to stderr and exits bootprobe.NoVerdictExit, which asks the daemon's probe Sandbox for
// another attempt rather than refusing the image. Unlike the TypeScript command, the contract is
// always checked: with none named, against this binary's own DaemonAPIVersion, which the plugin
// packed from the same commit must declare. The load probe also holds every task agent the prompts
// dispatch to its own model; the OK line says so (agent-models=resolved), or that the build's
// probe, which has no operator model configuration, skipped it (--skip-agent-models).
// With --skip-browser, the build of an image for a deployment that turns Oh My Pi's browser off
// (`browser.enabled: false`), which carries no Chromium, the capability check leaves the browser
// row unchecked, and the OK line's capabilities mark says so (capabilities=browser-skipped, which
// the daemon's probe Sandbox refuses: it never passes the flag, and passes only an image whose
// whole capability list was checked). The
// probe Sandbox starts it on the pod baseline's variables, with no state home of its
// own: with --pod-safety, as a worker's shim starts Oh My Pi (podsafety.Apply: the two
// session-placing variables where the pod leaves them unset, and no overlay of Legion's own,
// since it writes none, so the probe reads the role's own PI_CONFIG_FILES as its pod carries
// it), but not the per-role state home the shim makes (podsafety.EnsureStateHome): the probe
// pod runs one container, so no broker lock collides; with --provider-env-dir, each
// provider key exported after it as the shim exports them (shim.ReadProviderEnv); and with
// --role-references, the references of the role prompts the daemon inlines into its pods
// (promptrefs.Encode's encoding, decoded as the flags are read). Without it, the probe resolves the
// references of the role prompts this binary embeds (prompts.RoleReferences). When its environment
// names a NATS nkey seed (a probe pod's NATS_NKEY_SEED_FILE, the providers Secret's key), that seed
// is read as the daemon reads its own (natsauth.Seed) and must be a user's, and the line before the
// OK line names that user's public key (bootprobe.NATSUserLine), never the seed, for the daemon to
// compare with its own. With --role-namespaces, which the probe Sandbox passes and the image build
// does not (a build's container may not make the namespaces), it first runs one role generation as
// an issue pod's launcher runs every role, its own `legion version` as the shim, over a layout of
// its own in that directory (launcher.CheckRoleNamespaces), and refuses, exit 1, when the
// namespaces, a mount role-init makes or the shim fails there. That layout's home is an empty
// directory on the probe's volume carrying no mount, so the check does not bind a home's volume
// back or copy a config home.
func newProbeImageCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "probe-image --plugin-root <dir> --envoy-plugin-root <dir>",
		Short:  "run the worker image's launch probes (the image build and the daemon's probe Sandbox run it)",
		Hidden: true,
	}
	flags := cmd.Flags()
	f := &probeImageFlags{}
	flags.StringVar(&f.omp, "omp", "", "the OMP executable to probe (default: $LEGION_OMP_PATH)")
	flags.StringVar(&f.contract, "daemon-api-version", strconv.Itoa(api.DaemonAPIVersion),
		"the daemon API contract the image's pi-legion must declare (the daemon's probe Sandbox passes its own)")
	flags.StringVar(&f.pluginRoot, "plugin-root", "", "the Legion plugin directory a pod loads as an explicit extension, which the load probe loads the same way (required)")
	flags.StringVar(&f.envoyPluginRoot, "envoy-plugin-root", "", "the Envoy plugin directory a pod loads as its other explicit extension, which the load probe loads the same way (required)")
	flags.StringVar(&f.claudeRoot, "claude-root", imageClaudeRoot, "the claude-envoy package root a worker on the `claude` backend loads, which the Claude lane checks")
	flags.StringVar(&f.claudeCodePin, "claude-code-pin", imageClaudeCodePin, "the directory of the package.json and package-lock.json a pod installs Claude Code from at launch, which the Claude lane checks")
	flags.BoolVar(&f.podSafety, "pod-safety", false, "run the probes on a pod's baseline (internal/podsafety), as a pod's shim starts Oh My Pi")
	flags.StringVar(&f.providerEnvDir, "provider-env-dir", "", "a directory whose files NAME=contents the probes' Oh My Pi gets, as a worker's shim exports them")
	flags.DurationVar(&f.probeBudget, "probe-budget", 0, "the duration the daemon's probe pod reserves for this command")
	flags.BoolVar(&f.skipAgentModels, "skip-agent-models", false, "leave the prompt-named task agents' models unresolved (the image build's probe)")
	flags.BoolVar(&f.skipBrowser, "skip-browser", false, "leave the browser row unchecked: the build of an image for a deployment that turns Oh My Pi's browser off (browser.enabled: false), which carries no Chromium")
	flags.StringVar(&f.roleReferences, "role-references", "", "the task agents and skills the daemon's own role prompts name, resolved in place of the role prompts this binary embeds")
	flags.StringVar(&f.roleNamespaces, "role-namespaces", "", "run one role generation's namespaces (legion launcher role-init) over a layout of its own in this directory, on a volume of its own, and refuse when it fails; its home is an empty directory there, not the image's (the probe Sandbox passes it)")
	flags.StringVar(&f.testPostgres, "test-postgres", "", "prove the pod's own test database in this directory — the image's legion-testpg checked and started, a database made and dropped through LEGION_TEST_PG_DSN, and the server stopped — and refuse when it fails (the probe Sandbox passes it for a deployment that sets runtime.kubernetes.test_postgres; the image build does not)")
	flags.Var(&f.roleOverlays, strings.TrimPrefix(bootprobe.RoleConfigFilesFlag, "--"), "<role>=<file>[:<file>…]: one whole PI_CONFIG_FILES tail of a role (role_config_files, and the overlays of one arm of one declared experiment), read after the environment's PI_CONFIG_FILES as that role's Oh My Pi starts on it; repeatable, once per role per arm, and each occurrence is read on its own (the probe Sandbox passes it)")
	cmd.RunE = runs(func(ctx context.Context, args []string) int { return runProbeImage(ctx, f, args, stdout, stderr) })
	return cmd
}

// probeImageFlags are `legion probe-image`'s flag values, as cobra reads them.
type probeImageFlags struct {
	omp, contract, pluginRoot, envoyPluginRoot, claudeRoot, claudeCodePin, providerEnvDir string
	roleReferences, roleNamespaces, testPostgres                                          string
	podSafety, skipAgentModels, skipBrowser                                               bool
	probeBudget                                                                           time.Duration
	roleOverlays                                                                          roleConfigFilesFlag
}

func runProbeImage(ctx context.Context, f *probeImageFlags, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "legion probe-image: unexpected argument %q\n", args[0])
		return 2
	}
	if f.pluginRoot == "" {
		fmt.Fprintln(stderr, "legion probe-image: --plugin-root is required: the Legion plugin directory a pod loads as an explicit extension, which the load probe loads the same way")
		return 2
	}
	if f.envoyPluginRoot == "" {
		fmt.Fprintln(stderr, "legion probe-image: --envoy-plugin-root is required: the Envoy plugin directory a pod loads as its other explicit extension, which the load probe loads the same way")
		return 2
	}
	if f.claudeRoot == "" || f.claudeCodePin == "" {
		fmt.Fprintln(stderr, "legion probe-image: --claude-root and --claude-code-pin each name a directory: the claude-envoy plugin and the Claude Code pin a worker on the `claude` backend needs, which the Claude lane checks")
		return 2
	}

	if f.probeBudget < 0 {
		fmt.Fprintln(stderr, "legion probe-image: --probe-budget must not be negative")
		return 1
	}
	if f.probeBudget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.probeBudget)
		defer cancel()
	}
	if f.roleNamespaces != "" {
		self, err := os.Executable()
		if err == nil {
			err = launcher.CheckRoleNamespaces(f.roleNamespaces, self, "version")
		}
		if err != nil {
			fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
			return 1
		}
	}
	// The pod's own test database, under the runtime a pod runs on: the probe pod is the first
	// place this deployment's gVisor starts the server, so a runtime it cannot run under refuses
	// the boot probe rather than every pod's first Postgres test.
	if f.testPostgres != "" {
		if err := launcher.CheckTestPostgres(f.testPostgres); err != nil {
			fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
			return 1
		}
	}
	// The role prompts a pod is handed: the daemon's, when it passes their references, else the
	// ones this binary embeds.
	var references promptrefs.Names
	if f.roleReferences == "" {
		references = prompts.RoleReferences()
	} else {
		decoded, err := promptrefs.Decode(f.roleReferences)
		if err != nil {
			fmt.Fprintf(stderr, "legion probe-image: --role-references: %v\n", err)
			return 1
		}
		references = decoded
	}
	// --omp and LEGION_OMP_PATH each name the OMP executable, a path, as LEGION_OMP_PATH is for the
	// daemon (omplaunch.ResolveInvocation). Every launch below is shell text, so the path goes in as
	// one shell word: a path holding a space or a glob character still names the one file.
	omp := f.omp
	if omp == "" {
		omp = os.Getenv("LEGION_OMP_PATH")
	}
	if omp == "" {
		fmt.Fprintln(stderr, "legion probe-image: set LEGION_OMP_PATH (or pass --omp) to the OMP executable to probe")
		return 1
	}
	invocation := shellprefix.Word(omp)
	expected, err := strconv.Atoi(f.contract)
	if !digits.MatchString(f.contract) || err != nil || expected < 1 {
		fmt.Fprintf(stderr, "legion probe-image: --daemon-api-version must be a positive integer (got %q)\n", f.contract)
		return 1
	}
	workDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	// The seed a pod's Envoy connections authenticate with: a probe pod's NATS_NKEY_SEED_FILE names
	// the providers Secret's NATS_NKEY_SEED, which must be a user's seed, and whose user the daemon
	// compares with its own seed's. The image build's probe names none.
	natsUser := ""
	if seed, err := natsauth.Seed("", os.LookupEnv); err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	} else if seed != "" {
		if natsUser, err = natsauth.PublicKey(seed); err != nil {
			fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
			return 1
		}
	}
	environ := os.Environ()
	if f.podSafety {
		environ = podsafety.Apply(environ)
	}
	if f.providerEnvDir != "" {
		providerEnv, err := shim.ReadProviderEnv(f.providerEnvDir, os.LookupEnv)
		if err != nil {
			fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
			return 1
		}
		environ = append(environ, providerEnv...)
	}
	env := map[string]string{}
	for _, pair := range environ {
		if name, value, ok := strings.Cut(pair, "="); ok {
			env[name] = value
		}
	}
	// From here on each step is judged as it returns: a failure it saw while ctx was alive ends
	// the probe with exit 1, and what the probe's deadline (--probe-budget) cut has no verdict,
	// which ends nothing, so the later steps still run and a failure they see still fails. noVerdict
	// names each step with no verdict; where nothing failed, they make the exit
	// bootprobe.NoVerdictExit.
	var noVerdict []string
	err = daemon.ProbeImage(ctx, daemon.ImageProbe{
		Omp: invocation, Contract: expected, Env: env, WorkDir: workDir, PluginRoot: f.pluginRoot, EnvoyPluginRoot: f.envoyPluginRoot,
		ClaudeRoot: f.claudeRoot, ClaudeCodePin: f.claudeCodePin,
		RoleReferences: references, SkipAgentModels: f.skipAgentModels,
		Log: slog.New(slog.NewTextHandler(stderr, redact.HandlerOptions(&slog.HandlerOptions{Level: slog.LevelWarn}))),
	})
	switch {
	case err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && errors.Is(err, context.DeadlineExceeded):
		noVerdict = append(noVerdict, "launch probes: "+err.Error())
	case err != nil:
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	// The capability check reads the environment the launch probes ran under — the pod baseline's
	// and the provider keys' included — so a PATH or a PUPPETEER_EXECUTABLE_PATH the operator's pod
	// env sets is checked as a worker's Oh My Pi would honour it. The table is printed whole even
	// when a row is missing: the operator reads what was checked, then why it failed.
	img := capabilities.Image{
		Launch:        invocation,
		Env:           environ,
		WorkDir:       workDir,
		ClaudeRoot:    f.claudeRoot,
		ClaudeCodePin: f.claudeCodePin,
		SkipBrowser:   f.skipBrowser,
	}
	lines, err := capabilities.CheckImage(ctx, img)
	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	for _, line := range lines {
		if line.Status == capabilities.StatusUnanswered {
			noVerdict = append(noVerdict, "capability "+string(line.Name)+": not checked")
		}
	}
	modelFallback, err := capabilities.ReadModelFallback(ctx, img)
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: model-fallback not read; the deployment's capability report shows the row open: %v\n", err)
		modelFallback = bootprobe.ModelFallbackUnread
	}
	// Which Oh My Pi the image carries, and whether it takes the --session-id a daemon under an
	// exchange audience starts each new role with. A read that fails before the deadline refuses
	// nothing: its mark says unread, with the failure on stderr, and the daemon that reads the
	// marks decides. A read that failed once ctx ended has no verdict, as checkRoleOverlays judges
	// a role's read: the deadline stops the read's process group, so the failure may be its own
	// doing, and an omp-session-id=unread it would leave is a mark a daemon under an exchange
	// audience refuses for good. A read that answered keeps its answer, whenever it returned.
	ompVersion, err := capabilities.ReadOmpVersion(ctx, img)
	switch {
	case err != nil && ctx.Err() != nil:
		noVerdict = append(noVerdict, "omp-version: "+err.Error())
	case err != nil:
		fmt.Fprintf(stderr, "legion probe-image: Oh My Pi's version not read; the OK line says omp-version=%s: %v\n", bootprobe.OmpVersionUnread, err)
		ompVersion = bootprobe.OmpVersionUnread
	}
	sessionIDFlag, err := capabilities.ReadSessionIDFlag(ctx, img)
	switch {
	case err != nil && ctx.Err() != nil:
		noVerdict = append(noVerdict, "omp-session-id: "+err.Error())
	case err != nil:
		fmt.Fprintf(stderr, "legion probe-image: whether Oh My Pi takes --session-id not read; the OK line says omp-session-id=%s, which a daemon that sets runtime.kubernetes.agent_secrets.exchange_audience refuses: %v\n",
			bootprobe.SessionIDFlagUnread, err)
		sessionIDFlag = bootprobe.SessionIDFlagUnread
	}
	// Each role's own overlays, read as that role's Oh My Pi will start on them, after the table so
	// the log still reads as the declared list first.
	roleImage := capabilities.Image{Launch: invocation, Env: environ, WorkDir: workDir}
	code, overlaysNoVerdict := checkRoleOverlays(ctx, f.roleOverlays, func(value, alone []string) (string, error) {
		overlays := append(capabilities.OverlayEntries(envValue(environ, "PI_CONFIG_FILES")), value...)
		return capabilities.CheckOverlays(ctx, roleImage, overlays, alone)
	}, stderr)
	if code != 0 {
		return code
	}
	noVerdict = append(noVerdict, overlaysNoVerdict...)
	if len(noVerdict) > 0 {
		fmt.Fprintln(stderr, bootprobe.NoVerdictLine(noVerdict))
		return bootprobe.NoVerdictExit
	}
	agentModels := bootprobe.AgentModelsResolved
	if f.skipAgentModels {
		agentModels = bootprobe.AgentModelsSkipped
	}
	checked := bootprobe.CapabilitiesAll
	if f.skipBrowser {
		checked = bootprobe.CapabilitiesBrowserSkipped
	}
	if natsUser != "" {
		fmt.Fprintln(stdout, bootprobe.NATSUserLine(natsUser))
	}
	fmt.Fprintln(stdout, bootprobe.OKLine(omp, expected, agentModels, checked, modelFallback, ompVersion, sessionIDFlag))
	return 0
}

// roleOverlays is one --role-config-files value: a role and its own overlays, in order.
type roleOverlays struct {
	role  string
	files []string
}

// roleConfigFilesFlag is every --role-config-files value, in the order given, one entry per
// occurrence: a role is named once per whole PI_CONFIG_FILES tail it starts on, so a deployment
// that declares two arms naming overlays passes that role one value per arm, and each is read on
// its own — the union of two arms is a configuration no launch produces. The probe loads no
// settings, so the flag knows a role by its name alone: a name of the role grammar (claim.IsRole:
// one of Legion's or a role the deployment defines) or the controller. Any other name, and a file
// that is not an absolute path, are refused as the flag is read.
type roleConfigFilesFlag []roleOverlays

func (f *roleConfigFilesFlag) String() string {
	values := make([]string, len(*f))
	for i, entry := range *f {
		values[i] = bootprobe.RoleConfigFilesValue(entry.role, entry.files)
	}
	return strings.Join(values, " ")
}

// Type is how the flag's help names its value.
func (f *roleConfigFilesFlag) Type() string { return "role=files" }

func (f *roleConfigFilesFlag) Set(value string) error {
	role, files, err := bootprobe.ParseRoleConfigFiles(value)
	if err != nil {
		return err
	}
	if !legionclaim.IsRole(legionclaim.Role(role)) && legionclaim.Role(role) != legionclaim.RoleController {
		return fmt.Errorf("%q is not a role", role)
	}
	for _, file := range files {
		if !filepath.IsAbs(file) {
			return fmt.Errorf("%s's overlay %q is not an absolute path", role, file)
		}
	}
	*f = append(*f, roleOverlays{role: role, files: files})
	return nil
}

// envValue is the value environ gives name, "" when it gives none.
func envValue(environ []string, name string) string {
	value := ""
	for _, pair := range environ {
		if found, v, _ := strings.Cut(pair, "="); found == name {
			value = v
		}
	}
	return value
}

// checkRoleOverlays runs check on each role's whole PI_CONFIG_FILES value in order and answers
// the exit code: 1, after printing the role's RoleOverlaysRefusedLine, for the first value check
// refuses, else 0. A run check reports as unanswered (a timeout) is printed and does not stop
// that value's verdict: a refusal found after it still refuses.
//
// Each call of check is judged by ctx, the probe's, as it returns. One that returned after ctx
// ended has no verdict, whether it answered a refusal or left a run unanswered, since a refusal
// read then may be the deadline's own doing: the end of ctx stops the read's process group with a
// SIGTERM (procgroup), and an Oh My Pi that then exits on its own with a non-zero status reads as
// refusing (capabilities.CheckOverlays). noVerdict names each such call, "role overlays: <role>:
// <the refusal or the error>", for the next attempt to read again; it fails nothing.
//
// check takes the whole value and, beside it, the overlays of that value to read on their own.
// Each distinct value is read once and each distinct overlay is read alone once, over every
// entry (capabilities.OverlayReads, which the boot gate's own read holds too): a role's own
// files stand in the value of every arm the project declares, and Oh My Pi gives one file the
// same answer whichever value it stands in, so a deployment that declares arms would otherwise
// pay for the thing it added at every probe. The refusal still names the first role holding the
// refused value, since the loop reaches it before any other role of it.
func checkRoleOverlays(ctx context.Context, entries []roleOverlays, check func(value, alone []string) (string, error), stderr io.Writer) (code int, noVerdict []string) {
	reads := capabilities.OverlayReads{}
	for _, entry := range entries {
		unread, first := reads.Unread(entry.files)
		if !first {
			continue
		}
		why, err := check(entry.files, unread)
		if ctx.Err() != nil {
			if why == "" && err != nil {
				why = err.Error()
			}
			if why != "" {
				noVerdict = append(noVerdict, "role overlays: "+entry.role+": "+why)
			}
			continue
		}
		if err != nil {
			fmt.Fprintf(stderr, "legion probe-image: role %s's overlays were read but Oh My Pi did not answer on some of them: %v\n", entry.role, err)
		}
		if why != "" {
			fmt.Fprintln(stderr, bootprobe.RoleOverlaysRefusedLine(entry.role, why))
			return 1, noVerdict
		}
	}
	return 0, noVerdict
}
