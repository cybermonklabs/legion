// Package busprojectstest gives a test of a switch to private what busprojects.HubAdmission's proof
// reads: a listener replica that answers its privacy read (ReadyListener), and a hub with an auth
// callout whose stand-in auth service admits Dispatch's hub-admission proof only on a hub that
// carries the private-projects server tag (StartHub).
package busprojectstest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/busprojects"
)

// ReadyListenerSource is the privacy source ReadyListener's replica reads and its target expects.
const ReadyListenerSource = "https://broker.example.invalid"

// readyListenerInstances is ReadyListener's target's instances: an IP literal, so the proof
// resolves it without DNS, and the port a listener serves on.
const readyListenerInstances = "127.0.0.1:9020"

// ReadyListener is a listener inventory of one workload and the client that reads it, for
// HubAdmission.SetListeners: the workload's instances are an IP literal, and the client's transport
// answers its replica's GET busprojects.ReadinessPath, and nothing else, with 200 and a read that
// answered in required privacy mode from the workload's source. Nothing listens anywhere: the
// transport is the replica. A switch test calls hubAdmission.SetListeners(ReadyListener(t)) so the
// proof's listener part passes and its hubs decide it.
func ReadyListener(t testing.TB) ([]busprojects.ListenerTarget, *http.Client) {
	t.Helper()
	target := busprojects.ListenerTarget{
		Name:      "example-listener",
		Source:    ReadyListenerSource,
		Host:      "listener.example.invalid",
		Instances: readyListenerInstances,
	}
	body, err := json.Marshal(map[string]any{"instance": "example-listener-0", "required": true, "source": target.Source, "read": busprojects.ReadAnswered})
	if err != nil {
		t.Fatalf("encode the ready listener's answer: %v", err)
	}
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Host != readyListenerInstances || r.URL.Path != busprojects.ReadinessPath {
			return answer(r, http.StatusNotFound, "not the ready listener's privacy-readiness route\n"), nil
		}
		return answer(r, http.StatusOK, string(body)), nil
	})}
	return []busprojects.ListenerTarget{target}, client
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func answer(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       r,
	}
}
