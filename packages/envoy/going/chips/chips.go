// Package chips is how the command bar draws the links of an answer. The bar draws an answer with
// chips (the web's MarkdownBody with chips, ItemChip and RefLink): the first link to each issue
// shows the issue's chip, read from Dispatch, in place of the words the model gave the link; a
// later link to that issue, and a link to another item with words of its own, shows those words;
// and a link to another item whose words are its address shows the item, read from Dispatch.
// RewriteLinks finds the links of an answer's markdown, ParseTarget reads the item a link names,
// and Target.Words is what the bar draws for it; RenderAnswer writes an answer as the eval's judge
// reads it with them, and DrawnWords counts the words the bar draws of it. LinkBareKeys writes an
// answer so that each issue key it names bare, which a read of its run returned, is a link the bar
// draws as the issue's chip, KeepLinkWords so that the words of a link that say more than its chip
// draws stand beside the link, and DropCodeBlocks takes out the code blocks that hold a block the
// bar draws itself; the server does each to each answer of a run as it stores it
// (internal/dispatch/api's going_link_words.go).
package chips

import (
	"cmp"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Items is what the bar reads of Dispatch to draw a link to an item: an issue's status and title
// by its key, the name of a document or a file by its owner (an issue's key or a project's key)
// and its slug, and the text of an ask, a comment or a message by the id Dispatch stores it under,
// a message on its issue. going/eval's Store answers it for a try's store after its run, its Seed
// for the seed, and the server for its store as the run's person reads it.
type Items interface {
	IssueChip(key string) (status, title string, found bool)
	DocumentName(owner, slug string) (string, bool)
	ItemText(kind, issue, id string) (string, bool)
}

// pageOrigin stands for the dashboard's origin, which a dashboard path in an answer is read
// against, as the browser reads it against its own.
const pageOrigin = "http://dispatch.invalid"

// issueTab is a link to a tab of an issue's page, which the bar draws as the issue's title: by
// its reference (dispatch://DEMO-3/plan; the conversation is also dispatch://DEMO-3/log) or by its
// dashboard path (/issues/DEMO-3/plan). The Decisions tab is its document, decisions.md, which
// text.ParseRef reads.
var issueTab = regexp.MustCompile(`^(?:dispatch://|/issues/)([A-Z][A-Z0-9]{1,9}-[1-9][0-9]*)/(verification|plan|steps|activity|children|artifacts|collaborators|conversation|log)$`)

// Target is what a link names as the bar reads its href: the item a reference or a dashboard path
// names (text.ParseRef), or, where tab is set, that tab of the page of the issue ref names.
type Target struct {
	ref text.Ref
	tab string
}

// ParseTarget is what href names, and false for an href that names no item the bar draws: a web
// address, a time (dispatch-time:), a reference of a kind the bar does not read (a component), or
// no reference. A dashboard path names its item as it does on the dashboard's own origin; a web
// address names none, since the bar draws one as a chip only on its own origin, which an answer
// gives as a path.
func ParseTarget(href string) (Target, bool) {
	value, serverURL := href, ""
	switch {
	case strings.HasPrefix(href, "dispatch://"):
	case strings.HasPrefix(href, "/") && !strings.HasPrefix(href, "//"):
		value, serverURL = pageOrigin+href, pageOrigin
	default:
		return Target{}, false
	}
	page := href
	if !strings.HasPrefix(href, "dispatch://") {
		page, _, _ = strings.Cut(page, "#")
		page, _, _ = strings.Cut(page, "?")
	}
	if match := issueTab.FindStringSubmatch(page); match != nil {
		if match[2] == "log" && !strings.HasPrefix(href, "dispatch://") {
			return Target{}, false
		}
		tab := match[2]
		if tab == "log" {
			tab = "conversation"
		}
		return Target{ref: text.Ref{Kind: "issue", IssueKey: match[1], ID: match[1]}, tab: tab}, true
	}
	ref, ok := text.ParseRef(value, serverURL)
	if !ok || (ref.Kind != "issue" && ref.Kind != "artifact" && ref.Kind != "ask" && ref.Kind != "comment" &&
		ref.Kind != "message" && ref.Kind != "block") {
		return Target{}, false
	}
	return Target{ref: ref}, true
}

// Words is what the bar draws for the target, read from items. A link to an issue is the issue's
// chip: its key, its status as the chip words it and its title, "DEMO-12 · In progress · Export:
// CSV and PDF". A link to an issue's spec or to a tab of its page is the issue's title; to a
// document or a file of an issue or a project, its name; to an ask, a comment or a message, the
// head of its text, cut as the bar cuts a link's title (itemHeadline). A link to an item that
// items does not hold, and a link to a block or to an agent's file, which items holds no words
// of, is its bare reference (Reference), such as DEMO-404, so it carries no fact the store does
// not hold.
func (t Target) Words(items Items) string {
	words, _, _ := t.drawn(items)
	return words
}

// drawn is what the bar draws for the target (Words), and, for a target the bar draws as an issue
// items holds - the issue, its spec or a tab of its page - that issue's key and title; "" for both
// for any other target.
func (t Target) drawn(items Items) (string, string, string) {
	ref := t.ref
	switch {
	case ref.Kind == "issue":
		status, title, found := items.IssueChip(ref.IssueKey)
		switch {
		case !found:
		case t.tab != "":
			return title, ref.IssueKey, title
		default:
			return ref.IssueKey + " · " + StatusText(status) + " · " + title, ref.IssueKey, title
		}
	case ref.Kind == "artifact" && ref.Session == "" && ref.IssueKey != "" && ref.ID == "spec":
		// The bar titles a link to an issue's spec with the issue's title.
		if _, title, found := items.IssueChip(ref.IssueKey); found {
			return title, ref.IssueKey, title
		}
	case ref.Kind == "artifact" && ref.Session == "":
		if name, found := items.DocumentName(ref.IssueKey+ref.Project, ref.ID); found {
			return name, "", ""
		}
	case ref.Kind == "ask" || ref.Kind == "comment" || ref.Kind == "message":
		if written, found := items.ItemText(ref.Kind, ref.IssueKey, ref.ID); found {
			return itemHeadline(written), "", ""
		}
	}
	return t.Reference(), "", ""
}

// Reference is the target's bare reference, as a task of the eval names an item: DEMO-12,
// DEMO-3/spec, DEMO-16/artifact/<slug>, OPS/artifact/<slug>, DEMO-3/ask/<id>,
// OPS/artifact/<slug>/comment/<id>, agent/<session>/artifact/<slug>, DEMO-3/plan, and a block's
// as its item's with #b-<block id>.
func (t Target) Reference() string {
	ref := t.ref
	kind := ref.Kind
	if kind == "block" {
		kind = cmp.Or(ref.Item, "artifact")
	}
	var written string
	switch kind {
	case "issue":
		written = ref.IssueKey
		if t.tab != "" {
			written += "/" + t.tab
		}
	case "artifact":
		switch {
		case ref.Session != "":
			written = "agent/" + ref.Session + "/artifact/" + ref.ID
		case ref.IssueKey != "" && ref.ID == "spec":
			written = ref.IssueKey + "/spec"
		default:
			written = ref.IssueKey + ref.Project + "/artifact/" + ref.ID
		}
	default:
		if ref.Document != "" {
			written = ref.Project + "/artifact/" + ref.Document + "/" + kind + "/" + ref.ID
		} else {
			written = ref.IssueKey + "/" + kind + "/" + ref.ID
		}
	}
	if ref.Kind == "block" {
		written += "#b-" + ref.Block
		if ref.Pin > 0 {
			written += "@v" + strconv.Itoa(ref.Pin)
		}
	}
	return written
}

// Short is the target's short form, which the bar writes for a reference inside the text of a
// link's title and for a link to an item until it reads the item (the web's shortForm): DEMO-12,
// DEMO-3 spec, DEMO-16 <slug>, OPS/<slug>, DEMO-3 ask, OPS/<slug> comment, DEMO-3 plan,
// agent/session:<8 characters>…/<slug>, and a block's as its item's with " block", and " v<N>"
// for a pinned one.
func (t Target) Short() string {
	ref := t.ref
	kind := ref.Kind
	if kind == "block" {
		kind = cmp.Or(ref.Item, "artifact")
	}
	var written string
	switch {
	case kind == "issue" && t.tab != "":
		written = ref.IssueKey + " " + strings.ReplaceAll(t.tab, "conversation", "log")
	case kind == "issue":
		written = ref.IssueKey
	case kind == "artifact" && ref.Session != "":
		return "agent/" + shortSessionID(ref.Session) + "/" + ref.ID
	case kind == "artifact" && ref.Project != "":
		written = ref.Project + "/" + ref.ID
	case kind == "artifact":
		written = ref.IssueKey + " " + ref.ID
	case ref.Document != "":
		written = ref.Project + "/" + ref.Document + " " + kind
	default:
		written = ref.IssueKey + " " + kind
	}
	if ref.Kind == "block" {
		written += " block"
		if ref.Pin > 0 {
			written += " v" + strconv.Itoa(ref.Pin)
		}
	}
	return written
}

// shortSessionID is how the bar writes a session by its id where it has no title (the contracts'
// shortSessionId): session: and its first eight characters, and the Legion daemon's identity
// whole.
func shortSessionID(id string) string {
	if strings.HasPrefix(id, "legion-daemon:") {
		return id
	}
	runes := []rune(id)
	return "session:" + string(runes[:min(len(runes), 8)]) + "…"
}

// StatusWords is each status as the issue's chip words it (the web's statusText).
var StatusWords = map[string]string{
	"triage": "Triage", "icebox": "Icebox", "backlog": "Backlog", "todo": "Todo", "in_progress": "In progress",
	"testing": "Testing", "needs_review": "Needs review", "retro": "Retro", "done": "Done",
}

// StatusText is status as the chip words it: a status of the lifecycle by its words, any other as
// it is.
func StatusText(status string) string {
	if words, ok := StatusWords[status]; ok {
		return words
	}
	return status
}

// titleHeadlineMax is how many characters of an ask's, a comment's or a message's text the bar
// titles a link to it with (the web's TITLE_HEADLINE_MAX).
const titleHeadlineMax = 60

// itemHeadline is the title the bar gives a link to an ask, a comment or a message, from its text
// (the web's useMarkdownHeadline): the words of the text as Dispatch reads it for display, on one
// line, a reference outside code written as its short form, cut to titleHeadlineMax characters
// at the last space in their second half (the web's textHead), or at the limit where none is
// there, with an ellipsis after a cut.
func itemHeadline(markdown string) string {
	var words strings.Builder
	plainWords(&words, pmdoc.ReadForDisplay(markdown, pmdoc.DisplayLatest), false)
	line := strings.Join(strings.Fields(words.String()), " ")
	runes := []rune(line)
	if len(runes) <= titleHeadlineMax {
		return line
	}
	head := runes[:titleHeadlineMax+1]
	end := titleHeadlineMax
	for at := len(head) - 1; at >= 0; at-- {
		if unicode.IsSpace(head[at]) {
			if at >= titleHeadlineMax/2 {
				end = at
			}
			break
		}
	}
	return strings.TrimRightFunc(string(runes[:end]), unicode.IsSpace) + "…"
}

// plainWords writes the words of a display tree's descendants as the web's plainWords reads them:
// a text as it reads, its references shortened outside code; an inline HTML tag as its characters;
// and every other node a space.
func plainWords(out *strings.Builder, node *pmdoc.Node, code bool) {
	for _, child := range node.Children {
		switch child.Type {
		case "text":
			if code || hasMark(child.Marks, "inlineCode") {
				out.WriteString(child.Text)
			} else {
				out.WriteString(shortenReferences(child.Text))
			}
		case "html":
			value, _ := child.Attrs["value"].(string)
			out.WriteString(value)
		default:
			out.WriteString(" ")
		}
		plainWords(out, child, code || child.Type == "code_block")
	}
}

// hasMark reports whether marks hold one of the type named.
func hasMark(marks []pmdoc.Mark, name string) bool {
	for _, mark := range marks {
		if mark.Type == name {
			return true
		}
	}
	return false
}

// referenceSpan is a reference-shaped span of text, as the web's referenceSpans and the server's
// text package find one: a dispatch:// reference or a web address, up to a space, an angle or
// square bracket, a quote, a backtick or a pipe.
var referenceSpan = regexp.MustCompile("(?:dispatch://|https?://(?:\\[[0-9A-Fa-f:.]+\\])?)[^\\t\\n\\f\\r \\p{Z}<>\"'`\\[\\]|]+")

// shortenReferences is text with each dispatch:// reference in it that names an item written as
// its short form, the punctuation that trails it kept.
func shortenReferences(written string) string {
	return referenceSpan.ReplaceAllStringFunc(written, func(span string) string {
		if !strings.HasPrefix(span, "dispatch://") {
			return span
		}
		reference := trimReference(span)
		named, ok := ParseTarget(reference)
		if !ok {
			return span
		}
		return named.Short() + span[len(reference):]
	})
}

// trimReference drops what trails a reference in prose, as the server's text package does:
// sentence punctuation, the marks that close emphasis around it, and a closing parenthesis that
// opens nowhere in it.
func trimReference(raw string) string {
	opened, closed := strings.Count(raw, "("), strings.Count(raw, ")")
	end := len(raw)
	for ; end > 0; end-- {
		switch raw[end-1] {
		case '.', ',', ';', ':', '!', '?', '*', '_', '~':
		case ')':
			if opened >= closed {
				return raw[:end]
			}
			closed--
		default:
			return raw[:end]
		}
	}
	return ""
}
