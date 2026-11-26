package textcheck

// This test reads internal/config's key table as source text rather than importing the
// package, so the table is a read of its own beside the guide.
//ci:reads packages/daemon/internal/config/keytable.go `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestThePlanGuidanceRollbackSectionNamesTheKeyTheTableDeclares (planGuidanceKeyTable)
//ci:reads docs/kubernetes.md `packages/daemon/internal/textcheck/waiting_line_texts_test.go#shippedText` reads it under ../../../.. for TestThePlanGuidanceRollbackSectionNamesTheKeyTheTableDeclares (wayBackGuide)

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// A top-level key this release adds is refused by the loader of every earlier release, by name, so
// a deployment that sets it cannot boot the earlier image until the key is out of legion.yaml.
// docs/kubernetes.md's rollback section for the release is where an operator reads that, and this
// check holds the two together: the key as internal/config's table declares it, and the section
// that tells the operator to take it out. A key renamed in the table and not in the guide leaves
// the operator a step that does not match their file, and the section is where they look.
//
// The table is read as source text rather than imported, for the reason way_back_doc_test.go
// gives: importing a product package would put that package's files, and every file its own tests
// read, into this job's filter.
const (
	// planGuidanceKey is the key this release adds, as legion.yaml and the guide write it.
	planGuidanceKey = "plan_guidance"
	// planGuidanceKeyTable is internal/config's table of the top-level keys, a path from the
	// repository's root, four directories above this package (shippedText).
	planGuidanceKeyTable = "packages/daemon/internal/config/keytable.go"
	// planGuidanceRollbackSection is the heading of the guide's section that gives the way back.
	planGuidanceRollbackSection = "Rolling back the release that gives each owner the deployment's plan guidance"
)

// The guide's rollback section of this release names the key the daemon's table declares and the
// refusal an earlier loader answers with, so an operator takes the key out of legion.yaml before
// they pin the earlier image.
func TestThePlanGuidanceRollbackSectionNamesTheKeyTheTableDeclares(t *testing.T) {
	table := shippedText(t, planGuidanceKeyTable)
	declared := regexp.MustCompile(`Name:\s*"`+regexp.QuoteMeta(planGuidanceKey)+`"`).FindAllString(table, -1)
	if len(declared) != 1 {
		t.Errorf("%s declares %d rows named %q, want one: a key renamed here is renamed in %s's section %q too, "+
			"which tells an operator which key to take out of legion.yaml",
			planGuidanceKeyTable, len(declared), planGuidanceKey, wayBackGuide, planGuidanceRollbackSection)
	}

	guide := shippedText(t, wayBackGuide)
	if err := sectionHeadingIn(guide, planGuidanceRollbackSection); err != nil {
		t.Fatalf("%s: %v: the section is where an operator reads the step this release's key owes a rollback",
			wayBackGuide, err)
	}
	section, err := sectionTextIn(guide, planGuidanceRollbackSection)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"`" + planGuidanceKey + "` must come out of `legion.yaml` before the earlier image is pinned",
		"unknown key " + planGuidanceKey,
	} {
		if !strings.Contains(section, want) {
			t.Errorf("%s's section %q does not say %q: an operator who pins the earlier image with the key set "+
				"has a daemon that refuses to start", wayBackGuide, planGuidanceRollbackSection, want)
		}
	}
}

// sectionTextIn is the visible text of heading's section in text, a Markdown page: from the line
// after that heading to the next heading of its level or above, or the end of the page. A page
// with no such heading is an error rather than an empty string, which would read as a section that
// says nothing.
func sectionTextIn(text, heading string) (string, error) {
	lines := strings.Split(visibleMarkdown(text), "\n")
	section, level, end := -1, 0, len(lines)
	for i, line := range lines {
		opens := markdownHeading.FindStringSubmatch(line)
		switch {
		case opens == nil:
		case opens[2] == heading && section < 0:
			section, level = i+1, len(opens[1])
		case section >= 0 && end == len(lines) && len(opens[1]) <= level:
			end = i
		}
	}
	if section < 0 {
		return "", fmt.Errorf("the page carries no heading %q", heading)
	}
	return strings.Join(lines[section:end], "\n"), nil
}
