package sandbox

import "github.com/sjawhar/legion/daemon/internal/runtime"

var _ runtime.IssuePodReader = (*Runtime)(nil)

// IssuePodRunsConfiguredImage says whether the pod of issue's Sandbox is running the image the
// daemon's configuration names (r.image), read from the pod the runtime's store holds: the pod
// must be the Sandbox's own, not ended, and no container of it — a role's or an init container —
// may run another image. That last reading is imageDrift's, the one a relaunch replaces a pod on
// (relaunch.go), so a release and a relaunch never disagree about which pods are behind.
//
// An idle release reads this before it releases a pod, because a released pod comes back as a
// fresh pod on the configured image: releasing a pod that already runs another image would wake
// its roles onto a daemon release they never ran under, and a pod on an image this daemon does
// not serve is a pod a relaunch of any of its roles replaces outright. Anything that leaves the
// image unread — no Sandbox, no pod, a pod of another Sandbox, an ended pod — answers false: a
// pod whose image cannot be read is not on the configured image, so the release holds rather
// than guesses.
func (r *Runtime) IssuePodRunsConfiguredImage(issue string) bool {
	name, err := issueSandboxName(r.project, issue)
	if err != nil {
		return false
	}
	s, err := r.storedSandbox(name)
	if err != nil || s == nil {
		return false
	}
	pod := r.storedPod(name)
	if !ownedBy(pod, s.UID) || terminal(pod) {
		return false
	}
	_, drifted := imageDrift(pod, r.image)
	return !drifted
}

// IssueSandbox says whether the runtime still holds the Sandbox of issue, and what that Sandbox
// is called: the one name the six role claims of the issue share, which its pod carries too
// (SandboxName, through issueSandboxName), read from the Sandbox the runtime's store holds. The
// name is built from the issue key and never read off the object, so presence is the whole of
// what the store is asked, and the object is taken as the informer holds it (storedObject):
// decoding a whole Sandbox to answer it would cost tens of kilobytes and hundreds of
// allocations, once per pod per tick. A release that takes the pod down records the name it
// took.
//
// It is the informer's store and never an API call: the idle pass asks this of every pod of the
// project on every tick, and a read per pod against the API server would cost the apiserver a
// request a pod a minute to answer what the runtime already watches. Anything that leaves the
// Sandbox unread — an issue key no name can be built from, a store that cannot answer, a
// Sandbox the store does not hold — answers present false with no name, which holds the pod
// rather than releasing it: a pod whose Sandbox cannot be read is one this daemon knows nothing
// about, and guessing it gone would release a pod the runtime still runs.
func (r *Runtime) IssueSandbox(issue string) (string, bool) {
	name, err := issueSandboxName(r.project, issue)
	if err != nil {
		return "", false
	}
	u, err := r.storedObject(name)
	if err != nil || u == nil {
		return "", false
	}
	return name, true
}

// IssuePod satisfies runtime.IssuePodReader: the uid of the pod issue's Sandbox holds now, ""
// where the runtime holds no Sandbox of issue or that Sandbox holds no pod of its own, read from
// the stores the runtime already watches, as IssueSandbox and PodDrift read them. The daemon's
// sweep asks it of an issue whose update round no claim records a pod for (internal/daemon's
// podlessRound), and the uid is what says whether the pod the round was opened over is the one
// still there.
//
// A pod is the Sandbox's own only where the Sandbox controls it (ownedBy), so a same-named pod of
// a Sandbox deleted and made again is no pod of this one. A pod that has run to its end, or is
// being ended under a suspension, is answered all the same: its disk is still there until the
// pod is deleted, and the round over it waits until it is gone rather than end over a disk that
// may still hold its workers' unpushed work. Where the store itself cannot be read, or no Sandbox
// name can be built from the issue key, nothing is answered (read false), since a round must not
// end on a read that was never made, which is where this parts from IssueSandbox: that read
// holds a pod by answering it absent, and this one would end a round by doing so.
func (r *Runtime) IssuePod(issue string) (string, bool) {
	name, err := issueSandboxName(r.project, issue)
	if err != nil {
		return "", false
	}
	u, err := r.storedObject(name)
	if err != nil {
		return "", false
	}
	if u == nil {
		return "", true
	}
	pod := r.storedPod(name)
	if !ownedBy(pod, u.GetUID()) {
		return "", true
	}
	return string(pod.UID), true
}
