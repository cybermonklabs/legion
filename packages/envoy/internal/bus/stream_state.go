// What SessionConsumers reads of the notification stream itself: its identity and sequences, read
// once for every call waiting on a read.

package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// sessionStreamState is what SessionConsumers reads of the stream: its identity, the first sequence
// it still holds and the last it stored.
type sessionStreamState struct {
	id                string
	firstSeq, lastSeq uint64
}

// sessionStreamReadWait bounds one read of the stream's state (streamState).
const sessionStreamReadWait = 5 * time.Second

// sessionStreamReadOverlap is how long the stream read in flight holds the next one back
// (readStreamStates): the next read starts when the one in flight answers or once it has been out
// this long, whichever comes first. It trades two things. A burst of calls that join while a read
// is out still shares one read, so the stream is sent at most one info per round trip, or per
// sessionStreamReadOverlap while reads stall, rather than one per call: at most 20 over a read's
// whole sessionStreamReadWait. And a read whose request or answer was lost - on a connection that
// stays up, or before ErrRequestLostWithConnection ends it - delays every other call by at most
// this bound rather than by its whole sessionStreamReadWait, which a registration, whose store
// work has a budget of 3.5 s, cannot wait out.
//
// 250 ms sits between the two: a stream info is answered from the server's memory in one round
// trip, a few ms on a pod's network and well under 100 ms on any link a listener runs over, so a
// read that is merely slow answers inside the bound and the burst behind it never sends a second
// one; and a lost read costs a registration behind it a small part of its budget.
const sessionStreamReadOverlap = 250 * time.Millisecond

// streamRead is one read of the stream's state, which every streamState call waiting on it shares.
type streamRead struct {
	// done is closed once state and err are set.
	done  chan struct{}
	state sessionStreamState
	err   error
}

// streamState reads the stream's identity and sequences with its info. Calls made at once share a
// read: a call waits for the next read to begin, after the one in flight answers or has been out
// sessionStreamReadOverlap, and every call waiting when it begins gets its answer. So no call gets
// a state read before it was made; a burst of calls, such as the restarts of every session at a
// listener's start, sends one stream info per round trip rather than one per call; and a read that
// never answers holds the calls behind it for at most sessionStreamReadOverlap, not for its whole
// sessionStreamReadWait. A call returns ctx's error if ctx ends first.
func (s *SessionConsumers) streamState(ctx context.Context) (sessionStreamState, error) {
	return s.joinStreamRead().wait(ctx)
}

// joinStreamRead returns the next read of the stream's state to begin (streamState), starting the
// goroutine that starts the reads when none runs.
func (s *SessionConsumers) joinStreamRead() *streamRead {
	s.streamMu.Lock()
	read := s.streamNext
	if read == nil {
		read = &streamRead{done: make(chan struct{})}
		s.streamNext = read
	}
	start := !s.streamReading
	s.streamReading = true
	s.streamMu.Unlock()
	if start {
		go s.readStreamStates()
	}
	return read
}

// wait returns what the read read, once it has answered, or ctx's error if ctx ends first.
func (r *streamRead) wait(ctx context.Context) (sessionStreamState, error) {
	select {
	case <-r.done:
		return r.state, r.err
	case <-ctx.Done():
		return sessionStreamState{}, ctx.Err()
	}
}

// readStreamStates starts the stream reads streamState calls wait for, each in a goroutine of its
// own and within sessionStreamReadWait (sendStreamRead), until none waits. Each read starts once
// the one before it has answered or has been out sessionStreamReadOverlap, so reads overlap only
// while one stalls, and then at most one starts per sessionStreamReadOverlap.
func (s *SessionConsumers) readStreamStates() {
	for {
		s.streamMu.Lock()
		read := s.streamNext
		s.streamNext = nil
		if read == nil {
			s.streamReading = false
			s.streamMu.Unlock()
			return
		}
		s.streamMu.Unlock()
		go s.sendStreamRead(read)
		overlap := time.NewTimer(sessionStreamReadOverlap)
		select {
		case <-read.done:
		case <-overlap.C:
		}
		overlap.Stop()
	}
}

// sendStreamRead makes read, within sessionStreamReadWait, and hands its answer to every call
// waiting on it.
func (s *SessionConsumers) sendStreamRead(read *streamRead) {
	ctx, cancel := context.WithTimeout(context.Background(), sessionStreamReadWait)
	defer cancel()
	read.state, read.err = s.readStreamState(ctx)
	close(read.done)
}

// readStreamState reads the stream's identity and sequences with one stream info.
func (s *SessionConsumers) readStreamState(ctx context.Context) (sessionStreamState, error) {
	reply, err := s.client.requestConfirmed(ctx, "$JS.API.STREAM.INFO."+Stream, nil)
	if err != nil {
		return sessionStreamState{}, err
	}
	var response struct {
		Config struct {
			Metadata map[string]string `json:"metadata"`
		} `json:"config"`
		State struct {
			FirstSeq uint64 `json:"first_seq"`
			LastSeq  uint64 `json:"last_seq"`
		} `json:"state"`
		Error *jetstream.APIError `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &response); err != nil {
		return sessionStreamState{}, fmt.Errorf("bus: read the info of %s: %w", Stream, err)
	}
	if response.Error != nil {
		return sessionStreamState{}, fmt.Errorf("bus: read the info of %s: %w", Stream, response.Error)
	}
	return sessionStreamState{
		id:       response.Config.Metadata[StreamIDMetadataKey],
		firstSeq: response.State.FirstSeq,
		lastSeq:  response.State.LastSeq,
	}, nil
}
