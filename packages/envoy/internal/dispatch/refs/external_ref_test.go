package refs

import (
	"errors"
	"strings"
	"testing"
)

// A repository is read as Dispatch stores it: lower-cased, and one trailing `.git` trimmed off the
// name. Each half is free of `/` and of the whitespace RE2's `\s` matches, and the name free of
// `#`, so `acme/widgets#5`, which names an issue, is no repository, and neither is a name with a
// space or a tab in or around it or a third segment: a caller that takes a value a person wrote
// trims it first, so each caller keeps the reading it had when it read the value as a reference.
// Every other space, the vertical tab and Unicode's U+00A0 among them, the pattern takes and
// ExternalRepo trims from around each half, so `acme/x\u00a0` reads as `acme/x`.
//
// The negative controls are a reading that lets `#` into the name, which takes `acme/widgets#5`,
// and one that trims the space around the value, which takes `acme/x ` and ` acme/x`.
func TestParseExternalRepoReadsARepositoryAsDispatchStoresIt(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		raw, want string
	}{
		{raw: "acme/widgets", want: "acme/widgets"},
		{raw: "Acme/Widgets.git", want: "acme/widgets"},
		{raw: " acme/x"},
		{raw: "acme/x "},
		{raw: "acme/x\t"},
		{raw: "acme/x\u00a0", want: "acme/x"},
		{raw: "\u00a0acme/x\v", want: "acme/x"},
		{raw: "acme/x.git.git", want: "acme/x.git"},
		{raw: "mona-cat_octo/x", want: "mona-cat_octo/x"},
		{raw: "acme/widgets#5"},
		{raw: "acme/wid gets"},
		{raw: "acme/widgets/extra"},
		{raw: "acme/"},
		{raw: "/widgets"},
		{raw: "acme"},
		{raw: ""},
	} {
		got, ok := ParseExternalRepo(row.raw)
		if ok != (row.want != "") || got != row.want {
			t.Errorf("ParseExternalRepo(%q) = %q, %v; want %q, %v", row.raw, got, ok, row.want, row.want != "")
		}
	}
}

// An issue reference is a repository ParseExternalRepo reads and a number after its last `#`,
// with the space around the whole reference trimmed and none inside it.
func TestParseExternalRefReadsItsRepositoryAsParseExternalRepoDoes(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		ref, repo, number string
	}{
		{ref: "Acme/Widgets.git#12", repo: "acme/widgets", number: "12"},
		{ref: " acme/widgets#3\n", repo: "acme/widgets", number: "3"},
		{ref: "acme/widgets #3"},
		{ref: "acme/widgets#0"},
		{ref: "acme/widgets#1#2"},
		{ref: "acme/widgets"},
		{ref: "#1"},
	} {
		repo, number, ok := ParseExternalRef(row.ref)
		if ok != (row.repo != "") || repo != row.repo || number != row.number {
			t.Errorf("ParseExternalRef(%q) = %q, %q, %v; want %q, %q, %v", row.ref, repo, number, ok, row.repo, row.number, row.repo != "")
		}
	}
}

// ParseGitHubRepo reads a repository as ParseExternalRepo does and holds the form it reads to the
// names GitHub allows (SplitGitHubRepo), so the rule is put to the name Dispatch stores:
// `acme/.git.git` is `acme/.git` stored, and refused, and `acme/<100 characters>.git` is the 100
// characters stored, and taken. A name the rule refuses is ErrNotGitHubRepo, and a value
// ParseExternalRepo cannot read is refused with an error that is not, so a caller can tell the
// two apart.
//
// The negative controls are the rule put to raw as written, which takes `acme/.git.git` and
// refuses `acme/<100 characters>.git`, and a reading refusal that wraps ErrNotGitHubRepo.
func TestParseGitHubRepoHoldsTheStoredFormToGitHubsNames(t *testing.T) {
	t.Parallel()
	hundred := strings.Repeat("a", 100)
	for _, row := range []struct {
		raw, owner, name string
		// ruled is a refusal by GitHub's rule, of a value the reading took.
		ruled bool
	}{
		{raw: "Acme/Widgets.git", owner: "acme", name: "widgets"},
		{raw: "acme/" + hundred + ".git", owner: "acme", name: hundred},
		{raw: "acme/x.git.git", owner: "acme", name: "x.git"},
		{raw: "acme/.git.git", ruled: true},
		{raw: "acme/...git", ruled: true},
		{raw: "acme/" + hundred + "a", ruled: true},
		{raw: "-acme/x", ruled: true},
		{raw: " acme/x"},
		{raw: "acme/widgets#5"},
	} {
		owner, name, err := ParseGitHubRepo(row.raw)
		switch {
		case row.owner != "":
			if err != nil || owner != row.owner || name != row.name {
				t.Errorf("ParseGitHubRepo(%q) = %q, %q, %v; want %q, %q", row.raw, owner, name, err, row.owner, row.name)
			}
		case err == nil:
			t.Errorf("ParseGitHubRepo(%q) = %q, %q; want it refused", row.raw, owner, name)
		case errors.Is(err, ErrNotGitHubRepo) != row.ruled:
			t.Errorf("ParseGitHubRepo(%q) refused it with %v; want ErrNotGitHubRepo %v", row.raw, err, row.ruled)
		}
	}
}
