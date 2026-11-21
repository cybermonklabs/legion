package textcheck

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The claim list under "The Dispatch identity the broker can issue" in docs/kubernetes.md is what
// a resource server writes its authorization against, and internal/cipher/oauth holds the claims
// an exchanged token carries to it. That comparison is in two halves, because a test that reads
// the guide puts the guide in its job's filter — a declared read applies to every compiled source
// file of a run — and a documentation-only edit would then start the whole envoy Go lane. The
// product half compares the exchange with internal/cipher/oauth/claims_doc_extract.txt, a copy of
// the list committed beside it; this half holds that copy to the guide's own bullets. Neither
// half alone is the guarantee: together they are the one the single test used to give.
//
// A claim is the backticked name a bullet opens with, followed by the em dash the list's shape
// puts after it, and a backticked word later in a bullet's prose is not a claim. The
// `project_scope` bullet is why: it names the words that claim takes and says what `projects`
// means, so a check that asked whether the section mentions a name anywhere between its bounds
// would find `projects` there and pass with the `projects` bullet deleted — the miss wearing a
// pass.
//
// The list is read in both readings of the guide (claimListOfThePage): as written, which an agent
// or an editor reads, and as the rendered page shows it (visibleMarkdown), which a person reads, so
// that a claim, the list or its heading that only an HTML comment holds fails rather than passing
// as the list.
const (
	// claimsDocHeading is the section's heading, matched as a whole line: a heading is a line of
	// its own, so the list is reached without parsing the hard-wrapped prose above it.
	claimsDocHeading = "#### The Dispatch identity the broker can issue"
	// claimsDocSeparator follows the name a bullet opens with. See the comment above: it is the
	// shape that distinguishes the claim a bullet documents from the code spans its prose uses.
	claimsDocSeparator = " — "
	// claimsExtract is the committed copy, read from the package it is about. The guide is at the
	// repository root, four directories above this one.
	claimsExtract   = "../cipher/oauth/claims_doc_extract.txt"
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
	documented, err := claimListOfThePage(string(doc))
	if err != nil {
		t.Fatalf("docs/kubernetes.md %v", err)
	}
	extracted := extractLines(t, claimsExtract)
	if !slices.Equal(extracted, documented) {
		t.Fatalf("%s and docs/kubernetes.md's claim list disagree:\n"+
			"  the extract: %s\n"+
			"  the guide:   %s\n"+
			"the extract is the copy internal/cipher/oauth checks the exchange against, so the "+
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

// errCommented is why claimListOfThePage refuses a guide: an HTML comment holds a claim, the list
// or its heading, or stands inside the list, so the guide as written and as its page renders read
// the list differently.
var errCommented = errors.New("the text as written and as the rendered page shows it read the claim " +
	"list differently, so an HTML comment decides it: take the comment out of the list and its heading")

// claimListOfThePage is the claim list of doc, a Markdown page, read in both its readings
// (claimListNames of the text as written and of visibleMarkdown's): a list the two read
// differently, or that one of them cannot read, is refused for the comment (errCommented), naming
// what each reading gives. Where both fail alike, the written one answers, so a page whose
// comments stand outside the list fails with the written reading's error alone.
func claimListOfThePage(doc string) ([]string, error) {
	written, writtenErr := claimListNames(doc)
	shown, shownErr := claimListNames(visibleMarkdown(doc))
	switch {
	case writtenErr != nil && shownErr != nil && writtenErr.Error() == shownErr.Error():
		return nil, writtenErr
	case writtenErr != nil || shownErr != nil || !slices.Equal(written, shown):
		return nil, fmt.Errorf("as written, HTML comments and all, %s; as the rendered page shows it, its "+
			"HTML comments hidden, %s: %w", claimReading(written, writtenErr), claimReading(shown, shownErr),
			errCommented)
	}
	return written, nil
}

// claimReading is what one reading of the claim list gives, for claimListOfThePage's refusal.
func claimReading(claims []string, err error) string {
	if err != nil {
		return err.Error()
	}
	return "names " + strings.Join(claims, ", ")
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

// A claim list a reader of the rendered page cannot see is none: the page shows nothing of a claim,
// the list or its heading inside an HTML comment, or of what follows a comment opened on a line of
// its own and never closed, and a comment that splits a claim's name shows it whole on the page
// alone. So the list is read in both readings of the guide, and each of those is refused for the
// comment. A comment inside a claim's prose, a "<!--" inside a fenced block above the list, a
// "<!--" inside a line that its paragraph never closes, which the page shows as text, and a comment
// after the list, where both readings have ended it, change nothing it reads; a guide with no list
// fails in both readings alike, with the written reading's refusal.
func TestTheClaimListIsReadWhereAReaderSeesIt(t *testing.T) {
	const (
		prose = "An issued token carries these claims and no others:\n\n"
		intro = claimsDocHeading + "\n\n" + prose
		list  = "- `session` — the session.\n\n- `person` — the person.\n"
		after = "\nThe subject token's own kind decides those claims.\n"
	)
	both := []string{"session", "person"}
	for _, guide := range []struct {
		shape, text string
		want        []string
		// refused is "" for a list both readings read alike, "comment" for errCommented, and
		// "written" for a refusal both readings give alike.
		refused string
	}{
		{"the list a reader sees", intro + list + after, both, ""},
		{"the list inside a comment", intro + "<!--\n" + list + "-->\n" + after, nil, "comment"},
		{"the first claim inside a comment",
			intro + "<!--\n- `session` — the session.\n-->\n- `person` — the person.\n" + after, nil, "comment"},
		{"the heading inside a comment", "<!--\n" + claimsDocHeading + "\n-->\n\n" + prose + list + after, nil, "comment"},
		{"a comment never closed before the list",
			claimsDocHeading + "\n\nAn issued token carries these claims:\n<!-- a note\n\n" + list + after, nil, "comment"},
		{"a \"<!--\" never closed inside a line before the list",
			claimsDocHeading + "\n\nAn issued token carries these claims <!-- a note\n\n" + list + after, both, ""},
		{"a comment splitting a claim's name",
			intro + "- `ses<!-- whole -->sion` — the session.\n\n- `person` — the person.\n" + after, nil, "comment"},
		{"a comment inside a claim's prose",
			intro + "- `session` — the <!-- whole --> session.\n\n- `person` — the person.\n" + after, both, ""},
		{"a comment opened inside a fenced block above the list",
			claimsDocHeading + "\n\n```html\n<!--\n```\n\n" + prose + list + after, both, ""},
		{"a comment after the list", intro + list + "\n<!--\n- `other` — a claim the copy lacks.\n-->\n" + after,
			both, ""},
		{"no list under the heading", intro + after, nil, "written"},
	} {
		got, err := claimListOfThePage(guide.text)
		switch guide.refused {
		case "":
			if err != nil || !slices.Equal(got, guide.want) {
				t.Errorf("%s: the list reads %v and the error %v, want %v and none: %q", guide.shape, got, err,
					guide.want, guide.text)
			}
		case "comment":
			if !errors.Is(err, errCommented) {
				t.Errorf("%s: the list reads %v and the error %v, want a refusal for the comment: %q", guide.shape,
					got, err, guide.text)
			}
		case "written":
			if err == nil || errors.Is(err, errCommented) {
				t.Errorf("%s: the list reads %v and the error %v, want the refusal both readings give: %q",
					guide.shape, got, err, guide.text)
			}
		}
	}
}
