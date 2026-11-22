package daemon

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// twelfthHold is the daemon's line that its relaunch guard holds its relaunch of w5's twelfth
// claim, as the check reads it: the reason the line gives ("" where the daemon wrote no such line),
// when it was written (at), and the times the run saw what the guard may name: each step the
// twelfth's worker stream carried, as the rig's relay read it (steps), and each activity report of
// the twelfth's turn, as the rig's front passed it on (reports).
type twelfthHold struct {
	reason         string
	at             time.Time
	steps, reports []time.Time
}

// seenWithin is how long after the rig saw a frame or a report the daemon may stamp it: the relay
// stamps a frame when it reads it and the front stamps a report when it reaches it, and the daemon
// stamps each later, once it has read it in turn.
const seenWithin = 2 * time.Second

// twelfthHeldInItsTurn is whether the daemon's guard line for w5's twelfth claim holds its
// relaunch as a claim whose process still runs its turn, as the check's Expected says: one of the
// two rules of supervise.RelaunchGuard that read a process in its turn, naming a time the run saw.
//
// One rule is the session's activity report in that turn. The other is the step rule, which the
// guard reads first. On an Oh My Pi build whose wait returns, the twelfth's wait is called again at
// most every 5 minutes, as the check says, so its worker stream shows a step and the step rule
// answers. Either counts only for a time the run saw (seenAs). The guard read its clock before the
// line was written, and names the daemon's stamp cut to the second, so the named time is at or
// before the line, and less than the guard's 20 minutes plus that second before it. A hold for a
// step or a report the run never saw does not pass, and neither does any other reason or no line.
func twelfthHeldInItsTurn(h twelfthHold) bool {
	if named, ok := namedAt(h.reason, "its session sent an activity report at "); ok && strings.Contains(h.reason, "so its process still runs that turn") {
		return h.inWindow(named) && seenAs(named, h.reports)
	}
	if named, ok := namedAt(h.reason, "its worker stream showed a step at "); ok {
		return h.inWindow(named) && seenAs(named, h.steps)
	}
	return false
}

// namedAt is the RFC 3339 time reason names right after prefix, up to the comma that ends it.
func namedAt(reason, prefix string) (time.Time, bool) {
	_, rest, found := strings.Cut(reason, prefix)
	if !found {
		return time.Time{}, false
	}
	stamp, _, _ := strings.Cut(rest, ",")
	named, err := time.Parse(time.RFC3339, stamp)
	return named, err == nil
}

// guardWindow is the guard's 20 minutes (supervise.UnreachableAfter), written as a literal so the
// file builds over the base's code, which w5's negative control lays it on.
const guardWindow = 20 * time.Minute

// inWindow is whether the guard can have named named before it wrote h: at or before the line, and
// less than the guard's 20 minutes, plus the second the name cuts, before it.
func (h twelfthHold) inWindow(named time.Time) bool {
	return !named.After(h.at) && h.at.Sub(named) < guardWindow+time.Second
}

// markedInTime is whether a mark at at came 20 to 22 minutes after a session's last registration,
// seen: the last registration a read of the listener's list showed once the rig had blocked the
// session, which no later registration passes. w5 reads its marks with it.
func markedInTime(at, seen time.Time) bool {
	return !seen.IsZero() && at.Sub(seen) >= guardWindow && at.Sub(seen) <= guardWindow+2*time.Minute
}

// TestAMarkIsInTimeOnlyTwentyToTwentyTwoMinutesAfterTheLastRegistration holds w5's mark clauses to
// their bounds: a mark under 20 minutes or more than 22 minutes after the last registration the
// rig read fails, and a mark with no registration read fails too.
func TestAMarkIsInTimeOnlyTwentyToTwentyTwoMinutesAfterTheLastRegistration(t *testing.T) {
	seen := time.Date(2026, 11, 22, 6, 1, 48, 855_000_000, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		seen time.Time
		want bool
	}{
		{"20m54s after the registration, as the daemon marked the implementer", seen.Add(20*time.Minute + 54*time.Second), seen, true},
		{"exactly 20 minutes after the registration", seen.Add(20 * time.Minute), seen, true},
		{"a second under 20 minutes after the registration", seen.Add(20*time.Minute - time.Second), seen, false},
		{"exactly 22 minutes after the registration", seen.Add(22 * time.Minute), seen, true},
		{"a second past 22 minutes after the registration", seen.Add(22*time.Minute + time.Second), seen, false},
		{"23m27s after the registration, which a bound from the block would let through", seen.Add(23*time.Minute + 27*time.Second), seen, false},
		{"no registration read", seen.Add(21 * time.Minute), time.Time{}, false},
	}
	for _, c := range cases {
		if got := markedInTime(c.at, c.seen); got != c.want {
			t.Errorf("%s: markedInTime = %t, want %t", c.name, got, c.want)
		}
	}
}

// seenAs is whether named, a daemon's stamp cut to the second, can be the stamp of one of seen,
// the times the rig saw the same frames or reports: the daemon stamps each at or after the rig did,
// within seenWithin, so named lies from that time cut to the second to seenWithin after it.
func seenAs(named time.Time, seen []time.Time) bool {
	return slices.ContainsFunc(seen, func(at time.Time) bool {
		at = at.UTC()
		return !named.Before(at.Truncate(time.Second)) && named.Before(at.Add(seenWithin))
	})
}

// TestTheTwelfthsHoldReadsEitherRuleOfAProcessInItsTurn holds w5's check of the twelfth claim to
// the two reasons of the relaunch guard that read a process still in its turn, each naming a time
// the run saw, and to nothing else. The step row has the shape of the guard's reason at 4db2269c2c
// on Oh My Pi 18.18.6-sami.20261119-162325, whose wait returns; the activity report's row has the
// shape of its reason at 9c242a3f41 on Oh My Pi 18.16.0, whose wait does not.
func TestTheTwelfthsHoldReadsEitherRuleOfAProcessInItsTurn(t *testing.T) {
	at := func(h, m, s, ms int) time.Time { return time.Date(2026, 11, 20, h, m, s, ms*1_000_000, time.UTC) }
	written := at(13, 52, 24, 900)
	steps := []time.Time{at(13, 43, 1, 101), at(13, 48, 1, 172), at(13, 48, 1, 200)}
	reports := []time.Time{at(13, 46, 3, 317), at(13, 51, 3, 317)}
	held := func(reason string) string { return "the relaunch guard refuses it: " + reason }
	step := func(named string) string {
		return held("its worker stream showed a step at " + named + ", less than 20 minutes ago")
	}
	report := func(named string) string {
		return held("its session sent an activity report at " + named + ", less than 20 minutes ago, in the turn it began at 2026-11-20T13:26:07Z, so its process still runs that turn")
	}
	cases := []struct {
		name           string
		reason         string
		steps, reports []time.Time
		want           bool
	}{
		{"the step rule, for a step the run saw", step("2026-11-20T13:48:01Z"), steps, reports, true},
		{"the step rule, for a step the relay read at the end of the second before the one the daemon stamped",
			step("2026-11-20T13:48:02Z"), []time.Time{at(13, 48, 1, 998)}, reports, true},
		{"the step rule, for a step stamped as the line was written",
			step("2026-11-20T13:52:24Z"), append(slices.Clone(steps), at(13, 52, 24, 100)), reports, true},
		{"the activity report in its turn, a report the run saw", report("2026-11-20T13:51:03Z"), steps, reports, true},
		{"the activity report in its turn, a report the run never saw", report("2026-11-20T13:50:03Z"), steps, reports, false},
		{"the step rule, for a step the run never saw", step("2026-11-20T13:48:01Z"), steps[:1], reports, false},
		{"the step rule, for a step more than 20 minutes before the line",
			step("2026-11-20T13:32:22Z"), append(slices.Clone(steps), at(13, 32, 22, 500)), reports, false},
		{"the step rule, for a step after the line",
			step("2026-11-20T13:53:01Z"), append(slices.Clone(steps), at(13, 53, 1, 0)), reports, false},
		{"an open call that is not a wait", held("its open call is bash (call-7), not a wait"), steps, reports, false},
		{"background work", held("its last activity report, at 2026-11-20T13:51:03Z, shows background work running: bash"), steps, reports, false},
		{"no line: a relaunch that went ahead", "", steps, reports, false},
	}
	for _, c := range cases {
		hold := twelfthHold{reason: c.reason, at: written, steps: c.steps, reports: c.reports}
		if got := twelfthHeldInItsTurn(hold); got != c.want {
			t.Errorf("%s: twelfthHeldInItsTurn = %t, want %t", c.name, got, c.want)
		}
	}
}
