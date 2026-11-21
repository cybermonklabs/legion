package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

const logContentMarker = "the-body-no-required-mode-line-logs"

// recordLine writes one "listener send" record of a message whose payload summary holds the
// marker, under privacy and decision, and answers the line it wrote.
func recordLine(t *testing.T, privacy *listenerPrivacy, decision *scopeDecision) string {
	t.Helper()
	var out bytes.Buffer
	rec := &requestRecord{logger: logging.NewWithWriter("test-machine", &out), line: "listener send", laneKey: "target_session", privacy: privacy, decision: decision}
	item := contracts.Envelope{EventID: "evt-1", Source: "agent", SourceSession: "ses_a", PayloadSummary: logContentMarker}
	rec.item = &item
	rec.write(200, outcomeAccepted)
	return out.String()
}

// receivedLine writes the "listener received" line of the same message under privacy, through the
// durable delivery handler's own writer (logReceived), and answers it.
func receivedLine(privacy *listenerPrivacy) string {
	var out bytes.Buffer
	item := contracts.Envelope{EventID: "evt-1", Source: "agent", SourceSession: "ses_a", PayloadSummary: logContentMarker}
	logReceived(listenerDeliveryHandlerConfig{logger: logging.NewWithWriter("test-machine", &out), privacy: privacy}, item)
	return out.String()
}

// TestNoListenerLogLineCarriesContentWithPrivateProjects: in the required mode neither the
// "listener send" record nor the "listener received" line writes a message's text, whatever the
// privacy answer lists, private projects or none; each still writes the event id. Control: in the
// all-public mode both write it, as at the merge base, so the row fails for the mode it names.
func TestNoListenerLogLineCarriesContentWithPrivateProjects(t *testing.T) {
	source := newPrivacySource(t, privacyListingPRIV)
	required := &listenerPrivacy{required: true, counters: newPrivacyCounters(metrics.New())}
	required.k7 = testK7Reader(source.server.URL, k7ReadTimeout)
	decided := func() *scopeDecision {
		decision := &scopeDecision{ctx: context.Background(), privacy: required}
		_, _ = decision.answer()
		return decision
	}
	for _, answer := range []string{privacyListingPRIV, privacyNone} {
		source.answer(answer)
		for name, line := range map[string]string{"listener send": recordLine(t, required, decided()), "listener received": receivedLine(required)} {
			if strings.Contains(line, logContentMarker) || !strings.Contains(line, `"event_id":"evt-1"`) {
				t.Errorf("required mode, answer %s: the %s line is %s; want the event id and no text", answer, name, line)
			}
		}
	}
	for name, line := range map[string]string{"listener send": recordLine(t, nil, nil), "listener received": receivedLine(nil)} {
		if !strings.Contains(line, logContentMarker) {
			t.Errorf("control, all-public: the %s line is %s; want the text, as at the merge base", name, line)
		}
	}
}
