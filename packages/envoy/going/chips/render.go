package chips

import (
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// render.go writes an answer as the person reads it in the command bar (RenderAnswer), and counts
// the words the bar draws of it (DrawnWords). The bar's conversation draws an answer with chips
// (the web's MarkdownBody with chips, ItemChip and RefLink): each link to an item shows the item,
// read from Dispatch, in place of the words the model gave the link. The eval's judge reads the
// answer so (going/eval), and a fact the person reads in a chip is stated, and words the chip
// replaced are not. A time the model wrote as a link ([5 days ago](dispatch-time:2026-11-18T11:35:00Z),
// the web's dispatch-time.tsx) shows its words and its full date on hover, so the judge reads
// both; a time link whose words are an issue's key shows the issue's chip in place of its words
// (the web's timeLinkIssue), so the judge reads the chip and the date. The server counts the
// words of an answer as the bar draws them (internal/dispatch/api's going_agent_words.go), which
// holds the answer's bound of 120 words: the date on a time link's hover is no word it draws, an
// issue is its chip at its first link alone, and a link that reads as its own words is those.

// RenderAnswer is answer, an answer's markdown, with each link the bar draws as an item written in
// place as the words the bar draws for it, read from items (Target.Words), between square
// brackets. A link to an issue (dispatch://DEMO-12, /issues/DEMO-12) is the issue's chip: its key,
// its status as the chip words it and its title, [DEMO-12 · In progress · Export: CSV and PDF]. A
// link to an issue's spec or to a tab of its page is the issue's title; to a document or a file of
// an issue or a project, its name; to an ask, a comment or a message, the head of its text, cut as
// the bar cuts a link's title. A link to an item that items does not hold, and a link to a block
// or to an agent's file, which items holds no words of, is its bare reference, such as [DEMO-404],
// so it carries no fact the store does not hold. A time link is its words and then the full date
// its hover shows (timeLinkWords): [5 days ago](dispatch-time:2026-11-18T11:35:00Z) is "5 days ago
// (18 Nov 2026, 11:35 UTC)". A time link whose words are an issue's key and nothing else, which
// the bar draws as the issue's chip with the time on hover, is the words of the issue's chip
// between square brackets and then that date: [DEMO-12](dispatch-time:2026-10-10T09:00:00Z) is
// "[DEMO-12 · In progress · Export: CSV and PDF] (10 Oct 2026, 09:00 UTC)", and the bare key in
// brackets for an issue items does not hold. Every other link - a web address, a picture, a
// reference the bar does not read - and all other text, code included, stays as written.
//
// The links are markdown's inline links, [words](href), and its autolinks, <dispatch://...>; a
// dashboard path names its item as it does on the dashboard's own origin. A web address names no
// item: the bar draws one as a chip only on its own origin, which an answer gives as a path.
func RenderAnswer(answer string, items Items) string {
	return render(answer, items, true)
}

// DrawnWords is how many words the bar draws of answer, an answer's markdown, with each item it
// links read from items: the answer written as the bar draws it (render where hover is not set) -
// the first link to each issue as its chip, its key, its status and its title, a later link to
// the same issue and a link to another item that reads as its own words as those words, every
// other link to an item as the words the bar draws for it, and each time link as its words alone,
// or its issue's chip, since the date on its hover is not drawn; then split on white space, each
// piece that holds a letter or a digit one word. So the marks of markdown that stand alone, a
// list's dash or a heading's hashes, and a chip's separators (·) are no words, and a word with
// marks around it, **Done**, is one. Each link stands in its own element in the bar's DOM, so a
// link's drawn words are separated by space from adjacent text such as a possessive
// ([DEMO-1](dispatch://DEMO-1)'s), as the DOM text-node counter joins them.
// packages/contracts/fixtures/drawn-words.json holds answers with the words the bar draws of each,
// which a test of the web's MarkdownBody holds it to too.
func DrawnWords(answer string, items Items) int {
	words := 0
	for piece := range strings.FieldsSeq(render(answer, items, false)) {
		if strings.ContainsFunc(piece, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			words++
		}
	}
	return words
}

// render is answer as RenderAnswer writes it where hover is set, each time link with the date its
// hover shows. Where hover is not set, it is the answer as the bar draws it: each time link as its
// words alone, each link whose own words the bar draws (drawnLinks.ownWords) as those words, and
// space either side of every link so text directly beside it is not joined onto the link's words.
func render(answer string, items Items, hover bool) string {
	var drawn drawnLinks
	return RewriteLinks(answer, func(out *strings.Builder, written, words, href string) {
		if !hover {
			out.WriteByte(' ')
		}
		if !hover && drawn.ownWords(words, href) {
			out.WriteString(words)
		} else if timed, ok := timeLinkWords(words, href, items, hover); ok {
			out.WriteString(timed)
		} else if named, ok := ParseTarget(href); !ok {
			out.WriteString(written)
		} else {
			out.WriteByte('[')
			out.WriteString(named.Words(items))
			out.WriteByte(']')
		}
		if !hover {
			out.WriteByte(' ')
		}
	})
}

// drawnLinks is what the bar drew of an answer's links so far, read in the order the answer holds
// them: the key of each issue whose chip it drew.
type drawnLinks struct {
	chipped []string
}

// ownWords reports whether the bar draws the next link of the answer, whose words are words and
// whose address is href, as those words, as the web's MarkdownBody with chips draws it. The bar
// draws the first link to each issue as the issue's chip (takeIssueLinks): a link to the issue
// itself, by its reference or its dashboard path, and a time link whose words are its key
// (timeLinkKey); a later link to the same issue it draws as the words it was written with. A link
// to another item - an issue's spec, a tab of its page, a document, an ask, a comment, a message,
// a block - it draws as its own words where they are not its address (RefLink, which reads such
// words as the author's by @legion/contracts' referenceReading), and as the item's name where
// they are. Every other link, a time's words included, is no item's link and is drawn as it is.
func (d *drawnLinks) ownWords(words, href string) bool {
	var key string
	if isTimeAddress(href) {
		key = shownWords(words)
		if !timeLinkKey.MatchString(key) {
			return false
		}
	} else {
		target, ok := ParseTarget(href)
		if !ok {
			return false
		}
		if target.ref.Kind != "issue" || target.tab != "" {
			return shownWords(words) != href
		}
		key = target.ref.IssueKey
	}
	if slices.Contains(d.chipped, key) {
		return true
	}
	d.chipped = append(d.chipped, key)
	return false
}

// timeScheme is the scheme of the address of a time the model writes as a link.
const timeScheme = "dispatch-time:"

// timeInstant is an instant as a time link's address writes it, the shape the web reads (its
// INSTANT): a date, a time to the minute or finer, and its zone.
var timeInstant = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:\d{2})$`)

// timeLinkKey is an issue's key as Dispatch writes one (issueKey): the words of a time link that
// the bar draws as the issue's chip, which are the key and nothing else (the web's timeLinkIssue).
var timeLinkKey = regexp.MustCompile("^(?:" + issueKey.String() + ")$")

// shownWords is a link's words as the page reads them for its chip, the text it draws
// (textContent): spaces around them, a pair of emphasis or code marks around the whole, and a
// backslash before a hyphen left out, so `**DEMO-12**` and `DEMO\-12` are the key DEMO-12 there
// as they are here. A mark that is not one of a pair stays, as Markdown draws it.
func shownWords(words string) string {
	shown := strings.ReplaceAll(strings.TrimSpace(words), `\-`, "-")
	for {
		trimmed := shown
		for _, mark := range []string{"**", "__", "*", "_", "`"} {
			if len(shown) > 2*len(mark) && strings.HasPrefix(shown, mark) && strings.HasSuffix(shown, mark) {
				trimmed = strings.TrimSpace(shown[len(mark) : len(shown)-len(mark)])
				break
			}
		}
		if trimmed == shown {
			return shown
		}
		shown = trimmed
	}
}

// timeLinkWords is what the bar shows for a link whose words are words and whose address is
// href, and whether href is a time's address at all, of any case and spaces around it, as the
// web's isTimeAddress reads it. Its words are words, or, where they are an issue's key and nothing
// else, around spaces (timeLinkKey), the words of the issue's chip read from items (Target.Words)
// between square brackets, as the bar draws that issue's chip in their place. Where hover is set,
// for an address that names an instant, the words and then, in parentheses, the instant's full
// date and time in UTC, as the hover shows it: "5 days ago (18 Nov 2026, 11:35 UTC)", its seconds
// too where it has some. For an address that names none, such as dispatch-time:last week, and
// where hover is not set, the words alone, as the bar draws them.
func timeLinkWords(words, href string, items Items, hover bool) (string, bool) {
	if !isTimeAddress(href) {
		return "", false
	}
	href = strings.TrimSpace(href)
	if key := shownWords(words); timeLinkKey.MatchString(key) {
		if issue, ok := ParseTarget("dispatch://" + key); ok {
			words = "[" + issue.Words(items) + "]"
		}
	}
	if !hover {
		return words, true
	}
	at, ok := linkInstant(href[len(timeScheme):])
	if !ok {
		return words, true
	}
	layout := "2 Jan 2006, 15:04 UTC"
	if at.Second() != 0 {
		layout = "2 Jan 2006, 15:04:05 UTC"
	}
	return words + " (" + at.Format(layout) + ")", true
}

// isTimeAddress reports whether href is a time's address, of any case and spaces around it, as
// the web's isTimeAddress reads it.
func isTimeAddress(href string) bool {
	href = strings.TrimSpace(href)
	return len(href) >= len(timeScheme) && strings.EqualFold(href[:len(timeScheme)], timeScheme)
}

// linkInstant is the instant written, a time link's address after its scheme, names, in UTC, and
// false where it names none: where it has no time's shape or no such time is, such as a 31 Nov.
// A time to the minute is read at its minute's start.
func linkInstant(written string) (time.Time, bool) {
	match := timeInstant.FindStringSubmatchIndex(written)
	if match == nil {
		return time.Time{}, false
	}
	if match[2] < 0 {
		zone := match[6]
		written = written[:zone] + ":00" + written[zone:]
	}
	at, err := time.Parse(time.RFC3339Nano, written)
	return at.UTC(), err == nil
}
