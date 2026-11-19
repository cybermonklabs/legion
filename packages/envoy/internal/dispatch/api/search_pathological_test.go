package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Saving a pathological body costs work linear in it (LEGION-465). Before the fix Postgres's
// search indexing read a run of `a_` or `q_` in quadratic time, and pmdoc parsed `a_b*` in nearly
// quadratic time. On one loaded machine, before the fix and after it at the same moment, 64 KiB of
// `a_` saved in 46 s and 0.3 s, 128 KiB of `a_b*` in 33 s and 1.3-2.1 s, an issue titled with
// 32 KiB of `q_` in 12 s and 0.06-0.17 s, and the duplicate check against a 16 KiB title took
// 18-21 s and 0.12-0.31 s. Times follow the runner's load, so no clock bound holds on every runner,
// and this test reads no clock: it counts work, and reads the structure that bounds it.
//
// Postgres's quadratic read was its text-search parser's (0056_search_text): wherever letters or
// digits meet an underscore it tries to read a host name to the end of the run, so a run costs the
// square of its length, all of it processor time, which no count of pages sees. On Postgres 18.6,
// `select to_tsvector('english', t) from (select repeat('a_', n) t offset 0) s` under EXPLAIN
// (ANALYZE, BUFFERS), the subquery keeping the text out of the planner's constant folding, read:
//
//	text     raw         through search_text   shared buffers, both
//	1 KiB    26 ms       8 ms                  22 hit, 0 read
//	8 KiB    2.2 s       75 ms                 22 hit, 0 read
//	16 KiB   8.1 s       96 ms                 22 hit, 0 read
//	32 KiB   36.3 s      178 ms                22 hit, 0 read
//
// So the fix is held by its structure: every trigger that builds a search vector reads its text
// through search_text (searchTriggers, read from pg_proc's source), and search_text leaves no run
// of more than searchTextRun underscore-joined segments in any body saved here
// (requireShortSearchRuns), which is what keeps the parser's rescans short. Beside them each save's
// indexing statement is counted in the shared buffers it reads (indexingBuffers) and held to
// linear growth (growsLinearly), which bounds the statement's own reads of the text: its TOAST,
// its index entries and its document_search copy.
//
// Written as paragraph text, the larger runs make more elements than one write may
// (TestUploadRefusesADocumentTooLargeToStore), so each run is a code block's text: the document
// stores it whole and search indexes it whole, and pmdoc's linear-time test holds the parser on
// the same runs. The `)_` ladder stops at 256 KiB, as before the element limit. `)_` saved in
// linear time before the fix too and is held to the same growth.
//
// A comment, a comment edit, a message and a broadcast hold up to contracts.DispatchBlockUnits
// characters in one block - 32,000 since INFRA-45, sixteen times the 2,000 that was too short
// for a ladder to tell the code before the fix from the code after it, whose removed timings
// measured a 2,000-character `a_` comment at 51 ms before the fix and 56 ms after. That bound is
// laddered here ("comment a_"), a one-block `a_` comment at it being the longest pathological
// text a caller can write outside a document, so a write that stops being bounded at the block
// bound fails this test rather than waiting for a reader to notice. An issue title is capped at
// 1,000 (contracts.IssueTitleMax), too short to ladder; the title's ladders, and the duplicate
// check's against a stored title, went with the title cap
// (TestIssueTitlesAreCappedOnCreateAndRetitle). Every one of these texts is indexed through the
// search_text the search triggers are read for.
//
// Settlement is held off. A document's settlement runs on a goroutine of its own after the save,
// beside the counts a rung reads, and the test server's shutdown, which would otherwise spend its
// drain budget settling 1 MiB documents, finds no document loaded and leaves each to resume.
func TestSavingAPathologicalBodyIsBounded(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createArtifactIssue(t, handler)
	t.Run("search triggers", func(t *testing.T) {
		triggers := searchTriggers(t, database)
		// The two the rungs below fire, so a reading of pg_proc that found none cannot pass.
		for _, fired := range []string{"artifact_versions_search", "comments_search"} {
			if _, read := triggers[fired]; !read {
				t.Fatalf("%s is not among the trigger functions read as building a search vector: %v", fired, triggers)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(triggers)) {
			if !triggers[name] {
				t.Errorf("%s builds a search vector from text that no search_text reads first, in its own source or in a vector function it calls: Postgres's parser reads an underscore-joined run in time that grows with the square of its length (0056_search_text)", name)
			}
		}
		t.Logf("trigger functions that build a search vector, each read for the search_text its text passes through: %v", slices.Sorted(maps.Keys(triggers)))
	})
	documents := 0
	for _, test := range []struct {
		shape string
		sizes []int
	}{
		{"a_", []int{1 << 10, 64 << 10, 1 << 20}},
		{"a_b*", []int{2 << 10, 128 << 10, 1 << 20}},
		{")_", []int{256, 16 << 10, 256 << 10}},
	} {
		t.Run("document "+test.shape, func(t *testing.T) {
			growsLinearly(t, "indexing a document of `"+test.shape+"`", "buffers", test.sizes, func(size int) uint64 {
				documents++
				const fence = "```\n"
				body := fence + strings.Repeat(test.shape, (size-2*len(fence)-1)/len(test.shape)) + "\n" + fence
				response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
					"name": fmt.Sprintf("pathological-%d.md", documents),
				}, "body.md", "text/markdown", []byte(body), "alice")
				if response.Code != http.StatusCreated {
					t.Fatalf("upload %s of %q: status=%d body=%.200s", sizeText(size), test.shape, response.Code, response.Body.String())
				}
				saved := decodeBody[struct {
					Artifact model.Artifact `json:"artifact"`
				}](t, response).Artifact
				requireShortSearchRuns(t, database, sizeText(size)+" of `"+test.shape+"`", `select markdown from artifact_versions where artifact_id = $1`, saved.ID)
				return indexingBuffers(t, database, `update artifact_versions set markdown = markdown where artifact_id = $1`, saved.ID)
			})
		})
	}
	// A comment's whole body is one search_text run, as a document's is, so the same pathological
	// shape is counted at the longest block a caller may send, in three rungs derived from it: the
	// block bound is a thirty-second of the largest document rung, enough of a span for a ladder
	// since INFRA-45.
	t.Run("comment a_", func(t *testing.T) {
		const shape = "a_"
		sizes := []int{contracts.DispatchBlockUnits / 32, contracts.DispatchBlockUnits / 4, contracts.DispatchBlockUnits}
		growsLinearly(t, "indexing a comment of `"+shape+"`", "buffers", sizes, func(size int) uint64 {
			body := strings.Repeat(shape, size/len(shape))
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": body,
			}, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("comment of %s of %q: status=%d body=%.200s", sizeText(size), shape, response.Code, response.Body.String())
			}
			comment := decodeBody[model.Comment](t, response)
			requireShortSearchRuns(t, database, "a comment of "+sizeText(size)+" of `"+shape+"`", `select body from comments where id = $1`, comment.ID)
			return indexingBuffers(t, database, `update comments set body = body where id = $1`, comment.ID)
		})
	})
	// Replacing a document of many short blocks with as many others, every block changing type,
	// takes work linear in its blocks (LEGION-465): pmdoc wrote each new block at its own index,
	// walking the document to it, so the replacement grew with the square of its blocks. A quote of
	// n headings replaced with a quote of n rules took 34 ms of server CPU at 682 blocks and 10.5 s
	// at 21,844, the most one write's element limit allows of it, before the fix, and 27 ms and
	// 1.5 s after it, on one machine at a load of 309 to 343. Replaced blocks that stand apart, kept
	// blocks between them, still cost a walk each in ygo (pmdoc's TestUpdateIsLinearInTheBlockCount).
	// Each rung counts the fragment items the replacement's writes reach (pmdoc.UpdateWork): ygo's
	// walk to each index Update gives it and the items it deletes or inserts there, which is where
	// the square was. The count is the process's, and no test in this package calls t.Parallel.
	t.Run("replacing many blocks", func(t *testing.T) {
		const from, to = "> #\n", "> ***\n"
		growsLinearly(t, "replacing a quote of headings with a quote of rules", "fragment items", []int{682 * len(to), 5_461 * len(to), 21_844 * len(to)}, func(size int) uint64 {
			blocks := size / len(to)
			upload := func(name string, body []byte) {
				response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": name}, "body.md", "text/markdown", body, "alice")
				if response.Code != http.StatusCreated {
					t.Fatalf("upload %d blocks of %q: status=%d body=%.200s", blocks, body[:len(from)], response.Code, response.Body.String())
				}
			}
			documents++
			name := fmt.Sprintf("many-blocks-%d.md", documents)
			upload(name, []byte(strings.Repeat(from, blocks)))
			before := pmdoc.UpdateWork()
			upload(name, []byte(strings.Repeat(to, blocks)))
			return pmdoc.UpdateWork() - before
		})
	})
}

// indexingBuffers is how many shared buffers PostgreSQL reads to index a stored text once more:
// statement writes the text back to itself, which fires its table's search trigger as the save's
// own write did, under EXPLAIN (ANALYZE, BUFFERS) in a transaction rolled back. Hit plus read is
// every page the statement asked for, whether the cache held it or not, so the count is the
// statement's work and not the cache's state.
func indexingBuffers(t *testing.T, database *store.Store, statement string, args ...any) uint64 {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the explain of %s: %v", statement, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var explained []struct {
		Plan struct {
			SharedHit  uint64 `json:"Shared Hit Blocks"`
			SharedRead uint64 `json:"Shared Read Blocks"`
		} `json:"Plan"`
	}
	if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+statement, args...).Scan(&explained); err != nil {
		t.Fatalf("explain %s: %v", statement, err)
	}
	if len(explained) != 1 {
		t.Fatalf("explain %s gave %d plans, want one", statement, len(explained))
	}
	return explained[0].Plan.SharedHit + explained[0].Plan.SharedRead
}

// searchTextRun is the most underscore-joined segments search_text leaves in one run: it breaks a
// run with a space after every sixteenth segment (0056_search_text), which bounds how far
// Postgres's parser rescans a run from each place in it.
const searchTextRun = 16

// requireShortSearchRuns fails t when search_text leaves a run of more than searchTextRun
// underscore-joined segments in the text stored selects, what being the text's name: that run is
// what the search triggers hand Postgres's parser. The run is read with search_text's own
// segment class.
func requireShortSearchRuns(t *testing.T, database *store.Store, what, stored string, args ...any) {
	t.Helper()
	var longest int
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(max(length(run[1]) - length(replace(run[1], '_', '')) + 1), 0)::int
		  from (`+stored+`) s(body),
		       regexp_matches(search_text(s.body), '([A-Za-z0-9.-]+(?:_[A-Za-z0-9.-]+)*)', 'g') run
	`, args...).Scan(&longest); err != nil {
		t.Fatalf("read the longest run search_text leaves in %s: %v", what, err)
	}
	if longest > searchTextRun {
		t.Fatalf("search_text left a run of %d underscore-joined segments in %s, past the %d it breaks a run at (0056_search_text): Postgres's parser reads a run in time that grows with the square of its length",
			longest, what, searchTextRun)
	}
}

// searchTriggers is each function a trigger runs that builds a text-search vector - one whose
// source calls a function returning tsvector - and whether it reads its text through search_text,
// in its own source or in the source of one of those vector functions it calls (issues_search
// builds its vector in issue_search_vector). It reads pg_proc's source text, so a function that
// names search_text only in a comment counts as reading through it.
func searchTriggers(t *testing.T, database *store.Store) map[string]bool {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select p.proname, p.prosrc, p.prorettype = 'tsvector'::regtype,
		       exists (select 1 from pg_trigger tr where tr.tgfoid = p.oid and not tr.tgisinternal)
		  from pg_proc p
		 where p.prorettype = 'tsvector'::regtype
		    or exists (select 1 from pg_trigger tr where tr.tgfoid = p.oid and not tr.tgisinternal)`)
	if err != nil {
		t.Fatalf("read the trigger and vector functions: %v", err)
	}
	type function struct {
		name, source    string
		vector, trigger bool
	}
	var functions []function
	for rows.Next() {
		var f function
		if err := rows.Scan(&f.name, &f.source, &f.vector, &f.trigger); err != nil {
			t.Fatalf("read a function's source: %v", err)
		}
		functions = append(functions, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the trigger and vector functions: %v", err)
	}
	calls := func(source, name string) bool {
		return regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*\(`).MatchString(source)
	}
	triggers := map[string]bool{}
	for _, f := range functions {
		if !f.trigger {
			continue
		}
		builds, through := false, calls(f.source, "search_text")
		for _, vector := range functions {
			if vector.vector && calls(f.source, vector.name) {
				builds = true
				through = through || calls(vector.source, "search_text")
			}
		}
		if builds {
			triggers[f.name] = through
		}
	}
	return triggers
}

// TestGrowsLinearlyRejectsQuadraticFinalRung proves the final rung's bound stays anchored to the
// first rung's count, even when the rung between fills the growth it may take: a final rung held
// to the rung before it would pass work that grows with the square of the input.
func TestGrowsLinearlyRejectsQuadraticFinalRung(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestGrowsLinearlyRejectsQuadraticFinalRungHelper$")
	command.Env = append(os.Environ(), "GROWS_LINEARLY_HELPER=true")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("quadratic final rung passed:\n%s", output)
	}
	if !strings.Contains(string(output), "at "+sizeText(quadraticFinalRung)) {
		t.Fatalf("quadratic final rung failed unexpectedly:\n%s", output)
	}
}

// quadraticFinalRung is the last size of the helper's ladder, the rung whose count grows
// quadratically and fails the bound.
const quadraticFinalRung = 64

func TestGrowsLinearlyRejectsQuadraticFinalRungHelper(t *testing.T) {
	if _, helper := os.LookupEnv("GROWS_LINEARLY_HELPER"); !helper {
		t.Skip("subprocess helper")
	}
	growsLinearly(t, "a quadratic final rung", "steps", []int{1, 8, quadraticFinalRung}, func(size int) uint64 {
		switch size {
		case 1:
			return 1
		case 8:
			return 32
		case quadraticFinalRung:
			return uint64(size * size / 4)
		default:
			t.Fatalf("unexpected size %d", size)
			return 0
		}
	})
}

// growthAllowance is the permitted variation in work per input byte from the first rung. Work
// linear in the input takes no more per input byte as it grows, and less while a fixed cost counts,
// so each later rung may take four times the first rung's count per byte. Quadratic growth exceeds
// that headroom across this ladder's input ratios.
const growthAllowance = 4

// growsLinearly counts a three-rung ladder's work and holds each later rung to the first. The
// counts are the work itself, which no load on the machine moves, so each size is counted once. A
// count of none at the first size fails, since a growth from nothing proves nothing.
func growsLinearly(t *testing.T, what, unit string, sizes []int, counted func(size int) uint64) {
	t.Helper()
	firstSize := sizes[0]
	first := counted(firstSize)
	if first == 0 {
		t.Fatalf("%s counted no %s at %s, so its growth says nothing: none of the work it does is counted", what, unit, sizeText(firstSize))
	}
	for _, size := range sizes[1:] {
		checkLinearGrowth(t, what, unit, firstSize, first, size, counted(size))
	}
}

func checkLinearGrowth(t *testing.T, what, unit string, firstSize int, first uint64, size int, count uint64) {
	t.Helper()
	growth := size / firstSize
	allowed := uint64(growthAllowance*growth) * first
	ratio := float64(count) / float64(first)
	if count > allowed {
		t.Fatalf("%s took %d %s at %s, %.0f times the %d it took at %s: the input grew %d times, and work linear in it may take %d at most (growthAllowance)",
			what, count, unit, sizeText(size), ratio, first, sizeText(firstSize), growth, allowed)
	}
	t.Logf("%s: %d %s at %s, %d at %s, %.1f times for an input %d times larger (at most %d)", what, first, unit, sizeText(firstSize), count, sizeText(size), ratio, growth, allowed)
}

func sizeText(size int) string {
	switch {
	case size >= 1<<20 && size%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", size>>20)
	case size >= 1<<10:
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
