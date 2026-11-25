package cipher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestTakesApproverFieldsFollowsTheBrokerWithinItsWindow: TakesApproverFields answers from one
// definite read of the broker's GET /v1/settings (approver_fields true, or settings answered 200
// without it) for ApproverFieldsTTL, on a clock the test holds, and reads again once that window has
// passed, so a broker that starts or stops saying approver_fields true is followed within the
// window and not before. A read that fails is an error, never an answer, even after a read that
// said true, and is not remembered; so is a settings answer that is not a JSON object or whose
// approver_fields is not the literal true or false (null included), and a caller whose context ends
// before the read answers. Each step names the reads it expects, so a window that never lapses, or
// one that lapses early, fails here.
//
// Controls: fresh answering every remembered read as fresh (its window never lapsing) fails the
// first flip: the broker says true, and the client goes on answering the false it read first.
// TakesApproverFields answering a failed read as false, with no error, fails the failed-read step.
// readApproverFields reading approver_fields with json.Unmarshal into a bool fails the null step,
// since that reads null as false with no error.
func TestTakesApproverFieldsFollowsTheBrokerWithinItsWindow(t *testing.T) {
	var (
		mu      sync.Mutex
		says    = `{"secret_type_tag":true}`
		status  = http.StatusOK
		reads   int
		holding chan struct{}
	)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/settings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		reads++
		body, code, hold := says, status, holding
		mu.Unlock()
		if hold != nil {
			<-hold
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	defer broker.Close()
	answer := func(body string, code int) {
		mu.Lock()
		defer mu.Unlock()
		says, status = body, code
	}
	readCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return reads
	}

	client := New(broker.URL, tokenFile(t, "account-token"))
	start := time.Date(2026, 11, 22, 19, 0, 0, 0, time.UTC)
	now := start
	client.approverFields.now = func() time.Time { return now }
	ctx := context.Background()
	step := func(at time.Duration, want bool, wantReads int, what string) {
		t.Helper()
		now = start.Add(at)
		if got, err := client.TakesApproverFields(ctx); err != nil || got != want {
			t.Fatalf("%s (at +%s): TakesApproverFields = %t, %v; want %t, no error", what, at, got, err, want)
		}
		if got := readCount(); got != wantReads {
			t.Fatalf("%s (at +%s): the broker's settings were read %d times, want %d", what, at, got, wantReads)
		}
	}
	unread := func(at time.Duration, wantReads int, what string) {
		t.Helper()
		now = start.Add(at)
		if got, err := client.TakesApproverFields(ctx); err == nil {
			t.Fatalf("%s (at +%s): TakesApproverFields = %t with no error, want an error: the settings could not be read", what, at, got)
		}
		if got := readCount(); got != wantReads {
			t.Fatalf("%s (at +%s): the broker's settings were read %d times, want %d", what, at, got, wantReads)
		}
	}

	step(0, false, 1, "a broker whose settings name no approver_fields")
	answer(`{"secret_type_tag":true,"approver_fields":true}`, http.StatusOK)
	step(ApproverFieldsTTL-time.Nanosecond, false, 1, "the broker now says approver_fields, inside the first read's window")
	step(ApproverFieldsTTL, true, 2, "the broker now says approver_fields, at the end of the first read's window")
	answer(`{"secret_type_tag":true}`, http.StatusOK)
	step(2*ApproverFieldsTTL-time.Nanosecond, true, 2, "the broker no longer says approver_fields, inside the second read's window")
	step(2*ApproverFieldsTTL, false, 3, "the broker no longer says approver_fields, at the end of the second read's window")

	answer(`{"secret_type_tag":true,"approver_fields":true}`, http.StatusOK)
	step(3*ApproverFieldsTTL, true, 4, "the broker says approver_fields again")
	answer(`{"code":"INTERNAL","error":"down"}`, http.StatusInternalServerError)
	unread(4*ApproverFieldsTTL, 5, "a failed read after one that said true")
	unread(4*ApproverFieldsTTL, 6, "a failed read is not remembered: the next ask reads again")
	answer(`<html>default backend</html>`, http.StatusOK)
	unread(4*ApproverFieldsTTL, 7, "a 200 that is no JSON object")
	answer(`null`, http.StatusOK)
	unread(4*ApproverFieldsTTL, 8, "a 200 that is null")
	answer(`{"approver_fields":"yes"}`, http.StatusOK)
	unread(4*ApproverFieldsTTL, 9, "an approver_fields that is no boolean")
	answer(`{"approver_fields":null}`, http.StatusOK)
	unread(4*ApproverFieldsTTL, 10, "an approver_fields that is null")

	// A caller whose context ends before the read answers is answered an error, and the read goes
	// on: the next caller joins it, or finds what it remembered, and is answered the broker's true
	// with no read of its own.
	answer(`{"secret_type_tag":true,"approver_fields":true}`, http.StatusOK)
	release := make(chan struct{})
	mu.Lock()
	holding = release
	mu.Unlock()
	now = start.Add(5 * ApproverFieldsTTL)
	ended, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := client.TakesApproverFields(ended); err == nil {
		t.Fatalf("a caller whose context ended before the settings read answered was answered %t with no error, want an error", got)
	}
	mu.Lock()
	holding = nil
	mu.Unlock()
	close(release)
	// The clock stays where it is: the read left running reads it.
	if got, err := client.TakesApproverFields(ctx); err != nil || !got {
		t.Fatalf("the next caller, after the ended caller's read was let answer, was answered %t, %v; want the broker's true", got, err)
	}
	if got := readCount(); got != 11 {
		t.Fatalf("the next caller made the broker's settings be read %d times in all, want 11: it joins the read left running, or finds what that read remembered", got)
	}
}
