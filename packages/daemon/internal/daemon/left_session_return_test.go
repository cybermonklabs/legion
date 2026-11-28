package daemon

import (
	"fmt"
	"testing"
)

// The tester's red test of INFRA-4730 at 5e4d775a, on the merges 82f691b1 and 1647406b, where
// main's #5908 (release the Dispatch claim of a session the daemon leaves behind) meets this
// branch's R20, and on 5e4d775a's port of #5908's tests to R20's return. R20 of
// dispatch://INFRA-3320: "When Dispatch shows it in work again, it stands in the admission line
// among the work that came back and resumes the phase it stood in at a new phase entry, with its
// sessions and its generation". INFRA-3144 (main's #5908): R1 "A move to backlog, icebox or
// triage that takes an issue out of the workflow releases the Dispatch claim its own tree's
// session holds"; R2 "Where the daemon leaves a claim on purpose, the reason is in the record a
// role can read"; R4 "A claim held by the session that works a claim of the issue's tree now
// stays: no claim release takes it"; R5 "When the daemon leaves a session behind for a claim, by a
// fresh session ..., it releases the Dispatch claim that session holds on the claim's issue."

// leftReleasesOf is every claim of issue that Dispatch released, in order.
func leftReleasesOf(r *leftRig, issue string) []leftRelease {
	var of []leftRelease
	for _, released := range r.dispatch.releases() {
		if released.issue == issue {
			of = append(of, released)
		}
	}
	return of
}

// The issue's own architect claims it on Dispatch in session S1; the controller's fresh_session
// leaves S1 behind (the architect now records S2), and S1's own release row is refused while
// Dispatch reads S1 live (or, as for a session left behind before #5908 was deployed, no row of
// S1's is queued). A person moves the issue to backlog, S1's process is gone, and the person
// writes todo: the release rows run before the return or only after it. The issue returns at
// generation 1 at a later phase entry (R20), and Dispatch releases S1's claim of the issue once,
// taking it from S1 alone. Then the returned architect's session S2 claims the issue, and every
// release row due runs again: S2's claim stays, and, for a child, the root's claim held by the
// root architect's live session, above the child on its chain, stays. Where the left session has
// no row and the issue returns before the move's row runs, the claim is released, or the record
// says why it stays (INFRA-3144 R2), as main's issue-runs arm says.
func TestAStopAndAReturnKeepTheGenerationAndReleaseTheLeftSessionsClaimOnceAndOnlyItsOwn(t *testing.T) {
	t.Parallel()
	const rootArchitect = "ses-root-architect"
	for _, issue := range []string{leftRoot, leftChild} {
		for _, rowsBeforeReturn := range []bool{false, true} {
			for _, ownRow := range []bool{true, false} {
				name := fmt.Sprintf("%s, release rows run before the return %t, the left session's own row queued %t", issue, rowsBeforeReturn, ownRow)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					r := newLeftRig(t)
					tr := leftTreeOf(r, issue)
					if issue == leftChild {
						r.dispatch.takes(leftRoot, rootArchitect)
					}
					r.dispatch.takes(issue, tr.session)
					left := tr.freshSession()
					if !ownRow {
						r.unqueue(left)
					}
					r.releases()
					for range 7 {
						r.later()
					}
					if waiting := r.queuedFor(left); ownRow && (waiting == nil || waiting.attempts < 8) {
						t.Fatalf("premise: the release of %s before the move is %+v, want it queued and refused while %s lives", left, waiting, left)
					}
					if got := r.dispatch.holder(issue); got != "session:"+left {
						t.Fatalf("premise: Dispatch shows %s claimed by %q before the move, want the claim of %s", issue, got, left)
					}
					tr.moveOut("backlog")
					stopped := r.fence(issue)
					r.dispatch.ends(left)
					if rowsBeforeReturn {
						r.releases()
					}
					r.move(issue, "todo")
					r.admitPass()
					r.wantReturned(issue, stopped)
					for range 3 {
						r.later()
					}
					r.drain()
					released := leftReleasesOf(r, issue)
					claimLeft := r.claimLeft(issue)
					t.Logf("after the stop and the return: %s at generation %d, fence %+v (stopped at %+v); released %+v; claimLeft %v; queued releases %+v; Dispatch holder %q",
						issue, r.issue(issue).Generation, r.fence(issue), stopped, released, claimLeft, r.queuedReleases(), r.dispatch.holder(issue))
					if r.issue(issue).Generation != 1 {
						t.Errorf("%s after its stop and return stands at generation %d, want 1: R20 keeps the generation on a return", issue, r.issue(issue).Generation)
					}
					for _, rel := range released {
						if rel.holder != "session:"+left {
							t.Errorf("Dispatch released the claim of %s held by %s (row %q), want only the claim of the left session %s released", issue, rel.holder, rel.row, left)
						}
					}
					switch {
					case len(released) > 1:
						t.Errorf("Dispatch released the claim of %s %d times (%+v), want once", issue, len(released), released)
					case len(released) == 0 && (ownRow || rowsBeforeReturn):
						t.Errorf("Dispatch never released the claim of %s held by %s, the session the daemon left behind (INFRA-3144 R1, R5); claimLeft %v", issue, left, claimLeft)
					case len(released) == 0 && (claimLeft == nil || claimLeft["holder"] != "session:"+left):
						t.Errorf("the claim of %s held by %s, a session of its own tree the daemon left behind, stays after the move and the return, and the record names claimLeft %v: want it released (INFRA-3144 R1) or the reason it stays in the record (R2), as main's issue-runs arm records it",
							issue, left, claimLeft)
					}

					r.dispatch.lives(tr.session)
					r.dispatch.takes(issue, tr.session)
					for range 3 {
						r.later()
					}
					r.drain()
					if got := r.dispatch.holder(issue); got != "session:"+tr.session {
						t.Errorf("Dispatch shows %s claimed by %q after the rows ran again, want the claim of %s, whose architect works it now (INFRA-3144 R4)", issue, got, tr.session)
					}
					if again := leftReleasesOf(r, issue); len(again) != len(released) {
						t.Errorf("Dispatch released the claim of %s again after %s took it: %+v, want no release past %+v (INFRA-3144 R4)", issue, tr.session, again, released)
					}
					if issue == leftChild {
						if got := r.dispatch.holder(leftRoot); got != "session:"+rootArchitect {
							t.Errorf("Dispatch shows %s claimed by %q, want the claim of %s, the root architect's live session, kept (INFRA-3144 R4)", leftRoot, got, rootArchitect)
						}
						if of := leftReleasesOf(r, leftRoot); len(of) != 0 {
							t.Errorf("Dispatch released the claim of %s: %+v, want none: its holder %s works the root now", leftRoot, of, rootArchitect)
						}
					}
					r.wantNoUnattributedLine()
				})
			}
		}
	}
}
