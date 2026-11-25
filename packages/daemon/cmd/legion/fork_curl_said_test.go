package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// TestAFailedForkReadPrintsNoRunOfTheTokenCurlSaid: the credential helper's fork read sends
// GH_CONFIG_DIR's token to curl, and a curl that heard an answer and then failed says why on its
// standard error, which can hold the token whatever answered in GitHub's place echoed. The
// helper's refusal quotes those words, and git shows the refusal, so it quotes them only as the
// rule reads them (leakguard.CurlResponse.Said), every run of the token redacted. The twin shows
// curl's own standard error, as the read hands it back, holds the token.
func TestAFailedForkReadPrintsNoRunOfTheTokenCurlSaid(t *testing.T) {
	const token = "ghs_fork_read_" + oauth2calltest.Token
	root := filepath.Join(t.TempDir(), "gh")
	if _, err := ghconfig.WriteBundle(root, ghconfig.Bundle{Default: "acme", Dirs: map[string]ghconfig.Rendered{
		"acme": ghconfig.Render(token, "implement", time.Now().Add(time.Hour)),
	}}); err != nil {
		t.Fatal(err)
	}
	const slug = "outside-author/sample-project"
	const failure = "curl: (56) Failure when receiving data from the peer"
	github := newFakeGitHub(t, token, `printf 'HTTP/2 200\r\n'`+"\n"+
		"printf '%s\\n' "+shellWord(failure+", after the request sent "+token)+" >&2\nexit 56\n")
	useForkRead(t, github.program, forkReadBound)
	t.Run("twin", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), forkReadBound)
		defer cancel()
		response, err := leakguard.New(leakguard.Options{Curl: github.program}).CurlGet(ctx, "repos/"+slug, token)
		if err != nil {
			t.Fatal(err)
		}
		if response.Exit != 56 || !strings.Contains(response.Stderr, token) {
			t.Fatalf("the stand-in curl exited %d saying %q, want exit 56 and the read's token in what it said, so the row can see a leak", response.Exit, response.Stderr)
		}
	})
	t.Run("stderr", func(t *testing.T) {
		forkReadPane(t, root, ghconfig.Dir(root, "acme"))
		code, stdout, stderr := runGitCredentialHere(t, forkRequest(slug), "get")
		oauth2calltest.RequireNoPiece(t, "legion-cli.fork-curl-stderr", stderr, token)
		answersNothingForTheFork(t, code, stdout, stderr, root, slug, "curl's read of "+slug+" exited 56: "+failure)
	})
}
