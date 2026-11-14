package leakguard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

type visibilityTestClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *visibilityTestClock) Now() time.Time { return c.now }

func (c *visibilityTestClock) Sleep(ctx context.Context, wait time.Duration) error {
	c.waits = append(c.waits, wait)
	c.now = c.now.Add(wait)
	return ctx.Err()
}

// Kept answers can only make a guard scan more, never let it skip a scan. A cache entry outside
// the guard's owned, private directory or anything other than public is therefore a cache miss.
func TestKeptVisibilityRejectsUntrustedEntries(t *testing.T) {
	const slug = "invented-org/invented-repo"
	writeEntry := func(t *testing.T, path string, visibility Visibility) {
		t.Helper()
		encoded, err := json.Marshal(visibilityCacheEntry{Visibility: visibility})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, prepare := range map[string]func(*testing.T, string, string){
		"symlinked file": func(t *testing.T, path, dir string) {
			target := filepath.Join(t.TempDir(), "entry")
			writeEntry(t, target, Public)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"symlinked directory": func(t *testing.T, path, dir string) {
			target := t.TempDir()
			writeEntry(t, filepath.Join(target, filepath.Base(path)), Public)
			if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, dir); err != nil {
				t.Fatal(err)
			}
		},
		"group-writable directory": func(t *testing.T, path, dir string) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o770); err != nil {
				t.Fatal(err)
			}
			writeEntry(t, path, Public)
		},
		"private entry": func(t *testing.T, path, dir string) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(struct {
				Visibility Visibility `json:"visibility"`
				CachedAt   time.Time  `json:"cached_at"`
			}{Visibility: Private, CachedAt: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			guard := New(Options{Env: append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state)})
			path := guard.visibilityCachePath(slug)
			prepare(t, path, filepath.Dir(path))
			if visibility, ok := guard.keptVisibility(slug); ok {
				t.Errorf("kept visibility = %q, true; want cache miss", visibility)
			}
		})
	}
}

// A rate limit reset close enough to the call's bound is waited out once, then the visibility is
// read again rather than treating an unchecked call as safe.
func TestVisibilityRetriesRateLimitAtNearReset(t *testing.T) {
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(time.Minute)
	var reads int
	visibility, err := New(Options{
		Clock: clock,
		Env:   append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+t.TempDir()),
		Visibility: func(context.Context, string) (Visibility, error) {
			reads++
			if reads == 1 {
				return "", &rateLimitError{reset: reset}
			}
			return Public, nil
		},
	}).Visibility(bound(t), "invented-org/invented-repo")
	if err != nil || visibility != Public {
		t.Fatalf("read visibility = %q, %v; want public after the reset", visibility, err)
	}
	if reads != 2 {
		t.Errorf("the visibility was read %d times, want the failed read and one retry", reads)
	}
	if want := []time.Duration{time.Minute}; !slices.Equal(clock.waits, want) {
		t.Errorf("the waits are %v, want %v", clock.waits, want)
	}
}

// A reset beyond the two-minute limit refuses at once and tells the role when the installation's
// limit resets, instead of waiting through a call it cannot clear.
func TestVisibilityRefusesRateLimitBeyondTheWaitBound(t *testing.T) {
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(10 * time.Minute)
	var reads int
	_, err := New(Options{
		Clock: clock,
		Env:   append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+t.TempDir()),
		Visibility: func(context.Context, string) (Visibility, error) {
			reads++
			return "", &rateLimitError{reset: reset}
		},
	}).Visibility(bound(t), "invented-org/invented-repo")
	if err == nil {
		t.Fatal("the rate-limited visibility read passed")
	}
	for _, want := range []string{"installation's rate limit is spent", reset.Format(time.RFC3339)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure %q does not name %q", err, want)
		}
	}
	if reads != 1 {
		t.Errorf("the visibility was read %d times, want no retry", reads)
	}
	if len(clock.waits) != 0 {
		t.Errorf("the waits are %v, want none", clock.waits)
	}
}
