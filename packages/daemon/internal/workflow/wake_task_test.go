package workflow

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/pods"
)

// The words a woken role reads: WakeTask for each wake and each stop, and the retells a cancel and
// a release's record make of a wake's start (CancelledWakeTask, PausedWakeTask, ReleasedWakeTask,
// TakenOverWakeTask). These rows read no database. The wakes themselves are in wake_test.go.

// R6: every wake's task names the wake, so the woken agent's first turn reads why its pod came
// back without reading anything else.
func TestWakeTaskNamesItsWake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind, detail, want string
	}{
		{kind: pods.WakeChecks, detail: "head1 failed build", want: "head1 failed build"},
		{kind: pods.WakeRoleMessage, detail: "legion-LEGION-208-implementer", want: "role topic"},
		{kind: pods.WakeAgentMessage, detail: "session-1", want: "sent to your session"},
		{kind: pods.WakeBackstop, detail: "the backstop came due", want: "backstop"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			task := WakeTask(tc.kind, tc.detail, StoppedByRelease)
			if !strings.Contains(task, tc.want) || !strings.Contains(task, "released") {
				t.Fatalf("WakeTask(%s) = %q, want it to name the release and %q", tc.kind, task, tc.want)
			}
		})
	}
}

// Decision 16 of dispatch://INFRA-3759: a wake of a session a project's pause stopped says so.
// The project was paused, its pause stopped the session, and the pod, the workspace and the
// unpushed work are as the session left them; the pod was never released. The lines about the
// message that woke it stay.
func TestTheWakeTaskOfASessionAPauseStoppedSaysThePauseStoppedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind, detail, want string
	}{
		{kind: pods.WakeChecks, detail: "head1 failed build", want: "head1 failed build"},
		{kind: pods.WakeRoleMessage, detail: "legion-LEGION-208-implementer", want: "role topic (legion-LEGION-208-implementer) while your session was stopped. A message that Envoy kept for your role reaches this session, for one hour after Envoy kept it."},
		{kind: pods.WakeAgentMessage, detail: "session-1", want: "sent to your session (session-1) while your session was stopped. A message that Envoy or Dispatch stored for your session reaches this session, for 72 hours after it was stored."},
		{kind: pods.WakeBackstop, detail: "the backstop came due", want: "your backstop running out"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			task := WakeTask(tc.kind, tc.detail, StoppedByPause)
			for _, want := range []string{"Your project was paused, and its pause stopped your session.",
				"Your pod, your workspace and any work you had not pushed are as you left them.", tc.want} {
				if !strings.Contains(task, want) {
					t.Fatalf("WakeTask(%s) after a pause = %q, want it to say %q", tc.kind, task, want)
				}
			}
			if strings.Contains(task, "released") || strings.Contains(task, "pod was gone") {
				t.Fatalf("WakeTask(%s) after a pause = %q, want no word of a release: the pause kept the pod", tc.kind, task)
			}
		})
	}
}

// A woken role's wake text is true (dispatch://INFRA-3589), against decision 12 of
// dispatch://INFRA-3759: a cancel of an idle release made while the project is paused keeps a wake
// that the release let run, and the release took no pod. The kept start's task becomes the task
// WakeTask writes for a session the pause stopped: the same wake and detail under the pause's
// words, with the sentences the queue appended after it, such as a reviewer's review round. A task
// that does not begin with the release's words for that wake is left as it is.
func TestAPausedWakeTaskIsTheTaskOfAWakeOfARoleThePauseStopped(t *testing.T) {
	t.Parallel()
	const appended = " The review round's two sentences follow the wake's task."
	for _, tc := range []struct{ kind, detail string }{
		{kind: pods.WakeChecks, detail: "head1 failed build"},
		{kind: pods.WakeRoleMessage, detail: "legion-LEGION-208-implementer"},
		{kind: pods.WakeAgentMessage, detail: "session-1"},
		{kind: pods.WakeBackstop, detail: "the backstop armed for LEGION-208's implementer came due"},
		{kind: pods.WakeDelivery, detail: "a notice of LEGION-208"},
	} {
		for _, suffix := range []string{"", appended} {
			released := WakeTask(tc.kind, tc.detail, StoppedByRelease) + suffix
			want := WakeTask(tc.kind, tc.detail, StoppedByPause) + suffix
			if got := PausedWakeTask(tc.kind, tc.detail, released); got != want {
				t.Errorf("PausedWakeTask(%s, %q, %q) = %q, want %q", tc.kind, tc.detail, released, got, want)
			}
		}
	}
	for _, task := range []string{"", "Implement LEGION-208.", WakeTask(pods.WakeChecks, "head1 failed build", StoppedByPause)} {
		if got := PausedWakeTask(pods.WakeChecks, "head1 failed build", task); got != task {
			t.Errorf("PausedWakeTask(%s, %q) = %q, want the task as it is: it says nothing of a release", pods.WakeChecks, task, got)
		}
	}
}

// A woken role's wake text is true (dispatch://INFRA-3589, R3): a wake of a role that a project's
// pause stopped, taken while a release that recorded the role still stood, is told the pause's
// words, and the release's record then takes the pod. The record retells that wake's start as
// the task WakeTask writes for a session the release stopped: the same wake and detail under the
// release's words, with the sentences the queue appended after it, such as a reviewer's review
// round. ReleasedWakeTask is the inverse of PausedWakeTask both ways. A task that does not begin
// with the pause's words for that wake is left as it is.
func TestAReleasedWakeTaskIsTheInverseOfAPausedWakeTask(t *testing.T) {
	t.Parallel()
	const appended = " The review round's two sentences follow the wake's task."
	for _, tc := range []struct{ kind, detail string }{
		{kind: pods.WakeChecks, detail: "head1 failed build"},
		{kind: pods.WakeRoleMessage, detail: "legion-LEGION-208-implementer"},
		{kind: pods.WakeAgentMessage, detail: "session-1"},
		{kind: pods.WakeBackstop, detail: "the backstop armed for LEGION-208's implementer came due"},
		{kind: pods.WakeDelivery, detail: "a notice of LEGION-208"},
	} {
		for _, suffix := range []string{"", appended} {
			released := WakeTask(tc.kind, tc.detail, StoppedByRelease) + suffix
			paused := WakeTask(tc.kind, tc.detail, StoppedByPause) + suffix
			if got := ReleasedWakeTask(tc.kind, tc.detail, paused); got != released {
				t.Errorf("ReleasedWakeTask(%s, %q, %q) = %q, want %q", tc.kind, tc.detail, paused, got, released)
			}
			if got := ReleasedWakeTask(tc.kind, tc.detail, PausedWakeTask(tc.kind, tc.detail, released)); got != released {
				t.Errorf("ReleasedWakeTask(%s, PausedWakeTask(%s, %q)) = %q, want the task as it was", tc.kind, tc.kind, released, got)
			}
			if got := PausedWakeTask(tc.kind, tc.detail, ReleasedWakeTask(tc.kind, tc.detail, paused)); got != paused {
				t.Errorf("PausedWakeTask(%s, ReleasedWakeTask(%s, %q)) = %q, want the task as it was", tc.kind, tc.kind, paused, got)
			}
		}
	}
	for _, task := range []string{"", "Implement LEGION-208.", WakeTask(pods.WakeChecks, "head1 failed build", StoppedByRelease)} {
		if got := ReleasedWakeTask(pods.WakeChecks, "head1 failed build", task); got != task {
			t.Errorf("ReleasedWakeTask(%s, %q) = %q, want the task as it is: it says nothing of a pause", pods.WakeChecks, task, got)
		}
	}
}

// A woken role's wake text is true (dispatch://INFRA-3589, R3): a cancel of an idle release keeps
// the wake's start of a role whose claim a close, a linger or an operator took over, and the role
// reads that start's task when the operator resumes it. The release took no pod. So the task
// becomes the same wake and detail under a cancelled release's words: its preamble says that the
// release did not finish and the role goes on in the same pod, a message wake says the message
// came while the session was stopped, and the wake's own sentences and those the queue appended
// stay. A task that does not begin with the release's words for that wake is left as it is.
func TestATakenOverWakeTaskIsTheTaskOfAWakeOfACancelledRelease(t *testing.T) {
	t.Parallel()
	const appended = " The review round's two sentences follow the wake's task."
	const cancelled = "An idle release of your issue's pod stopped your role, and that release did not finish, so your role goes on in the same pod. " +
		"Your session, your workspace and any work you had not pushed are as you left them. "
	for _, tc := range []struct {
		kind, detail, says string
		// away is whether the wake's sentence names the time the role was away (releaseAway).
		away bool
	}{
		{kind: pods.WakeChecks, detail: "head1 failed build", says: "The wake is a CI settlement on your pull request's head: head1 failed build."},
		{kind: pods.WakeRoleMessage, detail: "legion-LEGION-208-implementer", away: true,
			says: "role topic (legion-LEGION-208-implementer) while your session was stopped. A message that Envoy kept for your role reaches this session"},
		{kind: pods.WakeAgentMessage, detail: "session-1", away: true,
			says: "sent to your session (session-1) while your session was stopped. A message that Envoy or Dispatch stored for your session reaches this session"},
		{kind: pods.WakeBackstop, detail: "the backstop armed for LEGION-208's implementer came due",
			says: "The wake is the release's backstop running out: the backstop armed for LEGION-208's implementer came due."},
		{kind: pods.WakeDelivery, detail: "a notice of LEGION-208", says: "The wake is delivery: a notice of LEGION-208."},
	} {
		for _, suffix := range []string{"", appended} {
			released := WakeTask(tc.kind, tc.detail, StoppedByRelease) + suffix
			// The control: the task the retell is given says that the pod went away, in the words
			// the retell must not keep.
			if !strings.HasPrefix(released, wakePreamble) || tc.away != strings.Contains(released, releaseAway) {
				t.Fatalf("control: WakeTask(%s, %q, StoppedByRelease) = %q, want it to begin %q, and to name %q only for a message wake (here %t)",
					tc.kind, tc.detail, released, wakePreamble, releaseAway, tc.away)
			}
			got := TakenOverWakeTask(tc.kind, tc.detail, released)
			if !strings.HasPrefix(got, cancelled) || !strings.Contains(got, tc.says) || !strings.HasSuffix(got, suffix) ||
				strings.Contains(got, wakePreamble) || strings.Contains(got, releaseAway) {
				t.Errorf("TakenOverWakeTask(%s, %q, %q) = %q, want it to begin %q, say %q, end %q, and hold neither %q nor %q",
					tc.kind, tc.detail, released, got, cancelled, tc.says, suffix, wakePreamble, releaseAway)
			}
		}
	}
	for _, task := range []string{"", "Implement LEGION-208.", WakeTask(pods.WakeChecks, "head1 failed build", StoppedByPause)} {
		if got := TakenOverWakeTask(pods.WakeChecks, "head1 failed build", task); got != task {
			t.Errorf("TakenOverWakeTask(%s, %q) = %q, want the task as it is: it says nothing of a release that took the pod", pods.WakeChecks, task, got)
		}
	}
}

// R3 of dispatch://INFRA-3589: a wake that comes during a release is applied again, never dropped
// in silence. A CI settlement and a backstop reach the woken role only through the wake's task,
// so a cancel that drops such a wake hands its task to the role's resume. A wake of a role that a
// project's pause stopped, taken while the release stood, is told the pause's words, which say
// that the pod was kept. The cancel kept the pod, so the resume carries that task as it is, with
// the sentences the queue appended after it. A message wake still answers "": its message reaches
// the session anyway.
func TestACancelledWakeTaskKeepsThePausesWordsOfASettlementOrABackstop(t *testing.T) {
	t.Parallel()
	const appended = " The review round's two sentences follow the wake's task."
	for _, tc := range []struct{ kind, detail string }{
		{kind: pods.WakeChecks, detail: "head1 failed build"},
		{kind: pods.WakeBackstop, detail: "the backstop armed for LEGION-208's implementer came due"},
	} {
		for _, suffix := range []string{"", appended} {
			paused := WakeTask(tc.kind, tc.detail, StoppedByPause) + suffix
			if got := CancelledWakeTask(tc.kind, tc.detail, paused); got != paused {
				t.Errorf("CancelledWakeTask(%s, %q, %q) = %q, want the task as it is: the cancel kept the pod the pause kept", tc.kind, tc.detail, paused, got)
			}
			released := WakeTask(tc.kind, tc.detail, StoppedByRelease) + suffix
			if got, want := CancelledWakeTask(tc.kind, tc.detail, released), WakeTask(tc.kind, tc.detail, stoppedByCancelledRelease)+suffix; got != want {
				t.Errorf("CancelledWakeTask(%s, %q, %q) = %q, want %q", tc.kind, tc.detail, released, got, want)
			}
		}
	}
	for _, tc := range []struct{ kind, detail string }{
		{kind: pods.WakeRoleMessage, detail: "legion-LEGION-208-implementer"},
		{kind: pods.WakeAgentMessage, detail: "session-1"},
	} {
		for _, stop := range []WakeStop{StoppedByRelease, StoppedByPause} {
			if task := WakeTask(tc.kind, tc.detail, stop); CancelledWakeTask(tc.kind, tc.detail, task) != "" {
				t.Errorf("CancelledWakeTask(%s, %q) = %q, want none: the message reaches the session anyway", tc.kind, task, CancelledWakeTask(tc.kind, tc.detail, task))
			}
		}
	}
	if got := CancelledWakeTask(pods.WakeChecks, "head1 failed build", "Implement LEGION-208."); got != "" {
		t.Errorf("CancelledWakeTask(%s, a task no wake wrote) = %q, want none", pods.WakeChecks, got)
	}
	if got := CancelledWakeTask(pods.WakeChecks, "head2 failed lint", WakeTask(pods.WakeChecks, "head1 failed build", StoppedByRelease)); got != "" {
		t.Errorf("CancelledWakeTask(%s, a detail its task does not name) = %q, want none: the task is no task of that wake", pods.WakeChecks, got)
	}
}

// R4 of dispatch://INFRA-3589, the woken role's wake text is true: every retell renders WakeTask
// again from the wake's kind and detail (retellWakeTask) and cuts no words out of the sentence.
// A detail is whatever the wake names, a role token, a session, a head's settlement, and here it
// holds the very words a retell of a message wake changes, ") while your pod was gone. ". A retell
// that replaced the first such words in the task would change the detail and leave the sentence's
// own words. Rendered again, the detail stays as it was and the sentence is told in the new words.
// Each kind of WakeTask, a kind still to come among them (its default), is told so by every
// retell, with what the queue appended after it kept.
func TestARetellRendersTheWakeAgainFromItsKindAndDetail(t *testing.T) {
	t.Parallel()
	const appended = " The review round's two sentences follow the wake's task."
	const tricky = "legion-LEGION-208-implementer) while your pod was gone. (and more"
	for _, kind := range []string{pods.WakeChecks, pods.WakeRoleMessage, pods.WakeAgentMessage, pods.WakeBackstop, pods.WakeDelivery} {
		for _, tc := range []struct {
			name     string
			from, to WakeStop
			retell   func(kind, detail, task string) string
		}{
			{name: "PausedWakeTask", from: StoppedByRelease, to: StoppedByPause, retell: PausedWakeTask},
			{name: "ReleasedWakeTask", from: StoppedByPause, to: StoppedByRelease, retell: ReleasedWakeTask},
			{name: "TakenOverWakeTask", from: StoppedByRelease, to: stoppedByCancelledRelease, retell: TakenOverWakeTask},
		} {
			task := WakeTask(kind, tricky, tc.from) + appended
			want := WakeTask(kind, tricky, tc.to) + appended
			if got := tc.retell(kind, tricky, task); got != want {
				t.Errorf("%s(%s, %q, %q) = %q, want %q: the detail stays and the sentence is told in the new words",
					tc.name, kind, tricky, task, got, want)
			}
		}
	}
}

// A role message wake tells the woken role what Envoy kept for it (dispatch://INFRA-2846/spec
// R7): a message kept for the role reaches the woken session for one hour after the keep, as an
// Envoy card that shows kept_at, possibly before the wake's task; when that hour ends before a
// session takes it, Envoy drops it and tells its sender; and a message sent with keep: false is
// not kept, so its sender must send it again. Each clause is matched on its own, so a sentence
// that drops one fails and names it. The task says the same whether an idle release or a
// project's pause stopped the session.
func TestTheRoleMessageWakeTaskSaysWhatWasKept(t *testing.T) {
	t.Parallel()
	for _, stop := range []WakeStop{StoppedByRelease, StoppedByPause} {
		task := WakeTask(pods.WakeRoleMessage, "legion-LEGION-208-implementer", stop)
		for _, clause := range []struct{ rule, want string }{
			{rule: "a message kept for the role reaches the woken session as a card", want: "A message that Envoy kept for your role reaches this session, for one hour after Envoy kept it. It comes as an Envoy card"},
			{rule: "the card shows kept_at", want: "shows kept_at"},
			{rule: "the card can arrive before the task", want: "possibly before this task"},
			{rule: "a copy whose hour ended before a session took it does not reach the role", want: "When that hour ends before a session takes the message, Envoy drops it"},
			{rule: "Envoy keeps a role message for one hour", want: "for one hour after Envoy kept it"},
			{rule: "Envoy tells the sender when it drops one", want: "Envoy drops it and tells its sender"},
			{rule: "a message sent with keep: false is not kept", want: "A message that Envoy did not keep, such as one sent with keep: false, does not reach you"},
			{rule: "the sender of such a message sends it again", want: "its sender must send it again"},
		} {
			if !strings.Contains(task, clause.want) {
				t.Errorf("WakeTask(%s, stop %d) = %q, want it to say %s (%q)", pods.WakeRoleMessage, stop, task, clause.rule, clause.want)
			}
		}
		if strings.Contains(task, "which nothing kept") {
			t.Errorf("WakeTask(%s, stop %d) = %q, want no %q: Envoy keeps a role message for the role's next holder", pods.WakeRoleMessage, stop, task, "which nothing kept")
		}
	}
}
