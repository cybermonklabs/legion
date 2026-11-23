package reviewer

import (
	"strings"
	"testing"
	"time"
)

// The frame a held text's writer gets holds the findings, the reviewer's rewrite, the time left
// and the two commands, and says what posts when the writer does neither (Decision 5).
func TestTheWriterFindingsFrameOfAHeldText(t *testing.T) {
	rewrite := keptRewrite()
	findings := []Finding{
		{RuleSet: RulePhrases, Quote: "This names the problem.", Why: "It states no fact.", Fix: "The test fails on the node's first call."},
		{RuleSet: RuleReaderWords, Quote: "store", Why: "The reader asked what a store is.", Fix: "the database"},
	}
	body, err := FindingsFrame{ID: "4c1f", Kind: KindAsk, Findings: findings, Rewrite: &rewrite, Held: true, TimeLeft: 270 * time.Second, PostsRewrite: true}.Body()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Dispatch holds your ask 4c1f. No person can see it until you act on it. A reviewer read it and found 2 problems for its reader.",
		"1. Rule set \"phrases\". The words: \"This names the problem.\"\n   Why: It states no fact.\n   Fix: The test fails on the node's first call.",
		"2. Rule set \"reader_words\". The words: \"store\"",
		"## The rewrite from the reviewer\n\nFinding from tester\nWe start the node.",
		"- Hold: Wait until the test passes",
		"You have 4 minutes 30 seconds.",
		"run `dispatch held-edit 4c1f`",
		"run `dispatch held-post 4c1f --reason '<reason>'`",
		"If you do neither in 4 minutes 30 seconds, Dispatch posts the rewrite from the reviewer, marked as rewritten.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the frame does not hold %q:\n%s", want, body)
		}
	}

	// A rewrite that is not applied does not post; the text posts as written.
	body, err = FindingsFrame{ID: "4c1f", Kind: KindAsk, Findings: findings[:1], Rewrite: &rewrite, Held: true, TimeLeft: time.Minute, PostsRewrite: true,
		NotApplied: &NotApplied{Rule: KeepFirstLine, Reason: `the rewrite changes the first line "Finding from tester", which has 12 words or fewer`}}.Body()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"found 1 problem for its reader.",
		"Dispatch cannot post this rewrite as it is: the rewrite changes the first line \"Finding from tester\", which has 12 words or fewer.",
		"You have 1 minute.",
		"If you do neither in 1 minute, Dispatch posts your text as you wrote it.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the frame does not hold %q:\n%s", want, body)
		}
	}
}

// A text reviewed after its post is not held: its frame names the command that changes it, and
// neither the time nor the held text's commands.
func TestTheWriterFindingsFrameOfAPostedText(t *testing.T) {
	findings := []Finding{{RuleSet: RuleNoslop, Quote: "plays a vital role", Why: "It is puffery.", Fix: "Delete it."}}
	body, err := FindingsFrame{ID: "spec", Kind: KindDocument, Findings: findings}.Body()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Dispatch posted your document spec. A reviewer read it after the post") || !strings.Contains(body, "If a finding is correct, change the text: run `dispatch doc-edit`.") {
		t.Errorf("the frame of a posted document:\n%s", body)
	}
	for _, absent := range []string{"held-edit", "held-post", "You have", "rewrite from the reviewer"} {
		if strings.Contains(body, absent) {
			t.Errorf("the frame of a posted text holds %q:\n%s", absent, body)
		}
	}
	body, err = FindingsFrame{ID: "c1", Kind: KindComment, Findings: findings}.Body()
	if err != nil || !strings.Contains(body, "If a finding is correct, change the text.") {
		t.Errorf("the frame of a posted comment: %v\n%s", err, body)
	}
}

func TestSpokenDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "no time", 5 * time.Minute: "5 minutes", 61 * time.Second: "1 minute 1 second",
		time.Hour + 2*time.Second: "1 hour 2 seconds", 1500 * time.Millisecond: "2 seconds",
	} {
		if got := spokenDuration(d); got != want {
			t.Errorf("spokenDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

// The side questions name the text and what the reader needs.
func TestTheSideQuestions(t *testing.T) {
	question, err := RewriteQuestion("a-1", askRecord().Text)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"write your ask a-1 again, so that they can answer it without your conversation", "4. Your recommendation, with its reason.", "The ask:\n\nFinding from tester\n", "- Hold: Wait for the store"} {
		if !strings.Contains(question, want) {
			t.Errorf("the rewrite's question does not hold %q:\n%s", want, question)
		}
	}
	question, err = RewriteQuestion("c-1", Text{Kind: KindComment, Body: "This names the problem."})
	if err != nil || !strings.Contains(question, "so that they can understand it") || strings.Contains(question, "Your recommendation") {
		t.Errorf("the rewrite's question for a comment: %v\n%s", err, question)
	}
	question, err = DefineQuestion("b-1", KindDecisionBlock, []string{"store", "the node"})
	if err != nil || !strings.Contains(question, "your decision block b-1") || !strings.Contains(question, "- store\n- the node\n") {
		t.Errorf("the definition's question: %v\n%s", err, question)
	}
}
