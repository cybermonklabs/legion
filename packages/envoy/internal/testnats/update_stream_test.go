package testnats

import (
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// updateReturnBound is how long TestAnUpdateSentWhileTheStreamElectsItsLeaderIsFoundByARead lets
// UpdateStream take from its call while the stream's group has no leader: half of jetStreamWait.
// No member answers an update while the group has no leader, so a helper that waits for the
// update's answer, before its read-back or after it, takes jetStreamWait or more, and one that
// reads from its send returns at the first read of the copy left running that holds the update, a
// read cut at updateReadWait; the bound sits far from both, and well below the fixed wait the row
// exists to exclude.
const updateReturnBound = jetStreamWait / 2

// An update sent while the stream's group has no leader is answered by no one, and UpdateStream
// must return from its read-back, within updateReturnBound, before the stopped members are
// recovered. The stream has three copies on a cluster of five, so the metadata group keeps its
// quorum, and applies the update, when two members stop. The row stops the stream's leader and
// one other copy, and goes on only once the copy left running answers a read naming no leader;
// when that copy took the leadership instead, it starts both again and chooses anew. It keeps the
// two stopped for twice jetStreamWait, past any helper that waits that wait out, so such a helper
// returns while the group still has no leader and fails the bound by name.
func TestAnUpdateSentWhileTheStreamElectsItsLeaderIsFoundByARead(t *testing.T) {
	const stream = "ELECTING"
	cluster := StartCluster(t, 5)
	cluster.CreateStream(t, natsgo.StreamConfig{Name: stream, Subjects: []string{"electing.>"}, Replicas: 3, Storage: natsgo.FileStorage})
	held := HoldStreamWithoutLeader(t, cluster, stream, updateReadWait)

	config := held.Info.Config
	config.MaxAge = time.Hour
	recorder := &fatalRecorder{TB: t}
	js := cluster.MemberJetStream(t, held.Kept)
	var info *natsgo.StreamInfo
	var took time.Duration
	returned := make(chan struct{})
	began := time.Now()
	go func() {
		defer close(returned)
		info = UpdateStream(recorder, js, config)
		took = time.Since(began)
	}()
	returnedBeforeRecovery := false
	select {
	case <-returned:
		returnedBeforeRecovery = true
	case <-time.After(2 * jetStreamWait):
	}
	held.Restart(t)
	settled := AwaitStreamCopies(t, cluster.JetStream(t), stream, 3)
	if !returnedBeforeRecovery {
		select {
		case <-returned:
		case <-time.After(2 * time.Minute):
			t.Fatalf("UpdateStream did not return within 2m of the members' start again")
		}
		t.Fatalf("UpdateStream did not return within %s of its call while stream %s had no leader", 2*jetStreamWait, stream)
	}
	if recorder.message != "" || info == nil {
		t.Fatalf("the update sent while %s had no leader failed: %q", stream, recorder.message)
	}
	t.Logf("UpdateStream returned %s after its call, while stream %s had no leader", took.Round(time.Millisecond), stream)
	if took >= updateReturnBound {
		t.Fatalf("UpdateStream returned %s after its call while stream %s had no leader, want under %s: an update no member answers is read back from its send, not waited on for JetStream's fixed %s",
			took.Round(time.Millisecond), stream, updateReturnBound, jetStreamWait)
	}
	if info.Config.MaxAge != time.Hour {
		t.Fatalf("UpdateStream returned %s with age %s, want 1h0m0s", stream, info.Config.MaxAge)
	}
	if settled.Config.MaxAge != time.Hour {
		t.Fatalf("once it has a leader again, %s holds age %s, want 1h0m0s", stream, settled.Config.MaxAge)
	}
}

// droppedUpdates sends no update of a stream at all and answers each with nats.go's timeout, as an
// update the server never applies would draw. Every other request is js's own.
type droppedUpdates struct {
	natsgo.JetStreamContext
	sends int
}

func (d *droppedUpdates) UpdateStream(*natsgo.StreamConfig, ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	d.sends++
	return nil, natsgo.ErrTimeout
}

// deadlineRecorder is fatalRecorder with a test binary deadline of its own.
type deadlineRecorder struct {
	*fatalRecorder
	deadline time.Time
}

func (d deadlineRecorder) Deadline() (time.Time, bool) { return d.deadline, true }

// An update that never lands fails the test once the test's deadline, less updateMargin, has
// passed, and not before, naming the stream and the setting no read held.
func TestAnUpdateThatNeverLandsFailsAtTheTestsDeadlineNamingTheStreamAndTheSetting(t *testing.T) {
	const stream, waited = "NEVER", 2 * time.Second
	conn := Connect(t, URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.AddStream(&natsgo.StreamConfig{Name: stream, Subjects: []string{"never.>"}})
	if err != nil {
		t.Fatalf("add stream: %v", err)
	}
	config := info.Config
	config.MaxAge = time.Hour
	dropped := &droppedUpdates{JetStreamContext: js}
	recorder := deadlineRecorder{fatalRecorder: &fatalRecorder{TB: t}, deadline: time.Now().Add(updateMargin + waited)}
	began := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		UpdateStream(recorder, dropped, config)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatalf("UpdateStream did not give up on an update that never lands within a minute of a deadline %s away", updateMargin+waited)
	}
	took := time.Since(began)
	if took < waited {
		t.Fatalf("UpdateStream gave up after %s, before the test's deadline less %s, %s away", took.Round(time.Millisecond), updateMargin, waited)
	}
	for _, want := range []string{"stream " + stream, "max_age: sent 1h0m0s, the stream holds 0s"} {
		if !strings.Contains(recorder.message, want) {
			t.Fatalf("UpdateStream failed with %q, want it to name %q", recorder.message, want)
		}
	}
	if dropped.sends != 1 {
		t.Fatalf("the update that never lands sent %d updates, want one", dropped.sends)
	}

}

// refusalWait is how long TestUpdateStreamReturnsTheAnswerAndFailsOnARefusal waits for its refusal:
// the server refuses an update of a stream that does not exist at once, before any read-back.
const refusalWait = 5 * time.Second

// A refused update fails the test at once with the server's answer, and an answered one returns
// that answer. The refusal's recorder has a deadline of refusalWait, so an UpdateStream that no
// longer fails on a refusal gives up on its read-back at that deadline and the row fails by name,
// not at go test's own timeout.
func TestUpdateStreamReturnsTheAnswerAndFailsOnARefusal(t *testing.T) {
	conn := Connect(t, URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.AddStream(&natsgo.StreamConfig{Name: "ANSWERED", Subjects: []string{"answered.>"}})
	if err != nil {
		t.Fatalf("add stream: %v", err)
	}
	config := info.Config
	config.MaxAge = time.Hour
	if got := UpdateStream(t, js, config); got.Config.MaxAge != time.Hour {
		t.Fatalf("UpdateStream returned age %s, want 1h0m0s", got.Config.MaxAge)
	}

	missing := config
	missing.Name = "MISSING"
	missing.Subjects = []string{"missing.>"}
	began := time.Now()
	recorder := deadlineRecorder{fatalRecorder: &fatalRecorder{TB: t}, deadline: began.Add(updateMargin + refusalWait)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		UpdateStream(recorder, js, missing)
	}()
	<-done
	if took := time.Since(began); took > refusalWait || !strings.Contains(recorder.message, "refused the update of stream MISSING") || !strings.Contains(recorder.message, "stream not found") {
		t.Fatalf("an update of a stream that does not exist failed after %s with %q, want within %s with the server's refusal", took.Round(time.Millisecond), recorder.message, refusalWait)
	}
}

// An update that takes away a subject the stream holds, with no subject overlapping it left in its
// place, is refused before it is sent, naming that subject, and the stream keeps it. The same
// narrowing with KeptByEveryKeeper's subject in that place is sent, and so is the bare narrowing
// marked ForTheKeeperToPutBack.
func TestANarrowingThatLeavesNothingInPlaceOfASubjectIsRefusedBeforeItIsSent(t *testing.T) {
	const stream = "NARROWED"
	conn := Connect(t, URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.AddStream(&natsgo.StreamConfig{Name: stream, Subjects: []string{"narrowed.kept.>", "narrowed.taken.>"}})
	if err != nil {
		t.Fatalf("add stream: %v", err)
	}
	bare := info.Config
	bare.Subjects = []string{"narrowed.kept.>"}
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		UpdateStream(recorder, js, bare)
	}()
	<-done
	if !strings.Contains(recorder.message, "stream "+stream+" takes away narrowed.taken.> and leaves no subject overlapping it") {
		t.Fatalf("the bare narrowing failed with %q, want it refused naming narrowed.taken.>", recorder.message)
	}
	if held, err := js.StreamInfo(stream); err != nil || !slices.Equal(held.Config.Subjects, info.Config.Subjects) {
		t.Fatalf("after the refused narrowing the stream reads %v (%v), want %v: it was sent", held, err, info.Config.Subjects)
	}

	kept := info.Config
	kept.Subjects = []string{"narrowed.kept.>", KeptByEveryKeeper("narrowed.taken.")}
	if got := UpdateStream(t, js, kept); !slices.Equal(got.Config.Subjects, kept.Subjects) {
		t.Fatalf("the narrowing that keeps a subject in place left %v, want %v", got.Config.Subjects, kept.Subjects)
	}
	forTheKeeper := kept
	forTheKeeper.Subjects = []string{"narrowed.kept.>"}
	keeperRecorder := &fatalRecorder{TB: t}
	done = make(chan struct{})
	go func() {
		defer close(done)
		UpdateStream(keeperRecorder, js, forTheKeeper, ForTheKeeperToPutBack)
	}()
	<-done
	if !strings.Contains(keeperRecorder.message, "ForTheKeeperToPutBack is reserved for TestTheKeeperRestoresWhatAnOperatorTookAwayAndKeepsAForeignSubject") {
		t.Fatalf("the non-keeper narrowing marked ForTheKeeperToPutBack failed with %q, want it refused by the keeper-only option", keeperRecorder.message)
	}
}

// namedRecorder is fatalRecorder reporting name as its test's name, as a row of that name would.
type namedRecorder struct {
	*fatalRecorder
	name string
}

func (n namedRecorder) Name() string { return n.name }

// ForTheKeeperToPutBack is the keeper row's by its whole name (allowsKeeperRestoration): a row whose
// name only begins with keeperRestorationRow is refused the option by name and its bare narrowing
// is not sent, while the keeper row's own is. A name read by its prefix let any row named after the
// keeper row narrow a stream with nothing in place, and both rows that pass the option today still
// passed, since each is either the keeper row itself or a name no prefix reaches.
func TestOnlyTheKeeperRowByItsWholeNameMayLeaveTheKeeperASubjectToPutBack(t *testing.T) {
	const stream = "KEEPER_NAMED"
	conn := Connect(t, URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.AddStream(&natsgo.StreamConfig{Name: stream, Subjects: []string{"named.kept.>", "named.taken.>"}})
	if err != nil {
		t.Fatalf("add stream: %v", err)
	}
	bare := info.Config
	bare.Subjects = []string{"named.kept.>"}
	update := func(name string) string {
		recorder := namedRecorder{fatalRecorder: &fatalRecorder{TB: t}, name: name}
		done := make(chan struct{})
		go func() {
			defer close(done)
			UpdateStream(recorder, js, bare, ForTheKeeperToPutBack)
		}()
		<-done
		return recorder.message
	}
	if message := update(keeperRestorationRow + "InNameOnly"); !strings.Contains(message, "ForTheKeeperToPutBack is reserved for "+keeperRestorationRow) {
		t.Fatalf("a row named %sInNameOnly passed ForTheKeeperToPutBack and failed with %q, want it refused by the keeper-only option", keeperRestorationRow, message)
	}
	if held, err := js.StreamInfo(stream); err != nil || !slices.Equal(held.Config.Subjects, info.Config.Subjects) {
		t.Fatalf("after the refused narrowing the stream reads %v (%v), want %v: it was sent", held, err, info.Config.Subjects)
	}
	if message := update(keeperRestorationRow); message != "" {
		t.Fatalf("the keeper row's own narrowing marked ForTheKeeperToPutBack failed with %q, want it sent", message)
	}
	if held, err := js.StreamInfo(stream); err != nil || !slices.Equal(held.Config.Subjects, bare.Subjects) {
		t.Fatalf("after the keeper row's narrowing the stream reads %v (%v), want %v", held, err, bare.Subjects)
	}
}

// lostGuardReadJetStream loses StreamInfo calls before it delegates them, as a read on a member
// whose stream group has just lost its leader does. UpdateStream must not send a narrowing until
// a guard read has answered.
type lostGuardReadJetStream struct {
	natsgo.JetStreamContext
	lost, reads int
}

func (js *lostGuardReadJetStream) StreamInfo(stream string, options ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	js.reads++
	if js.lost > 0 {
		js.lost--
		return nil, natsgo.ErrTimeout
	}
	return js.JetStreamContext.StreamInfo(stream, options...)
}

func TestANarrowingAfterALostGuardReadIsRefusedBeforeItIsSent(t *testing.T) {
	const stream = "LOST_GUARD_READ"
	conn := Connect(t, URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.AddStream(&natsgo.StreamConfig{Name: stream, Subjects: []string{"lost.kept.>", "lost.taken.>"}})
	if err != nil {
		t.Fatalf("add stream: %v", err)
	}
	bare := info.Config
	bare.Subjects = []string{"lost.kept.>"}
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		UpdateStream(recorder, &lostGuardReadJetStream{JetStreamContext: js, lost: 1}, bare)
	}()
	<-done
	if !strings.Contains(recorder.message, "stream "+stream+" takes away lost.taken.> and leaves no subject overlapping it") {
		t.Fatalf("the narrowing after a lost guard read failed with %q, want it refused naming lost.taken.>", recorder.message)
	}
	if held, err := js.StreamInfo(stream); err != nil || !slices.Equal(held.Config.Subjects, info.Config.Subjects) {
		t.Fatalf("after the refused narrowing the stream reads %v (%v), want %v: it was sent", held, err, info.Config.Subjects)
	}
}

func TestANonnarrowingUpdatePassesAfterOneGuardRead(t *testing.T) {
	const stream = "ONE_GUARD_READ"
	conn := Connect(t, URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.AddStream(&natsgo.StreamConfig{Name: stream, Subjects: []string{"one.guard.>"}})
	if err != nil {
		t.Fatalf("add stream: %v", err)
	}
	config := info.Config
	config.MaxAge = time.Hour
	// The guard is called on its own: UpdateStream's read-back after the send goes through the
	// same js, so a count taken around UpdateStream would count those reads too.
	guarded := &lostGuardReadJetStream{JetStreamContext: js}
	refuseSubjectsTakenWithNothingInPlace(t, updateDeadline(t), guarded, config)
	if guarded.reads != 1 {
		t.Fatalf("the guard of the nonnarrowing update made %d reads, want one answered read", guarded.reads)
	}
	if got := UpdateStream(t, guarded, config); got.Config.MaxAge != time.Hour {
		t.Fatalf("the nonnarrowing update returned age %s, want 1h0m0s", got.Config.MaxAge)
	}
}

func TestANarrowingWhoseGuardReadNeverAnswersFailsByName(t *testing.T) {
	const stream = "NEVER_GUARDED"
	conn := Connect(t, URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.AddStream(&natsgo.StreamConfig{Name: stream, Subjects: []string{"never.kept.>", "never.taken.>"}})
	if err != nil {
		t.Fatalf("add stream: %v", err)
	}
	bare := info.Config
	bare.Subjects = []string{"never.kept.>"}
	recorder := deadlineRecorder{
		fatalRecorder: &fatalRecorder{TB: t},
		deadline:      time.Now().Add(updateMargin + 2*updateReadInterval),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		UpdateStream(recorder, &lostGuardReadJetStream{JetStreamContext: js, lost: 100}, bare)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatalf("the narrowing of stream %s did not fail at the test's deadline", stream)
	}
	if want := "the update of stream " + stream + " was not sent"; !strings.Contains(recorder.message, want) || !strings.Contains(recorder.message, natsgo.ErrTimeout.Error()) {
		t.Fatalf("the narrowing whose guard read never answered failed with %q, want %q and %v: UpdateStream's own failure after a send says neither", recorder.message, want, natsgo.ErrTimeout)
	}
	if held, err := js.StreamInfo(stream); err != nil || !slices.Equal(held.Config.Subjects, info.Config.Subjects) {
		t.Fatalf("after the unanswered guard read the stream reads %v (%v), want %v: it was sent", held, err, info.Config.Subjects)
	}
}

func TestANarrowingAfterAWideningIsRefusedAgainstTheFirstSubjects(t *testing.T) {
	const stream = "NET_NARROWING"
	conn := Connect(t, URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.AddStream(&natsgo.StreamConfig{Name: stream, Subjects: []string{"net.taken.>"}})
	if err != nil {
		t.Fatalf("add stream: %v", err)
	}
	widened := info.Config
	widened.Subjects = []string{"net.>"}
	if got := UpdateStream(t, js, widened); !slices.Equal(got.Config.Subjects, widened.Subjects) {
		t.Fatalf("the widening left %v, want %v", got.Config.Subjects, widened.Subjects)
	}
	narrowed := widened
	narrowed.Subjects = []string{"net.bound.>"}
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		UpdateStream(recorder, js, narrowed)
	}()
	<-done
	if !strings.Contains(recorder.message, "stream "+stream+" takes away net.taken.> and leaves no subject overlapping it") {
		t.Fatalf("the net narrowing failed with %q, want it refused naming net.taken.>", recorder.message)
	}
}
