package sandbox

//ci:builds packages/daemon ./cmd/legion buildLegion go build .../daemon/cmd/legion

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/envoy/testexec"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// Every agent of an issue can write its pod's disk: its pod's own generic ephemeral volume, or,
// for a microVM pod under keep_volume, the claim that its Sandbox owns (keptVolume). These rigs run
// an issue pod's workspace-init exactly as the manifest states it, with the mounted provisioning
// and cache credentials, and model each volume's lifetime. An ephemeral disk dies with its pod; a
// kept volume reaches the issue's next pod (TestAKeptVolumeCarriesItsWorkspaceToTheIssuesNextPod).

// canaryToken is the provisioning Secret's value in every pod the rig runs.
const canaryToken = "ghs_canary_the_tree_must_never_read"

// boundaryRepo is the repository every pod provisions (testSpec's).
const boundaryRepo = "sjawhar/legion-smoke"

// initRig runs pods' init containers, reaching "github.com" through the cluster's egress proxy: a
// stand-in that serves boundaryRepo over git's smart HTTP to a request that authenticates as
// x-access-token with canaryToken.
type initRig struct {
	t *testing.T
	r *Runtime
	// root holds everything the rig makes; pods holds each pod's volumes and container filesystems
	// (rigPod), and tree is the directory a claim a Sandbox owns maps to, shared by every pod that
	// mounts it and outliving them all.
	root, pods, tree string
	// bin holds isolated wrappers around the host git and jj for agent commands.
	bin    string
	legion string
	github *gitHubStandIn
	// cache is the repository cache of boundaryRepo that workspace-init fills the clone from.
	cache *httptest.Server
	// network is what every process reaching a remote is told: the egress proxy, the trust store
	// holding the stand-in's CA, and the hosts reached directly.
	network []string
	// cpus is what an agent's own command is told in place of the issue pod's CPU limit (podCPUs).
	cpus []string
	pod  int
}

// rigPod is a pod the rig has created (create) and not yet deleted (delete). Its disk, the volume
// mounted as issueVolume, is the one volume the pod's agents write: a directory of the pod's own
// under dir when the manifest makes it a generic ephemeral volume, as it does an issue pod's
// (issuePod.disk), which the pod keeps across a restart and loses when it is deleted; the
// rig's tree when the manifest makes it a claim a Sandbox owns, as it does a kept volume's
// (keptVolume).
type rigPod struct {
	spec corev1.PodSpec
	dir  string
	disk string
	// inherited is what the disk held when the pod was created, before its first init container
	// ran: nothing, for a pod whose disk is its own.
	inherited []string
}

func newInitRig(t *testing.T, legion string) *initRig {
	t.Helper()
	return newInitRigOf(t, legion, goldenOptions())
}

// newMicroVMInitRig is newInitRig for a project whose sandbox block is of type kind, names
// canaries in its canary list, and keeps an issue pod's volume where keep is true. An issue pod
// of the project that runs in a microVM (SandboxConfig.Of) then mounts the claim that its Sandbox
// owns (keptVolume), and the rig maps that claim to its tree. The microvm block names no metadata
// endpoint (testMicroVM's does): the endpoint changes only the launcher's container, which the rig
// does not run.
func newMicroVMInitRig(t *testing.T, legion string, kind SandboxType, keep bool, canaries ...string) *initRig {
	t.Helper()
	opts := goldenOptions()
	block := testMicroVM()
	block.Metadata, block.KeepVolume = nil, keep
	opts.Sandbox = SandboxConfig{Type: kind, CanaryIssues: canaries, MicroVM: block}
	return newInitRigOf(t, legion, opts)
}

// newInitRigOf is the rig of a runtime that opts configure.
func newInitRigOf(t *testing.T, legion string, opts Options) *initRig {
	t.Helper()
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	rig := &initRig{
		t: t, r: r, root: root, pods: filepath.Join(root, "pods"), tree: filepath.Join(root, "tree"),
		bin: filepath.Join(root, "bin"), legion: legion,
	}
	for _, dir := range []string{rig.pods, rig.tree, rig.bin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rig.github = newGitHubStandIn(t, root)
	rig.cache = newCacheStandIn(t, rig.github.remote)
	rig.network = []string{
		"HTTPS_PROXY=" + rig.github.proxy.URL, "https_proxy=" + rig.github.proxy.URL,
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost", "GIT_SSL_CAINFO=" + rig.github.caFile,
	}
	rig.cpus = podCPUs(opts.Resources.Issue.Limits[corev1.ResourceCPU])
	for _, name := range []string{"git", "jj"} {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("the rig drives a real %s: %v", name, err)
		}
		script := "#!/bin/sh\nexec " + shellprefix.Literal(real) + ` "$@"` + "\n"
		if err := testexec.WriteFile(filepath.Join(rig.bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return rig
}

// clone and workspace are where provisioning puts the issue's clone and an issue's workspace on the
// pod's disk.
func (pod *rigPod) clone() string {
	return filepath.Join(pod.disk, "repos", "github.com", "sjawhar", "legion-smoke")
}

func (pod *rigPod) workspace(issue string) string {
	return filepath.Join(pod.disk, "workspaces", "sjawhar", "legion-smoke", strings.ToLower(issue))
}

// launch is a pod of spec's claim, as the runtime would create it: its gh bundles minted, so each
// role's gh volume projects its owners' files.
func (rig *initRig) launch(spec runtime.SpawnSpec) corev1.PodSpec {
	rig.t.Helper()
	l := launchOf(rig.t, rig.r, spec)
	return rig.r.podTemplate(l).Spec
}

// start creates a pod of spec (create) and runs its init containers (initialize). The rig starts an
// issue's next pod only once it has deleted the issue's previous one, as the runtime does.
func (rig *initRig) start(spec corev1.PodSpec) (*rigPod, error) {
	rig.t.Helper()
	pod, err := rig.create(spec)
	if err != nil {
		return pod, err
	}
	return pod, rig.initialize(pod)
}

// create is a pod of spec as the kubelet holds it before its first init container runs: its disk
// is there (volumeDir), and inherited records what that disk already holds. A test reads the disk
// a pod starts on between create and initialize.
func (rig *initRig) create(spec corev1.PodSpec) (*rigPod, error) {
	rig.t.Helper()
	rig.pod++
	pod := &rigPod{spec: spec, dir: filepath.Join(rig.pods, fmt.Sprintf("pod-%d", rig.pod))}
	at := slices.IndexFunc(spec.Volumes, func(volume corev1.Volume) bool { return volume.Name == issueVolume })
	if at < 0 {
		return pod, fmt.Errorf("the pod has no %s volume", issueVolume)
	}
	pod.disk = rig.volumeDir(pod, spec.Volumes[at])
	if err := os.MkdirAll(pod.disk, 0o700); err != nil {
		return pod, err
	}
	entries, err := os.ReadDir(pod.disk)
	if err != nil {
		return pod, err
	}
	for _, entry := range entries {
		pod.inherited = append(pod.inherited, entry.Name())
	}
	return pod, nil
}

// volumeDir is the directory that is volume of pod: the rig's tree for a claim a Sandbox owns, and
// a directory of the pod's own under dir for every other volume.
func (rig *initRig) volumeDir(pod *rigPod, volume corev1.Volume) string {
	if volume.PersistentVolumeClaim != nil {
		return rig.tree
	}
	return filepath.Join(pod.dir, "volumes", volume.Name)
}

// restart runs the pod's init containers again on what the pod keeps, as the kubelet does when it
// recreates a running pod's sandbox: the pod's disk survives that with whatever its agents wrote on
// it, and every other volume is made afresh. The disk is the pod's own ephemeral volume, whose
// claim the Pod owns, or a kept volume. On a disk of the pod's own, a restart is the one way
// provisioning meets a disk an agent of the issue has written — a retained clone and a workspace
// that exists — so a test of provisioning against a planted disk opts into it by name. On a kept
// volume, the issue's next pod meets that disk too.
func (rig *initRig) restart(pod *rigPod) error {
	rig.t.Helper()
	return rig.initialize(pod)
}

// delete removes the pod, and with it every volume of its own, its disk included when the disk is
// its own; a claim a Sandbox owns, the tree, stays.
func (rig *initRig) delete(pod *rigPod) {
	rig.t.Helper()
	if err := os.RemoveAll(pod.dir); err != nil {
		rig.t.Fatal(err)
	}
}

// initialize executes the pod's init containers in order, as the kubelet does: each volume a
// directory — a claim a Sandbox owns the rig's tree, every pod's that mounts it and outliving them
// all; the pod's generic ephemeral volume a directory of the pod's own, which it keeps until it is
// deleted; every other volume a directory made for this run of the init containers and removed
// when they end, since no agent mounts one — and each container a process of the Go legion built
// from this commit, with the manifest's argv, environment, and working directory, every pod path
// in them moved to the directory of the volume mounted there.
// While a container runs, every volume it does not mount, and every other container's filesystem,
// is unreadable; a read-only mount is not writable. The first container that fails ends the run.
func (rig *initRig) initialize(pod *rigPod) error {
	rig.t.Helper()
	// The rig models the Secret routes the manifest uses; any other would carry the token past it.
	for _, volume := range pod.spec.Volumes {
		if volume.Projected != nil && slices.ContainsFunc(volume.Projected.Sources, func(source corev1.VolumeProjection) bool { return source.Secret != nil }) {
			rig.t.Fatalf("volume %s projects a Secret, which the rig does not model", volume.Name)
		}
		if volume.Secret != nil && len(volume.Secret.Items) == 0 {
			rig.t.Fatalf("volume %s mounts every key of Secret %s, which the rig does not model", volume.Name, volume.Secret.SecretName)
		}
	}
	for _, c := range slices.Concat(pod.spec.InitContainers, pod.spec.Containers) {
		for _, v := range c.Env {
			if v.ValueFrom != nil && v.ValueFrom.SecretKeyRef != nil {
				rig.t.Fatalf("%s reads %s from a Secret, which the rig does not model", c.Name, v.Name)
			}
		}
		if slices.ContainsFunc(c.EnvFrom, func(from corev1.EnvFromSource) bool { return from.SecretRef != nil }) {
			rig.t.Fatalf("%s takes its environment from a Secret, which the rig does not model", c.Name)
		}
	}
	scratch := []string{filepath.Join(pod.dir, "containers")}
	defer func() {
		for _, dir := range scratch {
			_ = os.RemoveAll(dir)
		}
	}()
	volumes := map[string]string{}
	for _, volume := range pod.spec.Volumes {
		dir := rig.volumeDir(pod, volume)
		switch {
		case volume.PersistentVolumeClaim != nil:
			// A claim a Sandbox owns outlives every pod, and every pod that mounts it shares it.
		case volume.Ephemeral != nil:
			// A generic ephemeral volume is the pod's own: the kubelet creates it with the pod and
			// deletes it with the pod (INFRA-80), so no other pod, the issue's next included, sees
			// it, and a restart of the pod finds it as the pod left it.
		default:
			scratch = append(scratch, dir)
		}
		if dir != rig.tree {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		if volume.Secret != nil {
			for _, item := range volume.Secret.Items {
				value := "not-the-provisioning-token"
				switch item.Key {
				case provisionTokenKey:
					value = canaryToken
				case cacheTokenKey:
					value = boundaryCacheToken
				}
				path := filepath.Join(dir, item.Path)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					return err
				}
				if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
					return err
				}
			}
		}
		volumes[volume.Name] = dir
	}
	for _, c := range pod.spec.InitContainers {
		if err := rig.runContainer(pod.dir, volumes, c); err != nil {
			return err
		}
	}
	return nil
}

func (rig *initRig) runContainer(podDir string, volumes map[string]string, c corev1.Container) error {
	local := filepath.Join(podDir, "containers", c.Name)
	home, tmp := filepath.Join(local, "home"), filepath.Join(local, "tmp")
	for _, dir := range []string{home, tmp} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	type move struct{ pod, host string }
	moves := []move{{podHome, home}}
	mounted := map[string]bool{local: true}
	var readOnly []string
	for _, mount := range c.VolumeMounts {
		host := filepath.Join(volumes[mount.Name], mount.SubPath)
		// The kubelet makes a subPath that does not exist yet before it mounts it.
		if mount.SubPath != "" {
			if err := os.MkdirAll(host, 0o700); err != nil {
				return err
			}
		}
		moves = append(moves, move{mount.MountPath, host})
		mounted[volumes[mount.Name]] = true
		if mount.ReadOnly {
			readOnly = append(readOnly, host)
		}
	}
	if !slices.ContainsFunc(moves, func(m move) bool { return m.pod == "/tmp" }) {
		moves = append(moves, move{"/tmp", tmp})
	}
	slices.SortFunc(moves, func(a, b move) int { return len(b.pod) - len(a.pod) })
	remap := func(value string) string {
		for _, m := range moves {
			if value == m.pod || strings.HasPrefix(value, m.pod+"/") {
				return m.host + value[len(m.pod):]
			}
		}
		return value
	}

	declared := map[string]string{}
	for _, v := range c.Env {
		declared[v.Name] = v.Value
	}
	env := []string{}
	for _, v := range c.Env {
		value := remap(kubeExpand(v.Value, declared))
		// The cache's address names its Service in the cluster; here it is the rig's stand-in.
		if v.Name == "LEGION_REPOSITORY_CACHE_URL" {
			value = rig.cache.URL
		}
		env = append(env, v.Name+"="+value)
	}
	if _, set := declared["TMPDIR"]; !set {
		env = append(env, "TMPDIR="+tmp)
	}
	env = append(env, "PATH="+rig.bin+":/usr/bin:/bin", "HOME="+home)
	env = append(env, rig.network...)
	env = append(env, podCPUs(c.Resources.Limits[corev1.ResourceCPU])...)
	argv := make([]string, len(c.Command))
	for i, arg := range c.Command {
		argv[i] = remap(kubeExpand(arg, declared))
	}
	if argv[0] != rig.r.tools.Legion {
		return fmt.Errorf("init container %s runs %s, not the Go legion", c.Name, argv[0])
	}
	argv[0] = rig.legion

	var hidden []string
	candidates := []string{rig.tree}
	for _, parent := range []string{filepath.Join(podDir, "volumes"), filepath.Join(podDir, "containers")} {
		entries, err := os.ReadDir(parent)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			candidates = append(candidates, filepath.Join(parent, entry.Name()))
		}
	}
	for _, dir := range candidates {
		if !mounted[dir] {
			if err := os.Chmod(dir, 0); err != nil {
				return err
			}
			hidden = append(hidden, dir)
		}
	}
	for _, dir := range readOnly {
		if err := chmodTree(dir, false); err != nil {
			return err
		}
	}
	defer func() {
		for _, dir := range hidden {
			_ = os.Chmod(dir, 0o700)
		}
		for _, dir := range readOnly {
			_ = chmodTree(dir, true)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = env
	command.Dir = remap(c.WorkingDir)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("init container %s: %w\n%s", c.Name, err, output)
	}
	return nil
}

// podCPUs is what the rig tells a process in place of the CPU limit, limit, of the container it
// would run in. In a pod that limit is the cgroup quota the Go runtime sizes GOMAXPROCS to, the
// host's CPUs or max(ceil(limit), 2) where that is fewer (runtime/cgroup_linux.go), and jj its
// thread pool to. The rig runs every process under no quota, where both size themselves to the
// host: on a 288-CPU host each legion started about 80 threads and each jj about 290, with the
// vectors' pods side by side.
func podCPUs(limit resource.Quantity) []string {
	cpus, host := (limit.MilliValue()+999)/1000, int64(goruntime.NumCPU())
	return []string{"GOMAXPROCS=" + strconv.FormatInt(min(host, max(cpus, 2)), 10), "RAYON_NUM_THREADS=" + strconv.FormatInt(min(host, max(cpus, 1)), 10)}
}

// chmodTree adds or removes every owner write permission under dir.
func chmodTree(dir string, writable bool) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() &^ 0o222
		if writable {
			mode |= 0o200
		}
		return os.Chmod(path, mode)
	})
}

// agent runs a tree agent's own commands in the rig's network, with its isolated git and jj first
// on PATH.
func (rig *initRig) agent() fixturegit.Runner {
	return fixturegit.Runner{TB: rig.t, Env: slices.Concat([]string{
		"PATH=" + rig.bin + ":" + os.Getenv("PATH"), "HOME=" + filepath.Join(rig.root, "home"),
		"XDG_CONFIG_HOME=" + filepath.Join(rig.root, "config"), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
	}, rig.network, rig.cpus)}
}

// fileHolds is nil when the file at path holds exactly want, and otherwise names the mismatch.
func fileHolds(path, want string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(body) != want {
		return fmt.Errorf("%s holds %q, not %q", path, body, want)
	}
	return nil
}

// buildLegion is the Go legion built from this commit, which runs every init container of the
// rig.
func buildLegion(t *testing.T) string {
	t.Helper()
	legion := filepath.Join(t.TempDir(), "legion")
	if output, err := exec.Command("go", "build", "-o", legion, "github.com/sjawhar/legion/daemon/cmd/legion").CombinedOutput(); err != nil {
		t.Fatalf("build the Go legion: %v\n%s", err, output)
	}
	return legion
}

// mountsClaim fails the test unless pod's disk is the claim name.
func mountsClaim(t *testing.T, pod *rigPod, name string) {
	t.Helper()
	if source := podDiskSource(t, pod.spec); source.PersistentVolumeClaim == nil || source.PersistentVolumeClaim.ClaimName != name {
		t.Fatalf("the pod's disk is %+v, want the claim %s of the issue's kept volume", source, name)
	}
}

// A pod with an ephemeral issue volume loses its agent's files when Kubernetes deletes that pod. The
// next pod renders the same workspace-init contract but begins on a new volume.
func TestAnEphemeralIssueVolumeDoesNotOutliveItsPod(t *testing.T) {
	t.Parallel()
	rig := newInitRig(t, buildLegion(t))
	first, err := rig.start(rig.launch(rootSpec(t)))
	if err != nil {
		t.Fatalf("the issue's first pod: %v", err)
	}
	plant := filepath.Join(first.disk, "plant")
	if err := os.WriteFile(plant, []byte("agent output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.delete(first)
	next, err := rig.start(rig.launch(workerSpec(t)))
	if err != nil {
		t.Fatalf("the issue's next pod: %v", err)
	}
	if len(next.inherited) != 0 {
		t.Errorf("the issue's next pod started on a disk holding %v, which its first pod wrote: an issue pod's disk is its own", next.inherited)
	}
	if _, err := os.Stat(filepath.Join(next.disk, "plant")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the issue's next pod has the first pod's plant: %v", err)
	}
}

// A kept volume carries the issue's workspace to the issue's next pod (keptVolume). The first pod
// provisions the workspace. An agent of the issue then leaves on the pod's disk a commit that no
// remote holds, a file that .gitignore names, a file under the volume's scratch directory and a
// file under its go directory. The next pod mounts the same claim, and its workspace-init exits 0
// on that disk. Its workspace holds the commit in its history, and its disk holds each file as the
// agent left it. The workspace's creation record is still the first pod's (workspace.Created), so
// an agent that resumes there is told that its workspace was not recreated. The issue of a
// microvm project keeps its volume, and so does a canary of a gvisor project. The same microvm
// project without keep_volume gives each pod a disk of its own, and its next pod finds nothing that
// the first pod's agent left.
func TestAKeptVolumeCarriesItsWorkspaceToTheIssuesNextPod(t *testing.T) {
	t.Parallel()
	legion := buildLegion(t)
	for _, tc := range []struct {
		name     string
		kind     SandboxType
		keep     bool
		canaries []string
	}{
		{name: "a microVM issue of a microvm project", kind: SandboxMicroVM, keep: true},
		{name: "a canary of a gvisor project", kind: SandboxGVisor, keep: true, canaries: []string{testTree}},
		{name: "a microVM issue of a microvm project that keeps no volume", kind: SandboxMicroVM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newMicroVMInitRig(t, legion, tc.kind, tc.keep, tc.canaries...)
			first, err := rig.start(rig.launch(rootSpec(t)))
			if err != nil {
				t.Fatalf("the issue's first pod: %v", err)
			}
			if tc.keep {
				mountsClaim(t, first, IssueClaimName(rootToken))
			} else if source := podDiskSource(t, first.spec); source.Ephemeral == nil {
				t.Fatalf("the first pod's disk is %+v, want an ephemeral volume of its own", source)
			}
			created, ok, err := workspace.Created(first.workspace(testTree))
			if err != nil || !ok {
				t.Fatalf("the first pod's workspace holds no creation record (%t, %v)", ok, err)
			}

			// What an agent of the issue leaves on the first pod's disk: a commit that no remote
			// holds, which adds work.txt and a .gitignore, then a file that the .gitignore names,
			// and a file under each of the volume's scratch and go directories.
			left := []struct {
				what string
				path func(pod *rigPod) string
				body string
			}{
				{"the commit's own file", func(pod *rigPod) string { return filepath.Join(pod.workspace(testTree), "work.txt") }, "work that no remote holds\n"},
				{"the file that .gitignore names", func(pod *rigPod) string { return filepath.Join(pod.workspace(testTree), "out", "build.log") }, "ignored output\n"},
				{"the file under scratch", func(pod *rigPod) string { return filepath.Join(pod.disk, "scratch", "notes.md") }, "scratch notes\n"},
				{"the file under go", func(pod *rigPod) string {
					return filepath.Join(pod.disk, "go", "pkg", "mod", "cache", "download", "kept.txt")
				}, "a module cache entry\n"},
			}
			agent, jj, ws := rig.agent(), filepath.Join(rig.bin, "jj"), first.workspace(testTree)
			writeFile(t, filepath.Join(ws, ".gitignore"), "/out/\n")
			writeFile(t, left[0].path(first), left[0].body)
			agent.Run(ws, jj, "commit", "-m", "Work that no remote holds")
			commit := strings.TrimSpace(agent.Out(ws, jj, "log", "--no-graph", "-r", "@-", "-T", "commit_id"))
			for _, file := range left[1:] {
				writeFile(t, file.path(first), file.body)
			}

			rig.delete(first)
			next, err := rig.start(rig.launch(workerSpec(t)))
			if err != nil {
				t.Fatalf("the issue's next pod: %v", err)
			}
			ws = next.workspace(testTree)
			held := strings.TrimSpace(agent.Out(ws, jj, "log", "--no-graph", "-r", "present("+commit+") & ::@", "-T", "commit_id"))
			recreated, ok, err := workspace.Created(ws)
			if err != nil || !ok {
				t.Fatalf("the next pod's workspace holds no creation record (%t, %v)", ok, err)
			}
			if !tc.keep {
				if source := podDiskSource(t, next.spec); source.Ephemeral == nil {
					t.Fatalf("the next pod's disk is %+v, want an ephemeral volume of its own", source)
				}
				if held != "" {
					t.Errorf("the next pod's workspace holds in its history the commit %s that the first pod's agent made", commit)
				}
				for _, file := range left {
					if _, err := os.Lstat(file.path(next)); !errors.Is(err, fs.ErrNotExist) {
						t.Errorf("the next pod's disk holds %s at %s, which the first pod's agent wrote (%v)", file.what, file.path(next), err)
					}
				}
				if recreated.Equal(created) {
					t.Errorf("the next pod's workspace records the first pod's creation, %v, on a disk of its own", created)
				}
				return
			}
			mountsClaim(t, next, IssueClaimName(rootToken))
			if held != commit {
				t.Errorf("the next pod's workspace holds %q in its history, want the commit %s that no remote holds", held, commit)
			}
			for _, file := range left {
				if err := fileHolds(file.path(next), file.body); err != nil {
					t.Errorf("the next pod's disk does not hold %s as the first pod's agent left it: %v", file.what, err)
				}
			}
			if !recreated.Equal(created) {
				t.Errorf("the next pod's workspace records its creation at %v, want the first pod's %v", recreated, created)
			}
		})
	}
}

// The rig stands in for each container's CPU limit, the cgroup quota a pod's processes size their
// threads to, by giving each process it starts GOMAXPROCS and RAYON_NUM_THREADS from that limit
// (podCPUs). This row fails by name when the two do not reach a process: each init container of an
// issue pod, which runContainer starts with its own container's limit, and an agent's own command,
// which agent() starts with the issue pod's limit. Without the row a lost cap shows only on a host
// whose CPU count carries the rig's processes past a task limit, and no CI runner has that many
// CPUs. Each child prints the two variables as it has them. The rig builds each child's environment
// from nothing; the test process's own values are emptied here all the same, so that a child that
// took them would not pass for one that has the cap.
func TestEveryProcessTheRigStartsHasItsContainersCPULimit(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	t.Setenv("GOMAXPROCS", "")
	t.Setenv("RAYON_NUM_THREADS", "")
	const report = `printf 'GOMAXPROCS=%s RAYON_NUM_THREADS=%s' "${GOMAXPROCS-unset}" "${RAYON_NUM_THREADS-unset}"`
	// The stand-in for the Go legion appends one line for each init container that runs it.
	record := filepath.Join(t.TempDir(), "record")
	standIn := filepath.Join(t.TempDir(), "legion")
	if err := testexec.WriteFile(standIn, []byte("#!/bin/sh\n{ "+report+"; echo; } >> "+shellprefix.Literal(record)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rig := newInitRig(t, standIn)
	pod, err := rig.start(rig.launch(rootSpec(t)))
	if err != nil {
		t.Fatalf("the issue's pod: %v", err)
	}
	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("no init container ran the stand-in: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(pod.spec.InitContainers) == 0 || len(lines) != len(pod.spec.InitContainers) {
		t.Fatalf("%d lines from the stand-in, want one for each of the pod's %d init containers: %q", len(lines), len(pod.spec.InitContainers), lines)
	}
	for i, c := range pod.spec.InitContainers {
		limit := c.Resources.Limits[corev1.ResourceCPU]
		if want := strings.Join(podCPUs(limit), " "); lines[i] != want {
			t.Errorf("init container %s ran with %q, want %q from its CPU limit %s", c.Name, lines[i], want, limit.String())
		}
	}
	issue := goldenOptions().Resources.Issue.Limits[corev1.ResourceCPU]
	if got, want := rig.agent().Out(rig.root, "/bin/sh", "-c", report), strings.Join(podCPUs(issue), " "); got != want {
		t.Errorf("an agent's own command ran with %q, want %q from the issue pod's CPU limit %s", got, want, issue.String())
	}
}

// writeFile writes body to path, with every directory above it.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitHubStandIn is github.com as a pod reaches it: through the cluster's egress proxy, which
// tunnels CONNECT github.com:443 to a TLS server whose certificate the stand-in's CA signed. It
// serves boundaryRepo over git's smart HTTP to a request authenticated as x-access-token with
// canaryToken, and challenges every other request.
type gitHubStandIn struct {
	proxy  *httptest.Server
	caFile string
	// remote is the bare repository the stand-in serves as boundaryRepo.
	remote string
	mu     sync.Mutex
	count  int
}

func newGitHubStandIn(t *testing.T, root string) *gitHubStandIn {
	t.Helper()
	projects := filepath.Join(root, "github")
	remote := filepath.Join(projects, boundaryRepo)
	seed := filepath.Join(root, "seed")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		fixturegit.InitBare(remote, "main")[1:],
		fixturegit.InitWorkTree(seed, "main")[1:],
		{"-C", seed, "commit", "--quiet", "--allow-empty", "-m", "seed"},
		{"-C", seed, "push", "--quiet", remote, "HEAD:main"},
	} {
		fixturegit.Isolated(t, t.TempDir()).Run(root, append([]string{git}, argv...)...)
	}
	ca, caKey, caPEM := newCertificateAuthority(t)
	caFile := filepath.Join(root, "github-ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	g := &gitHubStandIn{caFile: caFile, remote: remote}
	backend := &cgi.Handler{
		Path: git, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + projects, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"},
	}
	g.proxy = tlsTerminatingProxy(t, leafCertificate(t, ca, caKey), func(w http.ResponseWriter, r *http.Request) {
		if user, password, ok := r.BasicAuth(); !ok || user != "x-access-token" || password != canaryToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		g.mu.Lock()
		g.count++
		g.mu.Unlock()
		backend.ServeHTTP(w, r)
	}, nil)
	return g
}

// boundaryCacheToken is the bearer token the rig's Secret projections hold for the repository
// cache, and the one the stand-in cache takes.
const boundaryCacheToken = "the-repository-cache-token"

// newCacheStandIn is the repository cache of boundaryRepo as workspace-init reaches it at the
// address the runtime names: its /healthz answers ready with defaultBranchOnly, its /tar only the bearer boundaryCacheToken,
// it names sjawhar/legion-smoke on every answer it gives, is ready at once, and streams as its
// /tar the refs and packs (workspace.CacheEntry) of a bare clone of remote's default branch.
func newCacheStandIn(t *testing.T, remote string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Header().Set(workspace.CacheRepositoryHeader, "sjawhar/legion-smoke")
			_, _ = w.Write([]byte(`{"ready":true,"defaultBranchOnly":true,"repo":"sjawhar/legion-smoke","bytes":1}`))
		case "/tar":
			if r.Header.Get("Authorization") != "Bearer "+boundaryCacheToken {
				http.Error(w, "the repository cache's token is required", http.StatusUnauthorized)
				return
			}
			body, err := cacheTar(remote)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set(workspace.CacheRepositoryHeader, "sjawhar/legion-smoke")
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// cacheTar is a tar stream of the refs and packs of a bare clone of remote's default branch, as
// the repository cache streams its own.
func cacheTar(remote string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "boundary-cache-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	clone := filepath.Join(dir, "clone.git")
	command := exec.Command("git", "clone", "--bare", "--single-branch", "--no-tags", "--quiet", "--no-local", remote, clone)
	command.Dir = dir
	command.Env = fixtureenv.Isolated(dir)
	if output, err := command.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("clone %s: %w\n%s", remote, err, output)
	}
	var stream bytes.Buffer
	archive := tar.NewWriter(&stream)
	err = filepath.WalkDir(clone, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == clone {
			return err
		}
		relative, err := filepath.Rel(clone, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if !workspace.CacheEntry(name, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		if entry.IsDir() {
			header.Name += "/"
			return archive.WriteHeader(header)
		}
		body, err := os.ReadFile(path)
		if err == nil {
			err = archive.WriteHeader(header)
		}
		if err == nil {
			_, err = archive.Write(body)
		}
		return err
	})
	if err == nil {
		err = archive.Close()
	}
	return stream.Bytes(), err
}

// authorized is how many requests presented the provisioning token.
func (g *gitHubStandIn) authorized() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.count
}

// tlsTerminatingProxy is an HTTP proxy that answers every CONNECT itself: it terminates TLS with
// certificate and serves handler on the tunnel. onConnect, when set, runs for each CONNECT.
func tlsTerminatingProxy(t *testing.T, certificate tls.Certificate, handler http.HandlerFunc, onConnect func()) *httptest.Server {
	t.Helper()
	tunnels := &tunnelListener{conns: make(chan net.Conn), done: make(chan struct{})}
	inner := &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = inner.Serve(tunnels) }()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		if onConnect != nil {
			onConnect()
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			_ = conn.Close()
			return
		}
		select {
		case tunnels.conns <- tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}}):
		case <-tunnels.done:
			_ = conn.Close()
		}
	}))
	t.Cleanup(func() {
		proxy.Close()
		_ = inner.Close()
	})
	return proxy
}

// tunnelListener hands the proxy's tunnels to an http.Server as accepted connections.
type tunnelListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *tunnelListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *tunnelListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *tunnelListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func newCertificateAuthority(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Legion test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return ca, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// leafCertificate is a server certificate for github.com that ca signed.
func leafCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "github.com"}, DNSNames: []string{"github.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
