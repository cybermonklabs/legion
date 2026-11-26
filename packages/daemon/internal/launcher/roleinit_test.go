//go:build linux

package launcher

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
	"golang.org/x/sys/unix"
)

// The three re-entrant helpers this test runs as processes of its own: the stage, which builds a
// fake container layout inside a mount namespace of its own and starts role-init in it; role-init
// itself, which the stage starts as the init of the role's new namespaces; and the agent, the
// stand-in worker shim role-init runs inside them, which reports what it can reach.
const (
	roleInitStageEnv = "LEGION_ROLE_INIT_TEST_STAGE"
	roleInitAgentEnv = "LEGION_ROLE_INIT_TEST_AGENT"
)

// Each helper of this file is the test binary re-executed with its own LEGION_ marker, which
// Guard would take out, so TestMain runs none in such a child (helperChildVariables,
// main_test.go). This file builds only under Linux, where the namespaces it works in exist, so it
// registers its markers itself rather than leaving them in that list.
func init() {
	helperChildVariables = append(helperChildVariables, roleInitStageEnv, roleInitAgentEnv, uncoverControlEnv, roleCheckEnv, roleDrainEnv)
}

// stagePlan is what the stage builds and runs. Mount is the directory it covers with a memory
// filesystem of its own, so the layout it builds — and the role's own mounts over it — stay out
// of the machine's filesystem and out of the test process's view. Deadline is when a wait of the
// stage's, or of its agents', gives up: the test's beforeDeadline, which ends before the stage's
// own -test.timeout (reentry) and its agents', so a wait spent to it still leaves the stage its
// time to print what it read.
type stagePlan struct {
	Mount    string
	Role     string
	Sibling  string
	Owner    string
	Deadline time.Time
}

// agentPlan is what the agent reports on: the paths its role should and should not reach, the
// process the stage started outside its namespaces, and the detached, SIGTERM-ignoring process it
// leaves behind for the stage to look for once the namespaces are gone, by Detached, the token
// that process carries in its command line (detachedToken). Label is what its lines carry, so the
// stage's two generations read apart. TestPGLog says the stage made the server's log before this
// generation, so role-init bound it at the fixed path. Deadline is the stage's own (stagePlan).
type agentPlan struct {
	Home       string
	HomeMounts string
	RolesDir   string
	FixedRoot  string
	Role       string
	Sibling    string
	Owner      string
	Label      string
	Generation uint64
	OutsidePID int
	Detached   string
	TestPGLog  bool
	Deadline   time.Time
}

// TestRoleNamespaceProbeHelper is the probe itself: cloned into a role's namespaces, it does the
// first thing role-init does there. A machine may allow the namespaces and refuse the mounts.
func TestRoleNamespaceProbeHelper(t *testing.T) {
	// Not parallel: a helper child, run in a process of its own; here it returns at once.
	dir := os.Getenv(roleProbeEnv)
	if dir == "" {
		return
	}
	if err := privateMounts(); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
	if err := mountTmpfs(dir, "size=1m"); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
	exitHelper(t)
}

// Every start of a role runs in namespaces of its own. The role reads its own gh bundle and its
// generation's credentials at the fixed paths every role is told; a sibling role's directory and
// every launcher token, its own included, are gone; it reads the log of the pod's test database
// at its fixed path, the one file from under the roles directory it sees, and can neither write
// it nor, from a further namespace of its own, replace or remove it, and the launcher's file
// stays a regular file; it sees and can signal only its own processes; what it writes under its
// home and in its temporary directories lands on its own layer, invisible to the container; every
// mount its home carried is still there; a mount the container's /tmp hides is not carried into
// the role's, and the role sees nothing of the container's /tmp at its path; its config home is
// its own, a copy of the pod's shared one that no sibling reads; and when the role's init exits
// nothing of the role is left, not even a detached process that ignores SIGTERM.
func TestARoleRunsInNamespacesOfItsOwn(t *testing.T) {
	t.Parallel()
	requireRoleNamespaces(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	plan := stagePlan{Mount: stageMountPoint(t), Role: "a", Sibling: "b", Owner: "acme", Deadline: time.Now().Add(beforeDeadline(t))}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	stage := exec.Command(self, reentry(t, "TestRoleInitStageHelper")...)
	stage.Env = append(os.Environ(), roleInitStageEnv+"="+string(encoded))
	// The stage's own IPC namespace, as a container's, so the message queue it makes to stand for
	// the container's lands in no namespace of the machine's.
	stage.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	output, err := stage.CombinedOutput()
	if err != nil {
		t.Fatalf("the stage: %v\n%s", err, output)
	}
	report := reported(string(output))
	for _, want := range []struct{ key, value, why string }{
		{"agent own-gh", "gh-a", "the role reads its own gh bundle at the fixed path every role is told"},
		{"agent own-boot-token", "boot-a", "the role reads its generation's credentials at the fixed path the daemon names"},
		{"agent own-state", "writable", "the role's own state directory is writable at the fixed path"},
		{"agent sibling", "absent", "no sibling role's directory is reachable"},
		{"agent own-launcher-token", "absent", "no launcher token is reachable, the role's own included"},
		{"agent roles-entries", "0", "the roles directory is covered by an empty filesystem"},
		{"agent uncover", "refused", "the role cannot uncover the roles directory from a namespace of its own"},
		{"agent uid", "1000", "the role's agent runs as the image's unprivileged user"},
		{"agent outside-process", "absent", "the role sees only its own processes"},
		{"agent signal-outside", "refused", "the role cannot signal a process outside it"},
		{"agent init-mem", "refused", "the role cannot read the memory of its own role-init, which holds the cover's namespace"},
		{"agent init-environ", "refused", "the role cannot read the environment its own role-init was started with"},
		{"agent init-status-fd", "refused", "the role cannot reach the launcher's status pipe through its own role-init"},
		{"agent own-agent-secrets", "present", "the role reads its own agent-secrets key at the fixed path"},
		{"agent own-agent-secrets-key", "key-a", "the key it reads there is its own"},
		{"agent home-mount", "mounted-a", "a volume the home carries is bound back at its own path over the role's overlay"},
		{"agent home-mount-nested", "nested-a", "a mount inside a volume the home carries comes back with it"},
		{"agent home-mount-deep", "mounted-a", "a volume deep under the home lands at its path, made in the role's layer"},
		{"agent home-mount-writes", "volume", "what the role writes into a volume under its home reaches the volume"},
		{"agent home-mounts-view", "0", "the container's view of the home's volumes is covered: the role sees each at its own path alone"},
		{"agent tmp-mount", "mounted-a", "a mount /tmp carried is bound back over the role's own /tmp"},
		{"agent tmp-mount-nested", "nested-a", "a mount inside a mount /tmp carries comes back with it"},
		{"agent tmp-mount-nested-extra", "0", "role-init binds back only the outermost mount /tmp carries, whose recursive bind brings the one inside it, so the inner mount has no bind of its own: role-init resolves no path inside a volume every role writes, where a sibling role could put a link"},
		// The two mounts the container's /tmp hides (runStage): the stage's rows say its mount
		// table lists each and no path under /tmp reaches it, so the agent's rows read hidden
		// mounts and not mounts that were never made.
		{"stage shadowed", "listed reaches=absent", "the container's mount table lists the mount its /tmp hides under a later mount, and nothing is at its path"},
		{"stage beneath", "listed reaches=container", "the container's mount table lists the mount made beneath its /tmp after it, and its path reaches the container's own directory there"},
		{"agent tmp-shadowed", "absent", "a mount the container's /tmp hides under a later mount is not carried: no path under /tmp reaches it, and the role sees nothing at its path"},
		{"agent tmp-beneath", "absent", "a mount the container's /tmp hides, listed after the mount that hides it, is not carried either: the role sees nothing of the container's /tmp at its path"},
		{"agent home-carryover", "absent", "the role's first generation starts on a home layer with nothing of its own"},
		{"agent home-dir-rename", "ok", "the role renames a directory on its own home layer and the sandbox lives; under gVisor without userxattr the rename panics the sandbox instead"},
		{"stage upper-home", "present", "what the role wrote under its home landed on its own layer"},
		{"stage upper-tmp", "present", "what the role wrote in /tmp landed in its own temporary directory"},
		{"stage upper-var-tmp", "present", "what the role wrote in /var/tmp landed in its own temporary directory"},
		{"stage container-home", "absent", "what the role wrote under its home is invisible to the container"},
		{"stage container-tmp", "absent", "what the role wrote in /tmp is invisible to the container"},
		{"stage container-shm", "absent", "what the role wrote in /dev/shm is invisible to the container"},
		{"agent detached-carrier", "carried", "while the role ran, its detached process carried the generation's token and was the process that reported its ignore of SIGTERM: without that no process of the role ever carried the token, and `stage detached=gone` holds from the start"},
		{"stage detached", "gone", "a detached process of the role that ignores SIGTERM is gone once the role's init exits"},
		{"stage left-behind", "none", "no process of any generation outlives its role-init: the stage, the child subreaper of all it starts, is left no child to wait for"},
		{"stage status", "0", "role-init reports the shim's own wait status"},
		// The role's relaunch: a second generation of the same role, over the same role-homes
		// volume, with no agent-secrets directory, as a pod not enrolled with the broker has it.
		{"agent2 own-gh", "gh-a", "the relaunched role reads its own gh bundle again"},
		{"agent2 own-agent-secrets", "absent", "a role whose pod is not enrolled starts with no agent-secrets directory"},
		{"agent2 sibling", "absent", "the relaunched role reaches no sibling"},
		{"agent2 init-mem", "refused", "the relaunched role cannot read its role-init either"},
		{"agent2 home-carryover", "present", "the relaunched role keeps its own home layer across generations"},
		{"agent2 tmp-mount", "mounted-a", "the relaunch keeps the mount /tmp carries"},
		{"agent2 home-mount", "mounted-a", "the relaunch binds the home's volumes back again"},
		{"agent2 home-mount-deep", "mounted-a", "the relaunch makes the deep volume's place in its own layer again, over the link its first generation left there"},
		{"stage2 sibling-config", "absent", "a link the role left in its home layer is not followed: no directory appears at the sibling's own config home"},
		{"agent2 home-mount-other", "mounted-a", "the relaunch makes the other deep volume's place in its own layer again, over the link its first generation left there"},
		{"stage2 sibling-state", "absent", "a link the role left in its home layer is not followed: nothing appears in the sibling's state directory"},
		{"agent2 home-mounts-view", "0", "the relaunch covers the container's view of the home's volumes too"},
		{"stage2 status", "0", "the relaunch reports its own shim's wait status"},
		{"agent2 detached-carrier", "carried", "the relaunch's detached process carried its own token while the role ran"},
		{"stage2 detached", "gone", "the relaunch leaves nothing behind either"},
		// The config home: each role's own, seeded from the pod's shared one.
		{"agent config-legion", "legion-jj", "the role's config home holds the jj configuration workspace-init wrote in the shared one"},
		{"agent config-nested", "nested-a", "a mount under the shared config home comes back over the role's own"},
		{"agent config-planted", "absent", "the role's first generation starts on a config home with nothing of its own"},
		{"agent2 config-planted", "present", "the relaunched role keeps what it wrote in its own config home"},
		{"agent2 config-legion", "legion-jj", "the relaunched role still reads workspace-init's jj configuration"},
		{"agent-sibling own-gh", "gh-b", "the sibling role reads its own gh bundle"},
		{"agent-sibling config-legion", "legion-jj", "the sibling's config home holds workspace-init's jj configuration too, made again over a directory with no marker of a finished copy"},
		{"agent-sibling config-stale", "absent", "what stood at the sibling's own config home without the marker is gone"},
		{"agent-sibling config-planted", "absent", "what one role wrote in its config home never reaches a sibling's"},
		{"agent-sibling config-nested", "nested-a", "the sibling sees the mount under the shared config home too"},
		{"stage-sibling status", "0", "the sibling's role-init reports its shim's wait status"},
		{"agent-sibling detached-carrier", "carried", "the sibling's detached process carried its own token while the role ran"},
		{"stage-sibling detached", "gone", "the sibling leaves nothing behind"},
		{"stage shared-config-planted", "absent", "no role writes the pod's shared config home"},
		{"stage own-config-planted", "present", "what the role wrote in its config home landed in its own directory"},
		// Pseudo-terminals: the role's /dev/pts is a new instance holding none of the container's.
		{"agent pts-entries", "0", "the role's /dev/pts holds none of the container's terminals"},
		{"agent pty", "opened", "the role's agent can open a terminal of its own"},
		{"agent pts-own", "1", "the terminal the role opened is in its own /dev/pts"},
		{"agent2 pts-entries", "0", "the relaunch's /dev/pts holds none of the container's terminals either"},
		// The pod's test database log: the one file from under the roles directory a role reads,
		// through a read-only bind of that file alone.
		{"agent testpg-log", "testpg-line-1", "the role reads the pod's test database log at the fixed path every role is told"},
		{"agent testpg-log-write", "refused", "the role cannot write the log: role-init binds it read-only"},
		{"stage testpg-source", "testpg-line-1", "the role's write never reached the server's log, which holds what the stage wrote and nothing else"},
		// What the bind rests on: a namespace the role makes of its own may remount its view of
		// the log read-write (gVisor allows it, Linux refuses), and even then the log is a mount
		// point, which no rename, unlink or link replaces, so what the role reads is still the
		// server's file and the launcher's file is still a regular one.
		{"agent testpg-log-swap", "rename=busy unlink=busy link=busy", "a namespace of the role's own can neither rename a file over the log, nor remove it, nor link a file over it: it is a mount point"},
		{"agent testpg-log-after-swap", "testpg-line-1", "the log the role's own namespace reads after those tries is still the server's"},
		{"stage testpg-source-kind", "regular", "the server's log is still the regular file the launcher made, not a link or anything the role put there"},
		{"agent2 testpg-log", "testpg-line-1; testpg-line-2", "the relaunched role reads what the server has logged since, both lines"},
		{"agent2 testpg-log-swap", "rename=busy unlink=busy link=busy", "the relaunched role can neither replace nor remove the log either"},
		{"agent-sibling testpg-log", "", "a role of a pod that serves no test database starts with an empty mount point where the log would be"},
	} {
		if got, ok := report[want.key]; !ok || got != want.value {
			t.Errorf("%s = %q, want %q: %s\n%s", want.key, got, want.value, want.why, output)
		}
	}
	// The two keys the agent says when its detached process could not be started, or never
	// reported the ignore of SIGTERM it is there for, and then goes on. The table above names
	// neither, so nothing in it fails the row for them — and without a
	// process of the role's that ignores SIGTERM, `detached=gone` is met by role-init's own
	// SIGTERM and says nothing about the SIGKILL this case is about. So each key, under any
	// generation's prefix, fails the row here. Measured twice as a pass that proved nothing: with
	// the marker pointed at a path the agent cannot write, and with it pointed elsewhere under
	// `go test -overlay`.
	for _, key := range slices.Sorted(maps.Keys(report)) {
		who, what, _ := strings.Cut(key, " ")
		switch what {
		case "detached-start":
			t.Errorf("%s could not start its detached process: %s. No process of that generation then ignored SIGTERM, so its `detached=gone` says nothing about the SIGKILL this case is about\n%s", who, report[key], output)
		case "detached-trap":
			t.Errorf("%s said of its detached process: %s. That process may have died of role-init's own SIGTERM, so its `detached=gone` says nothing about the SIGKILL this case is about\n%s", who, report[key], output)
		}
	}
	// The relaunch's role-init says which link it replaced rather than followed.
	if want := "/deep was a link, which role-init does not follow on the way to a mount point"; !strings.Contains(string(output), want) {
		t.Errorf("the relaunch's role-init logged no %q\n%s", want, output)
	}
	// POSIX message queues, where the machine has a /dev/mqueue: the role's shows its own IPC
	// namespace's queues, none of the container's, and the container never sees one it makes.
	if report["stage mqueue"] == "present" {
		for _, want := range []struct{ key, value, why string }{
			{"agent container-queue", "absent", "the role's /dev/mqueue shows none of the container's queues"},
			{"agent own-queue", "made", "the role can make a queue in its own /dev/mqueue"},
			{"stage role-queue", "absent", "the container's /dev/mqueue shows none of the role's queues"},
		} {
			if got := report[want.key]; got != want.value {
				t.Errorf("%s = %q, want %q: %s\n%s", want.key, got, want.value, want.why, output)
			}
		}
	} else if report["stage mqueue"] != "absent" {
		t.Errorf("stage mqueue = %q, want present or absent\n%s", report["stage mqueue"], output)
	}
	// role-init and the agent, and nothing of the container's: the stage's own process, the
	// process it started outside, and the test binary that started the stage are all elsewhere.
	if pids, err := strconv.Atoi(report["agent pids"]); err != nil || pids < 2 || pids > 3 {
		t.Errorf("the role saw %q processes, want role-init and its own agent alone\n%s", report["agent pids"], output)
	}
}

// stageSelf is the test binary as a helper that covers stageMountPoint runs it again, as role-init
// and as the agent role-init starts: the kernel's link to the running executable, which reaches
// the file whatever mount now covers the path it was started from. `go test` builds the binary
// under GOTMPDIR, else TMPDIR, else /tmp, so that path can lie under the very directory a helper
// covers — /mnt, on a machine that keeps its scratch there — or under the /tmp the stage and
// role-init cover, and a process started by that path would find nothing there. A process started
// by this name runs the binary of the process that starts it, and each that starts one here runs
// this binary: the helper, and role-init, which starts the agent through the role's own /proc.
const stageSelf = "/proc/self/exe"

// stageShadowed and stageBeneath are the stage's own mounts under the /tmp it covers, which its
// own /tmp then hides: the first made before that /tmp, the second beneath it after (runStage).
const (
	stageShadowed = "/tmp/shadowed"
	stageBeneath  = "/tmp/beneath"
)

// stageMountPoint is a directory the stage can cover with a memory filesystem of its own. It is
// the one piece of the machine the test needs: the layout must sit outside /tmp and outside the
// home a role's overlay covers, and must be a path the role's own mounts never hide.
func stageMountPoint(t *testing.T) string {
	t.Helper()
	for _, dir := range []string{"/mnt", "/media", "/srv"} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	skipOutsideCI(t, "no directory to mount the role's layout on (/mnt, /media, /srv)")
	return ""
}

// reported is every `<who> <key>=<value>` line the stage and the agent printed.
func reported(output string) map[string]string {
	lines := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		who, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || (!strings.HasPrefix(who, "agent") && !strings.HasPrefix(who, "stage")) {
			continue
		}
		if key, value, ok := strings.Cut(rest, "="); ok {
			lines[who+" "+key] = value
		}
	}
	return lines
}

// TestRoleInitStageHelper is the container the role runs in: it builds a roles directory, a fixed
// root and a home inside a memory filesystem of its own, gives the home a mount of its own to
// carry, starts a process outside the role's namespaces, runs role-init over that layout, and
// reports what the container can see once the role is gone.
func TestRoleInitStageHelper(t *testing.T) {
	// Not parallel: a helper child, run in a process of its own; here it returns at once.
	encoded := os.Getenv(roleInitStageEnv)
	if encoded == "" {
		return
	}
	var plan stagePlan
	if err := json.Unmarshal([]byte(encoded), &plan); err != nil {
		fmt.Println("stage error:", err)
		os.Exit(2)
	}
	if err := runStage(t, plan); err != nil {
		fmt.Println("stage error:", err)
		os.Exit(1)
	}
	exitHelper(t)
}

// runStage is the stage's whole run, in the stage's own test binary t, whose deadline the case set
// (reentry), as each process of the test binary it starts has its own (role-init, and the agent
// role-init starts). Its wait for what each generation's role left (reportDetached) runs to the
// plan's Deadline, which ends before t's, so the stage prints that answer rather than its alarm.
func runStage(t *testing.T, plan stagePlan) error {
	if err := privateMounts(); err != nil {
		return err
	}
	// The stage is the child subreaper of all it starts (PR_SET_CHILD_SUBREAPER): a process of a
	// role that outlives its role-init, which no PID namespace of the role's then ends, is handed
	// to the stage as its parent exits, so the stage's own wait says whether any is left
	// (childrenLeft), and no scan of /proc can miss one between its reads.
	subreaper := ""
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		subreaper = "make the stage a child subreaper: " + err.Error()
	}
	if err := mountTmpfs(plan.Mount, "size=96m"); err != nil {
		return err
	}
	// Two mounts under /tmp that the container's own /tmp hides, as it would hide a mount the
	// machine's /tmp carries (a container runtime's leftover, say): made on a memory filesystem of
	// the stage's own over the machine's /tmp, so the machine never sees them. stageShadowed is
	// hidden by a mount made after it, the stage's own /tmp. stageBeneath is made after that /tmp,
	// beneath it, through a handle on the directory it covers, so the mount table lists it after
	// the mount that hides it, as a namespace gVisor copies may list a hidden mount: it numbers the
	// copy's mounts in the order it walks each mount's children, a Go map's (cloneMountTree,
	// gVisor's pkg/sentry/vfs/mount.go). The stage's own /tmp holds a directory of its own at
	// stageBeneath's path, so a role-init that carried a mount by its path alone would hand the
	// role the container's directory there.
	if err := mountTmpfs("/tmp", "size=1m"); err != nil {
		return err
	}
	below, err := os.Open("/tmp")
	if err != nil {
		return err
	}
	defer below.Close()
	if err := mountHidden(stageShadowed); err != nil {
		return err
	}
	// The stage's own /tmp and /dev/shm, so what the role plants if its privacy broke lands here
	// and never on the machine, and the stage can see that it did not. Each hides every mount the
	// machine's carries.
	for _, dir := range []string{"/tmp", "/dev/shm"} {
		if err := mountTmpfs(dir, "size=16m"); err != nil {
			return err
		}
	}
	if err := mountHidden(fmt.Sprintf("/proc/self/fd/%d/%s", below.Fd(), filepath.Base(stageBeneath))); err != nil {
		return err
	}
	if err := os.Mkdir(stageBeneath, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stageBeneath, "marker"), []byte("container"), 0o644); err != nil {
		return err
	}
	for _, path := range []string{stageShadowed, stageBeneath} {
		fmt.Printf("stage %s=%s\n", filepath.Base(path), hiddenMount(path))
	}
	rolesDir := filepath.Join(plan.Mount, "roles")
	fixedRoot := filepath.Join(plan.Mount, "fixed")
	home := filepath.Join(plan.Mount, "home")
	// The home mounts directory is reached through a link, as the worker image's is (its /var/run
	// is a link to /run): mountinfo names every mount under it by the link's target, so role-init
	// must find them there.
	homeMounts := filepath.Join(plan.Mount, "home-mounts")
	if err := os.MkdirAll(filepath.Join(plan.Mount, "run", "home-mounts"), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(filepath.Join(plan.Mount, "run", "home-mounts"), homeMounts); err != nil {
		return err
	}
	for _, role := range []string{plan.Role, plan.Sibling} {
		for _, name := range []string{RoleLauncherDir, RoleGHDir, RolePrivateDir, RoleStateDir, RoleAgentSecretsDir} {
			if err := os.MkdirAll(filepath.Join(rolesDir, role, name), 0o755); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Join(rolesDir, role, RoleGHDir, plan.Owner), 0o755); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(rolesDir, role, RolePrivateDir, "g1"), 0o700); err != nil {
			return err
		}
		for path, content := range map[string]string{
			filepath.Join(rolesDir, role, RoleLauncherDir, LauncherTokenFile):        "launcher-token-" + role,
			filepath.Join(rolesDir, role, RoleGHDir, plan.Owner, "hosts.yml"):        "gh-" + role,
			filepath.Join(rolesDir, role, RolePrivateDir, "g1", "LEGION_BOOT_TOKEN"): "boot-" + role,
			filepath.Join(rolesDir, role, RoleAgentSecretsDir, "key.pem"):            "key-" + role,
		} {
			if err := os.WriteFile(path, []byte(content), 0o400); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{RoleGHDir, RolePrivateDir, RoleStateDir, RoleAgentSecretsDir} {
		if err := os.MkdirAll(filepath.Join(fixedRoot, name), 0o755); err != nil {
			return err
		}
	}
	// The pod's test database log, as the launcher makes it under the roles directory before any
	// role starts, holding the server's first line, and its mount point under the fixed root,
	// the empty file the worker image makes.
	serverLog := testPGLog(rolesDir)
	if err := os.MkdirAll(testPGDir(rolesDir), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(serverLog, []byte("testpg-line-1\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(fixedRoot, RoleTestPGLog), nil, 0o444); err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	// The volumes the home carries, as an issue pod's sessions and operator mounts do: mounted
	// under the home mounts at their path relative to the home, as the pod builder renders them,
	// so the home itself carries none. One sits shallow and holds a mount of its own; one sits
	// deep, under directories the home does not have. A mount /tmp carries, as an operator's
	// volume may, is carried over the role's own /tmp, with a mount of its own inside it. The
	// pod's shared config home sits at its own place, holding the jj configuration workspace-init
	// writes there and a mount of its own.
	carried := filepath.Join(plan.Mount, "carried")
	nested := filepath.Join(plan.Mount, "nested")
	shared := filepath.Join(plan.Mount, "shared-config")
	for dir, marker := range map[string]string{carried: "mounted-" + plan.Role, nested: "nested-" + plan.Role} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(marker), 0o644); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(carried, "nested"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(shared, "jj", "repos", "id"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(shared, "jj", "repos", "id", "config.toml"), []byte("legion-jj"), 0o644); err != nil {
		return err
	}
	for _, bind := range [][2]string{
		{carried, filepath.Join(homeMounts, "mounted")},
		{nested, filepath.Join(homeMounts, "mounted", "nested")},
		{carried, filepath.Join(homeMounts, "other", "cache")},
		{carried, filepath.Join(homeMounts, "deep", "sessions")},
		{carried, "/tmp/mounted"},
		{nested, "/tmp/mounted/nested"},
		{shared, filepath.Join(homeMounts, ConfigHomeDir)},
		{nested, filepath.Join(homeMounts, ConfigHomeDir, "nested")},
	} {
		if err := os.MkdirAll(bind[1], 0o755); err != nil {
			return err
		}
		if err := bindMount(bind[0], bind[1]); err != nil {
			return err
		}
	}

	// A process outside the role, which the role must not see or signal. A parent-death signal ends
	// it with the stage, so a stage that dies at its own alarm, which runs no defer, leaves no sleep
	// behind it on the machine; the deferred kill ends it on every return.
	outside := exec.Command("/bin/sleep", "600")
	outside.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := outside.Start(); err != nil {
		return err
	}
	defer func() { _ = outside.Process.Kill(); _, _ = outside.Process.Wait() }()
	// A terminal of the container's own, held open while the role runs, and, where the machine
	// has a /dev/mqueue, a message queue of the container's IPC namespace: neither may reach the
	// role, and nothing the role opens may reach the container.
	pty, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return err
	}
	defer pty.Close()
	mqueue := "absent"
	if _, err := os.Stat("/dev/mqueue"); err == nil {
		if err := syscall.Mount("mqueue", "/dev/mqueue", "mqueue", 0, ""); err != nil {
			return err
		}
		room, err := makeQueue("container-queue")
		if err != nil {
			return err
		}
		fmt.Printf("stage container-queue-room=%s\n", room)
		if listed := presence("/dev/mqueue/container-queue"); listed != "present" {
			return fmt.Errorf("the stage's /dev/mqueue does not list the queue it made in its own IPC namespace: %s", listed)
		}
		mqueue = "present"
	}
	fmt.Printf("stage mqueue=%s\n", mqueue)
	roleInitLaunch = roleInitHelperLaunch(t, stageSelf)
	layout := agentPlan{
		Home: home, HomeMounts: homeMounts, RolesDir: rolesDir, FixedRoot: fixedRoot, Role: plan.Role,
		Sibling: plan.Sibling, Owner: plan.Owner, OutsidePID: outside.Process.Pid, Detached: detachedToken(),
		TestPGLog: true, Deadline: plan.Deadline,
	}

	// Each generation's role is looked for once every generation has run (reportDetached), in
	// the namespaces the stage holds from just after it starts the generation's role-init, and
	// its lines are printed however the stage ends.
	var readings []detachedReading
	defer func() {
		// The process outside the role is the stage's own child, and the agents are done with it.
		_ = outside.Process.Kill()
		_, _ = outside.Process.Wait()
		reportDetached(readings, subreaper, plan.Deadline)
	}()
	hold, err := runGeneration(t, plan, layout, 1, "agent")
	readings = append(readings, takeDetached("stage", layout.Detached, hold))
	if err != nil {
		return err
	}
	homes := filepath.Join(rolesDir, RoleHomesDir, plan.Role)
	fmt.Printf("stage upper-home=%s\n", presence(filepath.Join(homes, "upper", "planted")))
	fmt.Printf("stage upper-tmp=%s\n", presence(filepath.Join(homes, "tmp", "planted")))
	fmt.Printf("stage upper-var-tmp=%s\n", presence(filepath.Join(homes, "var-tmp", "planted")))
	fmt.Printf("stage container-home=%s\n", presence(filepath.Join(home, "planted")))
	fmt.Printf("stage container-tmp=%s\n", presence(filepath.Join("/tmp", "planted")))
	fmt.Printf("stage container-shm=%s\n", presence(filepath.Join("/dev/shm", "planted")))
	// The log's lines go on the one line a report carries (oneLine), here and in the agent.
	fmt.Printf("stage testpg-source=%s\n", oneLine(content(serverLog)))
	// Read without following a link: a link the role had put in the file's place would name
	// something else, and a link is what role-init refuses to bind.
	fmt.Printf("stage testpg-source-kind=%s\n", fileKind(serverLog))

	// The role's relaunch: a second generation over the same role-homes volume, with the
	// agent-secrets directory gone, as a pod that is not enrolled with the secrets broker has it.
	// Its first generation left, in its own home layer, a link where each deep volume's place
	// goes: one naming the sibling's own config home under the roles directory, which the sibling
	// has not made yet, and one naming the sibling's state directory, which exists. role-init must
	// make each volume's place in the role's layer without following either link.
	if err := os.RemoveAll(filepath.Join(rolesDir, plan.Role, RoleAgentSecretsDir)); err != nil {
		return err
	}
	siblingConfig := filepath.Join(rolesDir, RoleHomesDir, plan.Sibling, RoleConfigHomeDir)
	siblingState := filepath.Join(rolesDir, plan.Sibling, RoleStateDir)
	for link, target := range map[string]string{"deep": siblingConfig, "other": siblingState} {
		if err := os.RemoveAll(filepath.Join(homes, "upper", link)); err != nil {
			return err
		}
		if err := os.Symlink(target, filepath.Join(homes, "upper", link)); err != nil {
			return err
		}
	}
	// The server has gone on logging since the role's first generation: a second line, appended
	// to the same file as the server appends.
	appending, err := os.OpenFile(serverLog, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := appending.WriteString("testpg-line-2\n"); err != nil {
		return err
	}
	if err := appending.Close(); err != nil {
		return err
	}
	layout.Detached = detachedToken()
	// What the links named is reported even when the generation failed, so a role-init that
	// followed one shows where it wrote.
	hold, relaunched := runGeneration(t, plan, layout, 2, "agent2")
	readings = append(readings, takeDetached("stage2", layout.Detached, hold))
	fmt.Printf("stage2 sibling-config=%s\n", presence(siblingConfig))
	fmt.Printf("stage2 sibling-state=%s\n", presence(filepath.Join(siblingState, "cache")))
	if relaunched != nil {
		return relaunched
	}

	// The sibling role's first generation, after the role's two have written their own config
	// home: what the role wrote there never reaches the sibling, nor the pod's shared one. A
	// directory already stands at the sibling's own config home, as one a generation stopped
	// midway, or anything else, would leave: it holds no marker of a finished copy, so the
	// sibling's role-init makes the copy again.
	if err := os.MkdirAll(filepath.Join(siblingConfig, "stale"), 0o755); err != nil {
		return err
	}
	// The sibling starts as a role of a pod that serves no test database does: with no log under
	// the roles directory, so role-init binds nothing over the mount point.
	if err := os.Remove(serverLog); err != nil {
		return err
	}
	siblingPlan := plan
	siblingPlan.Role, siblingPlan.Sibling = plan.Sibling, plan.Role
	siblingLayout := layout
	siblingLayout.Role, siblingLayout.Sibling, siblingLayout.Detached = plan.Sibling, plan.Role, detachedToken()
	siblingLayout.TestPGLog = false
	hold, err = runGeneration(t, siblingPlan, siblingLayout, 1, "agent-sibling")
	readings = append(readings, takeDetached("stage-sibling", siblingLayout.Detached, hold))
	if err != nil {
		return err
	}
	fmt.Printf("stage shared-config-planted=%s\n", presence(filepath.Join(shared, "git", "config")))
	fmt.Printf("stage own-config-planted=%s\n", presence(filepath.Join(homes, RoleConfigHomeDir, "git", "config")))
	if mqueue == "present" {
		fmt.Printf("stage role-queue=%s\n", presence("/dev/mqueue/role-queue"))
	}
	return nil
}

// copyExecutable copies the executable at source to target, which the caller then runs in its
// place.
func copyExecutable(source, target string) error {
	body, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return testexec.WriteFile(target, body, 0o755)
}

// mountHidden makes path a memory filesystem of the stage's own that holds the marker `hidden`.
func mountHidden(path string) error {
	if err := os.Mkdir(path, 0o755); err != nil {
		return err
	}
	if err := mountTmpfs(path, "size=1m"); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(path, "marker"), []byte("hidden"), 0o644)
}

// hiddenMount says how the stage's container sees a mount it hid at path: whether its mount table
// lists a mount there, read from mountinfo itself, and what the path reaches (reached).
func hiddenMount(path string) string {
	listed := "unlisted"
	switch count, err := mountsAt(path); {
	case err != nil:
		return "unreadable: " + err.Error()
	case count > 0:
		listed = "listed"
	}
	return listed + " reaches=" + reached(path)
}

// mountsAt is how many mounts this process's mount table lists at path, read from mountinfo
// itself.
func mountsAt(path string) (int, error) {
	table, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(string(table), "\n") {
		if fields := strings.Fields(line); len(fields) > 4 && fields[4] == path {
			count++
		}
	}
	return count, nil
}

// reached is what path reaches where the stage hid a mount: `absent` where nothing is there, else
// the marker of what is there, `hidden` for the hidden mount itself and `container` for the
// directory the container's own /tmp holds over it.
func reached(path string) string {
	if found := presence(path); found != "present" {
		return found
	}
	return content(filepath.Join(path, "marker"))
}

// runGeneration runs one generation of the role over the stage's layout and reports the wait
// status role-init gave back for it. It holds the role's namespaces from just after role-init
// starts (holdRole) and hands them back, for the caller to read and then let go of; with no
// role-init started it holds none.
func runGeneration(t *testing.T, plan stagePlan, layout agentPlan, generation uint64, label string) (roleHold, error) {
	layout.Label = label
	layout.Generation = generation
	agent, err := json.Marshal(layout)
	if err != nil {
		return roleHold{unreadable: "no role-init was started"}, err
	}
	process, err := newRoleProcess(RoleInitConfig{
		RoleCover: RoleCover{
			RolesDir: layout.RolesDir, Home: layout.Home,
			HomeMounts: layout.HomeMounts, FixedRoot: layout.FixedRoot,
		},
		Role: plan.Role, Generation: generation,
		Argv: append([]string{stageSelf}, reentry(t, "TestRoleInitAgentHelper")...),
	}, append(os.Environ(), roleInitAgentEnv+"="+string(agent)), os.Stdout, os.Stderr)
	if err != nil {
		return roleHold{unreadable: "no role-init was started"}, err
	}
	if err := process.start(); err != nil {
		return roleHold{unreadable: "no role-init was started"}, err
	}
	hold := holdRole(process.cmd.Process.Pid)
	status, _ := io.ReadAll(process.status)
	_ = process.status.Close()
	if err := process.cmd.Wait(); err != nil {
		return hold, fmt.Errorf("role-init generation %d: %w", generation, err)
	}
	// The stage's line for a generation carries the agent's label with `stage` in place of
	// `agent`: stage, stage2, stage-sibling.
	who := "stage" + strings.TrimPrefix(label, "agent")
	if reported := parseStatus(status); reported == nil {
		fmt.Printf("%s status=none\n", who)
	} else {
		fmt.Printf("%s status=%d\n", who, reported.ExitStatus())
	}
	return hold, nil
}

// detachedToken is the token one generation's detached process carries in its command line, by
// which leftBehind tells it from every other process of the machine: random, so no process
// outside the role holds it, and drawn again for each generation, so no generation's reading
// finds another's process.
func detachedToken() string { return "legion-role-detached-" + rand.Text() }

// detachedReading is one generation's role as the stage looks for what it left once that
// generation's role-init has exited: the stage's line for it (who), the token the detached
// process's command line carried, and the role's namespaces the stage holds (roleHold).
type detachedReading struct {
	who, token string
	hold       roleHold
}

// roleHold is one generation's role namespaces as the stage holds them open, from just after it
// starts role-init until it has read what the role left (reportDetached). A namespace's link is
// its inode number, which a namespace made after the role's has ended may be given again; one
// held open cannot end, so no later namespace takes its number, and a process found in it is the
// role's whenever it started. The stage holds the role's PID and IPC namespaces where it does not
// share them (links), and the role's user namespace (user): every namespace the role's
// processes make, a user namespace of their own included, descends from it. It does not hold the
// role's mount namespace: that would keep the generation's home layer mounted while the next
// generation mounts its own over the same directories. Unreadable says why the stage holds
// nothing to look in; userUnread why it reads no user namespace's ancestry.
type roleHold struct {
	files                  []*os.File
	links                  map[string]string
	user                   *os.File
	userUnread, unreadable string
}

// nsGetParent is NS_GET_PARENT, _IO(0xb7, 0x2) (include/uapi/linux/nsfs.h): the ioctl that opens
// the parent of the user namespace a descriptor holds.
const nsGetParent = 0xb702

// holdRole opens the namespaces of role-init at pid (roleHold), each named by its link, and keeps
// those the stage does not share. It checks, on the role's own user namespace, that the kernel
// answers its parent with the stage's user namespace: where it does not (gVisor shows no real
// user namespace), it reads no ancestry and says so.
func holdRole(pid int) roleHold {
	hold := roleHold{links: map[string]string{}}
	for _, kind := range []string{"pid", "ipc", "user"} {
		file, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "ns", kind))
		if err != nil {
			if kind == "user" {
				hold.userUnread = "the role's user namespace: " + err.Error()
				continue
			}
			hold.unreadable = fmt.Sprintf("hold the role's %s namespace: %v", kind, err)
			return hold
		}
		hold.files = append(hold.files, file)
		link, err := os.Readlink(filepath.Join("/proc/self/fd", strconv.Itoa(int(file.Fd()))))
		if err != nil {
			hold.unreadable = fmt.Sprintf("the link of the role's %s namespace: %v", kind, err)
			return hold
		}
		own, err := os.Readlink(filepath.Join("/proc/self/ns", kind))
		if err != nil {
			hold.unreadable = fmt.Sprintf("the stage's own %s namespace: %v", kind, err)
			return hold
		}
		switch {
		case link == own && kind == "user":
			hold.userUnread = "the role shares the stage's user namespace"
		case link == own:
		case kind == "user":
			hold.user = file
		default:
			hold.links[kind] = link
		}
	}
	if hold.user != nil {
		if parent, err := namespaceParent(hold.user); err != nil {
			hold.user, hold.userUnread = nil, "the parent of the role's user namespace: "+err.Error()
		} else if stage, err := os.Open("/proc/self/ns/user"); err != nil {
			_ = parent.Close()
			hold.user, hold.userUnread = nil, "the stage's own user namespace: "+err.Error()
		} else {
			if !sameNamespace(parent, stage) {
				hold.user, hold.userUnread = nil, "the parent of the role's user namespace is not the stage's"
			}
			_ = parent.Close()
			_ = stage.Close()
		}
	}
	if len(hold.links) == 0 && hold.user == nil {
		hold.unreadable = "the role shares with the stage every namespace the stage could look in"
	}
	return hold
}

// close lets go of every namespace the hold keeps open.
func (hold roleHold) close() {
	for _, file := range hold.files {
		_ = file.Close()
	}
}

// namespaceParent opens the parent of the user namespace file holds (NS_GET_PARENT). The kernel
// refuses it, with EPERM, for a parent outside the caller's own user namespace and those under
// it.
func namespaceParent(file *os.File) (*os.File, error) {
	fd, err := unix.IoctlRetInt(int(file.Fd()), nsGetParent)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "parent user namespace"), nil
}

// sameNamespace says whether two namespace descriptors hold one namespace: the same inode of the
// same namespace filesystem.
func sameNamespace(a, b *os.File) bool {
	var one, two unix.Stat_t
	return unix.Fstat(int(a.Fd()), &one) == nil && unix.Fstat(int(b.Fd()), &two) == nil && one.Dev == two.Dev && one.Ino == two.Ino
}

// takeDetached is the stage's reading of the generation whose line is who, whose detached process
// carried token, and whose namespaces the stage holds in hold.
func takeDetached(who, token string, hold roleHold) detachedReading {
	if hold.userUnread != "" {
		fmt.Printf("%s detached-user-ancestry=unread: %s\n", who, hold.userUnread)
	}
	return detachedReading{who: who, token: token, hold: hold}
}

// reportDetached prints, for each of readings, `<who> detached=` and whether anything of that
// generation's role is still there, naming each process that is; then `stage left-behind=`,
// whether the stage, the child subreaper of all it starts, is left any child (childrenLeft),
// or why it cannot tell (subreaper, or a canary it did not receive: subreaperReceives); and then
// lets go of the namespaces it held. The stage
// shares the machine's PID namespace, so /proc lists every process of the machine. A process
// reads as the role's by the token (detachedToken), never by a value a process outside the role
// may carry — a pid, a port, a temporary directory's random digits — which would read a stranger
// as the role's detached process still running; or by running in one of the role's namespaces
// the stage holds, or in a user namespace that descends from the role's (roleHold): a process
// that has exec'd another command no longer carries the token, nor does a process it started,
// and a process that made namespaces of its own still descends from the role's user namespace.
// A scan of /proc is no one moment, though: a chain of processes, each starting the next and
// exiting, can be read after the one it has left and before the one it has started. The
// subreaper's wait is one moment, so it decides whether anything is left, and the scan says
// what.
//
// Each generation's role-init has been waited for. Linux lets a PID namespace's init be waited
// for only once every other process of the namespace is gone (kernel/pid_namespace.c,
// zap_pid_ns_processes), so there the first reading is the answer. gVisor's init sends each of
// them SIGKILL and exits without that wait (pkg/sentry/kernel/task_exit.go, exitChildrenLocked),
// so a process can still be ending: the reading waits for it until deadline, the test binary's
// own less beforeDeadline's margin, and a process still there then is one the namespace's end
// left alive. It reads every generation's at once, after the last generation: a wait spent to
// the deadline then leaves no generation still to run, so the row fails by name rather than at
// the binary's own deadline. A scan reads every process of the machine, which on a loaded
// machine of thousands of processes takes seconds, so the reading starts no scan that, as long as
// the longest one so far, would end past the deadline: what it read last is then the answer. Its
// line `stage detached-reading=` says how many scans it made and the longest.
func reportDetached(readings []detachedReading, subreaper string, deadline time.Time) {
	defer func() {
		for _, reading := range readings {
			reading.hold.close()
		}
	}()
	if subreaper == "" {
		subreaper = subreaperReceives()
	}
	var found [][]string
	var unreadable, unwaited error
	left := false
	var scans int
	var longest time.Duration
	began := time.Now()
	testwait.Holds(time.Until(deadline), 50*time.Millisecond, func() bool {
		scan := time.Now()
		found, unreadable = leftBehind(readings)
		if subreaper == "" {
			left, unwaited = childrenLeft()
		}
		scans++
		longest = max(longest, time.Since(scan))
		scanned := unreadable != nil || !slices.ContainsFunc(found, func(each []string) bool { return len(each) > 0 })
		return scanned && (unwaited != nil || !left) || time.Until(deadline) < 2*longest
	})
	fmt.Printf("stage detached-reading=%d scans in %s, the longest %s\n", scans, time.Since(began).Round(time.Millisecond), longest.Round(time.Millisecond))
	for index, reading := range readings {
		verdict := "gone"
		switch {
		case reading.hold.unreadable != "":
			verdict = "unreadable: " + reading.hold.unreadable
		case unreadable != nil:
			verdict = "unreadable: " + unreadable.Error()
		case len(found[index]) > 0:
			verdict = "running: " + strings.Join(found[index], "; ")
		}
		fmt.Printf("%s detached=%s\n", reading.who, verdict)
	}
	switch {
	case subreaper != "":
		fmt.Printf("stage left-behind=unread: %s\n", subreaper)
	case unwaited != nil:
		fmt.Printf("stage left-behind=unread: %v\n", unwaited)
	case left:
		fmt.Printf("stage left-behind=%s\n", childrenNamed())
	default:
		fmt.Println("stage left-behind=none")
	}
}

// childrenLeft reaps every child of the stage that has exited and says whether any child is
// left: a wait4 the kernel answers in one moment, ECHILD when the stage has no child at all.
func childrenLeft() (bool, error) {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG|syscall.WALL, nil)
		switch {
		case errors.Is(err, syscall.ECHILD):
			return false, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case err != nil:
			return false, fmt.Errorf("wait for the stage's children: %w", err)
		case pid == 0:
			return true, nil
		}
	}
}

// subreaperReceives is the control of the left-behind reading: an empty answer when the stage
// receives an orphan of its own and its reading sees it, else why not. The reading says `none`
// when the stage's wait finds no child; a stage that is no subreaper, or a reading that never
// waits, says the same while what a role left still runs elsewhere. So the stage starts a
// canary: a shell that starts a sleep and exits, which orphans the sleep. A subreaper is handed
// the orphan as the shell exits, before its wait returns. Then childrenLeft, the reading itself,
// must find a child, and a wait4 on the sleep's own pid must find it the stage's live child.
// The sleep is killed and reaped either way, by its pid.
func subreaperReceives() string {
	const failed = "the subreaper did not receive its canary"
	out, err := exec.Command("/bin/sh", "-c", "/bin/sleep 600 </dev/null >/dev/null 2>&1 & echo $!").Output()
	if err != nil {
		return fmt.Sprintf("%s: start it: %v", failed, err)
	}
	canary, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return fmt.Sprintf("%s: its pid %q: %v", failed, out, err)
	}
	defer func() {
		_ = syscall.Kill(canary, syscall.SIGKILL)
		var status syscall.WaitStatus
		for {
			if _, err := syscall.Wait4(canary, &status, syscall.WALL, nil); !errors.Is(err, syscall.EINTR) {
				return
			}
		}
	}()
	left, err := childrenLeft()
	switch {
	case err != nil:
		return fmt.Sprintf("%s: %v", failed, err)
	case !left:
		return fmt.Sprintf("%s: the stage's wait found no child while its canary, pid %d, ran", failed, canary)
	}
	var status syscall.WaitStatus
	if pid, err := syscall.Wait4(canary, &status, syscall.WNOHANG|syscall.WALL, nil); err != nil || pid != 0 {
		return fmt.Sprintf("%s: a wait on its pid %d answered pid %d, error %v, not a live child of the stage's", failed, canary, pid, err)
	}
	return ""
}

// childrenNamed names each child the stage has, by pid and command line, from each of its threads'
// /proc/self/task/<tid>/children: a list read in several reads, so a child that moved between
// them can be missing from it, which the answer then says.
func childrenNamed() string {
	var named []string
	tasks, _ := filepath.Glob("/proc/self/task/*/children")
	for _, task := range tasks {
		body, err := os.ReadFile(task)
		if err != nil {
			continue
		}
		for _, pid := range strings.Fields(string(body)) {
			cmdline, _ := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
			named = append(named, fmt.Sprintf("pid %s (%s)", pid, strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))))
		}
	}
	if len(named) == 0 {
		return "children the stage's wait found and its thread lists did not name"
	}
	return "children of the stage's, none its own: " + strings.Join(named, "; ")
}

// leftBehind names, for each of readings, each process of the machine whose command line holds
// its token, or that runs in its role (processNamespaces.in), by its pid and its command line. A
// /proc that does not list the stage's own process is not the stage's PID namespace's, and no
// reading of it says that a process is gone.
func leftBehind(readings []detachedReading) ([][]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	self, listedSelf := strconv.Itoa(os.Getpid()), false
	found := make([][]string, len(readings))
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		listedSelf = listedSelf || entry.Name() == self
		body, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmdline := strings.TrimSpace(strings.ReplaceAll(string(body), "\x00", " "))
		// A process that has exited, a zombie included, runs in no namespace of the role's.
		state, _, err := procStat(entry.Name())
		live := err == nil && state != "Z" && state != "X"
		process := processNamespaces{pid: entry.Name(), links: map[string]string{}}
		for index, reading := range readings {
			if reading.hold.unreadable != "" {
				continue
			}
			where := ""
			if live {
				if where, err = process.in(reading.hold); err != nil {
					return nil, err
				}
			}
			switch carries := strings.Contains(string(body), reading.token); {
			case where != "":
				found[index] = append(found[index], fmt.Sprintf("pid %s (%s), %s", entry.Name(), cmdline, where))
			case carries:
				found[index] = append(found[index], fmt.Sprintf("pid %s (%s)", entry.Name(), cmdline))
			}
		}
	}
	if !listedSelf {
		return nil, fmt.Errorf("/proc does not list the stage's own pid %s, so it is not the stage's PID namespace's", self)
	}
	return found, nil
}

// processNamespaces is what one scan of leftBehind reads of the namespaces of the process at pid,
// once for every reading: its namespace links (links), and the user namespaces from its own up
// through each parent the kernel will open for the stage (ancestry).
type processNamespaces struct {
	pid      string
	links    map[string]string
	ancestry []unix.Stat_t
	walked   bool
}

// in says where the process runs in the role hold keeps — `in the role's <kind> namespace`, or
// `in a user namespace of the role's` — or the empty string for nowhere. A process whose
// namespaces the kernel will not show the stage is nowhere: every process of the role runs in a
// user namespace that descends from the stage's own, where the stage holds every capability and
// so may read any of them, so a refused read is of a process outside the role — measured: another
// process of this uid on the machine refused it.
func (process *processNamespaces) in(hold roleHold) (string, error) {
	for _, kind := range []string{"pid", "ipc"} {
		want, ok := hold.links[kind]
		if !ok {
			continue
		}
		link, read := process.links[kind]
		if !read {
			var err error
			link, err = os.Readlink(filepath.Join("/proc", process.pid, "ns", kind))
			switch {
			case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
				link = ""
			case err != nil:
				return "", fmt.Errorf("the %s namespace of pid %s: %w", kind, process.pid, err)
			}
			process.links[kind] = link
		}
		if link == want {
			return "in the role's " + kind + " namespace", nil
		}
	}
	if hold.user == nil {
		return "", nil
	}
	var role unix.Stat_t
	if err := unix.Fstat(int(hold.user.Fd()), &role); err != nil {
		return "", fmt.Errorf("the role's user namespace: %w", err)
	}
	process.walk()
	for _, each := range process.ancestry {
		if each.Dev == role.Dev && each.Ino == role.Ino {
			return "in a user namespace of the role's", nil
		}
	}
	return "", nil
}

// walk reads, once, the user namespace of the process and each parent of it the kernel opens for
// the stage: it stops at the first it refuses, which is above the stage's own, and at 33, more
// than the 32 levels the kernel allows.
func (process *processNamespaces) walk() {
	if process.walked {
		return
	}
	process.walked = true
	file, err := os.Open(filepath.Join("/proc", process.pid, "ns", "user"))
	if err != nil {
		return
	}
	for range 33 {
		var each unix.Stat_t
		if unix.Fstat(int(file.Fd()), &each) != nil {
			break
		}
		process.ancestry = append(process.ancestry, each)
		parent, err := namespaceParent(file)
		if err != nil {
			break
		}
		_ = file.Close()
		file = parent
	}
	_ = file.Close()
}

// procStat reads the state and the start time of the process at pid from /proc/<pid>/stat: its
// third and twenty-second fields, counted past the command in parentheses, which may itself hold
// spaces and parentheses.
func procStat(pid string) (string, string, error) {
	body, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return "", "", err
	}
	end := strings.LastIndexByte(string(body), ')')
	if end < 0 {
		return "", "", fmt.Errorf("/proc/%s/stat holds no command: %q", pid, body)
	}
	fields := strings.Fields(string(body[end+1:]))
	if len(fields) < 20 {
		return "", "", fmt.Errorf("/proc/%s/stat holds %d fields after its command, not 20", pid, len(fields))
	}
	return fields[0], fields[19], nil
}

// TestRoleInitAgentHelper is the worker shim's stand-in: the process role-init starts inside the
// role's namespaces, which reports what the role can reach and leaves a detached process behind.
// Every line it prints carries its generation's label, so the stage's two runs read apart.
func TestRoleInitAgentHelper(t *testing.T) {
	// Not parallel: a helper child, run in a process of its own; here it returns at once.
	encoded := os.Getenv(roleInitAgentEnv)
	if encoded == "" {
		return
	}
	var plan agentPlan
	if err := json.Unmarshal([]byte(encoded), &plan); err != nil {
		fmt.Println("agent error:", err)
		os.Exit(2)
	}
	say := func(key, value string) { fmt.Printf("%s %s=%s\n", plan.Label, key, value) }
	say("uid", strconv.Itoa(os.Getuid()))
	say("own-gh", content(filepath.Join(plan.FixedRoot, RoleGHDir, plan.Owner, "hosts.yml")))
	say("own-boot-token", content(filepath.Join(plan.FixedRoot, RolePrivateDir, "g1", "LEGION_BOOT_TOKEN")))
	say("own-state", writable(filepath.Join(plan.FixedRoot, RoleStateDir)))
	// The pod's test database log at its fixed path: what the server has written so far, which
	// the role reads and cannot add to.
	say("testpg-log", oneLine(content(filepath.Join(plan.FixedRoot, RoleTestPGLog))))
	say("testpg-log-write", appended(filepath.Join(plan.FixedRoot, RoleTestPGLog)))
	// The property that bind rests on, tried from a further namespace of the role's own, as an
	// agent's `unshare -Urm` makes one: the log is a mount point there too, so even over a view
	// remounted read-write it can be neither replaced nor removed. A generation with no log bound
	// has an empty file of the image's there, which nothing need hold, so it tries nothing.
	if plan.TestPGLog {
		remount, swap, after := swapLog(plan.FixedRoot)
		say("testpg-log-remount", remount)
		say("testpg-log-swap", swap)
		say("testpg-log-after-swap", after)
	} else {
		say("testpg-log-swap", "no-log")
	}
	// The agent-secrets directory is bound only when the pod is enrolled with the broker: its
	// key is the role's own in the first generation, and the second runs with none.
	say("own-agent-secrets", presence(filepath.Join(plan.FixedRoot, RoleAgentSecretsDir, "key.pem")))
	say("own-agent-secrets-key", content(filepath.Join(plan.FixedRoot, RoleAgentSecretsDir, "key.pem")))
	say("sibling", presence(filepath.Join(plan.RolesDir, plan.Sibling)))
	say("own-launcher-token", presence(filepath.Join(plan.RolesDir, plan.Role, RoleLauncherDir, LauncherTokenFile)))
	if entries, err := os.ReadDir(plan.RolesDir); err != nil {
		say("roles-entries", "unreadable: "+err.Error())
	} else {
		say("roles-entries", strconv.Itoa(len(entries)))
	}
	say("uncover", uncover(plan.RolesDir, plan.Sibling, syscall.CLONE_NEWUSER|syscall.CLONE_NEWNS))
	say("pids", strconv.Itoa(processes()))
	say("outside-process", presence(filepath.Join("/proc", strconv.Itoa(plan.OutsidePID))))
	say("signal-outside", signalOutside(plan.OutsidePID))
	// Its own role-init, PID 1 of this namespace, holds CAP_SYS_ADMIN over the mount namespace
	// that carries the cover and the launcher's status pipe. The role runs in a user namespace
	// of its own, so the kernel refuses it every read of that process: its memory, the
	// environment its generation was started with, and the descriptors it holds.
	say("init-mem", readable("/proc/1/mem"))
	say("init-environ", readable("/proc/1/environ"))
	say("init-status-fd", readable("/proc/1/fd/"+strconv.Itoa(roleStatusFD)))
	say("home-mount", content(filepath.Join(plan.Home, "mounted", "marker")))
	say("home-mount-nested", content(filepath.Join(plan.Home, "mounted", "nested", "marker")))
	say("home-mount-deep", content(filepath.Join(plan.Home, "deep", "sessions", "marker")))
	say("home-mount-other", content(filepath.Join(plan.Home, "other", "cache", "marker")))
	if err := os.WriteFile(filepath.Join(plan.Home, "mounted", "written-"+plan.Label), []byte("volume"), 0o600); err != nil {
		say("home-mount-writes", err.Error())
	} else {
		say("home-mount-writes", content(filepath.Join(plan.Home, "deep", "sessions", "written-"+plan.Label)))
	}
	if entries, err := os.ReadDir(plan.HomeMounts); err != nil {
		say("home-mounts-view", "unreadable: "+err.Error())
	} else {
		say("home-mounts-view", strconv.Itoa(len(entries)))
	}
	say("tmp-mount", content(filepath.Join("/tmp", "mounted", "marker")))
	say("tmp-mount-nested", content(filepath.Join("/tmp", "mounted", "nested", "marker")))
	// How many mounts the role's table lists at the carried mount and at the mount inside it. The
	// container's own mount at each path is in the role's table too, hidden under the role's own
	// /tmp, so the row reads how many more the inner path has than the outer one: one more is a
	// bind role-init made of the inner mount by its own path.
	outer, outerErr := mountsAt(filepath.Join("/tmp", "mounted"))
	inner, innerErr := mountsAt(filepath.Join("/tmp", "mounted", "nested"))
	if err := errors.Join(outerErr, innerErr); err != nil {
		say("tmp-mount-nested-extra", "unreadable: "+err.Error())
	} else {
		say("tmp-mount-mounts", strconv.Itoa(outer))
		say("tmp-mount-nested-mounts", strconv.Itoa(inner))
		say("tmp-mount-nested-extra", strconv.Itoa(inner-outer))
	}
	// The mounts the container's /tmp hides are no mounts of /tmp's to carry: nothing is at their
	// paths in the role's own /tmp, neither the mount nor the container's directory over it.
	say("tmp-shadowed", reached(stageShadowed))
	say("tmp-beneath", reached(stageBeneath))
	// What an earlier generation of this role wrote under its home is still the role's own: the
	// writable layer is the role's across its generations, as a role container's filesystem was.
	say("home-carryover", presence(filepath.Join(plan.Home, "planted")))
	// A directory of the role's own, made and then renamed on its home overlay: the live
	// reading of the userxattr option the home layer is mounted with. On gVisor a home overlay
	// mounted without that option does not fail this case, it panics the sandbox and ends the
	// pod, so this case must never be run at a head whose options string lacks the option. The
	// safe negative control of the option is TestARoleHomeLayerIsMountedWithUserxattr, which
	// compares the string alone. The name carries the generation's label: the upper layer is
	// the role's across its generations and every generation of this helper renames here.
	made := filepath.Join(plan.Home, "renamed-"+plan.Label)
	if err := os.MkdirAll(made, 0o755); err != nil {
		say("home-dir-rename", err.Error())
	} else if err := os.Rename(made, made+"-again"); err != nil {
		say("home-dir-rename", err.Error())
	} else {
		say("home-dir-rename", "ok")
	}
	// The role's config home is its own: what workspace-init wrote in the pod's shared one is
	// there, a mount under the shared one comes back over it, and what this role's earlier
	// generations wrote there is still the role's, while a sibling's writes never are. A
	// planted git configuration stands for a `core.fsmonitor` that would run in the sibling's git.
	configHome := filepath.Join(plan.Home, ConfigHomeDir)
	say("config-legion", content(filepath.Join(configHome, "jj", "repos", "id", "config.toml")))
	say("config-nested", content(filepath.Join(configHome, "nested", "marker")))
	say("config-planted", presence(filepath.Join(configHome, "git", "config")))
	say("config-stale", presence(filepath.Join(configHome, "stale")))
	if err := os.MkdirAll(filepath.Join(configHome, "git"), 0o755); err != nil {
		say("config-write", err.Error())
	} else if err := os.WriteFile(filepath.Join(configHome, "git", "config"), []byte("[core]\n\tfsmonitor = /planted-by-"+plan.Label+"\n"), 0o600); err != nil {
		say("config-write", err.Error())
	}
	// The role's own terminals and message queues: none of the container's is there, and what
	// the role opens lands in its own.
	say("pts-entries", ptsEntries())
	if pty, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0); err != nil {
		say("pty", err.Error())
	} else {
		say("pty", "opened")
		say("pts-own", ptsEntries())
		_ = pty.Close()
	}
	say("container-queue", presence("/dev/mqueue/container-queue"))
	switch room, err := makeQueue("role-queue"); {
	case err != nil:
		say("own-queue", err.Error())
	case presence("/dev/mqueue/role-queue") != "present":
		say("own-queue-room", room)
		say("own-queue", "made in the role's IPC namespace, and its /dev/mqueue does not list it: "+presence("/dev/mqueue/role-queue"))
	default:
		say("own-queue-room", room)
		say("own-queue", "made")
	}
	for _, dir := range []string{plan.Home, "/tmp", "/var/tmp", "/dev/shm"} {
		if err := os.WriteFile(filepath.Join(dir, "planted"), []byte("planted"), 0o600); err != nil {
			say("write-"+dir, err.Error())
		}
	}
	// The detached process says it is running by making its marker, which it does once its ignore
	// of SIGTERM is installed: the agent exits the moment it is told that, role-init then reports
	// the shim's status and signals the role's whole namespace, and a detached process still
	// under the default disposition would die of that SIGTERM. The row reads only that nothing of
	// the role is left, so such a run would pass without the SIGKILL this case is about having
	// reached anything. The shell carries the generation's token as its $0, by which the stage
	// finds it, and stays the process that ignores SIGTERM for as long as it runs: its last
	// command is `:`, so the shell runs the sleep as a child of its own rather than exec it, which
	// a shell may do with its last command, leaving no process with the token in its command line.
	// It names its marker by its own pid ($$), so the agent can read, while the role still runs,
	// that the process it started is the one that reported and carries the token
	// (detachedCarrier): a token no live process of the role ever carried is gone from the start,
	// and the stage's `detached=gone` would hold for it. The markers are removed again before the
	// agent leaves, so the role's own /tmp is what it was.
	trapped := "/tmp/detached-trapped"
	detached := exec.Command("/bin/sh", "-c", "trap '' TERM; : >"+trapped+"-$$; /bin/sleep 987654; :", plan.Detached)
	detached.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := detached.Start(); err != nil {
		say("detached-start", err.Error())
	} else {
		var reported []string
		if !testwait.Holds(time.Until(plan.Deadline), 10*time.Millisecond, func() bool {
			reported, _ = filepath.Glob(trapped + "*")
			return len(reported) > 0
		}) {
			say("detached-trap", "the detached process had not reported its ignore of SIGTERM by the stage's deadline, "+plan.Deadline.UTC().Format(time.RFC3339Nano)+", which an earlier wait of the stage's may have spent")
		}
		say("detached-carrier", detachedCarrier(detached.Process.Pid, plan.Detached, trapped, reported))
	}
	markers, _ := filepath.Glob(trapped + "*")
	for _, marker := range markers {
		_ = os.Remove(marker)
	}
	say("done", "yes")
	exitHelper(t)
}

// detachedCarrier is what the agent reads, while the role runs, of the detached process it
// started at pid: `carried` when that process is the one that reported its ignore of SIGTERM
// (reported, the markers the agent found under trapped's name, is trapped-<pid> alone) and its
// command line holds token, which is how the stage finds it once role-init has exited; otherwise
// what it read instead. The reading is of a live process: a process that has ended, a zombie
// included, shows no command line.
func detachedCarrier(pid int, token, trapped string, reported []string) string {
	if want := fmt.Sprintf("%s-%d", trapped, pid); !slices.Equal(reported, []string{want}) {
		return fmt.Sprintf("the ignore of SIGTERM was reported by %q, not by the detached process alone (%s)", reported, want)
	}
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return fmt.Sprintf("pid %d's command line is unreadable: %v", pid, err)
	}
	if !strings.Contains(string(body), token) {
		return fmt.Sprintf("pid %d does not carry the token %s: %s", pid, token, strings.TrimSpace(strings.ReplaceAll(string(body), "\x00", " ")))
	}
	return "carried"
}

// readable is what the role gets when it tries to read path: `refused` where the kernel denies
// it, which is what every probe of its own role-init must answer, and `readable` wherever it
// could open and reach the file at all. It never blocks: a descriptor of role-init's that is a
// pipe would otherwise hold the role here until its own init let go of the other end.
func readable(path string) string {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		var one [1]byte
		_, err = syscall.Read(fd, one[:])
		_ = syscall.Close(fd)
	}
	switch {
	case err == nil, errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EIO):
		return "readable"
	case errors.Is(err, fs.ErrPermission):
		return "refused"
	case errors.Is(err, fs.ErrNotExist):
		return "absent"
	default:
		return "unreadable: " + err.Error()
	}
}

// uncover is what a further namespace of the role's own (cloneflags: a user and mount namespace,
// as an agent's `unshare -Urm` makes) can do to the cover over the roles directory. The nested
// shell tries to unmount it and then looks for the sibling's directory itself, in its own mount
// namespace, where an unmount would have taken effect: `refused` when the cover stayed and the
// sibling is out of reach, `uncovered` when the sibling came back, `unmounted` when the unmount
// succeeded though no sibling showed, and `no-namespace: …` when the nested namespace could not be
// made at all, which proves nothing about the cover.
func uncover(rolesDir, sibling string, cloneflags uintptr) string {
	script := `if umount "$1" 2>/dev/null || umount -l "$1" 2>/dev/null; then unmounted=yes; else unmounted=no; fi
if [ -e "$1/$2" ]; then echo uncovered; elif [ "$unmounted" = yes ]; then echo unmounted; else echo refused; fi`
	nested := exec.Command("/bin/sh", "-c", script, "sh", rolesDir, sibling)
	nested.SysProcAttr = &syscall.SysProcAttr{Cloneflags: cloneflags}
	if cloneflags&syscall.CLONE_NEWUSER != 0 {
		nested.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		nested.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	}
	output, err := nested.Output()
	if err != nil {
		return "no-namespace: " + err.Error()
	}
	return strings.TrimSpace(string(output))
}

// swapLog is what a further namespace of the role's own (a user and a mount namespace, as an
// agent's `unshare -Urm` makes) can do to the test database log bound under fixedRoot. The nested
// shell first remounts its view of the log read-write, which Linux refuses and gVisor allows, so
// remount is reported and asserted nowhere. It then makes a file in fixedRoot itself, on the
// filesystem the log's mount point sits on, so no refusal below is EXDEV, and tries to rename that
// file over the log, to remove the log, and to link that file over it. Each try reads `busy` when
// its error says so (EBUSY, a mount point's refusal), `allowed` when it went through, and its
// error's own text otherwise; swap is the three, `rename=… unlink=… link=…`. after is the log's
// content as that namespace reads it once the tries are done, where a rename or link that went
// through would show the planted file's emptiness instead. The planted file is removed again.
// A nested namespace that could not be made answers `no-namespace: …` for all three.
func swapLog(fixedRoot string) (remount, swap, after string) {
	script := `log=$1 planted=$2
if mount -o remount,bind,rw "$log" 2>/dev/null; then echo allowed; else echo refused; fi
try() {
	if err=$("$@" 2>&1); then
		echo allowed
		return
	fi
	case $err in
	*[Bb][Uu][Ss][Yy]*) echo busy ;;
	*) printf '%s' "$err" | tr '\n' ' ' ;;
	esac
}
: > "$planted"
rename=$(try mv -f "$planted" "$log")
unlink=$(try rm -f "$log")
: > "$planted"
link=$(try ln -f "$planted" "$log")
rm -f "$planted"
echo "rename=$rename unlink=$unlink link=$link"
cat "$log" 2>&1
exit 0`
	nested := exec.Command("/bin/sh", "-c", script, "sh", filepath.Join(fixedRoot, RoleTestPGLog), filepath.Join(fixedRoot, "testpg-swap"))
	nested.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	output, err := nested.Output()
	if err != nil {
		failed := "no-namespace: " + err.Error()
		return failed, failed, failed
	}
	remount, rest, _ := strings.Cut(string(output), "\n")
	swap, after, _ = strings.Cut(rest, "\n")
	return remount, strings.TrimSpace(swap), oneLine(after)
}

// oneLine is a report as one line of the test's output carries it: its lines, each trimmed and
// the empty ones dropped, joined with "; " (workspace.OutputLines), however many lines the file or
// the command it came from wrote.
func oneLine(report string) string {
	return strings.Join(workspace.OutputLines(report), "; ")
}

// fileKind is what Lstat finds at path, never following a link: `regular` for a regular file, the
// file's type otherwise, and the error's text where there is nothing to read.
func fileKind(path string) string {
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return "unreadable: " + err.Error()
	case info.Mode().IsRegular():
		return "regular"
	default:
		return info.Mode().Type().String()
	}
}

// The uncover probe can fail: over a cover a further mount namespace may remove — one made in the
// same user namespace, so the kernel does not lock it, as the reviewer's reproduction had it — it
// answers `uncovered`, read in the namespace where the unmount happened. A probe that read the
// sibling from its caller's namespace, where the cover never left, answered `refused` here.
func TestTheUncoverProbeReportsAnUnlockedCoverRemoved(t *testing.T) {
	t.Parallel()
	if dir := os.Getenv(uncoverControlEnv); dir != "" {
		if err := privateMounts(); err != nil {
			fmt.Println("control error:", err)
			os.Exit(1)
		}
		if err := stageOwnProc(); err != nil {
			fmt.Println("control error:", err)
			os.Exit(1)
		}
		roles := filepath.Join(dir, "roles")
		if err := os.MkdirAll(filepath.Join(roles, "b"), 0o755); err != nil {
			fmt.Println("control error:", err)
			os.Exit(1)
		}
		if err := mountTmpfs(roles, "size=1m"); err != nil {
			fmt.Println("control error:", err)
			os.Exit(1)
		}
		fmt.Printf("control locked=%s\n", uncover(roles, "b", syscall.CLONE_NEWUSER|syscall.CLONE_NEWNS))
		fmt.Printf("control unlocked=%s\n", uncover(roles, "b", syscall.CLONE_NEWNS))
		exitHelper(t)
	}
	requireRoleNamespaces(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	control := exec.Command(self, reentry(t, "TestTheUncoverProbeReportsAnUnlockedCoverRemoved")...)
	control.Env = append(os.Environ(), uncoverControlEnv+"="+t.TempDir())
	control.SysProcAttr = roleStageAttr()
	output, err := control.CombinedOutput()
	if err != nil {
		t.Fatalf("the control: %v\n%s", err, output)
	}
	report := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "control "), "="); ok {
			report[key] = value
		}
	}
	if report["unlocked"] != "uncovered" {
		t.Errorf("the probe over a cover the nested namespace may remove = %q, want uncovered\n%s", report["unlocked"], output)
	}
	if report["locked"] != "refused" {
		t.Errorf("the probe over a cover a nested user namespace holds locked = %q, want refused\n%s", report["locked"], output)
	}
}

// uncoverControlEnv names the directory TestTheUncoverProbeReportsAnUnlockedCoverRemoved's
// re-entered control builds its cover in.
const uncoverControlEnv = "LEGION_ROLE_INIT_TEST_UNCOVER"

// processes is how many processes the role can see.
func processes() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	count := 0
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err == nil {
			count++
		}
	}
	return count
}

func signalOutside(pid int) string {
	if err := syscall.Kill(pid, 0); err != nil {
		return "refused"
	}
	return "delivered"
}

// ptsEntries is how many terminals /dev/pts lists, its ptmx aside.
func ptsEntries() string {
	entries, err := os.ReadDir("/dev/pts")
	if err != nil {
		return "unreadable: " + err.Error()
	}
	count := 0
	for _, entry := range entries {
		if entry.Name() != "ptmx" {
			count++
		}
	}
	return strconv.Itoa(count)
}

// makeQueue makes the POSIX message queue name in the caller's IPC namespace, as mq_open(3)
// does, with room for one message of one byte, the least the kernel allows, and reads that room
// back (mq_getattr) before it answers: a queue made with more room fails here, by name, whatever
// else the machine holds. A queue's bytes, its messages' room and their index, are charged to
// its owner's uid at every level of its user namespaces up to the machine's, against
// RLIMIT_MSGQUEUE (ipc/mqueue.c, mqueue_get_inode), so every queue of one uid on a machine draws
// on one budget: 819,200 bytes by default. A queue made with open(2) on an mqueue filesystem
// takes the namespace's default room, ten messages of 8,192 bytes, about 82 KB, and five of these
// rows at once, on a machine whose agents share one uid, spent that budget: the next queue was
// refused with EMFILE, "too many open files". One of one byte takes about a hundred, so the row
// needs a few hundred bytes of the uid's budget.
//
// It answers the room it read, `1 x 1 B`. gVisor implements no mq_getsetattr (ENOSYS) and
// charges no queue to RLIMIT_MSGQUEUE (pkg/sentry/kernel/mq), so there the room is unread, which
// the answer says, and no queue of the row's can be refused for it.
func makeQueue(name string) (string, error) {
	path, err := syscall.BytePtrFromString(name)
	if err != nil {
		return "", err
	}
	attr := mqAttr{maxmsg: 1, msgsize: 1}
	fd, _, errno := syscall.Syscall6(syscall.SYS_MQ_OPEN, uintptr(unsafe.Pointer(path)), uintptr(syscall.O_RDONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC),
		0o600, uintptr(unsafe.Pointer(&attr)), 0, 0)
	if errno != 0 {
		return "", fmt.Errorf("make the message queue %s: %w", name, errno)
	}
	defer syscall.Close(int(fd))
	var got mqAttr
	_, _, errno = syscall.Syscall(syscall.SYS_MQ_GETSETATTR, fd, 0, uintptr(unsafe.Pointer(&got)))
	switch {
	case errno == syscall.ENOSYS:
		return "unread: this kernel has no mq_getsetattr", nil
	case errno != 0:
		return "", fmt.Errorf("read the room of the message queue %s: %w", name, errno)
	case got.maxmsg != 1 || got.msgsize != 1:
		return "", fmt.Errorf("the message queue %s has room for %d messages of %d B, not 1 of 1 B", name, got.maxmsg, got.msgsize)
	}
	return "1 x 1 B", nil
}

// mqAttr is the kernel's struct mq_attr: four longs, then four reserved, and a C long is a Go int
// on Linux.
type mqAttr struct {
	flags, maxmsg, msgsize, curmsgs int
	_                               [4]int
}

func presence(path string) string {
	switch _, err := os.Lstat(path); {
	case err == nil:
		return "present"
	case errors.Is(err, fs.ErrNotExist):
		return "absent"
	default:
		return "refused"
	}
}

func content(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return strings.TrimSpace(string(body))
}

// appended is what the role gets when it tries to add a line to the file at path: `refused` where
// the open is denied because the file is on a read-only mount (EROFS), `written` when the line
// went in, and the error's own text for anything else. EROFS alone is the bind's refusal: the
// role's agent maps to the file's owner and the file is 0600, so a permission denial would come
// from something other than the bind, and its text is reported rather than read as the bind's.
func appended(path string) string {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	switch {
	case errors.Is(err, syscall.EROFS):
		return "refused"
	case err != nil:
		return err.Error()
	}
	defer file.Close()
	if _, err := file.WriteString("written-by-the-role\n"); err != nil {
		return err.Error()
	}
	return "written"
}

func writable(dir string) string {
	path := filepath.Join(dir, "probe")
	if err := os.WriteFile(path, []byte("probe"), 0o600); err != nil {
		return "refused"
	}
	_ = os.Remove(path)
	return "writable"
}

// roleCheckEnv names the directory TestTheRoleNamespaceCheckRunsARoleGeneration's re-entered
// helper lays the check's layouts out under.
const roleCheckEnv = "LEGION_ROLE_INIT_TEST_CHECK"

// The image probe's role-namespace check runs a whole role generation: it passes where role-init
// sets the namespaces up and its shim exits 0, and refuses, naming the shim's exit, where the shim
// fails. Its layout sits on a memory filesystem of the helper's own outside /tmp, as the probe
// pod's sits on a volume of its own: role-init covers the roles directory after it has given the
// role its own /tmp.
func TestTheRoleNamespaceCheckRunsARoleGeneration(t *testing.T) {
	t.Parallel()
	if dir := os.Getenv(roleCheckEnv); dir != "" {
		err := privateMounts()
		if err == nil {
			err = stageOwnProc()
		}
		if err == nil {
			err = mountTmpfs(dir, "size=16m")
		}
		if err != nil {
			fmt.Println("check error:", err)
			os.Exit(1)
		}
		roleInitLaunch = roleInitHelperLaunch(t, stageSelf)
		answer := func(err error) string {
			if err == nil {
				return "passed"
			}
			return strings.ReplaceAll(err.Error(), "\n", " ")
		}
		fmt.Printf("check clean=%s\n", answer(CheckRoleNamespaces(filepath.Join(dir, "clean"), "/bin/true")))
		fmt.Printf("check failing=%s\n", answer(CheckRoleNamespaces(filepath.Join(dir, "failing"), "/bin/false")))
		exitHelper(t)
	}
	requireRoleNamespaces(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	check := exec.Command(self, reentry(t, "TestTheRoleNamespaceCheckRunsARoleGeneration")...)
	check.Env = append(os.Environ(), roleCheckEnv+"="+stageMountPoint(t))
	check.SysProcAttr = roleStageAttr()
	output, err := check.CombinedOutput()
	if err != nil {
		t.Fatalf("the check's helper: %v\n%s", err, output)
	}
	report := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "check "), "="); ok {
			report[key] = value
		}
	}
	if report["clean"] != "passed" {
		t.Errorf("the check over a shim that exits 0 = %q, want passed\n%s", report["clean"], output)
	}
	if got := report["failing"]; !strings.Contains(got, "did not run its shim to a clean exit") || !strings.Contains(got, "Code:1") {
		t.Errorf("the check over a shim that exits 1 = %q, want a refusal naming exit 1\n%s", got, output)
	}
}

// role-init makes a mount point's place in a role's own layer without following a link the role
// left there: a link where a directory of the path goes, naming another role's directory under the
// roles directory (one that exists, and the sibling's own config home before the sibling made it),
// is replaced by a directory in the role's layer and reported, and nothing appears where it
// pointed; a link where the mount point itself goes is replaced too, for a directory's bind and for
// a file's. A file already at a file's mount point is left as it is. An entry in the way that is
// neither a directory nor a link is refused, and so is a target outside the root.
func TestAMountPointsPlaceFollowsNoLinkARoleLeft(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, elsewhere := filepath.Join(dir, "home"), filepath.Join(dir, "roles", RoleHomesDir, "b")
	for _, path := range []string{root, elsewhere} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range map[string]string{
		"into": elsewhere, "deep": filepath.Join(elsewhere, RoleConfigHomeDir),
		"final-dir": filepath.Join(elsewhere, "dir"), "final-file": filepath.Join(elsewhere, "file"),
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"plain": "plain", "existing": "keep"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var replaced []string
	record := func(link string) { replaced = append(replaced, link) }
	for _, place := range []struct {
		target string
		dir    bool
	}{{"into/sessions", true}, {"deep/sessions", true}, {"final-dir", true}, {"final-file", false}, {"existing", false}} {
		if err := mountPointBeneath(root, filepath.Join(root, place.target), place.dir, record); err != nil {
			t.Errorf("the place of %s: %v", place.target, err)
		}
	}
	if entries, err := os.ReadDir(elsewhere); err != nil || len(entries) != 0 {
		t.Errorf("the sibling's directory holds %v (%v), want nothing: a link was followed", entries, err)
	}
	for path, want := range map[string]fs.FileMode{
		"into": fs.ModeDir, "into/sessions": fs.ModeDir, "deep": fs.ModeDir, "deep/sessions": fs.ModeDir, "final-dir": fs.ModeDir, "final-file": 0,
	} {
		if info, err := os.Lstat(filepath.Join(root, path)); err != nil || info.Mode().Type() != want {
			t.Errorf("%s is %v (%v), want type %v in the role's own layer", path, info, err, want)
		}
	}
	if want := []string{filepath.Join(root, "into"), filepath.Join(root, "deep"), filepath.Join(root, "final-dir"), filepath.Join(root, "final-file")}; strings.Join(replaced, " ") != strings.Join(want, " ") {
		t.Errorf("replaced links %q, want %q", replaced, want)
	}
	if got := content(filepath.Join(root, "existing")); got != "keep" {
		t.Errorf("the file already at a file's mount point reads %q, want it left as it was", got)
	}
	if err := mountPointBeneath(root, filepath.Join(root, "plain", "x"), true, record); err == nil || !strings.Contains(err.Error(), "neither a directory nor a link") {
		t.Errorf("a place through a regular file = %v, want a refusal", err)
	}
	if err := mountPointBeneath(root, filepath.Join(dir, "outside"), true, record); err == nil || !strings.Contains(err.Error(), "is not under") {
		t.Errorf("a place outside the root = %v, want a refusal", err)
	}
}

// A mount point's place is decided from a handle the walk opened and read, so a component that
// changed between the removal of the link that stood there and the making of the place is read
// again rather than assumed: the directory replaced leaves at the component's own name is the
// directory the walk descends, where a walk that took its own Mkdirat's refusal as the answer
// would refuse a place it had just been given. replaced is the only thing that can put an entry
// at that name while role-init runs — nothing of the role runs then, and no other role reaches
// the role's layers — so the callback below stands for that window and for nothing else. The two
// refusals are unchanged behaviour: a new link at the component's name is refused, and so is a
// directory where a file's mount point goes. No walk here, refused or not, leaves a descriptor
// behind.
func TestAMountPointsPlaceRereadsAnEntryThatChangedUnderTheWalk(t *testing.T) {
	// Not parallel: it reads every descriptor of the whole process (descriptorsKept), so a parallel
	// test's open file would read as a descriptor the walk left behind.
	dir := t.TempDir()
	root, elsewhere := filepath.Join(dir, "home"), filepath.Join(dir, "elsewhere")
	for _, path := range []string{root, elsewhere} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, place := range []struct {
		name    string // the component the role left a link at, one per case so the cases do not meet
		plant   string // what replaced puts back at that component's own name: a dir or a link
		target  string // the path under the root the walk is asked to make, relative to it
		dir     bool
		wantErr bool
	}{
		{name: "changed", plant: "dir", target: filepath.Join("changed", "sessions"), dir: true},
		{name: "relinked", plant: "link", target: filepath.Join("relinked", "sessions"), dir: true, wantErr: true},
		{name: "as-file", plant: "dir", target: "as-file", dir: false, wantErr: true},
	} {
		component := filepath.Join(root, place.name)
		if err := os.Symlink(filepath.Join(elsewhere, place.name), component); err != nil {
			t.Fatal(err)
		}
		planted, replaced := false, []string(nil)
		plant := func(link string) {
			replaced = append(replaced, link)
			if planted {
				return
			}
			planted = true
			var err error
			if place.plant == "link" {
				err = os.Symlink(filepath.Join(elsewhere, place.name+"-again"), component)
			} else {
				err = os.Mkdir(component, 0o755)
			}
			if err != nil {
				t.Error(err)
			}
		}
		before := openDescriptors(t)
		err := mountPointBeneath(root, filepath.Join(root, place.target), place.dir, plant)
		descriptorsKept(t, "the place of "+place.target, before, openDescriptors(t))
		switch {
		case place.wantErr && (err == nil || !strings.Contains(err.Error(), component)):
			t.Errorf("the place of %s over the %s replaced left = %v, want a refusal naming %s", place.target, place.plant, err, component)
		case !place.wantErr && err != nil:
			t.Errorf("the place of %s over the %s replaced left = %v, want the place made in the role's own layer", place.target, place.plant, err)
		}
		if want := []string{component}; strings.Join(replaced, " ") != strings.Join(want, " ") {
			t.Errorf("the place of %s replaced %q, want %q: the link it removed", place.target, replaced, want)
		}
	}
	for path, want := range map[string]fs.FileMode{
		"changed": fs.ModeDir, filepath.Join("changed", "sessions"): fs.ModeDir,
	} {
		if info, err := os.Lstat(filepath.Join(root, path)); err != nil || info.Mode().Type() != want {
			t.Errorf("%s is %v (%v), want type %v: the entry replaced left was read again", path, info, err, want)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "plain"), []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := openDescriptors(t)
	err := mountPointBeneath(root, filepath.Join(root, "plain", "x"), true, func(link string) {
		t.Errorf("the place through a regular file replaced %s, want no link in its way", link)
	})
	descriptorsKept(t, "the place through a regular file", before, openDescriptors(t))
	if err == nil || !strings.Contains(err.Error(), filepath.Join(root, "plain")) {
		t.Errorf("the place through a regular file = %v, want a refusal naming it", err)
	}
	if entries, err := os.ReadDir(elsewhere); err != nil || len(entries) != 0 {
		t.Errorf("what the links named holds %v (%v), want nothing: a link was followed", entries, err)
	}
}

// openDescriptors is every descriptor this process holds open and what each names, read the
// moment it is called.
func openDescriptors(t *testing.T) map[int]string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	open := map[int]string{}
	for _, entry := range entries {
		number, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			continue // the listing's own descriptor, closed before this read
		}
		open[number] = target
	}
	return open
}

// descriptorsKept reports what the call between the two snapshots left open, as a set and never
// as a count: a descriptor open after it, on a target that was not open on that number before
// it, is one the call opened and did not close. It reads the whole process, so it holds only
// while nothing else opens a descriptor in the window: its caller must run beside no other test,
// which requireRunsAlone checks first, and every goroutine a test starts ends with that test.
// That is why it checks this direction alone: a descriptor vanishing between the snapshots is no
// finding, because the finaliser of a file an earlier test left unreferenced closes it at whatever
// garbage collection lands in the window (measured, with the two snapshots around a runtime.GC()),
// and a finaliser closes where it never opens.
func descriptorsKept(t *testing.T, label string, before, after map[int]string) {
	t.Helper()
	requireRunsAlone(t, "descriptorsKept")
	for number, target := range after {
		if held, ok := before[number]; !ok || held != target {
			t.Errorf("%s left descriptor %d open on %s, want every handle it opened closed", label, number, target)
		}
	}
}

// A role's own config home is a finished copy only once it holds ownConfigHome's marker: a
// directory at its path without the marker — a copy a generation stopped midway, or an empty
// directory something else made there before the role's first start — is removed and copied
// again from the pod's shared one, and so is a link at its path, which is removed without touching
// what it named. A finished copy is the role's own from then on: what the role wrote there stays.
func TestAnOwnConfigHomeWithoutItsMarkerIsMadeAgain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	shared, own, elsewhere := filepath.Join(dir, "shared"), filepath.Join(dir, "own"), filepath.Join(dir, "elsewhere")
	for path, body := range map[string]string{
		filepath.Join(shared, "jj", "repos", "id", "config.toml"): "legion-jj",
		filepath.Join(own, "sessions", "stale"):                   "stale",
		filepath.Join(elsewhere, configHomeMarker):                "",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := ownConfigHome(shared, own, nil); err != nil || got != own {
		t.Fatalf("ownConfigHome over an unfinished copy = %q, %v", got, err)
	}
	if got := content(filepath.Join(own, "jj", "repos", "id", "config.toml")); got != "legion-jj" {
		t.Errorf("the role's config home reads %q at workspace-init's jj configuration, want legion-jj: an unfinished copy was kept", got)
	}
	if got := presence(filepath.Join(own, "sessions")); got != "absent" {
		t.Errorf("what stood at the role's config home without the marker is %s, want absent", got)
	}
	if err := os.WriteFile(filepath.Join(own, "role-wrote"), []byte("role"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ownConfigHome(shared, own, nil); err != nil {
		t.Fatal(err)
	}
	if got := content(filepath.Join(own, "role-wrote")); got != "role" {
		t.Errorf("what the role wrote in its finished config home reads %q, want it kept", got)
	}
	linked := filepath.Join(dir, "linked")
	if err := os.Symlink(elsewhere, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := ownConfigHome(shared, linked, nil); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(linked); err != nil || !info.IsDir() {
		t.Errorf("the config home made over a link is %v (%v), want a directory of its own", info, err)
	}
	if got := presence(filepath.Join(elsewhere, configHomeMarker)); got != "present" {
		t.Errorf("what the link named lost its file (%s): the link was followed", got)
	}
}

// The role's own layer over its home is mounted with userxattr, over the home as its lower layer
// and the role's own upper and work directories. Without that option gVisor marks a renamed
// directory opaque by writing trusted.overlay.opaque, which a task in the role's nested user
// namespace may not write, and gVisor panics when that write is refused, which ends the pod's
// whole sandbox. The safe negative control is this case alone: take ,userxattr out of the string
// homeOverlayOptions returns and the case fails, naming the options the launch would mount with.
// Never prove the option by running TestARoleRunsInNamespacesOfItsOwn against a head without it:
// under gVisor that ends the pod's sandbox rather than failing a test.
func TestARoleHomeLayerIsMountedWithUserxattr(t *testing.T) {
	t.Parallel()
	homes := filepath.Join("/roles", RoleHomesDir, "a")
	home, upper, work := Home, filepath.Join(homes, "upper"), filepath.Join(homes, "work", "0000000001")
	options := homeOverlayOptions(home, upper, work)
	named := strings.Split(options, ",")
	for _, want := range []string{"lowerdir=" + home, "upperdir=" + upper, "workdir=" + work, "userxattr"} {
		if !slices.Contains(named, want) {
			t.Errorf("the role's home layer would be mounted with %q, which holds no %s", options, want)
		}
	}
}

// A refused home layer names the options the mount was tried with, userxattr among them, so the
// operator who reads the failure is told what the launch asked the kernel for rather than that
// the mount failed. The home here is a regular file and the upper layer does not exist, only the
// work directory beside it: the file carries no mount, so the call reaches the mount itself,
// which is refused for every caller on every runtime because there is no upper layer to mount.
// Nothing is mounted and no directory is renamed. The case reads the refusal's text alone, and
// fails when the error is nil.
func TestARefusedRoleHomeLayerNamesTheOptionsItTried(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	homes, home := filepath.Join(dir, "homes"), filepath.Join(dir, "home")
	upper, work := filepath.Join(homes, "upper"), filepath.Join(homes, "work", "0000000001")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home, []byte("no caller mounts over a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := RoleInitConfig{
		RoleCover: RoleCover{
			RolesDir: filepath.Join(dir, "roles"), Home: home,
			HomeMounts: filepath.Join(dir, "home-mounts"), FixedRoot: filepath.Join(dir, "fixed"),
		},
		Role: "a", Generation: 1,
	}
	err := privateHome(cfg, home, homes, work)
	if err == nil {
		t.Fatalf("the role's home layer over the regular file %s went on, want the mount refused", home)
	}
	for _, want := range []string{"lowerdir=" + home, "upperdir=" + upper, "workdir=" + work, "userxattr"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal reads %q, which does not name %s the mount was tried with", err, want)
		}
	}
}

// TestTheStatusPipeReadsTheWidthItCarries pins the reading end of role-init's status pipe, which
// is parseStatus and nothing else: every line reportStatus can write — it writes the 32 bits of a
// syscall.WaitStatus as a decimal, so from zero to the largest of them — reads back as itself,
// and a line holding anything but such a number is no status at all rather than one truncated
// into a role exit that never happened. Whitespace around the number is ignored, which costs
// nothing and asks no caller to trim what it read: both readers take the pipe whole with
// io.ReadAll (roleinit.go:151, launcher.go:634), and the writer sends no newline of its own.
// What the writer prints is not checked here: reportStatus lives in roleinit_linux.go, runs
// inside the re-exec stage and writes to an inherited descriptor, so a test of it is a test of
// that stage rather than of this function.
func TestTheStatusPipeReadsTheWidthItCarries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  string
		want uint32
	}{
		{"an exit of zero", "0", 0},
		{"an exit code", "1792", 1792},
		{"a kill by signal", "9", 9},
		{"the widest status there is", strconv.FormatUint(math.MaxUint32, 10), math.MaxUint32},
		{"whitespace around the number", "1792\n", 1792},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := parseStatus([]byte(tc.raw))
			if status == nil {
				t.Fatalf("parseStatus(%q) read no status, want %d", tc.raw, tc.want)
			}
			if uint32(*status) != tc.want {
				t.Errorf("parseStatus(%q) = %d, want %d: the pipe carries the status as it was written",
					tc.raw, uint32(*status), tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"nothing at all", ""},
		{"role-init was killed before it wrote", "   "},
		{"a line that is not a number", "killed"},
		{"a negative number no writer sends", "-1"},
		{"a number wider than a wait status", strconv.FormatUint(math.MaxUint32+1, 10)},
		{"a number with something after it", "1792 9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status := parseStatus([]byte(tc.raw)); status != nil {
				t.Errorf("parseStatus(%q) = %d, want no status: a line role-init never wrote reports no exit",
					tc.raw, uint32(*status))
			}
		})
	}
}

// roleDrainEnv carries the plan of the re-entered stage of
// TestARoleWhoseShimExitsSignalsItsNamespaceBeforeItKills, and roleDrainFile is the file the
// role's descendant removes there when it catches SIGTERM, in the role's own /tmp.
const (
	roleDrainEnv  = "LEGION_ROLE_INIT_TEST_DRAIN"
	roleDrainFile = "descendant"
)

// roleDrainPlan is what that stage builds and runs: a role layout under Dir, which it covers with
// a memory filesystem of its own, and a shim that leaves a descendant behind which catches
// SIGTERM and removes roleDrainFile. Leaves says whether that descendant then exits or goes on
// running, which is the whole difference between the row's two cases. Bound is the bound
// role-init's wait takes in place of roleNamespaceDrain (roleDrainBoundEnv).
type roleDrainPlan struct {
	Dir    string
	Leaves bool
	Bound  time.Duration
}

// roleDrainStaysBound is the bound role-init's wait takes in the case whose descendant stays,
// which waits it out: thirty seconds, far past any stall of the descendant's trap, which must run
// before the kill for the case to read anything — a trap 2.25 s late is killed inside
// roleNamespaceDrain's two seconds — and short enough that the case costs half a minute. The case
// whose descendant leaves takes a bound no wait of it reaches, since its wait ends when the
// namespace empties; both are held under the test binary's deadline less a margin.
const roleDrainStaysBound = 30 * time.Second

// A role whose shim exits on its own has its namespace signalled before it is killed: role-init
// sends SIGTERM over the role's whole namespace once it has reported the shim's status and before
// the kill(-1, SIGKILL) that ends the namespace, so a git or a jj the role left running finishes
// and removes its lock rather than dying mid-write. The shim of each case here exits while a
// descendant that catches SIGTERM is still running, and the case reads the file that descendant
// removes when it is signalled: the role's namespace is gone by the time the stage looks, so a
// file that is gone was removed before role-init exited, and one still there is a descendant that
// was killed with no signal before it.
//
// The cases differ in what the descendant does once it has removed the file, and each reads how
// role-init's wait loop ended from the line role-init logs as it leaves the loop (runRoleShim),
// never from a clock of the stage's. One descendant goes on running, so nothing but the SIGKILL
// ends it: the loop must end at its bound, with role-init's own clock past that bound. The other
// leaves, and the loop must end with the namespace emptied, which is what tells it from a wait
// that sleeps its bound whatever the role does. Each case sets the bound itself (roleDrainPlan),
// far past any stall of the descendant's trap, so the trap runs before the kill however loaded the
// machine; roleNamespaceDrain's own two seconds are not this row's subject. The row reads how the
// loop ended and not when role-init exits: a role-init that slept after its loop and before its
// kill would pass it. The verdict is that order alone, so a loaded machine that slows role-init's
// polls or the descendant's trap changes no case's verdict; the stage's one deadline is a hang
// guard (roleDrainReport).
func TestARoleWhoseShimExitsSignalsItsNamespaceBeforeItKills(t *testing.T) {
	t.Parallel()
	if encoded := os.Getenv(roleDrainEnv); encoded != "" {
		var plan roleDrainPlan
		if err := json.Unmarshal([]byte(encoded), &plan); err != nil {
			fmt.Println("drain error:", err)
			os.Exit(2)
		}
		if err := runRoleDrain(t, plan); err != nil {
			fmt.Println("drain error:", err)
			os.Exit(1)
		}
		exitHelper(t)
	}
	requireRoleNamespaces(t)
	mount := stageMountPoint(t)
	// Each bound is whole milliseconds, the precision of the span role-init logs, so a span
	// rounded to them never reads under a bound the wait did reach.
	for _, tc := range []struct {
		name   string
		leaves bool
		bound  time.Duration
	}{
		{"a descendant that stays is signalled before it is killed", false, testwait.Bounded(t, roleDrainStaysBound, time.Minute).Truncate(time.Millisecond)},
		{"a descendant that leaves ends the wait with the namespace", true, testwait.Bounded(t, 2*time.Minute, time.Minute).Truncate(time.Millisecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, output := roleDrainReport(t, roleDrainPlan{Dir: mount, Leaves: tc.leaves, Bound: tc.bound})
			if report["status"] != "0" {
				t.Fatalf("the role's status = %q, want the shim's own exit 0\n%s", report["status"], output)
			}
			if report["descendant"] != "absent" {
				t.Errorf("the file the role's descendant removes on SIGTERM is %q, want it gone: the descendant was killed with no signal before it\n%s",
					report["descendant"], output)
			}
			ended, span := roleDrainEnd(output)
			switch {
			case tc.leaves && ended != roleDrainEmptied:
				t.Errorf("role-init's wait ended with %q, want %q: the wait does not end when the role's processes leave\n%s",
					ended, roleDrainEmptied, output)
			case !tc.leaves && ended != roleDrainBounded:
				t.Errorf("role-init's wait ended with %q, want %q: a descendant that never leaves is not waited for until the bound\n%s",
					ended, roleDrainBounded, output)
			case !tc.leaves && span < tc.bound:
				t.Errorf("role-init's wait ended at its bound %s after its SIGTERM by its own clock, want at least the bound the case set (%s): a descendant that never leaves is killed before the wait is over\n%s",
					span, tc.bound, output)
			}
		})
	}
}

// roleDrainEnd is how role-init's wait over the role's namespace ended, read from the line it
// logs then (roleDrainEmptied or roleDrainBounded, the empty string for no such line), and the
// span of the wait that line gives, by role-init's own clock.
func roleDrainEnd(output string) (string, time.Duration) {
	for _, line := range strings.Split(output, "\n") {
		for _, ended := range []string{roleDrainEmptied, roleDrainBounded} {
			_, rest, ok := strings.Cut(line, "role-init: "+ended+" ")
			if !ok {
				continue
			}
			spelled, _, _ := strings.Cut(rest, " ")
			span, err := time.ParseDuration(spelled)
			if err != nil {
				return ended + " (an unreadable span " + strconv.Quote(spelled) + ")", 0
			}
			return ended, span
		}
	}
	return "", 0
}

// roleDrainReport runs that row's stage in user, mount and PID namespaces of its own
// (roleStageAttr), as the role namespace check's helper runs, and answers every `drain
// <key>=<value>` line it printed, with its whole output for a failure to print. It waits for the
// stage to end with no bound of its own (reentry): the stage's own -test.timeout ends it, and its
// role-init's ends role-init before that, so a stage or a role-init that never ends fails here with
// its own alarm and goroutine dump, rather than as the binary's panic, and nothing of it outlives
// the test: the stage is the init of its PID namespace, so its end ends every process it started.
func roleDrainReport(t *testing.T, plan roleDrainPlan) (map[string]string, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	stage := exec.Command(self, reentry(t, "TestARoleWhoseShimExitsSignalsItsNamespaceBeforeItKills")...)
	// Once the stage has exited, its output is read for a second more at most: a process of the
	// role's that still held the pipe would otherwise hold this wait.
	stage.WaitDelay = time.Second
	stage.Env = append(os.Environ(), roleDrainEnv+"="+string(encoded))
	stage.SysProcAttr = roleStageAttr()
	output, err := stage.CombinedOutput()
	if err != nil {
		t.Fatalf("the stage: %v\n%s", err, output)
	}
	report := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "drain ")
		if !ok {
			continue
		}
		if key, value, ok := strings.Cut(rest, "="); ok {
			report[key] = value
		}
	}
	return report, string(output)
}

// runRoleDrain is that stage: it builds a role layout of its own on a memory filesystem under the
// plan's directory, as CheckRoleNamespaces lays one out, runs one generation over it whose shim
// leaves a descendant behind and exits, and reports the shim's status and whether the file the
// descendant removes on SIGTERM is still in the role's own /tmp once the role's namespace is
// gone. role-init's own lines, the one that says how its wait ended among them, reach the
// stage's output through its stderr.
func runRoleDrain(t *testing.T, plan roleDrainPlan) error {
	if err := privateMounts(); err != nil {
		return err
	}
	if err := stageOwnProc(); err != nil {
		return err
	}
	if err := mountTmpfs(plan.Dir, "size=16m"); err != nil {
		return err
	}
	roleInitLaunch = roleInitHelperLaunch(t, stageSelf)
	const role = "drain"
	caught := "rm -f /tmp/" + roleDrainFile + "; while :; do /bin/sleep 600 & wait; done"
	if plan.Leaves {
		caught = "rm -f /tmp/" + roleDrainFile + "; exit 0"
	}
	cfg := RoleInitConfig{
		RolesDir: filepath.Join(plan.Dir, "roles"), Role: role, Generation: 1,
		Home: filepath.Join(plan.Dir, "home"), HomeMounts: filepath.Join(plan.Dir, "home-mounts"),
		FixedRoot: filepath.Join(plan.Dir, "fixed"), Argv: []string{"/bin/sh", "-c", roleDrainShim(caught)},
	}
	dirs := []string{cfg.Home, cfg.HomeMounts}
	for _, name := range []string{RoleGHDir, RolePrivateDir, RoleStateDir} {
		dirs = append(dirs, filepath.Join(cfg.RolesDir, role, name), filepath.Join(cfg.FixedRoot, name))
	}
	for _, path := range dirs {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	process, err := newRoleProcess(cfg, append(os.Environ(), roleDrainBoundEnv+"="+plan.Bound.String()), os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if err := process.start(); err != nil {
		return err
	}
	raw, _ := io.ReadAll(process.status)
	_ = process.status.Close()
	if err := process.cmd.Wait(); err != nil {
		return fmt.Errorf("role-init: %w", err)
	}
	if status := parseStatus(raw); status == nil {
		fmt.Println("drain status=none")
	} else {
		fmt.Printf("drain status=%d\n", status.ExitStatus())
	}
	fmt.Printf("drain descendant=%s\n", presence(filepath.Join(cfg.RolesDir, RoleHomesDir, role, "tmp", roleDrainFile)))
	return nil
}

// roleDrainShim is the shim that stage runs: it starts a descendant that traps SIGTERM with
// caught, waits until that descendant is up, and exits on its own while it is still running, as a
// worker shim that leaves a git or a jj behind does. The descendant installs its trap before it
// writes the file it removes when it is signalled, so the shim's exit says the trap is in place,
// and a file that is gone was removed rather than never written. The shim waits for that file for
// as long as the descendant lives, with no count of its own, and a descendant gone without
// writing it is the shim's own exit 1 rather than a case that reads as a pass; a descendant that
// neither writes it nor ends is the stage's hang guard's (roleDrainReport).
//
// The descendant holds its life in one sleep, started before its trap so the trap can name it,
// and spends that life waiting on that one sleep: it forks nothing after the signal but its own
// cleanup — caught begins with the `rm -f` that removes the file, a process that runs after the
// SIGTERM and ends on its own — and its trap ends that sleep before caught runs. A descendant
// that waited in a loop of sleeps would fork its next sleep the moment role-init's SIGTERM killed
// the one it was waiting for, before the shell ran the pending trap, and a trap that then left
// would leave that new sleep behind with no signal of its own — a process of the row's own
// making, which role-init rightly waits the whole of its bound for and then kills, so
// the case that reads the namespace emptied would read the bound instead and fail for what the
// rig left rather than for what role-init did. The caught of the case whose descendant stays
// does go on forking sleeps after the signal, which is that case's whole point: a descendant that
// never leaves is what keeps the role's namespace up until the bound.
func roleDrainShim(caught string) string {
	return fmt.Sprintf(`/bin/sh -c "/bin/sleep 600 &
sleeper=\$!
trap 'kill \$sleeper 2>/dev/null; %s' TERM
printf alive > /tmp/%s
wait \$sleeper" &
descendant=$!
while [ ! -f /tmp/%s ]; do
	kill -0 "$descendant" 2>/dev/null || exit 1
	/bin/sleep 0.01
done
exit 0
`, caught, roleDrainFile, roleDrainFile)
}
