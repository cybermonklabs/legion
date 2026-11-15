//go:build e2e

// The reads part two of dispatch://INFRA-175's c-live (approved_version_news_sessions_live_test.go)
// makes of a session's transcript, held against fixture transcripts written as Oh My Pi writes a
// session file: which Envoy card counts as a holder told of a new version, and which call of the
// architect's counts as its own envoy_publish to that holder's role topic. A run of real sessions
// costs forty-five minutes of model calls, so these reads are proved here, where each wrong
// message is one line.
package daemon

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/notify"
)

// sessionsFixture is a session file holding entries, one JSON line each.
func sessionsFixture(t *testing.T, entries ...map[string]any) []byte {
	t.Helper()
	var body bytes.Buffer
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(append(line, '\n'))
	}
	return body.Bytes()
}

// sessionsFixtureCall is an assistant message holding one call of the tool name with args.
func sessionsFixtureCall(name string, args map[string]any) map[string]any {
	return map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "toolCall", "id": "toolu_1", "name": name, "arguments": args},
	}}}
}

// sessionsFixtureCard is an Envoy message card holding text, as the plugin writes one a session
// was sent.
func sessionsFixtureCard(text string) map[string]any {
	return map[string]any{"type": "custom_message", "customType": "envoy-message", "content": text}
}

// The architect's message reaches the holder only as its own envoy_publish to that holder's role
// topic naming version 2 and the change: the tool, its device and eval code that calls it count,
// and a publish to another topic, another way of sending, or a message missing the version or the
// change does not.
func TestApprovedVersionNewsCountsOnlyTheArchitectsPublishToTheHoldersRoleTopic(t *testing.T) {
	topic := notify.RoleTopicPrefix + "as0001-as0001-2-reviewer"
	other := notify.RoleTopicPrefix + "as0001-as0001-1-architect"
	told := "Version 2 of the Verification document of AS0001-2 changes " + sessionsCheckID + ": the read now answers 200 with the page it read."
	device, err := json.Marshal(map[string]string{"topic": topic, "message": told})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		entry map[string]any
		want  bool
	}{
		{"the tool itself, to the reviewer's role topic", sessionsFixtureCall("envoy_publish", map[string]any{"topic": topic, "message": told}), true},
		{"its xd:// device", sessionsFixtureCall("write", map[string]any{"path": "xd://envoy_publish", "content": string(device)}), true},
		{"eval code that calls it", sessionsFixtureCall("eval", map[string]any{"language": "js",
			"code": "await tool.envoy_publish({ topic: " + strconv.Quote(topic) + ", message: " + strconv.Quote(told) + " });"}), true},
		{"a publish to another role topic", sessionsFixtureCall("envoy_publish", map[string]any{"topic": other, "message": told}), false},
		{"envoy_send to the reviewer's session", sessionsFixtureCall("envoy_send", map[string]any{"session_id": "ses_reviewer", "message": told}), false},
		{"a Dispatch comment", sessionsFixtureCall("bash", map[string]any{"command": "dispatch comment --issue AS0001-2 --body " + strconv.Quote(told)}), false},
		{"a publish that names the change and no version", sessionsFixtureCall("envoy_publish", map[string]any{"topic": topic,
			"message": "The Verification document of AS0001-2 changes " + sessionsCheckID + "."}), false},
		{"a publish that names version 2 and no change", sessionsFixtureCall("envoy_publish", map[string]any{"topic": topic,
			"message": "Version 2 of the Verification document of AS0001-2 is written."}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := sessionsReadTranscript(sessionsFixture(t, tc.entry))
			call, got := read.publishedTo(topic, sessionsChecksChange...)
			if got != tc.want {
				t.Fatalf("publishedTo(%s) = %+v, %t over the architect's transcript of %s; want %t", topic, call, got, tc.name, tc.want)
			}
		})
	}
}

// A holder told of the off step's version 2 must see the change named: its new words, or the
// changed section's heading as the news names it or as the spec writes it. The word "design"
// alone names nothing, since the news itself is about the approved design.
func TestApprovedVersionNewsCardNamesTheChangedSectionNotTheWordDesign(t *testing.T) {
	for _, tc := range []struct {
		card string
		want bool
	}{
		{"Version 2 of the approved design of AS0001-1 is out: read it before you go on.", false},
		{"Version 2 of the spec of AS0001-1 changed \"Design\".", true},
		{"Version 2 of the spec of AS0001-1 rewrites the ## Design section.", true},
		{"Version 2 of the spec of AS0001-1: the reader now takes two pages at a time.", true},
		{"The spec of AS0001-1 now has the reader take two pages at a time.", false},
	} {
		t.Run(tc.card, func(t *testing.T) {
			read := sessionsReadTranscript(sessionsFixture(t, sessionsFixtureCard(tc.card)))
			if got := read.toldOf(sessionsSpecChange...); (got != "") != tc.want {
				t.Fatalf("toldOf over the card %q = %q; want a card: %t", tc.card, got, tc.want)
			}
		})
	}
}
