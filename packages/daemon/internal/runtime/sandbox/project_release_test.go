package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// heldRig is a rig whose runtime reads held as the daemon's project release mark
// (Options.ProjectReleaseHolds).
func heldRig(t *testing.T) (*rig, *atomic.Bool) {
	t.Helper()
	held := &atomic.Bool{}
	g := newRig(t, nil, withOptions(func(o *Options) { o.ProjectReleaseHolds = held.Load }))
	return g, held
}

// madePods is every write since the last clearActions that would make a pod of the project: a
// Sandbox created, or one patched to Running.
func madePods(g *rig) []string {
	var made []string
	for _, w := range g.writes() {
		switch {
		case w.verb == "create" && w.resource == "sandboxes":
			made = append(made, w.String())
		case w.verb == "patch" && w.resource == "sandboxes" && strings.Contains(w.patch, `"`+modeRunning+`"`):
			made = append(made, w.String())
		}
	}
	return made
}

// wantReleased holds err to the runtime's refusal under the project release, and the rig to no
// write that makes a pod.
func wantReleased(t *testing.T, g *rig, what string, err error) {
	t.Helper()
	if !errors.Is(err, runtime.ErrProjectReleased) {
		t.Fatalf("%s while the project release holds = %v, want runtime.ErrProjectReleased", what, err)
	}
	if made := madePods(g); len(made) != 0 {
		t.Fatalf("%s while the project release holds wrote %v, want nothing that makes a pod", what, made)
	}
}

// dispatch://INFRA-3583 R7: while the daemon's project release holds every pod, the Sandbox
// runtime makes none, whatever path asks — a launch into an issue with no Sandbox, a relaunch into
// a Suspended one, a repository cache made or its pod made again, an image probe, a pod update
// round's replacement — and each refuses with runtime.ErrProjectReleased before it writes. With
// the mark clear the same act goes through.
func TestProjectReleaseHoldsEverySandboxTheRuntimeWouldMake(t *testing.T) {
	t.Run("a launch into an issue with no sandbox", func(t *testing.T) {
		g, held := heldRig(t)
		held.Store(true)
		g.clearActions()
		_, err := g.r.Spawn(g.ctx, workerSpec(t))
		wantReleased(t, g, "the launch", err)
		held.Store(false)
		if loc := g.spawn(workerSpec(t)); g.sandbox(loc.Sandbox.Name) == nil {
			t.Fatal("the launch with the mark clear made no Sandbox")
		}
	})

	t.Run("a relaunch into a suspended sandbox", func(t *testing.T) {
		g, held := heldRig(t)
		spec := workerSpec(t)
		loc := g.spawn(spec)
		name := loc.Sandbox.Name
		if err := g.r.setMode(g.ctx, g.sandbox(name), modeSuspended); err != nil {
			t.Fatal(err)
		}
		g.eventually("the suspended Sandbox's pod to go", func() bool { return g.pod(name) == nil })
		held.Store(true)
		g.clearActions()
		// The next generation of the same role, as a relaunch after the pod's suspension starts it.
		spec.Generation, spec.BootToken = 2, "boot-"+string(spec.Claim)+"-g2"
		_, err := g.r.Spawn(g.ctx, spec)
		wantReleased(t, g, "the relaunch", err)
		if mode := g.sandbox(name).mode(); mode != modeSuspended {
			t.Fatalf("the refused relaunch left the Sandbox %s, want it suspended", mode)
		}
		held.Store(false)
		if _, err := g.r.Spawn(g.ctx, spec); err != nil {
			t.Fatalf("the relaunch with the mark clear: %v", err)
		}
		if mode := g.sandbox(name).mode(); mode != modeRunning {
			t.Fatalf("the relaunch with the mark clear left the Sandbox %s, want it running", mode)
		}
	})

	t.Run("a launch of another role into a kept pod", func(t *testing.T) {
		g, held := heldRig(t)
		loc := g.spawn(workerSpec(t))
		name := loc.Sandbox.Name
		spec := roleSpec(t, claim.RoleImplementer)
		g.launcher(spec.Claim)
		held.Store(true)
		g.clearActions()
		_, err := g.r.Spawn(g.ctx, spec)
		wantReleased(t, g, "the launch into the kept pod", err)
		started := func() bool { return countOf(g.sent(spec.Claim), shimwire.TypeLauncherStart) > 0 }
		if started() {
			t.Fatal("the launch into the kept pod started its role while the project release holds")
		}
		if mode := g.sandbox(name).mode(); mode != modeRunning {
			t.Fatalf("the refused launch left the Sandbox %s, want it running", mode)
		}
		held.Store(false)
		if _, err := g.r.Spawn(g.ctx, spec); err != nil {
			t.Fatalf("the launch with the mark clear: %v", err)
		}
		if !started() {
			t.Fatal("the launch with the mark clear sent its role's launcher no start")
		}
	})

	t.Run("a repository cache", func(t *testing.T) {
		g, held := heldRig(t)
		held.Store(true)
		g.clearActions()
		wantReleased(t, g, "the cache", g.r.EnsureRepositoryCache(g.ctx, cacheRepo))
		held.Store(false)
		if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
			t.Fatalf("the cache with the mark clear: %v", err)
		}
		if g.sandbox(CacheSandboxName(testProject, cacheRepo)) == nil {
			t.Fatal("the cache with the mark clear made no Sandbox")
		}
	})

	t.Run("a repository cache's pod made again", func(t *testing.T) {
		g, held := heldRig(t)
		if err := g.r.EnsureRepositoryCache(g.ctx, cacheRepo); err != nil {
			t.Fatal(err)
		}
		name := CacheSandboxName(testProject, cacheRepo)
		if err := g.r.setMode(g.ctx, g.sandbox(name), modeSuspended); err != nil {
			t.Fatal(err)
		}
		g.eventually("the suspended cache's pod to go", func() bool { return g.pod(name) == nil })
		held.Store(true)
		g.clearActions()
		wantReleased(t, g, "the cache pod made again", g.r.reconcileCachePod(g.ctx, g.sandbox(name), cacheRepo))
		held.Store(false)
		if err := g.r.reconcileCachePod(g.ctx, g.sandbox(name), cacheRepo); err != nil {
			t.Fatalf("the cache pod made again with the mark clear: %v", err)
		}
		if mode := g.sandbox(name).mode(); mode != modeRunning {
			t.Fatalf("the cache pod made again with the mark clear left its Sandbox %s, want it running", mode)
		}
	})

	t.Run("an image probe", func(t *testing.T) {
		g, held := heldRig(t)
		const name = "legion-probe-project-release"
		manifest := func() (*unstructured.Unstructured, error) {
			u := &unstructured.Unstructured{}
			u.SetAPIVersion(sandboxGVR.GroupVersion().String())
			u.SetKind("Sandbox")
			u.SetNamespace(testNamespace)
			u.SetName(name)
			u.SetLabels(map[string]string{labelProject: testProject})
			return u, nil
		}
		held.Store(true)
		g.clearActions()
		_, outcome, created := g.r.createProbe(g.ctx, name, "sha256:probe", manifest, time.Second)
		if created {
			t.Fatal("the probe Sandbox was created while the project release holds")
		}
		wantReleased(t, g, "the probe", outcome.Refusal)
		held.Store(false)
		if _, outcome, created := g.r.createProbe(g.ctx, name, "sha256:probe", manifest, time.Second); !created {
			t.Fatalf("the probe Sandbox with the mark clear was not created: %+v", outcome)
		}
	})

	t.Run("a pod update round's replacement", func(t *testing.T) {
		g, held := heldRig(t)
		loc := g.spawn(workerSpec(t))
		name := loc.Sandbox.Name
		known := runtime.Known{Claim: workerToken, Locator: &loc}
		held.Store(true)
		asked := false
		err := g.r.ReplacePod(g.ctx, known, func(context.Context) error {
			asked = true
			return nil
		})
		if !errors.Is(err, runtime.ErrProjectReleased) || asked {
			t.Fatalf("the replacement while the project release holds = %v (asked whether due: %t), want the refusal before anything", err, asked)
		}
		if mode := g.sandbox(name).mode(); mode != modeRunning {
			t.Fatalf("the refused replacement left the Sandbox %s, want it running", mode)
		}
		held.Store(false)
		if err := g.r.ReplacePod(g.ctx, known, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("the replacement with the mark clear: %v", err)
		}
		if mode := g.sandbox(name).mode(); mode != modeSuspended {
			t.Fatalf("the replacement with the mark clear left the Sandbox %s, want it suspended", mode)
		}
	})
}

// A project release's delete of the controller's Sandbox, or of a repository cache's, refuses a
// Sandbox written since its delete was decided, where the runtime's own deletes keep it for the
// next orphan sweep and answer nil: the release holds that sweep until the daemon's next boot, and
// the sweep takes no cache at all, so a Sandbox kept so would stand while the release recorded it
// deleted. The refusal names the Sandbox kept, and the Sandbox stays; the same delete of a Sandbox
// unwritten since goes through.
func TestAProjectReleaseDeleteRefusesASandboxWrittenSinceItWasRead(t *testing.T) {
	cache := CacheSandboxName(testProject, cacheRepo)
	for name, tc := range map[string]struct {
		sandbox  string
		objects  func(t *testing.T) []k8sruntime.Object
		start    func(t *testing.T, g *rig)
		deleteIt func(g *rig) error
	}{
		"the controller's": {
			sandbox: SandboxName(controllerToken),
			objects: func(*testing.T) []k8sruntime.Object { return nil },
			// The controller's delete is fenced to the store's copy, so the test waits for the store
			// to hold the Sandbox as the API does before the delete is asked.
			start: func(t *testing.T, g *rig) {
				g.spawn(controllerSpec(t))
				setResourceVersion(g, SandboxName(controllerToken), "rv-read")
				g.eventually("the store to hold the Sandbox as read", func() bool { return storedVersion(g, SandboxName(controllerToken)) == "rv-read" })
			},
			deleteIt: func(g *rig) error { return g.r.ReleaseProjectController(g.ctx, controllerToken) },
		},
		"a repository cache's": {
			sandbox: cache,
			objects: func(t *testing.T) []k8sruntime.Object {
				return []k8sruntime.Object{cacheObject(t, cacheRepo, "uid-cache", modeRunning, "")}
			},
			start:    func(*testing.T, *rig) {},
			deleteIt: func(g *rig) error { return g.r.DeleteProjectSandbox(g.ctx, cache) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			var written atomic.Bool
			var g *rig
			hooks := &sandboxHooks{
				beforeDelete: func(name string) {
					if name == tc.sandbox && written.CompareAndSwap(true, false) {
						setResourceVersion(g, name, "rv-written")
					}
				},
				afterGet: func(string) {},
			}
			g = newRig(t, tc.objects(t), withSandboxHooks(hooks))
			g.dyn.PrependReactor("delete", "sandboxes", fenceSandboxDeletes(g))
			tc.start(t, g)

			written.Store(true)
			if err := tc.deleteIt(g); !errors.Is(err, errWrittenSinceRead) || !strings.Contains(err.Error(), tc.sandbox) {
				var stands string
				if s := g.sandbox(tc.sandbox); s != nil {
					stands = fmt.Sprintf("rv %q, deletion %v, labels %v", s.ResourceVersion, s.DeletionTimestamp, s.Labels)
				}
				t.Fatalf("the release's delete of a Sandbox written since it was read = %v, want the refusal naming %s (delete asked: %t; the Sandbox: %s; writes %v)",
					err, tc.sandbox, !written.Load(), stands, g.writes())
			}
			if written.Load() {
				t.Fatal("the Sandbox was never asked to be deleted")
			}
			if g.sandbox(tc.sandbox) == nil {
				t.Fatal("a Sandbox written since the release's delete was decided was deleted")
			}

			g.eventually("the store to hold the Sandbox as written", func() bool { return storedVersion(g, tc.sandbox) == "rv-written" })
			if err := tc.deleteIt(g); err != nil {
				t.Fatalf("the release's delete of a Sandbox unwritten since it was read: %v", err)
			}
			if s := g.sandbox(tc.sandbox); s != nil {
				t.Fatalf("the Sandbox %s, unwritten since the second delete read it, is still there", s.Name)
			}
		})
	}
}

// dispatch://INFRA-3583 R6: a project release lists the project's Sandboxes once more before it
// takes the controller and the caches, from the API server (ProjectSandboxesFromAPI), since a
// create the runtime sent just before the release's mark can land after the release's first list,
// which reads the informer's store. That list holds a Sandbox the store has not heard of yet, told
// by its labels as the store's list tells it, and leaves out one already being deleted and one of
// another project.
func TestProjectSandboxesFromAPIListsASandboxTheStoreHasNotHeardOf(t *testing.T) {
	g := newRig(t, nil, withoutController(), withLaggingSandboxInformer(time.Minute))
	late := sandboxObject(t, "legion-legion-legion-300", "uid-late", modeSuspended,
		map[string]string{labelProject: testProject, labelTree: "LEGION-300", labelIssue: "LEGION-300"})
	cache := sandboxObject(t, "legion-legion-cache-late", "uid-cache", modeSuspended,
		map[string]string{labelProject: testProject, labelCache: cacheMarker})
	going := sandboxObject(t, "legion-legion-legion-301", "uid-going", modeSuspended,
		map[string]string{labelProject: testProject, labelTree: "LEGION-301", labelIssue: "LEGION-301"})
	going.SetDeletionTimestamp(&metav1.Time{Time: rigNow})
	other := sandboxObject(t, "legion-widgets-widgets-1", "uid-other", modeSuspended,
		map[string]string{labelProject: "widgets", labelTree: "WIDGETS-1", labelIssue: "WIDGETS-1"})
	for _, object := range []*unstructured.Unstructured{late, cache, going, other} {
		if err := g.dyn.Tracker().Create(sandboxGVR, object, testNamespace); err != nil {
			t.Fatal(err)
		}
	}

	stored, err := g.r.ProjectSandboxes(g.ctx)
	if err != nil || len(stored) != 0 {
		t.Fatalf("the store's list = %+v, %v; want none of the Sandboxes it has not heard of yet", stored, err)
	}
	listed, err := g.r.ProjectSandboxesFromAPI(g.ctx)
	if err != nil {
		t.Fatalf("the API server's list: %v", err)
	}
	want := []runtime.ProjectSandbox{
		{Name: "legion-legion-cache-late", Kind: runtime.ProjectSandboxCache},
		{Name: "legion-legion-legion-300", Kind: runtime.ProjectSandboxIssue, Issue: "LEGION-300", Tree: "LEGION-300"},
	}
	if !slices.Equal(listed, want) {
		t.Fatalf("the API server's list = %+v, want the project's Sandboxes not being deleted, in name order: %+v", listed, want)
	}
}
