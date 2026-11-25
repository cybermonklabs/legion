//go:build linux

package repocache

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// reapExited reaps the orphans a process is handed as their subreaper. It runs in a child of the
// test made a subreaper (PR_SET_CHILD_SUBREAPER), so an orphaned grandchild comes to it as one
// comes to a container's PID 1, and no other test's processes are in reach.
//
// The orphan's exit is a fact of the run, not a time: the grandchild writes its pid, runs until
// its parent has exited and the helper writes its release, and the helper calls reapExited once
// the grandchild is a zombie (/proc/<pid>/stat). A loaded machine can hold a grandchild that
// sleeps a fixed span past a fixed wait for it, and reapExited then finds it still running.
func TestReapExitedReapsOrphans(t *testing.T) {
	if os.Getenv("REPOCACHE_REAP_HELPER") == "1" {
		const prSetChildSubreaper = 36
		if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
			fmt.Println("prctl:", errno)
			os.Exit(2)
		}
		dir := os.Getenv("REPOCACHE_REAP_DIR")
		pidFile, release := filepath.Join(dir, "pid"), filepath.Join(dir, "release")
		// The grandchild runs until the release, or until this helper is gone, never for a span
		// of its own. Its paths reach it as arguments.
		orphan := fmt.Sprintf(`echo $$ >"$1.tmp" && mv "$1.tmp" "$1"; while [ ! -e "$2" ] && kill -0 %d 2>/dev/null; do sleep 0.01; done`, os.Getpid())
		// Run returns once the grandchild's parent has exited, which hands the grandchild to
		// this subreaper.
		if err := exec.Command("/bin/sh", "-c", `/bin/sh -c "$1" sh "$2" "$3" & exit 0`, "sh", orphan, pidFile, release).Run(); err != nil {
			fmt.Println("spawn:", err)
			os.Exit(2)
		}
		within := testwait.Bounded(t, time.Minute, time.Second)
		var pid int
		if !testwait.Holds(within, testwait.Poll, func() bool {
			raw, err := os.ReadFile(pidFile)
			if err != nil {
				return false
			}
			pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
			return err == nil && pid > 0
		}) {
			fmt.Println("the orphan never wrote its pid to", pidFile)
			os.Exit(2)
		}
		if err := os.WriteFile(release, nil, 0o600); err != nil {
			fmt.Println("release the orphan:", err)
			os.Exit(2)
		}
		if !testwait.Holds(within, testwait.Poll, func() bool { return zombie(pid) }) {
			fmt.Println("the orphan", pid, "never became a zombie")
			os.Exit(2)
		}
		reaped, err := reapExited()
		fmt.Printf("reaped=%d err=%v\n", reaped, err)
		os.Exit(0)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestReapExitedReapsOrphans$")
	command.Env = append(os.Environ(), "REPOCACHE_REAP_HELPER=1", "REPOCACHE_REAP_DIR="+t.TempDir())
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the helper failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "reaped=1 err=<nil>") {
		t.Fatalf("the helper said %q, want one orphan reaped", out)
	}
}

// zombie is whether the process pid has exited and is not yet reaped: its state in
// /proc/<pid>/stat, the field after the parenthesised command name, is Z.
func zombie(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(stat, ')')
	return end >= 0 && bytes.HasPrefix(stat[end+1:], []byte(" Z"))
}
