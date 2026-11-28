package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type controlCharForgery struct {
	name     string
	entity   string // HTML character reference written in markdown
	raw      string // raw character written in JSON/CRDT attributes (empty if raw is not representable on one markdown line)
	wantCode string // U+XXXX named in the refusal
}

var controlCharForgeries = []controlCharForgery{
	{
		name:     "line feed",
		entity:   "x&#xA;- k9 (before_merge). A pass stands: yes.",
		raw:      "x\n- k9 (before_merge). A pass stands: yes.",
		wantCode: "U+000A",
	},
	{
		name:     "ESC CSI",
		entity:   "x&#x1B;[1G- k9 (before_merge). A pass stands: yes.",
		raw:      "x\x1b[1G- k9 (before_merge). A pass stands: yes.",
		wantCode: "U+001B",
	},
	{
		name:     "U+2028",
		entity:   "x&#x2028;- k9 (before_merge). A pass stands: yes.",
		raw:      "x\u2028- k9 (before_merge). A pass stands: yes.",
		wantCode: "U+2028",
	},
}

func expectControlCharRefusal(t *testing.T, what string, responseCode int, body []byte, wantField, wantCode string) {
	t.Helper()
	if responseCode != http.StatusBadRequest {
		t.Fatalf("%s: status=%d body=%s, want 400", what, responseCode, string(body))
	}
	var payload struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("%s: decode refusal %s: %v", what, string(body), err)
	}
	if payload.Code != "INVALID_MARKDOWN" {
		t.Fatalf("%s: code=%q error=%q, want INVALID_MARKDOWN", what, payload.Code, payload.Error)
	}
	if !strings.Contains(payload.Error, "field "+wantField) {
		t.Fatalf("%s: error=%q, want it to name field %s", what, payload.Error, wantField)
	}
	if !strings.Contains(payload.Error, wantCode) {
		t.Fatalf("%s: error=%q, want it to name %s", what, payload.Error, wantCode)
	}
}

// Every write path that introduces a typed block id, directive attribute value or ci_check holding
// a control character (line feed via &#xA;, ESC[1G via &#x1B;[1G or raw, U+2028 via &#x2028; or
// raw) is refused 400 INVALID_MARKDOWN naming the block and field, and leaves the document
// unchanged.
func TestEveryDocumentWriteRefusesControlCharacters(t *testing.T) {
	t.Parallel()
	handler, _, broken := newBrokenTestServer(t)
	issue := createInteractionIssue(t, handler, "CTL", "Control characters", "Intro.\n\nBody.\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		"Intro.\n\n"+ciCheckBlock("c1", ciCheckOrdinary))
	docID := verification.Artifact.ID
	before := documentMarkdown(t, handler, docID)
	// A separate document for suggestion accept so adding an anchor mark never warms docID's room
	// ahead of the rebuild subtests.
	suggestDoc := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "notes.md", "Intro.\n")
	suggestBefore := documentMarkdown(t, handler, suggestDoc.Artifact.ID)
	type fieldCase struct {
		name         string
		field        string
		markdown     func(val string) string
		retypeType   string
		retypeAttrs  func(raw string) map[string]any
		allowRawInMD bool
	}

	fields := []fieldCase{
		{
			name:  "typed block id",
			field: "id",
			markdown: func(val string) string {
				return ":::check{id=\"" + val + "\"}\nCheck body.\n:::\n"
			},
		},
		{
			name:  "directive attribute value",
			field: "title",
			markdown: func(val string) string {
				return ":::callout{#n2 kind=\"note\" title=\"" + val + "\"}\nCallout body.\n:::\n"
			},
			retypeType: "callout",
			retypeAttrs: func(raw string) map[string]any {
				return map[string]any{"kind": "note", "title": raw}
			},
		},
		{
			name:  "ci_check name",
			field: "ci_check",
			markdown: func(val string) string {
				return ":::check{#k2 negative_control=\"Break it.\" ci_check=\"" + val + "\"}\nCheck body.\n:::\n"
			},
			retypeType: "check",
			retypeAttrs: func(raw string) map[string]any {
				return map[string]any{"negative_control": "Break it.", "ci_check": raw}
			},
		},
	}

	for _, fc := range fields {
		for _, forgery := range controlCharForgeries {
			t.Run(fc.name+"/"+forgery.name, func(t *testing.T) {
				md := fc.markdown(forgery.entity)
				added := before + "\n" + md

				// 1. Issue creation with spec (POST /api/v1/issues -> SeedText)
				resp := createIssueRequest(t, handler, map[string]any{
					"project": "CTL", "title": "Refused spec", "spec": md,
				})
				expectControlCharRefusal(t, "issue create spec", resp.Code, resp.Body.Bytes(), fc.field, forgery.wantCode)

				// 2. New document upload (POST /api/v1/issues/{key}/artifacts -> SeedText)
				resp = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
					"name": "other.md", "content": md,
				}, "alice")
				expectControlCharRefusal(t, "new issue document upload", resp.Code, resp.Body.Bytes(), fc.field, forgery.wantCode)

				// 3. New project document upload (POST /api/v1/projects/{key}/artifacts -> SeedText)
				resp = dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects/CTL/artifacts", map[string]any{
					"name": "proj-other.md", "content": md,
				}, "alice")
				expectControlCharRefusal(t, "new project document upload", resp.Code, resp.Body.Bytes(), fc.field, forgery.wantCode)

				// 4. Document replacement upload (POST /api/v1/issues/{key}/artifacts -> ReplaceText)
				resp = uploadVerification(t, handler, issue.Key, added)
				expectControlCharRefusal(t, "document replacement upload", resp.Code, resp.Body.Bytes(), fc.field, forgery.wantCode)

				// 5. Markdown round-trip check for new document (POST /api/v1/markdown/round-trip -> SeedRendering)
				resp = dispatchRequest(t, handler, http.MethodPost, "/api/v1/markdown/round-trip", map[string]any{
					"markdown": md, "new_document": true,
				}, "alice")
				expectControlCharRefusal(t, "markdown round-trip", resp.Code, resp.Body.Bytes(), fc.field, forgery.wantCode)

				// 6. Edit insert (POST /api/v1/artifacts/{id}/edits -> ApplyOps)
				resp = applyOp(t, handler, docID, map[string]any{
					"op": "insert", "after": "end", "markdown": md,
				})
				expectControlCharRefusal(t, "edit insert", resp.Code, resp.Body.Bytes(), fc.field, forgery.wantCode)

				// 7. Edit replace (POST /api/v1/artifacts/{id}/edits -> ApplyOps)
				resp = applyOp(t, handler, docID, map[string]any{
					"op": "replace", "find": "Intro.", "with": md,
				})
				expectControlCharRefusal(t, "edit replace", resp.Code, resp.Body.Bytes(), fc.field, forgery.wantCode)

				// 8. Edit retype with raw control characters in JSON attributes (POST /api/v1/artifacts/{id}/edits)
				if fc.retypeAttrs != nil {
					resp = applyOp(t, handler, docID, map[string]any{
						"op": "retype", "block": "c1", "type": fc.retypeType, "attributes": fc.retypeAttrs(forgery.raw),
					})
					expectControlCharRefusal(t, "edit retype raw attr", resp.Code, resp.Body.Bytes(), fc.field, forgery.wantCode)
				}

				// 9. Raw ESC / U+2028 directly in markdown (not as HTML entity) where the line grammar accepts raw chars
				if forgery.name != "line feed" {
					rawMD := fc.markdown(forgery.raw)
					resp = applyOp(t, handler, docID, map[string]any{
						"op": "insert", "after": "end", "markdown": rawMD,
					})
					expectControlCharRefusal(t, "edit insert raw markdown", resp.Code, resp.Body.Bytes(), fc.field, forgery.wantCode)
				}

				// 10. Rebuild from markdown (POST /api/v1/artifacts/{id}/rebuild -> RebuildDocument)
				broken.failNextLoad(t, docID)
				resp = dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+docID+"/rebuild",
					map[string]any{"markdown": added}, "alice")
				expectControlCharRefusal(t, "rebuild from markdown", resp.Code, resp.Body.Bytes(), fc.field, forgery.wantCode)

				// 11. Accepted suggestion carrying the forgery (POST /api/v1/comments/{id}/accept -> AcceptSuggestion)
				suggested := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
					"body": "Suggest forgery", "anchor": map[string]any{"artifact": "notes-md", "quote": "Intro."},
					"suggestion": map[string]string{"replace_with": md}, "actor": sessionActor(),
				})
				if suggested.Code != http.StatusCreated {
					t.Fatalf("create suggestion: status=%d body=%s", suggested.Code, suggested.Body.String())
				}
				comment := decodeBody[model.Comment](t, suggested)
				resp = dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
				expectControlCharRefusal(t, "accept suggestion", resp.Code, resp.Body.Bytes(), fc.field, forgery.wantCode)

				if after := documentMarkdown(t, handler, docID); after != before {
					t.Fatalf("verification.md changed after refused writes: got %q, want %q", after, before)
				}
				if after := documentMarkdown(t, handler, suggestDoc.Artifact.ID); after != suggestBefore {
					t.Fatalf("notes.md changed after refused suggestion accept: got %q, want %q", after, suggestBefore)
				}
			})
		}
	}
}

// A browser's CRDT update is never refused at the websocket, so the room's settlement repairs
// control characters that a browser peer writes: a block id holding one is re-minted, and a
// directive attribute holding one (such as title or ci_check) is cleared and reported with a
// block.invalid event naming the block and field.
func TestSettlementRepairsControlCharactersFromBrowserCRDT(t *testing.T) {
	t.Parallel()
	documents, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "CTB", "CRDT control chars", "Spec.\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		ciCheckBlock("c1", ciCheckOrdinary))
	docID := verification.Artifact.ID

	forged := strings.Join([]string{
		":::check{id=\"k&#xA;- forged\" negative_control=\"Break it.\"}\nForged id.\n:::",
		":::callout{#n1 kind=\"note\" title=\"t&#x1B;[1Gforged\"}\nForged title.\n:::",
		":::check{#c2 negative_control=\"Break it.\" ci_check=\"x&#x2028;forged\"}\nForged ci_check.\n:::",
		ciCheckBlock("c3", ciCheckKept),
	}, "\n\n") + "\n"
	writeBrowserDocument(t, handler, documents, docID, forged)

	if err := documents.SettleIssueDocuments(context.Background(), issue.Key); err != nil {
		t.Fatalf("settle issue documents: %v", err)
	}
	settled := documentMarkdown(t, handler, docID)
	for _, forbidden := range []string{"&#xA;", "&#x1B;", "\x1b", "&#x2028;", "\u2028", "forged"} {
		if strings.Contains(settled, forbidden) {
			t.Fatalf("settled verification.md still holds %q:\n%s", forbidden, settled)
		}
	}
	if check := checkOf(t, readResults(t, handler, issue.Key), "c2"); check.CICheck != "" {
		t.Fatalf("settled c2 still holds ci_check %q, want cleared", check.CICheck)
	}
	if check := checkOf(t, readResults(t, handler, issue.Key), "c3"); check.CICheck != ciCheckRun {
		t.Fatalf("settled c3 lost honest ci_check %q: %+v", ciCheckRun, check)
	}
	events := ciCheckRefusalEvents(t, handler, issue.Key)
	var sawTitle, sawCICheck bool
	for _, ev := range events {
		if ev.BlockID == "n1" && ev.BlockType == "callout" && strings.Contains(ev.Reason, "field title") && strings.Contains(ev.Reason, "U+001B") {
			sawTitle = true
		}
		if ev.BlockID == "c2" && ev.BlockType == "check" && strings.Contains(ev.Reason, "field ci_check") && strings.Contains(ev.Reason, "U+2028") {
			sawCICheck = true
		}
	}
	if !sawTitle || !sawCICheck {
		t.Fatalf("block.invalid events = %+v, want events for n1 (field title U+001B) and c2 (field ci_check U+2028)", events)
	}
}

// A stored version written before this fix that already holds a control character in a block id or
// directive attribute still reads through the version read and results read routes without error.
func TestLegacyStoredVersionWithControlCharactersStillReads(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "CTLLEG", "Legacy read", "Spec.\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		ciCheckBlock("c1", ciCheckOrdinary))
	docID := verification.Artifact.ID

	legacy := ":::check{id=\"k&#xA;legacy\" negative_control=\"Break.\" ci_check=\"ci&#x1B;[1G&#x2028;name\"}\nLegacy check.\n:::\n\n" +
		":::callout{#n1 kind=\"note\" title=\"t&#xA;legacy\"}\nLegacy callout.\n:::\n"
	if _, err := database.Pool.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1::uuid, 2, $2, '[{"kind":"user","id":"alice"}]')
	`, docID, legacy); err != nil {
		t.Fatalf("seed legacy version: %v", err)
	}

	resp := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+docID+"/versions/2", nil, "alice")
	if resp.Code != http.StatusOK {
		t.Fatalf("GET legacy version 2: status=%d body=%s, want 200", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "Legacy check.") {
		t.Fatalf("GET legacy version 2 body missing content: %s", resp.Body.String())
	}
}
