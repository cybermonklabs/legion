package ghroutetest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// HelperRunning says whether pid is still a row's helper: a process whose command line is `sleep
// <seconds>`, read from /proc so a pid the kernel has since given to another process does not
// count. ghroute's help-read timeout row and `legion gh`'s signal rows hold their work with such a
// helper.
func HelperRunning(pid int, seconds string) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	return strings.Join(strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00"), " ") == "sleep "+seconds
}

// RecordedHelper is the pid a stand-in recorded in pidFile, and whether that pid is still the
// stand-in's `sleep <seconds>` (HelperRunning). A file not written yet is no helper.
func RecordedHelper(pidFile, seconds string) (pid int, running bool) {
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid, err == nil && HelperRunning(pid, seconds)
}

// HoldsStandIn is the end of a stand-in script that holds its work: it records its pid in pidFile,
// written whole by a rename, and becomes `sleep <seconds>` under that pid, with a command line
// HelperRunning tells from a pid the kernel has reused. Each row gives its stand-in a sleep of its
// own, of about a day: longer than any test binary's deadline, so that the rows' waits, which the
// deadline alone bounds, never see the stand-in end by itself and read that as the stop. Each
// path is one literal shell word (shellprefix.Literal), whatever the temporary directory holds.
func HoldsStandIn(pidFile, seconds string) string {
	return "echo $$ >" + shellprefix.Literal(pidFile+".new") + "\nmv " + shellprefix.Literal(pidFile+".new") + " " + shellprefix.Literal(pidFile) + "\nexec sleep " + seconds + "\n"
}

// KillRecordedHelper registers a cleanup that kills the helper pidFile names if it is still
// running when the test ends, so a failure leaves nothing behind. It reads the file only then, so
// it is registered before whatever starts the helper, and holds whichever check fails first.
func KillRecordedHelper(t *testing.T, pidFile, seconds string) {
	t.Helper()
	t.Cleanup(func() {
		if pid, running := RecordedHelper(pidFile, seconds); running {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}
