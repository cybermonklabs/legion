package bus

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The keeper's lines about a bucket its grant refuses (keepBuckets).
const (
	keeperGrantRefusedMsg = "envoy nats keeper may not raise a bucket under its grant; it asks again after the backoff"
	keeperGrantCoversMsg  = "envoy nats keeper's grant now covers a bucket the server refused it"
	keeperRaiseFailedMsg  = "envoy nats keeper could not raise a bucket"
	keeperRaisedMsg       = "envoy nats keeper raised a bucket"
)

// keeperTestClock is a stepped client clock at a fixed start, which the keeper's no-grant backoff
// reads through WithReconnectErrClock (keepBuckets), so a row passes keeperNoGrantBackoff without
// waiting it.
func keeperTestClock() *steppedClock {
	return &steppedClock{now: time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)}
}

// deniedKeeper is a keeper of three copies on a three-member cluster whose NATS user may publish
// every subject but one, behind a relay in front of member 0 that counts the sends of one subject,
// which may be the denied one, with its own clock and the log captured.
type deniedKeeper struct {
	js     nats.JetStreamContext
	relay  *unansweredRelay
	clock  *steppedClock
	client *Client
	logs   *capturedLogs
}

// startDeniedKeeper creates buckets on a three-member cluster as an admin, then starts a keeper of
// them as a user denied denied, through a relay that counts the sends of counted (startUnansweredRelayOn).
func startDeniedKeeper(t *testing.T, denied, counted string, buckets ...string) *deniedKeeper {
	t.Helper()
	seed, user := testnats.User(t)
	_, admin := testnats.User(t)
	cluster := testnats.StartClusterConfig(t, 3, fmt.Sprintf(
		"authorization {\n  users = [ {nkey: %q, permissions: {publish: {allow: [\">\"], deny: [%q]}}}, {nkey: %q} ]\n}\nno_auth_user: %q\n",
		user, denied, admin, admin))
	k := &deniedKeeper{js: cluster.JetStream(t), clock: keeperTestClock()}
	adminConn := testnats.Connect(t, cluster.URL())
	t.Cleanup(adminConn.Close)
	for _, bucket := range buckets {
		cluster.CreateKeyValue(t, adminConn, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage})
	}
	k.logs = captureLogs()
	k.relay = startUnansweredRelayOn(t, cluster.Servers[0].URL(), counted)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := ConnectOwningStream([]string{k.relay.url}, WithReplicas(3), WithKeptBuckets(buckets...),
		WithKeepInterval(testKeepInterval), WithReconnectErrClock(k.clock.Now), WithLogger(k.logs.logger))
	if err != nil {
		t.Fatalf("start the keeper's client: %v", err)
	}
	t.Cleanup(client.Close)
	k.client = client
	return k
}

// A keeper whose NATS user may not read one of the buckets it keeps - the known-session bucket on
// a hub whose users' render predates its grant - asks the server about that bucket once per
// keeperNoGrantBackoff, not once per pass, and logs the refusal once, while it raises the bucket
// it may read each time an operator lowers it. The user's grant leaves out that bucket's
// STREAM.INFO alone, the request the server refused in production every 30 s; the relay counts the
// sends of that request, which the server never answers, so the count is the requests and not the
// answers. A send that took longer than its read limit to be refused is sent again by design
// (readAgain), so the count is read as its growth: none over several passes inside one backoff,
// some once the backoff has passed, none again after that.
func TestTheKeeperAsksABucketItsGrantRefusesOncePerBackoffAndRaisesTheOthersEachPass(t *testing.T) {
	const granted, refused = "envoy_keeper_granted", "envoy_keeper_refused"
	refusedRead := openSubject(refused)
	k := startDeniedKeeper(t, refusedRead, refusedRead, granted, refused)
	keepers := []*Client{k.client}

	// Three times over, the granted bucket is lowered and raised again, each raise a pass of its own.
	for round := range 3 {
		stream := "KV_" + granted
		if round > 0 {
			lowered := testnats.AwaitSettledStream(t, k.js, stream).Config
			lowered.Replicas = 1
			if _, err := k.js.UpdateStream(&lowered); err != nil {
				t.Fatalf("lower %s to one copy: %v", stream, err)
			}
		}
		awaitKept(t, k.js, stream, "three copies", func(info *nats.StreamInfo) bool { return info.Config.Replicas == 3 })
	}
	first := k.relay.sent()
	keeperPasses(t, keepers, 4)
	held := k.relay.sent()
	if first == 0 || held != first {
		t.Fatalf("sends of %s: %d after the granted bucket's three raises, %d after four passes more; want at least one and then none: one ask per backoff, not one per pass", refusedRead, first, held)
	}

	k.clock.Advance(keeperNoGrantBackoff)
	keeperPasses(t, keepers, 2)
	again := k.relay.sent()
	keeperPasses(t, keepers, 4)
	if after := k.relay.sent(); again <= held || after != again {
		t.Fatalf("sends of %s: %d before the backoff passed, %d two passes after it, %d four passes later; want more once it passed and then no more", refusedRead, held, again, after)
	}
	t.Logf("sends of %s: %d in the first backoff over the granted bucket's three raises and four passes, %d after the second ask", refusedRead, held, again)

	warnings := k.logs.recordsWithMsg(t, keeperGrantRefusedMsg)
	if len(warnings) != 1 || warnings[0]["bucket"] != refused || warnings[0]["probe_again_in"] != keeperNoGrantBackoff.String() {
		t.Fatalf("the keeper's refusal lines = %v, want one, for %s, naming the next ask in %s", warnings, refused, keeperNoGrantBackoff)
	}
	if failures := k.logs.recordsWithMsg(t, keeperRaiseFailedMsg); len(failures) != 0 {
		t.Fatalf("the keeper logged failed raises %v, want none: a refusal for want of a grant is its own line, once", failures)
	}
	if raises := k.logs.recordsWithMsg(t, keeperRaisedMsg); len(raises) < 3 {
		t.Fatalf("the keeper logged raises %v, want one for each of the granted bucket's three lowerings", raises)
	}
}

// A keeper whose NATS user may read a bucket but not update it - a grant with the bucket's
// STREAM.INFO and without its STREAM.UPDATE - logs the refused raise once, naming the update, and
// says nothing of a grant that covers the bucket once an operator has raised the bucket by hand:
// the keeper's next read finds it at its copies and sends no update, so the update is still
// refused. The relay counts the keeper's reads of the bucket, which the server answers, so the row
// knows the keeper asked again before it reads the log.
func TestTheKeeperClaimsNoGrantForAnUpdateItHasNotSentAgain(t *testing.T) {
	const refused = "envoy_keeper_update_refused"
	stream := "KV_" + refused
	k := startDeniedKeeper(t, updateSubject(refused), openSubject(refused), refused)

	warnings := testwait.Until(t, "the keeper's refusal line for "+refused, func() ([]map[string]any, bool) {
		records := k.logs.recordsWithMsg(t, keeperGrantRefusedMsg)
		return records, len(records) > 0
	})
	want := `Permissions Violation for Publish to "` + updateSubject(refused) + `"`
	if text, _ := warnings[0]["error"].(string); warnings[0]["bucket"] != refused || !strings.Contains(text, want) {
		t.Fatalf("the keeper's refusal line = %v, want it for %s naming %s", warnings[0], refused, want)
	}

	raised := testnats.AwaitSettledStream(t, k.js, stream).Config
	raised.Replicas = 3
	if _, err := k.js.UpdateStream(&raised); err != nil {
		t.Fatalf("raise %s to three copies as an operator: %v", stream, err)
	}
	awaitKept(t, k.js, stream, "three copies", func(info *nats.StreamInfo) bool { return info.Config.Replicas == 3 })
	before := k.relay.sent()
	k.clock.Advance(keeperNoGrantBackoff)
	testwait.Until(t, "the keeper to read "+stream+" again once the backoff passed", func() (int, bool) {
		sent := k.relay.sent()
		return sent, sent > before
	})
	keeperPasses(t, []*Client{k.client}, 2)

	if covers := k.logs.recordsWithMsg(t, keeperGrantCoversMsg); len(covers) != 0 {
		t.Fatalf("the keeper said its grant now covers %s (%v), though the update it was refused has not been sent since", refused, covers)
	}
	if lines := k.logs.recordsWithMsg(t, keeperGrantRefusedMsg); len(lines) != 1 {
		t.Fatalf("the keeper's refusal lines = %v, want the one", lines)
	}
	if failures := k.logs.recordsWithMsg(t, keeperRaiseFailedMsg); len(failures) != 0 {
		t.Fatalf("the keeper logged failed raises %v, want none", failures)
	}
}

// answeringJetStream is a JetStream context for the keeper's bucket pass (keepBuckets): it answers
// the read of each bucket's stream with the error set for it, or with the stream at its copies
// (three unless set), and an update with the error set for it, or by taking the update's copies. It
// counts the reads and the updates of each stream. The keeper sends it nothing else.
type answeringJetStream struct {
	nats.JetStreamContext
	mu        sync.Mutex
	copies    map[string]int
	readErr   map[string]error
	updateErr map[string]error
	reads     map[string]int
	updates   map[string]int
}

func newAnsweringJetStream() *answeringJetStream {
	return &answeringJetStream{copies: map[string]int{}, readErr: map[string]error{}, updateErr: map[string]error{},
		reads: map[string]int{}, updates: map[string]int{}}
}

func (js *answeringJetStream) info(stream string) *nats.StreamInfo {
	copies, set := js.copies[stream]
	if !set {
		copies = 3
	}
	return &nats.StreamInfo{Config: nats.StreamConfig{Name: stream, Replicas: copies}}
}

func (js *answeringJetStream) StreamInfo(stream string, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.reads[stream]++
	if err := js.readErr[stream]; err != nil {
		return nil, err
	}
	return js.info(stream), nil
}

func (js *answeringJetStream) UpdateStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.updates[cfg.Name]++
	if err := js.updateErr[cfg.Name]; err != nil {
		return nil, err
	}
	js.copies[cfg.Name] = cfg.Replicas
	return js.info(cfg.Name), nil
}

// set changes how js answers stream from now on: its copies (0 leaves them), its read's error and
// its update's error.
func (js *answeringJetStream) set(stream string, copies int, readErr, updateErr error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if copies > 0 {
		js.copies[stream] = copies
	}
	js.readErr[stream] = readErr
	js.updateErr[stream] = updateErr
}

// readRefusal and updateRefusal are the errors a client's read or update of bucket's stream ends
// with when the server refuses it under the connection's grant: the server's violation, named
// ErrPublishDenied (deniedPublishError).
func readRefusal(bucket string) error { return grantRefusalOf(openSubject(bucket)) }

func updateRefusal(bucket string) error { return grantRefusalOf(updateSubject(bucket)) }

func grantRefusalOf(subject string) error {
	return deniedPublishError(subject, fmt.Errorf("%w: Permissions Violation for Publish to %q", nats.ErrPermissionViolation, subject))
}

// The server's own answers to a request, which end an episode on that request as a success does.
var (
	notEnabledAnswer   = &nats.APIError{Code: 503, ErrorCode: 10039, Description: "jetstream not enabled for account"}
	insufficientAnswer = &nats.APIError{Code: 500, ErrorCode: nats.JSErrCodeInsufficientResourcesErr, Description: "insufficient resources"}
)

// keeperState is what a row of a bucket pass reads: the reads and the updates of the refused
// bucket's stream, and the counts of the keeper's lines about its buckets.
type keeperState struct {
	reads, updates                     int
	refusals, failures, covers, raises int
}

// keeperPassRow is a row over a keeper's bucket pass (keepBuckets) on a stand-in JetStream, with
// the record of refusals a keeper keeps across its passes (keep) and its clock.
type keeperPassRow struct {
	t       *testing.T
	js      *answeringJetStream
	clock   *steppedClock
	logs    *capturedLogs
	stream  string
	buckets []string
	client  *Client
	refused refusedBuckets
}

// newKeeperPassRow is a row whose bucket pass keeps buckets at three copies, and whose reads are of
// bucket's stream.
func newKeeperPassRow(t *testing.T, bucket string, buckets ...string) *keeperPassRow {
	t.Helper()
	clock, logs := keeperTestClock(), captureLogs()
	return &keeperPassRow{t: t, js: newAnsweringJetStream(), clock: clock, logs: logs, stream: "KV_" + bucket,
		buckets: buckets, client: &Client{name: "keeper-grant-test", stopCh: make(chan struct{}), reconnectErrClock: clock.Now, logger: logs.logger},
		refused: refusedBuckets{}}
}

// pass runs one bucket pass.
func (r *keeperPassRow) pass() {
	r.client.keepBuckets(r.t.Context(), r.js, r.buckets, 3, r.refused)
}

// reads counts the reads of bucket's stream.
func (r *keeperPassRow) reads(bucket string) int {
	r.js.mu.Lock()
	defer r.js.mu.Unlock()
	return r.js.reads["KV_"+bucket]
}

// want fails the row unless its stream's reads and updates and the keeper's lines are want.
func (r *keeperPassRow) want(when string, want keeperState) {
	r.t.Helper()
	r.js.mu.Lock()
	got := keeperState{reads: r.js.reads[r.stream], updates: r.js.updates[r.stream]}
	r.js.mu.Unlock()
	count := func(message string) int { return len(r.logs.recordsWithMsg(r.t, message)) }
	got.refusals, got.failures = count(keeperGrantRefusedMsg), count(keeperRaiseFailedMsg)
	got.covers, got.raises = count(keeperGrantCoversMsg), count(keeperRaisedMsg)
	if got != want {
		r.t.Fatalf("%s: %+v, want %+v", when, got, want)
	}
}

// A bucket whose read the server refuses for want of a grant is asked once per keeperNoGrantBackoff,
// to the nanosecond, and its refusal logged once with the time to the next ask; the bucket beside
// it is asked every pass.
func TestTheKeeperAsksABucketItsGrantRefusesOncePerBackoff(t *testing.T) {
	const granted, refused = "envoy_granted", "envoy_refused"
	r := newKeeperPassRow(t, refused, granted, refused)
	r.js.set(r.stream, 0, readRefusal(refused), nil)

	for range 4 {
		r.pass()
	}
	r.want("over four passes", keeperState{reads: 1, refusals: 1})
	if got := r.reads(granted); got != 4 {
		t.Fatalf("reads of the granted bucket over four passes = %d, want 4: a refusal holds only its own bucket", got)
	}
	r.clock.Advance(keeperNoGrantBackoff - time.Nanosecond)
	r.pass()
	r.want("a nanosecond before the backoff passed", keeperState{reads: 1, refusals: 1})
	r.clock.Advance(time.Nanosecond)
	r.pass()
	r.want("once the backoff passed", keeperState{reads: 2, refusals: 1})
	r.pass()
	r.want("on the pass after the second refusal, which holds the bucket again", keeperState{reads: 2, refusals: 1})

	warning := r.logs.recordsWithMsg(t, keeperGrantRefusedMsg)[0]
	if warning["bucket"] != refused || warning["probe_again_in"] != keeperNoGrantBackoff.String() {
		t.Fatalf("the keeper's refusal line = %v, want it for %s, naming the next ask in %s", warning, refused, keeperNoGrantBackoff)
	}
}

// The ask that ends a read refusal's backoff logs what it met: a failure that is no answer from the
// server its own line, and again on the next pass, which asks again since nothing refused it for
// want of a grant; an answer one line that the grant now covers the bucket, once; and a refusal
// after that answer starts a new episode, with its line again.
func TestTheKeeperLogsTheAskThatEndsABucketsBackoff(t *testing.T) {
	const granted, refused = "envoy_granted", "envoy_refused"
	r := newKeeperPassRow(t, refused, granted, refused)

	r.js.set(r.stream, 0, readRefusal(refused), nil)
	r.pass()
	r.want("after the refusal", keeperState{reads: 1, refusals: 1})

	r.js.set(r.stream, 0, errors.New("nats: connection closed"), nil)
	r.clock.Advance(keeperNoGrantBackoff)
	r.pass()
	r.want("after a failure that is no answer, at the end of the backoff", keeperState{reads: 2, refusals: 1, failures: 1})
	if failure := r.logs.recordsWithMsg(t, keeperRaiseFailedMsg)[0]; failure["bucket"] != refused {
		t.Fatalf("the failure line = %v, want it for %s", failure, refused)
	}
	r.pass()
	r.want("on the pass after that failure", keeperState{reads: 3, refusals: 1, failures: 2})

	r.js.set(r.stream, 0, nil, nil)
	r.pass()
	r.want("after the answer", keeperState{reads: 4, refusals: 1, failures: 2, covers: 1})
	if covered := r.logs.recordsWithMsg(t, keeperGrantCoversMsg)[0]; covered["bucket"] != refused {
		t.Fatalf("the grant line = %v, want it for %s", covered, refused)
	}
	r.pass()
	r.want("on the pass after the answer", keeperState{reads: 5, refusals: 1, failures: 2, covers: 1})

	r.js.set(r.stream, 0, readRefusal(refused), nil)
	r.pass()
	r.want("after a refusal once more", keeperState{reads: 6, refusals: 2, failures: 2, covers: 1})
	r.pass()
	r.want("on the pass after that refusal", keeperState{reads: 6, refusals: 2, failures: 2, covers: 1})
	if got := r.reads(granted); got != 7 {
		t.Fatalf("reads of the granted bucket over seven passes = %d, want 7", got)
	}
}

// A refusal ends at any answer the server gives the request it refused, an API error included: the
// read's refusal at the answer that the bucket is not there, and at an API error, which logs its
// failure too; the update's refusal at an API error to the update, which logs its failure too. A
// refusal of another request than the episode's is logged again, naming it.
func TestARefusalEndsAtAnyAnswerTheServerGivesTheRefusedRequest(t *testing.T) {
	const refused = "envoy_refused"
	r := newKeeperPassRow(t, refused, refused)

	r.js.set(r.stream, 0, readRefusal(refused), nil)
	r.pass()
	r.want("after the read's refusal", keeperState{reads: 1, refusals: 1})
	r.js.set(r.stream, 0, nats.ErrStreamNotFound, nil)
	r.clock.Advance(keeperNoGrantBackoff)
	r.pass()
	r.want("after the answer that the bucket is not there", keeperState{reads: 2, refusals: 1, covers: 1})

	r.js.set(r.stream, 0, readRefusal(refused), nil)
	r.pass()
	r.want("after the read's refusal again", keeperState{reads: 3, refusals: 2, covers: 1})
	r.js.set(r.stream, 0, notEnabledAnswer, nil)
	r.clock.Advance(keeperNoGrantBackoff)
	r.pass()
	r.want("after an API error to the read", keeperState{reads: 4, refusals: 2, failures: 1, covers: 2})

	r.js.set(r.stream, 1, nil, updateRefusal(refused))
	r.pass()
	r.want("after the update's refusal", keeperState{reads: 5, updates: 1, refusals: 3, failures: 1, covers: 2})
	if text, _ := r.logs.recordsWithMsg(t, keeperGrantRefusedMsg)[2]["error"].(string); !strings.Contains(text, updateSubject(refused)) {
		t.Fatalf("the third refusal line names %q, want the update %s", text, updateSubject(refused))
	}
	r.js.set(r.stream, 0, nil, insufficientAnswer)
	r.clock.Advance(keeperNoGrantBackoff)
	r.pass()
	r.want("after an API error to the update", keeperState{reads: 6, updates: 2, refusals: 3, failures: 2, covers: 3})

	r.js.set(r.stream, 0, nil, updateRefusal(refused))
	r.pass()
	r.want("after the update's refusal again", keeperState{reads: 7, updates: 3, refusals: 4, failures: 2, covers: 3})
	r.js.set(r.stream, 0, readRefusal(refused), updateRefusal(refused))
	r.clock.Advance(keeperNoGrantBackoff)
	r.pass()
	r.want("after the read's refusal while the update's episode runs", keeperState{reads: 8, updates: 3, refusals: 5, failures: 2, covers: 3})
	if text, _ := r.logs.recordsWithMsg(t, keeperGrantRefusedMsg)[4]["error"].(string); !strings.Contains(text, openSubject(refused)) {
		t.Fatalf("the fifth refusal line names %q, want the read %s", text, openSubject(refused))
	}
}

// A refused update - a grant with the bucket's STREAM.INFO and without its STREAM.UPDATE - ends only
// at an answer to an update. A read that finds the bucket at its copies, after an operator's raise,
// sends no update and claims no grant, and neither does an answer that the bucket is not there;
// such a read holds nothing back either, so the next pass reads again; once the bucket is lowered
// the update is refused again without a line; and the update the server answers once the grant
// carries it logs the grant line and the raise.
func TestAnUpdateRefusalEndsOnlyAtAnAnsweredUpdate(t *testing.T) {
	const granted, refused = "envoy_granted", "envoy_refused"
	r := newKeeperPassRow(t, refused, granted, refused)

	r.js.set(r.stream, 1, nil, updateRefusal(refused))
	r.pass()
	r.want("after the update's refusal", keeperState{reads: 1, updates: 1, refusals: 1})
	r.pass()
	r.want("on the pass after it, inside the backoff", keeperState{reads: 1, updates: 1, refusals: 1})

	r.js.set(r.stream, 3, nil, updateRefusal(refused))
	r.clock.Advance(keeperNoGrantBackoff)
	r.pass()
	r.want("after the operator's raise", keeperState{reads: 2, updates: 1, refusals: 1})
	r.pass()
	r.want("on the pass after the operator's raise", keeperState{reads: 3, updates: 1, refusals: 1})

	r.js.set(r.stream, 0, nats.ErrStreamNotFound, updateRefusal(refused))
	r.pass()
	r.want("after an answer that the bucket is not there", keeperState{reads: 4, updates: 1, refusals: 1})

	r.js.set(r.stream, 1, nil, updateRefusal(refused))
	r.pass()
	r.want("after the bucket was lowered again", keeperState{reads: 5, updates: 2, refusals: 1})
	r.pass()
	r.want("on the pass after that refusal", keeperState{reads: 5, updates: 2, refusals: 1})

	r.js.set(r.stream, 0, nil, nil)
	r.clock.Advance(keeperNoGrantBackoff)
	r.pass()
	r.want("after the update the server answered", keeperState{reads: 6, updates: 3, refusals: 1, covers: 1, raises: 1})
	if raised := r.logs.recordsWithMsg(t, keeperRaisedMsg)[0]; raised["bucket"] != refused || raised["replicas"] != "1 -> 3" {
		t.Fatalf("the raise line = %v, want %s raised 1 -> 3", raised, refused)
	}
	r.pass()
	r.want("on the pass after the raise", keeperState{reads: 7, updates: 3, refusals: 1, covers: 1, raises: 1})
	if got := r.reads(granted); got != 9 {
		t.Fatalf("reads of the granted bucket over nine passes = %d, want 9", got)
	}
}
