package textcheck

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The claim list under "The Dispatch identity the broker can issue" in docs/kubernetes.md is what
// a resource server writes its authorization against, and internal/broker/oauth holds the claims
// an exchanged token carries to it. That comparison is in two halves, because a test that reads
// the guide puts the guide in its job's filter — a declared read applies to every compiled source
// file of a run — and a documentation-only edit would then start the whole envoy Go lane. The
// product half compares the exchange with internal/broker/oauth/claims_doc_extract.txt, a copy of
// the list committed beside it; this half holds that copy to the guide's own bullets. Neither
// half alone is the guarantee: together they are the one the single test used to give.
//
// A claim is the backticked name a bullet opens with, followed by the em dash the list's shape
// puts after it, and a backticked word later in a bullet's prose is not a claim. The
// `project_scope` bullet is why: it reads "`named` or `person`, which says what `project` means",
// so a check that asked whether the section mentions a name anywhere between its bounds would
// find `project` there and pass with the `project` bullet deleted — the miss wearing a pass.
const (
	// claimsDocHeading is the section's heading, matched as a whole line: a heading is a line of
	// its own, so the list is reached without parsing the hard-wrapped prose above it.
	claimsDocHeading = "#### The Dispatch identity the broker can issue"
	// claimsDocSeparator follows the name a bullet opens with. See the comment above: it is the
	// shape that distinguishes the claim a bullet documents from the code spans its prose uses.
	claimsDocSeparator = " — "
	// claimsExtract is the committed copy, read from the package it is about. The guide is at the
	// repository root, four directories above this one.
	claimsExtract   = "../broker/oauth/claims_doc_extract.txt"
	kubernetesGuide = "../../../../docs/kubernetes.md"
)

// The committed copy of the claim list is the guide's list, name for name and in its order. An
// edit to either side alone fails here: a claim added to the guide and not to the copy would
// otherwise never reach the exchange's own check, which reads the copy and not the guide.
func TestTheClaimExtractIsTheGuidesClaimList(t *testing.T) {
	doc, err := os.ReadFile(filepath.FromSlash(kubernetesGuide))
	if err != nil {
		// A skip here would be an absence that reads exactly like a pass, and a silent absence is
		// what this check exists to stop.
		t.Fatalf("read docs/kubernetes.md: %v", err)
	}
	documented, err := claimListNames(string(doc))
	if err != nil {
		t.Fatalf("docs/kubernetes.md %v", err)
	}
	extracted := extractLines(t, claimsExtract)
	if !slices.Equal(extracted, documented) {
		t.Fatalf("%s and docs/kubernetes.md's claim list disagree:\n"+
			"  the extract: %s\n"+
			"  the guide:   %s\n"+
			"the extract is the copy internal/broker/oauth checks the exchange against, so the "+
			"guide answers for the code only while the two are equal; change them in one commit",
			claimsExtract, strings.Join(extracted, ", "), strings.Join(documented, ", "))
	}
}

// extractLines are the content lines of a committed extract: every line that is neither blank nor
// part of the `#` header saying what the file is, trimmed. The extracts are written to be read
// this way, so that the product test beside each of them parses it in a few lines and duplicates
// nothing of the markdown parsers here.
func extractLines(t *testing.T, path string) []string {
	t.Helper()
	text, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var lines []string
	for _, line := range strings.Split(string(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	// An empty extract is this check failing open: the file is still there, the parse matched
	// nothing, and the guide would then be held to a list nobody is reading.
	if len(lines) == 0 {
		t.Fatalf("%s holds nothing but its header: the copy it is supposed to carry is gone", path)
	}
	return lines
}

// claimListNames reads the claim names from the first bullet list under claimsDocHeading in doc, in
// the list's order. The list runs through the blank lines between its items and the indented lines
// of a wrapped item, and ends at the prose under it. A name the list carries twice is returned
// twice, so the caller's slices.Equal fails on it rather than a set hiding it.
func claimListNames(doc string) ([]string, error) {
	lines := strings.Split(doc, "\n")
	heading := slices.Index(lines, claimsDocHeading)
	if heading < 0 {
		return nil, fmt.Errorf("has no line %q: the section holding the claim list was renamed, "+
			"and this check stops checking the moment it cannot find it", claimsDocHeading)
	}

	var claims []string
	// The list is the section's first one: the prose lines between the heading and it are skipped.
	for i := heading + 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " ")
		if len(claims) > 0 && (line == "" || strings.HasPrefix(line, " ")) {
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			if len(claims) > 0 {
				break
			}
			if strings.HasPrefix(line, "#") {
				return nil, fmt.Errorf("reaches %q under %q with no bullet list between them: "+
					"the claims are no longer a list, and nothing is being held to them", line, claimsDocHeading)
			}
			continue
		}
		bullet := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		inner, opened := strings.CutPrefix(bullet, "`")
		name, rest, closed := strings.Cut(inner, "`")
		if !opened || !closed || !strings.HasPrefix(rest, claimsDocSeparator) {
			return nil, fmt.Errorf("line %d of the claim list is %q, which does not open with a "+
				"backticked claim name followed by %q: the list's shape is what tells this check "+
				"which of a bullet's code spans is the claim it documents",
				i+1, line, strings.TrimSpace(claimsDocSeparator))
		}
		claims = append(claims, name)
	}

	// An empty list is this check failing open: the heading is still there, the parse matched
	// nothing, and the extract would then be held to a list nobody is reading.
	if len(claims) == 0 {
		return nil, fmt.Errorf("names no claim under %q: the bullet list the section promises "+
			"is gone or no longer a list, and nothing is being held to it", claimsDocHeading)
	}
	return claims, nil
}

// The claim list is read whether its items follow one another directly or stand apart with a blank
// line between them, as the guide's one-sentence-per-line layout writes every list; an item wrapped
// onto an indented line is still one item; and the list ends at the prose under it, so a later
// list in the same section is never read as the claims.
func TestClaimListNamesReadsTightAndBlankSeparatedLists(t *testing.T) {
	const intro = claimsDocHeading + "\n\nAn issued token carries these claims and no others:\n\n"
	const after = "\nThe subject token's own kind decides those claims.\n\n- `other` — a later list, not a claim.\n"
	want := []string{"session", "person", "project_scope", "project"}
	for _, shape := range []struct{ name, list string }{{
		name: "tight",
		list: "- `session` — the session.\n" +
			"- `person` — the person.\n" +
			"- `project_scope` — `named` or `person`, which says what `project` means.\n" +
			"- `project` — the one project key.\n" +
			"  A `person` token carries none.\n",
	}, {
		name: "blank-separated",
		list: "- `session` — the session.\n\n" +
			"- `person` — the person.\n\n" +
			"- `project_scope` — `named` or `person`, which says what `project` means.\n\n" +
			"- `project` — the one project key.\n" +
			"  A `person` token carries none.\n",
	}} {
		t.Run(shape.name, func(t *testing.T) {
			got, err := claimListNames(intro + shape.list + after)
			if err != nil {
				t.Fatalf("claimListNames: %v", err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("claimListNames read %v, want %v", got, want)
			}
		})
	}
}
