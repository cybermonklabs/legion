package rendering

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The reconnect-hooks-stalled alarm's description, the longest, stays within CloudWatch's 1,024
// characters for the longest log group name the listener renders, and so does every other alarm's.
// The listener renders any name CheckListenerLogGroup takes; before it, it rendered any name
// CloudWatch takes, up to 512 characters, for which this description was 1,421 characters and the
// apply's alarm create would fail. The control is the longest taken name padded by one character,
// which pads that description past the limit and is refused, naming the alarm.
func TestTheStalledHooksAlarmDescriptionFitsCloudWatchForTheLongestLogGroup(t *testing.T) {
	var stalled LogSignal
	for _, s := range ListenerLogSignals {
		if s.alarm == "reconnect-hooks-stalled" {
			stalled = s
		}
	}
	longest := 0
	for n := 1; n <= 512 && CheckListenerLogGroup(strings.Repeat("g", n)) == nil; n++ {
		longest = n
	}
	if longest < len("/ecs/envoy-listener") {
		t.Fatalf("the listener takes log group names of at most %d characters, shorter than the deployed /ecs/envoy-listener", longest)
	}
	name := strings.Repeat("g", longest)
	for _, s := range ListenerLogSignals {
		if n := utf8.RuneCountInString(s.Description(name)); n > MaxAlarmDescription {
			t.Fatalf("the %s description for the longest log group name the listener takes, %d characters, is %d characters, past CloudWatch's %d", s.alarm, longest, n, MaxAlarmDescription)
		}
	}
	padded := name + "g"
	if n := utf8.RuneCountInString(stalled.Description(padded)); n <= MaxAlarmDescription {
		t.Fatalf("control: the reconnect-hooks-stalled description for a %d-character name is %d characters, not past %d", len(padded), n, MaxAlarmDescription)
	}
	if err := CheckListenerLogGroup(padded); err == nil || !strings.Contains(err.Error(), "reconnect-hooks-stalled") {
		t.Fatalf("control: CheckListenerLogGroup of a %d-character name = %v; want a refusal naming the reconnect-hooks-stalled alarm", len(padded), err)
	}
}
