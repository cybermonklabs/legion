//go:build linux

package launcher

import (
	"slices"
	"testing"
)

// role-init carries into a role's namespace the mounts under /tmp, and the other carried
// directories, that a path reaches, and binds back the home's mounts the same way (mountsUnder).
// Each case is a mount table as a kernel writes it, and the mount points a path under /tmp
// reaches in it, worked out by hand, each mount above another first. A table's top mount has a
// parent the table does not list, as Linux writes it and as gVisor does in a runsc container,
// whose root filesystem is mounted over a dummy root mount that mountinfo leaves out; or it is its
// own parent, as gVisor writes a namespace's root mount when the process's root is on that mount.
// gVisor may number the mounts on one parent in any order (cloneMountTree walks a Go map), so a
// table can list a mount after the mount that hides it. The rule reads no order, so a case also
// lists a mount before the mount it sits on.
func TestOnlyTheMountsAPathReachesAreCarried(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		table []tableMount
		want  []string
	}{
		{"a mount a later mount on /tmp hides, on Linux", []tableMount{
			{"27", "1", "/"}, {"383", "27", "/tmp"}, {"400", "383", "/tmp/shadowed"},
			{"401", "383", "/tmp"}, {"402", "401", "/tmp/mounted"}, {"403", "27", "/tmpfs"},
		}, []string{"/tmp/mounted"}},
		{"a mount listed after the mount on /tmp that hides it, under a root that is its own parent", []tableMount{
			{"1", "1", "/"}, {"5", "1", "/tmp"}, {"6", "5", "/tmp"}, {"7", "5", "/tmp/beneath"},
			{"8", "6", "/tmp/mounted"},
		}, []string{"/tmp/mounted"}},
		{"a mount that sits on a hidden mount", []tableMount{
			{"1", "1", "/"}, {"5", "1", "/tmp"}, {"7", "5", "/tmp/hidden"}, {"9", "7", "/tmp/hidden/inner"},
			{"6", "5", "/tmp"},
		}, nil},
		{"a mount that another mount at the same point sits on", []tableMount{
			{"27", "1", "/"}, {"30", "27", "/tmp/v"}, {"31", "30", "/tmp/v"},
		}, []string{"/tmp/v"}},
		{"a mount listed before the mount it sits on", []tableMount{
			{"1", "1", "/"}, {"9", "12", "/tmp/v/x"}, {"12", "1", "/tmp/v"},
		}, []string{"/tmp/v", "/tmp/v/x"}},
		{"two mounts on a parent the table does not list, one over the other", []tableMount{
			{"30", "2", "/tmp/a"}, {"31", "2", "/tmp"},
		}, nil},
		{"a mount beside another whose name begins with its own", []tableMount{
			{"27", "1", "/"}, {"40", "27", "/tmp/a"}, {"41", "27", "/tmp/ab"}, {"42", "40", "/tmp/a/b"},
		}, []string{"/tmp/a", "/tmp/a/b", "/tmp/ab"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reachableUnder(tc.table, "/tmp"); !slices.Equal(got, tc.want) {
				t.Errorf("the mounts carried under /tmp = %q, want %q", got, tc.want)
			}
		})
	}
}
