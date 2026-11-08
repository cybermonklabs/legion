package githubrest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"
)

// JobLogTail is how much of an Actions job's log JobLog keeps: its end, where a test runner
// prints what failed. The log that sized it is this repository's envoy-go suite, one of the
// eight that run only in the merge queue, which is the class this read exists for: job
// 128558893568 of run 42424201523 is 54,330,939 bytes, and the last of the two "--- FAIL:" lines
// in it stands 51,489,916 bytes before its end (measured 2026-11-08), so a tail short of that
// log's whole length names no test of it at all. 64 MiB holds all of it with a fifth to spare,
// and holds the other measured case, the browser suite of job 128479266994 whose summary block
// is its last 21,517 bytes, three thousand times over. What a log longer than this tail and
// shorter than JobLogBound loses by being read as its end alone is told under JobLogBound.
//
// What one read costs in memory is this, not the log's length: the ring the answer is streamed
// through is never longer than this, it is the body's own length where GitHub names a shorter
// one and grows to what arrives where GitHub names none (jobLogRingStart), and it is what the
// read answers, since its bytes are handed to their parser where they lie rather than copied
// into a string of their own. How many such rings stand in this process at once is
// jobLogReadsAtOnce.
const JobLogTail = 64 << 20

// JobLogBound is how far into a job's log JobLog reads before it gives up: a log longer than this
// is one whose end is not worth reading for. 160 MiB is three times the 54,330,939 bytes job
// 128558893568 of this repository measured on 2026-11-08, the longest log of the suites that run
// only in its merge queue, and two and a half times the tail kept out of it, so a log of that
// class has to grow by half again before its end stops being read. A read that passes this bound
// is still a log the daemon could not read (ErrJobLogTooLong), as it always was.
//
// Between the two bounds stands a gap taken knowingly: a log longer than JobLogTail and shorter
// than this one is read as its last JobLogTail bytes, so a Go failure printed early in such a log
// is not among the bytes kept, and the removal's line says the daemon could not read its failing
// tests — the end states no test name — rather than naming them. No log of this repository meets
// that gap today: the longest it produces is the 54,330,939 bytes of job 128558893568 of run
// 42424201523, which is inside the tail. The way out, when a suite grows past the tail, is a
// larger tail rather than a different read: the store GitHub redirects to answers no suffix
// range, and the bytes an early range would skip are the ones this read exists to find (JobLog).
const JobLogBound = 160 << 20

// DefaultJobLogTimeout is how long one read of a job's log takes before it gives up
// (ErrJobLogSlow) under a client that names no bound of its own: the production value, and what
// every Client in the tree runs with. It bounds the whole of one read — the wait for one of this
// process's jobLogReadsAtOnce places and the fetch that follows it — and nothing beyond it. The
// caller's own context bounds the read as well, and whichever of the two runs out first ends it:
// a removal's logs are read under the one deadline every read of that removal shares (the
// daemon's removalLogReads, three minutes), so one slow log gives up after this minute and the
// removal's whole reading after those three. A read the caller's budget cut short says so rather
// than naming this bound (jobLogBound). Measured 2026-11-08 from a cluster pod, job
// 128558893568's 54,330,939 bytes came down in 8.5 seconds, 6.4 MB/s: a minute is seven times
// that read, and a log at JobLogBound is read inside it at 2.8 MB/s.
const DefaultJobLogTimeout = time.Minute

// JobLogTimeBound is the time bound one of this client's JobLog reads runs under: the client's
// own JobLogTimeout, or DefaultJobLogTimeout where that field is zero, which is every client
// outside a test. A caller writing the bound into a message says it through this, so the message
// names the bound the read was given rather than the one production gives.
func (c Client) JobLogTimeBound() time.Duration {
	if c.JobLogTimeout > 0 {
		return c.JobLogTimeout
	}
	return DefaultJobLogTimeout
}

// jobLogRefusalBody is how much of GitHub's answer JobLog keeps when it refuses the read: enough
// for the message a refusal carries, with no bound of its own to spend on a body GitHub never
// sends.
const jobLogRefusalBody = 64 << 10

// jobLogReadsAtOnce is how many job-log reads this process makes at once, over every issue lane
// together: one place per read in flight, taken before the request is made and given back once
// the body has been read, so a read that waits for a place has allocated no ring at all. One
// read holds a ring of up to JobLogTail, so sixteen is a gigabyte of rings at the worst, an
// eighth of the 8 GiB the daemon pod runs in. The eight a single removal reads at a time
// (ghautomerge's jobLogReads) bounds that one removal and nothing else: without this limit every
// issue lane's removal reads its own eight at once, and the lanes are counted by nobody. Sixteen
// is two such waves, so two removals read at full speed together and a third waits for them
// rather than the pod's memory running out.
const jobLogReadsAtOnce = 16

var jobLogReaders = make(chan struct{}, jobLogReadsAtOnce)

// jobLogRingStart is how long the ring is before a byte of the body has arrived where GitHub
// names no length for it, and jobLogRingLeap how long a ring that is growing gets before it
// takes the whole of JobLogTail at once. A body whose length GitHub names is read into exactly
// that much memory; one that names none — a chunked answer, or a connection the store closes to
// end the body — used to cost the whole 64 MiB tail before its first byte, which is the memory
// of a 54 MB log spent on a log of two lines. Doubling from 64 KiB costs each step a copy of
// what has arrived, which is why the steps stop at a mebibyte: past it the read is plainly a
// long log and takes the tail in one step, so the largest copy a growing ring ever makes is that
// mebibyte and the most it ever holds beside the tail is the same.
const (
	jobLogRingStart = 64 << 10
	jobLogRingLeap  = 1 << 20
)

// The three ways JobLog reads no log that are not a failed fetch, each told apart from the others
// so the caller can say which it was: the body passed JobLogBound, the read passed the client's
// time bound (JobLogTimeBound), and GitHub refused the read, which for this route means the
// installation was never granted GitHub's optional Actions read (a 403) and is a normal
// deployment state rather than a defect.
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
// where a job's log is neither JSON nor bounded. The answer is streamed through a ring of at
// most JobLogTail bytes (readJobLogTail), so a log of any length costs that much memory and no
// more, and those bytes are what the read answers: the caller parses the ring where it lies
// (ghautomerge's failingTests), so a log costs its own length once rather than twice. The read
// gives up when the body passes JobLogBound (ErrJobLogTooLong) or the read passes the client's
// time bound, JobLogTimeBound (ErrJobLogSlow), which bounds the wait for one of this process's
// jobLogReadsAtOnce places as well as the read that follows it. A 403 GitHub's own API host
// answered that is not its rate limit is ErrJobLogForbidden, the grant a deployment may not
// have; one the blob store answers after the redirect is that store refusing the fetch, and an
// *Answer as every other answer other than 2xx is, call's included. A request that never
// arrived is its own error.
func (c Client) JobLog(ctx context.Context, id int64) ([]byte, error) {
	bounded, cancel := context.WithTimeout(ctx, c.JobLogTimeBound())
	defer cancel()
	// A place among the process's reads is taken before the request is made, under the read's own
	// time bound: a read that waited its bound out for one is a read that passed that bound, and
	// it holds no ring while it waits.
	select {
	case jobLogReaders <- struct{}{}:
		defer func() { <-jobLogReaders }()
	case <-bounded.Done():
		return nil, jobLogBound(ctx, bounded, bounded.Err())
	}
	target := fmt.Sprintf("%s/actions/jobs/%d/logs", c.API, id)
	request, err := http.NewRequestWithContext(bounded, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, jobLogBound(ctx, bounded, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, jobLogRefusalBody))
		limited := RateLimited(response, body)
		// A 403 is the grant this installation never had only where GitHub's own API host
		// answered it. The read follows GitHub's redirect to blob storage, and that store
		// answers a 403 of its own for a signature that has expired, which says nothing about
		// any grant: it is a fetch that failed. response.Request is the last request the client
		// made, the redirect's where it followed one, so its host tells the two apart.
		fromAPI := response.Request.URL.Host == request.URL.Host
		if response.StatusCode == http.StatusForbidden && !limited && fromAPI {
			return nil, ErrJobLogForbidden
		}
		return nil, c.refusal(http.MethodGet, request.URL, response, body, limited, BudgetCore)
	}
	switch tail, err := readJobLogTail(response.Body, response.ContentLength); {
	case errors.Is(err, ErrJobLogTooLong):
		return nil, err
	case err != nil:
		return nil, jobLogBound(ctx, bounded, err)
	default:
		return tail, nil
	}
}

// readJobLogTail is the end of body, streamed through a ring of at most JobLogTail bytes: length
// is the body's own length where the answer named one (Content-Length), and zero or less where
// it named none. A named length is the ring's length, held to the tail, and an unnamed one
// starts the ring at jobLogRingStart and grows it as the bytes arrive (growJobLogRing).
//
// Only a ring the answer did not size grows. One the answer sized holds the whole body already,
// and the last of that body need not come with its EOF: over HTTP/2 the EOF arrives on a read
// of its own after the bytes, where HTTP/1.1 ends the last read with it, so a grow there would
// double a ring that is full and right, or take the whole 64 MiB tail once twice its length
// passes the leap.
//
// A body past JobLogBound answers ErrJobLogTooLong; every other failure of a read is answered as
// it came, for the caller to say whose budget ended it (jobLogBound). A body longer than the
// ring wraps it.
func readJobLogTail(body io.Reader, length int64) ([]byte, error) {
	size, grows := jobLogRingStart, true
	if length > 0 {
		size, grows = int(min(length, int64(JobLogTail))), false
	}
	ring := make([]byte, size)
	// The body is streamed through the ring: at is where the next byte goes, and read the whole
	// body's length, which says whether the ring wrapped. A ring shorter than the tail grows
	// instead of wrapping, since the bytes it would overwrite are bytes the tail still holds.
	at, read := 0, 0
	for {
		if at == len(ring) {
			if grows && len(ring) < JobLogTail {
				ring = growJobLogRing(ring)
			} else {
				at = 0
			}
		}
		n, err := body.Read(ring[at:])
		at, read = at+n, read+n
		if read > JobLogBound {
			return nil, ErrJobLogTooLong
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if read <= len(ring) { // the ring holds the whole body, in the order it arrived
		return ring[:read], nil
	}
	// The ring wrapped — more of the body arrived than it holds — so its oldest byte is the one
	// at the write position, and the bytes are put back in order where they lie (rotateJobLogRing)
	// rather than appended to a second buffer of JobLogTail. That oldest byte stands in the middle
	// of a line, and so of a rune: everything before the first line break is the remains of a line
	// that is a line only by accident, and nothing is to parse it.
	if at %= len(ring); at > 0 {
		rotateJobLogRing(ring, at)
	}
	if first := bytes.IndexByte(ring, '\n'); first >= 0 {
		return ring[first+1:], nil
	}
	return ring, nil
}

// growJobLogRing is ring one step longer, with what has arrived in it: twice its length while
// that is under jobLogRingLeap, and the whole of JobLogTail once it is past, which is the last
// step any ring takes. The copy each step makes is what the read pays for not knowing the body's
// length, and the leap is what holds that copy to a mebibyte.
func growJobLogRing(ring []byte) []byte {
	size := 2 * len(ring)
	if size > jobLogRingLeap {
		size = JobLogTail
	}
	grown := make([]byte, size)
	copy(grown, ring)
	return grown
}

// rotateJobLogRing puts the bytes of a wrapped ring back into the order they arrived in, where
// they lie: the oldest stands at at, and the three reversals a rotation is made of move each byte
// twice and allocate nothing, where appending the ring's two halves to a buffer of their own
// would hold a second JobLogTail beside it for as long as the copy ran.
func rotateJobLogRing(ring []byte, at int) {
	slices.Reverse(ring[:at])
	slices.Reverse(ring[at:])
	slices.Reverse(ring)
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
