package githubrest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// logLine is one line of a job's log as these tests write it, numbered so a tail can be held to
// the lines it must and must not carry.
func logLine(n int) string { return fmt.Sprintf("line %06d of a job's log\n", n) }

// logOf is a log of n lines, oldest first.
func logOf(n int) string {
	var log strings.Builder
	for i := range n {
		log.WriteString(logLine(i))
	}
	return log.String()
}

// JobLog reads a job's log under the client's REST base and keeps its end, which is where a test
// runner prints what failed: a log shorter than JobLogTail whole, a longer one cut to the last
// JobLogTail bytes of it and then to the first line break in them, so the line the cut fell in is
// never half a line read as a whole one. GitHub answers the route with a redirect to blob
// storage, which the read follows as a browser would.
func TestAJobLogReadKeepsTheEndOfTheLog(t *testing.T) {
	// long is a log of more than JobLogTail bytes, so its start cannot be in the answer.
	long := logOf(2 * JobLogTail / len(logLine(0)))
	for _, tc := range []struct{ name, log string }{
		{name: "a log shorter than the tail kept", log: logOf(10)},
		{name: "a log longer than the tail cut to its end", log: long},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/widgets/actions/jobs/7/logs" || r.Header.Get("Authorization") != "Bearer token" {
					http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
					return
				}
				_, _ = io.WriteString(w, tc.log)
			}))
			defer server.Close()
			client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
			tail, err := client.JobLog(context.Background(), 7)
			switch {
			case err != nil:
				t.Fatalf("JobLog = %v, want the log's end", err)
			case len(tail) > JobLogTail:
				t.Fatalf("JobLog kept %d bytes, want no more than the %d of its tail", len(tail), JobLogTail)
			case !strings.HasSuffix(tail, tc.log[len(tc.log)-len(logLine(0)):]):
				t.Fatalf("JobLog ends %q, want the log's last line", tail[max(0, len(tail)-80):])
			case !strings.HasPrefix(tail, "line "):
				t.Fatalf("JobLog starts %q, want a whole line: the line the cut fell in is left out", tail[:min(len(tail), 80)])
			}
			if len(tc.log) <= JobLogTail && tail != tc.log {
				t.Fatalf("JobLog of a log that fits the tail = %d bytes, want all %d of it", len(tail), len(tc.log))
			}
			if len(tc.log) > JobLogTail && strings.Contains(tail, logLine(0)) {
				t.Fatalf("JobLog of a log longer than its tail carries the log's first line, want its end alone")
			}
		})
	}
}

// GitHub answers this route with a 302 to blob storage, so the read follows the redirect and
// reads the log from where it lands.
func TestAJobLogReadFollowsGitHubsRedirectToBlobStorage(t *testing.T) {
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, logOf(3))
	}))
	defer blob.Close()
	var redirects int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirects++
		http.Redirect(w, r, blob.URL+"/storage/job-7", http.StatusFound)
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
	tail, err := client.JobLog(context.Background(), 7)
	if err != nil || tail != logOf(3) || redirects != 1 {
		t.Fatalf("JobLog = %q, %v after %d redirects; want the blob's log read through GitHub's one redirect", tail, err, redirects)
	}
}

// Each way the read ends with no log is told apart from the others, so the caller says which it
// was: a body past JobLogBound, a read past JobLogTimeout, GitHub refusing the read because the
// installation was never granted its optional Actions read (a 403 that is no rate limit), and
// every other answer as the *Answer any refused REST call is, a 403 that is GitHub's rate limit
// among them, since that one is the token's budget and not the grant.
func TestAJobLogReadSaysWhyItReadNoLog(t *testing.T) {
	for _, tc := range []struct {
		name string
		// answer serves the read; want is the error the read must carry, and status the status
		// of the *Answer it must be when want is nil.
		answer func(w http.ResponseWriter, r *http.Request)
		want   error
		status int
		// limited is the rate limit the *Answer must carry.
		limited bool
	}{
		{name: "a body past the byte bound", answer: func(w http.ResponseWriter, r *http.Request) {
			block := strings.Repeat("x", 64<<10)
			for written := 0; written <= JobLogBound; written += len(block) {
				if _, err := io.WriteString(w, block); err != nil {
					return
				}
			}
		}, want: ErrJobLogTooLong},
		{name: "a refusal for want of the Actions read", answer: func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
		}, want: ErrJobLogForbidden},
		{name: "a 403 that is GitHub's rate limit", answer: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Minute).Unix()))
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		}, status: http.StatusForbidden, limited: true},
		{name: "a job GitHub does not have", answer: func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}, status: http.StatusNotFound},
		{name: "a read past the time bound", answer: func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}, want: ErrJobLogSlow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(tc.answer))
			defer server.Close()
			client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
			tail, err := client.JobLog(context.Background(), 7)
			if tail != "" {
				t.Fatalf("JobLog kept %d bytes of a read that answered no log", len(tail))
			}
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("JobLog = %v, want %v", err, tc.want)
				}
				return
			}
			var answer *Answer
			if !errors.As(err, &answer) || answer.Status != tc.status || answer.RateLimited != tc.limited {
				t.Fatalf("JobLog = %v, want an *Answer with status %d and rate limited %t", err, tc.status, tc.limited)
			}
			if answer.Path != "/actions/jobs/7/logs" {
				t.Fatalf("the *Answer names %q, want the path under the client's REST base", answer.Path)
			}
		})
	}
}

// A read the caller's own context ends is the caller's budget spent, not the log being slow: the
// row that reads a log has a budget of its own, and saying the log passed a bound it never
// reached would send its tree after the wrong thing.
func TestAJobLogReadTellsItsOwnBoundFromItsCallers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.JobLog(ctx, 7)
	if err == nil || errors.Is(err, ErrJobLogSlow) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("JobLog under a caller's spent budget = %v, want that budget's own error", err)
	}
}
