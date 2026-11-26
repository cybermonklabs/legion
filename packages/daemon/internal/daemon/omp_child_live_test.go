//go:build e2e

// The one read of a pane's process tree that two live proofs make: which Oh My Pi a pane's
// `legion worker-shim` started. role_prompt_rule_live_test.go reads that process's command line,
// and texts_folder_live_test.go its environment. It is a file of its own, and a free function rather
// than a method of either proof, because scripts/e2e/texts-folder.sh's negative control copies it
// with its driver into a tree of main, whose role_prompt_rule_live_test.go has only the method.
package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// ompChildOf is the pid of the Oh My Pi the shim at pid shim started: the live process whose
// PPid is that shim and whose argv[0] is omp, the binary LEGION_OMP_PATH names. /proc is walked
// rather than /proc/<pid>/task/<tid>/children read, because that file lists only the children
// one thread of the process forked, and the Go shim forks from whichever of its threads runs
// exec.Command: measured in a gVisor pod, the shim's main thread listed no child while another
// of its threads listed its Oh My Pi. A process that ends between the two reads is passed over,
// as a walk of /proc must do. A shim with no such child ends the test naming token, the children
// the shim has, and paneLogs, where that pane's own output is.
func ompChildOf(t *testing.T, omp, paneLogs string, token claim.Token, shim int) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc to find the Oh My Pi %s's shim (pid %d) started: %v", token, shim, err)
	}
	var ofTheShim []string
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == shim {
			continue
		}
		status, err := os.ReadFile("/proc/" + entry.Name() + "/status")
		if err != nil || parentOf(string(status)) != shim {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		argv := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		if len(argv) == 0 {
			continue
		}
		if argv[0] == omp {
			return pid
		}
		ofTheShim = append(ofTheShim, fmt.Sprintf("%d %s", pid, argv[0]))
	}
	t.Fatalf("the shim of %s (pid %d) has no live child whose argv[0] is %s, the Oh My Pi this run launches; its children are %v, and that pane's own output is under %s",
		token, shim, omp, ofTheShim, paneLogs)
	return 0
}
