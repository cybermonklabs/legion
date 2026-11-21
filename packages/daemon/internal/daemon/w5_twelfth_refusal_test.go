package daemon

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// twelfthAnswer is the daemon's answer to the relaunch of w5's twelfth claim as the check reads
// it: its status, code and body, when the request was sent (sent) and when the answer arrived
// (answered), and the times the run saw what the guard may name: each step the twelfth's worker
// stream carried, as the rig's relay read it (steps), and each activity report of the twelfth's
// turn, as the rig's front passed it on (reports).
type twelfthAnswer struct {
	status         int
	code, body     string
	sent, answered time.Time
	steps, reports []time.Time
}

// seenWithin is how long after the rig saw a frame or a report the daemon may stamp it: the relay
// stamps a frame when it reads it and the front stamps a report when it reaches it, and the daemon
// stamps each later, once it has read it in turn.
const seenWithin = 2 * time.Second

// twelfthRefusedInItsTurn is whether the daemon's answer to the relaunch of w5's twelfth claim
// refuses it as a claim whose process still runs its turn, as the check's Expected says. That
// answer is 409 RELAUNCH_GUARD for one of the two rules of supervise.RelaunchGuard that read a
// process in its turn, naming a time the run saw.
//
// One rule is the session's activity report in that turn. The other is the step rule, which the
// guard reads first. On an Oh My Pi build whose wait returns, the twelfth's wait is called again at
// most every 5 minutes, as the check says, so its worker stream shows a step and the step rule
// answers. Either counts only for a time the run saw (seenAs). The guard read its clock between
// the request's sending and the answer's arrival, and names the daemon's stamp cut to the second,
// so the named time is at or before the answer, and less than the guard's 20 minutes plus that
// second before the sending. A refusal for a step or a report the run never saw does not pass, and
// neither does any other refusal or a relaunch that went ahead.
func twelfthRefusedInItsTurn(a twelfthAnswer) bool {
	if a.status != http.StatusConflict || a.code != "RELAUNCH_GUARD" {
		return false
	}
	if named, ok := namedAt(a.body, "its session sent an activity report at "); ok && strings.Contains(a.body, "so its process still runs that turn") {
		return a.inWindow(named) && seenAs(named, a.reports)
	}
	if named, ok := namedAt(a.body, "its worker stream showed a step at "); ok {
		return a.inWindow(named) && seenAs(named, a.steps)
	}
	return false
}

// namedAt is the RFC 3339 time body names right after prefix, up to the comma that ends it.
func namedAt(body, prefix string) (time.Time, bool) {
	_, rest, found := strings.Cut(body, prefix)
	if !found {
		return time.Time{}, false
	}
	stamp, _, _ := strings.Cut(rest, ",")
	named, err := time.Parse(time.RFC3339, stamp)
	return named, err == nil
}

// inWindow is whether the guard can have named named while it judged a: at or before the answer's
// arrival, and less than the guard's 20 minutes, plus the second the name cuts, before the
// request's sending.
func (a twelfthAnswer) inWindow(named time.Time) bool {
	return !named.After(a.answered) && a.sent.Sub(named) < supervise.UnreachableAfter+time.Second
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

// TestTheTwelfthsRefusalReadsEitherRuleOfAProcessInItsTurn holds w5's check of the twelfth claim to
// the two answers of the relaunch guard that read a process still in its turn, each naming a time
// the run saw, and to nothing else. The first row is the answer at 4db2269c2c on Oh My Pi
// 18.18.6-sami.20261119-162325, whose wait returns; the activity report's row has the shape of the
// answer at 9c242a3f41 on Oh My Pi 18.16.0, whose wait does not.
func TestTheTwelfthsRefusalReadsEitherRuleOfAProcessInItsTurn(t *testing.T) {
	at := func(h, m, s, ms int) time.Time { return time.Date(2026, 11, 20, h, m, s, ms*1_000_000, time.UTC) }
	sent, answered := at(13, 52, 23, 756), at(13, 52, 24, 900)
	steps := []time.Time{at(13, 43, 1, 101), at(13, 48, 1, 172), at(13, 48, 1, 200)}
	reports := []time.Time{at(13, 46, 3, 317), at(13, 51, 3, 317)}
	refusal := func(reason string) string {
		return `{"code":"RELAUNCH_GUARD","error":"relaunch refused: the relaunch guard refuses the claim: ` + reason + `"}`
	}
	step := func(named string) string {
		return refusal("its worker stream showed a step at " + named + ", less than 20 minutes ago")
	}
	report := func(named string) string {
		return refusal("its session sent an activity report at " + named + ", less than 20 minutes ago, in the turn it began at 2026-11-20T13:26:07Z, so its process still runs that turn")
	}
	cases := []struct {
		name           string
		status         int
		code, body     string
		steps, reports []time.Time
		want           bool
	}{
		{"the step rule, for a step the run saw", http.StatusConflict, "RELAUNCH_GUARD", step("2026-11-20T13:48:01Z"), steps, reports, true},
		{"the step rule, for a step the relay read at the end of the second before the one the daemon stamped",
			http.StatusConflict, "RELAUNCH_GUARD", step("2026-11-20T13:48:02Z"), []time.Time{at(13, 48, 1, 998)}, reports, true},
		{"the step rule, for a step stamped while the request was on its way",
			http.StatusConflict, "RELAUNCH_GUARD", step("2026-11-20T13:52:24Z"), append(slices.Clone(steps), at(13, 52, 24, 100)), reports, true},
		{"the activity report in its turn, a report the run saw", http.StatusConflict, "RELAUNCH_GUARD", report("2026-11-20T13:51:03Z"), steps, reports, true},
		{"the activity report in its turn, a report the run never saw", http.StatusConflict, "RELAUNCH_GUARD", report("2026-11-20T13:50:03Z"), steps, reports, false},
		{"the step rule, for a step the run never saw", http.StatusConflict, "RELAUNCH_GUARD", step("2026-11-20T13:48:01Z"), steps[:1], reports, false},
		{"the step rule, for a step more than 20 minutes before the request", http.StatusConflict, "RELAUNCH_GUARD",
			step("2026-11-20T13:32:22Z"), append(slices.Clone(steps), at(13, 32, 22, 500)), reports, false},
		{"the step rule, for a step after the answer", http.StatusConflict, "RELAUNCH_GUARD",
			step("2026-11-20T13:53:01Z"), append(slices.Clone(steps), at(13, 53, 1, 0)), reports, false},
		{"an open call that is not a wait", http.StatusConflict, "RELAUNCH_GUARD", refusal("its open call is bash (call-7), not a wait"), steps, reports, false},
		{"background work", http.StatusConflict, "RELAUNCH_GUARD",
			refusal("its last activity report, at 2026-11-20T13:51:03Z, shows background work running: bash"), steps, reports, false},
		{"another refusal", http.StatusConflict, "RELAUNCHED_RECENTLY", `{"code":"RELAUNCHED_RECENTLY","error":"relaunch refused"}`, steps, reports, false},
		{"a relaunch that went ahead", http.StatusOK, "", `{}`, steps, reports, false},
	}
	for _, c := range cases {
		answer := twelfthAnswer{status: c.status, code: c.code, body: c.body, sent: sent, answered: answered, steps: c.steps, reports: c.reports}
		if got := twelfthRefusedInItsTurn(answer); got != c.want {
			t.Errorf("%s: twelfthRefusedInItsTurn = %t, want %t", c.name, got, c.want)
		}
	}
}
