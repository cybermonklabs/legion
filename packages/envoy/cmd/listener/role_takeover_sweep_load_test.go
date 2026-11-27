package main

// The load the kept-role sweep's takeover passes put on the stream's leader: what one pass reads,
// read off the wire, and how long a record's passes keep running.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// takeoverWire reads, off the wire, the JetStream API requests sent in a row's account: a connection
// of the row's own subscribes every API subject, and each stored-message read's body says what it
// reads. A mark a listener's connection publishes after a pass reaches the same channel after every
// request that connection sent before it, so a count read up to the mark holds the pass's whole.
type takeoverWire struct {
	t        *testing.T
	messages chan *natsgo.Msg
	mark     string
}

// takeoverReads is what a listener's connection sent between two marks: previous, the
// stored-message reads of an agent subject from a sequence on, which only a takeover pass makes of
// a previous holder's subject; records, the reads of a takeover record's newest message
// (bus.Client.TakeoverRecords); consumers, the reads of a session consumer's info, the grant's
// probe among them (bus.SessionConsumers.ProbeConsumerCalls); and all, every JetStream API request
// of the account, some another connection's.
type takeoverReads struct {
	previous, records, consumers, all int
}

func watchTakeoverWire(t *testing.T, url string) *takeoverWire {
	t.Helper()
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	wire := &takeoverWire{t: t, messages: make(chan *natsgo.Msg, 1<<16), mark: "takeoverwire.mark." + id.New()}
	for _, subject := range []string{"$JS.API.>", wire.mark} {
		if _, err := conn.ChanSubscribe(subject, wire.messages); err != nil {
			t.Fatalf("subscribe the wire reading to %s: %v", subject, err)
		}
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the wire reading's subscriptions: %v", err)
	}
	return wire
}

// count is what the wire carried since the last count, read up to a mark client publishes now.
func (w *takeoverWire) count(client *bus.Client) takeoverReads {
	w.t.Helper()
	return w.countAfter(func() error {
		if err := client.Conn.Publish(w.mark, nil); err != nil {
			return err
		}
		return client.Conn.Flush()
	})
}

// countAfter is what the wire carried since the last count, read up to the mark that publishMark
// publishes, on the connection whose requests the count is of.
func (w *takeoverWire) countAfter(publishMark func() error) takeoverReads {
	w.t.Helper()
	if err := publishMark(); err != nil {
		w.t.Fatalf("publish the wire mark: %v", err)
	}
	var reads takeoverReads
	for {
		select {
		case msg := <-w.messages:
			if msg.Subject == w.mark {
				return reads
			}
			reads.all++
			if strings.HasPrefix(msg.Subject, "$JS.API.CONSUMER.INFO."+bus.Stream+".") {
				reads.consumers++
				continue
			}
			if msg.Subject != "$JS.API.STREAM.MSG.GET."+bus.Stream {
				continue
			}
			var read struct {
				Seq  uint64 `json:"seq"`
				Next string `json:"next_by_subj"`
				Last string `json:"last_by_subj"`
			}
			if err := json.Unmarshal(msg.Data, &read); err != nil {
				w.t.Fatalf("decode a stored-message read %q: %v", msg.Data, err)
			}
			switch {
			case read.Seq > 0 && strings.HasPrefix(read.Next, "notifications.agent."):
				reads.previous++
			case strings.HasPrefix(read.Last, bus.TakeoverSubjectPrefix):
				reads.records++
			}
		case <-time.After(30 * time.Second):
			w.t.Fatal("the wire mark did not come back within 30 s")
		}
	}
}

// burstRecords writes n takeover records as production's sweeps find them: each of a role
// prefix-<i mod roles>, taken from a session of its own whose agent subject holds one message to
// that session alone, claimed age before the rig's clock, with Next one past that message, as the
// pass that read to the subject's end leaves it. With pastNext, each previous holder's subject also
// holds a forward of a role message of the record's role at Next, which a pass copies. It returns
// the roles.
func (rig *crashRig) burstRecords(prefix string, n, roles int, age time.Duration, pastNext bool) []string {
	rig.t.Helper()
	js, err := rig.lane.JetStream()
	if err != nil {
		rig.t.Fatalf("open JetStream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	names := make([]string, roles)
	for i := range names {
		names[i] = prefix + "-" + strconv.Itoa(i)
	}
	for i := range n {
		role, previous := names[i%roles], "ses_"+prefix+"_"+strconv.Itoa(i)
		subject := contracts.LegacyAgentSubject(previous)
		data, err := json.Marshal(newEnvelope("agent", subject, "to the session itself"))
		if err != nil {
			rig.t.Fatal(err)
		}
		ack, err := js.Publish(subject, data)
		if err != nil {
			rig.t.Fatalf("store a message to %s: %v", previous, err)
		}
		if pastNext {
			rig.lateForward(previous, role, "a role message past the record's Next")
		}
		record := bus.TakeoverRecord{Role: role, Previous: previous, ClaimedAt: rig.clock.Now().Add(-age).UnixMilli(), Next: ack.Sequence + 1}
		if err := rig.harness.client.WriteTakeoverRecord(ctx, bus.TakeoverSubject(role, previous, id.New()), record); err != nil {
			rig.t.Fatalf("write the takeover record of %s from %s: %v", role, previous, err)
		}
	}
	return names
}

// The sweep's burst: each pass of each shared listener read every takeover record's previous holder
// past Next, for the record's whole life, and each such read of a subject holding nothing new is a
// 404 the stream's leader answers and counts. 600 records whose copy window ended, as most of
// production's had, cost one such read each on the pass that ends them (takeoverRecordLife), one read
// of each finished record on the pass after it, and nothing on the next: a pass reads no previous
// holder of a finished record, and the client reads a finished record again only when its subject's
// message count moves, which no age-limit removal does within the row (bus.Client.TakeoverRecords).
//
// The control is 600 records inside their copy window, each previous holder's subject holding a role
// message past Next: the pass reads each subject - the forward, then the end - and copies every
// message to its role's kept lane, so the wire reading sees a pass's reads when it makes them.
func TestASweepPassReadsNoPreviousHolderOfARecordPastItsLife(t *testing.T) {
	t.Parallel()
	const records, roles = 600, 60
	t.Run("600 records past their copy window: once a pass ended them, the passes read no previous holder", func(t *testing.T) {
		rig := newCrashRig(t)
		rig.burstRecords("burst", records, roles, 2*time.Hour, false)
		wire := watchTakeoverWire(t, rig.url)
		cfg := rig.cfg()
		pass := func() takeoverReads {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			sweepOnce(ctx, cfg)
			return wire.count(cfg.client)
		}
		first, second, third := pass(), pass(), pass()
		t.Logf("API requests per pass at %d records: first %+v, second %+v, third %+v", records, first, second, third)
		if first.previous < records {
			t.Fatalf("the first pass read %d previous holders' subjects, want each of the %d at least once", first.previous, records)
		}
		if second.previous != 0 || third.previous != 0 {
			t.Fatalf("the passes after the first read %d and %d previous holders' subjects, want none: a record past its copy window is ended by the pass that reads its subject to the end", second.previous, third.previous)
		}
		if third.records != 0 {
			t.Fatalf("the third pass read %d takeover records, want none: each finished record was read once, by the second", third.records)
		}
	})
	t.Run("control: 600 records inside their copy window, each with a role message past Next: the pass reads every previous holder and copies each message", func(t *testing.T) {
		rig := newCrashRig(t)
		names := rig.burstRecords("young", records, roles, 10*time.Minute, true)
		wire := watchTakeoverWire(t, rig.url)
		cfg := rig.cfg()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		sweepOnce(ctx, cfg)
		reads := wire.count(cfg.client)
		t.Logf("API requests of the pass at %d young records: %+v", records, reads)
		if reads.previous < 2*records {
			t.Fatalf("the pass read %d previous holders' subjects, want at least %d: the forward and the end of each", reads.previous, 2*records)
		}
		copies := 0
		for _, role := range names {
			copies += len(rig.kept(role))
		}
		if copies != records {
			t.Fatalf("the kept lanes hold %d copies, want one of each of the %d forwards", copies, records)
		}
	})
}

// A takeover record's passes ran for 71 hours, the stream's age limit less one, and every pass of
// every shared listener read its previous holder past Next all that time. A record ends now within
// its life, one hour, and one sweep interval, while two listeners' sweeps keep running every
// interval; no pass writes it after it ended. A forward that resolved the previous holder before the
// claim and is stored inside the fence (roles.ForwardFenceAge) is still copied to the new holder,
// which is what the record's hour is for.
func TestATakeoverRecordEndsWithinItsLifeWhilePassesKeepRunning(t *testing.T) {
	t.Parallel()
	rig := newCrashRig(t)
	const a, b, role = "ses_life_a", "ses_life_b", "life"
	// bound is the record's life and one sweep interval; lateAt, when the row stores its late
	// forward, is inside the fence after the claim and before the record's hour ends.
	const bound, lateAt = time.Hour + roleWakeInterval, 50 * time.Minute
	rig.gone(a)
	rig.claim(rig.away, a, role)
	first := rig.sendTo(a, role, 1, "before the takeover")
	next := rig.relayed(b, answerDone)
	rig.claim(rig.mux, b, role)
	crashAwaitEvents(t, next, first, nil, "the new holder, from the claim's own pass")
	rig.idle(rig.cfg(), role)
	var ended atomic.Bool
	var afterEnd atomic.Int32
	setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{beforeRecordWrite: func(r, previous string, _ uint64, finished bool) {
		if r != role || previous != a {
			return
		}
		if ended.Load() {
			afterEnd.Add(1)
		}
		if finished {
			ended.Store(true)
		}
	}})
	listeners := []listenerDeliveryHandlerConfig{rig.cfg(), rig.anotherListener()}
	var late string
	var endedBy time.Duration
	for elapsed := roleWakeInterval; elapsed <= bound+3*roleWakeInterval; elapsed += roleWakeInterval {
		rig.clock.advance(roleWakeInterval)
		if elapsed == lateAt {
			late = rig.lateForward(a, role, "a forward that resolved the previous holder before the claim")
		}
		for _, cfg := range listeners {
			rig.sweep(cfg, role)
		}
		if endedBy == 0 && rig.onlyRecord(role, a).Finished {
			endedBy = elapsed
		}
	}
	if endedBy == 0 || endedBy > bound {
		t.Fatalf("the record ended %s after its claim (0: not at all within %s), want within %s: its passes ran on while the sweeps kept running", endedBy, bound+3*roleWakeInterval, bound)
	}
	if n := afterEnd.Load(); n != 0 {
		t.Fatalf("passes wrote the record %d times after the write that ended it, want none", n)
	}
	crashAwaitEvents(t, next, []string{late}, crashKeptCopy, "the forward stored inside the fence, copied before the record ended")
}
