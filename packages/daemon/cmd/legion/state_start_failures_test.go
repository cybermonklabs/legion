package main

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
)

// `legion state` names each claim whose last start failed with how long ago that start failed, so
// the operator reads a failure of days ago as days old: on 2026-11-20 one project's state carried
// a launch failure fixed two days before on 576 claims, and its reader took it for a current one.
// A failure whose moment no release kept says `time unknown` and never a time made up for it.
func TestTheStateNamesEachFailedStartWithItsAge(t *testing.T) {
	now := time.Date(2026, 11, 20, 23, 12, 0, 0, time.UTC)
	fourDaysAgo := now.Add(-96 * time.Hour)
	anHourAgo := now.Add(-time.Hour)
	const refused = "launch legion-legion-legion-215-implementer: start through its authenticated launcher: the shared public leak check could not be installed: the served check could not be run: waitid: no child processes"
	const notReady = "launch legion-legion-legion-216-tester: start through its authenticated launcher: the pod never became ready"
	const evicted = "launch legion-legion-legion-217-architect: start through its authenticated launcher: the pod was evicted"

	for _, tc := range []struct {
		name   string
		issues map[string]api.Issue
		want   []string
	}{
		{
			name:   "no claim's start failed",
			issues: map[string]api.Issue{"LEGION-215": {Workers: map[legionclaim.Role]api.PhaseView{legionclaim.RoleImplementer: {Claim: api.ClaimView{State: "working"}}}}},
			want:   []string{"last start failures: none"},
		},
		{
			name: "a failure four days old",
			issues: map[string]api.Issue{"LEGION-215": {Workers: map[legionclaim.Role]api.PhaseView{legionclaim.RoleImplementer: {Claim: api.ClaimView{
				State: "working", LastStartFailure: refused, LastStartFailureAt: &fourDaysAgo,
			}}}}},
			want: []string{
				"last start failures: 1, newest first",
				"  LEGION-215 implementer working: 96h0m0s ago, at 2026-11-16T23:12:00Z: " + refused,
			},
		},
		{
			name: "a failure whose moment no release kept",
			issues: map[string]api.Issue{"LEGION-216": {Workers: map[legionclaim.Role]api.PhaseView{legionclaim.RoleTester: {Claim: api.ClaimView{
				State: "idle", LastStartFailure: notReady,
			}}}}},
			want: []string{
				"last start failures: 1, newest first",
				"  LEGION-216 tester idle: time unknown: " + notReady,
			},
		},
		{
			// The newest failure comes first, whoever it is, and a failure of unknown moment
			// after every dated one: nothing says how old it is, so it is never read as the newest.
			name: "the newest first, and the unknown last",
			issues: map[string]api.Issue{
				"LEGION-215": {Workers: map[legionclaim.Role]api.PhaseView{legionclaim.RoleImplementer: {Claim: api.ClaimView{
					State: "working", LastStartFailure: refused, LastStartFailureAt: &fourDaysAgo,
				}}}},
				"LEGION-216": {Workers: map[legionclaim.Role]api.PhaseView{legionclaim.RoleTester: {Claim: api.ClaimView{
					State: "idle", LastStartFailure: notReady,
				}}}},
				"LEGION-217": {Architect: &api.ClaimView{State: "launching", LastStartFailure: evicted, LastStartFailureAt: &anHourAgo}},
			},
			want: []string{
				"last start failures: 3, newest first",
				"  LEGION-217 architect launching: 1h0m0s ago, at 2026-11-20T22:12:00Z: " + evicted,
				"  LEGION-215 implementer working: 96h0m0s ago, at 2026-11-16T23:12:00Z: " + refused,
				"  LEGION-216 tester idle: time unknown: " + notReady,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := startFailureLines(tc.issues, now); !slices.Equal(got, tc.want) {
				t.Errorf("startFailureLines =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// A deployment can carry hundreds of failed starts, so the lines name the newest
// startFailureNamedCap of them and then how many more there are; `legion state --json` carries
// every one with its moment.
func TestTheStateNamesTheNewestFailedStartsAndCountsTheRest(t *testing.T) {
	now := time.Date(2026, 11, 20, 23, 12, 0, 0, time.UTC)
	issues := map[string]api.Issue{}
	total := startFailureNamedCap + 3
	for i := range total {
		at := now.Add(-time.Duration(i+1) * time.Minute)
		issues[fmt.Sprintf("LEGION-%d", 300+i)] = api.Issue{Workers: map[legionclaim.Role]api.PhaseView{legionclaim.RoleImplementer: {Claim: api.ClaimView{
			State: "working", LastStartFailure: "the pod never became ready", LastStartFailureAt: &at,
		}}}}
	}
	got := startFailureLines(issues, now)
	if len(got) != startFailureNamedCap+2 {
		t.Fatalf("startFailureLines named %d lines, want the header, the newest %d and one count: %q", len(got), startFailureNamedCap, got)
	}
	if got[0] != fmt.Sprintf("last start failures: %d, newest first", total) {
		t.Errorf("the header is %q, want it to count all %d", got[0], total)
	}
	if want := "  LEGION-300 implementer working: 1m0s ago, at 2026-11-20T23:11:00Z: the pod never became ready"; got[1] != want {
		t.Errorf("the first named failure is %q, want the newest, %q", got[1], want)
	}
	if want := "  and 3 more"; got[len(got)-1] != want {
		t.Errorf("the last line is %q, want %q", got[len(got)-1], want)
	}
}
