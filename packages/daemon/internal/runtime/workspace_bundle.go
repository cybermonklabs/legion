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
// every bookmark the bundle holds but the issue's own branch (RestorableBookmarks).
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
	// a restore names the release whose work it put back and deletes no other
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
	// was never released, its restore is confirmed, a wake cancelled its release, the copy has
	// had its one attempt already, or this pod is the one the work was taken off.
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
	// WorkspaceBundleRefusedPushed: the copy was taken before the newest push of the issue's
	// branch the daemon recorded, so it is sent to no pod (DecideWorkspaceRestore). The refusal
	// is the copy's one attempt and its bytes stay for an operator, as RefusedOlder's do.
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
	// WorkspaceCopyReleaseBundle is the bundle the issue's last idle release saved after its
	// roles stopped (store.Store.PutWorkspaceBundle).
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
// each copy, and the kind of the copy it offers, which is the newer of the copies the push does
// not refuse, or empty when there is none. It is a pure function so that it is one decision, read
// in one place: the store calls it inside the transaction that holds both rows and records every
// take it answers but WorkspaceBundleNone (store.Store.TakeWorkspaceCopyForRestore), and a test
// outside the store drives the very same decision against real git and jj.
//
// A copy taken before the newest recorded push is refused (WorkspaceBundleRefusedPushed),
// whichever kind it is. No restore sets the issue's branch `legion/<KEY>` (RestorableBookmarks),
// so not even such a copy can move the branch; this rule is the second line, because the copy
// would still `jj edit` the workspace back onto work behind what GitHub holds and set its other
// bookmarks there, leaving the role to bring its work forward over the push. A running copy is
// taken while the roles work and push, so it is the common case; a release bundle is taken after
// the roles stop and is normally newer than any push, but one left unrestored while the issue's
// work went on — a roll back to an earlier daemon and forward again, a pod that came back without
// restoring it — is not. The comparison is the copy's `at`, by the clock of the node its pod ran
// on, against the daemon's own clock when it applied the push; the daemon takes a copy of the
// workspace each time it records a push (workflowRuntime.copyWorkspaceAtPush), so the issue's
// newest copy is normally one taken after it. An issue with no push recorded refuses nothing by
// this rule, since nothing newer is known to be on GitHub. The rule alone misses two copies that
// hold the branch behind GitHub's or beside it, and in both the withheld branch is what keeps the
// pushed commits on GitHub: a copy taken after a push from outside the pod that the workspace
// never fetched, and a copy taken before a push the daemon has not recorded.
//
// Of the copies left, the newer is offered and the older superseded (WorkspaceBundleSuperseded).
// `at` orders them because it is the one thing both carry and the only one that says which work
// descends from which: a running copy is taken while the roles work, a release bundle after they
// stop, and either can be the newer, since a pod can be released minutes after a copy or run for
// hours after a cancelled release left one behind. A tie goes to the release bundle, which is
// taken from a workspace whose roles have stopped: a copy of the same second was taken before
// those roles ended, so it holds no more than the bundle does, and restoring the bundle also lifts
// the hold its row puts on the issue's pod (workflow.IdleHoldBundleUnrestored). Which copy is the
// newer is a fact of the two copies and not of the pod asking, so the comparison reads every copy
// the push does not refuse, spent or not and whichever pod its work came off: an older copy is
// never sent because the newer one went to an earlier pod.
//
// The older is superseded, which spends its attempt, rather than left, because a copy left with
// its attempt unspent is a copy some later pod can still be sent, and that pod's workspace holds
// the descendants of the work this pod is about to restore: the older copy would then be applied
// over everything pushed in between.
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
// The offered copy is sent (WorkspaceBundleSend) to a pod created after it was taken, and refused
// to any other (WorkspaceBundleRefusedOlder), which spends its attempt: such a pod has been
// working off its own fetch since before the copy was taken, and a copy left open for the pod
// after it would be applied over work pushed since. The two instants come from two clocks —
// podCreated is the creationTimestamp the Kubernetes API server stamped, the copy's `at` the
// node's clock when the shim took it — and a creationTimestamp has one-second resolution, so the
// copy is read to its own second and a pod created inside that second is taken as the newer. A
// node running ahead of the API server stamps the copy in the replacement pod's future and that
// pod is refused, which costs the restore and loses no work: the direction to fail in.
func DecideWorkspaceRestore(bundle, running WorkspaceCopyHeld, push WorkspacePush, podUID string, podCreated time.Time) (bundleTake, runningTake WorkspaceBundleTake, offered WorkspaceCopyKind) {
	refused := func(c WorkspaceCopyHeld) bool { return push.Recorded() && c.At.Before(push.At) }
	open := func(c WorkspaceCopyHeld) bool { return c.Held() && !refused(c) }
	switch {
	case open(bundle) && open(running) && running.At.After(bundle.At):
		offered = WorkspaceCopyRunning
	case open(bundle):
		offered = WorkspaceCopyReleaseBundle
	case open(running):
		offered = WorkspaceCopyRunning
	}
	take := func(kind WorkspaceCopyKind, c WorkspaceCopyHeld) WorkspaceBundleTake {
		switch {
		case !c.Held(), c.Spent, c.Source == podUID:
			return WorkspaceBundleNone
		case refused(c):
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
// one of bookmarks, the copy's own, but the issue's branch `legion/<KEY>` (claim.IssueBranch). The
// launcher sets each bookmark it is sent with `jj bookmark set --allow-backwards`, and the copy's
// value of the issue's branch can sit behind GitHub's or beside it — after a push from outside the
// pod that the workspace never fetched, or a push the daemon has not recorded — so a restore that
// set it would put the branch where the role's next push removes the pushed commits from GitHub.
// Left out, the branch stays where the new pod's own fetch put it, on what GitHub holds; the
// copy's work comes back as visible heads (WorkspaceBundle.Tips) and as the working copy
// (WorkspaceBundle.Head), and the role's own `jj bookmark set legion/<KEY> -r @-` then moves the
// branch forward onto that work or is refused by jj as a move backwards or sideways.
//
// The map answered is bookmarks itself when it holds no such branch and a copy without it when it
// does, so bookmarks is never written.
func RestorableBookmarks(issue string, bookmarks map[string]string) map[string]string {
	branch := claim.IssueBranch(issue)
	if _, held := bookmarks[branch]; !held {
		return bookmarks
	}
	restorable := maps.Clone(bookmarks)
	delete(restorable, branch)
	return restorable
}

// WorkspaceRestore is what one pod's ask did to the copies of its issue's workspace
// (SavedWorkspaceBundle): one entry per copy the ask recorded an attempt on, the release bundle's
// first; the kind of the copy the decision offered (DecideWorkspaceRestore), which has no entry
// when the ask recorded nothing on it — its attempt spent already, or its work off this very pod;
// and the push the decision read. At most one entry is WorkspaceBundleSend, and it alone carries
// its body.
type WorkspaceRestore struct {
	Copies  []WorkspaceCopyTaken
	Offered WorkspaceCopyKind
	Push    WorkspacePush
}

// WorkspaceCopyTaken is what the ask did to one copy: its kind, the attempt's outcome, the row as
// the take read it back — its body only when it is being sent, since a refusal answers the facts
// its log line names and a copy runs to tens of megabytes — and the size the row stored.
type WorkspaceCopyTaken struct {
	Kind   WorkspaceCopyKind
	Take   WorkspaceBundleTake
	Bundle WorkspaceBundle
	Size   int
}
