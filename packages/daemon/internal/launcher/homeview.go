package launcher

import (
	"os"
	"path/filepath"
	"strings"
)

// homeViewPath is where the pod launcher, which runs outside every role's namespace, finds what a
// role sees at path. An issue pod's roles container mounts every volume that belongs at or under
// home under mounts at the same relative path (HomeMountsDir, which the launcher reaches), and
// only role-init binds each back at its own path inside a role. So a path at or under home whose
// mounts/<rest> exists, a link included (Lstat), is mounts/<rest>; any other path under home is the
// image's own file at that path, and a path outside home is the same in both views.
func homeViewPath(home, mounts, path string) string {
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	if view := filepath.Join(mounts, rel); lstatExists(view) {
		return view
	}
	return path
}

// lstatExists is whether an entry, a dangling link included, is at path.
func lstatExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// readHomeView reads a file where the launcher's view of the container holds it (homeViewPath),
// for texts.BaseIgnoredSkills: a missing file answers the error os.ReadFile does, so a caller's
// test for an absent file holds.
func readHomeView(home, mounts string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		return os.ReadFile(homeViewPath(home, mounts, path))
	}
}
