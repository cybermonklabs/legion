package sandbox

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// wrongTree is the tree every tree label is set to in the second run of each decision below: a
// tree no issue of the rig stands in.
const wrongTree = "LEGION-999"

// Every decision of the runtime that reads a Sandbox's or a pod's labels decides the same whatever
// the tree label says (decision 14 of dispatch://INFRA-4730, R17 of dispatch://INFRA-3320): the
// label is the gate root at the pod's creation and no decision reads it. Each decision runs twice
// over the same Sandboxes and pods, once with every tree label as the launch wrote it and once
// with every one set to another tree, and its outcome — what it wrote, what it answered, and what
// stands after it — is the same. This is k45's half in the runtime; internal/daemon's test of this
// name is its half over the daemon's decisions.
func TestEveryDecisionIsTheSameWithEveryStoredTreeWrong(t *testing.T) {
	t.Parallel()
	authorized := func(context.Context) (bool, error) { return true, nil }
	decisions := []struct {
		name   string
		decide func(t *testing.T, tree string) string
	}{
		{"the issue's own cleanup", func(t *testing.T, tree string) string {
			c := newCleanupRig(t)
			relabelTrees(t, c.rig, tree)
			err := c.r.CleanupIssue(c.ctx, childIssue)
			return fmt.Sprintf("error %v; deletes %v; left %v", err, c.deletes(), standing(c.rig, c.root, c.child, c.other, c.probe))
		}},
		{"the issue's suspension", func(t *testing.T, tree string) string {
			g := newRig(t, nil)
			g.spawn(childSpec(t))
			relabelTrees(t, g, tree)
			err := g.r.SuspendIssue(g.ctx, childIssue, "", authorized)
			return fmt.Sprintf("error %v; writes %v; left %v", err, writeNames(g), standing(g, SandboxName(childToken)))
		}},
		{"a launch into a running pod (ensureSandbox)", func(t *testing.T, tree string) string {
			g := newRig(t, nil)
			first := g.spawn(rootSpec(t))
			relabelTrees(t, g, tree)
			second := g.spawn(workerSpec(t))
			return fmt.Sprintf("writes %v; same pod %t; left %v", writeNames(g), first.Sandbox.PodUID == second.Sandbox.PodUID,
				standing(g, SandboxName(rootToken)))
		}},
		{"the pod-kind test", func(t *testing.T, tree string) string {
			var out []string
			for _, labels := range []map[string]string{claimLabels(claim.RoleTester), claimLabels(claim.RoleController),
				{labelProject: testProject, labelTree: testTree}} {
				if _, has := labels[labelTree]; has {
					labels[labelTree] = labelValue(tree)
				}
				kind, err := podKindOf(labels)
				out = append(out, fmt.Sprintf("%T %v", kind, err))
			}
			return strings.Join(out, "; ")
		}},
		{"the cache-pod test", func(t *testing.T, tree string) string {
			g := newRig(t, nil)
			if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
				t.Fatal(err)
			}
			object, err := g.dyn.Tracker().Get(sandboxGVR, testNamespace, CacheSandboxName(testProject, cacheRepo))
			if err != nil {
				t.Fatal(err)
			}
			cache := object.(*unstructured.Unstructured).DeepCopy()
			labels := cache.GetLabels()
			labels[labelTree] = labelValue(tree)
			cache.SetLabels(labels)
			return fmt.Sprintf("shape %q", cacheSandboxShape(*cache))
		}},
		{"the orphan reconcile", func(t *testing.T, tree string) string {
			orphan := claim.Token("legion-legion-legion-9-planner")
			g := newRig(t, []k8sruntime.Object{
				sandboxObject(t, SandboxName(orphan), "uid-sandbox-orphan", modeSuspended,
					map[string]string{labelProject: testProject, labelTree: "LEGION-9", labelIssue: "LEGION-9"}),
				sandboxObject(t, SandboxName(rootToken), "uid-sandbox-root", modeSuspended, claimLabels(claim.RoleArchitect)),
				sandboxObject(t, SandboxName(childToken), "uid-sandbox-child", modeSuspended,
					map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: childIssue}),
			})
			g.store.close("LEGION-9")
			// The first run keeps every label as the launch wrote it, the orphan's its own closed
			// tree, so a sweep that read the label would see a closed tree there and a live one in
			// the second run.
			if tree == testTree {
				g.clearActions()
			} else {
				relabelTrees(t, g, tree)
			}
			g.now.Store(new(g.sandbox(SandboxName(orphan)).CreationTimestamp.Add(3 * time.Minute)))
			err := g.r.ReconcileOrphans(g.ctx, nil, 2*time.Minute)
			return fmt.Sprintf("error %v; writes %v; left %v", err, writeNames(g),
				standing(g, SandboxName(orphan), SandboxName(rootToken), SandboxName(childToken)))
		}},
	}
	for _, decision := range decisions {
		t.Run(decision.name, func(t *testing.T) {
			right := decision.decide(t, testTree)
			wrong := decision.decide(t, wrongTree)
			if right != wrong {
				t.Fatalf("with every tree label right the decision is\n  %s\nand with every one set to %s it is\n  %s\nwant the same: no decision reads the tree label",
					right, wrongTree, wrong)
			}
		})
	}
}

// relabelTrees sets the tree label of every Sandbox, of each Sandbox's pod template and of every
// pod g's API holds to tree, where it carries one, and waits until the runtime's own informers
// hold the Sandboxes and the pods they held as relabelled, so the decision that follows reads them
// that way. The writes go to the trackers directly, and the requests recorded so far are cleared,
// so what the decision writes is all the rig records after it.
func relabelTrees(t *testing.T, g *rig, tree string) {
	t.Helper()
	sandboxes, err := g.dyn.Resource(sandboxGVR).Namespace(testNamespace).List(g.ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range sandboxes.Items {
		u := sandboxes.Items[i].DeepCopy()
		if labels := u.GetLabels(); labels[labelTree] != "" {
			labels[labelTree] = labelValue(tree)
			u.SetLabels(labels)
			names = append(names, u.GetName())
		}
		if template, found, _ := unstructured.NestedStringMap(u.Object, "spec", "podTemplate", "metadata", "labels"); found && template[labelTree] != "" {
			template[labelTree] = labelValue(tree)
			if err := unstructured.SetNestedStringMap(u.Object, template, "spec", "podTemplate", "metadata", "labels"); err != nil {
				t.Fatal(err)
			}
		}
		if err := g.dyn.Tracker().Update(sandboxGVR, u, testNamespace); err != nil {
			t.Fatal(err)
		}
	}
	pods, err := g.kube.CoreV1().Pods(testNamespace).List(g.ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var podNames []string
	for i := range pods.Items {
		pod := pods.Items[i].DeepCopy()
		if pod.Labels[labelTree] == "" {
			continue
		}
		if _, held, _ := g.r.pods.GetStore().GetByKey(testNamespace + "/" + pod.Name); held {
			podNames = append(podNames, pod.Name)
		}
		pod.Labels[labelTree] = labelValue(tree)
		if err := g.kube.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
			t.Fatal(err)
		}
	}
	g.eventually("the runtime's informers to hold every Sandbox and pod relabelled", func() bool {
		for _, name := range names {
			obj, ok, err := g.r.sandboxes.GetStore().GetByKey(testNamespace + "/" + name)
			if err != nil || !ok || obj.(*unstructured.Unstructured).GetLabels()[labelTree] != labelValue(tree) {
				return false
			}
		}
		for _, name := range podNames {
			obj, ok, err := g.r.pods.GetStore().GetByKey(testNamespace + "/" + name)
			if err != nil || !ok || obj.(*corev1.Pod).Labels[labelTree] != labelValue(tree) {
				return false
			}
		}
		return true
	})
	g.clearActions()
}

// writeNames are g's writes as "verb resource name", the verbs and names a decision chose, with
// no body: a body may carry the labels the run set.
func writeNames(g *rig) []string {
	var out []string
	for _, w := range g.writes() {
		out = append(out, strings.TrimSpace(w.verb+" "+w.resource+" "+w.name))
	}
	return out
}

// standing is which of names the API still holds as a Sandbox, each with its mode, in names' order.
func standing(g *rig, names ...string) []string {
	var out []string
	for _, name := range names {
		if s := g.sandbox(name); s != nil && s.DeletionTimestamp == nil {
			out = append(out, name+"="+s.Spec.OperatingMode)
		}
	}
	return out
}
