package identity

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/identity/directorytest"
)

// The signer, the delegated Workspace user and the people of the Directory tests.
const (
	directorySigner = "dispatch-directory@example-project.iam.gserviceaccount.com"
	directoryAdmin  = "directory-reader@example.com"
	directoryAda    = "ada@example.com"
	directoryBob    = "bob@example.com"
)

// newDirectoryRig is a DirectoryReader for directorySigner and directoryAdmin, which reaches the
// fake Google hosts and nothing else.
func newDirectoryRig(t *testing.T) (*directorytest.Google, *DirectoryReader) {
	t.Helper()
	google := directorytest.ServeGoogle(t)
	reader, err := NewDirectoryReader(t.Context(), directorySigner, directoryAdmin, google.ClientOptions()...)
	if err != nil {
		t.Fatalf("NewDirectoryReader: %v", err)
	}
	return google, reader
}

// checked is what one Check returned, and how long it took.
type checked struct {
	outcome DirectoryOutcome
	userID  string
	err     error
	took    time.Duration
}

// check runs one Check within timeout, and fails the test when Check outlives its context by more
// than the slack a loaded machine needs, so a check that does not honour its context fails here
// rather than hanging the run.
func check(t *testing.T, reader *DirectoryReader, ctx context.Context, timeout time.Duration, email, knownID string) checked {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan checked, 1)
	started := time.Now()
	go func() {
		outcome, userID, err := reader.Check(ctx, email, knownID)
		done <- checked{outcome: outcome, userID: userID, err: err, took: time.Since(started)}
	}()
	select {
	case result := <-done:
		return result
	case <-time.After(timeout + 10*time.Second):
		t.Fatalf("Check(%q, %q) still running %s after its %s context ended", email, knownID, 10*time.Second, timeout)
		return checked{}
	}
}

// failedReason is the reason of a Failed check's error, after holding the error to the rule that
// it names a class and an HTTP status only: no word Google wrote, and no address.
func failedReason(t *testing.T, result checked) string {
	t.Helper()
	if result.outcome != DirectoryFailed || result.userID != "" || result.err == nil {
		t.Fatalf("Check = %v, %q, %v; want Failed, no id, and an error", result.outcome, result.userID, result.err)
	}
	for _, word := range []string{directorytest.Planted, directoryAda, "ya29", "googleapis.com"} {
		if strings.Contains(result.err.Error(), word) {
			t.Errorf("the error %q holds %q", result.err, word)
		}
	}
	var failed *DirectoryError
	if !errors.As(result.err, &failed) {
		t.Fatalf("the error %q is no *DirectoryError", result.err)
	}
	return failed.Reason()
}

// TestIAPDirectoryOutcomes holds each answer Google can give a Directory check to its outcome: an
// active answer for the person's own account is Active, a suspended or missing user and an active
// answer for another account at another address (an alias of a colleague's account) are Held, an
// active answer for the asked address itself under another account than the stored one (the
// address deleted and created again) is Recreated, and everything else is Failed with an error
// that names its class and HTTP status only. Each step of the check - IAM's signJwt, the token
// exchange and the Directory read - ends when the check's context ends, the two the impersonate
// package sends included.
func TestIAPDirectoryOutcomes(t *testing.T) {
	stalled := directorytest.Answer{Stall: true}
	rows := []struct {
		name    string
		answer  func(*directorytest.Google)
		knownID string
		timeout time.Duration
		// outcome and userID are a row's answer when reason is empty; reason is a Failed row's.
		outcome DirectoryOutcome
		userID  string
		reason  string
	}{
		{name: "active, first answer", outcome: DirectoryActive, userID: "u-ada", answer: func(g *directorytest.Google) {
			g.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda})
		}},
		{name: "active, the stored id", knownID: "u-ada", outcome: DirectoryActive, userID: "u-ada", answer: func(g *directorytest.Google) {
			g.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda})
		}},
		{name: "suspended", knownID: "u-ada", outcome: DirectoryHeld, userID: "u-ada", answer: func(g *directorytest.Google) {
			g.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda, Suspended: true})
		}},
		{name: "404, a deleted or missing user", knownID: "u-ada", outcome: DirectoryHeld, answer: func(*directorytest.Google) {}},
		{name: "the address made an alias of a colleague's active account", knownID: "u-ada", outcome: DirectoryHeld, userID: "u-bob", answer: func(g *directorytest.Google) {
			g.SetUser(directoryAda, directorytest.User{ID: "u-bob", PrimaryEmail: directoryBob})
		}},
		{name: "the address made an alias of a colleague's active account, no stored id", outcome: DirectoryHeld, userID: "u-bob", answer: func(g *directorytest.Google) {
			g.SetUser(directoryAda, directorytest.User{ID: "u-bob", PrimaryEmail: directoryBob})
		}},
		{name: "a primaryEmail that differs only in letter case, no stored id", outcome: DirectoryActive, userID: "u-ada", answer: func(g *directorytest.Google) {
			g.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: "Ada@Example.COM"})
		}},
		{name: "a renamed person, the stored id", knownID: "u-ada", outcome: DirectoryActive, userID: "u-ada", answer: func(g *directorytest.Google) {
			g.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: "ada.lovelace@example.com"})
		}},
		{name: "the stored address answered with another id, deleted and created again", knownID: "u-ada", outcome: DirectoryRecreated, userID: "u-ada-2", answer: func(g *directorytest.Google) {
			g.SetUser(directoryAda, directorytest.User{ID: "u-ada-2", PrimaryEmail: directoryAda})
		}},
		{name: "the stored address answered with another id in other letter case, deleted and created again", knownID: "u-ada", outcome: DirectoryRecreated, userID: "u-ada-2", answer: func(g *directorytest.Google) {
			g.SetUser(directoryAda, directorytest.User{ID: "u-ada-2", PrimaryEmail: "ADA@example.com"})
		}},
		{name: "a 200 with no suspended", reason: "the Directory read's answer has no suspended (HTTP 200)", answer: func(g *directorytest.Google) {
			g.AnswerUser(directoryAda, directorytest.Answer{Status: http.StatusOK, Body: `{"id":"u-ada","primaryEmail":"ada@example.com","etag":"` + directorytest.Planted + `"}`})
		}},
		{name: "a 200 whose suspended is null", reason: "the Directory read's answer has no suspended (HTTP 200)", answer: func(g *directorytest.Google) {
			g.AnswerUser(directoryAda, directorytest.Answer{Status: http.StatusOK, Body: `{"id":"u-ada","primaryEmail":"ada@example.com","suspended":null}`})
		}},
		{name: "a 200 with no id", reason: "the Directory read's answer has no id (HTTP 200)", answer: func(g *directorytest.Google) {
			g.AnswerUser(directoryAda, directorytest.Answer{Status: http.StatusOK, Body: `{"primaryEmail":"ada@example.com","suspended":false,"etag":"` + directorytest.Planted + `"}`})
		}},
		{name: "401", reason: "the Directory read answered an error (HTTP 401)", answer: directoryRefusal(http.StatusUnauthorized)},
		{name: "403", reason: "the Directory read answered an error (HTTP 403)", answer: directoryRefusal(http.StatusForbidden)},
		{name: "429", reason: "the Directory read answered an error (HTTP 429)", answer: directoryRefusal(http.StatusTooManyRequests)},
		{name: "500", reason: "the Directory read answered an error (HTTP 500)", answer: directoryRefusal(http.StatusInternalServerError)},
		{name: "a body that does not decode", reason: "the Directory read's answer did not decode (HTTP 200)", answer: func(g *directorytest.Google) {
			g.AnswerUser(directoryAda, directorytest.Answer{Status: http.StatusOK, Body: "<html>" + directorytest.Planted + "</html>"})
		}},
		{name: "a reply later than the context", timeout: 300 * time.Millisecond, reason: "the Directory read did not answer before the check's context ended", answer: func(g *directorytest.Google) {
			g.AnswerUser(directoryAda, stalled)
		}},
		{name: "a signer that does not answer before the context ends", timeout: 300 * time.Millisecond, reason: "signJwt did not answer before the check's context ended", answer: func(g *directorytest.Google) {
			g.AnswerSigner(&stalled)
		}},
		{name: "a token endpoint that does not answer before the context ends", timeout: 300 * time.Millisecond, reason: "the token exchange did not answer before the check's context ended", answer: func(g *directorytest.Google) {
			g.AnswerScope(directorytest.DirectoryScope, &stalled)
		}},
		{name: "a signJwt refusal", reason: "signJwt answered an error (HTTP 403)", answer: func(g *directorytest.Google) {
			g.AnswerSigner(&directorytest.Answer{Status: http.StatusForbidden, Body: `{"error":{"code":403,"message":"Permission 'iam.serviceAccounts.signJwt' denied ` + directorytest.Planted + `"}}`})
		}},
		{name: "a signJwt answer that does not decode", reason: "signJwt's answer did not decode (HTTP 200)", answer: func(g *directorytest.Google) {
			g.AnswerSigner(&directorytest.Answer{Status: http.StatusOK, Body: directorytest.Planted})
		}},
		{name: "a token-exchange refusal", reason: "the token exchange answered an error (HTTP 400)", answer: func(g *directorytest.Google) {
			g.AnswerScope(directorytest.DirectoryScope, &directorytest.Answer{Status: http.StatusBadRequest, Body: `{"error":"invalid_grant","error_description":"` + directorytest.Planted + `"}`})
		}},
		{name: "a token exchange that grants no access token", reason: "the token exchange's answer held no access token (HTTP 200)", answer: func(g *directorytest.Google) {
			g.AnswerScope(directorytest.DirectoryScope, &directorytest.Answer{Status: http.StatusOK, Body: `{"token_type":"Bearer","expires_in":3600,"scope":"` + directorytest.Planted + `"}`})
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			google, reader := newDirectoryRig(t)
			row.answer(google)
			timeout := row.timeout
			if timeout == 0 {
				timeout = DirectoryFlight
			}
			result := check(t, reader, t.Context(), timeout, directoryAda, row.knownID)
			if row.reason != "" {
				if reason := failedReason(t, result); reason != row.reason {
					t.Errorf("Failed for %q, want %q", reason, row.reason)
				}
				if row.timeout != 0 && result.took > row.timeout+5*time.Second {
					t.Errorf("Check took %s past a %s context", result.took, row.timeout)
				}
			} else if result.outcome != row.outcome || result.userID != row.userID || result.err != nil {
				t.Errorf("Check = %v, %q, %v; want %v, %q, nil", result.outcome, result.userID, result.err, row.outcome, row.userID)
			}
			holdRequestsToTheGrant(t, google)
		})
	}

	t.Run("a minted token serves later checks until it expires", func(t *testing.T) {
		google, reader := newDirectoryRig(t)
		google.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda})
		for range 2 {
			if result := check(t, reader, t.Context(), DirectoryFlight, directoryAda, "u-ada"); result.outcome != DirectoryActive || result.err != nil {
				t.Fatalf("Check = %v, %v; want Active", result.outcome, result.err)
			}
		}
		if signs, reads := len(google.Signs()), len(google.Reads()); signs != 1 || reads != 2 {
			t.Errorf("two checks signed %d JWTs and read the Directory %d times, want 1 and 2", signs, reads)
		}
		holdRequestsToTheGrant(t, google)
	})

	t.Run("a check mints within its own context, after an earlier check's context ended", func(t *testing.T) {
		google, reader := newDirectoryRig(t)
		google.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda})
		// Each token expires as it is issued, so each check mints its own.
		google.TokenLife(0)
		first, endFirst := context.WithCancel(t.Context())
		if result := check(t, reader, first, DirectoryFlight, directoryAda, "u-ada"); result.outcome != DirectoryActive || result.err != nil {
			t.Fatalf("first Check = %v, %v; want Active", result.outcome, result.err)
		}
		endFirst()
		if result := check(t, reader, t.Context(), DirectoryFlight, directoryAda, "u-ada"); result.outcome != DirectoryActive || result.err != nil {
			t.Fatalf("Check after the first check's context ended = %v, %v; want Active", result.outcome, result.err)
		}
		if signs := len(google.Signs()); signs != 2 {
			t.Errorf("two checks with expired tokens signed %d JWTs, want 2", signs)
		}
		holdRequestsToTheGrant(t, google)
	})

	t.Run("a Directory 401 drops the minted token", func(t *testing.T) {
		google, reader := newDirectoryRig(t)
		directoryRefusal(http.StatusUnauthorized)(google)
		if reason := failedReason(t, check(t, reader, t.Context(), DirectoryFlight, directoryAda, "u-ada")); reason != "the Directory read answered an error (HTTP 401)" {
			t.Fatalf("Failed for %q, want the 401", reason)
		}
		google.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda})
		if result := check(t, reader, t.Context(), DirectoryFlight, directoryAda, "u-ada"); result.outcome != DirectoryActive || result.err != nil {
			t.Fatalf("Check after the 401 = %v, %v; want Active", result.outcome, result.err)
		}
		if signs := len(google.Signs()); signs != 2 {
			t.Errorf("a check after a 401 reused the token: %d JWTs signed, want 2", signs)
		}
	})

	t.Run("concurrent checks with no valid token share one signJwt and one exchange", func(t *testing.T) {
		google, reader := newDirectoryRig(t)
		google.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda})
		release := google.HoldSigner()
		defer release()
		const callers = 8
		start := make(chan struct{})
		results := make([]<-chan checked, callers)
		for i := range results {
			ctx, cancel := context.WithTimeout(t.Context(), DirectoryFlight)
			t.Cleanup(cancel)
			results[i] = goCheck(start, reader, ctx)
		}
		close(start)
		waitFor(t, "the first signJwt", func() bool { return len(google.Signs()) >= 1 })
		// While the first is held, a reader that minted for each check sends every other caller's
		// signJwt too; a second gives them the time, and ends early when all of them arrived.
		within(time.Second, func() bool { return len(google.Signs()) == callers })
		release()
		for i, result := range results {
			if r := receive(t, result); r.outcome != DirectoryActive || r.userID != "u-ada" || r.err != nil {
				t.Errorf("caller %d: Check = %v, %q, %v; want Active, u-ada", i, r.outcome, r.userID, r.err)
			}
		}
		if signs, exchanges := len(google.Signs()), len(google.Exchanges()); signs != 1 || exchanges != 1 {
			t.Errorf("%d concurrent checks sent %d signJwt and %d token exchanges, want 1 and 1", callers, signs, exchanges)
		}
		holdRequestsToTheGrant(t, google)
	})

	t.Run("a caller whose context ends during the shared mint fails alone, and a caller still waiting gets the token", func(t *testing.T) {
		google, reader := newDirectoryRig(t)
		google.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda})
		release := google.HoldSigner()
		defer release()
		// The first caller has a check's deadline, which the mint keeps, and hangs up before it.
		hangsUp, hangUp := context.WithTimeout(t.Context(), DirectoryFlight)
		defer hangUp()
		first := goCheck(nil, reader, hangsUp)
		waitFor(t, "the first caller's signJwt", func() bool { return len(google.Signs()) == 1 })
		short, endShort := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer endShort()
		timesOut := goCheck(nil, reader, short)
		long, endLong := context.WithTimeout(t.Context(), DirectoryFlight)
		defer endLong()
		waiting := goCheck(nil, reader, long)
		// Each of the two that end while the mint is held fails on its own context's end.
		r := receive(t, timesOut)
		if reason := failedReason(t, r); reason != "signJwt did not answer before the check's context ended" || !errors.Is(r.err, context.DeadlineExceeded) {
			t.Errorf("the caller whose deadline passed: Failed for %q (%v), want signJwt unanswered on its own deadline", reason, r.err)
		}
		hangUp()
		r = receive(t, first)
		if reason := failedReason(t, r); reason != "signJwt did not answer before the check's context ended" || !errors.Is(r.err, context.Canceled) {
			t.Errorf("the first caller, who hung up: Failed for %q (%v), want signJwt unanswered on its own cancel", reason, r.err)
		}
		release()
		if r := receive(t, waiting); r.outcome != DirectoryActive || r.userID != "u-ada" || r.err != nil {
			t.Errorf("the caller still waiting: Check = %v, %q, %v; want Active, u-ada: another caller's end failed it", r.outcome, r.userID, r.err)
		}
		if signs, exchanges := len(google.Signs()), len(google.Exchanges()); signs != 1 || exchanges != 1 {
			t.Errorf("three checks sent %d signJwt and %d token exchanges, want 1 and 1", signs, exchanges)
		}
	})

	t.Run("the shared mint's requests end at the deadline of the check that started it", func(t *testing.T) {
		for host, stall := range map[string]func(*directorytest.Google){
			directorytest.SignerHost: func(g *directorytest.Google) { g.AnswerSigner(&stalled) },
			directorytest.TokenHost:  func(g *directorytest.Google) { g.AnswerScope(directorytest.DirectoryScope, &stalled) },
		} {
			t.Run(host, func(t *testing.T) {
				google, reader := newDirectoryRig(t)
				google.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda})
				stall(google)
				ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
				defer cancel()
				deadline, _ := ctx.Deadline()
				result := goCheck(nil, reader, ctx)
				// The counter sees the stalled request open before the deadline: an Open that never
				// counted it would read 0 below for any mint.
				waitFor(t, "the stalled request to "+host+" open at the fake", func() bool { return google.Open(host) == 1 })
				if r := receive(t, result); r.err == nil || !errors.Is(r.err, context.DeadlineExceeded) {
					t.Fatalf("Check = %v, %v; want Failed on its own deadline", r.outcome, r.err)
				}
				// A second after the deadline is room for the server to see the client hang up; a
				// mint on a deadline of its own keeps the request open for that deadline's 15 seconds.
				if !within(time.Until(deadline)+time.Second, func() bool { return google.Open(host) == 0 }) {
					t.Errorf("the mint's request to %s was still open %s after the deadline of the check that started it",
						host, time.Since(deadline).Round(time.Millisecond))
				}
			})
		}
	})

	t.Run("a caller with more time than the check that started the shared mint gets its token from a second mint", func(t *testing.T) {
		google, reader := newDirectoryRig(t)
		google.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda})
		google.AnswerSigner(&stalled)
		short, endShort := context.WithTimeout(t.Context(), time.Second)
		defer endShort()
		first := goCheck(nil, reader, short)
		waitFor(t, "the first caller's signJwt", func() bool { return len(google.Signs()) == 1 })
		long, endLong := context.WithTimeout(t.Context(), 5*time.Second)
		defer endLong()
		second := goCheck(nil, reader, long)
		// The stalled signJwt stays stalled; the next one is answered.
		google.AnswerSigner(nil)
		if r := receive(t, first); r.err == nil || !errors.Is(r.err, context.DeadlineExceeded) {
			t.Errorf("the first caller: Check = %v, %v; want Failed on its own deadline", r.outcome, r.err)
		}
		if r := receive(t, second); r.outcome != DirectoryActive || r.userID != "u-ada" || r.err != nil {
			t.Errorf("the caller with more time: Check = %v, %q, %v; want Active from a second mint within its own deadline", r.outcome, r.userID, r.err)
		}
		if signs := len(google.Signs()); signs != 2 {
			t.Errorf("%d signJwt, want 2: the first caller's mint, which ended at its deadline, and the second caller's", signs)
		}
	})

	t.Run("a mint started by a check with no deadline ends at mintTimeout, and that check waits on a new mint", func(t *testing.T) {
		google, reader := newDirectoryRig(t)
		reader.mintTimeout = 300 * time.Millisecond
		google.SetUser(directoryAda, directorytest.User{ID: "u-ada", PrimaryEmail: directoryAda})
		google.AnswerSigner(&stalled)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := goCheck(nil, reader, ctx)
		waitFor(t, "the stalled signJwt", func() bool { return len(google.Signs()) == 1 })
		google.AnswerSigner(nil)
		if r := receive(t, result); r.outcome != DirectoryActive || r.userID != "u-ada" || r.err != nil {
			t.Errorf("Check = %v, %q, %v; want Active from a second mint", r.outcome, r.userID, r.err)
		}
		if signs := len(google.Signs()); signs != 2 {
			t.Errorf("%d signJwt, want 2: the lapsed mint and the one after it", signs)
		}
	})
}

// goCheck checks ada, with the stored id u-ada, under ctx once start closes (at once for a nil
// start), and delivers what Check returned.
func goCheck(start <-chan struct{}, reader *DirectoryReader, ctx context.Context) <-chan checked {
	done := make(chan checked, 1)
	go func() {
		if start != nil {
			<-start
		}
		started := time.Now()
		outcome, userID, err := reader.Check(ctx, directoryAda, "u-ada")
		done <- checked{outcome: outcome, userID: userID, err: err, took: time.Since(started)}
	}()
	return done
}

// receive is what a goCheck delivered, failing the test when it delivers nothing in 20 seconds.
func receive(t *testing.T, result <-chan checked) checked {
	t.Helper()
	select {
	case r := <-result:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("a Check returned nothing within 20 seconds")
		return checked{}
	}
}

// within polls cond every 5 milliseconds for at most d, and answers whether it held.
func within(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// waitFor fails the test when cond does not hold within 10 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if !within(10*time.Second, cond) {
		t.Fatalf("%s did not happen within 10 seconds", what)
	}
}

// directoryRefusal makes users.get answer ada with status and an error body in Google's shape.
func directoryRefusal(status int) func(*directorytest.Google) {
	return func(g *directorytest.Google) {
		g.AnswerUser(directoryAda, directorytest.Answer{Status: status, Body: `{"error":{"code":` + strconv.Itoa(status) + `,"message":"` + directorytest.Planted + `"}}`})
	}
}

// holdRequestsToTheGrant checks every request the reader sent: each signJwt names the signer, the
// delegated user and the Directory scope alone, for Google's token endpoint; each Directory read
// asks for ada with the three fields, under a token for the delegated user and that scope.
func holdRequestsToTheGrant(t *testing.T, google *directorytest.Google) {
	t.Helper()
	want := directorytest.Sign{Principal: directorySigner, Issuer: directorySigner, Subject: directoryAdmin,
		Scope: directorytest.DirectoryScope, Audience: "https://oauth2.googleapis.com/token"}
	for _, sign := range google.Signs() {
		if sign != want {
			t.Errorf("signJwt %+v, want %+v", sign, want)
		}
	}
	for _, read := range google.Reads() {
		if read.UserKey != directoryAda || read.Fields != "id,suspended,primaryEmail" {
			t.Errorf("users.get %+v, want ada with fields id,suspended,primaryEmail", read)
		}
		if read.Scope != "" && (read.Subject != directoryAdmin || read.Scope != directorytest.DirectoryScope) {
			t.Errorf("users.get under a token for %q with %q, want the delegated user and the Directory scope", read.Subject, read.Scope)
		}
	}
}

// TestNewDirectoryReaderRefusesAMissingAccount: a reader with no signer or no delegated user is
// refused when it is built, not at its first check.
func TestNewDirectoryReaderRefusesAMissingAccount(t *testing.T) {
	google := directorytest.ServeGoogle(t)
	for _, accounts := range [][2]string{{"", directoryAdmin}, {directorySigner, ""}} {
		if _, err := NewDirectoryReader(t.Context(), accounts[0], accounts[1], google.ClientOptions()...); err == nil {
			t.Errorf("NewDirectoryReader(%q, %q) built a reader, want a refusal", accounts[0], accounts[1])
		}
	}
}
