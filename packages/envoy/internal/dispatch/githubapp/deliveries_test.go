package githubapp_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
)

const deliveriesClientID = "Iv1.deliveries"

func newDeliveriesClient(t *testing.T) (*githubapp.Client, *githubapptest.Webhook) {
	t.Helper()
	key, pemText := githubapptest.Key(t)
	webhook := githubapptest.NewWebhook(t, &key.PublicKey, deliveriesClientID, nil)
	client, err := githubapp.New(&auth.AppConfig{ClientID: deliveriesClientID, PEM: pemText}, webhook.URL())
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client, webhook
}

// The listing returns every attempt whatever its status, follows GitHub's Link cursor across
// pages, and stops at the first attempt older than since; it returns them newest first.
func TestDeliveriesPagesNewestFirstAndStopsAtSince(t *testing.T) {
	client, webhook := newDeliveriesClient(t)
	base := time.Date(2026, 9, 26, 4, 0, 0, 0, time.UTC)
	for i := range 500 {
		code := http.StatusOK
		if i%2 == 1 {
			code = http.StatusServiceUnavailable
		}
		webhook.Record(githubapptest.Attempt{GUID: fmt.Sprintf("guid-%04d", i), Event: "check_run"}, "created", 42, code, base.Add(time.Duration(i)*time.Second))
	}
	since := base.Add(100 * time.Second)

	got, err := client.Deliveries(context.Background(), since, nil)
	if err != nil {
		t.Fatalf("deliveries: %v", err)
	}

	var want []int64
	log := webhook.Log()
	for i := len(log) - 1; i >= 0; i-- {
		if !log[i].DeliveredAt.Before(since) {
			want = append(want, log[i].ID)
		}
	}
	ids := make([]int64, len(got))
	for i, delivery := range got {
		ids[i] = delivery.ID
		wantStatus := "OK"
		if delivery.StatusCode != http.StatusOK {
			wantStatus = "Invalid HTTP Response: 503"
		}
		if delivery.Status != wantStatus || delivery.Event != "check_run" || delivery.Action != "created" || delivery.RepositoryID != 42 {
			t.Fatalf("delivery %d decoded as %+v", i, delivery)
		}
	}
	if len(want) != 400 || !slices.Equal(ids, want) {
		t.Fatalf("listed %d attempts, want the %d at or after since, newest first", len(ids), len(want))
	}
	if webhook.Listings() < 2 {
		t.Fatalf("listing served %d page(s), want the Link cursor followed past the first", webhook.Listings())
	}
}

// A redelivery request answered 202 succeeds; any other status is an error naming it.
func TestRedeliverRequestsTheAttemptAndReportsARefusal(t *testing.T) {
	client, webhook := newDeliveriesClient(t)
	now := time.Now()
	accepted := webhook.Record(githubapptest.Attempt{GUID: "guid-accepted", Event: "pull_request_review"}, "submitted", 7, http.StatusServiceUnavailable, now)
	refused := webhook.Record(githubapptest.Attempt{GUID: "guid-refused", Event: "pull_request_review"}, "submitted", 7, http.StatusServiceUnavailable, now)
	webhook.Refuse(refused.ID, http.StatusUnprocessableEntity)

	if err := client.Redeliver(context.Background(), accepted.ID); err != nil {
		t.Fatalf("redeliver an accepted attempt: %v", err)
	}
	err := client.Redeliver(context.Background(), refused.ID)
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("redeliver a refused attempt: got %v, want an error naming 422", err)
	}
	if got, want := webhook.Requests(), []int64{accepted.ID, refused.ID}; !slices.Equal(got, want) {
		t.Fatalf("redelivery requests %v, want %v", got, want)
	}
}

// A client with no App key cannot sign the JWT these endpoints require.
func TestDeliveriesWithoutAnAppKeyAreErrNoAppKey(t *testing.T) {
	var client *githubapp.Client
	if _, err := client.Deliveries(context.Background(), time.Now(), nil); !errors.Is(err, githubapp.ErrNoAppKey) {
		t.Fatalf("list without a key: got %v, want ErrNoAppKey", err)
	}
	if err := client.Redeliver(context.Background(), 1); !errors.Is(err, githubapp.ErrNoAppKey) {
		t.Fatalf("redeliver without a key: got %v, want ErrNoAppKey", err)
	}
}

// A GraphQL answer is a rate limit by GitHub's own envelope and headers, and by nothing a
// repository's own data can carry. GitHub's data is the names people give their check runs and
// their pull requests: a 200 whose data holds "secondary rate limit" is a page of a repository
// that has a check of that name, and a pass that read it as a limit would stop at that page
// every pass, never move its cursor past it, and leave every head behind it unreconciled. The
// same wording in an `errors` entry is GitHub speaking, and is the limit.
//
// Which of GitHub's two limits an answer is decides the scope it is recorded under
// (Client.GraphQL), so each case says that too: a spent GraphQL budget is primary, and the
// limits GitHub shares between REST and GraphQL are secondary.
func TestGraphQLRateLimitReadsTheEnvelopeAndNeverTheData(t *testing.T) {
	dataNamed := func(name string) string {
		return `{"data":{"repository":{"pullRequests":{"nodes":[{"number":7,"commits":{"nodes":[{"commit":` +
			`{"oid":"abc","checkSuites":{"pageInfo":{"hasNextPage":false},"nodes":[{"databaseId":1,"checkRuns":` +
			`{"pageInfo":{"hasNextPage":false},"nodes":[{"databaseId":2,"name":` + name + `}]}}]}}}]}}]}}}}`
	}
	reset := strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10)
	cases := []struct {
		name      string
		status    int
		header    http.Header
		body      string
		limited   bool
		secondary bool
		wait      time.Duration
	}{{
		name:   "a check run named for the limit is a repository's data",
		status: http.StatusOK,
		body:   dataNamed(`"You have exceeded a secondary rate limit"`),
	}, {
		name:   "a check run named for the error type is a repository's data",
		status: http.StatusOK,
		body:   dataNamed(`"RATE_LIMITED"`),
	}, {
		name:   "a good answer",
		status: http.StatusOK,
		body:   dataNamed(`"ci-ok"`),
	}, {
		name:      "the secondary wording in an errors entry",
		status:    http.StatusOK,
		body:      `{"data":null,"errors":[{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}]}`,
		limited:   true,
		secondary: true,
		wait:      time.Minute,
	}, {
		name:    "an errors entry of type RATE_LIMITED",
		status:  http.StatusOK,
		header:  http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}},
		body:    `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation"}]}`,
		limited: true,
		wait:    90 * time.Second,
	}, {
		name:    "the request that spent the last point",
		status:  http.StatusOK,
		header:  http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}},
		body:    dataNamed(`"ci-ok"`),
		limited: true,
		wait:    90 * time.Second,
	}, {
		name:      "a retry-after at 200",
		status:    http.StatusOK,
		header:    http.Header{"Retry-After": {"60"}},
		body:      dataNamed(`"ci-ok"`),
		limited:   true,
		secondary: true,
		wait:      time.Minute,
	}, {
		name:    "a 403 with no requests remaining",
		status:  http.StatusForbidden,
		header:  http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}},
		body:    `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation"}]}`,
		limited: true,
		wait:    90 * time.Second,
	}, {
		name:      "a 403 with the secondary wording alone",
		status:    http.StatusForbidden,
		body:      `{"data":null,"errors":[{"message":"You have exceeded a secondary rate limit"}]}`,
		limited:   true,
		secondary: true,
		wait:      time.Minute,
	}, {
		name:      "a 403 with a retry-after",
		status:    http.StatusForbidden,
		header:    http.Header{"Retry-After": {"30"}},
		body:      `{"message":"You have exceeded a secondary rate limit"}`,
		limited:   true,
		secondary: true,
		wait:      30 * time.Second,
	}, {
		name:   "a 403 refusing the query",
		status: http.StatusForbidden,
		body:   `{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`,
	}, {
		name:      "a 429",
		status:    http.StatusTooManyRequests,
		header:    http.Header{"Retry-After": {"60"}},
		body:      `{"message":"You have exceeded a secondary rate limit"}`,
		limited:   true,
		secondary: true,
		wait:      time.Minute,
	}}
	for _, want := range cases {
		t.Run(want.name, func(t *testing.T) {
			header := want.header
			if header == nil {
				header = http.Header{}
			}
			limited, secondary := githubapp.GraphQLRateLimit(want.status, header, []byte(want.body))
			if (limited != nil) != want.limited {
				t.Fatalf("the answer read as limited = %v (%v), want %v", limited != nil, limited, want.limited)
			}
			if limited == nil {
				return
			}
			if secondary != want.secondary {
				t.Errorf("the limit read as secondary = %v, want %v: the scope it is recorded under is which of the two it is", secondary, want.secondary)
			}
			// The reset cases are read from a timestamp, which is a second old by now.
			if limited.Wait > want.wait || limited.Wait < want.wait-2*time.Second {
				t.Errorf("the limit asks for %s, want GitHub's own %s", limited.Wait, want.wait)
			}
		})
	}
}
