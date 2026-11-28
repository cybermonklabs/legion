package record

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/signals"
)

// requestsSince is when the provider's record checks below began keeping ACME's model requests,
// under a limit of 6,000 tokens a minute; requestsAt is the moment m minutes and s seconds after.
var requestsSince = time.Date(2026, time.November, 26, 9, 0, 0, 0, time.UTC)

func requestsAt(m, s int) time.Time {
	return requestsSince.Add(time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
}

// beginRequests records that ACME's model requests are kept from requestsSince, under its limit.
func beginRequests(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx := context.Background()
	must(t, NewStore().BeginSignalInput(ctx, tx, "ACME", signals.Provider, requestsSince))
	must(t, NewStore().KeepTokenLimit(ctx, tx, "ACME", 6000, requestsSince))
}

// modelRequest is request id of model, ended at ended, with tokens uncached input tokens, or none
// where tokens is below 0.
func modelRequest(id, model string, ended time.Time, tokens int64) claim.ModelRequest {
	request := claim.ModelRequest{ID: id, Session: "ses_implementer", Model: model, EndedAt: ended}
	if tokens >= 0 {
		request.UncachedInputTokens = &tokens
	}
	return request
}

// keepRequests keeps one report of ACME-7's implementer at generation 1, as the daemon keeps it at
// requestsAt(10, 0), counting every model but other-model.
func keepRequests(t *testing.T, tx pgx.Tx, requests ...claim.ModelRequest) {
	t.Helper()
	report := ModelRequestsReport{Token: claim.Token("legion-acme-acme-7-implementer"), Generation: 1, Requests: requests}
	must(t, NewStore().KeepModelRequests(context.Background(), tx, "ACME", report,
		func(model string) bool { return model != "other-model" }, requestsAt(10, 0)))
}

// minuteWords is each minute of ACME's provider record from requestsAt(from, 0) to
// requestsAt(to, 0) as ProviderMinutes reads it: "<HH:MM> <tokens>", then " not held" where the
// record does not hold it and " (<n> untold)" where n counted requests of it have no token count.
func minuteWords(t *testing.T, tx pgx.Tx, from, to int) []string {
	t.Helper()
	minutes, err := NewStore().ProviderMinutes(context.Background(), tx, "ACME", requestsAt(from, 0), requestsAt(to, 0), 0)
	must(t, err)
	var words []string
	for start := requestsAt(from, 0); start.Before(requestsAt(to, 0)); start = start.Add(time.Minute) {
		minute, held := minutes.Read(start)
		word := fmt.Sprintf("%s %d", start.Format("15:04"), minute.Uncached)
		if !held {
			word += " not held"
		}
		if untold := minutes.Untold(start); untold > 0 {
			word += fmt.Sprintf(" (%d untold)", untold)
		}
		words = append(words, word)
	}
	return words
}

func wantMinutes(t *testing.T, tx pgx.Tx, when string, from, to int, want ...string) {
	t.Helper()
	if got := minuteWords(t, tx, from, to); !slices.Equal(got, want) {
		t.Fatalf("%s, the provider's record reads %q, want %q", when, got, want)
	}
}

// A request of a model the project counts that its host reported with no token count leaves its
// minute's tokens unknown, so the provider's record does not hold that minute and its headroom
// reads as not recorded, never as the sum of the minute's other requests (S2). The record counts
// such requests of each minute (untold). A request of a model the project does not count holds its
// minute whatever it reports, and the same report kept again counts nothing twice.
func TestAMinuteWithACountedRequestOfNoTokenCountIsNotHeld(t *testing.T) {
	inTx(t, migratedStore(t), func(tx pgx.Tx) {
		beginRequests(t, tx)
		report := []claim.ModelRequest{
			modelRequest("told-1", "acme-model", requestsAt(1, 5), 1000),
			modelRequest("told-2", "acme-model", requestsAt(1, 10), 500),
			modelRequest("untold-1", "acme-model", requestsAt(1, 20), -1),
			modelRequest("untold-2", "acme-model", requestsAt(2, 1), -1),
			modelRequest("untold-3", "acme-model", requestsAt(2, 2), -1),
			modelRequest("other", "other-model", requestsAt(3, 0), -1),
			modelRequest("told-3", "acme-model", requestsAt(3, 30), 700),
		}
		keepRequests(t, tx, report...)
		want := []string{"09:01 1500 not held (1 untold)", "09:02 0 not held (2 untold)", "09:03 700", "09:04 0"}
		wantMinutes(t, tx, "after the report", 1, 5, want...)
		keepRequests(t, tx, report...)
		wantMinutes(t, tx, "after the same report again", 1, 5, want...)
	})
}

// A request reported again with a later end, as the Claude Code bridge reports one a block of
// which came after its report, moves to that end: its tokens and its count of untold tokens leave
// the minute of its earlier end for the minute of its later one, so it counts once, in the minute
// of its last block (S1). A later end in the same minute moves no minute's sum, and an earlier or
// equal end, as a retry of the first report carries, moves nothing back.
func TestARequestReportedAgainWithALaterEndMovesToThatEndsMinute(t *testing.T) {
	inTx(t, migratedStore(t), func(tx pgx.Tx) {
		beginRequests(t, tx)
		keepRequests(t, tx,
			modelRequest("told", "acme-model", requestsAt(1, 58), 1200),
			modelRequest("untold", "acme-model", requestsAt(1, 59), -1),
			modelRequest("stays", "acme-model", requestsAt(1, 30), 300))
		wantMinutes(t, tx, "after the first report", 1, 3, "09:01 1500 not held (1 untold)", "09:02 0")
		keepRequests(t, tx,
			modelRequest("told", "acme-model", requestsAt(2, 10), 1200),
			modelRequest("untold", "acme-model", requestsAt(2, 11), -1))
		wantMinutes(t, tx, "after a later end of two of its requests", 1, 3, "09:01 300", "09:02 1200 not held (1 untold)")
		keepRequests(t, tx, modelRequest("told", "acme-model", requestsAt(2, 40), 1200))
		wantMinutes(t, tx, "after a later end in the same minute", 1, 3, "09:01 300", "09:02 1200 not held (1 untold)")
		keepRequests(t, tx,
			modelRequest("told", "acme-model", requestsAt(1, 58), 1200),
			modelRequest("untold", "acme-model", requestsAt(1, 59), -1))
		wantMinutes(t, tx, "after the first report again", 1, 3, "09:01 300", "09:02 1200 not held (1 untold)")
		var ended time.Time
		must(t, tx.QueryRow(context.Background(), `select ended_at from model_requests where project = 'ACME' and id = 'told'`).Scan(&ended))
		if !ended.Equal(requestsAt(2, 40)) {
			t.Errorf("the request told ends at %s, want its latest end %s", ended.UTC(), requestsAt(2, 40))
		}
	})
}
