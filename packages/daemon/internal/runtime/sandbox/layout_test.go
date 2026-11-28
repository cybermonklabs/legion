package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The boot census passes a project whose only Sandbox is an issue pod this runtime launched, and
// refuses one that still holds a per-claim Sandbox of the layout before issue pods, naming it.
func TestTheBootCensusRefusesLegacyIssueSandboxesOnly(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.spawn(rootSpec(t))
	sandboxes := g.dyn.Resource(sandboxGVR).Namespace(testNamespace)
	if err := rejectLegacyIssueSandboxes(g.ctx, sandboxes, testProject, g.opts.IssueDisks(), slogTo(io.Discard)); err != nil {
		t.Fatalf("census of a current issue pod = %v, want none", err)
	}
	legacy := sandboxObject(t, "legion-legion-legion-208-tester", "uid-legacy", modeRunning, claimLabels(workerSpec(t).Role))
	if err := g.dyn.Tracker().Create(sandboxGVR, legacy, testNamespace); err != nil {
		t.Fatal(err)
	}
	err := rejectLegacyIssueSandboxes(g.ctx, sandboxes, testProject, g.opts.IssueDisks(), slogTo(io.Discard))
	if err == nil || !strings.Contains(err.Error(), "legacy issue Sandbox") || !strings.Contains(err.Error(), legacy.GetName()) {
		t.Fatalf("legacy object census = %v", err)
	}
}

// The census passes both pods this runtime builds, an issue pod's one container and the project
// controller's one, and holds every other Sandbox to the one container of the kind its labels name
// (podKindOf): the controller Sandbox a daemon before issue pods made (role=controller, one worker
// container), a controller pod running the workflow launchers, an issue pod of the earlier layout
// missing a launcher or running one more, and a Sandbox whose labels name no kind are each refused,
// by name, before the daemon adopts, suspends or deletes any of them.
func TestTheBootCensusHoldsEverySandboxToTheLaunchersItsLabelsName(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.spawn(controllerSpec(t))
	g.spawn(rootSpec(t))
	if err := rejectLegacyIssueSandboxes(g.ctx, g.dyn.Resource(sandboxGVR).Namespace(testNamespace), testProject, g.opts.IssueDisks(), slogTo(io.Discard)); err != nil {
		t.Fatalf("census of a current controller pod and issue pod = %v, want none", err)
	}

	workflow := workflowContainers()
	issue := claimLabels(claim.RoleArchitect)
	controller := map[string]string{labelProject: testProject, labelRole: string(claim.RoleController)}
	for label, tc := range map[string]struct {
		name       string
		labels     map[string]string
		containers []string
		want       string
	}{
		"the controller pod of a daemon before issue pods": {"legion-legion-controller", controller, []string{"worker"}, "not the one container controller its labels name"},
		"a controller pod running the workflow launchers":  {"legion-legion-controller", controller, workflow, "not the one container controller its labels name"},
		"an issue pod missing a launcher":                  {"legion-legion-legion-208", issue, workflow[1:], "not the one container roles its labels name"},
		"an issue pod running a container more":            {"legion-legion-legion-208", issue, append(append([]string{}, workflow...), string(claim.RoleController)), "not the one container roles its labels name"},
		"a controller-labelled pod of a tree": {"legion-legion-other", map[string]string{labelProject: testProject, labelRole: string(claim.RoleController), labelTree: testTree},
			[]string{string(claim.RoleController)}, "name neither an issue nor the project controller"},
		"labels naming no role list": {"legion-legion-other", map[string]string{labelProject: testProject}, []string{string(claim.RoleController)}, "name neither an issue nor the project controller"},
	} {
		t.Run(label, func(t *testing.T) {
			sandboxes := newDynamic(t, sandboxRunning(t, tc.name, tc.labels, tc.containers...)).Resource(sandboxGVR).Namespace(testNamespace)
			err := rejectLegacyIssueSandboxes(context.Background(), sandboxes, testProject, testOptions().IssueDisks(), slogTo(io.Discard))
			if err == nil || !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("census = %v, want a refusal naming %s: %q", err, tc.name, tc.want)
			}
		})
	}
}

// A namespace of a deployment before issue pods holds a per-claim Sandbox for every running role.
// The census's one refusal names every Sandbox it refuses, each with its reason, and how many, and
// none of the pods this runtime builds, so the operator clears them all before the next boot.
func TestTheBootCensusNamesEveryRefusedSandboxAndHowMany(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.spawn(rootSpec(t))
	built := SandboxName(rootToken)
	legacy := map[string]string{
		"legion-legion-legion-208-tester":   "not the one container roles its labels name",
		"legion-legion-legion-208-reviewer": "not the one container roles its labels name",
		"legion-legion-controller":          "not the one container controller its labels name",
		"legion-legion-other":               "name neither an issue nor the project controller",
	}
	for name, labels := range map[string]map[string]string{
		"legion-legion-legion-208-tester":   claimLabels(claim.RoleTester),
		"legion-legion-legion-208-reviewer": claimLabels(claim.RoleReviewer),
		"legion-legion-controller":          {labelProject: testProject, labelRole: string(claim.RoleController)},
		"legion-legion-other":               {labelProject: testProject},
	} {
		if err := g.dyn.Tracker().Create(sandboxGVR, sandboxRunning(t, name, labels, "worker"), testNamespace); err != nil {
			t.Fatal(err)
		}
	}
	err := rejectLegacyIssueSandboxes(g.ctx, g.dyn.Resource(sandboxGVR).Namespace(testNamespace), testProject, g.opts.IssueDisks(), slogTo(io.Discard))
	if err == nil {
		t.Fatal("census = nil, want a refusal naming every legacy Sandbox")
	}
	if want := fmt.Sprintf("%d of project %s's Sandboxes are not pods this runtime builds", len(legacy), testProject); !strings.Contains(err.Error(), want) {
		t.Errorf("census = %q, want it to say %q", err, want)
	}
	for name, reason := range legacy {
		if !strings.Contains(err.Error(), "legacy issue Sandbox "+name) {
			t.Errorf("census = %q, want it to name %s", err, name)
		}
		_, after, _ := strings.Cut(err.Error(), "legacy issue Sandbox "+name)
		if next, _, _ := strings.Cut(after, "; legacy issue Sandbox "); !strings.Contains(next, reason) {
			t.Errorf("census names %s with %q, want %q", name, next, reason)
		}
	}
	if strings.Contains(err.Error(), "legacy issue Sandbox "+built+" ") || strings.Contains(err.Error(), "legacy issue Sandbox "+built+":") {
		t.Errorf("census = %q, which names the issue pod this runtime built, %s", err, built)
	}
}

// An issue Sandbox owns no claim template at all — its disk is its own pod's (INFRA-80) — and the
// controller's owns the one `issue` template its working root is on, which outlives its pods while
// its session is a row in the session database; the census passes both as this runtime builds
// them. It refuses, by name, every Sandbox of the tree-volume layout before
// per-issue volumes whose containers would otherwise pass: a tree root's Sandbox with its `tree`
// template, that layout's controller Sandbox with a `tree` template of its own, and a controller
// Sandbox owning no volume, which no release of this runtime built — each told it is of the
// tree-volume layout and pointed at docs/kubernetes.md's upgrade section — before the daemon
// adopts, suspends or recreates any of them.
func TestTheBootCensusRefusesEverySandboxOfTheTreeVolumeLayout(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.spawn(controllerSpec(t))
	g.spawn(rootSpec(t))
	if err := rejectLegacyIssueSandboxes(g.ctx, g.dyn.Resource(sandboxGVR).Namespace(testNamespace), testProject, g.opts.IssueDisks(), slogTo(io.Discard)); err != nil {
		t.Fatalf("census of a current issue pod and controller pod = %v, want none", err)
	}

	const layout = "it is of the tree-volume layout before per-issue volumes"
	const upgrade = `docs/kubernetes.md's "Upgrading a deployment with running trees"`
	controller := map[string]string{labelProject: testProject, labelRole: string(claim.RoleController)}
	for label, tc := range map[string]struct {
		object *unstructured.Unstructured
		want   string
	}{
		"a tree root's Sandbox with its tree template": {
			withTreeVolume(t, sandboxRunning(t, "legion-legion-legion-300", claimLabels(claim.RoleArchitect), workflowContainers()...)), `owns a volume claim template named "tree", not "issue"`,
		},
		"the controller Sandbox of that layout": {
			withTreeVolume(t, sandboxRunning(t, "legion-legion-controller", controller, string(claim.RoleController))),
			`owns a volume claim template named "tree", not "issue"`,
		},
		"a controller Sandbox owning no volume": {
			sandboxRunning(t, "legion-legion-controller", controller, string(claim.RoleController)), "owns no volume",
		},
	} {
		t.Run(label, func(t *testing.T) {
			sandboxes := newDynamic(t, tc.object).Resource(sandboxGVR).Namespace(testNamespace)
			err := rejectLegacyIssueSandboxes(context.Background(), sandboxes, testProject, testOptions().IssueDisks(), slogTo(io.Discard))
			if err == nil {
				t.Fatalf("census = nil, want a refusal naming %s", tc.object.GetName())
			}
			for _, want := range []string{"Sandbox " + tc.object.GetName() + " " + tc.want, layout, upgrade} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("census = %q, want it to say %q", err, want)
				}
			}
		})
	}
}

// An issue's disk has one of two shapes, and the census passes an issue Sandbox of either: one
// that owns no claim template, its disk being its own pod's, and one that owns the `issue`
// template labelled kept, where its microVM pod keeps its volume (keptVolume). It passes the one
// `issue` template the release before the pod-owned disk gave an issue too. volumeClaimTemplates
// is immutable in the CRD, so nothing but the delete-and-create of the issue's next launch can
// replace a Sandbox of another shape than that launch renders, and a boot refusal would stop the
// daemon until an operator deleted one Sandbox per open issue by hand. The census names each
// Sandbox that its issue's launch would replace, by the comparison that launch makes
// (diskShapeChange), in one info line with the reason, so the pending replacement is visible. It
// says nothing of the Sandboxes that already have the shape their launch renders. The issue's
// sandbox comes from the Sandbox's issue label, matched to the canary list as a launch matches
// its issue.
func TestTheBootCensusPassesAnIssueSandboxOfEitherDiskShape(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.spawn(controllerSpec(t))
	g.spawn(rootSpec(t))
	of := func(issue string) map[string]string {
		return map[string]string{labelProject: testProject, labelTree: testTree, labelIssue: issue}
	}
	// before is of the release before the pod-owned disk, and now of an ephemeral disk, both of
	// LEGION-208, which no configuration below puts on its canary list. kept owns the kept volume of
	// the canary LEGION-211, and keptBefore is of the release before the pod-owned disk and of the
	// canary LEGION-212.
	before := withSupersededDisk(t, sandboxRunning(t, "legion-legion-legion-209", claimLabels(claim.RoleImplementer), workflowContainers()...))
	now := sandboxRunning(t, "legion-legion-legion-210", claimLabels(claim.RoleImplementer), workflowContainers()...)
	kept := withKeptTemplate(t, sandboxRunning(t, "legion-legion-legion-211", of("LEGION-211"), workflowContainers()...), true)
	keptBefore := withSupersededDisk(t, sandboxRunning(t, "legion-legion-legion-212", of("LEGION-212"), workflowContainers()...))
	for _, object := range []*unstructured.Unstructured{before, now, kept, keptBefore} {
		if err := g.dyn.Tracker().Create(sandboxGVR, object, testNamespace); err != nil {
			t.Fatal(err)
		}
	}
	under := func(config func(*Options)) IssueDisks {
		o := testOptions()
		config(&o)
		return o.IssueDisks()
	}
	canaries := []string{"LEGION-211", "LEGION-212"}
	for row, tc := range map[string]struct {
		disks IssueDisks
		// replaced is the reason the census logs for each Sandbox it names, by name. It names no
		// other Sandbox.
		replaced map[string]sandboxReplacement
	}{
		"a configuration that keeps the canaries' volumes": {
			under(keptVolumes(SandboxGVisor, canaries...)),
			map[string]sandboxReplacement{before.GetName(): replacedFromPreviousRelease, keptBefore.GetName(): replacedFromPreviousRelease},
		},
		"a configuration that keeps none": {
			under(sandboxBlock(SandboxGVisor, canaries...)),
			map[string]sandboxReplacement{
				before.GetName(): replacedFromPreviousRelease, kept.GetName(): replacedToKeepNone, keptBefore.GetName(): replacedFromPreviousRelease,
			},
		},
		"a configuration that names no sandbox": {
			g.opts.IssueDisks(),
			map[string]sandboxReplacement{
				before.GetName(): replacedFromPreviousRelease, kept.GetName(): replacedToKeepNone, keptBefore.GetName(): replacedFromPreviousRelease,
			},
		},
	} {
		t.Run(row, func(t *testing.T) {
			logs := &bytes.Buffer{}
			if err := rejectLegacyIssueSandboxes(g.ctx, g.dyn.Resource(sandboxGVR).Namespace(testNamespace), testProject, tc.disks, slogTo(logs)); err != nil {
				t.Fatalf("census of an issue Sandbox of each disk shape = %v, want none", err)
			}
			for _, name := range []string{before.GetName(), now.GetName(), kept.GetName(), keptBefore.GetName(), SandboxName(rootToken), SandboxName(controllerToken)} {
				lines := linesHolding(logs.String(), "sandbox="+name+" ")
				reason, replaced := tc.replaced[name]
				switch {
				case !replaced && len(lines) > 0:
					t.Errorf("census logged %q, which names %s, a Sandbox that already has the disk shape its launch renders", lines, name)
				case replaced && len(lines) != 1:
					t.Errorf("census logged %s %d times, want once: %q", name, len(lines), logs)
				case replaced && (!strings.Contains(lines[0], "next launch that starts a pod replaces it") || !strings.Contains(lines[0], fmt.Sprintf("reason=%q", reason))):
					t.Errorf("census logged %q, want it to say %s's next launch replaces it because %s", lines[0], name, reason)
				}
			}
		})
	}
}

// One census names every tree-volume-layout Sandbox it refuses, with its reason, and how many,
// beside any legacy per-claim Sandbox, and neither the pods this runtime built nor the issue
// Sandbox whose next launch replaces its disk: the operator clears them all before the next boot
// rather than one per refused boot.
func TestTheBootCensusNamesEveryTreeVolumeLayoutSandboxInOneRefusal(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.spawn(controllerSpec(t))
	g.spawn(rootSpec(t))
	built := []string{SandboxName(rootToken), SandboxName(controllerToken), "legion-legion-legion-209"}
	refused := map[string]string{
		"legion-legion-controller-old": "owns no volume",
		"legion-legion-legion-300":     `owns a volume claim template named "tree"`,
		"legion-legion-legion-301":     "runs containers [worker], not the one container roles its labels name",
	}
	for _, object := range []*unstructured.Unstructured{
		sandboxRunning(t, "legion-legion-legion-209", claimLabels(claim.RoleImplementer), workflowContainers()...),
		sandboxRunning(t, "legion-legion-controller-old", map[string]string{labelProject: testProject, labelRole: string(claim.RoleController)}, string(claim.RoleController)),
		withTreeVolume(t, sandboxRunning(t, "legion-legion-legion-300", claimLabels(claim.RoleArchitect), workflowContainers()...)),
		sandboxRunning(t, "legion-legion-legion-301", claimLabels(claim.RoleTester), "worker"),
	} {
		if err := g.dyn.Tracker().Create(sandboxGVR, object, testNamespace); err != nil {
			t.Fatal(err)
		}
	}
	err := rejectLegacyIssueSandboxes(g.ctx, g.dyn.Resource(sandboxGVR).Namespace(testNamespace), testProject, g.opts.IssueDisks(), slogTo(io.Discard))
	if err == nil {
		t.Fatal("census = nil, want a refusal naming every tree-volume-layout Sandbox")
	}
	if want := fmt.Sprintf("%d of project %s's Sandboxes are not pods this runtime builds", len(refused), testProject); !strings.Contains(err.Error(), want) {
		t.Errorf("census = %q, want it to say %q", err, want)
	}
	for name, reason := range refused {
		_, after, found := strings.Cut(err.Error(), "Sandbox "+name+" ")
		if !found {
			t.Errorf("census = %q, want it to name %s", err, name)
			continue
		}
		if next, _, _ := strings.Cut(after, "; "); !strings.Contains(next, reason) {
			t.Errorf("census names %s with %q, want %q", name, next, reason)
		}
	}
	for _, name := range built {
		if strings.Contains(err.Error(), "Sandbox "+name+" ") || strings.Contains(err.Error(), "Sandbox "+name+":") {
			t.Errorf("census = %q, which names the pod this runtime built, %s", err, name)
		}
	}
}

// workflowContainers are an issue pod's containers in the layout before one container: one per
// workflow role, each named for its role.
func workflowContainers() []string {
	containers := make([]string, 0, len(claim.Roles))
	for _, role := range claim.Roles {
		containers = append(containers, string(role))
	}
	return containers
}

// withTreeVolume is u in the tree-volume layout's root or controller shape: its one claim template
// is named `tree`, the template that layout gave the tree root's Sandbox and the controller's.
func withTreeVolume(t *testing.T, u *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	return withClaimTemplate(t, u, "tree")
}

// sandboxRunning is a Sandbox as the API would hold it, whose pod runs containers by these names.
func sandboxRunning(t *testing.T, name string, labels map[string]string, containers ...string) *unstructured.Unstructured {
	t.Helper()
	u := sandboxObject(t, name, "uid-"+name, modeRunning, labels)
	list := make([]any, 0, len(containers))
	for _, container := range containers {
		list = append(list, map[string]any{"name": container, "image": testImage})
	}
	if err := unstructured.SetNestedSlice(u.Object, list, "spec", "podTemplate", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
	return u
}
