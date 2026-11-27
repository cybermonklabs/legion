package specgraph

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// ErrSelector is the refusal of a selector that picks no one test of the go runner.
var ErrSelector = errors.New("specgraph: selector is not <import path> <TopLevelTestName> for runner go")

// topLevelTestName is the name of a top-level Go test: Test, then nothing or an identifier that
// does not start with a lower-case letter, as go test runs it. A subtest's "/" is not part of it.
var topLevelTestName = regexp.MustCompile(`^Test(?:[A-Z0-9_][A-Za-z0-9_]*)?$`)

// ParseSelector reads the selector of a go check: a Go import path and the name of one top-level
// test in it, separated by one space. The path holds no white space, no empty element, no "." or
// ".." element, and starts with neither "/" nor ".".
func ParseSelector(selector string) (importPath, name string, err error) {
	path, test, ok := strings.Cut(selector, " ")
	if !ok || !importPathForm(path) || !topLevelTestName.MatchString(test) {
		return "", "", fmt.Errorf("%w: %q", ErrSelector, Cut(selector))
	}
	return path, test, nil
}

// MatchTests is each entry of ran that the check's test part picks: the entries of exactly the
// selector's package and top-level test, two when the name runs in both package p and package
// p_test, none when the test did not run. A runner other than go, or a selector that ParseSelector
// refuses, is an error wrapping ErrSelector.
func MatchTests(t Test, ran []TestOutcome) ([]TestOutcome, error) {
	if t.Runner != "go" {
		return nil, fmt.Errorf("%w: runner %q", ErrSelector, Cut(t.Runner))
	}
	path, name, err := ParseSelector(t.Selector)
	if err != nil {
		return nil, err
	}
	var matched []TestOutcome
	for _, o := range ran {
		if o.Package == path && o.Test == name {
			matched = append(matched, o)
		}
	}
	return matched, nil
}

// importPathForm reports whether path has the form of a Go import path.
func importPathForm(path string) bool {
	if path == "" || strings.ContainsFunc(path, unicode.IsSpace) {
		return false
	}
	for _, element := range strings.Split(path, "/") {
		if element == "" || element == "." || element == ".." {
			return false
		}
	}
	return !strings.HasPrefix(path, ".")
}
