package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// fakeEnvoy is the fake Envoy listener of the local Dispatch (packages/dispatch/e2e/fake-envoy.ts):
// the sessions it calls live, and every message the server sent through it. Changes 5 and 6 play
// the writer through it: the writer session of a case with a session is live with a host that
// takes side questions (btw), so the server sends it the side question, and the harness reads that
// send and answers it as the session would.
type fakeEnvoy struct {
	url  string
	http *http.Client
}

func newFakeEnvoy(url string) *fakeEnvoy {
	return &fakeEnvoy{url: strings.TrimRight(url, "/"), http: &http.Client{Timeout: 30 * time.Second}}
}

// fakeSend is one message the server sent a session through the listener: its target, the stored
// message's id (or a definition's) and body, and the delivery attempt a reply names.
type fakeSend struct {
	Target  string
	ID      string
	Body    string
	Attempt int
}

func (f *fakeEnvoy) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.url+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the fake Envoy's %s %s answered %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// seed makes each of sessions live with a host that takes side questions, keeping every session
// the listener already holds. It writes nothing where all of them are live already, since a
// reseed starts the listener's record of sends over.
func (f *fakeEnvoy) seed(ctx context.Context, sessions []string) error {
	var live []struct {
		SessionID    string   `json:"session_id"`
		Capabilities []string `json:"capabilities"`
	}
	if err := f.do(ctx, http.MethodGet, "/v1/sessions?include=unresponsive", nil, &live); err != nil {
		return err
	}
	type seeded struct {
		SessionID    string   `json:"session_id"`
		Title        string   `json:"title"`
		Capabilities []string `json:"capabilities"`
	}
	var all []seeded
	have := map[string]bool{}
	for _, s := range live {
		capabilities := s.Capabilities
		if slices.Contains(sessions, s.SessionID) && !slices.Contains(capabilities, "btw") {
			capabilities = append(slices.Clone(capabilities), "btw")
		} else {
			have[s.SessionID] = true
		}
		all = append(all, seeded{SessionID: s.SessionID, Title: s.SessionID, Capabilities: capabilities})
	}
	missing := false
	for _, s := range sessions {
		if !have[s] {
			missing = true
			if !slices.ContainsFunc(all, func(x seeded) bool { return x.SessionID == s }) {
				all = append(all, seeded{SessionID: s, Title: "harness writer", Capabilities: []string{"btw"}})
			}
		}
	}
	if !missing {
		return nil
	}
	return f.do(ctx, http.MethodPut, "/__fixture/sessions", all, nil)
}

// sends are the messages the server has sent through the listener.
func (f *fakeEnvoy) sends(ctx context.Context) ([]fakeSend, error) {
	var raw []struct {
		Target  string `json:"target_session"`
		Payload string `json:"payload"`
	}
	if err := f.do(ctx, http.MethodGet, "/__fixture/sends", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]fakeSend, 0, len(raw))
	for _, r := range raw {
		var frame struct {
			Event struct {
				Payload struct {
					ID   string `json:"id"`
					Body string `json:"body"`
				} `json:"payload"`
			} `json:"event"`
			Delivery struct {
				Attempt int `json:"attempt"`
			} `json:"delivery"`
		}
		if json.Unmarshal([]byte(r.Payload), &frame) != nil || frame.Event.Payload.ID == "" {
			continue
		}
		out = append(out, fakeSend{Target: r.Target, ID: frame.Event.Payload.ID, Body: frame.Event.Payload.Body, Attempt: max(frame.Delivery.Attempt, 1)})
	}
	return out, nil
}

// await waits, polling every poll, for the first send match accepts, until ctx ends.
func (f *fakeEnvoy) await(ctx context.Context, poll time.Duration, match func(fakeSend) bool) (fakeSend, error) {
	for {
		sends, err := f.sends(ctx)
		if err != nil {
			return fakeSend{}, err
		}
		for _, s := range sends {
			if match(s) {
				return s, nil
			}
		}
		select {
		case <-ctx.Done():
			return fakeSend{}, ctx.Err()
		case <-time.After(poll):
		}
	}
}
