package bus

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Every nats-server before v2.12.5 can refuse the later of two updates of one stream as
// overlapping an existing stream, the stream itself, when the other update lands while the server
// checks this one (updateStream); that is how TestTwoStartsRaisingOneStreamAtOnceBothSucceed
// failed on v2.10.29. Such an update is settled from the stream: one that, read again, holds what
// the caller asked for is done, one that does not is sent again, and one refused on every send, as
// a stream another stream really does overlap is, returns the refusal after overlapSends sends. An
// unanswered update is settled from the stream the same way and sent again while the stream does
// not hold it, each update sent again built from the read before it, so it carries what another
// writer changed since; any other refusal is returned at once.
func TestAnUpdateRefusedAsOverlappingItsOwnStreamIsSettledFromTheStream(t *testing.T) {
	t.Parallel()
	overlap := &nats.APIError{Code: 400, ErrorCode: jsStreamSubjectOverlap, Description: "subjects overlap with an existing stream"}
	otherRefusal := &nats.APIError{Code: 503, ErrorCode: 10023, Description: "insufficient resources"}
	for _, test := range []struct {
		name string
		// the server's answer to each send in turn, nil once the answers run out
		answers []error
		// the other program's update lands with the first answer that is not nil, giving the
		// stream what this update asks for
		landed bool
		// another writer sets the stream's byte limit with the first answer that is not nil, so
		// every send after it must carry that limit
		otherWrite bool
		sends      int
		reads      int
		want       error
	}{
		{name: "the other update gave the stream what this one asks", answers: []error{overlap}, landed: true, sends: 1, reads: 1},
		{name: "the stream read again does not hold it yet", answers: []error{overlap}, sends: 2, reads: 1},
		{name: "refused, then accepted on the last send", answers: slices.Repeat([]error{overlap}, overlapSends-1), sends: overlapSends, reads: overlapSends - 1},
		{name: "refused on every send", answers: slices.Repeat([]error{overlap}, overlapSends+1), sends: overlapSends, reads: overlapSends, want: overlap},
		{name: "refused while another writer changed the stream", answers: []error{overlap}, otherWrite: true, sends: 2, reads: 1},
		{name: "unanswered, and the stream does not hold it", answers: []error{nats.ErrTimeout}, sends: 2, reads: 1},
		{name: "unanswered twice, then accepted", answers: []error{nats.ErrTimeout, nats.ErrTimeout}, sends: 3, reads: 2},
		{name: "unanswered while another writer changed the stream", answers: []error{nats.ErrTimeout}, otherWrite: true, sends: 2, reads: 1},
		{name: "unanswered, and the stream holds it", answers: []error{nats.ErrTimeout}, landed: true, sends: 1, reads: 1},
		{name: "refused otherwise", answers: []error{otherRefusal}, sends: 1, want: otherRefusal},
		{name: "refused otherwise while the stream holds the update", answers: []error{otherRefusal}, landed: true, sends: 1, want: otherRefusal},
	} {
		t.Run(test.name, func(t *testing.T) {
			js := &scriptedUpdateJetStream{config: nats.StreamConfig{Name: Stream, Replicas: 1}, answers: test.answers, landed: test.landed, otherWrite: test.otherWrite}
			update := nats.StreamConfig{Name: Stream, Replicas: 3}

			_, err := updateStream(countedReads(t.Context()), js, &nats.StreamInfo{Config: js.config}, raisedToThree, func(again *nats.StreamInfo) bool { return again.Config.Replicas >= 3 })

			if !errors.Is(err, test.want) {
				t.Fatalf("updateStream = %v, want %v", err, test.want)
			}
			sent, reads := js.counts()
			if len(sent) != test.sends || reads != test.reads {
				t.Fatalf("updateStream sent %d updates and read the stream %d times, want %d and %d", len(sent), reads, test.sends, test.reads)
			}
			for i, got := range sent {
				want := update
				if test.otherWrite && i > 0 {
					want.MaxBytes = otherWriterMaxBytes
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("update %d = %+v, want %+v: the caller's change of the stream as the read before it found it", i+1, got, want)
				}
			}
		})
	}
}

// raisedToThree is the change of the rows above: the stream as a read found it, at three copies.
func raisedToThree(current *nats.StreamInfo) (nats.StreamConfig, bool) {
	raised := current.Config
	raised.Replicas = 3
	return raised, true
}

// otherWriterMaxBytes is the byte limit another writer gives the stream in a row of the table
// above, between this update's sends.
const otherWriterMaxBytes = 7 << 20

// An update the server never answers is sent again, each send waiting the caller's JetStream wait,
// until ctx ends, and the step then ends naming the update and ctx's cause: only the caller's
// context, which every caller bounds by a hang guard, ends it, and no send's own wait does.
func TestAnUpdateTheServerNeverAnswersIsSentAgainUntilTheWaitEnds(t *testing.T) {
	js := &scriptedUpdateJetStream{config: nats.StreamConfig{Name: Stream, Replicas: 1}, neverAnswers: true}
	ctx, cancel := context.WithTimeout(countedReads(t.Context()), 300*time.Millisecond)
	defer cancel()

	_, err := updateStream(ctx, js, &nats.StreamInfo{Config: js.config}, raisedToThree, func(again *nats.StreamInfo) bool { return again.Config.Replicas >= 3 })

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an update the server never answers = %v, want it to end with the wait's context.DeadlineExceeded", err)
	}
	if sent, _ := js.counts(); len(sent) < 2 {
		t.Fatalf("updateStream sent %d updates, want the update sent again while the server leaves it unanswered", len(sent))
	}
}

// An update that drew no answer is not sent again onto a stream a newer build stamped in the
// meantime: the start and the keeper leave a newer build's stream exactly as they find it on a
// first read (ensureStreamWithConfig, keepStream), and so on every read after a lost update. The
// row plays a rolling release: this build's first update is lost, and while it is in flight a
// newer build writes the stream, stamped with a higher generation and a 96 h age. Had this build
// sent its update again from that read, the stream would carry this build's generation and its
// 72 h age again.
func TestALostUpdateIsNotSentAgainOntoANewerBuildsStream(t *testing.T) {
	for _, row := range []struct {
		name string
		run  func(t *testing.T, js nats.JetStreamContext, cfg *nats.StreamConfig) error
	}{
		{"a start", func(t *testing.T, js nats.JetStreamContext, cfg *nats.StreamConfig) error {
			_, err := ensureStreamWithConfig(countedReads(t.Context()), js, cfg, capsAnUnsetStream, atStart)
			return err
		}},
		{"a keeper pass", func(t *testing.T, js nats.JetStreamContext, cfg *nats.StreamConfig) error {
			restored, err := keepStream(countedReads(t.Context()), js, cfg, capsAnUnsetStream)
			if len(restored) != 0 {
				t.Errorf("the keeper reported %v restored, want nothing: it wrote nothing that landed", restored)
			}
			return err
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			js := streamTestJetStream(t)
			own := parseConnectOptions([]ConnectOption(nil)).streamConfig()
			if _, err := ensureStreamWithConfig(t.Context(), js, &own, capsAnUnsetStream, atStart); err != nil {
				t.Fatalf("this build creates the stream: %v", err)
			}
			// A byte limit the stream lacks, so this build's start and keeper each have an update to
			// make.
			limited := parseConnectOptions([]ConnectOption{WithStreamMaxBytes(keeperByteLimit)}).streamConfig()
			lost := &newerWriteDuringLostUpdateJetStream{JetStreamContext: js}

			if err := row.run(t, lost, &limited); err != nil {
				t.Fatalf("%s whose update was lost while a newer build wrote the stream = %v, want nil", row.name, err)
			}
			got := deployedStream(t, js).Config
			if want := strconv.Itoa(streamConfigGeneration + 1); got.Metadata[streamGenerationKey] != want || got.MaxAge != newerBuildMaxAge {
				t.Fatalf("the stream carries generation %q and age %s, want the newer build's %s and %s", got.Metadata[streamGenerationKey], got.MaxAge, want, newerBuildMaxAge)
			}
			if updates := lost.updates.Load(); updates != 1 {
				t.Fatalf("%s sent %d updates, want 1: the lost one, and none onto the newer build's stream", row.name, updates)
			}
		})
	}
}

// newerBuildMaxAge is the age the newer build of TestALostUpdateIsNotSentAgainOntoANewerBuildsStream
// writes, one this build never sets.
const newerBuildMaxAge = 96 * time.Hour

// newerWriteDuringLostUpdateJetStream loses the answer to the first update sent through it, and
// while that update is in flight a newer build writes the stream: its generation above this
// build's and its own age. Every update is counted, the lost one included.
type newerWriteDuringLostUpdateJetStream struct {
	nats.JetStreamContext
	updates atomic.Int32
}

func (js *newerWriteDuringLostUpdateJetStream) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if js.updates.Add(1) > 1 {
		return js.JetStreamContext.UpdateStream(cfg, opts...)
	}
	info, err := js.JetStreamContext.StreamInfo(cfg.Name)
	if err != nil {
		return nil, err
	}
	newer := info.Config
	newer.MaxAge, newer.Duplicates = newerBuildMaxAge, time.Minute
	newer.Metadata = maps.Clone(info.Config.Metadata)
	newer.Metadata[streamGenerationKey] = strconv.Itoa(streamConfigGeneration + 1)
	if _, err := js.JetStreamContext.UpdateStream(&newer); err != nil {
		return nil, fmt.Errorf("the newer build's write: %w", err)
	}
	return nil, nats.ErrTimeout
}

// scriptedUpdateJetStream answers each stream update with the next of answers, applying the update
// when that answer is nil, and every read with the stream as it holds it, counting the reads. With
// landed, the other program's update lands with the first answer that is not nil, and it is this
// update itself: the stream then holds what the caller asked for without the caller's own send
// having been applied. With otherWrite, another writer sets the stream's byte limit with the first
// answer that is not nil. With neverAnswers, every update is unanswered. Its state is under mu,
// since an update a wait abandons (Bounded) runs on beside the test.
type scriptedUpdateJetStream struct {
	nats.JetStreamContext
	mu           sync.Mutex
	config       nats.StreamConfig
	answers      []error
	landed       bool
	otherWrite   bool
	neverAnswers bool
	sent         []nats.StreamConfig
	reads        int
}

func (js *scriptedUpdateJetStream) UpdateStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.sent = append(js.sent, *cfg)
	if js.neverAnswers {
		return nil, nats.ErrTimeout
	}
	var answer error
	if len(js.answers) > 0 {
		answer = js.answers[0]
		js.answers = js.answers[1:]
	}
	if answer == nil {
		js.config = *cfg
		return &nats.StreamInfo{Config: js.config}, nil
	}
	if js.landed {
		js.config = *cfg
		js.landed = false
	}
	if js.otherWrite {
		js.config.MaxBytes = otherWriterMaxBytes
		js.otherWrite = false
	}
	return nil, answer
}

func (js *scriptedUpdateJetStream) StreamInfo(string, ...nats.JSOpt) (*nats.StreamInfo, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.reads++
	return &nats.StreamInfo{Config: js.config}, nil
}

// counts is the updates js was sent and the reads it answered so far.
func (js *scriptedUpdateJetStream) counts() ([]nats.StreamConfig, int) {
	js.mu.Lock()
	defer js.mu.Unlock()
	return slices.Clone(js.sent), js.reads
}

// On a real three-server cluster, concurrent reconciles of one stream all end with the stream
// holding what each asked for. Three clients, one per member, send the same update at once, round
// after round: on nats-server v2.10.29 a run of these rounds meets the overlap refusal above (one
// in a few hundred sends), and with it settled from the stream no round fails and the stream holds
// every round's byte limit. The rounds are the measurement, so they are many and each is cheap: no
// round restarts the cluster.
func TestConcurrentUpdatesOfOneStreamEndWithTheStreamHoldingThem(t *testing.T) {
	t.Parallel()
	const rounds = 300
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(3))
	deployed := testnats.AwaitStreamCopies(t, js, Stream, 3).Config

	members := make([]nats.JetStreamContext, 0, len(cluster.URLs()))
	for i, url := range cluster.URLs() {
		memberJS, err := testnats.Connect(t, url).JetStream()
		if err != nil {
			t.Fatalf("member %d JetStream: %v", i, err)
		}
		members = append(members, memberJS)
	}

	for round := range rounds {
		update := deployed
		update.MaxBytes = testStreamMaxBytes + int64(round+1)
		errs := make([]error, len(members))
		var wg sync.WaitGroup
		for i, member := range members {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sent := update
				_, errs[i] = updateStream(countedReads(t.Context()), member, &nats.StreamInfo{Config: deployed}, func(current *nats.StreamInfo) (nats.StreamConfig, bool) {
					changed := current.Config
					changed.MaxBytes = sent.MaxBytes
					return changed, true
				}, func(again *nats.StreamInfo) bool {
					return again.Config.MaxBytes >= update.MaxBytes
				})
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d, the update of member %d: %v", round, i, err)
			}
		}
		if got := deployedStream(t, js).Config.MaxBytes; got != update.MaxBytes {
			t.Fatalf("round %d left the stream's byte limit at %d, want the %d every update asked for", round, got, update.MaxBytes)
		}
	}
}
