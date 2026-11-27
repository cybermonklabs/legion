package testnats

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// updateReturnBound is how long TestAnUpdateSentWhileTheStreamElectsItsLeaderIsFoundByARead lets
// UpdateStream take from its call while the stream's group has no leader: half of jetStreamWait.
// No member answers an update while the group has no leader, so a helper that waits for the
// update's answer, before its read-back or after it, takes jetStreamWait or more, and one that
// reads from its send returns at the first read of the copy left running that holds the update, a
// read cut at firstUpdateReadWait or at what that copy's answers set (updateReadLimit); the bound
// sits far from both, and well below the fixed wait the row
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
	held := HoldStreamWithoutLeader(t, cluster, stream, firstUpdateReadWait)

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

// firstUpdateAnswered answers the first update of a stream with first, sending nothing; every later
// update, and every other request, is js's own.
type firstUpdateAnswered struct {
	natsgo.JetStreamContext
	first *natsgo.APIError
	sends atomic.Int32
}

func (f *firstUpdateAnswered) UpdateStream(cfg *natsgo.StreamConfig, opts ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	if f.sends.Add(1) == 1 {
		return nil, f.first
	}
	return f.JetStreamContext.UpdateStream(cfg, opts...)
}

// leaderlessAnswer is the 503 a member answers while it knows no leader of the metadata group
// (JSClusterNotAvailErr), and insufficientResources a 503 that is a refusal (JSInsufficientResourcesErr),
// as nats-server v2.15.1 writes them.
var (
	leaderlessAnswer      = natsgo.APIError{Code: http.StatusServiceUnavailable, ErrorCode: 10008, Description: "JetStream system temporarily unavailable"}
	insufficientResources = natsgo.APIError{Code: http.StatusServiceUnavailable, ErrorCode: 10023, Description: "insufficient resources"}
)

// resentWait is how long TestAnUpdateAnsweredWithA503IsSentAgainAndLands gives UpdateStream before
// its recorder's deadline less updateMargin: a helper that took the 503 as a refusal fails at once,
// and one that never sent the update again gives up on its read-back here, so the row fails by
// name either way.
const resentWait = 10 * time.Second

// An update answered with a 503, which no member applied, is sent again, one read wait later, and
// lands; every other refusal still fails at once (TestUpdateStreamReturnsTheAnswerAndFailsOnARefusal).
func TestAnUpdateAnsweredWithA503IsSentAgainAndLands(t *testing.T) {
	conn := Connect(t, URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.AddStream(&natsgo.StreamConfig{Name: "LEADERLESS", Subjects: []string{"leaderless.>"}})
	if err != nil {
		t.Fatalf("add stream: %v", err)
	}
	config := info.Config
	config.MaxAge = time.Hour
	answer := leaderlessAnswer
	leaderless := &firstUpdateAnswered{JetStreamContext: js, first: &answer}
	recorder := deadlineRecorder{fatalRecorder: &fatalRecorder{TB: t}, deadline: time.Now().Add(updateMargin + resentWait)}
	var got *natsgo.StreamInfo
	done := make(chan struct{})
	go func() {
		defer close(done)
		got = UpdateStream(recorder, leaderless, config)
	}()
	<-done
	if recorder.message != "" || got == nil || got.Config.MaxAge != time.Hour {
		t.Fatalf("an update first answered with a 503 failed with %q and returned %+v, want it sent again and returned with age 1h0m0s", recorder.message, got)
	}
	if sends := leaderless.sends.Load(); sends != 2 {
		t.Fatalf("the update first answered with a 503 was sent %d times, want twice", sends)
	}
}

// An update answered with a 503 that is not the leaderless one, insufficient resources (10023), is
// a refusal: it fails the test at once with the server's answer, and is not sent again.
func TestAnUpdateAnsweredWithAnother503IsARefusal(t *testing.T) {
	conn := Connect(t, URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := js.AddStream(&natsgo.StreamConfig{Name: "INSUFFICIENT", Subjects: []string{"insufficient.>"}})
	if err != nil {
		t.Fatalf("add stream: %v", err)
	}
	config := info.Config
	config.MaxAge = time.Hour
	answer := insufficientResources
	refused := &firstUpdateAnswered{JetStreamContext: js, first: &answer}
	began := time.Now()
	recorder := deadlineRecorder{fatalRecorder: &fatalRecorder{TB: t}, deadline: began.Add(updateMargin + refusalWait)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		UpdateStream(recorder, refused, config)
	}()
	<-done
	if took := time.Since(began); took > refusalWait || !strings.Contains(recorder.message, "refused the update of stream INSUFFICIENT") || !strings.Contains(recorder.message, "insufficient resources") {
		t.Fatalf("an update answered 503 insufficient resources failed after %s with %q, want within %s with the server's refusal", took.Round(time.Millisecond), recorder.message, refusalWait)
	}
	if sends := refused.sends.Load(); sends != 1 {
		t.Fatalf("the refused update was sent %d times, want once", sends)
	}
}

// A read the server leaves unanswered, or answers with a 503, doubles the next read's wait, up to
// jetStreamWait; a read it answers sets the next to three times that read's own time, never under
// firstUpdateReadWait.
func TestUpdateStreamsReadWaitDoublesAfterALostReadAndFollowsAnAnsweredOne(t *testing.T) {
	limit := &updateReadLimit{limit: firstUpdateReadWait}
	limit.lost()
	if limit.limit != 2*firstUpdateReadWait {
		t.Fatalf("after one lost read the next waits %s, want %s", limit.limit, 2*firstUpdateReadWait)
	}
	for range 10 {
		limit.lost()
	}
	if limit.limit != jetStreamWait {
		t.Fatalf("after eleven lost reads the next waits %s, want jetStreamWait %s", limit.limit, jetStreamWait)
	}
	limit.answered(100 * time.Millisecond)
	if limit.limit != firstUpdateReadWait {
		t.Fatalf("after a read answered in 100ms the next waits %s, want firstUpdateReadWait %s", limit.limit, firstUpdateReadWait)
	}
	limit.answered(time.Second)
	if limit.limit != 3*time.Second {
		t.Fatalf("after a read answered in 1s the next waits %s, want 3s", limit.limit)
	}
	for _, lost := range []error{natsgo.ErrTimeout, context.DeadlineExceeded, &natsgo.APIError{Code: http.StatusServiceUnavailable, ErrorCode: 10008}} {
		if !readLost(lost) {
			t.Fatalf("readLost(%v) = false, want a read that came to nothing", lost)
		}
	}
	for _, answered := range []error{natsgo.ErrStreamNotFound, &insufficientResources} {
		if readLost(answered) {
			t.Fatalf("readLost(%v) = true, want an answer", answered)
		}
	}
}

// answerLost sends each update and drops its answer, as an update applied whose answer never came.
type answerLost struct{ natsgo.JetStreamContext }

func (a answerLost) UpdateStream(cfg *natsgo.StreamConfig, opts ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	_, _ = a.JetStreamContext.UpdateStream(cfg, opts...)
	return nil, natsgo.ErrTimeout
}

// slowLinkWait is how long TestAReadBackOverALinkSlowerThanTheFirstReadWaitFindsTheUpdate gives
// UpdateStream before its recorder's deadline less updateMargin: a read-back whose wait never grows
// past the link's round trip reads nothing for that long and fails the row by name.
const slowLinkWait = 40 * time.Second

// Every round trip through the link takes 1.2 s, longer than UpdateStream's first read wait, and the
// update's answer is lost, so only a read-back whose wait grows past 1.2 s after its lost reads
// (updateReadLimit) finds the update. A read-back that keeps firstUpdateReadWait cuts every read
// before its answer and gives up at slowLinkWait.
func TestAReadBackOverALinkSlowerThanTheFirstReadWaitFindsTheUpdate(t *testing.T) {
	uri := URL(t)
	direct := Connect(t, uri)
	t.Cleanup(direct.Close)
	directJS, err := direct.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	info, err := directJS.AddStream(&natsgo.StreamConfig{Name: "SLOWLINK", Subjects: []string{"slowlink.>"}})
	if err != nil {
		t.Fatalf("add stream: %v", err)
	}
	proxy := StartProxy(t, uri, ProxyLatency(1200*time.Millisecond))
	conn, err := natsgo.Connect(proxy.URL(), natsgo.Timeout(30*time.Second), natsgo.NoReconnect())
	if err != nil {
		t.Fatalf("connect through the slow link: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatal(err)
	}
	config := info.Config
	config.MaxAge = time.Hour
	recorder := deadlineRecorder{fatalRecorder: &fatalRecorder{TB: t}, deadline: time.Now().Add(updateMargin + slowLinkWait)}
	var got *natsgo.StreamInfo
	began := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		got = UpdateStream(recorder, answerLost{js}, config)
	}()
	<-done
	t.Logf("UpdateStream returned %s after its call over the 1.2s link", time.Since(began).Round(time.Millisecond))
	if recorder.message != "" || got == nil || got.Config.MaxAge != time.Hour {
		t.Fatalf("an update whose answer was lost, read back over a 1.2s link, failed with %q and returned %+v, want it found with age 1h0m0s", recorder.message, got)
	}
}
