package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"math/big"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// tokenRowAcker is the acker of a frame TestARelayReplyTokenCannotBeGuessed publishes: it counts
// each answer the relay gives the server for the frame's message, and sends none.
type tokenRowAcker struct {
	acks, progress, naks atomic.Int32
}

func (a *tokenRowAcker) Ack() error                       { a.acks.Add(1); return nil }
func (a *tokenRowAcker) InProgress() error                { a.progress.Add(1); return nil }
func (a *tokenRowAcker) NakWithDelay(time.Duration) error { a.naks.Add(1); return nil }

// answers is how many answers of any kind the relay gave for the frame.
func (a *tokenRowAcker) answers() int32 { return a.acks.Load() + a.progress.Load() + a.naks.Load() }

// counterStep reports whether next is one step after prev on a counter written in base 36, as the
// relay once wrote its tokens: a session that read prev on its own frame could name next.
func counterStep(prev, next string) bool {
	before, ok := new(big.Int).SetString(prev, 36)
	if !ok {
		return false
	}
	after, ok := new(big.Int).SetString(next, 36)
	return ok && after.Cmp(before.Add(before, big.NewInt(1))) == 0
}

// A session reads the reply subject of every frame it is sent, and the relay acknowledges a
// session's message on a done published to that frame's reply subject, which names the session
// and then the frame's token: NATS gives the relay no sender to check (onReply), and a sender whose
// grant publishes every _INBOX. subject may name any session. So no token may tell a session the
// token of another frame. Each token is 128 random bits from crypto/rand (bus.SessionReplySubject:
// crypto/rand.Text, 26 base32 characters): across 1,000 frames to two sessions no token is one
// counter step after the token before it, as the listener's shared counter made them, and at least
// 120 of the first 128 bit positions take both values. A done under the other session's own reply
// prefix on a token no open frame holds acknowledges nothing: a random token, the counter's next
// value after the session's own frame, the other session's token with its last character changed,
// a finished frame's token, and the right token with one more subject token after it. The right
// token acknowledges its frame once and ends it.
func TestARelayReplyTokenCannotBeGuessed(t *testing.T) {
	t.Parallel()
	// The rig does not hold the durable binding, so no relay runs: every frame is one this test
	// publishes through publishFrame, as a relay's pull loop does.
	rig := newRelayRigUnowned(t, relayPauseFirst, relayPauseMax)
	relays := rig.relays
	sessions := [2]string{"ses_token_reader", "ses_token_victim"}
	runs := [2]*relayRun{{sessionID: sessions[0]}, {sessionID: sessions[1]}}
	reader := testnats.Connect(t, rig.url)
	t.Cleanup(reader.Close)
	sent, err := reader.SubscribeSync(bus.DeliverSubjectPrefix + ">")
	if err != nil {
		t.Fatalf("subscribe to the relay subjects: %v", err)
	}
	if err := reader.Flush(); err != nil {
		t.Fatalf("flush the relay subjects' subscription: %v", err)
	}
	var seq uint64
	// publish relays one message to sessions[i] as a frame answered through acker and returns the
	// token of the frame's reply subject, the subject token after the session's id, as the session
	// reads it.
	publish := func(i int, acker bus.SessionAcker) string {
		t.Helper()
		seq++
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		msg := bus.SessionMessage{
			Subject:      contracts.LegacyAgentSubject(sessions[i]),
			Data:         []byte(`{}`),
			StreamSeq:    seq,
			Delivered:    1,
			StoredAt:     time.Now(),
			SessionAcker: acker,
		}
		if err := relays.publishFrame(ctx, runs[i], msg, "stream", false, relayGapRetention, ""); err != nil {
			t.Fatalf("publish frame %d: %v", seq, err)
		}
		frame, err := sent.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("read frame %d on the relay subjects: %v", seq, err)
		}
		if frame.Subject != bus.DeliverSubject(sessions[i]) {
			t.Fatalf("frame %d went to %s, want %s", seq, frame.Subject, bus.DeliverSubject(sessions[i]))
		}
		token, ok := strings.CutPrefix(frame.Reply, relays.replyPrefix+sessions[i]+".")
		if !ok || token == "" || strings.Contains(token, ".") {
			t.Fatalf("frame %d's reply subject %q is not one token under %s's prefix in the listener's reply inbox %q", seq, frame.Reply, sessions[i], relays.replyPrefix)
		}
		return token
	}

	tokens := make([]string, 1000)
	for i := range tokens {
		tokens[i] = publish(i%2, &tokenRowAcker{})
	}
	// Checked first, so a relay that counts its tokens fails here.
	for i := 1; i < len(tokens); i++ {
		if counterStep(tokens[i-1], tokens[i]) {
			t.Fatalf("frame %d's token %q is one counter step after frame %d's token %q: a session that reads its own frame can name the next frame of any session",
				i+1, tokens[i], i, tokens[i-1])
		}
	}
	var ones [128]int
	for i, token := range tokens {
		bits, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(token)
		if err != nil || len(bits) != 16 {
			t.Fatalf("frame %d's token %q is not 128 bits in base32: %d bytes decoded, error %v", i+1, token, len(bits), err)
		}
		for bit := range ones {
			if bits[bit/8]&(1<<(bit%8)) != 0 {
				ones[bit]++
			}
		}
	}
	both := 0
	for _, n := range ones {
		if n > 0 && n < len(tokens) {
			both++
		}
	}
	if both < 120 {
		t.Fatalf("%d of the 128 bit positions take both values across %d tokens, want at least 120", both, len(tokens))
	}

	// The reader's own frame is the one the reader reads. The victim's frame goes to the other
	// session right after it: under a counter its token was the reader's plus one. The finished
	// frame is answered done before any guess.
	ownAcker, victimAcker, finishedAcker := &tokenRowAcker{}, &tokenRowAcker{}, &tokenRowAcker{}
	own := publish(0, ownAcker)
	victim := publish(1, victimAcker)
	finished := publish(1, finishedAcker)
	forger := testnats.Connect(t, rig.url)
	t.Cleanup(forger.Close)
	// victimReply is the reply subject of the victim's frame whose token is token: the victim's own
	// reply prefix, which a forger may publish to while its grant publishes every _INBOX. subject.
	victimReply := func(token string) string { return relays.replyPrefix + sessions[1] + "." + token }
	done := func(subject string) {
		t.Helper()
		if err := forger.Publish(subject, []byte(relayReplyDone)); err != nil {
			t.Fatalf("publish done on %s: %v", subject, err)
		}
		if err := forger.Flush(); err != nil {
			t.Fatalf("flush the done on %s: %v", subject, err)
		}
	}
	// barrier publishes a frame to the victim's session, answers it done on the forger's
	// connection and waits until the relay acknowledges it. The relay hears every reply on one
	// subscription, in the order one connection sent them, so it has handled every reply the
	// forger sent before.
	barrier := func() {
		t.Helper()
		acker := &tokenRowAcker{}
		done(victimReply(publish(1, acker)))
		if !testwait.Holds(5*time.Second, 10*time.Millisecond, func() bool { return acker.acks.Load() == 1 }) {
			t.Fatalf("the relay did not acknowledge the barrier frame's done within 5s")
		}
	}
	open := func(token string) bool {
		relays.framesMu.Lock()
		defer relays.framesMu.Unlock()
		return relays.frames[token] != nil || runs[0].outstanding[token] != nil || runs[1].outstanding[token] != nil
	}
	done(victimReply(finished))
	barrier()
	if got := finishedAcker.acks.Load(); got != 1 || open(finished) {
		t.Fatalf("the finished frame's done acknowledged it %d times and left it open %t, want once and ended", got, open(finished))
	}

	next, ok := new(big.Int).SetString(own, 36)
	if !ok {
		t.Fatalf("the reader's token %q does not read as a number in base 36", own)
	}
	changed := victim[:len(victim)-1] + "A"
	if strings.HasSuffix(victim, "A") {
		changed = victim[:len(victim)-1] + "B"
	}
	for _, guess := range []struct{ name, token string }{
		{"a random token", rand.Text()},
		{"the counter's next value after the reader's own frame", next.Add(next, big.NewInt(1)).Text(36)},
		{"the victim's token with its last character changed", changed},
		{"a finished frame's token", finished},
		{"the victim's token with one more subject token after it", victim + ".x"},
	} {
		if open(guess.token) {
			t.Fatalf("%s, %q, is the token of an open frame", guess.name, guess.token)
		}
		done(victimReply(guess.token))
		barrier()
		if got := victimAcker.answers(); got != 0 || !open(victim) {
			t.Fatalf("a done on %s answered the victim's frame %d times and left it open %t, want no answer and open", guess.name, got, open(victim))
		}
		if got := ownAcker.answers(); got != 0 {
			t.Fatalf("a done on %s answered the reader's own frame %d times, want none", guess.name, got)
		}
		if got := finishedAcker.answers(); got != 1 {
			t.Fatalf("a done on %s left the finished frame answered %d times, want once", guess.name, got)
		}
	}

	// The right token, twice: the second done names a frame that has ended.
	done(victimReply(victim))
	done(victimReply(victim))
	barrier()
	if acks, answers := victimAcker.acks.Load(), victimAcker.answers(); acks != 1 || answers != 1 {
		t.Fatalf("the victim's frame was acknowledged %d times and answered %d times, want one acknowledgement and nothing else", acks, answers)
	}
	if open(victim) {
		t.Fatalf("the victim's frame is still in the relay's frames or its run after its done")
	}
}
