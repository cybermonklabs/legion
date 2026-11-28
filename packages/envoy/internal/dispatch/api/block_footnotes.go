package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/bytelru"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// blockTreeSource is what one block's tree is read with beside its markdown, which
// registerBlockTree takes on its own: the content typedBlocksIn hands over for the block, the one
// GET /api/v1/reference-targets cuts from the block's stored text (blockTextContent), a
// decision's section of decisions.md, or a text of a document's block the reference graph's read
// draws, an edge's excerpt among them. labels is the footnotes that markdown calls that its
// document defines outside it: the ones the block's node calls (pmdoc.CalledFootnoteLabels), or,
// for a text no node of the document's parse holds, the ones it writes as a call
// (footnotesCalledIn).
type blockTreeSource struct {
	labels []string
}

// registerBlockTree registers with trees, as field, the display tree of markdown, the content of
// a document's block, read with source beside it. Every read that answers a block's tree
// registers it here: the coverage read's requirements, the results read's checks, the batched
// reference read's blocks, the decisions read's decisions and the reference graph read's texts
// of documents' blocks, so one block answers one tree in each read that draws it whole, and each
// of them reads a call as the document does. The markdown is cut out of the document, so it
// holds no definition the document writes outside the block, and read alone it reads a call of
// such a label as text, where the document reads, and the browser editor holds, a reference. So
// the markdown of a block with labels is registered with a stand-in definition of each label
// after it (pmdoc.WithStandInDefinitions), and the stand-ins, those the markdown refers to and
// those it does not, are taken out of the tree before it is set (withoutStandInDefinitions); a
// block with none registers its markdown as it stands.
//
// A markdown can leave open a block that its document closes: an edge's excerpt, read as its cut
// stands, cut before the end of a fenced code block, or cut from one in a list item, whose stored
// text carries the item's indentation on each line but its first, so that, read alone, its closing
// fence closes nothing. A block's own text has its containers' prefix taken off before it is
// registered, the prefix its read learned from the blocks that hold it (withoutPrefix), so there
// the fence closes. The stand-ins read after such a markdown then stand inside the block it leaves
// open, so its tree does not end in them and they cannot be taken out. Such a block keeps the tree
// of its markdown alone, whatever its source, as a block with no labels does and as a block whose
// document cannot be read does: the reading of that markdown in trees' mode, taken from the
// process's display cache or built where the cache holds none (builtDisplayTree), once resolve has
// the tree with the stand-ins and so knows the block needs it. The shape is the block's own and no
// fault of the read, so nothing is logged for it.
func registerBlockTree(trees *displayTreeReader, field *model.DisplayTree, markdown string, source blockTreeSource) {
	if len(source.labels) == 0 || markdown == "" {
		trees.text(field, markdown)
		return
	}
	trees.textInto(func(tree json.RawMessage) {
		if shown, ok := withoutStandInDefinitions(tree, len(source.labels)); ok {
			*field = model.DisplayTree(shown)
			return
		}
		*field = model.DisplayTree(builtDisplayTree(pmdoc.TextSum(markdown), markdown, trees.mode))
	}, pmdoc.WithStandInDefinitions(markdown, source.labels))
}

// withoutStandInDefinitions is tree, the display tree of a block's content read with count
// stand-in footnote definitions after it (pmdoc.WithStandInDefinitions), with those definitions
// taken out, and whether tree ends in them at all: they are the last count children of its
// document, each a footnote_definition, after at least one block of the content's own, without
// which the document would hold no block to draw. Only the document's top level is decoded, and
// of each of its children only the type. The answer is tree's own bytes up to the end of the
// last child kept and from the close of the document's content on, so every other byte is as the
// reader built it, in an array of its own: tree is the reader's, shared with every request that
// reads the same text (pmdoc.DisplayJSON).
func withoutStandInDefinitions(tree json.RawMessage, count int) (json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(tree))
	if opening, err := decoder.Token(); err != nil || opening != json.Delim('{') {
		return nil, false
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		if key != "content" {
			if err := decoder.Decode(new(json.RawMessage)); err != nil {
				return nil, false
			}
			continue
		}
		if opening, err := decoder.Token(); err != nil || opening != json.Delim('[') {
			return nil, false
		}
		// ends is where each child of the document ends, and definitions how many of the last
		// children read are footnote definitions.
		var ends []int64
		definitions := 0
		for decoder.More() {
			var child struct {
				Type string `json:"type"`
			}
			if err := decoder.Decode(&child); err != nil {
				return nil, false
			}
			ends = append(ends, decoder.InputOffset())
			if child.Type == "footnote_definition" {
				definitions++
			} else {
				definitions = 0
			}
		}
		if closing, err := decoder.Token(); err != nil || closing != json.Delim(']') {
			return nil, false
		}
		kept := len(ends) - count
		if definitions < count || kept < 1 {
			return nil, false
		}
		cut, closing := ends[kept-1], decoder.InputOffset()-1
		return slices.Concat(tree[:cut], tree[closing:]), true
	}
	return nil, false
}

// footnoteDocumentsMax is how many documents one request of the batched reference read
// (blockTrees) or of the reference graph's read (readGraphFootnotes) reads and parses for the
// footnote definitions its texts call and the containers of its ambiguous blocks
// (calledFootnotes). The new issue page reads four documents of an issue - its spec, its plan, its
// Verification document and its decisions document - so 16 holds the issue's own and those of
// three issues whose blocks it names. A parse of a document of 25 to 43 KB takes 8.5 to 13.4 ms
// (decisionCuts), so 16 parses that footnoteParses does not hold take about 215 ms at that size,
// where the 256 documents that one batched read can name (contracts.ReferenceTargetsMax) would
// take about 3.4 s. A parse that works out the ranges of the markdown's nested blocks, as every
// parse of a version a pin names does and a parse of a latest version does where the request has
// an ambiguous block of that document, renders the parse once more for them (footnoteParseOf,
// nestedBlocks), about 38% more time: 1.30 ms beside a parse of 3.41 ms for a document of 39,573
// bytes, so 16 such parses take about 300 ms at that size. A request reads at most one version a
// pin names, the one its node's pin names (readGraphFootnotes).
const footnoteDocumentsMax = 16

// footnoteDocuments is what one request reads of its documents for the trees of their blocks'
// texts, over all of its calls of calledFootnotes and its read of a pin's containers
// (readGraphPrefixes): read, how many documents those calls of calledFootnotes have read, for the
// notes their texts call or the containers of their ambiguous texts, a document whose read failed
// included; unread, how many they left unread once read reached footnoteDocumentsMax;
// ambiguousUnread, how many ambiguous texts those unread documents hold, texts two of whose starts
// in their outermost container's text write different prefixes (placedPrefix), each of which
// keeps the prefix of the first start its read placed it at (documentBlockPrefix); and pinned, by
// document and version, what the request's read of each version a pin names answered
// (markdownAt).
type footnoteDocuments struct {
	read, unread, ambiguousUnread int
	pinned                        map[documentVersion]markdownRead
}

// documentVersion names one version of a document: the document's id and the version's number.
type documentVersion struct {
	artifactID string
	version    int
}

// markdownRead is what one read of a version's markdown answered (documentMarkdownAt): the
// markdown, or the error the read met.
type markdownRead struct {
	markdown string
	err      error
}

// warnUnread logs one line for a request whose calls of calledFootnotes left a document unread,
// naming route, the read the request is of, the count of documents it did not read, and the count
// of the ambiguous texts of those documents, two of whose starts write different prefixes, each
// left at the first start its read placed it at; a request that read every document it named logs
// nothing. It is called once, after the request's last call of calledFootnotes.
func (d *footnoteDocuments) warnUnread(route string) {
	if d.unread == 0 {
		return
	}
	slog.Warn("dispatch: leave the footnote definitions and nested blocks of documents past one request's bound unread for their blocks' trees",
		"read", route, "unread_documents", d.unread, "ambiguous_unread", d.ambiguousUnread, "footnote_documents_max", footnoteDocumentsMax)
}

// markdownAt is the markdown of version of the document artifactID (documentMarkdownAt). A
// version a pin names is read once in the request: what its first read answered, the markdown or
// the error, is kept in pinned and answers each later read of it. The reference graph's read
// reads such a version for the containers of the block its pin names (readGraphPrefixes) and then
// for the footnotes the pin's text calls (readGraphFootnotes), so the read of the footnotes takes
// the markdown that the read of the containers read. The document as it stands, version 0, is
// read on each call and not kept: the one call of calledFootnotes that reads it in a request reads
// each document once, so a kept markdown would save no read and would hold every markdown the call
// read until the request ends. It spends nothing of footnoteDocumentsMax: calledFootnotes spends
// one place for each document it reads, whether the request read its markdown before or not.
func (d *footnoteDocuments) markdownAt(ctx context.Context, s *server, artifactID string, version int) (string, error) {
	if version == 0 {
		return s.documentMarkdownAt(ctx, artifactID, version)
	}
	at := documentVersion{artifactID: artifactID, version: version}
	if read, held := d.pinned[at]; held {
		return read.markdown, read.err
	}
	markdown, err := s.documentMarkdownAt(ctx, artifactID, version)
	if d.pinned == nil {
		d.pinned = map[documentVersion]markdownRead{}
	}
	d.pinned[at] = markdownRead{markdown: markdown, err: err}
	return markdown, err
}

// calledFootnotes is what the documents of texts, document blocks' texts by key, give each of
// them beside its text: called, the source of its tree (blockTreeSource), the footnotes it calls
// that its document defines outside it, for the texts that hold a call at all (`[^`); and placed,
// what its containers write before each of its lines after the first, for the ambiguous texts
// (drawnText), whose bytes stand at two or more starts that container syntax allows in their
// outermost container's text, where two of those starts write different prefixes, which that
// text alone cannot tell apart (placedPrefix). A text that calls a note is a slice of its document,
// so it holds no definition the document writes outside the block, and read alone it reads a call
// of such a label as text. A key names a text, not a block: the batched reference read keys each
// block's stored text by its target (blockTrees), and the reference graph's read draws one block
// from its whole text and from an edge's excerpt of it (readGraphFootnotes), which can call
// different labels.
//
// keys is the keys of texts in the order in which the request first names each text, and the
// documents are taken in the order in which keys first names a text of each that holds a call or
// is ambiguous; a document none of whose texts here is either is not read and takes no place.
// budget is what the request reads of its documents (footnoteDocuments), what it has spent of
// footnoteDocumentsMax over all of its calls among them: while the bound has room, each document
// spends one place and is read, and a document whose read fails spends its place too, so that the
// request's order alone decides which documents are read; once the bound is spent, a document is
// not read and is counted unread in budget, with its ambiguous texts, and its texts have no entry
// in either answer, so they keep the tree of their stored text alone, as a text whose document
// cannot be read does, and an ambiguous text keeps the prefix of the first start its caller placed
// it at. What footnoteParses holds never decides which documents are read, nor whether the request
// read a document's markdown before, so the same request answers the same trees. The caller logs
// the counts of unread documents and ambiguous texts once the request's last call is done
// (footnoteDocuments.warnUnread).
//
// version is the version of their documents the texts were cut from, 0 for the latest. Each
// document is read and parsed once, however many of its blocks' texts texts holds, and that one
// parse serves both the notes its texts call and its ambiguous texts. Its id, and the id it holds
// each block under, are the ones the text's entry holds (drawnText): the batched reference read
// takes them from refs.BlockTexts, which reads them from the block index (document_blocks) with
// the block's text, and the reference graph's read from the node it draws the text for
// (graphDocumentBlock). Its latest markdown is read through TextWithToken: the block index cut
// each stored text from the markdown the document's latest version stored, and TextWithToken
// answers that markdown itself, without rendering the document, where that version is still the
// document's state and a room of this process holds it, and from the read cache by the
// document's stamp where no room holds it; Text renders a room's tree on every read. Any other
// version's markdown is the one that version stored (documentMarkdownAt), which the request reads
// once however many of its reads need it (footnoteDocuments.markdownAt): the reference graph's
// read reads the version a pin names for the block's containers before it reads it here. The
// markdown's parse as a rendering is kept by the markdown's sum (footnoteParses). The labels are
// read from it given the map of the document's texts that hold a call (footnotesCalledIn), whose
// answer is merged into called, and an ambiguous text's prefix is that of the first nested block
// of the parse whose bytes are the text (rangedPrefix, footnoteParse.Nested). A parse of any
// version but the latest works out the ranges of the markdown's nested blocks, which the
// reference graph's read of a pin's containers reads too, and a parse of the latest works them out
// only where an ambiguous text of the document needs them (footnoteParseOf). So no read parses a
// markdown again while its entry stays kept, but a read with ranges that finds it kept only
// without them, which parses it once more and keeps the ranges. An ambiguous text whose parse
// gives it no range, as a parse whose rendering is not its markdown gives none, has no entry in
// placed and keeps the prefix of its first start. A text whose document cannot be read or parsed
// has no entry and keeps the tree of its stored text alone; the failure is logged.
func calledFootnotes[K comparable](ctx context.Context, s *server, keys []K, texts map[K]drawnText, version int, budget *footnoteDocuments) (called map[K]blockTreeSource, placed map[K]string) {
	// order is the ids of the documents to read, in the order in which keys first names a text
	// of each that holds a call or is ambiguous, calling each one's texts that hold a call, by
	// key, and ambiguous the keys of each one's ambiguous texts.
	var order []string
	calling := map[string]map[K]drawnText{}
	ambiguous := map[string][]K{}
	for _, key := range keys {
		text := texts[key]
		calls := strings.Contains(text.text, "[^")
		if !calls && !text.ambiguous {
			continue
		}
		artifactID := text.block.ArtifactID
		if _, taken := calling[artifactID]; !taken {
			order = append(order, artifactID)
			calling[artifactID] = map[K]drawnText{}
		}
		if calls {
			calling[artifactID][key] = text
		}
		if text.ambiguous {
			ambiguous[artifactID] = append(ambiguous[artifactID], key)
		}
	}
	if len(order) == 0 {
		return nil, nil
	}
	called, placed = map[K]blockTreeSource{}, map[K]string{}
	for _, artifactID := range order {
		if budget.read == footnoteDocumentsMax {
			budget.unread++
			budget.ambiguousUnread += len(ambiguous[artifactID])
			continue
		}
		budget.read++
		markdown, err := budget.markdownAt(ctx, s, artifactID, version)
		if err != nil {
			slog.Warn("dispatch: read a document's footnote definitions and nested blocks for its blocks' trees", "artifact", artifactID, "error", err)
			continue
		}
		parse := footnoteParseOf(markdown, artifactID, version != 0 || len(ambiguous[artifactID]) > 0)
		if wanted := calling[artifactID]; len(wanted) > 0 {
			maps.Copy(called, footnotesCalledIn(parse, wanted))
		}
		for _, key := range ambiguous[artifactID] {
			if prefix, found := rangedPrefix(markdown, parse.Nested, texts[key].text); found {
				placed[key] = prefix
			}
		}
	}
	return called, placed
}

// documentMarkdownAt is the markdown of version of the document artifactID that calledFootnotes
// reads the document's footnote definitions and nested blocks from, and the reference graph's read
// the containers of the block a pin names at a version older than the parent tuples
// (readGraphPrefixes), both through what the request reads of its documents
// (footnoteDocuments.markdownAt): the document as it stands for version 0 (TextWithToken), and for
// any other the markdown that version stored (versionsMarkdown), which the block index cut that
// version's texts of its blocks from. A version the document does not hold is an error.
func (s *server) documentMarkdownAt(ctx context.Context, artifactID string, version int) (string, error) {
	if version == 0 {
		markdown, _, err := s.deps.Docs.TextWithToken(ctx, artifactID)
		return markdown, err
	}
	stored, err := s.versionsMarkdown(ctx, artifactID, []int{version})
	if err != nil {
		return "", fmt.Errorf("read version %d: %w", version, err)
	}
	markdown, held := stored[version]
	if !held {
		return "", fmt.Errorf("the document holds no version %d", version)
	}
	return markdown, nil
}

// drawnText is the text of a document's block that a read draws a tree of, as calledFootnotes and
// footnotesCalledIn read the footnotes it calls: block, where the block stands, the id of its
// document and its own block id; text, the text itself, a block's stored text, a decision's
// section of decisions.md, or a text of a document's block the reference graph's read draws, an
// edge's excerpt among them; and ambiguous, whether text is a block's whole stored text whose
// bytes stand at two or more starts that container syntax allows in its outermost container's
// text, where two of those starts write different prefixes (documentBlockPrefix), whose
// containers' prefix calledFootnotes then reads from its document's parse.
type drawnText struct {
	block     refs.DocumentBlock
	text      string
	ambiguous bool
}

// footnotesCalledIn is, by key, the source of the tree of each text of texts (blockTreeSource),
// where the block that the text's entry places it at (drawnText) is read from parse, the parse as
// a rendering of the markdown the text was cut from (footnoteParseOf). A typed block is the first
// node of the parse holding its id, as typedBlocksIn finds one, since the rendering writes a typed
// block's id into its opening line, and its labels are the ones its node calls
// (pmdoc.CalledFootnoteLabels), as the coverage and the results reads take them. Any other block -
// a paragraph, a heading, a list item - has no id in the rendering, so the parse gives it another,
// and so has a block its document no longer holds: no node holds its id, and its labels are each
// label the document defines that its text writes as a call, up to case
// (pmdoc.FootnoteDefinitionLabels, pmdoc.FootnoteLabelKeys.Written), read against the keys of
// those labels made at most once for the call, on the first text that needs them
// (pmdoc.KeyFootnoteLabels), so no text's labels depend on the order texts is walked in. Such a
// text can write a call that reads as text, escaped or in code, or one of a label it defines
// itself, which resolves to its own definition first; the stand-in of that label is then one
// nothing refers to, which is taken out of the tree with the rest (registerBlockTree). It answers
// the map it makes: a text that calls no such label has no entry in it, and the parse of a
// markdown the parser refuses answers none.
func footnotesCalledIn[K comparable](parse footnoteParse, texts map[K]drawnText) map[K]blockTreeSource {
	if !parse.Read {
		return nil
	}
	called := make(map[K]blockTreeSource, len(texts))
	var (
		defined pmdoc.FootnoteLabelKeys
		keyed   bool
	)
	for key, text := range texts {
		labels, held := parse.Called[text.block.BlockID]
		if !held {
			if !keyed {
				defined, keyed = pmdoc.KeyFootnoteLabels(parse.Defined), true
			}
			labels = defined.Written(text.text)
		}
		if len(labels) > 0 {
			called[key] = blockTreeSource{labels: labels}
		}
	}
	return called
}

// footnoteParse is what the reads keep of the parse of one markdown as a rendering: Read, whether
// the parser read the markdown at all; Called, by the id of each typed block of the parse, the
// labels the first node holding that id calls (pmdoc.CalledFootnoteLabels), a block that calls
// none held with no labels; Defined, the labels the document defines
// (pmdoc.FootnoteDefinitionLabels); and Nested, the byte range in the markdown of each block of the
// parse that holds more than one line and whose lead, the bytes before it on the line it starts
// on, is not empty, which is a block that stands in a container. footnotesCalledIn reads the
// first three. Nested is read for the prefix of the containers of an ambiguous block, whose bytes
// stand at two or more starts that container syntax allows in its outermost container's text,
// where two of those starts write different prefixes (calledFootnotes, rangedPrefix), and of a
// pinned block's containers at a version the graph store states no parent of it at
// (readGraphPrefixes, nestedPrefix). Only a parse made for a version a pin names, or for a latest
// version where a read has an ambiguous block of that document, works out Nested, and every other
// parse of a latest version holds none, whatever blocks its markdown nests; the key the entry is
// kept under says which it is (footnoteParseKey). Only a typed block's markdown writes its id, and
// every other block's id is one the parse mints, which names no block a text is cut from and would
// make the entry depend on more than the markdown, so Called holds the typed blocks alone, and
// Nested holds ranges, not ids. The ranges are those that the rendering of the parse gives its
// blocks (pmdoc.RenderWithBlockOffsets), which name the blocks' bytes in the markdown only where
// that rendering is the markdown byte for byte: where it is not, Nested holds none, such a pin
// reads as one whose containers the block index does not record (nestedPrefix), and such an
// ambiguous block keeps the prefix of its first start (documentBlockPrefix).
type footnoteParse struct {
	Read    bool                `json:"read"`
	Called  map[string][]string `json:"called,omitempty"`
	Defined []string            `json:"defined,omitempty"`
	Nested  [][2]int            `json:"nested,omitempty"`
}

// footnoteParseOf is the footnoteParse of markdown, a markdown of the document artifactID, with
// Nested worked out where ranged, for a version a pin names and for a latest version where a read
// has an ambiguous block of that document (calledFootnotes), and with none otherwise. It is the
// entry footnoteParses keeps of markdown with ranges (footnoteParseKey), which answers either
// read; else, for a read without ranges, the entry kept of it without them; else the one read
// from its parse as a rendering, Called and Defined from the parse and, where ranged, Nested from
// the rendering of the parse with its blocks' ranges (nestedBlocks), which is then kept. So a
// markdown is parsed at most once while an entry of it stays kept, but for a read with ranges that
// finds it kept only without them, as where a read of the latest version with no ambiguous block
// came first and a pin names that version's number, or a read of an ambiguous block of it comes
// next: that read parses the markdown once more and keeps the ranges, so a pin and an ambiguous
// block read the same Nested whatever the reads before them, and the same request answers the same
// trees and the same ETag. A refusal is kept with ranges whatever the read asked, since a markdown
// the parser refuses holds no block to range, so it too is parsed at most once while its entry
// stays kept; it is logged, with artifactID, by the parse that meets it. A parse that does not
// render back to the markdown keeps no ranges, which is no fault of the read and is not logged.
// Two reads that miss one entry at once both parse the markdown, and the entry the first keeps
// stands.
func footnoteParseOf(markdown, artifactID string, ranged bool) footnoteParse {
	sum := pmdoc.TextSum(markdown)
	held, ok := footnoteParses.Get(footnoteParseKey{sum: sum, ranged: true})
	if !ok && !ranged {
		held, ok = footnoteParses.Get(footnoteParseKey{sum: sum})
	}
	if ok {
		// held is json.Marshal of a footnoteParse, which decodes back without fail.
		var parse footnoteParse
		_ = json.Unmarshal(held, &parse)
		return parse
	}
	var parse footnoteParse
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		slog.Warn("dispatch: parse a document's footnote definitions for its blocks' trees", "artifact", artifactID, "error", err)
	} else {
		parse = footnoteParse{Read: true, Called: map[string][]string{}, Defined: pmdoc.FootnoteDefinitionLabels(tree)}
		pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
			if !pmdoc.IsTypedBlock(node.Type) {
				return true
			}
			id, _ := node.Attrs[pmdoc.BlockIDAttr].(string)
			if _, found := parse.Called[id]; id != "" && !found {
				parse.Called[id] = pmdoc.CalledFootnoteLabels(node)
			}
			return true
		})
		if ranged {
			parse.Nested = nestedBlocks(markdown, tree)
		}
	}
	// Marshal cannot fail on a bool, a map of string slices, a string slice and a slice of int
	// pairs.
	encoded, _ := json.Marshal(parse)
	footnoteParses.PutSize(footnoteParseKey{sum: sum, ranged: ranged || !parse.Read}, encoded, int64(len(encoded))+footnoteParseEntryBytes)
	return parse
}

// nestedBlocks is footnoteParse.Nested of markdown, whose parse as a rendering is tree, which
// footnoteParseOf works out for a read with ranges alone, of a version a pin names or of a latest
// version a read of an ambiguous block names: the range of each block that the rendering of tree
// gives a range (pmdoc.RenderWithBlockOffsets), every block but a list item, where its bytes hold
// more than one line and its lead is not empty; and none where that rendering is not markdown byte
// for byte, since only then are its ranges the blocks' bytes in markdown. The rendering adds about
// 38% to the time of the parse (footnoteDocumentsMax).
func nestedBlocks(markdown string, tree *pmdoc.Node) [][2]int {
	rendering, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil || rendering != markdown {
		return nil
	}
	var nested [][2]int
	for _, offset := range offsets {
		if offset.From > 0 && markdown[offset.From-1] != '\n' && strings.Contains(markdown[offset.From:offset.To], "\n") {
			nested = append(nested, [2]int{offset.From, offset.To})
		}
	}
	return nested
}

// footnoteParseEntryBytes is what one entry of footnoteParses is charged beside its encoding: its
// key (footnoteParseKey), and the map entry, list element and cache entry that hold it. bytelru's
// TestAnEntryCostsNoMoreHeapAfterTheCacheTurnsOverForLong measured 241 to 251 bytes of heap for an
// entry keyed by a SHA-256 sum on go1.27.3 linux/amd64, so the charge is that, rounded up.
const footnoteParseEntryBytes = 256

// footnoteParsesBytes bounds the entries footnoteParses keeps: 1 MiB. An entry holds labels, ids
// and offsets, not a tree or a markdown: about 45 bytes for each typed block of its document, the
// block's id (a UUID) with the labels it calls, about 14 for each block of more than one line in a
// container, the two offsets of its range, beside the labels the document defines and the charge
// for the entry itself (footnoteParseEntryBytes). Only the entry of a version a pin names, or of a
// latest version that a read of an ambiguous block of it parsed, holds ranges (footnoteParseOf),
// and the entries of the other latest versions hold none. So the bound holds the entries of about
// 500 documents of 40 typed blocks each and no ranges, as the entry of a latest version holds but
// where a read had an ambiguous block of it, of about 300 documents that hold 100 nested blocks
// beside, and of thousands with few, each of which saves a parse of 8.5 to 13.4 ms for a document
// of 25 to 43 KB (decisionCuts). A markdown read both without ranges and with them, as a latest
// version and as a version a pin names or a latest version a read of an ambiguous block names, can
// be kept twice (footnoteParseKey). It is a sliver of the bounds the display trees
// (pmdoc.DisplayCacheBytes) and the decision cuts (documentCutsBytes) of the same process are held
// to.
const footnoteParsesBytes = 1 << 20

// footnoteParseKey is what footnoteParses keeps an entry under: sum, the SHA-256 of its markdown
// (pmdoc.TextSum), and ranged, whether the parse it holds worked out the ranges of the markdown's
// nested blocks (footnoteParse.Nested), apart from whether it holds any: a parse made for a
// version a pin names, or for a latest version a read of an ambiguous block names, did, and a
// refusal, which holds no block to range, is kept as one that did (footnoteParseOf). The cache
// keeps one value under one key until it drops it (bytelru.Cache.PutSize), so the entry with
// ranges that such a read keeps of a markdown kept without them stands beside that entry rather
// than in its place. Every read looks for the entry with ranges first, so the one without them is
// read no more once the one with them is kept, and leaves the cache as its least recently used
// entries do.
type footnoteParseKey struct {
	sum    [32]byte
	ranged bool
}

// footnoteParses is the footnoteParse of each markdown that calledFootnotes, the decisions read
// (decisionFootnotes) or the reference graph's read of a pin's containers (readGraphPrefixes) has
// read, by its SHA-256 and by whether the parse worked out the ranges of its nested blocks
// (footnoteParseKey). What an entry holds is read from the markdown its key hashes under the block
// schema installed when it was read: whether the parser refuses the markdown, which of its nodes
// are typed blocks, and how the renderer writes its blocks back, follow that schema. The server
// can install the schema again while it runs (pmdoc/manifest.go), and an entry read under the
// earlier schema stays until it is dropped to make room for another, as the display trees' cache
// keeps its trees (pmdoc's displayCache).
var footnoteParses = bytelru.New[footnoteParseKey, []byte](footnoteParsesBytes)
