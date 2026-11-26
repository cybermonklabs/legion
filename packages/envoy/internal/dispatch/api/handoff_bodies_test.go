package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// wireHandoffBodies is GET /api/v1/issues/{key}/handoffs/bodies's answer as a client decodes it.
type wireHandoffBodies struct {
	Bodies []wireHandoffBody `json:"bodies"`
	Left   []string          `json:"left"`
}

// wireHandoffBody is one entry of the answer, its trees kept as the bytes they were sent as.
type wireHandoffBody struct {
	ID         string                     `json:"id"`
	Body       *string                    `json:"body"`
	FieldTrees map[string]json.RawMessage `json:"field_trees"`
}

// sentSize is how many bytes the entry's body and its trees take in the answer: the body as a
// JSON string as Go's encoder writes it, HTML escaping included, and the trees where the entry
// carries them. It is the size the route counts against contracts.HandoffBodiesMaxBytes.
func (entry wireHandoffBody) sentSize(t *testing.T) int {
	t.Helper()
	size := len(encodedForTest(t, entry.Body))
	if entry.FieldTrees != nil {
		size += len(encodedForTest(t, entry.FieldTrees))
	}
	return size
}

// encodedForTest is value as json.Marshal writes it, failing the test where it cannot.
func encodedForTest(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %T: %v", value, err)
	}
	return encoded
}

// decodeHandoffBodies requires a 200 answer that holds both members, `left` as a list even when
// it is empty, and decodes it.
func decodeHandoffBodies(t *testing.T, response *httptest.ResponseRecorder) wireHandoffBodies {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%.300s, want 200", response.Code, response.Body.String())
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &members); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if len(members) != 2 || members["bodies"] == nil || members["left"] == nil || string(members["left"]) == "null" {
		t.Fatalf("the answer's members are %v, want exactly bodies and left, each a list", slices.Sorted(maps.Keys(members)))
	}
	return decodeBody[wireHandoffBodies](t, response)
}

// requireBodies fails unless the answer holds an entry for each of want's ids, in that order,
// each with the body want gives it (nil for null), and leaves exactly left, in that order.
func requireBodies(t *testing.T, answer wireHandoffBodies, want []wireHandoffBody, left []string) {
	t.Helper()
	got := make([]string, len(answer.Bodies))
	for index, entry := range answer.Bodies {
		got[index] = entry.ID
	}
	wanted := make([]string, len(want))
	for index, entry := range want {
		wanted[index] = entry.ID
	}
	if !slices.Equal(got, wanted) {
		t.Fatalf("the answer holds the ids %v, want %v", got, wanted)
	}
	for index, entry := range want {
		gotBody := answer.Bodies[index].Body
		switch {
		case entry.Body == nil && gotBody != nil:
			t.Errorf("%s is answered %.80q, want null", entry.ID, *gotBody)
		case entry.Body != nil && gotBody == nil:
			t.Errorf("%s is answered null, want its body", entry.ID)
		case entry.Body != nil && *gotBody != *entry.Body:
			t.Errorf("%s is answered %.80q, want the body as stored %.80q", entry.ID, *gotBody, *entry.Body)
		}
	}
	if !slices.Equal(answer.Left, left) {
		t.Fatalf("the answer leaves %v, want %v", answer.Left, left)
	}
}

// handoffBodySentAs is a handoff body, one JSON object with a `label` and a `text` of filler, that
// the answer sends as exactly size bytes: len(json.Marshal(body)) is size. label tells bodies of
// one size apart, and every label of one length gives a body of one size.
func handoffBodySentAs(t *testing.T, label string, size int) string {
	t.Helper()
	shell := func(filler int) string {
		return fmt.Sprintf(`{"label":%q,"text":"%s"}`, label, strings.Repeat("x", filler))
	}
	body := shell(size - len(encodedForTest(t, shell(0))))
	if sent := len(encodedForTest(t, body)); sent != size {
		t.Fatalf("the body is sent as %d bytes, want %d", sent, size)
	}
	return body
}

// denseMarkdownHandoff is a handoff body of exactly contracts.DispatchHandoffBodyMax bytes, one
// JSON object with one string field, `text`, of markdown as dense as a handoff holds it: list
// items, each a link and a code span. Its tree is many times the size of its text, which is the
// worst single entry for the route, since it builds the trees to learn whether they pass the
// limit.
func denseMarkdownHandoff(t *testing.T) (body, markdown string) {
	t.Helper()
	// The body's size is its shell's and each line's as a JSON string, the quotes left out.
	size := len(`{"text":""}`)
	var text strings.Builder
	for line := 0; ; line++ {
		item := fmt.Sprintf("- [step %d](https://example.com/runs/%d) ran `go test ./step%d/...`\n", line, line, line)
		itemSize := len(encodedForTest(t, item)) - len(`""`)
		if size+itemSize > contracts.DispatchHandoffBodyMax {
			break
		}
		text.WriteString(item)
		size += itemSize
	}
	text.WriteString(strings.Repeat("x", contracts.DispatchHandoffBodyMax-size))
	markdown = text.String()
	body = string(encodedForTest(t, map[string]string{"text": markdown}))
	if len(body) != contracts.DispatchHandoffBodyMax {
		t.Fatalf("the dense handoff is %d bytes, want %d", len(body), contracts.DispatchHandoffBodyMax)
	}
	return body, markdown
}

// TestAnIssuesHandoffBodiesRead is b-c-bodies (dispatch://INFRA-4870's Verification document): GET
// /api/v1/issues/{key}/handoffs/bodies answers the body of each handoff of the issue it is asked
// for, in the order asked, up to contracts.HandoffBodiesMaxBytes of bodies and their trees as the
// answer sends them, always the first id asked, and names each id it left (the plan's D4 and D5).
func TestAnIssuesHandoffBodiesRead(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "BODY", "Handoff bodies", "spec")
	other := createInteractionIssue(t, handler, "ELSE", "Another issue's handoffs", "spec")
	route := "/api/v1/issues/" + issue.Key + "/handoffs/bodies"
	asking := func(ids ...string) string { return route + "?ids=" + strings.Join(ids, ",") }
	read := func(ids ...string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodGet, asking(ids...), nil, "alice")
	}
	stored := func(id, body string) wireHandoffBody { return wireHandoffBody{ID: id, Body: &body} }

	const plannedBody, reviewedBody = `{"step":"plan"}`, `{ "verdict" : "approve" ,"note":"spaced as written"}`
	planned := recordedHandoff(t, handler, issue.Key, "plan", plannedBody)
	exact := fortyKilobyteHandoff()
	implemented := recordedHandoff(t, handler, issue.Key, "implement", exact)
	reviewed := recordedHandoff(t, handler, issue.Key, "review", reviewedBody)

	const largeSize = 250_000
	large := make([]wireHandoffBody, 5)
	largeIDs := make([]string, len(large))
	for index := range large {
		body := handoffBodySentAs(t, strconv.Itoa(index), largeSize)
		large[index] = stored(recordedHandoff(t, handler, issue.Key, "large", body).ID, body)
		largeIDs[index] = large[index].ID
	}

	t.Run("answers each body as stored, in the order asked", func(t *testing.T) {
		response := read(implemented.ID, reviewed.ID, planned.ID)
		if !strings.Contains(response.Body.String(), `"left":[]`) {
			t.Errorf("an answer that leaves nothing writes left as %s, want []", response.Body.String()[max(0, response.Body.Len()-40):])
		}
		requireBodies(t, decodeHandoffBodies(t, response), []wireHandoffBody{
			stored(implemented.ID, exact), stored(reviewed.ID, reviewedBody), stored(planned.ID, plannedBody),
		}, []string{})
	})

	t.Run("five bodies of 250,000 bytes as sent: four answered, the fifth left", func(t *testing.T) {
		if 4*largeSize > contracts.HandoffBodiesMaxBytes || 5*largeSize <= contracts.HandoffBodiesMaxBytes {
			t.Fatalf("four bodies of %d bytes pass the limit of %d, or five do not: the case proves nothing", largeSize, contracts.HandoffBodiesMaxBytes)
		}
		requireBodies(t, decodeHandoffBodies(t, read(largeIDs...)), large[:4], largeIDs[4:])
	})

	t.Run("a first body of 262,144 bytes of < is answered alone, and the rest are left", func(t *testing.T) {
		escaped := `{"text":"` + strings.Repeat("<", contracts.DispatchHandoffBodyMax-len(`{"text":""}`)) + `"}`
		if len(escaped) != contracts.DispatchHandoffBodyMax {
			t.Fatalf("the body is %d bytes, want %d", len(escaped), contracts.DispatchHandoffBodyMax)
		}
		if sent := len(encodedForTest(t, escaped)); sent <= contracts.HandoffBodiesMaxBytes {
			t.Fatalf("the body is sent as %d bytes, within the limit of %d: the case proves nothing", sent, contracts.HandoffBodiesMaxBytes)
		}
		first := recordedHandoff(t, handler, issue.Key, "escaped", escaped)
		response := read(first.ID, planned.ID, reviewed.ID)
		requireBodies(t, decodeHandoffBodies(t, response), []wireHandoffBody{stored(first.ID, escaped)},
			[]string{planned.ID, reviewed.ID})
		if response.Body.Len() <= contracts.HandoffBodiesMaxBytes {
			t.Errorf("the answer is %d bytes, want the first body sent whole past the limit", response.Body.Len())
		}
	})

	t.Run("21 ids are refused", func(t *testing.T) {
		ids := make([]string, contracts.HandoffBodiesMaxIDs+1)
		for index := range ids {
			ids[index] = uuid.NewString()
		}
		refused := read(ids...)
		requireRefused(t, refused, http.StatusBadRequest, "INVALID_QUERY")
		if limit := strconv.Itoa(contracts.HandoffBodiesMaxIDs); !strings.Contains(refused.Body.String(), limit) {
			t.Errorf("the refusal of %d ids does not name the limit %s: %s", len(ids), limit, refused.Body.String())
		}
		// The count is refused before the form of each id is read.
		requireRefused(t, read(append(ids[1:], "not-a-uuid")...), http.StatusBadRequest, "INVALID_QUERY")
		// The limit itself is answered: every id is no handoff of the issue, so each is null.
		answer := decodeHandoffBodies(t, read(ids[1:]...))
		want := make([]wireHandoffBody, len(ids)-1)
		for index, id := range ids[1:] {
			want[index] = wireHandoffBody{ID: id}
		}
		requireBodies(t, answer, want, []string{})
	})

	t.Run("an id that is not a UUID is refused", func(t *testing.T) {
		for _, id := range []string{"not-a-uuid", planned.ID[:8], planned.ID + "0"} {
			t.Run(id, func(t *testing.T) {
				requireRefused(t, read(planned.ID, id), http.StatusBadRequest, "HANDOFF_ID_INPUT")
			})
		}
	})

	t.Run("no ids, an empty id and an id asked twice are refused", func(t *testing.T) {
		for what, target := range map[string]string{
			"no ids":                         route,
			"an empty ids":                   route + "?ids=",
			"an empty id between two":        asking(planned.ID, "", reviewed.ID),
			"an empty id at the end":         asking(planned.ID, ""),
			"an id of spaces":                route + "?ids=" + url.QueryEscape(planned.ID+", "),
			"an id asked twice":              asking(planned.ID, reviewed.ID, planned.ID),
			"an id asked twice in two cases": asking(planned.ID, strings.ToUpper(planned.ID)),
			"no uuid asked twice":            asking("not-a-uuid", "not-a-uuid"),
			"ids given twice":                asking(planned.ID) + "&ids=" + reviewed.ID,
		} {
			t.Run(what, func(t *testing.T) {
				requireRefused(t, dispatchRequest(t, handler, http.MethodGet, target, nil, "alice"),
					http.StatusBadRequest, "INVALID_QUERY")
			})
		}
	})

	t.Run("another issue's handoff is answered null, and its body is not sent", func(t *testing.T) {
		const secret = "the other issue's own plan"
		foreign := recordedHandoff(t, handler, other.Key, "plan", `{"note":"`+secret+`"}`)
		unknown := uuid.NewString()
		response := read(planned.ID, foreign.ID, unknown)
		requireBodies(t, decodeHandoffBodies(t, response), []wireHandoffBody{
			stored(planned.ID, plannedBody), {ID: foreign.ID}, {ID: unknown},
		}, []string{})
		if strings.Contains(response.Body.String(), secret) {
			t.Errorf("the answer under %s carries another issue's handoff: %s", issue.Key, response.Body.String())
		}
		own := dispatchRequest(t, handler, http.MethodGet,
			"/api/v1/issues/"+other.Key+"/handoffs/bodies?ids="+foreign.ID, nil, "alice")
		if !strings.Contains(own.Body.String(), secret) {
			t.Errorf("the handoff's own issue does not answer it: %s", own.Body.String())
		}
	})

	t.Run("a key that no issue has is answered 404", func(t *testing.T) {
		const missing = "BODY-999"
		issueRead := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+missing, nil, "alice")
		want := decodeBody[refusalAnswer](t, issueRead)
		if issueRead.Code != http.StatusNotFound || want.Code != "NOT_FOUND" {
			t.Fatalf("GET /api/v1/issues/%s: status=%d body=%s, want 404 NOT_FOUND", missing, issueRead.Code, issueRead.Body.String())
		}
		routed := dispatchRequest(t, handler, http.MethodGet,
			"/api/v1/issues/"+missing+"/handoffs/bodies?ids="+planned.ID, nil, "alice")
		var got refusalAnswer
		if err := json.Unmarshal(routed.Body.Bytes(), &got); err != nil || routed.Code != http.StatusNotFound || got != want {
			t.Fatalf("the bodies of %s: status=%d body=%s, want 404 with GET /api/v1/issues/%s's refusal %+v",
				missing, routed.Code, routed.Body.String(), missing, want)
		}
		// The ids are held to their rules before the key is read.
		requireRefused(t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+missing+"/handoffs/bodies", nil, "alice"),
			http.StatusBadRequest, "INVALID_QUERY")
	})

	const summary, proof = "Shipped the **parser**.", "`go test ./...` passes."
	treedBody := string(encodedForTest(t, map[string]any{"summary": summary, "count": 3, "proof": []any{proof, map[string]int{"n": 1}}}))
	treed := recordedHandoff(t, handler, issue.Key, "implement", treedBody)

	t.Run("with no trees header the answer carries no tree", func(t *testing.T) {
		response := read(treed.ID, largeIDs[0])
		if body := response.Body.String(); strings.Contains(body, "field_trees") || strings.Contains(body, "_tree") {
			t.Errorf("the answer without the header carries a tree: %.300s", body)
		}
		decodeHandoffBodies(t, response)
	})

	t.Run("with the trees header each string's tree is beside its body, and counts in the limit", func(t *testing.T) {
		tree := func(text string) json.RawMessage { return sameShape(t, pmdoc.DisplayJSON(text, pmdoc.DisplayLatest)) }
		answer := decodeHandoffBodies(t, treesRequest(t, handler, asking(treed.ID, planned.ID), true))
		requireBodies(t, answer, []wireHandoffBody{stored(treed.ID, treedBody), stored(planned.ID, plannedBody)}, []string{})
		for index, want := range []map[string]json.RawMessage{
			{"/summary": tree(summary), "/proof/0": tree(proof)},
			{"/step": tree("plan")},
		} {
			got := make(map[string]json.RawMessage, len(answer.Bodies[index].FieldTrees))
			for pointer, raw := range answer.Bodies[index].FieldTrees {
				got[pointer] = sameShape(t, raw)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s's field_trees =\n\t%s\nwant the tree of each string its card draws\n\t%s", answer.Bodies[index].ID, got, want)
			}
		}

		// The five bodies that answer four without the header answer fewer with it: each entry's
		// trees count beside its body, and the answer stops before the entry that would pass the
		// limit. Every one of the five is sent as the same size, so the next would have taken the
		// size of each answered one.
		counted := decodeHandoffBodies(t, treesRequest(t, handler, asking(largeIDs...), true))
		answered := len(counted.Bodies)
		if answered >= 4 {
			t.Fatalf("the header's answer holds %d of the five bodies, as many as the answer without the trees", answered)
		}
		requireBodies(t, counted, large[:answered], largeIDs[answered:])
		total := 0
		for _, entry := range counted.Bodies {
			if len(entry.FieldTrees) != 2 {
				t.Errorf("%s carries the trees %v, want the tree of its label and of its text", entry.ID, slices.Sorted(maps.Keys(entry.FieldTrees)))
			}
			total += entry.sentSize(t)
		}
		if size := counted.Bodies[0].sentSize(t); total > contracts.HandoffBodiesMaxBytes || total+size <= contracts.HandoffBodiesMaxBytes {
			t.Errorf("the %d answered entries come to %d bytes with their trees, and the next would have come to %d: want the answer to stop before the entry that passes %d",
				answered, total, total+size, contracts.HandoffBodiesMaxBytes)
		}
	})

	t.Run("with the trees header a first body whose trees pass the limit is sent without them", func(t *testing.T) {
		body, markdown := denseMarkdownHandoff(t)
		sent := len(encodedForTest(t, body))
		if trees := len(pmdoc.DisplayJSON(markdown, pmdoc.DisplayLatest)); sent > contracts.HandoffBodiesMaxBytes || sent+trees <= contracts.HandoffBodiesMaxBytes {
			t.Fatalf("the body is sent as %d bytes and its tree is %d: the case needs a body within the limit of %d whose tree takes it past",
				sent, trees, contracts.HandoffBodiesMaxBytes)
		}
		dense := recordedHandoff(t, handler, issue.Key, "dense", body)
		answer := decodeHandoffBodies(t, treesRequest(t, handler, asking(dense.ID, planned.ID), true))
		// The first entry counts its body alone, so the small one after it still fits, trees and all.
		requireBodies(t, answer, []wireHandoffBody{stored(dense.ID, body), stored(planned.ID, plannedBody)}, []string{})
		if trees := answer.Bodies[0].FieldTrees; trees != nil {
			t.Errorf("the first body is sent with the trees %v, want none: they pass the limit", slices.Sorted(maps.Keys(trees)))
		}
		if trees := answer.Bodies[1].FieldTrees; len(trees) != 1 {
			t.Errorf("the entry after it carries the trees %v, want its one string's", slices.Sorted(maps.Keys(trees)))
		}
	})
}
