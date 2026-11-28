package api

import "strings"

// footnoteIndent is what a footnote definition writes before each line of its content after the
// line its label opens, the renderer's definitionIndent (pmdoc's renderer.block): four columns,
// whether its first block stands on the label's line or on the line after it.
const footnoteIndent = "    "

// containerPrefix is what the containers of a document's block write before each of the block's
// lines after the first, given lead, the bytes before the block's first byte on the line it
// starts on. On that line the containers opened on an earlier line write what they write before
// each of their later lines, and those opened on the line itself write their opening syntax, which
// the renderer writes before each later line as (pmdoc's renderer.block and renderer.list): a
// quote's `> ` as itself, or `>` where its line holds nothing after it; a list item's marker,
// `- `, `* `, `N. ` or `N) `, as as many spaces, a task item's `[ ] ` or `[x] ` after the marker as
// nothing; a footnote definition's label, `[^label]: `, as the definition's four columns
// (footnoteIndent); and a typed block's opening line as nothing, since a block it holds starts on
// a later line. So each space of lead is kept, each `>` is kept with the one space after it where
// it has one, a marker (containerMarker, not `>`) followed by one space is written as that many
// spaces and one more, and a label, each `]` in which the renderer escapes with `\`, is written as
// four spaces. ok is false for a lead that holds any other byte, which no container writes before
// a block.
//
// A lead of spaces and `>` alone is its own prefix, answered with nothing allocated.
func containerPrefix(lead string) (prefix string, ok bool) {
	if strings.Trim(lead, " >") == "" {
		return lead, true
	}
	var written strings.Builder
	written.Grow(len(lead) + len(footnoteIndent))
	for at := 0; at < len(lead); {
		switch lead[at] {
		case ' ':
			written.WriteByte(' ')
			at++
		case '>':
			written.WriteByte('>')
			at++
			if at < len(lead) && lead[at] == ' ' {
				written.WriteByte(' ')
				at++
			}
		case '[':
			label := footnoteLabel(lead[at:])
			if label == 0 || !strings.HasPrefix(lead[at+label:], " ") {
				return "", false
			}
			written.WriteString(footnoteIndent)
			at += label + 1
		default:
			marker := containerMarker(lead[at:])
			if marker == 0 || !strings.HasPrefix(lead[at+marker:], " ") {
				return "", false
			}
			writeSpaces(&written, marker+1)
			at += marker + 1
			if rest := lead[at:]; strings.HasPrefix(rest, "[ ] ") || strings.HasPrefix(rest, "[x] ") {
				at += len("[ ] ")
			}
		}
	}
	return written.String(), true
}

// footnoteLabel is the length of the footnote definition's label that rest starts with, `[^`, the
// label, each `]` in which the renderer escapes with `\`, and `]:`, or 0 where rest starts with
// none.
func footnoteLabel(rest string) int {
	label, definition := strings.CutPrefix(rest, "[^")
	for at := 0; definition && at < len(label); at++ {
		switch label[at] {
		case '\\':
			at++
		case ']':
			if !strings.HasPrefix(label[at+1:], ":") {
				return 0
			}
			return len("[^") + at + len("]:")
		}
	}
	return 0
}

// placedPrefix is what the containers of a document's block write before each of its lines after
// the first, where containers is the stored texts of the blocks that hold it, outermost first
// (refs.BlockContainers, refs.BlockContainersAt), and text is the block's own stored text. Those
// texts are slices of one markdown of the document, and containers[0] stands at the document's
// top level, so its lines start at column 0. The block index records which blocks hold the block
// and not where in them it stands, so it is found: each container after the first, and then
// text, is the first occurrence of its bytes in containers[0] inside the span of the one before
// it, past that span's first byte, where that container's own syntax stands, whose lead, the
// bytes of containers[0] from the start of the occurrence's line to it, containerPrefix accepts.
// The answer is containerPrefix of text's lead. It is "" where containers is empty, for a block at
// the document's top level, and where a step finds no such occurrence, which texts of one
// markdown do not give: the block then reads as its text stands, as a block at the top level does.
//
// The first such occurrence is the block's own unless another block of the same container holds
// the same bytes after a lead that writes another prefix. Each line of a block's text after the
// first carries the prefix its lead writes, so two such blocks differ in every later line but
// where those lines start with what reads as the other's prefix, which only code whose lines start
// with spaces or `>` can do. Such a block is placed at the first of them.
func placedPrefix(containers []string, text string) string {
	if len(containers) == 0 {
		return ""
	}
	outer := containers[0]
	from, to, prefix := 0, len(outer), ""
	for step := 1; step <= len(containers); step++ {
		inner := text
		if step < len(containers) {
			inner = containers[step]
		}
		at, written, found := placedAt(outer, from, to, inner)
		if !found {
			return ""
		}
		from, to, prefix = at, at+len(inner), written
	}
	return prefix
}

// placedAt is where in outer the first occurrence of inner stands that starts past from and ends
// at to or before it, and whose lead, outer from the start of its line to it, containerPrefix
// accepts, with the prefix containerPrefix answers for that lead; found is false where no
// occurrence does.
func placedAt(outer string, from, to int, inner string) (at int, prefix string, found bool) {
	for start := from + 1; to-start >= len(inner); {
		index := strings.Index(outer[start:to], inner)
		if index < 0 {
			break
		}
		at = start + index
		if prefix, ok := containerPrefix(outer[lineStart(outer, at):at]); ok {
			return at, prefix, true
		}
		start = at + 1
	}
	return 0, "", false
}

// lineStart is where the line of text that holds the byte at starts.
func lineStart(text string, at int) int {
	return strings.LastIndexByte(text[:at], '\n') + 1
}

// nestedPrefix is what the containers of a document's block write before each of its lines after
// the first, where text is the block's stored text at a version, markdown that version's markdown
// and nested the byte ranges in it of the blocks that stand in a container and hold more than one
// line (footnoteParse.Nested): containerPrefix of the lead of the first range whose bytes are
// text, and "" where none is, so the block reads as its text stands, as a block at the
// document's top level does.
func nestedPrefix(markdown string, nested [][2]int, text string) string {
	for _, span := range nested {
		if markdown[span[0]:span[1]] == text {
			prefix, _ := containerPrefix(markdown[lineStart(markdown, span[0]):span[0]])
			return prefix
		}
	}
	return ""
}

// fencePrefix is what the containers of a typed block write before each of its lines after the
// first, read from whole, the block's stored text, for the coverage and the results reads
// (typedBlocksIn): the run of spaces and `>` that the last line of whole starts with, where the
// rest of that line is the block's closing fence, colons alone. The renderer writes that fence
// after the prefix of the block's containers (`"\n" + prefix + fence`, pmdoc's renderer.block), so
// the run is that prefix exactly. It is "" for a text of one line and for one whose last line is
// no such fence, which then reads as it stands.
func fencePrefix(whole string) string {
	end := strings.LastIndexByte(whole, '\n')
	if end < 0 {
		return ""
	}
	last := whole[end+1:]
	fence := strings.TrimLeft(last, " >")
	if fence == "" || strings.Trim(fence, ":") != "" {
		return ""
	}
	return last[:len(last)-len(fence)]
}

// withoutPrefix is whole, the stored text of a document's block, with prefix, what the block's
// containers write before each of its lines after the first (placedPrefix, nestedPrefix,
// fencePrefix), taken off each of those lines, from the outermost container in. The text is the
// document's markdown from the block's first byte to its last, so its first line starts past the
// containers' syntax and is kept as it stands. Read alone, the prefix changes the block: a fenced
// code block's or a typed block's closing fence four columns in, or after a `>`, closes nothing, a
// paragraph's line in a quote that starts with `>` opens a quote of its own, and a list item's
// line two columns in opens a list inside the item before it.
//
// Each line after the first has the segments of prefix that it carries taken off, in order
// (carriedSegments): a `>`, with the one space after it where it has one, and a run of spaces.
// From the first segment it does not carry, the rest of the line is kept as it stands: a
// paragraph's lazy continuation line carries none, and a blank line can carry fewer than all,
// since the renderer writes a blank line between blocks without a list item's indentation and a
// quote's as `>` alone, and a blank line of code in a footnote definition without the
// definition's indentation. So a blank line of code in a footnote definition that holds fewer
// spaces than the definition's indentation keeps them, as its document reads them, and one that
// holds as many or more has the definition's columns taken off, which the text cannot tell from
// the code's. A prefix of "" answers whole, with nothing allocated.
func withoutPrefix(whole, prefix string) string {
	if prefix == "" {
		return whole
	}
	first, rest, found := strings.Cut(whole, "\n")
	if !found {
		return whole
	}
	var written strings.Builder
	written.Grow(len(whole))
	written.WriteString(whole[:len(first)+1])
	for line := range strings.Lines(rest) {
		_, on := carriedSegments(prefix, line)
		written.WriteString(line[on:])
	}
	return written.String()
}

// carriedSegments walks prefix, a run of spaces and `>`, and line from their starts, segment by
// segment as withoutPrefix takes them off, while line carries each segment of prefix: a `>` where
// prefix has one, taken with the one space after it in each where it has one, and at least as
// many spaces as a run of prefix holds, of which that many are taken. at is how many bytes of
// prefix line carries, and on how many bytes of line those segments span.
func carriedSegments(prefix, line string) (at, on int) {
	for at < len(prefix) {
		if prefix[at] == '>' {
			if on == len(line) || line[on] != '>' {
				return at, on
			}
			at, on = at+1, on+1
			if at < len(prefix) && prefix[at] == ' ' {
				at++
			}
			if on < len(line) && line[on] == ' ' {
				on++
			}
			continue
		}
		spaces := spacesAt(prefix, at)
		if spacesAt(line, on) < spaces {
			return at, on
		}
		at, on = at+spaces, on+spaces
	}
	return at, on
}

// spacesAt is how many spaces text holds from at on.
func spacesAt(text string, at int) int {
	return len(text[at:]) - len(strings.TrimLeft(text[at:], " "))
}
