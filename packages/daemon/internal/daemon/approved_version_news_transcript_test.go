// The reads part two of dispatch://INFRA-175's c-live (approved_version_news_sessions_live_test.go,
// built with -tags e2e) makes of a session's transcript, and the tests that hold them against
// fixture transcripts written as Oh My Pi writes a session file: which Envoy card counts as a
// holder told of a new version, and which call of the architect's counts as its own envoy_publish
// to that holder's role topic. A run of real sessions costs forty-five minutes of model calls, so
// these reads are proved here, where each wrong message is one line. The file is untagged so `go
// test` runs these tests on every run: a change that breaks a read fails this package's tests, not
// the live proof the next time its sessions run.
package daemon

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/notify"
)

// sessionsCheckID is the id of the one check of A4's child.
const sessionsCheckID = "c-reader-pages"

// The words a message must name of each step's change, any one of them, beside version 2.
var (
	// sessionsChecksChange is A4's: the changed check, or its new words.
	sessionsChecksChange = []string{sessionsCheckID, "with the page it read"}
	// sessionsSpecChange is the off step's: the new words, or the changed section's heading as the
	// news names it (`"Design"`) or as the spec writes it (`## Design`). The heading's word alone
	// names nothing, since "the approved design" holds it too.
	sessionsSpecChange = []string{`"Design"`, "## Design", "two pages"}
)

// sessionsVersion2 is a text naming version 2 of a document as a word of its own.
var sessionsVersion2 = regexp.MustCompile(`(?i)\b(version\s*2|v2)\b`)

// sessionsTranscript is what one session's own transcript holds, as this proof reads it: the text
// of each Envoy message card, of each prompt it was given, and each tool call its model made.
type sessionsTranscript struct {
	envoy []string
	user  []string
	calls []sessionsCall
}

// sessionsCall is one tool call: the tool, its arguments, and the call as its transcript writes it.
// A call through the tool's xd:// device is the tool's own; eval code is the tool `eval`, whose
// code is its text.
type sessionsCall struct {
	name string
	args map[string]any
	text string
}

// sessionsReadTranscript is what body, a session file of Oh My Pi's, holds as this proof reads it:
// one JSON entry a line, and a line that is no entry skipped.
func sessionsReadTranscript(body []byte) sessionsTranscript {
	var read sessionsTranscript
	for _, line := range bytes.Split(body, []byte("\n")) {
		var entry struct {
			Type       string          `json:"type"`
			CustomType string          `json:"customType"`
			Content    json.RawMessage `json:"content"`
			Message    *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		switch {
		case entry.Type == "custom_message" && entry.CustomType == "envoy-message":
			read.envoy = append(read.envoy, sessionsText(entry.Content))
		case entry.Type == "message" && entry.Message != nil && entry.Message.Role == "user":
			read.user = append(read.user, sessionsText(entry.Message.Content))
		case entry.Type == "message" && entry.Message != nil && entry.Message.Role == "assistant":
			var parts []struct {
				Type      string          `json:"type"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(entry.Message.Content, &parts)
			for _, part := range parts {
				if part.Type == "toolCall" {
					read.calls = append(read.calls, sessionsToolCall(part.Name, part.Arguments))
				}
			}
		}
	}
	return read
}

// sessionsText is a message's content as text: the string itself, or its text parts joined.
func sessionsText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(content, &parts)
	said := make([]string, 0, len(parts))
	for _, part := range parts {
		said = append(said, part.Text)
	}
	return strings.Join(said, "\n")
}

// sessionsToolCall is one call as this proof reads it, a device write read as its tool's call.
func sessionsToolCall(name string, raw json.RawMessage) sessionsCall {
	var args map[string]any
	_ = json.Unmarshal(raw, &args)
	if path, _ := args["path"].(string); name == "write" && strings.HasPrefix(strings.TrimSpace(path), "xd://") {
		content, _ := args["content"].(string)
		var inner map[string]any
		_ = json.Unmarshal([]byte(content), &inner)
		return sessionsCall{name: strings.TrimPrefix(strings.TrimSpace(path), "xd://"), args: inner, text: content}
	}
	if code, _ := args["code"].(string); name == "eval" {
		return sessionsCall{name: name, args: args, text: code}
	}
	return sessionsCall{name: name, args: args, text: string(raw)}
}

// calling is every call of tool, by itself, its device, or eval code that names it.
func (read sessionsTranscript) calling(tool string) []sessionsCall {
	var found []sessionsCall
	for _, call := range read.calls {
		if call.name == tool || (call.name == "eval" && strings.Contains(call.text, tool)) {
			found = append(found, call)
		}
	}
	return found
}

// toldOf is the first Envoy message card that names version 2 and any of change, and "" when none
// does. A card says nothing of who sent it or how: publishedTo reads that from the sender's own
// transcript.
func (read sessionsTranscript) toldOf(change ...string) string {
	for _, card := range read.envoy {
		if sessionsNamesChange(card, change...) {
			return card
		}
	}
	return ""
}

// publishedTo is the first envoy_publish call to topic whose message names version 2 and any of
// change, and false when none does. A call of the tool or of its xd:// device is read by its
// arguments, `topic` and `message`; eval code that calls it is read whole, so its code must hold
// the topic and the words. A message sent another way, such as envoy_send to a session or a
// Dispatch comment, is not a publish to the holder's role topic, whatever card it left.
func (read sessionsTranscript) publishedTo(topic string, change ...string) (sessionsCall, bool) {
	for _, call := range read.calling("envoy_publish") {
		to, _ := call.args["topic"].(string)
		message, _ := call.args["message"].(string)
		if call.name == "eval" && strings.Contains(call.text, topic) {
			to, message = topic, call.text
		}
		if strings.TrimSpace(to) == topic && sessionsNamesChange(message, change...) {
			return call, true
		}
	}
	return sessionsCall{}, false
}

// sessionsNamesChange says whether text names version 2 and any of change, the words of the change
// in any case.
func sessionsNamesChange(text string, change ...string) bool {
	if !sessionsVersion2.MatchString(text) {
		return false
	}
	lower := strings.ToLower(text)
	for _, words := range change {
		if strings.Contains(lower, strings.ToLower(words)) {
			return true
		}
	}
	return false
}

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
