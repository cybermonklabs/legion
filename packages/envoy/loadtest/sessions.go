package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The simulated sessions: on NATS, each subscribes its relay subject, as the pi plugin does
// before it registers (relay.go); on the listener, each registers, asks for consumer delivery and
// sends its heartbeat. It subscribes nothing else: a session is delivered to by the relay of its
// own consumer alone (INFRA-771), so a registration the listener can give no consumer is refused
// and counted an error, and a core subscription here would credit such a session with deliveries
// no plugin receives.

// natsSessions is the sessions' NATS side: the shared connections and their subscriptions, every
// receipt, and one sample envelope per event type.
type natsSessions struct {
	conns     []*nats.Conn
	subs      int
	receipts  *collector[receipt]
	envelopes envelopeSamples
	// relay is the consumer-delivery side: every session's frames, answers and resume points.
	relay *relaySessions
	// pulls counts the pull requests on the stream's consumers (pullSpy); nil when the spy could
	// not subscribe.
	pulls *pullSpy
}

// subscribeSessions connects natsConns connections the sessions share; each session subscribes
// its relay subject, as the pi plugin does before it registers, and nothing else: every message
// a session gets arrives as a relay frame of its own consumer. The connections it made are
// closed by close, also when it fails.
func (st *step) subscribeSessions() (*natsSessions, error) {
	ns := &natsSessions{receipts: newCollector[receipt](1 << 16), envelopes: envelopeSamples{data: map[string][]byte{}, marked: map[string]bool{}}}
	for i := range st.cfg.natsConns {
		nc, err := nats.Connect(st.cfg.ep.nats, nats.Name(fmt.Sprintf("loadgen-%d", i)), nats.MaxReconnects(-1))
		if err != nil {
			return ns, fmt.Errorf("nats connect: %w", err)
		}
		ns.conns = append(ns.conns, nc)
	}
	if spy, err := startPullSpy(st.cfg.ep.nats); err != nil {
		st.lg.Printf("pull spy: %v; the pull-request rate stays empty", err)
	} else {
		ns.pulls = spy
	}
	ns.relay = newRelaySessions(len(st.ids), len(ns.conns))
	for i, id := range st.ids {
		nc := ns.conns[i%len(ns.conns)]
		if _, err := nc.Subscribe(bus.DeliverSubject(id), ns.relay.handler(ns, i, contracts.AgentSubject(id), st.nonce)); err != nil {
			return ns, err
		}
		ns.subs++
	}
	for _, nc := range ns.conns {
		_ = nc.Flush()
	}
	st.lg.Printf("nats: %d subscriptions on %d connections", ns.subs, len(ns.conns))
	return ns, nil
}

// envelopeEvent is the id and the type of the Dispatch event an envelope carries, as a session
// reads them off a relay frame of its own consumer; ok is false when the bytes are no envelope,
// or its payload is no event of Dispatch's (internal/contracts envelope.go).
func envelopeEvent(data []byte) (id int64, typ string, ok bool) {
	var env contracts.Envelope
	if json.Unmarshal(data, &env) != nil {
		return 0, "", false
	}
	var ev struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(env.Payload), &ev) != nil {
		return 0, "", false
	}
	return ev.ID, ev.Type, true
}

func (ns *natsSessions) close() {
	for _, nc := range ns.conns {
		nc.Close()
	}
	if ns.pulls != nil {
		ns.pulls.close()
	}
}

// envelopeSamples keeps one envelope per event type (agent topics apart), for envelope-*.json.
type envelopeSamples struct {
	mu     sync.Mutex
	data   map[string][]byte
	marked map[string]bool // whether each kept envelope carries the run's marker
}

// offer keeps the first envelope of key, then replaces it once with one that carries the run's
// marker.
func (e *envelopeSamples) offer(key string, data []byte, nonce int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, seen := e.data[key]
	if seen && e.marked[key] {
		return
	}
	_, marked := findMarker(data, nonce)
	if seen && !marked {
		return
	}
	e.data[key] = bytes.Clone(data)
	e.marked[key] = marked
}

// write saves each kept envelope as envelope-<key>.json in dir.
func (e *envelopeSamples) write(dir string, lg *log.Logger) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key, data := range e.data {
		name := "envelope-" + strings.Map(func(r rune) rune {
			if r == '.' || r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				return r
			}
			return '_'
		}, key) + ".json"
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			lg.Printf("write %s: %v", name, err)
		}
	}
}

// registrar registers the sessions with the listener as the pi plugin does. Each session's body
// is the same at every heartbeat, but for its delivery field, which carries the session's current
// resume point.
type registrar struct {
	url, token    string
	client        *http.Client
	bodies        []map[string]any
	ok, errs      atomic.Int64 // the first registration of each session
	heartbeatErrs atomic.Int64
	// relay hears each answer's delivery mode and holds the resume point each body asks from.
	relay *relaySessions
}

// registerAnswer is the part of a registration's answer the registrar reads: the delivery mode
// and stream. This listener answers every registration it takes with a delivery field naming
// consumer; one that carries none, or another mode, is counted a session with no consumer
// (relaySessions.answered).
type registerAnswer struct {
	Delivery *struct {
		Mode     string `json:"mode"`
		StreamID string `json:"stream_id"`
	} `json:"delivery"`
}

func (r *registrar) register(ctx context.Context, i int) error {
	body := maps.Clone(r.bodies[i])
	body["delivery"] = r.relay.delivery(i)
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", r.url+"/v1/interests/subscribe", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%d %s", resp.StatusCode, trunc(b))
	}
	var ans registerAnswer
	mode, streamID := "", ""
	if json.Unmarshal(b, &ans) == nil && ans.Delivery != nil {
		mode, streamID = ans.Delivery.Mode, ans.Delivery.StreamID
	}
	r.relay.answered(i, mode, streamID)
	return nil
}

// registrationFields is one session's registration with the listener: its directory, title and
// the topics it watches, which are the issue topics it follows in each of the step's
// populations. With -mention-frac, it advertises the default mention delivery, so Dispatch
// delivers a mention of the session. registrar.register adds the delivery field asking for
// consumer delivery.
func (st *step) registrationFields(i int) map[string]any {
	topics := []string{}
	for _, p := range st.pops {
		topics = append(topics, p.topics(p.sessions[i])...)
	}
	return registrationFieldsOf(st.ids[i], topics, st.cfg.mentionFrac > 0)
}

// registrationFieldsOf is that registration for a session id and the topics it follows, which
// the replay's roster builds one of before the session is laid out (newReplayStep). mentions
// advertises the default mention delivery, which is what -mention-frac needs of a session.
func registrationFieldsOf(id string, topics []string, mentions bool) map[string]any {
	capabilities := []string{}
	if mentions {
		capabilities = []string{mentionDelivery}
	}
	return map[string]any{"session_id": id, "dir": "/work/" + id, "title": "load " + id,
		"topics": topics, "port": 0, "self_subscribed": true, "driving": false, "capabilities": capabilities}
}

// registrationBody is registrationFieldsOf encoded, as the listener receives it.
func registrationBody(id string, topics []string, mentions bool) []byte {
	b, _ := json.Marshal(registrationFieldsOf(id, topics, mentions))
	return b
}

// registerSessions registers every session, 16 at a time. Each body asks for consumer delivery
// with the epoch of the session's NATS connection and, once the session has one, its resume point
// (relaySessions.delivery).
func (st *step) registerSessions(ctx context.Context, ns *natsSessions) *registrar {
	reg := &registrar{url: st.cfg.ep.listener, token: st.cfg.ep.listenerToken, client: &http.Client{Timeout: 10 * time.Second},
		bodies: make([]map[string]any, len(st.ids)), relay: ns.relay}
	for i := range st.ids {
		reg.bodies[i] = st.registrationFields(i)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i, id := range st.ids {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := reg.register(ctx, i); err != nil {
				if reg.errs.Add(1) <= 5 {
					st.lg.Printf("register %s: %v", id, err)
				}
				return
			}
			reg.ok.Add(1)
		})
	}
	wg.Wait()
	st.lg.Printf("registered %d sessions (%d errors)", reg.ok.Load(), reg.errs.Load())
	d := ns.relay.summary()
	st.lg.Printf("session delivery: %d sessions on consumer delivery, %d with no consumer, %d unanswered",
		d.ConsumerSessions, d.NoConsumerSessions, d.Unanswered)
	return reg
}

// startHeartbeat re-registers one session at a time, so that each one's heartbeat
// (contracts.SessionHeartbeat) is spread evenly, until ctx ends.
func (st *step) startHeartbeat(ctx context.Context, reg *registrar) {
	go func() {
		t := time.NewTicker(contracts.SessionHeartbeat / time.Duration(len(st.ids)))
		defer t.Stop()
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				go func(i int) {
					if err := reg.register(ctx, i); err != nil && ctx.Err() == nil {
						if reg.heartbeatErrs.Add(1) <= 5 {
							st.lg.Printf("heartbeat %s: %v", st.ids[i], err)
						}
					}
				}(i % len(st.ids))
			}
		}
	}()
}
