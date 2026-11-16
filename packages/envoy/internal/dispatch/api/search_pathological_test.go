package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// Saving a document takes time linear in a pathological run (LEGION-465). Before the fix Postgres's
// search indexing read a run of `a_` or `q_` in quadratic time, and pmdoc parsed `a_b*` in nearly
// quadratic time. On one loaded machine, before the fix and after it at the same moment, 64 KiB of
// `a_` saved in 46 s and 0.3 s, 128 KiB of `a_b*` in 33 s and 1.3-2.1 s, an issue titled with
// 32 KiB of `q_` in 12 s and 0.06-0.17 s, and the duplicate check against a 16 KiB title took
// 18-21 s and 0.12-0.31 s.
//
// Neither a bound on one save's time nor a bound on how it grows from a small size to a large one
// holds on every runner: the large save and the small one run at different moments under different
// load, and the small one is mostly the request's own cost, which load stretches differently from
// the work. On one machine at a load of 140 to 218, a ladder that timed the replacement below in
// this process's CPU time failed 2 of 100 runs, and passed 3 more only on a retry. Each write is
// held to its linear-size control instead (costsNoMoreThanItsParts): one write of a size, timed
// between its parts, sixteen writes of a sixteenth of it each (eight for the replacement below),
// the same input split among them. Work linear in the input makes the one write cost no more than
// its parts, whatever the load does to the request's own cost or to the work, since the parts do
// the same work and pay that cost once each; work quadratic in the input makes it cost about as
// many times as much as there are parts. Each side is read at its least over several samples
// (splitTimes): the load and a stall only ever add time, so a stalled part or a slowed whole write
// moves the verdict only when it reached every sample. The search indexing and the block walk
// below are work no allocation counts: Postgres does the one, and ygo's walk allocates nothing, so
// either defect, put back, left this process's allocations per save as they were and multiplied
// its time.
//
// Each pathological run is a code block's text. Written as paragraph text, the larger runs make
// more elements than one write may (TestUploadRefusesADocumentTooLargeToStore); the document stores
// a code block's text whole and search indexes it whole, which is where the time went, and pmdoc's
// linear-time test holds the parser on the same runs. The `)_` sizes stop at 256 KiB, as before the
// element limit. `)_` saved in linear time before the fix too and is held to the same control.
//
// A comment, a comment edit, a message and a broadcast hold up to contracts.DispatchBlockUnits
// characters in one block - 32,000 since INFRA-45, sixteen times the 2,000 that was too short for a
// comparison to tell the code before the fix from the code after it, whose removed timings measured
// a 2,000-character `a_` comment at 51 ms before the fix and 56 ms after. That bound is held here
// ("comment a_"), a one-block `a_` comment at it being the longest pathological text a caller can
// write outside a document, so a write that stops being bounded at the block bound fails this test
// rather than waiting for a reader to notice. An issue title is capped at 1,000
// (contracts.IssueTitleMax), too short to tell the two apart; the title's checks, and the duplicate
// check's against a stored title, went with the title cap (TestIssueTitlesAreCappedOnCreateAndRetitle).
// Every one of these texts is indexed through the search_text the document checks time.
//
// Settlement is held off. A document's settlement holds its issue's row while it renders, so a
// save timed while one ran would count the wait for it; and the test server's shutdown, which
// would otherwise spend its drain budget settling 1 MiB documents, finds no document loaded and
// leaves each to resume. No test in this package calls t.Parallel, so nothing else this process
// runs shares a timed write's moment.
func TestSavingAPathologicalBodyIsBounded(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createArtifactIssue(t, handler)
	documents := 0
	for _, test := range []struct {
		shape string
		sizes []int
	}{
		{"a_", []int{64 << 10, 1 << 20}},
		{"a_b*", []int{128 << 10, 1 << 20}},
		{")_", []int{16 << 10, 256 << 10}},
	} {
		t.Run("document "+test.shape, func(t *testing.T) {
			for _, size := range test.sizes {
				costsNoMoreThanItsParts(t, "saving a document of `"+test.shape+"`", size, splitParts, func(size int) time.Duration {
					documents++
					const fence = "```\n"
					body := fence + strings.Repeat(test.shape, (size-2*len(fence)-1)/len(test.shape)) + "\n" + fence
					started := time.Now()
					response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
						"name": fmt.Sprintf("pathological-%d.md", documents),
					}, "body.md", "text/markdown", []byte(body), "alice")
					elapsed := time.Since(started)
					if response.Code != http.StatusCreated {
						t.Fatalf("upload %s of %q: status=%d body=%.200s", sizeText(size), test.shape, response.Code, response.Body.String())
					}
					return elapsed
				})
			}
		})
	}
	// A comment's whole body is one search_text run, as a document's is, so the same pathological
	// shape is held at the longest block a caller may send and at a quarter of it.
	t.Run("comment a_", func(t *testing.T) {
		const shape = "a_"
		for _, size := range []int{contracts.DispatchBlockUnits / 4, contracts.DispatchBlockUnits} {
			costsNoMoreThanItsParts(t, "writing a comment of `"+shape+"`", size, splitParts, func(size int) time.Duration {
				body := strings.Repeat(shape, size/len(shape))
				started := time.Now()
				response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
					"body": body,
				}, "alice")
				elapsed := time.Since(started)
				if response.Code != http.StatusCreated {
					t.Fatalf("comment of %s of %q: status=%d body=%.200s", sizeText(size), shape, response.Code, response.Body.String())
				}
				return elapsed
			})
		}
	})
	// Replacing a document of many short blocks with as many others, every block changing type,
	// takes time linear in its blocks (LEGION-465): pmdoc wrote each new block at its own index,
	// walking the document to it, so the replacement grew with the square of its blocks. A quote of
	// n headings replaced with a quote of n rules took 34 ms of server CPU at 682 blocks and 10.5 s
	// at 21,844, the most one write's element limit allows of it, before the fix, and 27 ms and
	// 1.5 s after it, on one machine at a load of 309 to 343. Replaced blocks that stand apart, kept
	// blocks between them, still cost a walk each in ygo (pmdoc's TestUpdateIsLinearInTheBlockCount).
	// Each replacement is a document of its own, seeded with the headings untimed. The whole is split
	// in replacedParts, not splitParts: a part of 2,730 blocks, like the whole's 21,840, holds more
	// blocks than a write indexes in its own request (deferBlockIndexBlocks), where a sixteenth,
	// 1,365, indexes its blocks in its own request and the whole does not, work only the parts would
	// do. Split in sixteen, a quadratic scan put into the upload, 3.8 s of the whole's time, passed at
	// 1.5 times its parts, and pmdoc's walk put back failed at 6.5; split in eight, they failed at 3.9
	// to 4.5 and 11 to 12.7 times.
	t.Run("replacing many blocks", func(t *testing.T) {
		costsNoMoreThanItsParts(t, "replacing a quote of headings with a quote of rules", replacedBlocks*len(replacedTo), replacedParts, func(size int) time.Duration {
			blocks := size / len(replacedTo)
			documents++
			name := fmt.Sprintf("many-blocks-%d.md", documents)
			upload := func(body string) time.Duration {
				started := time.Now()
				response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": name}, "body.md", "text/markdown", []byte(body), "alice")
				elapsed := time.Since(started)
				if response.Code != http.StatusCreated {
					t.Fatalf("upload %d blocks of %q: status=%d body=%.200s", blocks, body[:len(replacedFrom)], response.Code, response.Body.String())
				}
				return elapsed
			}
			upload(strings.Repeat(replacedFrom, blocks))
			return upload(strings.Repeat(replacedTo, blocks))
		})
	})
}

// replacedBlocks is how many blocks TestSavingAPathologicalBodyIsBounded's replacement replaces in
// its whole write: a quote of that many replacedFrom headings becomes a quote of as many replacedTo
// rules, the most one write's element limit allows of it that splits into replacedParts equal
// parts.
const (
	replacedBlocks           = 21_840
	replacedParts            = 8
	replacedFrom, replacedTo = "> #\n", "> ***\n"
)

// TestASplitRefusesQuadraticWork holds costsNoMoreThanItsParts's measure and verdict (splitTimes,
// splitVerdict) to what they are for, with costs computed rather than timed: linear work with a
// request's own cost passes, and so does linear work one of whose whole writes stalls; quadratic
// work fails, and so does quadratic work one of whose parts stalls as long as the whole write
// takes, which the sample of least whole against its own parts passed, and so does the weakest
// defect this file's checks measured, put back - pmdoc writing each replaced block at its own
// index, whose quadratic walk sits beside the replacement's linear work in the whole and in each
// part.
func TestASplitRefusesQuadraticWork(t *testing.T) {
	const size = replacedBlocks * len(replacedTo)
	// The fixed replacement's whole took 0.49 s: a request's own cost and the work linear in it.
	linear := func(n int) time.Duration {
		return 20*time.Millisecond + time.Duration(n)*470*time.Millisecond/time.Duration(size)
	}
	quadratic := func(n int) time.Duration { return time.Duration(n) * time.Duration(n) }
	stalls := 0
	for _, test := range []struct {
		name    string
		cost    func(n int) time.Duration
		refused bool
	}{
		{"linear work with a request's own cost", linear, false},
		{"linear work whose first whole write stalls", func(n int) time.Duration {
			if n == size && stalls == 0 {
				stalls++
				return 20 * linear(n)
			}
			return linear(n)
		}, false},
		{"quadratic work", quadratic, true},
		{"quadratic work one of whose parts stalls as long as the whole write takes", func(n int) time.Duration {
			if n == size/replacedParts && stalls == 0 {
				stalls++
				return quadratic(size)
			}
			return quadratic(n)
		}, true},
		// The replacement with pmdoc's walk put back took 10.7 s for the whole: the fixed code's
		// 0.49 s and a walk of 10.2 s growing as the square of the blocks (the comment above
		// TestSavingAPathologicalBodyIsBounded's replacement has the measurement).
		{"the replacement's walk put back", func(n int) time.Duration {
			fraction := float64(n) / float64(size)
			return linear(n) + time.Duration(fraction*fraction*float64(10200*time.Millisecond))
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stalls = 0
			whole, parts := splitTimes(size, replacedParts, test.cost)
			if err := splitVerdict(test.name, size, replacedParts, whole, parts); (err != nil) != test.refused {
				t.Fatalf("verdict on %s (%s against %s for its parts) = %v, want refused %t", test.name, whole, parts, err, test.refused)
			}
		})
	}
}

// splitParts is how many writes a write's linear-size control splits its input among
// (costsNoMoreThanItsParts), where nothing the server does changes with the write's size.
const splitParts = 16

// splitAllowance is how many times its parts' time, between them, one write may take. Linear work
// makes the one write take less than its parts, which do the same work and pay a request's own
// cost once each; quadratic work makes it take about as many times as long as there are parts,
// eight or sixteen, less the linear work both sides share.
const splitAllowance = 2

// sampleCount is how many times splitTimes times the whole write and its parts, and so how many
// whole writes a load that slows the whole write but not the parts has to reach before the verdict
// reads it as growth, and how many samples' parts a stall has to reach before it can hide growth.
// With the replacement's process and its Postgres on one CPU that three busy loops took in bursts
// of 0.2 to 2 s, 9 of 120 samples of the fixed replacement had a whole over splitAllowance times
// its own parts; groups drawn from those samples put the least whole over splitAllowance times
// the least parts about once in 3,900 groups of five, once in 20,000 of six and once in 77,000 of
// seven. At this machine's own load of 165 to 308, seven samples put the replacement's verdict at
// 0.82 to 0.99 over 10 runs, and every other write's at 0.84 or less; the test took 46 to 55 s.
const sampleCount = 7

// costsNoMoreThanItsParts holds a write of size to its linear-size control: timed(size), one write
// of the whole input, against split calls of timed(size/split), each a write of its own, timed
// around it (splitTimes). It fails t when the whole takes more than splitAllowance times its parts
// between them (splitVerdict).
func costsNoMoreThanItsParts(t *testing.T, what string, size, split int, timed func(size int) time.Duration) {
	t.Helper()
	whole, parts := splitTimes(size, split, timed)
	if err := splitVerdict(what, size, split, whole, parts); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s at %s, %.2f times the %s %d writes of %s took between them (at most %d)",
		what, whole.Round(time.Millisecond), sizeText(size), float64(whole)/float64(parts), parts.Round(time.Millisecond), split, sizeText(size/split), splitAllowance)
}

// splitTimes takes sampleCount samples, each the whole write of size timed between two halves of
// its split parts, and returns the least whole and the least parts total over the samples. The
// machine's load, a stall and a request's warm-up only ever add to a time, so each least is the
// sample of its side that they reached least: a whole write slowed, or a part stalled, in some of
// the samples leaves another sample's least in its place, and only a load that slows every whole
// write and spares some sample's parts is read as growth. Writing the parts in halves around each
// whole measures both sides over the same stretch of the run, whose load changes as it goes.
func splitTimes(size, split int, timed func(size int) time.Duration) (whole, parts time.Duration) {
	for sample := range sampleCount {
		var each time.Duration
		for range split / 2 {
			each += timed(size / split)
		}
		one := timed(size)
		for range split - split/2 {
			each += timed(size / split)
		}
		if sample == 0 || one < whole {
			whole = one
		}
		if sample == 0 || each < parts {
			parts = each
		}
	}
	return whole, parts
}

// splitVerdict is nil when the whole write of size took at most splitAllowance times its split
// parts between them, and otherwise names both.
func splitVerdict(what string, size, split int, whole, parts time.Duration) error {
	if whole <= splitAllowance*parts {
		return nil
	}
	return fmt.Errorf("%s took %s at %s, %.1f times the %s %d writes of %s took between them: they write the same input, so a write linear in it takes no longer than they do, and may take %d times as long at most (splitAllowance)",
		what, whole.Round(time.Millisecond), sizeText(size), float64(whole)/float64(parts), parts.Round(time.Millisecond), split, sizeText(size/split), splitAllowance)
}

// sizeText is a size in bytes as this file's messages give it: in MiB or KiB where it is a whole
// number of them, else in bytes, so a comment's 2,000 characters never read as 1 KiB.
func sizeText(size int) string {
	switch {
	case size >= 1<<20 && size%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", size>>20)
	case size >= 1<<10 && size%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", size>>10)
	}
	return fmt.Sprintf("%d B", size)
}

// Ordinary text searches as it did before search_text: a title, a document, a comment, an ask
// with its answer and a message are each found by a word they hold, with the same snippet marks,
// and a word joined to others by underscores is still found by itself.
func TestSearchFindsOrdinaryAndUnderscoredTextAfterSearchText(t *testing.T) {
	handler := newTestHandler(t)
	corpus := seedSearchCorpus(t, handler)
	for _, test := range []struct{ query, kind, id string }{
		{"astrolabe", "document", corpus.primaryArtifactID},
		{"sextant", "comment", corpus.commentID},
		{"alidade", "ask", corpus.askID},
		{"compass calibration", "message", corpus.messageID},
		{"navigation instruments", "issue", corpus.issueKey},
	} {
		results := searchResponse(t, handler, "q="+url.QueryEscape(test.query)).Results
		found := false
		for _, result := range results {
			if result.Kind == test.kind && result.ID == test.id {
				found = true
			}
		}
		if !found {
			t.Errorf("search %q: no %s %s among %d results", test.query, test.kind, test.id, len(results))
		}
	}
	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+corpus.issueKey+"/messages", map[string]any{
		"body": "set LEGION_E2E_MODEL_GATEWAY_URL and the pod_safety_overlay_name_for_each_role_in_the_bundle_of_prompts_the_daemon_snapshots_at_boot_time first",
	}, "alice")
	if message.Code != http.StatusCreated {
		t.Fatalf("create underscored message: status=%d body=%s", message.Code, message.Body.String())
	}
	for _, query := range []string{"legion_e2e_model_gateway_url", "gateway", "snapshots", "boot"} {
		results := searchResponse(t, handler, "q="+url.QueryEscape(query)).Results
		found := false
		for _, result := range results {
			if result.Kind == "message" && strings.Contains(result.Snippet, "<mark>") {
				found = true
			}
		}
		if !found {
			t.Errorf("search %q: the underscored message was not found with a marked snippet", query)
		}
	}
}

// A document whose markdown makes more elements than one write may - 1 MiB of `)_`, 262,144
// italic spans, held a gigabyte while it was saved (LEGION-481) - is refused as too large to store,
// naming the limit, on a new document (SeedText) and on a replacement (ReplaceText), never a 500.
func TestUploadRefusesADocumentTooLargeToStore(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	body := strings.Repeat(")_", (1<<20)/2)
	// A new document (SeedText): refused, and nothing is created.
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "too-large.md"}, "body.md", "text/markdown", []byte(body), "alice")
	if !refusedTooLarge(t, response) || !strings.Contains(response.Body.String(), "more than 65536 elements") {
		t.Fatalf("seed of a document too large to store: status=%d body=%.400s, want 413 CAP_EXCEEDED naming the 65536-element limit, as the document service words it", response.Code, response.Body.String())
	}
	if listing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts", nil, "alice"); strings.Contains(listing.Body.String(), "too-large") {
		t.Fatalf("the refused seed left an artifact behind: %.300s", listing.Body.String())
	}
	// A replacement of a stored document (ReplaceText): refused the same way, and the document keeps
	// its version.
	if first := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "body.md", "text/markdown", []byte("# Notes\n"), "alice"); first.Code != http.StatusCreated {
		t.Fatalf("seed notes.md: status=%d body=%.300s", first.Code, first.Body.String())
	}
	response = multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "body.md", "text/markdown", []byte(body), "alice")
	if !refusedTooLarge(t, response) || !strings.Contains(response.Body.String(), "more than 65536 elements") {
		t.Fatalf("replacement by a document too large to store: status=%d body=%.400s, want 413 CAP_EXCEEDED naming the 65536-element limit, as the document service words it", response.Code, response.Body.String())
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts/notes-md/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"# Notes\n"`) {
		t.Fatalf("notes.md after the refused replacement: status=%d body=%.300s, want its first version", text.Code, text.Body.String())
	}
}

// Every route that writes a caller's markdown refuses markdown past the element limit as 413
// CAP_EXCEEDED naming the limit, and writes nothing: a spec at issue creation, an issue's and a
// project's document upload as JSON, and an edit's insert and replace.
func TestEveryMarkdownWriteRefusesMarkdownPastTheElementLimit(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Element limit", "Before.\n")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "DOCS", "name": "Docs"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	heavy := strings.Repeat(")_", 200_000)
	for _, write := range []struct {
		name, path string
		body       any
	}{
		{"a spec at issue creation", "/api/v1/issues", map[string]string{"project": "TEST", "title": "Too heavy", "spec": heavy}},
		{"an issue's document upload", "/api/v1/issues/" + issue.Key + "/artifacts", map[string]string{"name": "heavy.md", "content": heavy}},
		{"a project's document upload", "/api/v1/projects/DOCS/artifacts", map[string]string{"name": "heavy.md", "content": heavy}},
		{"an edit's insert", "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/edits", map[string]any{
			"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": heavy}},
		}},
		{"an edit's replace", "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/edits", map[string]any{
			"ops": []map[string]string{{"op": "replace", "find": "Before.", "with": heavy}},
		}},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, write.path, write.body, "alice")
		if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(response.Body.String(), "more than 65536 elements") {
			t.Errorf("%s past the element limit: status=%d body=%.300s, want 413 CAP_EXCEEDED naming the limit", write.name, response.Code, response.Body.String())
		}
	}
	if listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice"); strings.Contains(listed.Body.String(), "Too heavy") {
		t.Errorf("the refused issue was created: %.300s", listed.Body.String())
	}
	for _, path := range []string{"/api/v1/issues/" + issue.Key + "/artifacts", "/api/v1/projects/DOCS/artifacts"} {
		if listed := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice"); strings.Contains(listed.Body.String(), "heavy") {
			t.Errorf("a refused upload left a document behind at %s: %.300s", path, listed.Body.String())
		}
	}
	if text := documentMarkdown(t, handler, issue.PrimaryArtifactID); text != "Before.\n" {
		t.Errorf("the spec after the refused edits = %q, want it unchanged", text)
	}
}
