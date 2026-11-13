//go:build linux

package launcher

import (
	"flag"
	"io"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The list the deleter's cover is built from, held to what this package's cases plant and to what
// the deleter is told: the bait the swap case plants under every path a role's own namespace
// replaces (volumedelete_test.go), and the command line the deleter reads its cover from.

// replacedBait is one path a role's own mount namespace replaces, with what the swap case plants
// under it and what a reader is told when the bait is gone.
type replacedBait struct {
	what  string
	dir   string
	files []string
	// within is the directory under dir that the swap case's link names, where it does not name
	// dir itself: the deleter's /proc is a procfs of its own, whose links (/proc/1/root,
	// /proc/1/cwd) name the deleter's own root, and this kernel follows a link the recursion
	// re-opens (volumedir.go), so a removal let into the procfs would go on through them into all
	// the deleter sees, /legion with it. A directory only the container's tree holds is reached
	// through a cover that shows that tree, and is nothing in a procfs.
	within string
}

// roleReplacedBait is every path a role's own mount namespace replaces, written out with its
// bait. It is written out here rather than taken from the list the code holds
// (RoleCover.replaced): a path dropped from that list is a bait the swap case still plants and
// the deleter no longer covers, which that case goes red by name for, and a path added to that
// list with no bait here is TestTheCoverNamesEveryPathARolesNamespaceReplaces gone red by name.
//
// The image's own paths are under system, which a case points at a tree of its own
// (useRoleSystemRoot), since nothing can plant a file in the machine's /proc or /dev/pts and
// nothing of this package may plant one in the machine's /tmp.
func roleReplacedBait(cover RoleCover, system string) []replacedBait {
	return []replacedBait{
		{"the fixed root's gh mount point, where the role's own bundle is bound", filepath.Join(cover.FixedRoot, RoleGHDir), []string{"hosts.yml"}, ""},
		{"the fixed root's private mount point, where the role's own credentials are bound", filepath.Join(cover.FixedRoot, RolePrivateDir), []string{"BOOT_TOKEN"}, ""},
		{"the fixed root's state mount point", filepath.Join(cover.FixedRoot, RoleStateDir), []string{filepath.Join("omp", "profile.json")}, ""},
		{"the fixed root's agent-secrets mount point", filepath.Join(cover.FixedRoot, RoleAgentSecretsDir), []string{"ANTHROPIC_API_KEY"}, ""},
		{"the home, which is the lower layer of every role's own", cover.Home, []string{filepath.Join(ConfigHomeDir, "jj", "config.toml"), ".bashrc"}, ""},
		{"the container's /tmp, where a role has its own", filepath.Join(system, "tmp"), []string{"notes.md"}, ""},
		{"the container's /var/tmp, where a role has its own", filepath.Join(system, "var", "tmp"), []string{"notes.md"}, ""},
		{"the container's /run/lock", filepath.Join(system, "run", "lock"), []string{"held.lock"}, ""},
		{"the container's /var/lock", filepath.Join(system, "var", "lock"), []string{"held.lock"}, ""},
		{"the container's /dev/shm, where a role has its own", filepath.Join(system, "dev", "shm"), []string{"shared"}, ""},
		{"the container's pseudo-terminals", filepath.Join(system, "dev", "pts"), []string{"0"}, ""},
		{"the container's POSIX message queues", filepath.Join(system, "dev", "mqueue"), []string{"queue"}, ""},
		{"the roles directory, where another role's own credentials are", cover.RolesDir, []string{
			filepath.Join("tester", RolePrivateDir, "notes.md"),
			filepath.Join("tester", RoleStateDir, "omp", "profile.json"),
			filepath.Join("tester", RoleAgentSecretsDir, "ANTHROPIC_API_KEY"),
			filepath.Join("tester", RoleLauncherDir, LauncherTokenFile),
		}, ""},
		{"the home mounts directory, where the pod's shared config home is mounted", cover.HomeMounts, []string{
			filepath.Join(ConfigHomeDir, "jj", "config.toml"),
		}, ""},
		{"the container's /proc, where a role has its own", filepath.Join(system, "proc"), []string{
			filepath.Join("legion-bait", "cmdline"),
		}, "legion-bait"},
	}
}

// swapTarget is the path the swap case's link names for this bait: the covered path itself, or
// the directory under it where the link cannot name the path (within).
func (b replacedBait) swapTarget() string { return filepath.Join(b.dir, b.within) }

// plantReplacedBait plants one bait under every path of that list and answers them by what each
// path is, so a case that reads them back names the cover that let a deletion through.
func plantReplacedBait(t *testing.T, cover RoleCover, system string, at time.Time) map[string][]baitFile {
	t.Helper()
	planted := map[string][]baitFile{}
	for _, under := range roleReplacedBait(cover, system) {
		planted[under.what] = plantBait(t, under.dir, at, under.files...)
	}
	return planted
}

// Every path a role's own mount namespace replaces, written out, against the one list the code
// holds (RoleCover.replaced): the swap case plants a bait under each of these, so a path dropped
// from that list is this case naming the path that left and that case naming the bait that went,
// and a path added to it is this case naming the path that arrived with no bait planted under
// it. The cover is the pod's own, written out as the worker image lays it out, with the
// launcher's own construction of it read back beside it (roleCoverFor).
func TestTheCoverNamesEveryPathARolesNamespaceReplaces(t *testing.T) {
	cover := podCover()
	if built := roleCoverFor(cover.RolesDir); built != cover {
		t.Errorf("the launcher builds the cover %+v for an issue pod, want the layout of the worker image, %+v", built, cover)
	}
	want := roleReplacedBait(cover, "/")
	replaced := cover.replaced()
	if len(replaced) != len(want) {
		t.Fatalf("a role's namespace replaces %d paths and this file plants a bait under %d: %v against %v",
			len(replaced), len(want), pathsReplaced(replaced), baitPaths(want))
	}
	for at, path := range pathsReplaced(replaced) {
		if path != want[at].dir {
			t.Errorf("a role's namespace replaces %s where this file plants its bait under %s: every path replaced is a path the deleter covers and this file baits\n%v\nagainst\n%v",
				path, want[at].dir, pathsReplaced(replaced), baitPaths(want))
		}
	}
}

// podCover is the cover of a pod whose roles container is laid out as the worker image is,
// written out (layout.go).
func podCover() RoleCover {
	return RoleCover{
		RolesDir: "/var/run/legion/roles", Home: "/home/legion",
		HomeMounts: "/var/run/legion/home", FixedRoot: "/var/run/legion",
	}
}

// pathsReplaced is the path of each replacement, in the order the list holds them.
func pathsReplaced(replaced []roleReplacement) []string {
	paths := make([]string, 0, len(replaced))
	for _, one := range replaced {
		paths = append(paths, one.path)
	}
	return paths
}

// baitPaths is the directory each bait of this file is planted under.
func baitPaths(bait []replacedBait) []string {
	paths := make([]string, 0, len(bait))
	for _, one := range bait {
		paths = append(paths, one.dir)
	}
	return paths
}

// Every directory of the cover reaches the deleter on its command line, read back the way
// `legion launcher volume-trim-delete` reads it: through the flags that command registers
// (RoleCoverFlags), which is the one registration role-init makes too. The swap case drives the
// covers end to end through this test binary re-entered, which carries them in its environment
// since a test binary is not `legion` (useContainedDeleter), so this is what holds the argv of
// the real command to the whole of the cover: a directory of RoleCover the trimmer does not pass
// is a directory the pod's child never covers, and the flags read back as a cover other than the
// one the trimmer was given.
func TestTheDeletersCommandLineNamesEveryDirectoryItCovers(t *testing.T) {
	cover := podCover()
	argv, _, err := trimDeleteLaunch(cover)
	if err != nil {
		t.Fatal(err)
	}
	at := slices.Index(argv, VolumeTrimDeleteSubcommand)
	if at < 0 {
		t.Fatalf("the deleter is run as %v, want this binary's own %s subcommand", argv, VolumeTrimDeleteSubcommand)
	}
	flags := flag.NewFlagSet("launcher "+VolumeTrimDeleteSubcommand, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	read := RoleCoverFlags(flags)
	if err := flags.Parse(argv[at+1:]); err != nil {
		t.Fatalf("the deleter is run as %v, which its own flags refuse: %v", argv, err)
	}
	if got := read(); got != cover {
		t.Errorf("the deleter is run as %v, which its own flags read back as %+v, want %+v", argv, got, cover)
	}
}
