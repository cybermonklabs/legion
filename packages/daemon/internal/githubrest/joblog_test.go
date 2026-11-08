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
			kept := string(tail)
			switch {
			case err != nil:
				t.Fatalf("JobLog = %v, want the log's end", err)
			case len(tail) > JobLogTail:
				t.Fatalf("JobLog kept %d bytes, want no more than the %d of its tail", len(tail), JobLogTail)
			case !strings.HasSuffix(kept, tc.log[len(tc.log)-len(logLine(0)):]):
				t.Fatalf("JobLog ends %q, want the log's last line", kept[max(0, len(kept)-80):])
			case !strings.HasPrefix(kept, "line "):
				t.Fatalf("JobLog starts %q, want a whole line: the line the cut fell in is left out", kept[:min(len(kept), 80)])
			}
			if len(tc.log) <= JobLogTail && kept != tc.log {
				t.Fatalf("JobLog of a log that fits the tail = %d bytes, want all %d of it", len(tail), len(tc.log))
			}
			if len(tc.log) > JobLogTail && strings.Contains(kept, logLine(0)) {
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
	if err != nil || string(tail) != logOf(3) || redirects != 1 {
		t.Fatalf("JobLog = %q, %v after %d redirects; want the blob's log read through GitHub's one redirect", tail, err, redirects)
	}
}

// Each way the read ends with no log is told apart from the others, so the caller says which it
// was: a body past JobLogBound, a read past the client's time bound, GitHub refusing the read
// because the installation was never granted its optional Actions read (a 403 that is no rate
// limit), and every other answer as the *Answer any refused REST call is, a 403 that is GitHub's
// rate limit among them, since that one is the token's budget and not the grant.
//
// The row that proves the time bound sets a moment as the bound of its own client: what that row
// proves is that a read past the bound is told apart from the others, never what the bound's
// value is, and waiting the production minute out would cost every run of this suite a minute.
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
		// bound is the time bound the row's own client is built with, so a read that has to
		// pass that bound waits a moment rather than the minute production allows.
		bound time.Duration
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
		{name: "a read past the time bound", bound: 50 * time.Millisecond,
			answer: func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
			want:   ErrJobLogSlow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(tc.answer))
			defer server.Close()
			client := Client{Token: "token", API: server.URL + "/repos/acme/widgets", JobLogTimeout: tc.bound}
			tail, err := client.JobLog(context.Background(), 7)
			if len(tail) > 0 {
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

// A body GitHub names no length for — a chunked answer, or a connection the store closes to end
// it — is read into a ring that grows as its bytes arrive, where the whole 64 MiB tail used to be
// allocated before the first of them: the memory of a 54 MB log spent on a log of a few lines.
// The ring is what the read answers with, so what one cost is the capacity of what it answers.
func TestAJobLogReadGrowsItsRingWithABodyOfUnnamedLength(t *testing.T) {
	// Some 108,000 bytes: past the length the ring starts at, under the leap to the whole tail.
	log := logOf(4000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A flushed answer carries no Content-Length: its head is already sent when the body is
		// written, which is how GitHub's store answers a log it streams.
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, log)
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
	tail, err := client.JobLog(context.Background(), 7)
	if err != nil || string(tail) != log {
		t.Fatalf("JobLog = %d bytes, %v; want all %d of a body whose length GitHub named none for", len(tail), err, len(log))
	}
	if cap(tail) > 2*len(log) {
		t.Fatalf("JobLog read %d bytes into a ring of %d, want one grown with the body rather than the %d of the whole tail", len(tail), cap(tail), JobLogTail)
	}
}

// endingBody is a response body as Go's HTTP client hands one over, in the two shapes the end of
// a body arrives in: lateEOF answers the last bytes with no error of their own and the EOF on a
// read after them, which is HTTP/2's shape, and without it the last read carries the EOF, which
// is HTTP/1.1's for a body whose length the answer named.
type endingBody struct {
	rest    []byte
	lateEOF bool
}

func (b *endingBody) Read(p []byte) (int, error) {
	if len(b.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.rest)
	b.rest = b.rest[n:]
	if len(b.rest) == 0 && !b.lateEOF {
		return n, io.EOF
	}
	return n, nil
}

// The ring grows only where the answer named no length for the body. A named length sizes the
// ring to the whole log, which the read then holds entire, and the last of such a body need not
// arrive with its EOF: over HTTP/2 the EOF comes on a read of its own after the bytes, where
// HTTP/1.1 ends the last read with it. A ring that grew there would hold twice a log it already
// held whole, or the whole 64 MiB tail once twice its length passed the leap. The ring is what
// the read answers with, so what a read cost is the capacity of what it answers.
func TestAJobLogRingGrowsOnlyWhereTheAnswerNamedNoLength(t *testing.T) {
	// Some 108,000 bytes: past the length a growing ring starts at, under the leap to the tail.
	log := logOf(4000)
	for _, tc := range []struct {
		name string
		// length is the body's length as the answer named it, and zero where it named none;
		// lateEOF is the EOF arriving on a read of its own; ring is the capacity the read must
		// answer with.
		length  int64
		lateEOF bool
		ring    int
	}{
		{name: "a named length whose last read carries the EOF", length: int64(len(log)), ring: len(log)},
		{name: "a named length whose EOF arrives on a read of its own", length: int64(len(log)), lateEOF: true, ring: len(log)},
		{name: "no length named, read into a ring grown with the body", lateEOF: true, ring: 2 * jobLogRingStart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tail, err := readJobLogTail(&endingBody{rest: []byte(log), lateEOF: tc.lateEOF}, tc.length)
			if err != nil || string(tail) != log {
				t.Fatalf("readJobLogTail = %d bytes, %v; want all %d of the body", len(tail), err, len(log))
			}
			if cap(tail) != tc.ring {
				t.Fatalf("readJobLogTail read %d bytes into a ring of %d, want one of %d", len(tail), cap(tail), tc.ring)
			}
		})
	}
}

// GitHub answers this route with a redirect to blob storage, and that store refuses a read of
// its own with a 403 when the signature it was handed has expired. That refusal says nothing
// about any grant: only a 403 from GitHub's own API host is the optional Actions read a
// deployment's App may never have been granted. The *Answer names the path under the client's
// REST base, never the pre-signed URL the read was redirected to.
func TestAJobLogRefusedByTheBlobStoreIsNoMissingActionsGrant(t *testing.T) {
	const signature = "S3CR3T-signature-of-the-read"
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `<Error><Code>AuthenticationFailed</Code></Error>`, http.StatusForbidden)
	}))
	defer blob.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blob.URL+"/legion-logs/7?sig="+signature, http.StatusFound)
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
	tail, err := client.JobLog(context.Background(), 7)
	var answer *Answer
	switch {
	case len(tail) > 0:
		t.Fatalf("JobLog kept %d bytes of a read the store refused", len(tail))
	case errors.Is(err, ErrJobLogForbidden):
		t.Fatalf("JobLog = %v, want the store's refusal rather than this installation's missing Actions read", err)
	case !errors.As(err, &answer) || answer.Status != http.StatusForbidden || answer.RateLimited:
		t.Fatalf("JobLog = %v, want an *Answer with status 403 and no rate limit", err)
	case answer.Path != "/actions/jobs/7/logs":
		t.Fatalf("the *Answer names %q, want the path under the client's REST base", answer.Path)
	case strings.Contains(err.Error(), signature):
		t.Fatalf("JobLog = %v, want no pre-signed signature in what it says", err)
	}
}

// Every job-log read this process makes takes one of jobLogReadsAtOnce places, whatever issue's
// removal asked for it. One removal reads eight logs at once (ghautomerge's jobLogReads), which
// bounds that removal and nothing else: with no limit over the lanes together, every removal
// being told reads its own eight, each holding a ring of up to JobLogTail against the daemon
// pod's 8 GiB.
func TestJobLogReadsTakeOneOfTheProcessesPlaces(t *testing.T) {
	reads := jobLogReadsAtOnce + 1
	arrived, release := make(chan struct{}, reads), make(chan struct{}, reads)
	free := func() {
		for range reads {
			release <- struct{}{}
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		_, _ = io.WriteString(w, logOf(1))
	}))
	defer server.Close()
	defer free() // every read is let go before the server is closed, which waits for them
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
	done := make(chan error, reads)
	for range reads {
		go func() {
			_, err := client.JobLog(context.Background(), 7)
			done <- err
		}()
	}
	for range jobLogReadsAtOnce {
		<-arrived
	}
	select {
	case <-arrived:
		t.Fatalf("read %d of %d reached GitHub while the other %d were in flight, want it waiting for a place", reads, reads, jobLogReadsAtOnce)
	case <-time.After(50 * time.Millisecond):
	}
	free()
	for range reads {
		if err := <-done; err != nil {
			t.Fatalf("a read that waited for its place = %v, want the log it waited for", err)
		}
	}
}
