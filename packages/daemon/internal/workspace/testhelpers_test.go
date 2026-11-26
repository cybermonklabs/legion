package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

type testClock struct{ now time.Time }

func newTestClock() *testClock      { return &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }
func (c *testClock) Now() time.Time { return c.now }
func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return ctx.Err()
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

const testInstallationToken = "test-installation-token"

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
