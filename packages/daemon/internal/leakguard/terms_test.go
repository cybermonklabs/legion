package leakguard

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// The placeholder terms these tests plant, which name nothing: one plain, one holding a "/", which
// the guard finds across a path's parts, and a `key:` line, which names a Dispatch project for CI's
// issue-key rule and no term of this guard's. They stand for the deployment's own list, which no
// file of this repository may hold.
const (
	thePlainTerm = "zephyr-orchard"
	theSplitTerm = "lanternfish/harbor-map"
	theTermList  = thePlainTerm + "\n  \n" + theSplitTerm + "\nkey:QUILL\n"
)

// termRefusal is err as a *Refusal whose message names no spelling of either planted term.
func termRefusal(t *testing.T, err error) *Refusal {
	t.Helper()
	refused := refusal(t, err)
	for _, term := range []string{thePlainTerm, theSplitTerm, "lanternfish", "harbor-map"} {
		if strings.Contains(strings.ToLower(err.Error()), term) {
			t.Fatalf("the refusal names a planted term: %v", err)
		}
	}
	return refused
}

// The deployment's term list, handed to the guard in its environment, is a rule of its own: a
// text that names a term, in any case, is refused for a public destination naming the record and
// the class alone; the same text passes with no list, with a list of blank and `key:` lines alone,
// and for a private destination, as the repository's own rules do.
func TestTheDeploymentsTermsRefuseATextThatNamesOne(t *testing.T) {
	check := leakguardtest.Write(t, t.TempDir(), passes)
	text := TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\nsee the " + strings.ToUpper(thePlainTerm) + " runbook\n"}

	err := guardWith(t, check, Public, TermsVar+"="+theTermList).Text(bound(t), text)
	refused := termRefusal(t, err)
	if want := []string{"the body: " + termLabel}; !slices.Equal(refused.Lines, want) {
		t.Fatalf("the report = %q, want %q", refused.Lines, want)
	}

	for _, tc := range []struct {
		name       string
		visibility Visibility
		env        []string
	}{
		{"no list", Public, nil},
		{"a list of blank and key: lines alone", Public, []string{TermsVar + "=\n \nkey:QUILL\n"}},
		{"a private destination", Private, []string{TermsVar + "=" + theTermList}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := guardWith(t, check, tc.visibility, tc.env...).Text(bound(t), text); err != nil {
				t.Fatalf("Text = %v, want a pass", err)
			}
		})
	}
}

// A file's path that names a term is refused too, and the report masks every part a term touches,
// or the whole path where a term holding a "/" shows across its parts; the text is reported apart
// from the path.
func TestTheDeploymentsTermsRefuseAPathThatNamesOne(t *testing.T) {
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, TermsVar+"="+theTermList)
	for _, tc := range []struct {
		name   string
		record Record
		want   []string
	}{
		{"a part of the path", Record{Label: "docs/" + thePlainTerm + "-notes/readme.md", Text: "clean\n"},
			[]string{"docs/<name>/readme.md: " + termLabel + " in its path"}},
		{"a term across two parts", Record{Label: "docs/" + theSplitTerm + "/readme.md", Text: "clean\n"},
			[]string{"<name>: " + termLabel + " in its path"}},
		{"the path and the text", Record{Label: "docs/" + thePlainTerm + ".md", Text: "the " + thePlainTerm + " notes\n"},
			[]string{"docs/<name>: " + termLabel, "docs/<name>: " + termLabel + " in its path"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refused := termRefusal(t, guard.Rules(bound(t), tc.record))
			if !slices.Equal(refused.Lines, tc.want) {
				t.Fatalf("the report = %q, want %q", refused.Lines, tc.want)
			}
		})
	}
}

// A role holds the list behind its file pointer (TermsFileVar), as every launch secret is handed:
// the guard reads the file, and the file wins over a value of the variable itself; a pointer to a
// file that cannot be read refuses the write as a setup failure rather than run without the list.
func TestTheDeploymentsTermsAreReadFromTheFileARoleIsHanded(t *testing.T) {
	check := leakguardtest.Write(t, t.TempDir(), passes)
	file := filepath.Join(t.TempDir(), "terms")
	if err := os.WriteFile(file, []byte(theTermList), 0o600); err != nil {
		t.Fatal(err)
	}
	record := Record{Label: "the ref name", Text: "fix/" + thePlainTerm}

	err := guardWith(t, check, Public, TermsVar+"=unrelated-word", TermsFileVar+"="+file).Rules(bound(t), record)
	if want := []string{"the ref name: " + termLabel}; !slices.Equal(termRefusal(t, err).Lines, want) {
		t.Fatalf("the report = %q, want %q", termRefusal(t, err).Lines, want)
	}

	missing := filepath.Join(t.TempDir(), "absent")
	failed := setupFailure(t, guardWith(t, check, Public, TermsFileVar+"="+missing).Rules(bound(t), Record{Label: "the ref name", Text: "fix/clean"}))
	if !strings.Contains(failed.Error(), "the deployment's term list could not be read") {
		t.Fatalf("the setup failure = %q, want it to say the term list could not be read", failed)
	}
}
