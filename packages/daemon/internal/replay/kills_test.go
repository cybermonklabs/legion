package replay

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// guardKillTold is the daemon's line at at of the memory guard's kill of process pid of key's role,
// whose role it tells why (workflow's memoryGuardKill), or, where cause is no guard's, the line of
// a kill of that cause of an issue the daemon does not record.
func guardKillTold(b *windowBuilder, at time.Time, key string, role claim.Role, pid int, cause string) {
	b.t.Helper()
	msg := guardKillToldLine
	if cause != "" {
		msg = guardKillUnrecordedLine
	}
	b.info(at, msg, "issue", key, "role", string(b.token(key, role)), "pod", "00000000-0000-4000-8000-0000000000aa", "guard", "guard-1",
		"sequence", pid, "pid", pid, "cause", cause, "sizeBytes", 9000000000, "readingBytes", 9000000000, "counterBytes", 1, "limitBytes", 8589934592)
}

// The kill record is the launcher's line, and the daemon's line of each kill is read beside it: the
// two are matched kill by kill on the pod, the role, the pid and the time, and each kill one of them
// lacks is named (plan t30). The window's launcher records a kill of ACME-1's implementer, pid 100,
// at 10:00, which the daemon's line names two seconds later, and one of ACME-2's tester, pid 200,
// at 10:20, which no line of the daemon's names; the daemon's lines name a kill of ACME-3's
// reviewer, pid 300, at 10:40, which the launcher's do not, and a kill by the pod's kernel at 11:00,
// no kill of the memory guard's. The launcher's other lines of the guard, its start and a rise it
// ended nothing for, are no kills. The matcher must count one kill both hold and name the two one
// lacks, and the window must score as it does without the daemon's kill lines, which move nothing
// of the trace.
func TestTheKillMatcherNamesEachKillOneRecordLacks(t *testing.T) {
	write := func(kills bool) Window {
		b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
		b.boot(clock(9, 0))
		promotedRoot(b, clock(9, 5), "ACME-1", "a1")
		b.window.Launcher = []LauncherLine{
			{Time: clock(9, 0), Pod: "legion-acme-acme-1", Text: "launcher: memory guard: started act_percent=85 floor_percent=10 wait=5s interval=100ms limit=8589934592 reading=23052288"},
			{Time: clock(10, 0), Pod: "legion-acme-acme-1", Text: guardKillText(100, "implementer")},
			{Time: clock(10, 20), Pod: "legion-acme-acme-2", Text: guardKillText(200, "tester")},
			{Time: clock(10, 50), Pod: "legion-acme-acme-2", Text: "launcher: memory guard: stopped for this rise reason=nothing to end reading=7303979008 endable=0 protected=7429586944 unread=0 unheld=-125607936 limit=8589934592 left_out=0"},
		}
		if kills {
			guardKillTold(b, clock(10, 0).Add(2*time.Second), "ACME-1", claim.RoleImplementer, 100, "")
			guardKillTold(b, clock(10, 40), "ACME-3", claim.RoleReviewer, 300, "")
			guardKillTold(b, clock(11, 0), "ACME-4", claim.RoleImplementer, 400, "kernel")
		}
		return b.build()
	}
	with := write(true)
	match, err := MatchKills(with)
	if err != nil {
		t.Fatalf("MatchKills: %v", err)
	}
	want := KillMatch{
		Both:         1,
		LauncherOnly: []GuardKill{{At: clock(10, 20), Pod: "legion-acme-acme-2", Role: string(claim.RoleTester), PID: 200}},
		DaemonOnly:   []GuardKill{{At: clock(10, 40), Pod: "legion-acme-acme-3", Role: string(claim.RoleReviewer), PID: 300}},
	}
	if !reflect.DeepEqual(match, want) {
		t.Errorf("MatchKills = %+v, want %+v", match, want)
	}
	scored, err := Score(with)
	if err != nil {
		t.Fatalf("Score of the window with the daemon's kill lines: %v", err)
	}
	without, err := Score(write(false))
	if err != nil {
		t.Fatalf("Score of the window without them: %v", err)
	}
	if !reflect.DeepEqual(scored, without) {
		t.Errorf("the daemon's kill lines move the window's scorecard: %+v, without them %+v", scored, without)
	}
}
