package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// A failed attempt's error is the far side's own text: a relayed command's whole stderr, with
// whatever bytes it wrote and whatever URL it named. The row the attempt returns to the outbox
// still moves on — one more attempt, due again after its backoff — and keeps the start of that
// text, valid UTF-8 and cut to 2048 runes, with no URL's query (record.Store.RetryOutbox). What
// the cut leaves out is in the log, which the outbox writes through a handler built as `legion
// start` builds it (redact.HandlerOptions): the failure's own line carries the text the row keeps,
// the parts the attempt's settle logs join to the whole text once, its last words included
// (settle, storetext.LogWhole), no line is over the 256 KiB one Cloud Logging entry takes, and
// neither the row nor any line holds the token the text's URLs carried.
func TestARetryOutboxErrorThatIsInvalidIsStoredBoundedAndLoggedWhole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	now := time.Now().UTC().Truncate(time.Microsecond)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-208", record.OrdinaryStatusWrite("done", "in_progress", ""), now))

	const planted = "planted-retry-token"
	const first = "Dispatch answered 502 Bad Gateway for https://dispatch.example/api/v1/issues/LEGION-208?token=" + planted +
		" after \x00 an upstream reset \xff\n"
	const line = "the relayed command's stderr goes on, line after line.\n"
	const last = "the last request went to https://dispatch.example/api/v1/events?token=" + planted + " and these are the last words"
	failure := errors.New(first + strings.Repeat(line, (1<<20)/len(line)+1) + last)
	client := &outboxDispatch{issue: dispatch.Issue{Key: "LEGION-208", Status: "in_progress"}, statusErr: failure}
	var logs bytes.Buffer
	runner := &outbox{
		dispatchProject: "LEGION", log: slog.New(slog.NewJSONHandler(&logs, redact.HandlerOptions(nil))),
		pool: pool, records: records, dispatch: client,
		now: func() time.Time { return now },
	}

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}

	var id int64
	var attempts int
	var nextAt time.Time
	var lastError string
	if err := pool.QueryRow(ctx, "select id, attempts, next_at, last_error from outbox").Scan(&id, &attempts, &nextAt, &lastError); err != nil {
		t.Fatalf("read the row after its failed attempt: %v", err)
	}
	if attempts != 1 || !nextAt.Equal(now.Add(record.RetryBackoff(0))) {
		t.Errorf("the row is on attempt %d, next due at %s, want attempt 1 due at %s, after its first backoff",
			attempts, nextAt, now.Add(record.RetryBackoff(0)))
	}
	if !utf8.ValidString(lastError) || strings.ContainsRune(lastError, 0) {
		t.Fatalf("the row's last error starts %q, want valid UTF-8 with no NUL", lastError[:min(len(lastError), 96)])
	}
	if runes := utf8.RuneCountInString(lastError); runes != 2048 || !strings.HasSuffix(lastError, "…") {
		t.Errorf("the row's last error is %d runes ending %q, want 2048 runes ending with …", runes, lastError[max(0, len(lastError)-16):])
	}
	const cleaned = "Dispatch answered 502 Bad Gateway for https://dispatch.example/api/v1/issues/LEGION-208 after \uFFFD an upstream reset \uFFFD\n"
	if start := lastError[:min(len(lastError), 256)]; !strings.Contains(start, cleaned) {
		t.Errorf("the row's last error starts %q, want the error's first words %q", start, cleaned)
	}
	if strings.Contains(lastError, planted) {
		t.Errorf("the row's last error holds the planted token: %q", lastError[:min(len(lastError), 256)])
	}

	if bytes.Contains(logs.Bytes(), []byte(planted)) {
		t.Fatalf("the log holds the planted token")
	}
	type logged struct {
		Level string `json:"level"`
		Msg   string `json:"msg"`
		Row   int64  `json:"row"`
		Issue string `json:"issue"`
		Part  int    `json:"part"`
		Parts int    `json:"parts"`
		Error string `json:"error"`
	}
	var parts []logged
	failed := 0
	for _, text := range strings.Split(strings.TrimSuffix(logs.String(), "\n"), "\n") {
		if len(text) > 256<<10 {
			t.Errorf("a log line is %d bytes, over the 256 KiB one Cloud Logging entry takes: %.200s", len(text), text)
		}
		var entry logged
		if err := json.Unmarshal([]byte(text), &entry); err != nil {
			t.Fatalf("decode a log line of %d bytes: %v", len(text), err)
		}
		switch entry.Msg {
		case outboxErrorInFull:
			parts = append(parts, entry)
		case "outbox row failed":
			failed++
			if entry.Error != lastError {
				t.Errorf("the failure's line carries an error of %d bytes starting %q, want the text the row keeps", len(entry.Error), entry.Error[:min(len(entry.Error), 96)])
			}
		}
	}
	if failed != 1 {
		t.Errorf("the attempt logged %d failure lines, want one", failed)
	}
	if len(parts) < 2 {
		t.Fatalf("the attempt logged %d parts of its error, want the megabyte in several", len(parts))
	}
	slices.SortFunc(parts, func(a, b logged) int { return a.Part - b.Part })
	var whole strings.Builder
	for i, part := range parts {
		if part.Level != "INFO" || part.Row != id || part.Issue != "LEGION-208" || part.Part != i+1 || part.Parts != len(parts) {
			t.Fatalf("logged part %+v, want part %d of %d at INFO naming row %d of LEGION-208", part, i+1, len(parts), id)
		}
		whole.WriteString(part.Error)
	}
	const lastWords = "the last request went to https://dispatch.example/api/v1/events and these are the last words"
	if !strings.HasSuffix(whole.String(), lastWords) {
		t.Errorf("the logged parts join to a text ending %q, want the error's last words %q", whole.String()[max(0, whole.Len()-128):], lastWords)
	}
	if storetext.FarSideError(whole.String()) != lastError {
		t.Errorf("the row's last error is not the start of the text the log holds whole")
	}
}
