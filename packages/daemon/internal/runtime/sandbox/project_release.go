package sandbox

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var _ runtime.ProjectPods = (*Runtime)(nil)

// ProjectSandboxes is every Sandbox of the project the informer holds (it lists and watches on
// legion.dev/project alone), not counting one already being deleted, in name order: what a
// project release takes, and what its dry run reads. Each is told by its labels, as the orphan
// sweep tells them (podKindOf, labelCache, labelProbe), and an issue's pod carries the issue and
// tree its labels name, which SuspendIssue checks the Sandbox against. The pod is the one the
// Sandbox owns now and that has not ended, and its image is read as IssuePodRunsConfiguredImage
// reads it (imageDrift), so a release and an idle release never disagree about which pods run
// another image.
func (r *Runtime) ProjectSandboxes(context.Context) ([]runtime.ProjectSandbox, error) {
	if err := r.sandboxFeed.check(); err != nil {
		return nil, fmt.Errorf("list the project's Sandboxes: %w", err)
	}
	var listed []runtime.ProjectSandbox
	for _, obj := range r.sandboxes.GetStore().List() {
		listed = r.appendProjectSandbox(listed, obj.(*unstructured.Unstructured))
	}
	sortProjectSandboxes(listed)
	return listed, nil
}

// ProjectSandboxesFromAPI is ProjectSandboxes listed from the API server rather than read from the
// informer's store: the same selector (legion.dev/project alone), the same Sandboxes left out and
// the same order, each told as ProjectSandboxes tells it, its pod read from the store. A Sandbox
// the API holds and the store has not heard of yet is in it: a project release lists the project
// this way once more before it takes the controller and the caches, since a create whose check of
// the release's mark passed just before the mark was set can land after the release's first list.
func (r *Runtime) ProjectSandboxesFromAPI(ctx context.Context) ([]runtime.ProjectSandbox, error) {
	list, err := r.sandboxClient().List(ctx, metav1.ListOptions{LabelSelector: labelProject + "=" + r.project})
	if err != nil {
		return nil, fmt.Errorf("list the project's Sandboxes from the API server: %w", err)
	}
	var listed []runtime.ProjectSandbox
	for i := range list.Items {
		listed = r.appendProjectSandbox(listed, &list.Items[i])
	}
	sortProjectSandboxes(listed)
	return listed, nil
}

// appendProjectSandbox appends u to listed as ProjectSandboxes lists it (projectSandbox), unless u
// is already being deleted.
func (r *Runtime) appendProjectSandbox(listed []runtime.ProjectSandbox, u *unstructured.Unstructured) []runtime.ProjectSandbox {
	if u.GetDeletionTimestamp() != nil {
		return listed
	}
	return append(listed, r.projectSandbox(u))
}

// sortProjectSandboxes puts listed in name order.
func sortProjectSandboxes(listed []runtime.ProjectSandbox) {
	slices.SortFunc(listed, func(a, b runtime.ProjectSandbox) int { return strings.Compare(a.Name, b.Name) })
}

// projectSandbox is u as ProjectSandboxes lists it.
func (r *Runtime) projectSandbox(u *unstructured.Unstructured) runtime.ProjectSandbox {
	labels := u.GetLabels()
	listed := runtime.ProjectSandbox{Name: u.GetName(), Kind: projectSandboxKind(labels)}
	if listed.Kind == runtime.ProjectSandboxIssue {
		listed.Issue, listed.Tree = labels[labelIssue], labels[labelTree]
	}
	pod := r.storedPod(u.GetName())
	if ownedBy(pod, u.GetUID()) && !terminal(pod) {
		listed.PodUID = string(pod.UID)
		_, drifted := imageDrift(pod, r.image)
		listed.ConfiguredImage = !drifted
	}
	return listed
}

// projectSandboxKind is the kind labels name: a probe's and a cache's marks first, since neither
// carries an issue or a role, then the two kinds podKindOf tells apart, and unknown for any other
// labelling.
func projectSandboxKind(labels map[string]string) runtime.ProjectSandboxKind {
	switch {
	case labels[labelProbe] != "":
		return runtime.ProjectSandboxProbe
	case labels[labelCache] != "":
		return runtime.ProjectSandboxCache
	}
	switch kind, _ := podKindOf(labels); kind.(type) {
	case issuePod:
		return runtime.ProjectSandboxIssue
	case controllerPod:
		return runtime.ProjectSandboxController
	}
	return runtime.ProjectSandboxUnknown
}

// DeleteProjectSandbox deletes the repository cache's or the image probe's Sandbox name for a
// project release, through the fenced delete the cache sweep retires a cache with: under the pod's
// launch turn, fenced to the Sandbox as the API holds it now, so its foreground delete takes the
// cache's volume and Secret with it, and behind the session-volume guard, whose refusal is
// answered. The Sandbox is read from the API rather than the informer's store, because the fence
// is the object's resourceVersion and a store a write behind would make the delete a conflict. A
// Sandbox written between the read and the delete is refused (deleteFencedOrRefuse) rather than
// kept in silence as the sweep's own delete keeps it: the release holds the sweep, and the sweep
// takes no cache or probe anyway, so the release reports the Sandbox left. A Sandbox already gone
// is deleted; one of another kind is refused, since an issue's pod and the controller's go
// through their own deletes.
func (r *Runtime) DeleteProjectSandbox(ctx context.Context, name string) error {
	release, err := r.lockPod(ctx, name)
	if err != nil {
		return err
	}
	defer release()
	u, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete Sandbox %s: read it: %w", name, err)
	}
	labels := u.GetLabels()
	if labels[labelProject] != r.project {
		return fmt.Errorf("delete Sandbox %s: it is no Sandbox of project %s", name, r.project)
	}
	kind := projectSandboxKind(labels)
	if kind != runtime.ProjectSandboxCache && kind != runtime.ProjectSandboxProbe {
		return fmt.Errorf("delete Sandbox %s: it is a Sandbox of kind %s, which this delete does not take", name, kind)
	}
	if u.GetDeletionTimestamp() != nil {
		return nil
	}
	s, err := decodeSandbox(u)
	if err != nil {
		return err
	}
	return r.deleteFencedOrRefuse(ctx, s, "sandbox runtime: the project release deleted a Sandbox", "kind", string(kind))
}
