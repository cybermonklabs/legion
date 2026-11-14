package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// This file holds the reads that the helper git answers for a commit: the git 2.47.3 commands that
// read what git commit reads itself (config, rev-parse, log, var, stripspace, interpret-trailers).

// gitReader runs the helper git for the reads of a commit: with the caller's global options, with
// env as its environment (the process's own when env is nil), and with standard input from
// /dev/null unless the read hands it text. Its standard error goes nowhere, since a read prints
// nothing of its own.
type gitReader struct {
	path    string
	globals []string
	env     []string
}

// command is `<helper> <args…>` under the reader's environment.
func (g gitReader) command(args ...string) *exec.Cmd {
	cmd := exec.Command(g.path, args...)
	cmd.Env = g.env
	return cmd
}

// output runs `<helper> <globals> <args…>` with stdin as its standard input and answers what it
// printed.
func (g gitReader) output(stdin string, args ...string) (string, error) {
	cmd := g.command(slices.Concat(g.globals, args)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	return string(out), err
}

// atLeast is whether `<helper> version` names major.minor or later.
func (g gitReader) atLeast(major, minor int) bool {
	out, err := g.command("version").Output()
	if err != nil {
		return false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return false
	}
	parts := strings.SplitN(fields[2], ".", 3)
	if len(parts) < 2 {
		return false
	}
	got, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	gotMinor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return got > major || got == major && gotMinor >= minor
}

// config is the value that `git config --get name` gives, with the options in typ (a --type)
// before --get. found is false when name is unset, which git answers with exit 1.
func (g gitReader) config(name string, typ ...string) (value string, found bool, err error) {
	out, err := g.output("", slices.Concat([]string{"config"}, typ, []string{"--get", name})...)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSuffix(out, "\n"), true, nil
}

// commitText is a commit's message as git formats it: %s, its subject, the lines of the first
// paragraph joined by a space, %b, the body after that paragraph, and %B, the whole message.
type commitText struct {
	subject, body, message string
}

// commitMessage is %s, %b and %B of the commit name names, read in one `git log -1`, as
// prepare_to_commit and prepare_amend_commit format them: `squash! %s`, `fixup! %s`, the body of
// an amend! commit, and the whole message of -C, -c and --amend. NUL bytes part the three, which
// git keeps out of every commit message. `--end-of-options` and `--` keep a name that looks like
// an option or a path a revision, and a name git commit cannot look up (a range, a negative
// revision) prints no commit, which is a failure.
func (g gitReader) commitMessage(name string) (commitText, error) {
	out, err := g.output("", "log", "-1", "--no-show-signature", "--format=%s%x00%b%x00%B", "--end-of-options", name, "--")
	if err != nil {
		return commitText{}, err
	}
	parts := strings.Split(out, "\x00")
	message, ok := strings.CutSuffix(parts[len(parts)-1], "\n")
	if len(parts) != 3 || !ok {
		return commitText{}, fmt.Errorf("git log -1 %s printed no commit", name)
	}
	return commitText{subject: parts[0], body: parts[1], message: message}, nil
}

// stripComments is text cleaned by `git stripspace --strip-comments`, which runs
// strbuf_stripspace with the comment string as the strip cleanup does, so core.commentChar and
// core.commentString apply.
func (g gitReader) stripComments(text string) (string, error) {
	return g.output(text, "stripspace", "--strip-comments")
}

// interpretTrailers is text after git's trailer step with values: `git interpret-trailers
// --no-divider --trailer=<value>…`, as amend_file_with_trailers runs it over COMMIT_EDITMSG with
// the --trailer options git commit kept (parse_opt_passthru_argv writes each as
// `--trailer=<value>`).
func (g gitReader) interpretTrailers(text string, values []string) (string, error) {
	args := []string{"interpret-trailers", "--no-divider"}
	for _, value := range values {
		args = append(args, "--trailer="+value)
	}
	return g.output(text, args...)
}

// trailers is each trailer of message's trailer block, one `<key>: <value>` each, as
// `git interpret-trailers --parse --no-divider` prints them, and none when it has no block.
func (g gitReader) trailers(message string) ([]string, error) {
	out, err := g.output(message, "interpret-trailers", "--parse", "--no-divider")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n"), nil
}

// committer is the committer as -s names it (fmt_name): `git var GIT_COMMITTER_IDENT` without the
// date and the time zone at its end.
func (g gitReader) committer() (string, error) {
	out, err := g.output("", "var", "GIT_COMMITTER_IDENT")
	if err != nil {
		return "", err
	}
	ident := strings.TrimSuffix(out, "\n")
	for range 2 {
		end := strings.LastIndexByte(ident, ' ')
		if end < 0 {
			return "", fmt.Errorf("git var printed no committer: %q", out)
		}
		ident = ident[:end]
	}
	return ident, nil
}

// commitConfig is the configuration git commit reads that the wrapper needs, other than
// commit.template, which needs a path's expansion.
type commitConfig struct {
	// cleanup is commit.cleanup.
	cleanup commitString
	// includeStatus is commit.status: whether the editor's text gets the status lines.
	includeStatus bool
	// comment is the comment string, core.commentChar or core.commentString, the last one set,
	// and `#` when neither is. autoComment is whether that last one is auto, and then comment is
	// `#` until git picks a character from the message.
	comment     string
	autoComment bool
	// trailerCommand is whether some trailer.<key>.command or trailer.<key>.cmd is set.
	trailerCommand bool
}

// commitConfig reads commitConfig with one `git config -z --get-regexp`, whose output is each
// entry, in the order git reads them, as `<key>`, a line feed and the value when there is one, and
// a NUL. ok is false when the read fails, and when a value is one git dies on (git_commit_config,
// git_default_core_config): a commit.cleanup or comment string with no value, an empty comment
// string or one with a line feed, and a commit.status that is no boolean.
func (g gitReader) commitConfig() (cfg commitConfig, ok bool) {
	cfg = commitConfig{includeStatus: true, comment: "#"}
	out, err := g.output("", "config", "-z", "--get-regexp", `^(commit\.(cleanup|status)|core\.comment(char|string)|trailer\..+\.(command|cmd))$`)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return cfg, true
	}
	if err != nil {
		return cfg, false
	}
	for _, entry := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		key, value, hasValue := strings.Cut(entry, "\n")
		switch key {
		case "commit.cleanup":
			if !hasValue {
				return cfg, false
			}
			cfg.cleanup = commitString{true, value}
		case "commit.status":
			if cfg.includeStatus, ok = gitBool(value, hasValue); !ok {
				return cfg, false
			}
		case "core.commentchar", "core.commentstring":
			switch {
			case !hasValue || value == "" || strings.Contains(value, "\n"):
				return cfg, false
			case strings.EqualFold(value, "auto"):
				cfg.comment, cfg.autoComment = "#", true
			default:
				cfg.comment, cfg.autoComment = value, false
			}
		default:
			cfg.trailerCommand = true
		}
	}
	return cfg, true
}

// gitBool is git_config_bool for a value without a unit: no value is true, an empty one false,
// yes, on, true, no, off and false in any case, and else an integer that is true when it is not
// zero. ok is false for anything else, which git dies on.
func gitBool(value string, hasValue bool) (b, ok bool) {
	switch {
	case !hasValue:
		return true, true
	case value == "":
		return false, true
	case strings.EqualFold(value, "true") || strings.EqualFold(value, "yes") || strings.EqualFold(value, "on"):
		return true, true
	case strings.EqualFold(value, "false") || strings.EqualFold(value, "no") || strings.EqualFold(value, "off"):
		return false, true
	}
	n, err := strconv.Atoi(value)
	return n != 0, err == nil
}

// gitPaths is where git commit reads its files. top is the top of the work tree, the directory
// git commit runs in (setup_work_tree). prefix is the directory under top where git started, with
// its slash, or "" outside the work tree. merge and squash are MERGE_MSG and SQUASH_MSG.
type gitPaths struct {
	top, prefix, merge, squash string
}

// paths reads gitPaths with one `git rev-parse`. It fails outside a work tree, where git commit
// fails too.
func (g gitReader) paths() (gitPaths, error) {
	out, err := g.output("", "rev-parse", "--path-format=absolute", "--show-toplevel", "--show-prefix", "--git-path", "MERGE_MSG", "--git-path", "SQUASH_MSG")
	if err != nil {
		return gitPaths{}, err
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 4 {
		return gitPaths{}, fmt.Errorf("git rev-parse printed %d lines, not 4", len(lines))
	}
	return gitPaths{top: lines[0], prefix: lines[1], merge: lines[2], squash: lines[3]}, nil
}

// cliPath is where git commit reads a file that -F or -t names. fix_filename puts the prefix
// before a relative path, and git reads it from top. A lone `-` gets no prefix
// (prefix_filename_except_for_dash).
func (p gitPaths) cliPath(file string) string {
	switch {
	case filepath.IsAbs(file):
		return file
	case file == "-":
		return p.top + "/-"
	}
	return p.top + "/" + p.prefix + file
}

// configPath is where git commit reads the file commit.template names: a relative path is read
// from top.
func (p gitPaths) configPath(file string) string {
	if filepath.IsAbs(file) {
		return file
	}
	return p.top + "/" + file
}
