package tmux

// A claim can record the session its agent registered and no file of it (dispatch://INFRA-4108).
// Under tmux its launch resumes the session by its id, from the file the daemon's own host finds
// for it in the pane's sessions directory; a fresh agent names no session, and no pane resolves an
// id on its own.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// resumeSessionID is the id of the session a pod's PI_SESSION_FILE named, captured in the
// planner's pod on 2026-11-19, and resumeFolder the folder Oh My Pi kept it in, named for that
// pod's working directory.
const (
	resumeSessionID = "01a1f5de-4835-7667-b005-e99e3e17edd3"
	resumeFolder    = "--legion-workspaces-cybermonklabs-legion-infra-4108--"
)

// homed is a fake server runtime whose panes' HOME is under its state directory, so the pane's Oh
// My Pi sessions directory (Launcher.SessionsDir) is paneSessions(f).
func homed(t *testing.T) *fakeServerRuntime {
	t.Helper()
	return newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) {
		o.Environ = append(o.Environ, "HOME="+filepath.Join(o.StateDir, "home"))
	})
}

// openedCommand is the command of the pane the fake server opened, "" for none.
func openedCommand(f *fakeServerRuntime) string {
	for _, argv := range f.argvs {
		if verb(argv) == "new-window" {
			return argv[len(argv)-1]
		}
	}
	return ""
}

// A claim that records its session and no file of it is resumed from a folder of the pane's own
// sessions directory (Launcher.SessionsDir), where Oh My Pi keeps the sessions of every working
// directory: the newest regular file `*_<id>.jsonl` one folder down, which the pane's command
// resumes as it resumes a recorded file. A directory of that name, a session whose id shares the
// first 30 characters, a subagent's file and a file two folders down are none of it. With no such
// file the Resume answers runtime.ErrSessionFileMissing, naming the id and the directory, and
// opens no pane.
func TestAResumeOfASessionNamedByItsIDAloneFindsItsFile(t *testing.T) {
	ctx := context.Background()
	written := time.Date(2026, 11, 19, 22, 12, 27, 829_000_000, time.UTC)
	byID := func(t *testing.T, f *fakeServerRuntime) runtime.SpawnSpec {
		t.Helper()
		spec := f.spec(t, claim.RoleTester)
		spec.ResumeSession = resumeSessionID
		return spec
	}
	missing := func(t *testing.T, f *fakeServerRuntime) {
		t.Helper()
		_, err := f.Resume(ctx, nil, byID(t, f))
		if !errors.Is(err, runtime.ErrSessionFileMissing) || !strings.Contains(err.Error(), resumeSessionID) || !strings.Contains(err.Error(), paneSessions(f)) {
			t.Errorf("Resume of session %s = %v, want runtime.ErrSessionFileMissing naming the id and %s", resumeSessionID, err, paneSessions(f))
		}
		if opened(f.argvs) {
			t.Error("a resume of a session found nowhere opened a pane")
		}
	}

	t.Run("the newest of its files one folder down", func(t *testing.T) {
		f := homed(t)
		sessions := paneSessions(f)
		older := filepath.Join(sessions, resumeFolder, "2026-11-19T22-12-27-829Z_"+resumeSessionID+".jsonl")
		newest := filepath.Join(sessions, "--state-workspaces-acme-widgets-legion-43--", "2026-11-19T23-12-27-829Z_"+resumeSessionID+".jsonl")
		writeSession(t, older, written)
		writeSession(t, newest, written.Add(time.Hour))
		// Each written after the newest, and none of them the session's file one folder down.
		later := written.Add(2 * time.Hour)
		writeSession(t, filepath.Join(sessions, resumeFolder, "2026-11-20T00-12-27-829Z_"+resumeSessionID[:30]+"28aa01.jsonl"), later)
		writeSession(t, filepath.Join(strings.TrimSuffix(older, ".jsonl"), "0-scout.jsonl"), later)
		writeSession(t, filepath.Join(sessions, resumeFolder, "nested", "2026-11-20T00-12-27-829Z_"+resumeSessionID+".jsonl"), later)
		directory := filepath.Join(sessions, "--newer--", "2026-11-20T00-12-27-829Z_"+resumeSessionID+".jsonl")
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(directory, later, later); err != nil {
			t.Fatal(err)
		}

		if _, err := f.Resume(ctx, nil, byID(t, f)); err != nil {
			t.Fatalf("Resume of session %s by its id: %v", resumeSessionID, err)
		}
		if command := openedCommand(f); !strings.Contains(command, "--resume="+newest) || strings.Contains(command, "--resume="+resumeSessionID) {
			t.Errorf("the pane's command = %q, want it to resume %s", command, newest)
		}
	})

	t.Run("a file two folders down", func(t *testing.T) {
		f := homed(t)
		writeSession(t, filepath.Join(paneSessions(f), resumeFolder, "nested", "2026-11-19T22-12-27-829Z_"+resumeSessionID+".jsonl"), written)
		missing(t, f)
	})

	t.Run("no file of the session", func(t *testing.T) {
		f := homed(t)
		writeSession(t, filepath.Join(paneSessions(f), resumeFolder, "2026-11-19T22-12-27-829Z_"+resumeSessionID[:30]+"28aa01.jsonl"), written)
		missing(t, f)
	})
}

// A fresh agent names no session: a Spawn naming one by its id, or asking the agent's own command
// to resolve one, is refused as one naming a session file is, and opens no pane.
func TestTmuxSpawnRefusesAResumeSession(t *testing.T) {
	for name, change := range map[string]func(*runtime.SpawnSpec){
		"naming a session by its id":                 func(s *runtime.SpawnSpec) { s.ResumeSession = resumeSessionID },
		"asking the agent to resolve a session's id": func(s *runtime.SpawnSpec) { s.ResumeByAgent = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := homed(t)
			spec := f.spec(t, claim.RoleTester)
			change(&spec)
			if _, err := f.Spawn(context.Background(), spec); err == nil {
				t.Errorf("a Spawn %s was not refused", name)
			}
			if opened(f.argvs) {
				t.Errorf("a Spawn %s opened a pane", name)
			}
		})
	}
}

// The tmux runtime looks a session up by its id on the daemon's own host, so it never answers
// runtime.ErrSessionLookupUnavailable and no pane's agent resolves an id itself: a Resume asking
// for that (ResumeByAgent) is refused, whether it names the session by its id or by its file, and
// opens no pane, though the session's file is there to resume.
func TestTmuxResumeRefusesAResumeByAgent(t *testing.T) {
	for name, change := range map[string]func(spec *runtime.SpawnSpec, file string){
		"naming the session by its id": func(s *runtime.SpawnSpec, _ string) { s.ResumeSession = resumeSessionID },
		"naming the session's file":    func(s *runtime.SpawnSpec, file string) { s.ResumeSessionFile = file },
	} {
		t.Run(name, func(t *testing.T) {
			f := homed(t)
			file := filepath.Join(paneSessions(f), resumeFolder, "2026-11-19T22-12-27-829Z_"+resumeSessionID+".jsonl")
			writeSession(t, file, time.Now())
			spec := f.spec(t, claim.RoleTester)
			spec.ResumeByAgent = true
			change(&spec, file)
			_, err := f.Resume(context.Background(), nil, spec)
			if err == nil || errors.Is(err, runtime.ErrSessionLookupUnavailable) {
				t.Errorf("a Resume %s by the agent's own lookup = %v, want it refused, and not as a lookup the runtime could not make", name, err)
			}
			if opened(f.argvs) {
				t.Errorf("a Resume %s by the agent's own lookup opened a pane", name)
			}
		})
	}
}
