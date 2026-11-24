package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// shapeWindowLine is one window.jsonl line, an event as Dispatch served it, whose created_at is
// written: a time no decoder reads, where whatever answered in Dispatch's place wrote words of
// its own.
func shapeWindowLine(createdAt string) []byte {
	line, err := json.Marshal(map[string]any{"id": 1, "project": "PRJ", "seq": 1,
		"type": evIssueCreated, "actor": map[string]any{"kind": "session", "id": "s0"},
		"created_at": createdAt, "payload": map[string]any{}})
	if err != nil {
		panic(err)
	}
	return line
}

// TestADecodeOfDispatchsCachedBytesNamesNoneOfThem: replay-fetch's cache and a replay bundle hold
// Dispatch's answers as they came, and whatever answered in Dispatch's place can write the
// fetch's bearer and a credential's URL into a time of theirs; the decoder's error quotes a time
// it could not read whole, and the readers of those bytes printed it with %w (readBundle,
// parseWindow, parseReplayEvent, fetchLog's page, docText, the issue listing, an artifact's
// blocks). Every one of them decodes through decodeCached now, whose error names what was read
// and the rule's own reading of the failure (redact.DecodeError): the destination's type and the
// member that did not decode, and no value of the bytes. The control: the error still names the
// line of the window it refused. Captures loadtest.answer-members/cached-decode.
func TestADecodeOfDispatchsCachedBytesNamesNoneOfThem(t *testing.T) {
	line := shapeWindowLine(memberBearer + "-" + redacttest.Code)
	t.Run("window", func(t *testing.T) {
		_, err := parseWindow(line)
		redacttest.RequireNoPiece(t, "loadtest.answer-members/cached-decode", errorText(err), memberBearer)
		if !strings.Contains(errorText(err), "line 1: the event is not the shape replay-fetch reads") {
			t.Errorf("parseWindow = %v, want an error naming the line it refused", err)
		}
	})
	t.Run("twin", func(t *testing.T) {
		var ev replayEvent
		err := json.Unmarshal(line, &ev)
		if err == nil || !strings.Contains(err.Error(), memberBearer) {
			t.Fatalf("encoding/json reads the line as %v, want an error quoting the created_at it could not read", err)
		}
	})
}
