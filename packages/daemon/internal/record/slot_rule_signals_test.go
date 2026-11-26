package record

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// Two rules of the repository admit below P0 by signals, signals and signals-backoff: the daemon's
// own rule with R10's start of a P0 issue outside the cap and a mode of admission (Signals), and no
// other choice moved. The daemon's own rule, the zero rule that names none, the rules the recorded
// windows ran and every other candidate name no mode, so no signal is read for them.
func TestOnlyTheTwoSignalRulesAdmitBySignals(t *testing.T) {
	t.Parallel()
	want := map[string]signals.Mode{"signals": signals.Ceiling, "signals-backoff": signals.Backoff}
	if MainRule.Signals != signals.Off || (SlotRule{}).OrMain().Signals != signals.Off {
		t.Errorf("the daemon's own rule admits by signals (%q), and it reads none", MainRule.Signals)
	}
	for _, rule := range SlotRules() {
		if rule.Signals != want[rule.Name] {
			t.Errorf("the rule %q admits by signals in the mode %q, want %q", rule.Name, rule.Signals, want[rule.Name])
		}
		if _, signalRule := want[rule.Name]; !signalRule {
			continue
		}
		if !rule.P0AboveCap {
			t.Errorf("the rule %q starts no P0 issue outside the cap, and R10 holds under it", rule.Name)
		}
		plain := rule
		plain.Name, plain.P0AboveCap, plain.Signals = MainRule.Name, MainRule.P0AboveCap, MainRule.Signals
		if !reflect.DeepEqual(plain, MainRule) {
			t.Errorf("the rule %q = %+v moves a choice off the daemon's own besides R10's start and its mode: %+v", rule.Name, rule, plain)
		}
	}
}

// SignalsLet is signals.Decide in the rule's own mode, else in the snapshot's: a signal rule admits
// by its own mode whatever the snapshot's, and the daemon's own rule admits by signals only where
// its caller's snapshot names a mode, with MainRule and no other change. A tick starts nothing
// below P0 in either, an event does while no signal that has a limit binds, and the cap is the cap
// where no mode is named.
func TestASignalRuleDecidesInItsOwnModeAndTheDaemonsRuleInTheSnapshots(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.November, 25, 10, 30, 0, 0, time.UTC)
	killsAt := func(value float64) signals.Snapshot {
		return signals.Snapshot{At: now, Readings: map[string]signals.Reading{
			signals.Kills: {Value: signals.Value{Recorded: true, Value: value}, Limit: new(2.0)},
		}}
	}
	binding, under := killsAt(2), killsAt(1)
	moded := func(s signals.Snapshot, mode signals.Mode) signals.Snapshot {
		s.Mode = mode
		return s
	}
	history := func(s signals.Snapshot, at time.Time, setbacks ...signals.Setback) signals.Snapshot {
		s.At, s.Setbacks = at, setbacks
		return s
	}
	refusal := signals.Setback{At: now.Add(-30 * time.Minute), Count: 1}
	kill := signals.Setback{At: now.Add(-25 * time.Minute), Count: 1}
	twoRefusals := signals.Setback{At: refusal.At, Count: 2}
	ceilingRule, backoffRule := keptRule(t, "signals"), keptRule(t, "signals-backoff")
	for _, tc := range []struct {
		name   string
		rule   SlotRule
		kind   signals.PassKind
		s      signals.Snapshot
		capped int
		want   signals.Verdict
	}{
		{"main with a snapshot that names no mode: the cap, at a tick too", MainRule, signals.PassTick, binding, 2, signals.Starts},
		{"the zero rule is the daemon's own", SlotRule{}, signals.PassEvent, binding, 2, signals.Starts},
		{"main with a snapshot that names no mode and a full cap", MainRule, signals.PassEvent, under, 3, signals.HeldByCap},
		{"main in the snapshot's ceiling mode, a signal at its limit, an event", MainRule, signals.PassEvent, moded(binding, signals.Ceiling), 2, signals.HeldBySignal},
		{"main in the snapshot's ceiling mode, every signal under, an event", MainRule, signals.PassEvent, moded(under, signals.Ceiling), 2, signals.Starts},
		{"main in the snapshot's ceiling mode, every signal under, a deadline", MainRule, signals.PassDeadline, moded(under, signals.Ceiling), 2, signals.Starts},
		{"main in the snapshot's ceiling mode, every signal under, a tick", MainRule, signals.PassTick, moded(under, signals.Ceiling), 2, signals.HeldByTick},
		{"signals, a signal at its limit, an event", ceilingRule, signals.PassEvent, binding, 2, signals.HeldBySignal},
		{"signals, every signal under, an event", ceilingRule, signals.PassEvent, under, 2, signals.Starts},
		{"signals, every signal under, a deadline", ceilingRule, signals.PassDeadline, under, 2, signals.Starts},
		{"signals, every signal under, a tick", ceilingRule, signals.PassTick, under, 2, signals.HeldByTick},
		{"signals, the holders at the ceiling", ceilingRule, signals.PassEvent, under, 3, signals.HeldByCap},
		{"signals in its own mode, whatever mode the snapshot names", ceilingRule, signals.PassEvent,
			history(moded(under, signals.Backoff), now, twoRefusals), 2, signals.Starts},
		{"signals-backoff, allowance 3 and a refusal gives 2: the holders at 2", backoffRule, signals.PassEvent, history(under, now, refusal), 2, signals.HeldByCap},
		{"signals-backoff, allowance 2: one holder below it", backoffRule, signals.PassEvent, history(under, now, refusal), 1, signals.Starts},
		{"signals-backoff, allowance 1 and a kill stays 1: one holder", backoffRule, signals.PassEvent, history(under, now, twoRefusals, kill), 1, signals.HeldByCap},
		{"signals-backoff, allowance 1 and a kill stays 1: no holder", backoffRule, signals.PassEvent, history(under, now, twoRefusals, kill), 0, signals.Starts},
		{"signals-backoff, allowance 2 below a ceiling of 3 with nothing for an hour gives 3", backoffRule, signals.PassDeadline,
			history(under, refusal.At.Add(time.Hour), refusal), 2, signals.Starts},
		{"signals-backoff, a signal at its limit", backoffRule, signals.PassEvent, binding, 0, signals.HeldBySignal},
		{"signals-backoff, a tick", backoffRule, signals.PassTick, under, 0, signals.HeldByTick},
	} {
		if got := tc.rule.SignalsLet(tc.kind, tc.s, tc.capped, 3); got != tc.want {
			t.Errorf("%s: %q decides %q for an entry below P0 at a pass of kind %q with %d holders, want %q",
				tc.name, tc.rule.Name, got, tc.kind, tc.capped, tc.want)
		}
	}
}

// Only a rule that keeps the owner's direction of 2026-11-24 can be named best by a comparison of
// rules (the replay's spec, R7), and the rule value says which: R10 above P0, the line's priority
// order below it and admission by signals with the cap as a ceiling only. Of the rules the
// repository keeps, signals and signals-backoff keep it, and no other does: the daemon's own rule
// and before-p0 admit below P0 under the cap, p0-at-once and p0-idle start a P0 issue outside the
// cap and admit below it under the cap, and the recorded windows' rules neither start one outside
// the cap nor put priority first. A signal rule that loses any one of the three choices keeps it no
// more, and the zero rule, the daemon's own, does not keep it.
func TestOnlyTheSignalRulesKeepTheOwnersDirection(t *testing.T) {
	t.Parallel()
	keep := map[string]bool{"signals": true, "signals-backoff": true}
	for _, rule := range SlotRules() {
		if got := rule.KeepsOwnersDirection(); got != keep[rule.Name] {
			t.Errorf("the rule %q keeps the owner's direction: %t, want %t", rule.Name, got, keep[rule.Name])
		}
	}
	if (SlotRule{}).KeepsOwnersDirection() {
		t.Error("the zero rule, the daemon's own, keeps the owner's direction, and it admits below P0 under the cap")
	}
	for _, broken := range []struct {
		name   string
		change func(*SlotRule)
	}{
		{"no P0 outside the cap", func(r *SlotRule) { r.P0AboveCap = false }},
		{"no priority first", func(r *SlotRule) { r.PriorityFirst = false }},
		{"no admission by signals", func(r *SlotRule) { r.Signals = signals.Off }},
	} {
		rule := keptRule(t, "signals")
		broken.change(&rule)
		if rule.KeepsOwnersDirection() {
			t.Errorf("signals with %s keeps the owner's direction", broken.name)
		}
	}
}
