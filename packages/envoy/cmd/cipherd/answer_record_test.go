package main

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/api"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestTheBinaryRecordsARefusalAndCountsEveryAnswer drives the REAL compiled binary, whose API port
// main puts behind the answer recorder. A reread of a name no secret can carry (a public route)
// answers 400 SECRET_NAME_INVALID carrying one answer id in its X-Broker-Answer-Id header and
// as its body's answer_id, writes exactly one record under answer.RefusalMessage at WARNING
// naming that id, the code, the name the request named and the caller's class, and is counted on
// the metrics port. A reread of a well-formed name the store does not hold, sent with no proof,
// is no refusal: it answers 202 with accepted and the fixed reason, the answer every such reread
// gets whatever it names, writes no record and is counted served.
func TestTheBinaryRecordsARefusalAndCountsEveryAnswer(t *testing.T) {
	broker := startBroker(t, brokerOptions{Env: refusedSecretEnv(t, storetest.URL(t))})
	metricsAddr := broker.MetricsAddr(t)

	status, header, body := post(t, "http://"+broker.Addr+"/v1/secrets/bad%20name/reread")
	id := header.Get(answer.Header)
	if status != http.StatusBadRequest || body["code"] != "SECRET_NAME_INVALID" {
		t.Fatalf("POST /v1/secrets/bad%%20name/reread = %d %v; want 400 SECRET_NAME_INVALID", status, body)
	}
	if len(id) != 32 || body["answer_id"] != id {
		t.Fatalf("the refusal carries answer id %q in its header and %v in its body; want one id in both", id, body["answer_id"])
	}
	record := broker.AwaitRecord(t, answer.RefusalMessage, map[string]any{"answer_id": id})
	want := map[string]any{
		"severity": "WARNING", "status": 400, "code": "SECRET_NAME_INVALID", "method": http.MethodPost,
		"route": "POST /v1/secrets/{name}/reread", "caller_class": answer.CallerPublic, "caller": "", "secrets": []any{"bad name"},
	}
	if !carries(record, want) {
		t.Errorf("the refusal's record is %v; want it to carry %v", record, want)
	}
	if stamp, _ := record["time"].(string); stamp == "" {
		t.Errorf("the refusal's record carries no time: %v", record)
	}
	if message, _ := body["error"].(string); message == "" || recordQuotes(record, message) {
		t.Errorf("the refusal's record quotes its message %q, or the refusal said none: %v", message, record)
	}
	refused := `outcome="refused",code="SECRET_NAME_INVALID",caller_class="public"`
	if got := scrapeMetric(t, metricsAddr, "broker_answers_total")[refused]; got != 1 {
		t.Errorf("broker_answers_total{%s} = %v after one such refusal; want 1", refused, got)
	}

	served := `outcome="served",code="HTTP_202",caller_class="public"`
	before := scrapeMetric(t, metricsAddr, "broker_answers_total")[served]
	status, header, body = post(t, "http://"+broker.Addr+"/v1/secrets/NO_SUCH_SECRET/reread")
	absent := header.Get(answer.Header)
	if status != http.StatusAccepted || body["accepted"] != true || body["reason"] != api.RereadAcceptedReason || len(body) != 2 || len(absent) != 32 {
		t.Fatalf("POST /v1/secrets/NO_SUCH_SECRET/reread = %d %v with answer id %q; want 202, accepted and the fixed reason and nothing else, and an id", status, body, absent)
	}
	// A refusal sent after it is recorded after it, so once that record is in the log a record
	// of the absent reread would be too.
	_, after, _ := post(t, "http://"+broker.Addr+"/v1/nowhere")
	broker.AwaitRecord(t, answer.RefusalMessage, map[string]any{"answer_id": after.Get(answer.Header), "code": "HTTP_404"})
	if found := recordsWith(broker.Log(), answer.RefusalMessage, map[string]any{"answer_id": absent}); len(found) != 0 {
		t.Errorf("the reread of an absent name wrote %v; want no record", found)
	}
	if found := recordsWith(broker.Log(), answer.RefusalMessage, map[string]any{"answer_id": id}); len(found) != 1 {
		t.Errorf("the refusal wrote %d records; want 1: %v", len(found), found)
	}
	if got := scrapeMetric(t, metricsAddr, "broker_answers_total")[served]; got != before+1 {
		t.Errorf("broker_answers_total{%s} went from %v to %v over the absent reread; want one more", served, before, got)
	}
}

// TestTheBinaryKeepsARefusalsNameThatHoldsAByteItEscapes drives the REAL compiled binary with an
// unauthenticated reread of "%1Agent-secrets-placeholder-": a name that is the control byte 0x1A
// and "gent-secrets-placeholder-", which is no placeholder. The broker's handler writes the name
// as "\u001agent-secrets-placeholder-", and a mask that read that escaped text, not the name it
// spells, would see a placeholder from the escape's last digit on and break the escape, and
// slog, or a reader of the line, would then lose the whole value. The refusal's record names the
// one secret its request named, as it named it, and every line of the broker's log is one JSON
// object.
func TestTheBinaryKeepsARefusalsNameThatHoldsAByteItEscapes(t *testing.T) {
	broker := startBroker(t, brokerOptions{Env: refusedSecretEnv(t, storetest.URL(t))})
	status, header, body := post(t, "http://"+broker.Addr+"/v1/secrets/%1Agent-secrets-placeholder-/reread")
	id := header.Get(answer.Header)
	if status != http.StatusBadRequest || body["code"] != "SECRET_NAME_INVALID" || len(id) != 32 {
		t.Fatalf("POST /v1/secrets/%%1Agent-secrets-placeholder-/reread = %d %v with answer id %q; want 400 SECRET_NAME_INVALID and an id", status, body, id)
	}
	record := broker.AwaitRecord(t, answer.RefusalMessage, map[string]any{"answer_id": id})
	if want := []any{"\x1agent-secrets-placeholder-"}; !reflect.DeepEqual(record["secrets"], want) {
		t.Errorf("the refusal's record names the secrets %#v; want %#v, the name its request named: %v", record["secrets"], want, record)
	}
	for line := range strings.Lines(broker.Log()) {
		if !json.Valid([]byte(strings.TrimSuffix(line, "\n"))) {
			t.Errorf("a line of the broker's log is no JSON object: %q", line)
		}
	}
}

// post POSTs nothing to target and answers the status, the headers and the body as a JSON object
// (nil for a body that is not one).
func post(t *testing.T, target string) (int, http.Header, map[string]any) {
	t.Helper()
	resp, err := http.Post(target, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer to POST %s: %v", target, err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, resp.Header, body
}

// recordQuotes reports whether any string field of record holds text, at any depth: each field
// as the record's JSON decoded it, so no escaping of the encoding can hide the text.
func recordQuotes(record any, text string) bool {
	switch field := record.(type) {
	case string:
		return strings.Contains(field, text)
	case []any:
		return slices.ContainsFunc(field, func(item any) bool { return recordQuotes(item, text) })
	case map[string]any:
		for _, value := range field {
			if recordQuotes(value, text) {
				return true
			}
		}
	}
	return false
}
