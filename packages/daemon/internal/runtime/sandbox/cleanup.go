package sandbox

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// CleanupTree deletes every issue Sandbox of tree, and with each the role Secrets it owns and the
// kept volume it owns where its pod keeps one (deleteSandbox says how its foreground delete takes
// them). It keeps nothing for a child the tree carried into a tree of its own: an issue pod's
// ephemeral disk is the pod's own, which went with that pod, and a kept volume goes with its
// Sandbox here, so there is no disk to hand over and the child's next launch creates a Sandbox of
// its own (ensureSandbox). The tree's cleanup reservation calls it (store.CleanupReservedTree)
// once every stored claim of the tree has retired, or runs nothing and belongs to an issue that
// moved, so no launch creates a Sandbox of the tree meanwhile. The API's own listing by the
// tree's label is the census: each Sandbox it names is deleted, in any order, and awaited, and the
// listing is read again until none is left. Each delete is fenced to the listed object's UID and
// resourceVersion, and one the object changed since — the precondition the API refuses with 409,
// as a controller writing status through a foreground delete does — lists again after a recheck
// interval rather than spinning the census; a Sandbox the new tree's launch relabelled meanwhile
// fails the same fence and the next listing no longer names it, so it is left to that tree. A
// Sandbox already gone is gone, so a retry after a failure finishes the rest.
func (r *Runtime) CleanupTree(ctx context.Context, tree string) error {
	for {
		sandboxes, err := r.treeSandboxes(ctx, tree)
		if err != nil {
			return err
		}
		if len(sandboxes) == 0 {
			return nil
		}
		for _, object := range sandboxes {
			err := r.deleteSandbox(ctx, object)
			if apierrors.IsConflict(err) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(recheckInterval):
				}
				break
			}
			if err != nil {
				return err
			}
		}
	}
}

// treeSandboxes lists tree's issue Sandboxes from the API, the image probe's left out.
func (r *Runtime) treeSandboxes(ctx context.Context, tree string) ([]*unstructured.Unstructured, error) {
	list, err := r.sandboxClient().List(ctx, metav1.ListOptions{
		LabelSelector: labelProject + "=" + r.project + "," + labelTree + "=" + labelValue(tree),
	})
	if err != nil {
		return nil, fmt.Errorf("cleanup tree %s: list its Sandboxes: %w", tree, err)
	}
	var sandboxes []*unstructured.Unstructured
	for i := range list.Items {
		if object := &list.Items[i]; object.GetLabels()[labelProbe] == "" {
			sandboxes = append(sandboxes, object)
		}
	}
	return sandboxes, nil
}

// deleteSandbox deletes object with foreground propagation, fenced to its UID and resourceVersion,
// forgets its launchers' credentials once the API has accepted the delete, keeps that this
// runtime's delete took it (noteTaken), and waits until the API no longer has it. Every delete is
// foreground: the Sandbox owns its role Secrets by owner reference — and an issue Sandbox that owns
// an `issue` claim template also the PVC that the template made: a kept volume (keptVolume), or the
// disk of the release before the pod-owned disk — and foreground propagation has garbage collection
// remove them before the Sandbox is gone, so the Sandbox's absence is the API's confirmation that
// what it owned is gone — the restricted daemon identity has no PVC verb to confirm it with. An
// issue's ephemeral disk is not among them: it is its pod's own, and goes when the delete takes the
// pod. A conflict is returned as it is.
func (r *Runtime) deleteSandbox(ctx context.Context, object *unstructured.Unstructured) error {
	s, err := decodeSandbox(object)
	if err != nil {
		return err
	}
	if err := r.guardSessionVolumeReap(ctx, s); err != nil {
		return err
	}
	// The pod the Sandbox owns goes with it — a tree's cleanup, an issue's close, a launch
	// replacing a Sandbox of the superseded disk shape — so that end is the daemon's own and is
	// recorded as such before the delete is sent (meanToEndPod).
	r.meanToEndPodOf(ctx, s, daemonDeletedAct)
	name, uid, version := object.GetName(), object.GetUID(), object.GetResourceVersion()
	policy := metav1.DeletePropagationForeground
	options := metav1.DeleteOptions{
		PropagationPolicy: &policy,
		Preconditions:     &metav1.Preconditions{UID: &uid, ResourceVersion: &version},
	}
	err = r.sandboxClient().Delete(ctx, name, options)
	if err != nil && !apierrors.IsNotFound(err) {
		if apierrors.IsConflict(err) {
			return err
		}
		return fmt.Errorf("delete Sandbox %s: %w", name, err)
	}
	r.forgetLauncherCredentials(name)
	r.noteTaken(name, sandboxTaken{uid: uid})
	return r.awaitSandboxDeleted(ctx, name)
}

// sandboxTaken is what one of this runtime's own writes took from the Sandbox of uid before the
// informer's store shows it (Runtime.taken): its delete, or, where released is true, a release's
// record on a Sandbox it kept that its Secrets are deleted (secretsReleasedAnnotation).
type sandboxTaken struct {
	uid      types.UID
	released bool
}

// noteTaken keeps what one of this runtime's writes took from the Sandbox name (taken), once the
// API has answered that write: a delete accepted or answered already gone, or a release's record
// patched on. It keeps nothing the store has caught up with (caughtUp): the event that told the
// store has already run, or is about to run, the notify that would drop it, so an entry kept then
// would outlive it. Both reads are under mu, which notify holds for its own.
func (r *Runtime) noteTaken(name string, took sandboxTaken) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.caughtUp(name, took, nil) {
		r.taken[name] = took
	}
}

// storedTaken is the Sandbox name as the store holds it, nil for none, and what is kept of this
// runtime's write to it (taken), both read under one hold of mu, so notify cannot drop the entry
// between the two reads (IssueSandbox).
func (r *Runtime) storedTaken(name string) (*unstructured.Unstructured, sandboxTaken, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, err := r.storedObject(name)
	took, held := r.taken[name]
	return u, took, held, err
}

// dropCaughtUp drops what is kept of this runtime's write to the Sandbox name (taken) once the
// store, or event, the Sandbox object of the informer event notify is handling, shows it
// (caughtUp). Its caller holds mu.
func (r *Runtime) dropCaughtUp(name string, event *unstructured.Unstructured) {
	if took, held := r.taken[name]; held && r.caughtUp(name, took, event) {
		delete(r.taken, name)
	}
}

// forgetTaken drops what is kept of this runtime's write to the Sandbox name (taken): a launch
// that takes a release's record off a kept Sandbox makes it the issue's Sandbox again, whatever
// version of it the store holds.
func (r *Runtime) forgetTaken(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.taken, name)
}

// caughtUp is whether the store shows what took did to the Sandbox name: it holds no object of
// took's uid, or, for a release's record, the object of that uid carries it, as the store or event
// (an informer event's object, nil for none) shows it. event is read too because the store may
// already have moved past the version that showed the record, as a launch that took it off again
// moves it. A store that cannot be read has not caught up, for every caller alike. Its caller
// holds mu.
func (r *Runtime) caughtUp(name string, took sandboxTaken, event *unstructured.Unstructured) bool {
	u, err := r.storedObject(name)
	switch {
	case err != nil:
		return false
	case u == nil || u.GetUID() != took.uid:
		return true
	}
	return took.released && (recordsRelease(u, took.uid) || recordsRelease(event, took.uid))
}

// recordsRelease is whether u is the object of uid and carries a release's record, with its own
// uid, that its Secrets are deleted (secretsReleasedAnnotation), read in place.
func recordsRelease(u *unstructured.Unstructured, uid types.UID) bool {
	if u == nil || u.GetUID() != uid {
		return false
	}
	record, _, _ := unstructured.NestedString(u.Object, "metadata", "annotations", secretsReleasedAnnotation)
	return record != "" && record == string(uid)
}

// deleteStoredSandbox deletes the Sandbox name as the API holds it now, in the foreground, and
// returns once the API no longer has it; one already gone is gone. Each delete is fenced to the
// object as read (deleteSandbox), so one the Sandbox controller wrote in between — a status write —
// is refused as a conflict, read again and deleted again after a recheck interval. Both callers
// hold the pod's launch turn, so nothing of this daemon recreates the Sandbox under the loop: the
// end of an issue's pod (endIssuePod: a close's SuspendIssue, and a release's ReleaseIssue of a
// Sandbox that keeps no volume), and a launch replacing an issue Sandbox of another disk shape
// (replaceSupersededSandbox). Each logs the delete it wanted at its own call site.
func (r *Runtime) deleteStoredSandbox(ctx context.Context, name string) error {
	for {
		object, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read sandbox %s: %w", name, err)
		}
		if err := r.deleteSandbox(ctx, object); !apierrors.IsConflict(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(recheckInterval):
		}
	}
}

// awaitSandboxDeleted waits, up to the boot timeout, until the API no longer has the Sandbox name.
func (r *Runtime) awaitSandboxDeleted(ctx context.Context, name string) error {
	deadline := time.NewTimer(r.bootTimeout)
	defer deadline.Stop()
	for {
		_, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("confirm Sandbox %s deletion: %w", name, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("confirm Sandbox %s deletion: timed out after %s", name, r.bootTimeout)
		case <-time.After(recheckInterval):
		}
	}
}
