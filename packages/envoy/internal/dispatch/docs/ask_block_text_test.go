package docs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// flipDecision is a decision block whose question holds a link and a picture, as a Legion
// finding's does.
const flipDecision = ":::ask{#flip urgency=\"med\" multiple=\"false\" state=\"open\"}\n" +
	"Should the audit run first? See [the run](https://ci.test/runs/7) ![Flip rate](https://ci.test/flip.png)\n\n" +
	"- Ship: Release it\n- Hold: Wait for review\n:::\n"

// The write guard over two questions in markdown: a rewrite keeping every picture and link
// passes, and one that writes a picture as a bare address, or a link as plain text, is refused
// with the address it drops.
func TestRefuseDroppedInlinesNamesWhatARewriteDrops(t *testing.T) {
	const question = "Should the audit run first? See [the run](https://ci.test/runs/7) ![Flip rate](https://ci.test/flip.png)"
	for rewrite, want := range map[string]string{
		"Run the audit first? See [the run](https://ci.test/runs/7) ![Flip rate](https://ci.test/flip.png)": "",
		"Run the audit first? See [the run](https://ci.test/runs/7) https://ci.test/flip.png":               `it drops the picture "https://ci.test/flip.png"`,
		"Run the audit first? See the run ![Flip rate](https://ci.test/flip.png)":                           `it drops the link "https://ci.test/runs/7"`,
	} {
		err := RefuseDroppedInlines(question, rewrite)
		var unrepresentable *ErrAskBlockUnrepresentable
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: err = %v, want none", rewrite, err)
		case want != "" && (!errors.As(err, &unrepresentable) || unrepresentable.Reason != want):
			t.Errorf("%q: err = %v, want the refusal %q", rewrite, err, want)
		}
	}
}

// A rewrite's question is markdown (AskBlockEdit.QuestionMarkdown): it is read as the document
// holds it, written back as the document's own markdown, so the block keeps its picture and its
// link, and the row takes the plain text. A rewrite that turns the picture into a bare address
// writes nothing, and the refusal names the picture.
func TestSetAskBlockTextWritesAMarkdownQuestionWithItsPicture(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, flipDecision)
	settleCurrentGeneration(t, service, artifactID)

	question, err := AskBlockQuestionMarkdown(liveMarkdownOf(t, service, artifactID), "flip")
	if err != nil {
		t.Fatal(err)
	}
	if want := "Should the audit run first? See [the run](https://ci.test/runs/7) ![Flip rate](https://ci.test/flip.png)"; question != want {
		t.Fatalf("the block's question is %q, want %q", question, want)
	}

	rewritten := strings.Replace(question, "Should the audit run first?", "Run the audit first?", 1)
	var stored AskBlockText
	if err := joinedWrite(service, func(ctx context.Context) error {
		var err error
		stored, err = service.SetAskBlockText(ctx, artifactID, "flip",
			AskBlockEdit{Question: &rewritten, QuestionMarkdown: true}, model.Actor{Kind: "system", ID: "dispatch-reviewer"})
		return err
	}); err != nil {
		t.Fatalf("the rewrite's write: %v", err)
	}
	markdown := liveMarkdownOf(t, service, artifactID)
	for _, kept := range []string{"Run the audit first?", "[the run](https://ci.test/runs/7)", "![Flip rate](https://ci.test/flip.png)"} {
		if !strings.Contains(markdown, kept) {
			t.Fatalf("the document after the rewrite holds no %q:\n%s", kept, markdown)
		}
	}
	if stored.Question != "Run the audit first? See the run" {
		t.Fatalf("the row takes %q, want the plain text the block reads back as", stored.Question)
	}

	before := liveTree(t, service, artifactID)
	bare := strings.Replace(rewritten, "![Flip rate](https://ci.test/flip.png)", "https://ci.test/flip.png", 1)
	err = sendAskBlockEdit(service, artifactID, "flip", AskBlockEdit{Question: &bare, QuestionMarkdown: true})
	var unrepresentable *ErrAskBlockUnrepresentable
	if !errors.As(err, &unrepresentable) || unrepresentable.Reason != `it drops the picture "https://ci.test/flip.png"` {
		t.Fatalf("a rewrite dropping the picture: err = %v, want the refusal naming the picture", err)
	}
	if !liveTree(t, service, artifactID).EqualWithBlockIDs(before) {
		t.Fatal("a refused rewrite changed the document")
	}
}

// Every other edit's question is plain text, as it always was: the same markdown sent without
// QuestionMarkdown is written as the words it spells, and the block holds no picture.
func TestSetAskBlockTextWritesAPlainQuestionAsText(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, flipDecision)
	settleCurrentGeneration(t, service, artifactID)

	question := "Run the audit first? ![Flip rate](https://ci.test/flip.png)"
	var stored AskBlockText
	if err := joinedWrite(service, func(ctx context.Context) error {
		var err error
		stored, err = service.SetAskBlockText(ctx, artifactID, "flip", AskBlockEdit{Question: &question}, model.Actor{Kind: "user", ID: "alice"})
		return err
	}); err != nil {
		t.Fatalf("the plain edit: %v", err)
	}
	if stored.Question != question {
		t.Fatalf("the row takes %q, want the text as sent", stored.Question)
	}
	if markdown := liveMarkdownOf(t, service, artifactID); strings.Contains(markdown, "![Flip rate](") {
		t.Fatalf("a plain edit wrote a picture:\n%s", markdown)
	}
}
