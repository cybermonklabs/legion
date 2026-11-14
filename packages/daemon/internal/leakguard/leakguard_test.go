package leakguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
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
	buffered, err := guard.GhCall(bound(t), GhRequest{
		Args:  []string{"pr", "create", "--body-file", "-"},
		Stdin: strings.NewReader(body),
		Repo:  "invented-org/invented-repo",
	})
	if err != nil {
		t.Fatalf("a clean call: %v", err)
	}
	if string(buffered) != body {
		t.Errorf("the caller got %q back, want the bytes it gave", buffered)
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
	buffered, err := guard.GhCall(bound(t), GhRequest{
		Args:  []string{"pr", "create", "--body-file", "-"},
		Stdin: strings.NewReader(body),
		Repo:  "invented-org/invented-repo",
	})
	if string(buffered) != body {
		t.Errorf("the caller got %q back, want the bytes it gave", buffered)
	}
	refused := refusal(t, err)
	if want := []string{"the gh call's standard input: " + theClass}; !slices.Equal(refused.Lines, want) {
		t.Errorf("the report is %q, want %q", refused.Lines, want)
	}
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

// A public visibility answer comes from GitHub once for a role's state directory, and a failed
// read leaves no answer behind for a later call to mistake for a completed check.
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

// A push asks the same role-state visibility cache a preceding guarded call populated, so the
// shared check still runs while the destination is not read from GitHub twice.
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
	if reads != 1 {
		t.Errorf("the visibility was read %d times, want once", reads)
	}
}

// The two shared-check failures: no check resolves at all, and a check that exits 2. Each refuses
// the write, and each says it is a failure of setup rather than of the text.
func TestTheSharedChecksFailuresAreSetupFailures(t *testing.T) {
	t.Run("no check resolves", func(t *testing.T) {
		// An environment with no PUBLIC_LEAK_CHECK, a PATH with no check on it, and a
		// configuration home holding no pointer file: the three places Resolve looks.
		guard := New(Options{
			Visibility: answers(Public, nil),
			Env:        append(fixtureenv.Isolated(t.TempDir()), "PATH="+t.TempDir()),
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
