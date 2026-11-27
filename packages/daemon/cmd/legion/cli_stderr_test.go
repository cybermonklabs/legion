package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// stderrToken is the fake credential these tests put in a URL; no real token is ever used.
const stderrToken = "FAKEtok3nZQ9x"

// What `legion gh` prints itself passes the URL rule on its way to stderr (newRoot). It refuses a
// call in a checkout whose remotes name two owners and names each remote by its URL as `git
// remote -v` lists it (ghroute.Remote.what), and a remote's URL can carry a token, as a remote
// added as https://x-access-token:<token>@github.com/... does: the refusal names that remote by
// its URL with no credential, and holds no token.
func TestLegionGhPrintsNoCredentialARemotesURLCarries(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	stub, _ := visibilityReadGh(t, ":\n")
	repository := t.TempDir()
	fixture := fixturegit.Isolated(t, t.TempDir())
	for _, args := range [][]string{
		fixturegit.InitWorkTree("", "")[1:],
		{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
		{"remote", "add", "tmp", "https://x-access-token:" + stderrToken + "@github.com/sjawhar/legion.git"},
	} {
		fixture.Run(repository, append([]string{"git"}, args...)...)
	}
	code, stdout, stderr := legionGhIn(t, repository, map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"GH_TOKEN": "", "GITHUB_TOKEN": "", "XDG_STATE_HOME": t.TempDir(),
	}, "", "pr", "list", "--json", "title")
	shown := strings.ReplaceAll(stdout+stderr, stderrToken, "<the token>")
	if code != 2 {
		t.Errorf("legion gh pr list = exit %d, %q; want exit 2", code, shown)
	}
	if strings.Contains(stdout+stderr, stderrToken) {
		t.Errorf("legion gh printed the token a remote's URL carries: %s", shown)
	}
	if want := `remote "tmp" (https://` + redact.URLWithCredentials + `/sjawhar/legion.git)`; !strings.Contains(stderr, want) {
		t.Errorf("legion gh printed %q, want it to name %s", shown, want)
	}
}

// What any command prints itself passes the URL rule on its way to stderr (newRoot), cobra's own
// refusal of a flag's value among it: `legion state --port <url>` quotes the value it could not
// read, and prints that URL with no credential.
func TestACommandsFlagRefusalPrintsNoCredentialItsValueCarries(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"legion", "state", "--port", "https://x-access-token:" + stderrToken + "@github.com/acme/widgets.git"}, &stdout, &stderr)
	shown := strings.ReplaceAll(stdout.String()+stderr.String(), stderrToken, "<the token>")
	if code != 2 {
		t.Errorf("legion state --port <url> = exit %d, %q; want exit 2", code, shown)
	}
	if strings.Contains(stdout.String()+stderr.String(), stderrToken) {
		t.Errorf("legion state printed the token its flag's value carries: %s", shown)
	}
	if want := `"https://` + redact.URLWithCredentials + `/acme/widgets.git"`; !strings.Contains(stderr.String(), want) {
		t.Errorf("legion state printed %q, want it to quote the value as %s", shown, want)
	}
}
