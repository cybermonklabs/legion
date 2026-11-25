package replay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// shareClock is a clock a test's reader reads and waits on: a wait moves it on by its length,
// rather than sleeping, and is recorded.
type shareClock struct {
	mu    sync.Mutex
	at    time.Time
	waits []time.Duration
}

func (c *shareClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *shareClock) wait(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	c.at = c.at.Add(d)
	return nil
}

// shareStub serves a listing of pages pages, each but the last leading to the next through its
// Link header, each answered with the headers header gives its page, and records the time clock
// reads as each request arrives.
func shareStub(t *testing.T, pages int, header func(page int) map[string]string, clock func() time.Time) (*httptest.Server, *[]time.Time) {
	t.Helper()
	var mu sync.Mutex
	var arrived []time.Time
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrived = append(arrived, clock())
		mu.Unlock()
		page := max(atoi(r.URL.Query().Get("page")), 1)
		for name, value := range header(page) {
			w.Header().Set(name, value)
		}
		if page < pages {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, server.URL, r.URL.Path, page+1))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "[]")
	}))
	t.Cleanup(server.Close)
	return server, &arrived
}

// shareReader is the reader the collectors read GitHub with (newGitHubReader), of the API server
// serves, with reserve and rate, reading and waiting on clock, its waits told to told.
func shareReader(t *testing.T, server *httptest.Server, clock *shareClock, reserve int, rate float64, told *strings.Builder) (githubReader, *url.URL) {
	t.Helper()
	api, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	reader := newGitHubReader(api, stubGitHubToken, clock.wait, reserve, rate)
	reader.client, reader.now, reader.stderr = server.Client(), clock.now, told
	return reader, api.JoinPath("repos", "acme", "widgets", "commits")
}

// quotaLeft is the headers of an answer that leaves remaining requests of a quota of 15000,
// renewed at reset.
func quotaLeft(remaining int, reset time.Time) map[string]string {
	return map[string]string{"X-RateLimit-Limit": "15000", "X-RateLimit-Remaining": strconv.Itoa(remaining),
		"X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}
}

// shareStart is the moment the clock of these tests starts at.
var shareStart = time.Date(2001, time.March, 5, 9, 0, 0, 0, time.UTC)

// After an answer that leaves the quota at or below the reserve, the reader sends no request
// before that answer's reset and rateLimitMargin after it, and tells the wait on stderr in one line
// naming the quota left, the reserve and the moment the wait ends; an answer that leaves more than
// the reserve holds nothing. Here the first page leaves 2999 of a reserve of 3000, renewed in ten
// minutes, so the second page is asked for after one wait of ten minutes and five seconds, at the
// reset and its margin; the second page leaves 3001, so the third is asked for at once. It fails
// where the reader reads the remaining quota of no answer, waits short of the reset, or waits
// untold.
func TestGitHubReaderHoldsTheReserveUntilItsReset(t *testing.T) {
	t.Parallel()
	clock := &shareClock{at: shareStart}
	reset := shareStart.Add(10 * time.Minute)
	server, arrived := shareStub(t, 3, func(page int) map[string]string {
		return quotaLeft(map[int]int{1: 2999, 2: 3001, 3: 3001}[page], reset)
	}, clock.now)
	var told strings.Builder
	reader, first := shareReader(t, server, clock, 3000, 0, &told)
	if _, err := reader.pages(context.Background(), first); err != nil {
		t.Fatalf("the read refused with %v, want the three pages", err)
	}
	until := reset.Add(rateLimitMargin)
	if want := []time.Duration{until.Sub(shareStart)}; !slices.Equal(clock.waits, want) {
		t.Errorf("the reader waited %v, want %v: once, to the reset and its margin", clock.waits, want)
	}
	if want := []time.Time{shareStart, until, until}; !slices.EqualFunc(*arrived, want, time.Time.Equal) {
		t.Errorf("GitHub was asked at %v, want at %v", *arrived, want)
	}
	lines := strings.Split(strings.TrimSpace(told.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr holds %q, want one line for the one wait", told.String())
	}
	for _, part := range []string{"left 2999 of its 15000 requests", "reserve of 3000", "until " + until.Format(time.RFC3339), "GET " + server.URL} {
		if !strings.Contains(lines[0], part) {
			t.Errorf("stderr tells the wait as %q, which does not name %q", lines[0], part)
		}
	}
}

// The reader sends at most rate requests a second: at 2 a second, the four pages of a listing are
// asked for half a second apart, each after a wait of half a second but the first. It fails where
// the reader sends its requests at once.
func TestGitHubReaderPacesItsRequests(t *testing.T) {
	t.Parallel()
	clock := &shareClock{at: shareStart}
	server, arrived := shareStub(t, 4, func(int) map[string]string { return nil }, clock.now)
	reader, first := shareReader(t, server, clock, 0, 2, &strings.Builder{})
	if _, err := reader.pages(context.Background(), first); err != nil {
		t.Fatalf("the read refused with %v, want the four pages", err)
	}
	half := 500 * time.Millisecond
	if want := []time.Duration{half, half, half}; !slices.Equal(clock.waits, want) {
		t.Errorf("the reader waited %v, want %v", clock.waits, want)
	}
	want := []time.Time{shareStart, shareStart.Add(half), shareStart.Add(2 * half), shareStart.Add(3 * half)}
	if !slices.EqualFunc(*arrived, want, time.Time.Equal) {
		t.Errorf("GitHub was asked at %v, want at %v", *arrived, want)
	}
}

// A reserve of 0 and a rate of 0 change nothing: every answer leaves no request of the quota, and
// the reader asks for each page at once, waits for nothing and tells nothing. It fails where a
// reserve of 0 holds a request or a rate of 0 spaces them.
func TestGitHubReaderWithNoReserveAndNoRateChangesNothing(t *testing.T) {
	t.Parallel()
	clock := &shareClock{at: shareStart}
	server, arrived := shareStub(t, 4, func(int) map[string]string { return quotaLeft(0, shareStart.Add(30*time.Minute)) }, clock.now)
	var told strings.Builder
	reader, first := shareReader(t, server, clock, 0, 0, &told)
	if _, err := reader.pages(context.Background(), first); err != nil {
		t.Fatalf("the read refused with %v, want the four pages", err)
	}
	if len(clock.waits) != 0 || len(*arrived) != 4 || told.Len() != 0 {
		t.Errorf("the reader waited %v, asked %d times and told %q; want no wait, 4 requests and nothing told", clock.waits, len(*arrived), told.String())
	}
}

// A reserve's wait past rateLimitLongestWait fails the read, naming the reserve, the reset and
// the longest wait, as a rate limit's does, and sends no further request: here the first page
// leaves 10 of a reserve of 3000, renewed in two hours.
func TestGitHubReaderRefusesAReserveWaitPastItsLongestWait(t *testing.T) {
	t.Parallel()
	clock := &shareClock{at: shareStart}
	server, arrived := shareStub(t, 2, func(int) map[string]string { return quotaLeft(10, shareStart.Add(2*time.Hour)) }, clock.now)
	reader, first := shareReader(t, server, clock, 3000, 0, &strings.Builder{})
	_, err := reader.pages(context.Background(), first)
	if err == nil || !strings.Contains(err.Error(), "reserve of 3000") || !strings.Contains(err.Error(), "past the "+rateLimitLongestWait.String()) {
		t.Errorf("the read answered %v, want a refusal naming the reserve and the longest wait", err)
	}
	if len(*arrived) != 1 || len(clock.waits) != 0 {
		t.Errorf("the reader asked %d times and waited %v, want one request and no wait", len(*arrived), clock.waits)
	}
}

// Every copy of a reader keeps one pace, from any goroutine: two goroutines reading four pages each
// through one reader at 20 requests a second, on the real clock, send their eight requests at
// least about 50 ms apart, not each at its own pace. It fails where the copies keep paces of their
// own, or race on the one.
func TestGitHubReaderKeepsOnePaceAcrossGoroutines(t *testing.T) {
	t.Parallel()
	server, arrived := shareStub(t, 4, func(int) map[string]string { return nil }, time.Now)
	api, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	reader := newGitHubReader(api, stubGitHubToken, nil, 0, 20)
	reader.client = server.Client()
	var group sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		group.Add(1)
		go func() {
			defer group.Done()
			_, errs[i] = reader.pages(context.Background(), api.JoinPath("repos", "acme", "widgets", "commits"))
		}()
	}
	group.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("a read refused with %v", err)
		}
	}
	times := slices.Clone(*arrived)
	slices.SortFunc(times, time.Time.Compare)
	if len(times) != 8 {
		t.Fatalf("GitHub was asked %d times, want 8", len(times))
	}
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < 35*time.Millisecond {
			t.Errorf("requests %d and %d came %s apart, want about 50ms at 20 a second: %v", i, i+1, gap, times)
		}
	}
}
