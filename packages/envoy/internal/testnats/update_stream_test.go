package testnats

import (
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
