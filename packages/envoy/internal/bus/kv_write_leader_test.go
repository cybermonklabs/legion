package bus

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// sentInsideItsLimit returns the first send of sent, each send's start as a fake notes it, that
// came inside the limit of the send before it, with that gap and limit, judged one way only: send
// i+1 must come at least send i's limit after the last read before send i that the fake answered
// (answers), or after the row's start for the first send. The code reads its clock for send i's
// limit only after that read's answer, and starts send i+1 only once that limit has passed, so
// however late any send starts, a right limit is never judged short, and no allowance for
// scheduling is needed. A limit that came short by less than the time from that read's answer to
// the code's clock read gets through: the pause before every send after the first
// (bucketAppliedInterval, 25 ms) and the scheduling of that send. The limits are the run of
// writes' own: firstReadLimit, doubling up to limitCeiling.
func sentInsideItsLimit(sent, answers []time.Duration) (index int, gap, limit time.Duration, found bool) {
	limit = firstReadLimit
	for i := 0; i+1 < len(sent); i++ {
		var before time.Duration
		for _, answer := range answers {
			if answer < sent[i] && answer > before {
				before = answer
			}
		}
		if gap := sent[i+1] - before; gap < limit {
			return i + 1, gap, limit, true
		}
		limit = min(2*limit, limitCeiling)
	}
	return 0, 0, 0, false
}

// leaderReturnBound is how long after its leader is back a request that leader's members dropped
// meanwhile may land: one read period (firstReadLimit) and half of one. The code reads the leader
// a send waits on every read period (limitedWrite), and sends a copy of a read every read period
// (limitedRead), so the first read or copy sent after the return goes out within one read period
// of it; the half period is slack for the read-back, the pause before the send (25 ms,
// bucketAppliedInterval) and the scheduling between, which took at most 0.42 s in all in these
// rows. The bound tells the code apart from two faults: a leader read every two read periods,
// whose request lands up to two read periods after the return (1.04 s was seen), and a request
// waited out to its doubled limit, which landed at least 1.49 s after the return (3 s of absence,
// the in-flight send's limit 2.52 s from 1.94 s), and up to 9.58 s after it.
const leaderReturnBound = firstReadLimit + firstReadLimit/2

// leaderGone is a cluster one of whose leaders is gone until back, measured from began: the
// JetStream metadata leader, which answers a stream's update and a consumer's
// (jsStreamUpdateRequest and jsConsumerCreateRequest, v2.15.1 server/jetstream_api.go), or, when
// stream is set, the stream's own leader, which answers its purge (jsStreamPurgeRequest). Every
// write that leader answers sent before back is dropped, with no answer and no landing, as at a
// member that is not that leader, and each sent after it is applied and answered at once. The
// code's read of the leader it waits on (writeLeader) is the account's read while the metadata
// leader is gone: dropped, as at a member that is not that leader (jsAccountInfoRequest), or, from
// refuseAfter on when that is set, answered JetStream not available, as by a member that has seen
// its metadata group lose its leader (isLeaderless). While the stream's leader is gone, the code's
// read of it, a read of the stream, is answered with no leader in it, as by a member that sees the
// stream's group leaderless (jsStreamInfoRequest). Every other read, of the stream (a purge's
// read-back among them, filtered to a lane) and of its consumer, is answered at once. No server is
// behind it. A dropped request returns when the row ends (done), as nats.go's own wait would end
// it; its state is under mu, since a send the code abandons (Bounded) runs on beside the row.
type leaderGone struct {
	nats.JetStreamContext
	back        time.Duration
	refuseAfter time.Duration // when the account's read is answered with an error from, while back has not come; 0 for never
	stream      bool          // the stream's leader is gone, not the metadata leader
	done        <-chan struct{}

	mu       sync.Mutex
	began    time.Time
	config   nats.StreamConfig
	consumer nats.ConsumerConfig
	lane     uint64          // the messages on the role lane a purge empties
	writes   []time.Duration // when each write that leader answers went out
	answers  []time.Duration // when each read the write is read back by was answered
	reads    []time.Duration // when each read of the leader went out
	landed   time.Duration   // when the write that landed went out, or -1
}

func newLeaderGone(t *testing.T, back time.Duration) *leaderGone {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	return &leaderGone{
		back:     back,
		done:     done,
		config:   nats.StreamConfig{Name: "KV_envoy_leader_gone_test", MaxAge: time.Minute},
		consumer: nats.ConsumerConfig{Durable: "leader-gone", FilterSubject: "notifications.>"},
		lane:     3,
		landed:   -1,
	}
}

// start sets the clock the leader's absence is measured on.
func (js *leaderGone) start() {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.began = time.Now()
}

// taken notes a write going out, and reports when it went out and whether its leader took it; a
// write the leader did not take returns only when the row ends.
func (js *leaderGone) taken() (time.Duration, bool) {
	js.mu.Lock()
	at := time.Since(js.began)
	js.writes = append(js.writes, at)
	js.mu.Unlock()
	if at < js.back {
		<-js.done
		return at, false
	}
	return at, true
}

// readOfTheLeader notes a read of the leader going out and reports when it went out. mu is held.
func (js *leaderGone) readOfTheLeader() time.Duration {
	at := time.Since(js.began)
	js.reads = append(js.reads, at)
	return at
}

// answered notes a read-back's answer going back. mu is held.
func (js *leaderGone) answered() {
	if !js.began.IsZero() {
		js.answers = append(js.answers, time.Since(js.began))
	}
}

func (js *leaderGone) StreamInfo(_ string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	info := &nats.StreamInfo{Config: js.config}
	for _, opt := range opts {
		if request, ok := opt.(*nats.StreamInfoRequest); ok && request.SubjectsFilter != "" {
			if js.lane > 0 {
				info.State.Subjects = map[string]uint64{request.SubjectsFilter: js.lane}
			}
			js.answered()
			return info, nil
		}
	}
	if !js.stream {
		js.answered()
		return info, nil
	}
	info.Cluster = &nats.ClusterInfo{Leader: "n1"}
	if js.readOfTheLeader() < js.back {
		info.Cluster = &nats.ClusterInfo{}
	}
	return info, nil
}

func (js *leaderGone) ConsumerInfo(_, name string, _ ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.answered()
	return &nats.ConsumerInfo{Name: name, Config: js.consumer}, nil
}

func (js *leaderGone) AccountInfo(...nats.JSOpt) (*nats.AccountInfo, error) {
	js.mu.Lock()
	at := js.readOfTheLeader()
	js.mu.Unlock()
	switch {
	case js.stream || at >= js.back:
		return &nats.AccountInfo{}, nil
	case js.refuseAfter > 0 && at >= js.refuseAfter:
		return nil, &nats.APIError{Code: 503, ErrorCode: nats.JSErrCodeJetStreamNotAvailable, Description: "JetStream system temporarily unavailable"}
	}
	<-js.done
	return nil, nats.ErrTimeout
}

func (js *leaderGone) UpdateStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	at, ok := js.taken()
	if !ok {
		return nil, nats.ErrTimeout
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	js.config = *cfg
	js.land(at)
	return &nats.StreamInfo{Config: js.config}, nil
}

func (js *leaderGone) UpdateConsumer(_ string, cfg *nats.ConsumerConfig, _ ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	at, ok := js.taken()
	if !ok {
		return nil, nats.ErrTimeout
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	js.consumer = *cfg
	js.land(at)
	return &nats.ConsumerInfo{Config: js.consumer}, nil
}

func (js *leaderGone) PurgeStream(string, ...nats.JSOpt) error {
	at, ok := js.taken()
	if !ok {
		return nats.ErrTimeout
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	js.lane = 0
	js.land(at)
	return nil
}

// land notes the first write that landed, sent at at. mu is held.
func (js *leaderGone) land(at time.Duration) {
	if js.landed < 0 {
		js.landed = at
	}
}

// sent is when each write went out, when each read-back was answered, when each read of the
// leader went out, and when the write that landed went out (-1 when none did).
func (js *leaderGone) sent() ([]time.Duration, []time.Duration, []time.Duration, time.Duration) {
	js.mu.Lock()
	defer js.mu.Unlock()
	return slices.Clone(js.writes), slices.Clone(js.answers), slices.Clone(js.reads), js.landed
}

// leaderGoneWrites are the writes a start makes from a read, each run against js as a start runs
// it, with the leader it waits on gone as gone sets: the update of a stream that sets its age
// (updateStream) and the move of a consumer's filter off the role lanes (moveLegacyFilter), each
// waiting on the JetStream metadata leader, whose read is dropped; the update again, with the
// account's read answered with an error one second into the absence, as a member does once it sees
// the metadata group leaderless; and the purge of a role lane (purgeRoleLane), waiting on the
// stream's own leader, a read of the stream answered with no leader in it.
var leaderGoneWrites = []struct {
	name string
	gone func(js *leaderGone)
	run  func(ctx context.Context, js *leaderGone) error
}{
	{"update", func(*leaderGone) {}, updateTheAge},
	{"consumer move", func(*leaderGone) {}, func(ctx context.Context, js *leaderGone) error {
		return moveLegacyFilter(ctx, js, readLimitOf(ctx), writeLimitOf(ctx), js.consumer.Durable, []string{"notifications.github.>"})
	}},
	{"update the account read refuses", func(js *leaderGone) { js.refuseAfter = time.Second }, updateTheAge},
	{"purge", func(js *leaderGone) { js.stream, js.config.Replicas = true, 3 }, func(ctx context.Context, js *leaderGone) error {
		return purgeRoleLane(ctx, js, roleLaneSubjects[0])
	}},
}

// updateTheAge is the update of the stream js holds that sets its age, as a start's raise or age
// change is made (updateStream).
func updateTheAge(ctx context.Context, js *leaderGone) error {
	info, err := js.StreamInfo(js.config.Name)
	if err != nil {
		return err
	}
	_, err = updateStream(ctx, js, info,
		func(current *nats.StreamInfo) (nats.StreamConfig, bool) {
			aged := current.Config
			aged.MaxAge = time.Hour
			return aged, true
		},
		func(again *nats.StreamInfo) bool { return again.Config.MaxAge == time.Hour })
	return err
}

// A write the members dropped while its leader was gone lands within leaderReturnBound of that
// leader's return, within one read period and the read-back after it, not up to its whole limit
// later: a member that is not the JetStream metadata leader drops a stream's update and a
// consumer's with no answer, and one that is not a stream's leader drops its purge, so the write's
// limit doubles at each send, and a send waited out to its limit, 5.04 s and then 9 s, held the
// write that long after the leader was back. The write is waited for while the code reads its
// leader (metadataLeader, streamLeader) every read period, and a read the leader answers as there
// after one that drew no answer or that answered it is not there ends the wait on the send at once
// (limitedWrite). Here the leader is gone 3 s, 6 s and 10 s; a send waited out to its limit landed
// 1.49 s, 3.57 s and 9.58 s after the leader's return.
func TestAWriteTheLeaderDroppedLandsWithinAReadPeriodOfItsReturn(t *testing.T) {
	for _, write := range leaderGoneWrites {
		for _, back := range []time.Duration{3 * time.Second, 6 * time.Second, 10 * time.Second} {
			t.Run(write.name+"/"+back.String(), func(t *testing.T) {
				t.Parallel()
				js := newLeaderGone(t, back)
				write.gone(js)
				js.start()
				err := write.run(withReadLimit(t.Context()), js)
				writes, _, reads, landed := js.sent()
				if err != nil {
					t.Fatalf("the %s after its leader was gone %s = %v, want it landed; sent at %v", write.name, back, err, writes)
				}
				late := landed - back
				t.Logf("the %s landed %s after its leader was back, %d sends at %v, reads of the leader at %v",
					write.name, late.Round(time.Millisecond), len(writes), writes, reads)
				if late < 0 || late > leaderReturnBound {
					t.Fatalf("the %s landed %s after its leader was back (sent at %v, the leader read at %v), want within %s: a send waited out to its limit, or a leader read less often than every read period",
						write.name, late.Round(time.Millisecond), writes, reads, leaderReturnBound)
				}
			})
		}
	}
}

// With its leader gone the whole time, a write is still sent no faster than its own limit, the
// limit doubling from firstReadLimit up to limitCeiling (sentInsideItsLimit), and the reads of the
// leader that a send waiting past one read period makes (limitedWrite), none of which finds the
// leader there, send nothing early: neither a read that draws no answer nor one answered with an
// error or with no leader. The wait ends at its context after 12 s, with five sends, at about 0,
// 0.66, 1.94, 4.49 and 9.55 s.
func TestAWriteNoLeaderAnswersIsSentNoFasterThanItsLimit(t *testing.T) {
	for _, write := range leaderGoneWrites {
		t.Run(write.name, func(t *testing.T) {
			t.Parallel()
			js := newLeaderGone(t, time.Hour)
			write.gone(js)
			ctx, cancel := context.WithTimeout(withReadLimit(t.Context()), 12*time.Second)
			defer cancel()
			js.start()
			err := write.run(ctx, js)
			writes, answers, reads, landed := js.sent()
			t.Logf("the %s no leader answered: %d sends at %v, reads of the leader at %v, then %v", write.name, len(writes), writes, reads, err)
			if !errors.Is(err, context.DeadlineExceeded) || landed >= 0 {
				t.Fatalf("the %s no leader answered = %v (landed at %s), want it to end at its context with nothing landed", write.name, err, landed)
			}
			if len(reads) == 0 {
				t.Fatalf("the %s was sent %d times and the leader read none: no send waited past a read period", write.name, len(writes))
			}
			if i, gap, limit, inside := sentInsideItsLimit(writes, answers); inside {
				t.Fatalf("the %s was sent at %v: send %d came %s after the read before the send before it, inside that send's own limit, %s, while no leader answered",
					write.name, writes, i+1, gap.Round(time.Millisecond), limit)
			}
			if len(writes) != 5 {
				t.Fatalf("the %s was sent %d times in 12 s, at %v, want five: one for each limit, 630ms, 1.26s, 2.52s, 5.04s and then 9s, the last outlasting the wait", write.name, len(writes), writes)
			}
		})
	}
}

// droppedReads is a stream whose leader is gone until back, measured from began: every read of it
// sent before then is dropped, with no answer, as at a member that is not the stream's leader
// while it does not yet see the stream's group with no leader (jsStreamInfoRequest, v2.15.1
// server/jetstream_api.go:2100-2134), and each sent after it is answered at once. No server is
// behind it. A dropped read returns when the row ends (done).
type droppedReads struct {
	nats.JetStreamContext
	back time.Duration
	done <-chan struct{}

	mu    sync.Mutex
	began time.Time
	calls []time.Duration // when each read went out, copies of a read among them
}

func (js *droppedReads) StreamInfo(stream string, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.mu.Lock()
	at := time.Since(js.began)
	js.calls = append(js.calls, at)
	js.mu.Unlock()
	if at < js.back {
		<-js.done
		return nil, nats.ErrTimeout
	}
	return &nats.StreamInfo{Config: nats.StreamConfig{Name: stream}}, nil
}

func (js *droppedReads) sent() []time.Duration {
	js.mu.Lock()
	defer js.mu.Unlock()
	return slices.Clone(js.calls)
}

// readDroppedRead is a start's read of the stream js holds, sent again while unanswered
// (readAgain), as a start reads a bucket's stream.
func readDroppedRead(ctx context.Context, js *droppedReads) error {
	_, err := readAgain(ctx, "KV_envoy_dropped_reads_test STREAM.INFO", readLimitOf(ctx), func() (*nats.StreamInfo, error) {
		return js.StreamInfo("KV_envoy_dropped_reads_test")
	})
	return err
}

// A read the members dropped while the stream's leader was gone lands within leaderReturnBound of
// that leader's return, within one copy of it: while a read waits past the first limit of its run,
// a copy of it goes out every read period, and the first answer ends the wait (limitedRead). A read
// waited out to its doubled limit landed up to that whole limit after the leader was back: with no
// copies and a limit that doubled to ten seconds, a read landed 1.49 s, 3.56 s and 9.58 s after
// the return from absences of 3 s, 6 s and 10 s.
func TestAReadTheLeaderDroppedLandsWithinOneCopyOfItsReturn(t *testing.T) {
	for _, back := range []time.Duration{3 * time.Second, 6 * time.Second, 10 * time.Second} {
		t.Run(back.String(), func(t *testing.T) {
			t.Parallel()
			done := make(chan struct{})
			t.Cleanup(func() { close(done) })
			js := &droppedReads{back: back, done: done, began: time.Now()}
			err := readDroppedRead(withReadLimit(t.Context()), js)
			calls := js.sent()
			if err != nil {
				t.Fatalf("the read after the stream's leader was gone %s = %v, want it answered; sent at %v", back, err, calls)
			}
			first := slices.IndexFunc(calls, func(at time.Duration) bool { return at >= back })
			if first < 0 {
				t.Fatalf("the read was answered with no send after the leader was back, %s: sent at %v", back, calls)
			}
			late := calls[first] - back
			t.Logf("the read landed %s after the stream's leader was back, %d sends and copies at %v", late.Round(time.Millisecond), len(calls), calls)
			if late > leaderReturnBound {
				t.Fatalf("the read landed %s after the stream's leader was back (sent at %v), want within %s: a read waited out to its limit, not copied every read period",
					late.Round(time.Millisecond), calls, leaderReturnBound)
			}
		})
	}
}

// sendsBefore is the count of sends readAgain's error names (unansweredUntil).
var sendsBefore = regexp.MustCompile(`(\d+) sends? drew no answer`)

// With the stream's leader gone the whole time, a read is sent again on readAgain's own schedule,
// each send waited for up to its limit, the limit doubling from firstReadLimit up to limitCeiling:
// five sends in 12 s, at about 0, 0.66, 1.94, 4.49 and 9.55 s, each waiting past the first limit
// copied every read period besides (limitedRead). The wait ends at its context during the fifth,
// and its error names the four before it that drew no answer (unansweredUntil).
func TestAReadNoLeaderAnswersIsSentAgainOnItsLimitsAndCopied(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	js := &droppedReads{back: time.Hour, done: done, began: time.Now()}
	ctx, cancel := context.WithTimeout(withReadLimit(t.Context()), 12*time.Second)
	defer cancel()
	err := readDroppedRead(ctx, js)
	calls := js.sent()
	t.Logf("the read no leader answered: %d sends and copies at %v, then %v", len(calls), calls, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the read no leader answered = %v, want it to end at its context", err)
	}
	match := sendsBefore.FindStringSubmatch(err.Error())
	if match == nil {
		t.Fatalf("the read's error %q names no count of sends", err)
	}
	if sends, _ := strconv.Atoi(match[1]); sends != 4 {
		t.Fatalf("the read's error names %d sends that drew no answer in 12 s (%d sends and copies at %v), want four: one for each limit, 630ms, 1.26s, 2.52s and 5.04s, and a fifth under 9s in flight when the wait ended", sends, len(calls), calls)
	}
	if len(calls) <= 5 {
		t.Fatalf("the read went out %d times, at %v, want copies besides its five sends: a send waiting past one read period sent none", len(calls), calls)
	}
}

// A run's limit, of reads or of writes, doubles up to limitCeiling and no further, and sets to at
// most that after an answer that took longer, unless a test set a first limit above it
// (countedReads).
func TestALimitStopsGrowingAtTheLongestElectionTimeout(t *testing.T) {
	for name, run := range map[string]func(time.Duration) *readLimit{"reads": readRun, "writes": writeRun} {
		limit := run(firstReadLimit)
		for range 10 {
			limit.lost()
		}
		if limit.limit != limitCeiling {
			t.Fatalf("the run of %s after ten lost sends has a limit of %s, want %s", name, limit.limit, limitCeiling)
		}
		limit.answered(time.Minute)
		if limit.limit != limitCeiling {
			t.Fatalf("the run of %s after an answer that took a minute has a limit of %s, want %s", name, limit.limit, limitCeiling)
		}
		if counted := run(RequestTimeout); counted.most != RequestTimeout {
			t.Fatalf("the run of %s with a first limit of %s grows to %s, want %s", name, RequestTimeout, counted.most, RequestTimeout)
		}
	}
}

// slowLaneWrites applies and answers each consumer update and each purge writeDelay after its
// send, while it answers every read, of the stream, of its consumer and of the account, at once:
// the server of slowWritesJetStream, for the two other writes a start makes from a read. A purge
// empties the stream's messages on the lane it names. No server is behind it; sent is when each
// write went out and answers when each read-back was answered, measured from began.
type slowLaneWrites struct {
	nats.JetStreamContext
	began    time.Time
	mu       sync.Mutex
	consumer nats.ConsumerConfig
	lane     uint64
	sent     []time.Duration
	answers  []time.Duration
}

func (js *slowLaneWrites) AccountInfo(...nats.JSOpt) (*nats.AccountInfo, error) {
	return &nats.AccountInfo{}, nil
}

// StreamInfo answers at once; a read filtered to a lane is the purge's read-back and is noted, a
// read of the stream's leader (streamLeader) is not.
func (js *slowLaneWrites) StreamInfo(_ string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	info := &nats.StreamInfo{}
	for _, opt := range opts {
		if request, ok := opt.(*nats.StreamInfoRequest); ok && request.SubjectsFilter != "" {
			js.answers = append(js.answers, time.Since(js.began))
			if js.lane > 0 {
				info.State.Subjects = map[string]uint64{request.SubjectsFilter: js.lane}
			}
		}
	}
	return info, nil
}

func (js *slowLaneWrites) ConsumerInfo(_, name string, _ ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.answers = append(js.answers, time.Since(js.began))
	return &nats.ConsumerInfo{Name: name, Config: js.consumer}, nil
}

// note notes a write going out and holds it writeDelay.
func (js *slowLaneWrites) note() {
	js.mu.Lock()
	js.sent = append(js.sent, time.Since(js.began))
	js.mu.Unlock()
	time.Sleep(writeDelay)
}

func (js *slowLaneWrites) UpdateConsumer(_ string, cfg *nats.ConsumerConfig, _ ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	js.note()
	js.mu.Lock()
	defer js.mu.Unlock()
	js.consumer = *cfg
	return &nats.ConsumerInfo{Config: js.consumer}, nil
}

func (js *slowLaneWrites) PurgeStream(string, ...nats.JSOpt) error {
	js.note()
	js.mu.Lock()
	defer js.mu.Unlock()
	js.lane = 0
	return nil
}

// A consumer move and a purge the server answers later than the first limit, beside reads it
// answers at once, are each sent again only after their own limit (sentInsideItsLimit), as an
// update is (TestAWriteTheServerAnswersLaterThanItsReadsIsSentAgainOnlyAfterItsOwnLimit): each goes
// out under the run of writes (writeLimitOf), which a read's answer never sets back. Here each
// lands, and is answered, 2 s after its send, so it is sent at most three times, at about 0, 0.66
// and 1.94 s. A move or a purge sent under the run of reads went again after every read limit, at
// 0, 0.66, 1.31 and 1.97 s.
func TestAMoveAndAPurgeTheServerAnswersLaterThanItsReadsAreSentAgainOnlyAfterTheirOwnLimit(t *testing.T) {
	for _, write := range []struct {
		name string
		run  func(ctx context.Context, js *slowLaneWrites) error
	}{
		{"consumer move", func(ctx context.Context, js *slowLaneWrites) error {
			return moveLegacyFilter(ctx, js, readLimitOf(ctx), writeLimitOf(ctx), "slow-move", []string{"notifications.github.>"})
		}},
		{"purge", func(ctx context.Context, js *slowLaneWrites) error {
			return purgeRoleLane(ctx, js, roleLaneSubjects[0])
		}},
	} {
		t.Run(write.name, func(t *testing.T) {
			t.Parallel()
			js := &slowLaneWrites{consumer: nats.ConsumerConfig{Durable: "slow-move", FilterSubject: "notifications.>"}, lane: 3}
			ctx := withReadLimit(t.Context())
			js.mu.Lock()
			js.began = time.Now()
			js.mu.Unlock()
			if err := write.run(ctx, js); err != nil {
				t.Fatalf("the %s the server answers after %s = %v, want it landed", write.name, writeDelay, err)
			}
			js.mu.Lock()
			sent, answers := slices.Clone(js.sent), slices.Clone(js.answers)
			js.mu.Unlock()
			t.Logf("the %s the server answers after %s was sent %d times, at %v", write.name, writeDelay, len(sent), sent)
			if i, gap, limit, inside := sentInsideItsLimit(sent, answers); inside {
				t.Fatalf("the %s was sent at %v: send %d came %s after the read before the send before it, inside that send's own limit, %s", write.name, sent, i+1, gap.Round(time.Millisecond), limit)
			}
			if len(sent) > 3 {
				t.Fatalf("the %s was sent %d times, at %v, want at most three: one send for each write limit, 630ms, 1.26s and 2.52s, the last of which outlasts the %s answer", write.name, len(sent), sent, writeDelay)
			}
			if reads := readLimitOf(ctx).limit; reads != firstReadLimit {
				t.Fatalf("the run of reads ended at a limit of %s after reads answered at once, want %s: the writes set the reads' limit", reads, firstReadLimit)
			}
		})
	}
}
