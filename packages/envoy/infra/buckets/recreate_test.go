package buckets

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// recreate deletes no bucket outside INFRA-4219's writer hold, and refuses before it reads or
// calls anything, whatever its arguments: no arguments; a bucket and the server; that with each
// flag of the old observation proof added; with --break-glass; and with a malformed flag. Every
// shape exits 1 with a refusal naming the hold and the pull request that adds it
// (cybermonklabs/legion#4698), calls no kubectl, ssh or sh, sends no HTTP request, opens no
// connection to the server, and leaves the bucket there as it was. The fake world answers that no
// writer runs anywhere, so a proof that ran would pass and go on to delete the bucket.
//
// The server's count of accepted connections is cumulative (/varz total_connections), so a
// connection the run opened and closed again still moves it; a count of the clients open once the
// run has ended would not. The control opens and closes one connection and sees the count move by
// one.
//
// At #5201's head (73024df44b, the observation proof) the shape with --kube-context prod exits 0,
// deletes envoy_sessions and creates it again, its stream's creation time changing; the runner
// records its kubectl calls and the server counts its connection.
func TestRecreateRefusesOutsideTheHoldBeforeAnyCall(t *testing.T) {
	t.Setenv(bus.ReplicasEnvVar, "")
	server, uri := testnats.Start(t)
	_, js := jetStream(t, uri)
	config, _ := bus.CopiedBucketConfig(bus.SessionBucket, 1)
	kv, err := js.CreateKeyValue(&config)
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	if _, err := kv.Put("session-1", []byte("held")); err != nil {
		t.Fatalf("write a key: %v", err)
	}
	accepted := func(t *testing.T) uint64 {
		t.Helper()
		response, err := http.Get(server.MonitorURL() + "/varz")
		if err != nil {
			t.Fatalf("read the server's /varz: %v", err)
		}
		defer response.Body.Close()
		var varz struct {
			TotalConnections *uint64 `json:"total_connections"`
		}
		if err := json.NewDecoder(response.Body).Decode(&varz); err != nil || varz.TotalConnections == nil {
			t.Fatalf("read total_connections from the server's /varz: %v", err)
		}
		return *varz.TotalConnections
	}
	before := accepted(t)
	control, err := natsgo.Connect(uri, natsgo.NoReconnect())
	if err != nil {
		t.Fatalf("control: connect to the server: %v", err)
	}
	control.Close()
	if after := accepted(t); after != before+1 {
		t.Fatalf("control: a connection opened and closed moved the server's accepted connections from %d to %d, want %d", before, after, before+1)
	}

	named := []string{"recreate", "--bucket", bus.SessionBucket, "--nats", uri}
	with := func(args ...string) []string { return append(slices.Clone(named), args...) }
	for _, c := range []struct {
		name string
		args []string
	}{
		{"no arguments", []string{"recreate"}},
		{"--bucket and --nats", named},
		{"--kube-context", with("--kube-context", "prod")},
		{"--host", with("--host", "box-1")},
		{"--ssh", with("--ssh", "ssm-ssh", "--host", "box-1")},
		{"--this-machine", with("--this-machine")},
		{"--connz", with("--connz", "http://hub.invalid:8222")},
		{"--first-deployment", with("--first-deployment", "--kube-context", "prod")},
		{"--break-glass", with("--break-glass")},
		{"a malformed flag", with("--replicas", "three")},
	} {
		t.Run(c.name, func(t *testing.T) {
			stream := streamOf(t, js, bus.SessionBucket)
			connections := accepted(t)
			w := &world{pods: noPods, nodes: "node-a", running: map[string]string{"node-a": noPods},
				hostAnswers: map[string][]string{"box-1": {clean(150), clean(150)}, here: {clean(150), clean(150)}}}
			var sent []string
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				sent = append(sent, request.Method+" "+request.URL.String())
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: request,
					Body: io.NopCloser(strings.NewReader(`{"total":0,"offset":0,"connections":[]}`))}, nil
			})}
			var stdout, stderr strings.Builder
			code := command(context.Background(), c.args, &stdout, &stderr, w.run, client)
			if code != 1 {
				t.Errorf("recreate exited %d, want 1", code)
			}
			for _, says := range []string{"INFRA-4219's writer hold", "cybermonklabs/legion#4698"} {
				if !strings.Contains(stderr.String(), says) {
					t.Errorf("recreate's standard error does not name %q", says)
				}
			}
			if len(w.calls) != 0 {
				t.Errorf("recreate called the runner: %q", w.calls)
			}
			if len(sent) != 0 {
				t.Errorf("recreate sent HTTP requests: %q", sent)
			}
			if after := accepted(t); after != connections {
				t.Errorf("the server accepted %d connections during the run, want none", after-connections)
			}
			if after, err := js.StreamInfo("KV_" + bus.SessionBucket); err != nil || !after.Created.Equal(stream.Created) || after.State.LastSeq != stream.State.LastSeq {
				if err != nil {
					t.Errorf("read KV_%s after the run: %v, want it there as it was", bus.SessionBucket, err)
				} else {
					t.Errorf("KV_%s after the run: created %s, last sequence %d; want created %s, last sequence %d", bus.SessionBucket, after.Created, after.State.LastSeq, stream.Created, stream.State.LastSeq)
				}
			}
			if t.Failed() {
				t.Logf("%q\nstdout:\n%s\nstderr:\n%s", c.args, stdout.String(), stderr.String())
			}
		})
	}
}
