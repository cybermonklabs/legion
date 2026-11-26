package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
)

// A tmux daemon's boot writes the panes' launcher directory (workerbin.Install) for the legion
// overrides.paneLegion names. Where it names none, the boot writes it for its own executable, as
// in production. Every script the directory holds execs that one legion. bin/legion and bin/gh are
// always there, and bin/git and bin/jj where the boot's PATH leads to no image wrapper of that
// name. A boot that left paneLegion unread would write this test binary into each script of the
// named case.
func TestTheTmuxBootWritesThePaneLauncherForTheLegionItIsGiven(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, paneLegion, want string
	}{
		{name: "none named", want: self},
		{name: "one named", paneLegion: "/opt/proof/legion", want: "/opt/proof/legion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			o := fakeRuntime(fake.NewRuntime(), &built{})
			o.paneLegion = tc.paneLegion
			startDaemon(t, cfg, o)
			bin := workerbin.LauncherDir(cfg.StateDir)
			scripts, err := os.ReadDir(bin)
			if err != nil {
				t.Fatalf("read the launcher directory %s: %v", bin, err)
			}
			prefix := "#!/bin/sh\nexec " + shellprefix.Literal(tc.want) + " "
			found := map[string]bool{}
			for _, script := range scripts {
				body, err := os.ReadFile(filepath.Join(bin, script.Name()))
				if err != nil {
					t.Fatal(err)
				}
				found[script.Name()] = true
				if !strings.HasPrefix(string(body), prefix) {
					t.Errorf("bin/%s reads %q, want it to exec %s", script.Name(), body, tc.want)
				}
			}
			for _, name := range []string{"legion", "gh"} {
				if !found[name] {
					t.Errorf("the launcher directory %s holds no %s; it holds %v", bin, name, found)
				}
			}
		})
	}
}
