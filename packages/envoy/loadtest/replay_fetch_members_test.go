package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
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
// quoted the time. replay-fetch now refuses such an event where it decodes its page (eventInShape),
// so createdSpecs is handed issue keys alone, and it names none of the listing. The controls: the
// refusal names the page, and the listing's error still names the listing. Captures
// loadtest.answer-members/created-spec.
func TestACreatedIssuesListingNamesNoneOfItsWords(t *testing.T) {
	created := func(key string) replayEvent {
		return mkEvent(t, 1, key, evIssueCreated, mins(5), sess("s0"), issueSnap(key, "t", "open", map[string]any{"primary_artifact_id": nil}))
	}
	page := []json.RawMessage{created(memberBearer + "-" + redacttest.Code).Raw}
	f := memberFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			writeJSONAnswer(w, http.StatusOK, page)
			return
		}
		writeJSONAnswer(w, http.StatusOK, []map[string]any{{"id": memberUUID, "name": "spec", "kind": "doc", "primary": true, "created_at": echoed(r)}})
	})
	_, pageErr := f.fetchLog(context.Background(), "PRJ-1", 1, tUntil)
	_, listingErr := f.createdSpecs(context.Background(), []replayEvent{created("PRJ-2")})
	redacttest.RequireNoPiece(t, "loadtest.answer-members/created-spec", errorText(pageErr)+"\n"+errorText(listingErr), memberBearer)
	if !strings.Contains(errorText(pageErr), "PRJ-1 events after 0: event 1 ") {
		t.Errorf("fetchLog = %v, want a refusal naming PRJ-1's page after 0", pageErr)
	}
	if !strings.Contains(errorText(listingErr), "PRJ-2 artifacts: ") {
		t.Errorf("createdSpecs = %v, want an error naming PRJ-2's artifact listing", listingErr)
	}
}

// TestAnAnchoredDocumentsAnswersNameNoneOfTheirWords: an anchor names a document id that echoes
// the fetch's bearer and quotes a credential's URL, and Dispatch's answers for the document, or a
// stand-in's, do not decode: its artifact answer holds a time that echoes the bearer (meta), its
// version is no object (version), its blocks are no list (blocks). Each error printed the id, and
// the first the decoder's error, which quoted the time. replay-fetch now refuses such an anchor
// where it decodes its event's page (eventInShape), so fetchBlockMaps is handed ids in Dispatch's
// shape alone, and it names none of an answer. The controls: the refusal names the page, and each
// read's error still names the read. Captures loadtest.answer-members/anchored-<case>.
func TestAnAnchoredDocumentsAnswersNameNoneOfTheirWords(t *testing.T) {
	const commentID = "c0000000-0000-4000-8000-00000000000c"
	page := []json.RawMessage{mkEvent(t, 1, "PRJ-1", evCommentCreated, mins(-50), sess("s0"), comment(commentID, map[string]any{
		"anchor": map[string]any{"artifact_id": memberBearer + "-" + redacttest.Code, "quote": "A paragraph."}})).Raw}
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
			f := memberFetcher(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/events") {
					writeJSONAnswer(w, http.StatusOK, page)
					return
				}
				writeJSONAnswer(w, http.StatusOK, c.answer(r))
			})
			_, pageErr := f.fetchLog(context.Background(), "PRJ-1", 1, tUntil)
			id := memberUUID
			st := &replayState{Issues: []replayIssue{{Key: "PRJ-1", Number: 1, Exists: true,
				Comments: []replayComment{{ID: commentID, Anchor: &replayAnchor{ArtifactID: id}}}}}}
			byID, texts := map[string]listedArtifact{}, map[string]string{}
			if c.listed {
				byID[id] = listedArtifact{ID: id, Versions: []listedVersion{{Number: 1}}}
				st.Issues[0].Artifacts = []replayArtifact{{ID: id, Kind: "doc", Version: 1, TextVersion: 1, TextPath: "docs/d.md"}}
				texts[id] = "A paragraph.\n"
			}
			_, err := f.fetchBlockMaps(context.Background(), st, nil, byID, texts)
			redacttest.RequireNoPiece(t, "loadtest.answer-members/anchored-"+c.name, errorText(pageErr)+"\n"+errorText(err), memberBearer)
			if !strings.Contains(errorText(pageErr), "PRJ-1 events after 0: event 1 ") {
				t.Errorf("fetchLog = %v, want a refusal naming PRJ-1's page after 0", pageErr)
			}
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

// TestAListedKeyIsTheProjectsIssueKeyAlone: issueNumber takes a listed key only where it is one of
// the project's issue keys as Dispatch names one, since the bundle keeps the key and replay-fetch
// names a file of raw/ after it. It took any decimal Atoi reads after the project's prefix: a leading
// zero or a sign, and a project given as "../x" with a listed "../x-1" named a file outside raw/. The
// controls: the project's keys still give their numbers.
func TestAListedKeyIsTheProjectsIssueKeyAlone(t *testing.T) {
	for _, c := range []struct {
		project, key string
		want         int
	}{
		{"PRJ", "PRJ-1", 1}, {"PRJ", "PRJ-120", 120}, {"PRJ2", "PRJ2-7", 7},
		{"PRJ", "PRJ-01", 0}, {"PRJ", "PRJ-+1", 0}, {"PRJ", "PRJ-0", 0}, {"PRJ", "OTHER-1", 0}, {"../x", "../x-1", 0},
	} {
		n, err := issueNumber(c.project, c.key)
		if n != c.want || (err == nil) != (c.want > 0) {
			t.Errorf("issueNumber(%q, %q) = %d, %v; want %d", c.project, c.key, n, err, c.want)
		}
	}
}

// TestABundleHoldsNoIDThatEchoesItsRequest: Dispatch's answers, or a stand-in's in their place, name
// an id or a key that echoes the fetch's bearer and quotes a credential's URL: a document of the
// issue's artifact listing (listing), an ask an event of its log opens (ask), a comment an event
// creates (comment), and an issue a window event creates (window-key). replay-fetch kept each in the
// bundle as it came, and its readers printed it: the seed's errors name the document (`document %s
// has no text in the bundle`), the ask and the comment (here, as an actor the seed cannot act as),
// and bundle.meta.json names the created issue. replay-fetch now refuses each answer where it decodes
// it, so no bundle holds such an id and no reader prints one. The controls: the refusal names the
// answer it read, and the same answers naming the id or key in Dispatch's shape make a bundle that
// keeps it as it came and whose seed errors name it. Captures loadtest.answer-members/bundle-<case>.
func TestABundleHoldsNoIDThatEchoesItsRequest(t *testing.T) {
	const (
		spec      = "5c000000-0000-4000-8000-00000000000c"
		created   = "5c000000-0000-4000-8000-00000000000d"
		askID     = "a5000000-0000-4000-8000-000000000005"
		commentID = "c0000000-0000-4000-8000-00000000000c"
	)
	echo := memberBearer + "-" + redacttest.Code
	service := map[string]any{"kind": "service", "id": "svc"}
	issue := func() replayEvent {
		return mkEvent(t, 1, "PRJ-1", evIssueCreated, mins(-100), sess("s0"), issueSnap("PRJ-1", "t", "open", map[string]any{"primary_artifact_id": spec}))
	}
	entry := func(id string, primary bool) map[string]any {
		return map[string]any{"id": id, "name": "doc", "kind": "doc", "primary": primary, "created_at": mins(-100),
			"versions": []map[string]any{{"number": 1, "created_at": mins(-100)}}}
	}
	specOnly := func(string) []map[string]any { return []map[string]any{entry(spec, true)} }
	for _, c := range []struct {
		name, shaped string
		events       func(id string) []replayEvent
		listing      func(id string) []map[string]any
		// refusal is what replay-fetch's refusal names; kept is the id the shaped bundle keeps, and
		// printed what its seed errors print of it ("" where the seed prints none).
		refusal string
		kept    func(b *replayBundle) string
		printed string
	}{
		{"listing", memberUUID, func(string) []replayEvent { return []replayEvent{issue()} },
			func(id string) []map[string]any { return []map[string]any{entry(spec, true), entry(id, false)} },
			"PRJ-1 artifacts: ", func(b *replayBundle) string { return b.State.Issues[0].Artifacts[1].ID },
			"document " + memberUUID + " has no text in the bundle"},
		{"ask", askID, func(id string) []replayEvent {
			return []replayEvent{issue(), mkEvent(t, 2, "PRJ-1", evAskOpened, mins(-50), sess("s0"), map[string]any{
				"id": id, "kind": "question", "state": "open", "question": "q", "author": service, "created_at": mins(-50)})}
		}, specOnly, "PRJ-1 events after 0: event 2 ", func(b *replayBundle) string { return b.State.Issues[0].Asks[0].ID }, "ask " + askID + ": "},
		{"comment", commentID, func(id string) []replayEvent {
			return []replayEvent{issue(), mkEvent(t, 2, "PRJ-1", evCommentCreated, mins(-40), service, comment(id, nil))}
		}, specOnly, "PRJ-1 events after 0: event 2 ", func(b *replayBundle) string { return b.State.Issues[0].Comments[0].ID }, "comment " + commentID + ": "},
		{"window-key", "PRJ-2", func(key string) []replayEvent {
			return []replayEvent{issue(), mkEvent(t, 2, key, evIssueCreated, mins(5), sess("s0"), issueSnap(key, "t", "open", map[string]any{"primary_artifact_id": created}))}
		}, specOnly, "PRJ-1 events after 0: event 2 ", func(b *replayBundle) string {
			for key := range b.Meta.CreatedSpecs {
				return key
			}
			return ""
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			kept, _, err := fetchMemberBundle(t, c.events(c.shaped), c.listing(c.shaped), spec, created)
			if err != nil {
				t.Fatalf("replay-fetch over answers in Dispatch's shape = %v, want a bundle", err)
			}
			if got := c.kept(kept); got != c.shaped {
				t.Fatalf("the bundle keeps %q, want %q as it came", got, c.shaped)
			}
			if !strings.Contains(bundleReaderErrors(kept), c.printed) {
				t.Fatalf("the seed's errors over the bundle are\n%s\nwant one naming %q", bundleReaderErrors(kept), c.printed)
			}

			b, logged, err := fetchMemberBundle(t, c.events(echo), c.listing(echo), spec, created)
			printed := errorText(err) + "\n" + logged
			if b != nil {
				meta, _ := b.readFile(bundleMetaFile)
				printed += "\n" + bundleReaderErrors(b) + "\n" + string(meta)
			}
			redacttest.RequireNoPiece(t, "loadtest.answer-members/bundle-"+c.name, printed, memberBearer)
			if !strings.Contains(errorText(err), c.refusal) {
				t.Errorf("replay-fetch = %v, want a refusal naming %q", err, c.refusal)
			}
		})
	}
}

// fetchMemberBundle is one replay-fetch, into a directory of its own, of a stand-in Dispatch whose
// project lists PRJ-1, whose PRJ-1 log is events and artifact listing is listing, and which serves
// version 1 of each document readable names; it returns the bundle read back where the fetch made
// one, and the fetch's log.
func fetchMemberBundle(t *testing.T, events []replayEvent, listing []map[string]any, readable ...string) (*replayBundle, string, error) {
	t.Helper()
	var page []json.RawMessage
	for _, ev := range events {
		page = append(page, ev.Raw)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/api/v1/issues":
			writeJSONAnswer(w, http.StatusOK, []map[string]string{{"key": "PRJ-1"}})
		case p == "/api/v1/issues/PRJ-1/events":
			writeJSONAnswer(w, http.StatusOK, page)
		case p == "/api/v1/issues/PRJ-1/artifacts":
			writeJSONAnswer(w, http.StatusOK, listing)
		case slices.Contains(readable, strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/artifacts/"), "/versions/1")):
			writeJSONAnswer(w, http.StatusOK, map[string]string{"markdown": "A document's text.\n"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	out := filepath.Join(t.TempDir(), "bundle")
	var logged bytes.Buffer
	f := &replayFetcher{api: newAPI(srv.URL, memberBearer, 2), raw: filepath.Join(out, bundleRawDir), conc: 2, lg: log.New(&logged, "", 0)}
	if _, err := f.run(context.Background(), "PRJ", tAt, tUntil, out, nil); err != nil {
		return nil, logged.String(), err
	}
	b, err := readBundle(out)
	if err != nil {
		t.Fatalf("the bundle does not verify: %v", err)
	}
	return b, logged.String(), nil
}

// bundleReaderErrors are the errors the seed gives over b's documents, asks and comments, one a
// line: each document's text (docText), and the body of each ask and comment (questionBody,
// commentBody), which refuses an actor the seed cannot act as.
func bundleReaderErrors(b *replayBundle) string {
	s := &replaySeeder{b: b}
	var errs []string
	for i := range b.State.Issues {
		is := &b.State.Issues[i]
		for j := range is.Artifacts {
			if _, err := s.docText(&is.Artifacts[j]); err != nil {
				errs = append(errs, err.Error())
			}
		}
		for j := range is.Asks {
			if _, _, err := questionBody(&is.Asks[j]); err != nil {
				errs = append(errs, err.Error())
			}
		}
		for j := range is.Comments {
			if _, _, err := commentBody(&is.Comments[j], "", ""); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	return strings.Join(errs, "\n")
}

// TestABundlesMembersOfNoShapePrintNowhere: a bundle keeps members of Dispatch's answers that have
// no one shape Dispatch gives them, so replay-fetch cannot refuse the others: an actor's kind
// (Dispatch names no closed list of kinds), a typed ask block's id (any text a pasted block names;
// pmdoc's storedBlockID), a document's name, and an event's type (each feature adds its own).
// Whatever answers in Dispatch's place can make each echo the fetch's bearer and quote a
// credential's URL, and the bundle's readers printed each as it came: the seed's refusal of an actor
// it cannot act as (actor-kind) and of a typed block no ask was indexed from (block-id),
// replay-check-seed's difference lines and its -out file (check-document), replay-fetch's counts
// and bundle.meta.json (window-type), and the plan's counts of the events it does not send
// (plan-type). Each now names the member's object by its id, in Dispatch's shape, an event's type
// only where the package names it (shownEventType), or the member not at all. The controls: each
// still names what it refused or counted. Captures loadtest.answer-members/bundle-text-<case>.
func TestABundlesMembersOfNoShapePrintNowhere(t *testing.T) {
	echo := memberBearer + "-" + redacttest.Code
	const askID = "a5000000-0000-4000-8000-000000000005"
	t.Run("actor-kind", func(t *testing.T) {
		_, _, err := actorOf(&replayActor{Kind: echo, ID: "x"})
		redacttest.RequireNoPiece(t, "loadtest.answer-members/bundle-text-actor-kind", errorText(err), memberBearer)
		if !strings.Contains(errorText(err), "cannot be acted as") {
			t.Errorf("actorOf = %v, want the refusal of an actor the seed cannot act as", err)
		}
	})
	t.Run("block-id", func(t *testing.T) {
		pool, _ := loopTestDBAt(t)
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `create table public.asks (id uuid primary key, issue_key text not null, block_id text,
			created_at timestamptz not null default now())`); err != nil {
			t.Fatal(err)
		}
		s := &replaySeeder{pool: pool, ids: newIDStore(nil, nil)}
		_, err := s.ensureAsk(ctx, "PRJ-1", nil, &replayAsk{ID: askID, Kind: "question", State: "answered", BlockID: &echo})
		redacttest.RequireNoPiece(t, "loadtest.answer-members/bundle-text-block-id", errorText(err), memberBearer)
		if !strings.Contains(errorText(err), "ask "+askID+": no ask was indexed from its typed block") {
			t.Errorf("ensureAsk = %v, want the refusal naming the ask %s", err, askID)
		}
	})
	t.Run("check-document", func(t *testing.T) {
		const docID = "d0c00000-0000-4000-8000-000000000001"
		is := &replayIssue{Key: "PRJ-1", Number: 1, Exists: true, Artifacts: []replayArtifact{{ID: docID, Name: echo, Kind: "doc", Version: 1}}}
		r, err := checkSeed(context.Background(), nil, &replayBundle{}, []seedIssuePlan{{Key: "PRJ-1", Number: 1, Issue: is}}, map[[2]string]string{})
		if err != nil {
			t.Fatal(err)
		}
		file, _ := json.Marshal(checkResult{DifferenceList: r.Differences})
		redacttest.RequireNoPiece(t, "loadtest.answer-members/bundle-text-check-document", fmt.Sprintf("%v\n%s", r.Differences, file), memberBearer)
		if len(r.Differences) != 1 || r.Differences[0].Artifact != docID || r.Differences[0].What != "mapping" {
			t.Errorf("differences %+v, want the unmapped document %s", r.Differences, docID)
		}
	})
	t.Run("window-type", func(t *testing.T) {
		const commentID = "c0000000-0000-4000-8000-00000000000c"
		c := stateCounts(replayState{}, []replayEvent{mkEvent(t, 1, "PRJ-1", echo, mins(5), sess("s0"), map[string]any{}),
			mkEvent(t, 2, "PRJ-1", evCommentCreated, mins(6), sess("s0"), comment(commentID, nil))})
		meta, _ := json.Marshal(c)
		redacttest.RequireNoPiece(t, "loadtest.answer-members/bundle-text-window-type", string(meta), memberBearer)
		if c.WindowByType[notAReplayEventType] != 1 || c.WindowByType[evCommentCreated] != 1 {
			t.Errorf("window by type %v, want the comment by its type and the other by %q", c.WindowByType, notAReplayEventType)
		}
	})
	t.Run("plan-type", func(t *testing.T) {
		keyless := map[string]any{}
		if err := json.Unmarshal([]byte(rpEvent(2, echo, "ACME-1", rpSession, time.Second, map[string]any{})), &keyless); err != nil {
			t.Fatal(err)
		}
		keyless["issue_key"] = nil
		line, _ := json.Marshal(keyless)
		p := rpPlan(t, rpOptions(), []string{rpEvent(1, echo, "ACME-1", rpSession, time.Second, map[string]any{}), string(line)}, nil)
		counts, _ := json.Marshal(p.Counts)
		redacttest.RequireNoPiece(t, "loadtest.answer-members/bundle-text-plan-type", string(counts), memberBearer)
		if p.Counts.NotSent["unhandled:"+notAReplayEventType] != 1 || p.Counts.NotSent["no_issue:"+notAReplayEventType] != 1 {
			t.Errorf("not sent %v, want the events of a type the replay does not name counted under %q", p.Counts.NotSent, notAReplayEventType)
		}
	})
}
