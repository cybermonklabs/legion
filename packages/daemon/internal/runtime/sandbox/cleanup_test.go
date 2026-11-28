package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

const childIssue = "LEGION-209"

// cleanupRig is two issues, LEGION-208 and its child LEGION-209, as the API holds them: each
// issue's Sandbox, the pod it owns and the disk that pod owns, its own generic ephemeral volume
// (issuePod.disk), and Sandboxes an issue's cleanup must leave alone: another issue's and the
// image probe's. Every Sandbox delete's options are recorded, and a Sandbox's foreground delete
// removes its pod, and with the pod the pod's disk, as garbage collection would.
type cleanupRig struct {
	*rig
	root, child, other, probe string
	// disks are the claims of the two issue pods' own disks (issueDiskName), by Sandbox name.
	disks          map[string]string
	sandboxDeletes map[string]metav1.DeleteOptions
	pvcDeletes     map[string]metav1.DeleteOptions
}

func newCleanupRig(t *testing.T) *cleanupRig {
	t.Helper()
	c := &cleanupRig{
		root: SandboxName(rootToken), child: SandboxName(childToken), other: "legion-legion-legion-300", probe: "legion-probe-cleanup",
		disks:          map[string]string{SandboxName(rootToken): issueDiskName(rootToken), SandboxName(childToken): issueDiskName(childToken)},
		sandboxDeletes: map[string]metav1.DeleteOptions{}, pvcDeletes: map[string]metav1.DeleteOptions{},
	}
	rootLabels := claimLabels(rootSpec(t).Role)
	childLabels := map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: childIssue}
	rootSandbox := sandboxObject(t, c.root, "uid-sandbox-root", modeRunning, rootLabels)
	rootSandbox.SetResourceVersion("sandbox-rv-root")
	childSandbox := sandboxObject(t, c.child, "uid-sandbox-child", modeRunning, childLabels)
	childSandbox.SetResourceVersion("sandbox-rv-child")
	otherSandbox := sandboxObject(t, c.other, "uid-sandbox-other", modeRunning,
		map[string]string{labelProject: testProject, labelTree: "LEGION-300", labelIssue: "LEGION-300"})
	probeSandbox := sandboxObject(t, c.probe, "uid-sandbox-probe", modeRunning,
		map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: testTree, labelProbe: "true"})
	objects := []k8sruntime.Object{rootSandbox, childSandbox, otherSandbox, probeSandbox}
	for sandbox, issue := range map[string]struct {
		uid    types.UID
		labels map[string]string
	}{c.root: {"uid-sandbox-root", rootLabels}, c.child: {"uid-sandbox-child", childLabels}} {
		pod := podObject(sandbox, "uid-pod-"+sandbox, string(issue.uid), issue.labels, runningStatus())
		objects = append(objects, pod, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: c.disks[sandbox], Namespace: testNamespace, UID: "uid-pvc-" + issue.uid, ResourceVersion: "pvc-rv-" + sandbox,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID, Controller: new(true), BlockOwnerDeletion: new(true),
			}},
		}})
	}
	c.rig = newRig(t, objects, withoutController())
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		delete := action.(k8stesting.DeleteAction)
		options := delete.GetDeleteOptions()
		c.sandboxDeletes[delete.GetName()] = options
		disk, owns := c.disks[delete.GetName()]
		if owns && options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationForeground {
			if err := c.kube.Tracker().Delete(podsGVR, testNamespace, delete.GetName()); err != nil && !apierrors.IsNotFound(err) {
				return true, nil, err
			}
			if err := c.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), testNamespace, disk); err != nil && !apierrors.IsNotFound(err) {
				return true, nil, err
			}
		}
		return false, nil, nil
	})
	c.kube.PrependReactor("delete", "persistentvolumeclaims", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		delete := action.(k8stesting.DeleteAction)
		c.pvcDeletes[delete.GetName()] = delete.GetDeleteOptions()
		return false, nil, nil
	})
	c.clearActions()
	return c
}

// deletes are the Sandbox deletes the runtime requested, sorted.
func (c *cleanupRig) deletes() []string {
	var out []string
	for _, a := range c.writes() {
		if a.verb == "delete" {
			out = append(out, a.resource+" "+a.name)
		}
	}
	slices.Sort(out)
	return out
}

// disk is the tracker's copy of the claim of the disk the pod of Sandbox name owns, or nil once
// garbage collection removed it with that pod.
func (c *cleanupRig) disk(sandbox string) *corev1.PersistentVolumeClaim {
	pvc, err := c.kube.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), c.disks[sandbox], metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return pvc
}

// An issue's cleanup deletes the issue's own Sandbox, by the issue's identity alone, with
// foreground propagation and awaited, so garbage collection removes the pod it owns, and with it
// the disk that pod owns, before the Sandbox is absent; the restricted runtime makes no PVC request
// itself. The delete is fenced to the read object's UID and resourceVersion, so a stale cleanup
// cannot delete a replacement. The child's Sandbox, though labelled with the same tree, another
// issue's and the image probe's are left alone (R17 of dispatch://INFRA-3320).
func TestAnIssueCleanupForegroundDeletesOnlyItsOwnSandboxAndItsVolume(t *testing.T) {
	t.Parallel()
	c := newCleanupRig(t)
	if err := c.r.CleanupIssue(c.ctx, testTree); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if got, want := c.deletes(), []string{"sandboxes " + c.root}; !slices.Equal(got, want) {
		t.Fatalf("deletes = %v, want %v", got, want)
	}
	got := c.sandboxDeletes[c.root]
	if got.Preconditions == nil || got.Preconditions.UID == nil || got.Preconditions.ResourceVersion == nil ||
		string(*got.Preconditions.UID) != "uid-sandbox-root" || *got.Preconditions.ResourceVersion != "sandbox-rv-root" {
		t.Fatalf("Sandbox %s delete options = %+v, want UID uid-sandbox-root and resourceVersion sandbox-rv-root", c.root, got)
	}
	if got.PropagationPolicy == nil || *got.PropagationPolicy != metav1.DeletePropagationForeground {
		t.Fatalf("Sandbox %s delete propagation = %v, want foreground", c.root, got.PropagationPolicy)
	}
	if c.sandbox(c.root) != nil || c.disk(c.root) != nil {
		t.Fatalf("after the cleanup Sandbox %s is %+v and its pod's disk %+v, want both gone", c.root, c.sandbox(c.root), c.disk(c.root))
	}
	if len(c.pvcDeletes) != 0 {
		t.Fatalf("the restricted runtime sent PVC delete requests: %+v", c.pvcDeletes)
	}
	for _, kept := range []string{c.child, c.other, c.probe} {
		if c.sandbox(kept) == nil {
			t.Fatalf("the cleanup of %s deleted Sandbox %s", testTree, kept)
		}
	}
	if c.disk(c.child) == nil {
		t.Fatalf("the cleanup of %s took the disk of %s's pod", testTree, childIssue)
	}
}

// A delete the object's change since it was read refused (a status write moved its
// resourceVersion) is no failure: the Sandbox is read again and the delete fenced to what it holds
// now.
func TestAnIssueCleanupReadsAgainAfterAConflict(t *testing.T) {
	t.Parallel()
	c := newCleanupRig(t)
	conflicted := false
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.(k8stesting.DeleteAction).GetName() == c.root && !conflicted {
			conflicted = true
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: sandboxGVR.Group, Resource: sandboxGVR.Resource}, c.root, errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	if err := c.r.CleanupIssue(c.ctx, testTree); err != nil {
		t.Fatalf("cleanup after a conflict: %v", err)
	}
	if !conflicted || c.sandbox(c.root) != nil {
		t.Fatalf("conflicted %t, root Sandbox %+v; want the conflict met and the root deleted after it", conflicted, c.sandbox(c.root))
	}
}

// A failed delete fails the cleanup, leaving the Sandbox and its pod's disk, and the same cleanup
// run again finishes it.
func TestAnIssueCleanupThatFailedFinishesOnItsRetry(t *testing.T) {
	t.Parallel()
	c := newCleanupRig(t)
	failed := false
	c.dyn.PrependReactor("delete", "sandboxes", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.(k8stesting.DeleteAction).GetName() == c.root && !failed {
			failed = true
			return true, nil, errors.New("transient Sandbox API failure")
		}
		return false, nil, nil
	})
	if err := c.r.CleanupIssue(c.ctx, testTree); err == nil || !strings.Contains(err.Error(), "transient Sandbox API failure") {
		t.Fatalf("first cleanup = %v, want its transient delete failure", err)
	}
	if c.sandbox(c.root) == nil || c.disk(c.root) == nil {
		t.Fatalf("after the failure: root %+v, its pod's disk %+v; want both kept", c.sandbox(c.root), c.disk(c.root))
	}
	c.clearActions()
	if err := c.r.CleanupIssue(c.ctx, testTree); err != nil {
		t.Fatalf("retried cleanup: %v", err)
	}
	if got, want := c.deletes(), []string{"sandboxes " + c.root}; !slices.Equal(got, want) {
		t.Fatalf("retry deletes = %v, want %v", got, want)
	}
	if c.sandbox(c.root) != nil || c.disk(c.root) != nil {
		t.Fatalf("after the retry Sandbox %s is %+v and its pod's disk %+v, want both gone", c.root, c.sandbox(c.root), c.disk(c.root))
	}
}

// An issue the API holds no Sandbox of, as one whose close deleted it already, is released at
// once: nothing is written.
func TestAnIssueCleanupOfAnIssueWithNoSandboxWritesNothing(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil, withoutController())
	g.clearActions()
	if err := g.r.CleanupIssue(g.ctx, testTree); err != nil {
		t.Fatalf("cleanup of an issue with no Sandbox: %v", err)
	}
	if writes := g.writes(); len(writes) != 0 {
		t.Fatalf("an issue with no Sandbox wrote %+v", writes)
	}
}

// A Sandbox by the issue's name that is labelled with another issue is not this issue's: the
// cleanup deletes nothing and says so.
func TestAnIssueCleanupLeavesASandboxLabelledForAnotherIssue(t *testing.T) {
	t.Parallel()
	name := SandboxName(rootToken)
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-foreign", modeSuspended,
		map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: childIssue})}, withoutController())
	g.clearActions()
	if err := g.r.CleanupIssue(g.ctx, testTree); err == nil || !strings.Contains(err.Error(), "does not belong to this issue") {
		t.Fatalf("cleanup over another issue's Sandbox = %v, want a refusal naming it", err)
	}
	if g.sandbox(name) == nil {
		t.Fatalf("the cleanup of %s deleted a Sandbox labelled for %s", testTree, childIssue)
	}
}

// The sweep keeps an issue Sandbox while its issue's own lifecycle is live, whatever tree its
// label names — a child that was its parent's tree at its pod's creation and has another gate root
// now keeps its pod (R17 of dispatch://INFRA-3320) — and deletes it once the issue's own cleanup
// confirmed, as every other orphan is deleted.
func TestTheSweepKeepsAnIssuesSandboxWhileItsOwnLifecycleIsLive(t *testing.T) {
	t.Parallel()
	name := "legion-legion-legion-209"
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-sandbox-child", modeSuspended,
		map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: childIssue})}, withoutController())
	g.store.close(testTree)
	if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if s := g.sandbox(name); s == nil || s.UID != "uid-sandbox-child" {
		t.Fatalf("the sweep deleted the Sandbox of %s, whose own lifecycle is live, once the tree its label names closed", childIssue)
	}
	g.store.close(childIssue)
	if err := g.r.ReconcileOrphans(g.ctx, nil, 0); err != nil {
		t.Fatal(err)
	}
	if s := g.sandbox(name); s != nil {
		t.Fatalf("the Sandbox of %s = %+v, want it swept once the issue's own cleanup confirmed", childIssue, s)
	}
}
