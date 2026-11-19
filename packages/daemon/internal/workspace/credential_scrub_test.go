package workspace

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// echoesTheToken answers a command whose argv holds word as a git does that echoed its credentialed
// URL and then exited 128: stderr naming the token. Every other command succeeds, `jj config get`
// with `false`.
type echoesTheToken struct {
	word, token string
}

func (r echoesTheToken) Timeout() time.Duration { return time.Minute }

func (r echoesTheToken) Run(_ context.Context, command Command) (Result, error) {
	switch {
	case slices.Contains(command.Argv, r.word):
		said := "fatal: unable to access 'https://x-access-token:" + r.token + "@github.com/acme/widgets/': The requested URL returned error: 502"
		return Result{ExitCode: 128, Stderr: said}, nil
	case slices.Contains(command.Argv, "get"):
		return Result{Stdout: "false\n"}, nil
	}
	return Result{}, nil
}

// What a run under the one-shot credential returns carries no token: the shared clone's fetch
// (ensureFetchConfiguration) and its clone from GitHub (ensureRepoClone's `jj git clone`) scrub
// their stderr, as Fetch's transfers do, before an error is made from it. Both put the token in
// their error through commandFailure when git printed its credentialed URL and failed.
func TestARunUnderTheProvisioningCredentialReturnsItsTokenScrubbed(t *testing.T) {
	const token = "ghs_provisioningTokenThatMustNotLeak"
	t.Run("the shared clone's fetch", func(t *testing.T) {
		source, err := newProvisioningCredential(t.TempDir(), token)
		if err != nil {
			t.Fatal(err)
		}
		err = ensureFetchConfiguration(context.Background(), echoesTheToken{word: "fetch", token: token}, t.TempDir(), source)
		if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "x-access-token:***@github.com") {
			t.Errorf("ensureFetchConfiguration = %v, want the failure with its token scrubbed", err)
		}
	})
	t.Run("the clone from GitHub", func(t *testing.T) {
		source, err := newProvisioningCredential(t.TempDir(), token)
		if err != nil {
			t.Fatal(err)
		}
		cloneDir := filepath.Join(t.TempDir(), "repos", "acme", "widgets")
		err = ensureRepoClone(context.Background(), echoesTheToken{word: "clone", token: token}, WallClock{}, cloneDir, "https://github.com/acme/widgets", "main", source)
		if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "x-access-token:***@github.com") {
			t.Errorf("ensureRepoClone = %v, want the failure with its token scrubbed", err)
		}
	})
}
