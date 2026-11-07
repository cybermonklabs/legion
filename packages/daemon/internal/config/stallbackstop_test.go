package config

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// backstopFile is minimalFile with the key under test, written so each case names only its own
// roles.
func backstopFile(t *testing.T, body string) string {
	t.Helper()
	return writeConfigFile(t, minimalFile+"controller: daemon\n"+body)
}

// A file that names no bound runs every role on twice its own bound, which is what the approved
// spec's Design says: the daemon's backstop bound is twice the role's, so the worker acts first
// wherever it can act at all. The implementer and the reviewer wait four hours inside one call,
// so theirs is eight; every other role's and the controller's is two. The floor each of them
// must clear is the role's whole ladder, which is a different number and a shorter one.
func TestTheStallBackstopDefaultsToTwiceTheRolesOwnBound(t *testing.T) {
	cfg, err := Load(backstopFile(t, ""), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WorkerStallBackstops != nil {
		t.Fatalf("worker_stall_backstop_hours = %v, want none named", cfg.WorkerStallBackstops)
	}
	shortLadder := StallCallBound + StallCallGrace + 2*StallStep
	longLadder := StallCallBoundLong + StallCallGrace + 2*StallStep
	for role, want := range map[claim.Role]struct{ backstop, ladder time.Duration }{
		claim.RoleArchitect:   {2 * StallCallBound, shortLadder},
		claim.RolePlanner:     {2 * StallCallBound, shortLadder},
		claim.RoleImplementer: {2 * StallCallBoundLong, longLadder},
		claim.RoleTester:      {2 * StallCallBound, shortLadder},
		claim.RoleReviewer:    {2 * StallCallBoundLong, longLadder},
		claim.RoleMerger:      {2 * StallCallBound, shortLadder},
		claim.RoleController:  {2 * StallCallBound, shortLadder},
	} {
		if got := cfg.StallBackstopFor(role); got != want.backstop {
			t.Errorf("StallBackstopFor(%s) = %s, want twice the role's bound, %s", role, got, want.backstop)
		}
		if got := cfg.StallLadderFor(role); got != want.ladder {
			t.Errorf("StallLadderFor(%s) = %s, want %s", role, got, want.ladder)
		}
	}
}

// A role the key names runs on the bound it names, in hours and decimal hours alike, and every
// role it leaves out keeps twice its own bound. The controller is a role the key may name,
// as it is one `role_config_files` may name.
func TestTheStallBackstopTakesAPerRoleBound(t *testing.T) {
	cfg, err := Load(backstopFile(t, "worker_stall_backstop_hours:\n  implementer: 6\n  controller: 1.5\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.StallBackstopFor(claim.RoleImplementer); got != 6*time.Hour {
		t.Errorf("the implementer's bound = %s, want 6h", got)
	}
	if got := cfg.StallBackstopFor(claim.RoleController); got != 90*time.Minute {
		t.Errorf("the controller's bound = %s, want 1h30m", got)
	}
	if want := 2 * StallCallBoundLong; cfg.StallBackstopFor(claim.RoleReviewer) != want {
		t.Errorf("the reviewer's bound = %s, want twice its own bound, %s", cfg.StallBackstopFor(claim.RoleReviewer), want)
	}
}

// 0 is the backstop off for that role, and it is the one value below the role's ladder the file
// may set: no sweep acts on a role whose bound is 0, so nothing of the ladder is in its way.
func TestAZeroStallBackstopSwitchesTheRoleOff(t *testing.T) {
	cfg, err := Load(backstopFile(t, "worker_stall_backstop_hours: {tester: 0, implementer: 0}\n"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer} {
		if got := cfg.StallBackstopFor(role); got != 0 {
			t.Errorf("StallBackstopFor(%s) = %s, want the backstop off", role, got)
		}
	}
	if want := 2 * StallCallBound; cfg.StallBackstopFor(claim.RoleMerger) != want {
		t.Errorf("the merger's bound = %s, want twice its own bound %s: 0 is one role's answer, not the file's", cfg.StallBackstopFor(claim.RoleMerger), want)
	}
}

// A bound below the role's whole ladder is refused with the arithmetic in the message, because
// the daemon would end a turn the plugin it backs up is still recovering. The ladder's own length
// passes: that is the first bound at which nothing of the plugin's is left to run.
func TestAStallBackstopBelowTheLadderIsRefusedWithItsArithmetic(t *testing.T) {
	_, err := Load(backstopFile(t, "worker_stall_backstop_hours: {implementer: 4}\n"), noEnv)
	want := "worker_stall_backstop_hours.implementer (4h0m0s) is below the whole stall ladder the implementer's own plugin runs (4h0m0s + 5m0s + 2 × 2m0s = 4h9m0s, where the first number is the 4h0m0s bound a call no deadline judges runs on): the daemon's backstop would end a turn the plugin it backs up is still recovering; set 0 to switch the backstop off for that role"
	if err == nil || err.Error() != want {
		t.Fatalf("Load = %v, want %q", err, want)
	}

	ladder := StallCallBoundLong + StallCallGrace + 2*StallStep
	cfg, err := Load(backstopFile(t, "worker_stall_backstop_hours: {implementer: 4.15}\n"), noEnv)
	if err != nil {
		t.Fatalf("Load of the ladder's own length: %v", err)
	}
	if got := cfg.StallBackstopFor(claim.RoleImplementer); got != ladder {
		t.Fatalf("the implementer's bound = %s, want the ladder %s accepted", got, ladder)
	}
}

// The floor covers a call the worker judges by its own declared deadline, not only one it judges
// by the role's bound: the host clamps a declared deadline to an hour (R6), so a working call
// can legitimately run that hour and the grace over it before the worker's own ladder takes its
// first step. A deployment that shortens the bound below the hour has shortened nothing of that,
// and the floor it leaves is still the hour's.
func TestTheStallBackstopFloorCoversACallsOwnDeclaredDeadline(t *testing.T) {
	env := envMap(map[string]string{"LEGION_STALL_CALL_BOUND_MS": "1800000"})
	floor := StallDeclaredDeadlineCap + StallCallGrace + 2*StallStep

	_, err := Load(backstopFile(t, "worker_stall_backstop_hours: {tester: 1}\n"), env)
	want := "worker_stall_backstop_hours.tester (1h0m0s) is below the whole stall ladder the tester's own plugin runs (1h0m0s + 5m0s + 2 × 2m0s = 1h9m0s, where the first number is the hour the host clamps a call's own declared deadline to, longer here than the 30m0s bound a call no deadline judges runs on): the daemon's backstop would end a turn the plugin it backs up is still recovering; set 0 to switch the backstop off for that role"
	if err == nil || err.Error() != want {
		t.Fatalf("Load = %v, want %q", err, want)
	}

	cfg, err := Load(backstopFile(t, ""), env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer} {
		if got := cfg.StallLadderFor(role); got != floor {
			t.Errorf("StallLadderFor(%s) = %s, want the hour the host clamps a declared deadline to, %s", role, got, floor)
		}
		// Twice a half-hour bound is below that floor, so the default every role the file leaves
		// out runs on is the floor itself: a default below it would end a turn the file is
		// refused for ending.
		if got := cfg.StallBackstopFor(role); got != floor {
			t.Errorf("StallBackstopFor(%s) = %s, want the floor %s: twice this bound is below it", role, got, floor)
		}
	}
}

// LEGION_STALL_CALL_BOUND_MS of 0 is the worker's call watch switched off, not a bound of zero:
// no call is judged overdue and no step of the recovery ladder runs, so there is no ladder for
// the daemon's bound to stay above and nothing of the worker's to finish first. Both the
// backstop's default and its floor are reckoned from the role's own default bound, exactly as
// though the deployment had set no bound at all — otherwise a deployment that asked the worker
// to stop acting would have asked the daemon to end healthy turns nine minutes in.
func TestAZeroedStallCallBoundIsTheWorkersWatchOffAndNotABoundOfZero(t *testing.T) {
	env := envMap(map[string]string{"LEGION_STALL_CALL_BOUND_MS": "0"})
	cfg, err := Load(backstopFile(t, ""), env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for role, want := range map[claim.Role]struct{ backstop, ladder time.Duration }{
		claim.RoleImplementer: {2 * StallCallBoundLong, StallCallBoundLong + StallCallGrace + 2*StallStep},
		claim.RoleReviewer:    {2 * StallCallBoundLong, StallCallBoundLong + StallCallGrace + 2*StallStep},
		claim.RoleTester:      {2 * StallCallBound, StallCallBound + StallCallGrace + 2*StallStep},
		claim.RoleController:  {2 * StallCallBound, StallCallBound + StallCallGrace + 2*StallStep},
	} {
		if got := cfg.StallBackstopFor(role); got != want.backstop {
			t.Errorf("StallBackstopFor(%s) = %s with the worker's watch off, want the role's own %s", role, got, want.backstop)
		}
		if got := cfg.StallLadderFor(role); got != want.ladder {
			t.Errorf("StallLadderFor(%s) = %s with the worker's watch off, want the role's own %s", role, got, want.ladder)
		}
	}

	// The floor is the role's own too, so a file that would have been refused with the watch on
	// is refused with it off.
	_, err = Load(backstopFile(t, "worker_stall_backstop_hours: {implementer: 4}\n"), env)
	want := "worker_stall_backstop_hours.implementer (4h0m0s) is below the whole stall ladder the implementer's own plugin runs (4h0m0s + 5m0s + 2 × 2m0s = 4h9m0s, where the first number is the 4h0m0s bound a call no deadline judges runs on): the daemon's backstop would end a turn the plugin it backs up is still recovering; set 0 to switch the backstop off for that role"
	if err == nil || err.Error() != want {
		t.Fatalf("Load with the worker's watch off = %v, want %q", err, want)
	}
}

// The floor is the ladder the worker will actually run, so it reads the deployment's own
// LEGION_STALL_* numbers where it sets them: one LEGION_STALL_CALL_BOUND_MS is every role's, as
// the daemon copies one name from its own environment to every session, so a bound that clears
// the default ladder can still be below the deployment's own.
func TestTheStallBackstopFloorReadsTheEnvironmentsBound(t *testing.T) {
	env := envMap(map[string]string{
		"LEGION_STALL_CALL_BOUND_MS":  "21600000",
		"LEGION_STALL_CALL_GRACE_MS":  "600000",
		"LEGION_STALL_STEP_MS":        "180000",
		"LEGION_STALL_NOT_A_LIMIT_MS": "1",
	})
	_, err := Load(backstopFile(t, "worker_stall_backstop_hours: {tester: 5}\n"), env)
	want := "worker_stall_backstop_hours.tester (5h0m0s) is below the whole stall ladder the tester's own plugin runs (6h0m0s + 10m0s + 2 × 3m0s = 6h16m0s, where the first number is the 6h0m0s bound a call no deadline judges runs on): the daemon's backstop would end a turn the plugin it backs up is still recovering; set 0 to switch the backstop off for that role"
	if err == nil || err.Error() != want {
		t.Fatalf("Load = %v, want %q", err, want)
	}

	// The same numbers move the default every role the file leaves out runs on, the
	// implementer's included: the variable replaces the role's own default for every role alike,
	// and the default is twice it.
	cfg, err := Load(backstopFile(t, ""), env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer, claim.RoleController} {
		if got, want := cfg.StallBackstopFor(role), 12*time.Hour; got != want {
			t.Errorf("StallBackstopFor(%s) = %s, want twice the environment's own bound, %s", role, got, want)
		}
		if got, want := cfg.StallLadderFor(role), 6*time.Hour+16*time.Minute; got != want {
			t.Errorf("StallLadderFor(%s) = %s, want the environment's own ladder %s", role, got, want)
		}
	}

	// A value the worker's own plugin would refuse — not a whole number of milliseconds, or
	// below zero — is the default there and here, since the default is what that worker runs on.
	bad := envMap(map[string]string{"LEGION_STALL_CALL_BOUND_MS": "soon", "LEGION_STALL_STEP_MS": "-1"})
	cfg, err = Load(backstopFile(t, ""), bad)
	if err != nil {
		t.Fatalf("Load with unreadable numbers: %v", err)
	}
	if got, want := cfg.StallBackstopFor(claim.RoleTester), 2*StallCallBound; got != want {
		t.Errorf("StallBackstopFor(tester) = %s, want twice the plugin's own default %s", got, want)
	}
}

// One LEGION_STALL_*_MS is read from the set of strings the plugin's own reader takes it from
// and no other (`packages/pi-shared/src/stall-limits.ts`, integerSetting): the daemon's floor
// must be computed from the bound the worker is actually running on, and a reader of another
// grammar reads another bound. Every expectation below is what `Number.isInteger(Number(raw))
// && Number(raw) >= 0` answers for that string.
func TestStallMillisecondsReadsThePluginsOwnSetOfStrings(t *testing.T) {
	const none = -1
	for _, tc := range []struct {
		raw  string
		want time.Duration // none where the plugin's default stands
	}{
		{"5", 5 * time.Millisecond},
		{"0", 0},
		{"007", 7 * time.Millisecond},
		{"1800000", 30 * time.Minute},
		// The three strings the two grammars divide on: strconv.Atoi refuses each, and the
		// plugin takes each.
		{" 5 ", 5 * time.Millisecond},
		{"0x10", 16 * time.Millisecond},
		{"1.44e7", 4 * time.Hour},
		// The rest of what Number reads.
		{"0X10", 16 * time.Millisecond},
		{"0o17", 15 * time.Millisecond},
		{"0b101", 5 * time.Millisecond},
		{"1e3", time.Second},
		{"1E3", time.Second},
		{"1e+3", time.Second},
		{"2.0", 2 * time.Millisecond},
		{"0.0", 0},
		{"5.", 5 * time.Millisecond},
		{"+5", 5 * time.Millisecond},
		{"-0", 0},
		{"\n7\t", 7 * time.Millisecond},
		{"\u00a05\u00a0", 5 * time.Millisecond},
		{"\v5", 5 * time.Millisecond},
		// What Number answers NaN for, or no whole number of 0 or more: each is the plugin's
		// default, and so this reader's.
		{"", none},
		{"   ", none},
		{".5", none},
		{"5.5", none},
		{"1e-3", none},
		{"-1", none},
		{"1_000", none},
		{"Infinity", none},
		{"-Infinity", none},
		{"NaN", none},
		{"nan", none},
		{"inf", none},
		{"soon", none},
		{"5 5", none},
		{"0x", none},
		{"0xzz", none},
		{"1n", none},
		// The one string the two read differently, and the reason: the daemon cannot hold a
		// number of milliseconds a time.Duration has no room for.
		{"1e21", none},
		// The same line drawn through a based literal, which is read as a 64-bit integer
		// before it is a number at all: the largest a Duration holds (maxStallMilliseconds,
		// 0x8637bd05af6) is taken, the next one up and the largest literal ParseUint reads
		// at all leave the plugin's default standing.
		{"0x8637bd05af6", time.Duration(maxStallMilliseconds) * time.Millisecond},
		{"0x8637bd05af7", none},
		{"0xFFFFFFFFFFFFFFFF", none},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, ok := stallMilliseconds(tc.raw)
			if tc.want == none {
				if ok {
					t.Fatalf("stallMilliseconds(%q) = %s, want the plugin's own default left standing", tc.raw, got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("stallMilliseconds(%q) = %s, %t, want %s", tc.raw, got, ok, tc.want)
			}
		})
	}
}

// What the key refuses before anything else is read.
func TestStallBackstopRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"not a mapping", "worker_stall_backstop_hours: 6\n",
			"worker_stall_backstop_hours must be a mapping of a role to the hours the daemon's backstop waits, e.g. {implementer: 6, tester: 0}"},
		{"an unknown role", "worker_stall_backstop_hours: {coder: 6}\n",
			`worker_stall_backstop_hours names "coder", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller)`},
		{"a role named twice", "worker_stall_backstop_hours: {tester: 2, tester: 3}\n",
			"worker_stall_backstop_hours names tester twice"},
		{"a value that is no number", "worker_stall_backstop_hours: {tester: soon}\n",
			"worker_stall_backstop_hours.tester must be a number of hours, or 0 to switch the backstop off for that role"},
		{"a negative bound", "worker_stall_backstop_hours: {tester: -1}\n",
			"worker_stall_backstop_hours.tester must be 0 (the backstop off for that role) or a positive number of hours"},
		{"a bound past the timer bound", "worker_stall_backstop_hours: {tester: 1000}\n",
			"worker_stall_backstop_hours.tester must be at most 596"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(backstopFile(t, tc.body), noEnv)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Load = %v, want %q", err, tc.want)
			}
		})
	}
}
