package docs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An option sent back as its stored line, inline code included, is that option unchanged, so the
// edit that adds another option beside it succeeds and keeps the line's code; and a question an
// ask edit writes keeps its inline code as code (INFRA-4345 R4, from dispatch://INFRA-2374: the
// option was refused as a rewrite of a line holding markup, and the question stored each backtick
// escaped).
func TestAnAskEditKeepsInlineCodeInItsOptionsAndItsQuestion(t *testing.T) {
	t.Parallel()
	const switchLine = "Switch only: `coqc` and `rocqchk` take a switch"
	seed := ":::ask{#ask-lists urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich build?\n\n- " + switchLine + "\n- Hold: Wait\n:::\n"

	t.Run("an option sent as its stored line, beside a new one", func(t *testing.T) {
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		seedServiceText(t, service, artifactID, seed)
		settleCurrentGeneration(t, service, artifactID)
		options := []model.AskOption{
			{Label: "Switch only", Description: "`coqc` and `rocqchk` take a switch"},
			{Label: "Hold", Description: "Wait"},
			{Label: "Both", Description: "Do both"},
		}
		if err := sendAskOptions(service, artifactID, AskBlockEdit{Options: &options}); err != nil {
			t.Fatalf("the edit adding an option beside one sent as its stored line = %v, want it applied", err)
		}
		markdown := liveMarkdownOf(t, service, artifactID)
		if !strings.Contains(markdown, "- "+switchLine+"\n") || !strings.Contains(markdown, "- Both: Do both\n") {
			t.Fatalf("the ask reads\n%s\nwant the stored line with its code and the new option", markdown)
		}
		settleCurrentGeneration(t, service, artifactID)
		_, _, held := blockAskRow(t, service, artifactID, "ask-lists")
		if !slices.Equal(held, []model.AskOption{
			{Label: "Switch only", Description: "coqc and rocqchk take a switch"},
			{Label: "Hold", Description: "Wait"},
			{Label: "Both", Description: "Do both"},
		}) {
			t.Fatalf("the ask row lists %#v, want the three options", held)
		}
	})

	t.Run("a question holding inline code", func(t *testing.T) {
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		seedServiceText(t, service, artifactID, seed)
		settleCurrentGeneration(t, service, artifactID)
		question := "Which build runs `coqc` first?"
		if err := sendAskOptions(service, artifactID, AskBlockEdit{Question: &question}); err != nil {
			t.Fatal(err)
		}
		markdown := liveMarkdownOf(t, service, artifactID)
		if !strings.Contains(markdown, "\nWhich build runs `coqc` first?\n") || strings.Contains(markdown, "\\`") {
			t.Fatalf("the ask reads\n%s\nwant its question's code as code, with no escaped backtick", markdown)
		}
	})

	t.Run("an option a doc-edit inserts into the open ask", func(t *testing.T) {
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		seedServiceText(t, service, artifactID, seed)
		settleCurrentGeneration(t, service, artifactID)
		alice := model.Actor{Kind: "user", ID: "alice"}
		if err := joinedWrite(service, func(ctx context.Context) error {
			_, err := service.ApplyOps(ctx, artifactID, []model.EditOp{{Op: "insert", After: "Hold: Wait", Markdown: "- Both: Do both"}}, alice, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		settleCurrentGeneration(t, service, artifactID)
		state, _, held := blockAskRow(t, service, artifactID, "ask-lists")
		if state != "open" || !slices.Equal(held, []model.AskOption{
			{Label: "Switch only", Description: "coqc and rocqchk take a switch"},
			{Label: "Hold", Description: "Wait"},
			{Label: "Both", Description: "Do both"},
		}) {
			t.Fatalf("the ask row is %q listing %#v, want it open listing the three options", state, held)
		}
	})

	t.Run("a doc-edit moving an option out of the open ask", func(t *testing.T) {
		tree, err := parseInput("Intro.\n\n" + seed)
		if err != nil {
			t.Fatal(err)
		}
		_, err = applyAlone(tree, model.EditOp{Op: "move", Block: blockIDOfText(t, tree, "Hold: Wait"), Before: "Intro."})
		var invalid *ErrInvalidOp
		if !errors.As(err, &invalid) || !strings.Contains(err.Error(), `open ask "ask-lists"`) {
			t.Fatalf("the move answered %v, want a refusal naming the ask", err)
		}
	})
}
