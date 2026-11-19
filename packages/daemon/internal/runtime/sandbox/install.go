package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// crdGVR is CustomResourceDefinitions, read through the dynamic client: the typed
// apiextensions client would pull k8s.io/apiextensions-apiserver into the module graph.
var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// CheckInstalled is the boot refusal of LEGION-206 Requirement 10: the Sandbox CRD must exist and
// serve the version this runtime speaks, and the controller's Deployment must have an available
// replica. Each piece is checked, and every one missing is named, whether the API answered 404,
// 403 — under a resourceNames grant a piece that does not exist is forbidden, not absent — or
// the Deployment has no available replica. Its clients pace themselves and bound each request as
// the runtime's requests do (requestConfig).
func CheckInstalled(ctx context.Context, rc *rest.Config, ref InstallRef) error {
	c, err := clientsOf(requestConfig(rc))
	if err != nil {
		return err
	}
	return checkInstalled(ctx, c.dyn, c.kube, ref)
}

func checkInstalled(ctx context.Context, dyn dynamic.Interface, kube kubernetes.Interface, ref InstallRef) error {
	var errs []error
	crd, err := dyn.Resource(crdGVR).Get(ctx, ref.CRD, metav1.GetOptions{})
	if err != nil {
		errs = append(errs, refusal("the Sandbox CRD "+ref.CRD, err))
	} else if !servesVersion(crd, sandboxGVR.Version) {
		errs = append(errs, fmt.Errorf("agent sandbox: the Sandbox CRD %s does not serve %s, the version this runtime speaks",
			ref.CRD, sandboxGVR.Version))
	}
	piece := "the Agent Sandbox controller Deployment " + ref.ControllerNamespace + "/" + ref.ControllerName
	deployment, err := kube.AppsV1().Deployments(ref.ControllerNamespace).Get(ctx, ref.ControllerName, metav1.GetOptions{})
	if err != nil {
		errs = append(errs, refusal(piece, err))
	} else if deployment.Status.AvailableReplicas < 1 {
		errs = append(errs, fmt.Errorf("agent sandbox: %s has %d available replicas; the controller is not running",
			piece, deployment.Status.AvailableReplicas))
	}
	return errors.Join(errs...)
}

// refusal names piece and why it could not be read.
func refusal(piece string, err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("agent sandbox: %s is not installed (404): %w", piece, err)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("agent sandbox: %s cannot be read (403): it is absent, or this identity may not get it: %w", piece, err)
	default:
		return fmt.Errorf("agent sandbox: %s could not be read: %w", piece, err)
	}
}

// servesVersion reports whether the CRD serves version.
func servesVersion(crd *unstructured.Unstructured, version string) bool {
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, item := range versions {
		entry, _ := item.(map[string]any)
		if entry["name"] == version && entry["served"] == true {
			return true
		}
	}
	return false
}

// CensusLegacyIssueSandboxes is the Kubernetes half of the issue-pod layout fence. Daemon boot
// runs it before the store opens, so before any schema write, image probe or reconcile: a Sandbox
// this runtime did not build must never be adopted, suspended or deleted as though it were one of
// its pods, and nothing may change what an older binary needs to clean it up. Image probes are
// explicit non-issue Sandboxes and are excluded. disks is the configuration each issue's disk is
// rendered from, which tells the census which issue Sandboxes their next launch replaces. log
// carries the one line the census writes about a Sandbox it accepts rather than refuses
// (rejectLegacyIssueSandboxes). Its client paces itself and bounds each request as the runtime's
// requests do (requestConfig).
func CensusLegacyIssueSandboxes(ctx context.Context, rc *rest.Config, namespace, project string, disks IssueDisks, log *slog.Logger) error {
	dyn, err := dynamic.NewForConfig(requestConfig(rc))
	if err != nil {
		return fmt.Errorf("sandbox runtime: dynamic client: %w", err)
	}
	return rejectLegacyIssueSandboxes(ctx, dyn.Resource(sandboxGVR).Namespace(namespace), project, disks, log)
}

// IssueDisks is the configuration that an issue's disk is rendered from, as the daemon settles it.
// The boot census renders each issue's disk from it (templates) and compares the issue's Sandbox
// with that disk (diskShapeChange).
type IssueDisks struct {
	// Sandbox is runtime.kubernetes.sandbox, which decides whether an issue's pod runs in a microVM
	// and keeps its volume, and the class of a microVM pod's disk.
	Sandbox SandboxConfig
	// StorageClass is runtime.kubernetes.storage_class, the class of every other pod's disk.
	StorageClass string
	// Volume is runtime.kubernetes.issue_volume, the disk's size.
	Volume resource.Quantity
}

// IssueDisks is the configuration that the issues' disks of a runtime built from o are rendered
// from, which the daemon hands the boot census (CensusLegacyIssueSandboxes).
func (o Options) IssueDisks() IssueDisks {
	return IssueDisks{Sandbox: o.Sandbox, StorageClass: o.StorageClass, Volume: o.IssueVolume}
}

// templates are the claim templates that the launch of the issue whose legion.dev/issue label
// holds label renders (issuePod.disk), with the sandbox that prepare decides for that issue
// (SandboxConfig.ofIssueLabel): the kept volume's (keptVolumeTemplates) where that launch keeps
// its volume (keptVolume), on the class and at the size claimSpec gives, and none otherwise. The
// census runs before any runtime is built, so it renders them from the configuration alone. The
// stored label is already a label value, so it labels the template as it is.
func (d IssueDisks) templates(project, label string) []volumeClaimTemplate {
	var microVM *MicroVM
	if d.Sandbox.ofIssueLabel(label) == SandboxMicroVM {
		microVM = d.Sandbox.MicroVM
	}
	if !keptVolume(issuePod{}, microVM) {
		return nil
	}
	return keptVolumeTemplates(project, label, claimSpec(d.StorageClass, d.Volume, microVM))
}

// rejectLegacyIssueSandboxes refuses the project's Sandboxes whose pod is not one this runtime
// builds: its containers must be exactly the one container of the kind its labels name
// (podKindOf), `roles` for an issue Sandbox and `controller` for the project controller's, or, for
// an issue Sandbox alone, one container per workflow role, the layout before one container
// (earlierLayout), whose pod the next launch of any of its roles replaces (movedLayout); and it
// must own only the volumes that kind's Sandbox can own — at most the one `issue` claim template
// for an issue, and exactly that template for the controller, whose session outlives its pods
// (treeVolumeLayout). So a per-claim Sandbox of the layout before issue pods (one worker
// container), the controller Sandbox a daemon before issue pods made (role=controller, one worker
// container), a pod with any other set of containers, a Sandbox whose labels name no kind, and a
// Sandbox of the tree-volume layout before per-issue volumes (the tree root's `tree` template, and
// that layout's controller, which owns no volume) are each refused.
//
// An issue Sandbox whose disk shape is not the one its issue's launch renders from disks
// (diskShapeChange) is not refused. It is one of the release before the pod-owned disk, or one
// that a change of keep_volume, the issue's sandbox or the volume's class, size or access modes
// left behind. volumeClaimTemplates is immutable in the CRD, so the only way to the launch's shape
// is the delete-and-create of the issue's next launch that starts a pod, and a boot refusal would
// stop the whole daemon until an operator deleted one Sandbox per open issue by hand. The census
// logs it by name with the reason instead, so the pending replacement is visible without holding
// anything up. It logs nothing for a Sandbox that already has its launch's shape.
//
// A repository cache's Sandbox (labelled legion.dev/cache) is judged by its own shape alone
// (cacheSandboxShape), never as a pod of a kind, and refused, as `cache Sandbox <name>`, when it is
// not one this runtime builds. The refusal names every one of them, each with its reason, and how
// many there are, so an operator removes them all before the next boot rather than one per refused
// boot.
func rejectLegacyIssueSandboxes(ctx context.Context, sandboxes dynamic.ResourceInterface, project string, disks IssueDisks, log *slog.Logger) error {
	list, err := sandboxes.List(ctx, metav1.ListOptions{LabelSelector: labelProject + "=" + project})
	if err != nil {
		return fmt.Errorf("sandbox runtime: census existing issue Sandboxes before layout migration: %w", err)
	}
	var refused []string
	for _, object := range list.Items {
		if object.GetLabels()[labelProbe] != "" {
			continue
		}
		if object.GetLabels()[labelCache] != "" {
			if reason := cacheSandboxShape(object); reason != "" {
				refused = append(refused, "cache Sandbox "+object.GetName()+reason)
			}
			continue
		}
		kind, err := podKindOf(object.GetLabels())
		if err != nil {
			refused = append(refused, "legacy issue Sandbox "+object.GetName()+": "+err.Error())
			continue
		}
		if reason := legacyIssueSandbox(object, kind); reason != "" {
			refused = append(refused, "legacy issue Sandbox "+object.GetName()+reason)
			continue
		}
		switch reason, replacement := treeVolumeLayout(object, kind, project, disks); {
		case reason != "":
			refused = append(refused, "Sandbox "+object.GetName()+reason)
		case replacement != "":
			log.Info("sandbox runtime: the issue Sandbox does not have the disk shape its launch renders, so the issue's next launch that starts a pod replaces it",
				"sandbox", object.GetName(), "reason", string(replacement))
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("sandbox runtime: %d of project %s's Sandboxes are not pods this runtime builds; migrate or remove each before enabling issue pods: %s",
		len(refused), project, strings.Join(refused, "; "))
}

// treeVolumeLayout is why object does not own the volumes a Sandbox of kind can own, following its
// name in the census's refusal, and, when it is accepted, why its issue's next launch that starts
// a pod replaces it (diskShapeChange over the templates that launch renders from disks,
// IssueDisks.templates), or "" when nothing replaces it. An issue's Sandbox owns at most the one
// `issue` claim template: none where its pod's disk is the pod's own generic ephemeral volume, and
// that one where its microVM pod keeps its volume (issuePod.disk). The template of the release
// before the pod-owned disk has that name too. The controller's Sandbox owns the one named
// issueVolume, which outlives its pods because the controller's working root is on it
// (controllerPod.disk), and it is never replaced for its disk shape. The tree-volume layout
// before per-issue volumes gave only the tree root's Sandbox a template, named
// `tree`, which every child of the tree mounted, and its controller's Sandbox a `tree` template of
// its own; a controller owning no volume comes from that layout too. A root's volume holds every
// clone and workspace of its tree, which only the drained cutover docs/kubernetes.md describes may
// take apart, so a Sandbox owning a `tree` template is never adopted.
//
// What the fence no longer catches is a **child** of that layout: it owned no template either, so
// it is the new shape by this test, and it is adopted. That costs nothing — the Running patch
// replaces the pod template wholesale, so its pod gets an ephemeral disk of its own and reads
// nothing of the tree volume — but the refusal that forced the drained cutover now rests on the
// root's `tree` template and the old controller alone. There is no narrower test: a child of that
// layout and a Sandbox this runtime makes now are the same object.
func treeVolumeLayout(object unstructured.Unstructured, kind podKind, project string, disks IssueDisks) (reason string, replacement sandboxReplacement) {
	const upgrade = "; it is of the tree-volume layout before per-issue volumes, so drain and remove it as docs/kubernetes.md's \"Upgrading a deployment with running trees\" says"
	const unreadable = " has an unreadable volume claim template" + upgrade
	_, issue := kind.(issuePod)
	templates, found, err := unstructured.NestedSlice(object.Object, "spec", "volumeClaimTemplates")
	if err != nil {
		return unreadable, ""
	}
	if !issue && (!found || len(templates) == 0) {
		return " owns no volume" + upgrade, ""
	}
	var held []volumeClaimTemplate
	for _, raw := range templates {
		template, ok := raw.(map[string]any)
		if !ok {
			return unreadable, ""
		}
		name, _, _ := unstructured.NestedString(template, "metadata", "name")
		if name != issueVolume {
			return fmt.Sprintf(" owns a volume claim template named %q, not %q", name, issueVolume) + upgrade, ""
		}
		if issue {
			var decoded volumeClaimTemplate
			if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(template, &decoded); err != nil {
				return unreadable, ""
			}
			held = append(held, decoded)
		}
	}
	if !issue {
		return "", ""
	}
	return "", diskShapeChange(held, disks.templates(project, object.GetLabels()[labelIssue]))
}

// legacyIssueSandbox is why object's pod is not the one this runtime builds for kind, the kind its
// labels name (podKindOf), following its name in the census's refusal, or "" when it is one: its
// kind's one container, or an issue pod of the earlier layout (earlierLayout), which the next launch
// of any of its roles replaces (movedLayout).
func legacyIssueSandbox(object unstructured.Unstructured, kind podKind) string {
	containers, found, err := unstructured.NestedSlice(object.Object, "spec", "podTemplate", "spec", "containers")
	if err != nil || !found {
		return " has no issue-pod container shape"
	}
	var names []string
	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			return " has an unreadable container shape"
		}
		name, _ := container["name"].(string)
		names = append(names, name)
	}
	if slices.Equal(names, []string{kind.container()}) {
		return ""
	}
	if _, issue := kind.(issuePod); issue && earlierLayout(names) {
		return ""
	}
	return fmt.Sprintf(" runs containers %v, not the one container %s its labels name", names, kind.container())
}
