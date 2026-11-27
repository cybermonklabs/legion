package leakguard

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// namedWrite is one write a test makes through one entry point of guard.
type namedWrite struct {
	name  string
	write func(ctx context.Context, guard *Guard) error
}

// writesNamingTheHost are the three writes to a public destination that name the fixture's host,
// one through each entry point that runs the shared check: a push whose ref name names it, from
// the local commit local; a gh call whose title names it; and a text whose body names it.
func writesNamingTheHost(t *testing.T, local string) []namedWrite {
	t.Helper()
	dir := t.TempDir()
	return []namedWrite{
		{name: "a push", write: func(ctx context.Context, guard *Guard) error {
			return guard.Push(ctx, PushRequest{
				Dir:     dir,
				Remote:  "origin",
				URL:     "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: local, RemoteRef: "refs/heads/legion/" + leakguardtest.RulesHost, RemoteSHA: noSHA}},
			})
		}},
		{name: "a gh call", write: func(ctx context.Context, guard *Guard) error {
			_, err := guard.GhCall(ctx, GhRequest{
				Args: []string{"pr", "create", "--title", "a title naming " + leakguardtest.RulesHost},
				Repo: "invented-org/invented-repo",
			})
			return err
		}},
		{name: "a text", write: func(ctx context.Context, guard *Guard) error {
			return guard.Text(ctx, TextRequest{
				Repo: "invented-org/invented-repo",
				Text: "a clean title\n\na body naming " + leakguardtest.RulesHost + "\n",
			})
		}},
	}
}

// A deployment that names its rule file in LEGION_PRIVATE_NAME_RULES_FILE has the guard run it:
// a push, a gh call and a text that name the fixture's host to a public destination are each
// refused by the rule pass, after the shared check cleared them and the visibility read answered
// public.
func TestANamedRuleFileRefusesAFixtureName(t *testing.T) {
	for _, write := range writesNamingTheHost(t, noSHA) {
		t.Run(write.name, func(t *testing.T) {
			var asked []string
			guard := New(Options{
				Check:      leakguardtest.Write(t, t.TempDir(), passes),
				Visibility: answers(Public, &asked),
				Env:        withRules(t, fixtureenv.Isolated(t.TempDir())),
			})
			refused := refusal(t, write.write(bound(t), guard))
			if refused.By != byRules {
				t.Errorf("the write was refused by %q, want the rules", refused.By)
			}
			if len(refused.Lines) != 1 || !namesClass(refused.Lines[0], leakguardtest.RulesHostClass) {
				t.Errorf("the report is %q, want one line naming %q", refused.Lines, leakguardtest.RulesHostClass)
			}
			if len(asked) != 1 {
				t.Errorf("the visibility was read %d times, want once", len(asked))
			}
		})
	}
}

// A deployment that names no rule file runs no private-name rule: the same three writes pass. The
// shared check runs for each of them, as it does today; the visibility seam is never asked, and the
// push's commits are never read (the stand-in git on PATH, which would be asked for the commit the
// push names, never runs). An empty pointer names no file either.
func TestNoRuleFileRunsNoPrivateNameRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pointer []string
	}{
		{name: "no pointer"},
		{name: "an empty pointer", pointer: []string{RulesFileVar + "="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, records := t.TempDir(), t.TempDir()
			checked, gitRan := filepath.Join(records, "checked"), filepath.Join(records, "git")
			writeProgram(t, bin, "git", recordsItsCall(gitRan)+"exit 1\n")
			var asked []string
			guard := New(Options{
				Check:      leakguardtest.Write(t, t.TempDir(), leakguardtest.Recording(leakguardtest.Passes)),
				Visibility: answers(Public, &asked),
				Env:        append(append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "LEAK_CHECK_RECORD="+checked), tc.pointer...),
			})
			writes := writesNamingTheHost(t, strings.Repeat("1", 40))
			for _, write := range writes {
				if err := write.write(bound(t), guard); err != nil {
					t.Errorf("%s naming the fixture's host, with no rule file named: %v", write.name, err)
				}
			}
			answer, err := guard.GhCall(bound(t), GhRequest{Args: []string{"pr", "comment", "1", "--body", leakguardtest.RulesHost}, Repo: "invented-org/invented-repo"})
			if err != nil || answer.Read != GhReadNoRules {
				t.Errorf("GhCall = %+v, %v; want it passed, saying no rule is named", answer, err)
			}
			if got, want := countLines(t, checked), len(writes)+1; got != want {
				t.Errorf("the shared check ran %d times, want %d: once for each write that runs it", got, want)
			}
			if len(asked) != 0 {
				t.Errorf("the visibility of %q was read, want no read with no rule to run", asked)
			}
			if got := countLines(t, gitRan); got != 0 {
				t.Errorf("git ran %d times, want none: a push with no rule to run reads no commit", got)
			}
		})
	}
}

// A rule file the guard cannot use refuses the write as a setup failure that names the file,
// before any visibility is read: a deployment that named rules meant them to run. Each way the
// file can fail is one case. ReadRulesContent, the read the guard and the daemon's boot share,
// refuses a file it cannot read, or one longer than MaxRulesFile, in a phrase that follows the
// file's name, and answers every other file whole for the compile to refuse. A file one byte past
// MaxRulesFile is refused for its length, and one at the bound is read whole, so what refuses it
// is what it holds.
func TestARuleFileTheGuardCannotReadRefusesTheWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		// content is the file's text; a case with none writes no file at all.
		content string
		// says is what the failure must say of why.
		says string
		// unread is whether ReadRulesContent itself refuses the file, its error then exactly says.
		unread bool
	}{
		{name: "a missing file", says: "could not be opened: no such file or directory", unread: true},
		{
			name:    "a file one byte past the bound",
			content: leakguardtest.PaddedTo(leakguardtest.Rules, MaxRulesFile+1),
			says:    "is longer than 262144 bytes",
			unread:  true,
		},
		{
			name:    "a file at the bound with no rule",
			content: leakguardtest.PaddedTo("title = \"no rules here\"\n", MaxRulesFile),
			says:    "hold no rule",
		},
		{name: "text that is no TOML", content: "this is = = no TOML [\n", says: "are not TOML"},
		{name: "an unknown key", content: "severity = \"high\"\n" + leakguardtest.Rules, says: "invalid keys: severity"},
		{
			name:    "a misspelt [[rules]]",
			content: "[[rule]]\nid = \"word_rule\"\ndescription = \"" + leakguardtest.WordClass + "\"\nregex = '''" + leakguardtest.Word + "'''\n",
			says:    "invalid keys: rule",
		},
		{name: "an [extend] section", content: leakguardtest.Rules + "\n[extend]\nuseDefault = true\n", says: "[extend]"},
		{name: "a file with no rule", content: "title = \"no rules here\"\n", says: "hold no rule"},
		{
			name:    "two rules of one id",
			content: leakguardtest.Rules + "\n[[rules]]\nid = \"word_rule\"\ndescription = \"names the invented word again\"\nregex = '''" + leakguardtest.PairWord + "'''\n",
			says:    `two rules of the id "word_rule"`,
		},
		{
			name:    "a regex that panics",
			content: "[[rules]]\nid = \"broken_rule\"\ndescription = \"holds a pattern that does not compile\"\nregex = '''(?P<broken'''\n",
			says:    "did not compile",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private-name-rules.toml")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var asked []string
			guard := New(Options{
				Check:      leakguardtest.Write(t, t.TempDir(), passes),
				Visibility: answers(Public, &asked),
				Env:        append(fixtureenv.Isolated(t.TempDir()), RulesFileVar+"="+path),
			})
			failed := setupFailure(t, guard.Text(bound(t), TextRequest{Repo: "invented-org/invented-repo", Text: "a clean title\n"}))
			if !strings.Contains(failed.Why, path) || !strings.Contains(failed.Why, RulesFileVar) {
				t.Errorf("the setup failure's why is %q, want it to name %s and %s", failed.Why, path, RulesFileVar)
			}
			if !strings.Contains(failed.Error(), tc.says) {
				t.Errorf("the setup failure is %q, want it to say %q", failed.Error(), tc.says)
			}
			if len(asked) != 0 {
				t.Errorf("the visibility of %q was read, want none before the rules are read", asked)
			}
			content, err := ReadRulesContent(path)
			switch {
			case !tc.unread && err != nil:
				t.Errorf("ReadRulesContent refused a file it can read: %v", err)
			case !tc.unread && string(content) != tc.content:
				t.Errorf("ReadRulesContent answered %d bytes, want the file's %d whole", len(content), len(tc.content))
			case tc.unread && (err == nil || err.Error() != tc.says):
				t.Errorf("ReadRulesContent's error is %v, want %q, a phrase that follows the file's name", err, tc.says)
			case tc.content == "" && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("ReadRulesContent's error %q is no fs.ErrNotExist", err)
			}
		})
	}
}

// A rule set checks records with no guard at all, as the daemon's branch create runs the set it
// compiled at boot (internal/ghbranch's Create): a clean ref name passes, and one naming the
// fixture's host is refused by the rules, one line naming the class. It reads no environment: a
// pointer the environment holds, here one to a file that is not there, over which a guard would
// refuse the write, changes nothing.
func TestARuleSetChecksRecordsWithNoGuard(t *testing.T) {
	t.Setenv(RulesFileVar, filepath.Join(t.TempDir(), "absent.toml"))
	rules := fixtureRules(t)
	if err := rules.Check(bound(t), "the write", RefRecord("refs/heads/legion/INVENTED-1")); err != nil {
		t.Errorf("a clean ref name: %v", err)
	}
	refused := refusal(t, rules.Check(bound(t), "the write", RefRecord("refs/heads/legion/"+leakguardtest.RulesHost)))
	if refused.By != byRules || refused.What != "the write" || len(refused.Lines) != 1 || !namesClass(refused.Lines[0], leakguardtest.RulesHostClass) {
		t.Errorf("the write was refused as %q by %q with %q, want the write refused by the rules naming %q", refused.What, refused.By, refused.Lines, leakguardtest.RulesHostClass)
	}
}

// The secret and its pointer are spelled as the deployment spells them: a role's guard reads the
// pointer its launch's runtime sets for the secret.
func TestTheRuleFileVariableIsTheLaunchSecretsPointer(t *testing.T) {
	for got, want := range map[string]string{
		RulesSecret:  "LEGION_PRIVATE_NAME_RULES",
		RulesFileVar: "LEGION_PRIVATE_NAME_RULES_FILE",
	} {
		if got != want {
			t.Errorf("the name is %q, want %q", got, want)
		}
	}
}
