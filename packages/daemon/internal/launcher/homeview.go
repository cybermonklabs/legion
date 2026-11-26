package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// homeView is where the pod launcher, which runs outside every role's namespace, reads what a role
// sees under its home, home. An issue pod's roles container mounts every volume that belongs at or
// under home under mounts instead, at the same relative path (HomeMountsDir), and inside each role
// role-init lays the role's own layer over the image's home and binds back at its own path under
// it each mount it finds under mounts (privateHome, over mountsUnder), the outermost of each nest
// with the mounts under it. bound are those mount points, each by its path relative to mounts.
type homeView struct {
	home, mounts string
	bound        []string
}

// homeMountsUnder is the reading of what role-init binds back from a home mounts directory: the
// path the directory resolves to, and the mount points under it, named under that path. It is
// mountsUnder, the reading role-init itself makes, on Linux (homeview_linux.go), and no mount on
// any other platform, where no role-init runs (homeview_other.go). A test puts a table of its own
// here, as it points roleHome and roleHomeMounts.
var homeMountsUnder = boundHomeMounts

// launcherHomeView is the home view of a start now: the home and the home mounts directory as
// role-init is given them (roleHome and roleHomeMounts, Home and HomeMountsDir in a pod), and the
// mounts under the latter as this namespace's mount table lists them (homeMountsUnder), the table
// each role's namespace is made from. A home mounts directory that is not there holds no mount,
// so every path is the same in both views: the controller pod's single-role container, which runs
// no role-init, mounts what belongs under the home at its own path and nothing there.
func launcherHomeView() (homeView, error) {
	view := homeView{home: roleHome, mounts: roleHomeMounts}
	if _, err := os.Stat(roleHomeMounts); errors.Is(err, fs.ErrNotExist) {
		return view, nil
	}
	resolved, mounts, err := homeMountsUnder(roleHomeMounts)
	if err != nil {
		return homeView{}, fmt.Errorf("the mounts role-init binds back under %s: %w", roleHome, err)
	}
	for _, mount := range mounts {
		rel, err := filepath.Rel(resolved, mount)
		if err != nil {
			return homeView{}, fmt.Errorf("the mount %s under %s: %w", mount, resolved, err)
		}
		view.bound = append(view.bound, rel)
	}
	return view, nil
}

// path is where the launcher reads what a role sees at path. A path outside home is the same in
// both views. A path at or under home that a bound mount covers, one at the path or above it, is
// mounts/<rest>: the role sees that mount there, or the outermost mount above it with this one
// inside, so what the mount lacks is absent in both views, whatever the image holds at path. Any
// other path under home is the image's own file at path, which the role reads through its layer
// over the image's home; what a role wrote into that layer the launcher does not see. The pod's
// shared config home (ConfigHomeDir) is the one bound mount whose place role-init gives each role
// something else, its own copy of the shared one (ownConfigHome), so a path under it that no
// deeper mount covers is read from the shared config home that copy was made from.
func (v homeView) path(path string) string {
	rel, err := filepath.Rel(filepath.Clean(v.home), filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	for _, mount := range v.bound {
		if rel == mount || strings.HasPrefix(rel, mount+string(filepath.Separator)) {
			return filepath.Join(v.mounts, rel)
		}
	}
	return path
}

// read reads the settings file at path where the launcher's view holds it (path), for
// texts.BaseIgnoredSkills, with texts.ReadSettingsFile: a file the view does not hold, one a
// bound mount lacks among them, answers an error matching fs.ErrNotExist, which BaseIgnoredSkills
// reads as absent.
func (v homeView) read(path string) ([]byte, error) {
	return texts.ReadSettingsFile(v.path(path))
}
