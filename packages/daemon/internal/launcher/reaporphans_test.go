package launcher

import (
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Only the controller pod's single-role form tells its shim to reap orphans: there the shim is the
// launcher's own child, with no role-init above it, and the launcher, the container's PID 1, is in
// another process group than the shim's (reapingShim). The roles-dir form, an issue pod's and the
// controller's under writer_audience, starts role-init, which keeps its shim in role-init's own
// group and reaps the role's orphans there (runRoleShim), so its shim is never told: a shim that
// reaped there would make the role's orphans direct children of the shim, which the session peer
// check and the memory guard read as the shim's own (sessionPeer, podClassifier). An argv that
// runs no shim, as a test's stand-in generation does, is started as it is.
func TestOnlyTheSingleRoleShimIsToldToReapOrphans(t *testing.T) {
	t.Parallel()
	shim := []string{"/usr/local/bin/legion", "worker-shim", "--connect", "unix:///run/legion/stream", "--boot-token-file", "/run/boot", "--", "/opt/omp/bin/omp", "--mode", "rpc"}
	start := shimwire.LauncherStart{ID: "start-1", Generation: 1, Argv: slices.Clone(shim)}

	t.Run("the single-role form", func(t *testing.T) {
		process, err := (&manager{role: "controller"}).command(start, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := slices.Concat(shim[:2], []string{"--reap-orphans"}, shim[2:])
		if !slices.Equal(process.cmd.Args, want) {
			t.Errorf("the shim's command is %q, want %q", process.cmd.Args, want)
		}
		if !slices.Equal(start.Argv, shim) {
			t.Errorf("the start's own argv became %q: the flag went into the daemon's command as well", start.Argv)
		}
	})

	t.Run("a single-role generation that runs no shim", func(t *testing.T) {
		other := []string{"/bin/sh", "-c", "exit 0"}
		process, err := (&manager{role: "controller"}).command(shimwire.LauncherStart{ID: "start-2", Generation: 2, Argv: other}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(process.cmd.Args, other) {
			t.Errorf("the generation's command is %q, want %q as it was", process.cmd.Args, other)
		}
	})

	t.Run("the roles-dir form", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("role-init runs on Linux alone")
		}
		m := &manager{role: "tester", namespaced: true, log: &roleLog{}, cfg: Config{RolesDir: t.TempDir()}}
		process, err := m.command(start, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = process.status.Close()
			_ = process.write.Close()
		})
		// The control: what this reads is role-init's command, which carries the shim's argv
		// whole at its end, so a flag added to it would be read here.
		if args := process.cmd.Args; len(args) < len(shim) || !slices.Equal(args[len(args)-len(shim):], shim) {
			t.Fatalf("role-init's command %q does not end with the shim's argv %q", args, shim)
		}
		for _, word := range slices.Concat(process.cmd.Args, process.cmd.Env) {
			if strings.Contains(word, "--reap-orphans") {
				t.Errorf("role-init's command or environment holds %q: a shim under role-init must never reap orphans", word)
			}
		}
	})
}
