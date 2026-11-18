// Package omplaunch is how a Legion process launches a local Oh My Pi: the OMP invocation resolved
// from omp_invocation or LEGION_OMP_PATH, the operator's launch prefix before it, and the one
// --append-system-prompt word, the path of the file the prompt is written to. A tmux pane, the
// daemon's boot gate on the plugin every pane loads, and `legion controller start` on the
// operator's machine each launch it this way, so none of them depends on the tmux runtime to do it.
package omplaunch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// miseInvocation is the one configured invocation shape the daemon launches through mise.
var miseInvocation = regexp.MustCompile(`^mise x (\S+) -- omp$`)

// keepPathHead is what `mise x` starts in OMP's place, OMP's path and arguments after it: a shell
// that puts the first entry of the PATH the launch ran under back in front and then execs OMP.
// Every launch runs the fragment under a PATH led by its launcher directory (workerbin.Path): a
// pane's shell exports it, the boot gate runs under tmux.PaneEnvironment, and `legion controller
// start` under controllerEnvironment. `mise x` puts the tool's directories and each `_.path` entry
// of the mise config it reads ahead of that, and such an entry, a dotfiles shims directory say,
// can hold a gh, git or jj that is not Legion's. The launch's shell expands "${PATH%%:*}" before
// mise runs; the shell mise starts is /bin/sh by its absolute path, since a lookup would read
// mise's PATH; and its exec leaves OMP in the process the launch started, as mise's own exec does,
// so a pane's process, its marker and a search of /proc by name still find OMP. The rest of the
// PATH stays as mise made it, the launcher directory in it again where mise left the launch's.
const keepPathHead = `/bin/sh -c 'PATH=$1:$PATH && shift && exec "$@"' sh "${PATH%%:*}"`

// ResolveInvocation is the OMP launch fragment every pane and boot probe runs, resolved from
// the configured invocation and the daemon's own environment (resolveOmpInvocation, environment.ts).
// `LEGION_OMP_PATH` names a binary directly — the worker image, a build under test — and is used
// as its resolved real path: nothing runs between the launch and OMP, so OMP holds the launch's
// PATH as it is. Otherwise the invocation must be `mise x <tool> -- omp`: mise resolves the
// configured tool's own `bin/omp`, and the fragment still runs it through `mise x` so the tool's
// declared environment is activated, with the launch's first PATH entry put back ahead of what
// mise prepends (keepPathHead). The result is a shell fragment for a POSIX shell: each path is
// quoted for the pane's shell.
//
// An empty invocation — the file set no omp_invocation — is refused by name unless LEGION_OMP_PATH
// is set. The daemon holds no copy of the OMP fork pin, whose one home is the repository's
// .omp-pin.
func ResolveInvocation(invocation string, env func(string) string) (string, error) {
	configured, err := configuredPath(env, "LEGION_OMP_PATH")
	if err != nil {
		return "", err
	}
	if configured != "" {
		resolved, ok := resolveExecutable(configured)
		if !ok {
			return "", fmt.Errorf("LEGION_OMP_PATH is not an executable: %s", configured)
		}
		return shellprefix.Word(resolved), nil
	}
	if invocation == "" {
		return "", errors.New("omp_invocation is not set: set it to 'mise x <tool> -- omp', or set LEGION_OMP_PATH to an absolute executable path")
	}
	match := miseInvocation.FindStringSubmatch(invocation)
	if match == nil {
		return "", errors.New("OMP invocation must be 'mise x <tool> -- omp'. Set LEGION_OMP_PATH to an absolute executable path.")
	}
	mise, err := resolveMise(env)
	if err != nil {
		return "", err
	}
	omp, err := resolveMiseOmp(mise, match[1])
	if err != nil {
		return "", err
	}
	return shellprefix.Word(mise) + " x " + match[1] + " -- " + keepPathHead + " " + shellprefix.Word(omp), nil
}

// resolveMiseOmp resolves the configured tool's own OMP executable before any pane exists. `mise
// x <tool> -- omp` alone is not enough: an operator's PATH can put an OMP wrapper ahead of mise's
// activated bin. The pane still runs through `mise x` for the tool's declared environment, but it
// names this executable directly, so its binary and the boot probe's binary cannot diverge.
func resolveMiseOmp(mise, tool string) (string, error) {
	where := exec.Command(mise, "where", tool)
	where.Env = runtime.WithoutNATSSeeds(os.Environ())
	output, err := where.Output()
	if err != nil {
		return "", fmt.Errorf("mise could not resolve OMP tool %s: %w", tool, err)
	}
	install := strings.TrimSpace(string(output))
	omp, ok := resolveExecutable(filepath.Join(install, "bin", "omp"))
	if !ok {
		return "", fmt.Errorf("mise tool %s has no executable bin/omp under %s", tool, install)
	}
	return omp, nil
}

// configuredPath reads a `LEGION_<TOOL>_PATH` override: unset or empty is no override, and a set
// one must be absolute (configuredPath, environment.ts).
func configuredPath(env func(string) string, variable string) (string, error) {
	configured := env(variable)
	if configured == "" {
		return "", nil
	}
	if !filepath.IsAbs(configured) {
		return "", fmt.Errorf("%s must be an absolute executable path", variable)
	}
	return configured, nil
}

// resolveMise is mise's absolute path: `LEGION_MISE_PATH`, else the first executable `mise` on the
// daemon's PATH (resolveConfiguredOrFound and tmuxDaemonEnvironment, environment.ts).
func resolveMise(env func(string) string) (string, error) {
	missing := errors.New("Missing required daemon tool: mise (set LEGION_MISE_PATH to an absolute executable path)")
	configured, err := configuredPath(env, "LEGION_MISE_PATH")
	if err != nil {
		return "", err
	}
	if configured != "" {
		if resolved, ok := resolveExecutable(configured); ok {
			return resolved, nil
		}
		return "", missing
	}
	for _, dir := range filepath.SplitList(env("PATH")) {
		if dir == "" {
			continue
		}
		if resolved, ok := resolveExecutable(filepath.Join(dir, "mise")); ok {
			return resolved, nil
		}
	}
	return "", missing
}

// resolveExecutable is path's real path when it names an executable file
// (defaultResolveExecutable, environment.ts).
func resolveExecutable(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	return real, true
}

// SystemPromptArgument is the one `--append-system-prompt` word every pane's OMP receives, and
// the operator-launched controller's (`legion controller start`): the path of the file the
// launch's composed system prompt was written to (runtime.WriteSystemPrompt, which says why it is
// a file and why it is written first), quoted for the pane's shell. OMP's flag is last-wins, so
// the whole prompt rides that one file.
func SystemPromptArgument(promptFile string) string {
	return "--append-system-prompt " + shellprefix.Word(promptFile)
}

// WithPrefix prepends the configured launch prefix, each element quoted on its own
// (shellprefix.Command), to the OMP invocation — a fragment already fit for the shell
// (withOmpLaunchPrefix, runtime-tmux.ts). Every pane runs it, and so does the daemon's boot gate,
// which must launch Oh My Pi exactly as a pane will.
func WithPrefix(prefix []string, invocation string) string {
	if len(prefix) == 0 {
		return invocation
	}
	return shellprefix.Command(prefix) + " " + invocation
}
