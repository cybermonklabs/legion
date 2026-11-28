package legion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testwait"
)

// lastSeqOf is project's cursor's last_seq, 0 for none.
func lastSeqOf(t *testing.T, rig *intakeRig, project string) int64 {
	t.Helper()
	var last int64
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select coalesce(max(last_seq), 0) from legion_event_cursors where project = $1`, project).Scan(&last); err != nil {
		t.Fatalf("read the cursor of %s: %v", project, err)
	}
	return last
}

// TestFifteenProjectsInSlowReadsLeaveTheWindowAMessageOfAnotherProject: R3 holds back no project
// behind another project's read, and each project that owes a read holds at most one message of
// the consumer's window. A project keeps at most stateMaxWaiting messages waiting on its runner,
// the one in a read included; past that, one message holds the read it owes and goes back to the
// stream, and the others are acknowledged (judgeCrowded). So a project in a slow read holds
// stateMaxWaiting+1 messages of the durable's window of stateMaxAckPending, 17 of 256, and fifteen
// such projects hold 255. Fifteen projects each make a gap read that their daemon holds, and each
// publishes stateMaxWaiting+2 more messages while that read is held. A message of a sixteenth
// project is then applied in its usual time, and no slow project's daemon is asked for another
// read.
func TestFifteenProjectsInSlowReadsLeaveTheWindowAMessageOfAnotherProject(t *testing.T) {
	const slow = 15
	const usual = 3 * time.Second
	window := slow * (stateMaxWaiting + 1)
	rig := newIntakeRig(t)
	projects := make([]string, slow)
	daemons := make([]*fakeDaemon, slow)
	for i := range slow {
		projects[i] = fmt.Sprintf("SLOW%02d", i+1)
		daemons[i] = newFakeDaemon(t, daemonState(projects[i], 10, planning(projects[i])))
		seedLegionProject(t, rig.database, projects[i], daemons[i].URL)
	}
	quick := newFakeDaemon(t, daemonState("QUICK", 10, planning("QUICK")))
	seedLegionProject(t, rig.database, "QUICK", quick.URL)
	rig.start()
	rig.awaitWholeReads(slow + 1)
	// Each starting read's minute passes, so each project's gap read is due at once.
	rig.clock.advance(ReadRetryAfter + time.Second)

	releases := make([]func(), slow)
	for i, daemon := range daemons {
		releases[i] = daemon.holds(t)
	}
	for _, project := range projects {
		rig.publish(project, change(12, project+"-1", moved("phase", "testing")))
	}
	rig.waitFor(30*time.Second, func() bool {
		return !slices.ContainsFunc(daemons, func(daemon *fakeDaemon) bool { return daemon.served() < 2 })
	}, func() string { return "a slow project's gap read never began" })
	for _, project := range projects {
		for seq := 13; seq < 13+stateMaxWaiting+2; seq++ {
			rig.publish(project, change(seq, project+"-1", moved("phase", "testing")))
		}
	}
	rig.waitFor(30*time.Second, func() bool { return rig.consumerInfo().NumAckPending >= window }, func() string {
		return fmt.Sprintf("the slow projects' messages did not reach the intake: %+v", rig.consumerInfo())
	})

	published := time.Now()
	rig.publish("QUICK", change(11, "QUICK-1", moved("phase", "implementing")))
	if !testwait.Holds(usual, 25*time.Millisecond, func() bool { return lastSeqOf(t, rig, "QUICK") >= 11 }) {
		t.Errorf("QUICK's message was not applied within %s of its publish while fifteen projects were in slow reads: the window is full: %+v",
			usual, rig.consumerInfo())
	} else {
		t.Logf("QUICK's message was applied %s after its publish", time.Since(published).Round(time.Millisecond))
	}
	if held := rig.consumerInfo().NumAckPending; held != window {
		t.Errorf("the slow projects hold %d messages of the window, want %d: %d waiting on each runner and one held each", held, window, stateMaxWaiting)
	}
	if holds := rig.logRecords(heldLine); len(holds) != slow {
		t.Errorf("%d messages went back to the stream, want one for each slow project: %v", len(holds), holds)
	}
	for i, daemon := range daemons {
		if served := daemon.served(); served != 2 {
			t.Errorf("%s's daemon served %d reads, want its starting read and its gap read alone", projects[i], served)
		}
	}
	for _, release := range releases {
		release()
	}
}

// TestAProjectPastTheCapDuringAGoodReadThatGoesQuietIsReadWithinTheMinute: R3 keeps a project that
// goes quiet at most one minute behind while its reads succeed. INFRA's gap read at 12 is in flight,
// held by its daemon, which serves 30. Near the end of that read's minute on the intake's clock,
// INFRA publishes 15 messages at or below 30, 13 to 27, which wait on INFRA's runner behind the
// read, and then 31 to 33, past the read's snapshot and past the cap of 16 messages waiting
// (stateMaxWaiting, read-rule-cases row 8.1): each owes a gap read, which the read in flight does
// not clear (recordWhole), the first of them holds it and goes back to the stream until the minute
// ends, and the other two join it and are acknowledged. So while 31 holds the read, gone back
// once, INFRA holds 17 messages of the window: 16 waiting on its runner, 12 to 27, and 31. The
// read succeeds at 30, and INFRA goes quiet. Within the time left of the minute and a few seconds,
// a gap read is made, and INFRA-1's phase reaches 33's reviewing.
//
// The test catches a cap other than 16. It reads the window while the first message past the cap
// has gone back once: that message goes back again each holdAtLeast once the minute is over, and
// is let go after stateMaxHolds hand-backs, after which INFRA holds one message fewer. With a cap
// of 15 INFRA holds 16 messages while that message has gone back once, and with a cap of 17, 18.
func TestAProjectPastTheCapDuringAGoodReadThatGoesQuietIsReadWithinTheMinute(t *testing.T) {
	// left is what is left of the read's minute when the messages past the cap arrive.
	const left = 5 * time.Second
	phase := func(phase string) string {
		return strings.Replace(planning("INFRA"), `"planning"`, fmt.Sprintf("%q", phase), 1)
	}
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	release := daemon.holds(t)
	daemon.serves(daemonState("INFRA", 30, phase("implementing")))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "implementing")))
	rig.waitFor(20*time.Second, func() bool { return daemon.served() >= 2 }, func() string {
		return "INFRA's gap read never began"
	})
	// The read in flight serves the state at 30, which it took as it began; any later read serves 33.
	daemon.serves(daemonState("INFRA", 33, phase("reviewing")))
	rig.clock.advance(ReadRetryAfter - left)
	for seq := 13; seq <= 27; seq++ {
		rig.publish("INFRA", change(seq, "INFRA-1", moved("phase", "implementing")))
	}
	for seq, past := range []string{"testing", "merging", "reviewing"} {
		rig.publish("INFRA", change(31+seq, "INFRA-1", moved("phase", past)))
	}
	// held is INFRA's share of the window while the first message past the cap has gone back once.
	var held int
	rig.waitFor(20*time.Second, func() bool {
		info := rig.consumerInfo()
		if info.NumPending != 0 || len(rig.logRecords(heldLine)) != 1 {
			return false
		}
		held = info.NumAckPending
		return held == 17
	}, func() string {
		return fmt.Sprintf("INFRA holds %d messages of the window while the first message past the cap has gone back once, want 17: 16 waiting on its runner and that message (%+v)",
			held, rig.consumerInfo())
	})

	release()
	read := time.Now()
	if !testwait.Holds(left+15*time.Second, 100*time.Millisecond, func() bool {
		return currentValues(t, rig.database, "INFRA")["INFRA-1/phase"] == `"reviewing"`
	}) {
		t.Fatalf("%s after the read at 30, with INFRA quiet, INFRA-1's phase is %s and INFRA's cursor is %s: the events past the cap were never read",
			time.Since(read).Round(time.Second), currentValues(t, rig.database, "INFRA")["INFRA-1/phase"], cursorOf(t, rig.database, "INFRA"))
	}
	rig.awaitWholeReads(3)
	reads := wholeReadsOf(t, rig.database, "INFRA")
	if len(reads) != 3 {
		t.Fatalf("INFRA's whole reads are %v, want the starting read, the gap read at 30 and one more", reads)
	}
	requireWholeRead(t, reads[1], ReadReasonGap, true)
	requireWholeRead(t, reads[2], ReadReasonGap, true)
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return daemon.served() > 3 }) {
		t.Errorf("INFRA's daemon served %d reads, want the starting read and two gap reads", daemon.served())
	}
}

// startAfterTheTakeOversSettingsRead starts the rig's intake and waits until its take-over's
// settings read (takenOver, SettingsChanged) has listed the settings: LINUX, whose setting it
// writes first, is read whole at the take-over (awaitStartingRead). No settings wake reaches the
// rig's intake, so a setting written after that reaches no starting point, and its project has no
// cursor until its first message makes the starting read. rig.start alone does not wait for the
// settings read: it returns once the take-over has created the durable, and the take-over runs
// its callback, which queues the read, only after that. As awaitStartingRead does, it moves the
// intake's clock past the minute of LINUX's read.
func startAfterTheTakeOversSettingsRead(t *testing.T, rig *intakeRig) {
	t.Helper()
	linux := newFakeDaemon(t, daemonState("LINUX", 10, planning("LINUX")))
	seedLegionProject(t, rig.database, "LINUX", linux.URL)
	rig.start()
	rig.awaitStartingRead("LINUX")
}

// TestMessagesPastTheCapOfAProjectWithNoCursorHoldOneMessageAndAreReadOnTheRunner: row 8.1 for a
// project whose setting was written while no settings wake reached the intake, after the
// take-over's settings read (startAfterTheTakeOversSettingsRead), so it has no cursor yet. Its
// first message makes the starting read on the project's runner, and INFRA's daemon holds
// the answer, a state at 30. While that read is held, near the end of its minute on the intake's
// clock, INFRA publishes 15 more messages at or below 30, which wait on the runner behind it, and
// then 31 to 33, past the read's snapshot and past the cap of stateMaxWaiting. Those make no read
// beside the runner: the project is given a cursor that follows no daemon, which records the gap
// read they owe, the first of them holds that read and the other two are acknowledged, so INFRA
// holds stateMaxWaiting+1 messages of the window. Once the starting read lands, the gap read is
// made on the runner too, and INFRA-1's phase reaches 33's reviewing.
//
// At 46283d5bb626 the test wrote INFRA's setting as soon as rig.start returned, once the take-over
// had created the durable, and the take-over's settings read, which its callback queues only after
// that, could list the setting: INFRA's starting point then gave INFRA a cursor before its first
// message, in 1 of 50 runs. Its failure in CI's envoy-go leg 3 at 19e82b546576 (run 46114210725)
// was the intake's (TestEachSubscriptionCountsTheMessagesWaitingOnARunnerFromItsFirstMessage).
func TestMessagesPastTheCapOfAProjectWithNoCursorHoldOneMessageAndAreReadOnTheRunner(t *testing.T) {
	// left is what is left of the starting read's minute when the messages past the cap arrive.
	const left = 5 * time.Second
	phase := func(phase string) string {
		return strings.Replace(planning("INFRA"), `"planning"`, fmt.Sprintf("%q", phase), 1)
	}
	rig := newIntakeRig(t)
	startAfterTheTakeOversSettingsRead(t, rig)
	daemon := newFakeDaemon(t, daemonState("INFRA", 30, phase("implementing")))
	release := daemon.holds(t)
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	var cursors int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from legion_event_cursors where project = 'INFRA'`).Scan(&cursors); err != nil || cursors != 0 {
		t.Fatalf("INFRA has %d cursors before its first message (%v), want none", cursors, err)
	}

	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.waitFor(20*time.Second, func() bool { return daemon.served() >= 1 }, func() string {
		return "INFRA's first message never made the starting read"
	})
	// The read in flight serves the state at 30, which it took as it began; any later read serves 33.
	daemon.serves(daemonState("INFRA", 33, phase("reviewing")))
	rig.clock.advance(ReadRetryAfter - left)
	for seq := 12; seq < 12+stateMaxWaiting-1; seq++ {
		rig.publish("INFRA", change(seq, "INFRA-1", moved("phase", "implementing")))
	}
	for seq, past := range []string{"testing", "merging", "reviewing"} {
		rig.publish("INFRA", change(31+seq, "INFRA-1", moved("phase", past)))
	}
	rig.waitFor(20*time.Second, func() bool {
		info := rig.consumerInfo()
		return info.NumPending == 0 && info.NumAckPending == stateMaxWaiting+1
	}, func() string {
		return fmt.Sprintf("INFRA holds %+v and %d messages went back, want %d messages waiting on its runner and one held",
			rig.consumerInfo(), len(rig.logRecords(heldLine)), stateMaxWaiting)
	})
	if served := daemon.served(); served != 1 {
		t.Errorf("INFRA's daemon served %d reads while its starting read was held, want that read alone: a message past the cap made a read beside the runner", served)
	}
	if holds := rig.logRecords(heldLine); len(holds) != 1 || holds[0]["seq"] != float64(31) || holds[0]["reason"] != ReadReasonGap {
		t.Errorf("the messages sent back = %v, want the first message past the cap, at 31, once, holding the gap read", holds)
	}
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonGap}) || c.readHolder == nil || c.readHolds != 1 ||
		c.lastSeq != nil || c.stateID != nil {
		t.Errorf("INFRA's cursor while its starting read is held is %s, want one that follows no daemon, owes the gap read and names its holder once", c)
	}
	if drops := rig.awaitLogRecords(crowdedLine, 1); drops[0]["project"] != "INFRA" || drops[0]["reason"] != ReadReasonGap {
		t.Errorf("the drops logged = %v, want one naming INFRA and the gap", drops)
	}

	release()
	rig.waitFor(left+15*time.Second, func() bool {
		return currentValues(t, rig.database, "INFRA")["INFRA-1/phase"] == `"reviewing"`
	}, func() string {
		return fmt.Sprintf("after the starting read at 30 INFRA-1's phase is %s and INFRA's cursor is %s: the events past the cap were never read",
			currentValues(t, rig.database, "INFRA")["INFRA-1/phase"], cursorOf(t, rig.database, "INFRA"))
	})
	rig.settled()
	reads := wholeReadsOf(t, rig.database, "INFRA")
	if len(reads) != 2 {
		t.Fatalf("INFRA's whole reads are %v, want the starting read and the gap read", reads)
	}
	requireWholeRead(t, reads[0], ReadReasonStart, true)
	requireWholeRead(t, reads[1], ReadReasonGap, true)
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return daemon.served() > 2 }) {
		t.Errorf("INFRA's daemon served %d reads, want the starting read and the gap read", daemon.served())
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || c.readHolder != nil || c.lastSeq == nil || *c.lastSeq != 33 {
		t.Errorf("INFRA's cursor is %s, want it at the gap read's 33 with nothing owed and no holder", c)
	}
}

// keyOn keys a message of project as the subscription of the rig's durable at the deliver subject
// subject delivered it, as that subscription's callback keys each message it receives
// (messageKey), and answers the message's key. The message is never delivered.
func keyOn(t *testing.T, rig *intakeRig, project, subject string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"project": project})
	if err != nil {
		t.Fatalf("encode the state message: %v", err)
	}
	data, err := json.Marshal(contracts.Envelope{Payload: string(payload)})
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	key, _, _ := rig.intake.messageKey(&natsgo.Msg{Subject: contracts.LegionStateSubject(strings.ToLower(project)), Data: data, Sub: &natsgo.Subscription{Subject: subject}})
	return key
}

// TestEachSubscriptionCountsTheMessagesWaitingOnARunnerFromItsFirstMessage: a project keeps at
// most stateMaxWaiting messages waiting on its runner, counted for the subscription of the
// durable that delivered them (subscriptionState). A take-over subscribes on a fresh deliver
// subject and creates the durable, which delivers there from then on, and runs its callback
// (takenOver) only after that, once it has recorded the subject in its hold, so the new
// subscription can key messages before the callback runs. The messages of the subscription before
// it, which its stop handed back, count none of the new subscription's, and the new
// subscription's count its own whenever the callback runs.
//
// The test keys INFRA's messages itself (keyOn) and delivers none of them: stateMaxWaiting
// messages of a subscription before the rig's take-over, which its stop handed back; then a
// message of the subscription the take-over bound, which waits on INFRA's runner; then
// stateMaxWaiting-2 more of it, the take-over's callback and one more, so stateMaxWaiting of its
// messages wait, all but one keyed before the callback ran; then its next message, which runs on
// INFRA's crowded key.
//
// At 19e82b546576 the callback started the counts again at 0 (CI's envoy-go leg 3, run
// 46114210725, job 140884474253): in
// TestMessagesPastTheCapOfAProjectWithNoCursorHoldOneMessageAndAreReadOnTheRunner the subscription
// keyed INFRA's first message before the callback ran, so INFRA's message at 31, the 17th to wait,
// waited on its runner, and the message at 32 held the gap read and went back to the stream
// stateMaxHolds times.
func TestEachSubscriptionCountsTheMessagesWaitingOnARunnerFromItsFirstMessage(t *testing.T) {
	rig := newIntakeRig(t)
	rig.start()

	before := natsgo.NewInbox()
	for range stateMaxWaiting {
		if got := keyOn(t, rig, "INFRA", before); got != "INFRA" {
			t.Fatalf("control: a message of the subscription before the take-over is keyed %q with fewer than %d waiting, want INFRA's runner", got, stateMaxWaiting)
		}
	}
	if got := keyOn(t, rig, "INFRA", rig.subject); got != "INFRA" {
		t.Fatalf("the first message of the subscription the take-over bound is keyed %q, want INFRA's runner: the %d messages the subscription before it handed back count against it", got, stateMaxWaiting)
	}
	for range stateMaxWaiting - 2 {
		keyOn(t, rig, "INFRA", rig.subject)
	}
	rig.intake.takenOver()
	if got := keyOn(t, rig, "INFRA", rig.subject); got != "INFRA" {
		t.Fatalf("the subscription's message %d is keyed %q, want INFRA's runner", stateMaxWaiting, got)
	}
	if got := keyOn(t, rig, "INFRA", rig.subject); got != crowdedKey("INFRA") {
		t.Fatalf("the subscription's message %d, with %d of its messages waiting on INFRA's runner, %d of them keyed before the take-over's callback ran, is keyed %q, want INFRA's crowded key",
			stateMaxWaiting+1, stateMaxWaiting, stateMaxWaiting-1, got)
	}
}

// TestAMessageAfterTheTakeOverCallbackQueuesNoSecondCallForTheOwedRead: at most one call for a
// project's owed read waits on its runner (callRead), for the subscription that delivered the
// message that queued it (subscriptionState). A message of the new subscription can queue that
// call before the take-over's callback (takenOver) runs, as it can be keyed before it, and a
// message after the callback queues none beside it: a call queued behind a read the daemon never
// answers would find the read due again once that read's RequestTimeout is over, and make one
// more.
//
// INFRA's starting read at the take-over is refused, so INFRA owes a read for retry that no
// message holds, as in TestATakeOverReadsAProjectWhoseOwedReadNoMessageHolds. Work the test
// queues keeps INFRA's runner busy. For a message the rig's subscription keyed (keyOn), the test
// calls for the owed read, as a message of a store INFRA's cursor drops does; then it runs the
// take-over's callback, and calls again for a second message of the subscription. The intake's
// clock then moves ReadRetryAfter on at each reading (intakeClock.jumps), so each call that runs
// finds the read due, and the runner is let go. INFRA's daemon serves two more reads, the first
// call's and the one the callback's settings read queues (SettingsChanged, startingPoint), and no
// third.
//
// At 46283d5bb626 the callback emptied the calls that waited, so the second message queued a
// second call, and the daemon served a third read.
func TestAMessageAfterTheTakeOverCallbackQueuesNoSecondCallForTheOwedRead(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	daemon.refuses(http.StatusServiceUnavailable)
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitWholeReads(1)
	requireWholeRead(t, wholeReadsOf(t, rig.database, "INFRA")[0], ReadReasonStart, false)
	requireOwedWithNoHolder(t, rig, "INFRA")
	release := make(chan struct{})
	letGo := sync.OnceFunc(func() { close(release) })
	t.Cleanup(letGo)
	if !rig.intake.durable.Queue("INFRA", func(ctx context.Context) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}) {
		t.Fatal("control: INFRA's runner took no work")
	}

	owed := cursor{exists: true, readReasons: []string{ReadReasonRetry}}
	keyOn(t, rig, "INFRA", rig.subject)
	rig.intake.callRead("INFRA", owed)
	rig.intake.takenOver()
	keyOn(t, rig, "INFRA", rig.subject)
	rig.intake.callRead("INFRA", owed)

	served := daemon.served()
	rig.clock.jumps(ReadRetryAfter)
	letGo()
	rig.waitFor(20*time.Second, func() bool { return daemon.served() >= served+2 }, func() string {
		return fmt.Sprintf("INFRA's daemon served %d reads once its runner was let go, want the first call's and the one the take-over's callback queues", daemon.served()-served)
	})
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return daemon.served() > served+2 }) {
		t.Errorf("INFRA's daemon served %d reads once its runner was let go, want 2, the first call's and the one the take-over's callback queues: the message after the callback queued a second call beside the one that waited",
			daemon.served()-served)
	}
}

// TestABootOfADroppedStorePastTheCapFindsTheSettingsDaemonAgain: read-rule-cases rows 3.5 and
// 8.1. A boot of a store the cursor drops - a store
// marked as no daemon of the project, or the recorded old store - while the cursor follows the
// setting's URL in another store a read verified, is judged as a message of another store, on the
// project's runner and past the cap of stateMaxWaiting alike: it owes the store read and holds
// it, and its redelivery makes that read. So a daemon that a redeploy started behind the setting's
// URL in that store is found again within about a minute, even where its boot is the project's
// 17th waiting message and no read is in flight.
//
// Each case sets INFRA's setting as TestABootOfAMarkedOrOldStoreWhoseReadFindsTheCursorsStoreUnchangedAppliesNothing
// does: A read well in the rig's store, B whose starting read fails, then C read well in a store of
// its own, so A's store is the recorded old store; for the marked store, B's message marks B's
// store. C's URL comes to serve the dropped store at 41, with no write of the setting, and the
// intake's clock moves ReadRetryAfter and a second on. Work queued on INFRA's runner keeps it busy
// with no read in flight, 16 messages of C's store wait behind that work, and the dropped store's
// boot at 41 arrives past the cap. The test holds the boot to owing the store read and holding it:
// the boot goes back to the stream holding that read, at its hold's first hand-back (heldLine), and
// INFRA's cursor owes the store read alone and names the boot as its holder, while no read is in
// flight. The runner is then let go, and within 75 s of wall-clock time INFRA's cursor follows the
// dropped store at 41.
//
// C serves the store at the boot's sequence, as a daemon that published the boot does: a daemon
// commits each event before it publishes it. So the cursor reaches 41 whichever delivery makes
// the store read the boot owes: the boot's redelivery, or the first message that waited on the
// runner, which makes a read the cursor owes once it is due, whatever message holds it, none
// included (cursor.readFor). The cursor at 41 so does not show the hold, and the test checks the
// hold before it lets the runner go.
//
// At d69f92b6, judgeCrowded finished the boot as a message of a dropped store (cursor.forgets)
// while no read was in flight: it owed nothing, and INFRA stayed on C's store.
func TestABootOfADroppedStorePastTheCapFindsTheSettingsDaemonAgain(t *testing.T) {
	const bootRun = "4b8e1d6a-2c7f-4a93-b5e0-9d3c6f1a8e24"
	for _, tc := range []struct {
		name  string
		store string // the store that boots
		marks bool
	}{
		{name: "a store marked as no daemon of the project", store: bStore, marks: true},
		{name: "the recorded old store", store: rigStateID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIntakeRig(t)
			a := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
			seedLegionProject(t, rig.database, "INFRA", a.URL)
			rig.start()
			rig.awaitStartingRead("INFRA")
			b := newFakeDaemon(t, "")
			b.refuses(http.StatusServiceUnavailable)
			rig.pointAt(b, 2)
			c := newFakeDaemon(t, daemonStateIn("INFRA", 5, inPhase("implementing"), cStore, cRun))
			rig.pointAt(c, 3)
			if tc.marks {
				rig.publishIn("INFRA", bRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, change(20, "INFRA-1", moved("phase", "merging")))
				rig.settled()
			}
			if cur := cursorOf(t, rig.database, "INFRA"); cur.stateID == nil || *cur.stateID != cStore || cur.previousStateID == nil ||
				*cur.previousStateID != rigStateID || slices.Contains(cur.foreignStateIDs, bStore) != tc.marks || cur.readOwed {
				t.Fatalf("control: INFRA's cursor before the redeploy is %s, want C's store %s, A's store %s recorded, B's store marked %v and nothing owed",
					cur, cStore, rigStateID, tc.marks)
			}

			// The redeploy: C's URL serves the dropped store, with no write of the setting.
			c.serves(daemonStateIn("INFRA", 41, inPhase("reviewing"), tc.store, bootRun))
			rig.clock.advance(ReadRetryAfter + time.Second)
			release := make(chan struct{})
			let := sync.OnceFunc(func() { close(release) })
			t.Cleanup(let)
			if !rig.intake.durable.Queue("INFRA", func(ctx context.Context) {
				select {
				case <-release:
				case <-ctx.Done():
				}
			}) {
				t.Fatal("control: the intake took no work on INFRA's runner")
			}
			at := time.Now().UTC().Truncate(time.Microsecond)
			for seq := 6; seq < 6+stateMaxWaiting; seq++ {
				rig.publishIn("INFRA", cRun, cStore, at, false, change(seq, "INFRA-1", moved("phase", "implementing")))
			}
			rig.waitFor(20*time.Second, func() bool {
				info := rig.consumerInfo()
				return info.NumPending == 0 && info.NumAckPending >= stateMaxWaiting
			}, func() string {
				return fmt.Sprintf("control: INFRA holds %+v, want %d messages waiting on its runner", rig.consumerInfo(), stateMaxWaiting)
			})
			rig.publishIn("INFRA", bootRun, tc.store, at, false, booted(41, stateSchema, 2))
			boot := rig.lastStreamSeq()
			rig.waitFor(20*time.Second, func() bool { return rig.consumerInfo().NumPending == 0 }, func() string {
				return fmt.Sprintf("control: the boot past the cap was not delivered: %+v", rig.consumerInfo())
			})
			var held map[string]any
			rig.waitFor(20*time.Second, func() bool {
				for _, record := range rig.logRecords(heldLine) {
					if record["streamSeq"] == float64(boot) && record["reason"] == ReadReasonStore {
						held = record
						return true
					}
				}
				return false
			}, func() string {
				return fmt.Sprintf("the boot of %s past the cap (stream sequence %d) never went back to the stream holding the store read: the messages sent back = %v, and INFRA's cursor is %s",
					tc.store, boot, rig.logRecords(heldLine), cursorOf(t, rig.database, "INFRA"))
			})
			if cur := cursorOf(t, rig.database, "INFRA"); held["handBack"] != float64(1) || cur.readHolder == nil || *cur.readHolder != int64(boot) ||
				!slices.Equal(cur.readReasons, []string{ReadReasonStore}) {
				t.Fatalf("the boot went back as %v with INFRA's cursor %s, want its hold's first hand-back, and the cursor owing the store read alone with the boot (stream sequence %d) as its holder",
					held, cur, boot)
			}
			if rig.intake.readsNow("INFRA") {
				t.Fatal("control: a whole read of INFRA is in flight while the boot past the cap is judged")
			}
			let()
			if !testwait.Holds(75*time.Second, 200*time.Millisecond, func() bool {
				cur := cursorOf(t, rig.database, "INFRA")
				return cur.stateID != nil && *cur.stateID == tc.store && cur.lastSeq != nil && *cur.lastSeq == 41
			}) {
				t.Errorf("75 s after the boot of %s past the cap, with no read in flight, INFRA's cursor is %s and C's URL served %d reads: the store read the boot held never found the daemon behind the setting's URL in that store",
					tc.store, cursorOf(t, rig.database, "INFRA"), c.served())
			}
		})
	}
}

// readsLockWaiters counts the statements of the rig's database that wait for a lock on
// legion_reads.
func readsLockWaiters(t *testing.T, rig *intakeRig) int {
	t.Helper()
	var waiters int
	if err := rig.database.Pool.QueryRow(t.Context(), `
		select count(*) from pg_locks
		where not granted and relation = 'legion_reads'::regclass
			and database = (select oid from pg_database where datname = current_database())
	`).Scan(&waiters); err != nil {
		t.Fatalf("count the waits for a lock on legion_reads: %v", err)
	}
	return waiters
}

// rowLockWaiters counts the statements of the rig's database that wait for a row another
// transaction has locked, as a transaction of judgeCrowded waits for a project's cursor that the
// record of a whole read has locked (lockCursor).
func rowLockWaiters(t *testing.T, rig *intakeRig) int {
	t.Helper()
	var waiters int
	if err := rig.database.Pool.QueryRow(t.Context(), `
		select count(*) from pg_locks l join pg_stat_activity a on a.pid = l.pid
		where not l.granted and l.locktype in ('transactionid', 'tuple') and a.datname = current_database()
	`).Scan(&waiters); err != nil {
		t.Fatalf("count the waits for a locked row: %v", err)
	}
	return waiters
}

// TestEventsPastTheCapWhileTheFirstReadOfAProjectWithNoCursorIsRecordedAreApplied: R3-h and the
// second invariant of read-rule-cases. A project with no cursor row is given one that follows no
// daemon by its starting read, before the read calls the daemon (Intake.readWhole, read-rule-cases
// row 7.6). Messages past the cap of stateMaxWaiting that judgeCrowded judges while that read's
// record is written record on that row the gap read they owe, its holder and the sequences they
// skipped. The record, which locks the row (lockCursor), must neither write over them nor miss
// them.
//
// INFRA has a setting, written after the take-over's settings read
// (startAfterTheTakeOversSettingsRead), and no cursor. Its first message makes the starting read,
// which serves 30, and the test holds the read's record in the database: a lock on legion_reads
// keeps the insert of the read's row waiting, after the record has read INFRA's position.
// Meanwhile 15 more messages at or below 30 wait on INFRA's runner, and 31 to 33 arrive past the
// cap, past the read's snapshot, near the end of the read's minute. Once 31 is judged beside the
// runner as far as it goes while the record is held - its transaction waits for INFRA's cursor,
// which the record locked - the lock is let go. INFRA then goes quiet: within the time left of the
// minute and 15 s, INFRA-1's phase reaches 33's reviewing.
//
// At d69f92b6, the record read no cursor and upserted one computed from none: it cleared the gap
// read, its holder and the skipped sequences that judgeCrowded recorded meanwhile. The holder at 31
// was then applied at its redelivery, and 32 and 33 were never read.
func TestEventsPastTheCapWhileTheFirstReadOfAProjectWithNoCursorIsRecordedAreApplied(t *testing.T) {
	// left is what is left of the starting read's minute when the messages past the cap arrive.
	const left = 5 * time.Second
	rig := newIntakeRig(t)
	startAfterTheTakeOversSettingsRead(t, rig)
	daemon := newFakeDaemon(t, daemonState("INFRA", 30, inPhase("implementing")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	var cursors int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from legion_event_cursors where project = 'INFRA'`).Scan(&cursors); err != nil || cursors != 0 {
		t.Fatalf("control: INFRA has %d cursors before its first message (%v), want none", cursors, err)
	}
	lock, err := rig.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("open the transaction that holds the read's record: %v", err)
	}
	t.Cleanup(func() { _ = lock.Rollback(context.Background()) })
	if _, err := lock.Exec(t.Context(), `lock table legion_reads in share mode`); err != nil {
		t.Fatalf("lock legion_reads: %v", err)
	}

	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.waitFor(20*time.Second, func() bool { return daemon.served() >= 1 && readsLockWaiters(t, rig) > 0 }, func() string {
		return fmt.Sprintf("control: the starting read's record does not wait on legion_reads: INFRA's daemon served %d reads", daemon.served())
	})
	// Any later read serves 33.
	daemon.serves(daemonState("INFRA", 33, inPhase("reviewing")))
	rig.clock.advance(ReadRetryAfter - left)
	for seq := 12; seq < 12+stateMaxWaiting-1; seq++ {
		rig.publish("INFRA", change(seq, "INFRA-1", moved("phase", "implementing")))
	}
	for seq, past := range []string{"testing", "merging", "reviewing"} {
		rig.publish("INFRA", change(31+seq, "INFRA-1", moved("phase", past)))
	}
	rig.waitFor(20*time.Second, func() bool {
		info := rig.consumerInfo()
		return info.NumPending == 0 && info.NumAckPending > stateMaxWaiting
	}, func() string {
		return fmt.Sprintf("control: INFRA holds %+v, want %d messages waiting on its runner and the messages past the cap", rig.consumerInfo(), stateMaxWaiting)
	})
	// 31 runs on INFRA's crowded key, and 32 and 33 wait behind it there. Its transaction waits for
	// INFRA's cursor, which the record locked (lockCursor); where the record held no lock on it, 31
	// would record what it owes and go back to the stream holding the gap read.
	rig.waitFor(20*time.Second, func() bool {
		return rowLockWaiters(t, rig) > 0 || len(rig.logRecords(heldLine)) > 0
	}, func() string {
		return fmt.Sprintf("control: the first message past the cap neither waits for INFRA's cursor nor went back to the stream: %d statements wait for a locked row, %d messages went back",
			rowLockWaiters(t, rig), len(rig.logRecords(heldLine)))
	})
	if err := lock.Rollback(t.Context()); err != nil {
		t.Fatalf("let the read's record go: %v", err)
	}
	rig.waitFor(left+15*time.Second, func() bool {
		return currentValues(t, rig.database, "INFRA")["INFRA-1/phase"] == `"reviewing"`
	}, func() string {
		return fmt.Sprintf("after the starting read at 30, INFRA-1's phase is %s, INFRA's cursor is %s and its daemon served %d reads: the events past the cap were never read",
			currentValues(t, rig.database, "INFRA")["INFRA-1/phase"], cursorOf(t, rig.database, "INFRA"), daemon.served())
	})
}
