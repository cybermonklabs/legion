package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// A Claude Code resume whose session files the pod's own disk cannot take is no store fault: the
// table answered, and the start is refused plainly, naming the restore, a refusal the daemon
// charges. The disk refuses through a link where the transcript should be, which the restore never
// follows, or a projects folder that cannot be written.
func TestAClaudeResumeWhoseFilesThePodsDiskCannotTakeIsNoStoreFault(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a directory whatever its mode")
	}
	for name, fault := range map[string]func(t *testing.T, c *claudeRig){
		"the transcript on the pod's disk is a link": func(t *testing.T, c *claudeRig) {
			if err := os.MkdirAll(filepath.Dir(c.transcript), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere.jsonl"), c.transcript); err != nil {
				t.Fatal(err)
			}
		},
		"the transcript's folder cannot be made": func(t *testing.T, c *claudeRig) {
			projects := filepath.Join(c.configDir, "projects")
			_ = os.RemoveAll(projects)
			if err := os.MkdirAll(projects, 0o500); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(projects, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(projects, 0o700) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, dsnFile := testdb.DSNFile(t, "legion_launcher_claude_local_fault_test")
			c := claudeRigOn(t, newSessionRig(t, Config{}), dsnFile)
			shortenResumeLookup(t)
			c.seed(c.transcript, "{\"n\":1}\n", time.Now())
			fault(t, c)
			got := c.start(c.command(1, "exec sleep 600", true))
			if got.OK {
				c.stop(1)
				t.Fatalf("a Claude Code resume whose files the pod's disk cannot take started: %#v", got)
			}
			if got.Session == shimwire.SessionStoreUnreachable || !strings.Contains(got.Error, "restore "+c.transcript) {
				t.Fatalf("a Claude Code resume whose files the pod's disk cannot take = %#v, want a plain refusal naming the restore of %s, not %q",
					got, c.transcript, shimwire.SessionStoreUnreachable)
			}
		})
	}
}
