//go:build linux

package launcher

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"golang.org/x/sys/unix"
)

// roleUID is the uid a role's agent runs as inside its own namespaces: the worker image's
// unprivileged user, so the files it writes are that user's outside too, and every path the
// image lays out for it reads as it does in a container of its own. It holds no capability
// outside that nested namespace.
const roleUID = 1000

// roleNamespaceAttr clones a generation into the role's own user, mount, PID and IPC namespaces,
// with the launcher's own user mapped to root inside them, so role-init can mount there while
// nothing it does reaches the container or another role. gVisor supports exactly these flags
// (pkg/sentry/kernel/task_clone.go, SupportedCloneFlags).
func roleNamespaceAttr() (*syscall.SysProcAttr, error) {
	return &syscall.SysProcAttr{
		Setpgid:                    true,
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWIPC,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}, nil
}

// roleInit is `legion launcher role-init`'s body: the namespaces' setup, then the shim as this
// process's child until it ends.
func roleInit(stop context.Context, cfg RoleInitConfig) error {
	if os.Getpid() != 1 {
		return errors.New("role-init: not the init of a PID namespace of its own")
	}
	// The kernel delivers a signal to a PID namespace's init only while it handles it. `legion`'s
	// main registers for the launcher's stop (SIGTERM, and SIGINT) before it runs any subcommand,
	// and ends stop on it, so a stop that arrives while the namespace is still being set up
	// reaches the shim the moment it starts, rather than waiting out the launcher's grace for a
	// kill. A stop before main runs misses it, in one of two ways. Before the Go runtime installs
	// its own handlers, in its first instructions, the kernel drops it: role-init goes on to start
	// the shim as though no stop came, and the launcher's SIGKILL at the end of its grace ends the
	// role. From then until main registers, during package initialization, the runtime's handler
	// finds no one registered and ends role-init (runtime/signal_unix.go, dieFromSignal: its
	// raise is ignored by an init, so it exits 128+15), which the launcher reports as that exit
	// rather than as a setup failure. Child exits arrive on a channel of their own; a dropped
	// SIGCHLD costs nothing, since one reap pass takes every exited orphan.
	exits := make(chan os.Signal, 1)
	signal.Notify(exits, syscall.SIGCHLD)
	defer signal.Stop(exits)
	// The shim never inherits the launcher's status pipe: role-init alone reports the role's exit.
	syscall.CloseOnExec(cfg.StatusFD)
	if err := prepareRoleNamespace(cfg); err != nil {
		return fmt.Errorf("role-init: %w", err)
	}
	return runRoleShim(stop, cfg, exits)
}

// prepareRoleNamespace is everything the role's mount namespace holds that the container's does
// not: the role's own layers under the roles directory, and then what the role is given at every
// path its namespace replaces, in the order the one list both this and the deletions' namespace
// are built from names them (RoleCover.replaced, roleinit.go). That order matters: every mount
// whose source is under the roles directory is made before the cover goes over it, and the
// role's own /proc last, so the role sees its own processes alone from the moment it starts.
func prepareRoleNamespace(cfg RoleInitConfig) error {
	role := filepath.Join(cfg.RolesDir, cfg.Role)
	homes := filepath.Join(cfg.RolesDir, RoleHomesDir, cfg.Role)
	stage := filepath.Join(homes, "stage")
	works := filepath.Join(homes, "work")
	work := filepath.Join(works, shimwire.GenerationDir(cfg.Generation))
	if err := privateMounts(); err != nil {
		return fmt.Errorf("hold this role's mounts to itself: %w", err)
	}
	// A generation that failed midway left its stage's empty stand-ins behind, whose mounts went
	// with its namespace, so nothing under the stage is a mount here; every other generation left
	// the stage itself, empty (carryOver). Every removal role-init makes under the role's homes,
	// which the role writes, follows no link (rolefile.RemoveTree).
	if err := rolefile.RemoveTree(stage); err != nil {
		return fmt.Errorf("clear the role's stage: %w", err)
	}
	// The role's own directory of every path the list binds from role-init's own directory under
	// the role-homes directory (roleReplacement.inHomes, RoleCover.replaced), its temporary
	// directories among them, so a path added to that list is given its directory here with no
	// edit of this one.
	dirs := []string{filepath.Join(homes, "upper"), work, stage}
	for _, replaced := range cfg.replaced() {
		if replaced.inHomes {
			dirs = append(dirs, replaced.from(role, homes))
		}
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("make the role's own %s directory: %w", filepath.Base(dir), err)
		}
	}
	// Each generation's overlay works in a directory of its own, so its mount never waits on the
	// previous generation's namespace letting go of one, which the kernel may finish after that
	// generation's init is reaped. The upper layer is the role's across generations, as a role
	// container's own filesystem was. What earlier generations' overlays left is cleared; a
	// leftover only costs disk, so one that cannot be cleared is logged and the role starts.
	entries, err := os.ReadDir(works)
	if err != nil {
		return fmt.Errorf("read the role's overlay work directories: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == filepath.Base(work) {
			continue
		}
		if err := rolefile.RemoveTree(filepath.Join(works, entry.Name())); err != nil {
			roleLogf(cfg, "role-init: clear an earlier generation's overlay work directory: %v", err)
		}
	}
	// The log of the pod's test database, at the fixed path every role is told (testPGEnv): the
	// one file from under the roles directory a role reads, so a role whose connect was refused
	// reads what the server said of it. A pod that serves no test database has no such file, and
	// its role starts with the image's empty mount point there. It is bound before the list's
	// cover goes over the roles directory, and it is not on the list (RoleCover.replaced), which
	// holds directories: the deleter walks the same list, and has nothing to cover at a file.
	//
	// The file is bound, read-only, and never the directory it is in. Under gVisor a read-only
	// bind is not locked: measured in an issue pod on 2026-11-17, a role's own `unshare -Urm`
	// remounted one read-write, which Linux refuses, while the cover over the roles directory
	// held (its unmount answered EINVAL). A bound directory would therefore be writable to a
	// role that tried, and that role could replace the log with a link, which the next `pg_ctl
	// --log` or the script's own append would follow: into postgresql.auto.conf, say, where a
	// line the role made the server log becomes configuration of a server that runs in the
	// launcher's namespace. A role that remounts its view of the bound file read-write can write
	// it, truncate it or chmod it, and can neither replace it with a link or another file nor
	// remove it, since it is a mount point: measured in an issue pod on 2026-11-18, a rename,
	// an unlink and a link over it from such a namespace each answered EBUSY. legion-testpg puts
	// the log's mode back before each start, so a chmod cannot stop a restart, and nothing that
	// reads the cluster's configuration reads the log.
	//
	// The source is opened without following a link and bound through that descriptor, and
	// anything but a regular file is refused: a link planted at the source would otherwise bind
	// its target into every role. Nothing can plant one today, since the directory is behind the
	// locked cover, and the check costs nothing.
	source, target := testPGLog(cfg.RolesDir), filepath.Join(cfg.FixedRoot, RoleTestPGLog)
	switch fd, err := unix.Open(source, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); {
	case errors.Is(err, unix.ENOENT):
	case err != nil:
		return fmt.Errorf("the pod's test database log %s: %w", source, err)
	default:
		err := bindRegularFileReadOnly(fd, target)
		_ = unix.Close(fd)
		if err != nil {
			return fmt.Errorf("bind %s read-only at %s: %w", source, target, err)
		}
	}
	// Everything the role's namespace holds that the container's does not, in the order the one
	// list both namespaces are built from names it (RoleCover.replaced, roleinit.go): the role's
	// own directories at the fixed paths every role is told, so neither the shim nor the agent
	// learns a path of its own and its launcher token is bound nowhere; its own layer over the
	// home; its own temporary directories, on the pod's disk as a container filesystem is; its
	// own small memory filesystems where the image has one for the whole container; its own
	// pseudo-terminals and message queues; the covers; and its own /proc.
	return eachReplaced(cfg.RoleCover, func(replaced roleReplacement, path string) error {
		place := func(mount func() error) error {
			if !replaced.carried {
				return mount()
			}
			return carryOver(cfg, path, stage, mount)
		}
		switch replaced.kind {
		case roleBound:
			source := replaced.from(role, homes)
			if _, err := os.Stat(source); err != nil {
				if replaced.optional && errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("the role's %s directory: %w", filepath.Base(source), err)
			}
			if err := place(func() error { return bindMount(source, path) }); err != nil {
				return fmt.Errorf("bind %s at %s: %w", source, path, err)
			}
		case roleOverlaid:
			return privateHome(cfg, path, homes, work)
		case roleMemory:
			if err := place(func() error { return mountTmpfs(path, "size=64m") }); err != nil {
				return fmt.Errorf("give the role its own %s: %w", path, err)
			}
		case roleTerminals:
			return privateTerminals(path)
		case roleQueues:
			return privateQueues(path)
		case roleHidden:
			// The covers are the last mounts made from under them: every role's directories go
			// behind an empty filesystem, this role's launcher token with them, and so does the
			// container's view of the home's volumes, which the role now sees at their own paths
			// alone.
			if err := mountTmpfs(path, "size=1m,mode=0755"); err != nil {
				return fmt.Errorf("cover %s: %w", path, err)
			}
		case roleProc:
			if err := mountProc(path); err != nil {
				return fmt.Errorf("mount the role's own %s: %w", path, err)
			}
		}
		return nil
	})
}

// privateTerminals gives the role its own pseudo-terminals where the image's /dev has them, at
// the pts directory the one list names (RoleCover.replaced). A new devpts instance there: the
// container's lists, and lets the opener of a sibling's terminal read and write it, every
// terminal of every role, all the same user outside their namespaces; the new one holds only
// those the role opens, with the image's ptmx opening into it (bound over a ptmx that is a node;
// a link to pts/ptmx already resolves there). Linux lets the owner of a user namespace mount
// devpts, and gVisor registers it as mountable there (runsc/boot/vfs.go: devpts AllowUserMount,
// which takes newinstance). ptmxmode 0666 is what container runtimes give /dev/ptmx, so the
// role's unprivileged agent can open it.
func privateTerminals(pts string) error {
	if err := unix.Mount("devpts", pts, "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666"); err != nil {
		return fmt.Errorf("give the role its own %s: %w", pts, err)
	}
	ptmx := systemPath("dev/ptmx")
	info, err := os.Lstat(ptmx)
	switch {
	case errors.Is(err, fs.ErrNotExist), err == nil && info.Mode()&fs.ModeSymlink != 0:
	case err != nil:
		return fmt.Errorf("give the role its own %s: %w", ptmx, err)
	default:
		if err := unix.Mount(filepath.Join(pts, "ptmx"), ptmx, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("give the role its own %s: %w", ptmx, err)
		}
	}
	return nil
}

// privateQueues gives the role its own view of POSIX message queues where the image's /dev has
// them. The mqueue filesystem mounted from inside the role's own IPC namespace shows that
// namespace's queues alone, where the container's mount showed — and let a role create — queues
// of the container's IPC namespace, a channel every sibling could read. Linux lets the owner of a
// user namespace mount it, and gVisor registers mqfs as mountable there (runsc/boot/vfs.go), a
// new IPC namespace getting its own POSIX queue registry (task_clone.go).
func privateQueues(mqueue string) error {
	if err := unix.Mount("mqueue", mqueue, "mqueue", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("give the role its own %s: %w", mqueue, err)
	}
	return nil
}

// mountProc puts a procfs of this namespace's own PID namespace at path, so the processes it
// shows are its own and none of the container's.
func mountProc(path string) error {
	return unix.Mount("proc", path, "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "")
}

// privateHome gives the role a writable layer of its own over the image's home, then puts every
// volume the home carries — the shared config home, an operator's mount — back at its
// own path on top. The container mounts those volumes under the home mounts directory rather than
// under the home itself, so the overlay's lower layer, the home, has no mount under it: Linux
// refuses an overlay whose lower layer has mounts under it that a more privileged namespace locked
// (clone_private_mount), and every mount the container carries is locked in the role's namespaces.
// A home that carries a mount anyway is refused, naming it, rather than overlaid on one kernel and
// hidden on another. What the role writes anywhere else under its home is its own: no sibling role
// and nothing in the container sees it. Each volume's place is made in the role's own layer
// without following a link the role left there (makeLike). The pod's shared config home is the
// one volume not bound back: the role's own config home goes at its path instead (ownConfigHome),
// and a mount under the shared one is bound back on top of the role's.
func privateHome(cfg RoleInitConfig, home, homes, work string) error {
	if _, carried, err := mountsUnder(home); err != nil {
		return err
	} else if len(carried) > 0 {
		return fmt.Errorf("the home %s carries mounts (%s): the container mounts what belongs under the home under %s instead",
			home, strings.Join(carried, ", "), cfg.HomeMounts)
	}
	options := homeOverlayOptions(home, filepath.Join(homes, "upper"), work)
	if err := unix.Mount("overlay", home, "overlay", 0, options); err != nil {
		return fmt.Errorf("give the role its own layer over %s with %s: %w", home, options, err)
	}
	// The image reaches the home mounts directory through a link (its /var/run is /run), and
	// mountinfo names each mount by the path the link resolves to, so each is placed relative to
	// that resolved directory.
	homeMounts, mounts, err := mountsUnder(cfg.HomeMounts)
	if err != nil {
		return err
	}
	// A mount under another is carried by that one's recursive bind, so only the outermost of
	// each nest is bound; mountsUnder lists a mount above another first.
	var bound []string
	for _, mount := range mounts {
		if slices.ContainsFunc(bound, func(outer string) bool { return strings.HasPrefix(mount, outer+"/") }) {
			continue
		}
		rel, err := filepath.Rel(homeMounts, mount)
		if err != nil {
			return err
		}
		target := filepath.Join(home, rel)
		source := mount
		if rel == ConfigHomeDir {
			if source, err = ownConfigHome(mount, filepath.Join(homes, RoleConfigHomeDir), mounts); err != nil {
				return fmt.Errorf("give the role its own config home: %w", err)
			}
		}
		if err := makeLike(cfg, source, home, target); err != nil {
			return fmt.Errorf("make %s's place in the role's home: %w", target, err)
		}
		if err := bindMount(source, target); err != nil {
			return fmt.Errorf("bind %s at %s: %w", source, target, err)
		}
		if source == mount {
			bound = append(bound, mount)
		}
	}
	return nil
}

// homeOverlayOptions is the option string for the role's own layer over its home. userxattr is
// there because gVisor marks a renamed directory opaque by writing the extended attribute
// trusted.overlay.opaque, which a task in a nested user namespace may not write, and gVisor panics
// when that write is refused, which ends the pod's whole sandbox. userxattr puts the mark in the
// user.overlay. namespace instead, which the role's own upper layer keeps and its owner may write.
// gVisor turns the option on by itself only when the upper layer is a gofer mount, and a role's
// upper layer is a sandbox tmpfs.
func homeOverlayOptions(home, upper, work string) string {
	return "lowerdir=" + home + ",upperdir=" + upper + ",workdir=" + work + ",userxattr"
}

// ownConfigHome is the role's own config home, own, made the first time a generation of the role
// starts in the pod as a copy of the pod's shared one, shared, and the role's from then on: what
// workspace-init wrote there (Legion's jj configuration of the issue's clone, which jj keeps in
// the config home) is what the role's jj reads, and what the role writes there stays its own
// across its generations. A git `core.fsmonitor` or a jj alias one role sets in its config home
// would otherwise run in every sibling's git or jj, with the sibling's credentials. The copy
// leaves out every mount under the shared config home (among mounts, mountinfo's list), which
// privateHome binds back over the role's copy, and is made under a name of its own, finished with
// the marker file configHomeMarker, and renamed into place. An own that is not a directory
// holding that marker is no finished copy of role-init's — a generation that stopped midway, or a
// directory something else made at that path — and is removed and made again, so a role never
// starts without workspace-init's jj configuration. The marker is in the role's own config home,
// so a role that removes it has its config home copied afresh at its next generation.
func ownConfigHome(shared, own string, mounts []string) (string, error) {
	switch complete, err := configHomeComplete(own); {
	case err != nil:
		return "", err
	case complete:
		return own, nil
	}
	if err := rolefile.RemoveTree(own); err != nil {
		return "", err
	}
	partial := own + ".partial"
	if err := rolefile.RemoveTree(partial); err != nil {
		return "", err
	}
	err := filepath.WalkDir(shared, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != shared && slices.Contains(mounts, path) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(shared, path)
		if err != nil {
			return err
		}
		target := filepath.Join(partial, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.Mkdir(target, info.Mode().Perm())
		case entry.Type().IsRegular():
			return copyRegular(path, target, info.Mode().Perm())
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return fmt.Errorf("%s is neither a directory, a regular file nor a link", path)
		}
	})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(partial, configHomeMarker), nil, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(partial, own); err != nil {
		return "", err
	}
	return own, nil
}

// configHomeMarker is the file ownConfigHome writes into a role's own config home last, before it
// renames the copy into place.
const configHomeMarker = ".legion-config-home-complete"

// configHomeComplete reports whether own is a finished copy of ownConfigHome's: a directory, not a
// link, holding the regular file configHomeMarker.
func configHomeComplete(own string) (bool, error) {
	info, err := os.Lstat(own)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !info.IsDir():
		return false, nil
	}
	marker, err := os.Lstat(filepath.Join(own, configHomeMarker))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	return marker.Mode().IsRegular(), nil
}

// copyRegular copies the regular file source to target, which it creates with perm.
func copyRegular(source, target string, perm fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// carryOver puts the role's own filesystem at dir (place) while keeping every mount strictly
// under dir that a path reaches — an operator's volume under /tmp, say — visible on top of it:
// the outermost mount of each nest is held aside on the role's stage first and bound back at its
// own path after, since the new filesystem would otherwise hide it and what the role writes there
// would never reach the volume. Its recursive bind brings every mount inside it, so role-init
// resolves no path inside a carried mount: a volume every role may write, where a sibling role
// could put a link in the way of a mount inside it. A mount already hidden beneath another mount
// is not carried (mountsUnder): the role could not reach it in the container either, and its path
// reaches something else, or nothing. What the role writes anywhere else under dir is its own: no
// sibling role and nothing in the container sees it. The home is not carried this way
// (privateHome): an overlay's lower layer may have no mount under it at all.
//
// The directory it holds the mounts aside in is its own, and it removes it once every stand-in in
// it is let go of, so it leaves nothing on the stage whichever entry of the list comes after it:
// the stage is under the roles directory, which a cover goes over (RoleCover.replaced).
func carryOver(cfg RoleInitConfig, dir, stage string, place func() error) error {
	root, mounts, err := mountsUnder(dir)
	if err != nil {
		return err
	}
	// mountsUnder lists a mount above another first.
	carried := mounts[:0]
	for _, mount := range mounts {
		if !slices.ContainsFunc(carried, func(outer string) bool { return strings.HasPrefix(mount, outer+"/") }) {
			carried = append(carried, mount)
		}
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	held, err := os.MkdirTemp(stage, "held-")
	if err != nil {
		return err
	}
	for index, mount := range carried {
		aside := filepath.Join(held, strconv.Itoa(index))
		if err := makeLike(cfg, mount, held, aside); err != nil {
			return fmt.Errorf("hold %s aside: %w", mount, err)
		}
		if err := bindMount(mount, aside); err != nil {
			return fmt.Errorf("hold %s aside: %w", mount, err)
		}
	}
	if err := place(); err != nil {
		return err
	}
	for index, mount := range carried {
		aside := filepath.Join(held, strconv.Itoa(index))
		if err := makeLike(cfg, aside, root, mount); err != nil {
			return fmt.Errorf("put %s back: %w", mount, err)
		}
		if err := bindMount(aside, mount); err != nil {
			return fmt.Errorf("put %s back: %w", mount, err)
		}
		if err := unix.Unmount(aside, unix.MNT_DETACH); err != nil {
			return fmt.Errorf("let go of %s's stand-in: %w", mount, err)
		}
	}
	if err := rolefile.RemoveTree(held); err != nil {
		return fmt.Errorf("clear the stand-ins of %s: %w", dir, err)
	}
	return nil
}

// runRoleShim runs the generation's shim as this init's own child, in a nested user namespace as
// the image's unprivileged user with no capability outside it. role-init stays the role's PID 1:
// it reaps the orphans the role leaves behind, reports the shim's own wait status, and ends the
// namespace with its own exit. A generation ends down one of two paths, and on either of them
// every process of the role is asked to leave before anything is killed.
//
// On the launcher's stop (stop's end, held since role-init began) role-init forwards it to every
// process of the role as SIGTERM, once, and the shim has the launcher's stop grace to end; the
// launcher's SIGKILL of role-init at the end of that grace ends the namespace with it, without
// asking, and is the only other signal the launcher sends.
//
// A shim that exits on its own is the other path, and there is no stop and no grace on it: role-
// init sends that SIGTERM itself once it has reported the shim's status, and waits up to
// roleNamespaceDrain for the role's processes to go before the SIGKILL below ends the namespace.
// Without it a git or a jj the role left running would be killed outright and leave its lock in
// the issue's shared clone, which stops every jj command of every role of the pod.
//
// The shim runs in role-init's own process group, not one of its own. Oh My Pi and every process
// it starts without a group of its own are in the shim's group, and an orphan they leave (a `cmd
// &` whose shell exits) comes to role-init, which reaps it. The cluster's gVisor keeps a count,
// per process group, of the members whose parent is in another group of the session: it takes one
// from that count when such a member is reaped, and adds none when a member is reparented
// (gVisor's pkg/sentry/kernel: the count is `ancestors` in sessions.go, taken from on a reap in
// task_exit.go, and left as it is by reparentLocked there). With the shim in a group of its own,
// each orphan role-init reaped took the count of the shim's group one lower, until it read 0
// while the shim still ran; gVisor then treated the group as orphaned, and a member stopped at
// that moment brought SIGHUP and SIGCONT to the whole group, which ended the shim and Oh My Pi.
// In role-init's group every such orphan is reaped by a parent in its own group, which takes
// nothing from the count, and the group's one member with a parent outside it is role-init
// itself, whose parent is the launcher: the group cannot read as orphaned while role-init lives.
// Nothing here signals the shim's group: role-init, as its namespace's PID 1, signals every
// process of the role with kill(-1) (signalGeneration), and the launcher signals role-init alone
// (signalChild). A process of the role can signal the group itself, and role-init is in it: with
// kill(0), or with the group's number, which is 1 in the role's PID namespace, role-init's own
// pid, so `kill -- -<pgid>` there is kill(-1), which reaches every process of the role but
// role-init. What a signal to the group does to role-init: SIGTERM and SIGINT start its stop, as
// the launcher's stop does, SIGTERM to the whole role and no drain after the shim's exit; SIGCHLD
// asks for a reap; each other signal the Go runtime ends a process on when it is sent
// (roleInitDropped, SIGHUP and SIGQUIT among them) role-init catches and drops, so it ends only
// the processes that take it as theirs, and role-init reports the shim's status and drains the
// role as it does for any shim that exits on its own (`legion worker-shim` takes SIGHUP as its
// stop). It catches them rather than ignoring them: an ignored signal stays ignored across exec,
// in the shim and in Oh My Pi, and a caught one goes back to its default there. Every other
// signal the Go runtime takes no action on. Oh My Pi sends no signal to its own group in its
// normal run (its embedded shell refuses kill 0).
func runRoleShim(stop context.Context, cfg RoleInitConfig, exits <-chan os.Signal) error {
	cmd := exec.Command(cfg.Argv[0], cfg.Argv[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = cfg.Stdout, cfg.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: roleUID, HostID: 0, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: roleUID, HostID: 0, Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	dropped := make(chan os.Signal, len(roleInitDropped))
	signal.Notify(dropped, roleInitDropped...)
	defer signal.Stop(dropped)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("role-init: start the role's shim: %w", err)
	}
	ended := make(chan struct{})
	go func() { _ = cmd.Wait(); close(ended) }()
	stopped := stop.Done()
	stopArrived := false
	for running := true; running; {
		select {
		case <-ended:
			running = false
		case <-exits:
			if _, err := reapNamespace(cmd.Process.Pid); err != nil {
				roleLogf(cfg, "role-init: %v", err)
			}
		case sig := <-dropped:
			roleLogf(cfg, "role-init: dropped a signal sent to the role's process group (%v): it ends only the processes of the role that take it", sig)
		case <-stopped:
			// Every process of the role, a detached one included: this is its PID 1. The stop is
			// forwarded once: the channel goes to nil so this arm, ready for good once the stop's
			// end is closed, stops being chosen, and stopArrived is what the drain below reads.
			stopped, stopArrived = nil, true
			if err := signalGeneration(cmd.Process.Pid, syscall.SIGTERM); err != nil {
				roleLogf(cfg, "role-init: %v", err)
			}
		}
	}
	reportStatus(cfg, cmd.ProcessState)
	// The role's processes are asked to leave before the kill below takes the namespace: a git or
	// a jj the shim left running, killed outright, leaves its lock behind in the issue's shared
	// clone. Where the launcher's stop arrived its SIGTERM is that signal, sent once and already
	// answered through the stop grace, so this path is the one where no stop came, and no role of
	// a container's stop pays this wait twice. A stop that arrives during the wait does not cut
	// it short: every process it waits for has this init's SIGTERM in hand, and ending the wait
	// there would send the SIGKILL below to a git a few milliseconds after it was asked to leave,
	// which is the one thing this wait exists to prevent. It holds the container's stop up by
	// nothing worth having: roleNamespaceDrain is two seconds, inside the shortest stop grace the
	// daemon allows, and the wait ends the moment the namespace is empty.
	if !stopArrived {
		if err := signalGeneration(cmd.Process.Pid, syscall.SIGTERM); err != nil {
			roleLogf(cfg, "role-init: %v", err)
		}
		// Each pass reaps as it counts, so the zombies this init adopts as the role's processes
		// end are not counted as processes that keep the wait running. How the wait ended goes to
		// the role's log: whether the role's processes left on the signal, or the bound came first
		// and the kill below ends what is left, which is where a git or a jj can leave a lock.
		ticks := time.NewTicker(namespaceReapPoll)
		began := time.Now()
		drained := began.Add(roleDrainBound)
		for draining := true; draining; {
			remaining, err := reapNamespace()
			switch {
			case err != nil:
				roleLogf(cfg, "role-init: %v", err)
				draining = false
			case !remaining:
				roleLogf(cfg, "role-init: %s %s after its SIGTERM", roleDrainEmptied, time.Since(began).Round(time.Millisecond))
				draining = false
			case !time.Now().Before(drained):
				roleLogf(cfg, "role-init: %s %s after its SIGTERM, the drain's bound of %s: killing them", roleDrainBounded, time.Since(began).Round(time.Millisecond), roleDrainBound)
				draining = false
			default:
				<-ticks.C
			}
		}
		ticks.Stop()
	}
	// Nothing of the role outlives its report. The kernel ends a PID namespace with its init, and
	// this kill makes that certain before the launcher admits the role's next generation.
	_ = syscall.Kill(-1, syscall.SIGKILL)
	return nil
}

// roleInitDropped are the signals role-init catches and drops while its shim runs (runRoleShim):
// each signal the Go runtime ends a process on when another process sends it, but SIGINT and
// SIGTERM, which are role-init's stop. os/signal delivers SIGSEGV, SIGBUS, SIGFPE, SIGILL and
// SIGTRAP to a channel only when they are sent, so a fault of role-init's own still ends it.
var roleInitDropped = []os.Signal{
	syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGABRT, syscall.SIGILL, syscall.SIGTRAP,
	syscall.SIGSYS, syscall.SIGSTKFLT, syscall.SIGBUS, syscall.SIGFPE, syscall.SIGSEGV,
}

// reportStatus writes the shim's own wait status to the launcher's pipe, so the daemon is told
// the role's exit code and signal rather than its namespace init's. It writes the status as the
// 32 bits a syscall.WaitStatus is, which is the width parseStatus reads back.
func reportStatus(cfg RoleInitConfig, state *os.ProcessState) {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok {
		return
	}
	file := os.NewFile(uintptr(cfg.StatusFD), "role status")
	if file == nil {
		roleLogf(cfg, "role-init: no status pipe at descriptor %d", cfg.StatusFD)
		return
	}
	if _, err := fmt.Fprintf(file, "%d", uint32(status)); err != nil {
		roleLogf(cfg, "role-init: report the shim's status: %v", err)
	}
	_ = file.Close()
}

// roleLogf writes one line of role-init's own to cfg.Stderr, through the URL rule
// (redact.URLCredentials): cfg.Stderr is the stream the role's shim is handed as its own, so the
// rule is applied to this line alone rather than to the stream.
func roleLogf(cfg RoleInitConfig, format string, args ...any) {
	if cfg.Stderr == nil {
		return
	}
	fmt.Fprintln(cfg.Stderr, redact.URLCredentials(fmt.Sprintf(format, args...)))
}

// mountsUnder are the mount points strictly under dir that a path reaches, sorted, so a mount
// above another comes first and is bound back first, and the path dir resolves to, by which
// mountinfo names them: a dir reached through a link (the image's /var/run is a link to /run) is
// listed by its target, so dir as given would match none of its mounts. A mount the table lists
// that no path reaches, one hidden beneath a later mount on /tmp say, is left out
// (reachableMount): every caller acts on a mount by its path, which reaches something else there,
// or nothing. The table's order is not used: a table may list a mount before the mount it sits on.
func mountsUnder(dir string) (string, []string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve %s: %w", dir, err)
	}
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", nil, fmt.Errorf("read this namespace's mounts: %w", err)
	}
	defer file.Close()
	var table []tableMount
	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) < 5 {
			continue
		}
		table = append(table, tableMount{id: fields[0], parent: fields[1], point: unescapeMount(fields[4])})
	}
	if err := lines.Err(); err != nil {
		return "", nil, fmt.Errorf("read this namespace's mounts: %w", err)
	}
	return resolved, reachableUnder(table, resolved), nil
}

// reachableUnder are the mount points in table strictly under dir that a path reaches
// (reachableMount), sorted: of two that a path reaches, one under the other sits in that one's
// tree, so sorting puts each mount after every mount it sits in.
func reachableUnder(table []tableMount, dir string) []string {
	byID := make(map[string]tableMount, len(table))
	for _, mount := range table {
		byID[mount.id] = mount
	}
	var under []string
	for _, mount := range table {
		if strings.HasPrefix(mount.point, dir+string(filepath.Separator)) && reachableMount(table, byID, mount) {
			under = append(under, mount.point)
		}
	}
	slices.Sort(under)
	return under
}

// tableMount is one line of mountinfo: the mount's id, the id of the mount it sits on, and its
// mount point.
type tableMount struct {
	id, parent, point string
}

// reachableMount reports whether a path reaches mount, decided as a lookup goes: at a mount point
// it goes into the mount there, and a mount over another sits on that one's root, so the one it
// covers is its parent. A mount is hidden when another mount sits on its root, when another mount
// on the same parent sits at its mount point or at a directory above it, or when the mount it sits
// on is hidden. The chain of parents decides it and the table's order does not: a table may list a
// mount after the mount that hides it, as one made beneath a mount through a handle on the covered
// directory is, and as gVisor may list two mounts on one parent in a namespace it copies, which it
// numbers in the order it walks a Go map (cloneMountTree, pkg/sentry/vfs/mount.go).
//
// The chain ends at the top of the table, which has one of two shapes. In the first, the top
// mount's parent is a mount the table does not list. Linux writes the table this way, and so does
// gVisor in a runsc container, an issue pod's included: runsc mounts the container's root
// filesystem over a dummy root mount of the namespace (createMountNamespace, runsc/boot/vfs.go),
// and mountinfo lists only the mounts from the process's root down (GenerateProcMountInfo,
// pkg/sentry/vfs/mount.go), which leaves the dummy root out. In the second, the top mount is its
// own parent: gVisor writes this for a namespace's root mount, which has no parent, and lists that
// mount only when the process's root is on it.
func reachableMount(table []tableMount, byID map[string]tableMount, mount tableMount) bool {
	for _, other := range table {
		if other.parent == mount.id && other.id != mount.id && other.point == mount.point {
			return false
		}
	}
	// Each step goes one mount up a tree, so a chain longer than the table is a loop, which no
	// kernel lists.
	for range len(table) {
		if mount.parent == mount.id {
			return true
		}
		for _, other := range table {
			if other.id != mount.id && other.id != mount.parent && other.parent == mount.parent && pathAtOrAbove(other.point, mount.point) {
				return false
			}
		}
		parent, listed := byID[mount.parent]
		if !listed {
			return true
		}
		mount = parent
	}
	return false
}

// pathAtOrAbove reports whether the absolute path above is path or a directory path is under.
func pathAtOrAbove(above, path string) bool {
	return above == path || strings.HasPrefix(path, strings.TrimSuffix(above, "/")+"/")
}

// unescapeMount reads mountinfo's octal escapes for the characters it cannot write plainly.
func unescapeMount(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for index := 0; index < len(field); index++ {
		if field[index] == '\\' && index+3 < len(field) {
			if value, err := strconv.ParseUint(field[index+1:index+4], 8, 8); err == nil {
				out.WriteByte(byte(value))
				index += 3
				continue
			}
		}
		out.WriteByte(field[index])
	}
	return out.String()
}

// makeLike makes target, a path under root, a mount point of source's kind — a directory for a
// directory, an empty file for anything else — so a bind of source has somewhere to land, and
// follows no link below root on the way (mountPointBeneath). A file already there is the mount
// point itself, and is left as it is: it is never opened for writing.
func makeLike(cfg RoleInitConfig, source, root, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	return mountPointBeneath(root, target, info.IsDir(), func(link string) {
		roleLogf(cfg, "role-init: %s was a link, which role-init does not follow on the way to a mount point: replaced it with the mount point's place", link)
	})
}

// mountPointBeneath makes target, a path under root, a mount point — a directory when dir is
// set, an empty file otherwise — following no link at or below root's entries. root is a directory
// role-init named itself (the home, a temporary directory, the stage); what lies below it is one
// of the role's own layers, which an earlier generation of the role wrote, so a link there could
// name any path the container shows, another role's directory under the roles directory among
// them, before the covers go on, and a walk that followed it would make directories there: inside
// a config home that already carries its marker, what the walk makes there is what a sibling then
// keeps as its own. A link in the way is the role's own entry, and is removed and replaced by the
// directory or file the mount needs, with replaced told its path; any other entry in the way that
// is not a directory is refused. Each component is decided from a handle the walk opened and read:
// the kind comes from fstat of the handle, never from a flag of the open and never by comparing
// two readings of the name. Nothing of the role runs while role-init does, and no other role
// reaches the role's layers, so the path the walk made is the path the bind that follows lands on.
func mountPointBeneath(root, target string, dir bool, replaced func(link string)) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s is not under %s", target, root)
	}
	if rel == "." {
		if !dir {
			return fmt.Errorf("%s is a directory, where a file's mount point goes", target)
		}
		return nil
	}
	parent, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", root, err)
	}
	defer func() { _ = unix.Close(parent) }()
	names := strings.Split(rel, string(filepath.Separator))
	path := root
	// read opens the component named name and reads the handle it opened. The open is O_PATH,
	// so it opens the entry itself whatever that entry is, a symlink included, and then
	// the kind comes from fstat of the handle, not from a flag of the open. It reads parent
	// and path at each call, which the loop below moves to the component it descends from.
	read := func(name string) (int, unix.Stat_t, error) {
		var stat unix.Stat_t
		fd, err := unix.Openat(parent, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, stat, fmt.Errorf("open %s: %w", path, err)
		}
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			return -1, stat, fmt.Errorf("read %s: %w", path, err)
		}
		return fd, stat, nil
	}
	for index, name := range names {
		path = filepath.Join(path, name)
		file := !dir && index == len(names)-1
		entry, stat, err := read(name)
		if err == nil && stat.Mode&unix.S_IFMT == unix.S_IFLNK {
			_ = unix.Close(entry)
			if err := unix.Unlinkat(parent, name, 0); err != nil {
				return fmt.Errorf("remove the link %s: %w", path, err)
			}
			replaced(path)
			entry, err = -1, unix.ENOENT
		}
		switch {
		case errors.Is(err, unix.ENOENT) && file:
			fd, err := unix.Openat(parent, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
			if err != nil {
				return fmt.Errorf("make %s: %w", path, err)
			}
			return unix.Close(fd)
		case errors.Is(err, unix.ENOENT):
			// An entry that appeared under the walk answers EEXIST here; the read below
			// decides it, whatever it is, and there is no second attempt.
			if err := unix.Mkdirat(parent, name, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("make %s: %w", path, err)
			}
			entry, stat, err = read(name)
			if err != nil {
				return err
			}
			if stat.Mode&unix.S_IFMT == unix.S_IFLNK {
				_ = unix.Close(entry)
				return fmt.Errorf("%s became a link under the walk, where a mount point's directory goes", path)
			}
		case err != nil:
			return err
		case file:
			_ = unix.Close(entry)
			return nil
		}
		// Whether the walk found this component or made it, it descends only from a directory.
		// No link reaches here: the one the first read saw was removed above, and one that appeared
		// at the name after Mkdirat was refused where the second read decided it.
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(entry)
			return fmt.Errorf("%s is neither a directory nor a link, where a mount point's directory goes (kind %#o)", path, stat.Mode&unix.S_IFMT)
		}
		_ = unix.Close(parent)
		parent = entry
	}
	return nil
}

// privateMounts holds every mount this namespace makes or drops to itself, so nothing it does
// propagates back to the container or to another role.
func privateMounts() error {
	return unix.Mount("none", "/", "", unix.MS_REC|unix.MS_PRIVATE, "")
}

// containDeletions is the cache trimmer's deleter put where it sees no more than a role sees:
// this process's mounts held to itself, then an empty filesystem over every path a role's own
// namespace replaces, a procfs of its own PID namespace where a role is given one, and its
// working directory at the root of its own namespace. /legion is left as the container mounted
// it, since the deletions are made there.
//
// It is built from the one list a role's namespace is built from (RoleCover.replaced, which says
// what is in it and what is not) under the same clone flags the role generations run under
// (roleNamespaceAttr), made for a process that only deletes. The removal it makes follows no link
// (removeEntryAt, volumedir_linux.go); this is the bound behind that one: a link a removal did
// follow would reach, at every path of that list, an empty filesystem of this process's own
// (volumedelete.go, which says what that bounds and what it does not).
//
// Where a role is given something of its own — its temporary directories, its memory
// filesystems, its devices, a writable layer over the home — this process is given an empty
// filesystem instead: a role reads what the image lays out there and writes under it, while this
// process reads nothing anywhere, so the whole of its reach at such a path is what it would put
// there itself. The one path it needs rather than hides is its own /proc, through which it opens
// the descriptors the listing passed it (openPassedDir, volumedelete.go).
//
// The working directory is the launcher's until this chdir: a child inherits it, and an inherited
// directory is a handle the cover over its path does not take away, which /proc/self/cwd names.
// At the root of the namespace it names what a link to `/` names, which no cover of a process
// that must reach /legion could take away either.
//
// A path that cannot be covered is a refusal rather than a deletion made uncovered: the deleter
// holds descriptors on the pod's cache directories, and the whole of what this is for is that
// what a removal followed out of them would reach only the role's own.
func containDeletions(cover RoleCover) error {
	if err := privateMounts(); err != nil {
		return fmt.Errorf("hold the deletions' mounts to themselves: %w", err)
	}
	if err := eachReplaced(cover, func(replaced roleReplacement, path string) error {
		if replaced.kind == roleProc {
			if err := mountProc(path); err != nil {
				return fmt.Errorf("mount the deletions' own %s: %w", path, err)
			}
			return nil
		}
		if err := mountTmpfs(path, "size=1m,mode=0755"); err != nil {
			return fmt.Errorf("cover %s: %w", path, err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("stand the deletions at the root of their own namespace: %w", err)
	}
	return nil
}

func mountTmpfs(path, options string) error {
	return unix.Mount("tmpfs", path, "tmpfs", 0, options)
}

func bindMount(source, target string) error {
	return unix.Mount(source, target, "", unix.MS_BIND|unix.MS_REC, "")
}

// bindReadOnly binds source at target and makes that one mount read-only, which takes a second
// call: the kernel ignores every flag but MS_REC on the call that makes a bind, so a bind is born
// with its source mount's flags. The remount names, beside MS_RDONLY, each flag the new mount
// already carries, read from statfs. Linux locks the nosuid, nodev, noexec and access-time flags
// of every mount a user namespace was given by a more privileged one, which in a role's is every
// mount of the container; a bind of such a mount inherits the locks, and a remount that would
// drop or change a locked flag is refused with EPERM.
func bindReadOnly(source, target string) error {
	if err := bindMount(source, target); err != nil {
		return err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(target, &stat); err != nil {
		return fmt.Errorf("read the mount's flags: %w", err)
	}
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY)
	for _, carried := range []struct{ held, remount uintptr }{
		{unix.ST_NOSUID, unix.MS_NOSUID},
		{unix.ST_NODEV, unix.MS_NODEV},
		{unix.ST_NOEXEC, unix.MS_NOEXEC},
		{unix.ST_NOATIME, unix.MS_NOATIME},
		{unix.ST_NODIRATIME, unix.MS_NODIRATIME},
		{unix.ST_RELATIME, unix.MS_RELATIME},
	} {
		if uintptr(stat.Flags)&carried.held != 0 {
			flags |= carried.remount
		}
	}
	if err := unix.Mount("", target, "", flags, ""); err != nil {
		return fmt.Errorf("make the mount read-only: %w", err)
	}
	return nil
}

// bindRegularFileReadOnly binds the file fd was opened on read-only at target, through the
// descriptor's own /proc/self/fd entry, so the mount is of that file and never of whatever its
// name names by the time the mount runs. fd is an O_PATH|O_NOFOLLOW descriptor, which on Linux
// opens a link itself rather than failing, so a link is refused here, with anything else that is
// no regular file.
func bindRegularFileReadOnly(fd int, target string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("read what the file is: %w", err)
	}
	if kind := stat.Mode & unix.S_IFMT; kind != unix.S_IFREG {
		return fmt.Errorf("it is no regular file (kind %#o)", kind)
	}
	return bindReadOnly("/proc/self/fd/"+strconv.Itoa(fd), target)
}
