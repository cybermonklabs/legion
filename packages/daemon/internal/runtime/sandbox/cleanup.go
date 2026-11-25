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

// CleanupIssue deletes the issue's own Sandbox, named by the issue's identity alone
// (issueSandboxName), and with it the role Secrets it owns and the kept volume it owns where its
// pod keeps one (deleteSandbox says how its foreground delete takes them): nothing of another
// issue, whatever tree either stored, and nothing found by the tree label, which is a Sandbox's
// creation's and read by no decision (R17 of dispatch://INFRA-3320, decision 14 of
// dispatch://INFRA-4730). The issue's cleanup reservation calls it (store.CleanupReservedIssue)
// once every stored claim of the issue has retired, so no launch of the issue creates a Sandbox
// meanwhile; it runs under the pod's launch turn all the same, as every other delete of an issue
// Sandbox does. The issue's close has usually deleted the Sandbox already (SuspendIssue), and one
// already gone is gone, so a retry after a failure finishes. A Sandbox by that name labelled with
// another project or issue, or the image probe's, is not this issue's: it is left alone, with an
// error that names it.
func (r *Runtime) CleanupIssue(ctx context.Context, issue string) error {
	name, err := issueSandboxName(r.project, issue)
	if err != nil {
		return fmt.Errorf("cleanup issue %s: %w", issue, err)
	}
	endTurn, err := r.lockPod(ctx, name)
	if err != nil {
		return err
	}
	defer endTurn()
	object, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cleanup issue %s: read Sandbox %s: %w", issue, name, err)
	}
	if !issueOwns(object, r.project, issue) {
		return fmt.Errorf("cleanup issue %s: Sandbox %s does not belong to this issue (labels %v); kept", issue, name, object.GetLabels())
	}
	if err := r.deleteStoredSandbox(ctx, name); err != nil {
		return fmt.Errorf("cleanup issue %s: %w", issue, err)
	}
	r.log.Info("sandbox runtime: the issue's cleanup deleted its Sandbox and the role Secrets it owned", "issue", issue, "sandbox", name, "uid", object.GetUID())
	return nil
}

// issueOwns is whether object, the Sandbox by issue's name, is issue's own: labelled with project
// and with issue, and not the image probe's. Its tree label decides nothing.
func issueOwns(object *unstructured.Unstructured, project, issue string) bool {
	labels := object.GetLabels()
	return labels[labelProject] == project && labels[labelIssue] == labelValue(issue) && labels[labelProbe] == ""
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
