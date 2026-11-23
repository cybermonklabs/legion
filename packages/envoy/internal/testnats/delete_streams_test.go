package testnats

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsGoWait is how long nats.go waits for the answer to a JetStream request whose context has no
// deadline (jetstream.go:461 and js.go:303 in v1.55.0), the wait the cleanup's deletes had until
// they waited until the test's deadline.
const natsGoWait = 5 * time.Second

// errorRecorder is t with each Errorf recorded rather than failing t, and with a deadline of its
// own, end, when end is set: a wait given it then ends halfway to end (testDeadline), as one of a
// test run with a short -timeout does.
type errorRecorder struct {
	testing.TB
	end time.Time

	mu     sync.Mutex
	errors []string
}

func (r *errorRecorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *errorRecorder) Deadline() (time.Time, bool) {
	if !r.end.IsZero() {
		return r.end, true
	}
	if timed, ok := r.TB.(interface{ Deadline() (time.Time, bool) }); ok {
		return timed.Deadline()
	}
	return time.Time{}, false
}

func (r *errorRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errors...)
}

// noAnswer is an answer's delay that holds the answer for good.
const noAnswer = time.Duration(-1)

// lateJetStream is a server whose JetStream is a client of the test's, as heldJetStream's is: it
// answers the account information Connect reads at once, answers the listing of the account's
// streams with one stream, LATE, after listAfter, and answers the delete of LATE after
// deleteAfter, each of them never when its delay is noAnswer. The channel it returns receives once
// the delete of LATE has reached it.
func lateJetStream(t *testing.T, listAfter, deleteAfter time.Duration) (uri string, deleteAsked <-chan struct{}) {
	t.Helper()
	server := start(t, "", nil)
	conn, err := natsgo.Connect(server.URL(), natsgo.NoReconnect())
	if err != nil {
		t.Fatalf("connect the JetStream stand-in: %v", err)
	}
	t.Cleanup(conn.Close)
	asked := make(chan struct{}, 1)
	answer := func(m *natsgo.Msg, after time.Duration, body string) {
		if after == noAnswer {
			return
		}
		time.AfterFunc(after, func() { _ = m.Respond([]byte(body)) })
	}
	if _, err := conn.Subscribe("$JS.API.>", func(m *natsgo.Msg) {
		switch m.Subject {
		case "$JS.API.INFO":
			answer(m, 0, `{"type":"io.nats.jetstream.api.v1.account_info_response"}`)
		case "$JS.API.STREAM.NAMES":
			answer(m, listAfter, `{"type":"io.nats.jetstream.api.v1.stream_names_response","total":1,"offset":0,"limit":1024,"streams":["LATE"]}`)
		case "$JS.API.STREAM.DELETE.LATE":
			select {
			case asked <- struct{}{}:
			default:
			}
			answer(m, deleteAfter, `{"type":"io.nats.jetstream.api.v1.stream_delete_response","success":true}`)
		}
	}); err != nil {
		t.Fatalf("subscribe the JetStream stand-in: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the JetStream stand-in: %v", err)
	}
	return server.URL(), asked
}

// The cleanup deletes every stream of the test's account on a real server, and says nothing.
func TestTheCleanupDeletesEveryStreamOfTheTestsAccount(t *testing.T) {
	uri := URL(t)
	conn := Connect(t, uri)
	defer conn.Close()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "PLAIN", Subjects: []string{"plain.>"}}); err != nil {
		t.Fatalf("create stream PLAIN: %v", err)
	}
	if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "bucket"}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	recorder := &errorRecorder{TB: t}
	deleteStreams(recorder, uri)
	if errs := recorder.recorded(); len(errs) != 0 {
		t.Fatalf("the cleanup failed: %q", errs)
	}
	names := js.StreamNames(ctx)
	var left []string
	for name := range names.Name() {
		left = append(left, name)
	}
	if err := names.Err(); err != nil {
		t.Fatalf("list the streams the cleanup left: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("the cleanup left streams %q", left)
	}
}

// The cleanup waits for the answer to a delete the server sends past nats.go's own wait for a
// JetStream request: at a load near 1,400 on 288 CPUs the shared server answered cmd/listener's
// deletes later than that, and the cleanup failed two tests that had passed.
func TestTheCleanupWaitsForADeleteAnsweredPastNatsGosOwnWait(t *testing.T) {
	lateDelete := natsGoWait + time.Second
	uri, deleteAsked := lateJetStream(t, 0, lateDelete)
	recorder := &errorRecorder{TB: t}
	begun := time.Now()
	deleteStreams(recorder, uri)
	took := time.Since(begun)
	t.Logf("the cleanup returned %s after it began; the stand-in answers the delete %s after it is asked", took, lateDelete)
	if errs := recorder.recorded(); len(errs) != 0 {
		t.Fatalf("the cleanup of a stream whose delete is answered %s late failed: %q", lateDelete, errs)
	}
	select {
	case <-deleteAsked:
	default:
		t.Fatal("the cleanup never asked the server to delete LATE")
	}
	if took < lateDelete {
		t.Fatalf("the cleanup returned after %s, before the delete's answer %s after it was asked", took, lateDelete)
	}
}

// A delete the server never answers fails the cleanup by name before the test's deadline, so a
// cleanup that waits until that deadline still ends by it.
func TestACleanupDeleteTheServerNeverAnswersFailsByNameInTime(t *testing.T) {
	const window = time.Second
	uri, deleteAsked := lateJetStream(t, 0, noAnswer)
	recorder := &errorRecorder{TB: t, end: time.Now().Add(2 * window)}
	begun := time.Now()
	deleteStreams(recorder, uri)
	took := time.Since(begun)
	t.Logf("the cleanup returned %s after it began, with the test's deadline %s after it began: %q", took, 2*window, recorder.recorded())
	errs := recorder.recorded()
	if len(errs) != 1 || !strings.Contains(errs[0], "delete the test's stream LATE: no answer by ") || !strings.Contains(errs[0], "short of the test's deadline") {
		t.Fatalf("a cleanup whose delete is never answered failed with %q, want one failure naming the stream and the test's deadline", errs)
	}
	select {
	case <-deleteAsked:
	default:
		t.Fatal("the cleanup never asked the server to delete LATE")
	}
	if took >= 2*window {
		t.Errorf("the cleanup failed after %s, past the test's deadline %s after it began", took, 2*window)
	}
}

// A listing the server never answers fails the cleanup by name before the test's deadline: a
// listing that ended with nothing listed once left every stream of the account in place, and the
// cleanup said nothing.
func TestACleanupListingTheServerNeverAnswersFailsByNameInTime(t *testing.T) {
	const window = time.Second
	uri, deleteAsked := lateJetStream(t, noAnswer, 0)
	recorder := &errorRecorder{TB: t, end: time.Now().Add(2 * window)}
	begun := time.Now()
	deleteStreams(recorder, uri)
	took := time.Since(begun)
	t.Logf("the cleanup returned %s after it began, with the test's deadline %s after it began: %q", took, 2*window, recorder.recorded())
	errs := recorder.recorded()
	if len(errs) != 1 || !strings.Contains(errs[0], "list the test's streams to delete them: no answer by ") || !strings.Contains(errs[0], "short of the test's deadline") {
		t.Fatalf("a cleanup whose listing is never answered failed with %q, want one failure naming the listing and the test's deadline", errs)
	}
	select {
	case <-deleteAsked:
		t.Error("the cleanup asked for a delete with no listing answered")
	default:
	}
	if took >= 2*window {
		t.Errorf("the cleanup failed after %s, past the test's deadline %s after it began", took, 2*window)
	}
}
