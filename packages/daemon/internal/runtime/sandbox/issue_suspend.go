package sandbox

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
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
	_, err := r.endIssuePod(ctx, issue, tree, authorize, nil, false)
	return err
}

// ReleaseIssue is the teardown of a release, which leaves the issue in the workflow: the idle
// release and the release of a pod no claim of the issue can use (runtime.IssueReleaser). It ends
// the pod as SuspendIssue does, under the same launch turn and after the same authorize. Then a
// Sandbox that owns the issue's kept volume (ownsKeptVolume) is kept, Suspended, with its uid and
// the claim of its volume, for the issue's next pod, which mounts that claim (issuePod.disk). The
// seven Secrets the Sandbox owns are deleted: the provisioning Secret and the six role Secrets,
// each of which holds a token that no pod reads any more. The next launch writes each of them
// again before its pod starts (provision, writeLauncherSecrets). Once every delete has succeeded,
// the release records on the Sandbox that it deleted them (secretsReleasedAnnotation), in a patch
// of its own. A release that stops before that patch leaves no record, and the next release
// deletes the Secrets again, each one already gone counted as deleted, and then records it. Every
// other Sandbox holds nothing the next pod can use, and is deleted as SuspendIssue deletes it.
//
// The pod may show that an init container failed on the kept volume for a cause local to it
// (volumeFault, localInitFailure). The release then writes the reset mark (volumeResetAnnotation)
// in the patch that suspends the Sandbox, from the pod as it stood before the suspension. For a
// Sandbox that is already Suspended, it writes the mark in a patch of its own, unless the
// Sandbox carries that mark already (suspendOver). The issue's next launch reads no pod, and it
// resets the volume on the mark (volumeReset). A close (SuspendIssue) writes no mark, because it
// deletes the Sandbox and the volume with it.
//
// It answers, and calls writing, as runtime.IssueReleaser says. A kept Sandbox an earlier release
// already released is one releasedKept reads so, which it leaves as it is.
func (r *Runtime) ReleaseIssue(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error),
	writing func(context.Context) error) (bool, error) {
	return r.endIssuePod(ctx, issue, tree, authorize, writing, true)
}

// endIssuePod is SuspendIssue, and ReleaseIssue where keep is true: the issue's pod ended under
// its launch turn, and then the issue's Sandbox deleted, or kept where keep is true and the
// Sandbox owns a kept volume. Under the turn, the Sandbox is read first, and the steps after act
// on that read. One labelled with another project, issue or tree is refused, and authorize is not
// asked: the daemon's authorize of a release commits the release where it acts (store's
// CommitIssueRelease), and a committed release is never cancelled. A refusal after that commit
// would come on every attempt, and the release would never end. Then authorize is asked, and
// false acts on nothing. A Sandbox already gone, and a kept Sandbox an earlier release already
// released (releasedKept), are done once authorize has answered, because the caller acts on that
// answer as on any other. The Sandbox is set Suspended in the one write of its mode here
// (suspendOver), and its pod is waited out. That write is the bare JSON patch of the mode
// (sendSuspended), for a Sandbox that is not Suspended yet, and nothing for one that is. A release
// reads the stored pod for that write and a close reads none, so a release whose pod failed on the
// kept volume (volumeFault) sends one merge patch of the mode and the reset mark in its place, or
// of the mark alone for a Sandbox already Suspended that does not carry it. The suspension names
// nothing beside the mode (daemonEnd), so it records the daemon's end of the pod under
// daemonSuspendedAct before either patch is sent. A release that keeps the Sandbox sends one merge
// patch more once it has deleted the Secrets, which records that delete (annotate,
// secretsReleasedAnnotation) and changes no mode.
//
// It answers true once it has deleted or kept the Sandbox it found, and false where it acted on
// nothing (runtime.IssueReleaser): authorize's false, a Sandbox already gone, and the released
// kept Sandbox read off the object the API answered (releasedKept), each with authorize's error,
// if any. writing, where it is not nil, runs once that Sandbox is found and is this release's to
// act on, after authorize and just before suspendOver, the first write; a writing that fails
// leaves the daemon's end unrecorded and the Sandbox unwritten. A delete and a release's record
// on a kept Sandbox are kept for IssueSandbox until the store shows them (noteTaken).
func (r *Runtime) endIssuePod(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error),
	writing func(context.Context) error, keep bool) (bool, error) {
	act := "suspend"
	if keep {
		act = "release"
	}
	name, err := issueSandboxName(r.project, issue)
	if err != nil {
		return false, fmt.Errorf("%s issue %s: %w", act, issue, err)
	}
	endTurn, err := r.lockPod(ctx, name)
	if err != nil {
		return false, err
	}
	defer endTurn()
	// The Sandbox is read and its labels checked before authorize: the daemon's authorize of a
	// release commits the release where it acts (store's CommitIssueRelease), so a Sandbox that
	// this call refuses commits nothing. The turn keeps every launch of the issue out, so the steps
	// below act on this read.
	object, err := r.sandboxClient().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// A Sandbox already gone is done, and authorize is still asked: the caller acts on its
		// answer as on the answer for a pod that this call takes. A close then ends the pause's
		// hold on the pod; a release ends it too, and records itself where its rule says so.
		_, err = authorize(ctx)
		return false, err
	}
	if err != nil {
		return false, fmt.Errorf("%s issue %s: read Sandbox: %w", act, issue, err)
	}
	s, err := decodeSandbox(object)
	if err != nil {
		return false, err
	}
	if s.Labels[labelProject] != r.project || s.Labels[labelIssue] != labelValue(issue) || s.Labels[labelTree] != labelValue(tree) {
		return false, fmt.Errorf("%s issue %s: Sandbox %s does not belong to this issue and tree", act, issue, s.Name)
	}
	authorized, err := authorize(ctx)
	if err != nil || !authorized {
		return false, err
	}
	// A kept Sandbox an earlier release already released holds nothing this release could take;
	// a second patch would only write the same record again.
	if keep && releasedKept(object) {
		return false, nil
	}
	// The pod as it stands before the suspension takes it down: the issue's next launch reads no
	// pod, so a release marks a kept volume that the pod shows failed for the launch to reset. A
	// close deletes the Sandbox and the volume with it, so it reads no pod and writes no mark.
	var pod *corev1.Pod
	if keep {
		pod = r.storedPod(name)
	}
	if writing != nil {
		if err := writing(ctx); err != nil {
			return false, fmt.Errorf("%s issue %s: %w", act, issue, err)
		}
	}
	// The pod the suspension ends is ended by the daemon, and the suspension, naming nothing beside
	// the mode, records that under daemonSuspendedAct before its patch is sent, the reset mark's
	// merge patch as the JSON patch (daemonEnd, suspendOver), so the end reads as the daemon's
	// own rather than as something outside the pod ending it. The idle release's teardown comes
	// through here too: both are this one effect on the issue's pod. A Sandbox a try of its own
	// already suspended takes no second suspension, since that try made the record its patch
	// needed.
	if _, err := r.suspendOver(ctx, s, pod, daemonEnd{}); err != nil {
		return false, fmt.Errorf("%s issue %s: %w", act, issue, err)
	}
	if _, err := r.awaitPodGone(ctx, s); err != nil {
		return false, err
	}
	if keep && ownsKeptVolume(s) {
		if err := r.deleteIssueSecrets(ctx, name); err != nil {
			return false, fmt.Errorf("%s issue %s: %w", act, issue, err)
		}
		// The record goes on only after every delete has succeeded, so it is never on a Sandbox
		// that still owns a Secret (secretsReleasedAnnotation).
		uid := string(s.UID)
		if _, err := r.annotate(ctx, s, map[string]*string{secretsReleasedAnnotation: &uid}); err != nil {
			return false, fmt.Errorf("%s issue %s: record that its Secrets are deleted: %w", act, issue, err)
		}
		r.noteTaken(name, sandboxTaken{uid: s.UID, released: true})
		r.log.Info("sandbox runtime: released the issue's pod and kept its Sandbox, which owns the issue's kept volume, Suspended; deleted the Secrets that the next launch writes again",
			"issue", issue, "sandbox", name, "uid", s.UID, "claim", issueClaimName(name))
		return true, nil
	}
	if err := r.deleteStoredSandbox(ctx, name); err != nil {
		return false, fmt.Errorf("%s issue %s: %w", act, issue, err)
	}
	if keep {
		r.log.Info("sandbox runtime: deleted the released issue's Sandbox and the role Secrets it owned", "issue", issue, "sandbox", name, "uid", s.UID)
		return true, nil
	}
	r.log.Info("sandbox runtime: deleted the closed issue's Sandbox and the role Secrets it owned", "issue", issue, "sandbox", name, "uid", s.UID)
	return true, nil
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
