package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

var _ runtime.SlotCredentials = (*Runtime)(nil)

// WriteSlotCredential satisfies runtime.SlotCredentials: the one-slot write of the refresh
// (writeSlotCredential) of the slot of token in its issue's live pod, for the App of the role the
// refresh judges for it (judgeSlot: runtime.SlotRole of the claim the store holds in the slot,
// judged on each read of the slot's Secret, so a step, or a merge forward asked of the owner, that
// began or ended since the read makes the write a conflict judged again), minted on the issue the
// slot's Secret records (claimBundle), so a role a plan defines holds the App of its type. The
// Secret is rewritten only where it does not hold that render already. While a launch into the
// slot is in flight (markSlotLaunch) the write is refused, as a refresh's is (refreshUpdate): the
// launch's start waits for the very token the launch wrote, so no other write may change it until
// the start ends. The write then waits for the end of that launch, its start answered, failed or
// cut short (slotLaunchEnd), never for a clock, and is made again on a fresh read of the pod and
// the stored claims, for the App judged then. Where ctx ends first it answers
// runtime.ErrSlotCredentialPending, a wait, and writes nothing. It holds no lock and no
// transaction while it waits, so the launch it waits for needs nothing of it. A runtime with no
// credential function wrote no slot any gh files, and an issue with no live pod has no slot to
// write: the claim's next launch writes the App runtime.SlotRole answers then, judged the same
// way.
func (r *Runtime) WriteSlotCredential(ctx context.Context, token claim.Token) error {
	if r.gitHubCredential == nil {
		return nil
	}
	slot, ok := token.Slot()
	if !ok {
		return fmt.Errorf("write the slot of %s: the token names no slot", token)
	}
	for {
		pod, uid, live := r.slotPod(token)
		if !live {
			return nil
		}
		held, err := r.slotHolders(ctx)
		if err != nil {
			return fmt.Errorf("write the slot of %s: the stored claims: %w", token, err)
		}
		repo, _ := fetchRepository(pod)
		_, err = r.writeSlotCredential(ctx, pod, uid, slot, repo, r.claimBundle, slotWrite{role: r.judgeSlot(held, pod.Name, slot)})
		if !errors.Is(err, errSlotLaunching) {
			if err != nil {
				return fmt.Errorf("write the slot of %s: %w", token, err)
			}
			return nil
		}
		if ended := r.slotLaunchEnd(slotKey{sandbox: pod.Name, slot: slot}); ended != nil {
			select {
			case <-ended:
			case <-ctx.Done():
				return fmt.Errorf("write the slot of %s: %w: %w", token, err, runtime.ErrSlotCredentialPending)
			}
		}
	}
}

// SlotHoldsCredential satisfies runtime.SlotCredentials: it renders what the slot of token holds
// once it is written for role (renderSlot, which writes nothing) and asks the slot's launcher,
// once, whether the slot's gh files hold that token now (shimwire.LauncherCredentialCheck). It
// answers nil once they do; runtime.ErrSlotCredentialPending, a wait, while the Secret or the
// files hold another credential, the issue has no live pod, or no launcher of the slot is
// connected from it; and runtime.ErrAwaitsPodUpdate where the slot's launcher said in its hello it
// answers no check, a launcher of an image before the check, which only the pod's update round or
// its release replaces. A slot whose gh acts as nobody, and a runtime with no credential function,
// hold nothing to wait for.
func (r *Runtime) SlotHoldsCredential(ctx context.Context, token claim.Token, role claim.Role) error {
	if r.gitHubCredential == nil {
		return nil
	}
	pod, _, live := r.slotPod(token)
	if !live {
		return fmt.Errorf("%s has no live pod whose slot of %s can be read: %w", SandboxName(token), token, runtime.ErrSlotCredentialPending)
	}
	session, checks := r.launchers.credentialChecker(token, string(pod.UID))
	if session == nil {
		return fmt.Errorf("no launcher of %s is connected from the pod %s: %w", token, pod.UID, runtime.ErrSlotCredentialPending)
	}
	if !checks {
		return fmt.Errorf("the launcher of %s in the pod %s cannot say what its slot's gh files hold: %w", token, pod.UID, runtime.ErrAwaitsPodUpdate)
	}
	slot, ok := token.Slot()
	if !ok {
		return fmt.Errorf("read the slot of %s: the token names no slot", token)
	}
	reading, cancel := call(ctx)
	secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(reading, roleSecretName(pod.Name, launcherOf(slot)), metav1.GetOptions{})
	cancel()
	if err != nil {
		return fmt.Errorf("read the slot of %s: %w", token, err)
	}
	repo, _ := fetchRepository(pod)
	_, _, want, err := r.renderSlot(ctx, pod, slot, repo, r.claimBundle, secret, role)
	if err != nil {
		return fmt.Errorf("render the slot of %s for %s: %w", token, role, err)
	}
	if want.owner == "" {
		return nil
	}
	if held, _ := ghconfig.TokenFromHosts(secret.Data[GitHubHostsKey(want.owner)]); held != want.token {
		return fmt.Errorf("the Secret of the slot of %s does not hold %s's credential yet: %w", token, role, runtime.ErrSlotCredentialPending)
	}
	named := want.launcherCredential()
	check := shimwire.LauncherCredentialCheck{
		ID:          "credential-check-" + strconv.FormatUint(r.credentialChecks.Add(1), 10),
		HostsFile:   named.HostsFile,
		TokenDigest: named.TokenDigest,
	}
	asking, cancel := call(ctx)
	answer, _, err := r.launchers.requestSession(asking, session, check, check.ID)
	cancel()
	if err != nil {
		return fmt.Errorf("ask the launcher of %s what its slot's gh files hold: %w", token, err)
	}
	result, ok := answer.(shimwire.LauncherCredentialCheckResult)
	switch {
	case !ok:
		return fmt.Errorf("the launcher of %s answered a credential check with %T", token, answer)
	case result.Error != "":
		return fmt.Errorf("the launcher of %s refused the credential check: %s", token, result.Error)
	case !result.OK:
		return fmt.Errorf("the gh files of the slot of %s do not hold %s's credential yet: %w", token, role, runtime.ErrSlotCredentialPending)
	}
	return nil
}

// slotPod is the live pod of token's issue (livePod), with the uid of the Sandbox that owns it,
// and false where the issue has no Sandbox or no live pod. It reads the informers' stores alone.
func (r *Runtime) slotPod(token claim.Token) (*corev1.Pod, types.UID, bool) {
	name := SandboxName(token)
	s, err := r.storedSandbox(name)
	if err != nil || s == nil {
		return nil, "", false
	}
	pod := r.storedPod(name)
	if pod == nil || !livePod(s, pod) {
		return nil, "", false
	}
	return pod, s.UID, true
}
