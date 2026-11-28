package admit

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// nowStandsAboveWords is the sentence of a re-parent notice that names key's architect as the
// architect above the moved issue now (workflow's reparentedWords).
func nowStandsAboveWords(key string) string {
	return "The architect of " + key + " now stands above it"
}

// R18 of dispatch://INFRA-3320: "When the Dispatch parent of an issue in Legion's work changes, the daemon tells
// ... the architect above it now", and "The architect above it now reads the issue's spec against
// its own requirements ... and keeps it, or writes done, icebox or backlog on it"; the architect's
// ruling dispatch://INFRA-4730/comment/000d0b0f-743a-4130-8ba2-3c95b97d1ff7, item 4: "Tell the
// architect above now once the chain is known, whatever the issue's phase is then". A person moves
// LEGION-500, at work under LEGION-208, under LEGION-700, which the copy lacks; then writes backlog
// on it; then, while it is out of Legion's work, moves it under another parent (LEGION-300, or back
// under LEGION-208); then writes todo, and it returns under that parent. R16's read of the gap then
// lands (LEGION-700 under LEGION-600, a root at work with its architect). The issue stands under
// the later parent, so the architect of LEGION-600 does not stand above it now: no notice may tell
// that architect it now stands above the issue and asks it to keep or end it, and no notice may
// name LEGION-600's architect as the architect above it now. The seeds are read either only after
// the return or also while the issue is stopped, as the outbox runs a seed at once.
func TestAMoveWhileStoppedAfterAMoveOntoAnUnknownChainNamesNoFormerArchitectAsAboveNow(t *testing.T) {
	const aboveGap = "LEGION-600"
	for _, tc := range []struct {
		later            string
		readWhileStopped bool
	}{{rstNew, false}, {rstOld, false}, {rstNew, true}, {rstOld, true}} {
		later := tc.later
		t.Run(fmt.Sprintf("stopped, then moved under %s, seeds read while stopped %t", later, tc.readWhileStopped), func(t *testing.T) {
			t.Parallel()
			pool, admission := rstFixture(t)
			seedStarted(t, pool, aboveGap, phase.Planning, nil)
			putWorkerClaim(t, pool, aboveGap, aboveGap, claim.RoleArchitect, 1, supervise.StateWorking, "")
			rstMove(t, pool, admission, "in_progress", rstUnread, 2)
			if seeds := rstSeeds(t, pool); len(seeds) != 1 || seeds[0].seed.Parent != rstUnread {
				t.Fatalf("the re-parent seeds of %s = %+v, want one, to %s: the case's premise", rstChild, seeds, rstUnread)
			}
			rstMove(t, pool, admission, "backlog", rstUnread, 3)
			rstMove(t, pool, admission, "backlog", later, 4)
			var last int64
			if tc.readWhileStopped {
				// The outbox runs the seed at once, while the issue is out of Legion's work.
				last = rstReadSeeds(t, pool, admission, 0)
			}
			rstMove(t, pool, admission, "todo", later, 5)
			back := issue(t, pool, rstChild)
			if !record.InLegionsWork(back) || back.Parent == nil || *back.Parent != later {
				t.Fatalf("%s after the todo = %s/%s under %v, want it back in Legion's work under %s: the case's premise", rstChild, back.Phase, back.Status, back.Parent, later)
			}
			last = rstReadSeeds(t, pool, admission, last)
			spMirror(t, pool, rstUnread, "backlog", aboveGap, 9)
			last = rstReadSeeds(t, pool, admission, last)
			rstReadSeeds(t, pool, admission, last)
			rstLogTold(t, pool)
			t.Logf("reparented notices to %s: %q; %s's record: parent %v, told parent %q", aboveGap, rstToldTo(t, pool, aboveGap), rstChild, *issue(t, pool, rstChild).Parent, issue(t, pool, rstChild).ToldParent)
			for _, to := range []string{rstChild, rstOld, rstNew, aboveGap} {
				for _, reason := range rstToldTo(t, pool, to) {
					if strings.Contains(reason, nowStandsAboveWords(aboveGap)) {
						t.Errorf("the notice to the architect of %s says %q: it names the architect of %s as above %s now, while %s stands under %s (R18: the architect above it now)",
							to, reason, aboveGap, rstChild, rstChild, later)
					}
				}
			}
		})
	}
}
