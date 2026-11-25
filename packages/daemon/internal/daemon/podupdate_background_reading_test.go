package daemon

// R1's hold read over claims built here (dispatch://INFRA-4203 R1): the round's end
// (readRoundEnd) and the walk's reading (readReplacement) over one group, with no store, no
// machine and no runtime, so each row reads the rule and nothing else. The rig tests of
// podupdate_background_test.go drive the same rule through the sweep, the store and the claims'
// machines.

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// roundMember is one role of podUpdateIssue as the reading takes it: a claim with a session that
// the round asked and that answered it, idle, with a process in pod ("" for none), whose last
// turn started at podUpdateStart and whose report, read a second later, says that nothing of its
// own runs.
func roundMember(t *testing.T, role claim.Role, pod string) supervise.Claim {
	t.Helper()
	token := mustClaimToken(t, podUpdateIssue, role)
	opened, answered := podUpdateStart, podUpdateStart
	c := supervise.Claim{
		Token: token, Project: podUpdateProject, Tree: podUpdateIssue, Issue: podUpdateIssue, Role: role,
		Generation: 1, State: supervise.StateIdle, Session: "ses_" + string(role),
		UpdateRound: podUpdateRound, UpdateOpenedAt: &opened, UpdateSignalledAt: &answered,
		LastTurnStart: podUpdateStart,
		ActivityAt:    podUpdateStart.Add(time.Second),
	}
	if pod != "" {
		c.Locator = podUpdateLocator(token, podUpdateIssue, pod)
	}
	return c
}

// runningReport is c whose report, read at the same moment, names background work of the kinds
// that runs.
func runningReport(c supervise.Claim, kinds ...string) supervise.Claim {
	c.ActivityBackground, c.ActivityKinds = true, kinds
	return c
}

// inState is c in state; a suspended claim is the daemon's suspension, which no operator holds.
func inState(c supervise.Claim, state supervise.ClaimState) supervise.Claim {
	c.State = state
	if state == supervise.StateSuspended {
		c.SuspendedBy = supervise.SuspendedByDaemon
	}
	return c
}

// reportedAt is c whose report was read at at.
func reportedAt(c supervise.Claim, at time.Time) supervise.Claim {
	c.ActivityAt = at
	return c
}

// Each row is one group of the round podUpdateRound and one moment. Every claim the round asks
// has answered and no claim is an operator's, so the rows read R1's hold beside the holds of
// today. named and unreported are the hold as the round's end reads it, at the bound too, which
// is what the bound's line names; wait is the walk's reading; bounded is whether the bound is what
// let a replacement through; unsettled is a claim the round asks that has not answered or a turn
// that runs, which alone stops the read of the workspace (roundEnd.settled, dispatch://INFRA-4109
// R1): a running report the bound stepped over does not stop it.
func TestPodUpdateBackgroundReadingOfEachClaimOfTheRound(t *testing.T) {
	t.Parallel()
	before := podUpdateStart.Add(time.Minute)
	tests := []struct {
		name       string
		group      func(t *testing.T) []supervise.Claim
		now        time.Time
		wait       replacementWait
		named      map[string][]string
		unreported []string
		bounded    bool
		unsettled  bool
	}{
		{
			name: "a report read at the instant the last turn started counts, and one that names nothing running holds nothing",
			group: func(t *testing.T) []supervise.Claim {
				return []supervise.Claim{
					reportedAt(roundMember(t, claim.RoleImplementer, podUpdatePod), podUpdateStart),
					roundMember(t, claim.RoleTester, podUpdatePod),
				}
			},
			now:  before,
			wait: replacementReady,
		},
		{
			name: "a report read before the last turn started holds the round as a role with no report",
			group: func(t *testing.T) []supervise.Claim {
				return []supervise.Claim{
					roundMember(t, claim.RoleImplementer, podUpdatePod),
					reportedAt(roundMember(t, claim.RoleTester, podUpdatePod), podUpdateStart.Add(-time.Second)),
				}
			},
			now:        before,
			wait:       waitsForBackground,
			unreported: []string{string(claim.RoleTester)},
		},
		{
			name: "a claim with no report at all holds the round as a role with no report",
			group: func(t *testing.T) []supervise.Claim {
				return []supervise.Claim{
					reportedAt(roundMember(t, claim.RoleImplementer, podUpdatePod), time.Time{}),
					roundMember(t, claim.RoleTester, podUpdatePod),
				}
			},
			now:        before,
			wait:       waitsForBackground,
			unreported: []string{string(claim.RoleImplementer)},
		},
		{
			name: "a suspended claim that still records the round's pod holds nothing, whatever its report says",
			group: func(t *testing.T) []supervise.Claim {
				return []supervise.Claim{
					roundMember(t, claim.RoleImplementer, podUpdatePod),
					inState(runningReport(roundMember(t, claim.RoleTester, podUpdatePod), "subagent"), supervise.StateSuspended),
				}
			},
			now:  before,
			wait: replacementReady,
		},
		{
			name: "a retired claim holds nothing, whatever its report says",
			group: func(t *testing.T) []supervise.Claim {
				return []supervise.Claim{
					roundMember(t, claim.RoleImplementer, podUpdatePod),
					inState(runningReport(roundMember(t, claim.RoleTester, podUpdatePod), "subagent"), supervise.StateRetired),
				}
			},
			now:  before,
			wait: replacementReady,
		},
		{
			name: "a failed claim holds nothing, whatever its report says",
			group: func(t *testing.T) []supervise.Claim {
				return []supervise.Claim{
					roundMember(t, claim.RoleImplementer, podUpdatePod),
					inState(runningReport(roundMember(t, claim.RolePlanner, podUpdatePod), "async-job"), supervise.StateFailed),
				}
			},
			now:  before,
			wait: replacementReady,
		},
		{
			name: "past the bound a running report holds nothing, and the bound is what let the replacement through",
			group: func(t *testing.T) []supervise.Claim {
				return []supervise.Claim{
					runningReport(roundMember(t, claim.RoleImplementer, podUpdatePod), "subagent"),
					roundMember(t, claim.RoleTester, podUpdatePod),
				}
			},
			now:     podUpdateBound,
			wait:    replacementReady,
			named:   map[string][]string{string(claim.RoleImplementer): {"subagent"}},
			bounded: true,
		},
		{
			name: "a turn that runs beside another claim's running report is read first",
			group: func(t *testing.T) []supervise.Claim {
				return []supervise.Claim{
					runningReport(roundMember(t, claim.RoleImplementer, podUpdatePod), "subagent"),
					inState(roundMember(t, claim.RoleTester, podUpdatePod), supervise.StateWorking),
				}
			},
			now:       before,
			wait:      waitsForTurn,
			named:     map[string][]string{string(claim.RoleImplementer): {"subagent"}},
			unsettled: true,
		},
		{
			name: "a claim whose process is in another pod holds nothing, whatever its report says",
			group: func(t *testing.T) []supervise.Claim {
				return []supervise.Claim{
					roundMember(t, claim.RoleImplementer, podUpdatePod),
					runningReport(roundMember(t, claim.RoleTester, podUpdateReplacementPod), "subagent"),
				}
			},
			now:  before,
			wait: waitsForPodInFlight,
		},
		{
			name: "a claim the round has not asked, still launching in the round's pod with no session, holds the round with its kinds",
			group: func(t *testing.T) []supervise.Claim {
				planner := runningReport(inState(roundMember(t, claim.RolePlanner, podUpdatePod), supervise.StateLaunching), "subagent")
				planner.Session, planner.UpdateRound, planner.UpdateOpenedAt, planner.UpdateSignalledAt = "", "", nil, nil
				planner.LastTurnStart = time.Time{}
				return []supervise.Claim{
					roundMember(t, claim.RoleImplementer, podUpdatePod),
					planner,
					roundMember(t, claim.RoleTester, podUpdatePod),
				}
			},
			now:   before,
			wait:  waitsForBackground,
			named: map[string][]string{string(claim.RolePlanner): {"subagent"}},
		},
		{
			// Two claims of one issue can hold one role: the agent implementer's claim on the
			// tester's step, beside the tester's own claim. Each is named by its agent, which
			// names one claim of the issue, so the one's kinds do not stand for the other's.
			name: "two claims of one role that both report background work are each named by their agent",
			group: func(t *testing.T) []supervise.Claim {
				keyed := runningReport(roundMember(t, claim.RoleImplementer, podUpdatePod), "subagent")
				keyed.Role = claim.RoleTester
				return []supervise.Claim{
					keyed,
					runningReport(roundMember(t, claim.RoleTester, podUpdatePod), "async-job"),
				}
			},
			now:  before,
			wait: waitsForBackground,
			named: map[string][]string{
				string(claim.RoleImplementer): {"subagent"},
				string(claim.RoleTester):      {"async-job"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			group := tc.group(t)
			clock := roundClock{now: tc.now, grace: podUpdateGrace}
			bound := clock.roundBound(group, podUpdateRound)
			if bound == nil || !bound.Equal(podUpdateBound) {
				t.Fatalf("the group's bound is %v, want %s", bound, podUpdateBound)
			}

			end := readRoundEnd(group, podUpdateRound, clock)
			if got := end.background.running; !maps.EqualFunc(got, tc.named, slices.Equal[[]string]) {
				t.Errorf("the round's end names background %v, want %v", got, tc.named)
			}
			if got := end.background.unreported; !slices.Equal(got, tc.unreported) {
				t.Errorf("the round's end names unreported %v, want %v", got, tc.unreported)
			}
			if got := end.bounded(); got != tc.bounded {
				t.Errorf("the round's end reads bounded=%t, want %t", got, tc.bounded)
			}
			if got := end.settled(); got == tc.unsettled {
				t.Errorf("the round's end reads settled=%t, want %t", got, !tc.unsettled)
			}

			reading := readReplacement(group, podUpdateRound, clock)
			if reading.wait != tc.wait {
				t.Fatalf("the walk reads wait %d, want %d", reading.wait, tc.wait)
			}
			if tc.wait != waitsForBackground {
				if reading.background.holds() {
					t.Errorf("the walk's reading %d carries the hold %+v, want it set for waitsForBackground alone", reading.wait, reading.background)
				}
				return
			}
			if got := reading.background.running; !maps.EqualFunc(got, tc.named, slices.Equal[[]string]) {
				t.Errorf("the walk's reading names background %v, want %v", got, tc.named)
			}
			if got := reading.background.unreported; !slices.Equal(got, tc.unreported) {
				t.Errorf("the walk's reading names unreported %v, want %v", got, tc.unreported)
			}
		})
	}
}
