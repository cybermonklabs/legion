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

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// theHost is the one refusable literal these tests need: a Tailscale MagicDNS host built out of
// invented words, which names no machine and no tailnet. It is spelled in two pieces so that this
// file itself passes .github/scripts/check-private-names.sh, the way private-names.toml spells
// every literal of its own patterns; joining the pieces here would put a host the rules refuse
// into a tracked file of a public repository.
const theHost = "wobble-fern.quiet-harbor.ts" + ".net"

// theClass is what the rules call that host: the description of tailnet_host_rule, which is what
// a report names and the whole of what it says about the text.
const theClass = "names a Tailscale MagicDNS host"

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
// workspace but the test's (fixtureenv.Isolated), with extra on top.
func guardWith(t *testing.T, check string, visibility Visibility, extra ...string) *Guard {
	t.Helper()
	return New(Options{
		Check:      check,
		Visibility: answers(visibility, nil),
		Env:        append(fixtureenv.Isolated(t.TempDir()), extra...),
	})
}

// bound is a context every call of these tests runs under, so that a stand-in that hangs fails
// the test rather than the package's ten minutes.
func bound(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// refusal is err as a *Refusal, failing the test where it is anything else, and failing it where
// the message holds the text that was refused: a report names the record and the class, and the
// text it matched is exactly what must not travel out of the process that read it.
func refusal(t *testing.T, err error) *Refusal {
	t.Helper()
	var refused *Refusal
	if !errors.As(err, &refused) {
		t.Fatalf("got %v, want a refusal", err)
	}
	if strings.Contains(err.Error(), theHost) {
		t.Fatalf("the refusal holds the text it refused: %v", err)
	}
	return refused
}

// wantRefused fails the test unless err is the refusal whose report is want (refusal), or, where
// want is nil, unless the write passed.
func wantRefused(t *testing.T, err error, want []string) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Fatalf("got %v, want the write passed", err)
		}
		return
	}
	if refused := refusal(t, err); !slices.Equal(refused.Lines, want) {
		t.Errorf("the report is %q, want %q", refused.Lines, want)
	}
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

// wantSetupFailure fails the test unless err is a setup failure (setupFailure) whose message is
// want.
func wantSetupFailure(t *testing.T, err error, want string) {
	t.Helper()
	setupFailure(t, err)
	if err.Error() != want {
		t.Errorf("the failure is\n  %s\nwant\n  %s", err, want)
	}
}

// Every entry point runs its own shape of the shared check and refuses what that check refuses,
// and every one of them refuses a record of its own that names something private. Rules is the
// one that runs no check at all: it refuses and passes under a stand-in that refuses everything,
// which is how this says that the daemon's branch create reaches no check.
func TestEachEntryPointRefusesWhatTheCheckRefusesAndWhatTheRulesMatch(t *testing.T) {
	cleanPush := PushRequest{
		Dir:     t.TempDir(),
		Remote:  "origin",
		URL:     "https://github.com/invented-org/invented-repo.git",
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: noSHA, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
	}
	namedPush := cleanPush
	namedPush.Updates = []Update{{LocalRef: "refs/heads/work", LocalSHA: noSHA, RemoteRef: "refs/heads/" + theHost, RemoteSHA: noSHA}}
	for _, entry := range []struct {
		name string
		// checked runs the entry point against guard, with a record that names something
		// private when named.
		checked func(ctx context.Context, guard *Guard, named bool) error
		// readsCheck is whether the entry point runs the shared check at all.
		readsCheck bool
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
			readsCheck: true,
		},
		{
			name: "GhCall",
			checked: func(ctx context.Context, guard *Guard, named bool) error {
				args := []string{"pr", "create", "--title", "a clean title"}
				if named {
					args = []string{"pr", "create", "--title", "a title naming " + theHost}
				}
				_, err := guard.GhCall(ctx, GhRequest{Args: args, Repo: "invented-org/invented-repo"})
				return err
			},
			readsCheck: true,
		},
		{
			name: "Text",
			checked: func(ctx context.Context, guard *Guard, named bool) error {
				text := "a clean title\n\na clean body\n"
				if named {
					text = "a clean title\n\na body naming " + theHost + "\n"
				}
				return guard.Text(ctx, TextRequest{Repo: "invented-org/invented-repo", Text: text})
			},
			readsCheck: true,
		},
		{
			name: "Rules",
			checked: func(ctx context.Context, guard *Guard, named bool) error {
				ref := "legion/INVENTED-1"
				if named {
					ref = "legion/" + theHost
				}
				return guard.Rules(ctx, RefRecord(ref))
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
			if len(refused.Lines) != 1 || !strings.HasSuffix(refused.Lines[0], ": "+theClass) {
				t.Errorf("the report is %q, want one line naming the class", refused.Lines)
			}
			byCheck := guardWith(t, leakguardtest.Write(t, t.TempDir(), refuses), Public)
			err := entry.checked(ctx, byCheck, false)
			if !entry.readsCheck {
				if err != nil {
					t.Fatalf("a clean write reached the shared check: %v", err)
				}
				return
			}
			if refused := refusal(t, err); refused.By != bySharedCheck {
				t.Errorf("a write the check refused was refused by %q, want the shared check", refused.By)
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
	body := "a body naming " + theHost + "\n"
	answer, err := guard.GhCall(bound(t), GhRequest{
		Args:  []string{"pr", "create", "--body-file", "-"},
		Stdin: strings.NewReader(body),
		Repo:  "invented-org/invented-repo",
	})
	if string(answer.Stdin) != body {
		t.Errorf("the caller got %q back, want the bytes it gave", answer.Stdin)
	}
	wantRefused(t, err, []string{"the gh call's standard input: " + theClass})
}

// The report names the record and the class and nothing else: not the text that matched, not the
// component of the path that matched, and nothing the engine itself printed.
func TestTheReportNamesTheRecordAndTheClass(t *testing.T) {
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	err := guard.Rules(bound(t), Record{Label: "commit 1a2b3c4 message", Text: "deployed from " + theHost + " this morning"})
	refused := refusal(t, err)
	want := []string{"commit 1a2b3c4 message: " + theClass}
	if !slices.Equal(refused.Lines, want) {
		t.Fatalf("the report is %q, want %q", refused.Lines, want)
	}
	if message := refused.Error(); !strings.Contains(message, "LEAK CHECK: refusing") || !strings.Contains(message, want[0]) {
		t.Errorf("the message is %q, want the shared check's own refusing line and the report", message)
	}
}

// A path the rules match is reported with the component they matched replaced by <name>, the
// masking .github/scripts/check-private-names.sh does, so that a report of a private name does
// not carry that name.
func TestTheReportMasksAPathComponent(t *testing.T) {
	// The component is built out of two pieces for the same reason theHost is: spelled whole, it
	// would be a name this repository's own check refuses in this very file.
	path := "packages/" + "agen" + "t-c/main.go"
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	// A path record is read twice over, as a path by the path rules and as a text by the content
	// rules, so a path that names the deployment repository earns both of that class's wordings.
	refused := refusal(t, guard.Rules(bound(t), Record{Label: path, Text: path}))
	const class = "names the private deployment repository or its Dispatch project key"
	want := []string{"packages/<name>/main.go: " + class, "packages/<name>/main.go: " + class + " in its path"}
	if !slices.Equal(refused.Lines, want) {
		t.Fatalf("the report is %q, want %q", refused.Lines, want)
	}
	if strings.Contains(refused.Error(), "t-c/") {
		t.Errorf("the report holds the component it masked: %q", refused.Error())
	}
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
		Env: fixtureenv.Isolated(t.TempDir()),
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
				Env:        append(fixtureenv.Isolated(t.TempDir()), "LEAK_CHECK_RECORD="+record),
			})
			text := "a title naming " + theHost + "\n"
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
		Env:        env,
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
		if failed.Why != whyNoCheck {
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

// theChecker stands for the deployment's manifest digest (Setting.ManifestSHA256) in an environment whose
// deployment runs a shared leak check: no check of these tests hashes to it.
var theChecker = strings.Repeat("5e", 32)

// A deployment that names no shared leak check (no CheckerVar in the environment) refuses no
// write for the check's absence: with no check named anywhere, a push, a text and a gh call each
// pass the guard with no shared check run, and this repository's own rules still read each — the
// gh call's body on standard input among them, which the guard buffers itself where no check's
// handshake asks for it, and hands back for gh. A broken setup is refused whatever the deployment
// runs, naming the path it is broken at (BrokenAt): a check named and not there, a file of the
// check's name on PATH that is no executable (a dangling link, no execute bit), and a pointer file
// that names nothing or cannot be read as a file.
func TestAGuardOfADeploymentThatNamesNoCheckRunsTheRulesAlone(t *testing.T) {
	noCheck := func(t *testing.T, extra ...string) *Guard {
		t.Helper()
		return New(Options{
			Visibility: answers(Public, nil),
			Env:        append(append(fixtureenv.Isolated(t.TempDir()), "PATH="+t.TempDir()), extra...),
		})
	}
	t.Run("a clean text passes", func(t *testing.T) {
		if err := noCheck(t).Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"}); err != nil {
			t.Fatalf("a clean text = %v, want it cleared", err)
		}
	})
	t.Run("the rules refuse a text", func(t *testing.T) {
		err := noCheck(t).Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a title\na body naming " + theHost + "\n"})
		if refused := refusal(t, err); refused.By != byRules {
			t.Fatalf("the refusal is by %q, want this repository's own rules", refused.By)
		}
	})
	t.Run("a clean gh call passes and its body comes back", func(t *testing.T) {
		const body = "a clean body\n"
		answer, err := noCheck(t).GhCall(bound(t), GhRequest{
			Args: []string{"pr", "create", "--body-file", "-"}, Stdin: strings.NewReader(body), Repo: "invented-org/invented-repo",
		})
		if err != nil || string(answer.Stdin) != body || answer.CheckedOnly {
			t.Fatalf("a clean gh call = %+v, %v; want it cleared, its body handed back whole and not checked-only", answer, err)
		}
	})
	t.Run("the rules refuse a gh call's body on standard input", func(t *testing.T) {
		for _, req := range []GhRequest{
			{Args: []string{"pr", "create", "--body-file", "-"}},
			{Args: []string{"pr", "create", "--body-file", "-"}, Files: []GhFile{}, StdinSent: true},
		} {
			req.Stdin, req.Repo = strings.NewReader("a body naming "+theHost+"\n"), "invented-org/invented-repo"
			_, err := noCheck(t).GhCall(bound(t), req)
			if refused := refusal(t, err); refused.By != byRules {
				t.Fatalf("the refusal of %+v is by %q, want this repository's own rules", req, refused.By)
			}
		}
	})
	t.Run("a read is guarded whole", func(t *testing.T) {
		var asked []string
		guard := New(Options{
			Visibility: answers(Public, &asked),
			Env:        append(fixtureenv.Isolated(t.TempDir()), "PATH="+t.TempDir()),
		})
		answer, err := guard.GhCall(bound(t), GhRequest{Args: []string{"pr", "list"}, Repo: "invented-org/invented-repo", Files: []GhFile{}, WritesNothing: true})
		if err != nil || answer.CheckedOnly || len(asked) != 1 {
			t.Fatalf("a read = %+v, %v after %d visibility reads; want it cleared by the whole guard, one read", answer, err, len(asked))
		}
	})
	t.Run("a push passes and the rules still read it", func(t *testing.T) {
		// The push's records are read with git, so the guard's PATH holds git's own directory
		// and nothing else: no check is on it. The destination is an empty local bare repository,
		// which the guard reads what it holds from with git ls-remote (Guard.advertised), and which
		// names no repository GitHub serves, so the rules read the push as they read one to a
		// public destination (scanThrough).
		git, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		push := func(t *testing.T, message string) error {
			t.Helper()
			home, repo := t.TempDir(), filepath.Join(t.TempDir(), "repo")
			destination := filepath.Join(t.TempDir(), "destination.git")
			fixture := fixturegit.Isolated(t, home)
			fixture.Run(filepath.Dir(repo), fixturegit.InitWorkTree(repo, "main")...)
			fixture.Run(repo, fixturegit.InitBare(destination, "main")...)
			fixture.Run(repo, "git", "commit", "--allow-empty", "--quiet", "-m", message)
			head := fixture.Run(repo, "git", "rev-parse", "HEAD")
			return noCheck(t, "PATH="+filepath.Dir(git)).Push(bound(t), PushRequest{
				Dir: repo, Remote: "origin", URL: destination,
				Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
			})
		}
		if err := push(t, "a commit of nothing in particular"); err != nil {
			t.Fatalf("a clean push = %v, want it cleared", err)
		}
		if refused := refusal(t, push(t, "a commit naming "+theHost)); refused.By != byRules {
			t.Fatalf("the refusal of the push is by %q, want this repository's own rules", refused.By)
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
			if failed := setupFailure(t, err); failed.Why != whyNoCheck || !strings.Contains(failed.Error(), at) {
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
			if failed := setupFailure(t, err); failed.Why != whyNoCheck || !strings.Contains(failed.Error(), at) {
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
					_, err := g.GhCall(bound(t), GhRequest{Args: []string{"pr", "list"}, Repo: "invented-org/invented-repo", Files: []GhFile{}})
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
