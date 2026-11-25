package chips

import (
	"regexp"
	"strings"
)

// RewriteLinks is markdown with each of its links outside code written by link, which is given
// the link as written, its words and its href and writes what stands in its place to out. The
// links are markdown's inline links, [words](href), and its autolinks, <dispatch://...>, whose
// words are their href. Code spans and fenced code blocks, escaped characters, pictures and all
// other text stay as written.
func RewriteLinks(markdown string, link func(out *strings.Builder, written, words, href string)) string {
	var out strings.Builder
	out.Grow(len(markdown))
	fence, from := "", 0
	for at := 0; at < len(markdown); {
		end := strings.IndexByte(markdown[at:], '\n') + 1
		if end == 0 {
			end = len(markdown) - at
		}
		line := markdown[at : at+end]
		if fence == "" {
			if opened := codeFence(line); opened != "" {
				rewriteInline(&out, markdown[from:at], link)
				fence, from = opened, at
			}
		} else if closesFence(line, fence) {
			out.WriteString(markdown[from : at+end])
			fence, from = "", at+end
		}
		at += end
	}
	if fence != "" {
		out.WriteString(markdown[from:])
	} else {
		rewriteInline(&out, markdown[from:], link)
	}
	return out.String()
}

// fenceLine is a line that opens or closes a fenced code block: up to three spaces, then three
// backticks or tildes or more.
var fenceLine = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")

// codeFence is the fence line opens a fenced code block with, "" for a line that opens none: a
// backtick fence whose info string holds a backtick is no fence.
func codeFence(line string) string {
	match := fenceLine.FindStringSubmatch(line)
	if match == nil || (match[1][0] == '`' && strings.Contains(line[len(match[0]):], "`")) {
		return ""
	}
	return match[1]
}

// closesFence reports whether line closes the code block fence opened: a fence of the same mark,
// as long or longer, with nothing but spaces after it.
func closesFence(line, fence string) bool {
	match := fenceLine.FindStringSubmatch(line)
	return match != nil && match[1][0] == fence[0] && len(match[1]) >= len(fence) && strings.TrimSpace(line[len(match[0]):]) == ""
}

// rewriteInline writes markdown text that holds no fenced code block, each link in it as link
// writes it: code spans and escaped characters as written, and a picture whole.
func rewriteInline(out *strings.Builder, markdown string, link func(out *strings.Builder, written, words, href string)) {
	for at := 0; at < len(markdown); {
		switch c := markdown[at]; {
		case c == '\\' && at+1 < len(markdown) && asciiPunct(markdown[at+1]):
			out.WriteString(markdown[at : at+2])
			at += 2
		case c == '`':
			end := codeSpanEnd(markdown, at)
			out.WriteString(markdown[at:end])
			at = end
		case c == '!' && strings.HasPrefix(markdown[at+1:], "["):
			end := at + 2
			if linkEnd, _, _, ok := inlineLink(markdown, at+1); ok {
				end = linkEnd
			}
			out.WriteString(markdown[at:end])
			at = end
		case c == '[':
			end, words, href, ok := inlineLink(markdown, at)
			if !ok {
				out.WriteByte(c)
				at++
				continue
			}
			link(out, markdown[at:end], words, href)
			at = end
		case c == '<':
			end := strings.IndexByte(markdown[at:], '>')
			if end < 0 || !autolink.MatchString(markdown[at+1:at+end]) {
				out.WriteByte(c)
				at++
				continue
			}
			autolinked := markdown[at+1 : at+end]
			link(out, markdown[at:at+end+1], autolinked, autolinked)
			at += end + 1
		default:
			out.WriteByte(c)
			at++
		}
	}
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
