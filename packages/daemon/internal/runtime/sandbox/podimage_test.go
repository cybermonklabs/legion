package sandbox

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// podImageNames are the three answers of IssuePodImage as a failure prints them.
var podImageNames = map[runtime.PodImage]string{
	runtime.PodImageUnread:     "unread",
	runtime.PodImageConfigured: "configured",
	runtime.PodImageOther:      "other",
}

// The idle release reads an issue pod's image (IssuePodImage) before it releases the pod, and the
// read answers one of three. A pod that the issue's Sandbox owns and that has not ended answers
// the configured image when every container of it, init containers included, runs the image the
// runtime launches with, and another image when one container runs any other. The release takes
// a pod of either answer once it is idle. Every read that sees no image answers unread, which
// holds the pod, and each of its six cases is one row here: no Sandbox, no pod, a foreign pod, an
// ended pod, a key no Sandbox name can be built from, and a failed store read.
//
// Each row is a fresh rig with the worker of testTree launched on the configured image, changed in
// one way, and it waits until the runtime's informer stores hold that change before it reads.
// In the row of no Sandbox and the row of the undecodable Sandbox the pod stays in the store, on
// the configured image, so a read that looked at the pod alone would answer configured there.
//
// The store answers the read through storedSandbox, whose error is the store's GetByKey's or the
// Sandbox's decode's. The informer's store never answers GetByKey with an error, so a test cannot
// make that failure; a Sandbox that the daemon cannot decode is the failed read this test can make.
func TestTheImageReadAnswersThreeWays(t *testing.T) {
	images := func(init, own string) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			for i := range p.Spec.InitContainers {
				p.Spec.InitContainers[i].Image = init
			}
			for i := range p.Spec.Containers {
				p.Spec.Containers[i].Image = own
			}
		}
	}
	runs := func(init, own string) func(*corev1.Pod) bool {
		return func(p *corev1.Pod) bool {
			if p == nil || len(p.Spec.InitContainers) == 0 || len(p.Spec.Containers) == 0 {
				return false
			}
			for _, c := range p.Spec.InitContainers {
				if c.Image != init {
					return false
				}
			}
			for _, c := range p.Spec.Containers {
				if c.Image != own {
					return false
				}
			}
			return true
		}
	}
	for _, tc := range []struct {
		name string
		// issue is the key the read asks for: testTree where it is empty.
		issue string
		// change is what the row does to the launched pod or its Sandbox, named name, and waits
		// for in the runtime's stores; nil leaves both as the launch made them.
		change func(t *testing.T, g *rig, name string)
		want   runtime.PodImage
	}{
		{
			name: "every container of the pod runs the configured image",
			change: func(t *testing.T, g *rig, name string) {
				g.eventually("the pod store to hold the launched pod on the configured image", func() bool {
					return runs(testImage, testImage)(g.r.storedPod(name))
				})
			},
			want: runtime.PodImageConfigured,
		},
		{
			name: "every container of the pod runs another image",
			change: func(t *testing.T, g *rig, name string) {
				g.update(g.pod(name), images(nextImage, nextImage))
				g.eventually("the pod store to hold the pod on another image", func() bool {
					return runs(nextImage, nextImage)(g.r.storedPod(name))
				})
			},
			want: runtime.PodImageOther,
		},
		{
			name: "an init container alone runs another image",
			change: func(t *testing.T, g *rig, name string) {
				g.update(g.pod(name), images(nextImage, testImage))
				g.eventually("the pod store to hold the pod with its init container on another image", func() bool {
					return runs(nextImage, testImage)(g.r.storedPod(name))
				})
			},
			want: runtime.PodImageOther,
		},
		{
			name: "unread: the issue holds no Sandbox",
			change: func(t *testing.T, g *rig, name string) {
				if err := g.dyn.Tracker().Delete(sandboxGVR, testNamespace, name); err != nil {
					t.Fatal(err)
				}
				g.eventually("the sandbox store to see the Sandbox gone, with its pod still there", func() bool {
					s, err := g.r.storedSandbox(name)
					return err == nil && s == nil && runs(testImage, testImage)(g.r.storedPod(name))
				})
			},
			want: runtime.PodImageUnread,
		},
		{
			name: "unread: the Sandbox holds no pod",
			change: func(t *testing.T, g *rig, name string) {
				g.hold.Store(true)
				if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
					t.Fatal(err)
				}
				g.eventually("the pod store to see the pod gone", func() bool { return g.r.storedPod(name) == nil })
			},
			want: runtime.PodImageUnread,
		},
		{
			name: "unread: the pod of the Sandbox's name is a pod the Sandbox does not own",
			change: func(t *testing.T, g *rig, name string) {
				const foreign = types.UID("uid-sandbox-of-another-launch")
				g.update(g.pod(name), func(p *corev1.Pod) { p.OwnerReferences[0].UID = foreign })
				g.eventually("the pod store to hold the pod under another owner", func() bool {
					p := g.r.storedPod(name)
					return p != nil && len(p.OwnerReferences) > 0 && p.OwnerReferences[0].UID == foreign
				})
			},
			want: runtime.PodImageUnread,
		},
		{
			name: "unread: the pod has ended",
			change: func(t *testing.T, g *rig, name string) {
				g.editPod(name, "the pod store to hold the pod failed", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
			},
			want: runtime.PodImageUnread,
		},
		{
			name: "unread: no Sandbox name can be built from the key", issue: "not-an-issue-key",
			want: runtime.PodImageUnread,
		},
		{
			name: "unread: the store read of the Sandbox fails, through storedSandbox, on a Sandbox the daemon cannot decode",
			change: func(t *testing.T, g *rig, name string) {
				object, err := g.dyn.Tracker().Get(sandboxGVR, testNamespace, name)
				if err != nil {
					t.Fatal(err)
				}
				undecodable := object.(*unstructured.Unstructured).DeepCopy()
				if err := unstructured.SetNestedField(undecodable.Object, int64(7), "spec", "operatingMode"); err != nil {
					t.Fatal(err)
				}
				if err := g.dyn.Tracker().Update(sandboxGVR, undecodable, testNamespace); err != nil {
					t.Fatal(err)
				}
				g.eventually("the sandbox store to hold a Sandbox it cannot decode, with its pod still there", func() bool {
					_, err := g.r.storedSandbox(name)
					return err != nil && runs(testImage, testImage)(g.r.storedPod(name))
				})
			},
			want: runtime.PodImageUnread,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil)
			loc := g.spawn(workerSpec(t))
			if tc.change != nil {
				tc.change(t, g, loc.Sandbox.Name)
			}
			issue := tc.issue
			if issue == "" {
				issue = testTree
			}
			if got := g.r.IssuePodImage(issue); got != tc.want {
				t.Fatalf("IssuePodImage(%q) = %s, want %s", issue, podImageNames[got], podImageNames[tc.want])
			}
		})
	}
}
