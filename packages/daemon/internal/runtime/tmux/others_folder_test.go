package tmux

// A folder another user owns and denies this daemon's user is no part of this daemon's session
// store, at every level the search reads: the recorded path's own folders as much as a sessions
// directory or a folder of one. A session recorded under such a folder is looked for where it was
// carried, and is missing where it was carried nowhere. /root stands for such a folder: a host's
// root user owns it with mode 0700. Each test skips where this user can read /root or owns it.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/supervise/supervisetest"
)

// requireOthersFolder skips unless /root is a folder another user owns that denies this one.
func requireOthersFolder(t *testing.T) {
	t.Helper()
	info, err := os.Stat("/root")
	if err != nil {
		t.Skipf("no /root to stand for another user's folder: %v", err)
	}
	if _, err := os.ReadDir("/root"); !errors.Is(err, os.ErrPermission) {
		t.Skipf("this user can read /root (%v), so it stands for no folder another user denies", err)
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); !ok || int(owner.Uid) == os.Geteuid() {
		t.Skip("/root is this user's own, so it stands for no folder another user owns")
	}
}

// A claim whose recorded session is under a folder another user denies, and was carried nowhere,
// starts a fresh agent at once, uncharged, as a session found nowhere does (supervise's
// startFresh); it never waits as a store fault. The control is the same claim whose recorded
// folder is simply not there.
func TestARecordedSessionInAFolderAnotherUserDeniesReadsMissingNotAStoreFault(t *testing.T) {
	requireOthersFolder(t)
	const name = "2026-10-20T10-00-00-000Z_ses-abc.jsonl"
	for label, recorded := range map[string]func(sessions string) string{
		"control: the recorded folder is not there": func(sessions string) string {
			return filepath.Join(sessions, "--gone--", name)
		},
		"the recorded path is under another user's folder": func(string) string {
			return filepath.Join("/root", ".omp", "profiles", "legion", "agent", "sessions", "--legion-workspaces-acme-widgets--", name)
		},
	} {
		t.Run(label, func(t *testing.T) {
			sessions := filepath.Join(t.TempDir(), "sessions")
			if err := os.MkdirAll(sessions, 0o700); err != nil {
				t.Fatal(err)
			}
			recorded := recorded(sessions)
			got, searched, _, err := sessionFileOf(recorded, paneAt(filepath.Join(t.TempDir(), "pane-sessions")))
			if err != nil || got != "" {
				t.Errorf("sessionFileOf(%s) = %q, %v, %v; want nothing found and no error", recorded, got, searched, err)
			}

			conns := fake.NewConns()
			f := newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) {
				o.Conns = conns
				o.Environ = append(o.Environ, "HOME="+filepath.Join(o.StateDir, "home"))
			})
			spec := f.spec(t, claim.RoleTester)
			clock := supervisetest.NewClock(time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC))
			suspended := supervise.Claim{Token: spec.Claim, Project: spec.Project, Tree: spec.Tree, Issue: spec.Issue, Role: spec.Role,
				Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByOperator,
				Session: "ses-abc", SessionFile: recorded}
			m := storeFaultMachine(t, f, conns, spec, clock, suspended)
			err = m.Handle(context.Background(), supervise.RequestResume{Claim: spec.Claim})
			m.Wait()
			c := m.Claim()
			if errors.Is(err, runtime.ErrSessionStore) || c.Budgets.LaunchFailures != 0 || !opened(f.argvs) || c.Locator == nil {
				t.Fatalf("the resume of a session recorded at %s answered %v and left the claim %s, %d launch failures, locator %v, a pane opened %t; want a fresh agent started at once, uncharged",
					recorded, err, c.State, c.Budgets.LaunchFailures, c.Locator, opened(f.argvs))
			}
		})
	}
}

// A takeover from Kubernetes: the pod recorded its session under the image's home, which on this
// host is a folder another user owns and denies the daemon, and the operator copied the session
// into the pane's own sessions directory. The resume opens the pane on that copy.
func TestATakeoverWhoseImageHomeIsAnotherUsersDeniedFolderResumesTheCarriedSession(t *testing.T) {
	requireOthersFolder(t)
	f := takingOver(t)
	f.stopGrace = time.Hour
	spec := f.spec(t, claim.RoleTester)
	prev := sandboxLocator(t, spec.Project, spec.Issue, claim.RoleTester)
	recorded := strings.Replace(podSession, "/home/legion/", "/root/", 1)
	spec.ResumeSessionFile = recorded
	carried := filepath.Join(paneSessions(f), filepath.Base(filepath.Dir(recorded)), filepath.Base(recorded))
	writeSession(t, carried, time.Now())
	if _, err := f.Resume(context.Background(), &prev, spec); err != nil {
		t.Fatalf("Resume of a session recorded under another user's folder = %v, want the carried copy resumed", err)
	}
	var command string
	for _, argv := range f.argvs {
		if verb(argv) == "new-window" {
			command = argv[len(argv)-1]
		}
	}
	if !strings.Contains(command, "--resume="+carried) {
		t.Errorf("the pane's command = %q, want the carried session %s resumed", command, carried)
	}
}

// A denied folder this daemon's own user owns is still a store fault at every level, since the
// session may be in it: the recorded session's own folder. (Its sessions directory and its folders
// are held by TestSessionFileOfAnswersAPlaceItCannotReadAsAStoreFault.)
func TestARecordedSessionInAFolderOfThisUserThatDeniesItIsAStoreFault(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}
	folder := filepath.Join(t.TempDir(), "sessions", "--legion-workspace--")
	recorded := filepath.Join(folder, "2026-10-20T10-00-00-000Z_ses-abc.jsonl")
	writeSession(t, recorded, time.Now())
	if err := os.Chmod(folder, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(folder, 0o700) })
	if got, _, _, err := sessionFileOf(recorded, paneAt(filepath.Join(t.TempDir(), "pane"))); !errors.Is(err, runtime.ErrSessionStore) {
		t.Fatalf("sessionFileOf of a session in this user's own denied folder = %q, %v, want runtime.ErrSessionStore", got, err)
	}
}
