package leakguard

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// readsAndEnvironment is the sh with which a stand-in real gh writes, to the file at path, its
// arguments on the first line and then its whole environment, one entry a line, which
// readArgvAndEnvironment reads back.
func readsAndEnvironment(path string) string {
	return "printf 'argv=%s\\n' \"$*\" > " + shQuote(path) + "\nenv >> " + shQuote(path) + "\n"
}

// readArgvAndEnvironment is the arguments and the environment a stand-in wrote with
// readsAndEnvironment, and fails the test where it wrote nothing.
func readArgvAndEnvironment(t *testing.T, path string) (string, []string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the real gh the caller answered never ran: %v", err)
	}
	first, rest, _ := strings.Cut(string(content), "\n")
	return strings.TrimPrefix(first, "argv="), strings.Split(strings.TrimSuffix(rest, "\n"), "\n")
}

// Where the caller names the gh of the guard's own reads (Options.GhRead), as cmd/legion's `legion
// gh` and `legion leak-check` do, the REST read and the GraphQL read each run the program the
// caller answers for the read's repository, with gh's arguments as ever and with the environment
// the caller answered, whole: the guard adds none of its own names to it, and the gh on the
// environment's PATH never runs. The caller is handed the repository and the environment the
// guard would have started `gh` from PATH with (Guard.env, ghReadEnv on top), which is what a
// `legion gh` started for the read read its own from. Here the REST read meets a spent core limit,
// and the GraphQL read, made the same way, answers private.
func TestAVisibilityReadRunsTheGhTheCallerAnswersWithTheEnvironmentItAnswers(t *testing.T) {
	bin, records := t.TempDir(), t.TempDir()
	onPath := filepath.Join(records, "gh-on-path")
	writeProgram(t, bin, "gh", "echo ran >> "+shQuote(onPath)+"\nexit 1\n")
	rest, graphql := filepath.Join(records, "rest"), filepath.Join(records, "graphql")
	real := filepath.Join(t.TempDir(), "real-gh")
	writeProgram(t, filepath.Dir(real), filepath.Base(real), ghStandIn(
		readsAndEnvironment(rest)+spentRateLimit(time.Now().Add(time.Hour), filepath.Join(records, "reads")),
		readsAndEnvironment(graphql)+graphQLAnswers(filepath.Join(records, "graphql-calls"), "PRIVATE"),
	))
	answered := []string{"PATH=" + os.Getenv("PATH"), "INVENTED_ANSWERED_NAME=invented-value"}
	var slugs []string
	var environs [][]string
	var sources []VisibilitySource
	guard := New(Options{
		RoleState:    t.TempDir(),
		OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
		Env:          append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
		GhRead: func(slug string, environ []string) (GhReadChild, error) {
			slugs, environs = append(slugs, slug), append(environs, slices.Clone(environ))
			return GhReadChild{Program: real, Env: answered}, nil
		},
	})
	visibility, err := guard.Visibility(bound(t), theRepo)
	if err != nil || visibility != Private {
		t.Fatalf("read %q, %v; want the GraphQL read's private", visibility, err)
	}
	if want := []VisibilitySource{VisibilityFromGraphQL}; !slices.Equal(sources, want) {
		t.Errorf("the guard reported the sources %q, want %q", sources, want)
	}
	if got := countLines(t, onPath); got != 0 {
		t.Errorf("the gh on the guard's PATH ran %d times, want never", got)
	}
	if want := []string{theRepo, theRepo}; !slices.Equal(slugs, want) {
		t.Errorf("the caller was asked for the gh of %q, want %q: once for each read", slugs, want)
	}
	from := guard.env(guard.base(), nil, ghReadEnv(theRepo)...)
	for i, environ := range environs {
		if !slices.Equal(environ, from) {
			t.Errorf("the caller was handed, for read %d, the environment\n%q\nwant the one `gh` would have started from\n%q", i+1, environ, from)
		}
	}
	for _, read := range []struct {
		name, record string
		argv         []string
	}{
		{"REST", rest, VisibilityReadWithHeadersArgs(theRepo)},
		{"GraphQL", graphql, VisibilityGraphQLArgs(theRepo)},
	} {
		argv, environ := readArgvAndEnvironment(t, read.record)
		if want := strings.Join(read.argv, " "); argv != want {
			t.Errorf("the %s read ran the real gh as %q, want %q", read.name, argv, want)
		}
		if !slices.Contains(environ, "INVENTED_ANSWERED_NAME=invented-value") {
			t.Errorf("the %s read ran the real gh without the environment the caller answered:\n%s", read.name, strings.Join(environ, "\n"))
		}
		for _, entry := range environ {
			name, _, _ := strings.Cut(entry, "=")
			if slices.Contains([]string{VisibilityReadVar, "GH_HOST", "NO_COLOR", "CLICOLOR_FORCE", "GIT_TERMINAL_PROMPT", "GIT_ASKPASS", RunByRealGhVar, "XDG_STATE_HOME"}, name) {
				t.Errorf("the %s read handed the real gh %s, which the caller did not answer", read.name, entry)
			}
		}
	}
}

// The caller's refusal of the gh of the guard's REST read is read as `legion gh`'s refusal of the
// same read is (TestOnlyLegionGhsRefusalOfTheOwnerIsReadWithNoCredential): an OwnerNotServedError
// alone, for the owner of the repository read, lowercased as `legion gh` names it, and for the
// bundle LEGION_GH_OWNERS_DIR names, sends the read to ls-remote with no credential, and an owner
// spelled in capitals is read at the URL as it was spelled. Every other refusal is the setup
// failure it was, naming the read and the refusal, and git never runs, although the stand-in git
// would answer public: the refusal of another owner or of another bundle, the refusal where the
// guard's environment names no bundle, a configured rename's refusal, which holds an
// OwnerNotServedError inside a longer one (cmd/legion's ghConfigDirFor), and any other refusal.
// No gh runs for any of them, and the GraphQL read is never asked for.
func TestAVisibilityReadTheCallerRefusesForAnUnservedOwnerIsReadWithNoCredential(t *testing.T) {
	const owners = "/var/run/invented/gh"
	notServed := &OwnerNotServedError{Owner: "invented-org", OwnersDir: owners}
	rename := fmt.Errorf("the configured rename invented-org/invented-repo -> moved-org/invented-repo serves invented-org/invented-repo from moved-org's gh directory, and %w; GitHub holds the repository under the name it moved to, so no other token of this bundle reaches it",
		&OwnerNotServedError{Owner: "moved-org", OwnersDir: owners})
	for _, tc := range []struct {
		name string
		// slug is the repository read, theRepo where it is empty; refused is what the caller
		// answers; env is what the guard's environment holds beyond the test's own,
		// LEGION_GH_OWNERS_DIR=owners where it is nil.
		slug    string
		refused error
		env     []string
		// public is whether the read answers public through ls-remote; a read that does not
		// fails naming the read and the refusal.
		public bool
	}{
		{name: "the refusal of the owner", refused: notServed, public: true},
		{name: "the refusal of an owner spelled in capitals", slug: "Invented-Org/invented-repo", refused: notServed, public: true},
		{name: "the refusal of another owner", refused: &OwnerNotServedError{Owner: "other-owner", OwnersDir: owners}},
		{name: "the refusal for another bundle", refused: &OwnerNotServedError{Owner: "invented-org", OwnersDir: "/var/run/other/gh"}},
		{name: "the refusal where no bundle is named", refused: notServed, env: []string{"LEGION_GH_OWNERS_DIR="}},
		{name: "the refusal of a configured rename whose new owner has no directory", refused: rename},
		{name: "another refusal", refused: errors.New("GH_CONFIG_DIR is unset: a Legion pane or pod sets it, and nothing else runs this gh")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slug := cmp.Or(tc.slug, theRepo)
			env := tc.env
			if env == nil {
				env = []string{"LEGION_GH_OWNERS_DIR=" + owners}
			}
			bin, records := t.TempDir(), t.TempDir()
			onPath, record := filepath.Join(records, "gh-on-path"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", "echo ran >> "+shQuote(onPath)+"\nexit 1\n")
			writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
			var asked []string
			visibility, err := New(Options{
				RoleState: t.TempDir(),
				Env:       append(append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()), env...),
				GhRead: func(slug string, _ []string) (GhReadChild, error) {
					asked = append(asked, slug)
					return GhReadChild{}, tc.refused
				},
			}).Visibility(bound(t), slug)
			calls := readCalls(t, record)
			if tc.public {
				if err != nil || visibility != Public {
					t.Fatalf("read %q, %v; want public from ls-remote", visibility, err)
				}
				if len(calls) != 1 || !strings.Contains(calls[0]["argv"], " ls-remote "+githubOrigin+"/"+slug+".git HEAD") {
					t.Errorf("git ran as %q, want one ls-remote of %s", calls, slug)
				}
			} else {
				want := "gh api repos/" + slug + " was refused before gh ran: " + tc.refused.Error()
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("read %q, %v; want the read refused saying %q", visibility, err, want)
				}
				if len(calls) != 0 {
					t.Errorf("git ran %d times, want never: only the refusal of the owner is read with no credential", len(calls))
				}
			}
			if want := []string{slug}; !slices.Equal(asked, want) {
				t.Errorf("the caller was asked for the gh of %q, want %q: the REST read's alone", asked, want)
			}
			if got := countLines(t, onPath); got != 0 {
				t.Errorf("the gh on the guard's PATH ran %d times, want never", got)
			}
		})
	}
}
