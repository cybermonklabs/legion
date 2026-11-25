//go:build capture

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// TestCaptureStatusChangeAnswers writes two answers of Dispatch's status-change route that no
// single curl can be made to give to the dispatch client's saved answers
// (internal/dispatch/testdata, whose README names them), each whole in the form `curl --include`
// prints, from the rig's real scratch Dispatch with its parent-status rule on:
//
//   - status_change_midway_refused.http: a call over a parent with 200 open children, refused at
//     a later step because a person wrote the parent between two steps, which answers 409 with the
//     entries of the steps that committed before it, three at least, so the daemon's tests read
//     an entry of each kind its record can stand at. The person's write must land after the third
//     step and before the named issue's, so the capture tries again on new issues until it does.
//   - status_change_keyed_repeat.http: the repeat, under the same request key, of a call whose
//     Dispatch process was killed (SIGKILL) after a step committed, sent to the process started
//     again on the same database once the first run's lease on its key lapsed: the route finishes
//     what the first run left and answers the whole call.
//
// It runs only by hand, on a tree whose Dispatch serves the route (INFRA-4731), and needs the
// Postgres LEGION_TEST_PG_DSN names:
//
//	go -C packages/daemon test -tags capture -count=1 -run '^TestCaptureStatusChangeAnswers$' ./internal/daemon/
func TestCaptureStatusChangeAnswers(t *testing.T) {
	d := startScratchDispatchOn(t, scratchDispatchOptions{env: []string{parentStatusRule + "=on"}})
	if !servesStatusChange(t, d.base) {
		t.Fatal("this tree's Dispatch serves no status-change route: run the capture on a tree with INFRA-4731 merged in")
	}
	createScratchDispatchIssue(t, d.base)
	t.Run("midway refused", func(t *testing.T) { captureMidwayRefusal(t, d) })
	// The keyed repeat kills and starts d, whose helpers fail through the t that started it, so it
	// runs in this test's own body rather than in a subtest.
	captureKeyedRepeat(t, d)
}

// childrenPerCall is how many open children each captured call moves: enough steps that a write
// between two of them, or a kill after one, lands while the call still runs.
const childrenPerCall = 200

func captureMidwayRefusal(t *testing.T, d *scratchDispatch) {
	for attempt := 1; attempt <= 5; attempt++ {
		parent, seq := openChildren(t, d, fmt.Sprintf("midway refusal %d", attempt), childrenPerCall)
		body := backlogWithChildren(t, parent, seq)
		answered := make(chan routeAnswer, 1)
		go func() { answered <- sendStatusChange(d, parent, fmt.Sprintf("midway-refusal-%d", attempt), body) }()
		awaitSteps(t, d, parent, seq, midwaySteps)
		written := asScratchPerson(t, http.MethodPatch, d.base+"/api/v1/issues/"+url.PathEscape(parent),
			map[string]string{"title": "written by a person while the call runs"})
		written.Body.Close()
		if written.StatusCode != http.StatusOK {
			t.Fatalf("a person's write of %s mid-call = %s", parent, written.Status)
		}
		answer := <-answered
		if answer.err != nil {
			t.Fatalf("the call over %s: %v", parent, answer.err)
		}
		if answer.status == http.StatusConflict && len(answer.changed) >= midwaySteps {
			saveAnswer(t, "status_change_midway_refused.http", answer.raw)
			return
		}
		t.Logf("attempt %d answered %d %s with %d committed steps; trying again on new issues",
			attempt, answer.status, answer.code, len(answer.changed))
	}
	t.Fatal("no call was refused midway in five attempts")
}

func captureKeyedRepeat(t *testing.T, d *scratchDispatch) {
	parent, seq := openChildren(t, d, "keyed repeat", childrenPerCall)
	body := backlogWithChildren(t, parent, seq)
	const requestKey = "keyed-repeat-1"
	lost := make(chan routeAnswer, 1)
	go func() { lost <- sendStatusChange(d, parent, requestKey, body) }()
	awaitSteps(t, d, parent, seq, 1)
	d.kill()
	if first := <-lost; first.err == nil {
		t.Fatalf("the killed call answered %d before its process died; nothing was left to finish", first.status)
	}
	d.start()
	// The killed run holds its key until the key's lease lapses (Dispatch's IdempotencyLease, a
	// minute): until then a repeat is answered 409 KEY_IN_FLIGHT and asked again after Retry-After.
	var answer routeAnswer
	if !testwait.Holds(3*time.Minute, time.Second, func() bool {
		answer = sendStatusChange(d, parent, requestKey, body)
		if answer.err != nil {
			t.Fatalf("the repeat over %s: %v", parent, answer.err)
		}
		return answer.status != http.StatusConflict || answer.code != "KEY_IN_FLIGHT"
	}) {
		t.Fatal("the repeat was still told KEY_IN_FLIGHT three minutes after the restart")
	}
	t.Logf("the repeat answered %d %s with %d entries", answer.status, answer.code, len(answer.changed))
	saveAnswer(t, "status_change_keyed_repeat.http", answer.raw)
}

// openChildren is a parent at work with count children at work under it, and the parent's
// sequence once they are.
func openChildren(t *testing.T, d *scratchDispatch, name string, count int) (string, int64) {
	t.Helper()
	ctx := context.Background()
	client := dispatch.New(d.base, dispatch.BearerTransport(d.token, nil))
	parent := createScratchDispatchIssueUnder(t, d.base, name+": the parent", "")
	if _, err := client.SetStatus(ctx, parent, "in_progress"); err != nil {
		t.Fatalf("move %s into work: %v", parent, err)
	}
	for child := 1; child <= count; child++ {
		key := createScratchDispatchIssueUnder(t, d.base, fmt.Sprintf("%s: child %d", name, child), parent)
		if _, err := client.SetStatus(ctx, key, "in_progress"); err != nil {
			t.Fatalf("move %s into work: %v", key, err)
		}
	}
	issue, err := client.GetIssue(ctx, parent)
	if err != nil {
		t.Fatalf("read %s: %v", parent, err)
	}
	return parent, issue.LastSeq
}

// backlogWithChildren is the body the client sends to move parent to the backlog with every issue
// under it, fenced at seq.
func backlogWithChildren(t *testing.T, parent string, seq int64) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"status": "backlog", "expected_sequence": seq, "include_children": true,
		"actor": map[string]string{"kind": "session", "id": dispatch.DaemonSession(parent)},
	})
	if err != nil {
		t.Fatalf("encode the status change: %v", err)
	}
	return body
}

// midwaySteps is how many steps the midway refusal's call commits before the person's write.
const midwaySteps = 3

// awaitSteps waits until steps steps of the call over parent have committed: each child's step
// adds a child.status event to the parent, which moves its sequence past seq, the call's fence.
func awaitSteps(t *testing.T, d *scratchDispatch, parent string, seq int64, steps int) {
	t.Helper()
	client := dispatch.New(d.base, dispatch.BearerTransport(d.token, nil))
	testwait.WithinPolling(t, time.Minute, 5*time.Millisecond, fmt.Sprintf("%d steps of the call over %s to commit", steps, parent), func() bool {
		issue, err := client.GetIssue(context.Background(), parent)
		return err == nil && issue.LastSeq >= seq+int64(steps)
	})
}

// routeAnswer is one answer of the route: whole as it came (raw), its status, and the code and
// the committed entries its body names.
type routeAnswer struct {
	raw     []byte
	status  int
	code    string
	changed []json.RawMessage
	err     error
}

// sendStatusChange posts body to the status-change route of key under requestKey, with the
// headers the client sends, and returns the answer.
func sendStatusChange(d *scratchDispatch, key, requestKey string, body []byte) routeAnswer {
	request, err := http.NewRequest(http.MethodPost, d.base+"/api/v1/issues/"+url.PathEscape(key)+"/status-change", bytes.NewReader(body))
	if err != nil {
		return routeAnswer{err: err}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+d.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", requestKey)
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Do(request)
	if err != nil {
		return routeAnswer{err: err}
	}
	defer response.Body.Close()
	raw, err := httputil.DumpResponse(response, true)
	if err != nil {
		return routeAnswer{err: err}
	}
	var members struct {
		Code    string            `json:"code"`
		Changed []json.RawMessage `json:"changed"`
	}
	if err := json.NewDecoder(response.Body).Decode(&members); err != nil {
		return routeAnswer{err: fmt.Errorf("decode the answer %d: %w", response.StatusCode, err)}
	}
	return routeAnswer{raw: raw, status: response.StatusCode, code: members.Code, changed: members.Changed}
}

// saveAnswer writes raw to the dispatch client's saved answers under name.
func saveAnswer(t *testing.T, name string, raw []byte) {
	t.Helper()
	path := filepath.Join("..", "dispatch", "testdata", name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("save %s: %v", path, err)
	}
	t.Logf("saved %s", path)
}
