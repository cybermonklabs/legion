package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// closeWaitBoard is outboxDispatch holding the issue's close-wait.md as Dispatch holds a
// document: none until the first upload, then each version, each with its own token. An upload
// that names the standing token, or names none where no document stands, writes a version. One
// that names another token is refused 409 PRECONDITION_FAILED with the standing token, or with
// none where no document stands. One that names no token over a standing document is written and
// counted in unguarded, as Dispatch counts each unguarded replacement. editsAfterRead is how many
// of the next token reads are each followed by a person's edit of the document, which moves its
// token before the upload that read meant to name it. tokenErr and uploadErr are what a read of
// the token and an upload answer in place of all that.
type closeWaitBoard struct {
	*outboxDispatch
	mu             sync.Mutex
	versions       []string
	uploads        []closeWaitUpload
	unguarded      int
	editsAfterRead int
	tokenErr       error
	uploadErr      error
}

// closeWaitUpload is one upload of close-wait.md: the token it named, "" for none, and the
// markdown it carried.
type closeWaitUpload struct {
	token, content string
}

// closeWaitToken is the whole-document token of version n of the board's document.
func closeWaitToken(n int) string { return fmt.Sprintf("sha256:%064d", n) }

func (d *closeWaitBoard) ArtifactToken(_ context.Context, key, slug string) (dispatch.DocumentToken, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if slug != record.CloseWaitDocumentSlug {
		return dispatch.DocumentToken{}, fmt.Errorf("a token read of %s's document %s, want %s", key, slug, record.CloseWaitDocumentSlug)
	}
	if d.tokenErr != nil {
		return dispatch.DocumentToken{}, d.tokenErr
	}
	if len(d.versions) == 0 {
		return dispatch.DocumentToken{}, fmt.Errorf("%w: %w", dispatch.ErrDocumentNotFound,
			&dispatch.Error{Status: http.StatusNotFound, Code: dispatch.ArtifactNotFound, Message: `document "close-wait-md" not found by slug`})
	}
	read := dispatch.DocumentToken{Token: closeWaitToken(len(d.versions)), Version: len(d.versions)}
	if d.editsAfterRead > 0 {
		d.editsAfterRead--
		d.versions = append(d.versions, "A person's edit.\n")
	}
	return read, nil
}

func (d *closeWaitBoard) UploadArtifactOver(_ context.Context, key, name, content, token string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if name != record.CloseWaitDocumentName {
		return "", fmt.Errorf("an upload of %s's document %s, want %s", key, name, record.CloseWaitDocumentName)
	}
	d.uploads = append(d.uploads, closeWaitUpload{token: token, content: content})
	if d.uploadErr != nil {
		return "", d.uploadErr
	}
	standing := ""
	if len(d.versions) > 0 {
		standing = closeWaitToken(len(d.versions))
	}
	switch {
	case token == "" && standing != "":
		d.unguarded++
	case token != standing:
		return "", &dispatch.StaleDocument{Current: standing,
			Err: &dispatch.Error{Status: http.StatusConflict, Code: "PRECONDITION_FAILED", Message: "document upload precondition failed"}}
	}
	d.versions = append(d.versions, content)
	return record.CloseWaitDocumentSlug, nil
}

// waitingIssue is LEGION-208 standing in verifying since its merge, its row holding wait as its
// stored wait.
func waitingIssue(t *testing.T, since time.Time, wait record.StoredWait) record.Issue {
	t.Helper()
	stored, err := json.Marshal(wait)
	if err != nil {
		t.Fatalf("encode the stored wait: %v", err)
	}
	return record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Verifying, Generation: 3, Status: "testing", Rank: "U",
		CloseWaitSince: &since, CloseWait: stored}
}

// The close_wait_publish row writes close-wait.md over the token it read just before: its first
// write names no token, since no document stands, and a later write names the token of the version
// it read. It renders the wait the issue's row holds when the row runs, not the one it held when
// the row was queued, so a row that stands while the wait changes writes the newest wait. A
// person's edit between the read and the write is refused with the token that stands, and the row
// writes once more over it, replacing the edit and logging that it did. No write replaces the
// document unguarded. A permanent refusal and a closed issue's ISSUE_CLOSED finish the row, each
// logged, and an outage fails it, so the row runs again.
func TestTheCloseWaitDocumentIsWrittenOverItsToken(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	since := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	entered := since.Add(time.Minute)
	first := record.StoredWait{Entered: entered, Checks: []record.WaitCheck{
		{ID: "c-close", When: "after_merge", Answer: "no", Reason: "no_result", Since: entered}}}
	second := record.StoredWait{Entered: entered, Checks: []record.WaitCheck{
		{ID: "c-close", When: "after_merge", Answer: "unknown", Reason: "pass_and_fail", Since: entered.Add(time.Hour)}}}
	third := record.StoredWait{Entered: entered, Unread: &record.WaitUnread{Cause: "a read before the newest entry into verifying", Since: entered}}
	render := func(wait record.StoredWait) string {
		t.Helper()
		content, err := record.CloseWaitDocument(waitingIssue(t, since, wait))
		if err != nil {
			t.Fatalf("render the stored wait: %v", err)
		}
		return content
	}
	if render(first) == render(second) || render(second) == render(third) {
		t.Fatal("two stored waits render one document, so this test cannot tell which wait a row wrote")
	}
	putOutboxIssue(t, pool, records, waitingIssue(t, since, first))
	board := &closeWaitBoard{outboxDispatch: &outboxDispatch{}}
	var logs bytes.Buffer
	runner := &outbox{log: slog.New(slog.NewJSONHandler(&logs, nil)), pool: pool, records: records, dispatch: board}
	run := func() error {
		t.Helper()
		row := mustOutboxRow(t, "LEGION-208", record.CloseWaitPublish{Generation: 3}, time.Now())
		row.ID = 71
		return runner.execute(context.Background(), row)
	}

	if err := run(); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	if want := []closeWaitUpload{{token: "", content: render(first)}}; !slices.Equal(board.uploads, want) {
		t.Fatalf("uploads = %+v, want the first wait written once, naming no token", board.uploads)
	}

	// The row is queued while the record holds the second wait, and the record holds the third
	// when it runs.
	putOutboxIssue(t, pool, records, waitingIssue(t, since, second))
	queued := mustOutboxRow(t, "LEGION-208", record.CloseWaitPublish{Generation: 3}, time.Now())
	queued.ID = 72
	putOutboxIssue(t, pool, records, waitingIssue(t, since, third))
	if err := runner.execute(context.Background(), queued); err != nil {
		t.Fatalf("the later write: %v", err)
	}
	if got, want := board.uploads[len(board.uploads)-1], (closeWaitUpload{token: closeWaitToken(1), content: render(third)}); got != want || board.unguarded != 0 {
		t.Fatalf("the later write = %+v, with %d unguarded replacements; want the wait the record held when the row ran, over the token of version 1",
			got, board.unguarded)
	}

	board.editsAfterRead = 1
	if err := run(); err != nil {
		t.Fatalf("the write over a person's edit: %v", err)
	}
	if got, want := board.uploads[len(board.uploads)-2:], []closeWaitUpload{{token: closeWaitToken(2), content: render(third)},
		{token: closeWaitToken(3), content: render(third)}}; !slices.Equal(got, want) {
		t.Fatalf("the writes over a person's edit = %+v, want the token read refused, then the write over the token that stands", got)
	}
	if lines := loggedLines(t, logs.String(), closeWaitEditReplaced); len(lines) != 1 || lines[0]["issue"] != "LEGION-208" {
		t.Fatalf("logged %v, want one line saying the edit of LEGION-208's document was replaced:\n%s", lines, logs.String())
	}
	if last := board.versions[len(board.versions)-1]; last != render(third) || len(board.versions) != 4 {
		t.Fatalf("the document's versions = %q, want the daemon's wait standing over the person's edit", board.versions)
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "a refusal Dispatch makes every time", err: &dispatch.Error{Status: http.StatusRequestEntityTooLarge, Code: "CAP_EXCEEDED", Message: "a markdown document is at most 1 MiB"}},
		{name: "a closed issue", err: &dispatch.Error{Status: http.StatusConflict, Code: "ISSUE_CLOSED", Message: "issue is closed"}},
	} {
		logs.Reset()
		board.uploadErr = tc.err
		if err := run(); err != nil {
			t.Fatalf("%s = %v, want the row finished", tc.name, err)
		}
		if lines := loggedLines(t, logs.String(), closeWaitRefused); len(lines) != 1 || lines[0]["issue"] != "LEGION-208" {
			t.Fatalf("%s logged %v, want one line naming the issue:\n%s", tc.name, lines, logs.String())
		}
	}
	board.uploadErr = &dispatch.Error{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Message: "Dispatch is down"}
	queued.ID = 73
	if err := runner.execute(context.Background(), queued); err == nil || permanentRefusal(queued, err) {
		t.Fatalf("an outage = %v, want a failure the row runs again on", err)
	}
	board.uploadErr = nil
	board.tokenErr = &dispatch.Error{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE", Message: "Dispatch is down"}
	uploads := len(board.uploads)
	if err := runner.execute(context.Background(), queued); err == nil || len(board.uploads) != uploads {
		t.Fatalf("a failed token read = %v after %d uploads, want a failure and no upload", err, len(board.uploads)-uploads)
	}
	if board.unguarded != 0 {
		t.Fatalf("%d uploads replaced the document unguarded, want none: each later write names the token it read", board.unguarded)
	}
}
