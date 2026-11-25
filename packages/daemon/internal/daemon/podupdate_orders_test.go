package daemon

// The ends of an update round over a pod, P, against every kind of member the round can have
// (dispatch://INFRA-4425). Every end names a member by one fact, read by one predicate
// (workOnRoundPod): the member had not answered the round, and its unpushed work may be on P's
// disk, because an agent of it registered a session in P, P's disk holds work from before P (a
// kept volume that an earlier pod of the issue mounted) and the member has a session or a pod in
// its set, or a restart left its set unknown. Each row of the table brings the implementer, as one
// kind of member, to one end of the round over P, and holds the issue's message and the end's log
// line to naming the implementer, or to naming nobody.
//
// The ends (roundPodEnd): the sweep replaces P through a claim whose launch waits on it
// (replacePodForRound); the sweep replaces P through another claim's process, the tester's, whose
// read of the workspace finds work (readRoundWorkspace); P goes on its own, and the runtime
// answers it gone while the claims still record it (endRoundOverAGonePod); P is recreated with
// the exchange identity, and the claims record the pod in its place (endRoundWhosePodWent); P is
// recreated without it, and the runtime refuses each retry again for the pod in its place, which
// the claims then wait on; a launch replaces P (roundPodEndsAtLaunch, endRoundALaunchReplaced);
// and the sweep replaces P by issue while no claim records a pod (replaceIssuePodForRound).
//
// The members (memberKind): an agent of it registered in P and it answered; an agent of it
// registered in P and it did not answer; an agent of it registered in P, and it lost that
// session, its fresh start refused for P with no session, so it waits on P; it was refused for P
// with no session and never registered there; it was refused for P with no session and its retry
// came up in the pod in P's place, where its agent registered; an agent of it registered in P, and
// when P went its relaunch found no session and started fresh in the pod in P's place, whose
// agent has not registered by the end; and the three restart rows the architect asked for
// (2026-11-22T12:14Z), each a machine the daemon's restart built over a stored claim: one whose
// state holds a registered agent in P, which always shows the session that agent registered, so
// the set is unknown (named); one that shows an abandoned session and no registered agent, so the
// set is unknown and the end names it, both where it waits on P with no session at the end and
// where it records a process in the pod in P's place by then; and one that shows no session, so
// the set is known and empty (named nothing). No release came before P in any of these.
//
// Then the members of an issue whose idle release took an earlier pod, the rows INFRA-1484's
// architect asked for (2026-11-22T17:33Z), each held to what main's idle release, which keeps no
// saved workspace (#4869), leaves on P. An agent of each of the implementer and the tester
// registered in an earlier pod, P0. The release of P0, which takes a pod only where the workspace
// both share holds nothing that origin does not, suspended both, and the tester's wake brought the
// issue's pod back as P, where the tester registered (wakesInto). Where P is on a volume of its
// own, it holds nothing from before it, so a member whose agents registered in P0 alone is named
// at no end: the implementer stays released, so the round passes it over; the same after a second
// release, of the pod between P0 and P, with the implementer released throughout; a wake of the
// implementer during the round that the runtime refuses for P before its agent registers; and a
// wake that comes up in P before its agent registers. Where P mounts the issue's kept volume,
// which P0 mounted, the store records each pod of that volume at a role's start in it, as a pod's
// launcher does (launcherStarts), and marks P, so the released implementer is named, and its work
// is on that volume. Then the same two wakes after a takeover: once the round has opened, the
// daemon, as a linger, a park, a close or a hand-over does, or the operator suspends the released
// implementer, which takes it over (supervise's suspendedAgain), and then resumes it as that source
// resumes a claim, the daemon by its rule and the operator by its own resume; the launch is refused
// for P, or comes up in P before its agent registers. These rows run on a pod with a volume of its
// own, so each is named at no end, as each woken twin is. A rule under which a takeover takes the
// pod's earlier work off the role, the loss of the tester's round 36
// (dispatch://INFRA-4425/artifact/tester-round36-at-b7bca9a7-md), fails none of them. The six rows
// of TestATakenOverReleasedRoleIsNamedWhereALaterPodOfItsKeptVolumeWent hold that loss, since there
// P mounts the kept volume of the release, and the role stays taken over to the round's end. Two
// more rows hold the rule to an agent that ran somewhere: a released member whose session is
// forgotten during the round, whose set still holds P0, is named at no end, since P holds nothing
// of P0's; and a member refused for P with no session, which never registered anywhere and which
// the daemon suspends during the round, of an issue with no release, is named at no end. And the
// tester's order (2026-11-22T20:32Z): a member refused for P with no session, whose retry a
// project's pause stops as released, and whose resume by rule after the pause is refused for P
// again, is named at no end, since a pause keeps the pod, and no agent of it ever ran.
//
// Then four orders that INFRA-1484's architect ruled on
// (dispatch://INFRA-4425/comment/566feff4-2b27-41f2-89a9-b1a333d97ecd), each held to every end it
// reaches, and named at none: no agent of the implementer registered in P, and P, on a volume of
// its own, holds nothing from before it. The operator, or the daemon, takes the released
// implementer over, and P goes before any resume, or P is recreated and the resume that follows
// comes up in the pod in P's place or is refused for it. The wake pacing parks the woken
// implementer's launch, which records no process and waits on no pod, and the sweep replaces P by
// issue. The woken implementer's own launch replaces P and comes up in the pod the launch builds in
// its place. And the operator, or the daemon, suspended the implementer before the release, which
// suspends only the roles that run, and either resumes it during the round, its launch refused for
// P, after which it is brought to each end as a woken member is, or holds it suspended until P goes
// or is recreated. A role that failed before the release is no member of the round over P
// (noticeClaims: its machine ignores the round's post), so no end names it as a member that lost
// work (lostClaims), and the table has no row for it; the line of the replacement through a
// process names a failed claim with a session the round did not wait for (notWaitedFor), and no
// read is made for it.
//
// Then the orders of a kept volume, which INFRA-1484's architect also ruled on
// (dispatch://INFRA-4425/comment/65c8b607-a452-4d88-9bcd-624fd7ab8b27). A kept volume outlives its
// pod, and each role's start in a pod of a kept volume records that pod with its volume in the
// store, as the pod's launcher does (launcherStarts), marked where an earlier pod of the issue
// recorded the same volume. P0's release suspends both and keeps the volume, the tester's wake
// brings the issue's pod back as the pod between P0 and P, on the volume P0 mounted, and that pod
// goes and the tester comes back in P on the same volume: the released implementer, whose agents
// registered in P0 alone, is named. With no release at all, the daemon suspends the implementer
// after its agent ran in P0, and P0 goes and the tester comes back in P on the volume P0 mounted:
// the implementer is named. In both, the issue's Sandbox carries a reset mark or a change of its
// disk's shape by the round's end, so the issue's next launch replaces it with its volume and P's
// work. And P as the first pod of a new kept volume, recorded at the tester's start in it: the
// implementer is refused for P with no session and registers in the pod in P's place, the next pod
// of that volume, which records itself before the round's end reads P, where a launch replaces P
// or P is recreated; the implementer is named at no end. In the order with no release, an eviction
// rather than a round of P0's own takes P0, since each row of the table holds one round; the store
// records the volume's next pod alike whatever took the pod before it.
//
// The pairs the table has no row for are of two sorts. These cannot occur, each for the reason
// given:
//
//   - A member that runs in the pod in P's place (registered there, or started fresh there) beside
//     the sweep's replacement of P, through a waiting claim or through a process: the claims would
//     record two pods (groupPod), which holds every replacement while it lasts, or, with no claim
//     left in P, record the pod in P's place, which is the end where P is recreated.
//   - The same member where P goes on its own and the runtime answers it gone: a claim that records
//     the pod in P's place makes the sweep read the claims' other pod before it asks the runtime
//     about P (reconcileIssuePodUpdate), which is the end where P is recreated.
//   - The same member where P is recreated without the identity: the pod in P's place refuses
//     every launch, so no retry comes up anywhere, and a fresh start refused there is the member
//     that lost its session and waits.
//   - A member woken into P, or resumed into P after a takeover, that comes up there before its
//     agent registers, where the sweep replaces P through a waiting claim or through the tester's
//     process, or a launch replaces P: its process in P is the one the sweep replaces P through
//     (podProcess) and keeps P from a launch's replacement (keepAtLaunch), and the round asks it
//     and waits for its answer (roundAsks, awaitsAnswer), so the sweep replaces P only at the
//     round's bound.
//   - A member that the operator holds suspended, where the sweep or a launch replaces P: the
//     round waits for the operator whatever its bound (heldByOperator), so only P's own going ends
//     it.
//
// The others are left out on purpose. A member the daemon suspended before the release and never
// resumed (daemonStoppedHeld) is held where P goes, on its own or recreated: a daemon's suspension
// does not hold the round, so the sweep or a launch can replace P over it, and it is read there by
// the same case of the predicate as the member released once (releasedOnce), whose rows cover
// every end. A member released through a chain of two releases (releasedChain) is held at two
// ends, the read through the tester's process and P going on its own, and is read by that same
// case too. So are a member released on a kept volume, with P the next pod of P0's volume
// (releasedKeptVolume) or with a pod of that volume between P0 and P (releasedKeptLaterPod), and a
// member the daemon suspended after its agent ran in an earlier pod of the kept volume P mounts,
// with no release (daemonStoppedKeptVolume), each held at those two ends: each is named by one case
// of the predicate, since the store marks P at P's own start and every end reads that one mark, and
// only the way its work came to P's disk differs. A released member whose session is forgotten is
// held where P goes, on its own or recreated, the ends that read it by lostClaims; P holds nothing
// of P0's, so neither names it, and the replacements that read only the claims with a session
// (notWaitedFor, notAskedToPush) do not read it at all.
//
// The four kinds of the two named tests have no row here either: a member taken over on the kept
// volume P mounts, which stays suspended (daemonTakeoverKeptHeld, operatorTakeoverKeptHeld), and a
// woken member on that volume that the daemon or the operator suspends before its launch reaches
// any pod (daemonSuspendedWokenKept, operatorSuspendedWokenKept). suspendedTo brings each to its
// end for TestATakenOverReleasedRoleIsNamedWhereALaterPodOfItsKeptVolumeWent and
// TestAWokenReleasedRoleSuspendedBeforeAnyPodIsNamedWhereThePodWent, so that each test's control
// fails that test's six rows and no other row.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// pauseFlag is the project's pause flag as a test drives it (supervise.Deps.Paused): the open
// pause's id, 0 while the project runs.
type pauseFlag struct{ id atomic.Int64 }

// Paused is the flag as a machine reads it.
func (p *pauseFlag) Paused() (int64, bool) {
	id := p.id.Load()
	return id, id != 0
}

// roundPodEnd is how the round over podUpdatePod ends in the order table.
type roundPodEnd int

const (
	// endBySweep is the sweep's own replacement of P through a claim whose launch waits on it
	// (replacePodForRound), the implementer, or the tester where the implementer stays released: no
	// role process runs in P, so no read of the workspace is made.
	endBySweep roundPodEnd = iota
	// endThroughAProcess is the sweep's own replacement of P through the tester's process, once the
	// tester answered, after one read of the workspace through the tester where a member's work is
	// on P's disk, which the runtime is scripted to find work in (readRoundWorkspace).
	endThroughAProcess
	// endPodGone is P going on its own, which the runtime answers gone while the claims still record
	// it, as a process or as the pod a launch waits on (endRoundOverAGonePod).
	endPodGone
	// endRecreated is P recreated in podUpdateReplacementPod with the exchange identity: the next
	// launch of each claim that ran in P comes up there, and the sweep reads the claims recording it
	// (reconcileIssuePodUpdate, endRoundWhosePodWent).
	endRecreated
	// endRecreatedRefusing is P recreated in podUpdateReplacementPod without the exchange identity:
	// the runtime refuses the next launch of each claim that ran in P for that pod, and the sweep
	// reads the claims waiting on it (endRoundWhosePodWent).
	endRecreatedRefusing
	// endAtALaunch is a launch's replacement of P (roundPodEndsAtLaunch), the implementer's own, or
	// the tester's where the implementer stays released, which the sweep ends the round as
	// (endRoundALaunchReplaced).
	endAtALaunch
	// endByIssue is the sweep's own replacement of P by issue (replaceIssuePodForRound), where no
	// claim records a process in a pod or waits on one and the issue's Sandbox still holds P: the
	// tester answered and the daemon suspended it, and the implementer's launch waits in the wake
	// pacing.
	endByIssue
)

func (e roundPodEnd) String() string {
	return [...]string{
		"the sweep replaces the pod through a waiting claim",
		"the sweep replaces the pod through the tester's process after a read that finds work",
		"the pod goes on its own",
		"the pod is recreated with the exchange identity",
		"the pod is recreated without it and refuses the retry again",
		"a launch replaces the pod",
		"the sweep replaces the pod by issue",
	}[e]
}

// memberKind is what the implementer, the member each row is about, did in and around P before
// the round's end.
type memberKind int

const (
	answeredInPod memberKind = iota
	unansweredInPod
	lostSessionInPod
	neverInPod
	registeredElsewhere
	freshElsewhere
	restartRegistered
	restartUnknown
	restartEmpty
	pausedNeverRun
	releasedOnce
	releasedChain
	releasedKeptVolume
	wokenRefused
	wokenIntoPod
	daemonTakeoverRefused
	daemonTakeoverIntoPod
	operatorTakeoverRefused
	operatorTakeoverIntoPod
	releasedLostSession
	neverRunSuspended
	operatorTakeoverPodWent
	daemonTakeoverPodWent
	wokenParked
	wokenReplacesThePod
	operatorStoppedRefused
	daemonStoppedRefused
	operatorStoppedHeld
	daemonStoppedHeld
	releasedKeptLaterPod
	daemonStoppedKeptVolume
	firstOfVolumeElsewhere
	daemonTakeoverKeptHeld
	operatorTakeoverKeptHeld
	daemonSuspendedWokenKept
	operatorSuspendedWokenKept
)

func (k memberKind) String() string {
	return [...]string{
		"an agent of it registered in the pod and it answered",
		"an agent of it registered in the pod and it did not answer",
		"an agent of it registered in the pod, then it lost its session and was refused with none",
		"it was refused for the pod with no session and never registered there",
		"it was refused for the pod with no session, then registered in the pod in its place",
		"an agent of it registered in the pod, then it started fresh in the pod in its place",
		"after a restart over a stored claim whose registered agent runs in the pod, read as unknown",
		"after a restart over a stored claim that shows an abandoned session",
		"after a restart over a stored claim that shows no session",
		"it was refused for the pod with no session, stopped by a pause, and refused again after a resume",
		"an idle release suspended it in an earlier pod, and the pod is not of a kept volume",
		"an idle release suspended it in an earlier pod, and it stayed suspended through a second release",
		"an idle release suspended it in an earlier pod of the kept volume the pod mounts",
		"an idle release suspended it, and its wake was refused for the pod before its agent registered",
		"an idle release suspended it, and its wake came up in the pod before its agent registered",
		"an idle release suspended it, the daemon took it over, and its resume was refused for the pod before its agent registered",
		"an idle release suspended it, the daemon took it over, and its resume came up in the pod before its agent registered",
		"an idle release suspended it, the operator took it over, and its resume was refused for the pod before its agent registered",
		"an idle release suspended it, the operator took it over, and its resume came up in the pod before its agent registered",
		"an idle release suspended it in an earlier pod, and its session was forgotten during the round",
		"it was refused for the pod with no session, never registered, and the daemon suspended it",
		"an idle release suspended it, the operator took it over, and the pod went or was recreated before the operator's resume",
		"an idle release suspended it, the daemon took it over, and the pod went or was recreated before the daemon's resume",
		"an idle release suspended it, and the wake pacing parked its wake's launch before any pod",
		"an idle release suspended it, and its wake's own launch replaced the pod",
		"the operator suspended it before an idle release of its pod, and its resume was refused for the pod",
		"the daemon suspended it before an idle release of its pod, and its resume by rule was refused for the pod",
		"the operator suspended it before an idle release of its pod, and it still holds it",
		"the daemon suspended it before an idle release of its pod, and it was never resumed",
		"an idle release suspended it in an earlier pod of the kept volume the pod mounts, a pod of that volume came between them, and the round's end deletes the volume",
		"the daemon suspended it after its agent ran in an earlier pod of the kept volume the pod mounts, with no release, and the round's end deletes the volume",
		"it was refused for the first pod of a kept volume with no session, then registered in the next pod of that volume",
		"an idle release suspended it in an earlier pod of the kept volume the pod mounts, the daemon took it over, and it stayed suspended",
		"an idle release suspended it in an earlier pod of the kept volume the pod mounts, the operator took it over, and it stayed suspended",
		"an idle release suspended it in an earlier pod of the kept volume the pod mounts, its wake was refused for the pod before its agent registered, and the daemon suspended it before its launch reached any pod",
		"an idle release suspended it in an earlier pod of the kept volume the pod mounts, its wake was refused for the pod before its agent registered, and the operator suspended it before its launch reached any pod",
	}[k]
}

// suspended is whether the daemon or the operator suspended the implementer of kind before the
// round's end, so suspendedTo brings it to its end: an idle release before the round over P
// opened, whether or not the daemon or the operator takes it over while the round runs, and
// resumes it or not, or suspends it once its wake is refused for P; a suspension by the daemon or
// the operator before that release, which then suspends the tester alone; the daemon's own
// suspension in an earlier pod of a kept volume, with no release; or the daemon's own rule while
// the round runs.
func (k memberKind) suspended() bool {
	switch k {
	case releasedOnce, releasedChain, releasedKeptVolume, wokenRefused, wokenIntoPod,
		daemonTakeoverRefused, daemonTakeoverIntoPod, operatorTakeoverRefused, operatorTakeoverIntoPod,
		releasedLostSession, neverRunSuspended, operatorTakeoverPodWent, daemonTakeoverPodWent,
		wokenParked, wokenReplacesThePod, operatorStoppedRefused, daemonStoppedRefused, operatorStoppedHeld, daemonStoppedHeld,
		releasedKeptLaterPod, daemonStoppedKeptVolume, daemonTakeoverKeptHeld, operatorTakeoverKeptHeld,
		daemonSuspendedWokenKept, operatorSuspendedWokenKept:
		return true
	}
	return false
}

// passedOver is whether the implementer of kind stays suspended by a source the round passes over
// until the round's end (supervise's Claim.PassedOverByRound), at an end that replaces P: the
// replacement's line names it under notWaitedFor, beside the claims the round did not wait for,
// where it has a session and is named, and the claim whose launch waits on P at the sweep's
// replacement is the tester. A member woken during the round, or resumed after a takeover, is not
// passed over: it is launching by the round's end.
func (k memberKind) passedOver() bool {
	switch k {
	case releasedOnce, releasedChain, releasedKeptVolume, releasedLostSession, neverRunSuspended,
		releasedKeptLaterPod, daemonStoppedKeptVolume:
		return true
	}
	return false
}

// keptVolume is whether the Sandbox of P, and of every pod of the issue before it in the order of
// kind, owns the issue's kept volume (podUpdateRig.ownsKeptVolume), so each role's start in such a
// pod records it with that volume (launcherStarts): P0 and P on the volume of a release, with or
// without a pod of that volume between them, P0 and P on a volume with no release, and P as the
// first pod of its volume, where P's Sandbox was made with P.
func (k memberKind) keptVolume() bool {
	switch k {
	case releasedKeptVolume, releasedKeptLaterPod, daemonStoppedKeptVolume, firstOfVolumeElsewhere,
		daemonTakeoverKeptHeld, operatorTakeoverKeptHeld, daemonSuspendedWokenKept, operatorSuspendedWokenKept:
		return true
	}
	return false
}

// losesKeptVolume is whether the round's end deletes the kept volume of kind (keptVolume): the
// issue's Sandbox carries a reset mark or a change of its disk's shape by then
// (podUpdateRig.keptVolumeReplaced), so the end says nothing of work left on a volume.
func (k memberKind) losesKeptVolume() bool {
	switch k {
	case releasedKeptLaterPod, daemonStoppedKeptVolume:
		return true
	}
	return false
}

// takenOverBy is the source that suspends the released implementer of kind again once the round
// over P has opened, which takes the claim over (supervise's suspendedAgain): the daemon, as a
// linger, a park, a close or a hand-over does, or the operator. That source then resumes it
// (resumeOf), save where P goes first and is not recreated, and save the kinds that hold it
// suspended to the round's end (daemonTakeoverKeptHeld, operatorTakeoverKeptHeld). It is "" for a
// kind that no source takes over.
func (k memberKind) takenOverBy() supervise.SuspendSource {
	switch k {
	case daemonTakeoverRefused, daemonTakeoverIntoPod, daemonTakeoverPodWent, daemonTakeoverKeptHeld:
		return supervise.SuspendedByDaemon
	case operatorTakeoverRefused, operatorTakeoverIntoPod, operatorTakeoverPodWent, operatorTakeoverKeptHeld:
		return supervise.SuspendedByOperator
	}
	return ""
}

// stoppedBeforeRelease is the source that suspends the implementer of kind in P0 before P0's idle
// release, which suspends only the claims that run (internal/workflow's idle release reads the
// live claims) and so suspends the tester alone: the daemon, as a park does, or the operator. The
// implementer's agent ran in P0, whose workspace the release took only once it held nothing that
// origin does not, so P, on a volume of its own, holds none of the implementer's work. It is "" for
// every other kind.
func (k memberKind) stoppedBeforeRelease() supervise.SuspendSource {
	switch k {
	case daemonStoppedRefused, daemonStoppedHeld:
		return supervise.SuspendedByDaemon
	case operatorStoppedRefused, operatorStoppedHeld:
		return supervise.SuspendedByOperator
	}
	return ""
}

// suspendsWoken is the source that suspends the woken implementer of kind once its wake is refused
// for P, before its launch reaches any pod: the daemon or the operator. The release no longer holds
// the claim by then, so this suspension is no takeover (supervise's suspended, not
// suspendedAgain), and the implementer stays suspended to the round's end. It is "" for every
// other kind.
func (k memberKind) suspendsWoken() supervise.SuspendSource {
	switch k {
	case daemonSuspendedWokenKept:
		return supervise.SuspendedByDaemon
	case operatorSuspendedWokenKept:
		return supervise.SuspendedByOperator
	}
	return ""
}

// woken is the woken member whose order the order of kind is without its takeover, its twin:
// wokenRefused for a kind whose resume is refused for P, wokenIntoPod for one whose resume comes
// up in P, and kind itself for every other kind.
func (k memberKind) woken() memberKind {
	switch k {
	case daemonTakeoverRefused, operatorTakeoverRefused, daemonStoppedRefused, operatorStoppedRefused:
		return wokenRefused
	case daemonTakeoverIntoPod, operatorTakeoverIntoPod:
		return wokenIntoPod
	}
	return k
}

// The update round over P names a member only where it had not answered and its work may be on
// P's disk, at every end. named is the role and the state each row's message and line name, ""
// for none.
func TestPodUpdateNamesWhatTheRoundsPodTookAtEachEndOfEachMember(t *testing.T) {
	const launching, ready, suspended = "implementer (launching)", "implementer (ready)", "implementer (suspended)"
	for _, row := range []struct {
		end   roundPodEnd
		kind  memberKind
		named string
	}{
		{endBySweep, answeredInPod, ""},
		{endBySweep, unansweredInPod, launching},
		{endBySweep, lostSessionInPod, launching},
		{endBySweep, neverInPod, ""},
		{endThroughAProcess, answeredInPod, ""},
		{endThroughAProcess, unansweredInPod, launching},
		{endThroughAProcess, lostSessionInPod, launching},
		{endThroughAProcess, neverInPod, ""},
		{endPodGone, answeredInPod, ""},
		{endPodGone, unansweredInPod, ready},
		{endPodGone, lostSessionInPod, launching},
		{endPodGone, neverInPod, ""},
		{endRecreated, answeredInPod, ""},
		{endRecreated, unansweredInPod, ready},
		{endRecreated, lostSessionInPod, launching},
		{endRecreated, neverInPod, ""},
		{endRecreated, registeredElsewhere, ""},
		{endRecreated, freshElsewhere, launching},
		{endRecreatedRefusing, answeredInPod, ""},
		{endRecreatedRefusing, unansweredInPod, launching},
		{endRecreatedRefusing, lostSessionInPod, launching},
		{endRecreatedRefusing, neverInPod, ""},
		{endAtALaunch, answeredInPod, ""},
		{endAtALaunch, unansweredInPod, launching},
		{endAtALaunch, lostSessionInPod, launching},
		{endAtALaunch, neverInPod, ""},
		{endAtALaunch, registeredElsewhere, ""},
		{endAtALaunch, freshElsewhere, launching},
		{endBySweep, restartRegistered, launching},
		{endBySweep, restartUnknown, launching},
		{endRecreated, restartUnknown, launching},
		{endRecreated, restartEmpty, ""},
		// The members of an issue whose idle release took an earlier pod, P0, which held nothing
		// unpushed: P, on a volume of its own, holds nothing from before it, so a member whose
		// agents registered in P0 alone is named at no end.
		{endBySweep, releasedOnce, ""},
		{endThroughAProcess, releasedOnce, ""},
		{endPodGone, releasedOnce, ""},
		{endRecreated, releasedOnce, ""},
		{endRecreatedRefusing, releasedOnce, ""},
		{endAtALaunch, releasedOnce, ""},
		{endThroughAProcess, releasedChain, ""},
		{endPodGone, releasedChain, ""},
		{endThroughAProcess, releasedKeptVolume, suspended},
		{endPodGone, releasedKeptVolume, suspended},
		{endBySweep, wokenRefused, ""},
		{endThroughAProcess, wokenRefused, ""},
		{endPodGone, wokenRefused, ""},
		{endRecreated, wokenRefused, ""},
		{endRecreatedRefusing, wokenRefused, ""},
		{endAtALaunch, wokenRefused, ""},
		{endPodGone, wokenIntoPod, ""},
		{endRecreated, wokenIntoPod, ""},
		{endRecreatedRefusing, wokenIntoPod, ""},
		{endBySweep, daemonTakeoverRefused, ""},
		{endThroughAProcess, daemonTakeoverRefused, ""},
		{endPodGone, daemonTakeoverRefused, ""},
		{endRecreated, daemonTakeoverRefused, ""},
		{endRecreatedRefusing, daemonTakeoverRefused, ""},
		{endAtALaunch, daemonTakeoverRefused, ""},
		{endPodGone, daemonTakeoverIntoPod, ""},
		{endRecreated, daemonTakeoverIntoPod, ""},
		{endRecreatedRefusing, daemonTakeoverIntoPod, ""},
		{endBySweep, operatorTakeoverRefused, ""},
		{endThroughAProcess, operatorTakeoverRefused, ""},
		{endPodGone, operatorTakeoverRefused, ""},
		{endRecreated, operatorTakeoverRefused, ""},
		{endRecreatedRefusing, operatorTakeoverRefused, ""},
		{endAtALaunch, operatorTakeoverRefused, ""},
		{endPodGone, operatorTakeoverIntoPod, ""},
		{endRecreated, operatorTakeoverIntoPod, ""},
		{endRecreatedRefusing, operatorTakeoverIntoPod, ""},
		// The four orders INFRA-1484's architect ruled on: a takeover before any resume, the parked
		// wake, the woken role's own launch replacing the pod, and a role the release did not
		// suspend.
		{endPodGone, operatorTakeoverPodWent, ""},
		{endRecreated, operatorTakeoverPodWent, ""},
		{endRecreatedRefusing, operatorTakeoverPodWent, ""},
		{endPodGone, daemonTakeoverPodWent, ""},
		{endRecreated, daemonTakeoverPodWent, ""},
		{endRecreatedRefusing, daemonTakeoverPodWent, ""},
		{endByIssue, wokenParked, ""},
		{endAtALaunch, wokenReplacesThePod, ""},
		{endBySweep, operatorStoppedRefused, ""},
		{endThroughAProcess, operatorStoppedRefused, ""},
		{endPodGone, operatorStoppedRefused, ""},
		{endRecreated, operatorStoppedRefused, ""},
		{endRecreatedRefusing, operatorStoppedRefused, ""},
		{endAtALaunch, operatorStoppedRefused, ""},
		{endBySweep, daemonStoppedRefused, ""},
		{endThroughAProcess, daemonStoppedRefused, ""},
		{endPodGone, daemonStoppedRefused, ""},
		{endRecreated, daemonStoppedRefused, ""},
		{endRecreatedRefusing, daemonStoppedRefused, ""},
		{endAtALaunch, daemonStoppedRefused, ""},
		{endPodGone, operatorStoppedHeld, ""},
		{endRecreated, operatorStoppedHeld, ""},
		{endRecreatedRefusing, operatorStoppedHeld, ""},
		{endPodGone, daemonStoppedHeld, ""},
		{endRecreated, daemonStoppedHeld, ""},
		{endRecreatedRefusing, daemonStoppedHeld, ""},
		{endPodGone, releasedLostSession, ""},
		{endRecreated, releasedLostSession, ""},
		{endBySweep, neverRunSuspended, ""},
		{endThroughAProcess, neverRunSuspended, ""},
		{endPodGone, neverRunSuspended, ""},
		{endRecreated, neverRunSuspended, ""},
		{endRecreatedRefusing, neverRunSuspended, ""},
		{endAtALaunch, neverRunSuspended, ""},
		{endBySweep, pausedNeverRun, ""},
		{endThroughAProcess, pausedNeverRun, ""},
		{endPodGone, pausedNeverRun, ""},
		{endRecreated, pausedNeverRun, ""},
		{endRecreatedRefusing, pausedNeverRun, ""},
		{endAtALaunch, pausedNeverRun, ""},
		// The orders of a kept volume: a later pod of the volume of a release, with a pod of that
		// volume between them, a pod of a volume an earlier pod of the issue mounted with no
		// release, each over an end that deletes the volume, and the first pod of a new volume,
		// ended after the next pod of that volume recorded itself.
		{endThroughAProcess, releasedKeptLaterPod, suspended},
		{endPodGone, releasedKeptLaterPod, suspended},
		{endThroughAProcess, daemonStoppedKeptVolume, suspended},
		{endPodGone, daemonStoppedKeptVolume, suspended},
		{endRecreated, firstOfVolumeElsewhere, ""},
		{endAtALaunch, firstOfVolumeElsewhere, ""},
	} {
		t.Run(row.end.String()+": "+row.kind.String(), func(t *testing.T) {
			rig := orderedTo(t, row.end, row.kind)
			wantRoundEnd(t, rig, row.end, row.kind, row.named)
		})
	}
}

// orderedTo brings a round over podUpdatePod, with the implementer of kind as its member, to end,
// and answers the rig once the implementer's machine has taken the end of the round. The
// implementer is queued with no session at the rig's boot, so its machine knows every pod an agent
// of it registers in from then on (supervise.Claim.RegisteredPods); where end is
// endThroughAProcess, the tester runs in P beside it, registered, and answers the round. The
// restart rows restart the daemon (podUpdateRig.restart) and go on with the rig it answers. A
// member the daemon suspended before the round's end is brought to its end by suspendedTo, and a
// member refused for the first pod of a kept volume by firstOfVolumeTo. The rig of the member a
// pause stopped reads a pause flag of its own (podUpdateRigOptions.paused).
func orderedTo(t *testing.T, end roundPodEnd, kind memberKind) *podUpdateRig {
	t.Helper()
	switch {
	case kind.suspended():
		return suspendedTo(t, end, kind)
	case kind == firstOfVolumeElsewhere:
		return firstOfVolumeTo(t, end)
	}
	implementer, tester := claim.RoleImplementer, claim.RoleTester
	roles := []podUpdateRole{{role: implementer, state: supervise.StateQueued}}
	if end == endThroughAProcess {
		roles = append(roles, readyWorker(tester))
	}
	var opts podUpdateRigOptions
	pause := &pauseFlag{}
	if kind == pausedNeverRun {
		opts.paused = pause.Paused
	}
	rig := newPodUpdateRigWith(t, opts, roles...)
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1})
	rig.drifts(podUpdateExchangeDrift)
	refusals := rig.refuseLaunches()

	switch kind {
	case lostSessionInPod:
		rig.startInPod(implementer, podUpdatePod)
		rig.loseSessionRefused(refusals, implementer, podUpdatePod)
	case neverInPod, registeredElsewhere, restartEmpty:
		refusals.refuse(implementer, podUpdatePod)
		rig.refusedFirstLaunch(implementer)
	case pausedNeverRun:
		// The tester's order (dispatch://INFRA-4425/artifact/tester-pausestop-test-md): refused for
		// P with no session, the implementer's retry meets a project's pause, whose gate stops it as
		// released (supervise's pauseGate, pauseStopDue). The pause ends, and a resume by rule
		// launches it again, which is refused for P again. A pause keeps the same pod, and no
		// agent of the implementer ever ran, so its set stays empty.
		refusals.refuse(implementer, podUpdatePod)
		rig.refusedFirstLaunch(implementer)
		pause.id.Store(7)
		rig.retryLaunch()
		testwait.Eventually(t, "the pause's gate to stop the implementer as released", func() bool {
			rig.clock.fire(0)
			return rig.claimOf(implementer).Released()
		})
		pause.id.Store(0)
		if err := rig.machineOf(implementer).Handle(rig.ctx, supervise.RequestRuleResume{Claim: rig.tokens[implementer], Reason: "the pause ended"}); !errors.Is(err, runtime.ErrAwaitingPodRound) {
			t.Fatalf("the implementer's resume after the pause answered %v, want its launch refused for the pod's round", err)
		}
		if c := rig.claimOf(implementer); c.Session != "" || c.Locator != nil || podOf(c.AwaitsPod) != podUpdatePod {
			t.Fatalf("the resumed implementer has session %q and process %+v, waiting on %+v; want no session, no process and the wait on pod %s",
				c.Session, c.Locator, c.AwaitsPod, podUpdatePod)
		}
	case restartUnknown:
		// An agent of it registers in P, an operator suspends it and forgets its session, and the
		// daemon restarts before any round: the stored claim shows the abandoned session and no
		// registered agent (supervise.Claim.Abandoned), so the restarted machine's set is
		// unknown. The operator's resume, with no session, is refused for P.
		rig.startInPod(implementer, podUpdatePod)
		rig.suspendByOperator(implementer)
		rig.handle(implementer, supervise.RequestForgetSession{Claim: rig.tokens[implementer], Session: "ses_" + string(implementer)})
		rig, refusals = rig.restarted()
		refusals.refuse(implementer, podUpdatePod)
		rig.resumedRefused(implementer)
	default:
		rig.startInPod(implementer, podUpdatePod)
	}

	rig.tick()
	rig.awaitRound(implementer, podUpdateRound)
	if end == endThroughAProcess {
		rig.awaitAsked(tester, podUpdateRound)
	}
	if kind == answeredInPod {
		rig.awaitAsked(implementer, podUpdateRound)
		rig.signal(implementer, "c0ffee")
	}

	switch kind {
	case restartRegistered:
		// The daemon restarts while the implementer's registered agent runs in P, so the stored
		// claim shows the session that agent registered, and the restarted machine reads the set
		// as unknown, with no pod in it (supervise's bootRegisteredPods). After the restart its
		// process ends, its relaunch finds no session, and its fresh start is refused for P with
		// none.
		rig, refusals = rig.restarted()
		rig.loseSessionRefused(refusals, implementer, podUpdatePod)
	case restartEmpty:
		// The operator suspends the waiting implementer, which ends its wait, before the daemon
		// restarts: the stored claim shows no session, so the restarted machine's set is known
		// and empty. The operator's resume is refused for P.
		if err := rig.machineOf(implementer).Handle(rig.ctx, supervise.RequestSuspend{Claim: rig.tokens[implementer], By: supervise.SuspendedByOperator}); err != nil &&
			!errors.Is(err, supervise.ErrSuspendHeld) {
			t.Fatalf("suspend the waiting implementer: %v", err)
		}
		rig.retryLaunch()
		rig.awaitClaim(implementer, "the held suspension to suspend it", func(c supervise.Claim) bool {
			return c.State == supervise.StateSuspended && c.AwaitsPod == nil
		})
		rig, refusals = rig.restarted()
		refusals.refuse(implementer, podUpdatePod)
		rig.resumedRefused(implementer)
	}

	waitsWithItsSession := kind == answeredInPod || kind == unansweredInPod
	switch end {
	case endBySweep, endThroughAProcess:
		if waitsWithItsSession {
			rig.goneRefused(refusals, implementer, podUpdatePod, false)
		}
		if end == endThroughAProcess {
			rig.signal(tester, "c0ffee")
		}
		rig.awaitReplacement(1, "by the sweep")
		rig.awaitClaim(implementer, "the round's end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
		rig.settleAll()
		return rig
	case endPodGone:
		rig.podGone()
	case endRecreated:
		switch kind {
		case answeredInPod, unansweredInPod:
			rig.displacedInto(implementer, podUpdateReplacementPod)
		case freshElsewhere:
			rig.startsFreshInto(implementer, podUpdateReplacementPod, true)
		default:
			pod := rig.retriesInto(refusals, implementer, podUpdateReplacementPod)
			if kind == registeredElsewhere || kind == restartEmpty {
				rig.registerInto(implementer, pod)
			}
		}
		rig.rt.ScriptPodDrift(rig.tokens[implementer])
	case endRecreatedRefusing:
		if waitsWithItsSession {
			rig.goneRefused(refusals, implementer, podUpdateReplacementPod, true)
		} else {
			refusals.refuse(implementer, podUpdateReplacementPod)
			rig.retryLaunch()
			rig.awaitClaim(implementer, "its retry refused for the pod in P's place", func(c supervise.Claim) bool {
				return podOf(c.AwaitsPod) == podUpdateReplacementPod
			})
		}
		rig.rt.ScriptPodDrift(rig.tokens[implementer])
	case endAtALaunch:
		if waitsWithItsSession {
			rig.goneRefused(refusals, implementer, podUpdatePod, false)
		}
		var pod string
		if kind == freshElsewhere {
			pod = rig.startsFreshInto(implementer, podUpdatePod, false)
		} else {
			pod = rig.retriesInto(refusals, implementer, podUpdatePod)
		}
		rig.wantReplacedAtLaunch(implementer, pod)
		if kind == registeredElsewhere {
			rig.registerInto(implementer, pod)
		}
	}
	rig.awaitSwept(implementer, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.settleAll()
	return rig
}

// firstOfVolumeTo is orderedTo for an implementer refused for P, the first pod of a kept volume,
// with no session (firstOfVolumeElsewhere): P's Sandbox is made with P, the tester's start records
// P, which no earlier pod of the volume precedes, and the implementer is refused for P as
// neverInPod is. The round over P opens and asks the tester, which answers. Then P goes and comes
// back as the next pod of its volume (endRecreated), the tester relaunched into it, or the
// workflow suspends the tester and the implementer's own launch replaces P (endAtALaunch); either
// way the implementer's retry starts in that next pod, whose start records it before the round's
// end, and an agent of it registers there.
func firstOfVolumeTo(t *testing.T, end roundPodEnd) *podUpdateRig {
	t.Helper()
	implementer, tester := claim.RoleImplementer, claim.RoleTester
	rig := newPodUpdateRigWith(t, podUpdateRigOptions{}, podUpdateRole{role: implementer, state: supervise.StateQueued},
		podUpdateRole{role: tester, state: supervise.StateQueued})
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1})
	rig.drifts(podUpdateExchangeDrift)
	refusals := rig.refuseLaunches()
	rig.ownsKeptVolume(roundPodMadeAt)
	rig.startInPod(tester, podUpdatePod)
	refusals.refuse(implementer, podUpdatePod)
	rig.refusedFirstLaunch(implementer)

	rig.tick()
	rig.awaitRound(implementer, podUpdateRound)
	rig.awaitAsked(tester, podUpdateRound)
	rig.signal(tester, "c0ffee")
	switch end {
	case endRecreated:
		rig.displacedInto(tester, podUpdateReplacementPod)
		rig.rt.ScriptPodDrift(rig.tokens[tester])
		pod := rig.retriesInto(refusals, implementer, podUpdateReplacementPod)
		rig.registerInto(implementer, pod)
		rig.rt.ScriptPodDrift(rig.tokens[implementer])
	case endAtALaunch:
		rig.suspendBy(tester, supervise.SuspendedByDaemon)
		pod := rig.retriesInto(refusals, implementer, podUpdatePod)
		rig.wantReplacedAtLaunch(implementer, pod)
		rig.registerInto(implementer, pod)
	default:
		t.Fatalf("end %s is not one this order brings about", end)
	}
	rig.awaitSwept(implementer, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.settleAll()
	return rig
}

// wantReplacedAtLaunch wants the round's pod replaced once at a launch, by role's (the runtime's
// ReplaceRoundPodAtLaunch, roundPodEndsAtLaunch), and role's launch to have come up in pod, the
// pod that launch built in P's place, not in P.
func (r *podUpdateRig) wantReplacedAtLaunch(role claim.Role, pod string) {
	r.t.Helper()
	if pod == podUpdatePod {
		r.t.Fatalf("the %s's launch came up in the round's pod %s, want the pod the launch built in its place", role, pod)
	}
	if atLaunch := r.rt.CallsOf("ReplaceRoundPodAtLaunch"); len(atLaunch) != 1 || atLaunch[0].Spec.Claim != r.tokens[role] {
		r.t.Fatalf("the round's pod was replaced at a launch %+v, want once, by the %s's", atLaunch, role)
	}
}

// wantRoundEnd holds the end of the round over podUpdatePod that orderedTo brought about to the
// issue's message and the end's log line, which name the implementer, as named renders it, or
// nobody where named is "". kind is the implementer's: whether it answered the round is what the
// line of a pod that went says where it names nobody; one the round passes over is named under
// notWaitedFor where a replacement's line names claims the round did not wait for, and at the
// sweep's replacement through a waiting claim that claim is the tester; and on a kept volume the
// line and the message say the work is still on that volume (roundKeptVolume).
func wantRoundEnd(t *testing.T, rig *podUpdateRig, end roundPodEnd, kind memberKind, named string) {
	t.Helper()
	implementer := claim.RoleImplementer
	answered, kept := kind == answeredInPod, kind.keptVolume() && !kind.losesKeptVolume()
	var namedRoles []claim.Role
	if named != "" {
		namedRoles = []claim.Role{implementer}
	}
	messages := rig.messages()
	replaced := loggedLines(t, rig.logs.String(), podUpdateReplacedLine)
	switch end {
	case endBySweep, endThroughAProcess:
		through := implementer
		if end == endThroughAProcess || kind.passedOver() {
			through = claim.RoleTester
		}
		if calls := rig.replacements(); len(calls) != 1 || calls[0].Pod.Claim != rig.tokens[through] || podOf(calls[0].Pod.Locator) != podUpdatePod {
			t.Fatalf("ReplacePod was asked %+v, want once, through the %s in pod %s", calls, through, podUpdatePod)
		}
		if len(replaced) != 1 || replaced[0]["atTheBound"] != false {
			t.Fatalf("the replacement was logged %+v, want one line that no bound made", replaced)
		}
		line := replaced[0]
		if kind.passedOver() {
			wantField(t, line, "notWaitedFor", namedRoles...)
			wantField(t, line, "waitingOnThePodNotWaitedFor")
		} else {
			wantField(t, line, "notWaitedFor")
			wantField(t, line, "waitingOnThePodNotWaitedFor", namedRoles...)
		}
		if value, logged := line["keptVolume"]; (value == true) != kept || logged != kept {
			t.Errorf("the replacement's line says keptVolume=%v, want it there (true) only where the pod's disk is a kept volume (%t)", value, kept)
		}
		switch {
		case end == endBySweep:
			if line["workspaceRead"] != "not made" || line["workspaceNotReadBecause"] != noProcessRead {
				t.Errorf("the replacement's line names the read %+v, want one not made because %q", line, noProcessRead)
			}
			if named != "" {
				wantToldUnanswered(t, messages, named)
				return
			}
		case named != "":
			wantReadThroughTheTester(t, rig, line, "found work")
			if len(messages) != 1 || !strings.Contains(messages[0], toldNotWaitedForRoles+named+".") || !strings.Contains(messages[0], passedOverCommit) {
				t.Fatalf("the issue was written %q, want one message that names %s under %q, and the commit %s", messages, named, toldNotWaitedForRoles, passedOverCommit)
			}
			wantKeptSaid(t, messages[0], kept, "The pod is gone. Its disk is a kept volume, and the volume is still there.")
			return
		default:
			if reads := rig.rt.CallsOf("WorkspaceWork"); len(reads) != 0 || line["workspaceRead"] != "not made" ||
				line["workspaceNotReadBecause"] != nothingUnaskedRead {
				t.Errorf("the workspace was read %+v and the line names the read %+v, want no read, not made because %q", reads, line, nothingUnaskedRead)
			}
		}
	case endAtALaunch, endByIssue:
		by := "atLaunch"
		if end == endByIssue {
			by = "byIssue"
			if calls := rig.issueReplacements(); len(calls) != 1 || calls[0].PodUID != podUpdatePod {
				t.Fatalf("the pod was replaced by issue %+v, want once, pod %s", calls, podUpdatePod)
			}
		}
		if calls := rig.replacements(); len(calls) != 0 {
			t.Errorf("ReplacePod called %+v, want the %s replacement alone", calls, by)
		}
		if len(replaced) != 1 || replaced[0][by] != true {
			t.Fatalf("the replacement was logged %+v, want one line of a replacement with %s", replaced, by)
		}
		wantField(t, replaced[0], "unansweredMembers", namedRoles...)
		if named != "" {
			wantToldUnanswered(t, messages, named)
			return
		}
	default:
		if calls := rig.replacements(); len(calls) != 0 || len(replaced) != 0 {
			t.Errorf("ReplacePod called %+v and logged %+v, want no replacement: the round's pod went on its own", calls, replaced)
		}
		over := podWentOverLine
		if end == endPodGone {
			over = podNotThereOverLine
		}
		went := podWentLostLine
		if kept {
			went = podWentKeptOnVolumeLine
		}
		logs := rig.logs.String()
		lost, noWorker, overs := loggedLines(t, logs, went), loggedLines(t, logs, podWentNoWorkerLine), loggedLines(t, logs, over)
		switch {
		case named != "":
			if len(lost) != 1 || len(noWorker)+len(overs) != 0 {
				t.Fatalf("the round's end was logged %+v, %+v and %+v, want one line that a member lost what it had not pushed", lost, noWorker, overs)
			}
			wantField(t, lost[0], "lost", implementer)
			if len(messages) != 1 || !strings.Contains(messages[0], "The pod that ran this issue's roles is gone") ||
				!strings.Contains(messages[0], "before the pod went: "+named+".") {
				t.Fatalf("the issue was written %q, want one message that its pod went and that %s lost what it had not pushed", messages, named)
			}
			wantKeptSaid(t, messages[0], kept, "The pod's disk is a kept volume, and the volume is still there.")
			return
		case answered:
			if len(overs) != 1 || len(lost)+len(noWorker) != 0 {
				t.Fatalf("the round's end was logged %+v, %+v and %+v, want one line that every member answered", overs, lost, noWorker)
			}
		default:
			if len(noWorker) != 1 || len(lost)+len(overs) != 0 {
				t.Fatalf("the round's end was logged %+v, %+v and %+v, want one line that no member that had not answered ran a worker in the pod",
					noWorker, lost, overs)
			}
			wantField(t, noWorker[0], "ranNoWorkerInPod", implementer)
		}
	}
	if len(messages) != 0 {
		t.Errorf("the issue was written %q, want nothing: no member that had not answered ran a worker in the round's pod", messages)
	}
}

// podWentKeptOnVolumeLine is the line of the end of a round whose pod went with a member that had
// not answered, where the pod's disk is a kept volume that is still there (podWentWithoutItsRound,
// roundKeptVolume): what that member had not pushed is still on the volume.
const podWentKeptOnVolumeLine = "supervise: the pod of an open update round is gone; its disk is a kept volume, so what the roles that had not answered did not push is still on it"

// wantKeptSaid holds message, written on the issue at a round's end, to saying says, its sentence
// that the pod's disk is a kept volume that is still there, where kept is true, and to not saying
// it otherwise.
func wantKeptSaid(t *testing.T, message string, kept bool, says string) {
	t.Helper()
	if strings.Contains(message, says) != kept {
		t.Errorf("the message says %q: %t, want %t, whether the pod's disk is a kept volume that is still there:\n%s",
			says, !kept, kept, message)
	}
}

// launchRefusals is, by claim, the pod the runtime refuses each launch of that claim for, for want
// of the pod's update round (awaitingRound), and "" for a claim whose launches go through
// (refuseLaunches). A test writes a claim's pod before the launch it is for, and the launches read
// it as they run.
type launchRefusals struct {
	mu     sync.Mutex
	tokens map[claim.Role]claim.Token
	pods   map[claim.Token]string
	// resumes is each claim one Resume of which goes through to its scripted answer whatever pod
	// it is refused for, as a relaunch that finds its session in no store is answered before any
	// pod is asked (loseSessionRefused).
	resumes map[claim.Token]bool
}

// refuse makes each later launch of role's claim refused for pod, or, for "", lets it through.
func (f *launchRefusals) refuse(role claim.Role, pod string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods[f.tokens[role]] = pod
}

// refusing is the pod the launch of token that method makes now is refused for, "" for none.
func (f *launchRefusals) refusing(token claim.Token, method string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if method == "Resume" && f.resumes[token] {
		delete(f.resumes, token)
		return ""
	}
	return f.pods[token]
}

// refuseLaunches makes every Spawn and Resume of a claim of the rig the runtime's refusal of a
// launch into the pod launchRefusals names for that claim (awaitingRound), at the launch's own
// generation, and lets it through where it names none. It answers the refusals, which name none.
func (r *podUpdateRig) refuseLaunches() *launchRefusals {
	refusals := &launchRefusals{tokens: r.tokens, pods: map[claim.Token]string{}, resumes: map[claim.Token]bool{}}
	hook := func(method string) func(context.Context) error {
		return func(context.Context) error {
			calls := r.rt.CallsOf(method)
			spec := calls[len(calls)-1].Spec
			if pod := refusals.refusing(spec.Claim, method); pod != "" {
				return awaitingRound(spec.Claim, pod, spec.Generation)
			}
			return nil
		}
	}
	r.rt.OnSpawn, r.rt.OnResume = hook("Spawn"), hook("Resume")
	return refusals
}

// restarted is the daemon restarting (restart), with the sweep of the rig it answers writing its
// lines where this rig's do, and that rig's runtime refusing launches as refuseLaunches makes it,
// over the drift of the pod the round is over.
func (r *podUpdateRig) restarted() (*podUpdateRig, *launchRefusals) {
	r.t.Helper()
	next := r.restart()
	next.s.log = r.log
	next.drifts(podUpdateExchangeDrift)
	return next, next.refuseLaunches()
}

// machineOf is role's claim's machine.
func (r *podUpdateRig) machineOf(role claim.Role) *supervise.Machine {
	r.t.Helper()
	m, ok := r.sup.Machine(r.tokens[role])
	if !ok {
		r.t.Fatalf("the %s is not supervised", role)
	}
	return m
}

// goneRefused is role's process ending, with its pod where displaced is set, and the relaunch that
// end raises refused for pod: the claim keeps its session, records no process, and waits on pod.
func (r *podUpdateRig) goneRefused(refusals *launchRefusals, role claim.Role, pod string, displaced bool) {
	r.t.Helper()
	ended := r.claimOf(role).Locator
	if ended == nil {
		r.t.Fatalf("the %s records no process to end", role)
	}
	refusals.refuse(role, pod)
	err := r.machineOf(role).Handle(r.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *ended, Kind: runtime.Gone, At: r.clock.Now(), Displaced: displaced,
	}})
	if !errors.Is(err, runtime.ErrAwaitingPodRound) {
		r.t.Fatalf("the %s's relaunch answered %v, want the runtime's wait for its pod's round", role, err)
	}
	if c := r.claimOf(role); c.Session == "" || c.Locator != nil || podOf(c.AwaitsPod) != pod {
		r.t.Fatalf("the refused %s has session %q and process %+v, waiting on %+v; want its session kept, no process, and the wait on pod %s",
			role, c.Session, c.Locator, c.AwaitsPod, pod)
	}
}

// loseSessionRefused is role's process ending while its pod stays, and its relaunch finding the
// session in no store, so the launch forgets the session and spawns a fresh agent in the same try
// (supervise's startFresh), which the runtime refuses for pod: the claim waits on pod with no
// session, its abandoned session recorded (supervise.Claim.Abandoned).
func (r *podUpdateRig) loseSessionRefused(refusals *launchRefusals, role claim.Role, pod string) {
	r.t.Helper()
	token := r.tokens[role]
	ended := r.claimOf(role).Locator
	if ended == nil {
		r.t.Fatalf("the %s records no process to end", role)
	}
	refusals.refuse(role, pod)
	refusals.mu.Lock()
	refusals.resumes[token] = true
	refusals.mu.Unlock()
	r.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: the session table holds no such session", token, runtime.ErrSessionFileMissing)})
	err := r.machineOf(role).Handle(r.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *ended, Kind: runtime.Gone, At: r.clock.Now(),
	}})
	if !errors.Is(err, runtime.ErrAwaitingPodRound) {
		r.t.Fatalf("the %s's relaunch answered %v, want its fresh spawn refused for the pod's round", role, err)
	}
	if c := r.claimOf(role); c.Session != "" || c.Abandoned.Session == "" || c.Locator != nil || podOf(c.AwaitsPod) != pod {
		r.t.Fatalf("the %s has session %q, abandoned %q, process %+v, waiting on %+v; want its session forgotten, no process and the wait on pod %s",
			role, c.Session, c.Abandoned.Session, c.Locator, c.AwaitsPod, pod)
	}
}

// startsFreshInto is role's process ending, with its pod where displaced is set, and its relaunch
// finding the session in no store, so the launch forgets the session and spawns a fresh agent
// into pod, which comes up there, or in the pod a launch into pod builds in its place (fake's
// launchInto). No agent of it registers there. It answers the pod the fresh agent came up in.
func (r *podUpdateRig) startsFreshInto(role claim.Role, pod string, displaced bool) string {
	r.t.Helper()
	token := r.tokens[role]
	ended := r.claimOf(role).Locator
	if ended == nil {
		r.t.Fatalf("the %s records no process to end", role)
	}
	conn := fake.NewConn()
	r.conns.Register(token, conn)
	r.workers[role] = conn
	r.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: the session table holds no such session", token, runtime.ErrSessionFileMissing)})
	r.rt.ScriptSpawn(fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, pod)})
	r.handle(role, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *ended, Kind: runtime.Gone, At: r.clock.Now(), Displaced: displaced,
	}})
	c := r.claimOf(role)
	if c.Session != "" || c.Locator == nil || podOf(c.Locator) == podUpdatePod {
		r.t.Fatalf("the %s started fresh has session %q and process %+v, want no session and a process in a pod other than %s",
			role, c.Session, c.Locator, podUpdatePod)
	}
	r.launcherStarts(podOf(c.Locator))
	return podOf(c.Locator)
}

// retriesInto lets role's next retry of its refused launch through to a launch into pod, a Resume
// where the claim has a session and a Spawn where it has none, which comes up there, or in the pod
// a launch into pod builds in its place (fake's launchInto). It answers the pod the launch came up
// in.
func (r *podUpdateRig) retriesInto(refusals *launchRefusals, role claim.Role, pod string) string {
	r.t.Helper()
	token := r.tokens[role]
	conn := fake.NewConn()
	r.conns.Register(token, conn)
	r.workers[role] = conn
	launch := fake.SpawnResult{Locator: *podUpdateLocator(token, podUpdateIssue, pod)}
	if r.claimOf(role).Session != "" {
		r.rt.ScriptResume(launch)
	} else {
		r.rt.ScriptSpawn(launch)
	}
	refusals.refuse(role, "")
	r.retryLaunch()
	c := r.awaitClaim(role, "its retry to record a process", func(c supervise.Claim) bool { return c.Locator != nil })
	r.launcherStarts(podOf(c.Locator))
	return podOf(c.Locator)
}

// resumedRefused is an operator's resume of role's suspended claim, whose launch the runtime
// refuses for the pod refuseLaunches names for it: the claim waits on that pod with no process.
func (r *podUpdateRig) resumedRefused(role claim.Role) {
	r.t.Helper()
	if err := r.machineOf(role).Handle(r.ctx, supervise.RequestResume{Claim: r.tokens[role]}); !errors.Is(err, runtime.ErrAwaitingPodRound) {
		r.t.Fatalf("the operator's resume of the %s answered %v, want its launch refused for the pod's round", role, err)
	}
	if c := r.claimOf(role); c.Locator != nil || c.AwaitsPod == nil {
		r.t.Fatalf("the resumed %s records process %+v, waiting on %+v; want no process and a wait on a pod", role, c.Locator, c.AwaitsPod)
	}
}

// The pods of the members an idle release suspended (suspendedTo): releasedPod is P0, the issue's
// pod before P, in which an agent of the implementer and of the tester registered and whose idle
// release found nothing unpushed in the workspace they share; chainedPod is the pod between P0 and
// P, where a second release came before the round (releasedChain), or, on a kept volume, the pod of
// that volume between P0 and P (releasedKeptLaterPod).
const (
	releasedPod = "2b6d9e14-7c30-4a85-b1f6-0e9a3c5d7f28"
	chainedPod  = "6e1f3a8c-9d24-4b07-a5c3-8f2e0b4d6a19"
)

// The clock before the round over P opens at podUpdateStart: the Sandbox whose kept volume P0 and
// P mount is made (each kind of memberKind.keptVolume but firstOfVolumeElsewhere), and P is made
// (firstOfVolumeElsewhere).
var (
	keptVolumeMadeAt = podUpdateStart.Add(-2 * time.Hour)
	roundPodMadeAt   = podUpdateStart.Add(-10 * time.Minute)
)

// parkedWakeLetOut is when the wake pacing lets out the woken implementer's launch it parks
// (wokenParked, parkingPace): ten minutes into the round, before its bound.
var parkedWakeLetOut = podUpdateStart.Add(10 * time.Minute)

// parkingPace is a wake pacing that lets every paced launch out at once until park is set, and
// from then on lets each out at parkedWakeLetOut (supervise.WakePace): the tester's wakes before
// the round run as each is raised, and the woken implementer's launch waits in the pacing with no
// process and no pod, as a wave of wakes leaves it.
type parkingPace struct{ park atomic.Bool }

// pace is the pacing a rig's machines read (podUpdateRigOptions.wakePace).
func (p *parkingPace) pace() supervise.WakePace {
	return supervise.WakePace{Rate: 1, Burst: 1, Reserve: func(now time.Time) time.Time {
		if p.park.Load() {
			return parkedWakeLetOut
		}
		return now
	}}
}

// suspendedTo is orderedTo for a member the daemon or the operator suspended before the round's
// end (memberKind.suspended). The implementer and the tester are queued with no session at the
// rig's boot, so each machine knows every pod an agent of it registers in from then on.
//
// Where kind is neverRunSuspended, the tester registers in P, and the implementer's first launch,
// with no session, is refused for P, so it waits on P and no agent of it registers anywhere; once
// the round over P has opened, the daemon suspends the implementer by its own rule. Where kind is
// daemonStoppedKeptVolume, nothing is released: both start in P0, a pod of the issue's kept
// volume, the daemon suspends the implementer, and P0 goes and the tester comes back in P on the
// same volume (displacedInto). Every other kind is released first. Both start in P0, where an
// agent of each registers, and P0's idle release, which found nothing unpushed in the workspace
// they share, suspends both (releases), or, where the daemon or the operator suspended the
// implementer before it (memberKind.stoppedBeforeRelease), the tester alone. Where kind is
// releasedChain, the tester's wake brings back the pod between P0 and P, and that pod's release
// suspends the tester; the implementer stays released through both. Then the tester's wake brings
// the issue's pod back as P, on a volume of its own, or on the kept volume P0 mounted
// (releasedKeptVolume, daemonTakeoverKeptHeld, operatorTakeoverKeptHeld, daemonSuspendedWokenKept,
// operatorSuspendedWokenKept), and the tester registers in P (wakesInto). Where kind is
// releasedKeptLaterPod, that wake brings the pod back as the pod between P0 and P on the kept
// volume, and that pod goes and the tester comes back in P on the same volume. Each start in a pod
// of a kept volume records the pod with its volume (launcherStarts); where the round's end is to
// delete the volume (releasedKeptLaterPod, daemonStoppedKeptVolume), the Sandbox is marked so
// before the round opens (keptVolumeReplaced). The round over P opens and asks the tester. Then the
// implementer's session is forgotten (releasedLostSession), or a woken implementer is woken, before
// the tester answers: its launch is refused for P (wokenRefused) or comes up in P (wokenIntoPod),
// and no agent of it registers, or the wake pacing parks it (wokenParked). A member taken over is
// suspended again first, by the daemon or by the operator, which takes the released claim over
// (supervise's suspendedAgain), and is then resumed as that source resumes a claim (resumeOf), its
// launch refused for P or coming up in P as its woken twin's is (memberKind.woken); a member the
// daemon or the operator suspended before the release is resumed so too, its launch refused for P.
// A member taken over whose pod goes first is resumed only once P is recreated, its launch coming
// up in the pod in P's place or refused for it, and stays suspended where P goes on its own. A
// member taken over on a kept volume (daemonTakeoverKeptHeld, operatorTakeoverKeptHeld) is never
// resumed. A woken member whose wake is refused for P on a kept volume is then suspended by the
// daemon or the operator (memberKind.suspendsWoken) before its launch reaches any pod. Both stay
// suspended until P goes or is recreated. The tester answers, and the round goes to end as the
// tester, the one claim whose agent ran in P, and a woken or resumed implementer bring it about; a
// woken implementer whose own launch replaces P is woken only then (wokenReplacesThePod).
func suspendedTo(t *testing.T, end roundPodEnd, kind memberKind) *podUpdateRig {
	t.Helper()
	implementer, tester := claim.RoleImplementer, claim.RoleTester
	var opts podUpdateRigOptions
	pacing := &parkingPace{}
	if kind == wokenParked {
		opts.wakePace = pacing.pace()
	}
	rig := newPodUpdateRigWith(t, opts, podUpdateRole{role: implementer, state: supervise.StateQueued}, podUpdateRole{role: tester, state: supervise.StateQueued})
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, runtime.WorkspaceWork{Head: passedOverHead, Commits: []string{passedOverCommit}, Count: 1})
	refusals := rig.refuseLaunches()

	// stoodAtTheRound is the source the implementer stands suspended by when the round over P
	// opens: the release's, or the one that suspended it before the release, or the daemon's
	// where nothing was released.
	stoodAtTheRound := supervise.SuspendedByReleased
	if kind.keptVolume() {
		rig.ownsKeptVolume(keptVolumeMadeAt)
	}
	switch kind {
	case neverRunSuspended:
		rig.startInPod(tester, podUpdatePod)
		refusals.refuse(implementer, podUpdatePod)
		rig.refusedFirstLaunch(implementer)
	case daemonStoppedKeptVolume:
		rig.startInPod(implementer, releasedPod)
		rig.startInPod(tester, releasedPod)
		rig.suspendBy(implementer, supervise.SuspendedByDaemon)
		stoodAtTheRound = supervise.SuspendedByDaemon
		rig.displacedInto(tester, podUpdatePod)
	default:
		rig.startInPod(implementer, releasedPod)
		rig.startInPod(tester, releasedPod)
		released := []claim.Role{implementer, tester}
		if by := kind.stoppedBeforeRelease(); by != "" {
			rig.suspendBy(implementer, by)
			stoodAtTheRound, released = by, []claim.Role{tester}
		}
		rig.releases(releasedPod, released...)
		switch kind {
		case releasedChain:
			rig.wakesInto(tester, chainedPod)
			rig.releases(chainedPod, tester)
			rig.wakesInto(tester, podUpdatePod)
		case releasedKeptLaterPod:
			rig.wakesInto(tester, chainedPod)
			rig.displacedInto(tester, podUpdatePod)
		default:
			rig.wakesInto(tester, podUpdatePod)
		}
	}
	if kind.losesKeptVolume() {
		rig.keptVolumeReplaced()
	}
	if end == endByIssue {
		// The issue's Sandbox holds P, which the sweep asks of a round whose claims record no pod
		// (podlessRound).
		rig.rt.ScriptIssuePod(podUpdateIssue, podUpdatePod)
	}
	rig.drifts(podUpdateExchangeDrift)

	rig.tick()
	rig.awaitRound(implementer, podUpdateRound)
	rig.awaitAsked(tester, podUpdateRound)
	if c := rig.claimOf(implementer); kind != neverRunSuspended &&
		(c.State != supervise.StateSuspended || c.SuspendedBy != stoodAtTheRound || c.UpdateTaskedAt != nil) {
		t.Fatalf("the implementer is %s by %q, asked at %v; want it suspended by %q and never asked: the round asks no suspended claim",
			c.State, c.SuspendedBy, c.UpdateTaskedAt, stoodAtTheRound)
	}
	if by := kind.takenOverBy(); by != "" {
		rig.suspendBy(implementer, by)
		if c := rig.claimOf(implementer); c.State != supervise.StateSuspended || c.SuspendedBy != by || c.Session == "" ||
			len(c.RegisteredPods.Pods) != 1 || c.RegisteredPods.Pods[0] != releasedPod {
			t.Fatalf("the implementer is %s by %q with session %q and set %+v; want it taken over by the %s, with its session and the set of releasedPod alone",
				c.State, c.SuspendedBy, c.Session, c.RegisteredPods, by)
		}
	}
	switch kind.woken() {
	case neverRunSuspended:
		// The daemon suspends the waiting implementer by its own rule; the suspension waits for the
		// launch's turn, which the retry gives it.
		if err := rig.machineOf(implementer).Handle(rig.ctx, supervise.RequestSuspend{Claim: rig.tokens[implementer], By: supervise.SuspendedByDaemon}); err != nil &&
			!errors.Is(err, supervise.ErrSuspendHeld) {
			t.Fatalf("suspend the waiting implementer: %v", err)
		}
		rig.retryLaunch()
		c := rig.awaitClaim(implementer, "the held suspension to suspend it", func(c supervise.Claim) bool {
			return c.State == supervise.StateSuspended && c.AwaitsPod == nil
		})
		if !c.PassedOverByRound() || c.Session != "" || len(c.RegisteredPods.Pods) != 0 || c.RegisteredPods.Unknown {
			t.Fatalf("the suspended implementer is %s by %q with session %q and set %+v; want it passed over, with no session and a known, empty set",
				c.State, c.SuspendedBy, c.Session, c.RegisteredPods)
		}
	case releasedLostSession:
		rig.handle(implementer, supervise.RequestForgetSession{Claim: rig.tokens[implementer], Session: "ses_" + string(implementer)})
		if c := rig.claimOf(implementer); !c.Released() || c.Session != "" || c.UpdateRound != podUpdateRound || len(c.RegisteredPods.Pods) != 1 || c.RegisteredPods.Unknown {
			t.Fatalf("the implementer is %s by %q with session %q in round %q and set %+v; want it released in the round, with no session and the set it registered P0 in",
				c.State, c.SuspendedBy, c.Session, c.UpdateRound, c.RegisteredPods)
		}
	case wokenRefused, daemonSuspendedWokenKept, operatorSuspendedWokenKept:
		refusals.refuse(implementer, podUpdatePod)
		if err := rig.machineOf(implementer).Handle(rig.ctx, rig.resumeOf(implementer, kind)); !errors.Is(err, runtime.ErrAwaitingPodRound) {
			t.Fatalf("the implementer's resume answered %v, want its launch refused for the pod's round", err)
		}
		if c := rig.claimOf(implementer); c.Session == "" || c.Locator != nil || podOf(c.AwaitsPod) != podUpdatePod {
			t.Fatalf("the resumed implementer has session %q and process %+v, waiting on %+v; want its session kept, no process, and the wait on pod %s",
				c.Session, c.Locator, c.AwaitsPod, podUpdatePod)
		}
	case wokenIntoPod:
		rig.intoPod(implementer, podUpdatePod)
		rig.handle(implementer, rig.resumeOf(implementer, kind))
		if c := rig.claimOf(implementer); c.State != supervise.StateLaunching || podOf(c.Locator) != podUpdatePod {
			t.Fatalf("the resumed implementer is %s at %+v, want it launching in pod %s, its agent not registered", c.State, c.Locator, podUpdatePod)
		}
	case wokenParked:
		pacing.park.Store(true)
		rig.intoPod(implementer, podUpdatePod)
		launched := len(rig.launchesOf(implementer))
		rig.handle(implementer, rig.wakeFor(implementer))
		view := rig.machineOf(implementer).View()
		if parked := view.WakeWaiting; parked == nil || parked.Kind != supervise.WakeLaunch || !parked.At.Equal(parkedWakeLetOut) ||
			view.State != supervise.StateLaunching || view.Locator != nil || view.AwaitsPod != nil ||
			len(view.RegisteredPods.Pods) != 1 || len(rig.launchesOf(implementer)) != launched {
			t.Fatalf("the woken implementer is %s at %+v waiting on %+v with wake %+v and set %+v after %d launches more; want it launching under the launch the pacing parked until %s, with no process, no pod waited on, the set the P0 it registered in and no launch made",
				view.State, view.Locator, view.AwaitsPod, parked, view.RegisteredPods, len(rig.launchesOf(implementer))-launched, parkedWakeLetOut)
		}
	}
	if by := kind.suspendsWoken(); by != "" {
		// The suspension of the woken implementer, whose launch waits on P, waits for the launch's
		// turn where the machine holds it, which the retry gives it.
		if err := rig.machineOf(implementer).Handle(rig.ctx, supervise.RequestSuspend{Claim: rig.tokens[implementer], By: by}); err != nil &&
			!errors.Is(err, supervise.ErrSuspendHeld) {
			t.Fatalf("suspend the woken implementer by %s: %v", by, err)
		} else if err != nil {
			rig.retryLaunch()
		}
		c := rig.awaitClaim(implementer, "its suspension before any pod", func(c supervise.Claim) bool {
			return c.State == supervise.StateSuspended && c.Locator == nil && c.AwaitsPod == nil
		})
		if c.SuspendedBy != by || c.Session == "" || len(c.RegisteredPods.Pods) != 1 || c.RegisteredPods.Pods[0] != releasedPod {
			t.Fatalf("the implementer is suspended by %q with session %q and set %+v; want the %s's suspension, its session and the set of releasedPod alone",
				c.SuspendedBy, c.Session, c.RegisteredPods, by)
		}
		rig.settleAll()
	}
	rig.signal(tester, "c0ffee")

	switch end {
	case endThroughAProcess:
		rig.awaitReplacement(1, "by the sweep")
		rig.awaitClaim(implementer, "the round's end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
		rig.settleAll()
		return rig
	case endByIssue:
		// No claim records a process or waits on a pod: the tester answered and the daemon
		// suspended it, and the implementer's launch waits in the wake pacing. The sweep replaces
		// P by issue, and the parked launch runs at its turn, into the pod built in P's place.
		rig.suspendBy(tester, supervise.SuspendedByDaemon)
		rig.awaitIssueReplacement("while the woken launch waits for its turn")
		rig.runParkedLaunch(implementer, podUpdateStart, parkedWakeLetOut)
	case endBySweep, endAtALaunch:
		// No role process is left in P. Where the implementer stays released, the tester's
		// process ends and its relaunch is refused for P, so the tester waits on P; where the
		// woken or resumed implementer waits on P, the workflow suspends the tester, which
		// answered, so the implementer's wait is the one way to P.
		through := implementer
		if kind.passedOver() {
			through = tester
			rig.goneRefused(refusals, tester, podUpdatePod, false)
		} else {
			rig.suspendBy(tester, supervise.SuspendedByDaemon)
		}
		if end == endBySweep {
			rig.awaitReplacement(1, "by the sweep")
			rig.awaitClaim(implementer, "the round's end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
			rig.settleAll()
			return rig
		}
		var pod string
		if kind == wokenReplacesThePod {
			// The wake's own launch, the implementer's first since the release, reads the round
			// over P under P's launch turn and replaces P (roundPodEndsAtLaunch).
			rig.intoPod(implementer, podUpdatePod)
			rig.handle(implementer, rig.wakeFor(implementer))
			pod = podOf(rig.awaitClaim(implementer, "its wake's launch to record its process", func(c supervise.Claim) bool { return c.Locator != nil }).Locator)
		} else {
			pod = rig.retriesInto(refusals, through, podUpdatePod)
		}
		rig.wantReplacedAtLaunch(through, pod)
	case endPodGone:
		rig.podGone()
	case endRecreated:
		rig.displacedInto(tester, podUpdateReplacementPod)
		switch kind.woken() {
		case wokenRefused:
			if pod := rig.retriesInto(refusals, implementer, podUpdateReplacementPod); pod != podUpdateReplacementPod {
				t.Fatalf("the implementer's retry came up in pod %s, want the pod in P's place, %s", pod, podUpdateReplacementPod)
			}
		case wokenIntoPod:
			rig.relaunchedUnregistered(implementer, podUpdateReplacementPod)
		case operatorTakeoverPodWent, daemonTakeoverPodWent:
			// P was recreated before any resume; the resume of the source that took the
			// implementer over comes up in the pod in P's place.
			rig.intoPod(implementer, podUpdateReplacementPod)
			rig.handle(implementer, rig.resumeOf(implementer, kind))
			rig.awaitClaim(implementer, "its resume into the pod in P's place", func(c supervise.Claim) bool {
				return podOf(c.Locator) == podUpdateReplacementPod
			})
		}
		rig.rt.ScriptPodDrift(rig.tokens[implementer])
		rig.rt.ScriptPodDrift(rig.tokens[tester])
	case endRecreatedRefusing:
		rig.goneRefused(refusals, tester, podUpdateReplacementPod, true)
		switch kind.woken() {
		case wokenRefused:
			refusals.refuse(implementer, podUpdateReplacementPod)
			rig.retryLaunch()
			rig.awaitClaim(implementer, "its retry refused for the pod in P's place", func(c supervise.Claim) bool {
				return podOf(c.AwaitsPod) == podUpdateReplacementPod
			})
		case wokenIntoPod:
			rig.goneRefused(refusals, implementer, podUpdateReplacementPod, true)
		case operatorTakeoverPodWent, daemonTakeoverPodWent:
			// P was recreated before any resume; the resume of the source that took the
			// implementer over is refused for the pod in P's place.
			refusals.refuse(implementer, podUpdateReplacementPod)
			if err := rig.machineOf(implementer).Handle(rig.ctx, rig.resumeOf(implementer, kind)); !errors.Is(err, runtime.ErrAwaitingPodRound) {
				t.Fatalf("the implementer's resume answered %v, want its launch refused for the pod in P's place", err)
			}
			if c := rig.claimOf(implementer); podOf(c.AwaitsPod) != podUpdateReplacementPod {
				t.Fatalf("the resumed implementer waits on %+v, want the pod in P's place, %s", c.AwaitsPod, podUpdateReplacementPod)
			}
		}
		rig.rt.ScriptPodDrift(rig.tokens[implementer])
		rig.rt.ScriptPodDrift(rig.tokens[tester])
	}
	rig.awaitSwept(implementer, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.settleAll()
	return rig
}

// releases is the idle release of pod, the issue's pod, in which each of roles runs, as the
// outbox's release rows make it once the release's check of the workspace every role of the issue
// shares found nothing that origin does not hold: each role stop suspends its claim released
// (supervise.SuspendedByReleased), which lets its process go. The teardown that takes pod is the
// runtime's, and no claim records pod after the stops.
func (r *podUpdateRig) releases(pod string, roles ...claim.Role) {
	r.t.Helper()
	for _, role := range roles {
		if ran := podOf(r.claimOf(role).Locator); ran != pod {
			r.t.Fatalf("the %s runs in pod %s, want it in pod %s, the pod the release takes", role, ran, pod)
		}
		r.suspendBy(role, supervise.SuspendedByReleased)
	}
}

// wakesInto is a wake of role's released claim (wakeFor) that brings the issue's pod back as pod,
// where role starts and its agent registers. The release left nothing unpushed, so a pod on a
// volume of its own holds nothing from before it. A pod whose Sandbox owns the issue's kept volume
// (podUpdateRig.keptSince) mounts the volume the released pod mounted, and its launcher records it
// with that volume at the role's start (launcherStarts), which marks it as holding work from
// before it.
func (r *podUpdateRig) wakesInto(role claim.Role, pod string) {
	r.t.Helper()
	r.launcherStarts(pod)
	r.intoPod(role, pod)
	r.handle(role, r.wakeFor(role))
	r.registerInto(role, pod)
}

// ownsKeptVolume makes the issue's Sandbox one that owns the issue's kept volume, made at made:
// the runtime answers that the issue's next launch keeps the volume (runtime.IssueVolumeReader),
// and each role's start into a pod from then on asks the store what a launcher of a pod of that
// Sandbox asks (podUpdateRig.keptSince, launcherStarts).
func (r *podUpdateRig) ownsKeptVolume(made time.Time) {
	r.rt.ScriptKeptVolume(podUpdateIssue, made)
	r.keptSince = made
}

// keptVolumeReplaced is the issue's Sandbox carrying a reset mark, or a change of its disk's shape,
// by the round's end: the runtime answers that the issue's next launch replaces the Sandbox with
// its volume, so the round's end deletes the volume and what is on it.
func (r *podUpdateRig) keptVolumeReplaced() {
	r.rt.ScriptKeptVolume(podUpdateIssue, time.Time{})
}

// wakeFor is the resume a wake makes of role's released claim, as the outbox's wake for a
// delivery the daemon holds for that role makes it (wakeForDelivery).
func (r *podUpdateRig) wakeFor(role claim.Role) supervise.RequestRuleResume {
	return supervise.RequestRuleResume{Claim: r.tokens[role], Reason: "a wake for a delivery the daemon holds for the " + string(role)}
}

// resumeOf is the resume of role's suspended claim by the source that suspended it last, as the
// order of kind makes it (memberKind.takenOverBy, memberKind.stoppedBeforeRelease): the operator's
// own resume (supervise.RequestResume) of a claim the operator suspended, which no rule resumes;
// the daemon's resume by its rule (supervise.RequestRuleResume) of a claim the daemon suspended,
// as the outbox's start of the role's phase makes it; and a wake (wakeFor) of a claim that stands
// released.
func (r *podUpdateRig) resumeOf(role claim.Role, kind memberKind) supervise.Event {
	switch cmp.Or(kind.takenOverBy(), kind.stoppedBeforeRelease()) {
	case supervise.SuspendedByOperator:
		return supervise.RequestResume{Claim: r.tokens[role]}
	case supervise.SuspendedByDaemon:
		return supervise.RequestRuleResume{Claim: r.tokens[role], Reason: "the outbox's start of the " + string(role) + "'s phase"}
	}
	return r.wakeFor(role)
}

// relaunchedUnregistered is role's process ending with its pod and the daemon starting the role
// again on its own session in pod (displacedInto), where no agent of it registers by the end.
func (r *podUpdateRig) relaunchedUnregistered(role claim.Role, pod string) {
	r.t.Helper()
	ended := r.claimOf(role).Locator
	if ended == nil {
		r.t.Fatalf("the %s records no process to end with its pod", role)
	}
	r.intoPod(role, pod)
	r.handle(role, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *ended, Kind: runtime.Gone, At: r.clock.Now(), Displaced: true,
	}})
	r.awaitClaim(role, "its relaunch into the pod in P's place", func(c supervise.Claim) bool { return podOf(c.Locator) == pod })
}
