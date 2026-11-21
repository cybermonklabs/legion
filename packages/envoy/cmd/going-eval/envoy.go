package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/going/eval"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/listenerwire"
)

// fakeEnvoy is the Envoy listener one try's Dispatch server reads: it answers the seed's agent
// sessions as the listener does - a live session at the time of each read, an unresponsive one
// as the seed last saw it, a gone one not at all - and takes a send to a live or unresponsive
// session. A live session answers a send with its status reply (eval.Session.StatusReply), posted
// back to the message as that session through Dispatch's reply route, which is how an agent of
// the seed answers the person who asks it where its work stands. It holds no role, no interest
// and no Legion agent entry.
type fakeEnvoy struct {
	sessions map[string]eval.Session
	listener net.Listener
	server   *http.Server

	// dispatch is the origin of the try's server, which a reply is posted to, and agentToken the
	// shared token a session's reply presents; both are set once the server is known.
	mu         sync.Mutex
	dispatch   string
	agentToken string
	replies    sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
}

// startFakeEnvoy serves the seed's sessions on a loopback port.
func startFakeEnvoy(sessions []eval.Session) (*fakeEnvoy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for the fake Envoy: %w", err)
	}
	f := &fakeEnvoy{sessions: map[string]eval.Session{}, listener: listener}
	for _, session := range sessions {
		f.sessions[session.ID] = session
	}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	f.server = &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = f.server.Serve(listener) }()
	return f, nil
}

// url is the base URL the try's server names as ENVOY_URL.
func (f *fakeEnvoy) url() string {
	return "http://" + f.listener.Addr().String()
}

// answerTo names the Dispatch server a reply is posted to and the token it presents.
func (f *fakeEnvoy) answerTo(origin, agentToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatch, f.agentToken = origin, agentToken
}

// close stops serving and waits for every reply in flight.
func (f *fakeEnvoy) close() {
	f.cancel()
	_ = f.server.Close()
	f.replies.Wait()
}

// answered is a session as the listener answers it, in the state the seed gives it: a live one
// seen now.
func (f *fakeEnvoy) answered(session eval.Session) dispatchenvoy.Session {
	seen := session.LastSeen
	if session.State == dispatchenvoy.StateLive {
		seen = time.Now()
	}
	return dispatchenvoy.Session{
		SessionID: session.ID, Title: session.Title, MachineID: "eval", Host: "eval",
		Roles: []string{}, Capabilities: []string{"btw", "aside", "steer"},
		LastSeen: seen.UnixMilli(), State: session.State,
	}
}

// includes reports whether the request's include selector asks for asked.
func includes(r *http.Request, asked string) bool {
	for _, value := range strings.Split(r.URL.Query().Get("include"), ",") {
		if value == asked {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeEnvoy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/healthz":
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case r.Method == http.MethodGet && path == "/v1/sessions":
		list := []dispatchenvoy.Session{}
		for _, session := range f.sessions {
			if session.State == dispatchenvoy.StateLive || (session.State == dispatchenvoy.StateUnresponsive && includes(r, "unresponsive")) {
				list = append(list, f.answered(session))
			}
		}
		writeJSON(w, http.StatusOK, list)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/registry/"):
		id := strings.TrimPrefix(path, "/v1/registry/")
		session, ok := f.sessions[id]
		switch {
		case !ok || session.State == dispatchenvoy.StateGone:
			writeJSON(w, http.StatusNotFound, listenerwire.APIError{Error: "no live session " + id})
		case session.State == dispatchenvoy.StateUnresponsive && !includes(r, "unresponsive"):
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": "no live session " + id, "state": session.State, "last_seen": session.LastSeen.UnixMilli(),
			})
		default:
			writeJSON(w, http.StatusOK, f.answered(session))
		}
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/roles/"):
		writeJSON(w, http.StatusNotFound, listenerwire.APIError{Error: "no holder for role " + strings.TrimPrefix(path, "/v1/roles/")})
	case r.Method == http.MethodGet && (path == "/v1/legion/agents" || strings.HasPrefix(path, "/v1/legion/agents/roles/")):
		writeJSON(w, http.StatusOK, map[string]any{"entries": []any{}})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/legion/agents/sessions/"):
		writeJSON(w, http.StatusNotFound, listenerwire.APIError{Error: "unknown legion agent " + strings.TrimPrefix(path, "/v1/legion/agents/sessions/")})
	case r.Method == http.MethodGet && path == "/v1/interests/":
		writeJSON(w, http.StatusOK, []any{})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/interests/"):
		http.NotFound(w, r)
	case r.Method == http.MethodPost && path == "/v1/interests/unsubscribe":
		var body struct {
			Topics []string `json:"topics"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, http.StatusOK, map[string]any{"removed": body.Topics})
	case r.Method == http.MethodPost && path == "/v1/messages/send":
		f.send(w, r)
	default:
		http.NotFound(w, r)
	}
}

// send takes a send to a live or unresponsive session of the seed and refuses one to a gone or
// unknown session as the listener does. A live session's status reply is posted back to the
// message once the send is answered.
func (f *fakeEnvoy) send(w http.ResponseWriter, r *http.Request) {
	var request listenerwire.SendRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, listenerwire.APIError{Error: "the send is no JSON: " + err.Error()})
		return
	}
	session, ok := f.sessions[request.TargetSession]
	if !ok || session.State == dispatchenvoy.StateGone {
		writeJSON(w, http.StatusNotFound, listenerwire.APIError{Error: "no live session " + request.TargetSession})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"event_id":        fmt.Sprintf("eval-%d", time.Now().UnixNano()),
		"recipient":       session.ID,
		"recipient_state": session.State,
		"state":           "connected",
	})
	if session.StatusReply == "" || request.Payload == nil {
		return
	}
	var frame struct {
		Event struct {
			Payload struct {
				ID string `json:"id"`
			} `json:"payload"`
		} `json:"event"`
		Delivery struct {
			Attempt int `json:"attempt"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(*request.Payload), &frame); err != nil || frame.Event.Payload.ID == "" || frame.Delivery.Attempt < 1 {
		return
	}
	f.replies.Add(1)
	go func() {
		defer f.replies.Done()
		f.reply(session, frame.Event.Payload.ID, frame.Delivery.Attempt)
	}()
}

// reply posts session's status reply to the message's delivery attempt, as the session, trying
// again while the attempt the send belongs to is not yet written.
func (f *fakeEnvoy) reply(session eval.Session, message string, attempt int) {
	f.mu.Lock()
	origin, token := f.dispatch, f.agentToken
	f.mu.Unlock()
	if origin == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"actor":   map[string]string{"kind": "session", "id": session.ID},
		"attempt": attempt,
		"body":    session.StatusReply,
	})
	for range 20 {
		select {
		case <-f.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		request, err := http.NewRequestWithContext(f.ctx, http.MethodPost, origin+"/api/v1/messages/"+message+"/reply", bytes.NewReader(body))
		if err != nil {
			return
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			continue
		}
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusCreated {
			return
		}
		slog.Debug("going-eval: the fake Envoy's reply waits", "session", session.ID, "status", response.StatusCode)
	}
	slog.Warn("going-eval: a session's status reply was never taken", "session", session.ID, "message", message)
}
