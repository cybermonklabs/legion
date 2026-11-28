package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
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
// draws. labels is the footnotes that markdown calls that its document defines outside it: the
// ones the block's node calls (pmdoc.CalledFootnoteLabels), or, for a text no node of the
// document's parse holds, the ones it writes as a call (footnotesCalledIn); artifactID and
// blockID are the document that holds the block and the id it holds it under, which
// registerBlockTree logs a tree it cannot set with. Every source with labels names them. cut is
// set, where the source is made (drawnText), for a markdown that is a cut of the block's text, an
// edge's excerpt, which can stop inside a block whose end the cut leaves out (a fenced code
// block): the stand-ins read after it then stand inside that block, so a tree that does not end
// in them is no fault of the read, and registerBlockTree sets it nowhere and logs nothing, for
// the caller to read the cut alone.
type blockTreeSource struct {
	artifactID, blockID string
	labels              []string
	cut                 bool
}

// registerBlockTree registers with trees, as field, the display tree of markdown, the content of
// the block source names: the one its artifactID's document holds under its blockID. Every read
// that answers a block's tree registers it here: the coverage read's requirements, the results
// read's checks, the batched reference read's blocks, the decisions read's decisions and the
// reference graph read's texts of documents' blocks, so one block answers one tree in each read
// that draws it whole, and each of them reads a call as the document does. The markdown is cut
// out of the document, so it holds no definition the document writes outside the block, and read
// alone it reads a call of such a label as text, where the document reads, and the browser editor
// holds, a reference. So the markdown of a block with labels is registered with a stand-in
// definition of each label after it (pmdoc.WithStandInDefinitions), and the stand-ins, those the
// markdown refers to and those it does not, are taken out of the tree before it is set
// (withoutStandInDefinitions); a block with none registers its markdown as it stands. A tree that
// does not end in the stand-ins is set nowhere, since no other field of the read depends on it,
// and is logged unless source is a cut, which can end that way (blockTreeSource).
func registerBlockTree(trees *displayTreeReader, field *model.DisplayTree, markdown string, source blockTreeSource) {
	if len(source.labels) == 0 || markdown == "" {
		trees.text(field, markdown)
		return
	}
	trees.textInto(func(tree json.RawMessage) {
		shown, ok := withoutStandInDefinitions(tree, len(source.labels))
		if !ok {
			if !source.cut {
				slog.Warn("dispatch: take the stand-in footnote definitions out of a block's tree",
					"artifact", source.artifactID, "block", source.blockID, "labels", source.labels)
			}
			return
		}
		*field = model.DisplayTree(shown)
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
		shown := make(json.RawMessage, 0, int64(len(tree))-(closing-cut))
		return append(append(shown, tree[:cut]...), tree[closing:]...), true
	}
	return nil, false
}

// footnoteDocumentsMax is how many documents one request of the batched reference read
// (blockTrees) or of the reference graph's read (readGraphFootnotes) reads the footnote
// definitions of (calledFootnotes). The new issue page reads four documents of an issue - its
// spec, its plan, its Verification document and its decisions document - so 16 holds the issue's
// own and those of three issues whose blocks it names. A parse of a document of 25 to 43 KB takes
// 8.5 to 13.4 ms (decisionCuts), so 16 parses that footnoteParses does not hold take about 215 ms
// at that size, where the 256 documents that one batched read can name
// (contracts.ReferenceTargetsMax) would take about 3.4 s.
const footnoteDocumentsMax = 16

// footnoteDocuments is what one request has spent of footnoteDocumentsMax over all of its calls of
// calledFootnotes: read, how many documents those calls have read the definitions of, a document
// whose read failed included, and unread, how many they left unread once read reached the bound.
type footnoteDocuments struct {
	read, unread int
}

// warnUnread logs one line for a request whose calls of calledFootnotes left a document unread,
// naming route, the read the request is of, and the count of documents it did not read; a request
// that read every document it named logs nothing. It is called once, after the request's last
// call of calledFootnotes.
func (d *footnoteDocuments) warnUnread(route string) {
	if d.unread == 0 {
		return
	}
	slog.Warn("dispatch: leave the footnote definitions of documents past one request's bound unread for their blocks' trees",
		"read", route, "unread_documents", d.unread, "footnote_documents_max", footnoteDocumentsMax)
}

// calledFootnotes is, by key, the source of the tree of each document block's text of texts
// (blockTreeSource): its document's id, its own block id, the footnotes it calls that its
// document defines outside it and whether it is a cut, for the texts that hold a call at all
// (`[^`). Such a text is a slice of its document, so it holds no definition the document writes
// outside the block, and read alone it reads a call of such a label as text. A key names a text,
// not a block: the batched reference read keys each block's stored text by its target
// (blockTrees), and the reference graph's read draws one block from its whole text and from an
// edge's excerpt of it (readGraphFootnotes), which can call different labels.
//
// keys is the keys of texts in the order in which the request first names each text, and the
// documents are taken in the order in which keys first names a text of each that holds a call; a
// document none of whose texts here holds one is not read and takes no place. budget is what the
// request has spent of footnoteDocumentsMax over all of its calls: while the bound has room, each
// document spends one place and is read, a document whose read fails included, since it was
// loaded; once the bound is spent, a document is not read and is counted unread in budget, and
// its texts have no entry, so they keep the tree of their stored text alone, as a text whose
// document cannot be read does. The request's order alone decides which documents are read, never
// what footnoteParses holds, so the same request answers the same trees. The caller logs the
// count of unread documents once the request's last call is done (footnoteDocuments.warnUnread).
//
// version is the version of their documents the texts were cut from, 0 for the latest. Each
// document is read once, however many of its blocks' texts texts holds. Its id, and the id it
// holds each block under, are the ones the text's entry holds (drawnText): the batched reference
// read takes them from refs.BlockTexts, which reads them from the block index (document_blocks)
// with the block's text, and the reference graph's read from the node it draws the text for
// (graphDocumentBlock). Its latest markdown is read through TextWithToken: the block index cut
// each stored text from the markdown the document's latest version stored, and TextWithToken
// answers that markdown itself, without rendering the document, where that version is still the
// document's state and a room of this process holds it, and from the read cache by the
// document's stamp where no room holds it; Text renders a room's tree on every read. Any other
// version's markdown is the one that version stored (documentMarkdownAt). The labels are read
// from the markdown's parse as a rendering, given the map of the document's texts that hold a
// call (footnotesCalledIn), whose answer is merged into this one, and what they are read from is
// kept by the markdown's sum (footnoteParses), so no read parses a markdown again while its entry
// stays kept. A text whose document cannot be read or parsed has no entry and keeps the tree of
// its stored text alone; the failure is logged.
func calledFootnotes[K comparable](ctx context.Context, s *server, keys []K, texts map[K]drawnText, version int, budget *footnoteDocuments) map[K]blockTreeSource {
	// order is the ids of the documents to read, in the order in which keys first names a text
	// of each that holds a call, and wanted is each one's texts that hold one, by key.
	var order []string
	wanted := map[string]map[K]drawnText{}
	for _, key := range keys {
		text, held := texts[key]
		if !held || !strings.Contains(text.text, "[^") {
			continue
		}
		artifactID := text.block.ArtifactID
		if _, taken := wanted[artifactID]; !taken {
			order = append(order, artifactID)
			wanted[artifactID] = map[K]drawnText{}
		}
		wanted[artifactID][key] = text
	}
	if len(order) == 0 {
		return nil
	}
	called := map[K]blockTreeSource{}
	for _, artifactID := range order {
		if budget.read == footnoteDocumentsMax {
			budget.unread++
			continue
		}
		budget.read++
		markdown, err := s.documentMarkdownAt(ctx, artifactID, version)
		if err != nil {
			slog.Warn("dispatch: read a document's footnote definitions for its blocks' trees", "artifact", artifactID, "error", err)
			continue
		}
		maps.Copy(called, footnotesCalledIn(markdown, artifactID, wanted[artifactID]))
	}
	return called
}

// documentMarkdownAt is the markdown of version of the document artifactID that calledFootnotes
// reads the document's footnote definitions from: the document as it stands for version 0
// (TextWithToken), and for any other the markdown that version stored (versionsMarkdown), which
// the block index cut that version's texts of its blocks from. A version the document does not
// hold is an error.
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
// section of decisions.md, or a text of a document's block the reference graph's read draws; and
// cut, whether text is a cut of the block's text, an edge's excerpt, which the text's source then
// carries (blockTreeSource).
type drawnText struct {
	block refs.DocumentBlock
	text  string
	cut   bool
}

// footnotesCalledIn is, by key, the source of the tree of each text of texts (blockTreeSource),
// where the block of the document artifactID that the text's entry places it at (drawnText) is
// read from markdown, the markdown the text was cut from, as its parse as a rendering reads it
// (footnoteParseOf). A typed block is the first node of the parse holding its id, as typedBlocksIn
// finds one, since the rendering writes a typed block's id into its opening line, and its labels
// are the ones its node calls (pmdoc.CalledFootnoteLabels), as the coverage and the results reads
// take them. Any other block - a paragraph, a heading, a list item - has no id in the rendering,
// so the parse gives it another, and so has a block its document no longer holds: no node holds
// its id, and its labels are each label the document defines that its text writes as a call, up
// to case (pmdoc.FootnoteDefinitionLabels, pmdoc.FootnoteLabelKeys.Written), read against the keys
// of those labels made at most once for the call, on the first text that needs them
// (pmdoc.KeyFootnoteLabels), so no text's labels depend on the order texts is walked in. Such a
// text can write a call that reads as text, escaped or in code, or one of a label it defines
// itself, which resolves to its own definition first; the stand-in of that label is then one
// nothing refers to, which is taken out of the tree with the rest (registerBlockTree). Each source
// is a cut where its text is. It answers the map it makes: a text that calls no such label has no
// entry in it, and a markdown the parser refuses answers none.
func footnotesCalledIn[K comparable](markdown, artifactID string, texts map[K]drawnText) map[K]blockTreeSource {
	parse := footnoteParseOf(markdown, artifactID)
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
			called[key] = blockTreeSource{artifactID: artifactID, blockID: text.block.BlockID, labels: labels, cut: text.cut}
		}
	}
	return called
}

// footnoteParse is what footnotesCalledIn reads of the parse of one markdown as a rendering: Read,
// whether the parser read the markdown at all; Called, by the id of each typed block of the parse,
// the labels the first node holding that id calls (pmdoc.CalledFootnoteLabels), a block that calls
// none held with no labels; and Defined, the labels the document defines
// (pmdoc.FootnoteDefinitionLabels). Only a typed block's markdown writes its id, and every other
// block's id is one the parse mints, which names no block a text is cut from and would make the
// entry depend on more than the markdown, so Called holds the typed blocks alone.
type footnoteParse struct {
	Read    bool                `json:"read"`
	Called  map[string][]string `json:"called,omitempty"`
	Defined []string            `json:"defined,omitempty"`
}

// footnoteParseOf is the footnoteParse of markdown, a markdown of the document artifactID: the one
// footnoteParses keeps for it, else its parse as a rendering, which is then kept, a refusal
// included, so a markdown is parsed at most once while its entry stays kept. A refusal is logged,
// with artifactID, by the parse that meets it. Two reads that miss one markdown at once both parse
// it, and the entry the first keeps stands.
func footnoteParseOf(markdown, artifactID string) footnoteParse {
	sum := sha256.Sum256([]byte(markdown))
	if held, ok := footnoteParses.Get(sum); ok {
		var parse footnoteParse
		if json.Unmarshal(held, &parse) == nil {
			return parse
		}
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
	}
	if encoded, err := json.Marshal(parse); err == nil {
		footnoteParses.PutSize(sum, encoded, int64(len(encoded))+footnoteParseEntryBytes)
	}
	return parse
}

// footnoteParseEntryBytes is what one entry of footnoteParses is charged beside its encoding: its
// sum, and the map entry, list element and cache entry that hold it, rounded up.
const footnoteParseEntryBytes = 128

// footnoteParsesBytes bounds the entries footnoteParses keeps: 1 MiB. An entry holds labels and
// ids, not a tree or a markdown: about 45 bytes for each typed block of its document, the block's
// id (a UUID) with the labels it calls, beside the labels the document defines and the charge for
// the entry itself (footnoteParseEntryBytes). So the bound holds the entries of more than 500
// documents of 40 typed blocks each, and of thousands with few, each of which saves a parse of
// 8.5 to 13.4 ms for a document of 25 to 43 KB (decisionCuts). It is a sliver of the bounds the
// display trees (pmdoc.DisplayCacheBytes) and the decision cuts (documentCutsBytes) of the same
// process are held to.
const footnoteParsesBytes = 1 << 20

// footnoteParses is the footnoteParse of each markdown footnotesCalledIn has read, by its SHA-256.
// What an entry holds is read from the markdown its key hashes under the block schema installed
// when it was read: whether the parser refuses the markdown, and which of its nodes are typed
// blocks, follow that schema. The server can install the schema again while it runs
// (pmdoc/manifest.go), and an entry read under the earlier schema stays until it is dropped to
// make room for another, as the display trees' cache keeps its trees (pmdoc's displayCache).
var footnoteParses = bytelru.New[[32]byte, []byte](footnoteParsesBytes)
