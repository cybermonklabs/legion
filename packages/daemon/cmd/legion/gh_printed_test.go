package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/redact"
)

// madeUpGhToken is the made-up token these rows write where a person or a CI checkout could.
const madeUpGhToken = "zqtoken0123"

// ghPrinted is err as legion gh writes it to stderr (printGhRefusal).
func ghPrinted(err error) string {
	var stderr bytes.Buffer
	printGhRefusal(&stderr, err)
	return stderr.String()
}

// A token in a URL a person puts where legion gh reads a repository - GH_REPO, -R, a remote's
// gh-resolved marker - reaches the refusal that names that value, and legion gh writes the refusal
// through the URL rule, as every other command's (printRefusal): the line holds no part of the
// token, and still says what was refused. A selector is named by its kind alone
// (repositoryTarget): a URL with a user name or a password, on github.com or another host, and on
// another host with a token in its path; a value with no scheme, which the URL rule cannot read
// (a user name and password before its host, a token as its user, a token in its query, on another
// host or on a github.com one), which is refused, not admitted. A marker's value other than `base`
// or the OWNER/REPO gh writes prints as no URL (markerShown), and so does a refused remote's URL
// (ghRemote.what), whose path no rule has read as a repository's. A remote whose repository
// segment holds a token (written into the URL's path, as a user name and password, as a token and
// an "@", as a token and a %40, or in an scp-like URL), beside an upstream of another served owner,
// is refused as naming no OWNER/REPO, so the refusal of two owners, which prints the owner and name
// a remote names, never reaches it; a remote with a token in its path and no "@", where the URL
// rule finds no user info, is named by its git name alone; and a marker naming acme/widgets with a
// token after it, beside an upstream of another owner, prints none of it in the two-owner refusal.
func TestLegionGhPrintsNoTokenOfASelectorOrAMarker(t *testing.T) {
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
	marker := func(value string) func() ([]ghRemote, error) {
		return func() ([]ghRemote, error) {
			return []ghRemote{{name: "origin", url: "https://github.com/acme/widgets.git"},
				ghMarkerLine("remote.origin.gh-resolved " + value)}, nil
		}
	}
	const kindWords = "holds an \"@\", a \":\", a \"?\" or a \"#\" where a GitHub repository's name holds none"
	schemeless := map[string]string{
		"a user name and a token":             "x-access-token:" + madeUpGhToken + at + "ghe.example.com/acme/widgets",
		"a token as the user":                 madeUpGhToken + at + "ghe.example.com/acme/widgets",
		"a token in the query":                "ghe.example.com/acme/widgets?t=" + madeUpGhToken,
		"a token in the fragment":             "ghe.example.com/acme/widgets#" + madeUpGhToken,
		"a user and password on www.github":   "x-access-token:" + madeUpGhToken + at + "www.github.com/acme/widgets",
		"a user and password on github.com":   "x-access-token:" + madeUpGhToken + at + "github.com/acme/widgets",
		"an scp-like token":                   madeUpGhToken + at + "github.com:acme/widgets",
		"a space before the @, with a scheme": "https://x-access-token:zq tok0123" + at + "github.com/acme/widgets",
	}
	// A marker value markerShown does not print is named by the rule's own words for a URL not
	// printed (redact.URLNotPrinted), read from the rule, never written out here.
	markerNotPrinted := "remote.origin.gh-resolved " + redact.URLNotPrinted
	type row struct {
		argv    []string
		env     map[string]string
		remotes func() ([]ghRemote, error)
		names   string
	}
	rows := map[string]row{
		"GH_REPO with user info":      {[]string{"pr", "list"}, with(map[string]string{"GH_REPO": tokenURL("ghe.example.com")}), noGitRepository, "the GH_REPO value holds a user name or a password"},
		"-R with user info":           {[]string{"pr", "list", "-R", tokenURL("ghe.example.com")}, with(nil), noGitRepository, "a -R value holds a user name or a password"},
		"-R with user info on github": {[]string{"pr", "list", "-R", tokenURL("github.com")}, with(nil), noGitRepository, "a -R value holds a user name or a password"},
		"-R on another host":          {[]string{"pr", "list", "-R", "https://ghe.example.com/" + madeUpGhToken + "/widgets"}, with(nil), noGitRepository, "a -R value names another host than github.com"},
		"GH_REPO on another host":     {[]string{"pr", "list"}, with(map[string]string{"GH_REPO": "ghe.example.com/" + madeUpGhToken + "/widgets"}), noGitRepository, "the GH_REPO value names another host than github.com"},
		"a gh-resolved marker's URL":  {[]string{"pr", "list"}, with(nil), marker(tokenURL("ghe.example.com")), markerNotPrinted},
		"an scp-form marker": {[]string{"pr", "list"}, with(nil), marker("x-access-token:" + madeUpGhToken + at + "ghe.example.com:acme/widgets"),
			markerNotPrinted},
		"a marker with no scheme": {[]string{"pr", "list"}, with(nil), marker("x-access-token:" + madeUpGhToken + at + "ghe.example.com/acme/widgets"),
			markerNotPrinted},
		"a marker with a space before the @": {[]string{"pr", "list"}, with(nil), marker("https://x:zq tok0123" + at + "ghe.example.com/acme/widgets"),
			markerNotPrinted},
		"a marker with a token in the query": {[]string{"pr", "list"}, with(nil), marker("ghe.example.com/acme/widgets?t=" + madeUpGhToken),
			markerNotPrinted},
	}
	beside := func(origin string) func() ([]ghRemote, error) {
		return func() ([]ghRemote, error) {
			return []ghRemote{{name: "origin", url: origin}, {name: "upstream", url: "https://github.com/other/app"}}, nil
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
		func() ([]ghRemote, error) {
			return []ghRemote{ghMarkerLine("remote.origin.gh-resolved github.com/acme/widgets/" + madeUpGhToken), {name: "upstream", url: "https://github.com/other/app"}}, nil
		}, "acme (the working directory's " + markerNotPrinted + " naming acme/widgets)"}
	for shape, value := range schemeless {
		rows["-R, "+shape] = row{[]string{"pr", "list", "-R", value}, with(nil), noGitRepository, "a -R value " + kindWords}
		rows["GH_REPO, "+shape] = row{[]string{"pr", "list"}, with(map[string]string{"GH_REPO": value}), noGitRepository, "the GH_REPO value " + kindWords}
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			_, err := ghTargetOfKept(t, tc.argv, tc.env, savedGhHelp, tc.remotes, ownersOwnDirectories)
			if err == nil {
				t.Fatal("ghTargetOf = nil error, want a refusal")
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

// A value a person gives where the command has no place for it is named by its position or, an
// option the help does not list, by its place, never by its value or its name: a secret given to
// `gh secret set` without --body, a misspelt option holding one (`--bdy=<secret>`, a short
// `-Z<secret>`, `--body <secret>` as one argument), and a header given to gh api without -H. Real
// gh, refusing each, prints none of it.
func TestLegionGhNamesAStrayArgumentOrOptionWithoutItsValue(t *testing.T) {
	const secret = "ghp_zqtoken0123"
	pane := map[string]string{"GH_CONFIG_DIR": "/owners/acme", "LEGION_GH_OWNERS_DIR": "/owners"}
	for name, tc := range map[string]struct {
		argv  []string
		names string
	}{
		"a secret without --body":                 {[]string{"secret", "set", "DEPLOY_TOKEN", secret, "-R", "acme/widgets"}, "the 2nd argument has no place in `gh secret set`'s usage"},
		"a misspelt long option":                  {[]string{"secret", "set", "DEPLOY_TOKEN", "--bdy=" + secret, "-R", "acme/widgets"}, "an option the command's help does not list is given to `gh secret set`"},
		"a misspelt short option":                 {[]string{"secret", "set", "DEPLOY_TOKEN", "-Z" + secret, "-R", "acme/widgets"}, "a short option the command's help does not list is given to `gh secret set`"},
		"an option and its value as one argument": {[]string{"secret", "set", "DEPLOY_TOKEN", "--body " + secret, "-R", "acme/widgets"}, "an option the command's help does not list is given to `gh secret set`"},
		"a header without -H":                     {[]string{"api", "repos/acme/widgets", "Authorization: token " + secret}, "the 2nd argument has no place in `gh api`'s usage"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ghTargetOfKept(t, tc.argv, pane, savedGhHelp, noGitRepository, ownersOwnDirectories)
			if err == nil {
				t.Fatal("ghTargetOf = nil error, want a refusal")
			}
			printed := ghPrinted(err)
			if strings.Contains(printed, "zqtoken") {
				t.Errorf("legion gh printed %q, which holds the secret", printed)
			}
			if !strings.Contains(printed, tc.names) {
				t.Errorf("legion gh printed %q, want it to name %q", printed, tc.names)
			}
		})
	}
}

// An ssh URL glued to a URL holding a token goes through the URL rule with it, so no part of the
// token prints.
func TestPrintGhRefusalLeavesNoLoginGluedToAToken(t *testing.T) {
	at := "@"
	for _, text := range []string{
		`"https://u:` + madeUpGhToken + "ssh://git" + at + `h.example/o/r" names another host than github.com`,
		"https://h.example/" + madeUpGhToken + "/ssh://git" + at + "h/o names another host",
	} {
		if printed := ghPrinted(errors.New(text)); strings.Contains(printed, madeUpGhToken) {
			t.Errorf("printGhRefusal(%q) wrote %q, which holds the token", text, printed)
		}
	}
}

// A URL-scoped git config key (`http.<url>.proxy`) is refused by its key, and a token written in
// that URL prints in no part of the line legion gh writes, though the refusal names the key twice
// (the advice to unset it among them). Real git sets the key.
func TestLegionGhPrintsNoTokenOfAURLScopedGitConfigKey(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	repository := t.TempDir()
	t.Chdir(repository)
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	key := "http.https://ghuser:" + madeUpGhToken + "@" + "ghe.example.com/.proxy"
	for _, args := range [][]string{{"init", "-q"}, {"config", key, "http://proxy.example:3128"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repository, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	err := ghRepoGitConfigRefusal(context.Background(), env)
	if err == nil {
		t.Fatal("ghRepoGitConfigRefusal = nil, want the key refused")
	}
	printed := ghPrinted(err)
	if strings.Contains(printed, madeUpGhToken) {
		t.Errorf("legion gh printed %q, which holds the token", printed)
	}
	// The key as the URL rule writes it, read from the rule and never written out here.
	if !strings.Contains(printed, redact.URLCredentials(key)) {
		t.Errorf("legion gh printed %q, want the key named with its URL as the mark", printed)
	}
}

// A `git remote -v` line legion gh cannot read as gh does - a remote URL holding whitespace, where
// the URL rule ends a URL - is refused by the remote's name alone, the line not printed: a token
// before a space and an "@", a token holding a space, an scp-like remote with a token user and a
// space, and a URL followed by a second one. Real git writes each remote.
func TestReadGitRemotesNamesAnUnreadableLineByItsRemoteAlone(t *testing.T) {
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
			_, err := readGitRemotes(context.Background())
			if err == nil {
				t.Fatal("readGitRemotes = nil error, want the line refused")
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
// tell an SSH login from a token written as the user (`<token>@gh-work:owner/repo`), and newRoot
// passes every command's stderr through the URL rule whatever remoteShown writes. remoteShown reads
// the URL as gh reads it, which drops a port and reads git+ssh as ssh, and keeps the path, the
// repository it names: each row names that reading, and the rule's text of it is read from the
// rule, never written out here. An ssh remote on another host, which the alias-host rule refuses,
// is named by its git name alone: its path is no checked OWNER/REPO.
func TestLegionGhPrintsAnSSHRemotesLoginAsTheMark(t *testing.T) {
	at := "@"
	for raw, gh := range map[string]string{
		"git" + at + "gh-work:sjawhar/legion.git":                  "ssh://git" + at + "gh-work/sjawhar/legion.git",
		madeUpGhToken + at + "gh-work:sjawhar/legion.git":          "ssh://" + madeUpGhToken + at + "gh-work/sjawhar/legion.git",
		"ssh://git" + at + "github.com/sjawhar/legion.git":         "ssh://git" + at + "github.com/sjawhar/legion.git",
		"ssh://git" + at + "ghe.example.com:2222/acme/widgets":     "ssh://git" + at + "ghe.example.com/acme/widgets",
		"git+ssh://deploy" + at + "github.com/acme/widgets":        "ssh://deploy" + at + "github.com/acme/widgets",
		"ssh://git:zqpass0123" + at + "gh-work/sjawhar/legion.git": "ssh://git:zqpass0123" + at + "gh-work/sjawhar/legion.git",
	} {
		got := remoteShown(raw)
		if want := redact.URL(gh); got != want {
			t.Errorf("remoteShown(%q) = %q, want %q, the rule's text of %q", raw, got, want, gh)
		}
		for _, credential := range []string{madeUpGhToken, "zqpass0123", "deploy"} {
			if strings.Contains(got, credential) {
				t.Errorf("remoteShown(%q) = %q, which holds %q", raw, got, credential)
			}
		}
	}
	s := &selectors{}
	err := s.fromWorkingDirectory(func() ([]ghRemote, error) {
		return []ghRemote{{name: "work", url: madeUpGhToken + at + "gh-work:sjawhar/legion.git"}}, nil
	}, false)
	if err == nil {
		t.Fatal("fromWorkingDirectory = nil, want the alias-host refusal")
	}
	if printed := ghPrinted(err); strings.Contains(printed, madeUpGhToken) || !strings.Contains(printed, `remote "work" `+redact.URLNotPrinted+` is an ssh remote whose host`) {
		t.Errorf("legion gh printed %q, want the refused remote named by its git name alone and no token", printed)
	}
}

// printGhRefusal writes every URL through the URL rule: a URL with user info and a password, one
// with a query, an ssh URL whose user holds a password, and an ssh URL whose user is a name alone,
// each lose their user info and query.
func TestPrintGhRefusalWritesEveryURLThroughTheRule(t *testing.T) {
	at := "@"
	glued := "ssh://" + madeUpGhToken + at + "h.example/o/r.git"
	text := "a https://u:" + madeUpGhToken + at + "h.example/x, b https://h.example/x?t=" + madeUpGhToken +
		", c ssh://u:" + madeUpGhToken + at + "h.example/x, d " + glued + " and e ssh://git" + at + "h.example/o?t=" + madeUpGhToken
	printed := ghPrinted(errors.New(text))
	if strings.Contains(printed, madeUpGhToken) {
		t.Errorf("printGhRefusal wrote %q, which holds the token", printed)
	}
	// The rule's text of the URL, read from the rule and never written out here.
	if want := "d " + redact.URLCredentials(glued) + " and"; !strings.Contains(printed, want) {
		t.Errorf("printGhRefusal wrote %q, want %q", printed, want)
	}
}
