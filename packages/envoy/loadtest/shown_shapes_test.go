package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// shapeProject is the project whose keys these rows read, and shapeKey one of its issue keys with
// number written where the number goes.
const shapeProject = "PRJ"

func shapeKey(number string) string { return shapeProject + "-" + number }

// TestAnIssueKeyIsTheProjectsKeyAndABoundedNumber: an issue key is a project key, a hyphen and a
// positive decimal of at most issueKeyDigits digits, which is every number Dispatch can give an
// issue. The bound is what the file names replay-fetch builds from a key rest on (replay_bundle.go
// claims no name longer than a few dozen bytes), and the number had none: a key of a thousand
// digits was an issue key, and named a file of a thousand bytes. The control: a key of the
// bound's own length is one still, and the project's keys still give their numbers.
func TestAnIssueKeyIsTheProjectsKeyAndABoundedNumber(t *testing.T) {
	t.Run("bound", func(t *testing.T) {
		// The bound as a number, so the row says what an issue key is rather than echoing the
		// constant that holds it (issueKeyDigits).
		const digits = 10
		for _, c := range []struct {
			number string
			want   bool
		}{
			{"1", true},
			{"2147483647", true},
			{strings.Repeat("9", digits), true},
			{strings.Repeat("9", digits+1), false},
			{strings.Repeat("9", 1000), false},
			{"0", false},
			{"01", false},
		} {
			key := shapeKey(c.number)
			if isIssueKey(key) != c.want {
				t.Errorf("isIssueKey(a key of %d digits) = %t, want %t", len(c.number), !c.want, c.want)
			}
			n, err := issueNumber(shapeProject, key)
			if (err == nil) != c.want {
				t.Errorf("issueNumber(a key of %d digits) = %d, %v; want it %snamed", len(c.number), n, err, map[bool]string{true: "", false: "not "}[c.want])
			}
		}
	})
	t.Run("project-pattern", func(t *testing.T) {
		// isIssueKey read the project key by editing the text of another package's pattern: the
		// "$" trimmed off contracts.ProjectKeyPattern and this glued to the end of it. A project
		// pattern anchored any other way -- with \z, say -- leaves that anchor in the middle of
		// the pattern it built, which then matches no key at all, and nothing says so. isIssueKey
		// reads the key through contracts.IssueProject now, so no text of that pattern reaches it.
		anchored := strings.Replace(contracts.ProjectKeyPattern, "$", `\z`, 1)
		edited := regexp.MustCompile(strings.TrimSuffix(anchored, "$") + `-[1-9][0-9]*$`)
		key := shapeKey("12")
		if edited.MatchString(key) {
			t.Errorf("the edited pattern names %s, so it is no control for a pattern anchored another way", key)
		}
		if !isIssueKey(key) {
			t.Errorf("isIssueKey does not name %s", key)
		}
	})
}
