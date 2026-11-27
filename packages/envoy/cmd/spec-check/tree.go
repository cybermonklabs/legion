package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// runTree is "spec-check tree --dir <dir> [--allow-empty] [--retired <file>]": it judges one
// directory and answers 0 for a valid or empty tree, 1 for a refused one, and 2 for a directory or
// a file it cannot read or arguments it does not take.
func runTree(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tree", flag.ContinueOnError)
	dirFlag := flags.String("dir", "", "the directory of spec files")
	allowEmpty := flags.Bool("allow-empty", false, "read a directory that does not exist or holds no spec file as the empty tree")
	retiredFlag := flags.String("retired", "", "a file of the ids the approved spec removed, one id a line")
	if code, ok := parse(flags, args, stderr, "dir"); !ok {
		return code
	}
	dir, ok := absolute(stderr, *dirFlag)
	if !ok {
		return 2
	}
	var o specgraph.Options
	if *retiredFlag != "" {
		path, ok := absolute(stderr, *retiredFlag)
		if !ok {
			return 2
		}
		retired, err := readRetired(path)
		if err != nil {
			say(stderr, "%s: cannot be read: %s", path, err)
			return 2
		}
		o.Retired = retired
	}

	j, err := judge(stderr, dir, o)
	what, empty := noSpecFile(j, err)
	var refused *notes.SizeRefusal
	switch {
	case errors.As(err, &refused):
		// The importer refuses a set with a file above the bound before it parses any, so the tree
		// is refused with no file parsed.
	case empty && !*allowEmpty:
		say(stderr, "%s: %s, and tree reads it as the empty tree only with --allow-empty", dir, what)
		return 2
	case empty:
		// Git keeps no empty directory, so a repository with no spec file yet has no directory, and
		// the importer reads a missing directory as the empty set.
	case err != nil:
		return cannotJudge(stderr, dir, err)
	}
	if o.Retired == nil {
		say(stderr, "%s: no --retired list, so no id is checked against the ids the approved spec removed", dir)
	}
	if refused != nil {
		for _, problem := range refused.Problems {
			say(stderr, "%s", problem)
		}
		say(stdout, "%s: %s; the tree is refused", dir, j.counts())
		return 1
	}
	j.sayProblems(stderr)
	switch {
	case len(j.problems) > 0:
		say(stdout, "%s: %s; the tree is refused", dir, j.counts())
		return 1
	case j.files == 0:
		say(stdout, "%s: %s; the tree is empty", dir, j.counts())
	default:
		say(stdout, "%s: %s; the tree is valid", dir, j.counts())
	}
	return 0
}

// readRetired reads the ids of a --retired file: one id a line, white space around it trimmed,
// and lines that start with "#" and blank lines passed over. An empty file gives an empty list,
// not nil, so every id is checked against it.
func readRetired(path string) ([]string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	retired := []string{}
	for line := range strings.Lines(string(content)) {
		if id := strings.TrimSpace(line); id != "" && !strings.HasPrefix(id, "#") {
			retired = append(retired, id)
		}
	}
	return retired, nil
}
