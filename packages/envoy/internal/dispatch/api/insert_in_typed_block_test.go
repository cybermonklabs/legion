package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// typedInsertSpec is a spec marking one requirement, r1, beside an ask, a1, whose question comes
// before its two options, and a closing paragraph at the document's own level.
const typedInsertSpec = ":::requirement{#r1}\nThe read succeeds.\n:::\n\n" +
	":::ask{#a1 urgency=\"med\"}\nWhich transport?\n\n- NATS\n- HTTP\n:::\n\n" +
	"After.\n"

// paragraphIDOf is the id of the block GET …/blocks lists whose markdown is text alone: a
// paragraph, which a typed block holding it lists apart from itself.
func paragraphIDOf(t *testing.T, handler http.Handler, artifactID, text string) string {
	t.Helper()
	for markdown, id := range blockIDsByText(t, handler, artifactID) {
		if strings.TrimSpace(markdown) == text {
			return id
		}
	}
	t.Fatalf("no block of %s reads %q alone", artifactID, text)
	return ""
}

// insertAfter posts one insert of markdown after the block holding text alone, as the session.
func insertAfter(t *testing.T, handler http.Handler, artifactID, text, markdown string) (int, string, string) {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops": []map[string]any{{
			"op": "insert", "after": "block:" + paragraphIDOf(t, handler, artifactID, text), "markdown": markdown,
		}},
		"actor": sessionActor(),
	})
	body := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	return response.Code, body.Code, body.Error
}

// An insert anchored on a block inside a typed block lands next to that block, inside the typed
// block, where it was written at the document's own level after the typed block and refused
// nothing (dispatch://INFRA-3142): a requirement's words landed outside every requirement, which
// the `serves` of each check of it then did not reach. Each row reads the block back where its
// anchor put it, and for the requirement and the check the coverage reading still finds the
// requirement, holding the inserted words and served by its one check.
func TestAnInsertAnchoredInsideATypedBlockLandsInsideIt(t *testing.T) {
	for _, test := range []struct {
		name string
		// verification is true where the insert is into the Verification document's check rather
		// than into the spec.
		verification     bool
		anchor, markdown string
		// holds is the stretch of the document that holds the insert inside its typed block.
		holds string
		// requirement is what the coverage reading's r1 must hold, "" where the row reads none.
		requirement string
	}{
		{
			name:        "a requirement",
			anchor:      "The read succeeds.",
			markdown:    "It answers within a second.",
			holds:       ":::requirement{#r1}\nThe read succeeds.\n\nIt answers within a second.\n:::\n",
			requirement: "It answers within a second.",
		},
		{
			name:     "an ask",
			anchor:   "Which transport?",
			markdown: "The one the daemon already holds.",
			holds:    "Which transport?\n\nThe one the daemon already holds.\n\n- NATS\n- HTTP\n:::\n",
		},
		{
			name:         "a check",
			verification: true,
			anchor:       "The read returns 200.",
			markdown:     "And within a second.",
			holds:        "\nThe read returns 200.\n\nAnd within a second.\n:::\n",
			requirement:  "The read succeeds.",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			issue := createInteractionIssue(t, handler, "TYPED", "Insert into a typed block", typedInsertSpec)
			verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
				checkServing("k1", "dispatch://"+issue.Key+"/spec#b-r1")+"\n"+
					":::check{#k2 serves=\"dispatch://"+issue.Key+"/spec#b-r1\" surface=\"api\" run=\"go test ./x\"}\nThe read returns 200.\n:::\n")
			artifactID := issue.PrimaryArtifactID
			if test.verification {
				artifactID = verification.Artifact.ID
			}
			if status, code, refusal := insertAfter(t, handler, artifactID, test.anchor, test.markdown); status != http.StatusOK {
				t.Fatalf("insert after %q: status=%d code=%q error=%q, want 200", test.anchor, status, code, refusal)
			}
			if markdown := documentMarkdown(t, handler, artifactID); !strings.Contains(markdown, test.holds) {
				t.Fatalf("document after the insert = %q, want the typed block to hold it: %q", markdown, test.holds)
			}
			if test.requirement == "" {
				return
			}
			coverage := readCoverage(t, handler, issue.Key)
			r1 := requirementOf(t, coverage, "r1")
			if !strings.Contains(r1.Text, test.requirement) || r1.Checks != 2 || len(coverage.Gaps) != 0 {
				t.Fatalf("coverage after the insert: r1 = %+v, gaps %q; want r1 holding %q, served by both checks, and no gap", r1, coverage.Gaps, test.requirement)
			}
		})
	}
}

// An insert anchored inside a typed block whose content rule cannot hold its markdown there is
// refused 400 INSERT_DOES_NOT_FIT, naming the block, its rule and what breaks it, and the
// document keeps its text and its versions: a requirement holds paragraphs alone, and an ask
// holds no paragraph after its options, which an insert anchored on the options list's own block
// writes (one anchored on an option's text lands in that option's item, beside its text). Each
// was written after the typed block at the document's own level. Nothing settles the spec while
// the row runs, so its versions are the ones the row read.
func TestAnInsertATypedBlockCannotHoldIsRefused(t *testing.T) {
	for _, test := range []struct {
		name, anchor, markdown string
		wants                  []string
	}{
		{
			name:     "a heading in a requirement",
			anchor:   "The read succeeds.",
			markdown: "## Latency",
			wants:    []string{"operation 0", `requirement block "r1"`, "paragraph+", "a heading", `after: "block:r1"`},
		},
		{
			name:     "a paragraph after an ask's options",
			anchor:   "- NATS\n- HTTP",
			markdown: "Pick one by Friday.",
			wants:    []string{"operation 0", `ask block "a1"`, "paragraph+ bullet_list?", "a paragraph after its options", `after: "block:a1"`},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
			issue := createInteractionIssue(t, handler, "TYPED", "Refused insert", typedInsertSpec)
			before, versions := documentMarkdown(t, handler, issue.PrimaryArtifactID), len(documentVersions(t, handler, issue.PrimaryArtifactID))
			status, code, refusal := insertAfter(t, handler, issue.PrimaryArtifactID, test.anchor, test.markdown)
			if status != http.StatusBadRequest || code != "INSERT_DOES_NOT_FIT" {
				t.Fatalf("insert of %q after %q: status=%d code=%q error=%q, want 400 INSERT_DOES_NOT_FIT", test.markdown, test.anchor, status, code, refusal)
			}
			for _, want := range test.wants {
				if !strings.Contains(refusal, want) {
					t.Fatalf("refusal = %q, want it to say %q", refusal, want)
				}
			}
			if after := documentMarkdown(t, handler, issue.PrimaryArtifactID); after != before {
				t.Fatalf("document after the refused insert = %q, want %q unchanged", after, before)
			}
			if after := len(documentVersions(t, handler, issue.PrimaryArtifactID)); after != versions {
				t.Fatalf("versions after the refused insert = %d, want %d", after, versions)
			}
		})
	}
}

// An insert anchored on a block at the document's own level lands beside it there, as it always
// has: after a paragraph, and after a typed block named by its own id rather than by a block
// inside it.
func TestAnInsertAnchoredOnATopLevelBlockLandsBesideIt(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TYPED", "Top-level insert", typedInsertSpec)
	if status, code, refusal := insertAfter(t, handler, issue.PrimaryArtifactID, "After.", "Closing."); status != http.StatusOK {
		t.Fatalf("insert after a top-level paragraph: status=%d code=%q error=%q, want 200", status, code, refusal)
	}
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":   []map[string]any{{"op": "insert", "after": "block:r1", "markdown": "Beside the requirement."}},
		"actor": sessionActor(),
	})
	if response.Code != http.StatusOK {
		t.Fatalf("insert after the requirement's own block: status=%d body=%s", response.Code, response.Body.String())
	}
	markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	for _, want := range []string{
		":::requirement{#r1}\nThe read succeeds.\n:::\n\nBeside the requirement.\n\n:::ask",
		"\nAfter.\n\nClosing.\n",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("document after the top-level inserts = %q, want %q", markdown, want)
		}
	}
}
