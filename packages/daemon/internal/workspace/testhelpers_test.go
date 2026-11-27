package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// testClock is the clock of a fetch, a cache stream or a fill in a test: each wait passes at once
// and moves it on by its length and overshoot, as a timer firing late does, and as advance does for
// the time an attempt takes. It records every wait it was asked for.
type testClock struct {
	now       time.Time
	overshoot time.Duration
	waits     []time.Duration
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 13, 17, 49, 23, 0, time.UTC)}
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.advance(d + c.overshoot)
	c.waits = append(c.waits, d)
	return ctx.Err()
}

func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := execfile.Write(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

const testInstallationToken = "test-installation-token"

// fetchRequest is the repository cache's fetch request: the cache's volume and the credential
// directory are the test's own, and its retries run on the wall clock.
func fetchRequest(t *testing.T) FetchRequest {
	t.Helper()
	return FetchRequest{
		Repo: ghrepo.MustParse("acme/widgets"), Token: staticToken(testInstallationToken),
		CredentialDir: t.TempDir(), Root: t.TempDir(),
		Log: func(string) {}, Clock: clock.Real(),
	}
}

func isSharedCloneStep(argv []string) bool {
	return commandWith(argv, "jj", "git", "clone") || commandWith(argv, "jj", "git", "init", "--colocate")
}
