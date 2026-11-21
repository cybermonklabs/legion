package main

import (
	"iter"
	"slices"
	"strings"
)

// This file reads a jj command line as `legion jj` (jj.go) reads it: its subcommand pair, whether
// it is a push the step runs before, the options every command of the step carries, the words
// that stay on the real push alone, and each revision argument (readJJPush).

// jjPush is a jj command line as legion jj reads it (readJJPush).
type jjPush struct {
	args []string
	// push is whether the command's subcommand pair is `git push`.
	push bool
	// leave is whether a push runs no step: one that sends nothing (--dry-run, -h, --help), or
	// one at an earlier operation (--at-op, --at-operation).
	leave bool
	// options are the words of the caller's -R/--repository, --config, --config-file and
	// --config-toml, each with its value, in the order written: what every command of the step
	// carries first, so it reads the repository and the configuration the push reads.
	options []string
	// pushOnly are the indexes of the words that stay on the real push alone: --quiet, --debug,
	// --no-integrate-operation, --ignore-immutable, and --color with its value.
	pushOnly map[int]bool
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

// jjGlobalValues are jj's global options whose value may be the next word: the list of the pod's
// wrapper (packages/daemon/docker/jj), which finds the subcommand pair past them.
var jjGlobalValues = []string{"-R", "--repository", "--at-operation", "--at-op", "--color", "--config", "--config-file", "--config-toml"}

// jjPushValues are jj's global options and `jj git push`'s own whose value may be the next word.
var jjPushValues = slices.Concat(jjGlobalValues, []string{"--remote", "-b", "--bookmark", "-t", "--tag", "-r", "--revision", "--revisions", "-c", "--change", "--named", "-o", "--option"})

// readJJPush reads args, a jj command line. It finds the subcommand pair as the pod's wrapper
// (packages/daemon/docker/jj) finds it: the first two words that are no option and no option's
// value, jj's global options being the ones that take one. Past the pair it reads `git push`'s
// options too, and each revision argument (jjRevision). An option takes its value as the next
// word, after `=` (`--change=X`, `-c=X`), or attached to a short name (`-cX`), as clap reads each.
func readJJPush(args []string) jjPush {
	p := jjPush{args: args, pushOnly: map[int]bool{}}
	var pair []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		values := jjGlobalValues
		if len(pair) == 2 {
			values = jjPushValues
		}
		name, offset, option := jjOption(arg, values)
		if !option {
			switch arg {
			case "--dry-run", "-h", "--help":
				p.leave = true
			case "--quiet", "--debug", "--no-integrate-operation", "--ignore-immutable":
				p.pushOnly[i] = true
			default:
				if !strings.HasPrefix(arg, "-") && len(pair) < 2 {
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
			p.options = append(p.options, args[first:at+1]...)
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
