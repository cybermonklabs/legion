package refs

//ci:reads packages/envoy/internal/dispatch/**/*.go !packages/envoy/internal/dispatch/**/*_test.go the heads test parses every non-test Go file of the Dispatch tree (..)

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// TestTheHeadsAReaderDrawsCutNoReferenceInHalf holds each head heads.go cuts, and each reader of
// one, to text.HeadRefs: a key the bound falls inside is left out whole rather than shown as
// `CORE-12`, a working link to another issue. The API's rows hold the subject, the candidate's
// first line and the excerpts an edge shows to the same rule end to end.
func TestTheHeadsAReaderDrawsCutNoReferenceInHalf(t *testing.T) {
	t.Parallel()
	core := text.Ref{Kind: "issue", IssueKey: "CORE-1", ID: "CORE-1"}
	t.Run("a first line", func(t *testing.T) {
		lead := strings.Repeat("x", 151)
		if got, want := FirstLine("\n  "+lead+" CORE-1234 decides it.\nMore.", ""), lead+"…"; got != want {
			t.Fatalf("FirstLine = %q, want %q", got, want)
		}
	})
	t.Run("a document mention's block excerpt", func(t *testing.T) {
		block := "CORE-1 " + strings.Repeat("y", 464)
		markdown := block + " CORE-1234 follows."
		excerpts := MentionExcerpts(markdown, []model.ArtifactBlock{{ID: "p1", From: 0, To: len(markdown)}},
			[]text.Located{{Ref: core, Offset: 0}}, "")
		if got := excerpts[Target{Kind: "issue", ID: "CORE-1"}]; got != (model.GraphExcerpt{BlockID: "p1", Text: block}) {
			t.Fatalf("the excerpt = %#v, want the block's %d runes before the key", got, len(block))
		}
	})
	t.Run("an edit that moves no head an edge shows", func(t *testing.T) {
		// 232 runes, so the key after them crosses the cut at 240 in both texts: the head an edge
		// shows is the lead in both, though the first 240 runes of the two differ.
		lead := strings.Repeat("x", 231) + " "
		if got := EditedTargets(lead+"CORE-1234 a", lead+"CORE-1294 b", "", []text.Ref{core}); got != nil {
			t.Fatalf("EditedTargets = %#v, want none: the head an edge shows did not move", got)
		}
		if got := EditedTargets("A first.", "A second.", "", []text.Ref{core}); len(got) != 1 {
			t.Fatalf("EditedTargets = %#v, want the cited target: the head moved", got)
		}
	})
}

// TestEveryHeadCutOfDispatchTextIsListed is the inventory of the cuts that never split a
// reference. A head of stored text a reader draws with its references is cut by heads.go
// (FirstLine, itemExcerpt, BlockExcerpt), which cut where no reference crosses the cut. Every
// other call of the plain cuts under internal/dispatch - text.HeadRunes, text.HeadWords and
// contracts.OneLineSummary - is listed here, by its file and function, with the reason no reader
// draws its head with references. A new call is refused until it is one or the other, so a head
// a reader draws is never cut by the plain rule unnoticed; a listed call that is gone is refused
// too, so the list stays the inventory it says it is. The read is of each file's syntax tree, so
// a cut named in a comment is not a call.
func TestEveryHeadCutOfDispatchTextIsListed(t *testing.T) {
	t.Parallel()
	exempt := map[string]string{
		"api/messages.go messageReplyPreview":     "the parent's text a reply's event and delivery frame carry, which an agent reads as written",
		"api/priorities.go priorityChangedQuote":  "the stored title or description a 409 PRIORITY_CHANGED refusal quotes in its error message, which a caller reads as its characters, and which the page names by its code alone (web/src/features/writes/conflicts.ts)",
		"api/results.go cut":                      "a check's line in the pull request body, which GitHub draws, not Dispatch",
		"api/results_ci.go ciObserved":            "a CI run's output, a check result's observed text drawn as the characters it is",
		"api/results_ci.go ciUnreadDetail":        "the server's own words on a CI read it could not make, quoting GitHub's error",
		"api/status_change.go claimedSessionName": "a session id a request's body named, which the comment Dispatch signs quotes as an inline code span, so every reader draws it as its characters and reads no reference in it",
		"embed/embed.go boundBatch":               "the input of an embedding, which no person reads",
		"itemblocks/windows.go windowText":        "the text of an embedding window, which no person reads",
		"outbox/publisher.go payloadSummary":      "an envelope's one-line summary, which an agent's notification carries",
	}
	plain := map[string]map[string]bool{
		"text":      {"HeadRunes": true, "HeadWords": true},
		"contracts": {"OneLineSummary": true},
	}
	found := map[string]bool{}
	fileSet := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		file, err := filepath.Rel("..", path)
		if err != nil {
			return err
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := selector.X.(*ast.Ident); !ok || !plain[pkg.Name][selector.Sel.Name] {
					return true
				}
				site := filepath.ToSlash(file) + " " + function.Name.Name
				found[site] = true
				if _, listed := exempt[site]; !listed {
					t.Errorf("%s (%s) cuts Dispatch text with %s.%s: cut a head a reader draws with "+
						"refs.FirstLine, itemExcerpt or BlockExcerpt, or list it here with the reason no reader "+
						"draws it with its references",
						site, fileSet.Position(call.Pos()), selector.X.(*ast.Ident).Name, selector.Sel.Name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/dispatch: %v", err)
	}
	for site := range exempt {
		if !found[site] {
			t.Errorf("%s is listed but makes no plain cut any more: take it off the list", site)
		}
	}
}
