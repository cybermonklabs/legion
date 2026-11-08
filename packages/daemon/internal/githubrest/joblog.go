package githubrest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// JobLogTail is how much of an Actions job's log JobLog keeps: its end, where a test runner
// prints what failed. The log that sized it is this repository's envoy-go suite, one of the
// eight that run only in the merge queue, which is the class this read exists for: job
// 128558893568 of run 42424201523 is 54,330,939 bytes, and the last of the two "--- FAIL:" lines
// in it stands 51,489,916 bytes before its end (measured 2026-11-08), so a tail short of that
// log's whole length names no test of it at all. 64 MiB holds all of it with a fifth to spare,
// and holds the other measured case, the browser suite of job 128479266994 whose summary block
// is its last 21,517 bytes, three thousand times over.
//
// What one read costs in memory is this, not the log's length: the ring the answer is streamed
// through is this long, or the body's own length where GitHub names a shorter one.
const JobLogTail = 64 << 20

// JobLogBound is how far into a job's log JobLog reads before it gives up: a log longer than this
// is one whose end is not worth reading for. 160 MiB is three times the 54,330,939 bytes job
// 128558893568 of this repository measured on 2026-11-08, the longest log of the suites that run
// only in its merge queue, and two and a half times the tail kept out of it, so a log of that
// class has to grow by half again before its end stops being read. A read that passes this bound
// is still a log the daemon could not read (ErrJobLogTooLong), as it always was.
const JobLogBound = 160 << 20

// JobLogTimeout is how long one read of a job's log takes before it gives up (ErrJobLogSlow). It
// is that read's whole budget: the merge row that reads a removal's logs lends it none of its own
// 30 seconds any more, since a read of a 54 MB log cannot finish inside them (the daemon's
// readyMerge detaches these reads from the row's deadline), so the time bound rises with the byte
// bound. Measured 2026-11-08 from a cluster pod, job 128558893568's 54,330,939 bytes came down in
// 8.5 seconds, 6.4 MB/s: a minute is seven times that read, and a log at JobLogBound is read
// inside it at 2.8 MB/s.
//
// A minute is the production bound, and the only reason this is a var is that a test proving a
// read past it is reported would otherwise wait the whole minute out: such a test lowers this
// and restores it with t.Cleanup. Nothing but a test writes it.
var JobLogTimeout = time.Minute

// jobLogRefusalBody is how much of GitHub's answer JobLog keeps when it refuses the read: enough
// for the message a refusal carries, with no bound of its own to spend on a body GitHub never
// sends.
const jobLogRefusalBody = 64 << 10

// The three ways JobLog reads no log that are not a failed fetch, each told apart from the others
// so the caller can say which it was: the body passed JobLogBound, the read passed JobLogTimeout,
// and GitHub refused the read, which for this route means the installation was never granted
// GitHub's optional Actions read (a 403) and is a normal deployment state rather than a defect.
var (
	ErrJobLogTooLong   = errors.New("the job's log passed the bound this read takes")
	ErrJobLogSlow      = errors.New("the read of the job's log passed its time bound")
	ErrJobLogForbidden = errors.New("GitHub refused the job's log: this installation was never granted GitHub's optional Actions read")
)

// JobLog is the end of the log of Actions job id, the last JobLogTail bytes of it, read under the
// client's REST base (RepositoryAPI): GET /actions/jobs/{id}/logs, which GitHub answers with a
// redirect to blob storage (REST API, Download job logs for a workflow run,
// https://docs.github.com/en/rest/actions/workflow-jobs#download-job-logs-for-a-workflow-run). The
// redirect is followed as any other, and Go's client drops the installation token on the way to
// another host, which is what the pre-signed blob URL wants.
//
// The whole body is read: no Range asks the store for the tail alone, and two measurements taken
// on 2026-11-08 against job 128558893568 of cybermonklabs/legion say that asking is a path
// production would never take, and one that could not help if it did. The store GitHub redirects
// to (Server: Windows-Azure-Blob/1.0) does not implement the suffix form: `Range: bytes=-65536`
// was answered 200 OK with the whole 54,330,939 bytes, where `Range: bytes=0-1023` was answered
// 206 with `Content-Range: bytes 0-1023/54330939`. And a tail short of the whole log would name
// nothing anyway: that log's two "--- FAIL:" lines stand at bytes 422,094 and 2,841,023, the last
// of them 51,489,916 bytes before an end that is Docker teardown. The bytes a range would save
// are the bytes this read exists to find, which is why the tail is 64 MiB — the whole of the
// measured log — and why the body is streamed through the ring below rather than asked for in
// part.
//
// This is a call shape of its own beside call: call reads a whole body and parses it as JSON,
// where a job's log is neither JSON nor bounded. The answer is streamed through a ring of
// JobLogTail bytes, so a log of any length costs that much memory and no more, and the read gives
// up when the body passes JobLogBound (ErrJobLogTooLong) or the read passes JobLogTimeout
// (ErrJobLogSlow). A 403 that is not GitHub's rate limit is ErrJobLogForbidden, the grant a
// deployment may not have; every other answer other than 2xx is an *Answer, as call's is, and a
// request that never arrived is its own error. A log longer than the ring wraps it, and the tail
// then begins after its first line break, since the line it begins in is a line only by accident.
func (c Client) JobLog(ctx context.Context, id int64) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, JobLogTimeout)
	defer cancel()
	target := fmt.Sprintf("%s/actions/jobs/%d/logs", c.API, id)
	request, err := http.NewRequestWithContext(bounded, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", jobLogBound(ctx, bounded, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, jobLogRefusalBody))
		limited := RateLimited(response, body)
		if response.StatusCode == http.StatusForbidden && !limited {
			return "", ErrJobLogForbidden
		}
		return "", c.refusal(http.MethodGet, request.URL, response, body, limited, BudgetCore)
	}
	// The ring is the tail, or the answer's own length where GitHub names a shorter one: a body
	// whose length is known is never read into more memory than it has.
	size := JobLogTail
	if length := response.ContentLength; length > 0 && length < int64(size) {
		size = int(length)
	}
	// The body is streamed through the ring, oldest bytes overwritten as it goes: at is where the
	// next byte goes, and read the whole body's length, which says whether the ring wrapped.
	ring, at, read := make([]byte, size), 0, 0
	for {
		if at == len(ring) {
			at = 0
		}
		n, err := response.Body.Read(ring[at:])
		at, read = at+n, read+n
		if read > JobLogBound {
			return "", ErrJobLogTooLong
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", jobLogBound(ctx, bounded, err)
		}
	}
	tail := ring[:at]
	switch {
	case read < len(ring): // the ring holds the whole body, in the order it arrived
	case at == 0 || at == len(ring): // the body filled the ring exactly, in that order too
		tail = ring
	default: // the ring wrapped: its oldest byte is the one at the write position
		tail = make([]byte, 0, len(ring))
		tail = append(tail, ring[at:]...)
		tail = append(tail, ring[:at]...)
	}
	// The ring wrapped — more of the body arrived than it holds — so its oldest byte stands in
	// the middle of a line, and so of a rune: everything before the first line break is the
	// remains of a line that is a line only by accident, and nothing is to parse it.
	if read > len(ring) {
		if first := bytes.IndexByte(tail, '\n'); first >= 0 {
			tail = tail[first+1:]
		}
	}
	return string(tail), nil
}

// jobLogBound is err as ErrJobLogSlow where JobLog's own time bound is what ended the read, and
// err itself where the caller's context ended first: a read the caller's own budget cut short is
// not the log being slow, and the caller says the two differently.
func jobLogBound(caller, bounded context.Context, err error) error {
	if caller.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return ErrJobLogSlow
	}
	return err
}
