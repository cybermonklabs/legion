package launcher

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// roleHome is the home directory a role's overlay covers, roleHomeMounts where the container
// mounts what belongs under it, and roleFixedRoot the root a role's own directories are bound back
// under: the worker image's (Home, HomeMountsDir, FixedRoot). roleSystemRoot is where the image's
// own paths a role is given its own of — its temporary directories, its lock directories, its
// devices and its /proc — are rooted, which in a pod is the root of the filesystem.
//
// A test points each of them at a directory of its own, since a machine running the tests has
// none of the image's, and since a case that reads back what a namespace covers must plant a
// file under every path that namespace replaces, which it can do in a tree of its own and not
// in the machine's /proc, /dev/pts or /tmp.
var (
	roleHome       = Home
	roleHomeMounts = HomeMountsDir
	roleFixedRoot  = FixedRoot
	roleSystemRoot = "/"
)

// systemPath is one of the image's own paths, under the root a test points at a tree of its own.
func systemPath(path string) string { return filepath.Join(roleSystemRoot, path) }

// RoleCover is the directories of an issue pod's roles container that a role's own namespace and
// the cache trimmer's deleter are each built over: the roles directory, which holds every role's
// launcher token and its own gh, private, state and agent-secrets directories; the home mounts
// directory, where the container mounts the pod's shared config home and an operator's mount
// (layout.go); the home, whose files are the lower layer each role's own home overlays; and the
// fixed root, under which the role's own directories are bound back at the paths every role is
// told.
//
// It is one value rather than a field of each caller's own: the launcher builds it once
// (roleCoverFor), role-init carries it (RoleInitConfig), every sweep's deleter is given it
// (trimDeleteLaunch, VolumeTrimDelete), one table registers its flags on both command lines and
// writes them back out (roleCoverFlags), and both namespaces are built from the paths it names
// (RoleCover.replaced). What the one table guarantees is that both command lines write and read
// the same directories. A directory added to it is still several edits: its field here, its row
// of roleCoverFlags, its value in roleCoverFor and in the probe's cover (CheckRoleNamespaces),
// and its row of RoleCover.replaced.
type RoleCover struct {
	RolesDir   string `json:"rolesDir"`
	Home       string `json:"home"`
	HomeMounts string `json:"homeMounts"`
	FixedRoot  string `json:"fixedRoot"`
}

// roleCoverFlags is the one table that carries a cover on a command line: the flag each directory
// is named by, what an error calls it, the help the flag prints, the default the worker image
// lays out, and where the directory sits in the value. `legion launcher role-init` and `legion
// launcher volume-trim-delete` register their cover from it (RoleCoverFlags) and the command line
// of each is written from it (RoleCover.flags), so a directory one is told and the other is not
// is a directory neither command can be given.
var roleCoverFlags = []struct {
	flag, what, usage string
	def               func() string
	at                func(*RoleCover) *string
}{
	{
		flag: "roles-dir", what: "roles directory",
		usage: "the container-level per-role root: each role's launcher token, gh bundle and private, state and agent-secrets directories under <dir>/<role>",
		def:   func() string { return "" },
		at:    func(c *RoleCover) *string { return &c.RolesDir },
	},
	{
		flag: "home", what: "home",
		usage: "the home directory a role's own layer covers, and which the deleter covers before it removes anything",
		def:   func() string { return roleHome },
		at:    func(c *RoleCover) *string { return &c.Home },
	},
	{
		flag: "home-mounts", what: "home mounts directory",
		usage: "where the container mounts every volume that belongs under the home, at the same relative path",
		def:   func() string { return roleHomeMounts },
		at:    func(c *RoleCover) *string { return &c.HomeMounts },
	},
	{
		flag: "fixed-root", what: "fixed root",
		usage: "the root the role's own directories are bound back under",
		def:   func() string { return roleFixedRoot },
		at:    func(c *RoleCover) *string { return &c.FixedRoot },
	},
}

// RoleCoverFlags registers the cover's flags on flags and answers what they were set to: the one
// registration both hidden subcommands make, so neither can be given a directory the other is not
// (cmd/legion/launcher.go).
func RoleCoverFlags(flags *flag.FlagSet) func() RoleCover {
	var cover RoleCover
	for _, named := range roleCoverFlags {
		flags.StringVar(named.at(&cover), named.flag, named.def(), named.usage)
	}
	return func() RoleCover { return cover }
}

// flags is the cover written back out as the flags it was read from, which role-init's command
// line and the deleter's each carry (roleInitLaunch, trimDeleteLaunch).
func (c RoleCover) flags() []string {
	argv := make([]string, 0, 2*len(roleCoverFlags))
	for _, named := range roleCoverFlags {
		argv = append(argv, "--"+named.flag, *named.at(&c))
	}
	return argv
}

// validate refuses a cover a pod's layout never gives, naming the command it reached: a relative
// directory, which neither namespace could mount over, and a home mounts directory at or under
// the home, which would leave the overlay's lower layer carrying a mount.
func (c RoleCover) validate(command string) error {
	for _, named := range roleCoverFlags {
		if path := *named.at(&c); !filepath.IsAbs(path) {
			return fmt.Errorf("%s: %s %q is not absolute", command, named.what, path)
		}
	}
	if overlaps(c.HomeMounts, c.Home) {
		return fmt.Errorf("%s: home mounts %q is not a directory apart from the home %q", command, c.HomeMounts, c.Home)
	}
	return nil
}

// roleCoverFor is the cover of a pod whose roles container is laid out as the worker image is:
// the launcher's own --roles-dir, with the home, the home mounts directory and the fixed root as
// role-init is given them (roleHome, roleHomeMounts, roleFixedRoot), which a test points at a
// layout of its own (useRoleLayout). It is the one construction of a pod's cover: the launcher
// builds role-init's with it and the trimmer builds every sweep's deleter's with it.
func roleCoverFor(rolesDir string) RoleCover {
	return RoleCover{RolesDir: rolesDir, Home: roleHome, HomeMounts: roleHomeMounts, FixedRoot: roleFixedRoot}
}

// roleReplacementKind is what a role's own mount namespace puts at one of the paths it replaces.
// The deleter puts an empty filesystem at every one of them instead, except the one proc, which
// it mounts as a role's is (containDeletions).
type roleReplacementKind int

const (
	// roleBound is bound from a directory of the role's own: its gh, private, state and
	// agent-secrets directories under the roles directory, and its own temporary directories
	// under the role-homes directory.
	roleBound roleReplacementKind = iota
	// roleOverlaid is the role's writable layer over the image's home (privateHome).
	roleOverlaid
	// roleMemory is a small memory filesystem of the role's own where the image has one for the
	// whole container: the lock directories and POSIX shared memory.
	roleMemory
	// roleTerminals is the role's own pseudo-terminals and roleQueues its own view of POSIX
	// message queues (privateTerminals, privateQueues).
	roleTerminals
	roleQueues
	// roleProc is a procfs of the namespace's own PID namespace, which shows its own processes
	// and nothing of the container's.
	roleProc
	// roleHidden is a directory the role is given nothing of: an empty filesystem over the
	// container's own, which is what the deleter puts at every path of this list.
	roleHidden
)

// roleReplacement is one path of an issue pod's roles container that a role's own mount namespace
// replaces with something of the role's.
type roleReplacement struct {
	path string
	kind roleReplacementKind
	// from is where a roleBound is bound from: the role's own directory under the roles
	// directory, or its own directory under the role-homes directory.
	from func(role, homes string) string
	// inHomes is a roleBound whose source is role-init's own directory under the role-homes
	// directory, which role-init makes before it binds it (prepareRoleNamespace). Every other
	// source is under the roles directory, where the pod mounts it.
	inHomes bool
	// carried holds every mount strictly under path — an operator's volume under /tmp, say —
	// aside and binds it back on top of what the role is given there (carryOver).
	carried bool
	// optional is a path the image need not have: the role is given nothing there and the
	// deleter has nothing to cover. A path the image has that neither can replace is a refusal.
	optional bool
}

// replaced is every path of an issue pod's roles container that a role's own mount namespace
// replaces with something of the role's, in the order role-init makes them: every bind whose
// source is under the roles directory before the cover goes over it, and the namespace's own
// /proc last. prepareRoleNamespace walks this list to build a role's namespace and
// containDeletions walks it to build the deleter's, so neither holds a list of its own. A
// temporary(...) path added here is given to a role and covered for the deleter in one edit:
// role-init makes its source under the role-homes directory. A fixed(...) path also needs the
// volume the pod mounts at its source under each role's directory (runtime/sandbox/podkind.go,
// launcherContainer), since role-init refuses a required source it cannot find; the probe makes
// that source from this list (CheckRoleNamespaces).
//
// Every path in it is a directory. The one name a role's namespace replaces that is not —
// /dev/ptmx, which opens into the role's own pseudo-terminals — is left to the kind that replaces
// it (privateTerminals): what the deleter's cover is against is a link a removal follows, and
// both the deleter's removal and Root.RemoveAll, which the containment cases race in its place,
// unlink a name that is not a directory as the name it is rather than recursing through it
// (volumedelete.go).
func (c RoleCover) replaced() []roleReplacement {
	fixed := func(name string, optional bool) roleReplacement {
		return roleReplacement{
			path: filepath.Join(c.FixedRoot, name), kind: roleBound, optional: optional,
			from: func(role, _ string) string { return filepath.Join(role, name) },
		}
	}
	temporary := func(path, own string) roleReplacement {
		return roleReplacement{
			path: systemPath(path), kind: roleBound, carried: true, inHomes: true,
			from: func(_, homes string) string { return filepath.Join(homes, own) },
		}
	}
	memory := func(path string) roleReplacement {
		return roleReplacement{path: systemPath(path), kind: roleMemory, carried: true, optional: true}
	}
	return []roleReplacement{
		fixed(RoleGHDir, false),
		fixed(RolePrivateDir, false),
		fixed(RoleStateDir, false),
		// The agent-secrets directory is there only when the pod is enrolled with the secrets
		// broker.
		fixed(RoleAgentSecretsDir, true),
		{path: c.Home, kind: roleOverlaid},
		temporary("tmp", "tmp"),
		temporary("var/tmp", "var-tmp"),
		// The image's /var/lock is a link to its /run/lock, which the walk resolves and replaces
		// once (eachReplaced).
		memory("run/lock"),
		memory("var/lock"),
		memory("dev/shm"),
		{path: systemPath("dev/pts"), kind: roleTerminals, optional: true},
		{path: systemPath("dev/mqueue"), kind: roleQueues, optional: true},
		{path: c.RolesDir, kind: roleHidden},
		{path: c.HomeMounts, kind: roleHidden},
		{path: systemPath("proc"), kind: roleProc},
	}
}

// eachReplaced walks the paths a namespace of cover's replaces, in the order replaced names them,
// and calls replace with each replacement and the path the kernel reaches for it. A path that is
// another's link is resolved and replaced once, and a path the image does not have is passed over
// where the replacement is optional. Anything else the walk cannot read is a refusal, as is a
// required path the image does not have: a namespace that went on with one of these left as the
// container holds it is what both callers exist to prevent.
func eachReplaced(cover RoleCover, replace func(roleReplacement, string) error) error {
	replaced := map[string]bool{}
	for _, candidate := range cover.replaced() {
		path, err := filepath.EvalSymlinks(candidate.path)
		switch {
		case errors.Is(err, fs.ErrNotExist) && candidate.optional:
			continue
		case err != nil:
			return fmt.Errorf("the path %s a namespace of its own replaces: %w", candidate.path, err)
		case replaced[path]:
			continue
		}
		replaced[path] = true
		if err := replace(candidate, path); err != nil {
			return err
		}
	}
	return nil
}

// requiredUnder is every path of the list (replaced) under root that the image must have: each
// directory a layout rooted at root holds for a namespace of the cover's to be built over it, since
// the walk refuses a required path it cannot find (eachReplaced). The probe lays its own out with
// it (CheckRoleNamespaces), and so does a test that builds a layout of its own.
func (c RoleCover) requiredUnder(root string) []string {
	var paths []string
	for _, replaced := range c.replaced() {
		if !replaced.optional && strings.HasPrefix(replaced.path, root+string(filepath.Separator)) {
			paths = append(paths, replaced.path)
		}
	}
	return paths
}

// RoleInitConfig is `legion launcher role-init`'s resolved flags: the cover its namespace is built
// over, the role whose namespaces this process is the init of and the generation it runs, the pipe
// it writes the shim's own wait status to, and the shim argv it runs as its child.
type RoleInitConfig struct {
	RoleCover
	Role       string   `json:"role"`
	Generation uint64   `json:"generation"`
	StatusFD   int      `json:"statusFd"`
	Argv       []string `json:"argv"`
	// Stdout and Stderr are the launcher's own, which the shim inherits, so one container's log
	// carries every role's output behind its own prefix.
	Stdout io.Writer `json:"-"`
	Stderr io.Writer `json:"-"`
}

// roleStatusFD is the descriptor role-init writes the shim's wait status to: the launcher's one
// extra file, so the first descriptor after the standard three.
const roleStatusFD = 3

// RoleInitSetupFailedExit is the exit status `legion launcher role-init` uses for one thing alone:
// it failed before it started the shim, setting up the role's namespaces or starting the shim
// itself. Every other end of role-init is told apart from it: once the shim started, role-init
// reports the shim's own wait status and exits 0, a role-init that panics exits 2, and one that
// Go's runtime ends for a signal no one registered for exits 128 plus the signal (roleInit).
// sysexits' EX_OSERR, an operating-system error, which is what such a failure is.
const RoleInitSetupFailedExit = 71

func (c RoleInitConfig) validate() error {
	if err := c.RoleCover.validate("role-init"); err != nil {
		return err
	}
	switch {
	case c.Role == "":
		return errors.New("role-init: no role")
	case c.Generation == 0:
		return errors.New("role-init: no generation")
	case c.StatusFD < roleStatusFD:
		return fmt.Errorf("role-init: status descriptor %d is not one the launcher passes", c.StatusFD)
	case len(c.Argv) == 0:
		return errors.New("role-init: no shim argv")
	}
	return nil
}

// RoleInit is `legion launcher role-init`: the init of one role generation's user, mount, PID and
// IPC namespaces, which the launcher clones into them and nothing else ever runs. It hides every
// other role's credentials and its own launcher token, binds the role's own directories back at
// the fixed paths, gives the role a private home overlay, its own config home, private temporary
// directories and its own pseudo-terminals and message queues, starts the shim as its child in a
// nested user namespace as the image's unprivileged user, forwards the launcher's stop to it,
// reaps the orphans the role leaves, and reports the shim's own wait status. Every failure it can
// hit is this role's exit and never the launcher's. stop ends at the launcher's stop: the context
// `legion`'s main ends on SIGTERM or SIGINT, the signals it registers for in its first statement.
// Every error it returns is a failure before the shim started, which `legion launcher role-init`
// exits RoleInitSetupFailedExit for.
func RoleInit(stop context.Context, cfg RoleInitConfig) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	return roleInit(stop, cfg)
}

// CheckRoleNamespaces runs one role generation as an issue pod's launcher runs every role — its
// role-init cloned into a user, mount, PID and IPC namespace of its own, which sets the namespaces
// up and starts its shim in a nested user namespace as the image's unprivileged user — over a
// layout of its own under dir, with shim as the shim's argv, and refuses unless the shim ran and
// exited 0. `legion probe-image --role-namespaces` runs it in the daemon's probe pod, placed and
// secured as every issue pod is, with dir on a disk volume as the role homes volume is and its
// own `legion version` as the shim, so a runtime or node that refuses a role's namespaces or one
// of the mounts role-init makes in them fails the daemon's boot rather than every role's start.
// The layout is the cover role-init's flags name — the roles directory, the home, the home mounts
// directory and the fixed root — with every path under it that a role's namespace must replace
// (RoleCover.replaced) and the one role's gh, private and state directories, and nothing else:
// the home is an empty directory on dir's volume and the home mounts directory carries no mount.
// So the check makes an overlay over a home on that volume, the role's binds, its own /tmp and
// /var/tmp, its memory filesystems, devpts and mqueue, the covers and its /proc; it does not make
// the overlay over the image's own home, whose lower layer is on the container's root filesystem,
// bind a home's volume back, or copy a config home (ownConfigHome). Those first run at a role's
// start in an issue pod.
func CheckRoleNamespaces(dir string, shim ...string) error {
	if len(shim) == 0 {
		return errors.New("role namespaces: no shim argv")
	}
	const role = "probe"
	cfg := RoleInitConfig{
		RoleCover: RoleCover{
			RolesDir: filepath.Join(dir, "roles"), Home: filepath.Join(dir, "home"),
			HomeMounts: filepath.Join(dir, "home-mounts"), FixedRoot: filepath.Join(dir, "fixed"),
		},
		Role: role, Generation: 1, Argv: shim,
	}
	// Every path of the probe's own layout a role's namespace replaces, which the image lays out
	// for a role and dir holds for the probe, and the source of every required bind of that list
	// under the roles directory, which the pod mounts for a role: a path or a bind added to that
	// list is made here too. Role-init makes the sources under the role-homes directory itself
	// (inHomes). The image's own paths, which lie outside dir, are the machine's.
	dirs := cfg.requiredUnder(dir)
	own, homes := filepath.Join(cfg.RolesDir, role), filepath.Join(cfg.RolesDir, RoleHomesDir, role)
	for _, replaced := range cfg.replaced() {
		if replaced.kind == roleBound && !replaced.optional && !replaced.inHomes {
			dirs = append(dirs, replaced.from(own, homes))
		}
	}
	for _, path := range dirs {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("role namespaces: make the probe's layout: %w", err)
		}
	}
	var output bytes.Buffer
	process, err := newRoleProcess(cfg, os.Environ(), &output, &output)
	if err != nil {
		return fmt.Errorf("role namespaces: %w", err)
	}
	if err := process.start(); err != nil {
		return fmt.Errorf("role namespaces: start role-init in namespaces of its own: %w", err)
	}
	raw, _ := io.ReadAll(process.status)
	_ = process.status.Close()
	waitErr := process.cmd.Wait()
	if status := parseStatus(raw); waitErr != nil || status == nil || status.ExitStatus() != 0 || status.Signaled() {
		shim := "none, it never ran or never ended"
		if status != nil {
			shim = fmt.Sprintf("%+v", exitOfStatus(*status, cfg.Generation))
		}
		return fmt.Errorf("role namespaces: a role generation over a probe layout under %s did not run its shim to a clean exit (role-init: %v; the shim's exit: %s): %s",
			dir, waitErr, shim, strings.TrimSpace(output.String()))
	}
	return nil
}

// roleInitLaunch is the command line and the extra environment that run one generation's
// role-init: this binary's own hidden subcommand. A test replaces it with its re-entrant helper.
var roleInitLaunch = func(cfg RoleInitConfig) (argv, env []string, err error) {
	self, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("find this binary: %w", err)
	}
	argv = append([]string{self, "launcher", RoleInitSubcommand}, cfg.flags()...)
	argv = append(argv,
		"--role", cfg.Role, "--generation", strconv.FormatUint(cfg.Generation, 10),
		"--status-fd", strconv.Itoa(cfg.StatusFD), "--",
	)
	return append(argv, cfg.Argv...), nil, nil
}

// overlaps reports whether one of two clean absolute paths is the other or lies under it.
func overlaps(a, b string) bool {
	under := func(child, parent string) bool { return parent == "/" || strings.HasPrefix(child, parent+"/") }
	return a == b || under(a, b) || under(b, a)
}

// RoleInitSubcommand is `legion launcher`'s hidden subcommand, the one the launcher runs itself.
const RoleInitSubcommand = "role-init"

// roleProcess is one generation's role-init: the command that is the init of the role's new
// namespaces, the pipe it writes the shim's own wait status to, and, in an issue pod's container,
// the prefixed writers of the generation's own output, which its wait flushes.
type roleProcess struct {
	cmd    *exec.Cmd
	status *os.File
	write  *os.File
	output []io.Writer
}

// newRoleProcess builds a generation's role-init over cfg, with env the generation's environment
// and stdout and stderr the role's own prefixed writers.
func newRoleProcess(cfg RoleInitConfig, env []string, stdout, stderr io.Writer) (*roleProcess, error) {
	attr, err := roleNamespaceAttr()
	if err != nil {
		return nil, err
	}
	cfg.StatusFD = roleStatusFD
	argv, extra, err := roleInitLaunch(cfg)
	if err != nil {
		return nil, err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("role %s status pipe: %w", cfg.Role, err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = mergeEnv(env, extra)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.ExtraFiles = []*os.File{write}
	cmd.SysProcAttr = attr
	return &roleProcess{cmd: cmd, status: read, write: write}, nil
}

// start runs the generation and hands the status pipe's write end to it alone, so the read end
// ends as soon as role-init is gone.
func (p *roleProcess) start() error {
	err := p.cmd.Start()
	_ = p.write.Close()
	if err != nil {
		_ = p.status.Close()
	}
	return err
}

// parseStatus reads the wait status role-init wrote to its status pipe: the shim's own, so the
// daemon is told the role's exit and not its namespace init's. It is nil when role-init wrote
// none — it was killed before the shim ended, or never got that far.
//
// The pipe carries a wait status, which is 32 bits wide (syscall.WaitStatus), and reportStatus
// writes exactly that: a line naming anything else is a line this protocol never produced, so it
// is refused here rather than truncated into a status the role never had.
func parseStatus(raw []byte) *syscall.WaitStatus {
	value, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 32)
	if err != nil {
		return nil
	}
	status := syscall.WaitStatus(value)
	return &status
}

// exitOfStatus is the exit a wait status reports.
func exitOfStatus(status syscall.WaitStatus, generation uint64) shimwire.LauncherExit {
	exit := shimwire.LauncherExit{Generation: generation, Code: status.ExitStatus()}
	if status.Signaled() {
		exit.Signal = status.Signal().String()
	}
	return exit
}
