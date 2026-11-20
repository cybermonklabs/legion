package kindtest_test

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/kindtest"
)

// The rig comes up on the runner, and an issue pod's shape runs on it: a Sandbox, which Agent
// Sandbox's controller turns into a pod on the Legion pool under gVisor, whose requests are below
// its limits (Burstable), on the image built FROM the worker image under test, clones a private
// repository from the fake GitHub as https://github.com with a token the fake takes, trusting the
// fake's certificate through the image's own trust store. A second pod under gVisor mounts an NFS
// share of a server in the cluster, reads the file the server was seeded with and writes one the
// server then holds. The test logs, by name, how long each piece took, and `df -h` of the runner
// before the rig and with it up: the measurements the kind tests that come after are sized from.
func TestABurstableGVisorPodOnTheKindRigClonesFromTheFakeGitHub(t *testing.T) {
	t.Logf("df -h before the rig:\n%s", kindtest.DiskFree(t))
	rig := kindtest.Start(t)
	// Registered after Start's teardown, so it runs before it, with the rig still up.
	t.Cleanup(func() {
		t.Logf("df -h with the rig up:\n%s", kindtest.DiskFree(t))
		t.Logf("df -h of the node %s:\n%s", rig.Node, rig.NodeDiskFree(t))
		rig.Report(t)
	})

	repo := rig.GitHub.NewRepository(t, "acme", "widgets", false)
	const token = "ghs_kind_rig"
	rig.GitHub.Grant(token, false)
	main := revParse(t, repo.Bare, "refs/heads/main")

	t.Run("clone", func(t *testing.T) {
		// The kubelet expands $(NAME) of a container's env in its command; the script names no
		// variable that way, and reads TOKEN as the shell's own $TOKEN.
		script := strings.Join([]string{
			"set -eu",
			`echo "kernel: $(dmesg 2>/dev/null | head -1)"`,
			"start=$(date +%s%N)",
			`git -c credential.helper= -c 'credential.helper=!f() { echo username=x-access-token; echo "password=$TOKEN"; }; f' clone --quiet ` +
				repo.URL() + " /tmp/widgets",
			"end=$(date +%s%N)",
			`echo "cloned $(git -C /tmp/widgets rev-parse HEAD) in $(( (end - start) / 1000000 )) ms"`,
		}, "\n")
		rig.Measure(t, "clone", func() {
			rig.Apply(t, fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata: {name: rig-clone-token, namespace: legion}
stringData: {token: %q}
---
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata: {name: rig-clone, namespace: legion, labels: {legion.dev/rig: clone}}
spec:
  operatingMode: Running
  podTemplate:
    metadata: {labels: {legion.dev/rig: clone}}
    spec:
      restartPolicy: Never
      runtimeClassName: gvisor
      nodeSelector: {legion.dev/pool: legion}
      automountServiceAccountToken: false
      enableServiceLinks: false
      securityContext: {runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000}
      containers:
      - name: clone
        image: %s
        command: [/bin/sh, -c, %q]
        env:
        - {name: HOME, value: /tmp}
        - {name: TOKEN, valueFrom: {secretKeyRef: {name: rig-clone-token, key: token}}}
        resources:
          requests: {cpu: 250m, memory: 256Mi, ephemeral-storage: 256Mi}
          limits: {cpu: "1", memory: 1Gi, ephemeral-storage: 2Gi}
        securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
`, token, rig.Image, script))
			t.Cleanup(func() {
				rig.Kubectl(t, "-n", "legion", "delete", "sandbox/rig-clone", "secret/rig-clone-token", "--wait=false")
			})
			pod := rig.WaitPod(t, "legion", "legion.dev/rig=clone", 5*time.Minute)
			logs := rig.Kubectl(t, "-n", "legion", "logs", pod.Name)
			t.Logf("the clone pod %s ended %s (exit %d), QoS %s, runtime class %s:\n%s",
				pod.Name, pod.Phase, pod.ExitCode, pod.QOSClass, pod.RuntimeClassName, logs)
			if pod.Phase != "Succeeded" {
				t.Fatalf("the clone pod %s ended %s, exit %d", pod.Name, pod.Phase, pod.ExitCode)
			}
			if pod.QOSClass != "Burstable" || pod.RuntimeClassName != "gvisor" {
				t.Errorf("the clone pod ran in QoS class %q under runtime class %q, want Burstable under gvisor", pod.QOSClass, pod.RuntimeClassName)
			}
			if !strings.Contains(logs, "gVisor") {
				t.Errorf("the clone pod's kernel is not gVisor's: %s", logs)
			}
			cloned := regexp.MustCompile(`(?m)^cloned ([0-9a-f]{40}) in ([0-9]+) ms$`).FindStringSubmatch(logs)
			if cloned == nil || cloned[1] != main {
				t.Fatalf("the clone pod did not clone the fake GitHub's main, %s: %s", main, logs)
			}
			t.Logf("the pod's own clone of %s took %s ms", repo.URL(), cloned[2])
		})
	})

	t.Run("nfs", func(t *testing.T) {
		seed := fmt.Sprintf("seeded by the kind rig at %s", time.Now().UTC().Format(time.RFC3339))
		var server string
		rig.Measure(t, "nfs-server", func() { server = rig.StartNFSServer(t, seed) })
		rig.Measure(t, "nfs-mount", func() {
			rig.Apply(t, fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: rig-nfs, namespace: legion, labels: {legion.dev/rig: nfs}}
spec:
  restartPolicy: Never
  runtimeClassName: gvisor
  nodeSelector: {legion.dev/pool: legion}
  automountServiceAccountToken: false
  enableServiceLinks: false
  securityContext: {runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000}
  containers:
  - name: mount
    image: %s
    command: [/bin/sh, -c, %q]
    resources:
      requests: {cpu: 100m, memory: 64Mi}
      limits: {cpu: 500m, memory: 256Mi}
    volumeMounts: [{name: team, mountPath: /mnt/team}]
  volumes:
  - {name: team, nfs: {server: %s, path: /team}}
`, rig.Image, "set -eu\ncat /mnt/team/seed.txt\nprintf '%s\\n' 'written under gVisor' >/mnt/team/from-gvisor\ncat /mnt/team/from-gvisor", server))
			// The pod goes while the server still runs, so the node's NFS client unmounts the
			// share cleanly and the cluster's teardown waits on no hard mount.
			t.Cleanup(func() { rig.Kubectl(t, "-n", "legion", "delete", "pod/rig-nfs", "--wait=true", "--timeout=120s") })
			pod := rig.WaitPod(t, "legion", "legion.dev/rig=nfs", 4*time.Minute)
			logs := rig.Kubectl(t, "-n", "legion", "logs", pod.Name)
			t.Logf("the NFS pod ended %s (exit %d) under runtime class %s:\n%s", pod.Phase, pod.ExitCode, pod.RuntimeClassName, logs)
			if pod.Phase != "Succeeded" || !strings.Contains(logs, seed) {
				t.Fatalf("a gVisor pod does not mount %s:/team and read its seed: %s", server, logs)
			}
			held := strings.TrimSpace(rig.Kubectl(t, "-n", "nfs", "exec", "ganesha", "--", "cat", "/export/team/from-gvisor"))
			if held != "written under gVisor" {
				t.Fatalf("the NFS server holds %q in /export/team/from-gvisor, not what the gVisor pod wrote", held)
			}
			t.Logf("a gVisor pod mounts the NFS share %s:/team, reads its seed, and writes a file the server holds", server)
		})
	})
}

// revParse is what ref names in the bare repository bare, read with no configuration of the
// machine's.
func revParse(t *testing.T, bare, ref string) string {
	t.Helper()
	command := exec.Command("git", "--git-dir="+bare, "rev-parse", ref)
	command.Env = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + t.TempDir()}
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v: %s", ref, err, out)
	}
	return strings.TrimSpace(string(out))
}
