package daemon

// The disks of the Kubernetes stand-in's Sandboxes, each named by an id no other disk of the API
// had (newDiskLocked, disk, diskID), and what the project release and move tests
// (dispatch://INFRA-3583) read and do of a cluster through the stand-in: the Sandboxes it holds and
// the ones a daemon made, a Sandbox and pod an older daemon made, and a person's delete of every
// Sandbox.

import (
	"encoding/json"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// newDiskLocked gives the Sandbox name a new disk, named by an id no other disk of this API had.
func (a *issueSandboxAPI) newDiskLocked(name string) {
	a.diskCount++
	a.disks[name], a.diskIDs[name] = true, fmt.Sprintf("%sdisk-%d", a.prefix, a.diskCount)
}

// disk is whether the disk of the Sandbox's pod, the claim that pod owns, still exists.
func (a *issueSandboxAPI) disk(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.disks[name]
}

// diskID is the id of the disk of the Sandbox's pod, "" once that disk is gone (newDiskLocked).
func (a *issueSandboxAPI) diskID(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.diskIDs[name]
}

// sandboxNames is the name of every Sandbox the API holds, in name order: what a person reads of
// the cluster with `kubectl get sandboxes`.
func (a *issueSandboxAPI) sandboxNames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	names := make([]string, 0, len(a.objects["sandboxes"]))
	for name := range a.objects["sandboxes"] {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// createdNames is every Sandbox a daemon made through this API, in the order it made them.
func (a *issueSandboxAPI) createdNames() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.created)
}

// madeByAnOlderDaemon has the Sandbox of name and its pod read as a daemon of an older version
// made them, running that daemon's image: the pod's containers run image (what the runtime's
// image mark reads, sandbox's imageDrift), and the Sandbox's template records the shape that
// daemon built it to, image and all (the pod-shape annotation, which the pod update sweep reads,
// sandbox's podShapeDrift). It answers whether the API held both.
func (a *issueSandboxAPI) madeByAnOlderDaemon(name, image string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	heldSandbox, ok := a.objects["sandboxes"][name]
	heldPod, podOK := a.objects["pods"][name]
	if !ok || !podOK {
		return false
	}
	runs := func(spec map[string]any) {
		for _, key := range []string{"initContainers", "containers"} {
			containers, _ := spec[key].([]any)
			for _, c := range containers {
				if container, _ := c.(map[string]any); container != nil {
					container["image"] = image
				}
			}
		}
	}
	object := kubeClone(heldSandbox)
	template, _ := object["spec"].(map[string]any)["podTemplate"].(map[string]any)
	if template == nil {
		return false
	}
	if spec, _ := template["spec"].(map[string]any); spec != nil {
		runs(spec)
	}
	meta, _ := template["metadata"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	var shape map[string]any
	if recorded, _ := annotations["legion.dev/pod-shape"].(string); json.Unmarshal([]byte(recorded), &shape) != nil {
		return false
	}
	shape["image"] = image
	encoded, err := json.Marshal(shape)
	if err != nil {
		return false
	}
	annotations["legion.dev/pod-shape"] = string(encoded)
	a.putLocked("sandboxes", name, object)
	pod := kubeClone(heldPod)
	spec, _ := pod["spec"].(map[string]any)
	runs(spec)
	pod["status"] = sandboxPodStatus(spec)
	a.putLocked("pods", name, pod)
	return true
}

// deleteEverySandbox deletes every Sandbox the API holds as a person's `kubectl delete sandboxes
// --all` does, in the foreground, so each pod's disk goes with it; the controller stand-in then
// deletes the pods (reconcile). It answers the names it deleted.
func (a *issueSandboxAPI) deleteEverySandbox() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	foreground := metav1.DeletePropagationForeground
	var names []string
	for name := range a.objects["sandboxes"] {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		a.deleteLocked("sandboxes", name)
		a.deletes[name] = metav1.DeleteOptions{PropagationPolicy: &foreground}
		delete(a.disks, name)
		delete(a.diskIDs, name)
	}
	return names
}
