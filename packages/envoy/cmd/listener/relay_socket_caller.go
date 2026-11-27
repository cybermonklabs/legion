package main

// relay_socket_caller.go is what a replica's relay sockets count per session and per caller: the
// places a socket takes (admit) and gives back (release), and the caller each socket's places and
// held bytes are counted against (relayCaller, relaySocketMaxPerCaller,
// relaySocketCallerPendingBytes).

import (
	"net/http"
	"sync/atomic"

	"github.com/sjawhar/envoy/internal/ciphertoken"
)

// relayCaller is one caller's sockets on the replica: how many are open, under relaySockets.mu,
// and the bytes they hold for their clients, which relaySockets.callerBudget bounds. It is
// forgotten once its last socket is released, which comes after that socket's pump has given back
// the last frame it held (read), so both counts end together.
type relayCaller struct {
	sockets  int
	buffered atomic.Int64
}

// relayCallerKey is the caller a socket's sockets and bytes are counted against: its person, or
// the service its token names when it names no person (a Caller names one of the two).
func relayCallerKey(caller ciphertoken.Caller) string {
	if caller.Person != "" {
		return "person " + caller.Person
	}
	return "service " + caller.Service
}

// admit records socket as open, counted against its session and its caller, or answers the status
// it is refused with: 503 once the listener is stopping or holds its cap of sockets (maxSockets),
// on which the client opens it on another replica, and 429 when its session already holds
// relaySocketMaxPerSession or its caller relaySocketMaxPerCaller.
func (s *relaySockets) admit(socket *relaySocket) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return http.StatusServiceUnavailable, "the listener is stopping; open the socket again on another replica"
	}
	held := s.open[socket.session]
	if len(held) >= relaySocketMaxPerSession {
		return http.StatusTooManyRequests, "this session already holds the most relay sockets a replica serves it"
	}
	key := relayCallerKey(socket.admitted)
	caller := s.callers[key]
	if caller != nil && caller.sockets >= relaySocketMaxPerCaller {
		return http.StatusTooManyRequests, "this token's person or service already holds the most relay sockets a replica serves one"
	}
	if s.count >= s.maxSockets {
		return http.StatusServiceUnavailable, "this replica holds the most relay sockets it serves; open the socket again on another replica"
	}
	if held == nil {
		held = map[*relaySocket]struct{}{}
		s.open[socket.session] = held
	}
	if caller == nil {
		caller = &relayCaller{}
		s.callers[key] = caller
	}
	held[socket] = struct{}{}
	caller.sockets++
	socket.caller = caller
	s.count++
	return 0, ""
}

// release forgets socket, whatever ended it.
func (s *relaySockets) release(socket *relaySocket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.open[socket.session][socket]; !held {
		return
	}
	s.count--
	delete(s.open[socket.session], socket)
	if len(s.open[socket.session]) == 0 {
		delete(s.open, socket.session)
	}
	socket.caller.sockets--
	if socket.caller.sockets == 0 {
		delete(s.callers, relayCallerKey(socket.admitted))
	}
}
