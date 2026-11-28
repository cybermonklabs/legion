package ghroute

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// madeUpGhToken is the made-up token these rows write where a person or a CI checkout could, the
// one ghroutetest.PrintedSelectorCalls write.
const madeUpGhToken = ghroutetest.MadeUpToken

// ghPrinted is err as `legion gh` writes it to stderr (PrintRefusal).
func ghPrinted(err error) string {
	var stderr bytes.Buffer
	PrintRefusal(&stderr, err)
	return stderr.String()
}

// A token in a URL a person puts where TargetOf reads a repository - GH_REPO, -R, a remote's
// gh-resolved marker - reaches the refusal that names that value, and `legion gh` writes the
// refusal through the URL rule, as every other command's (PrintRefusal): the line holds no part of
// the token, and still says what was refused. A selector is named by its kind alone
// (repositoryTarget): a URL with a user name or a password, on github.com or another host, and on
// another host with a token in its path; a value with no scheme, which the URL rule cannot read
// (a user name and password before its host, a token as its user, a token in its query, on another
// host or on a github.com one), which is refused, not admitted. A marker's value other than `base`
// or the OWNER/REPO gh writes prints as no URL (markerShown), and so does a refused remote's URL
// (Remote.what), whose path no rule has read as a repository's. A remote whose repository
// segment holds a token (written into the URL's path, as a user name and password, as a token and
// an "@", as a token and a %40, or in an scp-like URL), beside an upstream of another served owner,
// is refused as naming no OWNER/REPO, so the refusal of two owners, which prints the owner and name
// a remote names, never reaches it; a remote with a token in its path and no "@", where the URL
// rule finds no user info, is named by its git name alone; and a marker naming acme/widgets with a
// token after it, beside an upstream of another owner, prints none of it in the two-owner refusal.
// The rows that read no git remote are ghroutetest.PrintedSelectorCalls, which `legion gh`'s
// tests also resolve through its keep of gh's help.
func TestLegionGhPrintsNoTokenOfASelectorOrAMarker(t *testing.T) {
	t.Parallel()
	at := "@"
	pane := map[string]string{"GH_CONFIG_DIR": "/owners/acme", "LEGION_GH_OWNERS_DIR": "/owners"}
	with := func(extra map[string]string) map[string]string {
		env := map[string]string{}
		for k, v := range pane {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	tokenURL := func(host string) string {
		return "https://x-access-token:" + madeUpGhToken + at + host + "/acme/widgets"
	}
	marker := func(value string) func() ([]Remote, error) {
		return func() ([]Remote, error) {
			return []Remote{{name: "origin", url: "https://github.com/acme/widgets.git"},
				markerLine("remote.origin.gh-resolved " + value)}, nil
		}
	}
	// A marker value markerShown does not print is named by the rule's own words for a URL not
	// printed (redact.URLNotPrinted), read from the rule, never written out here.
	markerNotPrinted := "remote.origin.gh-resolved " + redact.URLNotPrinted
	type row struct {
		argv    []string
		env     map[string]string
		remotes func() ([]Remote, error)
		names   string
	}
	rows := map[string]row{
		"a gh-resolved marker's URL": {[]string{"pr", "list"}, with(nil), marker(tokenURL("ghe.example.com")), markerNotPrinted},
		"an scp-form marker": {[]string{"pr", "list"}, with(nil), marker("x-access-token:" + madeUpGhToken + at + "ghe.example.com:acme/widgets"),
			markerNotPrinted},
		"a marker with no scheme": {[]string{"pr", "list"}, with(nil), marker("x-access-token:" + madeUpGhToken + at + "ghe.example.com/acme/widgets"),
			markerNotPrinted},
		"a marker with a space before the @": {[]string{"pr", "list"}, with(nil), marker("https://x:zq tok0123" + at + "ghe.example.com/acme/widgets"),
			markerNotPrinted},
		"a marker with a token in the query": {[]string{"pr", "list"}, with(nil), marker("ghe.example.com/acme/widgets?t=" + madeUpGhToken),
			markerNotPrinted},
	}
	for name, call := range ghroutetest.PrintedSelectorCalls {
		rows[name] = row{call.Argv, call.Env, noGitRepository, call.Names}
	}
	beside := func(origin string) func() ([]Remote, error) {
		return func() ([]Remote, error) {
			return []Remote{{name: "origin", url: origin}, {name: "upstream", url: "https://github.com/other/app"}}, nil
		}
	}
	const remoteWords = "names no OWNER/REPO whose owner is a GitHub login and whose name is a repository name"
	for shape, origin := range map[string]string{
		"a user name and a token in the path": "https://github.com/acme/x-access-token:" + madeUpGhToken + at + "github.com",
		"a token and an @ in the path":        "https://github.com/acme/" + madeUpGhToken + at + "x",
		"a token and a %40 in the path":       "https://github.com/acme/" + madeUpGhToken + "%40x",
		"a token in an scp-like path":         "git" + at + "github.com:acme/u:" + madeUpGhToken + at + "github.com",
	} {
		rows["a remote with "+shape] = row{[]string{"pr", "list"}, with(nil), beside(origin), remoteWords}
	}
	rows["a refused remote with a token in its path and no @"] = row{[]string{"pr", "list"}, with(nil),
		beside("https://github.com/acme/x-access-token:" + madeUpGhToken), `the working directory's remote "origin" ` + redact.URLNotPrinted + ` ` + remoteWords}
	rows["a marker with a token after OWNER/REPO, beside another owner"] = row{[]string{"pr", "list"}, with(nil),
		func() ([]Remote, error) {
			return []Remote{markerLine("remote.origin.gh-resolved github.com/acme/widgets/" + madeUpGhToken), {name: "upstream", url: "https://github.com/other/app"}}, nil
		}, "acme (the working directory's " + markerNotPrinted + " naming acme/widgets)"}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			_, err := TargetOf(tc.argv, tc.env, savedGhHelp, tc.remotes, ghroutetest.OwnersOwnDirectories)
			if err == nil {
				t.Fatal("TargetOf = nil error, want a refusal")
			}
			printed := ghPrinted(err)
			if strings.Contains(printed, madeUpGhToken) || strings.Contains(printed, "tok0123") {
				t.Errorf("legion gh printed %q, which holds the token", printed)
			}
			if !strings.HasPrefix(printed, "legion gh: ") || !strings.Contains(printed, tc.names) {
				t.Errorf("legion gh printed %q, want it to name %q", printed, tc.names)
			}
		})
	}
}

// An ssh URL glued to a URL holding a token goes through the URL rule with it, so no part of the
// token prints.
func TestPrintGhRefusalLeavesNoLoginGluedToAToken(t *testing.T) {
	t.Parallel()
	at := "@"
	for _, text := range []string{
		`"https://u:` + madeUpGhToken + "ssh://git" + at + `h.example/o/r" names another host than github.com`,
		"https://h.example/" + madeUpGhToken + "/ssh://git" + at + "h/o names another host",
	} {
		if printed := ghPrinted(errors.New(text)); strings.Contains(printed, madeUpGhToken) {
			t.Errorf("PrintRefusal(%q) wrote %q, which holds the token", text, printed)
		}
	}
}

// A `git remote -v` line `legion gh` cannot read as gh does - a remote URL holding whitespace,
// where the URL rule ends a URL - is refused by the remote's name alone, the line not printed: a
// token before a space and an "@", a token holding a space, an scp-like remote with a token user
// and a space, and a URL followed by a second one. Real git writes each remote.
func TestReadGitRemotesNamesAnUnreadableLineByItsRemoteAlone(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process; it changes the whole process's working directory.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	at := "@"
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	for name, remote := range map[string]string{
		"a space before the @":     "https://x-access-token:" + madeUpGhToken + " " + at + "github.com/acme/widgets",
		"a token holding a space":  "https://x-access-token:zq tok0123" + at + "github.com/o/r",
		"an scp token and a space": madeUpGhToken + at + "github.com:o/r x",
		"a second URL after it":    "https://x-access-token:" + madeUpGhToken + at + "github.com/a/b https://github.com/evilown/y",
	} {
		t.Run(name, func(t *testing.T) {
			repository := t.TempDir()
			t.Chdir(repository)
			for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = repository
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
			}
			_, err := ReadGitRemotes(context.Background(), os.Environ())
			if err == nil {
				t.Fatal("ReadGitRemotes = nil error, want the line refused")
			}
			printed := ghPrinted(err)
			for _, part := range []string{madeUpGhToken, "tok0123", "zq tok"} {
				if strings.Contains(printed, part) {
					t.Errorf("legion gh printed %q, which holds %q", printed, part)
				}
			}
			if !strings.Contains(printed, `for the remote "origin"`) {
				t.Errorf("legion gh printed %q, want the remote named", printed)
			}
		})
	}
}

// An ssh remote's user name alone (`git@host:owner/repo`, `ssh://git@host/…`) prints as the URL
// rule writes it (redact.URL), its host with it, as every other user info does: the rule cannot
// tell an SSH login from a token written as the user (`<token>@gh-work:owner/repo`), and
// cmd/legion's newRoot passes every command's stderr through the URL rule whatever remoteShown
// writes. remoteShown reads the URL as gh reads it (ghParseGitURL), which drops a port and reads
// git+ssh and the scp-like form as ssh, and keeps the path, the repository it names: each row
// names that reading, which the parse must give, and the rule's text of it is read from the rule,
// never written out here. No row prints a user name, a login alone included. An ssh remote on
// another host, which the alias-host rule refuses, is named by its git name alone: its path is no
// checked OWNER/REPO.
func TestLegionGhPrintsAnSSHRemotesLoginAsTheMark(t *testing.T) {
	t.Parallel()
	at := "@"
	for raw, gh := range map[string]string{
		"git" + at + "gh-work:sjawhar/legion.git":                  "ssh://git" + at + "gh-work/sjawhar/legion.git",
		madeUpGhToken + at + "gh-work:sjawhar/legion.git":          "ssh://" + madeUpGhToken + at + "gh-work/sjawhar/legion.git",
		"ssh://git" + at + "github.com/sjawhar/legion.git":         "ssh://git" + at + "github.com/sjawhar/legion.git",
		"ssh://git" + at + "ghe.example.com:2222/acme/widgets":     "ssh://git" + at + "ghe.example.com/acme/widgets",
		"git+ssh://deploy" + at + "github.com/acme/widgets":        "ssh://deploy" + at + "github.com/acme/widgets",
		"ssh://git:zqpass0123" + at + "gh-work/sjawhar/legion.git": "ssh://git:zqpass0123" + at + "gh-work/sjawhar/legion.git",
	} {
		if u, err := ghParseGitURL(raw); err != nil || u.String() != gh {
			t.Errorf("ghParseGitURL(%q) = %v, %v; want %q, the URL as gh reads it", raw, u, err, gh)
		}
		got := remoteShown(raw)
		if want := redact.URL(gh); got != want {
			t.Errorf("remoteShown(%q) = %q, want %q, the rule's text of %q", raw, got, want, gh)
		}
		// Every row's user info ends in an "@", the login alone included: none may print.
		if strings.Contains(got, at) {
			t.Errorf("remoteShown(%q) = %q, which holds the remote's user info", raw, got)
		}
		for _, credential := range []string{madeUpGhToken, "zqpass0123", "deploy"} {
			if strings.Contains(got, credential) {
				t.Errorf("remoteShown(%q) = %q, which holds %q", raw, got, credential)
			}
		}
	}
	s := &selectors{}
	err := s.fromWorkingDirectory(func() ([]Remote, error) {
		return []Remote{{name: "work", url: madeUpGhToken + at + "gh-work:sjawhar/legion.git"}}, nil
	}, false)
	if err == nil {
		t.Fatal("fromWorkingDirectory = nil, want the alias-host refusal")
	}
	if printed := ghPrinted(err); strings.Contains(printed, madeUpGhToken) || !strings.Contains(printed, `remote "work" `+redact.URLNotPrinted+` is an ssh remote whose host`) {
		t.Errorf("legion gh printed %q, want the refused remote named by its git name alone and no token", printed)
	}
}

// PrintRefusal writes every URL through the URL rule: a URL with user info and a password, one
// with a query, an ssh URL whose user holds a password, and an ssh URL whose user is a name alone,
// each lose their user info and query.
func TestPrintGhRefusalWritesEveryURLThroughTheRule(t *testing.T) {
	t.Parallel()
	at := "@"
	tokenUser := "ssh://" + madeUpGhToken + at + "h.example/o/r.git"
	text := "a https://u:" + madeUpGhToken + at + "h.example/x, b https://h.example/x?t=" + madeUpGhToken +
		", c ssh://u:" + madeUpGhToken + at + "h.example/x, d " + tokenUser + " and e ssh://git" + at + "h.example/o?t=" + madeUpGhToken
	printed := ghPrinted(errors.New(text))
	if strings.Contains(printed, madeUpGhToken) {
		t.Errorf("PrintRefusal wrote %q, which holds the token", printed)
	}
	// The rule's text of the URL, read from the rule and never written out here.
	if want := "d " + redact.URLCredentials(tokenUser) + " and"; !strings.Contains(printed, want) {
		t.Errorf("PrintRefusal wrote %q, want %q", printed, want)
	}
}
