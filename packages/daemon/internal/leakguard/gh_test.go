package leakguard

//ci:reads packages/daemon/cmd/legion/testdata/gh-help/api.txt TestGhLeafOptionTablesAreTheirHelps reads ../../cmd/legion/testdata/gh-help/<help>, api.txt for gh api
//ci:reads packages/daemon/cmd/legion/testdata/gh-help/workflow_run.txt TestGhLeafOptionTablesAreTheirHelps reads ../../cmd/legion/testdata/gh-help/<help>, workflow_run.txt for gh workflow run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// theProgram is the public_leak_check.py of a stand-in check these tests count as proven: its
// sha256 (hexSHA256) is the value a caller names in GhRequest.ProvenCheck.
const theProgram = "# stands for the shared check a read rule was proven against\n"

// A call its caller found writes nothing (GhRequest.WritesNothing), under the check its caller's
// rule was proven against (GhRequest.ProvenCheck), runs the shared check and nothing more. A check
// that clears it clears it with no visibility read and no private-name pass, though its argv
// names a host the rules refuse: a read leaves no text in any repository. A check that refuses
// it, one that cannot finish, and one its bound stops refuse it as they refuse any call. Under a
// check that is not the proven one — its program another file, or no file — and as a write, the
// same call reads the destination's visibility and is refused by the rules.
func TestAGhCallThatWritesNothingRunsTheSharedCheckAlone(t *testing.T) {
	const repo = "invented-org/invented-repo"
	args := []string{"search", "code", leakguardtest.RulesHost}
	proven := hexSHA256(theProgram)
	// guardFor is a guard under check, with program beside it, and the destinations its
	// visibility was asked of; call is its GhCall under ctx.
	guardFor := func(t *testing.T, check, program string) (*Guard, *[]string) {
		t.Helper()
		asked := new([]string)
		script := leakguardtest.Write(t, t.TempDir(), check)
		if program != "" {
			if err := os.WriteFile(filepath.Join(filepath.Dir(script), checkProgram), []byte(program), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return New(Options{
			Check:      script,
			Visibility: answers(Public, asked),
			Env:        withRules(t, fixtureenv.Isolated(t.TempDir())),
		}), asked
	}
	ghCall := func(guard *Guard, ctx context.Context, writesNothing bool) (GhAnswer, error) {
		return guard.GhCall(ctx, GhRequest{Args: args, Repo: repo, Sent: &GhSent{}, WritesNothing: writesNothing, ProvenCheck: proven})
	}
	call := func(t *testing.T, ctx context.Context, check, program string, writesNothing bool) ([]string, GhAnswer, error) {
		t.Helper()
		guard, asked := guardFor(t, check, program)
		answer, err := ghCall(guard, ctx, writesNothing)
		return *asked, answer, err
	}
	t.Run("a read the check clears", func(t *testing.T) {
		if asked, answer, err := call(t, bound(t), passes, theProgram, true); err != nil || len(asked) != 0 || answer.Read != GhReadCheckedOnly {
			t.Fatalf("GhCall = %+v, %v, visibility asked of %q; want the read cleared by the check alone", answer, err, asked)
		}
	})
	t.Run("a read the check refuses", func(t *testing.T) {
		asked, _, err := call(t, bound(t), refuses, theProgram, true)
		refusal(t, err)
		if len(asked) != 0 {
			t.Fatalf("visibility asked of %q for a read the check refused", asked)
		}
	})
	t.Run("a read whose check cannot finish", func(t *testing.T) {
		_, _, err := call(t, bound(t), cannotFinish, theProgram, true)
		setupFailure(t, err)
	})
	t.Run("a read whose check runs past its bound", func(t *testing.T) {
		const theBound = 2 * time.Second
		guard, asked := guardFor(t, "cat >/dev/null 2>&1\nexec sleep 86400\n", theProgram)
		ctx, cancel := context.WithTimeout(context.Background(), theBound)
		defer cancel()
		var err error
		started := time.Now()
		answered(t, "the read whose check never answers", func() { _, err = ghCall(guard, ctx, true) })
		// The stop is the caller's own deadline, not a bound of the guard's shorter than it: a
		// bound the guard set itself would end the check, and the call, while ctx is still alive,
		// and stoppedByItsBound would read its shorter seconds as a late start.
		if ctx.Err() == nil {
			t.Fatalf("the call answered %s after it began, inside the caller's %s: a bound shorter than the caller's stopped the check",
				time.Since(started).Round(time.Millisecond), theBound)
		}
		stoppedByItsBound(t, setupFailure(t, err), theBound)
		if len(*asked) != 0 {
			t.Fatalf("visibility asked of %q for a read whose check was stopped", *asked)
		}
	})
	for _, tc := range []struct {
		name          string
		program       string
		writesNothing bool
	}{
		{"a read under a check whose program is another file", theProgram + "# an edit\n", true},
		{"a read under a check with no program beside it", "", true},
		{"the same call as a write", theProgram, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked, answer, err := call(t, bound(t), passes, tc.program, tc.writesNothing)
			if report := refusal(t, err); len(report.Lines) != 1 || !namesClass(report.Lines[0], leakguardtest.RulesHostClass) {
				t.Fatalf("the refusal does not name the class: %v", report)
			}
			if !slices.Equal(asked, []string{repo}) || answer.Read != GhReadWhole {
				t.Fatalf("visibility asked of %q and the guard read %v, want %q asked once and the whole guard", asked, answer.Read, repo)
			}
		})
	}
}

// theStop is the sentence a setup failure says of a shared check its context stopped
// (stoppedError.said): the whole seconds the check ran, then the bound it was given or the cancel.
var theStop = regexp.MustCompile(`^the shared public leak check ran (\d+) s and was stopped by (?:its bound of (\d+) s|a cancel)$`)

// sharedStop is the shared check's stop as failed says it, failing t where failed says something
// else: the whole seconds the check ran, and the bound it was given, or a cancel where byBound is
// false.
func sharedStop(t *testing.T, failed *SetupFailure) (ran, bound int64, byBound bool) {
	t.Helper()
	said := theStop.FindStringSubmatch(failed.Why)
	if said == nil {
		t.Fatalf("the setup failure says %q, want the shared check stopped by its bound or a cancel", failed.Error())
	}
	ran, err := strconv.ParseInt(said[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if said[2] == "" {
		return ran, 0, false
	}
	if bound, err = strconv.ParseInt(said[2], 10, 64); err != nil {
		t.Fatal(err)
	}
	return ran, bound, true
}

// stoppedByItsBound fails t unless failed is the shared check stopped by its bound, said with the
// seconds the guard counted: a bound no longer than ceiling, the caller's own, and a run no shorter
// than that bound, since the check is stopped at its deadline and never before. The bound is what
// was left of the caller's deadline when the check started (Guard.run), so on a loaded machine,
// where the guard's own work before the check takes longer, it is less than the caller's whole
// bound: it is read against the ceiling, as stopBound reads a child's, and never for its exact
// seconds.
func stoppedByItsBound(t *testing.T, failed *SetupFailure, ceiling time.Duration) {
	t.Helper()
	ran, bound, byBound := sharedStop(t, failed)
	if !byBound || bound > wholeSeconds(ceiling) || ran < bound {
		t.Fatalf("the setup failure says %q, want the shared check stopped by a bound no longer than the caller's %d s, after a run no shorter than its bound",
			failed.Error(), wholeSeconds(ceiling))
	}
}

// stoppedByACancel fails t unless failed is the shared check stopped by a cancel, said with a run
// no shorter than atLeast, the time the check had run when the cancel came. The run is measured
// from the check's start to the end of its stop, and on a loaded machine the stop takes longer
// after the cancel, so it is read against that floor and never for its exact seconds.
func stoppedByACancel(t *testing.T, failed *SetupFailure, atLeast time.Duration) {
	t.Helper()
	ran, _, byBound := sharedStop(t, failed)
	if byBound || ran < wholeSeconds(atLeast) {
		t.Fatalf("the setup failure says %q, want the shared check stopped by a cancel after a run no shorter than the %d s it had run when the cancel came",
			failed.Error(), wholeSeconds(atLeast))
	}
}

// hexSHA256 is the sha256 of text, in hex.
func hexSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// A gh call that carries its body in a file publishes the file's text, not the file's name, and
// the deployment's private-name rules must read that text.
//
// The shared public leak check already does: measured at leak-check-v15 from a pod on
// 2026-11-10, `public-leak-check.sh gh-guard pr comment 1 --body-file <file>` over a file holding
// a credential exits 3, so the check opens the file a body option names (its own BODY_FILES, and
// the `-F <key>=@<path>` form `gh api` takes). What it does not read is a deployment's private
// names: with PUBLIC_LEAK_CHECK_REPO_NAMES and PUBLIC_LEAK_CHECK_INTERNAL off, which R6 of
// dispatch://INFRA-1527 keeps them, the same call over a file holding a Tailscale MagicDNS host
// exits 0. So for those names the rule file is the whole of the cover, and a body option's file is
// a hole in it: the same host inline in `--body` is refused.
//
// This is the surface the issue was written for — 43 of its 60 measured refusals are text an
// agent posted to GitHub — and a file is how a Legion role posts it:
// skills/legion-worker/references/pr-body.md writes a pull request's body back with
// `gh api --method PATCH repos/<owner>/<repo>/pulls/<n> -F body=@<file>`.
func TestAGhCallsBodyFileIsRead(t *testing.T) {
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("the box is at "+leakguardtest.RulesHost+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "--body-file", args: []string{"pr", "comment", "1", "--body-file", body}},
		{name: "--notes-file", args: []string{"release", "create", "v0", "--notes-file", body}},
		{name: "-F body=@", args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "--method", "PATCH", "-F", "body=@" + body}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			_, err := guard.GhCall(bound(t), GhRequest{Args: tc.args, Repo: "invented-org/invented-repo"})
			if err == nil {
				t.Fatal("the call was cleared, so the file's text reaches GitHub unread")
			}
			report := refusal(t, err)
			if !strings.Contains(report.Error(), leakguardtest.RulesHostClass) {
				t.Fatalf("the refusal does not name the class: %v", report)
			}
		})
	}
}

// A file a body option names that the guard cannot read is a setup failure naming the option,
// never a call passed through: a guard that did not read what the call publishes has cleared
// nothing, and a pass here would be the hole the test above closes by another door. The bound on
// the read (maxGhFile) is the second door: a file one byte past it is refused rather than pulled
// into memory, and that failure alone says the file is more than the guard reads of what a call
// sends, which a file that cannot be opened does not.
func TestAGhCallsBodyFileThatCannotBeReadIsASetupFailure(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "absent.md")
	huge := filepath.Join(dir, "huge.md")
	if err := os.WriteFile(huge, bytes.Repeat([]byte("a"), maxGhFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		args   []string
		want   string
		longer bool
	}{
		{
			name: "a file that is not there",
			args: []string{"pr", "comment", "1", "--body-file", absent},
			want: "the file --body-file names",
		},
		{
			name:   "a file past the bound",
			args:   []string{"release", "create", "v0", "--notes-file", huge},
			want:   "the file --notes-file names",
			longer: true,
		},
		{
			name: "a field's file that is not there",
			args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "-F", "body=@" + absent},
			want: "the file -F names",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			_, err := guard.GhCall(bound(t), GhRequest{Args: tc.args, Repo: "invented-org/invented-repo"})
			failed := setupFailure(t, err)
			if !strings.Contains(failed.Error(), tc.want) {
				t.Fatalf("the failure is %q, want it to name the option in %q", failed, tc.want)
			}
			if says := strings.Contains(failed.Error(), maxGhFileSaid); says != tc.longer {
				t.Errorf("the failure is %q; that it says %q is %t, want %t", failed, maxGhFileSaid, says, tc.longer)
			}
		})
	}
}

// A body option whose value is `-` is standard input, which the guard reads as GhRequest.Sent
// says (TestTheGhShapeScansWhatItBuffered): there is no file of that name to open, and a guard
// that tried would turn every such call into a setup failure.
func TestAGhCallsBodyFileOfStandardInputOpensNoFile(t *testing.T) {
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	if _, err := guard.GhCall(bound(t), GhRequest{
		Args: []string{"pr", "comment", "1", "--body-file", "-"},
		Repo: "invented-org/invented-repo",
	}); err != nil {
		t.Fatalf("a call whose body is standard input: %v", err)
	}
}

// gh 2.104.0's --attach uploads a file this guard does not read, so a bare argv naming it, the
// fallback's input, is refused naming the option, as `legion gh` refuses it before the guard:
// separately and with its value attached, on every leaf that carries it. The same argv past a
// bare `--` names no option gh reads and is cleared, and so is the same call without --attach.
func TestAGhCallsUploadTheGuardDoesNotReadIsASetupFailure(t *testing.T) {
	for _, args := range [][]string{
		{"pr", "comment", "1", "--body", "hi", "--attach", "notes.svg"},
		{"issue", "create", "--title", "t", "--body", "b", "--attach=notes.svg#alt"},
		{"issue", "artifact", "edit", "142", "2", "--attach", "./flow.png"},
	} {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			_, err := guard.GhCall(bound(t), GhRequest{Args: args, Repo: "invented-org/invented-repo"})
			failed := setupFailure(t, err)
			if !strings.Contains(failed.Error(), "--attach uploads the file it names, which this guard does not read") {
				t.Fatalf("the failure is %q, want it to name --attach", failed)
			}
		})
	}
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	for _, args := range [][]string{
		{"pr", "comment", "1", "--body", "hi"},
		{"repo", "clone", "invented-org/invented-repo", "--", "--attach", "notes.svg"},
	} {
		if _, err := guard.GhCall(bound(t), GhRequest{Args: args, Repo: "invented-org/invented-repo"}); err != nil {
			t.Fatalf("GhCall(%q): %v, want it cleared", args, err)
		}
	}
}

// A body file naming nothing private is cleared, which is what makes the refusal above a report
// of the text rather than of the option.
func TestAGhCallsCleanBodyFileIsCleared(t *testing.T) {
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("the box is in the other room\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	if _, err := guard.GhCall(bound(t), GhRequest{
		Args: []string{"pr", "comment", "1", "--body-file", body},
		Repo: "invented-org/invented-repo",
	}); err != nil {
		t.Fatalf("a clean body file: %v", err)
	}
}

// The option tables themselves (readGhArgv): which arguments name a file gh would send as
// content, in both spellings of each, and which name no file at all, and what use of standard
// input they name. The entries that answer nothing are the tables' boundary, and each is a call
// that must not be refused over a file nobody named. A file option given `-` and a field given
// `@-` name no file but standard input, which the tables answer apart (sent, GhSent.StdinSent),
// for the guard to read as GhRequest.Sent says.
func TestTheFilesAGhCallWouldSend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		want  []GhFile
		stdin StdinUse
		sent  bool
	}{
		{
			name: "a separate --body-file",
			args: []string{"pr", "comment", "1", "--body-file", "body.md"},
			want: []GhFile{{Option: "--body-file", Path: "body.md"}},
		},
		{
			name: "an attached --body-file",
			args: []string{"pr", "create", "--body-file=body.md"},
			want: []GhFile{{Option: "--body-file", Path: "body.md"}},
		},
		{
			name: "--notes-file",
			args: []string{"release", "create", "v0", "--notes-file", "notes.md"},
			want: []GhFile{{Option: "--notes-file", Path: "notes.md"}},
		},
		{
			name: "gh api's --input",
			args: []string{"api", "repos/invented-org/invented-repo/pulls", "--input", "request.json"},
			want: []GhFile{{Option: "--input", Path: "request.json"}},
		},
		{
			name: "gh agent-task create's --from-file",
			args: []string{"agent-task", "create", "--from-file", "task.md"},
			want: []GhFile{{Option: "--from-file", Path: "task.md"}},
		},
		{
			name: "a short -F whose value holds no =",
			args: []string{"pr", "comment", "1", "-F", "body.md"},
			want: []GhFile{{Option: "-F", Path: "body.md"}},
		},
		{
			name: "a short -F with its value attached",
			args: []string{"pr", "comment", "1", "-Fbody.md"},
			want: []GhFile{{Option: "-F", Path: "body.md"}},
		},
		{
			// pflag reads `-F=<value>` as `-F <value>`, and so does this reader (readOther).
			name: "a short -F with its value attached by an equals sign",
			args: []string{"pr", "create", "-F=body.md"},
			want: []GhFile{{Option: "-F", Path: "body.md"}},
		},
		{
			name: "a field read from a file",
			args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "-F", "body=@body.md"},
			want: []GhFile{{Option: "-F", Path: "body.md", Field: "body"}},
		},
		{
			name: "a field read from a file, attached to -F by an equals sign",
			args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "-F=body=@notes.md"},
			want: []GhFile{{Option: "-F", Path: "notes.md", Field: "body"}},
		},
		{
			name: "a GraphQL query read from a file, attached to -F by an equals sign",
			args: []string{"api", "graphql", "-F=query=@q.graphql"},
			want: []GhFile{{Option: "-F", Path: "q.graphql", Field: "query"}},
		},
		{
			name: "an attached --field read from a file",
			args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "--field=body=@body.md"},
			want: []GhFile{{Option: "--field", Path: "body.md", Field: "body"}},
		},
		{
			name: "a separate --field read from a file",
			args: []string{"api", "graphql", "--field", "query=@query.graphql"},
			want: []GhFile{{Option: "--field", Path: "query.graphql", Field: "query"}},
		},
		{
			// gh api's short letters are all known, so its cluster is read as pflag reads it: `-i`
			// is --include, and the -F that ends the cluster is --field (readLeaf).
			name: "a field read from a file, at the end of gh api's short cluster",
			args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "-iF", "body=@body.md"},
			want: []GhFile{{Option: "-F", Path: "body.md", Field: "body"}},
		},
		{
			name: "a field read from a file, attached to gh api's short cluster",
			args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "-iFbody=@notes.md"},
			want: []GhFile{{Option: "-F", Path: "notes.md", Field: "body"}},
		},
		{
			name: "the same file named twice",
			args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "-F", "body=@body.md", "--input", "body.md"},
			want: []GhFile{{Option: "-F", Path: "body.md", Field: "body"}},
		},
		{
			name: "gh workflow run's field read from a file",
			args: []string{"workflow", "run", "w.yml", "-F", "notes=@notes.md"},
			want: []GhFile{{Option: "-F", Path: "notes.md", Field: "notes"}},
		},
		{
			// -F is --body-file outside gh api and gh workflow run, so its whole value is the path,
			// an `=@` in it included.
			name: "another command's -F whose path holds an =@",
			args: []string{"pr", "create", "-F", "notes=@draft.md"},
			want: []GhFile{{Option: "-F", Path: "notes=@draft.md"}},
		},
		{name: "a field whose value is a literal", args: []string{"api", "x", "-F", "body=text"}},
		{name: "a raw field, which sends its value literally", args: []string{"api", "x", "-f", "body=@body.md"}},
		{name: "an attached raw field, which sends its value literally", args: []string{"api", "x", "-f=body=@body.md"}},
		{name: "a --field naming a project's field", args: []string{"project", "field-list", "1", "--field", "Status"}},
		{name: "a body option whose value is standard input", args: []string{"pr", "comment", "1", "--body-file", "-"}, stdin: StdinText, sent: true},
		{name: "a field read from standard input", args: []string{"api", "x", "-F", "body=@-"}, stdin: StdinField, sent: true},
		{name: "a long field read from standard input", args: []string{"api", "x", "--field", "body=@-"}, stdin: StdinField, sent: true},
		{name: "a field read from standard input, at the end of gh api's short cluster", args: []string{"api", "graphql", "-iF", "query=@-"}, stdin: StdinField, sent: true},
		// A cluster whose first value letter is another option's: `-iXF` is --include and the
		// method F, so the argument after it is a positional of gh's and names no file.
		{name: "gh api's short cluster ending in another option", args: []string{"api", "x", "-iXF", "body=@body.md"}},
		{name: "gh api's request body on standard input", args: []string{"api", "x", "--input", "-"}, stdin: StdinRequestBody, sent: true},
		{name: "gh api's attached request body on standard input", args: []string{"api", "x", "--input=-"}, stdin: StdinRequestBody, sent: true},
		{name: "release notes on standard input", args: []string{"release", "create", "v0", "--notes-file", "-"}, stdin: StdinText, sent: true},
		// -F outside gh api and gh workflow run is one of three options whose input is text, and
		// the leaf alone says which.
		{name: "a short -F of standard input", args: []string{"pr", "comment", "1", "-F", "-"}, stdin: StdinText, sent: true},
		{
			name:  "a file beside standard input",
			args:  []string{"api", "x", "--input", "-", "-F", "body=@body.md"},
			want:  []GhFile{{Option: "-F", Path: "body.md", Field: "body"}},
			stdin: StdinRequestBody,
			sent:  true,
		},
		// Only one of two options can have the input, so the guard is told it does not know, and
		// that the call sends it.
		{name: "two options of standard input that differ", args: []string{"api", "x", "--input", "-", "-F", "body=@-"}, sent: true},
		{name: "one option of standard input twice", args: []string{"api", "x", "-F", "title=@-", "-F", "body=@-"}, stdin: StdinField, sent: true},
		{name: "a secret's env file", args: []string{"secret", "set", "NAME", "--env-file", "secrets.env"}},
		{name: "a template of the repository", args: []string{"pr", "create", "--template", "pull_request_template.md"}},
		{name: "a gist's positional files", args: []string{"gist", "create", "note.md"}},
		{name: "a short option whose own value is F", args: []string{"gist", "create", "-dF", "note.md"}},
		// The shape this reader cannot tell apart from a call that names no file, and the reason
		// `legion gh` hands its own parse instead (GhRequest.Sent): a short cluster's last
		// letter is the option in `gh pr create` (`--draft --body-file`) and the value of the
		// option before it in `gh gist create` (`--desc F`), and only the leaf's own help says
		// which.
		{name: "a short cluster whose last letter is the option", args: []string{"pr", "create", "-dF", "body.md"}},
		{name: "an option past a double dash", args: []string{"repo", "clone", "invented-org/invented-repo", "--", "--body-file", "body.md"}},
		{name: "standard input past a double dash", args: []string{"repo", "clone", "invented-org/invented-repo", "--", "--input", "-"}},
		{name: "an option with no value at all", args: []string{"pr", "comment", "1", "--body-file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readGhArgv(tc.args); !slices.Equal(got.sent.Files, tc.want) || got.sent.stdinUse != tc.stdin || got.sent.StdinSent() != tc.sent {
				t.Fatalf("readGhArgv(%q) read the files %v and standard input %v, sent %t; want %v and %v, sent %t",
					tc.args, got.sent.Files, got.sent.stdinUse, got.sent.StdinSent(), tc.want, tc.stdin, tc.sent)
			}
		})
	}
}

// The files the caller parsed are the files read, and the table is not consulted for a caller
// that handed its parse: `legion gh` reads the command with gh's own help of the leaf, which
// knows shapes the table cannot (cmd/legion's ghSentFiles), and two readers of one argv would be
// two answers to one question. A caller that hands no parse at all — a nil GhRequest.Sent, which
// is what a bare argv is — still gets the table.
func TestTheFilesTheCallerParsedAreTheFilesRead(t *testing.T) {
	dir := t.TempDir()
	named := filepath.Join(dir, "named.md")
	unnamed := filepath.Join(dir, "unnamed.md")
	for _, path := range []string{named, unnamed} {
		if err := os.WriteFile(path, []byte("the box is at "+leakguardtest.RulesHost+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a file the argv never spells is read", func(t *testing.T) {
		// The shape the table misses and the parse does not: `-dF <path>` is `--draft
		// --body-file <path>` to gh, and the caller hands the file it read out of that.
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
		_, err := guard.GhCall(bound(t), GhRequest{
			Args: []string{"pr", "create", "-dF", unnamed},
			Repo: "invented-org/invented-repo",
			Sent: &GhSent{Files: []GhFile{{Option: "--body-file", Path: unnamed}}},
		})
		if report := refusal(t, err); !strings.Contains(report.Error(), leakguardtest.RulesHostClass) {
			t.Fatalf("the refusal does not name the class: %v", report)
		}
	})
	t.Run("a caller that parsed no file reads none", func(t *testing.T) {
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
		if _, err := guard.GhCall(bound(t), GhRequest{
			Args: []string{"pr", "comment", "1", "--body-file", named},
			Repo: "invented-org/invented-repo",
			Sent: &GhSent{},
		}); err != nil {
			t.Fatalf("a call whose caller parsed no file: %v", err)
		}
	})
	t.Run("a caller that parsed nothing gets the table", func(t *testing.T) {
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
		_, err := guard.GhCall(bound(t), GhRequest{
			Args: []string{"pr", "comment", "1", "--body-file", named},
			Repo: "invented-org/invented-repo",
		})
		if report := refusal(t, err); !strings.Contains(report.Error(), leakguardtest.RulesHostClass) {
			t.Fatalf("the refusal does not name the class: %v", report)
		}
	})
}

// gh api's short cluster (readLeaf) is gh api's alone. In `gh pr create`, `-dF <path>` is
// `--draft --body-file <path>`: the file the caller parsed out of it is read as the body it is,
// and the argv makes no field of it. The file's name holds an `=`, so a reader that took `-dF`
// for gh api's field option would make a field of the path.
func TestAShortClusterOutsideGhAPIIsNoField(t *testing.T) {
	body := fieldFile(t, "notes=draft.md", "the "+leakguardtest.PairName+" and the "+leakguardtest.PairWord)
	args := []string{"pr", "create", "-dF", body}
	for _, record := range readGhArgv(args).records {
		if strings.HasPrefix(record.Label, "the gh call's field ") {
			t.Fatalf("readGhArgv(%q) made the field record %q", args, record.Label)
		}
	}
	err := ghFieldCall(t, GhRequest{Args: args, Sent: &GhSent{Files: []GhFile{{Option: "--body-file", Path: body}}}})
	reports(t, refusal(t, err).Lines, body+": "+leakguardtest.PairClass)
}

// In a `gh api` call a short cluster is read as pflag reads it (readLeaf): `-iF query=<v>`,
// `-iFquery=<v>` and `-if query=<v>` are the query field after --include. So a GraphQL query given
// so is read one literal at a time: the fixture's two words in two literals of one mutation pass,
// though a read of the argument whole joins them, and the two in one literal are refused, the
// report naming the literal.
func TestGhAPIShortClusterQueryReadsOneLiteralAtATime(t *testing.T) {
	apart := graphQLMutation("the "+leakguardtest.PairName+" notes", "a "+leakguardtest.PairWord+" aside")
	together := graphQLMutation("a clean title", "the "+leakguardtest.PairName+" and the "+leakguardtest.PairWord)
	refusedWhole(t, apart)
	for _, tc := range []struct {
		name string
		args func(query string) []string
	}{
		{"-iF query=", func(query string) []string { return []string{"api", "graphql", "-iF", "query=" + query} }},
		{"-iFquery=", func(query string) []string { return []string{"api", "graphql", "-iFquery=" + query} }},
		{"-if query=", func(query string) []string { return []string{"api", "graphql", "-if", "query=" + query} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleared(t, ghFieldCall(t, GhRequest{Args: tc.args(apart)}))
			err := ghFieldCall(t, GhRequest{Args: tc.args(together)})
			reports(t, refusal(t, err).Lines, "the gh call's field query, GraphQL string 3: "+leakguardtest.PairClass)
		})
	}
}

// Standard input a field other than `query` takes in a `gh api graphql` call is text GitHub stores
// as it stands, one record: a mutation on it holding the fixture's two words in two literals is
// refused, since that input is a body and no query. A query and another field both of standard
// input are one input gh reads into one of them, so it is read both ways, raw and one literal at
// a time: the two words apart are refused by the raw record, and a name escaped inside one literal
// by that literal. A query of standard input whose use the caller's parse does not know, where
// the parse names the field, is read one literal at a time.
func TestGhAPIGraphQLStandardInputIsReadByTheFieldsThatTakeIt(t *testing.T) {
	apart := graphQLMutation("the "+leakguardtest.PairName+" notes", "a "+leakguardtest.PairWord+" aside")
	escaped := graphQLMutation("the "+strings.Replace(leakguardtest.PairName, "l", `\u006c`, 1)+" and the "+leakguardtest.PairWord, "a clean body")
	refusedWhole(t, apart)
	if strings.Contains(escaped, leakguardtest.PairName) {
		t.Fatalf("the query %q spells the name in its own bytes, so it proves nothing", escaped)
	}
	const stdin = "the gh call's standard input"
	for _, tc := range []struct {
		name string
		sent *GhSent
	}{
		{"read out of the argv", nil},
		{"named by the caller's parse", stdinParse("--field", "body", StdinField)},
	} {
		t.Run("another field's input is text, "+tc.name, func(t *testing.T) {
			err := ghFieldCall(t, GhRequest{Args: []string{"api", "graphql", "-F", "body=@-"}, Stdin: strings.NewReader(apart), Sent: tc.sent})
			reports(t, refusal(t, err).Lines, stdin+": "+leakguardtest.PairClass)
		})
	}
	t.Run("a query and another field are read both ways", func(t *testing.T) {
		args := []string{"api", "graphql", "-F", "query=@-", "-F", "body=@-"}
		err := ghFieldCall(t, GhRequest{Args: args, Stdin: strings.NewReader(apart)})
		reports(t, refusal(t, err).Lines, stdin+": "+leakguardtest.PairClass)
		err = ghFieldCall(t, GhRequest{Args: args, Stdin: strings.NewReader(escaped)})
		reports(t, refusal(t, err).Lines, stdin+", GraphQL string 2: "+leakguardtest.PairClass)
	})
	t.Run("a query no option is known for", func(t *testing.T) {
		// A parse that names the field and no use, as one naming two uses of the input does: the
		// field's `@-`, and then `--input -`, in the order cmd/legion's ghSentFiles reads them.
		sent := stdinParse("--field", graphQLQuery, StdinField)
		sent.UsesStdin(StdinRequestBody)
		if sent.stdinUse != StdinUnknown || !sent.StdinSent() {
			t.Fatalf("the parse %+v names a use of standard input, or sends none, so this row proves nothing", sent)
		}
		call := func(query string) GhRequest {
			return GhRequest{Args: []string{"api", "graphql", "-F", "query=@-"}, Stdin: strings.NewReader(query), Sent: sent}
		}
		cleared(t, ghFieldCall(t, call(apart)))
		reports(t, refusal(t, ghFieldCall(t, call(escaped))).Lines, stdin+", GraphQL string 2: "+leakguardtest.PairClass)
	})
}

// The use a call makes of its standard input is read out of its argv by the command's own
// options, the command found past the -R gh takes before it. In `gh workflow run`, `-F <name>=@-`
// makes the input the value of a field, text gh makes one JSON string of; in `gh pr create`, `-F
// -` is `--body-file -`, and the input is the body, text too. So a JSON document on either is read
// as the text it is, an escape and all, and is cleared where only its decoded string names the
// fixture's pair, which an input of no known use, read decoded as well, is refused for; and either
// input is refused where its own text names the pair.
func TestGhStandardInputIsReadAsTheCommandUsesIt(t *testing.T) {
	// The word with its first letter written as a JSON escape: decoded, the document's one string
	// names the pair, and as text it does not.
	escaped := `{"body":"the ` + leakguardtest.PairName + ` and the \u006d` + leakguardtest.PairWord[1:] + `"}`
	if leakguardtest.PairWord[0] != 'm' || strings.Contains(escaped, leakguardtest.PairWord) {
		t.Fatalf("the document %q spells the word in its own bytes, so it proves nothing", escaped)
	}
	plain := "the " + leakguardtest.PairName + " and the " + leakguardtest.PairWord
	const stdin = "the gh call's standard input"
	for _, tc := range []struct {
		name   string
		args   []string
		use    StdinUse
		fields []string
	}{
		{"a field of gh workflow run past -R", []string{"-R", "invented-org/invented-repo", "workflow", "run", "w.yml", "-F", "x=@-"}, StdinField, []string{"x"}},
		{"gh pr create's -F", []string{"pr", "create", "-F", "-"}, StdinText, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if argv := readGhArgv(tc.args); argv.sent.stdinUse != tc.use || !slices.Equal(argv.sent.stdinFields, tc.fields) || argv.sent.Files != nil {
				t.Fatalf("readGhArgv(%q) read standard input as %v, by the fields %q, and the files %v; want %v, by %q, and no file", tc.args, argv.sent.stdinUse, argv.sent.stdinFields, argv.sent.Files, tc.use, tc.fields)
			}
			cleared(t, ghFieldCall(t, GhRequest{Args: tc.args, Stdin: strings.NewReader(escaped)}))
			reports(t, refusal(t, ghFieldCall(t, GhRequest{Args: tc.args, Stdin: strings.NewReader(plain)})).Lines, stdin+": "+leakguardtest.PairClass)
		})
	}
	t.Run("the same document of no known use is read decoded", func(t *testing.T) {
		err := ghFieldCall(t, GhRequest{Args: []string{"pr", "create", "-F", "-"}, Stdin: strings.NewReader(escaped), Sent: &GhSent{}})
		reports(t, refusal(t, err).Lines, stdin+", JSON .body: "+leakguardtest.PairClass)
	})
}

// The option tables of ghLeaves are the helps of their commands in gh 2.98.0, the version the
// worker image pins (cmd/legion/testdata/gh-help): every option whose line under FLAGS or
// INHERITED FLAGS names a value after its flag names (`  -X, --method string`, `      --input
// file`, `  -F, --field key=value`) is in its command's table, its long name and its short letter
// both, and no option whose line names none (`  -i, --include`) is. A table short of an option
// would read that option's value as an argument of its own, a field or the endpoint where gh reads
// a value, and a table holding one too many would take the argument after a flag for its value.
func TestGhLeafOptionTablesAreTheirHelps(t *testing.T) {
	for _, tc := range []struct {
		help string
		leaf *ghLeaf
	}{
		{"api.txt", &ghAPILeaf},
		{"workflow_run.txt", &ghWorkflowRunLeaf},
	} {
		t.Run(strings.Join(tc.leaf.path, " "), func(t *testing.T) {
			help, err := os.ReadFile(filepath.Join("..", "..", "cmd", "legion", "testdata", "gh-help", tc.help))
			if err != nil {
				t.Fatal(err)
			}
			long, short, options := map[string]bool{}, []byte{}, 0
			flags := false
			for line := range strings.Lines(string(help)) {
				line = strings.TrimSuffix(line, "\n")
				if line != "" && line[0] != ' ' {
					// A heading: USAGE, FLAGS, INHERITED FLAGS, EXAMPLES and the rest.
					flags = line == "FLAGS" || line == "INHERITED FLAGS"
					continue
				}
				// An option's line is its flag names, the value it takes, if any, and, past two
				// spaces, its description.
				names, _, _ := strings.Cut(strings.TrimLeft(line, " "), "  ")
				words := strings.Fields(names)
				if !flags || len(words) == 0 || !strings.HasPrefix(words[0], "-") {
					continue
				}
				letter, hasShort := strings.CutSuffix(words[0], ",")
				if hasShort {
					words = words[1:]
				}
				options++
				if len(words) < 2 {
					continue
				}
				long[words[0]] = true
				if hasShort {
					short = append(short, strings.TrimPrefix(letter, "-")...)
				}
			}
			if options == 0 {
				t.Fatalf("%s lists no option under FLAGS or INHERITED FLAGS, so it was not read", tc.help)
			}
			if !maps.Equal(long, tc.leaf.long) {
				t.Errorf("the long options of %s that take a value are %q, and the table holds %q", tc.help, slices.Sorted(maps.Keys(long)), slices.Sorted(maps.Keys(tc.leaf.long)))
			}
			table := []byte(tc.leaf.short)
			slices.Sort(short)
			slices.Sort(table)
			if string(short) != string(table) {
				t.Errorf("the short options of %s that take a value are %q, and the table holds %q", tc.help, short, table)
			}
		})
	}
}

// A JSON body is read decoded. A string's escapes are text GitHub stores and shows — `\u0074` is
// the letter t — and an escape inside a name parts a word the rules read whole, so a request body
// that spells a host in escapes names that host as surely as one that spells it out. Both doors a
// JSON body comes through are driven here: `gh api --input <file>` and the standard input the
// shared check's exit-4 handshake buffers.
func TestAJsonBodysEscapesAreRead(t *testing.T) {
	// The host, with the first letter of each of its three labels written as a JSON escape, so
	// no pattern of the rule file matches the bytes of the document itself.
	escaped := `{"body": "the box is at \u0061mber-kite.\u0073till-meadow.ts.\u006eet"}`
	if strings.Contains(escaped, leakguardtest.RulesHost) {
		t.Fatal("the fixture spells the host in the document's own bytes, so it proves nothing")
	}
	t.Run("gh api --input", func(t *testing.T) {
		body := filepath.Join(t.TempDir(), "request.json")
		if err := os.WriteFile(body, []byte(escaped), 0o600); err != nil {
			t.Fatal(err)
		}
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
		_, err := guard.GhCall(bound(t), GhRequest{
			Args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "--method", "PATCH", "--input", body},
			Repo: "invented-org/invented-repo",
		})
		report := refusal(t, err)
		reports(t, report.Lines, body+", JSON .body: "+leakguardtest.RulesHostClass)
	})
	t.Run("the buffered standard input", func(t *testing.T) {
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), leakguardtest.Reading(0, "")), Public)
		_, err := guard.GhCall(bound(t), GhRequest{
			Args:  []string{"api", "repos/invented-org/invented-repo/pulls/1", "--method", "PATCH", "--input", "-"},
			Stdin: strings.NewReader(escaped),
			Repo:  "invented-org/invented-repo",
		})
		report := refusal(t, err)
		reports(t, report.Lines, "the gh call's standard input, JSON .body: "+leakguardtest.RulesHostClass)
	})
	t.Run("a JSON body naming nothing is cleared", func(t *testing.T) {
		body := filepath.Join(t.TempDir(), "request.json")
		if err := os.WriteFile(body, []byte(`{"body": "the box is in the other \u0072oom"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
		if _, err := guard.GhCall(bound(t), GhRequest{
			Args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "--input", body},
			Repo: "invented-org/invented-repo",
		}); err != nil {
			t.Fatalf("a clean JSON body: %v", err)
		}
	})
}

// The standard input the guard reads is bounded as a named file is: the bytes are held in memory
// and scanned there. Input past the bound is a setup failure, which refuses the call, never a
// truncated buffer handed to gh as though the guard had read the whole: for a public destination
// under a check that asks for the input, under one that clears the call without asking, and under
// no shared check. A private destination runs no rule pass, and the guard reads no input there
// that the check did not ask for (GhRequest.Sent): under a check that clears without asking and
// under no check, the same call to it is cleared, nothing comes back, and the caller's input is
// still whole for gh. A check that asks reads the input in its handshake, before any visibility
// answer, so the bound refuses the call to a private destination there too.
func TestStandardInputPastTheBoundIsASetupFailure(t *testing.T) {
	const size = maxGhFile + 1
	for _, tc := range []struct {
		name string
		// check is the stand-in shared check, "" for a deployment that runs none.
		check      string
		visibility Visibility
		// refused is whether the call is refused for its input; one that is not is cleared with
		// that input unread.
		refused bool
	}{
		{"a check that asks, to a public destination", leakguardtest.Reading(0, ""), Public, true},
		{"a check that asks, to a private destination", leakguardtest.Reading(0, ""), Private, true},
		{"a check that clears without asking, to a public destination", passes, Public, true},
		{"a check that clears without asking, to a private destination", passes, Private, false},
		{"no shared check, to a public destination", "", Public, true},
		{"no shared check, to a private destination", "", Private, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No check named and none on PATH is a deployment that runs no shared check.
			check, extra := "", []string{"PATH=" + t.TempDir()}
			if tc.check != "" {
				check, extra = leakguardtest.Write(t, t.TempDir(), tc.check), nil
			}
			stdin := bytes.NewReader(bytes.Repeat([]byte("a"), size))
			answer, err := guardWith(t, check, tc.visibility, extra...).GhCall(bound(t), GhRequest{
				Args:  []string{"pr", "create", "--body-file", "-"},
				Stdin: stdin,
				Repo:  "invented-org/invented-repo",
			})
			if !tc.refused {
				if err != nil || answer.Stdin != nil || answer.Read != GhReadWhole {
					t.Fatalf("GhCall = %d bytes back, read %v, %v; want it cleared by the whole guard with its input unread", len(answer.Stdin), answer.Read, err)
				}
				if stdin.Len() != size {
					t.Errorf("the guard took %d bytes of the caller's input, want it whole for gh", size-stdin.Len())
				}
				return
			}
			failed := setupFailure(t, err)
			if !strings.Contains(failed.Error(), "standard input is longer than") {
				t.Fatalf("the failure is %q, want it to name the bound", failed)
			}
			if answer.Stdin != nil {
				t.Errorf("the caller got %d bytes back of an input the guard never read whole", len(answer.Stdin))
			}
		})
	}
}
