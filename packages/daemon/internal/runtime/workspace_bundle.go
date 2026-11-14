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
	// At is when the shim took the bundle.
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
// (store.Store.TakeWorkspaceCopyForRestore). A copy is offered to one pod only — the first pod
// that is not the one its work came off to ask for it — and the ask that reaches that pod closes
// it to every later one, whether the body was sent or refused.
type WorkspaceBundleTake int

const (
	// WorkspaceBundleNone: there is nothing to send this pod and nothing was recorded. The issue
	// was never released, a wake cancelled its release, the copy has had its one attempt already,
	// or this pod is the one the work was taken off or the one whose restore of it is confirmed.
	WorkspaceBundleNone WorkspaceBundleTake = iota
	// WorkspaceBundleSend: this pod takes the copy, and the row records the attempt as its
	// before a byte of it leaves the daemon.
	WorkspaceBundleSend
	// WorkspaceBundleRefusedOlder: this pod is not the one the work came off and was not created
	// after the copy was taken, so it is not the pod the release's teardown replaced — or it is,
	// under a node clock that runs ahead of the API server's — and nothing is sent to it. The
	// refusal is the copy's one attempt: no later pod is sent it either, since the roles of this
	// pod go on working and pushing, and a copy applied over that work would `jj edit` the
	// workspace back to the copy's head behind it. The work stays in the database for a person to
	// recover, and a release bundle also holds the issue's pod (workflow.IdleHoldBundleUnrestored).
	WorkspaceBundleRefusedOlder
	// WorkspaceBundleRefusedPushed: the copy is a running copy taken before the newest push of
	// the issue's branch the daemon recorded, so it is sent to no pod (DecideWorkspaceRestore);
	// a release bundle is never refused so. The refusal is the copy's one attempt and its bytes
	// stay for an operator, as RefusedOlder's do.
	WorkspaceBundleRefusedPushed
	// WorkspaceBundleSuperseded: the issue's other copy is the copy this pod is offered — the
	// newer one, or the release bundle beside a running copy off the bundle's own pod, which is
	// that pod's last copy (DecideWorkspaceRestore) — so this one is closed unsent. The pods
	// after this one hold the descendants of the work it restores, so a copy left open for them
	// would be applied over everything pushed in between. Its bytes stay for an operator too.
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
	case WorkspaceBundleRefusedOlder:
		return "WorkspaceBundleRefusedOlder"
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
// (store.Store.RecordIssuePush): At is the daemon's clock when it applied the push webhook, and
// Head the commit the push left. The zero value is an issue with no push recorded.
type WorkspacePush struct {
	At   time.Time
	Head string
}

// Recorded is whether the daemon has recorded a push of the issue's branch at all.
func (p WorkspacePush) Recorded() bool { return !p.At.IsZero() }

// WorkspaceCopyHeld is one copy as the restore's decision reads it off its row: when the shim took
// it (WorkspaceBundle.At), whether a pod has had the copy's one attempt already, and the uid of the
// pod its work came off (WorkspaceBundle.PodUID). Nothing else of the row enters the decision. The
// zero value is a copy the issue does not hold, as the zero WorkspacePush is a push not recorded:
// a shim stamps every copy it takes with the time it took it.
type WorkspaceCopyHeld struct {
	At     time.Time
	Spent  bool
	Source string
}

// Held is whether the issue holds the copy at all.
func (c WorkspaceCopyHeld) Held() bool { return !c.At.IsZero() }

// DecideWorkspaceRestore is the whole of the choice the restore of a new pod makes between the
// two copies of its issue's workspace. It reads what each copy's row says of it, the newest push
// of the issue's branch the daemon recorded, and the pod asking: its uid, podUID, and the
// creationTimestamp the API server stamped it with, podCreated. It answers what the ask does to
// each copy, and the kind of the copy it offers: the newer of the copies the issue holds, but the
// release bundle beside a running copy off the bundle's own pod, and empty when it holds neither.
// It is a pure function so that it is one decision, read in one place: the store calls it inside
// the transaction that holds both rows and records every take it answers but WorkspaceBundleNone
// (store.Store.TakeWorkspaceCopyForRestore), and a test outside the store drives the very same
// decision against real git and jj.
//
// The newer copy is offered and the older superseded (WorkspaceBundleSuperseded). `at` orders
// them because it is the one thing both carry and the only one that says which work descends
// from which: a running copy is taken while the roles work, a release bundle while they are idle,
// just before the release stops them, and either can be the newer, since a pod can be released
// minutes after a copy or run for hours after a cancelled release left one behind. A tie goes to
// the release bundle, which is taken from a workspace whose roles are idle: a copy of the same
// second holds no more than the bundle does, and restoring the bundle also lifts the hold its row
// puts on the issue's pod (workflow.IdleHoldBundleUnrestored). Which copy is the newer is a fact
// of the two copies and not of the pod asking or of the push, so the comparison reads every copy
// the issue holds, spent or not, whichever pod its work came off and whether or not the push
// refuses it: an older copy is never sent because the newer one went to an earlier pod, nor
// because the newer one is a running copy the push refuses, which would put the workspace further
// back still than the copy the push refused.
//
// The older is superseded, which spends its attempt, rather than left, because a copy left with
// its attempt unspent is a copy some later pod can still be sent, and that pod's workspace holds
// the descendants of the work this pod is about to restore: the older copy would then be applied
// over everything pushed in between.
//
// A running copy off the pod the release bundle came off never supersedes that bundle, whatever
// their times: the bundle is offered and the copy is closed as an older copy is. Once an idle
// release has saved its bundle, that bundle is the pod's last copy. The release's rows run the
// save first, then a stop per role, then the teardown (internal/daemon's idleReleaseRows), so the
// pod's roles are still live after the save, and a copy taken off them in that window, such as
// the copy a push the daemon records there asks for, is the later of the two by the clock. It
// holds no work the bundle lacks: the roles were idle, work given to the pod before its first
// role stops cancels the release and drops its bundle (store.Store.CancelIssueRelease), and work
// given after that waits for the release to end. The daemon stores no such copy
// (store.Store.PutRunningWorkspaceCopy), and this rule answers any row a daemon that stored one
// left behind. Offered, such a copy would be sent to the next pod and the bundle closed unsent,
// and a row of workspace_bundles of any outcome holds the issue's pod
// (workflow.IdleHoldBundleUnrestored), so no idle release of the issue would run again; a push
// after both would refuse the copy, and that pod would be sent nothing at all. The copy's own
// take follows the rules below as any other copy's does: it is superseded, or refused by the push
// when it was taken before it.
//
// A running copy taken before the newest recorded push is refused (WorkspaceBundleRefusedPushed),
// the offered one or the older. No restore sets the issue's branch `legion/<KEY>`
// (RestorableBookmarks), so not even such a copy can move the branch; this rule is the second
// line, because the copy would still `jj edit` the workspace back onto work behind what GitHub
// holds and set its other bookmarks there, leaving the role to bring its work forward over the
// push. A running copy is taken while the roles work and push, so a push after it is normally the
// roles' own push of that copy's work and more: an update round replaces a pod just after its
// roles push. The comparison is the copy's `at`, by the clock of the node its pod ran on, against
// the daemon's own clock when it applied the push; the daemon takes a copy of the workspace each
// time it records a push (workflowRuntime.copyWorkspaceAtPush), so the issue's newest copy is
// normally one taken after it. An issue with no push recorded refuses nothing by this rule, since
// nothing newer is known to be on GitHub. The rule alone misses two copies that hold the branch
// behind GitHub's or beside it, and in both the withheld branch is what keeps the pushed commits
// on GitHub: a copy taken after a push from outside the pod that the workspace never fetched, and
// a copy taken before a push the daemon has not recorded.
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
// The offered copy, when the push does not refuse it, is sent (WorkspaceBundleSend) to a pod
// created after it was taken, and refused to any other (WorkspaceBundleRefusedOlder), which spends
// its attempt: such a pod has been working off its own fetch since before the copy was taken, and
// a copy left open for the pod after it would be applied over work pushed since. The two instants
// come from two clocks — podCreated is the creationTimestamp the Kubernetes API server stamped,
// the copy's `at` the node's clock when the shim took it — and a creationTimestamp has one-second
// resolution, so the copy is read to its own second and a pod created inside that second is taken
// as the newer. A node running ahead of the API server stamps the copy in the replacement pod's
// future and that pod is refused, which costs the restore and loses no work: the direction to
// fail in.
func DecideWorkspaceRestore(bundle, running WorkspaceCopyHeld, push WorkspacePush, podUID string, podCreated time.Time) (bundleTake, runningTake WorkspaceBundleTake, offered WorkspaceCopyKind) {
	// A running copy off the bundle's own pod is never the newer (above), so only a copy off
	// another pod is weighed against the bundle by time.
	switch {
	case running.Held() && (!bundle.Held() || (running.Source != bundle.Source && running.At.After(bundle.At))):
		offered = WorkspaceCopyRunning
	case bundle.Held():
		offered = WorkspaceCopyReleaseBundle
	}
	take := func(kind WorkspaceCopyKind, c WorkspaceCopyHeld) WorkspaceBundleTake {
		switch {
		case !c.Held(), c.Spent, c.Source == podUID:
			return WorkspaceBundleNone
		case kind == WorkspaceCopyRunning && push.Recorded() && c.At.Before(push.At):
			return WorkspaceBundleRefusedPushed
		case kind != offered:
			return WorkspaceBundleSuperseded
		case c.At.Truncate(time.Second).After(podCreated):
			return WorkspaceBundleRefusedOlder
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
// tip GitHub holds (rescue.Target). The shim's bundler keeps a local bookmark of it, like any
// other, where it is on a commit no remote branch holds, and a rescue the pod pushes after the
// copy was taken moves GitHub's tip past that commit. A restore that set the bookmark back there
// would leave it behind GitHub's tip: a role's `jj git push` of it moves GitHub's branch backwards
// and drops the later rescue, and the next rescue push leaves the bookmark conflicted, which jj
// then refuses to push.
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
// each a take of WorkspaceBundleNone when the ask recorded nothing on it — the issue holds no such
// copy, its attempt is spent already, or its work came off this very pod; the kind of the copy
// the decision offered (DecideWorkspaceRestore), empty when the issue holds neither; and the push
// the decision read. The decision sends no copy but the offered one, so the send is
// Of(Offered) when its take is WorkspaceBundleSend, and it alone carries its body.
type WorkspaceRestore struct {
	ReleaseBundle WorkspaceCopyTaken
	Running       WorkspaceCopyTaken
	Offered       WorkspaceCopyKind
	Push          WorkspacePush
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
// line names and a copy runs to tens of megabytes — and the size the row stored. The zero value is
// a copy the ask recorded nothing on.
type WorkspaceCopyTaken struct {
	Take   WorkspaceBundleTake
	Bundle WorkspaceBundle
	Size   int
}
