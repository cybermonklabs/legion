package inboxcompare

import (
	"bytes"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Each old row stands for the node the plan names: an Inbox ask row for its ask, a Reply row for
// its thread's root, a node of the thread's own kind, an Agent row for its root message, and in a
// person's own conversation the root they wrote and each reply its session wrote, each for its
// own message. A root somebody else wrote, one on an issue and a reply by anybody but the session
// are no rows of the person's conversations, and a Reply row of a kind no thread has is refused
// rather than mapped to a node it does not name.
func TestEachOldRowStandsForItsNode(t *testing.T) {
	asks := inboxRows([]inboxAsk{{ID: "a1"}})
	threads, err := repliesRows([]replyRow{
		{ThreadID: "t1", Kind: "ask"}, {ThreadID: "t2", Kind: "comment"}, {ThreadID: "t3", Kind: "message"},
		{ThreadID: "m1", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("map the Reply rows: %v", err)
	}
	issue, session := "CORE-1", "session:s1"
	person := func(id string) model.Actor { return model.Actor{Kind: "user", ID: id} }
	agent := model.Actor{Kind: "session", ID: "s1"}
	conversations := conversationRows("alice", "s1", []conversation{
		{
			Message: conversationMessage{ID: "m1", Target: &session, Author: person("Alice")},
			Replies: []conversationMessage{{ID: "m2", Author: agent}, {ID: "m3", Author: person("alice")}},
		},
		{Message: conversationMessage{ID: "m4", Target: &session, Author: person("bob")}, Replies: []conversationMessage{{ID: "m5", Author: agent}}},
		{Message: conversationMessage{ID: "m6", Target: &session, IssueKey: &issue, Author: person("alice")}},
	})
	var got []string
	for _, row := range slices.Concat(asks, threads, conversations) {
		got = append(got, strings.Join([]string{row.arm, row.kind, row.row, row.node, row.shape}, " "))
	}
	want := []string{
		"inbox assigned_ask ask:a1 ask:a1 inbox_ask",
		"replies thread_reply reply:t1 ask:t1 ask_thread",
		"replies thread_reply reply:t2 comment:t2 comment_thread",
		"replies thread_reply reply:t3 message:t3 message_thread",
		"replies direct_reply agent:m1 message:m1 agent_row",
		"conversations direct_reply message:m1 message:m1 ",
		"conversations direct_reply message:m2 message:m2 conversation_reply",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the old rows map to\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := repliesRows([]replyRow{{ThreadID: "t9", Kind: "document"}}); err == nil || !strings.Contains(err.Error(), `"document"`) {
		t.Errorf("a Reply row of kind document mapped with %v, want a refusal naming the kind", err)
	}
}

// The gate is met only by a run with no difference, nobody uncovered and a row of every shape, and
// a run missing a shape is refused as proof: its report prints no zero count as a pass and names
// the shape. A difference in one arm fails that person's arm and its kind whatever the other arms
// read.
func TestTheGateIsMetOnlyByAWholeCleanRun(t *testing.T) {
	whole := map[string]int{}
	for _, shape := range Shapes {
		whole[shape] = 1
	}
	without := func(shape string) map[string]int {
		shapes := map[string]int{}
		for name, count := range whole {
			if name != shape {
				shapes[name] = count
			}
		}
		return shapes
	}
	for _, c := range []struct {
		name string
		run  Run
		met  bool
	}{
		{"clean", Run{PeopleChecked: 2, Shapes: whole}, true},
		{"a difference", Run{DifferenceCount: 1, PeopleChecked: 2, Shapes: whole}, false},
		{"a person uncovered", Run{PeopleChecked: 2, PeopleUncovered: 1, Shapes: whole}, false},
		{"a shape missing", Run{PeopleChecked: 2, Shapes: without(ShapePersonWithNone)}, false},
	} {
		if got := c.run.Met(); got != c.met {
			t.Errorf("%s: Met() = %v, want %v", c.name, got, c.met)
		}
	}

	var report bytes.Buffer
	(&Comparison{
		People: []Person{{Login: "alice", Rows: map[string]int{ArmInbox: 1}, Entries: 3}},
		Run:    Run{PeopleChecked: 1, Shapes: without(ShapeReplyChain)},
	}).Write(&report)
	for _, want := range []string{
		"FAIL: shape reply_chain_at_bound: the corpus holds none of it",
		"FAIL: comparison refused: the corpus lacks reply_chain_at_bound",
		"FAIL: gate not met: the corpus lacks reply_chain_at_bound",
		"missing_shapes=reply_chain_at_bound gate=not-met",
	} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("the report of a run without a chain holds no %q:\n%s", want, report.String())
		}
	}
	if strings.Contains(report.String(), "PASS: differences") || strings.Contains(report.String(), "PASS: arm ") {
		t.Errorf("the report of a refused run prints a zero count as a pass:\n%s", report.String())
	}

	difference := Difference{Person: "alice", Arm: ArmReplies, Kind: KindThreadReply, Row: "reply:t1", Node: "comment:t1"}
	report.Reset()
	(&Comparison{
		People: []Person{{Login: "alice", Rows: map[string]int{ArmInbox: 1, ArmReplies: 1}, Entries: 9, Differences: []Difference{difference}}},
		Run:    Run{DifferenceCount: 1, PeopleChecked: 1, Shapes: whole, Differences: []Difference{difference}},
	}).Write(&report)
	for _, want := range []string{
		"FAIL: difference person=alice arm=replies kind=thread_reply row=reply:t1 node=comment:t1",
		"PASS: arm person=alice arm=inbox: 0 differences",
		"FAIL: arm person=alice arm=replies: 1 differences",
		"FAIL: kind thread_reply: 1 differences",
		"PASS: kind assigned_ask: 0 differences",
		"FAIL: differences: 1",
		"FAIL: gate not met: 1 differences",
	} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("the report of a run with one difference holds no %q:\n%s", want, report.String())
		}
	}
}

// ChainLinks is the outbox's own bound on the walk up a reply_to chain, copied rather than
// exported: the chain the corpus must hold is at the depth that walk stops at.
func TestTheChainIsAsLongAsTheOutboxWalk(t *testing.T) {
	source, err := os.ReadFile("../outbox/publisher.go")
	if err != nil {
		t.Fatalf("read outbox.maxCommentThreadDepth's declaration: %v", err)
	}
	declared := regexp.MustCompile(`(?m)^\s*maxCommentThreadDepth\s*=\s*(\d+)\s*$`).FindSubmatch(source)
	if declared == nil {
		t.Fatal("outbox/publisher.go declares no maxCommentThreadDepth = <number>, which ChainLinks copies")
	}
	if depth, _ := strconv.Atoi(string(declared[1])); depth != ChainLinks {
		t.Fatalf("outbox.maxCommentThreadDepth is %d and ChainLinks is %d: move ChainLinks with it", depth, ChainLinks)
	}
}
