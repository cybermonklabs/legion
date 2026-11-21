package harness

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
)

// writingRulesRefusalLines are the lines before the rules of the refusal the `dispatch` command
// prints before a session's first write (packages/envoy-client/src/writing-rules.ts,
// refusalText), the first with %s for the command's name. TestTheRefusalIsTheCommands holds them
// to that source, so change 2's writer reads the text the command prints.
var writingRulesRefusalLines = []string{
	"dispatch %s was not called: this call is the first write of this session, or the first write after a compaction of the session.",
	"Before that write, read the rules for text that a person reads. The rules are below. Then do these steps:",
	"1. Compare each sentence of your text with the rules.",
	"2. Change each sentence that does not obey a rule.",
	"3. Run the command again with the changed text. The next call sends the write.",
	"",
}

// writingRulesRefusal is the refusal of tool's call (dispatch_ask, dispatch_comment, ...) with the
// rules Dispatch's POST /api/v1/writing-rules answered for its reader, as writing-rules.ts writes
// it.
func writingRulesRefusal(tool, rules string) string {
	lines := make([]string, len(writingRulesRefusalLines), len(writingRulesRefusalLines)+1)
	copy(lines, writingRulesRefusalLines)
	lines[0] = fmt.Sprintf(lines[0], commandName(tool))
	return strings.Join(append(lines, strings.TrimRightFunc(rules, unicode.IsSpace)), "\n")
}

// commandName is a dispatch tool's command, as dispatch-command.ts's commandName writes it:
// dispatch_comment is comment.
func commandName(tool string) string {
	return strings.ReplaceAll(strings.TrimPrefix(tool, "dispatch_"), "_", "-")
}

// checkRefusal is the refusal of pull request #3981's writing check, which change 7a gives the
// writer: each finding of the structure rules and the detector, with its fix, then what to do. The
// text is #3981's (api/writing_check.go's writingCheckRefusalOf at db71690c701c), with its last
// line naming the way to keep a correct text that this harness's writer has in place of the
// override flag R3 removed.
func checkRefusal(findings []writingcheck.Finding) string {
	var b strings.Builder
	b.WriteString("The writing check refused this write. Fix each sentence below and send the write again.\n\n")
	for i, f := range findings {
		fmt.Fprintf(&b, "%d. %s Rule %s, field %s: %s.\n", i+1, quotesAsCode(f.Rewrite), codeSpan(f.Rule), codeSpan(f.Field), codeSpan(f.Text))
	}
	b.WriteString("\nIf the text is correct as it is, send the write again as it is.")
	return b.String()
}

// codeSpan sets text as one Markdown code span on one line, with a fence longer than any run of
// backticks it holds.
func codeSpan(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	fence := "`"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		text = " " + text + " "
	}
	return fence + text + fence
}

// quotesAsCode sets every phrase a fix quotes in double quotes as inline code, so the phrase it
// names is read as a token and not as a sentence of the refusal.
func quotesAsCode(fix string) string {
	parts := strings.Split(fix, `"`)
	if len(parts)%2 == 0 {
		return fix
	}
	for i := 1; i < len(parts); i += 2 {
		parts[i] = codeSpan(parts[i])
	}
	return strings.Join(parts, "")
}
