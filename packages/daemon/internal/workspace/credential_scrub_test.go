package workspace

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// echoesTheToken answers a command whose argv holds word as a git does that echoed its credentialed
// URL and then failed: stderr naming the token, with exit 128, or with exit -1 and end, the error
// of a signal's death. Every other command succeeds, `jj config get` with `false`.
type echoesTheToken struct {
	word, token string
	end         error
}

func (r echoesTheToken) Timeout() time.Duration { return time.Minute }

func (r echoesTheToken) Run(_ context.Context, command Command) (Result, error) {
	switch {
	case slices.Contains(command.Argv, r.word):
		said := "fatal: unable to access 'https://x-access-token:" + r.token + "@github.com/acme/widgets/': The requested URL returned error: 502"
		if r.end != nil {
			return Result{ExitCode: -1, Stderr: said}, r.end
		}
		return Result{ExitCode: 128, Stderr: said}, nil
	case slices.Contains(command.Argv, "get"):
		return Result{Stdout: "false\n"}, nil
	}
	return Result{}, nil
}

// What a run under the one-shot credential returns carries no token, whether the run exited or a
// signal ended it: the shared clone's fetch (ensureFetchConfiguration) and its clone from GitHub
// (ensureRepoClone's `jj git clone`) scrub their stderr, as Fetch's transfers do, before an error
// is made from it, through commandFailure for an exit and through runFailure for a signal's death.
func TestARunUnderTheProvisioningCredentialReturnsItsTokenScrubbed(t *testing.T) {
	const token = "ghs_provisioningTokenThatMustNotLeak"
	killed := exec.Command("sh", "-c", "kill -KILL $$").Run()
	for _, ending := range []struct {
		name string
		end  error
	}{{"exited 128", nil}, {"killed", killed}} {
		t.Run("the shared clone's fetch, "+ending.name, func(t *testing.T) {
			source, err := newProvisioningCredential(t.TempDir(), token)
			if err != nil {
				t.Fatal(err)
			}
			err = ensureFetchConfiguration(context.Background(), echoesTheToken{word: "fetch", token: token, end: ending.end}, t.TempDir(), source)
			if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "x-access-token:***@github.com") {
				t.Errorf("ensureFetchConfiguration = %v, want the failure with its token scrubbed", err)
			}
		})
		t.Run("the clone from GitHub, "+ending.name, func(t *testing.T) {
			source, err := newProvisioningCredential(t.TempDir(), token)
			if err != nil {
				t.Fatal(err)
			}
			cloneDir := filepath.Join(t.TempDir(), "repos", "acme", "widgets")
			err = ensureRepoClone(context.Background(), echoesTheToken{word: "clone", token: token, end: ending.end}, WallClock{}, cloneDir, "https://github.com/acme/widgets", "main", source)
			if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "x-access-token:***@github.com") {
				t.Errorf("ensureRepoClone = %v, want the failure with its token scrubbed", err)
			}
		})
	}
}
