package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// unreadableLegionRecord is bytes no read of the Legion agent registry decodes, as an entry or as
// a project's answering record.
const unreadableLegionRecord = "not a registry record"

// A record the legion_agents bucket holds and the listener cannot decode is answered
// listenerwire.FaultStatus with no Retry-After, naming the record: a session's entry, an issue
// listing's key and a project's answering record on the reads, and a session's stored entry on
// the two entry writes, which read it before they write, the batch answering that it wrote none.
// The bucket answered, and will answer the same record again, so a Retry-After would only send the
// caller back for it, and the bus-unavailable 503 would send an operator after an outage that never
// happened. The records are left as they were. The control is the same reads once NATS has
// stopped: a read the bucket does not answer is still the 503 with Retry-After: 1.
func TestALegionRegistryRecordThatDoesNotDecodeIsAServerError(t *testing.T) {
	t.Parallel()
	server, uri := testnats.Start(t)
	client, err := bus.Connect([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	l := newLegionListener(t, client)
	kv, err := client.JS().KeyValue(bus.LegionAgentBucket)
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	keys := []string{"session.ses-bad", "issue.INFRA-448.ses-bad", "project.infra"}
	for _, key := range keys {
		if _, err := kv.Put(key, []byte(unreadableLegionRecord)); err != nil {
			t.Fatalf("store %s: %v", key, err)
		}
	}
	entry := legionEntry("ses-bad", "INFRA-448", "tester", "2026-10-26T03:15:00Z")
	requests := []struct {
		name, method, target, body, names string

		// read marks the requests the control asks again with NATS stopped; batch the one whose
		// answer carries written.
		read, batch bool
	}{
		{name: "a session's entry", method: http.MethodGet, target: "/v1/legion/agents/sessions/ses-bad", names: "legion agent ses-bad", read: true},
		{name: "an issue listing's key", method: http.MethodGet, target: "/v1/legion/agents?issue=INFRA-448", names: "key issue.INFRA-448.ses-bad", read: true},
		{name: "a project's answering record", method: http.MethodGet, target: "/v1/legion/agents/roles/legion-infra-infra-448-tester", names: "legion project infra", read: true},
		{name: "an entry write's stored entry", method: http.MethodPut, target: "/v1/legion/agents/sessions/ses-bad", body: entry, names: "legion agent ses-bad"},
		{name: "a batch write's stored entry", method: http.MethodPut, target: "/v1/legion/agents", body: `{"entries":[` + entry + `]}`, names: "legion agent ses-bad", batch: true},
	}
	const head = "envoy: the legion agent registry holds a record it cannot decode: "
	for _, tc := range requests {
		recorder := l.do(t, tc.method, tc.target, tc.body)
		if recorder.Code != listenerwire.FaultStatus {
			t.Fatalf("%s: %s %s = %d %s, want %d", tc.name, tc.method, tc.target, recorder.Code, recorder.Body.String(), listenerwire.FaultStatus)
		}
		if got := recorder.Header().Get("Retry-After"); got != "" {
			t.Fatalf("%s: Retry-After = %q, want none: a retry reads the same record", tc.name, got)
		}
		failure := decodeLegion[struct {
			Error   string `json:"error"`
			Written *int   `json:"written"`
		}](t, recorder.Body.String())
		if !strings.HasPrefix(failure.Error, head) || !strings.Contains(failure.Error, tc.names) {
			t.Fatalf("%s: the answer reads %q, want it to begin %q and name %q", tc.name, failure.Error, head, tc.names)
		}
		if tc.batch && (failure.Written == nil || *failure.Written != 0) {
			t.Fatalf("%s: the answer is %s, want written 0: the batch read the stored entry before its first write", tc.name, recorder.Body.String())
		}
	}
	for _, key := range keys {
		stored, err := kv.Get(key)
		if err != nil {
			t.Fatalf("read %s after the requests: %v", key, err)
		}
		if !bytes.Equal(stored.Value(), []byte(unreadableLegionRecord)) {
			t.Fatalf("%s after the requests = %q, want %q, untouched", key, stored.Value(), unreadableLegionRecord)
		}
	}

	server.Stop(t)
	for _, tc := range requests {
		if !tc.read {
			continue
		}
		recorder := l.do(t, tc.method, tc.target, tc.body)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "1" {
			t.Fatalf("%s with NATS stopped: %d (Retry-After %q) %s, want a 503 to retry", tc.name, recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}
		if failure := decodeLegion[listenerwire.APIError](t, recorder.Body.String()); !strings.HasPrefix(failure.Error, "envoy: the legion agent registry did not answer: ") {
			t.Fatalf("%s with NATS stopped: the answer reads %q, want the registry that did not answer", tc.name, failure.Error)
		}
	}
}
