// packages/envoy/internal/cipher/helper/login_poll_test.go
//go:build linux

package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/cipheraddr"
	"github.com/sjawhar/envoy/internal/redirects"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestALoginPollSettlesOnTheBrokersRefusal: a poll the broker answers access_denied settles the
// login "denied", and one it answers expired_token "expired", at any status that is a refusal
// (oauthrefusal.Refusal). The real broker answers access_denied 403, as authelia's OAuth library
// does, which is the fake's own status for it; expired_token is 400 there.
func TestALoginPollSettlesOnTheBrokersRefusal(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status  int    // the answer's status in place of the fake's own; 0 keeps it
		outcome string // the device code grant's answer: "denied" (access_denied) or "expired" (expired_token)
	}{
		{0, "denied"},
		{0, "expired"},
		{http.StatusForbidden, "expired"},
		{http.StatusUnauthorized, "denied"},
	} {
		t.Run(fmt.Sprintf("%d %s", c.status, c.outcome), func(t *testing.T) {
			f := newFakeBroker(t)
			b := &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client()}
			t.Cleanup(b.Close)
			f.mu.Lock()
			f.loginOutcome, f.oauthErrorStatus = c.outcome, c.status
			f.mu.Unlock()
			if _, err := b.Login(context.Background(), "host-a"); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return b.LoginStatus().State == c.outcome })
		})
	}
}

// TestALoginPollTheBrokerCouldNotAnswerSettlesNothing: the device code grant settles a login only
// from an answer whose status is a refusal (oauthrefusal.Refusal). A 5xx, a 408 or a 429 whose
// body names access_denied or expired_token is a broker that could not answer, so the login stays
// pending and the helper polls again, 5 s further apart; the same error at the broker's own status
// then settles it, at that next poll.
func TestALoginPollTheBrokerCouldNotAnswerSettlesNothing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status  int
		outcome string // the device code grant's answer: "denied" (access_denied) or "expired" (expired_token)
	}{
		{http.StatusServiceUnavailable, "denied"},
		{http.StatusServiceUnavailable, "expired"},
		{http.StatusInternalServerError, "expired"},
		{http.StatusRequestTimeout, "denied"},
		{http.StatusTooManyRequests, "denied"},
	} {
		t.Run(fmt.Sprintf("%d %s", c.status, c.outcome), func(t *testing.T) {
			f := newFakeBroker(t)
			b := &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client()}
			t.Cleanup(b.Close)
			f.mu.Lock()
			f.loginOutcome, f.oauthErrorStatus = c.outcome, c.status
			f.mu.Unlock()
			if _, err := b.Login(context.Background(), "host-a"); err != nil {
				t.Fatal(err)
			}
			// The first poll is answered at c.status; once it is, every later one is answered at
			// the fake's own status, under the lock the fake answers under.
			if !testwait.Holds(30*time.Second, 5*time.Millisecond, func() bool {
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.devicePolls >= 1 {
					f.oauthErrorStatus = 0
				}
				return f.devicePolls >= 1
			}) {
				t.Fatal("the helper never polled the device code")
			}
			// The second poll comes 1 + 5 s after the first: the broker's interval and the step
			// an outage adds.
			if !testwait.Holds(30*time.Second, 5*time.Millisecond, func() bool { return b.LoginStatus().State == c.outcome }) {
				t.Fatalf("login-status %+v, want %s", b.LoginStatus(), c.outcome)
			}
			// A login settles at most once, and polls no more once it has.
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.devicePolls < 2 {
				t.Fatalf("the login settled %s after %d poll, want a second: the first, answered %d naming its error, settled it", c.outcome, f.devicePolls, c.status)
			}
		})
	}
}

// pollAnswer answers one device code poll of the fake broker (fakeBroker.devicePollAnswer).
type pollAnswer func(w http.ResponseWriter)

// answering is an OAuth error answer of status naming code ("" names none), with header's pairs.
func answering(status int, code string, header ...string) pollAnswer {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		writeJSON(w, status, map[string]string{"error": code})
	}
}

// hangingUp answers a poll with no answer at all: the connection closes once the request is read.
func hangingUp(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		conn.Close()
	}
}

// answeringBody answers a poll with status and body, typed contentType.
func answeringBody(status int, contentType, body string) pollAnswer {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// cutOff answers a poll 200 with a JSON body cut off while it is read: the connection closes
// partway through the body its Content-Length names.
func cutOff(w http.ResponseWriter) {
	conn, sent, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	_, _ = sent.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"access_token\":")
	_ = sent.Flush()
	conn.Close()
}

// pollRig is a Broker logged in against the fake broker, whose first polls are answered in turn
// by answers and every later one by the fake itself (an approval), and whose waits before each
// poll are recorded, not waited (Broker.testLoginWait). lapseAt, when not 0, is the wait that
// lapses the code, after which the helper polls no more; a negative one also leaves the code 2 s
// at the first wait, and lapses it at the wait -lapseAt.
type pollRig struct {
	f       *fakeBroker
	b       *Broker
	started time.Time // read just before the login started
	mu      sync.Mutex
	waits   []time.Duration
	until   []time.Duration // each wait's time left of the code's lifetime, read as it was handed over
}

func newPollRig(t *testing.T, lapseAt int, answers ...pollAnswer) *pollRig {
	t.Helper()
	return newPollRigNaming(t, nil, lapseAt, answers...)
}

// newPollRigNaming is newPollRig whose device authorization answers fields in place of the fake's
// own pairs (fakeBroker.deviceAuthFields).
func newPollRigNaming(t *testing.T, fields map[string]any, lapseAt int, answers ...pollAnswer) *pollRig {
	t.Helper()
	r := &pollRig{f: newFakeBroker(t)}
	r.f.mu.Lock()
	r.f.deviceAuthFields = fields
	r.f.devicePollAnswer = func(poll int, w http.ResponseWriter) bool {
		if poll > len(answers) {
			return false
		}
		answers[poll-1](w)
		return true
	}
	r.f.mu.Unlock()
	r.b = &Broker{URL: cipheraddr.New(r.f.srv.URL), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: r.f.srv.Client(),
		testLoginWait: func(wait time.Duration, da *oauth2.DeviceAuthResponse) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.waits, r.until = append(r.waits, wait), append(r.until, time.Until(da.Expiry))
			if lapseAt < 0 && len(r.waits) == 1 {
				da.Expiry = time.Now().Add(2 * time.Second)
			}
			if len(r.waits) == lapseAt || len(r.waits) == -lapseAt {
				da.Expiry = time.Now().Add(-time.Second)
			}
		}}
	t.Cleanup(r.b.Close)
	r.started = time.Now()
	if _, err := r.b.Login(context.Background(), "host-a"); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *pollRig) seen() ([]time.Duration, []time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.waits), slices.Clone(r.until)
}

func (r *pollRig) polls() int {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return r.f.devicePolls
}

// TestALoginPollGrowsItsIntervalAsRFC8628Has: each poll waits the interval first, the fake's 1 s.
// RFC 8628 section 3.5 grows it by 5 s for that poll and every later one on a slow_down, at
// whatever status its body names it, and on a poll the broker did not answer or answered with an
// outage, once for an answer that is both; a 2xx that holds no token and names no error is no
// token endpoint's answer, so an outage too. authorization_pending, and a refusal naming no
// deciding error, keep it. These are the TypeScript device login's rules.
func TestALoginPollGrowsItsIntervalAsRFC8628Has(t *testing.T) {
	const s = time.Second
	for _, c := range []struct {
		name    string
		answers []pollAnswer // the first polls' answers; the next is the approval
		want    []time.Duration
	}{
		{"no answer, then a 503, then the approval", []pollAnswer{hangingUp, answering(http.StatusServiceUnavailable, "")}, []time.Duration{s, 6 * s, 11 * s}},
		{"a 429 and a 408 naming access_denied, then the approval", []pollAnswer{answering(http.StatusTooManyRequests, "access_denied"), answering(http.StatusRequestTimeout, "access_denied")}, []time.Duration{s, 6 * s, 11 * s}},
		{"a slow_down, a 503, authorization_pending, then the approval", []pollAnswer{answering(http.StatusBadRequest, "slow_down"), answering(http.StatusServiceUnavailable, ""), answering(http.StatusBadRequest, "authorization_pending")}, []time.Duration{s, 6 * s, 11 * s, 11 * s}},
		{"a 503 naming authorization_pending, then the approval", []pollAnswer{answering(http.StatusServiceUnavailable, "authorization_pending")}, []time.Duration{s, 6 * s}},
		{"a 429 naming slow_down, then the approval", []pollAnswer{answering(http.StatusTooManyRequests, "slow_down")}, []time.Duration{s, 6 * s}},
		{"a 200 with no token, then the approval", []pollAnswer{answeringBody(http.StatusOK, "application/json", `{"token_type":"DPoP"}`)}, []time.Duration{s, 6 * s}},
		{"a 200 page, then the approval", []pollAnswer{answeringBody(http.StatusOK, "text/html", "<html>a portal's sign-in page</html>")}, []time.Duration{s, 6 * s}},
		{"a 200 naming slow_down, then the approval", []pollAnswer{answering(http.StatusOK, "slow_down")}, []time.Duration{s, 6 * s}},
		// Controls: authorization_pending, and a refusal naming no deciding error, keep the interval.
		{"a 503, a 401 invalid_client, then the approval", []pollAnswer{answering(http.StatusServiceUnavailable, ""), answering(http.StatusUnauthorized, "invalid_client")}, []time.Duration{s, 6 * s, 6 * s}},
		{"authorization_pending twice, then the approval", []pollAnswer{answering(http.StatusBadRequest, "authorization_pending"), answering(http.StatusBadRequest, "authorization_pending")}, []time.Duration{s, s, s}},
		// A 2xx naming an error is the broker's answer, as x/oauth2 reads it (devicePoll).
		{"a 200 naming authorization_pending, then the approval", []pollAnswer{answering(http.StatusOK, "authorization_pending")}, []time.Duration{s, s}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newPollRig(t, 0, c.answers...)
			waitFor(t, func() bool { return r.b.LoginStatus().State == "issued" })
			if got, _ := r.seen(); !slices.Equal(got, c.want) {
				t.Fatalf("the polls waited %v, want %v", got, c.want)
			}
		})
	}
}

// TestALoginPollWaitsOutAnOutagesRetryAfter: an outage's Retry-After, in seconds or as an HTTP
// date, is waited before the next poll where it asks longer than the interval, though never past
// the code's lifetime. The broker's own 429 on its token endpoint asks 1 s (its token limit
// refills one request every 500 ms, which Retry-After rounds up); a proxy before it may ask
// longer, and 30 s is such a value.
func TestALoginPollWaitsOutAnOutagesRetryAfter(t *testing.T) {
	t.Run("Retry-After: 30 on a 429", func(t *testing.T) {
		r := newPollRig(t, 0, answering(http.StatusTooManyRequests, "invalid_request", "Retry-After", "30"))
		waitFor(t, func() bool { return r.b.LoginStatus().State == "issued" })
		if got, _ := r.seen(); !slices.Equal(got, []time.Duration{time.Second, 30 * time.Second}) {
			t.Fatalf("the polls waited %v, want [1s 30s]", got)
		}
	})
	t.Run("an HTTP date on a 503", func(t *testing.T) {
		at := time.Now().Add(31 * time.Second).Truncate(time.Second)
		r := newPollRig(t, 0, answering(http.StatusServiceUnavailable, "", "Retry-After", at.UTC().Format(http.TimeFormat)))
		waitFor(t, func() bool { return r.b.LoginStatus().State == "issued" })
		got, _ := r.seen()
		// Read between the answer and now, so no clock decides the row.
		if len(got) != 2 || got[1] > 31*time.Second || got[1] < time.Until(at) {
			t.Fatalf("the polls waited %v, want 1 s and then until %s", got, at)
		}
	})
	t.Run("a Retry-After past the code's lifetime", func(t *testing.T) {
		r := newPollRig(t, 0, answering(http.StatusTooManyRequests, "", "Retry-After", "86400"))
		waitFor(t, func() bool { return r.b.LoginStatus().State == "issued" })
		got, until := r.seen()
		// The fake's code lives 900 s; the wait is what was left of it, read before it was handed over.
		if len(got) != 2 || got[1] > 900*time.Second || got[1] < until[1] {
			t.Fatalf("the polls waited %v with %v of the code left, want 1 s and then the code's lifetime", got, until)
		}
	})
}

// TestALoginThatLapsesAfterUnansweredPollsNamesThem: a code whose lifetime passes after polls the
// broker did not answer, or answered with an outage, records "expired" with a Reason naming how
// many came in a row just before; the lapse itself is no poll. An answered poll since the last
// outage, and the lapse names none.
func TestALoginThatLapsesAfterUnansweredPollsNamesThem(t *testing.T) {
	for _, c := range []struct {
		name    string
		answers []pollAnswer // the polls' answers; the wait after them lapses the code
		reason  string
	}{
		{"a 503, then no answer", []pollAnswer{answering(http.StatusServiceUnavailable, ""), hangingUp}, "the broker was unreachable or busy for the last 2 polls"},
		// Each poll counts, and authorization_pending is an answer that ends the run.
		{"a 503, then authorization_pending", []pollAnswer{answering(http.StatusServiceUnavailable, ""), answering(http.StatusBadRequest, "authorization_pending")}, ""},
		{"a 503, authorization_pending, a 503", []pollAnswer{answering(http.StatusServiceUnavailable, ""), answering(http.StatusBadRequest, "authorization_pending"), answering(http.StatusServiceUnavailable, "")}, "the broker was unreachable or busy for the last poll"},
		{"a refusal, then a 429", []pollAnswer{answering(http.StatusUnauthorized, "invalid_client"), answering(http.StatusTooManyRequests, "")}, "the broker was unreachable or busy for the last poll"},
		// A 2xx that holds no token is no token endpoint's answer.
		{"a 200 with no token, then a 200 whose body does not parse", []pollAnswer{answeringBody(http.StatusOK, "application/json", "{}"), answeringBody(http.StatusOK, "application/json", "{")}, "the broker was unreachable or busy for the last 2 polls"},
		// Control: the outage was followed by an answer.
		{"a 503, then a refusal", []pollAnswer{answering(http.StatusServiceUnavailable, ""), answering(http.StatusUnauthorized, "invalid_client")}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newPollRig(t, len(c.answers)+1, c.answers...)
			waitFor(t, func() bool { return r.b.LoginStatus().State == "expired" })
			if got := r.b.LoginStatus().Reason; got != c.reason {
				t.Fatalf("the expired login's reason %q, want %q", got, c.reason)
			}
			if got := r.polls(); got != len(c.answers) {
				t.Fatalf("the helper polled %d times, want %d", got, len(c.answers))
			}
		})
	}
}

// TestALoginPollsWaitEndsAtTheCodesLifetime: no wait passes the code's lifetime. With 2 s of it
// left, a 503 that grows the interval to 6 s waits what is left, and a code that lapses in that
// wait is polled no more and records the outage before it.
func TestALoginPollsWaitEndsAtTheCodesLifetime(t *testing.T) {
	r := newPollRig(t, -2, answering(http.StatusServiceUnavailable, ""))
	waitFor(t, func() bool { return r.b.LoginStatus().State == "expired" })
	got, _ := r.seen()
	if len(got) != 2 || got[0] != time.Second || got[1] > 2*time.Second {
		t.Fatalf("the polls waited %v, want 1 s and then no more than the 2 s left", got)
	}
	if reason := r.b.LoginStatus().Reason; reason != "the broker was unreachable or busy for the last poll" || r.polls() != 1 {
		t.Fatalf("the lapse recorded %q after %d polls, want the last poll named after 1", reason, r.polls())
	}
}

// TestALoginPollHoldsTheBrokersIntervalAndTheCodesLifetime: the poll starts at the broker's
// interval, 1 s at least and 5 s where it names none, and the code lives at most an hour, so long
// where the broker names no lifetime, as the TypeScript device login holds them. A negative
// interval would poll as fast as the broker answers authorization_pending. A longer interval is
// waited whole, as RFC 8628 section 3.5 has it. x/oauth2 reads an interval or a lifetime of 0 as
// none.
func TestALoginPollHoldsTheBrokersIntervalAndTheCodesLifetime(t *testing.T) {
	const s = time.Second
	for _, c := range []struct {
		name     string
		interval any // the device authorization's interval; nil names none
		want     time.Duration
	}{
		{"a negative interval", -3, s},
		{"an interval of 90 s", 90, 90 * s},
		// Controls: 0, none, an interval of a few seconds, and one just inside the fake's 900 s
		// code, which Login still starts (the hook passes no time, so both waits are whole).
		{"an interval of 0", 0, 5 * s},
		{"no interval", nil, 5 * s},
		{"an interval of 30 s", 30, 30 * s},
		{"an interval of 890 s", 890, 890 * s},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newPollRigNaming(t, map[string]any{"interval": c.interval}, 0, answering(http.StatusBadRequest, "authorization_pending"))
			waitFor(t, func() bool { return r.b.LoginStatus().State == "issued" })
			if got, _ := r.seen(); !slices.Equal(got, []time.Duration{c.want, c.want}) {
				t.Fatalf("the polls waited %v, want %v twice", got, c.want)
			}
		})
	}
	for _, c := range []struct {
		name      string
		expiresIn any // the device authorization's expires_in; nil names none
	}{
		{"a lifetime of two hours", 7200},
		{"no lifetime", nil},
		{"a lifetime of 0", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newPollRigNaming(t, map[string]any{"expires_in": c.expiresIn}, 0)
			waitFor(t, func() bool { return r.b.LoginStatus().State == "issued" })
			// The code's end is set between the login's start and its first wait, so what is left
			// of it at that wait is at most the hour, and at least the hour less the time since.
			if _, until := r.seen(); len(until) != 1 || until[0] > time.Hour || until[0] < time.Hour-time.Since(r.started) {
				t.Fatalf("the first wait had %v of the code left, want an hour less the time since the login started", until)
			}
		})
	}
}

// TestALoginWhoseCodeLapsesBeforeItsFirstPollIsRefused: a code that lapses no later than its first
// poll, the broker's interval on, could never be polled, so Login refuses it at once, before any
// code is returned, starts no poll and records no login, rather than wait the code out and record
// it expired; one that had already lapsed is refused so too. The line names the interval the
// broker named, or none, with the one used where the bounds set it, and what ends the code
// first: the broker's lifetime, the hour this machine polls a code at most where the broker names
// a longer lifetime or none, or the answer's round trip, which x/oauth2 takes off the lifetime.
// It names what must change at the broker, worded from that cause, not asking again, which would
// meet the same answer, but where the round trip ended the code, which a faster answer cures.
func TestALoginWhoseCodeLapsesBeforeItsFirstPollIsRefused(t *testing.T) {
	const lapses = `: the code would lapse before its first poll; no machine login works at that broker until `
	const lapsed = `a device code that had lapsed by the time its answer arrived \(a negative lifetime, or one this machine's OAuth library wraps into the past, as it does one of about 292 to 584 years\), with `
	const unwrapped = `, and this machine's OAuth library does not wrap their lifetime into the past, as it wraps none under about 292 years`
	// What a login needs once the codes' lifetime outlasts the interval, said of every line that
	// names a lifetime the interval is no shorter than: no cure by either alone, for a lifetime
	// x/oauth2 wraps forward keeps a fraction the helper cannot tell from a pause.
	const outlive = `, and then only once its codes outlive its poll interval by more than its answers take to arrive`
	const longer = `a poll interval of 1 s; no machine login works at that broker until its codes live longer than 1 s, the least poll interval this machine keeps` + unwrapped
	const hour = `, and this machine polls a device code for an hour at most` + lapses + `its poll interval is shorter than an hour`
	for _, c := range []struct {
		name   string
		fields map[string]any
		trip   time.Duration // the round trip Login measures: it asks at t0 and reads the clock at t0 plus this after the answer
		answer string        // what the line says the broker answered and what must change, a regular expression
	}{
		{"an interval as long as the code", map[string]any{"interval": 900}, 0, `a device code that lives 900 s, with a poll interval of 900 s` + lapses + `its poll interval is shorter than its codes' lifetime` + outlive},
		{"an interval of a day", map[string]any{"interval": 86400}, 0, `a device code that lives 900 s, with a poll interval of 86400 s, so one of 3600 s` + lapses + `its poll interval is shorter than its codes' lifetime` + outlive},
		{"an interval past what a time.Duration holds", map[string]any{"interval": int64(1e12)}, 0, `a device code that lives 900 s, with a poll interval of 1000000000000 s, so one of 3600 s` + lapses + `its poll interval is shorter than its codes' lifetime` + outlive},
		{"no interval and a code of 5 s", map[string]any{"interval": nil, "expires_in": 5}, 0, `a device code that lives 5 s, with no poll interval or one of 0 s, so one of 5 s` + lapses + `it names a poll interval shorter than its codes' lifetime, or its codes live longer than 5 s` + outlive},
		{"no interval and a code of 1 s", map[string]any{"interval": nil, "expires_in": 1}, 0, `a device code that lives 1 s, with no poll interval or one of 0 s, so one of 5 s` + lapses + `its codes live longer than 5 s, or than both 1 s and a poll interval it names` + outlive},
		// x/oauth2 reads an interval of 0 as none.
		{"an interval of 0 and a code of 3 s", map[string]any{"interval": 0, "expires_in": 3}, 0, `a device code that lives 3 s, with no poll interval or one of 0 s, so one of 5 s` + lapses + `it names a poll interval shorter than its codes' lifetime, or its codes live longer than 5 s` + outlive},
		// The broker's interval is already shorter than its code: only a longer lifetime helps.
		{"a negative interval and a code of 1 s", map[string]any{"interval": -3, "expires_in": 1}, 0, `a device code that lives 1 s, with a poll interval of -3 s, so one of 1 s` + lapses + `its codes live longer than 1 s, the least poll interval this machine keeps` + outlive},
		// An interval of 1 s named is the least too: no shorter one the broker names is polled.
		{"an interval of 1 s and a code of 1 s", map[string]any{"interval": 1, "expires_in": 1}, 0, `a device code that lives 1 s, with a poll interval of 1 s` + lapses + `its codes live longer than 1 s, the least poll interval this machine keeps` + outlive},
		// The answer outlasted the code: it had lapsed, not "would lapse", and a login works only
		// once the codes outlive the interval by more than the answers take; codes that outlive only
		// the answers are refused too, as 6 s codes answered in 1.5 s against 5 s are.
		{"a round trip longer than the code", map[string]any{"interval": nil, "expires_in": 1}, 1500 * time.Millisecond, `a device code that lives 1 s, with no poll interval or one of 0 s, so one of 5 s, but its answer took \d+\.\d{3} s to arrive, so the code had lapsed; no machine login works at that broker until its codes live longer than 5 s, or than both 1 s and a poll interval it names` + outlive},
		{"codes that outlive only their answers", map[string]any{"interval": nil, "expires_in": 6}, 1500 * time.Millisecond, `a device code that lives 6 s, with no poll interval or one of 0 s, so one of 5 s, but its answer took \d+\.\d{3} s to arrive, which left \d\.\d{3} s of it: the code would lapse before its first poll; a machine login works at that broker where its answer arrives in less than 1 s, or once its codes outlive its poll interval by more than its answers take to arrive`},
		{"a code of two hours and an interval past the hour", map[string]any{"interval": 5000, "expires_in": 7200}, 0, `a poll interval of 5000 s, so one of 3600 s` + hour},
		{"no lifetime and an interval of an hour", map[string]any{"interval": 3600, "expires_in": nil}, 0, `a poll interval of 3600 s` + hour},
		// x/oauth2 reads a lifetime of 0 as none, so the hour ends the code, where the TypeScript
		// device login refuses it as a code already lapsed.
		{"a lifetime of 0 and an interval of an hour", map[string]any{"interval": 3600, "expires_in": 0}, 0, `a poll interval of 3600 s` + hour},
		{"a code that had already lapsed", map[string]any{"expires_in": -5}, 0, lapsed + longer},
		// 1e10 s is past what a time.Duration holds: x/oauth2 wraps it to a time 268 years past.
		{"a lifetime past what a time.Duration holds", map[string]any{"expires_in": int64(1e10)}, 0, lapsed + longer},
		// 18446744974 s is past 2^64 ns: x/oauth2 wraps it forward to about 900 s, which the line
		// names as the broker's, where the TypeScript device login holds it to the hour and polls.
		{"a lifetime x/oauth2 wraps forward under the interval", map[string]any{"interval": 1000, "expires_in": int64(18446744974)}, 0, `a device code that lives 90[01] s, with a poll interval of 1000 s` + lapses + `its poll interval is shorter than its codes' lifetime` + outlive},
		// 18446744074 s wraps forward to about 0.29 s: a code that had not passed, so no lapsed one,
		// but one of 0 s in whole seconds, which a slow answer may outlast.
		{"a lifetime x/oauth2 wraps forward to under a second", map[string]any{"expires_in": int64(18446744074)}, 0, `a device code that lives [01] s, with a poll interval of 1 s(` + lapses + `its codes live longer than 1 s, the least poll interval this machine keeps` + outlive + `|, but its answer took \d+\.\d{3} s to arrive, so the code had lapsed; no machine login works at that broker until its codes live longer than 1 s, the least poll interval this machine keeps` + outlive + `)`},
		// 18446744075 s, which x/oauth2 wraps forward to about 1.29 s, is a subtest of its own below:
		// its round trip is handed to Login, not waited for.
		// An interval of the hour or more cannot be polled whatever the lifetime: the hour must go.
		{"a code that had already lapsed and an interval past the hour", map[string]any{"interval": 5000, "expires_in": -5}, 0, lapsed + `a poll interval of 5000 s, so one of 3600 s; no machine login works at that broker until its poll interval is shorter than an hour and its codes live longer than it` + unwrapped},
		// A slow answer leaves less than the hour of a code that lives past it, but no faster answer
		// cures an interval of the hour: the hour ends the code.
		{"a round trip under a code past the hour and an interval past it", map[string]any{"interval": 7200, "expires_in": 3601}, 1500 * time.Millisecond, `a poll interval of 7200 s, so one of 3600 s` + hour},
		// The broker's numbers leave the code a second past its first poll, which a slow answer
		// takes: a faster one cures it.
		{"a round trip that takes the code's margin", map[string]any{"interval": 1, "expires_in": 2}, 1500 * time.Millisecond, `a device code that lives 2 s, with a poll interval of 1 s, but its answer took \d+\.\d{3} s to arrive, which left (0\.\d{3} s of it: the code would lapse before its first poll|none of it, so the code had lapsed); a machine login works at that broker where its answer arrives in less than 1 s, or once its codes outlive its poll interval by more than its answers take to arrive`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeBroker(t)
			f.mu.Lock()
			f.deviceAuthFields = c.fields
			f.mu.Unlock()
			b := &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client()}
			t.Cleanup(b.Close)
			if c.trip > 0 {
				// The rows' lines name the round trip the row says, not the time the runner took.
				// Login's first reading (so nothing Login does before it counts) is dated slack
				// early. That trades one window for another; it does not remove the real clock,
				// since x/oauth2 still dates the code's end from its own two readings of it
				// (deviceauth.go:73 and :162). A stall between those two readings shortens the
				// code's end: with no slack, 2 ms of it made "less than 1 s" read "less than 0.998
				// s"; with the slack, the rows fail only past about 250 ms (dating Login's reading
				// 200 ms later passes, 300 ms fails). A pause between Login's first reading and
				// x/oauth2's start lengthens it, and the lifetime rounds to whole seconds: with no
				// slack, 400 ms passed; with the slack, the rows fail past about 250 ms (a sleep
				// after that reading: 200 ms passes, 300 ms fails). Each window is microseconds of
				// work, so neither stall is likely, but either still flips a row.
				const slack = 250 * time.Millisecond
				var t0 time.Time
				reads := 0
				b.testLoginNow = func() time.Time {
					reads++
					if reads == 1 {
						t0 = time.Now().Add(-slack)
						return t0
					}
					return t0.Add(c.trip)
				}
			}
			code, err := b.Login(context.Background(), "host-a")
			if err == nil {
				t.Fatalf("Login answered the code %q, want a refusal", code)
			}
			if want := regexp.MustCompile(`^the broker answered ` + c.answer + `$`); !want.MatchString(err.Error()) {
				t.Fatalf("Login refused with %q, want it to match %s", err, want)
			}
			// Login records the login's state before it starts a poll, so no state is no poll.
			if state := b.LoginStatus().State; state != "" {
				t.Fatalf("the refused login recorded the state %q, want none", state)
			}
		})
	}
	// Control: a lifetime of 0 is none here, so a code with the fake's 1 s interval is polled for
	// the hour, not refused as the TypeScript device login refuses it.
	t.Run("a lifetime of 0 and an interval of 1 s", func(t *testing.T) {
		f := newFakeBroker(t)
		f.mu.Lock()
		f.deviceAuthFields = map[string]any{"expires_in": 0}
		f.mu.Unlock()
		b := &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client()}
		t.Cleanup(b.Close)
		if code, err := b.Login(context.Background(), "host-a"); err != nil || code == "" {
			t.Fatalf("Login answered %q, %v, want a code and no refusal", code, err)
		}
	})
	// 18446744075 s wraps forward to about 1.29 s. The round trip Login measures is the one the
	// subtest hands it (testLoginNow: Login asks at its first clock reading, t0, and reads the
	// clock at t0 plus the round trip after the answer), not the time the runner took. A round trip
	// of 0.5 s leaves the 1 s interval 0.79 s of the code: refused, reading "lives 1 s", and its
	// cure is no lifetime the code already has; answered at once the code leaves it 1.29 s and is
	// polled. The code's end is still x/oauth2's own reading, microseconds after t0, so the fast
	// row is polled whatever that gap is, and the slow row is refused unless the gap passes 0.21 s.
	wrapped := func(t *testing.T, roundTrip time.Duration) (string, error) {
		t.Helper()
		f := newFakeBroker(t)
		f.mu.Lock()
		f.deviceAuthFields = map[string]any{"expires_in": int64(18446744075)}
		f.mu.Unlock()
		b := &Broker{URL: cipheraddr.New(f.srv.URL), OperatorFile: operatorFile(t, "ada@example.com"), MachineFile: machineFilePath(t), HTTP: f.srv.Client()}
		t.Cleanup(b.Close)
		var t0 time.Time
		reads := 0
		b.testLoginNow = func() time.Time {
			reads++
			if reads == 1 {
				t0 = time.Now()
				return t0
			}
			return t0.Add(roundTrip)
		}
		return b.Login(context.Background(), "host-a")
	}
	t.Run("a lifetime x/oauth2 wraps forward to over a second, answered slowly", func(t *testing.T) {
		code, err := wrapped(t, 500*time.Millisecond)
		if err == nil {
			t.Fatalf("Login answered the code %q, want a refusal", code)
		}
		want := regexp.MustCompile(`^the broker answered a device code that lives 1 s, with a poll interval of 1 s` + lapses + `its codes live longer than 1 s, the least poll interval this machine keeps` + outlive + `$`)
		if !want.MatchString(err.Error()) {
			t.Fatalf("Login refused with %q, want it to match %s", err, want)
		}
	})
	t.Run("a lifetime x/oauth2 wraps forward to over a second, answered at once", func(t *testing.T) {
		if code, err := wrapped(t, 0); err != nil || code == "" {
			t.Fatalf("Login answered %q, %v, want a code and no refusal", code, err)
		}
	})
}

// TestTheRefusalReadsTheBrokersWholeSecondsThoughAClockReadingIsLate: x/oauth2 dates a code's end
// from its own reading of the clock as the answer is parsed (deviceauth.go:73), less the time since
// its request's start, read later (:162); Login reads its own start before x/oauth2's. A pause
// between x/oauth2's two readings, as a preemption or a collection gives, leaves the time Login
// measures short of the broker's whole seconds, and a pause before x/oauth2's start leaves it
// over them. Neither, a few hundred milliseconds long, changes the lifetime the line names, nor
// which cause it names: the round trip, where the broker's numbers alone leave a poll. The time
// an answer must beat, where it is under the whole seconds, follows what the code's end leaves
// (the pause between the readings is part of the shortfall), and a shortfall of a millisecond or
// less keeps the whole seconds: the last two rows hold that edge.
func TestTheRefusalReadsTheBrokersWholeSecondsThoughAClockReadingIsLate(t *testing.T) {
	const s, pause = time.Second, 300 * time.Millisecond
	for _, c := range []struct {
		name        string
		lived, left time.Duration // the code's end less Login's start, and less now
		interval    int64
		answer      string
	}{
		{"a pause between x/oauth2's readings", 900*s - pause, 900*s - pause - 5*time.Millisecond, 900, `a device code that lives 900 s, with a poll interval of 900 s: the code would lapse before its first poll; no machine login works at that broker until its poll interval is shorter than its codes' lifetime, and then only once its codes outlive its poll interval by more than its answers take to arrive`},
		{"a pause before x/oauth2's start", 900*s + pause, 900*s - 5*time.Millisecond, 900, `a device code that lives 900 s, with a poll interval of 900 s: the code would lapse before its first poll; no machine login works at that broker until its poll interval is shorter than its codes' lifetime, and then only once its codes outlive its poll interval by more than its answers take to arrive`},
		{"a pause between x/oauth2's readings, the round trip taking the margin", 2*s - pause, 200 * time.Millisecond, 1, `a device code that lives 2 s, with a poll interval of 1 s, but its answer took 1.500 s to arrive, which left 0.200 s of it: the code would lapse before its first poll; a machine login works at that broker where its answer arrives in less than 0.700 s, or once its codes outlive its poll interval by more than its answers take to arrive`},
		// A pause of 2 ms between x/oauth2's readings leaves a 6 s code 5.998 s, so the 1.5 s answer
		// against a 5 s interval names what the code leaves, not the whole second: a row on the real
		// clock would meet this on a loaded runner, so it is handed the readings here.
		{"a pause between x/oauth2's readings, a 6 s code and a 1.5 s round trip", 6*s - 2*time.Millisecond, 6*s - 2*time.Millisecond - 1500*time.Millisecond, 5, `a device code that lives 6 s, with a poll interval of 5 s, but its answer took 1.500 s to arrive, which left 4.498 s of it: the code would lapse before its first poll; a machine login works at that broker where its answer arrives in less than 0.998 s, or once its codes outlive its poll interval by more than its answers take to arrive`},
		// A shortfall of half a millisecond keeps the whole second; one of a millisecond and a half
		// names what the code leaves. The tolerance is one millisecond: these rows fail if it is 0,
		// or 50 ms.
		{"a pause of half a millisecond between x/oauth2's readings", 2*s - 500*time.Microsecond, 400 * time.Millisecond, 1, fmt.Sprintf(`a device code that lives 2 s, with a poll interval of 1 s, but its answer took %.3f s to arrive, which left 0.400 s of it: the code would lapse before its first poll; a machine login works at that broker where its answer arrives in less than 1 s, or once its codes outlive its poll interval by more than its answers take to arrive`, (2*s - 500*time.Microsecond - 400*time.Millisecond).Seconds())},
		{"a pause of a millisecond and a half between x/oauth2's readings", 2*s - 1500*time.Microsecond, 400 * time.Millisecond, 1, fmt.Sprintf(`a device code that lives 2 s, with a poll interval of 1 s, but its answer took %.3f s to arrive, which left 0.400 s of it: the code would lapse before its first poll; a machine login works at that broker where its answer arrives in less than 0.998 s, or once its codes outlive its poll interval by more than its answers take to arrive`, (2*s - 1500*time.Microsecond - 400*time.Millisecond).Seconds())},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := unpollableCode(c.lived, c.left, c.interval, c.interval, false)
			if want := "the broker answered " + c.answer; err == nil || err.Error() != want {
				t.Fatalf("the refusal said %v, want %q", err, want)
			}
		})
	}
}

// TestTheRefusalNamesNoCureAForwardWrappedLifetimeAlreadyMeets: x/oauth2's DeviceAuthResponse holds
// the code's end and no raw expires_in, and a lifetime it wraps forward (past 2^64 ns, about 585
// years, as 18446744075 s) keeps a fraction of a second that no reading of lived tells from a pause,
// so the helper cannot name what the broker sent. Its line must then name no cure the code's own end
// may already meet: the wrapped code of 1.29 s is polled where its answer arrives in under 0.29 s,
// and the line for a slow one says the codes must outlive the interval by more than the answers
// take, and a cure by the answer's speed names no more time than lived leaves over the interval,
// not the lifetime's whole seconds where lived holds fewer.
func TestTheRefusalNamesNoCureAForwardWrappedLifetimeAlreadyMeets(t *testing.T) {
	const s = time.Second
	for _, c := range []struct {
		name        string
		lived, left time.Duration // the code's end less Login's start, and less now
		interval    int64
		answer      string
	}{
		// 18446744075 s wraps to 1.290448384 s; the answer took 0.5 s.
		{"a wrap to 1.29 s, answered in 0.5 s", 1290448384, 790448384, 1, `a device code that lives 1 s, with a poll interval of 1 s: the code would lapse before its first poll; no machine login works at that broker until its codes live longer than 1 s, the least poll interval this machine keeps, and then only once its codes outlive its poll interval by more than its answers take to arrive`},
		// 36893488149 s wraps to 1.580896768 s; the answer took 0.7 s, in less than the 1 s the whole
		// seconds name, and left 0.881 s: it needs under 0.580 s.
		{"a wrap to 1.58 s, answered in 0.7 s", 1580896768, 880896768, 1, `a device code that lives 2 s, with a poll interval of 1 s, but its answer took 0.700 s to arrive, which left 0.881 s of it: the code would lapse before its first poll; a machine login works at that broker where its answer arrives in less than 0.580 s, or once its codes outlive its poll interval by more than its answers take to arrive`},
		// The same code answered in 1.2 s, over the whole second the 0.7 s answer met, still names
		// the time lived leaves, not the second.
		{"a wrap to 1.58 s, answered in 1.2 s", 1580896768, 380896768, 1, `a device code that lives 2 s, with a poll interval of 1 s, but its answer took 1.200 s to arrive, which left 0.381 s of it: the code would lapse before its first poll; a machine login works at that broker where its answer arrives in less than 0.580 s, or once its codes outlive its poll interval by more than its answers take to arrive`},
		// A code whose end holds the whole seconds (2.1 s, a pause before x/oauth2's start) keeps
		// them: lived over the interval is 1.1 s, more than the 1 s.
		{"a code of 2.1 s, answered in 1.2 s", 2100 * time.Millisecond, 900 * time.Millisecond, 1, `a device code that lives 2 s, with a poll interval of 1 s, but its answer took 1.200 s to arrive, which left 0.900 s of it: the code would lapse before its first poll; a machine login works at that broker where its answer arrives in less than 1 s, or once its codes outlive its poll interval by more than its answers take to arrive`},
		// 18446747674 s wraps to 3600.290448384 s; the answer took 0.4 s.
		{"a wrap to 3600.29 s, answered in 0.4 s", 3600*s + 290448384, 3600*s + 290448384 - 400*time.Millisecond, 3600, `a device code that lives 3600 s, with a poll interval of 3600 s: the code would lapse before its first poll; no machine login works at that broker until its poll interval is shorter than its codes' lifetime, and then only once its codes outlive its poll interval by more than its answers take to arrive`},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := unpollableCode(c.lived, c.left, c.interval, c.interval, false)
			if want := "the broker answered " + c.answer; err == nil || err.Error() != want {
				t.Fatalf("the refusal said %v, want %q", err, want)
			}
		})
	}
}

// TestADevicePollReadsItsAnswerAsXOAuth2Does: devicePoll reads a token endpoint's answer as
// x/oauth2 v0.37.0 reads it (internal/token.go:258-333). A 2xx whose JSON, form or text body holds
// an access_token and names no error is the token. Any other status, and a 2xx naming an error,
// is the broker's answer, an *oauth2.RetrieveError with the error its body names. A 2xx whose body
// does not parse or holds no access_token, and an answer cut off while it is read, is a plain
// error, which pollLogin counts as an outage.
func TestADevicePollReadsItsAnswerAsXOAuth2Does(t *testing.T) {
	const (
		form     = "application/x-www-form-urlencoded"
		text     = "text/plain; charset=utf-8"
		approval = "access_token=a&token_type=DPoP&refresh_token=r&credential_id=lcred-1"
		approved = `{"access_token":"a","token_type":"DPoP","refresh_token":"r","credential_id":"lcred-1"`
	)
	// read is what a poll came to: the access token of an approval, the status and the error of
	// the broker's answer, or, for a plain error, whether it is an answer cut off while it was read.
	type read struct {
		token  string
		status int
		code   string
		cut    bool
	}
	for _, c := range []struct {
		name   string
		answer pollAnswer
		want   read
	}{
		{"a 200 JSON approval", answeringBody(http.StatusOK, "application/json", approved+"}"), read{token: "a"}},
		{"a 200 form approval", answeringBody(http.StatusOK, form, approval), read{token: "a"}},
		{"a 200 text approval", answeringBody(http.StatusOK, text, approval), read{token: "a"}},
		{"a 201 JSON approval", answeringBody(http.StatusCreated, "application/json", approved+"}"), read{token: "a"}},
		{"a 200 JSON approval whose expires_in is a fraction", answeringBody(http.StatusOK, "application/json", approved+`,"expires_in":900.5}`), read{token: "a"}},
		{"a 400 form slow_down", answeringBody(http.StatusBadRequest, form, "error=slow_down"), read{status: http.StatusBadRequest, code: "slow_down"}},
		{"a 401 text access_denied", answeringBody(http.StatusUnauthorized, text, "error=access_denied"), read{status: http.StatusUnauthorized, code: "access_denied"}},
		{"a 200 naming slow_down", answering(http.StatusOK, "slow_down"), read{status: http.StatusOK, code: "slow_down"}},
		{"a 200 form naming access_denied beside a token", answeringBody(http.StatusOK, form, "error=access_denied&"+approval), read{status: http.StatusOK, code: "access_denied"}},
		// The redirect is refused (redirects.Refused), so the 302 is the answer read.
		{"a 302", func(w http.ResponseWriter) {
			w.Header().Set("Location", "/elsewhere")
			answeringBody(http.StatusFound, "text/html", "<html>moved</html>")(w)
		}, read{status: http.StatusFound}},
		{"a 503 page", answeringBody(http.StatusServiceUnavailable, "text/html", "<html>down</html>"), read{status: http.StatusServiceUnavailable}},
		{"a 400 whose JSON does not parse", answeringBody(http.StatusBadRequest, "application/json", "{"), read{status: http.StatusBadRequest}},
		// A 2xx that holds no token and names no error is no token endpoint's answer.
		{"a 200 with no access_token", answeringBody(http.StatusOK, "application/json", `{"token_type":"DPoP","refresh_token":"r"}`), read{}},
		{"a 200 page", answeringBody(http.StatusOK, "text/html", "<html>a portal's sign-in page</html>"), read{}},
		{"a 200 whose JSON does not parse", answeringBody(http.StatusOK, "application/json", "{"), read{}},
		{"a 200 form that does not parse", answeringBody(http.StatusOK, form, "access_token=%zz"), read{}},
		{"a 200 cut off while it is read", cutOff, read{cut: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { c.answer(w) }))
			t.Cleanup(srv.Close)
			cfg := &oauth2.Config{ClientID: "machine-login", Endpoint: oauth2.Endpoint{TokenURL: srv.URL}}
			tok, err := devicePoll(context.Background(), redirects.Refused(srv.Client()), cfg, &oauth2.DeviceAuthResponse{DeviceCode: "device-1"})
			var got read
			var answered *oauth2.RetrieveError
			switch {
			case err == nil:
				got.token = tok.AccessToken
				if id := tok.Extra("credential_id"); id != "lcred-1" {
					t.Fatalf("the approval's credential_id %v, want lcred-1", id)
				}
			case errors.As(err, &answered):
				got.status, got.code = answered.Response.StatusCode, answered.ErrorCode
			default:
				got.cut = errors.Is(err, io.ErrUnexpectedEOF)
			}
			if got != c.want {
				t.Fatalf("devicePoll read %+v (%v), want %+v", got, err, c.want)
			}
		})
	}
}
