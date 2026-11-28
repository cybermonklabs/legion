package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghroute"
	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// madeUpGhToken is the made-up token these rows write where a person or a CI checkout could.
// internal/ghroute's printed_test.go holds the rows of the refusals it makes itself: of a
// selector, a marker, a remote and PrintRefusal's URL rule.
const madeUpGhToken = "zqtoken0123"

// ghPrinted is err as legion gh writes it to stderr (ghroute.PrintRefusal).
func ghPrinted(err error) string {
	var stderr bytes.Buffer
	ghroute.PrintRefusal(&stderr, err)
	return stderr.String()
}

// A value a person gives where the command has no place for it is named by its position or, an
// option the help does not list, by its place, never by its value or its name: a secret given to
// `gh secret set` without --body, a misspelt option holding one (`--bdy=<secret>`, a short
// `-Z<secret>`, `--body <secret>` as one argument), and a header given to gh api without -H. Real
// gh, refusing each, prints none of it.
func TestLegionGhNamesAStrayArgumentOrOptionWithoutItsValue(t *testing.T) {
	t.Parallel()
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
			_, err := ghTargetOfKept(t, tc.argv, pane, savedGhHelp, noGitRepository, ghroutetest.OwnersOwnDirectories)
			if err == nil {
				t.Fatal("ghroute.TargetOf = nil error, want a refusal")
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

// A URL-scoped git config key (`http.<url>.proxy`) is refused by its key, and a token written in
// that URL prints in no part of the line legion gh writes, though the refusal names the key twice
// (the advice to unset it among them). Real git sets the key.
func TestLegionGhPrintsNoTokenOfAURLScopedGitConfigKey(t *testing.T) {
	// Not parallel: it changes the whole process's working directory.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	repository := t.TempDir()
	t.Chdir(repository)
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	const user = "ghuser"
	key := "http.https://" + user + ":" + madeUpGhToken + "@" + "ghe.example.com/.proxy"
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
	if strings.Contains(printed, user) {
		t.Errorf("legion gh printed %q, which holds the user name the key's URL carries", printed)
	}
	// The key as the URL rule writes it, read from the rule and never written out here.
	if !strings.Contains(printed, redact.URLCredentials(key)) {
		t.Errorf("legion gh printed %q, want the key named with its URL as the mark", printed)
	}
}
