package specgraph

import (
	"slices"
	"strings"
	"testing"
)

// A tie covers a path it names, each file directly in a "<dir>/*" directory and each file under a
// "<dir>/**" directory, and no other path: not a file one level deeper than "<dir>/*", not a path
// that only starts with a tied path, and not a file in a directory that only starts with a tied
// directory's name.
func TestTieCovers(t *testing.T) {
	t.Parallel()
	tie, err := NewTie([]string{"p/testdata/**", "go.work", "p/*", "go.work"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"go.work", "p/*", "p/testdata/**"}; !tie.Known || !slices.Equal(tie.Paths, want) {
		t.Errorf("NewTie = %+v, want known, paths %q", tie, want)
	}
	for path, want := range map[string]bool{
		"go.work":             true,
		"p/a.go":              true,
		"p/testdata/x/y.json": true,
		"p/testdata/z.json":   true,
		"p/q/b.go":            false,
		"go.work.sum":         false,
		"pp/a.go":             false,
		"p":                   false,
		"p/testdata":          true, // a file named testdata directly in p
		"p/testdatax/y.json":  false,
	} {
		if got := tie.Covers(path); got != want {
			t.Errorf("Covers(%q) = %t, want %t", path, got, want)
		}
	}
	for _, path := range []string{"go.work", "p/a.go", "p/testdata/x/y.json", ""} {
		if (Tie{Known: false, Paths: []string{"go.work", "p/**"}}).Covers(path) {
			t.Errorf("a tie that is not known covers %q", path)
		}
	}
}

// NewTie refuses an entry in any form but a repository path, "<dir>/*" and "<dir>/**", and names
// the entry. It refuses a tie of no path, which would read as none (R18: a tie that Dispatch cannot
// derive "reads as unknown, not as none").
func TestNewTieRefusesAnotherForm(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"p/*.go", "p/**/x", "", "/abs", "*", "**", "p/", "p//a.go", "./p/a.go", "p/../q", `p\a.go`} {
		_, err := NewTie([]string{"go.work", entry})
		if err == nil {
			t.Errorf("NewTie accepts %q", entry)
			continue
		}
		if quoted := `"` + strings.ReplaceAll(entry, `\`, `\\`) + `"`; !strings.Contains(err.Error(), quoted) {
			t.Errorf("NewTie(%q): %v, want the entry %s named", entry, err, quoted)
		}
	}
	if _, err := NewTie(nil); err == nil {
		t.Error("NewTie accepts a tie of no path")
	}
}

// ReadCommitTies reads the tie of each check at each commit (R18: "At each commit, Dispatch derives
// four ties"). A check that the object of a commit does not name, and each check at a commit that
// the form does not name, has a tie that is not known. It refuses a commit of no letter and no digit,
// as the scenario reader refuses such a commit of a subject, a tie that NewTie refuses, naming its
// commit and its check, and a value of another type, such as the form by check alone, which names
// no commit, with the form that the file needs and the place of the value.
func TestReadCommitTies(t *testing.T) {
	t.Parallel()
	ties, err := ReadCommitTies(strings.NewReader(`{"c1":{"a":["p/**","go.work"],"b":["go.work"]},"c2":{"a":["go.work"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		commit, check string
		want          Tie
	}{
		{"c1", "a", Tie{Paths: []string{"go.work", "p/**"}, Known: true}},
		{"c1", "b", Tie{Paths: []string{"go.work"}, Known: true}},
		{"c2", "a", Tie{Paths: []string{"go.work"}, Known: true}},
		{"c2", "b", Tie{}},
		{"c3", "a", Tie{}},
	} {
		if got := ties[row.commit][row.check]; got.Known != row.want.Known || !slices.Equal(got.Paths, row.want.Paths) {
			t.Errorf("the tie of %s at %s = %+v, want %+v", row.check, row.commit, got, row.want)
		}
	}
	const form = `it is not in the form {"<commit>":{"<check id>":["<path or pattern>", …]}}`
	for text, want := range map[string]string{
		`{" \t ":{"a":["go.work"]}}`:         `commit " \t ": a commit of no letter and no digit names no commit`,
		`{"":{"a":["go.work"]}}`:             `commit "": a commit of no letter and no digit names no commit`,
		`{"c1":{"a":["p/*.go"]}}`:            `commit "c1": check "a": specgraph: tie entry "p/*.go" is not a repository path, <dir>/* or <dir>/**`,
		`{"c1":{"a":[]}}`:                    `commit "c1": check "a": specgraph: a tie holds no path`,
		`{"c1":{"a":["go.work"],"a":["x"]}}`: `member "/c1/a" is in one object two times`,
		`{"a":["go.work"]}`:                  form + `: the value at "/a" is of another type`,
		`{"c1":{"a":"go.work"}}`:             form + `: the value at "/c1/a" is of another type`,
		`{"c1":{"a":[7]}}`:                   form + `: the value at "/c1/a/0" is of another type`,
		`["go.work"]`:                        form,
	} {
		if _, err := ReadCommitTies(strings.NewReader(text)); err == nil || err.Error() != want {
			t.Errorf("ReadCommitTies(%s): %v, want %s", text, err, want)
		}
	}
}
