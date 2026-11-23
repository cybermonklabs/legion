package tmux

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The daemon's private server reads no tmux configuration of the operator's. tmux holds the client
// that starts a server until the server's configuration has run, so a ~/.tmux.conf that runs a
// command (a plugin manager; here a stand-in that marks that it ran) put that command's time inside
// the first launch's new-session, and its options and commands on the daemon's server.
func TestRealTmuxThePrivateServerReadsNoTmuxConfigOfTheOperators(t *testing.T) {
	r := newRig(t)
	// Spawn's one bound is the test binary's deadline less the 30 s that testwait.Receive leaves for
	// the failure and the cleanups, so a Spawn that never returns ends there and fails the row by name.
	ctx := t.Context()
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-30*time.Second))
		defer cancel()
	}
	home := filepath.Join(r.dir, "operator")
	ran := filepath.Join(r.dir, "operator-config-ran")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	conf := "set -g @legion_test_operator_config read\nrun-shell 'touch " + ran + "'\n"
	if err := os.WriteFile(filepath.Join(home, ".tmux.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	r.environ = append(r.environ, "HOME="+home)
	rt := r.newRuntime()
	if _, err := rt.Spawn(ctx, r.spec("LEGION-8", "LEGION-8", claim.RoleArchitect)); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if got := r.mustTmux("show-options", "-gqv", "@legion_test_operator_config"); got != "" {
		t.Errorf("the private server's @legion_test_operator_config is %q: it read the operator's ~/.tmux.conf", got)
	}
	if _, err := os.Stat(ran); !os.IsNotExist(err) {
		t.Errorf("the operator's ~/.tmux.conf ran a command on the private server (stat %s: %v)", ran, err)
	}
}
