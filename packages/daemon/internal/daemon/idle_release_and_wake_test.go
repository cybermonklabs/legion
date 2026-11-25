package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/roleholder"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The issues of the rig, one pod each. Each subtest drives the pods it names and no other, so one
// release wave readies them all and no subtest waits for another's.
const (
	idleRigDelivery   = "delivery"
	idleRigChecks     = "checks"
	idleRigRoleWake   = "role-message"
	idleRigAgentWake  = "agent-message"
	idleRigBackstop   = "backstop"
	idleRigKeepNone   = "keeps-nothing"
	idleRigSubscribed = "subscribed-topic"
	idleRigStuck      = "stuck-resume"
	idleRigRestart    = "daemon-restart"
	idleRigLapsed     = "lapsed-holder"
	idleRigFreed      = "freed-claim"
	// The four pods whose sessions end as a released Oh My Pi's does in production
	// (idleRigPlan.ExitsAsReleasedInProduction): one per message a sender sends that wakes such a
	// pod, and one for the daemon's own hook prompts.
	idleRigJustReleasedAgent  = "just-released-agent"
	idleRigJustReleasedRole   = "just-released-role"
	idleRigJustReleasedRouted = "just-released-routed"
	idleRigJustReleasedOutbox = "just-released-outbox"
)

// idleRigSubscribedTopic is the topic the subscribed pod's session takes with envoy_subscribe: a
// team topic, which is neither a role topic nor an agent subject. It is a team topic because the
// team namespace is the one an agent names a topic of its own under that the notification stream
// captures, and a session is delivered only what its own consumer holds (dispatch://INFRA-771):
// the listener refuses a publish to a notification topic no stream subject captures, by name, so
// a topic of this test's own invention would answer 400 here rather than reach the session.
const idleRigSubscribedTopic = "notifications.team.idlerig-watched"

// idleRigSender is the session every message of this test is sent from.
const idleRigSender = "idle-rig-sender"

// TestIdleReleaseAndWake is INFRA-945's A3 acceptance rig: the Go daemon on a real Postgres, a
// real nats-server, a real Envoy listener and the Sandbox runtime on a fake Kubernetes API that
// starts a real `legion launcher` for each pod it reports Ready, whose role processes are
// scripted stand-ins for Oh My Pi that register, claim their role and record each message they
// get. idle_rig_test.go says where each part comes from.
//
// Every pod of the rig is launched and made quiet, and one pass of the daemon's own idle tick
// releases them all: each live claim suspended with the source `released`, and the pod, its disk
// and its Sandbox deleted. Each subtest then drives its own pods' wakes.
func TestIdleReleaseAndWake(t *testing.T) {
	t.Parallel()
	// The rig is given every plan that sets a field a role process reads as it starts (idleRigPlan):
	// the topic the subscribed pod's session takes, as `envoy_subscribe` does, and how each
	// just-released pod's process ends. The stuck pod's plan is left alone until its own subtest,
	// which can set it then, since TakeTurn is read at each task (idleRigPlan): a session that
	// acknowledges a task and takes no turn has a turn due, which is a pod the rule keeps.
	endsAsReleased := idleRigPlan{TakeTurn: true, ExitsAsReleasedInProduction: true}
	rig := newIdleWakeRig(t, map[string]idleRigPlan{
		idleRigSubscribed:         {TakeTurn: true, Topics: []string{idleRigSubscribedTopic}},
		idleRigJustReleasedAgent:  endsAsReleased,
		idleRigJustReleasedRole:   endsAsReleased,
		idleRigJustReleasedRouted: endsAsReleased,
		idleRigJustReleasedOutbox: endsAsReleased,
	},
		idleRigDelivery, idleRigChecks, idleRigRoleWake, idleRigAgentWake, idleRigBackstop,
		idleRigKeepNone, idleRigSubscribed, idleRigStuck, idleRigRestart, idleRigLapsed, idleRigFreed,
		idleRigJustReleasedAgent, idleRigJustReleasedRole, idleRigJustReleasedRouted, idleRigJustReleasedOutbox)

	rig.quiet()
	rig.awaitReleased(rig.issues...)
	// What each role was given before its pod was released — the phase task the daemon's boot
	// handed it — is what every count below is taken from.
	rig.markPrompts()

	// First, while the release is moments old: the role claim each of these processes left
	// standing is freed by the listener's reaper once it has gone a live window unrenewed, and the
	// role message below must find that claim still naming the released session before then.
	t.Run("AMessageToAJustReleasedRoleWakesItsPod", func(t *testing.T) {
		idleRigWakeJustReleased(t, rig)
	})

	// The daemon's own messages to a released role, two hook prompts, whose rows wake the role's
	// pod and then publish while its woken session has not claimed the role yet.
	t.Run("TheHookPromptsOfAReleasedRoleReachItsWokenSessionOnce", func(t *testing.T) {
		idleRigOutboxWakesJustReleased(t, rig)
	})

	t.Run("AReleasedPodComesBackOnEachOfTheFiveWakes", func(t *testing.T) {
		idleRigWakeAll(t, rig)
	})

	// This reads what the pass above drove: the five wakes happen once, and this counts what each
	// woken role was given for them. The pass above waited for every one of these tasks, so a
	// count of none here is a task that never came rather than one still on its way.
	t.Run("EachWokenRoleGetsItsTaskOnceNamingTheWake", func(t *testing.T) {
		for _, wake := range idleRigWakes(rig) {
			prompts := rig.newPrompts(wake.issue)
			if len(prompts) != 1 {
				t.Errorf("%s's woken role was given %d tasks, want exactly one: %v", wake.issue.name, len(prompts), idleRigTexts(prompts))
				continue
			}
			if !strings.Contains(prompts[0].Text, wake.task) {
				t.Errorf("%s's woken role was given %q, want a task naming the wake with %q", wake.issue.name, prompts[0].Text, wake.task)
			}
			if wake.issue.name != idleRigDelivery && !strings.Contains(prompts[0].Text, idleRigWakePreamble) {
				t.Errorf("%s's task does not say its pod was released and brought back: %q", wake.issue.name, prompts[0].Text)
			}
		}
	})

	// The task each role was given at the rig's start is its phase's own: the daemon's boot
	// starts the implementer of an issue at `implementing`, whose phase it is, and the task it
	// gives names that phase. The daemon adopts the working copy for such a task, once, before it
	// sends it, so each start task came behind exactly one adopt-working-copy frame, which the
	// role's own process recorded before it answered it (idleRigAdoption.Ahead). That the task
	// names its phase is read from the daemon's own account of the adoption — the line it logs
	// once the shim answers ok, naming the task's delivery and its phase — so this holds for the
	// phase task itself and not for a frame some other send happened to bring.
	t.Run("APhaseTaskReachesItsRoleBehindOneAdoption", func(t *testing.T) {
		for _, issue := range rig.issues {
			prompts := rig.agents.prompts(issue.key, string(issue.role))
			if len(prompts) == 0 {
				t.Errorf("%s's role was given no task at the rig's start", issue.name)
				continue
			}
			start := prompts[0]
			if ahead := rig.adoptionsAhead(issue, 0); len(ahead) != 1 {
				t.Errorf("%s's phase task came behind %d adopt-working-copy frames, want exactly one: %+v",
					issue.name, len(ahead), ahead)
			}
			if adopted := rig.logged(idleRigAdoptedLine, map[string]string{
				"claim": string(rig.token(issue)), "delivery": start.Delivery, "phase": string(phase.Implementing),
			}); adopted != 1 {
				t.Errorf("the daemon logged %d adoptions of the working copy for %s's start task %q of phase %s, want one",
					adopted, issue.name, start.Delivery, phase.Implementing)
			}
		}
	})

	// Each of the five wakes gives its woken role a task of no phase: the row every wake writes
	// names none (workflow's wakeRow), and the operator's delivery is the operator's own task,
	// which names none either. The daemon adopts the working copy for no such task (supervise's
	// Machine.adopts), so each wake's task reaches its role with no adopt-working-copy frame ahead
	// of it, nor any since the release. A role-message wake whose task adopted the working copy is
	// what LINUX-1529 and BET-846 at 04:54Z recorded on a release before cybermonklabs/legion#2539:
	// the woken role's adoption ran its jj commands, `jj metaedit --update-author` or `jj new`, on
	// the `@` of whichever role was working in the issue's one workspace (dispatch://INFRA-2695).
	// The five-wake pass waited for every one of these tasks, and the daemon sends a frame before
	// the task it is for and that task only once the frame is answered, so every frame a wake
	// brought is on the record by now.
	t.Run("ATaskWithNoPhaseReachesItsRoleWithNoAdoption", func(t *testing.T) {
		for _, wake := range idleRigWakes(rig) {
			if idleRigCount(rig.newPrompts(wake.issue), wake.task) == 0 {
				t.Errorf("%s's woken role was never given its wake's task %q", wake.issue.name, wake.task)
				continue
			}
			if frames := rig.newAdoptions(wake.issue); len(frames) != 0 {
				t.Errorf("%s's woken role was sent %d adopt-working-copy frames since its release, want none: its wake's task names no phase: %+v",
					wake.issue.name, len(frames), frames)
			}
		}
	})

	// Row (a) of check k3 (dispatch://INFRA-2846): a role message published right after the
	// release finds the claim the released session left standing. The role process removed its
	// session's record as it stopped and nothing else (idleRigRoleProcess.unregister), so the
	// claim names a holder that is not live, and the listener keeps the message for the role's
	// next live holder and answers 202 holder_lapsed (spec R4), raising the wake report beside
	// that answer.
	t.Run("ARoleMessageToALapsedHolderIsKeptWakesTheRoleAndReachesItOnce", func(t *testing.T) {
		idleRigKeptWakesAndReaches(t, rig, rig.issue(idleRigLapsed), idleRigRoleLapsed,
			"kept while the released session's claim stood", "lapsed-holder-1")
	})

	// Row (b) of check k3: a role message published after the released session's claim is freed
	// finds the role unclaimed, and the listener keeps it and answers 202 unclaimed, raising the
	// wake report beside that answer. The claim is freed by the listener's forced release, for
	// this row's pod alone (idleRigListener.freeClaims).
	t.Run("ARoleMessageToAFreedClaimIsKeptWakesTheRoleAndReachesItOnce", func(t *testing.T) {
		issue := rig.issue(idleRigFreed)
		if status, body := rig.listener.freeClaims(rig.session(issue)); status != http.StatusNoContent {
			t.Fatalf("freeing the claims of %s's released session answered %d %s, want 204", issue.key, status, body)
		}
		idleRigKeptWakesAndReaches(t, rig, issue, idleRigRoleUnclaimed,
			"kept after the released session's claim was freed", "freed-claim-1")
	})

	t.Run("ARoleMessageThatKeepsNothingStillWakesTheRole", func(t *testing.T) {
		issue := rig.issue(idleRigKeepNone)
		token := string(rig.token(issue))
		// A role publish to a released role has two answers now that the listener keeps a role
		// message for the role's next holder (sjawhar/legion#2588): kept, which the five-wake
		// pass's own role message and rows (a) and (b) above take, and this one, row (c) of check
		// k3. `keep: false` is the field a sender that holds a queue of its own sets
		// (notify.Publish), it keeps nothing, and it is answered 404 with the holder state the
		// listener found: holder_lapsed, since the released session's claim still stands
		// (dispatch://INFRA-2846/spec R4). It is the `keep: false` case R3 item 3 names, and the
		// one where only the wake can bring the role back: nothing waits on the role's kept lane,
		// so the sender's next attempt is the only way this message ever reaches a session.
		status, body := rig.listener.publish(map[string]any{
			"topic": notify.RoleTopicPrefix + token, "message": "keeps nothing", "keep": false,
			"source": "agent", "source_session": idleRigSender, "idempotency_key": "keep-none-1",
		})
		idleRigKeptNothing(t, status, body, idleRigRoleLapsed)
		rig.awaitResumed(issue, 2)
		rig.awaitTask(issue, "\ntopic: "+notify.RoleTopicPrefix+token+"\n")
		if prompts := rig.newPrompts(issue); len(prompts) != 1 {
			t.Fatalf("the woken role was given %v, want exactly one task", idleRigTexts(prompts))
		}
		// Nothing was kept, so the woken session is given nothing until the sender tries again;
		// its next attempt reaches it, once.
		if messages := rig.agents.messages(issue.key, string(issue.role)); len(messages) != 0 {
			t.Errorf("the woken session was given %v, want nothing: the publish kept nothing", idleRigTexts(messages))
		}
		idleRigRetryPublish(t, rig, issue, "keeps nothing", "keep-none-1")
	})

	t.Run("AnEventOnASubscribedTopicDoesNotWakeThePod", func(t *testing.T) {
		issue := rig.issue(idleRigSubscribed)
		token := rig.token(issue)
		before := rig.pods.starts(issue.pod)
		status, body := rig.listener.publish(map[string]any{
			"topic": idleRigSubscribedTopic, "message": "an event on a topic the session took",
			"source": "agent", "source_session": idleRigSender, "idempotency_key": "subscribed-1",
		})
		if status != http.StatusOK && status != http.StatusAccepted {
			t.Fatalf("the topic publish answered %d %s, want it taken", status, body)
		}
		// The listener reports a wake only for a role topic no live session holds and for an
		// agent subject nothing can place; a topic a session took with envoy_subscribe is neither,
		// so the pod stays released and its role waits for the backstop instead. The released
		// session's interest record still names the topic, since its process removed only its
		// session's record as it stopped, so the listener's fanout finds that session, has no
		// session record to deliver through, and gives the delivery up without a wake report
		// (fanoutDelivery).
		idleRigSettle(t, 8*time.Second, func() {
			state := rig.api.claim(token)
			if state.State != string(supervise.StateSuspended) || state.SuspendedBy != string(supervise.SuspendedByReleased) {
				t.Fatalf("%s is %s (suspended by %q) after an event on its subscribed topic, want it still released",
					issue.key, state.State, state.SuspendedBy)
			}
			if rig.kube.pod(issue.pod) {
				t.Fatalf("a pod came back for %s after an event on its subscribed topic", issue.key)
			}
		})
		if started := rig.pods.starts(issue.pod); started != before {
			t.Errorf("%s's pod started %d times, want the %d it had: a subscribed topic wakes nothing", issue.key, started, before)
		}
		for _, resume := range rig.releaseRecords(rig.issue(idleRigDelivery)).Resumes {
			if resume.Issue == issue.key {
				t.Errorf("a resume of %s was recorded (%s): a subscribed topic wakes nothing", issue.key, resume.Kind)
			}
		}
	})

	t.Run("AResumeWithNoFirstTurnAfterTenMinutesIsWarnedAndRecordedStuck", func(t *testing.T) {
		issue := rig.issue(idleRigStuck)
		// From here its session acknowledges the task its wake gives it and takes no turn.
		rig.agents.setPlan(issue.key, string(issue.role), idleRigPlan{TakeTurn: false})
		// A role message, because R5's resume is what the daemon records for a wake it answered
		// (store.RecordPodWakeTx); a delivery the daemon already held is its own account of why
		// the pod came back and records none. This sender sets no keep flag, so the listener
		// keeps the message for the role's next holder and answers 202 kept, and raises the wake
		// report beside that answer as it does beside the 404 (roleSend.noHolder).
		status, body := rig.listener.publish(map[string]any{
			"topic": notify.RoleTopicPrefix + string(rig.token(issue)), "message": "take up your phase",
			"source": "agent", "source_session": idleRigSender, "idempotency_key": "stuck-1",
		})
		idleRigKept(t, status, body, idleRigRoleLapsed)
		rig.awaitResumed(issue, 2)
		idleRigEventually(t, idleRigRecordWait, "the resume of "+issue.key+" recorded", func() bool {
			return len(rig.resumesOf(issue)) == 1
		})
		// The clock the stuck pass reads is the resume's stuck clock against the daemon's now
		// (warnStuckResumes), so the test moves the wake and its clock ten minutes into the past.
		// The role process acknowledged its task and started no turn, so no first turn was ever
		// stamped.
		rig.ageResume(issue, 11*time.Minute)
		idleRigEventually(t, idleRigWarningWait, "the warning about the resume with no first turn", func() bool {
			return rig.logged("wake: a released pod's resume has had no first turn since its wake",
				map[string]string{"issue": issue.key, "role": string(issue.role)}) == 1
		})
		idleRigEventually(t, idleRigStuckWait, "the stuck resume on the record", func() bool {
			for _, resume := range rig.resumesOf(issue) {
				if resume.Stuck {
					return true
				}
			}
			return false
		})
		for _, resume := range rig.resumesOf(issue) {
			if resume.FirstTurnAt != "" {
				t.Errorf("the resume of %s records a first turn at %s, want none: its role took no turn", issue.key, resume.FirstTurnAt)
			}
		}
		if warned := rig.logged("wake: a released pod's resume has had no first turn since its wake",
			map[string]string{"issue": issue.key}); warned != 1 {
			t.Errorf("the stuck resume of %s was warned about %d times, want once", issue.key, warned)
		}
	})

	// Last, because it restarts the daemon: the worker stream binds a port of its own on every
	// boot, so the launchers of the pods the earlier subtests woke are left dialling the old one.
	t.Run("AMessageSentDuringTheReleaseReachesTheWokenSessionOnceAcrossARestart", func(t *testing.T) {
		// First the two the daemon was up for, the role message and the direct message of the
		// five-wake pass. The role message's sender tries again, keeping nothing, and that next
		// attempt reaches the woken session exactly once. The direct message needs no next
		// attempt: the listener stored it when it was sent, so that one send reaches the woken
		// session exactly once, from the session's own consumer.
		idleRigRetryPublish(t, rig, rig.issue(idleRigRoleWake), "the role message the sender tried again", "role-retry-1")
		idleRigDeliveredOnce(t, rig, rig.issue(idleRigAgentWake), "the direct message, again")

		// Then the same two across a restart, on a pod still released: the listener reports both
		// wakes while the daemon is down, its durable consumer holds them, and the daemon acts on
		// them once it is back.
		issue := rig.issue(idleRigRestart)
		token := string(rig.token(issue))
		session := rig.session(issue)
		rig.stopDaemon()
		status, body := rig.listener.publish(map[string]any{
			"topic": notify.RoleTopicPrefix + token, "message": "sent while the daemon was down",
			"source": "agent", "source_session": idleRigSender, "idempotency_key": "restart-role-1",
		})
		idleRigKept(t, status, body, idleRigRoleLapsed)
		status, body = rig.listener.send(map[string]any{
			"target_session": session, "message": "sent to the session while the daemon was down",
			"source": "agent", "source_session": idleRigSender, "idempotency_key": "restart-agent-1",
		})
		idleRigStoredForGone(t, status, body)
		rig.restart()
		rig.awaitResumed(issue, 2)
		// The two wakes the listener reported while the daemon was down reach it on its durable
		// consumer once it is back, and the first of them resumes the claim: a wake acts only on
		// a claim that is still released (workflow's wakeReleased), so the pod comes back once
		// and its role is given exactly one task, naming the wake that woke it.
		// The resume is the claim's; the task reaches its agent once the replacement pod is up and
		// the delivery's own retry has found it, which is a few seconds later. Either wake may be
		// the one that took it, so this waits for the task rather than for one wake's wording.
		idleRigEventually(t, idleRigRecordWait, "the task the wake gave "+issue.key+"'s woken role", func() bool {
			return len(rig.newPrompts(issue)) > 0
		})
		prompts := rig.newPrompts(issue)
		if len(prompts) != 1 {
			t.Fatalf("the woken role of %s was given %d tasks after the restart, want exactly one: %v",
				issue.key, len(prompts), idleRigTexts(prompts))
		}
		role := "\ntopic: " + notify.RoleTopicPrefix + token + "\n"
		agent := "\nsession: " + session
		if !strings.Contains(prompts[0].Text, role) && !strings.Contains(prompts[0].Text, agent) {
			t.Errorf("the woken role of %s was given %q, want a task naming the role message or the direct message",
				issue.key, prompts[0].Text)
		}
		if !strings.Contains(prompts[0].Text, idleRigWakePreamble) {
			t.Errorf("%s's task does not say its pod was released and brought back: %q", issue.key, prompts[0].Text)
		}
		// The role message was kept for the role's next holder, so the claim the wake's own pod
		// makes is what hands it over (drainKeptRole) and it reaches the woken session once:
		// nothing but this pod coming back claims that role, so the kept copy moves no sooner
		// than the wake. The direct message was stored on the session's own agent subject when it
		// was sent, the listener knowing the session from its record (idleRigStoredForGone). The
		// woken session registers under the same id, and its new consumer starts at the floor
		// the released one left, so it is handed that message from its own consumer, once, and
		// its sender has no second attempt to make.
		idleRigDeliveredOnce(t, rig, issue, "sent while the daemon was down")
		idleRigDeliveredOnce(t, rig, issue, "sent to the session while the daemon was down")
	})

	rig.kube.requireServed(t)
}

// idleRigWakePreamble is the line every wake's task opens with (workflow.PodWake).
const idleRigWakePreamble = "kind: pod-wake"

// idleRigDeliveredTask is the task the daemon has for the role of the delivery pod, which is R3
// item 1's wake: the delivery is its own account of why the pod came back.
const idleRigDeliveredTask = "the daemon had this task for you while your pod was gone"

// idleRigAdoptedLine is the line the daemon logs for each adoption of the working copy the shim
// answered ok (supervise's adopt), beside the claim, the task's delivery and the task's phase.
const idleRigAdoptedLine = "supervise: the working copy was adopted for the phase task"

// idleRigWake is one of the five wakes of R3: the issue whose pod it brings back, and the words
// the task it gives its woken role names that wake with.
type idleRigWake struct {
	issue idleRigIssue
	task  string
}

// idleRigWakes is the five wakes with a fact of the task each gives, written once: the pass that
// drives them waits for each of these facts and the subtest that counts them reads the same
// words, so neither can assert on a wording the other does not wait for. Each is a line the
// woken role's task holds, written here by hand rather than by the builder of that task: the
// wake's own topic for a role message and its session for an agent message. The role token and
// the session id are the released process's, and a relaunched process registers the same of both
// (the session is derived from the pod and the role, idleRigRoleProcess.register), so this reads
// the same before a wake and after it.
func idleRigWakes(rig *idleWakeRig) []idleRigWake {
	checks, role, agent := rig.issue(idleRigChecks), rig.issue(idleRigRoleWake), rig.issue(idleRigAgentWake)
	return []idleRigWake{
		{rig.issue(idleRigDelivery), idleRigDeliveredTask},
		{checks, "head: " + idleRigHead(checks.key)},
		{role, "\nwake: " + pods.WakeRoleMessage + "\ntopic: " + notify.RoleTopicPrefix + string(rig.token(role)) + "\n"},
		{agent, "\nwake: " + pods.WakeAgentMessage + "\nsession: " + rig.session(agent)},
		{rig.issue(idleRigBackstop), "wake: " + pods.WakeBackstop},
	}
}

// idleRigWakeAll drives the five wakes of R3, once each, and waits for each pod to come back and
// for the task its wake gives it.
func idleRigWakeAll(t *testing.T, rig *idleWakeRig) {
	t.Helper()
	// 1. A delivery the daemon has for a role of the pod. It reaches the released claim through
	//    deliverResuming, the same path every workflow start takes to a suspended claim.
	delivery := rig.issue(idleRigDelivery)
	rig.deliver(delivery, idleRigDeliveredTask)

	// 2. A CI settlement the daemon records for the head of the issue's pull request, while the
	//    phase worker's phase is open.
	checks := rig.issue(idleRigChecks)
	rig.publishSettlement(checks)

	// 3. A message published to a role topic of the pod, from a sender that sets no keep flag:
	//    no live session holds the role, whose claim still names the released session, so the
	//    listener keeps the message for the role's next live holder and answers the sender 202
	//    kept for the lapsed holder (sjawhar/legion#2588, dispatch://INFRA-2846), raising the wake
	//    report beside that answer. The kept copy waits for a live holder of the role, which
	//    nothing but this pod coming back brings, so the wake is still the only thing that moves
	//    this message on.
	role := rig.issue(idleRigRoleWake)
	status, body := rig.listener.publish(map[string]any{
		"topic": notify.RoleTopicPrefix + string(rig.token(role)), "message": "the role message, again",
		"source": "agent", "source_session": idleRigSender, "idempotency_key": "role-wake-1",
	})
	idleRigKept(t, status, body, idleRigRoleLapsed)

	// 4. A message sent to the agent subject of a released session. The session's entry went
	//    with its pod and the listener's known-session store still holds its record, so the
	//    listener stores the message on the session's agent subject, answers the sender 200
	//    stopped and gone, and raises the wake report beside that answer, as it raised it beside
	//    the 404 such a send was answered before the store. The stored message waits in the
	//    session's own consumer for the session to run again under the same id.
	agent := rig.issue(idleRigAgentWake)
	status, body = rig.listener.send(map[string]any{
		"target_session": rig.session(agent), "message": "the direct message, again",
		"source": "agent", "source_session": idleRigSender, "idempotency_key": "agent-wake-1",
	})
	idleRigStoredForGone(t, status, body)

	// 5. The backstop, which needs nothing sent: it was armed by the release itself, and the
	//    test brings its due time forward rather than waiting the period out.
	backstop := rig.issue(idleRigBackstop)
	rig.dueBackstop(backstop)
	for _, issue := range []idleRigIssue{delivery, checks, role, agent, backstop} {
		rig.awaitResumed(issue, 2)
	}
	// The claim's resume and the task are two steps of one outbox `start` row, in that order: the
	// row resumes the claim, waits out the relaunch, and only then delivers the row's task
	// (outbox's start executor). A wake's task therefore reaches its woken role strictly after
	// the resume above, so every wake waits for its own task as well.
	for _, wake := range idleRigWakes(rig) {
		rig.awaitTask(wake.issue, wake.task)
	}
	for _, issue := range []idleRigIssue{delivery, checks, role, agent, backstop} {
		// A second pod was made for the wake. Whether one stands now is not asserted: a woken pod
		// that goes quiet again for the rig's idle period (idleRigIdleMinutes) is released afresh,
		// which is the rule working, not a wake that failed.
		if started := rig.pods.starts(issue.pod); started < 2 {
			t.Errorf("%s's pod was started %d times, want a second pod for the wake", issue.key, started)
		}
	}
	// Each wake is recorded against the release it ended, with the kind that woke it.
	kinds := map[string]string{
		delivery.key: "",
		checks.key:   pods.WakeChecks,
		role.key:     pods.WakeRoleMessage,
		agent.key:    pods.WakeAgentMessage,
		backstop.key: pods.WakeBackstop,
	}
	recorded := map[string]string{}
	for _, resume := range rig.releaseRecords(delivery).Resumes {
		if want, ours := kinds[resume.Issue]; ours && want != "" {
			recorded[resume.Issue] = resume.Kind
		}
	}
	for issue, want := range kinds {
		if want == "" {
			continue
		}
		if got := recorded[issue]; got != want {
			t.Errorf("the resume of %s is recorded as %q, want %q", issue, got, want)
		}
	}
}

// idleRigRoleLapsed is the holder state a role of a released pod resolves to: its session's
// record is removed as its process stops, by the process itself (idleRigRoleProcess.unregister)
// or, for one that ends as a released Oh My Pi does, by the daemon's clean stop
// (idleRigPlan.ExitsAsReleasedInProduction), and its claim is left standing, so the claim names a
// holder that is not live. It is the word both answers of a role publish carry for it — the
// 202's kept_reason and the 404's reason (roleholder.HolderLapsed).
const idleRigRoleLapsed = roleholder.HolderLapsed

// idleRigRoleUnclaimed is the holder state of a role no claim names, in the same two answers: a
// released pod's role once its claim is freed (idleRigListener.freeClaims).
const idleRigRoleUnclaimed = roleholder.Unclaimed

// idleRigRoleAnswer is what a role publish's answer says about the keep and the holder: the
// 202's own kept, kept_reason and kept_until (listenerwire.PublishResponse), the 404's reason
// (listenerwire.RoleRefusal), which names the same holder states in the same words, and the holder
// and holder_state both carry for a holder the claim names.
type idleRigRoleAnswer struct {
	Kept        bool   `json:"kept"`
	KeptReason  string `json:"kept_reason"`
	KeptUntil   int64  `json:"kept_until"`
	Reason      string `json:"reason"`
	Holder      string `json:"holder"`
	HolderState string `json:"holder_state"`
}

func idleRigRoleAnswerOf(t *testing.T, body string) idleRigRoleAnswer {
	t.Helper()
	var answer idleRigRoleAnswer
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode the role publish's answer %q: %v", body, err)
	}
	return answer
}

// idleRigKept is the answer a role publish gets when the listener kept the message for the
// role's next live holder: 202, kept true, kept_reason naming the state the resolution found, and
// kept_until naming the end of the copy's hour, in epoch milliseconds, which lies ahead of the
// answer. Every field is read, so a 202 that kept nothing cannot pass for one that kept the
// message.
func idleRigKept(t *testing.T, status int, body, reason string) {
	t.Helper()
	answer := idleRigRoleAnswerOf(t, body)
	if status != http.StatusAccepted || !answer.Kept || answer.KeptReason != reason {
		t.Fatalf("the role publish answered %d %s, want 202 kept for %q: no live session holds the role, so the listener keeps the message for its next holder",
			status, body, reason)
	}
	if answer.KeptUntil <= time.Now().UnixMilli() {
		t.Fatalf("the role publish answered %d %s, want kept_until ahead of now: the listener keeps the message for an hour and names when that hour ends",
			status, body)
	}
}

// idleRigKeptNothing is the answer a role publish gets when nothing was kept: the 404 whose
// reason names the state the resolution found, which is what a sender's own fallback runs on
// (notify.ErrNoHolder). The absent kept flag is read too, so a publish the listener kept after
// all cannot pass for one that kept nothing.
func idleRigKeptNothing(t *testing.T, status int, body, reason string) {
	t.Helper()
	if answer := idleRigRoleAnswerOf(t, body); status != http.StatusNotFound || answer.Kept || answer.Reason != reason {
		t.Fatalf("the role publish answered %d %s, want 404 %s with nothing kept: no live session holds the role",
			status, body, reason)
	}
}

// idleRigKeptWakesAndReaches holds rows (a) and (b) of check k3 for the released role of issue:
// a role message from a sender that sets no keep flag is answered 202 kept for reason, the wake
// the listener reports beside that answer brings the role back with one task naming the role
// message, and the kept copy reaches the woken session once. The woken pod's claim of the role
// is what hands the copy over (drainKeptRole): nothing but that pod coming back gives the role a
// live holder, so the copy moves no sooner than the wake.
func idleRigKeptWakesAndReaches(t *testing.T, rig *idleWakeRig, issue idleRigIssue, reason, message, key string) {
	t.Helper()
	token := string(rig.token(issue))
	status, body := rig.listener.publish(map[string]any{
		"topic": notify.RoleTopicPrefix + token, "message": message,
		"source": "agent", "source_session": idleRigSender, "idempotency_key": key,
	})
	idleRigKept(t, status, body, reason)
	rig.awaitResumed(issue, 2)
	rig.awaitTask(issue, "\ntopic: "+notify.RoleTopicPrefix+token+"\n")
	if prompts := rig.newPrompts(issue); len(prompts) != 1 {
		t.Fatalf("the woken role of %s was given %v, want exactly one task", issue.key, idleRigTexts(prompts))
	}
	idleRigDeliveredOnce(t, rig, issue, message)
}

// idleRigRetryPublish is the sender's next attempt at a role message, which the wake exists to
// make reach a live session: the sender repeats while no live session holds the role — each such
// attempt is itself a wake — and the one attempt the listener accepts reaches the woken session
// exactly once.
//
// It publishes keep: false, the field the one sender that retries from a queue of its own sets
// (the Legion daemon's notices, notify.Publish). Nothing is kept for the role, so the attempt a
// live holder took is the only way this message can reach the session and the count below reads
// that attempt and nothing else. Every other role publish of the rig lets the listener keep the
// message, as the daemon's own kept publishes do. One kept copy is counted by no function: the
// five-wake pass's role message (idleRigWakeAll's `role-wake-1`), whose pass reads the wake it
// raises and not the copy's hand-over. Every other is counted where it is handed over
// (idleRigDeliveredOnce), by the function that counts it: idleRigKeptWakesAndReaches counts one
// copy for each of rows (a) and (b) of check k3; idleRigWakeJustReleased counts the
// just-released role's message; idleRigOutboxWakesJustReleased counts the daemon's two hook
// prompts; and the restart subtest
// (AMessageSentDuringTheReleaseReachesTheWokenSessionOnceAcrossARestart) counts the role message
// sent while the daemon was down.
func idleRigRetryPublish(t *testing.T, rig *idleWakeRig, issue idleRigIssue, message, key string) {
	t.Helper()
	publish := func() int {
		status, _ := rig.listener.publish(map[string]any{
			"topic": notify.RoleTopicPrefix + string(rig.token(issue)), "message": message, "keep": false,
			"source": "agent", "source_session": idleRigSender, "idempotency_key": key,
		})
		return status
	}
	idleRigUntil(t, idleRigPodWait, "a role message reaching the live holder of "+issue.key, func() bool {
		return publish() == http.StatusOK
	})
	idleRigDeliveredOnce(t, rig, issue, message)
}

// The words a send's answer gives its target (listenerwire.SendResponse, cmd/listener's
// sendHandler): state stopped is a session nothing holds the relay subject of, or one the listener
// knows only by its known-session record, and recipient_state gone is a session the registry
// holds no entry of within the gone horizon. A role publish names a lapsed holder's session by the
// same word, its holder_state.
const (
	idleRigSendStopped   = "stopped"
	idleRigRecipientGone = "gone"
)

// idleRigSendAnswer is what a send's answer says of its target: the state the listener found it
// in, its recipient state and its last registration (Unix ms).
type idleRigSendAnswer struct {
	State          string `json:"state"`
	RecipientState string `json:"recipient_state"`
	LastSeen       int64  `json:"last_seen"`
}

func idleRigSendAnswerOf(t *testing.T, body string) idleRigSendAnswer {
	t.Helper()
	var answer idleRigSendAnswer
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decode the send's answer %q: %v", body, err)
	}
	return answer
}

// idleRigIsStoredForGone is whether a send's answer is the one a send to a released session gets:
// the session's entry went with its pod and the listener's known-session store still holds its
// record, so the listener stores the message on the session's agent subject and answers 200,
// state stopped, recipient_state gone and last_seen the record's. Every field is read, so neither
// the 404 of a session no store knows nor the answer of a live one can pass for it. It decodes the
// body itself and fails nothing, so a caller that carries on past a wrong answer can.
func idleRigIsStoredForGone(status int, body string) bool {
	var answer idleRigSendAnswer
	return status == http.StatusOK && json.Unmarshal([]byte(body), &answer) == nil &&
		answer.State == idleRigSendStopped && answer.RecipientState == idleRigRecipientGone && answer.LastSeen > 0
}

// idleRigStoredForGone fails the test unless the send's answer is the one a send to a released
// session gets (idleRigIsStoredForGone).
func idleRigStoredForGone(t *testing.T, status int, body string) {
	t.Helper()
	if !idleRigIsStoredForGone(status, body) {
		t.Fatalf("the send answered %d %s, want 200 stored with state %s, recipient_state %s and the record's last_seen: the session is gone with its pod, and the listener still knows it",
			status, body, idleRigSendStopped, idleRigRecipientGone)
	}
}

// idleRigDeliveredOnce holds the once-ness R3 items 3 and 4 ask of a sender whose message the
// wake brought a session back for: the one attempt the listener accepted reaches the woken
// session exactly once, and stays at one.
//
// What makes "stays at one" something to read rather than a span to wait out is the session's
// own consumer. Every message a session is given reaches it as a frame of that one consumer, in
// the order the listener stored them, and the role process records each frame as it answers it
// (idleRigRoleProcess.openDelivery and relayed, idle_rig_agent_test.go) — a role-lane forward
// included, since the listener stores that on the session's agent subject and it comes back as
// a frame of the same consumer. So a second copy of the message, from whichever lane, is
// recorded before a message the listener took after it: this sends one such message and waits
// for it, and the count is read once that message is in. That is what the six-second poll it
// replaces was waiting out, and it is an observation of the rig's own delivery rather than a
// guess at how long one takes.
func idleRigDeliveredOnce(t *testing.T, rig *idleWakeRig, issue idleRigIssue, message string) {
	t.Helper()
	recorded := func(text string) int {
		return idleRigCount(rig.agents.messages(issue.key, string(issue.role)), text)
	}
	idleRigEventually(t, idleRigRecordWait, "the message reaching the woken session of "+issue.key, func() bool {
		return recorded(message) >= 1
	})
	// The text is the session's own message count, which only grows, so each of this row's
	// deliveries is followed by a message of its own and no wait here is answered by an earlier
	// one. It holds none of the message's own words, which idleRigCount matches on.
	after := fmt.Sprintf("the rig's next message to %s, its %dth", issue.key,
		len(rig.agents.messages(issue.key, string(issue.role))))
	status, body := rig.listener.send(map[string]any{
		"target_session": rig.session(issue), "message": after,
		"source": "agent", "source_session": idleRigSender, "idempotency_key": after,
	})
	if status != http.StatusOK {
		t.Fatalf("the message sent to the session of %s after %q answered %d %s, want 200",
			issue.key, message, status, body)
	}
	// A woken pod that goes quiet is released afresh within the rig's idle period, so the
	// session may have gone again since the message reached it. Such a send is stored and
	// answered stopped, and raises the wake its pod comes back for, so its record waits for a
	// pod as well as for the delivery.
	within := idleRigRecordWait
	if idleRigSendAnswerOf(t, body).State == idleRigSendStopped {
		within = idleRigPodWait
	}
	idleRigEventually(t, within, "the message sent to "+issue.key+" after the one its wake was for", func() bool {
		return recorded(after) >= 1
	})
	if got := recorded(message); got != 1 {
		t.Fatalf("the woken session of %s recorded the message %d times for the one attempt the listener accepted, want once: %v",
			issue.key, got, idleRigTexts(rig.agents.messages(issue.key, string(issue.role))))
	}
}

// idleRigUntil is idleRigEventually at a sender's pace: a second between attempts, so a retry
// loop that is itself a wake does not send a hundred of them a second. Its span is its caller's,
// clamped by the run's own deadline as idleRigEventually's is (idleRigWaiting).
func idleRigUntil(t *testing.T, within time.Duration, what string, attempt func() bool) {
	t.Helper()
	idleRigWaiting(t, within, what, time.Second, attempt)
}

// awaitTask waits until the issue's woken role has been given a task carrying want.
func (r *idleWakeRig) awaitTask(issue idleRigIssue, want string) {
	r.t.Helper()
	idleRigEventually(r.t, idleRigRecordWait, "the woken role of "+issue.key+" given its wake's task", func() bool {
		return idleRigCount(r.newPrompts(issue), want) >= 1
	})
}

// idleRigSettle runs check now and again for the whole of within, so a test that must see
// nothing happen watches for it rather than reading once and moving on. Its span is not a bound
// on a fault, as every other wait of the row carries, but the watch itself: it has one caller,
// the subscribed topic, whose claim is that nothing was woken. The listener decides whether to
// raise a wake report inside the publish this follows, so what the span covers is the daemon's
// own pass over the reports it raised — and a publish that raised none leaves nothing positive
// to wait for. Every other wait of the row is for something that does happen, and gives up at
// its own span instead (idleRigEventually).
func idleRigSettle(t *testing.T, within time.Duration, check func()) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		check()
		if t.Failed() || time.Now().After(deadline) {
			return
		}
		time.Sleep(idleRigPoll)
	}
}

// idleRigTexts is what a failure prints of the messages a role recorded.
func idleRigTexts(messages []idleRigMessage) []string {
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		texts = append(texts, message.Text)
	}
	return texts
}

// deliver hands the issue's role a task the daemon holds for it, through the route that posts a
// RequestDeliver to the claim's machine — the same event every workflow start carries, and the
// one deliverResuming resumes a released claim for.
func (r *idleWakeRig) deliver(issue idleRigIssue, task string) {
	r.t.Helper()
	token := r.token(issue)
	status, body := r.api.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		api.DeliverRequest{Task: task}, true)
	if status != http.StatusOK && status != http.StatusAccepted {
		r.t.Fatalf("deliver to %s = %d %s", token, status, body)
	}
}

// session is the session id the issue's role process registered.
func (r *idleWakeRig) session(issue idleRigIssue) string {
	r.t.Helper()
	seen := r.agents.role(issue.key, string(issue.role))
	if seen.Session == "" {
		r.t.Fatalf("%s's role process has registered no session", issue.key)
	}
	return seen.Session
}

// resumesOf is every recorded resume of the issue.
func (r *idleWakeRig) resumesOf(issue idleRigIssue) []api.ResumeRecord {
	r.t.Helper()
	var mine []api.ResumeRecord
	for _, resume := range r.releaseRecords(issue).Resumes {
		if resume.Issue == issue.key {
			mine = append(mine, resume)
		}
	}
	return mine
}

// dueBackstop brings the issue's armed backstop due now, which is the clock the backstop pass
// reads: the row's due time against the daemon's own (workflowRuntime.fireDueBackstops).
func (r *idleWakeRig) dueBackstop(issue idleRigIssue) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(),
		`update pod_wake_backstops set due_at = now() - interval '1 minute' where project = $1 and issue = $2 and not fired`,
		r.project, issue.key); err != nil {
		r.t.Fatalf("bring the backstop of %s due: %v", issue.key, err)
	}
}

// ageResume moves the issue's recorded wakes, and the stuck clock each starts, into the past. The
// stuck pass reads the resume's stuck clock against the daemon's clock and nothing else
// (warnStuckResumes, store's clock_at), so this row is the clock the ten minutes of R5 is measured
// on.
func (r *idleWakeRig) ageResume(issue idleRigIssue, by time.Duration) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(),
		fmt.Sprintf(`update pod_resumes set at = at - interval '%[1]d seconds', clock_at = clock_at - interval '%[1]d seconds'
			where project = $1 and issue = $2`, int(by.Seconds())),
		r.project, issue.key); err != nil {
		r.t.Fatalf("age the resumes of %s: %v", issue.key, err)
	}
}
