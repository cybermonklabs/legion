package runtime

import (
	"maps"
	"strconv"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// MaxWorkspaceBundleBytes is the largest bundle a release carries. A bundle above it stops the
// release, which is recorded with its reason and leaves the pod where it is: the work a role has
// not pushed is kept at the cost of a pod, never dropped to save one. 40 MiB fits the worker
// stream's reassembly bound with room, and a change that large and still unpushed is rare enough
// that holding the pod for it is the cheaper mistake.
const MaxWorkspaceBundleBytes = 40 << 20

// WorkspaceBundle is the unpushed work of one issue's workspace, saved when its pod is released
// and restored into the workspace of the pod that replaces it. The bundle itself is a git bundle
// holding every visible commit no remote branch holds, on any head, and the working copy's own
// change, with the commits a new pod's workspace fetch already holds as its bases.
//
// Head is the commit the bundle's working copy sat on, Bookmarks maps each local bookmark that
// points into the saved set to its commit, and Tips names the heads the bundle carries, so the
// restore can make each visible again: a bundle that only unbundles leaves every saved head out of
// jj's view, since jj's visible heads live in .jj/repo, which dies with the pod. The restore sets
// every bookmark the bundle holds but the issue's own branch and its rescue branch
// (RestorableBookmarks).
type WorkspaceBundle struct {
	// Bundle is the git bundle's bytes.
	Bundle []byte
	// Head is the working-copy commit id the bundle was taken at.
	Head string
	// Tips are the commit ids of the heads the bundle carries.
	Tips []string
	// Bookmarks maps a local bookmark name to the commit id it pointed at.
	Bookmarks map[string]string
	// At is when the copy's take began, by the store's clock: the store's time read before the
	// shim's bundler read the workspace (store.Store.BeginWorkspaceCopy), which each write of a
	// copy stores as the row's `at` (store.Store.PutWorkspaceBundle,
	// store.Store.PutRunningWorkspaceCopy), and which a read fills back in from the row. The shim
	// fills it with the time it read the workspace, by the clock of the node its pod runs on, and
	// the store keeps none of that. The restore compares it with times of that same clock alone
	// (DecideWorkspaceRestore). A copy left for the next pod is answered with the time the store
	// wrote it here instead (WorkspaceCopyTaken).
	At time.Time
	// Release is the id of the release that saved the row this bundle was read from, which the
	// store fills on the read. The save takes that id as an argument of its own
	// (store.Store.PutWorkspaceBundle): the bundle itself is the shim's, built in a pod that
	// knows nothing of the release, and the id is the release's. The restore of the pod that
	// replaces the released one carries it to the launcher and back, so the report that confirms
	// a restore names the release whose work it put back and keeps no other
	// (internal/runtime/sandbox's restoreReported).
	Release int64
	// PodUID is the uid of the pod the work was taken off: the issue pod the save's own
	// BundleWorkspace asked, which the runtime fills because only it knows which pod the role
	// it asked was running in (sandbox's Runtime.BundleWorkspace reads it off the claim's
	// locator). The store keeps it on the row (store.Store.PutWorkspaceBundle) and refuses that
	// pod by it: a role relaunched in the pod being released asks for the bundle like any other
	// start, its workspace is the one the work came from, and it must leave the attempt for the
	// pod that replaces it. The read fills it back in from the row.
	PodUID string
}

// Size is the bundle's length in bytes, which the release records and the 40 MiB bound reads.
func (b WorkspaceBundle) Size() int { return len(b.Bundle) }

// WorkspaceBundleTake is what one pod's ask did to one saved copy of an issue's workspace
// (store.Store.TakeWorkspaceCopyForRestore). A copy is offered to one pod only — the first pod to
// ask for it that is not the one its work came off and that first asked at or after the copy was
// written — and the ask that reaches that pod closes it to every later one, whether the body was
// sent or refused.
type WorkspaceBundleTake int

const (
	// WorkspaceBundleNone: there is nothing to send this pod and nothing was recorded. The issue
	// was never released, a wake cancelled its release, the copy has had its one attempt already,
	// or this pod is the one the work was taken off or the one whose restore of it is confirmed.
	WorkspaceBundleNone WorkspaceBundleTake = iota
	// WorkspaceBundleSend: this pod takes the copy, and the row records the attempt as its
	// before a byte of it leaves the daemon.
	WorkspaceBundleSend
	// WorkspaceBundleLeftForNextPod: the copy is the one this pod's ask is offered, and it was
	// written after this pod first asked for a restore, so nothing is sent to this pod and nothing
	// is recorded on the copy. This pod has been working since that first ask, and the copy would
	// `jj edit` its workspace back over that work; the copy's one attempt stays for the next pod
	// of the issue, whose first ask comes after the copy was written (DecideWorkspaceRestore).
	WorkspaceBundleLeftForNextPod
	// WorkspaceBundleRefusedPushed: the copy is a running copy whose take began before the newest
	// push of the issue's branch the daemon recorded, so it is sent to no pod
	// (DecideWorkspaceRestore); a release bundle is never refused so. The refusal is the copy's
	// one attempt, and the work stays in the database for a person to recover.
	WorkspaceBundleRefusedPushed
	// WorkspaceBundleSuperseded: the issue's other copy is the newer one and is the copy this
	// pod is offered, so this one is closed unsent. The pods after this one hold the
	// descendants of the work it restores, so a copy left open for them would be applied over
	// everything pushed in between. Its bytes stay for an operator too.
	WorkspaceBundleSuperseded
)

// String is the take by its constant's name, so a test that finds the wrong take, and a log line
// that writes one, prints a word a reader can find in this file rather than a number. A value
// outside the five is printed with its number, since it is no take this file declares.
func (t WorkspaceBundleTake) String() string {
	switch t {
	case WorkspaceBundleNone:
		return "WorkspaceBundleNone"
	case WorkspaceBundleSend:
		return "WorkspaceBundleSend"
	case WorkspaceBundleLeftForNextPod:
		return "WorkspaceBundleLeftForNextPod"
	case WorkspaceBundleRefusedPushed:
		return "WorkspaceBundleRefusedPushed"
	case WorkspaceBundleSuperseded:
		return "WorkspaceBundleSuperseded"
	}
	return "WorkspaceBundleTake(" + strconv.Itoa(int(t)) + ")"
}

// WorkspaceCopyKind names which of an issue's two saved copies of its workspace one is, which is
// also the name every line of the restore calls it by, so that an operator reading one knows
// which of the issue's rows it is about.
type WorkspaceCopyKind string

const (
	// WorkspaceCopyReleaseBundle is the bundle the issue's last idle release saved while its
	// roles were idle, just before it stopped them (store.Store.PutWorkspaceBundle).
	WorkspaceCopyReleaseBundle WorkspaceCopyKind = "release bundle"
	// WorkspaceCopyRunning is the copy the daemon took while the issue's pod ran, at a turn's
	// end, inside a long turn and at each push the daemon recorded
	// (store.Store.PutRunningWorkspaceCopy).
	WorkspaceCopyRunning WorkspaceCopyKind = "running copy"
)

// WorkspacePush is the newest push of an issue's branch `legion/<KEY>` the daemon has recorded
// (store.Store.RecordIssuePush): At is the store's clock when it recorded the push, in the push
// fact's own transaction, and Head the commit the push left. The zero value is an issue with no
// push recorded.
type WorkspacePush struct {
	At   time.Time
	Head string
}

// Recorded is whether the daemon has recorded a push of the issue's branch at all.
func (p WorkspacePush) Recorded() bool { return !p.At.IsZero() }

// WorkspaceCopyHeld is one copy as the restore's decision reads it off its row: its two times,
// both by the store's clock — At, when its take began (WorkspaceBundle.At), and Written, when the
// store wrote it — whether a pod has had the copy's one attempt already, and the uid of the pod its
// work came off (WorkspaceBundle.PodUID). Nothing else of the row enters the decision. The zero
// value is a copy the issue does not hold, as the zero WorkspacePush is a push not recorded: the
// store writes no copy without the time its take began.
//
// The two times answer two questions. At says what the work is as of: the bundler read the
// workspace after the take began, so At orders the work of the issue's two copies
// (NewerWorkspaceCopy) and says whether a running copy can be older than a push (the push rule).
// Written says whether the copy was there to be read at a pod's first ask (the late-copy rule):
// that ask and every write of a copy take the issue's copy lock first and read the clock once
// they hold it (store's lockWorkspaceCopies), so the order of the write and the ask is the order
// of their times, whenever the copy's take began.
type WorkspaceCopyHeld struct {
	At      time.Time
	Written time.Time
	Spent   bool
	Source  string
}

// Held is whether the issue holds the copy at all.
func (c WorkspaceCopyHeld) Held() bool { return !c.At.IsZero() }

// NewerWorkspaceCopy is which of an issue's two copies of its workspace holds the newer work, and
// empty when the issue holds neither. `at` orders them because it is the one thing both carry and
// the only one that says which work descends from which: a running copy is taken while the roles
// work, a release bundle while they are idle, just before the release stops them, and either can
// be the newer, since a pod can be released minutes after a copy or run for hours after a
// cancelled release left one behind. Both are the store's clock when the copy's take began, before
// its bundler read the workspace, so the copy read later holds the newer work whichever of the two
// writes landed first. A tie goes to the release bundle, which is taken from a workspace whose
// roles are idle: a copy taken at the same instant holds no more than the bundle does, and
// restoring the bundle also lifts the hold its row puts on the issue's pod
// (workflow.IdleHoldBundleUnrestored).
//
// Which copy is the newer is a fact of the two copies alone, so it reads nothing else of them:
// not whether a copy's attempt is spent, not the pod its work came off, and nothing of a pod
// asking or of a push. The restore offers the copy it answers (DecideWorkspaceRestore), and the
// two writes that turn a release bundle into the issue's running copy leave a running copy it
// answers as it is (store.Store.KeepRestoredWorkspaceBundle, store.Store.CancelIssueRelease).
func NewerWorkspaceCopy(bundle, running WorkspaceCopyHeld) WorkspaceCopyKind {
	switch {
	case running.Held() && (!bundle.Held() || running.At.After(bundle.At)):
		return WorkspaceCopyRunning
	case bundle.Held():
		return WorkspaceCopyReleaseBundle
	}
	return ""
}

// DecideWorkspaceRestore is the whole of the choice the restore of a new pod makes between the
// two copies of its issue's workspace. It reads what each copy's row says of it, the newest push
// of the issue's branch the daemon recorded, and the pod asking: its uid, podUID, and when it
// first asked for a restore, firstAsked. It answers what the ask does to each copy, and the kind
// of the copy it offers, which is the newer of the copies the issue holds, or empty when it holds
// neither. It is a pure function so that it is one decision, read in one place: the store calls
// it inside the transaction that holds both rows and records every take it answers but
// WorkspaceBundleNone and WorkspaceBundleLeftForNextPod (store.Store.TakeWorkspaceCopyForRestore),
// and a test outside the store drives the very same decision against real git and jj.
//
// Every time it compares is the store's clock, so no two machines' clocks meet in it: a copy's
// `at` is when its take began, read before its bundler read the workspace
// (store.Store.BeginWorkspaceCopy) and stored by its write (store.Store.PutRunningWorkspaceCopy,
// store.Store.PutWorkspaceBundle), its written time when the store wrote it, under the issue's
// copy lock (and both are the time of the move for the moves that keep a release bundle as a
// running copy), a push's At when the store recorded the push in the push fact's own transaction
// (store.Store.RecordIssuePush), and firstAsked when the store recorded this pod's first ask for a
// restore (store.Store.TakeWorkspaceCopyForRestore).
//
// The newer copy (NewerWorkspaceCopy) is offered and the older superseded
// (WorkspaceBundleSuperseded). The comparison reads every copy the issue holds, spent or not,
// whichever pod its work came off and whether or not the push refuses it: an older copy is never
// sent because the newer one went to an earlier pod, nor because the newer one is a running copy
// the push refuses, which would put the workspace further back still than the copy the push
// refused.
//
// The older is superseded, which spends its attempt, rather than left, because a copy left with
// its attempt unspent is a copy some later pod can still be sent, and that pod's workspace holds
// the descendants of the work this pod is about to restore: the older copy would then be applied
// over everything pushed in between.
//
// A running copy whose take began before the newest recorded push is refused
// (WorkspaceBundleRefusedPushed), the offered one or the older. No restore sets the issue's branch
// `legion/<KEY>` (RestorableBookmarks), so not even such a copy can move the branch; this rule is
// the second line, because the copy would still `jj edit` the workspace back onto work behind what
// GitHub holds and set its other bookmarks there, leaving the role to bring its work forward over
// the push. A running copy is taken while the roles work and push, so a push after it is normally
// the roles' own push of that copy's work and more: an update round replaces a pod just after its
// roles push. The daemon takes a copy of the workspace each time it records a push
// (workflowRuntime.copyWorkspaceAtPush), so the issue's newest copy is normally one taken after
// it.
//
// The rule reads when the copy's take began and not when the store wrote it, because the bundler
// reads the workspace after the take begins: a copy whose take began before the push's record can
// have read the workspace before the pushed commits were made, however late its write lands.
// Stamped at its write, a copy read before the push and written after the push's record would
// carry the later time and pass this rule. The daemon stores no copy that a push overtook while it
// was being built (internal/daemon's runningCopies.pushed), but it marks the copy only once the
// push's transaction has committed, so a copy whose check runs just before that mark and whose
// write lands just after the record is stored all the same, and a pod that ends before the copy
// owed behind the push is stored would leave the next pod restored behind the pushed commits.
//
// An issue with no push recorded refuses nothing by this rule, since nothing newer is known to be
// on GitHub. The rule alone misses two copies that hold the branch behind GitHub's or beside it,
// and in both the withheld branch is what keeps the pushed commits on GitHub: a copy taken after a
// push from outside the pod that the workspace never fetched, and a copy taken before a push the
// daemon has not recorded.
//
// A release bundle is never refused by the push: the other rules alone offer, supersede and send
// it. It is the pod's last copy, taken while the issue's roles were idle and just before the
// release stopped them, so a push the daemon records after it is no push of those roles' work: it
// is a push from outside the pod — GitHub's "Update branch", a person's push — whose commits the
// bundle lacks, or a push the daemon applied late, such as one whose webhook was lost and which
// the per-issue read rebuilt, whose commits the bundle holds. Restoring the bundle over either
// loses no pushed commit, since the branch stays on what GitHub holds and the role brings its work
// forward over the push. Refusing it would protect nothing and cost the issue both its work and
// its pod: the disk the bundle came off is gone, and a bundle no pod restored holds the issue's
// pod (workflow.IdleHoldBundleUnrestored), so a push during any release that saved one would stop
// every later idle release of that issue. A bundle left unrestored while the issue's work went on,
// by a pod that came back without restoring it, is the newer-copy rule's to close: that pod's
// running copies, the copy taken at each push among them, are newer than it.
//
// Two rules answer a copy WorkspaceBundleNone whatever the rules above say of it, and the store
// records nothing on such a copy:
//
//   - One attempt per copy. The launcher's own record of the restore it ran lives in the process
//     that ran it: a roles container that restarts on the pod's own disk comes back with that
//     record gone and the workspace as the roles left it, hours of work past the copy, and a copy
//     sent again there would `jj edit` back to its head. The attempt is recorded before any slice
//     leaves the daemon, so a send that fails halfway, a restore that fails and a restore whose
//     report never arrived all leave the same row: one no pod is sent again.
//   - None spent by the pod the work came off (Source). Every start of every role asks for its
//     pod (sandbox's launchers.start), and that pod goes on starting roles — a release stops them
//     and a role relaunched before the teardown starts again, and a cadence copies a pod that is
//     working. Its workspace is the work, so it is answered nothing and the row is left as it was,
//     with the attempt still there for the pod that replaces it.
//
// The offered copy, when the push does not refuse it, is sent (WorkspaceBundleSend) to a pod that
// first asked for a restore at or after the store wrote the copy, and to a pod that first asked
// before, nothing is sent and nothing is recorded (WorkspaceBundleLeftForNextPod). This rule reads
// the copy's write and not its take's begin: a pod's first ask and every write of a copy take the
// issue's copy lock first, so a copy written before the ask had committed when the ask read the
// copies, and one written after it had not, whenever its take began. Every pod's launcher asks at
// its first role start (sandbox's launchers.start), so a pod that existed before a copy was
// written asked before it, and has been working since. That copy is a late copy: one read off the
// pod before this one, before that pod ended, and written after this pod's first ask, its take
// begun before that ask or after it. Applying it here would `jj edit` this pod's workspace back
// over the work done since that ask, so it is not sent here, and it keeps its one attempt for the
// next pod of the issue, whose first ask comes after the write. Its work reaches that pod only
// while this pod takes no copy of its own: this pod's next copy replaces it, and a copy of this
// pod that finds nothing unpushed drops it. That loss needs a write stalled through a whole pod
// start, and is accepted.
func DecideWorkspaceRestore(bundle, running WorkspaceCopyHeld, push WorkspacePush, podUID string, firstAsked time.Time) (bundleTake, runningTake WorkspaceBundleTake, offered WorkspaceCopyKind) {
	offered = NewerWorkspaceCopy(bundle, running)
	take := func(kind WorkspaceCopyKind, c WorkspaceCopyHeld) WorkspaceBundleTake {
		switch {
		case !c.Held(), c.Spent, c.Source == podUID:
			return WorkspaceBundleNone
		case kind == WorkspaceCopyRunning && push.Recorded() && c.At.Before(push.At):
			return WorkspaceBundleRefusedPushed
		case kind != offered:
			return WorkspaceBundleSuperseded
		// Both times here are the store's clock. The offered copy is sent to a pod whose first
		// ask for a restore is at or after the copy's write. To a pod that first asked before the
		// copy was written, nothing is sent and nothing is recorded: that pod has been working
		// since its first ask, so the copy is not applied over that work, and the copy keeps its
		// one attempt for the next pod, whose first ask comes after the copy was written. The
		// write and not the take's begin, since the copy lock orders the write and the ask.
		case c.Written.After(firstAsked):
			return WorkspaceBundleLeftForNextPod
		}
		return WorkspaceBundleSend
	}
	return take(WorkspaceCopyReleaseBundle, bundle), take(WorkspaceCopyRunning, running), offered
}

// RestorableBookmarks is the bookmarks a restore of a saved copy of issue's workspace sets: every
// one of bookmarks, the copy's own, but the issue's branch `legion/<KEY>` (claim.IssueBranch) and
// its rescue branch `legion/<KEY>-rescue` (claim.RescueBranch). The launcher sets each bookmark it
// is sent with `jj bookmark set --allow-backwards`, so a restore that set either would move it off
// where the new pod's own fetch put it, on what GitHub holds, and onto the copy's commit.
//
// The issue's branch is withheld for the commits GitHub holds on it. The copy's value of it can
// sit behind GitHub's or beside it — a release bundle taken before a push from outside the pod, a
// copy taken after such a push that the workspace never fetched, or a copy taken before a push the
// daemon has not recorded — so a restore that set it would put the branch where the role's next
// push removes the pushed commits from GitHub. Left out, the copy's work comes back as visible
// heads (WorkspaceBundle.Tips) and as the working copy (WorkspaceBundle.Head), and the role's own
// `jj bookmark set legion/<KEY> -r @-` then moves the branch forward onto that work or is refused
// by jj as a move backwards or sideways.
//
// The rescue branch is withheld because it only moves forward: each rescue is one commit on the
// tip GitHub holds (claim.RescueBranch). No daemon of this release pushes one, but an earlier
// daemon's rescue branch stays on GitHub and a role may push it. The shim's bundler keeps a local
// bookmark of it, like any other, where it is on a commit no remote branch holds, and a rescue
// pushed after the copy was taken moves GitHub's tip past that commit. A restore that set the
// bookmark back there would leave it behind GitHub's tip: a role's `jj git push` of it moves
// GitHub's branch backwards and drops the later rescue, and the next rescue push leaves the
// bookmark conflicted, which jj then refuses to push.
//
// It also answers what it left out, withheld: each of the two names that bookmarks holds, with
// the commit it holds it on, and nil when bookmarks holds neither. The one line that tells an
// operator what the send withheld is written from this answer (internal/runtime/sandbox's
// bundleFrames and restoreWorkspace), so it names exactly what the send left out.
//
// The map answered is bookmarks itself when it holds neither name and a copy without them when it
// holds either, so bookmarks is never written.
func RestorableBookmarks(issue string, bookmarks map[string]string) (restorable, withheld map[string]string) {
	restorable = bookmarks
	for _, name := range [...]string{claim.IssueBranch(issue), claim.RescueBranch(issue)} {
		commit, held := bookmarks[name]
		if !held {
			continue
		}
		if withheld == nil {
			restorable, withheld = maps.Clone(bookmarks), make(map[string]string, 2)
		}
		delete(restorable, name)
		withheld[name] = commit
	}
	return restorable, withheld
}

// WorkspaceRestore is what one pod's ask did to the copies of its issue's workspace
// (SavedWorkspaceBundle): what it did to the release bundle and what it did to the running copy,
// each a take of WorkspaceBundleNone when the ask had nothing to answer of it — the issue holds no
// such copy, its attempt is spent already, or its work came off this very pod; the kind of the
// copy the decision offered (DecideWorkspaceRestore), empty when the issue holds neither; the push
// the decision read; and when this pod first asked for a restore, by the store's clock, which is
// the time the decision weighed the offered copy against. The decision sends no copy but the
// offered one, so the send is Of(Offered) when its take is WorkspaceBundleSend, and it alone
// carries its body.
type WorkspaceRestore struct {
	ReleaseBundle WorkspaceCopyTaken
	Running       WorkspaceCopyTaken
	Offered       WorkspaceCopyKind
	Push          WorkspacePush
	FirstAsked    time.Time
}

// Of is what the ask did to the issue's copy of kind: ReleaseBundle or Running, and the zero take,
// nothing recorded, for any other kind — which is what Of(Offered) answers when the issue holds
// neither copy and Offered is empty.
func (r WorkspaceRestore) Of(kind WorkspaceCopyKind) WorkspaceCopyTaken {
	switch kind {
	case WorkspaceCopyReleaseBundle:
		return r.ReleaseBundle
	case WorkspaceCopyRunning:
		return r.Running
	}
	return WorkspaceCopyTaken{}
}

// WorkspaceCopyTaken is what the ask did to one copy: the attempt's outcome, the row as the take
// read it back — its body only when it is being sent, since a refusal answers the facts its log
// line names and a copy runs to tens of megabytes — and the size the row stored. A copy left for
// the next pod (WorkspaceBundleLeftForNextPod) has nothing recorded on it and answers its two
// times alone: its Bundle's At, when its take began, and Written, when the store wrote it, which
// is the time the decision compared with the pod's first ask and which its log line names. The
// zero value is a copy the ask had nothing to answer of.
type WorkspaceCopyTaken struct {
	Take    WorkspaceBundleTake
	Bundle  WorkspaceBundle
	Size    int
	Written time.Time
}
