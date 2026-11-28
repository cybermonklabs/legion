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
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/redact"
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

// PrefixShown is prefix as a line, a refusal or the state's settings show it: its program
// (programShown) and how many arguments follow, never an argument, since a prefix such as
// `env NAME=<token> …` or a proxy wrapper handed a URL with a password carries a credential in its
// arguments. An empty prefix shows as nothing.
func PrefixShown(prefix []string) []string {
	if len(prefix) == 0 {
		return nil
	}
	program := programShown(prefix[0])
	switch rest := len(prefix) - 1; rest {
	case 0:
		return []string{program}
	case 1:
		return []string{program, "(1 argument, not shown)"}
	default:
		return []string{program, fmt.Sprintf("(%d arguments, not shown)", rest)}
	}
}

// WithPrefixShown is WithPrefix as a line or a refusal shows it: PrefixShown's words before the
// invocation, which holds no credential (the loader's omp_invocation, or the binary's path). What
// runs is WithPrefix's; this is only ever printed.
func WithPrefixShown(prefix []string, invocation string) string {
	if len(prefix) == 0 {
		return invocation
	}
	return strings.Join(PrefixShown(prefix), " ") + " " + invocation
}

// ProgramNotShown stands for a launch prefix's first word that has no command's shape.
const ProgramNotShown = "(a first word that is not shown)"

// programShown is word, a launch prefix's first word, as PrefixShown names it: a NAME=value word
// (assignment) as one, and otherwise its base name when that has a command's shape
// (commandShaped), or ProgramNotShown: a credential pasted where the program goes, a path's last
// part included (`/opt/bin/<token>`), is no command's name, and the launch it names cannot run,
// so the boot gate's refusal would print it.
func programShown(word string) string {
	if _, ok := assignment(word); ok {
		return "(a NAME=value word)"
	}
	if base := filepath.Base(word); commandShaped(base) {
		return base
	}
	return ProgramNotShown
}

// commandName is a command's name: letters, digits, ".", "_", "+" and "-", from a letter or a
// digit, 64 characters at most.
var commandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// commandShaped reports whether name has a command's shape: commandName's, holding no run of 8 or
// more hexadecimal digits (a hex key), no run of 10 or more letters and digits that holds a digit (a
// token), and not three or more "-"-separated segments that each hold a digit (a token written in
// segments, `a1b2-c3d4-e5f6`). `env`, `nice`, `secrets`, `python3.12`, `omp-proxy-wrapper` and
// `aarch64-linux-gnu-gcc-12` have it; the launch prefixes this repository's tests, documents and
// deploy examples write run `env`, `secrets` and their paths.
func commandShaped(name string) bool {
	if !commandName.MatchString(name) || hexRun.MatchString(name) {
		return false
	}
	for _, run := range alphanumericRun.FindAllString(name, -1) {
		if strings.ContainsAny(run, "0123456789") {
			return false
		}
	}
	segments := strings.Split(name, "-")
	return len(segments) < 3 || slices.ContainsFunc(segments, func(segment string) bool { return !strings.ContainsAny(segment, "0123456789") })
}

// hexRun is a run of 8 or more hexadecimal digits, and alphanumericRun one of 10 or more letters
// and digits.
var hexRun, alphanumericRun = regexp.MustCompile(`[0-9A-Fa-f]{8,}`), regexp.MustCompile(`[A-Za-z0-9]{10,}`)

// MaskedPrefixWord is the shortest text of the launch prefix PrefixMasked takes out of a launch's
// output: a shorter one (a flag, `--`, a short name) is too common to replace without rewriting
// what Oh My Pi said, and holds no credential worth the name.
const MaskedPrefixWord = 8

// PrefixMasked is text, the output of a launch that ran with prefix, a line at a time by
// redact.Withheld's rule, the rule the `secrets` and private_key_command outputs are printed by:
// every copy of each of the prefix's words, and of the value of each NAME=value word
// (assignment), of MaskedPrefixWord characters or more, is replaced by `<prefix word N>`, N the
// word's place in prefix (0 the program), wherever it stands; and a line that still holds
// redact.HeldRun characters in a row of an argument, or of such a value, compared without case, is
// not printed, its length printed instead. A NAME=value word whole is taken out only as an exact
// copy, so a line that names the variable is kept. The program is taken out as an exact copy and
// by its base name, which a shell's refusal names (`sh: 1: <name>: not found`); a line that shares
// a directory with it (`/usr/local/lib/…` beside `/usr/local/bin/<wrapper>`, a check's JSON naming
// one) is kept. Its base name's runs are held to as an argument's are only when that base name has
// no command's shape (commandShaped): a credential pasted where the program goes, whose cut or
// case-changed copy is then not printed, where a command's name is a word the output may name
// freely. A prefix such as `env NAME=<token>`, a proxy wrapper handed a URL with a password, or a
// credential pasted where the program goes carries a credential in its words, and a launch that
// refuses or fails can echo it, whole, as the value alone, folded or cut, in the output a refusal
// quotes.
func PrefixMasked(text string, prefix []string) string {
	var words []redact.Word
	for place, word := range prefix {
		shown := fmt.Sprintf("<prefix word %d>", place)
		value, assigns := assignment(word)
		switch {
		case place == 0 && !assigns:
			base := filepath.Base(word)
			if len(word) >= MaskedPrefixWord {
				words = append(words, redact.Word{Text: word, Shown: shown, Exact: true})
			}
			if len(base) >= MaskedPrefixWord {
				words = append(words, redact.Word{Text: base, Shown: shown, Exact: commandShaped(base)})
			}
		case len(word) >= MaskedPrefixWord:
			words = append(words, redact.Word{Text: word, Shown: shown, Exact: assigns})
		}
		if assigns && len(value) >= MaskedPrefixWord {
			words = append(words, redact.Word{Text: value, Shown: shown})
		}
	}
	if len(words) == 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if shown, ok := redact.Withheld(line, words); ok {
			lines[i] = shown
		} else {
			lines[i] = fmt.Sprintf("(a line of %d bytes that holds part of the launch prefix, not printed)", len(line))
		}
	}
	return strings.Join(lines, "\n")
}

// variableName is the NAME of a NAME=value word: a shell variable's name.
var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// assignment is the value of word when word sets a variable, as a shell reads a NAME=value word,
// and ok false otherwise: its part before the first "=" is a variable's name, and its value is
// more than "=" signs, which end a base64 token (`<token>==`) rather than set a variable to them.
// Any other word holding "=" (`--key=<token>`, a base64 token) is a word whole, whose runs a line
// may not hold.
func assignment(word string) (value string, ok bool) {
	name, value, found := strings.Cut(word, "=")
	return value, found && variableName.MatchString(name) && strings.Trim(value, "=") != ""
}
