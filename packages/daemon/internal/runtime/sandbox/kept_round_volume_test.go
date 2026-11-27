package sandbox

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// When a kept pod goes before its update round is over, the issue is told that the work its roles
// did not push is still on the kept volume, and that the next pod of the issue mounts the volume
// (podWentMessage, from IssueKeepsVolume). That is true only where the issue's next launch keeps
// the Sandbox. A launch into a Sandbox of another disk shape deletes it with its claim (R3, "a
// launch into a Sandbox of the other disk shape"): after a change of keep_volume, of
// microvm.storage_class or of issue_volume, each of which is also a reason for the round itself.
// A Sandbox that carries the reset mark is deleted by the next launch too (R6). In each case
// IssueKeepsVolume answers false, so the issue is told that the work its roles did not push is lost.
func TestIssueKeepsVolumeIsFalseWhereTheNextLaunchReplacesTheVolume(t *testing.T) {
	t.Parallel()
	name := SandboxName(childToken)
	// launched is a kept rig with the child's first pod launched, and a time after its Sandbox was
	// made, which is a time a round over that pod can have opened at.
	launched := func(t *testing.T) (*rig, time.Time) {
		t.Helper()
		g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)))
		g.spawn(childSpec(t))
		opened := g.sandbox(name).CreationTimestamp.Add(time.Second)
		if !g.r.IssueKeepsVolume(childIssue, opened) {
			t.Fatal("IssueKeepsVolume of the kept Sandbox = false before any change, want true")
		}
		return g, opened
	}
	for _, tc := range []struct {
		name string
		move func(*Options)
	}{
		{"keep_volume turned off", func(o *Options) {
			o.Sandbox = SandboxConfig{Type: SandboxMicroVM, MicroVM: testMicroVM()}
		}},
		{"issue_volume grown", func(o *Options) {
			keptVolumes(SandboxMicroVM)(o)
			o.IssueVolume = resource.MustParse("100Gi")
		}},
		{"microvm.storage_class moved", func(o *Options) {
			keptVolumes(SandboxMicroVM)(o)
			o.Sandbox.MicroVM.StorageClass = "another-microvm-class"
		}},
		{"canary_issues moved the issue to gVisor", func(o *Options) {
			keptVolumes(SandboxMicroVM, childIssue)(o)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, opened := launched(t)
			r := secondRuntime(t, g, tc.move)()
			if r.IssueKeepsVolume(childIssue, opened) {
				t.Fatalf("IssueKeepsVolume = true after %s, want false: the issue's next launch replaces the Sandbox and deletes its kept volume", tc.name)
			}
		})
	}
	t.Run("the reset mark on the Sandbox", func(t *testing.T) {
		g, opened := launched(t)
		object, err := g.dyn.Tracker().Get(sandboxGVR, testNamespace, name)
		if err != nil {
			t.Fatal(err)
		}
		marked := object.(*unstructured.Unstructured).DeepCopy()
		annotations := marked.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[volumeResetAnnotation] = initContainer + " exited 1 in pod uid-pod-failed"
		marked.SetAnnotations(annotations)
		if err := g.dyn.Tracker().Update(sandboxGVR, marked, testNamespace); err != nil {
			t.Fatal(err)
		}
		g.eventually("the Sandbox store to see the reset mark", func() bool {
			stored, err := g.r.storedSandbox(name)
			return err == nil && stored != nil && stored.Annotations[volumeResetAnnotation] != ""
		})
		if g.r.IssueKeepsVolume(childIssue, opened) {
			t.Fatal("IssueKeepsVolume = true for a Sandbox that carries the reset mark, want false: the issue's next launch resets the volume")
		}
	})
}
