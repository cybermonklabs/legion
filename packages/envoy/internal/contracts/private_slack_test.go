package contracts

import "testing"

// TestSubjectProjectReadsThePrivateSlackLane: a Slack event of a private project's channel and its
// thread copy are named on that project's Legion lane (PrivateSlackSubject,
// PrivateSlackThreadSubject, PrivateSlackTopic), and SubjectProject reads both as the project, so
// the relay decides them as the project's frames. Neither is a notifications.slack. subject.
func TestSubjectProjectReadsThePrivateSlackLane(t *testing.T) {
	channel := PrivateSlackSubject("PRIV", "T1", "C0PRIV00001", "mention")
	thread := PrivateSlackThreadSubject("PRIV", "T1", "C0PRIV00001", "1700000000.000100", "mention")
	for subject, want := range map[string]string{
		channel: "notifications.legion.priv.slack.T1.C0PRIV00001.mention",
		thread:  "notifications.legion.priv.slack.T1.C0PRIV00001.thread.1700000000_000100.mention",
	} {
		if subject != want {
			t.Errorf("the private Slack lane is %q, want %q", subject, want)
		}
		if project, ok := SubjectProject(subject); project != "PRIV" || !ok {
			t.Errorf("SubjectProject(%q) = %q, %v; want PRIV", subject, project, ok)
		}
	}
	for _, slack := range []string{SlackSubject("T1", "C0PRIV00001", "mention"), SlackThreadSubject("T1", "C0PRIV00001", "1700000000.000100", "mention")} {
		moved, ok := PrivateSlackTopic("PRIV", slack)
		if !ok || (moved != channel && moved != thread) {
			t.Errorf("PrivateSlackTopic(PRIV, %q) = %q, %v; want one of PRIV's two lanes", slack, moved, ok)
		}
	}
	if moved, ok := PrivateSlackTopic("PRIV", "notifications.github.acme.pub.push"); ok {
		t.Errorf("PrivateSlackTopic moved a GitHub subject to %q", moved)
	}
}
