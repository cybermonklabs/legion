// The reads part two of dispatch://INFRA-175's c-live (approved_version_news_sessions_live_test.go,
// built with -tags e2e) makes of a session's transcript and of the comments on an issue, the word
// each of its runs ends with, and the tests that hold them against fixture transcripts written as
// Oh My Pi writes a session file: which Envoy card counts as a holder told of a new version, which
// call of the architect's counts as its own envoy_publish to that holder's role topic, which
// comment counts as a parent's approving review of a child's new version, which tool result is the
// plugin refusing a daemon answer, and which word a run's line prints, which the script counts. A
// run of real sessions costs forty-five minutes of model calls, so these reads are proved here,
// where each wrong message is one line. The file is untagged so `go test` runs these tests on
// every run: a change that breaks a read fails this package's tests, not the live proof the next
// time its sessions run.
package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/notify"
)

// sessionsOutcome is how one run of a step ended: the word its `run` line prints, which the script
// counts. Only a run that reached the step's assertion counts for its arm. A run that stopped
// before its assertion, or that the harness failed, says nothing about the prompts the arms differ
// in, so a base arm that lost a run that way has not failed.
type sessionsOutcome string

const (
	// sessionsPassed is a run whose assertion held.
	sessionsPassed sessionsOutcome = "PASS"
	// sessionsFailed is a run whose assertion did not hold: the sessions did not do what the step
	// asks of the arm's prompts.
	sessionsFailed sessionsOutcome = "FAIL"
	// sessionsSetup is a run that stopped before its assertion, on what the step needs first: a
	// holder that did not end its start turn in the run's bound, a reviewer the daemon never gave
	// its news, an architect that did not write the version the operator's task asks for, or, in
	// the a4 step, a child's architect that did not tell its parent of that version, so that no
	// parent reviewed it. The assertion of the a4 and off steps is the telling of that version,
	// which the head's prompt puts to a child's architect only once its parent's review approved
	// it, so a run in which it was never written, or never reviewed, never tried the telling, in
	// either arm.
	sessionsSetup sessionsOutcome = "SETUP"
	// sessionsHarness is a run the harness failed, which ends the proof in either arm: a fault of
	// the harness before or after the step's own outcome, up to the daemon's stop and the closes
	// of the run's cleanups (sessionsRunLine.printAtEnd), or a session whose `legion` tool could
	// not read the daemon (sessionsEnding).
	sessionsHarness sessionsOutcome = "HARNESS"
)

// sessionsUnwritten is why a run stopped before its assertion when Dispatch holds no version 2 of
// document with the operator's new words, words: held is whether Dispatch holds a version 2, and
// text is that version as Dispatch answers it. It is "" when the version holds the words.
func sessionsUnwritten(document string, held bool, text, words string) string {
	if held && strings.Contains(text, words) {
		return ""
	}
	return fmt.Sprintf("Dispatch holds no version 2 of %s with the new words: the architect did not make the edit the operator's task asks for, "+
		"so the run did not reach its assertion", document)
}

// sessionsJudged is the outcome of a step whose assertion is the telling of a version 2 that the
// operator's task asks the architect to write. A run that stopped before that assertion (stopped,
// the reason, is not ""), on a version the architect did not write (sessionsUnwritten) or, in the
// a4 step, one its parent never reviewed because the child's architect never told it of the
// version, is SETUP whatever else it shows, and counts for neither arm. Otherwise the run reached
// its assertion: it is FAIL, naming each part of it that did not hold (missed), or PASS.
func sessionsJudged(stopped string, missed []string) (sessionsOutcome, string) {
	switch {
	case stopped != "":
		return sessionsSetup, stopped
	case len(missed) > 0:
		return sessionsFailed, strings.Join(missed, "; ")
	}
	return sessionsPassed, ""
}

// sessionsEnding is the word and the reason a run's line prints. outcome and why are what the
// step's body answered, or HARNESS and its reason when the harness stopped the body first; failed
// is whether the run's test failed; refused names each session whose transcript holds the plugin's
// refusal of a daemon answer (refusedByContract). A refused session makes the run HARNESS whatever
// the body answered: its `legion` tool could not read the daemon, so the session worked without
// the record its prompt tells it to read, and the run says nothing of the prompts. A test that
// failed after the body answered makes the run HARNESS too: the proof ends at that failure, so the
// script must not count the body's word.
func sessionsEnding(outcome sessionsOutcome, why string, failed bool, refused []string) (sessionsOutcome, string) {
	switch {
	case len(refused) > 0:
		return sessionsHarness, "the legion plugin refused a daemon answer as a body the daemon API contract does not describe, " +
			"so these sessions ran with a `legion` tool that could not read the daemon: " + strings.Join(refused, "; ")
	case failed && outcome != sessionsHarness:
		ended := string(outcome)
		if outcome != sessionsPassed {
			ended += " (" + why + ")"
		}
		return sessionsHarness, "the harness failed after the step ended " + ended + "; the test log above names the failure"
	}
	return outcome, why
}

// sessionsRunLine is what a run's line prints from: the word and the reason the step's body
// answered, and each session whose transcript holds the plugin's refusal of a daemon answer
// (refusedByContract).
type sessionsRunLine struct {
	outcome sessionsOutcome
	why     string
	refused []string
}

// newSessionsRunLine is a run's line before its step answers. It is HARNESS, since a run the
// harness stops before its step answers holds no word of the step's.
func newSessionsRunLine() *sessionsRunLine {
	return &sessionsRunLine{outcome: sessionsHarness, why: "the harness failed; its log is above"}
}

// printAtEnd prints the line of the run called name when t, the run's subtest, ends, with the
// run's time and the model calls it made, as calls counts them. The print is a cleanup of t, and
// the run registers it first, so it runs last: a test runs its cleanups in the reverse order of
// their registration. The line then prints after the sessions end and their transcripts are kept,
// and after every cleanup the run registers later, the daemon's stop and the store's close among
// them, whichever of them stops the subtest. So a fault in any of them makes the line HARNESS
// (sessionsEnding), and the script does not count the step's word.
func (line *sessionsRunLine) printAtEnd(t *testing.T, name string, calls func() int64) {
	started, before := time.Now(), calls()
	t.Cleanup(func() {
		line.outcome, line.why = sessionsEnding(line.outcome, line.why, t.Failed(), line.refused)
		said := string(line.outcome)
		if line.outcome != sessionsPassed {
			said += ": " + line.why
		}
		fmt.Printf("run %s: %s in %ds, %d model calls\n", name, said, int(time.Since(started).Seconds()), calls()-before)
	})
}

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

// sessionsApprovedReview is the first of comments, the bodies of the comments on child, that
// approves version 2 of child's Verification document in the form a parent architect posts its
// review of a child's spec and checks (skill://legion-architect/references/decomposition.md,
// "Reviewing a child's spec"): a line `Spec review of <child> v<n> by <parent>: approved`, and a
// line opening `Verification document v2`. It is "" when none does. The a4 step reads back with
// it the review it posts as the child's parent (reviewAsParent), so a review whose words leave
// that form fails the run's harness rather than leaving the child's architect waiting for an
// approval it never got. A review that requests changes, one of another version of the
// Verification document, one by another issue's architect or of another child, and a sentence
// that quotes the form are no such approval.
func sessionsApprovedReview(child, parent string, comments []string) string {
	verdict := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta("Spec review of "+child+" v") + `\d+` + regexp.QuoteMeta(" by "+parent+": approved") + `$`)
	read := regexp.MustCompile(`(?m)^Verification document v2\b`)
	for _, body := range comments {
		if verdict.MatchString(body) && read.MatchString(body) {
			return body
		}
	}
	return ""
}

// sessionsTranscript is what one session's own transcript holds, as this proof reads it: the text
// of each Envoy message card, of each prompt it was given, each tool call its model made, and the
// text of each tool result it was answered.
type sessionsTranscript struct {
	envoy   []string
	user    []string
	calls   []sessionsCall
	results []string
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
		case entry.Type == "message" && entry.Message != nil && entry.Message.Role == "toolResult":
			read.results = append(read.results, sessionsText(entry.Message.Content))
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

// sessionsContractRefusal is the refusal the plugin's daemon client throws when the daemon answers
// a route with a body the daemon API contract does not describe (LegionDaemonContractError,
// packages/pi-shared/src/daemon-client.ts), to the end of its line: the route, the status and the
// field that is wrong. A prompt quotes the clause without a route and a status
// (prompts/go/worker-common.md), so a session that read its prompt back holds no such line.
var sessionsContractRefusal = regexp.MustCompile(`[A-Z]+ /\S* answered \d+ with a body the daemon API contract does not describe: [^\n]*`)

// refusedByContract is the first refusal of a daemon answer (sessionsContractRefusal) that a tool
// result of the session holds, and "" when none does. Only a tool's result counts: that is what
// the session was answered, by the `legion` tool itself or by eval code that called it. A model's
// own text that quotes a refusal is no refusal.
func (read sessionsTranscript) refusedByContract() string {
	for _, result := range read.results {
		if refusal := sessionsContractRefusal.FindString(result); refusal != "" {
			return refusal
		}
	}
	return ""
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

// sessionsFixtureResult is the result of a call of tool holding text, as Oh My Pi writes the
// answer a session's model was given, refused when isError.
func sessionsFixtureResult(tool, text string, isError bool) map[string]any {
	return map[string]any{"type": "message", "message": map[string]any{
		"role": "toolResult", "toolCallId": "toolu_1", "toolName": tool, "isError": isError,
		"content": []any{map[string]any{"type": "text", "text": text}},
	}}
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

// The a4 step reads back the review it posts as the child's parent, and only an approval of
// version 2 of the child's Verification document in the form a parent posts its review counts,
// since the head's prompt has the child's architect tell its workers of a version only after that
// approval. A review that requests changes, one that read another version, one by another issue's
// architect, one of another child, and a sentence that quotes the form are no such approval: read
// back as one, a review the child's architect rightly took for no approval would make a run whose
// architect rightly waited read as a FAIL of its prompt.
func TestApprovedVersionNewsCountsOnlyTheParentsApprovalOfVersion2(t *testing.T) {
	const child, parent = "AS0001-2", "AS0001-1"
	const reasons = "\n\nRead against the root spec at its approved version, with a fresh reviewer.\n"
	for _, tc := range []struct {
		name, comment string
		want          bool
	}{
		{"the approval a live parent posted in round 12", "Spec review of AS0001-2 v1 by AS0001-1: approved\nVerification document v2" + reasons, true},
		{"a review that requests changes", "Spec review of AS0001-2 v1 by AS0001-1: changes requested\nVerification document v2" + reasons, false},
		{"an approval of version 1", "Spec review of AS0001-2 v1 by AS0001-1: approved\nVerification document v1" + reasons, false},
		{"an approval by another issue's architect", "Spec review of AS0001-2 v1 by AS0001-9: approved\nVerification document v2" + reasons, false},
		{"an approval of another child", "Spec review of AS0001-3 v1 by AS0001-1: approved\nVerification document v2" + reasons, false},
		{"a sentence that quotes the form", "Version 2 waits for `Spec review of AS0001-2 v1 by AS0001-1: approved` and a line naming `Verification document v2`.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comments := []string{"Starting the review of version 2.", tc.comment}
			if got := sessionsApprovedReview(child, parent, comments); (got != "") != tc.want {
				t.Fatalf("sessionsApprovedReview(%s, %s) over %q = %q; want an approval: %t", child, parent, tc.comment, got, tc.want)
			}
		})
	}
}

// A session whose `legion` tool could not read the daemon holds the plugin's refusal in a tool's
// result: the tool's own, or the result of eval code that called it. The clause a prompt quotes,
// read back, a model's own text that quotes a refusal, and a result the tool answered are none.
func TestApprovedVersionNewsReadsThePluginsRefusalOfADaemonAnswer(t *testing.T) {
	const refusal = "GET /legion/v1/state answered 200 with a body the daemon API contract does not describe: " +
		"settings.review_round_cap: Too small: expected number to be >0"
	quoted := map[string]any{"type": "message", "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "text", "text": "The legion tool said: " + refusal + ". I report it to the controller."},
	}}}
	for _, tc := range []struct {
		name  string
		entry map[string]any
		want  string
	}{
		{"the legion tool's own result", sessionsFixtureResult("legion", refusal, true), refusal},
		{"the result of eval code that called it", sessionsFixtureResult("eval", "Error: "+refusal+"\n    at tool.legion (cell 1)", true), refusal},
		{"a read of the prompt, which quotes the clause", sessionsFixtureResult("read", "A `legion` call your plugin refuses because the "+
			"daemon's answer is not the shape it reads — the tool says the body is one the daemon API contract does not describe — "+
			"answers the same way however often you call it again", false), ""},
		{"the model's own text, which quotes the refusal", quoted, ""},
		{"a record the tool read", sessionsFixtureResult("legion", `{"issue": "AS0001-2", "phase": "reviewing"}`, false), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionsReadTranscript(sessionsFixture(t, tc.entry)).refusedByContract(); got != tc.want {
				t.Fatalf("refusedByContract over %s = %q; want %q", tc.name, got, tc.want)
			}
		})
	}
}

// The a4 and off steps assert the telling of a version 2 that the operator's task asks the
// architect to write. A run whose version is not on Dispatch with the task's new words stopped
// before that assertion: it is SETUP whatever else it shows, so it fails a head run by name and
// never counts for the base arm. Only a run whose version holds the words is judged on its
// assertion.
func TestApprovedVersionNewsAVersionTheArchitectDidNotWriteStopsTheRunBeforeItsAssertion(t *testing.T) {
	// The check's words before and after the edit, as the a4 step writes them.
	const document, before, after = "AS0001-2's Verification document", "The read answers 200.", "The read answers 200 with the page it read."
	written := `{"number": 2, "content": "- c-reader-pages: ` + after + `"}`
	missed := []string{"the reviewer of AS0001-2 was sent no Envoy message naming version 2 and the change"}
	for _, tc := range []struct {
		name   string
		held   bool
		text   string
		missed []string
		want   sessionsOutcome
	}{
		{"no version 2, and no message", false, `{"error": "not found"}`, missed, sessionsSetup},
		{"no version 2, though the architect sent the message", false, `{"error": "not found"}`, nil, sessionsSetup},
		{"a version 2 without the new words", true, `{"number": 2, "content": "- c-reader-pages: ` + before + `"}`, missed, sessionsSetup},
		{"the version, and no message", true, written, missed, sessionsFailed},
		{"the version, and the message", true, written, nil, sessionsPassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := sessionsJudged(sessionsUnwritten(document, tc.held, tc.text, after), tc.missed)
			if got != tc.want {
				t.Fatalf("the run of %s ended %s (%q); want %s", tc.name, got, why, tc.want)
			}
			if got == sessionsSetup && !strings.Contains(why, "Dispatch holds no version 2 of "+document) {
				t.Errorf("the run of %s ended SETUP saying %q, which does not name the version 2 of %s it lacks", tc.name, why, document)
			}
		})
	}
}

// A run's line prints the word the script counts. The step's own word stands when its test did
// not fail. A failure after the step answered, or a session whose `legion` tool could not read the
// daemon, makes the run HARNESS, named so: the script counts it for neither arm, and the proof ends
// at it. A harness that stopped the step first keeps its own reason.
func TestApprovedVersionNewsARunLinePrintsHarnessForAFaultAfterItsStep(t *testing.T) {
	const stopped = "the harness failed; its log is above"
	refused := []string{`legion-as0001-as0001-2-reviewer: "GET /legion/v1/state answered 200 with a body the daemon API contract does not describe: …"`}
	for _, tc := range []struct {
		name    string
		outcome sessionsOutcome
		why     string
		failed  bool
		refused []string
		want    sessionsOutcome
		says    string
	}{
		{"a pass", sessionsPassed, "", false, nil, sessionsPassed, ""},
		{"a failure at the assertion", sessionsFailed, "no message", false, nil, sessionsFailed, "no message"},
		{"a stop before the assertion", sessionsSetup, "no version 2", false, nil, sessionsSetup, "no version 2"},
		{"a step the harness stopped", sessionsHarness, stopped, true, nil, sessionsHarness, stopped},
		{"a pass the harness failed after", sessionsPassed, "", true, nil, sessionsHarness,
			"the harness failed after the step ended PASS; the test log above names the failure"},
		{"a failure the harness failed after", sessionsFailed, "no message", true, nil, sessionsHarness,
			"the harness failed after the step ended FAIL (no message); the test log above names the failure"},
		{"a pass whose reviewer could not read the daemon", sessionsPassed, "", true, refused, sessionsHarness,
			"so these sessions ran with a `legion` tool that could not read the daemon: " + refused[0]},
		{"a failure whose reviewer could not read the daemon", sessionsFailed, "no message", true, refused, sessionsHarness,
			"so these sessions ran with a `legion` tool that could not read the daemon: " + refused[0]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := sessionsEnding(tc.outcome, tc.why, tc.failed, tc.refused)
			if got != tc.want || !strings.HasSuffix(why, tc.says) {
				t.Fatalf("the line of %s ends %s: %q; want %s ending %q", tc.name, got, why, tc.want, tc.says)
			}
		})
	}
}

// sessionsCleanupChildEnv tells the re-executed test binary to run the run below, which is skipped
// in an ordinary run. That run has to fail, and a test that fails fails its parent, so it runs in
// a binary of its own.
const sessionsCleanupChildEnv = "LEGION_TEST_SESSIONS_CLEANUP_CHILD"

// sessionsCleanupChildRun is the name of the child's run, as its line prints it.
const sessionsCleanupChildRun = "a4-the-architect-tells-the-child-reviewer base 1"

// TestApprovedVersionNewsRunWhoseCleanupFails is the child of the test below: a run whose step
// ends FAIL at its assertion, and whose later cleanup, which stands for the daemon's stop, then
// fails the test as daemon.stop does. It runs only when the parent re-executes this binary.
func TestApprovedVersionNewsRunWhoseCleanupFails(t *testing.T) {
	if os.Getenv(sessionsCleanupChildEnv) != "1" {
		t.Skip(sessionsCleanupChildEnv + " is not set: this test is the child of TestApprovedVersionNewsARunLinePrintsHarnessForAFaultInItsCleanup")
	}
	line := newSessionsRunLine()
	line.printAtEnd(t, sessionsCleanupChildRun, func() int64 { return 0 })
	t.Cleanup(func() { t.Fatal("run: the daemon did not stop cleanly") })
	line.outcome, line.why = sessionsFailed, "no message"
}

// A fault in a cleanup of a run makes its line HARNESS, whatever its step answered. The line is
// the run's first cleanup, so it prints after the cleanups the run registers later: the daemon's
// stop, which fails the test when the daemon does not stop cleanly, and the closes. A line
// printed before them would print the step's word, and the script would count a base FAIL as the
// control biting. The line says the test log above names the failure, so the fault prints above
// it. The child runs with no database, since it reads none.
func TestApprovedVersionNewsARunLinePrintsHarnessForAFaultInItsCleanup(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run", "^TestApprovedVersionNewsRunWhoseCleanupFails$", "-test.v")
	child.Env = append(os.Environ(), sessionsCleanupChildEnv+"=1", "LEGION_TEST_PG_DSN=")
	out, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s", out)
	}
	const fault = "run: the daemon did not stop cleanly"
	want := "run " + sessionsCleanupChildRun + ": HARNESS: the harness failed after the step ended FAIL (no message); " +
		"the test log above names the failure in "
	at := strings.Index(string(out), want)
	if at < 0 {
		t.Fatalf("the child's output holds no line %q:\n%s", want, out)
	}
	if above := strings.Index(string(out), fault); above < 0 || above > at {
		t.Fatalf("the child's output does not hold the cleanup's fault %q above its line:\n%s", fault, out)
	}
}
