package sandbox

// The restore hop of an issue pod's saved workspace copy: what the daemon sends a pod's launcher
// of the copies the store holds (launchers.restoreWorkspace), the record of which launcher process
// already holds the copy it was sent (heldCopy), and what a launcher's report of its restore keeps
// (launchers.restoreReported). They are a part of the launcher's connection (launcher.go) kept in a
// file of their own.

import (
	"context"
	"strconv"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// heldCopy is one send of an issue's saved workspace copy that the pod's launcher answered Held:
// the pod it was sent to, the restore id it carried, and the LauncherID of each launcher session
// of that pod connected when the answer came.
//
// A pod's `roles` container runs one launcher process, which holds a connection of its own to
// the daemon per role, each with a LauncherID of its own that the process mints at its start and
// keeps across its reconnects (internal/launcher's newManagers), and which holds one restore for
// all of its roles (internal/launcher's bundleRestore): what one role's connection was sent whole,
// the process applies at its first start of any role. So the sessions of the pod connected when
// the launcher answered Held are the process that holds the copy, or an earlier process of the
// pod that has ended, whose ids no connection says again. A role start whose session's LauncherID
// is in the set is one of that process, and the copy is in its hands: the start takes nothing of
// the issue's copies and sends nothing, where before every role start of the pod took the copy
// again under the issue's copy lock and streamed every slice to a launcher that answered each
// from its own record. A session not in the set takes again as it always did: a restarted
// container, whose new process brings new ids for every role; a session that connected after the
// answer, which its process answers from its own record without a second restore; and every
// session after a daemon restart, which held this record in memory alone. What the store answers
// such a take decides whether anything is sent, as it always has
// (store.TakeWorkspaceCopyForRestore).
//
// It is recorded only on Held, so a send that failed records nothing, and each issue keeps one,
// replaced by the next send its launcher held, so the record grows only with the issues the
// daemon has sent a copy for.
type heldCopy struct {
	podUID    string
	restore   int64
	launchers map[string]bool
}

// holdsCopy is the send of issue's copy that the launcher process of session already holds, and
// whether it holds one: the issue's heldCopy is of session's pod and names session's LauncherID.
func (d *launchers) holdsCopy(issue string, session *launcherSession) (heldCopy, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	held, ok := d.held[issue]
	return held, ok && held.podUID == session.podUID && held.launchers[session.id]
}

// heldBy records that the launcher of podUID answered Held to the send of issue's copy under
// restore: the LauncherID of every session of that pod the daemon holds now, replacing whatever
// the issue held before (heldCopy).
func (d *launchers) heldBy(issue, podUID string, restore int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	launchers := map[string]bool{}
	for _, s := range d.sessions {
		if s.issue == issue && s.podUID == podUID {
			launchers[s.id] = true
		}
	}
	d.held[issue] = heldCopy{podUID: podUID, restore: restore, launchers: launchers}
}

// bundleRestoreBound is how long the hop of one pod's saved bundle may take. The 40 MiB a release
// carries at most is 160 slices of shimwire.LauncherBundlePayloadBytes, each one round trip the
// launcher answers out of memory, so this is room for a slow connection rather than for the
// common case; a hop that outruns it costs the restore and never the start.
const bundleRestoreBound = 2 * time.Minute

// leftForNextPodLine is the line a copy written after this pod first asked for a restore is logged
// with (runtime.WorkspaceBundleLeftForNextPod), at Info: the copy stays unspent for the next pod
// of the issue, which is sent it unless this pod's own next copy replaces it first. It names when
// this pod first asked, when the copy's take began and when the store wrote it, all by the store's
// clock; the write after the ask is the whole of why this pod was sent nothing.
const leftForNextPodLine = "launcher bundle: the saved workspace copy was written after this pod first asked for a restore, so it is not sent to this pod, which has been working since that ask, and nothing is recorded on it: it stays for the next pod of the issue"

// refusedPushedLine is the warning a running copy older than the issue's newest recorded push is
// logged with (runtime.WorkspaceBundleRefusedPushed); a release bundle is never refused so. A
// copy's time is when its take began, so the line says taken. It is the line an operator reads to
// know why a pod started on what was pushed although the daemon held a copy, and what that copy
// would have done.
const refusedPushedLine = "launcher bundle: the saved workspace copy was taken before the newest push of the issue's branch the daemon recorded, so it is refused and no pod is sent it: restoring it would put the workspace back on work behind the pushed commits, which the roles would have to bring forward over them; its bytes stay in the daemon's database"

// supersededLine is the line the older of the issue's two copies is closed unsent with
// (runtime.WorkspaceBundleSuperseded), by the take (runtime.DecideWorkspaceRestore) or by the drop
// of the copies a kept volume holds (runtime.DecideKeptVolumeDrop). It names the newer copy
// (runtime.WorkspaceRestore.Offered) and what this pod's ask did with it, `newerTake`, since that
// copy is not always sent here. Beside a drop it is WorkspaceBundleHeldByVolume: the newer copy was
// deleted, its work being on the volume this pod mounts. Beside a take it is what the take answered
// the newer copy, which is no send where its attempt is spent already, this pod is the one its work
// came off, the push refuses it, or it was written after this pod first asked.
const supersededLine = "launcher bundle: the older of the issue's two saved workspace copies is closed unsent, since the newer one's work is later, offered to the issue's pods or already on this pod's kept volume, and no pod may restore the older over it; its bytes stay in the database"

// withheldBranchLine is the one line a copy that holds the issue's branch `legion/<KEY>`, its
// rescue branch `legion/<KEY>-rescue` or both is sent with. The restore sets every bookmark the
// copy holds but those two (bundleFrames, runtime.RestorableBookmarks), so each stays where the
// pod's own fetch put it, on what GitHub holds: the issue's branch so that the role's next push
// keeps the commits GitHub holds on it, and the rescue branch because it only moves forward, so a
// bookmark set back behind GitHub's tip would be pushed as a move backwards that drops a later
// rescue, or be left conflicted by the next rescue push, which jj then refuses to push. The line
// names each bookmark withheld with the commit the copy held it on, as the very call that left
// them out of the slices answered them.
const withheldBranchLine = "launcher bundle: the saved workspace copy's own values of the issue's branch and its rescue branch are withheld where it holds them, so the restore leaves each where the pod's fetch from GitHub put it; the copy's commits come back as heads and as the working copy"

// reachedLine is the line a copy the launcher holds whole is logged with, before the launcher
// applies it.
const reachedLine = "launcher bundle: the issue's saved workspace work reached the pod that restores it"

// alreadyHeldLine is the line a role start whose launcher process already holds the copy its pod
// was sent is logged with (heldCopy): the start takes nothing of the issue's copies and sends
// nothing, and the process applies the copy it holds at its first start of any role. It names the
// restore id of the copy that process holds.
const alreadyHeldLine = "launcher bundle: this role's launcher process already holds the saved workspace copy its pod was sent, so this role start neither takes the issue's copies nor sends the copy again"

// restoreFailedLine is the warning a launcher's report of a restore that failed is logged with.
// The copy stays in the daemon's database, but a running copy only until the next copy of the
// issue's workspace replaces it, which the line says so that an operator who wants its bytes reads
// them first (restoreReported). The roles start on what was pushed, and the first of them to
// register spends the copy (store.Store.RecordPodRegistration), so no pod is sent it again.
const restoreFailedLine = "launcher bundle: the pod's launcher could not restore the issue's saved workspace work, which no pod is sent again once a role of this pod registers; it stays in the daemon's database, a running copy only until the next copy of the issue's workspace replaces it"

// keptVolumeHoldsLine is the line each copy a pod's kept volume holds is dropped with
// (runtime.WorkspaceBundleHeldByVolume), at Info, since no work is lost: the copy came off the
// volume the pod mounts, so the pod's workspace holds its work. It names which copy, its restore
// id, when its take began, by the store's clock, and when the pod's Sandbox, and so its volume,
// was created, by the API server's.
const keptVolumeHoldsLine = "launcher bundle: this pod's kept volume holds the workspace the saved workspace copy came off, so the copy is dropped and the pod is sent nothing of it"

// keptVolumeDropFailedLine is the warning a drop of the copies a pod's kept volume holds that
// failed is logged with (restoreWorkspace). The pod is sent nothing, and every copy stays as it
// was for the issue's next start, which asks again.
const keptVolumeDropFailedLine = "launcher bundle: the saved workspace copies that this pod's kept volume may hold could not be dropped, so the pod is sent nothing and every copy stays for the next start"

// restoreWorkspace carries the newer of the two copies of this pod's issue workspace to the pod's
// launcher: the bundle the issue's last idle release saved, and the copy the daemon took while
// the pod that held the work was running. The launcher applies what it is sent to the workspace
// before the first role of the pod runs, so every call is from a start that has not been issued
// yet (start), and it reports what the restore did in a frame of its own, which is what the copy
// it put back is kept as this pod's own on (restoreReported).
//
// Which copy, if either, the pod is sent is the store's one decision, made in the transaction
// that records this pod's first ask and spends the attempts (store.TakeWorkspaceCopyForRestore,
// runtime.DecideWorkspaceRestore): the newer copy is offered and the older closed unsent, a
// running copy whose take began before the newest push of the issue's branch the daemon recorded
// is refused, where a release bundle never is, and a copy written after this pod first asked is
// left for the next pod. Every time that decision compares is the store's clock, so this hop
// passes it nothing of the pod but its uid; the drop a pod of a kept Sandbox asks first is also
// passed when that Sandbox was made (keptSince). This hop only carries out what that answer says
// and logs each copy it touched, so no state of the store depends on this hop finishing. Whatever
// copy it sends goes without its own values of the issue's branch and its rescue branch
// (bundleFrames), so no restore moves `legion/<KEY>` or `legion/<KEY>-rescue` off what GitHub
// holds.
//
// Nothing here stops a role. A slice the launcher refuses, a body that did not arrive as it was
// sent, a store that cannot be read and a connection that fails are each logged and left: a pod
// that never starts is worse than one whose workspace holds only what was pushed, and the
// launcher writes the agent its own notice of what was lost (internal/launcher's restoreWorkspace).
//
// The take records the pod this send is for before it answers the body, and the copy is spent once
// a role of this pod registers (store.Store.RecordPodRegistration), after which the take answers
// nothing for it. So once a role has registered, a roles container that restarts, a pod that
// replaces this one and a third pod after an eviction are each sent nothing, and no copy is ever
// applied over work a running pod's roles have done since it was taken. Before any role of this
// pod registered no role of it ran, so a pod, or its roles container, that ends then leaves the
// copy unspent: the restarted container's first role start, or the next pod's, asks again, the
// store decides again, and the copy it sends is restored over the workspace no role has touched.
// The work itself stays in the database; a release bundle no pod restored holds the issue's pod
// (workflow.IdleHoldBundleUnrestored) until the issue's work is over, and a running copy holds
// none, the disk it was taken off being where its work is.
//
// Whose attempt it is the session's own pod decides, and the store decides it: the pod a copy was
// taken off goes on starting roles — a release stops them, and a cadence copies a pod that is
// working — and such a start reaches here too, for the very workspace the work came from. The
// store knows that pod by the uid the copy recorded and answers it nothing, leaving the copy for
// the pod that replaces it. Every other pod is sent it the first time it asks after the copy was
// written. A copy written after this pod's first ask, which is this pod's first role start, is not
// sent here and nothing is recorded on it: this pod has been working since that ask, and the copy
// stays for the next pod of the issue. That line is Info, since the copy stays for the next pod;
// the refusal of a running copy older than a recorded push is this hop's warning an operator must
// read — the work is in the database and no pod will put it back on its own.
//
// A pod whose own kept volume holds a copy takes none of it, and the store drops that copy first
// (store.DropHeldWorkspaceCopies, runtime.DecideKeptVolumeDrop). That pod's Sandbox owns the
// issue's kept volume (launcherSession.keptSince), so every copy taken since that Sandbox was
// created came off the volume the pod mounts: the bundle a release saved before it kept the
// Sandbox, and the running copy the cadence took off an earlier pod of the Sandbox, alike. Its
// launcher would refuse such a copy, because the workspace is older than the copy, and log a
// notice that the work was lost, while a release bundle held the issue's pod. The drop sends no
// slice and keeps the take's rules, and records this pod's first ask as the take does, so this
// start, which takes nothing more, leaves a copy written after it for the next pod. Each copy it
// drops is logged by name, and an older copy it closes beside a newer one dropped is logged as a
// closed copy is. A drop that deletes no copy leaves the take to run as for any pod: a copy taken
// before the Sandbox was created came off an earlier volume, which a reset or a change of the
// disk's shape replaced, and the take restores it. A drop that fails is logged and the pod is
// sent nothing, as a take that fails: the rows are as they were, and the next start asks again.
// The drop runs at a role's start, after the pod's launcher connected, and that launcher runs
// after both init containers of the pod. So a kept volume whose init failed never drops the copy
// its reset needs: the reset replaces that Sandbox, and the pod of the new Sandbox restores the
// work.
//
// A start whose launcher process already holds the copy, because a role of the same process was
// sent it and that process answered Held, takes and sends nothing (heldCopy, holdsCopy): its pod
// starts up to six roles, and the copy is the process's, not the role's. Every other start takes
// as above, a restarted container's and every start after a daemon restart among them, and a Held
// answer records the sessions of the pod connected then (heldBy).
//
// The held check, the drop, the take, the send and the record of a Held answer all run inside the
// pod's launch turn, which the caller holds: relaunch takes lockPod for the whole start, and the
// six roles of an issue share one Sandbox name (SandboxName). So the starts of one issue's pod run
// one at a time, and a start reads the record of who holds the copy that the start before it left,
// skipping where its launcher process holds the copy now. Nothing a hop waits on waits on another
// start: the drop and the take are the store's, the slices are answered by the launcher's frame
// loop, and the report of a restore is read there too (restoreReported), outside any turn.
func (d *launchers) restoreWorkspace(ctx context.Context, token claim.Token, podUID string) {
	if d.saved == nil || d.restored == nil {
		return
	}
	// The session names the issue its pod is of, as the Secret its launcher authenticated with
	// did. A launcher that never arrives is the start's own error to report, not this hop's.
	session, err := d.await(ctx, token, podUID)
	if err != nil || session.issue == "" {
		return
	}
	issue := session.issue
	if held, ok := d.holdsCopy(issue, session); ok {
		d.log.Info(alreadyHeldLine, "issue", issue, "launcher", token, "pod", podUID, "restore", held.restore)
		return
	}
	restoring, cancel := context.WithTimeout(ctx, bundleRestoreBound)
	defer cancel()
	if !session.keptSince.IsZero() && d.dropHeld != nil {
		dropped, err := d.dropHeld(restoring, issue, podUID, session.keptSince)
		if err != nil {
			d.log.Warn(keptVolumeDropFailedLine,
				"issue", issue, "launcher", token, "pod", podUID, "volume_created", session.keptSince, "error", err)
			return
		}
		// A drop that deleted a copy leaves nothing the take could send this pod: a copy taken
		// since the Sandbox was made is deleted, and an older one beside it is closed.
		if dropped.ReleaseBundle.Take == runtime.WorkspaceBundleHeldByVolume || dropped.Running.Take == runtime.WorkspaceBundleHeldByVolume {
			d.logCopyTakes(session, token, dropped)
			return
		}
	}
	restore, err := d.saved(restoring, issue, podUID)
	if err != nil {
		d.log.Warn("launcher bundle: the issue's saved workspace copies could not be taken for this pod's restore, so the pod starts on what was pushed and every copy is left as it was",
			"issue", issue, "launcher", token, "pod", podUID, "error", err)
		return
	}
	d.logCopyTakes(session, token, restore)
	// The offered copy, when its take is a send, is the only one this hop goes on with: the
	// decision sends no other.
	offered := restore.Of(restore.Offered)
	if offered.Take != runtime.WorkspaceBundleSend || len(offered.Bundle.Bundle) == 0 {
		return
	}
	kind, bundle := restore.Offered, offered.Bundle
	frames, withheld, err := bundleFrames(issue, bundle)
	if err != nil {
		d.log.Warn("launcher bundle: the saved workspace copy could not be sliced, so the pod starts on what was pushed",
			"issue", issue, "launcher", token, "copy", kind, "taken", bundle.At, "error", err)
		return
	}
	if len(withheld) > 0 {
		d.log.Info(withheldBranchLine, "issue", issue, "launcher", token, "copy", kind,
			"withheld", withheld, "restore", bundle.Release)
	}
	for _, frame := range frames {
		answer, _, err := d.request(restoring, token, podUID, frame, bundleWaiter(frame.ID, frame.Index))
		if err != nil {
			d.log.Warn("launcher bundle: a slice of the saved workspace copy did not reach the launcher, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "copy", kind, "slice", frame.Index, "slices", frame.Count, "error", err)
			return
		}
		result, ok := answer.(shimwire.LauncherBundleResult)
		if !ok {
			d.log.Warn("launcher bundle: the launcher answered a bundle slice with another frame, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "copy", kind, "slice", frame.Index, "answer", answer.FrameType())
			return
		}
		if !result.OK {
			d.log.Warn("launcher bundle: the launcher refused a slice of the saved workspace copy, so the pod starts on what was pushed",
				"issue", issue, "launcher", token, "copy", kind, "slice", frame.Index, "slices", frame.Count, "error", result.Error)
			return
		}
		if !result.Held {
			continue
		}
		// Held ends the sequence: the launcher has the whole, digest-checked body and applies it
		// before its first role runs. The daemon's copy stays as it is until the launcher reports
		// what the restore itself did (restoreReported) — a restore can still fail, and the work
		// is then in the daemon's database alone, which is where the launcher's notice says it is.
		// The copy is spent once a role of this pod registers, and no pod is sent it after that;
		// a release bundle stays until the issue's work is over, and a running copy until the
		// next copy of the issue's workspace replaces its row. The launcher process holds it for
		// every role it serves, so the sessions of this pod connected now are recorded as holding
		// it, and a later start on one of them sends it nothing (heldCopy).
		d.heldBy(issue, podUID, bundle.Release)
		d.log.Info(reachedLine,
			"issue", issue, "launcher", token, "copy", kind, "taken", bundle.At,
			"bytes", len(bundle.Bundle), "slices", len(frames), "restore", bundle.Release)
		return
	}
	d.log.Warn("launcher bundle: the launcher took every slice of the saved workspace copy without holding the whole",
		"issue", issue, "launcher", token, "copy", kind, "taken", bundle.At, "slices", len(frames))
}

// logCopyTakes logs what one ask of session's pod did to each copy of its issue's workspace, the
// drop's (runtime.HeldWorkspaceCopiesDrop) or the take's (runtime.SavedWorkspaceBundle), both of
// which answer in one shape (runtime.WorkspaceRestore). A copy the ask had nothing to answer of —
// the issue holds none, its attempt is spent, or its work came off this very pod — is a take of
// WorkspaceBundleNone and logs nothing, the offered one included, and a send is logged by the hop
// that carries it.
func (d *launchers) logCopyTakes(session *launcherSession, token claim.Token, restore runtime.WorkspaceRestore) {
	issue, podUID := session.issue, session.podUID
	offered := restore.Of(restore.Offered)
	for _, kind := range [...]runtime.WorkspaceCopyKind{runtime.WorkspaceCopyReleaseBundle, runtime.WorkspaceCopyRunning} {
		taken := restore.Of(kind)
		b := taken.Bundle
		// Each line names the copy the same way, and then what its own decision read.
		copyOf := func(more ...any) []any {
			return append([]any{"issue", issue, "launcher", token, "pod", podUID, "copy", kind, "taken", b.At}, more...)
		}
		switch taken.Take {
		case runtime.WorkspaceBundleHeldByVolume:
			d.log.Info(keptVolumeHoldsLine, copyOf("restore", b.Release, "bytes", taken.Size, "volume_created", session.keptSince)...)
		case runtime.WorkspaceBundleLeftForNextPod:
			d.log.Info(leftForNextPodLine, copyOf("firstAsked", restore.FirstAsked, "written", taken.Written)...)
		case runtime.WorkspaceBundleRefusedPushed:
			d.log.Warn(refusedPushedLine, copyOf("bytes", taken.Size, "head", b.Head, "restore", b.Release,
				"pushed", restore.Push.At, "pushedHead", restore.Push.Head)...)
		case runtime.WorkspaceBundleSuperseded:
			d.log.Info(supersededLine, copyOf("bytes", taken.Size, "newer", restore.Offered, "newerTake", offered.Take)...)
		}
	}
}

// restoreReported keeps the saved copy of this pod's issue workspace that a launcher's report
// names as this pod's own running copy, once that launcher reports that its restore put the work
// back. A restore that failed leaves the copy as the send left it, spent once a role of this pod
// registers, and a pod that ends before any role of it registered leaves it unspent for the next
// start of the issue (restoreWorkspace). A release bundle no pod restored is ended by the issue's
// close or its tree's close (outbox's dropReleasedWork), or by a cancel of the release that saved
// it, which moves it into the issue's running copy (store.CancelIssueRelease). A running copy no
// pod restored is replaced by the next copy taken off any pod of the issue, the restoring pod's own
// included (store.PutRunningWorkspaceCopy).
//
// The confirmed copy is kept, not deleted, because from here on the restoring pod's disk is the
// one place its work is, and a pod that does nothing takes no copy of its own: an agent relaunched
// in it comes up Ready rather than ending a turn, the cadence copies only an issue in a turn, and
// the push copy needs a push. The store sources the kept copy to this session's pod, so that pod's
// own later asks are answered nothing and the pod after it is offered the work, and a second end
// from outside before any turn of this pod still restores it (store.KeepRestoredWorkspaceCopy).
// The session's pod is the pod the report is of: a launcher process lives in its pod, and one that
// restarts there holds no report.
//
// The restore id the slices carried is the whole of what the keep reads of the copy, and all this
// hop knows of it: a launcher keeps its report until some connection carries it (internal/launcher's
// bundleRestore.owed), so one can reach the daemon after the issue has saved again, and rebinding
// the issue's row on such a report would take newer work for this pod's. The id says both which
// kind of copy it is and which row of that kind, because the two kinds draw their ids from disjoint
// spaces (store.MinRunningWorkspaceCopyRestoreID); which of the two tables it names is read by the
// store that owns both (store.KeepRestoredWorkspaceCopy, which the daemon hands this package as
// runtime.WorkspaceBundleRestored), not here, and a report naming a row either table has replaced
// changes nothing.
//
// It runs on the launcher's own frame loop rather than in a goroutine of its own, so a pod whose
// restore is confirmed has its copy kept before any later frame of that launcher is read; the
// keep is one indexed row of each table.
func (d *launchers) restoreReported(s *launcherSession, report shimwire.LauncherRestored) {
	if d.restored == nil || s.issue == "" {
		return
	}
	if !report.OK {
		d.log.Warn(restoreFailedLine,
			"issue", s.issue, "launcher", s.token, "sequence", report.ID, "restore", report.Release, "error", report.Error)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()
	if err := d.restored(ctx, s.issue, s.podUID, report.Release); err != nil {
		d.log.Warn("launcher bundle: the restored workspace copy could not be kept as the restoring pod's own copy in the store",
			"issue", s.issue, "launcher", s.token, "pod", s.podUID, "restore", report.Release, "error", err)
		return
	}
	d.log.Info("launcher bundle: the pod's launcher restored the issue's saved workspace work, which the database keeps as that pod's own copy",
		"issue", s.issue, "launcher", s.token, "pod", s.podUID, "sequence", report.ID, "restore", report.Release)
}

// bundleFrames are the slices that carry one saved copy of issue's workspace, with every bookmark
// of it but the issue's own branch and its rescue branch (runtime.RestorableBookmarks), and what
// that left out: withheld, each of the two the copy held with the commit it held it on, nil when
// it held neither, which is what the restore's withheldBranchLine names. Every send of either kind
// of copy passes here, so no restore sets `legion/<KEY>` or `legion/<KEY>-rescue`, wherever the
// copy held them. The sequence's id is the issue, the copy's own restore id and the digest of the
// body (shimwire.SplitWorkspaceBundle), so it is the same every time the same copy is sent, which
// is what a pod whose launcher reconnects needs: the launcher answers a slice it has already taken
// under an id it knows with the answer it gave, Held included, and refuses only a payload that
// differs from the one that id carried — which another copy of the same issue never is, since
// every copy of either kind has a restore id of its own and so a sequence of its own
// (internal/launcher's bundleRestore.receive).
func bundleFrames(issue string, b runtime.WorkspaceBundle) (frames []shimwire.LauncherBundle, withheld map[string]string, err error) {
	bookmarks, withheld := runtime.RestorableBookmarks(issue, b.Bookmarks)
	frames, err = shimwire.SplitWorkspaceBundle("restore-"+issue, b.Release, shimwire.WorkspaceBundle{
		Bundle: b.Bundle, Head: b.Head, Tips: b.Tips, Bookmarks: bookmarks, At: b.At,
	})
	return frames, withheld, err
}

// bundleWaiter names the request one slice's answer belongs to, of a launcher_bundle sequence or
// a launcher_leak_check one. A sequence is answered slice by slice, so its id alone would name as
// many answers as it has slices; the launcher keys its own results by the same pair.
func bundleWaiter(sequence string, index int) string { return sequence + "/" + strconv.Itoa(index) }
