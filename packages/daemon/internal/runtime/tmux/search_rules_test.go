package tmux

// Four rules of the search for a carried session (sessionFileOf, newestSession) that the
// store-fault tests do not reach: an entry of a sessions directory that is no folder holds no
// session, a candidate that is not there by the time it is looked at holds none, on equal times the
// file of the recorded name is taken, and a recorded path under a file where a folder should be is
// refused by name.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Oh My Pi keeps a regular file beside the folders of its sessions directory
// (`.session-index.tsv`). The search lists every entry of a sessions directory and opens each as a
// folder, so such a file must read as no folder and hold no session, never as a store fault.
func TestSessionFileOfPassesOverAFileBesideTheFoldersOfASessionsDirectory(t *testing.T) {
	const name = "2026-10-20T10-00-00-000Z_ses-abc.jsonl"
	sessions, pane := filepath.Join(t.TempDir(), "sessions"), filepath.Join(t.TempDir(), "pane-sessions")
	for _, dir := range []string{sessions, pane} {
		writeSession(t, filepath.Join(dir, "--other-work--", "2026-10-19T09-00-00-000Z_ses-other.jsonl"), time.Now())
		if err := os.WriteFile(filepath.Join(dir, ".session-index.tsv"), []byte("index\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a session found nowhere reads missing", func(t *testing.T) {
		got, searched, _, err := sessionFileOf(filepath.Join(sessions, "--legion-workspace--", name), paneAt(pane))
		if err != nil || got != "" || len(searched) != 2 {
			t.Fatalf("sessionFileOf beside a file of the sessions directory = %q, %v, %v; want nothing found in both directories, no error", got, searched, err)
		}
	})
	t.Run("a carried session is still found", func(t *testing.T) {
		carried := filepath.Join(pane, "--pod-work--", name)
		writeSession(t, carried, time.Now())
		got, _, _, err := sessionFileOf(filepath.Join(sessions, "--legion-workspace--", name), paneAt(pane))
		if err != nil || got != carried {
			t.Fatalf("sessionFileOf beside a file of the sessions directory = %q, %v; want %s", got, err, carried)
		}
	})
}

// "On equal times a file of the recorded name is taken before one of the session id alone."
func TestSessionFileOfTakesTheRecordedNameOnEqualTimes(t *testing.T) {
	const name = "2026-10-20T10-00-00-000Z_ses-abc.jsonl"
	sessions := filepath.Join(t.TempDir(), "sessions")
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	named := filepath.Join(sessions, "--pod-work--", name)
	other := filepath.Join(sessions, "--pod-work--", "2026-10-21T09-00-00-000Z_ses-abc.jsonl")
	writeSession(t, named, at)
	writeSession(t, other, at)
	got, _, _, err := sessionFileOf(filepath.Join(sessions, "--legion-workspace--", name), paneAt(filepath.Join(t.TempDir(), "pane")))
	if err != nil || got != named {
		t.Fatalf("sessionFileOf over two sessions of equal time = %q, %v; want the one of the recorded name, %s", got, err, named)
	}
}

// A candidate that is gone by the time it is looked at (a link to nothing here, or a file removed
// between the listing and the look) holds no session: the search goes on, and finds the session
// elsewhere or nowhere, never a store fault.
func TestSessionFileOfPassesOverACandidateThatIsNotThere(t *testing.T) {
	const name = "2026-10-20T10-00-00-000Z_ses-abc.jsonl"
	sessions, pane := filepath.Join(t.TempDir(), "sessions"), filepath.Join(t.TempDir(), "pane")
	dangling := filepath.Join(sessions, "--pod-work--", name)
	if err := os.MkdirAll(filepath.Dir(dangling), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone.jsonl"), dangling); err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(sessions, "--legion-workspace--", name)
	if got, _, _, err := sessionFileOf(recorded, paneAt(pane)); err != nil || got != "" {
		t.Fatalf("sessionFileOf beside a link to nothing = %q, %v; want nothing found, no error", got, err)
	}
	carried := filepath.Join(pane, "--pod-work--", name)
	writeSession(t, carried, time.Now())
	if got, _, _, err := sessionFileOf(recorded, paneAt(pane)); err != nil || got != carried {
		t.Fatalf("sessionFileOf beside a link to nothing = %q, %v; want the carried %s", got, err, carried)
	}
}

// A recorded path one of whose folders is a regular file names no folder there, which no wait
// mends: the search refuses it in words naming the path, as the start's own failure the machine
// charges, and never as a store fault it would wait out for ever. The control is the same name as
// an entry of a sessions directory the search lists, which it passes over.
func TestSessionFileOfRefusesARecordedPathUnderAFileByName(t *testing.T) {
	const name = "2026-10-20T10-00-00-000Z_ses-abc.jsonl"
	sessions := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "--a-file--"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorded := filepath.Join(sessions, "--a-file--", name)
	got, _, _, err := sessionFileOf(recorded, paneAt(filepath.Join(t.TempDir(), "pane-sessions")))
	if got != "" || err == nil || errors.Is(err, runtime.ErrSessionStore) || !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), recorded) {
		t.Fatalf("sessionFileOf of a recorded path under a file = %q, %v; want a refusal naming %s that is no runtime.ErrSessionStore", got, err, recorded)
	}
	other := filepath.Join(t.TempDir(), "other", "--gone--", name)
	if got, _, _, err := sessionFileOf(other, paneAt(sessions)); err != nil || got != "" {
		t.Fatalf("control: sessionFileOf with that file beside the pane's folders = %q, %v; want nothing found, no error", got, err)
	}
}
