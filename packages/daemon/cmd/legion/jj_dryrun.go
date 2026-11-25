package main

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

// The shapes of a line inside a block `Changes to push to <remote>:`. jj 0.45 prints
// `  bookmark: <name> [<action>]`, the action in one of jjRefActions' shapes; jj 0.38 prints
// `  <Action> bookmark <name> …`, in one of jjOldRefLines'. Each name is everything between the
// words around it, so a line is placed whatever its name holds. A shape's one group is the commit
// id the line adds or moves its ref to; a delete's shape has none.
var (
	jjRefLine    = regexp.MustCompile(`^  (?:bookmark|tag): .+ \[(.+)\]$`)
	jjRefActions = []*regexp.Regexp{
		regexp.MustCompile(`^add to ([0-9a-f]+)$`),
		regexp.MustCompile(`^move (?:forward|backward|sideways) from [0-9a-f]+ to ([0-9a-f]+)$`),
		regexp.MustCompile(`^delete from [0-9a-f]+$`),
	}
	jjOldRefLines = []*regexp.Regexp{
		regexp.MustCompile(`^  Add (?:bookmark|tag) .+ to ([0-9a-f]+)$`),
		regexp.MustCompile(`^  Move (?:forward|backward|sideways) (?:bookmark|tag) .+ from [0-9a-f]+ to ([0-9a-f]+)$`),
		regexp.MustCompile(`^  Delete (?:bookmark|tag) .+ from [0-9a-f]+$`),
	}
	jjNoAuthorRefusal = regexp.MustCompile(`^Error: Won't push commit ([0-9a-f]+) since it has no author and/or committer set$`)
)

// parseJJDryRun reads a dry run's standard error, in either jj 0.45's shape
// (`  bookmark: <name> [add to <id>]`, `[move forward|backward|sideways from <a> to <b>]`,
// `[delete from <a>]`, `  tag: <name> [add to <id>]`) or jj 0.38's
// (`  Add bookmark <name> to <id>`, `  Move forward|sideways|backward bookmark <name> from <a> to <b>`,
// `  Delete bookmark <name> from <a>`, and the same with `tag`). It answers the commit ids the
// push sends: the one each line of a block adds or moves its ref to, each once, in the order of
// the lines; none for a dry run that printed `Nothing changed.`. Lines outside a block are passed
// over. The error names the first line inside a block that it could not place, or the heading of
// a block with no line under it, which jj never prints, or says the output has neither a block
// nor `Nothing changed.`.
func parseJJDryRun(stderr string) ([]string, error) {
	var sends []string
	heading, placed := "", 0 // the heading of the block a line is in ("" outside one), and its lines
	blocks, nothing := 0, false
	// end ends the block a line was in, where it was in one.
	end := func() error {
		if heading != "" && placed == 0 {
			return errors.New(heading + " with no line under it")
		}
		heading = ""
		return nil
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(stderr, "\n"), "\n") {
		switch {
		case heading != "" && strings.HasPrefix(line, "  "):
			id, ok := parseJJRefLine(line)
			if !ok {
				return nil, errors.New(strings.TrimSpace(line))
			}
			placed++
			if id != "" && !slices.Contains(sends, id) {
				sends = append(sends, id)
			}
		case strings.HasPrefix(line, "Changes to push to ") && strings.HasSuffix(line, ":"):
			if err := end(); err != nil {
				return nil, err
			}
			heading, placed = line, 0
			blocks++
		default:
			if err := end(); err != nil {
				return nil, err
			}
			nothing = nothing || line == "Nothing changed."
		}
	}
	if err := end(); err != nil {
		return nil, err
	}
	if blocks == 0 && !nothing {
		return nil, errors.New("the dry run printed neither a block of changes nor `Nothing changed.`")
	}
	return sends, nil
}

// parseJJRefLine places one line inside a block, in either release's shape: ok is whether it has
// one of them, and id the commit id it adds or moves its ref to, "" for a delete.
func parseJJRefLine(line string) (id string, ok bool) {
	shapes := jjOldRefLines
	if parts := jjRefLine.FindStringSubmatch(line); parts != nil {
		line, shapes = parts[1], jjRefActions
	}
	for _, shape := range shapes {
		if parts := shape.FindStringSubmatch(line); parts != nil {
			if len(parts) > 1 {
				return parts[1], true
			}
			return "", true
		}
	}
	return "", false
}

// jjNoAuthorCommit is the commit id that jj's refusal
// `Error: Won't push commit <id> since it has no author and/or committer set` names, and line, that
// refusal's line, if stderr holds it.
func jjNoAuthorCommit(stderr string) (id, line string, ok bool) {
	for l := range strings.SplitSeq(stderr, "\n") {
		if parts := jjNoAuthorRefusal.FindStringSubmatch(l); parts != nil {
			return parts[1], l, true
		}
	}
	return "", "", false
}
