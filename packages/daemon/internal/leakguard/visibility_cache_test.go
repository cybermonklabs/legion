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

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// visibilityTestClock is a clocktest.Fake that a wait moves on by its length at once, recording
// it: Visibility waits on the test's own goroutine, where the Fake's own Sleep would wait for a move
// nobody makes. Every other timer, ls-remote's own cap among them, waits for the test's move.
type visibilityTestClock struct {
	*clocktest.Fake
	waits []time.Duration
}

func newVisibilityTestClock(start time.Time) *visibilityTestClock {
	return &visibilityTestClock{Fake: clocktest.New(start)}
}

func (c *visibilityTestClock) Sleep(ctx context.Context, wait time.Duration) error {
	c.waits = append(c.waits, wait)
	c.Advance(wait)
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

// boundPastAWait is a caller's bound on clock (boundOn) with room for the guard's wait for a near
// reset of at most rateLimitWaitBound and the reads after it, which the guard waits only where the
// caller's bound leaves (waitFits). The guard judges that room on its clock, the test's, so the
// room is the same however long the machine takes; the test clock's wait itself takes no time.
func boundPastAWait(t *testing.T, clock *visibilityTestClock) context.Context {
	t.Helper()
	return boundOn(t, clock.Fake, rateLimitWaitBound+lsRemoteBound+time.Minute)
}

// A rate limit reset close enough to the call's bound is waited out once, then the visibility is
// read again rather than treating an unchecked call as safe.
func TestVisibilityRetriesRateLimitAtNearReset(t *testing.T) {
	clock := newVisibilityTestClock(time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC))
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
	}).Visibility(boundPastAWait(t, clock), "invented-org/invented-repo")
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
			clock := newVisibilityTestClock(time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC))
			first := clock.Now().Add(time.Minute)
			latest := first.Add(time.Hour)
			bin, records := t.TempDir(), t.TempDir()
			graphql, record := filepath.Join(records, "graphql"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", tc.graphql(graphql))
			writeProgram(t, bin, "git", recordsItsCall(record)+tc.git)
			var reads int
			visibility, err := New(Options{
				Clock: clock,
				Env:   append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
				Visibility: func(context.Context, string) (Visibility, error) {
					reads++
					if reads == 1 {
						return "", &rateLimitError{reset: first}
					}
					return "", &rateLimitError{reset: latest}
				},
			}).Visibility(boundPastAWait(t, clock), "invented-org/invented-repo")
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
			clock := newVisibilityTestClock(now)
			bin, records := t.TempDir(), t.TempDir()
			graphql, record := filepath.Join(records, "graphql"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", graphQLRateLimited(graphql))
			writeProgram(t, bin, "git", recordsItsCall(record)+cannotConnect)
			var reads int
			_, err := New(Options{
				Clock: clock,
				Env:   append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
				Visibility: func(context.Context, string) (Visibility, error) {
					reads++
					return "", tc.limited
				},
			}).Visibility(testwait.Context(t), "invented-org/invented-repo")
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
