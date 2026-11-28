//go:build linux

package launcher

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

func init() {
	coverRoleInitTemporaries = ownTemporaries
}

// ownTemporaries is what a role-init the package's role layout starts (useRoleLayout) does before
// role-init's own body: it holds its mounts to itself and covers every directory role-init carries
// mounts under (RoleCover.replaced's carried: /tmp, /var/tmp, the lock directories, /dev/shm) with
// a memory filesystem of its own, as the stage of TestARoleRunsInNamespacesOfItsOwn covers its own
// /tmp and /dev/shm (runStage).
//
// The launcher under test runs in the test process, so role-init's mount namespace starts as a copy
// of the machine's, and role-init carries every mount it finds under those directories into the
// role (carryOver). A machine's /tmp may carry a mount of another process's at any moment, one this
// user cannot even stat (a `runsc do` network namespace's was one), and a role-init that met one
// failed the role's start, and with it a case about something else. An issue pod's container holds
// only the mounts its pod spec names; this is the test's container, which should hold none.
//
// The memory filesystem hides what the machine's directory held, but the mounts under it stay
// listed in this namespace's table, locked where the kernel copied them. Each gets a memory
// filesystem of the case's own at its path, parents first, as runStage's own does, so a role-init
// that carries every listed mount from its path (carryOver, mountsUnder) carries empty ones and
// nothing of the machine's. The mounts are read from mountinfo itself, so the stand-ins do not
// depend on which mounts mountsUnder answers.
func ownTemporaries(cover RoleCover) error {
	if err := privateMounts(); err != nil {
		return fmt.Errorf("hold the test role-init's mounts to itself: %w", err)
	}
	return eachReplaced(cover, func(replaced roleReplacement, path string) error {
		if !replaced.carried {
			return nil
		}
		if err := mountTmpfs(path, "size=16m"); err != nil {
			return fmt.Errorf("cover %s: %w", path, err)
		}
		hidden, err := mountPointsUnder(path)
		if err != nil {
			return err
		}
		for _, point := range hidden {
			if err := os.MkdirAll(point, 0o755); err != nil {
				return fmt.Errorf("stand in for %s: %w", point, err)
			}
			if err := mountTmpfs(point, "size=1m"); err != nil {
				return fmt.Errorf("stand in for %s: %w", point, err)
			}
		}
		return nil
	})
}

// mountPointsUnder is every mount point this namespace's mountinfo lists strictly under dir, read
// whole, sorted so each comes after every point above it, and each once.
func mountPointsUnder(dir string) ([]string, error) {
	table, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read this namespace's mounts: %w", err)
	}
	var points []string
	for _, line := range strings.Split(string(table), "\n") {
		if fields := strings.Fields(line); len(fields) > 4 {
			if point := unescapeMount(fields[4]); strings.HasPrefix(point, dir+"/") {
				points = append(points, point)
			}
		}
	}
	slices.Sort(points)
	return slices.Compact(points), nil
}
