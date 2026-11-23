// Package plainwords is the text of the repository's skills/plain-words skill, the rules agents
// write by (INFRA-3231, R6): ce-noslop's tests, its pattern catalog and its terminology, and the
// five phrases it tells agents to stop. The server shows these rules to an agent before its write
// (POST /api/v1/writing-rules), so the agent reads the same rules as the skill holds.
//
// The text is embedded from skill/, a copy of skills/plain-words that `go generate` makes, since
// a Go embed cannot read a file outside its module. TestTheCopyIsTheSkill fails when the copy and
// the skill differ, so an edit of the skill without `go generate` fails the build's tests.
package plainwords

import (
	_ "embed"
	"strings"
)

//go:generate sh -c "rm -rf skill && cp -R ../../../../../../skills/plain-words skill"

// skillFile is the skill's SKILL.md whole, its frontmatter included.
//
//go:embed skill/SKILL.md
var skillFile string

// Patterns is the skill's references/patterns.md: ce-noslop's catalog of the patterns that mark
// a text as an agent's, each with its fix.
//
//go:embed skill/references/patterns.md
var Patterns string

// Terminology is the skill's references/terminology.md: how to choose the words of a sentence.
//
//go:embed skill/references/terminology.md
var Terminology string

// Skill is SKILL.md after its frontmatter: ce-noslop's modes, register and tests, then the five
// phrases it tells agents to stop.
var Skill = withoutFrontmatter(skillFile)

// Rules is the whole of the written rules as one text: Skill, then Patterns, then Terminology,
// each whole and a blank line between them. SKILL.md names the two references by their
// skill:// address, and here they follow it.
var Rules = Skill + "\n" + Patterns + "\n" + Terminology

// withoutFrontmatter is text after its leading `---` frontmatter block and the blank lines that
// follow it; text with no such block is answered whole.
func withoutFrontmatter(text string) string {
	rest, found := strings.CutPrefix(text, "---\n")
	if !found {
		return text
	}
	_, body, found := strings.Cut(rest, "\n---\n")
	if !found {
		return text
	}
	return strings.TrimLeft(body, "\n")
}
