package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// stubSource answers extraction's reads from maps, and counts them.
type stubSource struct {
	comments map[string]string
	messages map[string]string
	parents  map[string]string
	versions map[string]string // "<artifact>@<n>"
	results  map[string]checkResult
	reads    []string
}

func (s *stubSource) commentBody(_ context.Context, id string) (string, error) {
	s.reads = append(s.reads, "comment "+id)
	if body, ok := s.comments[id]; ok {
		return body, nil
	}
	return "", errNotFound
}

func (s *stubSource) messageBody(_ context.Context, id string, _ []string) (string, error) {
	s.reads = append(s.reads, "message "+id)
	if body, ok := s.messages[id]; ok {
		return body, nil
	}
	return "", errNotFound
}

func (s *stubSource) issueParent(_ context.Context, key string) (string, error) {
	s.reads = append(s.reads, "issue "+key)
	return s.parents[key], nil
}

func (s *stubSource) documentVersion(_ context.Context, id string, n int) (string, error) {
	key := fmt.Sprintf("%s@%d", id, n)
	s.reads = append(s.reads, "version "+key)
	if text, ok := s.versions[key]; ok {
		return text, nil
	}
	return "", errNotFound
}

func (s *stubSource) result(_ context.Context, _, id string) (checkResult, error) {
	s.reads = append(s.reads, "result "+id)
	if r, ok := s.results[id]; ok {
		return r, nil
	}
	return checkResult{}, errNotFound
}

const agent = `{"kind": "session", "id": "01a1d1e8-0000-7000-8000-00000000aaaa"}`

// ev is one log event of type typ by actor, on issue (none when ""), with payload.
func ev(id int64, typ, actor, issue, payload string) event {
	raw := fmt.Sprintf(`{"id": %d, "issue_key": %s, "artifact_id": null, "project": "PROJ", "type": %q,
		"actor": %s, "created_at": "2026-11-01T10:00:00Z", "payload": %s}`, id, jsonOrNull(issue), typ, actor, payload)
	var e event
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		panic(err)
	}
	return e
}

func jsonOrNull(s string) string {
	if s == "" {
		return "null"
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// got is a case's surface, field and text, the part most tests compare.
type got struct{ surface, field, text string }

func summarize(cases []Case) []got {
	out := make([]got, 0, len(cases))
	for _, c := range cases {
		out = append(out, got{c.Surface, c.Field, c.Text})
	}
	return out
}

func TestEachProseEventYieldsItsCases(t *testing.T) {
	t.Parallel()
	src := &stubSource{
		comments: map[string]string{"c-long": "The whole body of the long comment."},
		parents:  map[string]string{"PROJ-2": "PROJ-1"},
		versions: map[string]string{
			"doc-1@1": "# Heading kept\n\nThe first paragraph stays.\n\nA second paragraph goes.",
			"doc-1@2": "# Heading kept\n\nThe first paragraph stays.\n\nA new paragraph arrives.\n\n# Heading kept",
		},
		results: map[string]checkResult{"r-1": {Run: "Run the suite twice.", Observed: "Both runs passed.", NegativeControl: ""}},
	}
	x := newExtractor(src)
	for _, tc := range []struct {
		name  string
		event event
		want  []got
	}{
		{"comment", ev(1, "comment.created", agent, "PROJ-2", `{"id": "c-1", "body": "The lamp needs a new bulb.",
			"suggestion": {"replace_with": "Fit a brighter bulb.", "accepted": null}, "artifact_id": "doc-1", "artifact_name": "spec.md"}`),
			[]got{{surfaceComment, "body", "The lamp needs a new bulb."}, {surfaceCommentSuggestion, "suggestion.replace_with", "Fit a brighter bulb."}}},
		{"comment cut in the event", ev(2, "comment.created", agent, "PROJ-2", `{"id": "c-long", "body": "The whole", "body_truncated": true}`),
			[]got{{surfaceComment, "body", "The whole body of the long comment."}}},
		{"comment edit", ev(3, "comment.edited", agent, "PROJ-2", `{"id": "c-1", "body": "The lamp needs two bulbs."}`),
			[]got{{surfaceCommentEdit, "body", "The lamp needs two bulbs."}}},
		{"comment delivery reply", ev(4, "comment.answered", agent, "PROJ-2", `{"id": "c-2", "body": "I fitted the bulb."}`),
			[]got{{surfaceCommentDeliveryReply, "body", "I fitted the bulb."}}},
		{"issue message", ev(5, "message.created", agent, "PROJ-2", `{"id": "m-1", "body": "The kettle is on.", "target": null, "broadcast_id": null}`),
			[]got{{surfaceMessage, "body", "The kettle is on."}}},
		{"send to agent", ev(6, "message.created", agent, "", `{"id": "m-2", "body": "Please water the fern.", "target": "session:abc"}`),
			[]got{{surfaceSendToAgent, "body", "Please water the fern."}}},
		{"delivery reply", ev(7, "message.answered", agent, "", `{"id": "m-3", "body": "The fern is watered.", "reply_body": "Please water the fern.", "in_reply_to": "m-2"}`),
			[]got{{surfaceDeliveryReply, "body", "The fern is watered."}}},
		{"ask", ev(8, "ask.opened", agent, "PROJ-2", `{"id": "a-1", "kind": "question", "question": "Which shelf holds the atlas?",
			"options": [{"label": "The top shelf", "description": "Near the window."}, {"label": "The low shelf"}]}`),
			[]got{{surfaceAskQuestion, "question", "Which shelf holds the atlas?"},
				{surfaceAskOption, "options[0].label", "The top shelf"}, {surfaceAskOption, "options[0].description", "Near the window."},
				{surfaceAskOption, "options[1].label", "The low shelf"}}},
		{"ask edit keeps only what changed", ev(9, "ask.edited", agent, "PROJ-2", `{"id": "a-1", "kind": "question", "question": "Which shelf holds the atlas?",
			"options": [{"label": "The top shelf", "description": "Beside the door."}, {"label": "The low shelf"}],
			"previous": {"question": "Which shelf holds the atlas?", "options": [{"label": "The top shelf", "description": "Near the window."}, {"label": "The low shelf"}]}}`),
			[]got{{surfaceAskOption, "options[0].description", "Beside the door."}}},
		{"approval request", ev(10, "ask.opened", agent, "PROJ-2", `{"id": "a-2", "kind": "approval",
			"question": "Approve spec.md (version 4) and verification.md (version 2)? The garden plan now waters twice a day.",
			"options": [{"label": "Approve"}], "approval": {"artifact_id": "doc-1", "name": "spec.md"}}`),
			[]got{{surfaceApprovalSummary, "question", "The garden plan now waters twice a day."}}},
		{"approval request with no summary", ev(11, "ask.opened", agent, "PROJ-2", `{"id": "a-3", "kind": "approval",
			"question": "Approve spec.md (version 4)?", "options": [{"label": "Approve"}]}`), nil},
		{"answer for a person", ev(12, "ask.answered", `{"kind": "user", "id": "person@example.com", "act": {"session": "01a1d1e8-0000-7000-8000-00000000bbbb"}}`,
			"PROJ-2", `{"id": "a-1", "answer": {"text": "The top shelf holds it.", "selected": []}}`),
			[]got{{surfaceAskAnswer, "answer.text", "The top shelf holds it."}}},
		{"decline", ev(13, "ask.resolved", agent, "PROJ-2", `{"id": "a-1", "resolution": {"kind": "declined", "reason": "I cannot reach the shelf."}}`),
			[]got{{surfaceAskDecline, "resolution.reason", "I cannot reach the shelf."}}},
		{"retraction", ev(14, "ask.resolved", agent, "PROJ-2", `{"id": "a-1", "resolution": {"kind": "retracted", "reason": "The atlas was found."}}`),
			[]got{{surfaceAskResolution, "resolution.reason", "The atlas was found."}}},
		{"withdrawal", ev(15, "ask.resolved", agent, "PROJ-2", `{"id": "a-1", "resolution": {"kind": "withdrawn", "reason": "Closed with its issue."}}`), nil},
		{"issue created", ev(16, "issue.created", agent, "PROJ-3", `{"key": "PROJ-3", "title": "Paint the shed", "changed": ["title"], "ancestors": ["PROJ-1"]}`),
			[]got{{surfaceIssueTitle, "title", "Paint the shed"}}},
		{"issue retitled", ev(17, "issue.updated", agent, "PROJ-3", `{"key": "PROJ-3", "title": "Paint the shed green", "changed": ["title", "status"], "ancestors": ["PROJ-1"]}`),
			[]got{{surfaceIssueTitle, "title", "Paint the shed green"}}},
		{"issue status move", ev(18, "issue.updated", agent, "PROJ-3", `{"key": "PROJ-3", "title": "Paint the shed green", "changed": ["status"]}`), nil},
		{"document created", ev(19, "artifact.created", agent, "PROJ-2", `{"artifact": {"id": "doc-1", "kind": "doc", "name": "spec.md", "versions": [{"number": 1, "summary": null}]}}`),
			[]got{{surfaceDocument, "markdown", "# Heading kept\n\nThe first paragraph stays.\n\nA second paragraph goes."}}},
		{"document version keeps the new blocks", ev(20, "artifact.version", agent, "PROJ-2", `{"artifact_id": "doc-1", "name": "spec.md",
			"version": {"number": 2, "summary": "Swap the second paragraph."}}`),
			[]got{{surfaceDocument, "markdown", "A new paragraph arrives."}, {surfaceVersionSummary, "version.summary", "Swap the second paragraph."}}},
		{"file version", ev(21, "artifact.version", agent, "PROJ-2", `{"artifact_id": "file-1", "name": "chart.png", "version": {"number": 3, "mime": "image/png"}}`), nil},
		{"review", ev(22, "artifact.changes_requested", agent, "PROJ-2", `{"artifact_id": "doc-1", "name": "spec.md", "version": 2, "reason": "Name the paint colour."}`),
			[]got{{surfaceReviewReason, "reason", "Name the paint colour."}}},
		{"approval with no reason", ev(23, "artifact.approved", agent, "PROJ-2", `{"artifact_id": "doc-1", "name": "spec.md", "version": 2, "reason": null}`), nil},
		{"result", ev(24, "result.recorded", agent, "PROJ-2", `{"result_id": "r-1", "check_issue": "PROJ-1"}`),
			[]got{{surfaceResultRun, "run", "Run the suite twice."}, {surfaceResultObserved, "observed", "Both runs passed."}}},
		{"handoff", ev(25, "handoff.recorded", agent, "PROJ-2", `{"phase": "implement", "handoff_id": "h-1"}`), nil},
	} {
		// The events share one extractor, in order, as one pass over the log does: the
		// document version reads the version the create before it read.
		cases, exemption, err := x.cases(context.Background(), tc.event)
		if err != nil || exemption != "" {
			t.Fatalf("%s: err %v, exemption %q", tc.name, err, exemption)
		}
		if g := summarize(cases); !reflect.DeepEqual(g, tc.want) && !(len(g) == 0 && len(tc.want) == 0) {
			t.Errorf("%s: cases %q, want %q", tc.name, g, tc.want)
		}
	}
}

func TestACaseCarriesWhereItWasWritten(t *testing.T) {
	t.Parallel()
	src := &stubSource{parents: map[string]string{"PROJ-9": "PROJ-5", "PROJ-5": ""}}
	x := newExtractor(src)
	actor := `{"kind": "user", "id": "person@example.com", "act": {"session": "01a1d1e8-0000-7000-8000-00000000cccc"}}`
	cases, _, err := x.cases(context.Background(), ev(40, "comment.created", actor, "PROJ-9",
		`{"id": "c-9", "body": "The gate is painted.", "artifact_id": "doc-7", "artifact_name": "plan.md"}`))
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases %+v, err %v, want one", cases, err)
	}
	digest := sha256.Sum256([]byte("The gate is painted."))
	want := Case{
		EventID: 40, EventType: "comment.created", Time: "2026-11-01T10:00:00Z", Project: "PROJ",
		Issue: "PROJ-9", RootIssue: "PROJ-5", Document: "doc-7", DocumentName: "plan.md", SourceID: "c-9",
		Surface: surfaceComment, Field: "body", ActorKind: "user", ActorID: "person@example.com",
		ActSession: "01a1d1e8-0000-7000-8000-00000000cccc", Text: "The gate is painted.",
		SHA256: hex.EncodeToString(digest[:]),
	}
	if cases[0] != want {
		t.Errorf("case %+v, want %+v", cases[0], want)
	}
	// The root is read once and kept.
	if _, _, err := x.cases(context.Background(), ev(41, "comment.created", actor, "PROJ-9", `{"id": "c-10", "body": "The gate is dry."}`)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(src.reads, ",") != "issue PROJ-9,issue PROJ-5" {
		t.Errorf("reads %v, want each issue read once", src.reads)
	}
}

func TestTheActorRuleExemptsWritesBeforeAnyRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, actor, want string
	}{
		{"daemon", `{"kind": "session", "id": "legion-daemon:PROJ"}`, "exempt_daemon"},
		{"person", `{"kind": "user", "id": "person@example.com"}`, "exempt_person"},
		{"system", `{"kind": "system", "id": "document-settlement"}`, "exempt_system"},
	} {
		src := &stubSource{}
		x := newExtractor(src)
		cases, exemption, err := x.cases(context.Background(), ev(50, "comment.created", tc.actor, "PROJ-2",
			`{"id": "c-long", "body": "Cut short", "body_truncated": true}`))
		if err != nil || len(cases) != 0 || exemption != tc.want {
			t.Errorf("%s: cases %d, exemption %q, err %v, want none and %q", tc.name, len(cases), exemption, err, tc.want)
		}
		if len(src.reads) != 0 {
			t.Errorf("%s: reads %v, want none for an exempt write", tc.name, src.reads)
		}
	}
	// An event with no prose is no write the rule exempts.
	x := newExtractor(&stubSource{})
	if _, exemption, _ := x.cases(context.Background(), ev(51, "issue.claimed", `{"kind": "user", "id": "p@example.com"}`, "PROJ-2", `{}`)); exemption != "" {
		t.Errorf("a claim is exempted as %q, want no exemption", exemption)
	}
}

func TestABroadcastIsOneCase(t *testing.T) {
	t.Parallel()
	x := newExtractor(&stubSource{})
	var all []Case
	for i, target := range []string{"session:one", "session:two", "session:three"} {
		cases, _, err := x.cases(context.Background(), ev(int64(60+i), "message.created", agent, "",
			fmt.Sprintf(`{"id": "m-%d", "body": "The office closes at noon.", "target": %q, "broadcast_id": "b-1"}`, i, target)))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, cases...)
	}
	if len(all) != 1 || all[0].Surface != surfaceBroadcast || all[0].SourceID != "b-1" {
		t.Errorf("cases %+v, want one broadcast case", all)
	}
}

// A text Dispatch no longer serves keeps what the event carried, marked cut and counted.
func TestAnUnreadableTextKeepsTheEventsPart(t *testing.T) {
	t.Parallel()
	x := newExtractor(&stubSource{})
	cases, _, err := x.cases(context.Background(), ev(70, "message.created", agent, "PROJ-2",
		`{"id": "m-gone", "body": "The first half", "body_truncated": true}`))
	if err != nil || len(cases) != 1 || !cases[0].Cut || cases[0].Text != "The first half" {
		t.Fatalf("cases %+v, err %v, want the cut text kept", cases, err)
	}
	if x.unread[surfaceMessage] != 1 {
		t.Errorf("unread %v, want the message counted", x.unread)
	}
}

func TestApprovalSummary(t *testing.T) {
	t.Parallel()
	for question, want := range map[string]string{
		"Approve spec.md (version 3)?":                                           "",
		"Approve spec.md (version 3)? Adds a rain gauge.":                        "Adds a rain gauge.",
		"Approve a.md (version 1), b.md (version 2) and c.md (version 9)? Done.": "Done.",
		"A question an agent wrote?":                                             "",
	} {
		if got := approvalSummary(question); got != want {
			t.Errorf("approvalSummary(%q) = %q, want %q", question, got, want)
		}
	}
}

// A version's new text is its new paragraphs, list items and table rows, a table's rows under
// its head; a fenced block is one block whatever blank lines it holds.
func TestADocumentVersionKeepsOnlyItsNewBlocks(t *testing.T) {
	t.Parallel()
	before := "# Garden\n\nThe beds face south.\n\n- Water the roses.\n- Weed the path.\n  The path is gravel.\n\n" +
		"| Plant | Month |\n| --- | --- |\n| Tulip | April |\n| Aster | September |\n\n```\nrake\n\nhoe\n```"
	after := "# Garden\n\nThe beds face south.\n\n- Water the roses.\n- Weed the path.\n  The path is sand.\n- Mow the lawn.\n\n" +
		"| Plant | Month |\n| --- | --- |\n| Tulip | April |\n| Aster | October |\n| Daisy | June |\n\n```\nrake\n\nhoe\n```"
	src := &stubSource{versions: map[string]string{"doc-2@6": before, "doc-2@7": after}}
	x := newExtractor(src)
	text, err := x.newBlocks(context.Background(), "doc-2", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := "- Weed the path.\n  The path is sand.\n\n- Mow the lawn.\n\n" +
		"| Plant | Month |\n| --- | --- |\n| Aster | October |\n| Daisy | June |"
	if text != want {
		t.Errorf("new text\n%s\nwant\n%s", text, want)
	}
	// The next version is read against this one, kept, with no second read of it.
	src.versions["doc-2@8"] = after + "\n\nThe shed is new."
	src.reads = nil
	if text, err = x.newBlocks(context.Background(), "doc-2", 8); err != nil || text != "The shed is new." {
		t.Errorf("new text %q, err %v, want the one new paragraph", text, err)
	}
	if strings.Join(src.reads, ",") != "version doc-2@8" {
		t.Errorf("reads %v, want the new version alone", src.reads)
	}
}

func TestResumeDropsTheLastEventAndACutLine(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/cases.jsonl"
	line := func(c Case) string { b, _ := json.Marshal(c); return string(b) + "\n" }
	content := line(Case{EventID: 10, Surface: surfaceBroadcast, SourceID: "b-1"}) +
		line(Case{EventID: 12, Surface: surfaceComment}) +
		line(Case{EventID: 14, Surface: surfaceBroadcast, SourceID: "b-2"}) +
		`{"event_id": 14, "surf`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	x := newExtractor(&stubSource{})
	f, after, err := resumeCases(path, 5, x)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if after != 13 {
		t.Errorf("resumes after %d, want 13, to read event 14 again", after)
	}
	cases, err := readCases(path)
	if err != nil || len(cases) != 2 || cases[1].EventID != 12 {
		t.Errorf("kept %+v, err %v, want the lines of events 10 and 12", cases, err)
	}
	if !x.broadcasts["b-1"] || x.broadcasts["b-2"] {
		t.Errorf("broadcasts %v, want b-1 taken and b-2, dropped with its event, not", x.broadcasts)
	}
}
