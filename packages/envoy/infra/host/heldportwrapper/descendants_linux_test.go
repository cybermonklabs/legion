package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// prSetChildSubreaper is prctl's PR_SET_CHILD_SUBREAPER (linux/prctl.h).
const prSetChildSubreaper = 36

// becomeSubreaper makes this test process the subreaper of everything it starts: a process whose
// parent exits is handed to it, not to the machine's init, so a process the wrapper left behind is
// still one of this test's descendants and requireTheTestsAlone finds it by walking them.
func becomeSubreaper() {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		panic(fmt.Sprintf("prctl(PR_SET_CHILD_SUBREAPER): %v", errno))
	}
}

// descendantsOf returns pid's live descendants, from each of its threads' children lists
// (/proc/<pid>/task/<tid>/children) to the bottom. A process or a thread that ended since its
// listing has none to list. A kernel without those lists (CONFIG_PROC_CHILDREN off) is an error,
// never an empty answer, since a walk that cannot read them would find nothing and pass.
func descendantsOf(pid int) ([]int, error) {
	var found []int
	seen := map[int]bool{pid: true}
	pending := []int{pid}
	for len(pending) > 0 {
		parent := pending[0]
		pending = pending[1:]
		tasks, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", parent))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, task := range tasks {
			list, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", parent, task.Name()))
			if errors.Is(err, fs.ErrNotExist) {
				if _, gone := os.Stat(fmt.Sprintf("/proc/%d/task/%s", parent, task.Name())); errors.Is(gone, fs.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("this kernel lists no children of a process (/proc/<pid>/task/<tid>/children): %w", err)
			}
			if err != nil {
				return nil, err
			}
			for _, field := range strings.Fields(string(list)) {
				child, err := strconv.Atoi(field)
				if err != nil {
					return nil, fmt.Errorf("read /proc/%d/task/%s/children: %q is not a pid", parent, task.Name(), field)
				}
				if !seen[child] {
					seen[child] = true
					found = append(found, child)
					pending = append(pending, child)
				}
			}
		}
	}
	return found, nil
}
