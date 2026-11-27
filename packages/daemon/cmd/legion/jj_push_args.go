package main

import (
	"iter"
	"slices"
	"strings"
)

// This file reads a jj command line as `legion jj` (jj.go) reads it once jj has no alias left to
// expand in it: its subcommand pair, whether it is a push the step runs before, the options every
// command of the step carries, the words that stay on the real push alone, and each revision
// argument (readJJPush). Each line it reads is a line that jj expands no further: one whose command
// word is a built-in jj command, as the role wrote it, or the expansion of the role's line through
// jj's aliases and ui.default-command, which the pod's wrapper (packages/daemon/docker/jj) makes in
// a pod and expandJJ under tmux.

// jjPush is a jj command line as legion jj reads it (readJJPush).
type jjPush struct {
	args []string
	// push is whether the command's subcommand pair is `git push`.
	push bool
	// leave is whether a push runs no step: one that sends nothing (--dry-run, or a word of help or
	// version, jjHelpWord), or one at an earlier operation (--at-op, --at-operation).
	leave bool
	// options are the words of the caller's -R/--repository, --config, --config-file and
	// --config-toml that jj keeps for the push, each with its value, in the order written
	// (readJJPush): what every command of the step carries first, in front of its subcommand, so
	// it reads the repository and the configuration the push reads.
	options []string
	// repository is whether the push names its repository by -R or --repository, which the read
	// of the aliases (jjAliasRead) leaves out: that read loads the working directory's repository.
	repository bool
	// end is the index of the first `--`, which ends the options, as clap reads each word after it
	// as a positional argument, or the number of words where there is none: readJJPush reads no
	// word from it on, and the dry run's own words go in front of it (jjStep.dryRun).
	end int
	// configEnd is the index of the word in front of which the dry run's own --config goes
	// (jjDryRunQuiet): the end of the level of the command line that holds the caller's kept
	// --config (readJJPush), the first word of the subcommand pair for level 0 and its second for
	// level 1, and end for level 2 and where the caller wrote no --config.
	configEnd int
	// pushOnly are the indexes of the words that stay on the real push alone: --quiet, --debug,
	// --no-integrate-operation, --ignore-immutable, --ignore-working-copy, which the dry run's
	// flags hold already (jjDryRunFlags) and which jj 0.38 refuses twice, and --color with its
	// value.
	pushOnly map[int]bool
	// noSnapshot is whether the push takes no snapshot of the working copy: the role's own
	// --ignore-working-copy, before or after the subcommand pair, as jj takes it. Every command of
	// the step reads as such a push does, so the step takes no snapshot before it
	// (jjStep.snapshotDue), and the real push never gets that flag twice, as jjStep.snapshotted
	// says.
	noSnapshot bool
	// quiet is whether the push runs with the role's own --quiet, before or after the subcommand
	// pair, as jj takes it. The step's snapshot carries --quiet then too (jjStep.snapshot), so it
	// prints what the push's own snapshot would print under it: jj's warnings, and no status line
	// such as that of the descendants of @ it rebased. The dry run leaves it out (pushOnly).
	quiet bool
	// revisions are the push's revision arguments, in order.
	revisions []jjRevision
}

// revision is revision argument i's word, split where the argument starts: prefix, what stands in
// front of it (the name of --named, or the option's own spelling where the value is attached to
// it), and argument, the revset the role wrote.
func (p jjPush) revision(i int) (prefix, argument string) {
	r := p.revisions[i]
	return p.args[r.index][:r.offset], p.args[r.index][r.offset:]
}

// revisionWords is each word that revision argument i takes of the command where words hold it:
// each of words, and, where the argument's value follows its option (`-r <ids>`), the word of that
// option again before each of words after the first. A value attached to its option (`-r<ids>`,
// `--revisions=<ids>`) has the option in each of words already. jj takes -c and -r more than once
// and sends the union of what they name; --named names one commit, so its argument is never in
// more than one word.
func (p jjPush) revisionWords(i int, words []string) iter.Seq[string] {
	r := p.revisions[i]
	return func(yield func(string) bool) {
		for k, word := range words {
			if k > 0 && r.offset == 0 && !yield(p.args[r.index-1]) {
				return
			}
			if !yield(word) {
				return
			}
		}
	}
}

// jjRevision is one revision argument of a push: the value of -c/--change or
// -r/--revision/--revisions, or the revision of --named <name>=<revision>, which is the word at
// index from offset on. jj itself resolves it (jjStep.readRevisions).
type jjRevision struct{ index, offset int }

// jjGlobalValues are jj's global options whose value may be the next word, which readJJPush skips
// to find the subcommand pair: those that jjCommandWord skips (jjWordValues), and --config-toml.
var jjGlobalValues = slices.Concat(jjWordValues, []string{"--config-toml"})

// jjPushValues are jj's global options and `jj git push`'s own whose value may be the next word.
var jjPushValues = slices.Concat(jjGlobalValues, []string{"--remote", "-b", "--bookmark", "-t", "--tag", "-r", "--revision", "--revisions", "-c", "--change", "--named", "-o", "--option"})

// jjOccurrence is one -R/--repository, --config, --config-file or --config-toml of a command line
// (readJJPush): its option's name as written, the level of the command line it stands at, 0 in
// front of the subcommand pair, 1 between its two words and 2 after them, and its words, from
// first to last.
type jjOccurrence struct {
	name               string
	level, first, last int
}

// readJJPush reads args, a jj command line that jj expands no further. It finds the subcommand
// pair: the first two words that are no option and no option's value, jj's global options being
// the ones that take one. Past the pair it reads `git push`'s options too, and each revision
// argument (jjRevision). An option takes its value as the next word, after `=` (`--change=X`,
// `-c=X`), or attached to a short name (`-cX`), as clap reads each. It reads no word from the
// first `--` on (end), each of which clap reads as a positional argument.
//
// It records each -R/--repository, --config, --config-file and --config-toml with its level
// (jjOccurrence), and keeps in options those jj keeps, in the order written: every
// -R/--repository, the last of which names the repository, and, of each of --config,
// --config-file and --config-toml apart, those at the deepest level that holds one. clap keeps
// the values of each of these options only from the deepest level of the command line that holds
// any, and jj layers what it keeps in the order written: measured with jj 0.45.1-sami and 0.38.0,
// `jj --config 'templates.git_push_bookmark="\"zz-\" ++ change_id.short()"' git push -c @-
// --dry-run` makes the bookmark `zz-<change>`, and the same command with `--config
// ui.quiet=false` after `git push` makes `push-<change>`, as jj drops, unread, the --config in
// front of the pair. The end of the level that holds the kept --config is configEnd.
func readJJPush(args []string) jjPush {
	p := jjPush{args: args, pushOnly: map[int]bool{}, end: len(args)}
	var pair []string
	var pairAt [2]int
	var occurrences []jjOccurrence
	configLevel := -1
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			p.end = i
			break
		}
		values := jjGlobalValues
		if len(pair) == 2 {
			values = jjPushValues
		}
		name, offset, option := jjOption(arg, values)
		if !option {
			switch {
			case arg == "--dry-run" || jjHelpWord(arg):
				p.leave = true
			case arg == "--debug" || arg == "--no-integrate-operation" || arg == "--ignore-immutable":
				p.pushOnly[i] = true
			case arg == "--quiet":
				p.pushOnly[i], p.quiet = true, true
			case arg == "--ignore-working-copy":
				p.pushOnly[i], p.noSnapshot = true, true
			default:
				if !strings.HasPrefix(arg, "-") && len(pair) < 2 {
					pairAt[len(pair)] = i
					pair = append(pair, arg)
				}
			}
			continue
		}
		first, at := i, i
		if offset < 0 {
			if i+1 == len(args) {
				break
			}
			i++
			at, offset = i, 0
		}
		switch name {
		case "-R", "--repository", "--config", "--config-file", "--config-toml":
			occurrences = append(occurrences, jjOccurrence{name: name, level: len(pair), first: first, last: at})
			switch name {
			case "-R", "--repository":
				p.repository = true
			case "--config":
				configLevel = len(pair)
			}
		case "--at-operation", "--at-op":
			p.leave = true
		case "--color":
			for j := first; j <= at; j++ {
				p.pushOnly[j] = true
			}
		case "-c", "--change", "-r", "--revision", "--revisions":
			p.revisions = append(p.revisions, jjRevision{index: at, offset: offset})
		case "--named":
			if eq := strings.IndexByte(args[at][offset:], '='); eq >= 0 {
				p.revisions = append(p.revisions, jjRevision{index: at, offset: offset + eq + 1})
			}
		}
	}
	// jj drops an occurrence of --config, --config-file or --config-toml where a later one of the
	// same option stands at a deeper level.
	for k, o := range occurrences {
		deeper := func(later jjOccurrence) bool { return later.name == o.name && later.level > o.level }
		if o.name == "-R" || o.name == "--repository" || !slices.ContainsFunc(occurrences[k+1:], deeper) {
			p.options = append(p.options, args[o.first:o.last+1]...)
		}
	}
	p.configEnd = p.end
	if configLevel == 0 || configLevel == 1 {
		p.configEnd = pairAt[configLevel]
	}
	p.push = len(pair) == 2 && pair[0] == "git" && pair[1] == "push"
	return p
}

// jjOption reads arg as one of the options among values, by its name: the name, and the offset
// in arg of the value it holds itself (`--name=value`, `-Xvalue`, `-X=value`), or -1 when its
// value is the next word.
func jjOption(arg string, values []string) (name string, offset int, ok bool) {
	for _, value := range values {
		switch {
		case arg == value:
			return value, -1, true
		case strings.HasPrefix(value, "--"):
			if strings.HasPrefix(arg, value+"=") {
				return value, len(value) + 1, true
			}
		case strings.HasPrefix(arg, value):
			if arg[len(value)] == '=' {
				return value, len(value) + 1, true
			}
			return value, len(value), true
		}
	}
	return "", 0, false
}

// steps is whether the command is a push the step runs before.
func (p jjPush) steps() bool { return p.push && !p.leave }
