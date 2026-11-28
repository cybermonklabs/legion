package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// mintedBlockID is a block id the store mints, a uuid after the block's #.
var mintedBlockID = regexp.MustCompile(`#[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// TestTheV2RoundTripsRefuseEachTextAsTheirV1RoutesDo holds POST /api/v2/markdown-round-trips and
// POST /api/v2/markdown-file-round-trips, the successors of the two /api/v1 round trips, to what
// those answer for every text of the refusal corpus (roundTripRefusals): the same status, the same
// words, the code /api/v2 names the /api/v1 code by (413 CAP_EXCEEDED is REQUEST_TOO_LARGE there),
// and the mark of a refusal of the text (contracts.TextRefusalHeader), asked for a new document's
// refusals and without them. A text holding U+0000 is among them: the v2 decoder leaves its refusal
// to the handler (requestBody.TextChecked), which makes it in the upload's words, marked, as the
// /api/v1 route does, so a client that moves to /api/v2 loses no refusal and no mark. A text the
// store takes is answered the markdown the /api/v1 route answers, a minted block id aside.
func TestTheV2RoundTripsRefuseEachTextAsTheirV1RoutesDo(t *testing.T) {
	handler := newTestHandler(t)
	v2Code := map[string]string{}
	for _, code := range apicode.All() {
		if code.InV1() && code.InV2() {
			v2Code[fmt.Sprintf("%d %s", code.V1Status(), code.V1Name())] = code.Name()
		}
	}
	roundTripV2 := func(kind, text string, newDocument bool) *httptest.ResponseRecorder {
		t.Helper()
		pattern, body := "/api/v2/markdown-round-trips", map[string]any{"markdown": text}
		if kind == "file" {
			pattern, body = "/api/v2/markdown-file-round-trips", map[string]any{"file": []byte(text)}
		}
		if newDocument {
			body["new_document"] = true
		}
		return dispatchRequest(t, handler, http.MethodPost, pattern, body, "alice")
	}
	for _, test := range roundTripRefusals(t) {
		t.Run(test.name, func(t *testing.T) {
			for _, kind := range test.sent {
				for _, newDocument := range []bool{true, false} {
					v1 := roundTripOf(t, handler, kind, test.text, newDocument)
					v2 := roundTripV2(kind, test.text, newDocument)
					if v1.Code == http.StatusOK {
						// A replacement mints a fresh id for the second block of an id named twice, so
						// each answer's minted ids are read as one placeholder.
						minted := func(answered *httptest.ResponseRecorder) string {
							return mintedBlockID.ReplaceAllString(decodeBody[markdownRoundTripResponse](t, answered).Markdown, "#minted")
						}
						if v2.Code != http.StatusOK || minted(v2) != minted(v1) {
							t.Fatalf("the v2 round trip of the %s, new_document %v: status=%d body=%.300s, want /api/v1's 200 %.300s",
								kind, newDocument, v2.Code, v2.Body.String(), v1.Body.String())
						}
						continue
					}
					v1Code, v1Message := refusalOf(t, v1)
					code, message := refusalOf(t, v2)
					want, known := v2Code[fmt.Sprintf("%d %s", v1.Code, v1Code)]
					if !known || v2.Code != v1.Code || code != want || message != v1Message {
						t.Fatalf("the v2 round trip of the %s, new_document %v: %d %s %.200q, want /api/v1's %d %s %.200q as /api/v2 names it",
							kind, newDocument, v2.Code, code, message, v1.Code, v1Code, v1Message)
					}
					if mark, v1Mark := v2.Header().Get(contracts.TextRefusalHeader), v1.Header().Get(contracts.TextRefusalHeader); mark != v1Mark {
						t.Fatalf("the v2 round trip's refusal of the %s, new_document %v, carries %s %q, want /api/v1's %q",
							kind, newDocument, contracts.TextRefusalHeader, mark, v1Mark)
					}
				}
			}
		})
	}
}

// TestEachRoundTripIgnoresAU0000InAMemberItDoesNotDeclare sends each of the four round trips, the
// inline and the file one of /api/v1 and of /api/v2, a text the store takes beside a member the
// body does not declare that holds U+0000. Each reads its body without the JSON routes' refusal of
// U+0000 (readJSONBodyAllowingNUL, requestBody.TextChecked) and ignores the member, so each answers
// 200 with the same markdown, and a client that moves to /api/v2 is answered as /api/v1 answers it.
func TestEachRoundTripIgnoresAU0000InAMemberItDoesNotDeclare(t *testing.T) {
	handler := newTestHandler(t)
	const text = "# Plan\n\nShip the **parser**.\n"
	var first string
	for _, route := range []struct {
		pattern, member string
		text            any
	}{
		{markdownRoundTripPattern, "markdown", text},
		{"/api/v2/markdown-round-trips", "markdown", text},
		{fileRoundTripPattern, "file", []byte(text)},
		{"/api/v2/markdown-file-round-trips", "file", []byte(text)},
	} {
		answered := dispatchRequest(t, handler, http.MethodPost, route.pattern,
			map[string]any{route.member: route.text, "note": "a\x00b"}, "alice")
		if answered.Code != http.StatusOK {
			t.Fatalf("POST %s with a U+0000 in an undeclared member answered %d %s, want 200",
				route.pattern, answered.Code, answered.Body.String())
		}
		markdown := decodeBody[markdownRoundTripResponse](t, answered).Markdown
		if first == "" {
			if markdown == "" {
				t.Fatalf("POST %s answered the empty markdown for %q", route.pattern, text)
			}
			first = markdown
		}
		if markdown != first {
			t.Errorf("POST %s answered the markdown %q, want %q, the answer of POST %s", route.pattern, markdown, first, markdownRoundTripPattern)
		}
	}
}
