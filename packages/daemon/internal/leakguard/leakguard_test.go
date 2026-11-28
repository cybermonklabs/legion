package leakguard

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// noSHA is git's "this ref is not there", which a pre-push line carries for a ref the push
// creates and for one it deletes.
const noSHA = "0000000000000000000000000000000000000000"

// The three stand-in shared checks these tests drive the guard against, which are the shared
// ones: each reads its standard input, as the real check does, so that nothing is left in a pipe
// when it exits.
const (
	passes       = leakguardtest.Passes
	refuses      = leakguardtest.Refuses
	cannotFinish = leakguardtest.CannotFinish
)

// answers is a visibility reader that answers visibility for every slug it is asked about, and
// counts what it was asked.
func answers(visibility Visibility, asked *[]string) func(context.Context, string) (Visibility, error) {
	return func(_ context.Context, slug string) (Visibility, error) {
		if asked != nil {
			*asked = append(*asked, slug)
		}
		return visibility, nil
	}
}

// guardWith is a guard whose shared check is check, whose destinations are all visibility, and
// whose children run under an environment of the test's own: no credential, no repository and no
// workspace but the test's (fixtureenv.Isolated), the fixture rule file named (withRules), and
// extra on top.
func guardWith(t *testing.T, check string, visibility Visibility, extra ...string) *Guard {
	t.Helper()
	return New(Options{
		Check:      check,
		Visibility: answers(visibility, nil),
		Env:        withRules(t, append(fixtureenv.Isolated(t.TempDir()), extra...)),
	})
}

// withRules is env with RulesFileVar naming the fixture rule file (leakguardtest.Rules), written
// into a directory of the test's own: the guard runs the rules a deployment names, so a test of
// the rule pass names the fixture as a deployment names its file.
func withRules(t *testing.T, env []string) []string {
	t.Helper()
	return append(env, RulesFileVar+"="+leakguardtest.WriteRules(t, t.TempDir()))
}

// fixtureRules is the fixture rule file, compiled.
func fixtureRules(t *testing.T) *RuleSet {
	t.Helper()
	rules, err := LoadRules([]byte(leakguardtest.Rules))
	if err != nil {
		t.Fatalf("the fixture rules did not compile: %v", err)
	}
	return rules
}

// bound is a context every call of these tests runs under, so that a stand-in that hangs fails
// the test rather than the package's ten minutes.
func bound(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// refusal is err as a *Refusal, failing the test where it is anything else.
func refusal(t *testing.T, err error) *Refusal {
	t.Helper()
	var refused *Refusal
	if !errors.As(err, &refused) {
		t.Fatalf("got %v, want a refusal", err)
	}
	return refused
}

// reports fails the test unless lines, a rule pass's report, is one line for each of want, in
// order, each naming the record and the class want names ("<record>: <class>"). A line is read by
// its front: what the report writes after the class, the match and its line, is not this test's
// (TestPushRefusalShowsMatch and TestRefusalShowsMatchWithinItsBounds read whole lines).
func reports(t *testing.T, lines []string, want ...string) {
	t.Helper()
	same := len(lines) == len(want)
	for i := 0; same && i < len(want); i++ {
		same = lines[i] == want[i] || strings.HasPrefix(lines[i], want[i]+": ")
	}
	if !same {
		t.Errorf("the report is %q, want a line of each of %q", lines, want)
	}
}

// namesClass is whether one line of a rule pass's report names class, whatever its record.
func namesClass(line, class string) bool {
	return strings.HasSuffix(line, ": "+class) || strings.Contains(line, ": "+class+": ")
}

// setupFailure is err as a *SetupFailure, failing the test where it is anything else.
func setupFailure(t *testing.T, err error) *SetupFailure {
	t.Helper()
	var failed *SetupFailure
	if !errors.As(err, &failed) {
		t.Fatalf("got %v, want a setup failure", err)
	}
	return failed
}

// Every entry point runs its own shape of the shared check and refuses what that check refuses,
// and every one of them refuses a record of its own that names something private. A write with no
// repository behind it runs no entry point and no check (TestARuleSetChecksRecordsWithNoGuard).
func TestEachEntryPointRefusesWhatTheCheckRefusesAndWhatTheRulesMatch(t *testing.T) {
	cleanPush := PushRequest{
		Dir:     t.TempDir(),
		Remote:  "origin",
		URL:     "https://github.com/invented-org/invented-repo.git",
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: noSHA, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
	}
	namedPush := cleanPush
	namedPush.Updates = []Update{{LocalRef: "refs/heads/work", LocalSHA: noSHA, RemoteRef: "refs/heads/" + leakguardtest.RulesHost, RemoteSHA: noSHA}}
	for _, entry := range []struct {
		name string
		// checked runs the entry point against guard, with a record that names something
		// private when named.
		checked func(ctx context.Context, guard *Guard, named bool) error
	}{
		{
			name: "Push",
			checked: func(ctx context.Context, guard *Guard, named bool) error {
				request := cleanPush
				if named {
					request = namedPush
				}
				return guard.Push(ctx, request)
			},
		},
		{
			name: "GhCall",
			checked: func(ctx context.Context, guard *Guard, named bool) error {
				args := []string{"pr", "create", "--title", "a clean title"}
				if named {
					args = []string{"pr", "create", "--title", "a title naming " + leakguardtest.RulesHost}
				}
				_, err := guard.GhCall(ctx, GhRequest{Args: args, Repo: "invented-org/invented-repo"})
				return err
			},
		},
		{
			name: "Text",
			checked: func(ctx context.Context, guard *Guard, named bool) error {
				text := "a clean title\n\na clean body\n"
				if named {
					text = "a clean title\n\na body naming " + leakguardtest.RulesHost + "\n"
				}
				return guard.Text(ctx, TextRequest{Repo: "invented-org/invented-repo", Text: text})
			},
		},
	} {
		t.Run(entry.name, func(t *testing.T) {
			ctx := bound(t)
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			if err := entry.checked(ctx, guard, false); err != nil {
				t.Fatalf("a clean write: %v", err)
			}
			refused := refusal(t, entry.checked(ctx, guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public), true))
			if refused.By != byRules {
				t.Errorf("a write naming a host was refused by %q, want the rules", refused.By)
			}
			if len(refused.Lines) != 1 || !namesClass(refused.Lines[0], leakguardtest.RulesHostClass) {
				t.Errorf("the report is %q, want one line naming the class", refused.Lines)
			}
			byCheck := guardWith(t, leakguardtest.Write(t, t.TempDir(), refuses), Public)
			err := entry.checked(ctx, byCheck, false)
			if refused := refusal(t, err); refused.By != bySharedCheck {
				t.Errorf("a write the check refused was refused by %q, want the shared check", refused.By)
			}
		})
	}
}

// takesV33CallForms is a stand-in shared check that takes a call as leak-check-v33's main() does
// (its README.md, "Modes"): a call of exactly two arguments whose first is a mode word is refused
// with exit 2, since that is how a pre-push hook calls the check, with a remote's name and its URL.
// Every other call it clears. Under Recording it records each call first.
const takesV33CallForms = `case "$1" in
check-public-text | --commits | --tree | gh-guard | --help | -h)
	if [ "$#" -eq 2 ]; then
		echo 'LEAK CHECK ERROR: a call of two arguments is how a pre-push hook calls this check'
		exit 2
	fi
	;;
esac
` + passes

// Each entry point calls the shared check in a form leak-check-v33 takes, and no other: a gh call
// runs `gh-guard -- <its words>`, one of a single word and one whose own first word is `--` among
// them, a gh call of no words runs `gh-guard` alone, the text runs `check-public-text -- -`, and a
// push runs the hook's two arguments, the remote and its URL, as git hands them. The stand-in
// refuses a call of two arguments whose first is a mode word, as that check does, so a form it
// would refuse fails here as a setup failure, and each call it records is the whole of what the
// guard handed it.
func TestEachEntryPointCallsTheSharedCheckInAFormItTakes(t *testing.T) {
	const url = "https://github.com/invented-org/invented-repo.git"
	gh := func(args ...string) func(context.Context, *Guard) error {
		return func(ctx context.Context, guard *Guard) error {
			_, err := guard.GhCall(ctx, GhRequest{Args: args, Repo: "invented-org/invented-repo"})
			return err
		}
	}
	for _, tc := range []struct {
		name string
		call func(context.Context, *Guard) error
		want string
	}{
		{name: "a gh call", call: gh("pr", "view", "1"), want: "checked gh-guard -- pr view 1\n"},
		{name: "a gh call of one word", call: gh("status"), want: "checked gh-guard -- status\n"},
		{name: "a gh call whose first word is --", call: gh("--", "pr"), want: "checked gh-guard -- -- pr\n"},
		{name: "a gh call of no words", call: gh(), want: "checked gh-guard\n"},
		{
			name: "the text",
			call: func(ctx context.Context, guard *Guard) error {
				return guard.Text(ctx, TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n\na clean body\n"})
			},
			want: "checked check-public-text -- -\n",
		},
		{
			name: "a push",
			call: func(ctx context.Context, guard *Guard) error {
				return guard.Push(ctx, PushRequest{
					Dir:     t.TempDir(),
					Remote:  "origin",
					URL:     url,
					Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: noSHA, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
				})
			},
			want: "checked origin " + url + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "record")
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), leakguardtest.Recording(takesV33CallForms)), Public,
				"LEAK_CHECK_RECORD="+record)
			if err := tc.call(bound(t), guard); err != nil {
				t.Fatalf("a clean write under a check that takes leak-check-v33's call forms: %v", err)
			}
			got, err := os.ReadFile(record)
			if err != nil {
				t.Fatalf("the shared check did not run: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("the shared check recorded %q, want %q", got, tc.want)
			}
		})
	}
}

// The gh shape's second ask: the stand-in exits 4 to say the call reads standard input, the guard
// buffers that input to a regular file it hands over as a descriptor, the stand-in reads the file
// twice over, and the caller gets the bytes back to hand gh in its place.
func TestTheGhShapeBuffersStandardInputWhenTheCheckAsksForIt(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record")
	asksForStdin := `if [ -z "${PUBLIC_LEAK_CHECK_STDIN}" ]; then
  echo 'this call reads standard input'
  exit 4
fi
{
  echo "buffers=${PUBLIC_LEAK_CHECK_STDIN_BUFFERS}"
  if [ -f "${PUBLIC_LEAK_CHECK_STDIN}" ]; then echo "regular=yes"; else echo "regular=no"; fi
  echo "first=$(cat "${PUBLIC_LEAK_CHECK_STDIN}")"
  echo "again=$(cat "${PUBLIC_LEAK_CHECK_STDIN}")"
} > "${STANDIN_RECORD}"
exit 0
`
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), asksForStdin), Public, "STANDIN_RECORD="+record)
	body := "a body gh reads on its standard input"
	answer, err := guard.GhCall(bound(t), GhRequest{
		Args:  []string{"pr", "create", "--body-file", "-"},
		Stdin: strings.NewReader(body),
		Repo:  "invented-org/invented-repo",
	})
	if err != nil {
		t.Fatalf("a clean call: %v", err)
	}
	if string(answer.Stdin) != body {
		t.Errorf("the caller got %q back, want the bytes it gave", answer.Stdin)
	}
	read, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"buffers=1", "regular=yes", "first=" + body, "again=" + body} {
		if !strings.Contains(string(read), want) {
			t.Errorf("the check read %q, want it to hold %q", read, want)
		}
	}
}

// What the gh shape buffered is scanned as a record of its own, so a body that never reaches an
// argument is read all the same.
func TestTheGhShapeScansWhatItBuffered(t *testing.T) {
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), leakguardtest.Reading(0, "")), Public)
	body := "a body naming " + leakguardtest.RulesHost + "\n"
	answer, err := guard.GhCall(bound(t), GhRequest{
		Args:  []string{"pr", "create", "--body-file", "-"},
		Stdin: strings.NewReader(body),
		Repo:  "invented-org/invented-repo",
	})
	if string(answer.Stdin) != body {
		t.Errorf("the caller got %q back, want the bytes it gave", answer.Stdin)
	}
	refused := refusal(t, err)
	reports(t, refused.Lines, "the gh call's standard input: "+leakguardtest.RulesHostClass)
}

// A destination whose visibility cannot be read refuses the write, as a failure of setup rather
// than of the text: a guard that cannot tell a public repository from a private one has checked
// nothing.
func TestAVisibilityThatCannotBeReadRefuses(t *testing.T) {
	guard := New(Options{
		Check: leakguardtest.Write(t, t.TempDir(), passes),
		Visibility: func(context.Context, string) (Visibility, error) {
			return "", errors.New("gh api exited 1")
		},
		Env: withRules(t, fixtureenv.Isolated(t.TempDir())),
	})
	err := guard.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"})
	failed := setupFailure(t, err)
	if !strings.Contains(failed.Why, "visibility") {
		t.Errorf("the setup failure is %q, want it to name the visibility", failed.Error())
	}
}

// A destination GitHub reports private or internal runs the shared check and skips the rule pass:
// a name of ours in a repository nobody outside the company can read is no leak, and that is the
// same line the shared check draws for its own content scan.
func TestAPrivateOrInternalDestinationSkipsTheRules(t *testing.T) {
	for _, visibility := range []Visibility{Private, Internal} {
		t.Run(string(visibility), func(t *testing.T) {
			var asked []string
			record := filepath.Join(t.TempDir(), "record")
			guard := New(Options{
				Check:      leakguardtest.Write(t, t.TempDir(), leakguardtest.Recording(leakguardtest.Passes)),
				Visibility: answers(visibility, &asked),
				Env:        withRules(t, append(fixtureenv.Isolated(t.TempDir()), "LEAK_CHECK_RECORD="+record)),
			})
			text := "a title naming " + leakguardtest.RulesHost + "\n"
			if err := guard.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: text}); err != nil {
				t.Fatalf("a write to a %s repository: %v", visibility, err)
			}
			if _, err := os.Stat(record); err != nil {
				t.Errorf("the shared check did not run: %v", err)
			}
			if len(asked) != 1 {
				t.Errorf("the visibility was read %d times, want once", len(asked))
			}
		})
	}
}

// Public answers remain for a role's pod life. Private and internal answers are read for every
// guarded call, and a failed read leaves no answer behind for a later call to mistake for a
// completed check.
func TestVisibilityKeepsAnswersButNeverFailures(t *testing.T) {
	t.Run("public", func(t *testing.T) {
		var reads int
		env := append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+t.TempDir())
		read := func(context.Context, string) (Visibility, error) {
			reads++
			return Public, nil
		}
		for range 2 {
			visibility, err := New(Options{Visibility: read, Env: env}).Visibility(bound(t), "invented-org/invented-repo")
			if err != nil || visibility != Public {
				t.Fatalf("read visibility = %q, %v; want public", visibility, err)
			}
		}
		if reads != 1 {
			t.Errorf("the visibility was read %d times, want once", reads)
		}
	})

	t.Run("private and internal", func(t *testing.T) {
		for _, expected := range []Visibility{Private, Internal} {
			t.Run(string(expected), func(t *testing.T) {
				var reads int
				env := append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+t.TempDir())
				read := func(context.Context, string) (Visibility, error) {
					reads++
					return expected, nil
				}
				for range 2 {
					visibility, err := New(Options{Visibility: read, Env: env}).Visibility(bound(t), "invented-org/invented-repo")
					if err != nil || visibility != expected {
						t.Fatalf("read visibility = %q, %v; want %q", visibility, err, expected)
					}
				}
				if reads != 2 {
					t.Errorf("the %s visibility was read %d times, want twice", expected, reads)
				}
			})
		}
	})

	t.Run("failed", func(t *testing.T) {
		var reads int
		env := append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+t.TempDir())
		read := func(context.Context, string) (Visibility, error) {
			reads++
			if reads == 1 {
				return "", errors.New("the read failed")
			}
			return Public, nil
		}
		if _, err := New(Options{Visibility: read, Env: env}).Visibility(bound(t), "invented-org/invented-repo"); err == nil {
			t.Fatal("the first visibility read passed, want its failure")
		}
		if visibility, err := New(Options{Visibility: read, Env: env}).Visibility(bound(t), "invented-org/invented-repo"); err != nil || visibility != Public {
			t.Fatalf("the second visibility read = %q, %v; want public", visibility, err)
		}
		if reads != 2 {
			t.Errorf("the failed visibility read was kept: read %d times, want twice", reads)
		}
	})
}

// A push reads a private destination again after a preceding guarded call: only public answers
// are kept, so a repository made public cannot skip the rule pass.
func TestPushUsesTheRoleKeptVisibility(t *testing.T) {
	state := t.TempDir()
	env := append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state)
	var reads int
	read := func(context.Context, string) (Visibility, error) {
		reads++
		return Private, nil
	}
	if visibility, err := New(Options{Visibility: read, Env: env}).Visibility(bound(t), "invented-org/invented-repo"); err != nil || visibility != Private {
		t.Fatalf("read visibility = %q, %v; want private", visibility, err)
	}
	err := New(Options{
		Check:      leakguardtest.Write(t, t.TempDir(), passes),
		Visibility: read,
		Env:        withRules(t, env),
	}).Push(bound(t), PushRequest{
		Dir:    t.TempDir(),
		Remote: "origin",
		URL:    "https://github.com/invented-org/invented-repo.git",
	})
	if err != nil {
		t.Fatalf("push after the visibility read: %v", err)
	}
	if reads != 2 {
		t.Errorf("the visibility was read %d times, want twice", reads)
	}
}

// The two shared-check failures: no check resolves at all under a deployment that names one (its
// checker, CheckerVar, in the environment), and a check that exits 2. Each refuses the write, and
// each says it is a failure of setup rather than of the text.
func TestTheSharedChecksFailuresAreSetupFailures(t *testing.T) {
	t.Run("no check resolves", func(t *testing.T) {
		// An environment with no PUBLIC_LEAK_CHECK, a PATH with no check on it, and a
		// configuration home holding no pointer file: the three places Resolve looks.
		guard := New(Options{
			Visibility: answers(Public, nil),
			Env:        append(fixtureenv.Isolated(t.TempDir()), "PATH="+t.TempDir(), CheckerVar+"="+theChecker),
		})
		err := guard.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"})
		failed := setupFailure(t, err)
		if failed.Why != WhyNoCheck {
			t.Errorf("the setup failure is %q, want the missing check", failed.Error())
		}
	})
	t.Run("the check exits 2", func(t *testing.T) {
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), cannotFinish), Public)
		err := guard.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"})
		failed := setupFailure(t, err)
		if !strings.Contains(failed.Why, "exit 2") {
			t.Errorf("the setup failure is %q, want it to name the status", failed.Error())
		}
		if !strings.Contains(failed.Error(), "LEAK CHECK SETUP ERROR") {
			t.Errorf("the message is %q, want the shared check's own setup-error line", failed.Error())
		}
	})
}

// A setup failure shows each text a caller wrote that it names — the path of a file a gh call
// sends, the path of the rule file, a destination's slug, a push's remote — on its one line and
// bounded, as a report shows a label (shownText): a newline is written as Go writes it in a
// quoted string, and a slug of a kilobyte is shown as its first 60 bytes and its last 140.
func TestASetupFailureShowsWhatTheCallerWroteOnOneBoundedLine(t *testing.T) {
	dir := t.TempDir()
	passing := func(t *testing.T) string { return leakguardtest.Write(t, t.TempDir(), passes) }
	long := strings.Repeat("r", 1024)
	for _, tc := range []struct {
		name  string
		write func(t *testing.T) error
		says  string
	}{
		{
			name: "the path of a file a gh call sends",
			write: func(t *testing.T) error {
				_, err := guardWith(t, passing(t), Public).GhCall(bound(t), GhRequest{
					Args: []string{"pr", "create", "--body-file", filepath.Join(dir, "body\nnotes.md")},
					Repo: "invented-org/invented-repo",
				})
				return err
			},
			says: "the file --body-file names, " + dir + `/body\nnotes.md, could not be opened: no such file or directory`,
		},
		{
			name: "the path of the rule file",
			write: func(t *testing.T) error {
				guard := New(Options{
					Check:      passing(t),
					Visibility: answers(Public, nil),
					Env:        append(fixtureenv.Isolated(t.TempDir()), RulesFileVar+"="+filepath.Join(dir, "rules\nfile.toml")),
				})
				return guard.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"})
			},
			says: "the private-name rule file " + dir + `/rules\nfile.toml, which ` + RulesFileVar + " names, could not be opened",
		},
		{
			name: "a slug",
			write: func(t *testing.T) error {
				return guardWith(t, passing(t), Public).Text(bound(t), TextRequest{Repo: "invented-org/a\nb", Text: "a clean title\n"})
			},
			says: `the destination invented-org/a\nb is no repository GitHub serves`,
		},
		{
			name: "a slug of a kilobyte",
			write: func(t *testing.T) error {
				return guardWith(t, passing(t), Public).Text(bound(t), TextRequest{Repo: "invented-org/" + long, Text: "a clean title\n"})
			},
			says: "the destination invented-org/" + long[:47] + "…" + long[:140] + " is no repository GitHub serves",
		},
		{
			// A deployment that names its checker (CheckerVar) refuses a write where no check
			// resolves: a deployment that names none would run the write with no shared check.
			name: "a push's remote",
			write: func(t *testing.T) error {
				guard := New(Options{Visibility: answers(Public, nil), Env: append(fixtureenv.Isolated(t.TempDir()), "PATH="+t.TempDir(), CheckerVar+"="+theChecker)})
				return guard.Push(bound(t), PushRequest{Dir: t.TempDir(), Remote: "a\nremote", URL: "https://github.com/invented-org/invented-repo.git"})
			},
			says: `LEAK CHECK SETUP ERROR: the push to a\nremote: ` + WhyNoCheck,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed := setupFailure(t, tc.write(t))
			if message := failed.Error(); strings.Contains(message, "\n") || !strings.Contains(message, tc.says) {
				t.Errorf("the setup failure is %q, want one line holding %q", message, tc.says)
			}
		})
	}
}

// theChecker stands for the deployment's manifest digest (Setting.ManifestSHA256) in an environment whose
// deployment runs a shared leak check: no check of these tests hashes to it.
var theChecker = strings.Repeat("5e", 32)

// A deployment that names no shared leak check (no CheckerVar in the environment) refuses no
// write for the check's absence: with no check named anywhere, a push, a text and a gh call each
// pass the guard with no shared check run, and the deployment's private-name rules (here the
// fixture rule file, withRules) still read each — the gh call's body on standard input among
// them, which the guard reads itself (GhRequest.Sent) and hands back for gh. A broken setup is
// refused whatever the deployment runs, naming the path it is broken at
// (BrokenAt): a check named and not there, a file of the check's name on PATH that is no
// executable (a dangling link, no execute bit), and a pointer file that names nothing or cannot be
// read as a file.
func TestAGuardOfADeploymentThatNamesNoCheckRunsTheRulesAlone(t *testing.T) {
	noCheck := func(t *testing.T, extra ...string) *Guard {
		t.Helper()
		return New(Options{
			Visibility: answers(Public, nil),
			Env:        withRules(t, append(append(fixtureenv.Isolated(t.TempDir()), "PATH="+t.TempDir()), extra...)),
		})
	}
	t.Run("a clean text passes", func(t *testing.T) {
		if err := noCheck(t).Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"}); err != nil {
			t.Fatalf("a clean text = %v, want it cleared", err)
		}
	})
	t.Run("the rules refuse a text", func(t *testing.T) {
		err := noCheck(t).Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a title\na body naming " + leakguardtest.RulesHost + "\n"})
		if refused := refusal(t, err); refused.By != byRules {
			t.Fatalf("the refusal is by %q, want the deployment's private-name rules", refused.By)
		}
	})
	t.Run("a clean gh call passes and its body comes back", func(t *testing.T) {
		const body = "a clean body\n"
		answer, err := noCheck(t).GhCall(bound(t), GhRequest{
			Args: []string{"pr", "create", "--body-file", "-"}, Stdin: strings.NewReader(body), Repo: "invented-org/invented-repo",
		})
		if err != nil || string(answer.Stdin) != body || answer.Read != GhReadWhole {
			t.Fatalf("a clean gh call = %+v, %v; want it cleared, its body handed back whole and the guard run whole", answer, err)
		}
	})
	t.Run("the rules refuse a gh call's body on standard input", func(t *testing.T) {
		for _, req := range []GhRequest{
			{Args: []string{"pr", "create", "--body-file", "-"}},
			{Args: []string{"pr", "create", "--body-file", "-"}, Sent: stdinParse("--body-file", "", StdinText)},
		} {
			req.Stdin, req.Repo = strings.NewReader("a body naming "+leakguardtest.RulesHost+"\n"), "invented-org/invented-repo"
			_, err := noCheck(t).GhCall(bound(t), req)
			if refused := refusal(t, err); refused.By != byRules {
				t.Fatalf("the refusal of %+v is by %q, want the deployment's private-name rules", req, refused.By)
			}
		}
	})
	t.Run("a read is guarded whole", func(t *testing.T) {
		var asked []string
		guard := New(Options{
			Visibility: answers(Public, &asked),
			Env:        withRules(t, append(fixtureenv.Isolated(t.TempDir()), "PATH="+t.TempDir())),
		})
		answer, err := guard.GhCall(bound(t), GhRequest{Args: []string{"pr", "list"}, Repo: "invented-org/invented-repo", Sent: &GhSent{}, WritesNothing: true})
		if err != nil || answer.Read != GhReadWhole || len(asked) != 1 {
			t.Fatalf("a read = %+v, %v after %d visibility reads; want it cleared by the whole guard, one read", answer, err, len(asked))
		}
	})
	t.Run("a push passes and the rules still read it", func(t *testing.T) {
		// The push's records are read with git, so the guard's PATH holds git's own directory
		// and nothing else: no check is on it.
		git, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		push := func(t *testing.T, message string) error {
			t.Helper()
			home, repo := t.TempDir(), filepath.Join(t.TempDir(), "repo")
			fixture := fixturegit.Isolated(t, home)
			fixture.Run(filepath.Dir(repo), fixturegit.InitWorkTree(repo, "main")...)
			fixture.Run(repo, "git", "commit", "--allow-empty", "--quiet", "-m", message)
			head := fixture.Run(repo, "git", "rev-parse", "HEAD")
			return noCheck(t, "PATH="+filepath.Dir(git)).Push(bound(t), PushRequest{
				Dir: repo, Remote: "origin", URL: "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
			})
		}
		if err := push(t, "a commit of nothing in particular"); err != nil {
			t.Fatalf("a clean push = %v, want it cleared", err)
		}
		if refused := refusal(t, push(t, "a commit naming "+leakguardtest.RulesHost)); refused.By != byRules {
			t.Fatalf("the refusal of the push is by %q, want the deployment's private-name rules", refused.By)
		}
	})
	// Neither a check nor a rule file named: every write passes with nothing read, the call's
	// standard input included, which stays the caller's to hand gh.
	t.Run("with no rule file named either, a write passes with nothing read", func(t *testing.T) {
		var asked []string
		guard := New(Options{
			Visibility: answers(Public, &asked),
			Env:        append(fixtureenv.Isolated(t.TempDir()), "PATH="+t.TempDir()),
		})
		if err := guard.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a title naming " + leakguardtest.RulesHost + "\n"}); err != nil {
			t.Fatalf("a text = %v, want it cleared", err)
		}
		answer, err := guard.GhCall(bound(t), GhRequest{
			Args: []string{"pr", "create", "--body-file", "-"}, Stdin: strings.NewReader("a body naming " + leakguardtest.RulesHost + "\n"), Repo: "invented-org/invented-repo",
		})
		if err != nil || answer.Stdin != nil || answer.Read != GhReadNoRules {
			t.Fatalf("a gh call = %+v, %v; want it cleared with no rule named and its standard input never read", answer, err)
		}
		if len(asked) != 0 {
			t.Fatalf("the visibility of %q was read, want none with neither a check nor a rule file named", asked)
		}
	})
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Dir(git)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (extra []string, at string)
	}{
		{"a check named and not there", func(t *testing.T) ([]string, string) {
			gone := filepath.Join(t.TempDir(), checkName)
			return []string{CheckVar + "=" + gone}, gone
		}},
		{"a check named that has no execute bit", func(t *testing.T) ([]string, string) {
			file := filepath.Join(t.TempDir(), checkName)
			if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{CheckVar + "=" + file}, file
		}},
		{"a check named that is a directory", func(t *testing.T) ([]string, string) {
			dir := filepath.Join(t.TempDir(), checkName)
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			return []string{CheckVar + "=" + dir}, dir
		}},
		{"a dangling link of the check's name on PATH", func(t *testing.T) ([]string, string) {
			dir := t.TempDir()
			link := filepath.Join(dir, checkName)
			if err := os.Symlink(filepath.Join(t.TempDir(), "moved"), link); err != nil {
				t.Fatal(err)
			}
			return []string{"PATH=" + dir + ":" + gitDir}, link
		}},
		{"a file of the check's name on PATH with no execute bit", func(t *testing.T) ([]string, string) {
			dir := t.TempDir()
			file := filepath.Join(dir, checkName)
			if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{"PATH=" + dir + ":" + gitDir}, file
		}},
		{"a pointer file that names nothing", func(t *testing.T) ([]string, string) {
			config := t.TempDir()
			pointer := filepath.Join(config, pointerFile)
			if err := os.MkdirAll(filepath.Dir(pointer), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(pointer, []byte("\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{"XDG_CONFIG_HOME=" + config}, pointer
		}},
		{"a pointer file that is a directory", func(t *testing.T) ([]string, string) {
			config := t.TempDir()
			pointer := filepath.Join(config, pointerFile)
			if err := os.MkdirAll(pointer, 0o700); err != nil {
				t.Fatal(err)
			}
			return []string{"XDG_CONFIG_HOME=" + config}, pointer
		}},
		{"a pointer file naming a file with no execute bit", func(t *testing.T) ([]string, string) {
			config := t.TempDir()
			pointer := filepath.Join(config, pointerFile)
			file := filepath.Join(t.TempDir(), checkName)
			if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(pointer), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(pointer, []byte(file+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{"XDG_CONFIG_HOME=" + config}, file
		}},
	} {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			extra, at := tc.setup(t)
			guard := noCheck(t, append([]string{"PATH=" + gitDir}, extra...)...)
			_, resolveErr := guard.Resolve()
			if broken, ok := BrokenAt(resolveErr); !ok || broken != at || NoneNamed(resolveErr) {
				t.Fatalf("Resolve = %v, want a broken setup at %s", resolveErr, at)
			}
			err := guard.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"})
			if failed := setupFailure(t, err); failed.Why != WhyNoCheck || !strings.Contains(failed.Error(), at) {
				t.Fatalf("the text's setup failure is %q, want the broken setup at %s named", failed.Error(), at)
			}
			// A push runs the check through bash, which would run a file the other shapes cannot:
			// it is refused alike, before any record is read.
			home, repo := t.TempDir(), filepath.Join(t.TempDir(), "repo")
			fixture := fixturegit.Isolated(t, home)
			fixture.Run(filepath.Dir(repo), fixturegit.InitWorkTree(repo, "main")...)
			fixture.Run(repo, "git", "commit", "--allow-empty", "--quiet", "-m", "a commit of nothing in particular")
			head := fixture.Run(repo, "git", "rev-parse", "HEAD")
			err = guard.Push(bound(t), PushRequest{
				Dir: repo, Remote: "origin", URL: "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
			})
			if failed := setupFailure(t, err); failed.Why != WhyNoCheck || !strings.Contains(failed.Error(), at) {
				t.Fatalf("the push's setup failure is %q, want the broken setup at %s named", failed.Error(), at)
			}
		})
	}
	t.Run("a file of the check's name on PATH that does not run hides no pointer file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join(t.TempDir(), "moved"), filepath.Join(dir, checkName)); err != nil {
			t.Fatal(err)
		}
		good := leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes)
		config := t.TempDir()
		pointer := filepath.Join(config, pointerFile)
		if err := os.MkdirAll(filepath.Dir(pointer), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pointer, []byte(good+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		guard := noCheck(t, "PATH="+dir+":"+gitDir, "XDG_CONFIG_HOME="+config)
		if check, err := guard.Resolve(); err != nil || check != good {
			t.Fatalf("Resolve = %q, %v; want the check the pointer file names, %s", check, err, good)
		}
		if err := guard.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"}); err != nil {
			t.Fatalf("a clean text = %v, want it cleared by the check the pointer file names", err)
		}
	})
}

// Under a deployment's manifest digest (CheckerVar) a write runs only under the check that digest
// names: a check whose manifest (the launcher script, the program, the README and the must-pass
// lines beside it) has that digest runs and clears the write. One whose program is another, one
// whose launcher script is edited to `exit 0` beside an untouched program, and one missing a file
// of the manifest are each refused as whyOtherCheck naming the digest and what is there, and the
// check is never run. With no digest told, the same checks run, as they did before the setting.
func TestAGuardUnderACheckerRunsOnlyTheCheckItNames(t *testing.T) {
	const otherProgram = "# a check of another version\n"
	launcher := leakguardtest.Recording(leakguardtest.Passes)
	install := func(t *testing.T, launcher, program string, drop string) string {
		t.Helper()
		dir := t.TempDir()
		check := leakguardtest.Write(t, dir, launcher)
		leakguardtest.Companions(t, dir, program)
		if drop != "" {
			if err := os.Remove(filepath.Join(dir, drop)); err != nil {
				t.Fatal(err)
			}
		}
		return check
	}
	digestOf := func(t *testing.T, check string) string {
		t.Helper()
		digest, err := manifestSHA256(check)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	deployments := digestOf(t, install(t, launcher, leakguardtest.Program, ""))
	guard := func(t *testing.T, check, checker string) (*Guard, string) {
		t.Helper()
		record := filepath.Join(t.TempDir(), "record")
		env := append(fixtureenv.Isolated(t.TempDir()), "LEAK_CHECK_RECORD="+record)
		if checker != "" {
			env = append(env, CheckerVar+"="+checker)
		}
		return New(Options{Check: check, Visibility: answers(Public, nil), Env: env}), record
	}
	ran := func(t *testing.T, record string) bool {
		t.Helper()
		got, err := os.ReadFile(record)
		return err == nil && len(got) > 0
	}
	text := TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"}
	for _, tc := range []struct {
		name, program, checker string
	}{
		{name: "the deployment's own check", program: leakguardtest.Program, checker: deployments},
		{name: "no digest told and no program", program: "", checker: ""},
		{name: "no digest told and another program", program: otherProgram, checker: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, record := guard(t, install(t, launcher, tc.program, ""), tc.checker)
			if err := g.Text(bound(t), text); err != nil || !ran(t, record) {
				t.Fatalf("a clean text = %v, check ran %t; want it cleared by the check", err, ran(t, record))
			}
		})
	}
	for _, tc := range []struct {
		name     string
		launcher string
		program  string
		drop     string
		readable bool
	}{
		{name: "a program of another digest", launcher: launcher, program: otherProgram, readable: true},
		{name: "a launcher edited to exit 0 beside an untouched program", launcher: leakguardtest.Recording("exit 0\n"), program: leakguardtest.Program, readable: true},
		{name: "no program beside the check", launcher: launcher, program: ""},
		{name: "no must-pass lines beside the check", launcher: launcher, program: leakguardtest.Program, drop: leakguardtest.MustPassName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, shape := range []struct {
				name  string
				write func(*Guard) error
			}{
				{"a text", func(g *Guard) error { return g.Text(bound(t), text) }},
				{"a gh call", func(g *Guard) error {
					_, err := g.GhCall(bound(t), GhRequest{Args: []string{"pr", "list"}, Repo: "invented-org/invented-repo", Sent: &GhSent{}})
					return err
				}},
			} {
				check := install(t, tc.launcher, tc.program, tc.drop)
				want := []string{CheckerVar + "=" + deployments, "could not be read"}
				if tc.readable {
					want = []string{CheckerVar + "=" + deployments, "has the manifest digest " + digestOf(t, check)}
				}
				g, record := guard(t, check, deployments)
				failed := setupFailure(t, shape.write(g))
				if failed.Why != whyOtherCheck {
					t.Errorf("%s: the setup failure is %q, want %q", shape.name, failed.Error(), whyOtherCheck)
				}
				for _, w := range want {
					if !strings.Contains(failed.Error(), w) {
						t.Errorf("%s: the setup failure %q does not name %q", shape.name, failed.Error(), w)
					}
				}
				if ran(t, record) {
					t.Errorf("%s: the check ran, want it never run under another digest", shape.name)
				}
			}
		})
	}
	// A check that runs a file the manifest does not cover is refused, though the four files beside
	// what runs are the deployment's own: a public-leak-check.sh linked to a `pass.sh` beside copies
	// of the real files, and a check named as `other.sh` beside them. The digest holds the file that
	// runs, not only the files beside it.
	for _, tc := range []struct {
		name  string
		check func(t *testing.T) string
	}{
		{"a launcher linked out beside copies", func(t *testing.T) string {
			copies := t.TempDir()
			leakguardtest.Write(t, copies, launcher)
			leakguardtest.Companions(t, copies, leakguardtest.Program)
			pass := filepath.Join(copies, "pass.sh")
			if err := os.WriteFile(pass, []byte(leakguardtest.Recording("exit 0\n")), 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(t.TempDir(), checkName)
			if err := os.Symlink(pass, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
		{"a check of another name beside copies", func(t *testing.T) string {
			copies := t.TempDir()
			leakguardtest.Write(t, copies, launcher)
			leakguardtest.Companions(t, copies, leakguardtest.Program)
			other := filepath.Join(copies, "other.sh")
			if err := os.WriteFile(other, []byte(leakguardtest.Recording("exit 0\n")), 0o755); err != nil {
				t.Fatal(err)
			}
			return other
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, record := guard(t, tc.check(t), deployments)
			failed := setupFailure(t, g.Text(bound(t), text))
			if failed.Why != whyOtherCheck || !strings.Contains(failed.Error(), "which is not the "+checkName+" its manifest covers") {
				t.Errorf("the setup failure is %q, want %q naming the file that runs", failed.Error(), whyOtherCheck)
			}
			if ran(t, record) {
				t.Errorf("the check ran, want it never run")
			}
		})
	}
}

// The manifest digest is what the check's own install procedure records and an operator computes
// with one shell line: `(cd <dir> && sha256sum public-leak-check.sh public_leak_check.py README.md
// must-pass-lines.txt) | sha256sum`. sha256sumLineDigest is a portable oracle for those exact
// lines, so this test runs on a host with no sha256sum binary and still holds the Go digest to the
// documented formula; a reordering of the files, a missing file and an edit of any one of them
// each change the digest or refuse it.
func TestTheManifestDigestIsWhatSha256sumPrintsOfTheCheck(t *testing.T) {
	dir := t.TempDir()
	check := leakguardtest.Write(t, dir, leakguardtest.Passes)
	leakguardtest.Companions(t, dir, leakguardtest.Program)
	oracle := sha256sumLineDigest(t, dir, ManifestFiles)
	got, err := manifestSHA256(check)
	if err != nil || got != oracle {
		t.Fatalf("manifestSHA256 = %q, %v; want %q, what sha256sum prints", got, err, oracle)
	}
	if !slices.Equal(ManifestFiles, []string{"public-leak-check.sh", "public_leak_check.py", "README.md", "must-pass-lines.txt"}) {
		t.Fatalf("ManifestFiles = %q, want the install procedure's home set in its order", ManifestFiles)
	}
	reordered := []string{ManifestFiles[1], ManifestFiles[0], ManifestFiles[2], ManifestFiles[3]}
	if other := sha256sumLineDigest(t, dir, reordered); other == oracle {
		t.Fatalf("the reordered sha256sum lines digest to %s, want a digest other than %s", other, oracle)
	}
	for _, name := range ManifestFiles {
		t.Run("an edit of "+name, func(t *testing.T) {
			edited := t.TempDir()
			leakguardtest.Write(t, edited, leakguardtest.Passes)
			leakguardtest.Companions(t, edited, leakguardtest.Program)
			f, err := os.OpenFile(filepath.Join(edited, name), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("# an edit\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if other, err := manifestSHA256(filepath.Join(edited, checkName)); err != nil || other == oracle {
				t.Fatalf("an edit of %s left the digest %q (%v), want another", name, other, err)
			}
		})
	}
	if err := os.Remove(filepath.Join(dir, ManifestFiles[len(ManifestFiles)-1])); err != nil {
		t.Fatal(err)
	}
	if _, err := manifestSHA256(check); err == nil {
		t.Fatal("manifestSHA256 succeeded with a manifest file missing")
	}
}

func sha256sumLineDigest(t *testing.T, dir string, files []string) string {
	t.Helper()
	var lines strings.Builder
	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		if _, err := fmt.Fprintf(&lines, "%x  %s\n", sum, name); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256([]byte(lines.String()))
	return fmt.Sprintf("%x", sum)
}

// A refusal written to a log or a terminal writes no character of a path that a terminal could act
// on or show as something else: a path is the push author's choice, and an escape sequence in it
// (C0 or C1) would act on the terminal that shows the refusal, erasing the line that names the
// file, a line break would start a line the guard did not write, and U+202E would reverse how the
// rest of the path reads. The report's lines show the path as shownText shows a label: each such
// character and a byte that is not UTF-8 as Go writes it in a quoted string, and a backslash
// doubled, so that one of the path reads apart from an escape. Error then escapes each character
// still raw (termsafe.Escaped), none here, and writes a backslash as it is, so those lines, and
// the shared check's already escaped ones, pass unchanged.
func TestARefusalWritesNoControlByteOfAPathRaw(t *testing.T) {
	const text = "the runner answers at " + leakguardtest.RulesHost
	labels := []struct{ label, shown string }{
		{"a b\x1b[2Kx.md", `a b\x1b[2Kx.md`},
		{"c\nLEAK CHECK: clean\x7f.md", `c\nLEAK CHECK: clean\x7f.md`},
		{"c1\u009b2Kz.md", `c1\u009b2Kz.md`},
		{"bidi\u202edm.x.txt", `bidi\u202edm.x.txt`},
		{`back\x1b.md`, `back\\x1b.md`},
		{"bad\xffz.md", `bad\xffz.md`},
		{"tag\U000e0041.md", `tag\U000e0041.md`},
	}
	records := make([]Record, 0, len(labels))
	lines := make([]string, 0, len(labels))
	for _, label := range labels {
		records = append(records, Record{Label: label.label, Text: text})
		lines = append(lines, leakguardtest.ReportLine(label.shown, leakguardtest.RulesHostClass, leakguardtest.RulesHost, 1, text))
	}
	refused := refusal(t, fixtureRules(t).Check(bound(t), "the write", records...))
	if !slices.Equal(refused.Lines, lines) {
		t.Fatalf("the report is %q, want %q", refused.Lines, lines)
	}
	written := refused.Error()
	for _, line := range lines {
		if !strings.Contains(written, "\n  "+line) {
			t.Errorf("the refusal is %q, want it to hold %q", written, "\n  "+line)
		}
	}
	if !utf8.ValidString(written) || strings.Count(written, "\n") != len(refused.Lines) {
		t.Errorf("the refusal writes a byte that is not UTF-8 or a line of its own: %q", written)
	}
	for _, r := range written {
		if r != '\n' && !unicode.IsPrint(r) {
			t.Errorf("the refusal writes %U raw: %q", r, written)
		}
	}
}
