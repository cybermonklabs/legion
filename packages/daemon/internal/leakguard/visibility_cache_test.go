package leakguard

import (
	"context"
	"encoding/json"
	"errors"
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

// Kept answers in $XDG_STATE_HOME hold public answers alone, and kept private or internal answers
// live in the role's own state volume. Neither store trusts a link, a directory or file accessible
// to group or others, or an entry of the wrong kind or time.
func TestKeptVisibilityRejectsUntrustedEntries(t *testing.T) {
	const slug = "invented-org/invented-repo"
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
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
	writePrivateEntry := func(t *testing.T, path string, visibility Visibility, readAt string) {
		t.Helper()
		encoded, err := json.Marshal(struct {
			Visibility Visibility `json:"visibility"`
			ReadAt     string     `json:"read_at,omitempty"`
		}{Visibility: visibility, ReadAt: readAt})
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
			}{Visibility: Private, CachedAt: now})
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
			guard := New(Options{
				Clock:     &visibilityTestClock{now: now},
				RoleState: t.TempDir(),
				Env:       append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			})
			path := guard.visibilityCachePath(slug)
			prepare(t, path, filepath.Dir(path))
			if visibility, ok := guard.keptVisibility(slug); ok {
				t.Errorf("kept visibility = %q, true; want cache miss", visibility)
			}
		})
	}
	fresh := now.Add(-30 * time.Second).Format(time.RFC3339Nano)
	for name, prepare := range map[string]func(*testing.T, string, string){
		"role volume symlinked file": func(t *testing.T, path, dir string) {
			target := filepath.Join(t.TempDir(), "entry")
			writePrivateEntry(t, target, Private, fresh)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"role volume symlinked directory": func(t *testing.T, path, dir string) {
			target := t.TempDir()
			writePrivateEntry(t, filepath.Join(target, filepath.Base(path)), Private, fresh)
			if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, dir); err != nil {
				t.Fatal(err)
			}
		},
		"role volume group-writable directory": func(t *testing.T, path, dir string) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o770); err != nil {
				t.Fatal(err)
			}
			writePrivateEntry(t, path, Private, fresh)
		},
		"role volume other-writable directory": func(t *testing.T, path, dir string) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o777); err != nil {
				t.Fatal(err)
			}
			writePrivateEntry(t, path, Private, fresh)
		},
		"role volume group-readable file": func(t *testing.T, path, dir string) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writePrivateEntry(t, path, Private, fresh)
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		},
		"role volume public entry": func(t *testing.T, path, dir string) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writePrivateEntry(t, path, Public, fresh)
		},
		"role volume entry without read_at": func(t *testing.T, path, dir string) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writePrivateEntry(t, path, Private, "")
		},
		"role volume entry with future read_at": func(t *testing.T, path, dir string) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writePrivateEntry(t, path, Private, now.Add(time.Second).Format(time.RFC3339Nano))
		},
	} {
		t.Run(name, func(t *testing.T) {
			roleState := t.TempDir()
			guard := New(Options{
				Clock:     &visibilityTestClock{now: now},
				RoleState: roleState,
				Env:       append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+t.TempDir()),
			})
			path := guard.privateVisibilityCachePath(slug)
			prepare(t, path, filepath.Dir(path))
			if visibility, ok := guard.keptVisibility(slug); ok {
				t.Errorf("kept visibility = %q, true; want cache miss", visibility)
			}
		})
	}
}

// A private or internal visibility answer is kept in the role's own state volume for 2 minutes
// from the read GitHub answered, never extended by a hit, never honoured past the bound or from
// the future, never honoured from $XDG_STATE_HOME, never written through an untrusted directory or
// entry, never written when no writable role volume exists, and bypassed by FreshVisibility.
func TestVisibilityKeepsAPrivateAnswerForItsBound(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 123456789, time.UTC)

	readPrivateEntry := func(t *testing.T, path string) (Visibility, time.Time) {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read private keep %s: %v", path, err)
		}
		var entry struct {
			Visibility Visibility `json:"visibility"`
			ReadAt     string     `json:"read_at"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatalf("unmarshal private keep %q: %v", raw, err)
		}
		at, err := time.Parse(time.RFC3339Nano, entry.ReadAt)
		if err != nil {
			t.Fatalf("parse read_at %q: %v", entry.ReadAt, err)
		}
		return entry.Visibility, at
	}

	plantPrivate := func(t *testing.T, base string, slug string, visibility Visibility, readAt time.Time) string {
		t.Helper()
		dir := filepath.Join(base, "legion-gh", "visibility")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, visibilityCacheFile(slug))
		encoded, err := json.Marshal(struct {
			Visibility Visibility `json:"visibility"`
			ReadAt     string     `json:"read_at"`
		}{Visibility: visibility, ReadAt: readAt.UTC().Format(time.RFC3339Nano)})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	assertMissing := func(t *testing.T, path string) {
		t.Helper()
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected no file at %s, got stat err %v", path, err)
		}
	}

	t.Run("no entry; REST answers private", func(t *testing.T) {
		state, roleState := t.TempDir(), t.TempDir()
		clock := &visibilityTestClock{now: t0}
		var reads int
		var sources []VisibilitySource
		guard := New(Options{
			Clock:        clock,
			RoleState:    roleState,
			OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
			Env:          append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			Visibility: func(context.Context, string) (Visibility, error) {
				reads++
				return Private, nil
			},
		})
		got, err := guard.Visibility(bound(t), theRepo)
		if err != nil || got != Private {
			t.Fatalf("Visibility = %q, %v; want private", got, err)
		}
		if reads != 1 {
			t.Errorf("reads = %d, want 1", reads)
		}
		if !slices.Equal(sources, []VisibilitySource{VisibilityFromGitHub}) {
			t.Errorf("sources = %q, want [%s]", sources, VisibilityFromGitHub)
		}
		entryPath := guard.privateVisibilityCachePath(theRepo)
		vis, at := readPrivateEntry(t, entryPath)
		if vis != Private || !at.Equal(t0) {
			t.Errorf("kept entry = (%q, %s), want (private, %s)", vis, at.Format(time.RFC3339Nano), t0.Format(time.RFC3339Nano))
		}
		info, err := os.Lstat(entryPath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("entry mode = %v, %v; want 0600", info, err)
		}
		dirInfo, err := os.Lstat(filepath.Dir(entryPath))
		if err != nil || dirInfo.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, %v; want 0700", dirInfo, err)
		}
		// Private entry is written under RoleState and nowhere under XDG_STATE_HOME.
		if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
			t.Errorf("XDG_STATE_HOME entries = %v, %v; want nothing written there", entries, err)
		}
	})

	t.Run("an entry 1 min 59 s old", func(t *testing.T) {
		state, roleState := t.TempDir(), t.TempDir()
		entryPath := plantPrivate(t, roleState, theRepo, Private, t0)
		clock := &visibilityTestClock{now: t0.Add(1*time.Minute + 59*time.Second)}
		var reads int
		var sources []VisibilitySource
		guard := New(Options{
			Clock:        clock,
			RoleState:    roleState,
			OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
			Env:          append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			Visibility: func(context.Context, string) (Visibility, error) {
				reads++
				return Public, nil
			},
		})
		got, err := guard.Visibility(bound(t), theRepo)
		if err != nil || got != Private {
			t.Fatalf("Visibility = %q, %v; want private", got, err)
		}
		if reads != 0 {
			t.Errorf("reads = %d, want 0", reads)
		}
		if !slices.Equal(sources, []VisibilitySource{VisibilityFromCache}) {
			t.Errorf("sources = %q, want [%s]", sources, VisibilityFromCache)
		}
		if vis, at := readPrivateEntry(t, entryPath); vis != Private || !at.Equal(t0) {
			t.Errorf("kept entry changed to (%q, %s), want unchanged (private, %s)", vis, at.Format(time.RFC3339Nano), t0.Format(time.RFC3339Nano))
		}
	})

	t.Run("an entry 2 min 1 s old", func(t *testing.T) {
		state, roleState := t.TempDir(), t.TempDir()
		entryPath := plantPrivate(t, roleState, theRepo, Private, t0)
		t1 := t0.Add(2*time.Minute + 1*time.Second)
		clock := &visibilityTestClock{now: t1}
		var reads int
		var sources []VisibilitySource
		guard := New(Options{
			Clock:        clock,
			RoleState:    roleState,
			OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
			Env:          append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			Visibility: func(context.Context, string) (Visibility, error) {
				reads++
				return Private, nil
			},
		})
		got, err := guard.Visibility(bound(t), theRepo)
		if err != nil || got != Private {
			t.Fatalf("Visibility = %q, %v; want private", got, err)
		}
		if reads != 1 {
			t.Errorf("reads = %d, want 1", reads)
		}
		if !slices.Equal(sources, []VisibilitySource{VisibilityFromGitHub}) {
			t.Errorf("sources = %q, want [%s]", sources, VisibilityFromGitHub)
		}
		if vis, at := readPrivateEntry(t, entryPath); vis != Private || !at.Equal(t1) {
			t.Errorf("kept entry = (%q, %s), want replaced (private, %s)", vis, at.Format(time.RFC3339Nano), t1.Format(time.RFC3339Nano))
		}
	})

	t.Run("an entry 1 min old; a call then another 1 min 30 s later", func(t *testing.T) {
		state, roleState := t.TempDir(), t.TempDir()
		entryPath := plantPrivate(t, roleState, theRepo, Private, t0)
		clock := &visibilityTestClock{now: t0.Add(1 * time.Minute)}
		var reads int
		var sources []VisibilitySource
		call := func() Visibility {
			t.Helper()
			got, err := New(Options{
				Clock:        clock,
				RoleState:    roleState,
				OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
				Env:          append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
				Visibility: func(context.Context, string) (Visibility, error) {
					reads++
					return Private, nil
				},
			}).Visibility(bound(t), theRepo)
			if err != nil {
				t.Fatalf("Visibility: %v", err)
			}
			return got
		}
		if got := call(); got != Private {
			t.Fatalf("first call = %q, want private", got)
		}
		if reads != 0 {
			t.Errorf("after first call: reads = %d, want 0", reads)
		}
		if _, at := readPrivateEntry(t, entryPath); !at.Equal(t0) {
			t.Errorf("after first call: read_at = %s, want unchanged %s", at.Format(time.RFC3339Nano), t0.Format(time.RFC3339Nano))
		}
		t2 := clock.now.Add(1*time.Minute + 30*time.Second)
		clock.now = t2
		if got := call(); got != Private {
			t.Fatalf("second call = %q, want private", got)
		}
		if reads != 1 {
			t.Errorf("after second call: reads = %d, want 1 (hit must not extend the bound)", reads)
		}
		if !slices.Equal(sources, []VisibilitySource{VisibilityFromCache, VisibilityFromGitHub}) {
			t.Errorf("sources = %q, want [%s %s]", sources, VisibilityFromCache, VisibilityFromGitHub)
		}
		if _, at := readPrivateEntry(t, entryPath); !at.Equal(t2) {
			t.Errorf("after second call: read_at = %s, want %s", at.Format(time.RFC3339Nano), t2.Format(time.RFC3339Nano))
		}
	})

	t.Run("an entry whose time is later than now", func(t *testing.T) {
		state, roleState := t.TempDir(), t.TempDir()
		entryPath := plantPrivate(t, roleState, theRepo, Private, t0.Add(10*time.Second))
		clock := &visibilityTestClock{now: t0}
		var reads int
		got, err := New(Options{
			Clock:     clock,
			RoleState: roleState,
			Env:       append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			Visibility: func(context.Context, string) (Visibility, error) {
				reads++
				return Private, nil
			},
		}).Visibility(bound(t), theRepo)
		if err != nil || got != Private {
			t.Fatalf("Visibility = %q, %v; want private", got, err)
		}
		if reads != 1 {
			t.Errorf("reads = %d, want 1", reads)
		}
		if _, at := readPrivateEntry(t, entryPath); !at.Equal(t0) {
			t.Errorf("read_at = %s, want replaced %s", at.Format(time.RFC3339Nano), t0.Format(time.RFC3339Nano))
		}
	})

	t.Run("an internal entry inside the bound", func(t *testing.T) {
		state, roleState := t.TempDir(), t.TempDir()
		entryPath := plantPrivate(t, roleState, theRepo, Internal, t0)
		clock := &visibilityTestClock{now: t0.Add(1 * time.Minute)}
		var reads int
		var sources []VisibilitySource
		got, err := New(Options{
			Clock:        clock,
			RoleState:    roleState,
			OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
			Env:          append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			Visibility: func(context.Context, string) (Visibility, error) {
				reads++
				return Public, nil
			},
		}).Visibility(bound(t), theRepo)
		if err != nil || got != Internal {
			t.Fatalf("Visibility = %q, %v; want internal", got, err)
		}
		if reads != 0 {
			t.Errorf("reads = %d, want 0", reads)
		}
		if !slices.Equal(sources, []VisibilitySource{VisibilityFromCache}) {
			t.Errorf("sources = %q, want [%s]", sources, VisibilityFromCache)
		}
		if vis, at := readPrivateEntry(t, entryPath); vis != Internal || !at.Equal(t0) {
			t.Errorf("kept entry = (%q, %s), want unchanged (internal, %s)", vis, at.Format(time.RFC3339Nano), t0.Format(time.RFC3339Nano))
		}
	})

	t.Run("no entry; the read fails", func(t *testing.T) {
		state, roleState := t.TempDir(), t.TempDir()
		clock := &visibilityTestClock{now: t0}
		var reads int
		read := func(context.Context, string) (Visibility, error) {
			reads++
			if reads == 1 {
				return "", errors.New("gh api exited 1")
			}
			return Private, nil
		}
		g1 := New(Options{
			Clock:      clock,
			RoleState:  roleState,
			Env:        append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			Visibility: read,
		})
		if _, err := g1.Visibility(bound(t), theRepo); err == nil {
			t.Fatal("first call succeeded, want error")
		}
		assertMissing(t, g1.privateVisibilityCachePath(theRepo))
		assertMissing(t, g1.visibilityCachePath(theRepo))
		got, err := New(Options{
			Clock:      clock,
			RoleState:  roleState,
			Env:        append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			Visibility: read,
		}).Visibility(bound(t), theRepo)
		if err != nil || got != Private {
			t.Fatalf("second call = %q, %v; want private", got, err)
		}
		if reads != 2 {
			t.Errorf("reads = %d, want 2", reads)
		}
	})

	t.Run("no entry; GraphQL answers private after a spent REST limit", func(t *testing.T) {
		bin, state, roleState, records := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
		readsFile, graphqlFile, gitFile := filepath.Join(records, "reads"), filepath.Join(records, "graphql"), filepath.Join(records, "git")
		reset := t0.Add(30 * time.Minute)
		writeProgram(t, bin, "gh", ghStandIn(spentRateLimit(reset, readsFile), graphQLAnswers(graphqlFile, "PRIVATE")))
		writeProgram(t, bin, "git", recordsItsCall(gitFile)+answersTheRefs)
		clock := &visibilityTestClock{now: t0}
		var sources []VisibilitySource
		call := func() (*Guard, Visibility) {
			t.Helper()
			g := New(Options{
				Clock:        clock,
				RoleState:    roleState,
				OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
				Env:          append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+state),
			})
			got, err := g.Visibility(bound(t), theRepo)
			if err != nil {
				t.Fatalf("Visibility: %v", err)
			}
			return g, got
		}
		g, got := call()
		if got != Private {
			t.Fatalf("first call = %q, want private", got)
		}
		vis, at := readPrivateEntry(t, g.privateVisibilityCachePath(theRepo))
		if vis != Private || !at.Equal(t0) {
			t.Errorf("kept entry = (%q, %s), want (private, %s)", vis, at.Format(time.RFC3339Nano), t0.Format(time.RFC3339Nano))
		}
		assertMissing(t, g.visibilityCachePath(theRepo))
		clock.now = t0.Add(1 * time.Minute)
		if _, got := call(); got != Private {
			t.Fatalf("second call = %q, want private", got)
		}
		if !slices.Equal(sources, []VisibilitySource{VisibilityFromGraphQL, VisibilityFromCache}) {
			t.Errorf("sources = %q, want [%s %s]", sources, VisibilityFromGraphQL, VisibilityFromCache)
		}
		if got := countLines(t, readsFile); got != 1 {
			t.Errorf("gh REST reads = %d, want 1", got)
		}
		if got := len(readCalls(t, graphqlFile)); got != 1 {
			t.Errorf("gh GraphQL reads = %d, want 1", got)
		}
	})

	t.Run("no entry; curl answers private after a gh transport failure", func(t *testing.T) {
		bin, state, roleState, records := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
		readsFile := filepath.Join(records, "reads")
		graphqlFile := filepath.Join(records, "graphql")
		credentialFile := filepath.Join(records, "credential")
		curlFile := filepath.Join(records, "curl")
		curlStdin := filepath.Join(records, "curl-stdin")
		gitFile := filepath.Join(records, "git")
		writeProgram(t, bin, "gh", transportGh(readsFile, ghTLSTimeout, noGraphQL(graphqlFile), credentialFile, theToken))
		writeProgram(t, bin, "curl", curlStandIn(curlFile, curlStdin, curlAnswers("private")))
		writeProgram(t, bin, "git", recordsItsCall(gitFile)+answersTheRefs)
		clock := &visibilityTestClock{now: t0}
		var sources []VisibilitySource
		call := func() (*Guard, Visibility) {
			t.Helper()
			g := New(Options{
				Clock:        clock,
				RoleState:    roleState,
				Curl:         filepath.Join(bin, "curl"),
				OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
				Env:          append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+state),
			})
			got, err := g.Visibility(bound(t), theRepo)
			if err != nil {
				t.Fatalf("Visibility: %v", err)
			}
			return g, got
		}
		g, got := call()
		if got != Private {
			t.Fatalf("first call = %q, want private", got)
		}
		vis, at := readPrivateEntry(t, g.privateVisibilityCachePath(theRepo))
		if vis != Private || !at.Equal(t0) {
			t.Errorf("kept entry = (%q, %s), want (private, %s)", vis, at.Format(time.RFC3339Nano), t0.Format(time.RFC3339Nano))
		}
		assertMissing(t, g.visibilityCachePath(theRepo))
		clock.now = t0.Add(1 * time.Minute)
		if _, got := call(); got != Private {
			t.Fatalf("second call = %q, want private", got)
		}
		if !slices.Equal(sources, []VisibilitySource{VisibilityFromCurl, VisibilityFromCache}) {
			t.Errorf("sources = %q, want [%s %s]", sources, VisibilityFromCurl, VisibilityFromCache)
		}
		if got := countLines(t, readsFile); got != 1 {
			t.Errorf("gh REST reads = %d, want 1", got)
		}
		if got := countLines(t, curlFile); got != 1 {
			t.Errorf("curl reads = %d, want 1", got)
		}
	})

	t.Run("no entry; REST answers public", func(t *testing.T) {
		state, roleState := t.TempDir(), t.TempDir()
		clock := &visibilityTestClock{now: t0}
		guard := New(Options{
			Clock:      clock,
			RoleState:  roleState,
			Env:        append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			Visibility: answers(Public, nil),
		})
		got, err := guard.Visibility(bound(t), theRepo)
		if err != nil || got != Public {
			t.Fatalf("Visibility = %q, %v; want public", got, err)
		}
		if _, err := os.Lstat(guard.visibilityCachePath(theRepo)); err != nil {
			t.Errorf("expected public keep in XDG_STATE_HOME: %v", err)
		}
		if entries, err := os.ReadDir(roleState); err != nil || len(entries) != 0 {
			t.Errorf("role volume entries = %v, %v; want no private entry", entries, err)
		}
	})

	t.Run("a private entry in XDG_STATE_HOME", func(t *testing.T) {
		state, roleState := t.TempDir(), t.TempDir()
		plantPrivate(t, state, theRepo, Private, t0)
		clock := &visibilityTestClock{now: t0.Add(10 * time.Second)}
		var reads int
		got, err := New(Options{
			Clock:     clock,
			RoleState: roleState,
			Env:       append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			Visibility: func(context.Context, string) (Visibility, error) {
				reads++
				return Public, nil
			},
		}).Visibility(bound(t), theRepo)
		if err != nil || got != Public {
			t.Fatalf("Visibility = %q, %v; want public from fresh read", got, err)
		}
		if reads != 1 {
			t.Errorf("reads = %d, want 1 (private entry in XDG_STATE_HOME must miss)", reads)
		}
	})

	t.Run("public keep in XDG_STATE_HOME wins over private keep in role volume", func(t *testing.T) {
		state, roleState := t.TempDir(), t.TempDir()
		plantPrivate(t, roleState, theRepo, Private, t0)
		pubDir := filepath.Join(state, "legion-gh", "visibility")
		if err := os.MkdirAll(pubDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pubDir, visibilityCacheFile(theRepo)), []byte(`{"visibility":"public"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var reads int
		got, err := New(Options{
			Clock:     &visibilityTestClock{now: t0.Add(10 * time.Second)},
			RoleState: roleState,
			Env:       append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			Visibility: func(context.Context, string) (Visibility, error) {
				reads++
				return Private, nil
			},
		}).Visibility(bound(t), theRepo)
		if err != nil || got != Public {
			t.Fatalf("Visibility = %q, %v; want public", got, err)
		}
		if reads != 0 {
			t.Errorf("reads = %d, want 0", reads)
		}
	})

	t.Run("untrusted keep directory or entry", func(t *testing.T) {
		t1 := t0.Add(30 * time.Second)
		const sentinel = `{"visibility":"private","read_at":"2026-11-27T12:00:00.123456789Z"}` + "\n"
		for _, tc := range []struct {
			name    string
			prepare func(t *testing.T, roleState, dir, path string) (verify func(t *testing.T))
		}{
			{
				name: "keep directory is a link",
				prepare: func(t *testing.T, _, dir, path string) func(*testing.T) {
					targetDir := t.TempDir()
					targetFile := filepath.Join(targetDir, filepath.Base(path))
					if err := os.WriteFile(targetFile, []byte(sentinel), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(targetDir, dir); err != nil {
						t.Fatal(err)
					}
					return func(t *testing.T) {
						got, err := os.ReadFile(targetFile)
						if err != nil || string(got) != sentinel {
							t.Errorf("target file = %q, %v; want untouched sentinel", got, err)
						}
						entries, err := os.ReadDir(targetDir)
						if err != nil || len(entries) != 1 {
							t.Errorf("target dir entries = %v, %v; want only the original entry", entries, err)
						}
					}
				},
			},
			{
				name: "entry is a link",
				prepare: func(t *testing.T, _, dir, path string) func(*testing.T) {
					targetFile := filepath.Join(t.TempDir(), "target")
					if err := os.WriteFile(targetFile, []byte(sentinel), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(targetFile, path); err != nil {
						t.Fatal(err)
					}
					return func(t *testing.T) {
						got, err := os.ReadFile(targetFile)
						if err != nil || string(got) != sentinel {
							t.Errorf("target file = %q, %v; want untouched sentinel", got, err)
						}
						info, err := os.Lstat(path)
						if err != nil || info.Mode()&os.ModeSymlink == 0 {
							t.Errorf("entry lstat = %v, %v; want original symlink untouched", info, err)
						}
					}
				},
			},
			{
				name: "directory is group-writable",
				prepare: func(t *testing.T, _, dir, path string) func(*testing.T) {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(sentinel), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(dir, 0o770); err != nil {
						t.Fatal(err)
					}
					return func(t *testing.T) {
						got, err := os.ReadFile(path)
						if err != nil || string(got) != sentinel {
							t.Errorf("entry = %q, %v; want untouched sentinel", got, err)
						}
						info, err := os.Lstat(dir)
						if err != nil || info.Mode().Perm() != 0o770 {
							t.Errorf("dir mode = %v, %v; want unchanged 0770", info, err)
						}
					}
				},
			},
			{
				name: "directory is other-writable",
				prepare: func(t *testing.T, _, dir, path string) func(*testing.T) {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(sentinel), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(dir, 0o777); err != nil {
						t.Fatal(err)
					}
					return func(t *testing.T) {
						got, err := os.ReadFile(path)
						if err != nil || string(got) != sentinel {
							t.Errorf("entry = %q, %v; want untouched sentinel", got, err)
						}
						info, err := os.Lstat(dir)
						if err != nil || info.Mode().Perm() != 0o777 {
							t.Errorf("dir mode = %v, %v; want unchanged 0777", info, err)
						}
					}
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				state, roleState := t.TempDir(), t.TempDir()
				dir := filepath.Join(roleState, "legion-gh", "visibility")
				path := filepath.Join(dir, visibilityCacheFile(theRepo))
				verify := tc.prepare(t, roleState, dir, path)
				var reads int
				got, err := New(Options{
					Clock:     &visibilityTestClock{now: t1},
					RoleState: roleState,
					Env:       append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
					Visibility: func(context.Context, string) (Visibility, error) {
						reads++
						return Private, nil
					},
				}).Visibility(bound(t), theRepo)
				if err != nil || got != Private {
					t.Fatalf("Visibility = %q, %v; want private", got, err)
				}
				if reads != 1 {
					t.Errorf("reads = %d, want 1", reads)
				}
				verify(t)
			})
		}
	})

	t.Run("no role volume", func(t *testing.T) {
		t.Run("missing", func(t *testing.T) {
			state := t.TempDir()
			missing := filepath.Join(t.TempDir(), "missing")
			clock := &visibilityTestClock{now: t0}
			var reads int
			for range 2 {
				got, err := New(Options{
					Clock:     clock,
					RoleState: missing,
					Env:       append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
					Visibility: func(context.Context, string) (Visibility, error) {
						reads++
						return Private, nil
					},
				}).Visibility(bound(t), theRepo)
				if err != nil || got != Private {
					t.Fatalf("Visibility = %q, %v; want private", got, err)
				}
				clock.now = clock.now.Add(10 * time.Second)
			}
			if reads != 2 {
				t.Errorf("reads = %d, want 2", reads)
			}
			assertMissing(t, missing)
			if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
				t.Errorf("XDG_STATE_HOME entries = %v, %v; want nothing written", entries, err)
			}
		})

		t.Run("not writable", func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root can write mode 0555 directories")
			}
			state := t.TempDir()
			readOnly := t.TempDir()
			if err := os.Chmod(readOnly, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
			clock := &visibilityTestClock{now: t0}
			var reads int
			for range 2 {
				got, err := New(Options{
					Clock:     clock,
					RoleState: readOnly,
					Env:       append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
					Visibility: func(context.Context, string) (Visibility, error) {
						reads++
						return Private, nil
					},
				}).Visibility(bound(t), theRepo)
				if err != nil || got != Private {
					t.Fatalf("Visibility = %q, %v; want private", got, err)
				}
				clock.now = clock.now.Add(10 * time.Second)
			}
			if reads != 2 {
				t.Errorf("reads = %d, want 2", reads)
			}
			if entries, err := os.ReadDir(readOnly); err != nil || len(entries) != 0 {
				t.Errorf("read-only role state entries = %v, %v; want nothing written", entries, err)
			}
			if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
				t.Errorf("XDG_STATE_HOME entries = %v, %v; want nothing written", entries, err)
			}
		})
	})

	t.Run("an entry inside the bound; FreshVisibility", func(t *testing.T) {
		state, roleState := t.TempDir(), t.TempDir()
		entryPath := plantPrivate(t, roleState, theRepo, Private, t0)
		clock := &visibilityTestClock{now: t0.Add(30 * time.Second)}
		var reads int
		got, err := New(Options{
			Clock:     clock,
			RoleState: roleState,
			Env:       append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+state),
			Visibility: func(context.Context, string) (Visibility, error) {
				reads++
				return Public, nil
			},
		}).FreshVisibility(bound(t), theRepo)
		if err != nil || got != Public {
			t.Fatalf("FreshVisibility = %q, %v; want public", got, err)
		}
		if reads != 1 {
			t.Errorf("reads = %d, want 1", reads)
		}
		if vis, at := readPrivateEntry(t, entryPath); vis != Private || !at.Equal(t0) {
			t.Errorf("kept entry = (%q, %s), want unchanged (private, %s)", vis, at.Format(time.RFC3339Nano), t0.Format(time.RFC3339Nano))
		}
		if entries, err := os.ReadDir(state); err != nil || len(entries) != 0 {
			t.Errorf("XDG_STATE_HOME entries = %v, %v; want nothing written by FreshVisibility", entries, err)
		}
	})
}

// boundPastAWait is a test's bound with room for the guard's wait for a near reset of at most
// rateLimitWaitBound and the reads after it, which the guard waits only where the caller's bound
// leaves (waitFits). The test clock's wait itself takes no time.
func boundPastAWait(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rateLimitWaitBound+lsRemoteBound+time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// A rate limit reset close enough to the call's bound is waited out once, then the visibility is
// read again rather than treating an unchecked call as safe.
func TestVisibilityRetriesRateLimitAtNearReset(t *testing.T) {
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(time.Minute)
	var reads int
	visibility, err := New(Options{
		Clock:     clock,
		RoleState: t.TempDir(),
		Env:       append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+t.TempDir()),
		Visibility: func(context.Context, string) (Visibility, error) {
			reads++
			if reads == 1 {
				return "", &rateLimitError{reset: reset}
			}
			return Public, nil
		},
	}).Visibility(boundPastAWait(t), "invented-org/invented-repo")
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

// A near reset whose retried read is still refused for the spent limit is not waited out again:
// the guard reads the visibility through GraphQL, and only where GraphQL's own limit is spent
// through git's ls-remote. The call waits once, makes the REST read twice and the GraphQL read
// once. GraphQL's answer stands, as does ls-remote's, and where ls-remote cannot answer either,
// the refusal names the reset of the retried read, the latest GitHub gave, rather than the one
// already waited out.
func TestVisibilityReadsWithoutRESTWhenTheRetryIsStillLimited(t *testing.T) {
	for _, tc := range []struct {
		name string
		// graphql is what the stand-in gh's GraphQL read does, given the file it records its call
		// in; git is what the stand-in git does after it records its call, gitRuns how many times
		// it runs, and want the answer of the call, empty where the call is refused.
		graphql func(record string) string
		git     string
		gitRuns int
		want    Visibility
	}{
		{
			name:    "GraphQL answers",
			graphql: func(record string) string { return graphQLAnswers(record, "PRIVATE") },
			git:     answersTheRefs,
			want:    Private,
		},
		{name: "GraphQL is rate-limited and ls-remote answers", graphql: graphQLRateLimited, git: answersTheRefs, gitRuns: 1, want: Public},
		{name: "GraphQL is rate-limited and ls-remote fails", graphql: graphQLRateLimited, git: cannotConnect, gitRuns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
			first := clock.Now().Add(time.Minute)
			latest := first.Add(time.Hour)
			bin, records := t.TempDir(), t.TempDir()
			graphql, record := filepath.Join(records, "graphql"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", tc.graphql(graphql))
			writeProgram(t, bin, "git", recordsItsCall(record)+tc.git)
			var reads int
			visibility, err := New(Options{
				Clock:     clock,
				RoleState: t.TempDir(),
				Env:       append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
				Visibility: func(context.Context, string) (Visibility, error) {
					reads++
					if reads == 1 {
						return "", &rateLimitError{reset: first}
					}
					return "", &rateLimitError{reset: latest}
				},
			}).Visibility(boundPastAWait(t), "invented-org/invented-repo")
			switch {
			case tc.want != "":
				if err != nil || visibility != tc.want {
					t.Errorf("read visibility = %q, %v; want %s", visibility, err, tc.want)
				}
			case err == nil:
				t.Errorf("read visibility = %q, want the call refused", visibility)
			default:
				if want := "the installation's rate limit is spent until " + latest.Format(time.RFC3339); !strings.Contains(err.Error(), want) {
					t.Errorf("the failure %q does not name the latest reset: %q", err, want)
				}
				if strings.Contains(err.Error(), first.Format(time.RFC3339)) {
					t.Errorf("the failure %q names the reset already waited out, %s", err, first.Format(time.RFC3339))
				}
			}
			if reads != 2 {
				t.Errorf("the REST read ran %d times, want the failed read and one retry", reads)
			}
			if want := []time.Duration{time.Minute}; !slices.Equal(clock.waits, want) {
				t.Errorf("the waits are %v, want %v", clock.waits, want)
			}
			if got := len(readCalls(t, graphql)); got != 1 {
				t.Errorf("gh's GraphQL read ran %d times, want once", got)
			}
			if got := len(readCalls(t, record)); got != tc.gitRuns {
				t.Errorf("git ran %d times, want %d", got, tc.gitRuns)
			}
		})
	}
}

// A core-limit reset beyond the two-minute limit is not waited for, nor is one already past, nor a
// core limit GitHub gave no reset for. The guard reads the visibility through GraphQL instead,
// once, and here GraphQL's own limit is spent too, so through git's ls-remote, once
// (TestVisibilityWithoutRESTDecidesThePush). Where that read cannot answer either, the call is
// refused at once and tells the role when the installation's limit resets, or that GitHub gave no
// reset, instead of waiting through a call it cannot clear. A secondary limit never goes to
// GraphQL (TestVisibilityReadsASecondaryRateLimitWithLsRemote).
func TestVisibilityRefusesRateLimitBeyondTheWaitBound(t *testing.T) {
	now := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		limited *rateLimitError
		// says is what the refusal says of the reset.
		says string
	}{
		{
			name:    "a reset ten minutes away",
			limited: &rateLimitError{reset: now.Add(10 * time.Minute)},
			says:    "the installation's rate limit is spent until " + now.Add(10*time.Minute).Format(time.RFC3339),
		},
		{
			name:    "a reset already past",
			limited: &rateLimitError{reset: now.Add(-5 * time.Minute)},
			says:    "the installation's rate limit is spent until " + now.Add(-5*time.Minute).Format(time.RFC3339),
		},
		{
			name:    "no reset",
			limited: &rateLimitError{},
			says:    "the installation's rate limit is spent and GitHub gave no reset time",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &visibilityTestClock{now: now}
			bin, records := t.TempDir(), t.TempDir()
			graphql, record := filepath.Join(records, "graphql"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", graphQLRateLimited(graphql))
			writeProgram(t, bin, "git", recordsItsCall(record)+cannotConnect)
			var reads int
			_, err := New(Options{
				Clock:     clock,
				RoleState: t.TempDir(),
				Env:       append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
				Visibility: func(context.Context, string) (Visibility, error) {
					reads++
					return "", tc.limited
				},
			}).Visibility(bound(t), "invented-org/invented-repo")
			if err == nil {
				t.Fatal("the rate-limited visibility read passed")
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the failure %q does not say %q", err, tc.says)
			}
			if reads != 1 {
				t.Errorf("the REST read ran %d times, want no retry", reads)
			}
			if len(clock.waits) != 0 {
				t.Errorf("the waits are %v, want none", clock.waits)
			}
			if got := len(readCalls(t, graphql)); got != 1 {
				t.Errorf("gh's GraphQL read ran %d times, want once", got)
			}
			if got := len(readCalls(t, record)); got != 1 {
				t.Errorf("git ran %d times, want the one ls-remote", got)
			}
		})
	}
}
