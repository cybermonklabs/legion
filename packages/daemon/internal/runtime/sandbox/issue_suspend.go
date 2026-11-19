package sandbox

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var (
	_ runtime.IssueSuspender = (*Runtime)(nil)
	_ runtime.IssueReleaser  = (*Runtime)(nil)
)

// SuspendIssue is the durable close effect, not a per-role stop. Its pod's launch turn (lockPod, by
// the issue's Sandbox name) orders authorize (the store's check of the close against the issue's
// starts, stops and stored roles) and the mode change with physical starts: a later re-admission
// starts only after this patch, and makes a Sandbox of its own rather than resuming this one. No
// database transaction spans a Kubernetes call. Once the pod is gone the Sandbox is deleted
// (deleteStoredSandbox), with the six role Secrets it owns — each holding a launcher token and a
// rendered `gh` credential bundle — so a close leaves the issue nothing. An ephemeral disk was the
// pod's own and went with it. A kept volume (keptVolume) is the Sandbox's own claim, and the
// foreground delete takes it with the Sandbox: a close deletes a kept volume too. The issue's next
// launch, if a person sets it todo again, makes the Sandbox afresh (ensureSandbox). The parent's
// Sandbox is another issue's and is never touched.
func (r *Runtime) SuspendIssue(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error)) error {
	return r.endIssuePod(ctx, issue, tree, authorize, false)
}

// ReleaseIssue is the teardown of a release, which leaves the issue in the workflow: the idle
// release and the release of a pod no claim of the issue can use (runtime.IssueReleaser). It ends
// the pod as SuspendIssue does, under the same launch turn and after the same authorize. Then a
// Sandbox that owns the issue's kept volume (ownsKeptVolume) is kept, Suspended, with its uid and
// the claim of its volume, for the issue's next pod, which mounts that claim (issuePod.disk). The
// seven Secrets the Sandbox owns are deleted: the provisioning Secret and the six role Secrets,
// each of which holds a token that no pod reads any more. The next launch writes each of them
// again before its pod starts (provision, writeLauncherSecrets). Every other Sandbox holds nothing
// the next pod can use, and is deleted as SuspendIssue deletes it.
//
// The pod may show that an init container failed on the kept volume for a cause local to it
// (volumeFault, localInitFailure). The release then writes the reset mark (volumeResetAnnotation)
// in the patch that suspends the Sandbox, from the pod as it stood before the suspension. It sends
// that patch also when the Sandbox is already Suspended. The issue's next launch reads no pod, and
// it resets the volume on the mark (volumeReset). A close (SuspendIssue) writes no mark, because it
// deletes the Sandbox and the volume with it.
func (r *Runtime) ReleaseIssue(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error)) error {
	return r.endIssuePod(ctx, issue, tree, authorize, true)
}

// endIssuePod is SuspendIssue, and ReleaseIssue where keep is true: the issue's pod ended under
// its launch turn, and then the issue's Sandbox deleted, or kept where keep is true and the
// Sandbox owns a kept volume. Under the turn, authorize is asked first and false acts on nothing.
// A Sandbox already gone is done. One labelled with another project, issue or tree is refused. The
// Sandbox is set Suspended, in the one write of its mode here, and its pod is waited out. That
// write is setMode's JSON patch, which sends nothing for a Sandbox already Suspended. A release
// whose pod failed on the kept volume (volumeFault) sends one merge patch of the mode and the reset
// mark in its place (suspendAnnotating), also for a Sandbox already Suspended.
func (r *Runtime) endIssuePod(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error), keep bool) error {
	act := "suspend"
	if keep {
		act = "release"
	}
	name, err := issueSandboxName(r.project, issue)
	if err != nil {
		return fmt.Errorf("%s issue %s: %w", act, issue, err)
	}
	endTurn, err := r.lockPod(ctx, name)
	if err != nil {
		return err
	}
	defer endTurn()
	authorized, err := authorize(ctx)
	if err != nil || !authorized {
		return err
	}
	object, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s issue %s: read Sandbox: %w", act, issue, err)
	}
	s, err := decodeSandbox(object)
	if err != nil {
		return err
	}
	if s.Labels[labelProject] != r.project || s.Labels[labelIssue] != labelValue(issue) || s.Labels[labelTree] != labelValue(tree) {
		return fmt.Errorf("%s issue %s: Sandbox %s does not belong to this issue and tree", act, issue, s.Name)
	}
	// The pod as it stands before the suspension takes it down: the issue's next launch reads no
	// pod, so a release marks a kept volume that the pod shows failed for the launch to reset.
	fault := ""
	if keep {
		fault = volumeFault(s, r.storedPod(name))
	}
	switch {
	case fault != "":
		if _, err := r.suspendAnnotating(ctx, s, map[string]string{volumeResetAnnotation: fault}); err != nil {
			return fmt.Errorf("%s issue %s: %w", act, issue, err)
		}
	case s.mode() != modeSuspended:
		if err := r.setMode(ctx, s, modeSuspended); err != nil {
			return fmt.Errorf("%s issue %s: %w", act, issue, err)
		}
	}
	if _, err := r.awaitPodGone(ctx, s); err != nil {
		return err
	}
	if keep && ownsKeptVolume(s) {
		if err := r.deleteIssueSecrets(ctx, name); err != nil {
			return fmt.Errorf("%s issue %s: %w", act, issue, err)
		}
		// The claim's name is IssueClaimName's, which the Sandbox's name gives.
		r.log.Info("sandbox runtime: released the issue's pod and kept its Sandbox, which owns the issue's kept volume, Suspended; deleted the Secrets that the next launch writes again",
			"issue", issue, "sandbox", name, "uid", s.UID, "claim", issueVolume+"-"+name)
		return nil
	}
	if err := r.deleteStoredSandbox(ctx, name); err != nil {
		return fmt.Errorf("%s issue %s: %w", act, issue, err)
	}
	if keep {
		r.log.Info("sandbox runtime: deleted the released issue's Sandbox and the role Secrets it owned", "issue", issue, "sandbox", name, "uid", s.UID)
		return nil
	}
	r.log.Info("sandbox runtime: deleted the closed issue's Sandbox and the role Secrets it owned", "issue", issue, "sandbox", name, "uid", s.UID)
	return nil
}

// deleteIssueSecrets deletes the seven Secrets that the issue Sandbox name owns, the provisioning
// Secret (secretName) and the role Secret of each of the issue's six roles (roleSecretName), each
// one already gone counted as deleted, and then forgets what is held of the role Secrets
// (forgetLauncherCredentials). Its caller holds the pod's launch turn, so no launch writes one of
// them in between. It is the delete a writeSecret already sends, so it needs no new permission.
func (r *Runtime) deleteIssueSecrets(ctx context.Context, name string) error {
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	remove := func(secret string) error {
		if err := secrets.Delete(ctx, secret, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete Secret %s: %w", secret, err)
		}
		return nil
	}
	if err := remove(secretName(name)); err != nil {
		return err
	}
	for _, role := range claim.Roles {
		if err := remove(roleSecretName(name, role)); err != nil {
			return err
		}
	}
	r.forgetLauncherCredentials(name)
	return nil
}
