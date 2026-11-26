package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// daemonTools are the binaries Legion itself runs on the host, or hands a pane: the git and jj of
// workspace provisioning and removal (internal/workspace), and the real gh every pane's `legion gh`
// runs, which a pane is told as LEGION_GH_PATH. Boot resolves each once, so no Legion-owned child is
// a PATH lookup that whatever wrapper heads the operator's PATH could answer. A pane's gh is the
// launcher directory's `legion gh`, and its git and its jj are the launcher directory's
// `legion git --after` and `legion jj --after` wrappers where workerbin.Install writes each. A
// daemon started from a pane's shell can resolve either wrapper here. Workspace jj then runs the
// git wrapper as its git.executable-path, which does no harm: the wrapper reaches a real git one
// exec later, and provisioning makes no commit. The jj wrapper does no harm either: `legion jj`
// runs every command but a push on the next jj with its own words, provisioning's commands and
// checkJJVersion's `jj --version` with no read before them, since each is a built-in jj command or
// a version flag, and provisioning runs no push. The answer loop's gh tool runs the same real gh
// (answersGH).
var daemonTools = []string{"git", "jj", "gh"}

// toolEnv is the daemon's own override for a tool at boot, LEGION_<TOOL>_PATH. A pane is told
// LEGION_GH_PATH, the gh boot resolved, so a daemon started from a pane's shell resolves that
// pane's real gh rather than the `legion gh` first on the pane's PATH.
func toolEnv(tool string) string { return "LEGION_" + strings.ToUpper(tool) + "_PATH" }

// resolveTools resolves each daemon tool (resolveTool). It names every missing tool with its
// override in one error, and refuses a jj older than the oldest the daemon runs (checkJJVersion).
func resolveTools(lookupEnv func(string) (string, bool)) (map[string]string, error) {
	tools := map[string]string{}
	var missing []string
	for _, tool := range daemonTools {
		found, err := resolveTool(lookupEnv, tool)
		if err != nil {
			return nil, err
		}
		if found == "" {
			missing = append(missing, fmt.Sprintf("%s (set %s to an absolute executable path)", tool, toolEnv(tool)))
			continue
		}
		tools[tool] = found
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required daemon tools: %s", strings.Join(missing, ", "))
	}
	if err := checkJJVersion(tools["jj"]); err != nil {
		return nil, err
	}
	return tools, nil
}

// resolveTool is tool's LEGION_<TOOL>_PATH override, which must be an absolute executable, or the
// first executable tool on PATH, or "" when there is neither. A gh found on PATH that is a script
// is refused (RefuseScriptGH); an override is the operator's own word and is taken as it is.
func resolveTool(lookupEnv func(string) (string, bool), tool string) (string, error) {
	if configured, ok := lookupEnv(toolEnv(tool)); ok && configured != "" {
		if !filepath.IsAbs(configured) || !isExecutable(configured) {
			return "", fmt.Errorf("%s is not an absolute executable path: %q", toolEnv(tool), configured)
		}
		return configured, nil
	}
	path, _ := lookupEnv("PATH")
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, tool)
		if dir != "" && filepath.IsAbs(candidate) && isExecutable(candidate) {
			if tool == "gh" {
				if err := RefuseScriptGH(candidate); err != nil {
					return "", err
				}
			}
			return candidate, nil
		}
	}
	return "", nil
}

// answersGH is the real gh the answer loop's gh tool runs on the daemon's own host, which a daemon
// with the answer loop on resolves at boot under either runtime: the gh resolveTools already
// resolved for a tmux daemon's own tools, else resolveTool's (LEGION_GH_PATH, or the first gh on
// PATH, never a script). A Kubernetes daemon resolves none otherwise, since its pods run the image's gh. Boot is
// refused, naming gh and its override, when neither gives one.
func answersGH(lookupEnv func(string) (string, bool), resolved map[string]string) (string, error) {
	if gh := resolved["gh"]; gh != "" {
		return gh, nil
	}
	gh, err := resolveTool(lookupEnv, "gh")
	if err != nil {
		return "", err
	}
	if gh == "" {
		return "", fmt.Errorf("answers needs gh on the daemon's host, which the answer loop's gh tool runs, and none was found: set %s to an absolute executable path, or put gh on PATH", toolEnv("gh"))
	}
	return gh, nil
}

// dispatchInstall is where the worker image keeps the dispatch command, the Envoy plugin's /bin/sh
// shim that execs bun on its bundled CLI (the image build's last step runs its --help,
// docs/kubernetes.md). The image's PATH does not hold that directory, and a Kubernetes daemon runs
// from that image, so this is where such a daemon finds it.
const dispatchInstall = "/opt/legion/pi-envoy/bin/dispatch"

// dispatchFound is what boot found for the answer loop's dispatch tool: the dispatch command, bun's
// directory, which that command's shim starts bun from, and the daemon's Dispatch token file, made
// absolute; or, where the loop offers no dispatch tool, why (absent), and none of the three.
type dispatchFound struct {
	dispatch, bunDir, tokenFile string
	absent                      string
}

// answersDispatch is the dispatch command the answer loop's dispatch tool runs, which a daemon with
// the answer loop on finds at boot under either runtime, as it finds the loop's gh: the
// LEGION_DISPATCH_PATH override, which must be an absolute executable, else the first dispatch on
// PATH, else install where it is executable (dispatchInstall, the worker image's). Unlike gh, a
// script is taken: the Envoy plugin's dispatch is a /bin/sh shim. bun, which that shim starts by
// name, is LEGION_BUN_PATH or else the first bun on PATH, and the tool runs with its directory on
// PATH. The tool runs against the daemon's own Dispatch, dispatchURL, with its token, tokenFile.
// Where the daemon names no dispatch_url, no dispatch_token_file (tokenFile "", as under
// dispatch_token_broker, whose token acts only as the daemon's own session), finds no dispatch, or
// no bun for it, nothing refuses the boot: the loop offers no dispatch tool, and absent says why,
// which the boot line and `legion config check` name (loopTools). An empty tokenFile is never made
// absolute, which would name the daemon's working directory as the tool's token file.
func answersDispatch(lookupEnv func(string) (string, bool), dispatchURL, tokenFile, install string) dispatchFound {
	if dispatchURL == "" {
		return dispatchFound{absent: "no dispatch_url, so no Dispatch to run it against"}
	}
	if tokenFile == "" {
		return dispatchFound{absent: "no dispatch_token_file, so no Dispatch token for an agent's session"}
	}
	dispatch, err := resolveTool(lookupEnv, "dispatch")
	if err != nil {
		return dispatchFound{absent: err.Error()}
	}
	if dispatch == "" && isExecutable(install) {
		dispatch = install
	}
	if dispatch == "" {
		return dispatchFound{absent: fmt.Sprintf("no dispatch command on PATH or at %s (set %s)", install, toolEnv("dispatch"))}
	}
	bun, err := resolveTool(lookupEnv, "bun")
	if err != nil {
		return dispatchFound{absent: err.Error()}
	}
	if bun == "" {
		return dispatchFound{absent: fmt.Sprintf("no bun for %s, which runs on it (set %s, or put bun on PATH)", dispatch, toolEnv("bun"))}
	}
	token, err := filepath.Abs(tokenFile)
	if err != nil {
		return dispatchFound{absent: fmt.Sprintf("dispatch_token_file %s: %v", tokenFile, err)}
	}
	return dispatchFound{dispatch: dispatch, bunDir: filepath.Dir(bun), tokenFile: token}
}

// loopTools are the answer loop's tools as boot found them: each the loop offers, in the loop's
// order, and each it does not offer with why.
type loopTools struct {
	offered []string
	absent  [][2]string
}

// loopToolsOf is the loop's tools with dispatch as boot found it and web_search as answers
// configures it: gh, which boot refuses to start without; dispatch where boot found its command;
// and web_search where answers offers it (config.Answers.WebSearchOff).
func loopToolsOf(dispatch dispatchFound, answers config.Answers) loopTools {
	tools := loopTools{offered: []string{store.ToolGH}}
	if dispatch.dispatch != "" {
		tools.offered = append(tools.offered, store.ToolDispatch)
	} else {
		tools.absent = append(tools.absent, [2]string{store.ToolDispatch, dispatch.absent})
	}
	if off := answers.WebSearchOff(); off == "" {
		tools.offered = append(tools.offered, store.ToolWebSearch)
	} else {
		tools.absent = append(tools.absent, [2]string{store.ToolWebSearch, off})
	}
	return tools
}

// answersToolsLine is the message of the boot's one line naming the loop's tools, which an
// after_merge check of INFRA's boot log reads, as its keys: tools, the list of those the loop
// offers, and one key per tool it does not offer, named for the tool, holding why.
const answersToolsLine = "answers: tools"

// attrs are the line's keys and values (answersToolsLine).
func (t loopTools) attrs() []any {
	attrs := []any{"tools", t.offered}
	for _, absent := range t.absent {
		attrs = append(attrs, absent[0], absent[1])
	}
	return attrs
}

// String is the line as `legion config check` prints it, in the words of the boot's line:
// `answers: tools [gh dispatch web_search]`, and after the list, for each tool the loop does not
// offer, `; <tool>: <why>`.
func (t loopTools) String() string {
	line := fmt.Sprintf("%s [%s]", answersToolsLine, strings.Join(t.offered, " "))
	for _, absent := range t.absent {
		line += fmt.Sprintf("; %s: %s", absent[0], absent[1])
	}
	return line
}

// AnswersToolsLine is `legion config check`'s line naming the answer loop's tools, read from the
// daemon's environment (lookup) as boot reads it (answersDispatch) and from cfg's answers
// (loopToolsOf), and false for a configuration without answers, whose daemon runs no loop.
func AnswersToolsLine(cfg config.Config, lookup func(string) (string, bool)) (string, bool) {
	if cfg.Answers == nil {
		return "", false
	}
	return loopToolsOf(answersDispatch(lookup, cfg.DispatchURL, cfg.DispatchTokenFile, dispatchInstall), *cfg.Answers).String(), true
}

// answersLeakCheck is where the shared public leak check comes from that the answer loop's gh tool
// runs before every call (ghtool.Host.LeakCheck), which a daemon with the answer loop on reads at
// boot beside its gh (answersGH); answersTools states the one rule each runtime keeps. Under tmux
// it is the machine's own check, resolved in the daemon's environment as a pane's is (leakguard's
// Resolve): boot refuses a machine that carries none, and the gh tool resolves the check again at
// each call the same way (ghtool.MachineLeakCheck). Under kubernetes it is the check the
// deployment's setting names (config's leak_check, leakguard.Setting), which the daemon reads from
// that repository at the tag it installs the check at once its GitHub App can read it, and holds to
// that manifest digest (installAnswersLeakCheck): its environment's LEGION_LEAK_CHECK_TAG, the pin
// the worker image sets for every container it runs, a pod's launcher's and the daemon's alike,
// since a Kubernetes daemon's environment holds none of the three places a check is resolved from.
// Boot is refused, naming the check and where it looked, when neither gives one: a Kubernetes
// deployment that names no setting, whose roles start without a check, is refused naming its two
// settings, before the pin is read.
func answersLeakCheck(runtimeName string, setting leakguard.Setting, lookupEnv func(string) (string, bool)) (check, tag string, err error) {
	if runtimeName == "kubernetes" {
		const needs = "answers needs the shared public leak check, which the answer loop's gh tool runs before every call"
		if !setting.Configured() {
			return "", "", errors.New(needs + ", and a Kubernetes daemon reads it from the repository the deployment's leak check setting names, " +
				"which names none: set " + leakguard.RepositoryVar + " and " + leakguard.ManifestVar + " in the daemon's environment, " +
				"or leak_check.repository and leak_check.manifest_sha256 in legion.yaml")
		}
		const pin = needs + ", and a Kubernetes daemon installs it at the tag " + leakguard.TagVariable + " names"
		tag, _ = lookupEnv(leakguard.TagVariable)
		switch {
		case tag == "":
			return "", "", errors.New(pin + ", which the daemon's environment does not set: run the daemon from the worker image, " +
				"which sets it to the image's pin, or set it to the tag runtime.kubernetes.image pins (its ARG LEAK_CHECK_TAG)")
		case !shimwire.ValidLeakCheckTag(tag):
			return "", "", fmt.Errorf("%s, which is %q, not of the published shape leak-check-v<n>", pin, tag)
		}
		return "", tag, nil
	}
	if check, err = leakguard.New(leakguard.Options{Env: leakCheckEnviron(lookupEnv)}).Resolve(); err != nil {
		return "", "", fmt.Errorf("answers needs the shared public leak check on the daemon's host, which the answer loop's gh tool runs before every call, "+
			"and none was found: %w: install it with the check's own installer, or set %s to its path", err, leakguard.CheckVar)
	}
	return check, "", nil
}

// leakCheckEnviron is the daemon's environment as the shared check is resolved from it: the four
// names leakguard's Resolve reads (the variable naming the check, PATH, and the two that place the
// configuration home), and never nil, which the guard would read as this process's environment.
func leakCheckEnviron(lookupEnv func(string) (string, bool)) []string {
	env := []string{}
	for _, name := range []string{leakguard.CheckVar, "PATH", "XDG_CONFIG_HOME", "HOME"} {
		if value, ok := lookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// RefuseScriptGH refuses gh, the first gh on PATH, when it is a script, a file whose first two
// bytes are `#!`. The real gh is a compiled program, so a script there is a gh wrapper: a
// machine's own shim, which picks a token of its own (or loops back through the `legion gh`
// wrapper first on a pane's PATH), a version manager's shim (asdf's), or a `legion gh` wrapper,
// which would run `legion gh` again. Run as the real gh, a wrapper answers in place of the GitHub
// App token of the owner's gh directory `legion gh` hands it. The check reads scripts only: a
// compiled multiplexer such as mise's shim passes, and falls back to the next gh on PATH when mise
// has no gh for the directory, so the refusal names an install path, not a shims directory. A
// LEGION_GH_PATH override is the operator's own word and is taken as it is; this check reads only
// what PATH found. `legion controller start` makes the same check on the operator's PATH.
func RefuseScriptGH(gh string) error {
	script, _, err := lookpath.Kind(gh)
	if err != nil {
		return fmt.Errorf("read %s, the first gh on PATH: %w", gh, err)
	}
	if script {
		return fmt.Errorf("the first gh on PATH, %s, is a script, not the real gh: a gh wrapper would answer in place of the GitHub App token `legion gh` gives it; set LEGION_GH_PATH to the real gh's install path (`mise which gh` or `asdf which gh` prints it; a version manager's shims directory is no install path)", gh)
	}
	return nil
}

// minimumJJ is the oldest jj the daemon runs, as major and minor: 0.38, the release that moved a
// repository's and a workspace's own configuration out of the repository and into the config
// home, under the id `.jj/repo/config-id` or `.jj/workspace-config-id` names (jj's CHANGELOG,
// 0.38.0). Provisioning and removal assume that layout: the runner removes a `.jj/repo/config.toml`
// with no config-id beside it as a legacy file jj would migrate into the configuration it reads
// (internal/workspace, disarmLegacyConfig), and on a jj before 0.38 that file is the repository's
// live configuration, the one `jj config set --repo` writes Legion's own
// git.abandon-unreachable-commits into. The worker image's jj is 0.46; this holds the host's jj,
// which the tmux runtime runs, to the same layout.
var minimumJJ = [2]int{0, 38}

// jjVersionTimeout bounds `jj --version`, which starts no other process.
const jjVersionTimeout = 30 * time.Second

// jjVersion reads the release from `jj --version`'s first line, `jj <major>.<minor>.<patch>`, a
// build's suffix after it (`-<hash>`, a fork's `-sami.<date>-<hash>`) ignored.
var jjVersion = regexp.MustCompile(`^jj (\d+)\.(\d+)\.\d+`)

// checkJJVersion runs jj --version and refuses a jj older than minimumJJ, or one whose version it
// cannot read, naming the path, what it found and LEGION_JJ_PATH.
//
// It takes no lock: `jj --version` prints the binary's own release and opens no repository, so it
// can leave no lock file behind for the next reader of one. R8 of dispatch://INFRA-3098 gives a
// process group of its own and the SIGTERM-then-grace-then-SIGKILL stop to the two command runners
// of internal/workspace, whose jj and git do take locks, and leaves a read that takes none as it
// is: this child needs neither the group nor the grace.
func checkJJVersion(jj string) error {
	override := toolEnv("jj")
	floor := fmt.Sprintf("%d.%d", minimumJJ[0], minimumJJ[1])
	ctx, cancel := context.WithTimeout(context.Background(), jjVersionTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, jj, "--version")
	command.Env = runtime.WithoutNATSSeeds(os.Environ())
	command.WaitDelay = time.Second
	output, err := command.Output()
	if err != nil {
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exited.Stderr)))
		}
		return fmt.Errorf("could not read the version of the jj at %s (`jj --version`: %v): set %s to an absolute path of jj %s or later", jj, err, override, floor)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	match := jjVersion.FindStringSubmatch(first)
	if match == nil {
		return fmt.Errorf("could not read the version of the jj at %s (`jj --version` printed %q): set %s to an absolute path of jj %s or later", jj, first, override, floor)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	if major < minimumJJ[0] || (major == minimumJJ[0] && minor < minimumJJ[1]) {
		return fmt.Errorf("the jj at %s is %s, older than %s, the oldest the daemon runs: set %s to an absolute path of jj %s or later", jj, strings.TrimPrefix(first, "jj "), floor, override, floor)
	}
	return nil
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func envValue(environ []string, name string) (string, bool) {
	for i := len(environ) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(environ[i], name+"="); ok {
			return value, true
		}
	}
	return "", false
}
