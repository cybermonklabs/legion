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

// JobLogTail is how much of an Actions job's log JobLog keeps: its end, where a test runner prints
// what failed. 256 KiB holds the measured worst case twelve times over: this repository's browser
// suite over 1,536 tests ends in a summary block of 21,517 bytes.
const JobLogTail = 256 << 10

// JobLogBound is how far into a job's log JobLog reads before it gives up: a log longer than this
// is one whose end is not worth the merge row's budget. The measured worst case, this
// repository's browser suite over 1,536 tests in 24 minutes, is 448,490 bytes in 4,307 lines, so
// the bound is 37 times it.
const JobLogBound = 16 << 20

// JobLogTimeout is how long JobLog reads for before it gives up. The row that reads a failing
// job's log has a budget of 30 seconds for all of its GitHub calls, so one log is given a third of
// it: a read this bound ends tells the caller it could not read the log, and holds up no merge.
const JobLogTimeout = 10 * time.Second

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
// another host, which is what the pre-signed blob URL wants. No byte range is asked for: measured
// on this route, `gh api -H "Range: bytes=-65536"` answered 0 bytes, so a range read is unproven
// here and the whole answer is streamed instead.
//
// This is a call shape of its own beside call: call reads a whole body and parses it as JSON,
// where a job's log is neither JSON nor bounded. The answer is streamed through a ring of
// JobLogTail bytes, so a log of any length costs that much memory and no more, and the read gives
// up when the body passes JobLogBound (ErrJobLogTooLong) or the read passes JobLogTimeout
// (ErrJobLogSlow). A 403 that is not GitHub's rate limit is ErrJobLogForbidden, the grant a
// deployment may not have; every other answer other than 2xx is an *Answer, as call's is, and a
// request that never arrived is its own error. A tail the ring cut into starts after its first
// line break, since the line the cut fell in is a line only by accident.
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
	ring, at, wrapped, read := make([]byte, JobLogTail), 0, false, 0
	for {
		if at == len(ring) {
			at, wrapped = 0, true
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
	if !wrapped {
		return string(ring[:at]), nil
	}
	tail := make([]byte, 0, len(ring))
	tail = append(tail, ring[at:]...)
	tail = append(tail, ring[:at]...)
	if cut := bytes.IndexByte(tail, '\n'); cut >= 0 {
		tail = tail[cut+1:]
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
