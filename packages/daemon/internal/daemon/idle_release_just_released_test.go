package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// idleRigStopLine is the line the daemon logs once the listener deleted the registration of a
// role whose process ended (sessionStop.send), beside the session it deleted.
const idleRigStopLine = "the Envoy session of a role whose process is gone was stopped"

// idleRigWakeJustReleased drives three of the pods whose sessions end as a released Oh My Pi's
// does in production (idleRigPlan.ExitsAsReleasedInProduction), one for each message a sender
// sends such a pod: nothing of the process deletes its registration, so the daemon's clean stop at
// the release is what makes the session gone, and a message to it then wakes its pod. Each
// message has a pod of its own, because the daemon takes one wake per role while its pod comes
// back (store.TakePodWake).
//
// Steps 1 to 3 fail without stopping, so a run where the session still reads live says what each
// sender met before the wait for a resume that never comes ends the subtest.
func idleRigWakeJustReleased(t *testing.T, rig *idleWakeRig) {
	t.Helper()
	agent, role, routed := rig.issue(idleRigJustReleasedAgent), rig.issue(idleRigJustReleasedRole), rig.issue(idleRigJustReleasedRouted)
	agentSession, roleSession, routedSession := rig.session(agent), rig.session(role), rig.session(routed)
	token := string(rig.token(role))

	// 1. The release's stop of each process sent the clean stop of its session, and the
	//    listener deleted the registration. The stop runs on a goroutine of its own once the
	//    machine's decision is made (cleanSessionStop), so the lines are waited for. Each poll
	//    reads the daemon's log once, collecting every session it logged that line for, and a
	//    failure names the sessions the last read was missing.
	released := []struct {
		issue   idleRigIssue
		session string
	}{{agent, agentSession}, {role, roleSession}, {routed, routedSession}}
	var stopped map[string]bool
	testwait.Holds(testwait.Bounded(t, idleRigRecordWait, idleRigDeadlineMargin), idleRigPoll, func() bool {
		stopped = map[string]bool{}
		for _, entry := range rig.loggedEntries(idleRigStopLine) {
			stopped[fmt.Sprint(entry["session"])] = true
		}
		for _, pod := range released {
			if !stopped[pod.session] {
				return false
			}
		}
		return true
	})
	for _, pod := range released {
		if !stopped[pod.session] {
			t.Errorf("the daemon logged no %q for %s's session %s: the release's stop of its process did not delete its registration",
				idleRigStopLine, pod.issue.key, pod.session)
		}
	}

	// 2. A direct message to the agent pod's session: the registry holds no entry of it, and the
	//    listener's known-session store still holds its record, so the send is stored on the
	//    session's agent subject, answered 200 stopped and gone, and reports the wake beside that
	//    answer (idleRigIsStoredForGone). Before the stop, the entry still stood: the send was
	//    answered for a live session and no wake was reported.
	const agentMessage, agentKey = "a direct message to a session just released", "just-released-agent-1"
	status, body := rig.listener.send(map[string]any{
		"target_session": agentSession, "message": agentMessage,
		"source": "agent", "source_session": idleRigSender, "idempotency_key": agentKey,
	})
	if !idleRigIsStoredForGone(status, body) {
		t.Errorf("the send to %s answered %d %s, want 200 stored with state %s, recipient_state %s and the record's last_seen: the release's stop deleted its registration",
			agentSession, status, body, idleRigSendStopped, idleRigRecipientGone)
	}

	// 3. A role message to the role pod, from a sender that sets no keep flag. Nothing of the
	//    release frees the role's claim, so the claim still names the session whose registration
	//    the stop deleted, a holder that is gone: the listener keeps the message on the role's
	//    kept lane for the role's next live holder and answers 202 kept for holder_lapsed, naming
	//    that session, its state and when the copy's hour ends (dispatch://INFRA-2846's R4), and
	//    reports the wake beside that answer. Before the stop, the session still read live: the
	//    message was delivered to it and no wake was reported.
	const roleMessage, roleKey = "a role message to a role just released", "just-released-role-1"
	status, body = rig.listener.publish(map[string]any{
		"topic": notify.RoleTopicPrefix + token, "message": roleMessage,
		"source": "agent", "source_session": idleRigSender, "idempotency_key": roleKey,
	})
	var answer idleRigRoleAnswer
	decoded := json.Unmarshal([]byte(body), &answer) == nil
	if !decoded || status != http.StatusAccepted || !answer.Kept || answer.KeptReason != idleRigRoleLapsed ||
		answer.Holder != roleSession || answer.HolderState != idleRigRecipientGone || answer.KeptUntil <= time.Now().UnixMilli() {
		t.Errorf("the role publish to %s answered %d %s, want 202 kept for %s, naming its holder %s with holder_state %s and kept_until ahead of now: the role's claim still names the session the release's stop deleted",
			token, status, body, idleRigRoleLapsed, roleSession, idleRigRecipientGone)
	}

	// 4. A message routed to the routed pod's session as Dispatch's outbox routes a person's
	//    answer to a claimed issue's session: published straight onto the stream on the session's
	//    agent subject, with no send route to refuse it. The session's interest row still names
	//    the rig's listener, which holds the copy for its unplaceable window and then gives it up
	//    and reports the wake.
	const routedMessage = "a person's answer routed to a session just released"
	rig.publish(idleRigAgentSubjectPrefix+routedSession, "dispatch", "just-released-routed-1", routedMessage)

	// 5. Each pod comes back for its own message, and its role's task names that wake. The
	//    routed pod's wake comes after the listener's window, which is well inside the pod wait.
	wakes := []struct {
		issue        idleRigIssue
		kind, detail string
	}{
		{agent, pods.WakeAgentMessage, agentSession},
		{role, pods.WakeRoleMessage, token},
		{routed, pods.WakeAgentMessage, routedSession},
	}
	for _, wake := range wakes {
		rig.awaitResumed(wake.issue, 2)
	}
	for _, wake := range wakes {
		rig.awaitTask(wake.issue, workflow.WakeTask(wake.kind, wake.detail))
		if resumes := rig.resumesOf(wake.issue); len(resumes) != 1 || resumes[0].Kind != wake.kind {
			t.Errorf("the resumes of %s are recorded as %+v, want one, of kind %q", wake.issue.key, resumes, wake.kind)
		}
	}

	// 6. The direct message was stored when it was sent, so it reaches the woken session once,
	//    from the session's own consumer, and its sender has no second attempt to make. The role
	//    message was kept for the role's next live holder, so the woken session's claim of the
	//    role is what hands it over (drainKeptRole): it reaches that session once, and its sender
	//    has no second attempt to make either.
	idleRigDeliveredOnce(t, rig, agent, agentMessage)
	idleRigDeliveredOnce(t, rig, role, roleMessage)

	// 7. The routed copy waited in the stream for the session's consumer, which the woken
	//    session reads again: it is given the message once, and nobody sent it a second time.
	idleRigDeliveredOnce(t, rig, routed, routedMessage)
}

// idleRigOutboxWakesJustReleased drives the fourth of those pods, the one the daemon's own
// messages are for: the release's stop of its process is what deletes its registration, so its
// session reads gone, and the daemon then queues its role a phase-ended message and a hook
// prompt, each a row of the role-message lane, as a conflict and a hook's firing queue them. No
// sender tries either again. The first row's first attempt wakes the pod (wakeForDelivery) and
// waits on its backoff, and each row's first attempt after that resume has launched publishes,
// with keep, while the woken session has not claimed the role yet: its process is held before it
// registers with the listener (idleRigPlan.HoldsBeforeEnvoy), as a woken Oh My Pi is still
// starting then. No live session holds the role, so the listener keeps each message on the role's
// kept lane — holder_lapsed while the released session's claim stands, unclaimed once the
// listener's reaper freed it — and each row ends. Once the process goes on, its registration
// under the claim that still names its session, or its claim of the role, hands both to the woken
// session (cybermonklabs/legion#4079). A row that published with keep: false would meet the 404
// and end undelivered, and the woken session would never be given its message.
func idleRigOutboxWakesJustReleased(t *testing.T, rig *idleWakeRig) {
	t.Helper()
	issue := rig.issue(idleRigJustReleasedOutbox)
	session := rig.session(issue)
	registered := rig.agents.role(issue.key, string(issue.role)).Registered

	// 1. The release's stop of the process sent the clean stop of its session, and the listener
	//    deleted the registration. The stop runs on a goroutine of its own once the machine's
	//    decision is made (cleanSessionStop), so the line is waited for.
	idleRigEventually(t, idleRigRecordWait, "the daemon's stop of "+issue.key+"'s session "+session, func() bool {
		return rig.logged(idleRigStopLine, map[string]string{"session": session}) >= 1
	})

	// 2. The woken process will hold before Envoy, and the two rows are written through the
	//    record store the daemon writes them through, each for the issue's implementer, the role
	//    its pod runs: the end of the implementer's retro phase and the prompt of a hook that fired
	//    on a move back from reviewing.
	rig.agents.setPlan(issue.key, string(issue.role), idleRigPlan{TakeTurn: true, ExitsAsReleasedInProduction: true, HoldsBeforeEnvoy: true})
	ended := record.PhaseEnded{Role: issue.role, From: phase.Retro, To: phase.Implementing,
		Reason: "the head conflicts with main: merge main forward"}
	hook := record.HookPrompt{Hook: "back-to-implementing", Role: issue.role, From: phase.Reviewing, To: phase.Implementing,
		Count: 1, Counts: map[string]int{"reviewing-implementing": 1}, Prompt: "Read the reviewer's thread before you write anything."}
	now := time.Now().UTC()
	enqueueOutbox(t, rig.pool, rig.records, mustOutboxRow(t, issue.key, ended, now))
	enqueueOutbox(t, rig.pool, rig.records, mustOutboxRow(t, issue.key, hook, now))

	// 3. The woken process registers with the daemon and holds, and both rows end while it does:
	//    each publish meets a role no live session holds.
	idleRigEventually(t, idleRigPodWait, "the woken process of "+issue.key+" registered with the daemon", func() bool {
		return rig.agents.role(issue.key, string(issue.role)).Registered > registered
	})
	idleRigEventually(t, idleRigRecordWait, "the phase-ended and hook-prompt rows of "+issue.key+" finished", func() bool {
		var left int
		if err := rig.pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = any($2)`,
			issue.key, []string{string(record.OutboxKindPhaseEnded), string(record.OutboxKindHookPrompt)}).Scan(&left); err != nil {
			t.Fatalf("read the outbox rows of %s: %v", issue.key, err)
		}
		return left == 0
	})
	rig.agents.setPlan(issue.key, string(issue.role), idleRigPlan{TakeTurn: true, ExitsAsReleasedInProduction: true})

	// 4. The pod comes back once. The first row's wake is the one resume: a later attempt of
	//    either row finds the claim no longer released and wakes nothing, and the listener's wake
	//    report beside each kept publish meets the resume that wake opened.
	rig.awaitResumed(issue, registered+1)
	if resumes := rig.resumesOf(issue); len(resumes) != 1 || resumes[0].Kind != pods.WakeDelivery {
		t.Errorf("the resumes of %s are recorded as %+v, want one, of kind %q", issue.key, resumes, pods.WakeDelivery)
	}

	// 5. Each message was kept for the role's next live holder, so the woken session's
	//    registration, or its claim of the role, is what hands it over (drainKeptRole): each
	//    reaches that session once, and nothing sends either again. The daemon publishes a role
	//    message's text as its payload too, which the listener carries as a JSON document
	//    (notify's Publish), and the session records that payload, so it is the text's JSON
	//    encoding that is counted.
	idleRigDeliveredOnce(t, rig, issue, idleRigPublishedText(t, ended.Message(issue.key)))
	idleRigDeliveredOnce(t, rig, issue, idleRigPublishedText(t, hook.Message(issue.key)))
}

// idleRigPublishedText is text as a role session records a message the daemon published: the
// payload it sent, text encoded as a JSON document.
func idleRigPublishedText(t *testing.T, text string) string {
	t.Helper()
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("encode %q: %v", text, err)
	}
	return string(encoded)
}
