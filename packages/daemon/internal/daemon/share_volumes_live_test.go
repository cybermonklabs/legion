// The share volumes live test, which the kind job of ci.yaml runs on the rig of internal/kindtest,
// against the rig's NFS server (NFS-Ganesha in the cluster, exporting /team, seeded with one file,
// which a PersistentVolume and the claim team-share bind, and /scratch). Each case's legion.yaml is
// read by the daemon's own loader, and the `shares` case's Agent Sandbox runtime, built by the
// daemon's own sandboxOptions, launches one issue's root on the cluster through Agent Sandbox's
// controller and the gvisor runtime class (share_volumes_kind_test.go and burstable_kind_test.go
// build both). That root, as its agent sees the pod, reads the claim's seed, is refused a write
// there, and writes a file to the export the server then holds. `legion probe-boot`'s probe
// (ProbeBoot) mounts both shares on the worker image under test, and refuses a claim that does not
// exist, an export the server does not have, a server that never answers and an image digest the
// registry does not have, each naming what it could not mount or pull. What the test stands in for
// lies outside the volume path, as the Burstable test's does: the agent is a sleep stub, the store
// answers as the daemon's does for an issue's first launch, the repository is a public one of the
// rig's fake GitHub, the shared public leak check is the rig's stand-in, and the roles' gh files
// hold a placeholder token.
package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/kindtest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// shareLiveProbeBound is the time a probe refusal must come within: one attempt of a probe pod
// under the probe's default budget of 300 s. A refusal later than that came from a second attempt.
const shareLiveProbeBound = 300 * time.Second

// TestShareVolumesMountInAnIssuePodLive writes every case's legion.yaml, holds each to the pod
// block the loader must read from it, launches the `shares` case's issue pod, and holds that pod,
// as the API server recorded it, to the block: the claim and the export as volumes, each mounted in
// the roles container, the claim read-only and the export writable, under gVisor. Then the root's
// agent reads and writes the shares, and the image probe mounts the good shares and refuses each
// broken one.
func TestShareVolumesMountInAnIssuePodLive(t *testing.T) {
	fixtureenv.Live(t)
	rig := kindtest.Start(t)
	sessions := rig.StartPostgres(t)
	rig.GitHub.NewRepository(t, burstableLiveRepository.Owner(), burstableLiveRepository.Name(), true)
	leakCheck := kindtest.LeakCheck(t)
	seed := fmt.Sprintf("seeded by %s at %s", t.Name(), time.Now().UTC().Format(time.RFC3339))
	server := rig.StartNFSServer(t, seed)
	rig.Apply(t, fmt.Sprintf(`apiVersion: v1
kind: PersistentVolume
metadata: {name: team-share-pv}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteMany]
  nfs: {server: %s, path: /team}
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  claimRef: {namespace: legion, name: %s}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: %s, namespace: legion}
spec: {accessModes: [ReadWriteMany], storageClassName: "", volumeName: team-share-pv, resources: {requests: {storage: 1Gi}}}
`, server, shareLiveClaim, shareLiveClaim))
	rig.Kubectl(t, "-n", "legion", "wait", "--for=jsonpath={.status.phase}=Bound", "pvc/"+shareLiveClaim, "--timeout=60s")
	// Registered after the rig's teardown, so it runs first: on a pass every pod that mounts the
	// share goes while the server still runs, so the node's kernel client unmounts it cleanly and
	// the cluster's deletion waits on no hard NFS mount. A failed run keeps them for the rig's
	// record of the cluster.
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		rig.Kubectl(t, "-n", "legion", "delete", "sandboxes", "--all", "--wait=true", "--timeout=180s")
		rig.Kubectl(t, "-n", "legion", "delete", "pods", "--all", "--wait=true", "--timeout=180s")
	})
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	want := map[string]config.PodConfig{
		"shares":         shareLivePod(shareLiveClaim, server, "/scratch"),
		"missing-claim":  shareLivePod("absent-share", server, "/scratch"),
		"missing-export": shareLivePod(shareLiveClaim, server, "/absent"),
		"silent-server":  shareLivePod(shareLiveClaim, shareLiveSilentServer, "/scratch"),
	}
	work := t.TempDir()
	// probed is each case's file on the worker image under test, unmodified, which the image
	// probe runs: the checks of the image itself run on the image as it is published.
	probed := map[string]config.Config{}
	for _, tc := range shareLiveCases(server) {
		kindLiveProviders(t, rig, tc.project, sessions.InCluster)
		path := writeBurstableLiveConfig(t, filepath.Join(work, tc.name), tc.project, rig.Gateway, rig.WorkerImage,
			rig.Kubeconfig, rig.Context, tc.pod)
		cfg, err := config.LoadForValidation(path, nil)
		if err != nil {
			t.Fatalf("%s: load %s: %v", tc.name, path, err)
		}
		if got := cfg.Runtime.Kubernetes.Pod; !reflect.DeepEqual(got, want[tc.name]) {
			t.Fatalf("%s: the loader read the pod block as %+v, want %+v", tc.name, got, want[tc.name])
		}
		fmt.Printf("== %s: %s loads %d volumes, %d mounts\n", tc.name, path, len(cfg.Runtime.Kubernetes.Pod.Volumes), len(cfg.Runtime.Kubernetes.Pod.VolumeMounts))
		probed[tc.name] = cfg
	}

	// The launch runs on the image every other pod of a kind test starts, which trusts the fake
	// GitHub its workspace is cloned from.
	shares := shareLiveCases(server)[0]
	path := writeBurstableLiveConfig(t, filepath.Join(work, "shares-launch"), shares.project, rig.Gateway, rig.Image,
		rig.Kubeconfig, rig.Context, shares.pod)
	cfg, err := config.LoadForValidation(path, nil)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	started := time.Now()
	pod := launchBurstableLiveRoot(ctx, t, cfg, leakCheck, log)
	fmt.Printf("== launch: pod %s/%s: runtimeClassName %s, node %s, PodScheduled %s\n", pod.Namespace, pod.Name,
		liveString(pod.Spec.RuntimeClassName), pod.Spec.NodeName, liveCondition(pod, corev1.PodScheduled))
	if liveString(pod.Spec.RuntimeClassName) != "gvisor" {
		t.Errorf("pod %s runs under %q, want gvisor", pod.Name, liveString(pod.Spec.RuntimeClassName))
	}
	for _, volume := range want["shares"].Volumes {
		var got []corev1.Volume
		for _, v := range pod.Spec.Volumes {
			if v.Name == volume.Name {
				got = append(got, v)
			}
		}
		if len(got) != 1 || !reflect.DeepEqual(got[0].VolumeSource, volume.VolumeSource) {
			t.Errorf("pod %s carries volume %s as %+v, want exactly %+v", pod.Name, volume.Name, got, volume.VolumeSource)
		}
	}
	for _, c := range pod.Spec.Containers {
		for _, mount := range want["shares"].VolumeMounts {
			found := false
			for _, m := range c.VolumeMounts {
				found = found || reflect.DeepEqual(m, mount)
			}
			if !found {
				t.Errorf("container %s mounts %+v, want %+v among them", c.Name, c.VolumeMounts, mount)
			}
		}
		fmt.Printf("   container %s mounts team at /mnt/team (read-only) and scratch at /mnt/scratch (writable)\n", c.Name)
	}
	for _, c := range pod.Spec.InitContainers {
		for _, m := range c.VolumeMounts {
			if m.Name == "team" || m.Name == "scratch" {
				t.Errorf("init container %s mounts the operator's %s", c.Name, m.Name)
			}
		}
	}
	fmt.Printf("ok launch in %s\n", time.Since(started).Round(time.Second))
	if t.Failed() {
		return
	}

	shareLivePodShares(t, rig, pod.Name, seed)
	shareLiveProbes(ctx, t, rig, probed, server)
}

// shareLivePodShares holds the launched root's view of the shares to the pod block: as the
// architect's agent sees the pod (role_enter_sh of scripts/e2e/lib/role-namespaces.sh: inside its
// role-init's namespaces, as its shim's user), under gVisor's kernel, it reads the seed from the
// claim at /mnt/team, is refused a write there (its mount is read-only by default), and writes a
// file to the export at /mnt/scratch, which the NFS server then holds.
func shareLivePodShares(t *testing.T, rig *kindtest.Rig, pod, seed string) {
	t.Helper()
	started := time.Now()
	fmt.Printf("== pod-shares\n")
	enter, err := exec.Command("bash", "-c", `. "$1" && printf %s "$role_enter_sh"`, "role-enter",
		filepath.Join(kindtest.RepositoryRoot(t), "scripts", "e2e", "lib", "role-namespaces.sh")).Output()
	if err != nil || len(enter) == 0 {
		t.Fatalf("read role_enter_sh from scripts/e2e/lib/role-namespaces.sh: %v", err)
	}
	inPod := func(command ...string) (string, error) {
		args := append([]string{"--kubeconfig", rig.Kubeconfig, "--context", rig.Context, "-n", "legion", "exec", pod, "-c", runtime.IssuePodContainer, "--"}, command...)
		var stdout, stderr bytes.Buffer
		cmd := exec.Command("kubectl", args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return strings.TrimSpace(stdout.String() + stderr.String()), err
	}
	asArchitect := func(command ...string) (string, error) {
		return inPod(append([]string{"sh", "-c", string(enter), "role-enter", "architect"}, command...)...)
	}
	// The shim starts the role before the launch returns; its namespaces are entered once its
	// role-init runs.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		entered, err := asArchitect("sh", "-c", `echo "uid $(id -u), pid 1 $(tr "\0" " " </proc/1/cmdline | cut -c1-80)"`)
		if err == nil {
			fmt.Printf("   role architect: %s\n", entered)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cannot enter the architect role's namespaces in pod %s: %v: %s", pod, err, entered)
		}
		time.Sleep(2 * time.Second)
	}
	kernel, err := inPod("sh", "-c", "dmesg 2>/dev/null | head -1")
	if err != nil || !strings.Contains(kernel, "gVisor") {
		t.Fatalf("the roles container's kernel is not gVisor's: %v: %s", err, kernel)
	}
	fmt.Printf("   container %s: %s\n", runtime.IssuePodContainer, kernel)
	mounts, _ := asArchitect("sh", "-c", `grep -E " /mnt/(team|scratch) " /proc/mounts`)
	fmt.Printf("   role architect mounts: %s\n", strings.ReplaceAll(mounts, "\n", "; "))
	read, err := asArchitect("cat", "/mnt/team/seed.txt")
	if err != nil || read != seed {
		t.Fatalf("the architect read %q from /mnt/team/seed.txt (%v), not the seed %q", read, err, seed)
	}
	fmt.Printf("   cat /mnt/team/seed.txt → %s\n", read)
	refused, err := asArchitect("sh", "-c", "echo no >/mnt/team/written-by-pod")
	if err == nil {
		t.Fatal("the architect wrote to /mnt/team, which the pod block mounts read-only")
	}
	if !strings.Contains(refused, "Read-only file system") {
		t.Fatalf("the write to /mnt/team failed for another reason: %s", refused)
	}
	fmt.Printf("   echo no >/mnt/team/written-by-pod → refused: %s\n", refused)
	token := fmt.Sprintf("written by %s at %s", pod, time.Now().UTC().Format(time.RFC3339))
	written, err := asArchitect("sh", "-c", `printf '%s\n' "$1" >/mnt/scratch/from-issue-pod && ls -ln /mnt/scratch/from-issue-pod`, "write", token)
	if err != nil {
		t.Fatalf("the architect cannot write /mnt/scratch/from-issue-pod: %v: %s", err, written)
	}
	fmt.Printf("   printf … >/mnt/scratch/from-issue-pod → %s\n", written)
	if held := strings.TrimSpace(rig.Kubectl(t, "-n", "nfs", "exec", "ganesha", "--", "cat", "/export/scratch/from-issue-pod")); held != token {
		t.Fatalf("the NFS server holds %q in /export/scratch/from-issue-pod, not what the pod wrote", held)
	}
	fmt.Printf("ok pod-shares (%ds)\n", int(time.Since(started).Seconds()))
}

// shareLiveProbes runs the image probe of `legion probe-boot` (ProbeBoot) on each case's file,
// on the worker image under test, and holds each to its refusal: the shares file's probe pod
// mounts both shares and runs the image's probe container, whose verdict is the image's own (the
// test's agents have no model route, so the image refuses on its agents' models, and that refusal,
// never a `cannot mount`, is the evidence that the probe mounted every share); each broken file is
// refused naming the volume and its share, the silent server's and the missing image's within one
// attempt; and the shares file with its image's digest swapped for one the registry does not hold
// is refused once the kubelet met that answer, naming the image, rather than retried.
func shareLiveProbes(ctx context.Context, t *testing.T, rig *kindtest.Rig, probed map[string]config.Config, server string) {
	t.Helper()
	missing := probed["shares"]
	kubernetes := *missing.Runtime.Kubernetes
	repository, _, _ := strings.Cut(kubernetes.Image, "@sha256:")
	kubernetes.Image = repository + "@sha256:" + strings.Repeat("0", 64)
	missing.Runtime.Kubernetes = &kubernetes
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		want    []string
		refuse  []string
		bounded bool
	}{
		{"probe-mounts-shares", probed["shares"], []string{"worker image ", "failed its probe: pod ", " Failed (container probe terminated"},
			[]string{"cannot mount", "over its mounts"}, false},
		{"probe-missing-claim", probed["missing-claim"], []string{`cannot mount volume "team", which runtime.kubernetes.pod.volumes gives it from PersistentVolumeClaim "absent-share"`,
			`persistentvolumeclaim "absent-share" not found`}, nil, false},
		{"probe-missing-export", probed["missing-export"], []string{`cannot mount volume "scratch", which runtime.kubernetes.pod.volumes gives it from NFS export ` + server + `:/absent`,
			"mount failed: "}, nil, false},
		// 192.0.2.10 is RFC 5737's documentation range: no host answers, so the node's mount.nfs
		// times out after its own two minutes of retries, and the probe refuses that first failure.
		{"probe-silent-server", probed["silent-server"], []string{`cannot mount volume "scratch", which runtime.kubernetes.pod.volumes gives it from NFS export ` + shareLiveSilentServer + `:/scratch`,
			"mount failed: "}, nil, true},
		{"probe-missing-image", missing, []string{"worker image sha256:" + strings.Repeat("0", 64) + " failed its probe: the registry does not have image " + kubernetes.Image,
			"rpc error: code = NotFound desc = failed to pull and unpack image ", ": not found"}, []string{"failed transiently"}, true},
	} {
		started := time.Now()
		fmt.Printf("== %s\n", tc.name)
		var logged bytes.Buffer
		_, err := ProbeBoot(ctx, tc.cfg, os.LookupEnv, slog.New(slog.NewJSONHandler(&logged, nil)))
		took := time.Since(started)
		if err == nil {
			t.Fatalf("%s: the probe passed, want its refusal; its log:\n%s", tc.name, logged.String())
		}
		said := err.Error() + "\n" + logged.String()
		for _, want := range tc.want {
			if !strings.Contains(said, want) {
				t.Errorf("%s: the probe's refusal does not say %q: %v", tc.name, want, err)
			}
		}
		for _, refused := range tc.refuse {
			if strings.Contains(said, refused) {
				t.Errorf("%s: the probe's refusal says %q, which it must not: %v", tc.name, refused, err)
			}
		}
		if tc.bounded && took >= shareLiveProbeBound {
			t.Errorf("%s: refused only after %s, past one %s attempt", tc.name, took.Round(time.Second), shareLiveProbeBound)
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("%s: the test's own deadline ended the probe", tc.name)
		}
		line, _, _ := strings.Cut(err.Error(), "\n")
		fmt.Printf("   refused in %s: %s\n", took.Round(time.Second), line)
		fmt.Printf("ok %s (%ds)\n", tc.name, int(took.Seconds()))
	}
}
