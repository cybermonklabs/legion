package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/redeliver"
)

// A decision's GUID, event and action are GitHub's text, so the table of redeliver-webhooks shows
// each as githubapp.ShownBody shows it: past 512 characters, the first 512 and the text's size,
// and never the text whole. The event column is the event, a dot and the action. Each text here
// is 600 bytes of a letter of its own, so no text holds another.
func TestRedeliveryReportShowsGitHubsTextToABound(t *testing.T) {
	guid, event, action := strings.Repeat("g", 600), strings.Repeat("e", 600), strings.Repeat("a", 600)
	var out bytes.Buffer
	writeRedeliveryReport(&out, redeliver.Report{
		Since:  time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC),
		Listed: 1,
		Decisions: []redeliver.Decision{{
			GUID:         guid,
			DeliveryID:   7,
			DeliveredAt:  time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC),
			Event:        event,
			Action:       action,
			RepositoryID: 42,
			StatusCode:   502,
			Attempts:     1,
			Outcome:      redeliver.Redelivered,
		}},
		Complete: true,
	})
	text := out.String()
	for _, whole := range []struct{ name, text string }{{"GUID", guid}, {"event", event}, {"action", action}} {
		if githubapp.ShownBody(whole.text) == whole.text {
			t.Fatalf("the %s is %d bytes, which ShownBody shows whole; the test needs one it cuts", whole.name, len(whole.text))
		}
		if strings.Contains(text, whole.text) {
			t.Errorf("the report holds the %d-byte %s whole; want it only as ShownBody shows it", len(whole.text), whole.name)
		}
	}
	// The table pads each column but the last with at least two spaces, and ShownBody's words
	// hold single spaces only, so a column is its text between two runs of two spaces.
	for _, column := range []struct{ name, text string }{
		{"the GUID", githubapp.ShownBody(guid)},
		{"the event, a dot and the action", githubapp.ShownBody(event) + "." + githubapp.ShownBody(action)},
	} {
		if !strings.Contains(text, "  "+column.text+"  ") {
			t.Errorf("the report %.200q… has no column of %s as ShownBody shows each, %.40q… ending %q", text, column.name, column.text, column.text[max(0, len(column.text)-40):])
		}
	}
}
