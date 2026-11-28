package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// This file expands a jj command line as jj expands it, for the tmux form of `legion jj` (runJJ):
// the command word as clap finds it (jjCommandWord), jj's built-in commands (jjBuiltins), the one
// read of every alias and of ui.default-command (readJJAliases), and the expansion (expandJJ). The
// --real form expands nothing: the pod's wrapper (packages/daemon/docker/jj) expands the line
// itself before it hands legion jj a push.
//
// The command word and the built-in names are a port of the pod's wrapper (command_word and its
// builtins= line). The expansion follows jj 0.45's resolve_aliases and expand_args, which jj
// 0.46.0 keeps (the worker image pins jj 0.46.0-sami, worker.Dockerfile), where the pod's wrapper
// does not: an alias of options alone used twice on one line (`jj q q …`), which the pod's wrapper
// refuses, expands, and a name is a cycle only where jj finds one (resolveAliases); there is no
// bound of 32 aliases; any word of help or version with no command word, a cluster such as `-hV`
// among them, runs as written (jjHelpWord); a name jj warns is a built-in command is never
// expanded (jjOverridesBuiltin); and where the pod's wrapper refuses a line, legion jj prints one
// line and runs it as written (jjUnexpandable), since it refuses nothing. As jj and the pod's
// wrapper both do, the expansion puts ui.default-command in front of a line that an alias leaves
// with no command word (expandJJ). docs/deployment.md's paragraph on the pane's jj states where
// the expansion and jj still differ, its limits.

// jjBuiltins are the commands jj has built in, by clap's names: each command's name and every
// other name clap gives it, hidden ones included. They are the 51 names of the pod's wrapper's
// builtins= line (packages/daemon/docker/jj), the names jj 0.45.1-sami warns of where an alias
// has one ("Cannot define an alias that overrides the built-in command"); jj 0.38.0 has each of
// them but arrange and converge. jj ignores an alias of such a name, so a command word among them
// is never expanded, and the expansion needs no read of the aliases; a push among them that the
// step runs on gets the step's read of its configuration (jjStep.readConfiguration).
var jjBuiltins = []string{
	"abandon", "absorb", "arrange", "bisect", "bookmark", "commit", "config", "converge", "debug", "describe",
	"diff", "diffedit", "duplicate", "edit", "evolog", "evolution-log", "file", "fix", "gerrit", "git",
	"help", "interdiff", "log", "metaedit", "new", "next", "obslog", "op", "operation", "parallelize",
	"prev", "rebase", "redo", "resolve", "restore", "revert", "root", "run", "show", "sign",
	"simplify-parents", "sparse", "split", "squash", "status", "tag", "undo", "unsign", "util", "version",
	"workspace",
}

// jjWordValues are jj's global options whose value may be the next word, as clap reads them in
// front of the command word: `jj --help`'s Global Options, the same in jj 0.38.0 and 0.45.1-sami
// (docker/jj's command_word). Each takes its value after `=` too, and -R attached to it
// (jjOption).
var jjWordValues = []string{"-R", "--repository", "--at-operation", "--at-op", "--color", "--config", "--config-file"}

// jjWordFlags are jj's global flags, which clap reads in front of the command word.
var jjWordFlags = []string{"--ignore-working-copy", "--no-integrate-operation", "--ignore-immutable", "--debug", "--quiet", "--no-pager"}

// jjCommandWord is the index in args of jj's command word as clap finds it, the first word that is
// no global option and no option's value, or -1 for none, as the pod's wrapper finds it
// (command_word). A global option's value is the next word or is in the same word (jjOption of
// jjWordValues), a flag stands alone (jjWordFlags), and clap reads a word that starts with -h or
// -V as help or version (jjHelpWord), and `--` as no command word. An empty word and a lone `-`
// are command words. Any other option in front of the command word is one clap does not know, and
// clap stops there: jj then reads no command word, and puts ui.default-command in front of the
// line, so the answer is -1.
func jjCommandWord(args []string) int {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if _, offset, ok := jjOption(arg, jjWordValues); ok {
			if offset < 0 {
				i++
			}
			continue
		}
		switch {
		case slices.Contains(jjWordFlags, arg), arg == "--", jjHelpWord(arg):
		case len(arg) > 1 && arg[0] == '-':
			return -1
		default:
			return i
		}
	}
	return -1
}

// jjHelpWord is whether arg is a word clap reads as help or version: --help, --version, or a word
// of one dash that starts with -h or -V, a cluster such as `-hV` among them. jj puts no
// ui.default-command in front of a line that holds one and has no command word: measured with jj
// 0.38.0 and 0.45.1-sami, `jj -h`, `jj --help`, `jj -V`, `jj --version` and `jj -hV` print and push
// nothing with a push as the default command.
func jjHelpWord(arg string) bool {
	return arg == "--help" || arg == "--version" || strings.HasPrefix(arg, "-h") || strings.HasPrefix(arg, "-V")
}

// jjCompleting is whether the environment's COMPLETE names a shell: jj then prints shell
// completions and runs no command (clap's CompleteEnv), and a read of its configuration would
// print completions too, as the pod's wrapper notes. Empty or `0` is no completion.
func jjCompleting() bool {
	value := os.Getenv("COMPLETE")
	return value != "" && value != "0"
}

// jjAliasRead is the one read of the configuration that the expansion makes: each alias and
// ui.default-command, a line each, its name and its value in JSON (json()), which jj 0.38.0,
// 0.45.1-sami and 0.46.0 print alike (testdata/jj-config-list). jj reads aliases and
// ui.default-command from its defaults, the user's files and the configuration of the repository
// the working directory is in, and from neither the repository -R names nor a --config or
// --config-file: measured with jj 0.38.0 and 0.45.1-sami, `jj -R <repo> q` is `unrecognized
// subcommand` where only <repo> defines q, and `jj --config aliases.q=… q` warns and refuses q. So
// the read runs in the working directory with none of the role's options, and with
// --include-defaults, since jj's defaults hold aliases of their own (b, ci, desc, st). The read is
// itself a `jj config` command, so it sees an alias that a [[--scope]] with --when.commands =
// ["config"] defines, which jj runs for no other command: such an alias is expanded as the pod's
// wrapper expands it, and a push it names runs with the step where jj alone refuses it, a limit of
// the expansion.
var jjAliasRead = []string{"--ignore-working-copy", "--color=never", "config", "list", "--include-defaults", "-T",
	`if(name.starts_with("aliases.") || name == "ui.default-command", json(name) ++ " " ++ json(value) ++ "\n")`}

// jjOverridesBuiltin is jj's warning of an alias that has the name of a built-in command, which
// every jj command prints on standard error, the read included; its group is the name. jj 0.45.1-sami
// ends the line with a full stop, and jj 0.38.0 does not.
var jjOverridesBuiltin = regexp.MustCompile(`Cannot define an alias that overrides the built-in command '([^']*)'`)

// jjAliases is what the read (jjAliasRead) gives the expansion: values, each value in JSON by its
// name as jj lists it (`aliases.<name>`, `aliases.<name>.definition`, `ui.default-command`);
// builtins, each name that jj warns is a built-in command (jjOverridesBuiltin), whatever
// jjBuiltins holds; and quoted, whether jj lists an alias by a quoted name (`aliases."a.b"`), which
// only a command word that is no bare name (jjBareName) can be.
type jjAliases struct {
	values   map[string]json.RawMessage
	builtins map[string]bool
	quoted   bool
}

// jjUnexpandable is a command line that legion jj cannot expand (expandJJ): what of it, and why.
// runJJ prints its one line, and the command runs as the role wrote it.
type jjUnexpandable struct{ what, why string }

func (u jjUnexpandable) Error() string { return "could not expand " + u.what + ": " + u.why }

// readJJAliases runs the read (jjAliasRead) as each command of the step runs (runChild): next,
// in env, in the working directory, as a child in this process's group, prints on stderr
// jjOnceOnly of what it printed on standard error, and reads what it printed (parseJJAliases).
// jj's error is named by the line of a read that fails (jjUnexpandable), which runJJ prints next.
// A read that cannot start prints nothing of its own, and is jjUnexpandable too, with its start
// error.
func readJJAliases(next string, env []string, stderr io.Writer) (jjAliases, error) {
	stdout, printed, code, err := runChild(next, env, jjAliasRead)
	if err != nil {
		return jjAliases{}, jjUnexpandable{"the command", "jj config list could not start: " + err.Error()}
	}
	fmt.Fprint(stderr, jjOnceOnly(printed))
	return parseJJAliases(stdout, printed, code)
}

// parseJJAliases reads what the read (jjAliasRead) printed on standard output and standard error,
// the whole of each, and its exit status, code. A read that exited non-zero, a configuration that
// does not parse among them, or whose standard output holds what is no name and value in JSON, is
// jjUnexpandable.
func parseJJAliases(stdout, stderr string, code int) (jjAliases, error) {
	if code != 0 {
		return jjAliases{}, jjUnexpandable{"the command", "jj config list failed: " + jjErrorLine(stderr, "jj config list", code)}
	}
	a := jjAliases{values: map[string]json.RawMessage{}, builtins: map[string]bool{}}
	for _, warned := range jjOverridesBuiltin.FindAllStringSubmatch(stderr, -1) {
		a.builtins[warned[1]] = true
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	for {
		var name string
		var value json.RawMessage
		err := decoder.Decode(&name)
		if errors.Is(err, io.EOF) {
			return a, nil
		}
		if err == nil {
			err = decoder.Decode(&value)
		}
		if err != nil {
			return jjAliases{}, jjUnexpandable{"the command", "jj config list printed what is no name and value in JSON: " + err.Error()}
		}
		a.values[name] = value
		if rest, ok := strings.CutPrefix(name, "aliases."); ok && (strings.HasPrefix(rest, `"`) || strings.HasPrefix(rest, "'")) {
			a.quoted = true
		}
	}
}

// jjBareName is whether word is a TOML bare key, the only name jj lists an alias by unquoted.
func jjBareName(word string) bool {
	return word != "" && strings.Trim(word, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") == ""
}

// builtin is whether jj takes word for a built-in command: one of jjBuiltins, or a name jj warned
// of in the read (jjOverridesBuiltin).
func (a jjAliases) builtin(word string) bool {
	return slices.Contains(jjBuiltins, word) || a.builtins[word]
}

// defaultCommand is ui.default-command as jj reads it: a list of strings, or one string, which is
// one command word, or `log` where nothing sets it.
func (a jjAliases) defaultCommand() ([]string, error) {
	value, set := a.values["ui.default-command"]
	if !set {
		return []string{"log"}, nil
	}
	var word string
	if json.Unmarshal(value, &word) == nil {
		return []string{word}, nil
	}
	var words []string
	if json.Unmarshal(value, &words) != nil {
		return nil, jjUnexpandable{"ui.default-command", fmt.Sprintf("it is %s, which is neither a string nor a list of strings", value)}
	}
	return words, nil
}

// alias is the definition of the alias that word names, and whether one does. An alias is a list
// of strings, `aliases.<word> = […]`, or a table whose definition is one, which jj 0.45.1-sami and
// 0.46.0 run and the pod's wrapper expands; a table with no definition names no alias. A value
// that is no list of strings is jjUnexpandable, as is a table with `enabled = false`, which jj
// 0.45.1-sami runs and jj 0.46.0 refuses. A word that is no bare name (jjBareName) names no alias
// unless jj lists one by a quoted name, which legion jj does not read, and which is
// jjUnexpandable.
//
// Two limits of the expansion are here. A table with `enabled = false` runs as the role wrote it,
// on both forms, since the pod's wrapper leaves it to jj too. Measured with `jj p`, where the
// repository's aliases.p is such a table with the definition ["git", "push", "-b", "b"]: jj
// 0.45.1-sami pushes b, so its push goes with no step; jj 0.46.0, which the worker image pins as
// 0.46.0-sami, refuses it (`unrecognized subcommand 'p'`, exit 2) and pushes nothing; and jj
// 0.38.0 refuses it, as it refuses every table-form alias (`Invalid type or value for
// aliases.<word>`, exit 1). The expansion takes any other table as jj 0.45.1-sami and 0.46.0 do,
// so under jj 0.38.0 its push runs with the step where jj alone refuses it.
func (a jjAliases) alias(word string) ([]string, bool, error) {
	if !jjBareName(word) {
		if a.quoted {
			return nil, false, jjUnexpandable{fmt.Sprintf("the command word %q", word), "it is no bare name, and the configuration names an alias by a quoted name, which legion jj does not read"}
		}
		return nil, false, nil
	}
	name := "aliases." + word
	value, plain := a.values[name]
	if !plain {
		var defined bool
		if value, defined = a.values[name+".definition"]; !defined {
			return nil, false, nil
		}
		if string(a.values[name+".enabled"]) == "false" {
			return nil, false, jjUnexpandable{name, "its enabled is false, which jj 0.45.1-sami ignores and jj 0.46.0 obeys"}
		}
		name += ".definition"
	}
	var words []string
	if json.Unmarshal(value, &words) != nil {
		return nil, false, jjUnexpandable{name, fmt.Sprintf("it is %s, which is no list of strings", value)}
	}
	return words, true, nil
}

// jjExpansion is one alias on the chain of resolveAliases: its name, and the positions of the line
// its words fill, from start to before end.
type jjExpansion struct {
	name       string
	start, end int
}

// resolveAliases is line with the aliases jj expands in it expanded, by the rule of jj 0.45's
// resolve_aliases (cli/src/cli_util.rs at v0.45.0, which 0.46.0 keeps): while the command word
// names an alias, its words go in place of the command word. The chain holds the aliases whose
// words the command word came from, each with the positions its words fill. Before an alias
// expands, each alias at the top of the chain whose positions do not hold the command word leaves
// the chain, and the alias below it then fills its own positions and those, less the one of the
// name that went. Where the command word is inside the top's positions, an alias of its name on
// the chain is a cycle (`Recursive alias definition involving`), which is jjUnexpandable. So an
// alias of options alone, whose command word comes from outside its own words, leaves the chain,
// and `jj q q version` with q = ["--quiet"] expands, as jj 0.45.1-sami and 0.46.0 run it; jj
// 0.38.0 refuses any name that comes twice. It ends at a built-in command word, at a word that no
// alias names, and at a line with no command word, and has no bound of its own: it ends where jj's
// does.
func (a jjAliases) resolveAliases(line []string) ([]string, error) {
	var chain []jjExpansion
	for {
		at := jjCommandWord(line)
		if at < 0 || a.builtin(line[at]) {
			return line, nil
		}
		word := line[at]
		words, ok, err := a.alias(word)
		if err != nil {
			return nil, err
		}
		if !ok {
			return line, nil
		}
		for len(chain) > 0 {
			top := chain[len(chain)-1]
			if top.start <= at && at < top.end {
				if slices.ContainsFunc(chain, func(e jjExpansion) bool { return e.name == word }) {
					return nil, jjUnexpandable{"aliases." + word, "jj would expand it again inside its own expansion, a cycle jj refuses"}
				}
				break
			}
			chain = chain[:len(chain)-1]
			if below := len(chain) - 1; below >= 0 {
				chain[below].end += top.end - top.start - 1
			}
		}
		chain = append(chain, jjExpansion{name: word, start: at, end: at + len(words)})
		line = slices.Concat(line[:at], words, line[at+1:])
	}
}

// expandJJ is args as jj expands them, where the expansion ends at a built-in command word, which
// readJJPush then reads as it reads a line the role wrote; it is nil where jj runs no built-in
// command, or where legion jj cannot expand args (jjUnexpandable), and the command then runs with
// the role's own words. read is whether it made the read of the aliases. A line whose command word
// is a built-in name (jjBuiltins) is args, and a line with no command word that holds a word of
// help or version (jjHelpWord) is nil, each with no read of the aliases. Every other line costs
// one read through next in env (readJJAliases), which prints on stderr jjOnceOnly of what jj
// prints on standard error.
//
// The expansion is jj 0.45's (expand_args): the aliases of the line (resolveAliases); then, where
// that leaves no command word, as it does a bare `jj`, a line that starts with an option clap does
// not know, and an alias of options alone, ui.default-command in front of the line, unless the
// line holds a word of help or version, and the aliases of that line, afresh. Measured with jj
// 0.45.1-sami and 0.46.0: `jj q` with q = ["--quiet"] and ui.default-command = ["git", "push", "-b",
// "b"] pushes b, which jj 0.38.0, which puts the default command in front before it expands any
// alias, refuses (`'jj' requires a subcommand`); the pod's wrapper pushes it, as legion jj does, a
// limit of the expansion under jj 0.38.0. A word that no alias names, and a line that the
// expansion leaves with no command word, end it with no built-in command: jj refuses the word, or
// runs no push.
func expandJJ(args []string, next string, env []string, stderr io.Writer) (line []string, read bool, err error) {
	at := jjCommandWord(args)
	switch {
	case at < 0 && slices.ContainsFunc(args, jjHelpWord):
		return nil, false, nil
	case at >= 0 && slices.Contains(jjBuiltins, args[at]):
		return args, false, nil
	}
	aliases, err := readJJAliases(next, env, stderr)
	if err != nil {
		return nil, true, err
	}
	if line, err = aliases.resolveAliases(args); err != nil {
		return nil, true, err
	}
	if jjCommandWord(line) < 0 {
		if slices.ContainsFunc(line, jjHelpWord) {
			return nil, true, nil
		}
		words, err := aliases.defaultCommand()
		if err != nil {
			return nil, true, err
		}
		if line, err = aliases.resolveAliases(slices.Concat(words, line)); err != nil {
			return nil, true, err
		}
	}
	if at := jjCommandWord(line); at < 0 || !aliases.builtin(line[at]) {
		return nil, true, nil
	}
	return line, true, nil
}
