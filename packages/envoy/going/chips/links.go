package chips

import (
	"iter"
	"regexp"
	"strings"
)

// RewriteLinks is markdown with each of its links outside code written by link, which is given
// the link as written, its words and its href and writes what stands in its place to out. The
// links are markdown's inline links, [words](href), and its autolinks, <dispatch://...>, whose
// words are their href. Code spans and fenced code blocks, escaped characters, pictures and all
// other text stay as written.
func RewriteLinks(markdown string, link func(out *strings.Builder, written, words, href string)) string {
	return rewriteMarkdown(markdown, link, writeText)
}

// rewriteMarkdown is markdown with each of its links outside code written by link, as
// RewriteLinks writes them, and each run of its plain text written by text. Plain text is what
// stands outside fenced code blocks, code spans, escaped characters, pictures and links, so a
// writer of text changes no code, no link and no address. text is given the markdown the run
// stands in, the lines between two fenced code blocks, and the run's bounds in it, so it can read
// what stands either side of the run, and writes what stands in its place to out. Fenced code
// blocks, code spans, escaped characters and pictures stay as written.
func rewriteMarkdown(markdown string, link func(out *strings.Builder, written, words, href string), text func(out *strings.Builder, markdown string, start, end int)) string {
	var out strings.Builder
	out.Grow(len(markdown))
	from := 0
	for block := range fencedBlocks(markdown) {
		rewriteInline(&out, markdown[from:block.start], link, text)
		out.WriteString(markdown[block.start:block.end])
		from = block.end
	}
	rewriteInline(&out, markdown[from:], link, text)
	return out.String()
}

// writeText writes the run of plain text from start to end of markdown as written, which is how
// RewriteLinks writes all text.
func writeText(out *strings.Builder, markdown string, start, end int) {
	out.WriteString(markdown[start:end])
}

// fenceLine is a line that opens or closes a fenced code block: up to three spaces, then three
// backticks or tildes or more.
var fenceLine = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")

// codeFence is the fence line opens a fenced code block with, "" for a line that opens none, and
// its info string, the words after the fence: a backtick fence whose info string holds a backtick
// is no fence.
func codeFence(line string) (string, string) {
	match := fenceLine.FindStringSubmatch(line)
	if match == nil || (match[1][0] == '`' && strings.Contains(line[len(match[0]):], "`")) {
		return "", ""
	}
	return match[1], strings.TrimSpace(line[len(match[0]):])
}

// closesFence reports whether line closes the code block fence opened: a fence of the same mark,
// as long or longer, with nothing but spaces after it.
func closesFence(line, fence string) bool {
	match := fenceLine.FindStringSubmatch(line)
	return match != nil && match[1][0] == fence[0] && len(match[1]) >= len(fence) && strings.TrimSpace(line[len(match[0]):]) == ""
}

// fencedBlock is one fenced code block of markdown (fencedBlocks): where the line that opens it
// starts, where its body, the lines between its fences, starts and ends, where it ends, after the
// line that closes it, and its info string (codeFence).
type fencedBlock struct {
	start, body, bodyEnd, end int
	info                      string
}

// fencedBlocks yields each fenced code block of markdown in order. A block whose fence closes
// nowhere runs to the end of markdown, its body too, as CommonMark reads one.
func fencedBlocks(markdown string) iter.Seq[fencedBlock] {
	return func(yield func(fencedBlock) bool) {
		var block fencedBlock
		fence := ""
		for at := 0; at < len(markdown); {
			end := strings.IndexByte(markdown[at:], '\n') + 1
			if end == 0 {
				end = len(markdown) - at
			}
			line := markdown[at : at+end]
			switch {
			case fence == "":
				if fence, block.info = codeFence(line); fence != "" {
					block.start, block.body = at, at+end
				}
			case closesFence(line, fence):
				block.bodyEnd, block.end = at, at+end
				if !yield(block) {
					return
				}
				fence = ""
			}
			at += end
		}
		if fence != "" {
			block.bodyEnd, block.end = len(markdown), len(markdown)
			yield(block)
		}
	}
}

// DropCodeBlocks is markdown with each fenced code block that drop reports true for taken out,
// its fence lines with it. drop is given the block's info string, the words after its opening
// fence, and its body, the lines between its fences. Every other block, and all other text, stays
// byte for byte. A block whose fence closes nowhere runs to the end of markdown, as CommonMark
// reads one.
func DropCodeBlocks(markdown string, drop func(info, body string) bool) string {
	var out strings.Builder
	// written is where the markdown not yet written to out starts.
	written := 0
	for block := range fencedBlocks(markdown) {
		if drop(block.info, markdown[block.body:block.bodyEnd]) {
			out.WriteString(markdown[written:block.start])
			written = block.end
		}
	}
	if written == 0 {
		return markdown
	}
	out.WriteString(markdown[written:])
	return out.String()
}

// rewriteInline writes markdown text that holds no fenced code block, each link in it as link
// writes it and each run of plain text between them as text writes it: code spans and escaped
// characters as written, and a picture whole.
func rewriteInline(out *strings.Builder, markdown string, link func(out *strings.Builder, written, words, href string), text func(out *strings.Builder, markdown string, start, end int)) {
	// plain is where the run of plain text that the next code span, escaped character, picture or
	// link ends starts.
	plain := 0
	// endText writes the run of plain text that ends at at with text, before what stands from at,
	// and starts the next run at end, after it.
	endText := func(at, end int) {
		if plain < at {
			text(out, markdown, plain, at)
		}
		plain = end
	}
	for at := 0; at < len(markdown); {
		switch c := markdown[at]; {
		case c == '\\' && at+1 < len(markdown) && asciiPunct(markdown[at+1]):
			endText(at, at+2)
			out.WriteString(markdown[at : at+2])
			at += 2
		case c == '`':
			end := codeSpanEnd(markdown, at)
			endText(at, end)
			out.WriteString(markdown[at:end])
			at = end
		case c == '!' && strings.HasPrefix(markdown[at+1:], "["):
			end := at + 2
			if linkEnd, _, _, ok := inlineLink(markdown, at+1); ok {
				end = linkEnd
			}
			endText(at, end)
			out.WriteString(markdown[at:end])
			at = end
		case c == '[':
			end, words, href, ok := inlineLink(markdown, at)
			if !ok {
				at++
				continue
			}
			endText(at, end)
			link(out, markdown[at:end], words, href)
			at = end
		case c == '<':
			end := strings.IndexByte(markdown[at:], '>')
			if end < 0 || !autolink.MatchString(markdown[at+1:at+end]) {
				at++
				continue
			}
			endText(at, at+end+1)
			autolinked := markdown[at+1 : at+end]
			link(out, markdown[at:at+end+1], autolinked, autolinked)
			at += end + 1
		default:
			at++
		}
	}
	endText(len(markdown), len(markdown))
}

// autolink is the inside of a markdown autolink: a scheme, a colon and no space or angle bracket.
var autolink = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]{1,31}:[^\x00-\x20<>]*$`)

// codeSpanEnd is where the code span that the run of backticks at start opens ends: after the next
// run of as many backticks, or after the run itself, which is then text, where none follows.
func codeSpanEnd(markdown string, start int) int {
	run := start
	for run < len(markdown) && markdown[run] == '`' {
		run++
	}
	width := run - start
	for at := run; at < len(markdown); {
		if markdown[at] != '`' {
			at++
			continue
		}
		closing := at
		for closing < len(markdown) && markdown[closing] == '`' {
			closing++
		}
		if closing-at == width {
			return closing
		}
		at = closing
	}
	return run
}

// inlineLink reads the inline link whose words open at the '[' at open: its words to their
// matching ']', then '(', the href (in angle brackets or bare, its parentheses balanced), an
// optional title, and ')'. It answers where the link ends, its words as written and its href, and
// false where open starts no inline link.
func inlineLink(markdown string, open int) (int, string, string, bool) {
	depth, at := 0, open
	for ; at < len(markdown); at++ {
		switch markdown[at] {
		case '\\':
			at++
		case '`':
			at = codeSpanEnd(markdown, at) - 1
		case '[':
			depth++
		case ']':
			depth--
		}
		if depth == 0 {
			break
		}
	}
	if at+1 >= len(markdown) || markdown[at+1] != '(' {
		return 0, "", "", false
	}
	words := markdown[open+1 : at]
	at = skipSpace(markdown, at+2)
	var href string
	if at < len(markdown) && markdown[at] == '<' {
		end := strings.IndexAny(markdown[at+1:], ">\n")
		if end < 0 || markdown[at+1+end] != '>' {
			return 0, "", "", false
		}
		href, at = markdown[at+1:at+1+end], at+end+2
	} else {
		start, parens := at, 0
		for ; at < len(markdown); at++ {
			c := markdown[at]
			if c == '\\' && at+1 < len(markdown) && asciiPunct(markdown[at+1]) {
				at++
				continue
			}
			if c <= ' ' || (c == ')' && parens == 0) {
				break
			}
			switch c {
			case '(':
				parens++
			case ')':
				parens--
			}
		}
		href = markdown[start:at]
	}
	at = skipSpace(markdown, at)
	if at < len(markdown) && strings.IndexByte(`"'(`, markdown[at]) >= 0 {
		closing := markdown[at]
		if closing == '(' {
			closing = ')'
		}
		end := strings.IndexByte(markdown[at+1:], closing)
		if end < 0 {
			return 0, "", "", false
		}
		at = skipSpace(markdown, at+end+2)
	}
	if at >= len(markdown) || markdown[at] != ')' {
		return 0, "", "", false
	}
	return at + 1, words, href, true
}

// skipSpace is the first byte at or after at that is no space, tab or line feed.
func skipSpace(markdown string, at int) int {
	for at < len(markdown) && (markdown[at] == ' ' || markdown[at] == '\t' || markdown[at] == '\n') {
		at++
	}
	return at
}

// asciiPunct reports whether b is an ASCII punctuation mark, as [[:punct:]] reads one.
func asciiPunct(b byte) bool {
	return '!' <= b && b <= '/' || ':' <= b && b <= '@' || '[' <= b && b <= '`' || '{' <= b && b <= '~'
}
