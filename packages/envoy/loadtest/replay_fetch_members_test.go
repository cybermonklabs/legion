package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The rows of replay-fetch's prints of what Dispatch's answers name: an event, a listing, an
// artifact's answer and the ids and keys in them, each of which whatever answers in Dispatch's
// place can make echo the fetch's bearer and quote a credential's URL (answer_members_test.go).

// memberFetcher is a replayFetcher of the stand-in at handler, with the GitHub-shaped bearer and a
// cache of its own.
func memberFetcher(t *testing.T, handler http.HandlerFunc) *replayFetcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &replayFetcher{api: newAPI(srv.URL, memberBearer, 2), raw: filepath.Join(t.TempDir(), bundleRawDir), conc: 2, lg: log.New(io.Discard, "", 0)}
}

// TestAnEventThatDoesNotDecodeNamesNoneOfIt: Dispatch's event page, or a stand-in's in its place,
// serves an event whose created_at echoes the fetch's bearer and quotes a credential's URL, and
// the decoder's error quoted the time whole. replay-fetch's error names the event's place in the
// page and holds none of it. The control: it still names the issue and the page. Captures
// loadtest.answer-members/event-page.
func TestAnEventThatDoesNotDecodeNamesNoneOfIt(t *testing.T) {
	f := memberFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONAnswer(w, http.StatusOK, []map[string]any{{"id": 1, "issue_key": "PRJ-1", "project": "PRJ", "seq": 1,
			"type": evIssueCreated, "actor": sess("s0"), "created_at": echoed(r), "payload": map[string]any{}}})
	})
	_, err := f.fetchLog(context.Background(), "PRJ-1", 1, tUntil)
	redacttest.RequireNoPiece(t, "loadtest.answer-members/event-page", errorText(err), memberBearer)
	if !strings.Contains(errorText(err), "PRJ-1 events after 0") {
		t.Errorf("fetchLog = %v, want an error naming PRJ-1's page after 0", err)
	}
}

// TestAnEventPayloadThatDoesNotDecodeNamesNoneOfIt: an event of the issue family (its type begins
// issue.) names a type and a payload time that quote a credential's URL and echo a bearer, and
// decodePayload printed the type and the decoder's error, which quoted the time. buildState's
// error names the event's id and none of either. The control: it still names the issue and the
// event. Captures loadtest.answer-members/event-payload.
func TestAnEventPayloadThatDoesNotDecodeNamesNoneOfIt(t *testing.T) {
	ev := mkEvent(t, 2, "PRJ-1", evPrefixIssue+redacttest.Code, mins(-50), sess("s0"),
		map[string]any{"title": "t", "created_at": memberBearer + " " + redacttest.Words})
	_, err := buildState("PRJ", tAt, tUntil, []issueLog{{Key: "PRJ-1", Number: 1, Events: []replayEvent{ev}}}, false)
	redacttest.RequireNoPiece(t, "loadtest.answer-members/event-payload", errorText(err), memberBearer)
	if !strings.Contains(errorText(err), "PRJ-1: event 2 (") {
		t.Errorf("buildState = %v, want an error naming PRJ-1's event 2", err)
	}
}

// TestACreatedIssuesListingNamesNoneOfItsWords: a window's issue.created names an issue key that
// echoes the fetch's bearer and quotes a credential's URL, and the listing replay-fetch reads for
// its spec holds a time that does too. The error printed the key and the decoder's error, which
// quoted the time; it names the key only in an issue key's shape and none of the listing. The
// control: it still names the listing. Captures loadtest.answer-members/created-spec.
func TestACreatedIssuesListingNamesNoneOfItsWords(t *testing.T) {
	f := memberFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONAnswer(w, http.StatusOK, []map[string]any{{"id": memberUUID, "name": "spec", "kind": "doc", "primary": true, "created_at": echoed(r)}})
	})
	key := memberBearer + "-" + redacttest.Code
	window := []replayEvent{mkEvent(t, 1, key, evIssueCreated, mins(5), sess("s0"), issueSnap(key, "t", "open", map[string]any{"primary_artifact_id": nil}))}
	_, err := f.createdSpecs(context.Background(), window)
	redacttest.RequireNoPiece(t, "loadtest.answer-members/created-spec", errorText(err), memberBearer)
	if !strings.Contains(errorText(err), "artifacts: ") {
		t.Errorf("createdSpecs = %v, want an error naming the issue's artifact listing", err)
	}
}

// TestAnAnchoredDocumentsAnswersNameNoneOfTheirWords: an anchor names a document id that echoes
// the fetch's bearer and quotes a credential's URL, and Dispatch's answers for it, or a stand-in's,
// do not decode: its artifact answer holds a time that echoes the bearer (meta), its version is no
// object (version), its blocks are no list (blocks). Each error printed the id, and the first the
// decoder's error, which quoted the time; each names the id only in Dispatch's shape and none of
// the answer. The control: each still names the read. Captures
// loadtest.answer-members/anchored-<case>.
func TestAnAnchoredDocumentsAnswersNameNoneOfTheirWords(t *testing.T) {
	id := memberBearer + "-" + redacttest.Code
	created := tAt.Add(-time.Hour).Format(time.RFC3339Nano)
	for _, c := range []struct {
		name, want string
		listed     bool
		answer     func(r *http.Request) any
	}{
		{"meta", "the answer is not the shape", false, func(r *http.Request) any {
			if strings.HasSuffix(r.URL.Path, "/blocks") {
				return []any{}
			}
			return map[string]any{"id": "d", "created_at": echoed(r)}
		}},
		{"version", " version 1: ", false, func(r *http.Request) any {
			switch {
			case strings.HasSuffix(r.URL.Path, "/blocks"):
				return []any{}
			case strings.Contains(r.URL.Path, "/versions/"):
				return []any{}
			}
			return map[string]any{"id": "d", "created_at": created, "versions": []map[string]any{{"number": 1, "created_at": created}}}
		}},
		{"blocks", " blocks: ", true, func(*http.Request) any { return map[string]any{"blocks": "none"} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := memberFetcher(t, func(w http.ResponseWriter, r *http.Request) { writeJSONAnswer(w, http.StatusOK, c.answer(r)) })
			st := &replayState{Issues: []replayIssue{{Key: "PRJ-1", Number: 1, Exists: true,
				Comments: []replayComment{{ID: "c1", Anchor: &replayAnchor{ArtifactID: id}}}}}}
			byID, texts := map[string]listedArtifact{}, map[string]string{}
			if c.listed {
				byID[id] = listedArtifact{ID: id, Versions: []listedVersion{{Number: 1}}}
				st.Issues[0].Artifacts = []replayArtifact{{ID: id, Kind: "doc", Version: 1, TextVersion: 1, TextPath: "docs/d.md"}}
				texts[id] = "A paragraph.\n"
			}
			_, err := f.fetchBlockMaps(context.Background(), st, nil, byID, texts)
			redacttest.RequireNoPiece(t, "loadtest.answer-members/anchored-"+c.name, errorText(err), memberBearer)
			if !strings.HasPrefix(errorText(err), "artifact ") || !strings.Contains(errorText(err), c.want) {
				t.Errorf("fetchBlockMaps = %v, want an error naming the artifact's read (%q)", err, c.want)
			}
		})
	}
}

// TestAListedKeyThatIsNotTheProjectsNamesNoneOfIt: Dispatch's issue listing, or a stand-in's,
// names a key that echoes the fetch's bearer and quotes a credential's URL, and replay-fetch's
// refusal of a key not of the project quoted it. It names the key only in an issue key's shape.
// The control: the refusal still names the project's key shape. Captures
// loadtest.answer-members/listed-key.
func TestAListedKeyThatIsNotTheProjectsNamesNoneOfIt(t *testing.T) {
	f := memberFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		listing, _ := json.Marshal([]map[string]string{{"key": echoed(r)}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(listing)
	})
	_, err := f.run(context.Background(), "PRJ", tAt, tUntil, t.TempDir(), nil)
	redacttest.RequireNoPiece(t, "loadtest.answer-members/listed-key", errorText(err), memberBearer)
	if !strings.Contains(errorText(err), "is not PRJ-<number>") {
		t.Errorf("replay-fetch = %v, want the refusal of a key not of PRJ", err)
	}
}
