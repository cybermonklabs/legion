package text

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// referenceEnd is every character that ends a reference: whitespace (the Unicode space separators
// a no-break or ideographic space belongs to as well), an angle or square bracket, a quote, a
// backtick or a pipe. No Dispatch reference holds one, so a reference in a code span ends at its
// closing backtick, one in a table cell at the cell's `|` however the cell is padded, and the link
// a document stores for the autolink `<dispatch://CORE-1>`,
// `[dispatch://CORE-1](dispatch://CORE-1)`, is two references to CORE-1 rather than one
// unparseable one. The whitespace is spelled out rather than written `\s`, which means another set
// in each language: JavaScript's also matches a vertical tab and U+FEFF. The dashboard composer
// reads text by the same rule, and `testdata/dispatch-text-references.json` is the table both are
// tested against.
const referenceEnd = `\t\n\f\r \p{Z}<>"'` + "`" + `\[\]|`

// referencePattern is a reference span. The one bracket a span may hold is a bracketed IPv6 host
// right after `http(s)://`, so a server at an IPv6 literal keeps its dashboard URLs as references.
var referencePattern = regexp.MustCompile(`(?:dispatch://|https?://(?:\[[0-9A-Fa-f:.]+\])?)[^` + referenceEnd + `]+`)
var issueKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*$`)
var projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
var artifactSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// versionPattern is a document version as a reference writes one: a positive decimal, with no sign
// and no leading zero.
var versionPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// The kinds of node a reference names by the node's own id in the store, with no issue key or
// project in the address (graphstore.KindInteraction and the kinds beside it): where such a node
// stands is a tuple, so a move changes no address. An interaction, a lens, a risk and a constraint
// are `dispatch://<kind>/<uuid>`, the uuid as the store writes it (storeUUIDPattern), and a lens
// kind is `dispatch://lens_kind/<slug>`, its slug as an artifact's is written. A finding of a lens
// is a block of the lens, `dispatch://lens/<uuid>#b-<block id>` (parseBlock). The dashboard's
// twin is storeAddressReference in `web/src/features/refs/store-address.ts`, which
// text-references.ts and the composer read text through.
var uuidAddressedKinds = []string{"interaction", "lens", "risk", "constraint"}

const lensKindAddressed = "lens_kind"

// storeUUIDPattern is a uuid as the store writes one, uuid.UUID.String's spelling: lowercase hex
// in five hyphenated groups. No other spelling names a node of those kinds, since the store keys
// each by this one.
var storeUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ComponentIDPattern is the component id charset (a lowercase slug), the regexp
// IsComponentID matches; the architecture importer quotes it in its file-name error.
const ComponentIDPattern = `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`

var componentIDPattern = regexp.MustCompile(ComponentIDPattern)

// roleTokenPattern is a role as a reference names one, `dispatch://role/<token>`: the charset a
// `role:<token>` delivery route is written in (model.ParseRoute).
var roleTokenPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// personLoginPattern is a person as a reference names one, `dispatch://person/<login>`: their
// email lowercased, the stored form of a login (model.CanonicalLogin) and the form a person's
// owner tag is written in (policy.ValidPersonOwner). A login written any other way - with an
// upper-case letter, or without a domain - is no login Dispatch holds, so it names nobody.
var personLoginPattern = regexp.MustCompile(`^[a-z0-9._%+'-]+@[a-z0-9-]+(\.[a-z0-9-]+)+$`)

func hasControl(value string) bool {
	return strings.ContainsFunc(value, unicode.IsControl)
}

// IsComponentID reports whether value is a component id: a lowercase slug of
// [a-z0-9] with single-character-bounded hyphens, such as web or dispatch-server.
func IsComponentID(value string) bool {
	return componentIDPattern.MatchString(value)
}

// IsSessionID reports whether value can own an artifact: an Envoy session id as the Agents page
// shows it, which a reference carries as written. It is not empty and holds no `/`, `?` or `#`,
// no whitespace or control character, and none of the characters that end a reference in text
// (referenceEnd), so every owner the API accepts is one a reference can name.
func IsSessionID(value string) bool {
	return value != "" && !strings.ContainsFunc(value, func(r rune) bool {
		return strings.ContainsRune(`/?#<>"'`+"`"+`[]|`, r) || r == '\ufeff' ||
			unicode.IsSpace(r) || unicode.Is(unicode.Z, r) || unicode.IsControl(r)
	})
}

// Ref is a parsed Dispatch target. ID is the issue key for issue references,
// an artifact slug for artifacts, the item identifier for asks and comments, the
// component id for components, the uuid of an interaction, a lens, a risk or a constraint, the
// slug of a lens kind, and the party's own name - a session id, a role token or a login - for
// the three parties (Kind "session", "role" and "person"), which name a reader rather than a
// record and are the kinds the graph stores nothing for (refs.TargetOf). Project is set instead
// of IssueKey for unlinked project-document references and for components, and Session instead
// of both for an artifact an agent's conversation owns (`agent/<session id>/artifact/<slug>`);
// none of the three is set for the kinds a reference names by their store id alone.
// Document is the slug of the project document an ask or a comment is written on
// (`dispatch://<PROJECT>/artifact/<slug>/ask/<id>`, and the dashboard address of the same item),
// the owner such an item has in place of an issue; it is empty for every other reference,
// an item of an issue's document included, since that item's reference names the issue alone.
// A block (Kind "block") carries the block's id as written in Block, and in Pin the version or
// revision a pinned reference (`@v<N>` after the fragment) names, or 0. A block of a document
// carries its document's fields; a block of an ask, a comment or a message, or a finding of a
// lens, carries the item's kind in Item and the item's fields, its id in ID.
type Ref struct {
	Kind     string
	Item     string
	IssueKey string
	Project  string
	Session  string
	ID       string
	Document string
	Block    string
	Pin      int
}

// Located is a parsed reference, the byte offset of its first character in the body it
// was extracted from, and whether that body writes it in a code context.
type Located struct {
	Ref
	Offset int
	// InCode says the reference sits in a code context - a fenced code block, an indented code
	// block or a code span, the three codeRanges reads - where it names nobody: a party
	// reference there is delivered to no one and joins no party set
	// (api.commentPartiesOf), while the reference itself is read and indexed as every other
	// one is.
	InCode bool
}

// Extract parses Dispatch links from body. Links outside serverURL stay URL
// references so a post never loses an ordinary external link. It reads what text cites, which the
// graph indexes, so a reference past the bounds the index keys (inBound) names nothing there: a
// `dispatch://` one is dropped, a dashboard page whose block is past them names its document
// (pageInBound), and one whose document or item is past them is the URL it is, as one that names
// no page is.
func Extract(body, serverURL string) []Ref {
	return Refs(ExtractAt(body, serverURL))
}

// Refs is the references of located, without their offsets.
func Refs(located []Located) []Ref {
	refs := make([]Ref, len(located))
	for index, item := range located {
		refs[index] = item.Ref
	}
	return refs
}

// ExtractAt is Extract with each reference's byte offset into body, so a caller can find the
// block a mention sits in. Trailing punctuation the grammar trims never moves the start.
func ExtractAt(body, serverURL string) []Located {
	return extractAt(body, serverURL, true)
}

// ParseRef reads value, the one reference a request names - an API route's `?to=` or `?from=`, or
// a block-links document - by the grammar Extract reads text by, without the bounds: they decide
// only which references text indexes, so a node whatever the length of its name, and a block
// whatever the length of its id, is read by its own reference (a block no index keys is simply
// no node). value names a reference when it starts with one and holds no other, as Extract would
// find it in text; an external URL or a dashboard URL that names no page is none. It answers a
// Ref and never the code fact: a request parameter is no markdown, and the one reference it
// names starts at its first byte, which every code context would have taken for a fence, an
// indent or a backtick.
func ParseRef(value, serverURL string) (Ref, bool) {
	located := extractAt(value, serverURL, false)
	if len(located) != 1 || located[0].Offset != 0 || located[0].Kind == "url" {
		return Ref{}, false
	}
	return located[0].Ref, true
}

// extractAt is ExtractAt, holding what each reference names to the bounds the index keys when
// bounded (Extract's reading of text, inBound) and not when not (ParseRef's of a request).
func extractAt(body, serverURL string, bounded bool) []Located {
	located, _ := extractSpans(body, serverURL, referencePattern.FindAllStringIndex(body, -1), bounded)
	return located
}

// extractSpans is extractAt over spans, body's reference spans (referencePattern) already found,
// so a reader that walks the gaps between them as well - Mentions, reading what a text writes
// bare - scans body for them once. It reads body's code contexts once here too (codeRanges),
// marking each reference that sits in one, and hands them back for that same reader to mark what
// it finds in the gaps: no consumer reads them a second time.
func extractSpans(body, serverURL string, spans [][]int, bounded bool) ([]Located, []codeRange) {
	refs := []Located{}
	code := codeRanges(body)
	var base *url.URL
	baseParsed := false
	baseOf := func() *url.URL {
		if !baseParsed {
			base, _ = url.Parse(serverURL)
			baseParsed = true
		}
		return base
	}
	for _, span := range spans {
		raw := spanReference(body, span[0], span[1])
		if raw == "" {
			continue
		}
		if ref, ok := locateSpan(raw, baseOf, bounded); ok {
			refs = append(refs, Located{Ref: ref, Offset: span[0], InCode: inCode(code, span[0])})
		}
	}
	return refs, code
}

// locateSpan is what raw, one reference span as spanReference leaves it, names as Extract reads
// it: a `dispatch://` address that parses, held to the bounds the index keys when bounded
// (inBound), else nothing; a page of the dashboard at base (parseServer), held to the bounds
// when bounded (pageInBound); and any other URL as itself, Kind "url", so a post never loses an
// ordinary link. base is called only for a span that needs it, so a text of addresses alone parses
// no server URL.
func locateSpan(raw string, base func() *url.URL, bounded bool) (Ref, bool) {
	if rest, dispatch := strings.CutPrefix(raw, "dispatch://"); dispatch {
		ref, ok := parseDispatch(rest)
		return ref, ok && (!bounded || inBound(ref))
	}
	if ref, ok := parseServer(raw, base()); ok {
		if bounded {
			ref, ok = pageInBound(ref)
		}
		if ok {
			return ref, true
		}
	}
	return Ref{Kind: "url", ID: raw}, true
}

// pageInBound is what a dashboard page cites in text: what the page names while it is in bound
// (inBound), else the document or item of a block that is not, while that one's target id fits -
// the page still opens it - else nothing.
func pageInBound(ref Ref) (Ref, bool) {
	if inBound(ref) {
		return ref, true
	}
	if ref.Kind != "block" {
		return Ref{}, false
	}
	ref.Kind, ref.Block = "artifact", ""
	if ref.Item != "" {
		ref.Kind, ref.Item = ref.Item, ""
	}
	return ref, targetFits(ref)
}

// spanReference is the reference body writes at [start, end), one reference span
// (referencePattern), as text reads it. A span that stands as an inline link's or a picture's
// address - it starts right after the `](` of `[words](` or `![caption](` - ends at the link's
// own `)`, the first in it that no `(` in it opened, as CommonMark reads a link's address
// (linkAddress): a word written straight after the link, `[@Bob](dispatch://agent/abc)hello`,
// is the link's next word to every renderer, never more of its address. What is left of a span
// is trimmed as any span is (trimReference), so `[w](dispatch://CORE-1.)` reads CORE-1 as
// `(see dispatch://CORE-1.)` does. Every reader of a span reads it here: Extract and Mentions,
// the head cuts (cutReferences) and the address spans pmdoc keeps whole (DispatchSpans). Its twin
// is `spanReference` in @legion/contracts' `reference-spans.ts`.
func spanReference(body string, start, end int) string {
	raw := body[start:end]
	if strings.HasSuffix(body[:start], "](") {
		raw = linkAddress(raw)
	}
	return trimReference(raw)
}

// linkAddress is raw up to its first `)` that no `(` before it in raw opened, or all of raw where
// every `)` closes one: a link's address read from the first byte after its `(`.
func linkAddress(raw string) string {
	depth := 0
	for at := range len(raw) {
		switch raw[at] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return raw[:at]
			}
			depth--
		}
	}
	return raw
}

// trimReference drops what trails a reference in prose: sentence punctuation, the `*`, `_` and `~`
// that close emphasis and strikethrough around it (GFM's autolinks drop the same characters,
// keeping them inside a link), and a closing parenthesis that opens nowhere in the reference. It
// counts the parentheses once and walks back from the end, so a run of closers costs one pass.
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

// DispatchSpans is where each `dispatch://` address stands in value, as text reads one: each
// reference span (referencePattern) that starts with `dispatch://`, from its first byte to the end
// of what spanReference leaves of it, in order, whether or not the address names anything. It is
// the span pmdoc keeps an address whole over, so no email or URL autolink starts inside it. Its
// twin is `referenceSpans(text, "dispatch://")` in @legion/contracts, the span the browser
// editor's engine keeps whole, read by the same rule.
func DispatchSpans(value string) [][2]int {
	if !strings.Contains(value, "dispatch://") {
		return nil
	}
	var spans [][2]int
	for _, span := range referencePattern.FindAllStringIndex(value, -1) {
		if strings.HasPrefix(value[span[0]:span[1]], "dispatch://") {
			spans = append(spans, [2]int{span[0], span[0] + len(spanReference(value, span[0], span[1]))})
		}
	}
	return spans
}

// issueTabs is every tab of an issue's page whose address names the issue itself, holding no
// block, slug or id: the dashboard's `PLAIN_ISSUE_TABS` and its conversation
// (web/src/features/refs/routes.ts). A reference to a tab, `dispatch://CORE-1/plan` or the
// dashboard's `/issues/CORE-1/plan`, reads as the issue, as the dashboard draws it, so the graph
// holds the edge the page shows. The shared text table holds a row per tab in both forms, so a tab
// one reader names and the other does not fails there.
var issueTabs = []string{"verification", "plan", "steps", "activity", "children", "artifacts", "collaborators", "conversation"}

// parseDispatch reads a `dispatch://` reference, without its scheme, by the grammar the dashboard's
// `parseDispatchReference` reads: a slug as written, an `@v` version, an agent's session id as
// written, an item id decoded as `itemSegment` decodes it, a node of a kind addressed by its store
// id alone as parseStoreAddress reads it, a `#b-` block of a document, an ask, a comment or a
// message, or a finding of a lens, as parseBlock reads it, and the three parties, `agent/<session
// id>`, `role/<token>` and `person/<login>`. An issue's key with nothing after it, a trailing
// slash, a tab of its page (issueTabs) or `log`, the conversation's older name, is the issue. A
// reference holding a control character names nothing.
func parseDispatch(value string) (Ref, bool) {
	if hasControl(value) {
		return Ref{}, false
	}
	if document, fragment, found := strings.Cut(value, "#"); found {
		return parseBlock(document, fragment)
	}
	key, tail, found := strings.Cut(value, "/")
	if issueKeyPattern.MatchString(key) {
		if !found || tail == "" || tail == "log" || slices.Contains(issueTabs, tail) {
			return Ref{Kind: "issue", IssueKey: key, ID: key}, true
		}
		if tail == "spec" {
			return Ref{Kind: "artifact", IssueKey: key, ID: "spec"}, true
		}
		// The issue's Decisions page, which reads the issue's decision nodes: the reference names
		// the issue, as the page's URL does (parseServer).
		if tail == "decisions" {
			return Ref{Kind: "issue", IssueKey: key, ID: key}, true
		}
		if artifact, ok := strings.CutPrefix(tail, "artifact/"); ok {
			if slug, ok := parseArtifactSlug(artifact); ok {
				return Ref{Kind: "artifact", IssueKey: key, ID: slug}, true
			}
			return Ref{}, false
		}
		kind, segment, _ := strings.Cut(tail, "/")
		if kind == "ask" || kind == "comment" || kind == "message" {
			if id, ok := itemSegment(segment); ok {
				return Ref{Kind: kind, IssueKey: key, ID: id}, true
			}
		}
		return Ref{}, false
	}
	if key == "agent" {
		session, rest, owns := strings.Cut(tail, "/")
		if !IsSessionID(session) {
			return Ref{}, false
		}
		// The session alone is the agent itself; anything under it is what its conversation
		// owns, which is a document and nothing else.
		if !owns {
			return Ref{Kind: "session", ID: session}, true
		}
		return parseAgentArtifact(session, rest)
	}
	if key == "role" {
		if !roleTokenPattern.MatchString(tail) {
			return Ref{}, false
		}
		return Ref{Kind: "role", ID: tail}, true
	}
	if key == "person" {
		if !personLoginPattern.MatchString(tail) {
			return Ref{}, false
		}
		return Ref{Kind: "person", ID: tail}, true
	}
	if ref, ok, addressed := parseStoreAddress(key, tail); addressed {
		return ref, ok
	}
	if !projectKeyPattern.MatchString(key) || !found {
		return Ref{}, false
	}
	if id, ok := strings.CutPrefix(tail, "component/"); ok {
		if IsComponentID(id) {
			return Ref{Kind: "component", Project: key, ID: id}, true
		}
		return Ref{}, false
	}
	artifact, ok := strings.CutPrefix(tail, "artifact/")
	if !ok {
		return Ref{}, false
	}
	parts := strings.Split(artifact, "/")
	slug, ok := parseArtifactSlug(parts[0])
	if !ok {
		return Ref{}, false
	}
	switch {
	case len(parts) == 1:
		return Ref{Kind: "artifact", Project: key, ID: slug}, true
	case len(parts) == 3 && (parts[1] == "ask" || parts[1] == "comment"):
		// The document the item is written on is its owner, so the reference carries its slug.
		if id, ok := itemSegment(parts[2]); ok {
			return Ref{Kind: parts[1], Project: key, ID: id, Document: slug}, true
		}
	}
	return Ref{}, false
}

// IsParty reports whether the reference names a party rather than a record: an agent's session,
// a role or a person, the three kinds parseDispatch reads out of `agent/<session id>`,
// `role/<token>` and `person/<login>`. A party is a reader, so the graph keys no node for one
// (refs.TargetOf) and a comment's delivering parties are read out of these alone
// (api.commentPartiesOf). Its twin on the web is isPartyRoute in `party-references.ts`.
func (r Ref) IsParty() bool {
	return r.Kind == "session" || r.Kind == "role" || r.Kind == "person"
}

// parseAgentArtifact reads what an agent's session owns, `artifact/<slug>` with an optional
// `@v` version, the one thing an agent's conversation owns. The session is its caller's, which
// read it out of the reference and held it to IsSessionID.
func parseAgentArtifact(session, rest string) (Ref, bool) {
	artifact, ok := strings.CutPrefix(rest, "artifact/")
	if !ok {
		return Ref{}, false
	}
	slug, ok := parseArtifactSlug(artifact)
	if !ok {
		return Ref{}, false
	}
	return Ref{Kind: "artifact", Session: session, ID: slug}, true
}

// parseStoreAddress reads a reference to a node of a kind addressed by its store id alone
// (uuidAddressedKinds, lensKindAddressed): key is the kind and tail what follows its `/`, which
// is the whole id, a uuid as the store writes it or a lens kind's slug. addressed is false where
// key is no such kind, and the caller reads the reference by the rest of the grammar.
func parseStoreAddress(key, tail string) (ref Ref, ok, addressed bool) {
	switch {
	case slices.Contains(uuidAddressedKinds, key):
		ok = storeUUIDPattern.MatchString(tail)
	case key == lensKindAddressed:
		ok = artifactSlugPattern.MatchString(tail)
	default:
		return Ref{}, false, false
	}
	if !ok {
		return Ref{}, false, true
	}
	return Ref{Kind: key, ID: tail}, true, true
}

// parseArtifactSlug reads a slug as a reference writes it, whole, with an optional `@v` version.
func parseArtifactSlug(value string) (string, bool) {
	slug, version, versioned := strings.Cut(value, "@")
	if versioned {
		number, ok := strings.CutPrefix(version, "v")
		if !ok || !versionPattern.MatchString(number) {
			return "", false
		}
	}
	if !artifactSlugPattern.MatchString(slug) {
		return "", false
	}
	return slug, true
}

// MaxBlockIDBytes bounds a block id, decoded, in UTF-8 bytes. The reference graph keys a block by
// it in B-tree keys - document_blocks', document_block_texts' and block_refs' primary keys, and a
// reference's refs.to_id - and a Postgres B-tree key holds at most 2704 bytes, so an unbounded id
// fails every write that indexes it. (A block's node id `<ref_key>#<block id>` holds the
// document's unbounded ref_key too, so it is hash indexed, never a B-tree key.) Text citing a
// longer id cites nothing (inBound), and the index makes no node of a block carrying one
// (refs.PlanBlocks). It decides only which blocks the graph keys: a block whose id is longer is
// written, stored, read and rendered as any other, and a request naming it (ParseRef) finds no
// node. Its twin is MAX_BLOCK_ID_BYTES in @legion/contracts.
const MaxBlockIDBytes = 256

// MaxTargetBytes bounds a reference's target id in text, in bytes: TargetID, which refs.to_id and
// block_refs.to_id store in B-tree keys (refs_pkey, refs_to, block_refs_pkey, block_refs_to_source)
// of at most 2704 bytes. The grammar bounds no part of that id but a project key, and text no part
// but a block id (MaxBlockIDBytes), so text citing a target whose id is past this cites nothing
// (inBound): Extract finds no reference there, and a live document page whose fragment names a
// block past it names the document. The bound decides only what text indexes: data stored under
// a longer name - a document whose ref_key is longer - is read and written all the same, and a
// request naming it reads it (ParseRef). At the bound the widest of those keys, block_refs_pkey
// with a citing block id of MaxBlockIDBytes, is about 2360 bytes, block_refs_to_source with the
// citing document's uuid as text about 2130, and refs_pkey with a uuid source and a `component`
// target about 2120. Its twin is MAX_REFERENCE_TARGET_BYTES in @legion/contracts.
const MaxTargetBytes = 2048

// TargetID is the durable id the reference graph stores an edge to ref under, refs.to_id and
// block_refs.to_id: an issue's key; a document's ref_key, `<issue key or project>/<slug>` (`spec`
// for an issue's spec) or `agent/<session id>/<slug>`; a document block's `<document's
// ref_key>#<block id>`; an ask's, a comment's or a message's block's `<item kind>/<item id>#<block
// id>` (`comment/<uuid>#b2`, a lowercase kind no issue key, project or `agent/` can be), and a
// finding's `lens/<uuid>#<block id>` the same way; a component's `<project>/<id>` node address;
// an ask's, a comment's or a message's id; and the uuid of an interaction, a lens, a risk or a
// constraint and the slug of a lens kind, each the id the store keys that node by. A document's
// version and a block's pin are not part of it: a pinned and an unpinned reference to one block
// are one target. A party's id is its own name, and the graph stores no edge under it
// (refs.TargetOf): it is read only to hold a party reference to the same bound as any other
// (targetFits).
func TargetID(ref Ref) string {
	switch ref.Kind {
	case "artifact":
		if ref.Session != "" {
			return "agent/" + ref.Session + "/" + ref.ID
		}
		key := ref.IssueKey
		if key == "" {
			key = ref.Project
		}
		return key + "/" + ref.ID
	case "component":
		return ref.Project + "/" + ref.ID
	case "block":
		if ref.Item != "" {
			return ref.Item + "/" + ref.ID + "#" + ref.Block
		}
		document := ref
		document.Kind, document.Block, document.Pin = "artifact", "", 0
		return TargetID(document) + "#" + ref.Block
	}
	return ref.ID
}

// targetFits reports whether ref, a parsed reference, names a target id (TargetID) of at most
// MaxTargetBytes.
func targetFits(ref Ref) bool {
	return len(TargetID(ref)) <= MaxTargetBytes
}

// inBound reports whether text citing ref cites it, the bounds the graph's index keys: its target
// id fits (targetFits) and a block's id holds at most MaxBlockIDBytes.
func inBound(ref Ref) bool {
	return len(ref.Block) <= MaxBlockIDBytes && targetFits(ref)
}

// EncodeBlockID writes a block id as a reference's `#b-` fragment carries it: every UTF-8 byte
// percent-encoded in upper-case hex except ASCII letters, digits and `-`, so no character a
// reference ends at or the grammar trims, and no `@` a pin could be read from, is written bare.
// Its twin is encodeBlockId in @legion/contracts; both are tested against
// `testdata/dispatch-block-id-encodings.json`.
func EncodeBlockID(id string) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(id))
	for i := range len(id) {
		c := id[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(digits[c>>4])
		b.WriteByte(digits[c&15])
	}
	return b.String()
}

// FormatBlockRef writes a block of a document as every writer writes one:
// `dispatch://<ISSUE>/spec` for a block of an issue's primary document,
// `dispatch://<ISSUE or PROJECT>/artifact/<slug>` for any other document of an issue or a project
// (a project's document whose slug is spec included), then `#b-` and the id as EncodeBlockID
// writes it, then `@v<Pin>` when the reference is pinned. It is false for a reference that is not
// to a block, for a block of a comment or a message (Item set), and for a block of an agent
// conversation's document, which no block reference names. Its twin is formatBlockReference in
// @legion/contracts.
func FormatBlockRef(ref Ref) (string, bool) {
	if ref.Kind != "block" || ref.Item != "" || ref.Session != "" || ref.ID == "" || (ref.IssueKey == "") == (ref.Project == "") {
		return "", false
	}
	var document string
	switch {
	case ref.IssueKey != "" && ref.ID == "spec":
		document = ref.IssueKey + "/spec"
	case ref.IssueKey != "":
		document = ref.IssueKey + "/artifact/" + ref.ID
	default:
		document = ref.Project + "/artifact/" + ref.ID
	}
	written := "dispatch://" + document + "#b-" + EncodeBlockID(ref.Block)
	if ref.Pin > 0 {
		written += "@v" + strconv.Itoa(ref.Pin)
	}
	return written, true
}

// CanonicalBlockRef reads value as one block of a document written exactly as FormatBlockRef
// writes it - the one way a typed block's `block[]` attribute holds a pointer: value is the one
// reference text would cite there (ExtractAt with no server URL, so a dashboard URL is never one),
// it starts at value's first byte, it is a block, and FormatBlockRef writes it back as value. That
// last check refuses what the reader alone accepts: lower-case or needless percent-encoding, an
// issue's primary document spelled `artifact/spec`, anything trailing the reference, and a block
// of a comment or a message.
func CanonicalBlockRef(value string) (Ref, bool) {
	located := ExtractAt(value, "")
	if len(located) != 1 || located[0].Offset != 0 || located[0].Kind != "block" {
		return Ref{}, false
	}
	if written, ok := FormatBlockRef(located[0].Ref); !ok || written != value {
		return Ref{}, false
	}
	return located[0].Ref, true
}

// canonicalItemUUID is an item's id as an item reference writes it: the whole uuid, in lower
// case, with its four hyphens, which is the spelling every item route returns.
var canonicalItemUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// FormatItemBlockRef writes an issue's ask, comment or message, or a block of one, as a
// placement names it: `dispatch://<ISSUE>/<ask|comment|message>/<uuid>` for the item itself
// (Kind the item's kind), and that followed by `#b-<id>` for a block of it (Kind "block", Item the
// item's kind), the item's whole lower-case uuid and the id as EncodeBlockID writes it, with no
// pin, because a node placed under an item is placed under the item as it stands and not under
// one revision of it. It is false for any other reference: a document or a block of one, an item
// a project document holds (Project set), an item whose id is no whole lower-case uuid (a prefix,
// an upper-case spelling), a pinned block and a block with no id.
func FormatItemBlockRef(ref Ref) (string, bool) {
	item := ref.Item
	if ref.Kind != "block" {
		item = ref.Kind
		if ref.Item != "" || ref.Block != "" {
			return "", false
		}
	} else if ref.Block == "" {
		return "", false
	}
	if ref.Pin != 0 || ref.Project != "" || ref.Session != "" || ref.Document != "" ||
		!issueKeyPattern.MatchString(ref.IssueKey) || !canonicalItemUUID.MatchString(ref.ID) ||
		(item != "ask" && item != "comment" && item != "message") {
		return "", false
	}
	written := "dispatch://" + ref.IssueKey + "/" + item + "/" + ref.ID
	if ref.Kind == "block" {
		written += "#b-" + EncodeBlockID(ref.Block)
	}
	return written, true
}

// CanonicalItemBlockRef reads value as one of an issue's asks, comments or messages, or a block
// of one, written exactly as FormatItemBlockRef writes it: value is the one reference text would
// cite there (ExtractAt with no server URL, which holds the block id and the target id to the
// bounds text cites by, as CanonicalBlockRef reads a block of a document), it starts at value's
// first byte, and FormatItemBlockRef writes it back as value. That refuses what the reader alone
// accepts: a uuid prefix or an upper-case uuid, lower-case or needless percent-encoding, a pin,
// anything trailing the reference, a project document's item or a block of one, and a document or
// a block of one.
func CanonicalItemBlockRef(value string) (Ref, bool) {
	located := ExtractAt(value, "")
	if len(located) != 1 || located[0].Offset != 0 {
		return Ref{}, false
	}
	if written, ok := FormatItemBlockRef(located[0].Ref); !ok || written != value {
		return Ref{}, false
	}
	return located[0].Ref, true
}

// parseBlock reads `<document or item reference>#b-<block id>[@v<N>]`: a block of an issue's
// spec, an issue's document or a project's document, optionally pinned to version N of that
// document, or a part of an ask (`KEY/ask/<id>`, or a project document's
// `PROJECT/artifact/<slug>/ask/<id>`), optionally pinned to revision N of the ask's parts, or a
// block of a comment (`KEY/comment/<id>`, or a project document's
// `PROJECT/artifact/<slug>/comment/<id>`), optionally pinned to revision N of that comment, or of
// an issue's message (`KEY/message/<id>`), or a finding of a lens (`lens/<uuid>`). A message
// records no revisions and a lens records no revision of its findings, so a pinned block of either
// is no reference. The id is read with any valid percent-encoding (EncodeBlockID writes one), so
// `%40` is an `@` in the id and a raw `@` is only ever the pin. Any other fragment, a raw `@` that
// does not start a well-formed pin, a versioned document, an agent conversation's document, or an
// id that is empty or does not decode names nothing. (Extract holds the block's id to
// MaxBlockIDBytes and its whole target id to MaxTargetBytes in text, inBound.) A document version
// shows only as an `@` in the document part: no key or slug holds one.
func parseBlock(document, fragment string) (Ref, bool) {
	encoded, ok := strings.CutPrefix(fragment, "b-")
	if !ok || strings.Contains(document, "@") {
		return Ref{}, false
	}
	pin := 0
	if unpinned, version, pinned := strings.Cut(encoded, "@"); pinned {
		number, ok := strings.CutPrefix(version, "v")
		if !ok || !versionPattern.MatchString(number) {
			return Ref{}, false
		}
		parsed, err := strconv.Atoi(number)
		if err != nil {
			return Ref{}, false
		}
		encoded, pin = unpinned, parsed
	}
	id, ok := blockID(encoded)
	if !ok {
		return Ref{}, false
	}
	ref, ok := parseDispatch(document)
	switch {
	case !ok:
		return Ref{}, false
	case ref.Kind == "artifact" && ref.Session == "":
	case ref.Kind == "ask", ref.Kind == "comment", ref.Kind == "message" && pin == 0, ref.Kind == "lens" && pin == 0:
		ref.Item = ref.Kind
	default:
		return Ref{}, false
	}
	ref.Kind, ref.Block, ref.Pin = "block", id, pin
	return ref, true
}

// blockID decodes the id part of a `#b-` fragment: not empty, holding no raw `@`, and decoding
// as decodeItemID decodes an item id to a non-empty id, of any length.
func blockID(encoded string) (string, bool) {
	if encoded == "" || strings.Contains(encoded, "@") {
		return "", false
	}
	id, ok := decodeItemID(encoded)
	return id, ok && id != ""
}

// itemSegment is an item id as a reference writes it, decoded, as the dashboard reads one: written
// without a `/`, `?`, `#` or whitespace (JavaScript's `\s`, which adds U+FEFF to the Unicode space
// separators; its ASCII whitespace is control characters, which no reference holds), then decoded
// by `decodeItemID`.
func itemSegment(raw string) (string, bool) {
	if raw == "" || strings.ContainsFunc(raw, func(r rune) bool {
		return r == '/' || r == '?' || r == '#' || r == '\ufeff' || unicode.Is(unicode.Z, r)
	}) {
		return "", false
	}
	return decodeItemID(raw)
}

// decodeItemID decodes an item id as JavaScript's decodeURIComponent does, refusing a `%` that two
// hex digits do not follow and escapes that do not decode to UTF-8, and refuses an id that decodes
// to a control character, as a reference written with one is refused: no item has such an id, and
// the index binds ids as `text[]`, where Postgres refuses a NUL and fails the write holding it.
func decodeItemID(value string) (string, bool) {
	decoded, err := url.PathUnescape(value)
	return decoded, err == nil && utf8.ValidString(decoded) && !hasControl(decoded)
}

// parseServer reads a dashboard URL against its parsed server URL as the dashboard's
// `referenceRouteFromHref` reads it: the key and the path's item id as written, the query as
// `searchParams` reads it, a version a positive decimal, and a `#b-` fragment on a document page
// as documentPage reads it and on an issue ask's, comment's or message's page as pageBlock reads
// it. A URL holding a control character names nothing; net/url refuses only the ASCII ones.
func parseServer(raw string, base *url.URL) (Ref, bool) {
	if base == nil || base.Scheme == "" || base.Host == "" {
		return Ref{}, false
	}
	if hasControl(raw) {
		return Ref{}, false
	}
	value, err := url.Parse(raw)
	if err != nil || value.Scheme != base.Scheme || value.Host != base.Host {
		return Ref{}, false
	}
	basePath := strings.TrimSuffix(base.EscapedPath(), "/")
	path := value.EscapedPath()
	if basePath != "" {
		if !strings.HasPrefix(path, basePath+"/") {
			return Ref{}, false
		}
		path = strings.TrimPrefix(path, basePath)
	}
	query := searchParams(value.RawQuery)
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "issues" {
		key := parts[1]
		if !issueKeyPattern.MatchString(key) {
			return Ref{}, false
		}
		switch {
		// The issue's page, or a tab of it that names the issue itself (issueTabs).
		case len(parts) == 2 || (len(parts) == 3 && slices.Contains(issueTabs, parts[2])):
			return Ref{Kind: "issue", IssueKey: key, ID: key}, true
		// A document page of the issue, which `dispatch search` links to with the item in its query
		// for an anchored comment or ask.
		case len(parts) == 3 && parts[2] == "spec":
			return documentPage(Ref{Kind: "artifact", IssueKey: key, ID: "spec"}, query, "v", value)
		// The Decisions tab reads the issue's decision nodes, so its page names the issue,
		// whatever its fragment, as `dispatch://KEY/decisions` does (parseDispatch) and as the
		// dashboard and the agent client read it.
		case len(parts) == 3 && parts[2] == "decisions":
			return Ref{Kind: "issue", IssueKey: key, ID: key}, true
		// A document's page, at `artifacts/<slug>` or at `artifact/<slug>`, which the issue page opens
		// as the same document (the dashboard's `parseIssuePath`).
		case len(parts) == 4 && (parts[2] == "artifacts" || parts[2] == "artifact"):
			slug, err := url.PathUnescape(parts[3])
			if err != nil || !artifactSlugPattern.MatchString(slug) {
				return Ref{}, false
			}
			if version := query.Get("v"); version != "" && !versionPattern.MatchString(version) {
				return Ref{}, false
			}
			return documentPage(Ref{Kind: "artifact", IssueKey: key, ID: slug}, query, "v", value)
		case len(parts) == 4 && (parts[2] == "asks" || parts[2] == "comments" || parts[2] == "messages"):
			if id, ok := itemSegment(parts[3]); ok {
				return pageBlock(Ref{Kind: strings.TrimSuffix(parts[2], "s"), IssueKey: key, ID: id}, value), true
			}
		}
		return Ref{}, false
	}
	if len(parts) == 4 && parts[0] == "agents" && parts[2] == "artifacts" {
		return agentArtifactPage(parts[1], parts[3], query)
	}
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "documents" ||
		!projectKeyPattern.MatchString(parts[1]) {
		return Ref{}, false
	}
	slug, err := url.PathUnescape(parts[3])
	if err != nil || !artifactSlugPattern.MatchString(slug) {
		return Ref{}, false
	}
	if version := query.Get("version"); version != "" && !versionPattern.MatchString(version) {
		return Ref{}, false
	}
	return documentPage(Ref{Kind: "artifact", Project: parts[1], ID: slug}, query, "version", value)
}

// documentPage reads a document page of an issue or a project: the item its query names, as
// documentItem reads it, else the block of the document its `#b-<block id>` fragment names
// (pageBlock), else the document. A `?comment=` or an `?ask=` names that item whatever the
// fragment: the dashboard writes a document block's fragment beside it, so the fragment there is
// never a block of the item. A version page (`versionParam` set) is the document whatever its
// fragment: a version's stored markdown carries no block ids to land on. Extract holds what this
// names to the bounds in text (pageInBound): a block past them is the document, a document or
// item past them nothing.
func documentPage(document Ref, query url.Values, versionParam string, page *url.URL) (Ref, bool) {
	ref, ok := documentItem(document, query)
	if !ok || ref.Kind != "artifact" || query.Get(versionParam) != "" {
		return ref, ok
	}
	return pageBlock(ref, page), true
}

// pageBlock is what a dashboard page showing ref - a document, or an ask, a comment or a message
// - names: the block its `#b-<block id>` fragment names (blockID; a raw `@` is no pin here, since
// pins are written only as `dispatch://` references), else ref itself.
func pageBlock(ref Ref, page *url.URL) Ref {
	encoded, found := strings.CutPrefix(page.EscapedFragment(), "b-")
	if !found {
		return ref
	}
	id, ok := blockID(encoded)
	if !ok {
		return ref
	}
	if ref.Kind != "artifact" {
		ref.Item = ref.Kind
	}
	ref.Kind, ref.Block = "block", id
	return ref
}

// agentArtifactPage reads the dashboard page of an artifact an agent's conversation owns,
// `/agents/<session id>/artifacts/<slug>[?v=N]`: both segments decoded as the browser encoded
// them, the session id then held to IsSessionID. The conversation owns no comment or ask, so a
// `?comment=` or `?ask=` there, which Dispatch never emits, names nothing.
func agentArtifactPage(sessionSegment, slugSegment string, query url.Values) (Ref, bool) {
	session, err := url.PathUnescape(sessionSegment)
	if err != nil || !IsSessionID(session) {
		return Ref{}, false
	}
	slug, err := url.PathUnescape(slugSegment)
	if err != nil || !artifactSlugPattern.MatchString(slug) {
		return Ref{}, false
	}
	if version := query.Get("v"); version != "" && !versionPattern.MatchString(version) {
		return Ref{}, false
	}
	if query.Has("comment") || query.Has("ask") {
		return Ref{}, false
	}
	return Ref{Kind: "artifact", Session: session, ID: slug}, true
}

// documentItem is the one link rule every Dispatch URL parser applies: `?comment=` or `?ask=` on a
// document path names that item, not the document. A query Dispatch never emits - both parameters
// at once, or an id that is empty, holds a `/` or does not decode (`decodeItemID`) - names nothing
// rather than a guess. The same rule is `itemFromSearch` in `@legion/contracts` for the SPA and the
// agent client, and `testdata/dispatch-href-references.json` is the one table all three are tested
// against.
func documentItem(document Ref, query url.Values) (Ref, bool) {
	hasAsk, hasComment := query.Has("ask"), query.Has("comment")
	if !hasAsk && !hasComment {
		return document, true
	}
	if hasAsk && hasComment {
		return Ref{}, false
	}
	kind := "comment"
	if hasAsk {
		kind = "ask"
	}
	raw := query.Get(kind)
	if raw == "" || strings.Contains(raw, "/") {
		return Ref{}, false
	}
	id, ok := decodeItemID(raw)
	if !ok {
		return Ref{}, false
	}
	// A project document's item is addressed under that document, as its `dispatch://` form
	// writes it; an issue's document's item is addressed under the issue, which names no slug.
	slug := ""
	if document.Project != "" {
		slug = document.ID
	}
	return Ref{Kind: kind, IssueKey: document.IssueKey, Project: document.Project, ID: id, Document: slug}, true
}

// searchParams reads a query as the browser's URLSearchParams does, which is how the dashboard
// reads one: pairs split on `&` alone, `+` a space, `%` and two hex digits a byte and any other
// `%` itself, and the bytes UTF-8. net/url's ParseQuery is not that reading: it also splits on
// `;`, and drops a pair holding one or holding a bad escape, so it names the document where the
// dashboard names the item, or a version the dashboard refuses.
func searchParams(rawQuery string) url.Values {
	params := url.Values{}
	for pair := range strings.SplitSeq(rawQuery, "&") {
		if pair != "" {
			name, value, _ := strings.Cut(pair, "=")
			params.Add(formDecode(name), formDecode(value))
		}
	}
	return params
}

func formDecode(value string) string {
	decoded := make([]byte, 0, len(value))
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char == '%' && index+2 < len(value) {
			if escaped, err := strconv.ParseUint(value[index+1:index+3], 16, 8); err == nil {
				decoded = append(decoded, byte(escaped))
				index += 2
				continue
			}
		}
		if char == '+' {
			char = ' '
		}
		decoded = append(decoded, char)
	}
	return strings.ToValidUTF8(string(decoded), "\uFFFD")
}
