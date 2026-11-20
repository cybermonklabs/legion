// Stage 4b's tree on the kind rig (kind_tree_rig_live_test.go), which the kind job of ci.yaml runs:
// the whole daemon drives an issue tree on the Agent Sandbox runtime, its roles real Oh My Pi
// agents of the worker image under test in issue pods under gVisor, each pod's launcher dialling
// the daemon's worker stream at advertise_host, the kind network's gateway. Three tests hold the
// checkpoints, each on a rig of its own and run by a kind part of its own, since one rig running
// all fourteen runs past the part budget. One subtest per checkpoint prints `CHECK <name>: PASS`,
// and in each test the first that fails ends the test, since every checkpoint reads the state the
// ones before it left.
//
// The tree: tree 1 is seeded in planning, its architect and planner launched into its pod by the
// boot; two children filed under it in Dispatch take the cap's other two slots, each in a pod and
// on a disk of its own, while tree 2, a root set todo, waits in the line; child 1 closed and child
// 2 parked free the slots, and tree 2 takes one. Tree 1's planner completes planning through the
// `legion` tool, so its implementer starts in the same pod. TestStage4bSandboxTreeOnTheKindRigLive
// drives that tree, then ends one role's generation and loses its pod. The daemon's restarts start
// from the claims that tree holds at the end of it, seeded (newStage4bRunningTree): three restarts
// in TestStage4bDaemonRestartsOnTheKindRigLive, the last under `controller: daemon`, and one in
// TestStage4bStreamMoveOnTheKindRigLive. The model is the rig's stand-in, which
// answers every turn with a WAITING line and the turns a checkpoint scripts with a call of the
// tool the checkpoint names.
//
// What the devbox script this replaces also judged and this test does not: the checks that read a
// production system (its preflight, the deployment's own pod block, the projected token's
// rotation, the real Apps' bot logins and check runs, the autoscaler's node release), which are
// the operator's to prove; and its part (c), whose covers are the workflow's and supervise's own
// tests. Two pieces of its checkpoints depend on a real model's work and are left to the tests that
// own them: a task subagent's gh (github-credential) and the operator-launched controller's tmux
// session (controller), which this test reads as the daemon-launched controller's pod instead.
package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// The daemon's own lines the checkpoints read.
const (
	stage4bProbePassed  = "sandbox runtime: the worker image passed its probe"
	stage4bDeleted      = "sandbox runtime: deleted the closed issue's Sandbox and the role Secrets it owned"
	stage4bWritten      = "sandbox runtime: github credential written"
	stage4bRefreshed    = "sandbox runtime: github credential refreshed"
	stage4bStale        = "supervise: the process is alive at a stale address; replacing it with one at the current address"
	stage4bStaleHello   = "worker-stream: rejected hello (stale worker generation)"
	stage4bLaunchFailed = "supervise: launch failed"
	stage4bReported     = "capabilities: session reported"
	stage4bGhOwnersDir  = "/var/run/legion/gh"
	stage4bRolesDir     = "/var/run/legion/roles"
)

// The capability rows: those the image probe proves, and those each live session reports.
var (
	stage4bImageRows = []string{"eval-js", "eval-python", "browser", "lsp", "codegraph", "skills", "toolchain"}
	stage4bLiveRows  = []string{"subagents", "web-search", "mcp", "repository-extensions", "dispatch-envoy-tools", "github"}
)

// What the daemon's log may not hold: a token route, and a live capability row read open.
var (
	stage4bTokenRoute  = regexp.MustCompile(`/legion/v1/(gh-token|git-credential|provisioning-credential)`)
	stage4bLiveRowOpen = regexp.MustCompile(`^capability (subagents|web-search|mcp|repository-extensions|dispatch-envoy-tools|github) is open`)
)

// stage4bLive is one run: the rig, the tree, and what one checkpoint reads for the next.
type stage4bLive struct {
	*kindTreeRig
	tree1, tree2, child1, child2  string
	rootPod, child1Pod, child2Pod string
}

// TestStage4bSandboxTreeOnTheKindRigLive drives tree 1 and its children through the checkpoints of
// the stage 4b proof up to one role's end and its pod's loss, each a subtest. The daemon's restarts
// are TestStage4bDaemonRestartsOnTheKindRigLive's and TestStage4bStreamMoveOnTheKindRigLive's,
// each on a rig of its own: one rig running all fourteen checkpoints runs past the kind job's part
// budget.
func TestStage4bSandboxTreeOnTheKindRigLive(t *testing.T) {
	// A token lives seventeen minutes, so the daemon's refresh, which comes at least fifteen
	// minutes before a lease's expiry, comes inside the run. A quiet pod is kept thirty minutes,
	// past the test's -timeout, so no idle release takes a pod a checkpoint reads.
	rig := newKindTreeRig(t, kindTreeOptions{prefix: "SB", admissionCap: 3, tokenLifetime: 17 * time.Minute, idleMinutes: 30})
	r := &stage4bLive{kindTreeRig: rig}
	r.tree1 = r.seedRoot("stage 4b tree 1", phase.Planning, claim.RolePlanner)
	r.branch(r.tree1)
	r.boot()
	r.checkpoint("boot", r.bootCheck)
	r.checkpoint("advertise-host", r.advertiseHost)
	r.checkpoint("issue-independence", r.issueIndependence)
	r.checkpoint("child-release", r.childRelease)
	r.checkpoint("github-credential", r.githubCredential)
	r.checkpoint("github-credential-refresh", r.githubCredentialRefresh)
	r.checkpoint("full-agent", r.fullAgent)
	r.checkpoint("kill-role-resume", r.killRoleResume)
	r.checkpoint("fence", r.fence)
	r.checkpoint("pod-shape", r.podShape)
}

// TestStage4bDaemonRestartsOnTheKindRigLive restarts the daemon over a running tree at the same
// addresses, then handing the pods another address of the listener, then under `controller:
// daemon`. The controller's restart reads nothing of the tree, so it runs here, where it keeps
// each part under the budget: on CI run 44766893801 it took 185 s, and the part that ran it with
// the stream move took 24.0 minutes of its 24 minutes 21 seconds.
func TestStage4bDaemonRestartsOnTheKindRigLive(t *testing.T) {
	r := newStage4bRunningTree(t)
	r.checkpoint("restart-mid-tree", r.restartMidTree)
	r.checkpoint("address-moved-env-same-pod", r.addressMovedEnvSamePod)
	r.checkpoint("controller", r.controllerCheck)
}

// TestStage4bStreamMoveOnTheKindRigLive restarts the daemon over a running tree with its API and
// worker stream ports swapped, so every pod is replaced onto the moved stream, and judges the
// shape of every pod it ran.
func TestStage4bStreamMoveOnTheKindRigLive(t *testing.T) {
	r := newStage4bRunningTree(t)
	r.checkpoint("address-moved-stream-new-pod", r.addressMovedStreamNewPod)
	r.checkpoint("pod-shape", r.podShape)
}

// newStage4bRunningTree brings up a rig and boots the daemon on the claims
// TestStage4bSandboxTreeOnTheKindRigLive's tree holds when the daemon's restarts begin: tree 1 in
// implementing, its architect, planner and implementer in its pod, and tree 2 promoted from the
// line, its architect in a pod of its own, each registered. Tree 1's planner and implementer are
// seeded mid-launch beside its architect (seedRoot), and tree 2 is filed and set todo after the
// boot, as issue-independence files it, so admission's promotion starts its architect. The first
// checkpoint of each test reads the live claims at once, so the four are waited for here.
func newStage4bRunningTree(t *testing.T) *stage4bLive {
	// A quiet pod is kept thirty minutes, past either test's -timeout: the seeded roles take no
	// turn, and the checkpoints read their pods for longer than the default ten minutes.
	r := &stage4bLive{kindTreeRig: newKindTreeRig(t, kindTreeOptions{prefix: "SB", admissionCap: 3, idleMinutes: 30})}
	r.tree1 = r.seedRoot("stage 4b tree 1", phase.Implementing, claim.RolePlanner, claim.RoleImplementer)
	r.branch(r.tree1)
	r.boot()
	r.tree2 = r.newIssue("stage 4b tree 2", "")
	r.setStatus(r.tree2, "todo")
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer} {
		r.awaitLive(r.tree1, role)
	}
	r.awaitLive(r.tree2, claim.RoleArchitect)
	r.note("%s in implementing runs its architect, planner and implementer; %s runs its architect", r.tree1, r.tree2)
	return r
}

// boot: the daemon boots on the run's file, its image probe passes, and its capability report
// carries the probe's verdict on every image row, every one present; every live row is unchecked
// until a session reports; the only open rows are secrets, and pool-capacity where the pool had no
// room for the probe; and the daemon's state_dir holds no workspaces directory under the Sandbox
// runtime, which provisions every workspace in its pod.
func (r *stage4bLive) bootCheck() {
	t := r.t
	// The daemon probes its image before its API serves, and the rig's boot returns only once the
	// API answers, so the pass is already in the log: it is read there rather than waited for.
	probe := r.lastLog(func(l loggedRecord) bool { return l.Message == stage4bProbePassed })
	if probe == nil {
		t.Fatalf("the daemon logged no %q before its API served", stage4bProbePassed)
	}
	r.note("image probe: %v", probe.Attrs)
	var state kindState
	r.await("the capability report to carry the probe's verdict on every image row", time.Minute, func() bool {
		state = r.state()
		return !slices.ContainsFunc(state.Capabilities, func(row kindCapability) bool {
			return slices.Contains(stage4bImageRows, row.Name) && row.Status == "unchecked"
		})
	})
	rows := map[string]string{}
	var open []string
	for _, row := range state.Capabilities {
		rows[row.Name] = row.Status + ": " + row.Detail
		if row.Status == "open" {
			open = append(open, row.Name)
		}
		switch {
		case slices.Contains(stage4bImageRows, row.Name) && row.Status != "present":
			t.Errorf("the image row %s reads %s %q after the image probe passed, not present", row.Name, row.Status, row.Detail)
		case slices.Contains(stage4bLiveRows, row.Name) && (row.Status != "unchecked" || !strings.HasPrefix(row.Detail, "no session has reported yet (")):
			t.Errorf("the live row %s reads %s %q, not unchecked awaiting the first session's report", row.Name, row.Status, row.Detail)
		}
	}
	for _, name := range append(slices.Clone(stage4bImageRows), stage4bLiveRows...) {
		if _, ok := rows[name]; !ok {
			t.Errorf("the capability report has no row %s", name)
		}
	}
	sort.Strings(open)
	if !slices.Equal(open, []string{"secrets"}) && !slices.Equal(open, []string{"pool-capacity", "secrets"}) {
		t.Errorf("the open capabilities are %v, not secrets with or without pool-capacity", open)
	}
	if _, err := os.Stat(r.cfg.StateDir + "/workspaces"); err == nil {
		t.Errorf("the daemon's state_dir has a workspaces/ directory under the Sandbox runtime")
	}
	r.note("capabilities at boot: every image row present, every live row unchecked; open: %v", open)
}

// advertise-host: the daemon's file binds every interface and advertises the kind network's
// gateway. Both its listeners answer on loopback and at the gateway; every issue pod the watch has
// seen ready dials the worker stream at advertise_host, and tree 1's architect registered from
// such a pod; a recorded pod whose launcher dials the bound address instead departs from the pod
// shape; and the daemon's own loader refuses the file with advertise_host dropped, and with a
// loopback bind beside it, each naming bind.
func (r *stage4bLive) advertiseHost() {
	t := r.t
	for _, port := range []int{r.apiPort, r.streamPort} {
		for _, host := range []string{"127.0.0.1", r.kind.Gateway} {
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
			if err != nil {
				t.Fatalf("port %d does not answer on %s, so it does not listen on every interface: %v", port, host, err)
			}
			conn.Close()
		}
	}
	architect := r.awaitLive(r.tree1, claim.RoleArchitect)
	stream := r.stream()
	var pods map[string]kindPod
	r.await("the pod watch to record the architect's pod ready", time.Minute, func() bool {
		pods, _ = r.watch.recorded()
		_, ok := pods[architect.Locator.Sandbox.PodUID]
		return ok
	})
	for uid, pod := range pods {
		if got := pod.connect(); got != stream {
			t.Errorf("pod %s (uid %s) dials %s, not advertise_host at %s", pod.Metadata.Name, uid, got, stream)
		}
	}
	r.note("%s's architect registered (%s) from pod %s, whose launcher dials %s", r.tree1, architect.State, architect.Locator.Sandbox.Name, stream)
	wrong := pods[architect.Locator.Sandbox.PodUID]
	wrong.Spec.Containers = slices.Clone(wrong.Spec.Containers)
	for i, container := range wrong.Spec.Containers {
		argv := slices.Clone(container.Command)
		if j := slices.Index(argv, "--connect"); j >= 0 && j+1 < len(argv) {
			argv[j+1] = fmt.Sprintf("tcp://0.0.0.0:%d", r.streamPort)
		}
		wrong.Spec.Containers[i].Command = argv
	}
	want := fmt.Sprintf("launcher dials tcp://0.0.0.0:%d, not advertise_host at %s", r.streamPort, stream)
	if verdict := strings.Join(stage4bPodShape(wrong, stream), "; "); !strings.Contains(verdict, want) {
		t.Errorf("the pod shape did not refuse a pod told --connect tcp://0.0.0.0:%d: %q", r.streamPort, verdict)
	}
	file, err := os.ReadFile(r.configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, refusal := range []struct{ edit, want string }{
		{regexp.MustCompile(`(?m)^advertise_host: .*\n`).ReplaceAllString(string(file), ""), "bind 0.0.0.0 is not an address a pod can reach"},
		{regexp.MustCompile(`(?m)^bind: .*$`).ReplaceAllString(string(file), "bind: 127.0.0.1"), "bind 127.0.0.1 is loopback, where no pod reaches the worker stream"},
	} {
		path := r.configPath + ".refused"
		if err := os.WriteFile(path, []byte(refusal.edit), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := config.LoadForValidation(path, nil)
		if err == nil || !strings.Contains(err.Error(), refusal.want) {
			t.Errorf("the loader answered %v for the edited file, want a refusal naming %q", err, refusal.want)
		}
		r.note("the loader refused the edited file: %v", err)
	}
}

// issue-independence: while tree 1's planner holds, two children filed under tree 1 and set todo
// are entered by the daemon under the live tree, each its planner in an issue pod of its own. The
// three issues hold the whole cap of three, and tree 2, set todo, waits in the line. The three
// pods are Running with no affinity, each its own disk Bound and owned by the pod, and no pod lists
// another issue's workspace, each way. Then tree 1's planner completes planning through the
// `legion` tool, naming the plan record written under its session, and its implementer starts in
// the same pod.
func (r *stage4bLive) issueIndependence() {
	t := r.t
	planner := r.awaitLive(r.tree1, claim.RolePlanner)
	r.child1 = r.newIssue("stage 4b tree 1 child 1: closed done", r.tree1)
	r.child2 = r.newIssue("stage 4b tree 1 child 2: parked in backlog", r.tree1)
	r.setStatus(r.child1, "todo")
	r.setStatus(r.child2, "todo")
	for _, child := range []string{r.child1, r.child2} {
		held := r.awaitLive(child, claim.RolePlanner)
		if held.Tree != r.tree1 {
			t.Fatalf("the daemon records %s's planner under tree %q, not %s", child, held.Tree, r.tree1)
		}
	}
	r.tree2 = r.newIssue("stage 4b tree 2", "")
	r.setStatus(r.tree2, "todo")
	r.await("the cap full of tree 1 and its two children, with tree 2 waiting", 5*time.Minute, func() bool {
		state := r.state()
		active := slices.Sorted(slices.Values(state.Admission.Active))
		want := slices.Sorted(slices.Values([]string{r.tree1, r.child1, r.child2}))
		return state.Admission.Cap == 3 && slices.Equal(active, want) && slices.Equal(state.Admission.Waiting, []string{r.tree2})
	})
	architect, _ := r.claimOf(r.tree1, claim.RoleArchitect)
	one, _ := r.claimOf(r.child1, claim.RolePlanner)
	two, _ := r.claimOf(r.child2, claim.RolePlanner)
	r.rootPod, r.child1Pod, r.child2Pod = architect.Locator.Sandbox.Name, one.Locator.Sandbox.Name, two.Locator.Sandbox.Name
	pods := []string{r.rootPod, r.child1Pod, r.child2Pod}
	if distinct := slices.Compact(slices.Sorted(slices.Values(pods))); len(distinct) != 3 {
		t.Fatalf("tree 1's root and children do not run in three pods: %v", pods)
	}
	disks := map[string]bool{}
	for _, name := range pods {
		pod, err := r.pod(name)
		if err != nil {
			t.Fatalf("read pod %s: %v", name, err)
		}
		if pod.Status.Phase != "Running" || (len(pod.Spec.Affinity) > 0 && string(pod.Spec.Affinity) != "null") {
			t.Errorf("pod %s is %s with affinity %s, want Running with none", name, pod.Status.Phase, pod.Spec.Affinity)
		}
		disk := r.assertOwnDisk(name)
		disks[disk] = true
	}
	if len(disks) != 3 {
		t.Errorf("the three issue pods' disks are not three: %v", disks)
	}
	for _, pair := range []struct {
		own        string
		ownRole    claim.Role
		other      string
		otherRole  claim.Role
		otherIssue string
	}{
		{r.rootPod, claim.RoleArchitect, r.child1Pod, claim.RolePlanner, r.child1},
		{r.child1Pod, claim.RolePlanner, r.rootPod, claim.RoleArchitect, r.tree1},
	} {
		path := "/legion/workspaces/" + r.repo.Slug() + "/" + strings.ToLower(pair.otherIssue)
		if out, err := r.roleExec(pair.other, pair.otherRole, "ls", path); err != nil {
			t.Fatalf("%s's own workspace %s is not in its pod %s: %v %s", pair.otherIssue, path, pair.other, err, out)
		}
		out, err := r.roleExec(pair.own, pair.ownRole, "ls", path)
		if err == nil || !strings.Contains(out+err.Error(), "No such file or directory") {
			t.Errorf("pod %s lists %s's workspace %s, or fails for another reason than its absence: %v %s", pair.own, pair.otherIssue, path, err, out)
		}
	}
	r.note("tree 1 (%s) and its children %s and %s run in pods %v, each on a disk of its own; tree 2 (%s) waits", r.tree1, r.child1, r.child2, pods, r.tree2)

	marker := "Kind proof planning operation " + r.tree1
	record := r.writeRecord(r.tree1, planner.Session, "plan")
	r.model.callOn(func(seen kindSeen) bool { return strings.Contains(seen.turn, marker) }, "legion",
		map[string]any{"op": "handoff_complete", "summary": "Planned the one-file smoke change."})
	r.send(planner.Session, marker+": complete planning now.")
	r.await(r.tree1+" in implementing", kindTreeWait, func() bool { return r.state().Issues[r.tree1].Phase == string(phase.Implementing) })
	implementer := r.awaitLive(r.tree1, claim.RoleImplementer)
	if implementer.Locator.Sandbox.PodUID != architect.Locator.Sandbox.PodUID {
		t.Errorf("tree 1's implementer runs in pod uid %s, not its issue's pod %s", implementer.Locator.Sandbox.PodUID, architect.Locator.Sandbox.PodUID)
	}
	r.note("tree 1's planner completed planning naming its record %s (version %d); its implementer runs in pod %s", record.ID, record.Version, implementer.Locator.Sandbox.Name)
}

// assertOwnDisk holds pod's disk, <pod>-issue, Bound and owned by the pod itself, and returns
// its uid.
func (r *stage4bLive) assertOwnDisk(pod string) string {
	out, err := r.kubectl("get", "pvc", pod+"-issue", "-o", `jsonpath={.status.phase} {.metadata.uid} {.metadata.ownerReferences[?(@.controller==true)].kind}/{.metadata.ownerReferences[?(@.controller==true)].name}`)
	fields := strings.Fields(out)
	if err != nil || len(fields) != 3 || fields[0] != "Bound" || fields[2] != "Pod/"+pod {
		r.t.Fatalf("pod %s's disk %s-issue reads %q (%v), want Bound and owned by the pod", pod, pod, out, err)
	}
	return fields[1]
}

// child-release: child 1 closed done is released while its parent runs: every claim of it is
// retired keeping its session, its Sandbox is deleted, logged once, its pod goes and its disk with
// it, and no lost volume is reported; the root's Sandbox stays Running, its own disk Bound and its
// pod the same. Child 2 parked in backlog leaves no Sandbox, pod or disk either. The two slots go,
// and tree 2, first in the line, takes one.
func (r *stage4bLive) childRelease() {
	t := r.t
	root, err := r.pod(r.rootPod)
	if err != nil {
		t.Fatal(err)
	}
	lost := len(r.logs(func(l loggedRecord) bool { return strings.Contains(l.Message, "volume was lost") }))
	if n := len(r.logged(stage4bDeleted, map[string]string{"issue": r.child1})); n != 0 {
		t.Fatalf("the daemon deleted %s's Sandbox before its done", r.child1)
	}
	r.setStatus(r.child1, "done")
	r.awaitReleased(r.child1, r.child1Pod)
	mode, err := r.kubectl("get", "sandbox", r.rootPod, "-o", "jsonpath={.spec.operatingMode}")
	if err != nil || (mode != "" && mode != "Running") {
		t.Errorf("the root's Sandbox %s is %q (%v) after %s's release, want Running", r.rootPod, mode, err, r.child1)
	}
	r.assertOwnDisk(r.rootPod)
	if now, err := r.pod(r.rootPod); err != nil || now.Metadata.UID != root.Metadata.UID {
		t.Errorf("the root's pod %s changed from uid %s across %s's release: %+v %v", r.rootPod, root.Metadata.UID, r.child1, now.Metadata, err)
	}
	// The daemon logs the deletion once the API server has answered it, so the line can land a
	// moment after the Sandbox, the pod and the disk read gone (CI run 44656539661: 02:17:01.77,
	// after the read above returned).
	r.await("the daemon to log its deletion of "+r.child1+"'s Sandbox", time.Minute, func() bool {
		return len(r.logged(stage4bDeleted, map[string]string{"issue": r.child1})) > 0
	})
	if n := len(r.logged(stage4bDeleted, map[string]string{"issue": r.child1})); n != 1 {
		t.Errorf("the daemon logged %q for %s %d times, want once", stage4bDeleted, r.child1, n)
	}
	if now := len(r.logs(func(l loggedRecord) bool { return strings.Contains(l.Message, "volume was lost") })); now != lost {
		t.Errorf("the daemon reported a lost volume across %s's release", r.child1)
	}
	for _, held := range r.claims() {
		if held.Issue == r.child1 && (held.State != "retired" || held.Session == "" || held.SessionFile == "") {
			t.Errorf("%s's claim %s is %s with session %q at %q after its done, want retired with its session kept", r.child1, held.Token, held.State, held.Session, held.SessionFile)
		}
	}
	r.setStatus(r.child2, "backlog")
	r.awaitReleased(r.child2, r.child2Pod)
	r.await("tree 2 to take a slot the two children freed", 5*time.Minute, func() bool {
		state := r.state()
		return slices.Contains(state.Admission.Active, r.tree2) && !slices.Contains(state.Admission.Waiting, r.tree2)
	})
	r.note("%s done and %s in backlog left no Sandbox, pod or disk; the root kept pod uid %s; tree 2 took a slot", r.child1, r.child2, root.Metadata.UID)
}

// awaitReleased waits until the cluster holds no Sandbox or pod of issue and no disk of pod.
func (r *stage4bLive) awaitReleased(issue, pod string) {
	r.await(issue+"'s Sandbox, pod and disk to be gone", 10*time.Minute, func() bool {
		out, err := r.kubectl("get", "sandboxes,pods", "-l", "legion.dev/project="+r.project+",legion.dev/issue="+issue, "-o", "name")
		if err != nil || strings.TrimSpace(out) != "" {
			return false
		}
		disk, err := r.kubectl("get", "pvc", pod+"-issue", "--ignore-not-found", "-o", "name")
		return err == nil && strings.TrimSpace(disk) == ""
	})
}

// github-credential: each role's GitHub App token is a file its plain gh and git read. In tree 1's
// pod the implementer's namespaces hold the implement App's hosts.yml and the architect's the
// review App's, each its own role directory's copy, which differ; each role's gh answers its own
// App's bot; the agent's environment names GH_CONFIG_DIR and no grant file or tool pin, its PATH the
// image's legion directory first and no worker-bin shim; inside a role's namespaces the roles
// directory is covered empty and /proc lists one worker-shim, its own, while the container holds
// all six roles' directories; the daemon wrote every role's credential and serves no token route;
// the implementer's own bash runs jj undo --help and reads jj's own first help line; the real gh
// with no files is nobody; and the implementer's push, made with its App's token, reaches the
// listener as a signed webhook the listener takes.
func (r *stage4bLive) githubCredential() {
	t := r.t
	implementer := r.awaitLive(r.tree1, claim.RoleImplementer)
	pod := implementer.Locator.Sandbox.Name
	configDir := stage4bGhOwnersDir + "/" + strings.ToLower(r.repo.Owner)
	if bin, err := r.roleExec(pod, claim.RoleImplementer, "sh", "-c", "command -v gh"); err != nil || bin != "/opt/legion/bin/gh" {
		t.Errorf("the implementer's gh is %q (%v), want the image's wrapper /opt/legion/bin/gh", bin, err)
	}
	status, _ := r.agentShell(pod, claim.RoleImplementer, "LEGION_GH_PATH=/usr/local/bin/gh LEGION_GH_OWNERS_DIR="+stage4bGhOwnersDir+
		" GH_CONFIG_DIR="+configDir+" GH_TOKEN= GITHUB_TOKEN= GH_HOST= gh auth status 2>&1")
	if !strings.Contains(status, "x-access-token") {
		t.Errorf("gh auth status in the implementer's namespaces names no x-access-token account: %s", status)
	}
	hashes := map[claim.Role]string{}
	for role, app := range map[claim.Role]string{claim.RoleImplementer: r.implement.Slug, claim.RoleArchitect: r.review.Slug} {
		if viewer := r.viewer(pod, role); viewer != app+"[bot]" {
			t.Errorf("gh in the %s's namespaces of %s acts as %q, want %s[bot]", role, pod, viewer, app)
		}
		inside, err := r.roleExec(pod, role, "sh", "-c", "sha256sum "+configDir+"/hosts.yml | cut -d' ' -f1")
		if err != nil {
			t.Fatalf("hash the %s's hosts.yml: %v", role, err)
		}
		outside, err := r.podExec(pod, "sh", "-c", fmt.Sprintf("sha256sum %s/%s/gh/%s/hosts.yml | cut -d' ' -f1", stage4bRolesDir, role, strings.ToLower(r.repo.Owner)))
		if err != nil || outside != inside {
			t.Errorf("the %s's %s/hosts.yml (sha256 %s) is not its own %s/%s/gh copy (sha256 %s, %v)", role, configDir, inside, stage4bRolesDir, role, outside, err)
		}
		hashes[role] = inside
		if seen, err := r.roleExec(pod, role, "ls", "-A", stage4bRolesDir); err != nil || seen != "" {
			t.Errorf("the %s's namespaces of %s see %s holding %q (%v), want it covered empty", role, pod, stage4bRolesDir, seen, err)
		}
		shims, err := r.roleExec(pod, role, "sh", "-c", `for d in /proc/[0-9]*; do [ "$(tr '\0' '\n' 2>/dev/null <"$d/cmdline" | sed -n 2p)" = worker-shim ] && echo "${d##*/}"; done; true`)
		if err != nil || len(strings.Fields(shims)) != 1 {
			t.Errorf("the %s's namespaces of %s list worker-shims %q (%v), want its own alone", role, pod, shims, err)
		}
	}
	if hashes[claim.RoleImplementer] == hashes[claim.RoleArchitect] {
		t.Errorf("the implementer's and the architect's hosts.yml in %s are one file", pod)
	}
	env, err := r.agentEnviron(pod, claim.RoleImplementer)
	if err != nil {
		t.Fatalf("no readable Oh My Pi environment in the implementer's namespaces of %s: %v", pod, err)
	}
	for _, want := range []string{"GH_CONFIG_DIR=" + configDir, "GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST=", "LEGION_GH_OWNERS_DIR=" + stage4bGhOwnersDir, "LEGION_GH_PATH=/usr/local/bin/gh"} {
		if !slices.Contains(env, want) {
			t.Errorf("the implementer's Oh My Pi environment lacks %s", want)
		}
	}
	for _, absent := range []string{"LEGION_GIT_PATH", "LEGION_JJ_PATH", "LEGION_CREDENTIAL_HELPER", "LEGION_GRANT_FILE", "LEGION_GRANT", "LEGION_IMPLEMENT_APP_LOGIN", "LEGION_REVIEW_APP_LOGIN"} {
		if value := kindEnvValue(env, absent); value != "" || slices.Contains(env, absent+"=") {
			t.Errorf("the implementer's Oh My Pi environment still carries %s", absent)
		}
	}
	if path := kindEnvValue(env, "PATH"); !strings.HasPrefix(path+":", "/opt/legion/bin:") || strings.Contains(":"+path+":", "worker-bin") {
		t.Errorf("the implementer's PATH is %s, want the image's legion directory first and no worker-bin shim", path)
	}
	if spec, err := r.pod(pod); err != nil || (spec.Spec.ShareProcessNamespace != nil && *spec.Spec.ShareProcessNamespace) {
		t.Errorf("%s shares its process namespace (%v)", pod, err)
	}
	held, err := r.podExec(pod, "ls", "-A", stage4bRolesDir)
	if err != nil {
		t.Fatalf("list %s in the roles container of %s: %v", stage4bRolesDir, pod, err)
	}
	for _, role := range []string{"architect", "planner", "implementer", "tester", "reviewer", "merger"} {
		if !slices.Contains(strings.Fields(held), role) {
			t.Errorf("the roles container of %s holds no %s/%s: %s", pod, stage4bRolesDir, role, held)
		}
		if len(r.logged(stage4bWritten, map[string]string{"sandbox": pod, "role": role})) == 0 {
			t.Errorf("the daemon log has no %q line for %s of %s", stage4bWritten, role, pod)
		}
	}
	if routes := r.logs(func(l loggedRecord) bool {
		return stage4bTokenRoute.MatchString(l.Message + fmt.Sprint(l.Attrs))
	}); len(routes) > 0 {
		t.Errorf("the daemon log names a token route: %+v", routes[0])
	}
	if out, err := r.roleExec(pod, claim.RoleImplementer, "sh", "-c", "GH_CONFIG_DIR=/nonexistent GH_TOKEN= GITHUB_TOKEN= GH_HOST= /usr/local/bin/gh api graphql -f query='{viewer{login}}' --jq .data.viewer.login 2>&1"); err == nil || !strings.Contains(out, "GH_TOKEN") {
		t.Errorf("gh with GH_CONFIG_DIR=/nonexistent answered %q (%v), want gh's own not-logged-in refusal", out, err)
	}

	since := time.Now()
	jj := "Kind proof jj operation " + r.tree1
	r.model.callOn(func(seen kindSeen) bool { return strings.Contains(seen.turn, jj) }, "bash", map[string]any{"command": "jj undo --help | head -n 1"})
	r.send(implementer.Session, jj+": run jj undo --help.")
	r.await("the implementer's bash to read jj undo --help's first line", 5*time.Minute, func() bool {
		return len(r.model.requests(since, "Undo the last operation")) > 0
	})
	push := "Kind proof push operation " + r.tree1
	workspace := "/legion/workspaces/" + r.repo.Slug() + "/" + strings.ToLower(r.tree1)
	before := len(r.kind.GitHub.Deliveries())
	r.model.callOn(func(seen kindSeen) bool { return strings.Contains(seen.turn, push) }, "bash", map[string]any{"command": fmt.Sprintf(
		"cd %s && mkdir -p smoke && echo %s > smoke/%s.md && jj commit -m 'Add the smoke file' && jj bookmark set legion/%s -r @- && jj git push --bookmark legion/%s",
		workspace, r.tree1, strings.ToLower(r.tree1), r.tree1, r.tree1)})
	r.send(implementer.Session, push+": commit and push the smoke file.")
	r.await("the implementer's push to reach the listener as a signed webhook it takes", 5*time.Minute, func() bool {
		return slices.ContainsFunc(r.kind.GitHub.Deliveries()[before:], func(line string) bool {
			return strings.HasPrefix(line, "push from "+r.implement.Slug+"[bot] ") && strings.Contains(line, "-> 200")
		})
	})
	r.note("in %s the implementer's gh is %s[bot] and the architect's %s[bot], from their own hosts.yml; the implementer's push by its App reached the listener", pod, r.implement.Slug, r.review.Slug)
}

// viewer is the login gh answers GraphQL's viewer with in role's namespaces of pod, run as the
// role's agent runs it (agentShell).
func (r *stage4bLive) viewer(pod string, role claim.Role) string {
	out, _ := r.agentShell(pod, role, "gh api graphql -f query='{viewer{login}}' --jq .data.viewer.login")
	return strings.TrimSpace(out)
}

// agentGhEnv is what a role's agent hands its gh that an exec into the role's namespaces does not
// have: the shared public leak check every `legion gh` resolves before it writes or reads
// (leakguard.CheckVar) and the deployment's manifest digest it holds that check to
// (leakguard.CheckerVar), which the runtime sets in each role's environment and the container's own
// exec does not carry, the real gh and the bundle it reads, and the PATH that puts the image's
// wrapper first.
var agentGhEnv = []string{leakguard.CheckVar, leakguard.CheckerVar, "LEGION_GH_PATH", "LEGION_GH_OWNERS_DIR", "GH_CONFIG_DIR", "PATH"}

// agentShell runs script in role's namespaces of pod with agentGhEnv as role's agent holds it
// (agentEnviron), so a command there meets what the agent's own bash would.
func (r *stage4bLive) agentShell(pod string, role claim.Role, script string) (string, error) {
	in := func(command ...string) (string, error) { return r.roleExec(pod, role, command...) }
	out, err := agentShellIn(in, script)
	if err != nil {
		return "", fmt.Errorf("the %s's agent shell in %s: %w", role, pod, err)
	}
	return out, nil
}

// agentShellIn runs script through in, an exec that sees the agent's processes (a role's
// namespaces, or the controller's container), with agentGhEnv as the agent there holds it
// (agentEnvironIn).
func agentShellIn(in func(command ...string) (string, error), script string) (string, error) {
	env, err := agentEnvironIn(in)
	if err != nil {
		return "", fmt.Errorf("read the agent's environment: %w", err)
	}
	var exports strings.Builder
	for _, name := range agentGhEnv {
		if value := kindEnvValue(env, name); value != "" {
			fmt.Fprintf(&exports, "export %s='%s'; ", name, strings.ReplaceAll(value, "'", `'\''`))
		}
	}
	return in("sh", "-c", exports.String()+script)
}

// agentEnviron is the environment of role's agent in pod (agentEnvironIn in role's namespaces).
func (r *stage4bLive) agentEnviron(pod string, role claim.Role) ([]string, error) {
	return agentEnvironIn(func(command ...string) (string, error) { return r.roleExec(pod, role, command...) })
}

// agentEnvironIn is the environment of the agent in, an exec that sees its processes: the child
// of its worker-shim, one NAME=value a line.
func agentEnvironIn(in func(command ...string) (string, error)) ([]string, error) {
	out, err := in("sh", "-c", `for d in /proc/[0-9]*; do [ "$(tr '\0' '\n' 2>/dev/null <"$d/cmdline" | sed -n 2p)" = worker-shim ] && shim=${d##*/}; done
[ -n "$shim" ] || exit 1
for d in /proc/[0-9]*; do [ "$(cut -d' ' -f4 "$d/stat" 2>/dev/null)" = "$shim" ] && { tr '\0' '\n' <"$d/environ"; exit 0; }; done
exit 1`)
	if err != nil {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// hostsToken is the oauth_token role's hosts.yml holds in pod.
func (r *stage4bLive) hostsToken(pod string, role claim.Role) string {
	out, err := r.roleExec(pod, role, "sh", "-c", "sed -n 's/^ *oauth_token: *//p' "+stage4bGhOwnersDir+"/"+strings.ToLower(r.repo.Owner)+"/hosts.yml | head -n 1")
	if err != nil {
		r.t.Fatalf("read the %s's hosts.yml in %s: %v", role, pod, err)
	}
	return strings.TrimSpace(out)
}

// github-credential-refresh: the daemon refreshes tree 1's architect's credential in its running
// pod: its lease chain in the log starts with one written line, every later line is a refresh by
// the review App at least fifteen minutes before the expiry of the lease it replaced, each
// expiring later; the pod's hosts.yml changes to a new token, in the same pod; and the architect's
// gh still answers the review App.
func (r *stage4bLive) githubCredentialRefresh() {
	t := r.t
	architect := r.awaitLive(r.tree1, claim.RoleArchitect)
	pod, uid := architect.Locator.Sandbox.Name, architect.Locator.Sandbox.PodUID
	read := time.Now()
	token := r.hostsToken(pod, claim.RoleArchitect)
	if token == "" {
		t.Fatalf("the architect's hosts.yml in %s names no oauth_token", pod)
	}
	chain := func() []loggedRecord {
		return r.logs(func(l loggedRecord) bool {
			return (l.Message == stage4bWritten || l.Message == stage4bRefreshed) && fmt.Sprint(l.Attrs["sandbox"]) == pod && fmt.Sprint(l.Attrs["role"]) == "architect"
		})
	}
	r.await("the daemon to refresh the architect's github credential", 20*time.Minute, func() bool {
		return slices.ContainsFunc(chain(), func(l loggedRecord) bool { return l.Message == stage4bRefreshed && l.at.After(read) })
	})
	leases := chain()
	if leases[0].Message != stage4bWritten {
		t.Fatalf("the architect's lease chain does not start with a written line: %+v", leases[0])
	}
	for i := 1; i < len(leases); i++ {
		line, previous := leases[i], leases[i-1]
		if line.Message != stage4bRefreshed || fmt.Sprint(line.Attrs["app"]) != "review" {
			t.Errorf("the architect's lease chain carries %q by %v after its first line, want a refresh by the review App", line.Message, line.Attrs["app"])
		}
		was, now := stage4bTime(t, previous.Attrs["expiresAt"]), stage4bTime(t, line.Attrs["expiresAt"])
		if line.at.After(was.Add(-15 * time.Minute)) {
			t.Errorf("the daemon refreshed the architect's credential at %s, fewer than fifteen minutes before the expiry %s of the lease it replaced", line.at, was)
		}
		if !now.After(was) {
			t.Errorf("the refreshed lease expires at %s, no later than the one it replaced, %s", now, was)
		}
	}
	var refreshed string
	r.await("the architect's hosts.yml in its pod to carry the refreshed token", 5*time.Minute, func() bool {
		refreshed = r.hostsToken(pod, claim.RoleArchitect)
		return refreshed != "" && refreshed != token
	})
	if now, err := r.pod(pod); err != nil || now.Metadata.UID != uid {
		t.Errorf("the architect's pod %s was replaced while the refresh was awaited", pod)
	}
	if viewer := r.viewer(pod, claim.RoleArchitect); viewer != r.review.Slug+"[bot]" {
		t.Errorf("the architect's gh acts as %q after the refresh, want %s[bot]", viewer, r.review.Slug)
	}
	r.note("the architect's lease chain holds %d lines, each refresh at least fifteen minutes before the expiry it replaced; its hosts.yml carries the refreshed token in pod uid %s", len(leases), uid)
}

// stage4bTime is a log attribute's time.
func stage4bTime(t *testing.T, value any) time.Time {
	text := fmt.Sprint(value)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700 MST"} {
		if at, err := time.Parse(layout, text); err == nil {
			return at
		}
	}
	t.Fatalf("a lease line's expiresAt %q is not a time", text)
	return time.Time{}
}

// full-agent: every live session measures the six live capability rows at an idle tick and sends
// them; the daemon renders each row present from the live reports, every row's detail naming the
// pod of a live claim; tree 1's planner's own view carries its report at its running incarnation,
// every row proved; the log's report line for that incarnation holds the rows; and no live row has
// read open.
func (r *stage4bLive) fullAgent() {
	t := r.t
	var rows []kindCapability
	r.await("the six live capability rows to read present", 5*time.Minute, func() bool {
		rows = rows[:0]
		for _, row := range r.state().Capabilities {
			if slices.Contains(stage4bLiveRows, row.Name) {
				rows = append(rows, row)
			}
		}
		return len(rows) == len(stage4bLiveRows) && !slices.ContainsFunc(rows, func(row kindCapability) bool {
			return row.Status != "present"
		})
	})
	live := r.liveClaims()
	for _, row := range rows {
		if !strings.Contains(row.Detail, "pod ") || !slicesContainsFunc(live, func(held api.OperatorClaim) bool { return strings.Contains(row.Detail, held.Locator.Sandbox.Name) }) {
			t.Errorf("the live row %s's detail names no pod of a live claim: %s", row.Name, row.Detail)
		}
	}
	view := r.workerView(r.tree1, "planner")
	var capabilities struct {
		OK          []string `json:"ok"`
		Open        []string `json:"open"`
		Incarnation string   `json:"incarnation"`
	}
	_ = json.Unmarshal(view["capabilities"], &capabilities)
	var locator struct {
		Incarnation string `json:"incarnation"`
	}
	_ = json.Unmarshal(view["locator"], &locator)
	if !slices.Equal(slices.Sorted(slices.Values(capabilities.OK)), slices.Sorted(slices.Values(stage4bLiveRows))) || len(capabilities.Open) != 0 ||
		capabilities.Incarnation != locator.Incarnation {
		t.Errorf("tree 1's planner view carries %+v at incarnation %s, want every live row proved by its running process", capabilities, locator.Incarnation)
	}
	if len(r.logged(stage4bReported, map[string]string{"incarnation": locator.Incarnation})) == 0 {
		t.Errorf("the daemon log holds no %q line for tree 1's planner process %s", stage4bReported, locator.Incarnation)
	}
	if opened := r.logs(func(l loggedRecord) bool {
		return stage4bLiveRowOpen.MatchString(l.Message)
	}); len(opened) > 0 {
		t.Errorf("the daemon log holds a live row open: %+v", opened[0])
	}
	r.note("the six live rows read present; tree 1's planner (process %s) carries them proved", locator.Incarnation)
}

// slicesContainsFunc is whether any claim of live satisfies f.
func slicesContainsFunc(live map[claim.Token]api.OperatorClaim, f func(api.OperatorClaim) bool) bool {
	for _, held := range live {
		if f(held) {
			return true
		}
	}
	return false
}

// workerView is the claim of role's worker of issue as GET /legion/v1/state shows it, by field.
func (r *stage4bLive) workerView(issue, role string) map[string]json.RawMessage {
	_, body := r.d.request("GET", "/legion/v1/state", nil, true)
	var state struct {
		Issues map[string]struct {
			Workers map[string]struct {
				Claim map[string]json.RawMessage `json:"claim"`
			} `json:"workers"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		r.t.Fatalf("decode the state: %v", err)
	}
	return state.Issues[issue].Workers[role].Claim
}

// kill-role-resume: one kill of tree 1's planner's worker-shim process group, sent from the roles
// container outside every role's namespaces, found through the role-init of the planner's
// generation (killRole), ends that generation alone: the claim resumes the same session in the
// same pod, the roles container the same, and every other role of the pod keeps its generation and
// process.
func (r *stage4bLive) killRoleResume() {
	t := r.t
	planner := r.awaitLive(r.tree1, claim.RolePlanner)
	pod, uid := planner.Locator.Sandbox.Name, planner.Locator.Sandbox.PodUID
	before, err := r.pod(pod)
	if err != nil {
		t.Fatal(err)
	}
	peers := r.peers(uid, planner.Token)
	if len(peers) == 0 {
		t.Fatalf("no other role runs in the planner's pod %s, so the kill proves nothing about its peers", pod)
	}
	if out, err := r.killRole(pod, claim.RolePlanner, planner.Locator.Sandbox.Generation); err != nil {
		t.Fatalf("kill the planner's worker-shim process group at generation %d in %s: %v %s", planner.Locator.Sandbox.Generation, pod, err, out)
	}
	var resumed api.OperatorClaim
	r.await("the planner to resume after only its generation ended", kindTreeWait, func() bool {
		resumed, _ = r.claimOf(r.tree1, claim.RolePlanner)
		return kindLive(resumed) && resumed.Locator != nil && resumed.Locator.Incarnation != planner.Locator.Incarnation
	})
	if resumed.Locator.Sandbox.PodUID != uid || resumed.Session != planner.Session {
		t.Errorf("the planner resumed in pod uid %s with session %s, want %s and %s", resumed.Locator.Sandbox.PodUID, resumed.Session, uid, planner.Session)
	}
	if after, err := r.pod(pod); err != nil || after.rolesContainer() != before.rolesContainer() {
		t.Errorf("the planner's kill and relaunch changed the roles container of %s: %s to %s (%v)", pod, before.rolesContainer(), after.rolesContainer(), err)
	}
	if now := r.peers(uid, planner.Token); !maps.Equal(now, peers) {
		t.Errorf("the planner's kill changed another role's process: before %v, after %v", peers, now)
	}
	r.note("killed the planner's generation in pod %s; it resumed session %s at %s in the same pod, its peers %d unchanged", pod, resumed.Session, resumed.Locator.Incarnation, len(peers))
}

// peers is every live claim but skip in the pod of uid, by token, as its generation and process.
func (r *stage4bLive) peers(uid string, skip claim.Token) map[claim.Token]string {
	peers := map[claim.Token]string{}
	for token, held := range r.liveClaims() {
		if token != skip && held.Locator.Sandbox.PodUID == uid {
			peers[token] = fmt.Sprintf("%d %s", held.Generation, held.Locator.Incarnation)
		}
	}
	return peers
}

// fence: tree 1's pod deleted, the planner resumes the same session in a replacement pod, its boot
// token rotated there, and a hello with the replaced generation's token is refused by the worker
// stream as stale, which the daemon logs.
func (r *stage4bLive) fence() {
	t := r.t
	planner := r.awaitLive(r.tree1, claim.RolePlanner)
	pod, uid := planner.Locator.Sandbox.Name, planner.Locator.Sandbox.PodUID
	bootToken := func(held api.OperatorClaim) (string, error) {
		return r.podExec(held.Locator.Sandbox.Name, "cat", fmt.Sprintf("%s/planner/private/g%d/LEGION_BOOT_TOKEN", stage4bRolesDir, held.Locator.Sandbox.Generation))
	}
	old, err := bootToken(planner)
	if err != nil || old == "" {
		t.Fatalf("the planner's private generation directory has no LEGION_BOOT_TOKEN: %v", err)
	}
	oldFile := r.work + "/old-boot-token"
	if err := os.WriteFile(oldFile, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.kubectl("delete", "pod", pod, "--wait=false"); err != nil {
		t.Fatalf("delete pod %s: %v", pod, err)
	}
	var replaced api.OperatorClaim
	r.await("the planner to resume in a replacement pod", kindTreeWait, func() bool {
		replaced, _ = r.claimOf(r.tree1, claim.RolePlanner)
		return kindLive(replaced) && replaced.Locator != nil && replaced.Locator.Sandbox != nil && replaced.Locator.Sandbox.PodUID != uid
	})
	if replaced.Session != planner.Session {
		t.Errorf("the replacement planner has session %s, want %s", replaced.Session, planner.Session)
	}
	r.await("the planner's boot token to rotate in its replacement pod", 5*time.Minute, func() bool {
		now, err := bootToken(replaced)
		return err == nil && now != "" && now != old
	})
	refused := len(r.logs(func(l loggedRecord) bool { return l.Message == stage4bStaleHello }))
	shim := stage4bRun(r.legion, "worker-shim", "--connect", r.stream(), "--boot-token-file", oldFile, "--", "true")
	if shim == nil {
		t.Fatalf("the worker stream accepted a hello with the previous generation's boot token")
	}
	r.await("the daemon to log the replaced generation's hello as stale", 30*time.Second, func() bool {
		return len(r.logs(func(l loggedRecord) bool { return l.Message == stage4bStaleHello })) > refused
	})
	r.note("the replacement pod is uid %s; the former boot token was refused: %v", replaced.Locator.Sandbox.PodUID, shim)
}

// stage4bRun runs a command for a minute at most and answers its failure, nil when it exits 0.
func stage4bRun(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// restart-mid-tree: the daemon stopped and booted again at the same addresses re-adopts every
// live claim as it was: the same session, process, pod and container, no generation relaunched a
// while after, every pod the uid its claim records, and no stale-address replacement logged.
func (r *stage4bLive) restartMidTree() {
	t := r.t
	planner := r.awaitLive(r.tree1, claim.RolePlanner)
	before := r.liveClaims()
	stale := len(r.logs(func(l loggedRecord) bool { return l.Message == stage4bStale }))
	r.restart()
	r.await("the planner to be re-adopted", 5*time.Minute, func() bool {
		held, ok := r.claimOf(r.tree1, claim.RolePlanner)
		return ok && kindLive(held)
	})
	after, _ := r.claimOf(r.tree1, claim.RolePlanner)
	if after.Session != planner.Session || after.Locator.Incarnation != planner.Locator.Incarnation || *after.Locator.Sandbox != *planner.Locator.Sandbox {
		t.Errorf("the restart relaunched the planner: %+v to %+v", planner.Locator, after.Locator)
	}
	time.Sleep(75 * time.Second)
	now := r.liveClaims()
	for token, was := range before {
		held, ok := now[token]
		if !ok || held.Generation != was.Generation || held.Locator.Incarnation != was.Locator.Incarnation {
			t.Errorf("the restart at the same addresses relaunched %s: %+v to %+v", token, was.Locator, held.Locator)
			continue
		}
		pod, err := r.pod(held.Locator.Sandbox.Name)
		if err != nil || pod.Metadata.UID != held.Locator.Sandbox.PodUID {
			t.Errorf("%s's pod %s is uid %s, not its recorded %s (%v)", token, held.Locator.Sandbox.Name, pod.Metadata.UID, held.Locator.Sandbox.PodUID, err)
		}
	}
	if n := len(r.logs(func(l loggedRecord) bool { return l.Message == stage4bStale })); n != stale {
		t.Errorf("the restart at the same addresses logged %d stale-address replacements", n-stale)
	}
	r.note("the daemon restarted and re-adopted %d live claims as they were", len(before))
}

// addressMoved restarts the daemon with what move changes of the rig's addresses, and holds every
// claim that ran before to its next generation, registered and ready: one stale-address
// replacement each, whose detail ends with detail, no failed launch, its session file kept and
// passed to its agent's --resume, no launch failure or new death counted, and in the same pod or
// a new one as samePod says; its role's agent handed the current ENVOY_URL and its pod's launcher
// the current stream.
func (r *stage4bLive) addressMoved(move func() string, samePod bool) {
	t := r.t
	before := r.liveClaims()
	if len(before) == 0 {
		t.Fatal("no claim runs a process for the move to relaunch")
	}
	staleBefore := len(r.logs(func(l loggedRecord) bool { return l.Message == stage4bStale }))
	failedBefore := len(r.logs(func(l loggedRecord) bool { return l.Message == stage4bLaunchFailed }))
	r.stopDaemon()
	detail := move()
	r.writeConfig()
	r.boot()
	for token, was := range before {
		var held api.OperatorClaim
		r.await(string(token)+" to run its next generation, registered and ready", 15*time.Minute, func() bool {
			var ok bool
			held, ok = r.liveClaims()[token]
			return ok && held.Generation > was.Generation
		})
		if (held.Locator.Sandbox.PodUID == was.Locator.Sandbox.PodUID) != samePod {
			t.Errorf("%s runs in pod uid %s after the move, was %s; same pod wanted: %v", token, held.Locator.Sandbox.PodUID, was.Locator.Sandbox.PodUID, samePod)
		}
		if held.SessionFile != was.SessionFile {
			t.Errorf("%s's session file moved: %s to %s", token, was.SessionFile, held.SessionFile)
		}
		if held.SessionFile != "" {
			argv, err := r.roleArgv(held.Locator.Sandbox.Name, held.Role, 0, "/opt/omp/bin/omp")
			if err != nil || !slices.Contains(argv, "--resume="+held.SessionFile) {
				t.Errorf("%s's %s runs %q (%v), not --resume=%s", token, held.Role, argv, err, held.SessionFile)
			}
		}
		if held.Budgets.LaunchFailures != 0 || held.Budgets.Deaths != was.Budgets.Deaths {
			t.Errorf("%s's budgets read %+v, want no launch failure and deaths %d", token, held.Budgets, was.Budgets.Deaths)
		}
		stale := r.logs(func(l loggedRecord) bool {
			return l.Message == stage4bStale && fmt.Sprint(l.Attrs["claim"]) == string(token)
		})
		if len(stale) == 0 || !strings.HasSuffix(fmt.Sprint(stale[len(stale)-1].Attrs["detail"]), detail) {
			t.Errorf("%s's stale-address replacement names %+v, want a detail ending %q", token, stale, detail)
		}
		env, err := r.agentEnviron(held.Locator.Sandbox.Name, held.Role)
		if err != nil || kindEnvValue(env, "ENVOY_URL") != r.envoyURL {
			t.Errorf("%s's %s is handed ENVOY_URL %q (%v), want %s", token, held.Role, kindEnvValue(env, "ENVOY_URL"), err, r.envoyURL)
		}
		if pod, err := r.pod(held.Locator.Sandbox.Name); err != nil || pod.connect() != r.stream() {
			t.Errorf("%s's pod's launcher dials %q (%v), want %s", token, pod.connect(), err, r.stream())
		}
	}
	if n := len(r.logs(func(l loggedRecord) bool { return l.Message == stage4bStale })) - staleBefore; n != len(before) {
		t.Errorf("the move logged %d stale-address replacements for %d claims", n, len(before))
	}
	for _, failed := range r.logs(func(l loggedRecord) bool { return l.Message == stage4bLaunchFailed })[failedBefore:] {
		t.Errorf("the daemon logged a failed launch since the move: %+v", failed.Attrs)
	}
	byIssue := map[string]map[string]bool{}
	for _, held := range r.liveClaims() {
		if byIssue[held.Issue] == nil {
			byIssue[held.Issue] = map[string]bool{}
		}
		byIssue[held.Issue][held.Locator.Sandbox.PodUID] = true
	}
	for issue, pods := range byIssue {
		if len(pods) != 1 {
			t.Errorf("issue %s runs its roles in more than one pod: %v", issue, pods)
		}
	}
	r.note("every one of %d claims ran its next generation, %s", len(before), map[bool]string{true: "in its own pod", false: "in a new pod"}[samePod])
}

// address-moved-env-same-pod: the daemon restarted handing the pods another address of the same
// listener, its worker stream and API unmoved: every claim runs its next generation in the same
// issue pod, handed the new ENVOY_URL.
func (r *stage4bLive) addressMovedEnvSamePod() {
	r.addressMoved(func() string {
		old := r.envoyURL
		r.envoyURL = r.listenerAlias
		return fmt.Sprintf("started with addresses a generation started now is not handed: ENVOY_URL %s, now %s", old, r.envoyURL)
	}, true)
}

// address-moved-stream-new-pod: the daemon restarted with its API and worker stream ports swapped:
// every claim runs its next generation in a new pod, whose launcher dials the new stream, and the
// roles of each issue share one pod.
func (r *stage4bLive) addressMovedStreamNewPod() {
	r.addressMoved(func() string {
		old := r.stream()
		r.swapPorts()
		return fmt.Sprintf("holds addresses a pod launched now is not handed: --connect %s, now %s", old, r.stream())
	}, false)
}

// controller: the daemon restarted under `controller: daemon` launches the project controller in
// a pod of its own, which registers; the controller's gh acts as the review App; and the daemon's
// `tick on <project>` reaches the controller's session, naming as open only secrets, with or
// without pool-capacity.
func (r *stage4bLive) controllerCheck() {
	t := r.t
	r.controller = true
	since := time.Now()
	r.restart()
	var state kindState
	r.await("the controller's locator in the state", kindTreeWait, func() bool {
		state = r.state()
		return state.ControllerLocator != nil && state.ControllerLocator.SessionID != ""
	})
	var controllerPod string
	for _, held := range r.claims() {
		if held.Role == claim.RoleController && held.Locator != nil && held.Locator.Sandbox != nil {
			controllerPod = held.Locator.Sandbox.Name
		}
	}
	if controllerPod == "" {
		t.Fatalf("the daemon's claims hold no controller in a pod")
	}
	// The controller's gh run as its agent runs it: an exec into the container carries none of
	// the agent's gh environment (agentGhEnv), which `legion gh` refuses to run without.
	inController := func(command ...string) (string, error) {
		out, err := r.kubectl(append([]string{"exec", controllerPod, "-c", "controller", "--"}, command...)...)
		return strings.TrimSpace(out), err
	}
	viewer, err := agentShellIn(inController, "gh api graphql -f query='{viewer{login}}' --jq .data.viewer.login")
	if err != nil || strings.TrimSpace(viewer) != r.review.Slug+"[bot]" {
		t.Errorf("the controller's gh answered viewer %q (%v), want the review App's %s[bot]", viewer, err, r.review.Slug)
	}
	tick := "tick on " + r.cfg.Project
	r.await("'"+tick+"' to reach the controller's session", 5*time.Minute, func() bool { return len(r.model.requests(since, tick)) > 0 })
	// pi-envoy renders a delivery into the session as TOON, where the tick's list reads
	// `openCapabilities[<n>]: <name>,<name>` on one line (envoy-client's renderInbound), which the
	// request body carries inside a JSON string, so the line ends at its escaped newline.
	open := regexp.MustCompile(`openCapabilities\[(\d+)\]: ([^\\"]*)`)
	lists := 0
	for _, seen := range r.model.requests(since, tick) {
		for _, found := range open.FindAllStringSubmatch(seen.body, -1) {
			lists++
			names := strings.Split(found[2], ",")
			sort.Strings(names)
			if found[1] != strconv.Itoa(len(names)) || (!slices.Equal(names, []string{"secrets"}) && !slices.Equal(names, []string{"pool-capacity", "secrets"})) {
				t.Errorf("a tick delivery carries %q, not openCapabilities of secrets with or without pool-capacity", found[0])
			}
		}
	}
	if lists == 0 {
		t.Errorf("no tick delivery the controller's session sent the model carries an openCapabilities line, where the kind deployment always leaves secrets open")
	}
	r.note("the controller (session %s) runs in pod %s as %s and was told %q, with %d openCapabilities lines read", state.ControllerLocator.SessionID, controllerPod, strings.TrimSpace(viewer), tick, lists)
}

// pod-shape: every issue pod the watch recorded ready, deleted ones included, has the shape: the
// gvisor runtime class, no affinity, every container, the init containers included, requesting
// its CPU and memory as both request and limit and an ephemeral-storage request no greater than
// its limit, so the pod is Guaranteed, and its launcher dialling the stream the run's file named
// then; every pod a live claim runs in is among them; no pod's command, args or environment
// carries a value of its project's Secrets; and a recorded pod made to burst, pinned by an
// affinity or run under another runtime class departs from the shape.
func (r *stage4bLive) podShape() {
	t := r.t
	pods, streams := r.watch.recorded()
	for token, held := range r.liveClaims() {
		if held.Role == claim.RoleController {
			continue
		}
		if _, ok := pods[held.Locator.Sandbox.PodUID]; !ok {
			t.Errorf("the pod watch never recorded %s's pod %s (uid %s)", token, held.Locator.Sandbox.Name, held.Locator.Sandbox.PodUID)
		}
	}
	var last kindPod
	for uid, pod := range pods {
		for _, departure := range stage4bPodShape(pod, streams[uid]) {
			t.Errorf("%s", departure)
		}
		last = pod
	}
	out, err := r.kubectl("get", "secrets", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var secrets struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Data map[string]string `json:"data"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &secrets); err != nil {
		t.Fatal(err)
	}
	judged, hostTokens := 0, 0
	for _, secret := range secrets.Items {
		if !strings.Contains(secret.Metadata.Name, r.project) {
			continue
		}
		for key, encoded := range secret.Data {
			value, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				continue
			}
			for _, credential := range stage4bCredentials(key, string(value)) {
				if len(credential) < 8 {
					continue
				}
				judged++
				if _, hosts := sandbox.GitHubHostsDir(key); hosts {
					hostTokens++
				}
				for _, pod := range pods {
					for _, container := range append(slices.Clone(pod.Spec.Containers), pod.Spec.InitContainers...) {
						text := strings.Join(append(append(slices.Clone(container.Command), container.Args...), stage4bEnvValues(container)...), "\n")
						if strings.Contains(text, credential) {
							t.Errorf("pod %s's container %s carries a credential of Secret %s's %s", pod.Metadata.Name, container.Name, secret.Metadata.Name, key)
						}
					}
				}
			}
		}
	}
	// The reader above must find the App tokens, or the check judges nothing it exists for.
	if hostTokens == 0 {
		t.Errorf("no role Secret of %s held an oauth_token in a gh hosts.yml key: the credential check judged %d values and no GitHub token", r.project, judged)
	}
	stream := streams[last.Metadata.UID]
	bursting := last
	bursting.Spec.Containers = slices.Clone(last.Spec.Containers)
	for i, container := range bursting.Spec.Containers {
		if container.Name == "roles" {
			limits := map[string]string{}
			for k, v := range container.Resources.Limits {
				limits[k] = v
			}
			limits["cpu"] = "4"
			bursting.Spec.Containers[i].Resources.Limits = limits
		}
	}
	bursting.Status.QOSClass = "Burstable"
	if verdict := strings.Join(stage4bPodShape(bursting, stream), "; "); !strings.Contains(verdict, "container roles requests") || !strings.Contains(verdict, "qosClass Burstable") {
		t.Errorf("the pod shape did not refuse a pod whose roles container bursts past its request: %q", verdict)
	}
	pinned := last
	pinned.Spec.Affinity = json.RawMessage(`{"podAffinity":{}}`)
	if verdict := strings.Join(stage4bPodShape(pinned, stream), "; "); !strings.Contains(verdict, "affinity ") {
		t.Errorf("the pod shape did not refuse a pod pinned by an affinity: %q", verdict)
	}
	wrong := last
	wrong.Spec.RuntimeClassName = "runc"
	if len(stage4bPodShape(wrong, stream)) == 0 {
		t.Errorf("the pod shape did not refuse a pod under the runc runtime class")
	}
	r.note("%d issue pods judged from the pod watch's record, deleted ones included; %d Secret credentials held to every container, none carried", len(pods), judged)
}

// stage4bCredentials is what of a claim Secret's key value is a credential, as the devbox proof
// judged it (stage4b-sandbox-tree.sh's check_pod_shape): each oauth_token of a bundle directory's
// hosts.yml (sandbox.GitHubHostsKey), never the rest of that file, which names the host and the
// login; nothing of its config.yml, of the default owner (sandbox.GitHubDefaultKey) or of the
// renames (sandbox.GitHubPairsKey), which name owners and repositories every init container's
// arguments name too; and every other key's whole value: the launcher, boot, provisioning,
// Dispatch and cache tokens.
func stage4bCredentials(key, value string) []string {
	if _, hosts := sandbox.GitHubHostsDir(key); hosts {
		var tokens []string
		for _, line := range strings.Split(value, "\n") {
			if _, token, ok := strings.Cut(line, "oauth_token: "); ok {
				tokens = append(tokens, strings.TrimSpace(token))
			}
		}
		return tokens
	}
	if strings.HasPrefix(key, sandbox.GitHubConfigKey("")) || key == sandbox.GitHubDefaultKey || key == sandbox.GitHubPairsKey {
		return nil
	}
	return []string{value}
}

// stage4bEnvValues is the literal values of container's environment.
func stage4bEnvValues(container kindContainer) []string {
	var values []string
	for _, env := range container.Env {
		values = append(values, env.Value)
	}
	return values
}

// stage4bPodShape is every departure of pod from an issue pod's shape, the launcher handed stream.
func stage4bPodShape(pod kindPod, stream string) []string {
	var departures []string
	name := pod.Metadata.Name
	if pod.Spec.RuntimeClassName != "gvisor" {
		departures = append(departures, fmt.Sprintf("pod %s runs runtime class %q, not gvisor", name, pod.Spec.RuntimeClassName))
	}
	if len(pod.Spec.Affinity) > 0 && string(pod.Spec.Affinity) != "null" && string(pod.Spec.Affinity) != "{}" {
		departures = append(departures, fmt.Sprintf("pod %s carries affinity %s", name, pod.Spec.Affinity))
	}
	for _, container := range append(slices.Clone(pod.Spec.InitContainers), pod.Spec.Containers...) {
		for _, kind := range []string{"cpu", "memory"} {
			if !stage4bSameQuantity(container.Resources.Requests[kind], container.Resources.Limits[kind]) {
				departures = append(departures, fmt.Sprintf("pod %s: container %s requests %s %q, limited to %q", name, container.Name, kind,
					container.Resources.Requests[kind], container.Resources.Limits[kind]))
			}
		}
		request, err1 := resource.ParseQuantity(container.Resources.Requests["ephemeral-storage"])
		limit, err2 := resource.ParseQuantity(container.Resources.Limits["ephemeral-storage"])
		if err1 != nil || err2 != nil || request.Cmp(limit) > 0 {
			departures = append(departures, fmt.Sprintf("pod %s: container %s requests ephemeral-storage %q over its limit %q", name, container.Name,
				container.Resources.Requests["ephemeral-storage"], container.Resources.Limits["ephemeral-storage"]))
		}
	}
	if pod.Status.QOSClass != "Guaranteed" {
		departures = append(departures, fmt.Sprintf("pod %s: qosClass %s, not Guaranteed", name, pod.Status.QOSClass))
	}
	if got := pod.connect(); got != stream {
		departures = append(departures, fmt.Sprintf("pod %s: launcher dials %s, not advertise_host at %s", name, got, stream))
	}
	return departures
}

// stage4bSameQuantity is whether two quantities are set and equal.
func stage4bSameQuantity(a, b string) bool {
	x, err1 := resource.ParseQuantity(a)
	y, err2 := resource.ParseQuantity(b)
	return err1 == nil && err2 == nil && x.Cmp(y) == 0
}
