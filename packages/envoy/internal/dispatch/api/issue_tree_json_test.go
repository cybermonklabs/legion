package api

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// requireTreeReadWrittenAsWriteJSON fails unless writeIssueTreeRead answers read with the status,
// the media type and the body WriteJSON answers it with, naming the first byte the two bodies hold
// apart and the bytes around it in each.
func requireTreeReadWrittenAsWriteJSON(t *testing.T, what string, read model.IssueTreeRead) {
	t.Helper()
	want := httptest.NewRecorder()
	WriteJSON(want, http.StatusOK, read)
	got := httptest.NewRecorder()
	writeIssueTreeRead(got, &read)
	if got.Code != want.Code || got.Header().Get("Content-Type") != want.Header().Get("Content-Type") {
		t.Fatalf("%s: writeIssueTreeRead answers status %d of type %q, WriteJSON status %d of type %q",
			what, got.Code, got.Header().Get("Content-Type"), want.Code, want.Header().Get("Content-Type"))
	}
	gotBody, wantBody := got.Body.Bytes(), want.Body.Bytes()
	offset := 0
	for offset < len(gotBody) && offset < len(wantBody) && gotBody[offset] == wantBody[offset] {
		offset++
	}
	if offset < len(gotBody) || offset < len(wantBody) {
		t.Fatalf("%s: writeIssueTreeRead writes byte %d otherwise than WriteJSON: it writes %q where WriteJSON writes %q",
			what, offset, treeBytesAround(gotBody, offset), treeBytesAround(wantBody, offset))
	}
}

// treeBytesAround is the bytes of body from 10 before offset to 24 after it.
func treeBytesAround(body []byte, offset int) string {
	return string(body[max(0, offset-10):min(len(body), offset+24)])
}

// treeStringsOf is the strings an answer carries in each place a string of its stands: the issue,
// a node's text, a parent's first line, and an attribute's name and value.
func treeStringsOf(s string) model.IssueTreeRead {
	return model.IssueTreeRead{Issue: s, Nodes: []model.IssueTreeNode{{
		Text: s, Parent: &model.IssueTreeParent{FirstLine: s}, Attributes: map[string]string{s: s},
	}}}
}

// treeReadPieces are the pieces generated strings are made of: letters, a space, each character
// encoding/json escapes - the quote, the backslash, control characters, and '<', '>' and '&' for
// HTML - DEL, U+2028 and U+2029, which it escapes for JavaScript, U+FFFD written out, characters of
// two, three and four bytes, and invalid UTF-8: a lone continuation byte, a byte that starts no
// character, a cut sequence, an encoded surrogate and an overlong encoding.
var treeReadPieces = []string{
	"a", "Z", " ", "<", ">", "&", "\"", "\\", "\n", "\r", "\t", "\b", "\f", "\x00", "\x01", "\x1f", "\x7f",
	"\u2028", "\u2029", "\ufffd", "é", "€", "😀", "\u0085", "\u00a0", "/",
	"\x80", "\xff", "\xc3", "\xe2\x80", "\xed\xa0\x80", "\xc0\x80",
}

// randomTreeString is up to eleven pieces of treeReadPieces or random bytes.
func randomTreeString(random *rand.Rand) string {
	var b []byte
	for range random.IntN(12) {
		if random.IntN(4) == 0 {
			b = append(b, byte(random.IntN(256)))
			continue
		}
		b = append(b, treeReadPieces[random.IntN(len(treeReadPieces))]...)
	}
	return string(b)
}

// treeDisplayTrees are the display trees generated answers carry: compact ones, one with
// whitespace and numbers between its tokens, one holding raw and escaped U+2028, an escape, and
// invalid UTF-8, one with a name twice, and an array, a string and an empty object.
var treeDisplayTrees = []string{
	`{"type":"doc","content":[{"type":"text","text":"a <b> & c"}]}`,
	`{"type":"doc", "content" : [ 1, 2.50, -0, 1e10, true, null ]}`,
	"{\"a\":\"\\u003c\\n\\u2028\u2028\xff\"}",
	`{"a":"x","a":"y"}`,
	`[]`, `"s"`, `{}`,
}

// randomTreeRead is one answer of the tree read drawn from random: every member of the answer and
// of its nodes, each pointer and slice nil, empty or held, an actor of its kind, its id and its
// proof and one with an owner and a project, times in four zones, and each member few nodes carry -
// a document, an ask, a decision, a result and its retraction, a check, a plan, an issue's own
// members.
func randomTreeRead(random *rand.Rand) model.IssueTreeRead {
	text := func() string { return randomTreeString(random) }
	held := func() *string {
		if random.IntN(3) == 0 {
			return nil
		}
		s := text()
		return &s
	}
	zones := []*time.Location{time.UTC, time.FixedZone("east", 5*3600+30*60), time.FixedZone("west", -8*3600), time.FixedZone("zero", 0)}
	when := func() time.Time {
		return time.Unix(random.Int64N(4_000_000_000), random.Int64N(1_000_000_000)).In(zones[random.IntN(len(zones))])
	}
	read := model.IssueTreeRead{Issue: text(), View: text(), Converted: random.IntN(2) == 0, Tree: held(), ViewOf: held(), Cursor: held()}
	if random.IntN(2) == 0 {
		version := random.IntN(1000) - 3
		read.Version = &version
	}
	if random.IntN(3) == 0 {
		read.Documents = &model.IssueTreeDocuments{Spec: held(), Verification: held(), Plan: held()}
	}
	if random.IntN(4) > 0 {
		read.Nodes = []model.IssueTreeNode{}
	}
	for range random.IntN(4) {
		node := model.IssueTreeNode{Kind: text(), ID: text(), BlockID: text(), Supertag: text(), Issue: text(), ReadOnly: random.IntN(2) == 0,
			Role: text(), State: text(), Visibility: text(), Text: text(), NoConversation: random.IntN(2) == 0, Rank: held()}
		if random.IntN(2) == 0 {
			node.Parent = &model.IssueTreeParent{Kind: text(), ID: text(), Supertag: text(), FirstLine: text(), Issue: text()}
		}
		switch random.IntN(3) {
		case 1:
			node.Author = []model.Actor{}
		case 2:
			for range 1 + random.IntN(2) {
				actor := model.Actor{Kind: text(), ID: text(), Proven: random.IntN(2) == 0}
				if random.IntN(3) == 0 {
					actor.Owner, actor.Project = held(), held()
				}
				node.Author = append(node.Author, actor)
			}
		}
		if random.IntN(2) == 0 {
			made := when()
			node.MadeAt = &made
		}
		if random.IntN(3) == 0 {
			edited := when()
			node.EditedAt = &edited
		}
		if random.IntN(3) == 0 {
			deleted := when()
			node.DeletedAt = &deleted
			if random.IntN(2) == 0 {
				node.DeletedBy = &model.Actor{Kind: text(), ID: text(), Proven: random.IntN(2) == 0}
				if random.IntN(3) == 0 {
					node.DeletedBy.Owner = held()
				}
			}
		}
		if random.IntN(3) > 0 {
			node.Attributes = map[string]string{}
			for range random.IntN(6) {
				node.Attributes[text()] = text()
			}
		}
		if random.IntN(2) == 0 {
			node.TextTree = model.DisplayTree(treeDisplayTrees[random.IntN(len(treeDisplayTrees))])
		}
		if random.IntN(4) == 0 {
			node.Document = &model.IssueTreeDocument{ArtifactID: text(), Slug: text(), Position: random.IntN(9)}
		}
		if random.IntN(4) == 0 {
			node.Ask = &model.IssueTreeAsk{Options: []model.AskOption{{ID: text(), Label: text()}}, Urgency: text(), Multiple: random.IntN(2) == 0}
		}
		if random.IntN(4) == 0 {
			changed := when()
			node.Decision = &model.IssueTreeDecision{Decision: model.Decision{BlockID: text(), Text: text(), TextTree: model.DisplayTree(treeDisplayTrees[0]), Asks: []string{text()}, ChangedAt: &changed},
				Uncertainty: held(), UncertaintyReason: held()}
		}
		if random.IntN(4) == 0 {
			node.Result = &model.IssueTreeResult{Verdict: text(), CheckVersion: random.IntN(9), CheckSHA256: text()}
			if random.IntN(2) == 0 {
				node.Result.Retracted = &model.CheckResultRetracted{Reason: text(), CreatedAt: when()}
			}
		}
		if random.IntN(4) == 0 {
			node.Check = &model.IssueTreeCheck{TextSHA256: text(), ChangedVersion: random.IntN(9)}
		}
		if random.IntN(4) == 0 {
			position := random.IntN(9)
			node.Plan = &model.IssueTreePlan{Position: &position, Needs: []model.IssueTreeHalfEnd{{Kind: text(), ID: text(), Half: text()}}, Size: held(),
				Start: &model.IssueTreePlan{Needs: []model.IssueTreeHalfEnd{}}}
		}
		if random.IntN(3) == 0 {
			node.IssueTreeIssue = &model.IssueTreeIssue{Converted: random.IntN(2) == 0, Tree: held(), ViewOf: held()}
			if random.IntN(2) == 0 {
				node.IssueTreeIssue.Documents = &model.IssueTreeDocuments{Spec: held()}
			}
		}
		read.Nodes = append(read.Nodes, node)
	}
	if random.IntN(4) > 0 {
		read.Tuples = []model.IssueTreeTuple{}
	}
	for range random.IntN(4) {
		read.Tuples = append(read.Tuples, model.IssueTreeTuple{Kind: text(), From: model.IssueTreeEnd{Kind: text(), ID: text()}, To: model.IssueTreeEnd{Kind: text(), ID: text()}})
	}
	switch random.IntN(3) {
	case 1:
		read.OwedDocuments = []model.OwedDocument{}
	case 2:
		read.OwedDocuments = []model.OwedDocument{{ArtifactID: text(), Version: random.IntN(9)}}
	}
	if random.IntN(3) > 0 {
		read.DocumentNodes = []model.IssueTreeDocumentNode{}
	}
	for range random.IntN(3) {
		read.DocumentNodes = append(read.DocumentNodes, model.IssueTreeDocumentNode{ArtifactID: text(), Slug: text(), BlockCount: random.IntN(6000)})
	}
	switch random.IntN(3) {
	case 1:
		read.CommentsBefore = json.RawMessage("null")
	case 2:
		position, err := json.Marshal(text())
		if err != nil {
			panic(err)
		}
		read.CommentsBefore = position
	}
	return read
}

// The tree read writes its answer by hand rather than through encoding/json's reflection
// (writeIssueTreeRead), and every answer it writes is the status, the media type and the bytes
// WriteJSON writes for the same answer, the newline after it included. It holds over: every byte
// value alone and between two letters, in each place a string of the answer stands; every
// character of one and two bytes and every seventh one above, with the edges of each length, the
// surrogates' edges, U+2028, U+2029, U+FEFF and U+FFFD; invalid UTF-8 of each sort; 3,000
// answers drawn from a fixed seed over every member the answer and its nodes carry; and the
// answers encoding/json refuses or reformats - a time whose year has five digits, a zone of 25
// hours, a display tree that is no JSON - which writeIssueTreeRead leaves to WriteJSON.
func TestTheTreeReadWritesItsAnswerAsWriteJSONWritesIt(t *testing.T) {
	for c := range 256 {
		alone := string([]byte{byte(c)})
		requireTreeReadWrittenAsWriteJSON(t, fmt.Sprintf("byte %#02x alone", c), treeStringsOf(alone))
		requireTreeReadWrittenAsWriteJSON(t, fmt.Sprintf("byte %#02x between letters", c), treeStringsOf("x"+alone+"y"))
	}
	runes := []rune{0x7ff, 0x800, 0xd7ff, 0xe000, 0xfeff, 0xfffd, 0xffff, 0x10000, 0x10ffff, '\u2028', '\u2029'}
	for r := rune(0); r < 0x800; r++ {
		runes = append(runes, r)
	}
	for r := rune(0x800); r <= utf8.MaxRune; r += 7 {
		runes = append(runes, r)
	}
	for _, r := range runes {
		requireTreeReadWrittenAsWriteJSON(t, fmt.Sprintf("rune %U", r), model.IssueTreeRead{Issue: "a" + string(r) + "b"})
	}
	for _, invalid := range []string{"\x80", "\xbf", "\xc0\x80", "\xc1\xbf", "\xc3", "\xe2\x80", "\xe2\x80\x28", "\xed\xa0\x80", "\xed\xbf\xbf", "\xf0\x80\x80\x80", "\xf4\x90\x80\x80", "\xf5\x80\x80\x80", "\xfe", "\xff"} {
		requireTreeReadWrittenAsWriteJSON(t, fmt.Sprintf("invalid UTF-8 %q", invalid), treeStringsOf("a"+invalid+"b"))
	}
	random := rand.New(rand.NewPCG(1, 2))
	for index := range 3000 {
		requireTreeReadWrittenAsWriteJSON(t, fmt.Sprintf("generated answer %d", index), randomTreeRead(random))
	}
	fiveDigitYear := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	requireTreeReadWrittenAsWriteJSON(t, "a time whose year has five digits", model.IssueTreeRead{Nodes: []model.IssueTreeNode{{MadeAt: &fiveDigitYear}}})
	longZone := time.Date(2020, 1, 1, 0, 0, 0, 0, time.FixedZone("long", 25*3600))
	requireTreeReadWrittenAsWriteJSON(t, "a time in a zone of 25 hours", model.IssueTreeRead{Nodes: []model.IssueTreeNode{{MadeAt: &longZone}}})
	requireTreeReadWrittenAsWriteJSON(t, "a display tree that is no JSON", model.IssueTreeRead{Nodes: []model.IssueTreeNode{{TextTree: model.DisplayTree(`{"a":`)}}})
}
